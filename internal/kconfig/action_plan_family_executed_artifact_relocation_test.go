package kconfig

import (
	"bytes"
	"encoding/base64"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func reviewRelocatedArtifactVariant(t *testing.T) ActionPlanFamilyVariant {
	t.Helper()
	return reviewArtifactVariant(t, func(_ *ActionPlanNode, _ *ActionRecipe, producer *ActionPlanNode) {
		producer.Outputs[1].Path = path.Join(compactKbuildSideOutputStateDirectory, strings.Repeat("a", 64)+".state")
	})
}

func TestReviewExecutedArtifactCopyAllocationSeparatesProducts(t *testing.T) {
	variant := reviewRelocatedArtifactVariant(t)
	plan := snapshotActionPlan(variant.Snapshot)
	first := *reviewArtifactConsumer(t, plan)
	second := first
	second.Product = "modules"
	second.Outputs = slices.Clone(first.Outputs)
	second.Outputs[0].Path = "drivers/another.o"
	second.Outputs[1].Path = "another.state"
	recipe := cloneActionRecipe(plan.Recipes[second.Recipe])
	recipe.WorkingOutputs["00000000"] = "drivers/another.o"
	script, err := base64.StdEncoding.DecodeString(recipe.Arguments[1])
	if err != nil {
		t.Fatal(err)
	}
	recipe.Arguments[1] = base64.StdEncoding.EncodeToString(bytes.ReplaceAll(script, []byte("drivers/example.o"), []byte("drivers/another.o")))
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[recipeID], second.Recipe = recipe, recipeID
	second.ID = second.ContentID()
	before := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		before[node.ID] = variant.Snapshot.ConfigDependencies[node.ID]
	}
	before[second.ID] = variant.Snapshot.ConfigDependencies[first.ID]
	plan.Nodes = append(plan.Nodes, second)
	plan.Products = append(plan.Products, ActionPlanProduct{Name: "modules", Tree: "objects", Path: LinuxKernelTreeRootMarker})
	oldSets := make([]ConfigDependencySet, len(plan.Nodes))
	for index, node := range plan.Nodes {
		oldSets[index] = before[node.ID]
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sets := map[string]ConfigDependencySet{}
	for index, node := range plan.Nodes {
		sets[node.ID] = oldSets[index]
	}
	variant.Snapshot, err = canonicalActionPlanSnapshot(plan, sets, variant.Snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	fixture := reviewArtifactSetup(t, variant)
	updated, sets, err := substituteExecutedArtifacts(fixture.plan, variant.Snapshot.ConfigDependencies, fixture.replay, fixture.observed)
	if err != nil {
		t.Fatal(err)
	}
	physicalOwners := map[string]string{}
	contents := map[string]bool{}
	products := map[string]int{}
	for _, node := range updated.Nodes {
		if node.Kind != "metadata" || len(node.Sources) != 1 {
			continue
		}
		for _, source := range updated.Sources {
			if source.ID != node.Sources[0].SourceID || source.Namespace != "observed-artifacts" {
				continue
			}
			contents[source.Path] = true
			products[node.Product]++
			key := node.Outputs[0].Tree + "/" + actionPlanOutputArtifactPath(node.Outputs[0])
			if previous := physicalOwners[key]; previous != "" && previous != node.ID {
				t.Fatal("different copy contracts own one physical output")
			}
			physicalOwners[key] = node.ID
		}
	}
	if len(physicalOwners) != 4 || len(contents) != 2 || products["image"] != 2 || products["modules"] != 2 {
		t.Fatalf("copy allocations=%d byte sources=%d product counts=%v, want 4/2/two each", len(physicalOwners), len(contents), products)
	}
	reviewArtifactAssertEdges(t, fixture, updated, sets, [2]bool{false, false})
}

func reviewRelocatedArtifactKey(t *testing.T, fixture reviewArtifactFixture) ActionPlanFamilyExecutionCutRoot {
	t.Helper()
	for key, output := range fixture.observed.outputs {
		if output.output.Output.ObservedPath != "" {
			return key
		}
	}
	t.Fatal("missing observed sidecar")
	return ActionPlanFamilyExecutionCutRoot{}
}

func TestReviewExecutedArtifactRelocationRetainsWriterBytes(t *testing.T) {
	for _, disposition := range []toolaction.ObservedOutputDisposition{toolaction.ObservedOutputAbsent, toolaction.ObservedOutputPresent, toolaction.ObservedOutputDeleted} {
		t.Run(string(disposition), func(t *testing.T) {
			fixture := reviewArtifactSetup(t, reviewRelocatedArtifactVariant(t))
			observed, err := observeExecutedArtifacts(fixture.cut, executedArtifactStoresForTest(t, fixture.cut, disposition), 1024, 8192)
			if err != nil {
				t.Fatal(err)
			}
			fixture.observed = observed
			key := reviewRelocatedArtifactKey(t, fixture)
			artifact := observed.outputs[key]
			original := fixture.plan.Nodes[0]
			for _, node := range fixture.plan.Nodes {
				if node.ID == fixture.producerID {
					original = node
				}
			}
			if original.Outputs[key.Slot].Path == artifact.output.Output.Path ||
				!plannerOwnedObservedFamilyState(original.Outputs[key.Slot]) {
				t.Fatal("fixture did not exercise a real planner-owned path relocation")
			}
			before := bytes.Clone(observed.contents[artifact.contentID])
			state, err := toolaction.DecodeObservedOutputState(before)
			if err != nil {
				t.Fatal(err)
			}
			if disposition != toolaction.ObservedOutputAbsent && state.Writer != key.NodeID {
				t.Fatal("fixture has no exact original executed writer")
			}
			updated, sets, err := substituteExecutedArtifacts(fixture.plan, fixture.variant.Snapshot.ConfigDependencies, fixture.replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			reviewArtifactAssertEdges(t, fixture, updated, sets, [2]bool{false, false})
			if !bytes.Equal(before, observed.contents[artifact.contentID]) {
				t.Fatal("sidecar relocation rewrote writer or payload bytes")
			}
			consumer := reviewArtifactConsumer(t, updated)
			copyID := consumer.Inputs[1].ProducerID
			foundSource := false
			for _, node := range updated.Nodes {
				if node.ID != copyID {
					continue
				}
				for _, edge := range node.Sources {
					for _, source := range updated.Sources {
						foundSource = foundSource || source.ID == edge.SourceID &&
							source.Namespace == "observed-artifacts" && source.Path == "content/"+artifact.contentID
					}
				}
			}
			if !foundSource {
				t.Fatal("sidecar copy did not retain its complete envelope content identity")
			}
		})
	}
}

func TestReviewExecutedArtifactRelocationRejectsUnsealedDescriptors(t *testing.T) {
	for _, failure := range []string{"different family allocation", "original allocation", "different logical path", "different slot", "different node", "changed origin", "missing cut", "uninitialized cut"} {
		t.Run(failure, func(t *testing.T) {
			fixture := reviewArtifactSetup(t, reviewRelocatedArtifactVariant(t))
			copy := *fixture.observed
			copy.outputs = maps.Clone(copy.outputs)
			key := reviewRelocatedArtifactKey(t, fixture)
			artifact := copy.outputs[key]
			switch failure {
			case "different family allocation":
				artifact.output.Output.Path = familyOwnedObservedStatePath(strings.Repeat("b", 64), key.Slot)
			case "original allocation":
				artifact.output.Output.Path = path.Join(compactKbuildSideOutputStateDirectory, strings.Repeat("a", 64)+".state")
			case "different logical path":
				artifact.output.Output.ObservedPath = "other-side-effect"
			case "different slot":
				artifact.output.Slot = 0
			case "different node":
				artifact.output.NodeID = strings.Repeat("c", 64)
			case "changed origin":
				wrong := strings.Repeat("d", 64)
				fixture.replay.executedIDs[fixture.producerID] = wrong
				delete(copy.outputs, key)
				key.NodeID, artifact.output.NodeID = wrong, wrong
			case "missing cut":
				copy.cut = nil
			case "uninitialized cut":
				copy.cut = &ActionPlanFamilyExecutionCut{}
			}
			copy.outputs[key] = artifact
			updated, sets, err := substituteExecutedArtifacts(fixture.plan, fixture.variant.Snapshot.ConfigDependencies, fixture.replay, &copy)
			if err == nil || updated != nil || sets != nil {
				t.Fatalf("unsealed descriptor accepted: plan=%v sets=%v error=%v", updated != nil, sets != nil, err)
			}
		})
	}
}

// This shape helper is not authority; the substitution caller additionally
// requires the exact cut origin, full replay seal and exact sealed descriptor.
func TestReviewExecutedArtifactRelocationShapeIsNarrow(t *testing.T) {
	original := ActionPlanOutput{Tree: "prehost", Path: path.Join(compactKbuildSideOutputStateDirectory, strings.Repeat("a", 64)+".state"), ObservedPath: "side-effect"}
	executed := original
	executed.Path = familyOwnedObservedStatePath(strings.Repeat("b", 64), 2)
	if !executedArtifactOutputOwnership(original, executed, 2) {
		t.Fatal("recognized exact-slot family allocation rejected")
	}
	for _, failure := range []string{"ordinary output", "unknown original allocator", "unknown final allocator", "noncanonical final digest", "wrong slot", "extra path component", "logical path", "tree", "artifact alias"} {
		t.Run(failure, func(t *testing.T) {
			left, right := original, executed
			switch failure {
			case "ordinary output":
				left.ObservedPath, right.ObservedPath = "", ""
			case "unknown original allocator":
				left.Path = "state/original"
			case "unknown final allocator":
				right.Path = "state/final"
			case "noncanonical final digest":
				right.Path = familyOwnedObservedStatePath(strings.Repeat("B", 64), 2)
			case "wrong slot":
				right.Path = familyOwnedObservedStatePath(strings.Repeat("b", 64), 1)
			case "extra path component":
				right.Path = path.Join(right.Path, "extra")
			case "logical path":
				right.ObservedPath = "other-side-effect"
			case "tree":
				right.Tree = "host"
			case "artifact alias":
				right.ArtifactPath = "state/alias"
			}
			if executedArtifactOutputOwnership(left, right, 2) {
				t.Fatal("unrelated output accepted as a family state relocation")
			}
		})
	}
}
