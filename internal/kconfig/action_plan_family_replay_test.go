package kconfig

import (
	"crypto/sha256"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFamilyReplayNormalizesOnlyUnreferencedDiscoveredSources(t *testing.T) {
	for _, mode := range []string{"discovered", "unlisted", "opaque", "wrong-namespace", "no-metadata", "direct-reference", "input-set-reference"} {
		t.Run(mode, func(t *testing.T) {
			_, initial, current := familyReplayTest(t)
			current.metadata = &CompactMetadata{}
			added := ActionPlanSource{ID: "src-00000003", Namespace: "kernel", Path: "include/discovered.h"}
			initial.Sources = append(initial.Sources, added)
			initial.ConfigDependencies[initial.Nodes[0].ID] = ConfigDependencySet{SourcePaths: []string{added.Path}}
			switch mode {
			case "unlisted":
				initial.ConfigDependencies[initial.Nodes[0].ID] = ConfigDependencySet{}
			case "opaque":
				initial.ConfigDependencies[initial.Nodes[0].ID] = ConfigDependencySet{Opaque: true, Reason: "not proven"}
			case "wrong-namespace":
				initial.Sources[len(initial.Sources)-1].Namespace = "foreign"
			case "no-metadata":
				current.metadata = nil
			case "direct-reference":
				initial.Nodes[0].Sources[0].SourceID = added.ID
				initial.Nodes[0].ID = initial.Nodes[0].ContentID()
				initial.ConfigDependencies = map[string]ConfigDependencySet{initial.Nodes[0].ID: {SourcePaths: []string{added.Path}}}
			case "input-set-reference":
				plan := snapshotActionPlan(initial)
				plan.Nodes[0].InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
					Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: added.Path}, SourceID: added.ID,
				})
				if err := contentAddressActionPlanNodes(plan); err != nil {
					t.Fatal(err)
				}
				var err error
				initial, err = canonicalActionPlanSnapshot(plan,
					map[string]ConfigDependencySet{plan.Nodes[0].ID: {SourcePaths: []string{added.Path}}}, initial.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
			}
			want, err := canonicalFamilyReplayInitialPlan(initial, current)
			addressed := cloneActionPlan(current)
			if addressErr := contentAddressActionPlanNodes(addressed); addressErr != nil {
				t.Fatal(addressErr)
			}
			got, currentErr := canonicalFamilyReplayPlan(addressed, initial.ConfigFiles)
			if currentErr != nil {
				t.Fatal(currentErr)
			}
			matches := err == nil && slices.Equal(got, want)
			if (mode == "direct-reference" || mode == "input-set-reference") && (err == nil || !strings.Contains(err.Error(), "unknown source")) {
				t.Fatalf("normalization did not validate the remaining source references: %v", err)
			}
			if matches != (mode == "discovered") {
				t.Fatalf("normalization mode %s matched=%v, err=%v", mode, matches, err)
			}
			if len(current.Sources) != 2 || len(initial.Sources) != 3 {
				t.Fatal("normalization mutated the caller's source catalog")
			}
		})
	}
}

func TestFamilyReplayAnalysisSealRequiresUnchangedOriginalCatalog(t *testing.T) {
	for _, mode := range []string{"append", "replace", "truncate", "reorder", "recipe", "config", "foreign-plan"} {
		t.Run(mode, func(t *testing.T) {
			cut, initial, current := familyReplayTest(t)
			replay, err := cut.VerifyVariantReplay("base", initial, current, initial.ConfigFiles)
			if err != nil {
				t.Fatal(err)
			}
			current.Sources = append(current.Sources, ActionPlanSource{ID: "src-00000003", Namespace: "kernel", Path: "include/discovered.h"})
			files := maps.Clone(initial.ConfigFiles)
			switch mode {
			case "replace":
				current.Sources[0].Path = "changed.c"
			case "truncate":
				current.Sources = current.Sources[:1]
			case "reorder":
				current.Sources[0], current.Sources[2] = current.Sources[2], current.Sources[0]
			case "recipe":
				recipe := current.Recipes[current.Nodes[0].Recipe]
				recipe.Arguments = append(slices.Clone(recipe.Arguments), "-DCHANGED=1")
				current.Recipes[current.Nodes[0].Recipe] = recipe
			case "config":
				files["include/config/kernel.release"] = "changed\n"
			case "foreign-plan":
				current = cloneActionPlan(current)
			}
			if err := contentAddressActionPlanNodes(current); err != nil {
				t.Fatal(err)
			}
			digest, err := replay.sealAnalysis(current, files)
			if mode != "append" {
				if err == nil {
					t.Fatalf("sealed changed %s", mode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			complete, err := canonicalFamilyReplayPlan(current, files)
			if err != nil || digest != sha256.Sum256(complete) || digest == replay.contractDigest {
				t.Fatalf("append did not seal the complete expanded catalog: %v", err)
			}
		})
	}
}

func familyReplayTest(t *testing.T) (*ActionPlanFamilyExecutionCut, ActionPlanSnapshot, *ActionPlan) {
	t.Helper()
	initial := familyTestSnapshot(t, 1, "", familyTestConfig("1", "0"), ConfigDependencySet{Opaque: true, Reason: "initial conservative classification"})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: initial}})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: family.Nodes[0].ID, Slot: 0}})
	if err != nil {
		t.Fatal(err)
	}
	current := snapshotActionPlan(initial)
	current.Nodes[0].ID = "provisional-node"
	return cut, initial, current
}

func TestFamilyExecutionCutVerifiesOriginalReplayBeforeAnalysis(t *testing.T) {
	cut, initial, current := familyReplayTest(t)
	before := cloneActionPlanNodes(current.Nodes)
	replay, err := cut.VerifyVariantReplay("base", initial, current, initial.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	if replay.plan != current || replay.cutID != cut.ID() || replay.variant != "base" ||
		replay.originalIDs["provisional-node"] != initial.Nodes[0].ID || !replay.opaqueNodes["provisional-node"] ||
		replay.executedIDs["provisional-node"] != cut.Roots()[0].NodeID {
		t.Fatalf("replay lost provisional/original binding or opacity: %#v", replay)
	}
	if !reflect.DeepEqual(before, current.Nodes) {
		t.Fatal("verification rewrote the live selection graph's node identities")
	}
	if err := replay.validateCurrent(current); err != nil {
		t.Fatal(err)
	}
	// Old annotations are not execution authority. Only the full lowering,
	// source/tool/input/projection contract participates in replay matching.
	initial.ConfigDependencies[initial.Nodes[0].ID] = ConfigDependencySet{Symbols: []string{"CONFIG_USED"}}
	if _, err := cut.VerifyVariantReplay("base", initial, current, initial.ConfigFiles); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyExecutionCutReplayRejectsPostGateMutation(t *testing.T) {
	for _, change := range []string{"recipe-value", "source-path", "toolset", "output", "rekey", "foreign-plan"} {
		t.Run(change, func(t *testing.T) {
			cut, initial, current := familyReplayTest(t)
			replay, err := cut.VerifyVariantReplay("base", initial, current, initial.ConfigFiles)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "recipe-value":
				recipe := cloneActionRecipe(current.Recipes[current.Nodes[0].Recipe])
				recipe.Arguments = append(recipe.Arguments, "-DCHANGED=1")
				current.Recipes[current.Nodes[0].Recipe] = recipe
			case "source-path":
				current.Sources[0].Path = "changed/source.c"
			case "toolset":
				current.Toolsets["target"] = "sha256-" + strings.Repeat("9", 64)
			case "output":
				current.Nodes[0].Outputs[0].Path = "changed.o"
			case "rekey":
				current.Nodes[0].ID = initial.Nodes[0].ID
			case "foreign-plan":
				current = cloneActionPlan(current)
			}
			if err := replay.validateCurrent(current); err == nil {
				t.Fatalf("accepted %s after first replay gate", change)
			}
		})
	}
}

func TestFamilyExecutionCutRejectsChangedReplayBeforeAnalysis(t *testing.T) {
	for _, change := range []string{"recipe", "source", "toolset", "output", "config", "variant", "nil-plan"} {
		t.Run(change, func(t *testing.T) {
			cut, initial, current := familyReplayTest(t)
			files := maps.Clone(initial.ConfigFiles)
			variant := "base"
			switch change {
			case "recipe":
				recipe := cloneActionRecipe(current.Recipes[current.Nodes[0].Recipe])
				recipe.Arguments = append(recipe.Arguments, "-DCHANGED=1")
				id, err := recipe.ID()
				if err != nil {
					t.Fatal(err)
				}
				current.Recipes = map[string]ActionRecipe{id: recipe}
				current.Nodes[0].Recipe = id
			case "source":
				current.Sources[0].Path = "changed/source.c"
			case "toolset":
				current.Toolsets["target"] = "sha256-" + strings.Repeat("9", 64)
			case "output":
				current.Nodes[0].Outputs[0].Path = "different.o"
			case "config":
				files["include/config/kernel.release"] += "changed\n"
			case "variant":
				variant = "unknown"
			case "nil-plan":
				current = nil
			}
			if replay, err := cut.VerifyVariantReplay(variant, initial, current, files); err == nil || replay != nil {
				t.Fatalf("accepted changed original %s: %#v / %v", change, replay, err)
			}
		})
	}
}

func TestFamilyExecutionCutReplayRequiresOriginalMembership(t *testing.T) {
	cut, initial, current := familyReplayTest(t)
	// Cut internals cannot be edited by external callers. Simulate corruption
	// to ensure the final original-ID lookup is mandatory, not an optional hint.
	cut.contract.Origins[0].OriginalNodeID = strings.Repeat("0", 64)
	if replay, err := cut.VerifyVariantReplay("base", initial, current, initial.ConfigFiles); err == nil || replay != nil {
		t.Fatalf("accepted missing original membership: %#v / %v", replay, err)
	}
}
