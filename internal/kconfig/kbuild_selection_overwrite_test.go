package kconfig

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compactKbuildOverwriteTestConfig(t *testing.T, reverse bool) CompactConfig {
	t.Helper()
	writer := func(name, makefile, contents string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, makefile, "", contents, nil)
	}
	first := writer("a-writer", "scripts/a.mk", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`)
	before := writer("before-consumer", "scripts/before.mk", `
cmd_copy = cat $< > $@
before.out: generated/shared.out FORCE
	$(call if_changed,copy)
`)
	second := writer("b-writer", "scripts/b.mk", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`)
	after := writer("after-consumer", "scripts/after.mk", `
cmd_copy = cat $< > $@
after.out: generated/shared.out FORCE
	$(call if_changed,copy)
`)
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.out", Profile: first.Name, Target: "generated/shared.out",
	}
	secondArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.out", Profile: second.Name, Target: "generated/shared.out",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &before, []CompactKbuildVisibleArtifact{firstArtifact})
	before.InvocationPredecessors = []string{first.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{before.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &after, []CompactKbuildVisibleArtifact{secondArtifact})
	after.InvocationPredecessors = []string{second.Name}
	profiles := []CompactKbuildProfile{first, before, second, after}
	selections := []CompactKbuildSelection{
		{Profile: first.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		{
			Profile: before.Name, Target: "before.out", MakeTarget: "before.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
		},
		{
			Profile: second.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
		},
		{
			Profile: after.Name, Target: "after.out", MakeTarget: "after.out", Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
			InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{secondArtifact}),
		},
	}
	if reverse {
		slices.Reverse(profiles)
		slices.Reverse(selections)
	}
	return CompactConfig{KbuildProfiles: profiles, KbuildSelections: selections}
}

func lowerCompactKbuildOverwriteTestPlan(
	t *testing.T,
	config CompactConfig,
) (*ActionPlan, *compactKbuildSelectionGraph) {
	t.Helper()
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	for _, selection := range ordered {
		key := compactKbuildSelectionKey{
			profile: selection.Profile, target: selection.Target, stage: selection.Stage,
		}
		if graph.forwardingSelections[key] {
			continue
		}
		profile := graph.profiles[key.profile]
		context := compactKbuildSelectionPlanContext(config, selection)
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).
			withSelectionGraph(graph).
			forSelection(key, profile).
			forOutput(context.Stage, context.OutputTree, context.Product)
		producer, err := builder.build(key.target)
		if err != nil {
			t.Fatalf("lower %s: %v", compactKbuildSelectionKeyString(key), err)
		}
		if err := graph.recordMaterializedProducer(key, producer); err != nil {
			t.Fatal(err)
		}
	}
	return plan, graph
}

func compactKbuildOverwriteTestNode(
	t *testing.T,
	plan *ActionPlan,
	graph *compactKbuildSelectionGraph,
	profile, target string,
) ActionPlanNode {
	t.Helper()
	key, ok := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: profile, target: target}]
	if !ok {
		t.Fatalf("missing selection %s:%s", profile, target)
	}
	producer, ok := graph.materializedProducers[key]
	if !ok {
		t.Fatalf("missing materialized producer for %s", compactKbuildSelectionKeyString(key))
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing node %s", producer)
	}
	return node
}

func compactKbuildOverwriteTestHasProducer(node ActionPlanNode, producer string) bool {
	for _, input := range node.Inputs {
		if input.ProducerID == producer {
			return true
		}
	}
	return false
}

func TestCompactKbuildOrderedSamePathWritersUseExactImmutableVersions(t *testing.T) {
	plan, graph := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, false))
	first := compactKbuildOverwriteTestNode(t, plan, graph, "a-writer", "generated/shared.out")
	before := compactKbuildOverwriteTestNode(t, plan, graph, "before-consumer", "before.out")
	second := compactKbuildOverwriteTestNode(t, plan, graph, "b-writer", "generated/shared.out")
	after := compactKbuildOverwriteTestNode(t, plan, graph, "after-consumer", "after.out")

	firstKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile: "a-writer", target: "generated/shared.out"}]
	wantVersion := ".linux-bzl-versions/" + compactKbuildSelectionArtifactVersionID(firstKey) + "/generated/shared.out"
	if got := actionPlanOutputArtifactPath(first.Outputs[0]); got != wantVersion {
		t.Fatalf("first physical output = %q, want %q", got, wantVersion)
	}
	if !actionPlanOutputIsCanonical(second.Outputs[0]) {
		t.Fatalf("published tail is not canonical: %#v", second.Outputs[0])
	}
	canonical, _, ok := planProducerByOutput(plan, "objects", "generated/shared.out")
	if !ok || canonical != second.ID {
		t.Fatalf("canonical shared output = (%q, %t), want second writer %q", canonical, ok, second.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(second, first.ID) {
		t.Fatalf("second writer inputs = %#v, want exact first-writer edge %s", second.Inputs, first.ID)
	}
	if !slices.Contains(second.Inputs, ActionPlanNodeEdge{
		Role: compactKbuildOverwriteInputRole, ProducerID: first.ID,
	}) {
		t.Fatalf("second writer inputs = %#v, want typed overwrite-lineage edge from %s", second.Inputs, first.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(before, first.ID) || compactKbuildOverwriteTestHasProducer(before, second.ID) {
		t.Fatalf("before-consumer inputs = %#v, want only first writer %s", before.Inputs, first.ID)
	}
	if !compactKbuildOverwriteTestHasProducer(after, second.ID) || compactKbuildOverwriteTestHasProducer(after, first.ID) {
		t.Fatalf("after-consumer inputs = %#v, want only second writer %s", after.Inputs, second.ID)
	}

	physical := map[string]string{}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			key := actionPlanLookupKey(output.Tree, actionPlanOutputArtifactPath(output))
			if previous := physical[key]; previous != "" {
				t.Fatalf("physical output %q is owned by %s and %s", key, previous, node.ID)
			}
			physical[key] = node.ID
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("ordered same-path plan is invalid: %v", err)
	}
	for _, node := range []ActionPlanNode{first, second} {
		recipe := plan.Recipes[node.Recipe]
		if !actionPlanOutputIsCanonical(node.Outputs[0]) && recipe.WorkingOutputs["00000000"] != "generated/shared.out" {
			t.Fatalf("shadow writer recipe does not preserve logical output path: %#v", recipe)
		}
	}
}

func TestKbuildInvocationMaterializationUsesExactShadowTerminal(t *testing.T) {
	config := compactKbuildOverwriteTestConfig(t, false)
	plan, graph := lowerCompactKbuildOverwriteTestPlan(t, config)
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	consumer := graph.profiles["after-consumer"]
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forProfile(consumer)
	materialization, err := builder.compactKbuildInvocationDependencyMaterialization(
		"after.out",
		consumer,
		CompactKbuildInvocationDependency{Profile: "a-writer", Goals: []string{"."}},
	)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: "a-writer", target: "generated/shared.out",
	}]
	secondKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: "b-writer", target: "generated/shared.out",
	}]
	firstProducer := graph.materializedProducers[firstKey]
	secondProducer := graph.materializedProducers[secondKey]
	if len(materialization.roots) != 1 ||
		materialization.roots[0].producer != firstProducer ||
		materialization.roots[0].producer == secondProducer {
		t.Fatalf(
			"recursive materialization roots=%#v, want exact shadow writer %q and not canonical writer %q",
			materialization.roots, firstProducer, secondProducer,
		)
	}
	firstNode, ok := compactKbuildPlanNode(plan, firstProducer)
	if !ok {
		t.Fatalf("missing first writer node %q", firstProducer)
	}
	wantSlot := slices.IndexFunc(firstNode.Outputs, func(output ActionPlanOutput) bool {
		return output.Path == "generated/shared.out"
	})
	if wantSlot < 0 || materialization.roots[0].slot != wantSlot ||
		!slices.Equal(materialization.outputs, []string{"${work:root}/generated/shared.out"}) {
		t.Fatalf(
			"recursive materialization=%#v, want exact first-writer slot %d and rooted output",
			materialization, wantSlot,
		)
	}
	direct, err := builder.compactKbuildInvocationDependencyMaterialization(
		"after.out",
		consumer,
		CompactKbuildInvocationDependency{
			Profile: "a-writer", Goals: []string{"generated/shared.out"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.roots) != 1 || direct.roots[0].producer != firstProducer ||
		direct.roots[0].slot != wantSlot || direct.roots[0].producer == secondProducer ||
		!slices.Equal(direct.outputs, []string{"${work:root}/generated/shared.out"}) {
		t.Fatalf(
			"direct recursive materialization=%#v, want exact shadow writer %q slot %d and not canonical writer %q",
			direct, firstProducer, wantSlot, secondProducer,
		)
	}
}

func TestKbuildInvocationMaterializationRejectsGroupedSamePathOverwriteWithPeer(t *testing.T) {
	parent := mustCompactKbuildProfileForTest(t, "parent-link", "scripts/parent.mk", "", `
shared.out: FORCE
	touch $@
`, nil)
	child := mustCompactKbuildProfileForTest(t, "grouped-postlink", "scripts/postlink.mk", "", `
cmd_postlink = touch $@
a-peer shared.out &: FORCE
	$(call if_changed,postlink)
`, nil)
	parentArtifact := CompactKbuildVisibleArtifact{
		Path: "shared.out", Profile: parent.Name, Target: "shared.out",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &child, []CompactKbuildVisibleArtifact{parentArtifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, child},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: parent.Name, Target: "shared.out", MakeTarget: "shared.out",
				Lifecycle: "target", Scope: "target", Stage: "target",
			},
			{
				Profile: child.Name, Target: "a-peer", MakeTarget: "a-peer", GroupedTrigger: "a-peer",
				Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{parentArtifact}),
			},
			{
				Profile: child.Name, Target: "shared.out", MakeTarget: "shared.out", GroupedTrigger: "a-peer",
				Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{parentArtifact}),
			},
		},
	}
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.prepareGroupedSelections(metadata, config); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	parentKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: parent.Name, target: "shared.out",
	}]
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forSelection(parentKey, parent)
	_, err = builder.compactKbuildInvocationDependencyMaterialization(
		"shared.out",
		parent,
		CompactKbuildInvocationDependency{Profile: child.Name, Goals: []string{"shared.out"}},
	)
	if err == nil || !strings.Contains(err.Error(), "non-overwritten outputs") ||
		!strings.Contains(err.Error(), "a-peer") {
		t.Fatalf(
			"grouped overwrite error=%v, want fail-closed diagnostic for omitted peer a-peer",
			err,
		)
	}
}

func TestKbuildInvocationMaterializationFindsOverwriteOfGroupedParentPeer(t *testing.T) {
	parent := mustCompactKbuildProfileForTest(t, "grouped-parent-link", "scripts/parent.mk", "", `
cmd_link = touch a-peer shared.out
a-peer shared.out &: FORCE
	$(call if_changed,link)
`, nil)
	child := mustCompactKbuildProfileForTest(t, "single-postlink", "scripts/postlink.mk", "", `
cmd_postlink = touch $@
shared.out: FORCE
	$(call if_changed,postlink)
`, nil)
	parentArtifact := CompactKbuildVisibleArtifact{
		Path: "shared.out", Profile: parent.Name, Target: "shared.out",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &child, []CompactKbuildVisibleArtifact{parentArtifact})
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, child},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: parent.Name, Target: "a-peer", MakeTarget: "a-peer", GroupedTrigger: "a-peer",
				Lifecycle: "target", Scope: "target", Stage: "target",
			},
			{
				Profile: parent.Name, Target: "shared.out", MakeTarget: "shared.out", GroupedTrigger: "a-peer",
				Lifecycle: "target", Scope: "target", Stage: "target",
			},
			{
				Profile: child.Name, Target: "shared.out", MakeTarget: "shared.out",
				Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{parentArtifact}),
			},
		},
	}
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.prepareGroupedSelections(metadata, config); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	parentKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: parent.Name, target: "a-peer",
	}]
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forSelection(parentKey, parent)
	materialization, err := builder.compactKbuildInvocationDependencyMaterialization(
		"a-peer",
		parent,
		CompactKbuildInvocationDependency{
			Target: "a-peer", Profile: child.Name, Goals: []string{"shared.out"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(materialization.roots) != 0 ||
		!slices.Equal(materialization.outputs, []string{"${work:root}/shared.out"}) {
		t.Fatalf(
			"grouped-parent peer overwrite materialization=%#v, want only exact parent peer shared.out",
			materialization,
		)
	}
}

func TestCompactKbuildPostLinkOverwriteRetainsLinkVmlinuxSystemMapOwner(t *testing.T) {
	link := mustCompactKbuildProfileForTest(t, "link-vmlinux", "scripts/Makefile.vmlinux", "", `
cmd_link_vmlinux = $(srctree)/scripts/link-vmlinux.sh > $@; $(MAKE) -f $(srctree)/arch/x86/Makefile.postlink $@; printf '%s\n' '$(MAKE) -f $(srctree)/arch/x86/Makefile.postlink $@' > .vmlinux.cmd
vmlinux: scripts/link-vmlinux.sh FORCE
	$(call if_changed,link_vmlinux)
`, map[string]string{
		"MAKE": CompactKbuildRecursiveMakeProvenanceToken, "srctree": "__LINUX_BZL_SOURCE_TREE__",
	})
	link = compactKbuildProfileWithSourcesForTest(t, link, "scripts/link-vmlinux.sh")
	if err := SetCompactKbuildProfileInvocationLocation(&link, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	linkRoot := link.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, linkRoot, "scripts/link-vmlinux.sh", `#!/bin/sh
"$MAKE" -f "$srctree/arch/x86/Makefile.postlink" vmlinux
printf linked
`)

	postlink := mustCompactKbuildProfileForTest(t, "x86-postlink", "arch/x86/Makefile.postlink", "", `
CMD_RELOCS = arch/x86/tools/relocs
OUT_RELOCS = arch/x86/boot/compressed
cmd_relocs = true; mkdir -p $(OUT_RELOCS); $(CMD_RELOCS) $@ > $(OUT_RELOCS)/$@.relocs; $(CMD_RELOCS) --abs-relocs $@
cmd_strip_relocs = $(OBJCOPY) --remove-section=.rel.* --remove-section=.rela.* $@
vmlinux: FORCE
	@true
	$(call cmd,relocs)
	$(call cmd,strip_relocs)
`, map[string]string{"OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	if err := SetCompactKbuildProfileInvocationLocation(&postlink, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	link.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: "vmlinux", Profile: postlink.Name, Goals: []string{"vmlinux"},
		ReplayArguments: []string{
			"-f", "__LINUX_BZL_SOURCE_TREE__/arch/x86/Makefile.postlink", "vmlinux",
		},
	}}
	linkArtifact := CompactKbuildVisibleArtifact{
		Path: "vmlinux", Profile: link.Name, Target: "vmlinux",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &postlink, []CompactKbuildVisibleArtifact{linkArtifact})

	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{link, postlink},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: link.Name, Target: "vmlinux", MakeTarget: "vmlinux",
				Lifecycle: "target", Scope: "target", Stage: "target",
			},
			{
				Profile: postlink.Name, Target: "vmlinux", MakeTarget: "vmlinux",
				Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
					[]CompactKbuildVisibleArtifact{linkArtifact},
				),
			},
		},
	}
	metadata := &CompactMetadata{
		Config: config, configFragment: map[string]string{},
		actionRoles: testConfiguredScopedActionRoles,
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: "arch/x86/tools/relocs"}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}); err != nil {
		t.Fatal(err)
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	linkKey := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: link.Name, target: "vmlinux",
	}]
	if graph.forwardingSelections[linkKey] {
		t.Fatalf("source-script link writer was classified as recursive forwarding: %#v", graph.forwardingSelections)
	}
	if owner, ownerErr := graph.compactKbuildVisibleArtifactOwner(linkArtifact); ownerErr != nil || owner != linkKey {
		t.Fatalf("exact link-vmlinux artifact owner = %s, %v; want %s", compactKbuildSelectionKeyString(owner), ownerErr, compactKbuildSelectionKeyString(linkKey))
	}
	linkNode := compactKbuildOverwriteTestNode(t, plan, graph, link.Name, "vmlinux")
	postlinkNode := compactKbuildOverwriteTestNode(t, plan, graph, postlink.Name, "vmlinux")
	canonical, _, ok := planProducerByOutput(plan, "vmlinux", "vmlinux")
	if !ok || canonical != postlinkNode.ID || actionPlanOutputIsCanonical(linkNode.Outputs[0]) {
		t.Fatalf(
			"canonical vmlinux = (%q,%t), want postlink %q after shadow link writer %#v",
			canonical, ok, postlinkNode.ID, linkNode.Outputs,
		)
	}
	if !compactKbuildOverwriteTestHasProducer(postlinkNode, linkNode.ID) {
		t.Fatalf("postlink inputs = %#v, want exact link-vmlinux producer %s in its working closure", postlinkNode.Inputs, linkNode.ID)
	}
	hasLinkOverwrite := false
	for _, node := range plan.Nodes {
		hasLinkOverwrite = hasLinkOverwrite || slices.Contains(node.Inputs, ActionPlanNodeEdge{
			Role: "overwrite", ProducerID: linkNode.ID,
		})
	}
	if !hasLinkOverwrite {
		t.Fatalf("postlink plan lost the exact overwrite edge from link-vmlinux %s", linkNode.ID)
	}
	const relocations = "arch/x86/boot/compressed/vmlinux.relocs"
	var relocationNode ActionPlanNode
	var relocationOutput ActionPlanOutput
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Path == relocations {
				relocationNode = node
				relocationOutput = output
				break
			}
		}
		if relocationNode.ID != "" {
			break
		}
	}
	if relocationNode.ID == "" {
		t.Fatalf("postlink plan outputs = %#v, want exact nested output %q", plan.Nodes, relocations)
	}
	if relocationNode.ID != postlinkNode.ID {
		t.Fatalf("relocation producer = %q, want atomic postlink node %q", relocationNode.ID, postlinkNode.ID)
	}
	if !strings.HasPrefix(relocationOutput.ArtifactPath, ".linux-bzl-side-outputs/") {
		t.Fatalf("relocation output = %#v, want immutable nested side-output artifact", relocationOutput)
	}
	postlinkRecipe := plan.Recipes[relocationNode.Recipe]
	hasRelocationsWorkingOutput := false
	for _, workingOutput := range postlinkRecipe.WorkingOutputs {
		hasRelocationsWorkingOutput = hasRelocationsWorkingOutput || workingOutput == relocations
	}
	if !hasRelocationsWorkingOutput {
		t.Fatalf("postlink working outputs = %q, want %q", postlinkRecipe.WorkingOutputs, relocations)
	}
	postlinkScriptIndex := slices.Index(postlinkRecipe.Arguments, "-script_content_base64")
	if postlinkScriptIndex < 0 || postlinkScriptIndex+1 == len(postlinkRecipe.Arguments) {
		t.Fatalf("postlink recipe arguments = %q, want atomic script", postlinkRecipe.Arguments)
	}
	postlinkScript, err := base64.StdEncoding.DecodeString(postlinkRecipe.Arguments[postlinkScriptIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(postlinkScript), "> arch/x86/boot/compressed/vmlinux.relocs") ||
		strings.Contains(string(postlinkScript), "${tree:prep}/vmlinux.relocs") {
		t.Fatalf("postlink script = %q, want the same exact nested working output", postlinkScript)
	}
	if err := appendNearestSourceScriptSideOutput(plan, postlinkNode.ID, ActionPlanOutput{
		Tree: "vmlinux", Path: "System.map",
	}); err != nil {
		t.Fatal(err)
	}
	producer, slot, ok := planProducerByOutput(plan, "vmlinux", "System.map")
	indexByID := make(map[string]int, len(plan.Nodes))
	for index, node := range plan.Nodes {
		indexByID[node.ID] = index
	}
	producerIndex, producerExists := indexByID[producer]
	if !ok || !producerExists || producer == postlinkNode.ID || slot != 2 ||
		!actionPlanNodeExecutesImmutableSourceScriptPath(
			plan, indexByID, plan.Nodes[producerIndex], plan.Recipes[plan.Nodes[producerIndex].Recipe],
			"kernel", "scripts/link-vmlinux.sh",
		) {
		t.Fatalf(
			"System.map producer = (%q,%d,%t), want the source-owned link-vmlinux action before postlink %q",
			producer, slot, ok, postlinkNode.ID,
		)
	}
	linkRecipe := plan.Recipes[plan.Nodes[producerIndex].Recipe]
	encodedIndex := slices.Index(linkRecipe.Arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 == len(linkRecipe.Arguments) {
		t.Fatalf("source-owned link-vmlinux recipe arguments = %q", linkRecipe.Arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(linkRecipe.Arguments[encodedIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decoded), "__LINUX_BZL_MAKE__") ||
		!strings.Contains(string(decoded), "make -f ${tree:kernel}/arch/x86/Makefile.postlink vmlinux") ||
		!strings.Contains(string(decoded), "'make -f ${tree:kernel}/arch/x86/Makefile.postlink vmlinux'") ||
		linkRecipe.Environment["MAKE"] != "make" || len(linkRecipe.CommandReplays) != 1 {
		t.Fatalf(
			"link-vmlinux script=%q environment=%#v replays=%#v, want only the exact recursive Make proxy",
			decoded, linkRecipe.Environment, linkRecipe.CommandReplays,
		)
	}
	if len(linkRecipe.CommandReplays[0].Invocations) != 1 ||
		!slices.Equal(linkRecipe.CommandReplays[0].Invocations[0].Outputs, []string{"${work:root}/vmlinux"}) {
		t.Fatalf(
			"link-vmlinux replay=%#v, want the in-place postlink overwrite to verify vmlinux",
			linkRecipe.CommandReplays[0],
		)
	}
}

func TestCompactKbuildOrderedSamePathPlanIsStableUnderSerializedInputReversal(t *testing.T) {
	first, _ := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, false))
	second, _ := lowerCompactKbuildOverwriteTestPlan(t, compactKbuildOverwriteTestConfig(t, true))
	if err := contentAddressActionPlanNodes(first); err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(second); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(first.Nodes, func(left, right ActionPlanNode) int { return strings.Compare(left.ID, right.ID) })
	slices.SortFunc(second.Nodes, func(left, right ActionPlanNode) int { return strings.Compare(left.ID, right.ID) })
	if !reflect.DeepEqual(first.Sources, second.Sources) ||
		!reflect.DeepEqual(first.Recipes, second.Recipes) ||
		!reflect.DeepEqual(first.Nodes, second.Nodes) {
		t.Fatalf("reversing serialized profiles/selections changed the plan\nfirst=%#v\nsecond=%#v", first.Nodes, second.Nodes)
	}
}

func TestCompactKbuildSamePathWritersRequireTotalSourceProvenOrder(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_emit = touch $@
generated/shared.out: FORCE
	$(call if_changed,emit)
`, nil)
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "generated/shared.out", MakeTarget: "generated/shared.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous owners") || !strings.Contains(err.Error(), "unordered") {
		t.Fatalf("unordered same-path writers error = %v", err)
	}
}

func TestCompactKbuildOrderedSideOutputWritersResolveExactRuntimeWriter(t *testing.T) {
	producer := func(name, target string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "scripts/"+name+".mk", "", `
cmd_emit = touch $@
`+target+`: FORCE
	$(call if_changed,emit)
`, nil)
	}
	consumer := func(name, target string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "scripts/"+name+".mk", "", `
cmd_copy = cat $< > $@
`+target+`: generated/shared.side FORCE
	$(call if_changed,copy)
`, nil)
	}
	first := producer("side-a", "a.marker")
	before := consumer("side-before", "before.side.out")
	second := producer("side-b", "b.marker")
	after := consumer("side-after", "after.side.out")
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.side", Profile: first.Name, Target: "a.marker",
	}
	secondArtifact := CompactKbuildVisibleArtifact{
		Path: "generated/shared.side", Profile: second.Name, Target: "b.marker",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &before, []CompactKbuildVisibleArtifact{firstArtifact})
	before.InvocationPredecessors = []string{first.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{before.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &after, []CompactKbuildVisibleArtifact{secondArtifact})
	after.InvocationPredecessors = []string{second.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, before, second, after},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "a.marker", MakeTarget: "a.marker", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: before.Name, Target: "before.side.out", MakeTarget: "before.side.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "b.marker", MakeTarget: "b.marker", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: after.Name, Target: "after.side.out", MakeTarget: "after.side.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config, configFragment: map[string]string{}}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	firstNode := compactKbuildOverwriteTestNode(t, plan, graph, first.Name, "a.marker")
	secondNode := compactKbuildOverwriteTestNode(t, plan, graph, second.Name, "b.marker")
	beforeNode := compactKbuildOverwriteTestNode(t, plan, graph, before.Name, "before.side.out")
	afterNode := compactKbuildOverwriteTestNode(t, plan, graph, after.Name, "after.side.out")
	stateOutput := func(t *testing.T, node ActionPlanNode, logical string) (int, ActionPlanOutput) {
		t.Helper()
		for slot, candidate := range node.Outputs {
			if candidate.ObservedPath == logical {
				return slot, candidate
			}
		}
		t.Fatalf("node %s omits absolute runtime state for side output %q: %#v", node.ID, logical, node.Outputs)
		return 0, ActionPlanOutput{}
	}
	firstSlot, firstState := stateOutput(t, firstNode, "generated/shared.side")
	beforeSlot, beforeState := stateOutput(t, beforeNode, "generated/shared.side")
	secondSlot, secondState := stateOutput(t, secondNode, "generated/shared.side")
	statePaths := map[string]bool{}
	for _, state := range []ActionPlanOutput{firstState, beforeState, secondState} {
		if statePaths[state.Path] {
			t.Fatalf("distinct candidate states collide at %q", state.Path)
		}
		statePaths[state.Path] = true
	}
	for _, node := range []ActionPlanNode{firstNode, beforeNode, secondNode} {
		for _, output := range node.Outputs {
			if output.Path == "generated/shared.side" {
				t.Fatalf("candidate %s statically publishes opaque side output: %#v", node.ID, node.Outputs)
			}
		}
	}
	if producerID, slot, ok := planProducerByOutput(plan, "objects", "generated/shared.side"); ok {
		t.Fatalf("opaque side output has a guessed canonical producer (%q, %d)", producerID, slot)
	}
	for _, state := range []struct {
		name string
		node ActionPlanNode
		slot int
	}{
		{name: "first", node: firstNode, slot: firstSlot},
		{name: "before", node: beforeNode, slot: beforeSlot},
		{name: "second", node: secondNode, slot: secondSlot},
	} {
		if got := plan.Recipes[state.node.Recipe].ObservedOutputs[planOrdinal(state.slot)]; got != "generated/shared.side" {
			t.Fatalf("%s absolute state observes %q, want generated/shared.side", state.name, got)
		}
	}

	assertStateParents := func(t *testing.T, node ActionPlanNode, stateSlot int, want []ActionPlanNodeEdge) {
		t.Helper()
		got := []ActionPlanNodeEdge{}
		bindings := []string{}
		for index, input := range node.Inputs {
			if input.Role != "observed-state" {
				continue
			}
			got = append(got, input)
			binding := "observed-state:" + planOrdinal(index)
			if gotBinding := plan.Recipes[node.Recipe].Inputs[index]; gotBinding != binding {
				t.Fatalf("candidate %s state input %d binding = %q, want %q", node.ID, index, gotBinding, binding)
			}
			bindings = append(bindings, binding)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("candidate %s state parents = %#v, want nearest frontier %#v", node.ID, got, want)
		}
		if gotBases := plan.Recipes[node.Recipe].ObservedOutputBases[planOrdinal(stateSlot)]; !slices.Equal(gotBases, bindings) {
			t.Fatalf("candidate %s state base bindings = %q, want input bindings %q", node.ID, gotBases, bindings)
		}
	}
	assertStateParents(t, firstNode, firstSlot, nil)
	assertStateParents(t, beforeNode, beforeSlot, []ActionPlanNodeEdge{{
		Role: "observed-state", ProducerID: firstNode.ID, Slot: firstSlot,
	}})
	assertStateParents(t, secondNode, secondSlot, []ActionPlanNodeEdge{{
		Role: "observed-state", ProducerID: beforeNode.ID, Slot: beforeSlot,
	}})

	resolverFor := func(t *testing.T, consumer ActionPlanNode) ActionPlanNode {
		t.Helper()
		for _, input := range consumer.Inputs {
			candidate, exists := compactKbuildPlanNode(plan, input.ProducerID)
			if exists && candidate.Tool == "actionfile" && len(candidate.Outputs) == 1 && candidate.Outputs[0].Path == "generated/shared.side" {
				return candidate
			}
		}
		t.Fatalf("consumer %s has no runtime side-output resolver: %#v", consumer.ID, consumer.Inputs)
		return ActionPlanNode{}
	}
	beforeResolver := resolverFor(t, beforeNode)
	afterResolver := resolverFor(t, afterNode)
	for _, resolver := range []struct {
		name string
		node ActionPlanNode
		want ActionPlanNodeEdge
	}{
		{name: "before", node: beforeResolver, want: ActionPlanNodeEdge{Role: "state", ProducerID: firstNode.ID, Slot: firstSlot}},
		{name: "after", node: afterResolver, want: ActionPlanNodeEdge{Role: "state", ProducerID: secondNode.ID, Slot: secondSlot}},
	} {
		if got := resolver.node.Inputs; !slices.Equal(got, []ActionPlanNodeEdge{resolver.want}) {
			t.Fatalf("%s-side resolver inputs = %#v, want maximal absolute state %#v", resolver.name, got, resolver.want)
		}
		recipe := plan.Recipes[resolver.node.Recipe]
		if got, want := recipe.Inputs, []string{"state:00000000"}; !slices.Equal(got, want) {
			t.Fatalf("%s-side resolver input bindings = %q, want %q", resolver.name, got, want)
		}
		if got, want := recipe.Arguments, []string{
			"-out", "${output:00000000}", "-state", "${input:state:00000000}",
		}; !slices.Equal(got, want) {
			t.Fatalf("%s-side resolver arguments = %q, want %q", resolver.name, got, want)
		}
		if !recipe.ArgumentsFile {
			t.Fatalf("%s-side resolver does not use the arguments-file protocol: %#v", resolver.name, recipe)
		}
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("runtime-resolved same-path side-output plan is invalid: %v", err)
	}
}
