package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func counterGuardSnapshotForTest(t *testing.T, scopes *KbuildProbeScopes, args []string, count int, output string) (*KbuildCompilerGuardAnswers, *ProbePlan) {
	t.Helper()
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := discovery.CompilerCounterSequence("target", "cc", "c", args, nil, count, nil); err != nil || ready {
		t.Fatalf("discovery = %t, %v", ready, err)
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if answers, err := discovery.Answers(); err == nil || answers != nil {
		t.Fatal("discovery published answers")
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{compilerCounterSequenceStep: output})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := replay.CompilerCounterSequence("target", "cc", "c", args, nil, count, nil); err != nil || !ready {
		t.Fatalf("replay = %t, %v", ready, err)
	}
	answers, err := replay.Answers()
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := replay.CompilerCounterSequence("target", "cc", "c", args, nil, count, nil); err == nil || ready {
		t.Fatal("frozen batch accepted another query")
	}
	return answers, plan
}

func TestCompilerCounterGuardAnswersKeepContextAndBindingSeparate(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	args := []string{"-DOTHER=73"}
	answers, probes := counterGuardSnapshotForTest(t, scopes, args, 3, "7 42 9")
	plan := &ActionPlan{Toolsets: maps.Clone(probes.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
	sequence, identity, err := configDependencySupplementalCompilerCounter(plan, "target", "cc", "c", args, nil, nil)
	if err != nil || sequence == nil || identity == "" || !slices.Equal(sequence.values, []string{"7", "42", "9"}) {
		t.Fatalf("counter snapshot = %#v, %q, %v", sequence, identity, err)
	}
	if definedness, id, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", args, nil, nil); err != nil || definedness != nil || id != "" {
		t.Fatal("counter vector promoted to initial definedness")
	}
	for _, change := range []string{"scope", "role", "language", "arguments", "units", "environment"} {
		t.Run(change, func(t *testing.T) {
			scope, role, language, argv := "target", "cc", "c", args
			var units []string
			var environment map[string]string
			switch change {
			case "scope":
				scope = "host"
			case "role":
				role = "cxx"
			case "language":
				language = "c++"
			case "arguments":
				argv = []string{"-DOTHER=74"}
			case "units":
				units = []string{"source.c"}
			case "environment":
				environment = map[string]string{"MODE": "different"}
			}
			if value, id, err := configDependencySupplementalCompilerCounter(plan, scope, role, language, argv, units, environment); err != nil || value != nil || id != "" {
				t.Fatal("different context borrowed a measured vector")
			}
		})
	}
	plan.Toolsets["target"] = "sha256-" + strings.Repeat("f", 64)
	if value, id, err := configDependencySupplementalCompilerCounter(plan, "target", "cc", "c", args, nil, nil); err == nil || value != nil || id != "" {
		t.Fatal("different toolset borrowed a measured vector")
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("detached query changed ordinary planner state")
	}
}

func TestCompilerCounterGuardMergePreservesSequenceAndConflicts(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	small, _ := counterGuardSnapshotForTest(t, scopes, nil, 2, "7 42")
	large, _ := counterGuardSnapshotForTest(t, scopes, nil, 3, "7 42 9")
	conflict, _ := counterGuardSnapshotForTest(t, scopes, nil, 3, "7 43 9")
	key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	if equalKbuildCompilerCounterAnswers(small.counters, large.counters) {
		t.Fatal("different measured lengths compared equal")
	}
	merged, err := MergeKbuildCompilerGuardAnswers(small, large)
	if err != nil || !slices.Equal(merged.counters[key].sequence.values, []string{"7", "42", "9"}) {
		t.Fatalf("merge lost longest authenticated prefix: %#v, %v", merged, err)
	}
	permuted, err := MergeKbuildCompilerGuardAnswers(large, nil, small, large)
	if err != nil || !equalKbuildCompilerCounterAnswers(merged.counters, permuted.counters) {
		t.Fatal("round order or duplicate changed merged vector identity")
	}
	if got, err := MergeKbuildCompilerGuardAnswers(large, large); err != nil || got != large {
		t.Fatal("identical immutable round lost snapshot identity")
	}
	for _, snapshots := range [][]*KbuildCompilerGuardAnswers{{small, conflict}, {conflict, large}, {large, small, conflict}} {
		if got, err := MergeKbuildCompilerGuardAnswers(snapshots...); err == nil || got != nil {
			t.Fatal("conflicting vector published a merged prefix")
		}
	}
	if len(small.counters[key].sequence.values) != 2 || small.counters[key].sequence.values[1] != "42" {
		t.Fatal("merge mutated original shorter snapshot")
	}
}

func TestCompilerCounterGuardFailedReplayCannotPublishEarlierFacts(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := discovery.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__COUNTER__"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := discovery.CompilerCounterSequence("target", "cc", "c", nil, nil, 3, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-definedness": "1", compilerCounterSequenceStep: "7 42"})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := replay.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__COUNTER__"}, nil); err != nil || !ready {
		t.Fatalf("definedness: %t, %v", ready, err)
	}
	if ready, err := replay.CompilerCounterSequence("target", "cc", "c", nil, nil, 3, nil); err == nil || ready {
		t.Fatal("truncated sequence accepted")
	}
	if answers, err := replay.Answers(); err == nil || answers != nil {
		t.Fatal("failed counter batch published earlier definedness")
	}
}

func TestCompilerCounterDeliveredAnswersDriveFreshSourceState(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	counters, probes := counterGuardSnapshotForTest(t, scopes, nil, 3, "7 42 9")
	definitions := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__COUNTER__"}, "1")
	answers, err := MergeKbuildCompilerGuardAnswers(counters, definitions)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Toolsets: maps.Clone(probes.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
	sequence, sequenceID, err := configDependencySupplementalCompilerCounter(plan, "target", "cc", "c", nil, nil, nil)
	if err != nil || sequence == nil || sequenceID == "" {
		t.Fatalf("sequence delivery: %v", err)
	}
	measured, definednessID, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
	if err != nil || definednessID == "" {
		t.Fatalf("definedness delivery: %v", err)
	}
	const dump = "#define DROP(x)\n"
	parsed := macroReplacementStateForTest(t, dump)
	initial, valid := configDependencyApplyCompilerDefinedness(*parsed, measured, definednessID)
	if !valid {
		t.Fatal("invalid measured initial namespace")
	}
	context := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	for unit := 0; unit < 2; unit++ {
		state := initial.branch()
		if !state.installInitialCompilerCounter(measured, context, dump, definednessID, sequence) {
			t.Fatal("authenticated vector not admitted on fresh source branch")
		}
		coverage := &configDependencyCallCoverage{}
		if reason := coverage.expand("DROP(__COUNTER__) __COUNTER__", state); reason != "" {
			t.Fatal(reason)
		}
		if value, reason := coverage.condition("__COUNTER__ == 42", state); reason != "" || value != configDependencyMacroDefined {
			t.Fatalf("delivered sequence did not control source condition: %d, %s", value, reason)
		}
		if state.counter.next != 2 || len(coverage.counters) != 2 || coverage.counters[0].Token != "7" {
			t.Fatal("translation unit borrowed another unit's cursor")
		}
	}
	if initial.counter != (compilerCounterCursor{}) || len(sequence.values) != 3 {
		t.Fatal("source observation mutated initial state or immutable measured vector")
	}
}

func TestCompilerCounterAnswersBypassDefinednessOnlyCompletedCache(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	answers, probes := counterGuardSnapshotForTest(t, scopes, nil, 3, "7 42 9")
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index := 0; index < 2; index++ {
		plan, node := configDependencyCompletedCachePlanForTest(t, root, "variant", "compile", "src-00000001", "image", probes.Toolsets["target"], map[string]string{"CONFIG_RELEVANT": "y"})
		plan.Toolsets = maps.Clone(probes.Toolsets)
		if index == 1 {
			plan.metadata.compilerGuardAnswers = answers
		}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_RELEVANT"}) {
			t.Fatalf("unexpected fresh source analysis: %#v", set)
		}
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("counter answers entered definedness-only completed cache: hits %d stores %d",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func TestCompilerCounterGuardConflictIsTransactionalWithinRound(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	args := []string{"-nostdinc"}
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{2, 3} {
		if _, err := discovery.CompilerCounterSequence("target", "cc", "c", args, nil, count, nil); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{compilerCounterSequenceStep: "7 42"})
	for _, node := range plan.Nodes {
		count, err := compilerCounterProbeCount(plan.Requests[node.RequestID].Steps[0])
		if err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			result := oracle.results[node.ID]
			result.Text, result.Steps[0].Stdout = "7 43 9", "7 43 9"
			oracle.results[node.ID] = result
		}
	}
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := replay.CompilerCounterSequence("target", "cc", "c", args, nil, 2, nil); err != nil || !ready {
		t.Fatalf("first vector: %t, %v", ready, err)
	}
	key := configDependencyCompilerPredefineKey("target", "cc", "c", args, nil, nil)
	before := maps.Clone(replay.counters[key])
	if ready, err := replay.CompilerCounterSequence("target", "cc", "c", args, nil, 3, nil); err == nil || ready {
		t.Fatal("conflicting vector accepted")
	}
	if !maps.Equal(before, replay.counters[key]) {
		t.Fatal("conflict changed earlier observations")
	}
	if answers, err := replay.Answers(); err == nil || answers != nil {
		t.Fatal("conflicted round published its earlier prefix")
	}
}
