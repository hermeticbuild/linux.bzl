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

func executedArtifactSequenceVariantForTest(t *testing.T, variant ActionPlanFamilyVariant, change string) ActionPlanFamilyVariant {
	t.Helper()
	plan := snapshotActionPlan(variant.Snapshot)
	actionfile := strings.HasPrefix(change, "sequence-actionfile")
	if change == "sequence-direct" || actionfile {
		// Keep the cut's helper root when the consumer no longer uses it.
		plan.Products = append(plan.Products, ActionPlanProduct{Name: "sdk", Tree: "prehost", Path: "tools/helper"})
	}
	checkPlan := snapshotActionPlan(familyTestSourceCheckSnapshot(t))
	check, found := compactKbuildPlanNode(checkPlan, checkPlan.executionCheckRoots[0])
	if !found {
		t.Fatal("missing source check")
	}
	check.ID, check.Stage, check.Outputs[0].Tree = "check", "prehost", "prehost"
	for _, source := range checkPlan.Sources {
		if source.ID == check.Sources[0].SourceID {
			plan.Sources = append(plan.Sources, source)
			break
		}
	}
	check.Sources = append(check.Sources, ActionPlanSourceEdge{Role: "config", SourceID: "src-00000002"})
	checkRecipe := cloneActionRecipe(checkPlan.Recipes[check.Recipe])
	checkRecipe.Sources = append(checkRecipe.Sources, "config:00000001")
	checkRecipe.WorkingInputs["source:config:00000001"] = "include/generated/autoconf.h"
	id, err := checkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[id], check.Recipe = checkRecipe, id
	sets := make([]ConfigDependencySet, len(plan.Nodes))
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		sets[index] = variant.Snapshot.ConfigDependencies[node.ID]
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		if node.Kind == "compile" && change == "sequence-direct" {
			base := familyTestSnapshot(t, 1, "", variant.Snapshot.ConfigFiles, sets[index])
			recipe = base.Recipes[base.Nodes[0].Recipe]
			node.Tool, node.Inputs, node.Outputs = recipe.Tool, nil, base.Nodes[0].Outputs
		}
		if node.Kind == "compile" && actionfile {
			recipe = ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
				Arguments:     []string{"-input", "${source:source:00000000}", "-out", "${output:00000000}"},
				ArgumentsFile: true,
				Sources:       []string{"source:00000000"}, Outputs: []string{"00000000"},
			}
			switch change {
			case "sequence-actionfile-at":
				recipe.Arguments, recipe.ArgumentsFile = []string{"@${source:source:00000000}"}, false
			case "sequence-actionfile-json":
				recipe.Arguments, recipe.ArgumentsFile = []string{"-arguments_file", "${source:source:00000000}"}, false
			case "sequence-actionfile-content":
				recipe.Arguments, recipe.ArgumentsFile = []string{"${content:args}"}, false
				recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
					"args": {Input: "source:source:00000000", Transform: ActionRecipeContentTransformMakeShellWord},
				}
			}
			if !recipe.ArgumentsFile {
				recipe.WorkingDirectory = "copy"
				recipe.WorkingOutputs = map[string]string{"00000000": "drivers/example.o"}
			}
			node.Kind, node.Tool, node.Inputs = recipe.Kind, recipe.Tool, nil
			node.Sources = slices.DeleteFunc(node.Sources, func(source ActionPlanSourceEdge) bool { return source.Role != "source" })
			node.Outputs = node.Outputs[:1]
			sets[index] = ConfigDependencySet{SourcePaths: []string{"drivers/example.c"}}
		}
		remap := map[string]string{}
		for ordinal, input := range node.Inputs {
			remap["input:"+recipe.Inputs[ordinal]] = input.Role + ":" + planOrdinal(ordinal+1)
		}
		working := map[string]string{}
		for reference, pathname := range recipe.WorkingInputs {
			if binding := remap[reference]; binding != "" {
				reference = "input:" + binding
			}
			working[reference] = pathname
		}
		recipe = rewriteFamilyRecipeBindings(recipe, remap, working)
		node.Inputs = append([]ActionPlanNodeEdge{{Role: "sequence", ProducerID: check.ID, Slot: 0}}, node.Inputs...)
		recipe.Inputs = make([]string, len(node.Inputs))
		for ordinal, input := range node.Inputs {
			recipe.Inputs[ordinal] = input.Role + ":" + planOrdinal(ordinal)
		}
		if node.Kind == "compile" {
			switch change {
			case "sequence-data":
				recipe.Environment = map[string]string{"STATUS": "${input:sequence:00000000}"}
			case "sequence-input-set":
				node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
					Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "status-alias"},
					ProducerID: check.ID, Slot: 0, CompilerUse: true, AuxiliaryUse: true,
				})
			}
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id], node.Recipe = recipe, id
	}
	plan.Nodes = append(plan.Nodes, check)
	plan.executionCheckRoots = []string{check.ID}
	sets = append(sets, ConfigDependencySet{Opaque: true, Reason: "selected source check"})
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for index, node := range plan.Nodes {
		dependencies[node.ID] = sets[index]
	}
	variant.Snapshot, err = canonicalActionPlanSnapshot(plan, dependencies, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	return variant
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
		{"completed ordering", toolaction.ObservedOutputAbsent, "sequence"},
		{"direct compiler ordering", toolaction.ObservedOutputAbsent, "sequence-direct"},
		{"actionfile ordering with canonical arguments transport", toolaction.ObservedOutputAbsent, "sequence-actionfile"},
		{"actionfile opaque multiline arguments", toolaction.ObservedOutputAbsent, "sequence-actionfile-at"},
		{"actionfile opaque JSON arguments", toolaction.ObservedOutputAbsent, "sequence-actionfile-json"},
		{"actionfile arguments from file contents", toolaction.ObservedOutputAbsent, "sequence-actionfile-content"},
		{"unexecuted ordering", toolaction.ObservedOutputAbsent, "sequence-unexecuted"},
		{"ordering data use", toolaction.ObservedOutputAbsent, "sequence-data"},
		{"ordering materialization alias", toolaction.ObservedOutputAbsent, "sequence-input-set"},
		{"invalid completion", toolaction.ObservedOutputPresent, "sequence-invalid"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			state := testcase.state
			consumerKind := "compile"
			if strings.HasPrefix(testcase.change, "sequence-actionfile") {
				consumerKind = "copy"
			}
			variants := []ActionPlanFamilyVariant{executedArtifactVariantForTest(t, "0"), executedArtifactVariantForTest(t, "1")}
			if strings.HasPrefix(testcase.change, "sequence") {
				for index := range variants {
					variants[index] = executedArtifactSequenceVariantForTest(t, variants[index], testcase.change)
				}
			}
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
			baselineConsumers := 0
			for _, node := range baseline.Nodes {
				if node.Kind == consumerKind {
					baselineConsumers++
				}
			}
			if baselineConsumers != 2 {
				t.Fatalf("fixture baseline has %d %s nodes, want2", baselineConsumers, consumerKind)
			}
			initial, err := BuildConservativeActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			var roots []ActionPlanFamilyExecutionCutRoot
			for _, variant := range variants {
				for _, node := range variant.Snapshot.Nodes {
					if node.Stage == "prehost" && node.Outputs[0].ObservedPath == "" {
						roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: initial.originalNodeIDs[variant.Name][node.ID], Slot: 0})
					}
				}
			}
			if testcase.change == "sequence-unexecuted" {
				roots = nil
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
				if testcase.change == "sequence-invalid" {
					if err == nil || !strings.Contains(err.Error(), "execution completion is not an absent status output") {
						t.Fatalf("expected non-absent completion error, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if testcase.change == "sequence-unexecuted" {
					for _, node := range updated.Nodes {
						if node.Kind == "compile" && !slices.ContainsFunc(node.Inputs, func(edge ActionPlanNodeEdge) bool {
							return edge.Role == "sequence"
						}) {
							t.Fatal("discarded an unexecuted ordering check")
						}
					}
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
			for _, variant := range variants {
				for _, original := range variant.Snapshot.ExecutionCheckRoots {
					id := initial.originalNodeIDs[variant.Name][original]
					if !slices.Contains(family.family.Validations, ActionPlanFamilyValidation{Variant: variant.Name, NodeID: id, Slot: 0}) {
						t.Fatal("lost the original execution check")
					}
				}
			}
			consumers := 0
			for _, node := range family.family.Nodes {
				if node.Kind == consumerKind {
					consumers++
				}
			}
			want := 2
			if state == toolaction.ObservedOutputAbsent && (testcase.change == "" || testcase.change == "work" || testcase.change == "tree" || testcase.change == "sequence" || testcase.change == "sequence-direct" || testcase.change == "sequence-actionfile") {
				want = 1
			}
			if consumers != want {
				t.Fatalf("%s instances=%d, want%d", consumerKind, consumers, want)
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
