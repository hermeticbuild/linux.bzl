package kconfig

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestCanonicalKbuildDeferredContentEnvironmentExcludesCapabilityTags(t *testing.T) {
	firstCodec, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	secondCodec, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstCodec.EncodePath("host", "external/compiler/include")
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondCodec.EncodePath("host", "external/compiler/include")
	if err != nil {
		t.Fatal(err)
	}
	firstCanonical, err := canonicalKbuildDeferredContentEnvironment(map[string]string{"FLAGS": "-I" + first})
	if err != nil {
		t.Fatal(err)
	}
	secondCanonical, err := canonicalKbuildDeferredContentEnvironment(map[string]string{"FLAGS": "-I" + second})
	if err != nil {
		t.Fatal(err)
	}
	if firstCanonical != secondCanonical {
		t.Fatalf("deferred-content environment retains ephemeral capability tag\nfirst: %q\nsecond: %q", firstCanonical, secondCanonical)
	}
}

func TestCompactKbuildEnvironmentInternerCopiesOnceAndKeepsDistinctValues(t *testing.T) {
	interner := compactKbuildEnvironmentInterner{}
	caller := map[string]string{"CC": "clang", "FLAGS": "-O2"}
	first, err := interner.intern(caller)
	if err != nil {
		t.Fatal(err)
	}
	caller["CC"] = "caller-mutated"
	caller["ADDED"] = "caller-only"
	if got := first.values["CC"]; got != "clang" {
		t.Fatalf("interned CC after caller mutation = %q, want clang", got)
	}
	if _, leaked := first.values["ADDED"]; leaked {
		t.Fatalf("caller mutation leaked into interned environment: %#v", first.values)
	}

	repeated, err := interner.intern(map[string]string{"FLAGS": "-O2", "CC": "clang"})
	if err != nil {
		t.Fatal(err)
	}
	if repeated != first {
		t.Fatal("equal exported environments did not reuse their immutable snapshot")
	}
	distinct, err := interner.intern(map[string]string{"CC": "gcc", "FLAGS": "-O2"})
	if err != nil {
		t.Fatal(err)
	}
	if distinct == first {
		t.Fatal("distinct exported environments reused one snapshot")
	}
}

func TestBindCompactKbuildGroupedActionRejectsPartialAuthority(t *testing.T) {
	profile := CompactKbuildProfile{}
	if err := BindCompactKbuildGroupedAction(&profile, 7, "stem", "first", []string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	delete(profile.groupedActions, "second")
	if err := BindCompactKbuildGroupedAction(&profile, 7, "stem", "first", []string{"first", "second"}); err == nil || !strings.Contains(err.Error(), "partially bound") {
		t.Fatalf("partial grouped authority error = %v, want partially bound diagnostic", err)
	}
}

func controlTestStepper(t *testing.T, profile CompactKbuildProfile, options KbuildControlEvaluationOptions) *SelectedKbuildControlStepper {
	t.Helper()
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree}); err != nil {
		t.Fatal(err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, options)
	if err != nil {
		t.Fatal(err)
	}
	return stepper
}

func applyControlTestRecipe(t *testing.T, stepper *SelectedKbuildControlStepper, line KbuildSelectedControlRecipeLine) *KbuildSelectedControlRecipeSnapshot {
	t.Helper()
	line.RuleIndex = selectedControlTestRuleIndex(t, stepper.profile, line.Target)
	snapshot, err := stepper.BeforeRecipe(line, selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestSelectedKbuildControlEffectsActivateExactSourceOrderedEnvironments(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
export MODE := before
unexport DROP_ME
all: early mutate late
early: export MODE := early
early:
	@echo early
mutate:
	$(eval GENERATED := $(shell printf generated))
	$(eval export MODE := after)
late:
	@echo late
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		EnvironmentVariables:   map[string]string{"DROP_ME": "configured"},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	var active map[string]string
	stepper := controlTestStepper(t, profile, KbuildControlEvaluationOptions{
		BindProbeEnvironment: func(environment map[string]string) (func() error, error) {
			exact := maps.Clone(environment)
			return func() error { active = maps.Clone(exact); return nil }, nil
		},
	})
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []struct {
		target string
		modes  []string
	}{
		{"early", []string{"early"}}, {"mutate", []string{"before", "before"}}, {"late", []string{"after"}},
	} {
		if _, err := stepper.BeginTarget(selected.target, selected.target, "all"); err != nil {
			t.Fatal(err)
		}
		for recipeIndex, want := range selected.modes {
			applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: selected.target, RecipeIndex: recipeIndex})
			if active["MODE"] != want {
				t.Fatalf("%s line %d MODE = %q, want %q", selected.target, recipeIndex, active["MODE"], want)
			}
			if _, leaked := active["DROP_ME"]; leaked {
				t.Fatalf("source unexport left DROP_ME: %#v", active)
			}
		}
	}
	evaluation, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCompactKbuildTargetSymbolicText(evaluation.Profile, "late", "literal"); err != nil {
		t.Fatal(err)
	}
	if got, want := active["MODE"], "after"; got != want {
		t.Fatalf("late target activation MODE = %q, want %q", got, want)
	}
	if _, err := ResolveCompactKbuildTargetSymbolicText(evaluation.Profile, "early", "literal"); err != nil {
		t.Fatal(err)
	}
	if got, want := active["MODE"], "early"; got != want {
		t.Fatalf("early target activation after late MODE = %q, want %q", got, want)
	}
	if len(evaluation.Queries) != 1 {
		t.Fatalf("deferred queries = %#v, want one", evaluation.Queries)
	}
	if _, err := ResolveCompactKbuildSymbolicText(evaluation.Queries[0].Profile, "literal"); err != nil {
		t.Fatal(err)
	}
	if got, want := active["MODE"], "before"; got != want {
		t.Fatalf("deferred query snapshot activation MODE = %q, want %q", got, want)
	}

}

func TestKbuildControlRestoresInheritedEnvironmentBeforeEachExportExpansion(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
export FLAGS = $(shell probe-flags)
all:
	@echo first
	@echo second
`), 0o644); err != nil {
		t.Fatal(err)
	}
	active := map[string]string{"FLAGS": "inherited"}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		EnvironmentVariables:  map[string]string{"FLAGS": "inherited"},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
		Shell: func(command string) (string, error) {
			if command != "probe-flags" {
				t.Fatalf("unexpected Kbuild probe command %q", command)
			}
			return "measured-" + active["FLAGS"], nil
		},
		// A recursive exported variable's shell sees its incoming process
		// value while that export is being expanded. Restore the active fake
		// workload afterward, as the configured probe scope does.
		shellExportLoopOverride: func(fallbacks []kbuildShellExportFallback) (func() error, error) {
			if len(fallbacks) != 1 || fallbacks[0].name != "FLAGS" ||
				fallbacks[0].value != "inherited" || !fallbacks[0].present {
				return nil, fmt.Errorf("unexpected incoming FLAGS shell scope: %#v", fallbacks)
			}
			previous := maps.Clone(active)
			active = maps.Clone(active)
			active["FLAGS"] = fallbacks[0].value
			return func() error { active = previous; return nil }, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	stepper := controlTestStepper(t, profile, KbuildControlEvaluationOptions{
		ResetProbeEnvironment: func() error {
			active = map[string]string{"FLAGS": "inherited"}
			return nil
		},
		BindProbeEnvironment: func(environment map[string]string) (func() error, error) {
			bound := maps.Clone(environment)
			return func() error { active = maps.Clone(bound); return nil }, nil
		},
	})
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	for recipeIndex := range 2 {
		applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: "all", RecipeIndex: recipeIndex})
		if active["FLAGS"] != "measured-inherited" {
			t.Fatalf("line %d consumed previous export: %#v", recipeIndex, active)
		}
	}
	if _, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{})); err != nil {
		t.Fatal(err)
	}
	if active["FLAGS"] != "measured-inherited" {
		t.Fatalf("final export consumed previous export: %#v", active)
	}

	// The same fake Shell without a scoped incoming activation must fail
	// before it runs with a partially expanded exported FLAGS value.
	unbound := profile
	unboundParser := cloneKbuildParserForEvaluation(profile.evaluator.template)
	unboundParser.shellExportLoopOverride = nil
	unbound.evaluator = &kbuildTargetEvaluator{template: unboundParser}
	unboundStepper := controlTestStepper(t, unbound, KbuildControlEvaluationOptions{})
	if _, err := unboundStepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := unboundStepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all")}, selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{})); err == nil ||
		!strings.Contains(err.Error(), "no scoped activation is available") {
		t.Fatalf("standalone recursive exported Shell without incoming scope = %v, want rejection", err)
	}
}

func TestDeferredKbuildControlQueryLowersToExactProducerAndContentEdge(t *testing.T) {
	dir := t.TempDir()
	makefile := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefile, []byte(`
AWK := /selected/awk
KBUILD_CFLAGS := -DBASE
export KBUILD_CFLAGS
prepare: stack-prepare
stack-prepare: prepare0
	$(eval KBUILD_CFLAGS += -mstack-offset=$(shell $(AWK) '{if ($$2 == "CANARY") print $$3;}' $(objtree)/include/generated/asm-offsets.h))
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		Variables:             map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		CommandLineVariables:  map[string]string{"AWK": KbuildActionRoleToken("target", "awk")},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootProfile, err := NewCompactKbuildProfile("root", makefile, dir, parsed)
	if err != nil {
		t.Fatal(err)
	}
	rootProfile.EntryTargets = []string{"prepare"}
	stepper := controlTestStepper(t, rootProfile, KbuildControlEvaluationOptions{})
	if _, err := stepper.BeginTarget("stack-prepare", "stack-prepare", ""); err != nil {
		t.Fatal(err)
	}
	applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: "stack-prepare", Normal: []string{"prepare0"}})
	evaluation, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportedKbuildControlVariables(evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := exported["KBUILD_CFLAGS"], "-DBASE -mstack-offset="+evaluation.Queries[0].Token; got != want {
		t.Fatalf("post-control exported KBUILD_CFLAGS = %q, want %q", got, want)
	}

	childMakefile := filepath.Join(dir, "scripts", "Makefile.build")
	if err := os.MkdirAll(filepath.Dir(childMakefile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childMakefile, []byte("KBUILD_CFLAGS += -DCHILD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	childParsed, err := ParseKbuildFileTree(childMakefile, KbuildOptions{
		Variables:             exported,
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childProfile, err := NewCompactKbuildProfile("build:demo", childMakefile, dir, childParsed)
	if err != nil {
		t.Fatal(err)
	}
	childProfile, err = AttachKbuildDeferredContentQueries(childProfile, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(childProfile, "demo.o", "demo", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	wantFlags := "-DBASE -mstack-offset=" + evaluation.Queries[0].Token + " -DCHILD"
	if values["KBUILD_CFLAGS"] != wantFlags {
		t.Fatalf("propagated child KBUILD_CFLAGS = %q, want %q", values["KBUILD_CFLAGS"], wantFlags)
	}
	if query, ok := childProfile.deferredContentQueries[evaluation.Queries[0].Token]; !ok || query.Command != evaluation.Queries[0].Command {
		t.Fatalf("child deferred query provenance = %#v, want exact root query", childProfile.deferredContentQueries)
	}

	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{evaluation.Profile, childProfile},
			KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
				Token: evaluation.Queries[0].Token, Profile: evaluation.Queries[0].Origin.Profile,
				Target:    evaluation.Queries[0].Origin.Target,
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
		actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	seed := ActionPlanNode{
		Stage: "prep", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/asm-offsets.h"}},
	}
	seedID, err := appendActionPlanNode(plan, seed, seedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: kbuildFields(values["KBUILD_CFLAGS"]), Outputs: []string{"00000000"},
	}
	consumerRecipe.Arguments = append(consumerRecipe.Arguments, "-o", "${output:00000000}")
	consumer := ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "demo.o"}},
	}
	consumerID, err := appendActionPlanNode(plan, consumer, consumerRecipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 {
		t.Fatalf("nodes = %#v, want seed, deferred query, consumer", plan.Nodes)
	}
	var queryNode, consumerNode ActionPlanNode
	for _, node := range plan.Nodes {
		switch node.ID {
		case consumerID:
			consumerNode = node
		case seedID:
		default:
			queryNode = node
		}
	}
	if queryNode.Tool != "awk" || queryNode.Stage != "target" || len(queryNode.Inputs) != 1 || queryNode.Inputs[0].ProducerID != seedID {
		t.Fatalf("deferred query node = %#v, want target awk depending on asm-offset seed %s", queryNode, seedID)
	}
	if len(consumerNode.Inputs) != 1 || consumerNode.Inputs[0].ProducerID != queryNode.ID {
		t.Fatalf("consumer inputs = %#v, want deferred query %s", consumerNode.Inputs, queryNode.ID)
	}
	boundRecipe := plan.Recipes[consumerNode.Recipe]
	if len(boundRecipe.ContentSubstitutions) != 1 {
		t.Fatalf("consumer content substitutions = %#v", boundRecipe.ContentSubstitutions)
	}
	for _, argument := range boundRecipe.Arguments {
		if strings.Contains(argument, kbuildDeferredContentTokenPrefix) {
			t.Fatalf("consumer argument retained opaque token: %q", argument)
		}
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("typed deferred-content graph failed plan validation: %v", err)
	}
}

func deferredControlEnvironmentPlanForTest(
	t *testing.T,
	evaluation KbuildControlEvaluation,
	roles ...string,
) *ActionPlan {
	t.Helper()
	selections := make([]KbuildDeferredContentSelection, 0, len(evaluation.Queries))
	arguments := make([]string, 0, len(evaluation.Queries)+2)
	for _, query := range evaluation.Queries {
		selections = append(selections, KbuildDeferredContentSelection{
			Token: query.Token, Profile: query.Origin.Profile, Target: query.Origin.Target,
			Lifecycle: "target", Scope: "target", Stage: "target",
			UsesInitialObjectTree: query.ObjectTree.ObservesObjectTree,
		})
		arguments = append(arguments, query.Token)
	}
	arguments = append(arguments, "-o", "${output:00000000}")
	configured := append([]string{"cc"}, roles...)
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles:                  []CompactKbuildProfile{evaluation.Profile},
			KbuildDeferredContentSelections: selections,
		},
		actionRoles: testTargetActionRoles(configured...),
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	_, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "consumer.o"}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: arguments, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestDeferredKbuildControlQueryIdentityAndArgvEnvironmentFollowExportMutation(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "query-environment-order", "Makefile", "", `
export STATE := before
export MODE := base
export SOURCE_ROOT := $(srctree)
all:
	$(eval FIRST := $(shell MODE=inline $(AWK) same))
	$(eval export STATE := after)
	$(eval SECOND := $(shell MODE=inline $(AWK) same))
`, map[string]string{
		"AWK":     KbuildActionRoleToken("target", "awk"),
		"srctree": "__LINUX_BZL_SOURCE_TREE__",
	})
	profile.EntryTargets = []string{"all"}
	stepper := controlTestStepper(t, profile, KbuildControlEvaluationOptions{})
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	for recipeIndex := range 3 {
		applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: "all", RecipeIndex: recipeIndex})
	}
	evaluation, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Queries) != 2 {
		t.Fatalf("queries = %#v, want two source-ordered snapshots", evaluation.Queries)
	}
	first, second := evaluation.Queries[0], evaluation.Queries[1]
	if first.Target != second.Target || first.Command != second.Command {
		t.Fatalf("query source differs: first=%#v second=%#v", first, second)
	}
	if first.Token == second.Token || first.Generation == second.Generation {
		t.Fatalf("query identities did not change with exported state: first=%#v second=%#v", first, second)
	}
	if got, want := first.Environment["STATE"], "before"; got != want {
		t.Fatalf("first STATE = %q, want %q", got, want)
	}
	if got, want := second.Environment["STATE"], "after"; got != want {
		t.Fatalf("second STATE = %q, want %q", got, want)
	}

	plan := deferredControlEnvironmentPlanForTest(t, evaluation, "awk")
	states := map[string]bool{}
	queryCount := 0
	for _, node := range plan.Nodes {
		if node.Tool != "awk" || len(node.Outputs) == 0 || !strings.HasPrefix(node.Outputs[0].Path, ".linux-bzl-content/") {
			continue
		}
		queryCount++
		recipe := plan.Recipes[node.Recipe]
		states[recipe.Environment["STATE"]] = true
		if got, want := recipe.Environment["MODE"], "inline"; got != want {
			t.Errorf("argv query MODE = %q, want inline precedence %q", got, want)
		}
		if got, want := recipe.Environment["SOURCE_ROOT"], "${tree:kernel}"; got != want {
			t.Errorf("argv query SOURCE_ROOT = %q, want canonical %q", got, want)
		}
	}
	if queryCount != 2 || !states["before"] || !states["after"] {
		t.Fatalf("lowered argv query environments = %#v across %d nodes, want before and after", states, queryCount)
	}
}

func TestDeferredKbuildControlCompoundQueryUsesInheritedTargetExport(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "query-target-export", "Makefile", "", `
export UNUSED_ROLE := $(HOSTCC)
export UNUSED_OBJECT_ROOT := $(objtree)
export OBJECT_PATH := $(objtree)/include/generated/query-input
root: export MODE = inherited-$@-$^
root: query
query: input
	$(eval VALUE := $(shell $(AWK) "$$OBJECT_PATH" | $(AWK) second))
input:
`, map[string]string{
		"AWK":     KbuildActionRoleToken("target", "awk"),
		"HOSTCC":  KbuildActionRoleToken("host", "cc"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"root"}
	stepper := controlTestStepper(t, profile, KbuildControlEvaluationOptions{})
	if _, err := stepper.BeginTarget("root", "root", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("query", "query", "root"); err != nil {
		t.Fatal(err)
	}
	applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: "query", Normal: []string{"input"}})
	evaluation, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Queries) != 1 {
		t.Fatalf("queries = %#v, want one compound query", evaluation.Queries)
	}
	query, err := normalizedKbuildDeferredContentQuery(evaluation.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := query.Environment["MODE"], "inherited-query-input"; got != want {
		t.Fatalf("captured inherited MODE = %q, want %q", got, want)
	}
	if !query.ObjectTree.ObservesObjectTree || query.ObjectTree.ObservesAll ||
		!slices.Contains(query.ObjectTree.References, "include/generated/query-input") {
		t.Fatalf("environment-only object-tree observation = %#v", query.ObjectTree)
	}
	if slices.Contains(query.ActionRoles, KbuildActionRoleRef{Scope: "host", Role: "cc"}) {
		t.Fatalf("unused host export leaked into query roles: %#v", query.ActionRoles)
	}
	wholeRoot := evaluation.Queries[0]
	wholeRoot.Command = KbuildActionRoleToken("target", "awk") + ` "$UNUSED_OBJECT_ROOT"`
	wholeRoot, err = normalizedKbuildDeferredContentQuery(wholeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !wholeRoot.ObjectTree.ObservesAll {
		t.Fatalf("explicit whole object-root read = %#v, want conservative all-visible observation", wholeRoot.ObjectTree)
	}

	plan := deferredControlEnvironmentPlanForTest(t, evaluation, "awk")
	var queryRecipe ActionRecipe
	for _, node := range plan.Nodes {
		if node.Tool == compactKbuildScriptRunnerRole && len(node.Outputs) != 0 && strings.HasPrefix(node.Outputs[0].Path, ".linux-bzl-content/") {
			queryRecipe = plan.Recipes[node.Recipe]
		}
	}
	if queryRecipe.Tool == "" {
		t.Fatalf("compound deferred query was not lowered through %q: %#v", compactKbuildScriptRunnerRole, plan.Nodes)
	}
	if got, want := queryRecipe.Environment["MODE"], "inherited-query-input"; got != want {
		t.Fatalf("compound query MODE = %q, want %q", got, want)
	}
	if _, exists := queryRecipe.Environment["UNUSED_ROLE"]; exists {
		t.Fatalf("unused role-bearing export leaked into compound environment: %#v", queryRecipe.Environment)
	}
	if got, want := queryRecipe.Environment["OBJECT_PATH"], "${work:root}/include/generated/query-input"; got != want {
		t.Fatalf("compound query OBJECT_PATH = %q, want %q", got, want)
	}
	if got, want := queryRecipe.Environment["UNUSED_OBJECT_ROOT"], "${work:root}"; got != want {
		t.Fatalf("compound query UNUSED_OBJECT_ROOT = %q, want preserved execution value %q", got, want)
	}
}

func TestDeferredKbuildControlArgvQueryClassifiesUnexportedConfigShellHelper(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "scripts/query.sh", "#!/bin/sh\nprintf '%s\\n' \"$AUX:$SOURCE_PATH\"\n")
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte(`
CONFIG_SHELL := `+KbuildActionRoleToken("target", compactKbuildScriptRuntimeRole)+`
AUX := `+KbuildActionRoleToken("target", "objcopy")+`
export AUX
export SOURCE_PATH := $(srctree)/scripts/query.sh
all:
	$(eval VALUE := $(shell $(CONFIG_SHELL) scripts/query.sh))
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
		Variables: map[string]string{
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("query-source-helper", makefile, root, kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	stepper := controlTestStepper(t, profile, KbuildControlEvaluationOptions{})
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	applyControlTestRecipe(t, stepper, KbuildSelectedControlRecipeLine{Target: "all"})
	evaluation, err := stepper.Finish(selectedControlTestFrontier("empty", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Queries) != 1 {
		t.Fatalf("queries = %#v, want one source-helper query", evaluation.Queries)
	}
	query, err := normalizedKbuildDeferredContentQuery(evaluation.Queries[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []KbuildActionRoleRef{
		{Scope: "target", Role: compactKbuildScriptRuntimeRole},
		{Scope: "target", Role: "objcopy"},
	} {
		if !slices.Contains(query.ActionRoles, want) {
			t.Errorf("query action roles = %#v, want environment/helper role %#v", query.ActionRoles, want)
		}
	}
	if query.CommandShell == "" {
		t.Fatal("query omitted the unexported CONFIG_SHELL snapshot")
	}

	plan := deferredControlEnvironmentPlanForTest(
		t, evaluation, compactKbuildScriptRuntimeRole, "objcopy",
	)
	var queryRecipe ActionRecipe
	for _, node := range plan.Nodes {
		if node.Tool == compactKbuildScriptRunnerRole && len(node.Outputs) != 0 && strings.HasPrefix(node.Outputs[0].Path, ".linux-bzl-content/") {
			queryRecipe = plan.Recipes[node.Recipe]
		}
	}
	if queryRecipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("CONFIG_SHELL query recipe = %#v, want source-script runner", queryRecipe)
	}
	if got, want := queryRecipe.Environment["AUX"], "objcopy"; got != want {
		t.Fatalf("source-helper AUX = %q, want configured role %q", got, want)
	}
	if got, want := queryRecipe.Environment["SOURCE_PATH"], "${tree:kernel}/scripts/query.sh"; got != want {
		t.Fatalf("source-helper SOURCE_PATH = %q, want canonical %q", got, want)
	}
}
