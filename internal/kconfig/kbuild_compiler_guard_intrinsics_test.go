package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compilerIntrinsicGuardSnapshotForTest(t *testing.T, scopes *KbuildProbeScopes, args []string, call CompilerIntrinsicCall, text string) (*KbuildCompilerGuardAnswers, *ProbePlan) {
	t.Helper()
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if value, ready, err := discovery.CompilerIntrinsicInteger("target", "cc", "c", args, nil, call.Operator, call.Operand, nil); err != nil || ready || value != "" {
		t.Fatalf("intrinsic discovery: %q %t %v", value, ready, err)
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-intrinsic-integer": text})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if value, ready, err := replay.CompilerIntrinsicInteger("target", "cc", "c", args, nil, call.Operator, call.Operand, nil); err != nil || !ready || value != strings.TrimSpace(text) {
		t.Fatalf("intrinsic replay: %q %t %v", value, ready, err)
	}
	answers, err := replay.Answers()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := replay.CompilerIntrinsicInteger("target", "cc", "c", args, nil, call.Operator, call.Operand, nil); err == nil {
		t.Fatal("answer snapshot did not freeze the batch")
	}
	return answers, plan
}

func TestKbuildCompilerGuardIntrinsicAnswersAreExactAndSeparate(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	args := []string{"-DFEATURE=7", "-UFEATURE", "-DFEATURE=8"}
	call := CompilerIntrinsicCall{"__has_attribute", "__retain__"}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	var identities []string
	for _, token := range []string{"0", "1", "202311", "0x1"} {
		answers, probePlan := compilerIntrinsicGuardSnapshotForTest(t, scopes, args, call, token+"\n")
		plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
		value, identity, ready, err := configDependencySupplementalCompilerIntrinsic(plan, "target", "cc", "c", args, nil, nil, call)
		if err != nil || !ready || value != token || identity == "" || slices.Contains(identities, identity) {
			t.Fatalf("exact integer answer: %q %q %t %v", value, identity, ready, err)
		}
		identities = append(identities, identity)
		definitions, definednessID, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", args, nil, nil)
		if err != nil || definitions != nil || definednessID != "" {
			t.Fatal("integer predicate was promoted to a definedness answer")
		}
		for _, change := range []string{"operand", "replacement", "order", "environment", "language", "role", "units", "scope"} {
			t.Run(token+"/"+change, func(t *testing.T) {
				changedCall, changedArgs := call, slices.Clone(args)
				scope, role, language := "target", "cc", "c"
				var units []string
				var environment map[string]string
				switch change {
				case "operand":
					changedCall.Operand = "btf_type_tag"
				case "replacement":
					changedArgs[2] = "-DFEATURE=9"
				case "order":
					changedArgs[0], changedArgs[1] = changedArgs[1], changedArgs[0]
				case "environment":
					environment = map[string]string{"MODE": "different"}
				case "language":
					language = "assembler-with-cpp"
				case "role":
					role = "cxx"
				case "units":
					units = []string{"source.c"}
				case "scope":
					scope = "host"
				}
				value, identity, ready, err := configDependencySupplementalCompilerIntrinsic(plan, scope, role, language, changedArgs, units, environment, changedCall)
				if err != nil || ready || value != "" || identity != "" {
					t.Fatalf("changed context borrowed answer: %q %q %t %v", value, identity, ready, err)
				}
			})
		}
		plan.Toolsets["target"] = "sha256-" + strings.Repeat("f", 64)
		if _, _, ready, err := configDependencySupplementalCompilerIntrinsic(plan, "target", "cc", "c", args, nil, nil, call); err == nil || ready {
			t.Fatal("changed toolset borrowed intrinsic answer")
		}
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("supplemental calls changed the ordinary workload")
	}
}

func TestKbuildCompilerGuardIntrinsicMergeRetainsTypedFacts(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	scopes := compilerGuardBatchScopesForTest(t, options)
	firstCall, secondCall := CompilerIntrinsicCall{"__has_attribute", "__retain__"}, CompilerIntrinsicCall{"__has_attribute", "btf_type_tag"}
	first, probePlan := compilerIntrinsicGuardSnapshotForTest(t, scopes, nil, firstCall, "0\n")
	second, _ := compilerIntrinsicGuardSnapshotForTest(t, scopes, nil, secondCall, "202311\n")
	conflict, _ := compilerIntrinsicGuardSnapshotForTest(t, scopes, nil, firstCall, "1\n")
	definedBatch, _ := compilerGuardAnswerBatchForTest(t, scopes, nil, nil, nil, [][]string{{"__has_attribute"}}, []string{"1\n"})
	defined, err := definedBatch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	for _, inputs := range [][]*KbuildCompilerGuardAnswers{{first, second, defined}, {defined, second, first, first, nil}} {
		merged, err := MergeKbuildCompilerGuardAnswers(inputs...)
		if err != nil {
			t.Fatal(err)
		}
		plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: merged}}
		for call, want := range map[CompilerIntrinsicCall]string{firstCall: "0", secondCall: "202311"} {
			value, identity, ready, err := configDependencySupplementalCompilerIntrinsic(plan, "target", "cc", "c", nil, nil, nil, call)
			if err != nil || !ready || value != want || identity == "" {
				t.Fatalf("merged intrinsic: %q %q %t %v", value, identity, ready, err)
			}
		}
		definitions, _, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
		if err != nil || !maps.Equal(definitions, map[string]bool{"__has_attribute": true}) {
			t.Fatalf("typed definedness changed: %#v %v", definitions, err)
		}
		plan.Toolsets["host"] = "sha256-" + strings.Repeat("f", 64)
		if _, _, ready, err := configDependencySupplementalCompilerIntrinsic(plan, "target", "cc", "c", nil, nil, nil, firstCall); err == nil || ready {
			t.Fatal("target predicate ignored changed host toolset")
		}
	}
	if merged, err := MergeKbuildCompilerGuardAnswers(first, conflict); err == nil || merged != nil {
		t.Fatal("conflicting intrinsic tokens published a merged snapshot")
	}
	if merged, err := MergeKbuildCompilerGuardAnswers(first, first, nil); err != nil || merged != first {
		t.Fatal("equivalent intrinsic snapshots did not preserve identity")
	}
	if len(first.intrinsics) != 1 || len(second.intrinsics) != 1 || len(first.entries) != 0 {
		t.Fatal("merge mutated its immutable inputs")
	}
}

func TestKbuildCompilerGuardIntrinsicRejectsPartialAndMalformedReplay(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	call := CompilerIntrinsicCall{"__has_attribute", "__retain__"}
	_, plan := compilerIntrinsicGuardSnapshotForTest(t, scopes, nil, call, "1\n")
	id := plan.Nodes[0].ID
	for _, failure := range []string{"missing", "request", "scope", "toolset", "step", "failed", "stdout", "boolean", "expression", "overflow", "second missing", "invalid call"} {
		t.Run(failure, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-intrinsic-integer": "1\n"})
			result := oracle.results[id]
			switch failure {
			case "request":
				result.RequestID = strings.Repeat("f", 64)
			case "scope":
				result.Scope = "host"
			case "toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "step":
				result.Steps = nil
			case "failed":
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			case "stdout":
				result.Steps[0].Stdout = "0\n"
			case "boolean":
				result.Text, result.Steps[0].Stdout = "true\n", "true\n"
			case "expression":
				result.Text, result.Steps[0].Stdout = "1+1\n", "1+1\n"
			case "overflow":
				result.Text, result.Steps[0].Stdout = "9223372036854775808\n", "9223372036854775808\n"
			}
			oracle.results[id] = result
			if failure == "missing" {
				delete(oracle.results, id)
			}
			batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			_, ready, err := batch.CompilerIntrinsicInteger("target", "cc", "c", nil, nil, call.Operator, call.Operand, nil)
			if failure == "second missing" || failure == "invalid call" {
				if err != nil || !ready {
					t.Fatal("first valid call failed")
				}
				operand := "btf_type_tag"
				if failure == "invalid call" {
					operand = "invalid(argument)"
				}
				_, ready, err = batch.CompilerIntrinsicInteger("target", "cc", "c", nil, nil, call.Operator, operand, nil)
			}
			if err == nil || ready {
				t.Fatal("bad or missing intrinsic result supplied an answer")
			}
			if answers, err := batch.Answers(); err == nil || answers != nil {
				t.Fatal("failed intrinsic round published partial answers")
			}
		})
	}
}

func TestKbuildCompilerGuardIntrinsicReplaysCurrentSymbolicDependency(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	evaluator := scopes.evaluators["target"]
	argument, err := evaluator.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "intrinsic-input", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "intrinsic-input", Stream: "stdout", RequireSuccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	dependency := before.Nodes[0].ID
	query := func(oracle *ProbeResultOracle) (*KbuildCompilerGuardBatch, bool, error) {
		batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			return nil, false, err
		}
		_, ready, err := batch.CompilerIntrinsicIntegers("target", "cc", "c", []string{argument}, nil, []CompilerIntrinsicCall{{"__has_attribute", "__retain__"}, {"__has_attribute", "btf_type_tag"}}, nil)
		return batch, ready, err
	}
	discovery, ready, err := query(nil)
	if err != nil || ready {
		t.Fatalf("symbolic intrinsic discovery: %t %v", ready, err)
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != 2 {
		t.Fatalf("symbolic intrinsic plan: %#v %v", plan, err)
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"intrinsic-input": "-DFEATURE=7", "compiler-intrinsic-integer": "1 202311\n"})
	current := &ProbeResultOracle{toolsets: maps.Clone(plan.Toolsets), results: map[string]ProbeResult{dependency: oracle.results[dependency]}}
	evaluator.oracle = current
	batch, ready, err := query(oracle)
	if err != nil || !ready {
		t.Fatalf("current symbolic replay: %t %v", ready, err)
	}
	if _, err := batch.Answers(); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"current missing", "current changed", "prior missing", "prior malformed"} {
		t.Run(failure, func(t *testing.T) {
			oldCurrent, oldPrior := current.results[dependency], oracle.results[dependency]
			switch failure {
			case "current missing":
				delete(current.results, dependency)
			case "current changed":
				changed := oldCurrent
				changed.Steps = slices.Clone(changed.Steps)
				changed.Text, changed.Steps[0].Stdout = "-DFEATURE=8", "-DFEATURE=8"
				current.results[dependency] = changed
			case "prior missing":
				delete(oracle.results, dependency)
			case "prior malformed":
				changed := oldPrior
				changed.Text = "-DFEATURE=8"
				oracle.results[dependency] = changed
			}
			if _, ready, err := query(oracle); err == nil || ready {
				t.Fatal("stale or missing symbolic dependency supplied intrinsic authority")
			}
			current.results[dependency], oracle.results[dependency] = oldCurrent, oldPrior
		})
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("intrinsic import changed ordinary registration")
	}
}

func compilerIntrinsicVectorPlanForTest(t *testing.T, scopes *KbuildProbeScopes, vectors [][]CompilerIntrinsicCall) *ProbePlan {
	t.Helper()
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, calls := range vectors {
		original := slices.Clone(calls)
		if values, ready, err := batch.CompilerIntrinsicIntegers("target", "cc", "c", nil, nil, calls, nil); err != nil || ready || values != nil {
			t.Fatalf("vector discovery: %#v %t %v", values, ready, err)
		}
		if !slices.Equal(original, calls) {
			t.Fatal("vector registration mutated caller-owned calls")
		}
	}
	plan, err := batch.Plan()
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func compilerIntrinsicVectorOracleForTest(t *testing.T, plan *ProbePlan) (*ProbeResultOracle, map[string]string) {
	t.Helper()
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-intrinsic-integer": "0 1\n"})
	ids := map[string]string{}
	for _, node := range plan.Nodes {
		stdin := plan.Requests[node.RequestID].Steps[0].Stdin
		if strings.Contains(stdin, "__has_attribute(b)") {
			ids["ab"] = node.ID
		} else if strings.Contains(stdin, "__has_attribute(c)") {
			ids["ac"] = node.ID
			result := oracle.results[node.ID]
			result.Text, result.Steps[0].Stdout = "0 202311\n", "0 202311\n"
			oracle.results[node.ID] = result
		} else {
			t.Fatalf("unexpected vector fixture source: %q", stdin)
		}
	}
	return oracle, ids
}

func TestKbuildCompilerGuardIntrinsicVectorsRetainCanonicalContributors(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	a, b, c := CompilerIntrinsicCall{"__has_attribute", "a"}, CompilerIntrinsicCall{"__has_attribute", "b"}, CompilerIntrinsicCall{"__has_attribute", "c"}
	expected := map[CompilerIntrinsicCall]string{a: "0", b: "1", c: "202311"}
	key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	var first *KbuildCompilerGuardAnswers
	for _, vectors := range [][][]CompilerIntrinsicCall{{{b, a, a}, {c, a}}, {{a, c}, {a, b}, {c, a}}} {
		plan := compilerIntrinsicVectorPlanForTest(t, scopes, vectors)
		if len(plan.Nodes) != 2 || len(plan.Terminal) != 2 {
			t.Fatalf("two distinct canonical vectors produced %d nodes/%d terminals", len(plan.Nodes), len(plan.Terminal))
		}
		oracle, _ := compilerIntrinsicVectorOracleForTest(t, plan)
		batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			t.Fatal(err)
		}
		for _, calls := range vectors {
			values, ready, err := batch.CompilerIntrinsicIntegers("target", "cc", "c", nil, nil, calls, nil)
			if err != nil || !ready || len(values) != 2 {
				t.Fatalf("overlapping vector replay: %#v %t %v", values, ready, err)
			}
			for _, call := range calls {
				if values[call] != expected[call] {
					t.Fatalf("vector value for %#v = %q", call, values[call])
				}
			}
			values[a], values[CompilerIntrinsicCall{"__has_attribute", "unqueried"}] = "wrong", "0"
		}
		if len(batch.intrinsics[key][a]) != 2 || len(batch.intrinsics[key][b]) != 1 || len(batch.intrinsics[key][c]) != 1 {
			t.Fatal("overlap lost or duplicated its distinct contributing references")
		}
		answers, err := batch.Answers()
		if err != nil {
			t.Fatal(err)
		}
		planForLookup := &ActionPlan{Toolsets: maps.Clone(plan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
		for call, want := range expected {
			value, identity, ready, err := configDependencySupplementalCompilerIntrinsic(planForLookup, "target", "cc", "c", nil, nil, nil, call)
			if err != nil || !ready || value != want || identity == "" {
				t.Fatalf("immutable vector answer: %q %q %t %v", value, identity, ready, err)
			}
		}
		if first != nil && !reflect.DeepEqual(first, answers) {
			t.Fatal("contributor snapshot identity depends on registration order or duplicates")
		}
		first = answers
	}
	single, _ := compilerIntrinsicGuardSnapshotForTest(t, scopes, nil, a, "0\n")
	if single.intrinsics[key][a].identity == first.intrinsics[key][a].identity {
		t.Fatal("changed contributing requests reused the same per-call identity")
	}
	merged, err := MergeKbuildCompilerGuardAnswers(single, first, first)
	if err != nil || merged.intrinsics[key][a].token != "0" || len(merged.intrinsics[key]) != 3 {
		t.Fatalf("singleton/vector cross-round merge: %#v %v", merged, err)
	}
}

func TestKbuildCompilerGuardIntrinsicVectorFailurePublishesNoPrefix(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	a, b, c := CompilerIntrinsicCall{"__has_attribute", "a"}, CompilerIntrinsicCall{"__has_attribute", "b"}, CompilerIntrinsicCall{"__has_attribute", "c"}
	ab, ac := []CompilerIntrinsicCall{a, b}, []CompilerIntrinsicCall{a, c}
	plan := compilerIntrinsicVectorPlanForTest(t, scopes, [][]CompilerIntrinsicCall{ab, ac})
	key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	for _, failure := range []string{"overlap conflict", "invalid last token", "missing last token", "missing result", "repeated node contradiction", "same node changed reference"} {
		t.Run(failure, func(t *testing.T) {
			oracle, ids := compilerIntrinsicVectorOracleForTest(t, plan)
			batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			if values, ready, err := batch.CompilerIntrinsicIntegers("target", "cc", "c", nil, nil, ab, nil); err != nil || !ready || len(values) != 2 {
				t.Fatalf("initial valid vector: %#v %t %v", values, ready, err)
			}
			before := batch.intrinsicAnswers(plan.Toolsets)
			if failure == "same node changed reference" {
				reference := batch.intrinsics[key][a][ids["ab"]].reference
				reference.RequestID = strings.Repeat("f", 64)
				if err := batch.recordIntrinsicAnswers(key, reference, map[CompilerIntrinsicCall]string{a: "0", c: "202311"}); err == nil {
					t.Fatal("same node ID accepted changed full reference metadata")
				}
				if !reflect.DeepEqual(before, batch.intrinsicAnswers(plan.Toolsets)) {
					t.Fatal("invalid reference published a new call before rejecting overlap")
				}
				return
			}
			calls, id := ac, ids["ac"]
			if failure == "repeated node contradiction" {
				calls, id = ab, ids["ab"]
			}
			result := oracle.results[id]
			result.Steps = slices.Clone(result.Steps)
			switch failure {
			case "overlap conflict", "repeated node contradiction":
				result.Text = "1 202311\n"
			case "invalid last token":
				result.Text = "0 invalid\n"
			case "missing last token":
				result.Text = "0\n"
			}
			result.Steps[0].Stdout = result.Text
			oracle.results[id] = result
			if failure == "missing result" {
				delete(oracle.results, id)
			}
			values, ready, err := batch.CompilerIntrinsicIntegers("target", "cc", "c", nil, nil, calls, nil)
			if err == nil || ready || values != nil {
				t.Fatalf("bad vector leaked a result prefix: %#v %t %v", values, ready, err)
			}
			if !reflect.DeepEqual(before, batch.intrinsicAnswers(plan.Toolsets)) {
				t.Fatal("failed vector mutated earlier observations")
			}
			if answers, err := batch.Answers(); err == nil || answers != nil {
				t.Fatal("failed vector round published earlier partial answers")
			}
		})
	}
}
