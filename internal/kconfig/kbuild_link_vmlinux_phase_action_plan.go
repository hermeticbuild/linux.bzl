package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
)

// buildSelectedSourceScriptPhase lowers an authenticated write within the
// selected link script before its next recursive Make call. Its graph target
// deliberately differs from the enclosing Make target: closing a "vmlinux"
// working frontier here would import both children, including the later
// modpost child which consumes vmlinux.o.
func (b *compactKbuildRulePlanBuilder) buildSelectedSourceScriptPhase(
	phase CompactKbuildSelectedSourcePhase,
	owner compactKbuildSelectionKey,
) (string, error) {
	if b == nil || !b.selectionBound || b.selectionGraph == nil || b.profile == nil ||
		b.selection.profile != owner.profile || b.selection.stage != owner.stage ||
		b.selection.target != phase.OutputPath || owner.target != phase.OwnerTarget ||
		!b.configuredActionRole(KbuildActionRoleRef{Scope: b.actionScope(), Role: "ld"}) {
		return "", fmt.Errorf("selected source phase %q lacks an exact owner and configured linker", phase.OutputPath)
	}
	ownerSelection, exists := b.selectionGraph.selections[owner]
	if !exists || ownerSelection.SourceScriptPhase != "" {
		return "", fmt.Errorf("selected source phase %q lacks its real Make selection", phase.OutputPath)
	}
	match, matched, err := b.metadata.compactKbuildRuleForProfileMakeTarget(
		*b.profile, owner.target, ownerSelection.MakeTarget,
	)
	if err != nil || !matched {
		return "", fmt.Errorf("selected source phase %q owner Make rule: matched=%t: %w", phase.OutputPath, matched, err)
	}
	content, err := readCompactKbuildProfileSource(*b.profile, phase.SourcePath)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q immutable script: %w", phase.OutputPath, err)
	}
	analyzed, recognized, err := AnalyzeCompactKbuildLinkVmlinuxPhases(string(content))
	if err != nil || !recognized || phase.SourceSHA256 != analyzed.SourceSHA256 {
		return "", fmt.Errorf("selected source phase %q lost its source authentication: recognized=%t: %w", phase.OutputPath, recognized, err)
	}
	var script string
	var expected []CompactKbuildLinkVmlinuxSourceSpan
	switch phase.Ordinal {
	case 0:
		script, expected = analyzed.VersionScript, analyzed.VersionSpans
	case 1:
		script, expected = analyzed.ObjectScript, analyzed.ObjectSpans
	default:
		return "", fmt.Errorf("selected source phase %q has unsupported ordinal %d", phase.OutputPath, phase.Ordinal)
	}
	if !slices.Equal(phase.Spans, expected) || script == "" || len(phase.SourceArguments) == 0 {
		return "", fmt.Errorf("selected source phase %q has inconsistent spans or missing linker argv", phase.OutputPath)
	}
	// Keep the source-selected Make argv in the profile for provenance, then
	// lower each configured action token exactly as a normal Kbuild recipe does.
	// The LLVM toolset passes LD as an inert role token while evaluating Make;
	// scriptrun installs its configured ld proxy under the private PATH. The
	// phase script's original `$LD` therefore executes the same linker as the
	// unsplit selected source invocation.
	runtimeArguments := slices.Clone(phase.SourceArguments)
	argumentRoles := []string{}
	for index, argument := range runtimeArguments {
		rewritten, bound, rewriteErr := rewriteKbuildActionRoleRefs(argument, b.actionScope(), b.metadata.actionRoles, false)
		if rewriteErr != nil {
			return "", fmt.Errorf("selected source phase %q linker argv %d: %w", phase.OutputPath, index, rewriteErr)
		}
		runtimeArguments[index] = rewritten
		argumentRoles = append(argumentRoles, bound...)
	}
	if runtimeArguments[0] != "ld" {
		return "", fmt.Errorf("selected source phase %q linker argv %q is not the configured ld proxy", phase.OutputPath, runtimeArguments[0])
	}
	usage, err := compactKbuildHermeticScriptEnvironmentUsage(*b.profile, script, nil)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q environment usage: %w", phase.OutputPath, err)
	}
	// The selected script analyzer proves its top-level config import. The
	// scanner also sees shell programs inside function definitions, so check
	// actual dot-source operands for additional imports before binding the
	// generated assignment file from the Make-visible snapshot.
	scan, err := scanCompactKbuildSourceScript(script)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q generated shell import: %w", phase.OutputPath, err)
	}
	if scan.dynamicDotSource || len(scan.dotSources) != 1 || scan.dotSources[0] != "include/config/auto.conf" {
		return "", fmt.Errorf("selected source phase %q has no unique generated static config import: %q", phase.OutputPath, scan.dotSources)
	}
	declared, err := compactKbuildDeclaredGeneratedShellSource(*b.profile, scan.dotSources[0])
	if err != nil || !declared {
		return "", fmt.Errorf("selected source phase %q generated static config source: declared=%t: %w", phase.OutputPath, declared, err)
	}
	if usage.generatedShellSources == nil {
		usage.generatedShellSources = map[string]bool{}
	}
	usage.generatedShellSources[scan.dotSources[0]] = true
	// Each source phase sees only the files preceding its own source position.
	// The selection graph supplies exact producer identities for these roots;
	// the private closure stages their generated inputs, archives, and headers.
	dependencies, err := b.selectionGraph.selectionDependencies(b.metadata, b.selection)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q input frontier: %w", phase.OutputPath, err)
	}
	direct := []compactKbuildRuleInput{}
	for _, dependency := range dependencies {
		dependency = b.selectionGraph.compactKbuildGroupedSelectionRepresentative(dependency)
		producer := b.selectionGraph.materializedProducers[dependency]
		if producer == "" {
			return "", fmt.Errorf("selected source phase %q predecessor %s has no producer", phase.OutputPath, compactKbuildSelectionKeyString(dependency))
		}
		node, found := compactKbuildPlanNode(b.plan, producer)
		if !found || !compactKbuildPlanStageVisible(node.Stage, b.planContext().Stage) {
			return "", fmt.Errorf("selected source phase %q predecessor %s is absent or in a later stage", phase.OutputPath, compactKbuildSelectionKeyString(dependency))
		}
		outputSlot := -1
		for slot, output := range node.Outputs {
			if output.Path == dependency.target && output.ObservedPath == "" {
				if outputSlot >= 0 {
					return "", fmt.Errorf("selected source phase %q predecessor %s publishes repeated target", phase.OutputPath, compactKbuildSelectionKeyString(dependency))
				}
				outputSlot = slot
			}
		}
		if outputSlot < 0 {
			// A source-selected PHONY check can precede this script phase
			// without publishing a pathname. Its private completion is bound
			// by appendCompactKbuildSelectedPlanNode from this same dependency.
			if compactKbuildAuthenticatedExecutionCheckCompletion(b.plan, node, dependency.target) {
				continue
			}
			return "", fmt.Errorf("selected source phase %q predecessor %s has no file output", phase.OutputPath, compactKbuildSelectionKeyString(dependency))
		}
		direct = append(direct, compactKbuildRuleInput{path: dependency.target, producer: producer, slot: outputSlot})
	}
	// The source prelude imports Kconfig's generated auto.conf into every
	// phase. Bind its exact staged projection separately from Make targets.
	for _, pathname := range slices.Sorted(maps.Keys(usage.generatedShellSources)) {
		input, found, inputErr := b.existingInput(pathname)
		if inputErr != nil {
			return "", inputErr
		}
		if !found {
			input, found, inputErr = b.compactKbuildConfigProjectionBaselineInput(pathname)
			if inputErr != nil {
				return "", inputErr
			}
		}
		if !found {
			return "", fmt.Errorf("selected source phase %q imports %q without exact config input", phase.OutputPath, pathname)
		}
		input.workingOnly = true
		direct = upsertCompactKbuildRuleInput(direct, input)
	}
	inputFrontier, err := b.compactKbuildWorkingTreeInputFrontier(phase.OutputPath, *b.profile, direct)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q private object tree: %w", phase.OutputPath, err)
	}
	if phase.Ordinal == 0 {
		if err := b.requireFreshLinkVmlinuxVersionState(inputFrontier); err != nil {
			return "", fmt.Errorf("selected source phase %q version state: %w", phase.OutputPath, err)
		}
	}
	inputs := inputFrontier.direct
	// Neither phase includes a recursive Make boundary. MAKE may nevertheless
	// be exported from the enclosing Make invocation with an evaluator-owned
	// capability token; it belongs only to the later child replay, not to this
	// action. Reject a source phase that actually reads MAKE, then drop only
	// this exact unused private token before serializing the recipe.
	if usage.uses("MAKE") {
		return "", fmt.Errorf("selected source phase %q reads recursive MAKE without a child replay", phase.OutputPath)
	}
	environment, roles, err := compactKbuildSourceScriptEnvironment(
		owner.target, match, inputs, nil, usage, b.actionScope(), b.metadata.actionRoles,
	)
	if err != nil {
		return "", fmt.Errorf("selected source phase %q exported environment: %w", phase.OutputPath, err)
	}
	if environment["MAKE"] == CompactKbuildRecursiveMakeProvenanceToken {
		delete(environment, "MAKE")
	}
	if !slices.Contains(roles, "ld") {
		roles = append(roles, "ld")
	}
	roles = append(roles, argumentRoles...)
	sort.Strings(roles)
	roles = slices.Compact(roles)
	applets, err := compactKbuildScriptRuntimeApplets(b.metadata.actionRoles, b.actionScope())
	if err != nil {
		return "", err
	}
	node := b.actionNode("generate", compactKbuildScriptRunnerRole, phase.OutputPath)
	node.InputSet = inputFrontier.inputSet
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{
			"-interpreter", "${tool:" + compactKbuildScriptRuntimeRole + "}",
			"-interpreter_arg", "sh", "-multicall", "${tool:" + compactKbuildScriptRuntimeRole + "}",
			"-tool", compactKbuildScriptRuntimeRole + "=${tool:" + compactKbuildScriptRuntimeRole + "}",
		},
		WorkingDirectory: "kbuild-link-phase-" + escapeKbuildIdentifier(phase.OutputPath),
		WorkingInputs:    map[string]string{}, WorkingOutputs: map[string]string{"00000000": phase.OutputPath},
		Environment: environment, Outputs: []string{"00000000"},
		AuxiliaryTools: []string{compactKbuildScriptRuntimeRole},
	}
	scriptID, err := b.metadata.ensureActionPlanSource(b.plan, phase.SourcePath)
	if err != nil {
		return "", err
	}
	scriptKey, err := appendCompactKbuildRecipeInput(&node, &recipe,
		compactKbuildRuleInput{path: phase.SourcePath, sourceID: scriptID}, "script")
	if err != nil {
		return "", err
	}
	recipe.Arguments = append(recipe.Arguments,
		"-script", "${source:"+scriptKey+"}", "-script_source_sha256", phase.SourceSHA256)
	for _, span := range phase.Spans {
		recipe.Arguments = append(recipe.Arguments,
			"-script_source_span", strconv.Itoa(span.Start)+":"+strconv.Itoa(span.End))
	}
	recipe.Arguments = append(recipe.Arguments,
		"-static_source_assignments", "${work:root}/include/config/auto.conf")
	for _, role := range roles {
		recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, role)
		recipe.Arguments = append(recipe.Arguments, "-tool", role+"=${tool:"+role+"}")
	}
	for _, applet := range applets {
		recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, applet.role)
		recipe.Arguments = append(recipe.Arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
	}
	recipe.Arguments = append(recipe.Arguments, "--")
	recipe.Arguments = append(recipe.Arguments, runtimeArguments...)
	node.AuxiliaryTools = slices.Clone(recipe.AuxiliaryTools)
	for _, input := range inputs {
		if input.path == "" {
			continue
		}
		role := compactKbuildRuleInputEdgeRole(input, "prerequisite")
		key, inputErr := appendCompactKbuildRecipeInput(&node, &recipe, input, role)
		if inputErr != nil {
			return "", inputErr
		}
		prefix := "source:"
		if input.producer != "" {
			prefix = "input:"
		}
		recipe.WorkingInputs[prefix+key] = input.path
	}
	if err := b.bindCompactKbuildSourceOverlayWorkingTree(*b.profile, &recipe); err != nil {
		return "", err
	}
	if err := appendReferencedPlanTrees(b.plan, &node, &recipe); err != nil {
		return "", err
	}
	return b.appendCompactKbuildSelectedPlanNode(phase.OutputPath, node, recipe)
}

// The historical link script increments an existing .version rather than
// writing a constant. The bounded phase runner currently models a clean
// object tree; reusing an earlier selected or preconfigured .version would
// silently take the wrong branch unless its exact bytes and owner were
// staged. Check the complete invocation-start view and persistent frontier,
// since the owner's serialized initial-artifact subset may omit this read.
func (b *compactKbuildRulePlanBuilder) requireFreshLinkVmlinuxVersionState(frontier compactKbuildInputFrontier) error {
	const path = ".version"
	if artifact, found := CompactKbuildProfileInitialVisibleArtifact(*b.profile, path); found {
		return fmt.Errorf("prior selected %q artifact from %s:%s requires an exact incremental writer", path, artifact.Profile, artifact.Target)
	}
	evidence, err := b.sourcePathEvidence(path)
	if err != nil {
		return fmt.Errorf("inspect existing %q: %w", path, err)
	}
	if evidence.exists {
		return fmt.Errorf("preexisting %q source state requires an exact incremental writer", path)
	}
	if slices.ContainsFunc(frontier.direct, func(input compactKbuildRuleInput) bool { return input.path == path }) {
		return fmt.Errorf("existing %q working input requires an exact incremental writer", path)
	}
	store, err := b.plan.planningActionPlanInputSetStore()
	if err != nil {
		return fmt.Errorf("inspect %q working frontier: %w", path, err)
	}
	_, present, err := store.Lookup(frontier.inputSet, ActionPlanInputSetTarget{
		Kind: ActionPlanInputSetWorkTarget, Path: path,
	})
	if err != nil {
		return fmt.Errorf("inspect %q working frontier: %w", path, err)
	}
	if present {
		return fmt.Errorf("existing %q working frontier requires an exact incremental writer", path)
	}
	return nil
}
