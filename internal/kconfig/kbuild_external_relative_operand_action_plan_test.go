package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestExternalKbuildActionStagesInvocationRelativePreparedOperand(t *testing.T) {
	const (
		directory       = ".linux-bzl/external/demo"
		preparedOperand = "include/generated/asm-offsets.h"
		relativeOperand = "../../../include/generated/asm-offsets.h"
		target          = directory + "/asm-offsets.s"
	)

	profile := mustCompactKbuildProfileForTest(
		t,
		"external-relative-operand",
		"scripts/Makefile.build",
		directory,
		"",
		nil,
	)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, preparedOperand, "#define NR_PAGEFLAGS 1\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": t.TempDir(),
		"__LINUX_BZL_OBJECT_TREE__": objectRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree:      CompactKbuildInvocationObjectTree,
		Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}

	metadata := &CompactMetadata{
		actionRoles:             testTargetActionRoles("awk"),
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			preparedOperand: "prep",
		},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.appendCompactKbuildRecipe(
		target,
		compactKbuildRuleMatch{profile: profile},
		nil,
		nil,
		[]compactKbuildRecipeCommand{{
			program:   KbuildActionRoleToken("target", "awk"),
			arguments: []string{relativeOperand},
			stdout:    target,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external awk producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	preparedSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "prep" && source.Path == preparedOperand {
			preparedSourceID = source.ID
			break
		}
	}
	if preparedSourceID == "" {
		t.Fatalf("plan sources = %#v, want exact prep/%s source", plan.Sources, preparedOperand)
	}
	if !slices.ContainsFunc(node.Sources, func(edge ActionPlanSourceEdge) bool {
		return edge.SourceID == preparedSourceID
	}) {
		t.Fatalf("node source edges = %#v, want source %q for prep/%s", node.Sources, preparedSourceID, preparedOperand)
	}

	workingBinding := ""
	for binding, pathname := range recipe.WorkingInputs {
		if pathname == preparedOperand {
			workingBinding = binding
			break
		}
	}
	if workingBinding == "" {
		t.Fatalf("working inputs = %#v, want canonical path %q", recipe.WorkingInputs, preparedOperand)
	}
	if !slices.Contains(recipe.Arguments, "${"+workingBinding+"}") {
		t.Fatalf("arguments = %#v, want exact working-input binding %q", recipe.Arguments, "${"+workingBinding+"}")
	}
	for _, argument := range recipe.Arguments {
		if strings.Contains(argument, relativeOperand) || strings.Contains(argument, "../../../") {
			t.Fatalf("arguments retain invocation-relative operand %q: %#v", relativeOperand, recipe.Arguments)
		}
	}
}

func TestExternalCompoundCompilerProjectsProducerBackedSourceFromTypedDirectory(t *testing.T) {
	const (
		directory = ".linux-bzl/external/demo"
		source    = directory + "/demo.mod.c"
		target    = directory + "/demo.mod.o"
	)

	profile := mustCompactKbuildProfileForTest(
		t,
		"external-generated-compiler-input",
		"scripts/Makefile.build",
		directory,
		`
cmd_cc_o_c = { $(CC) -c -o $@ $<; }
`+target+`: `+source+` FORCE
	$(call if_changed,cc_o_c)
FORCE:
`,
		map[string]string{"CC": KbuildActionRoleToken("target", "cc")},
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}

	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles(append(testConfiguredActionRoles, "cc")...),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	sourceProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "modules",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: source}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""},
		Outputs:   []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("external generated compiler rule for %q was not selected", target)
	}

	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{{
		path: source, producer: sourceProducer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external generated compiler producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("external generated compiler = node %#v recipe %#v, want atomic scriptrun", node, recipe)
	}
	if recipe.ExecutionDirectory != directory {
		t.Fatalf("external generated compiler directory = %q, want %q", recipe.ExecutionDirectory, directory)
	}
	if !slices.ContainsFunc(node.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == sourceProducer
	}) || !slices.ContainsFunc(sortedStringMapValues(recipe.WorkingInputs), func(pathname string) bool {
		return pathname == source
	}) {
		t.Fatalf("external generated compiler input closure = node %#v recipe %#v, want producer-backed %q", node.Inputs, recipe.WorkingInputs, source)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	wantSource := "../../../" + source
	if !strings.Contains(script, wantSource) {
		t.Fatalf("external generated compiler script omits typed source %q: %q", wantSource, script)
	}
	if strings.Contains(script, " "+source) || strings.Contains(script, "'"+source+"'") {
		t.Fatalf("external generated compiler script retains work-root path from nested cwd: %q", script)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("validate external generated compiler action plan: %v", err)
	}
}

func TestExternalKbuildRecordmcountRetainsKernelSourceScriptProvenance(t *testing.T) {
	const (
		directory    = ".linux-bzl/external/demo"
		moduleSource = directory + "/demo.c"
		recordmcount = "scripts/recordmcount.pl"
		target       = directory + "/demo.o"
	)

	profile := mustCompactKbuildProfileForTest(
		t,
		"external-recordmcount",
		"scripts/Makefile.build",
		directory,
		`
cmd_cc_o_c = $(CC) -c -o $@ $<
cmd_record_mcount = perl $(srctree)/scripts/recordmcount.pl arm little 32 $@
$(obj)/demo.o: $(src)/demo.c $(srctree)/scripts/recordmcount.pl FORCE
	$(call cmd,cc_o_c)
	$(call cmd,record_mcount)
`,
		map[string]string{
			"CC":      KbuildActionRoleToken("target", "cc"),
			"obj":     directory,
			"src":     directory,
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
	)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, moduleSource, recordmcount)
	kernelRoot := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, kernelRoot, recordmcount, "#!/usr/bin/env perl\n")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree:      CompactKbuildInvocationObjectTree,
		Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}

	perlRole := compactKbuildScriptAppletRolePrefix + "perl"
	metadata := &CompactMetadata{
		actionRoles: append(slices.Clone(testConfiguredScopedActionRoles), testTargetActionRoles(perlRole)...),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("external recordmcount rule was not selected")
	}
	if got, want := match.commandSequence(), []string{"cc_o_c", "record_mcount"}; !slices.Equal(got, want) {
		t.Fatalf("external recordmcount command sequence = %q, want %q", got, want)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	inputs, err := builder.ruleInputs(target, match)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inputs, func(input compactKbuildRuleInput) bool {
		return input.path == recordmcount && input.sourceID != "" && input.producer == "" && !input.objectTree
	}) {
		t.Fatalf("external recordmcount inputs = %#v, want exact immutable kernel source", inputs)
	}
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}

	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external recordmcount producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("external recordmcount tool = %q/%q, want %q", node.Tool, recipe.Tool, compactKbuildScriptRunnerRole)
	}
	wantInterpreter := "${tool:" + perlRole + "}"
	if len(recipe.Arguments) < 2 || recipe.Arguments[0] != "-interpreter" || recipe.Arguments[1] != wantInterpreter {
		t.Fatalf("external recordmcount arguments = %#v, want interpreter %q", recipe.Arguments, wantInterpreter)
	}
	if slices.Contains(recipe.Arguments, "perl") || slices.Contains(recipe.Arguments, recordmcount) {
		t.Fatalf("external recordmcount arguments retain untyped interpreter or script dispatch: %#v", recipe.Arguments)
	}
	if !slices.Contains(node.AuxiliaryTools, perlRole) || !slices.Contains(recipe.AuxiliaryTools, perlRole) {
		t.Fatalf("external recordmcount Perl role is absent from tool closure: node=%#v recipe=%#v", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
	if len(node.Inputs) == 0 {
		t.Fatalf("external recordmcount node has no compiler dependency: %#v", node)
	}
	compiled := ActionPlanNode{}
	for _, input := range node.Inputs {
		candidate, found := compactKbuildPlanNode(plan, input.ProducerID)
		if found && candidate.Tool == "cc" {
			compiled = candidate
			break
		}
	}
	if compiled.ID == "" || len(compiled.Outputs) == 0 || compiled.Outputs[0].Path != target {
		t.Fatalf("external recordmcount compiler dependency = %#v, want cc producer for %q", compiled, target)
	}
	scriptSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "kernel" && source.Path == recordmcount {
			scriptSourceID = source.ID
			break
		}
	}
	if scriptSourceID == "" {
		t.Fatalf("plan sources = %#v, want exact kernel source %q", plan.Sources, recordmcount)
	}
	scriptEdge := false
	for _, edge := range node.Sources {
		if edge.Role == "script" && edge.SourceID == scriptSourceID {
			scriptEdge = true
		}
	}
	if !scriptEdge {
		t.Fatalf("external recordmcount source edges = %#v, want script source %q", node.Sources, scriptSourceID)
	}
}

func TestExternalKbuildResponseFileRetainsInvocationDirectory(t *testing.T) {
	const (
		directory    = ".linux-bzl/external/demo"
		responseFile = directory + "/combined.mod"
		objectFile   = directory + "/leaf.o"
		target       = directory + "/combined.o"
	)

	profile := mustCompactKbuildProfileForTest(
		t,
		"external-response-file",
		"scripts/Makefile.build",
		directory,
		`
cmd_mod = printf '%s\n' leaf.o | $(AWK) '!x[$$0]++ { print("$(obj)/"$$0) }' > $@
cmd_ld_multi_m = $(LD) -r -o $@ @$<
multi-obj-m := $(obj)/combined.o
$(obj)/%.mod: FORCE
	$(call cmd,mod)
$(multi-obj-m): %.o: %.mod FORCE
	$(call cmd,ld_multi_m)
$(multi-obj-m): $(obj)/leaf.o
`,
		map[string]string{
			"AWK": KbuildActionRoleToken("target", "awk"),
			"LD":  KbuildActionRoleToken("target", "ld"),
			"obj": "__LINUX_BZL_OBJECT_TREE__/" + directory,
		},
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree:      CompactKbuildInvocationObjectTree,
		Directory: directory,
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
	objectProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: objectFile}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""},
		Outputs:   []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}

	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildSelectedTarget(target, "combined.o")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external response-file producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "ld" || recipe.Tool != "ld" || recipe.ExecutionDirectory != directory {
		t.Fatalf("external response-file execution = tool %q/%q directory %q, want ld in %q", node.Tool, recipe.Tool, recipe.ExecutionDirectory, directory)
	}
	wantOutput := "../../../" + target
	wantResponse := "@combined.mod"
	if !slices.Contains(recipe.Arguments, wantOutput) || !slices.Contains(recipe.Arguments, wantResponse) {
		t.Fatalf("external response-file arguments = %q, want output %q and response file %q", recipe.Arguments, wantOutput, wantResponse)
	}
	workingPaths := map[string]bool{}
	for _, pathname := range recipe.WorkingInputs {
		workingPaths[pathname] = true
	}
	for _, pathname := range []string{responseFile, objectFile} {
		if !workingPaths[pathname] {
			t.Errorf("external response-file working inputs omit %q: %#v", pathname, recipe.WorkingInputs)
		}
	}
	responseProducer := ""
	for _, input := range node.Inputs {
		candidate, found := compactKbuildPlanNode(plan, input.ProducerID)
		if !found || len(candidate.Outputs) == 0 {
			continue
		}
		if candidate.Outputs[0].Path == responseFile {
			responseProducer = candidate.ID
		}
	}
	if responseProducer == "" || !slices.ContainsFunc(node.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == objectProducer
	}) {
		t.Fatalf("external response-file dependencies = %#v, want response producer and object %q", node.Inputs, objectProducer)
	}
	responseNode, ok := compactKbuildPlanNode(plan, responseProducer)
	if !ok {
		t.Fatalf("external response-file node %q not found", responseProducer)
	}
	responseRecipe := plan.Recipes[responseNode.Recipe]
	if responseRecipe.Tool != compactKbuildScriptRunnerRole || responseRecipe.ExecutionDirectory != directory {
		t.Fatalf("response-file producer = tool %q directory %q, want scriptrun in %q", responseRecipe.Tool, responseRecipe.ExecutionDirectory, directory)
	}
	responseScript := compactKbuildRecipeScriptContentForTest(t, responseRecipe)
	if !strings.Contains(responseScript, "../../../"+directory+"/") || !strings.Contains(responseScript, "leaf.o") {
		t.Fatalf("response-file producer script does not retain typed cwd paths: %q", responseScript)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("validate external response-file action plan: %v", err)
	}
}
