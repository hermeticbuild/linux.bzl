package kconfig

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// An outputless source check can pass the selected Kbuild compiler a depfile
// destination without making that depfile a Make target. The depfile remains
// private to the check's writable tree; every other change is still rejected
// by the runner before it publishes the check's completion state.
func compactKbuildSourceCheckCompilerDepfileEffects(
	profile CompactKbuildProfile,
	target string,
	executionDirectory string,
	arguments, auxiliaryTools []string,
) []ActionRecipePrivateWorkingEffect {
	separator := slices.Index(arguments, "--")
	if separator < 0 || separator+1 >= len(arguments) {
		return nil
	}
	command := arguments[separator+1:]
	role := command[0]
	_, sourceRole, _, valid := toolaction.SplitBinding(role)
	if !valid || sourceRole != "cc" && sourceRole != "cxx" || !slices.Contains(auxiliaryTools, role) {
		return nil
	}
	bound := false
	for index := 0; index+1 < separator; index++ {
		if arguments[index] == "-tool" && arguments[index+1] == role+"=${tool:"+role+"}" {
			bound = true
			break
		}
	}
	if !bound {
		return nil
	}

	target = canonicalKbuildRulePath(target)
	if target == "" {
		return nil
	}
	depfile := path.Join(path.Dir(target), "."+path.Base(target)+".d")
	if err := validatePlanRelativePath("source-check compiler depfile", depfile); err != nil {
		return nil
	}
	// The selected compiler runs below the recipe's private object tree at
	// executionDirectory. Kbuild's $(dot-target).d can therefore be spelled
	// relative to that cwd (for a root PHONY target, ./.target.d), as well as
	// with the explicit private-root marker. Bind only these exact spellings;
	// a different or traversing compiler destination is not a private effect.
	from := executionDirectory
	if from == "" {
		from = "."
	}
	relative, err := filepath.Rel(filepath.FromSlash(from), filepath.FromSlash(depfile))
	if err != nil {
		return nil
	}
	relative = filepath.ToSlash(relative)
	withinCwd := relative != "." && relative != ".." && !strings.HasPrefix(relative, "../")
	location, located := CompactKbuildProfileInvocationLocation(profile)
	if located && location.Directory != executionDirectory {
		return nil
	}
	if !located {
		if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
			Tree: CompactKbuildInvocationObjectTree, Directory: executionDirectory,
		}); err != nil {
			return nil
		}
	}
	outputs, err := compactKbuildExactCDependencyOutputs(profile, command[1:])
	if err != nil || !slices.Equal(outputs, []string{depfile}) {
		return nil
	}
	rooted := "${work:root}/" + depfile
	matches := func(argument string) bool {
		return argument == rooted || withinCwd && (argument == relative || argument == "./"+relative)
	}
	var destinations []string
	for index := 1; index < len(command); index++ {
		argument := command[index]
		if argument == "--" {
			break
		}
		if argument == "-MF" {
			if index+1 >= len(command) {
				return nil
			}
			index++
			destinations = append(destinations, command[index])
			continue
		}
		if strings.HasPrefix(argument, "-MF") {
			destinations = append(destinations, strings.TrimPrefix(argument, "-MF"))
			continue
		}
		if strings.HasPrefix(argument, "-Wp,") {
			forwarded := strings.Split(argument, ",")
			for field := 1; field < len(forwarded); field++ {
				if forwarded[field] != "-MD" && forwarded[field] != "-MMD" && forwarded[field] != "-MF" {
					continue
				}
				if field+1 >= len(forwarded) {
					return nil
				}
				field++
				destinations = append(destinations, forwarded[field])
			}
		}
	}
	if len(destinations) == 0 {
		return nil
	}
	for _, destination := range destinations {
		if !matches(destination) {
			return nil
		}
	}
	return []ActionRecipePrivateWorkingEffect{{Path: depfile, Kind: "regular"}}
}

// A cmd_<name> wrapper executes the same authenticated source-script argv as a
// direct source check, but through one enclosing shell. Project its already
// selected script arguments into that shell's private object tree and use the
// direct check's compiler/depfile proof. The wrapper itself remains intact.
func compactKbuildSourceCheckInvocationDepfileEffects(
	profile CompactKbuildProfile,
	target string,
	invocation compactKbuildSourceScriptInvocation,
) []ActionRecipePrivateWorkingEffect {
	executionDirectory, _, err := compactKbuildTypedPrivateExecution(profile)
	if err != nil {
		return nil
	}
	if len(invocation.scriptArguments) == 0 {
		return nil
	}
	role := invocation.scriptArguments[0]
	_, sourceRole, _, valid := toolaction.SplitBinding(role)
	if !valid || sourceRole != "cc" && sourceRole != "cxx" || !slices.Contains(invocation.toolRoles, role) {
		return nil
	}
	arguments := []string{"-tool", role + "=${tool:" + role + "}", "--"}
	for _, argument := range invocation.scriptArguments {
		arguments = append(arguments, compactKbuildSourceScriptWorkingValue(
			compactKbuildFinalizeRootedActionRecipeText(compactKbuildProfileCanonicalRecipeText(profile, argument)),
		))
	}
	return compactKbuildSourceCheckCompilerDepfileEffects(
		profile, target, executionDirectory, arguments, invocation.toolRoles,
	)
}

// A wrapped check has two separate source identities: the Make-expanded
// command line and the script staged in the private object tree. Check the
// command actually serialized for execution against that selected line, then
// prove its compiler depfile with the same bounded parser as a direct check.
// Projected object paths may be relative to the selected invocation cwd.
func compactKbuildWrappedSourceCheckEffects(recipe ActionRecipe) ([]ActionRecipePrivateWorkingEffect, error) {
	completion := recipe.MakePhonyCompletion
	if completion == nil || completion.ScriptPath == "" {
		return nil, fmt.Errorf("wrapped source check has no source-script receipt")
	}
	selected, err := compactKbuildCompoundProgramCommands(completion.SelectedLine)
	if err != nil {
		return nil, fmt.Errorf("selected PHONY source command: %w", err)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("selected PHONY source command has no executable command")
	}
	directory := recipe.ExecutionDirectory
	if directory == "" {
		directory = "."
	}
	objectRoot, err := filepath.Rel(filepath.FromSlash(directory), ".")
	if err != nil {
		return nil, fmt.Errorf("project PHONY source working directory: %w", err)
	}
	objectRoot = filepath.ToSlash(objectRoot)
	// The selected shell bytes, including quoting, redirection and control
	// operators, must be the bytes executed by the private runner. Cooked argv
	// equality would conflate quoted and unquoted globs or active expansions.
	projected := replaceCompactKbuildTreeRoot(completion.SelectedLine, "${tree:prep}", objectRoot)
	refs, err := KbuildActionRoleRefs(projected)
	if err != nil {
		return nil, err
	}
	for _, sourceRef := range refs {
		_, binding, valid := kbuildActionRoleBinding(sourceRef, completion.ActionScope)
		if !valid || !slices.Contains(recipe.AuxiliaryTools, binding) ||
			!compactKbuildRecipeBindsAuxiliaryTool(recipe, binding) {
			return nil, fmt.Errorf("selected PHONY command uses unbound action role %q", sourceRef.Role)
		}
		projected = strings.ReplaceAll(projected, KbuildActionRoleToken(sourceRef.Scope, sourceRef.Role), binding)
	}
	selectedShell := compactKbuildRecipeLineShells([]string{projected})
	// Literal tree spellings stay protected while projecting active roots.
	// Restore them over the whole script, where the runner's byte offsets are
	// defined, and bind exactly those offsets to the selected source line.
	selectedScript, literalOffsets, err := restoreCompactKbuildLiteralActionMarkers(
		"#!/bin/sh\nset -e\n" + selectedShell + "\n",
	)
	if err != nil {
		return nil, fmt.Errorf("selected PHONY literal tree marker: %w", err)
	}
	boundOffsets, err := actionRecipeLiteralTreeOffsetArguments(recipe.Arguments)
	if err != nil {
		return nil, err
	}
	actualOffsets := make([]int, 0, len(boundOffsets))
	for _, bound := range boundOffsets {
		actualOffsets = append(actualOffsets, bound.offset)
	}
	slices.Sort(actualOffsets)
	if !slices.Equal(literalOffsets, actualOffsets) {
		return nil, fmt.Errorf("selected PHONY literal tree offsets differ from the executed script")
	}
	selectedShell = strings.TrimSuffix(strings.TrimPrefix(selectedScript, "#!/bin/sh\nset -e\n"), "\n")
	if completion.ExpandedLine != selectedShell {
		index := 0
		for index < min(len(selectedShell), len(completion.ExpandedLine)) &&
			selectedShell[index] == completion.ExpandedLine[index] {
			index++
		}
		start := max(0, index-12)
		return nil, fmt.Errorf(
			"selected PHONY source shell bytes differ from the executed wrapper at byte %d (selected %d bytes %q, executed %d bytes %q)",
			index, len(selectedShell), selectedShell[start:min(len(selectedShell), index+20)],
			len(completion.ExpandedLine), completion.ExpandedLine[start:min(len(completion.ExpandedLine), index+20)],
		)
	}
	var effects []ActionRecipePrivateWorkingEffect
	matchedScript := false
	for _, command := range selected {
		argumentStart := -1
		if command.program == "${tree:kernel}/"+completion.ScriptPath {
			argumentStart = 0
		} else if compactKbuildShellProgram(command.program) {
			invocation := compactKbuildShellArguments(command.arguments)
			if invocation.mode == compactKbuildShellModeFile && invocation.scriptIndex >= 0 &&
				invocation.scriptIndex < len(command.arguments) &&
				command.arguments[invocation.scriptIndex] == "${tree:kernel}/"+completion.ScriptPath {
				argumentStart = invocation.scriptIndex + 1
			}
		}
		if argumentStart < 0 {
			continue
		}
		if matchedScript {
			return nil, fmt.Errorf("selected PHONY command repeats its authenticated source script")
		}
		matchedScript = true
		if argumentStart == len(command.arguments) {
			continue
		}
		ref, roleRef := parseKbuildActionRoleToken(command.arguments[argumentStart])
		if !roleRef || ref.Role != "cc" && ref.Role != "cxx" {
			continue
		}
		_, binding, valid := kbuildActionRoleBinding(ref, completion.ActionScope)
		if !valid || !slices.Contains(recipe.AuxiliaryTools, binding) ||
			!compactKbuildRecipeBindsAuxiliaryTool(recipe, binding) {
			return nil, fmt.Errorf("selected PHONY source script has no bound compiler")
		}
		arguments := []string{"-tool", binding + "=${tool:" + binding + "}", "--", binding}
		for _, argument := range command.arguments[argumentStart+1:] {
			arguments = append(arguments, strings.ReplaceAll(argument, "${tree:prep}", "${work:root}"))
		}
		effects = compactKbuildSourceCheckCompilerDepfileEffects(
			CompactKbuildProfile{}, completion.Target, recipe.ExecutionDirectory,
			arguments, recipe.AuxiliaryTools,
		)
	}
	if !matchedScript {
		return nil, fmt.Errorf("selected PHONY command does not execute its authenticated source script")
	}
	return effects, nil
}

func compactKbuildRecipeBindsAuxiliaryTool(recipe ActionRecipe, role string) bool {
	for index := 0; index+1 < len(recipe.Arguments); index += 2 {
		if recipe.Arguments[index] == "-tool" && recipe.Arguments[index+1] == role+"=${tool:"+role+"}" {
			return true
		}
	}
	return false
}
