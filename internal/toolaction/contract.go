// Package toolaction defines the execution-time contract for invoking one
// configured Bazel tool action from another declared tool. The contract is
// role-based and deliberately independent of compiler family or executable
// filename.
package toolaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	KbuildArgumentsSentinel = "__LINUX_BZL_KBUILD_ARGS_V1__"
	ExecutionRootMarker     = "__LINUX_BZL_EXECROOT__"
	EnvironmentName         = "LINUX_BZL_TOOL_ACTION_CONTRACTS_V1"
	// RuntimeToolPathEnvironmentName is a runner-owned handoff to nested
	// source-script executors.  It names the private directory containing only
	// identity-bound tool-role aliases; source and configured action
	// environments may not set it.
	RuntimeToolPathEnvironmentName = "LINUX_BZL_RUNTIME_TOOL_PATH_V1"
)

const driverLinkContractSuffix = "-link"

const toolBindingScopeSeparator = "@"

type Contract struct {
	Arguments   []string          `json:"arguments"`
	Environment map[string]string `json:"environment"`
}

func Encode(contracts map[string]Contract) (string, error) {
	if err := Validate(contracts); err != nil {
		return "", err
	}
	data, err := json.Marshal(contracts)
	if err != nil {
		return "", fmt.Errorf("encode tool action contracts: %w", err)
	}
	return string(data), nil
}

func Decode(value string) (map[string]Contract, error) {
	if value == "" {
		return map[string]Contract{}, nil
	}
	contracts := map[string]Contract{}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contracts); err != nil {
		return nil, fmt.Errorf("decode tool action contracts: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return nil, fmt.Errorf("decode tool action contracts: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode tool action contracts: %w", err)
	}
	if err := Validate(contracts); err != nil {
		return nil, err
	}
	return contracts, nil
}

func Validate(contracts map[string]Contract) error {
	for role, contract := range contracts {
		if !ValidBinding(role) {
			return fmt.Errorf("invalid tool action role %q", role)
		}
		if contract.Arguments == nil || contract.Environment == nil {
			return fmt.Errorf("tool action role %q must declare arguments and environment", role)
		}
		sentinels := 0
		for _, argument := range contract.Arguments {
			if strings.ContainsRune(argument, 0) {
				return fmt.Errorf("tool action role %q has a NUL argument", role)
			}
			if argument == KbuildArgumentsSentinel {
				sentinels++
			}
		}
		if len(contract.Arguments) != 0 && sentinels != 1 {
			return fmt.Errorf("tool action role %q has %d Kbuild argument markers, want one", role, sentinels)
		}
		for name, value := range contract.Environment {
			if !validEnvironmentName(name) || strings.ContainsRune(value, 0) {
				return fmt.Errorf("tool action role %q has invalid environment entry %q", role, name)
			}
		}
	}
	return nil
}

// ExpandExecutionRootValue resolves the reserved action-contract marker
// against the original Bazel execution root. Configured tool actions can then
// retain absolute access to selected toolchain inputs after a runner changes
// its working directory.
func ExpandExecutionRootValue(value, executionRoot string) (string, error) {
	marker := ExecutionRootMarker
	prefix := marker + "/"
	if !strings.Contains(value, marker) {
		return value, nil
	}
	if !filepath.IsAbs(executionRoot) {
		return "", fmt.Errorf("execution root %q is not absolute", executionRoot)
	}
	root := filepath.ToSlash(filepath.Clean(executionRoot))
	if root != "/" {
		root += "/"
	}
	expanded := strings.ReplaceAll(value, prefix, root)
	if strings.Contains(expanded, marker) {
		return "", fmt.Errorf("configured toolchain action value contains malformed execution-root marker")
	}
	return expanded, nil
}

// LinkContractRole returns the semantic link-action contract paired with a C
// or C++ compiler-driver role.  The executable role does not change: cc-link
// and cxx-link are contracts for invoking the same source-selected cc/cxx
// executable in link mode.
func LinkContractRole(role string) (string, bool) {
	scope, base, scoped, valid := SplitBinding(role)
	if !valid || (base != "cc" && base != "cxx") {
		return "", false
	}
	base += driverLinkContractSuffix
	if scoped {
		return ScopedBinding(scope, base)
	}
	return base, true
}

// BaseContractRole returns the compiler-driver role paired with a semantic
// link contract.
func BaseContractRole(role string) (string, bool) {
	scope, base, scoped, valid := SplitBinding(role)
	base, ok := strings.CutSuffix(base, driverLinkContractSuffix)
	if !valid || !ok || (base != "cc" && base != "cxx") {
		return "", false
	}
	if scoped {
		return ScopedBinding(scope, base)
	}
	return base, true
}

// InvocationContractRole selects the configured action contract for one
// concrete argv.  It recognizes only the compiler driver's stable,
// family-independent mode interface.  A link invocation must have an explicit
// output and no compile/preprocess/dependency-only mode, which prevents
// queries such as `cc --version` from accidentally acquiring link flags.
type compilerInvocationProperties struct {
	hasOutput   bool
	compileOnly bool
	textOnly    bool
}

func inspectCompilerInvocation(arguments []string) compilerInvocationProperties {
	properties := compilerInvocationProperties{}
	expectOutput := false
	for _, argument := range arguments {
		if expectOutput {
			if argument != "" {
				properties.hasOutput = true
			}
			expectOutput = false
			continue
		}
		switch argument {
		case "-c":
			properties.compileOnly = true
		case "-S", "-E", "-M", "-MM", "-fsyntax-only":
			properties.textOnly = true
		case "-o":
			expectOutput = true
		default:
			if strings.HasPrefix(argument, "-o") && len(argument) > len("-o") {
				properties.hasOutput = true
			}
		}
	}
	return properties
}

func InvocationContractRole(role string, arguments []string) string {
	linkRole, compilerDriver := LinkContractRole(role)
	if !compilerDriver {
		return role
	}
	properties := inspectCompilerInvocation(arguments)
	if properties.compileOnly || properties.textOnly {
		return role
	}
	if properties.hasOutput {
		return linkRole
	}
	return role
}

// CompilerInvocationProducesBinaryOutput reports whether a configured C or
// C++ driver invocation's primary -o output is an object or linked image.
// Stable driver modes are interpreted independently of compiler family. Side
// dependency modes such as -MD/-MMD do not change a -c primary output, while
// preprocessing, dependency-only, assembly-text, and syntax-only modes are
// deliberately retained as potential generated source data.
func CompilerInvocationProducesBinaryOutput(role string, arguments []string) bool {
	_, base, _, valid := SplitBinding(role)
	if !valid || (base != "cc" && base != "cxx") {
		return false
	}
	properties := inspectCompilerInvocation(arguments)
	if properties.textOnly {
		return false
	}
	return properties.compileOnly || properties.hasOutput
}

// InstallToolActionProxy materializes one private executable which applies the
// configured action contract for role before invoking executable. C/C++ driver
// roles may supply their semantic link companion; the proxy selects it from
// the caller's stable driver-mode argv without identifying a compiler family.
// multicall must provide a POSIX sh applet and is written into the proxy's
// shebang so execution never searches an ambient shell.
func InstallToolActionProxy(
	directory, multicall, role, executable string,
	contract Contract,
	linkContract *Contract,
) (string, error) {
	if !ValidBinding(role) {
		return "", fmt.Errorf("invalid tool action proxy role %q", role)
	}
	if _, companion := BaseContractRole(role); companion {
		return "", fmt.Errorf("tool action proxy role %q is a semantic contract, not an executable role", role)
	}
	if directory == "" || multicall == "" || executable == "" {
		return "", fmt.Errorf("tool action proxy %q requires a directory, multicall runtime, and executable", role)
	}
	if !filepath.IsAbs(multicall) || strings.ContainsAny(multicall, "\x00\r\n\t ") || strings.ContainsRune(executable, 0) {
		return "", fmt.Errorf("tool action proxy %q has an invalid runtime or executable path", role)
	}
	contracts := map[string]Contract{role: contract}
	linkRole := ""
	if linkContract != nil {
		var ok bool
		linkRole, ok = LinkContractRole(role)
		if !ok {
			return "", fmt.Errorf("tool action proxy role %q cannot have a driver-link companion contract", role)
		}
		contracts[linkRole] = *linkContract
	}
	if err := Validate(contracts); err != nil {
		return "", err
	}

	destination := filepath.Join(directory, role)
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var script strings.Builder
	script.WriteString("#!")
	script.WriteString(multicall)
	script.WriteString(" sh\n")
	if linkContract != nil {
		script.WriteString("linux_bzl_link=\nlinux_bzl_expect_output=\n")
		script.WriteString("for linux_bzl_arg do\n")
		script.WriteString("  if [ -n \"$linux_bzl_expect_output\" ]; then\n")
		script.WriteString("    if [ -n \"$linux_bzl_arg\" ]; then linux_bzl_link=1; fi\n")
		script.WriteString("    linux_bzl_expect_output=\n    continue\n  fi\n")
		script.WriteString("  case \"$linux_bzl_arg\" in\n")
		script.WriteString("    -c|-S|-E|-M|-MM|-fsyntax-only) linux_bzl_link=; break ;;\n")
		script.WriteString("    -o) linux_bzl_expect_output=1 ;;\n")
		script.WriteString("    -o?*) linux_bzl_link=1 ;;\n")
		script.WriteString("  esac\ndone\n")
		script.WriteString("if [ \"$linux_bzl_link\" = 1 ]; then\n")
		writeToolActionContractInvocation(&script, executable, *linkContract, "  ")
		script.WriteString("fi\n")
	}
	writeToolActionContractInvocation(&script, executable, contract, "")
	if err := os.WriteFile(destination, []byte(script.String()), 0o700); err != nil {
		return "", err
	}
	return destination, nil
}

func writeToolActionContractInvocation(script *strings.Builder, executable string, contract Contract, indent string) {
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
			if argument == KbuildArgumentsSentinel {
				script.WriteString(" \"$@\"")
			} else {
				script.WriteByte(' ')
				script.WriteString(shellQuote(argument))
			}
		}
	}
	script.WriteByte('\n')
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

// PrepareRuntimeToolDirectory creates a private PATH component containing one
// symlink per configured tool role.  Callers own privateRoot and must invoke
// the returned cleanup function.  No ambient PATH entries are inspected.
func PrepareRuntimeToolDirectory(privateRoot string, tools map[string]string) (string, func(), error) {
	noop := func() {}
	if len(tools) == 0 {
		return "", noop, nil
	}
	if privateRoot == "" {
		return "", noop, fmt.Errorf("runtime tools require a private working-directory root")
	}
	privateRoot, err := filepath.Abs(privateRoot)
	if err != nil {
		return "", noop, fmt.Errorf("resolve private working-directory root: %w", err)
	}

	roles := make([]string, 0, len(tools))
	absoluteTools := make(map[string]string, len(tools))
	for role, tool := range tools {
		if !ValidBinding(role) {
			return "", noop, fmt.Errorf("invalid runtime tool role %q", role)
		}
		if tool == "" {
			return "", noop, fmt.Errorf("runtime tool %q is empty", role)
		}
		absolute, err := filepath.Abs(tool)
		if err != nil {
			return "", noop, fmt.Errorf("resolve runtime tool %q: %w", role, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return "", noop, fmt.Errorf("inspect runtime tool %q: %w", role, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", noop, fmt.Errorf("runtime tool %q is not an executable regular file", role)
		}
		roles = append(roles, role)
		absoluteTools[role] = absolute
	}
	sort.Strings(roles)

	runtimeRoot := filepath.Join(privateRoot, ".linux-bzl-tool-runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		return "", noop, fmt.Errorf("create private runtime-tool root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(runtimeRoot) }
	toolDirectory := filepath.Join(runtimeRoot, "bin")
	if err := os.Mkdir(toolDirectory, 0o700); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("create private runtime-tool bin: %w", err)
	}
	for _, role := range roles {
		if err := os.Symlink(absoluteTools[role], filepath.Join(toolDirectory, role)); err != nil {
			cleanup()
			return "", noop, fmt.Errorf("install runtime tool %q: %w", role, err)
		}
	}
	return toolDirectory, cleanup, nil
}

func Roles(contracts map[string]Contract) []string {
	roles := make([]string, 0, len(contracts))
	for role := range contracts {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// ValidRole reports whether value is a canonical action-role identifier shared
// by toolset manifests, source-time Make tokens, and execution contracts.
func ValidRole(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

// ScopedBinding returns the collision-free execution binding for role from an
// explicitly selected host or target toolset. '@' is deliberately excluded
// from ValidRole, so a scoped binding cannot alias any source-owned role.
func ScopedBinding(scope, role string) (string, bool) {
	if (scope != "host" && scope != "target") || !ValidRole(role) {
		return "", false
	}
	return scope + toolBindingScopeSeparator + role, true
}

// SplitBinding validates a configured tool binding and returns its optional
// scope plus underlying source-owned role.
func SplitBinding(value string) (scope, role string, scoped, valid bool) {
	if ValidRole(value) {
		return "", value, false, true
	}
	scope, role, found := strings.Cut(value, toolBindingScopeSeparator)
	if !found || strings.Contains(role, toolBindingScopeSeparator) ||
		(scope != "host" && scope != "target") || !ValidRole(role) {
		return "", "", false, false
	}
	return scope, role, true, true
}

func ValidBinding(value string) bool {
	_, _, _, valid := SplitBinding(value)
	return valid
}

func validEnvironmentName(value string) bool {
	if value == "" || !asciiLetterOrUnderscore(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !asciiLetterOrUnderscore(value[index]) && (value[index] < '0' || value[index] > '9') {
			return false
		}
	}
	return true
}

func asciiLetterOrUnderscore(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
