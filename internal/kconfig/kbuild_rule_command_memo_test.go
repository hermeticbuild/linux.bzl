package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

func compactKbuildRuleMatchForMemoTest(
	t testing.TB,
	profile CompactKbuildProfile,
	target string,
) compactKbuildRuleMatch {
	t.Helper()
	for ruleOrder := range profile.Rules {
		rule := profile.Rules[ruleOrder]
		for targetOrder, candidate := range rule.Targets {
			if candidate == target {
				return compactKbuildRuleMatch{
					profile: profile, rule: rule, lookupTarget: target,
					ruleOrder: ruleOrder, targetOrder: targetOrder, explicit: true, resolved: true,
				}
			}
		}
	}
	t.Fatalf("Kbuild profile %q has no rule for %q", profile.Name, target)
	return compactKbuildRuleMatch{}
}

func TestEvaluatedKbuildRuleCommandsMemoKeysExactProbeEnvironment(t *testing.T) {
	const source = `
rust_probe = $(shell { trap "rm -rf .tmp_2" EXIT; mkdir .tmp_2; $(RUSTC) -Zbrand-new --crate-type=rlib /dev/null --out-dir=.tmp_2 -o .tmp_2/tmp.rlib; } >/dev/null 2>&1 && echo "-Zbrand-new" || echo "")
cmd_compile = $(RUSTC) $(rust_probe) -o $@ $<
result.o: result.c FORCE
	$(call cmd,compile)
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	targetOptions := testKbuildProbeScopeOptions(t, fixture)
	targetOptions.Tools["rustc"] = "/configured/rustc"
	workloadOptions := KbuildProbeWorkloadOptions{Target: targetOptions}
	_, err := EvaluateKbuildProbeWorkload(
		workloadOptions,
		nil,
		func(scopes *KbuildProbeScopes) (struct{}, error) {
			activateA, err := scopes.BindExactScriptEnvironments(map[string]map[string]string{
				"target": {"RUSTC": "/configured/rustc", "RUSTC_BOOTSTRAP": "a"},
			})
			if err != nil {
				return struct{}{}, err
			}
			activateB, err := scopes.BindExactScriptEnvironments(map[string]map[string]string{
				"target": {"RUSTC": "/configured/rustc", "RUSTC_BOOTSTRAP": "b"},
			})
			if err != nil {
				return struct{}{}, err
			}
			if err := activateA(); err != nil {
				return struct{}{}, err
			}
			options, err := scopes.Options("target", KbuildOptions{
				Variables: map[string]string{
					"RUSTC": workloadOptions.Target.Tools["rustc"],
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
			})
			if err != nil {
				return struct{}{}, err
			}
			parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
			if err != nil {
				return struct{}{}, err
			}
			profile, err := NewCompactKbuildProfile("build:memo", "Makefile", "", parsed)
			if err != nil {
				return struct{}{}, err
			}
			profile.probeEnvironmentActivation = activateA
			matchA := compactKbuildRuleMatchForMemoTest(t, profile, "result.o")
			first, found, err := evaluatedKbuildRuleCommands("result.o", matchA)
			if err != nil || !found || len(first) != 1 {
				return struct{}{}, fmt.Errorf("resolve environment A: templates %#v, found %t: %w", first, found, err)
			}

			profileB := profile
			profileB.probeEnvironmentActivation = activateB
			matchB := compactKbuildRuleMatchForMemoTest(t, profileB, "result.o")
			second, found, err := evaluatedKbuildRuleCommands("result.o", matchB)
			if err != nil || !found || len(second) != 1 {
				return struct{}{}, fmt.Errorf("resolve environment B: templates %#v, found %t: %w", second, found, err)
			}
			if first[0].Text == second[0].Text {
				return struct{}{}, fmt.Errorf("distinct exact compiler environments selected identical command %q", first[0].Text)
			}

			// Mutating a caller-owned result must not contaminate the stored value.
			wantA := first[0].Text
			first[0].Text = "caller-mutated"
			third, found, err := evaluatedKbuildRuleCommands("result.o", matchA)
			if err != nil || !found || len(third) != 1 {
				return struct{}{}, fmt.Errorf("resolve environment A again: templates %#v, found %t: %w", third, found, err)
			}
			if third[0].Text != wantA {
				return struct{}{}, fmt.Errorf("restored environment A command = %q, want %q", third[0].Text, wantA)
			}
			evaluator := profile.evaluator
			evaluator.commandMemoMu.Lock()
			hits, misses := evaluator.commandMemoHits, evaluator.commandMemoMisses
			evaluator.commandMemoMu.Unlock()
			if hits != 1 || misses != 2 {
				return struct{}{}, fmt.Errorf("A/B/A command memo hits/misses = %d/%d, want 1/2", hits, misses)
			}
			return struct{}{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvaluatedKbuildRuleCommandsMemoDoesNotSkipPrivateDeferredRegistry(t *testing.T) {
	const source = `
real-prereqs = $(filter-out FORCE,$^)
runtime = $(shell runtime-query $(real-prereqs))
cmd_pack = printf '%s' $(runtime) > $@
result: first second FORCE
	$(call if_changed,pack)
`
	base := mustCompactKbuildProfileForTest(t, "build:deferred-memo", "Makefile", "", source, nil)
	base.evaluator.template.shell = func(command string) (string, error) {
		return "", &LinuxProbeOwnedUnsupportedCommandError{Architecture: "x86", Command: command}
	}
	base.evaluator.template.probeEnvironmentIdentity = func() string { return "fixed-test-environment" }

	resolve := func() CompactKbuildProfile {
		profile := base
		profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
		match := compactKbuildRuleMatchForMemoTest(t, profile, "result")
		selections, found, err := evaluatedKbuildRuleCommands("result", match)
		if err != nil {
			t.Fatal(err)
		}
		if !found || len(selections) != 1 {
			t.Fatalf("deferred command resolution = %#v, found %t", selections, found)
		}
		return profile
	}
	first := resolve()
	second := resolve()
	if len(first.deferredContentQueries) != 1 || len(second.deferredContentQueries) != 1 {
		t.Fatalf("private deferred registries have %d/%d queries, want 1/1", len(first.deferredContentQueries), len(second.deferredContentQueries))
	}
	firstTokens := slices.Sorted(maps.Keys(first.deferredContentQueries))
	secondTokens := slices.Sorted(maps.Keys(second.deferredContentQueries))
	if !slices.Equal(firstTokens, secondTokens) {
		t.Fatalf("private deferred registry tokens = %q and %q, want identical query identity", firstTokens, secondTokens)
	}
	base.evaluator.commandMemoMu.Lock()
	entries := len(base.evaluator.commandMemo)
	base.evaluator.commandMemoMu.Unlock()
	if entries != 0 {
		t.Fatalf("deferred command memo entries = %d, want none for registry-mutating resolutions", entries)
	}
}

func TestBuildResolvedEvaluatesConcreteCommandSelectionOnce(t *testing.T) {
	const source = `
selection_probe = $(shell count-command-selection)
cmd = $(cmd_$(1))
if_changed = $(call cmd,$(1))
cmd_touch = $(selection_probe)touch $@
result: FORCE
	$(call if_changed,touch)
`
	selectionCalls := 0
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
		Shell: func(command string) (string, error) {
			if command != "count-command-selection" {
				return "", fmt.Errorf("unexpected shell command %q", command)
			}
			selectionCalls++
			return "", nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:single-selection", "Makefile", "", parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "result")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("result rule was not selected")
	}
	selectionCalls = 0
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildResolved("result", &match)
	if err != nil {
		t.Fatal(err)
	}
	if selectionCalls != 2 {
		t.Fatalf("lowering command-selection evaluations = %d, want one concrete and one symbolic", selectionCalls)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("lowered producer %q is absent", producer)
	}
	if got, want := node.Outputs, []ActionPlanOutput{{Tree: "objects", Path: "result"}}; !slices.Equal(got, want) {
		t.Fatalf("lowered touch outputs = %#v, want %#v", got, want)
	}
}

func BenchmarkEvaluatedKbuildRuleCommandsMemo(b *testing.B) {
	const source = `
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
cmd_and_fixdep = $(cmd); fixdep $(depfile) $@ '$(cmd_$(1))'; rm -f $(depfile)
if_changed_rule = $(if $(filter-out FORCE,$?),$(rule_$(1)),@:)
cmd_cc_o_c = $(CC) -Werror -c -o $@ $<
cmd_checksrc =
cmd_gen_objtooldep = { echo; echo '$@: $$(wildcard tools/objtool/objtool)'; } >> $(dir $@).$(notdir $@).cmd
define rule_cc_o_c
	$(call cmd_and_fixdep,cc_o_c)
	$(call cmd,checksrc)
	$(call cmd,gen_objtooldep)
endef
result.o: result.c FORCE
	$(call if_changed_rule,cc_o_c)
`
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		Variables: map[string]string{
			"CC": KbuildActionRoleToken("target", "cc"),
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		b.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:benchmark", "Makefile", "", parsed)
	if err != nil {
		b.Fatal(err)
	}
	match := compactKbuildRuleMatchForMemoTest(b, profile, "result.o")
	if _, found, err := evaluatedKbuildRuleCommands("result.o", match); err != nil || !found {
		b.Fatalf("warm command memo: found %t: %v", found, err)
	}

	b.Run("memoized", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, found, err := evaluatedKbuildRuleCommands("result.o", match); err != nil || !found {
				b.Fatalf("memoized command resolution: found %t: %v", found, err)
			}
		}
	})
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, found, err := evaluateKbuildRuleCommandsUncached("result.o", match); err != nil || !found {
				b.Fatalf("uncached command resolution: found %t: %v", found, err)
			}
		}
	})
}

func BenchmarkCompactKbuildBuildResolvedNestedCommands(b *testing.B) {
	const source = `
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
if_changed_rule = $(if 1,$(rule_$(1)),@:)
cmd_touch = touch $@
cmd_optional =
define rule_inner
	$(call cmd,touch)
	$(call cmd,optional)
endef
define rule_outer
	$(call if_changed_rule,inner)
endef
result: FORCE
	$(call if_changed_rule,outer)
`
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		b.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:lowering-benchmark", "Makefile", "", parsed)
	if err != nil {
		b.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		b.Fatal(err)
	}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "result")
	if err != nil || !found {
		b.Fatalf("resolve benchmark target: found %t: %v", found, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
		if _, err := builder.buildResolved("result", &match); err != nil {
			b.Fatal(err)
		}
	}
}
