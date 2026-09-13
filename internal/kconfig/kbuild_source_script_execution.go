package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// compactKbuildSourceScriptInjections binds Make's invocation-tree variables
// to stable planner markers. Source paths remain immutable tree inputs; object
// paths are rewritten to the recipe's private writable root when the concrete
// source-script action is assembled.
func compactKbuildSourceScriptInjections(directory, targetStem string) map[string]string {
	directory = strings.Trim(strings.TrimSpace(directory), "/")
	if directory == "." {
		directory = ""
	}
	objectDirectory := "__LINUX_BZL_OBJECT_TREE__"
	sourceDirectory := "__LINUX_BZL_SOURCE_TREE__"
	if directory != "" {
		objectDirectory += "/" + directory
		sourceDirectory += "/" + directory
	}
	injected := map[string]string{
		"abs_output":  "__LINUX_BZL_OBJECT_TREE__",
		"abs_srctree": "__LINUX_BZL_SOURCE_TREE__",
		"obj":         objectDirectory,
		"objtree":     "__LINUX_BZL_OBJECT_TREE__",
		"src":         sourceDirectory,
		"srcroot":     "__LINUX_BZL_SOURCE_TREE__",
		"srctree":     "__LINUX_BZL_SOURCE_TREE__",
	}
	if targetStem != "" {
		injected["target-stem"] = targetStem
	}
	return injected
}

// compactKbuildSourceOverlayRoot identifies an out-of-tree source mapping
// whose Make process executes against a writable object overlay. A plain
// object-tree invocation is not enough: ordinary in-tree Kbuild also executes
// from the object tree. The nested source-root mapping is the explicit
// provenance that the profile's logical directory belongs to a separately
// supplied source tree (for example M= for an external module).
//
// The process cwd is deliberately not used for containment. Linux may invoke
// scripts/Makefile.build from the object-tree root with obj=$M and no -C; that
// leaves InvocationLocation.Directory empty while profile.Directory carries
// the selected external directory.
func compactKbuildSourceOverlayRoot(profile CompactKbuildProfile) (string, bool, error) {
	location, locationSet := CompactKbuildProfileInvocationLocation(profile)
	if !locationSet || location.Tree != CompactKbuildInvocationObjectTree ||
		profile.evaluator == nil || profile.evaluator.template == nil {
		return "", false, nil
	}

	const sourceRoot = "__LINUX_BZL_SOURCE_TREE__"
	directory := strings.Trim(strings.TrimSpace(profile.Directory), "/")
	if directory == "." {
		directory = ""
	}
	best := ""
	for marker := range profile.evaluator.template.sourceRoots {
		relative, nested := strings.CutPrefix(marker, sourceRoot+"/")
		if !nested {
			continue
		}
		canonical, err := canonicalCompactKbuildInvocationPath(relative)
		if err != nil || canonical != relative {
			if err == nil {
				err = fmt.Errorf("path is not canonical")
			}
			return "", false, fmt.Errorf("nested Kbuild source root %q: %w", marker, err)
		}
		if directory != canonical && !strings.HasPrefix(directory, canonical+"/") {
			continue
		}
		if len(canonical) > len(best) {
			best = canonical
		}
	}
	if best == "" {
		return "", false, nil
	}
	return best, true, nil
}

// CompactKbuildProfileSourceOverlayRoot exposes the source-derived overlay
// identity to invocation discovery. Exact generated-content probes run before
// ActionPlan lowering, so they must reject this writable namespace at the same
// boundary as the final working-tree proof.
func CompactKbuildProfileSourceOverlayRoot(profile CompactKbuildProfile) (string, bool, error) {
	return compactKbuildSourceOverlayRoot(profile)
}

// compactKbuildGraphPathUsesSourceOverlay reports whether graphPath belongs to
// the separately supplied source tree which is overlaid into this Make
// invocation's private writable root. Component-aware containment is
// important here: an overlay named "external/demo" must not claim a sibling
// such as "external/demo-other".
func compactKbuildGraphPathUsesSourceOverlay(profile CompactKbuildProfile, graphPath string) (bool, error) {
	root, overlay, err := compactKbuildSourceOverlayRoot(profile)
	if err != nil || !overlay {
		return false, err
	}
	graphPath = compactKbuildGraphTargetPath(graphPath)
	if graphPath == "" {
		return false, nil
	}
	return graphPath == root || strings.HasPrefix(graphPath, root+"/"), nil
}

// compactKbuildSourceScriptInjectionsForTarget evaluates Kbuild's own
// target-stem definition before invocation-tree paths are replaced by stable
// plan markers. The logical obj/src spellings make source expressions such as
// $(basename $(patsubst $(obj)/%,%,$@)) observe the same target relationship
// as the captured Make process, including explicit rules whose pattern stem is
// empty.
func compactKbuildSourceScriptInjectionsForTarget(
	profile CompactKbuildProfile,
	target, patternStem string,
	normal, orderOnly []string,
) (map[string]string, error) {
	return compactKbuildSourceScriptInjectionsForMakeTarget(
		profile, target, target, target, patternStem, normal, orderOnly,
	)
}

func compactKbuildSourceScriptInjectionsForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, patternStem string,
	normal, orderOnly []string,
) (map[string]string, error) {
	directory := profile.Directory
	if directory == "" {
		directory = "."
	}
	values, err := evaluateCompactKbuildTargetForMakeTarget(
		profile,
		target,
		lookupTarget,
		automaticTarget,
		patternStem,
		normal,
		orderOnly,
		map[string]string{"obj": directory, "src": directory},
		true,
		"target-stem",
	)
	if err != nil {
		return nil, fmt.Errorf("evaluate source-derived target-stem for %q: %w", target, err)
	}
	targetStem := strings.TrimSpace(values["target-stem"])
	if targetStem == "" {
		// A compact fixture or non-Kbuild Make graph may not define Kbuild's
		// target-stem helper. In that case GNU Make's matched pattern stem is
		// the authoritative value. Explicit rules still keep an empty pattern
		// stem unless their source graph derives one from $@ above.
		targetStem = patternStem
	}
	// obj/src describe the current recursive Make invocation, not the selected
	// target's parent directory. A parent Kbuild legitimately owns a child goal
	// such as $(obj)/compressed/vmlinux while its recipe recursively invokes
	// obj=$(obj)/compressed. Deriving obj from that goal would append compressed
	// twice and turn the child driver into an object-tree pathname.
	injected := compactKbuildSourceScriptInjections(profile.Directory, targetStem)
	overlayRoot, overlay, err := compactKbuildSourceOverlayRoot(profile)
	if err != nil {
		return nil, err
	}
	if !overlay {
		return injected, nil
	}

	// External Kbuild source inputs are staged at their logical paths in every
	// action's private writable tree.  Point aliases derived from M= at that
	// overlay so source-selected include directories such as -I$(src) observe
	// the staged external tree.  The kernel srctree/objtree capabilities remain
	// rooted at the kernel source and prepared object trees respectively.
	overlayDirectory := "__LINUX_BZL_OBJECT_TREE__/" + overlayRoot
	injected["abs_output"] = overlayDirectory
	injected["srcroot"] = overlayDirectory
	injected["src"] = "__LINUX_BZL_OBJECT_TREE__/" + strings.Trim(strings.TrimSpace(profile.Directory), "/")
	return injected, nil
}

// CompactKbuildTargetEvaluationInjections returns the exact source/object-tree
// bindings used when lowering a selected target. Graph selection must evaluate
// command templates through this same target context: an invocation-wide
// profile value for $(obj) is not specific enough to prove that the resulting
// action observes its writable object-tree directory.
func CompactKbuildTargetEvaluationInjections(
	profile CompactKbuildProfile,
	target, patternStem string,
	normal, orderOnly []string,
) (map[string]string, error) {
	return compactKbuildSourceScriptInjectionsForTarget(
		profile, target, patternStem, normal, orderOnly,
	)
}

// CompactKbuildTargetEvaluationInjectionsForMakeTarget preserves GNU Make's
// lexical target identity while deriving source-defined helpers such as
// target-stem. target remains the canonical graph/producer identity;
// lookupTarget and automaticTarget are the exact rule-search and $@ spellings.
func CompactKbuildTargetEvaluationInjectionsForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, patternStem string,
	normal, orderOnly []string,
) (map[string]string, error) {
	return compactKbuildSourceScriptInjectionsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, patternStem, normal, orderOnly,
	)
}

func compactKbuildSourceScriptInjectionsForRuleTarget(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (map[string]string, error) {
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, match, inputs)
	if err != nil {
		return nil, err
	}
	return compactKbuildSourceScriptInjectionsForMakeTarget(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order,
	)
}

func compactKbuildSourceScriptWorkingValue(value string) string {
	return strings.ReplaceAll(value, "${tree:prep}", "${work:root}")
}

func compactKbuildSourceScriptReplayValue(profile CompactKbuildProfile, value string) string {
	// Replay argv is executed from the same private writable view as the source
	// script. Use the generic profile root projection so nested external roots,
	// host dependencies, and ordinary kernel/object roots retain their exact
	// provenance without a maintained list of Make variable names.
	return compactKbuildSourceScriptWorkingValue(
		compactKbuildProfileCanonicalRecipeText(profile, value),
	)
}

type compactKbuildInvocationMaterialization struct {
	outputs []string
	roots   []compactKbuildRuleInput
}

// compactKbuildInvocationDependencyMaterialization resolves the regular files
// which make one recursive invocation complete. A recursive goal can itself be
// phony; in that case the selected terminal recipes, rather than the goal
// spelling passed to Make, are the files staged into the private object tree
// and verified by the exact-argv replay proxy.
func (b *compactKbuildRulePlanBuilder) compactKbuildInvocationDependencyMaterialization(
	target string,
	profile CompactKbuildProfile,
	dependency CompactKbuildInvocationDependency,
) (compactKbuildInvocationMaterialization, error) {
	if b == nil || b.plan == nil {
		return compactKbuildInvocationMaterialization{}, fmt.Errorf(
			"recursive Make materialization for target %q requires an action-plan builder",
			target,
		)
	}
	materialization := compactKbuildInvocationMaterialization{}
	seenOutputs := map[string]bool{}
	seenRoots := map[string]bool{}
	appendOutput := func(pathname string) error {
		pathname = canonicalKbuildRulePath(pathname)
		if err := validatePlanRelativePath("recursive Make materialized output", pathname); err != nil {
			return err
		}
		if !seenOutputs[pathname] {
			seenOutputs[pathname] = true
			// Replays execute from the source command's typed cwd, which can be a
			// nested invocation directory. Verify the staged artifact through the
			// private object-tree root instead of interpreting its graph path
			// relative to that cwd.
			materialization.outputs = append(materialization.outputs, "${work:root}/"+pathname)
		}
		return nil
	}
	appendRoot := func(input compactKbuildRuleInput) {
		identity := fmt.Sprintf("%s\x00%d", input.producer, input.slot)
		if !seenRoots[identity] {
			seenRoots[identity] = true
			input.objectTree = true
			materialization.roots = append(materialization.roots, input)
		}
	}

	target = canonicalKbuildRulePath(target)
	appendSelectedRoot := func(selection compactKbuildSelectionKey) error {
		artifact := CompactKbuildVisibleArtifact{
			Path: selection.target, Profile: selection.profile, Target: selection.target,
		}
		input, err := b.exactObjectTreeArtifactInput(artifact)
		if err != nil {
			return fmt.Errorf(
				"working object-tree target %q recursive invocation %q terminal %s: %w",
				target, dependency.Profile, compactKbuildSelectionKeyString(selection), err,
			)
		}
		appendRoot(input)
		return appendOutput(selection.target)
	}
	selectedRootIsMaterialized := func(selection compactKbuildSelectionKey) (bool, error) {
		artifact := CompactKbuildVisibleArtifact{
			Path: selection.target, Profile: selection.profile, Target: selection.target,
		}
		owner, err := b.selectionGraph.compactKbuildVisibleArtifactOwner(artifact)
		if err != nil {
			return false, err
		}
		_, materialized := b.selectionGraph.materializedProducers[owner]
		return materialized, nil
	}
	appendTerminals := func(skipTargets map[string]bool) error {
		if b == nil || b.selectionGraph == nil {
			return fmt.Errorf(
				"working object-tree target %q recursive invocation %q has no exact selection graph",
				target, dependency.Profile,
			)
		}
		terminals, err := b.selectionGraph.compactKbuildTerminalRecipeSelections(
			b.metadata, dependency.Profile, b.planContext().Stage,
		)
		if err != nil {
			return fmt.Errorf(
				"working object-tree target %q recursive invocation %q terminals: %w",
				target, dependency.Profile, err,
			)
		}
		for _, terminal := range terminals {
			skippedProducer := false
			peerOutputs := map[string]bool{}
			members := b.selectionGraph.compactKbuildGroupedSelectionMembers(terminal)
			for _, member := range members {
				pathname := canonicalKbuildRulePath(member.target)
				if skipTargets[pathname] {
					skippedProducer = true
				} else if pathname != "" {
					peerOutputs[pathname] = true
				}
				producer := b.selectionGraph.materializedProducers[member]
				node, materialized := compactKbuildPlanNode(b.plan, producer)
				if !materialized {
					continue
				}
				if slices.ContainsFunc(node.Outputs, func(output ActionPlanOutput) bool {
					return skipTargets[canonicalKbuildRulePath(output.Path)]
				}) {
					skippedProducer = true
				}
				for _, output := range node.Outputs {
					pathname := canonicalKbuildRulePath(output.Path)
					if pathname != "" && !skipTargets[pathname] {
						peerOutputs[pathname] = true
					}
				}
			}
			if skippedProducer {
				if len(peerOutputs) != 0 {
					return fmt.Errorf(
						"working object-tree target %q recursive invocation %q terminal %s overwrites parent paths %q but shares its producer with non-overwritten outputs %q",
						target, dependency.Profile, compactKbuildSelectionKeyString(terminal),
						slices.Sorted(maps.Keys(skipTargets)), slices.Sorted(maps.Keys(peerOutputs)),
					)
				}
				continue
			}
			if err := appendSelectedRoot(terminal); err != nil {
				return err
			}
		}
		return nil
	}

	if b != nil && b.selectionGraph != nil {
		parentPaths := map[string]bool{}
		if target != "" {
			parentPaths[target] = true
		}
		appendParentSelection := func(selection compactKbuildSelectionKey) {
			if selection.profile != profile.Name {
				return
			}
			for _, member := range b.selectionGraph.compactKbuildGroupedSelectionMembers(selection) {
				if pathname := canonicalKbuildRulePath(member.target); pathname != "" {
					parentPaths[pathname] = true
				}
			}
		}
		if b.selectionBound {
			appendParentSelection(b.selection)
		}
		for _, parentTarget := range []string{target, canonicalKbuildRulePath(dependency.Target)} {
			if selection, selected := b.selectionGraph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
				profile: profile.Name, target: parentTarget,
			}]; selected {
				appendParentSelection(selection)
			}
		}
		overwrittenParentPaths := map[string]bool{}
		for _, parentPath := range slices.Sorted(maps.Keys(parentPaths)) {
			artifact, ok := b.selectionGraph.compactKbuildInitialVisibleArtifact(dependency.Profile, parentPath)
			if ok && artifact == (CompactKbuildVisibleArtifact{
				Path: parentPath, Profile: profile.Name, Target: parentPath,
			}) {
				overwrittenParentPaths[parentPath] = true
				if err := appendOutput(parentPath); err != nil {
					return compactKbuildInvocationMaterialization{}, err
				}
			}
		}
		if len(overwrittenParentPaths) != 0 {
			// The parent script materializes the version visible when the child
			// starts. The recursive invocation overwrites those same regular files,
			// so the proxy verifies the parent outputs but the parent action does
			// not acquire a self-input edge. This comparison covers every exact
			// grouped peer produced by the parent, not only the representative used
			// to lower the shared action.
			if err := appendTerminals(overwrittenParentPaths); err != nil {
				return compactKbuildInvocationMaterialization{}, err
			}
			return materialization, nil
		}
	}

	unresolvedGoals := []string{}
	for _, rawGoal := range dependency.Goals {
		goal := compactKbuildGraphTargetPath(rawGoal)
		directoryGoal := goal == "." || strings.HasSuffix(goal, "/")
		validatedGoal := strings.TrimSuffix(goal, "/")
		if goal != "." {
			if err := validatePlanRelativePath("recursive Make goal", validatedGoal); err != nil {
				return compactKbuildInvocationMaterialization{}, err
			}
		}
		if directoryGoal {
			// A trailing slash (or the invocation root itself) is a Make dispatch
			// goal, not a regular file the replay proxy can verify.
			unresolvedGoals = append(unresolvedGoals, goal)
			continue
		}
		if goal == "" {
			return compactKbuildInvocationMaterialization{}, fmt.Errorf(
				"working object-tree target %q recursive invocation %q has an empty goal",
				target, dependency.Profile,
			)
		}
		if b != nil && b.selectionGraph != nil {
			selection, selected := b.selectionGraph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
				profile: dependency.Profile, target: goal,
			}]
			if selected {
				materialized, err := selectedRootIsMaterialized(selection)
				if err != nil {
					return compactKbuildInvocationMaterialization{}, err
				}
				if materialized {
					if err := appendSelectedRoot(selection); err != nil {
						return compactKbuildInvocationMaterialization{}, err
					}
					continue
				}
				// Phony, directory-setup, and other ordering-only selections are
				// retained in the exact graph but intentionally publish no regular
				// file. Complete their invocation through its materialized terminal
				// recipes instead of treating the selected target spelling as output.
				unresolvedGoals = append(unresolvedGoals, goal)
				continue
			}
			if len(b.selectionGraph.outputOwnersByPath[goal]) > 1 {
				unresolvedGoals = append(unresolvedGoals, goal)
				continue
			}
		}
		producer, slot, ok := b.existingProducer(goal)
		if !ok {
			unresolvedGoals = append(unresolvedGoals, goal)
			continue
		}
		appendRoot(compactKbuildRuleInput{
			path: goal, producer: producer, slot: slot, objectTree: true,
		})
		if err := appendOutput(goal); err != nil {
			return compactKbuildInvocationMaterialization{}, err
		}
	}
	if len(unresolvedGoals) == 0 {
		return materialization, nil
	}
	if b == nil || b.selectionGraph == nil {
		return compactKbuildInvocationMaterialization{}, fmt.Errorf(
			"working object-tree target %q recursive invocation %q goals %q have no materialized producer",
			target, dependency.Profile, unresolvedGoals,
		)
	}
	if err := appendTerminals(nil); err != nil {
		return compactKbuildInvocationMaterialization{}, err
	}
	return materialization, nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildSourceScriptCommandReplays(
	profile CompactKbuildProfile,
	consumerTarget string,
	targets ...string,
) ([]ActionRecipeCommandReplay, error) {
	targetSet := map[string]bool{}
	for _, target := range targets {
		if target = canonicalKbuildRulePath(target); target != "" {
			targetSet[target] = true
		}
	}
	invocations := []ActionRecipeCommandReplayInvocation{}
	invocationByArguments := map[string]int{}
	for _, dependency := range profile.TargetInvocationDependencies {
		if !targetSet[canonicalKbuildRulePath(dependency.Target)] {
			continue
		}
		if len(dependency.ReplayArguments) == 0 {
			return nil, fmt.Errorf(
				"source-script targets %q recursive invocation %q has no replay argv",
				slices.Sorted(maps.Keys(targetSet)), dependency.Profile,
			)
		}
		arguments := make([]string, len(dependency.ReplayArguments))
		for index, argument := range dependency.ReplayArguments {
			arguments[index] = compactKbuildSourceScriptReplayValue(profile, argument)
		}
		materialization, err := b.compactKbuildInvocationDependencyMaterialization(
			consumerTarget, profile, dependency,
		)
		if err != nil {
			return nil, err
		}
		key := strings.Join(arguments, "\x00")
		if invocationIndex, seen := invocationByArguments[key]; seen {
			outputs := append(invocations[invocationIndex].Outputs, materialization.outputs...)
			sort.Strings(outputs)
			invocations[invocationIndex].Outputs = slices.Compact(outputs)
			continue
		}
		invocationByArguments[key] = len(invocations)
		invocations = append(invocations, ActionRecipeCommandReplayInvocation{
			Arguments: arguments,
			Outputs:   materialization.outputs,
		})
	}
	if len(invocations) == 0 {
		return nil, nil
	}
	return []ActionRecipeCommandReplay{{Name: CompactKbuildRecursiveMakeReplayName, Invocations: invocations}}, nil
}

// compactKbuildSourceScriptEnvironment returns GNU Make's exact exported
// target environment, with inline recipe assignments taking precedence just
// as they do for an ordinary shell recipe. Configured executable values are
// identity-bound action-role proxies; object-tree markers point at the private
// writable working root rather than a prior-stage immutable TreeArtifact.
func compactKbuildSourceScriptEnvironment(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildSourceScriptEnvironmentWithResolution(
		target, match, inputs, inline, usage, expectedScope, configured, true,
	)
}

func compactKbuildSourceScriptEnvironmentSymbolic(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildSourceScriptEnvironmentWithResolution(
		target, match, inputs, inline, usage, expectedScope, configured, false,
	)
}

func compactKbuildSourceScriptEnvironmentWithResolution(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	usage compactKbuildSourceScriptEnvironmentUsage,
	expectedScope string,
	configured []KbuildActionRoleRef,
	resolveSymbolic bool,
) (map[string]string, []string, error) {
	if match.capturedEnvironment != nil {
		effectiveUsage := compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}}
		effectiveUsage.merge(usage)
		effectiveUsage.merge(match.capturedEnvironmentUsage)
		captured := maps.Clone(match.capturedEnvironment)
		var err error
		if resolveSymbolic {
			captured, err = compactKbuildResolvedCapturedEnvironment(match.profile, captured)
		}
		if err != nil {
			return nil, nil, err
		}
		environment, _, err := compactKbuildProjectedSourceScriptEnvironmentValues(
			match.profile, captured, inline, effectiveUsage,
		)
		if err != nil {
			return nil, nil, err
		}
		environment, roles, err := compactKbuildRewriteSourceScriptEnvironment(
			environment, expectedScope, configured,
		)
		if err != nil {
			return nil, nil, err
		}
		for name, value := range environment {
			value = compactKbuildFinalizeRootedActionRecipeText(value)
			environment[name] = compactKbuildSourceScriptWorkingValue(value)
		}
		return environment, roles, nil
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
	if err != nil {
		return nil, nil, err
	}
	injected = compactKbuildActionTreeInjections(injected)
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return nil, nil, err
	}
	environment, roles, err := compactKbuildSourceScriptExportedEnvironmentForMakeTargetWithResolution(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected, inline, usage, expectedScope,
		configured, resolveSymbolic,
	)
	if err != nil {
		return nil, nil, err
	}
	for name, value := range environment {
		value = compactKbuildFinalizeRootedActionRecipeText(value)
		environment[name] = compactKbuildSourceScriptWorkingValue(value)
	}
	return environment, roles, nil
}

// compactKbuildActionEnvironment projects GNU Make's exported target
// environment onto an ordinary configured-tool action. Role-free exports are
// source-owned action state. Inline assignments remain authoritative, while
// unrelated role-bearing exports stay out of the action capability set.
func compactKbuildActionEnvironment(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildActionEnvironmentWithResolution(
		target, match, inputs, inline, expectedScope, configured, true,
	)
}

func compactKbuildActionEnvironmentSymbolic(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
) (map[string]string, []string, error) {
	return compactKbuildActionEnvironmentWithResolution(
		target, match, inputs, inline, expectedScope, configured, false,
	)
}

func compactKbuildActionEnvironmentWithResolution(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	inline map[string]string,
	expectedScope string,
	configured []KbuildActionRoleRef,
	resolveSymbolic bool,
) (map[string]string, []string, error) {
	var environment map[string]string
	var err error
	if match.capturedEnvironment != nil {
		captured := maps.Clone(match.capturedEnvironment)
		if resolveSymbolic {
			captured, err = compactKbuildResolvedCapturedEnvironment(match.profile, captured)
		}
		if err == nil {
			environment, _, err = compactKbuildProjectedSourceScriptEnvironmentValues(
				match.profile, captured, inline, match.capturedEnvironmentUsage,
			)
		}
	} else {
		var injected map[string]string
		injected, err = compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
		if err == nil {
			injected = compactKbuildActionTreeInjections(injected)
			var automatic compactKbuildAutomaticContext
			automatic, err = compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
			if err == nil {
				environment, _, err = compactKbuildProjectedSourceScriptEnvironmentForMakeTargetWithResolution(
					match.profile, target, match.lookupTarget, automatic.target,
					automatic.stem, automatic.normal, automatic.order, injected, inline,
					compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}}, resolveSymbolic,
				)
			}
		}
	}
	if err != nil {
		return nil, nil, err
	}
	used := map[string]bool{}
	for _, name := range sortedStringMapKeys(environment) {
		rewritten, roles, err := rewriteKbuildActionRoleRefs(
			environment[name], expectedScope, configured, true,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("Kbuild action effective variable %s: %w", name, err)
		}
		if name == "MAKE" && rewritten == CompactKbuildRecursiveMakeProvenanceToken {
			delete(environment, name)
			continue
		}
		environment[name] = compactKbuildFinalizeRootedActionRecipeText(rewritten)
		for _, role := range roles {
			used[role] = true
		}
	}
	roles := make([]string, 0, len(used))
	for role := range used {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return environment, roles, nil
}

// compactKbuildConfigProjectionBaselineInput returns the immutable Kconfig
// source behind one resolved object-tree projection when its selected Kbuild
// writer cannot provide the current working-tree baseline. Prehost, bootstrap,
// and host actions run before the preparation tree exists, so they always use
// the source projection. Prep and target actions prefer a materialized exact
// owner, but can be planned before an equivalent filechk writer because these
// config paths are existing inputs to Make rather than generated-file
// dependencies. In that case the resolved config source is already available
// and is the authoritative baseline content.
//
// Keep this exception local to the six Kconfig-owned projections. Ordinary
// generated artifacts continue to require exact selection ownership through
// existingInput.
func (b *compactKbuildRulePlanBuilder) compactKbuildConfigProjectionBaselineInput(
	pathname string,
) (compactKbuildRuleInput, bool, error) {
	stage := b.planContext().Stage
	inputPath := ""
	for _, projection := range resolvedConfigProjections() {
		if projection.output == pathname {
			inputPath = projection.input
			break
		}
	}
	if inputPath == "" {
		return compactKbuildRuleInput{}, false, nil
	}
	if stage == "prep" || stage == "target" {
		if !b.selectionBound || b.selectionGraph == nil {
			return compactKbuildRuleInput{}, false, nil
		}
		owner, selected, err := b.selectionGraph.compactKbuildSelectionPathOwner(b.selection, pathname)
		if err != nil {
			return compactKbuildRuleInput{}, false, err
		}
		if selected {
			if _, materialized := b.selectionGraph.materializedProducers[owner]; materialized {
				return compactKbuildRuleInput{}, false, nil
			}
		}
	} else if stage != "prehost" && stage != "bootstrap" && stage != "host" {
		return compactKbuildRuleInput{}, false, nil
	}
	if b.plan == nil {
		return compactKbuildRuleInput{}, false, fmt.Errorf("resolved config projection %q requires an action plan", pathname)
	}
	if err := b.plan.ensureSourceLookupIndex(); err != nil {
		return compactKbuildRuleInput{}, false, err
	}
	sourceID, ok := b.plan.sourceIDs[actionPlanLookupKey("config", inputPath)]
	if !ok {
		return compactKbuildRuleInput{}, false, nil
	}
	return compactKbuildRuleInput{
		path: pathname, sourceID: sourceID, objectTree: true,
	}, true, nil
}

// compactKbuildWorkingTreeClosureInputs projects the exact already-planned
// object-tree state into a recipe's private writable root. The invocation's
// initial visible-artifact frontier is authoritative: unrelated preparation
// outputs must not leak into an earlier action merely because they share the
// eventual prep tree. Resolved Kconfig projections are the other baseline;
// they are supplied independently of Make's generated-file frontier and may
// rebase to their immutable config sources before the prep stage.
//
// Direct rule prerequisites seed the ancestor traversal, and recursive Make
// child goals add source-owned edges. Pretarget actions use this closure
// because their object-tree state is split across physical stages; source
// scripts use it at every stage because they may read or mutate arbitrary
// source-owned paths from that exact state.
func (b *compactKbuildRulePlanBuilder) compactKbuildWorkingTreeClosureInputs(
	target string,
	profile CompactKbuildProfile,
	direct []compactKbuildRuleInput,
) ([]compactKbuildRuleInput, error) {
	roots := make([]compactKbuildRuleInput, 0, len(direct))
	for _, input := range direct {
		if input.producer != "" && !input.workingOnly {
			roots = append(roots, input)
		}
	}
	return b.compactKbuildWorkingTreeClosureInputsFromRoots(target, profile, direct, roots)
}

func (b *compactKbuildRulePlanBuilder) compactKbuildWorkingTreeInputFrontier(
	target string,
	profile CompactKbuildProfile,
	direct []compactKbuildRuleInput,
) (compactKbuildInputFrontier, error) {
	roots := make([]compactKbuildRuleInput, 0, len(direct))
	for _, input := range direct {
		if input.producer != "" && !input.workingOnly {
			roots = append(roots, input)
		}
	}
	return b.compactKbuildWorkingTreeInputFrontierFromRoots(target, profile, direct, roots)
}

// compactKbuildWorkingTreeMaterializedCore is the consumer-independent
// projection of one producer-root set. inputSet holds every single-writer
// ordinary output in the same persistent radix store used by serialized action
// plans. Only duplicate-writer paths remain as flat conflict witnesses. A core
// which reaches any path-sensitive archive has neither projection: replayNodes
// instead names the complete DFS-ordered reached-node sequence so live
// archive/source handling remains interleaved with ordinary producer outputs
// exactly as it was during the authoritative traversal. Native-root
// classification, direct inputs, exact frontier ownership, and overwrite
// selection remain outside this cache.
type compactKbuildWorkingTreeMaterializedCore struct {
	inputSet        string
	conflicts       []compactKbuildWorkingTreeMaterializedCorePath
	replayNodes     []compactKbuildWorkingTreeReplayNode
	replayRootOrder string
}

type compactKbuildWorkingTreeMaterializedCorePath struct {
	path     string
	versions []compactKbuildWorkingTreeMaterializedOutput
}

// compactKbuildWorkingTreeMaterializedOutput is intentionally narrower than
// compactKbuildRuleInput. A materialized core may retain only immutable graph
// facts; native, recipe-local, object-tree, order-only, and overwrite flags are
// reconstructed from the concrete consumer query.
type compactKbuildWorkingTreeMaterializedOutput struct {
	path     string
	producer string
	slot     int
}

// compactKbuildWorkingTreeReplayNode retains only local topology and the
// path-sensitive output shape. For an archive-bearing core there is one record
// for every reached node; ordinary records have an empty slot vector. The node
// and its recipe are always reread from the current ActionPlan before
// processNode is invoked.
type compactKbuildWorkingTreeReplayNode struct {
	index uint32
	slots []int
}

func appendCompactKbuildClosureSignatureString(builder *strings.Builder, value string) {
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
}

func compactKbuildClosureRootSetSignature(roots []string) string {
	canonical := append([]string(nil), roots...)
	sort.Strings(canonical)
	return compactKbuildClosureSequenceSignature(canonical)
}

func compactKbuildClosureSequenceSignature(values []string) string {
	var builder strings.Builder
	builder.WriteString(strconv.Itoa(len(values)))
	builder.WriteByte(';')
	for _, value := range values {
		appendCompactKbuildClosureSignatureString(&builder, value)
	}
	return builder.String()
}

func cloneCompactKbuildRuleInputs(inputs []compactKbuildRuleInput) []compactKbuildRuleInput {
	if inputs == nil {
		return nil
	}
	cloned := make([]compactKbuildRuleInput, len(inputs))
	copy(cloned, inputs)
	return cloned
}

func (p *ActionPlan) compactKbuildWorkingTreeMaterializedNodeInputSet(
	index uint32,
) (string, []compactKbuildWorkingTreeMaterializedCorePath, error) {
	if p == nil || int(index) >= len(p.Nodes) {
		return "", nil, nil
	}
	if root, ok := p.workingTreeMaterializedNodeInputSets[index]; ok {
		return root, p.workingTreeMaterializedNodeConflicts[index], nil
	}
	store, err := p.planningActionPlanInputSetStore()
	if err != nil {
		return "", nil, err
	}
	p.workingTreeMaterializedNodeProjections++
	node := p.Nodes[index]
	root := ""
	conflictsByPath := map[string][]compactKbuildWorkingTreeMaterializedOutput{}
	for slot, output := range node.Outputs {
		if output.ObservedPath != "" {
			continue
		}
		entry := ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: output.Path},
			ProducerID: node.ID,
			Slot:       slot,
		}
		if previous, found, lookupErr := store.Lookup(root, entry.Target); lookupErr != nil {
			return "", nil, lookupErr
		} else if found && previous != entry {
			versions := conflictsByPath[output.Path]
			if len(versions) == 0 {
				versions = append(versions, compactKbuildWorkingTreeMaterializedOutput{
					path: output.Path, producer: previous.ProducerID, slot: previous.Slot,
				})
			}
			conflictsByPath[output.Path] = append(versions, compactKbuildWorkingTreeMaterializedOutput{
				path: output.Path, producer: entry.ProducerID, slot: entry.Slot,
			})
			continue
		}
		root, err = store.Insert(root, entry)
		if err != nil {
			return "", nil, err
		}
	}
	conflicts := make([]compactKbuildWorkingTreeMaterializedCorePath, 0, len(conflictsByPath))
	for _, pathname := range slices.Sorted(maps.Keys(conflictsByPath)) {
		versions := conflictsByPath[pathname]
		sort.Slice(versions, func(i, j int) bool {
			if versions[i].producer != versions[j].producer {
				return versions[i].producer < versions[j].producer
			}
			return versions[i].slot < versions[j].slot
		})
		conflicts = append(conflicts, compactKbuildWorkingTreeMaterializedCorePath{
			path: pathname, versions: versions,
		})
		var deleted bool
		root, deleted, err = store.Delete(root, ActionPlanInputSetTarget{
			Kind: ActionPlanInputSetWorkTarget, Path: pathname,
		})
		if err != nil {
			return "", nil, err
		}
		if !deleted {
			return "", nil, fmt.Errorf("node %s duplicate output path %q has no persistent representative", node.ID, pathname)
		}
	}
	if len(p.workingTreeMaterializedNodeInputSets) < maximumWorkingTreeMaterializedNodeEntries {
		if p.workingTreeMaterializedNodeInputSets == nil {
			p.workingTreeMaterializedNodeInputSets = map[uint32]string{}
		}
		p.workingTreeMaterializedNodeInputSets[index] = root
		if len(conflicts) != 0 {
			if p.workingTreeMaterializedNodeConflicts == nil {
				p.workingTreeMaterializedNodeConflicts = map[uint32][]compactKbuildWorkingTreeMaterializedCorePath{}
			}
			p.workingTreeMaterializedNodeConflicts[index] = conflicts
		}
	}
	return root, conflicts, nil
}

func (p *ActionPlan) retainCompactKbuildWorkingTreeMaterializedCore(
	key string,
	core *compactKbuildWorkingTreeMaterializedCore,
) {
	if p == nil || core == nil {
		return
	}
	if _, exists := p.workingTreeMaterializedCoreCache[key]; exists {
		return
	}
	inputCount := 0
	for _, entry := range core.conflicts {
		inputCount += len(entry.versions)
	}
	for _, replay := range core.replayNodes {
		inputCount += 1 + len(replay.slots)
	}
	retainedKeyBytes := len(key) + len(core.inputSet) + len(core.replayRootOrder)
	if len(p.workingTreeMaterializedCoreCache) >= maximumWorkingTreeMaterializedCoreEntries ||
		inputCount > maximumWorkingTreeMaterializedCoreInputs-p.workingTreeMaterializedCoreInputs ||
		retainedKeyBytes > maximumWorkingTreeMaterializedCoreKeyBytes-p.workingTreeMaterializedCoreKeyBytes {
		return
	}
	if p.workingTreeMaterializedCoreCache == nil {
		p.workingTreeMaterializedCoreCache = map[string]*compactKbuildWorkingTreeMaterializedCore{}
	}
	p.workingTreeMaterializedCoreCache[key] = core
	p.workingTreeMaterializedCoreInputs += inputCount
	p.workingTreeMaterializedCoreKeyBytes += retainedKeyBytes
}

// replayCompactKbuildWorkingTreeMaterializedCore validates the complete live
// path-sensitive shape before invoking any callbacks. Ordinary cores are
// consumer-independent only while their materialized targets do not intersect
// consumer-local state. On an intersection we deliberately replay the live
// topology so duplicate-path error ordering remains GNU-Make compatible
// without retaining a flattened DFS output list in the cache.
func (p *ActionPlan) replayCompactKbuildWorkingTreeMaterializedCore(
	core *compactKbuildWorkingTreeMaterializedCore,
	roots []string,
	consumerPaths map[string]compactKbuildRuleInput,
	processNode func(uint32) error,
) (bool, error) {
	if p == nil || core == nil {
		return false, nil
	}
	if len(core.replayNodes) == 0 {
		if core.replayRootOrder != "" {
			return false, nil
		}
		store, err := p.planningActionPlanInputSetStore()
		if err != nil {
			return false, err
		}
		if err := store.ValidateRoot(core.inputSet); err != nil {
			return false, nil
		}
		previousPath := ""
		conflictPaths := make(map[string]bool, len(core.conflicts))
		for _, conflict := range core.conflicts {
			if conflict.path == "" || len(conflict.versions) < 2 ||
				(previousPath != "" && conflict.path <= previousPath) {
				return false, nil
			}
			previousPath = conflict.path
			conflictPaths[conflict.path] = true
			target := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: conflict.path}
			if _, found, lookupErr := store.Lookup(core.inputSet, target); lookupErr != nil || found {
				return false, lookupErr
			}
			for _, version := range conflict.versions {
				if version.path != conflict.path || version.producer == "" || version.slot < 0 {
					return false, nil
				}
			}
		}
		for pathname := range consumerPaths {
			if conflictPaths[pathname] {
				return false, nil
			}
			target := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname}
			if _, found, lookupErr := store.Lookup(core.inputSet, target); lookupErr != nil {
				return false, lookupErr
			} else if found {
				return false, nil
			}
		}
		return true, nil
	}
	if core.inputSet != "" || len(core.conflicts) != 0 ||
		core.replayRootOrder != compactKbuildClosureSequenceSignature(roots) {
		return false, nil
	}
	seen := make(map[uint32]bool, len(core.replayNodes))
	hasPathSensitiveArchive := false
	for _, replay := range core.replayNodes {
		if int(replay.index) >= len(p.Nodes) || seen[replay.index] {
			return false, nil
		}
		seen[replay.index] = true
		liveSlots := p.pathSensitiveArchiveSlots(p.Nodes[replay.index])
		if !slices.Equal(replay.slots, liveSlots) {
			return false, nil
		}
		hasPathSensitiveArchive = hasPathSensitiveArchive || len(liveSlots) != 0
	}
	if !hasPathSensitiveArchive {
		return false, nil
	}
	if processNode != nil {
		for _, replay := range core.replayNodes {
			if err := processNode(replay.index); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// compactKbuildWorkingTreeMaterializedCoreWhileProcessing preserves the old
// first-computation error order by invoking processNode in the original DFS
// position while it assembles a cache candidate. An archive-free cache hit
// needs no callbacks. An archive-bearing hit invokes callbacks for the complete
// DFS-ordered reached-node sequence because archive source insertion and
// ordinary output insertion do not commute.
func (p *ActionPlan) compactKbuildWorkingTreeMaterializedCoreWhileProcessing(
	roots []string,
	processNode func(uint32) error,
) (*compactKbuildWorkingTreeMaterializedCore, bool, error) {
	return p.compactKbuildWorkingTreeMaterializedCoreForConsumer(roots, nil, processNode)
}

func (p *ActionPlan) compactKbuildWorkingTreeMaterializedCoreForConsumer(
	roots []string,
	consumerPaths map[string]compactKbuildRuleInput,
	processNode func(uint32) error,
) (*compactKbuildWorkingTreeMaterializedCore, bool, error) {
	if p == nil {
		return nil, false, fmt.Errorf("working object-tree closure requires an action plan")
	}
	key := compactKbuildClosureRootSetSignature(roots)
	if core, ok := p.workingTreeMaterializedCoreCache[key]; ok {
		valid, replayErr := p.replayCompactKbuildWorkingTreeMaterializedCore(core, roots, consumerPaths, processNode)
		if replayErr != nil {
			return nil, false, replayErr
		}
		if valid {
			return core, false, nil
		}
	}
	if p.familyPlanningCache != nil {
		core, hit, lookupErr := p.familyPlanningCache.lookupMaterializedCore(p, key)
		if lookupErr == nil && hit {
			valid, replayErr := p.replayCompactKbuildWorkingTreeMaterializedCore(core, roots, consumerPaths, processNode)
			if replayErr != nil {
				return nil, false, replayErr
			}
			if valid {
				p.retainCompactKbuildWorkingTreeMaterializedCore(key, core)
				return core, false, nil
			}
		}
	}
	p.workingTreeMaterializedCoreComputations++
	store, err := p.planningActionPlanInputSetStore()
	if err != nil {
		return nil, false, err
	}
	materializedRoot := ""
	conflictsByPath := map[string][]compactKbuildWorkingTreeMaterializedOutput{}
	rememberConflict := func(entry ActionPlanInputSetEntry) {
		versions := conflictsByPath[entry.Target.Path]
		for _, existing := range versions {
			if existing.producer == entry.ProducerID && existing.slot == entry.Slot {
				return
			}
		}
		conflictsByPath[entry.Target.Path] = append(versions, compactKbuildWorkingTreeMaterializedOutput{
			path: entry.Target.Path, producer: entry.ProducerID, slot: entry.Slot,
		})
	}
	mergeNodeInputSet := func(nodeRoot string) error {
		var unionErr error
		materializedRoot, unionErr = store.Union(materializedRoot, nodeRoot, func(
			target ActionPlanInputSetTarget,
			left, right ActionPlanInputSetEntry,
		) (ActionPlanInputSetEntry, error) {
			if left == right {
				return left, nil
			}
			rememberConflict(left)
			rememberConflict(right)
			return left, nil
		})
		return unionErr
	}
	visited := actionPlanNodeVisitSet{}
	replayOrder := []compactKbuildWorkingTreeReplayNode{}
	hasPathSensitiveArchive := false
	for _, producer := range roots {
		err := p.walkCompactKbuildWorkingTreeTopology(producer, visited, func(index uint32) error {
			if p.workingTreeMaterializedReachedNodes == nil {
				p.workingTreeMaterializedReachedNodes = actionPlanNodeVisitSet{}
			}
			p.workingTreeMaterializedReachedNodes.add(index)
			node := p.Nodes[index]
			slots := p.pathSensitiveArchiveSlots(node)
			replayOrder = append(replayOrder, compactKbuildWorkingTreeReplayNode{
				index: index,
				slots: append([]int(nil), slots...),
			})
			if len(slots) != 0 {
				hasPathSensitiveArchive = true
			} else {
				nodeRoot, nodeConflicts, rootErr := p.compactKbuildWorkingTreeMaterializedNodeInputSet(index)
				if rootErr != nil {
					return rootErr
				}
				if rootErr := mergeNodeInputSet(nodeRoot); rootErr != nil {
					return rootErr
				}
				for _, conflict := range nodeConflicts {
					target := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: conflict.path}
					if existing, found, lookupErr := store.Lookup(materializedRoot, target); lookupErr != nil {
						return lookupErr
					} else if found {
						rememberConflict(existing)
					}
					for _, version := range conflict.versions {
						rememberConflict(ActionPlanInputSetEntry{
							Target: target, ProducerID: version.producer, Slot: version.slot,
						})
					}
				}
			}
			if processNode != nil {
				return processNode(index)
			}
			return nil
		})
		if err != nil {
			return nil, true, err
		}
	}
	var replayNodes []compactKbuildWorkingTreeReplayNode
	if hasPathSensitiveArchive {
		replayNodes = replayOrder
		// The callbacks above already preserved exact interleaving on this live
		// computation. Future hits must rebuild that same state from the complete
		// replay order rather than merging an independently canonicalized path set.
		materializedRoot = ""
		conflictsByPath = nil
	}
	core := &compactKbuildWorkingTreeMaterializedCore{
		inputSet:    materializedRoot,
		conflicts:   make([]compactKbuildWorkingTreeMaterializedCorePath, 0, len(conflictsByPath)),
		replayNodes: replayNodes,
	}
	if len(replayNodes) != 0 {
		core.replayRootOrder = compactKbuildClosureSequenceSignature(roots)
	}
	for _, pathname := range slices.Sorted(maps.Keys(conflictsByPath)) {
		core.inputSet, _, err = store.Delete(
			core.inputSet,
			ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
		)
		if err != nil {
			return nil, true, err
		}
		versions := conflictsByPath[pathname]
		sort.Slice(versions, func(i, j int) bool {
			if versions[i].producer != versions[j].producer {
				return versions[i].producer < versions[j].producer
			}
			return versions[i].slot < versions[j].slot
		})
		core.conflicts = append(core.conflicts, compactKbuildWorkingTreeMaterializedCorePath{
			path:     pathname,
			versions: versions,
		})
	}
	p.retainCompactKbuildWorkingTreeMaterializedCore(key, core)
	if p.familyPlanningCache != nil {
		p.familyPlanningCache.storeMaterializedCore(key, core)
	}
	return core, true, nil
}

// compactKbuildWorkingTreeClosureInputsFromRoots keeps every native direct
// input native while preserving workingOnly on an already-expanded closure.
// It traverses only the producers whose filesystem representation requires
// their ancestors. Thin archives use this narrower form: unrelated ordinary
// operands remain direct inputs without pulling their whole producer lineage
// into the writable tree.
func (b *compactKbuildRulePlanBuilder) compactKbuildWorkingTreeClosureInputsFromRoots(
	target string,
	profile CompactKbuildProfile,
	direct []compactKbuildRuleInput,
	traversalRoots []compactKbuildRuleInput,
) ([]compactKbuildRuleInput, error) {
	return b.compactKbuildWorkingTreeClosureInputsFromRootsImpl(
		target, profile, direct, traversalRoots, nil,
	)
}

func (b *compactKbuildRulePlanBuilder) compactKbuildWorkingTreeInputFrontierFromRoots(
	target string,
	profile CompactKbuildProfile,
	direct []compactKbuildRuleInput,
	traversalRoots []compactKbuildRuleInput,
) (compactKbuildInputFrontier, error) {
	sharedRoot := ""
	inputs, err := b.compactKbuildWorkingTreeClosureInputsFromRootsImpl(
		target, profile, direct, traversalRoots, &sharedRoot,
	)
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	frontier, err := compactKbuildInputFrontierFromResolved(b.plan, inputs)
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	if sharedRoot == "" {
		return frontier, nil
	}
	store, err := b.plan.planningActionPlanInputSetStore()
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	frontier.inputSet, err = store.Union(sharedRoot, frontier.inputSet, func(
		target ActionPlanInputSetTarget,
		left, right ActionPlanInputSetEntry,
	) (ActionPlanInputSetEntry, error) {
		if left != right {
			return ActionPlanInputSetEntry{}, fmt.Errorf(
				"persistent working frontier target %s has conflicting provenance",
				actionPlanInputSetTargetDescription(target),
			)
		}
		return left, nil
	})
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	return frontier, nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildWorkingTreeClosureInputsFromRootsImpl(
	target string,
	profile CompactKbuildProfile,
	direct []compactKbuildRuleInput,
	traversalRoots []compactKbuildRuleInput,
	sharedInputSet *string,
) ([]compactKbuildRuleInput, error) {
	if b == nil || b.plan == nil {
		return nil, fmt.Errorf("working object-tree closure requires an action plan")
	}
	b.plan.ensureNodeLookupIndexes()
	roots := []string{}
	seenRoot := map[string]bool{}
	type producerOutput struct {
		producer string
		slot     int
	}
	nativeRoots := map[producerOutput]bool{}
	addRoot := func(producer string) {
		if producer != "" && !seenRoot[producer] {
			seenRoot[producer] = true
			roots = append(roots, producer)
		}
	}
	for _, input := range direct {
		if input.producer != "" && !input.workingOnly {
			nativeRoots[producerOutput{producer: input.producer, slot: input.slot}] = true
		}
	}
	for _, input := range traversalRoots {
		if input.producer == "" {
			continue
		}
		addRoot(input.producer)
	}
	baseline := []compactKbuildRuleInput{}
	visibleByPath := map[string]bool{}
	if b.planContext().UsesInitialObjectTree {
		for _, artifact := range b.initialObjectTreeArtifacts {
			pathname := canonicalKbuildRulePath(artifact.Path)
			if pathname == "" || pathname != artifact.Path || !compactKbuildProfileHasInitialVisibleArtifact(profile, artifact) {
				return nil, fmt.Errorf(
					"profile %q initial object-tree artifact %#v is invalid or outside its exact visible frontier",
					profile.Name, artifact,
				)
			}
			if err := validatePlanRelativePath("initial visible Kbuild artifact", pathname); err != nil {
				return nil, err
			}
			if visibleByPath[pathname] {
				continue
			}
			visibleByPath[pathname] = true
			input, err := b.exactObjectTreeArtifactInput(artifact)
			if err != nil {
				return nil, fmt.Errorf(
					"profile %q initial visible artifact %q: %w",
					profile.Name, pathname, err,
				)
			}
			input.workingOnly = true
			// A selected invocation may consume the exact version of its own
			// output path left by a predecessor and then replace it in a different
			// immutable Bazel stage. That byte-state dependency is an overwrite,
			// even though the writers cannot collide in one physical output tree.
			// Require both source-recorded initial visibility and registered output
			// ownership so an incidental same-named working-tree file cannot become
			// terminal producer lineage.
			if b.selectionBound &&
				pathname == canonicalKbuildRulePath(target) &&
				b.selectionGraph.compactKbuildSelectionOwnsPath(b.selection, pathname) {
				input.overwriteLineage = true
			}
			baseline = append(baseline, input)
		}
	}
	// Generated source references are independent from the invocation's
	// initial visible frontier. A source script may name one exact selected
	// producer through immutable source text even when it otherwise observes no
	// pre-existing object-tree state.
	for _, artifact := range b.generatedObjectTreeArtifacts {
		pathname := canonicalKbuildRulePath(artifact.Path)
		if pathname == "" || pathname != artifact.Path {
			return nil, fmt.Errorf("profile %q generated object-tree artifact %#v is invalid", profile.Name, artifact)
		}
		if err := validatePlanRelativePath("generated source Kbuild artifact", pathname); err != nil {
			return nil, err
		}
		if visibleByPath[pathname] {
			continue
		}
		visibleByPath[pathname] = true
		input, err := b.exactObjectTreeArtifactInput(artifact)
		if err != nil {
			return nil, fmt.Errorf(
				"profile %q generated source artifact %q: %w",
				profile.Name, pathname, err,
			)
		}
		input.workingOnly = true
		baseline = append(baseline, input)
	}

	// Kconfig replay owns this small, explicit object-tree interface. Unlike
	// generated Kbuild artifacts, a projection may not have run yet at a
	// pretarget stage, so existingInput is allowed to peel its one-file copy to
	// the immutable config source. A preconfigured object tree exposes the same
	// logical path directly through the selected profile's source namespace.
	for _, pathname := range ResolvedConfigProjectionOutputs() {
		if visibleByPath[pathname] {
			continue
		}
		input, visible, err := b.compactKbuildConfigProjectionBaselineInput(pathname)
		if err == nil && !visible {
			input, visible, err = b.existingInput(pathname)
		}
		if err != nil {
			return nil, fmt.Errorf("resolved config projection %q: %w", pathname, err)
		}
		sourceExists := false
		if !visible {
			evidence, sourceErr := b.metadata.compactKbuildGraphSourcePathExists(profile, pathname)
			sourceExists, err = evidence.exists, sourceErr
			if err != nil {
				return nil, fmt.Errorf("resolved config source %q: %w", pathname, err)
			}
		}
		if !visible && sourceExists {
			if b.metadata == nil {
				return nil, fmt.Errorf("resolved config source %q requires source-derived Kbuild metadata", pathname)
			}
			sourceID, sourceErr := b.metadata.ensureActionPlanSource(b.plan, pathname)
			if sourceErr != nil {
				return nil, fmt.Errorf("resolved config source %q: %w", pathname, sourceErr)
			}
			input = compactKbuildRuleInput{path: pathname, sourceID: sourceID, objectTree: true}
			visible = true
		}
		if visible {
			input.workingOnly = true
			baseline = append(baseline, input)
		}
	}
	for _, dependency := range profile.TargetInvocationDependencies {
		if canonicalKbuildRulePath(dependency.Target) != canonicalKbuildRulePath(target) {
			continue
		}
		materialization, err := b.compactKbuildInvocationDependencyMaterialization(
			target, profile, dependency,
		)
		if err != nil {
			return nil, err
		}
		for _, root := range materialization.roots {
			addRoot(root.producer)
			nativeRoots[producerOutput{producer: root.producer, slot: root.slot}] = true
		}
	}
	preferInput := func(preferred, other compactKbuildRuleInput) compactKbuildRuleInput {
		preferred.overwriteLineage = preferred.overwriteLineage || other.overwriteLineage
		// The selected producer is still a native prerequisite when either
		// equivalent graph position was a native root. Preserve the native
		// root's order-only classification in that case.
		if preferred.workingOnly && !other.workingOnly {
			preferred.workingOnly = false
			preferred.orderOnly = other.orderOnly
		} else if !preferred.workingOnly && !other.workingOnly {
			preferred.orderOnly = preferred.orderOnly && other.orderOnly
		}
		return preferred
	}
	byPath := make(map[string]compactKbuildRuleInput, len(direct)+len(baseline))
	producerVersions := map[string][]compactKbuildRuleInput{}
	baselineProducers := map[string]map[string]bool{}
	directProducers := map[string]map[string]bool{}
	directImmutablePaths := map[string]bool{}
	for _, input := range direct {
		if input.path != "" && input.producer == "" && !input.workingOnly {
			directImmutablePaths[input.path] = true
		}
	}
	rememberProducerVersion := func(input compactKbuildRuleInput) {
		if input.path == "" || input.producer == "" {
			return
		}
		versions := producerVersions[input.path]
		for index, existing := range versions {
			if existing.producer == input.producer && existing.slot == input.slot {
				// The same graph output can be rediscovered through ancestry after it
				// was supplied directly by an earlier command in this recipe. Keep the
				// stronger consumer-local provenance independent of discovery order.
				if input.recipeLocal && !existing.recipeLocal {
					versions[index].recipeLocal = true
					producerVersions[input.path] = versions
				}
				return
			}
		}
		producerVersions[input.path] = append(versions, input)
	}
	seedInputs := func(
		inputs []compactKbuildRuleInput,
		provenance map[string]map[string]bool,
		includeWorkingOnly bool,
	) {
		for _, input := range inputs {
			if input.path == "" {
				continue
			}
			byPath[input.path] = input
			rememberProducerVersion(input)
			if input.producer != "" && (includeWorkingOnly || !input.workingOnly) {
				if provenance[input.path] == nil {
					provenance[input.path] = map[string]bool{}
				}
				provenance[input.path][input.producer] = true
			}
		}
	}
	// A native prerequisite wins over an invocation/config baseline at the
	// same logical pathname.
	seedInputs(baseline, baselineProducers, true)
	seedInputs(direct, directProducers, false)
	type producerLineage struct {
		descendant string
		ancestor   string
	}
	lineage := map[producerLineage]bool{}
	// Dependency materialization above is complete. Share adjacency reads for
	// this call only instead of flattening and sorting every input set again
	// for each ancestor pair. Retain at most 32K records/references.
	producerTraversal := actionPlanProducerTraversal{plan: b.plan, remaining: 1 << 15}
	var lineageInputErr error
	producerDescendsFrom := func(descendant, ancestor string) bool {
		key := producerLineage{descendant: descendant, ancestor: ancestor}
		if result, ok := lineage[key]; ok {
			return result
		}
		result, err := producerTraversal.descendsFrom(descendant, ancestor)
		if err != nil {
			lineageInputErr = err
			return false
		}
		lineage[key] = result
		return result
	}
	// A selected invocation can start from one version of a pathname and then
	// select another writer for that same pathname. Those versions need not be
	// joined by an ActionPlan edge: each producer runs in a private writable
	// tree, while the selected consumer records the exact version to stage. Use
	// that consumer-local ownership after validating producer provenance to keep
	// an earlier snapshot visible to a consumer between two writes. ActionPlan
	// ancestry alone cannot make a target-lifecycle version visible backward to
	// a prep consumer. A path without an exact recorded owner continues to fail
	// closed below.
	recordedPathProducer := func(pathname string) (string, error) {
		if !b.selectionBound || b.selectionGraph == nil {
			return "", nil
		}
		if len(baselineProducers[pathname]) == 0 && len(directProducers[pathname]) == 0 {
			// Working-only ancestry can contain snapshots from actions which the
			// consumer does not observe directly. A same-invocation writer selected
			// elsewhere is not execution provenance for such a historical path and
			// may legitimately be materialized after this consumer.
			return "", nil
		}
		owner, selected, err := b.selectionGraph.compactKbuildSelectionRecordedPathOwner(
			b.selection, pathname,
		)
		if compactKbuildPathOwnerIsUnrecorded(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if !selected {
			return "", nil
		}
		producer, materialized := b.selectionGraph.materializedProducers[owner]
		if !materialized || producer == "" {
			return "", fmt.Errorf(
				"Kbuild selection %s working object-tree path %q exact owner %s has not been materialized",
				compactKbuildSelectionKeyString(b.selection), pathname,
				compactKbuildSelectionKeyString(owner),
			)
		}
		return producer, nil
	}
	resolveProducerVersions := func(
		pathname string,
		versions []compactKbuildRuleInput,
	) (compactKbuildRuleInput, error) {
		versions = append([]compactKbuildRuleInput(nil), versions...)
		sort.Slice(versions, func(i, j int) bool {
			if versions[i].producer != versions[j].producer {
				return versions[i].producer < versions[j].producer
			}
			return versions[i].slot < versions[j].slot
		})
		for index := 1; index < len(versions); index++ {
			if versions[index-1].producer == versions[index].producer {
				return compactKbuildRuleInput{}, fmt.Errorf(
					"working object-tree path %q has multiple output slots from producer %q",
					pathname, versions[index].producer,
				)
			}
		}
		recipeLocalVersion := compactKbuildRuleInput{}
		for _, version := range versions {
			if !version.recipeLocal {
				continue
			}
			if recipeLocalVersion.producer != "" {
				return compactKbuildRuleInput{}, fmt.Errorf(
					"working object-tree path %q has multiple recipe-local producers %q and %q",
					pathname, recipeLocalVersion.producer, version.producer,
				)
			}
			recipeLocalVersion = version
		}
		if recipeLocalVersion.producer != "" {
			// The recorded owner describes the pathname at selection entry. Once a
			// physical command in this recipe replaces it, every later command must
			// stage that replacement. The explicit command sequence supplies the
			// execution edge; global overwrite ownership remains unchanged.
			winner := recipeLocalVersion
			for _, version := range versions {
				winner = preferInput(winner, version)
			}
			return winner, nil
		}
		recordedProducer, err := recordedPathProducer(pathname)
		if err != nil {
			return compactKbuildRuleInput{}, err
		}
		recordedVersion := compactKbuildRuleInput{}
		if recordedProducer != "" {
			for _, version := range versions {
				if version.producer == recordedProducer {
					recordedVersion = version
					break
				}
			}
			if recordedVersion.producer == "" {
				return compactKbuildRuleInput{}, fmt.Errorf(
					"working object-tree path %q exact consumer owner producer %q is absent from its materialized closure",
					pathname, recordedProducer,
				)
			}
		}
		if len(versions) == 1 {
			return versions[0], nil
		}
		edges := map[string]map[string]bool{}
		addEdge := func(before, after string) {
			if before == "" || after == "" || before == after {
				return
			}
			if edges[before] == nil {
				edges[before] = map[string]bool{}
			}
			edges[before][after] = true
		}
		for leftIndex, left := range versions {
			for _, right := range versions[leftIndex+1:] {
				sourceWinner, sourceOrdered := "", false
				if b.selectionGraph != nil {
					var orderErr error
					sourceWinner, sourceOrdered, orderErr = b.selectionGraph.compactKbuildSourceOrderedPathProducer(
						pathname, left.producer, right.producer,
					)
					if orderErr != nil {
						return compactKbuildRuleInput{}, orderErr
					}
				}
				leftBaselineOnly := baselineProducers[pathname][left.producer] &&
					!directProducers[pathname][left.producer]
				rightBaselineOnly := baselineProducers[pathname][right.producer] &&
					!directProducers[pathname][right.producer]
				leftBeforeRight := leftBaselineOnly && directProducers[pathname][right.producer]
				rightBeforeLeft := rightBaselineOnly && directProducers[pathname][left.producer]
				if !leftBeforeRight && !rightBeforeLeft {
					leftBeforeRight = producerDescendsFrom(right.producer, left.producer)
					rightBeforeLeft = producerDescendsFrom(left.producer, right.producer)
					if lineageInputErr != nil {
						return compactKbuildRuleInput{}, fmt.Errorf("resolve working object-tree producer lineage: %w", lineageInputErr)
					}
				}
				if !leftBeforeRight && !rightBeforeLeft && sourceOrdered {
					leftBeforeRight = sourceWinner == right.producer
					rightBeforeLeft = sourceWinner == left.producer
				}
				if leftBeforeRight {
					addEdge(left.producer, right.producer)
				}
				if rightBeforeLeft {
					addEdge(right.producer, left.producer)
				}
			}
		}
		reachable := func(from, to string) bool {
			pending := []string{}
			for successor := range edges[from] {
				pending = append(pending, successor)
			}
			seen := map[string]bool{}
			for len(pending) != 0 {
				last := len(pending) - 1
				candidate := pending[last]
				pending = pending[:last]
				if candidate == to {
					return true
				}
				if seen[candidate] {
					continue
				}
				seen[candidate] = true
				for successor := range edges[candidate] {
					pending = append(pending, successor)
				}
			}
			return false
		}
		for _, version := range versions {
			if reachable(version.producer, version.producer) {
				return compactKbuildRuleInput{}, fmt.Errorf(
					"working object-tree path %q has cyclic producer provenance at %q",
					pathname, version.producer,
				)
			}
		}
		if recordedProducer != "" {
			winner := recordedVersion
			for _, version := range versions {
				winner = preferInput(winner, version)
			}
			return winner, nil
		}
		maximal := []compactKbuildRuleInput{}
		for _, candidate := range versions {
			precedesAnother := false
			for _, other := range versions {
				if candidate.producer != other.producer && reachable(candidate.producer, other.producer) {
					precedesAnother = true
					break
				}
			}
			if !precedesAnother {
				maximal = append(maximal, candidate)
			}
		}
		if len(maximal) != 1 {
			producers := make([]string, 0, len(maximal))
			for _, candidate := range maximal {
				producers = append(producers, candidate.producer)
			}
			return compactKbuildRuleInput{}, fmt.Errorf(
				"working object-tree path %q has ambiguous maximal producers %q",
				pathname, producers,
			)
		}
		winner := maximal[0]
		for _, version := range versions {
			winner = preferInput(winner, version)
		}
		return winner, nil
	}
	processOutputCandidate := func(candidate compactKbuildRuleInput) error {
		candidate.workingOnly = !nativeRoots[producerOutput{
			producer: candidate.producer,
			slot:     candidate.slot,
		}]
		rememberProducerVersion(candidate)
		if existing, exists := byPath[candidate.path]; exists {
			if existing.producer == candidate.producer && existing.slot == candidate.slot {
				return nil
			}
			if existing.producer == "" {
				if directImmutablePaths[candidate.path] {
					baselineOnly := baselineProducers[candidate.path][candidate.producer] &&
						!directProducers[candidate.path][candidate.producer]
					if baselineOnly {
						// A native immutable prerequisite is the consumer's current
						// version of this pathname. A generated version seeded by the
						// initial object-tree baseline remains older even when another
						// root also traverses that producer.
						return nil
					}
				}
				if existing.sourceID == "" || !existing.objectTree || !existing.workingOnly {
					return fmt.Errorf(
						"working object-tree path %q producer %q conflicts with immutable source %q",
						candidate.path, candidate.producer, existing.sourceID,
					)
				}
				// Pretarget actions begin with immutable Kconfig source
				// projections. A generated version in the traversed lineage
				// replaces that initial baseline; multiple generated versions
				// are resolved together after the complete closure is known.
				byPath[candidate.path] = preferInput(candidate, existing)
			}
			return nil
		}
		byPath[candidate.path] = candidate
		return nil
	}
	processNode := func(nodeIndex uint32) error {
		node := b.plan.Nodes[nodeIndex]
		producer := node.ID
		// A thin archive can retain a pathname to an immutable object rather
		// than to another node output. Recover those exact logical paths from
		// the archive recipe's source bindings. Do this only for producers
		// carrying archive provenance: ordinary compile ancestors also stage
		// their source text in a private cwd, but that text is not an archive
		// member required by the downstream link.
		if b.plan.hasPathSensitiveArchiveOutput(producer) {
			// Archive source bindings are deliberately live planner state: a
			// path-sensitive marker may be attached after append, and probe-only
			// compaction can release or retain its recipe payload independently.
			// The topology remains reusable, but a complete result containing that
			// policy must be rebuilt from the current recipe on every query.
			recipe, ok := b.plan.Recipes[node.Recipe]
			if !ok {
				return fmt.Errorf("path-sensitive archive producer %q references absent recipe %q", producer, node.Recipe)
			}
			if len(recipe.Sources) != len(node.Sources) {
				return fmt.Errorf(
					"path-sensitive archive producer %q has %d source bindings for %d source edges",
					producer, len(recipe.Sources), len(node.Sources),
				)
			}
			for index, edge := range node.Sources {
				// Only native object/prerequisite source edges can be archive
				// members. Atomic recipes also carry source scripts and the
				// writable object-tree baseline as source edges, but those files
				// merely make the action executable; propagating them as retained
				// members would invent archive contents and can resurrect an
				// obsolete Kconfig baseline after an exact writer has replaced it.
				if edge.Role != "object" && edge.Role != "prerequisite" {
					continue
				}
				pathname := recipe.WorkingInputs["source:"+recipe.Sources[index]]
				if pathname == "" {
					continue
				}
				canonical := canonicalKbuildRulePath(pathname)
				if canonical == "" || canonical != pathname {
					return fmt.Errorf("path-sensitive archive producer %q has invalid source working path %q", producer, pathname)
				}
				if err := validatePlanRelativePath("path-sensitive archive source member", canonical); err != nil {
					return err
				}
				candidate := compactKbuildRuleInput{
					path: canonical, sourceID: edge.SourceID, workingOnly: true,
				}
				if existing, exists := byPath[canonical]; exists {
					if existing.sourceID != candidate.sourceID || existing.producer != "" {
						return fmt.Errorf(
							"path-sensitive archive source member %q conflicts with an existing input",
							canonical,
						)
					}
					continue
				}
				byPath[canonical] = candidate
			}
		}
		for slot, output := range node.Outputs {
			if output.ObservedPath != "" {
				// Observation envelopes are private planner state, not files in
				// Kbuild's writable object-tree namespace.
				continue
			}
			if err := processOutputCandidate(compactKbuildRuleInput{
				path: output.Path, producer: node.ID, slot: slot,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	fastMaterializedResults := []compactKbuildRuleInput{}
	persistentMaterializedRoot := ""
	if len(roots) != 0 {
		core, processedLive, err := b.plan.compactKbuildWorkingTreeMaterializedCoreForConsumer(
			roots, byPath, processNode,
		)
		if err != nil {
			return nil, err
		}
		if !processedLive {
			store, storeErr := b.plan.planningActionPlanInputSetStore()
			if storeErr != nil {
				return nil, storeErr
			}
			if sharedInputSet == nil {
				if walkErr := store.Walk(core.inputSet, func(entry ActionPlanInputSetEntry) error {
					input, inputErr := compactKbuildRuleInputFromSetEntry(entry)
					if inputErr != nil {
						return inputErr
					}
					input.workingOnly = !nativeRoots[producerOutput{producer: input.producer, slot: input.slot}]
					fastMaterializedResults = append(fastMaterializedResults, input)
					return nil
				}); walkErr != nil {
					return nil, walkErr
				}
			} else {
				persistentMaterializedRoot = core.inputSet
				// Native-root policy is consumer-local. Pull only those exact slots
				// out of the shared root; every other entry keeps the persistent
				// ancestry identity reused by sibling configurations.
				nativeOutputs := make([]compactKbuildWorkingTreeMaterializedOutput, 0, len(nativeRoots))
				for root := range nativeRoots {
					node, exists := b.plan.nodesByID[root.producer]
					if !exists || root.slot < 0 || root.slot >= len(node.Outputs) {
						continue
					}
					output := node.Outputs[root.slot]
					if output.ObservedPath == "" {
						nativeOutputs = append(nativeOutputs, compactKbuildWorkingTreeMaterializedOutput{
							path: output.Path, producer: root.producer, slot: root.slot,
						})
					}
				}
				sort.Slice(nativeOutputs, func(i, j int) bool {
					if nativeOutputs[i].path != nativeOutputs[j].path {
						return nativeOutputs[i].path < nativeOutputs[j].path
					}
					if nativeOutputs[i].producer != nativeOutputs[j].producer {
						return nativeOutputs[i].producer < nativeOutputs[j].producer
					}
					return nativeOutputs[i].slot < nativeOutputs[j].slot
				})
				for _, output := range nativeOutputs {
					target := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: output.path}
					entry, found, lookupErr := store.Lookup(persistentMaterializedRoot, target)
					if lookupErr != nil {
						return nil, lookupErr
					}
					if !found || entry.ProducerID != output.producer || entry.Slot != output.slot {
						continue
					}
					var deleted bool
					persistentMaterializedRoot, deleted, lookupErr = store.Delete(persistentMaterializedRoot, target)
					if lookupErr != nil {
						return nil, lookupErr
					}
					if !deleted {
						return nil, fmt.Errorf("native materialized target %q disappeared from persistent frontier", output.path)
					}
					if candidateErr := processOutputCandidate(compactKbuildRuleInput{
						path: output.path, producer: output.producer, slot: output.slot,
					}); candidateErr != nil {
						return nil, candidateErr
					}
				}
			}
			for _, conflict := range core.conflicts {
				for _, output := range conflict.versions {
					if err := processOutputCandidate(compactKbuildRuleInput{
						path: output.path, producer: output.producer, slot: output.slot,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for pathname, versions := range producerVersions {
		winner, err := resolveProducerVersions(pathname, versions)
		if err != nil {
			return nil, err
		}
		if existing, ok := byPath[pathname]; ok {
			if existing.producer == "" && directImmutablePaths[pathname] {
				baselineOnly := baselineProducers[pathname][winner.producer] &&
					!directProducers[pathname][winner.producer]
				if baselineOnly {
					continue
				}
				return nil, fmt.Errorf(
					"working object-tree path %q producer %q conflicts with native immutable source %q",
					pathname, winner.producer, existing.sourceID,
				)
			}
			winner = preferInput(winner, existing)
		}
		byPath[pathname] = winner
	}
	paths := make([]string, 0, len(byPath))
	for pathname := range byPath {
		paths = append(paths, pathname)
	}
	sort.Strings(paths)
	result := make([]compactKbuildRuleInput, 0, len(paths)+len(fastMaterializedResults))
	fastIndex, localIndex := 0, 0
	for fastIndex < len(fastMaterializedResults) || localIndex < len(paths) {
		if fastIndex == len(fastMaterializedResults) {
			result = append(result, byPath[paths[localIndex]])
			localIndex++
			continue
		}
		if localIndex == len(paths) || fastMaterializedResults[fastIndex].path < paths[localIndex] {
			result = append(result, fastMaterializedResults[fastIndex])
			fastIndex++
			continue
		}
		if paths[localIndex] < fastMaterializedResults[fastIndex].path {
			result = append(result, byPath[paths[localIndex]])
			localIndex++
			continue
		}
		return nil, fmt.Errorf(
			"working object-tree materialized core path %q also entered consumer-local closure state",
			paths[localIndex],
		)
	}
	if sharedInputSet != nil {
		*sharedInputSet = persistentMaterializedRoot
	}
	return result, nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildInputIsHostToolOutput(input compactKbuildRuleInput) bool {
	if b == nil || b.plan == nil || input.producer == "" {
		return false
	}
	b.plan.ensureNodeLookupIndexes()
	node, ok := b.plan.nodesByID[input.producer]
	return ok && (node.Stage == "prehost" || node.Stage == "host")
}
