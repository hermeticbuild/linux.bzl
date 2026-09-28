package kconfig

// This file lowers an invocation of a declared Linux source script into the
// generic probe protocol. It intentionally contains no script-name switch and
// no reconstruction of script behavior: the selected source file, registered
// script runtime, and configured tool action contracts determine the result.

import (
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path"
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

	linuxProbeEvaluatedScriptSafePrefix   = "linux-bzl-evaluated-script-output-v1\n"
	linuxProbeEvaluatedScriptUnsafeOutput = "linux-bzl-evaluated-script-side-effects-v1\n"
	// proberun bounds stdout to 64 KiB. Base64 may insert line wrapping, so a
	// 45-KiB source payload leaves deterministic room for the envelope and
	// wrapping while making every larger result an explicit unsafe fallback.
	linuxProbeEvaluatedScriptMaxOutputBytes = 45 << 10
	linuxProbeEvaluatedScriptTimeoutSeconds = 10
	linuxProbeEvaluatedScriptWorkScratch    = "working-tree"
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

// EvaluatedScriptOutputText executes one already-expanded, source-selected
// shell recipe whose only statically named output is target. Unlike
// SourceScriptOutputText, the recipe itself need not be a checked-in script:
// its exact text, complete source-exported environment, selected runtime, and
// Linux source root form the content-addressed probe request.
//
// The probe runs below a private working directory, stages declared source
// prerequisites exactly as the final mapped action does, and verifies that no
// other path survives. Discovery reports recognized with concrete=false.
// Replay reports recognized=false when execution found a side output, allowing
// the caller to retain the ordinary conservative Kbuild action instead of
// erasing an effect it cannot reproduce.
func (s *KbuildProbeScopes) EvaluatedScriptOutputText(
	scope string,
	target string,
	recipe string,
	sourcePrerequisites []string,
	workingTreeContents map[string]string,
) (text string, concrete, recognized bool, err error) {
	if s == nil {
		return "", false, false, fmt.Errorf("Kbuild probe scopes are nil")
	}
	evaluator := s.evaluators[scope]
	if evaluator == nil {
		return "", false, false, fmt.Errorf("Kbuild probe workload has no %s scope", scope)
	}
	request, dependencies, recognized, err := evaluator.evaluatedScriptOutputTextRequest(
		target, recipe, sourcePrerequisites, workingTreeContents,
	)
	if err != nil || !recognized {
		return "", false, recognized, err
	}
	request = evaluator.canonicalSourceRequest(request)
	reference, err := evaluator.discovery.Request(scope, request, dependencies...)
	if err != nil {
		return "", false, true, err
	}
	if err := evaluator.symbolRegistry.publishDefinition(reference, request, dependencies); err != nil {
		return "", false, true, err
	}
	if !evaluator.seen[reference.NodeID] {
		evaluator.seen[reference.NodeID] = true
		evaluator.references = append(evaluator.references, reference)
	}
	if evaluator.oracle == nil {
		return "", false, true, nil
	}
	envelope, err := evaluator.readText(reference, request, dependencies...)
	if err != nil {
		return "", false, true, err
	}
	if envelope == linuxProbeEvaluatedScriptUnsafeOutput {
		return "", true, false, nil
	}
	if !strings.HasPrefix(envelope, linuxProbeEvaluatedScriptSafePrefix) {
		return "", false, true, fmt.Errorf("evaluated-script output probe returned an invalid result envelope")
	}
	encoded := strings.TrimSpace(strings.TrimPrefix(envelope, linuxProbeEvaluatedScriptSafePrefix))
	contents, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false, true, fmt.Errorf("decode evaluated-script output probe result: %w", err)
	}
	return string(contents), true, true, nil
}

// EvaluatedScriptOutputTextAcrossScopes resolves generated bytes only when
// every compiler scope available to this workload observes the same result.
// Generated-header discovery runs before the selected Kbuild graph has enough
// information to classify a recipe as target- or host-scoped. Requiring
// consensus here makes the result valid under either eventual classification;
// a scope-sensitive recipe remains an ordinary conservative Kbuild action.
// A target-only workload has only one possible authority and therefore needs
// only its target observation.
func (s *KbuildProbeScopes) EvaluatedScriptOutputTextAcrossScopes(
	target string,
	recipe string,
	sourcePrerequisites []string,
	workingTreeContents map[string]string,
) (text string, concrete, recognized bool, err error) {
	if s == nil {
		return "", false, false, fmt.Errorf("Kbuild probe scopes are nil")
	}
	available := make([]string, 0, 2)
	for _, scope := range []string{"target", "host"} {
		if s.evaluators[scope] != nil {
			available = append(available, scope)
		}
	}
	if len(available) == 0 {
		return "", false, false, fmt.Errorf("Kbuild probe workload has no compiler scope")
	}

	values := make([]string, 0, len(available))
	allConcrete := true
	allRecognized := true
	for _, scope := range available {
		value, scopeConcrete, scopeRecognized, scopeErr := s.EvaluatedScriptOutputText(
			scope, target, recipe, sourcePrerequisites, workingTreeContents,
		)
		if scopeErr != nil {
			return "", false, scopeRecognized, scopeErr
		}
		allConcrete = allConcrete && scopeConcrete
		allRecognized = allRecognized && scopeRecognized
		values = append(values, value)
	}
	if !allRecognized {
		return "", allConcrete, false, nil
	}
	if !allConcrete {
		return "", false, true, nil
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", true, false, nil
		}
	}
	return values[0], true, true, nil
}

func shellSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// compactKbuildEvaluatedScriptOutputRecipe recognizes syntax, not utilities:
// no filename, compiler family, command name, or Kconfig symbol participates.
// The exact recipe must have one static overwrite redirection to target, only
// bounded list/pipeline operators, bare runtime command heads, and no active
// shell expansion other than the planner-owned immutable source-tree marker.
func compactKbuildEvaluatedScriptOutputRecipe(
	recipe, target string,
	sourcePrerequisites []string,
) (string, bool) {
	recipe, ok := compactKbuildGeneratedTextRecipe(recipe)
	if !ok || len(recipe) > 1<<20 || strings.ContainsRune(recipe, 0) ||
		compactKbuildRecipeHasShellComment(recipe) || !compactKbuildGeneratedTextRedirectsTarget(recipe, target) {
		return "", false
	}
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return "", false
	}
	declaredSourceOperands := make(map[string]bool, len(sourcePrerequisites))
	for _, source := range sourcePrerequisites {
		source = canonicalKbuildRulePath(source)
		if source == "" || validateProbeSourcePath(source) != nil {
			return "", false
		}
		// The final mapped action stages every source prerequisite as a file in
		// its private working directory. A source cannot simultaneously be the
		// output or one of its directory ancestors (or descendants): those
		// layouts cannot be represented by the same regular-file topology.
		if source == target || strings.HasPrefix(source, target+"/") || strings.HasPrefix(target, source+"/") {
			return "", false
		}
		declaredSourceOperands["${tree:kernel}/"+source] = true
	}
	protectedOutputPaths := map[string]bool{}
	for candidate := target; candidate != "." && candidate != ""; candidate = path.Dir(candidate) {
		protectedOutputPaths[candidate] = true
	}
	withoutSourceTree := strings.ReplaceAll(recipe, "${tree:kernel}", "")
	if strings.ContainsAny(withoutSourceTree, "$`") ||
		compactKbuildContainsPrivateActionMarker(recipe) ||
		compactKbuildContainsPrivateProvenanceByte(recipe) ||
		compactKbuildContainsPrivateToolsetPathByte(recipe) {
		return "", false
	}
	roles, err := KbuildActionRoleRefs(recipe)
	if err != nil || len(roles) != 0 {
		return "", false
	}
	tokens, err := lexCompactKbuildRecipe(recipe)
	if err != nil {
		return "", false
	}
	redirects := 0
	for index, token := range tokens {
		if token.pathnameExpansion || token.shellExpansion &&
			strings.ContainsAny(strings.ReplaceAll(recipe[token.start:token.end], "${tree:kernel}", ""), "$`") {
			return "", false
		}
		if !token.operator {
			continue
		}
		switch token.value {
		case ";", "|":
			continue
		case ">":
			if index+1 >= len(tokens) || tokens[index+1].operator {
				return "", false
			}
			redirect, ok := compactKbuildGeneratedTextTokenPath(tokens[index+1])
			if !ok || canonicalKbuildRulePath(redirect) != target {
				return "", false
			}
			redirects++
		default:
			return "", false
		}
	}
	if redirects != 1 {
		return "", false
	}
	commands, err := compactKbuildCompoundProgramCommands(recipe)
	if err != nil || len(commands) == 0 {
		return "", false
	}
	for _, command := range commands {
		if command.program == "" || path.Base(command.program) != command.program ||
			len(command.environment) != 0 ||
			command.programPathnameExpansion || command.environmentPathnameExpansion ||
			command.stdinPathnameExpansion || command.stdoutPathnameExpansion || command.stdin != "" {
			return "", false
		}
		// Exact removals are observable state even when the removed path is
		// initially absent, so a post-execution directory scan cannot prove them
		// away. Reuse the same bounded removal grammar as final action lowering and
		// leave every such recipe conservative.
		if path.Base(command.program) == "rm" {
			return "", false
		}
		// The replacement actionfile always publishes a fresh non-executable
		// regular file. Reject any recipe which can name its output to a command
		// after the shell opens the one permitted stdout redirection: such a
		// command could chmod, rewrite, rename, or read the result through argv or
		// stdin without creating a separately observable side path.
		for _, value := range command.arguments {
			if declaredSourceOperands[value] {
				continue
			}
			// Non-source operands are deliberately limited to one bounded literal
			// word. This excludes nested command languages, implicit absolute-path
			// forms such as if=/proc, relative source paths which are not staged in
			// the private probe cwd, and malformed tree-marker concatenations.
			if strings.Contains(value, "${tree:kernel}") || !safeLinuxProbePathComponent(value) {
				return "", false
			}
			if protectedOutputPaths[canonicalKbuildRulePath(value)] {
				return "", false
			}
		}
	}
	if !safeLinuxEvaluatedOutputProgram(commands, declaredSourceOperands) {
		return "", false
	}
	return recipe, true
}

// safeLinuxEvaluatedOutputProgram is deliberately an argv-level semantic
// grammar, not an executable-name allowlist. Utilities such as awk and sed can
// execute source-owned programs which inspect the process or host even when
// their command line names only declared source files; test predicates can
// likewise observe staging timestamps and modes. Those recipes must remain
// ordinary Kbuild actions.
//
// The one currently proven program is Linux's time-constant shape: one literal
// decimal line is piped to bc, which evaluates one immutable declared program.
// The selected runtime, exact program bytes, stdin, argv, and environment are
// all request inputs. bc has no filesystem or process-inspection primitive, so
// this bounded shape can only derive output bytes from those declared inputs.
func safeLinuxEvaluatedOutputProgram(
	commands []compactKbuildRecipeCommand,
	declaredSourceOperands map[string]bool,
) bool {
	if len(commands) != 2 || len(declaredSourceOperands) != 1 {
		return false
	}
	emit, evaluate := commands[0], commands[1]
	if emit.program != "echo" || emit.connector != "|" ||
		len(emit.arguments) != 1 || !compactKbuildGeneratedTextDigits(emit.arguments[0]) ||
		emit.stdin != "" || emit.stdout != "" {
		return false
	}
	if evaluate.program != "bc" || (evaluate.connector != ";" && evaluate.connector != "") ||
		len(evaluate.arguments) != 2 || evaluate.arguments[0] != "-q" ||
		!declaredSourceOperands[evaluate.arguments[1]] ||
		evaluate.stdin != "" || evaluate.stdout != "" {
		return false
	}
	return true
}

func (e *LinuxProbeEvaluator) evaluatedScriptOutputTextRequest(
	target string,
	recipe string,
	sourcePrerequisites []string,
	workingTreeContents map[string]string,
) (ProbeRequest, []ProbeReference, bool, error) {
	if e == nil {
		return ProbeRequest{}, nil, false, fmt.Errorf("Linux evaluated-script probe evaluator is nil")
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return ProbeRequest{}, nil, false, nil
	}
	target = canonicalKbuildRulePath(target)
	recipe, recognized := compactKbuildEvaluatedScriptOutputRecipe(recipe, target, sourcePrerequisites)
	if !recognized {
		return ProbeRequest{}, nil, false, nil
	}
	// BusyBox bc treats BC_ENV_ARGS as additional program filenames. Those
	// pathname bytes are not represented by the selected rule's declared source
	// prerequisites, so even a fully content-addressed process environment would
	// leave the program frontier open. Keep that source-selected environment
	// shape on the ordinary Kbuild path.
	if e.scriptEnvironment["BC_ENV_ARGS"] != "" {
		return ProbeRequest{}, nil, false, nil
	}

	// This is an optional exactness proof. Exceeding a protocol bound must retain
	// the ordinary Kbuild action rather than turn an otherwise valid graph into a
	// planner error.
	if len(sourcePrerequisites) >= maxProbeSources {
		return ProbeRequest{}, nil, false, nil
	}
	sources := map[string]bool{linuxProbeRootAnchor: true}
	stagedSourceModes := map[string]string{}
	for _, candidate := range sourcePrerequisites {
		candidate = canonicalKbuildRulePath(candidate)
		candidatePath := filepath.Join(e.sourceRoot, filepath.FromSlash(candidate))
		info, statErr := os.Lstat(candidatePath)
		if statErr != nil || !info.Mode().IsRegular() {
			return ProbeRequest{}, nil, false, nil
		}
		candidate, err := e.immutableLinuxSourcePath(candidate, false)
		if err != nil {
			return ProbeRequest{}, nil, false, nil
		}
		sources[candidate] = true
		mode := "0644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "0755"
		}
		stagedSourceModes[candidate] = mode
	}
	stagedWorkingContents := map[string]string{}
	for candidate, content := range workingTreeContents {
		canonical := canonicalKbuildRulePath(candidate)
		if canonical == "" || canonical != candidate || validateProbeSourcePath(canonical) != nil ||
			len(content) > 1<<20 || strings.ContainsRune(content, 0) {
			return ProbeRequest{}, nil, false, nil
		}
		candidate = canonical
		if _, conflict := stagedSourceModes[candidate]; conflict ||
			candidate == target || strings.HasPrefix(candidate, target+"/") || strings.HasPrefix(target, candidate+"/") {
			return ProbeRequest{}, nil, false, nil
		}
		for source := range stagedSourceModes {
			if strings.HasPrefix(source, candidate+"/") || strings.HasPrefix(candidate, source+"/") {
				return ProbeRequest{}, nil, false, nil
			}
		}
		stagedWorkingContents[candidate] = content
	}
	for candidate := range stagedWorkingContents {
		for other := range stagedWorkingContents {
			if candidate != other && strings.HasPrefix(candidate, other+"/") {
				return ProbeRequest{}, nil, false, nil
			}
		}
	}

	environment := map[string]string{}
	auxiliarySet := map[string]bool{}
	if err := e.inheritSourceScriptEnvironment(environment, auxiliarySet); err != nil {
		return ProbeRequest{}, nil, false, nil
	}
	lowerer := newProbeSymbolicValueLowerer(e)
	environmentFragments, sourceRoots, err := lowerSourceScriptEnvironment(environment, lowerer)
	if err != nil {
		return ProbeRequest{}, nil, false, nil
	}
	if !slices.Contains(sourceRoots, linuxProbeSourceRootName) {
		sourceRoots = append(sourceRoots, linuxProbeSourceRootName)
		slices.Sort(sourceRoots)
	}

	configured := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		configured = append(configured, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	applets, err := compactKbuildScriptRuntimeApplets(configured, e.scope)
	if err != nil {
		return ProbeRequest{}, nil, false, nil
	}
	toolRoles := make([]string, 0, len(auxiliarySet))
	for role := range auxiliarySet {
		if role != linuxProbeScriptRuntime {
			toolRoles = append(toolRoles, role)
		}
	}
	slices.Sort(toolRoles)
	auxiliary := append([]string{linuxProbeScriptRuntime}, toolRoles...)
	for _, applet := range applets {
		auxiliary = append(auxiliary, applet.role)
	}
	slices.Sort(auxiliary)
	auxiliary = slices.Compact(auxiliary)
	baseArguments := func() []string {
		arguments := []string{
			"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
			"-interpreter_arg", "sh",
			"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
			"-tool", linuxProbeScriptRuntime + "=${tool:" + linuxProbeScriptRuntime + "}",
		}
		for _, applet := range applets {
			arguments = append(arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
		}
		for _, role := range toolRoles {
			arguments = append(arguments, "-tool", role+"=${tool:"+role+"}")
		}
		return arguments
	}

	workingScratchNames := map[string]string{}
	scratch := make([]ProbeScratch, 0, len(stagedWorkingContents)+1)
	for index, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
		name := fmt.Sprintf("working-input-%04d", index)
		workingScratchNames[candidate] = name
		scratch = append(scratch, ProbeScratch{
			Name: name, Kind: "file", Content: stagedWorkingContents[candidate], Present: true,
			ContentIsOpaque: true,
		})
	}
	scratch = append(scratch, ProbeScratch{Name: linuxProbeEvaluatedScriptWorkScratch, Kind: "directory"})
	slices.SortFunc(scratch, func(left, right ProbeScratch) int {
		return strings.Compare(left.Name, right.Name)
	})

	workingDirectories := map[string]bool{}
	for candidate := range stagedSourceModes {
		for parent := path.Dir(candidate); parent != "."; parent = path.Dir(parent) {
			workingDirectories[parent] = true
		}
	}
	for candidate := range stagedWorkingContents {
		for parent := path.Dir(candidate); parent != "."; parent = path.Dir(parent) {
			workingDirectories[parent] = true
		}
	}
	for parent := path.Dir(target); parent != "."; parent = path.Dir(parent) {
		workingDirectories[parent] = true
	}

	// Setup and validation run as separate processes around the measured recipe.
	// The recipe itself therefore enters scriptrun with the same top-level shell
	// state, argv, environment, and private cwd as the final Kbuild action.
	var setup strings.Builder
	setup.WriteString("#!/bin/sh\nset -e\numask 022\n")
	for _, directory := range slices.Sorted(maps.Keys(workingDirectories)) {
		setup.WriteString("mkdir -p -- ")
		setup.WriteString(shellSingleQuoted(directory))
		setup.WriteString("\nchmod 0755 -- ")
		setup.WriteString(shellSingleQuoted(directory))
		setup.WriteByte('\n')
	}
	for _, source := range slices.Sorted(maps.Keys(stagedSourceModes)) {
		setup.WriteString("cp -- ")
		setup.WriteString(shellSingleQuoted("${tree:kernel}/" + source))
		setup.WriteByte(' ')
		setup.WriteString(shellSingleQuoted(source))
		setup.WriteString("\nchmod ")
		setup.WriteString(stagedSourceModes[source])
		setup.WriteString(" -- ")
		setup.WriteString(shellSingleQuoted(source))
		setup.WriteByte('\n')
	}
	for index, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
		setup.WriteString("cp -- ")
		setup.WriteString(fmt.Sprintf("\"${%d}\"", index+1))
		setup.WriteByte(' ')
		setup.WriteString(shellSingleQuoted(candidate))
		setup.WriteString("\nchmod 0644 -- ")
		setup.WriteString(shellSingleQuoted(candidate))
		setup.WriteByte('\n')
	}
	setupArguments := baseArguments()
	for _, helper := range []string{"chmod", "cp", "mkdir", "sh"} {
		setupArguments = append(setupArguments, "-require_applet", helper)
	}
	setupArguments = append(setupArguments,
		"-tree", "kernel=${source_root:"+linuxProbeSourceRootName+"}",
		"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(setup.String())),
	)
	if len(stagedWorkingContents) != 0 {
		setupArguments = append(setupArguments, "--")
		for _, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
			setupArguments = append(setupArguments, "${scratch:"+workingScratchNames[candidate]+"}")
		}
	}

	declaredSourceInputs := make([]compactKbuildRuleInput, 0, len(stagedSourceModes))
	for _, source := range slices.Sorted(maps.Keys(stagedSourceModes)) {
		declaredSourceInputs = append(declaredSourceInputs, compactKbuildRuleInput{path: source, sourceID: source})
	}
	measuredRecipe, err := rewriteCompactKbuildScriptDeclaredTreeInputs(recipe, declaredSourceInputs)
	if err != nil {
		return ProbeRequest{}, nil, false, nil
	}
	measuredRecipe = "#!/bin/sh\nset -e\n" + measuredRecipe + "\n"
	recipeArguments := baseArguments()
	for _, tree := range []string{"kernel"} {
		if strings.Contains(measuredRecipe, "${tree:"+tree+"}") {
			recipeArguments = append(recipeArguments, "-tree", tree+"=${source_root:"+linuxProbeSourceRootName+"}")
		}
	}
	recipeArguments = append(recipeArguments,
		"-timeout_seconds", fmt.Sprintf("%d", linuxProbeEvaluatedScriptTimeoutSeconds),
		"-fallback_stdout_base64", base64.StdEncoding.EncodeToString([]byte(linuxProbeEvaluatedScriptUnsafeOutput)),
		"-max_file_size_bytes", fmt.Sprintf("%d", linuxProbeEvaluatedScriptMaxOutputBytes+4096),
		"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(measuredRecipe)),
	)

	quotedTarget := shellSingleQuoted(target)
	var validation strings.Builder
	validation.WriteString("#!/bin/sh\nset -e\n")
	validation.WriteString("unsafe() { printf '%s\\n' ")
	validation.WriteString(shellSingleQuoted(strings.TrimSuffix(linuxProbeEvaluatedScriptUnsafeOutput, "\n")))
	validation.WriteString("; exit 0; }\n")
	validation.WriteString("work_root=$PWD\nsource_root=${tree:kernel}\n")
	validation.WriteString("if [ \"${")
	validation.WriteString(fmt.Sprintf("%d", len(stagedWorkingContents)+1))
	validation.WriteString("-}\" != recipe-ok ]; then unsafe; fi\n")
	validation.WriteString("if [ ! -f ")
	validation.WriteString(quotedTarget)
	validation.WriteString(" ] || [ -L ")
	validation.WriteString(quotedTarget)
	validation.WriteString(" ]; then unsafe; fi\n")
	allowedPaths := map[string]bool{}
	orderedAllowedPaths := []string{}
	appendAllowedPath := func(candidate string) {
		if !allowedPaths[candidate] {
			allowedPaths[candidate] = true
			orderedAllowedPaths = append(orderedAllowedPaths, candidate)
		}
	}
	appendAllowedPath(target)
	for parent := path.Dir(target); parent != "."; parent = path.Dir(parent) {
		appendAllowedPath(parent)
	}
	for _, source := range slices.Sorted(maps.Keys(stagedSourceModes)) {
		appendAllowedPath(source)
		for parent := path.Dir(source); parent != "."; parent = path.Dir(parent) {
			appendAllowedPath(parent)
		}
	}
	for _, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
		appendAllowedPath(candidate)
		for parent := path.Dir(candidate); parent != "."; parent = path.Dir(parent) {
			appendAllowedPath(parent)
		}
	}
	validation.WriteString("extra=\"$(find . -mindepth 1")
	for _, allowed := range orderedAllowedPaths {
		validation.WriteString(" ! -path ")
		validation.WriteString(shellSingleQuoted("./" + allowed))
	}
	validation.WriteString(" -print -quit)\"\n")
	validation.WriteString("if [ -n \"$extra\" ]; then unsafe; fi\n")
	for _, source := range slices.Sorted(maps.Keys(stagedSourceModes)) {
		quotedSource := shellSingleQuoted(source)
		validation.WriteString("if [ ! -f ")
		validation.WriteString(quotedSource)
		validation.WriteString(" ] || [ -L ")
		validation.WriteString(quotedSource)
		validation.WriteString(" ]; then unsafe; fi\n")
		validation.WriteString("bad_source_mode=\"$(find ")
		validation.WriteString(quotedSource)
		validation.WriteString(" -prune ! -perm ")
		validation.WriteString(stagedSourceModes[source])
		validation.WriteString(" -print -quit)\"\nif [ -n \"$bad_source_mode\" ]; then unsafe; fi\n")
		validation.WriteString("cmp -s ")
		validation.WriteString(quotedSource)
		validation.WriteByte(' ')
		validation.WriteString(shellSingleQuoted("${tree:kernel}/" + source))
		validation.WriteString(" || unsafe\n")
	}
	for index, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
		quotedCandidate := shellSingleQuoted(candidate)
		validation.WriteString("if [ ! -f ")
		validation.WriteString(quotedCandidate)
		validation.WriteString(" ] || [ -L ")
		validation.WriteString(quotedCandidate)
		validation.WriteString(" ]; then unsafe; fi\n")
		validation.WriteString("bad_working_mode=\"$(find ")
		validation.WriteString(quotedCandidate)
		validation.WriteString(" -prune ! -perm 0644 -print -quit)\"\nif [ -n \"$bad_working_mode\" ]; then unsafe; fi\n")
		validation.WriteString("cmp -s ")
		validation.WriteString(quotedCandidate)
		validation.WriteByte(' ')
		validation.WriteString(fmt.Sprintf("\"${%d}\"", index+1))
		validation.WriteString(" || unsafe\n")
	}
	// actionfile writes literal content with mode 0644. Confirm the measured
	// recipe produced the same mode, including when shell builtins such as
	// umask changed creation permissions without naming target in argv.
	validation.WriteString("bad_mode=\"$(find ")
	validation.WriteString(quotedTarget)
	validation.WriteString(" -prune ! -perm 0644 -print -quit)\"\n")
	validation.WriteString("if [ -n \"$bad_mode\" ]; then unsafe; fi\n")
	validation.WriteString("size=\"$(wc -c < ")
	validation.WriteString(quotedTarget)
	validation.WriteString(")\" || unsafe\n")
	validation.WriteString("case \"$size\" in ''|*[!0-9]*) unsafe;; esac\n")
	validation.WriteString("if [ \"$size\" -gt ")
	validation.WriteString(fmt.Sprintf("%d", linuxProbeEvaluatedScriptMaxOutputBytes))
	validation.WriteString(" ]; then unsafe; fi\n")
	// Physical roots belong to the measurement action, not the final action's
	// declared inputs. Reject them, along with any other visibly absolute path,
	// before exact bytes can become a durable input-free action.
	validation.WriteString("contains_transient() { [ -n \"$1\" ] && LC_ALL=C grep -F -- \"$1\" ")
	validation.WriteString(quotedTarget)
	validation.WriteString(" >/dev/null 2>&1; }\n")
	validation.WriteString("for transient in \"$work_root\" \"$source_root\" \"${HOME-}\" \"${TMPDIR-}\"; do if contains_transient \"$transient\"; then unsafe; fi; done\n")
	validation.WriteString("old_ifs=$IFS; IFS=:; for transient in ${PATH-}; do if contains_transient \"$transient\"; then unsafe; fi; done; IFS=$old_ifs\n")
	validation.WriteString("if LC_ALL=C grep -E ")
	validation.WriteString(shellSingleQuoted(`(^|[[:space:]'"=(:,])/[[:alnum:]_.+-]`))
	validation.WriteByte(' ')
	validation.WriteString(quotedTarget)
	validation.WriteString(" >/dev/null 2>&1; then unsafe; fi\n")
	validation.WriteString("printf '%s\\n' ")
	validation.WriteString(shellSingleQuoted(strings.TrimSuffix(linuxProbeEvaluatedScriptSafePrefix, "\n")))
	validation.WriteString("\nbase64 ")
	validation.WriteString(quotedTarget)
	validation.WriteByte('\n')
	validationArguments := baseArguments()
	for _, helper := range []string{"base64", "cmp", "find", "grep", "sh", "wc"} {
		validationArguments = append(validationArguments, "-require_applet", helper)
	}
	validationArguments = append(validationArguments,
		"-tree", "kernel=${source_root:"+linuxProbeSourceRootName+"}",
		"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(validation.String())),
		"--",
	)
	for _, candidate := range slices.Sorted(maps.Keys(stagedWorkingContents)) {
		validationArguments = append(validationArguments, "${scratch:"+workingScratchNames[candidate]+"}")
	}
	recipeSucceeded := ProbePredicate{Operator: "all", Operands: []ProbePredicate{
		{Operator: "exit-zero", Step: "prepare-evaluated-script-output"},
		{Operator: "exit-zero", Step: "evaluated-script-output"},
		{Operator: "stream-empty", Step: "evaluated-script-output", Stream: "stdout"},
	}}
	validationConditional := ProbeConditionalArguments{
		Before: len(validationArguments), When: recipeSucceeded, Arguments: []string{"recipe-ok"},
	}

	declaredSources := make([]string, 0, len(sources))
	for source := range sources {
		declaredSources = append(declaredSources, source)
	}
	slices.Sort(declaredSources)
	setupStep := ProbeStep{
		Name: "prepare-evaluated-script-output", Tool: linuxProbeScriptRunner,
		AuxiliaryTools: auxiliary, WorkingDirectory: "${scratch:" + linuxProbeEvaluatedScriptWorkScratch + "}",
		Arguments: setupArguments, Environment: environment, EnvironmentFragments: environmentFragments,
		DiscardStdout: true,
	}
	recipeStep := ProbeStep{
		Name: "evaluated-script-output", Tool: linuxProbeScriptRunner,
		AuxiliaryTools: auxiliary, WorkingDirectory: "${scratch:" + linuxProbeEvaluatedScriptWorkScratch + "}",
		Arguments:   recipeArguments,
		Environment: environment, EnvironmentFragments: environmentFragments,
		DiscardStderr: true,
		When:          &ProbePredicate{Operator: "exit-zero", Step: setupStep.Name},
	}
	validationStep := ProbeStep{
		Name: "validate-evaluated-script-output", Tool: linuxProbeScriptRunner,
		AuxiliaryTools: auxiliary, WorkingDirectory: "${scratch:" + linuxProbeEvaluatedScriptWorkScratch + "}",
		Arguments: validationArguments, ConditionalArguments: []ProbeConditionalArguments{validationConditional},
		Environment: environment, EnvironmentFragments: environmentFragments,
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Sources: declaredSources, SourceRoots: sourceRoots,
		Scratch: scratch, Steps: []ProbeStep{setupStep, recipeStep, validationStep},
		Outcome: ProbeOutcome{Kind: "text", Step: validationStep.Name, Stream: "stdout", RequireSuccess: true},
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, nil, false, nil
	}
	return request, slices.Clone(lowerer.dependencies), true, nil
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
