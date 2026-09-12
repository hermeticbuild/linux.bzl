package kconfig

import "fmt"

func validatePlanCheckpointRows[V any](rows []planCheckpointSelectionEntry[V], profiles map[string]planCheckpointProfile) error {
	var previous compactKbuildSelectionKey
	for i, row := range rows {
		key := compactKbuildSelectionKey{row.Key[0], row.Key[1], row.Key[2]}
		if _, ok := profiles[key.profile]; !ok || key.target == "" {
			return fmt.Errorf("checkpoint selection has an unknown profile or empty target")
		}
		if i > 0 && !compactKbuildSelectionKeyLess(previous, key) {
			return fmt.Errorf("checkpoint selection rows are duplicate or noncanonical")
		}
		previous = key
	}
	return nil
}

func validatePlanCheckpointGraph(plan *ActionPlan, graph planCheckpointSelectionGraph) error {
	for _, err := range []error{
		validatePlanCheckpointRows(graph.Selections, graph.Profiles),
		validatePlanCheckpointRows(graph.Initial, graph.Profiles),
		validatePlanCheckpointRows(graph.Generated, graph.Profiles),
		validatePlanCheckpointRows(graph.Producers, graph.Profiles),
		validatePlanCheckpointRows(graph.Forwarding, graph.Profiles),
	} {
		if err != nil {
			return err
		}
	}
	nodes := map[string]ActionPlanNode{}
	for _, node := range plan.Nodes {
		nodes[node.ID] = node
	}
	for _, row := range graph.Producers {
		if _, ok := nodes[row.Value]; !ok {
			return fmt.Errorf("checkpoint owner names an absent producer")
		}
	}
	for _, rows := range [][]planCheckpointSelectionEntry[[]CompactKbuildVisibleArtifact]{graph.Initial, graph.Generated} {
		for _, row := range rows {
			for _, artifact := range row.Value {
				if _, ok := graph.Profiles[artifact.Profile]; !ok {
					return fmt.Errorf("checkpoint visible artifact has an unknown profile")
				}
			}
		}
	}
	knownKey := func(key [3]string) bool { _, ok := graph.Profiles[key[0]]; return ok && key[1] != "" }
	for _, owners := range graph.OutputOwners {
		for _, key := range owners {
			if !knownKey(key) {
				return fmt.Errorf("checkpoint output owner has invalid provenance")
			}
		}
	}
	for profile, targets := range graph.ProfileTargets {
		if _, ok := graph.Profiles[profile]; !ok {
			return fmt.Errorf("checkpoint target index has an unknown profile")
		}
		for target, key := range targets {
			if !knownKey(key) || target == "" {
				return fmt.Errorf("checkpoint target index has invalid provenance")
			}
		}
	}
	for target, keys := range graph.SelectionsByTarget {
		if target == "" {
			return fmt.Errorf("checkpoint has an empty indexed target")
		}
		for _, key := range keys {
			if !knownKey(key) {
				return fmt.Errorf("checkpoint target has invalid provenance")
			}
		}
	}
	for profile, targets := range graph.TargetInvocations {
		if _, ok := graph.Profiles[profile]; !ok {
			return fmt.Errorf("checkpoint invocation has an unknown parent")
		}
		for target, children := range targets {
			if target == "" {
				return fmt.Errorf("checkpoint invocation has an empty target")
			}
			for _, child := range children {
				if _, ok := graph.Profiles[child]; !ok {
					return fmt.Errorf("checkpoint invocation has an unknown child")
				}
			}
		}
	}
	for _, profile := range graph.Profiles {
		for _, parent := range profile.Predecessors {
			if _, ok := graph.Profiles[parent]; !ok {
				return fmt.Errorf("checkpoint profile has an unknown predecessor")
			}
		}
		if !profile.TemplateSet && len(profile.Roots) != 0 {
			return fmt.Errorf("checkpoint root has no source lookup context")
		}
	}
	for node := range plan.compilerProbeInvocations {
		if _, ok := nodes[node]; !ok {
			return fmt.Errorf("checkpoint compiler projection has an absent node")
		}
	}
	for node := range plan.compoundCompilerProbes {
		if _, ok := nodes[node]; !ok {
			return fmt.Errorf("checkpoint compound projection has an absent node")
		}
	}
	for _, node := range plan.projectedGeneratorValidations {
		if _, ok := nodes[node]; !ok {
			return fmt.Errorf("checkpoint projection validation has an absent node")
		}
	}
	for node := range plan.projectedGeneratorInternalNodes {
		if _, ok := nodes[node]; !ok {
			return fmt.Errorf("checkpoint projected internal node is absent")
		}
	}
	for output := range plan.projectedGeneratorInternalOutputs {
		node, ok := nodes[output.producerID]
		if !ok || output.slot < 0 || output.slot >= len(node.Outputs) {
			return fmt.Errorf("checkpoint projected internal output is absent")
		}
	}
	for node, original := range plan.projectedGeneratorOriginalOutputs {
		if _, ok := nodes[node]; !ok || original.TargetSlot < 0 || original.TargetSlot >= len(original.Outputs) {
			return fmt.Errorf("checkpoint projected original outputs are invalid")
		}
	}
	return nil
}
