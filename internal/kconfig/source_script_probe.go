package kconfig

// This file lowers an invocation of a declared Linux source script into the
// generic probe protocol. It intentionally contains no script-name switch and
// no reconstruction of script behavior: the selected source file, registered
// script runtime, and configured tool action contracts determine the result.

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	linuxProbeSourceRootName = "linux"
	rustProbeSourceRootName  = "rust"
	linuxProbeRootAnchor     = "Kconfig"
	linuxProbeScriptRunner   = "scriptrun"
	linuxProbeScriptRuntime  = "script-runtime"
	linuxProbeScriptOutput   = "output"
)

// KbuildSourceScriptArgumentKind identifies one typed argv capability for
// SourceScriptOutputText. Values are never parsed as command text: each kind
// is lowered directly to one probe-protocol argument.
type KbuildSourceScriptArgumentKind string

const (
	// KbuildSourceScriptOutputArgument passes the one private writable output
	// file. Its Value must be empty and exactly one output argument is required.
	KbuildSourceScriptOutputArgument KbuildSourceScriptArgumentKind = "output"
	// KbuildSourceScriptStdoutArgument declares that the script's stdout is the
	// generated content. Its Value must be empty and it is not passed in argv.
	KbuildSourceScriptStdoutArgument KbuildSourceScriptArgumentKind = "stdout"
	// KbuildSourceScriptSourceArgument passes one immutable Linux source file.
	// Value is a canonical path relative to the selected Linux source root.
	KbuildSourceScriptSourceArgument KbuildSourceScriptArgumentKind = "source"
	// KbuildSourceScriptLiteralArgument passes Value as one exact argv word.
	KbuildSourceScriptLiteralArgument KbuildSourceScriptArgumentKind = "literal"
	// KbuildSourceScriptToolArgument passes the private proxy for the configured
	// tool role named by Value.
	KbuildSourceScriptToolArgument KbuildSourceScriptArgumentKind = "tool"
)

// KbuildSourceScriptArgument is one ordered argument to an immutable Linux
// source script executed by SourceScriptOutputText.
type KbuildSourceScriptArgument struct {
	Kind  KbuildSourceScriptArgumentKind
	Value string
}

type linuxSourceScriptInvocation struct {
	path                 string
	arguments            []string
	environment          map[string]string
	environmentFragments []ProbeEnvironmentFragments
	auxiliary            []string
	sourceRoots          []string
	dependencies         []ProbeReference
	conditional          []ProbeConditionalArguments
	argumentFragments    []ProbeArgumentFragments
	candidate            *ProbeCandidateArguments
}

func cleanOptionalProbeSourceRoot(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return filepath.Clean(value)
}

const linuxProbeReadOutputScript = "exec cat \"$1\"\n"

// SourceScriptOutputText executes one immutable shell script from the selected
// Linux source tree. The script receives exactly the typed arguments supplied
// by the caller and must write the single output argument. A second hermetic
// scriptrun invocation reads that file with the selected runtime's cat applet.
//
// Discovery registers the content-addressed request and returns ("", false,
// nil). Replay reconstructs the same request, verifies its oracle result, and
// returns the output file's exact bytes with concrete=true.
func (s *KbuildProbeScopes) SourceScriptOutputText(
	scope string,
	script string,
	arguments []KbuildSourceScriptArgument,
) (text string, concrete bool, err error) {
	if s == nil {
		return "", false, fmt.Errorf("Kbuild probe scopes are nil")
	}
	evaluator := s.evaluators[scope]
	if evaluator == nil {
		return "", false, fmt.Errorf("Kbuild probe workload has no %s scope", scope)
	}
	request, dependencies, err := evaluator.sourceScriptOutputTextRequest(script, arguments)
	if err != nil {
		return "", false, err
	}
	request = evaluator.canonicalSourceRequest(request)
	reference, err := evaluator.discovery.Request(scope, request, dependencies...)
	if err != nil {
		return "", false, err
	}
	if err := evaluator.symbolRegistry.publishDefinition(reference, request, dependencies); err != nil {
		return "", false, err
	}
	if !evaluator.seen[reference.NodeID] {
		evaluator.seen[reference.NodeID] = true
		evaluator.references = append(evaluator.references, reference)
	}
	if evaluator.oracle == nil {
		return "", false, nil
	}
	text, err = evaluator.readText(reference, request, dependencies...)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

func (e *LinuxProbeEvaluator) sourceScriptOutputTextRequest(
	script string,
	descriptors []KbuildSourceScriptArgument,
) (ProbeRequest, []ProbeReference, error) {
	if e == nil {
		return ProbeRequest{}, nil, fmt.Errorf("Linux source-script probe evaluator is nil")
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return ProbeRequest{}, nil, fmt.Errorf(
			"%s source-script output probe requires configured %s and %s roles",
			e.scope, linuxProbeScriptRunner, linuxProbeScriptRuntime,
		)
	}
	script, err := e.immutableLinuxSourcePath(script, true)
	if err != nil {
		return ProbeRequest{}, nil, fmt.Errorf("source-script output probe script: %w", err)
	}

	sources := map[string]bool{linuxProbeRootAnchor: true, script: true}
	toolRoles := map[string]bool{}
	argv := make([]string, 0, len(descriptors))
	fileOutputs := 0
	stdoutOutputs := 0
	for index, descriptor := range descriptors {
		switch descriptor.Kind {
		case KbuildSourceScriptOutputArgument:
			if descriptor.Value != "" {
				return ProbeRequest{}, nil, fmt.Errorf("source-script output argument %d has nonempty value", index)
			}
			fileOutputs++
			argv = append(argv, "${scratch:"+linuxProbeScriptOutput+"}")
		case KbuildSourceScriptStdoutArgument:
			if descriptor.Value != "" {
				return ProbeRequest{}, nil, fmt.Errorf("source-script stdout argument %d has nonempty value", index)
			}
			stdoutOutputs++
		case KbuildSourceScriptSourceArgument:
			source, sourceErr := e.immutableLinuxSourcePath(descriptor.Value, false)
			if sourceErr != nil {
				return ProbeRequest{}, nil, fmt.Errorf("source-script source argument %d: %w", index, sourceErr)
			}
			sources[source] = true
			argv = append(argv, "${source:"+source+"}")
		case KbuildSourceScriptLiteralArgument:
			if err := validateSourceScriptProtocolLiteral(descriptor.Value); err != nil {
				return ProbeRequest{}, nil, fmt.Errorf("source-script literal argument %d: %w", index, err)
			}
			if linuxProbeSymbolPattern.MatchString(descriptor.Value) {
				return ProbeRequest{}, nil, fmt.Errorf("source-script literal argument %d contains a symbolic probe value", index)
			}
			argv = append(argv, descriptor.Value)
		case KbuildSourceScriptToolArgument:
			role := descriptor.Value
			if !validKbuildActionRolePart(role) || e.tools[role] == "" {
				return ProbeRequest{}, nil, fmt.Errorf("source-script tool argument %d has unavailable role %q", index, role)
			}
			toolRoles[role] = true
			argv = append(argv, role)
		default:
			return ProbeRequest{}, nil, fmt.Errorf("source-script argument %d has unsupported kind %q", index, descriptor.Kind)
		}
	}
	if fileOutputs+stdoutOutputs != 1 {
		return ProbeRequest{}, nil, fmt.Errorf(
			"source-script output probe requires exactly one output authority, got %d file and %d stdout",
			fileOutputs, stdoutOutputs,
		)
	}

	environment := map[string]string{}
	if err := e.inheritSourceScriptEnvironment(environment, toolRoles); err != nil {
		return ProbeRequest{}, nil, err
	}
	lowerer := newProbeSymbolicValueLowerer(e)
	environmentFragments, sourceRoots, err := lowerSourceScriptEnvironment(environment, lowerer)
	if err != nil {
		return ProbeRequest{}, nil, err
	}

	configured := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		configured = append(configured, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	applets, err := compactKbuildScriptRuntimeApplets(configured, e.scope)
	if err != nil {
		return ProbeRequest{}, nil, err
	}
	roles := make([]string, 0, len(toolRoles))
	for role := range toolRoles {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	generateArguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script", "${source:" + script + "}",
	}
	for _, role := range roles {
		generateArguments = append(generateArguments, "-tool", role+"=${tool:"+role+"}")
	}
	generateAuxiliary := slices.Clone(roles)
	readAuxiliary := make([]string, 0, len(applets))
	for _, applet := range applets {
		binding := applet.name + "=${tool:" + applet.role + "}"
		generateArguments = append(generateArguments, "-applet", binding)
		generateAuxiliary = append(generateAuxiliary, applet.role)
		readAuxiliary = append(readAuxiliary, applet.role)
	}
	slices.Sort(generateAuxiliary)
	generateAuxiliary = slices.Compact(generateAuxiliary)
	slices.Sort(readAuxiliary)
	generateArguments = append(generateArguments, "--")
	generateArguments = append(generateArguments, argv...)

	readArguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(linuxProbeReadOutputScript)),
	}
	for _, applet := range applets {
		readArguments = append(readArguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
	}
	readArguments = append(readArguments, "-require_applet", "cat", "--", "${scratch:"+linuxProbeScriptOutput+"}")

	declaredSources := make([]string, 0, len(sources))
	for source := range sources {
		declaredSources = append(declaredSources, source)
	}
	slices.Sort(declaredSources)
	generate := ProbeStep{
		Name: "source-script-output", Tool: linuxProbeScriptRunner,
		AuxiliaryTools:   generateAuxiliary,
		WorkingDirectory: "${source_root:" + linuxProbeSourceRootName + "}",
		Arguments:        generateArguments, Environment: environment, EnvironmentFragments: environmentFragments,
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Sources: declaredSources, SourceRoots: sourceRoots,
		Steps:   []ProbeStep{generate},
		Outcome: ProbeOutcome{Kind: "text", Step: generate.Name, Stream: "stdout", RequireSuccess: true},
	}
	if fileOutputs == 1 {
		request.Scratch = []ProbeScratch{{Name: linuxProbeScriptOutput, Kind: "file"}}
		request.Steps[0].DiscardStdout = true
		request.Steps = append(request.Steps,
			ProbeStep{
				Name: "read-source-script-output", Tool: linuxProbeScriptRunner,
				AuxiliaryTools: readAuxiliary, Arguments: readArguments,
				When: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
					{Operator: "exit-zero", Step: "source-script-output"},
					{Operator: "regular-file", Scratch: linuxProbeScriptOutput},
				}},
			},
		)
		request.Outcome.Step = "read-source-script-output"
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, nil, fmt.Errorf("source-script output probe request: %w", err)
	}
	return request, slices.Clone(lowerer.dependencies), nil
}

func (e *LinuxProbeEvaluator) immutableLinuxSourcePath(value string, requireShell bool) (string, error) {
	if err := validateProbeSourcePath(value); err != nil {
		return "", err
	}
	if e.sourceRoot == "" {
		return "", fmt.Errorf("selected Linux source root is empty")
	}
	root, err := filepath.Abs(e.sourceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve selected Linux source root: %w", err)
	}
	source, regular, err := sourceShellQueryFile(root, root, value)
	if err != nil {
		return "", err
	}
	if !regular || source != value {
		return "", fmt.Errorf("Linux source path %q is not one immutable regular file", value)
	}
	if requireShell && !strings.HasSuffix(source, ".sh") && !compactKbuildSourceFileUsesShell(filepath.Join(root, filepath.FromSlash(source))) {
		return "", fmt.Errorf("Linux source path %q is not a shell script", value)
	}
	return source, nil
}

// sourceScriptRequest recognizes one shell-free invocation whose program is a
// shell helper beneath the selected Linux source root. Extensionless helpers
// are selected from their immutable shebang; optional `env NAME=VALUE`
// prefixes are represented as action environment, and exact configured tool
// tokens become private scriptrun proxy names. Unknown source scripts require
// no Go change.
func (e *LinuxProbeEvaluator) sourceScriptRequest(command string, outcomeKind string) (ProbeRequest, []ProbeReference, bool, error) {
	invocation, recognized, err := e.parseSourceScriptInvocation(command)
	if err != nil || !recognized {
		return ProbeRequest{}, nil, recognized, err
	}
	configuredRoles := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		configuredRoles = append(configuredRoles, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	runtimeApplets, err := compactKbuildScriptRuntimeApplets(configuredRoles, e.scope)
	if err != nil {
		return ProbeRequest{}, nil, true, err
	}
	prefix := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script", "${source:" + invocation.path + "}",
	}
	for _, role := range invocation.auxiliary {
		prefix = append(prefix, "-tool", role+"=${tool:"+role+"}")
	}
	auxiliary := slices.Clone(invocation.auxiliary)
	for _, applet := range runtimeApplets {
		prefix = append(prefix, "-applet", applet.name+"=${tool:"+applet.role+"}")
		auxiliary = append(auxiliary, applet.role)
	}
	slices.Sort(auxiliary)
	auxiliary = slices.Compact(auxiliary)
	prefix = append(prefix, "--")
	arguments := append(prefix, invocation.arguments...)
	conditional := slices.Clone(invocation.conditional)
	for index := range conditional {
		conditional[index].Before += len(prefix)
	}
	argumentFragments := slices.Clone(invocation.argumentFragments)
	for index := range argumentFragments {
		argumentFragments[index].Index += len(prefix)
	}
	candidate := invocation.candidate
	if candidate != nil {
		candidate = &ProbeCandidateArguments{
			Policy:      candidate.Policy,
			Base:        slices.Clone(candidate.Base),
			Conditional: slices.Clone(candidate.Conditional),
		}
		for index := range candidate.Base {
			candidate.Base[index] += len(prefix)
		}
	}
	sources := []string{linuxProbeRootAnchor, invocation.path}
	slices.Sort(sources)
	sources = slices.Compact(sources)
	step := ProbeStep{
		Name: "source-script", Tool: linuxProbeScriptRunner,
		AuxiliaryTools:   auxiliary,
		WorkingDirectory: "${source_root:" + linuxProbeSourceRootName + "}",
		Arguments:        arguments, ConditionalArguments: conditional, ArgumentFragments: argumentFragments,
		Candidate: candidate, Environment: invocation.environment, EnvironmentFragments: invocation.environmentFragments,
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(invocation.dependencies),
		Sources: sources, SourceRoots: invocation.sourceRoots,
		Steps: []ProbeStep{step},
	}
	switch outcomeKind {
	case "boolean":
		request.Outcome = ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: step.Name}}
	case "text":
		request.Outcome = ProbeOutcome{Kind: "text", Step: step.Name, Stream: "stdout", TrimSpace: true}
	default:
		return ProbeRequest{}, nil, true, fmt.Errorf("unsupported source-script probe outcome %q", outcomeKind)
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, nil, true, fmt.Errorf("source script %s: %w", invocation.path, err)
	}
	return request, invocation.dependencies, true, nil
}

func (e *LinuxProbeEvaluator) parseSourceScriptInvocation(command string) (linuxSourceScriptInvocation, bool, error) {
	if !e.looksLikeSourceScript(command) {
		return linuxSourceScriptInvocation{}, false, nil
	}
	// The lexer owns this marker. Reject a source command which already
	// contains it so only quote/escape handling can create literal-dollar
	// provenance.
	if strings.Contains(command, compactKbuildLiteralDollarToken) {
		return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script command contains a reserved lexer token")
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return linuxSourceScriptInvocation{}, e.looksLikeSourceScript(command), err
	}
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.operator {
			return linuxSourceScriptInvocation{}, e.looksLikeSourceScript(command), fmt.Errorf("declared source script command contains shell operator %q", token.value)
		}
		words = append(words, token.value)
	}
	if len(words) == 0 {
		return linuxSourceScriptInvocation{}, false, nil
	}
	environment := map[string]string{}
	programIndex := 0
	if words[0] == "env" {
		programIndex = 1
		for programIndex < len(words) {
			name, value, assignment := strings.Cut(words[programIndex], "=")
			if !assignment {
				break
			}
			if !validKbuildCommandEnvironmentName(name) || strings.ContainsRune(value, 0) {
				return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script has invalid environment assignment %q", words[programIndex])
			}
			if _, exists := environment[name]; exists {
				return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script repeats environment %s", name)
			}
			environment[name] = value
			programIndex++
		}
	}
	if programIndex >= len(words) {
		return linuxSourceScriptInvocation{}, words[0] == "env", fmt.Errorf("declared source script command has no program")
	}
	program, _, err := e.sourceScriptShellWord(words[programIndex])
	if err != nil {
		return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script program: %w", err)
	}
	relative, recognized, err := e.sourceScriptRelativePath(program)
	if err != nil || !recognized {
		return linuxSourceScriptInvocation{}, recognized, err
	}
	arguments := slices.Clone(words[programIndex+1:])
	toolArguments := make([]bool, len(arguments))
	auxiliarySet := map[string]bool{}
	rewriteTool := func(value string, selectable bool) (string, bool, error) {
		if !selectable {
			return value, false, nil
		}
		role, selected, err := e.configuredSourceScriptToolRole(value)
		if err != nil {
			return "", false, err
		}
		if !selected {
			return value, false, nil
		}
		auxiliarySet[role] = true
		return role, true, nil
	}
	for index, argument := range arguments {
		selectable := false
		arguments[index], selectable, err = e.sourceScriptShellWord(argument)
		if err != nil {
			return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script argument %d: %w", index, err)
		}
		if linuxProbeSymbolPattern.MatchString(argument) {
			continue
		}
		arguments[index], toolArguments[index], err = rewriteTool(arguments[index], selectable)
		if err != nil {
			return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script argument %d: %w", index, err)
		}
	}
	for name, value := range environment {
		selectable := false
		// These NAME=VALUE words follow the `env` program and are ordinary
		// argv, not shell assignment prefixes. Exact active expansion is thus
		// subject to field splitting and empty removal just like script argv;
		// quote provenance is gone, so reject ambiguous cardinality.
		value, selectable, err = e.sourceScriptShellWord(value)
		if err != nil {
			return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script environment %s: %w", name, err)
		}
		environment[name], _, err = rewriteTool(value, selectable)
		if err != nil {
			return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script environment %s: %w", name, err)
		}
	}
	if err := e.inheritSourceScriptEnvironment(environment, auxiliarySet); err != nil {
		return linuxSourceScriptInvocation{}, true, err
	}
	auxiliary := make([]string, 0, len(auxiliarySet))
	for role := range auxiliarySet {
		auxiliary = append(auxiliary, role)
	}
	slices.Sort(auxiliary)
	// Tool path arguments are proxy names rather than compiler candidates. A
	// source script may also receive one bare command name as its leading
	// argument. That name can only resolve in scriptrun's private PATH, which
	// contains the checksum-pinned multicall applets and declared tool proxies;
	// it never searches the ambient host. Keep this structural allowance at
	// argument zero and leave every other source word owned by the execution-
	// time link-driver candidate policy.
	leadingCommand := false
	if len(arguments) != 0 && !toolArguments[0] {
		leadingCommand, err = e.sourceScriptLeadingCommandArgument(arguments[0])
		if err != nil {
			return linuxSourceScriptInvocation{}, true, fmt.Errorf("declared source script argument 0: %w", err)
		}
	}
	candidateArguments := make([]bool, len(arguments))
	for index := range arguments {
		candidateArguments[index] = !toolArguments[index] && (index != 0 || !leadingCommand)
	}
	lowerer := newProbeSymbolicValueLowerer(e)
	arguments, conditional, argumentFragments, candidate, err := lowerProbeCandidateArguments(
		lowerer, arguments, candidateArguments, ProbeCandidatePolicyCCLink,
	)
	if err != nil {
		return linuxSourceScriptInvocation{}, true, err
	}
	environmentFragments, sourceRoots, err := lowerSourceScriptEnvironment(environment, lowerer)
	if err != nil {
		return linuxSourceScriptInvocation{}, true, err
	}
	return linuxSourceScriptInvocation{
		path: relative, arguments: arguments, environment: environment,
		environmentFragments: environmentFragments,
		auxiliary:            auxiliary, sourceRoots: sourceRoots,
		dependencies: slices.Clone(lowerer.dependencies), conditional: conditional,
		argumentFragments: argumentFragments, candidate: candidate,
	}, true, nil
}

// inheritSourceScriptEnvironment installs the exact source-exported process
// environment used by both source-script probe forms. Configured executable
// spellings become private proxy names, while the selected Rust source tree is
// represented by a typed source-root binding. The caller owns environment and
// auxiliarySet, which may already contain inline values and explicit tools.
func (e *LinuxProbeEvaluator) inheritSourceScriptEnvironment(
	environment map[string]string,
	auxiliarySet map[string]bool,
) error {
	names := make([]string, 0, len(e.scriptEnvironment))
	for name := range e.scriptEnvironment {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if _, exists := environment[name]; exists {
			continue
		}
		value := e.scriptEnvironment[name]
		if err := validateSourceScriptProtocolLiteral(value); err != nil {
			return fmt.Errorf("declared source script inherited environment %s: %w", name, err)
		}
		if e.rustSourceRoot != "" && value == e.rustSourceRoot {
			environment[name] = "${source_root:" + rustProbeSourceRootName + "}"
			continue
		}
		role, selected, err := e.configuredSourceScriptToolRole(value)
		if err != nil {
			return fmt.Errorf("declared source script inherited environment %s: %w", name, err)
		}
		if selected {
			auxiliarySet[role] = true
			environment[name] = role
			continue
		}
		environment[name] = value
	}
	return nil
}

func lowerSourceScriptEnvironment(
	environment map[string]string,
	lowerer *probeSymbolicValueLowerer,
) ([]ProbeEnvironmentFragments, []string, error) {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	slices.Sort(names)
	fragments := make([]ProbeEnvironmentFragments, 0, len(names))
	for _, name := range names {
		valueFragments, symbolic, err := lowerer.value(environment[name])
		if err != nil {
			return nil, nil, fmt.Errorf("declared source script environment %s: %w", name, err)
		}
		if !symbolic {
			continue
		}
		fragments = append(fragments, ProbeEnvironmentFragments{Name: name, Fragments: valueFragments})
		delete(environment, name)
	}
	sourceRoots := []string{linuxProbeSourceRootName}
	for _, value := range environment {
		if value == "${source_root:"+rustProbeSourceRootName+"}" {
			sourceRoots = append(sourceRoots, rustProbeSourceRootName)
			break
		}
	}
	return fragments, sourceRoots, nil
}

// sourceScriptLeadingCommandArgument reports whether one source-expanded
// argument is a bounded command candidate. Finite boolean/selection values are
// valid when each branch is either empty or exactly one safe bare name,
// preserving the same condition in ProbeStep.ConditionalArguments. Arbitrary
// text and exact opaque Make expressions are not command names. A flag-like
// branch continues through the link-driver validator.
func (e *LinuxProbeEvaluator) sourceScriptLeadingCommandArgument(argument string) (bool, error) {
	symbol, symbolic, err := e.symbolArgument(argument)
	if err != nil {
		return false, err
	}
	branches := []string{argument}
	if symbolic {
		if symbol.kind == "text" || symbol.kind == "transformed-text" || symbol.kind == "make-text" {
			return false, nil
		}
		branches = []string{symbol.trueText, symbol.falseText}
		if symbol.kind == "selection" {
			branches = symbol.selectionValues
		}
	}
	nonempty := false
	for _, branch := range branches {
		fields := strings.Fields(branch)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 1 || !safeLinuxSourceScriptCommandName(fields[0]) {
			return false, nil
		}
		nonempty = true
	}
	return nonempty, nil
}

func safeLinuxSourceScriptCommandName(value string) bool {
	return !strings.HasPrefix(value, "-") &&
		!strings.ContainsAny(value, `/\\`) &&
		safeLinuxProbePathComponent(value)
}

// sourceScriptShellWord resolves only an exact active shell parameter through
// the declared source-script environment. Composite/modifier forms remain an
// owned error: scriptrun receives argv directly and would otherwise see raw
// dollar bytes rather than the shell expansion modeled by the source command.
// Dollars protected by single quotes or a backslash carry the lexer sentinel;
// restore those only after active expansion has been ruled out, and prevent a
// resulting literal from selecting a configured tool proxy.
func (e *LinuxProbeEvaluator) sourceScriptShellWord(value string) (string, bool, error) {
	// The shared recipe lexer does not retain quote provenance for backticks.
	// Reject them all rather than treating active command substitution as
	// literal argv; quoted/escaped backticks can be supported only with an
	// explicit provenance marker analogous to literal dollars.
	if strings.ContainsRune(value, '`') {
		return "", false, fmt.Errorf("unsupported shell command substitution in %q", value)
	}
	literalDollar := strings.Contains(value, compactKbuildLiteralDollarToken)
	if strings.ContainsRune(value, '$') {
		if literalDollar {
			return "", false, fmt.Errorf("mixed active and literal dollar expansion in %q", value)
		}
		name, exact := linuxProbeShellEnvironmentToken(value)
		if !exact {
			return "", false, fmt.Errorf("unsupported active shell parameter in %q", value)
		}
		expanded, exists := e.scriptEnvironment[name]
		if !exists {
			return "", false, fmt.Errorf("undeclared shell environment parameter %s", name)
		}
		if strings.Contains(expanded, compactKbuildLiteralDollarToken) {
			return "", false, fmt.Errorf("shell environment parameter %s contains a reserved lexer token", name)
		}
		if err := validateSourceScriptProtocolLiteral(expanded); err != nil {
			return "", false, fmt.Errorf("shell environment parameter %s: %w", name, err)
		}
		// The lexer no longer tells us whether an exact parameter was quoted.
		// Unquoted shell expansion would split whitespace and remove an empty
		// value, while double quotes would preserve one argv. Reject both
		// ambiguous shapes rather than silently choosing different argv.
		if expanded == "" || strings.ContainsAny(expanded, " \t\r\n") {
			return "", false, fmt.Errorf("shell environment parameter %s does not expand to exactly one argv word", name)
		}
		return expanded, true, nil
	}
	if literalDollar {
		restored := strings.ReplaceAll(value, compactKbuildLiteralDollarToken, "$")
		if err := validateSourceScriptProtocolLiteral(restored); err != nil {
			return "", false, err
		}
		return restored, false, nil
	}
	if err := validateSourceScriptProtocolLiteral(value); err != nil {
		return "", false, err
	}
	return value, true, nil
}

func validateSourceScriptProtocolLiteral(value string) error {
	if strings.Contains(value, compactKbuildLiteralDollarToken) {
		return fmt.Errorf("value contains a reserved lexer token")
	}
	// Probe placeholders are interpreted after lowering. Source-provided
	// literal bytes must never acquire tool, source, scratch, or result
	// capabilities by colliding with that internal syntax. Reject every ${
	// form; the planner currently has no protocol-level literal escape.
	if strings.Contains(value, "${") {
		return fmt.Errorf("source literal contains reserved probe placeholder syntax")
	}
	return nil
}

func (e *LinuxProbeEvaluator) sourceScriptRelativePath(program string) (string, bool, error) {
	if e.sourceRoot == "" {
		return "", false, nil
	}
	root := filepath.Clean(e.sourceRoot)
	candidate := filepath.Clean(program)
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", false, nil
	}
	relative = filepath.ToSlash(relative)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", false, nil
	}
	if !strings.HasSuffix(relative, ".sh") && !compactKbuildSourceFileUsesShell(filepath.Join(root, filepath.FromSlash(relative))) {
		return "", false, nil
	}
	if err := validateProbeSourcePath(relative); err != nil {
		return "", true, err
	}
	return relative, true, nil
}

func (e *LinuxProbeEvaluator) looksLikeSourceScript(command string) bool {
	return e.sourceRoot != "" && strings.Contains(filepath.ToSlash(command), filepath.ToSlash(e.sourceRoot)+"/")
}

func (e *LinuxProbeEvaluator) configuredToolRole(value string) (string, bool, error) {
	selected := ""
	for role, token := range e.tools {
		if token == "" || !e.isToolToken(value, role) {
			continue
		}
		if selected != "" && selected != role {
			return "", true, fmt.Errorf("configured tool token %q is ambiguous between roles %s and %s", value, selected, role)
		}
		selected = role
	}
	return selected, selected != "", nil
}

// configuredSourceScriptToolRole matches an already-lexed and, when needed,
// exactly expanded source-script word. Do not call isToolToken here: it would
// perform a second shell-environment expansion even though shell parameter
// results are not recursively expanded.
func (e *LinuxProbeEvaluator) configuredSourceScriptToolRole(value string) (string, bool, error) {
	selected := ""
	for role, token := range e.tools {
		if token == "" || value != token && value != KbuildActionRoleToken(e.scope, role) && value != KbuildActionRoleToken(KbuildActionRoleAutoScope, role) {
			continue
		}
		if selected != "" && selected != role {
			return "", true, fmt.Errorf("configured tool token %q is ambiguous between roles %s and %s", value, selected, role)
		}
		selected = role
	}
	return selected, selected != "", nil
}
