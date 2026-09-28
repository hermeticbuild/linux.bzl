package kconfig

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func inputSetOrderTargets() []ActionPlanInputSetTarget {
	paths := []string{"a", "aa", "a/b", "a b", "a-b", "a.b", "a_", "b", "z", "é", "日本語", strings.Repeat("directory/", 12) + "file.o"}
	var targets []ActionPlanInputSetTarget
	for _, pathname := range paths {
		for _, kind := range []ActionPlanInputSetTargetKind{ActionPlanInputSetWorkTarget, ActionPlanInputSetAmbientTarget} {
			targets = append(targets, ActionPlanInputSetTarget{Kind: kind, Path: pathname})
		}
		for tree := range LinuxKernelPlanTrees {
			targets = append(targets, ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: tree, Path: pathname})
		}
		// Canonical target validation accepts arbitrary plan-name trees, not
		// only the execution schema's current tree set. Exercise prefixes.
		for _, tree := range []string{"a", "a-", "aa"} {
			targets = append(targets, ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: tree, Path: pathname})
		}
	}
	return targets
}

func TestInputSetAllocationFreeOrderMatchesCanonicalKeys(t *testing.T) {
	targets := inputSetOrderTargets()
	for _, left := range targets {
		for _, right := range targets {
			want := actionPlanInputSetTargetKey(left) < actionPlanInputSetTargetKey(right)
			if got := actionPlanInputSetTargetLess(left, right); got != want {
				t.Fatalf("ordering changed for %+v / %+v: got %t want %t", left, right, got, want)
			}
		}
	}
	if allocations := testing.AllocsPerRun(100, func() {
		for _, left := range targets {
			for _, right := range targets {
				inputSetOrderingSink = actionPlanInputSetTargetLess(left, right)
			}
		}
	}); allocations != 0 {
		t.Fatalf("comparison allocated %g objects", allocations)
	}
}

var inputSetOrderingSink bool

func TestInputSetWalkRetainsExactOrderAndCallbackFailure(t *testing.T) {
	targets := inputSetOrderTargets()
	random := rand.New(rand.NewSource(20260909))
	for iteration := 0; iteration < 8; iteration++ {
		random.Shuffle(len(targets), func(a, b int) { targets[a], targets[b] = targets[b], targets[a] })
		store := NewActionPlanInputSetStore()
		var root string
		for _, target := range targets {
			var err error
			root, err = store.Insert(root, ActionPlanInputSetEntry{Target: target, SourceID: "src-00000001"})
			if err != nil {
				t.Fatal(err)
			}
		}
		want, err := store.collect(root)
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(want, func(a, b int) bool {
			return actionPlanInputSetTargetKey(want[a].Target) < actionPlanInputSetTargetKey(want[b].Target)
		})
		var got []ActionPlanInputSetEntry
		if err := store.Walk(root, func(entry ActionPlanInputSetEntry) error { got = append(got, entry); return nil }); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("walk changed exact entry order or provenance")
		}
		stop := fmt.Errorf("stop callback")
		got = nil
		if err := store.Walk(root, func(entry ActionPlanInputSetEntry) error {
			got = append(got, entry)
			if len(got) == len(want)/2 {
				return stop
			}
			return nil
		}); err != stop || !reflect.DeepEqual(got, want[:len(want)/2]) {
			t.Fatal("walk changed callback error or visited prefix")
		}
	}
}

func BenchmarkInputSetWalkCanonicalOrdering(b *testing.B) {
	store := NewActionPlanInputSetStore()
	var root string
	for index := 0; index < 8192; index++ {
		var err error
		root, err = store.Insert(root, ActionPlanInputSetEntry{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("tools/generated/deep/directory/%08d/file.o", index)}, SourceID: "src-00000001"})
		if err != nil {
			b.Fatal(err)
		}
	}
	for _, legacy := range []bool{true, false} {
		b.Run(fmt.Sprintf("legacy=%t", legacy), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				count := 0
				if legacy {
					entries, err := store.collect(root)
					if err != nil {
						b.Fatal(err)
					}
					sort.Slice(entries, func(a, c int) bool {
						return actionPlanInputSetTargetKey(entries[a].Target) < actionPlanInputSetTargetKey(entries[c].Target)
					})
					for range entries {
						count++
					}
				} else if err := store.Walk(root, func(ActionPlanInputSetEntry) error { count++; return nil }); err != nil {
					b.Fatal(err)
				}
				if count != 8192 {
					b.Fatal("walk lost entries")
				}
			}
		})
	}
}
