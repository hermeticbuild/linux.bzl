package kconfig

import (
	"encoding/base64"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestFamilySealedArtifactScannerConsumer(t *testing.T) {
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "int value = CONFIG_USED;\n")
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.ID != consumerID {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		binding := recipe.Inputs[0]
		recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
			Tool: recipe.Tool, Arguments: slices.Clone(recipe.Arguments),
			WorkingInputUsesComplete:  true,
			WorkingInputUses:          []string{"input:" + binding, "source:source:00000000"},
			AuxiliaryWorkingInputUses: []string{"input:" + binding},
		}
		recipe.Tool = "scriptrun"
		recipe.Arguments = []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte("include/generated/selected.h\ncc -nostdinc -c drivers/example/driver.c -o driver.o\n"))}
		recipe.ExecutableInputs = []string{binding}
		recipe.WorkingInputs["input:"+binding] = "include/generated/selected.h"
		recipe.WorkingInputs["source:source:00000000"] = "drivers/example/driver.c"
		recipe.WorkingOutputs = map[string]string{"00000000": node.Outputs[0].Path}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id] = recipe
		node.Recipe, node.Tool = id, recipe.Tool
	}
	plan.invalidateLookupIndexes()
	// Use the normal conservative gate; the scanner still reads the original
	// source and exact compiler invocation. The executable is never an include.
	replay, headers := observedDependencyGateForTest(t, plan, producerID, "#!/bin/sh\nexit 0\n")
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, headers)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(plan)
	if err != nil {
		t.Fatal(err)
	}
	if sets[consumerID].Opaque {
		t.Fatalf("real scanner consumer remains opaque: %s", sets[consumerID].Reason)
	}
	uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
	if err != nil || len(uses) != 0 {
		t.Fatalf("executable became a header receipt: %v %v", uses, err)
	}
	// Reconstruct the exact cut from the original snapshot as the CLI does.
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		t.Fatal(err)
	}
	originalRoot := ""
	for index, node := range plan.Nodes {
		if node.ID == producerID {
			originalRoot = addressed.Nodes[index].ID
		}
	}
	addressedSets, err := analysis.ByNodeID(addressed)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := canonicalActionPlanSnapshot(addressed, addressedSets, replay.configFiles)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: family.originalNodeIDs["base"][originalRoot], Slot: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if cut.ID() != replay.cutID {
		t.Fatal("fixture reconstruction changed original cut")
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	digest, err := replay.sealAnalysis(plan, replay.configFiles)
	if err != nil {
		t.Fatal(err)
	}
	result := &ActionPlanFamilyVariantPlanningResult{Plan: plan, analysis: analysis, observed: headers,
		cut: cut, variant: "base", replayConfigFiles: maps.Clone(replay.configFiles), replayContractDigest: digest}
	observed, err := observeExecutedArtifacts(cut,
		executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputAbsent), 1024, 16384)
	if err != nil {
		t.Fatal(err)
	}
	updated, updatedSets, err := substitutePlannedExecutedArtifacts(result, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Nodes) != len(plan.Nodes)+1 {
		t.Fatalf("real scanner consumer did not acquire content-copy input: %d => %d", len(plan.Nodes), len(updated.Nodes))
	}
	finalSnapshot, err := canonicalActionPlanSnapshot(updated, updatedSets, result.replayConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	final, err := buildValidatedActionPlanFamily([]actionPlanFamilyBuildVariant{{variant: ActionPlanFamilyVariant{Name: "base", Snapshot: finalSnapshot}, executionCut: cut}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cut.Verify(final.family); err != nil {
		t.Fatal(err)
	}
}

func TestFamilySealedArtifactAdapter(t *testing.T) {
	for _, change := range []string{"", "public diagnostics", "missing analysis", "missing seal", "config", "recipe", "source", "toolset", "variant", "cut"} {
		t.Run(change, func(t *testing.T) {
			// This helper exercises the actual scanner and original replay seal.
			// It has no supported executable edge, so this test verifies the
			// authoritative integration gate, not a reuse gain.
			result := observedCASSealedResultForTest(t, "prep")
			observed, err := observeExecutedArtifacts(result.cut,
				executedArtifactStoresForTest(t, result.cut, toolaction.ObservedOutputAbsent), 1024, 16384)
			if err != nil {
				t.Fatal(err)
			}
			want, err := result.analysis.ByNodeID(result.Plan)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "public diagnostics":
				result.Dependencies = map[string]ConfigDependencySet{}
				result.GeneratedHeaderDemands = ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true, Truncated: true}
				result.ObservedHeaderUses = []ConfigDependencyObservedHeaderUse{{ContentID: "caller-forged"}}
			case "missing analysis":
				result.analysis = nil
			case "missing seal":
				result.replayContractDigest = [32]byte{}
			case "config":
				result.replayConfigFiles = maps.Clone(result.replayConfigFiles)
				result.replayConfigFiles[".config"] += "CONFIG_FORGED=y\n"
			case "recipe":
				for key, recipe := range result.Plan.Recipes {
					recipe.Arguments = append(recipe.Arguments, "caller-forged")
					result.Plan.Recipes[key] = recipe
					break
				}
			case "source":
				result.Plan.Sources[0].Path = "forged.c"
			case "toolset":
				result.Plan.Toolsets["target"] = result.Plan.Toolsets["host"]
			case "variant":
				result.variant = "unknown"
			case "cut":
				observed.cutID = "different-execution"
			}
			plan, sets, err := substitutePlannedExecutedArtifacts(result, observed)
			if change != "" && change != "public diagnostics" {
				if err == nil || plan != nil || sets != nil {
					t.Fatalf("accepted %s mutation", change)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sets, want) {
				t.Fatal("public diagnostic fields influenced private analysis")
			}
			snapshot, err := canonicalActionPlanSnapshot(plan, sets, result.replayConfigFiles)
			if err != nil {
				t.Fatal(err)
			}
			family, err := buildValidatedActionPlanFamily([]actionPlanFamilyBuildVariant{{
				variant: ActionPlanFamilyVariant{Name: result.variant, Snapshot: snapshot}, executionCut: result.cut,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := result.cut.Verify(family.family); err != nil {
				t.Fatal(err)
			}
		})
	}
}
