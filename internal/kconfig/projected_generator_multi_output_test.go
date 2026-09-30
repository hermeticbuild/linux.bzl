package kconfig

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"path"
)

const (
	multiProjectedTarget       = "include/generated/multi-projected.h"
	multiProjectedOrdinarySide = "include/generated/.multi-projected.cmd"
	multiProjectedObserved     = "include/generated/.multi-projected.state"
)

type multiProjectedFixture struct {
	plan                        *ActionPlan
	generator                   string
	targetConsumer, cmdConsumer string
	observedConsumer            string
	originalOutputs             []ActionPlanOutput
	originalRecipe              ActionRecipe
}

func multiProjectedPlanForTest(t *testing.T, other string) multiProjectedFixture {
	t.Helper()
	metadata := &CompactMetadata{
		configFragment:       map[string]string{"CONFIG_USED": "y", "CONFIG_OTHER": other},
		configSymbolUniverse: []string{"CONFIG_OTHER", "CONFIG_USED"},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "tools/project.awk"},
			{ID: "src-00000002", Namespace: "kernel", Path: "include/features"},
			{ID: "src-00000003", Namespace: "config", Path: ".config"},
		},
		Recipes: map[string]ActionRecipe{}, metadata: metadata,
	}
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	configCopy, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: "src-00000003"}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: ".config"}},
	}, copyRecipe)
	if err != nil {
		t.Fatal(err)
	}
	statePath := path.Join(compactKbuildSideOutputStateDirectory, strings.Repeat("a", 64)+".state")
	outputs := []ActionPlanOutput{
		{Tree: "objects", Path: multiProjectedOrdinarySide},
		{Tree: "objects", Path: multiProjectedTarget},
		{Tree: "objects", Path: statePath, ObservedPath: multiProjectedObserved},
	}
	plan.observedOutputBases = map[string][]ActionPlanNodeEdge{
		actionPlanLookupKey(outputs[2].Tree, actionPlanOutputArtifactPath(outputs[2])): {
			{Role: "base", ProducerID: configCopy, Slot: 0},
		},
	}
	generatorRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "awk",
		Arguments: []string{
			"-f", "${source:script:00000000}",
			"${source:features:00000001}", "${input:config:00000000}",
		},
		WorkingDirectory: "multi-project",
		WorkingInputs: map[string]string{
			"source:script:00000000":   "tools/project.awk",
			"source:features:00000001": "include/features",
			"input:config:00000000":    ".config",
		},
		WorkingOutputs: map[string]string{"00000000": multiProjectedOrdinarySide},
		Sources:        []string{"script:00000000", "features:00000001"},
		Inputs:         []string{"config:00000000"},
		Outputs:        []string{"00000000", "00000001", "00000002"},
		Stdout:         "00000001",
	}
	generator, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "awk", Product: "image",
		Sources: []ActionPlanSourceEdge{
			{Role: "script", SourceID: "src-00000001"},
			{Role: "features", SourceID: "src-00000002"},
		},
		Inputs:  []ActionPlanNodeEdge{{Role: "config", ProducerID: configCopy, Slot: 0}},
		Outputs: slices.Clone(outputs),
	}, generatorRecipe)
	if err != nil {
		t.Fatal(err)
	}
	originalRecipe := cloneActionRecipe(plan.Recipes[projectedGeneratorNodeForTest(t, plan, generator).Recipe])
	plan.projectedGeneratorCandidates = map[string]projectedGeneratorCandidate{generator: {
		TargetPath: multiProjectedTarget, TargetSlot: 1,
		ConfigProjectionPrefixes: []string{"CONFIG_USED"},
	}}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${input:value:00000000}", "-out", "${output:00000000}"},
		Inputs:    []string{"value:00000000"}, Outputs: []string{"00000000"},
	}
	consumer := func(name string, slot int) string {
		id, appendErr := appendActionPlanNode(plan, ActionPlanNode{
			Stage: "target", Kind: "copy", Tool: "actionfile", Product: "image",
			Inputs:  []ActionPlanNodeEdge{{Role: "value", ProducerID: generator, Slot: slot}},
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "consumers/" + name}},
		}, consumerRecipe)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return id
	}
	fixture := multiProjectedFixture{
		plan: plan, generator: generator,
		targetConsumer: consumer("target", 1), cmdConsumer: consumer("cmd", 0),
		observedConsumer: consumer("observed", 2), originalOutputs: slices.Clone(outputs),
		originalRecipe: originalRecipe,
	}
	plan.Products = []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}}
	return fixture
}

func multiProjectedSnapshotForTest(t *testing.T, other string) ActionPlanSnapshot {
	t.Helper()
	fixture := multiProjectedPlanForTest(t, other)
	if err := lowerProjectedGeneratorsForFamilySnapshot(fixture.plan); err != nil {
		t.Fatal(err)
	}
	analysis, err := BuildActionPlanConfigDependencyAnalysis(fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(fixture.plan); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.plan.entries(); err != nil {
		t.Fatalf("content-addressed multi-output plan ownership: %v", err)
	}
	dependencies, err := analysis.ByNodeID(fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := canonicalActionPlanSnapshot(fixture.plan, dependencies, familyTestConfig("y", other))
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestProjectedGeneratorMultiOutputLoweringAndFamily(t *testing.T) {
	fixture := multiProjectedPlanForTest(t, "n")
	plan := fixture.plan
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	raw := projectedGeneratorNodeForTest(t, plan, fixture.generator)
	comparator := projectedGeneratorNodeForTest(t, plan, plan.projectedGeneratorValidations[0])
	full := projectedGeneratorNodeForTest(t, plan, comparator.Inputs[1].ProducerID)
	if comparator.Inputs[0].Slot != 1 || comparator.Inputs[1].Slot != 1 {
		t.Fatalf("comparator target slots = %#v", comparator.Inputs)
	}
	if len(raw.Outputs) != 3 || len(full.Outputs) != 3 {
		t.Fatalf("raw/full outputs = %d/%d, want 3/3", len(raw.Outputs), len(full.Outputs))
	}
	for slot := range raw.Outputs {
		if !projectedGeneratorOutputIsInternal(plan, raw.ID, slot) ||
			!strings.HasPrefix(raw.Outputs[slot].ArtifactPath, compactKbuildProjectedFilechkRoot+"/") ||
			raw.Outputs[slot].ObservedPath != fixture.originalOutputs[slot].ObservedPath {
			t.Fatalf("raw slot %d = %#v", slot, raw.Outputs[slot])
		}
	}
	wantFullRecipe := cloneActionRecipe(plan.Recipes[raw.Recipe])
	wantFullRecipe.ConfigProjectionPrefixes = nil
	if !reflect.DeepEqual(plan.Recipes[full.Recipe], fixture.originalRecipe) ||
		!reflect.DeepEqual(plan.Recipes[full.Recipe], wantFullRecipe) ||
		!reflect.DeepEqual(raw.Inputs, full.Inputs) {
		t.Fatalf("full replay rebound observed-state recipe/edges\nraw=%#v\nfull=%#v", raw, full)
	}
	if len(full.Inputs) != 2 || len(plan.Recipes[full.Recipe].ObservedOutputBases["00000002"]) != 1 {
		t.Fatalf("full replay duplicated observed base: inputs=%#v recipe=%#v", full.Inputs, plan.Recipes[full.Recipe])
	}
	for slot, original := range fixture.originalOutputs {
		if slot == 1 {
			if !projectedGeneratorOutputIsInternal(plan, full.ID, slot) ||
				!strings.HasPrefix(full.Outputs[slot].ArtifactPath, compactKbuildProjectionValidateRoot+"/") {
				t.Fatalf("full target slot = %#v", full.Outputs[slot])
			}
		} else if projectedGeneratorOutputIsInternal(plan, full.ID, slot) || !reflect.DeepEqual(full.Outputs[slot], original) {
			t.Fatalf("full side slot %d = %#v, want %#v", slot, full.Outputs[slot], original)
		}
	}
	assertInput := func(id, producer string, slot int) {
		node := projectedGeneratorNodeForTest(t, plan, id)
		if len(node.Inputs) != 1 || node.Inputs[0].ProducerID != producer || node.Inputs[0].Slot != slot {
			t.Fatalf("consumer %s inputs = %#v, want %s[%d]", id, node.Inputs, producer, slot)
		}
	}
	canonical := projectedGeneratorNodeForTest(t, plan, fixture.targetConsumer).Inputs[0].ProducerID
	assertInput(fixture.targetConsumer, canonical, 0)
	assertInput(fixture.cmdConsumer, full.ID, 0)
	assertInput(fixture.observedConsumer, full.ID, 2)

	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: multiProjectedSnapshotForTest(t, "n")},
		{Name: "irrelevant", Snapshot: multiProjectedSnapshotForTest(t, "y")},
	})
	if err != nil {
		t.Fatal(err)
	}
	fullIDs, privateIDs := map[string]bool{}, map[string]bool{}
	for _, node := range family.Nodes {
		recipe := family.Recipes[node.Recipe]
		if recipe.Tool == "awk" && len(recipe.ConfigProjectionPrefixes) == 0 {
			fullIDs[node.ID] = true
		}
		if len(recipe.ConfigProjectionPrefixes) != 0 || len(recipe.Arguments) != 0 && recipe.Arguments[0] == "-compare_input" {
			privateIDs[node.ID] = true
		}
	}
	if len(fullIDs) != 2 {
		t.Fatalf("full family replay count = %d, want 2", len(fullIDs))
	}
	for id := range fullIDs {
		slots := []int{}
		for _, view := range family.Views {
			if view.NodeID == id {
				slots = append(slots, view.Slot)
			}
		}
		sort.Ints(slots)
		if !slices.Equal(slots, []int{0, 2}) {
			t.Fatalf("full replay %s public slots = %v, want [0 2]", id, slots)
		}
	}
	for _, view := range family.Views {
		if privateIDs[view.NodeID] || fullIDs[view.NodeID] && view.Slot == 1 {
			t.Fatalf("internal output escaped to view: %#v", view)
		}
	}
}

func TestActionPlanFamilyViewBindsExactMultiOutputSlot(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: multiProjectedSnapshotForTest(t, "n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		nodes[node.ID] = node
	}
	for index, view := range family.Views {
		node := nodes[view.NodeID]
		for slot, output := range node.Outputs {
			artifactPath := actionPlanOutputArtifactPath(output)
			if slot == view.Slot || output.Tree != view.Tree || artifactPath == view.ArtifactPath {
				continue
			}
			family.Views[index].ArtifactPath = artifactPath
			if _, err := family.ReuseReport(); err == nil || !strings.Contains(err.Error(), "disagrees with producer") {
				t.Fatalf("ReuseReport() error = %v, want exact-slot view rejection", err)
			}
			return
		}
	}
	t.Fatal("multi-output family has no view with another output in the same tree")
}

func TestProjectedGeneratorOriginalOutputCommitmentRejectsTamper(t *testing.T) {
	snapshot := multiProjectedSnapshotForTest(t, "n")
	if len(snapshot.ProjectedGeneratorOutputs) != 1 {
		t.Fatalf("commitments = %#v, want one", snapshot.ProjectedGeneratorOutputs)
	}
	tampered := snapshot
	tampered.ProjectedGeneratorOutputs = slices.Clone(snapshot.ProjectedGeneratorOutputs)
	tampered.ProjectedGeneratorOutputs[0].Outputs = slices.Clone(snapshot.ProjectedGeneratorOutputs[0].Outputs)
	tampered.ProjectedGeneratorOutputs[0].Outputs[0].Path += ".tampered"
	if err := tampered.validate(); err == nil || !strings.Contains(err.Error(), "exact private raw descriptor") {
		t.Fatalf("tampered commitment error = %v", err)
	}
}

func TestProjectedGeneratorIdentityNormalizesPlannerOwnedOutputVector(t *testing.T) {
	state := func(digit string) ActionPlanOutput {
		return ActionPlanOutput{
			Tree:         "objects",
			Path:         path.Join(compactKbuildSideOutputStateDirectory, strings.Repeat(digit, 64)+".state"),
			ObservedPath: multiProjectedObserved,
		}
	}
	first := []ActionPlanOutput{
		{Tree: "objects", Path: multiProjectedOrdinarySide, ArtifactPath: familySideOutputArtifactDirectory + "/first"},
		{Tree: "objects", Path: multiProjectedTarget}, state("a"),
	}
	second := slices.Clone(first)
	second[0].ArtifactPath = familySideOutputArtifactDirectory + "/second"
	second[2] = state("b")
	firstID, secondID := compactKbuildProjectedGeneratorIdentity(first, 1), compactKbuildProjectedGeneratorIdentity(second, 1)
	if firstID != secondID {
		t.Fatalf("planner allocations split identity: %s != %s", firstID, secondID)
	}
	second[0].ArtifactPath = "vendor/private-side"
	if compactKbuildProjectedGeneratorIdentity(second, 1) == firstID {
		t.Fatal("non-planner-owned artifact path did not split identity")
	}
}
