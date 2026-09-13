package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestOptionalCompilerCounterAttemptKeepsMandatoryIdentity(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, first, err := batch.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", nil, nil, 3, nil)
	if err != nil || state != OptionalCompilerCounterPending || first.NodeID == "" || first.Kind != "boolean" {
		t.Fatalf("discovery supplied authority or lost terminal: %v %#v %v", state, first, err)
	}
	_, second, err := batch.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", nil, nil, 3, nil)
	if err != nil || first != second {
		t.Fatalf("duplicate attempt lost identity: %#v %v", second, err)
	}
	plan, err := batch.Plan()
	if err != nil || len(plan.Nodes) != 1 || len(plan.Terminal) != 1 {
		t.Fatalf("optional plan: %#v %v", plan, err)
	}
	if answers, err := batch.Answers(); err == nil || answers != nil {
		t.Fatal("discovery supplied answers")
	}
	mandatory, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mandatory.CompilerCounterSequence("target", "cc", "c", nil, nil, 3, nil); err != nil {
		t.Fatal(err)
	}
	mandatoryPlan, err := mandatory.Plan()
	if err != nil {
		t.Fatal(err)
	}
	optionalRequest := plan.Requests[plan.Nodes[0].RequestID]
	mandatoryRequest := mandatoryPlan.Requests[mandatoryPlan.Nodes[0].RequestID]
	if optionalRequest.Outcome.Kind != "boolean" || mandatoryRequest.Outcome.Kind != "text" ||
		!mandatoryRequest.Outcome.RequireSuccess || plan.Nodes[0].RequestID == mandatoryPlan.Nodes[0].RequestID ||
		!reflect.DeepEqual(optionalRequest.Steps, mandatoryRequest.Steps) {
		t.Fatal("optional outcome widened the compiler input or reused mandatory result identity")
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("optional attempt changed ordinary probes")
	}
}

func TestOptionalCompilerCounterAttemptAuthenticatesCompletion(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, reference, err := discovery.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", nil, nil, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"answered", "rejected plausible", "rejected malformed", "missing", "signal", "impossible exit", "skipped",
		"missing step", "extra step", "wrong step", "stale request", "stale node", "wrong scope", "wrong toolset",
		"wrong kind", "spoof success", "spoof rejection", "status mismatch", "missing token", "extra token", "invalid token",
	} {
		t.Run(name, func(t *testing.T) {
			oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{compilerCounterSequenceStep: "7 42 9"})
			result := oracle.results[reference.NodeID]
			wantState, wantError := OptionalCompilerCounterPending, true
			switch name {
			case "answered":
				wantState, wantError = OptionalCompilerCounterAnswered, false
			case "rejected plausible", "rejected malformed":
				wantState, wantError = OptionalCompilerCounterUnqueryable, false
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				if name == "rejected malformed" {
					result.Steps[0].Stdout = "not a counter sequence"
				}
			case "signal", "impossible exit":
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", -1
				if name == "impossible exit" {
					result.Steps[0].ExitCode = 256
				}
			case "skipped":
				*result.Boolean = false
				result.Steps[0] = ProbeStepResult{Name: compilerCounterSequenceStep, Status: "skipped", ExitCode: -1}
			case "missing step":
				result.Steps = nil
			case "extra step":
				result.Steps = append(result.Steps, ProbeStepResult{Name: "extra", Status: "success"})
			case "wrong step":
				result.Steps[0].Name = "other"
			case "stale request":
				result.RequestID = strings.Repeat("f", 64)
			case "stale node":
				result.NodeID = strings.Repeat("f", 64)
			case "wrong scope":
				result.Scope = "host"
			case "wrong toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "wrong kind":
				result.Kind, result.Boolean, result.Text = "text", nil, "7 42 9"
			case "spoof success":
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			case "spoof rejection":
				*result.Boolean = false
			case "status mismatch":
				result.Steps[0].ExitCode = 1
			case "missing token":
				result.Steps[0].Stdout = "7 42"
			case "extra token":
				result.Steps[0].Stdout = "7 42 9 10"
			case "invalid token":
				result.Steps[0].Stdout = "7 __COUNTER__ 9"
			}
			oracle.results[reference.NodeID] = result
			if name == "missing" {
				delete(oracle.results, reference.NodeID)
			}
			replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			state, gotReference, err := replay.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", nil, nil, 3, nil)
			if (err != nil) != wantError || state != wantState ||
				wantError && gotReference != (ProbeReference{}) || !wantError && gotReference != reference {
				t.Fatalf("attempt = %v %#v %v, want %v error %t", state, gotReference, err, wantState, wantError)
			}
			answers, err := replay.Answers()
			if wantError {
				if err == nil || answers != nil {
					t.Fatal("invalid attempt published a partial snapshot")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(answers.entries) != 0 || len(answers.intrinsics) != 0 {
					t.Fatal("counter attempt granted unrelated compiler facts")
				}
				if wantState == OptionalCompilerCounterUnqueryable {
					if len(answers.counters) != 0 {
						t.Fatal("rejected stdout became counter facts")
					}
				} else {
					key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
					answer := answers.counters[key]
					if answer.sequence == nil || !slices.Equal(answer.sequence.values, []string{"7", "42", "9"}) || answer.identity == "" {
						t.Fatal("lost measured vector identity")
					}
				}
			}
			mandatory, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			if ready, err := mandatory.CompilerCounterSequence("target", "cc", "c", nil, nil, 3, nil); err == nil || ready {
				t.Fatal("mandatory query borrowed optional attempt authority")
			}
		})
	}
}

func TestOptionalCompilerCounterAttemptAuthenticatesCurrentDependencies(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	evaluator := scopes.evaluators["target"]
	argument, err := evaluator.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "flags", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "flags", Stream: "stdout", RequireSuccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	args, units, environment := []string{argument, "source.c"}, []string{"source.c"}, map[string]string{"MODE": argument}
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, reference, err := discovery.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", args, units, 3, environment)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != 2 {
		t.Fatalf("dependency plan: %#v %v", plan, err)
	}
	terminal := plan.Nodes[len(plan.Nodes)-1]
	step := plan.Requests[terminal.RequestID].Steps[0]
	if !slices.Equal(terminal.Inputs, []string{before.Nodes[0].ID}) || len(step.ArgumentFragments) == 0 || len(step.EnvironmentFragments) != 1 {
		t.Fatal("optional counter lost its exact argv/environment dependency closure")
	}
	for _, name := range []string{"answered", "rejected", "missing current", "changed current", "spoof current", "missing supplemental"} {
		t.Run(name, func(t *testing.T) {
			oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{"flags": "-std=gnu11", compilerCounterSequenceStep: "7 42 9"})
			dependency := before.Nodes[0].ID
			current := &ProbeResultOracle{results: map[string]ProbeResult{dependency: oracle.results[dependency]}, toolsets: maps.Clone(plan.Toolsets)}
			if name != "answered" {
				result := oracle.results[reference.NodeID]
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				oracle.results[reference.NodeID] = result
			}
			switch name {
			case "missing current":
				delete(current.results, dependency)
			case "missing supplemental":
				delete(oracle.results, dependency)
			case "changed current", "spoof current":
				result := current.results[dependency]
				result.Steps = slices.Clone(result.Steps)
				result.Text = "-std=c11"
				if name == "changed current" {
					result.Steps[0].Stdout = result.Text
				}
				current.results[dependency] = result
			}
			evaluator.oracle = current
			replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			state, gotReference, err := replay.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", args, units, 3, environment)
			valid := name == "answered" || name == "rejected"
			if (err == nil) != valid || !valid && (state != OptionalCompilerCounterPending || gotReference != (ProbeReference{})) || valid && gotReference != reference {
				t.Fatalf("current-context replay: %v %#v %v", state, gotReference, err)
			}
			if name == "answered" && state != OptionalCompilerCounterAnswered || name == "rejected" && state != OptionalCompilerCounterUnqueryable {
				t.Fatal("wrong completion state")
			}
			answers, err := replay.Answers()
			if (err == nil) != valid || !valid && answers != nil {
				t.Fatal("unauthenticated dependency allowed partial snapshot")
			}
			if name == "rejected" && len(answers.counters) != 0 {
				t.Fatal("rejected vector supplied facts")
			}
		})
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("optional query mutated ordinary registration")
	}
}

func TestOptionalCompilerCounterAttemptPrefixConflictPoisonsWholeBatch(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	args := []string{"-nostdinc"}
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{2, 3} {
		if _, _, err := discovery.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", args, nil, count, nil); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{compilerCounterSequenceStep: "7 42"})
	for _, node := range plan.Nodes {
		count, err := compilerCounterProbeCount(plan.Requests[node.RequestID].Steps[0])
		if err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			result := oracle.results[node.ID]
			result.Steps[0].Stdout = "7 43 9"
			oracle.results[node.ID] = result
		}
	}
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if state, _, err := replay.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", args, nil, 2, nil); err != nil || state != OptionalCompilerCounterAnswered {
		t.Fatalf("first vector: %v %v", state, err)
	}
	if state, reference, err := replay.OptionalCompilerCounterSequenceAttempt("target", "cc", "c", args, nil, 3, nil); err == nil || state != OptionalCompilerCounterPending || reference != (ProbeReference{}) {
		t.Fatal("conflicting prefix gained authority")
	}
	if answers, err := replay.Answers(); err == nil || answers != nil {
		t.Fatal("conflict published earlier prefix")
	}
}

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
