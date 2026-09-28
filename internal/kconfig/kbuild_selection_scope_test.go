package kconfig

import (
	"path/filepath"
	"testing"
)

func appendSelectionScopeTestOutput(t *testing.T, plan *ActionPlan, stage, tree, output string) string {
	t.Helper()
	node := ActionPlanNode{
		Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: tree, Path: output}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-literal", "fixture", "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	id, err := appendActionPlanNode(plan, node, recipe)
	if err != nil {
		t.Fatal(err)
	}
	return id
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
