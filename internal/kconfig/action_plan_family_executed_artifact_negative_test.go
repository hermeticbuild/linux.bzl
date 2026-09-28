package kconfig

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// These independent negatives exercise the temporary prototype only. They do
// not grant the caller-supplied dependency annotations production authority.
func reviewArtifactVariant(t *testing.T, change func(*ActionPlanNode, *ActionRecipe, *ActionPlanNode)) ActionPlanFamilyVariant {
	t.Helper()
	variant := executedArtifactVariantForTest(t, "0")
	plan := snapshotActionPlan(variant.Snapshot)
	var consumer, producer *ActionPlanNode
	before := make([]ConfigDependencySet, len(plan.Nodes))
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		before[index] = variant.Snapshot.ConfigDependencies[node.ID]
		if node.Kind == "compile" {
			consumer = node
		} else {
			producer = node
		}
	}
	if consumer == nil || producer == nil {
		t.Fatal("missing fixture consumer or producer")
	}
	recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
	change(consumer, &recipe, producer)
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[recipeID], consumer.Recipe = recipe, recipeID
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sets := map[string]ConfigDependencySet{}
	for index, node := range plan.Nodes {
		sets[node.ID] = before[index]
	}
	variant.Snapshot, err = canonicalActionPlanSnapshot(plan, sets, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	return variant
}

type reviewArtifactFixture struct {
	variant    ActionPlanFamilyVariant
	family     *ActionPlanFamily
	cut        *ActionPlanFamilyExecutionCut
	plan       *ActionPlan
	replay     *ActionPlanFamilyVerifiedReplay
	observed   *ActionPlanFamilyExecutedArtifacts
	producerID string
}

func reviewArtifactSetup(t *testing.T, variant ActionPlanFamilyVariant) reviewArtifactFixture {
	t.Helper()
	family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{variant})
	if err != nil {
		t.Fatal(err)
	}
	producerID := ""
	for _, node := range variant.Snapshot.Nodes {
		if node.Kind == "generate" {
			producerID = node.ID
		}
	}
	if producerID == "" {
		t.Fatal("missing fixture generator")
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: family.originalNodeIDs[variant.Name][producerID], Slot: 0}})
	if err != nil {
		t.Fatal(err)
	}
	plan := snapshotActionPlan(variant.Snapshot)
	replay, err := cut.VerifyVariantReplay(variant.Name, variant.Snapshot, plan, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := observeExecutedArtifacts(cut, executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputAbsent), 1024, 8192)
	if err != nil {
		t.Fatal(err)
	}
	return reviewArtifactFixture{variant, family, cut, plan, replay, observed, producerID}
}

func reviewArtifactConsumer(t *testing.T, plan *ActionPlan) *ActionPlanNode {
	t.Helper()
	for index := range plan.Nodes {
		if plan.Nodes[index].Kind == "compile" {
			return &plan.Nodes[index]
		}
	}
	t.Fatal("missing fixture compiler")
	return nil
}

func reviewArtifactAssertEdges(t *testing.T, fixture reviewArtifactFixture, updated *ActionPlan, sets map[string]ConfigDependencySet, retained [2]bool) *ActionPlanFamily {
	t.Helper()
	if err := fixture.replay.validateCurrent(fixture.plan); err != nil {
		t.Fatalf("substitution mutated its original replay plan: %v", err)
	}
	consumer := reviewArtifactConsumer(t, updated)
	if len(consumer.Inputs) != 2 {
		t.Fatalf("got %d compiler inputs, want both original roles", len(consumer.Inputs))
	}
	for ordinal, wantRetained := range retained {
		edge := consumer.Inputs[ordinal]
		actualRetained := edge.ProducerID == fixture.producerID && edge.Slot == ordinal
		if actualRetained != wantRetained {
			t.Fatalf("input %d retained=%v, want %v; edge=%+v", ordinal, actualRetained, wantRetained, edge)
		}
	}
	// The complete final reducer must still retain and verify the original
	// execution, rather than testing only a locally rewritten node vector.
	snapshot, err := canonicalActionPlanSnapshot(updated, sets, fixture.variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	built, err := buildValidatedActionPlanFamily([]actionPlanFamilyBuildVariant{{variant: ActionPlanFamilyVariant{Name: fixture.variant.Name, Snapshot: snapshot}, executionCut: fixture.cut}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.cut.Verify(built.family); err != nil {
		t.Fatalf("pinned execution changed: %v", err)
	}
	for _, view := range built.family.Views {
		if strings.HasPrefix(view.ArtifactPath, ".linux-bzl-executed-artifacts/") {
			t.Fatal("unowned copy output escaped into a public view")
		}
	}
	return built.family
}

func TestReviewExecutedArtifactsRejectChangedAuthority(t *testing.T) {
	for _, failure := range []string{"nil replay", "nil observation", "other cut", "other plan pointer", "changed recipe", "changed source", "changed edge", "changed toolset", "missing consumer annotation", "missing producer annotation"} {
		t.Run(failure, func(t *testing.T) {
			fixture := reviewArtifactSetup(t, executedArtifactVariantForTest(t, "0"))
			plan, replay, observed := fixture.plan, fixture.replay, fixture.observed
			sets := maps.Clone(fixture.variant.Snapshot.ConfigDependencies)
			switch failure {
			case "nil replay":
				replay = nil
			case "nil observation":
				observed = nil
			case "other cut":
				empty, err := NewActionPlanFamilyExecutionCut(fixture.family, nil)
				if err != nil {
					t.Fatal(err)
				}
				observed, err = observeExecutedArtifacts(empty, nil, 1024, 8192)
				if err != nil {
					t.Fatal(err)
				}
			case "other plan pointer":
				plan = cloneActionPlan(plan)
			case "changed recipe":
				node := reviewArtifactConsumer(t, plan)
				recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
				recipe.Environment = map[string]string{"CHANGED": "1"}
				plan.Recipes[node.Recipe] = recipe
			case "changed source":
				plan.Sources[0].Path += ".changed"
			case "changed edge":
				reviewArtifactConsumer(t, plan).Inputs[0].Slot = 1
			case "changed toolset":
				plan.Toolsets["host"] = "sha256-" + strings.Repeat("9", 64)
			case "missing consumer annotation":
				delete(sets, reviewArtifactConsumer(t, plan).ID)
			case "missing producer annotation":
				delete(sets, fixture.producerID)
			}
			updated, classifications, err := substituteExecutedArtifacts(plan, sets, replay, observed)
			if err == nil || updated != nil || classifications != nil {
				t.Fatalf("changed authority accepted: plan=%v sets=%v err=%v", updated != nil, classifications != nil, err)
			}
		})
	}
}

func TestReviewExecutedArtifactsRetainUnsupportedBindings(t *testing.T) {
	for _, testcase := range []struct {
		name     string
		change   func(*ActionPlanNode, *ActionRecipe, *ActionPlanNode)
		retained [2]bool
	}{
		{"incomplete compound", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.CompilerInvocation.WorkingInputUsesComplete = false
			recipe.CompilerInvocation.WorkingInputUses = nil
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses = nil
		}, [2]bool{true, true}},
		{"untyped compound", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.CompilerInvocation = nil
		}, [2]bool{true, true}},
		{"argument transform", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.ArgumentTransforms = []ActionRecipeArgumentTransform{{Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64}}
		}, [2]bool{true, true}},
		{"no fixed executable staging", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			delete(recipe.WorkingInputs, "input:helper:00000000")
			recipe.CompilerInvocation.WorkingInputUses = slices.DeleteFunc(recipe.CompilerInvocation.WorkingInputUses, func(value string) bool { return value == "input:helper:00000000" })
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses = nil
		}, [2]bool{true, false}},
		{"different executable staging", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.WorkingInputs["input:helper:00000000"] = "tools/alias"
		}, [2]bool{true, false}},
		{"not executable staging", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.ExecutableInputs = nil
		}, [2]bool{true, false}},
		{"executable overwritten by consumer", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.WorkingOutputs["00000000"] = "tools/helper"
		}, [2]bool{true, false}},
		{"sidecar argument alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.Arguments = append(recipe.Arguments, "${input:observed-state:00000001}")
		}, [2]bool{false, true}},
		{"sidecar environment alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.Environment = map[string]string{"STATE": "${input:observed-state:00000001}"}
		}, [2]bool{false, true}},
		{"sidecar working alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.WorkingInputs["input:observed-state:00000001"] = "state/alias"
		}, [2]bool{false, true}},
		{"sidecar compiler alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.CompilerInvocation.Arguments = append(recipe.CompilerInvocation.Arguments, "-DSTATE=${input:observed-state:00000001}")
		}, [2]bool{false, true}},
		{"sidecar content alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{"state": {Input: "input:observed-state:00000001", Transform: ActionRecipeContentTransformMakeShellValue}}
			recipe.Environment = map[string]string{"STATE": "${content:state}"}
		}, [2]bool{false, true}},
		{"sidecar replay alias", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.CommandReplays = []ActionRecipeCommandReplay{{Name: "state", Invocations: []ActionRecipeCommandReplayInvocation{{Arguments: []string{"${input:observed-state:00000001}"}}}}}
		}, [2]bool{false, true}},
		{"mixed unsafe direct aliases", func(_ *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
			recipe.WorkingInputs["input:helper:00000000"] = "tools/alias"
			recipe.Environment = map[string]string{"STATE": "${input:observed-state:00000001}"}
		}, [2]bool{true, true}},
		{"public output tree", func(_ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
			producer.Stage = "target"
			for index := range producer.Outputs {
				producer.Outputs[index].Tree = "sdk"
			}
		}, [2]bool{true, true}},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			variant := reviewArtifactVariant(t, testcase.change)
			fixture := reviewArtifactSetup(t, variant)
			updated, sets, err := substituteExecutedArtifacts(fixture.plan, variant.Snapshot.ConfigDependencies, fixture.replay, fixture.observed)
			if err != nil {
				t.Fatal(err)
			}
			family := reviewArtifactAssertEdges(t, fixture, updated, sets, testcase.retained)
			if testcase.name == "public output tree" {
				visible := false
				for _, view := range family.Views {
					visible = visible || view.Tree == "sdk" && view.NodeID == fixture.family.originalNodeIDs[variant.Name][fixture.producerID] && view.Slot == 0
				}
				if !visible {
					t.Fatal("public-tree negative did not retain its original public view")
				}
			}
			if testcase.retained == [2]bool{true, true} {
				for _, source := range updated.Sources {
					if source.Namespace == "observed-artifacts" {
						t.Fatal("fully unsupported consumer acquired an observed source")
					}
				}
			}
		})
	}
}

func TestReviewExecutedArtifactsSidecarPathMismatchFailsClosed(t *testing.T) {
	variant := reviewArtifactVariant(t, func(consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		recipe.ObservedOutputs["00000001"] = "different-side-effect"
		consumer.Outputs[1].ObservedPath = "different-side-effect"
	})
	// Make the consumer output agree with its recipe; only its declared base
	// belongs to a different logical path.
	fixture := reviewArtifactSetup(t, variant)
	if _, _, err := substituteExecutedArtifacts(fixture.plan, variant.Snapshot.ConfigDependencies, fixture.replay, fixture.observed); err == nil {
		t.Fatal("sidecar for another logical path was substituted")
	}
}
