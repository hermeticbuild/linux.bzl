package kconfig

import (
	"slices"
	"sort"
	"testing"
)

var compactKbuildSelectionKeyLessSink bool

func TestCompactKbuildSelectionKeyLessUsesDirectFieldOrder(t *testing.T) {
	keys := []compactKbuildSelectionKey{
		{profile: "b", target: "a", stage: "host"},
		{profile: "a, b", target: "c", stage: "target"},
		{profile: "a", target: "b, c", stage: "target"},
		{profile: "a", target: "a", stage: "target"},
		{profile: "a", target: "a", stage: "host"},
	}
	sort.Slice(keys, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(keys[i], keys[j])
	})
	want := []compactKbuildSelectionKey{
		{profile: "a", target: "a", stage: "host"},
		{profile: "a", target: "a", stage: "target"},
		{profile: "a", target: "b, c", stage: "target"},
		{profile: "a, b", target: "c", stage: "target"},
		{profile: "b", target: "a", stage: "host"},
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("selection-key order = %#v, want %#v", keys, want)
	}
	if compactKbuildSelectionKeyLess(keys[0], keys[0]) {
		t.Fatal("equal selection key sorts before itself")
	}
}

func TestCompactKbuildSelectionKeyLessDoesNotAllocate(t *testing.T) {
	left := compactKbuildSelectionKey{profile: "build:arm", target: "arch/arm/kernel/head.o", stage: "target"}
	right := compactKbuildSelectionKey{profile: "build:arm", target: "arch/arm/kernel/setup.o", stage: "target"}
	allocations := testing.AllocsPerRun(1000, func() {
		compactKbuildSelectionKeyLessSink = compactKbuildSelectionKeyLess(left, right)
	})
	if allocations != 0 {
		t.Fatalf("selection-key comparison allocations = %v, want 0", allocations)
	}
}

func BenchmarkCompactKbuildSelectionKeyLess(b *testing.B) {
	left := compactKbuildSelectionKey{profile: "build:arm", target: "arch/arm/kernel/head.o", stage: "target"}
	right := compactKbuildSelectionKey{profile: "build:arm", target: "arch/arm/kernel/setup.o", stage: "target"}
	b.ReportAllocs()
	for range b.N {
		compactKbuildSelectionKeyLessSink = compactKbuildSelectionKeyLess(left, right)
	}
}
