package kconfig

import (
	"fmt"
	"slices"
	"strings"
)

func compactKbuildSelectionLifecycleScope(selection CompactKbuildSelection) (string, string, error) {
	lifecycle := selection.Lifecycle
	if lifecycle != "prep" && lifecycle != "target" {
		return "", "", fmt.Errorf("Kbuild selection %s target %q has unsupported lifecycle %q", selection.Profile, selection.Target, selection.Lifecycle)
	}
	scope := selection.Scope
	if scope != "target" && scope != "host" {
		return "", "", fmt.Errorf("Kbuild selection %s target %q has unsupported scope %q", selection.Profile, selection.Target, selection.Scope)
	}
	if selection.Stage == "prehost" {
		if scope != "host" {
			return "", "", fmt.Errorf(
				"Kbuild selection %s target %q prehost stage requires host scope, got %q",
				selection.Profile, selection.Target, scope,
			)
		}
		return lifecycle, scope, nil
	}
	if selection.Stage == "bootstrap" {
		if scope != "target" {
			return "", "", fmt.Errorf(
				"Kbuild selection %s target %q bootstrap stage requires target scope, got %q",
				selection.Profile, selection.Target, scope,
			)
		}
		return lifecycle, scope, nil
	}
	wantStage := lifecycle
	if scope == "host" {
		wantStage = "host"
	}
	if selection.Stage != wantStage {
		return "", "", fmt.Errorf(
			"Kbuild selection %s target %q lifecycle/scope %s/%s require physical stage %q, got %q",
			selection.Profile, selection.Target, lifecycle, scope, wantStage, selection.Stage,
		)
	}
	return lifecycle, scope, nil
}

func compactKbuildSelectionCanonicalPlanContext(
	config CompactConfig,
	selection CompactKbuildSelection,
) (compactKbuildRulePlanContext, error) {
	lifecycle, scope, err := compactKbuildSelectionLifecycleScope(selection)
	if err != nil {
		return compactKbuildRulePlanContext{}, err
	}
	if scope != "target" {
		return compactKbuildRulePlanContext{}, fmt.Errorf("Kbuild selection %s target %q canonical target projection requires target scope, got %q", selection.Profile, selection.Target, scope)
	}
	selection.Stage = lifecycle
	selection.Scope = "target"
	return compactKbuildSelectionPlanContext(config, selection), nil
}

// appendCompactKbuildBootstrapProjections copies every native bootstrap
// output into the tree where its source-selected lifecycle would ordinarily
// materialize it. Bootstrap remains the authoritative producer for host-stage
// consumers, while target/preparation consumers and exported products see the
// canonical object-tree ownership they would have without the scope boundary.
// Opaque observations are deliberately excluded: consumer-local resolver
// actions consume their bootstrap captures directly without attributing the
// observed path to this candidate.
func appendCompactKbuildBootstrapProjections(
	plan *ActionPlan,
	config CompactConfig,
	selection CompactKbuildSelection,
	producerID string,
) error {
	if selection.Stage != "bootstrap" {
		return nil
	}
	context, err := compactKbuildSelectionCanonicalPlanContext(config, selection)
	if err != nil {
		return err
	}

	producer, ok := compactKbuildPlanNode(plan, producerID)
	if !ok {
		return fmt.Errorf("bootstrap target %q has no action-plan producer %q", selection.Target, producerID)
	}
	projected := 0
	for slot, output := range producer.Outputs {
		if output.ObservedPath != "" {
			continue
		}
		if output.Tree != "bootstrap" {
			return fmt.Errorf("bootstrap target %q producer %s has non-bootstrap native output %s/%s", selection.Target, producerID, output.Tree, output.Path)
		}
		destinationContext := context
		if !actionPlanStageOwnsOutputTree(destinationContext.Stage, destinationContext.OutputTree) {
			return fmt.Errorf("bootstrap output %q has incompatible canonical stage/tree %s/%s", output.Path, destinationContext.Stage, destinationContext.OutputTree)
		}
		if existing, _, exists := planProducerByOutput(plan, destinationContext.OutputTree, output.Path); exists {
			return fmt.Errorf(
				"bootstrap output %q already has canonical %s-tree producer %s",
				output.Path, destinationContext.OutputTree, existing,
			)
		}
		node := ActionPlanNode{
			Stage: destinationContext.Stage, Kind: "copy", Tool: "actionfile", Product: destinationContext.Product,
			Inputs:  []ActionPlanNodeEdge{{Role: "input", ProducerID: producerID, Slot: slot}},
			Outputs: []ActionPlanOutput{{Tree: destinationContext.OutputTree, Path: output.Path}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
			Arguments: []string{"-input", "${input:input:00000000}", "-out", "${output:00000000}"},
			Inputs:    []string{"input:00000000"},
			Outputs:   []string{"00000000"},
		}
		if _, err := appendActionPlanNode(plan, node, recipe); err != nil {
			return fmt.Errorf("project bootstrap output %q into %s/%s: %w", output.Path, destinationContext.Stage, destinationContext.OutputTree, err)
		}
		projected++
	}
	if projected == 0 {
		return fmt.Errorf("bootstrap target %q producer %s has no native bootstrap output", selection.Target, producerID)
	}
	return nil
}

// appendCompactKbuildHostPrepMirrors projects every native output of a
// host-scoped preparation action into the preparation tree. The source Make
// graph, rather than a filename or tool catalogue, decides which selections
// receive this projection. Native prehost/host outputs remain authoritative
// for subsequent actions; the copies exist for target consumers and the
// exported module SDK.
func appendCompactKbuildHostPrepMirrors(
	plan *ActionPlan,
	selection CompactKbuildSelection,
	producerID string,
) error {
	if selection.Lifecycle != "prep" || selection.Scope != "host" {
		return nil
	}
	producer, ok := compactKbuildPlanNode(plan, producerID)
	if !ok {
		return fmt.Errorf("host-scoped preparation target %q has no action-plan producer %q", selection.Target, producerID)
	}
	mirrored := 0
	nativeTree := selection.Stage
	if nativeTree != "prehost" && nativeTree != "host" {
		return fmt.Errorf("host-scoped preparation target %q has invalid physical stage %q", selection.Target, selection.Stage)
	}
	for slot, output := range producer.Outputs {
		if output.ObservedPath != "" {
			continue
		}
		if output.Tree != nativeTree {
			continue
		}
		mirrorOutput := ActionPlanOutput{
			Tree: "prep", Path: output.Path, ArtifactPath: output.ArtifactPath, persistent: output.persistent,
		}
		if plan.selectionGraph != nil {
			mirrorOutput = plan.selectionGraph.compactKbuildSelectionOutput(
				compactKbuildSelectionKey{
					profile: selection.Profile, target: canonicalKbuildRulePath(selection.Target), stage: selection.Stage,
				}, mirrorOutput,
			)
		}
		if existing, _, exists := planProducerByOutput(plan, "prep", output.Path); exists && actionPlanOutputIsCanonical(mirrorOutput) {
			return fmt.Errorf(
				"canonical host-scoped preparation output %q already has prep-tree producer %s; source-selected publisher provenance is required",
				output.Path, existing,
			)
		}
		node := ActionPlanNode{
			Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
			Inputs:  []ActionPlanNodeEdge{{Role: "input", ProducerID: producerID, Slot: slot}},
			Outputs: []ActionPlanOutput{mirrorOutput},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
			Arguments: []string{"-input", "${input:input:00000000}", "-out", "${output:00000000}", "-preserve_mode"},
			Inputs:    []string{"input:00000000"},
			Outputs:   []string{"00000000"},
		}
		if _, err := appendActionPlanNode(plan, node, recipe); err != nil {
			return fmt.Errorf("mirror host-scoped preparation output %q: %w", output.Path, err)
		}
		mirrored++
	}
	if mirrored == 0 {
		if compactKbuildHostPrepCheckCompletion(plan, selection.Target, nativeTree, producer) {
			// The source check produced an execution state, not a host file.
			// Its selected native predecessor orders downstream preparation;
			// there is no filesystem product for the SDK to mirror.
			return nil
		}
		return fmt.Errorf("host-scoped preparation target %q producer %s has no native %s output", selection.Target, producerID, nativeTree)
	}
	return nil
}

// Only an authenticated, execution-only source or Make status may satisfy a
// host prep selection without a native file. Ordinary observations and
// incomplete host generators retain the missing-output error above.
func compactKbuildHostPrepCheckCompletion(
	plan *ActionPlan, target, nativeTree string, producer ActionPlanNode,
) bool {
	return producer.Stage == nativeTree && len(producer.Outputs) != 0 &&
		producer.Outputs[0].Tree == nativeTree &&
		compactKbuildAuthenticatedExecutionCheckCompletion(plan, producer, target)
}

// This private observed output is an execution root, never a Make file or a
// public tree product. Retain exact source-script and runner assertions across
// provisional checkpoints and family reduction so it cannot be replaced by an
// unrelated observed-state action during replay.
func compactKbuildSourceCheckCompletion(plan *ActionPlan, producer ActionPlanNode, target string) bool {
	if plan == nil || producer.Tool != compactKbuildScriptRunnerRole || len(producer.Outputs) == 0 {
		return false
	}
	recipe, exists := plan.Recipes[producer.Recipe]
	if !exists || recipe.MakePhonyCompletion != nil ||
		recipe.Tool != compactKbuildScriptRunnerRole || recipe.Kind != producer.Kind ||
		recipe.RequireAbsentObservedOutput != planOrdinal(0) ||
		recipe.RequireUnchangedWorkingTree == (len(recipe.PrivateWorkingEffects) != 0) ||
		recipe.ObservedOutputs[planOrdinal(0)] != target || len(recipe.WorkingOutputs) != 0 ||
		recipe.Stdout != "" || recipe.WorkingDirectory == "" ||
		len(recipe.Outputs) != len(producer.Outputs) {
		return false
	}
	boundScript := false
	for ordinal, source := range producer.Sources {
		if source.Role == "script" && source.SourceID != "" &&
			slices.Contains(recipe.Sources, fmt.Sprintf("script:%08d", ordinal)) {
			boundScript = true
			break
		}
	}
	if !boundScript {
		return false
	}
	primary := producer.Outputs[0]
	if !actionPlanStageOwnsOutputTree(producer.Stage, primary.Tree) || primary.ObservedPath != target ||
		!actionPlanOutputIsCanonical(primary) || !strings.HasPrefix(primary.Path, ".linux-bzl-intermediate/") {
		return false
	}
	for slot, output := range producer.Outputs {
		if output.Tree != primary.Tree || output.ObservedPath == "" ||
			recipe.ObservedOutputs[planOrdinal(slot)] != output.ObservedPath ||
			recipe.Outputs[slot] != planOrdinal(slot) {
			return false
		}
	}
	return true
}
