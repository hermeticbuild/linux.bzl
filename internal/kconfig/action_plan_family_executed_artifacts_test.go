package kconfig

import (
	"bytes"
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func executedArtifactStoresForTest(t *testing.T, cut *ActionPlanFamilyExecutionCut, disposition toolaction.ObservedOutputDisposition) map[string]string {
	t.Helper()
	stores := observedHeadersTestStores(t, cut, []byte("test executable bytes\n"))
	for _, output := range cut.Outputs() {
		filename := observedHeadersTestOutputPath(stores, output)
		if output.Output.ObservedPath == "" {
			if err := os.Chmod(filename, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		state := toolaction.ObservedOutputState{Disposition: disposition}
		if disposition != toolaction.ObservedOutputAbsent {
			state.Writer = output.NodeID
		}
		if disposition == toolaction.ObservedOutputPresent {
			state.Content = []byte("same payload, different writer\n")
		}
		content, err := toolaction.EncodeObservedOutputState(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stores
}

func executedArtifactVariantForTest(t *testing.T, other string) ActionPlanFamilyVariant {
	t.Helper()
	files := familyTestConfig("1", other)
	snapshot := familyTestSnapshot(t, 1, "", files, ConfigDependencySet{Symbols: []string{"CONFIG_USED"}, SourcePaths: []string{"drivers/example.c"}})
	plan := snapshotActionPlan(snapshot)
	plan.Toolsets["host"] = "sha256-" + strings.Repeat("2", 64)
	consumer := &plan.Nodes[0]
	consumer.ID = "consumer"
	recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
	compilerArgs := slices.Clone(recipe.Arguments)
	recipe.Tool = "scriptrun"
	recipe.Arguments = []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte("tools/helper\ncc -c drivers/example.c -o drivers/example.o\n"))}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{Tool: "cc", Arguments: compilerArgs, WorkingInputUsesComplete: true, WorkingInputUses: []string{"input:helper:00000000", "source:config:00000001", "source:source:00000000"}, AuxiliaryWorkingInputUses: []string{"input:helper:00000000"}}
	recipe.Inputs = []string{"helper:00000000", "observed-state:00000001"}
	recipe.ExecutableInputs = []string{"helper:00000000"}
	recipe.WorkingInputs["source:source:00000000"] = "drivers/example.c"
	recipe.WorkingInputs["input:helper:00000000"] = "tools/helper"
	recipe.WorkingOutputs = map[string]string{"00000000": "drivers/example.o"}
	recipe.Outputs = append(recipe.Outputs, "00000001")
	recipe.ObservedOutputs = map[string]string{"00000001": "side-effect"}
	recipe.ObservedOutputBases = map[string][]string{"00000001": {"observed-state:00000001"}}
	id, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes = map[string]ActionRecipe{id: recipe}
	consumer.Tool, consumer.Recipe = "scriptrun", id
	consumer.Inputs = []ActionPlanNodeEdge{{Role: "helper", ProducerID: "helper", Slot: 0}, {Role: "observed-state", ProducerID: "helper", Slot: 1}}
	consumer.Outputs = append(consumer.Outputs, ActionPlanOutput{Tree: "metadata", Path: "consumer.state", ObservedPath: "side-effect"})
	helperRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile", Arguments: []string{"-content_base64", base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nexit 0\n")), "-out", "${output:00000000}"}, WorkingDirectory: "helper", WorkingInputs: map[string]string{"source:config:00000000": "include/generated/autoconf.h"}, Sources: []string{"config:00000000"}, Outputs: []string{"00000000", "00000001"}, ObservedOutputs: map[string]string{"00000001": "side-effect"}}
	helperID, err := helperRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[helperID] = helperRecipe
	plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: "helper", Stage: "prehost", Kind: "generate", Tool: "actionfile", Recipe: helperID, Product: "sdk", Sources: []ActionPlanSourceEdge{{Role: "config", SourceID: "src-00000002"}}, Outputs: []ActionPlanOutput{{Tree: "prehost", Path: "tools/helper"}, {Tree: "prehost", Path: "helper.state", ObservedPath: "side-effect"}}})
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sets := map[string]ConfigDependencySet{plan.Nodes[0].ID: {Symbols: []string{"CONFIG_USED"}, SourcePaths: []string{"drivers/example.c"}, ObjectPaths: []string{"include/generated/autoconf.h"}}, plan.Nodes[1].ID: {Opaque: true, Reason: "config-dependent helper"}}
	snapshot, err = canonicalActionPlanSnapshot(plan, sets, files)
	if err != nil {
		t.Fatal(err)
	}
	name := "base"
	if other != "0" {
		name = "other"
	}
	return ActionPlanFamilyVariant{Name: name, Snapshot: snapshot}
}

func TestFamilyExecutedArtifactGraphReuse(t *testing.T) {
	for _, testcase := range []struct {
		name   string
		state  toolaction.ObservedOutputDisposition
		change string
	}{
		{"equal executable and absent state", toolaction.ObservedOutputAbsent, ""},
		{"different present writers", toolaction.ObservedOutputPresent, ""},
		{"different deleted writers", toolaction.ObservedOutputDeleted, ""},
		{"different executable bytes", toolaction.ObservedOutputAbsent, "bytes"},
		{"different executable mode", toolaction.ObservedOutputAbsent, "mode"},
		{"relevant configuration", toolaction.ObservedOutputAbsent, "config"},
		{"opaque consumer", toolaction.ObservedOutputAbsent, "opaque"},
		{"persistent work target", toolaction.ObservedOutputAbsent, "work"},
		{"persistent tree target", toolaction.ObservedOutputAbsent, "tree"},
		{"persistent additional alias", toolaction.ObservedOutputAbsent, "alias"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			state := testcase.state
			variants := []ActionPlanFamilyVariant{executedArtifactVariantForTest(t, "0"), executedArtifactVariantForTest(t, "1")}
			if testcase.change == "work" || testcase.change == "tree" || testcase.change == "alias" {
				for index := range variants {
					variants[index] = executedArtifactPersistentVariantForTest(t, variants[index], testcase.change)
				}
			}
			if testcase.change == "config" {
				variants[1].Snapshot.ConfigFiles = familyTestConfig("2", "1")
			}
			if testcase.change == "opaque" {
				for index := range variants {
					for _, node := range variants[index].Snapshot.Nodes {
						if node.Kind == "compile" {
							variants[index].Snapshot.ConfigDependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "unknown compiler input"}
						}
					}
				}
			}
			baseline, err := BuildActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			baselineCompilers := 0
			for _, node := range baseline.Nodes {
				if node.Kind == "compile" {
					baselineCompilers++
				}
			}
			if baselineCompilers != 2 {
				t.Fatalf("fixture baseline has %d compilers, want2", baselineCompilers)
			}
			initial, err := BuildConservativeActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			var roots []ActionPlanFamilyExecutionCutRoot
			for _, variant := range variants {
				for _, node := range variant.Snapshot.Nodes {
					if node.Stage == "prehost" {
						roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: initial.originalNodeIDs[variant.Name][node.ID], Slot: 0})
					}
				}
			}
			cut, err := NewActionPlanFamilyExecutionCut(initial, roots)
			if err != nil {
				t.Fatal(err)
			}
			stores := executedArtifactStoresForTest(t, cut, state)
			if testcase.change == "bytes" || testcase.change == "mode" {
				for _, output := range cut.Outputs() {
					if output.Output.ObservedPath != "" {
						continue
					}
					filename := observedHeadersTestOutputPath(stores, output)
					if testcase.change == "bytes" {
						if err := os.WriteFile(filename, []byte("different executable\n"), 0o755); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.Chmod(filename, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					break
				}
			}
			observed, err := observeExecutedArtifacts(cut, stores, 1024, 8192)
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
				updated, sets, err := substituteExecutedArtifacts(plan, variant.Snapshot.ConfigDependencies, replay, observed)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := canonicalActionPlanSnapshot(updated, sets, variant.Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, actionPlanFamilyBuildVariant{variant: ActionPlanFamilyVariant{Name: variant.Name, Snapshot: snapshot}, executionCut: cut})
			}
			family, err := buildValidatedActionPlanFamily(inputs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cut.Verify(family.family); err != nil {
				t.Fatalf("changed pinned execution: %v", err)
			}
			compilers := 0
			for _, node := range family.family.Nodes {
				if node.Kind == "compile" {
					compilers++
				}
			}
			want := 2
			if state == toolaction.ObservedOutputAbsent && (testcase.change == "" || testcase.change == "work" || testcase.change == "tree") {
				want = 1
			}
			if compilers != want {
				t.Fatalf("compiler instances=%d, want%d", compilers, want)
			}
			for _, view := range family.family.Views {
				if strings.HasPrefix(view.ArtifactPath, ".linux-bzl-executed-artifacts/") {
					t.Fatal("private content-copy appeared in a public view")
				}
			}
		})
	}
}

func executedArtifactPersistentVariantForTest(t *testing.T, variant ActionPlanFamilyVariant, mode string) ActionPlanFamilyVariant {
	t.Helper()
	plan := snapshotActionPlan(variant.Snapshot)
	producerID := ""
	for _, node := range plan.Nodes {
		if node.Stage == "prehost" {
			producerID = node.ID
		}
	}
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.Kind != "compile" {
			continue
		}
		target := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "tools/helper"}
		if mode == "tree" {
			target.Kind, target.Tree = ActionPlanInputSetTreeTarget, "prehost"
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			recipe.Trees = append(recipe.Trees, "prehost")
			node.Trees = append(node.Trees, "prehost")
			id, err := recipe.ID()
			if err != nil {
				t.Fatal(err)
			}
			plan.Recipes[id], node.Recipe = recipe, id
		}
		node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{Target: target, ProducerID: producerID, Slot: 0, CompilerUse: true, AuxiliaryUse: true})
		if mode == "alias" {
			target.Path = "tools/another-alias"
			node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{Target: target, ProducerID: producerID, Slot: 0, CompilerUse: true, AuxiliaryUse: true})
		}
	}
	before := make([]ConfigDependencySet, len(plan.Nodes))
	for index, node := range plan.Nodes {
		before[index] = variant.Snapshot.ConfigDependencies[node.ID]
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sets := map[string]ConfigDependencySet{}
	for index, node := range plan.Nodes {
		sets[node.ID] = before[index]
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, sets, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	variant.Snapshot = snapshot
	return variant
}

func TestFamilyExecutedArtifactsPreserveStateWriter(t *testing.T) {
	for _, state := range []toolaction.ObservedOutputDisposition{toolaction.ObservedOutputAbsent, toolaction.ObservedOutputPresent, toolaction.ObservedOutputDeleted} {
		t.Run(string(state), func(t *testing.T) {
			cut := observedHeadersTestCut(t, true)
			stores := executedArtifactStoresForTest(t, cut, state)
			observed, err := observeExecutedArtifacts(cut, stores, 1024, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if observed.cutID != cut.ID() || len(observed.outputs) != len(cut.Outputs()) {
				t.Fatal("lost cut ownership")
			}
			ordinary, sidecars := map[string]bool{}, map[string]bool{}
			for _, artifact := range observed.outputs {
				original, err := os.ReadFile(observedHeadersTestOutputPath(stores, artifact.output))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(original, observed.contents[artifact.contentID]) {
					t.Fatal("rewrote executed output bytes")
				}
				if artifact.output.Output.ObservedPath == "" {
					ordinary[artifact.contentID] = true
				} else {
					sidecars[artifact.contentID] = true
				}
			}
			wantStates := 2
			if state == toolaction.ObservedOutputAbsent {
				wantStates = 1
			}
			if len(ordinary) != 1 || len(sidecars) != wantStates {
				t.Fatalf("ordinary=%d sidecars=%d; want 1/%d", len(ordinary), len(sidecars), wantStates)
			}
		})
	}
}

func TestFamilyExecutedArtifactsRejectIncompleteOrAmbiguousStores(t *testing.T) {
	for _, failure := range []string{"missing", "symlink", "directory", "malformed-state", "unknown-writer", "file-budget", "total-budget"} {
		t.Run(failure, func(t *testing.T) {
			cut := observedHeadersTestCut(t, true)
			stores := executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputAbsent)
			var ordinary, sidecar ActionPlanFamilyExecutionCutOutput
			for _, output := range cut.Outputs() {
				if output.Output.ObservedPath == "" {
					ordinary = output
				} else {
					sidecar = output
				}
			}
			filename := observedHeadersTestOutputPath(stores, ordinary)
			perFile, total := 1024, 8192
			switch failure {
			case "missing":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(observedHeadersTestOutputPath(stores, sidecar), filename); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filename, 0o755); err != nil {
					t.Fatal(err)
				}
			case "malformed-state":
				if err := os.WriteFile(observedHeadersTestOutputPath(stores, sidecar), []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "unknown-writer":
				content, err := toolaction.EncodeObservedOutputState(toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputDeleted, Writer: strings.Repeat("f", 64)})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(observedHeadersTestOutputPath(stores, sidecar), content, 0o644); err != nil {
					t.Fatal(err)
				}
			case "file-budget":
				perFile = 1
			case "total-budget":
				total = 1
			}
			got, err := observeExecutedArtifacts(cut, stores, perFile, total)
			if err == nil || got != nil {
				t.Fatalf("accepted %s: %#v, %v", failure, got, err)
			}
		})
	}
}

func TestFamilyExecutedArtifactsDistinguishBytesAndMode(t *testing.T) {
	for _, change := range []string{"bytes", "mode"} {
		t.Run(change, func(t *testing.T) {
			cut := observedHeadersTestCut(t, true)
			stores := executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputAbsent)
			for _, output := range cut.Outputs() {
				if output.Output.ObservedPath != "" {
					continue
				}
				filename := observedHeadersTestOutputPath(stores, output)
				if change == "bytes" {
					if err := os.WriteFile(filename, []byte("different\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Chmod(filename, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				break
			}
			observed, err := observeExecutedArtifacts(cut, stores, 1024, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if len(observed.contents) != 3 {
				t.Fatalf("lost %s distinction", change)
			}
		})
	}
}
