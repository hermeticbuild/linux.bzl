package kconfig

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func appendSelectionScopeTestOutput(t *testing.T, plan *ActionPlan, stage, tree, output string) string {
	return appendSelectionScopeTestVersionedOutput(t, plan, stage, tree, output, "")
}

func appendSelectionScopeTestVersionedOutput(t *testing.T, plan *ActionPlan, stage, tree, output, artifactPath string) string {
	return appendSelectionScopeTestLiteralOutput(t, plan, stage, tree, output, artifactPath, "fixture")
}

func appendSelectionScopeTestLiteralOutput(t *testing.T, plan *ActionPlan, stage, tree, output, artifactPath, literal string) string {
	t.Helper()
	node := ActionPlanNode{
		Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: tree, Path: output, ArtifactPath: artifactPath}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-literal", literal, "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	id, err := appendActionPlanNode(plan, node, recipe)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHostScopedPreparationMirrorKeepsOrderedForceOutputVersions(t *testing.T) {
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	const output = "tools/objtool/fixdep.o"
	const earlierArtifact = ".linux-bzl-versions/earlier/tools/objtool/fixdep.o"
	selection := CompactKbuildSelection{
		Profile: "build:fixdep", Target: output, MakeTarget: output,
		Lifecycle: "prep", Scope: "host", Stage: "host",
	}
	earlier := appendSelectionScopeTestVersionedOutput(t, plan, "host", "host", output, earlierArtifact)
	if err := appendCompactKbuildHostPrepMirrors(plan, selection, earlier); err != nil {
		t.Fatal(err)
	}
	later := appendSelectionScopeTestOutput(t, plan, "host", "host", output)
	if err := appendCompactKbuildHostPrepMirrors(plan, selection, later); err != nil {
		t.Fatalf("ordered later native host writer failed to publish prep mirror: %v", err)
	}
	prepOwner, _, published := planProducerByOutput(plan, "prep", output)
	if !published {
		t.Fatal("source ordered later writer has no canonical prep publisher")
	}
	prepNode, ok := compactKbuildPlanNode(plan, prepOwner)
	if !ok || len(prepNode.Inputs) != 1 || prepNode.Inputs[0].ProducerID != later {
		t.Fatalf("canonical prep mirror = %#v, want later native host writer", prepNode)
	}
	if !slices.Contains(plan.Recipes[prepNode.Recipe].Arguments, "-preserve_mode") {
		t.Fatalf("prep projection strips executable permission from selected host output: %#v", plan.Recipes[prepNode.Recipe].Arguments)
	}
	seenPrivate := false
	for _, node := range plan.Nodes {
		if node.Stage != "prep" || len(node.Outputs) != 1 || node.Outputs[0].Path != output ||
			node.Outputs[0].ArtifactPath != earlierArtifact {
			continue
		}
		if len(node.Inputs) != 1 || node.Inputs[0].ProducerID != earlier {
			t.Fatalf("private prep mirror = %#v, want earlier exact host writer", node)
		}
		seenPrivate = true
	}
	if !seenPrivate {
		t.Fatal("earlier native host writer has no versioned prep mirror")
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("ordered mirrored host action plan invalid: %v", err)
	}
	// Distinct physical host stages can each contain one canonical writer;
	// without a source-selected publisher across stages neither may silently
	// claim the prep-tree path on behalf of the other.
	conflict := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	first := appendSelectionScopeTestOutput(t, conflict, "prehost", "prehost", output)
	earlyStage := selection
	earlyStage.Stage = "prehost"
	if err := appendCompactKbuildHostPrepMirrors(conflict, earlyStage, first); err != nil {
		t.Fatal(err)
	}
	second := appendSelectionScopeTestLiteralOutput(t, conflict, "host", "host", output, "", "distinct source recipe")
	if err := appendCompactKbuildHostPrepMirrors(conflict, selection, second); err == nil {
		t.Fatal("ambiguous prehost/host prep publishers silently replaced the earlier native writer")
	}
}

func TestHostScopedPreparationMirrorSelectsSourceOrderedCrossStagePublisher(t *testing.T) {
	const output = "tools/objtool/fixdep.o"
	makeWriter := func(name string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "tools/build/Makefile.build", "", `
cmd_host_cc_o_c = $(HOSTCC) -c -o $@ $<
`+output+`: tools/build/fixdep.c FORCE
	$(call if_changed_dep,host_cc_o_c)
`, nil)
	}
	first := makeWriter("build:fixdep-first")
	second := makeWriter("build:fixdep-second")
	firstArtifact := CompactKbuildVisibleArtifact{Path: output, Profile: first.Name, Target: output}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{first.Name}
	firstSelection := CompactKbuildSelection{
		Profile: first.Name, Target: output, MakeTarget: output,
		Lifecycle: "prep", Scope: "host", Stage: "prehost",
	}
	secondSelection := CompactKbuildSelection{
		Profile: second.Name, Target: output, MakeTarget: output,
		Lifecycle: "prep", Scope: "host", Stage: "host", UsesInitialObjectTree: true,
		InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
	}
	config := CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{first, second},
		KbuildSelections: []CompactKbuildSelection{firstSelection, secondSelection},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := compactKbuildSelectionKey{profile: first.Name, target: output, stage: "prehost"}
	secondKey := compactKbuildSelectionKey{profile: second.Name, target: output, stage: "host"}
	if owner, ok := graph.publishedOwner("prep", output); !ok || owner != secondKey {
		t.Fatalf("source-selected prep mirror publisher = (%#v, %t), want later host owner %#v", owner, ok, secondKey)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, selectionGraph: graph,
	}
	firstNative := appendSelectionScopeTestOutput(t, plan, "prehost", "prehost", output)
	secondNative := appendSelectionScopeTestOutput(t, plan, "host", "host", output)
	for _, candidate := range []struct {
		key compactKbuildSelectionKey
		id  string
	}{{firstKey, firstNative}, {secondKey, secondNative}} {
		if err := graph.recordMaterializedProducer(candidate.key, candidate.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := appendCompactKbuildHostPrepMirrors(plan, firstSelection, firstNative); err != nil {
		t.Fatal(err)
	}
	if err := appendCompactKbuildHostPrepMirrors(plan, secondSelection, secondNative); err != nil {
		t.Fatal(err)
	}
	var firstMirror, secondMirror string
	for _, node := range plan.Nodes {
		if node.Stage != "prep" || len(node.Outputs) != 1 || node.Outputs[0].Path != output ||
			len(node.Inputs) != 1 || node.Inputs[0].Role != "input" {
			continue
		}
		switch node.Inputs[0].ProducerID {
		case firstNative:
			firstMirror = node.ID
			if got := node.Outputs[0].ArtifactPath; !strings.HasPrefix(got, ".linux-bzl-versions/") || !strings.HasSuffix(got, "/"+output) {
				t.Fatalf("first prep mirror output = %#v, want private selected version", node.Outputs[0])
			}
		case secondNative:
			secondMirror = node.ID
			if !actionPlanOutputIsCanonical(node.Outputs[0]) {
				t.Fatalf("last source-ordered prep mirror is private: %#v", node.Outputs[0])
			}
		}
	}
	if firstMirror == "" || secondMirror == "" {
		t.Fatalf("prep mirror versions = (%q, %q), want both selected native producers", firstMirror, secondMirror)
	}
	for _, version := range []struct{ mirror, native string }{{firstMirror, firstNative}, {secondMirror, secondNative}} {
		if got := moduleSDKNativePrepMirrorProducer(plan, output, version.mirror); got != version.native {
			t.Fatalf("source-authenticated prep mirror %s originated from %s, want %s", version.mirror, got, version.native)
		}
	}
	prepOwner, _, ok := planProducerByOutput(plan, "prep", output)
	if !ok || prepOwner != secondMirror {
		t.Fatalf("canonical prep output owner = (%q, %t), want source last %s", prepOwner, ok, secondMirror)
	}
	seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	seedModuleSDKReleaseForTest(t, plan)
	if err := (&CompactMetadata{Config: config}).appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatalf("source-ordered native host mirrors cannot publish module SDK: %v", err)
	}
	projection := moduleSDKProjectionNodeForTest(t, plan, output)
	if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != secondMirror {
		t.Fatalf("module SDK fixdep.o projection = %#v, want last prep mirror %s", projection.Inputs, secondMirror)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("ordered cross-stage mirror plan invalid: %v", err)
	}
	forged, ok := compactKbuildPlanNode(plan, firstMirror)
	if !ok {
		t.Fatalf("source-selected first prep mirror %s is missing", firstMirror)
	}
	forged.ID = ""
	forged.Outputs = append([]ActionPlanOutput(nil), forged.Outputs...)
	forged.Outputs[0].ArtifactPath = ".linux-bzl-versions/forged/" + output
	forgedID, err := appendActionPlanNode(plan, forged, plan.Recipes[forged.Recipe])
	if err != nil {
		t.Fatal(err)
	}
	if got := moduleSDKNativePrepMirrorProducer(plan, output, forgedID); got != forgedID {
		t.Fatalf("arbitrary private copy %s acquired first selected native producer %s", forgedID, got)
	}

	// Two separately selected host producers in different physical trees
	// cannot claim one prep path without source order.
	second.InvocationPredecessors = nil
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, nil)
	secondSelection.UsesInitialObjectTree = false
	secondSelection.InitialObjectTreeArtifacts = ""
	config.KbuildProfiles[1] = second
	config.KbuildSelections[1] = secondSelection
	if _, err := newCompactKbuildSelectionGraph(config); err == nil || !strings.Contains(err.Error(), "ambiguous owners in physical tree \"prep\"") {
		t.Fatalf("unordered host prep mirror publisher error = %v", err)
	}
}

func TestHostScopedPreparationOutputsAreMirroredGenericallyIntoPrep(t *testing.T) {
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	const output = "arbitrary/upstream/generated-helper"
	hostProducer := appendSelectionScopeTestOutput(t, plan, "host", "host", output)
	selection := CompactKbuildSelection{
		Profile: "root", Target: output, MakeTarget: output, Lifecycle: "prep", Scope: "host", Stage: "host",
	}
	if err := appendCompactKbuildHostPrepMirrors(plan, selection, hostProducer); err != nil {
		t.Fatal(err)
	}
	prepProducer, slot, ok := planProducerByOutput(plan, "prep", output)
	if !ok || prepProducer == hostProducer || slot != 0 {
		t.Fatalf("prep mirror producer = (%q, %d, %t), host producer %q", prepProducer, slot, ok, hostProducer)
	}
	mirror, ok := compactKbuildPlanNode(plan, prepProducer)
	if !ok || mirror.Stage != "prep" || len(mirror.Inputs) != 1 || mirror.Inputs[0].ProducerID != hostProducer || mirror.Inputs[0].Slot != 0 {
		t.Fatalf("prep mirror = %#v, want generic copy from native host output", mirror)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("host-prep mirror plan validation failed: %v", err)
	}
}

func TestHostScopedPreparationSourceCheckKeepsCompletionWithoutPrepFile(t *testing.T) {
	const target = "check"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "Kbuild", "", `
CONFIG_SHELL := sh
obj := .
always-y += check
cmd = $(cmd_$(1))
cmd_check = $(CONFIG_SHELL) $<
check: scripts/check.sh FORCE
	$(call cmd,check)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "scripts/check.sh")
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
		metadata: metadata, Recipes: map[string]ActionRecipe{},
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).forOutput("host", "host", "sdk")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	selection := CompactKbuildSelection{
		Profile: profile.Name, Target: target, MakeTarget: target,
		Lifecycle: "prep", Scope: "host", Stage: "host",
	}
	if err := appendCompactKbuildHostPrepMirrors(plan, selection, producer); err != nil {
		t.Fatalf("source check without a Make file lost its host-prep ordering: %v", err)
	}
	if _, _, exists := planProducerByOutput(plan, "prep", target); exists {
		t.Fatal("outputless source check fabricated a prep-tree target file")
	}
	node, found := compactKbuildPlanNode(plan, producer)
	if !found || !compactKbuildHostPrepCheckCompletion(plan, target, "host", node) {
		t.Fatalf("source check producer %s lost its typed completion", producer)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("source check plan is invalid: %v", err)
	}

	// A plain observation alone does not prove an executed outputless check.
	badRecipe := plan.Recipes[node.Recipe]
	badRecipe.RequireAbsentObservedOutput = ""
	badRecipe.RequireUnchangedWorkingTree = false
	badPlan := &ActionPlan{Nodes: []ActionPlanNode{node}, Recipes: map[string]ActionRecipe{node.Recipe: badRecipe}}
	if err := appendCompactKbuildHostPrepMirrors(badPlan, selection, producer); err == nil {
		t.Fatal("unasserted observed-only host target bypassed the native-output gate")
	}
	badNode := node
	badNode.Outputs = append([]ActionPlanOutput(nil), node.Outputs...)
	badNode.Outputs = append(badNode.Outputs, ActionPlanOutput{
		Tree: "prep", Path: "unmirrored-sibling", ArtifactPath: ".linux-bzl-intermediate/unmirrored-sibling",
	})
	badPlan = &ActionPlan{Nodes: []ActionPlanNode{badNode}, Recipes: map[string]ActionRecipe{node.Recipe: plan.Recipes[node.Recipe]}}
	if err := appendCompactKbuildHostPrepMirrors(badPlan, selection, producer); err == nil {
		t.Fatal("unexpected native output of another tree bypassed the missing host-output gate")
	}
}

func TestHostScopedTargetLifecycleDoesNotCreatePrepMirror(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	const output = "tools/generated-helper"
	hostProducer := appendSelectionScopeTestOutput(t, plan, "host", "host", output)
	selection := CompactKbuildSelection{
		Profile: "root", Target: output, MakeTarget: output, Lifecycle: "target", Scope: "host", Stage: "host",
	}
	if err := appendCompactKbuildHostPrepMirrors(plan, selection, hostProducer); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := planProducerByOutput(plan, "prep", output); ok {
		t.Fatal("target-lifecycle host output unexpectedly received a prep mirror")
	}
}

func TestBootstrapPreparationOutputsProjectIntoSDKPrepTree(t *testing.T) {
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	const output = "include/generated/arbitrary-capability.h"
	bootstrapProducer := appendSelectionScopeTestOutput(t, plan, "bootstrap", "bootstrap", output)
	selection := CompactKbuildSelection{
		Profile: "root", Target: output, MakeTarget: output, Lifecycle: "prep", Scope: "target", Stage: "bootstrap",
	}
	if err := appendCompactKbuildBootstrapProjections(plan, CompactConfig{}, selection, bootstrapProducer); err != nil {
		t.Fatal(err)
	}
	prepProducer, slot, ok := planProducerByOutput(plan, "prep", output)
	if !ok || prepProducer == bootstrapProducer || slot != 0 {
		t.Fatalf("prep projection producer = (%q, %d, %t), bootstrap producer %q", prepProducer, slot, ok, bootstrapProducer)
	}
	projection, ok := compactKbuildPlanNode(plan, prepProducer)
	if !ok || projection.Stage != "prep" || projection.Product != "sdk" || len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != bootstrapProducer {
		t.Fatalf("bootstrap prep projection = %#v, want SDK copy from native bootstrap output", projection)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("bootstrap prep projection plan validation failed: %v", err)
	}
}

func TestHostBuilderPrefersNativeHostOutputOverPrepMirror(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	const output = "scripts/generated-tool"
	hostProducer := appendSelectionScopeTestOutput(t, plan, "host", "host", output)
	prepProducer := appendSelectionScopeTestOutput(t, plan, "prep", "prep", output)

	hostBuilder := newCompactKbuildRulePlanBuilder(nil, plan).forOutput("host", "host", "sdk")
	if got, _, ok := hostBuilder.existingProducer(output); !ok || got != hostProducer {
		t.Fatalf("host builder producer = (%q, %t), want native host %q", got, ok, hostProducer)
	}
	targetBuilder := newCompactKbuildRulePlanBuilder(nil, plan).forOutput("target", "objects", "vmlinux")
	if got, _, ok := targetBuilder.existingProducer(output); !ok || got != prepProducer {
		t.Fatalf("target builder producer = (%q, %t), want prep mirror %q", got, ok, prepProducer)
	}
}

func TestCompactKbuildSelectionRejectsInconsistentLifecycleScopeAndStage(t *testing.T) {
	profile := CompactKbuildProfile{
		Name: "root", EntryTargets: []string{"helper"},
		Rules: []KbuildRule{{Targets: []string{"helper"}, Recipe: []string{"touch $@"}}},
	}
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "helper", MakeTarget: "helper", Lifecycle: "prep", Scope: "host", Stage: "prep",
		}},
	})
	if err == nil {
		t.Fatal("selection graph accepted prep physical stage for host scope")
	}
}

func TestCompactKbuildSelectionRequiresExplicitLifecycleAndScope(t *testing.T) {
	profile := CompactKbuildProfile{
		Name: "root", EntryTargets: []string{"helper"},
		Rules: []KbuildRule{{Targets: []string{"helper"}, Recipe: []string{"touch $@"}}},
	}
	for _, test := range []struct {
		name      string
		selection CompactKbuildSelection
	}{
		{
			name: "missing lifecycle",
			selection: CompactKbuildSelection{
				Profile: profile.Name, Target: "helper", MakeTarget: "helper", Scope: "target", Stage: "target",
			},
		},
		{
			name: "missing scope",
			selection: CompactKbuildSelection{
				Profile: profile.Name, Target: "helper", MakeTarget: "helper", Lifecycle: "target", Stage: "target",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newCompactKbuildSelectionGraph(CompactConfig{
				KbuildProfiles:   []CompactKbuildProfile{profile},
				KbuildSelections: []CompactKbuildSelection{test.selection},
			})
			if err == nil {
				t.Fatalf("selection graph accepted %#v", test.selection)
			}
		})
	}
}

func TestCompactKbuildSelectionAllowsOnlyTargetScopeInBootstrapStage(t *testing.T) {
	profile := CompactKbuildProfile{
		Name: "root", EntryTargets: []string{"input.o"},
		Rules: []KbuildRule{{Targets: []string{"input.o"}, Recipe: []string{"touch $@"}}},
	}
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "input.o", MakeTarget: "input.o", Lifecycle: "target", Scope: "target", Stage: "bootstrap",
		}},
	})
	if err != nil {
		t.Fatalf("target bootstrap selection rejected: %v", err)
	}
	_, err = newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "input.o", MakeTarget: "input.o", Lifecycle: "target", Scope: "host", Stage: "bootstrap",
		}},
	})
	if err == nil {
		t.Fatal("bootstrap selection accepted host toolchain scope")
	}
}

func TestCompactKbuildSelectionAllowsOnlyHostScopeInPrehostStage(t *testing.T) {
	profile := CompactKbuildProfile{
		Name: "root", EntryTargets: []string{"helper"},
		Rules: []KbuildRule{{Targets: []string{"helper"}, Recipe: []string{"touch $@"}}},
	}
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "helper", MakeTarget: "helper", Lifecycle: "target", Scope: "host", Stage: "prehost",
		}},
	})
	if err != nil {
		t.Fatalf("host prehost selection rejected: %v", err)
	}
	_, err = newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: profile.Name, Target: "helper", MakeTarget: "helper", Lifecycle: "target", Scope: "target", Stage: "prehost",
		}},
	})
	if err == nil {
		t.Fatal("prehost selection accepted target toolchain scope")
	}
}
