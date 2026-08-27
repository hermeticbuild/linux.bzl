package kconfig

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var ifSuccessPattern = regexp.MustCompile(`^\{\s*(.*);\s*\}\s*>/dev/null\s+2>&1\s+&&\s+echo\s+"(.*)"\s+\|\|\s+echo\s+"(.*)"$`)

var (
	kbuildTryRunPattern = regexp.MustCompile(`(?s)^set -e;\s*TMP=([^;[:space:]]+)/tmp;\s*trap "rm -rf ([^"]+)" EXIT;\s*mkdir -p ([^;[:space:]]+);\s*if \((.*)\) >/dev/null 2>&1;\s*then echo "([^"]*)";\s*else echo "([^"]*)";\s*fi$`)
	kbuildTryRunTemp    = regexp.MustCompile(`^\.tmp_[0-9]*$`)
)

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

var (
	kbuildAssemblerProbeLinePattern  = regexp.MustCompile(`^(?:[A-Za-z_.][A-Za-z0-9_.]*|[0-9]+:)(?:[ \t]+[A-Za-z0-9_$@%.,+()\[\]\-]+(?:[ \t]+[A-Za-z0-9_$@%.,+()\[\]\-]+)*)?[ \t]*$`)
	kbuildAssemblerLocalLabelPattern = regexp.MustCompile(`^\.L[A-Za-z0-9_.$]*:$`)
)

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
		if kbuildAssemblerLocalLabelPattern.MatchString(line) {
			continue
		}
		if !kbuildAssemblerProbeLinePattern.MatchString(line) {
			return fmt.Errorf("unsafe assembler line %q", line)
		}
		if strings.HasPrefix(line, ".") {
			directive := strings.Fields(line)[0]
			switch directive {
			case ".arch", ".arch_extension", ".cfi_endproc", ".cfi_negate_ra_state", ".cfi_startproc",
				".endr", ".insn", ".inst", ".option", ".reloc", ".rept", ".uleb128", ".word":
			default:
				return fmt.Errorf("unsafe assembler directive %q", directive)
			}
		}
	}
	if nonempty == 0 {
		return fmt.Errorf("empty assembler source")
	}
	return nil
}

// ProbeCandidatePathKind identifies the read-only path authorities which
// a compiler candidate may request. The grammar only locates these operands;
// proberun must separately prove that each concrete path belongs to a declared
// source input or to the identity-bound toolset closure before execution.
type ProbeCandidatePathKind string

const (
	ProbeCandidatePathInclude       ProbeCandidatePathKind = "include"
	ProbeCandidatePathForcedInclude ProbeCandidatePathKind = "forced-include"
	ProbeCandidatePathRegularFile   ProbeCandidatePathKind = "regular-file"
)

// ProbeCandidatePathOperand locates one path within the original candidate
// argv. Start and End are byte offsets in argv[Argument]. They deliberately
// also support paths embedded in a forwarding word such as -Wa,-I,DIR.
// Consumers which rewrite more than one operand in one argument must process
// the returned spans from last to first.
type ProbeCandidatePathOperand struct {
	Kind     ProbeCandidatePathKind
	Argument int
	Start    int
	End      int
}

type probeCandidateToken struct {
	value           string
	argument, start int
	end             int
}

type probeCandidatePathOption struct {
	name       string
	kind       ProbeCandidatePathKind
	joined     bool
	equalsOnly bool
}

var probeCandidatePathOptions = []probeCandidatePathOption{
	{name: "-frandomize-layout-seed-file", kind: ProbeCandidatePathRegularFile, equalsOnly: true},
	{name: "-fsanitize-ignorelist", kind: ProbeCandidatePathRegularFile, equalsOnly: true},
	{name: "-fsanitize-blacklist", kind: ProbeCandidatePathRegularFile, equalsOnly: true},
	{name: "-iwithprefixbefore", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-iframeworkwithsysroot", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-include-pch", kind: ProbeCandidatePathForcedInclude, joined: true},
	{name: "-include-pth", kind: ProbeCandidatePathForcedInclude, joined: true},
	{name: "-iwithprefix", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-idirafter", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-iframework", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-isystem", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-include", kind: ProbeCandidatePathForcedInclude, joined: true},
	{name: "-imacros", kind: ProbeCandidatePathForcedInclude, joined: true},
	{name: "-iprefix", kind: ProbeCandidatePathInclude, joined: true},
	{name: "--include-directory", kind: ProbeCandidatePathInclude},
	{name: "--include", kind: ProbeCandidatePathForcedInclude},
	{name: "-iquote", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-I", kind: ProbeCandidatePathInclude, joined: true},
	{name: "-F", kind: ProbeCandidatePathInclude, joined: true},
}

const probeCandidatePolicyAssembler = "assembler-forwarded"

// ValidateProbeCandidateArguments validates one fully rendered, explicitly
// tagged candidate argv. It intentionally does not recognize compiler
// families, target triples, or semantic option operands: unknown flags and
// their non-path scalar operands remain valid. It rejects only argv authority
// which could change the managed probe mode, select undeclared tools or code,
// or read/write undeclared files. An empty candidate is valid because all of
// its conditional groups may be unselected at execution time.
func ValidateProbeCandidateArguments(policy string, argv []string) ([]ProbeCandidatePathOperand, error) {
	switch policy {
	case ProbeCandidatePolicyCC, ProbeCandidatePolicyLD, ProbeCandidatePolicyCCLink:
	default:
		return nil, fmt.Errorf("unsupported probe candidate policy %q", policy)
	}
	tokens := make([]probeCandidateToken, len(argv))
	for index, argument := range argv {
		tokens[index] = probeCandidateToken{value: argument, argument: index, end: len(argument)}
	}
	return validateProbeCandidateTokens(policy, tokens)
}

func validateProbeCandidateTokens(policy string, tokens []probeCandidateToken) ([]ProbeCandidatePathOperand, error) {
	paths := []ProbeCandidatePathOperand{}
	mayHaveScalarOperand := false
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		argument := token.value
		if err := validateProbeToken(argument); err != nil {
			return nil, err
		}

		if !strings.HasPrefix(argument, "-") {
			if !mayHaveScalarOperand || strings.ContainsAny(argument, `/\\`) {
				return nil, fmt.Errorf("input path or positional argument is prohibited: %q", argument)
			}
			mayHaveScalarOperand = false
			continue
		}
		mayHaveScalarOperand = false

		if argument == "-" || argument == "--" {
			return nil, fmt.Errorf("probe-controlled input or option terminator is prohibited: %q", argument)
		}
		if option, offset, matched := matchProbeCandidatePathOption(policy, argument); matched {
			if offset < 0 {
				if index+1 >= len(tokens) {
					return nil, fmt.Errorf("missing path operand for %s", option.name)
				}
				index++
				pathToken := tokens[index]
				if err := validateProbeToken(pathToken.value); err != nil {
					return nil, fmt.Errorf("%s path operand: %w", option.name, err)
				}
				paths = append(paths, ProbeCandidatePathOperand{
					Kind: option.kind, Argument: pathToken.argument, Start: pathToken.start, End: pathToken.end,
				})
				continue
			}
			if offset == len(argument) {
				return nil, fmt.Errorf("empty path operand for %s", option.name)
			}
			paths = append(paths, ProbeCandidatePathOperand{
				Kind: option.kind, Argument: token.argument, Start: token.start + offset, End: token.end,
			})
			continue
		}
		if recognized, err := validateProbeCandidatePrefixMap(policy, argument); recognized {
			if err != nil {
				return nil, err
			}
			continue
		}

		if strings.HasPrefix(argument, "-Wl,") || strings.HasPrefix(argument, "-Wa,") || strings.HasPrefix(argument, "-Wp,") {
			forwarded, err := splitProbeCandidateForwarding(token, 4)
			if err != nil {
				return nil, err
			}
			forwardedPolicy := policy
			switch argument[:4] {
			case "-Wl,":
				forwardedPolicy = ProbeCandidatePolicyLD
			case "-Wa,":
				forwardedPolicy = probeCandidatePolicyAssembler
			case "-Wp,":
				forwardedPolicy = ProbeCandidatePolicyCC
			}
			forwardedPaths, err := validateProbeCandidateTokens(forwardedPolicy, forwarded)
			if err != nil {
				return nil, fmt.Errorf("unsafe forwarded argument %q: %w", argument, err)
			}
			paths = append(paths, forwardedPaths...)
			continue
		}
		if probeCandidateUnsafeForwarder(argument) {
			return nil, fmt.Errorf("unsafe forwarding option is prohibited: %q", argument)
		}
		if argument == "-mllvm" {
			if index+1 >= len(tokens) {
				return nil, fmt.Errorf("missing operand for -mllvm")
			}
			index++
			operand := tokens[index].value
			if err := validateProbeToken(operand); err != nil {
				return nil, fmt.Errorf("-mllvm operand: %w", err)
			}
			if forbiddenMllvmOperand(operand) || strings.ContainsAny(operand, `/\\`) {
				return nil, fmt.Errorf("unsafe -mllvm operand %q", operand)
			}
			continue
		}
		if strings.HasPrefix(argument, "-mllvm=") {
			operand := strings.TrimPrefix(argument, "-mllvm=")
			if operand == "" || forbiddenMllvmOperand(operand) || strings.ContainsAny(operand, `/\\`) {
				return nil, fmt.Errorf("unsafe -mllvm operand %q", operand)
			}
			continue
		}
		if strings.ContainsAny(argument, `/\`) {
			return nil, fmt.Errorf("untyped filesystem-bearing option is prohibited: %q", argument)
		}
		if class := forbiddenProbeCandidateOption(argument, policy); class != "" {
			return nil, fmt.Errorf("%s option is prohibited: %q", class, argument)
		}
		if probeCandidateOptionRequiresScalar(argument, policy) {
			if index+1 >= len(tokens) {
				return nil, fmt.Errorf("missing scalar operand for %s", argument)
			}
			index++
			operand := tokens[index].value
			if err := validateProbeToken(operand); err != nil {
				return nil, fmt.Errorf("%s scalar operand: %w", argument, err)
			}
			if strings.ContainsAny(operand, `/\`) {
				return nil, fmt.Errorf("%s scalar operand contains an untyped filesystem path: %q", argument, operand)
			}
			continue
		}
		mayHaveScalarOperand = probeCandidateOptionMayHaveScalar(argument)
	}
	return paths, nil
}

func probeCandidateOptionRequiresScalar(argument, policy string) bool {
	if strings.ContainsRune(argument, '=') {
		return false
	}
	for _, option := range []string{"-D", "-U", "-G", "-target", "-meabi"} {
		if argument == option {
			return true
		}
	}
	if policy == ProbeCandidatePolicyLD || policy == ProbeCandidatePolicyCCLink || policy == probeCandidatePolicyAssembler {
		return argument == "-m" || argument == "-z"
	}
	return false
}

func matchProbeCandidatePathOption(policy, argument string) (probeCandidatePathOption, int, bool) {
	for _, option := range probeCandidatePathOptions {
		if option.kind == ProbeCandidatePathRegularFile && policy != ProbeCandidatePolicyCC {
			continue
		}
		if argument == option.name {
			if option.equalsOnly {
				return option, len(argument), true
			}
			return option, -1, true
		}
		if strings.HasPrefix(argument, option.name+"=") {
			return option, len(option.name) + 1, true
		}
		if option.joined && strings.HasPrefix(argument, option.name) {
			return option, len(option.name), true
		}
	}
	return probeCandidatePathOption{}, 0, false
}

func validateProbeCandidatePrefixMap(policy, argument string) (bool, error) {
	for _, option := range []string{"-fmacro-prefix-map", "-ffile-prefix-map", "-fdebug-prefix-map"} {
		if argument != option && !strings.HasPrefix(argument, option+"=") {
			continue
		}
		if policy != ProbeCandidatePolicyCC {
			return true, fmt.Errorf("prefix-map option is prohibited by %s policy: %q", policy, argument)
		}
		mapping := strings.TrimPrefix(argument, option+"=")
		old, _, ok := strings.Cut(mapping, "=")
		if argument == option || !ok || old == "" {
			return true, fmt.Errorf("prefix-map option requires one nonempty old prefix and an explicit new prefix: %q", argument)
		}
		return true, nil
	}
	return false, nil
}

func splitProbeCandidateForwarding(token probeCandidateToken, prefixBytes int) ([]probeCandidateToken, error) {
	payload := token.value[prefixBytes:]
	if payload == "" {
		return nil, fmt.Errorf("empty forwarding option %q", token.value)
	}
	result := []probeCandidateToken{}
	start := 0
	for start <= len(payload) {
		end := strings.IndexByte(payload[start:], ',')
		if end < 0 {
			end = len(payload)
		} else {
			end += start
		}
		if end == start {
			return nil, fmt.Errorf("empty forwarded argument in %q", token.value)
		}
		result = append(result, probeCandidateToken{
			value: payload[start:end], argument: token.argument,
			start: token.start + prefixBytes + start, end: token.start + prefixBytes + end,
		})
		if end == len(payload) {
			break
		}
		start = end + 1
	}
	return result, nil
}

func probeCandidateOptionMatches(argument, option string, attached bool) bool {
	return argument == option || strings.HasPrefix(argument, option+"=") ||
		attached && len(argument) > len(option) && strings.HasPrefix(argument, option)
}

func probeCandidateUnsafeForwarder(argument string) bool {
	for _, option := range []string{"-Xclang", "-Xassembler", "-Xlinker"} {
		if probeCandidateOptionMatches(argument, option, false) {
			return true
		}
	}
	return false
}

func probeCandidateToolSelectionOption(argument string) bool {
	for _, option := range []struct {
		name     string
		attached bool
	}{
		{name: "-B", attached: true},
		{name: "--config"}, {name: "--config-system-dir"},
		{name: "--gcc-toolchain"}, {name: "-gcc-toolchain"},
		{name: "--ld-path"}, {name: "-fuse-ld"},
		{name: "--resource-dir"}, {name: "-resource-dir"},
		{name: "-specs"}, {name: "--specs"}, {name: "-wrapper"},
		{name: "--sysroot"}, {name: "-isysroot"},
		{name: "-ccc-install-dir"}, {name: "-gcc-install-dir"},
		{name: "-working-directory"}, {name: "-ivfsoverlay"},
	} {
		if probeCandidateOptionMatches(argument, option.name, option.attached) {
			return true
		}
	}
	return false
}

func forbiddenProbeCandidateOption(argument, policy string) string {
	if probeCandidateToolSelectionOption(argument) {
		return "tool-selection"
	}
	for _, option := range []string{
		"-fsanitize-system-ignorelist", "-fsanitize-system-blacklist",
		"-fsanitize-coverage-allowlist", "-fsanitize-coverage-ignorelist",
		"-fsanitize-coverage-whitelist", "-fsanitize-coverage-blacklist", "-fsanitize-coverage-blocklist",
		"-fexperimental-sanitize-metadata-ignorelist",
	} {
		if probeCandidateOptionMatches(argument, option, false) {
			return "undeclared-input"
		}
	}
	// Linux's cc-option probe owns the compile mode and output path. The exact
	// split-DWARF flag cannot select a path; proberun additionally proves that
	// its auxiliary output remains inside the private scratch working directory.
	// Linker and forwarded interfaces do not have that managed-output contract.
	if argument == "-gsplit-dwarf" && policy == ProbeCandidatePolicyCC {
		return ""
	}
	for _, option := range []struct {
		name     string
		attached bool
	}{
		{name: "-o", attached: true}, {name: "--output"},
		{name: "-x", attached: true}, {name: "--compile"}, {name: "--assemble"}, {name: "--preprocess"},
		{name: "-fsyntax-only"},
		{name: "-save-temps"}, {name: "--save-temps"}, {name: "-ftime-trace"},
		{name: "-serialize-diagnostics"}, {name: "--serialize-diagnostics"},
		{name: "-fprofile", attached: true}, {name: "-fcoverage", attached: true},
		{name: "--coverage"}, {name: "-coverage"}, {name: "-ftest-coverage"},
		{name: "-fdump-", attached: true}, {name: "-fstack-usage"}, {name: "-gsplit-dwarf"},
		{name: "-fsave-optimization-record"}, {name: "-foptimization-record-file"},
		{name: "-fopt-info", attached: true},
		{name: "-dumpdir"}, {name: "-dumpbase"}, {name: "-auxbase"}, {name: "-auxbase-strip"},
		{name: "-Map", attached: true}, {name: "--Map"}, {name: "--print-map"},
		{name: "--out-implib"}, {name: "--output-def"},
	} {
		if probeCandidateOptionMatches(argument, option.name, option.attached) {
			return "output/mode"
		}
	}
	switch argument {
	case "-c", "-S", "-E", "-M", "-MM", "-MD", "-MMD":
		return "output/mode"
	}
	for _, option := range []struct {
		name     string
		attached bool
	}{
		{name: "-MF", attached: true}, {name: "-MT", attached: true},
		{name: "-MQ", attached: true}, {name: "-MJ", attached: true},
		{name: "--dependency-file"},
	} {
		if probeCandidateOptionMatches(argument, option.name, option.attached) {
			return "dependency-output"
		}
	}
	for _, option := range []struct {
		name     string
		attached bool
	}{
		{name: "-fplugin", attached: true}, {name: "-fpass-plugin", attached: true},
		{name: "-fplugin-arg-", attached: true},
		{name: "-load", attached: true}, {name: "--load", attached: true},
		{name: "-load-pass-plugin", attached: true},
		{name: "-plugin", attached: true}, {name: "--plugin", attached: true},
		{name: "--plugin-opt", attached: true}, {name: "-cc1", attached: true},
	} {
		if probeCandidateOptionMatches(argument, option.name, option.attached) {
			return "plugin/code-loading"
		}
	}
	for _, option := range []struct {
		name     string
		attached bool
	}{
		{name: "-L", attached: true}, {name: "--library-path"},
		{name: "-l", attached: true}, {name: "-T", attached: true}, {name: "-R", attached: true},
		{name: "--script"}, {name: "--default-script"}, {name: "--version-script"}, {name: "--dynamic-list"},
		{name: "--section-ordering-file"}, {name: "--remap-inputs-file"}, {name: "--error-handling-script"},
		{name: "--retain-symbols-file"}, {name: "--just-symbols"},
		{name: "-rpath-link"},
		{name: "-fsanitize-ignorelist"}, {name: "-fsanitize-blacklist"},
		{name: "-fmodule-map-file"}, {name: "-fmodule-file"}, {name: "-fmodules-cache-path"},
	} {
		if probeCandidateOptionMatches(argument, option.name, option.attached) {
			return "undeclared-input"
		}
	}
	if policy == probeCandidatePolicyAssembler {
		if argument == "-a" || strings.HasPrefix(argument, "-a=") ||
			strings.HasPrefix(argument, "-al") || strings.HasPrefix(argument, "-as") ||
			probeCandidateOptionMatches(argument, "--MD", true) {
			return "assembler-output"
		}
	}
	return ""
}

func probeCandidateOptionMayHaveScalar(argument string) bool {
	// There is deliberately no option-name table here. A future compiler may
	// give any otherwise harmless option one scalar operand; known filesystem
	// and execution authorities have already been handled above.
	return !strings.ContainsRune(argument, '=')
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

func forbiddenMllvmOperand(operand string) bool {
	lower := strings.ToLower(operand)
	for _, prefix := range []string{
		"-load", "--load", "-plugin", "--plugin",
		"-o=", "--output=", "-output=",
		"-file=", "-filename=", "-path=", "-directory=", "-dir=",
		"-stats-file=", "-pass-remarks-output=",
	} {
		if lower == strings.TrimSuffix(prefix, "=") || strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func shellExpr(command string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "expr" || len(fields) > 64 {
		return "", fmt.Errorf("unsupported expr command %q", command)
	}
	values := make([]int64, 0, (len(fields)+1)/2)
	operators := make([]string, 0, len(fields)/2)
	for index, field := range fields[1:] {
		if index%2 == 0 {
			if err := ValidateProbeSignedDecimalArgument(field); err != nil {
				return "", fmt.Errorf("unsupported expr command %q", command)
			}
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return "", fmt.Errorf("unsupported expr command %q", command)
			}
			values = append(values, value)
			continue
		}
		if field == `\*` {
			field = "*"
		}
		switch field {
		case "+", "-", "*", "/", "%":
			operators = append(operators, field)
		default:
			return "", fmt.Errorf("unsupported expr command %q", command)
		}
	}
	if len(values) != len(operators)+1 {
		return "", fmt.Errorf("unsupported expr command %q", command)
	}

	// POSIX expr applies multiplication, division, and remainder before
	// addition and subtraction. Reduce that tier first, preserving the
	// command's left associativity, then reduce the remaining sum.
	reducedValues := []int64{values[0]}
	reducedOperators := make([]string, 0, len(operators))
	for index, operator := range operators {
		right := values[index+1]
		if operator == "+" || operator == "-" {
			reducedOperators = append(reducedOperators, operator)
			reducedValues = append(reducedValues, right)
			continue
		}
		left := reducedValues[len(reducedValues)-1]
		value, ok := shellExprBinary(left, right, operator)
		if !ok {
			return "", fmt.Errorf("unsupported expr command %q", command)
		}
		reducedValues[len(reducedValues)-1] = value
	}
	value := reducedValues[0]
	for index, operator := range reducedOperators {
		var ok bool
		value, ok = shellExprBinary(value, reducedValues[index+1], operator)
		if !ok {
			return "", fmt.Errorf("unsupported expr command %q", command)
		}
	}
	return strconv.FormatInt(value, 10), nil
}

func shellExprBinary(left, right int64, operator string) (int64, bool) {
	switch operator {
	case "+":
		value := left + right
		return value, (right <= 0 || value >= left) && (right >= 0 || value <= left)
	case "-":
		value := left - right
		return value, (right >= 0 || value >= left) && (right <= 0 || value <= left)
	case "*":
		if left == 0 || right == 0 {
			return 0, true
		}
		if (left == -1<<63 && right == -1) || (right == -1<<63 && left == -1) {
			return 0, false
		}
		value := left * right
		return value, value/right == left
	case "/":
		if right == 0 || (left == -1<<63 && right == -1) {
			return 0, false
		}
		return left / right, true
	case "%":
		if right == 0 || (left == -1<<63 && right == -1) {
			return 0, false
		}
		return left % right, true
	default:
		return 0, false
	}
}

func unquoteShell(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}
