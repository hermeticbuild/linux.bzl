package kconfig

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	LinuxProbeDefaultRustcVersion     = 109700
	LinuxProbeDefaultRustcLLVMVersion = 220106

	linuxProbeCCName         = "Clang"
	linuxProbeCCVersion      = 220108
	linuxProbeCCVersionText  = "clang version 22.1.8None"
	linuxProbeASName         = "LLVM"
	linuxProbeASVersion      = 0
	linuxProbeLDName         = "LLD"
	linuxProbeLDVersion      = 220108
	linuxProbePaholeVersion  = 131
	linuxProbeBindgenVersion = "bindgen 0.72.1"
)

var ifSuccessPattern = regexp.MustCompile(`^\{\s*(.*);\s*\}\s*>/dev/null\s+2>&1\s+&&\s+echo\s+"(.*)"\s+\|\|\s+echo\s+"(.*)"$`)

// LinuxProbeShell models the one supported Linux compiler policy: Clang, its
// integrated assembler, and LLD at the LLVM 22.1.8 capability baseline.
// Architecture and the selected Rust compiler identity are the only inputs.
func LinuxProbeShell(
	architecture string,
	rustcVersion int,
	rustcLLVMVersion int,
) (func(context.Context, string) (string, error), error) {
	normalizedArchitecture, err := normalizeLinuxProbeArchitecture(architecture)
	if err != nil {
		return nil, err
	}
	if rustcVersion <= 0 {
		return nil, fmt.Errorf("invalid Linux Rust compiler version %d", rustcVersion)
	}
	if rustcLLVMVersion <= 0 {
		return nil, fmt.Errorf("invalid Linux Rust LLVM version %d", rustcLLVMVersion)
	}
	return (&linuxProbeShell{
		architecture:     normalizedArchitecture,
		rustcVersion:     rustcVersion,
		rustcLLVMVersion: rustcLLVMVersion,
	}).run, nil
}

// LinuxProbeShellWithTools uses actual integrity-pinned LLVM tools for
// capability and version probes while retaining the small set of pure shell
// expressions needed by Kconfig.include. Probe commands are parsed and mapped
// to direct argv execution; no command shell is used.
func LinuxProbeShellWithTools(
	probe *LinuxToolProbe,
	rustcVersion int,
	rustcLLVMVersion int,
) (func(context.Context, string) (string, error), error) {
	if probe == nil {
		return nil, fmt.Errorf("Linux tool probe is required")
	}
	if rustcVersion <= 0 || rustcLLVMVersion <= 0 {
		return nil, fmt.Errorf("invalid Linux Rust compiler identity")
	}
	return (&linuxProbeShell{
		architecture:     probe.profile.Name,
		rustcVersion:     rustcVersion,
		rustcLLVMVersion: rustcLLVMVersion,
		toolProbe:        probe,
	}).run, nil
}

type linuxProbeShell struct {
	architecture     string
	rustcVersion     int
	rustcLLVMVersion int
	toolProbe        *LinuxToolProbe
}

func (s *linuxProbeShell) run(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if match := ifSuccessPattern.FindStringSubmatch(command); match != nil {
		success, err := s.commandSucceeds(ctx, match[1])
		if err != nil {
			return "", err
		}
		if success {
			return match[2], nil
		}
		return match[3], nil
	}
	return s.output(command)
}

func (s *linuxProbeShell) output(command string) (string, error) {
	switch {
	case isKnownLinuxProbeScript(command, "cc-version.sh", "clang"):
		if s.toolProbe != nil {
			return fmt.Sprintf("Clang %d", s.toolProbe.clangCode), nil
		}
		return fmt.Sprintf("%s %d", linuxProbeCCName, linuxProbeCCVersion), nil
	case isLinuxProbeToolVersionCommand(command, "clang"):
		if s.toolProbe != nil {
			return s.toolProbe.clangVersion, nil
		}
		return linuxProbeCCVersionText, nil
	case isKnownLinuxProbeScript(command, "as-version.sh", "clang", "-fintegrated-as"):
		return fmt.Sprintf("%s %d", linuxProbeASName, linuxProbeASVersion), nil
	case isKnownLinuxProbeScript(command, "ld-version.sh", "ld.lld"):
		if s.toolProbe != nil {
			return fmt.Sprintf("LLD %d", s.toolProbe.lldCode), nil
		}
		return fmt.Sprintf("%s %d", linuxProbeLDName, linuxProbeLDVersion), nil
	case isKnownLinuxProbeScript(command, "pahole-version.sh", "pahole"):
		return strconv.Itoa(linuxProbePaholeVersion), nil
	case isKnownLinuxProbeScript(command, "rustc-version.sh", "rustc"):
		return strconv.Itoa(s.rustcVersion), nil
	case isKnownLinuxProbeScript(command, "rustc-llvm-version.sh", "rustc"):
		return strconv.Itoa(s.rustcLLVMVersion), nil
	case isKnownBindgenVersionCommand(command):
		return linuxProbeBindgenVersion, nil
	case isClangPrintPluginCommand(command):
		return "plugin", nil
	case strings.HasPrefix(command, "set -- "):
		return shellSetEcho(command)
	case strings.HasPrefix(command, "expr "):
		return shellExpr(command)
	default:
		return "", fmt.Errorf("unsupported Linux Kconfig probe command %q", command)
	}
}

func (s *linuxProbeShell) commandSucceeds(ctx context.Context, command string) (bool, error) {
	command = strings.TrimSpace(command)
	switch {
	case strings.HasPrefix(command, "command -v "):
		return s.commandExists(command)
	case strings.HasPrefix(command, "test "):
		return shellTest(strings.TrimSpace(strings.TrimPrefix(command, "test ")))
	case isKnownRustAvailableProbe(command):
		return true, nil
	case isKnownCCCanLinkProbe(command):
		return false, nil
	case isKnownStackProtectorProbe(command):
		return true, nil
	case isKnownRELRProbe(command):
		return true, nil
	case strings.Contains(command, " --help | head -n 1 | grep -qi llvm"):
		if strings.Contains(command, "llvm-nm") || strings.Contains(command, "llvm-ar") {
			return true, nil
		}
		return false, s.unsupportedCommand(command)
	case strings.Contains(command, "llvm-objcopy --version | head -n1 | grep -qv llvm"):
		return false, nil
	case strings.Contains(command, " --crate-type=rlib "):
		return true, nil
	case command == `python3 -c "import lxml"`:
		return false, nil
	}
	if supported, recognized, err := s.knownPowerPCCompilerScriptProbe(ctx, command); recognized || err != nil {
		return supported, err
	}
	if supported, recognized, err := s.knownClangSourceProbe(ctx, command); recognized || err != nil {
		return supported, err
	}
	if supported, recognized, err := s.knownClangAssemblerProbe(ctx, command); recognized || err != nil {
		return supported, err
	}
	if supported, recognized, err := s.knownLLDOptionProbe(ctx, command); recognized || err != nil {
		return supported, err
	}
	if supported, recognized, err := s.knownClangOptionProbe(ctx, command); recognized || err != nil {
		return supported, err
	}
	return false, s.unsupportedCommand(command)
}

func (s *linuxProbeShell) commandExists(command string) (bool, error) {
	fields := strings.Fields(command)
	if len(fields) != 3 || fields[0] != "command" || fields[1] != "-v" {
		return false, s.unsupportedCommand(command)
	}
	switch linuxProbeToolName(fields[2]) {
	case "clang", "ld.lld", "llvm-ar", "llvm-nm", "llvm-objcopy", "bindgen", "pahole":
		return true, nil
	case "rustc":
		return true, nil
	default:
		return false, s.unsupportedCommand(command)
	}
}

func linuxProbeToolName(value string) string {
	value = strings.Trim(value, `"'`)
	value = strings.TrimSuffix(value, ";")
	if index := strings.LastIndexAny(value, `/\`); index >= 0 {
		value = value[index+1:]
	}
	if len(value) > len(".exe") && strings.EqualFold(value[len(value)-len(".exe"):], ".exe") {
		value = value[:len(value)-len(".exe")]
	}
	return value
}

func linuxProbeScriptArgs(command, script string) ([]string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, false
	}
	path := strings.Trim(fields[0], `"'`)
	if !isLinuxProbeScriptPath(path, script) {
		return nil, false
	}
	return fields[1:], true
}

func isLinuxProbeScriptPath(path, script string) bool {
	path = filepath.ToSlash(path)
	return path == "scripts/"+script || strings.HasSuffix(path, "/scripts/"+script)
}

func linuxProbeArchitectureScriptArgs(command, architecture, script string) ([]string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, false
	}
	path := filepath.ToSlash(strings.Trim(fields[0], `"'`))
	want := "arch/" + architecture + "/tools/" + script
	if path != want && !strings.HasSuffix(path, "/"+want) {
		return nil, false
	}
	return fields[1:], true
}

func (s *linuxProbeShell) knownPowerPCCompilerScriptProbe(ctx context.Context, command string) (bool, bool, error) {
	for _, script := range []string{
		"gcc-check-mprofile-kernel.sh",
		"gcc-check-fpatchable-function-entry.sh",
	} {
		args, recognized := linuxProbeArchitectureScriptArgs(command, "powerpc", script)
		if !recognized {
			continue
		}
		if s.architecture != "ppc64le" || len(args) != 2 ||
			linuxProbeToolName(args[0]) != "clang" ||
			(args[1] != "-mlittle-endian" && args[1] != "-mbig-endian") {
			return false, true, s.unsupportedCommand(command)
		}
		if s.toolProbe != nil {
			supported, err := s.toolProbe.supportsPowerPCCompilerScript(ctx, script, args[1])
			return supported, true, err
		}
		// Pinned Clang 22 does not implement -mprofile-kernel, while its
		// ELFv2 patchable-function-entry layout has the two required nops.
		return script == "gcc-check-fpatchable-function-entry.sh", true, nil
	}
	return false, false, nil
}

func isKnownLinuxProbeScript(command, script string, expected ...string) bool {
	args, ok := linuxProbeScriptArgs(command, script)
	if !ok || len(args) != len(expected) {
		return false
	}
	for i, want := range expected {
		if strings.HasPrefix(want, "-") {
			if args[i] != want {
				return false
			}
			continue
		}
		if linuxProbeToolName(args[i]) != want {
			return false
		}
	}
	return true
}

func isLinuxProbeToolVersionCommand(command, tool string) bool {
	fields := strings.Fields(command)
	return len(fields) == 2 &&
		linuxProbeToolName(fields[0]) == tool &&
		fields[1] == "--version"
}

func isKnownBindgenVersionCommand(command string) bool {
	fields := strings.Fields(command)
	return len(fields) == 4 &&
		linuxProbeToolName(fields[0]) == "bindgen" &&
		fields[1] == "--version" &&
		fields[2] == "workaround-for-0.69.0" &&
		fields[3] == "2>/dev/null"
}

func isKnownRustAvailableProbe(command string) bool {
	args, ok := linuxProbeScriptArgs(command, "rust_is_available.sh")
	if !ok {
		return false
	}
	return len(args) == 0 || (len(args) == 1 && linuxProbeToolName(args[0]) == "rustc")
}

func isKnownCCCanLinkProbe(command string) bool {
	args, ok := linuxProbeScriptArgs(command, "cc-can-link.sh")
	if !ok || len(args) == 0 || linuxProbeToolName(args[0]) != "clang" {
		return false
	}
	for _, arg := range args[1:] {
		switch arg {
		case "-fintegrated-as", "-m32", "-m64", "-static":
		default:
			return false
		}
	}
	return true
}

func isKnownStackProtectorProbe(command string) bool {
	for _, script := range []string{
		"gcc-x86_32-has-stack-protector.sh",
		"gcc-x86_64-has-stack-protector.sh",
	} {
		if isKnownLinuxProbeScript(command, script, "clang", "-fintegrated-as") {
			return true
		}
	}
	return false
}

func isKnownRELRProbe(command string) bool {
	fields := strings.Fields(command)
	if len(fields) != 6 || fields[0] != "env" {
		return false
	}
	want := []struct {
		name string
		tool string
	}{
		{name: "CC", tool: "clang"},
		{name: "LD", tool: "ld.lld"},
		{name: "NM", tool: "llvm-nm"},
		{name: "OBJCOPY", tool: "llvm-objcopy"},
	}
	for i, expected := range want {
		assignment := strings.Trim(fields[i+1], `"'`)
		name, value, ok := strings.Cut(assignment, "=")
		if !ok || name != expected.name || linuxProbeToolName(value) != expected.tool {
			return false
		}
	}
	path := strings.Trim(fields[5], `"'`)
	return isLinuxProbeScriptPath(path, "tools-support-relr.sh")
}

func (s *linuxProbeShell) knownClangOptionProbe(ctx context.Context, command string) (bool, bool, error) {
	fields := strings.Fields(command)
	compiler := -1
	for i, field := range fields {
		if isLinuxProbeCompilerToken(field) {
			compiler = i
			break
		}
	}
	if compiler < 0 {
		return false, false, nil
	}
	hasNullInput := false
	hasCompileMode := false
	var candidate []string
	for i := compiler + 1; i < len(fields); i++ {
		field := strings.TrimSuffix(fields[i], ";")
		switch field {
		case "-c", "-E":
			hasCompileMode = true
		case "-Werror", "-fintegrated-as":
		case "$CLANG_FLAGS", "$(CLANG_FLAGS)":
			candidate = append(candidate, "-fintegrated-as")
		case "-x", "-o":
			i++
		case "/dev/null", "-":
			hasNullInput = true
		case "{", "}":
		default:
			if strings.HasPrefix(field, ".tmp_") || field == "/dev/null" {
				continue
			}
			candidate = append(candidate, field)
		}
	}
	if !hasCompileMode || !hasNullInput {
		return false, false, nil
	}
	key := normalizeLinuxProbeCandidate(candidate)
	if s.toolProbe != nil {
		supported, err := s.toolProbe.SupportsOption(ctx, "cc_option", candidate, nil)
		return supported, true, err
	}
	var supported, known bool
	if key == normalizeLinuxProbeCandidate([]string{"-m32"}) {
		// Kconfig.include uses this canonical preprocessing probe on every
		// architecture. Pinned Clang accepts the compatibility switch for all
		// supported profiles except AArch64, where it is an unknown option.
		supported = s.architecture != "aarch64"
		known = true
	}
	if key == normalizeLinuxProbeCandidate([]string{"-m64"}) {
		// Kconfig.include also probes the 64-bit compatibility switch on every
		// architecture. Pinned Clang accepts it for each supported 64-bit
		// profile, but rejects it for the 32-bit ARM profile.
		supported = s.architecture != "armv7"
		known = true
	}
	if key == normalizeLinuxProbeCandidate([]string{"-fpatchable-function-entry=8"}) {
		// RISC-V Kconfig probes this exact entry padding. Pinned Clang's
		// driver supports patchable entries for every supported profile here
		// except 32-bit ARM.
		supported = s.architecture != "armv7"
		known = true
	}
	if key == normalizeLinuxProbeCandidate([]string{
		"-mtp=cp15",
		"-mstack-protector-guard=tls",
		"-mstack-protector-guard-offset=0",
	}) {
		// ARM Kconfig probes these as one inseparable capability: Clang
		// requires both the TLS guard offset and the CP15 thread-pointer mode.
		supported = s.architecture == "armv7"
		known = true
	}
	switch s.architecture {
	case "x86_64":
		if !known {
			supported, known = linuxLLVMKconfigCCOptionsX86[key]
		}
	case "aarch64":
		if !known {
			supported, known = linuxLLVMKconfigCCOptionsARM64[key]
		}
	case "armv7":
		if !known {
			supported, known = linuxLLVMKconfigCCOptionsARMV7[key]
		}
	case "riscv64":
		if !known {
			supported, known = linuxLLVMKconfigCCOptionsRISCV64[key]
		}
	case "ppc64le":
		if !known {
			supported, known = linuxLLVMKconfigCCOptionsPPC64LE[key]
		}
	}
	if !known {
		supported, known = linuxLLVMKconfigCCOptionsCommon[key]
	}
	if !known {
		return false, true, fmt.Errorf(
			"unsupported Clang 22.1.8 Kconfig compiler candidate %q for architecture %q in command %q",
			strings.Join(candidate, " "),
			s.architecture,
			command,
		)
	}
	return supported, true, nil
}

func normalizeLinuxProbeCandidate(argv []string) string {
	return strings.Join(argv, "\x00")
}

func (s *linuxProbeShell) knownClangSourceProbe(ctx context.Context, command string) (bool, bool, error) {
	if !strings.Contains(command, "|") ||
		!strings.Contains(command, " -x c - ") ||
		(!strings.Contains(command, " -c ") && !strings.Contains(command, " -S ")) {
		return false, false, nil
	}
	for _, fragment := range linuxLLVMKnownCSourceFragments {
		if strings.Contains(command, fragment) {
			if s.toolProbe == nil {
				return true, true, nil
			}
			source, candidate, err := parseLinuxSourceProbe(command)
			if err != nil {
				return false, true, err
			}
			supported, err := s.toolProbe.SupportsSource(ctx, "c", candidate, source)
			return supported, true, err
		}
	}
	return false, false, nil
}

func (s *linuxProbeShell) knownClangAssemblerProbe(ctx context.Context, command string) (bool, bool, error) {
	if !strings.HasPrefix(command, `printf "%b\n" `) ||
		!strings.Contains(command, " -x assembler-with-cpp ") {
		return false, false, nil
	}
	for _, fragment := range linuxLLVMKnownAssemblerFragments {
		if strings.Contains(command, fragment) {
			if s.toolProbe == nil {
				return true, true, nil
			}
			source, candidate, err := parseLinuxSourceProbe(command)
			if err != nil {
				return false, true, err
			}
			source, err = decodeKbuildPrintfB(source)
			if err != nil {
				return false, true, fmt.Errorf("invalid Linux assembler source probe: %w", err)
			}
			supported, err := s.toolProbe.SupportsSource(ctx, "assembler-with-cpp", candidate, source)
			return supported, true, err
		}
	}
	return false, false, nil
}

func parseLinuxSourceProbe(command string) (string, []string, error) {
	left, right, ok := strings.Cut(command, "|")
	if !ok {
		return "", nil, fmt.Errorf("unsupported Linux source probe %q", command)
	}
	left = strings.TrimSpace(left)
	var quoted string
	if strings.HasPrefix(left, "echo ") {
		quoted = strings.TrimSpace(strings.TrimPrefix(left, "echo "))
	} else if strings.HasPrefix(left, `printf "%b\n" `) {
		quoted = strings.TrimSpace(strings.TrimPrefix(left, `printf "%b\n" `))
	} else {
		return "", nil, fmt.Errorf("unsupported Linux source producer %q", left)
	}
	source, err := unquoteLinuxProbeSource(quoted)
	if err != nil {
		return "", nil, err
	}
	fields := strings.Fields(strings.TrimSpace(right))
	compiler := -1
	for i, field := range fields {
		if isLinuxProbeCompilerToken(field) {
			compiler = i
			break
		}
	}
	if compiler < 0 {
		return "", nil, fmt.Errorf("Linux source probe has no clang invocation")
	}
	var candidate []string
	for i := compiler + 1; i < len(fields); i++ {
		field := strings.TrimSuffix(fields[i], ";")
		switch field {
		case "-c", "-S", "-", "/dev/null":
			continue
		case "$CLANG_FLAGS", "$(CLANG_FLAGS)":
			candidate = append(candidate, "-fintegrated-as")
			continue
		case "-x", "-o":
			i++
			continue
		}
		candidate = append(candidate, field)
	}
	return source, candidate, nil
}

func unquoteLinuxProbeSource(quoted string) (string, error) {
	if len(quoted) < 2 || (quoted[0] != '\'' && quoted[0] != '"') || quoted[len(quoted)-1] != quoted[0] {
		return "", fmt.Errorf("unsupported Linux source quoting %q", quoted)
	}
	body := quoted[1 : len(quoted)-1]
	if quoted[0] == '\'' {
		if strings.ContainsRune(body, '\'') {
			return "", fmt.Errorf("unsupported Linux source quoting %q", quoted)
		}
		return body, nil
	}

	var out strings.Builder
	out.Grow(len(body))
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			return "", fmt.Errorf("unsupported Linux source quoting %q", quoted)
		case '\\':
			if i+1 == len(body) {
				return "", fmt.Errorf("unsupported Linux source quoting %q", quoted)
			}
			next := body[i+1]
			switch next {
			case '$', '`', '"', '\\':
				out.WriteByte(next)
				i++
			case '\n':
				i++
			default:
				// Within double quotes, the shell preserves backslashes before
				// characters other than $, `, ", \\, and a newline. printf %b
				// interprets those remaining escapes in the following step.
				out.WriteByte('\\')
			}
		default:
			out.WriteByte(body[i])
		}
	}
	return out.String(), nil
}

func isLinuxProbeCompilerToken(field string) bool {
	field = strings.Trim(field, `"'`)
	return linuxProbeToolName(field) == "clang" || field == "$CC" || field == "$(CC)"
}

func (s *linuxProbeShell) knownLLDOptionProbe(ctx context.Context, command string) (bool, bool, error) {
	fields := strings.Fields(command)
	if len(fields) < 3 || linuxProbeToolName(fields[0]) != "ld.lld" || fields[1] != "-v" {
		return false, false, nil
	}
	candidate := strings.Join(fields[2:], " ")
	if s.toolProbe != nil {
		supported, err := s.toolProbe.SupportsOption(ctx, "ld_option", fields[2:], nil)
		return supported, true, err
	}
	supported, ok := linuxLLVMKconfigLDOptions[candidate]
	if !ok && s.architecture == "riscv64" {
		supported, ok = linuxLLVMKconfigLDOptionsRISCV64[candidate]
	}
	return supported, ok, nil
}

func (s *linuxProbeShell) unsupportedCommand(command string) error {
	return fmt.Errorf(
		"unsupported Clang 22.1.8 Linux Kconfig probe command for architecture %q: %q",
		s.architecture,
		command,
	)
}

func shellSetEcho(command string) (string, error) {
	before, after, ok := strings.Cut(command, "&&")
	if !ok {
		return "", fmt.Errorf("unsupported set command %q", command)
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(before, "set -- ")))
	echo := strings.TrimSpace(after)
	if !strings.HasPrefix(echo, "echo $") {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	index, err := strconv.Atoi(strings.TrimPrefix(echo, "echo $"))
	if err != nil {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	if index < 1 || index > len(fields) {
		return "", nil
	}
	return fields[index-1], nil
}

func shellExpr(command string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) == 4 && fields[2] == "/" {
		left, leftErr := strconv.Atoi(fields[1])
		right, rightErr := strconv.Atoi(fields[3])
		if leftErr != nil || rightErr != nil || right == 0 {
			return "", fmt.Errorf("unsupported expr command %q", command)
		}
		return strconv.Itoa(left / right), nil
	}
	return "", fmt.Errorf("unsupported expr command %q", command)
}

func shellTest(expr string) (bool, error) {
	if value, ok := strings.CutPrefix(expr, "-z "); ok {
		return unquoteShell(value) == "", nil
	}
	if value, ok := strings.CutPrefix(expr, "-e "); ok {
		path := unquoteShell(value)
		if strings.HasSuffix(path, "include/plugin-version.h") {
			return false, nil
		}
		return false, fmt.Errorf("unsupported Linux Kconfig test path %q", path)
	}
	fields := strings.Fields(expr)
	if len(fields) != 3 {
		return false, fmt.Errorf("unsupported Linux Kconfig test expression %q", expr)
	}
	left := unquoteShell(fields[0])
	right := unquoteShell(fields[2])
	switch fields[1] {
	case "=":
		return left == right, nil
	case "!=":
		return left != right, nil
	default:
		return false, fmt.Errorf("unsupported Linux Kconfig test expression %q", expr)
	}
}

func unquoteShell(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func normalizeLinuxProbeArchitecture(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "x86", "x86_64":
		return "x86_64", nil
	case "arm64", "aarch64":
		return "aarch64", nil
	case "arm", "armv7", "armv7l":
		return "armv7", nil
	case "riscv", "riscv64":
		return "riscv64", nil
	case "powerpc", "ppc64le":
		return "ppc64le", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q; expected x86_64, aarch64, armv7, riscv64, or ppc64le", value)
	}
}

func isClangPrintPluginCommand(command string) bool {
	fields := strings.Fields(command)
	return len(fields) == 2 &&
		linuxProbeToolName(fields[0]) == "clang" &&
		fields[1] == "-print-file-name=plugin"
}

var linuxLLVMKconfigCCOptionsCommon = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-Wimplicit-fallthrough=5"}):                                                                                 false,
	normalizeLinuxProbeCandidate([]string{"-Wunreachable-code-fallthrough"}):                                                                           true,
	normalizeLinuxProbeCandidate([]string{"-ffunction-sections", "-fdata-sections"}):                                                                   true,
	normalizeLinuxProbeCandidate([]string{"-fmin-function-alignment=8"}):                                                                               false,
	normalizeLinuxProbeCandidate([]string{"-frandomize-layout-seed-file=/dev/null"}):                                                                   true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize-coverage-stack-depth-callback-min=1"}):                                                           true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize-coverage=trace-cmp"}):                                                                            true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize-coverage=trace-pc"}):                                                                             true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize-ignorelist=/dev/null"}):                                                                          true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize-undefined-ignore-overflow-pattern=all"}):                                                         true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=alignment"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=array-bounds"}):                                                                                  true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=bool"}):                                                                                          true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=bounds-strict"}):                                                                                 false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=enum"}):                                                                                          true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=implicit-signed-integer-truncation"}):                                                            true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=implicit-unsigned-integer-truncation"}):                                                          true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=integer-divide-by-zero"}):                                                                        true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kcfi"}):                                                                                          true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kcfi", "-fsanitize-cfi-icall-experimental-normalize-integers"}):                                  true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-address"}):                                                                                true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-address", "--param", "asan-kernel-mem-intrinsic-prefix=1"}):                               false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-address", "-mllvm", "-asan-kernel-mem-intrinsic-prefix=1"}):                               true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-hwaddress"}):                                                                              true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=shift"}):                                                                                         true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=signed-integer-overflow"}):                                                                       true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=thread", "--param", "tsan-compound-read-before-write=1"}):                                        false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=thread", "--param", "tsan-distinguish-volatile=1"}):                                              false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=thread", "-mllvm", "-tsan-compound-read-before-write=1"}):                                        true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=thread", "-mllvm", "-tsan-distinguish-volatile=1"}):                                              true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=unreachable"}):                                                                                   true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=unsigned-integer-overflow"}):                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-fstack-protector"}):                                                                                        true,
	normalizeLinuxProbeCandidate([]string{"-fstack-protector-strong"}):                                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-ftrivial-auto-var-init=pattern"}):                                                                          true,
	normalizeLinuxProbeCandidate([]string{"-ftrivial-auto-var-init=zero"}):                                                                             true,
	normalizeLinuxProbeCandidate([]string{"-ftrivial-auto-var-init=zero", "-enable-trivial-auto-var-init-zero-knowing-it-will-be-removed-from-clang"}): false,
	normalizeLinuxProbeCandidate([]string{"-fzero-call-used-regs=used-gpr"}):                                                                           true,
	normalizeLinuxProbeCandidate([]string{"-gsplit-dwarf"}):                                                                                            true,
	normalizeLinuxProbeCandidate([]string{"-gz=zlib"}):                                                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-gz=zstd"}):                                                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-m64", "-D__SIZEOF_INT128__=0"}):                                                                            false,
	normalizeLinuxProbeCandidate([]string{"-mrecord-mcount"}):                                                                                          false,
	normalizeLinuxProbeCandidate([]string{"-fno-stack-protector"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-D__SIZEOF_INT128__=0"}):                                                                                    false,
	normalizeLinuxProbeCandidate([]string{"-D__SIZEOF_INT128__=16"}):                                                                                   true,
}

var linuxLLVMKconfigCCOptionsX86 = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-fcf-protection=branch", "-mindirect-branch-register"}):         false,
	normalizeLinuxProbeCandidate([]string{"-fpatchable-function-entry=16"}):                                true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kcfi", "-fsanitize-kcfi-arity"}):                     true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory"}):                                     true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-fsanitize-memory-param-retval"}):   true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-mllvm", "-msan-disable-checks=1"}): true,
	normalizeLinuxProbeCandidate([]string{"-m32"}):                                                         true,
	normalizeLinuxProbeCandidate([]string{"-m64"}):                                                         true,
	normalizeLinuxProbeCandidate([]string{"-march=native"}):                                                false,
	normalizeLinuxProbeCandidate([]string{"-mfunction-return=thunk-extern"}):                               true,
	normalizeLinuxProbeCandidate([]string{"-mharden-sls=all"}):                                             true,
}

var linuxLLVMKconfigCCOptionsARM64 = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-Wa,-march=armv8.2-a"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-Wa,-march=armv8.3-a"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-Wa,-march=armv8.4-a"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-Wa,-march=armv8.5-a"}):                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-fpatchable-function-entry=2"}):                                                                             true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory"}):                                                                                 false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-fsanitize-memory-param-retval"}):                                               false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-mllvm", "-msan-disable-checks=1"}):                                             false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=shadow-call-stack", "-ffixed-x18"}):                                                              true,
	normalizeLinuxProbeCandidate([]string{"-m32"}):                                                                                                     false,
	normalizeLinuxProbeCandidate([]string{"-m64"}):                                                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-mbranch-protection=pac-ret+leaf"}):                                                                         true,
	normalizeLinuxProbeCandidate([]string{"-mbranch-protection=pac-ret+leaf+bti"}):                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-msign-return-address=all"}):                                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mstack-protector-guard=sysreg", "-mstack-protector-guard-reg=sp_el0", "-mstack-protector-guard-offset=0"}): true,
}

var linuxLLVMKconfigCCOptionsARMV7 = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory"}):                                     false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-fsanitize-memory-param-retval"}):   false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-mllvm", "-msan-disable-checks=1"}): false,
}

var linuxLLVMKconfigCCOptionsRISCV64 = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory"}):                                                                          false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-fsanitize-memory-param-retval"}):                                        false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-mllvm", "-msan-disable-checks=1"}):                                      false,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=shadow-call-stack"}):                                                                      true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64imv"}):                                                                      true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32imv"}):                                                                     true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zabha"}):                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zabha"}):                                                               true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zacas"}):                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zacas"}):                                                               true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zbb"}):                                                                  true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zbb"}):                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zba"}):                                                                  true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zba"}):                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zbc"}):                                                                  true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zbc"}):                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-mabi=lp64", "-march=rv64ima_zbkb"}):                                                                 true,
	normalizeLinuxProbeCandidate([]string{"-mabi=ilp32", "-march=rv32ima_zbkb"}):                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mstack-protector-guard=tls", "-mstack-protector-guard-reg=tp", "-mstack-protector-guard-offset=0"}): true,
}

var linuxLLVMKconfigCCOptionsPPC64LE = map[string]bool{
	normalizeLinuxProbeCandidate([]string{"-fpatchable-function-entry=2"}):                                                                               true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory"}):                                                                                   true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-fsanitize-memory-param-retval"}):                                                 true,
	normalizeLinuxProbeCandidate([]string{"-fsanitize=kernel-memory", "-mllvm", "-msan-disable-checks=1"}):                                               true,
	normalizeLinuxProbeCandidate([]string{"-m32", "-mstack-protector-guard=tls", "-mstack-protector-guard-reg=r2", "-mstack-protector-guard-offset=0"}):  false,
	normalizeLinuxProbeCandidate([]string{"-m64", "-mstack-protector-guard=tls", "-mstack-protector-guard-reg=r13", "-mstack-protector-guard-offset=0"}): true,
	normalizeLinuxProbeCandidate([]string{"-mabi=elfv2"}):                                                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mcpu=power10", "-mpcrel"}):                                                                                   true,
	normalizeLinuxProbeCandidate([]string{"-mcpu=power10", "-mprefixed"}):                                                                                true,
	normalizeLinuxProbeCandidate([]string{"-mtune=power10"}):                                                                                             true,
	normalizeLinuxProbeCandidate([]string{"-mtune=power8"}):                                                                                              true,
	normalizeLinuxProbeCandidate([]string{"-mtune=power9"}):                                                                                              true,
}

var linuxLLVMKconfigLDOptions = map[string]bool{
	"--compress-debug-sections=zlib": true,
	"--compress-debug-sections=zstd": true,
	"--fix-cortex-a53-843419":        true,
	"--gc-sections":                  true,
	"--orphan-handling=error":        true,
	"--orphan-handling=warn":         true,
}

var linuxLLVMKconfigLDOptionsRISCV64 = map[string]bool{
	"--no-relax-gp": true,
}

var linuxLLVMKnownCSourceFragments = []string{
	`__attribute__((__counted_by__(count)))`,
	`__attribute__((__nonstring__))`,
	`__attribute__((no_profile_instrument_function))`,
	`asm goto (".long (%l[bar]) - ."`,
	`asm goto ("": "=r"(x)`,
	`asm inline ("")`,
	`cleanup(b)`,
	`int __seg_fs fs; int __seg_gs gs;`,
}

var linuxLLVMKnownAssemblerFragments = []string{
	`.insn 0x100000f`,
	`.option arch, +m`,
	`.option arch, +v, +zvkb`,
	`R_RISCV_SET_ULEB128`,
	`.arch armv8.2-a+sha3`,
	`.arch armv8.5-a+memtag`,
	`.arch_extension lse`,
	`.arch_extension mops`,
	`.arch_extension rcpc`,
	`.cfi_negate_ra_state`,
	`.uleb128 .Lexpr_end4 - .Lexpr_start3`,
	`.inst 0`,
	`endbr64`,
	`sha1msg1`,
	`sha256msg1`,
	`stgm xzr`,
	`tpause`,
	`vaesenc`,
	`vgf2p8mulb`,
	`vpclmulqdq`,
	`vpmovm2b`,
	`wrussq`,
}
