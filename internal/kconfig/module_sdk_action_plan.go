package kconfig

// The module SDK is the configured Kbuild object tree, projected without a
// second external-module recipe language. External modules run the same
// source-derived Make/Kbuild planner against this tree as in-tree targets.

import (
	"fmt"
	"sort"
)

type moduleSDKSelectedArtifact struct {
	producer string
	slot     int
	tree     string
	path     string
}

type moduleSDKProjection struct {
	artifact moduleSDKSelectedArtifact
	role     string
	path     string
}

// appendModuleSDKActionPlanNodes projects the source-selected preparation
// closure to its original object-tree paths. Host-scoped preparation actions
// and bootstrap actions already have generic prep-tree projections, so the
// exported SDK has one lifecycle boundary and does not reconstruct toolchain
// behavior from physical host stages.
func (m *CompactMetadata) appendModuleSDKActionPlanNodes(plan *ActionPlan) error {
	if m == nil {
		return fmt.Errorf("module SDK action plan requires one resolved config")
	}
	if plan == nil {
		return fmt.Errorf("module SDK action plan requires a non-nil action plan")
	}

	projections := map[string]moduleSDKProjection{}
	add := func(role string, artifact moduleSDKSelectedArtifact) error {
		if artifact.path == "" {
			return fmt.Errorf("module SDK %s has an empty public destination", role)
		}
		existing, exists := projections[artifact.path]
		if exists {
			if existing.artifact.producer == artifact.producer && existing.artifact.slot == artifact.slot {
				return nil
			}
			// Prefer ordinary action dependencies, then Kbuild's source-derived
			// overwrite provenance for versions which intentionally have no direct
			// action edge.
			// A deterministic node or marker ordering is never evidence; unresolved
			// conflicts remain ambiguous and fail closed.
			winner, ordered, err := moduleSDKOrderedProducer(
				plan, artifact.path, existing.artifact.producer, artifact.producer,
			)
			if err != nil {
				return fmt.Errorf("order module SDK destination %q: %w", artifact.path, err)
			}
			if ordered {
				if winner == existing.artifact.producer {
					return nil
				}
				if winner == artifact.producer {
					projections[artifact.path] = moduleSDKProjection{artifact: artifact, role: role, path: artifact.path}
					return nil
				}
				return fmt.Errorf("module SDK destination %q selected unknown writer %q", artifact.path, winner)
			}
			return fmt.Errorf(
				"module SDK destination %q is claimed by %s (%s/%s) and %s (%s/%s)",
				artifact.path,
				existing.role, existing.artifact.tree, existing.artifact.path,
				role, artifact.tree, artifact.path,
			)
		}
		projections[artifact.path] = moduleSDKProjection{artifact: artifact, role: role, path: artifact.path}
		return nil
	}

	for _, node := range plan.Nodes {
		if node.Stage != "prep" {
			continue
		}
		for slot, output := range node.Outputs {
			if output.Tree != "prep" || (!actionPlanOutputIsCanonical(output) && !output.persistent) {
				continue
			}
			if err := add("selected prep output", moduleSDKSelectedArtifact{
				producer: node.ID,
				slot:     slot,
				tree:     output.Tree,
				path:     output.Path,
			}); err != nil {
				return err
			}
		}
	}

	symvers, err := selectedModuleSDKProduct(plan, "module_symvers")
	if err != nil {
		return err
	}
	if symvers.path == "" {
		return fmt.Errorf("module SDK kernel symbol versions have an empty public destination")
	}
	// The selected module_symvers product is the public kernel ABI contract.
	// A preparation or host artifact which happens to use the same basename
	// must never shadow it through generic object-tree projection.
	projections[symvers.path] = moduleSDKProjection{
		artifact: symvers,
		role:     "kernel symbol versions",
		path:     symvers.path,
	}

	destinations := make([]string, 0, len(projections))
	for destination := range projections {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	for _, destination := range destinations {
		projection := projections[destination]
		if _, err := appendActionPlanProjection(
			plan,
			projection.artifact.producer,
			projection.artifact.slot,
			"sdk", "sdk", projection.path,
		); err != nil {
			return fmt.Errorf("project module SDK %s: %w", projection.role, err)
		}
	}

	for _, product := range []ActionPlanProduct{
		{Name: "sdk", Tree: "sdk", Path: LinuxKernelTreeRootMarker},
		{Name: "modules", Tree: "modules", Path: LinuxKernelTreeRootMarker},
	} {
		if err := appendActionPlanProduct(plan, product); err != nil {
			return err
		}
	}
	return nil
}

// moduleSDKOrderedProducer selects the later producer for one logical path
// using execution edges first and the source Kbuild selection graph second.
// The latter is required for immutable snapshots of ordered overwrites: Bazel
// actions deliberately write distinct physical paths, so two selected Kbuild
// invocations can be source-ordered without a data dependency between their
// final producer actions.
func moduleSDKOrderedProducer(plan *ActionPlan, pathname, left, right string) (string, bool, error) {
	rightAfterLeft, err := moduleSDKProducerDependsOn(plan, right, left)
	if err != nil {
		return "", false, err
	}
	leftAfterRight, err := moduleSDKProducerDependsOn(plan, left, right)
	if err != nil {
		return "", false, err
	}
	if rightAfterLeft && leftAfterRight {
		return "", false, fmt.Errorf("module SDK writers %s and %s form a dependency cycle", left, right)
	}
	if rightAfterLeft {
		return right, true, nil
	}
	if leftAfterRight {
		return left, true, nil
	}
	if plan.selectionGraph == nil {
		return "", false, nil
	}
	winner, ordered, err := plan.selectionGraph.compactKbuildSourceOrderedPathProducer(pathname, left, right)
	if err != nil {
		return "", false, err
	}
	if !ordered {
		return "", false, nil
	}
	if winner != left && winner != right {
		return "", false, fmt.Errorf(
			"Kbuild overwrite provenance selected %q outside candidate writers %q and %q",
			winner, left, right,
		)
	}
	return winner, true, nil
}

// moduleSDKProducerDependsOn reports whether consumer is ordered after
// ancestor by ordinary action-plan producer edges. It deliberately does not
// use node slice order: that order is a planner implementation detail and is
// replaced by content-ID sorting before serialization.
func moduleSDKProducerDependsOn(plan *ActionPlan, consumer, ancestor string) (bool, error) {
	if plan == nil {
		return false, fmt.Errorf("module SDK writer ordering requires a non-nil action plan")
	}
	if consumer == "" || ancestor == "" {
		return false, fmt.Errorf("module SDK writer ordering has an empty producer ID")
	}
	plan.ensureNodeLookupIndexes()
	if _, ok := plan.nodesByID[consumer]; !ok {
		return false, fmt.Errorf("module SDK writer %q is absent from the action plan", consumer)
	}
	if _, ok := plan.nodesByID[ancestor]; !ok {
		return false, fmt.Errorf("module SDK ancestor writer %q is absent from the action plan", ancestor)
	}
	if consumer == ancestor {
		return true, nil
	}

	visited := map[string]bool{}
	pending := []string{consumer}
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[current] {
			continue
		}
		visited[current] = true
		node, ok := plan.nodesByID[current]
		if !ok {
			return false, fmt.Errorf("module SDK dependency writer %q is absent from the action plan", current)
		}
		for _, input := range node.Inputs {
			if input.ProducerID == ancestor {
				return true, nil
			}
			if _, ok := plan.nodesByID[input.ProducerID]; !ok {
				return false, fmt.Errorf(
					"module SDK writer %q references absent producer %q",
					current, input.ProducerID,
				)
			}
			if !visited[input.ProducerID] {
				pending = append(pending, input.ProducerID)
			}
		}
	}
	return false, nil
}

func selectedModuleSDKProduct(plan *ActionPlan, name string) (moduleSDKSelectedArtifact, error) {
	var selected *ActionPlanProduct
	for _, product := range plan.Products {
		if product.Name != name {
			continue
		}
		if selected != nil && *selected != product {
			return moduleSDKSelectedArtifact{}, fmt.Errorf(
				"module SDK product %q is ambiguous between %s/%s and %s/%s",
				name, selected.Tree, selected.Path, product.Tree, product.Path,
			)
		}
		copy := product
		selected = &copy
	}
	if selected == nil {
		return moduleSDKSelectedArtifact{}, fmt.Errorf("module SDK requires selected product %q", name)
	}
	if selected.Path == LinuxKernelTreeRootMarker {
		return moduleSDKSelectedArtifact{}, fmt.Errorf("module SDK product %q names a tree root, want a file", name)
	}
	producer, slot, ok := planProducerByOutput(plan, selected.Tree, selected.Path)
	if !ok {
		return moduleSDKSelectedArtifact{}, fmt.Errorf(
			"module SDK product %q has no producer for %s/%s", name, selected.Tree, selected.Path,
		)
	}
	return moduleSDKSelectedArtifact{producer: producer, slot: slot, tree: selected.Tree, path: selected.Path}, nil
}
