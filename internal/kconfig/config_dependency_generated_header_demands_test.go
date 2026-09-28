package kconfig

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func generatedHeaderDemandPlanForTest(t *testing.T, mode string) (*ActionPlan, ActionPlanNode, string) {
	t.Helper()
	const pathname = "include/generated/selected.h"
	if mode == "tree" {
		plan, node, _, _, _ := configDependencyUnrecordedTreeGeneratedHeaderPlanForTest(t)
		recipe := plan.Recipes["tree-header-recipe"]
		recipe.Arguments = []string{"sh", "unmodeled-generator"}
		plan.Recipes["tree-header-recipe"] = recipe
		return plan, node, pathname
	}
	arguments := []string{"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c"}
	if mode == "forced" {
		arguments = append([]string{"-include", "${input:generated:00000000}"}, arguments...)
	}
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <selected.h>\n",
	}, arguments, nil)
	configDependencyStageGeneratedHeaderForTest(t, plan, &node, "selected-header", pathname, ActionRecipe{
		Tool: "scriptrun", Arguments: []string{"unmodeled-generator"},
	})
	if mode == "work" {
		node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
			ProducerID: "selected-header", Slot: 0,
		})
		node.Inputs = nil
		recipe := plan.Recipes[node.Recipe]
		recipe.Inputs, recipe.WorkingInputs = nil, nil
		plan.Recipes[node.Recipe] = recipe
		plan.Nodes[0] = node
	}
	return plan, node, pathname
}

func collectGeneratedHeaderDemandsForTest(t *testing.T, plan *ActionPlan) (*ActionPlanConfigDependencyAnalysis, ConfigDependencyGeneratedHeaderDemandCollection) {
	t.Helper()
	ordinary, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ordinary.ByNodeID(plan)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := ordinary.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || disabled.Enabled || len(disabled.Demands) != 0 {
		t.Fatalf("disabled collection = %#v, %v", disabled, err)
	}
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := analysis.ByNodeID(plan)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("annotations changed: got %#v, want %#v, error %v", got, want, err)
	}
	collection, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || !collection.Enabled || collection.Truncated {
		t.Fatalf("collection = %#v, %v", collection, err)
	}
	return analysis, collection
}

func TestConfigDependencyGeneratedHeaderDemandsExactBindings(t *testing.T) {
	for _, mode := range []string{"direct", "work", "tree", "forced"} {
		t.Run(mode, func(t *testing.T) {
			plan, node, pathname := generatedHeaderDemandPlanForTest(t, mode)
			analysis, collection := collectGeneratedHeaderDemandsForTest(t, plan)
			producerID := "selected-header"
			if mode == "tree" {
				producerID = "tree-header"
			}
			want := ConfigDependencyGeneratedHeaderDemand{
				ConsumerNodeID: node.ID, ProducerNodeID: producerID, Slot: 0,
				Tree: "prep", Path: pathname, ArtifactPath: pathname, LogicalPath: pathname,
			}
			if !reflect.DeepEqual(collection.Demands, []ConfigDependencyGeneratedHeaderDemand{want}) {
				t.Fatalf("demands = %#v, want %#v", collection.Demands, want)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || !sets[node.ID].Opaque {
				t.Fatalf("encountered unavailable output must remain opaque: %#v, %v", sets, err)
			}
		})
	}
}

func TestConfigDependencyGeneratedHeaderDemandsPreserveFirstAndNestedFrontier(t *testing.T) {
	for _, modeledFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable_first", true: "modeled_first"}[modeledFirst], func(t *testing.T) {
			source := "#include <first.h>\n#include <later.h>\n"
			if modeledFirst {
				source = "#include <first.h>\n"
			}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": source,
			}, []string{"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c"}, nil)
			for _, name := range []string{"first", "later", "nested"} {
				recipe := ActionRecipe{Tool: "scriptrun", Arguments: []string{"unmodeled-generator"}}
				if name == "first" && modeledFirst {
					recipe = ActionRecipe{Tool: "actionfile", Arguments: []string{
						"-out", "${output:00000000}", "-line", "#include <nested.h>",
					}}
				}
				configDependencyStageGeneratedHeaderForTest(t, plan, &node, name, "include/generated/"+name+".h", recipe)
			}
			_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
			want := "first"
			if modeledFirst {
				want = "nested"
			}
			if len(collection.Demands) != 1 || collection.Demands[0].ProducerNodeID != want {
				t.Fatalf("encountered frontier = %#v, want only %s", collection.Demands, want)
			}
		})
	}
}

func TestConfigDependencyGeneratedHeaderDemandsRejectObservedAndAmbiguousOwners(t *testing.T) {
	t.Run("observed", func(t *testing.T) {
		plan, _, _ := generatedHeaderDemandPlanForTest(t, "direct")
		plan.Nodes[1].Outputs[0].ObservedPath = "state/header.h"
		_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
		if len(collection.Demands) != 0 {
			t.Fatalf("observed output accepted as text: %#v", collection)
		}
	})
	t.Run("ambiguous_tree", func(t *testing.T) {
		plan, _, _ := generatedHeaderDemandPlanForTest(t, "tree")
		duplicate := plan.Nodes[1]
		duplicate.ID = "other-header"
		plan.Nodes = append(plan.Nodes, duplicate)
		_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
		if len(collection.Demands) != 0 {
			t.Fatalf("ambiguous tree owner accepted: %#v", collection)
		}
	})
	t.Run("source_wins", func(t *testing.T) {
		plan, node := configDependencyCompilePlanForTest(t, map[string]string{
			"drivers/example/driver.c": "#include <selected.h>\n",
			"include/selected.h":       "CONFIG_SOURCE_SELECTED\n",
		}, []string{"-nostdinc", "-I${tree:kernel}/include", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c"}, nil)
		configDependencyStageGeneratedHeaderForTest(t, plan, &node, "selected-header", "include/generated/selected.h", ActionRecipe{
			Tool: "scriptrun", Arguments: []string{"unmodeled-generator"},
		})
		_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
		if len(collection.Demands) != 0 {
			t.Fatalf("unselected object candidate demanded: %#v", collection)
		}
	})
}

func TestConfigDependencyGeneratedHeaderDemandsRebindSortAndContentAddress(t *testing.T) {
	for _, mode := range []string{"direct", "work"} {
		t.Run(mode, func(t *testing.T) {
			plan, _, _ := generatedHeaderDemandPlanForTest(t, mode)
			analysis, before := collectGeneratedHeaderDemandsForTest(t, plan)
			slices.Reverse(plan.Nodes)
			sorted, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
			if err != nil || !reflect.DeepEqual(sorted, before) {
				t.Fatalf("sorting changed collection: %#v, %v", sorted, err)
			}
			if err := contentAddressActionPlanNodes(plan); err != nil {
				t.Fatal(err)
			}
			slices.Reverse(plan.Nodes)
			after, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
			if err != nil || len(after.Demands) != 1 {
				t.Fatalf("rebound collection = %#v, %v", after, err)
			}
			got := after.Demands[0]
			if got.ConsumerNodeID == before.Demands[0].ConsumerNodeID || got.ProducerNodeID == before.Demands[0].ProducerNodeID {
				t.Fatalf("provisional IDs retained: %#v", got)
			}
			found := false
			for _, node := range plan.Nodes {
				if node.ID == got.ProducerNodeID {
					found = node.Outputs[got.Slot].Path == got.Path
				}
			}
			if !found {
				t.Fatalf("rebound producer output absent: %#v", got)
			}
			after.Demands[0].Path = "mutated"
			again, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
			if err != nil || again.Demands[0].Path != before.Demands[0].Path {
				t.Fatalf("caller mutation leaked: %#v, %v", again, err)
			}
		})
	}
}

func TestConfigDependencyGeneratedHeaderDemandsRejectStaleOrAmbiguousRebinding(t *testing.T) {
	t.Run("changed_recipe", func(t *testing.T) {
		plan, _, _ := generatedHeaderDemandPlanForTest(t, "direct")
		analysis, _ := collectGeneratedHeaderDemandsForTest(t, plan)
		recipe := plan.Recipes["selected-header-recipe"]
		recipe.Arguments = []string{"different"}
		plan.Recipes["selected-header-recipe"] = recipe
		if _, err := analysis.GeneratedHeaderDemandsByNodeID(plan); err == nil {
			t.Fatal("changed recipe beneath stable recipe ID accepted")
		}
	})
	t.Run("ambiguous_witness", func(t *testing.T) {
		plan, _, _ := generatedHeaderDemandPlanForTest(t, "direct")
		duplicate := plan.Nodes[1]
		duplicate.ID = "duplicate-header"
		plan.Nodes = append(plan.Nodes, duplicate)
		analysis, err := BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := analysis.GeneratedHeaderDemandsByNodeID(plan); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous distinct producer witness = %v", err)
		}
	})
}

func TestConfigDependencyGeneratedHeaderDemandsBudgetsDiscardWholeFrontier(t *testing.T) {
	consumer := ActionPlanNode{ID: "consumer"}
	first := ActionPlanNode{ID: "first", Outputs: []ActionPlanOutput{{Tree: "objects", Path: "first.h"}}}
	second := ActionPlanNode{ID: "second", Outputs: []ActionPlanOutput{{Tree: "objects", Path: "second.h"}}}
	for _, producers := range [][]ActionPlanNode{{first, second}, {second, first}} {
		collector := configDependencyGeneratedHeaderDemandCollector{maximumRecords: 1}
		for _, producer := range producers {
			collector.record(consumer, producer, 0, producer.Outputs[0].Path)
		}
		if !collector.truncated || len(collector.records) != 0 || collector.bytes != 0 {
			t.Fatalf("partial overflow frontier retained: %#v", collector)
		}
	}
	bytesOnly := configDependencyGeneratedHeaderDemandCollector{maximumRecords: 100, maximumBytes: 1}
	bytesOnly.record(consumer, first, 0, "first.h")
	if !bytesOnly.truncated || len(bytesOnly.records) != 0 {
		t.Fatalf("byte overflow accepted: %#v", bytesOnly)
	}
	dedup := configDependencyGeneratedHeaderDemandCollector{maximumRecords: 1}
	dedup.record(consumer, first, 0, "first.h")
	dedup.record(consumer, first, 0, "first.h")
	if dedup.truncated || len(dedup.records) != 1 {
		t.Fatalf("duplicate demand consumed budget: %#v", dedup)
	}
}

func TestConfigDependencyGeneratedHeaderDemandsDoNotLeakSpeculativeScannerState(t *testing.T) {
	scanner := configDependencyClosureScanner{
		generated:               map[string]bool{"header.h": true},
		collectGeneratedHeaders: true,
	}
	scratch := scanner.headerReplayScanner()
	if _, found := scratch.objectFile("header.h"); found || scratch.unavailableGeneratedHeader.logical != "header.h" {
		t.Fatalf("scratch did not reach unavailable frontier: %#v", scratch)
	}
	if scanner.opaqueReason != "" || scanner.unavailableGeneratedHeader.logical != "" {
		t.Fatalf("speculative cache replay leaked frontier: %#v", scanner)
	}
}

func TestConfigDependencyGeneratedHeaderDemandsPreserveSlotAndArtifact(t *testing.T) {
	plan, node, pathname := generatedHeaderDemandPlanForTest(t, "direct")
	producer := &plan.Nodes[1]
	output := producer.Outputs[0]
	output.ArtifactPath = ".linux-bzl-versions/selected/" + pathname
	producer.Outputs = []ActionPlanOutput{{Tree: "prep", Path: "other.h"}, output}
	node.Inputs[0].Slot = 1
	plan.Nodes[0] = node
	recipe := plan.Recipes[producer.Recipe]
	recipe.Outputs = []string{"00000000", "00000001"}
	plan.Recipes[producer.Recipe] = recipe
	_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(collection.Demands) != 1 || collection.Demands[0].Slot != 1 ||
		collection.Demands[0].Path != pathname || collection.Demands[0].ArtifactPath != output.ArtifactPath {
		t.Fatalf("exact slot/artifact was lost: %#v", collection)
	}
}

func TestConfigDependencyGeneratedHeaderDemandsPreserveEarlierConservativeStop(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <selected.h>\n",
	}, []string{"-nostdinc", "-I/uninspectable", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c"}, nil)
	configDependencyStageGeneratedHeaderForTest(t, plan, &node, "selected-header", "include/generated/selected.h", ActionRecipe{
		Tool: "scriptrun", Arguments: []string{"unmodeled-generator"},
	})
	_, collection := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(collection.Demands) != 0 {
		t.Fatalf("collector crossed an earlier external-root stop: %#v", collection)
	}
}

func TestConfigDependencyGeneratedHeaderDemandsPublishedTruncation(t *testing.T) {
	plan, _, _ := generatedHeaderDemandPlanForTest(t, "direct")
	analysis, err := buildActionPlanConfigDependencyAnalysis(plan, nil, &configDependencyGeneratedHeaderDemandCollector{maximumBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	collection, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || !collection.Enabled || !collection.Truncated || len(collection.Demands) != 0 {
		t.Fatalf("truncation must be visible and empty: %#v, %v", collection, err)
	}
}

func TestConfigDependencyGeneratedHeaderDemandsStableMultiConsumerOrder(t *testing.T) {
	plan, node, _ := generatedHeaderDemandPlanForTest(t, "direct")
	second := node
	second.ID = "another-consumer"
	second.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/other.o"}}
	plan.Nodes = append(plan.Nodes, second)
	selection := compactKbuildSelectionKey{profile: "config-dependency", target: second.Outputs[0].Path, stage: second.Stage}
	plan.selectionGraph.selections[selection] = CompactKbuildSelection{Profile: selection.profile, Target: selection.target, Stage: selection.stage}
	plan.selectionGraph.materializedProducers[selection] = second.ID
	analysis, before := collectGeneratedHeaderDemandsForTest(t, plan)
	if len(before.Demands) != 2 || before.Demands[0].ConsumerNodeID != second.ID {
		t.Fatalf("deterministic consumer ordering = %#v", before)
	}
	slices.Reverse(plan.Nodes)
	after, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("permuted demand result = %#v, %v", after, err)
	}
}
