package kconfig

// In-tree modules are ordinary selected Kbuild targets. This file contains
// only their stable public facade: it validates the native outputs already
// materialized by generic rule lowering, registers products, and derives the
// module manifest from the exact selected .ko targets.

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

func (m *CompactMetadata) appendSelectedModuleActionPlanProducts(
	plan *ActionPlan,
	selections *compactKbuildSelectionGraph,
) error {
	if m == nil {
		return fmt.Errorf("selected module products require one resolved config")
	}
	if plan == nil {
		return fmt.Errorf("selected module products require a non-nil action plan")
	}
	if selections == nil {
		return fmt.Errorf("selected module products require the selected Kbuild graph")
	}
	config := m.Config
	modules := []string{}
	for _, selection := range selections.orderedSelections() {
		if !strings.HasSuffix(selection.Target, ".ko") {
			continue
		}
		_, _, tree, err := selectedKbuildPlanOutput(plan, config, selections, selection.Target)
		if err != nil {
			return fmt.Errorf("selected in-tree module %q: %w", selection.Target, err)
		}
		if tree != "modules" {
			return fmt.Errorf("selected in-tree module %q is in output tree %q, want modules", selection.Target, tree)
		}
		modules = append(modules, selection.Target)
	}
	sort.Strings(modules)

	for _, product := range []struct {
		name     string
		basename string
		tree     string
	}{
		{name: "module_symvers", basename: "Module.symvers", tree: "metadata"},
		{name: "modules_order", basename: "modules.order", tree: "modules"},
	} {
		target, selected, targetErr := selectedModuleProductTarget(plan, config, selections, product.basename)
		if targetErr != nil {
			return fmt.Errorf("selected module product %s: %w", product.name, targetErr)
		}
		tree := product.tree
		if selected {
			_, _, selectedTree, err := selectedKbuildPlanOutput(plan, config, selections, target)
			if err != nil {
				return fmt.Errorf("selected module product %s: %w", product.name, err)
			}
			tree = selectedTree
		} else if len(modules) == 0 {
			// With no selected .ko targets, upstream Kbuild legitimately omits
			// Module.symvers and/or modules.order (for example CONFIG_MODULES=n).
			// Keep the stable SDK facade total by projecting the graph-derived
			// empty set, while still requiring native products whenever a module
			// was actually selected.
			target = product.basename
			if err := appendEmptySelectedModuleProduct(plan, product.name, tree, target); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("selected module product %s: Kbuild has no selected %q target", product.name, product.basename)
		}
		if tree != product.tree {
			return fmt.Errorf(
				"selected module product %s target %q is in output tree %q, want %q",
				product.name, target, tree, product.tree,
			)
		}
		if err := appendActionPlanProduct(plan, ActionPlanProduct{
			Name: product.name, Tree: tree, Path: target,
		}); err != nil {
			return err
		}
	}

	contents := strings.Join(modules, "\n")
	if len(modules) != 0 {
		contents += "\n"
	}
	if err := appendSelectedModulesManifest(plan, contents); err != nil {
		return err
	}
	return appendActionPlanProduct(plan, ActionPlanProduct{
		Name: "modules", Tree: "modules", Path: LinuxKernelTreeRootMarker,
	})
}

func selectedModuleProductTarget(
	plan *ActionPlan,
	config CompactConfig,
	selections *compactKbuildSelectionGraph,
	basename string,
) (string, bool, error) {
	type candidate struct {
		target string
		output actionPlanOutputRef
	}
	candidates := []candidate{}
	seen := map[string]bool{}
	for _, selection := range selections.orderedSelections() {
		if selection.Target != basename && !strings.HasSuffix(selection.Target, "/"+basename) {
			continue
		}
		producer, slot, _, err := selectedKbuildPlanOutput(plan, config, selections, selection.Target)
		if err != nil {
			return "", false, fmt.Errorf("resolve candidate %q: %w", selection.Target, err)
		}
		if seen[selection.Target] {
			continue
		}
		seen[selection.Target] = true
		candidates = append(candidates, candidate{
			target: selection.Target,
			output: actionPlanOutputRef{producerID: producer, slot: slot},
		})
	}
	if len(candidates) == 0 {
		return "", false, nil
	}
	if len(candidates) == 1 {
		return candidates[0].target, true, nil
	}
	if basename != "modules.order" {
		details := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			details = append(details, fmt.Sprintf("%q via %s slot %d", candidate.target, candidate.output.producerID, candidate.output.slot))
		}
		sort.Strings(details)
		return "", false, fmt.Errorf(
			"Kbuild selected %d ambiguous %s products: %s",
			len(candidates), basename, strings.Join(details, ", "),
		)
	}

	indexByID := make(map[string]int, len(plan.Nodes))
	for index, node := range plan.Nodes {
		indexByID[node.ID] = index
	}
	outputs := make([]actionPlanOutputRef, 0, len(candidates))
	for _, candidate := range candidates {
		outputs = append(outputs, candidate.output)
	}
	frontier, err := nativeModuleProductOutputFrontier(plan.Nodes, indexByID, outputs)
	if err != nil {
		return "", false, fmt.Errorf("resolve native output frontier: %w", err)
	}
	frontierOutputs := make(map[actionPlanOutputRef]bool, len(frontier))
	for _, output := range frontier {
		frontierOutputs[output] = true
	}
	frontierTargets := map[string]bool{}
	details := []string{}
	for _, candidate := range candidates {
		if !frontierOutputs[candidate.output] {
			continue
		}
		frontierTargets[candidate.target] = true
		details = append(details, fmt.Sprintf("%q via %s slot %d", candidate.target, candidate.output.producerID, candidate.output.slot))
	}
	if len(frontierTargets) != 1 {
		sort.Strings(details)
		return "", false, fmt.Errorf(
			"Kbuild selected %d independent downstream %s products: %s",
			len(frontierTargets), basename, strings.Join(details, ", "),
		)
	}
	for target := range frontierTargets {
		return target, true, nil
	}
	return "", false, fmt.Errorf("Kbuild selected no downstream %s product", basename)
}

// nativeModuleProductOutputFrontier removes modules.order artifacts consumed
// by another selected modules.order producer. The relation is output-slot
// precise: one action may publish several directory aggregates, and consuming
// one of them does not make its siblings upstream. Ordering-only and private
// working-tree edges do not prove that an aggregate contains another file.
func nativeModuleProductOutputFrontier(
	nodes []ActionPlanNode,
	indexByID map[string]int,
	outputs []actionPlanOutputRef,
) ([]actionPlanOutputRef, error) {
	unique := []actionPlanOutputRef{}
	candidates := map[actionPlanOutputRef]bool{}
	candidateProducers := map[string]bool{}
	orderedCandidateProducers := []string{}
	validate := func(output actionPlanOutputRef) (int, error) {
		index, ok := indexByID[output.producerID]
		if !ok {
			return 0, fmt.Errorf("terminal dependency %q is absent from the plan", output.producerID)
		}
		if output.slot < 0 || output.slot >= len(nodes[index].Outputs) {
			return 0, fmt.Errorf(
				"terminal dependency %q output slot %d is outside its %d outputs",
				output.producerID, output.slot, len(nodes[index].Outputs),
			)
		}
		return index, nil
	}
	for _, output := range outputs {
		if _, err := validate(output); err != nil {
			return nil, err
		}
		if candidates[output] {
			continue
		}
		candidates[output] = true
		if !candidateProducers[output.producerID] {
			candidateProducers[output.producerID] = true
			orderedCandidateProducers = append(orderedCandidateProducers, output.producerID)
		}
		unique = append(unique, output)
	}
	includeEdge := func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID != "" && edge.Role != compactKbuildWorkingClosureInputRole &&
			edge.Role != compactKbuildOverwriteInputRole && edge.Role != "sequence" && edge.Role != "order-only"
	}
	pending := []ActionPlanNodeEdge{}
	for _, producer := range orderedCandidateProducers {
		index := indexByID[producer]
		for _, edge := range nodes[index].Inputs {
			if includeEdge(edge) {
				pending = append(pending, edge)
			}
		}
	}
	upstream := map[actionPlanOutputRef]bool{}
	expanded := map[string]bool{}
	for len(pending) != 0 {
		last := len(pending) - 1
		edge := pending[last]
		pending = pending[:last]
		output := actionPlanOutputRef{producerID: edge.ProducerID, slot: edge.Slot}
		index, err := validate(output)
		if err != nil {
			return nil, err
		}
		// Observe the exact incoming slot before deduplicating expansion of the
		// producer action; a grouped producer may be reached through more than
		// one output slot.
		if candidates[output] {
			upstream[output] = true
		}
		if expanded[output.producerID] {
			continue
		}
		expanded[output.producerID] = true
		for _, dependency := range nodes[index].Inputs {
			if includeEdge(dependency) {
				pending = append(pending, dependency)
			}
		}
	}
	frontier := make([]actionPlanOutputRef, 0, len(unique))
	for _, output := range unique {
		if !upstream[output] {
			frontier = append(frontier, output)
		}
	}
	return frontier, nil
}

func appendEmptySelectedModuleProduct(plan *ActionPlan, product, tree, target string) error {
	node := ActionPlanNode{
		Stage: "target", Kind: "metadata", Tool: "actionfile", Product: product,
		Outputs: []ActionPlanOutput{{Tree: tree, Path: target}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile",
		Arguments: []string{
			"-out", "${output:00000000}",
			"-content_base64", base64.StdEncoding.EncodeToString(nil),
		},
		Outputs: []string{"00000000"},
	}
	_, err := appendActionPlanNode(plan, node, recipe)
	return err
}

func appendSelectedModulesManifest(plan *ActionPlan, contents string) error {
	node := ActionPlanNode{
		Stage: "target", Kind: "metadata", Tool: "actionfile", Product: "modules",
		Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "modules.manifest"}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile",
		Arguments: []string{
			"-out", "${output:00000000}",
			"-content_base64", base64.StdEncoding.EncodeToString([]byte(contents)),
		},
		Outputs: []string{"00000000"},
	}
	_, err := appendActionPlanNode(plan, node, recipe)
	return err
}
