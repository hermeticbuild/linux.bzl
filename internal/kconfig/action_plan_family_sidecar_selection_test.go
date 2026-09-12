package kconfig

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func sidecarSelectionWithExecutableForTest(t *testing.T, other string) ActionPlanFamilyVariant {
	t.Helper()
	return selectionMutateVariant(t, selectionArtifactVariant(t, other), func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, helper *ActionPlanNode) {
		producer := *helper
		producer.ID, producer.Stage = "metadata-state-producer", "target"
		producer.Outputs = []ActionPlanOutput{
			{Tree: "objects", Path: "include/generated/sidecar-data.h"},
			{Tree: "metadata", Path: compactKbuildSideOutputStateDirectory + "/" + strings.Repeat("a", 64) + ".state", ObservedPath: "side-effect"},
		}
		consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "observed-state", ProducerID: producer.ID, Slot: 1})
		binding := "observed-state:00000002"
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.ObservedOutputBases["00000001"] = append(recipe.ObservedOutputBases["00000001"], binding)
		plan.Nodes = append(plan.Nodes, producer)
	})
}

func sidecarSelectionMutateForTest(t *testing.T, mutate func(*ActionPlan, *ActionPlanNode, *ActionRecipe, *ActionPlanNode)) ActionPlanFamilyVariant {
	t.Helper()
	variant := metadataStateVariantForTest(t, "0", "")
	var dependency ConfigDependencySet
	for _, node := range variant.Snapshot.Nodes {
		if node.Kind == "compile" {
			dependency = variant.Snapshot.ConfigDependencies[node.ID]
		}
	}
	variant = selectionMutateVariant(t, variant, mutate)
	for _, node := range variant.Snapshot.Nodes {
		if node.Kind == "compile" {
			variant.Snapshot.ConfigDependencies[node.ID] = dependency
		}
	}
	return variant
}

func TestInitialSidecarSelectionPreciseVariantsAndImmutability(t *testing.T) {
	variants := []ActionPlanFamilyVariant{metadataStateVariantForTest(t, "0", ""), metadataStateVariantForTest(t, "1", "")}
	before := make([][]byte, len(variants))
	for index, variant := range variants {
		before[index], _ = marshalCanonicalActionPlanSnapshot(variant.Snapshot)
	}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 2, 2)
	if len(initial.Cut.Outputs()) != 4 || len(initial.Cut.Origins()) != 2 {
		t.Fatal("sidecar selection lost complete output vectors or variant origins")
	}
	for _, node := range initial.Cut.contract.Nodes {
		if node.Node.Kind == "compile" {
			t.Fatal("sidecar observation pinned a useful compiler")
		}
	}
	for index, variant := range variants {
		after, _ := marshalCanonicalActionPlanSnapshot(variant.Snapshot)
		if !bytes.Equal(before[index], after) {
			t.Fatal("sidecar scheduling mutated original snapshot")
		}
	}
	slices.Reverse(variants)
	reversed, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil || reversed.Cut.ID() != initial.Cut.ID() {
		t.Fatalf("variant order changed sealed selection: %v", err)
	}
}

func TestInitialSidecarSelectionComplementsProspectiveExecutable(t *testing.T) {
	variants := []ActionPlanFamilyVariant{sidecarSelectionWithExecutableForTest(t, "0"), sidecarSelectionWithExecutableForTest(t, "1")}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 4, 4)
	if len(initial.Cut.Outputs()) != 8 {
		t.Fatal("complementary groups lost sidecars")
	}
	for _, record := range initial.Cut.contract.Nodes {
		if record.Node.Kind == "compile" {
			t.Fatal("complementary sidecar roots pinned their compiler")
		}
	}
}

func TestInitialSidecarSelectionConservativeControls(t *testing.T) {
	for _, shape := range []string{"public state", "opaque", "other semantic use", "sequence", "prior prehost tree"} {
		t.Run(shape, func(t *testing.T) {
			variants := []ActionPlanFamilyVariant{metadataStateVariantForTest(t, "0", shape)}
			initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
			if err != nil {
				t.Fatal(err)
			}
			selectionRequireValid(t, initial, 0, 0)
		})
	}
	for name, mutate := range map[string]func(*ActionPlan, *ActionPlanNode, *ActionRecipe, *ActionPlanNode){
		"incomplete compiler": func(_ *ActionPlan, _ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.CompilerInvocation.WorkingInputUsesComplete = false
			recipe.CompilerInvocation.WorkingInputUses = nil
		},
		"argument transform": func(_ *ActionPlan, _ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.ArgumentTransforms = []ActionRecipeArgumentTransform{{Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}
		},
		"ordinary config root": func(_ *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
			producer.Outputs[0].Path = "include/generated/autoconf.h"
		},
		"config artifact alias": func(_ *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
			producer.Outputs[0].ArtifactPath = "include/config/auto.conf"
		},
		"other public tree": func(_ *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
			producer.Outputs[1].Tree = "objects"
		},
		"working alias": func(_ *ActionPlan, _ *ActionPlanNode, recipe *ActionRecipe, producer *ActionPlanNode) {
			recipe.WorkingInputs["input:observed-state:00000000"] = producer.Outputs[1].Path
		},
		"persistent alias": func(plan *ActionPlan, consumer *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
			store, err := plan.planningActionPlanInputSetStore()
			if err != nil {
				t.Fatal(err)
			}
			consumer.InputSet, err = store.Insert(consumer.InputSet, ActionPlanInputSetEntry{
				Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: producer.Outputs[1].Path}, ProducerID: producer.ID, Slot: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			variants := []ActionPlanFamilyVariant{sidecarSelectionMutateForTest(t, mutate)}
			initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
			if err != nil {
				t.Fatal(err)
			}
			selectionRequireValid(t, initial, 0, 0)
		})
	}
}

func TestInitialSidecarSelectionBudgetPreservesExistingGroups(t *testing.T) {
	variants := []ActionPlanFamilyVariant{sidecarSelectionWithExecutableForTest(t, "0"), sidecarSelectionWithExecutableForTest(t, "1")}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range [][2]int{{1, MaxActionPlanFamilyExecutionCutBytes}, {MaxActionPlanFamilyExecutionCutRecords, 1}} {
		groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
		if overflow, err := addInitialFamilyExecutableGroups(family, variants, groups, 0, 0, MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
			t.Fatalf("executable groups: %t, %v", overflow, err)
		}
		before, err := selectActionPlanFamilyHeaderExecution(family, slices.Collect(maps.Values(groups)), defaultActionPlanFamilyExecutionCutLimits(), maxActionPlanFamilyHeaderExecutionAttempts)
		if err != nil {
			t.Fatal(err)
		}
		if overflow, err := addInitialFamilySidecarGroups(family, variants, groups, limit[0], limit[1]); err != nil || !overflow {
			t.Fatalf("sidecar overflow: %t, %v", overflow, err)
		}
		after, err := selectActionPlanFamilyHeaderExecution(family, slices.Collect(maps.Values(groups)), defaultActionPlanFamilyExecutionCutLimits(), maxActionPlanFamilyHeaderExecutionAttempts)
		if err != nil || after.ID() != before.ID() || len(groups) != 1 {
			t.Fatalf("optional discovery replaced existing selection: %v", err)
		}
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	if overflow, err := addInitialFamilyExecutableGroups(family, variants, groups, 0, 0, MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
		t.Fatalf("executable groups: %t, %v", overflow, err)
	}
	if overflow, err := addInitialFamilySidecarGroups(family, variants, groups, MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
		t.Fatalf("sidecar groups: %t, %v", overflow, err)
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.nodes = 2
	cut, err := selectActionPlanFamilyHeaderExecution(family, slices.Collect(maps.Values(groups)), limits, maxActionPlanFamilyHeaderExecutionAttempts)
	if err != nil || len(cut.Roots()) != 2 {
		t.Fatalf("over-budget complementary closure erased useful execution: %v", err)
	}
	for _, output := range cut.Outputs() {
		if output.Output.Tree != "prehost" {
			t.Fatal("over-budget sidecar group displaced the original executable group")
		}
	}
}

func TestInitialSidecarSelectionDeduplicatesConsumerRootBudget(t *testing.T) {
	variant := sidecarSelectionMutateForTest(t, func(_ *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		consumer.Inputs = append(consumer.Inputs, consumer.Inputs[0])
		binding := "observed-state:00000001"
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.ObservedOutputBases["00000001"] = append(recipe.ObservedOutputBases["00000001"], binding)
	})
	variants := []ActionPlanFamilyVariant{variant}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	if overflow, err := addInitialFamilySidecarGroups(family, variants, groups, 1, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow || len(groups) != 1 {
		t.Fatalf("one consumer/root charged more than once: overflow=%t groups=%d err=%v", overflow, len(groups), err)
	}
	cut, err := selectActionPlanFamilyHeaderExecution(family, slices.Collect(maps.Values(groups)), defaultActionPlanFamilyExecutionCutLimits(), maxActionPlanFamilyHeaderExecutionAttempts)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, &ActionPlanFamilyInitialExecution{Family: family, Cut: cut}, 1, 1)
}

func TestInitialSidecarSelectionRetainsPersistentAncestors(t *testing.T) {
	variant := sidecarSelectionMutateForTest(t, func(plan *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
		ancestor := *producer
		ancestor.ID, ancestor.Stage = "state-seed", "prehost"
		ancestor.Outputs = []ActionPlanOutput{{Tree: "prehost", Path: "seed"}, {Tree: "prehost", Path: "seed.state", ObservedPath: "side-effect"}}
		store, err := plan.planningActionPlanInputSetStore()
		if err != nil {
			t.Fatal(err)
		}
		producer.InputSet, err = store.Insert("", ActionPlanInputSetEntry{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "seed"}, ProducerID: ancestor.ID, Slot: 0})
		if err != nil {
			t.Fatal(err)
		}
		plan.Nodes = append(plan.Nodes, ancestor)
	})
	variants := []ActionPlanFamilyVariant{variant}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 1, 2)
	if len(initial.Cut.Outputs()) != 4 || len(initial.Cut.contract.InputSets) == 0 {
		t.Fatal("sidecar observation lost persistent ancestors or complete output slots")
	}
}

func TestInitialSidecarSelectionRejectsMalformedBase(t *testing.T) {
	variant := sidecarSelectionMutateForTest(t, func(_ *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		recipe.ObservedOutputs["00000001"] = "different-effect"
		consumer.Outputs[1].ObservedPath = "different-effect"
	})
	variants := []ActionPlanFamilyVariant{variant}
	if initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants)); err == nil || initial != nil {
		t.Fatal("accepted sidecar for another logical path")
	}
	variant = metadataStateVariantForTest(t, "0", "")
	for id, recipe := range variant.Snapshot.Recipes {
		if recipe.CompilerInvocation != nil {
			recipe.ObservedOutputBases["00000001"] = []string{"missing:00000000"}
			variant.Snapshot.Recipes[id] = recipe
		}
	}
	variants = []ActionPlanFamilyVariant{variant}
	if initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants)); err == nil || initial != nil {
		t.Fatal("accepted an undeclared sidecar input")
	}
}

func TestInitialSidecarSelectionInternalOrdinaryRootKeepsProjectionValidation(t *testing.T) {
	variant := metadataStateVariantForTest(t, "0", "")
	plan := snapshotActionPlan(variant.Snapshot)
	var compilerDependencies ConfigDependencySet
	plan.projectedGeneratorCandidates = map[string]projectedGeneratorCandidate{}
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			compilerDependencies = variant.Snapshot.ConfigDependencies[node.ID]
			continue
		}
		plan.projectedGeneratorCandidates[node.ID] = projectedGeneratorCandidate{
			TargetPath: node.Outputs[0].Path, TargetSlot: 0,
			ConfigProjectionPrefixes: []string{"CONFIG_USED"},
		}
	}
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		switch {
		case node.Kind == "compile":
			dependencies[node.ID] = compilerDependencies
		case len(plan.Recipes[node.Recipe].ConfigProjectionPrefixes) != 0:
			dependencies[node.ID] = ConfigDependencySet{Symbols: []string{"CONFIG_USED"}}
		default:
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full generator or validation"}
		}
	}
	var err error
	variant.Snapshot, err = canonicalActionPlanSnapshot(plan, dependencies, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	variants := []ActionPlanFamilyVariant{variant}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 1, 3)
	root := initial.Cut.Roots()[0]
	if len(initial.Cut.contract.Validations) != 1 {
		t.Fatal("selecting a private ordinary output lost its projection comparator")
	}
	var comparator ActionPlanNode
	for _, record := range initial.Cut.contract.Nodes {
		if record.Node.ID == initial.Cut.contract.Validations[0].NodeID {
			comparator = record.Node
		}
		if record.Node.Kind == "compile" {
			t.Fatal("private generator observation pinned its precise consumer")
		}
	}
	if len(comparator.Inputs) != 2 || comparator.Inputs[1].ProducerID != root.NodeID || comparator.Inputs[1].Slot != root.Slot {
		t.Fatal("sidecar root is not the comparator's exact full-generator slot")
	}
	for _, input := range comparator.Inputs {
		if !slices.Contains(initial.Cut.NodeIDs(), input.ProducerID) {
			t.Fatal("cut omitted one side of the projection comparison")
		}
	}
	for _, view := range initial.Family.Views {
		if view.NodeID == root.NodeID && view.Slot == root.Slot {
			t.Fatal("scheduling published a validation-private ordinary output")
		}
	}
	observed, err := observeExecutedArtifacts(initial.Cut,
		executedArtifactStoresForTest(t, initial.Cut, toolaction.ObservedOutputAbsent), 1024, 8192)
	if err != nil {
		t.Fatal(err)
	}
	plan = snapshotActionPlan(variant.Snapshot)
	replay, err := initial.Cut.VerifyVariantReplay(variant.Name, variant.Snapshot, plan, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	// Production observed analysis forces every authenticated cut member back
	// to its full-config classification, including the projected comparator arm.
	// Reproduce that classification from the verified cut, not output names.
	replayDependencies := maps.Clone(variant.Snapshot.ConfigDependencies)
	for id := range replay.opaqueNodes {
		replayDependencies[id] = ConfigDependencySet{Opaque: true, Reason: "executed generator pinned to full config"}
	}
	updated, updatedDependencies, err := substituteExecutedArtifacts(plan, replayDependencies, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	updatedSnapshot, err := canonicalActionPlanSnapshot(updated, updatedDependencies, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	final, err := buildValidatedActionPlanFamily([]actionPlanFamilyBuildVariant{{
		variant: ActionPlanFamilyVariant{Name: variant.Name, Snapshot: updatedSnapshot}, executionCut: initial.Cut,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Cut.Verify(final.family); err != nil {
		t.Fatalf("private root substitution changed executed validation: %v", err)
	}
	if len(final.family.Validations) != len(initial.Family.Validations) {
		t.Fatal("final family lost the executed projection obligation")
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	if overflow, err := addInitialFamilySidecarGroups(initial.Family, variants, groups,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
		t.Fatalf("collect private sidecar root: %t, %v", overflow, err)
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.nodes = 2 // Producer alone fits; its complete comparator obligation does not.
	limited, err := selectActionPlanFamilyHeaderExecution(initial.Family, slices.Collect(maps.Values(groups)), limits, maxActionPlanFamilyHeaderExecutionAttempts)
	if err != nil || len(limited.Roots()) != 0 {
		t.Fatalf("budget retained a partial projection obligation: %v", err)
	}
	malformed := variant
	malformed.Snapshot.ValidationRoots = nil
	if _, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{malformed}, selectionEnabledDemands([]ActionPlanFamilyVariant{malformed})); err == nil {
		t.Fatal("private root bypassed snapshot validation")
	}
}
