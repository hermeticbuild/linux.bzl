package kconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func executionCutFamilyForTest(t *testing.T, snapshots ...ActionPlanSnapshot) *ActionPlanFamily {
	t.Helper()
	variants := make([]ActionPlanFamilyVariant, len(snapshots))
	for index, snapshot := range snapshots {
		variants[index] = ActionPlanFamilyVariant{Name: "variant-" + planOrdinal(index), Snapshot: snapshot}
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	return family
}

func executionCutOpaqueSnapshotForTest(snapshot ActionPlanSnapshot) ActionPlanSnapshot {
	snapshot.ConfigDependencies = maps.Clone(snapshot.ConfigDependencies)
	for id, set := range snapshot.ConfigDependencies {
		set.Opaque = true
		set.Reason = "initial conservative execution"
		snapshot.ConfigDependencies[id] = set
	}
	return snapshot
}

func executionCutRootsForTest(family *ActionPlanFamily, pathname string) []ActionPlanFamilyExecutionCutRoot {
	roots := []ActionPlanFamilyExecutionCutRoot{}
	for _, node := range family.Nodes {
		for slot, output := range node.Outputs {
			if output.Path == pathname && output.ObservedPath == "" {
				roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: node.ID, Slot: slot})
			}
		}
	}
	return roots
}

func TestActionPlanFamilyExecutionCutRetainsOriginalSharedBindingsAndAllOutputs(t *testing.T) {
	left := executionCutOpaqueSnapshotForTest(familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), true))
	right := executionCutOpaqueSnapshotForTest(familyTestObservedStateSnapshot(t, strings.Repeat("b", 64), true))
	family := executionCutFamilyForTest(t, left, right)
	roots := executionCutRootsForTest(family, "arch/x86/boot/bzImage")
	if len(roots) != 1 {
		t.Fatalf("fixture must reduce two original chains to one: %v", roots)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.NodeIDs()) != 2 || len(cut.Outputs()) != 2 || len(cut.Origins()) != 4 {
		t.Fatalf("incomplete shared closure: %+v", cut.contract)
	}
	for variant, snapshot := range map[string]ActionPlanSnapshot{"variant-00000000": left, "variant-00000001": right} {
		for _, original := range snapshot.Nodes {
			found := false
			for _, origin := range cut.Origins() {
				if origin.Variant == variant && origin.OriginalNodeID == original.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("original snapshot identity %s/%s was lost", variant, original.ID)
			}
		}
	}
	observed := 0
	for _, output := range cut.Outputs() {
		if output.Output.ObservedPath != "" {
			observed++
		}
	}
	if observed != 1 {
		t.Fatalf("observed output count = %d, want 1", observed)
	}
	ids, err := cut.Verify(family)
	if err != nil || !slices.Equal(ids, cut.NodeIDs()) {
		t.Fatalf("verify = %v, %v", ids, err)
	}
}

func TestActionPlanFamilyExecutionCutIsDetachedAndCanonical(t *testing.T) {
	snapshot := executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1))
	family := executionCutFamilyForTest(t, snapshot)
	roots := executionCutRootsForTest(family, "drivers/final.o")
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	before, err := cut.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.contract.InputSets) == 0 || len(cut.contract.Sources) != 1 {
		t.Fatal("persistent producer/source fixture did not reach seal")
	}
	exposed, _ := cut.CanonicalJSON()
	exposed[0] = '!'
	cut.Roots()[0].NodeID = "changed"
	cut.Origins()[0].OriginalNodeID = "changed"
	cut.Outputs()[0].Output.Path = "changed"
	cut.NodeIDs()[0] = "changed"
	roots[0].NodeID = "changed"
	family.Nodes[0].Outputs[0].Path = "changed"
	for id, recipe := range family.Recipes {
		recipe.Arguments[0] = "changed"
		family.Recipes[id] = recipe
		break
	}
	for id, node := range family.InputSets {
		node.Entries[0].Target.Path = "changed"
		family.InputSets[id] = node
		break
	}
	after, _ := cut.CanonicalJSON()
	if !bytes.Equal(before, after) {
		t.Fatal("captured contract aliases caller or accessor memory")
	}
	fresh := executionCutFamilyForTest(t, snapshot)
	if _, err := cut.Verify(fresh); err != nil {
		t.Fatal(err)
	}
	other, err := NewActionPlanFamilyExecutionCut(fresh, cut.Roots())
	if err != nil || other.ID() != cut.ID() {
		t.Fatalf("canonical fresh seal changed: %v", err)
	}
}

func TestActionPlanFamilyExecutionCutIncludesReducedImplicitPriorInputs(t *testing.T) {
	snapshot := familyTestOpaqueWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))
	for _, node := range snapshot.Nodes {
		if node.Kind == "compile" && len(node.Inputs) != 0 {
			t.Fatal("fixture has explicit pre-reduction edges")
		}
	}
	family := executionCutFamilyForTest(t, snapshot)
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, "drivers/example.o"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.NodeIDs()) != 2 {
		t.Fatalf("implicit prep producer missing from cut: %v", cut.NodeIDs())
	}
	found := false
	for _, output := range cut.Outputs() {
		if output.Output.Path == "include/generated/full-config.h" {
			found = true
		}
	}
	if !found {
		t.Fatal("whole-family prior-tree lowering was not retained")
	}

	// An extracted raw graph cannot reproduce that execution contract: its
	// opaque prior-tree candidate universe has been changed before reduction.
	extracted := snapshot
	extracted.Nodes = nil
	extracted.ConfigDependencies = map[string]ConfigDependencySet{}
	for _, node := range snapshot.Nodes {
		if node.Kind == "compile" {
			extracted.Nodes = append(extracted.Nodes, node)
			extracted.ConfigDependencies[node.ID] = snapshot.ConfigDependencies[node.ID]
		}
	}
	cutOnly := executionCutFamilyForTest(t, extracted)
	if ids, err := cut.Verify(cutOnly); err == nil || ids != nil {
		t.Fatalf("extracted re-reduction accepted: %v %v", ids, err)
	}
}

func TestActionPlanFamilyExecutionCutAllowsNonCutPrecisionChanges(t *testing.T) {
	snapshot := familyTestMixedPrecisionSnapshot(t, familyTestConfig("y", "n"))
	initial := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(snapshot))
	cut, err := NewActionPlanFamilyExecutionCut(initial, executionCutRootsForTest(initial, "drivers/opaque.o"))
	if err != nil {
		t.Fatal(err)
	}
	final := executionCutFamilyForTest(t, snapshot)
	if ids, err := cut.Verify(final); err != nil || len(ids) != 1 {
		t.Fatalf("non-cut precision disturbed pinned opaque node: %v %v", ids, err)
	}
}

func TestActionPlanFamilyExecutionCutSharedRootUnionIsCanonical(t *testing.T) {
	snapshot := executionCutOpaqueSnapshotForTest(familyTestMixedPrecisionSnapshot(t, familyTestConfig("y", "n")))
	family := executionCutFamilyForTest(t, snapshot, snapshot)
	roots := append(executionCutRootsForTest(family, "drivers/opaque.o"), executionCutRootsForTest(family, "drivers/example.o")...)
	if len(roots) != 2 {
		t.Fatalf("fixture roots = %v", roots)
	}
	union := map[string]bool{}
	for _, root := range roots {
		part, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{root})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range part.NodeIDs() {
			union[id] = true
		}
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cut.NodeIDs(), slices.Sorted(maps.Keys(union))) || len(cut.Origins()) != 4 {
		t.Fatalf("shared union lost nodes or original bindings: %v %v", cut.NodeIDs(), cut.Origins())
	}
	slices.Reverse(roots)
	reversed, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil || reversed.ID() != cut.ID() {
		t.Fatalf("root order changed canonical union: %v", err)
	}
}

func TestActionPlanFamilyExecutionCutActivatesApplicableValidation(t *testing.T) {
	snapshot := executionCutOpaqueSnapshotForTest(familyTestProjectedGeneratorWholePrepTreeSnapshot(t, familyTestConfig("y", "n")))
	family := executionCutFamilyForTest(t, snapshot)
	if len(family.Validations) != 1 {
		t.Fatalf("validation fixture has %d roots", len(family.Validations))
	}
	var validator ActionPlanNode
	for _, node := range family.Nodes {
		if node.ID == family.Validations[0].NodeID {
			validator = node
		}
	}
	for _, input := range validator.Inputs {
		t.Run(input.Role, func(t *testing.T) {
			cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: input.ProducerID, Slot: input.Slot}})
			if err != nil {
				t.Fatal(err)
			}
			if len(cut.contract.Validations) != 1 || !slices.Contains(cut.NodeIDs(), validator.ID) {
				t.Fatal("compared producer lost validation obligation")
			}
			for _, compared := range validator.Inputs {
				if !slices.Contains(cut.NodeIDs(), compared.ProducerID) {
					t.Fatal("comparison did not close over both producers")
				}
			}
		})
	}
}

func TestActionPlanFamilyExecutionCutRejectsChangedContracts(t *testing.T) {
	snapshot := executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1))
	makeFamily := func() *ActionPlanFamily { return executionCutFamilyForTest(t, snapshot) }
	initial := makeFamily()
	cut, err := NewActionPlanFamilyExecutionCut(initial, executionCutRootsForTest(initial, "drivers/final.o"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ActionPlanFamily){
		"node": func(f *ActionPlanFamily) { f.Nodes[0].Kind = "copy" },
		"recipe": func(f *ActionPlanFamily) {
			for id, recipe := range f.Recipes {
				recipe.Arguments = append(recipe.Arguments, "changed")
				f.Recipes[id] = recipe
				return
			}
		},
		"source":  func(f *ActionPlanFamily) { f.Sources[0].Path = "include/changed.h" },
		"toolset": func(f *ActionPlanFamily) { f.Toolsets["host"] = "sha256-" + strings.Repeat("8", 64) },
		"inputset": func(f *ActionPlanFamily) {
			for id, set := range f.InputSets {
				set.Entries[0].Target.Path = "changed"
				f.InputSets[id] = set
				return
			}
		},
		"output": func(f *ActionPlanFamily) { f.Nodes[0].Outputs[0].ArtifactPath = "changed" },
		"missing-origin": func(f *ActionPlanFamily) {
			for _, originals := range f.originalNodeIDs {
				for id := range originals {
					delete(originals, id)
					return
				}
			}
		},
		"changed-origin": func(f *ActionPlanFamily) {
			for _, originals := range f.originalNodeIDs {
				for id, final := range originals {
					delete(originals, id)
					originals[strings.Repeat("7", 64)] = final
					return
				}
			}
		},
		"missing-provenance": func(f *ActionPlanFamily) { f.originalNodeIDs = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			final := makeFamily()
			mutate(final)
			if ids, err := cut.Verify(final); err == nil || ids != nil {
				t.Fatalf("changed %s returned pins %v, error %v", name, ids, err)
			}
		})
	}
}

func TestActionPlanFamilyExecutionCutRejectsChangedCapsule(t *testing.T) {
	left := executionCutFamilyForTest(t, familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Opaque: true, Reason: "full"}))
	cut, err := NewActionPlanFamilyExecutionCut(left, executionCutRootsForTest(left, "drivers/example.o"))
	if err != nil {
		t.Fatal(err)
	}
	right := executionCutFamilyForTest(t, familyTestSnapshot(t, 1, "", familyTestConfig("y", "y"), ConfigDependencySet{Opaque: true, Reason: "full"}))
	if ids, err := cut.Verify(right); err == nil || ids != nil {
		t.Fatal("changed full configuration accepted")
	}
}

func TestActionPlanFamilyExecutionCutEmptyAndInvalidRoots(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), true)))
	empty, err := NewActionPlanFamilyExecutionCut(family, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{})
	if err != nil || empty.ID() != other.ID() {
		t.Fatalf("empty cut is not canonical: %v", err)
	}
	if len(empty.NodeIDs()) != 0 || len(empty.Outputs()) != 0 || len(empty.Origins()) != 0 {
		t.Fatal("empty cut contains execution authority")
	}
	family.originalNodeIDs = nil
	if ids, err := empty.Verify(family); err != nil || len(ids) != 0 {
		t.Fatalf("empty provenance subset is not vacuous: %v %v", ids, err)
	}
	family.Toolsets["target"] = "sha256-" + strings.Repeat("f", 64)
	if ids, err := empty.Verify(family); err == nil || ids != nil {
		t.Fatal("empty cut ignored changed toolset")
	}

	family = executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), true)))
	root := executionCutRootsForTest(family, "arch/x86/boot/bzImage")[0]
	for name, roots := range map[string][]ActionPlanFamilyExecutionCutRoot{
		"missing": {{NodeID: strings.Repeat("0", 64)}}, "negative": {{NodeID: root.NodeID, Slot: -1}},
		"slot": {{NodeID: root.NodeID, Slot: 1}}, "duplicate": {root, root},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewActionPlanFamilyExecutionCut(family, roots); err == nil {
				t.Fatal("invalid roots accepted")
			}
		})
	}
	for _, node := range family.Nodes {
		if node.Outputs[0].ObservedPath != "" {
			if _, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: node.ID}}); err == nil {
				t.Fatal("observed envelope accepted as ordinary root")
			}
		}
	}
	var zero ActionPlanFamilyExecutionCut
	if ids, err := zero.Verify(family); err == nil || ids != nil {
		t.Fatal("uninitialized cut accepted")
	}
	if _, err := zero.CanonicalJSON(); err == nil {
		t.Fatal("uninitialized seal accepted")
	}
	if _, err := NewActionPlanFamilyExecutionCut(nil, nil); err == nil {
		t.Fatal("nil family accepted")
	}
}

func TestActionPlanFamilyExecutionCutBounds(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
	roots := executionCutRootsForTest(family, "drivers/final.o")
	for name, change := range map[string]func(*actionPlanFamilyExecutionCutLimits){
		"nodes":   func(l *actionPlanFamilyExecutionCutLimits) { l.nodes = 1 },
		"outputs": func(l *actionPlanFamilyExecutionCutLimits) { l.outputs = 1 },
		"records": func(l *actionPlanFamilyExecutionCutLimits) { l.records = 1 },
		"bytes":   func(l *actionPlanFamilyExecutionCutLimits) { l.bytes = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := defaultActionPlanFamilyExecutionCutLimits()
			change(&limits)
			var budget *actionPlanFamilyExecutionCutBudgetError
			if _, err := newActionPlanFamilyExecutionCut(family, roots, limits); !errors.As(err, &budget) {
				t.Fatalf("cut limit must be distinguishable from a malformed contract: %v", err)
			}
		})
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.inputSets = len(cut.contract.InputSets)
	boundary, err := newActionPlanFamilyExecutionCut(family, roots, limits)
	if err != nil || !reflect.DeepEqual(boundary.NodeIDs(), cut.NodeIDs()) {
		t.Fatalf("exact input-set bound failed: %v", err)
	}
	canonical, _ := cut.CanonicalJSON()
	limits = defaultActionPlanFamilyExecutionCutLimits()
	limits.bytes = len(canonical)
	if _, err := newActionPlanFamilyExecutionCut(family, roots, limits); err != nil {
		t.Fatalf("exact byte bound failed: %v", err)
	}
	limits.bytes--
	if _, err := newActionPlanFamilyExecutionCut(family, roots, limits); err == nil {
		t.Fatal("one-byte canonical overflow accepted")
	}
}

func TestActionPlanFamilyExecutionCutBoundsSharedInputSetSubtrees(t *testing.T) {
	base := familyTestInputSetSnapshot(t, 1)
	plan := snapshotActionPlan(base)
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.InputSet == "" {
			continue
		}
		for ordinal := 0; ordinal < 40; ordinal++ {
			node.InputSet, err = store.Insert(node.InputSet, ActionPlanInputSetEntry{
				Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "extra/" + planOrdinal(ordinal)}, SourceID: plan.Sources[0].ID,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full"}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, base.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	family := executionCutFamilyForTest(t, snapshot)
	roots := executionCutRootsForTest(family, "drivers/final.o")
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.contract.InputSets) <= 1 {
		t.Fatal("fixture did not create shared radix branches")
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.inputSets = 1
	if _, err := newActionPlanFamilyExecutionCut(family, roots, limits); err == nil || !strings.Contains(err.Error(), "input-set nodes") {
		t.Fatalf("input-set bound error = %v", err)
	}
}

func TestActionPlanFamilyExecutionCutRejectsAdditionalOriginalBinding(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, "drivers/final.o"))
	if err != nil {
		t.Fatal(err)
	}
	origin := cut.Origins()[0]
	family.originalNodeIDs[origin.Variant][strings.Repeat("f", 64)] = origin.NodeID
	if ids, err := cut.Verify(family); err == nil || ids != nil {
		t.Fatalf("new origin omitted from comparison: %v %v", ids, err)
	}
}

func TestActionPlanFamilyExecutionCutEmptyBindsVariantSet(t *testing.T) {
	snapshot := executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1))
	one := executionCutFamilyForTest(t, snapshot)
	empty, err := NewActionPlanFamilyExecutionCut(one, nil)
	if err != nil {
		t.Fatal(err)
	}
	two := executionCutFamilyForTest(t, snapshot, snapshot)
	if ids, err := empty.Verify(two); err == nil || ids != nil {
		t.Fatalf("empty cut ignored new variant: %v %v", ids, err)
	}
}

func TestActionPlanFamilyExecutionCutPrunesInactiveVariantOrigins(t *testing.T) {
	right := familyTestOpaqueWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))
	plan := snapshotActionPlan(right)
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.Kind != "compile" {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		recipe.Trees, recipe.WorkingTrees = nil, nil
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id] = recipe
		node.Recipe, node.Trees = id, nil
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "conservative"}
	}
	left, err := canonicalActionPlanSnapshot(plan, dependencies, right.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	family := executionCutFamilyForTest(t, left, right)
	roots := executionCutRootsForTest(family, "include/generated/full-config.h")
	if len(roots) != 1 || !slices.Equal(family.Memberships[roots[0].NodeID], []string{"variant-00000001"}) {
		t.Fatalf("fixture did not prune only the first variant's shared producer: %v", family.Memberships)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.Origins()) != 1 || cut.Origins()[0].Variant != "variant-00000001" {
		t.Fatalf("inactive snapshot origins survived: %v", cut.Origins())
	}
	if _, err := cut.Verify(family); err != nil {
		t.Fatal(err)
	}
}

func executionCutWriteTransportForTest(t *testing.T, data []byte) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "cut.json")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestActionPlanFamilyExecutionCutTransportReconstructsCanonicalAuthority(t *testing.T) {
	for _, empty := range []bool{false, true} {
		family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
		roots := executionCutRootsForTest(family, "drivers/final.o")
		if empty {
			roots = nil
		}
		cut, err := NewActionPlanFamilyExecutionCut(family, roots)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := cut.CanonicalJSON()
		filename := executionCutWriteTransportForTest(t, data)
		decoded, err := ReadActionPlanFamilyExecutionCut(family, filename)
		if err != nil || decoded.ID() != cut.ID() || !reflect.DeepEqual(decoded.Origins(), cut.Origins()) {
			t.Fatalf("read empty=%t: %v %v", empty, decoded, err)
		}
		if _, err := decoded.Verify(family); err != nil {
			t.Fatal(err)
		}
		for tool := range family.Toolsets {
			family.Toolsets[tool] = "sha256-" + strings.Repeat("a", 64)
			break
		}
		if got, err := ReadActionPlanFamilyExecutionCut(family, filename); err == nil || got != nil {
			t.Fatalf("transport trusted changed complete family: %v %v", got, err)
		}
	}
}

func TestActionPlanFamilyExecutionCutTransportRejectsAssertionsAndAlternateEncoding(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, "drivers/final.o"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := cut.CanonicalJSON()
	mutated := func(edit func(*actionPlanFamilyExecutionCutContract)) []byte {
		var contract actionPlanFamilyExecutionCutContract
		if err := json.Unmarshal(data, &contract); err != nil {
			t.Fatal(err)
		}
		edit(&contract)
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(contract); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	cases := map[string][]byte{
		"unknown field":          append([]byte("{\"untrusted\":true,"), data[1:]...),
		"duplicate field":        append([]byte("{\"schema\":\""+LinuxKernelFamilyExecutionCutSchema+"\","), data[1:]...),
		"duplicate nested field": bytes.Replace(data, []byte("\"node_id\":"), []byte("\"node_id\":\"ignored\",\"node_id\":"), 1),
		"trailing value":         append(slices.Clone(data), []byte("{}\n")...),
		"leading space":          append([]byte(" "), data...),
		"missing newline":        slices.Clone(data[:len(data)-1]),
		"null":                   []byte("null\n"),
		"truncated":              slices.Clone(data[:len(data)/2]),
		"changed source":         mutated(func(c *actionPlanFamilyExecutionCutContract) { c.Sources[0].Path += ".changed" }),
		"changed output":         mutated(func(c *actionPlanFamilyExecutionCutContract) { c.Nodes[0].Node.Outputs[0].Path += ".changed" }),
		"changed origin":         mutated(func(c *actionPlanFamilyExecutionCutContract) { c.Origins[0].OriginalNodeID = strings.Repeat("f", 64) }),
		"omitted dependency":     mutated(func(c *actionPlanFamilyExecutionCutContract) { c.Nodes = c.Nodes[:1] }),
		"unavailable root":       mutated(func(c *actionPlanFamilyExecutionCutContract) { c.Roots[0].Slot = 100 }),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ReadActionPlanFamilyExecutionCut(family, executionCutWriteTransportForTest(t, input))
			if err == nil || got != nil {
				t.Fatalf("accepted untrusted transport: %v %v", got, err)
			}
		})
	}
	if got, err := ReadActionPlanFamilyExecutionCut(family, t.TempDir()); err == nil || got != nil {
		t.Fatalf("accepted directory: %v %v", got, err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxActionPlanFamilyExecutionCutBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadActionPlanFamilyExecutionCut(family, oversized); err == nil || got != nil {
		t.Fatalf("accepted oversized sparse file: %v %v", got, err)
	}
}

func TestActionPlanFamilyExecutionCutTransportBoundsBeforeMaterializingAssertions(t *testing.T) {
	// Deliberately invalid past the limit: the collection bound must stop the
	// streaming reader before either parsing the tail or allocating typed nodes.
	nodes := []byte(`{"nodes":[` + strings.Repeat(`{},`, MaxActionPlanFamilyExecutionCutNodes) + `INVALID]}`)
	if _, err := readActionPlanFamilyExecutionCutRoots(nodes); err == nil || !strings.Contains(err.Error(), "collection exceeds") {
		t.Fatalf("node collection was not bounded before tail parsing: %v", err)
	}
	roots := []byte(`{"roots":[` + strings.Repeat(`{},`, MaxActionPlanFamilyExecutionCutOutputs) + `INVALID]}`)
	if _, err := readActionPlanFamilyExecutionCutRoots(roots); err == nil || !strings.Contains(err.Error(), "roots") {
		t.Fatalf("root collection was not bounded before tail parsing: %v", err)
	}
	nested := []byte(`{"nodes":` + strings.Repeat(`[`, 65) + strings.Repeat(`]`, 65) + `}`)
	if _, err := readActionPlanFamilyExecutionCutRoots(nested); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("unbounded malformed nesting accepted: %v", err)
	}
}

func executionCutMarkerPathsForTest(t *testing.T, directory string) []string {
	t.Helper()
	paths := []string{}
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) != 0 {
			t.Fatalf("invalid marker %s: %q %v", path, data, err)
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

func TestActionPlanFamilyExecutionCutMarkersVerifyBeforePublication(t *testing.T) {
	for _, empty := range []bool{false, true} {
		family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
		roots := executionCutRootsForTest(family, "drivers/final.o")
		if empty {
			roots = nil
		}
		cut, err := NewActionPlanFamilyExecutionCut(family, roots)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"cut", "pinned"} {
			directory := filepath.Join(t.TempDir(), "execution")
			if mode == "cut" {
				err = cut.WriteSelectionMarkers(directory)
			} else {
				err = cut.WritePinnedMarkers(family, directory)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"schema/" + LinuxKernelFamilyExecutionSchema, "mode/" + mode, "seal/" + cut.ID()}
			for _, id := range cut.NodeIDs() {
				want = append(want, mode+"/"+id)
			}
			slices.Sort(want)
			if got := executionCutMarkerPathsForTest(t, directory); !slices.Equal(got, want) {
				t.Fatalf("markers empty=%t mode=%s: %v want %v", empty, mode, got, want)
			}
			if err := cut.WriteSelectionMarkers(directory); err == nil {
				t.Fatal("overwrote nonempty marker directory")
			}
		}
		for tool := range family.Toolsets {
			family.Toolsets[tool] = "sha256-" + strings.Repeat("a", 64)
			break
		}
		directory := filepath.Join(t.TempDir(), "must-not-exist", "execution")
		if err := cut.WritePinnedMarkers(family, directory); err == nil {
			t.Fatal("published changed family pin")
		}
		if _, err := os.Lstat(filepath.Dir(directory)); !os.IsNotExist(err) {
			t.Fatalf("verification failure touched output parent: %v", err)
		}
	}
	directory := filepath.Join(t.TempDir(), "invalid")
	if err := new(ActionPlanFamilyExecutionCut).WriteSelectionMarkers(directory); err == nil {
		t.Fatal("zero cut published markers")
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("zero cut touched output: %v", err)
	}
}
