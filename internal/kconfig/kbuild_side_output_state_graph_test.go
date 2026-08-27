package kconfig

import (
	"slices"
	"testing"
)

func TestKbuildSideOutputStateGraphUsesNearestCandidatePredecessors(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:state", "scripts/Makefile.build", "", `
cmd_emit = touch $@
a: FORCE
	$(call if_changed,emit)
b: a FORCE
	$(call if_changed,emit)
c: b FORCE
	$(call if_changed,emit)
`, nil)
	selection := func(target string) CompactKbuildSelection {
		return CompactKbuildSelection{Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"}
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			selection("a"), selection("b"), selection("c"),
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	key := func(target string) compactKbuildSelectionKey {
		return compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	}
	stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, []compactKbuildSelectionKey{
		key("a"), key("b"), key("c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := stateGraph.parents[key("a")]; len(got) != 0 {
		t.Fatalf("a parents = %v, want none", got)
	}
	if got, want := stateGraph.parents[key("b")], []compactKbuildSelectionKey{key("a")}; !slices.Equal(got, want) {
		t.Fatalf("b parents = %v, want %v", got, want)
	}
	if got, want := stateGraph.parents[key("c")], []compactKbuildSelectionKey{key("b")}; !slices.Equal(got, want) {
		t.Fatalf("c parents = %v, want nearest predecessor only %v", got, want)
	}
	if got, want := stateGraph.maximal, []compactKbuildSelectionKey{key("c")}; !slices.Equal(got, want) {
		t.Fatalf("maximal candidates = %v, want %v", got, want)
	}
	sparse, err := graph.compactKbuildSideOutputStateGraph(metadata, []compactKbuildSelectionKey{key("a"), key("c")})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sparse.parents[key("c")], []compactKbuildSelectionKey{key("a")}; !slices.Equal(got, want) {
		t.Fatalf("transparent noncandidate projection = %v, want %v", got, want)
	}
}

func TestKbuildSideOutputStateGraphPreservesForkJoinFrontier(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:state", "scripts/Makefile.build", "", `
cmd_emit = touch $@
a: FORCE
	$(call if_changed,emit)
b: a FORCE
	$(call if_changed,emit)
c: a FORCE
	$(call if_changed,emit)
d: b c FORCE
	$(call if_changed,emit)
`, nil)
	selection := func(target string) CompactKbuildSelection {
		return CompactKbuildSelection{Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"}
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			selection("a"), selection("b"), selection("c"), selection("d"),
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	key := func(target string) compactKbuildSelectionKey {
		return compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	}
	stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, []compactKbuildSelectionKey{
		key("d"), key("b"), key("a"), key("c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stateGraph.parents[key("d")], []compactKbuildSelectionKey{key("b"), key("c")}; !slices.Equal(got, want) {
		t.Fatalf("join parents = %v, want unordered frontier %v", got, want)
	}
	if got, want := stateGraph.maximal, []compactKbuildSelectionKey{key("d")}; !slices.Equal(got, want) {
		t.Fatalf("maximal candidates = %v, want %v", got, want)
	}
}
