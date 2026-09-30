package kconfig

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"unsafe"
)

func configDependencyMacroSnapshotTestCell(
	definition configDependencyMacroDefinition,
	recorded bool,
) configDependencyMacroSnapshotCell {
	return configDependencyMacroSnapshotCell{definition: definition, recorded: recorded}
}

func requireConfigDependencyMacroSnapshotCell(
	t *testing.T,
	snapshot *configDependencyMacroSnapshot,
	name string,
	want configDependencyMacroSnapshotCell,
) {
	t.Helper()
	got, valid := snapshot.lookup(name)
	if !valid || got != want {
		t.Fatalf("snapshot %q = %#v, valid=%t, want %#v", name, got, valid, want)
	}
}

func TestConfigDependencyMacroSnapshotAbsentAndExactCells(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	requireConfigDependencyMacroSnapshotCell(t, snapshot, "CONFIG_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
	requireConfigDependencyMacroSnapshotCell(t, snapshot, "ORDINARY_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
	requireConfigDependencyMacroSnapshotCell(t, snapshot, "_RESERVED_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))

	base := snapshot
	for _, test := range []struct {
		name       string
		definition configDependencyMacroDefinition
	}{
		{name: "CONFIG_EXACT_UNKNOWN", definition: configDependencyMacroUnknown},
		{name: "ORDINARY_EXACT_UNDEFINED", definition: configDependencyMacroUndefined},
		{name: "_RESERVED_EXACT_DEFINED", definition: configDependencyMacroDefined},
	} {
		var valid bool
		snapshot, valid = snapshot.withDefinition(test.name, test.definition)
		if !valid {
			t.Fatalf("withDefinition(%q) rejected a valid definition", test.name)
		}
		requireConfigDependencyMacroSnapshotCell(t, snapshot, test.name,
			configDependencyMacroSnapshotTestCell(test.definition, true))
	}

	// Persistent updates must not change the snapshot from which they branch.
	requireConfigDependencyMacroSnapshotCell(t, base, "CONFIG_EXACT_UNKNOWN",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
	if _, valid := snapshot.withDefinition("", configDependencyMacroDefined); valid {
		t.Fatal("empty exact macro name was accepted")
	}
	if _, valid := snapshot.withDefinition("INVALID", configDependencyMacroDefinition(7)); valid {
		t.Fatal("invalid macro definition was accepted")
	}
}

func TestConfigDependencyMacroSnapshotNumericMaybeDefineAllCells(t *testing.T) {
	cells := []configDependencyMacroSnapshotCell{}
	for _, definition := range []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	} {
		for _, recorded := range []bool{false, true} {
			cells = append(cells, configDependencyMacroSnapshotTestCell(definition, recorded))
		}
	}

	snapshot := newConfigDependencyMacroSnapshot()
	for index, cell := range cells {
		var valid bool
		snapshot, valid = snapshot.withCell(fmt.Sprintf("ORDINARY_%d", index), cell)
		if !valid {
			t.Fatalf("ordinary cell %d was rejected", index)
		}
		snapshot, valid = snapshot.withCell(fmt.Sprintf("_RESERVED_%d", index), cell)
		if !valid {
			t.Fatalf("reserved cell %d was rejected", index)
		}
		snapshot, valid = snapshot.withCell(fmt.Sprintf("CONFIG_CONTROL_%d", index), cell)
		if !valid {
			t.Fatalf("CONFIG cell %d was rejected", index)
		}
	}

	before := snapshot
	after, valid := snapshot.withValidatedNumericMacroHeader()
	if !valid {
		t.Fatal("numeric maybe-define rejected a valid snapshot")
	}
	for index, cell := range cells {
		want := configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true)
		if cell.definition == configDependencyMacroDefined {
			want.definition = configDependencyMacroDefined
		}
		requireConfigDependencyMacroSnapshotCell(t, after, fmt.Sprintf("ORDINARY_%d", index), want)
		requireConfigDependencyMacroSnapshotCell(t, after, fmt.Sprintf("_RESERVED_%d", index), want)
		requireConfigDependencyMacroSnapshotCell(t, after, fmt.Sprintf("CONFIG_CONTROL_%d", index), cell)
	}
	requireConfigDependencyMacroSnapshotCell(t, after, "ORDINARY_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	requireConfigDependencyMacroSnapshotCell(t, after, "_RESERVED_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	requireConfigDependencyMacroSnapshotCell(t, after, "CONFIG_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))

	// The namespace-wide transform is a tagged view: applying it cannot rewrite
	// or detach the complete existing tree.
	if before.ordinary.facts.root != after.ordinary.facts.root ||
		before.reserved.facts.root != after.reserved.facts.root {
		t.Fatal("numeric maybe-define did not retain non-CONFIG tree roots")
	}
}

func TestConfigDependencyMacroSnapshotPackedTransformAlgebra(t *testing.T) {
	cells := []configDependencyMacroSnapshotCell{}
	for _, definition := range []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	} {
		for _, recorded := range []bool{false, true} {
			cells = append(cells, configDependencyMacroSnapshotTestCell(definition, recorded))
		}
	}
	type transformCase struct {
		name  string
		value configDependencyMacroSnapshotTransform
		apply func(configDependencyMacroSnapshotCell) configDependencyMacroSnapshotCell
	}
	transforms := []transformCase{
		{
			name:  "identity",
			value: configDependencyMacroSnapshotIdentityTransform(),
			apply: func(cell configDependencyMacroSnapshotCell) configDependencyMacroSnapshotCell { return cell },
		},
		{
			name:  "maybe-define",
			value: configDependencyMacroSnapshotMaybeDefineTransform(),
			apply: configDependencyMacroSnapshotMaybeDefineCell,
		},
	}
	for index, fallback := range cells {
		fallback := fallback
		transforms = append(transforms,
			transformCase{
				name:  fmt.Sprintf("join-left-fallback-%d", index),
				value: configDependencyMacroSnapshotJoinLeftFallbackTransform(fallback),
				apply: func(cell configDependencyMacroSnapshotCell) configDependencyMacroSnapshotCell {
					return configDependencyMacroSnapshotJoinCells(fallback, cell)
				},
			},
			transformCase{
				name:  fmt.Sprintf("join-right-fallback-%d", index),
				value: configDependencyMacroSnapshotJoinRightFallbackTransform(fallback),
				apply: func(cell configDependencyMacroSnapshotCell) configDependencyMacroSnapshotCell {
					return configDependencyMacroSnapshotJoinCells(cell, fallback)
				},
			},
		)
	}
	for _, transform := range transforms {
		for _, cell := range cells {
			got, valid := configDependencyMacroSnapshotApplyTransform(transform.value, cell)
			if want := transform.apply(cell); !valid || got != want {
				t.Errorf("%s(%#v) = %#v, valid=%t, want %#v", transform.name, cell, got, valid, want)
			}
		}
	}
	for _, outer := range transforms {
		for _, inner := range transforms {
			composed := configDependencyMacroSnapshotComposeTransforms(outer.value, inner.value)
			joined := configDependencyMacroSnapshotJoinTransforms(outer.value, inner.value)
			for _, cell := range cells {
				got, valid := configDependencyMacroSnapshotApplyTransform(composed, cell)
				if want := outer.apply(inner.apply(cell)); !valid || got != want {
					t.Errorf("compose(%s,%s)(%#v) = %#v, valid=%t, want %#v", outer.name, inner.name, cell, got, valid, want)
				}
				got, valid = configDependencyMacroSnapshotApplyTransform(joined, cell)
				if want := configDependencyMacroSnapshotJoinCells(outer.apply(cell), inner.apply(cell)); !valid || got != want {
					t.Errorf("join(%s,%s)(%#v) = %#v, valid=%t, want %#v", outer.name, inner.name, cell, got, valid, want)
				}
			}
		}
	}

	invalid := configDependencyMacroSnapshotIdentityTransform()
	invalid[0] = configDependencyMacroSnapshotCellCount
	if _, valid := configDependencyMacroSnapshotApplyTransform(invalid, cells[0]); valid {
		t.Fatal("packed transform accepted an invalid cell index")
	}
	if got := configDependencyMacroSnapshotApplyKnownTransform(invalid, cells[0]); got !=
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true) {
		t.Fatalf("invalid packed transform fallback = %#v", got)
	}
}

func TestConfigDependencyMacroSnapshotPackedTreeLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(configDependencyMacroSnapshotTransform{}), uintptr(configDependencyMacroSnapshotCellCount); got != want {
		t.Fatalf("packed transform size = %d, want %d", got, want)
	}
	if unsafe.Sizeof(uintptr(0)) == 8 {
		if got := unsafe.Sizeof(configDependencyMacroSnapshotTree{}); got != 64 {
			t.Fatalf("64-bit snapshot tree node size = %d, want 64", got)
		}
	}
}

// The checkpoint oracle keeps the complete namespace eager and replays each
// write through immutable snapshot primitives. It never calls a mutable state's
// materializedSnapshot, putSnapshotCell, commitBranch, or applySnapshotChanges.
type configDependencyMacroCheckpointEagerOracle struct {
	base, current *configDependencyMacroSnapshot
	changes       configDependencyMacroSnapshotChangeSet
}

func newConfigDependencyMacroCheckpointEagerOracle(snapshot *configDependencyMacroSnapshot) *configDependencyMacroCheckpointEagerOracle {
	return &configDependencyMacroCheckpointEagerOracle{base: snapshot, current: snapshot}
}

func (oracle *configDependencyMacroCheckpointEagerOracle) put(t *testing.T, name string, cell configDependencyMacroSnapshotCell) {
	t.Helper()
	var valid bool
	oracle.current, valid = oracle.current.withCell(name, cell)
	if !valid || !oracle.changes.putNormalized(oracle.base, name, cell) {
		t.Fatal("eager exact write failed")
	}
}

func (oracle *configDependencyMacroCheckpointEagerOracle) wildcard(t *testing.T) {
	t.Helper()
	var valid bool
	oracle.current, valid = configDependencyMacroStateWithValidatedNumericMacroHeader(oracle.current)
	if !valid || !oracle.changes.applyValidatedNumericMacroHeaderNormalized(oracle.base) {
		t.Fatal("eager wildcard failed")
	}
}

func (oracle *configDependencyMacroCheckpointEagerOracle) branch() *configDependencyMacroCheckpointEagerOracle {
	return newConfigDependencyMacroCheckpointEagerOracle(oracle.current)
}

func (oracle *configDependencyMacroCheckpointEagerOracle) apply(t *testing.T, changes configDependencyMacroSnapshotChangeSet) {
	t.Helper()
	if changes.nonConfigWildcard {
		oracle.wildcard(t)
	}
	changes.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		oracle.put(t, name, cell)
	})
}

func (oracle *configDependencyMacroCheckpointEagerOracle) join(t *testing.T, left, right *configDependencyMacroCheckpointEagerOracle) {
	t.Helper()
	leftChanges, rightChanges := configDependencyMacroSnapshotChangeSet{}, configDependencyMacroSnapshotChangeSet{}
	if left != nil {
		leftChanges = left.changes
	}
	if right != nil {
		rightChanges = right.changes
	}
	// Cell algebra and normalization are unchanged by the checkpoint patch.
	// Keep the oracle's poststate eager instead of using the mutable join path.
	joined := configDependencyMacroSnapshotChangeSet{
		nonConfigWildcard: leftChanges.nonConfigWildcard || rightChanges.nonConfigWildcard,
	}
	visit := func(name string, _ configDependencyMacroSnapshotCell) {
		leftCell, leftValid := leftChanges.cell(oracle.current, name)
		rightCell, rightValid := rightChanges.cell(oracle.current, name)
		cell := configDependencyMacroStateCanonicalCell(name, configDependencyMacroSnapshotJoinCells(leftCell, rightCell))
		if !leftValid || !rightValid || !joined.putNormalized(oracle.current, name, cell) {
			t.Fatal("eager join failed")
		}
	}
	leftChanges.forEach(visit)
	rightChanges.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		if _, found := leftChanges.lookup(name); !found {
			visit(name, cell)
		}
	})
	oracle.apply(t, joined)
}

func requireConfigDependencyMacroCheckpointMatchesEager(t *testing.T, state *configDependencyMacroState, oracle *configDependencyMacroCheckpointEagerOracle) {
	t.Helper()
	// Inspect the complete normalized overlay without flushing the checkpoint.
	// Flushing here would hide multi-write suffix bugs from every later fork.
	visible, valid := state.snapshotChanges.materialize(state.snapshot)
	if !valid || state.tainted || state.consumed {
		t.Fatal("invalid deferred macro state")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, visible, oracle.current)
	for _, kind := range []configDependencyMacroSnapshotNamespaceKind{
		configDependencyMacroSnapshotOrdinaryNamespace, configDependencyMacroSnapshotConfigNamespace, configDependencyMacroSnapshotReservedNamespace,
	} {
		configDependencyMacroSnapshotWalkTree(oracle.current.namespace(kind).facts, func(name string, want configDependencyMacroSnapshotCell) {
			got, valid := state.snapshotCell(name)
			if !valid || got != want {
				t.Fatalf("unflushed read %s = %#v/%t, want %#v", name, got, valid, want)
			}
		})
	}
	for _, name := range []string{"UNMENTIONED_CHECKPOINT", "CONFIG_UNMENTIONED_CHECKPOINT", "_UNMENTIONED_CHECKPOINT"} {
		want, _ := oracle.current.lookup(name)
		got, valid := state.snapshotCell(name)
		if !valid || got != want {
			t.Fatalf("unflushed fallback %s = %#v/%t, want %#v", name, got, valid, want)
		}
	}
}

func flushConfigDependencyMacroCheckpointForTest(t *testing.T, state *configDependencyMacroState, oracle *configDependencyMacroCheckpointEagerOracle) *configDependencyMacroSnapshot {
	t.Helper()
	revision := state.snapshotRevision
	result, valid := state.materializedSnapshot()
	if !valid || !state.materializedSnapshotPending.empty() || state.snapshotRevision != revision {
		t.Fatal("checkpoint flush changed revision or retained a pending suffix")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, result, oracle.current)
	again, valid := state.materializedSnapshot()
	if !valid || again != result || state.snapshotRevision != revision {
		t.Fatal("unchanged checkpoint flush did not retain its identity")
	}
	return result
}

func TestConfigDependencyMacroStateCheckpointSuffixMatchesEagerOracle(t *testing.T) {
	base := configDependencyMacroEffectInputForTest()
	state := (&configDependencyMacroState{snapshot: base}).branch()
	oracle := newConfigDependencyMacroCheckpointEagerOracle(base)
	checkpoint := flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
	checkpointValue := oracle.current
	random := rand.New(rand.NewSource(0x51ff17))
	var revisions uint64
	for step := 0; step < 180; step++ {
		if step%13 == 0 {
			if !state.applySnapshotValidatedNumericMacroHeader() {
				t.Fatal("deferred wildcard failed")
			}
			oracle.wildcard(t)
		} else {
			prefix := []string{"ORDINARY_", "CONFIG_", "_RESERVED_"}[random.Intn(3)]
			name := fmt.Sprintf("%s%d", prefix, random.Intn(configDependencyMacroSnapshotCellCount))
			if step%19 == 0 {
				name = configDependencyResolvedAutoconfGuard
			}
			cell, _ := configDependencyMacroSnapshotCellAt(random.Intn(configDependencyMacroSnapshotCellCount))
			if step%7 == 0 {
				cell, _ = oracle.current.lookup(name)
			}
			if name == configDependencyResolvedAutoconfGuard {
				// Real explicit guard writes always record provenance. The raw
				// six-cell/equal-write boundary is covered separately below.
				cell.recorded = true
			}
			if !state.putSnapshotCell(name, cell) {
				t.Fatal("deferred exact write failed")
			}
			oracle.put(t, name, cell)
		}
		revisions++
		if state.snapshotRevision != revisions || state.materializedSnapshotCache != checkpoint {
			t.Fatal("write changed the checkpoint or lost its revision increment")
		}
		requireConfigDependencyMacroCheckpointMatchesEager(t, state, oracle)
		requireConfigDependencyMacroEffectSnapshotsEqual(t, checkpoint, checkpointValue)
		if step%12 == 11 {
			checkpoint = flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
			checkpointValue = oracle.current
		}
	}
	flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
}

func TestConfigDependencyMacroStateCheckpointInterleavedSiblingForks(t *testing.T) {
	base := configDependencyMacroEffectInputForTest()
	state := (&configDependencyMacroState{snapshot: base}).branch()
	oracle := newConfigDependencyMacroCheckpointEagerOracle(base)
	flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
	for round := 0; round < 24; round++ {
		checkpoint, checkpointValue := state.materializedSnapshotCache, oracle.current
		name := fmt.Sprintf("PARENT_CHECKPOINT_%d", round%3)
		cell, _ := configDependencyMacroSnapshotCellAt(round % configDependencyMacroSnapshotCellCount)
		if !state.putSnapshotCell(name, cell) {
			t.Fatal("intervening parent write failed")
		}
		oracle.put(t, name, cell)
		if state.materializedSnapshotCache != checkpoint || state.materializedSnapshotPending.exactSize() != 1 {
			t.Fatal("intervening write did not retain a bounded checkpoint suffix")
		}
		child, childOracle := state.branch(), oracle.branch()
		if child == nil || child.snapshot != state.materializedSnapshotCache || !state.materializedSnapshotPending.empty() {
			t.Fatal("sibling fork did not flush only the current suffix")
		}
		requireConfigDependencyMacroEffectSnapshotsEqual(t, child.snapshot, oracle.current)
		requireConfigDependencyMacroEffectSnapshotsEqual(t, checkpoint, checkpointValue)
		for write := 0; write < 8; write++ {
			name := fmt.Sprintf("CHILD_CHECKPOINT_%d", write)
			cell, _ := configDependencyMacroSnapshotCellAt((round + write) % configDependencyMacroSnapshotCellCount)
			if !child.putSnapshotCell(name, cell) {
				t.Fatal("child exact write failed")
			}
			childOracle.put(t, name, cell)
		}
		// A nested fork establishes a child checkpoint, then later writes make
		// that checkpoint stale before either definite commit or optional join.
		grandchild := child.branch()
		if grandchild == nil || !child.commitBranch(grandchild) {
			t.Fatal("empty nested child commit failed")
		}
		for write := 0; write < 3; write++ {
			name := fmt.Sprintf("AFTER_NESTED_CHECKPOINT_%d", write)
			cell := configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined}
			if !child.putSnapshotCell(name, cell) {
				t.Fatal("post-checkpoint write failed")
			}
			childOracle.put(t, name, cell)
		}
		if round%4 == 0 {
			if !child.applySnapshotValidatedNumericMacroHeader() {
				t.Fatal("post-checkpoint wildcard failed")
			}
			childOracle.wildcard(t)
		}
		if child.materializedSnapshotPending.empty() {
			t.Fatal("fixture did not leave a dirty child checkpoint")
		}
		if round%2 == 0 {
			if !state.commitBranch(child) {
				t.Fatal("dirty child checkpoint commit failed")
			}
			oracle.apply(t, childOracle.changes)
		} else {
			state.mergePossibleBranch(child)
			oracle.join(t, nil, childOracle)
		}
		if !child.consumed || !child.materializedSnapshotPending.empty() || state.snapshot != base {
			t.Fatal("child was not consumed or the original parent fork was replaced")
		}
		requireConfigDependencyMacroCheckpointMatchesEager(t, state, oracle)
		flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
	}
}

func TestConfigDependencyMacroStateCheckpointEqualAndNormalizedAwayWrites(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	state := root.branch()
	if !state.beginForcedHeaderTrace() {
		t.Fatal("trace initialization failed")
	}
	state.set("CHECKPOINT_UNDO", configDependencyMacroDefined)
	checkpoint, valid := state.materializedSnapshot()
	if !valid {
		t.Fatal("initial checkpoint failed")
	}
	state.set("CHECKPOINT_UNDO", configDependencyMacroUndefined)
	state.set("CHECKPOINT_UNDO", configDependencyMacroUndefined)
	pending, found := state.materializedSnapshotPending.lookup("CHECKPOINT_UNDO")
	if !state.snapshotChanges.empty() || !found || pending.definition != configDependencyMacroUndefined ||
		state.snapshotRevision != 3 || state.materializedSnapshotCache != checkpoint ||
		!state.forcedHeaderTouches.contains("CHECKPOINT_UNDO") {
		t.Fatal("normalized-away/equal writes lost their suffix, revisions, or trace touch")
	}
	result, valid := state.materializedSnapshot()
	if !valid || !state.materializedSnapshotPending.empty() {
		t.Fatal("undo checkpoint flush failed")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, result, root.snapshot)
	requireConfigDependencyMacroSnapshotCell(t, checkpoint, "CHECKPOINT_UNDO",
		configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
	state.endForcedHeaderTrace()
	if !root.commitBranch(state) || root.snapshotRevision != 0 || !state.consumed {
		t.Fatal("net-empty child commit changed parent revision or failed consumption")
	}
}

func TestConfigDependencyMacroStateCheckpointEffectCaptureAndReplay(t *testing.T) {
	base := configDependencyMacroEffectInputForTest()
	parent := (&configDependencyMacroState{snapshot: base}).branch()
	child := parent.branch()
	oracle := newConfigDependencyMacroCheckpointEagerOracle(base)
	flushConfigDependencyMacroCheckpointForTest(t, child, oracle)
	for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
		cell, _ := configDependencyMacroSnapshotCellAt(index)
		name := fmt.Sprintf("EFFECT_CHECKPOINT_%d", index)
		if !child.putSnapshotCell(name, cell) {
			t.Fatal("effect checkpoint write failed")
		}
		oracle.put(t, name, cell)
	}
	if !child.applySnapshotValidatedNumericMacroHeader() {
		t.Fatal("effect checkpoint wildcard failed")
	}
	oracle.wildcard(t)
	child.set(configDependencyResolvedAutoconfGuard, configDependencyMacroUndefined)
	oracle.put(t, configDependencyResolvedAutoconfGuard,
		configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined, recorded: true})
	if child.materializedSnapshotPending.empty() {
		t.Fatal("effect fixture did not retain a pending suffix")
	}
	effect, valid := captureConfigDependencyMacroEffect(child)
	if !valid || !child.materializedSnapshotPending.empty() || effect.input != base {
		t.Fatal("effect capture did not flush the pending suffix")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, effect.output, oracle.current)
	if !parent.commitBranch(child) || parent.materializedSnapshotCache != effect.output || !parent.materializedSnapshotPending.empty() {
		t.Fatal("captured exact poststate was not adopted")
	}
	replay := (&configDependencyMacroState{snapshot: base}).branch()
	if !effect.apply(replay) || replay.materializedSnapshotCache != effect.output || !replay.materializedSnapshotPending.empty() {
		t.Fatal("exact cache replay did not install a clean checkpoint")
	}
	replay.set("EFFECT_CHECKPOINT_2", configDependencyMacroDefined)
	if replay.materializedSnapshotPending.empty() {
		t.Fatal("post-hit write did not become a private suffix")
	}
	retained, valid := effect.changes.materialize(effect.input)
	if !valid {
		t.Fatal("captured effect changes became invalid")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, retained, effect.output)
	requireConfigDependencyMacroEffectSnapshotsEqual(t, parent.materializedSnapshotCache, oracle.current)
	changed := (&configDependencyMacroState{snapshot: base}).branch()
	changed.set("CHANGED_EFFECT_INPUT", configDependencyMacroDefined)
	if effect.apply(changed) || changed.tainted {
		t.Fatal("mismatched exact cache input was accepted or tainted")
	}
}

func TestConfigDependencyMacroStateCheckpointRejectsInvalidLineage(t *testing.T) {
	base := configDependencyMacroEffectInputForTest()
	parent := (&configDependencyMacroState{snapshot: base}).branch()
	child := parent.branch()
	checkpoint := parent.materializedSnapshotCache
	cell, _ := parent.snapshotCell("CONFIG_5")
	if !parent.putSnapshotCell("CONFIG_5", cell) || parent.snapshotRevision != 1 {
		t.Fatal("equal parent write did not advance revision")
	}
	if parent.commitBranch(child) || !parent.tainted || child.consumed {
		t.Fatal("stale child was accepted after an equal-value parent write")
	}
	if _, valid := parent.materializedSnapshot(); valid || parent.materializedSnapshotCache != checkpoint {
		t.Fatal("tainted state published a pending checkpoint")
	}
	state := (&configDependencyMacroState{snapshot: base}).branch()
	state.materializedSnapshot()
	state.set("CONSUMED_CHECKPOINT", configDependencyMacroDefined)
	state.consume()
	if !state.materializedSnapshotPending.empty() || state.putSnapshotCell("AFTER_CONSUME", cell) {
		t.Fatal("consumption retained or revived a pending suffix")
	}
	if _, valid := state.materializedSnapshot(); valid {
		t.Fatal("consumed checkpoint was materialized")
	}
	invalid := &configDependencyMacroState{}
	if _, valid := invalid.materializedSnapshot(); valid || invalid.putSnapshotCell("INVALID", cell) {
		t.Fatal("state without an immutable base was accepted")
	}
}

func TestConfigDependencyMacroStateCheckpointWildcardRecordsEqualAutoconfGuard(t *testing.T) {
	for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
		t.Run(fmt.Sprintf("cell_%d", index), func(t *testing.T) {
			cell, _ := configDependencyMacroSnapshotCellAt(index)
			base, _ := newConfigDependencyMacroSnapshot().withCell(configDependencyResolvedAutoconfGuard, cell)
			state := (&configDependencyMacroState{snapshot: base}).branch()
			oracle := newConfigDependencyMacroCheckpointEagerOracle(base)
			flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
			if !state.putSnapshotCell(configDependencyResolvedAutoconfGuard, cell) || !state.snapshotChanges.empty() {
				t.Fatal("equal guard write did not normalize away")
			}
			oracle.put(t, configDependencyResolvedAutoconfGuard, cell)
			if !state.applySnapshotValidatedNumericMacroHeader() {
				t.Fatal("guard wildcard failed")
			}
			oracle.wildcard(t)
			requireConfigDependencyMacroCheckpointMatchesEager(t, state, oracle)
			result := flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
			requireConfigDependencyMacroSnapshotCell(t, result, configDependencyResolvedAutoconfGuard,
				configDependencyMacroSnapshotMaybeDefineCell(cell))
			if !state.putSnapshotCell(configDependencyResolvedAutoconfGuard, cell) {
				t.Fatal("exact guard write after wildcard failed")
			}
			oracle.put(t, configDependencyResolvedAutoconfGuard, cell)
			requireConfigDependencyMacroCheckpointMatchesEager(t, state, oracle)
			flushConfigDependencyMacroCheckpointForTest(t, state, oracle)
		})
	}
}

func TestConfigDependencyMacroSnapshotPackedTransformsConcurrent(t *testing.T) {
	cells := []configDependencyMacroSnapshotCell{}
	base := newConfigDependencyMacroSnapshot()
	names := make([]string, 0, configDependencyMacroSnapshotCellCount)
	for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
		cell, _ := configDependencyMacroSnapshotCellAt(index)
		cells = append(cells, cell)
		name := fmt.Sprintf("ORDINARY_PACKED_%d", index)
		names = append(names, name)
		var valid bool
		base, valid = base.withCell(name, cell)
		if !valid {
			t.Fatalf("could not seed packed-transform cell %d", index)
		}
	}
	base, valid := base.withValidatedNumericMacroHeader()
	if !valid {
		t.Fatal("could not create lazy transformed snapshot")
	}

	const (
		workers    = 16
		iterations = 64
	)
	errors := make(chan string, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				index := (worker + iteration) % len(names)
				name := names[index]
				inherited := configDependencyMacroSnapshotMaybeDefineCell(cells[index])
				if got, valid := base.lookup(name); !valid || got != inherited {
					errors <- fmt.Sprintf("base %s = %#v, valid=%t, want %#v", name, got, valid, inherited)
					return
				}
				written := cells[(worker*iterations+iteration)%len(cells)]
				updated, valid := base.withCell(name, written)
				if !valid {
					errors <- "concurrent packed-tree update failed"
					return
				}
				joined, valid := configDependencyJoinMacroSnapshots(base, updated)
				if !valid {
					errors <- "concurrent packed-tree join failed"
					return
				}
				want := configDependencyMacroSnapshotJoinCells(inherited, written)
				if got, valid := joined.lookup(name); !valid || got != want {
					errors <- fmt.Sprintf("joined %s = %#v, valid=%t, want %#v", name, got, valid, want)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
}

func TestConfigDependencyMacroSnapshotBulkConfigEffects(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	for name, cell := range map[string]configDependencyMacroSnapshotCell{
		"CONFIG_DEFINED":   configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true),
		"CONFIG_UNDEFINED": configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, true),
		"CONFIG_UNKNOWN":   configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false),
	} {
		var valid bool
		snapshot, valid = snapshot.withCell(name, cell)
		if !valid {
			t.Fatalf("seed %s was rejected", name)
		}
	}

	names := []string{"CONFIG_DEFINED", "CONFIG_UNDEFINED", "CONFIG_UNKNOWN", "CONFIG_ABSENT"}
	defined, valid := snapshot.withConfigDefinitions(names)
	if !valid {
		t.Fatal("CONFIG define set was rejected")
	}
	for _, name := range names {
		requireConfigDependencyMacroSnapshotCell(t, defined, name,
			configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true))
	}

	maybe, valid := snapshot.withMaybeConfigDefinitions(names)
	if !valid {
		t.Fatal("CONFIG maybe-define set was rejected")
	}
	requireConfigDependencyMacroSnapshotCell(t, maybe, "CONFIG_DEFINED",
		configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true))
	for _, name := range []string{"CONFIG_UNDEFINED", "CONFIG_UNKNOWN", "CONFIG_ABSENT"} {
		requireConfigDependencyMacroSnapshotCell(t, maybe, name,
			configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	}

	if _, valid := snapshot.withConfigDefinitions([]string{"CONFIG_OK", "NOT_CONFIG"}); valid {
		t.Fatal("mixed CONFIG/non-CONFIG define set was accepted")
	}
	requireConfigDependencyMacroSnapshotCell(t, snapshot, "CONFIG_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
}

func TestConfigDependencyMacroSnapshotJoinAllCells(t *testing.T) {
	cells := []configDependencyMacroSnapshotCell{}
	for _, definition := range []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	} {
		for _, recorded := range []bool{false, true} {
			cells = append(cells, configDependencyMacroSnapshotTestCell(definition, recorded))
		}
	}

	for leftIndex, leftCell := range cells {
		for rightIndex, rightCell := range cells {
			left, valid := newConfigDependencyMacroSnapshot().withCell("CONFIG_JOIN", leftCell)
			if !valid {
				t.Fatalf("left cell %d was rejected", leftIndex)
			}
			right, valid := newConfigDependencyMacroSnapshot().withCell("CONFIG_JOIN", rightCell)
			if !valid {
				t.Fatalf("right cell %d was rejected", rightIndex)
			}
			joined, valid := configDependencyJoinMacroSnapshots(left, right)
			if !valid {
				t.Fatalf("join (%d,%d) was rejected", leftIndex, rightIndex)
			}
			wantDefinition := configDependencyMacroUnknown
			if leftCell.definition == rightCell.definition {
				wantDefinition = leftCell.definition
			}
			requireConfigDependencyMacroSnapshotCell(t, joined, "CONFIG_JOIN",
				configDependencyMacroSnapshotTestCell(
					wantDefinition, leftCell.recorded || rightCell.recorded,
				))
		}
	}

	base := newConfigDependencyMacroSnapshot()
	if joined, valid := configDependencyJoinMacroSnapshots(base, base); !valid || joined != base {
		t.Fatal("joining an identical branch did not preserve snapshot identity")
	}
	wildcard, valid := base.withValidatedNumericMacroHeader()
	if !valid {
		t.Fatal("could not create wildcard branch")
	}
	joined, valid := configDependencyJoinMacroSnapshots(base, wildcard)
	if !valid {
		t.Fatal("could not join base and wildcard branches")
	}
	requireConfigDependencyMacroSnapshotCell(t, joined, "ORDINARY_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	requireConfigDependencyMacroSnapshotCell(t, joined, "_RESERVED_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	requireConfigDependencyMacroSnapshotCell(t, joined, "CONFIG_ABSENT",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
}

type configDependencyMacroSnapshotOracle struct {
	fallbacks map[configDependencyMacroSnapshotNamespaceKind]configDependencyMacroSnapshotCell
	cells     map[string]configDependencyMacroSnapshotCell
}

func newConfigDependencyMacroSnapshotOracle() *configDependencyMacroSnapshotOracle {
	return &configDependencyMacroSnapshotOracle{
		fallbacks: map[configDependencyMacroSnapshotNamespaceKind]configDependencyMacroSnapshotCell{
			configDependencyMacroSnapshotConfigNamespace: {
				definition: configDependencyMacroUndefined,
			},
			configDependencyMacroSnapshotOrdinaryNamespace: {
				definition: configDependencyMacroUndefined,
			},
			configDependencyMacroSnapshotReservedNamespace: {
				definition: configDependencyMacroUnknown,
			},
		},
		cells: map[string]configDependencyMacroSnapshotCell{},
	}
}

func (oracle *configDependencyMacroSnapshotOracle) clone() *configDependencyMacroSnapshotOracle {
	result := newConfigDependencyMacroSnapshotOracle()
	for kind, fallback := range oracle.fallbacks {
		result.fallbacks[kind] = fallback
	}
	for name, cell := range oracle.cells {
		result.cells[name] = cell
	}
	return result
}

func (oracle *configDependencyMacroSnapshotOracle) lookup(name string) configDependencyMacroSnapshotCell {
	if cell, found := oracle.cells[name]; found {
		return cell
	}
	return oracle.fallbacks[configDependencyMacroSnapshotNamespaceForName(name)]
}

func (oracle *configDependencyMacroSnapshotOracle) set(
	name string,
	cell configDependencyMacroSnapshotCell,
) {
	kind := configDependencyMacroSnapshotNamespaceForName(name)
	if cell == oracle.fallbacks[kind] {
		delete(oracle.cells, name)
		return
	}
	oracle.cells[name] = cell
}

func (oracle *configDependencyMacroSnapshotOracle) numericMaybeDefine() {
	for _, kind := range []configDependencyMacroSnapshotNamespaceKind{
		configDependencyMacroSnapshotOrdinaryNamespace,
		configDependencyMacroSnapshotReservedNamespace,
	} {
		oracle.fallbacks[kind] = configDependencyMacroSnapshotMaybeDefineCell(oracle.fallbacks[kind])
	}
	for name, cell := range oracle.cells {
		if configDependencyMacroSnapshotNamespaceForName(name) != configDependencyMacroSnapshotConfigNamespace {
			oracle.set(name, configDependencyMacroSnapshotMaybeDefineCell(cell))
		}
	}
}

func (oracle *configDependencyMacroSnapshotOracle) configSet(names []string, maybe bool) {
	for _, name := range names {
		cell := configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true)
		if maybe {
			cell = configDependencyMacroSnapshotMaybeDefineCell(oracle.lookup(name))
		}
		oracle.set(name, cell)
	}
}

func joinConfigDependencyMacroSnapshotOracles(
	left, right *configDependencyMacroSnapshotOracle,
) *configDependencyMacroSnapshotOracle {
	result := newConfigDependencyMacroSnapshotOracle()
	for kind, leftFallback := range left.fallbacks {
		result.fallbacks[kind] = configDependencyMacroSnapshotJoinCells(leftFallback, right.fallbacks[kind])
	}
	names := map[string]bool{}
	for name := range left.cells {
		names[name] = true
	}
	for name := range right.cells {
		names[name] = true
	}
	for name := range names {
		result.set(name, configDependencyMacroSnapshotJoinCells(left.lookup(name), right.lookup(name)))
	}
	return result
}

func assertConfigDependencyMacroSnapshotMatchesOracle(
	t *testing.T,
	stage string,
	snapshot *configDependencyMacroSnapshot,
	oracle *configDependencyMacroSnapshotOracle,
	names []string,
) {
	t.Helper()
	for _, name := range names {
		got, valid := snapshot.lookup(name)
		if want := oracle.lookup(name); !valid || got != want {
			t.Fatalf("%s: %s = %#v, valid=%t, want %#v", stage, name, got, valid, want)
		}
	}
}

func TestConfigDependencyMacroSnapshotRandomProgramsMatchOracle(t *testing.T) {
	names := []string{}
	configNames := []string{}
	for index := 0; index < 24; index++ {
		configName := fmt.Sprintf("CONFIG_RANDOM_%02d", index)
		configNames = append(configNames, configName)
		names = append(names, configName, fmt.Sprintf("ORDINARY_RANDOM_%02d", index), fmt.Sprintf("_RESERVED_RANDOM_%02d", index))
	}
	names = append(names, "CONFIG_UNMENTIONED", "ORDINARY_UNMENTIONED", "_RESERVED_UNMENTIONED")
	cells := []configDependencyMacroSnapshotCell{}
	for _, definition := range []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	} {
		for _, recorded := range []bool{false, true} {
			cells = append(cells, configDependencyMacroSnapshotTestCell(definition, recorded))
		}
	}

	random := rand.New(rand.NewSource(0x5eed))
	snapshot := newConfigDependencyMacroSnapshot()
	oracle := newConfigDependencyMacroSnapshotOracle()
	applyMutation := func(
		snapshot *configDependencyMacroSnapshot,
		oracle *configDependencyMacroSnapshotOracle,
	) (*configDependencyMacroSnapshot, *configDependencyMacroSnapshotOracle) {
		switch random.Intn(4) {
		case 0:
			name := names[random.Intn(len(names)-3)]
			cell := cells[random.Intn(len(cells))]
			var valid bool
			snapshot, valid = snapshot.withCell(name, cell)
			if !valid {
				t.Fatalf("random exact write to %s was rejected", name)
			}
			oracle.set(name, cell)
		case 1:
			var valid bool
			snapshot, valid = snapshot.withValidatedNumericMacroHeader()
			if !valid {
				t.Fatal("random numeric maybe-define was rejected")
			}
			oracle.numericMaybeDefine()
		case 2, 3:
			set := []string{}
			for index := 0; index < 1+random.Intn(6); index++ {
				set = append(set, configNames[random.Intn(len(configNames))])
			}
			maybe := random.Intn(2) != 0
			var valid bool
			if maybe {
				snapshot, valid = snapshot.withMaybeConfigDefinitions(set)
			} else {
				snapshot, valid = snapshot.withConfigDefinitions(set)
			}
			if !valid {
				t.Fatal("random CONFIG set was rejected")
			}
			oracle.configSet(set, maybe)
		}
		return snapshot, oracle
	}

	for step := 0; step < 500; step++ {
		if step%7 != 0 {
			snapshot, oracle = applyMutation(snapshot, oracle)
		} else {
			leftSnapshot, leftOracle := snapshot, oracle.clone()
			rightSnapshot, rightOracle := snapshot, oracle.clone()
			for count := 0; count < 1+random.Intn(4); count++ {
				leftSnapshot, leftOracle = applyMutation(leftSnapshot, leftOracle)
			}
			for count := 0; count < 1+random.Intn(4); count++ {
				rightSnapshot, rightOracle = applyMutation(rightSnapshot, rightOracle)
			}
			var valid bool
			snapshot, valid = configDependencyJoinMacroSnapshots(leftSnapshot, rightSnapshot)
			if !valid {
				t.Fatalf("random branch join at step %d was rejected", step)
			}
			oracle = joinConfigDependencyMacroSnapshotOracles(leftOracle, rightOracle)
		}
		assertConfigDependencyMacroSnapshotMatchesOracle(
			t, fmt.Sprintf("step %d", step), snapshot, oracle, names,
		)
	}
}

func TestConfigDependencyMacroSnapshotConditionalProjection(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	var valid bool
	snapshot, valid = snapshot.withDefinition("CONFIG_ENABLED", configDependencyMacroDefined)
	if !valid {
		t.Fatal("could not define CONFIG macro")
	}
	snapshot, valid = snapshot.withDefinition("ORDINARY_DEFINED", configDependencyMacroDefined)
	if !valid {
		t.Fatal("could not define ordinary macro")
	}
	snapshot, valid = snapshot.withDefinition("ORDINARY_UNDEFINED", configDependencyMacroUndefined)
	if !valid {
		t.Fatal("could not undefine ordinary macro")
	}
	snapshot, valid = snapshot.withValidatedNumericMacroHeader()
	if !valid {
		t.Fatal("could not apply wildcard")
	}
	snapshot, valid = snapshot.withDefinition("POST_WILDCARD_UNDEFINED", configDependencyMacroUndefined)
	if !valid {
		t.Fatal("could not add wildcard override")
	}

	projection, valid := snapshot.conditionalProjection()
	if !valid || !projection.unknownNonConfig || projection.key == "" {
		t.Fatalf("projection = %#v, valid=%t", projection, valid)
	}
	for name, want := range map[string]configDependencyMacroDefinition{
		"CONFIG_ENABLED":             configDependencyMacroDefined,
		"ORDINARY_DEFINED":           configDependencyMacroDefined,
		"ORDINARY_UNDEFINED":         configDependencyMacroUnknown,
		"POST_WILDCARD_UNDEFINED":    configDependencyMacroUndefined,
		"UNMENTIONED_AFTER_WILDCARD": configDependencyMacroUnknown,
		"CONFIG_UNMENTIONED":         configDependencyMacroUndefined,
		"_RESERVED_UNMENTIONED":      configDependencyMacroUnknown,
	} {
		if got := projection.definition(name); got != want {
			t.Errorf("projection %s = %d, want %d", name, got, want)
		}
	}
	second, valid := snapshot.conditionalProjection()
	if !valid || second.key != projection.key {
		t.Fatalf("cached projection key = %q, want %q", second.key, projection.key)
	}
}

func TestConfigDependencyMacroSnapshotProjectionIsCanonicalAcrossConstructionOrder(t *testing.T) {
	names := []string{
		"CONFIG_ZETA",
		"ORDINARY_ALPHA",
		"_RESERVED_MIDDLE",
		"CONFIG_ALPHA",
		"ORDINARY_ZETA",
	}
	build := func(indices []int) *configDependencyMacroSnapshot {
		snapshot := newConfigDependencyMacroSnapshot()
		for _, index := range indices {
			definition := configDependencyMacroDefined
			if index%2 != 0 {
				definition = configDependencyMacroUndefined
			}
			var valid bool
			snapshot, valid = snapshot.withDefinition(names[index], definition)
			if !valid {
				t.Fatalf("ordered definition %d was rejected", index)
			}
		}
		var valid bool
		snapshot, valid = snapshot.withValidatedNumericMacroHeader()
		if !valid {
			t.Fatal("ordered wildcard was rejected")
		}
		return snapshot
	}
	forward := build([]int{0, 1, 2, 3, 4})
	reverse := build([]int{4, 3, 2, 1, 0})
	forwardProjection, valid := forward.conditionalProjection()
	if !valid {
		t.Fatal("forward projection was rejected")
	}
	reverseProjection, valid := reverse.conditionalProjection()
	if !valid {
		t.Fatal("reverse projection was rejected")
	}
	if forwardProjection.key != reverseProjection.key {
		t.Fatalf("construction-order keys differ: %q != %q", forwardProjection.key, reverseProjection.key)
	}
	for _, name := range names {
		if forwardProjection.definition(name) != reverseProjection.definition(name) {
			t.Errorf("construction-order projection differs for %s", name)
		}
	}
}

func collectConfigDependencyMacroSnapshotTreeNodes(
	root *configDependencyMacroSnapshotTree,
	nodes map[*configDependencyMacroSnapshotTree]bool,
) {
	if root == nil || nodes[root] {
		return
	}
	nodes[root] = true
	collectConfigDependencyMacroSnapshotTreeNodes(root.left.root, nodes)
	collectConfigDependencyMacroSnapshotTreeNodes(root.right.root, nodes)
}

func sharedConfigDependencyMacroSnapshotTreeNodes(
	left, right *configDependencyMacroSnapshotTree,
) (int, int) {
	leftNodes := map[*configDependencyMacroSnapshotTree]bool{}
	rightNodes := map[*configDependencyMacroSnapshotTree]bool{}
	collectConfigDependencyMacroSnapshotTreeNodes(left, leftNodes)
	collectConfigDependencyMacroSnapshotTreeNodes(right, rightNodes)
	shared := 0
	for node := range leftNodes {
		if rightNodes[node] {
			shared++
		}
	}
	return shared, len(leftNodes)
}

func configDependencyMacroSnapshotScaleFixture(t testing.TB, count int) *configDependencyMacroSnapshot {
	t.Helper()
	snapshot := newConfigDependencyMacroSnapshot()
	for index := 0; index < count; index++ {
		var valid bool
		snapshot, valid = snapshot.withDefinition(
			fmt.Sprintf("CONFIG_SCALE_%05d", index), configDependencyMacroDefined,
		)
		if !valid {
			t.Fatalf("scale definition %d was rejected", index)
		}
	}
	return snapshot
}

func TestConfigDependencyMacroSnapshotSparseUpdatesShareLargeNamespace(t *testing.T) {
	const count = 4096
	base := configDependencyMacroSnapshotScaleFixture(t, count)
	left, valid := base.withDefinition("CONFIG_SCALE_01024", configDependencyMacroUndefined)
	if !valid {
		t.Fatal("left sparse update was rejected")
	}
	right, valid := base.withDefinition("CONFIG_SCALE_03072", configDependencyMacroUndefined)
	if !valid {
		t.Fatal("right sparse update was rejected")
	}

	for name, snapshot := range map[string]*configDependencyMacroSnapshot{
		"left":  left,
		"right": right,
	} {
		shared, total := sharedConfigDependencyMacroSnapshotTreeNodes(
			base.config.facts.root, snapshot.config.facts.root,
		)
		if shared < total-128 {
			t.Errorf("%s sparse update shares %d/%d nodes, want at least %d", name, shared, total, total-128)
		}
	}

	joined, valid := configDependencyJoinMacroSnapshots(left, right)
	if !valid {
		t.Fatal("sparse branch join was rejected")
	}
	for _, name := range []string{"CONFIG_SCALE_01024", "CONFIG_SCALE_03072"} {
		requireConfigDependencyMacroSnapshotCell(t, joined, name,
			configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true))
	}
	shared, total := sharedConfigDependencyMacroSnapshotTreeNodes(
		base.config.facts.root, joined.config.facts.root,
	)
	if shared < total-256 {
		t.Fatalf("sparse join shares %d/%d nodes, want at least %d", shared, total, total-256)
	}
	projection, valid := joined.conditionalProjection()
	if !valid || len(projection.facts) != count {
		t.Fatalf("scale projection facts = %d, valid=%t, want %d", len(projection.facts), valid, count)
	}
}

func TestConfigDependencyMacroSnapshotConcurrentReadersAndBranches(t *testing.T) {
	base := configDependencyMacroSnapshotScaleFixture(t, 512)
	const (
		workers    = 16
		iterations = 64
	)
	errors := make(chan string, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				name := fmt.Sprintf("CONFIG_SCALE_%05d", (worker+iteration)%512)
				left, valid := base.withDefinition(name, configDependencyMacroUndefined)
				if !valid {
					errors <- "sparse exact update failed"
					return
				}
				right, valid := base.withMaybeConfigDefinitions([]string{name})
				if !valid {
					errors <- "bulk maybe-define failed"
					return
				}
				joined, valid := configDependencyJoinMacroSnapshots(left, right)
				if !valid {
					errors <- "branch join failed"
					return
				}
				cell, valid := joined.lookup(name)
				if !valid || cell != configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, true) {
					errors <- fmt.Sprintf("joined %s = %#v, valid=%t", name, cell, valid)
					return
				}
				if _, valid := base.conditionalProjection(); !valid {
					errors <- "concurrent base projection failed"
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
	requireConfigDependencyMacroSnapshotCell(t, base, "CONFIG_SCALE_00000",
		configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true))
}

func TestConfigDependencyMacroStateSparseSnapshotBranchJoin(t *testing.T) {
	base := configDependencyMacroSnapshotScaleFixture(t, 4096)
	state := &configDependencyMacroState{snapshot: base}
	left := state.branch()
	right := state.branch()
	left.set("CONFIG_SCALE_00017", configDependencyMacroUndefined)
	right.set("CONFIG_SCALE_02017", configDependencyMacroUndefined)
	state.mergePossibleBranches(left, right)
	if state.tainted {
		t.Fatal("sparse snapshot branch join tainted the parent")
	}
	for _, name := range []string{"CONFIG_SCALE_00017", "CONFIG_SCALE_02017"} {
		requireConfigDependencyMacroSnapshotCell(t, state.snapshot, name,
			configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
	}
	requireConfigDependencyMacroSnapshotCell(t, state.snapshot, "CONFIG_SCALE_01000",
		configDependencyMacroSnapshotTestCell(configDependencyMacroDefined, true))
	if left.snapshot != nil || right.snapshot != nil || !left.consumed || !right.consumed {
		t.Fatal("joined snapshot branches were not consumed")
	}
}

func TestConfigDependencyMacroStateNestedCommitPropagatesSparseSupport(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	outer := root.branch()
	inner := outer.branch()
	inner.set("NESTED_GUARD", configDependencyMacroDefined)
	if !outer.commitBranch(inner) {
		t.Fatal("nested snapshot branch commit failed")
	}
	root.mergePossibleBranch(outer)
	if root.tainted {
		t.Fatal("nested snapshot branch join tainted the root")
	}
	requireConfigDependencyMacroSnapshotCell(t, root.snapshot, "NESTED_GUARD",
		configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
}

func TestConfigDependencyMacroStateOverlayCanonicalizesUnobservedFallbackWrites(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition configDependencyMacroDefinition
	}{
		{name: "ORDINARY_FALLBACK", definition: configDependencyMacroUndefined},
		{name: "_RESERVED_FALLBACK", definition: configDependencyMacroUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			branch := root.branch()
			branch.set(test.name, test.definition)
			root.mergePossibleBranch(branch)
			requireConfigDependencyMacroSnapshotCell(t, root.snapshot, test.name,
				configDependencyMacroSnapshotTestCell(test.definition, false))
		})
	}
}

func TestConfigDependencyMacroStateOverlayPreservesWildcardOrdering(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	beforeWildcard := root.branch()
	beforeWildcard.set("BEFORE_WILDCARD", configDependencyMacroUndefined)
	beforeWildcard.applyValidatedNumericMacroHeader()
	requireDefinition, recorded := beforeWildcard.explicitDefinition("BEFORE_WILDCARD")
	if requireDefinition != configDependencyMacroUnknown || recorded {
		t.Fatalf("undef then wildcard = (%v,%t), want (unknown,false)", requireDefinition, recorded)
	}
	afterWildcard := root.branch()
	afterWildcard.applyValidatedNumericMacroHeader()
	afterWildcard.set("AFTER_WILDCARD", configDependencyMacroUndefined)
	requireDefinition, recorded = afterWildcard.explicitDefinition("AFTER_WILDCARD")
	if requireDefinition != configDependencyMacroUndefined || recorded {
		t.Fatalf("wildcard then undef = (%v,%t), want (undefined,false)", requireDefinition, recorded)
	}
}

func TestConfigDependencyMacroStatePreservesOnlyObservedAutoconfProvenance(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	root.applyValidatedNumericMacroHeader()
	for _, test := range []struct {
		name         string
		wantRecorded bool
	}{
		{name: configDependencyResolvedAutoconfGuard, wantRecorded: true},
		{name: "ORDINARY_NUMERIC_MACRO", wantRecorded: false},
		{name: "__ORDINARY_RESERVED_MACRO", wantRecorded: false},
	} {
		definition, recorded := root.explicitDefinition(test.name)
		if definition != configDependencyMacroUnknown || recorded != test.wantRecorded {
			t.Errorf(
				"numeric wildcard %q = (%v,%t), want (unknown,%t)",
				test.name, definition, recorded, test.wantRecorded,
			)
		}
	}
}

func TestConfigDependencyMacroStateSnapshotBulkAndWildcardJoins(t *testing.T) {
	t.Run("config bulk", func(t *testing.T) {
		root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
		branch := root.branch()
		if !branch.applyConfigNameSet([]string{"CONFIG_ALPHA", "CONFIG_BETA"}, false) {
			t.Fatal("CONFIG definition set was rejected")
		}
		root.mergePossibleBranch(branch)
		for _, name := range []string{"CONFIG_ALPHA", "CONFIG_BETA"} {
			requireConfigDependencyMacroSnapshotCell(t, root.snapshot, name,
				configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
		}
	})
	t.Run("numeric wildcard", func(t *testing.T) {
		root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
		branch := root.branch()
		branch.applyValidatedNumericMacroHeader()
		root.mergePossibleBranch(branch)
		requireConfigDependencyMacroSnapshotCell(t, root.snapshot, "ARBITRARY_NUMERIC_NAME",
			configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
		requireConfigDependencyMacroSnapshotCell(t, root.snapshot, "_ARBITRARY_RESERVED_NAME",
			configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
		requireConfigDependencyMacroSnapshotCell(t, root.snapshot, "CONFIG_UNTOUCHED",
			configDependencyMacroSnapshotTestCell(configDependencyMacroUndefined, false))
	})
}

func TestConfigDependencyMacroStateSnapshotRejectsParentMutationWithLiveBranch(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	branch := root.branch()
	root.set("PARENT_CHANGED", configDependencyMacroDefined)
	branch.set("BRANCH_CHANGED", configDependencyMacroDefined)
	root.mergePossibleBranch(branch)
	if !root.tainted {
		t.Fatal("snapshot join accepted a branch forked from a stale parent")
	}
}

func BenchmarkConfigDependencyMacroSnapshotSparseBranchJoin(b *testing.B) {
	base := configDependencyMacroSnapshotScaleFixture(b, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		left, _ := base.withDefinition(
			fmt.Sprintf("CONFIG_SCALE_%05d", index%8192), configDependencyMacroUndefined,
		)
		right, _ := base.withDefinition(
			fmt.Sprintf("CONFIG_SCALE_%05d", (index+4096)%8192), configDependencyMacroUndefined,
		)
		if joined, valid := configDependencyJoinMacroSnapshots(left, right); !valid || joined == nil {
			b.Fatal("sparse branch join failed")
		}
	}
}

func BenchmarkConfigDependencyMacroStateSparseBranchJoin(b *testing.B) {
	base := configDependencyMacroSnapshotScaleFixture(b, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		state := &configDependencyMacroState{snapshot: base}
		left := state.branch()
		right := state.branch()
		left.set(fmt.Sprintf("CONFIG_SCALE_%05d", index%8192), configDependencyMacroUndefined)
		right.set(fmt.Sprintf("CONFIG_SCALE_%05d", (index+4096)%8192), configDependencyMacroUndefined)
		state.mergePossibleBranches(left, right)
		if state.tainted {
			b.Fatal("sparse state branch join failed")
		}
	}
}

func BenchmarkConfigDependencyMacroStateNestedSparseOverlayJoin(b *testing.B) {
	base := configDependencyMacroSnapshotScaleFixture(b, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		root := &configDependencyMacroState{snapshot: base}
		state := root.branch()
		left := state.branch()
		right := state.branch()
		left.set(fmt.Sprintf("CONFIG_SCALE_%05d", index%8192), configDependencyMacroUndefined)
		right.set(fmt.Sprintf("CONFIG_SCALE_%05d", (index+4096)%8192), configDependencyMacroUndefined)
		state.mergePossibleBranches(left, right)
		if state.tainted {
			b.Fatal("nested sparse overlay join failed")
		}
	}
}

func BenchmarkConfigDependencyMacroStateNestedSingleGuardOverlayJoin(b *testing.B) {
	base := configDependencyMacroSnapshotScaleFixture(b, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		root := &configDependencyMacroState{snapshot: base}
		state := root.branch()
		branch := state.branch()
		branch.set("CONFIG_SCALE_00017", configDependencyMacroUndefined)
		state.mergePossibleBranch(branch)
		if state.tainted {
			b.Fatal("nested single-guard overlay join failed")
		}
	}
}

func TestConfigDependencyMacroStateSequentialSiblingJoinsRetainMaterializedPrefix(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	state := root.branch()
	const siblingCount = 64
	for index := 0; index < siblingCount; index++ {
		before := state.materializedSnapshotCache
		branch := state.branch()
		if branch == nil || state.materializedSnapshotCache == nil ||
			branch.snapshot != state.materializedSnapshotCache {
			t.Fatalf("sibling %d did not fork from the materialized prefix", index)
		}
		if before != nil && state.materializedSnapshotCache != before {
			t.Fatalf("sibling %d rebuilt the unchanged materialized prefix", index)
		}

		name := fmt.Sprintf("SEQUENTIAL_SIBLING_EFFECT_%04d", index)
		branch.set(name, configDependencyMacroDefined)
		state.mergePossibleBranch(branch)
		if state.tainted {
			t.Fatalf("sibling %d tainted the speculative parent", index)
		}
		if state.snapshot != root.snapshot {
			t.Fatalf("sibling %d materialized into the speculative base", index)
		}
		if state.materializedSnapshotCache == nil {
			t.Fatalf("sibling %d discarded the updated materialized prefix", index)
		}
		if got := state.snapshotChanges.exactSize(); got != index+1 {
			t.Fatalf("sibling %d sparse support = %d, want %d", index, got, index+1)
		}
		if got := state.definition(name); got != configDependencyMacroUnknown {
			t.Fatalf("sibling %d joined definition = %d, want unknown", index, got)
		}
	}
}

func BenchmarkConfigDependencyMacroStateSequentialSiblingOverlayJoins(b *testing.B) {
	base := configDependencyMacroSnapshotScaleFixture(b, 8192)
	for _, siblingCount := range []int{8, 32, 128} {
		b.Run(fmt.Sprintf("siblings-%d", siblingCount), func(b *testing.B) {
			names := make([]string, siblingCount)
			for index := range names {
				names[index] = fmt.Sprintf("SEQUENTIAL_SIBLING_EFFECT_%04d", index)
			}
			b.ReportAllocs()
			b.ReportMetric(float64(siblingCount), "siblings/op")
			b.ResetTimer()
			for range b.N {
				root := &configDependencyMacroState{snapshot: base}
				state := root.branch()
				for _, name := range names {
					branch := state.branch()
					branch.set(name, configDependencyMacroDefined)
					state.mergePossibleBranch(branch)
				}
				if state.tainted || state.snapshotChanges.exactSize() != siblingCount ||
					state.materializedSnapshotCache == nil {
					b.Fatal("sequential sibling overlay join failed")
				}
			}
		})
	}
}

func applyConfigDependencyNestedOptionalGuardChainForBenchmark(
	state *configDependencyMacroState,
	guards []string,
	effects []string,
) {
	if len(guards) == 0 {
		return
	}
	included := state.branch()
	skipped := included.branch()
	skipped.set(guards[0], configDependencyMacroDefined)
	executed := included.branch()
	executed.set(guards[0], configDependencyMacroUndefined)
	executed.set(guards[0], configDependencyMacroDefined)
	if len(effects) != 0 {
		executed.set(effects[0], configDependencyMacroDefined)
		effects = effects[1:]
	}
	applyConfigDependencyNestedOptionalGuardChainForBenchmark(executed, guards[1:], effects)
	included.mergePossibleBranches(skipped, executed)
	state.mergePossibleBranch(included)
}

func BenchmarkConfigDependencyMacroStateNestedOptionalGuardChain(b *testing.B) {
	for _, retainEffects := range []bool{false, true} {
		name := "provenance-only"
		if retainEffects {
			name = "retained-effects"
		}
		b.Run(name, func(b *testing.B) {
			for _, depth := range []int{8, 32, 128} {
				b.Run(fmt.Sprintf("depth-%d", depth), func(b *testing.B) {
					guards := make([]string, depth)
					effects := []string(nil)
					if retainEffects {
						effects = make([]string, depth)
					}
					for index := range guards {
						guards[index] = fmt.Sprintf("__OPTIONAL_GUARD_%04d", index)
						if retainEffects {
							effects[index] = fmt.Sprintf("OPTIONAL_EFFECT_%04d", index)
						}
					}
					b.ReportAllocs()
					b.ReportMetric(float64(depth), "guards/op")
					b.ResetTimer()
					for range b.N {
						root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
						state := root.branch()
						applyConfigDependencyNestedOptionalGuardChainForBenchmark(state, guards, effects)
						if state.tainted || state.snapshotChanges.exactSize() != len(effects) {
							b.Fatalf(
								"optional guard support = %d, want %d retained effects",
								state.snapshotChanges.exactSize(), len(effects),
							)
						}
					}
				})
			}
		})
	}
}

func BenchmarkConfigDependencyMacroSnapshotNumericNamespaceTransform(b *testing.B) {
	snapshot := newConfigDependencyMacroSnapshot()
	for index := 0; index < 8192; index++ {
		var valid bool
		snapshot, valid = snapshot.withDefinition(
			fmt.Sprintf("ORDINARY_SCALE_%05d", index), configDependencyMacroUndefined,
		)
		if !valid {
			b.Fatal("ordinary scale definition failed")
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result, valid := snapshot.withValidatedNumericMacroHeader(); !valid || result == nil {
			b.Fatal("numeric namespace transform failed")
		}
	}
}

// Keep the original eager-exposure lookup independent of the optimized search
// and TransformTree fast path. It is also the benchmark's pre-change baseline.
func originalConfigDependencyMacroSnapshotTreeGetForTest(
	tree configDependencyMacroSnapshotTreeView,
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	transformTree := func(child configDependencyMacroSnapshotTreeView, transform configDependencyMacroSnapshotTransform) configDependencyMacroSnapshotTreeView {
		if child.root == nil {
			return configDependencyMacroSnapshotTreeView{}
		}
		composed := configDependencyMacroSnapshotComposeTransforms(transform, child.transform)
		if composed == child.transform {
			return child
		}
		return configDependencyMacroSnapshotTreeView{root: child.root, transform: composed}
	}
	for tree.root != nil {
		cell := configDependencyMacroSnapshotApplyKnownTransform(tree.transform, tree.root.cell)
		left := transformTree(tree.root.left, tree.transform)
		right := transformTree(tree.root.right, tree.transform)
		switch {
		case name < tree.root.name:
			tree = left
		case name > tree.root.name:
			tree = right
		default:
			return cell, true
		}
	}
	return configDependencyMacroSnapshotCell{}, false
}

func TestConfigDependencyMacroSnapshotTreeGetMatchesOriginalExposure(t *testing.T) {
	random := rand.New(rand.NewSource(493852))
	for iteration := 0; iteration < 300; iteration++ {
		// Arbitrary tags exercise noncommuting compositions, not only the
		// currently reachable maybe-define/join transforms. Corrupt packed cells
		// must retain the old conservative normalization, including under an
		// identity parent. Each tree is immutable once published to the reader.
		var build func(int, int) configDependencyMacroSnapshotTreeView
		build = func(first, limit int) configDependencyMacroSnapshotTreeView {
			if first == limit {
				return configDependencyMacroSnapshotTreeView{}
			}
			middle := (first + limit) / 2
			cell, _ := configDependencyMacroSnapshotCellAt(random.Intn(configDependencyMacroSnapshotCellCount))
			if iteration%7 == 0 {
				cell.definition = configDependencyMacroDefinition(127)
			}
			tree := configDependencyMacroSnapshotNewTree(fmt.Sprintf("M_%02d", middle), cell, 0, build(first, middle), build(middle+1, limit))
			if iteration%3 != 0 {
				for index := range tree.transform {
					tree.transform[index] = uint8(random.Intn(configDependencyMacroSnapshotCellCount))
				}
			}
			if iteration%5 == 0 {
				tree.transform[random.Intn(configDependencyMacroSnapshotCellCount)] = uint8(6 + random.Intn(250))
			}
			return tree
		}
		tree := build(0, 31)
		for query := -1; query <= 31; query++ {
			name := fmt.Sprintf("M_%02d", query)
			got, found := configDependencyMacroSnapshotTreeGet(tree, name)
			want, wantFound := originalConfigDependencyMacroSnapshotTreeGetForTest(tree, name)
			if got != want || found != wantFound {
				t.Fatalf("iteration %d query %q: got %#v/%t, want %#v/%t", iteration, name, got, found, want, wantFound)
			}
		}
	}
}

func configDependencyMacroSnapshotLookupFixtureForBenchmark(size int) (*configDependencyMacroSnapshot, []string) {
	snapshot := newConfigDependencyMacroSnapshot()
	names := make([]string, size)
	for index := range names {
		names[index] = fmt.Sprintf("ORDINARY_LOOKUP_%05d", index)
		snapshot, _ = snapshot.withDefinition(names[index], configDependencyMacroDefined)
	}
	return snapshot, names
}

func BenchmarkConfigDependencyMacroSnapshotTreeLookup(b *testing.B) {
	for _, size := range []int{1024, 8192} {
		snapshot, names := configDependencyMacroSnapshotLookupFixtureForBenchmark(size)
		for _, tagged := range []bool{false, true} {
			tree := snapshot.ordinary.facts
			if tagged {
				tree = configDependencyMacroSnapshotTransformTree(tree, configDependencyMacroSnapshotMaybeDefineTransform())
			}
			for _, original := range []bool{true, false} {
				b.Run(fmt.Sprintf("names_%d/tagged_%t/original_%t", size, tagged, original), func(b *testing.B) {
					lookup := configDependencyMacroSnapshotTreeGet
					if original {
						lookup = originalConfigDependencyMacroSnapshotTreeGetForTest
					}
					b.ReportAllocs()
					b.ResetTimer()
					for index := range b.N {
						cell, found := lookup(tree, names[index%len(names)])
						if !found || cell.definition != configDependencyMacroDefined || !cell.recorded {
							b.Fatal("lookup fixture did not retain exact definitions")
						}
					}
				})
			}
		}
	}
}
