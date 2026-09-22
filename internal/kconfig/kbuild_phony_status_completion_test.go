package kconfig

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func makePhonyCompletionSnapshotForTest(t *testing.T, line string, recipeIndex int) ActionPlanSnapshot {
	t.Helper()
	base := familyTestChainSnapshot(t, "make-phony-completion", "")
	plan := snapshotActionPlan(base)
	const (
		sourceID = "src-00000003"
		target   = "scripts_basic"
	)
	plan.Sources = append(plan.Sources, ActionPlanSource{
		ID: sourceID, Namespace: "kernel", Path: "Makefile",
	})
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{"-script_content_base64", base64.StdEncoding.EncodeToString(
			[]byte("#!/bin/sh\nset -e\n" + line + "\n"),
		)},
		WorkingDirectory:            "make-phony-check",
		ObservedOutputs:             map[string]string{"00000000": target},
		RequireAbsentObservedOutput: "00000000",
		RequireUnchangedWorkingTree: true,
		MakePhonyCompletion: &ActionRecipeMakePhonyCompletion{
			Profile: "build:root", Target: target, SourcePath: "Makefile",
			RuleIndex: 1, RecipeIndex: recipeIndex, ExpandedLine: line, SequenceInputs: 1,
		},
		Sources: []string{"makefile:00000000"}, Inputs: []string{"sequence:00000000"},
		Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[recipeID] = recipe
	predecessor := plan.Nodes[0].ID
	node := ActionPlanNode{
		Stage: "target", Kind: "generate", Recipe: recipeID,
		Tool: compactKbuildScriptRunnerRole, Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "makefile", SourceID: sourceID}},
		Inputs:  []ActionPlanNodeEdge{{Role: "sequence", ProducerID: predecessor, Slot: 0}},
		Outputs: []ActionPlanOutput{{
			Tree: "objects", Path: ".linux-bzl-intermediate/check/scripts-basic.state",
			ObservedPath: target,
		}},
	}
	node.ID = node.ContentID()
	plan.Nodes = append(plan.Nodes, node)
	plan.executionCheckRoots = []string{node.ID}
	dependencies := maps.Clone(base.ConfigDependencies)
	dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "selected Make status"}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, base.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestMakePhonyCompletionAuthenticatesExecutionRoot(t *testing.T) {
	for _, test := range []struct {
		name, line string
		index      int
	}{
		{"source cleanup", "rm -f .tmp_quiet_recordmcount", 1},
		{"recursive-only terminal", ":", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := makePhonyCompletionSnapshotForTest(t, test.line, test.index)
			id := snapshot.ExecutionCheckRoots[0]
			plan := snapshotActionPlan(snapshot)
			node, found := compactKbuildPlanNode(plan, id)
			if !found || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, "scripts_basic") {
				t.Fatalf("PHONY Make completion %s was not authenticated", id)
			}
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
			if err != nil {
				t.Fatal(err)
			}
			finalID := family.originalNodeIDs["base"][id]
			if finalID == "" || !slices.Contains(family.Validations, ActionPlanFamilyValidation{
				Variant: "base", NodeID: finalID, Slot: 0,
			}) {
				t.Fatalf("selected Make status %s has no family execution root", finalID)
			}
			for _, view := range family.Views {
				if view.NodeID == finalID {
					t.Fatal("PHONY Make completion was published as an ordinary file")
				}
			}
		})
	}
}

func TestMakePhonyCompletionContentAddressRemapsExecutionRoot(t *testing.T) {
	snapshot := makePhonyCompletionSnapshotForTest(t, "rm -f .tmp_quiet_recordmcount", 1)
	plan := snapshotActionPlan(snapshot)
	const provisionalID = "provisional-phony-completion"
	originalID := snapshot.ExecutionCheckRoots[0]
	changed := false
	for index := range plan.Nodes {
		if plan.Nodes[index].ID == originalID {
			plan.Nodes[index].ID = provisionalID
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("missing PHONY completion to content-address")
	}
	plan.executionCheckRoots = []string{provisionalID}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	if got := plan.executionCheckRoots; !slices.Equal(got, []string{originalID}) {
		t.Fatalf("content-addressed execution roots = %q, want original PHONY completion %q", got, originalID)
	}
	addressed, err := canonicalActionPlanSnapshot(plan, snapshot.ConfigDependencies, snapshot.ConfigFiles)
	if err != nil {
		t.Fatalf("snapshot content-addressed PHONY completion: %v", err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: addressed}})
	if err != nil {
		t.Fatalf("build family with content-addressed PHONY completion: %v", err)
	}
	if finalID := family.originalNodeIDs["base"][originalID]; finalID == "" ||
		!slices.Contains(family.Validations, ActionPlanFamilyValidation{
			Variant: "base", NodeID: finalID, Slot: 0,
		}) {
		t.Fatalf("content-addressed PHONY completion %q has no family execution root", originalID)
	}
}

func TestMakePhonyCompletionContentAddressRejectsUnknownExecutionRoot(t *testing.T) {
	snapshot := makePhonyCompletionSnapshotForTest(t, "rm -f .tmp_quiet_recordmcount", 1)
	plan := snapshotActionPlan(snapshot)
	plan.executionCheckRoots = []string{"missing-phony-completion"}
	if err := contentAddressActionPlanNodes(plan); err == nil ||
		!strings.Contains(err.Error(), "execution check root references unknown node missing-phony-completion") {
		t.Fatalf("unknown PHONY completion root error = %v", err)
	}
}

func TestMakePhonyCompletionRejectsAlteredRunnerOrSourceBindings(t *testing.T) {
	snapshot := makePhonyCompletionSnapshotForTest(t, "rm -f .tmp_quiet_recordmcount", 1)
	plan := snapshotActionPlan(snapshot)
	node, found := compactKbuildPlanNode(plan, snapshot.ExecutionCheckRoots[0])
	if !found {
		t.Fatal("missing Make completion node")
	}
	recipe := plan.Recipes[node.Recipe]
	for _, test := range []struct {
		name   string
		change func(*ActionPlan, *ActionPlanNode, *ActionRecipe)
	}{
		{"changed script", func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe) {
			r.Arguments[1] = base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\nset -e\nfalse\n"))
		}},
		{"fallback status", func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe) {
			r.Arguments = append(r.Arguments, "-timeout_seconds", "1", "-fallback_stdout_base64", "eA==")
		}},
		{"weakened tree assertion", func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe) {
			r.RequireUnchangedWorkingTree = false
		}},
		{"ordinary output", func(_ *ActionPlan, _ *ActionPlanNode, r *ActionRecipe) {
			r.WorkingOutputs = map[string]string{"00000000": "scripts_basic"}
		}},
		{"wrong Makefile", func(p *ActionPlan, _ *ActionPlanNode, _ *ActionRecipe) {
			p.Sources[len(p.Sources)-1].Path = "other/Makefile"
		}},
		{"script masquerading as Makefile", func(_ *ActionPlan, n *ActionPlanNode, r *ActionRecipe) {
			n.Sources[0].Role = "script"
			r.Sources[0] = "script:00000000"
		}},
		{"wrong logical target", func(_ *ActionPlan, n *ActionPlanNode, _ *ActionRecipe) {
			n.Outputs[0].ObservedPath = "other"
		}},
		{"published completion path", func(_ *ActionPlan, n *ActionPlanNode, _ *ActionRecipe) {
			n.Outputs[0].Path = "scripts_basic"
		}},
		{"missing child ordering", func(_ *ActionPlan, n *ActionPlanNode, r *ActionRecipe) {
			n.Inputs = nil
			r.Inputs = nil
		}},
		{"duplicated child", func(_ *ActionPlan, n *ActionPlanNode, r *ActionRecipe) {
			n.Inputs = append(n.Inputs, n.Inputs[0])
			r.Inputs = append(r.Inputs, "sequence:00000001")
			r.MakePhonyCompletion.SequenceInputs = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := snapshotActionPlan(snapshot)
			alteredNode := node
			alteredNode.Sources = slices.Clone(node.Sources)
			alteredNode.Inputs = slices.Clone(node.Inputs)
			alteredNode.Outputs = slices.Clone(node.Outputs)
			alteredRecipe := cloneActionRecipe(recipe)
			test.change(candidate, &alteredNode, &alteredRecipe)
			candidate.Recipes[node.Recipe] = alteredRecipe
			if compactKbuildAuthenticatedExecutionCheckCompletion(candidate, alteredNode, "scripts_basic") {
				t.Fatal("accepted altered Make source or status recipe")
			}
		})
	}
	invalid := snapshot
	for _, other := range snapshot.Nodes {
		if other.ID != node.ID {
			invalid.ExecutionCheckRoots = []string{other.ID}
			break
		}
	}
	if err := invalid.validate(); err == nil || !strings.Contains(err.Error(), "authenticated completion") {
		t.Fatalf("invalid snapshot execution root = %v, want authentication rejection", err)
	}
}

func TestMakePhonyCompletionCheckpointRetainsAuthenticatedRoot(t *testing.T) {
	snapshot := makePhonyCompletionSnapshotForTest(t, "rm -f .tmp_quiet_recordmcount", 1)
	plan := snapshotActionPlan(snapshot)
	plan.selectionGraph = &compactKbuildSelectionGraph{profiles: map[string]CompactKbuildProfile{}}
	plan.metadata = &CompactMetadata{
		configFragment:       map[string]string{},
		configSymbolUniverse: []string{},
		actionContracts:      map[KbuildActionRoleRef]CompactKbuildActionContract{},
	}
	bindings := ActionPlanCheckpointBindings{
		Variant:         "base",
		SourceArtifacts: map[string]string{"linux": t.TempDir()},
		Toolsets:        maps.Clone(plan.Toolsets),
		ConfigValues:    maps.Clone(plan.metadata.configFragment),
		ConfigFiles:     maps.Clone(snapshot.ConfigFiles),
		ActionContracts: maps.Clone(plan.metadata.actionContracts),
	}
	data, err := CaptureActionPlanCheckpoint(plan, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreActionPlanCheckpoint(data, bindings); err != nil {
		t.Fatalf("restore source-bound PHONY status: %v", err)
	}
	var checkpoint actionPlanCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	id := checkpoint.ExecutionCheckRoots[0]
	node, found := compactKbuildPlanNode(checkpoint.Plan, id)
	if !found {
		t.Fatal("checkpoint lost PHONY status root")
	}
	recipe := checkpoint.Plan.Recipes[node.Recipe]
	recipe.MakePhonyCompletion.ExpandedLine = "false"
	checkpoint.Plan.Recipes[node.Recipe] = recipe
	altered, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreActionPlanCheckpoint(altered, bindings); err == nil || !strings.Contains(err.Error(), "authenticated completion") {
		t.Fatalf("altered checkpoint Make status = %v, want authentication rejection", err)
	}
}
