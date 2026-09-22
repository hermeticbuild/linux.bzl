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

func compactKbuildLinkVmlinuxPlanFixture(t *testing.T) CompactConfig {
	return compactKbuildLinkVmlinuxPlanFixtureWithCommand(t,
		"sh scripts/link-vmlinux.sh ld -z defs; printf '%s\\n' 'cmd_vmlinux := sh scripts/link-vmlinux.sh ld -z defs' > .vmlinux.cmd")
}

func compactKbuildLinkVmlinuxPlanFixtureWithCommand(t *testing.T, command string) CompactConfig {
	return compactKbuildLinkVmlinuxPlanFixtureWithExports(t, command, "", nil)
}

func compactKbuildLinkVmlinuxPlanFixtureWithExports(t *testing.T, command, exports string, injected map[string]string) CompactConfig {
	t.Helper()
	config := compactKbuildSourcePhaseGraphFixture(t)
	original := config.KbuildProfiles[0]
	parent := mustCompactKbuildProfileForTest(t, original.Name, original.Path, original.Directory, `
export MAKE OBJCOPY CONFIG_SHELL `+exports+`
cmd_vmlinux = `+command+`
vmlinux: scripts/link-vmlinux.sh
	$(call if_changed,vmlinux)
`, func() map[string]string {
		values := map[string]string{
			"MAKE":         CompactKbuildRecursiveMakeProvenanceToken,
			"OBJCOPY":      KbuildActionRoleToken("target", "objcopy"),
			"CONFIG_SHELL": "sh",
		}
		for name, value := range injected {
			values[name] = value
		}
		return values
	}())
	parent.evaluator.template.sourceRoots = original.evaluator.template.sourceRoots
	if err := SetCompactKbuildProfileInvocationLocation(&parent, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	parent.evaluator.template.virtualFileView = &testKbuildVirtualFileView{
		files: map[string]testKbuildVirtualFile{
			"__LINUX_BZL_OBJECT_TREE__/include/config/auto.conf": {
				content: "CONFIG_LOCALVERSION=\"\"\nCONFIG_LOCALVERSION_AUTO=n\n", exact: true,
			},
		},
	}
	parent.SelectedSourceScriptPhases = original.SelectedSourceScriptPhases
	parent.TargetInvocationDependencies = original.TargetInvocationDependencies
	parent.TargetInvocationDependencies[0].ReplayArguments = []string{
		"-f", "__LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build", "obj=init", "need-builtin=1",
	}
	parent.TargetInvocationDependencies[1].ReplayArguments = []string{
		"-f", "__LINUX_BZL_SOURCE_TREE__/scripts/Makefile.modpost", "MODPOST_VMLINUX=1",
	}
	config.KbuildProfiles[0] = parent
	return config
}

func compactKbuildLinkVmlinuxActionPlan(t *testing.T, config CompactConfig) (*ActionPlan, *compactKbuildSelectionGraph) {
	t.Helper()
	metadata := &CompactMetadata{

		Config: config, configFragment: map[string]string{},
		actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{
		Recipes:  map[string]ActionRecipe{},
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
	}
	if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
		t.Fatal(err)
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatalf("lower source-selected link-vmlinux phases: %v", err)
	}
	return plan, graph
}

func compactKbuildLinkVmlinuxExportedHostProgramFixture(t *testing.T, withWriter bool) CompactConfig {
	t.Helper()
	const program = "tools/bpf/resolve_btfids/resolve_btfids"
	config := compactKbuildLinkVmlinuxPlanFixtureWithExports(t,
		"sh scripts/link-vmlinux.sh ld -z defs; printf '%s\\n' 'cmd_vmlinux := sh scripts/link-vmlinux.sh ld -z defs' > .vmlinux.cmd",
		"RESOLVE_BTFIDS", map[string]string{"RESOLVE_BTFIDS": "__LINUX_BZL_OBJECT_TREE__/" + program})
	parent := &config.KbuildProfiles[0]
	root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	script := strings.Replace(compactKbuildLinkVmlinuxFixture(true),
		"mksysmap vmlinux System.map\n", `mksysmap vmlinux System.map
if [ -n "${CONFIG_DEBUG_INFO_BTF}" -a -n "${CONFIG_BPF}" ]; then
	${RESOLVE_BTFIDS} vmlinux
fi
`, 1)
	analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(script)
	if err != nil || !selected {
		t.Fatalf("analyze source selected executable call: selected=%t, error=%v", selected, err)
	}
	mustWriteSource(t, root, "scripts/link-vmlinux.sh", script)
	for index := range parent.SelectedSourceScriptPhases {
		phase := &parent.SelectedSourceScriptPhases[index]
		phase.SourceSHA256 = analyzed.SourceSHA256
		if phase.Ordinal == 0 {
			phase.Spans = slices.Clone(analyzed.VersionSpans)
		} else {
			phase.Spans = slices.Clone(analyzed.ObjectSpans)
		}
	}
	parent.evaluator.template.virtualFileView = &testKbuildVirtualFileView{
		files: map[string]testKbuildVirtualFile{
			"__LINUX_BZL_OBJECT_TREE__/include/config/auto.conf": {
				content: "CONFIG_BPF=y\nCONFIG_DEBUG_INFO_BTF=y\nCONFIG_LOCALVERSION=\"\"\nCONFIG_LOCALVERSION_AUTO=n\n", exact: true,
			},
		},
	}
	if withWriter {
		profile := mustCompactKbuildProfileForTest(t, "host-btfids", "tools/bpf/resolve_btfids/Makefile", "", `
`+program+`: FORCE
	printf '#!/bin/sh\nexit 0\n' > $@
	chmod +x $@
`, nil)
		config.KbuildProfiles = append(config.KbuildProfiles, profile)
		config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
			Profile: profile.Name, Target: program, MakeTarget: program,
			Lifecycle: "prep", Scope: "host", Stage: "host",
		})
	}
	return config
}

func compactKbuildLinkVmlinuxLiteralHostProgramFixture(t *testing.T, withWriter bool) CompactConfig {
	t.Helper()
	const program = "scripts/kallsyms"
	config := compactKbuildLinkVmlinuxPlanFixture(t)
	parent := &config.KbuildProfiles[0]
	root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	script := strings.Replace(compactKbuildLinkVmlinuxFixture(true),
		"vmlinux_link()\n{\n\t${LD} ${KBUILD_LDFLAGS} -o ${1} ${KBUILD_VMLINUX_OBJS}\n}",
		"vmlinux_link()\n{\n\tscripts/kallsyms vmlinux\n\t${LD} ${KBUILD_LDFLAGS} -o ${1} ${KBUILD_VMLINUX_OBJS}\n}", 1)
	analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(script)
	if err != nil || !selected {
		t.Fatalf("analyze source selected literal program: selected=%t error=%v", selected, err)
	}
	mustWriteSource(t, root, "scripts/link-vmlinux.sh", script)
	for index := range parent.SelectedSourceScriptPhases {
		phase := &parent.SelectedSourceScriptPhases[index]
		phase.SourceSHA256 = analyzed.SourceSHA256
		if phase.Ordinal == 0 {
			phase.Spans = slices.Clone(analyzed.VersionSpans)
		} else {
			phase.Spans = slices.Clone(analyzed.ObjectSpans)
		}
	}
	if withWriter {
		profile := mustCompactKbuildProfileForTest(t, "host-kallsyms", "scripts/Makefile.host", "", `
scripts/kallsyms: FORCE
	printf '#!/bin/sh\nexit 0\n' > $@
	chmod +x $@
`, nil)
		config.KbuildProfiles = append(config.KbuildProfiles, profile)
		config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
			Profile: profile.Name, Target: program, MakeTarget: program,
			Lifecycle: "prep", Scope: "host", Stage: "host",
		})
	}
	return config
}

func compactKbuildLinkVmlinuxObjectRootHostProgramFixture(t *testing.T, rootValue string) CompactConfig {
	return compactKbuildLinkVmlinuxPreludeHostProgramFixture(t,
		"${HELPER_ROOT}/scripts/sorttable", "HELPER_ROOT", map[string]string{"HELPER_ROOT": rootValue})
}

func compactKbuildLinkVmlinuxPreludeHostProgramFixture(
	t *testing.T, commandHead, exports string, values map[string]string,
) CompactConfig {
	t.Helper()
	const program = "scripts/sorttable"
	config := compactKbuildLinkVmlinuxPlanFixtureWithExports(t,
		"sh scripts/link-vmlinux.sh ld -z defs; printf '%s\\n' 'cmd_vmlinux := sh scripts/link-vmlinux.sh ld -z defs' > .vmlinux.cmd",
		exports, values)
	parent := &config.KbuildProfiles[0]
	root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	script := strings.Replace(compactKbuildLinkVmlinuxFixture(true),
		"vmlinux_link()\n{\n\t${LD} ${KBUILD_LDFLAGS} -o ${1} ${KBUILD_VMLINUX_OBJS}\n}",
		"vmlinux_link()\n{\n\t"+commandHead+" vmlinux\n\t${LD} ${KBUILD_LDFLAGS} -o ${1} ${KBUILD_VMLINUX_OBJS}\n}", 1)
	if !strings.Contains(script, commandHead+" vmlinux") {
		t.Fatal("source phase fixture has no exported-root command head")
	}
	analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(script)
	if err != nil || !selected {
		t.Fatalf("analyze selected exported-root executable: selected=%t error=%v", selected, err)
	}
	mustWriteSource(t, root, "scripts/link-vmlinux.sh", script)
	for index := range parent.SelectedSourceScriptPhases {
		phase := &parent.SelectedSourceScriptPhases[index]
		phase.SourceSHA256 = analyzed.SourceSHA256
		if phase.Ordinal == 0 {
			phase.Spans = slices.Clone(analyzed.VersionSpans)
		} else {
			phase.Spans = slices.Clone(analyzed.ObjectSpans)
		}
	}
	profile := mustCompactKbuildProfileForTest(t, "host-sorttable", "scripts/Makefile.host", "", `
scripts/sorttable: FORCE
	printf '#!/bin/sh\nexit 0\n' > $@
	chmod +x $@
`, nil)
	config.KbuildProfiles = append(config.KbuildProfiles, profile)
	config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
		Profile: profile.Name, Target: program, MakeTarget: program,
		Lifecycle: "prep", Scope: "host", Stage: "host",
	})
	return config
}

func TestLinkVmlinuxPreludeStagesWholeExportedHostExecutable(t *testing.T) {
	const program = "scripts/sorttable"
	config := compactKbuildLinkVmlinuxPreludeHostProgramFixture(t,
		"${HELPER_PROGRAM}", "HELPER_PROGRAM",
		map[string]string{"HELPER_PROGRAM": "__LINUX_BZL_OBJECT_TREE__/" + program})
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-sorttable", program)
	final := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, "vmlinux")
	recipe := plan.Recipes[final.Recipe]
	bound := false
	for binding, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		name := strings.TrimPrefix(binding, "input:")
		bound = name != binding && slices.Contains(recipe.ExecutableInputs, name) &&
			slices.ContainsFunc(final.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			})
	}
	if !bound || !slices.Contains(compactKbuildLinkVmlinuxStagedInputPaths(t, plan, final), program) {
		t.Fatalf("prelude exported executable lost its selected host writer: final=%#v recipe=%#v", final, recipe)
	}
}

func TestLinkVmlinuxFinalScansOnlyExecutedSourceSpans(t *testing.T) {
	config := compactKbuildLinkVmlinuxPlanFixture(t)
	profile := config.KbuildProfiles[0]
	source, err := readCompactKbuildProfileSource(profile, "scripts/link-vmlinux.sh")
	if err != nil {
		t.Fatal(err)
	}
	analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(string(source))
	if err != nil || !selected {
		t.Fatalf("analyze selected final source: selected=%t error=%v", selected, err)
	}
	whole, err := compactKbuildSourceScriptUsage(profile, "scripts/link-vmlinux.sh")
	if err != nil || !whole.programVariables["MAKE"] {
		t.Fatalf("whole source lacks removed recursive Make program head: usage=%#v error=%v", whole.programVariables, err)
	}
	final, err := compactKbuildSourceScriptUsageWithSelectedContent(
		profile, "scripts/link-vmlinux.sh", analyzed.FinalScript,
	)
	if err != nil || final.programVariables["MAKE"] {
		t.Fatalf("final source retained program head from an earlier phase: usage=%#v error=%v", final.programVariables, err)
	}
	// The Make wrapper also scans child shell sources. Its environment scan
	// must not restore a command head from the excluded top-level spans.
	wrapper := "sh scripts/link-vmlinux.sh ld -z defs"
	usage, err := compactKbuildHermeticScriptEnvironmentUsageWithSelectedContent(
		profile, wrapper, nil, "scripts/link-vmlinux.sh", analyzed.FinalScript,
	)
	if err != nil || usage.programVariables["MAKE"] {
		t.Fatalf("final wrapper retained removed recursive Make program head: usage=%#v error=%v", usage.programVariables, err)
	}
}

func TestLinkVmlinuxFinalStagesExportedRootHostExecutable(t *testing.T) {
	const program = "scripts/sorttable"
	config := compactKbuildLinkVmlinuxObjectRootHostProgramFixture(t, "__LINUX_BZL_OBJECT_TREE__")
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-sorttable", program)
	final := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, "vmlinux")
	recipe := plan.Recipes[final.Recipe]
	bound := false
	for binding, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		name := strings.TrimPrefix(binding, "input:")
		if name == binding || !slices.Contains(recipe.ExecutableInputs, name) ||
			!slices.ContainsFunc(final.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			}) {
			t.Fatalf("source command head lost selected executable writer: final=%#v recipe=%#v", final, recipe)
		}
		bound = true
	}
	if !bound || !slices.Contains(compactKbuildLinkVmlinuxStagedInputPaths(t, plan, final), program) {
		t.Fatalf("exported object-root executable missing from final working tree: %#v", recipe)
	}
}

func TestLinkVmlinuxFinalRejectsExportedRootOutsidePrivateObjectTree(t *testing.T) {
	config := compactKbuildLinkVmlinuxObjectRootHostProgramFixture(t, "__LINUX_BZL_SOURCE_TREE__")
	usage, usageErr := compactKbuildSourceScriptUsage(config.KbuildProfiles[0], "scripts/link-vmlinux.sh")
	if usageErr != nil || !usage.objectProgramHeads["${HELPER_ROOT}/scripts/sorttable"] {
		t.Fatalf("selected source command-head usage=%#v error=%v", usage.objectProgramHeads, usageErr)
	}
	metadata := &CompactMetadata{

		Config: config, configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{
		"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity,
	}}
	if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
		t.Fatal(err)
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err == nil || !strings.Contains(err.Error(), "no exported private object-root value") {
		if err == nil {
			final := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, "vmlinux")
			t.Fatalf("untrusted exported executable root was accepted: environment HELPER_ROOT=%q", plan.Recipes[final.Recipe].Environment["HELPER_ROOT"])
		}
		t.Fatalf("untrusted exported executable root was accepted: %v", err)
	}
}

func TestLinkVmlinuxFinalStagesLiteralHostProgramFromSelectedWriter(t *testing.T) {
	const program = "scripts/kallsyms"
	config := compactKbuildLinkVmlinuxLiteralHostProgramFixture(t, true)
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-kallsyms", program)
	final := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, "vmlinux")
	recipe := plan.Recipes[final.Recipe]
	bound := false
	for binding, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		name := strings.TrimPrefix(binding, "input:")
		if name == binding || !slices.Contains(recipe.ExecutableInputs, name) ||
			!slices.ContainsFunc(final.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			}) {
			t.Fatalf("literal helper has no selected executable host producer: recipe=%#v node=%#v", recipe, final)
		}
		bound = true
	}
	if !bound || !slices.Contains(compactKbuildLinkVmlinuxStagedInputPaths(t, plan, final), program) {
		t.Fatalf("literal generated helper is absent from final link workspace: %#v", recipe)
	}
}

func TestLinkVmlinuxPreludeLiteralRejectsUnselectedWorkingExecutable(t *testing.T) {
	const program = "scripts/kallsyms"
	config := compactKbuildLinkVmlinuxLiteralHostProgramFixture(t, false)
	metadata := &CompactMetadata{

		Config: config, configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{
		"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity,
	}}
	if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: program}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-literal", "unselected executable", "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err == nil ||
		!strings.Contains(err.Error(), "unselected plan producer") {
		t.Fatalf("unselected executable occupied authenticated prelude command head: %v", err)
	}
}

func TestSelectedSourceScriptStagesLiteralGeneratedHostProgram(t *testing.T) {
	const program = "scripts/kallsyms"
	const target = "include/generated/helper.h"
	config := compactKbuildLinkVmlinuxLiteralHostProgramFixture(t, true)
	root := config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, "scripts/build-helper.sh", "#!/bin/sh\nset -e\nscripts/kallsyms > \"$1\"\n")
	profile := mustCompactKbuildProfileForTest(t, "helper-script", "scripts/Makefile.build", "", `
include/generated/helper.h: scripts/build-helper.sh FORCE
	sh scripts/build-helper.sh $@
`, nil)
	profile.evaluator.template.sourceRoots = config.KbuildProfiles[0].evaluator.template.sourceRoots
	observed, observeErr := EvaluateCompactKbuildSourceScriptObjectTreeObservation(
		profile, target, "", []string{"scripts/build-helper.sh"}, nil, nil,
		"sh scripts/build-helper.sh "+target,
	)
	if observeErr != nil || observed.ObservesAll || !slices.Contains(observed.References, program) {
		t.Fatalf("selected source-script helper source observation=%#v error=%v", observed, observeErr)
	}
	config.KbuildProfiles = append(config.KbuildProfiles, profile)
	config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
		Profile: profile.Name, Target: target, MakeTarget: target,
		Lifecycle: "target", Scope: "target", Stage: "target",
	})
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-kallsyms", program)
	consumer := compactKbuildOverwriteTestNode(t, plan, graph, profile.Name, target)
	recipe := plan.Recipes[consumer.Recipe]
	bound := false
	for key, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		binding := strings.TrimPrefix(key, "input:")
		bound = binding != key && slices.Contains(recipe.ExecutableInputs, binding) &&
			slices.ContainsFunc(consumer.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			})
	}
	if !bound {
		t.Fatalf("ordinary source script lost selected generated helper: node=%#v recipe=%#v", consumer, recipe)
	}
}

func TestSelectedSourceScriptStagesExportedRootGeneratedHostProgram(t *testing.T) {
	const program = "scripts/kallsyms"
	const target = "include/generated/helper.h"
	config := compactKbuildLinkVmlinuxLiteralHostProgramFixture(t, true)
	root := config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, "scripts/build-helper.sh", "#!/bin/sh\nset -e\n${HELPER_ROOT}/scripts/kallsyms > \"$1\"\n")
	profile := mustCompactKbuildProfileForTest(t, "helper-script", "scripts/Makefile.build", "", `
export HELPER_ROOT
include/generated/helper.h: scripts/build-helper.sh FORCE
	sh scripts/build-helper.sh $@
`, map[string]string{"HELPER_ROOT": "__LINUX_BZL_OBJECT_TREE__"})
	profile.evaluator.template.sourceRoots = config.KbuildProfiles[0].evaluator.template.sourceRoots
	observed, observeErr := EvaluateCompactKbuildSourceScriptObjectTreeObservation(
		profile, target, "", []string{"scripts/build-helper.sh"}, nil, nil,
		"sh scripts/build-helper.sh "+target,
	)
	if observeErr != nil || observed.ObservesAll || !slices.Contains(observed.References, program) {
		t.Fatalf("exported-root executable source observation=%#v error=%v", observed, observeErr)
	}
	config.KbuildProfiles = append(config.KbuildProfiles, profile)
	config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
		Profile: profile.Name, Target: target, MakeTarget: target,
		Lifecycle: "target", Scope: "target", Stage: "target",
	})
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-kallsyms", program)
	consumer := compactKbuildOverwriteTestNode(t, plan, graph, profile.Name, target)
	recipe := plan.Recipes[consumer.Recipe]
	bound := false
	for key, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		binding := strings.TrimPrefix(key, "input:")
		bound = binding != key && slices.Contains(recipe.ExecutableInputs, binding) &&
			slices.ContainsFunc(consumer.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			})
	}
	if !bound {
		t.Fatalf("ordinary selected script lost exported-root generated executable: node=%#v recipe=%#v", consumer, recipe)
	}
}

func TestLinkVmlinuxFinalStagesExportedHostProgramFromSelectedWriter(t *testing.T) {
	const program = "tools/bpf/resolve_btfids/resolve_btfids"
	config := compactKbuildLinkVmlinuxExportedHostProgramFixture(t, true)
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	host := compactKbuildOverwriteTestNode(t, plan, graph, "host-btfids", program)
	final := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, "vmlinux")
	recipe := plan.Recipes[final.Recipe]
	bound := false
	for binding, pathname := range recipe.WorkingInputs {
		if pathname != program {
			continue
		}
		name := strings.TrimPrefix(binding, "input:")
		if name == binding || !slices.Contains(recipe.ExecutableInputs, name) ||
			!slices.ContainsFunc(final.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == host.ID && input.Role == "prerequisite"
			}) {
			t.Fatalf("final script program %q is not bound to selected executable host writer: recipe=%#v node=%#v host=%#v", program, recipe, final, host)
		}
		bound = true
	}
	if !bound || !slices.Contains(compactKbuildLinkVmlinuxStagedInputPaths(t, plan, final), program) {
		t.Fatalf("final script lacks staged source selected host program: %#v", recipe)
	}
	prep, _, exists := planProducerByOutput(plan, "prep", program)
	prepNode, present := compactKbuildPlanNode(plan, prep)
	if !exists || !present || prep == host.ID || !slices.Contains(plan.Recipes[prepNode.Recipe].Arguments, "-preserve_mode") {
		t.Fatalf("host program prep projection lost its executable mode: host=%q prep=%q", host.ID, prep)
	}
}

func TestLinkVmlinuxFinalRejectsUnselectedProgramAlreadyInWorkingTree(t *testing.T) {
	config := compactKbuildLinkVmlinuxExportedHostProgramFixture(t, false)
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
	if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "tools/bpf/resolve_btfids/resolve_btfids"}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-literal", "unselected executable", "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err == nil || !strings.Contains(err.Error(), "unselected plan producer") {
		t.Fatalf("exported program occupied by unselected working file: %v", err)
	}
}

func TestLinkVmlinuxFinalLeavesAbsentExportedProgramForRuntimeShell(t *testing.T) {
	for _, test := range []struct {
		name, config  string
		unconditional bool
	}{
		{name: "guard disabled", config: "CONFIG_DEBUG_INFO_BTF=y\n"},
		{name: "nonempty n runs the source shell branch", config: "CONFIG_BPF=n\nCONFIG_DEBUG_INFO_BTF=y\n"},
		{name: "unconditional command stays in source", config: "CONFIG_DEBUG_INFO_BTF=y\n", unconditional: true},
		{name: "modified guard variable stays in source", config: "CONFIG_DEBUG_INFO_BTF=y\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildLinkVmlinuxExportedHostProgramFixture(t, false)
			parent := &config.KbuildProfiles[0]
			parent.evaluator.template.virtualFileView = &testKbuildVirtualFileView{
				files: map[string]testKbuildVirtualFile{
					"__LINUX_BZL_OBJECT_TREE__/include/config/auto.conf": {
						content: test.config + "CONFIG_LOCALVERSION=\"\"\nCONFIG_LOCALVERSION_AUTO=n\n", exact: true,
					},
				},
			}
			if test.unconditional || test.name == "modified guard variable" {
				root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
				script, err := os.ReadFile(filepath.Join(root, "scripts/link-vmlinux.sh"))
				if err != nil {
					t.Fatal(err)
				}
				content := string(script)
				if test.unconditional {
					content = strings.Replace(content,
						"if [ -n \"${CONFIG_DEBUG_INFO_BTF}\" -a -n \"${CONFIG_BPF}\" ]; then\n\t${RESOLVE_BTFIDS} vmlinux\nfi\n",
						"${RESOLVE_BTFIDS} vmlinux\n", 1)
				} else {
					content = strings.Replace(content,
						"if [ -n \"${CONFIG_DEBUG_INFO_BTF}\" -a -n \"${CONFIG_BPF}\" ]; then\n",
						"CONFIG_BPF=y\nif [ -n \"${CONFIG_DEBUG_INFO_BTF}\" -a -n \"${CONFIG_BPF}\" ]; then\n", 1)
				}
				analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(content)
				if err != nil || !selected {
					t.Fatalf("analyze changed final script: selected=%t error=%v", selected, err)
				}
				mustWriteSource(t, root, "scripts/link-vmlinux.sh", content)
				for index := range parent.SelectedSourceScriptPhases {
					phase := &parent.SelectedSourceScriptPhases[index]
					phase.SourceSHA256 = analyzed.SourceSHA256
					if phase.Ordinal == 0 {
						phase.Spans = slices.Clone(analyzed.VersionSpans)
					} else {
						phase.Spans = slices.Clone(analyzed.ObjectSpans)
					}
				}
			}
			metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}, actionRoles: testConfiguredScopedActionRoles}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
			if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
				t.Fatal(err)
			}
			graph, err := metadata.appendGeneratedActionPlan(plan)
			if err != nil {
				t.Fatalf("absent exported program was bound or its source branch was interpreted: %v", err)
			}
			final := compactKbuildOverwriteTestNode(t, plan, graph, parent.Name, "vmlinux")
			if recipe := plan.Recipes[final.Recipe]; len(recipe.ExecutableInputs) != 0 ||
				slices.Contains(compactKbuildLinkVmlinuxStagedInputPaths(t, plan, final), "tools/bpf/resolve_btfids/resolve_btfids") ||
				recipe.Environment["RESOLVE_BTFIDS"] != "${work:root}/tools/bpf/resolve_btfids/resolve_btfids" {
				t.Fatalf("unselected executable gained authority or source command changed: %#v", recipe)
			}
		})
	}
}

func compactKbuildLinkVmlinuxStagedInputPaths(t *testing.T, plan *ActionPlan, node ActionPlanNode) []string {
	t.Helper()
	paths := []string{}
	for _, pathname := range plan.Recipes[node.Recipe].WorkingInputs {
		paths = append(paths, pathname)
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Walk(node.InputSet, func(entry ActionPlanInputSetEntry) error {
		if entry.Target.Kind == ActionPlanInputSetWorkTarget {
			paths = append(paths, entry.Target.Path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestLinkVmlinuxSourcePhaseSeparatesConfigImportFromShellChild(t *testing.T) {
	source := compactKbuildLinkVmlinuxFixture(true)
	analyzed, found, err := AnalyzeCompactKbuildLinkVmlinuxPhases(source)
	if err != nil || !found {
		t.Fatalf("analyze selected source: found=%t, error=%v", found, err)
	}
	for _, script := range []string{analyzed.VersionScript, analyzed.ObjectScript} {
		scan, err := scanCompactKbuildSourceScript(script)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(scan.sources, "scripts/mksysmap") ||
			!slices.Equal(scan.dotSources, []string{"include/config/auto.conf"}) {
			t.Fatalf("source scripts = %q, dot imports = %q", scan.sources, scan.dotSources)
		}
	}
	unsafe := strings.Replace(source, "\tlocal objects\n", "\t. include/config/other.conf\n\tlocal objects\n", 1)
	analyzed, found, err = AnalyzeCompactKbuildLinkVmlinuxPhases(unsafe)
	if err != nil || !found {
		t.Fatalf("analyze selected extra import: found=%t, error=%v", found, err)
	}
	scan, err := scanCompactKbuildSourceScript(analyzed.ObjectScript)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(scan.dotSources, []string{"include/config/auto.conf", "include/config/other.conf"}) {
		t.Fatalf("extra dot import lost: %q", scan.dotSources)
	}
	unsafe = strings.Replace(source, "\tlocal objects\n", "\t. \"${other_config}\"\n\tlocal objects\n", 1)
	analyzed, found, err = AnalyzeCompactKbuildLinkVmlinuxPhases(unsafe)
	if err != nil || !found {
		t.Fatalf("analyze selected dynamic import: found=%t, error=%v", found, err)
	}
	scan, err = scanCompactKbuildSourceScript(analyzed.ObjectScript)
	if err != nil || !scan.dynamicDotSource {
		t.Fatalf("dynamic dot import not rejected: detected=%t, error=%v", scan.dynamicDotSource, err)
	}
}

func TestLinkVmlinuxSelectedPhaseRejectsUnreplayedRecursiveMakeRead(t *testing.T) {
	config := compactKbuildLinkVmlinuxPlanFixture(t)
	parent := &config.KbuildProfiles[0]
	root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	content := strings.Replace(compactKbuildLinkVmlinuxFixture(true),
		"info()\n{\n", "info()\n{\n\t: \"$MAKE\"\n", 1)
	analyzed, found, err := AnalyzeCompactKbuildLinkVmlinuxPhases(content)
	if err != nil || !found {
		t.Fatalf("analyze changed immutable source: found=%t, error=%v", found, err)
	}
	mustWriteSource(t, root, "scripts/link-vmlinux.sh", content)
	for index := range parent.SelectedSourceScriptPhases {
		phase := &parent.SelectedSourceScriptPhases[index]
		phase.SourceSHA256 = analyzed.SourceSHA256
		if phase.Ordinal == 0 {
			phase.Spans = slices.Clone(analyzed.VersionSpans)
		} else {
			phase.Spans = slices.Clone(analyzed.ObjectSpans)
		}
	}
	metadata := &CompactMetadata{
		Config: config, configFragment: map[string]string{},
		actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{}, Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
	}
	if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err == nil ||
		!strings.Contains(err.Error(), "reads recursive MAKE without a child replay") {
		t.Fatalf("phase with source-selected MAKE read: %v", err)
	}
}

func TestLinkVmlinuxSelectedPhasesActionPlan(t *testing.T) {
	config := compactKbuildLinkVmlinuxPlanFixture(t)
	plan, graph := compactKbuildLinkVmlinuxActionPlan(t, config)
	parent, init, modpost := config.KbuildProfiles[0], config.KbuildProfiles[1], config.KbuildProfiles[2]
	versionNode := compactKbuildOverwriteTestNode(t, plan, graph, parent.Name, ".version")
	initNode := compactKbuildOverwriteTestNode(t, plan, graph, init.Name, "init/built-in.a")
	objectNode := compactKbuildOverwriteTestNode(t, plan, graph, parent.Name, "vmlinux.o")
	modpostNode := compactKbuildOverwriteTestNode(t, plan, graph, modpost.Name, "vmlinux.symvers")
	finalNode := compactKbuildOverwriteTestNode(t, plan, graph, parent.Name, "vmlinux")
	if len(objectNode.Outputs) != 1 || objectNode.Outputs[0].Path != "vmlinux.o" {
		t.Fatalf("the earlier object phase claimed final script outputs: %#v", objectNode.Outputs)
	}
	for _, output := range []ActionPlanOutput{
		{Tree: "metadata", Path: "modules.builtin.modinfo"},
		{Tree: "metadata", Path: "modules.builtin"},
		{Tree: "vmlinux", Path: "System.map"},
	} {
		producer, slot, found := planProducerByOutput(plan, output.Tree, output.Path)
		if !found || producer != finalNode.ID || slot == 0 ||
			plan.Recipes[finalNode.Recipe].WorkingOutputs[planOrdinal(slot)] != output.Path {
			t.Fatalf("selected final script output %+v = (%q,%d,%t), want final %q with its working file",
				output, producer, slot, found, finalNode.ID)
		}
	}
	terminalConfig := config
	terminalConfig.imageTarget = "arch/x86/boot/bzImage"
	terminalConfig.KbuildSelections = append(slices.Clone(config.KbuildSelections), CompactKbuildSelection{
		Profile: parent.Name, Target: terminalConfig.imageTarget, MakeTarget: terminalConfig.imageTarget,
		Lifecycle: "target", Scope: "target", Stage: "target",
	})
	terminalGraph, err := newCompactKbuildSelectionGraph(terminalConfig)
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: "selected-boot-image", Stage: "target", Kind: "copy", Tool: "actionfile",
		Outputs: []ActionPlanOutput{{Tree: "image", Path: terminalConfig.imageTarget}}})
	plan.invalidateLookupIndexes()
	if err := (&CompactMetadata{
		Config: terminalConfig}).appendTerminalActionPlanNodes(plan, terminalGraph); err != nil {
		t.Fatalf("terminal projection of selected final script outputs: %v", err)
	}
	for _, product := range []string{"modules_builtin_modinfo", "modules_builtin", "system_map"} {
		if !slices.ContainsFunc(plan.Products, func(value ActionPlanProduct) bool { return value.Name == product }) {
			t.Errorf("selected final source output %q was not projected as a terminal product", product)
		}
	}
	indexes := map[string]int{}
	for index, node := range plan.Nodes {
		indexes[node.ID] = index
	}
	if !(indexes[versionNode.ID] < indexes[initNode.ID] && indexes[initNode.ID] < indexes[objectNode.ID] &&
		indexes[objectNode.ID] < indexes[modpostNode.ID] && indexes[modpostNode.ID] < indexes[finalNode.ID]) {
		t.Fatalf("source phase order: version=%d init=%d object=%d modpost=%d final=%d",
			indexes[versionNode.ID], indexes[initNode.ID], indexes[objectNode.ID], indexes[modpostNode.ID], indexes[finalNode.ID])
	}
	for _, item := range []struct {
		name, output string
		node         ActionPlanNode
		forbidden    []string
		prior        []string
	}{
		{name: "version", node: versionNode, output: ".version",
			forbidden: []string{"init/built-in.a", "vmlinux.o", "vmlinux.symvers"}},
		{name: "object", node: objectNode, output: "vmlinux.o",
			prior: []string{".version", "init/built-in.a"}, forbidden: []string{"vmlinux.symvers"}},
	} {
		recipe := plan.Recipes[item.node.Recipe]
		if _, present := recipe.Environment["MAKE"]; present {
			t.Errorf("%s source phase leaked the enclosing recursive MAKE capability", item.name)
		}
		if recipe.Tool != compactKbuildScriptRunnerRole ||
			!slices.Contains(recipe.Arguments, "-script_source_sha256") ||
			!slices.Contains(recipe.Arguments, "-script_source_span") ||
			recipe.WorkingOutputs["00000000"] != item.output {
			t.Fatalf("%s source writer recipe = %#v", item.name, recipe)
		}
		inputs := compactKbuildLinkVmlinuxStagedInputPaths(t, plan, item.node)
		for _, forbidden := range item.forbidden {
			if slices.Contains(inputs, forbidden) {
				t.Errorf("%s source writer imported future %q: %q", item.name, forbidden, inputs)
			}
		}
		for _, prior := range item.prior {
			if !slices.Contains(inputs, prior) {
				t.Errorf("%s source writer lacks previous %q: %q", item.name, prior, inputs)
			}
		}
	}
	finalRecipe := plan.Recipes[finalNode.Recipe]
	if len(finalRecipe.CommandReplays) != 1 || len(finalRecipe.CommandReplays[0].Invocations) != 1 {
		t.Fatalf("final owner replay = %#v, want only the modpost child", finalRecipe.CommandReplays)
	}
	if _, exported := finalRecipe.Environment["MAKE"]; exported {
		t.Fatal("final source exported the recursive Make capability from an earlier phase")
	}
	if want := []string{"-f", "${tree:kernel}/scripts/Makefile.modpost", "MODPOST_VMLINUX=1"}; !slices.Equal(finalRecipe.CommandReplays[0].Invocations[0].Arguments, want) {
		t.Fatalf("final child argv = %q, want %q", finalRecipe.CommandReplays[0].Invocations[0].Arguments, want)
	}
	encoded := slices.Index(finalRecipe.Arguments, "-script_content_base64")
	if encoded < 0 || encoded+1 >= len(finalRecipe.Arguments) {
		t.Fatalf("final owner lacks original Make wrapper script: %q", finalRecipe.Arguments)
	}
	wrapper, err := base64.StdEncoding.DecodeString(finalRecipe.Arguments[encoded+1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrapper), ".vmlinux.cmd") ||
		!strings.Contains(string(wrapper), ` -c '. "$`+compactKbuildFinalPhaseEnvironment+`"'`) ||
		finalRecipe.Environment[compactKbuildFinalPhaseEnvironment] == "" {
		t.Fatalf("final wrapper = %q, environment = %#v; want saved Make command and sourced final span",
			wrapper, finalRecipe.Environment)
	}
	finalSource, recognized, err := AnalyzeCompactKbuildLinkVmlinuxPhases(compactKbuildLinkVmlinuxFixture(true))
	if err != nil || !recognized || !strings.Contains(finalSource.FinalScript, ".vmlinux.d") {
		t.Fatalf("final immutable source lacks older-kernel dependency-file write: recognized=%t error=%v", recognized, err)
	}
	foundEmitter := false
	for _, node := range plan.Nodes {
		if !slices.ContainsFunc(node.Outputs, func(output ActionPlanOutput) bool {
			return strings.HasPrefix(output.Path, ".linux-bzl-source-phases/") && strings.HasSuffix(output.Path, "/final.sh")
		}) {
			continue
		}
		recipe := plan.Recipes[node.Recipe]
		store, err := plan.planningActionPlanInputSetStore()
		if err != nil {
			t.Fatal(err)
		}
		entry, bound, err := store.Lookup(finalNode.InputSet, ActionPlanInputSetTarget{
			Kind: ActionPlanInputSetWorkTarget, Path: node.Outputs[0].Path,
		})
		if err != nil {
			t.Fatal(err)
		}
		if node.Tool != compactKbuildScriptRunnerRole || !slices.Contains(recipe.Arguments, "-script_source_emit") ||
			!slices.Contains(recipe.Arguments, finalSource.SourceSHA256) ||
			indexes[node.ID] >= indexes[finalNode.ID] ||
			!bound || entry.ProducerID != node.ID || entry.Slot != 0 {
			t.Fatalf("final source emitter = %#v recipe = %#v final binding = %#v (found %t)",
				node, recipe, entry, bound)
		}
		for _, span := range finalSource.FinalSpans {
			bound := fmt.Sprintf("%d:%d", span.Start, span.End)
			if !slices.Contains(recipe.Arguments, bound) {
				t.Errorf("final source emitter lacks span %q: %q", bound, recipe.Arguments)
			}
		}
		foundEmitter = true
	}
	if !foundEmitter {
		t.Fatal("final owner has no authenticated source-span emitter")
	}
}

func TestLinkVmlinuxSelectedFinalOutputsRequireActualFinalWrites(t *testing.T) {
	const modinfo = "${OBJCOPY} -j .modinfo -O binary vmlinux.o modules.builtin.modinfo"
	const builtin = "tr '\\0' '\\n' < modules.builtin.modinfo | sed -n 's/^.*\\.file=//p' > modules.builtin"
	for _, test := range []struct {
		name, before, after, helper, want string
	}{
		{name: "missing modinfo", before: modinfo, after: "echo no module metadata", want: `no declared write to "modules.builtin.modinfo"`},
		{name: "conditional modinfo", before: modinfo, after: "if false; then\n" + modinfo + "\nfi", want: `no declared write to "modules.builtin.modinfo"`},
		{name: "missing builtin", before: builtin, after: "echo no builtin metadata", want: `no declared write to "modules.builtin"`},
		{name: "short-circuited builtin", before: builtin, after: "false && echo x > modules.builtin", want: `can skip its "modules.builtin.modinfo" writer: "false && echo x > modules.builtin"`},
		{name: "looped builtin", before: builtin, after: "for x in modules.builtin; do\n" + builtin + "\ndone", want: `can replace or skip its "modules.builtin.modinfo" writer: "for x in modules.builtin; do"`},
		{name: "removed builtin", before: builtin, after: builtin + "\nrm -f modules.builtin", want: `removes "modules.builtin" after writing it`},
		{name: "missing symbol map", before: "mksysmap vmlinux System.map", after: "echo no symbol map", want: `no declared write to "System.map"`},
		{name: "conditional symbol map", before: "mksysmap vmlinux System.map", after: "if false; then\nmksysmap vmlinux System.map\nfi", want: `no declared write to "System.map"`},
		{name: "early success before symbol map", before: "mksysmap vmlinux System.map", after: "exit 0\nmksysmap vmlinux System.map", want: `can skip its "modules.builtin.modinfo" writer: "exit 0"`},
		{name: "removed symbol map", before: "mksysmap vmlinux System.map", after: "mksysmap vmlinux System.map\nrm -f System.map", want: `removes "System.map" after writing it`},
		{name: "helper writes another file", helper: "#!/bin/sh\n$NM -n $1 > not-System.map\n", want: `no declared write to "System.map"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildLinkVmlinuxPlanFixture(t)
			parent := &config.KbuildProfiles[0]
			root := parent.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
			if test.helper != "" {
				mustWriteSource(t, root, "scripts/mksysmap", test.helper)
			} else {
				content := compactKbuildLinkVmlinuxFixture(true)
				if !strings.Contains(content, test.before) {
					t.Fatalf("source fixture lacks %q", test.before)
				}
				content = strings.Replace(content, test.before, test.after, 1)
				analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(content)
				if err != nil || !selected {
					t.Fatalf("modified source shape: selected=%t, error=%v", selected, err)
				}
				mustWriteSource(t, root, "scripts/link-vmlinux.sh", content)
				for index := range parent.SelectedSourceScriptPhases {
					phase := &parent.SelectedSourceScriptPhases[index]
					phase.SourceSHA256 = analyzed.SourceSHA256
					if phase.Ordinal == 0 {
						phase.Spans = slices.Clone(analyzed.VersionSpans)
					} else {
						phase.Spans = slices.Clone(analyzed.ObjectSpans)
					}
				}
			}
			metadata := &CompactMetadata{
				Config: config, configFragment: map[string]string{},
				actionRoles: testConfiguredScopedActionRoles}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
			if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.appendGeneratedActionPlan(plan); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("modified final source output error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLinkVmlinuxSelectedPhasesRejectChangedSourceInvocation(t *testing.T) {
	for _, test := range []struct {
		name, command, wantErr string
	}{
		{"changed script argv", "sh scripts/link-vmlinux.sh cc -z defs; printf saved > .vmlinux.cmd", "source arguments differ from the exact selected recipe"},
		{"duplicate script invocation", "sh scripts/link-vmlinux.sh ld -z defs; sh scripts/link-vmlinux.sh ld -z defs; printf saved > .vmlinux.cmd", "more than one selected invocation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildLinkVmlinuxPlanFixtureWithCommand(t, test.command)
			metadata := &CompactMetadata{
				Config: config, configFragment: map[string]string{},
				actionRoles: testConfiguredScopedActionRoles,
			}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity}}
			if _, err := metadata.appendGeneratedActionPlan(plan); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("changed source invocation error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLinkVmlinuxSelectedPhaseBindsConfiguredLinkerArgv(t *testing.T) {
	for _, test := range []struct {
		name, role, wantError string
	}{
		{name: "target linker token", role: KbuildActionRoleToken("target", "ld")},
		{name: "opposite scope linker token", role: KbuildActionRoleToken("host", "ld"), wantError: "not the configured ld proxy"},
		{name: "unconfigured linker token", role: KbuildActionRoleToken("target", "unknown_linker"), wantError: "unconfigured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation := "sh scripts/link-vmlinux.sh " + test.role + " -z defs"
			config := compactKbuildLinkVmlinuxPlanFixtureWithCommand(t,
				invocation+"; printf '%s\\n' 'cmd_vmlinux := "+invocation+"' > .vmlinux.cmd")
			for index := range config.KbuildProfiles[0].SelectedSourceScriptPhases {
				config.KbuildProfiles[0].SelectedSourceScriptPhases[index].SourceArguments[0] = test.role
			}
			metadata := &CompactMetadata{
				Config: config, configFragment: map[string]string{},
				actionRoles: testConfiguredScopedActionRoles,
			}
			plan := &ActionPlan{
				Recipes:  map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
			}
			if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
				t.Fatal(err)
			}
			graph, err := metadata.appendGeneratedActionPlan(plan)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("source linker role error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{".version", "vmlinux.o"} {
				node := compactKbuildOverwriteTestNode(t, plan, graph, config.KbuildProfiles[0].Name, phase)
				arguments := plan.Recipes[node.Recipe].Arguments
				end := slices.Index(arguments, "--")
				if end < 0 || end+1 >= len(arguments) || arguments[end+1] != "ld" ||
					!slices.Contains(arguments, "ld=${tool:ld}") || slices.Contains(arguments, test.role) {
					t.Errorf("selected source phase %q runtime linker argv = %q", phase, arguments)
				}
			}
		})
	}
}

func TestLinkVmlinuxVersionPhaseRejectsExistingObjectState(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *CompactConfig)
		want    string
	}{
		{
			name: "source root version",
			prepare: func(t *testing.T, config *CompactConfig) {
				root := config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
				if err := os.WriteFile(filepath.Join(root, ".version"), []byte("7\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: `preexisting ".version" source state`,
		},
		{
			name: "prior selected writer in full initial frontier",
			prepare: func(t *testing.T, config *CompactConfig) {
				setTestCompactKbuildInitialVisibleArtifacts(t, &config.KbuildProfiles[0], []CompactKbuildVisibleArtifact{
					{Path: ".version", Profile: "earlier-invocation", Target: ".version"},
				})
			},
			want: `prior selected ".version" artifact`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildLinkVmlinuxPlanFixture(t)
			test.prepare(t, &config)
			metadata := &CompactMetadata{
				Config: config, configFragment: map[string]string{},
				actionRoles: testConfiguredScopedActionRoles,
			}
			plan := &ActionPlan{
				Recipes:  map[string]ActionRecipe{},
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
			}
			if _, err := ensureActionPlanSource(plan, "config", "auto.conf"); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.appendGeneratedActionPlan(plan); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("source version state error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLinkVmlinuxSelectedPhasesPreserveFixdepWrapper(t *testing.T) {
	config := compactKbuildLinkVmlinuxPlanFixture(t)
	parent := config.KbuildProfiles[0]
	const invocation = "sh scripts/link-vmlinux.sh ld -z defs"
	const wrapper = invocation + "; scripts/basic/fixdep .vmlinux.d vmlinux 'cmd_vmlinux := " + invocation + "' > .vmlinux.cmd; rm -f .vmlinux.d"
	got, err := compactKbuildLinkVmlinuxFinalWrapper(parent, wrapper, parent.SelectedSourceScriptPhases[1])
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(wrapper, invocation,
		`sh -c '. "$`+compactKbuildFinalPhaseEnvironment+`"' scripts/link-vmlinux.sh ld -z defs`, 1)
	if got != want {
		t.Fatalf("source-script replacement changed fixdep and command-save wrapper:\n got: %q\nwant: %q", got, want)
	}
	phases, recognized, err := AnalyzeCompactKbuildLinkVmlinuxPhases(compactKbuildLinkVmlinuxFixture(false))
	if err != nil || !recognized || !strings.Contains(phases.FinalScript, `.vmlinux.d`) {
		t.Fatalf("5.15-shaped selected final source dependency file: recognized=%t error=%v", recognized, err)
	}
}
