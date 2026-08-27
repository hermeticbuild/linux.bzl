package kconfig

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func compactKbuildScriptProfileForTest(t *testing.T, command string) CompactKbuildProfile {
	return compactKbuildScriptProfileWithExportsForTest(t, command)
}

func compactKbuildScriptProfileWithExportsForTest(t *testing.T, command string, exports ...string) CompactKbuildProfile {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "transform.sh"), []byte("#!/bin/sh\nawk '{ print }'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	additionalExports := ""
	if len(exports) != 0 {
		additionalExports = "export " + strings.Join(exports, " ") + "\n"
	}
	makefile := `
CONFIG_SHELL = sh
sub_make_done = 1
AWK = ` + KbuildActionRoleToken("target", "awk") + `
CC = ` + KbuildActionRoleToken("target", "cc") + `
HOSTCC = ` + KbuildActionRoleToken("host", "cc") + `
FROBNICATOR = ` + KbuildActionRoleToken("target", "frobnicator") + `
export HOSTCC
export SCRIPT_OBJECT_ROOT = $(objtree)
export SCRIPT_SOURCE_ROOT = $(srctree)
` + additionalExports + `cmd_transform = ` + command + `
generated/result.h: input.txt scripts/transform.sh FORCE
	$(call if_changed,transform)
`
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", KbuildOptions{
		Variables: map[string]string{
			"srctree": root,
		},
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
	profile, err := NewCompactKbuildProfile("prep", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestKbuildSourceScriptUsesHermeticRuntimeAndToolClosure(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t,
		`FROBNICATOR=$(FROBNICATOR) $(CONFIG_SHELL) $(srctree)/scripts/transform.sh --mode exact $(FROBNICATOR) < $< > $@`,
	)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("frobnicator"),
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Recipes: map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want one source-script action: %#v", got, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("source-script tool=%q/%q, want %q", node.Tool, recipe.Tool, compactKbuildScriptRunnerRole)
	}
	wantTools := []string{compactKbuildScriptRuntimeRole, "frobnicator"}
	if !slices.Equal(node.AuxiliaryTools, wantTools) || !slices.Equal(recipe.AuxiliaryTools, wantTools) {
		t.Fatalf("auxiliary tools node=%q recipe=%q, want %q", node.AuxiliaryTools, recipe.AuxiliaryTools, wantTools)
	}
	if got, want := recipe.Environment["FROBNICATOR"], "frobnicator"; got != want {
		t.Fatalf("FROBNICATOR environment=%q, want %q", got, want)
	}
	if got, want := recipe.Environment["SCRIPT_OBJECT_ROOT"], "${work:root}"; got != want {
		t.Fatalf("SCRIPT_OBJECT_ROOT environment=%q, want %q", got, want)
	}
	if got, want := recipe.Environment["SCRIPT_SOURCE_ROOT"], "${tree:kernel}"; got != want {
		t.Fatalf("SCRIPT_SOURCE_ROOT environment=%q, want %q", got, want)
	}
	for _, argument := range []string{
		"-interpreter", "${tool:script-runtime}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:script-runtime}",
		"-tool", "script-runtime=${tool:script-runtime}",
		"-tool", "frobnicator=${tool:frobnicator}",
		"--", "--mode", "exact", "frobnicator",
	} {
		if !slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("source-script arguments=%q, want %q", recipe.Arguments, argument)
		}
	}
	if slices.Contains(recipe.Arguments, "/bin/sh") || slices.Contains(recipe.Arguments, "scripts/transform.sh") {
		t.Fatalf("source-script arguments use ambient interpreter or unbound source path: %q", recipe.Arguments)
	}
	if recipe.Stdout != "00000000" || !strings.HasPrefix(recipe.Stdin, "source:stdin:") {
		t.Fatalf("source-script streams stdin=%q stdout=%q", recipe.Stdin, recipe.Stdout)
	}
	var scriptSourceID string
	for _, source := range plan.Sources {
		if source.Namespace == "kernel" && source.Path == "scripts/transform.sh" {
			scriptSourceID = source.ID
		}
	}
	if scriptSourceID == "" {
		t.Fatalf("plan sources=%#v, want immutable kernel source scripts/transform.sh", plan.Sources)
	}
	var scriptBinding string
	for index, source := range node.Sources {
		if source.Role == "script" && source.SourceID == scriptSourceID {
			scriptBinding = "script:" + planOrdinal(index)
		}
	}
	if scriptBinding == "" || !slices.Contains(recipe.Sources, scriptBinding) || !slices.Contains(recipe.Arguments, "${source:"+scriptBinding+"}") {
		t.Fatalf("script source edge=%#v recipe=%#v", node.Sources, recipe)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("canonical plan validation failed: %v", err)
	}
}

func TestKbuildSourceScriptUsesShebangSelectedInterpreterRole(t *testing.T) {
	const (
		directory   = "scripts"
		script      = directory + "/generate"
		target      = "generated/result.h"
		interpreter = "customlang"
	)
	role := compactKbuildScriptAppletRolePrefix + interpreter
	profile := mustCompactKbuildProfileForTest(t, "generate", "Makefile", "", `
cmd_generate = customlang $(srctree)/scripts/generate --mode exact > $@
generated/result.h: scripts/generate FORCE
	$(call cmd,generate)
`, map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__"})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, script)
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, script, "#!/usr/bin/customlang -x\nthis is deliberately not shell syntax {{\n")

	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program:   interpreter,
		arguments: []string{"${tree:kernel}/" + script, "--mode", "exact"},
	}, nil, "target", testTargetActionRoles(role))
	if err != nil {
		t.Fatal(err)
	}
	if !matched || invocation.interpreterRole != role || invocation.interpreter != "" ||
		!slices.Equal(invocation.interpreterArguments, []string{"-x"}) {
		t.Fatalf("generic source interpreter invocation=(%#v,%t), want shebang-selected %q -x without basename dispatch", invocation, matched, role)
	}
	if !invocation.environmentUsage.ObservesAll {
		t.Fatal("opaque non-shell source interpreter did not conservatively observe its complete environment")
	}

	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles(role),
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Recipes: map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("generic source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	var scriptBinding string
	for index, source := range node.Sources {
		if source.Role == "script" {
			scriptBinding = "script:" + planOrdinal(index)
			break
		}
	}
	if scriptBinding == "" {
		t.Fatalf("generic source-script node has no immutable script edge: node=%#v recipe=%#v", node, recipe)
	}
	wantArguments := []string{
		"-interpreter", "${tool:" + role + "}",
		"-interpreter_arg", "-x",
		"-multicall", "${tool:" + compactKbuildScriptRuntimeRole + "}",
		"-tool", compactKbuildScriptRuntimeRole + "=${tool:" + compactKbuildScriptRuntimeRole + "}",
		"-script", "${source:" + scriptBinding + "}",
		"-applet", interpreter + "=${tool:" + role + "}",
		"--", "--mode", "exact",
	}
	if !slices.Equal(recipe.Arguments, wantArguments) {
		t.Fatalf("generic source-script arguments=%q, want exact interpreter argv %q", recipe.Arguments, wantArguments)
	}
	if slices.Contains(recipe.Arguments, interpreter) {
		t.Fatalf("generic source-script arguments inject basename dispatch into direct interpreter: %q", recipe.Arguments)
	}
	if !slices.Contains(node.AuxiliaryTools, role) || !slices.Contains(recipe.AuxiliaryTools, role) {
		t.Fatalf("generic interpreter role missing from tool closure: node=%q recipe=%q", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("generic source-script canonical plan validation failed: %v", err)
	}
}

func TestKbuildSourceScriptExplicitShellConsumesValuedOptions(t *testing.T) {
	const script = "scripts/bash-generator"
	role := compactKbuildScriptAppletRolePrefix + "bash"
	profile := mustCompactKbuildProfileForTest(t, "bash-generator", "Makefile", "", `
generated/result.h: scripts/bash-generator FORCE
	@true
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, script)
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, script, "#!/usr/bin/env -S bash -eo pipefail +O extglob\nprintf '%s\\n' bounded\n")

	arguments := []string{
		"-o", "errexit", "${tree:kernel}/" + script,
		"-c", "input.c",
	}
	match, matched, err := compactKbuildProfileSourceInterpreterCommand(profile, "bash", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || match.scriptPath != script || match.scriptIndex != 2 {
		t.Fatalf("explicit bash match=(%#v,%t), want script index 2", match, matched)
	}
	invocation, matched, err := compactKbuildSourceScriptCommand(
		profile,
		compactKbuildRecipeCommand{program: "bash", arguments: arguments},
		map[string]string{"CONFIG_SHELL": "sh"},
		"target",
		testTargetActionRoles(role),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || invocation.interpreterRole != role || invocation.scriptPath != script {
		t.Fatalf("explicit bash invocation=(%#v,%t), want shebang-selected script", invocation, matched)
	}
	if got, want := invocation.interpreterArguments, []string{
		"-eo", "pipefail", "+O", "extglob", "-o", "errexit",
	}; !slices.Equal(got, want) {
		t.Fatalf("explicit bash interpreter arguments=%q, want %q", got, want)
	}
	if got, want := invocation.scriptArguments, []string{"-c", "input.c"}; !slices.Equal(got, want) {
		t.Fatalf("explicit bash script arguments=%q, want %q", got, want)
	}
}

func TestKbuildSourceScriptActionExportsCanonicalRootAliases(t *testing.T) {
	aliases := []string{"abs_output", "abs_srctree", "objtree", "srcroot", "srctree", "sub_make_done"}
	profile := compactKbuildScriptProfileWithExportsForTest(
		t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
		aliases...,
	)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("awk", "cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	want := map[string]string{
		"abs_output":    "${work:root}",
		"abs_srctree":   "${tree:kernel}",
		"objtree":       "${work:root}",
		"srcroot":       "${tree:kernel}",
		"srctree":       "${tree:kernel}",
		"sub_make_done": "1",
	}
	for _, name := range aliases {
		if got := recipe.Environment[name]; got != want[name] {
			t.Errorf("source-script action environment %s=%q, want %q; environment=%#v", name, got, want[name], recipe.Environment)
		}
	}
}

func TestKbuildSourceScriptInstallsToolchainRuntimeAppletOverrides(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	metadata := &CompactMetadata{
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles(
			"cc",
			compactKbuildScriptAppletRolePrefix+"find",
		),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	role := compactKbuildScriptAppletRolePrefix + "find"
	if !slices.Contains(node.AuxiliaryTools, role) || !slices.Contains(recipe.AuxiliaryTools, role) {
		t.Fatalf("runtime applet binding missing: node=%q recipe=%q", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
	want := "find=${tool:" + role + "}"
	for index, argument := range recipe.Arguments {
		if argument == "-applet" && index+1 < len(recipe.Arguments) && recipe.Arguments[index+1] == want {
			return
		}
	}
	t.Fatalf("source-script arguments=%q, want -applet %q", recipe.Arguments, want)
}

func TestKbuildBareCommandSelectsToolchainRuntimeAppletOverride(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `find $< > $@`)
	role := compactKbuildScriptAppletRolePrefix + "find"
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("cc", role),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("runtime applet producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != role || recipe.Tool != role {
		t.Fatalf("bare find selected %q/%q, want runtime applet role %q", node.Tool, recipe.Tool, role)
	}
	if len(recipe.Arguments) == 0 || recipe.Arguments[0] != "find" {
		t.Fatalf("recipe arguments=%q, want source-selected multicall dispatch name", recipe.Arguments)
	}
}

func TestKbuildSourceScriptPlanOmitsUnusedHostCompilerCapability(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if _, exists := recipe.Environment["HOSTCC"]; exists {
		t.Fatalf("unused host compiler leaked into target source-script environment: %#v", recipe.Environment)
	}
	if slices.Contains(recipe.AuxiliaryTools, "cc") {
		t.Fatalf("unused host compiler acquired a tool binding: %q", recipe.AuxiliaryTools)
	}
	if !slices.Contains(recipe.Arguments, "script-runtime=${tool:script-runtime}") {
		t.Fatalf("source-script runtime contract is not paired with a tool binding: %q", recipe.Arguments)
	}
}

func TestKbuildSourceScriptBindsSourceDiscoveredBareConfiguredProgram(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(
		t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("awk", "cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(recipe.AuxiliaryTools, "awk") || !slices.Contains(node.AuxiliaryTools, "awk") {
		t.Fatalf("source-discovered awk binding missing: node=%q recipe=%q", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
	if !slices.Contains(recipe.Arguments, "awk=${tool:awk}") {
		t.Fatalf("scriptrun arguments omit configured awk proxy: %q", recipe.Arguments)
	}
}

func TestKbuildSourceScriptWithoutOutputArgumentOwnsRuleTarget(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(srctree)/scripts/transform.sh`)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("producer %q is missing", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if got := recipe.WorkingOutputs["00000000"]; got != "generated/result.h" {
		t.Fatalf("implicit source-script output=%q, want target", got)
	}
}

func TestKbuildSourceScriptReplaysDiscoveredRecursiveMakeDependency(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(srctree)/scripts/transform.sh > $@`)
	profile.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "generated/result.h", Profile: "build:init", Goals: []string{"init/version-timestamp.o"},
		ReplayArguments: []string{"-f", "__LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build", "obj=init", "init/version-timestamp.o"},
	}}
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	childRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	childNode := ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "init/version-timestamp.o"}},
	}
	if _, err := appendActionPlanNode(plan, childNode, childRecipe); err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("producer %q is missing", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.Environment["MAKE"] != "make" || len(recipe.CommandReplays) != 1 {
		t.Fatalf("source-script replay environment=%#v replays=%#v", recipe.Environment, recipe.CommandReplays)
	}
	replay := recipe.CommandReplays[0]
	if replay.Name != "make" || len(replay.Invocations) != 1 {
		t.Fatalf("command replay=%#v", replay)
	}
	wantArguments := []string{"-f", "${tree:kernel}/scripts/Makefile.build", "obj=init", "init/version-timestamp.o"}
	if !slices.Equal(replay.Invocations[0].Arguments, wantArguments) ||
		!slices.Equal(replay.Invocations[0].Outputs, []string{"init/version-timestamp.o"}) {
		t.Fatalf("command replay invocation=%#v", replay.Invocations[0])
	}
	foundStagedGoal := false
	for _, pathname := range recipe.WorkingInputs {
		foundStagedGoal = foundStagedGoal || pathname == "init/version-timestamp.o"
	}
	if !foundStagedGoal {
		t.Fatalf("working inputs=%#v, want recursive Make result", recipe.WorkingInputs)
	}
	foundNativeGoalEdge := false
	for _, input := range node.Inputs {
		if input.ProducerID == plan.Nodes[0].ID {
			foundNativeGoalEdge = input.Role != compactKbuildWorkingClosureInputRole
		}
	}
	if !foundNativeGoalEdge {
		t.Fatalf("source-script inputs=%#v, want recursive Make goal as native lineage", node.Inputs)
	}
}

func TestKbuildSourceScriptStagesTerminalOfPhonyRecursiveGoal(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(srctree)/scripts/transform.sh > $@`)
	child := mustCompactKbuildProfileForTest(t, "build:init", "scripts/Makefile.build", "init", `
.PHONY: init/__build
init/__build: init/version-timestamp.o
cmd_emit = touch $@
init/version-timestamp.o: FORCE
	$(call if_changed,emit)
`, nil)
	profile.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "generated/result.h", Profile: child.Name, Goals: []string{"init/__build"},
		ReplayArguments: []string{"-f", "__LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build", "obj=init", "init/__build"},
	}}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile, child},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: child.Name, Target: "init/version-timestamp.o", MakeTarget: "init/version-timestamp.o", Lifecycle: "target", Scope: "target", Stage: "target",
		}},
	}
	metadata := &CompactMetadata{
		Config: config, actionRoles: testTargetActionRoles("cc"),
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	childRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	childNode := ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "init/version-timestamp.o"}},
	}
	if _, err := appendActionPlanNode(plan, childNode, childRecipe); err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("producer %q is missing", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), "init/version-timestamp.o") {
		t.Fatalf("working inputs=%#v, want materialized terminal behind phony recursive goal", recipe.WorkingInputs)
	}
	if !slices.ContainsFunc(node.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == plan.Nodes[0].ID && input.Role != compactKbuildWorkingClosureInputRole
	}) {
		t.Fatalf("source-script inputs=%#v, want terminal producer as native recursive lineage", node.Inputs)
	}
}

func TestKbuildSourceScriptClosureStagesAncestorsAndVisibleFrontier(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(srctree)/scripts/transform.sh > $@`)
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: "include/generated/asm/types.h", Profile: "headers", Target: "include/generated/asm/types.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})
	producer := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "target", Kind: "compile",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "leaf.o"}},
	}
	ancestor := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "archive",
		Inputs:  []ActionPlanNodeEdge{{Role: "leaf", ProducerID: producer.ID}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "built-in.a"}},
	}
	prep := ActionPlanNode{
		ID: strings.Repeat("c", 64), Stage: "prep", Kind: "copy",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/asm/types.h"}},
	}
	unrelated := ActionPlanNode{
		ID: strings.Repeat("d", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/unrelated.h"}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{producer, ancestor, prep, unrelated}}
	builder := withMaterializedInitialObjectTreeArtifactForTest(
		t, newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan),
		visibleArtifact, "prep", "prep", "target", prep.ID,
	)
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		"generated/result.h", profile,
		[]compactKbuildRuleInput{{path: "built-in.a", producer: ancestor.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		got[input.path] = input
	}
	for _, want := range []string{"built-in.a", "leaf.o", "include/generated/asm/types.h"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("closure paths=%v, want %q", got, want)
		}
	}
	if _, leaked := got["include/generated/unrelated.h"]; leaked {
		t.Fatalf("closure paths=%v include unrelated prep output", got)
	}
	if got["built-in.a"].workingOnly {
		t.Fatal("direct Make prerequisite was classified as working-closure-only")
	}
	for _, want := range []string{"leaf.o", "include/generated/asm/types.h"} {
		if !got[want].workingOnly {
			t.Fatalf("closure input %q was not classified as working-only: %#v", want, got[want])
		}
	}
}

func TestKbuildWorkingTreeClosureDoesNotTraverseExactBaselineOnReentry(t *testing.T) {
	const (
		visiblePath  = "include/generated/asm/types.h"
		siblingPath  = "include/generated/asm/unistd.h"
		ancestorPath = "scripts/basic/fixdep"
	)
	ancestor := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "host", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: ancestorPath}},
	}
	baseline := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "prep", Kind: "generate",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: ancestor.ID,
		}},
		Outputs: []ActionPlanOutput{
			{Tree: "prep", Path: visiblePath},
			{Tree: "prep", Path: siblingPath},
		},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{ancestor, baseline}}
	artifact := CompactKbuildVisibleArtifact{
		Path: visiblePath, Profile: "headers", Target: visiblePath,
	}
	profile := CompactKbuildProfile{Name: "consumer"}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{artifact})
	builder := withMaterializedInitialObjectTreeArtifactForTest(
		t,
		newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
			forOutput("target", "objects", "vmlinux"),
		artifact,
		"prep", "prep", "target",
		baseline.ID,
	)
	first, err := builder.compactKbuildWorkingTreeClosureInputs("consumer.out", profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].path != visiblePath || first[0].producer != baseline.ID || first[0].slot != 0 || !first[0].workingOnly {
		t.Fatalf("initial exact baseline = %#v, want only slot 0 from %q", first, baseline.ID)
	}
	second, err := builder.compactKbuildWorkingTreeClosureInputs("consumer.out", profile, first)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second, first) {
		t.Fatalf("repeated exact baseline closure = %#v, want stable %#v", second, first)
	}
}

func TestKbuildWorkingTreeClosureUsesRecordedSamePathProducer(t *testing.T) {
	const pathname = "generated/shared.h"
	artifact := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: "last-writer", Target: pathname,
	}
	consumer := CompactKbuildProfile{Name: "consumer"}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []CompactKbuildVisibleArtifact{artifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			{Name: "first-writer", Path: "scripts/first.mk", EntryTargets: []string{pathname}},
			{Name: "last-writer", Path: "scripts/last.mk", EntryTargets: []string{pathname}},
		},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: "first-writer", Target: pathname, MakeTarget: pathname, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: "last-writer", Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	first := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	last := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	if err := graph.recordMaterializedProducer(
		compactKbuildSelectionKey{profile: "first-writer", target: pathname, stage: "prep"}, first.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(
		compactKbuildSelectionKey{profile: "last-writer", target: pathname, stage: "target"}, last.ID,
	); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{first, last}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forOutput("target", "objects", "vmlinux").
		withInitialObjectTree(true, artifact)
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs("consumer.out", consumer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].producer != last.ID || inputs[0].slot != 0 || !inputs[0].workingOnly {
		t.Fatalf("exact initial working-tree inputs = %#v, want recorded target-stage writer %q", inputs, last.ID)
	}
}

func TestKbuildWorkingTreeClosureUsesRecordedVersionAcrossIncomparableSnapshots(t *testing.T) {
	pathname := "rust/" + linuxProbeSymbolPrefix + strings.Repeat("1", 64)
	const (
		dependencyPath = "rust/compiler_builtins.o"
		consumerPath   = "rust/pin_init.o"
	)
	hostProfile := CompactKbuildProfile{
		Name: "build:rust-host", Path: "scripts/Makefile.build",
		EntryTargets: []string{pathname},
	}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: "build:rust-target", Target: pathname,
	}
	targetProfile := CompactKbuildProfile{
		Name: "build:rust-target", Path: "scripts/Makefile.build",
		EntryTargets: []string{pathname},
	}
	consumerProfile := CompactKbuildProfile{
		Name: "build:rust-consumer", Path: "scripts/Makefile.build",
		EntryTargets: []string{consumerPath},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumerProfile, []CompactKbuildVisibleArtifact{visibleArtifact})
	consumerSelection := CompactKbuildSelection{
		Profile: consumerProfile.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target",
		UsesInitialObjectTree: true,
		InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
			[]CompactKbuildVisibleArtifact{visibleArtifact},
		),
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{hostProfile, targetProfile, consumerProfile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: hostProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: targetProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
			consumerSelection,
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	hostWriter := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "host", Kind: "generate",
		Outputs: []ActionPlanOutput{{
			Tree: "host", Path: pathname,
			ArtifactPath: ".linux-bzl-intermediate/host/command-00000002",
		}},
	}
	dependency := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: hostWriter.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: dependencyPath}},
	}
	targetWriter := ActionPlanNode{
		ID: strings.Repeat("c", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{{
			Tree: "objects", Path: pathname,
			ArtifactPath: ".linux-bzl-intermediate/target/command-00000002",
		}},
	}
	hostKey := compactKbuildSelectionKey{
		profile: hostProfile.Name, target: pathname, stage: "host",
	}
	targetKey := compactKbuildSelectionKey{
		profile: targetProfile.Name, target: pathname, stage: "target",
	}
	consumerKey := compactKbuildSelectionKey{
		profile: consumerProfile.Name, target: consumerPath, stage: "target",
	}
	if err := graph.recordMaterializedProducer(hostKey, hostWriter.ID); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(targetKey, targetWriter.ID); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{hostWriter, dependency, targetWriter}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, consumerProfile).
		forOutput("target", "objects", "vmlinux").
		withInitialObjectTree(true, visibleArtifact)
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		consumerPath,
		consumerProfile,
		[]compactKbuildRuleInput{
			{path: dependencyPath, producer: dependency.ID},
			{path: pathname, producer: targetWriter.ID},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
		if strings.HasPrefix(input.path, ".linux-bzl-intermediate/") {
			t.Fatalf("working closure exposed physical intermediate path %q", input.path)
		}
	}
	if got := byPath[pathname]; got.producer != targetWriter.ID || got.workingOnly {
		t.Fatalf("recorded Rust filename input = %#v, want native target writer %q", got, targetWriter.ID)
	}
	if got := byPath[dependencyPath]; got.producer != dependency.ID || got.workingOnly {
		t.Fatalf("native Rust dependency input = %#v, want %q", got, dependency.ID)
	}
}

func TestKbuildWorkingTreeClosureExcludesObservedStateEnvelopes(t *testing.T) {
	const logicalPath = "generated/result.h"
	producer := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{
			{Tree: "objects", Path: logicalPath},
			{
				Tree: "objects", Path: ".linux-bzl-state/result.state",
				ObservedPath: "generated/opaque.result",
			},
		},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{producer}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		forOutput("target", "objects", "vmlinux")
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		"generated/consumer.o",
		CompactKbuildProfile{},
		[]compactKbuildRuleInput{{path: logicalPath, producer: producer.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].path != logicalPath || inputs[0].producer != producer.ID {
		t.Fatalf("working closure = %#v, want only logical output %q", inputs, logicalPath)
	}
}

func TestKbuildWorkingTreeClosurePrepSnapshotBeatsLaterTargetLifecycleVersion(t *testing.T) {
	const (
		pathname       = "scripts/basic/fixdep"
		dependencyPath = "rust/compiler_builtins.o"
		consumerPath   = "rust/LINUX_BZL_PROBE_deadbeef"
	)
	prepWriterProfile := CompactKbuildProfile{
		Name: "prep-fixdep", Path: "scripts/Makefile.host", EntryTargets: []string{pathname},
	}
	targetWriterProfile := CompactKbuildProfile{
		Name: "target-fixdep", Path: "scripts/Makefile.host", EntryTargets: []string{pathname},
	}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: pathname, Profile: prepWriterProfile.Name, Target: pathname,
	}
	consumerProfile := CompactKbuildProfile{
		Name: "build:rust-prep", Path: "scripts/Makefile.build",
		EntryTargets: []string{consumerPath},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumerProfile, []CompactKbuildVisibleArtifact{visibleArtifact})
	consumerSelection := CompactKbuildSelection{
		Profile: consumerProfile.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "prep", Scope: "target", Stage: "prep",
		UsesInitialObjectTree: true,
		InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
			[]CompactKbuildVisibleArtifact{visibleArtifact},
		),
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{prepWriterProfile, targetWriterProfile, consumerProfile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: prepWriterProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "prep", Scope: "host", Stage: "host"},
			{Profile: targetWriterProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "host", Stage: "host"},
			consumerSelection,
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	prepWriter := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "host", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: pathname}},
	}
	targetWriter := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "host", Kind: "generate",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: prepWriter.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "host", Path: pathname}},
	}
	dependency := ActionPlanNode{
		ID: strings.Repeat("c", 64), Stage: "prep", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: targetWriter.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: dependencyPath}},
	}
	prepKey := compactKbuildSelectionKey{profile: prepWriterProfile.Name, target: pathname, stage: "host"}
	targetKey := compactKbuildSelectionKey{profile: targetWriterProfile.Name, target: pathname, stage: "host"}
	if err := graph.recordMaterializedProducer(prepKey, prepWriter.ID); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(targetKey, targetWriter.ID); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{prepWriter, targetWriter, dependency}}
	consumerKey := compactKbuildSelectionKey{profile: consumerProfile.Name, target: consumerPath, stage: "prep"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, consumerProfile).
		forOutput("prep", "prep", "sdk").
		withInitialObjectTree(true, visibleArtifact)
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		consumerPath,
		consumerProfile,
		[]compactKbuildRuleInput{{path: dependencyPath, producer: dependency.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
	}
	if got := byPath[pathname]; got.producer != prepWriter.ID || !got.workingOnly {
		t.Fatalf("prep fixdep snapshot = %#v, want exact prep producer %q", got, prepWriter.ID)
	}
	if got := byPath[dependencyPath]; got.producer != dependency.ID || got.workingOnly {
		t.Fatalf("native Rust dependency = %#v, want %q", got, dependency.ID)
	}
}

func TestKbuildWorkingTreeClosureUsesSourceOrderedVersionAcrossStageTrees(t *testing.T) {
	const (
		pathname       = "rust/kernel/generated_arch_warn_asm.rs"
		dependencyPath = "rust/kernel.o"
		consumerPath   = "vmlinux.a"
	)
	for _, test := range []struct {
		name    string
		ordered bool
	}{
		{name: "prep lifecycle orders target rewrite", ordered: true},
		{name: "unrelated target lifecycle writers remain ambiguous", ordered: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepProfile := CompactKbuildProfile{
				Name: "build:rust-prep", Path: "scripts/Makefile.build",
				EntryTargets: []string{pathname},
			}
			targetProfile := CompactKbuildProfile{
				Name: "build:rust-target", Path: "scripts/Makefile.build",
				EntryTargets: []string{pathname},
			}
			consumerProfile := CompactKbuildProfile{
				Name: "driver:Makefile", Path: "Makefile",
				EntryTargets: []string{consumerPath},
			}
			firstSelection := CompactKbuildSelection{
				Profile: prepProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "prep", Scope: "target", Stage: "prep",
			}
			firstNodeStage, firstNodeTree := "prep", "prep"
			if !test.ordered {
				firstSelection.Stage = "host"
				firstSelection.Lifecycle = "target"
				firstSelection.Scope = "host"
				firstNodeStage, firstNodeTree = "host", "host"
			}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{prepProfile, targetProfile, consumerProfile},
				KbuildSelections: []CompactKbuildSelection{
					firstSelection,
					{Profile: targetProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
					{Profile: consumerProfile.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target"},
				},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			prepWriter := ActionPlanNode{
				ID: strings.Repeat("a", 64), Stage: firstNodeStage, Kind: "generate",
				Outputs: []ActionPlanOutput{{Tree: firstNodeTree, Path: pathname}},
			}
			dependency := ActionPlanNode{
				ID: strings.Repeat("b", 64), Stage: "target", Kind: "compile",
				Inputs: []ActionPlanNodeEdge{{
					Role: compactKbuildWorkingClosureInputRole, ProducerID: prepWriter.ID,
				}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: dependencyPath}},
			}
			targetWriter := ActionPlanNode{
				ID: strings.Repeat("c", 64), Stage: "target", Kind: "generate",
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
			}
			prepKey := compactKbuildSelectionKey{
				profile: prepProfile.Name, target: pathname, stage: firstSelection.Stage,
			}
			targetKey := compactKbuildSelectionKey{
				profile: targetProfile.Name, target: pathname, stage: "target",
			}
			consumerKey := compactKbuildSelectionKey{
				profile: consumerProfile.Name, target: consumerPath, stage: "target",
			}
			if err := graph.recordMaterializedProducer(prepKey, prepWriter.ID); err != nil {
				t.Fatal(err)
			}
			if err := graph.recordMaterializedProducer(targetKey, targetWriter.ID); err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{Nodes: []ActionPlanNode{prepWriter, dependency, targetWriter}}
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
				withSelectionGraph(graph).
				forSelection(consumerKey, consumerProfile).
				forOutput("target", "objects", "vmlinux")
			inputs, closureErr := builder.compactKbuildWorkingTreeClosureInputs(
				consumerPath,
				consumerProfile,
				[]compactKbuildRuleInput{
					{path: dependencyPath, producer: dependency.ID},
					{path: pathname, producer: targetWriter.ID},
				},
			)
			if !test.ordered {
				if closureErr == nil || !strings.Contains(closureErr.Error(), "ambiguous") {
					t.Fatalf("unordered stage versions error = %v, want fail-closed ambiguity", closureErr)
				}
				return
			}
			if closureErr != nil {
				t.Fatal(closureErr)
			}
			byPath := map[string]compactKbuildRuleInput{}
			for _, input := range inputs {
				byPath[input.path] = input
			}
			if got := byPath[pathname]; got.producer != targetWriter.ID || got.workingOnly {
				t.Fatalf("source-ordered Rust input = %#v, want native target writer %q", got, targetWriter.ID)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureUsesSourceOrderedMaterializedSideOutput(t *testing.T) {
	const (
		firstTarget    = "rust/libpin_init_internal-prep.so"
		targetTarget   = "rust/libpin_init_internal.so"
		sideOutputPath = "rust/.libpin_init_internal.so.cmd"
		dependencyPath = "rust/pin_init.o"
		consumerPath   = "vmlinux.a"
	)
	for _, test := range []struct {
		name    string
		ordered bool
	}{
		{name: "prep lifecycle side output precedes target rewrite", ordered: true},
		{name: "unrelated target lifecycle side outputs remain ambiguous", ordered: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstProfile := CompactKbuildProfile{
				Name: "build:rust-procmacro-first", Path: "rust/Makefile",
				EntryTargets: []string{firstTarget},
			}
			targetProfile := CompactKbuildProfile{
				Name: "build:rust-procmacro-target", Path: "rust/Makefile",
				EntryTargets: []string{targetTarget},
			}
			consumerProfile := CompactKbuildProfile{
				Name: "driver:Makefile", Path: "Makefile",
				EntryTargets: []string{consumerPath},
			}
			firstSelection := CompactKbuildSelection{
				Profile: firstProfile.Name, Target: firstTarget, MakeTarget: firstTarget,
				Lifecycle: "prep", Scope: "target", Stage: "prep",
			}
			firstNodeStage, firstNodeTree := "prep", "prep"
			if !test.ordered {
				firstSelection.Lifecycle = "target"
				firstSelection.Scope = "host"
				firstSelection.Stage = "host"
				firstNodeStage, firstNodeTree = "host", "host"
			}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{firstProfile, targetProfile, consumerProfile},
				KbuildSelections: []CompactKbuildSelection{
					firstSelection,
					{Profile: targetProfile.Name, Target: targetTarget, MakeTarget: targetTarget, Lifecycle: "target", Scope: "target", Stage: "target"},
					{Profile: consumerProfile.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target"},
				},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			firstWriter := ActionPlanNode{
				ID: strings.Repeat("a", 64), Stage: firstNodeStage, Kind: "generate",
				Outputs: []ActionPlanOutput{
					{Tree: firstNodeTree, Path: firstTarget},
					{Tree: firstNodeTree, Path: sideOutputPath, ArtifactPath: ".linux-bzl-versions/rust-procmacro-first/" + sideOutputPath},
				},
			}
			dependency := ActionPlanNode{
				ID: strings.Repeat("b", 64), Stage: "target", Kind: "compile",
				Inputs: []ActionPlanNodeEdge{{
					Role: compactKbuildWorkingClosureInputRole, ProducerID: firstWriter.ID, Slot: 1,
				}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: dependencyPath}},
			}
			targetWriter := ActionPlanNode{
				ID: strings.Repeat("c", 64), Stage: "target", Kind: "generate",
				Outputs: []ActionPlanOutput{
					{Tree: "objects", Path: targetTarget},
					{Tree: "objects", Path: sideOutputPath, ArtifactPath: ".linux-bzl-versions/rust-procmacro-target/" + sideOutputPath},
				},
			}
			firstKey := compactKbuildSelectionKey{
				profile: firstProfile.Name, target: firstTarget, stage: firstSelection.Stage,
			}
			targetKey := compactKbuildSelectionKey{
				profile: targetProfile.Name, target: targetTarget, stage: "target",
			}
			consumerKey := compactKbuildSelectionKey{
				profile: consumerProfile.Name, target: consumerPath, stage: "target",
			}
			if err := graph.recordMaterializedProducer(firstKey, firstWriter.ID); err != nil {
				t.Fatal(err)
			}
			if err := graph.recordMaterializedProducer(targetKey, targetWriter.ID); err != nil {
				t.Fatal(err)
			}
			if got := len(graph.outputOwnersByPath[sideOutputPath]); got != 0 {
				t.Fatalf("materialized side-output pathname has %d statically registered owners, want none", got)
			}
			plan := &ActionPlan{Nodes: []ActionPlanNode{firstWriter, dependency, targetWriter}}
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
				withSelectionGraph(graph).
				forSelection(consumerKey, consumerProfile).
				forOutput("target", "objects", "vmlinux")
			inputs, closureErr := builder.compactKbuildWorkingTreeClosureInputs(
				consumerPath,
				consumerProfile,
				[]compactKbuildRuleInput{
					{path: dependencyPath, producer: dependency.ID},
					{path: sideOutputPath, producer: targetWriter.ID, slot: 1},
				},
			)
			if got := len(graph.outputOwnersByPath[sideOutputPath]); got != 0 {
				t.Fatalf("source comparison published %d side-output owners, want none", got)
			}
			if !test.ordered {
				if closureErr == nil || !strings.Contains(closureErr.Error(), "working object-tree path \""+sideOutputPath+"\" has ambiguous maximal producers") {
					t.Fatalf("unordered materialized side-output error = %v, want path-specific fail-closed ambiguity", closureErr)
				}
				return
			}
			if closureErr != nil {
				t.Fatal(closureErr)
			}
			byPath := map[string]compactKbuildRuleInput{}
			for _, input := range inputs {
				byPath[input.path] = input
			}
			if got := byPath[sideOutputPath]; got.producer != targetWriter.ID || got.slot != 1 || got.workingOnly {
				t.Fatalf("source-ordered materialized side output = %#v, want native target producer %q slot 1", got, targetWriter.ID)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureRejectsIncomparableUnrecordedVersions(t *testing.T) {
	const pathname = "generated/shared.out"
	first := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	root := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: first.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/root.o"}},
	}
	second := ActionPlanNode{
		ID: strings.Repeat("c", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{first, root, second}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		forOutput("target", "objects", "vmlinux")
	_, err := builder.compactKbuildWorkingTreeClosureInputs(
		"generated/consumer.o",
		CompactKbuildProfile{},
		[]compactKbuildRuleInput{
			{path: "generated/root.o", producer: root.ID},
			{path: pathname, producer: second.ID},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("unrecorded same-path versions error = %v, want fail-closed ambiguity", err)
	}
}

func TestKbuildWorkingTreeClosureResolvesAllVersionsIndependentOfTraversalOrder(t *testing.T) {
	const pathname = "generated/shared.out"
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			first := ActionPlanNode{
				ID: strings.Repeat("a", 64), Stage: "target", Kind: "generate",
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
			}
			second := ActionPlanNode{
				ID: strings.Repeat("b", 64), Stage: "target", Kind: "generate",
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
			}
			inputs := []ActionPlanNodeEdge{
				{Role: compactKbuildWorkingClosureInputRole, ProducerID: first.ID},
				{Role: compactKbuildWorkingClosureInputRole, ProducerID: second.ID},
			}
			if reverse {
				slices.Reverse(inputs)
			}
			tail := ActionPlanNode{
				ID: strings.Repeat("c", 64), Stage: "target", Kind: "generate",
				Inputs: inputs, Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
			}
			plan := &ActionPlan{Nodes: []ActionPlanNode{first, second, tail}}
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
				forOutput("target", "objects", "vmlinux")
			closure, err := builder.compactKbuildWorkingTreeClosureInputs(
				"generated/consumer.out",
				CompactKbuildProfile{},
				[]compactKbuildRuleInput{{path: pathname, producer: tail.ID}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(closure) != 1 || closure[0].producer != tail.ID || closure[0].workingOnly {
				t.Fatalf("three-version closure = %#v, want native tail %q", closure, tail.ID)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureRejectsCrossTreeOverwriteCycle(t *testing.T) {
	const (
		pathname     = "generated/shared.out"
		consumerPath = "generated/consumer.out"
	)
	profiles := []CompactKbuildProfile{
		{Name: "prehost-writer", Path: "a.mk", EntryTargets: []string{pathname}},
		{Name: "bootstrap-writer", Path: "b.mk", EntryTargets: []string{pathname}},
		{Name: "host-writer", Path: "c.mk", EntryTargets: []string{pathname}},
		{Name: "consumer", Path: "consumer.mk", EntryTargets: []string{consumerPath}},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profiles[0], []CompactKbuildVisibleArtifact{{
		Path: pathname, Profile: profiles[2].Name, Target: pathname,
	}})
	setTestCompactKbuildInitialVisibleArtifacts(t, &profiles[1], []CompactKbuildVisibleArtifact{{
		Path: pathname, Profile: profiles[0].Name, Target: pathname,
	}})
	setTestCompactKbuildInitialVisibleArtifacts(t, &profiles[2], []CompactKbuildVisibleArtifact{{
		Path: pathname, Profile: profiles[1].Name, Target: pathname,
	}})
	selections := []CompactKbuildSelection{
		{Profile: profiles[0].Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "host", Stage: "prehost"},
		{Profile: profiles[1].Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
		{Profile: profiles[2].Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "host", Stage: "host"},
		{Profile: profiles[3].Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target"},
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: profiles, KbuildSelections: selections,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := []ActionPlanNode{
		{ID: strings.Repeat("a", 64), Stage: "prehost", Kind: "generate", Outputs: []ActionPlanOutput{{Tree: "prehost", Path: pathname}}},
		{ID: strings.Repeat("b", 64), Stage: "bootstrap", Kind: "generate", Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: pathname}}},
		{ID: strings.Repeat("c", 64), Stage: "host", Kind: "generate", Outputs: []ActionPlanOutput{{Tree: "host", Path: pathname}}},
	}
	for index := range nodes {
		key := compactKbuildSelectionKey{
			profile: selections[index].Profile, target: pathname, stage: selections[index].Stage,
		}
		if err := graph.recordMaterializedProducer(key, nodes[index].ID); err != nil {
			t.Fatal(err)
		}
	}
	plan := &ActionPlan{Nodes: nodes}
	consumerKey := compactKbuildSelectionKey{
		profile: profiles[3].Name, target: consumerPath, stage: "target",
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, profiles[3]).
		forOutput("target", "objects", "vmlinux")
	direct := make([]compactKbuildRuleInput, 0, len(nodes))
	for _, node := range nodes {
		direct = append(direct, compactKbuildRuleInput{path: pathname, producer: node.ID})
	}
	_, err = builder.compactKbuildWorkingTreeClosureInputs(consumerPath, profiles[3], direct)
	if err == nil || !strings.Contains(err.Error(), "cyclic overwrite provenance") {
		t.Fatalf("cyclic cross-tree overwrite error = %v, want fail-closed cycle", err)
	}
}

func TestKbuildWorkingTreeClosureRejectsCyclicActionPlanProducerProvenance(t *testing.T) {
	const pathname = "generated/shared.out"
	first := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "target", Kind: "generate",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: strings.Repeat("b", 64),
		}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	second := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "generate",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: first.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{first, second}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		forOutput("target", "objects", "vmlinux")
	_, err := builder.compactKbuildWorkingTreeClosureInputs(
		"generated/consumer.out",
		CompactKbuildProfile{},
		[]compactKbuildRuleInput{
			{path: pathname, producer: first.ID},
			{path: pathname, producer: second.ID},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "cyclic producer provenance") {
		t.Fatalf("cyclic ActionPlan provenance error = %v, want resolver cycle failure", err)
	}
}

func TestKbuildWorkingTreeClosureRejectsUnmaterializedExactOwner(t *testing.T) {
	const (
		pathname     = "generated/shared.out"
		consumerPath = "generated/consumer.out"
	)
	prep := CompactKbuildProfile{Name: "prep-writer", Path: "prep.mk", EntryTargets: []string{pathname}}
	target := CompactKbuildProfile{
		Name: "target", Path: "target.mk", EntryTargets: []string{pathname, consumerPath},
	}
	artifact := CompactKbuildVisibleArtifact{Path: pathname, Profile: target.Name, Target: pathname}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{prep, target},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: prep.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: target.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
			{
				Profile: target.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target",
				GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
					[]CompactKbuildVisibleArtifact{artifact},
				),
			},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	prepNode := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	targetNode := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	prepKey := compactKbuildSelectionKey{profile: prep.Name, target: pathname, stage: "prep"}
	if err := graph.recordMaterializedProducer(prepKey, prepNode.ID); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{prepNode, targetNode}}
	consumerKey := compactKbuildSelectionKey{profile: target.Name, target: consumerPath, stage: "target"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, target).
		forOutput("target", "objects", "vmlinux")
	for _, test := range []struct {
		name   string
		direct []compactKbuildRuleInput
	}{
		{
			name:   "sole observed version",
			direct: []compactKbuildRuleInput{{path: pathname, producer: prepNode.ID}},
		},
		{
			name: "two observed versions",
			direct: []compactKbuildRuleInput{
				{path: pathname, producer: prepNode.ID},
				{path: pathname, producer: targetNode.ID},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := builder.compactKbuildWorkingTreeClosureInputs(
				consumerPath, target, test.direct,
			)
			if err == nil || !strings.Contains(err.Error(), "exact owner") || !strings.Contains(err.Error(), "has not been materialized") {
				t.Fatalf("unmaterialized exact owner error = %v, want exact invariant failure", err)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureRejectsMaterializedExactOwnerAbsentFromSoleVersion(t *testing.T) {
	const (
		pathname     = "generated/shared.out"
		consumerPath = "generated/consumer.out"
	)
	firstProfile := CompactKbuildProfile{Name: "first", Path: "first.mk", EntryTargets: []string{pathname}}
	exactProfile := CompactKbuildProfile{Name: "exact", Path: "exact.mk", EntryTargets: []string{pathname}}
	artifact := CompactKbuildVisibleArtifact{Path: pathname, Profile: exactProfile.Name, Target: pathname}
	consumerProfile := CompactKbuildProfile{
		Name: "consumer", Path: "consumer.mk", EntryTargets: []string{consumerPath},
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumerProfile, []CompactKbuildVisibleArtifact{artifact})
	consumerSelection := CompactKbuildSelection{
		Profile: consumerProfile.Name, Target: consumerPath, MakeTarget: consumerPath, Lifecycle: "target", Scope: "target", Stage: "target",
		UsesInitialObjectTree: true,
		InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
			[]CompactKbuildVisibleArtifact{artifact},
		),
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{firstProfile, exactProfile, consumerProfile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: firstProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: exactProfile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
			consumerSelection,
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	first := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "host", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: pathname}},
	}
	exact := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
	}
	if err := graph.recordMaterializedProducer(
		compactKbuildSelectionKey{profile: firstProfile.Name, target: pathname, stage: "host"}, first.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(
		compactKbuildSelectionKey{profile: exactProfile.Name, target: pathname, stage: "target"}, exact.ID,
	); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{first, exact}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(
			compactKbuildSelectionKey{profile: consumerProfile.Name, target: consumerPath, stage: "target"},
			consumerProfile,
		).
		forOutput("target", "objects", "vmlinux")
	_, err = builder.compactKbuildWorkingTreeClosureInputs(
		consumerPath,
		consumerProfile,
		[]compactKbuildRuleInput{{path: pathname, producer: first.ID}},
	)
	if err == nil || !strings.Contains(err.Error(), "exact consumer owner producer") || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing exact closure version error = %v, want exact invariant failure", err)
	}
}

func TestKbuildWorkingTreeClosureDoesNotBindUnobservedSameInvocationWriter(t *testing.T) {
	const (
		pathname = "rust/exports_core_generated.h"
		consumer = "rust/core.o"
	)
	profile := CompactKbuildProfile{
		Name: "build:rust", Path: "rust/Makefile", EntryTargets: []string{consumer, pathname},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	earlier := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{earlier}}
	consumerKey := compactKbuildSelectionKey{profile: profile.Name, target: consumer, stage: "target"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, profile).
		forOutput("target", "objects", "vmlinux")
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		consumer,
		profile,
		[]compactKbuildRuleInput{{path: pathname, producer: earlier.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].producer != earlier.ID || inputs[0].workingOnly {
		t.Fatalf("unobserved future-writer closure = %#v, want native earlier producer %q", inputs, earlier.ID)
	}
	futureKey := compactKbuildSelectionKey{profile: profile.Name, target: pathname, stage: "target"}
	if _, materialized := graph.materializedProducers[futureKey]; materialized {
		t.Fatalf("future same-invocation writer %s unexpectedly materialized", compactKbuildSelectionKeyString(futureKey))
	}
}

func TestKbuildWorkingTreeClosureDoesNotBindUnmaterializedOwnerForWorkingOnlyHistory(t *testing.T) {
	const (
		pathname = "rust/LINUX_BZL_PROBE_deadbeef"
		rootPath = "rust/compiler_builtins.o"
		consumer = "rust/core.o"
	)
	profile := CompactKbuildProfile{
		Name: "build:rust-target", Path: "scripts/Makefile.build",
		EntryTargets: []string{pathname, consumer},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: pathname, MakeTarget: pathname, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	historical := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	root := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: historical.ID,
		}},
		Outputs: []ActionPlanOutput{
			{Tree: "objects", Path: rootPath},
			{Tree: "objects", Path: pathname},
		},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{historical, root}}
	consumerKey := compactKbuildSelectionKey{profile: profile.Name, target: consumer, stage: "target"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, profile).
		forOutput("target", "objects", "vmlinux")
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		consumer,
		profile,
		[]compactKbuildRuleInput{{path: rootPath, producer: root.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
	}
	if got := byPath[pathname]; got.producer != root.ID || got.slot != 1 || !got.workingOnly {
		t.Fatalf("working-only sibling output = %#v, want slot 1 from %q", got, root.ID)
	}
	repeated, err := builder.compactKbuildWorkingTreeClosureInputs(consumer, profile, inputs)
	if err != nil {
		t.Fatalf("repeat closure expansion: %v", err)
	}
	if !slices.Equal(repeated, inputs) {
		t.Fatalf("repeated closure = %#v, want stable %#v", repeated, inputs)
	}
	exactKey := compactKbuildSelectionKey{profile: profile.Name, target: pathname, stage: "target"}
	if _, materialized := graph.materializedProducers[exactKey]; materialized {
		t.Fatalf("same-invocation writer %s unexpectedly materialized", compactKbuildSelectionKeyString(exactKey))
	}
}

func TestKbuildSourceScriptClosurePrefersDownstreamPreparationProjection(t *testing.T) {
	const output = "scripts/mod/devicetable-offsets.s"
	prehost := ActionPlanNode{
		ID: strings.Repeat("0", 64), Stage: "prehost", Kind: "compile",
		Outputs: []ActionPlanOutput{{Tree: "prehost", Path: output}},
	}
	bootstrap := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "bootstrap", Kind: "copy", Tool: "actionfile",
		Inputs:  []ActionPlanNodeEdge{{Role: "input", ProducerID: prehost.ID}},
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: output}},
	}
	projection := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "prep", Kind: "copy", Tool: "actionfile",
		Inputs:  []ActionPlanNodeEdge{{Role: "input", ProducerID: bootstrap.ID}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: output}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{prehost, bootstrap, projection}}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: output, Profile: "headers", Target: output,
	}
	profile := CompactKbuildProfile{
		Name: "scripts/mod",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})
	for _, test := range []struct {
		name            string
		stage           string
		wantProducer    string
		direct          []compactKbuildRuleInput
		wantWorkingOnly bool
		wantOrderOnly   bool
	}{
		{name: "target preparation baseline", stage: "target", wantProducer: projection.ID, wantWorkingOnly: true},
		{name: "prep preparation baseline", stage: "prep", wantProducer: projection.ID, wantWorkingOnly: true},
		{name: "host rebases projection", stage: "host", wantProducer: bootstrap.ID, wantWorkingOnly: true},
		{name: "bootstrap rebases projection", stage: "bootstrap", wantProducer: bootstrap.ID, wantWorkingOnly: true},
		{name: "prehost native producer", stage: "prehost", wantProducer: prehost.ID, wantWorkingOnly: true},
		{
			name: "native ancestor", stage: "target", wantProducer: bootstrap.ID,
			direct: []compactKbuildRuleInput{{
				path: output, producer: bootstrap.ID, orderOnly: true,
			}},
			wantOrderOnly: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput(test.stage, map[string]string{
				"prehost": "prehost", "bootstrap": "bootstrap", "host": "host", "prep": "prep", "target": "objects",
			}[test.stage], map[string]string{"prehost": "sdk", "bootstrap": "sdk", "host": "sdk", "prep": "sdk", "target": "vmlinux"}[test.stage])
			ownerStage, ownerLifecycle, ownerScope := "prep", "prep", "target"
			if test.wantProducer == bootstrap.ID {
				ownerStage = "bootstrap"
				ownerLifecycle = "target"
			} else if test.wantProducer == prehost.ID {
				ownerStage = "prehost"
				ownerLifecycle, ownerScope = "target", "host"
			}
			builder = withMaterializedInitialObjectTreeArtifactForTest(
				t, builder, visibleArtifact, ownerStage, ownerLifecycle, ownerScope, test.wantProducer,
			)
			inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
				"arch/x86/include/generated/asm/orc_hash.h",
				profile,
				test.direct,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 {
				t.Fatalf("closure inputs=%#v, want one canonical projection", inputs)
			}
			got := inputs[0]
			if got.producer != test.wantProducer || got.slot != 0 || got.path != output {
				t.Fatalf("closure input=%#v, want stage-visible producer %s", got, test.wantProducer)
			}
			if got.workingOnly != test.wantWorkingOnly || got.orderOnly != test.wantOrderOnly {
				t.Fatalf(
					"closure input=%#v, want workingOnly=%t orderOnly=%t",
					got, test.wantWorkingOnly, test.wantOrderOnly,
				)
			}
		})
	}
}

func TestKbuildHostSourceScriptClosureRebasesConfigProjectionToSource(t *testing.T) {
	const (
		input  = "config/autoconf.h"
		output = "include/generated/autoconf.h"
	)
	source := ActionPlanSource{ID: "src-00000001", Namespace: "config", Path: input}
	projection := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "prep", Kind: "copy", Tool: "actionfile",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: source.ID}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: output}},
	}
	plan := &ActionPlan{Sources: []ActionPlanSource{source}, Nodes: []ActionPlanNode{projection}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput("host", "host", "sdk")
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs("arch/x86/boot/mkcpustr", CompactKbuildProfile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("host closure inputs=%#v, want one config source projection", inputs)
	}
	got := inputs[0]
	if got.path != output || got.sourceID != source.ID || got.producer != "" || !got.workingOnly {
		t.Fatalf("host config baseline=%#v, want source %s staged at %s", got, source.ID, output)
	}
}

func TestKbuildWorkingTreeClosurePrefersGeneratedConfigStateInLineage(t *testing.T) {
	const configPath = ".config"
	configSource := ActionPlanSource{ID: "src-00000001", Namespace: "config", Path: configPath}
	configWriter := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "bootstrap", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: configPath}},
	}
	root := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "bootstrap", Kind: "compile",
		Inputs:  []ActionPlanNodeEdge{{Role: compactKbuildWorkingClosureInputRole, ProducerID: configWriter.ID}},
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: "generated/leaf.o"}},
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{configSource},
		Nodes:   []ActionPlanNode{configWriter, root},
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		forOutput("bootstrap", "bootstrap", "sdk")
	inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
		"generated/consumer.o",
		CompactKbuildProfile{},
		[]compactKbuildRuleInput{{path: "generated/leaf.o", producer: root.ID}},
	)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
	}
	configInput, ok := byPath[configPath]
	if !ok || configInput.producer != configWriter.ID || configInput.slot != 0 || configInput.sourceID != "" || !configInput.workingOnly {
		t.Fatalf("config closure input=%#v, want generated lineage state %q", configInput, configWriter.ID)
	}
	if leaf := byPath["generated/leaf.o"]; leaf.producer != root.ID || leaf.workingOnly {
		t.Fatalf("native closure root=%#v, want %q", leaf, root.ID)
	}
}

func TestKbuildSourceScriptClosurePrefersNativeInputOverVisibleFrontier(t *testing.T) {
	const (
		output       = "scripts/mod/devicetable-offsets.s"
		dependentOut = "generated/dependent.o"
		sourceID     = "src-direct-devicetable-offsets"
	)
	bootstrap := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "bootstrap", Kind: "compile",
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: output}},
	}
	independent := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "prep", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: bootstrap.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: output}},
	}
	dependent := ActionPlanNode{
		ID: strings.Repeat("c", 64), Stage: "prep", Kind: "compile",
		Inputs: []ActionPlanNodeEdge{{
			Role: compactKbuildWorkingClosureInputRole, ProducerID: independent.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: dependentOut}},
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{bootstrap, independent, dependent}}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: output, Profile: "producer", Target: output,
	}
	profile := CompactKbuildProfile{
		Name: "consumer",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})
	for _, test := range []struct {
		name         string
		direct       []compactKbuildRuleInput
		wantProducer string
		wantSource   string
	}{
		{
			name: "generated native prerequisite",
			direct: []compactKbuildRuleInput{{
				path: output, producer: bootstrap.ID,
			}},
			wantProducer: bootstrap.ID,
		},
		{
			name: "generated native prerequisite with baseline traversed elsewhere",
			direct: []compactKbuildRuleInput{
				{path: output, producer: bootstrap.ID},
				{path: dependentOut, producer: dependent.ID},
			},
			wantProducer: bootstrap.ID,
		},
		{
			name: "immutable native prerequisite",
			direct: []compactKbuildRuleInput{{
				path: output, sourceID: sourceID,
			}},
			wantSource: sourceID,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := withMaterializedInitialObjectTreeArtifactForTest(
				t, newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan),
				visibleArtifact, "prep", "prep", "target", independent.ID,
			)
			inputs, err := builder.compactKbuildWorkingTreeClosureInputs(
				"arch/x86/include/generated/asm/orc_hash.h",
				profile,
				test.direct,
			)
			if err != nil {
				t.Fatal(err)
			}
			byPath := map[string]compactKbuildRuleInput{}
			for _, input := range inputs {
				byPath[input.path] = input
			}
			got := byPath[output]
			if got.producer != test.wantProducer || got.sourceID != test.wantSource || got.workingOnly {
				t.Fatalf(
					"native path input = %#v, want producer=%q source=%q",
					got, test.wantProducer, test.wantSource,
				)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureRejectsVisibleArtifactAfterConsumerStage(t *testing.T) {
	const output = "arch/x86/include/generated/uapi/asm/types.h"
	producer := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "generate",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: output}},
	}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: output, Profile: "headers", Target: output,
	}
	profile := CompactKbuildProfile{
		Name: "scripts/mod",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})
	plan := &ActionPlan{Nodes: []ActionPlanNode{producer}}
	builder := withMaterializedInitialObjectTreeArtifactForTest(
		t,
		newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput("bootstrap", "bootstrap", "sdk"),
		visibleArtifact, "prep", "prep", "target", producer.ID,
	)
	_, err := builder.compactKbuildWorkingTreeClosureInputs(
		"scripts/mod/devicetable-offsets.s", profile, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "is not visible from bootstrap stage") {
		t.Fatalf("closure error=%v, want fail-closed later-stage artifact", err)
	}
}

func TestKbuildDirectSourceShellScriptUsesHermeticRuntime(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t,
		`$(srctree)/scripts/transform.sh --mode direct $< > $@`,
	)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.h")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("direct source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("direct source script tool=%q/%q, want %q", node.Tool, recipe.Tool, compactKbuildScriptRunnerRole)
	}
	for _, argument := range []string{"-interpreter_arg", "sh", "--", "--mode", "direct"} {
		if !slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("direct source-script arguments=%q, want %q", recipe.Arguments, argument)
		}
	}
	if !slices.ContainsFunc(recipe.Arguments, func(argument string) bool {
		return strings.HasPrefix(argument, "${source:prerequisite:")
	}) {
		t.Fatalf("direct source-script arguments=%q, want bound input source", recipe.Arguments)
	}
	if slices.Contains(recipe.Arguments, "scripts/transform.sh") {
		t.Fatalf("direct source script was not bound as an immutable source: %q", recipe.Arguments)
	}
}

func TestKbuildSourceScriptEnvironmentRewritesConfiguredToolProvenance(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:demo", "scripts/Makefile.build", "demo", `
export CC = `+KbuildActionRoleToken("target", "cc")+`
export TOOL_AND_FLAGS = $(CC) --selected
demo/%.out: export TARGET_VALUE = $*
demo/%.out: demo/%.in
`, nil)
	environment, roles, err := compactKbuildSourceScriptExportedEnvironment(
		profile,
		"demo/result.out",
		"result",
		[]string{"demo/result.in"},
		nil,
		nil,
		nil,
		compactKbuildSourceScriptEnvironmentUsage{ObservesAll: true},
		"target",
		testTargetActionRoles("cc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"CC":             "cc",
		"TOOL_AND_FLAGS": "cc --selected",
		"TARGET_VALUE":   "result",
	} {
		if got := environment[name]; got != want {
			t.Fatalf("%s=%q, want %q (environment %#v)", name, got, want, environment)
		}
	}
	if !slices.Equal(roles, []string{"cc"}) {
		t.Fatalf("environment action roles=%q, want cc", roles)
	}
}

func TestKbuildSourceScriptEnvironmentRejectsLiteralTreeMarker(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:demo", "scripts/Makefile.build", "demo", `
export LITERAL_TREE = '$${tree:prep}'
demo/result.out: FORCE
`, nil)
	_, _, err := compactKbuildSourceScriptExportedEnvironment(
		profile,
		"demo/result.out",
		"",
		[]string{"FORCE"},
		nil,
		nil,
		nil,
		compactKbuildSourceScriptEnvironmentUsage{ObservesAll: true},
		"target",
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "literal ${tree:...} marker") {
		t.Fatalf("literal tree environment error=%v", err)
	}
}

func TestKbuildSourceScriptEnvironmentProjectsRoleCapabilitiesFromUsage(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:demo", "scripts/Makefile.build", "demo", `
export CC HOSTCC YACC
export ROLE_FREE := preserved exactly
`, map[string]string{
		"CC":     KbuildActionRoleToken("target", "cc"),
		"HOSTCC": KbuildActionRoleToken("host", "cc"),
		"YACC":   KbuildActionRoleToken("host", "bison"),
	})
	for name, test := range map[string]struct {
		usage      compactKbuildSourceScriptEnvironmentUsage
		inline     map[string]string
		scope      string
		configured []KbuildActionRoleRef
		want       map[string]string
		wantRoles  []string
		wantError  bool
	}{
		"target only": {
			usage:      compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{"CC": true}},
			scope:      "target",
			configured: testTargetActionRoles("cc"),
			want:       map[string]string{"CC": "cc", "ROLE_FREE": "preserved exactly"},
			wantRoles:  []string{"cc"},
		},
		"host only": {
			usage:      compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{"HOSTCC": true}},
			scope:      "host",
			configured: testHostActionRoles("cc"),
			want:       map[string]string{"HOSTCC": "cc", "ROLE_FREE": "preserved exactly"},
			wantRoles:  []string{"cc"},
		},
		"unused parser generator": {
			usage:      compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}},
			scope:      "target",
			configured: testTargetActionRoles("cc"),
			want:       map[string]string{"ROLE_FREE": "preserved exactly"},
		},
		"inline overrides export before projection": {
			usage:      compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{"CC": true}},
			inline:     map[string]string{"CC": "literal-override"},
			scope:      "host",
			configured: testHostActionRoles("cc"),
			want:       map[string]string{"CC": "literal-override", "ROLE_FREE": "preserved exactly"},
		},
		"dynamic observes mixed scopes": {
			usage:      compactKbuildSourceScriptEnvironmentUsage{ObservesAll: true},
			scope:      "target",
			configured: testTargetActionRoles("cc"),
			wantError:  true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment, roles, err := compactKbuildSourceScriptExportedEnvironment(
				profile, "demo/result", "", nil, nil, nil,
				test.inline, test.usage, test.scope, test.configured,
			)
			if test.wantError {
				if err == nil {
					t.Fatalf("projected environment unexpectedly succeeded: %#v", environment)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(environment, test.want) {
				t.Fatalf("projected environment = %#v, want %#v", environment, test.want)
			}
			if !slices.Equal(roles, test.wantRoles) {
				t.Fatalf("projected environment roles = %q, want %q", roles, test.wantRoles)
			}
		})
	}
}

func TestKbuildSourceScriptRejectsCommandTextAndGeneratedPayload(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "command text", command: `$(CONFIG_SHELL) -c scripts/transform.sh > $@`, want: "non-file mode"},
		{name: "object payload", command: `$(CONFIG_SHELL) $(objtree)/scripts/transform.sh > $@`, want: "not a declared source file"},
		{name: "split startup file", command: `$(CONFIG_SHELL) --rcfile scripts/bashrc $(srctree)/scripts/transform.sh > $@`, want: "non-file mode"},
		{name: "equals startup file", command: `$(CONFIG_SHELL) --init-file=scripts/bashrc $(srctree)/scripts/transform.sh > $@`, want: "non-file mode"},
		{name: "unknown long option", command: `$(CONFIG_SHELL) --startup-file scripts/bashrc $(srctree)/scripts/transform.sh > $@`, want: "non-file mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := compactKbuildScriptProfileForTest(t, test.command)
			metadata := &CompactMetadata{
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				actionRoles: testTargetActionRoles("awk", "cc"),
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{Recipes: map[string]ActionRecipe{}}).forProfile(profile)
			_, err := builder.build("generated/result.h")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("build error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestKbuildSourceScriptRejectsShebangShellStartupInput(t *testing.T) {
	const (
		script = "scripts/bash-generator"
		target = "generated/result.h"
	)
	role := compactKbuildScriptAppletRolePrefix + "bash"
	for name, shebang := range map[string]string{
		"split startup file":  "#!/usr/bin/env -S bash --rcfile scripts/bashrc\n",
		"equals startup file": "#!/usr/bin/env -S bash --init-file=scripts/bashrc\n",
		"unknown long option": "#!/usr/bin/env -S bash --startup-file scripts/bashrc\n",
	} {
		t.Run(name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "bash-generator", "Makefile", "", `
cmd_generate = bash $(srctree)/scripts/bash-generator > $@
generated/result.h: scripts/bash-generator FORCE
	$(call cmd,generate)
`, nil)
			profile = compactKbuildProfileWithSourcesForTest(t, profile, script)
			root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
			mustWriteSource(t, root, script, shebang+"printf '%s\\n' bounded\n")

			_, matched, err := compactKbuildProfileSourceInterpreterCommand(
				profile,
				"bash",
				[]string{"${tree:kernel}/" + script},
			)
			if !matched || err == nil || !strings.Contains(err.Error(), "shebang interpreter arguments") {
				t.Fatalf("source-interpreter match=(%t, %v), want shebang argument rejection", matched, err)
			}

			metadata := &CompactMetadata{
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				actionRoles: testTargetActionRoles(role),
			}
			builder := newCompactKbuildRulePlanBuilder(
				metadata,
				&ActionPlan{Recipes: map[string]ActionRecipe{}},
			).forProfile(profile)
			if _, buildErr := builder.build(target); buildErr == nil ||
				!strings.Contains(buildErr.Error(), "shebang interpreter arguments") {
				t.Fatalf("unsafe shebang build error = %v, want argument rejection", buildErr)
			}
		})
	}
}

func TestKbuildSourceScriptPreservesConfiguredInterpreterArgumentsOnce(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`)
	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program: "sh",
		arguments: []string{
			"-eu",
			"${tree:kernel}/scripts/transform.sh",
		},
	}, map[string]string{"CONFIG_SHELL": "sh -eu"}, "target", testTargetActionRoles("awk", "cc"))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("configured source-script invocation did not match")
	}
	if got, want := invocation.interpreterArguments, []string{"-eu"}; !slices.Equal(got, want) {
		t.Fatalf("interpreter arguments=%q, want %q", got, want)
	}
	arguments, err := invocation.recipeArguments("script:00000000", map[string]string{"CONFIG_SHELL": "sh -eu"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := countString(arguments, "-eu"); got != 1 {
		t.Fatalf("recipe arguments contain configured interpreter option %d times, want once: %q", got, arguments)
	}
}

func TestKbuildSourceScriptPreservesValuedInterpreterArgumentsOnce(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`)
	interpreterArguments := []string{
		"-eo", "pipefail",
		"+O", "extglob",
	}
	commandArguments := append(slices.Clone(interpreterArguments),
		"${tree:kernel}/scripts/transform.sh", "-c", "input.c",
	)
	configuration := "sh " + strings.Join(interpreterArguments, " ")
	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program:   "sh",
		arguments: commandArguments,
	}, map[string]string{"CONFIG_SHELL": configuration}, "target", testTargetActionRoles("awk", "cc"))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("configured source-script invocation did not match")
	}
	if !slices.Equal(invocation.interpreterArguments, interpreterArguments) {
		t.Fatalf("interpreter arguments=%q, want %q", invocation.interpreterArguments, interpreterArguments)
	}
	if got, want := invocation.scriptArguments, []string{"-c", "input.c"}; !slices.Equal(got, want) {
		t.Fatalf("script arguments=%q, want %q", got, want)
	}
	recipeArguments, err := invocation.recipeArguments("script:00000000", map[string]string{"CONFIG_SHELL": configuration}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, argument := range interpreterArguments {
		if got := countString(recipeArguments, argument); got != 1 {
			t.Fatalf("recipe arguments contain interpreter argument %q %d times, want once: %q", argument, got, recipeArguments)
		}
	}
}

func TestKbuildSourceScriptRecognizesConfiguredRuntimeRole(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh $@ $^`)
	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program: KbuildActionRoleToken("host", compactKbuildScriptRuntimeRole),
		arguments: []string{
			"${tree:kernel}/scripts/transform.sh",
			"generated/result.h",
			"input.txt",
		},
	}, map[string]string{"CONFIG_SHELL": "sh"}, "host", testHostActionRoles(compactKbuildScriptRuntimeRole))
	if err != nil {
		t.Fatal(err)
	}
	if !matched || invocation.scriptPath != "scripts/transform.sh" || invocation.interpreter != "sh" {
		t.Fatalf("configured runtime invocation=(%#v,%t), want immutable shell script", invocation, matched)
	}
	if got, want := invocation.scriptArguments, []string{"generated/result.h", "input.txt"}; !slices.Equal(got, want) {
		t.Fatalf("configured runtime script arguments=%q, want %q", got, want)
	}
}

func TestKbuildSourceScriptRecognizesDefaultShWithoutVariableSnapshot(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`)
	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program:   "sh",
		arguments: []string{"${tree:kernel}/scripts/transform.sh"},
	}, nil, "target", testTargetActionRoles(compactKbuildScriptRuntimeRole))
	if err != nil {
		t.Fatal(err)
	}
	if !matched || invocation.scriptPath != "scripts/transform.sh" || invocation.interpreter != "sh" {
		t.Fatalf("default sh invocation=(%#v,%t), want immutable shell script", invocation, matched)
	}
}

func TestKbuildSourceScriptPreservesSelectedInterpreterApplet(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`)
	invocation, matched, err := compactKbuildSourceScriptCommand(profile, compactKbuildRecipeCommand{
		program: "/declared/runtime/ash",
		arguments: []string{
			"-e",
			"${tree:kernel}/scripts/transform.sh",
		},
	}, map[string]string{"CONFIG_SHELL": "/declared/runtime/ash -e"}, "target", testTargetActionRoles("awk", "cc"))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("configured source-script invocation did not match")
	}
	arguments, err := invocation.recipeArguments("script:00000000", map[string]string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(arguments, "ash") || slices.Contains(arguments, "sh") {
		t.Fatalf("recipe interpreter arguments=%q, want selected ash applet without injected sh", arguments)
	}
}

func TestConfiguredActionRoleProvenanceDisambiguatesSharedExecutable(t *testing.T) {
	profile := compactKbuildScriptProfileForTest(t, `$(CC) --emit > $@`)
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles("as", "cc"),
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	values := map[string]string{"CONFIG_SHELL": "sh"}
	commands := []compactKbuildRecipeCommand{{program: KbuildActionRoleToken("target", "cc"), arguments: []string{"--emit"}, stdout: "generated/result.h"}}
	producer, err := builder.appendCompactKbuildRecipe("generated/result.h", compactKbuildRuleMatch{profile: profile}, nil, values, commands)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "cc" {
		t.Fatalf("shared executable selected node=%#v, want cc provenance", node)
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
