package kconfig

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Keep the real filechk control shape: a temporary output, existing-target
// comparison, EXIT cleanup, and conditional rename. A pre-normalized pipeline
// would miss the discovery/final-helper boundary exercised here.
const selectedFilechkWrapperForTest = `
tmp-target = $(dir $@).tmp_$(notdir $@)
kecho = echo
define filechk
	$(check-FORCE)
	$(Q)set -e; \
	mkdir -p $(dir $@); \
	trap "rm -f $(tmp-target)" EXIT; \
	{ $(filechk_$(1)); } > $(tmp-target); \
	if [ ! -r $@ ] || ! cmp -s $@ $(tmp-target); then \
		$(kecho) '  UPD     $@'; \
		mv -f $(tmp-target) $@; \
	fi
endef
`

func selectedFilechkResolvedForTest(t *testing.T, source, target string) *CompactKbuildResolvedTarget {
	t.Helper()
	root := t.TempDir()
	mustWriteSource(t, root, "Kconfig", "# immutable root anchor\n")
	mustWriteSource(t, root, "arithmetic/program.dat", "immutable arithmetic program\n")
	profile := mustCompactKbuildProfileForTest(t, "selected-filechk", "Makefile", "", selectedFilechkWrapperForTest+source, nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree}); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, target, target)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSelectedDirectFilechkOutputRecipeUsesExactCallContext(t *testing.T) {
	const target = "include/generated/ticks_250.h"
	resolved := selectedFilechkResolvedForTest(t, `
RATE = 999
include/generated/ticks_250.h: RATE := 250
filechk_arbitrary = echo $(3) | bc -q $<
include/generated/ticks_%.h: arithmetic/program.dat FORCE
	$(call filechk,arbitrary,$(RATE),$(2))
.PHONY: FORCE
FORCE:
`, target)
	got, selected, err := resolved.SelectedDirectFilechkOutputRecipe()
	if err != nil || !selected {
		t.Fatalf("selected payload = %q/%t/%v", got, selected, err)
	}
	want := "{\necho 250 | bc -q ${tree:kernel}/arithmetic/program.dat\n} > '" + target + "'"
	if got != want {
		t.Fatalf("selected payload = %q, want %q", got, want)
	}
	if _, recognized := compactKbuildEvaluatedScriptOutputRecipe(got, target, []string{"arithmetic/program.dat"}); !recognized {
		t.Fatal("normalized direct helper does not reach the existing exact-output proof")
	}
	// General command-template text is still the real wrapper. Only the
	// optional output candidate adopts the final direct-helper semantics.
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, resolved.match, nil)
	if err != nil {
		t.Fatal(err)
	}
	templates, err := EvaluateCompactKbuildCommandTemplates(resolved.profile, resolved.match.rule, target, automatic.stem, nil)
	if err != nil || len(templates) != 1 || !strings.Contains(templates[0].Text, "cmp -s") || !strings.Contains(templates[0].Text, "trap") {
		t.Fatalf("general command-template wrapper changed: %#v/%v", templates, err)
	}
}

func TestSelectedDirectFilechkOutputRecipeKeepsUnsafeShapesConservative(t *testing.T) {
	const target = "include/generated/arith.h"
	for _, test := range []struct {
		name, body, call, rule string
		selected               bool
	}{
		{"shell_tail", "echo 250 | bc -q $<", "$(call filechk,arbitrary); echo changed > $@", "", false},
		{"conditional_call", "echo 250 | bc -q $<", "$(if y,$(call filechk,arbitrary))", "", false},
		{"multiple_calls", "echo 250 | bc -q $<", "$(call filechk,arbitrary) $(call filechk,arbitrary)", "", false},
		{"ignored_failure", "echo 250 | bc -q $<", "-$(call filechk,arbitrary)", "", false},
		{"multiple_recipe_lines", "echo 250 | bc -q $<", "$(call filechk,arbitrary)\n\techo changed > $@", "", false},
		{"grouped_outputs", "echo 250 | bc -q $<", "$(call filechk,arbitrary)", target + " include/generated/other.h &:", false},
		{"arbitrary_body_control", "echo 250 | bc -q $<; touch extra", "$(call filechk,arbitrary)", "", true},
		{"wrong_body_target", "echo 250 | bc -q $< > wrong.h", "$(call filechk,arbitrary)", "", true},
		{"multiple_body_targets", "echo 250 > first.h; echo 251 > second.h", "$(call filechk,arbitrary)", "", true},
		{"object_input", "echo 250 | bc -q generated/program.dat", "$(call filechk,arbitrary)", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule := test.rule
			if rule == "" {
				rule = target + ":"
			}
			resolved := selectedFilechkResolvedForTest(t,
				"filechk_arbitrary = "+test.body+"\n"+rule+" arithmetic/program.dat FORCE\n\t"+test.call+"\n.PHONY: FORCE\nFORCE:\n", target)
			got, selected, err := resolved.SelectedDirectFilechkOutputRecipe()
			if err != nil || selected != test.selected {
				t.Fatalf("selected payload = %q/%t/%v, want selected %t", got, selected, err, test.selected)
			}
			if selected {
				if _, recognized := compactKbuildEvaluatedScriptOutputRecipe(got, target, []string{"arithmetic/program.dat"}); recognized {
					t.Fatal("unsafe selected payload acquired exact-output authority")
				}
				root := resolved.profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
				builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
				if err != nil {
					t.Fatal(err)
				}
				scopes := testSourceScriptOutputScopes(t, root, builder, nil)
				if _, _, recognized, err := scopes.EvaluatedScriptOutputText("target", target, got, []string{"arithmetic/program.dat"}, nil); err != nil || recognized {
					t.Fatalf("unsafe selected payload registered a real probe: recognized=%t error=%v", recognized, err)
				}
				plan, err := builder.Plan(scopes.References()...)
				if err != nil || len(plan.Nodes) != 0 {
					t.Fatalf("unsafe payload probe plan = %#v/%v", plan, err)
				}
			}
		})
	}
	var absent *CompactKbuildResolvedTarget
	if _, selected, err := absent.SelectedDirectFilechkOutputRecipe(); selected || err != nil {
		t.Fatal("absent target supplied a selected filechk")
	}
}

func TestSelectedDirectFilechkOutputRecipePreservesProbeReplay(t *testing.T) {
	const target = "include/generated/arith.h"
	resolved := selectedFilechkResolvedForTest(t, `
filechk_arbitrary = echo 250 | bc -q $<
include/generated/arith.h: arithmetic/program.dat FORCE
	$(call filechk,arbitrary)
.PHONY: FORCE
FORCE:
`, target)
	root := resolved.profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	recipe, selected, err := resolved.SelectedDirectFilechkOutputRecipe()
	if err != nil || !selected {
		t.Fatalf("selected payload = %q/%t/%v", recipe, selected, err)
	}
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText("target", target, recipe, []string{"arithmetic/program.dat"}, nil); err != nil || concrete || !recognized {
		t.Fatalf("filechk discovery = concrete %t recognized %t error %v", concrete, recognized, err)
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("filechk discovery plan = %#v/%v", plan, err)
	}
	node := plan.Nodes[0]
	request := plan.Requests[node.RequestID]
	const contents = "#include <linux/needed.h>\n#define MEASURED 250\n"
	envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(contents)) + "\n"
	results := probeResultMap{node.ID: {
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
		Steps: evaluatedScriptOutputResultSteps(request, envelope),
	}}
	replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replayScopes := testSourceScriptOutputScopes(t, root, replayBuilder, results)
	replayRecipe, selected, err := resolved.SelectedDirectFilechkOutputRecipe()
	if err != nil || !selected || replayRecipe != recipe {
		t.Fatalf("filechk replay changed its normalized request: %q/%t/%v", replayRecipe, selected, err)
	}
	got, concrete, recognized, err := replayScopes.EvaluatedScriptOutputText("target", target, replayRecipe, []string{"arithmetic/program.dat"}, nil)
	if err != nil || !concrete || !recognized || got != contents {
		t.Fatalf("filechk replay = %q/%t/%t/%v", got, concrete, recognized, err)
	}
	// Environment rejection remains in the real probe boundary after direct
	// helper normalization; it must not infer that bc sees only its argv file.
	blockedBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	blocked := testSourceScriptOutputScopes(t, root, blockedBuilder, nil)
	blocked.evaluators["target"].scriptEnvironment["BC_ENV_ARGS"] = "additional-program.bc"
	if _, _, recognized, err := blocked.EvaluatedScriptOutputText("target", target, recipe, []string{"arithmetic/program.dat"}, nil); err != nil || recognized {
		t.Fatalf("filechk normalization bypassed compiler environment proof: %t/%v", recognized, err)
	}
}

func TestCompactKbuildRecipeIsExactCommandTemplateCall(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe string
		want   bool
	}{
		{name: "cmd", recipe: `$(call cmd,wrap)`, want: true},
		{name: "if changed", recipe: `$(call if_changed,wrap)`, want: true},
		{name: "extended wrapper", recipe: `+@$(call if_changed_except,ld_ko_o,vmlinux)`, want: true},
		{name: "leading whitespace", recipe: "  - $(call if_changed_dep,cc_o_c)  ", want: true},
		{name: "shell tail", recipe: `$(call if_changed,wrap); opaque-filter $@`},
		{name: "shell prefix", recipe: `prepare && $(call if_changed,wrap)`},
		{name: "nested make conditional", recipe: `$(if y,$(call cmd,wrap))`},
		{name: "unsupported wrapper", recipe: `$(call foreach,wrap)`},
		{name: "missing command", recipe: `$(call if_changed)`},
		{name: "multiple calls", recipe: `$(call cmd,first) $(call cmd,second)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CompactKbuildRecipeIsExactCommandTemplateCall(test.recipe); got != test.want {
				t.Fatalf("CompactKbuildRecipeIsExactCommandTemplateCall(%q) = %t, want %t", test.recipe, got, test.want)
			}
		})
	}
}

func TestKbuildSelectedMakeCmdEscapesLeafExactly(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:root", "Makefile", "", `
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if-changed-cond = 1
if_changed = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)

cmd_emit = printf '%s\n' '$$cash $(pound)hash it'\''s' > $@
generated/result: FORCE
	$(call if_changed,emit)
`, nil)

	templates, err := EvaluateCompactKbuildCommandTemplates(
		profile, profile.Rules[0], "generated/result", "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	const want = `set -e; printf '%s\n' '$cash #hash it'\''s' > generated/result; printf '%s\n' 'savedcmd_generated/result := printf '\''%s\n'\'' '\''$$cash $(pound)hash it'\''\'\'''\''s'\'' > generated/result' > generated/.result.cmd`
	if len(templates) != 1 || templates[0].Name != "emit" || templates[0].Text != want {
		t.Fatalf("selected make-cmd template = %#v, want %q", templates, want)
	}
}

func TestKbuildCommandTemplatesFlattenNestedSourceRuleMacros(t *testing.T) {
	const target = "rust/helpers/helpers.o"
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	source := `
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(cmd_$(1))
cmd_and_fixdep = $(cmd); fixdep $(depfile) $@ '$(make-cmd)'; rm -f $(depfile)
newer-prereqs = $(filter-out FORCE,$?)
if-changed-cond = $(newer-prereqs)
if_changed_rule = $(if $(if-changed-cond),$(rule_$(1)),@:)

objtool = $(objtree)/tools/objtool/objtool
cmd_objtool = $(if $(objtool-enabled), ; $(objtool) $@)
probe_flag = ` + probe + `
cmd_cc_o_c = $(CC) $(probe_flag) -c -o $@ $< $(cmd_objtool)
cmd_checksrc =
cmd_checkdoc =
cmd_gen_objtooldep = { echo ; echo '$@: $$(wildcard tools/objtool/objtool)' ; } >> $(dir $@).$(notdir $@).cmd
cmd_force_checksrc =
cmd_gendwarfksyms = $(GENDWARFKSYMS) $@

define rule_cc_o_c
	$(call cmd_and_fixdep,cc_o_c)
	$(call cmd,checksrc)
	$(call cmd,checkdoc)
	$(call cmd,gen_objtooldep)
endef

define rule_rust_cc_library
	$(call if_changed_rule,cc_o_c)
	$(call cmd,force_checksrc)
	$(call cmd,gendwarfksyms)
endef

part-of-module = $(if $(filter $(basename $@).o, $(real-obj-m)),y)
is-kernel-object = $(part-of-module)
is-standard-object = $(if $(filter-out y%, $(OBJECT_FILES_NON_STANDARD_$(target-stem).o)$(OBJECT_FILES_NON_STANDARD)n),$(is-kernel-object))
is-single-obj-m = $(and $(part-of-module),$(filter $@, $(obj-m)),y)
delay-objtool := y
$(obj)/%.o: private objtool-enabled = $(if $(is-standard-object),$(if $(delay-objtool),$(is-single-obj-m),y))
$(obj)/%.o: $(obj)/%.c FORCE
	+$(call if_changed_rule,rust_cc_library)
`
	profile := mustCompactKbuildProfileForTest(t, "build:rust", "rust/Makefile", "rust", source, map[string]string{
		"CC":            KbuildActionRoleToken("target", "cc"),
		"GENDWARFKSYMS": KbuildActionRoleToken("target", "gendwarfksyms"),
		"obj":           "rust",
		"objtree":       "",
		"obj-m":         target,
		"real-obj-m":    target,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "rust/helpers/helpers.c")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("nested source rule did not select %q", target)
	}
	if got, want := match.commandSequence(), []string{"cc_o_c", "gen_objtooldep", "gendwarfksyms"}; !slices.Equal(got, want) {
		t.Fatalf("nested source command sequence = %q, want %q", got, want)
	}
	templates, err := EvaluateCompactKbuildCommandTemplates(profile, match.rule, target, "helpers/helpers", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 3 {
		t.Fatalf("nested command templates = %#v, want three", templates)
	}
	if got, want := []string{templates[0].Name, templates[1].Name, templates[2].Name}, match.commandSequence(); !slices.Equal(got, want) {
		t.Fatalf("nested command templates = %q, want %q", got, want)
	}
	if !strings.Contains(templates[0].Text, KbuildActionRoleToken("target", "cc")) ||
		!strings.Contains(templates[0].Text, probe) ||
		!strings.Contains(templates[0].Text, "tools/objtool/objtool") ||
		!strings.Contains(templates[0].Text, "fixdep") ||
		!strings.Contains(templates[0].Text, "rm -f") ||
		!strings.Contains(templates[2].Text, KbuildActionRoleToken("target", "gendwarfksyms")) {
		t.Fatalf("nested command templates lost source wrapper effects: %#v", templates)
	}
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile, target, "helpers/helpers", match.rule.Prerequisites, match.rule.OrderOnly,
	)
	if err != nil {
		t.Fatal(err)
	}
	injected = compactKbuildActionTreeInjections(injected)
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(
		target, match, []compactKbuildRuleInput{{path: "rust/helpers/helpers.c", sourceID: "src-test"}}, injected,
	)
	if err != nil {
		t.Fatal(err)
	}
	privateTemplates, err := evaluatedKbuildRuleCommandSelections(
		profile, target, automatic.target, automatic.stem, automatic.normal,
		automatic.order, injected, match.rule.Recipe, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	privateNames := make([]string, 0, len(privateTemplates))
	for _, template := range privateTemplates {
		privateNames = append(privateNames, template.Name)
	}
	if want := match.commandSequence(); !slices.Equal(privateNames, want) {
		t.Fatalf("private-root nested command templates = %q, want %q", privateNames, want)
	}
}

func TestKbuildCommandSelectionRetainsLeafHiddenBySymbolicMakeIf(t *testing.T) {
	token := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	profile := mustCompactKbuildProfileForTest(t, "build:symbolic", "scripts/Makefile.build", "", `
gate = `+token+`
if_changed = $(if $(gate),$(cmd_emit),@:)
cmd_emit = $(CC) -c -o $@ $<
result.o: result.c FORCE
	$(call if_changed,emit)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "result.c")
	profile.evaluator.template.selectSymbolic = func(_, _ string, _ bool, _, _ string) (string, bool, error) {
		// Discovery deliberately hides both branch texts behind the exact
		// selector atom. Command identity must come from the observed source
		// expansion, not by inspecting that atom's private comparator payload.
		return token, true, nil
	}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "result.o")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !slices.Equal(match.commandSequence(), []string{"emit"}) {
		t.Fatalf("symbolic wrapper selected match=%#v found=%t, want hidden emit leaf", match, found)
	}
}

func TestKbuildCommandSelectionUsesExactResolvedEmptiness(t *testing.T) {
	emptyToken := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	firstToken := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	lastToken := linuxProbeSymbolPrefix + strings.Repeat("c", 64)
	recipe := []string{
		"$(call cmd,emit,first)",
		"$(call cmd,emit,optional)",
		"$(call cmd,emit,last)",
		"$(call cmd,emit,first)",
	}
	for _, test := range []struct {
		name, optional string
		resolved, fail bool
	}{
		{name: "discovery retains unresolved occurrences"},
		{name: "empty middle occurrence", resolved: true},
		{name: "whitespace middle occurrence", optional: " \t\n ", resolved: true},
		{name: "nonempty middle occurrence", optional: "echo optional", resolved: true},
		{name: "failed answer is not emptiness", resolved: true, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:selection", "Makefile", "", `
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
cmd_emit = $(payload_$(2))
payload_first = `+firstToken+`
payload_optional = `+emptyToken+`
payload_last = `+lastToken+`
result:
	$(call cmd,emit,first)
`, nil)
			answers := map[string]string{firstToken: "echo first", emptyToken: test.optional, lastToken: "echo last"}
			profile.evaluator.template.resolveSymbolic = func(value string) (string, error) {
				if !test.resolved {
					return value, nil
				}
				if test.fail && strings.Contains(value, emptyToken) {
					return "", fmt.Errorf("missing exact occurrence answer")
				}
				for token, answer := range answers {
					value = strings.ReplaceAll(value, token, answer)
				}
				return value, nil
			}
			for _, concrete := range []bool{true, false} {
				selections, err := evaluatedKbuildRuleCommandSelections(profile, "result", "result", "", nil, nil, nil, recipe, concrete)
				if test.fail {
					if err == nil || !strings.Contains(err.Error(), "missing exact occurrence answer") {
						t.Fatalf("concrete=%t swallowed failed oracle answer: %v", concrete, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				want := []string{firstToken, emptyToken, lastToken, firstToken}
				if test.resolved && strings.TrimSpace(test.optional) == "" {
					want = []string{firstToken, lastToken, firstToken}
				}
				if len(selections) != len(want) {
					t.Fatalf("concrete=%t selected %d occurrences, want %d: %#v", concrete, len(selections), len(want), selections)
				}
				for index, token := range want {
					text := token
					if concrete && test.resolved {
						text = answers[token]
					}
					if selections[index].Name != "emit" || selections[index].Text != text {
						t.Fatalf("concrete=%t occurrence %d = %#v, want emit/%q", concrete, index, selections[index], text)
					}
				}
			}
		})
	}
}

func TestKbuildSymbolicCommandSelectionSkipsProvenNonemptyResolution(t *testing.T) {
	token := linuxProbeSymbolPrefix + strings.Repeat("d", 64)
	profile := mustCompactKbuildProfileForTest(t, "build:nonempty", "Makefile", "", `
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
cmd_emit = configured-cc `+token+` -c source.c -o result.o
result.o:
	$(call cmd,emit)
`, nil)
	calls := 0
	profile.evaluator.template.resolveSymbolic = func(value string) (string, error) {
		calls++
		return strings.ReplaceAll(value, token, "-fselected"), nil
	}
	for _, concrete := range []bool{false, true} {
		calls = 0
		selections, err := evaluatedKbuildRuleCommandSelections(profile, "result.o", "result.o", "", nil, nil, nil, []string{"$(call cmd,emit)"}, concrete)
		if err != nil || len(selections) != 1 {
			t.Fatalf("concrete=%t selections=%#v error=%v", concrete, selections, err)
		}
		if !concrete {
			if calls != 0 || !strings.Contains(selections[0].Text, token) {
				t.Fatalf("symbolic nonempty occurrence resolved %d times: %q", calls, selections[0].Text)
			}
		} else if calls == 0 || strings.Contains(selections[0].Text, token) || !strings.Contains(selections[0].Text, "-fselected") {
			t.Fatalf("concrete occurrence did not resolve arguments: calls=%d text=%q", calls, selections[0].Text)
		}
	}
}

func TestKbuildCommandSelectionRecoversExactWrapperFromUnlowerableRebuildGuard(t *testing.T) {
	token := linuxProbeSymbolPrefix + strings.Repeat("e", 64)
	profile := mustCompactKbuildProfileForTest(t, "build:symbolic-guard", "scripts/Makefile.build", "", `
gate = `+token+`
if_changed_rule = $(if $(gate),$(rule_$(1)),@:)
rule_cc_o_c = $(cmd_cc_o_c)
cmd_cc_o_c = $(CC) -c -o $@ $<
result.o: result.c FORCE
	$(call if_changed_rule,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "result.c")
	classifications := 0
	profile.evaluator.template.selectSymbolic = func(_, _ string, _ bool, _, _ string) (string, bool, error) {
		classifications++
		return "", true, fmt.Errorf("whole command text has no process protocol")
	}

	templates, err := EvaluateCompactKbuildCommandTemplatesSymbolic(
		profile, profile.Rules[0], "result.o", "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if classifications != 1 {
		t.Fatalf("rebuild guard classifications = %d, want one failed exact comparison", classifications)
	}
	if len(templates) != 1 || templates[0].Name != "cc_o_c" ||
		!strings.Contains(templates[0].Text, KbuildActionRoleToken("target", "cc")) {
		t.Fatalf("recovered rebuild-guard command templates = %#v, want selected cc_o_c leaf", templates)
	}
}

func TestKbuildCommandSelectionRetainsNestedCmdLeavesHiddenBySymbolicMakeIf(t *testing.T) {
	conditionToken := linuxProbeSymbolPrefix + strings.Repeat("c", 64)
	selectionToken := linuxProbeSymbolPrefix + strings.Repeat("d", 64)
	profile := mustCompactKbuildProfileForTest(t, "build:symbolic-nested-cmd", "Makefile", "", `
gate = `+conditionToken+`
if_changed = $(cmd_$(1))
cmd_first = first-tool $@
cmd_second = second-tool $@
cmd_outer = $(if $(gate),$(cmd_first),$(cmd_second))
result: FORCE
	$(call if_changed,outer)
`, nil)

	var trueBranch, falseBranch string
	profile.evaluator.template.selectSymbolic = func(_, _ string, _ bool, trueText, falseText string) (string, bool, error) {
		if trueText == "1" && falseText == "" {
			return conditionToken, true, nil
		}
		trueBranch, falseBranch = trueText, falseText
		return selectionToken, true, nil
	}
	templateNames := func(templates []CompactKbuildCommandTemplate) []string {
		names := make([]string, 0, len(templates))
		for _, template := range templates {
			names = append(names, template.Name)
		}
		return names
	}
	discovery, err := EvaluateCompactKbuildCommandTemplatesSymbolic(profile, profile.Rules[0], "result", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := templateNames(discovery); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("symbolic nested cmd leaves = %q, want both branch commands", got)
	}

	for _, test := range []struct {
		name       string
		selectTrue bool
		want       string
	}{
		{name: "true", selectTrue: true, want: "first"},
		{name: "false", want: "second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile.evaluator.template.resolveSymbolic = func(value string) (string, error) {
				selected := falseBranch
				if test.selectTrue {
					selected = trueBranch
				}
				return strings.ReplaceAll(value, selectionToken, selected), nil
			}
			templates, err := EvaluateCompactKbuildCommandTemplates(profile, profile.Rules[0], "result", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := templateNames(templates); !slices.Equal(got, []string{test.want}) {
				t.Fatalf("resolved symbolic nested cmd leaves = %q, want %q", got, test.want)
			}
		})
	}
}

func TestKbuildCommandSelectionRetainsCallLocalArgumentsPerOccurrence(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:call-locals", "Makefile", "", `
if_changed = $(cmd_$(1))
cmd_emit = $(AWK) $(2) > $@
result: first.policy second.policy FORCE
	$(call if_changed,emit,first.policy)
	$(call if_changed,emit,second.policy)
`, map[string]string{"AWK": KbuildActionRoleToken("target", "awk")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "first.policy", "second.policy")
	templates, err := EvaluateCompactKbuildCommandTemplates(profile, profile.Rules[0], "result", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 {
		t.Fatalf("same-name command occurrences = %#v, want two ordered selections", templates)
	}
	if templates[0].Name != "emit" || templates[1].Name != "emit" ||
		!strings.Contains(templates[0].Text, "first.policy") ||
		!strings.Contains(templates[1].Text, "second.policy") {
		t.Fatalf("same-name command occurrences lost call-local arguments: %#v", templates)
	}
}

func TestKbuildCommandSelectionResolvesOneSymbolicBranchOccurrence(t *testing.T) {
	conditionToken := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	selectionToken := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	profile := mustCompactKbuildProfileForTest(t, "build:symbolic-occurrence", "Makefile", "", `
gate = `+conditionToken+`
if_changed = $(if $(gate),$(cmd_first),$(cmd_second))
cmd_first = first-tool $@
cmd_second = second-tool $@
result: FORCE
	$(call if_changed,unused)
`, nil)

	var trueBranch, falseBranch string
	profile.evaluator.template.selectSymbolic = func(_, _ string, _ bool, trueText, falseText string) (string, bool, error) {
		if trueText == "1" && falseText == "" {
			return conditionToken, true, nil
		}
		trueBranch, falseBranch = trueText, falseText
		return selectionToken, true, nil
	}
	discovery, err := EvaluateCompactKbuildCommandTemplatesSymbolic(profile, profile.Rules[0], "result", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery) != 2 {
		t.Fatalf("symbolic command occurrences = %#v, want two branch occurrences for graph discovery", discovery)
	}
	if got := []string{discovery[0].Name, discovery[1].Name}; !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("symbolic command occurrences = %q, want stable branch union for graph discovery", got)
	}

	for _, test := range []struct {
		name       string
		selectTrue bool
		want       string
	}{
		{name: "true", selectTrue: true, want: "first"},
		{name: "false", selectTrue: false, want: "second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile.evaluator.template.resolveSymbolic = func(value string) (string, error) {
				selected := falseBranch
				if test.selectTrue {
					selected = trueBranch
				}
				return strings.ReplaceAll(value, selectionToken, selected), nil
			}
			templates, err := EvaluateCompactKbuildCommandTemplates(profile, profile.Rules[0], "result", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(templates) != 1 || templates[0].Name != test.want {
				t.Fatalf("resolved symbolic command occurrences = %#v, want only %q", templates, test.want)
			}
		})
	}
}

func TestKbuildCommandSelectionLowersCallArgumentAndActionProvenance(t *testing.T) {
	const (
		target = "security/ipe/generated-policy.c"
		policy = "security/ipe/boot-policy.p7b"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:security/ipe", "security/ipe/Makefile", "security/ipe", `
if_changed = $(cmd_$(1))
cmd_polgen = $(AWK) --boot-policy $(2) > $@
security/ipe/generated-policy.c: security/ipe/boot-policy.p7b FORCE
	$(call if_changed,polgen,$(CONFIG_IPE_BOOT_POLICY))
`, map[string]string{
		"AWK":                    KbuildActionRoleToken("target", "awk"),
		"CONFIG_IPE_BOOT_POLICY": policy,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, policy)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}

	effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || !slices.Contains(effects.ActionRoles, KbuildActionRoleRef{Scope: "target", Role: "awk"}) {
		t.Fatalf("call-local command action provenance = %#v selected=%t, want target awk", effects, selected)
	}

	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("call-local command has no %q producer: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("call-local command producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	policyArgument := ""
	for _, argument := range recipe.Arguments {
		if strings.HasPrefix(argument, "${source:") && recipe.WorkingInputs[strings.TrimSuffix(strings.TrimPrefix(argument, "${"), "}")] == policy {
			policyArgument = argument
			break
		}
	}
	if node.Tool != "awk" || policyArgument == "" {
		t.Fatalf("call-local lowered argv lost CONFIG_IPE_BOOT_POLICY: node=%#v recipe=%#v", node, recipe)
	}
}

func TestKbuildNestedRuleSelectionFailsClosed(t *testing.T) {
	depthSource := strings.Builder{}
	depthSource.WriteString("if_changed_rule = $(rule_$(1))\n")
	for index := 0; index <= compactKbuildRuleSelectionDepthLimit; index++ {
		fmt.Fprintf(&depthSource, "define rule_level_%d\n", index)
		if index == compactKbuildRuleSelectionDepthLimit {
			depthSource.WriteString("\t$(call cmd,emit)\n")
		} else {
			fmt.Fprintf(&depthSource, "\t$(call if_changed_rule,level_%d)\n", index+1)
		}
		depthSource.WriteString("endef\n")
	}
	depthSource.WriteString("cmd_emit = touch $@\nresult: FORCE\n\t$(call if_changed_rule,level_0)\n")

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "cycle",
			source: `
if_changed_rule = $(rule_$(1))
define rule_first
	$(call if_changed_rule,second)
endef
define rule_second
	$(call if_changed_rule,first)
endef
result: FORCE
	$(call if_changed_rule,first)
`,
			want: "rule_first -> rule_second -> rule_first",
		},
		{
			name:   "depth",
			source: depthSource.String(),
			want:   "too deep Kbuild variable expansion",
		},
		{
			name: "direct shell",
			source: `
if_changed_rule = $(rule_$(1))
cmd_emit = touch $@
define rule_direct
	echo unmodeled > $@
	$(call cmd,emit)
endef
result: FORCE
	$(call if_changed_rule,direct)
`,
			want: "contains direct shell text",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:"+test.name, "Makefile", "", test.source, nil)
			metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
			_, _, err := metadata.compactKbuildRuleForProfile(profile, "result")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("nested rule error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveCompactKbuildSymbolicTextUsesExactReplayWithoutReexpandingMake(t *testing.T) {
	const source = linearFilterCompilerFixture + `
CC_FLAGS_LTO := $(call cc-option,-flto)
c_flags = -I$(srctree)/include -I$(objtree)/include $(CC_FLAGS_LTO)
cmd_cc_s_c = $(CC) $(filter-out $(DEBUG_CFLAGS) $(CC_FLAGS_LTO), $(c_flags)) -fverbose-asm -S -o $@ $<
result.s: result.c FORCE
	$(call if_changed_dep,cc_s_c)
`
	type evaluationValue struct {
		raw      string
		resolved string
		concrete string
	}
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	workload := func(scopes *KbuildProbeScopes) (evaluationValue, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC":      opts.Target.Tools["cc"],
				"SRCARCH": "x86",
				"objtree": "__LINUX_BZL_OBJECT_TREE__",
				"srctree": "__LINUX_BZL_SOURCE_TREE__",
			},
			SourceRoots: map[string]string{
				"__LINUX_BZL_SOURCE_TREE__": "/kernel",
				"__LINUX_BZL_OBJECT_TREE__": "/object",
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureTargetEvaluator:  true,
		})
		if err != nil {
			return evaluationValue{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", options, "")
		if err != nil {
			return evaluationValue{}, err
		}
		profile, err := NewCompactKbuildProfile("scripts/mod", "scripts/Makefile.build", "/kernel", parsed)
		if err != nil {
			return evaluationValue{}, err
		}
		var rule KbuildRule
		for _, candidate := range profile.Rules {
			if slices.Contains(candidate.Targets, "result.s") {
				rule = candidate
				break
			}
		}
		if len(rule.Recipe) == 0 {
			return evaluationValue{}, fmt.Errorf("result.s rule has no recipe")
		}
		templates, err := EvaluateCompactKbuildCommandTemplatesSymbolic(
			profile, rule, "result.s", "result", nil,
		)
		if err != nil {
			return evaluationValue{}, err
		}
		if len(templates) != 1 {
			return evaluationValue{}, fmt.Errorf("result.s has %d command templates, want one", len(templates))
		}
		resolved, err := ResolveCompactKbuildSymbolicText(profile, templates[0].Text)
		if err != nil {
			return evaluationValue{}, err
		}
		concrete, err := EvaluateCompactKbuildCommandTemplates(
			profile, rule, "result.s", "result", nil,
		)
		if err != nil {
			return evaluationValue{}, err
		}
		if len(concrete) != 1 {
			return evaluationValue{}, fmt.Errorf("result.s has %d concrete command templates, want one", len(concrete))
		}
		return evaluationValue{raw: templates[0].Text, resolved: resolved, concrete: concrete[0].Text}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("discovery plan has %d nodes, want one", got)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.raw) || discovery.Value.resolved != discovery.Value.raw {
		t.Fatalf("discovery raw/resolved = %q / %q, want the same opaque token", discovery.Value.raw, discovery.Value.resolved)
	}
	if strings.Contains(discovery.Value.resolved, "${tree:prep}") {
		t.Fatalf("discovery unexpectedly exposed object-tree path: %q", discovery.Value.resolved)
	}

	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(_ int, _ ProbePlanNode) bool { return true })
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(replay.Value.raw) {
		t.Fatalf("symbolic replay template lost its opaque atom: %q", replay.Value.raw)
	}
	wantSourceInclude := "-I${tree:kernel}/include"
	wantObjectInclude := "-I${tree:prep}/include"
	if sourceIndex, objectIndex := strings.Index(replay.Value.resolved, wantSourceInclude), strings.Index(replay.Value.resolved, wantObjectInclude); sourceIndex < 0 || objectIndex < 0 || sourceIndex >= objectIndex || linuxProbeSymbolPattern.MatchString(replay.Value.resolved) {
		t.Fatalf(
			"replay resolved template = %q, want ordered %q then %q and no opaque atom",
			replay.Value.resolved, wantSourceInclude, wantObjectInclude,
		)
	}
	concreteSourceInclude := "-I__LINUX_BZL_SOURCE_TREE__/include"
	concreteObjectInclude := "-I__LINUX_BZL_OBJECT_TREE__/include"
	concreteSourceIndex := strings.Index(replay.Value.concrete, concreteSourceInclude)
	concreteObjectIndex := strings.Index(replay.Value.concrete, concreteObjectInclude)
	if concreteSourceIndex < 0 || concreteObjectIndex < 0 || concreteSourceIndex >= concreteObjectIndex || linuxProbeSymbolPattern.MatchString(replay.Value.concrete) {
		t.Fatalf(
			"concrete replay template = %q, want ordered %q then %q and no hidden opaque atom",
			replay.Value.concrete, concreteSourceInclude, concreteObjectInclude,
		)
	}
}

func TestConcreteCommandTemplatesOmitDefinedSelectionResolvedEmpty(t *testing.T) {
	marker := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	source := strings.Replace(`
cmd = $(cmd_$(1))
cmd_compile = cc -c input.c -o result.o
cmd_optional = __DYNAMIC_OPTIONAL_COMMAND__
result.o:
	$(call cmd,compile)
	$(call cmd,optional)
`, "__DYNAMIC_OPTIONAL_COMMAND__", marker, 1)
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
		ResolveSymbolic: func(value string) (string, error) {
			return strings.ReplaceAll(value, marker, ""), nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:resolved-empty", "scripts/Makefile.build", "", parsed)
	if err != nil {
		t.Fatal(err)
	}
	var rule KbuildRule
	for _, candidate := range profile.Rules {
		if slices.Contains(candidate.Targets, "result.o") {
			rule = candidate
			break
		}
	}
	templates, err := EvaluateCompactKbuildCommandTemplates(profile, rule, "result.o", "result", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].Name != "compile" || strings.TrimSpace(templates[0].Text) == "" {
		t.Fatalf("resolved command templates = %#v, want only nonempty cmd_compile", templates)
	}
}

func TestConcreteCommandTemplatesDoNotManufactureUndefinedSelection(t *testing.T) {
	parsed, err := parseKbuildWithOptions(strings.NewReader(`
result.o:
	$(call cmd,missing)
`), "scripts/Makefile.build", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:undefined-command", "scripts/Makefile.build", "", parsed)
	if err != nil {
		t.Fatal(err)
	}
	templates, err := EvaluateCompactKbuildCommandTemplates(profile, profile.Rules[0], "result.o", "result", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 0 {
		t.Fatalf("undefined command templates = %#v, want no manufactured executable leaf", templates)
	}
}

func TestKbuildCommandTemplatesDeferOutputDependentRecipeShell(t *testing.T) {
	const makefile = `
real-prereqs = $(filter-out FORCE,$^)
size_append = printf $(shell dec_size=0; for F in $(real-prereqs); do fsize=$$(sh scripts/file-size.sh $$F); dec_size=$$(expr $$dec_size + $$fsize); done; printf '%08x' $$dec_size)
cmd_pack = { cat $(real-prereqs); $(size_append); } > $@
result: first second FORCE
	$(call if_changed,pack)
`
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", makefile, nil)
	var selected KbuildRule
	for _, rule := range profile.Rules {
		if slices.Contains(rule.Targets, "result") {
			selected = rule
			break
		}
	}
	if len(selected.Recipe) == 0 {
		t.Fatal("result rule has no recipe")
	}

	var shellCommands []string
	profile.evaluator.template.shell = func(command string) (string, error) {
		shellCommands = append(shellCommands, command)
		// The real Linux size query contains a source script, so the compiler
		// evaluator owns the command namespace even though this compound form is
		// execution-time recipe work rather than an analysis-time probe.
		return "", &LinuxProbeOwnedUnsupportedCommandError{Architecture: "x86", Command: command}
	}
	for _, test := range []struct {
		name     string
		evaluate func(CompactKbuildProfile, KbuildRule, string, string, map[string]string) ([]CompactKbuildCommandTemplate, error)
	}{
		{name: "concrete", evaluate: EvaluateCompactKbuildCommandTemplates},
		{name: "symbolic", evaluate: EvaluateCompactKbuildCommandTemplatesSymbolic},
	} {
		t.Run(test.name, func(t *testing.T) {
			shellCommands = nil
			templates, err := test.evaluate(profile, selected, "result", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(templates) != 1 {
				t.Fatalf("command template count = %d, want one", len(templates))
			}
			if got := len(shellCommands); got != 1 {
				t.Fatalf("probe evaluator calls = %d, want one unsupported classification", got)
			}
			tokens := kbuildDeferredContentTokenPattern.FindAllString(templates[0].Text, -1)
			if len(tokens) != 1 {
				t.Fatalf("template = %q, want one deferred-content token", templates[0].Text)
			}
			query, ok := profile.deferredContentQueries[tokens[0]]
			if !ok || query.Target != "result" || query.Transform != ActionRecipeContentTransformMakeShellSingleWord {
				t.Fatalf("deferred query = %#v, want result-scoped shell-word query", query)
			}
			for _, want := range []string{
				"cat first second",
				"printf " + tokens[0],
				"> result",
			} {
				if !strings.Contains(templates[0].Text, want) {
					t.Fatalf("template = %q, want fragment %q", templates[0].Text, want)
				}
			}
			for _, want := range []string{
				"dec_size=0; for F in first second;",
				"fsize=$(sh scripts/file-size.sh $F)",
				"dec_size=$(expr $dec_size + $fsize)",
			} {
				if !strings.Contains(query.Command, want) {
					t.Fatalf("query command = %q, want fragment %q", query.Command, want)
				}
			}
		})
	}
}

func TestKbuildCommandTemplatesRouteSourceShellBeforeDeferringRecipeShell(t *testing.T) {
	const makefile = `
source_flags = $(shell grep -Ev '^#|^$$' source.parameters)
runtime_value = $(shell runtime-query $@)
cmd_render = render $(source_flags) $(runtime_value) -o $@
result: FORCE
	$(call if_changed,render)
`
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", makefile, nil)
	var selected KbuildRule
	for _, rule := range profile.Rules {
		if slices.Contains(rule.Targets, "result") {
			selected = rule
			break
		}
	}
	if len(selected.Recipe) == 0 {
		t.Fatal("result rule has no recipe")
	}

	var compilerCommands, sourceCommands []string
	profile.evaluator.template.shell = func(command string) (string, error) {
		compilerCommands = append(compilerCommands, command)
		return "", &LinuxProbeUnsupportedCommandError{Command: command}
	}
	profile.evaluator.template.sourceShell = func(command, _ string) (string, error) {
		sourceCommands = append(sourceCommands, command)
		if command == "grep -Ev '^#|^$' source.parameters" {
			return "--from-source\n", nil
		}
		return "", &LinuxProbeUnsupportedCommandError{Command: command}
	}

	templates, err := EvaluateCompactKbuildCommandTemplatesSymbolic(profile, selected, "result", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 {
		t.Fatalf("command template count = %d, want one", len(templates))
	}
	if got, want := compilerCommands, []string{"grep -Ev '^#|^$' source.parameters", "runtime-query result"}; !slices.Equal(got, want) {
		t.Fatalf("compiler shell commands = %#v, want %#v", got, want)
	}
	if !slices.Equal(sourceCommands, compilerCommands) {
		t.Fatalf("source shell commands = %#v, want %#v", sourceCommands, compilerCommands)
	}
	if !strings.Contains(templates[0].Text, "render --from-source ") {
		t.Fatalf("template = %q, want source query output", templates[0].Text)
	}
	tokens := kbuildDeferredContentTokenPattern.FindAllString(templates[0].Text, -1)
	if len(tokens) != 1 {
		t.Fatalf("template = %q, want exactly one deferred-content token", templates[0].Text)
	}
	if got := len(profile.deferredContentQueries); got != 1 {
		t.Fatalf("deferred query count = %d, want one", got)
	}
	query, ok := profile.deferredContentQueries[tokens[0]]
	if !ok || query.Command != "runtime-query result" || query.Target != "result" {
		t.Fatalf("deferred query = %#v, want only the runtime query", query)
	}
}

func TestKbuildCommandTemplatesExposeSelectedImmutableSourceScripts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"first.sh":  "#!/bin/sh\n${MAKE} -f \"${srctree}/scripts/child.mk\" first\n",
		"second.sh": "#!/bin/sh\necho second\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "scripts", name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makefile := `
CONFIG_SHELL = sh
cmd_selected = $(CONFIG_SHELL) $(srctree)/scripts/first.sh --one; $(srctree)/scripts/second.sh --two
result: scripts/first.sh scripts/second.sh FORCE
	$(call if_changed,selected)
`
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), filepath.Join(root, "Makefile"), KbuildOptions{
		Variables: map[string]string{"srctree": root},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("selected", filepath.Join(root, "Makefile"), root, kb)
	if err != nil {
		t.Fatal(err)
	}
	var selected KbuildRule
	for _, rule := range profile.Rules {
		if slices.Contains(rule.Targets, "result") {
			selected = rule
			break
		}
	}
	if len(selected.Recipe) == 0 {
		t.Fatalf("parsed rules omit selected result recipe: %#v", profile.Rules)
	}
	templates, err := EvaluateCompactKbuildCommandTemplates(profile, selected, "result", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(templates), 1; got != want || templates[0].Name != "selected" {
		t.Fatalf("selected templates = %#v, want one cmd_selected expansion", templates)
	}
	scripts, err := ReadCompactKbuildCommandSourceScripts(
		profile, "result", "", selected.Prerequisites, selected.OrderOnly, nil, templates[0].Text,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(scripts), 2; got != want {
		t.Fatalf("selected scripts = %#v, want %d", scripts, want)
	}
	if got, want := []string{scripts[0].Path, scripts[1].Path}, []string{"scripts/first.sh", "scripts/second.sh"}; !slices.Equal(got, want) {
		t.Fatalf("selected script paths = %q, want %q", got, want)
	}
	if !strings.Contains(scripts[0].Content, `${MAKE} -f "${srctree}/scripts/child.mk"`) {
		t.Fatalf("first source script content = %q", scripts[0].Content)
	}
}

func TestKbuildCommandSourceScriptsDiscoverExtensionlessShellHelpers(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"direct-helper":      "#!/bin/sh\nprintf '%s\\n' direct\n",
		"interpreted-helper": "printf '%s\\n' interpreted\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "scripts", name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makefile := `
CONFIG_SHELL = sh
result: FORCE
	@true
`
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), filepath.Join(root, "Makefile"), KbuildOptions{
		Variables: map[string]string{"srctree": root},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("extensionless", filepath.Join(root, "Makefile"), root, kb)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := ReadCompactKbuildCommandSourceScripts(
		profile, "result", "", nil, nil, nil,
		`${tree:kernel}/scripts/direct-helper; sh ${tree:kernel}/scripts/interpreted-helper`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(scripts), 2; got != want {
		t.Fatalf("extensionless source scripts=%#v, want %d", scripts, want)
	}
	if got, want := []string{scripts[0].Path, scripts[1].Path}, []string{"scripts/direct-helper", "scripts/interpreted-helper"}; !slices.Equal(got, want) {
		t.Fatalf("extensionless source scripts=%q, want %q", got, want)
	}
}

func TestKbuildCommandSourceScriptsConsumeValuedShellOptions(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(
		t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	for name, command := range map[string]string{
		"split short option":    `sh -o pipefail ${tree:kernel}/scripts/transform.sh -c input.c`,
		"combined short option": `sh -eo pipefail ${tree:kernel}/scripts/transform.sh -c input.c`,
		"plus option":           `sh +O extglob ${tree:kernel}/scripts/transform.sh`,
	} {
		t.Run(name, func(t *testing.T) {
			scripts, err := ReadCompactKbuildCommandSourceScripts(
				profile, "generated/result.h", "", nil, nil, nil, command,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(scripts) != 1 || scripts[0].Path != "scripts/transform.sh" {
				t.Fatalf("source scripts = %#v, want only scripts/transform.sh", scripts)
			}
		})
	}
	for name, command := range map[string]string{
		"command":                   `sh -o pipefail -c ${tree:kernel}/scripts/transform.sh`,
		"stdin":                     `sh -o pipefail -s ${tree:kernel}/scripts/transform.sh`,
		"split startup file":        `sh --rcfile scripts/bashrc ${tree:kernel}/scripts/transform.sh`,
		"equals startup file":       `sh --init-file=scripts/bashrc ${tree:kernel}/scripts/transform.sh`,
		"unknown long option arity": `sh --startup-file scripts/bashrc ${tree:kernel}/scripts/transform.sh`,
	} {
		t.Run(name, func(t *testing.T) {
			scripts, err := ReadCompactKbuildCommandSourceScripts(
				profile, "generated/result.h", "", nil, nil, nil, command,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(scripts) != 0 {
				t.Fatalf("non-file shell mode discovered source scripts: %#v", scripts)
			}
		})
	}
}

func TestReadCompactKbuildProfileSourceSupportsDeclaredSymlinkForest(t *testing.T) {
	physical := t.TempDir()
	if err := os.WriteFile(filepath.Join(physical, "selected.sh"), []byte("#!/bin/sh\necho selected\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(physical, "selected.sh"), filepath.Join(root, "scripts", "selected.sh")); err != nil {
		t.Fatal(err)
	}
	profile := CompactKbuildProfile{
		Name: "symlink-forest",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
			sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		}},
	}
	content, err := readCompactKbuildProfileSource(profile, "scripts/selected.sh")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "#!/bin/sh\necho selected\n"; got != want {
		t.Fatalf("source script content = %q, want %q", got, want)
	}
}

func TestReadCompactKbuildProfileSourceRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	profile := CompactKbuildProfile{
		Name: "traversal",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
			sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		}},
	}
	if _, err := readCompactKbuildProfileSource(profile, "../outside.sh"); err == nil || !strings.Contains(err.Error(), "not a canonical relative path") {
		t.Fatalf("read traversal error = %v, want canonical relative path rejection", err)
	}
}
