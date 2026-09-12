package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestExecutedArtifactsRequireSameHeaderObservation(t *testing.T) {
	testExecutedArtifactsRequireSameHeaderObservation(t, false)
}

func TestExecutedArtifactsRequireSameExpandedHeaderObservation(t *testing.T) {
	testExecutedArtifactsRequireSameHeaderObservation(t, true)
}

func testExecutedArtifactsRequireSameHeaderObservation(t *testing.T, expanded bool) {
	for _, change := range []string{"", "nil", "cut", "missing root", "duplicate root", "slot", "bytes", "content identity", "mode", "stored mode", "artifact owner", "artifact slot", "matching forged descriptors"} {
		t.Run(change, func(t *testing.T) {
			result := observedCASSealedResultForTest(t, "prep")
			artifacts, err := result.cut.ObserveArtifacts(observedHeadersTestStores(t, result.cut, []byte("CONFIG_OBSERVED\n")))
			if err != nil {
				t.Fatal(err)
			}
			headers := *result.observed
			if expanded {
				projected, err := artifacts.ObserveHeaders()
				if err != nil {
					t.Fatal(err)
				}
				headers = *projected
			}
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

func TestExecutedHeaderProjectionRejectsChangedArtifactCatalog(t *testing.T) {
	for _, change := range []string{"nil", "cut", "missing output", "descriptor", "content", "content identity", "mode"} {
		t.Run(change, func(t *testing.T) {
			result := observedCASSealedResultForTest(t, "prep")
			artifacts, err := result.cut.ObserveArtifacts(observedHeadersTestStores(t, result.cut, []byte("CONFIG_OBSERVED\n")))
			if err != nil {
				t.Fatal(err)
			}
			root := result.cut.Roots()[0]
			artifact := artifacts.outputs[root]
			switch change {
			case "nil":
				artifacts = nil
			case "cut":
				artifacts.cutID = strings.Repeat("f", 64)
			case "missing output":
				delete(artifacts.outputs, root)
			case "descriptor":
				artifact.output.Output.Path = "forged.h"
				artifacts.outputs[root] = artifact
			case "content":
				artifacts.contents[artifact.contentID] = []byte("changed")
			case "content identity":
				artifact.contentID = strings.Repeat("f", 64)
				artifacts.outputs[root] = artifact
			case "mode":
				artifacts.modes[artifact.contentID] ^= 0o111
			}
			if projected, err := artifacts.ObserveHeaders(); err == nil || projected != nil {
				t.Fatalf("accepted changed %s", change)
			}
		})
	}
}

func TestExecutedHeaderProjectionExcludesSidecarsAndOwnsPublicBytes(t *testing.T) {
	cut := observedHeadersTestCut(t, true)
	artifacts, err := cut.ObserveArtifacts(executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputPresent))
	if err != nil {
		t.Fatal(err)
	}
	headers, err := artifacts.ObserveHeaders()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers.Headers()) != len(cut.Roots()) || len(cut.Outputs()) <= len(headers.Headers()) {
		t.Fatal("sidecar fixture did not distinguish ordinary outputs")
	}
	for _, header := range headers.Headers() {
		if header.Output.ObservedPath != "" {
			t.Fatal("sidecar became a header")
		}
		content, ok := headers.Content(header.ContentID)
		if !ok || len(content) == 0 {
			t.Fatal("missing ordinary content")
		}
		content[0] ^= 1
	}
	if err := artifacts.verifyHeaders(headers); err != nil {
		t.Fatalf("public byte access mutated private observations: %v", err)
	}
}

func checkExpandedNonrootHeaderAuthentication(t *testing.T, artifacts *ActionPlanFamilyExecutedArtifacts, original *ActionPlanFamilyObservedHeaders) {
	t.Helper()
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, root := range artifacts.cut.Roots() {
		roots[root] = true
	}
	index := -1
	for ordinal, header := range original.headers {
		if !roots[ActionPlanFamilyExecutionCutRoot{NodeID: header.NodeID, Slot: header.Slot}] {
			index = ordinal
			break
		}
	}
	if index < 0 {
		t.Fatal("fixture has no additional nonroot observation")
	}
	for _, change := range []string{"missing", "duplicate", "descriptor", "bytes", "mode", "owner", "root-only scope"} {
		t.Run("nonroot-"+change, func(t *testing.T) {
			headers := *original
			headers.headers = slices.Clone(original.headers)
			headers.contents = maps.Clone(original.contents)
			header := &headers.headers[index]
			switch change {
			case "missing":
				headers.headers = slices.Delete(headers.headers, index, index+1)
			case "duplicate":
				headers.headers = append(headers.headers, *header)
			case "descriptor":
				header.Output.Path = "forged.h"
			case "bytes":
				headers.contents[header.ContentID] = []byte("forged content")
			case "mode":
				header.ExecutableMode ^= 0o111
			case "owner":
				header.NodeID = strings.Repeat("f", 64)
			case "root-only scope":
				headers.allExecutedOutputs = false
			}
			if err := artifacts.verifyHeaders(&headers); err == nil {
				t.Fatalf("accepted modified additional observation: %s", change)
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
