package kconfig

import (
	"fmt"
	"slices"
	"strings"
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

func TestKbuildSideOutputStateGraphRemovesOnlyTransitiveSamePathParents(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:state", "scripts/Makefile.build", "", `
cmd_emit = touch $@
a: FORCE
	$(call if_changed,emit)
b: a FORCE
	$(call if_changed,emit)
c: a FORCE
	$(call if_changed,emit)
transparent: a FORCE
	$(call if_changed,emit)
d: transparent a b c FORCE
	$(call if_changed,emit)
e: a b c d FORCE
	$(call if_changed,emit)
`, nil)
	targets := []string{"a", "b", "c", "transparent", "d", "e"}
	config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
	key := func(target string) compactKbuildSelectionKey {
		return compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	}
	for _, target := range targets {
		config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
			Profile: profile.Name, Target: target, MakeTarget: target,
			Lifecycle: "target", Scope: "target", Stage: "target",
		})
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []compactKbuildSelectionKey{key("e"), key("d"), key("c"), key("b"), key("a")}
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(candidates)
		}
		stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, candidates)
		if err != nil {
			t.Fatal(err)
		}
		for target, want := range map[string][]compactKbuildSelectionKey{
			"a": nil,
			"b": {key("a")},
			"c": {key("a")},
			"d": {key("b"), key("c")},
			"e": {key("d")},
		} {
			if got := stateGraph.parents[key(target)]; !slices.Equal(got, want) {
				t.Fatalf("reverse=%v %s parents = %v, want maximal same-path frontier %v", reverse, target, got, want)
			}
		}
		if got, want := stateGraph.maximal, []compactKbuildSelectionKey{key("e")}; !slices.Equal(got, want) {
			t.Fatalf("maximal = %v, want %v", got, want)
		}
	}
}

func TestKbuildSideOutputStateGraphDenseShortcutsKeepLinearStateEdges(t *testing.T) {
	const count = 64
	var makefile strings.Builder
	makefile.WriteString("cmd_emit = touch $@\n")
	// Reverse lexical order exercises actual topological order, not key order.
	target := func(index int) string { return fmt.Sprintf("node%03d", count-index) }
	for index := range count {
		fmt.Fprintf(&makefile, "%s: FORCE", target(index))
		for predecessor := range index {
			fmt.Fprintf(&makefile, " %s", target(predecessor))
		}
		makefile.WriteString("\n\t$(call if_changed,emit)\n")
	}
	profile := mustCompactKbuildProfileForTest(t, "build:state", "scripts/Makefile.build", "", makefile.String(), nil)
	config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
	candidates := make([]compactKbuildSelectionKey, 0, count)
	for index := range count {
		config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
			Profile: profile.Name, Target: target(index), MakeTarget: target(index),
			Lifecycle: "target", Scope: "target", Stage: "target",
		})
		candidates = append(candidates, compactKbuildSelectionKey{
			profile: profile.Name, target: target(index), stage: "target",
		})
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, candidates)
	if err != nil {
		t.Fatal(err)
	}
	for index, candidate := range candidates {
		var want []compactKbuildSelectionKey
		if index != 0 {
			want = candidates[index-1 : index]
		}
		if got := stateGraph.parents[candidate]; !slices.Equal(got, want) {
			t.Fatalf("candidate %d parents = %v, want %v", index, got, want)
		}
		// State reduction must never remove source-declared command dependencies.
		dependencies, err := graph.selectionDependencies(metadata, candidate)
		if err != nil {
			t.Fatal(err)
		}
		for _, predecessor := range candidates[:index] {
			if !slices.Contains(dependencies, predecessor) {
				t.Fatalf("candidate %d lost command prerequisite %v", index, predecessor)
			}
		}
	}
}
