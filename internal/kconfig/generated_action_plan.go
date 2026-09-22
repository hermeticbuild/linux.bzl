package kconfig

// Preparatory and host actions are selected by the evaluated Kbuild profiles.
// This file intentionally contains no architecture, compiler, configuration,
// or generated-file catalogue.  Every demanded target is recursively lowered
// from the captured rule/evaluator graph by compactKbuildRulePlanBuilder.

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
)

type compactKbuildSideOutputObservationKey struct {
	candidate compactKbuildSelectionKey
	path      string
}

type compactKbuildSideOutputDemandStateKey struct {
	consumer compactKbuildSelectionKey
	tree     string
	path     string
}

// appendGeneratedActionPlan materializes every profile-exact Kbuild selection.
// Generated assignments are Kbuild's own declaration of the demanded closure;
// the generic rule builder follows their concrete rules and prerequisites.
// Config artifacts are input projections, not generated policy, and are copied
// into the object-tree layout expected by Kbuild.
func (m *CompactMetadata) appendGeneratedActionPlan(
	plan *ActionPlan,
) (*compactKbuildSelectionGraph, error) {
	if m == nil {
		return nil, fmt.Errorf("selected Kbuild action plan requires one resolved config")
	}
	selectionGraph := m.validatedSelectionGraph
	if selectionGraph == nil {
		var err error
		selectionGraph, err = newCompactKbuildSelectionGraph(m.Config)
		if err != nil {
			return nil, err
		}
	}
	if m.configFragment == nil {
		return nil, fmt.Errorf("selected Kbuild action plan requires a resolved config fragment")
	}
	// Every operation below may populate caches or materialization state. Consume
	// the eagerly validated graph before the first mutation so a failed or later
	// traversal cannot observe a partially lowered graph.
	m.validatedSelectionGraph = nil
	plan.selectionGraph = selectionGraph
	if err := selectionGraph.prepareGroupedSelections(m, m.Config); err != nil {
		return nil, err
	}
	b := generatedPlanBuilder{metadata: m, plan: plan, config: m.Config, fragment: maps.Clone(m.configFragment)}
	if !m.preconfiguredObjectTree {
		if err := b.internConfigProjectionSources(); err != nil {
			return nil, err
		}
	}
	sideOutputDemands, err := selectionGraph.compactKbuildSideOutputDemands(m, m.Config)
	if err != nil {
		return nil, err
	}
	if err := selectionGraph.compactKbuildRegisterSideOutputCandidateDependencies(sideOutputDemands); err != nil {
		return nil, err
	}
	demandsByConsumer := map[compactKbuildSelectionKey][]compactKbuildSideOutputDemand{}
	observationsByCandidate := map[compactKbuildSelectionKey][]compactKbuildObservedOutput{}
	observationByKey := map[compactKbuildSideOutputObservationKey]compactKbuildObservedOutput{}
	candidateSetsByPath := map[string]map[compactKbuildSelectionKey]bool{}
	for _, demand := range sideOutputDemands {
		demandsByConsumer[demand.consumer] = append(demandsByConsumer[demand.consumer], demand)
		for _, candidate := range demand.candidates {
			candidate = selectionGraph.compactKbuildGroupedSelectionRepresentative(candidate)
			observation, err := compactKbuildSideOutputStateObservation(demand, candidate)
			if err != nil {
				return nil, err
			}
			key := compactKbuildSideOutputObservationKey{candidate: candidate, path: demand.output.Path}
			if previous, exists := observationByKey[key]; exists {
				if previous.path != observation.path || previous.output != observation.output {
					return nil, fmt.Errorf("side-output observation %s/%q has conflicting capture identities", compactKbuildSelectionKeyString(candidate), demand.output.Path)
				}
			} else {
				observationByKey[key] = observation
				observationsByCandidate[candidate] = append(observationsByCandidate[candidate], observation)
			}
			if candidateSetsByPath[demand.output.Path] == nil {
				candidateSetsByPath[demand.output.Path] = map[compactKbuildSelectionKey]bool{}
			}
			candidateSetsByPath[demand.output.Path][candidate] = true
		}
	}
	for candidate := range observationsByCandidate {
		sort.Slice(observationsByCandidate[candidate], func(i, j int) bool {
			left, right := observationsByCandidate[candidate][i], observationsByCandidate[candidate][j]
			if left.path != right.path {
				return left.path < right.path
			}
			return actionPlanLookupKey(left.output.Tree, left.output.Path) < actionPlanLookupKey(right.output.Tree, right.output.Path)
		})
	}
	stateProjections, err := selectionGraph.sideOutputStateProjections(m, candidateSetsByPath, sideOutputDemands)
	if err != nil {
		return nil, err
	}
	stateGraphsByPath := stateProjections.byPath
	maximalCandidatesByDemand := stateProjections.maximalByDemand
	observedStates := map[string]compactKbuildRuleInput{}

	ordered, err := selectionGraph.materializationOrder(m)
	if err != nil {
		return nil, err
	}
	// Rule/closure caches are only needed to derive side-output ownership and
	// topological order. Do not retain that duplicate graph while the complete
	// action plan is lowered; the structural selection indexes remain intact
	// for product validation and can repopulate the caches on demand.
	selectionGraph.releasePlanningCaches()
	defer func() {
		// Some selected roots are control-only, forwarding, grouped peers, or
		// otherwise skipped below. Do not retain their one-shot rule graphs in
		// the completed ActionPlan after all materializable roots have had a
		// chance to consume their exact match.
		selectionGraph.selectedRootRuleResolutions = make(
			map[compactKbuildSelectedRuleResolutionKey]compactKbuildRuleResolution,
		)
	}()
	for _, selection := range ordered {
		profile, ok := selectionGraph.profile(selection.Profile)
		if !ok {
			return nil, fmt.Errorf("Kbuild demand references missing profile %q", selection.Profile)
		}
		target := compactKbuildGraphTargetPath(selection.Target)
		if target == "" {
			return nil, fmt.Errorf("profile %q contains an empty selected target", profile.Name)
		}
		selectionKey := compactKbuildSelectionKey{profile: selection.Profile, target: target, stage: selection.Stage}
		if selectionGraph.forwardingSelections[selectionKey] {
			continue
		}
		if _, materialized := selectionGraph.materializedProducers[selectionKey]; materialized {
			// A grouped rule records its one producer against every selected peer.
			// The first peer in dependency order already published this target.
			continue
		}
		selectedPhony := selectionGraph.compactKbuildProfileTargetIsPhony(profile, target)
		var phonyStatus *compactKbuildSelectedPhonyStatus
		if selectedPhony {
			// A frozen feature gate whose resolved status is already success
			// has no shell branch to run and no file to publish. Evaluate the
			// entire selected source line before omitting this PHONY recipe.
			featureCheck, featureErr := m.compactKbuildSelectedPhonyFeatureGate(
				profile, target, selection.MakeTarget,
			)
			if featureErr != nil {
				return nil, fmt.Errorf("inspect selected PHONY %s target %q: %w", selection.Stage, target, featureErr)
			}
			if featureCheck {
				continue
			}
			// Most PHONY goals only order prerequisite files or recursive Make.
			// A directly selected immutable source script can instead have its
			// own failure status without creating a file; authenticate that one
			// selected Make command before allowing it into action lowering.
			check, checkErr := m.compactKbuildSelectedPhonySourceScriptCheck(
				profile, target, selection.MakeTarget, selection.Scope,
			)
			if checkErr != nil {
				return nil, fmt.Errorf("inspect selected PHONY %s target %q: %w", selection.Stage, target, checkErr)
			}
			if !check {
				status, statusErr := m.compactKbuildSelectedPhonySourceStatus(profile, target, selection.MakeTarget)
				if statusErr != nil {
					return nil, fmt.Errorf("inspect selected PHONY %s target %q: %w", selection.Stage, target, statusErr)
				}
				if status == nil {
					continue
				}
				phonyStatus = status
			}
		}
		setupOnly := false
		if selection.SourceScriptPhase == "" && !selectedPhony && !selectionGraph.hasSelectedRootRuleResolution(
			m, selectionKey, profile, target, selection.MakeTarget,
		) {
			setupOnly, err = m.compactKbuildTargetIsOrderingOnlyInProfileForMakeTarget(
				profile, target, selection.MakeTarget,
			)
			if err != nil {
				return nil, fmt.Errorf("classify evaluated %s target %q: %w", selection.Stage, target, err)
			}
		}
		if setupOnly {
			// Preserve the selected Make ordering edge without inventing a file
			// for a source-declared control target.
			continue
		}
		context := compactKbuildSelectionPlanContext(m.Config, selection)
		initialArtifacts, err := compactKbuildSelectionInitialObjectTreeArtifacts(selection)
		if err != nil {
			return nil, fmt.Errorf("decode evaluated %s target %q initial object-tree artifacts: %w", selection.Stage, target, err)
		}
		generatedArtifacts, err := compactKbuildSelectionGeneratedObjectTreeArtifacts(selection)
		if err != nil {
			return nil, fmt.Errorf("decode evaluated %s target %q generated object-tree artifacts: %w", selection.Stage, target, err)
		}
		groupMembers := selectionGraph.compactKbuildGroupedSelectionMembers(selectionKey)
		resolvedSideOutputs := map[string]compactKbuildRuleInput{}
		groupDemands := map[string]compactKbuildSideOutputDemand{}
		for _, member := range groupMembers {
			for _, demand := range demandsByConsumer[member] {
				canonical := demand
				canonical.consumer = selectionGraph.compactKbuildGroupedSelectionRepresentative(selectionKey)
				if previous, exists := groupDemands[demand.output.Path]; exists {
					sameCandidates := len(previous.candidates) == len(canonical.candidates)
					if sameCandidates {
						for index := range previous.candidates {
							if previous.candidates[index] != canonical.candidates[index] {
								sameCandidates = false
								break
							}
						}
					}
					if previous.stage != canonical.stage || previous.product != canonical.product ||
						previous.output != canonical.output || !sameCandidates {
						return nil, fmt.Errorf(
							"grouped Kbuild selection %s has incompatible side-output demands for %q",
							compactKbuildSelectionKeyString(selectionKey), demand.output.Path,
						)
					}
					continue
				}
				groupDemands[demand.output.Path] = canonical
			}
		}
		groupDemandPaths := make([]string, 0, len(groupDemands))
		for sideOutputPath := range groupDemands {
			groupDemandPaths = append(groupDemandPaths, sideOutputPath)
		}
		sort.Strings(groupDemandPaths)
		for _, sideOutputPath := range groupDemandPaths {
			demand := groupDemands[sideOutputPath]
			demandStateKey := compactKbuildSideOutputDemandStateKey{
				consumer: selectionGraph.compactKbuildGroupedSelectionRepresentative(demand.consumer),
				tree:     demand.output.Tree,
				path:     demand.output.Path,
			}
			maximalCandidates, exists := maximalCandidatesByDemand[demandStateKey]
			if !exists || len(maximalCandidates) == 0 {
				return nil, fmt.Errorf("Kbuild side output %q has no maximal candidate state frontier", demand.output.Path)
			}
			states := make([]compactKbuildSideOutputCandidateState, 0, len(maximalCandidates))
			for _, candidate := range maximalCandidates {
				observation, observationErr := compactKbuildSideOutputStateObservation(demand, candidate)
				if observationErr != nil {
					return nil, observationErr
				}
				state, available := observedStates[actionPlanLookupKey(observation.output.Tree, observation.output.Path)]
				if !available {
					return nil, fmt.Errorf(
						"Kbuild side output %q consumer %s reached materialization before candidate %s capture %s/%s",
						demand.output.Path, compactKbuildSelectionKeyString(demand.consumer),
						compactKbuildSelectionKeyString(candidate), observation.output.Tree, observation.output.Path,
					)
				}
				states = append(states, compactKbuildSideOutputCandidateState{
					candidate: candidate,
					input:     state,
				})
			}
			nativeDemand := demand
			if demand.consumer.stage == "prehost" || demand.consumer.stage == "bootstrap" {
				nativeDemand.stage = demand.consumer.stage
				nativeDemand.product = "sdk"
				nativeDemand.output.Tree = demand.consumer.stage
			}
			resolved, resolveErr := appendCompactKbuildSideOutputResolver(
				plan, selectionGraph, m, nativeDemand, states,
			)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if previous, exists := resolvedSideOutputs[demand.output.Path]; exists &&
				(previous.producer != resolved.producer || previous.slot != resolved.slot) {
				return nil, fmt.Errorf(
					"grouped Kbuild selection %s has multiple side-output resolvers for %q",
					compactKbuildSelectionKeyString(selectionKey), demand.output.Path,
				)
			}
			resolvedSideOutputs[demand.output.Path] = resolved
		}
		observations := []compactKbuildObservedOutput{}
		seenObservations := map[string]bool{}
		for _, member := range groupMembers {
			candidate := selectionGraph.compactKbuildGroupedSelectionRepresentative(member)
			for _, observation := range observationsByCandidate[candidate] {
				key := actionPlanLookupKey(observation.output.Tree, observation.output.Path)
				if seenObservations[key] {
					continue
				}
				stateGraph, exists := stateGraphsByPath[observation.path]
				if !exists {
					return nil, fmt.Errorf("observed Kbuild path %q has no candidate state graph", observation.path)
				}
				parents, exists := stateGraph.parentsFor(candidate)
				if !exists {
					return nil, fmt.Errorf("observed Kbuild path %q has no state projection for candidate %s", observation.path, compactKbuildSelectionKeyString(candidate))
				}
				for _, parent := range parents {
					parentObservation, exists := observationByKey[compactKbuildSideOutputObservationKey{
						candidate: parent, path: observation.path,
					}]
					if !exists {
						return nil, fmt.Errorf(
							"observed Kbuild path %q candidate %s has unregistered parent %s",
							observation.path, compactKbuildSelectionKeyString(candidate), compactKbuildSelectionKeyString(parent),
						)
					}
					state, available := observedStates[actionPlanLookupKey(parentObservation.output.Tree, parentObservation.output.Path)]
					if !available {
						return nil, fmt.Errorf(
							"observed Kbuild path %q candidate %s reached materialization before parent %s state",
							observation.path, compactKbuildSelectionKeyString(candidate), compactKbuildSelectionKeyString(parent),
						)
					}
					observation.baseInputs = append(observation.baseInputs, state)
				}
				seenObservations[key] = true
				observations = append(observations, observation)
			}
		}
		builder := newCompactKbuildRulePlanBuilder(m, plan).
			withSelectionGraph(selectionGraph).
			forSelection(selectionKey, profile).
			forOutput(context.Stage, context.OutputTree, context.Product).
			withInitialObjectTree(selection.UsesInitialObjectTree, initialArtifacts...).
			withGeneratedObjectTreeArtifacts(generatedArtifacts...).
			withResolvedSideOutputs(resolvedSideOutputs)
		builder, err = builder.forObservedOutputs(target, observations)
		if err != nil {
			return nil, fmt.Errorf("declare evaluated %s target %q side-output observations: %w", selection.Stage, target, err)
		}
		producer, buildErr := "", error(nil)
		if selection.SourceScriptPhase != "" {
			phase, owner, authenticated := selectionGraph.selectedSourcePhase(selectionKey)
			if !authenticated {
				return nil, fmt.Errorf("selected source phase %s has no authenticated owner", compactKbuildSelectionKeyString(selectionKey))
			}
			producer, buildErr = builder.buildSelectedSourceScriptPhase(phase, owner)
		} else if phonyStatus != nil {
			producer, buildErr = builder.buildSelectedPhonyStatus(target, phonyStatus)
		} else {
			producer, buildErr = builder.buildSelectedTarget(target, selection.MakeTarget)
		}
		if buildErr != nil {
			return nil, fmt.Errorf("materialize evaluated %s target %q: %w", selection.Stage, target, buildErr)
		}
		if err := selectionGraph.recordMaterializedProducer(selectionKey, producer); err != nil {
			return nil, err
		}
		producerNode, exists := compactKbuildPlanNode(plan, producer)
		if !exists {
			return nil, fmt.Errorf("materialized Kbuild selection %s has no action-plan node %q", compactKbuildSelectionKeyString(selectionKey), producer)
		}
		completion := compactKbuildAuthenticatedExecutionCheckCompletion(plan, producerNode, target)
		if selectedPhony && !completion {
			return nil, fmt.Errorf("selected PHONY source script %q produced a file or lost its authenticated outputless execution state", target)
		}
		if completion && !slices.Contains(plan.executionCheckRoots, producer) {
			plan.executionCheckRoots = append(plan.executionCheckRoots, producer)
		}
		for slot, output := range producerNode.Outputs {
			if output.ObservedPath == "" {
				continue
			}
			key := actionPlanLookupKey(output.Tree, output.Path)
			state := compactKbuildRuleInput{path: output.Path, producer: producer, slot: slot}
			if previous, exists := observedStates[key]; exists &&
				(previous.producer != state.producer || previous.slot != state.slot) {
				return nil, fmt.Errorf("observed Kbuild state %s/%s has multiple producers", output.Tree, output.Path)
			}
			observedStates[key] = state
		}
		// A command with a fully proven, target-only filesystem effect keeps its
		// opaque side-output lineage in a separate no-op state node. Resolve those
		// candidate-specific captures by their canonical output identity without
		// making the selected target's byte producer depend on unrelated state.
		for _, observation := range observations {
			key := actionPlanLookupKey(observation.output.Tree, observation.output.Path)
			if _, exists := observedStates[key]; exists {
				continue
			}
			stateProducer, slot, exists := planProducerByOutput(
				plan, observation.output.Tree, observation.output.Path,
			)
			if !exists {
				return nil, fmt.Errorf(
					"materialized Kbuild selection %s has no observed state %s/%s",
					compactKbuildSelectionKeyString(selectionKey), observation.output.Tree, observation.output.Path,
				)
			}
			stateNode, exists := compactKbuildPlanNode(plan, stateProducer)
			if !exists || slot < 0 || slot >= len(stateNode.Outputs) || stateNode.Outputs[slot].ObservedPath != observation.path {
				return nil, fmt.Errorf(
					"materialized Kbuild selection %s observed state %s/%s does not capture %q",
					compactKbuildSelectionKeyString(selectionKey), observation.output.Tree, observation.output.Path, observation.path,
				)
			}
			observedStates[key] = compactKbuildRuleInput{
				path: observation.output.Path, producer: stateProducer, slot: slot,
			}
		}
		if err := appendCompactKbuildBootstrapProjections(plan, m.Config, selection, producer); err != nil {
			return nil, err
		}
		if err := appendCompactKbuildHostPrepMirrors(plan, selection, producer); err != nil {
			return nil, err
		}
	}
	plan.releaseProbeDiscoveryPayloads()
	return selectionGraph, nil
}

// A selected PHONY recipe may contain recursive Make lines followed by an
// outputless source shell command. Retain its last local command as an exact
// line-local execution status; the selected child invocations precede that
// status through the graph, without manufacturing a file named by the PHONY
// target. The source rule entry exists even when the file-oriented rule matcher
// correctly declines this rule.
type compactKbuildSelectedPhonyStatus struct {
	match       compactKbuildRuleMatch
	snapshot    *KbuildSelectedControlRecipeSnapshot
	command     string
	recipeIndex int
}

func (m *CompactMetadata) compactKbuildSelectedPhonySourceStatus(
	profile CompactKbuildProfile, target, makeTarget string,
) (*compactKbuildSelectedPhonyStatus, error) {
	entry := CompactKbuildSelectedControlRuleEntrySnapshot(profile, target)
	if entry == nil {
		for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget) {
			if candidate.ruleOrder >= 0 && candidate.ruleOrder < len(profile.Rules) &&
				len(profile.Rules[candidate.ruleOrder].Recipe) != 0 {
				return nil, fmt.Errorf("%s: selected PHONY target %q has an executable recipe without a source-selected entry", candidate.rule.Position, target)
			}
		}
		return nil, nil
	}
	index := entry.Line.RuleIndex
	if index < 0 || index >= len(profile.Rules) || entry.Line.Target != target ||
		entry.Profile.Name != profile.Name {
		return nil, fmt.Errorf("Kbuild PHONY target %q has no valid source-selected rule entry", target)
	}
	rule := profile.Rules[index]
	if len(rule.Recipe) == 0 {
		return nil, nil
	}
	snapshots := CompactKbuildSelectedControlRecipeSnapshots(profile, target)
	if len(snapshots) != len(rule.Recipe) {
		return nil, fmt.Errorf("%s: selected PHONY target %q has no complete frozen source recipe", rule.Position, target)
	}
	dependencies := []CompactKbuildInvocationDependency{}
	for _, dependency := range profile.TargetInvocationDependencies {
		if compactKbuildGraphTargetPath(dependency.Target) == target {
			dependencies = append(dependencies, dependency)
		}
	}
	consumed := make([]bool, len(dependencies))
	status := &compactKbuildSelectedPhonyStatus{recipeIndex: -1}
	localCommand := false
	for recipeIndex, raw := range rule.Recipe {
		snapshot := snapshots[recipeIndex]
		if snapshot == nil || snapshot.Line.RuleIndex != index ||
			snapshot.Line.RecipeIndex != recipeIndex ||
			snapshot.Line.LookupTarget != entry.Line.LookupTarget ||
			snapshot.Line.AutomaticTarget != entry.Line.AutomaticTarget {
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d has no matching source line", rule.Position, target, recipeIndex)
		}
		match := compactKbuildRuleMatch{
			profile: snapshot.Evaluation.Profile, rule: rule, ruleOrder: index,
			lookupTarget: entry.Line.LookupTarget, stem: entry.Line.Stem,
			explicit: true, resolved: true,
		}
		automatic := compactKbuildAutomaticContext{
			target: entry.Line.AutomaticTarget, stem: entry.Line.Stem,
			normal: entry.Line.Normal, order: entry.Line.OrderOnly,
		}
		pure, err := compactKbuildSelectedRecipeSourceExpansionIsPure(target, match, raw, automatic, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d Make expansion: %w", rule.Position, target, recipeIndex, err)
		}
		if !pure {
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d has stateful Make expansion", rule.Position, target, recipeIndex)
		}
		actual, err := evaluateCompactKbuildTextForMakeTarget(
			snapshot.Evaluation.Profile, target, entry.Line.LookupTarget,
			entry.Line.AutomaticTarget, entry.Line.Stem, entry.Line.Normal,
			entry.Line.OrderOnly, nil, raw, true,
		)
		if err != nil {
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d Make value: %w", rule.Position, target, recipeIndex, err)
		}
		command := compactKbuildRecipeExecutionText(actual)
		switch command {
		case "", ":", "true":
			continue
		case "false":
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d fails its source shell status", rule.Position, target, recipeIndex)
		}
		words, lexErr := lexCompactKbuildRecipe(command)
		if lexErr != nil {
			return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d shell lexing: %w", rule.Position, target, recipeIndex, lexErr)
		}
		if len(words) != 0 && words[0].value == CompactKbuildRecursiveMakeProvenanceToken {
			arguments := make([]string, 0, len(words)-1)
			for _, word := range words[1:] {
				if word.operator {
					return nil, fmt.Errorf("%s: selected PHONY target %q has a compound recursive Make line", rule.Position, target)
				}
				arguments = append(arguments, compactKbuildSourceScriptReplayValue(profile, word.value))
			}
			found := false
			for dependencyIndex, dependency := range dependencies {
				if consumed[dependencyIndex] || len(arguments) != len(dependency.ReplayArguments) {
					continue
				}
				equal := true
				for argumentIndex, argument := range dependency.ReplayArguments {
					if arguments[argumentIndex] != compactKbuildSourceScriptReplayValue(profile, argument) {
						equal = false
						break
					}
				}
				if equal {
					consumed[dependencyIndex] = true
					found = true
					break
				}
			}
			if !found || localCommand {
				return nil, fmt.Errorf("%s: selected PHONY target %q recipe %d has no matching source-ordered recursive child", rule.Position, target, recipeIndex)
			}
			continue
		}
		if localCommand || recipeIndex != len(rule.Recipe)-1 {
			return nil, fmt.Errorf("%s: selected PHONY target %q has multiple or nonterminal local shell effects", rule.Position, target)
		}
		localCommand = true
		status.match = match
		status.snapshot = snapshot
		status.command = command
		status.recipeIndex = recipeIndex
	}
	for index, matched := range consumed {
		if !matched {
			return nil, fmt.Errorf("%s: selected PHONY target %q has unaccounted recursive child %q", rule.Position, target, dependencies[index].Profile)
		}
	}
	if !localCommand {
		if len(dependencies) == 0 {
			return nil, nil
		}
		status.command = ":"
		status.match = compactKbuildRuleMatch{
			profile: snapshots[0].Evaluation.Profile, rule: rule, ruleOrder: index,
			lookupTarget: entry.Line.LookupTarget, stem: entry.Line.Stem,
			explicit: true, resolved: true,
		}
		status.snapshot = snapshots[0]
	}
	return status, nil
}

// compactKbuildSelectedPhonyFeatureGate proves the status-only PHONY idiom in
// tools Makefiles from one complete source-selected line. A resolved feature
// value of 1 makes the shell's sole failure branch unreachable; another value
// would make GNU Make fail before a dependent target, so fail planning at the
// same source gate. The strict source form and exact frozen Make expansion
// prevent an unmodeled PHONY command from disappearing as an ordering edge.
func (m *CompactMetadata) compactKbuildSelectedPhonyFeatureGate(
	profile CompactKbuildProfile, target, makeTarget string,
) (bool, error) {
	match, matched, err := m.compactKbuildRuleForProfileMakeTarget(profile, target, makeTarget)
	if err != nil || !matched {
		return false, err
	}
	if len(match.rule.Recipe) != 1 {
		for _, raw := range match.rule.Recipe {
			if strings.Contains(raw, "$(feature-") {
				return true, fmt.Errorf("%s: PHONY feature gate %q requires one complete selected source line", match.rule.Position, target)
			}
		}
		return false, nil
	}
	raw := strings.TrimSpace(match.rule.Recipe[0])
	if !strings.HasPrefix(strings.TrimLeft(raw, "@+"), `if [ "$(feature-`) {
		if strings.Contains(raw, "$(feature-") {
			return true, fmt.Errorf("%s: PHONY feature gate %q has unsupported source condition", match.rule.Position, target)
		}
		return false, nil
	}
	source := compactKbuildRecipeExecutionText(raw)
	const prefix = `if [ "$(feature-`
	name, rest, ok := strings.Cut(strings.TrimPrefix(source, prefix), `)" != "1" ]; then echo "`)
	if !strings.HasPrefix(source, prefix) || !ok || name == "" ||
		strings.ContainsFunc(name, func(character rune) bool {
			return !(character >= 'a' && character <= 'z' ||
				character >= '0' && character <= '9' || character == '-' || character == '_')
		}) {
		return true, fmt.Errorf("%s: PHONY feature gate %q has unsupported source condition", match.rule.Position, target)
	}
	message, suffix, ok := strings.Cut(rest, `"; exit 1`)
	if !ok || message == "" || strings.ContainsAny(message, "\"'$`\\\n\r;") ||
		(suffix != " ; fi" && suffix != "; fi") {
		return true, fmt.Errorf("%s: PHONY feature gate %q has unsupported failure branch", match.rule.Position, target)
	}
	match, err = compactKbuildSelectedRuleEntryMatch(target, match)
	if err != nil {
		return true, err
	}
	snapshots, err := compactKbuildSelectedRuleRecipeSnapshots(target, match)
	if err != nil {
		return true, err
	}
	if len(snapshots) != 1 || snapshots[0] == nil {
		return true, fmt.Errorf("%s: PHONY feature gate %q has no frozen source line", match.rule.Position, target)
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
	if err != nil {
		return true, err
	}
	injected = compactKbuildActionTreeInjections(injected)
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
	if err != nil {
		return true, err
	}
	lineMatch := match
	lineMatch.profile = snapshots[0].Evaluation.Profile
	pure, err := compactKbuildSelectedRecipeSourceExpansionIsPure(target, lineMatch, raw, automatic, injected)
	if err != nil {
		return true, err
	}
	if !pure {
		return true, fmt.Errorf("%s: PHONY feature gate %q has a stateful Make expansion", match.rule.Position, target)
	}
	expand := func(text string) (string, error) {
		return evaluateCompactKbuildTextForMakeTarget(
			lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected, text, true,
		)
	}
	variable := "feature-" + name
	value, err := expand("$(" + variable + ")")
	if err != nil {
		return true, fmt.Errorf("%s: PHONY feature gate %q status %q: %w", match.rule.Position, target, variable, err)
	}
	if value != "0" && value != "1" {
		return true, fmt.Errorf("%s: PHONY feature gate %q status %q is not a resolved boolean", match.rule.Position, target, variable)
	}
	actual, err := expand(raw)
	if err != nil {
		return true, fmt.Errorf("%s: PHONY feature gate %q recipe: %w", match.rule.Position, target, err)
	}
	expected := strings.Replace(source, "$("+variable+")", value, 1)
	if compactKbuildRecipeExecutionText(actual) != expected {
		return true, fmt.Errorf("%s: PHONY feature gate %q changed its selected shell command", match.rule.Position, target)
	}
	if value != "1" {
		return true, fmt.Errorf("%s: PHONY feature gate %q failed: %s", match.rule.Position, target, message)
	}
	return true, nil
}

// Check the selected PHONY recipe against its frozen Make evaluator before
// lowering. A PHONY goal with only prerequisite or recursive-Make closure
// remains an ordering node. Only one source-backed script invocation is
// eligible for an execution result; final lowering and the runner separately
// enforce that it creates no Make-visible file.
func (m *CompactMetadata) compactKbuildSelectedPhonySourceScriptCheck(
	profile CompactKbuildProfile, target, makeTarget, scope string,
) (bool, error) {
	match, matched, err := m.compactKbuildRuleForProfileMakeTarget(profile, target, makeTarget)
	if err != nil || !matched {
		return false, err
	}
	if len(match.rule.Recipe) == 0 {
		return false, nil
	}
	match, err = compactKbuildSelectedRuleEntryMatch(target, match)
	if err != nil {
		return false, err
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
	if err != nil {
		return false, err
	}
	injected = compactKbuildActionTreeInjections(injected)
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
	if err != nil {
		return false, err
	}
	snapshots, err := compactKbuildSelectedRuleRecipeSnapshots(target, match)
	if err != nil {
		return false, err
	}
	probeSourceScripts := func(lineMatch compactKbuildRuleMatch, actual string) ([]CompactKbuildSourceScript, error) {
		return readCompactKbuildCommandSourceScriptsForMakeTarget(
			lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected,
			compactKbuildDirectRecipeText(lineMatch.profile, actual), true,
		)
	}
	if len(match.rule.Recipe) != 1 {
		// Inspect each frozen line before retaining an executable PHONY target.
		// The generic rule lowerer must independently prove its side effects and
		// successful completion without declaring the PHONY name as a file.
		sourceScript := false
		scriptPath := ""
		rootedLines := make([]string, 0, len(match.rule.Recipe))
		for index, raw := range match.rule.Recipe {
			lineMatch := match
			if snapshot := snapshots[index]; snapshot != nil {
				lineMatch.profile = snapshot.Evaluation.Profile
			}
			actual, evaluateErr := evaluateCompactKbuildTextForMakeTarget(
				lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
				automatic.normal, automatic.order, injected, raw, true,
			)
			if evaluateErr != nil {
				return false, fmt.Errorf("selected PHONY recipe line %d: %w", index, evaluateErr)
			}
			rooted, rootErr := compactKbuildRootedActionDirectRecipeText(lineMatch.profile, actual)
			if rootErr != nil {
				return false, fmt.Errorf("selected PHONY recipe line %d: %w", index, rootErr)
			}
			rootedLines = append(rootedLines, compactKbuildFinalizeRootedActionRecipeText(rooted))
			scripts, scriptErr := probeSourceScripts(lineMatch, actual)
			if scriptErr != nil {
				return false, fmt.Errorf("selected PHONY recipe line %d: %w", index, scriptErr)
			}
			if len(scripts) != 0 {
				sourceScript = true
				scriptPath = scripts[0].Path
			}
		}
		if sourceScript && len(match.rule.Recipe) != 4 {
			return false, fmt.Errorf("cannot authenticate selected PHONY source script %q in a multiline Make recipe", scriptPath)
		}
		if !sourceScript {
			_, _, proved, proofErr := compactKbuildSelectedPhonyPrivateSetup(
				target, match, rootedLines, snapshots, automatic, injected,
			)
			if proofErr != nil {
				return false, proofErr
			}
			if proved {
				return true, nil
			}
		}
		return sourceScript, nil
	}
	lineMatch := match
	if snapshot := snapshots[0]; snapshot != nil {
		lineMatch.profile = snapshot.Evaluation.Profile
	}
	actual, err := evaluateCompactKbuildTextForMakeTarget(
		lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected, match.rule.Recipe[0], true,
	)
	if err != nil {
		return false, err
	}
	rooted, err := compactKbuildRootedActionDirectRecipeText(lineMatch.profile, actual)
	if err != nil {
		return false, err
	}
	commands, err := parseCompactKbuildRecipe(compactKbuildFinalizeRootedActionRecipeText(rooted), automatic)
	if err != nil || len(commands) != 1 {
		invocation, selected, selectionErr := compactKbuildSelectedPhonyCommandSourceScript(
			target, match, automatic, injected, scope, m.actionRoles,
		)
		if selectionErr != nil {
			return false, selectionErr
		}
		if selected && invocation.scriptPath != "" {
			return true, nil
		}
		// The direct parser cannot preserve every shell command form. A
		// selected source script within such a command still has an execution
		// result: do not silently erase its failure status. The source probe
		// observes exactly this frozen Make line and authenticates the script
		// against the selected source tree before rejecting unsupported shapes.
		scripts, scriptErr := probeSourceScripts(lineMatch, actual)
		if scriptErr != nil {
			return false, scriptErr
		}
		if len(scripts) != 0 {
			return false, fmt.Errorf("cannot authenticate selected PHONY source script %q in an unsupported command shape", scripts[0].Path)
		}
		// Recursive-Make PHONY without an executable source script remains
		// an ordering boundary through its selected native prerequisites.
		return false, nil
	}
	values, err := evaluateCompactKbuildRuleVariablesRooted(
		target, lineMatch, nil, injected, "CONFIG_SHELL",
	)
	if err != nil {
		return false, err
	}
	_, sourceScript, err := compactKbuildSourceScriptCommandWithSourceArguments(
		lineMatch.profile, commands[0], commands[0].arguments, values, scope, m.actionRoles,
	)
	return sourceScript, err
}

// A complete command-template call retains both its source-selected shell
// wrapper and the exact cmd_<name> leaf which invoked the immutable script.
// Inspect the leaf with the same script classifier as a direct recipe; the
// wrapper is executed whole and separately checked for private side effects.
func compactKbuildSelectedPhonyCommandSourceScript(
	target string,
	match compactKbuildRuleMatch,
	automatic compactKbuildAutomaticContext,
	injected map[string]string,
	scope string,
	actionRoles []KbuildActionRoleRef,
) (compactKbuildSourceScriptInvocation, bool, error) {
	if len(match.rule.Recipe) != 1 ||
		!CompactKbuildRecipeIsExactCommandTemplateCall(match.rule.Recipe[0]) {
		return compactKbuildSourceScriptInvocation{}, false, nil
	}
	selections, indices, err := evaluatedKbuildRuleCommandSelectionsBySourceLine(
		target, match, automatic, injected, true,
	)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, false, err
	}
	if len(selections) != 1 || selections[0].Name == "" ||
		len(indices) != 0 && (len(indices) != 1 || indices[0] != 0) {
		return compactKbuildSourceScriptInvocation{}, false, nil
	}
	snapshots, err := compactKbuildSelectedRuleRecipeSnapshots(target, match)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, false, err
	}
	snapshot := snapshots[0]
	lineMatch := match
	if len(indices) != 0 {
		if snapshot == nil {
			return compactKbuildSourceScriptInvocation{}, false, nil
		}
		lineMatch.profile = snapshot.Evaluation.Profile
	}
	leaf := compactKbuildDirectRecipeText(lineMatch.profile, selections[0].Text)
	commands, err := parseCompactKbuildRecipe(leaf, automatic)
	if err != nil || len(commands) != 1 {
		return compactKbuildSourceScriptInvocation{}, false, nil
	}
	values, err := evaluateCompactKbuildRuleVariablesRooted(
		target, lineMatch, nil, injected, "CONFIG_SHELL",
	)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, false, err
	}
	invocation, selected, err := compactKbuildSourceScriptCommandWithSourceArguments(
		lineMatch.profile, commands[0], commands[0].arguments, values, scope, actionRoles,
	)
	if err != nil || !selected {
		return invocation, selected, err
	}
	actual, err := evaluateCompactKbuildTextForMakeTarget(
		lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected, match.rule.Recipe[0], true,
	)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, false, err
	}
	scripts, err := readCompactKbuildCommandSourceScriptsForMakeTarget(
		lineMatch.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected,
		compactKbuildDirectRecipeText(lineMatch.profile, actual), true,
	)
	if err != nil {
		return compactKbuildSourceScriptInvocation{}, false, err
	}
	return invocation, len(scripts) == 1 && scripts[0].Path == invocation.scriptPath, nil
}

func compactKbuildSelectionPlanContext(config CompactConfig, selection CompactKbuildSelection) compactKbuildRulePlanContext {
	switch selection.Stage {
	case "prehost":
		return compactKbuildRulePlanContext{Stage: "prehost", OutputTree: "prehost", Product: "sdk"}
	case "bootstrap":
		return compactKbuildRulePlanContext{Stage: "bootstrap", OutputTree: "bootstrap", Product: "sdk"}
	case "host":
		return compactKbuildRulePlanContext{Stage: "host", OutputTree: "host", Product: "sdk"}
	case "prep":
		return compactKbuildRulePlanContext{Stage: "prep", OutputTree: "prep", Product: "sdk"}
	}
	context := defaultCompactKbuildRulePlanContext
	switch {
	case selection.Target == "vmlinux" || selection.Target == "System.map":
		context.OutputTree = "vmlinux"
		context.Product = "vmlinux"
	case selection.Target == config.imageTarget:
		context.OutputTree = "image"
		context.Product = "image"
	case strings.HasSuffix(selection.Target, ".ko"), selection.Target == "modules.order", strings.HasSuffix(selection.Target, "/modules.order"):
		context.OutputTree = "modules"
		context.Product = "modules"
	case selection.Target == "Module.symvers", strings.HasSuffix(selection.Target, "/Module.symvers"):
		context.OutputTree = "metadata"
		context.Product = "module_symvers"
	case selection.Target == "modules.builtin":
		context.OutputTree = "metadata"
		context.Product = "modules_builtin"
	case selection.Target == "modules.builtin.modinfo":
		context.OutputTree = "metadata"
		context.Product = "modules_builtin_modinfo"
	}
	return context
}

// ensureActionPlanSource interns a declared execution input. Kernel paths are
// resolved from the full source_files depset by the callback; config paths are
// exact planner outputs staged under the config namespace.
func ensureActionPlanSource(plan *ActionPlan, namespace, sourcePath string) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("cannot intern a source into a nil action plan")
	}
	if err := validatePlanName("source namespace", namespace); err != nil {
		return "", err
	}
	if err := validatePlanRelativePath("source", sourcePath); err != nil {
		return "", err
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return "", err
	}
	key := actionPlanLookupKey(namespace, sourcePath)
	if id, ok := plan.sourceIDs[key]; ok {
		return id, nil
	}
	if plan.maximumSourceID >= 99999999 {
		return "", fmt.Errorf("action plan source ID space is exhausted")
	}
	id := fmt.Sprintf("src-%08d", plan.maximumSourceID+1)
	plan.Sources = append(plan.Sources, ActionPlanSource{ID: id, Namespace: namespace, Path: sourcePath})
	plan.sourceIDs[key] = id
	plan.sourcesByID[id] = plan.Sources[len(plan.Sources)-1]
	plan.maximumSourceID++
	plan.sourceLookupCount = len(plan.Sources)
	return id, nil
}

func (m *CompactMetadata) ensureActionPlanSource(plan *ActionPlan, sourcePath string) (string, error) {
	namespace, err := m.actionPlanSourceNamespace(sourcePath)
	if err != nil {
		return "", err
	}
	return ensureActionPlanSource(plan, namespace, sourcePath)
}

func (m *CompactMetadata) actionPlanSourceNamespace(sourcePath string) (string, error) {
	namespace, selected, err := m.selectedActionPlanSourceNamespace(sourcePath)
	if err != nil {
		return "", err
	}
	if !selected {
		return "kernel", nil
	}
	return namespace, nil
}

// selectedActionPlanSourceNamespace returns the explicitly configured source
// namespace whose root contains sourcePath. The boolean distinguishes an
// explicit namespace from the implicit kernel-source default. A namespace is
// input-tree routing metadata only; physical profile roots provide existence
// evidence during graph discovery.
func (m *CompactMetadata) selectedActionPlanSourceNamespace(sourcePath string) (string, bool, error) {
	if err := validatePlanRelativePath("source", sourcePath); err != nil {
		return "", false, err
	}
	if err := m.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return "", false, err
	}
	// An exact object-tree leaf is a one-path overlay: it wins over a prefix at
	// the same graph path without claiming any of that prefix's descendants.
	if namespace, ok := m.exactSourcePaths[sourcePath]; ok {
		return namespace, true, nil
	}
	for prefix := sourcePath; prefix != "."; prefix = path.Dir(prefix) {
		if namespace, ok := m.sourceNamespacePrefixes[prefix]; ok {
			return namespace, true, nil
		}
	}
	return "", false, nil
}

func actionPlanSourceNamespacePrefix(rawPrefix string) (string, error) {
	if rawPrefix != strings.TrimSpace(rawPrefix) {
		return "", fmt.Errorf("invalid prefix %q", rawPrefix)
	}
	const sourceTreePrefix = "__LINUX_BZL_SOURCE_TREE__/"
	prefix := strings.TrimPrefix(rawPrefix, sourceTreePrefix)
	if prefix == rawPrefix && rawPrefix == strings.TrimSuffix(sourceTreePrefix, "/") {
		return "", fmt.Errorf("invalid prefix %q", rawPrefix)
	}
	if err := validatePlanRelativePath("source namespace prefix", prefix); err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(prefix, "/")
	if strings.HasPrefix(first, "__LINUX_BZL_") {
		return "", fmt.Errorf("source prefix %q uses reserved tree marker %q", rawPrefix, first)
	}
	return prefix, nil
}
func actionPlanExactSourceNamespacePath(rawPath string) (string, error) {
	if rawPath != strings.TrimSpace(rawPath) {
		return "", fmt.Errorf("invalid path %q", rawPath)
	}
	if err := validatePlanRelativePath("exact source namespace", rawPath); err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(rawPath, "/")
	if strings.HasPrefix(first, "__LINUX_BZL_") {
		return "", fmt.Errorf("exact source path %q uses reserved tree marker %q", rawPath, first)
	}
	return rawPath, nil
}

func (m *CompactMetadata) ensureActionPlanSourceNamespaceIndex() error {
	if m == nil {
		return nil
	}
	if m.sourceNamespaceIndexed {
		return m.sourceNamespaceIndexErr
	}
	m.sourceNamespaceIndexed = true
	m.sourceNamespacePrefixes = make(map[string]string, len(m.sourceNamespaces))
	m.exactSourcePaths = make(map[string]string, len(m.exactSourceNamespaces))
	index := func(
		kind string,
		configured, indexed map[string]string,
		normalize func(string) (string, error),
	) error {
		for rawPrefix, namespace := range configured {
			prefix, err := normalize(rawPrefix)
			if err != nil {
				return fmt.Errorf("%s namespace %q: %w", kind, namespace, err)
			}
			if err := validatePlanName(kind+" namespace", namespace); err != nil {
				return err
			}
			if existing, ok := indexed[prefix]; ok && existing != namespace {
				return fmt.Errorf("%s source path %q has ambiguous namespaces %q and %q", kind, prefix, existing, namespace)
			}
			indexed[prefix] = namespace
		}
		return nil
	}
	if err := index("source prefix", m.sourceNamespaces, m.sourceNamespacePrefixes, actionPlanSourceNamespacePrefix); err != nil {
		m.sourceNamespaceIndexErr = err
		return err
	}
	if err := index("exact source", m.exactSourceNamespaces, m.exactSourcePaths, actionPlanExactSourceNamespacePath); err != nil {
		m.sourceNamespaceIndexErr = err
		return err
	}
	return nil
}

func (m *CompactMetadata) hasExactActionPlanSourceNamespacePrefix(sourcePath string) (bool, error) {
	if err := validatePlanRelativePath("source namespace", sourcePath); err != nil {
		return false, err
	}
	if err := m.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return false, err
	}
	_, exact := m.exactSourcePaths[sourcePath]
	return exact, nil
}

func planProducerByOutput(plan *ActionPlan, tree, outputPath string) (string, int, bool) {
	if plan == nil {
		return "", 0, false
	}
	plan.ensureNodeLookupIndexes()
	if producer, ok := plan.outputProducers[actionPlanLookupKey(tree, outputPath)]; ok {
		return producer.producerID, producer.slot, true
	}
	return "", 0, false
}

type generatedPlanBuilder struct {
	metadata *CompactMetadata
	plan     *ActionPlan
	config   CompactConfig
	fragment map[string]string
}

func (b *generatedPlanBuilder) add(node ActionPlanNode, recipe ActionRecipe) (string, error) {
	return appendActionPlanNode(b.plan, node, recipe)
}

func (b *generatedPlanBuilder) addSource(node *ActionPlanNode, recipe *ActionRecipe, role, namespace, sourcePath string) (string, error) {
	id, err := ensureActionPlanSource(b.plan, namespace, sourcePath)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s:%08d", role, len(node.Sources))
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: role, SourceID: id})
	recipe.Sources = append(recipe.Sources, key)
	return key, nil
}

func appendReferencedPlanTrees(plan *ActionPlan, node *ActionPlanNode, recipe *ActionRecipe, values ...string) error {
	// Callers may supply source text before every recipe field has been folded,
	// but the final contract is the authoritative set. In particular recursive
	// Make replay argv can contain source/object-tree bindings even when the
	// wrapper command's own argv does not.
	values = append(values, recipe.Arguments...)
	values = append(values, recipe.WorkingDirectory)
	values = append(values, sortedStringMapValues(recipe.Environment)...)
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			values = append(values, invocation.Arguments...)
			values = append(values, invocation.Outputs...)
		}
	}
	seen := make(map[string]bool, len(node.Trees))
	for _, tree := range node.Trees {
		seen[tree] = true
	}
	bindTree := func(tree string) {
		if !seen[tree] {
			seen[tree] = true
			node.Trees = append(node.Trees, tree)
		}
		if !slices.Contains(recipe.Trees, tree) {
			recipe.Trees = append(recipe.Trees, tree)
		}
	}
	commandMetadataTrees, commandMetadataWorkingTrees, err := actionPlanCommandMetadataSourceTreeClosure(plan, node)
	if err != nil {
		return err
	}
	// fixdep writes physical compiler source/dependency paths into Kbuild .cmd
	// files, and modpost's srcversion calculation opens those paths later. Keep
	// only the source namespaces behind a consumed .cmd producer. An ancestor
	// WorkingTree means that compiler paths may be relative to its private object
	// overlay, so replay that same namespace into the consumer's private root.
	for _, tree := range commandMetadataWorkingTrees {
		if !slices.Contains(recipe.WorkingTrees, tree) {
			recipe.WorkingTrees = append(recipe.WorkingTrees, tree)
		}
	}
	sort.Strings(recipe.WorkingTrees)
	recipe.WorkingTrees = slices.Compact(recipe.WorkingTrees)
	for _, tree := range commandMetadataTrees {
		bindTree(tree)
	}
	// WorkingTrees are input-only tree bindings whose complete contents are
	// staged below the recipe's private writable root. They need not appear in
	// argv or environment placeholders, but they are still part of the exact
	// sandbox/RBE closure of both the node and its interned recipe.
	for _, tree := range recipe.WorkingTrees {
		bindTree(tree)
	}
	// A translation unit can include a sibling by a relative quoted path even
	// when no -I flag spells the source root (for example mkcpustr.c includes a
	// source .c file). Exact source edges still bind argv placeholders, while
	// this tree edge closes the compiler's source-relative filesystem view for
	// sandboxed and remote execution. Namespace comes from the plan source, not
	// from the logical path: config/object projections may also use source edges.
	if len(node.Sources) != 0 {
		if err := plan.ensureSourceLookupIndex(); err != nil {
			return err
		}
		for _, edge := range node.Sources {
			source, ok := plan.sourcesByID[edge.SourceID]
			if !ok {
				return fmt.Errorf("node source edge references unknown source %q", edge.SourceID)
			}
			if source.Namespace == "kernel" {
				bindTree("kernel")
			}
		}
	}
	for _, value := range values {
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if match[1] != "tree" {
				continue
			}
			bindTree(match[2])
		}
	}
	return nil
}

// actionPlanCommandMetadataSourceTreeClosure returns source namespaces whose
// physical paths may be retained by a consumed generated Kbuild .cmd file.
// Only that output's producer ancestry participates. The source-edge and tree
// intersection identifies a selected source namespace without forwarding
// unrelated object/preparation trees. A namespace is also returned as writable
// only when the producing ancestry staged it as a WorkingTree; this preserves
// relative compiler paths without treating immutable source roots the same way.
func actionPlanCommandMetadataSourceTreeClosure(
	plan *ActionPlan,
	consumer *ActionPlanNode,
) ([]string, []string, error) {
	if plan == nil {
		return nil, nil, fmt.Errorf("Kbuild command metadata source-tree closure requires an action plan")
	}
	plan.ensureNodeLookupIndexes()
	roots := []string{}
	seenRoots := map[string]bool{}
	addRoot := func(producerID string, slot int) error {
		metadata, err := commandMetadataProducerOutput(plan, producerID, slot)
		if err != nil {
			return err
		}
		if metadata && !seenRoots[producerID] {
			seenRoots[producerID] = true
			roots = append(roots, producerID)
		}
		return nil
	}
	for _, input := range consumer.Inputs {
		if err := addRoot(input.ProducerID, input.Slot); err != nil {
			return nil, nil, err
		}
	}
	if err := plan.walkCommandMetadataInputs(consumer.InputSet, commandMetadataGeneratedInput, func(entry ActionPlanInputSetEntry) error {
		return addRoot(entry.ProducerID, entry.Slot)
	}); err != nil {
		return nil, nil, err
	}
	if len(roots) == 0 {
		return nil, nil, nil
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return nil, nil, err
	}
	retained := map[string]bool{}
	staged := map[string]bool{}
	visited := actionPlanNodeVisitSet{}
	for _, root := range roots {
		err := plan.walkCompactKbuildWorkingTreeTopology(root, visited, func(nodeIndex uint32) error {
			producer := plan.Nodes[nodeIndex]
			declaredTrees := make(map[string]bool, len(producer.Trees))
			for _, tree := range producer.Trees {
				declaredTrees[tree] = true
			}
			if producer.Recipe != "" {
				recipe, ok := plan.Recipes[producer.Recipe]
				if !ok {
					return fmt.Errorf("Kbuild command metadata producer %q references unknown recipe %q", producer.ID, producer.Recipe)
				}
				for _, tree := range recipe.WorkingTrees {
					if declaredTrees[tree] {
						staged[tree] = true
					}
				}
			}
			retainSource := func(sourceID string) error {
				source, ok := plan.sourcesByID[sourceID]
				if !ok {
					return fmt.Errorf("Kbuild command metadata producer %q references unknown source %q", producer.ID, sourceID)
				}
				if declaredTrees[source.Namespace] {
					retained[source.Namespace] = true
				}
				return nil
			}
			for _, edge := range producer.Sources {
				if err := retainSource(edge.SourceID); err != nil {
					return err
				}
			}
			return plan.walkCommandMetadataInputs(producer.InputSet, commandMetadataSourceInput, func(entry ActionPlanInputSetEntry) error {
				return retainSource(entry.SourceID)
			})
		})
		if err != nil {
			return nil, nil, err
		}
	}
	working := map[string]bool{}
	for tree := range retained {
		if staged[tree] {
			working[tree] = true
		}
	}
	return slices.Sorted(maps.Keys(retained)), slices.Sorted(maps.Keys(working)), nil
}

func (b *generatedPlanBuilder) internConfigProjectionSources() error {
	for _, pathname := range b.metadata.configProjectionPaths {
		if _, err := ensureActionPlanSource(b.plan, "config", pathname); err != nil {
			return err
		}
	}
	return nil
}
