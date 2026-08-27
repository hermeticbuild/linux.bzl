package kconfig

import "testing"

func TestContentAddressActionPlanNodesRewritesSelectedDependencyEdges(t *testing.T) {
	profile := CompactKbuildProfile{Name: "root", Path: "Makefile"}
	selections := []CompactKbuildSelection{
		{Profile: profile.Name, Target: "generated/input.o", MakeTarget: "generated/input.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		{Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile}, KbuildSelections: selections,
	}
	if _, err := newCompactKbuildSelectionGraph(config); err != nil {
		t.Fatalf("selection-based fixture is invalid: %v", err)
	}
	selectionID := func(selection CompactKbuildSelection) string {
		return compactKbuildSelectionKeyString(compactKbuildSelectionKey{
			profile: selection.Profile,
			target:  selection.Target,
			stage:   selection.Stage,
		})
	}

	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy",
		Arguments: []string{"${input:src:00000000}", "${output:00000000}"},
		Inputs:    []string{"src:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		ID: selectionID(selections[0]), Stage: "target", Kind: "generate", Recipe: producerRecipeID,
		Tool: "cc", Product: "vmlinux", Outputs: []ActionPlanOutput{{Tree: "objects", Path: selections[0].Target}},
	}
	consumer := ActionPlanNode{
		ID: selectionID(selections[1]), Stage: "target", Kind: "copy", Recipe: consumerRecipeID,
		Tool: "objcopy", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "src", ProducerID: producer.ID}},
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: selections[1].Target}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Nodes[0].ID, plan.Nodes[0].ContentID(); got != want || got == producer.ID {
		t.Fatalf("producer content ID = %q, want %q distinct from provisional %q", got, want, producer.ID)
	}
	if got, want := plan.Nodes[1].Inputs[0].ProducerID, plan.Nodes[0].ID; got != want {
		t.Fatalf("consumer producer edge = %q, want readdressed producer %q", got, want)
	}
	if got, want := plan.Nodes[1].ID, plan.Nodes[1].ContentID(); got != want || got == consumer.ID {
		t.Fatalf("consumer content ID = %q, want %q distinct from provisional %q", got, want, consumer.ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("content-addressed selected graph is invalid: %v", err)
	}
}
