package kconfig

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compactKbuildOverwriteTestConfig(t *testing.T, reverse bool) CompactConfig {
	t.Helper()
	writer := func(name, makefile, contents string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, makefile, "", contents, nil)
	}
	first := writer("a-writer", "scripts/a.mk", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`)
	before := writer("before-consumer", "scripts/before.mk", `
cmd_copy = cat $< > $@
before.out: generated/shared.out FORCE
	$(call if_changed,copy)
`)
	second := writer("b-writer", "scripts/b.mk", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`)
	after := writer("after-consumer", "scripts/after.mk", `
cmd_copy = cat $< > $@
after.out: generated/shared.out FORCE
	$(call if_changed,copy)
`)
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.out", Profile: first.Name, Target: "generated/shared.out",
	}
	secondArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.out", Profile: second.Name, Target: "generated/shared.out",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &before, []CompactKbuildVisibleArtifact{firstArtifact})
	before.InvocationPredecessors = []string{first.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{before.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &after, []CompactKbuildVisibleArtifact{secondArtifact})
	after.InvocationPredecessors = []string{second.Name}
	profiles := []CompactKbuildProfile{first, before, second, after}
	selections := []CompactKbuildSelection{
		{Profile: first.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		{
			Profile: before.Name, Target: "before.out", MakeTarget: "before.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
		},
		{
			Profile: second.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
		},
		{
			Profile: after.Name, Target: "after.out", MakeTarget: "after.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{secondArtifact}),
		},
	}
	if reverse {
		slices.Reverse(profiles)
		slices.Reverse(selections)
	}
	return CompactConfig{KbuildProfiles: profiles, KbuildSelections: selections}
}

func lowerCompactKbuildOverwriteTestPlan(
	t *testing.T,
	config CompactConfig,
) (*ActionPlan, *compactKbuildSelectionGraph) {
	t.Helper()
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	for _, selection := range ordered {
		key := compactKbuildSelectionKey{
			profile: selection.Profile, target: selection.Target, stage: selection.Stage,
		}
		if graph.forwardingSelections[key] {
			continue
		}
		profile := graph.profiles[key.profile]
		context := compactKbuildSelectionPlanContext(config, selection)
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).
			withSelectionGraph(graph).
			forSelection(key, profile).
			forOutput(context.Stage, context.OutputTree, context.Product)
		producer, err := builder.build(key.target)
		if err != nil {
			t.Fatalf("lower %s: %v", compactKbuildSelectionKeyString(key), err)
		}
		if err := graph.recordMaterializedProducer(key, producer); err != nil {
			t.Fatal(err)
		}
	}
	return plan, graph
}

func compactKbuildOverwriteTestNode(
	t *testing.T,
	plan *ActionPlan,
	graph *compactKbuildSelectionGraph,
	profile, target string,
) ActionPlanNode {
	t.Helper()
	key, ok := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile, target: target}]
	if !ok {
		t.Fatalf("missing selection %s:%s", profile, target)
	}
	producer, ok := graph.materializedProducers[key]
	if !ok {
		t.Fatalf("missing materialized producer for %s", compactKbuildSelectionKeyString(key))
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing node %s", producer)
	}
	return node
}

func compactKbuildOverwriteTestHasProducer(node ActionPlanNode, producer string) bool {
	for _, input := range node.Inputs {
		if input.ProducerID == producer {
			return true
		}
	}
	return false
}

func TestCompactKbuildOrderedSamePathWritersUseExactImmutableVersions(t *testing.T) {
	plan, graph := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, false))
	first := compactKbuildOverwriteTestNode(t, plan, graph, "a-writer", "generated/shared.out")
	before := compactKbuildOverwriteTestNode(t, plan, graph, "before-consumer", "before.out")
	second := compactKbuildOverwriteTestNode(t, plan, graph, "b-writer", "generated/shared.out")
	after := compactKbuildOverwriteTestNode(t, plan, graph, "after-consumer", "after.out")

	firstKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: "a-writer", target: "generated/shared.out"}]
	wantVersion := ".linux-bzl-versions/" + compactKbuildSelectionArtifactVersionID(firstKey) + "/generated/shared.out"
	if got := actionPlanOutputArtifactPath(first.Outputs[0]); got != wantVersion {
		t.Fatalf("first physical output = %q, want %q", got, wantVersion)
	}
	if !actionPlanOutputIsCanonical(second.Outputs[0]) {
		t.Fatalf("published tail is not canonical: %#v", second.Outputs[0])
	}
	canonical, _, ok := planProducerByOutput(plan, "objects", "generated/shared.out")
	if !ok || canonical != second.ID {
		t.Fatalf("canonical shared output = (%q, %t), want second writer %q", canonical, ok, second.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(second, first.ID) {
		t.Fatalf("second writer inputs = %#v, want exact first-writer edge %s", second.Inputs, first.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(before, first.ID) || compactKbuildOverwriteTestHasProducer(before, second.ID) {
		t.Fatalf("before-consumer inputs = %#v, want only first writer %s", before.Inputs, first.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(after, second.ID) || compactKbuildOverwriteTestHasProducer(after, first.ID) {
		t.Fatalf("after-consumer inputs = %#v, want only second writer %s", after.Inputs, second.ID)
	}

	physical := map[string]string{}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			key := actionPlanLookupKey(output.Tree, actionPlanOutputArtifactPath(output))
			if previous := physical[key]; previous != "" {
				t.Fatalf("physical output %q is owned by %s and %s", key, previous, node.ID)
			}
			physical[key] = node.ID
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("ordered same-path plan is invalid: %v", err)
	}
	for _, node := range []ActionPlanNode{first, second} {
		recipe := plan.Recipes[node.Recipe]
		if !actionPlanOutputIsCanonical(node.Outputs[0]) && recipe.WorkingOutputs["00000000"] != "generated/shared.out" {
			t.Fatalf("shadow writer recipe does not preserve logical output path: %#v", recipe)
		}
	}
}

func TestCompactKbuildOrderedSamePathPlanIsStableUnderSerializedInputReversal(t *testing.T) {
	first, _ := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, false))
	second, _ := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, true))
	if err := contentAddressActionPlanNodes(first); err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(second); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(first.Nodes, func(left, right ActionPlanNode) int { return strings.Compare(left.ID, right.ID) })
	slices.SortFunc(second.Nodes, func(left, right ActionPlanNode) int { return strings.Compare(left.ID, right.ID) })
	if !reflect.DeepEqual(first.Sources, second.Sources) ||
		!reflect.DeepEqual(first.Recipes, second.Recipes) ||
		!reflect.DeepEqual(first.Nodes, second.Nodes) {
		t.Fatalf("reversing serialized profiles/selections changed the plan\nfirst=%#v\nsecond=%#v", first.Nodes, second.Nodes)
	}
}

func TestCompactKbuildSamePathWritersRequireTotalSourceProvenOrder(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`, nil)
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous owners") || !strings.Contains(err.Error(), "unordered") {
		t.Fatalf("unordered same-path writers error = %v", err)
	}
}

func TestCompactKbuildOrderedSideOutputWritersResolveExactRuntimeWriter(t *testing.T) {
	producer := func(name, target string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "scripts/"+name+".mk", "", `
cmd_emit = touch $@
`+target+`: FORCE
	$(call if_changed,emit)
`, nil)
	}
	consumer := func(name, target string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "scripts/"+name+".mk", "", `
cmd_copy = cat $< > $@
`+target+`: generated/shared.side FORCE
	$(call if_changed,copy)
`, nil)
	}
	first := producer("side-a", "a.marker")
	before := consumer("side-before", "before.side.out")
	second := producer("side-b", "b.marker")
	after := consumer("side-after", "after.side.out")
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.side", Profile: first.Name, Target: "a.marker",
	}
	secondArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.side", Profile: second.Name, Target: "b.marker",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &before, []CompactKbuildVisibleArtifact{firstArtifact})
	before.InvocationPredecessors = []string{first.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{before.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &after, []CompactKbuildVisibleArtifact{secondArtifact})
	after.InvocationPredecessors = []string{second.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, before, second, after},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "a.marker", MakeTarget: "a.marker", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: before.Name, Target: "before.side.out", MakeTarget: "before.side.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "b.marker", MakeTarget: "b.marker", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: after.Name, Target: "after.side.out", MakeTarget: "after.side.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	firstNode := compactKbuildOverwriteTestNode(t, plan, graph, first.Name, "a.marker")
	secondNode := compactKbuildOverwriteTestNode(t, plan, graph, second.Name, "b.marker")
	beforeNode := compactKbuildOverwriteTestNode(t, plan, graph, before.Name, "before.side.out")
	afterNode := compactKbuildOverwriteTestNode(t, plan, graph, after.Name, "after.side.out")
	stateOutput := func(t *testing.T, node ActionPlanNode, logical string) (int, ActionPlanOutput) {
		t.Helper()
		for slot, candidate := range node.Outputs {
			if candidate.ObservedPath == logical {
				return slot, candidate
			}
		}
		t.Fatalf("node %s omits absolute runtime state for side output %q: %#v", node.ID, logical, node.Outputs)
		return 0, ActionPlanOutput{}
	}
	firstSlot, firstState := stateOutput(t, firstNode, "generated/shared.side")
	beforeSlot, beforeState := stateOutput(t, beforeNode, "generated/shared.side")
	secondSlot, secondState := stateOutput(t, secondNode, "generated/shared.side")
	statePaths := map[string]bool{}
	for _, state := range []ActionPlanOutput{firstState, beforeState, secondState} {
		if statePaths[state.Path] {
			t.Fatalf("distinct candidate states collide at %q", state.Path)
		}
		statePaths[state.Path] = true
	}
	for _, node := range []ActionPlanNode{firstNode, beforeNode, secondNode} {
		for _, output := range node.Outputs {
			if output.Path == "generated/shared.side" {
				t.Fatalf("candidate %s statically publishes opaque side output: %#v", node.ID, node.Outputs)
			}
		}
	}
	if producerID, slot, ok := planProducerByOutput(plan, "objects", "generated/shared.side"); ok {
		t.Fatalf("opaque side output has a guessed canonical producer (%q, %d)", producerID, slot)
	}
	for _, state := range []struct {
		name string
		node ActionPlanNode
		slot int
	}{
		{name: "first", node: firstNode, slot: firstSlot},
		{name: "before", node: beforeNode, slot: beforeSlot},
		{name: "second", node: secondNode, slot: secondSlot},
	} {
		if got := plan.Recipes[state.node.Recipe].ObservedOutputs[planOrdinal(state.slot)]; got != "generated/shared.side" {
			t.Fatalf("%s absolute state observes %q, want generated/shared.side", state.name, got)
		}
	}

	assertStateParents := func(t *testing.T, node ActionPlanNode, stateSlot int, want []ActionPlanNodeEdge) {
		t.Helper()
		got := []ActionPlanNodeEdge{}
		bindings := []string{}
		for index, input := range node.Inputs {
			if input.Role != "observed-state" {
				continue
			}
			got = append(got, input)
			binding := "observed-state:" + planOrdinal(index)
			if gotBinding := plan.Recipes[node.Recipe].Inputs[index]; gotBinding != binding {
				t.Fatalf("candidate %s state input %d binding = %q, want %q", node.ID, index, gotBinding, binding)
			}
			bindings = append(bindings, binding)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("candidate %s state parents = %#v, want nearest frontier %#v", node.ID, got, want)
		}
		if gotBases := plan.Recipes[node.Recipe].ObservedOutputBases[planOrdinal(stateSlot)]; !slices.Equal(gotBases, bindings) {
			t.Fatalf("candidate %s state base bindings = %q, want input bindings %q", node.ID, gotBases, bindings)
		}
	}
	assertStateParents(t, firstNode, firstSlot, nil)
	assertStateParents(t, beforeNode, beforeSlot, []ActionPlanNodeEdge{{
		Role: "observed-state", ProducerID: firstNode.ID, Slot: firstSlot,
	}})
	assertStateParents(t, secondNode, secondSlot, []ActionPlanNodeEdge{{
		Role: "observed-state", ProducerID: beforeNode.ID, Slot: beforeSlot,
	}})

	resolverFor := func(t *testing.T, consumer ActionPlanNode) ActionPlanNode {
		t.Helper()
		for _, input := range consumer.Inputs {
			candidate, exists := compactKbuildPlanNode(plan, input.ProducerID)
			if exists && candidate.Tool == "actionfile" && len(candidate.Outputs) == 1 && candidate.Outputs[0].Path == "generated/shared.side" {
				return candidate
			}
		}
		t.Fatalf("consumer %s has no runtime side-output resolver: %#v", consumer.ID, consumer.Inputs)
		return ActionPlanNode{}
	}
	beforeResolver := resolverFor(t, beforeNode)
	afterResolver := resolverFor(t, afterNode)
	for _, resolver := range []struct {
		name string
		node ActionPlanNode
		want ActionPlanNodeEdge
	}{
		{name: "before", node: beforeResolver, want: ActionPlanNodeEdge{Role: "state", ProducerID: firstNode.ID, Slot: firstSlot}},
		{name: "after", node: afterResolver, want: ActionPlanNodeEdge{Role: "state", ProducerID: secondNode.ID, Slot: secondSlot}},
	} {
		if got := resolver.node.Inputs; !slices.Equal(got, []ActionPlanNodeEdge{resolver.want}) {
			t.Fatalf("%s-side resolver inputs = %#v, want maximal absolute state %#v", resolver.name, got, resolver.want)
		}
		recipe := plan.Recipes[resolver.node.Recipe]
		if got, want := recipe.Inputs, []string{"state:00000000"}; !slices.Equal(got, want) {
			t.Fatalf("%s-side resolver input bindings = %q, want %q", resolver.name, got, want)
		}
		if got, want := recipe.Arguments, []string{
			"-out", "${output:00000000}", "-state", "${input:state:00000000}",
		}; !slices.Equal(got, want) {
			t.Fatalf("%s-side resolver arguments = %q, want %q", resolver.name, got, want)
		}
		if !recipe.ArgumentsFile {
			t.Fatalf("%s-side resolver does not use the arguments-file protocol: %#v", resolver.name, recipe)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("runtime-resolved same-path side-output plan is invalid: %v", err)
	}
}
