package kconfig

import "testing"

func captureConfigDependencyMacroSpecializationForTest(
	t *testing.T,
	input *configDependencyMacroSnapshot,
	mutate func(*configDependencyMacroState),
) (configDependencyMacroSpecialization, *configDependencyMacroState) {
	t.Helper()
	parent := &configDependencyMacroState{snapshot: input}
	child := parent.branch()
	if !child.beginForcedHeaderTrace() {
		t.Fatal("cannot start specialization trace")
	}
	mutate(child)
	output, valid := child.materializedSnapshot()
	if !valid {
		t.Fatal("cannot materialize specialization output")
	}
	specialization, valid := captureConfigDependencyMacroSpecialization(child, input, output)
	if !valid {
		t.Fatal("cannot capture specialization")
	}
	return specialization, child
}

func configDependencyMacroSpecializationSnapshotForTest(t *testing.T, contents string) *configDependencyMacroSnapshot {
	t.Helper()
	state, reason := parseConfigDependencyCompilerPredefines(contents)
	if reason != "" {
		t.Fatal(reason)
	}
	return state.snapshot
}

func requireConfigDependencyMacroSpecializationMatchesOriginalForTest(
	t *testing.T,
	specialization configDependencyMacroSpecialization,
	input *configDependencyMacroSnapshot,
	mutate func(*configDependencyMacroState),
) {
	t.Helper()
	want := &configDependencyMacroState{snapshot: input}
	wantChild := want.branch()
	mutate(wantChild)
	if !originalConfigDependencyMacroCommitForEffectTest(want, wantChild) {
		t.Fatal("original uncached execution failed")
	}
	for _, nested := range []bool{false, true} {
		anchor := &configDependencyMacroState{snapshot: input}
		parent := anchor
		if nested {
			parent = anchor.branch()
		}
		if !specialization.apply(parent) || !parent.validSnapshotLineage() {
			t.Fatalf("specialized replay failed, nested=%t", nested)
		}
		output, valid := parent.materializedSnapshot()
		if !valid || parent.snapshotRevision != want.snapshotRevision {
			t.Fatalf("specialized replay changed commit revision, nested=%t", nested)
		}
		requireConfigDependencyMacroEffectSnapshotsEqual(t, output, want.snapshot)
		if nested {
			if parent.snapshot != input || parent.parent != anchor || !anchor.commitBranch(parent) {
				t.Fatal("specialized replay broke original branch ancestry")
			}
			requireConfigDependencyMacroEffectSnapshotsEqual(t, anchor.snapshot, want.snapshot)
		}
	}
}

func TestConfigDependencyMacroSpecializationPreservesUnreadAndEqualWrites(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "#define CONSTANT 1\n#define FIRST_ONLY 1\n")
	mutate := func(state *configDependencyMacroState) {
		state.set("CONSTANT", configDependencyMacroDefined)
		state.set("REMOVED", configDependencyMacroUndefined)
	}
	specialization, child := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
	if !child.snapshotChanges.empty() || specialization.transform.exactSize() != 2 || len(specialization.requirements) != 0 {
		t.Fatal("equal writes were lost or normalized changes used as the specialization")
	}
	current := configDependencyMacroSpecializationSnapshotForTest(t, "#define REMOVED 1\n#define SECOND_ONLY 1\n")
	requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, current, mutate)
	// On an entry where both explicit writes are no-ops, normalization must
	// preserve ordinary commit's zero revision increment as well.
	requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, input, mutate)
}

func TestConfigDependencyMacroSpecializationNoWritesStillChecksReadCells(t *testing.T) {
	for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
		cell, _ := configDependencyMacroSnapshotCellAt(index)
		input, _ := newConfigDependencyMacroSnapshot().withCell(configDependencyResolvedAutoconfGuard, cell)
		mutate := func(state *configDependencyMacroState) {
			_, _ = state.explicitDefinition(configDependencyResolvedAutoconfGuard)
		}
		specialization, _ := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
		if !specialization.transform.empty() || len(specialization.requirements) != 1 {
			t.Fatal("read-only execution lost its dependency or gained a write")
		}
		current, _ := input.withCell("UNREAD", configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
		requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, current, mutate)
		for otherIndex := 0; otherIndex < configDependencyMacroSnapshotCellCount; otherIndex++ {
			otherCell, _ := configDependencyMacroSnapshotCellAt(otherIndex)
			other, _ := current.withCell(configDependencyResolvedAutoconfGuard, otherCell)
			if specialization.matches(other) != (otherIndex == index) {
				t.Fatal("read requirement did not compare the complete six-cell value")
			}
		}
	}
}

func TestConfigDependencyMacroSpecializationPreservesExistingOverlayAndLaterJoins(t *testing.T) {
	base := configDependencyMacroSpecializationSnapshotForTest(t, "#define BASE 1\n")
	mutate := func(state *configDependencyMacroState) { state.set("HEADER", configDependencyMacroDefined) }
	specialization, _ := captureConfigDependencyMacroSpecializationForTest(t, base, mutate)
	anchor := &configDependencyMacroState{snapshot: base}
	parent := anchor.branch()
	parent.set("PRIOR", configDependencyMacroDefined)
	input, _ := parent.materializedSnapshot()
	want := &configDependencyMacroState{snapshot: input}
	wantChild := want.branch()
	mutate(wantChild)
	if !originalConfigDependencyMacroCommitForEffectTest(want, wantChild) || !specialization.apply(parent) {
		t.Fatal("initial replay or oracle commit failed")
	}
	for _, state := range []*configDependencyMacroState{parent, want} {
		later := state.branch()
		later.set("LATER", configDependencyMacroDefined)
		if !state.commitBranch(later) {
			t.Fatal("later direct child commit failed")
		}
		possible := state.branch()
		possible.set("POSSIBLE", configDependencyMacroDefined)
		state.mergePossibleBranch(possible)
	}
	if parent.snapshot != base || parent.parent != anchor || !anchor.commitBranch(parent) {
		t.Fatal("specialized replay broke preexisting overlay ancestry")
	}
	requireConfigDependencyMacroEffectSnapshotsEqual(t, anchor.snapshot, want.snapshot)
}

func TestConfigDependencyMacroSpecializationPreservesWildcardAndUntouchedCells(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "#define OLD_DEFINED 1\n")
	for _, possible := range []bool{false, true} {
		mutate := func(state *configDependencyMacroState) {
			if possible {
				child := state.branch()
				child.applyValidatedNumericMacroHeader()
				state.mergePossibleBranch(child)
			} else {
				state.applyValidatedNumericMacroHeader()
			}
			state.set("EXACT_AFTER", configDependencyMacroUndefined)
		}
		specialization, _ := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
		if !specialization.transform.nonConfigWildcard {
			t.Fatal("numeric wildcard was replaced by a captured fallback")
		}
		for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
			current := configDependencyMacroSpecializationSnapshotForTest(t,
				"#define NEW_DEFINED 1\n#define CONFIG_PRESERVED 1\n#define EXACT_AFTER 1\n")
			cell, _ := configDependencyMacroSnapshotCellAt(index)
			current, _ = current.withCell(configDependencyResolvedAutoconfGuard, cell)
			requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, current, mutate)
		}
	}
}

func TestConfigDependencyMacroSpecializationChecksOptionalJoinInputs(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "#define JOIN_TARGET 1\n")
	mutate := func(state *configDependencyMacroState) {
		child := state.branch()
		child.set("JOIN_TARGET", configDependencyMacroDefined)
		state.mergePossibleBranch(child)
	}
	specialization, child := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
	if !child.snapshotChanges.empty() || len(specialization.requirements) != 1 ||
		specialization.requirements[0].name != "JOIN_TARGET" {
		t.Fatal("optional normalized-away write did not retain the unchanged arm's entry dependency")
	}
	matching := configDependencyMacroSpecializationSnapshotForTest(t, "#define JOIN_TARGET 2\n#define EXTRA 1\n")
	requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, matching, mutate)
	other := configDependencyMacroSpecializationSnapshotForTest(t, "#define EXTRA 1\n")
	parent := &configDependencyMacroState{snapshot: other}
	if specialization.matches(other) || specialization.apply(parent) || parent.tainted || parent.snapshot != other || parent.snapshotRevision != 0 {
		t.Fatal("changed implicit join input was accepted or rejection mutated its parent")
	}
}

func TestConfigDependencyMacroSpecializationUnknownGuardAndRecordedRequirements(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "#define BODY_TARGET 1\n")
	mutate := func(state *configDependencyMacroState) {
		if state.definition("_UNKNOWN_GUARD") == configDependencyMacroUnknown {
			skipped := state.branch()
			skipped.set("_UNKNOWN_GUARD", configDependencyMacroDefined)
			executed := state.branch()
			executed.set("_UNKNOWN_GUARD", configDependencyMacroUndefined)
			executed.set("_UNKNOWN_GUARD", configDependencyMacroDefined)
			executed.set("BODY_TARGET", configDependencyMacroDefined)
			state.mergePossibleBranches(skipped, executed)
		}
		_, _ = state.explicitDefinition(configDependencyResolvedAutoconfGuard)
	}
	specialization, _ := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
	matching := configDependencyMacroSpecializationSnapshotForTest(t, "#define BODY_TARGET 1\n#define UNREAD 1\n")
	requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, matching, mutate)
	cell, _ := matching.lookup(configDependencyResolvedAutoconfGuard)
	cell.recorded = !cell.recorded
	changed, _ := matching.withCell(configDependencyResolvedAutoconfGuard, cell)
	if specialization.matches(changed) {
		t.Fatal("recorded-bit-only dependency change reused specialization")
	}
	changed, _ = matching.withCell("BODY_TARGET", configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined})
	if specialization.matches(changed) {
		t.Fatal("unknown-guard body join lost its inherited input dependency")
	}
}

func TestConfigDependencyMacroSpecializationOwnsCapturedWrites(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "")
	mutate := func(state *configDependencyMacroState) {
		for _, name := range []string{"A", "B", "C"} {
			state.set(name, configDependencyMacroDefined)
		}
		for _, name := range []string{"U", "V", "W"} {
			state.set(name, configDependencyMacroUnknown)
		}
	}
	specialization, capturing := captureConfigDependencyMacroSpecializationForTest(t, input, mutate)
	capturing.set("B", configDependencyMacroUndefined)
	capturing.set("V", configDependencyMacroDefined)
	for _, contents := range []string{"#define FIRST 1\n", "#define SECOND 1\n"} {
		current := configDependencyMacroSpecializationSnapshotForTest(t, contents)
		requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, current, mutate)
		parent := (&configDependencyMacroState{snapshot: current}).branch()
		if !specialization.apply(parent) {
			t.Fatal("cannot apply owned effect")
		}
		parent.set("B", configDependencyMacroUndefined)
		parent.set("V", configDependencyMacroDefined)
		requireConfigDependencyMacroSpecializationMatchesOriginalForTest(t, specialization, current, mutate)
	}
}

func TestConfigDependencyMacroSpecializationRejectsInvalidTraceAndParents(t *testing.T) {
	input := configDependencyMacroSpecializationSnapshotForTest(t, "")
	for _, invalidate := range []func(*configDependencyMacroState){
		func(state *configDependencyMacroState) { state.forcedHeaderTrace.wholeNamespace = true },
		func(state *configDependencyMacroState) { state.forcedHeaderTrace.invalid = true },
		func(state *configDependencyMacroState) {
			state.forcedHeaderTrace.entrySnapshot = newConfigDependencyMacroSnapshot()
		},
		func(state *configDependencyMacroState) { state.tainted = true },
		func(state *configDependencyMacroState) { state.parent.set("STALE", configDependencyMacroDefined) },
	} {
		parent := &configDependencyMacroState{snapshot: input}
		child := parent.branch()
		if !child.beginForcedHeaderTrace() {
			t.Fatal("cannot begin trace")
		}
		output, _ := child.materializedSnapshot()
		invalidate(child)
		if _, valid := captureConfigDependencyMacroSpecialization(child, input, output); valid {
			t.Fatal("invalid or whole-namespace trace captured")
		}
	}
	specialization, _ := captureConfigDependencyMacroSpecializationForTest(t, input, func(state *configDependencyMacroState) {
		state.set("OUTPUT", configDependencyMacroDefined)
	})
	for _, invalidate := range []func(*configDependencyMacroState){
		func(state *configDependencyMacroState) { state.tainted = true },
		func(state *configDependencyMacroState) { state.consume() },
		func(state *configDependencyMacroState) { state.beginForcedHeaderTrace() },
		func(state *configDependencyMacroState) { state.parent.beginForcedHeaderTrace() },
		func(state *configDependencyMacroState) { state.parent.set("STALE", configDependencyMacroDefined) },
	} {
		parent := (&configDependencyMacroState{snapshot: input}).branch()
		invalidate(parent)
		before, revision, tainted := parent.snapshot, parent.snapshotRevision, parent.tainted
		if specialization.apply(parent) || parent.snapshot != before || parent.snapshotRevision != revision || parent.tainted != tainted {
			t.Fatal("invalid replay accepted or rejection changed parent")
		}
	}
	if specialization.apply(nil) || (configDependencyMacroSpecialization{}).matches(input) {
		t.Fatal("nil parent or uncaptured specialization accepted")
	}
}
