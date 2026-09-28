package main

import (
	"fmt"
	"slices"
	"testing"
)

func TestKbuildPathSetEmptyContainsAndSortedRange(t *testing.T) {
	set := emptyKbuildPathSet()
	if got := kbuildPathSetLen(set); got != 0 {
		t.Fatalf("empty path-set length = %d, want 0", got)
	}
	if kbuildPathSetContains(set, "missing") {
		t.Fatal("empty path set contains a path")
	}
	called := false
	kbuildPathSetRange(set, func(string) bool {
		called = true
		return true
	})
	if called {
		t.Fatal("empty path-set range called its visitor")
	}

	for _, path := range []string{"z/last", "a/first", "m/middle", "a/second"} {
		set = kbuildPathSetAdd(set, path)
	}
	if got, want := kbuildPathSetLen(set), 4; got != want {
		t.Fatalf("path-set length = %d, want %d", got, want)
	}
	for _, path := range []string{"a/first", "a/second", "m/middle", "z/last"} {
		if !kbuildPathSetContains(set, path) {
			t.Errorf("path set does not contain %q", path)
		}
	}
	if kbuildPathSetContains(set, "m/missing") {
		t.Fatal("path set unexpectedly contains missing path")
	}

	var got []string
	kbuildPathSetRange(set, func(path string) bool {
		got = append(got, path)
		return true
	})
	want := []string{"a/first", "a/second", "m/middle", "z/last"}
	if !slices.Equal(got, want) {
		t.Fatalf("sorted range = %q, want %q", got, want)
	}

	got = nil
	kbuildPathSetRange(set, func(path string) bool {
		got = append(got, path)
		return len(got) < 2
	})
	if want := []string{"a/first", "a/second"}; !slices.Equal(got, want) {
		t.Fatalf("early-stop range = %q, want %q", got, want)
	}
	assertKbuildPathSetTreap(t, set.root, "", "")
}

func TestKbuildPathSetAddIsPersistentSharedAndIdempotent(t *testing.T) {
	base := emptyKbuildPathSet()
	for index := 0; index < 64; index++ {
		base = kbuildPathSetAdd(base, fmt.Sprintf("generated/%03d", index))
	}
	baseNodes := collectKbuildPathSetNodes(base.root, map[*kbuildPathSetNode]struct{}{})

	updated := kbuildPathSetAdd(base, "generated/999")
	if got, want := kbuildPathSetLen(base), 64; got != want {
		t.Fatalf("base length after descendant add = %d, want %d", got, want)
	}
	if kbuildPathSetContains(base, "generated/999") {
		t.Fatal("base snapshot observed descendant addition")
	}
	if got, want := kbuildPathSetLen(updated), 65; got != want {
		t.Fatalf("updated length = %d, want %d", got, want)
	}
	if !kbuildPathSetContains(updated, "generated/999") {
		t.Fatal("updated snapshot is missing added path")
	}

	shared := 0
	for node := range collectKbuildPathSetNodes(updated.root, map[*kbuildPathSetNode]struct{}{}) {
		if _, ok := baseNodes[node]; ok {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("persistent add did not share any untouched subtree")
	}

	again := kbuildPathSetAdd(updated, "generated/999")
	if again.root != updated.root {
		t.Fatal("idempotent add changed the immutable set root")
	}
	assertKbuildPathSetTreap(t, base.root, "", "")
	assertKbuildPathSetTreap(t, updated.root, "", "")
}

func TestKbuildPathSetInsertionOrderHasCanonicalShape(t *testing.T) {
	const count = 256
	ascending := emptyKbuildPathSet()
	reverse := emptyKbuildPathSet()
	for index := 0; index < count; index++ {
		ascending = kbuildPathSetAdd(ascending, fmt.Sprintf("path/%03d", index))
	}
	for index := count - 1; index >= 0; index-- {
		reverse = kbuildPathSetAdd(reverse, fmt.Sprintf("path/%03d", index))
	}
	if !equalKbuildPathSetTrees(ascending.root, reverse.root) {
		t.Fatal("equal path sets have different deterministic treap shapes")
	}
}

func TestKbuildPathSetUnionIteratesSmallerSnapshot(t *testing.T) {
	large := emptyKbuildPathSet()
	for _, path := range []string{"a", "b", "c", "d", "e"} {
		large = kbuildPathSetAdd(large, path)
	}
	subset := emptyKbuildPathSet()
	for _, path := range []string{"b", "d"} {
		subset = kbuildPathSetAdd(subset, path)
	}

	// Even when the smaller set is the first argument, union starts from the
	// larger root. Since every insertion is idempotent, the root stays shared.
	if got := kbuildPathSetUnion(subset, large); got.root != large.root {
		t.Fatal("union did not preserve the larger superset root")
	}
	if got := kbuildPathSetUnion(large, subset); got.root != large.root {
		t.Fatal("reverse union did not preserve the larger superset root")
	}
	if got := kbuildPathSetUnion(large, emptyKbuildPathSet()); got.root != large.root {
		t.Fatal("union with empty set did not preserve the non-empty root")
	}
	if got := kbuildPathSetUnion(large, large); got.root != large.root {
		t.Fatal("self-union did not preserve the set root")
	}

	extra := emptyKbuildPathSet()
	for _, path := range []string{"c", "f"} {
		extra = kbuildPathSetAdd(extra, path)
	}
	union := kbuildPathSetUnion(large, extra)
	var paths []string
	kbuildPathSetRange(union, func(path string) bool {
		paths = append(paths, path)
		return true
	})
	if want := []string{"a", "b", "c", "d", "e", "f"}; !slices.Equal(paths, want) {
		t.Fatalf("union paths = %q, want %q", paths, want)
	}
	if got, want := kbuildPathSetLen(union), 6; got != want {
		t.Fatalf("union length = %d, want %d", got, want)
	}
	assertKbuildPathSetTreap(t, union.root, "", "")
}

func collectKbuildPathSetNodes(node *kbuildPathSetNode, nodes map[*kbuildPathSetNode]struct{}) map[*kbuildPathSetNode]struct{} {
	if node == nil {
		return nodes
	}
	nodes[node] = struct{}{}
	collectKbuildPathSetNodes(node.left, nodes)
	collectKbuildPathSetNodes(node.right, nodes)
	return nodes
}

func equalKbuildPathSetTrees(left, right *kbuildPathSetNode) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.path == right.path &&
		equalKbuildPathSetTrees(left.left, right.left) &&
		equalKbuildPathSetTrees(left.right, right.right)
}

func assertKbuildPathSetTreap(t *testing.T, node *kbuildPathSetNode, lower, upper string) int {
	t.Helper()
	if node == nil {
		return 0
	}
	if lower != "" && node.path <= lower {
		t.Fatalf("path-set node %q is not greater than lower bound %q", node.path, lower)
	}
	if upper != "" && node.path >= upper {
		t.Fatalf("path-set node %q is not less than upper bound %q", node.path, upper)
	}
	leftSize := assertKbuildPathSetTreap(t, node.left, lower, node.path)
	rightSize := assertKbuildPathSetTreap(t, node.right, node.path, upper)
	if kbuildPathSetPriorityLess(node.left, node) {
		t.Fatalf("path-set left child %q has priority before parent %q", node.left.path, node.path)
	}
	if kbuildPathSetPriorityLess(node.right, node) {
		t.Fatalf("path-set right child %q has priority before parent %q", node.right.path, node.path)
	}
	wantSize := leftSize + rightSize + 1
	if node.size != wantSize {
		t.Fatalf("path-set node %q size = %d, want %d", node.path, node.size, wantSize)
	}
	return wantSize
}
