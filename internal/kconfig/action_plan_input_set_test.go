package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestActionPlanInputSetCanonicalPersistentOperations(t *testing.T) {
	entries := actionPlanInputSetTestEntries(96)
	forward := NewActionPlanInputSetStore()
	forwardRoot := actionPlanInputSetTestInsert(t, forward, "", entries)
	reverse := NewActionPlanInputSetStore()
	reversed := append([]ActionPlanInputSetEntry(nil), entries...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	reverseRoot := actionPlanInputSetTestInsert(t, reverse, "", reversed)
	if forwardRoot != reverseRoot {
		t.Fatalf("insertion order changed canonical root:\nforward %s\nreverse %s", forwardRoot, reverseRoot)
	}
	if err := forward.ValidateRoot(forwardRoot); err != nil {
		t.Fatalf("ValidateRoot(): %v", err)
	}

	var walked []string
	if err := forward.Walk(forwardRoot, func(entry ActionPlanInputSetEntry) error {
		walked = append(walked, actionPlanInputSetTargetKey(entry.Target))
		return nil
	}); err != nil {
		t.Fatalf("Walk(): %v", err)
	}
	if !sort.StringsAreSorted(walked) || len(walked) != len(entries) {
		t.Fatalf("Walk() returned %d non-lexical entries", len(walked))
	}

	oldEntry, found, err := forward.Lookup(forwardRoot, entries[17].Target)
	if err != nil || !found {
		t.Fatalf("Lookup(old root): found=%v err=%v", found, err)
	}
	replacement := oldEntry
	replacement.SourceID = ""
	replacement.ProducerID = actionPlanInputSetTestDigest("replacement")
	replacement.Slot = 3
	replacedRoot, err := forward.Replace(forwardRoot, replacement)
	if err != nil {
		t.Fatalf("Replace(): %v", err)
	}
	if replacedRoot == forwardRoot {
		t.Fatal("Replace() did not change root")
	}
	if stillOld, _, err := forward.Lookup(forwardRoot, oldEntry.Target); err != nil || stillOld != oldEntry {
		t.Fatalf("old root mutated: got %#v err=%v", stillOld, err)
	}
	if got, _, err := forward.Lookup(replacedRoot, replacement.Target); err != nil || got != replacement {
		t.Fatalf("replacement missing: got %#v err=%v", got, err)
	}
	missing := actionPlanInputSetTestEntry(999999)
	if _, err := forward.Replace(forwardRoot, missing); err == nil {
		t.Fatal("Replace() accepted a missing target")
	}
	unchanged, removed, err := forward.Delete(forwardRoot, missing.Target)
	if err != nil || removed || unchanged != forwardRoot {
		t.Fatalf("absent Delete() = (%q,%v,%v), want exact no-op", unchanged, removed, err)
	}
}

func TestActionPlanInputSetSplitCollapseAndLongPrefix(t *testing.T) {
	for _, test := range []struct {
		name      string
		keyDigest func(ActionPlanInputSetTarget) [sha256.Size]byte
	}{
		{name: "ordinary", keyDigest: actionPlanInputSetTargetDigest},
		{
			name: "long-common-prefix",
			keyDigest: func(target ActionPlanInputSetTarget) [sha256.Size]byte {
				var index int
				if _, err := fmt.Sscanf(target.Path, "obj/%08d.o", &index); err != nil {
					panic(err)
				}
				var digest [sha256.Size]byte
				digest[sha256.Size-1] = byte(index)
				return digest
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newActionPlanInputSetStoreWithDigests(actionPlanInputSetSHA256, test.keyDigest)
			entries := actionPlanInputSetTestEntries(actionPlanInputSetLeafCapacity + 1)
			leafRoot := actionPlanInputSetTestInsert(t, store, "", entries[:actionPlanInputSetLeafCapacity])
			leaf, _ := store.Node(leafRoot)
			if leaf.Kind != ActionPlanInputSetLeafNode {
				t.Fatalf("16-entry node kind = %q, want leaf", leaf.Kind)
			}
			branchRoot, err := store.Insert(leafRoot, entries[actionPlanInputSetLeafCapacity])
			if err != nil {
				t.Fatalf("17th Insert(): %v", err)
			}
			branch, _ := store.Node(branchRoot)
			if branch.Kind != ActionPlanInputSetBranchNode {
				t.Fatalf("17-entry node kind = %q, want branch", branch.Kind)
			}
			if err := store.ValidateRoot(branchRoot); err != nil {
				t.Fatalf("ValidateRoot(branch): %v", err)
			}
			collapsed, removed, err := store.Delete(branchRoot, entries[actionPlanInputSetLeafCapacity].Target)
			if err != nil || !removed {
				t.Fatalf("Delete(split entry): removed=%v err=%v", removed, err)
			}
			if collapsed != leafRoot {
				t.Fatalf("split/collapse did not recover canonical leaf: got %s want %s", collapsed, leafRoot)
			}
		})
	}
}

func TestActionPlanInputSetUnionFilterAndMap(t *testing.T) {
	store := NewActionPlanInputSetStore()
	leftEntries := actionPlanInputSetTestEntries(32)
	rightEntries := append([]ActionPlanInputSetEntry(nil), actionPlanInputSetTestEntries(48)[24:]...)
	rightEntries[4].SourceID = ""
	rightEntries[4].ProducerID = actionPlanInputSetTestDigest("right-conflict")
	rightEntries[4].Slot = 7
	left := actionPlanInputSetTestInsert(t, store, "", leftEntries)
	right := actionPlanInputSetTestInsert(t, store, "", rightEntries)
	if _, err := store.Union(left, right, nil); err == nil {
		t.Fatal("Union() accepted conflicting provenance without a resolver")
	}
	resolverCalled := false
	union, err := store.Union(left, right, func(target ActionPlanInputSetTarget, leftEntry, rightEntry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		resolverCalled = true
		if target != rightEntries[4].Target || leftEntry.Target != target || rightEntry != rightEntries[4] {
			t.Fatalf("resolver arguments lost left/right order: target=%#v left=%#v right=%#v", target, leftEntry, rightEntry)
		}
		return rightEntry, nil
	})
	if err != nil || !resolverCalled {
		t.Fatalf("Union(): resolverCalled=%v err=%v", resolverCalled, err)
	}
	if got := actionPlanInputSetTestCount(t, store, union); got != 48 {
		t.Fatalf("union count = %d, want 48", got)
	}
	if got, _, err := store.Lookup(union, rightEntries[4].Target); err != nil || got != rightEntries[4] {
		t.Fatalf("resolved union entry = %#v, err=%v", got, err)
	}
	if identical, err := store.Union(left, left, nil); err != nil || identical != left {
		t.Fatalf("identity Union() = %q, %v; want %q", identical, err, left)
	}
	if _, err := store.Union(left, right, func(_ ActionPlanInputSetTarget, _, rightEntry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		rightEntry.Target.Path = "moved/by/resolver.o"
		return rightEntry, nil
	}); err == nil {
		t.Fatal("Union() accepted a resolver that moved the target")
	}

	identityFilter, err := store.Filter(union, func(ActionPlanInputSetEntry) (bool, error) { return true, nil })
	if err != nil || identityFilter != union {
		t.Fatalf("identity Filter() = %q, %v; want %q", identityFilter, err, union)
	}
	filtered, err := store.Filter(union, func(entry ActionPlanInputSetEntry) (bool, error) {
		return entry.CompilerUse, nil
	})
	if err != nil {
		t.Fatalf("Filter(): %v", err)
	}
	if got := actionPlanInputSetTestCount(t, store, filtered); got != 24 {
		t.Fatalf("filtered count = %d, want 24", got)
	}
	empty, err := store.Filter(union, func(ActionPlanInputSetEntry) (bool, error) { return false, nil })
	if err != nil || empty != "" {
		t.Fatalf("rejecting Filter() = %q, %v; want empty root", empty, err)
	}

	identityMap, err := store.Map(union, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) { return entry, nil })
	if err != nil || identityMap != union {
		t.Fatalf("identity Map() = %q, %v; want %q", identityMap, err, union)
	}
	mappedTarget := leftEntries[3].Target
	mapped, err := store.Map(union, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if entry.Target == mappedTarget {
			entry.AuxiliaryUse = !entry.AuxiliaryUse
		}
		return entry, nil
	})
	if err != nil {
		t.Fatalf("target-stable Map(): %v", err)
	}
	if mapped == union {
		t.Fatal("target-stable Map() did not change root")
	}
	if shared := actionPlanInputSetTestSharedRootChildren(t, store, union, mapped); shared == 0 {
		t.Fatal("target-stable Map() did not retain any root child")
	}
	if _, err := store.Map(union, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if entry.Target == leftEntries[0].Target {
			entry.Target = leftEntries[1].Target
		}
		return entry, nil
	}); err == nil {
		t.Fatal("target-moving Map() accepted a duplicate result")
	}
}

func TestActionPlanInputSetCallbackErrors(t *testing.T) {
	store := NewActionPlanInputSetStore()
	root := actionPlanInputSetTestInsert(t, store, "", actionPlanInputSetTestEntries(24))
	sentinel := errors.New("stop")
	if _, err := store.Filter(root, func(ActionPlanInputSetEntry) (bool, error) { return false, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Filter() error = %v, want sentinel", err)
	}
	if _, err := store.Map(root, func(ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		return ActionPlanInputSetEntry{}, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Map() error = %v, want sentinel", err)
	}
	if err := store.Walk(root, func(ActionPlanInputSetEntry) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Walk() error = %v, want sentinel", err)
	}
	conflict := actionPlanInputSetTestEntry(1)
	conflict.SourceID = ""
	conflict.ProducerID = actionPlanInputSetTestDigest("conflict")
	right, err := store.Insert("", conflict)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Union(root, right, func(ActionPlanInputSetTarget, ActionPlanInputSetEntry, ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		return ActionPlanInputSetEntry{}, sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Union() resolver error = %v, want sentinel", err)
	}
}

func TestActionPlanInputSetTargetStableMapperSharesPersistentSubtries(t *testing.T) {
	store := NewActionPlanInputSetStore()
	baseEntries := actionPlanInputSetTestEntries(96)
	base := actionPlanInputSetTestInsert(t, store, "", baseEntries)
	left, err := store.Insert(base, actionPlanInputSetTestEntry(1000))
	if err != nil {
		t.Fatal(err)
	}
	right, err := store.Insert(base, actionPlanInputSetTestEntry(1001))
	if err != nil {
		t.Fatal(err)
	}

	callbackCount := 0
	mapper, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		callbackCount++
		entry.CompilerUse = true
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.Map(left); err != nil {
		t.Fatalf("Map(left): %v", err)
	}
	afterLeft := callbackCount
	if _, err := mapper.Map(right); err != nil {
		t.Fatalf("Map(right): %v", err)
	}
	if callbackCount-afterLeft >= len(baseEntries) {
		t.Fatalf("Map(right) invoked callback %d additional times; shared base has %d entries", callbackCount-afterLeft, len(baseEntries))
	}
	afterRight := callbackCount
	if _, err := mapper.Map(left); err != nil {
		t.Fatalf("Map(left) again: %v", err)
	}
	if callbackCount != afterRight {
		t.Fatalf("memoized Map(left) invoked %d callbacks", callbackCount-afterRight)
	}
}

func TestActionPlanInputSetTargetStableMapperContract(t *testing.T) {
	store := NewActionPlanInputSetStore()
	root := actionPlanInputSetTestInsert(t, store, "", actionPlanInputSetTestEntries(8))

	identity, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := store.NodeCount()
	got, err := identity.Map(root)
	if err != nil || got != root || store.NodeCount() != before {
		t.Fatalf("identity target-stable map = %q, %v with %d nodes; want %q and %d nodes", got, err, store.NodeCount(), root, before)
	}

	moving, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		entry.Target.Path += ".moved"
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := moving.Map(root); err == nil || !strings.Contains(err.Error(), "moved target") {
		t.Fatalf("target-moving map error = %v", err)
	}

	sentinel := errors.New("retry")
	fail := true
	retrying, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if fail {
			return ActionPlanInputSetEntry{}, sentinel
		}
		entry.CompilerUse = true
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retrying.Map(root); !errors.Is(err, sentinel) {
		t.Fatalf("first map error = %v, want sentinel", err)
	}
	fail = false
	if _, err := retrying.Map(root); err != nil {
		t.Fatalf("retry after callback failure: %v", err)
	}
}

func TestActionPlanInputSetEntryValidation(t *testing.T) {
	store := NewActionPlanInputSetStore()
	producer := actionPlanInputSetTestDigest("producer")
	valid := []ActionPlanInputSetEntry{
		{
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: "linux", Path: "include/generated/autoconf.h"},
			SourceID: "src-00000001",
		},
		{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetAmbientTarget, Path: "toolchain/cc"},
			ProducerID: producer,
			Slot:       maximumActionPlanOrdinal,
		},
		{
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "family/source.c"},
			SourceID: "src-" + actionPlanInputSetTestDigest("family-source"),
		},
	}
	root := actionPlanInputSetTestInsert(t, store, "", valid)
	if err := store.ValidateRoot(root); err != nil {
		t.Fatalf("valid target kinds: %v", err)
	}

	invalid := []ActionPlanInputSetEntry{
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "none.o"}},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "both.o"}, SourceID: "src-00000001", ProducerID: producer},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Tree: "unexpected", Path: "tree.o"}, SourceID: "src-00000001"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Path: "missing-tree.o"}, SourceID: "src-00000001"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetTargetKind("unknown"), Path: "unknown.o"}, SourceID: "src-00000001"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "../escape.o"}, SourceID: "src-00000001"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "source-slot.o"}, SourceID: "src-00000001", Slot: 1},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "bad-producer.o"}, ProducerID: "bad"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "bad-slot.o"}, ProducerID: producer, Slot: maximumActionPlanOrdinal + 1},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "aux-only.o"}, ProducerID: producer, AuxiliaryUse: true},
	}
	for _, entry := range invalid {
		if _, err := store.Insert(root, entry); err == nil {
			t.Errorf("Insert() accepted invalid entry %#v", entry)
		}
	}
}

func TestActionPlanInputSetCollisionWitnesses(t *testing.T) {
	constantID := strings.Repeat("0", 64)
	store := newActionPlanInputSetStoreWithDigest(func([]byte) string { return constantID })
	root, err := store.Insert("", actionPlanInputSetTestEntry(1))
	if err != nil {
		t.Fatalf("first Insert(): %v", err)
	}
	witness, ok := store.CanonicalWitness(root)
	if !ok || !strings.Contains(string(witness), `"kind":"leaf"`) {
		t.Fatalf("canonical witness = %q, %v", witness, ok)
	}
	witness[0] ^= 0xff
	untouched, _ := store.CanonicalWitness(root)
	if len(untouched) == 0 || witness[0] == untouched[0] {
		t.Fatal("CanonicalWitness() exposed mutable store memory")
	}
	if _, err := store.Insert(root, actionPlanInputSetTestEntry(2)); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("second Insert() error = %v, want collision", err)
	}

	zeroKey := func(ActionPlanInputSetTarget) [sha256.Size]byte { return [sha256.Size]byte{} }
	keyCollisionStore := newActionPlanInputSetStoreWithDigests(actionPlanInputSetSHA256, zeroKey)
	keyRoot := actionPlanInputSetTestInsert(t, keyCollisionStore, "", actionPlanInputSetTestEntries(actionPlanInputSetLeafCapacity))
	if _, err := keyCollisionStore.Insert(keyRoot, actionPlanInputSetTestEntry(actionPlanInputSetLeafCapacity+1)); err == nil || !strings.Contains(err.Error(), "same SHA-256 target digest") {
		t.Fatalf("17-way key collision error = %v", err)
	}
}

func TestActionPlanInputSetImportClosureAndImmutability(t *testing.T) {
	store := NewActionPlanInputSetStore()
	root := ""
	for _, entry := range actionPlanInputSetTestEntries(80) {
		var err error
		root, err = store.Insert(root, entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	allNodeCount := store.NodeCount()
	closure, err := store.ReachableNodes(root)
	if err != nil {
		t.Fatalf("ReachableNodes(): %v", err)
	}
	if len(closure) >= allNodeCount {
		t.Fatalf("reachable closure has %d nodes, persistent store has %d; historical nodes leaked", len(closure), allNodeCount)
	}
	imported, err := NewActionPlanInputSetStoreFromNodes(closure)
	if err != nil {
		t.Fatalf("NewActionPlanInputSetStoreFromNodes(): %v", err)
	}
	if err := imported.ValidateRoot(root); err != nil {
		t.Fatalf("imported ValidateRoot(): %v", err)
	}
	if got := actionPlanInputSetTestCount(t, imported, root); got != 80 {
		t.Fatalf("imported count = %d, want 80", got)
	}

	rootCopy, _ := store.Node(root)
	rootCopy.Children[0].Nibble = "f"
	allCopies := store.Nodes()
	allCopies[root] = ActionPlanInputSetNode{}
	for id, node := range closure {
		if len(node.Entries) != 0 {
			node.Entries[0].Target.Path = "mutated.o"
			closure[id] = node
			break
		}
	}
	if err := store.ValidateRoot(root); err != nil {
		t.Fatalf("defensive-copy mutation changed store: %v", err)
	}

	cleanClosure, err := store.ReachableNodes(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("hash-mismatch", func(t *testing.T) {
		corrupt := actionPlanInputSetTestCloneNodes(cleanClosure)
		node := corrupt[root]
		node.Count++
		corrupt[root] = node
		if _, err := NewActionPlanInputSetStoreFromNodes(corrupt); err == nil {
			t.Fatal("import accepted node whose ID does not match its canonical bytes")
		}
	})
	t.Run("missing-child", func(t *testing.T) {
		corrupt := actionPlanInputSetTestCloneNodes(cleanClosure)
		node := corrupt[root]
		if len(node.Children) == 0 {
			t.Fatal("test root is not a branch")
		}
		node.Children[0].ID = strings.Repeat("f", 64)
		delete(corrupt, root)
		canonical, err := actionPlanInputSetCanonicalNode(node)
		if err != nil {
			t.Fatal(err)
		}
		corrupt[actionPlanInputSetSHA256(canonical)] = node
		if _, err := NewActionPlanInputSetStoreFromNodes(corrupt); err == nil || !strings.Contains(err.Error(), "missing child") {
			t.Fatalf("missing-child import error = %v", err)
		}
	})
	t.Run("noncanonical-child-order", func(t *testing.T) {
		corrupt := actionPlanInputSetTestCloneNodes(cleanClosure)
		node := corrupt[root]
		if len(node.Children) < 2 {
			t.Fatal("test root has fewer than two children")
		}
		node.Children[0], node.Children[1] = node.Children[1], node.Children[0]
		delete(corrupt, root)
		canonical, err := actionPlanInputSetCanonicalNode(node)
		if err != nil {
			t.Fatal(err)
		}
		corrupt[actionPlanInputSetSHA256(canonical)] = node
		if _, err := NewActionPlanInputSetStoreFromNodes(corrupt); err == nil || !strings.Contains(err.Error(), "strict nibble order") {
			t.Fatalf("child-order import error = %v", err)
		}
	})
}

func TestActionPlanInputSetStructuralSharingScale(t *testing.T) {
	const entryCount = 4096
	store := NewActionPlanInputSetStore()
	root := actionPlanInputSetTestInsert(t, store, "", actionPlanInputSetTestEntries(entryCount))
	if err := store.ValidateRoot(root); err != nil {
		t.Fatalf("ValidateRoot(4K): %v", err)
	}
	if store.NodeCount() >= entryCount*8 {
		t.Fatalf("persistent node count = %d, want less than %d", store.NodeCount(), entryCount*8)
	}
	retainedLeafEntries := 0
	for _, node := range store.nodes {
		retainedLeafEntries += len(node.Entries)
	}
	if retainedLeafEntries >= entryCount*20 {
		t.Fatalf("historical leaf-entry occurrences = %d, want less than %d", retainedLeafEntries, entryCount*20)
	}

	oldRoot := root
	oldNode, _ := store.Node(oldRoot)
	beforeNodes := store.NodeCount()
	newRoot, err := store.Insert(oldRoot, actionPlanInputSetTestEntry(entryCount+1))
	if err != nil {
		t.Fatalf("incremental Insert(): %v", err)
	}
	created := store.NodeCount() - beforeNodes
	if created > actionPlanInputSetDigestNibbles+16 {
		t.Fatalf("one insertion created %d nodes, want at most %d", created, actionPlanInputSetDigestNibbles+16)
	}
	newNode, _ := store.Node(newRoot)
	shared := actionPlanInputSetTestSharedChildren(oldNode.Children, newNode.Children)
	if shared < len(oldNode.Children)-1 {
		t.Fatalf("one insertion shared %d/%d root children", shared, len(oldNode.Children))
	}
	if got := actionPlanInputSetTestCount(t, store, oldRoot); got != entryCount {
		t.Fatalf("old root count after insertion = %d, want %d", got, entryCount)
	}
	if got := actionPlanInputSetTestCount(t, store, newRoot); got != entryCount+1 {
		t.Fatalf("new root count = %d, want %d", got, entryCount+1)
	}
}

func actionPlanInputSetTestEntries(count int) []ActionPlanInputSetEntry {
	entries := make([]ActionPlanInputSetEntry, count)
	for index := range entries {
		entries[index] = actionPlanInputSetTestEntry(index + 1)
	}
	return entries
}

func actionPlanInputSetTestEntry(index int) ActionPlanInputSetEntry {
	return ActionPlanInputSetEntry{
		Target: ActionPlanInputSetTarget{
			Kind: ActionPlanInputSetWorkTarget,
			Path: fmt.Sprintf("obj/%08d.o", index),
		},
		SourceID:    fmt.Sprintf("src-%08d", index),
		CompilerUse: index%2 == 0,
	}
}

func actionPlanInputSetTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func actionPlanInputSetTestInsert(t *testing.T, store *ActionPlanInputSetStore, root string, entries []ActionPlanInputSetEntry) string {
	t.Helper()
	for _, entry := range entries {
		var err error
		root, err = store.Insert(root, entry)
		if err != nil {
			t.Fatalf("Insert(%s): %v", actionPlanInputSetTargetDescription(entry.Target), err)
		}
	}
	return root
}

func actionPlanInputSetTestCount(t *testing.T, store *ActionPlanInputSetStore, root string) int {
	t.Helper()
	count := 0
	if err := store.Walk(root, func(ActionPlanInputSetEntry) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("Walk(): %v", err)
	}
	return count
}

func actionPlanInputSetTestSharedRootChildren(t *testing.T, store *ActionPlanInputSetStore, left, right string) int {
	t.Helper()
	leftNode, ok := store.Node(left)
	if !ok {
		t.Fatalf("missing left root %s", left)
	}
	rightNode, ok := store.Node(right)
	if !ok {
		t.Fatalf("missing right root %s", right)
	}
	return actionPlanInputSetTestSharedChildren(leftNode.Children, rightNode.Children)
}

func actionPlanInputSetTestSharedChildren(left, right []ActionPlanInputSetChild) int {
	rightByNibble := make(map[string]string, len(right))
	for _, child := range right {
		rightByNibble[child.Nibble] = child.ID
	}
	shared := 0
	for _, child := range left {
		if rightByNibble[child.Nibble] == child.ID {
			shared++
		}
	}
	return shared
}

func actionPlanInputSetTestCloneNodes(nodes map[string]ActionPlanInputSetNode) map[string]ActionPlanInputSetNode {
	cloned := make(map[string]ActionPlanInputSetNode, len(nodes))
	for id, node := range nodes {
		cloned[id] = cloneActionPlanInputSetNode(node)
	}
	return cloned
}
