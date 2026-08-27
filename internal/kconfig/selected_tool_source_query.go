package kconfig

// This file lowers one source-owned text query whose producer is a selected
// configured tool and whose only input is an immutable Linux source file.  A
// representative upstream use asks bindgen for libclang's version banner and
// extracts one field with sed.  Neither the tool role, source name, regular
// expression, nor extracted value is modeled here: they remain selected
// Kbuild data and execute through the configured script runtime.

import (
	"fmt"
	"slices"
	"strings"
)

const selectedToolSourceTreePrefix = "__LINUX_BZL_SOURCE_TREE__/"

// selectedToolSourceQuery recognizes this deliberately small shell grammar:
//
//	CONFIGURED_TOOL IMMUTABLE_SOURCE 2>&1 | sed -ne 's/PATTERN/REPLACEMENT/p'
//
// The live stderr/stdout merge is required because tools commonly print their
// identity banner on stderr.  Sed runs with output suppression and exactly one
// printing substitution, so it has no file, command-execution, or write
// authority.  Every accepted word is reconstructed after validation rather
// than copying source shell text into the action.
func (e *LinuxProbeEvaluator) selectedToolSourceQuery(command string) (string, bool, error) {
	request, recognized, err := e.selectedToolSourceQueryRequest(command)
	if err != nil || !recognized {
		return "", recognized, err
	}
	value, err := e.requestText(request)
	return value, true, err
}

func (e *LinuxProbeEvaluator) selectedToolSourceQueryRequest(command string) (ProbeRequest, bool, error) {
	if e == nil {
		return ProbeRequest{}, false, fmt.Errorf("Linux selected-tool source-query evaluator is nil")
	}
	canonical := e.canonicalSourceCommand(strings.TrimSpace(command))
	if canonical == "" || !strings.Contains(canonical, selectedToolSourceTreePrefix) {
		return ProbeRequest{}, false, nil
	}
	if len(canonical) > 4096 || strings.ContainsAny(canonical, "\x00\r\n") || strings.Contains(canonical, compactKbuildLiteralDollarToken) {
		if e.ownsProbeCommand(canonical) {
			return ProbeRequest{}, true, e.unsupportedCommand(command)
		}
		return ProbeRequest{}, false, nil
	}

	tokens, err := lexCompactKbuildRecipe(canonical)
	if err != nil {
		if e.ownsProbeCommand(canonical) {
			return ProbeRequest{}, true, fmt.Errorf("lex selected-tool source query: %w", err)
		}
		return ProbeRequest{}, false, nil
	}
	if len(tokens) == 0 || tokens[0].operator {
		return ProbeRequest{}, false, nil
	}
	role, selected, err := e.compoundTryRunConfiguredToolRole(tokens[0].value)
	if err != nil {
		return ProbeRequest{}, true, err
	}
	if !selected {
		return ProbeRequest{}, false, nil
	}
	if role == linuxProbeScriptRunner || role == linuxProbeScriptRuntime || strings.HasPrefix(role, compactKbuildScriptAppletRolePrefix) {
		return ProbeRequest{}, true, fmt.Errorf("selected-tool source query invokes infrastructure role %q", role)
	}
	if role == "sed" {
		return ProbeRequest{}, true, fmt.Errorf("selected-tool source query producer role %q collides with its reducer", role)
	}
	if len(tokens) < 10 || len(tokens) > 12 {
		return ProbeRequest{}, true, e.unsupportedCommand(command)
	}

	sourceToken := tokens[1]
	if sourceToken.operator || !strings.HasPrefix(sourceToken.value, selectedToolSourceTreePrefix) {
		return ProbeRequest{}, true, e.unsupportedCommand(command)
	}
	source := strings.TrimPrefix(sourceToken.value, selectedToolSourceTreePrefix)
	if !selectedToolSourceSafePath(source) {
		return ProbeRequest{}, true, fmt.Errorf("selected-tool source query has unsafe source operand %q", sourceToken.value)
	}
	source, err = e.immutableLinuxSourcePath(source, false)
	if err != nil {
		return ProbeRequest{}, true, fmt.Errorf("selected-tool source query input: %w", err)
	}

	// Requiring contiguous token offsets distinguishes the stderr merge from
	// unrelated arguments and redirections such as `2 > & 1` or `2>file`.
	if tokens[2].operator || tokens[2].value != "2" ||
		!tokens[3].operator || tokens[3].value != ">" ||
		!tokens[4].operator || tokens[4].value != "&" ||
		tokens[5].operator || tokens[5].value != "1" ||
		tokens[2].end != tokens[3].start || tokens[3].end != tokens[4].start || tokens[4].end != tokens[5].start ||
		!tokens[6].operator || tokens[6].value != "|" {
		return ProbeRequest{}, true, e.unsupportedCommand(command)
	}
	for _, token := range tokens[7:] {
		if token.operator {
			return ProbeRequest{}, true, fmt.Errorf("selected-tool source query uses unsupported operator %q", token.value)
		}
	}
	if tokens[7].value != "sed" {
		return ProbeRequest{}, true, e.unsupportedCommand(command)
	}
	expression, sedArguments, err := selectedToolSourceSed(tokens[8:], canonical)
	if err != nil {
		return ProbeRequest{}, true, err
	}
	if err := validateSelectedToolSourceSedSubstitution(expression); err != nil {
		return ProbeRequest{}, true, err
	}

	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return ProbeRequest{}, true, fmt.Errorf(
			"%s selected-tool source query requires configured %s and %s roles",
			e.scope, linuxProbeScriptRunner, linuxProbeScriptRuntime,
		)
	}
	configured := make([]KbuildActionRoleRef, 0, len(e.tools))
	for configuredRole := range e.tools {
		configured = append(configured, KbuildActionRoleRef{Scope: e.scope, Role: configuredRole})
	}
	applets, err := compactKbuildScriptRuntimeApplets(configured, e.scope)
	if err != nil {
		return ProbeRequest{}, true, err
	}

	scriptWords := []string{
		role,
		sourceShellQueryScriptWord(source),
		"2>&1",
		"|",
		"sed",
	}
	for _, argument := range sedArguments {
		scriptWords = append(scriptWords, sourceShellQueryScriptWord(argument))
	}
	arguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_content", strings.Join(scriptWords, " "),
		"-tool", role + "=${tool:" + role + "}",
	}
	auxiliary := []string{role}
	for _, applet := range applets {
		if applet.name != "sed" {
			continue
		}
		arguments = append(arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
		auxiliary = append(auxiliary, applet.role)
	}
	arguments = append(arguments,
		"-require_applet", "sed",
		"-require_applet", "sh",
		"--",
	)
	slices.Sort(auxiliary)
	auxiliary = slices.Compact(auxiliary)
	sources := []string{linuxProbeRootAnchor, source}
	slices.Sort(sources)
	sources = slices.Compact(sources)

	const stepName = "selected-tool-source-query"
	request := ProbeRequest{
		Schema:      LinuxProbeRequestSchema,
		Sources:     sources,
		SourceRoots: []string{linuxProbeSourceRootName},
		Steps: []ProbeStep{{
			Name: stepName, Tool: linuxProbeScriptRunner,
			AuxiliaryTools:   auxiliary,
			WorkingDirectory: "${source_root:" + linuxProbeSourceRootName + "}",
			Arguments:        arguments,
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: stepName, Stream: "stdout", GNUMakeShell: true, RequireSuccess: true,
		},
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, true, fmt.Errorf("selected-tool source query request: %w", err)
	}
	return request, true, nil
}

// selectedToolSourceSafePath makes reconstructing the originally unquoted
// Kbuild source operand semantics-preserving.  Linux source components in
// this query need no globbing, parameter expansion, or shell punctuation.
func selectedToolSourceSafePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if !safeLinuxProbePathComponent(component) {
			return false
		}
	}
	return true
}

// selectedToolSourceSed accepts combined `-ne` or separate `-n -e` options.
// The expression must be one single-quoted source literal so rebuilding it as
// a shell literal cannot alter active expansion semantics.
func selectedToolSourceSed(tokens []compactKbuildRecipeToken, command string) (string, []string, error) {
	expressionIndex := -1
	switch {
	case len(tokens) == 2 && (tokens[0].value == "-ne" || tokens[0].value == "-en"):
		expressionIndex = 1
	case len(tokens) == 3 &&
		(tokens[0].value == "-n" && tokens[1].value == "-e" || tokens[0].value == "-e" && tokens[1].value == "-n"):
		expressionIndex = 2
	default:
		return "", nil, fmt.Errorf("selected-tool source query sed requires exactly -n, -e, and one expression")
	}
	raw := command[tokens[expressionIndex].start:tokens[expressionIndex].end]
	if len(raw) < 2 || raw[0] != '\'' || !sourceShellQueryQuotedLiteral(raw) {
		return "", nil, fmt.Errorf("selected-tool source query sed expression must be one single-quoted literal")
	}
	expression := strings.ReplaceAll(tokens[expressionIndex].value, compactKbuildLiteralDollarToken, "$")
	if strings.Contains(expression, "${") {
		return "", nil, fmt.Errorf("selected-tool source query sed expression uses reserved probe placeholder syntax")
	}
	arguments := make([]string, len(tokens))
	for index, token := range tokens {
		arguments[index] = strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
	}
	return expression, arguments, nil
}

// validateSelectedToolSourceSedSubstitution admits only a single substitution
// with sed's print flag.  Empty patterns are rejected because sed would reuse
// hidden regex state from an earlier command.
func validateSelectedToolSourceSedSubstitution(expression string) error {
	if len(expression) < 6 || expression[0] != 's' {
		return fmt.Errorf("selected-tool source query has unsupported sed expression %q", expression)
	}
	delimiter := expression[1]
	if delimiter < 0x21 || delimiter > 0x7e || delimiter == '\\' {
		return fmt.Errorf("selected-tool source query has invalid sed delimiter %q", delimiter)
	}
	patternEnd, err := linuxCompilerMachineDelimitedEnd(expression, 2, delimiter)
	if err != nil {
		return fmt.Errorf("selected-tool source query has malformed sed pattern: %w", err)
	}
	if patternEnd == 2 {
		return fmt.Errorf("selected-tool source query sed pattern is empty")
	}
	replacementEnd, err := linuxCompilerMachineDelimitedEnd(expression, patternEnd+1, delimiter)
	if err != nil {
		return fmt.Errorf("selected-tool source query has malformed sed replacement: %w", err)
	}
	if expression[replacementEnd+1:] != "p" {
		return fmt.Errorf("selected-tool source query sed substitution must have exactly the print flag")
	}
	return nil
}
