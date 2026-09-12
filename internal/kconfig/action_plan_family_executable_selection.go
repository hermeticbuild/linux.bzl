package kconfig

import (
	"fmt"
	"maps"
	"slices"
)

// This private helper is called only after every complete snapshot and the
// resulting family have been validated by BuildConservativeActionPlanFamily.
// It mutates only the caller-local scheduling groups, never a graph or recipe.
// An overflow abandons the entire opt-in selection, matching header admission.
type actionPlanFamilyDemandUsage struct{ records, bytes int }

func addInitialFamilyExecutableGroups(
	family *ActionPlanFamily, variants []ActionPlanFamilyVariant,
	groups map[string]*actionPlanFamilyHeaderDemandGroup,
	records, bytes, maximumRecords, maximumBytes int,
) (bool, error) {
	return addInitialFamilyExecutableGroupsWithUsage(family, variants, groups,
		records, bytes, maximumRecords, maximumBytes, nil)
}

func addInitialFamilyExecutableGroupsWithUsage(
	family *ActionPlanFamily, variants []ActionPlanFamilyVariant,
	groups map[string]*actionPlanFamilyHeaderDemandGroup,
	records, bytes, maximumRecords, maximumBytes int,
	usage *actionPlanFamilyDemandUsage,
) (bool, error) {
	defer func() {
		if usage != nil {
			*usage = actionPlanFamilyDemandUsage{records: records, bytes: bytes}
		}
	}()
	finalNodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		finalNodes[node.ID] = node
	}
	configPaths := map[string]bool{}
	for _, projection := range resolvedConfigProjections() {
		configPaths[projection.output] = true
	}
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	// Only the primary header frontier may admit an otherwise opaque consumer.
	// Snapshot it before adding executable groups so those groups cannot make
	// additional opaque consumers eligible merely through a shared helper.
	prospective := map[string]string{}
	for _, group := range groups {
		maps.Copy(prospective, group.consumers)
		for root := range group.roots {
			roots[root] = true
		}
	}
	for _, variant := range variants {
		plan := snapshotActionPlanView(variant.Snapshot)
		originals := make(map[string]ActionPlanNode, len(variant.Snapshot.Nodes))
		for _, node := range variant.Snapshot.Nodes {
			originals[node.ID] = node
		}
		originalIDs := family.originalNodeIDs[variant.Name]
		for _, consumer := range variant.Snapshot.Nodes {
			recipe := variant.Snapshot.Recipes[consumer.Recipe]
			invocation := recipe.CompilerInvocation
			if !typedFamilyCompilerNode(plan, consumer) || invocation == nil ||
				!invocation.WorkingInputUsesComplete || len(recipe.ArgumentTransforms) != 0 {
				continue
			}
			consumerID := originalIDs[consumer.ID]
			if consumerID == "" {
				// Dead original work must not be resurrected by this optimization.
				continue
			}
			consumerKey := variant.Name + "\x00" + consumer.ID
			dependencies, classified := variant.Snapshot.ConfigDependencies[consumer.ID]
			if prospective[consumerKey] != consumerID &&
				(!classified || !preciseFamilyCompilerNode(plan, consumer, dependencies)) {
				// Unknown opaque consumers remain ordinary build work. Declining
				// speculative helper execution does not change their dependencies
				// or grant precision, and avoids protecting unrelated ancestors.
				continue
			}
			written := familyRecipeSemanticWorkingPaths(recipe)
			for _, binding := range recipe.ExecutableInputs {
				if records >= maximumRecords {
					return true, nil
				}
				records++
				ordinal := slices.Index(recipe.Inputs, binding)
				if ordinal < 0 || ordinal >= len(consumer.Inputs) {
					return false, fmt.Errorf("initial executable binding %s/%s/%s has no exact input", variant.Name, consumer.ID, binding)
				}
				edge := consumer.Inputs[ordinal]
				producer, found := originals[edge.ProducerID]
				if !found || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
					return false, fmt.Errorf("initial executable binding %s/%s/%s has no exact producer slot", variant.Name, consumer.ID, binding)
				}
				output := producer.Outputs[edge.Slot]
				reference := "input:" + binding
				pathname := recipe.WorkingInputs[reference]
				size := 256 + len(variant.Name) + len(consumer.ID) + len(producer.ID) + len(binding) +
					len(pathname) + len(output.Tree) + len(output.Path) + len(output.ArtifactPath)
				if size > maximumBytes-bytes {
					return true, nil
				}
				bytes += size
				// Only declared auxiliary execution at a stable staging destination
				// is useful to the verified byte-substitution path. Source
				// executables, implicit tree lookup, and mutable aliases are not roots.
				if edge.Role == "sequence" || pathname != output.Path || written[pathname] ||
					!slices.Contains(invocation.AuxiliaryWorkingInputUses, reference) ||
					output.ObservedPath != "" || familyViewTree(output.Tree) ||
					configPaths[output.Path] || configPaths[actionPlanOutputArtifactPath(output)] || configPaths[pathname] {
					continue
				}
				producerID := originalIDs[producer.ID]
				if producerID == "" {
					continue
				}
				final, found := finalNodes[producerID]
				if !found || edge.Slot >= len(final.Outputs) {
					return false, fmt.Errorf("initial executable lost reduced output %s[%d]", producerID, edge.Slot)
				}
				if err := validateInitialFamilyHeaderOutput(output, final.Outputs[edge.Slot], edge.Slot); err != nil {
					return false, fmt.Errorf("initial executable %s/%s[%d]: %w", variant.Name, producerID, edge.Slot, err)
				}
				root := ActionPlanFamilyExecutionCutRoot{NodeID: producerID, Slot: edge.Slot}
				if !roots[root] && len(roots) >= MaxActionPlanFamilyExecutionCutOutputs {
					return true, nil
				}
				roots[root] = true
				key := output.Tree + "\x00" + output.Path
				group := groups[key]
				if group == nil {
					group = &actionPlanFamilyHeaderDemandGroup{key: key,
						roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
					groups[key] = group
				}
				group.roots[root] = true
				group.consumers[consumerKey] = consumerID
			}
		}
	}
	return false, nil
}
