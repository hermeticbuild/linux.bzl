package kconfig

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	compactKbuildScriptRunnerRole        = "scriptrun"
	compactKbuildScriptRuntimeRole       = "script-runtime"
	compactKbuildScriptAppletRolePrefix  = "script-applet-"
	compactKbuildOverwriteInputRole      = "overwrite"
	compactKbuildWorkingClosureInputRole = "working-closure"
)

type compactKbuildScriptRuntimeApplet struct {
	name string
	role string
}

// compactKbuildSourceInterpreter is immutable interpreter evidence read from a
// declared source script's shebang. The source chooses only a command name and
// its shebang arguments; final action lowering maps that name to a configured
// script-applet role instead of resolving an ambient executable.
type compactKbuildSourceInterpreter struct {
	program   string
	arguments []string
}

type compactKbuildSourceInterpreterCommandMatch struct {
	interpreter compactKbuildSourceInterpreter
	scriptPath  string
	scriptIndex int
}

// compactKbuildScriptRuntimeApplets derives the runtime's command overrides
// solely from the selected toolset role registry. The planner has no command
// compatibility table: a source command selects a matching applet name, while
// the registered toolchain selects its executable and action envelope.
func compactKbuildScriptRuntimeApplets(configured []KbuildActionRoleRef, expectedScope string) ([]compactKbuildScriptRuntimeApplet, error) {
	applets := []compactKbuildScriptRuntimeApplet{}
	seen := map[string]bool{}
	for _, ref := range configured {
		if ref.Scope != expectedScope || !strings.HasPrefix(ref.Role, compactKbuildScriptAppletRolePrefix) {
			continue
		}
		name := strings.TrimPrefix(ref.Role, compactKbuildScriptAppletRolePrefix)
		if !safeLinuxSourceScriptCommandName(name) {
			return nil, fmt.Errorf("%s script runtime has invalid applet role %q", expectedScope, ref.Role)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s script runtime repeats applet %q", expectedScope, name)
		}
		seen[name] = true
		applets = append(applets, compactKbuildScriptRuntimeApplet{name: name, role: ref.Role})
	}
	sort.Slice(applets, func(i, j int) bool { return applets[i].name < applets[j].name })
	return applets, nil
}

// compactKbuildSourceScriptInvocation is an evaluated interpreter invocation
// whose payload is a declared source file. The interpreter is deliberately
// represented by a tool role rather than by the source-selected command token:
// execution uses the registered hermetic runtime or matching script applet,
// never an ambient executable.
type compactKbuildSourceScriptInvocation struct {
	interpreterRole      string
	interpreter          string
	interpreterArguments []string
	scriptPath           string
	scriptArguments      []string
	observationArguments []string
	environment          map[string]string
	environmentUsage     compactKbuildSourceScriptEnvironmentUsage
	argumentUsage        compactKbuildSourceScriptEnvironmentUsage
	toolRoles            []string
}

func compactKbuildProjectedSourceScriptEnvironmentForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
) (map[string]string, []KbuildActionRoleRef, error) {
	values, err := evaluateCompactKbuildTargetEnvironmentForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true,
	)
	if err != nil {
		return nil, nil, err
	}
	return compactKbuildProjectedSourceScriptEnvironmentValues(profile, values, inline, usage)
}

// compactKbuildProjectedCapturedEnvironment resolves one source-ordered
// symbolic environment snapshot at action-plan time, then applies the same
// inline precedence, path canonicalization, and usage-based role filtering as
// an ordinary selected recipe. The profile is the query's evaluator snapshot;
// it must not be replaced with the synthetic content-output target context.
func compactKbuildProjectedCapturedEnvironment(
	profile CompactKbuildProfile,
	captured map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
) (map[string]string, []KbuildActionRoleRef, error) {
	values, err := compactKbuildResolvedCapturedEnvironment(profile, captured)
	if err != nil {
		return nil, nil, err
	}
	return compactKbuildProjectedSourceScriptEnvironmentValues(profile, values, inline, usage)
}

func compactKbuildResolvedCapturedEnvironment(
	profile CompactKbuildProfile,
	captured map[string]string,
) (map[string]string, error) {
	values := maps.Clone(captured)
	if values == nil {
		values = map[string]string{}
	}
	evaluator, err := compactKbuildProfileTargetEvaluator(profile, "")
	if err != nil {
		return nil, err
	}
	for name, value := range values {
		resolved, resolveErr := evaluator.template.resolveKbuildSymbolic(value)
		if resolveErr != nil {
			return nil, fmt.Errorf("captured Kbuild environment variable %s symbolic result: %w", name, resolveErr)
		}
		values[name] = resolved
	}
	return values, nil
}

func compactKbuildProjectedSourceScriptEnvironmentValues(
	profile CompactKbuildProfile,
	values map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
) (map[string]string, []KbuildActionRoleRef, error) {
	inlineNames := make(map[string]bool, len(inline))
	for name, value := range inline {
		values[name] = value
		inlineNames[name] = true
	}
	environment := make(map[string]string, len(values))
	refs := []KbuildActionRoleRef{}
	for _, name := range sortedStringMapKeys(values) {
		value := values[name]
		value = compactKbuildProfileCanonicalRecipeText(profile, value)
		if _, _, err := restoreCompactKbuildLiteralActionMarkers(value); err != nil {
			return nil, nil, fmt.Errorf("source-script effective variable %s literal marker: %w", name, err)
		}
		valueRefs, err := KbuildActionRoleRefs(value)
		if err != nil {
			return nil, nil, fmt.Errorf("source-script effective variable %s: %w", name, err)
		}
		if len(valueRefs) != 0 && !inlineNames[name] && !usage.uses(name) {
			continue
		}
		environment[name] = value
		refs = append(refs, valueRefs...)
	}
	return environment, canonicalKbuildActionRoleRefs(refs), nil
}

// compactKbuildSourceScriptExportedEnvironment rewrites the projected
// effective environment to private proxies from the action's selected scope.
func compactKbuildSourceScriptExportedEnvironment(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildSourceScriptExportedEnvironmentForAutomaticTarget(
		profile, target, target, stem, normal, orderOnly, injected, inline, usage,
		expectedScope, configured,
	)
}

func compactKbuildSourceScriptExportedEnvironmentForAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildSourceScriptExportedEnvironmentForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly,
		injected, inline, usage, expectedScope, configured,
	)
}

func compactKbuildSourceScriptExportedEnvironmentForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	environment, _, err := compactKbuildProjectedSourceScriptEnvironmentForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, inline, usage,
	)
	if err != nil {
		return nil, nil, err
	}
	return compactKbuildRewriteSourceScriptEnvironment(environment, expectedScope, configured)
}

func compactKbuildRewriteSourceScriptEnvironment(
	environment map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	roles := map[string]bool{}
	for _, name := range sortedStringMapKeys(environment) {
		rewritten, selectedRoles, err := rewriteKbuildActionRoleRefs(environment[name], expectedScope, configured, false)
		if err != nil {
			return nil, nil, fmt.Errorf("source-script effective variable %s: %w", name, err)
		}
		environment[name] = rewritten
		for _, role := range selectedRoles {
			roles[role] = true
		}
	}
	selectedRoles := make([]string, 0, len(roles))
	for role := range roles {
		selectedRoles = append(selectedRoles, role)
	}
	sort.Strings(selectedRoles)
	return environment, selectedRoles, nil
}

func compactKbuildSourceScriptCommand(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	values map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (compactKbuildSourceScriptInvocation, bool, error) {
	return compactKbuildSourceScriptCommandWithSourceArguments(
		profile, command, command.arguments, values, expectedScope, configured,
	)
}

// compactKbuildSourceScriptCommandWithSourceArguments classifies the payload
// from its pre-projection Make argv while retaining the rewritten command argv
// for execution. Invocation-relative input projection deliberately replaces a
// source spelling such as ../../../scripts/recordmcount.pl with its canonical
// staged path. Re-resolving that bare path from an external module's cwd would
// lose the source root and misclassify the interpreter as an ordinary applet.
func compactKbuildSourceScriptCommandWithSourceArguments(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	sourceArguments []string,
	values map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (compactKbuildSourceScriptInvocation, bool, error) {
	selected := kbuildFields(values["CONFIG_SHELL"])
	selectedProgram := len(selected) != 0 && command.program == selected[0]
	defaultProgram := len(selected) == 0 && command.program == "sh"
	configuredProgram := false
	if sourceRef, ok := parseKbuildActionRoleToken(command.program); ok {
		ref, matches := kbuildActionRoleRefForScope(sourceRef, expectedScope)
		if matches && ref.Role == compactKbuildScriptRuntimeRole {
			for _, candidate := range configured {
				candidate, matches = kbuildActionRoleRefForScope(candidate, expectedScope)
				if matches && candidate == ref {
					configuredProgram = true
					break
				}
			}
		}
	}
	directScriptPath, directSource, directPath := compactKbuildProfileCommandPath(profile, command.program)
	direct := !selectedProgram && !defaultProgram && !configuredProgram &&
		directPath && directSource && compactKbuildProfileSourceUsesShell(profile, directScriptPath) &&
		compactKbuildProfileSourcePathExists(profile, directScriptPath)
	if !direct && !selectedProgram && !defaultProgram && !configuredProgram {
		match, matched, err := compactKbuildProfileSourceInterpreterCommand(
			profile, command.program, sourceArguments,
		)
		if err != nil || !matched {
			return compactKbuildSourceScriptInvocation{}, matched, err
		}
		if match.scriptIndex >= len(command.arguments) {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf(
				"source-script payload index %d exceeds rewritten argument count %d",
				match.scriptIndex, len(command.arguments),
			)
		}
		roles := map[string]bool{}
		scriptArguments := slices.Clone(command.arguments[match.scriptIndex+1:])
		observationArguments := slices.Clone(sourceArguments[match.scriptIndex+1:])
		for index, argument := range scriptArguments {
			rewritten, selectedRoles, rewriteErr := rewriteKbuildActionRoleRefs(argument, expectedScope, configured, false)
			if rewriteErr != nil {
				return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("source-script argument %d: %w", index, rewriteErr)
			}
			scriptArguments[index] = rewritten
			for _, role := range selectedRoles {
				roles[role] = true
			}
		}
		environment := make(map[string]string, len(command.environment))
		for name, value := range command.environment {
			environment[name] = value
		}
		toolRoles := make([]string, 0, len(roles))
		for role := range roles {
			toolRoles = append(toolRoles, role)
		}
		sort.Strings(toolRoles)
		return compactKbuildSourceScriptInvocation{
			interpreterRole: compactKbuildScriptAppletRolePrefix + match.interpreter.program,
			interpreterArguments: append(
				slices.Clone(match.interpreter.arguments),
				command.arguments[:match.scriptIndex]...,
			),
			scriptPath:           match.scriptPath,
			scriptArguments:      scriptArguments,
			observationArguments: observationArguments,
			environment:          environment,
			environmentUsage: compactKbuildSourceScriptEnvironmentUsage{
				ObservesAll: true,
			},
			toolRoles: toolRoles,
		}, true, nil
	}
	selectedArguments := []string{}
	if !direct && selectedProgram {
		if len(sourceArguments) == 0 {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("selected CONFIG_SHELL command has no source script")
		}
		selectedArguments = selected[1:]
		if len(sourceArguments) < len(selectedArguments) || !slices.Equal(sourceArguments[:len(selectedArguments)], selectedArguments) {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("selected CONFIG_SHELL command does not preserve configured interpreter arguments")
		}
	}
	interpreterArguments := []string{}
	scriptIndex := -1
	scriptPath := ""
	if direct {
		scriptPath = directScriptPath
	} else {
		invocation := compactKbuildShellArguments(sourceArguments)
		if invocation.mode != compactKbuildShellModeFile {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf(
				"selected CONFIG_SHELL command uses non-file mode %q", invocation.mode,
			)
		}
		scriptIndex = invocation.scriptIndex
		if scriptIndex < 0 || scriptIndex >= len(sourceArguments) {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("selected CONFIG_SHELL command has no declared source script")
		}
		candidate, source, pathLike := compactKbuildProfileCommandPath(profile, sourceArguments[scriptIndex])
		if !pathLike || !source || !compactKbuildProfileSourcePathExists(profile, candidate) {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf(
				"selected CONFIG_SHELL payload %q is not a declared source file", sourceArguments[scriptIndex],
			)
		}
		if scriptIndex >= len(command.arguments) {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf(
				"selected CONFIG_SHELL script index %d exceeds rewritten argument count %d",
				scriptIndex, len(command.arguments),
			)
		}
		scriptPath = candidate
		interpreterArguments = slices.Clone(command.arguments[:scriptIndex])
	}
	roles := map[string]bool{}
	argumentStart := scriptIndex + 1
	if direct {
		argumentStart = 0
	}
	if argumentStart > len(command.arguments) || argumentStart > len(sourceArguments) {
		return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf(
			"source-script argument start %d exceeds rewritten/source argument counts %d/%d",
			argumentStart, len(command.arguments), len(sourceArguments),
		)
	}
	environmentUsage, err := compactKbuildSourceScriptUsage(profile, scriptPath)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, true, err
	}
	scriptContent, err := readCompactKbuildProfileSource(profile, scriptPath)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, true, err
	}
	argumentScan, err := scanCompactKbuildSourceScript(string(scriptContent))
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("inspect argument use in %q: %w", scriptPath, err)
	}
	scriptArguments := slices.Clone(command.arguments[argumentStart:])
	observationArguments := slices.Clone(sourceArguments[argumentStart:])
	for index, argument := range scriptArguments {
		rewritten, selectedRoles, err := rewriteKbuildActionRoleRefs(argument, expectedScope, configured, false)
		if err != nil {
			return compactKbuildSourceScriptInvocation{}, true, fmt.Errorf("source-script argument %d: %w", index, err)
		}
		scriptArguments[index] = rewritten
		for _, role := range selectedRoles {
			roles[role] = true
		}
	}
	for _, ref := range configured {
		selected, matches := kbuildActionRoleRefForScope(ref, expectedScope)
		if matches && environmentUsage.programs[selected.Role] {
			roles[selected.Role] = true
		}
	}
	environment := make(map[string]string, len(command.environment))
	for name, value := range command.environment {
		environment[name] = value
	}
	toolRoles := make([]string, 0, len(roles))
	for role := range roles {
		toolRoles = append(toolRoles, role)
	}
	sort.Strings(toolRoles)
	interpreter := "sh"
	if !direct && selectedProgram {
		interpreter = path.Base(selected[0])
	}
	return compactKbuildSourceScriptInvocation{
		interpreterRole:      compactKbuildScriptRuntimeRole,
		interpreter:          interpreter,
		interpreterArguments: interpreterArguments,
		scriptPath:           scriptPath,
		scriptArguments:      scriptArguments,
		observationArguments: observationArguments,
		environment:          environment,
		environmentUsage:     environmentUsage,
		argumentUsage:        argumentScan.usage,
		toolRoles:            toolRoles,
	}, true, nil
}

// compactKbuildProfileSourceInterpreterCommand recognizes an explicit
// interpreter command only when its first file operand is an immutable declared
// source and that source's shebang names the same interpreter. This is the
// action-capability boundary for non-shell source generators: neither the
// command name nor the interpreter family is hardcoded in the planner.
func compactKbuildProfileSourceInterpreterCommand(
	profile CompactKbuildProfile,
	program string,
	arguments []string,
) (compactKbuildSourceInterpreterCommandMatch, bool, error) {
	programName := path.Base(program)
	if ref, ok := parseKbuildActionRoleToken(program); ok && strings.HasPrefix(ref.Role, compactKbuildScriptAppletRolePrefix) {
		programName = strings.TrimPrefix(ref.Role, compactKbuildScriptAppletRolePrefix)
	}
	if !safeLinuxSourceScriptCommandName(programName) {
		return compactKbuildSourceInterpreterCommandMatch{}, false, nil
	}
	candidates := make([]int, 0, 1)
	if compactKbuildShellProgram(programName) {
		invocation := compactKbuildShellArguments(arguments)
		if invocation.mode != compactKbuildShellModeFile || invocation.scriptIndex < 0 {
			return compactKbuildSourceInterpreterCommandMatch{}, false, nil
		}
		candidates = append(candidates, invocation.scriptIndex)
	} else {
		for index, argument := range arguments {
			if !strings.HasPrefix(argument, "-") {
				candidates = append(candidates, index)
				break
			}
		}
	}
	for _, index := range candidates {
		argument := arguments[index]
		scriptPath, source, pathLike := compactKbuildProfileCommandPath(profile, argument)
		if !pathLike || !source || !compactKbuildProfileSourcePathExists(profile, scriptPath) {
			return compactKbuildSourceInterpreterCommandMatch{}, false, nil
		}
		interpreter, found, err := compactKbuildProfileSourceShebangInterpreter(profile, scriptPath)
		if err != nil {
			return compactKbuildSourceInterpreterCommandMatch{}, false, err
		}
		if !found || interpreter.program != programName {
			return compactKbuildSourceInterpreterCommandMatch{}, false, nil
		}
		if compactKbuildShellProgram(interpreter.program) {
			// The immutable shebang and explicit command line jointly form the
			// interpreter-owned prefix which scriptrun replays. Validate that
			// complete prefix against the same bounded shell grammar as an
			// ordinary CONFIG_SHELL invocation. In particular, a shebang-provided
			// startup file must not become an undeclared sandbox input.
			prefix := append(slices.Clone(interpreter.arguments), arguments[:index]...)
			classified := compactKbuildShellArguments(append(
				slices.Clone(prefix),
				"linux-bzl-declared-source-script",
			))
			if classified.mode != compactKbuildShellModeFile || classified.scriptIndex != len(prefix) {
				return compactKbuildSourceInterpreterCommandMatch{}, true, fmt.Errorf(
					"source-script shebang interpreter arguments do not select the declared script safely",
				)
			}
		}
		return compactKbuildSourceInterpreterCommandMatch{
			interpreter: interpreter,
			scriptPath:  scriptPath,
			scriptIndex: index,
		}, true, nil
	}
	return compactKbuildSourceInterpreterCommandMatch{}, false, nil
}

func compactKbuildProfileSourceShebangInterpreter(
	profile CompactKbuildProfile,
	sourcePath string,
) (compactKbuildSourceInterpreter, bool, error) {
	content, err := readCompactKbuildProfileSource(profile, sourcePath)
	if err != nil {
		return compactKbuildSourceInterpreter{}, false, err
	}
	interpreter, found := compactKbuildSourceShebangInterpreter(content)
	return interpreter, found, nil
}

// compactKbuildSourceShebangInterpreter parses only the immutable first line.
// /usr/bin/env spellings remain data-driven: the first non-option,
// non-assignment word after env is the interpreter and every following word is
// retained as shebang argument evidence.
func compactKbuildSourceShebangInterpreter(content []byte) (compactKbuildSourceInterpreter, bool) {
	line := string(content)
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	line = strings.TrimSuffix(line, "\r")
	if !strings.HasPrefix(line, "#!") {
		return compactKbuildSourceInterpreter{}, false
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
	if len(fields) == 0 {
		return compactKbuildSourceInterpreter{}, false
	}
	programIndex := 0
	program := path.Base(fields[programIndex])
	if program == "env" {
		programIndex = -1
		for index, field := range fields[1:] {
			if strings.HasPrefix(field, "-") || strings.Contains(field, "=") {
				continue
			}
			programIndex = index + 1
			program = path.Base(field)
			break
		}
		if programIndex < 0 {
			return compactKbuildSourceInterpreter{}, false
		}
	}
	if !safeLinuxSourceScriptCommandName(program) {
		return compactKbuildSourceInterpreter{}, false
	}
	return compactKbuildSourceInterpreter{
		program:   program,
		arguments: slices.Clone(fields[programIndex+1:]),
	}, true
}

// compactKbuildProfileSourceUsesShell recognizes direct immutable helpers from
// their declared source bytes. Linux intentionally gives many shell helpers no
// .sh suffix, so suffix-only detection turns them into generated host tools and
// can introduce a false bootstrap -> host dependency. Keep the historical .sh
// spelling as a conservative source-script signal, then inspect extensionless
// files for a bounded shell shebang. Explicit non-shell interpreter commands
// are classified separately from their immutable shebang evidence.
func compactKbuildProfileSourceUsesShell(profile CompactKbuildProfile, sourcePath string) bool {
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if sourcePath == "" {
		return false
	}
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	if runtime.sourceRoot == "" {
		return strings.HasSuffix(sourcePath, ".sh")
	}
	if err := validatePlanRelativePath("Kbuild source script", sourcePath); err != nil {
		return false
	}
	runtime.sourceMu.Lock()
	defer runtime.sourceMu.Unlock()
	if shell, ok := runtime.sourceShellScripts[sourcePath]; ok {
		return shell
	}
	shell := strings.HasSuffix(sourcePath, ".sh") || compactKbuildSourceFileUsesShell(
		filepath.Join(runtime.sourceRoot, filepath.FromSlash(sourcePath)),
	)
	runtime.sourceShellScripts[sourcePath] = shell
	return shell
}

// compactKbuildSourceFileUsesShell derives an extensionless source program's
// interpreter from its immutable first line. It deliberately recognizes only
// an actual shebang; ordinary source text mentioning a shell is not execution
// evidence.
func compactKbuildSourceFileUsesShell(filename string) bool {
	file, err := os.Open(filename)
	if err != nil {
		return false
	}
	var prefix [4096]byte
	count, _ := file.Read(prefix[:])
	_ = file.Close()
	interpreter, found := compactKbuildSourceShebangInterpreter(prefix[:count])
	return found && compactKbuildShellProgram(interpreter.program)
}

func (invocation compactKbuildSourceScriptInvocation) recipeArguments(
	scriptBinding string,
	_ map[string]string,
	runtimeApplets []compactKbuildScriptRuntimeApplet,
) ([]string, error) {
	interpreterRole := invocation.interpreterRole
	if interpreterRole == "" {
		interpreterRole = compactKbuildScriptRuntimeRole
	}
	arguments := []string{
		"-interpreter", "${tool:" + interpreterRole + "}",
	}
	if invocation.interpreter != "" {
		arguments = append(arguments, "-interpreter_arg", invocation.interpreter)
	}
	for _, argument := range invocation.interpreterArguments {
		arguments = append(arguments, "-interpreter_arg", argument)
	}
	arguments = append(arguments,
		"-multicall", "${tool:"+compactKbuildScriptRuntimeRole+"}",
		"-tool", compactKbuildScriptRuntimeRole+"=${tool:"+compactKbuildScriptRuntimeRole+"}",
		"-script", "${source:"+scriptBinding+"}",
	)
	for _, role := range invocation.toolRoles {
		arguments = append(arguments, "-tool", role+"=${tool:"+role+"}")
	}
	for _, applet := range runtimeApplets {
		arguments = append(arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, invocation.scriptArguments...)
	return arguments, nil
}

func (invocation compactKbuildSourceScriptInvocation) auxiliaryToolRoles(runtimeApplets []compactKbuildScriptRuntimeApplet) []string {
	roles := []string{compactKbuildScriptRuntimeRole}
	appendRole := func(role string) {
		if role != "" && !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	appendRole(invocation.interpreterRole)
	for _, role := range invocation.toolRoles {
		appendRole(role)
	}
	for _, applet := range runtimeApplets {
		appendRole(applet.role)
	}
	return roles
}

func (invocation compactKbuildSourceScriptInvocation) describesSource(pathname string) bool {
	return path.Clean(pathname) == invocation.scriptPath
}
