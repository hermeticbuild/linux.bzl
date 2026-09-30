package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// These exercise the private mechanical substitution boundary. Positive cases
// first obtain receipts through the real observation analysis; protected-binding
// cases subsequently vary one private recipe envelope to verify fail-closed
// behavior, not to assert that stale public replay contracts are acceptable.
func observedCASFixtureForTest(t *testing.T, mode, contents string) (*ActionPlan, string, string, map[string]ConfigDependencySet, []ConfigDependencyObservedHeaderUse, *ActionPlanFamilyObservedHeaders) {
	t.Helper()
	plan, consumer, producer := observedDependencyPlanForTest(t, mode, "#include <selected.h>\n")
	replay, observed := observedDependencyGateForTest(t, plan, producer, contents)
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(plan)
	if err != nil || sets[consumer].Opaque {
		t.Fatalf("precise fixture = %#v, %v", sets, err)
	}
	uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
	if err != nil || len(uses) != 1 {
		t.Fatalf("fixture receipts = %#v, %v", uses, err)
	}
	return plan, consumer, producer, sets, uses, observed
}

func observedCASNodeForTest(t *testing.T, plan *ActionPlan, id string) *ActionPlanNode {
	t.Helper()
	for index := range plan.Nodes {
		if plan.Nodes[index].ID == id {
			return &plan.Nodes[index]
		}
	}
	t.Fatalf("node %s absent", id)
	return nil
}

func TestObservedFamilyCASPreservesExactWorkAndTreeDestinations(t *testing.T) {
	for _, mode := range []string{"direct", "work", "tree"} {
		t.Run(mode, func(t *testing.T) {
			plan, consumerID, producerID, sets, uses, observed := observedCASFixtureForTest(t, mode, "CONFIG_OBSERVED\n")
			originalSources := slices.Clone(observedCASNodeForTest(t, plan, consumerID).Sources)
			if err := substituteFamilyObservedHeaderInputs(plan, sets, uses, observed); err != nil {
				t.Fatal(err)
			}
			if err := plan.ensureSourceLookupIndex(); err != nil {
				t.Fatalf("substitution broke ordinary source indexing: %v", err)
			}
			for _, source := range plan.Sources {
				if !validSourceID(source.ID) {
					t.Fatalf("pre-localization source has a nonordinal identity: %v", source)
				}
			}
			node := observedCASNodeForTest(t, plan, consumerID)
			if !reflect.DeepEqual(node.Sources, originalSources) {
				t.Fatal("ordinary source edges changed")
			}
			for _, edge := range node.Inputs {
				if edge.ProducerID == producerID && edge.Slot == uses[0].Slot {
					t.Fatal("replaceable direct producer retained")
				}
			}
			store, err := plan.planningActionPlanInputSetStore()
			if err != nil {
				t.Fatal(err)
			}
			var sourceID string
			for _, kind := range []ActionPlanInputSetTargetKind{ActionPlanInputSetWorkTarget, ActionPlanInputSetTreeTarget} {
				target := ActionPlanInputSetTarget{Kind: kind, Path: uses[0].LogicalPath}
				if kind == ActionPlanInputSetTreeTarget {
					target.Tree = uses[0].Tree
				}
				entry, found, err := store.Lookup(node.InputSet, target)
				if err != nil {
					t.Fatal(err)
				}
				want := mode != "tree" || kind == ActionPlanInputSetTreeTarget
				if found != want {
					t.Fatalf("%s target found %v, want %v", kind, found, want)
				}
				if !found {
					continue
				}
				if entry.SourceID == "" || entry.ProducerID != "" || entry.Slot != 0 || entry.Target != target {
					t.Fatalf("wrong source target: %#v", entry)
				}
				if sourceID != "" && sourceID != entry.SourceID {
					t.Fatal("work/tree copies did not share one immutable source")
				}
				sourceID = entry.SourceID
			}
			found := false
			for _, source := range plan.Sources {
				if source.ID == sourceID {
					found = source.Namespace == LinuxKernelObservedHeaderSourceNamespace && source.Path == "content/"+uses[0].ContentID
				}
			}
			if !found {
				t.Fatal("CAS descriptor not tied to authenticated content")
			}
		})
	}
}

func TestObservedFamilyCASRetainsProtectedInputs(t *testing.T) {
	for _, protection := range []string{"sequence", "argument", "executable", "auxiliary", "incomplete", "written", "alias", "persistent-auxiliary"} {
		t.Run(protection, func(t *testing.T) {
			plan, consumerID, _, sets, uses, observed := observedCASFixtureForTest(t, "direct", "CONFIG_OBSERVED\n")
			node := observedCASNodeForTest(t, plan, consumerID)
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			binding := recipe.Inputs[0]
			reference := "input:" + binding
			switch protection {
			case "sequence":
				node.Inputs[0].Role = "sequence"
				recipe.Inputs[0] = "sequence:00000000"
				recipe.WorkingInputs["input:sequence:00000000"] = recipe.WorkingInputs[reference]
				delete(recipe.WorkingInputs, reference)
			case "argument":
				recipe.Arguments = append(recipe.Arguments, "${input:"+binding+"}")
			case "executable":
				recipe.ExecutableInputs = append(recipe.ExecutableInputs, binding)
			case "auxiliary", "incomplete":
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool: "cc", Arguments: slices.Clone(recipe.Arguments),
					WorkingInputUsesComplete: protection != "incomplete",
					WorkingInputUses:         []string{reference}, AuxiliaryWorkingInputUses: []string{reference},
				}
			case "written":
				recipe.WorkingOutputs = map[string]string{"00000000": uses[0].LogicalPath}
			case "alias":
				recipe.WorkingInputs[reference] = "another/header.h"
			case "persistent-auxiliary":
				node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
					Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: uses[0].LogicalPath},
					ProducerID: uses[0].ProducerNodeID, Slot: uses[0].Slot, CompilerUse: true, AuxiliaryUse: true,
				})
			}
			plan.Recipes[node.Recipe] = recipe
			before := *node
			before.Inputs = slices.Clone(node.Inputs)
			if err := substituteFamilyObservedHeaderInputs(plan, sets, uses, observed); err != nil {
				t.Fatal(err)
			}
			node = observedCASNodeForTest(t, plan, consumerID)
			if !reflect.DeepEqual(node.Inputs, before.Inputs) || node.InputSet != before.InputSet {
				t.Fatalf("%s dependency changed: before %#v after %#v", protection, before, node)
			}
			for _, source := range plan.Sources {
				if source.Namespace == LinuxKernelObservedHeaderSourceNamespace {
					t.Fatal("protected use was replaced by CAS")
				}
			}
		})
	}
}

func TestObservedFamilyCASRetainsSidecarAndRemapsBase(t *testing.T) {
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	producer := observedCASNodeForTest(t, plan, producerID)
	producer.Outputs = append(producer.Outputs, ActionPlanOutput{Tree: "prep", Path: "generator.state", ObservedPath: ".state"})
	producerRecipe := cloneActionRecipe(plan.Recipes[producer.Recipe])
	delete(plan.Recipes, producer.Recipe)
	producerRecipe.Outputs = append(producerRecipe.Outputs, "00000001")
	producerRecipe.WorkingDirectory = "generator"
	producerRecipe.ObservedOutputs = map[string]string{"00000001": ".state"}
	id, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer.Recipe, plan.Recipes[id] = id, producerRecipe
	consumer := observedCASNodeForTest(t, plan, consumerID)
	consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "observed-state", ProducerID: producerID, Slot: 1})
	consumer.Outputs = append(consumer.Outputs, ActionPlanOutput{Tree: "metadata", Path: "consumer.state", ObservedPath: ".state"})
	recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
	delete(plan.Recipes, consumer.Recipe)
	recipe.Inputs = append(recipe.Inputs, "observed-state:00000001")
	recipe.Outputs = append(recipe.Outputs, "00000001")
	recipe.ObservedOutputs = map[string]string{"00000001": ".state"}
	recipe.ObservedOutputBases = map[string][]string{"00000001": {"observed-state:00000001"}}
	id, err = recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumer.Recipe, plan.Recipes[id] = id, recipe
	replay, observed := observedDependencyGateForTest(t, plan, producerID, "CONFIG_OBSERVED\n")
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(plan)
	if err != nil || sets[consumerID].Opaque {
		t.Fatalf("sidecar fixture = %#v, %v", sets, err)
	}
	uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
	if err != nil || len(uses) != 1 {
		t.Fatalf("sidecar uses = %#v, %v", uses, err)
	}
	if err := substituteFamilyObservedHeaderInputs(plan, sets, uses, observed); err != nil {
		t.Fatal(err)
	}
	consumer = observedCASNodeForTest(t, plan, consumerID)
	if len(consumer.Inputs) != 1 || consumer.Inputs[0].Role != "observed-state" || consumer.Inputs[0].Slot != 1 ||
		consumer.Inputs[0].ProducerID != producerID {
		t.Fatalf("sidecar authority lost: %#v", consumer.Inputs)
	}
	recipe = plan.Recipes[consumer.Recipe]
	if !slices.Equal(recipe.ObservedOutputBases["00000001"], []string{"observed-state:00000000"}) {
		t.Fatalf("sidecar base ordinal not rebound: %#v", recipe.ObservedOutputBases)
	}
}

func TestObservedFamilyCASContentDeterminesCompilerIdentity(t *testing.T) {
	var identities []string
	for _, contents := range []string{"CONFIG_OBSERVED\n", "CONFIG_OBSERVED\n", "CONFIG_DIFFERENT\n"} {
		plan, consumerID, _, sets, uses, observed := observedCASFixtureForTest(t, "direct", contents)
		if err := substituteFamilyObservedHeaderInputs(plan, sets, uses, observed); err != nil {
			t.Fatal(err)
		}
		ids, _, _, err := familyNodeIDs(plan, func(source ActionPlanSource) string {
			return semanticFamilySourceID(source.Namespace, source.Path)
		})
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, ids[consumerID])
	}
	if identities[0] != identities[1] || identities[1] == identities[2] {
		t.Fatalf("CAS sharing ignored content identity: %v", identities)
	}
}

func observedCASSealedResultForTest(t *testing.T, tree string) *ActionPlanFamilyVariantPlanningResult {
	t.Helper()
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	if tree == "objects" {
		producer := observedCASNodeForTest(t, plan, producerID)
		producer.Stage, producer.Outputs[0].Tree = "target", "objects"
		consumer := observedCASNodeForTest(t, plan, consumerID)
		consumer.Trees = []string{"objects"}
		recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
		delete(plan.Recipes, consumer.Recipe)
		recipe.Trees = []string{"objects"}
		for index, argument := range recipe.Arguments {
			recipe.Arguments[index] = strings.ReplaceAll(argument, "${tree:prep}", "${tree:objects}")
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		consumer.Recipe, plan.Recipes[id] = id, recipe
	}
	plan.Products = []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: LinuxKernelTreeRootMarker}}
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	originalRoot := ""
	for index, node := range addressed.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
		if plan.Nodes[index].ID == producerID {
			originalRoot = node.ID
		}
	}
	files := familyTestConfig("1", "0")
	snapshot, err := canonicalActionPlanSnapshot(addressed, dependencies, files)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{
		{NodeID: family.originalNodeIDs["base"][originalRoot], Slot: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := cut.VerifyVariantReplay("base", snapshot, plan, files)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte("CONFIG_OBSERVED\n")))
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(plan.Nodes, func(left, right ActionPlanNode) int { return strings.Compare(left.ID, right.ID) })
	return &ActionPlanFamilyVariantPlanningResult{
		Plan: plan, analysis: analysis, observed: observed, cut: cut, variant: "base",
		replayConfigFiles: files, replayContractDigest: replay.contractDigest,
	}
}

func TestObservedFamilyCASSealedWriterRetainsExecutedCut(t *testing.T) {
	for _, tree := range []string{"prep", "objects"} {
		t.Run(tree, func(t *testing.T) {
			result := observedCASSealedResultForTest(t, tree)
			artifacts, err := result.cut.ObserveArtifacts(observedHeadersTestStores(t, result.cut, []byte("CONFIG_OBSERVED\n")))
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			err = BuildAndWriteObservedActionPlanFamily(
				[]*ActionPlanFamilyVariantPlanningResult{result},
				actionPlanFamilySegmentOutputsForTest(directory),
				filepath.Join(directory, "reuse.json"), filepath.Join(directory, "pinned"), filepath.Join(directory, "headers"), filepath.Join(directory, "artifacts"), artifacts,
			)
			if err != nil {
				t.Fatalf("executed %s cut was not retained: %v", tree, err)
			}
			if _, err := os.Stat(filepath.Join(directory, "headers", "manifest.json")); err != nil {
				t.Fatal(err)
			}
			validated, cut, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cut.Verify(validated.family); err != nil {
				t.Fatal(err)
			}
			if tree == "prep" {
				for _, view := range validated.family.Views {
					if slices.Contains(cut.NodeIDs(), view.NodeID) {
						t.Fatal("execution retention created a fake public view")
					}
				}
				// Public graph/membership fields do not confer retention authority.
				unsealed := *validated.family
				unsealed.executionCut = nil
				if err := unsealed.validate(); err == nil {
					t.Fatal("public memberships retained otherwise unreachable execution")
				}
			}
		})
	}
}

func TestObservedFamilyCASPrepCutRetentionDoesNotSaltSharedCompiler(t *testing.T) {
	names := []string{"base", "other"}
	plans := make([]*ActionPlan, len(names))
	variants := make([]ActionPlanFamilyVariant, len(names))
	originalRoots := make([]string, len(names))
	for index, name := range names {
		plan, _, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
		producer := observedCASNodeForTest(t, plan, producerID)
		recipe := cloneActionRecipe(plan.Recipes[producer.Recipe])
		// Distinct real generator contracts can produce identical measured
		// header bytes. Retaining them must not reattach either to the compiler.
		recipe.Environment = map[string]string{"VARIANT_GENERATOR": name}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		producer.Recipe, plan.Recipes[id] = id, recipe
		plan.Products = []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: LinuxKernelTreeRootMarker}}
		addressed := cloneActionPlan(plan)
		if err := contentAddressActionPlanNodes(addressed); err != nil {
			t.Fatal(err)
		}
		dependencies := map[string]ConfigDependencySet{}
		for ordinal, node := range addressed.Nodes {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
			if plan.Nodes[ordinal].ID == producerID {
				originalRoots[index] = node.ID
			}
		}
		other := "0"
		if index != 0 {
			other = "1"
		}
		snapshot, err := canonicalActionPlanSnapshot(addressed, dependencies, familyTestConfig("1", other))
		if err != nil {
			t.Fatal(err)
		}
		plans[index], variants[index] = plan, ActionPlanFamilyVariant{Name: name, Snapshot: snapshot}
	}
	initial, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	var roots []ActionPlanFamilyExecutionCutRoot
	for index, name := range names {
		roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: initial.originalNodeIDs[name][originalRoots[index]], Slot: 0})
	}
	if roots[0].NodeID == roots[1].NodeID {
		t.Fatal("fixture did not create distinct executed generator contracts")
	}
	cut, err := NewActionPlanFamilyExecutionCut(initial, roots)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte("CONFIG_OBSERVED\n")))
	if err != nil {
		t.Fatal(err)
	}
	var results []*ActionPlanFamilyVariantPlanningResult
	var withoutRetention []actionPlanFamilyBuildVariant
	for index, variant := range variants {
		plan := plans[index]
		replay, err := cut.VerifyVariantReplay(variant.Name, variant.Snapshot, plan, variant.Snapshot.ConfigFiles)
		if err != nil {
			t.Fatal(err)
		}
		analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
		if err != nil {
			t.Fatal(err)
		}
		if err := contentAddressActionPlanNodes(plan); err != nil {
			t.Fatal(err)
		}
		results = append(results, &ActionPlanFamilyVariantPlanningResult{
			Plan: plan, analysis: analysis, observed: observed, cut: cut, variant: variant.Name,
			replayConfigFiles: variant.Snapshot.ConfigFiles, replayContractDigest: replay.contractDigest,
		})
		dependencies, err := analysis.ByNodeID(plan)
		if err != nil {
			t.Fatal(err)
		}
		uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
		if err != nil || len(uses) != 1 {
			t.Fatalf("fixture did not observe one precise header: %v, %v", uses, err)
		}
		snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, variant.Snapshot.ConfigFiles)
		if err != nil {
			t.Fatal(err)
		}
		withoutRetention = append(withoutRetention, actionPlanFamilyBuildVariant{
			variant: ActionPlanFamilyVariant{Name: variant.Name, Snapshot: snapshot}, snapshotValidated: true,
			observedHeaderUses: uses, observedHeaders: observed,
		})
	}
	validated, _, _, err := buildObservedActionPlanFamily(results)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cut.Verify(validated.family); err != nil {
		t.Fatal(err)
	}
	compilerIDs := func(family *ActionPlanFamily) []string {
		var ids []string
		for _, node := range family.Nodes {
			if node.Kind == "compile" {
				ids = append(ids, node.ID)
			}
		}
		return ids
	}
	ids := compilerIDs(validated.family)
	if len(ids) != 1 || !slices.Equal(validated.family.Memberships[ids[0]], names) {
		t.Fatalf("equal-content precise consumers did not share: %v", ids)
	}
	for _, origin := range cut.Origins() {
		if validated.family.originalNodeIDs[origin.Variant][origin.OriginalNodeID] != origin.NodeID {
			t.Fatal("retained cut lost an exact original binding")
		}
	}
	for _, view := range validated.family.Views {
		if slices.Contains(cut.NodeIDs(), view.NodeID) {
			t.Fatal("private prep execution was promoted to a public view")
		}
	}
	unretained, err := buildValidatedActionPlanFamily(withoutRetention)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, compilerIDs(unretained.family)) {
		t.Fatal("execution retention changed the compiler semantic identity")
	}
	if _, err := cut.Verify(unretained.family); err == nil {
		t.Fatal("fixture does not exercise cut nodes losing their sole data edges")
	}
}

func TestObservedFamilyCASSealedWriterRejectsForgedResultsAndIgnoresPublicEvidence(t *testing.T) {
	for _, change := range []string{"nil", "empty", "plan", "public-evidence"} {
		t.Run(change, func(t *testing.T) {
			options := familyVariantReplayOptionsForTest(t)
			result, err := familyVariantMetadataForTest(t, nil).ActionPlanFamilyVariant(
				actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, options)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "nil":
				result = nil
			case "empty":
				result = &ActionPlanFamilyVariantPlanningResult{Plan: result.Plan}
			case "plan":
				recipe := cloneActionRecipe(result.Plan.Recipes[result.Plan.Nodes[0].Recipe])
				recipe.Arguments = append(recipe.Arguments, "changed-after-replay")
				result.Plan.Recipes[result.Plan.Nodes[0].Recipe] = recipe
			case "public-evidence":
				result.Dependencies = map[string]ConfigDependencySet{"forged": {Opaque: true}}
				result.ObservedHeaderUses = []ConfigDependencyObservedHeaderUse{{
					ConfigDependencyGeneratedHeaderDemand: ConfigDependencyGeneratedHeaderDemand{ConsumerNodeID: "forged", ProducerNodeID: "forged"},
					ContentID:                             strings.Repeat("0", 64),
				}}
			}
			validated, cut, observed, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
			if change == "public-evidence" {
				if err != nil || validated == nil || cut == nil || observed == nil {
					t.Fatalf("public diagnostics replaced private authority: %v", err)
				}
			} else if err == nil || validated != nil || cut != nil || observed != nil {
				t.Fatalf("accepted forged %s result: %v", change, err)
			}
		})
	}
}
