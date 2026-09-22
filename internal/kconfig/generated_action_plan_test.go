package kconfig

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSelectedPhonyFeatureGateUsesFrozenSourceStatus(t *testing.T) {
	for _, test := range []struct {
		name, target, value, recipe, wantError string
	}{
		{name: "libelf success", target: "elfdep", value: "1", recipe: `@if [ "$(feature-libelf)" != "1" ]; then echo "No libelf found"; exit 1 ; fi`},
		{name: "libelf failure", target: "elfdep", value: "0", recipe: `@if [ "$(feature-libelf)" != "1" ]; then echo "No libelf found"; exit 1 ; fi`, wantError: "No libelf found"},
		{name: "zlib success", target: "zdep", value: "1", recipe: `@if [ "$(feature-zlib)" != "1" ]; then echo "No zlib found"; exit 1 ; fi`},
		{name: "zlib failure", target: "zdep", value: "0", recipe: `@if [ "$(feature-zlib)" != "1" ]; then echo "No zlib found"; exit 1 ; fi`, wantError: "No zlib found"},
		{name: "measured success", value: "1", recipe: `@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`},
		{name: "measured failure", value: "0", recipe: `@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`, wantError: "BPF API too old"},
		{name: "unknown result", value: "pending", recipe: `@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`, wantError: "not a resolved boolean"},
		{name: "changed failure status", value: "1", recipe: `@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 0 ; fi`, wantError: "unsupported failure branch"},
		{name: "changed comparison", value: "1", recipe: `@if [ "$(feature-bpf)" != "0" ]; then echo "BPF API too old"; exit 1 ; fi`, wantError: "unsupported source condition"},
		{name: "extra shell write", value: "1", recipe: `@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi; echo modified > bpfdep`, wantError: "unsupported failure branch"},
		{name: "prefixed shell write", value: "1", recipe: `@echo modified > bpfdep; if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`, wantError: "unsupported source condition"},
		{name: "second selected command", value: "1", recipe: "@if [ \"$(feature-bpf)\" != \"1\" ]; then echo \"BPF API too old\"; exit 1 ; fi\n\t@echo modified > bpfdep", wantError: "requires one complete selected source line"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := test.target
			if target == "" {
				target = "bpfdep"
			}
			profile, _, _ := selectedControlTestProfile(t,
				"feature-bpf := "+test.value+"\nfeature-libelf := "+test.value+"\nfeature-zlib := "+test.value+
					"\n.PHONY: "+target+"\n"+target+":\n\t"+test.recipe+"\n")
			frontier := selectedControlTestFrontier("selected-feature-gate", selectedControlTestFiles{}, KbuildControlReadArtifact{})
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget(target, target, ""); err != nil {
				t.Fatal(err)
			}
			for recipeIndex := range profile.Rules[selectedControlTestRuleIndex(t, profile, target)].Recipe {
				line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
					Target: target, RuleIndex: selectedControlTestRuleIndex(t, profile, target), RecipeIndex: recipeIndex,
				}, frontier)
				if err != nil {
					t.Fatal(err)
				}
				if err := stepper.ApplyRecipe(line); err != nil {
					t.Fatal(err)
				}
			}
			evaluation, err := stepper.Finish(frontier)
			if err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{evaluation.Profile}}}
			proven, err := metadata.compactKbuildSelectedPhonyFeatureGate(evaluation.Profile, target, target)
			if !proven {
				t.Fatalf("source feature status was not classified, error %v", err)
			}
			if test.wantError == "" && err != nil || test.wantError != "" &&
				(err == nil || !strings.Contains(err.Error(), test.wantError) || !strings.Contains(err.Error(), "Makefile:")) {
				t.Fatalf("feature status error = %v, want source-located %q", err, test.wantError)
			}
		})
	}
}

func TestGeneratedActionPlanInspectsSourceSelectedPhonyStatus(t *testing.T) {
	for _, test := range []struct {
		name, value, recipe, wantError, wantLine string
	}{
		{name: "failed aliased feature keeps failing source status", value: "0", recipe: `@if [ "$(CHECK_BPF)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`, wantLine: `if [ "0" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`},
		{name: "successful aliased feature still runs source status", value: "1", recipe: `@if [ "$(CHECK_BPF)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`, wantLine: `if [ "1" != "1" ]; then echo "BPF API too old"; exit 1 ; fi`},
		{name: "bare failure", value: "1", recipe: "@false", wantError: "fails its source shell status"},
		{name: "bare status check", value: "1", recipe: "@test -e missing.file", wantLine: "test -e missing.file"},
		{name: "inert status", value: "1", recipe: "@:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const target = "bpfdep"
			profile, _, _ := selectedControlTestProfile(t, `
feature-bpf := `+test.value+`
CHECK_BPF := $(feature-bpf)
.PHONY: bpfdep
bpfdep:
	`+test.recipe+`
`)
			frontier := selectedControlTestFrontier("selected-aliased-feature-gate", selectedControlTestFiles{}, KbuildControlReadArtifact{})
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget(target, target, ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: target, RuleIndex: selectedControlTestRuleIndex(t, profile, target), RecipeIndex: 0,
			}, frontier)
			if err != nil {
				t.Fatal(err)
			}
			if err := stepper.ApplyRecipe(line); err != nil {
				t.Fatal(err)
			}
			evaluation, err := stepper.Finish(frontier)
			if err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				Config: CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{evaluation.Profile},
					KbuildSelections: []CompactKbuildSelection{{
						Profile: profile.Name, Target: target, MakeTarget: target,
						Lifecycle: "target", Scope: "target", Stage: "target",
					}},
				},
				configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
			}
			plan := &ActionPlan{metadata: metadata, Recipes: map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
			graph, err := metadata.appendGeneratedActionPlan(plan)
			if test.wantError == "" && err != nil || test.wantError != "" &&
				(err == nil || !strings.Contains(err.Error(), test.wantError) || !strings.Contains(err.Error(), "Makefile:")) {
				t.Fatalf("selected PHONY %s completion = %v, want source-located %q", test.name, err, test.wantError)
			}
			if err != nil || test.wantLine == "" {
				return
			}
			key := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
			producer := graph.materializedProducers[key]
			node, exists := compactKbuildPlanNode(plan, producer)
			if !exists || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, target) {
				t.Fatalf("PHONY %q lacks a source-authenticated outputless status action", target)
			}
			receipt := plan.Recipes[node.Recipe].MakePhonyCompletion
			if receipt == nil || receipt.ExpandedLine != test.wantLine ||
				receipt.RecipeIndex != 0 || receipt.SourcePath != "Makefile" {
				t.Fatalf("selected PHONY source status = %#v, want exact Makefile line %q", receipt, test.wantLine)
			}
		})
	}
}

func TestSelectedPhonyRecursiveChildCompletesBeforeSourceCleanup(t *testing.T) {
	const childTarget = "scripts/basic/fixdep"
	parent, _, _ := selectedControlTestProfile(t, `
Q := @
.PHONY: scripts_basic
scripts_basic:
	$(Q)$(MAKE) child
	$(Q)rm -f .tmp_quiet_recordmcount
`, map[string]string{"MAKE": CompactKbuildRecursiveMakeProvenanceToken})
	child := mustCompactKbuildProfileForTest(t, "build:scripts/basic", "scripts/basic/Makefile.build", "scripts/basic", `
scripts/basic/fixdep:
	@printf 'selected-child\n' > $@
`, nil)
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/scripts_basic/basic-fixdep",
		Version: "sha256:source-child-fixdep", Producer: CompactKbuildVisibleArtifact{
			Path: childTarget, Profile: child.Name, Target: childTarget,
		},
	}
	before := selectedControlTestFrontier("before-recursive-basic", selectedControlTestFiles{}, artifact)
	after := selectedControlTestFrontier("after-recursive-basic", selectedControlTestFiles{
		files: map[string]testKbuildVirtualFile{
			"__LINUX_BZL_OBJECT_TREE__/" + childTarget: {content: "selected-child\n", exact: true},
		},
	}, artifact)
	stepper, err := NewSelectedKbuildControlStepper(parent, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("scripts_basic", "scripts_basic", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, parent, "scripts_basic")
	for recipeIndex, frontier := range []KbuildControlRecipeFrontier{before, after} {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: "scripts_basic", RuleIndex: index, RecipeIndex: recipeIndex,
		}, frontier)
		if err != nil {
			t.Fatal(err)
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := stepper.Finish(after)
	if err != nil {
		t.Fatal(err)
	}
	parent = evaluation.Profile
	parent.EntryTargets = []string{"scripts_basic"}
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "scripts_basic", Profile: child.Name, Goals: []string{childTarget}, ReplayArguments: []string{"child"},
	}}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, child},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: child.Name, Target: childTarget, MakeTarget: childTarget, Lifecycle: "prep", Scope: "host", Stage: "prehost"},
			{Profile: parent.Name, Target: "scripts_basic", MakeTarget: "scripts_basic", Lifecycle: "prep", Scope: "host", Stage: "prehost"},
		},
	}
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity}}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	childKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: child.Name, target: childTarget}]
	parentKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: parent.Name, target: "scripts_basic"}]
	childProducer := graph.materializedProducers[childKey]
	parentProducer := graph.materializedProducers[parentKey]
	statusNode, found := compactKbuildPlanNode(plan, parentProducer)
	if !found || childProducer == "" || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, statusNode, "scripts_basic") {
		t.Fatalf("selected child %q / cleanup %q have no source-authenticated action order", childProducer, parentProducer)
	}
	if !slices.ContainsFunc(statusNode.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.Role == "sequence" && edge.ProducerID == childProducer && edge.Slot == 0
	}) {
		t.Fatalf("cleanup status inputs = %#v, want source-selected child sequence", statusNode.Inputs)
	}
	statusRecipe := plan.Recipes[statusNode.Recipe]
	if statusRecipe.MakePhonyCompletion == nil || statusRecipe.MakePhonyCompletion.RecipeIndex != 1 ||
		statusRecipe.MakePhonyCompletion.ExpandedLine != "rm -f .tmp_quiet_recordmcount" ||
		statusRecipe.MakePhonyCompletion.SequenceInputs != 1 ||
		!statusRecipe.RequireUnchangedWorkingTree {
		t.Fatalf("cleanup status receipt = %#v, want exact second Make line and private status", statusRecipe)
	}
	if _, ownsFile := graph.owner("scripts_basic"); ownsFile {
		t.Fatal("PHONY status registered as ordinary object file")
	}
}

func TestSelectedPhonyStatusIsAnExecutionPrerequisiteWithoutAFile(t *testing.T) {
	profile := selectedPhonyStatusOrderProfile(t, `.PHONY: status
status: leaf.out
	@test -e leaf.out
leaf.out:
	@printf 'leaf\n' > $@
consumer.out: status
	@printf 'consumer\n' > $@
`)
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: "leaf.out", MakeTarget: "leaf.out", Lifecycle: "target", Scope: "target", Stage: "target"},
				{Profile: profile.Name, Target: "status", MakeTarget: "status", Lifecycle: "target", Scope: "target", Stage: "target"},
				{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			},
		},
		configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{
		"target": actionPlanTestProbeIdentity,
	}}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	status := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: "status"}]
	consumer := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: "consumer.out"}]
	statusProducer := graph.materializedProducers[status]
	consumerProducer := graph.materializedProducers[consumer]
	statusNode, present := compactKbuildPlanNode(plan, statusProducer)
	if !present || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, statusNode, "status") {
		t.Fatalf("selected PHONY status producer %q lacks private completion", statusProducer)
	}
	consumerNode, present := compactKbuildPlanNode(plan, consumerProducer)
	if !present || !slices.ContainsFunc(consumerNode.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.Role == "sequence" && edge.ProducerID == statusProducer && edge.Slot == 0
	}) {
		t.Fatalf("selected consumer inputs %#v omit source status %q", consumerNode.Inputs, statusProducer)
	}
	if _, ownsFile := graph.owner("status"); ownsFile {
		t.Fatal("PHONY status became an ordinary Make artifact")
	}
}

func TestNestedUnselectedRecursiveRecipeRetainsPhonyStatus(t *testing.T) {
	child, _, _ := selectedControlTestProfile(t, `.PHONY: status
status:
	@test "1" = "1"
`)
	stepper, err := NewSelectedKbuildControlStepper(child, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("status", "status", ""); err != nil {
		t.Fatal(err)
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "status", RuleIndex: selectedControlTestRuleIndex(t, child, "status"), RecipeIndex: 0,
	}, selectedControlTestFrontier("before-nested-status", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(line); err != nil {
		t.Fatal(err)
	}
	evaluation, err := stepper.Finish(selectedControlTestFrontier("after-nested-status", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	child = evaluation.Profile
	parent := mustCompactKbuildProfileForTest(t, "driver:nested", "nested/Makefile", "", `
outer.out: sub.out
	@printf 'outer\n' > $@
sub.out:
	@$(MAKE) child; printf 'sub\n' > $@
`, map[string]string{"MAKE": CompactKbuildRecursiveMakeProvenanceToken})
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "sub.out", Profile: child.Name, Goals: []string{"status"}, ReplayArguments: []string{"child"},
	}}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{parent, child},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: child.Name, Target: "status", MakeTarget: "status", Lifecycle: "prep", Scope: "host", Stage: "host"},
				{Profile: parent.Name, Target: "outer.out", MakeTarget: "outer.out", Lifecycle: "prep", Scope: "host", Stage: "host"},
			},
		},
		configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity}}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	key := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: child.Name, target: "status"}]
	statusProducer := graph.materializedProducers[key]
	if statusProducer == "" {
		t.Fatal("recursive PHONY child has no outputless completion")
	}
	var subordinate ActionPlanNode
	for _, node := range plan.Nodes {
		if slices.ContainsFunc(node.Outputs, func(output ActionPlanOutput) bool { return output.Path == "sub.out" }) {
			subordinate = node
			break
		}
	}
	if subordinate.ID == "" || !slices.ContainsFunc(subordinate.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.Role == "sequence" && edge.ProducerID == statusProducer && edge.Slot == 0
	}) {
		t.Fatalf("unselected nested sub.out inputs = %#v, want child PHONY status %q", subordinate.Inputs, statusProducer)
	}
}

func TestSelectedRecursiveStatusRejectsDifferentReplayArguments(t *testing.T) {
	child := selectedPhonyStatusOrderProfile(t, ".PHONY: status\nstatus:\n\t@test 1 = 1\n")
	parent := mustCompactKbuildProfileForTest(t, "driver:parent", "parent/Makefile", "", `
sub.out:
	@$(MAKE) child; printf 'sub\n' > $@
`, map[string]string{"MAKE": CompactKbuildRecursiveMakeProvenanceToken})
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "sub.out", Profile: child.Name, Goals: []string{"status"}, ReplayArguments: []string{"child"},
	}}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{parent, child}}}
	graph, err := newCompactKbuildSelectionGraph(metadata.Config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).withSelectionGraph(graph).forProfile(parent)
	_, err = builder.appendCompactKbuildSelectedPlanNode("sub.out", ActionPlanNode{
		Outputs: []ActionPlanOutput{{Path: "sub.out"}},
	}, ActionRecipe{CommandReplays: []ActionRecipeCommandReplay{{
		Name:        CompactKbuildRecursiveMakeReplayName,
		Invocations: []ActionRecipeCommandReplayInvocation{{Arguments: []string{"unrelated"}}},
	}}})
	if err == nil || !strings.Contains(err.Error(), `has no exact selected command replay`) || len(plan.Nodes) != 0 {
		t.Fatalf("mismatched recursive status replay: nodes %#v, error %v", plan.Nodes, err)
	}
}

func TestSelectedFeatureChecksCompleteRecursiveLibbpfWithoutPhonyFiles(t *testing.T) {
	const (
		static = "tools/lib/bpf/libbpf-in.o"
		shared = "tools/lib/bpf/libbpf-shared-in.o"
	)
	profile, _, _ := selectedControlTestProfile(t, `
feature-libelf := 1
feature-zlib := 1
feature-bpf := 1
PHONY += elfdep zdep bpfdep
.PHONY: $(PHONY)
all: `+static+` `+shared+`
elfdep:
	@if [ "$(feature-libelf)" != "1" ]; then echo "No libelf found"; exit 1 ; fi
zdep:
	@if [ "$(feature-zlib)" != "1" ]; then echo "No zlib found"; exit 1 ; fi
bpfdep:
	@if [ "$(feature-bpf)" != "1" ]; then echo "BPF API too old"; exit 1 ; fi
`+static+` `+shared+`: elfdep zdep bpfdep
	@printf 'built\n' > $@
`)
	frontier := selectedControlTestFrontier("selected-libbpf-feature-gates", selectedControlTestFiles{}, KbuildControlReadArtifact{})
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"elfdep", "zdep", "bpfdep", static, shared} {
		if _, err := stepper.BeginTarget(target, target, ""); err != nil {
			t.Fatal(err)
		}
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: target, RuleIndex: selectedControlTestRuleIndex(t, profile, target), RecipeIndex: 0,
		}, frontier)
		if err != nil {
			t.Fatal(err)
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := stepper.Finish(frontier)
	if err != nil {
		t.Fatal(err)
	}
	profile = evaluation.Profile
	profile.TargetInvocationDependencies = nil
	parent := mustCompactKbuildProfileForTest(t, "driver:resolve_btfids", "tools/bpf/resolve_btfids/Makefile", "tools/bpf/resolve_btfids", `
resolve_btfids:
	@echo selected
`, nil)
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "tools/bpf/resolve_btfids/resolve_btfids", Profile: profile.Name,
		Goals: []string{"all"}, ReplayArguments: []string{"all"},
	}}
	selections := []CompactKbuildSelection{}
	for _, target := range []string{"elfdep", "zdep", "bpfdep", static, shared} {
		selections = append(selections, CompactKbuildSelection{
			Profile: profile.Name, Target: target, MakeTarget: target,
			Lifecycle: "prep", Scope: "host", Stage: "host",
		})
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles:   []CompactKbuildProfile{profile, parent},
			KbuildSelections: selections,
		},
		actionRoles:    testConfiguredScopedActionRoles,
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"elfdep", "zdep", "bpfdep"} {
		key := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
		if !graph.compactKbuildProfileTargetIsPhony(profile, target) || graph.materializedProducers[key] != "" {
			t.Fatalf("proved source check %q has materialized file owner %q", target, graph.materializedProducers[key])
		}
	}
	for _, target := range []string{static, shared} {
		key := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
		if graph.materializedProducers[key] == "" {
			t.Fatalf("regular libbpf consumer %q was not materialized", target)
		}
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).forProfile(parent).forOutput("host", "host", "sdk")
	materialization, err := builder.compactKbuildInvocationDependencyMaterialization(
		"tools/bpf/resolve_btfids/resolve_btfids", parent, parent.TargetInvocationDependencies[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(materialization.outputs, []string{"${work:root}/" + static, "${work:root}/" + shared}) {
		t.Fatalf("recursive libbpf output paths = %#v, want only regular consumers", materialization.outputs)
	}
}

func TestGeneratedActionPlanMaterializesSelectedGroupedPatternPeersOnce(t *testing.T) {
	const (
		generatedC = "scripts/dtc/dtc-parser.tab.c"
		generatedH = "scripts/dtc/dtc-parser.tab.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/dtc", "scripts/Makefile.host", "scripts/dtc", `
cmd_bison = $(YACC) -o $(basename $@).c --defines=$(basename $@).h -t -l $<
scripts/dtc/%.tab.c scripts/dtc/%.tab.h: scripts/dtc/%.y FORCE
	$(call if_changed,bison)
`, map[string]string{"YACC": KbuildActionRoleToken("host", "bison")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "scripts/dtc/dtc-parser.y")
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("bison"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: generatedH, MakeTarget: generatedH, GroupedTrigger: generatedC, Lifecycle: "target", Scope: "host", Stage: "host"},
				{Profile: profile.Name, Target: generatedC, MakeTarget: generatedC, GroupedTrigger: generatedC, Lifecycle: "target", Scope: "host", Stage: "host"},
			},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.selectedRootRuleResolutions) != 0 {
		t.Fatalf("completed action plan retained %d selected-root rule resolutions", len(graph.selectedRootRuleResolutions))
	}

	bisonNodes := []ActionPlanNode{}
	for _, node := range plan.Nodes {
		if plan.Recipes[node.Recipe].Tool == "bison" {
			bisonNodes = append(bisonNodes, node)
		}
	}
	if len(bisonNodes) != 1 {
		t.Fatalf("bison nodes = %#v, want one grouped recipe invocation", bisonNodes)
	}
	producer := bisonNodes[0]
	wantOutputs := map[string]bool{generatedC: true, generatedH: true}
	if len(producer.Outputs) != len(wantOutputs) {
		t.Fatalf("grouped bison outputs = %#v, want %#v", producer.Outputs, wantOutputs)
	}
	for _, output := range producer.Outputs {
		if output.Tree != "host" || output.ArtifactPath != "" || !wantOutputs[output.Path] {
			t.Fatalf("grouped bison output = %#v, want canonical host output in %#v", output, wantOutputs)
		}
	}
	for _, target := range []string{generatedC, generatedH} {
		key := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "host"}
		if got := graph.materializedProducers[key]; got != producer.ID {
			t.Fatalf("selection %s producer = %q, want shared producer %q", compactKbuildSelectionKeyString(key), got, producer.ID)
		}
		if got, _, ok := planProducerByOutput(plan, "host", target); !ok || got != producer.ID {
			t.Fatalf("canonical host output %q producer = %q, %t; want %q", target, got, ok, producer.ID)
		}
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("grouped two-peer action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanMaterializesSelectedParentTraversalMakeTarget(t *testing.T) {
	const (
		directory  = "arch/x86/kvm"
		target     = "virt/kvm/kvm_main.o"
		makeTarget = directory + "/../../../virt/kvm/kvm_main.o"
		source     = "virt/kvm/kvm_main.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
stem_cflags = $(CFLAGS_$(target-stem).o)
CFLAGS_../../../virt/kvm/kvm_main.o = -DLEXICAL_TARGET_STEM
cmd_cc_o_c = $(CC) $(object_cflags) $(stem_cflags) -DRELATIVE_OBJECT=$(patsubst $(obj)/%,%,$@) -c -o $@ $<
$(obj)/%.o: private object_cflags = -DLEXICAL_KVM_TARGET
virt/kvm/kvm_main.o: FORCE
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":  KbuildActionRoleToken("target", "cc"),
		"obj": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: makeTarget,
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if orderingOnly, err := metadata.compactKbuildTargetIsOrderingOnlyInProfile(profile, target); err != nil {
		t.Fatal(err)
	} else if !orderingOnly {
		t.Fatalf("canonical target %q did not reproduce the prerequisite-only classification", target)
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.selectedRootRuleResolutions) != 0 || len(graph.ruleResolutions) != 0 {
		t.Fatalf(
			"completed parent-traversal plan retained selected/general rule resolutions %d/%d",
			len(graph.selectedRootRuleResolutions), len(graph.ruleResolutions),
		)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("generated plan omits canonical selected target %q: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("generated plan target producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	for _, argument := range []string{
		"-DLEXICAL_KVM_TARGET",
		"-DLEXICAL_TARGET_STEM",
		"-DRELATIVE_OBJECT=../../../virt/kvm/kvm_main.o",
	} {
		if !slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("generated selected-target arguments omit %q: %q", argument, recipe.Arguments)
		}
	}
	for _, candidate := range plan.Nodes {
		for _, output := range candidate.Outputs {
			if strings.Contains(output.Path, "..") || strings.Contains(output.ArtifactPath, "..") {
				t.Fatalf("generated plan leaked lexical traversal into artifact identity %#v", output)
			}
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("generated parent-traversal selection plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanMaterializesObjectRootedSelectionOutsideInvocationCwd(t *testing.T) {
	const (
		target = "tools/objtool/libsubcmd/exec-cmd.o"
		source = "tools/lib/subcmd/exec-cmd.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", "libsubcmd", `
cmd_cc_o_c = $(CC) -c -o $@ $<
$(OUTPUT)%.o: %.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":     KbuildActionRoleToken("host", "cc"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "host", Stage: "host",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": actionPlanTestProbeIdentity,
			"host":   actionPlanTestProbeIdentity,
		},
		Recipes: map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "host", target)
	if !ok {
		t.Fatalf("generated plan omits object-rooted selected target %q: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("generated plan target producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if got, want := recipe.Arguments, []string{"-c", "-o", "${output:00000000}", "${source:object:00000000}"}; !slices.Equal(got, want) {
		t.Fatalf("object-rooted selection arguments = %q, want %q", got, want)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("object-rooted selection working output = %q, want %q", got, target)
	}
	for _, directory := range recipe.WorkingDirectories {
		if strings.Contains(directory, "tools/lib/subcmd/tools/objtool") {
			t.Fatalf("object-rooted selection was rescoped below source cwd: %q", recipe.WorkingDirectories)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("object-rooted selection plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanSelectsSourceReleaseWriterAndSDKProjection(t *testing.T) {
	const (
		baselineInput = "include/config/auto.conf"
		configOutput  = "include/config/kernel.release"
		consumer      = "include/generated/release-consumer"
	)
	profile := mustCompactKbuildProfileForTest(t, "prep:config-refresh", "Makefile", "", `
cmd_release = $(AWK) $(objtree)/include/config/auto.conf -o $@
cmd_consume = $(AWK) $(objtree)/include/config/kernel.release -o $@
include/config/kernel.release: FORCE
	$(call if_changed,release)
include/generated/release-consumer: include/config/kernel.release FORCE
	$(call if_changed,consume)
`, map[string]string{
		"AWK":     KbuildActionRoleToken("target", "awk"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	metadata := &CompactMetadata{configProjectionPaths: recognizedConfigDocuments(),
		actionRoles: testScopedActionRoles("awk"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: configOutput, MakeTarget: configOutput, Lifecycle: "prep", Scope: "target", Stage: "prep"},
				{Profile: profile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}

	configSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "config" && source.Path == baselineInput {
			configSourceID = source.ID
			break
		}
	}
	if configSourceID == "" {
		t.Fatalf("plan sources omit config/%s: %#v", baselineInput, plan.Sources)
	}
	for _, source := range plan.Sources {
		if source.Namespace == "config" && source.Path == "kernel.release" {
			t.Fatalf("source-selected kernel.release was seeded by Kconfig: %#v", source)
		}
	}
	writerID, _, ok := planProducerByOutput(plan, "prep", configOutput)
	if !ok {
		t.Fatalf("plan omits selected config writer for prep/%s", configOutput)
	}
	writer, ok := compactKbuildPlanNode(plan, writerID)
	if !ok || plan.Recipes[writer.Recipe].Tool != "awk" {
		t.Fatalf("config writer = %#v, want selected awk action", writer)
	}
	consumerID, _, ok := planProducerByOutput(plan, "prep", consumer)
	if !ok {
		t.Fatalf("plan omits config consumer prep/%s", consumer)
	}
	consumerNode, _ := compactKbuildPlanNode(plan, consumerID)
	if !slices.ContainsFunc(consumerNode.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == writerID
	}) {
		t.Fatalf("config consumer inputs = %#v, want final writer %s", consumerNode.Inputs, writerID)
	}

	seedModuleSDKPlanOutputForTest(t, plan, "target", "metadata", "Module.symvers", "module_symvers")
	plan.Products = append(plan.Products, ActionPlanProduct{
		Name: "module_symvers", Tree: "metadata", Path: "Module.symvers",
	})
	seedModuleSDKVmlinuxForTest(t, plan)
	if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sdkProjection := moduleSDKProjectionNodeForTest(t, plan, configOutput)
	if len(sdkProjection.Inputs) != 1 || sdkProjection.Inputs[0].ProducerID != writerID {
		t.Fatalf("SDK config projection inputs = %#v, want selected writer %s", sdkProjection.Inputs, writerID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("selected config-writer plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanBindsConfigProjectionAsNativePrerequisite(t *testing.T) {
	const (
		target       = "scripts/target.json"
		prerequisite = "include/config/auto.conf"
		marker       = "include/config/USED"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts", "scripts/Makefile", "scripts", `
cmd_target_json = cp $< $@
scripts/target.json: include/config/auto.conf include/config/USED FORCE
	$(call if_changed,target_json)
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{configProjectionPaths: append(recognizedConfigDocuments(), marker),
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "prep", Scope: "target", Stage: "prep",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	producerID, _, ok := planProducerByOutput(plan, "prep", target)
	if !ok {
		t.Fatalf("plan omits prep/%s: %#v", target, plan.Nodes)
	}
	producer, ok := compactKbuildPlanNode(plan, producerID)
	if !ok {
		t.Fatalf("missing target producer %q", producerID)
	}
	recipe := plan.Recipes[producer.Recipe]
	for _, pathname := range []string{prerequisite, marker} {
		configSourceID := ""
		for _, source := range plan.Sources {
			if source.Namespace == "config" && source.Path == pathname {
				configSourceID = source.ID
				break
			}
		}
		if configSourceID == "" || !slices.ContainsFunc(producer.Sources, func(edge ActionPlanSourceEdge) bool {
			return edge.SourceID == configSourceID
		}) {
			t.Fatalf("target sources = %#v, want native source %s", producer.Sources, pathname)
		}
		if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), pathname) {
			t.Fatalf("target working inputs = %#v, want native prerequisite %q", recipe.WorkingInputs, pathname)
		}
		if _, _, exists := planProducerByOutput(plan, "prep", pathname); exists {
			t.Fatalf("immutable native prerequisite %s has a redundant producer", pathname)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("native config-prerequisite plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanUsesReachedGroupedTriggerAndPeerDependencyUnion(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:grouped-trigger", "scripts/Makefile.build", "", `
EMIT = /selected/emitter
cmd_dependency = $(EMIT) -o $@ $<
peer-only.generated: peer-only.in FORCE
	$(call if_changed,dependency)
cmd_group = $(EMIT) --target $@ --first $< --all $^ -o $@
a-peer z-trigger &: common.in FORCE
	$(call if_changed,group)
a-peer: peer-only.generated peer-only.source
`, map[string]string{"EMIT": KbuildActionRoleToken("target", "emit")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "common.in", "peer-only.in", "peer-only.source")
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "peer-only.generated", MakeTarget: "peer-only.generated", Lifecycle: "target", Scope: "target", Stage: "target"},
			// z-trigger is deliberately lexically later than a-peer. The parser's
			// first-reached identity, not target sorting, owns GNU automatic vars.
			{Profile: profile.Name, Target: "a-peer", MakeTarget: "a-peer", GroupedTrigger: "z-trigger", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "z-trigger", MakeTarget: "z-trigger", GroupedTrigger: "z-trigger", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testScopedActionRoles("emit"),
		Config:         config,
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}

	dependencyID, _, ok := planProducerByOutput(plan, "objects", "peer-only.generated")
	if !ok {
		t.Fatalf("plan omits peer-only generated dependency: %#v", plan.Nodes)
	}
	triggerKey := compactKbuildSelectionKey{profile: profile.Name, target: "z-trigger", stage: "target"}
	peerKey := compactKbuildSelectionKey{profile: profile.Name, target: "a-peer", stage: "target"}
	groupID := graph.materializedProducers[triggerKey]
	if groupID == "" || graph.materializedProducers[peerKey] != groupID {
		t.Fatalf("grouped producers = trigger %q, peer %q", groupID, graph.materializedProducers[peerKey])
	}
	groupNode, ok := compactKbuildPlanNode(plan, groupID)
	if !ok {
		t.Fatalf("missing grouped trigger node %q", groupID)
	}
	dependencyEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, groupNode, "peer-only.generated")
	wantDependencyEntry := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "peer-only.generated"},
		ProducerID: dependencyID,
	}
	if !found || dependencyEntry != wantDependencyEntry {
		t.Fatalf("grouped trigger persistent dependency = %#v, %t; want %#v", dependencyEntry, found, wantDependencyEntry)
	}
	groupRecipe := plan.Recipes[groupNode.Recipe]
	arguments := strings.Join(groupRecipe.Arguments, " ")
	if !strings.Contains(arguments, "--target z-trigger") {
		t.Fatalf("grouped trigger arguments = %q, want reached target z-trigger", arguments)
	}
	if strings.Contains(arguments, "peer-only.generated") {
		t.Fatalf("grouped trigger automatic prerequisite arguments include peer-only dependency: %q", arguments)
	}
	if strings.Contains(arguments, "peer-only.source") {
		t.Fatalf("grouped trigger automatic prerequisite arguments include peer-only source: %q", arguments)
	}
	peerSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "kernel" && source.Path == "peer-only.source" {
			peerSourceID = source.ID
			break
		}
	}
	if peerSourceID == "" {
		t.Fatalf("plan sources omit kernel/peer-only.source: %#v", plan.Sources)
	}
	peerSourceEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, groupNode, "peer-only.source")
	wantPeerSourceEntry := ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "peer-only.source"},
		SourceID: peerSourceID,
	}
	if !found || peerSourceEntry != wantPeerSourceEntry {
		t.Fatalf("grouped trigger persistent source = %#v, %t; want %#v", peerSourceEntry, found, wantPeerSourceEntry)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatalf("finalize grouped-trigger action plan: %v", err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("grouped-trigger action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanLowersTargetBootstrapDependencyIntoHostClosure(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:cross-scope", "scripts/Makefile.build", "", `
cmd_target_input = $(CC) -c -o $@ $<
cmd_header = cp $< $@
cmd_host_generator = $(HOSTCC) -o $@ $^
target-input.o: target-input.c FORCE
	$(call if_changed,target_input)
generated/header.h: target-input.o FORCE
	$(call if_changed,header)
host-generator: host-generator.c generated/header.h FORCE
	$(call if_changed,host_generator)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "host-generator.c", "target-input.c")
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: "target-input.o", MakeTarget: "target-input.o", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
				{Profile: profile.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "host", Stage: "host"},
				{Profile: profile.Name, Target: "host-generator", MakeTarget: "host-generator", Lifecycle: "target", Scope: "host", Stage: "host"},
			},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	bootstrapID, _, ok := planProducerByOutput(plan, "bootstrap", "target-input.o")
	if !ok {
		t.Fatalf("plan omits target bootstrap producer: %#v", plan.Nodes)
	}
	projectedID, _, ok := planProducerByOutput(plan, "objects", "target-input.o")
	if !ok || projectedID == bootstrapID {
		t.Fatalf("plan omits canonical target projection for bootstrap output: %#v", plan.Nodes)
	}
	projected, _ := compactKbuildPlanNode(plan, projectedID)
	if projected.Stage != "target" || projected.Product != "vmlinux" || !slices.ContainsFunc(projected.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == bootstrapID
	}) {
		t.Fatalf("bootstrap target projection = %#v, want target/vmlinux copy from %s", projected, bootstrapID)
	}
	headerID, _, ok := planProducerByOutput(plan, "host", "generated/header.h")
	if !ok {
		t.Fatalf("plan omits host-propagated neutral producer: %#v", plan.Nodes)
	}
	hostID, _, ok := planProducerByOutput(plan, "host", "host-generator")
	if !ok {
		t.Fatalf("plan omits host generator: %#v", plan.Nodes)
	}
	header, _ := compactKbuildPlanNode(plan, headerID)
	if !slices.ContainsFunc(header.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == bootstrapID }) {
		t.Fatalf("host header inputs = %#v, want bootstrap producer %s", header.Inputs, bootstrapID)
	}
	if slices.ContainsFunc(header.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == projectedID }) {
		t.Fatalf("host header inputs = %#v, must not depend backward through target projection %s", header.Inputs, projectedID)
	}
	host, _ := compactKbuildPlanNode(plan, hostID)
	if !slices.ContainsFunc(host.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == headerID }) {
		t.Fatalf("host generator inputs = %#v, want header producer %s", host.Inputs, headerID)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("cross-stage action plan is invalid: %v", err)
	}
}

func TestAppendReferencedPlanTreesIncludesRecursiveReplayBindings(t *testing.T) {
	plan := &ActionPlan{}
	node := ActionPlanNode{}
	recipe := ActionRecipe{CommandReplays: []ActionRecipeCommandReplay{{
		Name: "make",
		Invocations: []ActionRecipeCommandReplayInvocation{{
			Arguments: []string{"-f", "${tree:kernel}/scripts/Makefile.build"},
		}},
	}}}
	if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
		t.Fatal(err)
	}
	if got, want := node.Trees, []string{"kernel"}; !slices.Equal(got, want) {
		t.Fatalf("replay tree bindings = %q, want %q", got, want)
	}
	if !slices.Equal(recipe.Trees, node.Trees) {
		t.Fatalf("recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
	}
}

func TestAppendReferencedPlanTreesClosesWorkingTreesWithoutPlaceholders(t *testing.T) {
	plan := &ActionPlan{}
	node := ActionPlanNode{}
	recipe := ActionRecipe{WorkingTrees: []string{"vendor-overlay"}}
	if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
		t.Fatal(err)
	}
	if got, want := node.Trees, []string{"vendor-overlay"}; !slices.Equal(got, want) {
		t.Fatalf("working tree closure = %q, want %q", got, want)
	}
	if !slices.Equal(recipe.Trees, node.Trees) {
		t.Fatalf("recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
	}
}

func TestAppendReferencedPlanTreesClosesOnlyCommandMetadataSourceNamespaces(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{
		"compile-recipe": {WorkingTrees: []string{"external"}},
	}}
	sourceID, err := ensureActionPlanSource(plan, "external", ".linux-bzl/external/demo/demo.c")
	if err != nil {
		t.Fatal(err)
	}
	vendorSourceID, err := ensureActionPlanSource(plan, "vendor", "drivers/vendor/immutable.c")
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes = []ActionPlanNode{
		{
			ID:      "compile",
			Recipe:  "compile-recipe",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: sourceID}},
			Trees:   []string{"external", "prep"},
			Outputs: []ActionPlanOutput{{
				Tree: "metadata", Path: ".captures/demo.state",
				ObservedPath: ".linux-bzl/external/demo/.demo.o.cmd",
			}},
		},
		{
			ID: "resolver",
			Inputs: []ActionPlanNodeEdge{{
				Role: "state", ProducerID: "compile", Slot: 0,
			}},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/.demo.o.cmd",
			}},
		},
		{
			ID:      "immutable-command-metadata",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: sourceID}},
			Trees:   []string{"external"},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/.immutable.o.cmd",
			}},
		},
		{
			ID:      "vendor-command-metadata",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: vendorSourceID}},
			Trees:   []string{"vendor"},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: "drivers/vendor/.immutable.o.cmd",
			}},
		},
		{
			ID: "ordinary",
			Inputs: []ActionPlanNodeEdge{{
				Role: "state", ProducerID: "compile", Slot: 0,
			}},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/demo.o",
			}},
		},
	}
	for _, test := range []struct {
		name     string
		producer string
		want     []string
		working  []string
	}{
		{name: "relative generated command metadata", producer: "resolver", want: []string{"external"}, working: []string{"external"}},
		{name: "immutable command metadata", producer: "immutable-command-metadata", want: []string{"external"}},
		{name: "ordinary generated output", producer: "ordinary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := ActionPlanNode{Inputs: []ActionPlanNodeEdge{{
				Role: "prerequisite", ProducerID: test.producer, Slot: 0,
			}}}
			recipe := ActionRecipe{}
			if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(node.Trees, test.want) || !slices.Equal(recipe.Trees, test.want) {
				t.Fatalf("source metadata closure = node %q recipe %q, want %q", node.Trees, recipe.Trees, test.want)
			}
			if !slices.Equal(recipe.WorkingTrees, test.working) {
				t.Fatalf("source metadata working trees = %q, want %q", recipe.WorkingTrees, test.working)
			}
		})
	}

	t.Run("multiple command metadata roots union namespaces", func(t *testing.T) {
		node := ActionPlanNode{Inputs: []ActionPlanNodeEdge{
			{Role: "prerequisite", ProducerID: "resolver", Slot: 0},
			{Role: "prerequisite", ProducerID: "vendor-command-metadata", Slot: 0},
			{Role: "duplicate", ProducerID: "resolver", Slot: 0},
		}}
		recipe := ActionRecipe{}
		if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
			t.Fatal(err)
		}
		if got, want := node.Trees, []string{"external", "vendor"}; !slices.Equal(got, want) {
			t.Fatalf("multiple metadata roots node trees = %q, want %q", got, want)
		}
		if !slices.Equal(recipe.Trees, node.Trees) {
			t.Fatalf("multiple metadata roots recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
		}
		if got, want := recipe.WorkingTrees, []string{"external"}; !slices.Equal(got, want) {
			t.Fatalf("multiple metadata roots working trees = %q, want %q", got, want)
		}
	})
}

func TestCommandMetadataSourceClosureFollowsPersistentInputs(t *testing.T) {
	for _, persistentSource := range []bool{false, true} {
		for _, persistentConsumer := range []bool{false, true} {
			t.Run(fmt.Sprintf("source=%t/consumer=%t", persistentSource, persistentConsumer), func(t *testing.T) {
				plan := &ActionPlan{Recipes: map[string]ActionRecipe{
					"compile": {WorkingTrees: []string{"external"}},
				}}
				sourceID, err := ensureActionPlanSource(plan, "external", "module/hello.c")
				if err != nil {
					t.Fatal(err)
				}
				store, err := plan.planningActionPlanInputSetStore()
				if err != nil {
					t.Fatal(err)
				}
				producer := ActionPlanNode{
					ID: "compile", Recipe: "compile", Trees: []string{"external", "prep"},
					Outputs: []ActionPlanOutput{
						{Tree: "metadata", Path: ".captures/source-state", ObservedPath: "module/.hello.o.cmd"},
						{Tree: "objects", Path: "module/hello.o"},
					},
				}
				if persistentSource {
					producer.InputSet, err = store.Insert("", ActionPlanInputSetEntry{
						Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "module/hello.c"}, SourceID: sourceID,
					})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					producer.Sources = []ActionPlanSourceEdge{{Role: "source", SourceID: sourceID}}
				}
				plan.Nodes = []ActionPlanNode{producer}
				for _, slot := range []int{0, 1} {
					node := ActionPlanNode{}
					if persistentConsumer {
						node.InputSet, err = store.Insert("", ActionPlanInputSetEntry{
							Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "consumed-state"}, ProducerID: "compile", Slot: slot,
						})
						if err != nil {
							t.Fatal(err)
						}
					} else {
						node.Inputs = []ActionPlanNodeEdge{{Role: "state", ProducerID: "compile", Slot: slot}}
					}
					recipe := ActionRecipe{}
					if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
						t.Fatal(err)
					}
					var want []string
					if slot == 0 {
						want = []string{"external"}
					}
					if !slices.Equal(node.Trees, want) || !slices.Equal(recipe.Trees, want) || !slices.Equal(recipe.WorkingTrees, want) {
						t.Fatalf("slot %d lost source closure: node=%v recipe=%v working=%v, want=%v", slot, node.Trees, recipe.Trees, recipe.WorkingTrees, want)
					}
				}
			})
		}
	}
}

func TestCommandMetadataInputQuerySharesSummariesAndInvalidatesBindings(t *testing.T) {
	producerID := strings.Repeat("a", 64)
	plan := &ActionPlan{Nodes: []ActionPlanNode{{ID: producerID, Outputs: []ActionPlanOutput{
		{Tree: "objects", Path: ".hello.o.cmd"}, {Tree: "objects", Path: "hello.o"},
	}}}}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	for i := range 512 {
		slot := 1
		if i == 100 {
			slot = 0
		}
		root, err = store.Insert(root, ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("input-%04d", i)},
			ProducerID: producerID, Slot: slot,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(want int) {
		t.Helper()
		count := 0
		if err := plan.walkCommandMetadataInputs(root, commandMetadataGeneratedInput, func(entry ActionPlanInputSetEntry) error {
			count++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("metadata entries = %d, want %d", count, want)
		}
	}
	check(1)
	query := plan.commandMetadataInputQuery
	count := len(query.summaries)
	if count < 2 {
		t.Fatal("fixture did not exercise a branched trie")
	}
	check(1)
	if query != plan.commandMetadataInputQuery || len(query.summaries) != count {
		t.Fatal("unchanged query did not reuse its subtree summaries")
	}
	// Changing output provenance requires the same explicit invalidation as
	// every other public node-index mutation; staging paths remain unchanged.
	plan.Nodes[0].Outputs[0].Path = "ordinary-output"
	plan.invalidateLookupIndexes()
	check(0)
	if plan.commandMetadataInputQuery == query {
		t.Fatal("node-index invalidation retained output classification")
	}
	query = plan.commandMetadataInputQuery
	plan.inputSetStore = nil
	plan.InputSets, err = store.ReachableNodesForRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	check(0)
	if plan.commandMetadataInputQuery == query {
		t.Fatal("store rebinding retained a query into the old store")
	}
	query = plan.commandMetadataInputQuery
	for i := len(query.summaries); i < commandMetadataInputSummaryLimit; i++ {
		query.summaries[fmt.Sprintf("unused-%d", i)] = 0
	}
	check(0)
	if plan.commandMetadataInputQuery == query || len(plan.commandMetadataInputQuery.summaries) >= commandMetadataInputSummaryLimit {
		t.Fatal("full summary cache was not bounded/replaced")
	}
}

func TestCommandMetadataPersistentInputsRejectInvalidProvenance(t *testing.T) {
	for _, test := range []struct {
		name     string
		producer string
		slot     int
		message  string
	}{
		{"missing producer", "absent", 0, "absent producer"},
		{"missing slot", "compile", 1, "slot 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &ActionPlan{Nodes: []ActionPlanNode{{ID: "compile", Outputs: []ActionPlanOutput{{Tree: "objects", Path: "hello.o"}}}}}
			store, err := plan.planningActionPlanInputSetStore()
			if err != nil {
				t.Fatal(err)
			}
			root, err := store.Insert("", ActionPlanInputSetEntry{
				Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "ordinary-input"},
				ProducerID: test.producer, Slot: test.slot,
			})
			if err != nil {
				t.Fatal(err)
			}
			node := ActionPlanNode{InputSet: root}
			err = appendReferencedPlanTrees(plan, &node, &ActionRecipe{})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("invalid provenance error = %v, want %s", err, test.message)
			}
		})
	}
}

func TestAppendReferencedPlanTreesClosesKernelSourceRelativeIncludes(t *testing.T) {
	plan := &ActionPlan{}
	kernelID, err := ensureActionPlanSource(plan, "kernel", "arch/x86/boot/mkcpustr.c")
	if err != nil {
		t.Fatal(err)
	}
	configID, err := ensureActionPlanSource(plan, "config", "include/generated/autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		id    string
		trees []string
	}{
		{name: "kernel source", id: kernelID, trees: []string{"kernel"}},
		{name: "source-backed config projection", id: configID},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := ActionPlanNode{Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: test.id}}}
			recipe := ActionRecipe{}
			if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(node.Trees, test.trees) || !slices.Equal(recipe.Trees, test.trees) {
				t.Fatalf("source closure trees = node %q recipe %q, want %q", node.Trees, recipe.Trees, test.trees)
			}
		})
	}
}

func TestActionPlanSourceNamespaceUsesSelectedExternalRoot(t *testing.T) {
	metadata := &CompactMetadata{
		sourceNamespaces: map[string]string{
			"external/rust-src":                                  "toolchain",
			"external/rust-src/library":                          "rust",
			"__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/demo": "external",
		}}
	const source = "external/rust-src/library/core/src/lib.rs"
	if got, err := metadata.actionPlanSourceNamespace(source); err != nil || got != "rust" {
		t.Fatalf("actionPlanSourceNamespace(%q) = %q, %v; want rust", source, got, err)
	}
	plan := &ActionPlan{}
	id, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0] != (ActionPlanSource{ID: id, Namespace: "rust", Path: source}) {
		t.Fatalf("plan sources = %#v", plan.Sources)
	}
	if got, err := metadata.actionPlanSourceNamespace("rust/kernel/lib.rs"); err != nil || got != "kernel" {
		t.Fatalf("kernel namespace = %q, %v", got, err)
	}
	const externalSource = ".linux-bzl/external/demo/source.c"
	if got, err := metadata.actionPlanSourceNamespace(externalSource); err != nil || got != "external" {
		t.Fatalf("external namespace for %q = %q, %v", externalSource, got, err)
	}
}

func TestActionPlanSourceNamespaceRejectsInvalidSelectedRoot(t *testing.T) {
	for _, prefix := range []string{
		"../rust",
		"/external/rust",
		"external/../rust",
		"external//rust",
		" external/rust",
		"external/rust ",
		"__LINUX_BZL_SOURCE_TREE__",
		"__LINUX_BZL_SOURCE_TREE__/external/../rust",
		"__LINUX_BZL_OBJECT_TREE__/include/generated",
		"__LINUX_BZL_HOST_DEPS__/tools",
		"__LINUX_BZL_SOURCE_TREE__/__LINUX_BZL_OBJECT_TREE__/nested",
	} {
		t.Run(strings.ReplaceAll(prefix, "/", "_"), func(t *testing.T) {
			metadata := &CompactMetadata{sourceNamespaces: map[string]string{prefix: "rust"}}
			if _, err := metadata.actionPlanSourceNamespace("external/rust/core.rs"); err == nil {
				t.Fatalf("actionPlanSourceNamespace() accepted invalid selected root %q", prefix)
			}
		})
	}
}

func TestActionPlanSourceNamespaceRejectsAmbiguousNormalizedRoots(t *testing.T) {
	metadata := &CompactMetadata{
		sourceNamespaces: map[string]string{
			".linux-bzl/external/demo":                           "first",
			"__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/demo": "second",
		}}
	if _, err := metadata.actionPlanSourceNamespace(".linux-bzl/external/demo/source.c"); err == nil || !strings.Contains(err.Error(), "ambiguous namespaces") {
		t.Fatalf("actionPlanSourceNamespace() ambiguity error = %v", err)
	}
}

func TestActionPlanExactSourceNamespaceDoesNotOwnDescendants(t *testing.T) {
	metadata := &CompactMetadata{
		sourceNamespaces:      map[string]string{"foo/bar": "external"},
		exactSourceNamespaces: map[string]string{"foo": "prep", "foo/bar": "prep"},
	}
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "foo", want: "prep"},
		{path: "foo/bar", want: "prep"},
		{path: "foo/bar/source.c", want: "external"},
		{path: "foo/generated.h", want: "kernel"},
	} {
		if got, err := metadata.actionPlanSourceNamespace(test.path); err != nil || got != test.want {
			t.Fatalf("actionPlanSourceNamespace(%q) = %q, %v; want %q", test.path, got, err, test.want)
		}
	}
}

func TestActionPlanExactSourceNamespaceRejectsTreeMarkerSyntax(t *testing.T) {
	metadata := &CompactMetadata{
		exactSourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/include/generated/sdk.h": "prep",
		}}
	if _, err := metadata.actionPlanSourceNamespace("include/generated/sdk.h"); err == nil {
		t.Fatal("exact source namespace accepted reserved source-tree marker syntax")
	}
}

func TestGeneratedActionPlanDoesNotMaterializePatternDeclarations(t *testing.T) {
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{{
				Name:         "prep:arch/arm64/kernel/%.lds.S",
				Directory:    "arch/arm64/kernel",
				EntryTargets: []string{"arch/arm64/kernel/%.lds.S"},
			}},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if strings.Contains(output.Path, "%") {
				t.Fatalf("pattern declaration became an action output: %#v", node)
			}
		}
	}
}

func TestGeneratedActionPlanDoesNotInventPhonyArtifacts(t *testing.T) {
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{{
				Name:         "prep:remove-stale-files",
				EntryTargets: []string{"remove-stale-files"},
				Rules: []KbuildRule{
					{Targets: []string{".PHONY"}, Prerequisites: []string{"remove-stale-files"}},
					{Targets: []string{"remove-stale-files"}, Recipe: []string{"scripts/remove-stale-files"}},
				},
			}},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Path == "remove-stale-files" {
				t.Fatalf("phony control-flow node became an action output: %#v", node)
			}
		}
	}
}

func TestGeneratedActionPlanExecutesSelectedPhonyModulesCheckAfterOrder(t *testing.T) {
	const (
		check  = "modules_check"
		order  = "modules.order"
		script = "scripts/modules-check.sh"
	)
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "scripts/module-list", "drivers/first/demo.o\ndrivers/second/demo.o\n")
	mustWriteSource(t, sourceRoot, script, `#!/bin/sh
set -e
duplicates=$(sed 's:.*/::' "$1" | sort | uniq -d)
if [ -n "$duplicates" ]; then
	echo "error: duplicate module name" >&2
	exit 1
fi
`)
	profile := mustCompactKbuildProfileForTest(t, "build:modules", "Makefile", "", `
CONFIG_SHELL := sh
srctree := __LINUX_BZL_SOURCE_TREE__
PHONY += modules modules_check
modules: modules_check
modules_check: modules.order
	$(Q)$(CONFIG_SHELL) $(srctree)/scripts/modules-check.sh $<
modules.order: scripts/module-list FORCE
	cat $< > $@
.PHONY: $(PHONY)
`, nil)
	if profile.evaluator == nil || profile.evaluator.template == nil {
		t.Fatal("captured Make evaluator is absent")
	}
	profile.evaluator.template.sourceRoots = maps.Clone(profile.evaluator.template.sourceRoots)
	if profile.evaluator.template.sourceRoots == nil {
		profile.evaluator.template.sourceRoots = map[string]string{}
	}
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = sourceRoot
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	selections := []CompactKbuildSelection{}
	for _, target := range []string{"modules", check, order} {
		selections = append(selections, CompactKbuildSelection{
			Profile: profile.Name, Target: target, MakeTarget: target,
			Lifecycle: "target", Scope: "target", Stage: "target",
		})
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile}, KbuildSelections: selections,
		},
		configFragment: map[string]string{},
		actionRoles:    testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{
		metadata: metadata, Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	checkKey := compactKbuildSelectionKey{profile: profile.Name, target: check, stage: "target"}
	producer := graph.materializedProducers[checkKey]
	if producer == "" {
		t.Fatalf("selected PHONY source check %s was omitted from the plan", compactKbuildSelectionKeyString(checkKey))
	}
	if _, materialized := graph.materializedProducers[compactKbuildSelectionKey{
		profile: profile.Name, target: "modules", stage: "target",
	}]; materialized {
		t.Fatal("recursive PHONY modules goal was materialized as a file")
	}
	if _, _, found := planProducerByOutput(plan, "objects", check); found {
		t.Fatal("selected modules_check fabricated a Make-visible object file")
	}
	checkNode, found := compactKbuildPlanNode(plan, producer)
	if !found || checkNode.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("modules_check node = %#v, found=%t; want a selected source script", checkNode, found)
	}
	recipe := plan.Recipes[checkNode.Recipe]
	if len(checkNode.Outputs) == 0 || checkNode.Outputs[0].ObservedPath != check ||
		recipe.ObservedOutputs[planOrdinal(0)] != check || recipe.WorkingOutputs[planOrdinal(0)] != "" ||
		recipe.RequireAbsentObservedOutput != planOrdinal(0) || !recipe.RequireUnchangedWorkingTree {
		t.Fatalf("modules_check lost its private execution-only completion: node=%#v recipe=%#v", checkNode, recipe)
	}
	orderProducer, _, found := planProducerByOutput(plan, "modules", order)
	if !found {
		t.Fatalf("selected modules.order lacks a physical producer: %#v", plan.Nodes)
	}
	nativeOrder := false
	for _, input := range checkNode.Inputs {
		if input.ProducerID == orderProducer && input.Role != compactKbuildWorkingClosureInputRole {
			nativeOrder = true
		}
	}
	if !nativeOrder {
		t.Fatalf("modules_check lost its selected modules.order native predecessor %s: %#v", orderProducer, checkNode.Inputs)
	}
	if !slices.Contains(plan.executionCheckRoots, producer) {
		t.Fatalf("modules_check completion %s is not a demanded execution validation root: %#v", producer, plan.executionCheckRoots)
	}
	// The production planner exports the persistent staged-input closure
	// after generated action lowering and before serializing the plan.
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatalf("export modules_check and modules.order input sets: %v", err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("selected PHONY check plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanRejectsUnsupportedPhonySourceScriptChecks(t *testing.T) {
	for _, test := range []struct {
		name, command, wantError string
		scriptBody               string
	}{
		{
			name:       "physical-output",
			command:    "$(CONFIG_SHELL) $< $@",
			wantError:  "produced a file or lost its authenticated outputless execution state",
			scriptBody: "#!/bin/sh\nset -e\nprintf 'physical\\n' > \"$1\"\n",
		},
		{
			name:      "wrapped-source-script",
			command:   "set -e; $(CONFIG_SHELL) $<",
			wantError: "cannot authenticate selected PHONY source script",
		},
		{
			name:      "multiline-source-script",
			command:   "true\n\t$(CONFIG_SHELL) $<",
			wantError: "cannot authenticate selected PHONY source script",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:check", "Makefile", "", `
CONFIG_SHELL := sh
PHONY += check
check: scripts/check.sh FORCE
	`+test.command+`
.PHONY: $(PHONY)
`, nil)
			sourceRoot := t.TempDir()
			scriptBody := test.scriptBody
			if scriptBody == "" {
				scriptBody = "#!/bin/sh\nset -e\nexit 0\n"
			}
			mustWriteSource(t, sourceRoot, "scripts/check.sh", scriptBody)
			profile.evaluator.template.sourceRoots = maps.Clone(profile.evaluator.template.sourceRoots)
			if profile.evaluator.template.sourceRoots == nil {
				profile.evaluator.template.sourceRoots = map[string]string{}
			}
			profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = sourceRoot
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				Config: CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{profile},
					KbuildSelections: []CompactKbuildSelection{{
						Profile: profile.Name, Target: "check", MakeTarget: "check",
						Lifecycle: "target", Scope: "target", Stage: "target",
					}},
				},
				configFragment: map[string]string{},
				actionRoles:    testConfiguredScopedActionRoles,
			}
			plan := &ActionPlan{
				metadata: metadata, Recipes: map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
			}
			if _, err := metadata.appendGeneratedActionPlan(plan); err == nil ||
				!strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("selected PHONY %s source script lacked a safe execution check: %v", test.name, err)
			}
			if len(plan.executionCheckRoots) != 0 {
				t.Fatalf("unsupported PHONY source script registered a check completion root: %#v", plan.executionCheckRoots)
			}
		})
	}
}

func TestGeneratedActionPlanExecutesSelectedMultilinePhonySourceSetup(t *testing.T) {
	const target = "outputmakefile"
	profile, sourceRoot, _ := selectedControlTestProfile(t, `
srctree := __LINUX_BZL_SOURCE_TREE__
objtree := __LINUX_BZL_OBJECT_TREE__
abs_srctree := $(srctree)
SRCARCH := x86
CONFIG_SHELL := sh
Q := @
PHONY += outputmakefile
outputmakefile:
	$(Q)if [ -f $(srctree)/.config -o \
		-d $(srctree)/include/config -o \
		-d $(srctree)/arch/$(SRCARCH)/include/generated ]; then \
		echo >&2 "***"; \
		echo >&2 "*** The source tree is not clean, please run 'make$(if $(findstring command line, $(origin ARCH)), ARCH=$(ARCH)) mrproper' $(if $(wildcard $(objtree)/read-marker),yes,no)"; \
		echo >&2 "*** in $(abs_srctree)"; \
		echo >&2 "***"; \
		false; \
	fi
	$(Q)ln -fsn $(srctree) source
	$(Q)$(CONFIG_SHELL) $(srctree)/scripts/mkmakefile $(srctree)
	$(Q)test -e .gitignore || \
	{ echo "# this is build directory, ignore it"; echo "*"; } > .gitignore
.PHONY: $(PHONY)
`)
	if err := os.RemoveAll(filepath.Join(sourceRoot, "include", "config")); err != nil {
		t.Fatal(err)
	}
	mustWriteSource(t, sourceRoot, "scripts/mkmakefile", `#!/bin/sh
if [ "${quiet}" != "silent_" ]; then
	echo "  GEN     Makefile"
fi
cat << EOF > Makefile
# Automatically generated by $0: don't edit
include $1/Makefile
EOF
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget(target, target, ""); err != nil {
		t.Fatal(err)
	}
	ruleIndex := selectedControlTestRuleIndex(t, profile, target)
	frontier := selectedControlTestFrontier("before-out-of-tree-setup", selectedControlTestFiles{}, KbuildControlReadArtifact{})
	for index := range profile.Rules[ruleIndex].Recipe {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: target, RuleIndex: ruleIndex, RecipeIndex: index,
		}, frontier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := evaluateCompactKbuildTextForMakeTarget(
			line.Evaluation.Profile, target, line.Line.LookupTarget,
			line.Line.AutomaticTarget, line.Line.Stem,
			line.Line.Normal, line.Line.OrderOnly, nil,
			profile.Rules[ruleIndex].Recipe[index], true,
		); err != nil {
			t.Fatalf("selected PHONY recipe %d Make expansion: %v", index, err)
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := stepper.Finish(frontier)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := CompactKbuildSelectedControlRecipeSnapshots(evaluation.Profile, target)
	if len(snapshots) != 4 || snapshots[0].ReadIdentity() == "" ||
		snapshots[0].ReadIdentity() == snapshots[1].ReadIdentity() {
		t.Fatalf("selected PHONY line0 should have a distinct exact absent wildcard view: %#v", snapshots)
	}
	selection := CompactKbuildSelection{
		Profile: profile.Name, Target: target, MakeTarget: target,
		Lifecycle: "prep", Scope: "target", Stage: "prep",
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles:   []CompactKbuildProfile{evaluation.Profile},
			KbuildSelections: []CompactKbuildSelection{selection},
		},
		configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{metadata: metadata, Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	producer := graph.materializedProducers[compactKbuildSelectionKey{
		profile: profile.Name, target: target, stage: "prep",
	}]
	node, found := compactKbuildPlanNode(plan, producer)
	if !found || !slices.Contains(plan.executionCheckRoots, producer) {
		t.Fatalf("selected setup completion = %#v, found=%t roots=%#v", node, found, plan.executionCheckRoots)
	}
	if _, _, found := planProducerByOutput(plan, "prep", target); found {
		t.Fatal("PHONY outputmakefile was incorrectly published as a physical Make file")
	}
	if _, _, found := planProducerByOutput(plan, "prep", "Makefile"); found {
		t.Fatalf("private source-root Makefile was published with an action-specific absolute include: %#v", node.Outputs)
	}
	if len(plan.Recipes[node.Recipe].PrivateWorkingEffects) != 3 {
		t.Fatalf("selected private source setup has unbounded effects: %#v", plan.Recipes[node.Recipe].PrivateWorkingEffects)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedActionPlanExecutesSelectedInlinePhonyPrivateSetup(t *testing.T) {
	const target = "prepare-output"
	profile, sourceRoot, _ := selectedControlTestProfile(t, `
srctree := __LINUX_BZL_SOURCE_TREE__
objtree := __LINUX_BZL_OBJECT_TREE__
abs_srctree := $(srctree)
SRCARCH := x86
CONFIG_SHELL := sh
Q := @
quiet := quiet_
quiet_cmd_makefile = GEN     Makefile
echo-cmd = $(if $($(quiet)cmd_$(1)),echo '  $($(quiet)cmd_$(1))';)
redirect :=
quiet_redirect :=
silent_redirect := exec >/dev/null;
delete-on-interrupt = \
	$(if $(filter-out $(PHONY), $@), \
		$(foreach sig, HUP INT QUIT TERM PIPE, \
			trap 'rm -f $@; trap - $(sig); kill -s $(sig) $$$$' $(sig);))
cmd = @set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))
cmd_makefile = { \
	echo "\# Automatically generated by $(srctree)/Makefile: don't edit"; \
	echo "include $(srctree)/Makefile"; \
	} > Makefile
PHONY += prepare-output
prepare-output:
	$(Q)if [ -f $(srctree)/.config -o \
		-d $(srctree)/include/config -o \
		-d $(srctree)/arch/$(SRCARCH)/include/generated ]; then \
		echo >&2 "***"; \
		echo >&2 "*** The source tree is not clean, please run 'make mrproper'"; \
		echo >&2 "*** in $(abs_srctree)"; \
		echo >&2 "***"; \
		false; \
	fi
	$(Q)ln -fsn $(srctree) source
	$(call cmd,makefile)
	$(Q)test -e .gitignore || \
	{ echo "# this is build directory, ignore it"; echo "*"; } > .gitignore
.PHONY: $(PHONY)
`)
	if err := os.RemoveAll(filepath.Join(sourceRoot, "include", "config")); err != nil {
		t.Fatal(err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget(target, target, ""); err != nil {
		t.Fatal(err)
	}
	ruleIndex := selectedControlTestRuleIndex(t, profile, target)
	frontier := selectedControlTestFrontier("before-inline-private-setup", selectedControlTestFiles{}, KbuildControlReadArtifact{})
	for index := range profile.Rules[ruleIndex].Recipe {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: target, RuleIndex: ruleIndex, RecipeIndex: index,
		}, frontier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := evaluateCompactKbuildTextForMakeTarget(
			line.Evaluation.Profile, target, line.Line.LookupTarget,
			line.Line.AutomaticTarget, line.Line.Stem,
			line.Line.Normal, line.Line.OrderOnly, nil,
			profile.Rules[ruleIndex].Recipe[index], true,
		); err != nil {
			t.Fatalf("selected inline setup recipe %d Make expansion: %v", index, err)
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := stepper.Finish(frontier)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{evaluation.Profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "prep", Scope: "target", Stage: "prep",
			}},
		},
		configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{metadata: metadata, Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	producer := graph.materializedProducers[compactKbuildSelectionKey{
		profile: profile.Name, target: target, stage: "prep",
	}]
	node, found := compactKbuildPlanNode(plan, producer)
	if !found || !slices.Contains(plan.executionCheckRoots, producer) ||
		!compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, target) {
		t.Fatalf("inline setup has no authenticated execution-only completion: %#v", node)
	}
	if _, _, found := planProducerByOutput(plan, "prep", target); found {
		t.Fatal("inline PHONY setup published a file named by its control target")
	}
	recipe := plan.Recipes[node.Recipe]
	if receipt := recipe.MakePhonyCompletion; receipt == nil ||
		len(receipt.ExpandedLines) != 4 || len(recipe.PrivateWorkingEffects) != 3 {
		t.Fatalf("inline setup lost its selected source lines or private effects: %#v", recipe)
	} else if strings.Contains(receipt.ExpandedLines[2], "trap '") ||
		!strings.Contains(receipt.ExpandedLines[2], "set -e;") {
		t.Fatalf("PHONY setup command included a non-PHONY deletion trap or lost its source wrapper: %q", receipt.ExpandedLines[2])
	}
	for _, test := range []struct {
		name   string
		change func(*ActionRecipe)
	}{
		{"changed source line", func(candidate *ActionRecipe) {
			candidate.MakePhonyCompletion.ExpandedLines[2] += "; touch unexpected"
		}},
		{"changed private effect", func(candidate *ActionRecipe) {
			candidate.PrivateWorkingEffects[1].Path = "unexpected"
		}},
		{"changed script", func(candidate *ActionRecipe) {
			candidate.Arguments = append(candidate.Arguments, "-script_content_base64", "Zm9yZ2Vk")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			altered := cloneActionRecipe(recipe)
			test.change(&altered)
			if err := validateActionRecipeMakePhonyCompletion(altered); err == nil {
				t.Fatal("altered PHONY source line, effect, or script passed receipt validation")
			}
		})
	}
	altered := *plan
	altered.Sources = slices.Clone(plan.Sources)
	for index := range altered.Sources {
		if altered.Sources[index].Namespace == "kernel" && altered.Sources[index].Path == profile.Path {
			altered.Sources[index].Path = "different/Makefile"
		}
	}
	if compactKbuildMakePhonyCompletion(&altered, node, target) {
		t.Fatal("PHONY private completion accepted a different bound Makefile source")
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatal(err)
	}
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, selected := range plan.Nodes {
		dependencies[selected.ID] = ConfigDependencySet{Opaque: true, Reason: "selected PHONY source setup"}
	}
	checkpoint := filepath.Join(t.TempDir(), "inline-phony.snapshot.json.gz")
	if err := WriteActionPlanSnapshot(checkpoint, plan, dependencies, familyTestConfig("y", "n")); err != nil {
		t.Fatalf("write PHONY private checkpoint: %v", err)
	}
	restored, err := ReadActionPlanSnapshot(checkpoint)
	if err != nil {
		t.Fatalf("read PHONY private checkpoint: %v", err)
	}
	if got := snapshotActionPlan(restored); !compactKbuildMakePhonyCompletion(got, node, target) {
		t.Fatal("PHONY private completion lost its source receipt across checkpoint")
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "selected", Snapshot: restored}})
	if err != nil {
		t.Fatalf("PHONY private completion family: %v", err)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, nil)
	if err != nil {
		t.Fatalf("PHONY private completion execution cut: %v", err)
	}
	if _, err := cut.Verify(family); err != nil {
		t.Fatalf("PHONY private execution cut verification: %v", err)
	}
}

func TestGeneratedActionPlanBuildsPrerequisiteWithItsSelectedOwningProfile(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "input.c", "int selected_owner;\n")
	parent := mustCompactKbuildProfileForTest(t, "a-parent-profile", "scripts/Makefile.parent", "", `
cmd_copy = cat $< > $@
cmd_poison = $(CC) -DPOISON_PROFILE -c -o $@ $<
a-parent.out: z-dependency.out FORCE
	$(call if_changed,copy)
z-dependency.out: input.c FORCE
	$(call if_changed,poison)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	owner := mustCompactKbuildProfileForTest(t, "z-owner-profile", "scripts/Makefile.owner", "", `
cmd_owner = $(CC) -DOWNING_PROFILE -c -o $@ $<
z-dependency.out: input.c FORCE
	$(call if_changed,owner)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	for _, profile := range []*CompactKbuildProfile{&parent, &owner} {
		if profile.evaluator == nil || profile.evaluator.template == nil {
			t.Fatalf("profile %q has no captured evaluator", profile.Name)
		}
		if profile.evaluator.template.sourceRoots == nil {
			profile.evaluator.template.sourceRoots = map[string]string{}
		}
		profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = root
	}
	configFragment := map[string]string{}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, owner},
		KbuildSelections: []CompactKbuildSelection{
			// This is intentionally lexical rather than dependency order. The old
			// target sort built a-parent.out first and selected the poison rule
			// from the parent's invocation for z-dependency.out.
			{Profile: parent.Name, Target: "a-parent.out", MakeTarget: "a-parent.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: owner.Name, Target: "z-dependency.out", MakeTarget: "z-dependency.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testConfiguredScopedActionRoles,
		Config:         config,
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}

	var dependencyNode, parentNode *ActionPlanNode
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		for _, output := range node.Outputs {
			switch output.Path {
			case "z-dependency.out":
				dependencyNode = node
			case "a-parent.out":
				parentNode = node
			}
		}
	}
	if dependencyNode == nil || parentNode == nil {
		t.Fatalf("selected producers missing: dependency=%#v parent=%#v", dependencyNode, parentNode)
	}
	dependencyArguments := strings.Join(plan.Recipes[dependencyNode.Recipe].Arguments, " ")
	if !strings.Contains(dependencyArguments, "-DOWNING_PROFILE") || strings.Contains(dependencyArguments, "POISON_PROFILE") {
		t.Fatalf("dependency arguments = %q, want exact owning profile", dependencyArguments)
	}
	for _, recipe := range plan.Recipes {
		if strings.Contains(strings.Join(recipe.Arguments, " "), "POISON_PROFILE") {
			t.Fatalf("parent invocation manufactured selected prerequisite: %#v", recipe)
		}
	}
	dependsOnOwner := false
	for _, input := range parentNode.Inputs {
		if input.ProducerID == dependencyNode.ID {
			dependsOnOwner = true
			break
		}
	}
	if !dependsOnOwner {
		t.Fatalf("parent inputs = %#v, want selected owner %s", parentNode.Inputs, dependencyNode.ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("selected-owner action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanBindsExactGeneratedArtifactAcrossUnrelatedSamePathProfiles(t *testing.T) {
	consumer := compactKbuildScriptProfileForTest(t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	firstWriter := mustCompactKbuildProfileForTest(t, "m-first-writer", "scripts/first.mk", "", `
CC = `+KbuildActionRoleToken("target", "cc")+`
cmd_emit = $(CC) -DFIRST_WRITER -c -o $@ $<
generated/shared.h: first.c FORCE
	$(call if_changed,emit)
`, nil)
	firstWriter = compactKbuildProfileWithSourcesForTest(t, firstWriter, "first.c")
	exactWriter := mustCompactKbuildProfileForTest(t, "z-exact-writer", "scripts/exact.mk", "", `
CC = `+KbuildActionRoleToken("target", "cc")+`
cmd_emit = $(CC) -DEXACT_WRITER -c -o $@ $<
generated/shared.h: exact.c FORCE
	$(call if_changed,emit)
`, nil)
	exactWriter = compactKbuildProfileWithSourcesForTest(t, exactWriter, "exact.c")
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.h", Profile: exactWriter.Name, Target: "generated/shared.h",
	}
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{consumer, firstWriter, exactWriter},
			KbuildSelections: []CompactKbuildSelection{
				{
					Profile: consumer.Name, Target: "generated/result.h", MakeTarget: "generated/result.h", Lifecycle: "target", Scope: "target", Stage: "target",
					GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
				},
				{Profile: firstWriter.Name, Target: artifact.Path, MakeTarget: artifact.Path, Lifecycle: "prep", Scope: "target", Stage: "prep"},
				{Profile: exactWriter.Name, Target: artifact.Path, MakeTarget: artifact.Path, Lifecycle: "target", Scope: "target", Stage: "target"},
			},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	firstID, _, firstFound := planProducerByOutput(plan, "prep", artifact.Path)
	exactID, _, exactFound := planProducerByOutput(plan, "objects", artifact.Path)
	consumerID, _, consumerFound := planProducerByOutput(plan, "objects", "generated/result.h")
	if !firstFound || !exactFound || !consumerFound {
		t.Fatalf("same-path plan producers = first (%q, %t), exact (%q, %t), consumer (%q, %t): %#v", firstID, firstFound, exactID, exactFound, consumerID, consumerFound, plan.Nodes)
	}
	if firstID == exactID {
		t.Fatalf("unrelated same-path selections share producer %q, want distinct physical-stage actions", firstID)
	}
	consumerNode, ok := compactKbuildPlanNode(plan, consumerID)
	if !ok {
		t.Fatalf("consumer producer %q is missing", consumerID)
	}
	artifactEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, consumerNode, artifact.Path)
	wantArtifactEntry := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: artifact.Path},
		ProducerID: exactID,
	}
	if !found || artifactEntry != wantArtifactEntry {
		t.Fatalf("consumer persistent generated artifact = %#v, %t; want %#v", artifactEntry, found, wantArtifactEntry)
	}
	consumerEntries := actionPlanNodeInputSetEntriesForTest(t, plan, consumerNode)
	for _, entry := range consumerEntries {
		if entry.ProducerID == firstID {
			t.Fatalf("consumer persistent inputs = %#v, unrelated same-path producer %q leaked into exact closure", consumerEntries, firstID)
		}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatalf("finalize exact generated-artifact action plan: %v", err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("exact generated-artifact action plan is invalid: %v", err)
	}
}
