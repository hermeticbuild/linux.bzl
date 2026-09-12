package kconfig

import (
	"slices"
	"testing"
)

func TestInitialExecutableSelectionComplementsHeaderForSameConsumer(t *testing.T) {
	variant := selectionMutateVariant(t, selectionArtifactVariant(t, "0"), func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, producer *ActionPlanNode) {
		headerRecipe := cloneActionRecipe(plan.Recipes[producer.Recipe])
		headerRecipe.Outputs, headerRecipe.ObservedOutputs = []string{"00000000"}, nil
		id, err := headerRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id] = headerRecipe
		header := *producer
		header.ID, header.Recipe = "complementary-header", id
		header.Outputs = []ActionPlanOutput{{Tree: "prehost", Path: "include/generated/data.h"}}
		consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "generated", ProducerID: header.ID, Slot: 0})
		binding := "generated:00000002"
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.WorkingInputs["input:"+binding] = header.Outputs[0].Path
		recipe.CompilerInvocation.WorkingInputUses = append(recipe.CompilerInvocation.WorkingInputUses, "input:"+binding)
		slices.Sort(recipe.CompilerInvocation.WorkingInputUses)
		plan.Nodes = append(plan.Nodes, header)
	})
	var demand ConfigDependencyGeneratedHeaderDemand
	for _, node := range variant.Snapshot.Nodes {
		if node.Outputs[0].Path == "include/generated/data.h" {
			demand.ProducerNodeID, demand.Tree, demand.Path, demand.ArtifactPath, demand.LogicalPath = node.ID, "prehost", node.Outputs[0].Path, actionPlanOutputArtifactPath(node.Outputs[0]), node.Outputs[0].Path
		}
		if node.Kind == "compile" {
			demand.ConsumerNodeID = node.ID
		}
	}
	variants := []ActionPlanFamilyVariant{variant}
	demands := map[string]ConfigDependencyGeneratedHeaderDemandCollection{variant.Name: {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}}
	initial, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 2, 2)
	if len(initial.Cut.Outputs()) != 3 {
		t.Fatal("complementary selection lost complete sidecars")
	}
	for _, node := range initial.Cut.contract.Nodes {
		if node.Node.Kind == "compile" {
			t.Fatal("complementary roots pinned their compiler")
		}
	}
}
