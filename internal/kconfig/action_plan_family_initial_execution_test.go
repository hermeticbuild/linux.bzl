package kconfig

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A small early header feeds three compilers. A later image-derived header
// needs all three objects and feeds a fourth compiler. Selecting the latter
// must not pin the three compilers that observing the former could unblock.
func headerExecutionSelectionFamilyForTest(t *testing.T) (*ActionPlanFamily, []*actionPlanFamilyHeaderDemandGroup) {
	return headerExecutionSelectionFamilyWithPrerequisitesForTest(t, 3)
}

func headerExecutionSelectionFamilyWithPrerequisitesForTest(t *testing.T, prerequisites int) (*ActionPlanFamily, []*actionPlanFamilyHeaderDemandGroup) {
	t.Helper()
	snapshot := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	plan := snapshotActionPlan(snapshot)
	var compiler ActionPlanNode
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			compiler = node
		}
	}
	lateRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"}}
	lateRecipeID, err := lateRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[lateRecipeID] = lateRecipe
	late := ActionPlanNode{ID: "late-header", Stage: "target", Kind: "generate", Tool: "actionfile", Recipe: lateRecipeID, Product: "image",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "include/generated/late.h"}}}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		node := compiler
		if index != 0 {
			node.ID = fmt.Sprintf("early-compiler-%d", index)
			node.Outputs = []ActionPlanOutput{{Tree: "objects", Path: fmt.Sprintf("drivers/early-%d.o", index)}}
			plan.Nodes = append(plan.Nodes, node)
		}
		if index >= prerequisites {
			continue
		}
		late.InputSet, err = store.Insert(late.InputSet, ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: node.Outputs[0].Path}, ProducerID: node.ID, Slot: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	lateCompiler := compiler
	lateCompiler.ID = "late-compiler"
	lateCompiler.Inputs = []ActionPlanNodeEdge{{Role: "generated", ProducerID: late.ID, Slot: 0}}
	lateCompiler.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/late.o"}}
	plan.Nodes = append(plan.Nodes, late, lateCompiler)
	snapshot = initialExecutionSnapshotFromPlanForTest(t, plan, snapshot.ConfigFiles)
	family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	groups := []*actionPlanFamilyHeaderDemandGroup{
		{key: "early", roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}},
		{key: "late", roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}},
	}
	for _, root := range executionCutRootsForTest(family, "include/generated/full-config.h") {
		groups[0].roots[root] = true
	}
	for _, root := range executionCutRootsForTest(family, "include/generated/late.h") {
		groups[1].roots[root] = true
	}
	for _, node := range family.Nodes {
		if node.Kind != "compile" {
			continue
		}
		index := 0
		if node.Outputs[0].Path == "drivers/late.o" {
			index = 1
		}
		groups[index].consumers["base/"+node.ID] = node.ID
	}
	if len(family.Nodes) != 6 || len(groups[0].consumers) != 3 || len(groups[1].consumers) != 1 {
		t.Fatalf("unexpected selection fixture: nodes=%d early=%v late=%v", len(family.Nodes), groups[0], groups[1])
	}
	return family, groups
}

func TestActionPlanFamilyInitialExecutionSelectionRetainsCompilerBenefit(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	for _, nodeLimit := range []int{2, MaxActionPlanFamilyExecutionCutNodes} {
		limits := defaultActionPlanFamilyExecutionCutLimits()
		limits.nodes = nodeLimit
		cut, err := selectActionPlanFamilyHeaderExecution(family, groups, limits, 128)
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Collect(maps.Keys(groups[0].roots))
		if len(cut.NodeIDs()) != 1 || !slices.Equal(cut.Roots(), want) {
			t.Fatalf("node budget %d pinned useful compilers: roots=%v nodes=%v", nodeLimit, cut.Roots(), cut.NodeIDs())
		}
		if _, err := cut.Verify(family); err != nil {
			t.Fatal(err)
		}
		reordered, err := selectActionPlanFamilyHeaderExecution(family, []*actionPlanFamilyHeaderDemandGroup{groups[1], groups[0]}, limits, 128)
		if err != nil || reordered.ID() != cut.ID() {
			t.Fatalf("demand iteration order changed selection: %v", err)
		}
	}
}

func TestActionPlanFamilyInitialExecutionSelectionSkipsOversizedGroups(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	// Rank an oversized group first. Its rejection must not discard the later
	// small group, and the attempt bound must be independent of acceptance.
	groups[1].consumers = maps.Clone(groups[0].consumers)
	groups[1].consumers["extra"] = slices.Collect(maps.Values(groups[0].consumers))[0]
	limits := defaultActionPlanFamilyExecutionCutLimits()
	limits.nodes = 2
	for _, attempts := range []int{1, 2} {
		cut, err := selectActionPlanFamilyHeaderExecution(family, groups, limits, attempts)
		if err != nil || len(cut.NodeIDs()) != attempts-1 {
			t.Fatalf("attempts %d did not skip oversized group: %v %v", attempts, cut, err)
		}
	}
	cut, err := selectActionPlanFamilyHeaderExecution(family, groups[1:], limits, 128)
	if err != nil || len(cut.Roots()) != 0 {
		t.Fatalf("nothing fitting the budget must yield a valid empty cut: %v %v", cut, err)
	}
}

func TestActionPlanFamilyInitialExecutionSelectionKeepsGroupsAtomic(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{Name: "a", Snapshot: familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)},
		{Name: "b", Snapshot: familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "1"), true)},
	}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	group := &actionPlanFamilyHeaderDemandGroup{key: "header", roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
	for _, root := range executionCutRootsForTest(family, "include/generated/full-config.h") {
		group.roots[root] = true
	}
	for _, node := range family.Nodes {
		if node.Kind == "compile" {
			group.consumers[node.ID] = node.ID
		}
	}
	if len(group.roots) != 2 {
		t.Fatal("fixture must have two distinct original generators")
	}
	for name, change := range map[string]func(*actionPlanFamilyExecutionCutLimits){
		"nodes": func(l *actionPlanFamilyExecutionCutLimits) { l.nodes = 1 },
		"roots": func(l *actionPlanFamilyExecutionCutLimits) { l.outputs = 1 },
		"bytes": func(l *actionPlanFamilyExecutionCutLimits) { l.bytes = 1024 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := defaultActionPlanFamilyExecutionCutLimits()
			change(&limits)
			cut, err := selectActionPlanFamilyHeaderExecution(family, []*actionPlanFamilyHeaderDemandGroup{group}, limits, 128)
			if err != nil || len(cut.Roots()) != 0 {
				t.Fatalf("partially accepted cross-variant group: %v %v", cut, err)
			}
		})
	}
}

func TestActionPlanFamilyInitialExecutionSelectionRejectsMalformedContracts(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	groups[0].roots = map[ActionPlanFamilyExecutionCutRoot]bool{{NodeID: strings.Repeat("f", 64), Slot: 0}: true}
	if cut, err := selectActionPlanFamilyHeaderExecution(family, groups, defaultActionPlanFamilyExecutionCutLimits(), 128); err == nil || cut != nil {
		t.Fatalf("malformed root was treated as an optimization budget: %v %v", cut, err)
	}
	if cut, err := selectActionPlanFamilyHeaderExecution(family, nil, defaultActionPlanFamilyExecutionCutLimits(), 0); err == nil || cut != nil {
		t.Fatalf("invalid attempt budget accepted: %v %v", cut, err)
	}
	family.Nodes[0].Recipe = "missing"
	if cut, err := selectActionPlanFamilyHeaderExecution(family, nil, defaultActionPlanFamilyExecutionCutLimits(), 128); err == nil || cut != nil {
		t.Fatalf("malformed family was treated as an optimization budget: %v %v", cut, err)
	}
}

func initialExecutionDemandForTest(t *testing.T, snapshot ActionPlanSnapshot, pathname string) ConfigDependencyGeneratedHeaderDemand {
	t.Helper()
	var consumer ActionPlanNode
	var demand ConfigDependencyGeneratedHeaderDemand
	for _, node := range snapshot.Nodes {
		if node.Kind == "compile" {
			consumer = node
		}
		for slot, output := range node.Outputs {
			if output.Path == pathname {
				demand = ConfigDependencyGeneratedHeaderDemand{
					ProducerNodeID: node.ID, Slot: slot, Tree: output.Tree, Path: output.Path,
					ArtifactPath: actionPlanOutputArtifactPath(output), LogicalPath: pathname,
				}
			}
		}
	}
	if consumer.ID == "" || demand.ProducerNodeID == "" {
		t.Fatal("initial execution fixture lacks consumer or producer")
	}
	demand.ConsumerNodeID = consumer.ID
	return demand
}

func initialExecutionSnapshotFromPlanForTest(t *testing.T, plan *ActionPlan, files map[string]string) ActionPlanSnapshot {
	t.Helper()
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial fixture"}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, files)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestBuildConservativeActionPlanFamilyKeepsFullConfigAndOriginalSnapshots(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("1", "0"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})},
		{Name: "other", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("1", "1"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})},
	}
	before := [][]byte{}
	for _, variant := range variants {
		data, err := marshalCanonicalActionPlanSnapshot(variant.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, data)
	}
	precise, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	full, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if len(precise.Nodes) != 1 || len(full.Nodes) != 2 {
		t.Fatalf("conservative family retained precision sharing: precise=%d full=%d", len(precise.Nodes), len(full.Nodes))
	}
	for index, variant := range variants {
		after, err := marshalCanonicalActionPlanSnapshot(variant.Snapshot)
		if err != nil || !bytes.Equal(after, before[index]) {
			t.Fatalf("mutated original snapshot %s: %v", variant.Name, err)
		}
		found := false
		for _, capsule := range full.Capsules {
			if maps.Equal(capsule, variant.Snapshot.ConfigFiles) {
				found = true
			}
		}
		if !found {
			t.Fatalf("lost full resolved configuration for %s", variant.Name)
		}
	}
}

func TestActionPlanFamilyInitialExecutionMapsSharedUnionAndReconstructs(t *testing.T) {
	const pathname = "include/generated/full-config.h"
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)},
		{Name: "same", Snapshot: familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)},
		{Name: "other", Snapshot: familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "1"), true)},
	}
	demands := map[string]ConfigDependencyGeneratedHeaderDemandCollection{}
	for _, variant := range variants {
		demand := initialExecutionDemandForTest(t, variant.Snapshot, pathname)
		demands[variant.Name] = ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand, demand}}
	}
	initial, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Family.Nodes) != 4 || len(initial.Cut.Roots()) != 2 || len(initial.Cut.NodeIDs()) != 2 || len(initial.Cut.Origins()) != 3 {
		t.Fatalf("wrong complete family/shared cut shape: family=%d roots=%v nodes=%v origins=%v", len(initial.Family.Nodes), initial.Cut.Roots(), initial.Cut.NodeIDs(), initial.Cut.Origins())
	}
	for _, output := range initial.Cut.Outputs() {
		if output.Output.Path != pathname {
			t.Fatalf("cut selected unrelated output: %v", output)
		}
	}
	reconstructed, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	data, err := initial.Cut.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadActionPlanFamilyExecutionCut(reconstructed, executionCutWriteTransportForTest(t, data))
	if err != nil || read.ID() != initial.Cut.ID() {
		t.Fatalf("initial cut failed exact snapshot reconstruction: %v", err)
	}
	slices.Reverse(variants)
	reordered, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil || reordered.Cut.ID() != initial.Cut.ID() {
		t.Fatalf("variant order changed cut: %v", err)
	}
}

func TestActionPlanFamilyInitialExecutionKeepsPrivateOutputSlotProvenance(t *testing.T) {
	const pathname = "include/generated/full-config.h"
	base := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	plan := snapshotActionPlan(base)
	for index := range plan.Nodes {
		for slot := range plan.Nodes[index].Outputs {
			output := &plan.Nodes[index].Outputs[slot]
			if output.Path == pathname {
				output.ArtifactPath = familySelectionArtifactDirectory + "/" + strings.Repeat("a", 64) + "/" + output.Path
			}
		}
	}
	snapshot := initialExecutionSnapshotFromPlanForTest(t, plan, base.ConfigFiles)
	demand := initialExecutionDemandForTest(t, snapshot, pathname)
	initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}},
		map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Cut.Roots()) != 1 {
		t.Fatal("private output demand was dropped")
	}
	output := initial.Cut.Outputs()[0]
	if output.Slot != demand.Slot || output.Output.Tree != demand.Tree || output.Output.Path != demand.Path ||
		!strings.HasPrefix(output.Output.ArtifactPath, familyOwnedArtifactDirectory+"/") || output.Output.ArtifactPath == demand.ArtifactPath {
		t.Fatalf("wrong mapped private output: %v", output)
	}
}

func TestActionPlanFamilyInitialExecutionSkipsDeadConsumerAndProducer(t *testing.T) {
	const pathname = "include/generated/full-config.h"
	base := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	plan := snapshotActionPlan(base)
	var unused ActionPlanNode
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			unused = node
		}
	}
	unused.ID = "unused-compiler"
	unused.Stage = "prep"
	unused.Outputs = []ActionPlanOutput{{Tree: "prep", Path: "unused.o"}}
	plan.Nodes = append(plan.Nodes, unused)
	snapshot := initialExecutionSnapshotFromPlanForTest(t, plan, base.ConfigFiles)
	var deadID, liveID string
	for _, node := range snapshot.Nodes {
		if node.Outputs[0].Path == "unused.o" {
			deadID = node.ID
		} else if node.Kind == "compile" {
			liveID = node.ID
		}
	}
	demand := initialExecutionDemandForTest(t, snapshot, pathname)
	demand.ConsumerNodeID = deadID
	for _, deadProducer := range []bool{false, true} {
		if deadProducer {
			demand.ConsumerNodeID = liveID
			demand.ProducerNodeID, demand.Slot, demand.Tree, demand.Path, demand.ArtifactPath = deadID, 0, "prep", "unused.o", "unused.o"
		}
		initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}},
			map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}})
		if err != nil || len(initial.Cut.Roots()) != 0 {
			t.Fatalf("dead original was resurrected: %v %v", initial, err)
		}
		if _, exists := initial.Family.originalNodeIDs["base"][deadID]; exists {
			t.Fatal("fixture did not prune dead compiler")
		}
	}
}

func TestActionPlanFamilyInitialExecutionAbandonsIncompleteOrCappedFrontier(t *testing.T) {
	snapshot := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	variants := []ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}}
	demand := initialExecutionDemandForTest(t, snapshot, "include/generated/full-config.h")
	for name, collection := range map[string]ConfigDependencyGeneratedHeaderDemandCollection{
		"disabled": {}, "truncated": {Enabled: true, Truncated: true}, "empty": {Enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			initial, err := NewActionPlanFamilyInitialExecution(variants, map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": collection})
			if err != nil || len(initial.Cut.NodeIDs()) != 0 || len(initial.Family.Nodes) != 2 {
				t.Fatalf("incomplete frontier did not retain full family + empty cut: %v %v", initial, err)
			}
		})
	}
	if initial, err := NewActionPlanFamilyInitialExecution(variants, nil); err != nil || len(initial.Cut.NodeIDs()) != 0 {
		t.Fatalf("absent frontier = %v %v", initial, err)
	}
	for _, limits := range [][2]int{{1, MaxActionPlanFamilyExecutionCutBytes}, {MaxActionPlanFamilyExecutionCutRecords, 1}} {
		initial, err := newActionPlanFamilyInitialExecution(variants,
			map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand, demand}}}, limits[0], limits[1])
		if err != nil || len(initial.Cut.NodeIDs()) != 0 || len(initial.Family.Nodes) != 2 {
			t.Fatalf("budget did not abandon entire optimization: %v %v", initial, err)
		}
	}
}

func TestActionPlanFamilyInitialExecutionRejectsMalformedDemands(t *testing.T) {
	snapshot := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	variants := []ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}}
	original := initialExecutionDemandForTest(t, snapshot, "include/generated/full-config.h")
	for name, mutate := range map[string]func(*ConfigDependencyGeneratedHeaderDemand){
		"consumer":      func(d *ConfigDependencyGeneratedHeaderDemand) { d.ConsumerNodeID = strings.Repeat("f", 64) },
		"producer":      func(d *ConfigDependencyGeneratedHeaderDemand) { d.ProducerNodeID = strings.Repeat("f", 64) },
		"negative-slot": func(d *ConfigDependencyGeneratedHeaderDemand) { d.Slot = -1 },
		"missing-slot":  func(d *ConfigDependencyGeneratedHeaderDemand) { d.Slot = 999 },
		"tree":          func(d *ConfigDependencyGeneratedHeaderDemand) { d.Tree = "metadata" },
		"path":          func(d *ConfigDependencyGeneratedHeaderDemand) { d.Path = "different.h" },
		"artifact":      func(d *ConfigDependencyGeneratedHeaderDemand) { d.ArtifactPath = "different.h" },
		"logical":       func(d *ConfigDependencyGeneratedHeaderDemand) { d.LogicalPath = "../escape.h" },
	} {
		t.Run(name, func(t *testing.T) {
			demand := original
			mutate(&demand)
			initial, err := NewActionPlanFamilyInitialExecution(variants,
				map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}})
			if err == nil || initial != nil {
				t.Fatalf("accepted malformed demand: %v %v", initial, err)
			}
		})
	}
	for _, demands := range []map[string]ConfigDependencyGeneratedHeaderDemandCollection{
		{"unknown": {Enabled: true}},
		{"base": {Demands: []ConfigDependencyGeneratedHeaderDemand{original}}},
		{"base": {Enabled: true, Truncated: true, Demands: []ConfigDependencyGeneratedHeaderDemand{original}}},
	} {
		if initial, err := NewActionPlanFamilyInitialExecution(variants, demands); err == nil || initial != nil {
			t.Fatalf("accepted malformed collection: %v %v", initial, err)
		}
	}
}

func TestActionPlanFamilyInitialExecutionExcludesConfigProjectionAliases(t *testing.T) {
	snapshot := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("1", "0"), true)
	demand := initialExecutionDemandForTest(t, snapshot, "include/generated/full-config.h")
	demand.LogicalPath = "include/generated/autoconf.h"
	initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}},
		map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}})
	if err != nil || len(initial.Cut.Roots()) != 0 {
		t.Fatalf("config projection alias acquired observed-source authority: %v %v", initial, err)
	}
	for _, projection := range resolvedConfigProjections() {
		plan := snapshotActionPlan(snapshot)
		for index := range plan.Nodes {
			if plan.Nodes[index].ID == demand.ProducerNodeID {
				plan.Nodes[index].Outputs[0].Path = projection.output
			}
		}
		changed := initialExecutionSnapshotFromPlanForTest(t, plan, snapshot.ConfigFiles)
		projectionDemand := initialExecutionDemandForTest(t, changed, projection.output)
		initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: changed}},
			map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{projectionDemand}}})
		if err != nil || len(initial.Cut.Roots()) != 0 {
			t.Fatalf("config output %s acquired observed-source authority: %v %v", projection.output, initial, err)
		}
	}
}

func TestActionPlanFamilyInitialExecutionRejectsObservedSlot(t *testing.T) {
	snapshot := familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), true)
	var observed, consumer ActionPlanNode
	for _, node := range snapshot.Nodes {
		if node.Outputs[0].ObservedPath != "" {
			observed = node
		} else {
			consumer = node
		}
	}
	output := observed.Outputs[0]
	demand := ConfigDependencyGeneratedHeaderDemand{
		ConsumerNodeID: consumer.ID, ProducerNodeID: observed.ID, Tree: output.Tree, Path: output.Path,
		ArtifactPath: actionPlanOutputArtifactPath(output), LogicalPath: "header.h",
	}
	initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}},
		map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": {Enabled: true, Demands: []ConfigDependencyGeneratedHeaderDemand{demand}}})
	if err == nil || initial != nil || !strings.Contains(err.Error(), "observed-state") {
		t.Fatalf("observed envelope accepted as ordinary bytes: %v %v", initial, err)
	}
}

func TestBuildConservativeActionPlanFamilyRejectsMalformedOriginalAnnotations(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("1", "0"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	snapshot.ConfigDependencies = map[string]ConfigDependencySet{}
	before := maps.Clone(snapshot.ConfigDependencies)
	if family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}}); err == nil || family != nil {
		t.Fatalf("normalization repaired malformed original snapshot: %v %v", family, err)
	}
	if !reflect.DeepEqual(before, snapshot.ConfigDependencies) {
		t.Fatal("failure mutated original annotations")
	}
}
