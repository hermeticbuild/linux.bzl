package kconfig

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestActionPlanCheckpointBackendReplaysCompleteLoweredPlan(t *testing.T) {
	metadata := familyVariantMetadataForTest(t, nil)
	options := compilerDefinednessTestOptions(t)
	scopes := compilerGuardBatchScopesForTest(t, options)
	if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
		t.Fatal(err)
	}
	plan, _, err := metadata.lowerSelectedActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	oldRoot := metadata.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	// Discover through the real compiler bridge. The carrier stores requests and
	// symbolic expressions, not their answers. Replay receives a separate oracle.
	if _, err := BuildActionPlanConfigDependencyAnalysis(plan); err != nil {
		t.Fatal(err)
	}
	probePlan := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	if len(probePlan.Nodes) == 0 {
		t.Fatal("lowered fixture registered no compiler queries")
	}
	oracle := successfulProbeOracleForFixedPointTest(t, probePlan)
	bindings := ActionPlanCheckpointBindings{Variant: "base", SourceArtifacts: map[string]string{"linux": oldRoot}, Toolsets: maps.Clone(plan.Toolsets), ConfigValues: maps.Clone(metadata.configFragment), ConfigFiles: familyTestConfig("1", "0"), ConfigSymbolUniverse: slices.Clone(metadata.configSymbolUniverse), ActionContracts: maps.Clone(metadata.actionContracts)}
	bindings.ActionRoles = slices.Clone(metadata.actionRoles)
	data, err := CaptureActionPlanCheckpoint(plan, bindings)
	if err != nil {
		t.Fatal(err)
	}
	compilerState, err := scopes.MarshalCompilerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range addressed.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
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
	cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, familyVariantTestHeader))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte("CONFIG_OBSERVED\n")))
	if err != nil {
		t.Fatal(err)
	}
	finish := func(current *ActionPlan) ([]byte, map[string]ConfigDependencySet, []ConfigDependencyObservedHeaderUse) {
		replay, err := cut.VerifyVariantReplay("base", snapshot, current, files)
		if err != nil {
			t.Fatal(err)
		}
		analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(current, nil, replay, observed)
		if err != nil {
			t.Fatal(err)
		}
		if err := contentAddressActionPlanNodes(current); err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(current.Nodes, func(a, b ActionPlanNode) int { return strings.Compare(a.ID, b.ID) })
		seal, err := replay.sealAnalysis(current, files)
		if err != nil {
			t.Fatal(err)
		}
		result := &ActionPlanFamilyVariantPlanningResult{Plan: current, analysis: analysis, cut: cut, observed: observed, variant: "base", replayConfigFiles: maps.Clone(files), replayContractDigest: seal}
		final, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(final.family.Nodes, func(node ActionPlanNode) bool { return node.Kind == "compile" }) {
			t.Fatal("fixture final family lost its compiler action")
		}
		payload, err := json.Marshal(final.family)
		if err != nil {
			t.Fatal(err)
		}
		deps, err := analysis.ByNodeID(current)
		if err != nil {
			t.Fatal(err)
		}
		uses, err := analysis.ObservedHeaderUsesByNodeID(current)
		if err != nil {
			t.Fatal(err)
		}
		return payload, deps, uses
	}
	// The reference path really re-evaluates Make in a new workload, as today's
	// production replay does. Do not attach answers to the discovery evaluator:
	// its process-local query cache correctly retains unready discovery entries.
	baseline, err := EvaluateKbuildProbeWorkload(options, oracle, func(fresh *KbuildProbeScopes) (*KbuildProbeScopes, error) { return fresh, nil })
	if err != nil {
		t.Fatal(err)
	}
	baselineMetadata := familyVariantMetadataForTest(t, nil)
	baselineMetadata.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = oldRoot
	if err := baseline.Value.BindActionPlanToolsetPathCapabilities(baselineMetadata); err != nil {
		t.Fatal(err)
	}
	baselinePlan, _, err := baselineMetadata.lowerSelectedActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, wantDependencies, wantUses := finish(baselinePlan)
	if err := oracle.ValidatePlan(compilerGuardBatchOrdinaryPlanForTest(t, baseline.Value)); err != nil {
		t.Fatal(err)
	}
	newRoot := t.TempDir()
	mustWriteSource(t, newRoot, "drivers/variant.c", "#include <variant.h>\nCONFIG_USED\n")
	mustWriteSource(t, oldRoot, "drivers/variant.c", "CONFIG_STALE_ROOT_MUST_NOT_BE_READ\n")
	currentBindings := bindings
	currentBindings.SourceArtifacts = map[string]string{"linux": newRoot}
	restored, err := RestoreActionPlanCheckpoint(data, currentBindings)
	if err != nil {
		t.Fatal(err)
	}
	var freshScopes *KbuildProbeScopes
	evaluation, err := EvaluateKbuildProbeWorkload(options, oracle, func(fresh *KbuildProbeScopes) (*KbuildProbeScopes, error) {
		if err := fresh.RestoreCompilerCheckpoint(compilerState, nil); err != nil {
			return nil, err
		}
		return fresh, fresh.BindActionPlanToolsetPathCapabilities(restored.metadata)
	})
	if err != nil {
		t.Fatal(err)
	}
	freshScopes = evaluation.Value
	// Preserve both the public request protocol and exact in-process state;
	// the registry intentionally distinguishes nil and empty request fields.
	gotProbePlan, err := json.Marshal(evaluation.Plan)
	if err != nil {
		t.Fatal(err)
	}
	wantProbePlan, err := json.Marshal(probePlan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotProbePlan, wantProbePlan) || !reflect.DeepEqual(evaluation.Plan, probePlan) {
		t.Fatalf("combined checkpoint changed ordinary compiler plan\ngot: %s\nwant: %s", gotProbePlan, wantProbePlan)
	}
	freshCompilerCalls := 0
	restored.metadata.compilerPredefines = func(scope, role, language string, args, units []string, environment map[string]string) (string, bool, error) {
		freshCompilerCalls++
		return freshScopes.CompilerPredefines(scope, role, language, args, units, environment)
	}
	if restored == plan || restored.metadata == metadata || restored.selectionGraph == plan.selectionGraph {
		t.Fatal("retained original planner state")
	}
	replayOptions := ActionPlanFamilyVariantPlanningOptions{Variant: "base", InitialSnapshot: &snapshot, Cut: cut, ObservedHeaders: observed, ResolvedConfigFiles: maps.Clone(files)}
	if _, err := ReplayActionPlanCheckpoint(restored, ActionPlanFamilyVariantPlanningOptions{Variant: "base"}); err == nil {
		t.Fatal("checkpoint bypassed complete original replay inputs")
	}
	unbound, err := RestoreActionPlanCheckpoint(data, currentBindings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayActionPlanCheckpoint(unbound, replayOptions); err == nil {
		t.Fatal("checkpoint replay supplied implicit compiler answers")
	}
	guardPlan, err := RestoreActionPlanCheckpoint(data, currentBindings)
	if err != nil {
		t.Fatal(err)
	}
	if err := freshScopes.BindActionPlanToolsetPathCapabilities(guardPlan.metadata); err != nil {
		t.Fatal(err)
	}
	if err := DiscoverActionPlanCheckpointCompilerGuards(guardPlan, replayOptions); err == nil {
		t.Fatal("checkpoint guard discovery omitted the current round callback")
	}
	guardOptions := replayOptions
	guardCallbacks := 0
	guardOptions.PrepareCompilerGuards = func() error { guardCallbacks++; return nil }
	if err := DiscoverActionPlanCheckpointCompilerGuards(guardPlan, guardOptions); err != nil {
		t.Fatal(err)
	}
	if guardCallbacks != 1 {
		t.Fatal("checkpoint guard callback did not run exactly once")
	}
	result, err := ReplayActionPlanCheckpoint(restored, replayOptions)
	if err != nil {
		t.Fatal(err)
	}
	final, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(final.family)
	if err != nil {
		t.Fatal(err)
	}
	gotDependencies, gotUses := result.Dependencies, result.ObservedHeaderUses
	if freshCompilerCalls == 0 {
		t.Fatal("checkpoint did not requery its current compiler callback")
	}
	if !bytes.Equal(got, want) || !reflect.DeepEqual(gotDependencies, wantDependencies) || !reflect.DeepEqual(gotUses, wantUses) {
		t.Fatalf("detached relocated replay changed complete family or analysis: family equal=%t\ngot deps=%#v\nwant deps=%#v\ngot uses=%#v\nwant uses=%#v", bytes.Equal(got, want), gotDependencies, wantDependencies, gotUses, wantUses)
	}
	precise := false
	for _, d := range gotDependencies {
		if !d.Opaque && slices.Contains(d.Symbols, "CONFIG_OBSERVED") && slices.Contains(d.Symbols, "CONFIG_USED") {
			precise = true
		}
	}
	if !precise || len(gotUses) == 0 {
		t.Fatal("fixture did not retain precise executed-header evidence")
	}
	for _, p := range restored.selectionGraph.profiles {
		if len(p.Rules) != 0 || len(p.targetEvaluators) != 0 || p.probeEnvironmentActivation != nil {
			t.Fatal("retained lazy Make state")
		}
	}
	t.Logf("detached complete plan: %d bytes, %d nodes, %d observed uses", len(data), len(restored.Nodes), len(gotUses))
}

func TestActionPlanCheckpointVirtualRootsNeedCurrentBindings(t *testing.T) {
	m := familyVariantMetadataForTest(t, nil)
	const virtual = "__VIRTUAL_DEPENDENCY_TREE__"
	m.Config.KbuildProfiles[0].evaluator.template.sourceRoots[virtual] = virtual
	plan, _, err := m.lowerSelectedActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := m.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	b := ActionPlanCheckpointBindings{Variant: "base", SourceArtifacts: map[string]string{"linux": root}, VirtualSourceRoots: map[string]string{virtual: virtual}, Toolsets: plan.Toolsets, ConfigValues: m.configFragment, ConfigFiles: familyTestConfig("1", "0"), ConfigSymbolUniverse: m.configSymbolUniverse, ActionContracts: m.actionContracts, ActionRoles: m.actionRoles}
	data, err := CaptureActionPlanCheckpoint(plan, b)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreActionPlanCheckpoint(data, b)
	if err != nil {
		t.Fatal(err)
	}
	if restored.metadata.Config.KbuildProfiles[0].evaluator.template.sourceRoots[virtual] != virtual {
		t.Fatal("lost inert virtual root")
	}
	b.VirtualSourceRoots = nil
	if _, err := RestoreActionPlanCheckpoint(data, b); err == nil {
		t.Fatal("checkpoint invented a virtual root missing from current invocation")
	}
	b.VirtualSourceRoots = map[string]string{virtual: root}
	if _, err := RestoreActionPlanCheckpoint(data, b); err == nil {
		t.Fatal("checkpoint virtual root acquired filesystem authority")
	}
}

func TestActionPlanCheckpointInitialCaptureHook(t *testing.T) {
	m := familyVariantMetadataForTest(t, nil)
	root := m.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	b := ActionPlanCheckpointBindings{Variant: "base", SourceArtifacts: map[string]string{"linux": root}, Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity}, ConfigValues: m.configFragment, ConfigFiles: familyTestConfig("1", "0"), ConfigSymbolUniverse: m.configSymbolUniverse, ActionContracts: m.actionContracts, ActionRoles: m.actionRoles}
	var captured []byte
	calls := 0
	result, err := m.ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, ActionPlanFamilyVariantPlanningOptions{Variant: "base", CaptureCheckpoint: func(plan *ActionPlan) error {
		calls++
		var err error
		captured, err = CaptureActionPlanCheckpoint(plan, b)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(captured) == 0 || result.Plan == nil {
		t.Fatal("initial checkpoint capture did not accompany ordinary final planning")
	}
	if _, err := RestoreActionPlanCheckpoint(captured, b); err != nil {
		t.Fatal(err)
	}
}
