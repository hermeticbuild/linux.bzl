package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestKbuildProbeWorkloadIncludesActionPlanOnlyProbe(t *testing.T) {
	const (
		target     = "generated.txt"
		scriptPath = "scripts/generate.sh"
	)
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	mustWriteSource(t, root, scriptPath, "#!/bin/sh\n: \"$ACTION_ONLY\"\n: > \"$1\"\n")
	if err := os.WriteFile(makefile, []byte(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
if_changed = $(cmd_$(1))

export ACTION_ONLY = $(call cc-option,-faction-only)
cmd_generate = $(srctree)/scripts/generate.sh $@

generated.txt: FORCE
	$(call if_changed,generate)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type workloadValue struct {
		plan             *ActionPlan
		referencesBefore int
		referencesAfter  int
	}
	workload := func(discoveryOnly bool) func(*KbuildProbeScopes) (workloadValue, error) {
		return func(scopes *KbuildProbeScopes) (workloadValue, error) {
			options, err := scopes.Options("target", KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"CC":      KbuildActionRoleToken("target", "cc"),
					"SRCARCH": "x86",
					"srctree": "__LINUX_BZL_SOURCE_TREE__",
				},
				SourceRoots: map[string]string{
					"__LINUX_BZL_SOURCE_TREE__": root,
					"__LINUX_BZL_OBJECT_TREE__": root,
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
				SkipExportedVariables:   true,
			})
			if err != nil {
				return workloadValue{}, err
			}
			parsed, err := ParseKbuildFileTree(makefile, options)
			if err != nil {
				return workloadValue{}, err
			}
			profile, err := NewCompactKbuildProfile("build:root", makefile, root, parsed)
			if err != nil {
				return workloadValue{}, err
			}
			profile.EntryTargets = []string{target}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				return workloadValue{}, err
			}
			actionRoles := append([]KbuildActionRoleRef(nil), testConfiguredScopedActionRoles...)
			actionRoles = append(actionRoles,
				KbuildActionRoleRef{Scope: "target", Role: compactKbuildScriptRunnerRole},
				KbuildActionRoleRef{Scope: "target", Role: compactKbuildScriptRuntimeRole},
			)
			metadata := &CompactMetadata{
				Config: CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{profile},
					KbuildSelections: []CompactKbuildSelection{{
						Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target",
					}},
				},
				configFragment:          map[string]string{"CONFIG_MODULES": "n"},
				actionRoles:             actionRoles,
				preconfiguredObjectTree: true,
				selectedProductsOnly:    true,
			}
			value := workloadValue{referencesBefore: len(scopes.References())}
			if discoveryOnly {
				err = metadata.DiscoverActionPlanProbes(bootstrapTestIdentity, bootstrapTestIdentity)
			} else {
				value.plan, err = metadata.ActionPlan(bootstrapTestIdentity, bootstrapTestIdentity)
			}
			value.referencesAfter = len(scopes.References())
			return value, err
		}
	}

	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload(true))
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.referencesBefore != 0 || discovery.Value.referencesAfter != 1 {
		t.Fatalf("action-only probe references before/after lowering = %d/%d, want 0/1",
			discovery.Value.referencesBefore, discovery.Value.referencesAfter)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("discovery probe plan has %d nodes, want the action-only probe", got)
	}
	if discovery.Value.plan != nil {
		t.Fatal("discovery-only action traversal returned a provisional action plan")
	}

	fullDiscovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload(false))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discovery.Plan, fullDiscovery.Plan) {
		t.Fatalf("discovery-only probe plan differs from full action-plan traversal\ndiscovery-only: %#v\nfull: %#v", discovery.Plan, fullDiscovery.Plan)
	}
	if !actionPlanRecipesContainProbeSymbol(fullDiscovery.Value.plan) {
		t.Fatal("full discovery action plan does not retain the action-only probe atom")
	}

	resultRoots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": true})
	delete(resultRoots, "host")
	oracle, err := NewProbeResultOracleFromTrees(resultRoots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(probeOptions, oracle, workload(false))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.referencesBefore != 0 || replay.Value.referencesAfter != 1 {
		t.Fatalf("replay action-only probe references before/after lowering = %d/%d, want 0/1",
			replay.Value.referencesBefore, replay.Value.referencesAfter)
	}
	if got := len(replay.Plan.Nodes); got != 1 {
		t.Fatalf("replay probe plan has %d nodes, want the action-only probe", got)
	}
	if !reflect.DeepEqual(discovery.Plan, replay.Plan) {
		t.Fatalf("replayed probe plan differs from discovery-only traversal\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
	if actionPlanRecipesContainProbeSymbol(replay.Value.plan) {
		t.Fatal("replay action plan retains a compiler-probe atom")
	}
	if !actionPlanRecipeEnvironmentContains(replay.Value.plan, "ACTION_ONLY", "-faction-only") {
		t.Fatal("replay action plan omits the resolved ACTION_ONLY=-faction-only export")
	}
}

func TestDiscoverActionPlanProbesSkipsProductFinalization(t *testing.T) {
	metadata := &CompactMetadata{
		Config:                  CompactConfig{},
		configFragment:          map[string]string{"CONFIG_MODULES": "n"},
		preconfiguredObjectTree: true,
	}
	if err := metadata.DiscoverActionPlanProbes(bootstrapTestIdentity, bootstrapTestIdentity); err != nil {
		t.Fatalf("DiscoverActionPlanProbes() failed before product finalization: %v", err)
	}
	if _, err := metadata.ActionPlan(bootstrapTestIdentity, bootstrapTestIdentity); err == nil ||
		!strings.Contains(err.Error(), "terminal product vmlinux") {
		t.Fatalf("ActionPlan() error = %v, want terminal-product validation failure", err)
	}
}

func TestProbeDiscoveryPlanReleasesExecutorPayloadsIncrementally(t *testing.T) {
	plan := &ActionPlan{
		Recipes:            map[string]ActionRecipe{},
		probeDiscoveryOnly: true,
	}
	const nodes = 64
	for index := 0; index < nodes; index++ {
		output := fmt.Sprintf("generated/%08d.o", index)
		node := ActionPlanNode{
			Stage: "target", Kind: "generate", Tool: "cc", Product: "vmlinux",
			Trees:          []string{"kernel", "prep"},
			AuxiliaryTools: []string{"objcopy", "strip"},
			Outputs:        []ActionPlanOutput{{Tree: "objects", Path: output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
			Arguments: []string{
				fmt.Sprintf("-DUNIQUE_%08d=%s", index, strings.Repeat("x", 4096)),
				"${tool:objcopy}", "${tool:strip}",
				"-o", "${output:00000000}",
			},
			Outputs: []string{"00000000"},
			Trees:   []string{"kernel", "prep"},
			AuxiliaryTools: []string{
				"objcopy", "strip",
			},
		}
		if _, err := appendActionPlanNode(plan, node, recipe); err != nil {
			t.Fatalf("append discovery node %d: %v", index, err)
		}
		// The current append remains available until its caller has had the
		// opportunity to mark an archive output. Every earlier recipe is gone.
		if got := len(plan.Recipes); got != 1 {
			t.Fatalf("after node %d discovery retains %d recipes, want only the current append", index, got)
		}
	}
	plan.releaseProbeDiscoveryPayloads()
	if got := len(plan.Recipes); got != 0 {
		t.Fatalf("completed discovery retains %d non-archive recipes", got)
	}
	if got := len(plan.probeDiscoveryRecipeCanonical); got != nodes {
		t.Fatalf("discovery retained %d exact collision witnesses, want %d", got, nodes)
	}
	if got := len(plan.Nodes); got != nodes {
		t.Fatalf("discovery structural graph has %d nodes, want %d", got, nodes)
	}
	for index, node := range plan.Nodes {
		if node.Recipe != "" || node.Product != "" || node.Trees != nil || node.AuxiliaryTools != nil {
			t.Fatalf("discovery node %d retained executor payload: %#v", index, node)
		}
		if node.Stage != "target" || node.Kind != "generate" || node.Tool != "cc" || len(node.Outputs) != 1 {
			t.Fatalf("discovery node %d lost structural lowering state: %#v", index, node)
		}
	}
}

func actionPlanRecipesContainProbeSymbol(plan *ActionPlan) bool {
	if plan == nil {
		return false
	}
	for _, recipe := range plan.Recipes {
		data, err := recipe.CanonicalJSON()
		if err == nil && linuxProbeSymbolPattern.Match(data) {
			return true
		}
	}
	return false
}

func actionPlanRecipeEnvironmentContains(plan *ActionPlan, name, value string) bool {
	if plan == nil {
		return false
	}
	for _, recipe := range plan.Recipes {
		if strings.TrimSpace(recipe.Environment[name]) == value {
			return true
		}
	}
	return false
}
