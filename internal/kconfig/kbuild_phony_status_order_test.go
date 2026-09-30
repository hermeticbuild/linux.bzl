package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func selectedPhonyStatusOrderProfile(t *testing.T, source string) CompactKbuildProfile {
	t.Helper()
	profile, _, _ := selectedControlTestProfile(t, source)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("status", "status", ""); err != nil {
		t.Fatal(err)
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "status", RuleIndex: selectedControlTestRuleIndex(t, profile, "status"), RecipeIndex: 0,
	}, selectedControlTestFrontier("before-status", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(line); err != nil {
		t.Fatal(err)
	}
	evaluation, err := stepper.Finish(selectedControlTestFrontier("after-status", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	evaluation.Profile.EntryTargets = []string{"status"}
	return evaluation.Profile
}

func TestSelectedPhonyStatusOrdersConsumerAndRetainsFilePrerequisite(t *testing.T) {
	profile := selectedPhonyStatusOrderProfile(t, `.PHONY: status
status: leaf.out
	@test -e leaf.out
leaf.out:
	@touch $@
consumer.out: status
	@touch $@
`)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "leaf.out", MakeTarget: "leaf.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "status", MakeTarget: "status", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	consumer := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: "consumer.out"}]
	status := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: "status"}]
	leaf := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: "leaf.out"}]
	if _, ownsFile := graph.owner("status"); ownsFile {
		t.Fatal("PHONY status unexpectedly registered as a regular file owner")
	}
	dependencies, err := graph.selectionDependencies(&CompactMetadata{Config: config}, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dependencies, status) || !slices.Contains(dependencies, leaf) {
		t.Fatalf("consumer dependencies = %#v, want source status %#v and its ordinary prerequisite %#v", dependencies, status, leaf)
	}
}

func TestRecursivePhonyStatusTerminalKeepsSourceShellResult(t *testing.T) {
	for _, test := range []struct {
		name, command, wantError string
	}{
		{name: "no-op status", command: "@:"},
		{name: "failed status", command: "@false", wantError: "fails its source shell status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := selectedPhonyStatusOrderProfile(t, ".PHONY: status\nstatus:\n\t"+test.command+"\n")
			parent := CompactKbuildProfile{
				Name: "parent", Path: "parent.mk", EntryTargets: []string{"parent"},
				TargetInvocationDependencies: []CompactKbuildInvocationDependency{{
					Target: "parent", Profile: child.Name, Goals: []string{"status"}, ReplayArguments: []string{"status"},
				}},
			}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{parent, child},
				KbuildSelections: []CompactKbuildSelection{{
					Profile: child.Name, Target: "status", MakeTarget: "status", Lifecycle: "target", Scope: "target", Stage: "target",
				}},
			}
			metadata := &CompactMetadata{Config: config}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			terminal, err := graph.compactKbuildTerminalRecipeSelections(metadata, child.Name, "target")
			if err != nil {
				t.Fatal(err)
			}
			if len(terminal) != 1 || terminal[0].target != "status" {
				t.Fatalf("recursive source-status terminals = %#v, want selected PHONY recipe", terminal)
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{Recipes: map[string]ActionRecipe{}}).
				withSelectionGraph(graph).forProfile(parent).forOutput("target", "target", "kernel")
			materialization, err := builder.compactKbuildInvocationDependencyMaterialization(
				"parent", parent, parent.TargetInvocationDependencies[0],
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || !strings.Contains(err.Error(), "Makefile:") {
					t.Fatalf("recursive source status error = %v, want source-located %q", err, test.wantError)
				}
				return
			}
			if err != nil || len(materialization.outputs) != 0 || len(materialization.roots) != 0 || len(materialization.statusRoots) != 0 {
				t.Fatalf("no-op PHONY recursive materialization = %#v, error %v; want no fabricated file or status", materialization, err)
			}
		})
	}
}
