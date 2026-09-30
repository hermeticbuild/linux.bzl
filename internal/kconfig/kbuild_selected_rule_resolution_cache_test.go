package kconfig

import "testing"

func selectedRootRuleResolutionCacheFixture(
	t *testing.T,
) (CompactKbuildProfile, CompactKbuildSelection, compactKbuildSelectionKey, *CompactMetadata, *compactKbuildSelectionGraph) {
	t.Helper()
	profile := mustCompactKbuildProfileForTest(t, "selected-root-cache", "Makefile", "", `
cmd_cc = $(CC) -c -o $@ $<
result.o: input.c FORCE
	$(call if_changed,cc)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.c")
	selection := CompactKbuildSelection{
		Profile: profile.Name, Target: "result.o", MakeTarget: "result.o",
		Lifecycle: "target", Scope: "target", Stage: "target",
	}
	config := CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{selection},
	}
	metadata := &CompactMetadata{
		Config:      config,
		actionRoles: testTargetActionRoles("cc"),
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	key := compactKbuildSelectionKey{profile: profile.Name, target: selection.Target, stage: selection.Stage}
	if _, err := graph.materializationOrder(metadata); err != nil {
		t.Fatal(err)
	}
	return profile, selection, key, metadata, graph
}

func TestSelectedRootRuleResolutionCacheIsExactAndOneShot(t *testing.T) {
	profile, selection, selectionKey, metadata, graph := selectedRootRuleResolutionCacheFixture(t)
	makeTarget := compactKbuildRuleLookupTarget(
		profile,
		selection.Target,
		selection.MakeTarget,
	)
	resolutionKey := compactKbuildRuleResolutionKey{
		metadata: metadata,
		compactKbuildProfileTargetKey: compactKbuildProfileTargetKey{
			profile: profile.Name,
			target:  selection.Target,
		},
		makeTarget: makeTarget,
	}
	resolution, resolved := graph.ruleResolutions[resolutionKey]
	if !resolved || !resolution.matched || resolution.err != nil {
		t.Fatalf("planned selected-root resolution = (%#v, %t), want successful match", resolution, resolved)
	}

	otherMetadata := &CompactMetadata{Config: metadata.Config, actionRoles: metadata.actionRoles}
	otherMetadataKey := resolutionKey
	otherMetadataKey.metadata = otherMetadata
	graph.ruleResolutions[otherMetadataKey] = resolution
	alternateMakeTargetKey := resolutionKey
	alternateMakeTargetKey.makeTarget = "sub/../result.o"
	graph.ruleResolutions[alternateMakeTargetKey] = resolution
	unselectedTargetKey := resolutionKey
	unselectedTargetKey.target = "unselected.o"
	graph.ruleResolutions[unselectedTargetKey] = resolution

	graph.releasePlanningCaches()
	if len(graph.ruleResolutions) != 0 {
		t.Fatalf("general rule-resolution cache retained %d entries", len(graph.ruleResolutions))
	}
	if got, want := len(graph.selectedRootRuleResolutions), 2; got != want {
		t.Fatalf("selected-root cache entries = %d, want %d exact metadata identities", got, want)
	}

	wrongStage := selectionKey
	wrongStage.stage = "host"
	if _, cached := graph.takeSelectedRootRuleResolution(
		metadata, wrongStage, profile, selection.Target, selection.MakeTarget,
	); cached {
		t.Fatal("selected-root cache matched a different selection stage")
	}
	if _, cached := graph.takeSelectedRootRuleResolution(
		metadata, selectionKey, profile, selection.Target, "sub/../result.o",
	); cached {
		t.Fatal("selected-root cache matched a different lexical Make target")
	}
	if got, want := len(graph.selectedRootRuleResolutions), 2; got != want {
		t.Fatalf("cache miss consumed an entry: got %d entries, want %d", got, want)
	}

	cachedResolution, cached := graph.takeSelectedRootRuleResolution(
		metadata, selectionKey, profile, selection.Target, selection.MakeTarget,
	)
	if !cached || !cachedResolution.matched || cachedResolution.err != nil {
		t.Fatalf("exact selected-root cache lookup = (%#v, %t), want successful hit", cachedResolution, cached)
	}
	if _, cached := graph.takeSelectedRootRuleResolution(
		metadata, selectionKey, profile, selection.Target, selection.MakeTarget,
	); cached {
		t.Fatal("selected-root cache returned the same resolution twice")
	}
	if got, want := len(graph.selectedRootRuleResolutions), 1; got != want {
		t.Fatalf("one-shot cache entries = %d, want %d other metadata identity", got, want)
	}
	if _, cached := graph.takeSelectedRootRuleResolution(
		otherMetadata, selectionKey, profile, selection.Target, selection.MakeTarget,
	); !cached {
		t.Fatal("selected-root cache did not isolate the second metadata identity")
	}
}

func TestBuildSelectedTargetConsumesPlannedRootResolution(t *testing.T) {
	profile, selection, selectionKey, metadata, graph := selectedRootRuleResolutionCacheFixture(t)
	graph.releasePlanningCaches()
	if got, want := len(graph.selectedRootRuleResolutions), 1; got != want {
		t.Fatalf("selected-root cache entries before lowering = %d, want %d", got, want)
	}

	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, selectionGraph: graph}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forSelection(selectionKey, profile)
	producer, err := builder.buildSelectedTarget(selection.Target, selection.MakeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.selectedRootRuleResolutions) != 0 {
		t.Fatalf("selected-root cache retained %d entries after lowering", len(graph.selectedRootRuleResolutions))
	}
	node, found := compactKbuildPlanNode(plan, producer)
	if !found {
		t.Fatalf("selected-root producer %q is absent", producer)
	}
	if node.Tool != "cc" || len(node.Outputs) != 1 || node.Outputs[0].Path != selection.Target {
		t.Fatalf("selected-root producer = %#v, want cc output %q", node, selection.Target)
	}
}
