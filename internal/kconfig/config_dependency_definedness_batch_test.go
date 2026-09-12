package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Keep the pre-batching algorithm independent of the production implementation.
// In particular, preserve its per-name validation, persistent write, recorded-bit
// canonicalization and final digest chaining as the semantic parity oracle.
func configDependencyApplyCompilerDefinednessOldReference(
	state configDependencyMacroState,
	definitions map[string]bool,
	identity string,
) (configDependencyMacroState, bool) {
	if identity == "" {
		return state, true
	}
	for _, name := range slices.Sorted(maps.Keys(definitions)) {
		defined := definitions[name]
		identifier, valid := configDependencyMacroIdentifier(name)
		previous, present := state.snapshot.lookup(name)
		if !valid || identifier != name || !present || previous.definition == configDependencyMacroDefined && !defined {
			return configDependencyMacroState{}, false
		}
		cell := configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined}
		if defined {
			cell.definition = configDependencyMacroDefined
			cell.recorded = true
		}
		state.snapshot, valid = state.snapshot.withCell(name, configDependencyMacroStateCanonicalCell(name, cell))
		if !valid {
			return configDependencyMacroState{}, false
		}
	}
	digest := sha256.New()
	digest.Write(state.compilerPredefinedDigest[:])
	digest.Write([]byte(identity))
	copy(state.compilerPredefinedDigest[:], digest.Sum(nil))
	state.compilerPredefinedSnapshot = state.snapshot
	return state, true
}

type configDependencyDefinednessNamespaceImage struct {
	fallback configDependencyMacroSnapshotCell
	facts    map[string]configDependencyMacroSnapshotCell
}

func configDependencyDefinednessSnapshotImage(snapshot *configDependencyMacroSnapshot) []configDependencyDefinednessNamespaceImage {
	if snapshot == nil {
		return nil
	}
	var result []configDependencyDefinednessNamespaceImage
	for _, namespace := range []configDependencyMacroSnapshotNamespace{snapshot.ordinary, snapshot.config, snapshot.reserved} {
		image := configDependencyDefinednessNamespaceImage{namespace.fallback, map[string]configDependencyMacroSnapshotCell{}}
		configDependencyMacroSnapshotWalkTree(namespace.facts, func(name string, cell configDependencyMacroSnapshotCell) {
			// Lazy transforms can leave entries equal to fallback. Compare the
			// complete observable cells, not an incidental tree representation.
			if cell != namespace.fallback {
				image.facts[name] = cell
			}
		})
		result = append(result, image)
	}
	return result
}

func configDependencyDefinednessMemoImage(snapshot *configDependencyMacroSnapshot) map[any]any {
	result := map[any]any{}
	if snapshot != nil {
		snapshot.lookupMemo.Range(func(key, value any) bool {
			result[key] = value
			return true
		})
	}
	return result
}

func configDependencyDefinednessCloneRoot(state configDependencyMacroState) configDependencyMacroState {
	if state.snapshot != nil {
		original := state.snapshot
		// Share immutable subtrees, never a populated sync.Map or sync.Once.
		state.snapshot = &configDependencyMacroSnapshot{
			ordinary: original.ordinary, config: original.config, reserved: original.reserved,
		}
		if state.compilerPredefinedSnapshot == original {
			state.compilerPredefinedSnapshot = state.snapshot
		}
	}
	return state
}

func requireConfigDependencyDefinednessTreap(t testing.TB, tree configDependencyMacroSnapshotTreeView, lower, upper string) {
	t.Helper()
	if tree.root == nil {
		return
	}
	node, _ := configDependencyMacroSnapshotExposeTree(tree)
	if lower != "" && node.name <= lower || upper != "" && node.name >= upper {
		t.Fatalf("batch tree violates strict name ordering at %q", node.name)
	}
	for _, child := range []configDependencyMacroSnapshotTreeView{node.left, node.right} {
		if child.root != nil && !configDependencyMacroSnapshotTreePriorityGreater(node.priority, node.name, child.root.priority, child.root.name) {
			t.Fatalf("batch tree violates priority/name heap ordering at %q", node.name)
		}
	}
	requireConfigDependencyDefinednessTreap(t, node.left, lower, node.name)
	requireConfigDependencyDefinednessTreap(t, node.right, node.name, upper)
}

func requireConfigDependencyDefinednessBatchParity(t testing.TB, original configDependencyMacroState, definitions map[string]bool, identity string) (configDependencyMacroState, bool) {
	t.Helper()
	before := configDependencyDefinednessSnapshotImage(original.snapshot)
	memo := configDependencyDefinednessMemoImage(original.snapshot)
	answers := maps.Clone(definitions)
	want, wantOK := configDependencyApplyCompilerDefinednessOldReference(configDependencyDefinednessCloneRoot(original), definitions, identity)
	got, gotOK := configDependencyApplyCompilerDefinedness(original, definitions, identity)
	if gotOK != wantOK {
		t.Fatalf("batch validity = %t, old reference = %t; definitions=%v", gotOK, wantOK, definitions)
	}
	if !reflect.DeepEqual(before, configDependencyDefinednessSnapshotImage(original.snapshot)) ||
		!reflect.DeepEqual(memo, configDependencyDefinednessMemoImage(original.snapshot)) || !maps.Equal(answers, definitions) {
		t.Fatal("batch mutated an original namespace, lookup memo, or caller-owned answers")
	}
	if !gotOK {
		if !reflect.DeepEqual(got, configDependencyMacroState{}) {
			t.Fatal("rejected batch returned partial measured state")
		}
		return got, false
	}
	if !reflect.DeepEqual(configDependencyDefinednessSnapshotImage(got.snapshot), configDependencyDefinednessSnapshotImage(want.snapshot)) {
		t.Fatal("batch complete namespace differs from old reference")
	}
	if got.snapshot != nil {
		for _, namespace := range []configDependencyMacroSnapshotNamespace{got.snapshot.ordinary, got.snapshot.config, got.snapshot.reserved} {
			requireConfigDependencyDefinednessTreap(t, namespace.facts, "", "")
		}
	}
	gotProjection, gotProjectionOK := got.snapshot.conditionalProjection()
	wantProjection, wantProjectionOK := want.snapshot.conditionalProjection()
	if gotProjectionOK != wantProjectionOK || !reflect.DeepEqual(gotProjection, wantProjection) {
		t.Fatal("batch canonical conditional projection differs from old reference")
	}
	if got.compilerPredefinedDigest != want.compilerPredefinedDigest ||
		!reflect.DeepEqual(configDependencyDefinednessSnapshotImage(got.compilerPredefinedSnapshot), configDependencyDefinednessSnapshotImage(want.compilerPredefinedSnapshot)) {
		t.Fatal("batch changed the compiler witness digest or predefined namespace")
	}
	if identity != "" && got.compilerPredefinedSnapshot != got.snapshot {
		t.Fatal("measured compiler digest was not pinned to the published snapshot")
	}
	if identity == "" && (got.snapshot != original.snapshot || got.compilerPredefinedSnapshot != original.compilerPredefinedSnapshot) {
		t.Fatal("empty identity changed snapshot identity")
	}
	// Aside from these three explicitly derived fields, the state contract is
	// unchanged, including replacement records, CONFIG symbols and lineage.
	unchanged := got
	unchanged.snapshot = original.snapshot
	unchanged.compilerPredefinedSnapshot = original.compilerPredefinedSnapshot
	unchanged.compilerPredefinedDigest = original.compilerPredefinedDigest
	if !reflect.DeepEqual(unchanged, original) {
		t.Fatal("batch changed unrelated macro state")
	}
	return got, true
}

func configDependencyDefinednessStateForBatch(snapshot *configDependencyMacroSnapshot) configDependencyMacroState {
	return configDependencyMacroState{
		snapshot: snapshot, compilerPredefinedSnapshot: snapshot,
		compilerPredefinedDigest: sha256.Sum256([]byte("batch initial compiler witness")),
		symbols:                  map[string]bool{"CONFIG_RETAINED": true},
		macroReplacements: map[string]configDependencyMacroReplacement{
			"RETAINED": {text: "RETAINED CONFIG_RETAINED", origin: "compiler:1"},
		},
	}
}

func TestConfigDependencyDefinednessBatchAllCells(t *testing.T) {
	for _, name := range []string{"ORDINARY", "CONFIG_VALUE", "__RESERVED", configDependencyResolvedAutoconfGuard} {
		for index := range configDependencyMacroSnapshotCellCount {
			for _, defined := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cell%d/defined%t", name, index, defined), func(t *testing.T) {
					cell, _ := configDependencyMacroSnapshotCellAt(index)
					snapshot, valid := newConfigDependencyMacroSnapshot().withCell(name, cell)
					if !valid {
						t.Fatal("invalid fixture cell")
					}
					_, _ = snapshot.lookup(name)
					_, _ = snapshot.conditionalProjection()
					got, valid := requireConfigDependencyDefinednessBatchParity(t, configDependencyDefinednessStateForBatch(snapshot), map[string]bool{name: defined}, "measured")
					if valid != (cell.definition != configDependencyMacroDefined || defined) {
						t.Fatal("positive compiler fact conflict changed")
					}
					if valid {
						want := configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined}
						if defined {
							want.definition = configDependencyMacroDefined
							want.recorded = name == configDependencyResolvedAutoconfGuard
						}
						if actual, _ := got.snapshot.lookup(name); actual != want {
							t.Fatalf("measured cell = %#v, want %#v", actual, want)
						}
					}
				})
			}
		}
	}
}

func TestConfigDependencyDefinednessBatchAtomicValidationAndIdentity(t *testing.T) {
	original, reason := parseConfigDependencyCompilerPredefinesWithReplacements("#define __DUMPED 1\n#define ALIAS CONFIG_RETAINED\n", true)
	if reason != "" {
		t.Fatal(reason)
	}
	for _, name := range []string{"__DUMPED", "A_VALID", "__UNMEASURED"} {
		_, _ = original.snapshot.lookup(name)
	}
	_, _ = original.snapshot.conditionalProjection()
	for _, invalid := range []string{"", "9INVALID", "Z-BAD", "Z trailing", "Z\x00BAD", "Zé"} {
		t.Run(fmt.Sprintf("invalid_%q", invalid), func(t *testing.T) {
			// A_VALID sorts before the late invalid entries: no partially
			// published prefix or memo mutation is allowed on either ordering.
			if _, valid := requireConfigDependencyDefinednessBatchParity(t, original, map[string]bool{"A_VALID": true, invalid: false}, "measured"); valid {
				t.Fatal("invalid measured name accepted")
			}
		})
	}
	if _, valid := requireConfigDependencyDefinednessBatchParity(t, original, map[string]bool{"A_VALID": true, "__DUMPED": false}, "conflict"); valid {
		t.Fatal("positive-to-negative compiler conflict accepted")
	}
	for _, definitions := range []map[string]bool{nil, {}, {"INVALID-NAME": true, "__DUMPED": false}} {
		requireConfigDependencyDefinednessBatchParity(t, original, definitions, "")
	}
	for _, definitions := range []map[string]bool{nil, {}} {
		got, valid := requireConfigDependencyDefinednessBatchParity(t, original, definitions, "empty measurement")
		wantDigest := sha256.Sum256(append(slices.Clone(original.compilerPredefinedDigest[:]), []byte("empty measurement")...))
		if !valid || got.compilerPredefinedDigest != wantDigest {
			t.Fatal("empty named measurement lost its digest contribution")
		}
	}
	for _, test := range []struct {
		definitions map[string]bool
		identity    string
	}{
		{nil, ""}, {nil, "empty measurement"}, {map[string]bool{"__ANY": false}, "measured"},
	} {
		requireConfigDependencyDefinednessBatchParity(t, configDependencyMacroState{}, test.definitions, test.identity)
	}
	first, valid := requireConfigDependencyDefinednessBatchParity(t, original, map[string]bool{"__NEW": true}, "first round")
	if !valid {
		t.Fatal("first measured round rejected")
	}
	if _, valid := requireConfigDependencyDefinednessBatchParity(t, first, map[string]bool{"__NEW": false}, "second round"); valid {
		t.Fatal("successive measured rounds lost positive-to-negative conflict")
	}
}

func TestConfigDependencyDefinednessBatchSharesOnlyImmutableState(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	for _, name := range []string{"__RETAINED", "CONFIG_RETAINED", "ORDINARY_RETAINED"} {
		snapshot, _ = snapshot.withCell(name, configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
	}
	original := configDependencyDefinednessStateForBatch(snapshot)
	_, _ = original.snapshot.lookup("__NEGATIVE")
	projection, valid := original.snapshot.conditionalProjection()
	if !valid {
		t.Fatal("fixture projection is invalid")
	}
	clone := original
	sharedTrees := configDependencyDefinednessCloneRoot(original)
	before := configDependencyDefinednessSnapshotImage(original.snapshot)
	derived, valid := requireConfigDependencyDefinednessBatchParity(t, original,
		map[string]bool{"__NEGATIVE": false, "ORDINARY": true, "CONFIG_ANSWER": true}, "measured")
	if !valid || derived.snapshot == original.snapshot {
		t.Fatal("batch did not publish an independent snapshot")
	}
	derived.set("__AFTER", configDependencyMacroDefined)
	for _, state := range []configDependencyMacroState{original, clone, sharedTrees} {
		if !reflect.DeepEqual(before, configDependencyDefinednessSnapshotImage(state.snapshot)) {
			t.Fatal("measured batch or later mutation changed a shared ancestor")
		}
		got, ok := state.snapshot.conditionalProjection()
		if !ok || !reflect.DeepEqual(got, projection) {
			t.Fatal("shared primed projection changed")
		}
		if cell, _ := state.snapshot.lookup("__NEGATIVE"); cell.definition != configDependencyMacroUnknown {
			t.Fatal("original memo borrowed a measured negative fact")
		}
	}
}

func TestConfigDependencyDefinednessBatchSeededTransformsAndJoins(t *testing.T) {
	random := rand.New(rand.NewPCG(0xdef1, 0xb47c))
	names := []string{configDependencyResolvedAutoconfGuard}
	for index := range 96 {
		for _, prefix := range []string{"ORDINARY_", "CONFIG_", "__RESERVED_"} {
			names = append(names, fmt.Sprintf("%s%d", prefix, index))
		}
	}
	write := func(snapshot *configDependencyMacroSnapshot) *configDependencyMacroSnapshot {
		for range 20 {
			cell, _ := configDependencyMacroSnapshotCellAt(random.IntN(configDependencyMacroSnapshotCellCount))
			var valid bool
			snapshot, valid = snapshot.withCell(names[random.IntN(len(names))], cell)
			if !valid {
				t.Fatal("invalid random fixture cell")
			}
		}
		return snapshot
	}
	accepted, rejected := 0, 0
	for iteration := range 160 {
		snapshot := write(newConfigDependencyMacroSnapshot())
		if iteration%2 == 0 {
			snapshot, _ = snapshot.withValidatedNumericMacroHeader()
		}
		if iteration%3 == 0 {
			left, right := write(snapshot), write(snapshot)
			right, _ = right.withValidatedNumericMacroHeader()
			snapshot, _ = configDependencyJoinMacroSnapshots(left, right)
		}
		answers := map[string]bool{"__FRESH": random.IntN(2) == 0}
		for range 15 {
			name := names[random.IntN(len(names))]
			cell, _ := snapshot.lookup(name) // Prime the memo before batching.
			answers[name] = random.IntN(2) == 0 || iteration%2 == 0 && cell.definition == configDependencyMacroDefined
		}
		if iteration%4 == 0 {
			// Enough effective changes in each namespace to force the dense
			// path even when some existing cells already match the measurement.
			for _, name := range names {
				cell, _ := snapshot.lookup(name)
				answers[name] = !strings.HasPrefix(name, "_") || cell.definition == configDependencyMacroDefined
			}
		}
		_, _ = snapshot.conditionalProjection()
		if iteration%13 == 0 {
			answers = nil
		} else if iteration%19 == 0 {
			answers["Z-INVALID"] = true
		}
		identity := fmt.Sprintf("measured-%d", iteration)
		if iteration%17 == 0 {
			identity = ""
		}
		state := configDependencyDefinednessStateForBatch(snapshot)
		state.compilerPredefinedDigest = sha256.Sum256([]byte(identity + " initial"))
		_, valid := requireConfigDependencyDefinednessBatchParity(t, state, answers, identity)
		if valid {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("property cases did not exercise both outcomes: %d accepted, %d rejected", accepted, rejected)
	}
}

func TestConfigDependencyDefinednessBatchThresholdsAndDensityCap(t *testing.T) {
	for _, changes := range []int{63, 64} {
		for _, existing := range []int{4 * changes, 4*changes + 1} {
			t.Run(fmt.Sprintf("changes%d/existing%d", changes, existing), func(t *testing.T) {
				snapshot := newConfigDependencyMacroSnapshot()
				answers := map[string]bool{}
				for index := range existing {
					name := fmt.Sprintf("__EXISTING_%04d", index)
					snapshot, _ = snapshot.withCell(name, configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined})
					if index < changes {
						answers[name] = true
					}
				}
				if _, valid := requireConfigDependencyDefinednessBatchParity(t, configDependencyDefinednessStateForBatch(snapshot), answers, "density boundary"); !valid {
					t.Fatal("valid density fixture rejected")
				}
			})
		}
	}
}

func TestConfigDependencyDefinednessBatchDenseFallbackDeletion(t *testing.T) {
	for _, count := range []int{63, 64, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			snapshot := newConfigDependencyMacroSnapshot()
			answers := map[string]bool{}
			for index := range count {
				name := fmt.Sprintf("ORDINARY_%04d", index)
				snapshot, _ = snapshot.withCell(name, configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown, recorded: true})
				answers[name] = false
			}
			got, valid := requireConfigDependencyDefinednessBatchParity(t, configDependencyDefinednessStateForBatch(snapshot), answers, "measured absent")
			if !valid || got.snapshot.ordinary.facts.root != nil {
				t.Fatal("measuring every explicit cell at fallback left stored facts")
			}
		})
	}
}

func configDependencyDefinednessTreeNodes(tree configDependencyMacroSnapshotTreeView, nodes map[*configDependencyMacroSnapshotTree]bool) {
	if tree.root == nil {
		return
	}
	nodes[tree.root] = true
	configDependencyDefinednessTreeNodes(tree.root.left, nodes)
	configDependencyDefinednessTreeNodes(tree.root.right, nodes)
}

func TestConfigDependencyDefinednessBatchNoOpNamesStaySparse(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	answers := map[string]bool{}
	for index := range 128 {
		name := fmt.Sprintf("__EXISTING_%04d", index)
		snapshot, _ = snapshot.withCell(name, configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
		answers[name] = true
	}
	original := configDependencyDefinednessStateForBatch(snapshot)
	unchanged, valid := requireConfigDependencyDefinednessBatchParity(t, original, answers, "identical measured values")
	if !valid || unchanged.snapshot != snapshot {
		t.Fatal("no-op answers rebuilt the snapshot")
	}
	answers["__NEW"] = false
	got, valid := requireConfigDependencyDefinednessBatchParity(t, original, answers, "one effective answer")
	if !valid {
		t.Fatal("one effective answer rejected")
	}
	want, _ := configDependencyApplyCompilerDefinednessOldReference(configDependencyDefinednessCloneRoot(original), answers, "one effective answer")
	oldNodes, gotNodes, wantNodes := map[*configDependencyMacroSnapshotTree]bool{}, map[*configDependencyMacroSnapshotTree]bool{}, map[*configDependencyMacroSnapshotTree]bool{}
	configDependencyDefinednessTreeNodes(snapshot.reserved.facts, oldNodes)
	configDependencyDefinednessTreeNodes(got.snapshot.reserved.facts, gotNodes)
	configDependencyDefinednessTreeNodes(want.snapshot.reserved.facts, wantNodes)
	for node := range oldNodes {
		if gotNodes[node] != wantNodes[node] {
			t.Fatal("ineffective answers changed sparse persistent subtree sharing")
		}
	}
}

func TestConfigDependencyDefinednessBatchCartesianPriorityTies(t *testing.T) {
	namespace := configDependencyMacroSnapshotNamespace{fallback: configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined}}
	var changes []configDependencyMacroSnapshotEntry
	for index := range 64 {
		name := fmt.Sprintf("ORDINARY_%04d", index)
		// Equal priorities force the secondary name order. Each increasing
		// name is the next root, retaining an explicit valid collision tree.
		namespace.facts = configDependencyMacroSnapshotNewTree(name,
			configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown}, 42,
			namespace.facts, configDependencyMacroSnapshotTreeView{})
		changes = append(changes, configDependencyMacroSnapshotEntry{name: name,
			cell: configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined}})
	}
	want := configDependencyMacroSnapshotNamespace{fallback: namespace.fallback}
	for _, change := range changes {
		want.facts = configDependencyMacroSnapshotNewTree(change.name, change.cell, 42,
			want.facts, configDependencyMacroSnapshotTreeView{})
	}
	got := namespace.withSortedCells(changes)
	requireConfigDependencyDefinednessTreap(t, got.facts, "", "")
	if !reflect.DeepEqual(got, want) {
		t.Fatal("dense builder changed the deterministic equal-priority tree")
	}
	if cell := namespace.lookup(changes[0].name); cell.definition != configDependencyMacroUnknown {
		t.Fatal("dense collision construction mutated the prior tree")
	}
}

func configDependencyDefinednessOrderFixture(t testing.TB) (configDependencyMacroState, map[string]bool) {
	t.Helper()
	snapshot := newConfigDependencyMacroSnapshot()
	answers := map[string]bool{}
	for index := range 96 {
		for _, prefix := range []string{"ORDINARY_", "CONFIG_", "__RESERVED_"} {
			name := fmt.Sprintf("%s%03d", prefix, index)
			answers[name] = index%2 == 0
			cell := configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown}
			if index%3 == 0 {
				// Leave 32 no-ops and 64 effective updates in each namespace.
				cell.definition = configDependencyMacroUndefined
				if answers[name] {
					cell.definition = configDependencyMacroDefined
				}
			}
			var valid bool
			snapshot, valid = snapshot.withCell(name, cell)
			if !valid {
				t.Fatal("invalid insertion-order fixture cell")
			}
		}
	}
	var valid bool
	snapshot, valid = snapshot.withCell(configDependencyResolvedAutoconfGuard,
		configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown, recorded: true})
	if !valid {
		t.Fatal("invalid insertion-order guard fixture")
	}
	answers[configDependencyResolvedAutoconfGuard] = false
	for _, name := range []string{"ORDINARY_000", "CONFIG_001", "__RESERVED_002", configDependencyResolvedAutoconfGuard} {
		_, _ = snapshot.lookup(name)
	}
	_, _ = snapshot.conditionalProjection()
	return configDependencyDefinednessStateForBatch(snapshot), answers
}

func TestConfigDependencyDefinednessBatchInsertionOrder(t *testing.T) {
	original, definitions := configDependencyDefinednessOrderFixture(t)
	names := slices.Sorted(maps.Keys(definitions))
	random := rand.New(rand.NewPCG(0x0ade, 0xdef1))
	var canonical configDependencyMacroState
	for iteration := range 12 {
		order := slices.Clone(names)
		if iteration == 1 {
			slices.Reverse(order)
		} else if iteration > 1 {
			random.Shuffle(len(order), func(left, right int) {
				order[left], order[right] = order[right], order[left]
			})
		}
		// Insertion order does not fix Go map traversal. Reconstruct and
		// apply repeatedly, checking both semantics and canonical tree shape.
		answers := make(map[string]bool, len(order))
		for _, name := range order {
			answers[name] = definitions[name]
		}
		got, valid := requireConfigDependencyDefinednessBatchParity(t, original, answers, "same measured identity")
		if !valid {
			t.Fatal("valid insertion-order batch rejected")
		}
		if iteration == 0 {
			canonical = got
			continue
		}
		if got.compilerPredefinedDigest != canonical.compilerPredefinedDigest ||
			!reflect.DeepEqual(got.snapshot.ordinary, canonical.snapshot.ordinary) ||
			!reflect.DeepEqual(got.snapshot.config, canonical.snapshot.config) ||
			!reflect.DeepEqual(got.snapshot.reserved, canonical.snapshot.reserved) {
			t.Fatal("answer insertion order changed namespace shape or witness digest")
		}
	}
}

func TestConfigDependencyDefinednessBatchRejectsUnorderedPrefixes(t *testing.T) {
	original, definitions := configDependencyDefinednessOrderFixture(t)
	for _, rejected := range []string{"0INVALID", "ZZ-INVALID", "ORDINARY_000", "CONFIG_000", "__RESERVED_000"} {
		t.Run(rejected, func(t *testing.T) {
			answers := maps.Clone(definitions)
			// The last three entries conflict with original positive facts.
			// Every batch also contains a large valid, mixed-namespace prefix.
			answers[rejected] = false
			names := slices.Sorted(maps.Keys(answers))
			random := rand.New(rand.NewPCG(0xabad, 0xdef1))
			for iteration := range 12 {
				order := slices.Clone(names)
				if iteration == 1 {
					slices.Reverse(order)
				} else if iteration > 1 {
					random.Shuffle(len(order), func(left, right int) {
						order[left], order[right] = order[right], order[left]
					})
				}
				reordered := make(map[string]bool, len(order))
				for _, name := range order {
					reordered[name] = answers[name]
				}
				if _, valid := requireConfigDependencyDefinednessBatchParity(t, original, reordered, "rejected measurement"); valid {
					t.Fatal("invalid or conflicting batch published a successful prefix")
				}
			}
		})
	}
}

var configDependencyDefinednessBatchBenchmarkSink configDependencyMacroState

func BenchmarkConfigDependencyDefinednessBatch(b *testing.B) {
	for _, fixture := range []struct {
		name                string
		predefines, answers int
		mode                string
	}{
		{"4096_negatives_256_predefines", 256, 4096, ""},
		{"1_negative_8192_predefines", 8192, 1, ""},
		{"4096_noops_256_predefines", 256, 4096, "noop"},
		{"4096_mixed_256_predefines", 256, 4096, "mixed"},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			var dump strings.Builder
			for index := range fixture.predefines {
				fmt.Fprintf(&dump, "#define __PREDEFINED_%05d 1\n", index)
			}
			state, reason := parseConfigDependencyCompilerPredefines(dump.String())
			if reason != "" {
				b.Fatal(reason)
			}
			answers := make(map[string]bool, fixture.answers)
			for index := range fixture.answers {
				prefix, defined := "__MEASURED_", false
				if fixture.mode == "mixed" {
					prefix = []string{"ORDINARY_MEASURED_", "CONFIG_MEASURED_", "__MEASURED_"}[index%3]
					defined = index%2 == 0
				}
				answers[fmt.Sprintf("%s%05d", prefix, index)] = defined
			}
			if fixture.mode == "noop" {
				var valid bool
				state, valid = requireConfigDependencyDefinednessBatchParity(b, state, answers, "seed matching measured facts")
				if !valid {
					b.Fatal("no-op benchmark setup failed semantic preflight")
				}
			}
			identity := configDependencyCompilerDefinednessIdentity(slices.Sorted(maps.Keys(answers)), answers, true)
			measured, valid := requireConfigDependencyDefinednessBatchParity(b, state, answers, identity)
			if !valid {
				b.Fatal("benchmark fixture failed semantic preflight")
			}
			if fixture.mode == "noop" && measured.snapshot != state.snapshot {
				b.Fatal("no-op benchmark fixture rebuilt its snapshot")
			}
			// Compare production before/after the allocation-only change.
			// The old point-write reference is not the current batch baseline.
			for _, implementation := range []struct {
				name  string
				apply func(configDependencyMacroState, map[string]bool, string) (configDependencyMacroState, bool)
			}{
				{"old_reference", configDependencyApplyCompilerDefinednessOldReference},
				{"production", configDependencyApplyCompilerDefinedness},
			} {
				b.Run(implementation.name, func(b *testing.B) {
					input := configDependencyDefinednessCloneRoot(state)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						var valid bool
						configDependencyDefinednessBatchBenchmarkSink, valid = implementation.apply(input, answers, identity)
						if !valid {
							b.Fatal("measured answers rejected")
						}
					}
				})
			}
		})
	}
}
