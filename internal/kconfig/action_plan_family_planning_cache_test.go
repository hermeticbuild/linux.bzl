package kconfig

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestActionPlanFamilyPlanningCacheSharesPersistentInputSets(t *testing.T) {
	cache := NewActionPlanFamilyPlanningCache()
	first := &ActionPlan{}
	second := &ActionPlan{}
	first.attachFamilyPlanningCache(cache)
	second.attachFamilyPlanningCache(cache)

	firstStore, err := first.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := second.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	if firstStore != secondStore || firstStore != cache.inputSetStore() {
		t.Fatal("family plans do not share their persistent input-set store")
	}

	entries := familyPlanningInputSetEntries(256)
	base := familyPlanningInputSetInsert(t, firstStore, "", entries)
	if got := familyPlanningInputSetInsert(t, secondStore, "", entries); got != base {
		t.Fatalf("identical family input set has root %q, want reused root %q", got, base)
	}

	before := firstStore.NodeCount()
	variant, err := secondStore.Insert(base, familyPlanningInputSetEntry(len(entries)+1))
	if err != nil {
		t.Fatal(err)
	}
	if variant == base {
		t.Fatal("changed family input set retained the unchanged root")
	}
	created := firstStore.NodeCount() - before
	if created <= 0 || created > actionPlanInputSetDigestNibbles+16 {
		t.Fatalf("one configuration-local insertion created %d trie nodes", created)
	}
	baseNode, ok := firstStore.Node(base)
	if !ok || baseNode.Kind != ActionPlanInputSetBranchNode {
		t.Fatalf("base root = %#v, %t; want branch", baseNode, ok)
	}
	variantNode, ok := firstStore.Node(variant)
	if !ok || variantNode.Kind != ActionPlanInputSetBranchNode {
		t.Fatalf("variant root = %#v, %t; want branch", variantNode, ok)
	}
	if shared := familyPlanningInputSetSharedChildren(baseNode.Children, variantNode.Children); shared == 0 {
		t.Fatal("configuration-local insertion did not reuse any root subtries")
	}
}

func familyPlanningInputSetEntry(index int) ActionPlanInputSetEntry {
	return ActionPlanInputSetEntry{
		Target: ActionPlanInputSetTarget{
			Kind: ActionPlanInputSetWorkTarget,
			Path: fmt.Sprintf("family/%08d.o", index),
		},
		SourceID: fmt.Sprintf("src-%08d", index),
	}
}

func familyPlanningInputSetEntries(count int) []ActionPlanInputSetEntry {
	entries := make([]ActionPlanInputSetEntry, count)
	for index := range entries {
		entries[index] = familyPlanningInputSetEntry(index + 1)
	}
	return entries
}

func familyPlanningInputSetInsert(
	t *testing.T,
	store *ActionPlanInputSetStore,
	root string,
	entries []ActionPlanInputSetEntry,
) string {
	t.Helper()
	var err error
	for _, entry := range entries {
		root, err = store.Insert(root, entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func familyPlanningInputSetSharedChildren(left, right []ActionPlanInputSetChild) int {
	rightIDs := make(map[string]string, len(right))
	for _, child := range right {
		rightIDs[child.Nibble] = child.ID
	}
	shared := 0
	for _, child := range left {
		if rightIDs[child.Nibble] == child.ID {
			shared++
		}
	}
	return shared
}

func TestActionPlanFamilyPlanningCacheInitializesZeroValue(t *testing.T) {
	cache := &ActionPlanFamilyPlanningCache{}
	if cache.inputSetStore() == nil || cache.configDependencyCache() == nil {
		t.Fatal("zero-value family cache did not initialize shared stores")
	}
	firstStore := cache.inputSetStore()
	cache.initialize()
	if cache.inputSetStore() != firstStore {
		t.Fatal("family cache reinitialization replaced its persistent store")
	}
}

func TestActionPlanFamilyPlanningCacheReusesContentAddressedMaterializedCore(t *testing.T) {
	cache := NewActionPlanFamilyPlanningCache()
	newPlan := func() *ActionPlan {
		plan := &ActionPlan{Nodes: []ActionPlanNode{
			workingTreeTopologyNodeForTest("family-leaf"),
			workingTreeTopologyNodeForTest("family-root", "family-leaf"),
		}}
		plan.attachFamilyPlanningCache(cache)
		plan.inputSetStore = cache.inputSetStore()
		return plan
	}
	query := func(plan *ActionPlan) compactKbuildInputFrontier {
		t.Helper()
		builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
		frontier, err := builder.compactKbuildWorkingTreeInputFrontierFromRoots(
			"consumer", CompactKbuildProfile{Name: "family-persistent-core"}, nil,
			[]compactKbuildRuleInput{{producer: "family-root"}},
		)
		if err != nil {
			t.Fatal(err)
		}
		return frontier
	}

	first := newPlan()
	firstFrontier := query(first)
	if got, want := first.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("first family variant computed %d cores, want %d", got, want)
	}
	before := cache.inputSetStore().NodeCount()
	second := newPlan()
	secondFrontier := query(second)
	if got := second.workingTreeMaterializedCoreComputations; got != 0 {
		t.Fatalf("second family variant recomputed %d content-identical cores", got)
	}
	if cache.materializedStores != 1 || cache.materializedHits != 1 {
		t.Fatalf("persistent family core stores/hits = %d/%d, want 1/1", cache.materializedStores, cache.materializedHits)
	}
	if firstFrontier.inputSet == "" || secondFrontier.inputSet != firstFrontier.inputSet ||
		len(firstFrontier.direct) != 0 || len(secondFrontier.direct) != 0 {
		t.Fatalf("family persistent roots differ: first %#v second %#v", firstFrontier, secondFrontier)
	}
	if got := cache.inputSetStore().NodeCount(); got != before {
		t.Fatalf("second family variant grew shared input-set store from %d to %d nodes", before, got)
	}
	paths := func(plan *ActionPlan, frontier compactKbuildInputFrontier) []string {
		inputs, err := compactKbuildInputFrontierInputs(plan, frontier)
		if err != nil {
			t.Fatal(err)
		}
		result := make([]string, len(inputs))
		for index, input := range inputs {
			result[index] = input.path
		}
		return result
	}
	firstPaths := paths(first, firstFrontier)
	secondPaths := paths(second, secondFrontier)
	if !slices.Equal(firstPaths, secondPaths) ||
		!slices.Equal(secondPaths, []string{"family-leaf.o", "family-root.o"}) {
		t.Fatalf("family materialized paths differ: first %#v second %#v", firstPaths, secondPaths)
	}
}

// Each ordinary node depends on its predecessor, so querying the roots in
// order produces persistent cores of sizes 1..count. Populate through the real
// cold path, outside benchmark timing, rather than hand-installing cache entries.
type familyMaterializedCoreBenchmarkFixture struct {
	cache *ActionPlanFamilyPlanningCache
	plan  *ActionPlan
	roots [][]string
	keys  []string
	cores []*compactKbuildWorkingTreeMaterializedCore
}

func newFamilyMaterializedCoreBenchmarkFixture(tb testing.TB, count int) familyMaterializedCoreBenchmarkFixture {
	tb.Helper()
	setupStart := time.Now()
	fixture := familyMaterializedCoreBenchmarkFixture{
		cache: NewActionPlanFamilyPlanningCache(),
		plan:  &ActionPlan{Nodes: make([]ActionPlanNode, count)},
		roots: make([][]string, count),
		keys:  make([]string, count),
		cores: make([]*compactKbuildWorkingTreeMaterializedCore, count),
	}
	for index := range fixture.plan.Nodes {
		id := fmt.Sprintf("%064x", index+1)
		node := workingTreeTopologyNodeForTest(id)
		node.Outputs[0].Path = fmt.Sprintf("family/core/%08d.o", index)
		if index != 0 {
			node.Inputs = []ActionPlanNodeEdge{{Role: "input", ProducerID: fixture.plan.Nodes[index-1].ID}}
		}
		fixture.plan.Nodes[index] = node
		fixture.roots[index] = []string{id}
		fixture.keys[index] = compactKbuildClosureRootSetSignature(fixture.roots[index])
	}
	fixture.plan.attachFamilyPlanningCache(fixture.cache)
	nodeSetupElapsed := time.Since(setupStart)
	coldStart := time.Now()
	callbacks := 0
	for index, roots := range fixture.roots {
		core, processedLive, err := fixture.plan.compactKbuildWorkingTreeMaterializedCoreForConsumer(
			roots, nil, func(uint32) error { callbacks++; return nil },
		)
		if err != nil || !processedLive || core == nil {
			tb.Fatalf("cold core %d = %p, %t, %v", index, core, processedLive, err)
		}
		root, ok := fixture.cache.inputSets.Node(core.inputSet)
		if !ok || root.Count != index+1 || len(core.conflicts) != 0 || len(core.replayNodes) != 0 {
			tb.Fatalf("cold core %d is not an ordinary cumulative root: %#v", index, core)
		}
		if fixture.plan.workingTreeMaterializedCoreCache[fixture.keys[index]] != core ||
			fixture.cache.materializedCores[fixture.keys[index]] != core {
			tb.Fatalf("cold core %d was not retained in both production caches", index)
		}
		fixture.cores[index] = core
	}
	// Input-set insertion occurs inside the production cold-core queries, not
	// in the node/shared-store initialization phase. Neither phase is timed by
	// the cache-hit subbenchmarks below.
	tb.Logf("untimed fixture setup (%d roots): nodes/shared store %s; real cold cumulative core computation %s",
		count, nodeSetupElapsed, time.Since(coldStart))
	if fixture.plan.workingTreeMaterializedCoreComputations != count ||
		fixture.plan.workingTreeMaterializedNodeProjections != count ||
		callbacks != count*(count+1)/2 || fixture.cache.materializedStores != count || fixture.cache.materializedHits != 0 {
		tb.Fatalf("unexpected cold fixture work: computations=%d projections=%d callbacks=%d stores=%d hits=%d",
			fixture.plan.workingTreeMaterializedCoreComputations, fixture.plan.workingTreeMaterializedNodeProjections,
			callbacks, fixture.cache.materializedStores, fixture.cache.materializedHits)
	}
	return fixture
}

func (fixture familyMaterializedCoreBenchmarkFixture) siblingPlan() *ActionPlan {
	plan := &ActionPlan{Nodes: fixture.plan.Nodes}
	plan.attachFamilyPlanningCache(fixture.cache)
	// Index construction clears local cores and is not part of the ownership
	// walk or replay validation measured here.
	plan.ensureNodeLookupIndexes()
	return plan
}

func TestActionPlanFamilyMaterializedCoreCumulativeCacheHits(t *testing.T) {
	const count = 24
	fixture := newFamilyMaterializedCoreBenchmarkFixture(t, count)
	sibling := fixture.siblingPlan()
	before := fixture.cache.inputSets.NodeCount()
	callbacks := 0
	for pass := 0; pass < 2; pass++ {
		for index, roots := range fixture.roots {
			core, processedLive, err := sibling.compactKbuildWorkingTreeMaterializedCoreForConsumer(
				roots, nil, func(uint32) error { callbacks++; return nil },
			)
			if err != nil || processedLive || core != fixture.cores[index] {
				t.Fatalf("pass %d core %d missed production cache: %p, %t, %v", pass, index, core, processedLive, err)
			}
		}
		if fixture.cache.materializedHits != count {
			t.Fatalf("pass %d family hits = %d, want %d (second pass must be local)", pass, fixture.cache.materializedHits, count)
		}
	}
	if callbacks != 0 || sibling.workingTreeMaterializedCoreComputations != 0 ||
		sibling.workingTreeMaterializedNodeProjections != 0 || fixture.cache.inputSets.NodeCount() != before ||
		fixture.cache.materializedStores != count || len(fixture.cache.materializedCores) != count {
		t.Fatal("cached cumulative queries performed live work or changed the shared store")
	}
}

func BenchmarkActionPlanFamilyMaterializedCoreCumulativeCacheHits(b *testing.B) {
	for _, count := range []int{500, 1000, 2000} {
		b.Run(fmt.Sprintf("roots_%d", count), func(b *testing.B) {
			fixture := newFamilyMaterializedCoreBenchmarkFixture(b, count)
			for _, mode := range []string{"plan_warm", "family_ownership_only", "family_warm_plan_cold"} {
				b.Run(mode, func(b *testing.B) {
					plan := fixture.siblingPlan()
					if mode == "plan_warm" {
						plan = fixture.plan
					}
					beforeNodes := fixture.cache.inputSets.NodeCount()
					beforeHits := fixture.cache.materializedHits
					beforeComputations := plan.workingTreeMaterializedCoreComputations
					beforeProjections := plan.workingTreeMaterializedNodeProjections
					callbacks := 0
					processNode := func(uint32) error { callbacks++; return nil }
					b.ReportAllocs()
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						if mode == "family_warm_plan_cold" {
							b.StopTimer()
							plan.clearWorkingTreeMaterializedCoreCache()
							b.StartTimer()
						}
						for index, roots := range fixture.roots {
							if mode == "family_ownership_only" {
								core, hit, err := fixture.cache.lookupMaterializedCore(plan, fixture.keys[index])
								if err != nil || !hit || core != fixture.cores[index] {
									b.Fatalf("ownership query %d missed family cache: %p, %t, %v", index, core, hit, err)
								}
								continue
							}
							core, processedLive, err := plan.compactKbuildWorkingTreeMaterializedCoreForConsumer(roots, nil, processNode)
							if err != nil || processedLive || core != fixture.cores[index] {
								b.Fatalf("query %d missed production cache: %p, %t, %v", index, core, processedLive, err)
							}
						}
					}
					b.StopTimer()
					wantHits := count * b.N
					if mode == "plan_warm" {
						wantHits = 0
					}
					if callbacks != 0 || plan.workingTreeMaterializedCoreComputations != beforeComputations ||
						plan.workingTreeMaterializedNodeProjections != beforeProjections ||
						fixture.cache.materializedHits-beforeHits != wantHits || fixture.cache.inputSets.NodeCount() != beforeNodes ||
						fixture.cache.materializedStores != count || len(fixture.cache.materializedCores) != count {
						b.Fatal("benchmark did not remain on its intended cache-hit path")
					}
					// This is the fixture's logical entry count, not an instrumented
					// count of implementation visits.
					b.ReportMetric(float64(count*(count+1)/2), "logical_entries/op")
				})
			}
		})
	}
}

type materializedLeftUnionConflictForTest struct {
	left, right ActionPlanInputSetEntry
}

type materializedLeftUnionResultForTest struct {
	root      string
	conflicts []materializedLeftUnionConflictForTest
}

// This is the old public Union control flow, retained only as an independent
// uncached oracle. unionAt and its recursive merge implementation are unchanged;
// this helper never calls the new public cache or alters its enablement.
func uncachedActionPlanInputSetUnionForTest(
	store *ActionPlanInputSetStore, left, right string, resolve ActionPlanInputSetConflictResolver,
) (string, error) {
	if err := store.ready(); err != nil {
		return "", err
	}
	if left == "" {
		if right != "" {
			if err := store.validateRootReference(right); err != nil {
				return "", err
			}
		}
		return right, nil
	}
	if right == "" {
		if err := store.validateRootReference(left); err != nil {
			return "", err
		}
		return left, nil
	}
	if err := store.validateRootReference(left); err != nil {
		return "", err
	}
	if err := store.validateRootReference(right); err != nil {
		return "", err
	}
	return store.unionAt(left, right, resolve)
}

// Each observer belongs only to its current test call. Callback events are
// compared directly with the independent uncached implementation, never cached.
func materializedLeftUnionForTest(store *ActionPlanInputSetStore, left, right string) (materializedLeftUnionResultForTest, error) {
	var events []materializedLeftUnionConflictForTest
	root, err := store.Union(left, right, func(
		_ ActionPlanInputSetTarget, left, right ActionPlanInputSetEntry,
	) (ActionPlanInputSetEntry, error) {
		events = append(events, materializedLeftUnionConflictForTest{left: left, right: right})
		return left, nil
	})
	return materializedLeftUnionResultForTest{root: root, conflicts: events}, err
}

func originalMaterializedLeftUnionForTest(store *ActionPlanInputSetStore, left, right string) (materializedLeftUnionResultForTest, error) {
	var events []materializedLeftUnionConflictForTest
	root, err := uncachedActionPlanInputSetUnionForTest(store, left, right, func(
		_ ActionPlanInputSetTarget, left, right ActionPlanInputSetEntry,
	) (ActionPlanInputSetEntry, error) {
		events = append(events, materializedLeftUnionConflictForTest{left: left, right: right})
		return left, nil
	})
	return materializedLeftUnionResultForTest{root: root, conflicts: events}, err
}

func materializedLeftUnionEntryForTest(index int) ActionPlanInputSetEntry {
	return ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("family/core/%08d.o", index)},
		ProducerID: fmt.Sprintf("%064x", index+1),
	}
}

func materializedLeftUnionRootForTest(tb testing.TB, store *ActionPlanInputSetStore, entries []ActionPlanInputSetEntry) string {
	tb.Helper()
	root := ""
	for _, entry := range entries {
		var err error
		root, err = store.Insert(root, entry)
		if err != nil {
			tb.Fatal(err)
		}
	}
	return root
}

func TestConflictFreeUnionMemoCanonicalRootsAndOrderedEvents(t *testing.T) {
	store := NewActionPlanInputSetStore()
	base := make([]ActionPlanInputSetEntry, 64)
	changed := make([]ActionPlanInputSetEntry, len(base))
	for index := range base {
		base[index] = materializedLeftUnionEntryForTest(index)
		changed[index] = base[index]
		changed[index].ProducerID = fmt.Sprintf("%064x", index+1001)
	}
	leaf := materializedLeftUnionRootForTest(t, store, base[:8])
	leafChanged := materializedLeftUnionRootForTest(t, store, changed[4:12])
	branch := materializedLeftUnionRootForTest(t, store, base[:48])
	branchChanged := materializedLeftUnionRootForTest(t, store, changed[4:56])
	flags := base[0]
	flags.CompilerUse, flags.AuxiliaryUse = true, true
	flagRoot := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{flags})
	namespaces := []ActionPlanInputSetEntry{base[0], base[0], base[0], base[0]}
	namespaces[1].Target.Kind = ActionPlanInputSetAmbientTarget
	namespaces[2].Target.Kind, namespaces[2].Target.Tree = ActionPlanInputSetTreeTarget, "objects"
	namespaces[3].Target.Kind, namespaces[3].Target.Tree = ActionPlanInputSetTreeTarget, "headers"
	for _, test := range []struct {
		name        string
		left, right string
		conflicts   int
	}{
		{"empty", "", "", 0},
		{"empty_left", "", leaf, 0},
		{"empty_right", leaf, "", 0},
		{"identity", branch, branch, 0},
		{"identical_entries", leaf, branch, 0},
		{"leaf_leaf", leaf, leafChanged, 4},
		{"leaf_branch", leaf, branchChanged, 4},
		{"branch_leaf", branchChanged, leaf, 4},
		{"branch_branch", branch, branchChanged, 44},
		{"branch_branch_reversed", branchChanged, branch, 44},
		{"flags_only", leaf, flagRoot, 1},
		{"namespaces", materializedLeftUnionRootForTest(t, store, namespaces[:2]), materializedLeftUnionRootForTest(t, store, namespaces[2:]), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			want, err := originalMaterializedLeftUnionForTest(store, test.left, test.right)
			if err != nil || len(want.conflicts) != test.conflicts {
				t.Fatalf("oracle = %#v, %v; want %d conflicts", want, err, test.conflicts)
			}
			before := store.NodeCount()
			for pass := 0; pass < 3; pass++ {
				got, err := materializedLeftUnionForTest(store, test.left, test.right)
				if err != nil || got.root != want.root || !slices.Equal(got.conflicts, want.conflicts) {
					t.Fatalf("pass %d differs from original Union: got %#v, %v; want %#v", pass, got, err, want)
				}
				// The observer is fresh on every call; conflicts must execute
				// the current live callback rather than retain any old trace.
				if len(got.conflicts) != 0 {
					got.conflicts[0].left.ProducerID = "caller-mutated"
				}
			}
			_, cached := store.unionCache[[2]string{test.left, test.right}]
			wantCached := test.conflicts == 0 && test.left != "" && test.right != "" && test.left != test.right
			if cached != wantCached || store.NodeCount() != before {
				t.Fatal("unexpected memo admission, misses, or store growth")
			}
		})
	}
}

func TestConflictFreeUnionMemoStoreReplacementAndReimport(t *testing.T) {
	store := NewActionPlanInputSetStore()
	left := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(0)})
	right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(1)})
	want, err := materializedLeftUnionForTest(store, left, right)
	if err != nil {
		t.Fatal(err)
	}
	empty := NewActionPlanInputSetStore()
	if _, err := materializedLeftUnionForTest(empty, left, right); err == nil || len(empty.unionCache) != 0 {
		t.Fatal("store replacement reused a result for absent operands")
	}
	nodes, err := store.ReachableNodesForRoots([]string{left, right})
	if err != nil {
		t.Fatal(err)
	}
	reimported, err := NewActionPlanInputSetStoreFromNodes(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reimported.Node(want.root); exists {
		t.Fatal("operand-only reimport unexpectedly included cached result")
	}
	if len(reimported.unionCache) != 0 || !reimported.unionCacheEnabled {
		t.Fatal("standard reimport inherited memo entries or disabled caching")
	}
	for _, current := range []*ActionPlanInputSetStore{reimported, store} {
		for pass := 0; pass < 2; pass++ {
			got, err := materializedLeftUnionForTest(current, left, right)
			if err != nil || got.root != want.root || len(current.unionCache) != 1 {
				t.Fatalf("store switch pass %d = %#v, %v; cached pairs %d", pass, got, err, len(current.unionCache))
			}
			if _, exists := current.Node(got.root); !exists {
				t.Fatal("memo result is absent from the current store")
			}
		}
	}
}

func TestConflictFreeUnionMemoValidatesHitOperands(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionPlanInputSetStore, string) *ActionPlanInputSetStore
	}{
		{"nil_store", func(*ActionPlanInputSetStore, string) *ActionPlanInputSetStore { return nil }},
		{"uninitialized_store", func(*ActionPlanInputSetStore, string) *ActionPlanInputSetStore { return &ActionPlanInputSetStore{} }},
		{"unready_hit", func(store *ActionPlanInputSetStore, _ string) *ActionPlanInputSetStore {
			store.witnesses = nil
			return store
		}},
		{"missing_hit_operand", func(store *ActionPlanInputSetStore, left string) *ActionPlanInputSetStore {
			delete(store.nodes, left)
			return store
		}},
		{"nonroot_hit_operand", func(store *ActionPlanInputSetStore, left string) *ActionPlanInputSetStore {
			node := store.nodes[left]
			node.Depth++
			store.nodes[left] = node
			return store
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewActionPlanInputSetStore()
			left := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(0)})
			right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(1)})
			if _, err := materializedLeftUnionForTest(store, left, right); err != nil {
				t.Fatal(err)
			}
			// Deliberately violate store invariants to verify the retained
			// public readiness/reference checks still fail like original Union.
			store = test.mutate(store, left)
			want, wantErr := originalMaterializedLeftUnionForTest(store, left, right)
			got, err := materializedLeftUnionForTest(store, left, right)
			if wantErr == nil || err == nil || err.Error() != wantErr.Error() || got.root != want.root || !slices.Equal(got.conflicts, want.conflicts) {
				t.Fatalf("invalid operands returned %#v, %v; want %#v, %v", got, err, want, wantErr)
			}
		})
	}
}

func TestConflictFreeUnionMemoPreservesFailureEventPrefix(t *testing.T) {
	// Both operands are valid leaves. Their union resolves one conflict before
	// failing because a seventeenth distinct target has the same key digest.
	store := newActionPlanInputSetStoreWithDigests(actionPlanInputSetSHA256, func(ActionPlanInputSetTarget) [32]byte { return [32]byte{} })
	entries := make([]ActionPlanInputSetEntry, actionPlanInputSetLeafCapacity)
	for index := range entries {
		entries[index] = materializedLeftUnionEntryForTest(index)
	}
	left := materializedLeftUnionRootForTest(t, store, entries)
	changed := entries[0]
	changed.CompilerUse = true
	right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{changed, materializedLeftUnionEntryForTest(len(entries))})
	want, wantErr := originalMaterializedLeftUnionForTest(store, left, right)
	if wantErr == nil || len(want.conflicts) != 1 {
		t.Fatalf("oracle did not produce a conflict prefix followed by failure: %#v, %v", want, wantErr)
	}
	for pass := 0; pass < 2; pass++ {
		got, err := materializedLeftUnionForTest(store, left, right)
		if err == nil || err.Error() != wantErr.Error() || got.root != want.root || !slices.Equal(got.conflicts, want.conflicts) {
			t.Fatalf("failure pass %d = %#v, %v; want %#v, %v", pass, got, err, want, wantErr)
		}
		if len(store.unionCache) != 0 || store.unionCacheEnabled {
			t.Fatal("failed union or its event prefix was cached")
		}
	}
}

func TestConflictFreeUnionMemoBounds(t *testing.T) {
	store := NewActionPlanInputSetStore()
	entries := []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(0), materializedLeftUnionEntryForTest(1), materializedLeftUnionEntryForTest(2)}
	left := materializedLeftUnionRootForTest(t, store, entries[:1])
	right := materializedLeftUnionRootForTest(t, store, entries[1:2])
	extra := materializedLeftUnionRootForTest(t, store, entries[2:])
	changed := entries[0]
	changed.CompilerUse = true
	conflicting := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{changed})
	if _, err := store.Union(left, right, nil); err != nil {
		t.Fatal(err)
	}
	if len(store.unionCache) != 1 {
		t.Fatal("nontrivial conflict-free pair was not admitted")
	}
	// Fill irrelevant slots to test the production cap without building
	// 32,768 unrelated sets. These synthetic empty-left keys are never read:
	// public Union returns before cache lookup for every empty operand.
	for index := 0; len(store.unionCache) < maximumActionPlanInputSetUnionCachePairs; index++ {
		store.unionCache[[2]string{"", fmt.Sprintf("%064x", index)}] = left
	}
	for pass := 0; pass < 3; pass++ {
		for _, pair := range [][2]string{{left, right}, {left, extra}, {extra, left}, {left, conflicting}} {
			want, wantErr := originalMaterializedLeftUnionForTest(store, pair[0], pair[1])
			got, err := materializedLeftUnionForTest(store, pair[0], pair[1])
			if err != nil || wantErr != nil || got.root != want.root || !slices.Equal(got.conflicts, want.conflicts) {
				t.Fatalf("full cache changed live result/events: got %#v, %v; want %#v, %v", got, err, want, wantErr)
			}
			if len(store.unionCache) != maximumActionPlanInputSetUnionCachePairs {
				t.Fatal("full union cache grew or discarded existing entries")
			}
		}
	}
	if _, cached := store.unionCache[[2]string{left, extra}]; cached {
		t.Fatal("full cache admitted an additional pair")
	}
}

func TestConflictFreeUnionMemoResolverIndependenceAndLiveConflicts(t *testing.T) {
	store := NewActionPlanInputSetStore()
	entry := materializedLeftUnionEntryForTest(0)
	left := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{entry})
	right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(1)})
	entry.CompilerUse = true
	conflicting := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{entry})
	calls := 0
	chooseLeft := func(_ ActionPlanInputSetTarget, left, _ ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		calls++
		return left, nil
	}
	chooseRight := func(_ ActionPlanInputSetTarget, _, right ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		calls++
		return right, nil
	}
	fail := func(_ ActionPlanInputSetTarget, _, _ ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		calls++
		return ActionPlanInputSetEntry{}, fmt.Errorf("resolver failure")
	}
	for _, resolver := range []ActionPlanInputSetConflictResolver{nil, fail, chooseLeft, chooseRight} {
		got, err := store.Union(left, right, resolver)
		want, wantErr := uncachedActionPlanInputSetUnionForTest(store, left, right, resolver)
		if err != nil || wantErr != nil || got != want || calls != 0 {
			t.Fatalf("conflict-free resolver-independent union = %q, %v; calls %d", got, err, calls)
		}
	}
	if len(store.unionCache) != 1 || store.unionCache[[2]string{left, right}] == "" {
		t.Fatal("conflict-free pair was not shared across different resolvers")
	}
	for index, resolver := range []ActionPlanInputSetConflictResolver{chooseLeft, chooseRight, fail, nil} {
		beforeCalls := calls
		got, err := store.Union(left, conflicting, resolver)
		if resolver != nil && calls != beforeCalls+1 {
			t.Fatalf("conflicting call %d did not immediately invoke its current resolver", index)
		}
		want, wantErr := uncachedActionPlanInputSetUnionForTest(store, left, conflicting, resolver)
		if got != want || (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
			t.Fatalf("conflicting call %d = %q, %v; want %q, %v", index, got, err, want, wantErr)
		}
		if len(store.unionCache) != 1 {
			t.Fatal("conflicting or failed union was admitted")
		}
	}
}

func TestConflictFreeUnionMemoInvalidatesProvisionalMode(t *testing.T) {
	store := newPlanningActionPlanInputSetStore()
	leftEntry, rightEntry := materializedLeftUnionEntryForTest(0), materializedLeftUnionEntryForTest(1)
	leftEntry.ProducerID, rightEntry.ProducerID = "provisional-left", "provisional-right"
	left := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{leftEntry})
	right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{rightEntry})
	validRoot, err := store.Union(left, right, nil)
	if err != nil || len(store.unionCache) != 1 {
		t.Fatalf("provisional union = %q, %v", validRoot, err)
	}
	store.allowProvisionalProducerIDs = false
	want, wantErr := uncachedActionPlanInputSetUnionForTest(store, left, right, nil)
	got, err := store.Union(left, right, nil)
	if wantErr == nil || err == nil || err.Error() != wantErr.Error() || got != want || len(store.unionCache) != 0 {
		t.Fatalf("strict mode reused provisional result: %q, %v; want %q, %v", got, err, want, wantErr)
	}
	store.allowProvisionalProducerIDs = true
	for pass := 0; pass < 2; pass++ {
		got, err := store.Union(left, right, nil)
		if err != nil || got != validRoot || len(store.unionCache) != 1 {
			t.Fatalf("restored provisional mode pass %d = %q, %v; cached pairs %d", pass, got, err, len(store.unionCache))
		}
	}
}

func TestConflictFreeUnionMemoDisablesCustomHooks(t *testing.T) {
	for _, hook := range []string{"digest", "key_digest"} {
		t.Run(hook, func(t *testing.T) {
			calls := 0
			digest := actionPlanInputSetSHA256
			keyDigest := actionPlanInputSetTargetDigest
			if hook == "digest" {
				digest = func(value []byte) string { calls++; return actionPlanInputSetSHA256(value) }
			} else {
				keyDigest = func(target ActionPlanInputSetTarget) [32]byte { calls++; return actionPlanInputSetTargetDigest(target) }
			}
			store := newActionPlanInputSetStoreWithDigests(digest, keyDigest)
			entries := make([]ActionPlanInputSetEntry, 20)
			for index := range entries {
				entries[index] = materializedLeftUnionEntryForTest(index)
			}
			left := materializedLeftUnionRootForTest(t, store, entries[:16])
			right := materializedLeftUnionRootForTest(t, store, entries[16:])
			if store.unionCacheEnabled {
				t.Fatal("custom digest constructor enabled caching")
			}
			for pass := 0; pass < 2; pass++ {
				beforeCalls := calls
				if _, err := store.Union(left, right, nil); err != nil || calls <= beforeCalls || len(store.unionCache) != 0 {
					t.Fatalf("custom hook pass %d skipped live behavior: %v; calls %d -> %d", pass, err, beforeCalls, calls)
				}
			}
		})
	}
}

func TestConflictFreeUnionMemoConstructorEligibility(t *testing.T) {
	imported, err := NewActionPlanInputSetStoreFromNodes(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*ActionPlanInputSetStore{NewActionPlanInputSetStore(), newPlanningActionPlanInputSetStore(), imported} {
		if !store.unionCacheEnabled || len(store.unionCache) != 0 {
			t.Fatal("standard constructor did not enable an empty union cache")
		}
	}
	// Even passing the exact standard functions to a custom constructor does
	// not opt in. Eligibility depends on the constructor, not function identity.
	for _, store := range []*ActionPlanInputSetStore{
		newActionPlanInputSetStoreWithDigest(actionPlanInputSetSHA256),
		newActionPlanInputSetStoreWithDigests(actionPlanInputSetSHA256, actionPlanInputSetTargetDigest),
	} {
		if store.unionCacheEnabled {
			t.Fatal("custom constructor opted into union caching")
		}
	}
}

func TestConflictFreeUnionMemoPreservesHitReferenceErrorOrder(t *testing.T) {
	store := NewActionPlanInputSetStore()
	left := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(0)})
	right := materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(1)})
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		if _, err := store.Union(pair[0], pair[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	delete(store.nodes, left)
	delete(store.nodes, right)
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		want, wantErr := uncachedActionPlanInputSetUnionForTest(store, pair[0], pair[1], nil)
		got, err := store.Union(pair[0], pair[1], nil)
		if wantErr == nil || err == nil || err.Error() != wantErr.Error() || got != want {
			t.Fatalf("cached operand validation order changed: %q, %v; want %q, %v", got, err, want, wantErr)
		}
	}
}

func TestConflictFreeUnionMemoPostorderConflictCallbacks(t *testing.T) {
	const count, distinctPaths = 80, 32
	store := NewActionPlanInputSetStore()
	singletons := make([]string, count)
	for index := range singletons {
		entry := materializedLeftUnionEntryForTest(index)
		entry.Target = materializedLeftUnionEntryForTest(index % distinctPaths).Target
		singletons[index] = materializedLeftUnionRootForTest(t, store, []ActionPlanInputSetEntry{entry})
	}
	for pass := 0; pass < 2; pass++ {
		for end := 0; end < count; end++ {
			root, expectedRoot := "", ""
			var observed, expected []materializedLeftUnionConflictForTest
			for _, singleton := range singletons[:end+1] {
				want, wantErr := originalMaterializedLeftUnionForTest(store, expectedRoot, singleton)
				got, err := materializedLeftUnionForTest(store, root, singleton)
				if err != nil || wantErr != nil || got.root != want.root || !slices.Equal(got.conflicts, want.conflicts) {
					t.Fatalf("pass %d batch %d differs from original Union", pass, end)
				}
				root, expectedRoot = got.root, want.root
				observed = append(observed, got.conflicts...)
				expected = append(expected, want.conflicts...)
			}
			if !slices.Equal(observed, expected) || len(observed) != max(0, end+1-distinctPaths) {
				t.Fatalf("pass %d batch %d changed complete consumer conflict trace", pass, end)
			}
		}
	}
	if len(store.unionCache) != distinctPaths-1 {
		t.Fatalf("retained pairs = %d, want %d nontrivial conflict-free pairs", len(store.unionCache), distinctPaths-1)
	}
}

func BenchmarkMaterializedLeftUnionPostorderCumulative(b *testing.B) {
	for _, count := range []int{500, 1000, 2000} {
		b.Run(fmt.Sprintf("roots_%d", count), func(b *testing.B) {
			store := NewActionPlanInputSetStore()
			singletons, expected := make([]string, count), make([]string, count)
			prefix := ""
			for index := range singletons {
				singletons[index] = materializedLeftUnionRootForTest(b, store, []ActionPlanInputSetEntry{materializedLeftUnionEntryForTest(index)})
				result, err := originalMaterializedLeftUnionForTest(store, prefix, singletons[index])
				if err != nil || len(result.conflicts) != 0 {
					b.Fatalf("ordinary prefix fixture = %#v, %v", result, err)
				}
				prefix, expected[index] = result.root, result.root
			}
			for _, mode := range []string{"original_union", "production_cold", "production_warm"} {
				b.Run(mode, func(b *testing.B) {
					store.unionCache = nil
					leftResolver := func(_ ActionPlanInputSetTarget, left, _ ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
						return left, nil
					}
					if mode == "production_warm" {
						prefix := ""
						for _, singleton := range singletons {
							result, err := store.Union(prefix, singleton, leftResolver)
							if err != nil {
								b.Fatal(err)
							}
							prefix = result
						}
					}
					beforeNodes := store.NodeCount()
					queries := count * (count + 1) / 2
					b.ReportAllocs()
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						if mode == "production_cold" {
							b.StopTimer()
							store.unionCache = nil
							b.StartTimer()
						}
						// This is exactly the merge sequence produced by postorder
						// chain queries, without copying the planner or timing DFS.
						for end := 0; end < count; end++ {
							root := ""
							for index := 0; index <= end; index++ {
								var result materializedLeftUnionResultForTest
								var err error
								if mode == "original_union" {
									result, err = originalMaterializedLeftUnionForTest(store, root, singletons[index])
								} else {
									result.root, err = store.Union(root, singletons[index], leftResolver)
								}
								if err != nil || len(result.conflicts) != 0 {
									b.Fatalf("ordinary merge = %#v, %v", result, err)
								}
								root = result.root
							}
							if root != expected[end] {
								b.Fatalf("query %d root = %q, want %q", end, root, expected[end])
							}
						}
					}
					b.StopTimer()
					if store.NodeCount() != beforeNodes {
						b.Fatal("repeated merge sequence grew the canonical store")
					}
					if mode != "original_union" && len(store.unionCache) != count-1 {
						b.Fatalf("cached pairs = %d, want %d nontrivial pairs", len(store.unionCache), count-1)
					}
					b.ReportMetric(float64(queries), "unions/op")
				})
			}
		})
	}
}
