package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func assertFamilyExecutionCutSegmentContract(t *testing.T, family *ActionPlanFamily, cut *ActionPlanFamilyExecutionCut) map[string][]actionPlanEntry {
	t.Helper()
	before, err := json.Marshal(cut.contract)
	if err != nil {
		t.Fatal(err)
	}
	var originalNodes []ActionPlanNode
	nodes := map[string]ActionPlanNode{}
	for _, record := range cut.contract.Nodes {
		node := record.Node
		originalNodes = append(originalNodes, node)
		node.familySourceProjections = record.SourceProjections
		nodes[node.ID] = node
	}
	beforeNodes := cloneActionPlanNodes(originalNodes)
	entries, err := cut.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(linuxKernelFamilyPlanSegmentOrder) {
		t.Fatal("lost an execution segment")
	}
	complete, err := family.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	indexIDs := slices.Sorted(maps.Keys(nodes))
	store, err := NewActionPlanInputSetStoreFromNodes(cut.contract.InputSets)
	if err != nil {
		t.Fatal(err)
	}
	allSources := map[string]ActionPlanSource{}
	for _, source := range cut.contract.Sources {
		allSources[source.ID] = source
	}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		t.Run(segment, func(t *testing.T) {
			old := map[string][]byte{}
			for _, marker := range complete[segment] {
				old[marker.path] = marker.data
			}
			index := make([]string, len(indexIDs))
			local := map[string]bool{}
			prior := map[string]ActionPlanOutput{}
			sets := map[string]ActionPlanInputSetNode{}
			sources := map[string]ActionPlanSource{}
			packs := map[string]map[int]ActionPlanNodeEdge{}
			bindings := map[string]ActionPlanInputBindings{}
			projections := map[string]ActionPlanSourceProjections{}
			projectedOrdinals := map[string]map[int]string{}
			outputs := map[string]ActionPlanOutput{}
			validations := map[string]bool{}
			seenMarkers := map[string]bool{}
			for _, marker := range entries[segment] {
				if seenMarkers[marker.path] {
					t.Fatalf("duplicate marker %s", marker.path)
				}
				seenMarkers[marker.path] = true
				parts := strings.Split(marker.path, "/")
				if parts[0] == "index" {
					ordinal, err := strconv.Atoi(parts[1])
					if err != nil || len(parts[1]) != 8 || ordinal >= len(index) || index[ordinal] != "" {
						t.Fatalf("invalid cut index marker %s", marker.path)
					}
					index[ordinal] = parts[2]
				}
			}
			if !slices.Equal(index, indexIDs) {
				t.Fatalf("cut index=%v want=%v", index, indexIDs)
			}
			for _, marker := range entries[segment] {
				parts := strings.Split(marker.path, "/")
				switch parts[0] {
				case "prior":
					ordinal, err := strconv.Atoi(parts[1])
					if err != nil || ordinal >= len(index) {
						t.Fatal("invalid prior ordinal")
					}
					slot, err := strconv.Atoi(parts[2])
					if err != nil || len(parts[2]) != 8 {
						t.Fatal("invalid prior slot")
					}
					key := index[ordinal] + ":" + planOrdinal(slot)
					if _, duplicate := prior[key]; duplicate {
						t.Fatal("repeated prior output descriptor")
					}
					prior[key] = ActionPlanOutput{Tree: parts[3], Path: strings.Join(parts[5:], "/")}
				case "input-sets":
					if parts[2] == "manifest" {
						var node ActionPlanInputSetNode
						if err := json.Unmarshal(marker.data, &node); err != nil {
							t.Fatal(err)
						}
						sets[parts[1]] = node
					}
				case "sources":
					sources[parts[1]] = ActionPlanSource{ID: parts[1], Namespace: parts[2], Path: strings.Join(parts[3:], "/")}
				case "variants":
					if len(parts) != 6 || parts[2] != "validation" || parts[3] != "from" || segment != "target" {
						t.Fatalf("cut emitted final/public variant metadata: %s", marker.path)
					}
					validations[parts[1]+":"+parts[4]+":"+parts[5]] = true
				case "nodes":
					id := parts[2]
					node, ok := nodes[id]
					expectedSegment, _ := familyPlanSegmentForStage(node.Stage)
					if !ok || expectedSegment != segment {
						t.Fatal("unselected or wrong-segment node body")
					}
					local[id] = true
					if parts[3] == "in" && parts[4] == "node-pack" {
						role, _, tuples, err := decodeActionPlanPackedInputMarkerForTest(marker.path)
						if err != nil {
							t.Fatal(err)
						}
						if packs[id] == nil {
							packs[id] = map[int]ActionPlanNodeEdge{}
						}
						for _, tuple := range tuples {
							if tuple.producerOrdinal >= len(index) {
								t.Fatal("pack references outside cut index")
							}
							if _, duplicate := packs[id][tuple.inputOrdinal]; duplicate {
								t.Fatal("duplicate packed input")
							}
							packs[id][tuple.inputOrdinal] = ActionPlanNodeEdge{Role: role, ProducerID: index[tuple.producerOrdinal], Slot: tuple.slot}
						}
					}
					if parts[3] == "in" && parts[4] == "bindings" {
						var value ActionPlanInputBindings
						if err := json.Unmarshal(marker.data, &value); err != nil {
							t.Fatal(err)
						}
						bindings[id] = value
					}
					if parts[3] == "in" && parts[4] == "source-tree-bindings" {
						value, err := DecodeActionPlanSourceProjections(marker.data)
						if err != nil {
							t.Fatal(err)
						}
						projections[id] = value
					}
					if parts[3] == "in" && parts[4] == "source-tree-pack" {
						filename := parts[6]
						if len(filename) < 10 || filename[8] != '.' {
							t.Fatal("invalid source pack")
						}
						if projectedOrdinals[id] == nil {
							projectedOrdinals[id] = map[int]string{}
						}
						for _, token := range strings.Split(filename[9:], ",") {
							ordinal, err := strconv.ParseInt(token, 36, 64)
							if err != nil {
								t.Fatal(err)
							}
							if _, duplicate := projectedOrdinals[id][int(ordinal)]; duplicate {
								t.Fatal("repeated projected source")
							}
							projectedOrdinals[id][int(ordinal)] = parts[5]
						}
					}
					if parts[3] == "out" {
						key := id + ":" + parts[5]
						outputs[key] = ActionPlanOutput{Tree: parts[4], Path: strings.Join(parts[7:], "/")}
					}
				}
				// Everything except the intentionally local lexical ordinals
				// must remain byte-identical to complete-family publication.
				if parts[0] != "index" && parts[0] != "prior" && !strings.Contains(marker.path, "/in/node-pack/") {
					data, found := old[marker.path]
					if !found || !bytes.Equal(data, marker.data) {
						t.Fatalf("changed sealed marker %s", marker.path)
					}
				}
			}
			wantLocal, wantPrior, wantSources := map[string]bool{}, map[string]ActionPlanOutput{}, map[string]ActionPlanSource{}
			var roots []string
			expectPrior := func(producerID string, slot int) {
				producer, exists := nodes[producerID]
				if !exists || slot < 0 || slot >= len(producer.Outputs) {
					t.Fatal("sealed cut is not dependency closed")
				}
				ownerSegment, _ := familyPlanSegmentForStage(producer.Stage)
				if ownerSegment != segment {
					output := producer.Outputs[slot]
					wantPrior[producerID+":"+planOrdinal(slot)] = ActionPlanOutput{Tree: output.Tree, Path: actionPlanOutputArtifactPath(output)}
				}
			}
			wantOutputs := map[string]ActionPlanOutput{}
			for id, node := range nodes {
				ownerSegment, _ := familyPlanSegmentForStage(node.Stage)
				if ownerSegment != segment {
					continue
				}
				wantLocal[id] = true
				roots = append(roots, node.InputSet)
				for _, edge := range node.Sources {
					wantSources[edge.SourceID] = allSources[edge.SourceID]
				}
				if len(packs[id]) != len(node.Inputs) {
					t.Fatal("packed direct dependency count changed")
				}
				if len(bindings[id].Bindings) != len(node.Inputs) {
					t.Fatal("input binding manifest count changed")
				}
				for ordinal, edge := range node.Inputs {
					if packs[id][ordinal] != edge {
						t.Fatal("packed dependency changed producer/slot/role after index compaction")
					}
					expectPrior(edge.ProducerID, edge.Slot)
					output := nodes[edge.ProducerID].Outputs[edge.Slot]
					want := ActionPlanInputBinding{Tree: output.Tree, Path: path.Join("nodes", edge.ProducerID, planOrdinal(edge.Slot)), ProjectionTree: output.Tree, ProjectionPath: actionPlanOutputArtifactPath(output)}
					if bindings[id].Bindings[edge.Role+":"+planOrdinal(ordinal)] != want {
						t.Fatal("input manifest changed physical or logical binding")
					}
				}
				for slot, output := range node.Outputs {
					wantOutputs[id+":"+planOrdinal(slot)] = ActionPlanOutput{Tree: output.Tree, Path: actionPlanOutputArtifactPath(output)}
				}
				wantProjected := map[int]string{}
				if len(node.familySourceProjections) != 0 {
					expected, err := familyNodeSourceProjections(node)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(expected, projections[id]) {
						t.Fatal("source projection manifest changed")
					}
					for _, projection := range node.familySourceProjections {
						wantProjected[projection.SourceOrdinal] = projection.Tree
					}
				} else if _, found := projections[id]; found {
					t.Fatal("fabricated source projection")
				}
				if !maps.Equal(wantProjected, projectedOrdinals[id]) {
					t.Fatal("source projection packs differ from exact source ordinals")
				}
			}
			closure, err := store.ReachableNodesForRoots(roots)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(closure, sets) {
				t.Fatal("persistent set manifests do not equal exact segment closure")
			}
			for _, set := range closure {
				for _, entry := range set.Entries {
					if entry.ProducerID != "" {
						expectPrior(entry.ProducerID, entry.Slot)
					} else {
						wantSources[entry.SourceID] = allSources[entry.SourceID]
					}
				}
			}
			wantValidations := map[string]bool{}
			if segment == "target" {
				for _, validation := range cut.contract.Validations {
					wantValidations[validation.Variant+":"+validation.NodeID+":"+planOrdinal(validation.Slot)] = true
					expectPrior(validation.NodeID, validation.Slot)
				}
			}
			if !maps.Equal(local, wantLocal) || !maps.Equal(prior, wantPrior) || !maps.Equal(outputs, wantOutputs) ||
				!maps.Equal(sources, wantSources) || !maps.Equal(validations, wantValidations) {
				t.Fatalf("segment lost/added graph transport: local=%v prior=%v sources=%v validations=%v", maps.Equal(local, wantLocal), maps.Equal(prior, wantPrior), maps.Equal(sources, wantSources), maps.Equal(validations, wantValidations))
			}
		})
	}
	after, err := json.Marshal(cut.contract)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("emission mutated private cut contract")
	}
	var afterNodes []ActionPlanNode
	for _, record := range cut.contract.Nodes {
		afterNodes = append(afterNodes, record.Node)
	}
	if !reflect.DeepEqual(beforeNodes, afterNodes) {
		t.Fatal("emission mutated private node slices/flags")
	}
	again, err := cut.segmentEntries()
	if err != nil || !reflect.DeepEqual(entries, again) {
		t.Fatal("repeated cut emission changed markers")
	}
	// Caller-owned marker payloads cannot modify the cut or later publications.
	for segment := range entries {
		for index := range entries[segment] {
			if len(entries[segment][index].data) != 0 {
				entries[segment][index].data[0] ^= 1
			}
			entries[segment][index].path = "caller-modified"
		}
	}
	last, err := cut.segmentEntries()
	if err != nil || !reflect.DeepEqual(again, last) {
		t.Fatal("returned markers alias sealed emission state")
	}
	if _, err := cut.Verify(family); err != nil {
		t.Fatal(err)
	}
	return last
}

func TestActionPlanFamilyExecutionCutSegmentsNilOrUninitialized(t *testing.T) {
	for _, cut := range []*ActionPlanFamilyExecutionCut{nil, {}} {
		if markers, err := cut.segmentEntries(); err == nil || markers != nil {
			t.Fatal("uninitialized cut emitted markers")
		}
	}
}

func TestActionPlanFamilyExecutionCutSegmentsCrossStageOrdinalsAndPriorDescriptors(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestSegmentChainSnapshot(t)))
	for _, root := range []string{"generated/host", "drivers/final.o"} {
		t.Run(root, func(t *testing.T) {
			cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, root))
			if err != nil {
				t.Fatal(err)
			}
			if root == "generated/host" && len(cut.NodeIDs()) >= len(family.Nodes) {
				t.Fatal("subset fixture does not compact the index")
			}
			entries := assertFamilyExecutionCutSegmentContract(t, family, cut)
			count := 0
			for _, markers := range entries {
				for _, marker := range markers {
					if strings.HasPrefix(marker.path, "prior/") {
						count++
					}
				}
			}
			want := 2
			if root == "drivers/final.o" {
				want = 3
			}
			if count != want {
				t.Fatalf("cross-segment descriptors=%d want%d", count, want)
			}
		})
	}
}

func TestActionPlanFamilyExecutionCutSegmentsPersistentSourcesAndPriorEdges(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestInputSetSnapshot(t, 1)))
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, "drivers/final.o"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.contract.InputSets) == 0 {
		t.Fatal("fixture has no persistent dependency closure")
	}
	assertFamilyExecutionCutSegmentContract(t, family, cut)
}

func TestActionPlanFamilyExecutionCutSegmentsKeepCompleteSourceProjectionPacks(t *testing.T) {
	var headers []string
	for index := range 130 {
		headers = append(headers, fmt.Sprintf("include/%03d.h", index))
	}
	family := executionCutFamilyForTest(t, familyTestKernelSourceTreeSnapshot(t, headers...))
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, "drivers/example.o"))
	if err != nil {
		t.Fatal(err)
	}
	projected := 0
	for _, record := range cut.contract.Nodes {
		projected += len(record.SourceProjections)
	}
	if projected != 131 {
		t.Fatalf("fixture projections=%d want131", projected)
	}
	assertFamilyExecutionCutSegmentContract(t, family, cut)
}

func TestActionPlanFamilyExecutionCutSegmentsKeepActivatedComparison(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestProjectedGeneratorWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))))
	if len(family.Validations) != 1 {
		t.Fatal("fixture has no exact comparison")
	}
	var validator ActionPlanNode
	for _, node := range family.Nodes {
		if node.ID == family.Validations[0].NodeID {
			validator = node
		}
	}
	for _, input := range validator.Inputs {
		t.Run(input.Role, func(t *testing.T) {
			cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: input.ProducerID, Slot: input.Slot}})
			if err != nil {
				t.Fatal(err)
			}
			if len(cut.contract.Validations) != 1 {
				t.Fatal("fixture did not activate comparison")
			}
			assertFamilyExecutionCutSegmentContract(t, family, cut)
		})
	}
}

func TestActionPlanFamilyExecutionCutWriteSegments(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestSegmentChainSnapshot(t)))
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			var roots []ActionPlanFamilyExecutionCutRoot
			if !empty {
				roots = executionCutRootsForTest(family, "generated/host")
			}
			cut, err := NewActionPlanFamilyExecutionCut(family, roots)
			if err != nil {
				t.Fatal(err)
			}
			want, err := cut.segmentEntries()
			if err != nil {
				t.Fatal(err)
			}
			outputs := actionPlanFamilySegmentOutputsForTest(t.TempDir())
			if err := cut.WriteSegments(outputs); err != nil {
				t.Fatal(err)
			}
			for segment, entries := range want {
				for _, entry := range entries {
					data, err := os.ReadFile(filepath.Join(outputs[segment], filepath.FromSlash(entry.path)))
					if err != nil || !bytes.Equal(data, entry.data) {
						t.Fatalf("published different cut marker %s/%s", segment, entry.path)
					}
				}
			}
			if err := cut.WriteSegments(outputs); err == nil {
				t.Fatal("overwrote nonempty cut output")
			}
			if _, err := cut.Verify(family); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActionPlanFamilyExecutionCutWriteSegmentsRejectsInvalidOutputs(t *testing.T) {
	family := executionCutFamilyForTest(t, executionCutOpaqueSnapshotForTest(familyTestSegmentChainSnapshot(t)))
	cut, err := NewActionPlanFamilyExecutionCut(family, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"missing", "unknown", "alias", "blank", "nil cut"} {
		t.Run(failure, func(t *testing.T) {
			outputs := actionPlanFamilySegmentOutputsForTest(t.TempDir())
			current := cut
			switch failure {
			case "missing":
				delete(outputs, "host")
			case "unknown":
				outputs["later"] = outputs["host"]
				delete(outputs, "host")
			case "alias":
				outputs["host"] = outputs["bootstrap"]
			case "blank":
				outputs["host"] = ""
			case "nil cut":
				current = nil
			}
			if err := current.WriteSegments(outputs); err == nil {
				t.Fatal("invalid cut output accepted")
			}
		})
	}
}
