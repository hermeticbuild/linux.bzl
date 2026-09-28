package kconfig

import (
	"bytes"
	"encoding/base64"
	"maps"
	"os"
	"slices"
	"testing"
)

// This is an in-process integration fixture, not measured kernel reuse. Both
// header and executable observations come from the same immutable test stores.
func TestFamilyArtifactAndHeaderIntegration(t *testing.T) {
	testFamilyArtifactAndHeaderIntegration(t, false)
}

func TestFamilyArtifactAndTransitiveHeaderIntegration(t *testing.T) {
	testFamilyArtifactAndHeaderIntegration(t, true)
}

func testFamilyArtifactAndHeaderIntegration(t *testing.T, transitiveHeader bool) {
	for _, changed := range []string{"", "executable", "header", "mode"} {
		t.Run(changed, func(t *testing.T) {
			var plans []*ActionPlan
			var variants []ActionPlanFamilyVariant
			var originalRoots []string
			for index, name := range []string{"base", "other"} {
				plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\nint value = HEADER_VALUE;\n")
				producer := observedCASNodeForTest(t, plan, producerID)
				producer.Outputs = append(producer.Outputs, ActionPlanOutput{Tree: "prep", Path: "tools/helper"})
				producerRecipe := cloneActionRecipe(plan.Recipes[producer.Recipe])
				configSource, err := ensureActionPlanSource(plan, "config", "autoconf.h")
				if err != nil {
					t.Fatal(err)
				}
				producer.Sources = append(producer.Sources, ActionPlanSourceEdge{Role: "config", SourceID: configSource})
				configBinding := "config:" + planOrdinal(len(producer.Sources)-1)
				producerRecipe.Sources = append(producerRecipe.Sources, configBinding)
				if producerRecipe.WorkingInputs == nil {
					producerRecipe.WorkingInputs = map[string]string{}
				}
				producerRecipe.WorkingInputs["source:"+configBinding] = "include/generated/autoconf.h"
				producerRecipe.WorkingDirectory = "generator"
				producerRecipe.Stdout = ""
				producerRecipe.Arguments = append(producerRecipe.Arguments, "${output:00000000}", "${output:00000001}")
				producerRecipe.Outputs = append(producerRecipe.Outputs, "00000001")
				id, err := producerRecipe.ID()
				if err != nil {
					t.Fatal(err)
				}
				producer.Recipe, plan.Recipes[id] = id, producerRecipe
				consumer := observedCASNodeForTest(t, plan, consumerID)
				recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
				compilerArgs := slices.Clone(recipe.Arguments)
				binding := "helper:00000001"
				consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "helper", ProducerID: producerID, Slot: 1})
				recipe.Inputs = append(recipe.Inputs, binding)
				recipe.WorkingInputs["input:"+binding] = "tools/helper"
				recipe.WorkingInputs["source:source:00000000"] = "drivers/example/driver.c"
				recipe.WorkingOutputs = map[string]string{"00000000": consumer.Outputs[0].Path}
				recipe.ExecutableInputs = []string{binding}
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool: "cc", Arguments: compilerArgs, WorkingInputUsesComplete: true,
					WorkingInputUses:          []string{"input:" + recipe.Inputs[0], "input:" + binding, "source:source:00000000"},
					AuxiliaryWorkingInputUses: []string{"input:" + binding},
				}
				recipe.Tool = "scriptrun"
				recipe.Arguments = []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte("tools/helper\ncc -c drivers/example/driver.c -o driver.o\n"))}
				id, err = recipe.ID()
				if err != nil {
					t.Fatal(err)
				}
				consumer.Tool, consumer.Recipe, plan.Recipes[id] = recipe.Tool, id, recipe
				plan.Products = []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: LinuxKernelTreeRootMarker}}
				plan.invalidateLookupIndexes()
				if err := plan.exportReachableActionPlanInputSets(); err != nil {
					t.Fatal(err)
				}
				addressed := cloneActionPlan(plan)
				if err := contentAddressActionPlanNodes(addressed); err != nil {
					t.Fatal(err)
				}
				sets := map[string]ConfigDependencySet{}
				for ordinal, node := range addressed.Nodes {
					sets[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
					if plan.Nodes[ordinal].ID == producerID {
						originalRoots = append(originalRoots, node.ID)
					}
				}
				files := familyTestConfig("1", []string{"0", "1"}[index])
				snapshot, err := canonicalActionPlanSnapshot(addressed, sets, files)
				if err != nil {
					t.Fatal(err)
				}
				plans = append(plans, plan)
				variants = append(variants, ActionPlanFamilyVariant{Name: name, Snapshot: snapshot})
			}
			family, err := BuildConservativeActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			var roots []ActionPlanFamilyExecutionCutRoot
			for index, variant := range variants {
				slot := 0
				if transitiveHeader {
					// Executing the helper also executes its producer's header
					// output. The header itself is not an unresolved cut root.
					slot = 1
				}
				roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: family.originalNodeIDs[variant.Name][originalRoots[index]], Slot: slot})
			}
			cut, err := NewActionPlanFamilyExecutionCut(family, roots)
			if err != nil {
				t.Fatal(err)
			}
			stores := observedHeadersTestStores(t, cut, []byte("#define HEADER_VALUE 1\n"))
			otherProducer := family.originalNodeIDs["other"][originalRoots[1]]
			for _, output := range cut.Outputs() {
				filename := observedHeadersTestOutputPath(stores, output)
				if output.Slot == 1 {
					contents := "#!/bin/sh\nexit 0\n"
					if changed == "executable" && output.NodeID == otherProducer {
						contents = "#!/bin/sh\nexit 1\n"
					}
					if err := os.WriteFile(filename, []byte(contents), 0o755); err != nil {
						t.Fatal(err)
					}
					mode := os.FileMode(0o755)
					if changed == "mode" && output.NodeID == otherProducer {
						mode = 0o700
					}
					if err := os.Chmod(filename, mode); err != nil {
						t.Fatal(err)
					}
				} else if changed == "header" && output.NodeID == otherProducer {
					if err := os.WriteFile(filename, []byte("#define HEADER_VALUE 2\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			headers, err := cut.ObserveHeaders(stores)
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := observeExecutedArtifacts(cut, stores, 4096, 65536)
			if err != nil {
				t.Fatal(err)
			}
			if transitiveHeader {
				for _, header := range headers.Headers() {
					if header.Slot == 0 {
						t.Fatal("legacy root-only observation unexpectedly includes the transitive header")
					}
				}
				headers, err = artifacts.ObserveHeaders()
				if err != nil {
					t.Fatal(err)
				}
				if len(headers.Headers()) != len(cut.Outputs()) {
					t.Fatal("expanded observation omitted an ordinary executed output")
				}
				if changed == "" {
					checkExpandedNonrootHeaderAuthentication(t, artifacts, headers)
				}
			}
			var results []*ActionPlanFamilyVariantPlanningResult
			for index, plan := range plans {
				variant := variants[index]
				replay, err := cut.VerifyVariantReplay(variant.Name, variant.Snapshot, plan, variant.Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, headers)
				if err != nil {
					t.Fatal(err)
				}
				uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
				if err != nil || len(uses) != 1 {
					t.Fatalf("scanner header receipts: %v %v", uses, err)
				}
				if err := contentAddressActionPlanNodes(plan); err != nil {
					t.Fatal(err)
				}
				digest, err := replay.sealAnalysis(plan, variant.Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				results = append(results, &ActionPlanFamilyVariantPlanningResult{
					Plan: plan, analysis: analysis, observed: headers, cut: cut, variant: variant.Name,
					replayConfigFiles: maps.Clone(variant.Snapshot.ConfigFiles), replayContractDigest: digest,
				})
			}
			baseline, _, _, err := buildObservedActionPlanFamily(results)
			if err != nil {
				t.Fatal(err)
			}
			count := func(family *ActionPlanFamily) int {
				n := 0
				for _, node := range family.Nodes {
					if node.Kind == "compile" {
						n++
					}
				}
				return n
			}
			if count(baseline.family) != 2 {
				t.Fatalf("baseline compilers %d", count(baseline.family))
			}
			before := make([][]byte, len(plans))
			for index, plan := range plans {
				before[index], err = canonicalFamilyReplayPlan(plan, variants[index].Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
			}
			final, _, _, err := buildObservedActionPlanFamilyWithArtifacts(results, artifacts)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if changed == "" {
				want = 1
			}
			if got := count(final.family); got != want {
				t.Fatalf("integrated compilers %d, want %d", got, want)
			}
			if _, err := cut.Verify(final.family); err != nil {
				t.Fatal(err)
			}
			for index, plan := range plans {
				after, err := canonicalFamilyReplayPlan(plan, variants[index].Snapshot.ConfigFiles)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, before[index]) {
					t.Fatal("original sealed plan mutated")
				}
			}
			foundHeaders, foundArtifacts := false, false
			for _, source := range final.family.Sources {
				foundHeaders = foundHeaders || source.Namespace == LinuxKernelObservedHeaderSourceNamespace
				foundArtifacts = foundArtifacts || source.Namespace == "observed-artifacts"
			}
			if !foundHeaders || !foundArtifacts {
				t.Fatalf("missing separate source namespaces: headers=%t artifacts=%t", foundHeaders, foundArtifacts)
			}
		})
	}
}
