package kconfig

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func prospectiveNumericDemandPlanForTest(t *testing.T, source string) (*ActionPlan, string, string) {
	t.Helper()
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", source)
	partRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{"unmodeled-numeric-generator"}, Stdout: "00000000",
		Outputs: []string{"00000000"},
	}
	partRecipeID, err := partRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	const partID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	part := ActionPlanNode{
		ID: partID, Stage: "prep", Kind: "generate", Tool: "scriptrun", Product: "vmlinux",
		Recipe: partRecipeID, Outputs: []ActionPlanOutput{{Tree: "prep", Path: ".numeric-parts/00000000"}},
	}
	plan.Recipes[partRecipeID] = partRecipe
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		if node.ID == producerID {
			node.Tool = "actionfile"
			node.Inputs = []ActionPlanNodeEdge{{Role: "part", ProducerID: partID, Slot: 0}}
			recipe = ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-input", "${input:part:00000000}", "-validate_config_independent_macro_header_v1", "-out", "${output:00000000}"},
				Inputs:    []string{"part:00000000"}, Outputs: []string{"00000000"},
			}
		} else if node.ID == consumerID {
			recipe.Arguments = append([]string{"-U__RESERVED"}, recipe.Arguments...)
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		node.Recipe, plan.Recipes[id] = id, recipe
	}
	plan.Nodes = append(plan.Nodes, part)
	plan.invalidateLookupIndexes()
	return plan, consumerID, producerID
}

const prospectiveNumericDemandSource = "#include <selected.h>\n__RESERVED\n#define PICK(x) CONFIG_ ## x\nPICK(ON)\n"

func TestConfigDependencyProspectiveNumericDemandDoesNotDependOnScannerProgress(t *testing.T) {
	for _, test := range []struct {
		name, source string
		opaque       bool
		demands      int
	}{
		{"failed_complete", prospectiveNumericDemandSource, true, 1},
		{"fast_precise", "#include <selected.h>\nCONFIG_DRIVER\n", false, 0},
		{"complete_precise", "#include <selected.h>\n#define PICK(x) CONFIG_ ## x\nPICK(ON)\n", false, 0},
		{"source_restores", "#include <selected.h>\n#undef __RESERVED\n__RESERVED\n#define PICK(x) CONFIG_ ## x\nPICK(ON)\n", false, 0},
		{"unopened_later", "__STOP\n#include <selected.h>\n#define PICK(x) CONFIG_ ## x\nPICK(ON)\n", true, 1},
		{"inactive", "#if 0\n#include <selected.h>\n#endif\n__STOP\n#define PICK(x) CONFIG_ ## x\nPICK(ON)\n", true, 1},
		{"fast_failure", "#include <unavailable.h>\n#include <selected.h>\n", true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, consumerID, producerID := prospectiveNumericDemandPlanForTest(t, test.source)
			_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
			if len(collection.Demands) != 0 || collection.Truncated || collection.prospectiveTruncated {
				t.Fatalf("numeric stage contaminated baseline: %#v", collection)
			}
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, nil)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || sets[consumerID].Opaque != test.opaque || len(collection.prospective) != test.demands {
				t.Fatalf("sets=%#v collection=%#v error=%v", sets, collection, err)
			}
			if test.demands != 0 {
				demand := collection.prospective[0]
				if demand.ConsumerNodeID != consumerID || demand.ProducerNodeID != producerID ||
					demand.Slot != 0 || demand.LogicalPath != "include/generated/selected.h" {
					t.Fatalf("prospective demand lost exact ownership: %#v", demand)
				}
				if test.name == "failed_complete" && !strings.Contains(sets[consumerID].Reason, "unknown macro binding __RESERVED") {
					t.Fatalf("fixture missed post-summary unknown: %s", sets[consumerID].Reason)
				}
			}
		})
	}
}

func TestConfigDependencyProspectiveNumericDemandIncludesBoundUnenteredForcedHeader(t *testing.T) {
	plan, consumerID, _ := prospectiveNumericDemandPlanForTest(t, "#define PICK(x) CONFIG_ ## x\nPICK(ON)\n")
	profile := plan.selectionGraph.profiles["config-dependency"]
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, "include/stop.h", "__STOP\n")
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.ID != consumerID {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		// This fixture originally declares only the generated prep tree. The
		// new forced source header must have a real declared source-tree binding.
		node.Trees = append(node.Trees, "kernel")
		recipe.Trees = append(recipe.Trees, "kernel")
		recipe.Arguments = append([]string{"-include", "${tree:kernel}/include/stop.h", "-include", "${tree:prep}/include/generated/selected.h"}, recipe.Arguments...)
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		node.Recipe, plan.Recipes[id] = id, recipe
	}
	analysis, collection := collectGeneratedHeaderDemandsForTest(t, plan)
	sets, err := analysis.ByNodeID(plan)
	if err != nil || !sets[consumerID].Opaque || !strings.Contains(sets[consumerID].Reason, "unknown macro binding __STOP") {
		t.Fatalf("fixture did not stop inside the first forced header: %#v, %v", sets[consumerID], err)
	}
	if len(collection.prospective) != 1 {
		t.Fatalf("opaque consumer lost its bound numeric scheduling candidate: %#v", collection)
	}
}

func TestConfigDependencyProspectiveNumericObservedBytesOverrideSummary(t *testing.T) {
	const source = "#include <selected.h>\n#define PICK(x) CONFIG_ ## x\n#if defined(__RESERVED)\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n"
	for _, content := range []string{"#define GENERATED_NUMBER 7\n", "#define __RESERVED 1\n"} {
		t.Run(strings.TrimSpace(content), func(t *testing.T) {
			plan, consumerID, producerID := prospectiveNumericDemandPlanForTest(t, source)
			_, demands := collectGeneratedHeaderDemandsForTest(t, plan)
			if len(demands.prospective) != 1 {
				t.Fatalf("initial numeric frontier=%#v", demands)
			}
			replay, observed := observedDependencyGateForTest(t, plan, producerID, content)
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			want := "CONFIG_OFF"
			if strings.Contains(content, "__RESERVED") {
				want = "CONFIG_ON"
			}
			if err != nil || sets[consumerID].Opaque || !slices.Equal(sets[consumerID].Symbols, []string{want}) {
				t.Fatalf("actual numeric bytes did not replace summary: %#v, %v", sets, err)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(uses) != 1 || uses[0].ProducerNodeID != producerID ||
				uses[0].ContentID != observed.Headers()[0].ContentID || uses[0].OriginalProducerNodeID == "" {
				t.Fatalf("precise use lacks executed-owner receipt: %#v, %v", uses, err)
			}
			next, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
			if err != nil || len(next.prospective) != 0 {
				t.Fatalf("observed exact bytes still demanded: %#v, %v", next, err)
			}
			// The same current plan without the immutable observation still sees
			// the wildcard. Another analysis cannot borrow these exact bytes.
			ordinary, err := BuildActionPlanConfigDependencyAnalysis(plan)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := ordinary.ByNodeID(plan)
			if err != nil || !plain[consumerID].Opaque {
				t.Fatalf("observed state leaked to ordinary analysis: %#v, %v", plain, err)
			}
		})
	}
}

func TestConfigDependencyProspectiveNumericScannerToInitialCutAndReplay(t *testing.T) {
	const source = "#include <selected.h>\n#define PICK(x) CONFIG_ ## x\n#if defined(__RESERVED)\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n"
	plan, consumerID, _ := prospectiveNumericDemandPlanForTest(t, source)
	analysis, collection := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(collection.Demands) != 0 || len(collection.prospective) != 1 {
		t.Fatalf("scanner did not produce a distinct numeric demand: %#v", collection)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		t.Fatal(err)
	}
	collection, err := analysis.GeneratedHeaderDemandsByNodeID(addressed)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(addressed)
	if err != nil {
		t.Fatal(err)
	}
	files := familyTestConfig("1", "0")
	snapshot, err := canonicalActionPlanSnapshot(addressed, sets, files)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := NewActionPlanFamilyInitialExecution([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}},
		map[string]ConfigDependencyGeneratedHeaderDemandCollection{"base": collection})
	if err != nil {
		t.Fatal(err)
	}
	want := ActionPlanFamilyExecutionCutRoot{NodeID: initial.Family.originalNodeIDs["base"][collection.prospective[0].ProducerNodeID], Slot: collection.prospective[0].Slot}
	if !slices.Equal(initial.Cut.Roots(), []ActionPlanFamilyExecutionCutRoot{want}) {
		t.Fatal("scanner-derived numeric producer did not become the exact initial root")
	}
	replay, err := initial.Cut.VerifyVariantReplay("base", snapshot, plan, files)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := initial.Cut.ObserveHeaders(observedHeadersTestStores(t, initial.Cut, []byte("#define GENERATED_NUMBER 7\n")))
	if err != nil {
		t.Fatal(err)
	}
	final, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	precise, err := final.ByNodeID(plan)
	if err != nil || precise[consumerID].Opaque || !slices.Equal(precise[consumerID].Symbols, []string{"CONFIG_OFF"}) {
		t.Fatalf("initial numeric demand failed exact observed replay: %#v %v", precise[consumerID], err)
	}
	uses, err := final.ObservedHeaderUsesByNodeID(plan)
	if err != nil || len(uses) != 1 || uses[0].ContentID != observed.Headers()[0].ContentID {
		t.Fatalf("scanner-to-cut replay lacks exact observation receipt: %#v %v", uses, err)
	}
}

func TestConfigDependencyProspectiveNumericDemandRevalidatesExactProjection(t *testing.T) {
	for _, mode := range []string{"valid", "wrong_identity", "wrong_owner", "wrong_slot", "not_numeric"} {
		t.Run(mode, func(t *testing.T) {
			plan, consumerID, _ := prospectiveNumericDemandPlanForTest(t, prospectiveNumericDemandSource)
			var consumer ActionPlanNode
			for _, node := range plan.Nodes {
				if node.ID == consumerID {
					consumer = node
				}
			}
			context := newConfigDependencyAnalysisContext(plan)
			context.prospectiveHeaderDemands = &configDependencyGeneratedHeaderDemandCollector{}
			selection := context.selectionsForNode(consumer)[0]
			profile := plan.selectionGraph.profiles[selection.profile]
			recipe := plan.Recipes[consumer.Recipe]
			generated := context.boundGeneratedText(plan, profile, consumer, recipe)
			const logical = "include/generated/selected.h"
			projection, found := generated[logical]
			if !found || !projection.macroTable {
				t.Fatal("fixture lacks owned virtual numeric projection")
			}
			switch mode {
			case "wrong_identity":
				projection.identity += "-forged"
			case "wrong_owner":
				projection.producerID = strings.Repeat("c", 64)
			case "wrong_slot":
				projection.slot = 1
			case "not_numeric":
				projection.macroTable = false
			}
			generated[logical] = projection
			context.recordProspectiveNumericHeaderDemands(plan, profile, selection, consumer, recipe, generated)
			want := 0
			if mode == "valid" {
				want = 1
			}
			if len(context.prospectiveHeaderDemands.records) != want {
				t.Fatalf("%s admitted %d demands, want %d", mode, len(context.prospectiveHeaderDemands.records), want)
			}
		})
	}
}

func TestConfigDependencyProspectiveNumericDemandAdmissionIsBaselineFirst(t *testing.T) {
	producer := ActionPlanNode{ID: "producer", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/value.h"}}}
	record := func(c *configDependencyGeneratedHeaderDemandCollector, id string) {
		c.record(ActionPlanNode{ID: id}, producer, 0, producer.Outputs[0].Path)
	}
	for _, mode := range []string{"fit", "exact_record_bound", "exact_byte_bound", "record_overflow", "byte_overflow", "stage_overflow", "overlap"} {
		t.Run(mode, func(t *testing.T) {
			baseline := &configDependencyGeneratedHeaderDemandCollector{maximumRecords: 3}
			stage := &configDependencyGeneratedHeaderDemandCollector{maximumRecords: 3}
			record(stage, "prospective-a")
			record(stage, "prospective-b")
			// The real baseline arrives after the complete optional stage.
			record(baseline, "baseline")
			if mode == "exact_record_bound" {
				baseline.maximumRecords = 3
			}
			if mode == "record_overflow" {
				baseline.maximumRecords = 2
			}
			if mode == "byte_overflow" {
				baseline.maximumBytes = baseline.bytes + stage.bytes - 1
			}
			if mode == "exact_byte_bound" {
				baseline.maximumBytes = baseline.bytes + stage.bytes
			}
			if mode == "stage_overflow" {
				record(stage, "prospective-c")
				record(stage, "prospective-d")
			}
			if mode == "overlap" {
				stage = &configDependencyGeneratedHeaderDemandCollector{}
				record(stage, "baseline")
				record(stage, "prospective-a")
				baseline.maximumRecords = 2
			}
			before, bytesBefore := maps.Clone(baseline.records), baseline.bytes
			stageBefore, stageBytesBefore := maps.Clone(stage.records), stage.bytes
			got := baseline.acceptsProspective(stage)
			want := mode == "fit" || mode == "exact_record_bound" || mode == "exact_byte_bound" || mode == "overlap"
			if got != want || baseline.truncated {
				t.Fatalf("admission=%t truncated=%t, want %t/false", got, baseline.truncated, want)
			}
			for demand, witness := range before {
				if got, exists := baseline.records[demand]; !exists || got != witness {
					t.Fatal("prospective admission lost baseline evidence")
				}
			}
			if !maps.Equal(baseline.records, before) || baseline.bytes != bytesBefore {
				t.Fatal("rejected optional stage partially changed baseline")
			}
			if !maps.Equal(stage.records, stageBefore) || stage.bytes != stageBytesBefore {
				t.Fatal("admission mutated staged evidence")
			}

		})
	}
}

func TestConfigDependencyProspectiveNumericDemandOrderAndCacheIsolation(t *testing.T) {
	producer := ActionPlanNode{ID: "producer", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/value.h"}}}
	var expected map[ConfigDependencyGeneratedHeaderDemand]configDependencyGeneratedHeaderDemandWitness
	for _, order := range [][]string{{"one", "two"}, {"two", "one"}} {
		baseline := &configDependencyGeneratedHeaderDemandCollector{maximumRecords: 3}
		stage := &configDependencyGeneratedHeaderDemandCollector{maximumRecords: 3}
		for _, id := range order {
			stage.record(ActionPlanNode{ID: id}, producer, 0, producer.Outputs[0].Path)
		}
		baseline.record(ActionPlanNode{ID: "baseline"}, producer, 0, producer.Outputs[0].Path)
		if !baseline.acceptsProspective(stage) {
			t.Fatal("bounded permutation rejected")
		}
		if expected != nil && !maps.Equal(expected, stage.records) {
			t.Fatal("traversal permutation changed admitted complete stage")
		}
		expected = maps.Clone(stage.records)
	}
	cache := NewActionPlanConfigDependencySharedCache()
	for index, source := range []string{prospectiveNumericDemandSource, "#include <selected.h>\nCONFIG_DRIVER\n", prospectiveNumericDemandSource} {
		plan, _, _ := prospectiveNumericDemandPlanForTest(t, source)
		analysis, err := BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, cache)
		if err != nil {
			t.Fatal(err)
		}
		demands, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
		want := 1
		if index == 1 {
			want = 0
		}
		if err != nil || len(demands.prospective) != want {
			t.Fatalf("analysis %d borrowed another plan's prospective state: %#v, %v", index, demands, err)
		}
	}
}

func TestConfigDependencyProspectiveNumericDemandRebindingAndIsolation(t *testing.T) {
	plan, _, _ := prospectiveNumericDemandPlanForTest(t, prospectiveNumericDemandSource)
	analysis, before := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(before.Demands) != 0 || len(before.prospective) != 1 {
		t.Fatalf("missing separate numeric tier: %#v", before)
	}
	slices.Reverse(plan.Nodes)
	sorted, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || !reflect.DeepEqual(before, sorted) {
		t.Fatalf("sorting changed private tier: %#v %v", sorted, err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	after, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || len(after.prospective) != 1 || len(after.Demands) != 0 {
		t.Fatalf("rebinding lost tier: %#v %v", after, err)
	}
	got := after.prospective[0]
	if got.ConsumerNodeID == before.prospective[0].ConsumerNodeID || got.ProducerNodeID == before.prospective[0].ProducerNodeID {
		t.Fatal("prospective tier retained provisional IDs")
	}
	after.prospective[0].Path = "mutated"
	again, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || again.prospective[0].Path != before.prospective[0].Path {
		t.Fatalf("private tier exposed mutable analysis storage: %v", err)
	}
	for _, node := range plan.Nodes {
		if node.ID == got.ProducerNodeID {
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			recipe.Arguments = []string{"changed-under-stable-id"}
			plan.Recipes[node.Recipe] = recipe
			break
		}
	}
	if _, err := analysis.GeneratedHeaderDemandsByNodeID(plan); err == nil {
		t.Fatal("prospective tier accepted changed original producer recipe")
	}
}

func TestNumericDemandRebindingRejectsChangedConfiguredHelper(t *testing.T) {
	plan, _, producerID := prospectiveNumericDemandPlanForTest(t, prospectiveNumericDemandSource)
	analysis, collection := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(collection.prospective) != 1 {
		t.Fatal("missing numeric candidate")
	}
	for _, node := range plan.Nodes {
		if node.ID != producerID {
			continue
		}
		ref := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: node.Tool}
		plan.metadata.actionRoles = append(plan.metadata.actionRoles, ref)
		plan.metadata.actionContracts[ref] = CompactKbuildActionContract{PrefixArguments: []string{"--wrapper"}}
	}
	if _, err := analysis.GeneratedHeaderDemandsByNodeID(plan); err == nil {
		t.Fatal("numeric scheduling evidence survived a changed configured wrapper")
	}
	_, fresh := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(fresh.prospective) != 0 {
		t.Fatal("configured wrapper generated an exact numeric candidate")
	}
}

func TestNumericDemandFanoutUsesFamilyBudgetWithoutExpandingBaseline(t *testing.T) {
	baseline := &configDependencyGeneratedHeaderDemandCollector{}
	optional := &configDependencyGeneratedHeaderDemandCollector{
		maximumRecords: MaxActionPlanFamilyExecutionCutRecords,
		maximumBytes:   MaxActionPlanFamilyExecutionCutBytes,
	}
	producer := ActionPlanNode{ID: "numeric", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/value.h"}}}
	for index := 0; index < 4893; index++ {
		consumer := ActionPlanNode{ID: fmt.Sprintf("consumer-%d", index)}
		optional.record(consumer, producer, 0, producer.Outputs[0].Path)
		baseline.record(consumer, producer, 0, producer.Outputs[0].Path)
	}
	if !baseline.truncated || optional.truncated || len(optional.records) != 4893 {
		t.Fatal("optional fanout changed baseline limits or inherited its small frontier cap")
	}
	baseline = &configDependencyGeneratedHeaderDemandCollector{}
	baseline.record(ActionPlanNode{ID: "baseline"}, producer, 0, producer.Outputs[0].Path)
	if !baseline.acceptsProspective(optional) || len(baseline.records) != 1 {
		t.Fatal("bounded numeric fanout displaced the baseline")
	}
}

func TestInitialExecutionNumericPrerequisiteRootAddsNoExecutionNodes(t *testing.T) {
	plan, consumerID, producerID := prospectiveNumericDemandPlanForTest(t, prospectiveNumericDemandSource)
	const wrapperPath = "include/generated/wrapper.h"
	wrapperRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-input", "${input:header:00000000}", "-out", "${output:00000000}"},
		Inputs:    []string{"header:00000000"}, Outputs: []string{"00000000"},
	}
	recipeID, err := wrapperRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	const wrapperID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	wrapper := ActionPlanNode{
		ID: wrapperID, Stage: "prep", Kind: "generate", Recipe: recipeID, Tool: "actionfile", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "header", ProducerID: producerID, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: wrapperPath}},
	}
	plan.Recipes[recipeID] = wrapperRecipe
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.ID != consumerID {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		binding := "generated:" + planOrdinal(len(node.Inputs))
		node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: "generated", ProducerID: wrapperID, Slot: 0})
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.WorkingInputs["input:"+binding] = wrapperPath
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		node.Recipe, plan.Recipes[id] = id, recipe
	}
	plan.Nodes = append(plan.Nodes, wrapper)
	snapshot := initialExecutionSnapshotFromPlanForTest(t, plan, familyTestConfig("1", "0"))
	family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	consumers := map[string]string{}
	for _, node := range family.Nodes {
		if node.Kind == "compile" {
			consumers["base-consumer"] = node.ID
		}
	}
	group := func(key, pathname string) *actionPlanFamilyHeaderDemandGroup {
		roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
		for _, root := range executionCutRootsForTest(family, pathname) {
			roots[root] = true
		}
		if len(roots) != 1 {
			t.Fatalf("missing unique root %s", pathname)
		}
		return &actionPlanFamilyHeaderDemandGroup{key: key, roots: roots, consumers: maps.Clone(consumers)}
	}
	primary := group("a-wrapper", wrapperPath)
	prospective := group("z-numeric", "include/generated/selected.h")
	image := &actionPlanFamilyHeaderDemandGroup{key: "a-full-image", roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: maps.Clone(consumers)}
	for _, id := range consumers {
		image.roots[ActionPlanFamilyExecutionCutRoot{NodeID: id, Slot: 0}] = true
	}
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{primary},
		defaultActionPlanFamilyExecutionCutLimits(), maxActionPlanFamilyHeaderExecutionAttempts, nil)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{image, prospective},
		defaultActionPlanFamilyExecutionCutLimits(), maxActionPlanFamilyHeaderExecutionAttempts, seed)
	if err != nil {
		t.Fatal(err)
	}
	before, after := seed.cut, extended.cut
	if seed.attempts != 1 || extended.attempts != 3 {
		t.Fatal("declined image group displaced the useful numeric trial")
	}
	if len(before.Roots()) != 1 || len(after.Roots()) != 2 || !slices.Equal(before.NodeIDs(), after.NodeIDs()) ||
		!reflect.DeepEqual(before.contract.Nodes, after.contract.Nodes) || before.ID() == after.ID() {
		t.Fatal("numeric prerequisite promotion changed execution closure or lost root identity")
	}
	if _, err := after.Verify(family); err != nil {
		t.Fatal(err)
	}
	oldHeaders, err := before.ObserveHeaders(observedHeadersTestStores(t, before, []byte("#define GENERATED_NUMBER 7\n")))
	if err != nil {
		t.Fatal(err)
	}
	newHeaders, err := after.ObserveHeaders(observedHeadersTestStores(t, after, []byte("#define GENERATED_NUMBER 7\n")))
	if err != nil || len(oldHeaders.Headers()) != 1 || len(newHeaders.Headers()) != 2 {
		t.Fatalf("explicit numeric root observation=%d/%d, %v", len(oldHeaders.Headers()), len(newHeaders.Headers()), err)
	}
}
