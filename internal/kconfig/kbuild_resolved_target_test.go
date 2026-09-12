package kconfig

import (
	"reflect"
	"strings"
	"testing"
)

func resolvedTargetFixture(t *testing.T) CompactKbuildProfile {
	t.Helper()
	profile := mustCompactKbuildProfileForTest(t, "resolved-target", "Makefile", "", `
cmd_cc = $(CC) -c -o $@ $<
result.o: input.c FORCE
	$(call if_changed,cc)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
	})
	return compactKbuildProfileWithSourcesForTest(t, profile, "input.c")
}

func TestCompactKbuildResolvedTargetMatchesConvenienceAPIs(t *testing.T) {
	profile := resolvedTargetFixture(t)
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, "result.o", "result.o")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.HasSelectedRule() {
		t.Fatal("resolved target has no selected rule")
	}

	context, err := resolved.Context()
	if err != nil {
		t.Fatal(err)
	}
	normal, orderOnly, stem, err := EvaluateCompactKbuildTargetRuleContext(profile, "result.o")
	if err != nil {
		t.Fatal(err)
	}
	projectedNormal := make([]string, len(context.Normal))
	for index, prerequisite := range context.Normal {
		projectedNormal[index] = prerequisite.Target
	}
	projectedOrderOnly := make([]string, len(context.OrderOnly))
	for index, prerequisite := range context.OrderOnly {
		projectedOrderOnly[index] = prerequisite.Target
	}
	if !reflect.DeepEqual(projectedNormal, normal) ||
		!reflect.DeepEqual(projectedOrderOnly, orderOnly) || context.Stem != stem {
		t.Fatalf(
			"resolved context = (%#v, %#v, %q), want (%#v, %#v, %q)",
			context.Normal, context.OrderOnly, context.Stem, normal, orderOnly, stem,
		)
	}

	effects, selected, err := resolved.SelectedTargetEffects()
	if err != nil {
		t.Fatal(err)
	}
	wantEffects, wantSelected, err := EvaluateCompactKbuildSelectedTargetEffectsForMakeTarget(
		profile, "result.o", "result.o",
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected != wantSelected || !reflect.DeepEqual(effects, wantEffects) {
		t.Fatalf("resolved effects = (%#v, %t), want (%#v, %t)", effects, selected, wantEffects, wantSelected)
	}
	wantRoles := []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}
	if !selected || !reflect.DeepEqual(effects.ActionRoles, wantRoles) ||
		!reflect.DeepEqual(effects.PrimaryActionRoles, wantRoles) || len(effects.DeferredContentQueries) != 0 {
		t.Fatalf("resolved compiler effects = %#v, want target cc as the sole action and primary role", effects)
	}
}

func TestCompactKbuildResolvedTargetAccessorsReturnCallerOwnedValues(t *testing.T) {
	profile := resolvedTargetFixture(t)
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, "result.o", "result.o")
	if err != nil {
		t.Fatal(err)
	}
	context, err := resolved.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Normal) == 0 {
		t.Fatal("resolved context has no normal prerequisites")
	}
	context.Normal[0] = CompactKbuildResolvedPrerequisite{Target: "mutated", MakeTarget: "mutated"}
	context.OrderOnly = append(context.OrderOnly, CompactKbuildResolvedPrerequisite{Target: "mutated"})
	fresh, err := resolved.Context()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Normal[0].Target == "mutated" || len(fresh.OrderOnly) != 0 {
		t.Fatalf("caller mutation leaked into retained context: %#v", fresh)
	}

	effects, selected, err := resolved.SelectedTargetEffects()
	if err != nil || !selected {
		t.Fatalf("resolved effects = (%#v, %t, %v), want selected", effects, selected, err)
	}
	if len(effects.ActionRoles) == 0 {
		t.Fatal("resolved effects have no action roles")
	}
	effects.ActionRoles[0] = KbuildActionRoleRef{Scope: "mutated", Role: "mutated"}
	effects.PrimaryActionRoles[0] = KbuildActionRoleRef{Scope: "mutated", Role: "mutated"}
	freshEffects, freshSelected, err := resolved.SelectedTargetEffects()
	if err != nil || !freshSelected {
		t.Fatalf("fresh resolved effects = (%#v, %t, %v), want selected", freshEffects, freshSelected, err)
	}
	if freshEffects.ActionRoles[0].Scope == "mutated" || freshEffects.PrimaryActionRoles[0].Scope == "mutated" {
		t.Fatalf("caller mutation leaked into retained effects: %#v", freshEffects)
	}
}

func TestCompactKbuildResolvedTargetMemoOwnsDeferredQueryState(t *testing.T) {
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("a", 64)
	profile := mustCompactKbuildProfileForTest(t, "resolved-deferred", "Makefile", "", `
if_changed = $(cmd_$(1))
cmd_emit = $(AWK) `+token+` > $@
result: input FORCE
	$(call if_changed,emit)
`, map[string]string{
		"AWK": KbuildActionRoleToken("target", "awk"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input")

	queryProfile := profile
	queryProfile.InvocationPredecessors = []string{"original-predecessor"}
	queryProfile.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "original-target", Profile: "original-profile",
		Goals: []string{"original-goal"}, ReplayArguments: []string{"make", "original-goal"},
	}}
	queryProfile.EntryTargets = []string{"original-entry"}
	queryProfile.Generated = []KbuildTarget{{
		Target: "original-generated",
		Condition: KbuildCondition{
			Kind: "and", Conditions: []KbuildCondition{{Kind: "symbol", Symbol: "ORIGINAL_GENERATED"}},
		},
	}}
	queryProfile.Rules = cloneKbuildRules(profile.Rules)
	queryProfile.Rules[0].Condition = KbuildCondition{
		Kind: "and", Conditions: []KbuildCondition{{Kind: "symbol", Symbol: "ORIGINAL_RULE"}},
	}
	queryProfile.TargetVariables = []KbuildTargetVariable{{
		Targets: []string{"original-variable-target"}, Variable: "ORIGINAL", Value: "value",
		Modifiers: []string{"export"},
	}}
	profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{
		token: {
			Token: token,
			Command: KbuildActionRoleToken("target", "awk") +
				" __LINUX_BZL_OBJECT_TREE__/generated/query-input",
			Target:      "query-producer",
			Profile:     queryProfile,
			Environment: map[string]string{"ORIGINAL_ENV": "value"},
		},
	}

	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, "result", "result")
	if err != nil {
		t.Fatal(err)
	}
	effects, selected, err := resolved.SelectedTargetEffects()
	if err != nil || !selected {
		t.Fatalf("resolved effects = (%#v, %t, %v), want selected", effects, selected, err)
	}
	if len(effects.DeferredContentQueries) != 1 {
		t.Fatalf("deferred queries = %#v, want one", effects.DeferredContentQueries)
	}
	query := &effects.DeferredContentQueries[0]
	if len(query.ActionRoles) == 0 || len(query.ObjectTree.References) == 0 {
		t.Fatalf("deferred query lacks normalized nested state: %#v", query)
	}

	query.Environment["ORIGINAL_ENV"] = "mutated"
	query.ActionRoles[0] = KbuildActionRoleRef{Scope: "mutated", Role: "mutated"}
	query.ObjectTree.References[0] = "mutated"
	query.Profile.InvocationPredecessors[0] = "mutated"
	query.Profile.TargetInvocationDependencies[0].Goals[0] = "mutated"
	query.Profile.TargetInvocationDependencies[0].ReplayArguments[0] = "mutated"
	query.Profile.EntryTargets[0] = "mutated"
	query.Profile.Generated[0].Condition.Conditions[0].Symbol = "MUTATED"
	query.Profile.Rules[0].Targets[0] = "mutated"
	query.Profile.Rules[0].Prerequisites[0] = "mutated"
	query.Profile.Rules[0].Recipe[0] = "mutated"
	query.Profile.Rules[0].Condition.Conditions[0].Symbol = "MUTATED"
	query.Profile.TargetVariables[0].Targets[0] = "mutated"
	query.Profile.TargetVariables[0].Modifiers[0] = "mutated"

	fresh, freshSelected, err := resolved.SelectedTargetEffects()
	if err != nil || !freshSelected || len(fresh.DeferredContentQueries) != 1 {
		t.Fatalf("memoized effects = (%#v, %t, %v), want one selected query", fresh, freshSelected, err)
	}
	freshQuery := fresh.DeferredContentQueries[0]
	if freshQuery.Environment["ORIGINAL_ENV"] != "value" ||
		freshQuery.ActionRoles[0] != (KbuildActionRoleRef{Scope: "target", Role: "awk"}) ||
		freshQuery.ObjectTree.References[0] != "generated/query-input" ||
		freshQuery.Profile.InvocationPredecessors[0] != "original-predecessor" ||
		freshQuery.Profile.TargetInvocationDependencies[0].Goals[0] != "original-goal" ||
		freshQuery.Profile.TargetInvocationDependencies[0].ReplayArguments[0] != "make" ||
		freshQuery.Profile.EntryTargets[0] != "original-entry" ||
		freshQuery.Profile.Generated[0].Condition.Conditions[0].Symbol != "ORIGINAL_GENERATED" ||
		freshQuery.Profile.Rules[0].Targets[0] != "result" ||
		freshQuery.Profile.Rules[0].Prerequisites[0] != "input" ||
		freshQuery.Profile.Rules[0].Recipe[0] != "$(call if_changed,emit)" ||
		freshQuery.Profile.Rules[0].Condition.Conditions[0].Symbol != "ORIGINAL_RULE" ||
		freshQuery.Profile.TargetVariables[0].Targets[0] != "original-variable-target" ||
		freshQuery.Profile.TargetVariables[0].Modifiers[0] != "export" {
		t.Fatalf("caller mutation leaked into memoized deferred query: %#v", freshQuery)
	}

	// Removing the source registry makes a fresh evaluation fail, proving that
	// the successful public result below came from the handle-local memo.
	resolved.profile.deferredContentQueries = nil
	if _, _, err := resolved.selectedTargetEffectsUncached(); err == nil || !strings.Contains(err.Error(), "unknown deferred-content token") {
		t.Fatalf("uncached effects after registry removal returned %v, want unknown-token error", err)
	}
	if cached, cachedSelected, err := resolved.SelectedTargetEffects(); err != nil || !cachedSelected ||
		len(cached.DeferredContentQueries) != 1 || cached.DeferredContentQueries[0].Token != token {
		t.Fatalf("cached effects after registry removal = (%#v, %t, %v), want retained query", cached, cachedSelected, err)
	}
}

func TestCompactKbuildResolvedTargetPreservesLexicalParentTraversal(t *testing.T) {
	const (
		target     = "virt/kvm/kvm_main.o"
		makeTarget = "arch/x86/kvm/../../../virt/kvm/kvm_main.o"
		makeSource = "arch/x86/kvm/../../../virt/kvm/kvm_main.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "resolved-parent", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_cc = $(CC) -c -o $@ $<
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed,cc)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "virt/kvm/kvm_main.c")
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.HasSelectedRule() {
		t.Fatal("lexical parent-traversal target has no selected rule")
	}
	context, err := resolved.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Normal) < 1 || context.Normal[0] != (CompactKbuildResolvedPrerequisite{
		Target: "virt/kvm/kvm_main.c", MakeTarget: makeSource,
	}) {
		t.Fatalf("lexical source prerequisite = %#v, want canonical source with Make spelling %q", context.Normal, makeSource)
	}
}

func TestCompactKbuildResolvedTargetRetainsPrerequisiteOnlyContext(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "resolved-prerequisite-only", "Makefile", "", `
result.o: input.c FORCE
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.c")
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, "result.o", "result.o")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.HasSelectedRule() {
		t.Fatal("prerequisite-only declaration unexpectedly selected an executable rule")
	}
	context, err := resolved.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(context.Normal) != 2 || context.Normal[0].Target != "input.c" || context.Normal[1].Target != "FORCE" {
		t.Fatalf("prerequisite-only context = %#v, want input.c and FORCE", context.Normal)
	}
	if effects, selected, err := resolved.SelectedTargetEffects(); err != nil || selected {
		t.Fatalf("prerequisite-only effects = (%#v, %t, %v), want no selected action", effects, selected, err)
	}
}

func TestNilCompactKbuildResolvedTargetReturnsErrors(t *testing.T) {
	var resolved *CompactKbuildResolvedTarget
	if _, err := resolved.Context(); err == nil {
		t.Fatal("nil resolved target Context returned no error")
	}
	if _, _, err := resolved.SelectedTargetEffects(); err == nil {
		t.Fatal("nil resolved target SelectedTargetEffects returned no error")
	}
}
