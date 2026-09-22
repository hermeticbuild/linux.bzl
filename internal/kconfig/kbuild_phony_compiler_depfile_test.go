package kconfig

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSourceCheckCompilerDepfileRequiresSelectedPrivateCompilerOutput(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "scripts", "Kbuild", "", "", nil)
	const target = "scripts/missing-syscalls"
	const depfile = "scripts/.missing-syscalls.d"
	bound := []string{"-interpreter", "${tool:script-runtime}", "-script", "${source:script:00000000}",
		"-tool", "cc=${tool:cc}", "--", "cc"}
	for _, test := range []struct {
		name      string
		directory string
		arguments []string
		tools     []string
		want      bool
	}{
		{name: "Kbuild compiler side depfile", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/"+depfile, "-E", "-x", "c", "-"), tools: []string{"cc"}, want: true},
		{name: "separate MF operand", arguments: append(slices.Clone(bound), "-MMD", "-MF", "${work:root}/"+depfile), tools: []string{"cc"}, want: true},
		{name: "root cwd relative output", arguments: append(slices.Clone(bound), "-Wp,-MMD,"+depfile), tools: []string{"cc"}, want: true},
		{name: "nested cwd relative output", directory: "scripts", arguments: append(slices.Clone(bound), "-Wp,-MMD,./.missing-syscalls.d"), tools: []string{"cc"}, want: true},
		{name: "nested cwd traversing alias", directory: "scripts", arguments: append(slices.Clone(bound), "-Wp,-MMD,../scripts/.missing-syscalls.d"), tools: []string{"cc"}},
		{name: "root cwd wrong relative output", arguments: append(slices.Clone(bound), "-Wp,-MMD,./.missing-syscalls.d"), tools: []string{"cc"}},
		{name: "source rooted output", arguments: append(slices.Clone(bound), "-Wp,-MMD,${tree:kernel}/"+depfile), tools: []string{"cc"}},
		{name: "different output", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/scripts/.other.d"), tools: []string{"cc"}},
		{name: "multiple depfiles", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/"+depfile, "-MF${work:root}/scripts/.other.d"), tools: []string{"cc"}},
		{name: "private and source-root depfile", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/"+depfile, "-MF${tree:kernel}/"+depfile), tools: []string{"cc"}},
		{name: "private and absolute depfile", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/"+depfile, "-MF/tmp/out.d"), tools: []string{"cc"}},
		{name: "missing compiler binding", arguments: append(slices.Clone(bound[:len(bound)-4]), "--", "cc", "-Wp,-MMD,${work:root}/"+depfile), tools: []string{"cc"}},
		{name: "wrong program", arguments: append(slices.Clone(bound[:len(bound)-1]), "printf", "-Wp,-MMD,${work:root}/"+depfile), tools: []string{"cc"}},
		{name: "undeclared tool role", arguments: append(slices.Clone(bound), "-Wp,-MMD,${work:root}/"+depfile)},
		{name: "no depfile flag", arguments: append(slices.Clone(bound), "${work:root}/"+depfile), tools: []string{"cc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			effects := compactKbuildSourceCheckCompilerDepfileEffects(profile, target, test.directory, test.arguments, test.tools)
			want := []ActionRecipePrivateWorkingEffect(nil)
			if test.want {
				want = []ActionRecipePrivateWorkingEffect{{Path: depfile, Kind: "regular"}}
			}
			if !slices.Equal(effects, want) {
				t.Fatalf("source check compiler private effects = %#v, want %#v", effects, want)
			}
		})
	}
}

func TestSourceCheckCompilerDepfileAcceptsRootedDestinationOutsideInvocationCwd(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "nested", "Kbuild", "", "", nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "scripts",
	}); err != nil {
		t.Fatal(err)
	}
	bound := []string{"-tool", "cc=${tool:cc}", "--", "cc"}
	for _, test := range []struct {
		name, destination string
		want              bool
	}{
		{"private-root output", "${work:root}/.missing-syscalls.d", true},
		{"relative parent traversal", "../.missing-syscalls.d", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := append(slices.Clone(bound), "-Wp,-MMD,"+test.destination)
			got := compactKbuildSourceCheckCompilerDepfileEffects(
				profile, "missing-syscalls", "scripts", arguments, []string{"cc"},
			)
			if allowed := len(got) == 1 && got[0].Path == ".missing-syscalls.d"; allowed != test.want {
				t.Fatalf("private compiler depfile effects=%#v for %q, want admitted=%t", got, test.destination, test.want)
			}
		})
	}
}

func TestSelectedOutputlessKbuildCompilerCheckRetainsOnlyPrivateDepfile(t *testing.T) {
	const target = "missing-syscalls"
	const script = "scripts/checksyscalls.sh"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "Kbuild", "", fmt.Sprintf(`
CONFIG_SHELL := sh
CC := %s
objtree := __LINUX_BZL_OBJECT_TREE__
depfile = $(objtree)/.$(notdir $@).d
c_flags = -Wp,-MMD,$(depfile) -E -x c -
always-y += missing-syscalls
cmd = $(cmd_$(1))
cmd_syscalls = $(CONFIG_SHELL) $< $(CC) $(c_flags)
missing-syscalls: scripts/checksyscalls.sh FORCE
	$(call cmd,syscalls)
`, KbuildActionRoleToken("target", "cc")), nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, script)
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
		Recipes: map[string]ActionRecipe{}, metadata: metadata,
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).forOutput("target", "objects", "sdk")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, found := compactKbuildPlanNode(plan, producer)
	if !found || node.Tool != compactKbuildScriptRunnerRole || len(node.Outputs) != 1 ||
		node.Outputs[0].ObservedPath != target {
		t.Fatalf("selected outputless check producer=%#v found=%t", node, found)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Equal(recipe.PrivateWorkingEffects, []ActionRecipePrivateWorkingEffect{{
		Path: ".missing-syscalls.d", Kind: "regular",
	}}) || recipe.RequireUnchangedWorkingTree || recipe.RequireAbsentObservedOutput != "00000000" ||
		len(recipe.WorkingOutputs) != 0 || !slices.Contains(recipe.AuxiliaryTools, "cc") {
		t.Fatalf("selected compiler check lost bounded private depfile/PHONY completion: %#v", recipe)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("write source-authenticated compiler status check plan: %v", err)
	}
}

func selectedPhonyCommandTemplateCheckPlan(t *testing.T, wrapper string, selectedLeaf ...string) (*ActionPlan, ActionPlanNode, ActionRecipe) {
	return selectedPhonyCommandTemplateCheckPlanWithCompilerScope(t, "target", false, false, wrapper, selectedLeaf...)
}

func selectedPhonyCommandTemplateCheckPlanWithCompilerScope(
	t *testing.T, compilerScope string, nativeWrapper, literalStatus bool, wrapper string, selectedLeaf ...string,
) (*ActionPlan, ActionPlanNode, ActionRecipe) {
	t.Helper()
	const target = "missing-syscalls"
	const script = "scripts/checksyscalls.sh"
	leaf := "$(CONFIG_SHELL) $< $(CC) $(c_flags)"
	if len(selectedLeaf) == 1 {
		leaf = selectedLeaf[0]
	} else if len(selectedLeaf) != 0 {
		t.Fatal("source-shaped fixture has multiple selected leaves")
	}
	commandDefinitions := `echo-cmd = $(if $($(quiet)cmd_$(1)),echo '  $($(quiet)cmd_$(1))';)
quiet_redirect =
delete-on-interrupt = $(if $(filter-out $(PHONY),$@),echo unexpected-trap;)`
	cFlags := "-Wp,-MMD,$(depfile) -E -x c -"
	status := "CALL    $<"
	if literalStatus {
		status += " $${tree:prep}"
	}
	if nativeWrapper {
		// Preserve the real cmd wrapper's Make expansion, including escsq,
		// source-selected status output and its PHONY-only trap suppression.
		commandDefinitions = `squote := '
escsq = $(subst $(squote),'\$(squote)',$1)
echo-cmd = $(if $($(quiet)cmd_$(1)),\
	echo '  $(call escsq,$($(quiet)cmd_$(1)))$(echo-why)';)
       redirect :=
 quiet_redirect :=
silent_redirect := exec >/dev/null;
delete-on-interrupt = \
	$(if $(filter-out $(PHONY), $@), \
		$(foreach sig, HUP INT QUIT TERM PIPE, \
			trap 'rm -f $@; trap - $(sig); kill -s $(sig) $$$$' $(sig);))`
		cFlags = "-Wp,-MMD,$(depfile) -I $(srctree) -I $(objtree) -E -x c -"
	}
	depfileDefinition := "depfile = $(objtree)/.$(notdir $@).d"
	if nativeWrapper {
		depfileDefinition = "dot-target = $(dir $@).$(notdir $@)\ndepfile = $(dot-target).d"
	}
	profile := mustCompactKbuildProfileForTest(t, "build:root", "Kbuild", "", fmt.Sprintf(`
CONFIG_SHELL := sh
CC := %s
objtree := __LINUX_BZL_OBJECT_TREE__
srctree := __LINUX_BZL_SOURCE_TREE__
%s
c_flags = %s
quiet := quiet_
quiet_cmd_syscalls = %s
%s
cmd = %s
cmd_syscalls = %s
PHONY += missing-syscalls
missing-syscalls: scripts/checksyscalls.sh
	$(call cmd,syscalls)
.PHONY: $(PHONY)
`, KbuildActionRoleToken(compilerScope, "cc"), depfileDefinition, cFlags, status, commandDefinitions, wrapper, leaf), nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "Kbuild", script)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:    testConfiguredScopedActionRoles,
		configFragment: map[string]string{},
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{}, metadata: metadata,
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	producer := graph.materializedProducers[compactKbuildSelectionKey{
		profile: profile.Name, target: target, stage: "target",
	}]
	node, found := compactKbuildPlanNode(plan, producer)
	if !found || producer == "" || len(node.Outputs) != 1 || node.Outputs[0].ObservedPath != target {
		t.Fatalf("PHONY command-template source check node=%#v found=%t", node, found)
	}
	if _, _, exists := planProducerByOutput(plan, "objects", target); exists {
		t.Fatal("PHONY source check fabricated a Make-visible output")
	}
	recipe := plan.Recipes[node.Recipe]
	return plan, node, recipe
}

func TestSelectedPhonyCommandTemplateRunsSourceCheckWithPrivateDepfile(t *testing.T) {
	const target = "missing-syscalls"
	const script = "scripts/checksyscalls.sh"
	plan, node, recipe := selectedPhonyCommandTemplateCheckPlan(t,
		"@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))")
	if !slices.Equal(recipe.PrivateWorkingEffects, []ActionRecipePrivateWorkingEffect{{
		Path: ".missing-syscalls.d", Kind: "regular",
	}}) || recipe.RequireUnchangedWorkingTree || recipe.RequireAbsentObservedOutput != planOrdinal(0) ||
		len(recipe.WorkingOutputs) != 0 || recipe.MakePhonyCompletion == nil ||
		recipe.MakePhonyCompletion.ScriptPath != script ||
		recipe.MakePhonyCompletion.SelectedLine == "" ||
		!compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, target) {
		t.Fatalf("selected PHONY command lost script, Make receipt, or bounded depfile: %#v", recipe)
	}
	for _, test := range []struct {
		name   string
		change func(*ActionRecipe)
	}{
		{"selected compiler argument", func(r *ActionRecipe) {
			r.MakePhonyCompletion.SelectedLine = strings.Replace(r.MakePhonyCompletion.SelectedLine,
				"-Wp,-MMD,${tree:prep}/.missing-syscalls.d", "-Wp,-MMD,${tree:prep}/.other.d", 1)
		}},
		{"selected wrapper effect", func(r *ActionRecipe) {
			r.MakePhonyCompletion.SelectedLine = strings.Replace(r.MakePhonyCompletion.SelectedLine,
				"set -e; echo", "set -e; true; echo", 1)
		}},
		{"selected shell quoting", func(r *ActionRecipe) {
			const selected = "echo '  CALL    ${tree:kernel}/scripts/checksyscalls.sh'"
			if !strings.Contains(r.MakePhonyCompletion.SelectedLine, selected) {
				t.Fatal("selected PHONY fixture lost its quoted source status command")
			}
			r.MakePhonyCompletion.SelectedLine = strings.Replace(r.MakePhonyCompletion.SelectedLine,
				selected, "echo \"  CALL    ${tree:kernel}/scripts/checksyscalls.sh\"", 1)
		}},
		{"executed compiler argument", func(r *ActionRecipe) {
			r.MakePhonyCompletion.ExpandedLine = strings.Replace(r.MakePhonyCompletion.ExpandedLine,
				"-Wp,-MMD,.missing-syscalls.d", "-Wp,-MMD,.other.d", 1)
			r.Arguments[9] = base64.StdEncoding.EncodeToString([]byte(
				"#!/bin/sh\nset -e\n" + r.MakePhonyCompletion.ExpandedLine + "\n"))
		}},
		{"claimed private depfile", func(r *ActionRecipe) {
			r.PrivateWorkingEffects[0].Path = ".other.d"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			altered := cloneActionRecipe(recipe)
			receipt := *recipe.MakePhonyCompletion
			altered.MakePhonyCompletion = &receipt
			altered.Arguments = slices.Clone(recipe.Arguments)
			altered.PrivateWorkingEffects = slices.Clone(recipe.PrivateWorkingEffects)
			test.change(&altered)
			if err := validateActionRecipeMakePhonyCompletion(altered); err == nil {
				t.Fatal("altered selected compiler command or claimed private effect passed receipt validation")
			}
		})
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("PHONY command-template check plan: %v", err)
	}
}

func TestSelectedPhonyNativeCmdWrapperAuthenticatesSourceAndDepfile(t *testing.T) {
	const wrapper = "@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))"
	plan, node, recipe := selectedPhonyCommandTemplateCheckPlanWithCompilerScope(
		t, "target", true, false, wrapper,
		"$(CONFIG_SHELL) $< $(CC) $(c_flags) $(missing_syscalls_flags)",
	)
	if recipe.MakePhonyCompletion == nil ||
		!strings.Contains(recipe.MakePhonyCompletion.SelectedLine, "CALL") ||
		!strings.Contains(recipe.MakePhonyCompletion.SelectedLine, "-Wp,-MMD,./.missing-syscalls.d") ||
		!strings.Contains(recipe.MakePhonyCompletion.ExpandedLine, "-Wp,-MMD,./.missing-syscalls.d") ||
		!strings.Contains(recipe.MakePhonyCompletion.SelectedLine, "-I ${tree:prep} -E") ||
		!strings.Contains(recipe.MakePhonyCompletion.ExpandedLine, "-I . -E") ||
		strings.Contains(recipe.MakePhonyCompletion.ExpandedLine, "trap 'rm -f") ||
		!slices.Equal(recipe.PrivateWorkingEffects, []ActionRecipePrivateWorkingEffect{{
			Path: ".missing-syscalls.d", Kind: "regular",
		}}) || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, "missing-syscalls") {
		t.Fatalf("native cmd wrapper lost its selected source, PHONY trap suppression, or compiler depfile: %#v", recipe)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("native cmd wrapper action plan: %v", err)
	}
}

func TestSelectedPhonyWrapperPreservesLiteralAndActiveTreeRoots(t *testing.T) {
	const wrapper = "@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))"
	plan, node, recipe := selectedPhonyCommandTemplateCheckPlanWithCompilerScope(
		t, "target", true, true, wrapper,
	)
	completion := recipe.MakePhonyCompletion
	if completion == nil ||
		!strings.Contains(completion.SelectedLine, "-I ${tree:prep} -E") ||
		!strings.Contains(completion.SelectedLine, compactKbuildLiteralTreeEscapeByte+"{tree:prep}") ||
		!strings.Contains(completion.ExpandedLine, "-I . -E") ||
		!strings.Contains(completion.ExpandedLine, "'  CALL    ${tree:kernel}/scripts/checksyscalls.sh ${tree:prep}'") ||
		!compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, "missing-syscalls") {
		t.Fatalf("PHONY wrapper lost literal or active tree-root provenance: %#v", recipe)
	}
	offsets, err := actionRecipeLiteralTreeOffsetArguments(recipe.Arguments)
	if err != nil || len(offsets) != 1 {
		t.Fatalf("PHONY literal tree offset arguments=%#v, error=%v", offsets, err)
	}
	for _, test := range []struct {
		name   string
		change func(*ActionRecipe)
	}{
		{"changed literal offset", func(r *ActionRecipe) {
			r.Arguments[offsets[0].valueIndex] = strconv.Itoa(offsets[0].offset + 1)
		}},
		{"literal rewritten as active", func(r *ActionRecipe) {
			completion := *r.MakePhonyCompletion
			completion.SelectedLine = strings.Replace(completion.SelectedLine,
				compactKbuildLiteralTreeEscapeByte+"{tree:prep}", "${tree:prep}", 1)
			r.MakePhonyCompletion = &completion
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			altered := cloneActionRecipe(recipe)
			altered.Arguments = slices.Clone(recipe.Arguments)
			test.change(&altered)
			if err := validateActionRecipeMakePhonyCompletion(altered); err == nil {
				t.Fatal("changed literal provenance passed selected PHONY source validation")
			}
		})
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("literal PHONY wrapper action plan: %v", err)
	}
}

func TestSelectedPhonyCommandWrapperExecutesPreleafStatus(t *testing.T) {
	for _, test := range []struct {
		name, wrapper string
		wantFailure   bool
	}{
		{"ordinary wrapper", "@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))", false},
		{"failed setup before source script", "@set -e; false; $(cmd_$(1))", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, recipe := selectedPhonyCommandTemplateCheckPlan(t, test.wrapper)
			content, err := base64.StdEncoding.DecodeString(recipe.Arguments[9])
			if err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(t.TempDir(), "checksyscalls.sh")
			marker := filepath.Join(t.TempDir(), "ran-source-script")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			projected := strings.ReplaceAll(string(content), "${tree:kernel}/scripts/checksyscalls.sh", script)
			if output, err := exec.Command("sh", "-c", projected).CombinedOutput(); (err != nil) != test.wantFailure {
				t.Fatalf("selected wrapper status=%v output=%q, want failure=%t", err, output, test.wantFailure)
			}
			_, err = os.Stat(marker)
			if (err == nil) == test.wantFailure {
				t.Fatalf("source script execution marker error=%v, want failure=%t", err, test.wantFailure)
			}
		})
	}
}

func TestSelectedPhonyCommandWrapperWithoutCompilerRetainsUnchangedTree(t *testing.T) {
	plan, node, recipe := selectedPhonyCommandTemplateCheckPlan(t,
		"@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))", "$(CONFIG_SHELL) $<")
	if len(recipe.PrivateWorkingEffects) != 0 || !recipe.RequireUnchangedWorkingTree ||
		recipe.MakePhonyCompletion == nil ||
		!compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, "missing-syscalls") {
		t.Fatalf("wrapped source check without compiler lost unchanged-tree receipt: %#v", recipe)
	}
}

func TestSelectedPhonyCommandWrapperBindsOppositeScopeCompiler(t *testing.T) {
	const wrapper = "@set -e; $(echo-cmd) $($(quiet)redirect) $(delete-on-interrupt) $(cmd_$(1))"
	plan, node, recipe := selectedPhonyCommandTemplateCheckPlanWithCompilerScope(t, "host", false, false, wrapper)
	if recipe.MakePhonyCompletion == nil || recipe.MakePhonyCompletion.ActionScope != "target" ||
		!slices.Contains(recipe.AuxiliaryTools, "host@cc") ||
		!compactKbuildRecipeBindsAuxiliaryTool(recipe, "host@cc") ||
		!strings.Contains(recipe.MakePhonyCompletion.ExpandedLine, "host@cc") ||
		!slices.Equal(recipe.PrivateWorkingEffects, []ActionRecipePrivateWorkingEffect{{
			Path: ".missing-syscalls.d", Kind: "regular",
		}}) || !compactKbuildAuthenticatedExecutionCheckCompletion(plan, node, "missing-syscalls") {
		t.Fatalf("opposite-scope compiler lost its source-bound executable or private depfile: %#v", recipe)
	}
	altered := cloneActionRecipe(recipe)
	altered.MakePhonyCompletion.ActionScope = "host"
	if err := validateActionRecipeMakePhonyCompletion(altered); err == nil {
		t.Fatal("changing the receipt's action scope preserved its compiler authority")
	}
	altered = cloneActionRecipe(recipe)
	altered.AuxiliaryTools = []string{"cc"}
	if err := validateActionRecipeMakePhonyCompletion(altered); err == nil {
		t.Fatal("unqualified compiler satisfied an explicitly host-scoped source role")
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("write opposite-scope source-script receipt: %v", err)
	}
}
