package kconfig

import (
	"fmt"
	"maps"
	"slices"
)

// addInitialFamilySidecarGroups schedules prerequisites, never grants byte or
// dependency authority. Only already-prospective or precisely classified typed
// compilers are considered. Discovery has its own transactional budget: an
// oversized sidecar frontier cannot erase the existing header/executable work.
// The caller has validated the complete snapshots and conservative family.
func addInitialFamilySidecarGroups(
	family *ActionPlanFamily, variants []ActionPlanFamilyVariant,
	groups map[string]*actionPlanFamilyHeaderDemandGroup,
	maximumRecords, maximumBytes int,
) (bool, error) {
	if maximumRecords <= 0 || maximumBytes <= 0 {
		return false, fmt.Errorf("initial sidecar selection requires positive limits")
	}
	prospective := map[string]string{}
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, group := range groups {
		maps.Copy(prospective, group.consumers)
		maps.Copy(roots, group.roots)
	}
	finalNodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		finalNodes[node.ID] = node
	}
	configPaths := map[string]bool{}
	for _, projection := range resolvedConfigProjections() {
		configPaths[projection.output] = true
	}
	pending := map[string]*actionPlanFamilyHeaderDemandGroup{}
	type association struct {
		consumer string
		root     ActionPlanFamilyExecutionCutRoot
	}
	seen := map[association]bool{}
	records, bytes := 0, 0
	for _, variant := range variants {
		plan := snapshotActionPlanView(variant.Snapshot)
		originals := make(map[string]ActionPlanNode, len(plan.Nodes))
		privatePrehostCopyAllowed := true
		for _, node := range plan.Nodes {
			originals[node.ID] = node
			if slices.Contains(node.Trees, "prehost") {
				privatePrehostCopyAllowed = false
			}
		}
		if !privatePrehostCopyAllowed {
			continue
		}
		originalIDs := family.originalNodeIDs[variant.Name]
		for _, consumer := range plan.Nodes {
			consumerID := originalIDs[consumer.ID]
			consumerKey := variant.Name + "\x00" + consumer.ID
			dependencies, classified := variant.Snapshot.ConfigDependencies[consumer.ID]
			recipe := plan.Recipes[consumer.Recipe]
			invocation := recipe.CompilerInvocation
			if consumerID == "" || !typedFamilyCompilerNode(plan, consumer) || invocation == nil ||
				!invocation.WorkingInputUsesComplete || len(recipe.ArgumentTransforms) != 0 ||
				(prospective[consumerKey] != consumerID && (!classified || !preciseFamilyCompilerNode(plan, consumer, dependencies))) {
				continue
			}
			withoutBases := recipe
			withoutBases.ObservedOutputBases = nil
			otherSemantic := familyRecipeSemanticBindings(withoutBases)
			for _, base := range slices.Sorted(maps.Keys(recipe.ObservedOutputBases)) {
				for _, binding := range recipe.ObservedOutputBases[base] {
					ordinal := slices.Index(recipe.Inputs, binding)
					if ordinal < 0 || ordinal >= len(consumer.Inputs) {
						return false, fmt.Errorf("initial sidecar binding %s/%s/%s has no exact input", variant.Name, consumer.ID, binding)
					}
					edge := consumer.Inputs[ordinal]
					producer, found := originals[edge.ProducerID]
					if !found || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
						return false, fmt.Errorf("initial sidecar binding %s/%s/%s has no exact producer slot", variant.Name, consumer.ID, binding)
					}
					output := producer.Outputs[edge.Slot]
					reference := "input:" + binding
					if edge.Role == "sequence" || output.Tree != "metadata" || !plannerOwnedObservedFamilyState(output) ||
						otherSemantic[reference] || recipe.WorkingInputs[reference] != "" {
						continue
					}
					if recipe.ObservedOutputs[base] != output.ObservedPath {
						return false, fmt.Errorf("initial sidecar binding changed its logical observed path")
					}
					producerID := originalIDs[producer.ID]
					if producerID == "" {
						continue
					}
					final, found := finalNodes[producerID]
					if !found || edge.Slot >= len(final.Outputs) || !executedArtifactOutputOwnership(output, final.Outputs[edge.Slot], edge.Slot) {
						return false, fmt.Errorf("initial sidecar lost its exact reduced output ownership")
					}
					// Persistent materializations are not exclusively internal state
					// uses, even when the direct input also appears as a base.
					store, err := plan.planningActionPlanInputSetStore()
					if err != nil {
						return false, err
					}
					aliased := false
					for _, target := range []ActionPlanInputSetTarget{{Kind: ActionPlanInputSetWorkTarget, Path: output.Path}, {Kind: ActionPlanInputSetTreeTarget, Tree: output.Tree, Path: output.Path}} {
						_, found, err := store.Lookup(consumer.InputSet, target)
						if err != nil {
							return false, err
						}
						aliased = aliased || found
					}
					if aliased {
						continue
					}
					// A cut root is an ordinary slot, never the state envelope. Its
					// complete output vector still authenticates that exact state slot.
					// A validation-private ordinary slot is also eligible: validated
					// snapshots bind it to the exact projected/full comparator, and
					// cut capture closes over both producers and that obligation.
					// Scheduling it does not make it a public family output.
					rootSlot := -1
					for slot, ordinary := range producer.Outputs {
						if ordinary.ObservedPath == "" &&
							!configPaths[ordinary.Path] && !configPaths[actionPlanOutputArtifactPath(ordinary)] {
							rootSlot = slot
							break
						}
					}
					if rootSlot < 0 {
						continue
					}
					ordinary := producer.Outputs[rootSlot]
					if rootSlot >= len(final.Outputs) {
						return false, fmt.Errorf("initial sidecar lost its ordinary reduced root")
					}
					if err := validateInitialFamilyHeaderOutput(ordinary, final.Outputs[rootSlot], rootSlot); err != nil {
						return false, err
					}
					key := "sidecar\x00" + ordinary.Tree + "\x00" + ordinary.Path
					root := ActionPlanFamilyExecutionCutRoot{NodeID: producerID, Slot: rootSlot}
					group := pending[key]
					pair := association{consumer: consumerKey, root: root}
					if seen[pair] {
						continue
					}
					if records >= maximumRecords {
						return true, nil
					}
					records++
					size := 256 + len(consumerKey) + len(consumerID) + len(producerID) + len(ordinary.Tree) + len(ordinary.Path)
					if size > maximumBytes-bytes {
						return true, nil
					}
					bytes += size
					seen[pair] = true
					if !roots[root] && len(roots) >= MaxActionPlanFamilyExecutionCutOutputs {
						return true, nil
					}
					roots[root] = true
					if group == nil {
						group = &actionPlanFamilyHeaderDemandGroup{key: key, roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
						pending[key] = group
					}
					group.roots[root] = true
					group.consumers[consumerKey] = consumerID
				}
			}
		}
	}
	for key, group := range pending {
		if existing := groups[key]; existing != nil {
			maps.Copy(existing.roots, group.roots)
			maps.Copy(existing.consumers, group.consumers)
		} else {
			groups[key] = group
		}
	}
	return false, nil
}
