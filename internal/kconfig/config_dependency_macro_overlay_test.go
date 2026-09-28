package kconfig

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

type configDependencyMacroOverlayFixture struct {
	state  *configDependencyMacroState
	oracle *configDependencyMacroSnapshot
}

func configDependencyMacroOverlayCell(
	definition configDependencyMacroDefinition,
	recorded bool,
) configDependencyMacroSnapshotCell {
	return configDependencyMacroSnapshotCell{
		definition: definition,
		recorded:   recorded,
	}
}

func configDependencyMacroOverlayInitialSnapshot(
	t *testing.T,
	names []string,
) *configDependencyMacroSnapshot {
	t.Helper()
	cells := []configDependencyMacroSnapshotCell{}
	for _, definition := range []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	} {
		for _, recorded := range []bool{false, true} {
			cells = append(cells, configDependencyMacroOverlayCell(definition, recorded))
		}
	}

	snapshot := newConfigDependencyMacroSnapshot()
	for index, cell := range cells {
		// Seed every definition in every namespace. Production provenance is
		// canonicalized here as it is at each state mutation boundary; exhaustive
		// six-cell coverage remains in the snapshot-primitive tests.
		for _, nameIndex := range []int{index, index + 10, index + 20} {
			var valid bool
			name := names[nameIndex]
			snapshot, valid = snapshot.withCell(
				name, configDependencyMacroStateCanonicalCell(name, cell),
			)
			if !valid {
				t.Fatalf("seed %q with %#v was rejected", names[nameIndex], cell)
			}
		}
	}
	return snapshot
}

func assertConfigDependencyMacroOverlayMatchesSnapshot(
	t *testing.T,
	stage string,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
	names []string,
) {
	t.Helper()
	if state == nil || state.tainted {
		t.Fatalf("%s: production state is nil or tainted", stage)
	}
	materialized, valid := state.materializedSnapshot()
	if !valid {
		t.Fatalf("%s: production overlay could not be materialized", stage)
	}
	for _, name := range names {
		want, valid := oracle.lookup(name)
		if !valid {
			t.Fatalf("%s: oracle rejected lookup of %q", stage, name)
		}
		gotDefinition, gotRecorded := state.explicitDefinition(name)
		if gotDefinition != want.definition || gotRecorded != want.recorded {
			t.Fatalf(
				"%s: production %q = (%d,%t), snapshot oracle = (%d,%t)",
				stage,
				name,
				gotDefinition,
				gotRecorded,
				want.definition,
				want.recorded,
			)
		}
		if gotDefinition := state.definition(name); gotDefinition != want.definition {
			t.Fatalf(
				"%s: definition(%q) = %d, snapshot oracle = %d",
				stage,
				name,
				gotDefinition,
				want.definition,
			)
		}
		got, valid := materialized.lookup(name)
		if !valid || got != want {
			t.Fatalf(
				"%s: materialized %q = %#v, valid=%t, snapshot oracle = %#v",
				stage,
				name,
				got,
				valid,
				want,
			)
		}
	}
}

func forkConfigDependencyMacroOverlay(
	t *testing.T,
	stage string,
	parent *configDependencyMacroState,
) *configDependencyMacroState {
	t.Helper()
	wantRevision := parent.snapshotRevision
	child := parent.branch()
	if child == nil || child.tainted || child.parent != parent || child.snapshot == nil {
		t.Fatalf("%s: production branch was not a live snapshot overlay", stage)
	}
	if child.snapshotForkRevision != wantRevision {
		t.Fatalf(
			"%s: fork revision = %d, want parent revision %d",
			stage,
			child.snapshotForkRevision,
			wantRevision,
		)
	}
	return child
}

func requireConfigDependencyMacroOverlayConsumed(
	t *testing.T,
	stage string,
	child *configDependencyMacroState,
) {
	t.Helper()
	if child.parent != nil || child.snapshot != nil || !child.snapshotChanges.empty() ||
		!child.consumed {
		t.Fatalf("%s: production child was not consumed", stage)
	}
}

func applyConfigDependencyMacroOverlaySet(
	t *testing.T,
	random *rand.Rand,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
	names []string,
) *configDependencyMacroSnapshot {
	t.Helper()
	name := names[random.Intn(len(names)-3)]
	definitions := []configDependencyMacroDefinition{
		configDependencyMacroUnknown,
		configDependencyMacroUndefined,
		configDependencyMacroDefined,
	}
	definition := definitions[random.Intn(len(definitions))]
	state.set(name, definition)
	next, valid := oracle.withCell(name, configDependencyMacroStateCanonicalCell(
		name,
		configDependencyMacroOverlayCell(definition, true),
	))
	if !valid {
		t.Fatalf("snapshot oracle rejected exact write to %q", name)
	}
	return next
}

func applyConfigDependencyMacroOverlayNumeric(
	t *testing.T,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
) *configDependencyMacroSnapshot {
	t.Helper()
	state.applyValidatedNumericMacroHeader()
	next, valid := configDependencyMacroStateWithValidatedNumericMacroHeader(oracle)
	if !valid {
		t.Fatal("snapshot oracle rejected numeric generated-header effect")
	}
	return next
}

func applyConfigDependencyMacroOverlayConfigBulk(
	t *testing.T,
	random *rand.Rand,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
	configNames []string,
) *configDependencyMacroSnapshot {
	t.Helper()
	wanted := map[string]bool{}
	for count := 1 + random.Intn(4); len(wanted) < count; {
		wanted[configNames[random.Intn(len(configNames))]] = true
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	if random.Intn(2) == 0 {
		if !state.applyConfigNameSet(names, false) {
			t.Fatal("production state rejected CONFIG definition set")
		}
		next, valid := configDependencyMacroStateWithConfigNameSet(oracle, names, false)
		if !valid {
			t.Fatal("snapshot oracle rejected CONFIG define set")
		}
		return next
	}
	if !state.applyConfigNameSet(names, true) {
		t.Fatal("production state rejected possible CONFIG definition set")
	}
	next, valid := configDependencyMacroStateWithConfigNameSet(oracle, names, true)
	if !valid {
		t.Fatal("snapshot oracle rejected CONFIG maybe-define set")
	}
	return next
}

func applyRandomConfigDependencyMacroOverlayLeaf(
	t *testing.T,
	random *rand.Rand,
	stage string,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
	names, configNames []string,
) *configDependencyMacroSnapshot {
	t.Helper()
	switch random.Intn(3) {
	case 0:
		oracle = applyConfigDependencyMacroOverlaySet(t, random, state, oracle, names)
	case 1:
		oracle = applyConfigDependencyMacroOverlayNumeric(t, state, oracle)
	case 2:
		oracle = applyConfigDependencyMacroOverlayConfigBulk(t, random, state, oracle, configNames)
	}
	assertConfigDependencyMacroOverlayMatchesSnapshot(t, stage, state, oracle, names)
	return oracle
}

func applyRandomConfigDependencyMacroOverlayBranchProgram(
	t *testing.T,
	random *rand.Rand,
	stage string,
	state *configDependencyMacroState,
	oracle *configDependencyMacroSnapshot,
	names, configNames []string,
) *configDependencyMacroSnapshot {
	t.Helper()
	for mutation := 0; mutation < 1+random.Intn(4); mutation++ {
		oracle = applyRandomConfigDependencyMacroOverlayLeaf(
			t,
			random,
			fmt.Sprintf("%s leaf %d", stage, mutation),
			state,
			oracle,
			names,
			configNames,
		)
	}
	if random.Intn(2) == 0 {
		return oracle
	}

	child := forkConfigDependencyMacroOverlay(t, stage+" nested fork", state)
	childOracle := oracle
	for mutation := 0; mutation < 1+random.Intn(4); mutation++ {
		childOracle = applyRandomConfigDependencyMacroOverlayLeaf(
			t,
			random,
			fmt.Sprintf("%s nested leaf %d", stage, mutation),
			child,
			childOracle,
			names,
			configNames,
		)
	}
	if !state.commitBranch(child) {
		t.Fatalf("%s: nested production commit failed", stage)
	}
	requireConfigDependencyMacroOverlayConsumed(t, stage+" nested commit", child)
	assertConfigDependencyMacroOverlayMatchesSnapshot(
		t, stage+" after nested commit", state, childOracle, names,
	)
	return childOracle
}

func TestConfigDependencyMacroStateRandomOverlaysMatchPersistentSnapshot(t *testing.T) {
	configNames := []string{}
	ordinaryNames := []string{}
	reservedNames := []string{}
	for index := 0; index < 10; index++ {
		configNames = append(configNames, fmt.Sprintf("CONFIG_OVERLAY_%02d", index))
		ordinaryNames = append(ordinaryNames, fmt.Sprintf("OVERLAY_GUARD_%02d", index))
		reservedNames = append(reservedNames, fmt.Sprintf("__OVERLAY_RESERVED_%02d", index))
	}
	names := append([]string{}, configNames...)
	names = append(names, ordinaryNames...)
	names = append(names, reservedNames...)
	// These final names are never directly selected. They continuously check
	// namespace fallback and recordedness across wildcard and join operations.
	names = append(names,
		"CONFIG_OVERLAY_UNMENTIONED",
		"OVERLAY_UNMENTIONED",
		"__OVERLAY_RESERVED_UNMENTIONED",
	)

	initial := configDependencyMacroOverlayInitialSnapshot(t, names)
	fixture := configDependencyMacroOverlayFixture{
		state:  &configDependencyMacroState{snapshot: initial},
		oracle: initial,
	}
	random := rand.New(rand.NewSource(0x0f3a1a7))
	assertConfigDependencyMacroOverlayMatchesSnapshot(
		t, "initial", fixture.state, fixture.oracle, names,
	)

	for step := 0; step < 360; step++ {
		stage := fmt.Sprintf("step %03d", step)
		switch step % 6 {
		case 0:
			fixture.oracle = applyConfigDependencyMacroOverlaySet(
				t, random, fixture.state, fixture.oracle, names,
			)
		case 1:
			fixture.oracle = applyConfigDependencyMacroOverlayNumeric(
				t, fixture.state, fixture.oracle,
			)
		case 2:
			fixture.oracle = applyConfigDependencyMacroOverlayConfigBulk(
				t, random, fixture.state, fixture.oracle, configNames,
			)
		case 3:
			outer := forkConfigDependencyMacroOverlay(t, stage+" outer fork", fixture.state)
			outerOracle := applyRandomConfigDependencyMacroOverlayBranchProgram(
				t, random, stage+" outer", outer, fixture.oracle, names, configNames,
			)
			inner := forkConfigDependencyMacroOverlay(t, stage+" inner fork", outer)
			innerOracle := applyRandomConfigDependencyMacroOverlayBranchProgram(
				t, random, stage+" inner", inner, outerOracle, names, configNames,
			)
			if !outer.commitBranch(inner) {
				t.Fatalf("%s: inner production commit failed", stage)
			}
			requireConfigDependencyMacroOverlayConsumed(t, stage+" inner commit", inner)
			if !fixture.state.commitBranch(outer) {
				t.Fatalf("%s: outer production commit failed", stage)
			}
			requireConfigDependencyMacroOverlayConsumed(t, stage+" outer commit", outer)
			fixture.oracle = innerOracle
		case 4:
			left := forkConfigDependencyMacroOverlay(t, stage+" left fork", fixture.state)
			right := forkConfigDependencyMacroOverlay(t, stage+" right fork", fixture.state)
			leftOracle := applyRandomConfigDependencyMacroOverlayBranchProgram(
				t, random, stage+" left", left, fixture.oracle, names, configNames,
			)
			rightOracle := applyRandomConfigDependencyMacroOverlayBranchProgram(
				t, random, stage+" right", right, fixture.oracle, names, configNames,
			)
			fixture.state.mergePossibleBranches(left, right)
			var valid bool
			fixture.oracle, valid = configDependencyJoinMacroSnapshots(leftOracle, rightOracle)
			if !valid {
				t.Fatalf("%s: snapshot oracle rejected two-arm join", stage)
			}
			requireConfigDependencyMacroOverlayConsumed(t, stage+" left join", left)
			requireConfigDependencyMacroOverlayConsumed(t, stage+" right join", right)
		case 5:
			branch := forkConfigDependencyMacroOverlay(t, stage+" optional fork", fixture.state)
			branchOracle := applyRandomConfigDependencyMacroOverlayBranchProgram(
				t, random, stage+" optional", branch, fixture.oracle, names, configNames,
			)
			if random.Intn(2) == 0 {
				fixture.state.mergePossibleBranches(nil, branch)
			} else {
				fixture.state.mergePossibleBranches(branch, nil)
			}
			var valid bool
			fixture.oracle, valid = configDependencyJoinMacroSnapshots(fixture.oracle, branchOracle)
			if !valid {
				t.Fatalf("%s: snapshot oracle rejected nil-arm join", stage)
			}
			requireConfigDependencyMacroOverlayConsumed(t, stage+" optional join", branch)
		}
		assertConfigDependencyMacroOverlayMatchesSnapshot(
			t, stage+" complete", fixture.state, fixture.oracle, names,
		)
	}
}

func TestConfigDependencyMacroStateOverlayForkRevisionRejectsStaleChildren(t *testing.T) {
	for _, test := range []struct {
		name string
		use  func(*configDependencyMacroState, *configDependencyMacroState) bool
	}{
		{
			name: "commit",
			use: func(parent, child *configDependencyMacroState) bool {
				return parent.commitBranch(child)
			},
		},
		{
			name: "left join",
			use: func(parent, child *configDependencyMacroState) bool {
				parent.mergePossibleBranches(child, nil)
				return !parent.tainted
			},
		},
		{
			name: "right join",
			use: func(parent, child *configDependencyMacroState) bool {
				parent.mergePossibleBranches(nil, child)
				return !parent.tainted
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			child := forkConfigDependencyMacroOverlay(t, test.name, root)
			forkRevision := child.snapshotForkRevision
			child.set("CHILD_WRITE", configDependencyMacroDefined)
			if child.snapshotForkRevision != forkRevision {
				t.Fatalf(
					"child mutation changed captured fork revision: %d -> %d",
					forkRevision,
					child.snapshotForkRevision,
				)
			}
			root.set("PARENT_WRITE", configDependencyMacroDefined)
			if root.snapshotRevision == forkRevision {
				t.Fatal("parent semantic mutation did not advance its snapshot revision")
			}
			if test.use(root, child) {
				t.Fatal("production state accepted a child forked from a stale revision")
			}
			if !root.tainted {
				t.Fatal("stale child did not fail the production parent closed")
			}
		})
	}

	t.Run("nested commit", func(t *testing.T) {
		root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
		outer := forkConfigDependencyMacroOverlay(t, "outer", root)
		inner := forkConfigDependencyMacroOverlay(t, "inner", outer)
		outer.applyValidatedNumericMacroHeader()
		if outer.commitBranch(inner) {
			t.Fatal("nested commit accepted a child forked from a stale outer revision")
		}
		if !outer.tainted {
			t.Fatal("stale nested child did not fail its parent closed")
		}
	})
}

func TestConfigDependencyMacroStateConsumedChildIsInvalid(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	child := forkConfigDependencyMacroOverlay(t, "consumed child", root)
	child.set("CHILD_ONLY", configDependencyMacroDefined)
	root.mergePossibleBranch(child)
	requireConfigDependencyMacroOverlayConsumed(t, "joined child", child)

	if got := child.definition("CHILD_ONLY"); got != configDependencyMacroUnknown {
		t.Fatalf("consumed child definition = %d, want unknown", got)
	}
	if _, valid := configDependencyConditionalMacroStateKey(child); valid {
		t.Fatal("consumed child produced a recursive-state projection")
	}
	if descendant := child.branch(); descendant != nil {
		t.Fatalf("consumed child produced a live descendant: %#v", descendant)
	}
	child.set("AFTER_CONSUME", configDependencyMacroDefined)
	if !child.tainted {
		t.Fatal("mutation of a consumed child did not fail closed")
	}
	if got := root.definition("AFTER_CONSUME"); got != configDependencyMacroUndefined {
		t.Fatalf("consumed-child mutation changed parent: %d", got)
	}
}
