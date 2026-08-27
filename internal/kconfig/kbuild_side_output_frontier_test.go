package kconfig

import "testing"

func TestCompactKbuildSideOutputCandidateFrontierKeepsSingletonInline(t *testing.T) {
	first := compactKbuildSelectionKey{profile: "build", target: "first.o", stage: "target"}
	second := compactKbuildSelectionKey{profile: "build", target: "second.o", stage: "target"}
	third := compactKbuildSelectionKey{profile: "build", target: "third.o", stage: "target"}

	frontier := compactKbuildSingletonSideOutputCandidateFrontier(first)
	frontier.merge(compactKbuildSingletonSideOutputCandidateFrontier(first))
	if !frontier.hasSingle || frontier.single != first || frontier.multiple != nil {
		t.Fatalf("singleton frontier = %#v, want inline %v", frontier, first)
	}

	frontier.add(second)
	if frontier.hasSingle || len(frontier.multiple) != 2 || !frontier.multiple[first] || !frontier.multiple[second] {
		t.Fatalf("joined frontier = %#v, want {%v, %v}", frontier, first, second)
	}

	copy := compactKbuildSideOutputCandidateFrontier{}
	copy.merge(frontier)
	copy.add(third)
	if len(frontier.multiple) != 2 || frontier.multiple[third] {
		t.Fatalf("merging mutated memoized frontier: %#v", frontier)
	}
}
