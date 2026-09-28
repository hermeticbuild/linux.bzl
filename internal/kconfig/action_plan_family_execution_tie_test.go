package kconfig

import (
	"maps"
	"slices"
	"testing"
)

func TestExecutionComplementTieDoesNotSwapUsefulConsumers(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	// Retain just compiler A as the early group's benefit. The late header
	// requires A's output and feeds distinct compiler B. Both groups now rank
	// equally, with the early key first, so selecting the late root would trade
	// the existing unpinned A for B without improving benefit.
	key := slices.Sorted(maps.Keys(groups[0].consumers))[0]
	earlyCompiler := groups[0].consumers[key]
	groups[0].consumers = map[string]string{key: earlyCompiler}
	if len(groups[1].consumers) != 1 {
		t.Fatal("fixture must have one distinct late consumer")
	}
	for _, lateCompiler := range groups[1].consumers {
		if lateCompiler == earlyCompiler {
			t.Fatal("fixture must exchange distinct consumers, not complement one")
		}
	}
	want := slices.Collect(maps.Keys(groups[0].roots))
	if len(want) != 1 {
		t.Fatal("fixture must have one early root")
	}
	var seal string
	for _, order := range [][]*actionPlanFamilyHeaderDemandGroup{groups, {groups[1], groups[0]}} {
		cut, err := selectActionPlanFamilyHeaderExecution(family, order, defaultActionPlanFamilyExecutionCutLimits(), 128)
		if err != nil {
			t.Fatal(err)
		}
		if len(cut.NodeIDs()) != 1 || !slices.Equal(cut.Roots(), want) {
			t.Fatalf("equal-benefit trial swapped useful compilers: roots=%v nodes=%d, want only early root", cut.Roots(), len(cut.NodeIDs()))
		}
		if slices.Contains(cut.NodeIDs(), earlyCompiler) {
			t.Fatal("complementary selection pinned the previously useful compiler")
		}
		if _, err := cut.Verify(family); err != nil {
			t.Fatal(err)
		}
		if seal != "" && cut.ID() != seal {
			t.Fatal("group iteration order changed equal-benefit selection")
		}
		seal = cut.ID()
	}
}
