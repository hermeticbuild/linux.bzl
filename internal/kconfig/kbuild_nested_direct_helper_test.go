package kconfig

import (
	"strings"
	"testing"
)

func TestKbuildNestedRuleLowersExactFilechkHelper(t *testing.T) {
	const (
		target = "arch/arm64/kvm/hyp_constants.h"
		source = `
filechk = $(filechk_$(1))
define filechk_offsets
	echo "#ifndef $2"; \
	echo "#define $2"; \
	echo "#endif"
endef
if_changed_rule = $(rule_$(1))

define rule_gen_hyp_constants
	$(call filechk,offsets,__HYP_CONSTANTS_H__)
endef

arch/arm64/kvm/hyp_constants.h: arch/arm64/kvm/hyp-constants.s FORCE
	$(call if_changed_rule,gen_hyp_constants)
`
	)
	profile := mustCompactKbuildProfileForTest(
		t, "build:arch/arm64/kvm", "arch/arm64/kvm/Makefile", "arch/arm64/kvm", source, nil,
	)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "arch/arm64/kvm/hyp-constants.s")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("nested filechk has no %q producer: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("nested filechk producer %q is missing", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	joined := strings.Join(recipe.Arguments, "\n")
	for _, line := range []string{
		"#ifndef __HYP_CONSTANTS_H__",
		"#define __HYP_CONSTANTS_H__",
		"#endif",
	} {
		if !strings.Contains(joined, line) {
			t.Errorf("nested filechk recipe lost %q: %#v", line, recipe)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("nested filechk plan is invalid: %v", err)
	}
}

func TestKbuildConditionalRuleMkdirDoesNotHideSelectedCommand(t *testing.T) {
	const (
		target = "generated/sub/result.o"
		source = `
quiet := quiet_
echo-cmd = $(if $($(quiet)cmd_$(1)),echo '  $($(quiet)cmd_$(1))';)
quiet_cmd_mkdir = MKDIR $(dir $@)
cmd_mkdir = mkdir -p $(dir $@)
rule_mkdir = $(if $(wildcard $(dir $@)),,@$(call echo-cmd,mkdir) $(cmd_mkdir))
if_changed = $(cmd_$(1))
cmd_cc_o_c = $(CC) -c -o $@ $<

generated/sub/result.o: input.c FORCE
	$(call rule_mkdir)
	$(call if_changed,cc_o_c)
`
	)
	profile := mustCompactKbuildProfileForTest(
		t, "build:tools", "tools/build/Makefile.build", "", source,
		map[string]string{"CC": KbuildActionRoleToken("target", "cc")},
	)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.c")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("conditional rule_mkdir target has no producer: %#v", plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "cc" {
		t.Fatalf("conditional rule_mkdir selected node = %#v, want host cc", node)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("conditional rule_mkdir plan is invalid: %v", err)
	}
}

func TestKbuildDirectOccurrencePreservesSourceAndExecutableText(t *testing.T) {
	const source = `
opaque_helper = opaque-tool --emit untracked.side
if_changed = $(cmd_$(1))
cmd_emit = cp $< $@

result: input.txt FORCE
	$(call opaque_helper)
	$(call if_changed,emit)
`
	profile := mustCompactKbuildProfileForTest(t, "build:opaque", "Makefile", "", source, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "result")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(match.commandTemplates) != 2 {
		t.Fatalf("direct occurrence selection = %#v found=%t, want direct then emit", match.commandTemplates, found)
	}
	if direct := match.commandTemplates[0]; direct.Name != "" || direct.Source != "$(call opaque_helper)" ||
		direct.Text != "opaque-tool --emit untracked.side" {
		t.Fatalf("direct occurrence = %#v, want exact source plus evaluated executable text", direct)
	}
	if named := match.commandTemplates[1]; named.Name != "emit" || named.Source != "" || named.Text != "cp input.txt result" {
		t.Fatalf("named occurrence = %#v, want evaluated cmd_emit", named)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	err = buildCompactKbuildTargetForTest(metadata, plan, "result")
	if err == nil || !strings.Contains(err.Error(), "variables direct[0],cmd_emit") ||
		strings.Contains(err.Error(), "safe generic helper lowering") {
		t.Fatalf("unconfigured direct executable error = %v, want final action-binding failure", err)
	}
}

func TestKbuildNestedFilechkAndCommandTemplateUseOrderedGenericLowering(t *testing.T) {
	const source = `
filechk = $(filechk_$(1)) > $@
filechk_header = echo '\#define GENERATED 1'
cmd = $(cmd_$(1))
if_changed_rule = $(rule_$(1))
cmd_notify = echo generated $@

define rule_mixed
	$(call filechk,header)
	$(call cmd,notify)
endef

result: FORCE
	$(call if_changed_rule,mixed)
`
	profile := mustCompactKbuildProfileForTest(t, "build:mixed", "Makefile", "", source, nil)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "result")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(match.commandTemplates) != 2 || match.commandTemplates[0].Name != "" ||
		match.commandTemplates[0].Source != "$(call filechk,header)" || match.commandTemplates[1].Name != "notify" {
		t.Fatalf("mixed filechk selection = %#v found=%t, want direct filechk then notify", match.commandTemplates, found)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "result"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := planProducerByOutput(plan, "objects", "result"); !ok {
		t.Fatalf("mixed filechk sequence has no result producer: %#v", plan.Nodes)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("mixed filechk plan is invalid: %v", err)
	}
}

func TestKbuildCommandAndDirectTailRemainInSourceOrder(t *testing.T) {
	const source = `
if_changed = $(cmd_$(1))
cmd_image = cp $< $@

arch/x86/boot/bzImage: vmlinux FORCE
	$(call if_changed,image)
	@echo 'Kernel: $@ is ready'
`
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/boot", "arch/x86/boot/Makefile", "arch/x86/boot", source, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "vmlinux")
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, "arch/x86/boot/bzImage")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(match.commandTemplates) != 2 || match.commandTemplates[0].Name != "image" ||
		match.commandTemplates[1].Name != "" || match.commandTemplates[1].Source != "@echo 'Kernel: $@ is ready'" ||
		match.commandTemplates[1].Text != "echo 'Kernel: arch/x86/boot/bzImage is ready'" {
		t.Fatalf("bzImage occurrence order = %#v found=%t", match.commandTemplates, found)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "arch/x86/boot/bzImage"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := planProducerByOutput(plan, "objects", "arch/x86/boot/bzImage"); !ok {
		t.Fatalf("bzImage direct tail lost image producer: %#v", plan.Nodes)
	}
}
