package kconfig

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGeneratedActionPlanMaterializesSelectedGroupedPatternPeersOnce(t *testing.T) {
	const (
		generatedC = "scripts/dtc/dtc-parser.tab.c"
		generatedH = "scripts/dtc/dtc-parser.tab.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/dtc", "scripts/Makefile.host", "scripts/dtc", `
cmd_bison = $(YACC) -o $(basename $@).c --defines=$(basename $@).h -t -l $<
scripts/dtc/%.tab.c scripts/dtc/%.tab.h: scripts/dtc/%.y FORCE
	$(call if_changed,bison)
`, map[string]string{"YACC": KbuildActionRoleToken("host", "bison")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "scripts/dtc/dtc-parser.y")
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("bison"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: generatedH, MakeTarget: generatedH, GroupedTrigger: generatedC, Lifecycle: "target", Scope: "host", Stage: "host"},
				{Profile: profile.Name, Target: generatedC, MakeTarget: generatedC, GroupedTrigger: generatedC, Lifecycle: "target", Scope: "host", Stage: "host"},
			},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.selectedRootRuleResolutions) != 0 {
		t.Fatalf("completed action plan retained %d selected-root rule resolutions", len(graph.selectedRootRuleResolutions))
	}

	bisonNodes := []ActionPlanNode{}
	for _, node := range plan.Nodes {
		if plan.Recipes[node.Recipe].Tool == "bison" {
			bisonNodes = append(bisonNodes, node)
		}
	}
	if len(bisonNodes) != 1 {
		t.Fatalf("bison nodes = %#v, want one grouped recipe invocation", bisonNodes)
	}
	producer := bisonNodes[0]
	wantOutputs := map[string]bool{generatedC: true, generatedH: true}
	if len(producer.Outputs) != len(wantOutputs) {
		t.Fatalf("grouped bison outputs = %#v, want %#v", producer.Outputs, wantOutputs)
	}
	for _, output := range producer.Outputs {
		if output.Tree != "host" || output.ArtifactPath != "" || !wantOutputs[output.Path] {
			t.Fatalf("grouped bison output = %#v, want canonical host output in %#v", output, wantOutputs)
		}
	}
	for _, target := range []string{generatedC, generatedH} {
		key := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "host"}
		if got := graph.materializedProducers[key]; got != producer.ID {
			t.Fatalf("selection %s producer = %q, want shared producer %q", compactKbuildSelectionKeyString(key), got, producer.ID)
		}
		if got, _, ok := planProducerByOutput(plan, "host", target); !ok || got != producer.ID {
			t.Fatalf("canonical host output %q producer = %q, %t; want %q", target, got, ok, producer.ID)
		}
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("grouped two-peer action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanMaterializesSelectedParentTraversalMakeTarget(t *testing.T) {
	const (
		directory  = "arch/x86/kvm"
		target     = "virt/kvm/kvm_main.o"
		makeTarget = directory + "/../../../virt/kvm/kvm_main.o"
		source     = "virt/kvm/kvm_main.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
stem_cflags = $(CFLAGS_$(target-stem).o)
CFLAGS_../../../virt/kvm/kvm_main.o = -DLEXICAL_TARGET_STEM
cmd_cc_o_c = $(CC) $(object_cflags) $(stem_cflags) -DRELATIVE_OBJECT=$(patsubst $(obj)/%,%,$@) -c -o $@ $<
$(obj)/%.o: private object_cflags = -DLEXICAL_KVM_TARGET
virt/kvm/kvm_main.o: FORCE
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":  KbuildActionRoleToken("target", "cc"),
		"obj": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: makeTarget,
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if orderingOnly, err := metadata.compactKbuildTargetIsOrderingOnlyInProfile(profile, target); err != nil {
		t.Fatal(err)
	} else if !orderingOnly {
		t.Fatalf("canonical target %q did not reproduce the prerequisite-only classification", target)
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.selectedRootRuleResolutions) != 0 || len(graph.ruleResolutions) != 0 {
		t.Fatalf(
			"completed parent-traversal plan retained selected/general rule resolutions %d/%d",
			len(graph.selectedRootRuleResolutions), len(graph.ruleResolutions),
		)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("generated plan omits canonical selected target %q: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("generated plan target producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	for _, argument := range []string{
		"-DLEXICAL_KVM_TARGET",
		"-DLEXICAL_TARGET_STEM",
		"-DRELATIVE_OBJECT=../../../virt/kvm/kvm_main.o",
	} {
		if !slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("generated selected-target arguments omit %q: %q", argument, recipe.Arguments)
		}
	}
	for _, candidate := range plan.Nodes {
		for _, output := range candidate.Outputs {
			if strings.Contains(output.Path, "..") || strings.Contains(output.ArtifactPath, "..") {
				t.Fatalf("generated plan leaked lexical traversal into artifact identity %#v", output)
			}
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("generated parent-traversal selection plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanMaterializesObjectRootedSelectionOutsideInvocationCwd(t *testing.T) {
	const (
		target = "tools/objtool/libsubcmd/exec-cmd.o"
		source = "tools/lib/subcmd/exec-cmd.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", "libsubcmd", `
cmd_cc_o_c = $(CC) -c -o $@ $<
$(OUTPUT)%.o: %.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":     KbuildActionRoleToken("host", "cc"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "host", Stage: "host",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": actionPlanTestProbeIdentity,
			"host":   actionPlanTestProbeIdentity,
		},
		Recipes: map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "host", target)
	if !ok {
		t.Fatalf("generated plan omits object-rooted selected target %q: %#v", target, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("generated plan target producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if got, want := recipe.Arguments, []string{"-c", "-o", "${output:00000000}", "${source:object:00000000}"}; !slices.Equal(got, want) {
		t.Fatalf("object-rooted selection arguments = %q, want %q", got, want)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("object-rooted selection working output = %q, want %q", got, target)
	}
	for _, directory := range recipe.WorkingDirectories {
		if strings.Contains(directory, "tools/lib/subcmd/tools/objtool") {
			t.Fatalf("object-rooted selection was rescoped below source cwd: %q", recipe.WorkingDirectories)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("object-rooted selection plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanLetsSelectedPrepWriterSupersedeConfigSeed(t *testing.T) {
	const (
		baselineInput = "auto.conf"
		configOutput  = "include/config/kernel.release"
		consumer      = "include/generated/release-consumer"
	)
	profile := mustCompactKbuildProfileForTest(t, "prep:config-refresh", "Makefile", "", `
cmd_release = $(AWK) $(objtree)/include/config/auto.conf -o $@
cmd_consume = $(AWK) $(objtree)/include/config/kernel.release -o $@
include/config/kernel.release: FORCE
	$(call if_changed,release)
include/generated/release-consumer: include/config/kernel.release FORCE
	$(call if_changed,consume)
`, map[string]string{
		"AWK":     KbuildActionRoleToken("target", "awk"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("awk"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: configOutput, MakeTarget: configOutput, Lifecycle: "prep", Scope: "target", Stage: "prep"},
				{Profile: profile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}

	configSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "config" && source.Path == baselineInput {
			configSourceID = source.ID
			break
		}
	}
	if configSourceID == "" {
		t.Fatalf("plan sources omit config/%s: %#v", baselineInput, plan.Sources)
	}
	writerID, _, ok := planProducerByOutput(plan, "prep", configOutput)
	if !ok {
		t.Fatalf("plan omits selected config writer for prep/%s", configOutput)
	}
	writer, ok := compactKbuildPlanNode(plan, writerID)
	if !ok || plan.Recipes[writer.Recipe].Tool != "awk" {
		t.Fatalf("config writer = %#v, want selected awk action", writer)
	}
	copyCount := 0
	for _, projection := range resolvedConfigProjections() {
		producerID, _, found := planProducerByOutput(plan, "prep", projection.output)
		if !found {
			t.Errorf("plan omits final config projection prep/%s", projection.output)
			continue
		}
		producer, _ := compactKbuildPlanNode(plan, producerID)
		if plan.Recipes[producer.Recipe].Tool == "actionfile" {
			copyCount++
		}
	}
	if got, want := copyCount, len(resolvedConfigProjections())-1; got != want {
		t.Fatalf("fallback config copies = %d, want %d", got, want)
	}
	consumerID, _, ok := planProducerByOutput(plan, "prep", consumer)
	if !ok {
		t.Fatalf("plan omits config consumer prep/%s", consumer)
	}
	consumerNode, _ := compactKbuildPlanNode(plan, consumerID)
	if !slices.ContainsFunc(consumerNode.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == writerID
	}) {
		t.Fatalf("config consumer inputs = %#v, want final writer %s", consumerNode.Inputs, writerID)
	}

	seedModuleSDKPlanOutputForTest(t, plan, "target", "metadata", "Module.symvers", "module_symvers")
	plan.Products = append(plan.Products, ActionPlanProduct{
		Name: "module_symvers", Tree: "metadata", Path: "Module.symvers",
	})
	seedModuleSDKVmlinuxForTest(t, plan)
	if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sdkProjection := moduleSDKProjectionNodeForTest(t, plan, configOutput)
	if len(sdkProjection.Inputs) != 1 || sdkProjection.Inputs[0].ProducerID != writerID {
		t.Fatalf("SDK config projection inputs = %#v, want selected writer %s", sdkProjection.Inputs, writerID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("selected config-writer plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanBindsConfigProjectionAsNativePrerequisite(t *testing.T) {
	const (
		target       = "scripts/target.json"
		prerequisite = "include/config/auto.conf"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts", "scripts/Makefile", "scripts", `
cmd_target_json = cp $< $@
scripts/target.json: include/config/auto.conf FORCE
	$(call if_changed,target_json)
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "prep", Scope: "target", Stage: "prep",
			}},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	producerID, _, ok := planProducerByOutput(plan, "prep", target)
	if !ok {
		t.Fatalf("plan omits prep/%s: %#v", target, plan.Nodes)
	}
	producer, ok := compactKbuildPlanNode(plan, producerID)
	if !ok {
		t.Fatalf("missing target producer %q", producerID)
	}
	recipe := plan.Recipes[producer.Recipe]
	configSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "config" && source.Path == "auto.conf" {
			configSourceID = source.ID
			break
		}
	}
	if configSourceID == "" || !slices.ContainsFunc(producer.Sources, func(edge ActionPlanSourceEdge) bool {
		return edge.SourceID == configSourceID
	}) {
		t.Fatalf("target sources = %#v, want config/auto.conf source %q", producer.Sources, configSourceID)
	}
	if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), prerequisite) {
		t.Fatalf("target working inputs = %#v, want logical config prerequisite %q", recipe.WorkingInputs, prerequisite)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("native config-prerequisite plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanUsesReachedGroupedTriggerAndPeerDependencyUnion(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:grouped-trigger", "scripts/Makefile.build", "", `
EMIT = /selected/emitter
cmd_dependency = $(EMIT) -o $@ $<
peer-only.generated: peer-only.in FORCE
	$(call if_changed,dependency)
cmd_group = $(EMIT) --target $@ --first $< --all $^ -o $@
a-peer z-trigger &: common.in FORCE
	$(call if_changed,group)
a-peer: peer-only.generated peer-only.source
`, map[string]string{"EMIT": KbuildActionRoleToken("target", "emit")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "common.in", "peer-only.in", "peer-only.source")
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "peer-only.generated", MakeTarget: "peer-only.generated", Lifecycle: "target", Scope: "target", Stage: "target"},
			// z-trigger is deliberately lexically later than a-peer. The parser's
			// first-reached identity, not target sorting, owns GNU automatic vars.
			{Profile: profile.Name, Target: "a-peer", MakeTarget: "a-peer", GroupedTrigger: "z-trigger", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "z-trigger", MakeTarget: "z-trigger", GroupedTrigger: "z-trigger", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testScopedActionRoles("emit"),
		Config:         config,
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}

	dependencyID, _, ok := planProducerByOutput(plan, "objects", "peer-only.generated")
	if !ok {
		t.Fatalf("plan omits peer-only generated dependency: %#v", plan.Nodes)
	}
	triggerKey := compactKbuildSelectionKey{profile: profile.Name, target: "z-trigger", stage: "target"}
	peerKey := compactKbuildSelectionKey{profile: profile.Name, target: "a-peer", stage: "target"}
	groupID := graph.materializedProducers[triggerKey]
	if groupID == "" || graph.materializedProducers[peerKey] != groupID {
		t.Fatalf("grouped producers = trigger %q, peer %q", groupID, graph.materializedProducers[peerKey])
	}
	groupNode, ok := compactKbuildPlanNode(plan, groupID)
	if !ok {
		t.Fatalf("missing grouped trigger node %q", groupID)
	}
	dependencyEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, groupNode, "peer-only.generated")
	wantDependencyEntry := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "peer-only.generated"},
		ProducerID: dependencyID,
	}
	if !found || dependencyEntry != wantDependencyEntry {
		t.Fatalf("grouped trigger persistent dependency = %#v, %t; want %#v", dependencyEntry, found, wantDependencyEntry)
	}
	groupRecipe := plan.Recipes[groupNode.Recipe]
	arguments := strings.Join(groupRecipe.Arguments, " ")
	if !strings.Contains(arguments, "--target z-trigger") {
		t.Fatalf("grouped trigger arguments = %q, want reached target z-trigger", arguments)
	}
	if strings.Contains(arguments, "peer-only.generated") {
		t.Fatalf("grouped trigger automatic prerequisite arguments include peer-only dependency: %q", arguments)
	}
	if strings.Contains(arguments, "peer-only.source") {
		t.Fatalf("grouped trigger automatic prerequisite arguments include peer-only source: %q", arguments)
	}
	peerSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "kernel" && source.Path == "peer-only.source" {
			peerSourceID = source.ID
			break
		}
	}
	if peerSourceID == "" {
		t.Fatalf("plan sources omit kernel/peer-only.source: %#v", plan.Sources)
	}
	peerSourceEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, groupNode, "peer-only.source")
	wantPeerSourceEntry := ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "peer-only.source"},
		SourceID: peerSourceID,
	}
	if !found || peerSourceEntry != wantPeerSourceEntry {
		t.Fatalf("grouped trigger persistent source = %#v, %t; want %#v", peerSourceEntry, found, wantPeerSourceEntry)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatalf("finalize grouped-trigger action plan: %v", err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("grouped-trigger action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanLowersTargetBootstrapDependencyIntoHostClosure(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:cross-scope", "scripts/Makefile.build", "", `
cmd_target_input = $(CC) -c -o $@ $<
cmd_header = cp $< $@
cmd_host_generator = $(HOSTCC) -o $@ $^
target-input.o: target-input.c FORCE
	$(call if_changed,target_input)
generated/header.h: target-input.o FORCE
	$(call if_changed,header)
host-generator: host-generator.c generated/header.h FORCE
	$(call if_changed,host_generator)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "host-generator.c", "target-input.c")
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{
				{Profile: profile.Name, Target: "target-input.o", MakeTarget: "target-input.o", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
				{Profile: profile.Name, Target: "generated/header.h", MakeTarget: "generated/header.h", Lifecycle: "target", Scope: "host", Stage: "host"},
				{Profile: profile.Name, Target: "host-generator", MakeTarget: "host-generator", Lifecycle: "target", Scope: "host", Stage: "host"},
			},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	bootstrapID, _, ok := planProducerByOutput(plan, "bootstrap", "target-input.o")
	if !ok {
		t.Fatalf("plan omits target bootstrap producer: %#v", plan.Nodes)
	}
	projectedID, _, ok := planProducerByOutput(plan, "objects", "target-input.o")
	if !ok || projectedID == bootstrapID {
		t.Fatalf("plan omits canonical target projection for bootstrap output: %#v", plan.Nodes)
	}
	projected, _ := compactKbuildPlanNode(plan, projectedID)
	if projected.Stage != "target" || projected.Product != "vmlinux" || !slices.ContainsFunc(projected.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == bootstrapID
	}) {
		t.Fatalf("bootstrap target projection = %#v, want target/vmlinux copy from %s", projected, bootstrapID)
	}
	headerID, _, ok := planProducerByOutput(plan, "host", "generated/header.h")
	if !ok {
		t.Fatalf("plan omits host-propagated neutral producer: %#v", plan.Nodes)
	}
	hostID, _, ok := planProducerByOutput(plan, "host", "host-generator")
	if !ok {
		t.Fatalf("plan omits host generator: %#v", plan.Nodes)
	}
	header, _ := compactKbuildPlanNode(plan, headerID)
	if !slices.ContainsFunc(header.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == bootstrapID }) {
		t.Fatalf("host header inputs = %#v, want bootstrap producer %s", header.Inputs, bootstrapID)
	}
	if slices.ContainsFunc(header.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == projectedID }) {
		t.Fatalf("host header inputs = %#v, must not depend backward through target projection %s", header.Inputs, projectedID)
	}
	host, _ := compactKbuildPlanNode(plan, hostID)
	if !slices.ContainsFunc(host.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == headerID }) {
		t.Fatalf("host generator inputs = %#v, want header producer %s", host.Inputs, headerID)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("cross-stage action plan is invalid: %v", err)
	}
}

func TestAppendReferencedPlanTreesIncludesRecursiveReplayBindings(t *testing.T) {
	plan := &ActionPlan{}
	node := ActionPlanNode{}
	recipe := ActionRecipe{CommandReplays: []ActionRecipeCommandReplay{{
		Name: "make",
		Invocations: []ActionRecipeCommandReplayInvocation{{
			Arguments: []string{"-f", "${tree:kernel}/scripts/Makefile.build"},
		}},
	}}}
	if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
		t.Fatal(err)
	}
	if got, want := node.Trees, []string{"kernel"}; !slices.Equal(got, want) {
		t.Fatalf("replay tree bindings = %q, want %q", got, want)
	}
	if !slices.Equal(recipe.Trees, node.Trees) {
		t.Fatalf("recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
	}
}

func TestAppendReferencedPlanTreesClosesWorkingTreesWithoutPlaceholders(t *testing.T) {
	plan := &ActionPlan{}
	node := ActionPlanNode{}
	recipe := ActionRecipe{WorkingTrees: []string{"vendor-overlay"}}
	if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
		t.Fatal(err)
	}
	if got, want := node.Trees, []string{"vendor-overlay"}; !slices.Equal(got, want) {
		t.Fatalf("working tree closure = %q, want %q", got, want)
	}
	if !slices.Equal(recipe.Trees, node.Trees) {
		t.Fatalf("recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
	}
}

func TestAppendReferencedPlanTreesClosesOnlyCommandMetadataSourceNamespaces(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{
		"compile-recipe": {WorkingTrees: []string{"external"}},
	}}
	sourceID, err := ensureActionPlanSource(plan, "external", ".linux-bzl/external/demo/demo.c")
	if err != nil {
		t.Fatal(err)
	}
	vendorSourceID, err := ensureActionPlanSource(plan, "vendor", "drivers/vendor/immutable.c")
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes = []ActionPlanNode{
		{
			ID:      "compile",
			Recipe:  "compile-recipe",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: sourceID}},
			Trees:   []string{"external", "prep"},
			Outputs: []ActionPlanOutput{{
				Tree: "metadata", Path: ".captures/demo.state",
				ObservedPath: ".linux-bzl/external/demo/.demo.o.cmd",
			}},
		},
		{
			ID: "resolver",
			Inputs: []ActionPlanNodeEdge{{
				Role: "state", ProducerID: "compile", Slot: 0,
			}},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/.demo.o.cmd",
			}},
		},
		{
			ID:      "immutable-command-metadata",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: sourceID}},
			Trees:   []string{"external"},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/.immutable.o.cmd",
			}},
		},
		{
			ID:      "vendor-command-metadata",
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: vendorSourceID}},
			Trees:   []string{"vendor"},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: "drivers/vendor/.immutable.o.cmd",
			}},
		},
		{
			ID: "ordinary",
			Inputs: []ActionPlanNodeEdge{{
				Role: "state", ProducerID: "compile", Slot: 0,
			}},
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: ".linux-bzl/external/demo/demo.o",
			}},
		},
	}
	for _, test := range []struct {
		name     string
		producer string
		want     []string
		working  []string
	}{
		{name: "relative generated command metadata", producer: "resolver", want: []string{"external"}, working: []string{"external"}},
		{name: "immutable command metadata", producer: "immutable-command-metadata", want: []string{"external"}},
		{name: "ordinary generated output", producer: "ordinary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := ActionPlanNode{Inputs: []ActionPlanNodeEdge{{
				Role: "prerequisite", ProducerID: test.producer, Slot: 0,
			}}}
			recipe := ActionRecipe{}
			if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(node.Trees, test.want) || !slices.Equal(recipe.Trees, test.want) {
				t.Fatalf("source metadata closure = node %q recipe %q, want %q", node.Trees, recipe.Trees, test.want)
			}
			if !slices.Equal(recipe.WorkingTrees, test.working) {
				t.Fatalf("source metadata working trees = %q, want %q", recipe.WorkingTrees, test.working)
			}
		})
	}

	t.Run("multiple command metadata roots union namespaces", func(t *testing.T) {
		node := ActionPlanNode{Inputs: []ActionPlanNodeEdge{
			{Role: "prerequisite", ProducerID: "resolver", Slot: 0},
			{Role: "prerequisite", ProducerID: "vendor-command-metadata", Slot: 0},
			{Role: "duplicate", ProducerID: "resolver", Slot: 0},
		}}
		recipe := ActionRecipe{}
		if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
			t.Fatal(err)
		}
		if got, want := node.Trees, []string{"external", "vendor"}; !slices.Equal(got, want) {
			t.Fatalf("multiple metadata roots node trees = %q, want %q", got, want)
		}
		if !slices.Equal(recipe.Trees, node.Trees) {
			t.Fatalf("multiple metadata roots recipe trees = %q, want node trees %q", recipe.Trees, node.Trees)
		}
		if got, want := recipe.WorkingTrees, []string{"external"}; !slices.Equal(got, want) {
			t.Fatalf("multiple metadata roots working trees = %q, want %q", got, want)
		}
	})
}

func TestAppendReferencedPlanTreesClosesKernelSourceRelativeIncludes(t *testing.T) {
	plan := &ActionPlan{}
	kernelID, err := ensureActionPlanSource(plan, "kernel", "arch/x86/boot/mkcpustr.c")
	if err != nil {
		t.Fatal(err)
	}
	configID, err := ensureActionPlanSource(plan, "config", "autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		id    string
		trees []string
	}{
		{name: "kernel source", id: kernelID, trees: []string{"kernel"}},
		{name: "source-backed config projection", id: configID},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := ActionPlanNode{Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: test.id}}}
			recipe := ActionRecipe{}
			if err := appendReferencedPlanTrees(plan, &node, &recipe); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(node.Trees, test.trees) || !slices.Equal(recipe.Trees, test.trees) {
				t.Fatalf("source closure trees = node %q recipe %q, want %q", node.Trees, recipe.Trees, test.trees)
			}
		})
	}
}

func TestActionPlanSourceNamespaceUsesSelectedExternalRoot(t *testing.T) {
	metadata := &CompactMetadata{sourceNamespaces: map[string]string{
		"external/rust-src":                                  "toolchain",
		"external/rust-src/library":                          "rust",
		"__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/demo": "external",
	}}
	const source = "external/rust-src/library/core/src/lib.rs"
	if got, err := metadata.actionPlanSourceNamespace(source); err != nil || got != "rust" {
		t.Fatalf("actionPlanSourceNamespace(%q) = %q, %v; want rust", source, got, err)
	}
	plan := &ActionPlan{}
	id, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0] != (ActionPlanSource{ID: id, Namespace: "rust", Path: source}) {
		t.Fatalf("plan sources = %#v", plan.Sources)
	}
	if got, err := metadata.actionPlanSourceNamespace("rust/kernel/lib.rs"); err != nil || got != "kernel" {
		t.Fatalf("kernel namespace = %q, %v", got, err)
	}
	const externalSource = ".linux-bzl/external/demo/source.c"
	if got, err := metadata.actionPlanSourceNamespace(externalSource); err != nil || got != "external" {
		t.Fatalf("external namespace for %q = %q, %v", externalSource, got, err)
	}
}

func TestActionPlanSourceNamespaceRejectsInvalidSelectedRoot(t *testing.T) {
	for _, prefix := range []string{
		"../rust",
		"/external/rust",
		"external/../rust",
		"external//rust",
		" external/rust",
		"external/rust ",
		"__LINUX_BZL_SOURCE_TREE__",
		"__LINUX_BZL_SOURCE_TREE__/external/../rust",
		"__LINUX_BZL_OBJECT_TREE__/include/generated",
		"__LINUX_BZL_HOST_DEPS__/tools",
		"__LINUX_BZL_SOURCE_TREE__/__LINUX_BZL_OBJECT_TREE__/nested",
	} {
		t.Run(strings.ReplaceAll(prefix, "/", "_"), func(t *testing.T) {
			metadata := &CompactMetadata{sourceNamespaces: map[string]string{prefix: "rust"}}
			if _, err := metadata.actionPlanSourceNamespace("external/rust/core.rs"); err == nil {
				t.Fatalf("actionPlanSourceNamespace() accepted invalid selected root %q", prefix)
			}
		})
	}
}

func TestActionPlanSourceNamespaceRejectsAmbiguousNormalizedRoots(t *testing.T) {
	metadata := &CompactMetadata{sourceNamespaces: map[string]string{
		".linux-bzl/external/demo":                           "first",
		"__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/demo": "second",
	}}
	if _, err := metadata.actionPlanSourceNamespace(".linux-bzl/external/demo/source.c"); err == nil || !strings.Contains(err.Error(), "ambiguous namespaces") {
		t.Fatalf("actionPlanSourceNamespace() ambiguity error = %v", err)
	}
}

func TestActionPlanExactSourceNamespaceDoesNotOwnDescendants(t *testing.T) {
	metadata := &CompactMetadata{
		sourceNamespaces:      map[string]string{"foo/bar": "external"},
		exactSourceNamespaces: map[string]string{"foo": "prep", "foo/bar": "prep"},
	}
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "foo", want: "prep"},
		{path: "foo/bar", want: "prep"},
		{path: "foo/bar/source.c", want: "external"},
		{path: "foo/generated.h", want: "kernel"},
	} {
		if got, err := metadata.actionPlanSourceNamespace(test.path); err != nil || got != test.want {
			t.Fatalf("actionPlanSourceNamespace(%q) = %q, %v; want %q", test.path, got, err, test.want)
		}
	}
}

func TestActionPlanExactSourceNamespaceRejectsTreeMarkerSyntax(t *testing.T) {
	metadata := &CompactMetadata{exactSourceNamespaces: map[string]string{
		"__LINUX_BZL_SOURCE_TREE__/include/generated/sdk.h": "prep",
	}}
	if _, err := metadata.actionPlanSourceNamespace("include/generated/sdk.h"); err == nil {
		t.Fatal("exact source namespace accepted reserved source-tree marker syntax")
	}
}

func TestGeneratedActionPlanDoesNotMaterializePatternDeclarations(t *testing.T) {
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{{
				Name:         "prep:arch/arm64/kernel/%.lds.S",
				Directory:    "arch/arm64/kernel",
				EntryTargets: []string{"arch/arm64/kernel/%.lds.S"},
			}},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if strings.Contains(output.Path, "%") {
				t.Fatalf("pattern declaration became an action output: %#v", node)
			}
		}
	}
}

func TestGeneratedActionPlanDoesNotInventPhonyArtifacts(t *testing.T) {
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{{
				Name:         "prep:remove-stale-files",
				EntryTargets: []string{"remove-stale-files"},
				Rules: []KbuildRule{
					{Targets: []string{".PHONY"}, Prerequisites: []string{"remove-stale-files"}},
					{Targets: []string{"remove-stale-files"}, Recipe: []string{"scripts/remove-stale-files"}},
				},
			}},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Path == "remove-stale-files" {
				t.Fatalf("phony control-flow node became an action output: %#v", node)
			}
		}
	}
}

func TestGeneratedActionPlanBuildsPrerequisiteWithItsSelectedOwningProfile(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "input.c", "int selected_owner;\n")
	parent := mustCompactKbuildProfileForTest(t, "a-parent-profile", "scripts/Makefile.parent", "", `
cmd_copy = cat $< > $@
cmd_poison = $(CC) -DPOISON_PROFILE -c -o $@ $<
a-parent.out: z-dependency.out FORCE
	$(call if_changed,copy)
z-dependency.out: input.c FORCE
	$(call if_changed,poison)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	owner := mustCompactKbuildProfileForTest(t, "z-owner-profile", "scripts/Makefile.owner", "", `
cmd_owner = $(CC) -DOWNING_PROFILE -c -o $@ $<
z-dependency.out: input.c FORCE
	$(call if_changed,owner)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	for _, profile := range []*CompactKbuildProfile{&parent, &owner} {
		if profile.evaluator == nil || profile.evaluator.template == nil {
			t.Fatalf("profile %q has no captured evaluator", profile.Name)
		}
		if profile.evaluator.template.sourceRoots == nil {
			profile.evaluator.template.sourceRoots = map[string]string{}
		}
		profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = root
	}
	configFragment := map[string]string{}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, owner},
		KbuildSelections: []CompactKbuildSelection{
			// This is intentionally lexical rather than dependency order. The old
			// target sort built a-parent.out first and selected the poison rule
			// from the parent's invocation for z-dependency.out.
			{Profile: parent.Name, Target: "a-parent.out", MakeTarget: "a-parent.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: owner.Name, Target: "z-dependency.out", MakeTarget: "z-dependency.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testConfiguredScopedActionRoles,
		Config:         config,
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}

	var dependencyNode, parentNode *ActionPlanNode
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		for _, output := range node.Outputs {
			switch output.Path {
			case "z-dependency.out":
				dependencyNode = node
			case "a-parent.out":
				parentNode = node
			}
		}
	}
	if dependencyNode == nil || parentNode == nil {
		t.Fatalf("selected producers missing: dependency=%#v parent=%#v", dependencyNode, parentNode)
	}
	dependencyArguments := strings.Join(plan.Recipes[dependencyNode.Recipe].Arguments, " ")
	if !strings.Contains(dependencyArguments, "-DOWNING_PROFILE") || strings.Contains(dependencyArguments, "POISON_PROFILE") {
		t.Fatalf("dependency arguments = %q, want exact owning profile", dependencyArguments)
	}
	for _, recipe := range plan.Recipes {
		if strings.Contains(strings.Join(recipe.Arguments, " "), "POISON_PROFILE") {
			t.Fatalf("parent invocation manufactured selected prerequisite: %#v", recipe)
		}
	}
	dependsOnOwner := false
	for _, input := range parentNode.Inputs {
		if input.ProducerID == dependencyNode.ID {
			dependsOnOwner = true
			break
		}
	}
	if !dependsOnOwner {
		t.Fatalf("parent inputs = %#v, want selected owner %s", parentNode.Inputs, dependencyNode.ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("selected-owner action plan is invalid: %v", err)
	}
}

func TestGeneratedActionPlanBindsExactGeneratedArtifactAcrossUnrelatedSamePathProfiles(t *testing.T) {
	consumer := compactKbuildScriptProfileForTest(t,
		`$(CONFIG_SHELL) $(srctree)/scripts/transform.sh > $@`,
	)
	firstWriter := mustCompactKbuildProfileForTest(t, "m-first-writer", "scripts/first.mk", "", `
CC = `+KbuildActionRoleToken("target", "cc")+`
cmd_emit = $(CC) -DFIRST_WRITER -c -o $@ $<
generated/shared.h: first.c FORCE
	$(call if_changed,emit)
`, nil)
	firstWriter = compactKbuildProfileWithSourcesForTest(t, firstWriter, "first.c")
	exactWriter := mustCompactKbuildProfileForTest(t, "z-exact-writer", "scripts/exact.mk", "", `
CC = `+KbuildActionRoleToken("target", "cc")+`
cmd_emit = $(CC) -DEXACT_WRITER -c -o $@ $<
generated/shared.h: exact.c FORCE
	$(call if_changed,emit)
`, nil)
	exactWriter = compactKbuildProfileWithSourcesForTest(t, exactWriter, "exact.c")
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.h", Profile: exactWriter.Name, Target: "generated/shared.h",
	}
	configFragment := map[string]string{}
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("cc"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{consumer, firstWriter, exactWriter},
			KbuildSelections: []CompactKbuildSelection{
				{
					Profile: consumer.Name, Target: "generated/result.h", MakeTarget: "generated/result.h", Lifecycle: "target", Scope: "target", Stage: "target",
					GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{artifact}),
				},
				{Profile: firstWriter.Name, Target: artifact.Path, MakeTarget: artifact.Path, Lifecycle: "prep", Scope: "target", Stage: "prep"},
				{Profile: exactWriter.Name, Target: artifact.Path, MakeTarget: artifact.Path, Lifecycle: "target", Scope: "target", Stage: "target"},
			},
		},
		configFragment: configFragment,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}
	firstID, _, firstFound := planProducerByOutput(plan, "prep", artifact.Path)
	exactID, _, exactFound := planProducerByOutput(plan, "objects", artifact.Path)
	consumerID, _, consumerFound := planProducerByOutput(plan, "objects", "generated/result.h")
	if !firstFound || !exactFound || !consumerFound {
		t.Fatalf("same-path plan producers = first (%q, %t), exact (%q, %t), consumer (%q, %t): %#v", firstID, firstFound, exactID, exactFound, consumerID, consumerFound, plan.Nodes)
	}
	if firstID == exactID {
		t.Fatalf("unrelated same-path selections share producer %q, want distinct physical-stage actions", firstID)
	}
	consumerNode, ok := compactKbuildPlanNode(plan, consumerID)
	if !ok {
		t.Fatalf("consumer producer %q is missing", consumerID)
	}
	artifactEntry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, consumerNode, artifact.Path)
	wantArtifactEntry := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: artifact.Path},
		ProducerID: exactID,
	}
	if !found || artifactEntry != wantArtifactEntry {
		t.Fatalf("consumer persistent generated artifact = %#v, %t; want %#v", artifactEntry, found, wantArtifactEntry)
	}
	consumerEntries := actionPlanNodeInputSetEntriesForTest(t, plan, consumerNode)
	for _, entry := range consumerEntries {
		if entry.ProducerID == firstID {
			t.Fatalf("consumer persistent inputs = %#v, unrelated same-path producer %q leaked into exact closure", consumerEntries, firstID)
		}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatalf("finalize exact generated-artifact action plan: %v", err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("exact generated-artifact action plan is invalid: %v", err)
	}
}
