package kconfig

// The module SDK is the configured Kbuild object tree, projected without a
// second external-module recipe language. External modules run the same
// source-derived Make/Kbuild planner against this tree as in-tree targets.

import (
	"fmt"
	"sort"
	"strings"
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
			if output.Tree != "prep" || output.ObservedPath != "" || (!actionPlanOutputIsCanonical(output) && !output.persistent) {
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
	if _, ok := projections["include/config/kernel.release"]; !ok {
		const release = "include/config/kernel.release"
		selected := []string{}
		releaseProfiles := map[string]bool{}
		for _, selection := range m.Config.KbuildSelections {
			if selection.Target == release {
				selected = append(selected, fmt.Sprintf("%q lifecycle=%q stage=%q", selection.Profile, selection.Lifecycle, selection.Stage))
				releaseProfiles[selection.Profile] = true
			}
			if selection.Target == "include/generated/utsrelease.h" || selection.Target == "vmlinux" {
				selected = append(selected, fmt.Sprintf("%q target=%q lifecycle=%q stage=%q", selection.Profile, selection.Target, selection.Lifecycle, selection.Stage))
			}
		}
		ancestry := []string{}
		for index, profile := range m.Config.KbuildProfiles {
			if index > 1 && !releaseProfiles[profile.Name] {
				continue
			}
			ancestry = append(ancestry, fmt.Sprintf("profile %q source=%q goals=%q", profile.Name, profile.Path, profile.EntryTargets))
			for _, dependency := range profile.TargetInvocationDependencies {
				if dependency.Target == "__sub-make" || dependency.Target == "all" || dependency.Target == "modules_prepare" {
					ancestry = append(ancestry, fmt.Sprintf("%q child=%q goals=%q", dependency.Target, dependency.Profile, dependency.Goals))
				}
			}
			if releaseProfiles[profile.Name] {
				for _, rule := range profile.Rules {
					for _, target := range rule.Targets {
						if target == "modules_prepare" || target == "prepare" || target == "archprepare" || target == release {
							ancestry = append(ancestry, fmt.Sprintf("%q prerequisites=%q at %s", target, rule.Prerequisites, rule.Position))
						}
					}
				}
			}
		}
		outputs := []string{}
		for _, node := range plan.Nodes {
			for _, output := range node.Outputs {
				if output.Path == release {
					outputs = append(outputs, fmt.Sprintf(
						"%q stage=%q tree=%q artifact=%q canonical=%t persistent=%t",
						node.ID, node.Stage, output.Tree, actionPlanOutputArtifactPath(output),
						actionPlanOutputIsCanonical(output), output.persistent,
					))
				}
			}
		}
		sort.Strings(selected)
		sort.Strings(outputs)
		return fmt.Errorf(
			"module SDK has no source-selected Kbuild writer for %s (selected: %s; planned outputs: %s; source ancestry: %s)",
			release, strings.Join(selected, "; "), strings.Join(outputs, "; "), strings.Join(ancestry, "; "),
		)
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

	vmlinux, err := selectedModuleSDKProduct(plan, "vmlinux")
	if err != nil {
		return err
	}
	// External Kbuild clears KBUILD_BUILTIN, so Makefile.modfinal does not
	// retain vmlinux as a native prerequisite even when module BTF is enabled.
	// Linux instead expects the read-only basis object tree to contain it. Keep
	// the public terminal image in the SDK at that exact conventional path;
	// target-lifecycle intermediates such as vmlinux.o remain private.
	projections["vmlinux"] = moduleSDKProjection{
		artifact: vmlinux,
		role:     "kernel BTF base",
		path:     "vmlinux",
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
	// A host preparation action writes its native host/prehost path, and an
	// authenticated one-input copy publishes the same bytes to prep. The
	// source selection graph indexes the native writer, while SDK candidates
	// are the prep copies. Follow only this exact mirror edge when asking the
	// source graph which invocation published the final version.
	sourceLeft := moduleSDKNativePrepMirrorProducer(plan, pathname, left)
	sourceRight := moduleSDKNativePrepMirrorProducer(plan, pathname, right)
	winner, ordered, err := plan.selectionGraph.compactKbuildSourceOrderedPathProducer(pathname, sourceLeft, sourceRight)
	if err != nil {
		return "", false, err
	}
	if !ordered {
		return "", false, nil
	}
	if winner == sourceLeft && winner != sourceRight {
		return left, true, nil
	}
	if winner == sourceRight && winner != sourceLeft {
		return right, true, nil
	}
	if winner != sourceLeft && winner != sourceRight {
		return "", false, fmt.Errorf(
			"Kbuild overwrite provenance selected %q outside candidate writers %q and %q",
			winner, sourceLeft, sourceRight,
		)
	}
	return "", false, fmt.Errorf("Kbuild overwrite provenance cannot distinguish SDK mirrors %q and %q of native producer %q", left, right, winner)
}

// moduleSDKNativePrepMirrorProducer accepts only the one-to-one projection
// emitted for a selected host preparation output. Arbitrary actionfile copies
// and projections retain their own identities and must prove order by action
// edges or their exact source-selected producer IDs.
func moduleSDKNativePrepMirrorProducer(plan *ActionPlan, pathname, id string) string {
	copy, ok := compactKbuildPlanNode(plan, id)
	if !ok || copy.Stage != "prep" || copy.Kind != "copy" || copy.Tool != "actionfile" || copy.Product != "sdk" ||
		len(copy.Inputs) != 1 || copy.Inputs[0].Role != "input" || len(copy.Outputs) != 1 ||
		copy.Outputs[0].Tree != "prep" || copy.Outputs[0].Path != pathname || copy.Outputs[0].ObservedPath != "" {
		return id
	}
	recipe, ok := plan.Recipes[copy.Recipe]
	if !ok || recipe.Schema != LinuxKernelPlanSchema || recipe.Kind != "copy" || recipe.Tool != "actionfile" ||
		len(recipe.Arguments) != 5 || recipe.Arguments[0] != "-input" ||
		recipe.Arguments[1] != "${input:input:00000000}" || recipe.Arguments[2] != "-out" ||
		recipe.Arguments[3] != "${output:00000000}" || recipe.Arguments[4] != "-preserve_mode" || len(recipe.Inputs) != 1 ||
		recipe.Inputs[0] != "input:00000000" || len(recipe.Outputs) != 1 || recipe.Outputs[0] != "00000000" {
		return id
	}
	input := copy.Inputs[0]
	native, ok := compactKbuildPlanNode(plan, input.ProducerID)
	if !ok || native.Stage != "host" && native.Stage != "prehost" || input.Slot < 0 || input.Slot >= len(native.Outputs) {
		return id
	}
	output := native.Outputs[input.Slot]
	published := copy.Outputs[0]
	if output.Tree != native.Stage || output.Path != pathname || output.ObservedPath != "" ||
		output.persistent != published.persistent {
		return id
	}
	if actionPlanOutputArtifactPath(output) != actionPlanOutputArtifactPath(published) {
		// Source-ordered host writers in different physical stage trees can
		// each have a canonical native output while only the last publishes
		// the canonical prep mirror. Authenticate a remapped private mirror
		// against the exact selected native producer and its prep publisher;
		// an arbitrary actionfile copy cannot acquire this authority.
		graph := plan.selectionGraph
		matched := false
		if graph != nil {
			for _, owner := range graph.outputOwnersByPath[pathname] {
				selection := graph.selections[owner]
				if selection.Lifecycle != "prep" || selection.Scope != "host" ||
					selection.Stage != native.Stage || graph.materializedProducers[owner] != native.ID {
					continue
				}
				expected := graph.compactKbuildSelectionOutput(owner, ActionPlanOutput{Tree: "prep", Path: pathname})
				if actionPlanOutputArtifactPath(expected) == actionPlanOutputArtifactPath(published) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return id
		}
	}
	return native.ID
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
