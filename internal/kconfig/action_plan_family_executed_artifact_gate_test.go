package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestExecutedArtifactsRequireSameHeaderObservation(t *testing.T) {
	for _, change := range []string{"", "nil", "cut", "missing root", "duplicate root", "slot", "bytes", "content identity", "mode", "stored mode", "artifact owner", "artifact slot", "matching forged descriptors"} {
		t.Run(change, func(t *testing.T) {
			result := observedCASSealedResultForTest(t, "prep")
			artifacts, err := result.cut.ObserveArtifacts(observedHeadersTestStores(t, result.cut, []byte("CONFIG_OBSERVED\n")))
			if err != nil {
				t.Fatal(err)
			}
			headers := *result.observed
			headers.headers = slices.Clone(headers.headers)
			headers.contents = maps.Clone(headers.contents)
			headers.modes = maps.Clone(headers.modes)
			pointer := &headers
			root := artifacts.cut.Roots()[0]
			header := &headers.headers[0]
			switch change {
			case "nil":
				pointer = nil
			case "cut":
				headers.cutID = strings.Repeat("f", 64)
			case "missing root":
				headers.headers = nil
			case "duplicate root":
				headers.headers = append(headers.headers, *header)
			case "slot":
				header.Slot++
			case "bytes":
				headers.contents[header.ContentID] = []byte("other store")
			case "content identity":
				header.ContentID = strings.Repeat("f", 64)
			case "mode":
				header.ExecutableMode = 0o111
			case "stored mode":
				headers.modes[header.ContentID] = 0o111
			case "artifact owner":
				artifact := artifacts.outputs[root]
				artifact.output.NodeID = strings.Repeat("f", 64)
				artifacts.outputs[root] = artifact
			case "artifact slot":
				artifact := artifacts.outputs[root]
				artifact.output.Slot++
				artifacts.outputs[root] = artifact
			case "matching forged descriptors":
				header.Output.Path = "forged/header.h"
				artifact := artifacts.outputs[root]
				artifact.output.Output = header.Output
				artifacts.outputs[root] = artifact
			}
			err = artifacts.verifyHeaders(pointer)
			if change == "" && err != nil {
				t.Fatal(err)
			}
			if change != "" && err == nil {
				t.Fatalf("accepted changed %s", change)
			}
		})
	}
}

func TestExecutedArtifactRemapPreservesHeaderOwners(t *testing.T) {
	before := &ActionPlan{Nodes: []ActionPlanNode{{ID: "consumer"}, {ID: "producer"}}}
	uses := []ConfigDependencyObservedHeaderUse{{
		ConfigDependencyGeneratedHeaderDemand: ConfigDependencyGeneratedHeaderDemand{
			ConsumerNodeID: "consumer", ProducerNodeID: "producer", Slot: 2,
			Tree: "prep", Path: "header.h", LogicalPath: "include/header.h",
		},
		OriginalProducerNodeID: "original", ContentID: "content",
	}}
	for _, change := range []string{"", "missing node", "changed producer", "reordered nodes", "unknown consumer"} {
		t.Run(change, func(t *testing.T) {
			after := cloneActionPlan(before)
			after.Nodes[0].ID = "rekeyed-consumer"
			input := slices.Clone(uses)
			switch change {
			case "missing node":
				after.Nodes = after.Nodes[:1]
			case "changed producer":
				after.Nodes[1].ID = "different-producer"
			case "reordered nodes":
				slices.Reverse(after.Nodes)
			case "unknown consumer":
				input[0].ConsumerNodeID = "missing"
			}
			got, err := remapExecutedArtifactHeaderUses(before, after, input)
			if change != "" {
				if err == nil || got != nil {
					t.Fatalf("accepted %s", change)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(uses)
			want[0].ConsumerNodeID = "rekeyed-consumer"
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(input, uses) {
				t.Fatal("remap altered ownership or caller receipts")
			}
		})
	}
}

func TestObservedFamilyWithoutArtifactOptimizationIsUnchanged(t *testing.T) {
	result := observedCASSealedResultForTest(t, "prep")
	want, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
	if err != nil {
		t.Fatal(err)
	}
	got, _, _, err := buildObservedActionPlanFamilyWithArtifacts([]*ActionPlanFamilyVariantPlanningResult{result}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.family, want.family) {
		t.Fatal("disabled artifact optimization changed the family")
	}
}
