package kconfig

import (
	"fmt"
	"math/rand"
	"testing"
)

type configDependencyMacroSpecializationRandomOp struct {
	kind       int
	name       string
	target     string
	definition configDependencyMacroDefinition
	maybe      bool
	left       []configDependencyMacroSpecializationRandomOp
	right      []configDependencyMacroSpecializationRandomOp
}

// This interpreter executes the generated program afresh. The reference run
// uses the old rematerializing direct-child commit, never a cached transform or
// captured output. Both runs retain the existing read/touch instrumentation and
// possible-branch join semantics; this oracle tests specialization, not the
// independently tested implementation of the underlying macro-cell algebra.
func executeConfigDependencyMacroSpecializationRandomOps(
	t *testing.T,
	state *configDependencyMacroState,
	ops []configDependencyMacroSpecializationRandomOp,
	reference bool,
) {
	t.Helper()
	for _, op := range ops {
		switch op.kind {
		case 0:
			state.definition(op.name)
		case 1:
			_, recorded := state.explicitDefinition(op.name)
			definition := configDependencyMacroUndefined
			if recorded {
				definition = op.definition
			}
			state.set(op.target, definition)
		case 2:
			state.set(op.name, op.definition)
		case 3:
			// The write is definition-equivalent but still an explicit touch;
			// ordinary names also canonicalize their recorded bit here.
			state.set(op.name, state.definition(op.name))
		case 4:
			if !state.applyConfigNameSet([]string{op.name}, op.maybe) {
				t.Fatal("generated positive CONFIG batch failed")
			}
		case 5:
			state.applyValidatedNumericMacroHeader()
		case 6:
			child := state.branch()
			executeConfigDependencyMacroSpecializationRandomOps(t, child, op.left, reference)
			valid := false
			if reference {
				valid = originalConfigDependencyMacroCommitForEffectTest(state, child)
			} else {
				valid = state.commitBranch(child)
			}
			if !valid {
				t.Fatal("generated definite branch failed")
			}
		case 7:
			child := state.branch()
			executeConfigDependencyMacroSpecializationRandomOps(t, child, op.left, reference)
			state.mergePossibleBranch(child)
		case 8:
			left, right := state.branch(), state.branch()
			executeConfigDependencyMacroSpecializationRandomOps(t, left, op.left, reference)
			executeConfigDependencyMacroSpecializationRandomOps(t, right, op.right, reference)
			state.mergePossibleBranches(left, right)
		case 9:
			switch state.definition(op.name) {
			case configDependencyMacroDefined:
				executeConfigDependencyMacroSpecializationRandomOps(t, state, op.left, reference)
			case configDependencyMacroUndefined:
				executeConfigDependencyMacroSpecializationRandomOps(t, state, op.right, reference)
			default:
				left, right := state.branch(), state.branch()
				executeConfigDependencyMacroSpecializationRandomOps(t, left, op.left, reference)
				executeConfigDependencyMacroSpecializationRandomOps(t, right, op.right, reference)
				state.mergePossibleBranches(left, right)
			}
		default:
			t.Fatal("invalid random operation")
		}
		if !state.validSnapshotLineage() {
			t.Fatal("generated operation invalidated the macro-state lineage")
		}
	}
}

func TestConfigDependencyMacroSpecializationRandomProgramsMatchOracle(t *testing.T) {
	const seed int64 = 0x5eedcace
	const programs = 300
	names := []string{
		"A", "B", "C", "D", "CONFIG_A", "CONFIG_B", "CONFIG_C",
		"_A", "_B", "_C", configDependencyResolvedAutoconfGuard,
	}
	configNames := []string{"CONFIG_A", "CONFIG_B", "CONFIG_C"}
	allNames := append(append([]string(nil), names...), "UNREAD", "CONFIG_RANDOM_UNREAD", "_UNREAD")
	generatedKinds := [10]int{}
	seenCells := [configDependencyMacroSnapshotCellCount]bool{}
	var generate func(*rand.Rand, int, int) []configDependencyMacroSpecializationRandomOp
	generate = func(random *rand.Rand, depth, count int) []configDependencyMacroSpecializationRandomOp {
		ops := make([]configDependencyMacroSpecializationRandomOp, count)
		for index := range ops {
			kindCount := len(generatedKinds)
			if depth == 2 {
				kindCount = 6
			}
			cell, _ := configDependencyMacroSnapshotCellAt(random.Intn(configDependencyMacroSnapshotCellCount))
			op := configDependencyMacroSpecializationRandomOp{
				kind: random.Intn(kindCount), name: names[random.Intn(len(names))],
				target:     names[random.Intn(len(names))],
				definition: cell.definition, maybe: random.Intn(2) == 0,
			}
			if op.kind == 4 {
				op.name = configNames[random.Intn(len(configNames))]
			}
			if op.kind >= 6 {
				op.left = generate(random, depth+1, 1+random.Intn(3))
				if op.kind >= 8 {
					op.right = generate(random, depth+1, 1+random.Intn(3))
				}
			}
			generatedKinds[op.kind]++
			ops[index] = op
		}
		return ops
	}
	executedPrograms, acceptedCrossInputs, rejectedChangedReads, normalizedAwayWrites := 0, 0, 0, 0
	for program := 0; program < programs; program++ {
		t.Run(fmt.Sprintf("case_%03d", program), func(t *testing.T) {
			// Every case remains reproducible when selected with go test -run.
			caseSeed := seed + int64(program)
			random := rand.New(rand.NewSource(caseSeed))
			executedPrograms++
			input := newConfigDependencyMacroSnapshot()
			if random.Intn(2) == 0 {
				input, _ = configDependencyMacroStateWithValidatedNumericMacroHeader(input)
			}
			for _, name := range allNames {
				index := random.Intn(configDependencyMacroSnapshotCellCount)
				seenCells[index] = true
				cell, _ := configDependencyMacroSnapshotCellAt(index)
				input, _ = input.withCell(name, cell)
			}
			ops := generate(random, 0, 4+random.Intn(5))
			t.Logf("seed=%x program=%#v", caseSeed, ops)
			capturing := &configDependencyMacroState{snapshot: input}
			capturedChild := capturing.branch()
			if !capturedChild.beginForcedHeaderTrace() {
				t.Fatal("cannot start random program trace")
			}
			executeConfigDependencyMacroSpecializationRandomOps(t, capturedChild, ops, false)
			output, valid := capturedChild.materializedSnapshot()
			if !valid {
				t.Fatal("cannot materialize random program")
			}
			specialization, valid := captureConfigDependencyMacroSpecialization(capturedChild, input, output)
			if !valid {
				t.Fatal("valid finite random program was not captured")
			}
			if specialization.transform.exactSize() > capturedChild.snapshotChanges.exactSize() {
				normalizedAwayWrites++
			}
			capturedChild.endForcedHeaderTrace()
			for variant := 0; variant < 3; variant++ {
				current := newConfigDependencyMacroSnapshot()
				if random.Intn(2) == 0 {
					current, _ = configDependencyMacroStateWithValidatedNumericMacroHeader(current)
				}
				for _, name := range allNames {
					cell, _ := configDependencyMacroSnapshotCellAt(random.Intn(configDependencyMacroSnapshotCellCount))
					current, _ = current.withCell(name, cell)
				}
				// Change a semantically observable, never-read CONFIG cell. It
				// remains different even if the program applies numeric wildcards.
				oldCell, _ := input.lookup("CONFIG_RANDOM_UNREAD")
				oldIndex, _ := configDependencyMacroSnapshotCellIndex(oldCell)
				newCell, _ := configDependencyMacroSnapshotCellAt((oldIndex + variant + 1) % configDependencyMacroSnapshotCellCount)
				current, _ = current.withCell("CONFIG_RANDOM_UNREAD", newCell)
				for _, requirement := range specialization.requirements {
					current, _ = current.withCell(requirement.name, requirement.cell)
				}
				if current == input || !specialization.matches(current) {
					t.Fatal("constructed cross-input candidate does not match its read requirements")
				}
				want := &configDependencyMacroState{snapshot: current}
				wantChild := want.branch()
				// No trace and no cache are installed in the reference run.
				executeConfigDependencyMacroSpecializationRandomOps(t, wantChild, ops, true)
				if !originalConfigDependencyMacroCommitForEffectTest(want, wantChild) {
					t.Fatal("reference random program did not commit")
				}
				parent := &configDependencyMacroState{snapshot: current}
				if variant%2 != 0 {
					parent = parent.branch()
				}
				if !specialization.apply(parent) {
					t.Fatal("matching specialization did not apply")
				}
				actual, valid := parent.materializedSnapshot()
				if !valid || !parent.validSnapshotLineage() {
					t.Fatal("random replay invalidated its parent")
				}
				requireConfigDependencyMacroEffectSnapshotsEqual(t, actual, want.snapshot)
				if got, _ := actual.lookup("CONFIG_RANDOM_UNREAD"); got != newCell {
					t.Fatal("random replay overwrote an unread current-entry cell")
				}
				acceptedCrossInputs++
			}
			if len(specialization.requirements) != 0 {
				requirement := specialization.requirements[random.Intn(len(specialization.requirements))]
				index, _ := configDependencyMacroSnapshotCellIndex(requirement.cell)
				changedCell, _ := configDependencyMacroSnapshotCellAt((index + 1) % configDependencyMacroSnapshotCellCount)
				changed, _ := input.withCell(requirement.name, changedCell)
				parent := &configDependencyMacroState{snapshot: changed}
				if specialization.matches(changed) || specialization.apply(parent) || parent.tainted || parent.snapshot != changed || parent.snapshotRevision != 0 {
					t.Fatal("changed consumed cell was accepted or rejection mutated the entry")
				}
				rejectedChangedReads++
			}
		})
	}
	if acceptedCrossInputs != 3*executedPrograms || rejectedChangedReads < executedPrograms/2 || normalizedAwayWrites < executedPrograms/4 {
		t.Fatalf("insufficient coverage: cross_inputs=%d rejected_reads=%d normalized_away_writes=%d", acceptedCrossInputs, rejectedChangedReads, normalizedAwayWrites)
	}
	for kind, count := range generatedKinds {
		if executedPrograms == programs && count == 0 {
			t.Fatalf("operation kind %d was never generated", kind)
		}
	}
	for cell, seen := range seenCells {
		if executedPrograms == programs && !seen {
			t.Fatalf("cell %d was never generated", cell)
		}
	}
	t.Logf("cross_inputs=%d rejected_reads=%d normalized_away_writes=%d", acceptedCrossInputs, rejectedChangedReads, normalizedAwayWrites)
}
