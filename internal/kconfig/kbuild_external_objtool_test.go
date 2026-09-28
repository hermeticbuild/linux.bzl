package kconfig

import (
	"slices"
	"strings"
	"testing"
)

// The policy expressions and composite rule are copied from Linux 6.18.39
// scripts/Makefile.lib / Makefile.build. This fixture fixes only the source-
// derived post-real-search object lists and uses lightweight savecmd wrappers.
// In particular obj="." models the external invocation's local Make words.
const externalObjtoolSourceForTest = `
obj-m := demo.o solo.o
multi-obj-m := demo.o
real-obj-m := hello_module.o solo.o
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
part-of-builtin = $(if $(filter $(basename $@).o, $(real-obj-y) $(lib-y)),y)
part-of-module = $(if $(filter $(basename $@).o, $(real-obj-m)),y)
is-kernel-object = $(or $(part-of-builtin),$(part-of-module))
is-standard-object = $(if $(filter-out y%, $(OBJECT_FILES_NON_STANDARD_$(target-stem).o)$(OBJECT_FILES_NON_STANDARD)n),$(is-kernel-object))
is-single-obj-m = $(and $(part-of-module),$(filter $@, $(obj-m)),y)

objtool := $(objtree)/tools/objtool/objtool
objtool-args-$(CONFIG_X86_KERNEL_IBT) += --ibt
objtool-args-$(CONFIG_MITIGATION_RETHUNK) += --rethunk
objtool-args = $(objtool-args-y) $(if $(delay-objtool), --link) $(if $(part-of-module), --module)
delay-objtool := $(or $(CONFIG_LTO_CLANG),$(CONFIG_X86_KERNEL_IBT))
cmd_objtool = $(if $(objtool-enabled), ; $(objtool) $(objtool-args) $@)
cmd_gen_objtooldep = $(if $(objtool-enabled), { echo ; echo '$@: $$(wildcard $(objtool))' ; } >> $(dot-target).cmd)
objtool-enabled := y
$(obj)/%.o: private objtool-enabled = $(if $(is-standard-object),$(if $(delay-objtool),$(is-single-obj-m),y))

dot-target = $(dir $@).$(notdir $@)
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(cmd_$(1))
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if_changed_rule = $(rule_$(1))
cmd_cc_o_c = $(CC) -c -o $@ $< $(cmd_objtool)
rule_cc_o_c = $(call cmd_and_savecmd,cc_o_c)
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_rule,cc_o_c)

cmd_ld_multi_m = $(LD) $(ld_flags) -r -o $@ @$< $(cmd_objtool)
define rule_ld_multi_m
	$(call cmd_and_savecmd,ld_multi_m)
	$(call cmd,gen_objtooldep)
endef
$(multi-obj-m): private objtool-enabled := $(delay-objtool)
$(multi-obj-m): private part-of-module := y
$(multi-obj-m): %.o: %.mod FORCE
	$(call if_changed_rule,ld_multi_m)
demo.o: hello_module.o

# Synthetic downstream consumer, solely to check which aggregate value flows.
cmd_consume = $(LD) -r -o $@ $<
demo.ko: demo.o
	$(call cmd,consume)
.PHONY: FORCE
FORCE:
`

const externalObjtoolDirectoryForTest = ".linux-bzl/external/demo"

func externalObjtoolProfileForTest(t *testing.T, ibt, nonstandard, soloOverride string) CompactKbuildProfile {
	t.Helper()
	profile := mustCompactKbuildProfileForTest(t, "external-objtool", "scripts/Makefile.build", externalObjtoolDirectoryForTest, externalObjtoolSourceForTest, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "LD": KbuildActionRoleToken("target", "ld"),
		"obj": ".", "objtree": "__LINUX_BZL_OBJECT_TREE__",
		"CONFIG_OBJTOOL": "y", "CONFIG_MITIGATION_RETHUNK": "y",
		"CONFIG_X86_KERNEL_IBT": ibt, "CONFIG_LTO_NONE": "y", "CONFIG_LTO_CLANG": "",
		"OBJECT_FILES_NON_STANDARD": nonstandard, "OBJECT_FILES_NON_STANDARD_solo.o": soloOverride,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile,
		externalObjtoolDirectoryForTest+"/hello_module.c",
		externalObjtoolDirectoryForTest+"/solo.c",
		externalObjtoolDirectoryForTest+"/other.c",
		externalObjtoolDirectoryForTest+"/demo.mod",
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: externalObjtoolDirectoryForTest,
	}); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestExternalObjtoolNativeSelectionAndPrivateRoots(t *testing.T) {
	for _, tc := range []struct {
		name, ibt, nonstandard, soloOverride, word string
		enabled                                    bool
	}{
		{"ibt_composite_member", "y", "", "", "hello_module.o", false},
		{"ibt_composite_link", "y", "", "", "demo.o", true},
		{"ibt_single", "y", "", "", "solo.o", true},
		{"ibt_nonmember", "y", "", "", "other.o", false},
		{"plain_composite_member", "", "", "", "hello_module.o", true},
		{"plain_composite_link", "", "", "", "demo.o", false},
		{"plain_single", "", "", "", "solo.o", true},
		{"plain_nonmember", "", "", "", "other.o", false},
		{"ibt_directory_nonstandard_single", "y", "y", "", "solo.o", false},
		{"ibt_directory_nonstandard_composite_link", "y", "y", "", "demo.o", true},
		{"plain_directory_nonstandard_member", "", "y", "", "hello_module.o", false},
		{"ibt_file_nonstandard_single", "y", "", "y", "solo.o", false},
		{"ibt_file_override_directory", "y", "y", "n", "solo.o", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := externalObjtoolProfileForTest(t, tc.ibt, tc.nonstandard, tc.soloOverride)
			target := externalObjtoolDirectoryForTest + "/" + tc.word
			metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
			match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
			if err != nil || !found {
				t.Fatalf("selected match: found=%t err=%v", found, err)
			}
			for _, private := range []bool{false, true} {
				injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
				if err != nil {
					t.Fatal(err)
				}
				if private {
					injected = compactKbuildActionTreeInjections(injected)
				}
				automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
				if err != nil {
					t.Fatal(err)
				}
				values, err := evaluateCompactKbuildTargetForMakeTarget(profile, target, match.lookupTarget,
					automatic.target, automatic.stem, automatic.normal, automatic.order, injected, true,
					"objtool-enabled", "cmd_objtool", "part-of-module", "delay-objtool")
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSpace(values["objtool-enabled"]) != ""; got != tc.enabled {
					t.Fatalf("private=%t enabled=%t want=%t values=%q", private, got, tc.enabled, values)
				}
				if got := strings.Contains(values["cmd_objtool"], "tools/objtool/objtool"); got != tc.enabled {
					t.Fatalf("private=%t emitted objtool=%t want=%t", private, got, tc.enabled)
				}
				if tc.enabled {
					words := strings.Fields(values["cmd_objtool"])
					if !slices.Contains(words, "--rethunk") || !slices.Contains(words, "--module") ||
						slices.Contains(words, "--link") != (tc.ibt == "y") {
						t.Fatalf("private=%t source-derived objtool options=%q", private, words)
					}
				}
			}
		})
	}
}

func TestExternalObjtoolPostLinkMutationReachesConsumer(t *testing.T) {
	for _, ibt := range []string{"", "y"} {
		t.Run("ibt="+ibt, func(t *testing.T) {
			profile := externalObjtoolProfileForTest(t, ibt, "", "")
			metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles, Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			seed := func(stage, tree, pathname string) string {
				t.Helper()
				id, err := appendActionPlanNode(plan, ActionPlanNode{
					Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
					Outputs: []ActionPlanOutput{{Tree: tree, Path: pathname}},
				}, ActionRecipe{
					Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
					Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
				})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			objtoolID := seed("host", "host", "tools/objtool/objtool")
			seed("target", "objects", externalObjtoolDirectoryForTest+"/hello_module.o")
			seed("target", "objects", externalObjtoolDirectoryForTest+"/demo.mod")
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
			target := externalObjtoolDirectoryForTest + "/demo.o"
			aggregateID, err := builder.build(target)
			if err != nil {
				t.Fatal(err)
			}
			aggregate, ok := compactKbuildPlanNode(plan, aggregateID)
			if !ok || !slices.ContainsFunc(aggregate.Outputs, func(output ActionPlanOutput) bool {
				return output.Path == target && actionPlanOutputIsCanonical(output)
			}) {
				t.Fatalf("aggregate does not publish canonical %s: %#v", target, aggregate)
			}
			// The complete savecmd wrapper is deliberately retained. Check the
			// executing prefix, not the command string quoted in savedcmd metadata.
			script := compactKbuildRecipeScriptContentForTest(t, plan.Recipes[aggregate.Recipe])
			prefix, _, found := strings.Cut(script, "printf '%s")
			if !found {
				t.Fatalf("native savecmd boundary missing: %q", script)
			}
			if got := strings.Contains(prefix, "tools/objtool/objtool"); got != (ibt == "y") {
				t.Fatalf("executing aggregate objtool=%t want=%t: %q", got, ibt == "y", prefix)
			}
			if ibt == "y" && !slices.ContainsFunc(aggregate.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == objtoolID }) {
				t.Fatal("delayed aggregate omits immutable generated objtool input")
			}
			consumerID, err := builder.build(externalObjtoolDirectoryForTest + "/demo.ko")
			if err != nil {
				t.Fatal(err)
			}
			consumer, ok := compactKbuildPlanNode(plan, consumerID)
			if !ok || !slices.ContainsFunc(consumer.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == aggregateID }) {
				t.Fatal("downstream link bypasses final aggregate producer")
			}
		})
	}
}
