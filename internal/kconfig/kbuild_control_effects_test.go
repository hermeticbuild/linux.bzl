package kconfig

import (
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

func TestSelectedKbuildControlEffectsInternTargetRecipeEnvironments(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "recipe-environment-interning", "Makefile", "", `
export MODE := shared
all: first second third
first:
	true
second:
	true
third: export MODE := distinct
third:
	true
`, nil)
	profile.EntryTargets = []string{"all"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	first := evaluation.Profile.targetRecipeEnvironments["first"]
	second := evaluation.Profile.targetRecipeEnvironments["second"]
	third := evaluation.Profile.targetRecipeEnvironments["third"]
	if first == nil || second == nil || third == nil {
		t.Fatalf("target recipe environments = first %#v, second %#v, third %#v", first, second, third)
	}
	if first != second {
		t.Fatal("targets with equal exported environments retained separate snapshots")
	}
	if first == third {
		t.Fatal("targets with distinct exported environments retained one snapshot")
	}
	if got, want := first.values["MODE"], "shared"; got != want {
		t.Fatalf("shared target MODE = %q, want %q", got, want)
	}
	if got, want := third.values["MODE"], "distinct"; got != want {
		t.Fatalf("distinct target MODE = %q, want %q", got, want)
	}

	evaluation.Profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
	token, err := registerKbuildDeferredContentQuery(evaluation.Profile, "first", "printf output", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rendered := evaluation.Profile.deferredContentQueries[token].Environment
	if !maps.Equal(rendered, first.values) {
		t.Fatalf("rendered deferred-query environment = %#v, want %#v", rendered, first.values)
	}
	rendered["MODE"] = "render-mutated"
	if got := first.values["MODE"]; got != "shared" {
		t.Fatalf("rendered action mutated retained environment to %q", got)
	}
}

func TestSelectedKbuildControlEffectsKeepExternalModpostSymversInputOutsideLocalGraph(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "external-modpost", "scripts/Makefile.modpost", "", `
output-symdump := Module.symvers
modpost-deps := $(objtree)/Module.symvers
__modpost: $(output-symdump)
$(output-symdump): $(modpost-deps) FORCE
	$(eval REBUILT := yes)
FORCE:
`, map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"})
	profile.EntryTargets = []string{"__modpost"}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Effects) != 1 || evaluation.Effects[0].Variable != "REBUILT" {
		t.Fatalf("control effects = %#v, want Module.symvers recipe after cross-tree input", evaluation.Effects)
	}
}

func TestSelectedKbuildControlEffectsUseDFSTriggerAndUnionGroupedPeerPrerequisites(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "grouped-control", "Makefile", "", `
root: left1 right
left1: left2
left2: z-trigger
right: a-peer
a-peer z-trigger &: common
	$(eval GROUP_TRIGGER := $@)
a-peer: peer-effect
peer-effect:
	$(eval PEER_EFFECT := selected)
common:
`, nil)
	profile.EntryTargets = []string{"root"}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	_, _, trigger, outputs, ok := CompactKbuildGroupedActionForTarget(evaluation.Profile, "a-peer")
	if !ok || trigger != "z-trigger" || !slices.Equal(outputs, []string{"a-peer", "z-trigger"}) {
		t.Fatalf("grouped authority = trigger %q outputs %q exists %t, want DFS trigger z-trigger", trigger, outputs, ok)
	}
	variables := map[string][]KbuildControlEffect{}
	for _, effect := range evaluation.Effects {
		variables[effect.Variable] = append(variables[effect.Variable], effect)
	}
	if got := variables["PEER_EFFECT"]; len(got) != 1 || got[0].Target != "peer-effect" {
		t.Fatalf("peer-only control effects = %#v, want one selected prerequisite effect", got)
	}
	if got := variables["GROUP_TRIGGER"]; len(got) != 1 || got[0].Target != "z-trigger" || !strings.Contains(got[0].Assignment, "z-trigger") {
		t.Fatalf("grouped recipe effects = %#v, want one z-trigger evaluation", got)
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

func TestSelectedKbuildControlEffectsSkipSatisfiedGroupedPeerAndUseMergedTriggerAutomatics(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "grouped-satisfied", "Makefile", "", `
root: a-peer z-trigger
a-peer z-trigger &: common
	$(eval GROUP_AUTOMATICS := $@|$<|$^|$+)
	$(eval GROUP_MODE := $(MODE))
z-trigger: z-only
z-trigger: MODE = z-$@
a-peer: peer-only
a-peer: MODE = a-$@
common:
	$(eval COMMON_MODE := $(MODE))
z-only:
peer-only:
	$(eval PEER_MODE := $(MODE))
`, nil)
	profile.EntryTargets = []string{"root"}

	evaluation, err := EvaluateSelectedKbuildControlEffectsWithOptions(
		profile,
		KbuildControlEvaluationOptions{TargetIsSatisfied: func(target string) bool {
			return target == "a-peer"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, trigger, _, ok := CompactKbuildGroupedActionForTarget(evaluation.Profile, "a-peer")
	if !ok || trigger != "z-trigger" {
		t.Fatalf("grouped trigger = %q exists %t, want missing z-trigger after satisfied a-peer", trigger, ok)
	}
	effects := map[string]KbuildControlEffect{}
	for _, effect := range evaluation.Effects {
		effects[effect.Variable] = effect
	}
	if got, want := effects["GROUP_AUTOMATICS"].Assignment, "GROUP_AUTOMATICS := z-trigger|common|common z-only|common z-only"; got != want {
		t.Fatalf("group recipe automatics = %q, want merged trigger context %q", got, want)
	}
	for variable, want := range map[string]string{
		"COMMON_MODE": "COMMON_MODE := z-common",
		"PEER_MODE":   "PEER_MODE := z-peer-only",
		"GROUP_MODE":  "GROUP_MODE := z-z-trigger",
	} {
		if got := effects[variable].Assignment; got != want {
			t.Errorf("%s effect = %q, want trigger-inherited value %q", variable, got, want)
		}
	}
}

func TestSelectedKbuildControlEffectsRedirectGroupedPeerWithLexicalTrigger(t *testing.T) {
	const (
		directory     = "arch/x86/kvm"
		trigger       = "virt/kvm/unit.left"
		peer          = "virt/kvm/unit.right"
		lexicalPeer   = directory + "/../../../virt/kvm/unit.right"
		lexicalTarget = directory + "/../../../virt/kvm/unit.left"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
obj := arch/x86/kvm
$(obj)/%.left: MODE := lexical
virt/kvm/unit.left: MODE := canonical-must-not-leak
$(obj)/%.left $(obj)/%.right &:
	$(eval GROUP_SEEN := $(patsubst $(obj)/%,%,$@)|$(MODE))
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "virt/kvm/unit.c")

	var groupedCandidate compactKbuildResolvedRule
	found := false
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, peer, lexicalPeer) {
		if compactKbuildRuleHasGroupedOutputs(candidate.rule) {
			groupedCandidate = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fixture has no lexical grouped candidate for %q", lexicalPeer)
	}
	groupRule, outputs, grouped, err := ResolveCompactKbuildGroupedRule(
		profile, []int{groupedCandidate.ruleOrder}, peer, groupedCandidate.stem,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !grouped || !slices.Equal(outputs, []string{trigger, peer}) {
		t.Fatalf("grouped outputs = %q grouped %t, want lexical pattern peers", outputs, grouped)
	}
	if err := BindCompactKbuildGroupedAction(&profile, groupRule, groupedCandidate.stem, trigger, outputs); err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{lexicalPeer}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Effects; len(got) != 1 || got[0].Target != trigger || got[0].Assignment != "GROUP_SEEN := ../../../virt/kvm/unit.left|lexical" {
		t.Fatalf("grouped lexical trigger effects = %#v", got)
	}
	_, _, gotTrigger, _, ok := CompactKbuildGroupedActionForTarget(evaluation.Profile, peer)
	if !ok || gotTrigger != trigger {
		t.Fatalf("grouped peer authority trigger = %q exists %t, want %q", gotTrigger, ok, trigger)
	}
	if compactKbuildGraphTargetPath(lexicalTarget) != trigger {
		t.Fatalf("fixture lexical trigger %q does not canonicalize to %q", lexicalTarget, trigger)
	}
}

func TestSelectedKbuildControlEffectsInheritFirstReachedTargetVariables(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "target-inheritance", "Makefile", "", `
root: left right
left: MODE = left-$@
right: MODE = right-$@
left: shared
right: shared
shared:
	$(eval OBSERVED := $(MODE))
`, nil)
	profile.EntryTargets = []string{"root"}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Effects[0].Assignment; got != "OBSERVED := left-shared" {
		t.Fatalf("first-reached inherited value = %q, want left-shared", got)
	}
	values, err := EvaluateCompactKbuildTarget(evaluation.Profile, "shared", "", nil, nil, nil, "MODE")
	if err != nil {
		t.Fatal(err)
	}
	if got := values["MODE"]; got != "left-shared" {
		t.Fatalf("captured shared target context MODE = %q, want left-shared", got)
	}
}

func TestSelectedKbuildControlEffectsPreserveParentTraversalLookupTarget(t *testing.T) {
	const (
		directory = "arch/x86/kvm"
		object    = "virt/kvm/kvm_main.o"
		generated = "virt/kvm/kvm_main.generated"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
obj := arch/x86/kvm
$(obj)/built-in.a: $(obj)/../../../virt/kvm/kvm_main.o
	:
$(obj)/%.o: export OBJECT_MODE := lexical
$(obj)/%.o: CONFIG_SHELL := lexical-shell
virt/kvm/kvm_main.o: export OBJECT_MODE := canonical-must-not-leak
virt/kvm/kvm_main.o: CONFIG_SHELL := canonical-shell-must-not-leak
$(obj)/%.o: $(obj)/%.generated
	$(eval OBJECT_SEEN := $(OBJECT_MODE)|$(patsubst $(obj)/%,%,$@)|$(patsubst $(obj)/%,%,$<))
	true
$(obj)/%.generated: $(obj)/%.leaf
$(obj)/%.leaf:
	$(eval INHERITED_SEEN := $(OBJECT_MODE)|$(patsubst $(obj)/%,%,$@))
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "virt/kvm/kvm_main.c")
	profile.EntryTargets = []string{directory + "/built-in.a"}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	effects := map[string]string{}
	for _, effect := range evaluation.Effects {
		effects[effect.Variable] = effect.Assignment
	}
	for variable, want := range map[string]string{
		"INHERITED_SEEN": "INHERITED_SEEN := lexical|../../../virt/kvm/kvm_main.leaf",
		"OBJECT_SEEN":    "OBJECT_SEEN := lexical|../../../virt/kvm/kvm_main.o|../../../virt/kvm/kvm_main.generated",
	} {
		if got := effects[variable]; got != want {
			t.Errorf("%s effect = %q, want %q", variable, got, want)
		}
	}
	if got := evaluation.Profile.targetRecipeEnvironments[object].values["OBJECT_MODE"]; got != "lexical" {
		t.Errorf("canonical object recipe environment OBJECT_MODE = %q, want lexical", got)
	}
	if got := evaluation.Profile.targetRecipeShells[object]; got != "lexical-shell" {
		t.Errorf("canonical object recipe CONFIG_SHELL = %q, want lexical-shell", got)
	}
	if scope, ok := evaluation.Profile.targetVariableScopes[generated]; !ok || scope == nil {
		t.Fatalf("canonical generated target scope = %#v, exists %t; want inherited lexical object scope", scope, ok)
	}
}

func TestSelectedKbuildControlEffectsUseFinalBindingPrivacyAndOrderOnlyInheritance(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "target-private", "Makefile", "", `
root: MODE = outer-$@
root: hidden public | ordered
hidden: MODE = discarded
hidden: private MODE = private-$@
hidden: hidden-leaf
public: private MODE = discarded
public: MODE = public-$@
public: public-leaf
hidden-leaf:
	$(eval HIDDEN := $(MODE))
public-leaf:
	$(eval PUBLIC := $(MODE))
ordered:
	$(eval ORDERED := $(MODE))
`, nil)
	profile.EntryTargets = []string{"root"}

	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	effects := map[string]string{}
	for _, effect := range evaluation.Effects {
		effects[effect.Variable] = effect.Assignment
	}
	for variable, want := range map[string]string{
		"HIDDEN":  "HIDDEN := outer-hidden-leaf",
		"PUBLIC":  "PUBLIC := public-public-leaf",
		"ORDERED": "ORDERED := outer-ordered",
	} {
		if got := effects[variable]; got != want {
			t.Errorf("%s effect = %q, want %q", variable, got, want)
		}
	}
}

func TestTargetSpecificCommandLineOverrideAndUnexportInheritance(t *testing.T) {
	source := `
root: child
root: MODE = ignored
root: override MODE = override-$@
root: export KEEP = kept-$@
root: unexport DROP = ignored
child:
	$(eval OBSERVED := $(MODE))
`
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		CommandLineVariables:   map[string]string{"MODE": "command-line", "DROP": "command-line-drop"},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("target-modifiers", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"root"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Effects[0].Assignment; got != "OBSERVED := override-child" {
		t.Fatalf("target override effect = %q, want override-child", got)
	}
	environment, err := EvaluateCompactKbuildTargetEnvironmentSymbolic(
		evaluation.Profile, "child", "", nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := environment["MODE"]; got != "override-child" {
		t.Errorf("exported MODE = %q, want inherited override-child", got)
	}
	if got := environment["KEEP"]; got != "kept-child" {
		t.Errorf("exported KEEP = %q, want kept-child", got)
	}
	if _, exported := environment["DROP"]; exported {
		t.Errorf("target-specific unexport left DROP in child environment: %#v", environment)
	}
}

func TestTargetSpecificCommandLinePrecedenceIsResolvedPerInheritedFrame(t *testing.T) {
	source := `
root: plain mixed private-child
root: override MODE = parent
plain: MODE = child
plain: plain-leaf
mixed: MODE = ignored
mixed: override MODE += plus
mixed: mixed-leaf
private-child: private MODE = child
private-child: private-leaf
private-child:
	$(eval PRIVATE_SELF := $(MODE))
plain-leaf:
	$(eval PLAIN := $(MODE))
mixed-leaf:
	$(eval MIXED := $(MODE))
private-leaf:
	$(eval PRIVATE_DESCENDANT := $(MODE))
`
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		CommandLineVariables:   map[string]string{"MODE": "cmd"},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("target-frame-precedence", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"root"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	effects := map[string]string{}
	for _, effect := range evaluation.Effects {
		effects[effect.Variable] = effect.Assignment
	}
	for variable, want := range map[string]string{
		"PLAIN":              "PLAIN := cmd",
		"MIXED":              "MIXED := cmd plus",
		"PRIVATE_SELF":       "PRIVATE_SELF := cmd",
		"PRIVATE_DESCENDANT": "PRIVATE_DESCENDANT := parent",
	} {
		if got := effects[variable]; got != want {
			t.Errorf("%s effect = %q, want per-frame command-line result %q", variable, got, want)
		}
	}
}

func TestSelectedKbuildControlEffectsUseEffectiveOrdinaryAndDoubleColonRecipes(t *testing.T) {
	ordinary := mustCompactKbuildProfileForTest(t, "ordinary-recipes", "Makefile", "", `
all:
	$(eval OBSERVED += overridden)
all:
	$(eval OBSERVED += effective)
`, nil)
	ordinary.EntryTargets = []string{"all"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Effects; len(got) != 1 || got[0].Assignment != "OBSERVED += effective" {
		t.Fatalf("ordinary recipe effects = %#v, want only the last recipe", got)
	}

	doubleColon := mustCompactKbuildProfileForTest(t, "double-colon-recipes", "Makefile", "", `
all::
	$(eval OBSERVED += first)
all::
	$(eval OBSERVED += second)
`, nil)
	doubleColon.EntryTargets = []string{"all"}
	evaluation, err = EvaluateSelectedKbuildControlEffects(doubleColon)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Effects; len(got) != 2 || got[0].Assignment != "OBSERVED += first" || got[1].Assignment != "OBSERVED += second" {
		t.Fatalf("double-colon recipe effects = %#v, want both independent recipes in declaration order", got)
	}

	unsupportedTimeline := mustCompactKbuildProfileForTest(t, "double-colon-timeline", "Makefile", "", `
all:: first
	$(eval OBSERVED += first)
all:: second
	$(eval OBSERVED += second)
`, nil)
	unsupportedTimeline.EntryTargets = []string{"all"}
	if _, err := EvaluateSelectedKbuildControlEffects(unsupportedTimeline); err == nil || !strings.Contains(err.Error(), "interleaved prerequisite/recipe timeline") {
		t.Fatalf("double-colon prerequisite timeline error = %v, want fail-closed diagnostic", err)
	}
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
	var bindings []map[string]string
	evaluation, err := EvaluateSelectedKbuildControlEffectsWithOptions(
		profile,
		KbuildControlEvaluationOptions{BindProbeEnvironment: func(environment map[string]string) (func() error, error) {
			exact := maps.Clone(environment)
			bindings = append(bindings, exact)
			return func() error {
				active = maps.Clone(exact)
				return nil
			}, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 5 {
		t.Fatalf("source-ordered environment bindings = %#v, want early, two mutate lines, late, and final", bindings)
	}
	for _, environment := range bindings {
		if _, leaked := environment["DROP_ME"]; leaked {
			t.Fatalf("source unexport did not remove configured variable: %#v", environment)
		}
	}
	if got, want := bindings[0]["MODE"], "early"; got != want {
		t.Fatalf("early target environment MODE = %q, want %q", got, want)
	}
	for _, index := range []int{1, 2} {
		if got, want := bindings[index]["MODE"], "before"; got != want {
			t.Fatalf("mutate line %d environment MODE = %q, want %q", index-1, got, want)
		}
	}
	for _, index := range []int{3, 4} {
		if got, want := bindings[index]["MODE"], "after"; got != want {
			t.Fatalf("post-mutation environment %d MODE = %q, want %q", index, got, want)
		}
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

	ruleIndex := func(target string) int {
		t.Helper()
		for index, rule := range profile.Rules {
			if slices.Contains(rule.Targets, target) {
				return index
			}
		}
		t.Fatalf("profile rules omit %q", target)
		return -1
	}
	early, ok := KbuildControlEvaluationBeforeRecipeIndex(evaluation, "early", ruleIndex("early"), 0)
	if !ok {
		t.Fatal("control timeline omits early recipe")
	}
	mutateBeforeQuery, ok := KbuildControlEvaluationBeforeRecipeIndex(evaluation, "mutate", ruleIndex("mutate"), 0)
	if !ok {
		t.Fatal("control timeline omits first mutate recipe")
	}
	mutateBeforeExport, ok := KbuildControlEvaluationBeforeRecipeIndex(evaluation, "mutate", ruleIndex("mutate"), 1)
	if !ok {
		t.Fatal("control timeline omits second mutate recipe")
	}
	late, ok := KbuildControlEvaluationBeforeRecipeIndex(evaluation, "late", ruleIndex("late"), 0)
	if !ok {
		t.Fatal("control timeline omits late recipe")
	}
	if early.Profile.evaluator != mutateBeforeQuery.Profile.evaluator ||
		early.Profile.evaluator != evaluation.Queries[0].Profile.evaluator {
		t.Fatal("unchanged control generation did not reuse one evaluator clone")
	}
	if mutateBeforeExport.Profile.evaluator == mutateBeforeQuery.Profile.evaluator ||
		late.Profile.evaluator == mutateBeforeExport.Profile.evaluator {
		t.Fatal("control assignment did not invalidate the cached evaluator generation")
	}
}

func TestSelectedKbuildControlEffectsPreserveOrderFlavorAndDeferredQuery(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
KBUILD_CFLAGS := -DBASE
prepare: early first later second
early: prepare0
	$(CC) $(KBUILD_CFLAGS) -c -o $@ early.c
first: prepare0
	$(eval STACK_FLAGS := -mstack-guard-offset=$(shell awk '{if ($$2 == "CANARY") print $$3;}' $(objtree)/include/generated/asm-offsets.h))
	$(eval KBUILD_CFLAGS += $(STACK_FLAGS))
	$(eval ORIGINED := file)
	$(eval export SELECTED_EXPORT := yes)
later: first
	$(CC) $(KBUILD_CFLAGS) -c -o $@ later.c
second: later
	$(eval KBUILD_CFLAGS += -DAFTER)
unselected:
	$(eval KBUILD_CFLAGS += -DMUST_NOT_APPEAR)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
		Variables:              map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		EnvironmentVariables:   map[string]string{"ORIGINED": "environment"},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"prepare"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Queries) != 1 {
		t.Fatalf("queries = %#v, want one", evaluation.Queries)
	}
	query := evaluation.Queries[0]
	for _, want := range []string{"awk", `$2 == "CANARY"`, "$3", "__LINUX_BZL_OBJECT_TREE__/include/generated/asm-offsets.h"} {
		if !strings.Contains(query.Command, want) {
			t.Errorf("query command %q omits %q", query.Command, want)
		}
	}
	if strings.Contains(query.Command, "$$") {
		t.Fatalf("query command retained eval dollar escaping: %q", query.Command)
	}
	wantVariables := []string{"STACK_FLAGS", "KBUILD_CFLAGS", "ORIGINED", "SELECTED_EXPORT", "KBUILD_CFLAGS"}
	wantOperators := []string{":=", "+=", ":=", ":=", "+="}
	gotVariables, gotOperators := []string{}, []string{}
	for _, effect := range evaluation.Effects {
		gotVariables = append(gotVariables, effect.Variable)
		gotOperators = append(gotOperators, effect.Operator)
	}
	if !slices.Equal(gotVariables, wantVariables) || !slices.Equal(gotOperators, wantOperators) {
		t.Fatalf("effects = %#v, want variables %q operators %q", evaluation.Effects, wantVariables, wantOperators)
	}
	if evaluation.Effects[0].Flavor != "simple" || evaluation.Effects[1].Flavor != "simple" {
		t.Fatalf("effect flavors = %#v", evaluation.Effects)
	}
	values, err := EvaluateCompactKbuildTarget(evaluation.Profile, "demo.o", "demo", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	want := "-DBASE -mstack-guard-offset=" + query.Token + " -DAFTER"
	if got := values["KBUILD_CFLAGS"]; got != want {
		t.Fatalf("KBUILD_CFLAGS = %q, want %q", got, want)
	}
	if strings.Contains(values["KBUILD_CFLAGS"], "MUST_NOT_APPEAR") {
		t.Fatalf("unselected effect leaked into KBUILD_CFLAGS: %q", values["KBUILD_CFLAGS"])
	}
	exported, err := ExportedKbuildControlVariables(evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := exported["ORIGINED"], "file"; got != want {
		t.Fatalf("post-control inherited export = %q, want %q", got, want)
	}
	if got, want := exported["SELECTED_EXPORT"], "yes"; got != want {
		t.Fatalf("post-control eval export = %q, want %q", got, want)
	}
	origins, err := EvaluateCompactKbuildText(
		evaluation.Profile, "demo.o", "", nil, nil, nil,
		"$(origin ORIGINED) $(origin SELECTED_EXPORT)",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := origins, "file file"; got != want {
		t.Fatalf("post-control origins = %q, want %q", got, want)
	}
	earlyValues, err := EvaluateCompactKbuildTarget(evaluation.Profile, "early", "", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := earlyValues["KBUILD_CFLAGS"], "-DBASE"; got != want || strings.Contains(got, kbuildDeferredContentTokenPrefix) {
		t.Fatalf("pre-control action KBUILD_CFLAGS = %q, want %q without a deferred query", got, want)
	}
	secondValues, err := EvaluateCompactKbuildTarget(evaluation.Profile, "later", "", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := "-DBASE -mstack-guard-offset=" + query.Token
	if got := secondValues["KBUILD_CFLAGS"]; got != wantSecond {
		t.Fatalf("later action KBUILD_CFLAGS = %q, want %q before its own eval", got, wantSecond)
	}
	findRule := func(target string) KbuildRule {
		t.Helper()
		for _, rule := range profile.Rules {
			if slices.Contains(rule.Targets, target) {
				return rule
			}
		}
		t.Fatalf("profile rules omit %q", target)
		return KbuildRule{}
	}
	firstRule := findRule("first")
	beforeFirst, ok := KbuildControlEvaluationBeforeRecipe(evaluation, "first", firstRule, 0)
	if !ok {
		t.Fatal("control timeline omits first recipe")
	}
	beforeFirstValues, err := EvaluateCompactKbuildTarget(beforeFirst.Profile, "first", "", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := beforeFirstValues["KBUILD_CFLAGS"], "-DBASE"; got != want || len(beforeFirst.Queries) != 0 {
		t.Fatalf("state before first control recipe = flags %q queries %#v, want %q and none", got, beforeFirst.Queries, want)
	}
	beforeSecondLine, ok := KbuildControlEvaluationBeforeRecipe(evaluation, "first", firstRule, 1)
	if !ok || len(beforeSecondLine.Queries) != 1 {
		t.Fatalf("state before second control recipe = %#v, want first deferred query", beforeSecondLine)
	}
	secondRule := findRule("second")
	beforeSecondTarget, ok := KbuildControlEvaluationBeforeRecipe(evaluation, "second", secondRule, 0)
	if !ok {
		t.Fatal("control timeline omits later target recipe")
	}
	beforeSecondValues, err := EvaluateCompactKbuildTarget(beforeSecondTarget.Profile, "second", "", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	wantBeforeSecond := "-DBASE -mstack-guard-offset=" + query.Token
	if got := beforeSecondValues["KBUILD_CFLAGS"]; got != wantBeforeSecond {
		t.Fatalf("state before later target = %q, want %q without its own effect", got, wantBeforeSecond)
	}
}

func TestSelectedKbuildControlEffectsVisitSharedPrerequisiteOnce(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
VALUE := start
all: left right
left right: shared
shared:
	$(eval VALUE += once)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{MakeVariablesComplete: true, CaptureTargetEvaluator: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(evaluation.Profile, "", "", nil, nil, nil, "VALUE")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["VALUE"], "start once"; got != want {
		t.Fatalf("VALUE = %q, want %q", got, want)
	}
}

func TestSelectedKbuildControlEffectsUseOnlyPlannerSelectedImplicitRule(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
VALUE := base
all: selected.x
%.x: %.missing
	$(eval VALUE += wrong)
%.x: %.source
	$(eval VALUE += right)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{MakeVariablesComplete: true, CaptureTargetEvaluator: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	allRule, selectedRule := -1, -1
	for index, rule := range profile.Rules {
		if slices.Contains(rule.Targets, "all") {
			allRule = index
		}
		if slices.Contains(rule.Recipe, "$(eval VALUE += right)") {
			selectedRule = index
		}
	}
	if allRule < 0 || selectedRule < 0 {
		t.Fatalf("fixture rules = %#v", profile.Rules)
	}
	evaluation, err := EvaluateSelectedKbuildControlEffectsWithOptions(
		profile,
		KbuildControlEvaluationOptions{SelectedRuleIndexes: func(target, _ string) []int {
			switch target {
			case "all":
				return []int{allRule}
			case "selected.x":
				return []int{selectedRule}
			default:
				return nil
			}
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(evaluation.Profile, "", "", nil, nil, nil, "VALUE")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["VALUE"], "base right"; got != want {
		t.Fatalf("VALUE = %q, want %q from only the selected implicit rule", got, want)
	}
}

func TestExportedKbuildControlVariablesPreserveProbeAtoms(t *testing.T) {
	token := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte("KBUILD_CFLAGS := "+token+"\nexport KBUILD_CFLAGS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
		ResolveSymbolic: func(value string) (string, error) {
			return strings.ReplaceAll(value, token, "-fconcrete"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), parsed)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportedKbuildControlVariables(evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if got := exported["KBUILD_CFLAGS"]; got != token {
		t.Fatalf("exported KBUILD_CFLAGS = %q, want unresolved probe atom %q", got, token)
	}
}

func TestSelectedKbuildControlEffectsRejectEmbeddedEvalAndCycles(t *testing.T) {
	for name, test := range map[string][2]string{
		"embedded": {"all:\n\techo $(eval VALUE := unsafe)\n", "embedded"},
		"cycle":    {"all: loop\nloop: all\n\t$(eval VALUE := unsafe)\n", "cycle"},
	} {
		t.Run(name, func(t *testing.T) {
			makefile := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(makefile, []byte(test[0]), 0o644); err != nil {
				t.Fatal(err)
			}
			kb, err := ParseKbuildFileTree(makefile, KbuildOptions{MakeVariablesComplete: true, CaptureTargetEvaluator: true})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), kb)
			if err != nil {
				t.Fatal(err)
			}
			profile.EntryTargets = []string{"all"}
			_, err = EvaluateSelectedKbuildControlEffects(profile)
			if err == nil || !strings.Contains(err.Error(), test[1]) {
				t.Fatalf("error = %v, want %q", err, test[1])
			}
		})
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
	evaluation, err := EvaluateSelectedKbuildControlEffects(rootProfile)
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
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
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
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
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
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
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
