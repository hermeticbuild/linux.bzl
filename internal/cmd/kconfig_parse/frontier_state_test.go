package main

import (
	"fmt"
	"math/bits"
	"slices"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestKbuildFrontierStateBulkBuildAndOrderedRange(t *testing.T) {
	entries := make([]kbuildFrontierEntry, 1023)
	for index := range entries {
		path := fmt.Sprintf("generated/%04d", index)
		entries[index] = kbuildFrontierEntry{
			path: path,
			value: kbuildFrontierValue{
				artifact: kconfig.CompactKbuildVisibleArtifact{
					Path: path, Profile: "root", Target: path,
				},
				content: path + "\n",
				exact:   index%2 == 0,
			},
		}
	}

	state := newKbuildFrontierStateFromSorted(entries)
	if got, want := kbuildFrontierLen(state), len(entries); got != want {
		t.Fatalf("frontier length = %d, want %d", got, want)
	}
	for _, entry := range entries {
		got, ok := kbuildFrontierGet(state, entry.path)
		if !ok || got != entry.value {
			t.Fatalf("frontier[%q] = (%#v, %t), want (%#v, true)", entry.path, got, ok, entry.value)
		}
	}
	if _, ok := kbuildFrontierGet(state, "generated/missing"); ok {
		t.Fatal("missing frontier path unexpectedly resolved")
	}

	paths := []string{}
	kbuildFrontierRange(state, func(path string, _ kbuildFrontierValue) bool {
		paths = append(paths, path)
		return true
	})
	wantPaths := make([]string, len(entries))
	for index, entry := range entries {
		wantPaths[index] = entry.path
	}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("frontier range is not sorted: got %q, want %q", paths, wantPaths)
	}
	assertKbuildFrontierTreap(t, state.root, "", "")
}

func TestKbuildFrontierStateSetPreservesSnapshotsAndSharesUntouchedSubtrees(t *testing.T) {
	entries := make([]kbuildFrontierEntry, 7)
	for index := range entries {
		path := fmt.Sprintf("%d", index)
		entries[index] = kbuildFrontierEntry{
			path: path,
			value: kbuildFrontierValue{artifact: kconfig.CompactKbuildVisibleArtifact{
				Path: path, Profile: "initial", Target: path,
			}},
		}
	}
	base := newKbuildFrontierStateFromSorted(entries)
	leftOrigin := &kbuildRecursiveMakeFrontier{id: "left-write"}
	leftValue := kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{
			Path: "0", Profile: "left", Target: "left-output",
		},
		content: "left\n",
		exact:   true,
		origin:  leftOrigin,
	}
	left := kbuildFrontierSet(base, "0", leftValue)
	baseNodes := map[*kbuildFrontierNode]struct{}{}
	collectKbuildFrontierNodes(base.root, baseNodes)
	shared := 0
	for node := range collectKbuildFrontierNodes(left.root, map[*kbuildFrontierNode]struct{}{}) {
		if _, ok := baseNodes[node]; ok {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("path copy did not share any untouched subtree")
	}
	if got, _ := kbuildFrontierGet(base, "0"); got == leftValue || got.artifact.Profile != "initial" {
		t.Fatalf("updating child snapshot changed base value: %#v", got)
	}
	if got, ok := kbuildFrontierGet(left, "0"); !ok || got != leftValue {
		t.Fatalf("updated snapshot value = (%#v, %t), want (%#v, true)", got, ok, leftValue)
	}

	newValue := kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{
			Path: "7", Profile: "later", Target: "new-output",
		},
		exact:  false,
		origin: &kbuildRecursiveMakeFrontier{id: "later-write"},
	}
	later := kbuildFrontierSet(left, "7", newValue)
	if got, want := kbuildFrontierLen(base), 7; got != want {
		t.Fatalf("base length after descendant insert = %d, want %d", got, want)
	}
	if got, want := kbuildFrontierLen(left), 7; got != want {
		t.Fatalf("intermediate length after descendant insert = %d, want %d", got, want)
	}
	if got, want := kbuildFrontierLen(later), 8; got != want {
		t.Fatalf("descendant length = %d, want %d", got, want)
	}
	if _, ok := kbuildFrontierGet(base, "7"); ok {
		t.Fatal("base snapshot observed descendant insertion")
	}
	if _, ok := kbuildFrontierGet(left, "7"); ok {
		t.Fatal("intermediate snapshot observed descendant insertion")
	}
	if got, ok := kbuildFrontierGet(later, "7"); !ok || got != newValue {
		t.Fatalf("descendant inserted value = (%#v, %t), want (%#v, true)", got, ok, newValue)
	}

	assertKbuildFrontierTreap(t, base.root, "", "")
	assertKbuildFrontierTreap(t, left.root, "", "")
	assertKbuildFrontierTreap(t, later.root, "", "")
}

func TestKbuildFrontierStateInsertionOrderHasCanonicalIdentity(t *testing.T) {
	entries := make([]kbuildFrontierEntry, 1024)
	for index := range entries {
		path := fmt.Sprintf("artifact/%04d", index)
		entries[index] = kbuildFrontierEntry{
			path: path,
			value: kbuildFrontierValue{
				artifact: kconfig.CompactKbuildVisibleArtifact{Path: path, Target: path},
				content:  path + "\n",
				exact:    index%3 != 0,
			},
		}
	}
	ascending := newKbuildFrontierStateFromSorted(entries)
	reverse := kbuildFrontierState{}
	for index := len(entries) - 1; index >= 0; index-- {
		reverse = kbuildFrontierSet(reverse, entries[index].path, entries[index].value)
	}
	if got, want := kbuildFrontierDigest(reverse), kbuildFrontierDigest(ascending); got != want {
		t.Fatalf("equivalent frontiers have different digests: got %x, want %x", got, want)
	}
	if !equalKbuildFrontierTrees(ascending.root, reverse.root) {
		t.Fatal("equivalent frontiers have different canonical treap shapes")
	}
	changed := kbuildFrontierSet(reverse, entries[0].path, kbuildFrontierValue{
		artifact: entries[0].value.artifact,
		content:  "changed\n",
		exact:    true,
	})
	if kbuildFrontierDigest(ascending) == kbuildFrontierDigest(changed) {
		t.Fatal("frontiers with different exact bytes have equal digests")
	}
}

func TestKbuildFrontierStateAscendingInsertsRemainShallow(t *testing.T) {
	const count = 4096
	state := kbuildFrontierState{}
	for index := 0; index < count; index++ {
		path := fmt.Sprintf("artifact/%04d", index)
		state = kbuildFrontierSet(state, path, kbuildFrontierValue{
			artifact: kconfig.CompactKbuildVisibleArtifact{Path: path, Target: path},
		})
	}

	if got := kbuildFrontierLen(state); got != count {
		t.Fatalf("ascending-insert frontier length = %d, want %d", got, count)
	}
	assertKbuildFrontierTreap(t, state.root, "", "")
	if got, limit := state.root.height, 8*bits.Len(uint(count+1)); got > limit {
		t.Fatalf("ascending-insert frontier height = %d, want <= %d", got, limit)
	}

	visited := []string{}
	kbuildFrontierRange(state, func(path string, _ kbuildFrontierValue) bool {
		visited = append(visited, path)
		return len(visited) < 3
	})
	if got, want := visited, []string{"artifact/0000", "artifact/0001", "artifact/0002"}; !slices.Equal(got, want) {
		t.Fatalf("early-stop range visited %q, want %q", got, want)
	}
}

func TestKbuildFrontierStateEmpty(t *testing.T) {
	state := newKbuildFrontierStateFromSorted(nil)
	if got := kbuildFrontierLen(state); got != 0 {
		t.Fatalf("empty frontier length = %d, want 0", got)
	}
	if _, ok := kbuildFrontierGet(state, "anything"); ok {
		t.Fatal("empty frontier resolved a path")
	}
	called := false
	kbuildFrontierRange(state, func(string, kbuildFrontierValue) bool {
		called = true
		return true
	})
	if called {
		t.Fatal("empty frontier range called visitor")
	}
}

func TestKbuildFrontierStatePrefixRange(t *testing.T) {
	paths := []string{
		"arch/arm/Makefile",
		"arch/arm/boot/Image",
		"arch/arm64/Makefile",
		"drivers/base/core.o",
		"drivers/net/core.o",
	}
	entries := make([]kbuildFrontierEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, kbuildFrontierEntry{
			path: path,
			value: kbuildFrontierValue{artifact: kconfig.CompactKbuildVisibleArtifact{
				Path: path, Target: path,
			}},
		})
	}
	state := newKbuildFrontierStateFromSorted(entries)
	got := []string{}
	kbuildFrontierRangePrefix(state, "arch/arm/", func(path string, _ kbuildFrontierValue) bool {
		got = append(got, path)
		return true
	})
	if want := []string{"arch/arm/Makefile", "arch/arm/boot/Image"}; !slices.Equal(got, want) {
		t.Fatalf("prefix range = %q, want %q", got, want)
	}
	got = nil
	kbuildFrontierRangePrefix(state, "drivers/", func(path string, _ kbuildFrontierValue) bool {
		got = append(got, path)
		return false
	})
	if want := []string{"drivers/base/core.o"}; !slices.Equal(got, want) {
		t.Fatalf("early-stop prefix range = %q, want %q", got, want)
	}
}

func TestKbuildInvocationGeneratedTextProjectionResolvesFrontierOnDemand(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{Name: "drivers", Directory: "drivers"}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: "drivers",
	}); err != nil {
		t.Fatal(err)
	}
	entries := []kbuildFrontierEntry{}
	for _, path := range []string{"drivers/first.order", "drivers/second.order", "unrelated.order"} {
		entries = append(entries, kbuildFrontierEntry{
			path: path,
			value: kbuildFrontierValue{
				artifact: kconfig.CompactKbuildVisibleArtifact{Path: path, Target: path},
				content:  path + "\n",
				exact:    true,
			},
		})
	}
	state := newKbuildFrontierStateFromSorted(entries)
	got, exact, err := kbuildInvocationGeneratedTextProjectionFromFrontier(
		profile,
		"cat ${tree:prep}/drivers/first.order second.order first.order > ${tree:prep}/drivers/modules.order",
		"drivers/modules.order",
		state,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "drivers/first.order\ndrivers/second.order\ndrivers/first.order\n"
	if !exact || got != want {
		t.Fatalf("frontier projection = (%q, %t), want (%q, true)", got, exact, want)
	}

	got, exact, err = kbuildInvocationGeneratedTextProjectionFromFrontier(
		profile,
		"set -e; trap 'rm -f modules.order' HUP; { echo first.o; echo second.o; :; } > modules.order; printf '%s\\n' 'savedcmd_modules.order := generated' > ./.modules.order.cmd",
		"drivers/modules.order",
		state,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first.o\nsecond.o\n"; !exact || got != want {
		t.Fatalf("wrapped modules.order projection = (%q, %t), want (%q, true)", got, exact, want)
	}

	opaque := kbuildFrontierSet(state, "drivers/second.order", kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{Path: "drivers/second.order", Target: "drivers/second.order"},
	})
	if got, exact, err := kbuildInvocationGeneratedTextProjectionFromFrontier(
		profile,
		"cat second.order > "+kbuildEvalObjectTree+"/drivers/modules.order",
		"drivers/modules.order",
		opaque,
	); err != nil || exact {
		t.Fatalf("opaque frontier projection = (%q, %t, %v), want inexact", got, exact, err)
	}
}

func collectKbuildFrontierNodes(node *kbuildFrontierNode, nodes map[*kbuildFrontierNode]struct{}) map[*kbuildFrontierNode]struct{} {
	if node == nil {
		return nodes
	}
	nodes[node] = struct{}{}
	collectKbuildFrontierNodes(node.left, nodes)
	collectKbuildFrontierNodes(node.right, nodes)
	return nodes
}

func equalKbuildFrontierTrees(left, right *kbuildFrontierNode) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.path == right.path &&
		left.value.artifact == right.value.artifact &&
		left.value.content == right.value.content &&
		left.value.exact == right.value.exact &&
		equalKbuildFrontierTrees(left.left, right.left) &&
		equalKbuildFrontierTrees(left.right, right.right)
}

func assertKbuildFrontierTreap(t *testing.T, node *kbuildFrontierNode, lower, upper string) (height, size int) {
	t.Helper()
	if node == nil {
		return 0, 0
	}
	if lower != "" && node.path <= lower {
		t.Fatalf("frontier path %q is not greater than lower bound %q", node.path, lower)
	}
	if upper != "" && node.path >= upper {
		t.Fatalf("frontier path %q is not less than upper bound %q", node.path, upper)
	}
	leftHeight, leftSize := assertKbuildFrontierTreap(t, node.left, lower, node.path)
	rightHeight, rightSize := assertKbuildFrontierTreap(t, node.right, node.path, upper)
	if kbuildFrontierPriorityLess(node.left, node) {
		t.Fatalf("frontier left child %q has priority before parent %q", node.left.path, node.path)
	}
	if kbuildFrontierPriorityLess(node.right, node) {
		t.Fatalf("frontier right child %q has priority before parent %q", node.right.path, node.path)
	}
	wantHeight := max(leftHeight, rightHeight) + 1
	if node.height != wantHeight {
		t.Fatalf("frontier node %q height = %d, want %d", node.path, node.height, wantHeight)
	}
	wantSize := leftSize + rightSize + 1
	if node.size != wantSize {
		t.Fatalf("frontier node %q size = %d, want %d", node.path, node.size, wantSize)
	}
	return wantHeight, wantSize
}
