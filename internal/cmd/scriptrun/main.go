// scriptrun executes one declared source script or one planner-evaluated script
// through one declared interpreter. It never searches the ambient PATH.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

const (
	maxScriptBytes          = 16 << 20
	maxMulticallListSize    = 1 << 20
	maxMulticallApplets     = 4096
	maxReplayManifestBytes  = 1 << 20
	maxReplayTotalBytes     = 4 << 20
	maxReplayManifests      = 256
	maxReplayInvocations    = 4096
	maxReplayArguments      = 4096
	maxReplayOutputs        = 4096
	maxReplayValueBytes     = 1 << 20
	maxReplayValueTotalSize = 4 << 20
	maxReplayProtocolBytes  = maxReplayValueTotalSize + (maxReplayArguments * 4) + 64
	maxReplayDiagnosticSize = maxReplayValueBytes + 4096
	scriptReplayIOTimeout   = 30 * time.Second
	scriptAppletRolePrefix  = "script-applet-"
	scriptReplayProxyMode   = "__linux_bzl_internal_script_replay_proxy__"
	scriptReplayProtocol    = "LBZLRP01"
	scriptReplayResponse    = "LBZLRS01"
	// O_PATH is Linux-specific and intentionally not exposed by package syscall.
	// scriptrun itself is selected only by the Linux script-runtime toolchain.
	linuxOpenPath = 0x200000
)

type repeatedFlag []string

func (f *repeatedFlag) String() string         { return strings.Join(*f, " ") }
func (f *repeatedFlag) Set(value string) error { *f = append(*f, value); return nil }

type scriptReplayManifest struct {
	Name        string                   `json:"name"`
	Invocations []scriptReplayInvocation `json:"invocations"`
}

type scriptReplayInvocation struct {
	Arguments []string `json:"arguments"`
	Outputs   []string `json:"outputs"`
}

// The replay runtime representation is deliberately private to the original
// scriptrun process. It is never serialized into the source script's writable
// filesystem. The action plan keeps carrying only command arguments and output
// paths; concrete file identities are captured immediately before the source
// script runs and first-seen receipts remain in parent memory.
type scriptReplayRuntimeManifest struct {
	Name        string
	Invocations []scriptReplayRuntimeInvocation
}

type scriptReplayRuntimeInvocation struct {
	Arguments []string
	Outputs   []*scriptReplayRuntimeOutput
}

type scriptReplayRuntimeOutput struct {
	DisplayPath string
	Path        string
	Initial     *scriptReplayOutputSnapshot

	mu      sync.Mutex
	receipt *scriptReplayOutputSnapshot
}

type scriptReplayOutputSnapshot struct {
	Digest         string
	ExecutableBits uint32
}

type scriptReplayBroker struct {
	listener   *net.UnixListener
	endpoint   string
	replays    []*scriptReplayRuntimeManifest
	acceptDone chan error
	closeDone  chan struct{}
	closeOnce  sync.Once
	closeErr   error

	mu          sync.Mutex
	closing     bool
	connections map[*net.UnixConn]bool
	handlers    sync.WaitGroup
}

type scriptReplayProxyFailure struct {
	exitCode int
	message  string
}

func (failure *scriptReplayProxyFailure) Error() string { return failure.message }

type scriptRunOptions struct {
	interpreter        string
	interpreterArgs    []string
	multicall          string
	script             string
	scriptContent      string
	scriptStdin        bool
	scriptArgs         []string
	applets            map[string]string
	requiredApplets    []string
	tools              map[string]string
	trees              map[string]string
	literalTreeOffsets map[int]bool
	toolContracts      map[string]toolaction.Contract
	runtimeToolPath    string
	toolsetHandoff     string
	replays            []scriptReplayManifest
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
}

func runScript(opts scriptRunOptions) error {
	if opts.scriptStdin {
		if opts.script != "" || opts.scriptContent != "" {
			return fmt.Errorf("stdin script content is mutually exclusive with a source script or evaluated script content")
		}
		reader := opts.stdin
		if reader == nil {
			reader = strings.NewReader("")
		}
		content, err := io.ReadAll(io.LimitReader(reader, maxScriptBytes+1))
		if err != nil {
			return fmt.Errorf("read evaluated script from stdin: %w", err)
		}
		if len(content) > maxScriptBytes {
			return fmt.Errorf("evaluated script from stdin exceeds %d bytes", maxScriptBytes)
		}
		opts.scriptContent = string(content)
		// The selected action stdin has become the script itself. Do not expose a
		// second copy to that script: callers which need data stdin must use the
		// ordinary source/content modes and bind it independently.
		opts.stdin = strings.NewReader("")
	}
	if (opts.script == "") == (opts.scriptContent == "") {
		return fmt.Errorf("exactly one source script or evaluated script content is required")
	}
	interpreter, err := requireScriptExecutable(opts.interpreter, "interpreter")
	if err != nil {
		return err
	}
	// The declared source tree may be the command's working directory so that
	// source scripts observe the same relative layout as Kbuild. That tree is
	// an action input and therefore read-only. Keep the private runtime in the
	// action's writable temporary area instead of creating it beside the
	// source script.
	runtimeRoot, err := os.MkdirTemp("", "linux-bzl-script-runtime-")
	if err != nil {
		return fmt.Errorf("create private script runtime: %w", err)
	}
	defer os.RemoveAll(runtimeRoot)
	runtimeRoot, err = filepath.Abs(runtimeRoot)
	if err != nil {
		return fmt.Errorf("resolve private script runtime: %w", err)
	}
	var toolsetPaths *toolsetpath.Resolver
	if opts.toolsetHandoff != "" {
		projectionRoot := filepath.Join(runtimeRoot, "toolsets")
		if err := os.Mkdir(projectionRoot, 0o700); err != nil {
			return fmt.Errorf("create script toolset projection root: %w", err)
		}
		toolsetPaths, err = toolsetpath.LoadHandoff(opts.toolsetHandoff, projectionRoot)
		if err != nil {
			return fmt.Errorf("load script toolset bindings: %w", err)
		}
	}
	runtimeToolPath, err := validateRuntimeToolPath(opts.runtimeToolPath)
	if err != nil {
		return err
	}
	script := ""
	if opts.script != "" {
		script, err = requireSourceScript(opts.script)
		if err != nil {
			return err
		}
	} else {
		if len(opts.scriptContent) == 0 || len(opts.scriptContent) > maxScriptBytes || strings.ContainsRune(opts.scriptContent, 0) {
			return fmt.Errorf("evaluated script content is empty, invalid, or exceeds %d bytes", maxScriptBytes)
		}
		opts.scriptContent, err = expandScriptTreeBindingsWithLiteralOffsets(opts.scriptContent, opts.trees, opts.literalTreeOffsets)
		if err != nil {
			return err
		}
		opts.scriptContent, err = toolsetpath.RewriteShell(opts.scriptContent, toolsetPaths)
		if err != nil {
			return fmt.Errorf("expand evaluated-script compiler path: %w", err)
		}
		script = filepath.Join(runtimeRoot, "evaluated-kbuild-recipe.sh")
		if err := os.WriteFile(script, []byte(opts.scriptContent), 0o600); err != nil {
			return fmt.Errorf("materialize evaluated Kbuild recipe: %w", err)
		}
	}
	var toolsetAliasRoot *os.File
	if toolsetPaths != nil {
		toolsetAliasRoot, err = toolsetPaths.OpenShellAliasRoot()
		if err != nil {
			return fmt.Errorf("open evaluated-script toolset aliases: %w", err)
		}
		defer toolsetAliasRoot.Close()
	}
	toolDirectory := filepath.Join(runtimeRoot, "bin")
	tempDirectory := filepath.Join(runtimeRoot, "tmp")
	for _, directory := range []string{toolDirectory, tempDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return fmt.Errorf("create private script runtime directory: %w", err)
		}
	}
	multicall := ""
	applets := map[string]bool{}
	if opts.multicall != "" {
		multicall, err = requireScriptExecutable(opts.multicall, "multicall runtime")
		if err != nil {
			return err
		}
		listedApplets, err := multicallApplets(multicall)
		if err != nil {
			return err
		}
		for _, applet := range listedApplets {
			if err := installScriptTool(toolDirectory, applet, multicall); err != nil {
				return fmt.Errorf("install multicall applet %s: %w", applet, err)
			}
			applets[applet] = true
		}
	}
	appNames := make([]string, 0, len(opts.applets))
	for name := range opts.applets {
		appNames = append(appNames, name)
	}
	sort.Strings(appNames)
	// Runtime-toolchain applet overrides are separate from configured action
	// roles. Install them after the base multicall list so the selected runtime
	// owns command compatibility without teaching the planner command names.
	for _, name := range appNames {
		executable, err := requireScriptExecutable(opts.applets[name], "runtime applet "+name)
		if err != nil {
			return err
		}
		if err := installScriptTool(toolDirectory, name, executable); err != nil {
			return fmt.Errorf("install runtime applet %s: %w", name, err)
		}
		applets[name] = true
	}
	seenRequiredApplets := map[string]bool{}
	for _, name := range opts.requiredApplets {
		if err := validateScriptToolName(name); err != nil {
			return fmt.Errorf("required runtime applet: %w", err)
		}
		if seenRequiredApplets[name] {
			return fmt.Errorf("runtime applet %q is required more than once", name)
		}
		seenRequiredApplets[name] = true
		if !applets[name] {
			return fmt.Errorf("selected multicall runtime does not provide required applet %q", name)
		}
	}
	if err := validateToolContracts(opts.tools, opts.applets, opts.toolContracts); err != nil {
		return err
	}
	replayCollisions := make(map[string]string, len(opts.tools)+len(opts.applets))
	for name, executable := range opts.applets {
		replayCollisions[name] = executable
	}
	for name, executable := range opts.tools {
		replayCollisions[name] = executable
	}
	if err := validateReplayManifests(opts.replays, replayCollisions); err != nil {
		return err
	}
	if (len(opts.tools) != 0 || len(opts.replays) != 0) && (multicall == "" || !applets["sh"]) {
		return fmt.Errorf("external tools and command replays require a multicall runtime with a sh applet")
	}
	names := make([]string, 0, len(opts.tools))
	for name := range opts.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	// A multicall applet can share a configured role's basename. Installing the
	// selected proxy after all applets deliberately replaces that entry; an
	// omitted role has no configured proxy even if the runtime has a same-named
	// utility of its own.
	for _, name := range names {
		tool, err := requireScriptExecutable(opts.tools[name], "external tool "+name)
		if err != nil {
			return err
		}
		linkContract := (*toolaction.Contract)(nil)
		if linkRole, ok := toolaction.LinkContractRole(name); ok {
			if contract, exists := opts.toolContracts[linkRole]; exists {
				linkContract = &contract
			}
		}
		if _, err := toolaction.InstallToolActionProxy(toolDirectory, multicall, name, tool, opts.toolContracts[name], linkContract); err != nil {
			return fmt.Errorf("install external tool %s: %w", name, err)
		}
	}
	replays := append([]scriptReplayManifest(nil), opts.replays...)
	sort.Slice(replays, func(left, right int) bool { return replays[left].Name < replays[right].Name })
	preparedReplays := make([]*scriptReplayRuntimeManifest, 0, len(replays))
	replayVerifier := ""
	if len(replays) != 0 {
		replayVerifier, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve command replay verifier: %w", err)
		}
		replayVerifier, err = requireScriptExecutable(replayVerifier, "command replay verifier")
		if err != nil {
			return err
		}
	}
	for _, replay := range replays {
		prepared, err := prepareScriptReplayRuntime(replay)
		if err != nil {
			return fmt.Errorf("prepare command replay %s: %w", replay.Name, err)
		}
		preparedReplays = append(preparedReplays, prepared)
	}
	var replayBroker *scriptReplayBroker
	brokerOpen := false
	if len(preparedReplays) != 0 {
		replayBroker, err = startScriptReplayBroker(preparedReplays)
		if err != nil {
			return fmt.Errorf("start command replay broker: %w", err)
		}
		brokerOpen = true
		defer func() {
			if brokerOpen {
				_ = replayBroker.Close()
			}
		}()
		for replayIndex, replay := range replays {
			if err := installScriptReplayProxy(toolDirectory, multicall, replayVerifier, replayBroker.endpoint, uint32(replayIndex), replay); err != nil {
				return fmt.Errorf("install command replay %s: %w", replay.Name, err)
			}
		}
	}
	arguments := append([]string(nil), opts.interpreterArgs...)
	arguments = append(arguments, script)
	arguments = append(arguments, opts.scriptArgs...)
	command := exec.Command(interpreter, arguments...)
	if toolsetAliasRoot != nil {
		// ExtraFiles[0] is fd 3 in the child, matching toolsetpath's stable
		// /proc/self/fd/3 shell alias contract. Descendant shells and tool
		// proxies inherit the descriptor across their exec chains.
		command.ExtraFiles = []*os.File{toolsetAliasRoot}
	}
	environment := environmentMap(os.Environ())
	for _, name := range []string{
		"BASH_ENV", "CDPATH", "ENV", "GLOBIGNORE", "PATH", "SHELLOPTS", "TMPDIR",
		toolaction.EnvironmentName,
		toolaction.RuntimeToolPathEnvironmentName,
		toolsetpath.HandoffEnvironmentName,
	} {
		delete(environment, name)
	}
	environment["LC_ALL"] = "C"
	environment["PATH"] = toolDirectory
	if runtimeToolPath != "" {
		environment["PATH"] += string(os.PathListSeparator) + runtimeToolPath
	}
	environment["TMPDIR"] = tempDirectory
	environment["TZ"] = "UTC"
	command.Env = environmentList(environment)
	command.Stdin = opts.stdin
	command.Stdout = opts.stdout
	command.Stderr = opts.stderr
	commandErr := command.Run()
	var brokerErr error
	if replayBroker != nil {
		brokerErr = replayBroker.Close()
		brokerOpen = false
	}
	if err := verifyScriptReplayReceipts(preparedReplays); err != nil {
		if commandErr != nil {
			return fmt.Errorf("execute source script: %v; verify command replay outputs: %w", commandErr, err)
		}
		return fmt.Errorf("verify command replay outputs: %w", err)
	}
	if brokerErr != nil {
		if commandErr != nil {
			return fmt.Errorf("execute source script: %v; stop command replay broker: %w", commandErr, brokerErr)
		}
		return fmt.Errorf("stop command replay broker: %w", brokerErr)
	}
	if commandErr != nil {
		return fmt.Errorf("execute source script: %w", commandErr)
	}
	return nil
}

func requireScriptExecutable(filename, description string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", fmt.Errorf("%s is required", description)
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", description, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", description, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s %q is not an executable regular file", description, filename)
	}
	return absolute, nil
}

func requireSourceScript(filename string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", fmt.Errorf("source script is required")
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("resolve source script: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect source script: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxScriptBytes {
		return "", fmt.Errorf("source script %q is not a bounded regular file", filename)
	}
	return absolute, nil
}

func validateRuntimeToolPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, os.PathListSeparator) {
		return "", fmt.Errorf("runner-owned runtime-tool path %q is not one absolute directory", value)
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", fmt.Errorf("inspect runner-owned runtime-tool path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("runner-owned runtime-tool path %q is not a directory", value)
	}
	return value, nil
}

func multicallApplets(multicall string) ([]string, error) {
	command := exec.Command(multicall, "--list")
	command.Env = []string{"LC_ALL=C", "PATH="}
	var output strings.Builder
	command.Stdout = &boundedWriter{writer: &output, remaining: maxMulticallListSize}
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("list multicall applets: %w", err)
	}
	seen := map[string]bool{}
	var applets []string
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if err := validateScriptToolName(name); err != nil {
			return nil, fmt.Errorf("invalid multicall applet: %w", err)
		}
		if seen[name] {
			return nil, fmt.Errorf("multicall runtime repeats applet %q", name)
		}
		seen[name] = true
		applets = append(applets, name)
		if len(applets) > maxMulticallApplets {
			return nil, fmt.Errorf("multicall runtime exposes more than %d applets", maxMulticallApplets)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read multicall applets: %w", err)
	}
	if len(applets) == 0 {
		return nil, fmt.Errorf("multicall runtime exposes no applets")
	}
	sort.Strings(applets)
	return applets, nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *boundedWriter) Write(value []byte) (int, error) {
	if len(value) > w.remaining {
		return 0, fmt.Errorf("output exceeds %d-byte limit", maxMulticallListSize)
	}
	w.remaining -= len(value)
	return w.writer.Write(value)
}

func installScriptTool(directory, name, executable string) error {
	if err := validateScriptToolName(name); err != nil {
		return err
	}
	destination := filepath.Join(directory, name)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(executable, destination)
}

func validateToolContracts(tools, applets map[string]string, contracts map[string]toolaction.Contract) error {
	if err := toolaction.Validate(contracts); err != nil {
		return fmt.Errorf("external tool action contracts: %w", err)
	}
	for name := range tools {
		if _, companion := toolaction.BaseContractRole(name); companion {
			return fmt.Errorf("external tool %q is a semantic contract and cannot bind a separate executable", name)
		}
		if _, exists := contracts[name]; !exists {
			return fmt.Errorf("external tool %q has no configured action contract", name)
		}
	}
	for role := range contracts {
		if _, exists := tools[role]; exists {
			continue
		}
		if name, applet := strings.CutPrefix(role, scriptAppletRolePrefix); applet && applets[name] != "" {
			contract := contracts[role]
			if len(contract.Arguments) != 0 || len(contract.Environment) != 0 {
				return fmt.Errorf("runtime applet %q must have an empty action contract", name)
			}
			continue
		}
		base, companion := toolaction.BaseContractRole(role)
		if !companion || tools[base] == "" {
			return fmt.Errorf("configured action contract for unbound external tool %q", role)
		}
		if _, exists := contracts[base]; !exists {
			return fmt.Errorf("configured companion action contract %q has no base %q contract", role, base)
		}
	}
	return nil
}

func decodeReplayManifests(values []string) ([]scriptReplayManifest, error) {
	if len(values) > maxReplayManifests {
		return nil, fmt.Errorf("more than %d command replay manifests", maxReplayManifests)
	}
	manifests := make([]scriptReplayManifest, 0, len(values))
	totalBytes := 0
	for index, value := range values {
		if value == "" || len(value) > base64.StdEncoding.EncodedLen(maxReplayManifestBytes) {
			return nil, fmt.Errorf("command replay manifest %d is empty or exceeds %d decoded bytes", index, maxReplayManifestBytes)
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode command replay manifest %d: %w", index, err)
		}
		if len(decoded) == 0 || len(decoded) > maxReplayManifestBytes {
			return nil, fmt.Errorf("command replay manifest %d is empty or exceeds %d decoded bytes", index, maxReplayManifestBytes)
		}
		totalBytes += len(decoded)
		if totalBytes > maxReplayTotalBytes {
			return nil, fmt.Errorf("command replay manifests exceed %d decoded bytes", maxReplayTotalBytes)
		}

		var manifest scriptReplayManifest
		decoder := json.NewDecoder(bytes.NewReader(decoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			return nil, fmt.Errorf("decode command replay manifest %d JSON: %w", index, err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return nil, fmt.Errorf("decode command replay manifest %d JSON: %w", index, err)
		}
		manifests = append(manifests, manifest)
	}
	if err := validateReplayManifests(manifests, nil); err != nil {
		return nil, err
	}
	return manifests, nil
}

func validateReplayManifests(manifests []scriptReplayManifest, tools map[string]string) error {
	if len(manifests) > maxReplayManifests {
		return fmt.Errorf("more than %d command replay manifests", maxReplayManifests)
	}
	seenNames := make(map[string]bool, len(manifests))
	totalInvocations := 0
	totalArguments := 0
	totalOutputs := 0
	totalValueBytes := 0
	validateValue := func(description, value string, allowEmpty bool) error {
		if (!allowEmpty && value == "") || strings.ContainsRune(value, 0) || len(value) > maxReplayValueBytes {
			return fmt.Errorf("%s is empty, contains NUL, or exceeds %d bytes", description, maxReplayValueBytes)
		}
		totalValueBytes += len(value)
		if totalValueBytes > maxReplayValueTotalSize {
			return fmt.Errorf("command replay values exceed %d bytes", maxReplayValueTotalSize)
		}
		return nil
	}
	for manifestIndex, manifest := range manifests {
		if err := validateScriptToolName(manifest.Name); err != nil {
			return fmt.Errorf("command replay manifest %d: %w", manifestIndex, err)
		}
		if err := validateValue(fmt.Sprintf("command replay name %q", manifest.Name), manifest.Name, false); err != nil {
			return err
		}
		if seenNames[manifest.Name] {
			return fmt.Errorf("duplicate command replay name %q", manifest.Name)
		}
		seenNames[manifest.Name] = true
		if _, exists := tools[manifest.Name]; exists {
			return fmt.Errorf("command replay %q collides with an external tool", manifest.Name)
		}
		if len(manifest.Invocations) == 0 {
			return fmt.Errorf("command replay %q has no declared invocations", manifest.Name)
		}
		totalInvocations += len(manifest.Invocations)
		if totalInvocations > maxReplayInvocations {
			return fmt.Errorf("command replay manifests contain more than %d invocations", maxReplayInvocations)
		}
		seenInvocations := map[string]bool{}
		for invocationIndex, invocation := range manifest.Invocations {
			totalArguments += len(invocation.Arguments)
			if totalArguments > maxReplayArguments {
				return fmt.Errorf("command replay manifests contain more than %d arguments", maxReplayArguments)
			}
			totalOutputs += len(invocation.Outputs)
			if totalOutputs > maxReplayOutputs {
				return fmt.Errorf("command replay manifests contain more than %d outputs", maxReplayOutputs)
			}
			argumentsKey := scriptReplayArgumentsKey(invocation.Arguments)
			if seenInvocations[argumentsKey] {
				return fmt.Errorf("command replay %q repeats invocation arguments at index %d", manifest.Name, invocationIndex)
			}
			seenInvocations[argumentsKey] = true
			for argumentIndex, argument := range invocation.Arguments {
				if err := validateValue(fmt.Sprintf("command replay %q invocation %d argument %d", manifest.Name, invocationIndex, argumentIndex), argument, true); err != nil {
					return err
				}
			}
			seenOutputs := map[string]bool{}
			for outputIndex, output := range invocation.Outputs {
				if err := validateValue(fmt.Sprintf("command replay %q invocation %d output %d", manifest.Name, invocationIndex, outputIndex), output, false); err != nil {
					return err
				}
				if seenOutputs[output] {
					return fmt.Errorf("command replay %q invocation %d repeats output %q", manifest.Name, invocationIndex, output)
				}
				seenOutputs[output] = true
			}
		}
	}
	return nil
}

func scriptReplayArgumentsKey(arguments []string) string {
	var key strings.Builder
	for _, argument := range arguments {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(argument)))
		key.Write(size[:])
		key.WriteString(argument)
	}
	return key.String()
}

func prepareScriptReplayRuntime(manifest scriptReplayManifest) (*scriptReplayRuntimeManifest, error) {
	if err := validateReplayManifests([]scriptReplayManifest{manifest}, nil); err != nil {
		return nil, err
	}
	runtimeManifest := &scriptReplayRuntimeManifest{
		Name:        manifest.Name,
		Invocations: make([]scriptReplayRuntimeInvocation, len(manifest.Invocations)),
	}
	for invocationIndex, invocation := range manifest.Invocations {
		runtimeInvocation := scriptReplayRuntimeInvocation{
			Arguments: append([]string(nil), invocation.Arguments...),
			Outputs:   make([]*scriptReplayRuntimeOutput, len(invocation.Outputs)),
		}
		for outputIndex, output := range invocation.Outputs {
			// Generated replay paths are absolute after work-tree expansion. Keep
			// relative test/debug manifests useful by anchoring them to the same
			// pre-script directory in which their initial identity is captured.
			absolute, err := filepath.Abs(output)
			if err != nil {
				return nil, fmt.Errorf("resolve output %q: %w", output, err)
			}
			runtimeOutput := &scriptReplayRuntimeOutput{
				DisplayPath: output,
				Path:        absolute,
			}
			snapshot, available, err := snapshotScriptReplayOutput(absolute)
			if err != nil {
				return nil, fmt.Errorf("snapshot output %q: %w", output, err)
			}
			if available {
				runtimeOutput.Initial = &snapshot
			}
			runtimeInvocation.Outputs[outputIndex] = runtimeOutput
		}
		runtimeManifest.Invocations[invocationIndex] = runtimeInvocation
	}
	return runtimeManifest, nil
}

func snapshotScriptReplayOutput(path string) (scriptReplayOutputSnapshot, bool, error) {
	pinnedFD, pinned, available, err := pinScriptReplayOutput(path)
	if err != nil || !available {
		return scriptReplayOutputSnapshot{}, available, err
	}
	defer syscall.Close(pinnedFD)

	// Reopen the descriptor rather than the pathname. A source script can rename
	// or replace its output concurrently, but /proc/self/fd continues to name the
	// exact O_PATH-pinned inode and O_NOFOLLOW has already rejected a final
	// symlink. This also gives us a stable object on which to restore permissions.
	descriptorPath := "/proc/self/fd/" + strconv.Itoa(pinnedFD)
	file, err := os.Open(descriptorPath)
	originalMode := pinned.Mode & 0o7777
	if err != nil && originalMode&0o400 == 0 {
		if err := syscall.Chmod(descriptorPath, originalMode|0o400); err != nil {
			return scriptReplayOutputSnapshot{}, false, fmt.Errorf("temporarily make output readable: %w", err)
		}
		file, err = os.Open(descriptorPath)
		if file != nil {
			if restoreErr := syscall.Fchmod(int(file.Fd()), originalMode); restoreErr != nil {
				fallbackErr := syscall.Chmod(descriptorPath, originalMode)
				_ = file.Close()
				if fallbackErr != nil {
					return scriptReplayOutputSnapshot{}, false, fmt.Errorf("restore output mode through descriptor: %v; fallback restore: %w", restoreErr, fallbackErr)
				}
				return scriptReplayOutputSnapshot{}, false, fmt.Errorf("restore output mode: %w", restoreErr)
			}
		} else if restoreErr := syscall.Chmod(descriptorPath, originalMode); restoreErr != nil {
			return scriptReplayOutputSnapshot{}, false, fmt.Errorf("open output: %v; restore output mode: %w", err, restoreErr)
		}
	}
	if err != nil {
		return scriptReplayOutputSnapshot{}, false, err
	}
	defer file.Close()

	var opened syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &opened); err != nil {
		return scriptReplayOutputSnapshot{}, false, err
	}
	if !sameScriptReplayFile(pinned, opened) || opened.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return scriptReplayOutputSnapshot{}, false, fmt.Errorf("output changed identity while opening")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return scriptReplayOutputSnapshot{}, false, err
	}
	var afterRead syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &afterRead); err != nil {
		return scriptReplayOutputSnapshot{}, false, err
	}
	if !stableScriptReplayFile(opened, afterRead) {
		return scriptReplayOutputSnapshot{}, false, fmt.Errorf("output changed identity, metadata, or size while reading")
	}
	currentFD, current, currentAvailable, err := pinScriptReplayOutput(path)
	if err != nil {
		return scriptReplayOutputSnapshot{}, false, err
	}
	if currentFD >= 0 {
		defer syscall.Close(currentFD)
	}
	if !currentAvailable || !stableScriptReplayFile(afterRead, current) {
		return scriptReplayOutputSnapshot{}, false, fmt.Errorf("output path changed identity, metadata, or size while reading")
	}
	return scriptReplayOutputSnapshot{
		Digest:         base64.RawStdEncoding.EncodeToString(digest.Sum(nil)),
		ExecutableBits: afterRead.Mode & 0o111,
	}, true, nil
}

func pinScriptReplayOutput(path string) (int, syscall.Stat_t, bool, error) {
	fd, err := syscall.Open(path, linuxOpenPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return -1, syscall.Stat_t{}, false, nil
		}
		return -1, syscall.Stat_t{}, false, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = syscall.Close(fd)
		return -1, syscall.Stat_t{}, false, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = syscall.Close(fd)
		return -1, syscall.Stat_t{}, false, nil
	}
	return fd, stat, true, nil
}

func sameScriptReplayFile(left, right syscall.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}

func stableScriptReplayFile(left, right syscall.Stat_t) bool {
	return sameScriptReplayFile(left, right) &&
		left.Mode&syscall.S_IFMT == syscall.S_IFREG &&
		right.Mode&syscall.S_IFMT == syscall.S_IFREG &&
		left.Mode&0o111 == right.Mode&0o111 &&
		left.Size == right.Size &&
		left.Mtim.Sec == right.Mtim.Sec && left.Mtim.Nsec == right.Mtim.Nsec &&
		left.Ctim.Sec == right.Ctim.Sec && left.Ctim.Nsec == right.Ctim.Nsec
}

func scriptReplayOutputFailure(name string, output *scriptReplayRuntimeOutput, err error) error {
	if err == nil {
		return &scriptReplayProxyFailure{
			exitCode: 66,
			message:  "command replay " + name + " is missing regular output " + output.DisplayPath,
		}
	}
	return &scriptReplayProxyFailure{
		exitCode: 66,
		message:  fmt.Sprintf("command replay %s could not read regular output %s: %v", name, output.DisplayPath, err),
	}
}

func compareScriptReplayOutput(name string, output *scriptReplayRuntimeOutput, want, got scriptReplayOutputSnapshot, phase string) error {
	if want.Digest != got.Digest {
		return &scriptReplayProxyFailure{
			exitCode: 66,
			message:  fmt.Sprintf("command replay %s output %s changed content %s", name, output.DisplayPath, phase),
		}
	}
	if want.ExecutableBits != got.ExecutableBits {
		return &scriptReplayProxyFailure{
			exitCode: 66,
			message:  fmt.Sprintf("command replay %s output %s changed executable bits %s", name, output.DisplayPath, phase),
		}
	}
	return nil
}

func snapshotRequiredScriptReplayOutput(name string, output *scriptReplayRuntimeOutput) (scriptReplayOutputSnapshot, error) {
	snapshot, available, err := snapshotScriptReplayOutput(output.Path)
	if err != nil {
		return scriptReplayOutputSnapshot{}, scriptReplayOutputFailure(name, output, err)
	}
	if !available {
		return scriptReplayOutputSnapshot{}, scriptReplayOutputFailure(name, output, nil)
	}
	return snapshot, nil
}

func equalScriptReplayArguments(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func startScriptReplayBroker(replays []*scriptReplayRuntimeManifest) (*scriptReplayBroker, error) {
	if len(replays) == 0 || len(replays) > maxReplayManifests {
		return nil, fmt.Errorf("command replay broker requires between 1 and %d manifests", maxReplayManifests)
	}
	for index, replay := range replays {
		if replay == nil {
			return nil, fmt.Errorf("command replay broker manifest %d is nil", index)
		}
	}
	var nonce [18]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, fmt.Errorf("create command replay endpoint identity: %w", err)
	}
	// Linux abstract sockets have no source-visible directory entry to unlink or
	// replace. The random name also prevents collisions between concurrent local
	// or remote actions sharing one network namespace.
	endpoint := "@linux-bzl-replay-" + base64.RawURLEncoding.EncodeToString(nonce[:])
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: endpoint})
	if err != nil {
		return nil, err
	}
	broker := &scriptReplayBroker{
		listener:    listener,
		endpoint:    endpoint,
		replays:     append([]*scriptReplayRuntimeManifest(nil), replays...),
		acceptDone:  make(chan error, 1),
		closeDone:   make(chan struct{}),
		connections: map[*net.UnixConn]bool{},
	}
	go func() { broker.acceptDone <- broker.serve() }()
	return broker, nil
}

func (broker *scriptReplayBroker) serve() error {
	for {
		connection, err := broker.listener.AcceptUnix()
		if err != nil {
			return err
		}
		broker.mu.Lock()
		if broker.closing {
			broker.mu.Unlock()
			_ = connection.Close()
			continue
		}
		broker.connections[connection] = true
		broker.handlers.Add(1)
		broker.mu.Unlock()
		go broker.handle(connection)
	}
}

func (broker *scriptReplayBroker) handle(connection *net.UnixConn) {
	defer func() {
		_ = connection.Close()
		broker.mu.Lock()
		delete(broker.connections, connection)
		broker.mu.Unlock()
		broker.handlers.Done()
	}()
	_ = connection.SetReadDeadline(time.Now().Add(scriptReplayIOTimeout))
	replayID, arguments, err := readScriptReplayRequest(connection)
	_ = connection.SetReadDeadline(time.Time{})
	if err != nil {
		err = fmt.Errorf("decode command replay request: %w", err)
	} else {
		err = broker.evaluate(replayID, arguments)
	}
	exitCode, message := scriptReplayResult(err)
	_ = connection.SetWriteDeadline(time.Now().Add(scriptReplayIOTimeout))
	_ = writeScriptReplayResponse(connection, exitCode, message)
}

func (broker *scriptReplayBroker) Close() error {
	broker.closeOnce.Do(func() {
		broker.mu.Lock()
		broker.closing = true
		listenerErr := broker.listener.Close()
		broker.mu.Unlock()
		acceptErr := <-broker.acceptDone
		broker.handlers.Wait()
		if listenerErr != nil && !errors.Is(listenerErr, net.ErrClosed) {
			broker.closeErr = listenerErr
		} else if acceptErr != nil && !errors.Is(acceptErr, net.ErrClosed) {
			broker.closeErr = acceptErr
		}
		close(broker.closeDone)
	})
	<-broker.closeDone
	return broker.closeErr
}

func (broker *scriptReplayBroker) evaluate(replayID uint32, arguments []string) error {
	if uint64(replayID) >= uint64(len(broker.replays)) {
		return fmt.Errorf("unknown command replay identity %d", replayID)
	}
	manifest := broker.replays[replayID]
	for _, invocation := range manifest.Invocations {
		if !equalScriptReplayArguments(arguments, invocation.Arguments) {
			continue
		}
		for _, output := range invocation.Outputs {
			if err := checkScriptReplayOutput(manifest.Name, output); err != nil {
				return err
			}
		}
		return nil
	}
	return &scriptReplayProxyFailure{
		exitCode: 64,
		message:  "command replay " + manifest.Name + " rejected undeclared arguments",
	}
}

func checkScriptReplayOutput(name string, output *scriptReplayRuntimeOutput) error {
	output.mu.Lock()
	defer output.mu.Unlock()
	current, err := snapshotRequiredScriptReplayOutput(name, output)
	if err != nil {
		return err
	}
	if output.Initial != nil {
		return compareScriptReplayOutput(name, output, *output.Initial, current, "before replay")
	}
	if output.receipt != nil {
		return compareScriptReplayOutput(name, output, *output.receipt, current, "after replay")
	}
	receipt := current
	output.receipt = &receipt
	return nil
}

func verifyScriptReplayReceipts(manifests []*scriptReplayRuntimeManifest) error {
	for _, manifest := range manifests {
		for _, invocation := range manifest.Invocations {
			for _, output := range invocation.Outputs {
				// A preexisting output is staged from the separately planned child
				// action, so its identity matters only at the replay boundary. An
				// initially absent output is owned by this source-script action; its
				// receipt must still match when that action finishes and collects it.
				output.mu.Lock()
				if output.Initial != nil || output.receipt == nil {
					output.mu.Unlock()
					continue
				}
				receipt := *output.receipt
				current, err := snapshotRequiredScriptReplayOutput(manifest.Name, output)
				if err != nil {
					output.mu.Unlock()
					return err
				}
				if err := compareScriptReplayOutput(manifest.Name, output, receipt, current, "after replay"); err != nil {
					output.mu.Unlock()
					return err
				}
				output.mu.Unlock()
			}
		}
	}
	return nil
}

func writeScriptReplayBytes(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func writeScriptReplayRequest(writer io.Writer, replayID uint32, arguments []string) error {
	if len(arguments) > maxReplayArguments {
		return fmt.Errorf("command replay request contains more than %d arguments", maxReplayArguments)
	}
	total := len(scriptReplayProtocol) + 8
	for index, argument := range arguments {
		if len(argument) > maxReplayValueBytes {
			return fmt.Errorf("command replay argument %d exceeds %d bytes", index, maxReplayValueBytes)
		}
		total += 4 + len(argument)
		if total > maxReplayProtocolBytes {
			return fmt.Errorf("command replay request exceeds %d bytes", maxReplayProtocolBytes)
		}
	}
	var header [16]byte
	copy(header[:8], scriptReplayProtocol)
	binary.BigEndian.PutUint32(header[8:12], replayID)
	binary.BigEndian.PutUint32(header[12:16], uint32(len(arguments)))
	if err := writeScriptReplayBytes(writer, header[:]); err != nil {
		return err
	}
	for _, argument := range arguments {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(argument)))
		if err := writeScriptReplayBytes(writer, size[:]); err != nil {
			return err
		}
		if err := writeScriptReplayBytes(writer, []byte(argument)); err != nil {
			return err
		}
	}
	return nil
}

func readScriptReplayRequest(reader io.Reader) (uint32, []string, error) {
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	if string(header[:8]) != scriptReplayProtocol {
		return 0, nil, fmt.Errorf("invalid protocol identity")
	}
	replayID := binary.BigEndian.Uint32(header[8:12])
	argumentCount := binary.BigEndian.Uint32(header[12:16])
	if argumentCount > maxReplayArguments {
		return 0, nil, fmt.Errorf("argument count %d exceeds %d", argumentCount, maxReplayArguments)
	}
	arguments := make([]string, int(argumentCount))
	total := len(header)
	for index := range arguments {
		var encodedSize [4]byte
		if _, err := io.ReadFull(reader, encodedSize[:]); err != nil {
			return 0, nil, err
		}
		size := binary.BigEndian.Uint32(encodedSize[:])
		if size > maxReplayValueBytes {
			return 0, nil, fmt.Errorf("argument %d exceeds %d bytes", index, maxReplayValueBytes)
		}
		total += len(encodedSize) + int(size)
		if total > maxReplayProtocolBytes {
			return 0, nil, fmt.Errorf("request exceeds %d bytes", maxReplayProtocolBytes)
		}
		value := make([]byte, int(size))
		if _, err := io.ReadFull(reader, value); err != nil {
			return 0, nil, err
		}
		arguments[index] = string(value)
	}
	var trailing [1]byte
	read, err := reader.Read(trailing[:])
	if read != 0 {
		return 0, nil, fmt.Errorf("request contains trailing bytes")
	}
	if err != io.EOF {
		if err == nil {
			err = io.ErrNoProgress
		}
		return 0, nil, fmt.Errorf("finish request: %w", err)
	}
	return replayID, arguments, nil
}

func writeScriptReplayResponse(writer io.Writer, exitCode int, message string) error {
	if exitCode != 0 && exitCode != 64 && exitCode != 66 && exitCode != 70 {
		return fmt.Errorf("invalid command replay exit code %d", exitCode)
	}
	if len(message) > maxReplayDiagnosticSize {
		message = message[:maxReplayDiagnosticSize]
	}
	var header [16]byte
	copy(header[:8], scriptReplayResponse)
	binary.BigEndian.PutUint32(header[8:12], uint32(exitCode))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(message)))
	if err := writeScriptReplayBytes(writer, header[:]); err != nil {
		return err
	}
	return writeScriptReplayBytes(writer, []byte(message))
}

func readScriptReplayResponse(reader io.Reader) (int, string, error) {
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, "", err
	}
	if string(header[:8]) != scriptReplayResponse {
		return 0, "", fmt.Errorf("invalid response protocol identity")
	}
	exitCode := int(binary.BigEndian.Uint32(header[8:12]))
	if exitCode != 0 && exitCode != 64 && exitCode != 66 && exitCode != 70 {
		return 0, "", fmt.Errorf("invalid response exit code %d", exitCode)
	}
	size := binary.BigEndian.Uint32(header[12:16])
	if size > maxReplayDiagnosticSize {
		return 0, "", fmt.Errorf("response diagnostic exceeds %d bytes", maxReplayDiagnosticSize)
	}
	message := make([]byte, int(size))
	if _, err := io.ReadFull(reader, message); err != nil {
		return 0, "", err
	}
	if exitCode != 0 && len(message) == 0 {
		return 0, "", fmt.Errorf("error response has no diagnostic")
	}
	return exitCode, string(message), nil
}

func scriptReplayResult(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	exitCode := 70
	var failure *scriptReplayProxyFailure
	if errors.As(err, &failure) {
		exitCode = failure.exitCode
	}
	return exitCode, err.Error()
}

func runScriptReplayProxy(endpoint string, replayID uint32, arguments []string) error {
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: endpoint})
	if err != nil {
		return &scriptReplayProxyFailure{exitCode: 70, message: "connect command replay broker: " + err.Error()}
	}
	defer connection.Close()
	if err := writeScriptReplayRequest(connection, replayID, arguments); err != nil {
		return &scriptReplayProxyFailure{exitCode: 70, message: "write command replay request: " + err.Error()}
	}
	if err := connection.CloseWrite(); err != nil {
		return &scriptReplayProxyFailure{exitCode: 70, message: "finish command replay request: " + err.Error()}
	}
	exitCode, message, err := readScriptReplayResponse(connection)
	if err != nil {
		return &scriptReplayProxyFailure{exitCode: 70, message: "read command replay response: " + err.Error()}
	}
	if exitCode == 0 {
		return nil
	}
	return &scriptReplayProxyFailure{exitCode: exitCode, message: message}
}

func executeScriptReplayProxyMode(arguments []string, stderr io.Writer) (bool, int) {
	if len(arguments) == 0 || arguments[0] != scriptReplayProxyMode {
		return false, 0
	}
	if len(arguments) < 3 {
		fmt.Fprintln(stderr, "command replay proxy is missing its broker endpoint or identity")
		return true, 70
	}
	replayID, parseErr := strconv.ParseUint(arguments[2], 10, 32)
	if parseErr != nil {
		fmt.Fprintln(stderr, "command replay proxy has an invalid replay identity")
		return true, 70
	}
	err := runScriptReplayProxy(arguments[1], uint32(replayID), arguments[3:])
	if err == nil {
		return true, 0
	}
	exitCode := 70
	var failure *scriptReplayProxyFailure
	if errors.As(err, &failure) {
		exitCode = failure.exitCode
	}
	fmt.Fprintln(stderr, err)
	return true, exitCode
}

func installScriptReplayProxy(directory, multicall, verifier, endpoint string, replayID uint32, manifest scriptReplayManifest) error {
	if err := validateReplayManifests([]scriptReplayManifest{manifest}, nil); err != nil {
		return err
	}
	if !strings.HasPrefix(endpoint, "@") || strings.ContainsRune(endpoint, 0) {
		return fmt.Errorf("command replay endpoint is not an abstract Unix socket address")
	}
	destination := filepath.Join(directory, manifest.Name)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var script strings.Builder
	script.WriteString("#!")
	script.WriteString(multicall)
	script.WriteString(" sh\n")
	script.WriteString("exec ")
	script.WriteString(shellQuote(verifier))
	script.WriteByte(' ')
	script.WriteString(shellQuote(scriptReplayProxyMode))
	script.WriteByte(' ')
	script.WriteString(shellQuote(endpoint))
	script.WriteByte(' ')
	script.WriteString(strconv.FormatUint(uint64(replayID), 10))
	script.WriteString(" \"$@\"\n")
	if err := os.WriteFile(destination, []byte(script.String()), 0o700); err != nil {
		return err
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validateScriptToolName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("invalid tool name %q", name)
	}
	// POSIX test and the shell conditional applet are emitted by BusyBox's
	// --list protocol. Both are safe single path components and must not make a
	// complete, checksum-pinned multicall runtime unusable.
	if name == "[" || name == "[[" {
		return nil
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_+.@-", character) {
			continue
		}
		return fmt.Errorf("invalid tool name %q", name)
	}
	return nil
}

func parseToolBindings(values []string) (map[string]string, error) {
	tools := map[string]string{}
	for _, value := range values {
		name, executable, ok := strings.Cut(value, "=")
		if !ok || executable == "" {
			return nil, fmt.Errorf("expected NAME=EXECUTABLE, got %q", value)
		}
		if err := validateScriptToolName(name); err != nil {
			return nil, err
		}
		if _, exists := tools[name]; exists {
			return nil, fmt.Errorf("repeated external tool %q", name)
		}
		tools[name] = executable
	}
	return tools, nil
}

func expandScriptTreeBindingsWithLiteralOffsets(script string, trees map[string]string, literalOffsets map[int]bool) (string, error) {
	used := map[string]bool{}
	protected := map[int]bool{}
	for offset := range literalOffsets {
		if offset < 0 || offset >= len(script) || !strings.HasPrefix(script[offset:], "${tree:") {
			return "", fmt.Errorf("literal evaluated-script tree offset %d does not identify a tree marker", offset)
		}
		protected[offset] = false
	}
	var out strings.Builder
	for cursor := 0; ; {
		relative := strings.Index(script[cursor:], "${tree:")
		if relative < 0 {
			out.WriteString(script[cursor:])
			break
		}
		start := cursor + relative
		out.WriteString(script[cursor:start])
		relativeEnd := strings.IndexByte(script[start+len("${tree:"):], '}')
		if relativeEnd < 0 {
			return "", fmt.Errorf("unterminated evaluated-script tree binding")
		}
		end := start + len("${tree:") + relativeEnd
		name := script[start+len("${tree:") : end]
		if err := validateScriptToolName(name); err != nil {
			return "", fmt.Errorf("invalid evaluated-script tree binding: %w", err)
		}
		if _, literal := protected[start]; literal {
			protected[start] = true
			out.WriteString(script[start : end+1])
			cursor = end + 1
			continue
		}
		root, ok := trees[name]
		if !ok || root == "" || strings.ContainsRune(root, 0) {
			return "", fmt.Errorf("unbound evaluated-script tree %q", name)
		}
		used[name] = true
		out.WriteString(root)
		cursor = end + 1
	}
	for name := range trees {
		if !used[name] {
			return "", fmt.Errorf("unused evaluated-script tree %q", name)
		}
	}
	for offset, consumed := range protected {
		if !consumed {
			return "", fmt.Errorf("unused literal evaluated-script tree offset %d", offset)
		}
	}
	return out.String(), nil
}

func parseLiteralTreeOffsets(values []string) (map[int]bool, error) {
	offsets := map[int]bool{}
	for _, value := range values {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return nil, fmt.Errorf("invalid literal evaluated-script tree offset %q", value)
		}
		if offsets[offset] {
			return nil, fmt.Errorf("repeated literal evaluated-script tree offset %d", offset)
		}
		offsets[offset] = true
	}
	return offsets, nil
}

func environmentMap(values []string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		if name, data, ok := strings.Cut(value, "="); ok {
			out[name] = data
		}
	}
	return out
}

func environmentList(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for index, name := range names {
		out[index] = name + "=" + values[name]
	}
	return out
}

func resolveScriptContent(sourceScript, rawContent, base64Content string) (string, error) {
	selected := 0
	for _, value := range []string{sourceScript, rawContent, base64Content} {
		if value != "" {
			selected++
		}
	}
	if selected > 1 {
		return "", fmt.Errorf("source script, raw evaluated script content, and base64 evaluated script content are mutually exclusive")
	}
	if base64Content == "" {
		return rawContent, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(base64Content)
	if err != nil {
		return "", fmt.Errorf("decode evaluated Kbuild recipe: %w", err)
	}
	return string(decoded), nil
}

func main() {
	if handled, exitCode := executeScriptReplayProxyMode(os.Args[1:], os.Stderr); handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}
	var appletFlags, interpreterArgs, literalTreeOffsetFlags, replayFlags, requiredAppletFlags, toolFlags, treeFlags repeatedFlag
	interpreter := flag.String("interpreter", "", "declared interpreter executable")
	multicall := flag.String("multicall", "", "optional declared multicall executable used to populate PATH")
	script := flag.String("script", "", "declared source script")
	scriptContentRaw := flag.String("script_content", "", "raw evaluated Kbuild recipe")
	scriptContentBase64 := flag.String("script_content_base64", "", "base64-encoded evaluated Kbuild recipe")
	scriptStdin := flag.Bool("script_stdin", false, "read evaluated Kbuild recipe from stdin")
	flag.Var(&interpreterArgs, "interpreter_arg", "interpreter argument before the source script (repeatable)")
	flag.Var(&appletFlags, "applet", "runtime applet override NAME=EXECUTABLE (repeatable)")
	flag.Var(&requiredAppletFlags, "require_applet", "runtime applet required by evaluated script (repeatable)")
	flag.Var(&replayFlags, "replay_base64", "base64-encoded exact command replay manifest (repeatable)")
	flag.Var(&toolFlags, "tool", "external script tool NAME=EXECUTABLE (repeatable)")
	flag.Var(&treeFlags, "tree", "evaluated-script tree binding NAME=ROOT (repeatable)")
	flag.Var(&literalTreeOffsetFlags, "literal_tree_offset", "byte offset of a literal evaluated-script tree marker (repeatable)")
	flag.Parse()
	applets, err := parseToolBindings(appletFlags)
	tools := map[string]string{}
	if err == nil {
		tools, err = parseToolBindings(toolFlags)
	}
	trees := map[string]string{}
	if err == nil {
		trees, err = parseToolBindings(treeFlags)
	}
	literalTreeOffsets := map[int]bool{}
	if err == nil {
		literalTreeOffsets, err = parseLiteralTreeOffsets(literalTreeOffsetFlags)
	}
	var replays []scriptReplayManifest
	if err == nil {
		replays, err = decodeReplayManifests(replayFlags)
	}
	scriptContent := ""
	if err == nil {
		scriptContent, err = resolveScriptContent(*script, *scriptContentRaw, *scriptContentBase64)
	}
	if err == nil {
		var contracts map[string]toolaction.Contract
		contracts, err = toolaction.Decode(os.Getenv(toolaction.EnvironmentName))
		if err != nil {
			err = fmt.Errorf("decode configured tool action contracts: %w", err)
		}
		if err == nil {
			err = runScript(scriptRunOptions{
				interpreter: *interpreter, interpreterArgs: interpreterArgs, multicall: *multicall,
				script: *script, scriptContent: scriptContent, scriptStdin: *scriptStdin, scriptArgs: flag.Args(), applets: applets, requiredApplets: requiredAppletFlags, tools: tools, trees: trees, literalTreeOffsets: literalTreeOffsets, toolContracts: contracts,
				runtimeToolPath: os.Getenv(toolaction.RuntimeToolPathEnvironmentName),
				toolsetHandoff:  os.Getenv(toolsetpath.HandoffEnvironmentName),
				replays:         replays,
				stdin:           os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
			})
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scriptrun: %v\n", err)
		os.Exit(1)
	}
}
