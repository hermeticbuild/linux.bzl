package kconfig

// Dynamic expr probes preserve the ordering imposed by GNU Make when a
// source-owned $(shell expr ...) consumes text measured by an earlier probe.
// The planner validates only a bounded scalar expression grammar.  The
// selected script runtime evaluates that expression after map_directory has
// supplied the dependency result, so no compiler-derived value is guessed in
// the repository phase.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// GNU Make consumes $(shell ...) stdout for expr's ordinary true, false, and
// invalid-expression statuses. scriptrun validates the required expr applet
// before starting this script; propagate statuses above expr's documented
// 0/1/2 range so RequireSuccess still catches runtime/tool setup failures.
const linuxProbeDynamicExprScript = "expr \"$@\"\nstatus=$?\ncase \"$status\" in 0|1|2) exit 0;; *) exit \"$status\";; esac\n"

func (e *LinuxProbeEvaluator) dynamicShellExpr(command string) (string, bool, error) {
	if !strings.HasPrefix(command, "expr ") || !linuxProbeSymbolPattern.MatchString(command) {
		return "", false, nil
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return "", true, fmt.Errorf(
			"%s dynamic expr probe requires configured %s and %s roles",
			e.scope, linuxProbeScriptRunner, linuxProbeScriptRuntime,
		)
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", true, fmt.Errorf("dynamic expr probe: %w", err)
	}
	// One binary scalar expression is the complete dynamic grammar. Keeping
	// the argv shape fixed lets the runner validate every measured operand as
	// one decimal before expr can interpret it as another operator or function.
	if len(tokens) != 4 || tokens[0].operator || tokens[0].value != "expr" {
		return "", true, fmt.Errorf("unsupported dynamic expr command %q", command)
	}

	arguments := make([]string, 0, len(tokens)-1)
	dynamicIndexes := []int{}
	for index, token := range tokens[1:] {
		if token.operator {
			return "", true, fmt.Errorf("unsupported dynamic expr shell operator %q in %q", token.value, command)
		}
		value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
		raw := command[token.start:token.end]
		if index%2 == 0 {
			if exactLinuxProbeSymbol(value) {
				// Make inserts this unquoted value into shell source upstream. Accept
				// only one exact measured atom here, then deliberately fail closed at
				// runtime by requiring one signed decimal instead of granting the
				// measured text shell syntax, globbing, or additional expr operands.
				if raw != value {
					return "", true, fmt.Errorf("dynamic expr operand must be one unquoted measured value in %q", command)
				}
				symbol, exists, adoptErr := e.adoptSymbol(value)
				if adoptErr != nil {
					return "", true, fmt.Errorf("dynamic expr operand %q: %w", value, adoptErr)
				}
				if !exists {
					return "", true, fmt.Errorf("dynamic expr operand references unknown measured value %q", value)
				}
				if symbol.kind != "text" && symbol.kind != "transformed-text" && symbol.kind != "make-text" {
					return "", true, fmt.Errorf("dynamic expr operand %q has non-text kind %q", value, symbol.kind)
				}
				dynamicIndexes = append(dynamicIndexes, index)
			} else if ValidateProbeSignedDecimalArgument(value) != nil {
				return "", true, fmt.Errorf("unsupported dynamic expr operand %q", value)
			} else if _, parseErr := strconv.ParseInt(value, 10, 64); parseErr != nil {
				return "", true, fmt.Errorf("unsupported dynamic expr operand %q", value)
			}
		} else if !safeDynamicExprOperator(raw, value) {
			return "", true, fmt.Errorf("unsupported dynamic expr operator %q", value)
		}
		arguments = append(arguments, value)
	}
	if len(dynamicIndexes) == 0 {
		return "", false, nil
	}

	arguments, conditional, fragments, dependencies, err := e.lowerSymbolicArguments(arguments)
	if err != nil {
		return "", true, fmt.Errorf("lower dynamic expr arguments: %w", err)
	}
	if len(conditional) != 0 || len(fragments) != len(dynamicIndexes) {
		return "", true, fmt.Errorf(
			"lower dynamic expr arguments produced %d conditional and %d text groups for %d measured operands",
			len(conditional), len(fragments), len(dynamicIndexes),
		)
	}
	for index, dynamicIndex := range dynamicIndexes {
		if fragments[index].Index != dynamicIndex {
			return "", true, fmt.Errorf(
				"lower dynamic expr operand %d into argv index %d, want %d",
				index, fragments[index].Index, dynamicIndex,
			)
		}
		fragments[index].Mode = ProbeArgumentFragmentsModeSignedDecimal
	}
	configured := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		configured = append(configured, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	applets, err := compactKbuildScriptRuntimeApplets(configured, e.scope)
	if err != nil {
		return "", true, err
	}
	prefix := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_content", linuxProbeDynamicExprScript,
	}
	auxiliary := make([]string, 0, len(applets))
	for _, applet := range applets {
		prefix = append(prefix, "-applet", applet.name+"=${tool:"+applet.role+"}")
		auxiliary = append(auxiliary, applet.role)
	}
	slices.Sort(auxiliary)
	prefix = append(prefix, "-require_applet", "expr", "-require_applet", "sh", "--")
	for index := range conditional {
		conditional[index].Before += len(prefix)
	}
	for index := range fragments {
		fragments[index].Index += len(prefix)
	}
	arguments = append(prefix, arguments...)
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "dynamic-expr", Tool: linuxProbeScriptRunner,
			AuxiliaryTools: auxiliary, Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: fragments,
		}},
		// expr uses a nonzero exit status for a false comparison while still
		// printing its value.  GNU Make consumes stdout regardless of that status.
		Outcome: ProbeOutcome{
			Kind: "text", Step: "dynamic-expr", Stream: "stdout", GNUMakeShell: true, RequireSuccess: true,
		},
	}
	if err := request.Validate(); err != nil {
		return "", true, fmt.Errorf("dynamic expr probe request: %w", err)
	}
	value, err := e.requestText(request, dependencies...)
	return value, true, err
}

func exactLinuxProbeSymbol(value string) bool {
	matches := linuxProbeSymbolPattern.FindAllString(value, -1)
	return len(matches) == 1 && matches[0] == value
}

func safeDynamicExprOperator(raw, value string) bool {
	switch value {
	case "+", "-", "/", "%", "=", "!=":
		return raw == value || sourceShellQueryQuotedLiteral(raw)
	case "*", "<", "<=", ">", ">=":
		// These spellings have glob/redirection meaning to a shell.  Accept only
		// an explicit source escape or one exact quoted word before lowering to
		// shell-free argv.
		return raw == "\\"+value || sourceShellQueryQuotedLiteral(raw)
	default:
		return false
	}
}
