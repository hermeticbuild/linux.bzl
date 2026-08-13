package kconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	linuxProbeTimeout                = 20 * time.Second
	linuxProbeOutputLimit            = 64 << 10
	linuxProbeMinimumLLVMVersion     = 150000
	linuxProbeMinimumGCCVersion      = 80100
	linuxProbeMinimumBinutilsVersion = 23000
)

// LinuxToolProbeOptions describes integrity-pinned compiler tools used to answer
// Kconfig and Kbuild capability probes. Tools are executed directly with argv;
// probe text is never passed to a command shell.
type LinuxToolProbeOptions struct {
	Profile            string
	Architecture       string
	TargetTriple       string
	CompilerPath       string
	LinkerPath         string
	ArchiverPath       string
	NMPath             string
	ObjcopyPath        string
	CompilerArgs       []string
	CompilerSuffixArgs []string
	LinkerArgs         []string
	LinkerSuffixArgs   []string
	// DisableCCCanLink and DisableGCCPlugins make capabilities whose complete
	// runtime closure is unavailable fail closed without consulting ambient
	// executor files. Both default to false for existing callers.
	DisableCCCanLink  bool
	DisableGCCPlugins bool
	TempDir           string
	Identity          string
	Timeout           time.Duration
	OutputLimit       int
}

// LinuxToolProbe executes and memoizes safe compiler/linker capability probes.
type LinuxToolProbe struct {
	profile            LinuxTargetProfile
	compilerPath       string
	linkerPath         string
	archiverPath       string
	nmPath             string
	objcopyPath        string
	compilerArgs       []string
	compilerSuffixArgs []string
	linkerArgs         []string
	linkerSuffixArgs   []string
	disableCCCanLink   bool
	disableGCCPlugins  bool
	tempDir            string
	identity           string
	timeout            time.Duration
	outputLimit        int
	mu                 sync.Mutex
	cache              map[string]bool
	compilerFamily     string
	compilerName       string
	compilerVersion    string
	compilerCode       int
	assemblerName      string
	assemblerCode      int
	linkerName         string
	linkerVersion      string
	linkerCode         int
}

func NewLinuxToolProbe(opts LinuxToolProbeOptions) (*LinuxToolProbe, error) {
	profile, err := LinuxTargetProfileByName(opts.Profile)
	if err != nil {
		return nil, err
	}
	if err := profile.ValidateTargetIdentity(opts.Architecture, opts.TargetTriple); err != nil {
		return nil, err
	}
	if opts.CompilerPath == "" || opts.LinkerPath == "" || opts.ArchiverPath == "" || opts.NMPath == "" || opts.ObjcopyPath == "" {
		return nil, fmt.Errorf("real Linux probes require compiler, linker, archiver, nm, and objcopy paths")
	}
	if err := validateCompilerPrefixArgs(opts.CompilerArgs); err != nil {
		return nil, fmt.Errorf("invalid configured compiler prefix: %w", err)
	}
	if err := validateCompilerPrefixArgs(opts.CompilerSuffixArgs); err != nil {
		return nil, fmt.Errorf("invalid configured compiler suffix: %w", err)
	}
	if err := validateLinkerDriverPrefixArgs(opts.LinkerArgs); err != nil {
		return nil, fmt.Errorf("invalid configured linker prefix: %w", err)
	}
	if err := validateLinkerDriverPrefixArgs(opts.LinkerSuffixArgs); err != nil {
		return nil, fmt.Errorf("invalid configured linker suffix: %w", err)
	}
	compilerPath, err := filepath.Abs(opts.CompilerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve compiler path: %w", err)
	}
	linkerPath, err := filepath.Abs(opts.LinkerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve linker path: %w", err)
	}
	archiverPath, err := filepath.Abs(opts.ArchiverPath)
	if err != nil {
		return nil, fmt.Errorf("resolve archiver path: %w", err)
	}
	nmPath, err := filepath.Abs(opts.NMPath)
	if err != nil {
		return nil, fmt.Errorf("resolve nm path: %w", err)
	}
	objcopyPath, err := filepath.Abs(opts.ObjcopyPath)
	if err != nil {
		return nil, fmt.Errorf("resolve objcopy path: %w", err)
	}
	for name, path := range map[string]string{
		"compiler": compilerPath,
		"linker":   linkerPath,
		"archiver": archiverPath,
		"nm":       nmPath,
		"objcopy":  objcopyPath,
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("stat probe %s %q: %w", name, path, statErr)
		}
		if !probeToolModeIsExecutable(runtime.GOOS, info.Mode()) {
			return nil, fmt.Errorf("probe %s %q is not an executable file", name, path)
		}
	}
	p := &LinuxToolProbe{
		profile: profile, compilerPath: compilerPath, linkerPath: linkerPath,
		archiverPath: archiverPath, nmPath: nmPath, objcopyPath: objcopyPath,
		compilerArgs:       append([]string(nil), opts.CompilerArgs...),
		compilerSuffixArgs: append([]string(nil), opts.CompilerSuffixArgs...),
		linkerArgs:         append([]string(nil), opts.LinkerArgs...),
		linkerSuffixArgs:   append([]string(nil), opts.LinkerSuffixArgs...),
		disableCCCanLink:   opts.DisableCCCanLink,
		disableGCCPlugins:  opts.DisableGCCPlugins,
		tempDir:            opts.TempDir, identity: opts.Identity, cache: map[string]bool{},
		timeout: opts.Timeout, outputLimit: opts.OutputLimit,
	}
	if p.timeout <= 0 {
		p.timeout = linuxProbeTimeout
	}
	if p.outputLimit <= 0 {
		p.outputLimit = linuxProbeOutputLimit
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	// Linux's cc-version.sh identifies the compiler from predefined macros,
	// not from the branding printed by --version.  First use the unadorned
	// version command only as a compatibility hint for the legacy implicit
	// Clang target prefix, then make the configured preprocessing invocation
	// authoritative below.  In particular, this permits compiler wrappers
	// whose own --version output does not identify the wrapped frontend.
	compilerText, versionErr := p.run(ctx, compilerPath, []string{"--version"}, nil)
	if versionErr != nil {
		if _, ok := versionErr.(*exec.ExitError); !ok {
			return nil, fmt.Errorf("read compiler version text: %w", versionErr)
		}
	}
	if family, name, _, code, parseErr := parseCompilerVersion(compilerText); parseErr == nil {
		p.compilerFamily = family
		p.compilerName = name
		p.compilerCode = code
	}
	configuredCompilerArgs := p.configuredCompilerArgs()
	if len(configuredCompilerArgs) != 0 || len(p.compilerSuffixArgs) != 0 {
		versionArgs := p.compilerInvocation("--version")
		compilerText, versionErr = p.run(ctx, compilerPath, versionArgs, nil)
		if versionErr != nil {
			if _, ok := versionErr.(*exec.ExitError); !ok {
				return nil, fmt.Errorf("read configured compiler version text: %w", versionErr)
			}
		}
	}
	p.compilerVersion = strings.TrimSpace(strings.SplitN(compilerText, "\n", 2)[0])
	compilerInfo, err := p.identifyConfiguredCompiler(ctx)
	if err != nil {
		return nil, err
	}
	p.compilerFamily, p.compilerName, p.compilerCode, err = parseCompilerMacroIdentity(compilerInfo)
	if err != nil {
		return nil, err
	}
	if p.compilerFamily == "clang" && p.compilerCode < linuxProbeMinimumLLVMVersion {
		return nil, fmt.Errorf("probe clang version is %d, want at least Linux 6.18 LLVM minimum %d", p.compilerCode, linuxProbeMinimumLLVMVersion)
	}
	if p.compilerFamily == "gcc" && p.compilerCode < linuxProbeMinimumGCCVersion {
		return nil, fmt.Errorf("probe GCC version is %d, want at least %d", p.compilerCode, linuxProbeMinimumGCCVersion)
	}
	linkerText, err := p.run(ctx, linkerPath, p.linkerInvocation("--version"), nil)
	if err != nil {
		return nil, fmt.Errorf("identify linker probe tool: %w", err)
	}
	p.linkerName, p.linkerVersion, p.linkerCode, err = parseLinkerVersion(linkerText)
	if err != nil {
		return nil, err
	}
	switch p.linkerName {
	case linuxProbeLDName:
		if p.linkerCode < linuxProbeMinimumLLVMVersion {
			return nil, fmt.Errorf("probe ld.lld version is %d, want at least Linux 6.18 LLVM minimum %d", p.linkerCode, linuxProbeMinimumLLVMVersion)
		}
	case "BFD":
		if p.linkerCode < linuxProbeMinimumBinutilsVersion {
			return nil, fmt.Errorf("probe GNU ld version is %d, want at least %d", p.linkerCode, linuxProbeMinimumBinutilsVersion)
		}
	default:
		return nil, fmt.Errorf("probe linker has unsupported family %q", p.linkerName)
	}
	assemblerText := compilerText
	integratedAssembler := false
	if p.compilerFamily == "clang" {
		integratedAssembler, err = p.detectClangIntegratedAssembler(ctx)
		if err != nil {
			return nil, err
		}
	}
	if integratedAssembler {
		p.assemblerName = linuxProbeASName
		p.assemblerCode = linuxProbeASVersion
	} else {
		assemblerArgs := p.compilerInvocation("-Wa,--version", "-c", "-x", "assembler-with-cpp", os.DevNull, "-o", os.DevNull)
		assemblerText, err = p.run(ctx, compilerPath, assemblerArgs, nil)
		if err != nil {
			return nil, fmt.Errorf("identify assembler through compiler driver: %w", err)
		}
		p.assemblerName, p.assemblerCode, err = parseGNUAssemblerVersion(assemblerText)
		if err != nil {
			return nil, err
		}
		if p.assemblerCode < linuxProbeMinimumBinutilsVersion {
			return nil, fmt.Errorf("probe GNU assembler version is %d, want at least %d", p.assemblerCode, linuxProbeMinimumBinutilsVersion)
		}
	}
	archiverText, err := p.run(ctx, archiverPath, []string{"--version"}, nil)
	if err != nil {
		return nil, fmt.Errorf("identify archiver probe tool: %w", err)
	}
	nmText, err := p.run(ctx, nmPath, []string{"--version"}, nil)
	if err != nil {
		return nil, fmt.Errorf("identify nm probe tool: %w", err)
	}
	objcopyText, err := p.run(ctx, objcopyPath, []string{"--version"}, nil)
	if err != nil {
		return nil, fmt.Errorf("identify objcopy probe tool: %w", err)
	}
	if p.identity == "" {
		p.identity, err = toolIdentityWithVersionLines(
			[]string{compilerText, compilerInfo, assemblerText, linkerText, archiverText, nmText, objcopyText},
			compilerPath,
			linkerPath,
			archiverPath,
			nmPath,
			objcopyPath,
		)
		if err != nil {
			return nil, err
		}
	}
	p.identity = extendToolIdentity(
		p.identity,
		p.profile,
		p.compilerArgs,
		p.compilerSuffixArgs,
		p.linkerArgs,
		p.linkerSuffixArgs,
		!p.disableCCCanLink,
		!p.disableGCCPlugins,
	)
	return p, nil
}

const linuxCompilerIdentitySource = `#if defined(__clang__)
Clang __clang_major__ __clang_minor__ __clang_patchlevel__
#elif defined(__GNUC__)
GCC __GNUC__ __GNUC_MINOR__ __GNUC_PATCHLEVEL__
#else
unknown
#endif
`

// identifyConfiguredCompiler reproduces the substantive part of Linux's
// scripts/cc-version.sh: preprocess predefined compiler macros through the
// complete configured compiler prefix.  stderr is intentionally ignored just
// as cc-version.sh redirects it, while a non-zero compiler exit remains fatal.
func (p *LinuxToolProbe) identifyConfiguredCompiler(ctx context.Context) (string, error) {
	args := p.compilerInvocation("-E", "-P", "-x", "c", "-")
	stdout, _, err := p.runSeparate(ctx, p.compilerPath, args, []byte(linuxCompilerIdentitySource))
	if err != nil {
		return "", fmt.Errorf("identify configured compiler from predefined macros: %w", err)
	}
	return stdout, nil
}

var clangCC1AssemblerPattern = regexp.MustCompile(`(?:^|[\s"])-cc1as(?:[\s"]|$)`)

func (p *LinuxToolProbe) detectClangIntegratedAssembler(ctx context.Context) (bool, error) {
	selected := ""
	for _, arg := range p.compilerInvocation() {
		if arg == "-fintegrated-as" || arg == "-fno-integrated-as" {
			selected = arg
		}
	}
	if selected != "" {
		return selected == "-fintegrated-as", nil
	}
	args := p.compilerInvocation("-###", "-c", "-x", "assembler-with-cpp", os.DevNull, "-o", os.DevNull)
	trace, err := p.run(ctx, p.compilerPath, args, nil)
	if err != nil {
		return false, fmt.Errorf("identify Clang assembler selection: %w", err)
	}
	if strings.TrimSpace(trace) == "" {
		return false, fmt.Errorf("identify Clang assembler selection: empty -### output")
	}
	return clangCC1AssemblerPattern.MatchString(trace), nil
}

func probeToolModeIsExecutable(goos string, mode os.FileMode) bool {
	if mode.IsDir() {
		return false
	}
	// Windows does not represent executable files with Unix permission bits:
	// os.Stat reports ordinary .exe files as 0666. exec.Command performs the
	// authoritative executable-file validation when the probe is launched.
	return goos == "windows" || mode&0o111 != 0
}

func (p *LinuxToolProbe) Identity() string { return p.identity }

func (p *LinuxToolProbe) CompilerFamily() string { return p.compilerFamily }

func (p *LinuxToolProbe) CompilerVersionText() string { return p.compilerVersion }

func (p *LinuxToolProbe) ClangFlags() string {
	if p.compilerFamily != "clang" {
		return ""
	}
	if p.assemblerName == linuxProbeASName {
		return "-fintegrated-as"
	}
	return "-fno-integrated-as"
}

func (p *LinuxToolProbe) compilerToolName() string {
	return linuxProbeToolName(p.compilerPath)
}

func (p *LinuxToolProbe) linkerToolName() string {
	return linuxProbeToolName(p.linkerPath)
}

func (p *LinuxToolProbe) ArchiverPath() string { return p.archiverPath }

func (p *LinuxToolProbe) NMPath() string { return p.nmPath }

func (p *LinuxToolProbe) ObjcopyPath() string { return p.objcopyPath }

func probeToolValueMatches(value, selectedPath string) bool {
	value = strings.Trim(value, `"'`)
	if value == "" {
		return false
	}
	if absolute, err := filepath.Abs(value); err == nil && filepath.Clean(absolute) == filepath.Clean(selectedPath) {
		return true
	}
	return !strings.ContainsAny(value, `/\\`) && linuxProbeToolName(value) == linuxProbeToolName(selectedPath)
}

func (p *LinuxToolProbe) configuredCompilerArgs() []string {
	if len(p.compilerArgs) != 0 {
		return append([]string(nil), p.compilerArgs...)
	}
	// Before callers could pass the configured CcToolchain command line, the
	// measured probe always supplied Clang's target explicitly. Preserve that
	// behavior for a legacy caller probing a non-host Linux target. GCC has no
	// equivalent driver option; a GCC cross compiler must encode its target in
	// the selected executable or in the configured compiler prefix.
	if p.compilerFamily == "clang" && !linuxProbeProfileMatchesHost(p.profile) {
		return []string{"--target=" + p.profile.TargetTriple}
	}
	return nil
}

func (p *LinuxToolProbe) compilerInvocation(arguments ...string) []string {
	result := append([]string(nil), p.configuredCompilerArgs()...)
	result = append(result, arguments...)
	return append(result, p.compilerSuffixArgs...)
}

func (p *LinuxToolProbe) linkerInvocation(arguments ...string) []string {
	result := append([]string(nil), p.linkerArgs...)
	result = append(result, arguments...)
	return append(result, p.linkerSuffixArgs...)
}

func linuxProbeProfileMatchesHost(profile LinuxTargetProfile) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	switch runtime.GOARCH {
	case "amd64":
		return profile.Name == "x86_64"
	case "arm64":
		return profile.Name == "aarch64"
	case "arm":
		return profile.Name == "armv7"
	case "riscv64":
		return profile.Name == "riscv64"
	case "ppc64le":
		return profile.Name == "ppc64le"
	default:
		return false
	}
}

func toolIdentity(paths ...string) (string, error) {
	return toolIdentityWithVersionLines(nil, paths...)
}

func toolIdentityWithVersionLines(versionOutputs []string, paths ...string) (string, error) {
	h := sha256.New()
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("hash probe tool %q: %w", path, err)
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash probe tool %q: %w", path, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close probe tool %q: %w", path, closeErr)
		}
		h.Write([]byte{0})
	}
	for _, output := range versionOutputs {
		line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
		h.Write([]byte(line))
		h.Write([]byte{0})
	}
	return "sha256-" + hex.EncodeToString(h.Sum(nil)), nil
}

func extendToolIdentity(base string, profile LinuxTargetProfile, compilerArgs, compilerSuffixArgs, linkerArgs, linkerSuffixArgs []string, policy ...bool) string {
	allowCCCanLink := true
	allowGCCPlugins := true
	if len(policy) > 0 {
		allowCCCanLink = policy[0]
	}
	if len(policy) > 1 {
		allowGCCPlugins = policy[1]
	}
	h := sha256.New()
	values := append([]string{
		"linux.bzl/measured-toolchain/v2",
		base,
		profile.Name,
		profile.TargetTriple,
		"compiler-args",
	}, compilerArgs...)
	values = append(values, "compiler-suffix-args")
	values = append(values, compilerSuffixArgs...)
	values = append(values, "linker-args")
	values = append(values, linkerArgs...)
	values = append(values, "linker-suffix-args")
	values = append(values, linkerSuffixArgs...)
	values = append(values,
		"allow-cc-can-link", strconv.FormatBool(allowCCCanLink),
		"allow-gcc-plugins", strconv.FormatBool(allowGCCPlugins),
	)
	for _, value := range values {
		fmt.Fprintf(h, "%d:", len(value))
		h.Write([]byte(value))
	}
	return "sha256-" + hex.EncodeToString(h.Sum(nil))
}

func validateCompilerPrefixArgs(args []string) error {
	for i, arg := range args {
		if err := validateProbeToken(arg); err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
		if strings.HasPrefix(arg, "@") {
			return fmt.Errorf("response-file argument %q is prohibited", arg)
		}
		switch arg {
		case "-c", "-S", "-E", "-M", "-MM", "-MD", "-MMD", "-o", "--output", "-MF", "-MT", "-MQ", "-MJ", "--serialize-diagnostics", "-save-temps", "--save-temps":
			return fmt.Errorf("compiler-controlled input, output, or mode argument %q is prohibited", arg)
		}
		for _, prefix := range []string{
			"--output=", "-MF=", "-MJ=", "--serialize-diagnostics=", "-save-temps=", "--save-temps=",
		} {
			if strings.HasPrefix(arg, prefix) {
				return fmt.Errorf("compiler-controlled output argument %q is prohibited", arg)
			}
		}
	}
	return nil
}

func validateLinkerDriverPrefixArgs(args []string) error {
	expectOperand := ""
	for i, arg := range args {
		if err := validateProbeToken(arg); err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
		if expectOperand != "" {
			if strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, `\`) {
				return fmt.Errorf("unsafe %s operand %q", expectOperand, arg)
			}
			expectOperand = ""
			continue
		}
		switch arg {
		case "-c", "-S", "-E", "-M", "-MM", "-MD", "-MMD", "-o", "--output", "-MF", "-MT", "-MQ", "-MJ", "--serialize-diagnostics", "-save-temps", "--save-temps":
			return fmt.Errorf("linker-driver-controlled input, output, or mode argument %q is prohibited", arg)
		}
		for _, prefix := range []string{
			"--output=", "-MF=", "-MJ=", "--serialize-diagnostics=", "-save-temps=", "--save-temps=",
		} {
			if strings.HasPrefix(arg, prefix) {
				return fmt.Errorf("linker-driver-controlled output argument %q is prohibited", arg)
			}
		}
		if !strings.HasPrefix(arg, "-") {
			return fmt.Errorf("linker-driver input or positional argument %q is prohibited", arg)
		}
		if arg == "-target" || arg == "-resource-dir" {
			expectOperand = arg
		}
	}
	if expectOperand != "" {
		return fmt.Errorf("missing operand for final linker-driver option %s", expectOperand)
	}
	return nil
}

var llvmVersionPattern = regexp.MustCompile(`(?i)(?:clang version|LLD(?: version)?) ([0-9]+)\.([0-9]+)\.([0-9]+)`)
var semanticVersionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

func parseLLVMVersion(output, tool string) (string, int, error) {
	line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	match := llvmVersionPattern.FindStringSubmatch(line)
	if match == nil {
		return "", 0, fmt.Errorf("probe %s returned an unsupported version line %q", tool, line)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	if minor > 99 || patch > 99 {
		return "", 0, fmt.Errorf("probe %s version cannot be represented by Linux: %q", tool, line)
	}
	return line, major*10000 + minor*100 + patch, nil
}

func parseCompilerVersion(output string) (string, string, string, int, error) {
	line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	lower := strings.ToLower(line)
	if strings.Contains(lower, "clang version") {
		text, code, err := parseLLVMVersion(line, "clang")
		return "clang", linuxProbeCCName, text, code, err
	}
	if strings.Contains(lower, "gcc") || strings.Contains(lower, "gnu compiler collection") {
		code, err := parseLastSemanticVersion(line, "GCC")
		return "gcc", "GCC", line, code, err
	}
	return "", "", "", 0, fmt.Errorf("probe compiler returned an unsupported version line %q", line)
}

func parseCompilerMacroIdentity(output string) (string, string, int, error) {
	fields := strings.Fields(output)
	if len(fields) < 4 || (fields[0] != "Clang" && fields[0] != "GCC") {
		return "", "", 0, fmt.Errorf("configured compiler returned unsupported predefined macros %q", strings.TrimSpace(output))
	}
	parts := [3]int{}
	for i := range parts {
		value, err := strconv.Atoi(fields[i+1])
		if err != nil || value < 0 || (i != 0 && value > 99) {
			return "", "", 0, fmt.Errorf("configured compiler returned invalid macro identity %q", strings.TrimSpace(output))
		}
		parts[i] = value
	}
	if parts[0] == 0 {
		return "", "", 0, fmt.Errorf("configured compiler returned invalid macro identity %q", strings.TrimSpace(output))
	}
	if fields[0] == "Clang" {
		return "clang", linuxProbeCCName, parts[0]*10000 + parts[1]*100 + parts[2], nil
	}
	return "gcc", "GCC", parts[0]*10000 + parts[1]*100 + parts[2], nil
}

func parseLinkerVersion(output string) (string, string, int, error) {
	line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	if strings.Contains(strings.ToLower(line), "lld") {
		text, code, err := parseLLVMVersion(line, "LLD")
		return linuxProbeLDName, text, code, err
	}
	if strings.Contains(line, "GNU ld") {
		code, err := parseLastSemanticVersion(line, "GNU ld")
		return "BFD", line, code, err
	}
	return "", "", 0, fmt.Errorf("probe linker returned an unsupported version line %q", line)
}

func parseGNUAssemblerVersion(output string) (string, int, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "GNU assembler") {
			continue
		}
		code, err := parseLastSemanticVersion(line, "GNU assembler")
		return "GNU", code, err
	}
	return "", 0, fmt.Errorf("probe assembler returned unsupported version output %q", strings.TrimSpace(output))
}

func parseLastSemanticVersion(line, tool string) (int, error) {
	matches := semanticVersionPattern.FindAllStringSubmatch(line, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("probe %s returned an unsupported version line %q", tool, line)
	}
	match := matches[len(matches)-1]
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch := 0
	if match[3] != "" {
		patch, _ = strconv.Atoi(match[3])
	}
	if minor > 99 || patch > 99 {
		return 0, fmt.Errorf("probe %s version cannot be represented by Linux: %q", tool, line)
	}
	return major*10000 + minor*100 + patch, nil
}

// SupportsOption performs one controlled cc-option/as-option/ld-option probe.
// A normal non-zero tool exit reports unsupported; malformed argv and tool
// execution failures are errors.
func (p *LinuxToolProbe) SupportsOption(ctx context.Context, kind string, candidate, probeContext []string) (bool, error) {
	if kind != "cc_option" && kind != "as_option" && kind != "ld_option" {
		return false, fmt.Errorf("unsupported Linux tool probe kind %q", kind)
	}
	if err := validateProbeCandidate(kind, candidate); err != nil {
		return false, fmt.Errorf("invalid %s candidate: %w", kind, err)
	}
	execContext, err := sanitizeProbeContext(kind, probeContext)
	if err != nil {
		return false, fmt.Errorf("invalid %s context: %w", kind, err)
	}
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, kind,
		strings.Join(probeContext, "\x00"), strings.Join(candidate, "\x00"),
	}, "\x01")
	p.mu.Lock()
	value, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
		return value, nil
	}

	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var supported bool
	if kind == "ld_option" {
		arguments := append([]string{"-v"}, execContext...)
		arguments = append(arguments, candidate...)
		args := p.linkerInvocation(arguments...)
		_, err = p.run(timedCtx, p.linkerPath, args, nil)
	} else {
		output, createErr := os.CreateTemp(p.tempDir, "linux-bzl-probe-*.o")
		if createErr != nil {
			return false, fmt.Errorf("create Linux probe output: %w", createErr)
		}
		outputPath := output.Name()
		if closeErr := output.Close(); closeErr != nil {
			os.Remove(outputPath)
			return false, fmt.Errorf("close Linux probe output: %w", closeErr)
		}
		defer os.Remove(outputPath)
		language := "c"
		if kind == "as_option" {
			language = "assembler-with-cpp"
		}
		arguments := append([]string{"-Werror"}, execContext...)
		arguments = append(arguments, candidate...)
		arguments = append(arguments, "-x", language, "-c", "-o", outputPath, "-")
		args := p.compilerInvocation(arguments...)
		_, err = p.run(timedCtx, p.compilerPath, args, []byte("\n"))
	}
	if err == nil {
		supported = true
	} else if _, ok := err.(*exec.ExitError); !ok {
		return false, err
	}
	p.mu.Lock()
	p.cache[key] = supported
	p.mu.Unlock()
	return supported, nil
}

// SupportsPreprocessorOption performs Linux's cc-option-bit check. Unlike a
// normal cc-option, this intentionally stops after preprocessing so assembler
// availability cannot change the selected -m32/-m64 driver flag.
func (p *LinuxToolProbe) SupportsPreprocessorOption(ctx context.Context, candidate []string) (bool, error) {
	if err := validateProbeCandidate("cc_option_bit", candidate); err != nil {
		return false, fmt.Errorf("invalid cc_option_bit candidate: %w", err)
	}
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "cc-option-bit",
		strings.Join(candidate, "\x00"),
	}, "\x01")
	if supported, ok := p.cachedCapability(key); ok {
		return supported, nil
	}
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	arguments := append([]string{"-Werror"}, candidate...)
	arguments = append(arguments, "-E", "-x", "c", os.DevNull, "-o", os.DevNull)
	args := p.compilerInvocation(arguments...)
	_, runErr := p.run(timedCtx, p.compilerPath, args, nil)
	supported, err := capabilityResult(runErr)
	if err != nil {
		return false, err
	}
	p.cacheCapability(key, supported)
	return supported, nil
}

// SupportsSource runs one allowlisted Kconfig feature-test source using its
// original compile/assembly mode and controlled input/output arguments.
func (p *LinuxToolProbe) SupportsSource(ctx context.Context, language, mode string, candidate []string, source string) (bool, error) {
	if language != "c" && language != "assembler-with-cpp" {
		return false, fmt.Errorf("unsupported Linux source probe language %q", language)
	}
	if mode != "-c" && mode != "-S" {
		return false, fmt.Errorf("unsupported Linux source probe mode %q", mode)
	}
	if len(candidate) != 0 {
		if err := validateProbeCandidate("source", candidate); err != nil {
			return false, fmt.Errorf("invalid Linux source probe candidate: %w", err)
		}
	}
	digest := sha256.Sum256([]byte(source))
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "source", language, mode,
		strings.Join(candidate, "\x00"), hex.EncodeToString(digest[:]),
	}, "\x01")
	p.mu.Lock()
	value, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
		return value, nil
	}
	output, err := os.CreateTemp(p.tempDir, "linux-bzl-source-probe-*.o")
	if err != nil {
		return false, fmt.Errorf("create Linux source probe output: %w", err)
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		os.Remove(outputPath)
		return false, fmt.Errorf("close Linux source probe output: %w", err)
	}
	defer os.Remove(outputPath)
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	arguments := append([]string(nil), candidate...)
	arguments = append(arguments, "-x", language, mode, "-o", outputPath, "-")
	args := p.compilerInvocation(arguments...)
	_, runErr := p.run(timedCtx, p.compilerPath, args, []byte(source))
	supported := runErr == nil
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			return false, runErr
		}
	}
	p.mu.Lock()
	p.cache[key] = supported
	p.mu.Unlock()
	return supported, nil
}

const linuxCCCanLinkSource = `#include <stdio.h>
int main(void)
{
	printf("\n");
	return 0;
}
`

// CanLink reproduces scripts/cc-can-link.sh with a fixed input and output.
// Script arguments have already been separated from the recognized command;
// only compiler options that cannot select an input, output, or plugin are
// accepted here.
func (p *LinuxToolProbe) CanLink(ctx context.Context, scriptArgs []string) (bool, error) {
	if err := validateLinkDriverProbeArgs(scriptArgs); err != nil {
		return false, fmt.Errorf("invalid cc-can-link arguments: %w", err)
	}
	if p.disableCCCanLink {
		return false, nil
	}
	digest := sha256.Sum256([]byte(linuxCCCanLinkSource))
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "cc-can-link",
		strings.Join(scriptArgs, "\x00"), hex.EncodeToString(digest[:]),
	}, "\x01")
	if supported, ok := p.cachedCapability(key); ok {
		return supported, nil
	}

	output, err := os.CreateTemp(p.tempDir, "linux-bzl-can-link-*")
	if err != nil {
		return false, fmt.Errorf("create cc-can-link output: %w", err)
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		os.Remove(outputPath)
		return false, fmt.Errorf("close cc-can-link output: %w", err)
	}
	defer os.Remove(outputPath)

	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	arguments := append([]string(nil), scriptArgs...)
	arguments = append(arguments, "-Werror", "-Wl,--fatal-warnings", "-x", "c", "-", "-o", outputPath)
	args := p.compilerInvocation(arguments...)
	_, runErr := p.run(timedCtx, p.compilerPath, args, []byte(linuxCCCanLinkSource))
	supported, err := capabilityResult(runErr)
	if err != nil {
		return false, err
	}
	if supported {
		if err := requireProbeRegularFile(outputPath, "cc-can-link executable"); err != nil {
			return false, err
		}
	}
	p.cacheCapability(key, supported)
	return supported, nil
}

const linuxStackProtectorProbeSource = "int foo(void) { char X[200]; return 3; }\n"

// SupportsX86StackProtector reproduces the legacy x86 stack-protector helper
// scripts. Those scripts check generated assembly for the %gs canary access,
// so merely checking whether the option parses would not be equivalent.
func (p *LinuxToolProbe) SupportsX86StackProtector(ctx context.Context, bits int, scriptArgs []string) (bool, error) {
	if p.profile.Name != "x86_64" {
		return false, fmt.Errorf("x86 stack-protector probe requires x86_64 profile, got %q", p.profile.Name)
	}
	if bits != 32 && bits != 64 {
		return false, fmt.Errorf("invalid x86 stack-protector width %d", bits)
	}
	wantArgs := []string{}
	if clangFlags := p.ClangFlags(); clangFlags != "" {
		wantArgs = []string{clangFlags}
	}
	if len(scriptArgs) != len(wantArgs) {
		return false, fmt.Errorf("invalid x86 stack-protector script arguments %q, want %q", scriptArgs, wantArgs)
	}
	for i, arg := range scriptArgs {
		if arg != wantArgs[i] {
			return false, fmt.Errorf("invalid x86 stack-protector script argument %q, want %q", arg, wantArgs[i])
		}
	}
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "x86-stack-protector",
		strconv.Itoa(bits), strings.Join(scriptArgs, "\x00"),
	}, "\x01")
	if supported, ok := p.cachedCapability(key); ok {
		return supported, nil
	}

	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	arguments := append([]string(nil), scriptArgs...)
	arguments = append(arguments, "-S", "-x", "c", "-c", fmt.Sprintf("-m%d", bits), "-O0")
	if bits == 64 {
		arguments = append(arguments, "-mcmodel=kernel", "-fno-PIE")
	}
	arguments = append(arguments, "-fstack-protector", "-", "-o", "-")
	args := p.compilerInvocation(arguments...)
	assembly, runErr := p.run(timedCtx, p.compilerPath, args, []byte(linuxStackProtectorProbeSource))
	compiled, err := capabilityResult(runErr)
	if err != nil {
		return false, err
	}
	supported := compiled && strings.Contains(assembly, "%gs")
	p.cacheCapability(key, supported)
	return supported, nil
}

// SupportsRELR reproduces scripts/tools-support-relr.sh using the selected
// compiler and linker plus the NM and OBJCOPY executables named by the
// recognized Kconfig command. Every input and output path is controlled here.
func (p *LinuxToolProbe) SupportsRELR(ctx context.Context) (bool, error) {
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "tools-support-relr",
	}, "\x01")
	if supported, ok := p.cachedCapability(key); ok {
		return supported, nil
	}

	tempDir, err := os.MkdirTemp(p.tempDir, "linux-bzl-relr-*")
	if err != nil {
		return false, fmt.Errorf("create RELR probe directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	objectPath := filepath.Join(tempDir, "probe.o")
	sharedPath := filepath.Join(tempDir, "probe.so")
	binaryPath := filepath.Join(tempDir, "probe.bin")
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	compileArgs := p.compilerInvocation("-c", "-x", "c", "-", "-o", objectPath)
	_, runErr := p.run(timedCtx, p.compilerPath, compileArgs, []byte("void *p = &p;\n"))
	if supported, resultErr := capabilityResult(runErr); resultErr != nil || !supported {
		return supported, resultErr
	}
	if err := requireProbeRegularFile(objectPath, "RELR object"); err != nil {
		return false, err
	}

	_, runErr = p.run(timedCtx, p.linkerPath, p.linkerInvocation(
		objectPath, "-shared", "-Bsymbolic", "--pack-dyn-relocs=relr", "-o", sharedPath,
	), nil)
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			return false, runErr
		}
		fallbackOutput, fallbackErr := p.run(timedCtx, p.linkerPath, p.linkerInvocation(
			objectPath, "-shared", "-Bsymbolic", "-z", "pack-relative-relocs", "-o", sharedPath,
		), nil)
		fallbackSupported, resultErr := capabilityResult(fallbackErr)
		if resultErr != nil {
			return false, resultErr
		}
		if !fallbackSupported || strings.Contains(fallbackOutput, "pack-relative-relocs") {
			p.cacheCapability(key, false)
			return false, nil
		}
	}
	if err := requireProbeRegularFile(sharedPath, "RELR shared object"); err != nil {
		return false, err
	}

	_, nmStderr, nmErr := p.runSeparate(timedCtx, p.nmPath, []string{sharedPath}, nil)
	nmSupported, resultErr := capabilityResult(nmErr)
	if resultErr != nil {
		return false, resultErr
	}
	if !nmSupported || strings.TrimSpace(nmStderr) != "" {
		p.cacheCapability(key, false)
		return false, nil
	}
	_, runErr = p.run(timedCtx, p.objcopyPath, []string{"-O", "binary", sharedPath, binaryPath}, nil)
	supported, err := capabilityResult(runErr)
	if err != nil {
		return false, err
	}
	if supported {
		if err := requireProbeRegularFile(binaryPath, "RELR binary"); err != nil {
			return false, err
		}
	}
	p.cacheCapability(key, supported)
	return supported, nil
}

func requireProbeRegularFile(path, description string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s %q: %w", description, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %q is not a regular file", description, path)
	}
	return nil
}

func (p *LinuxToolProbe) compilerPrintFileName(ctx context.Context, name string) (string, error) {
	if name != "plugin" {
		return "", fmt.Errorf("unsupported compiler print-file-name value %q", name)
	}
	if p.disableGCCPlugins {
		return "", nil
	}
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	args := p.compilerInvocation("-print-file-name=" + name)
	stdout, stderr, runErr := p.runSeparate(timedCtx, p.compilerPath, args, nil)
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); ok {
			return "", nil
		}
		return "", runErr
	}
	if strings.TrimSpace(stderr) != "" {
		return "", fmt.Errorf("compiler print-file-name wrote to stderr: %q", strings.TrimSpace(stderr))
	}
	value := strings.TrimSpace(stdout)
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("compiler print-file-name returned invalid path %q", value)
	}
	return value, nil
}

func (p *LinuxToolProbe) compilerPluginHeaderExists(ctx context.Context, path string) (bool, error) {
	if p.disableGCCPlugins {
		return false, nil
	}
	pluginDir, err := p.compilerPrintFileName(ctx, "plugin")
	if err != nil {
		return false, err
	}
	if pluginDir == "" {
		return false, nil
	}
	want := filepath.Clean(filepath.Join(pluginDir, "include", "plugin-version.h"))
	if filepath.Clean(path) != want {
		return false, fmt.Errorf("compiler plugin header probe path %q does not match %q", path, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat compiler plugin header %q: %w", want, err)
	}
	return !info.IsDir(), nil
}

func (p *LinuxToolProbe) auxiliaryToolFirstLineContains(ctx context.Context, tool, option, needle string) (bool, error) {
	var executable string
	switch tool {
	case "ar":
		executable = p.archiverPath
	case "nm":
		executable = p.nmPath
	case "objcopy":
		executable = p.objcopyPath
	default:
		return false, fmt.Errorf("unsupported auxiliary probe tool %q", tool)
	}
	if option != "--help" && option != "--version" {
		return false, fmt.Errorf("unsupported auxiliary probe option %q", option)
	}
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stdout, _, runErr := p.runSeparate(timedCtx, executable, []string{option}, nil)
	supported, err := capabilityResult(runErr)
	if err != nil || !supported {
		return supported, err
	}
	line := strings.SplitN(stdout, "\n", 2)[0]
	return strings.Contains(strings.ToLower(line), strings.ToLower(needle)), nil
}

func (p *LinuxToolProbe) cachedCapability(key string) (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value, ok := p.cache[key]
	return value, ok
}

func (p *LinuxToolProbe) cacheCapability(key string, value bool) {
	p.mu.Lock()
	p.cache[key] = value
	p.mu.Unlock()
}

func capabilityResult(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil
	}
	return false, err
}

func validateLinkDriverProbeArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	if err := validateProbeCandidate("cc_can_link", args); err != nil {
		return err
	}
	for _, arg := range args {
		lower := strings.ToLower(arg)
		for _, prefix := range []string{
			"--config", "--config-system-dir", "--gcc-toolchain", "--ld-path", "--resource-dir",
			"-fuse-ld", "-resource-dir", "-specs", "-wrapper",
		} {
			if lower == prefix || strings.HasPrefix(lower, prefix+"=") {
				return fmt.Errorf("tool-selection option is prohibited: %q", arg)
			}
		}
		if strings.HasPrefix(arg, "-B") {
			return fmt.Errorf("tool-selection option is prohibited: %q", arg)
		}
	}
	return nil
}

// SupportsKbuildSource compiles a bounded source fragment from a recognized
// Kbuild capability check with its concrete compiler context. Kbuild's
// as-instr helper feeds printf's %b output to the compiler, so escape decoding
// is reproduced without invoking a shell.
func (p *LinuxToolProbe) SupportsKbuildSource(
	ctx context.Context,
	language string,
	source string,
	probeContext []string,
) (bool, error) {
	if language != "assembler-with-cpp" {
		return false, fmt.Errorf("unsupported Kbuild source probe language %q", language)
	}
	decoded, err := decodeKbuildPrintfB(source)
	if err != nil {
		return false, fmt.Errorf("invalid Kbuild source probe: %w", err)
	}
	if err := validateKbuildAssemblerProbeSource(decoded); err != nil {
		return false, fmt.Errorf("invalid Kbuild source probe: %w", err)
	}
	execContext, err := sanitizeProbeContext("as_option", probeContext)
	if err != nil {
		return false, fmt.Errorf("invalid Kbuild source probe context: %w", err)
	}
	digest := sha256.Sum256([]byte(decoded))
	key := strings.Join([]string{
		p.identity, p.profile.Name, p.profile.TargetTriple, "kbuild-source", language,
		strings.Join(probeContext, "\x00"), hex.EncodeToString(digest[:]),
	}, "\x01")
	p.mu.Lock()
	value, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
		return value, nil
	}

	output, err := os.CreateTemp(p.tempDir, "linux-bzl-kbuild-source-probe-*.o")
	if err != nil {
		return false, fmt.Errorf("create Kbuild source probe output: %w", err)
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		os.Remove(outputPath)
		return false, fmt.Errorf("close Kbuild source probe output: %w", err)
	}
	defer os.Remove(outputPath)
	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	arguments := append([]string{"-Werror"}, execContext...)
	arguments = append(arguments, "-Wa,--fatal-warnings", "-x", language, "-c", "-o", outputPath, "-")
	args := p.compilerInvocation(arguments...)
	_, runErr := p.run(timedCtx, p.compilerPath, args, []byte(decoded))
	supported := runErr == nil
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			return false, runErr
		}
	}
	p.mu.Lock()
	p.cache[key] = supported
	p.mu.Unlock()
	return supported, nil
}

func decodeKbuildPrintfB(source string) (string, error) {
	if len(source) > 1024 {
		return "", fmt.Errorf("source exceeds 1024 bytes")
	}
	var out strings.Builder
	suppressNewline := false
	for i := 0; i < len(source); i++ {
		if source[i] != '\\' {
			out.WriteByte(source[i])
			continue
		}
		if i+1 == len(source) {
			out.WriteByte('\\')
			continue
		}
		i++
		switch source[i] {
		case 'a':
			out.WriteByte('\a')
		case 'b':
			out.WriteByte('\b')
		case 'c':
			suppressNewline = true
			i = len(source)
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'v':
			out.WriteByte('\v')
		case '\\':
			out.WriteByte('\\')
		default:
			// POSIX printf %b preserves unrecognized backslash escapes.
			out.WriteByte('\\')
			out.WriteByte(source[i])
		}
	}
	if !suppressNewline {
		out.WriteByte('\n')
	}
	return out.String(), nil
}

var kbuildAssemblerProbeLinePattern = regexp.MustCompile(`^[A-Za-z_.][A-Za-z0-9_.]*(?:[ \t]+[A-Za-z0-9_@%.,+()\-]+(?:[ \t]+[A-Za-z0-9_@%.,+()\-]+)*)?[ \t]*$`)

func validateKbuildAssemblerProbeSource(source string) error {
	if strings.ContainsAny(source, "\x00\r") {
		return fmt.Errorf("source contains a prohibited control character")
	}
	lines := strings.Split(source, "\n")
	if len(lines) > 16 {
		return fmt.Errorf("source exceeds 16 lines")
	}
	nonempty := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonempty++
		if !kbuildAssemblerProbeLinePattern.MatchString(line) {
			return fmt.Errorf("unsafe assembler line %q", line)
		}
		if strings.HasPrefix(line, ".") && !strings.HasPrefix(line, ".cfi_") {
			return fmt.Errorf("unsafe assembler directive %q", line)
		}
	}
	if nonempty == 0 {
		return fmt.Errorf("empty assembler source")
	}
	return nil
}

var powerPCPatchableFunctionPattern = regexp.MustCompile(`(?ms)^func:.*?^[ \t]*\.localentry[^\n]*\n.*?^[ \t]*nop(?:[ \t].*)?\n[ \t]*nop(?:[ \t].*)?$`)

// supportsPowerPCCompilerScript reproduces the two architecture script checks
// used by PowerPC Kconfig with fixed source and argv. The script path from
// Kconfig is recognized but never executed.
func (p *LinuxToolProbe) supportsPowerPCCompilerScript(ctx context.Context, script, endian string) (bool, error) {
	if p.profile.Name != "ppc64le" {
		return false, fmt.Errorf("PowerPC compiler script probe requires ppc64le, got %q", p.profile.Name)
	}
	if endian != "-mlittle-endian" && endian != "-mbig-endian" {
		return false, fmt.Errorf("unsupported PowerPC endian option %q", endian)
	}
	if script != "gcc-check-mprofile-kernel.sh" && script != "gcc-check-fpatchable-function-entry.sh" {
		return false, fmt.Errorf("unsupported PowerPC compiler script %q", script)
	}
	key := strings.Join([]string{p.identity, p.profile.Name, p.profile.TargetTriple, "powerpc-script", script, endian}, "\x01")
	p.mu.Lock()
	value, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
		return value, nil
	}

	timedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	compile := func(source string, featureFlags ...string) (string, bool, error) {
		arguments := []string{
			endian,
			"-m64",
			"-mabi=elfv2",
			"-S",
			"-x", "c",
			"-O2",
		}
		arguments = append(arguments, featureFlags...)
		arguments = append(arguments, "-", "-o", "-")
		args := p.compilerInvocation(arguments...)
		output, err := p.run(timedCtx, p.compilerPath, args, []byte(source))
		if err == nil {
			return output, true, nil
		}
		if _, ok := err.(*exec.ExitError); ok {
			return output, false, nil
		}
		return output, false, err
	}

	var supported bool
	switch script {
	case "gcc-check-mprofile-kernel.sh":
		profiled, compiled, err := compile("int func() { return 0; }\n", "-p", "-mprofile-kernel")
		if err != nil {
			return false, err
		}
		if compiled && strings.Contains(profiled, "_mcount") {
			notrace, notraceCompiled, err := compile("__attribute__((no_instrument_function)) int func() { return 0; }\n", "-p", "-mprofile-kernel")
			if err != nil {
				return false, err
			}
			supported = notraceCompiled && !strings.Contains(notrace, "_mcount")
		}
	case "gcc-check-fpatchable-function-entry.sh":
		section, compiled, err := compile("int func() { return 0; }\n", "-fpatchable-function-entry=2")
		if err != nil {
			return false, err
		}
		if compiled && strings.Contains(section, "__patchable_function_entries") {
			layout, layoutCompiled, err := compile("int x; int func() { return x; }\n", "-fpatchable-function-entry=2")
			if err != nil {
				return false, err
			}
			supported = layoutCompiled && powerPCPatchableFunctionPattern.MatchString(layout)
		}
	}
	p.mu.Lock()
	p.cache[key] = supported
	p.mu.Unlock()
	return supported, nil
}

func validateProbeCandidate(kind string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty candidate")
	}
	operand := false
	operandOption := ""
	for _, arg := range argv {
		if err := validateProbeToken(arg); err != nil {
			return err
		}
		if operand {
			operand = false
			if !safeProbeOperand(operandOption, arg) {
				return fmt.Errorf("unsafe operand %q for %s", arg, operandOption)
			}
			continue
		}
		if forbiddenProbeOption(arg) {
			return fmt.Errorf("file/plugin/output option is prohibited: %q", arg)
		}
		switch arg {
		case "-o", "--output", "-x", "-c", "-S", "-E":
			return fmt.Errorf("probe-controlled compiler mode argument is prohibited: %q", arg)
		case "--param", "-mllvm", "-m":
			operand = true
			operandOption = arg
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return fmt.Errorf("input path or positional argument is prohibited: %q", arg)
		}
	}
	if operand {
		return fmt.Errorf("missing operand for final option")
	}
	return nil
}

func validateProbeToken(arg string) error {
	if arg == "" || strings.ContainsAny(arg, "\x00\r\n") {
		return fmt.Errorf("empty or control-character argument %q", arg)
	}
	if strings.HasPrefix(arg, "@") {
		return fmt.Errorf("response files are prohibited: %q", arg)
	}
	return nil
}

func forbiddenProbeOption(arg string) bool {
	lower := strings.ToLower(arg)
	for _, prefix := range []string{
		"-fplugin", "-fpass-plugin", "-load", "--plugin", "-plugin",
		"-xclang", "-save-temps", "--save-temps", "-ftime-trace",
		"-xlinker", "-xassembler", "-wl,",
		"-serialize-diagnostics", "-mj", "-fprofile", "-fcoverage",
		"--script", "-t", "--version-script",
		"--dependency-file", "--sysroot", "-l", "--library-path",
		"-i", "-include", "-isystem", "-iquote", "-idirafter",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	if lower == "-map" || lower == "--map" || strings.HasPrefix(lower, "-map=") || strings.HasPrefix(lower, "--map=") {
		return true
	}
	if strings.HasPrefix(lower, "-wa,") {
		forwarded := strings.TrimPrefix(lower, "-wa,")
		return strings.Contains(forwarded, "-i") || strings.Contains(forwarded, "-a=") || strings.Contains(forwarded, "--listing")
	}
	return false
}

func safeProbeOperand(option, operand string) bool {
	if strings.ContainsAny(operand, `/\\`) || strings.HasPrefix(operand, "@") {
		return false
	}
	switch option {
	case "-m", "-z":
		return regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`).MatchString(operand)
	case "--param":
		return regexp.MustCompile(`^[A-Za-z0-9_-]+=[A-Za-z0-9_-]+$`).MatchString(operand)
	case "-mllvm":
		switch operand {
		case "-asan-kernel-mem-intrinsic-prefix=1", "-msan-disable-checks=1", "-tsan-compound-read-before-write=1", "-tsan-distinguish-volatile=1":
			return true
		}
	}
	return false
}

// KBUILD_{CPP,C,A,LD}FLAGS contain include/search paths that are irrelevant to
// option acceptance. They are deliberately omitted rather than handed to a
// controlled probe action. Output/plugin options remain hard errors.
func sanitizeProbeContext(kind string, argv []string) ([]string, error) {
	out := make([]string, 0, len(argv))
	skipOperand := false
	safeOperand := ""
	for _, arg := range argv {
		if err := validateProbeToken(arg); err != nil {
			return nil, err
		}
		if skipOperand {
			skipOperand = false
			continue
		}
		if safeOperand != "" {
			option := safeOperand
			safeOperand = ""
			if !safeProbeOperand(option, arg) {
				return nil, fmt.Errorf("unsafe operand %q for %s", arg, option)
			}
			out = append(out, arg)
			continue
		}
		lower := strings.ToLower(arg)
		if arg == "-I" || arg == "-isystem" || arg == "-include" || arg == "-iquote" || arg == "-idirafter" || arg == "-L" {
			skipOperand = true
			continue
		}
		if strings.HasPrefix(lower, "-i") || strings.HasPrefix(lower, "-l") || strings.HasPrefix(lower, "--sysroot=") {
			continue
		}
		if forbiddenProbeOption(arg) {
			return nil, fmt.Errorf("file/plugin/output option is prohibited: %q", arg)
		}
		if kind == "ld_option" && (arg == "-m" || arg == "-z") {
			out = append(out, arg)
			safeOperand = arg
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("positional context argument is prohibited: %q", arg)
		}
		out = append(out, arg)
	}
	if skipOperand {
		return nil, fmt.Errorf("missing path operand in probe context")
	}
	if safeOperand != "" {
		return nil, fmt.Errorf("missing operand for final option %s", safeOperand)
	}
	return out, nil
}

func (p *LinuxToolProbe) run(ctx context.Context, executable string, args []string, input []byte) (string, error) {
	stdout, stderr, err := p.runSeparate(ctx, executable, args, input)
	return stdout + stderr, err
}

func (p *LinuxToolProbe) runSeparate(ctx context.Context, executable string, args []string, input []byte) (string, string, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LANG=") || strings.HasPrefix(entry, "LC_ALL=") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "LANG=C", "LC_ALL=C")
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	stdout := &limitedProbeBuffer{remaining: p.outputLimit}
	stderr := &limitedProbeBuffer{remaining: p.outputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("Linux probe timed out or was cancelled: %w", ctx.Err())
	}
	if stdout.exceeded || stderr.exceeded {
		return stdout.String(), stderr.String(), fmt.Errorf("Linux probe output exceeded %d bytes per stream", p.outputLimit)
	}
	return stdout.String(), stderr.String(), err
}

type limitedProbeBuffer struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *limitedProbeBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > w.remaining {
		data = data[:w.remaining]
		w.exceeded = true
	}
	_, _ = w.buffer.Write(data)
	w.remaining -= len(data)
	return original, nil
}

func (w *limitedProbeBuffer) String() string { return w.buffer.String() }
