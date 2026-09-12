package kconfig

import (
	"fmt"
	"maps"
	"testing"
)

// Preserve the pre-effect commit path as an independent replay oracle: this
// deliberately uses applySnapshotChanges without a proven materialized result.
func originalConfigDependencyMacroCommitForEffectTest(parent, branch *configDependencyMacroState) bool {
	if parent != nil && parent.rejectInvalidMutation() {
		return false
	}
	if parent == nil || branch == nil || branch.parent != parent {
		if parent != nil {
			parent.tainted = true
		}
		return false
	}
	if parent.tainted || branch.tainted {
		parent.tainted = true
		return false
	}
	input, valid := parent.materializedSnapshot()
	if !valid || branch.snapshot == nil || branch.snapshot != input || branch.snapshotForkRevision != parent.snapshotRevision {
		parent.tainted = true
		return false
	}
	if !parent.applySnapshotChanges(input, branch.snapshotChanges) {
		parent.tainted = true
		return false
	}
	parent.mergeForcedHeaderTouches(branch, false)
	branch.consume()
	return true
}

func requireConfigDependencyMacroEffectSnapshotsEqual(t *testing.T, got, want *configDependencyMacroSnapshot) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("nil snapshot: got=%p want=%p", got, want)
	}
	for _, kind := range []configDependencyMacroSnapshotNamespaceKind{
		configDependencyMacroSnapshotOrdinaryNamespace, configDependencyMacroSnapshotConfigNamespace, configDependencyMacroSnapshotReservedNamespace,
	} {
		left, right := got.namespace(kind), want.namespace(kind)
		if left.fallback != right.fallback {
			t.Fatalf("namespace %d fallback: got %#v want %#v", kind, left.fallback, right.fallback)
		}
		cells := func(namespace configDependencyMacroSnapshotNamespace) map[string]configDependencyMacroSnapshotCell {
			result := map[string]configDependencyMacroSnapshotCell{}
			configDependencyMacroSnapshotWalkTree(namespace.facts, func(name string, cell configDependencyMacroSnapshotCell) {
				if cell != namespace.fallback {
					result[name] = cell
				}
			})
			return result
		}
		if leftCells, rightCells := cells(left), cells(right); !maps.Equal(leftCells, rightCells) {
			t.Fatalf("namespace %d cells: got %#v want %#v", kind, leftCells, rightCells)
		}
	}
}

func configDependencyMacroEffectInputForTest() *configDependencyMacroSnapshot {
	snapshot := newConfigDependencyMacroSnapshot()
	for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
		cell, _ := configDependencyMacroSnapshotCellAt(index)
		for _, prefix := range []string{"ORDINARY_", "CONFIG_", "_RESERVED_"} {
			snapshot, _ = snapshot.withCell(fmt.Sprintf("%s%d", prefix, index), cell)
		}
	}
	return snapshot
}

func TestConfigDependencyMacroEffectMatchesOriginalCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*configDependencyMacroState)
	}{
		{"no_write", func(*configDependencyMacroState) {}},
		{"exact_cells", func(state *configDependencyMacroState) {
			for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
				cell, _ := configDependencyMacroSnapshotCellAt(index)
				for _, prefix := range []string{"ORDINARY_", "CONFIG_", "_RESERVED_"} {
					state.putSnapshotCell(fmt.Sprintf("%s%d", prefix, 5-index), cell)
				}
			}
		}},
		{"wildcard_then_exact", func(state *configDependencyMacroState) {
			state.applyValidatedNumericMacroHeader()
			state.set("ORDINARY_AFTER", configDependencyMacroUndefined)
			state.set("_RESERVED_AFTER", configDependencyMacroDefined)
			state.applyConfigNameSet([]string{"CONFIG_ADDED"}, false)
		}},
		{"exact_then_wildcard", func(state *configDependencyMacroState) {
			state.set("ORDINARY_BEFORE", configDependencyMacroDefined)
			state.set("_RESERVED_BEFORE", configDependencyMacroUndefined)
			state.applyValidatedNumericMacroHeader()
		}},
		{"recorded_only", func(state *configDependencyMacroState) {
			cell, _ := state.snapshotCell(configDependencyResolvedAutoconfGuard)
			cell.recorded = !cell.recorded
			state.putSnapshotCell(configDependencyResolvedAutoconfGuard, cell)
		}},
		{"written_back", func(state *configDependencyMacroState) {
			state.set("UNCHANGED", configDependencyMacroDefined)
			state.set("UNCHANGED", configDependencyMacroUndefined)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := configDependencyMacroEffectInputForTest()
			capturing := &configDependencyMacroState{snapshot: input}
			branch := capturing.branch()
			test.mutate(branch)
			effect, valid := captureConfigDependencyMacroEffect(branch)
			if !valid || branch.consumed || effect.input != input || effect.output == nil {
				t.Fatal("capture failed or consumed its source branch")
			}
			want := &configDependencyMacroState{snapshot: input}
			wantBranch := want.branch()
			test.mutate(wantBranch)
			if !originalConfigDependencyMacroCommitForEffectTest(want, wantBranch) {
				t.Fatal("original commit rejected fixture")
			}
			for _, nested := range []bool{false, true} {
				anchor := &configDependencyMacroState{snapshot: input}
				got := anchor
				if nested {
					got = anchor.branch()
				}
				if !effect.apply(got) || !got.validSnapshotLineage() {
					t.Fatalf("replay failed, nested=%t", nested)
				}
				post, valid := got.materializedSnapshot()
				if !valid || post != effect.output || got.snapshotRevision != want.snapshotRevision {
					t.Fatalf("replay rebuilt its proven post-state or changed revision behavior, nested=%t", nested)
				}
				requireConfigDependencyMacroEffectSnapshotsEqual(t, post, want.snapshot)
				if nested && (got.snapshot != input || anchor.snapshot != input) {
					t.Fatal("replay replaced a speculative fork snapshot or mutated its parent")
				}
				if nested && !anchor.commitBranch(got) {
					t.Fatal("replayed speculative state cannot commit into its original parent")
				}
				requireConfigDependencyMacroEffectSnapshotsEqual(t, anchor.snapshot, want.snapshot)
			}
		})
	}
}

func TestConfigDependencyMacroEffectPreservesExistingOverlayAndLaterBranches(t *testing.T) {
	base := configDependencyMacroEffectInputForTest()
	anchor := &configDependencyMacroState{snapshot: base}
	parent := anchor.branch()
	parent.set("PRIOR", configDependencyMacroDefined)
	input, _ := parent.materializedSnapshot()
	capturing := &configDependencyMacroState{snapshot: input}
	child := capturing.branch()
	child.set("HEADER", configDependencyMacroDefined)
	child.applyValidatedNumericMacroHeader()
	effect, valid := captureConfigDependencyMacroEffect(child)
	if !valid || !effect.apply(parent) || parent.snapshot != base || parent.parent != anchor {
		t.Fatal("effect replay lost existing overlay ancestry")
	}
	// The independent original path starts from the same complete entry value,
	// but must rematerialize the header's writes on its own.
	want := &configDependencyMacroState{snapshot: input}
	wantChild := want.branch()
	wantChild.set("HEADER", configDependencyMacroDefined)
	wantChild.applyValidatedNumericMacroHeader()
	if !originalConfigDependencyMacroCommitForEffectTest(want, wantChild) {
		t.Fatal("original header commit failed")
	}
	for _, state := range []*configDependencyMacroState{parent, want} {
		later := state.branch()
		later.set("LATER", configDependencyMacroDefined)
		if !state.commitBranch(later) {
			t.Fatal("later exact branch cannot commit")
		}
		left, right := state.branch(), state.branch()
		left.set("POSSIBLE", configDependencyMacroDefined)
		right.set("POSSIBLE", configDependencyMacroUndefined)
		state.mergePossibleBranches(left, right)
		if state.tainted || !left.consumed || !right.consumed {
			t.Fatal("later possible-branch join failed")
		}
	}
	if !anchor.commitBranch(parent) {
		t.Fatal("later mutations invalidated the original ancestry")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, anchor.snapshot, want.snapshot)
}

func TestConfigDependencyMacroEffectOwnsMutableChanges(t *testing.T) {
	input := newConfigDependencyMacroSnapshot()
	root := &configDependencyMacroState{snapshot: input}
	branch := root.branch()
	for _, name := range []string{"A", "B", "C"} {
		branch.set(name, configDependencyMacroDefined)
	}
	for _, name := range []string{"U", "V", "W"} {
		branch.set(name, configDependencyMacroUnknown)
	}
	effect, valid := captureConfigDependencyMacroEffect(branch)
	if !valid {
		t.Fatal("capture failed")
	}
	branch.set("B", configDependencyMacroUndefined)
	branch.set("V", configDependencyMacroDefined)
	for iteration := 0; iteration < 3; iteration++ {
		anchor := &configDependencyMacroState{snapshot: input}
		parent := anchor.branch()
		if !effect.apply(parent) {
			t.Fatal("replay failed")
		}
		post, _ := parent.materializedSnapshot()
		requireConfigDependencyMacroEffectSnapshotsEqual(t, post, effect.output)
		if cell, _ := parent.snapshotCell("B"); cell.definition != configDependencyMacroDefined {
			t.Fatal("capturing branch or previous replay changed cached concrete cells")
		}
		if cell, _ := parent.snapshotCell("V"); cell.definition != configDependencyMacroUnknown {
			t.Fatal("capturing branch or previous replay changed cached unknown names")
		}
		parent.set("B", configDependencyMacroUndefined)
		parent.set("V", configDependencyMacroDefined)
	}
	// A capture does not freeze its live branch. A later ordinary commit must
	// use the branch's updated materialized cache, not the earlier effect output.
	if !root.commitBranch(branch) {
		t.Fatal("capturing branch could not commit subsequent mutations")
	}
	if cell, _ := root.snapshotCell("B"); cell.definition != configDependencyMacroUndefined {
		t.Fatal("ordinary commit reused a stale materialized result")
	}
}

func TestConfigDependencyMacroEffectRejectsInvalidCaptureAndReplay(t *testing.T) {
	input := newConfigDependencyMacroSnapshot()
	root := &configDependencyMacroState{snapshot: input}
	branch := root.branch()
	branch.set("WRITTEN", configDependencyMacroDefined)
	effect, valid := captureConfigDependencyMacroEffect(branch)
	if !valid {
		t.Fatal("capture fixture failed")
	}
	for _, test := range []struct {
		name   string
		modify func(*configDependencyMacroState)
	}{
		{"tainted", func(state *configDependencyMacroState) { state.tainted = true }},
		{"consumed", func(state *configDependencyMacroState) { state.consumed = true }},
		{"traced", func(state *configDependencyMacroState) { state.beginForcedHeaderTrace() }},
		{"touches", func(state *configDependencyMacroState) { state.forcedHeaderTouches.put("TOUCHED") }},
		{"stale_parent", func(state *configDependencyMacroState) { state.parent.set("PARENT", configDependencyMacroDefined) }},
		{"traced_parent", func(state *configDependencyMacroState) { state.parent.beginForcedHeaderTrace() }},
		{"mismatched_entry", func(state *configDependencyMacroState) { state.snapshot = newConfigDependencyMacroSnapshot() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := &configDependencyMacroState{snapshot: input}
			current := parent.branch()
			test.modify(current)
			if _, valid := captureConfigDependencyMacroEffect(current); valid {
				t.Fatal("invalid branch was captured")
			}
			beforeSnapshot, beforeRevision, beforeTainted := current.snapshot, current.snapshotRevision, current.tainted
			if effect.apply(current) {
				t.Fatal("invalid/mismatched parent accepted replay")
			}
			if current.snapshot != beforeSnapshot || current.snapshotRevision != beforeRevision || current.tainted != beforeTainted {
				t.Fatal("failed cache replay mutated or tainted its caller")
			}
		})
	}
	if _, valid := captureConfigDependencyMacroEffect(nil); valid {
		t.Fatal("nil capture accepted")
	}
	if _, valid := captureConfigDependencyMacroEffect(root); valid {
		t.Fatal("root capture accepted without direct-child entry proof")
	}
	if effect.apply(nil) || (configDependencyMacroEffect{}).apply(root) {
		t.Fatal("nil or zero effect accepted")
	}
	// Even definition-equivalent snapshots cannot bypass exact pointer identity;
	// the old recursion key additionally ignores this observable recorded bit.
	different, _ := input.withCell(configDependencyResolvedAutoconfGuard, configDependencyMacroSnapshotCell{
		definition: configDependencyMacroUnknown, recorded: true,
	})
	if effect.apply(&configDependencyMacroState{snapshot: different}) ||
		effect.apply(&configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}) {
		t.Fatal("non-identical entry snapshot accepted")
	}
}

func TestConfigDependencyMacroEffectMaterializedWriteUndoneIsNoOp(t *testing.T) {
	input := newConfigDependencyMacroSnapshot()
	root := &configDependencyMacroState{snapshot: input}
	branch := root.branch()
	branch.set("TEMPORARY", configDependencyMacroDefined)
	if _, valid := branch.materializedSnapshot(); !valid {
		t.Fatal("cannot materialize intermediate write")
	}
	branch.set("TEMPORARY", configDependencyMacroUndefined)
	effect, valid := captureConfigDependencyMacroEffect(branch)
	if !valid || !effect.changes.empty() || effect.output == input {
		t.Fatal("fixture must retain a distinct materialization of a net-empty effect")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, effect.output, input)
	for _, replay := range []bool{false, true} {
		parent := &configDependencyMacroState{snapshot: input}
		if replay {
			if !effect.apply(parent) {
				t.Fatal("cannot replay net-empty effect")
			}
		} else {
			child := parent.branch()
			child.snapshotChanges = cloneConfigDependencyMacroEffectChanges(effect.changes)
			child.materializedSnapshotCache = effect.output
			if !parent.commitBranch(child) || !child.consumed {
				t.Fatal("cannot commit net-empty materialized child")
			}
		}
		if parent.snapshot != input || parent.snapshotRevision != 0 {
			t.Fatal("net-empty effect replaced entry identity or advanced its revision")
		}
	}
}

// Compare only transferring an already captured header effect. Both paths
// clone its privately owned finite overlay and preserve speculative ancestry;
// the old commit oracle additionally materializes every changed tree cell.
// This is not a benchmark of parsing or the complete scanner cache.
func BenchmarkConfigDependencyMacroEffectReplay(b *testing.B) {
	input := newConfigDependencyMacroSnapshot()
	for index := 0; index < 4096; index++ {
		var valid bool
		input, valid = input.withCell(fmt.Sprintf("PREDEFINED_%04d", index), configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
		if !valid {
			b.Fatal("invalid benchmark input")
		}
	}
	for _, count := range []int{512, 2048} {
		capturing := &configDependencyMacroState{snapshot: input}
		child := capturing.branch()
		for index := 0; index < count; index++ {
			child.set(fmt.Sprintf("HEADER_%04d", index), configDependencyMacroDefined)
		}
		effect, valid := captureConfigDependencyMacroEffect(child)
		if !valid || effect.changes.exactSize() != count {
			b.Fatal("invalid benchmark effect")
		}
		for _, replay := range []bool{false, true} {
			name := "original_commit"
			if replay {
				name = "captured_replay"
			}
			b.Run(fmt.Sprintf("%d/%s", count, name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					anchor := &configDependencyMacroState{snapshot: input}
					parent := anchor.branch()
					if replay {
						if !effect.apply(parent) || parent.materializedSnapshotCache != effect.output {
							b.Fatal("replay failed to retain proven post-state")
						}
					} else {
						branch := parent.branch()
						branch.snapshotChanges = cloneConfigDependencyMacroEffectChanges(effect.changes)
						branch.materializedSnapshotCache = effect.output
						if !originalConfigDependencyMacroCommitForEffectTest(parent, branch) {
							b.Fatal("original commit failed")
						}
					}
				}
			})
		}
	}
}
