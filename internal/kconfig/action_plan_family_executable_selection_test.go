package kconfig

import (
	"bytes"
	"encoding/base64"
	"maps"
	"slices"
	"strings"
	"testing"
)

func selectionArtifactVariant(t *testing.T, other string) ActionPlanFamilyVariant {
	t.Helper()
	files := familyTestConfig("1", other)
	snapshot := familyTestSnapshot(t, 1, "", files, ConfigDependencySet{Symbols: []string{"CONFIG_USED"}, SourcePaths: []string{"drivers/example.c"}})
	plan := snapshotActionPlan(snapshot)
	plan.Toolsets["host"] = "sha256-" + strings.Repeat("2", 64)
	consumer := &plan.Nodes[0]
	consumer.ID = "consumer"
	recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
	compilerArgs := slices.Clone(recipe.Arguments)
	recipe.Tool = "scriptrun"
	recipe.Arguments = []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte("tools/helper\ncc -c drivers/example.c -o drivers/example.o\n"))}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{Tool: "cc", Arguments: compilerArgs, WorkingInputUsesComplete: true, WorkingInputUses: []string{"input:helper:00000000", "source:config:00000001", "source:source:00000000"}, AuxiliaryWorkingInputUses: []string{"input:helper:00000000"}}
	recipe.Inputs = []string{"helper:00000000", "observed-state:00000001"}
	recipe.ExecutableInputs = []string{"helper:00000000"}
	recipe.WorkingInputs["source:source:00000000"] = "drivers/example.c"
	recipe.WorkingInputs["input:helper:00000000"] = "tools/helper"
	recipe.WorkingOutputs = map[string]string{"00000000": "drivers/example.o"}
	recipe.Outputs = append(recipe.Outputs, "00000001")
	recipe.ObservedOutputs = map[string]string{"00000001": "side-effect"}
	recipe.ObservedOutputBases = map[string][]string{"00000001": {"observed-state:00000001"}}
	id, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes = map[string]ActionRecipe{id: recipe}
	consumer.Tool, consumer.Recipe = "scriptrun", id
	consumer.Inputs = []ActionPlanNodeEdge{{Role: "helper", ProducerID: "helper", Slot: 0}, {Role: "observed-state", ProducerID: "helper", Slot: 1}}
	consumer.Outputs = append(consumer.Outputs, ActionPlanOutput{Tree: "metadata", Path: "consumer.state", ObservedPath: "side-effect"})
	helperRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile", Arguments: []string{"-content_base64", base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexit 0\n")), "-out", "${output:00000000}"}, WorkingDirectory: "helper", WorkingInputs: map[string]string{"source:config:00000000": "include/generated/autoconf.h"}, Sources: []string{"config:00000000"}, Outputs: []string{"00000000", "00000001"}, ObservedOutputs: map[string]string{"00000001": "side-effect"}}
	helperID, err := helperRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[helperID] = helperRecipe
	plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: "helper", Stage: "prehost", Kind: "generate", Tool: "actionfile", Recipe: helperID, Product: "sdk", Sources: []ActionPlanSourceEdge{{Role: "config", SourceID: "src-00000002"}}, Outputs: []ActionPlanOutput{{Tree: "prehost", Path: "tools/helper"}, {Tree: "prehost", Path: "helper.state", ObservedPath: "side-effect"}}})
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sets := map[string]ConfigDependencySet{plan.Nodes[0].ID: {Symbols: []string{"CONFIG_USED"}, SourcePaths: []string{"drivers/example.c"}, ObjectPaths: []string{"include/generated/autoconf.h"}}, plan.Nodes[1].ID: {Opaque: true, Reason: "config-dependent helper"}}
	snapshot, err = canonicalActionPlanSnapshot(plan, sets, files)
	if err != nil {
		t.Fatal(err)
	}
	name := "base"
	if other != "0" {
		name = "other"
	}
	return ActionPlanFamilyVariant{Name: name, Snapshot: snapshot}
}

func selectionEnabledDemands(variants []ActionPlanFamilyVariant) map[string]ConfigDependencyGeneratedHeaderDemandCollection {
	result := map[string]ConfigDependencyGeneratedHeaderDemandCollection{}
	for _, variant := range variants {
		result[variant.Name] = ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true}
	}
	return result
}

func selectionMutateVariant(t *testing.T, variant ActionPlanFamilyVariant, mutate func(*ActionPlan, *ActionPlanNode, *ActionRecipe, *ActionPlanNode)) ActionPlanFamilyVariant {
	t.Helper()
	plan := snapshotActionPlan(variant.Snapshot)
	var consumer, producer *ActionPlanNode
	consumerIndex, producerIndex := -1, -1
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if plan.Recipes[node.Recipe].CompilerInvocation != nil {
			consumer = node
			consumerIndex = index
		} else if node.Outputs[0].Path == "tools/helper" {
			producer = node
			producerIndex = index
		}
	}
	if consumer == nil || producer == nil {
		t.Fatal("missing fixture nodes")
	}
	consumerDependencies, classified := variant.Snapshot.ConfigDependencies[consumer.ID]
	recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
	mutate(plan, consumer, &recipe, producer)
	id, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[id] = recipe
	consumer.Recipe = id
	plan.Nodes[consumerIndex], plan.Nodes[producerIndex] = *consumer, *producer
	variant.Snapshot = initialExecutionSnapshotFromPlanForTest(t, plan, variant.Snapshot.ConfigFiles)
	// This helper changes graph bindings, not the fixture's classification.
	// Newly introduced compilers remain conservatively opaque unless the test
	// explicitly supplies a classification or a primary header demand.
	if classified {
		variant.Snapshot.ConfigDependencies[plan.Nodes[consumerIndex].ID] = consumerDependencies
	}
	return variant
}

func selectionRequireValid(t *testing.T, initial *ActionPlanFamilyInitialExecution, wantRoots, wantNodes int) {
	t.Helper()
	if len(initial.Cut.Roots()) != wantRoots || len(initial.Cut.NodeIDs()) != wantNodes {
		t.Fatalf("selection roots/nodes=%d/%d, want %d/%d", len(initial.Cut.Roots()), len(initial.Cut.NodeIDs()), wantRoots, wantNodes)
	}
	if _, err := initial.Cut.Verify(initial.Family); err != nil {
		t.Fatal(err)
	}
	for _, root := range initial.Cut.Roots() {
		for _, output := range initial.Cut.Outputs() {
			if output.NodeID == root.NodeID && output.Slot == root.Slot && output.Output.ObservedPath != "" {
				t.Fatal("sidecar became an ordinary execution root")
			}
		}
	}
}

func TestInitialExecutableSelectionVariantsAndDetachedClosure(t *testing.T) {
	variants := []ActionPlanFamilyVariant{selectionArtifactVariant(t, "0"), selectionArtifactVariant(t, "1")}
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
		t.Fatal("closed roots lost complete sidecar slots or variant origins")
	}
	for _, record := range initial.Cut.contract.Nodes {
		if initial.Family.Recipes[record.Node.Recipe].CompilerInvocation != nil {
			t.Fatal("selection pinned the prospective compiler consumer")
		}
	}
	for index, variant := range variants {
		after, _ := marshalCanonicalActionPlanSnapshot(variant.Snapshot)
		if !bytes.Equal(before[index], after) {
			t.Fatal("selection mutated original snapshot")
		}
	}
	reversed := slices.Clone(variants)
	slices.Reverse(reversed)
	reordered, err := NewActionPlanFamilyInitialExecution(reversed, selectionEnabledDemands(reversed))
	if err != nil || reordered.Cut.ID() != initial.Cut.ID() {
		t.Fatalf("variant iteration changed exact cut: %v", err)
	}

}

func TestInitialExecutableSelectionConsumerAdmission(t *testing.T) {
	for _, test := range []struct {
		name       string
		opaque     bool
		demand     bool
		wantGroups int
	}{
		{name: "precise without demand", wantGroups: 1},
		{name: "opaque with recorded demand", opaque: true, demand: true, wantGroups: 2},
		{name: "opaque without demand", opaque: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			variant := selectionArtifactVariant(t, "0")
			var consumer ActionPlanNode
			for _, node := range variant.Snapshot.Nodes {
				if node.Kind == "compile" {
					consumer = node
				}
			}
			if consumer.ID == "" {
				t.Fatal("missing compiler consumer")
			}
			if test.opaque {
				variant.Snapshot.ConfigDependencies[consumer.ID] = ConfigDependencySet{Opaque: true, Reason: "unrelated conservative limitation"}
			}
			variants := []ActionPlanFamilyVariant{variant}
			family, err := BuildConservativeActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
			consumerKey := variant.Name + "\x00" + consumer.ID
			consumerID := family.originalNodeIDs[variant.Name][consumer.ID]
			if test.demand {
				// Primary header collection has already authenticated and mapped
				// this exact consumer before executable collection is called.
				groups["primary header"] = &actionPlanFamilyHeaderDemandGroup{
					key: "primary header", roots: map[ActionPlanFamilyExecutionCutRoot]bool{},
					consumers: map[string]string{consumerKey: consumerID},
				}
			}
			overflow, err := addInitialFamilyExecutableGroups(family, variants, groups, 0, 0,
				MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes)
			if err != nil || overflow || len(groups) != test.wantGroups {
				t.Fatalf("groups=%d, want %d; overflow=%t error=%v", len(groups), test.wantGroups, overflow, err)
			}
			if test.wantGroups != 0 {
				group := groups["prehost\x00tools/helper"]
				if group == nil || group.consumers[consumerKey] != consumerID || len(group.roots) != 1 {
					t.Fatal("eligible consumer lost its exact executable group")
				}
			}
			if family.originalNodeIDs[variant.Name][consumer.ID] == "" {
				t.Fatal("declining speculative execution removed ordinary compiler work")
			}
		})
	}
}

func TestInitialExecutableSelectionOpaquePrerequisiteDoesNotBlockSidecar(t *testing.T) {
	variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		// The opaque prerequisite compiler uses the same helper as the useful
		// consumer, but has no recorded header demand of its own. It must not
		// become a protected speculative consumer merely because it uses it.
		prerequisiteRecipe := cloneActionRecipe(*recipe)
		prerequisiteRecipe.WorkingOutputs = map[string]string{"00000000": "drivers/prerequisite.s"}
		prerequisiteRecipeID, err := prerequisiteRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[prerequisiteRecipeID] = prerequisiteRecipe
		prerequisite := *consumer
		prerequisite.ID, prerequisite.Recipe = "prerequisite-compiler", prerequisiteRecipeID
		prerequisite.Outputs = []ActionPlanOutput{
			{Tree: "objects", Path: "drivers/prerequisite.s"},
			{Tree: "metadata", Path: "prerequisite.state", ObservedPath: "side-effect"},
		}
		headerRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-input", "${input:seed:00000000}", "-out", "${output:00000000}"},
			Inputs:    []string{"seed:00000000"}, Outputs: []string{"00000000", "00000001"},
			WorkingDirectory: "derive-header", ObservedOutputs: map[string]string{"00000001": "side-effect"}}
		headerRecipeID, err := headerRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[headerRecipeID] = headerRecipe
		header := ActionPlanNode{ID: "derived-header", Stage: "target", Kind: "generate", Tool: "actionfile", Recipe: headerRecipeID, Product: consumer.Product,
			Inputs: []ActionPlanNodeEdge{{Role: "seed", ProducerID: prerequisite.ID, Slot: 0}},
			Outputs: []ActionPlanOutput{
				{Tree: "objects", Path: "include/generated/complement.h"},
				{Tree: "metadata", Path: compactKbuildSideOutputStateDirectory + "/" + strings.Repeat("c", 64) + ".state", ObservedPath: "side-effect"},
			}}
		consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "observed-state", ProducerID: header.ID, Slot: 1})
		binding := "observed-state:00000002"
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.ObservedOutputBases["00000001"] = append(recipe.ObservedOutputBases["00000001"], binding)
		plan.Nodes = append(plan.Nodes, prerequisite, header)
	})
	variants := []ActionPlanFamilyVariant{variant}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 2, 3)
	if len(initial.Family.Nodes) != 4 || len(initial.Cut.Outputs()) != 6 {
		t.Fatal("selection lost the complete family or a prerequisite output vector")
	}
	for _, node := range variant.Snapshot.Nodes {
		id := initial.Family.originalNodeIDs[variant.Name][node.ID]
		pinned := slices.Contains(initial.Cut.NodeIDs(), id)
		switch node.Outputs[0].Path {
		case "drivers/example.o":
			if pinned {
				t.Fatal("sidecar closure pinned the useful compiler")
			}
		case "drivers/prerequisite.s":
			if !pinned || !variant.Snapshot.ConfigDependencies[node.ID].Opaque {
				t.Fatal("opaque prerequisite was not executed with its full original contract")
			}
		}
	}
}

func TestInitialExecutableSelectionConservativeControls(t *testing.T) {
	for name, mutate := range map[string]func(*ActionPlan, *ActionPlanNode, *ActionRecipe, *ActionPlanNode){
		"incomplete": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) {
			r.CompilerInvocation.WorkingInputUsesComplete = false
			r.CompilerInvocation.WorkingInputUses, r.CompilerInvocation.AuxiliaryWorkingInputUses = nil, nil
		},
		"untyped": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) { r.CompilerInvocation = nil },
		"transform": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) {
			r.ArgumentTransforms = []ActionRecipeArgumentTransform{{Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}
		},
		"no auxiliary use": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) {
			r.CompilerInvocation.AuxiliaryWorkingInputUses = nil
		},
		"relocated destination": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) {
			r.WorkingInputs["input:helper:00000000"] = "tools/alias"
		},
		"overwritten destination": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, _ *ActionPlanNode) {
			r.WorkingOutputs["00000000"] = "tools/helper"
		},
		"public view": func(_ *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, p *ActionPlanNode) {
			p.Stage = "target"
			for index := range p.Outputs {
				p.Outputs[index].Tree = "sdk"
			}
		},
		"config output": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, p *ActionPlanNode) {
			p.Outputs[0].Path = "include/generated/autoconf.h"
			r.WorkingInputs["input:helper:00000000"] = p.Outputs[0].Path
			delete(r.WorkingInputs, "source:config:00000001")
			r.CompilerInvocation.WorkingInputUses = []string{"input:helper:00000000", "source:source:00000000"}
		},
		"config artifact alias": func(_ *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, p *ActionPlanNode) {
			p.Outputs[0].ArtifactPath = "include/config/auto.conf"
		},
		"observed slot": func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe, p *ActionPlanNode) {
			r.ExecutableInputs = []string{"observed-state:00000001"}
			r.WorkingInputs["input:observed-state:00000001"] = p.Outputs[1].Path
			r.CompilerInvocation.WorkingInputUses = []string{"input:helper:00000000", "input:observed-state:00000001", "source:config:00000001", "source:source:00000000"}
			r.CompilerInvocation.AuxiliaryWorkingInputUses = []string{"input:observed-state:00000001"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), mutate)
			variants := []ActionPlanFamilyVariant{variant}
			initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
			if err != nil {
				t.Fatal(err)
			}
			selectionRequireValid(t, initial, 0, 0)
			if name == "public view" {
				found := false
				for _, view := range initial.Family.Views {
					found = found || view.ArtifactPath == "tools/helper" && view.Tree == "sdk"
				}
				if !found {
					t.Fatal("public-view negative lost its original view")
				}
			}
		})
	}
}

func TestInitialExecutableSelectionBudgetAndUnavailableFrontier(t *testing.T) {
	variants := []ActionPlanFamilyVariant{selectionArtifactVariant(t, "0"), selectionArtifactVariant(t, "1")}
	for _, limit := range [][2]int{{1, MaxActionPlanFamilyExecutionCutBytes}, {MaxActionPlanFamilyExecutionCutRecords, 1}} {
		initial, err := newActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants), limit[0], limit[1])
		if err != nil {
			t.Fatal(err)
		}
		selectionRequireValid(t, initial, 0, 0)
	}
	for name, demands := range map[string]map[string]ConfigDependencyGeneratedHeaderDemandCollection{
		"absent":    nil,
		"disabled":  {"base": {}, "other": {Enabled: true}},
		"truncated": {"base": {Enabled: true, Truncated: true}, "other": {Enabled: true}},
	} {
		t.Run(name, func(t *testing.T) {
			initial, err := NewActionPlanFamilyInitialExecution(variants, demands)
			if err != nil {
				t.Fatal(err)
			}
			selectionRequireValid(t, initial, 0, 0)
		})
	}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	if overflow, err := addInitialFamilyExecutableGroups(family, variants, groups, 0, 0, MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
		t.Fatalf("collect: %v %t", err, overflow)
	}
	if len(groups) != 1 {
		t.Fatal("cross-variant executable group split")
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.nodes = 1
	cut, err := selectActionPlanFamilyHeaderExecution(family, slices.Collect(maps.Values(groups)), limits, 128)
	if err != nil || len(cut.Roots()) != 0 {
		t.Fatalf("budget split atomic variant group: %v", err)
	}
}

func TestInitialExecutableSelectionMalformedBindings(t *testing.T) {
	for name, mutate := range map[string]func(*ActionPlanSnapshot){
		"missing slot": func(s *ActionPlanSnapshot) {
			for index := range s.Nodes {
				if len(s.Nodes[index].Inputs) > 0 {
					s.Nodes[index].Inputs[0].Slot = 99
					return
				}
			}
		},
		"missing producer": func(s *ActionPlanSnapshot) {
			for index := range s.Nodes {
				if len(s.Nodes[index].Inputs) > 0 {
					s.Nodes[index].Inputs[0].ProducerID = strings.Repeat("e", 64)
					return
				}
			}
		},
		"unbound executable": func(s *ActionPlanSnapshot) {
			for id, recipe := range s.Recipes {
				if recipe.CompilerInvocation != nil {
					recipe.ExecutableInputs = []string{"missing:00000000"}
					s.Recipes[id] = recipe
					return
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			variant := selectionArtifactVariant(t, "0")
			mutate(&variant.Snapshot)
			variants := []ActionPlanFamilyVariant{variant}
			if initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants)); err == nil || initial != nil {
				t.Fatalf("accepted malformed binding: %v %v", initial, err)
			}
		})
	}
}

func TestInitialExecutableSelectionPersistentAncestorsAndSidecars(t *testing.T) {
	variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(plan *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
		ancestor := *producer
		ancestor.ID = "ancestor"
		ancestor.Inputs = nil
		ancestor.Outputs = []ActionPlanOutput{{Tree: "prehost", Path: "tools/seed"}, {Tree: "prehost", Path: "seed.state", ObservedPath: "side-effect"}}
		store, err := plan.planningActionPlanInputSetStore()
		if err != nil {
			t.Fatal(err)
		}
		producer.InputSet, err = store.Insert("", ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "tools/seed"}, ProducerID: ancestor.ID, Slot: 0,
		})
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
		t.Fatal("persistent producer closure or complete output vector lost")
	}
	found := false
	for _, output := range initial.Cut.Outputs() {
		found = found || output.Output.Path == "tools/seed"
	}
	if !found {
		t.Fatal("persistent ancestor not captured")
	}
}

func TestInitialExecutableSelectionUnionWithHeaderGroups(t *testing.T) {
	variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, producer *ActionPlanNode) {
		headerRecipe := cloneActionRecipe(plan.Recipes[producer.Recipe])
		headerRecipe.Outputs, headerRecipe.ObservedOutputs = []string{"00000000"}, nil
		headerRecipeID, err := headerRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[headerRecipeID] = headerRecipe
		header := *producer
		header.ID, header.Stage, header.Recipe = "header", "target", headerRecipeID
		header.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "include/generated/data.h"}}
		otherRecipe := cloneActionRecipe(*recipe)
		otherRecipe.Inputs, otherRecipe.ExecutableInputs = []string{"generated:00000000"}, nil
		delete(otherRecipe.WorkingInputs, "input:helper:00000000")
		otherRecipe.WorkingInputs["input:generated:00000000"] = header.Outputs[0].Path
		otherRecipe.CompilerInvocation.WorkingInputUses = []string{"input:generated:00000000", "source:config:00000001", "source:source:00000000"}
		otherRecipe.CompilerInvocation.AuxiliaryWorkingInputUses = []string{"input:generated:00000000"}
		otherRecipe.Outputs = []string{"00000000"}
		otherRecipe.ObservedOutputs, otherRecipe.ObservedOutputBases = nil, nil
		otherRecipe.WorkingOutputs = map[string]string{"00000000": "drivers/header-user.o"}
		otherRecipeID, err := otherRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[otherRecipeID] = otherRecipe
		other := *consumer
		other.ID, other.Recipe = "header-user", otherRecipeID
		other.Inputs = []ActionPlanNodeEdge{{Role: "generated", ProducerID: header.ID, Slot: 0}}
		other.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/header-user.o"}}
		plan.Nodes = append(plan.Nodes, header, other)
	})
	var demand ConfigDependencyGeneratedHeaderDemand
	for _, node := range variant.Snapshot.Nodes {
		if node.Outputs[0].Path == "include/generated/data.h" {
			demand.ProducerNodeID, demand.Tree, demand.Path, demand.ArtifactPath, demand.LogicalPath = node.ID, "objects", node.Outputs[0].Path, actionPlanOutputArtifactPath(node.Outputs[0]), node.Outputs[0].Path
		}
		if node.Outputs[0].Path == "drivers/header-user.o" {
			demand.ConsumerNodeID = node.ID
		}
	}
	variants := []ActionPlanFamilyVariant{variant}
	demands := selectionEnabledDemands(variants)
	demands[variant.Name] = ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}
	initial, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 2, 2)
	if len(initial.Cut.Outputs()) != 3 {
		t.Fatal("header/executable union lost sidecar")
	}
	full, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Nodes) != 4 || len(initial.Family.Nodes) != len(full.Nodes) {
		t.Fatal("selection reduced only an extracted graph")
	}
}

func TestInitialExecutableSelectionDoesNotResurrectDeadConsumer(t *testing.T) {
	variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		deadRecipe := cloneActionRecipe(*recipe)
		deadRecipe.WorkingOutputs = map[string]string{"00000000": "unused.o"}
		deadRecipeID, err := deadRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[deadRecipeID] = deadRecipe
		dead := *consumer
		dead.ID, dead.Stage, dead.Recipe = "dead-consumer", "prep", deadRecipeID
		dead.Outputs = []ActionPlanOutput{{Tree: "prep", Path: "unused.o"}, {Tree: "prep", Path: "unused.state", ObservedPath: "side-effect"}}
		plan.Nodes = append(plan.Nodes, dead)
		recipe.CompilerInvocation, recipe.ExecutableInputs = nil, nil
	})
	variants := []ActionPlanFamilyVariant{variant}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 0, 0)
	for _, node := range variant.Snapshot.Nodes {
		if node.Outputs[0].Path == "unused.o" && initial.Family.originalNodeIDs[variant.Name][node.ID] != "" {
			t.Fatal("dead compiler fixture was not pruned")
		}
	}
}

func TestInitialExecutableSelectionNoFilenameOrDistinctOriginAssumption(t *testing.T) {
	base := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(_ *ActionPlan, _ *ActionPlanNode, recipe *ActionRecipe, producer *ActionPlanNode) {
		producer.Outputs[0].Path = "arbitrary/program-without-kernel-name"
		recipe.WorkingInputs["input:helper:00000000"] = producer.Outputs[0].Path
	})
	other := base
	other.Name = "same-bytes-other-config-name"
	variants := []ActionPlanFamilyVariant{base, other}
	initial, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 1, 1)
	if len(initial.Cut.Origins()) != 2 {
		t.Fatal("coalescing lost one original variant/producer origin")
	}
	if initial.Cut.Outputs()[0].Output.Path != "arbitrary/program-without-kernel-name" {
		t.Fatal("filename-specific executable selection")
	}
}
