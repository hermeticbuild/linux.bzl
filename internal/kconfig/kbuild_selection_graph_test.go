package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestPrepareGroupedSelectionsRejectsMissingTriggerAuthority(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "grouped", "Makefile", "", `
first.out second.out &: FORCE
	touch $@
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	err = graph.prepareGroupedSelections(&CompactMetadata{Config: config}, config)
	if err == nil || !strings.Contains(err.Error(), "no source-order trigger authority") {
		t.Fatalf("missing grouped trigger error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphIndexesExactSelectionsAndDeduplicates(t *testing.T) {
	profiles := []CompactKbuildProfile{
		{Name: "root", Path: "Makefile"},
		{Name: "build:drivers/demo#one", Path: "scripts/Makefile.build", Directory: "drivers/demo"},
	}
	config := CompactConfig{
		KbuildProfiles: profiles,
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profiles[1].Name, Target: "drivers/demo/prehost-tool", MakeTarget: "drivers/demo/prehost-tool", Scope: "host", Lifecycle: "target", Stage: "prehost"},
			{Profile: profiles[1].Name, Target: "drivers/demo/bootstrap.o", MakeTarget: "drivers/demo/bootstrap.o", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
			{Profile: profiles[0].Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profiles[1].Name, Target: "drivers/demo/helper", MakeTarget: "drivers/demo/helper", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: profiles[1].Name, Target: "drivers/demo/generated.h", MakeTarget: "drivers/demo/generated.h", Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: profiles[0].Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}

	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(graph.selections), 5; got != want {
		t.Fatalf("selection count = %d, want %d", got, want)
	}
	ordered := graph.orderedSelections()
	if got, want := len(ordered), 5; got != want {
		t.Fatalf("ordered selection count = %d, want %d", got, want)
	}
	for index, stage := range []string{"prehost", "bootstrap", "host", "prep", "target"} {
		if ordered[index].Stage != stage {
			t.Fatalf("ordered selections = %#v, want prehost/bootstrap/host/prep/target stage order", ordered)
		}
	}
	key := compactKbuildSelectionKey{profile: profiles[0].Name, target: "vmlinux", stage: "target"}
	if selection, ok := graph.selection(key); !ok || selection != config.KbuildSelections[2] {
		t.Fatalf("exact selection = %#v, %t, want %#v", selection, ok, config.KbuildSelections[2])
	}
	if profile, ok := graph.profile(profiles[1].Name); !ok || profile.Name != profiles[1].Name {
		t.Fatalf("profile lookup = %#v, %t", profile, ok)
	}
	if owner, ok := graph.owner("./vmlinux"); !ok || owner != key {
		t.Fatalf("vmlinux owner = %#v, %t, want %#v", owner, ok, key)
	}
	if got := len(graph.overwriteEdgesByOutput); got != 0 {
		t.Fatalf("unique selected outputs retained %d empty overwrite groups", got)
	}
	if got := len(graph.overwriteDependencies); got != 0 {
		t.Fatalf("unique selected outputs produced %d overwrite dependency groups", got)
	}
}

func TestCompactKbuildSelectionGraphRejectsMissingProfile(t *testing.T) {
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildSelections: []CompactKbuildSelection{{Profile: "missing", Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"}},
	})
	if err == nil || !strings.Contains(err.Error(), `missing profile "missing"`) {
		t.Fatalf("missing profile error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphRejectsRepeatedProfileIdentity(t *testing.T) {
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			{Name: "same", Path: "Makefile"},
			{Name: "same", Path: "scripts/Makefile.build"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `identity "same" is repeated`) {
		t.Fatalf("repeated profile error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphRejectsUnsupportedStage(t *testing.T) {
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{{Name: "root", Path: "Makefile"}},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: "root", Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "config",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `require physical stage "target", got "config"`) {
		t.Fatalf("unsupported stage error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphRejectsNonCanonicalOrNonConcreteTargets(t *testing.T) {
	for _, target := range []string{
		"./vmlinux",
		"drivers//demo.o",
		"drivers/../demo.o",
		" drivers/demo.o",
		`drivers\demo.o`,
		"drivers/%.o",
		"$(obj)/demo.o",
	} {
		t.Run(strings.NewReplacer("/", "_", "\\", "_").Replace(target), func(t *testing.T) {
			_, err := newCompactKbuildSelectionGraph(CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{{Name: "root", Path: "Makefile"}},
				KbuildSelections: []CompactKbuildSelection{{
					Profile: "root", Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target",
				}},
			})
			if err == nil {
				t.Fatalf("target %q was accepted", target)
			}
		})
	}
}

func TestCompactKbuildSelectionGraphRequiresProfileRootRelativeTarget(t *testing.T) {
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{{
			Name: "build:drivers/demo", Path: "scripts/Makefile.build", Directory: "drivers/demo",
		}},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: "build:drivers/demo", Target: "demo.o", MakeTarget: "demo.o", Lifecycle: "target", Scope: "target", Stage: "target",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `want "drivers/demo/demo.o"`) {
		t.Fatalf("profile-relative target error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphRetainsPhonyControlFlowWithoutArtifactOwner(t *testing.T) {
	profile := CompactKbuildProfile{
		Name: "root", Path: "Makefile",
		Rules: []KbuildRule{{Targets: []string{".PHONY"}, Prerequisites: []string{"clean"}}},
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{Profile: "root", Target: "clean", MakeTarget: "clean", Lifecycle: "target", Scope: "target", Stage: "target"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.orderedSelections(); len(got) != 1 || got[0].Target != "clean" {
		t.Fatalf("phony selections = %#v, want clean control-flow selection", got)
	}
	if owner, ok := graph.owner("clean"); ok {
		t.Fatalf("phony target owns native artifact via %#v", owner)
	}
}

func TestCompactKbuildSelectionGraphRejectsAmbiguousNativeOwner(t *testing.T) {
	for _, test := range []struct {
		name   string
		second CompactKbuildSelection
		owner  string
	}{
		{
			name:   "different profile",
			second: CompactKbuildSelection{Profile: "second", Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
			owner:  "(second, vmlinux, target)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newCompactKbuildSelectionGraph(CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{
					{Name: "first", Path: "Makefile"},
					{Name: "second", Path: "scripts/Makefile.vmlinux"},
				},
				KbuildSelections: []CompactKbuildSelection{
					{Profile: "first", Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
					test.second,
				},
			})
			if err == nil || !strings.Contains(err.Error(), `native artifact "vmlinux" has ambiguous owners`) ||
				!strings.Contains(err.Error(), "(first, vmlinux, target)") ||
				!strings.Contains(err.Error(), test.owner) {
				t.Fatalf("ambiguous owner error = %v", err)
			}
		})
	}
}

func TestCompactKbuildSelectionGraphLetsRecursiveInvocationOwnForwardedArtifact(t *testing.T) {
	parent := CompactKbuildProfile{
		Name: "parent", Path: "tools/objtool/Makefile",
		TargetInvocationDependencies: []CompactKbuildInvocationDependency{{
			Target: "tools/objtool/objtool-in.o", Profile: "child",
			Goals: []string{"tools/objtool/objtool-in.o"},
		}},
	}
	child := CompactKbuildProfile{Name: "child", Path: "tools/build/Makefile.build", Directory: "tools/objtool"}
	parentSelection := CompactKbuildSelection{Profile: parent.Name, Target: "tools/objtool/objtool-in.o", MakeTarget: "tools/objtool/objtool-in.o", Lifecycle: "prep", Scope: "target", Stage: "prep"}
	childSelection := CompactKbuildSelection{Profile: child.Name, Target: "tools/objtool/objtool-in.o", MakeTarget: "tools/objtool/objtool-in.o", Lifecycle: "prep", Scope: "target", Stage: "prep"}
	for _, selections := range [][]CompactKbuildSelection{
		{parentSelection, childSelection},
		{childSelection, parentSelection},
	} {
		graph, err := newCompactKbuildSelectionGraph(CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{parent, child}, KbuildSelections: selections,
		})
		if err != nil {
			t.Fatal(err)
		}
		owner, ok := graph.owner("tools/objtool/objtool-in.o")
		if !ok || owner.profile != child.Name {
			t.Fatalf("forwarded artifact owner = %#v, %t; want child invocation", owner, ok)
		}
		parentKey := compactKbuildSelectionKey{profile: parent.Name, target: parentSelection.Target, stage: parentSelection.Stage}
		if !graph.forwardingSelections[parentKey] {
			t.Fatalf("parent selection was not retained as forwarding boundary: %#v", graph.forwardingSelections)
		}
		if got := graph.orderedSelections(); len(got) != 1 || got[0].Profile != child.Name {
			t.Fatalf("public artifact selections = %#v, want only child owner", got)
		}
	}
}

func TestCompactKbuildSelectionGraphMaterializesExactTargetAtEarliestStage(t *testing.T) {
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{{Name: "first", Path: "Makefile"}},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: "first", Target: "scripts/mod/empty.o", MakeTarget: "scripts/mod/empty.o", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
			{Profile: "first", Target: "scripts/mod/empty.o", MakeTarget: "scripts/mod/empty.o", Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: "first", Target: "scripts/mod/empty.o", MakeTarget: "scripts/mod/empty.o", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: "first", Target: "scripts/mod/empty.o", MakeTarget: "scripts/mod/empty.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selections := graph.orderedSelections()
	if len(selections) != 1 || selections[0].Stage != "bootstrap" {
		t.Fatalf("exact target selections = %#v, want one bootstrap-stage owner", selections)
	}
}

func TestCompactKbuildSelectionGraphIndexesTargetInvocationDependencies(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first-child", "scripts/first.mk", "", `
cmd_emit = touch $@
first.out: FORCE
	$(call if_changed,emit)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second-child", "scripts/second.mk", "", `
cmd_emit = touch $@
second.out: FORCE
	$(call if_changed,emit)
`, nil)
	parent := mustCompactKbuildProfileForTest(t, "parent", "scripts/parent.mk", "", `
cmd_emit = touch $@
consumer-one.out: FORCE
	$(call if_changed,emit)
consumer-two.out: FORCE
	$(call if_changed,emit)
`, nil)
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{
		{Target: "consumer-one.out", Profile: first.Name, Goals: []string{"first.out"}},
		{Target: "consumer-two.out", Profile: second.Name, Goals: []string{"second.out"}},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: parent.Name, Target: "consumer-one.out", MakeTarget: "consumer-one.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: parent.Name, Target: "consumer-two.out", MakeTarget: "consumer-two.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: first.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}

	firstKey := compactKbuildProfileTargetKey{profile: parent.Name, target: "consumer-one.out"}
	secondKey := compactKbuildProfileTargetKey{profile: parent.Name, target: "consumer-two.out"}
	if got := graph.targetInvocations[firstKey]; len(got) != 1 || got[0] != first.Name {
		t.Fatalf("first target invocation index = %q, want %q", got, first.Name)
	}
	if got := graph.targetInvocations[secondKey]; len(got) != 1 || got[0] != second.Name {
		t.Fatalf("second target invocation index = %q, want %q", got, second.Name)
	}

	selectionKey := compactKbuildSelectionKey{profile: parent.Name, target: "consumer-one.out", stage: "target"}
	dependencies, err := graph.selectionDependencies(metadata, selectionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dependencies, []compactKbuildSelectionKey{{profile: first.Name, target: "first.out", stage: "target"}}; !slices.Equal(got, want) {
		t.Fatalf("first target dependencies = %#v, want %#v", got, want)
	}
}

func TestCompactKbuildSelectionGraphKeepsCanonicalGoalOutsideChildCwd(t *testing.T) {
	parent := CompactKbuildProfile{
		Name:         "objtool-parent",
		EntryTargets: []string{"tools/objtool/all"},
		TargetInvocationDependencies: []CompactKbuildInvocationDependency{{
			Target:  "tools/objtool/all",
			Profile: "fixdep-child",
			Goals:   []string{"tools/objtool/fixdep"},
		}},
	}
	child := CompactKbuildProfile{
		Name:         "fixdep-child",
		Directory:    "tools/build",
		EntryTargets: []string{"tools/objtool/fixdep"},
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, child},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := compactKbuildProfileTargetKey{profile: parent.Name, target: "tools/objtool/all"}
	if got, want := graph.targetInvocations[key], []string{child.Name}; !slices.Equal(got, want) {
		t.Fatalf("canonical child invocation index = %q, want %q", got, want)
	}
}

func TestCompactKbuildSelectionGraphOrdersEveryTerminalOfPredecessorInvocation(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
first.out:
	$(call if_changed,emit)
second.out:
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: producer.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{profile: consumer.Name, target: "consumer.out", stage: "target"}
	dependencies, err := graph.selectionDependencies(metadata, consumerKey)
	if err != nil {
		t.Fatal(err)
	}
	wantDependencies := []compactKbuildSelectionKey{
		{profile: producer.Name, target: "first.out", stage: "target"},
		{profile: producer.Name, target: "second.out", stage: "target"},
	}
	if !slices.Equal(dependencies, wantDependencies) {
		t.Fatalf("consumer dependencies = %#v, want every terminal predecessor %#v", dependencies, wantDependencies)
	}

	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, selection := range ordered {
		positions[selection.Target] = index
	}
	for _, predecessor := range []string{"first.out", "second.out"} {
		if positions[predecessor] >= positions["consumer.out"] {
			t.Fatalf("materialization order = %#v, want %s before consumer.out", ordered, predecessor)
		}
	}
}

func TestCompactKbuildSelectionGraphMemoizesResolvedDependenciesByGraphAndMetadata(t *testing.T) {
	newFixture := func(t *testing.T, profileName, producer string) (*compactKbuildSelectionGraph, *CompactMetadata, compactKbuildSelectionKey, compactKbuildSelectionKey) {
		t.Helper()
		profile := mustCompactKbuildProfileForTest(t, profileName, "Makefile", "", strings.ReplaceAll(`
cmd_emit = touch $@
PRODUCER:
	$(call if_changed,emit)
consumer.out: PRODUCER
	$(call if_changed,emit)
`, "PRODUCER", producer), nil)
		config := CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: producer, MakeTarget: producer, Lifecycle: "target", Scope: "target", Stage: "target"},
				{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			},
		}
		graph, err := newCompactKbuildSelectionGraph(config)
		if err != nil {
			t.Fatal(err)
		}
		return graph, &CompactMetadata{Config: config},
			compactKbuildSelectionKey{profile: profile.Name, target: "consumer.out", stage: "target"},
			compactKbuildSelectionKey{profile: profile.Name, target: producer, stage: "target"}
	}

	graph, metadata, consumer, producer := newFixture(t, "first", "producer.out")
	dependencies, err := graph.selectionDependencies(metadata, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dependencies, []compactKbuildSelectionKey{producer}) {
		t.Fatalf("dependencies = %#v, want producer %#v", dependencies, producer)
	}
	dependencies[0] = compactKbuildSelectionKey{profile: "poison", target: "poison", stage: "target"}
	graph.releasePlanningCaches()
	nativeMisses := graph.cacheMisses.nativeDependencies
	dependencies, err = graph.selectionDependencies(metadata, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dependencies, []compactKbuildSelectionKey{producer}) {
		t.Fatalf("caller mutation poisoned memoized dependencies: %#v", dependencies)
	}
	if len(graph.nativeDependencies) != 0 || graph.cacheMisses.nativeDependencies != nativeMisses {
		t.Fatalf("resolved dependency memo did not survive planning-cache release: native entries=%d misses=%d, want 0/%d", len(graph.nativeDependencies), graph.cacheMisses.nativeDependencies, nativeMisses)
	}
	if got, want := len(graph.resolvedDependencies), 1; got != want {
		t.Fatalf("resolved dependency memo entries = %d, want %d", got, want)
	}

	otherMetadata := &CompactMetadata{Config: metadata.Config}
	if _, err := graph.selectionDependencies(otherMetadata, consumer); err != nil {
		t.Fatal(err)
	}
	if got, want := len(graph.resolvedDependencies), 2; got != want {
		t.Fatalf("metadata identities share dependency memo entry: got %d entries, want %d", got, want)
	}

	otherGraph, otherGraphMetadata, otherConsumer, otherProducer := newFixture(t, "second", "other.out")
	otherDependencies, err := otherGraph.selectionDependencies(otherGraphMetadata, otherConsumer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(otherDependencies, []compactKbuildSelectionKey{otherProducer}) {
		t.Fatalf("other graph dependencies = %#v, want %#v", otherDependencies, otherProducer)
	}
	if got, want := len(otherGraph.resolvedDependencies), 1; got != want {
		t.Fatalf("other graph dependency memo entries = %d, want %d", got, want)
	}
}

func TestCompactKbuildSelectionGraphInvalidatesResolvedDependenciesForSideOutputEdges(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "side-output", "Makefile", "", `
cmd_emit = touch $@
producer.out:
	$(call if_changed,emit)
consumer.out:
	$(call if_changed,emit)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "producer.out", MakeTarget: "producer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	producer := compactKbuildSelectionKey{profile: profile.Name, target: "producer.out", stage: "target"}
	consumer := compactKbuildSelectionKey{profile: profile.Name, target: "consumer.out", stage: "target"}
	dependencies, err := graph.selectionDependencies(metadata, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 0 {
		t.Fatalf("initial dependencies = %#v, want none", dependencies)
	}
	if err := graph.compactKbuildRegisterSideOutputCandidateDependencies([]compactKbuildSideOutputDemand{{
		consumer: consumer, candidates: []compactKbuildSelectionKey{producer}, output: ActionPlanOutput{Path: "opaque.out"},
	}}); err != nil {
		t.Fatal(err)
	}
	dependencies, err = graph.selectionDependencies(metadata, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dependencies, []compactKbuildSelectionKey{producer}) {
		t.Fatalf("dependencies after side-output registration = %#v, want %#v", dependencies, producer)
	}
}

func TestCompactKbuildSelectionGraphDoesNotMemoizeDependencyErrors(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "stages", "Makefile", "", `
cmd_emit = touch $@
later.out:
	$(call if_changed,emit)
early.out: later.out
	$(call if_changed,emit)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "later.out", MakeTarget: "later.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "early.out", MakeTarget: "early.out", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumer := compactKbuildSelectionKey{profile: profile.Name, target: "early.out", stage: "bootstrap"}
	cacheKey := compactKbuildSelectionMetadataKey{metadata: metadata, selection: consumer}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := graph.selectionDependencies(metadata, consumer); err == nil || !strings.Contains(err.Error(), "later physical stage") {
			t.Fatalf("dependency error attempt %d = %v", attempt, err)
		}
		if _, cached := graph.resolvedDependencies[cacheKey]; cached {
			t.Fatalf("dependency error attempt %d was memoized", attempt)
		}
	}
}

func TestCompactKbuildSelectionGraphOmitsLaterStagePredecessorForBootstrapConsumer(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
host.out:
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
bootstrap.out:
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "host.out", MakeTarget: "host.out", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: consumer.Name, Target: "bootstrap.out", MakeTarget: "bootstrap.out", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{profile: consumer.Name, target: "bootstrap.out", stage: "bootstrap"}
	dependencies, err := graph.selectionDependencies(metadata, consumerKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 0 {
		t.Fatalf("bootstrap consumer dependencies = %#v, want later host-stage invocation predecessor omitted", dependencies)
	}
}

func TestCompactKbuildSelectionGraphRejectsLaterStageNativeDependencyForBootstrapConsumer(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "producer-consumer", "scripts/native.mk", "", `
cmd_emit = touch $@
host.out:
	$(call if_changed,emit)
bootstrap.out: host.out
	$(call if_changed,emit)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "host.out", MakeTarget: "host.out", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: profile.Name, Target: "bootstrap.out", MakeTarget: "bootstrap.out", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{profile: profile.Name, target: "bootstrap.out", stage: "bootstrap"}
	_, err = graph.selectionDependencies(metadata, consumerKey)
	if err == nil || !strings.Contains(err.Error(), "native artifact dependency") || !strings.Contains(err.Error(), "later physical stage") {
		t.Fatalf("later-stage native dependency error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphRejectsLaterStageNativeDependencyHiddenByWeakBoundary(t *testing.T) {
	early := mustCompactKbuildProfileForTest(t, "early-mixed", "scripts/early.mk", "", `
cmd_emit = touch $@
post-host.out:
	$(call if_changed,emit)
`, nil)
	later := mustCompactKbuildProfileForTest(t, "later-host", "scripts/later.mk", "", `
cmd_emit = touch $@
later-host.out: post-host.out
	$(call if_changed,emit)
`, nil)
	later.InvocationPredecessors = []string{early.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{early, later},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: early.Name, Target: "post-host.out", MakeTarget: "post-host.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: later.Name, Target: "later-host.out", MakeTarget: "later-host.out", Lifecycle: "target", Scope: "host", Stage: "host"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{profile: later.Name, target: "later-host.out", stage: "host"}
	_, err = graph.selectionDependencies(metadata, consumerKey)
	if err == nil || !strings.Contains(err.Error(), "native artifact dependency") || !strings.Contains(err.Error(), "later physical stage") {
		t.Fatalf("weak-boundary later-stage native dependency error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphOrdersSelectedOwnerBeforeLexicallyEarlierDependent(t *testing.T) {
	parent := mustCompactKbuildProfileForTest(t, "a-parent-profile", "scripts/Makefile.parent", "", `
cmd_copy = cat $< > $@
cmd_poison = $(CC) -DPOISON_PROFILE -c -o $@ $<
a-parent.out: z-dependency.out FORCE
	$(call if_changed,copy)
z-dependency.out: input.c FORCE
	$(call if_changed,poison)
`, map[string]string{"CC": "/selected/cc"})
	owner := mustCompactKbuildProfileForTest(t, "z-owner-profile", "scripts/Makefile.owner", "", `
cmd_owner = $(CC) -DOWNING_PROFILE -c -o $@ $<
z-dependency.out: input.c FORCE
	$(call if_changed,owner)
`, map[string]string{"CC": "/selected/cc"})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, owner},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: parent.Name, Target: "a-parent.out", MakeTarget: "a-parent.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: owner.Name, Target: "z-dependency.out", MakeTarget: "z-dependency.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(ordered), 2; got != want {
		t.Fatalf("materialization count = %d, want %d: %#v", got, want, ordered)
	}
	if ordered[0].Profile != owner.Name || ordered[0].Target != "z-dependency.out" || ordered[1].Target != "a-parent.out" {
		t.Fatalf("materialization order = %#v, want selected owner before lexically earlier dependent", ordered)
	}
}

func TestCompactKbuildSelectionGraphOrdersInitialVisibleOwnerBeforeConsumer(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "a-consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "z-owner", "scripts/owner.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: owner.Name, Target: "generated/header.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{visibleArtifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, owner},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true, InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{visibleArtifact})},
			{Profile: owner.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, selection := range ordered {
		positions[selection.Target] = index
	}
	ownerPosition, ownerFound := positions["generated/header.h"]
	consumerPosition, consumerFound := positions["consumer.out"]
	if !ownerFound || !consumerFound || ownerPosition >= consumerPosition {
		t.Fatalf("materialization order = %#v, want initial visible owner before lexical consumer", ordered)
	}
}

func TestCompactKbuildSelectionGraphOrdersGeneratedOwnerWithoutInitialFrontier(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "a-consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "z-owner", "scripts/owner.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	generatedArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: owner.Name, Target: "generated/header.h",
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, owner},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
				GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{generatedArtifact}),
			},
			{Profile: owner.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(&CompactMetadata{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, selection := range ordered {
		positions[selection.Profile+"\x00"+selection.Target] = index
	}
	ownerPosition, ownerFound := positions[owner.Name+"\x00generated/header.h"]
	consumerPosition, consumerFound := positions[consumer.Name+"\x00consumer.out"]
	if !ownerFound || !consumerFound || ownerPosition >= consumerPosition {
		t.Fatalf("materialization order = %#v, want generated owner before consumer without an initial frontier", ordered)
	}
}

func TestCompactKbuildSelectionPathOwnerRequiresRecordedCrossProfileProvenance(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "owner", "scripts/owner.mk", "", `
cmd_emit = touch $@
kernel/built-in.a:
	$(call if_changed,emit)
`, nil)
	artifact := CompactKbuildVisibleArtifact{
		Path: "kernel/built-in.a", Profile: owner.Name, Target: "kernel/built-in.a",
	}
	for _, test := range []struct {
		name      string
		generated string
		wantOwner bool
	}{
		{name: "unique pathname alone is not provenance"},
		{
			name:      "generated frontier is exact provenance",
			generated: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
			wantOwner: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{consumer, owner},
				KbuildSelections: []CompactKbuildSelection{
					{
						Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
						GeneratedObjectTreeArtifacts: test.generated,
					},
					{Profile: owner.Name, Target: artifact.Target, MakeTarget: artifact.Target, Lifecycle: "target", Scope: "target", Stage: "target"},
				},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			consumerKey := compactKbuildSelectionKey{profile: consumer.Name, target: "consumer.out", stage: "target"}
			got, selected, err := graph.compactKbuildSelectionRecordedPathOwner(consumerKey, artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			if selected != test.wantOwner {
				t.Fatalf("selected owner = %t (%#v), want %t", selected, got, test.wantOwner)
			}
			if selected && (got.profile != owner.Name || got.target != artifact.Target) {
				t.Fatalf("selected owner = %#v, want exact generated artifact owner", got)
			}
		})
	}
}

func TestCompactKbuildSelectionRecordedPathOwnerUsesExactTargetFrontier(t *testing.T) {
	const pathname = "generated/future.h"
	profile := mustCompactKbuildProfileForTest(t, "build:shared", "scripts/shared.mk", "", `
cmd_emit = touch $@
consumer.o:
	$(call if_changed,emit)
generated/future.h:
	$(call if_changed,emit)
`, nil)
	artifact := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: profile.Name, Target: pathname,
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{artifact})
	consumer := CompactKbuildSelection{
		Profile: profile.Name, Target: "consumer.o", MakeTarget: "consumer.o", Lifecycle: "target", Scope: "target", Stage: "target",
	}
	writer := CompactKbuildSelection{
		Profile: profile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target",
	}
	config := CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{consumer, writer},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{
		profile: profile.Name, target: consumer.Target, stage: consumer.Stage,
	}
	if owner, selected, err := graph.compactKbuildSelectionRecordedPathOwner(consumerKey, pathname); err != nil {
		t.Fatal(err)
	} else if selected {
		t.Fatalf("unobserved same-invocation writer selected as exact owner: %#v", owner)
	}

	consumer.UsesInitialObjectTree = true
	consumer.InitialObjectTreeArtifacts = EncodeCompactKbuildInitialObjectTreeArtifacts(
		[]CompactKbuildVisibleArtifact{artifact},
	)
	config.KbuildSelections[0] = consumer
	graph, err = newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	owner, selected, err := graph.compactKbuildSelectionRecordedPathOwner(consumerKey, pathname)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || owner.profile != profile.Name || owner.target != pathname {
		t.Fatalf("exact target frontier owner = (%#v, %t), want %s:%s", owner, selected, profile.Name, pathname)
	}
}

func TestCompactKbuildSelectionGraphRejectsGeneratedArtifactWithoutTerminalOwner(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "owner", "scripts/owner.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: owner.Name, Target: "generated/header.h",
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, owner},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
			GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
		}},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.materializationOrder(&CompactMetadata{Config: config})
	want := `visible artifact "generated/header.h" from owner:generated/header.h resolves to 0 terminal selected owners`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("missing generated-artifact owner error = %v, want substring %q", err, want)
	}
}

func TestCompactKbuildSelectionGraphRejectsGeneratedArtifactWithTwoTerminalOwners(t *testing.T) {
	const pathname = "generated/header.h"
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	forwarder := mustCompactKbuildProfileForTest(t, "forwarder", "scripts/forwarder.mk", "", `
generated/header.h:
	@:
`, nil)
	first := mustCompactKbuildProfileForTest(t, "first-owner", "scripts/first.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second-owner", "scripts/second.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	forwarder.TargetInvocationDependencies = []CompactKbuildInvocationDependency{
		{Target: pathname, Profile: first.Name, Goals: []string{pathname}},
		{Target: pathname, Profile: second.Name, Goals: []string{pathname}},
	}
	artifact := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: forwarder.Name, Target: pathname,
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, forwarder, first, second},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
				GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
			},
			{Profile: forwarder.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: first.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: second.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.materializationOrder(&CompactMetadata{Config: config})
	if err == nil ||
		!strings.Contains(err.Error(), `visible artifact "generated/header.h" from forwarder:generated/header.h resolves to 2 terminal selected owners`) ||
		!strings.Contains(err.Error(), "first-owner") ||
		!strings.Contains(err.Error(), "second-owner") {
		t.Fatalf("ambiguous generated-artifact owner error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphUsesRecordedOwnerForUnrelatedSamePathSelections(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "a-consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	firstWriter := mustCompactKbuildProfileForTest(t, "m-first-writer", "scripts/first.mk", "", `
cmd_emit = touch $@
generated/shared.h:
	$(call if_changed,emit)
`, nil)
	lastWriter := mustCompactKbuildProfileForTest(t, "z-last-writer", "scripts/last.mk", "", `
cmd_emit = touch $@
generated/shared.h:
	$(call if_changed,emit)
`, nil)
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.h", Profile: lastWriter.Name, Target: "generated/shared.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{artifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, firstWriter, lastWriter},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
			},
			{Profile: firstWriter.Name, Target: "generated/shared.h", MakeTarget: "generated/shared.h", Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: lastWriter.Name, Target: "generated/shared.h", MakeTarget: "generated/shared.h", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatalf("same-path selections in distinct physical trees: %v", err)
	}
	if owner, ok := graph.owner(artifact.Path); ok {
		t.Fatalf("same-path global owner = %#v, want no path-only owner", owner)
	}
	exact, err := graph.compactKbuildVisibleArtifactOwner(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if exact.profile != lastWriter.Name || exact.target != artifact.Target || exact.stage != "target" {
		t.Fatalf("exact visible owner = %#v, want recorded last writer", exact)
	}
	ordered, err := graph.materializationOrder(&CompactMetadata{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, selection := range ordered {
		positions[selection.Profile+"\x00"+selection.Target] = index
	}
	if positions[lastWriter.Name+"\x00"+artifact.Target] >= positions[consumer.Name+"\x00consumer.out"] {
		t.Fatalf("materialization order = %#v, want recorded last writer before consumer", ordered)
	}
}

func TestCompactKbuildSelectionGraphUsesInvocationFrontierForNativePrerequisite(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "boot:image", "arch/arm/boot/Makefile", "arch/arm/boot", `
cmd_objcopy = objcopy $< $@
$(obj)/Image: vmlinux FORCE
	$(call if_changed,objcopy)
`, map[string]string{"obj": "__LINUX_BZL_OBJECT_TREE__/arch/arm/boot"})
	parent := mustCompactKbuildProfileForTest(t, "boot:zImage", "arch/arm/boot/Makefile", "arch/arm/boot", `
cmd_objcopy = objcopy $< $@
$(obj)/Image: vmlinux FORCE
	$(call if_changed,objcopy)
`, map[string]string{"obj": "__LINUX_BZL_OBJECT_TREE__/arch/arm/boot"})
	child := mustCompactKbuildProfileForTest(t, "boot:compressed", "arch/arm/boot/compressed/Makefile", "arch/arm/boot/compressed", `
cmd_compress = gzip -c $< > $@
$(obj)/piggy_data: $(obj)/../Image FORCE
	$(call if_changed,compress)
`, map[string]string{"obj": "__LINUX_BZL_OBJECT_TREE__/arch/arm/boot/compressed"})
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: "arch/arm/boot/Image", Profile: first.Name, Target: "arch/arm/boot/Image",
	}
	parentArtifact := CompactKbuildVisibleArtifact{
		Path: "arch/arm/boot/Image", Profile: parent.Name, Target: "arch/arm/boot/Image",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &parent, []CompactKbuildVisibleArtifact{firstArtifact})
	parent.InvocationPredecessors = []string{first.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &child, []CompactKbuildVisibleArtifact{
		{Path: "arch/arm/boot/000-before", Profile: first.Name, Target: firstArtifact.Target},
		parentArtifact,
		{Path: "arch/arm/boot/zzz-after", Profile: parent.Name, Target: parentArtifact.Target},
	})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, parent, child},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: firstArtifact.Target, MakeTarget: firstArtifact.Target, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: parent.Name, Target: parentArtifact.Target, MakeTarget: parentArtifact.Target, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: child.Name, Target: "arch/arm/boot/compressed/piggy_data", MakeTarget: "arch/arm/boot/compressed/piggy_data", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumer := compactKbuildSelectionKey{
		profile: child.Name, target: "arch/arm/boot/compressed/piggy_data", stage: "target",
	}
	owner, selected, err := graph.compactKbuildSelectionPathOwner(consumer, parentArtifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || owner.profile != parent.Name || owner.target != parentArtifact.Target {
		t.Fatalf("native prerequisite owner = (%#v, %t), want exact invocation-frontier owner %s:%s", owner, selected, parent.Name, parentArtifact.Target)
	}
	if got, ok := graph.compactKbuildInitialVisibleArtifact(child.Name, "arch/arm/boot/not-present"); ok {
		t.Fatalf("missing invocation-frontier lookup = %#v, want no artifact", got)
	}
	metadata := &CompactMetadata{Config: config}
	match, matched, err := graph.compactKbuildRuleForProfileMakeTarget(
		metadata, child, consumer.target, consumer.target,
	)
	if err != nil || !matched {
		t.Fatalf("piggy_data rule = (%#v, %t, %v), want selected native-prerequisite rule", match, matched, err)
	}
	normal, _, _, err := graph.compactKbuildTargetRuleContext(child, consumer.target, match)
	if err != nil {
		t.Fatalf("piggy_data rule context: %v", err)
	}
	if len(normal) == 0 {
		t.Fatalf("piggy_data rule context has no normal prerequisites: %#v", match)
	}
	dependencies, err := graph.selectionDependencies(metadata, consumer)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := compactKbuildSelectionKey{profile: first.Name, target: firstArtifact.Target, stage: "target"}
	parentKey := compactKbuildSelectionKey{profile: parent.Name, target: parentArtifact.Target, stage: "target"}
	if !slices.Contains(dependencies, parentKey) || slices.Contains(dependencies, firstKey) {
		t.Fatalf("piggy_data prerequisites = %#v, dependencies = %#v, want later Image owner %#v and not earlier owner %#v", normal, dependencies, parentKey, firstKey)
	}
	if owner, selected, err := graph.compactKbuildSelectionRecordedPathOwner(consumer, parentArtifact.Path); err == nil || !compactKbuildPathOwnerIsUnrecorded(err) || selected {
		t.Fatalf("strict recorded owner = (%#v, %t, %v), want unrecorded ambiguity", owner, selected, err)
	}
}

func TestCompactKbuildSelectionGraphRejectsPathOnlyInitialArtifactMatch(t *testing.T) {
	const pathname = "generated/shared.h"
	consumer := CompactKbuildProfile{
		Name: "consumer", Path: "scripts/consumer.mk", EntryTargets: []string{"consumer.out"},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{{
		Path: pathname, Profile: "recorded-writer", Target: pathname,
	}})
	wrong := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: "unrelated-writer", Target: pathname,
	}
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			consumer,
			{Name: "recorded-writer", Path: "scripts/recorded.mk", EntryTargets: []string{pathname}},
			{Name: "unrelated-writer", Path: "scripts/unrelated.mk", EntryTargets: []string{pathname}},
		},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{wrong}),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "outside its exact visible frontier") {
		t.Fatalf("path-only provenance match error = %v, want exact-record rejection", err)
	}
}

func TestCompactKbuildSelectionGraphMovesArtifactCacheToEarlierStage(t *testing.T) {
	firstArtifact := CompactKbuildVisibleArtifact{Path: "generated/a.h", Profile: "first", Target: "generated/a.h"}
	secondArtifact := CompactKbuildVisibleArtifact{Path: "generated/b.h", Profile: "second", Target: "generated/b.h"}
	consumer := CompactKbuildProfile{
		Name: "consumer", Path: "scripts/consumer.mk", EntryTargets: []string{"consumer.out"},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{firstArtifact, secondArtifact})
	selection := func(lifecycle, stage string, artifact CompactKbuildVisibleArtifact) CompactKbuildSelection {
		return CompactKbuildSelection{
			Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: lifecycle, Scope: "target", Stage: stage,
			UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
				[]CompactKbuildVisibleArtifact{artifact},
			),
		}
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			consumer,
			{Name: "first", Path: "scripts/first.mk", EntryTargets: []string{firstArtifact.Target}},
			{Name: "second", Path: "scripts/second.mk", EntryTargets: []string{secondArtifact.Target}},
		},
		KbuildSelections: []CompactKbuildSelection{
			selection("target", "target", firstArtifact),
			selection("prep", "prep", secondArtifact),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepKey := compactKbuildSelectionKey{profile: consumer.Name, target: "consumer.out", stage: "prep"}
	targetKey := compactKbuildSelectionKey{profile: consumer.Name, target: "consumer.out", stage: "target"}
	if got := graph.selectionInitialArtifacts[prepKey]; !slices.Equal(got, []CompactKbuildVisibleArtifact{secondArtifact}) {
		t.Fatalf("winning prep selection artifact cache = %#v, want %#v", got, secondArtifact)
	}
	if _, exists := graph.selectionInitialArtifacts[targetKey]; exists {
		t.Fatalf("replaced target-stage selection retained a stale artifact cache: %#v", graph.selectionInitialArtifacts[targetKey])
	}
}

func TestCompactKbuildSelectionGraphOrdersRecursiveForwardingOwnerBeforeInitialVisibleConsumer(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "a-consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	forwarder := mustCompactKbuildProfileForTest(t, "m-forwarder", "scripts/forwarder.mk", "", `
generated/header.h:
	@:
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "z-recursive-owner", "scripts/owner.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)
	forwarder.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "generated/header.h", Profile: owner.Name, Goals: []string{"generated/header.h"},
	}}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: forwarder.Name, Target: "generated/header.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{visibleArtifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, forwarder, owner},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true, InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{visibleArtifact})},
			{Profile: forwarder.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: owner.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	selectedOwner, ok := graph.owner("generated/header.h")
	if !ok || selectedOwner.profile != owner.Name {
		t.Fatalf("recursive forwarded owner = %#v, %t; want %q", selectedOwner, ok, owner.Name)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for index, selection := range ordered {
		positions[selection.Profile+"\x00"+selection.Target] = index
	}
	ownerPosition, ownerFound := positions[owner.Name+"\x00generated/header.h"]
	consumerPosition, consumerFound := positions[consumer.Name+"\x00consumer.out"]
	if !ownerFound || !consumerFound || ownerPosition >= consumerPosition {
		t.Fatalf("materialization order = %#v, want recursive forwarded owner before lexical consumer", ordered)
	}
}

func TestCompactKbuildSelectionGraphRejectsInvalidInitialVisibleOwner(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out:
	$(call if_changed,consume)
`, nil)
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
generated/header.h:
	$(call if_changed,emit)
`, nil)

	for _, test := range []struct {
		name      string
		artifact  CompactKbuildVisibleArtifact
		profiles  []CompactKbuildProfile
		selection CompactKbuildSelection
		want      string
	}{
		{
			name: "missing selected owner",
			artifact: CompactKbuildVisibleArtifact{
				Path: "generated/header.h", Profile: producer.Name, Target: "generated/header.h",
			},
			profiles:  []CompactKbuildProfile{consumer, producer},
			selection: CompactKbuildSelection{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true},
			want:      `visible artifact "generated/header.h" from producer:generated/header.h resolves to 0 terminal selected owners`,
		},
		{
			name: "self owner",
			artifact: CompactKbuildVisibleArtifact{
				Path: "consumer.out", Profile: consumer.Name, Target: "consumer.out",
			},
			profiles:  []CompactKbuildProfile{consumer},
			selection: CompactKbuildSelection{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true},
			want:      `consumes itself through initial visible artifact "consumer.out"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profiles := append([]CompactKbuildProfile(nil), test.profiles...)
			setTestCompactKbuildInitialVisibleArtifacts(t, &profiles[0], []CompactKbuildVisibleArtifact{test.artifact})
			test.selection.InitialObjectTreeArtifacts = EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{test.artifact})
			config := CompactConfig{
				KbuildProfiles:   profiles,
				KbuildSelections: []CompactKbuildSelection{test.selection},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = graph.materializationOrder(&CompactMetadata{Config: config})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid initial visible owner error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestCompactKbuildSelectionGraphRejectsSelectedArtifactDependencyCycle(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "root", "Makefile", "", `
cmd_copy = cat $< > $@
first.out: second.out FORCE
	$(call if_changed,copy)
second.out: first.out FORCE
	$(call if_changed,copy)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.materializationOrder(&CompactMetadata{Config: config})
	if err == nil || !strings.Contains(err.Error(), "selected Kbuild artifact dependency cycle") ||
		!strings.Contains(err.Error(), "first.out") || !strings.Contains(err.Error(), "second.out") {
		t.Fatalf("selected dependency cycle error = %v", err)
	}
}

func TestCompactKbuildSelectionGraphReleasePlanningCachesRemainsReusable(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "root", "Makefile", "", `
cmd_copy = cat $< > $@
result.out: input.c FORCE
	$(call if_changed,copy)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "result.out", MakeTarget: "result.out", Lifecycle: "target", Scope: "target", Stage: "target",
		}},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.materializationOrder(metadata); err != nil {
		t.Fatal(err)
	}
	if len(graph.ruleResolutions) == 0 || len(graph.nativeDependencies) == 0 {
		t.Fatalf("planning did not populate caches: rules=%d dependencies=%d", len(graph.ruleResolutions), len(graph.nativeDependencies))
	}

	graph.releasePlanningCaches()
	if len(graph.ruleResolutions) != 0 || len(graph.nativeDependencies) != 0 {
		t.Fatalf("released caches remain populated: rules=%d dependencies=%d", len(graph.ruleResolutions), len(graph.nativeDependencies))
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 1 || ordered[0].Target != "result.out" {
		t.Fatalf("recomputed materialization order = %#v", ordered)
	}
}

func TestCompactKbuildSelectionGraphCachesLexicalRuleAliasesSeparately(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "aliases", "scripts/Makefile.build", "", `
cmd_cc = touch $@
left/%.o: left/%.c FORCE
	$(call if_changed,cc)
right/%.o: right/%.S FORCE
	$(call if_changed,cc)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "shared/unit.c", "shared/unit.S")
	config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}

	type aliasCase struct {
		makeTarget string
		graphInput string
		makeInput  string
	}
	for _, test := range []aliasCase{
		{makeTarget: "left/../shared/unit.o", graphInput: "shared/unit.c", makeInput: "left/../shared/unit.c"},
		{makeTarget: "right/../shared/unit.o", graphInput: "shared/unit.S", makeInput: "right/../shared/unit.S"},
	} {
		match, found, err := graph.compactKbuildRuleForProfileMakeTarget(
			metadata, profile, "shared/unit.o", test.makeTarget,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("lexical alias %q did not resolve an implicit rule", test.makeTarget)
		}
		normal, _, _, err := graph.compactKbuildTargetRuleContext(profile, "shared/unit.o", match)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := normal[0], (compactKbuildEvaluatedPath{graphPath: test.graphInput, makeWord: test.makeInput}); got != want {
			t.Fatalf("lexical alias %q prerequisite = %#v, want %#v", test.makeTarget, got, want)
		}
	}
	if got, want := len(graph.ruleResolutions), 2; got != want {
		t.Fatalf("lexical rule-resolution cache entries = %d, want %d", got, want)
	}
	if got, want := len(graph.targetRuleContexts), 2; got != want {
		t.Fatalf("lexical rule-context cache entries = %d, want %d", got, want)
	}
}

func TestCompactKbuildSelectionGraphTraversesLexicalParentPrerequisites(t *testing.T) {
	const (
		archive = "arch/x86/kvm/built-in.a"
		header  = "generated/kvm-config.h"
		object  = "virt/kvm/kvm_main.o"
		alias   = "arch/x86/kvm/../../../virt/kvm/kvm_main.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/kvm", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_ar = touch $@
cmd_cc = touch $@
cmd_gen = touch $@
$(obj)/built-in.a: $(obj)/../../../virt/kvm/kvm_main.o FORCE
	$(call if_changed,ar)
$(obj)/%.o: $(obj)/%.c generated/kvm-config.h FORCE
	$(call if_changed,cc)
generated/kvm-config.h: FORCE
	$(call if_changed,gen)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "virt/kvm/kvm_main.c")
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: archive, MakeTarget: archive, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: header, MakeTarget: header, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := graph.compactKbuildRuleForProfile(metadata, profile, object); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("canonical object %q unexpectedly matched the lexical $(obj)/%%.o rule", object)
	}
	match, found, err := graph.compactKbuildRuleForProfileMakeTarget(metadata, profile, object, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !found || match.lookupTarget != alias {
		t.Fatalf("lexical object resolution = %#v, found %t; want lookup target %q", match, found, alias)
	}

	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(ordered))
	for index, selection := range ordered {
		got[index] = selection.Target
	}
	if want := []string{header, archive}; !slices.Equal(got, want) {
		t.Fatalf("lexical parent-traversal materialization order = %q, want %q", got, want)
	}
}

func TestCompactKbuildSelectionGraphTraversesLexicalSelectedRoot(t *testing.T) {
	const (
		header = "zz-generated.h"
		object = "aa/unit.o"
		alias  = "arch/x86/kvm/../../../aa/unit.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/kvm", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_cc = touch $@
cmd_gen = touch $@
$(obj)/%.o: $(obj)/%.c zz-generated.h FORCE
	$(call if_changed,cc)
zz-generated.h: FORCE
	$(call if_changed,gen)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "aa/unit.c")
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: object, MakeTarget: alias, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: header, MakeTarget: header, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	objectKey := compactKbuildSelectionKey{profile: profile.Name, target: object, stage: "target"}
	dependencies, err := graph.selectionNativeDependencies(metadata, objectKey)
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := compactKbuildSelectionKey{profile: profile.Name, target: header, stage: "target"}
	if got, want := dependencies, []compactKbuildSelectionKey{wantHeader}; !slices.Equal(got, want) {
		t.Fatalf("lexical selected-root dependencies = %v, want %v", got, want)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(ordered))
	for index, selection := range ordered {
		got[index] = selection.Target
	}
	if want := []string{header, object}; !slices.Equal(got, want) {
		t.Fatalf("lexical selected-root materialization order = %q, want %q", got, want)
	}
}

func TestPrepareGroupedSelectionsUsesLexicalSelectedRoots(t *testing.T) {
	const (
		first       = "virt/kvm/unit.first"
		second      = "virt/kvm/unit.second"
		firstAlias  = "arch/x86/kvm/../../../virt/kvm/unit.first"
		secondAlias = "arch/x86/kvm/../../../virt/kvm/unit.second"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/kvm", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_group = touch $@
$(obj)/%.first $(obj)/%.second &: FORCE
	$(call if_changed,group)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: first, MakeTarget: firstAlias, GroupedTrigger: first, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: second, MakeTarget: secondAlias, GroupedTrigger: first, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.prepareGroupedSelections(metadata, config); err != nil {
		t.Fatal(err)
	}
	firstKey := compactKbuildSelectionKey{profile: profile.Name, target: first, stage: "target"}
	secondKey := compactKbuildSelectionKey{profile: profile.Name, target: second, stage: "target"}
	if !graph.compactKbuildSelectionsShareProducer(firstKey, secondKey) {
		t.Fatalf("lexical grouped selections %s and %s do not share a producer", first, second)
	}
	if got := graph.compactKbuildGroupedSelectionRepresentative(secondKey); got != firstKey {
		t.Fatalf("lexical grouped representative = %v, want %v", got, firstKey)
	}
}
