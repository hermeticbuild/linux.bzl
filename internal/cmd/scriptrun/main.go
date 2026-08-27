// scriptrun executes one declared source script or one planner-evaluated script
// through one declared interpreter. It never searches the ambient PATH.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
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
	scriptAppletRolePrefix  = "script-applet-"
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
		script = filepath.Join(runtimeRoot, "evaluated-kbuild-recipe.sh")
		if err := os.WriteFile(script, []byte(opts.scriptContent), 0o600); err != nil {
			return fmt.Errorf("materialize evaluated Kbuild recipe: %w", err)
		}
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
		if err := installScriptToolProxy(toolDirectory, multicall, name, tool, opts.toolContracts[name], linkContract); err != nil {
			return fmt.Errorf("install external tool %s: %w", name, err)
		}
	}
	replays := append([]scriptReplayManifest(nil), opts.replays...)
	sort.Slice(replays, func(left, right int) bool { return replays[left].Name < replays[right].Name })
	for _, replay := range replays {
		if err := installScriptReplayProxy(toolDirectory, multicall, replay); err != nil {
			return fmt.Errorf("install command replay %s: %w", replay.Name, err)
		}
	}
	arguments := append([]string(nil), opts.interpreterArgs...)
	arguments = append(arguments, script)
	arguments = append(arguments, opts.scriptArgs...)
	command := exec.Command(interpreter, arguments...)
	environment := environmentMap(os.Environ())
	for _, name := range []string{"BASH_ENV", "CDPATH", "ENV", "GLOBIGNORE", "PATH", "SHELLOPTS", "TMPDIR", toolaction.EnvironmentName, toolaction.RuntimeToolPathEnvironmentName} {
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
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute source script: %w", err)
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

func installScriptToolProxy(
	directory, multicall, name, executable string,
	contract toolaction.Contract,
	linkContract *toolaction.Contract,
) error {
	if err := validateScriptToolName(name); err != nil {
		return err
	}
	contracts := map[string]toolaction.Contract{name: contract}
	if linkContract != nil {
		linkRole, ok := toolaction.LinkContractRole(name)
		if !ok {
			return fmt.Errorf("tool %q cannot have a driver-link companion contract", name)
		}
		contracts[linkRole] = *linkContract
	}
	if err := toolaction.Validate(contracts); err != nil {
		return err
	}
	destination := filepath.Join(directory, name)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var script strings.Builder
	script.WriteString("#!")
	script.WriteString(multicall)
	script.WriteString(" sh\n")
	if linkContract != nil {
		script.WriteString("linux_bzl_link=\nlinux_bzl_expect_output=\n")
		script.WriteString("for linux_bzl_arg do\n")
		script.WriteString("  if [ -n \"$linux_bzl_expect_output\" ]; then\n")
		script.WriteString("    linux_bzl_link=1\n    linux_bzl_expect_output=\n    continue\n  fi\n")
		script.WriteString("  case \"$linux_bzl_arg\" in\n")
		script.WriteString("    -c|-S|-E|-M|-MM|-fsyntax-only) linux_bzl_link=; break ;;\n")
		script.WriteString("    -o) linux_bzl_expect_output=1 ;;\n")
		script.WriteString("    -o?*) linux_bzl_link=1 ;;\n")
		script.WriteString("  esac\ndone\n")
		script.WriteString("if [ \"$linux_bzl_link\" = 1 ]; then\n")
		writeScriptToolContractInvocation(&script, executable, *linkContract, "  ")
		script.WriteString("fi\n")
	}
	writeScriptToolContractInvocation(&script, executable, contract, "")
	if err := os.WriteFile(destination, []byte(script.String()), 0o700); err != nil {
		return err
	}
	return nil
}

func writeScriptToolContractInvocation(script *strings.Builder, executable string, contract toolaction.Contract, indent string) {
	for _, environmentName := range sortedEnvironmentNames(contract.Environment) {
		script.WriteString(indent)
		script.WriteString("export ")
		script.WriteString(environmentName)
		script.WriteString("=")
		script.WriteString(shellQuote(contract.Environment[environmentName]))
		script.WriteByte('\n')
	}
	script.WriteString(indent)
	script.WriteString("exec ")
	script.WriteString(shellQuote(executable))
	if len(contract.Arguments) == 0 {
		script.WriteString(" \"$@\"")
	} else {
		for _, argument := range contract.Arguments {
			if argument == toolaction.KbuildArgumentsSentinel {
				script.WriteString(" \"$@\"")
			} else {
				script.WriteByte(' ')
				script.WriteString(shellQuote(argument))
			}
		}
	}
	script.WriteByte('\n')
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
			encodedArguments, err := json.Marshal(append([]string{}, invocation.Arguments...))
			if err != nil {
				return fmt.Errorf("encode command replay %q invocation %d arguments: %w", manifest.Name, invocationIndex, err)
			}
			argumentsKey := string(encodedArguments)
			if seenInvocations[argumentsKey] {
				return fmt.Errorf("command replay %q repeats invocation arguments %s", manifest.Name, argumentsKey)
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

func installScriptReplayProxy(directory, multicall string, manifest scriptReplayManifest) error {
	if err := validateReplayManifests([]scriptReplayManifest{manifest}, nil); err != nil {
		return err
	}
	destination := filepath.Join(directory, manifest.Name)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var script strings.Builder
	script.WriteString("#!")
	script.WriteString(multicall)
	script.WriteString(" sh\n")
	for _, invocation := range manifest.Invocations {
		script.WriteString("if [ \"$#\" -eq ")
		script.WriteString(strconv.Itoa(len(invocation.Arguments)))
		script.WriteString(" ]")
		for index, argument := range invocation.Arguments {
			script.WriteString(" && [ \"${")
			script.WriteString(strconv.Itoa(index + 1))
			script.WriteString("}\" = ")
			script.WriteString(shellQuote(argument))
			script.WriteString(" ]")
		}
		script.WriteString("; then\n")
		for _, output := range invocation.Outputs {
			script.WriteString("  if [ ! -f ")
			script.WriteString(shellQuote(output))
			script.WriteString(" ]; then\n")
			script.WriteString("    printf '%s\\n' ")
			script.WriteString(shellQuote("command replay " + manifest.Name + " is missing regular output " + output))
			script.WriteString(" >&2\n")
			script.WriteString("    exit 66\n")
			script.WriteString("  fi\n")
		}
		script.WriteString("  exit 0\n")
		script.WriteString("fi\n")
	}
	script.WriteString("printf '%s\\n' ")
	script.WriteString(shellQuote("command replay " + manifest.Name + " rejected undeclared arguments"))
	script.WriteString(" >&2\n")
	script.WriteString("exit 64\n")
	if err := os.WriteFile(destination, []byte(script.String()), 0o700); err != nil {
		return err
	}
	return nil
}

func sortedEnvironmentNames(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
				runtimeToolPath: os.Getenv(toolaction.RuntimeToolPathEnvironmentName), replays: replays,
				stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
			})
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scriptrun: %v\n", err)
		os.Exit(1)
	}
}
