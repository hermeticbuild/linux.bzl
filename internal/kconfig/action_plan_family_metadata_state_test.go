package kconfig

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func metadataStateVariantForTest(t *testing.T, other, shape string) ActionPlanFamilyVariant {
	t.Helper()
	variant := executedArtifactVariantForTest(t, other)
	plan := snapshotActionPlan(variant.Snapshot)
	sets := make([]ConfigDependencySet, len(plan.Nodes))
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		sets[index] = variant.Snapshot.ConfigDependencies[node.ID]
		if node.Kind != "compile" {
			node.Stage = "target"
			if shape == "prior prehost tree" {
				recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
				node.Trees, recipe.Trees = []string{"prehost"}, []string{"prehost"}
				id, err := recipe.ID()
				if err != nil {
					t.Fatal(err)
				}
				plan.Recipes[id], node.Recipe = recipe, id
			}
			for slot := range node.Outputs {
				node.Outputs[slot].Tree = "metadata"
			}
			if shape != "public state" {
				node.Outputs[1].Path = compactKbuildSideOutputStateDirectory + "/" + strings.Repeat("a", 64) + ".state"
			}
			continue
		}
		// Model a compiler's inherited state without an unrelated executable
		// input: the public generator's ordinary output must remain public but
		// is not otherwise consumed by this compiler.
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		recipe.Arguments = []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte("cc -c drivers/example.c -o drivers/example.o\n"))}
		binding := "observed-state:00000000"
		if shape == "sequence" {
			binding = "sequence:00000000"
		}
		recipe.Inputs = []string{binding}
		recipe.ObservedOutputBases = map[string][]string{"00000001": {binding}}
		recipe.ExecutableInputs = nil
		delete(recipe.WorkingInputs, "input:helper:00000000")
		recipe.CompilerInvocation.WorkingInputUses = slices.DeleteFunc(recipe.CompilerInvocation.WorkingInputUses, func(value string) bool {
			return value == "input:helper:00000000"
		})
		recipe.CompilerInvocation.AuxiliaryWorkingInputUses = nil
		node.Inputs = node.Inputs[1:]
		switch shape {
		case "opaque":
			sets[index] = ConfigDependencySet{Opaque: true, Reason: "unsupported compiler"}
		case "other semantic use":
			recipe.Arguments = append(recipe.Arguments, "${input:"+binding+"}")
		case "sequence":
			node.Inputs[0].Role = "sequence"
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id], node.Recipe = recipe, id
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for index, node := range plan.Nodes {
		dependencies[node.ID] = sets[index]
	}
	var err error
	variant.Snapshot, err = canonicalActionPlanSnapshot(plan, dependencies, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	return variant
}

func TestFamilyExecutedMetadataStatePrivateCopies(t *testing.T) {
	for _, testcase := range []struct {
		name, shape string
		state       toolaction.ObservedOutputDisposition
		wantCopies  int
		wantCompile int
	}{
		{"equal absent envelopes", "", toolaction.ObservedOutputAbsent, 1, 1},
		{"present retains different writers", "", toolaction.ObservedOutputPresent, 2, 2},
		{"deleted retains different writers", "", toolaction.ObservedOutputDeleted, 2, 2},
		{"public state stays public", "public state", toolaction.ObservedOutputAbsent, 0, 2},
		{"opaque compiler", "opaque", toolaction.ObservedOutputAbsent, 0, 2},
		{"other semantic state use", "other semantic use", toolaction.ObservedOutputAbsent, 0, 2},
		{"sequence edge", "sequence", toolaction.ObservedOutputAbsent, 0, 2},
		{"pinned prior-tree consumer", "prior prehost tree", toolaction.ObservedOutputAbsent, 0, 2},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			variants := []ActionPlanFamilyVariant{
				metadataStateVariantForTest(t, "0", testcase.shape),
				metadataStateVariantForTest(t, "1", testcase.shape),
			}
			initial, err := BuildConservativeActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			var roots []ActionPlanFamilyExecutionCutRoot
			for _, variant := range variants {
				for _, node := range variant.Snapshot.Nodes {
					if node.Kind == "generate" {
						roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: initial.originalNodeIDs[variant.Name][node.ID], Slot: 0})
					}
				}
			}
			cut, err := NewActionPlanFamilyExecutionCut(initial, roots)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := observeExecutedArtifacts(cut, executedArtifactStoresForTest(t, cut, testcase.state), 1024, 8192)
			if err != nil {
				t.Fatal(err)
			}
			var inputs []actionPlanFamilyBuildVariant
			for _, variant := range variants {
				plan := snapshotActionPlan(variant.Snapshot)
				replay, err := cut.VerifyVariantReplay(variant.Name, variant.Snapshot, plan, variant.Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				updated, dependencies, err := substituteExecutedArtifacts(plan, variant.Snapshot.ConfigDependencies, replay, observed)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := canonicalActionPlanSnapshot(updated, dependencies, variant.Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, actionPlanFamilyBuildVariant{variant: ActionPlanFamilyVariant{Name: variant.Name, Snapshot: snapshot}, executionCut: cut})
			}
			built, err := buildValidatedActionPlanFamily(inputs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cut.Verify(built.family); err != nil {
				t.Fatalf("changed original execution: %v", err)
			}
			copies, compilers := 0, 0
			for _, node := range built.family.Nodes {
				if node.Kind == "compile" {
					compilers++
				}
				if len(node.Outputs) == 1 && strings.HasPrefix(node.Outputs[0].Path, "executed-artifacts/") {
					copies++
					if node.Stage != "prehost" || node.Outputs[0].Tree != "prehost" || len(node.Inputs) != 0 {
						t.Fatalf("copy is not private content-only work: %+v", node)
					}
				}
			}
			if copies != testcase.wantCopies || compilers != testcase.wantCompile {
				t.Fatalf("copies=%d compilers=%d; want %d/%d", copies, compilers, testcase.wantCopies, testcase.wantCompile)
			}
			for _, view := range built.family.Views {
				if strings.HasPrefix(view.ArtifactPath, ".linux-bzl-executed-artifacts/") {
					t.Fatal("private copy leaked into public views")
				}
			}
			for _, root := range roots {
				for slot := range 2 {
					if !slices.ContainsFunc(built.family.Views, func(view ActionPlanFamilyView) bool {
						return view.NodeID == root.NodeID && view.Slot == slot && view.Tree == "metadata"
					}) {
						t.Fatalf("lost original public output %s[%d]", root.NodeID, slot)
					}
				}
			}
		})
	}
}
