package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
)

const (
	compactKbuildSideOutputStateDirectory      = ".linux-bzl-observed-side-output"
	compactKbuildSideOutputResolutionDirectory = ".linux-bzl-side-output-resolution"
)

type compactKbuildSideOutputCandidateState struct {
	candidate compactKbuildSelectionKey
	input     compactKbuildRuleInput
}

// compactKbuildSideOutputStateGraph is the candidate-only projection of the
// selected source DAG for one observed logical path. Each candidate consumes
// only its nearest candidate predecessors' absolute states. Maximal candidates
// are the only states a final consumer needs to merge.
type compactKbuildSideOutputStateGraph struct {
	parents map[compactKbuildSelectionKey][]compactKbuildSelectionKey
	maximal []compactKbuildSelectionKey
}

// compactKbuildSideOutputCandidateFrontier keeps the common empty and
// singleton frontiers inline. A map is needed only at a real DAG join. This is
// immutable once returned from nearest below; merge always builds in the
// receiver and never mutates a memoized frontier's map.
type compactKbuildSideOutputCandidateFrontier struct {
	single    compactKbuildSelectionKey
	hasSingle bool
	multiple  map[compactKbuildSelectionKey]bool
}

func compactKbuildSingletonSideOutputCandidateFrontier(
	candidate compactKbuildSelectionKey,
) compactKbuildSideOutputCandidateFrontier {
	return compactKbuildSideOutputCandidateFrontier{single: candidate, hasSingle: true}
}

func (f *compactKbuildSideOutputCandidateFrontier) add(candidate compactKbuildSelectionKey) {
	if f.multiple != nil {
		f.multiple[candidate] = true
		return
	}
	if !f.hasSingle {
		f.single = candidate
		f.hasSingle = true
		return
	}
	if f.single == candidate {
		return
	}
	f.multiple = map[compactKbuildSelectionKey]bool{
		f.single:  true,
		candidate: true,
	}
	f.single = compactKbuildSelectionKey{}
	f.hasSingle = false
}

func (f *compactKbuildSideOutputCandidateFrontier) merge(
	other compactKbuildSideOutputCandidateFrontier,
) {
	if other.multiple != nil {
		for candidate := range other.multiple {
			f.add(candidate)
		}
		return
	}
	if other.hasSingle {
		f.add(other.single)
	}
}

func (f compactKbuildSideOutputCandidateFrontier) appendTo(
	destination []compactKbuildSelectionKey,
) []compactKbuildSelectionKey {
	if f.multiple != nil {
		for candidate := range f.multiple {
			destination = append(destination, candidate)
		}
		return destination
	}
	if f.hasSingle {
		destination = append(destination, f.single)
	}
	return destination
}

func (g *compactKbuildSelectionGraph) compactKbuildSideOutputStateGraph(
	metadata *CompactMetadata,
	candidates []compactKbuildSelectionKey,
) (compactKbuildSideOutputStateGraph, error) {
	if g == nil || metadata == nil {
		return compactKbuildSideOutputStateGraph{}, fmt.Errorf("cannot project side-output state without selections and metadata")
	}
	ordered, err := g.canonicalSideOutputCandidates(candidates)
	if err != nil {
		return compactKbuildSideOutputStateGraph{}, err
	}
	candidateSet := make(map[compactKbuildSelectionKey]bool, len(ordered))
	for _, candidate := range ordered {
		candidateSet[candidate] = true
	}
	result := compactKbuildSideOutputStateGraph{
		parents: make(map[compactKbuildSelectionKey][]compactKbuildSelectionKey, len(ordered)),
	}
	if len(ordered) == 0 {
		return result, nil
	}

	// A transparent selected node may sit between two materialized candidates.
	// Memoize the nearest-candidate frontier below each such node, stopping as
	// soon as a candidate is reached because its absolute state already carries
	// the complete earlier history.
	frontierMemo := map[compactKbuildSelectionKey]compactKbuildSideOutputCandidateFrontier{}
	frontierVisiting := map[compactKbuildSelectionKey]bool{}
	var nearest func(compactKbuildSelectionKey) (compactKbuildSideOutputCandidateFrontier, error)
	nearest = func(key compactKbuildSelectionKey) (compactKbuildSideOutputCandidateFrontier, error) {
		key = g.compactKbuildGroupedSelectionRepresentative(key)
		if candidateSet[key] {
			return compactKbuildSingletonSideOutputCandidateFrontier(key), nil
		}
		if cached, ok := frontierMemo[key]; ok {
			return cached, nil
		}
		if frontierVisiting[key] {
			return compactKbuildSideOutputCandidateFrontier{}, fmt.Errorf("side-output state dependency cycle reaches %s", compactKbuildSelectionKeyString(key))
		}
		frontierVisiting[key] = true
		frontier := compactKbuildSideOutputCandidateFrontier{}
		dependencies, err := g.selectionDependencies(metadata, key)
		if err != nil {
			return compactKbuildSideOutputCandidateFrontier{}, err
		}
		for _, dependency := range dependencies {
			dependency = g.compactKbuildGroupedSelectionRepresentative(dependency)
			if dependency == key {
				continue
			}
			below, dependencyErr := nearest(dependency)
			if dependencyErr != nil {
				return compactKbuildSideOutputCandidateFrontier{}, dependencyErr
			}
			frontier.merge(below)
		}
		delete(frontierVisiting, key)
		frontierMemo[key] = frontier
		return frontier, nil
	}

	consumed := map[compactKbuildSelectionKey]bool{}
	for _, candidate := range ordered {
		parentFrontier := compactKbuildSideOutputCandidateFrontier{}
		dependencies, err := g.selectionDependencies(metadata, candidate)
		if err != nil {
			return compactKbuildSideOutputStateGraph{}, err
		}
		for _, dependency := range dependencies {
			dependency = g.compactKbuildGroupedSelectionRepresentative(dependency)
			if dependency == candidate {
				continue
			}
			frontier, dependencyErr := nearest(dependency)
			if dependencyErr != nil {
				return compactKbuildSideOutputStateGraph{}, dependencyErr
			}
			parentFrontier.merge(frontier)
		}
		parents := parentFrontier.appendTo(nil)
		filteredParents := parents[:0]
		for _, parent := range parents {
			if parent == candidate {
				continue
			}
			consumed[parent] = true
			filteredParents = append(filteredParents, parent)
		}
		parents = filteredParents
		sort.Slice(parents, func(i, j int) bool {
			return compactKbuildSelectionKeyLess(parents[i], parents[j])
		})
		result.parents[candidate] = parents
	}

	const (
		unvisited = iota
		visiting
		visited
	)
	state := map[compactKbuildSelectionKey]int{}
	topologicalOrder := map[compactKbuildSelectionKey]int{}
	topologicalCandidates := make([]compactKbuildSelectionKey, 0, len(ordered))
	var visit func(compactKbuildSelectionKey) error
	visit = func(candidate compactKbuildSelectionKey) error {
		switch state[candidate] {
		case visiting:
			return fmt.Errorf("side-output candidate state graph contains a cycle at %s", compactKbuildSelectionKeyString(candidate))
		case visited:
			return nil
		}
		state[candidate] = visiting
		for _, parent := range result.parents[candidate] {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[candidate] = visited
		topologicalOrder[candidate] = len(topologicalOrder)
		topologicalCandidates = append(topologicalCandidates, candidate)
		return nil
	}
	for _, candidate := range ordered {
		if err := visit(candidate); err != nil {
			return compactKbuildSideOutputStateGraph{}, err
		}
		if !consumed[candidate] {
			result.maximal = append(result.maximal, candidate)
		}
	}
	// Stopping at a candidate on each dependency path is not sufficient at a
	// join: another path may reach an ancestor of that same candidate. Its
	// absolute state is already carried by the descendant, and merging both
	// can incorrectly report two ordered writers as unordered. Reduce only
	// this same-path candidate DAG, never ordinary command prerequisites.
	// Reduce ancestors first so each later traversal sees their compact DAG.
	for _, candidate := range topologicalCandidates {
		parents := result.parents[candidate]
		if len(parents) < 2 {
			continue
		}
		newestFirst := append([]compactKbuildSelectionKey(nil), parents...)
		sort.Slice(newestFirst, func(i, j int) bool {
			return topologicalOrder[newestFirst[i]] > topologicalOrder[newestFirst[j]]
		})
		oldest := topologicalOrder[newestFirst[len(newestFirst)-1]]
		seen := map[compactKbuildSelectionKey]bool{}
		redundant := map[compactKbuildSelectionKey]bool{}
		for _, parent := range newestFirst {
			if seen[parent] {
				redundant[parent] = true
				continue
			}
			pending := []compactKbuildSelectionKey{parent}
			for len(pending) != 0 {
				ancestor := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				if seen[ancestor] || topologicalOrder[ancestor] < oldest {
					continue
				}
				seen[ancestor] = true
				pending = append(pending, result.parents[ancestor]...)
			}
		}
		// Retain the existing canonical order and no transitive-closure cache.
		frontier := parents[:0]
		for _, parent := range parents {
			if !redundant[parent] {
				frontier = append(frontier, parent)
			}
		}
		result.parents[candidate] = frontier
	}
	return result, nil
}

func compactKbuildSideOutputIdentity(demand compactKbuildSideOutputDemand) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("linux-bzl-side-output-demand-v1\x00"))
	_, _ = hash.Write([]byte(compactKbuildSelectionKeyString(demand.consumer)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(demand.stage))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(demand.output.Tree))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(demand.output.Path))
	return hex.EncodeToString(hash.Sum(nil))
}

func compactKbuildSideOutputStateObservation(
	demand compactKbuildSideOutputDemand,
	candidate compactKbuildSelectionKey,
) (compactKbuildObservedOutput, error) {
	tree, ok := actionPlanStageScratchTree(candidate.stage)
	if !ok {
		return compactKbuildObservedOutput{}, fmt.Errorf(
			"side-output candidate %s has unsupported stage %q",
			compactKbuildSelectionKeyString(candidate), candidate.stage,
		)
	}
	// One candidate/path pair has one observation, even when several later
	// consumers demand those bytes. Consumer identity belongs to the resolver
	// output, not the candidate capture; duplicating the observation would map
	// two declared outputs onto the same working path in one recipe.
	digest := sha256.Sum256([]byte(
		"linux-bzl-side-output-state-v1\x00" + compactKbuildSelectionKeyString(candidate) + "\x00" +
			demand.output.Path,
	))
	statePath := path.Join(
		compactKbuildSideOutputStateDirectory,
		hex.EncodeToString(digest[:])+".state",
	)
	return compactKbuildObservedOutput{
		output: ActionPlanOutput{Tree: tree, Path: statePath},
		path:   demand.output.Path,
	}, nil
}

func appendCompactKbuildSideOutputResolver(
	plan *ActionPlan,
	graph *compactKbuildSelectionGraph,
	metadata *CompactMetadata,
	demand compactKbuildSideOutputDemand,
	states []compactKbuildSideOutputCandidateState,
) (compactKbuildRuleInput, error) {
	if plan == nil {
		return compactKbuildRuleInput{}, fmt.Errorf("cannot resolve a Kbuild side output without an action plan")
	}
	if len(states) == 0 {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"Kbuild side output %q for %s has no observed candidate states",
			demand.output.Path, compactKbuildSelectionKeyString(demand.consumer),
		)
	}
	if !actionPlanStageOwnsOutputTree(demand.stage, demand.output.Tree) {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"Kbuild side output %q resolver stage/tree %s/%s is incompatible",
			demand.output.Path, demand.stage, demand.output.Tree,
		)
	}

	if graph == nil || metadata == nil {
		return compactKbuildRuleInput{}, fmt.Errorf("cannot resolve a Kbuild side output without selection provenance")
	}
	states = append([]compactKbuildSideOutputCandidateState(nil), states...)
	sort.Slice(states, func(i, j int) bool {
		if states[i].candidate != states[j].candidate {
			return compactKbuildSelectionKeyLess(states[i].candidate, states[j].candidate)
		}
		if states[i].input.producer != states[j].input.producer {
			return states[i].input.producer < states[j].input.producer
		}
		return states[i].input.slot < states[j].input.slot
	})
	stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, demand.candidates)
	if err != nil {
		return compactKbuildRuleInput{}, fmt.Errorf("project final Kbuild side-output state for %q: %w", demand.output.Path, err)
	}
	wantCandidates := stateGraph.maximal
	if len(states) != len(wantCandidates) {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"Kbuild side output %q has %d maximal candidate states, want %d",
			demand.output.Path, len(states), len(wantCandidates),
		)
	}
	for index := range states {
		if states[index].candidate != wantCandidates[index] {
			return compactKbuildRuleInput{}, fmt.Errorf(
				"Kbuild side output %q candidate state %d is %s, want maximal %s",
				demand.output.Path, index,
				compactKbuildSelectionKeyString(states[index].candidate),
				compactKbuildSelectionKeyString(wantCandidates[index]),
			)
		}
	}
	for index := 1; index < len(states); index++ {
		if states[index].candidate == states[index-1].candidate ||
			(states[index].input.producer == states[index-1].input.producer && states[index].input.slot == states[index-1].input.slot) {
			return compactKbuildRuleInput{}, fmt.Errorf(
				"Kbuild side output %q repeats candidate state %s slot %d",
				demand.output.Path, states[index].input.producer, states[index].input.slot,
			)
		}
	}

	identity := compactKbuildSideOutputIdentity(demand)
	resolvedOutput := demand.output
	resolvedOutput.ArtifactPath = path.Join(
		compactKbuildSideOutputResolutionDirectory,
		identity,
		demand.output.Path,
	)
	node := ActionPlanNode{
		Stage: demand.stage, Kind: "generate", Tool: "actionfile", Product: demand.product,
		Outputs: []ActionPlanOutput{resolvedOutput},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments:     []string{"-out", "${output:00000000}"},
		ArgumentsFile: true,
		Outputs:       []string{"00000000"},
	}
	for _, state := range states {
		input := state.input
		if input.producer == "" || input.slot < 0 {
			return compactKbuildRuleInput{}, fmt.Errorf(
				"Kbuild side output %q has invalid candidate state %#v",
				demand.output.Path, input,
			)
		}
		binding := fmt.Sprintf("state:%08d", len(node.Inputs))
		node.Inputs = append(node.Inputs, ActionPlanNodeEdge{
			Role: "state", ProducerID: input.producer, Slot: input.slot,
		})
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.Arguments = append(recipe.Arguments, "-state", "${input:"+binding+"}")
	}
	producer, err := appendActionPlanNode(plan, node, recipe)
	if err != nil {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"append Kbuild side-output resolver for %q: %w", demand.output.Path, err,
		)
	}
	return compactKbuildRuleInput{
		path: demand.output.Path, producer: producer, slot: 0,
	}, nil
}
