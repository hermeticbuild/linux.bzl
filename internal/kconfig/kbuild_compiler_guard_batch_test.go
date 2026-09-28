package kconfig

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compilerGuardBatchScopesForTest(t *testing.T, options KbuildProbeWorkloadOptions) *KbuildProbeScopes {
	t.Helper()
	evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (*KbuildProbeScopes, error) {
		return scopes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluation.Value
}

func compilerGuardBatchOrdinaryPlanForTest(t *testing.T, scopes *KbuildProbeScopes) *ProbePlan {
	t.Helper()
	plan, err := scopes.evaluators["target"].discovery.(*ProbePlanBuilder).Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestKbuildCompilerGuardBatchMatchesOrdinaryRequestAndFreezes(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	args := []string{"-DFEATURE=7", "-UFEATURE", "-DFEATURE=8", "-include", "forced.h", "source.c"}
	units, names := []string{"source.c"}, []string{"__SECOND", "__FIRST", "__SECOND"}
	environment := map[string]string{"MODE": "exact"}
	if _, ready, err := scopes.CompilerDefinedness("target", "cc", "c", args, units, names, environment); err != nil || ready {
		t.Fatalf("ordinary discovery: ready=%t error=%v", ready, err)
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		values, ready, err := batch.CompilerDefinedness("target", "cc", "c", args, units, names, environment)
		if err != nil || ready || values != nil {
			t.Fatalf("detached discovery = %#v %t %v", values, ready, err)
		}
	}
	plan, err := batch.Plan()
	if err != nil || len(plan.Nodes) != 1 || len(plan.Terminal) != 1 {
		t.Fatalf("detached plan = %#v %v", plan, err)
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("supplemental registration changed the ordinary workload")
	}
	ordinary := before.Requests[before.Nodes[0].RequestID]
	detached := plan.Requests[plan.Nodes[0].RequestID]
	want, err := ordinary.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := detached.CanonicalJSON()
	if err != nil || !bytes.Equal(got, want) || plan.Nodes[0].ID != before.Nodes[0].ID {
		t.Fatalf("detached request differs from ordinary bytes: %s != %s (%v)", got, want, err)
	}
	args[0], units[0], names[0], environment["MODE"] = "changed", "changed", "changed", "changed"
	detached.Steps[0].Arguments[0] = "mutated returned plan"
	again, err := batch.Plan()
	if err != nil {
		t.Fatal(err)
	}
	got, err = again.Requests[again.Nodes[0].RequestID].CanonicalJSON()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("caller-owned arguments or returned plan mutated the frozen request")
	}
	if _, ready, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__LATE"}, nil); err == nil || ready {
		t.Fatal("frozen batch accepted a later demand")
	}
}

func TestKbuildCompilerGuardBatchContextIdentity(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	options.Target.Tools = maps.Clone(options.Target.Tools)
	options.Target.Tools["cxx"] = options.Target.Tools["cc"]
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	scopes := compilerGuardBatchScopesForTest(t, options)
	seen := map[string]string{}
	for _, test := range []struct {
		name, scope, role, language string
		args, units, names          []string
		environment                 map[string]string
	}{
		{"base", "target", "cc", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__A"}, nil},
		{"argument order", "target", "cc", "c", []string{"-UA", "-DA=1", "source.c"}, []string{"source.c"}, []string{"__A"}, nil},
		{"source projection", "target", "cc", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"other.c"}, []string{"__A"}, nil},
		{"names", "target", "cc", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__B"}, nil},
		{"environment", "target", "cc", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__A"}, map[string]string{"MODE": "other"}},
		{"language", "target", "cc", "assembler-with-cpp", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__A"}, nil},
		{"role", "target", "cxx", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__A"}, nil},
		{"scope", "host", "cc", "c", []string{"-DA=1", "-UA", "source.c"}, []string{"source.c"}, []string{"__A"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, ready, err := batch.CompilerDefinedness(test.scope, test.role, test.language, test.args, test.units, test.names, test.environment); err != nil || ready {
				t.Fatalf("identity discovery: %t %v", ready, err)
			}
			plan, err := batch.Plan()
			if err != nil || len(plan.Nodes) != 1 {
				t.Fatalf("identity plan: %#v %v", plan, err)
			}
			id := plan.Nodes[0].ID
			if previous, exists := seen[id]; exists {
				t.Fatalf("%s reused %s request", test.name, previous)
			}
			seen[id] = test.name
		})
	}
}

func TestKbuildCompilerGuardBatchImportsSymbolicDependencyClosure(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	evaluator := scopes.evaluators["target"]
	request := func(name string) ProbeRequest {
		return ProbeRequest{Schema: LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: name, Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{Kind: "text", Step: name, Stream: "stdout", RequireSuccess: true}}
	}
	if _, err := evaluator.requestText(request("argument-input")); err != nil {
		t.Fatal(err)
	}
	argumentReference := evaluator.References()[0]
	argument, err := evaluator.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema, InputCount: 1,
		Outcome: ProbeOutcome{Kind: "text", Result: "00000000", Word: 1}}, argumentReference)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := evaluator.requestText(request("environment-input"))
	if err != nil {
		t.Fatal(err)
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	query := func(oracle *ProbeResultOracle) (*ProbePlan, bool, error) {
		batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			return nil, false, err
		}
		values, ready, err := batch.CompilerDefinedness("target", "cc", "c", []string{argument, "source.c"}, []string{"source.c"}, []string{"__GUARD"}, map[string]string{"MODE": environment})
		if err != nil {
			return nil, false, err
		}
		if ready && (!maps.Equal(values, map[string]bool{"__GUARD": false})) {
			t.Fatalf("symbolic replay values: %#v", values)
		}
		plan, err := batch.Plan()
		return plan, ready, err
	}
	plan, ready, err := query(nil)
	if err != nil || ready || len(plan.Nodes) != 4 {
		t.Fatalf("symbolic detached discovery = %#v %t %v", plan, ready, err)
	}
	for _, node := range before.Nodes {
		if !slices.ContainsFunc(plan.Nodes, func(imported ProbePlanNode) bool { return reflect.DeepEqual(imported, node) }) {
			t.Fatalf("lost original dependency identity/input ordering: %#v", node)
		}
	}
	terminal := plan.Nodes[len(plan.Nodes)-1]
	step := plan.Requests[terminal.RequestID].Steps[0]
	if len(terminal.Inputs) != 2 || terminal.Inputs[0] != before.Nodes[1].ID || terminal.Inputs[1] != before.Nodes[2].ID ||
		len(step.ArgumentFragments) == 0 || len(step.EnvironmentFragments) != 1 || step.Candidate == nil {
		t.Fatalf("symbolic request lost exact fragments or dependency order: %#v %#v", terminal, step)
	}
	oracle := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(plan.Toolsets)}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		result := ProbeResult{Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "text", Text: "-std=gnu11"}
		if len(request.Steps) != 0 {
			name := request.Steps[0].Name
			if name == "compiler-definedness" {
				result.Text = "0\n"
			}
			result.Steps = []ProbeStepResult{{Name: name, Status: "success", Stdout: result.Text}}
		}
		oracle.results[node.ID] = result
	}
	current := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(plan.Toolsets)}
	for _, node := range before.Nodes {
		current.results[node.ID] = oracle.results[node.ID]
	}
	evaluator.oracle = current
	replay, ready, err := query(oracle)
	if err != nil || !ready || !reflect.DeepEqual(plan, replay) {
		t.Fatalf("symbolic replay = %#v %t %v", replay, ready, err)
	}
	for _, changed := range []string{"missing oracle", "missing result", "different measured value", "malformed current reduction"} {
		t.Run(changed, func(t *testing.T) {
			previous := current.results[argumentReference.NodeID]
			switch changed {
			case "missing oracle":
				evaluator.oracle = nil
			case "missing result":
				delete(current.results, argumentReference.NodeID)
			case "different measured value", "malformed current reduction":
				result := previous
				result.Steps = slices.Clone(previous.Steps)
				result.Text = "-std=c11"
				if changed == "different measured value" {
					result.Steps[0].Stdout = result.Text
				}
				current.results[argumentReference.NodeID] = result
			}
			if _, ready, err := query(oracle); err == nil || ready {
				t.Fatal("old symbolic IDs supplied facts for a changed current compiler context")
			}
			evaluator.oracle = current
			current.results[argumentReference.NodeID] = previous
		})
	}
	spoofed := oracle.results[argumentReference.NodeID]
	spoofed.Text = "-std=c11"
	oracle.results[argumentReference.NodeID] = spoofed
	if _, ready, err := query(oracle); err == nil || ready {
		t.Fatal("supplemental replay accepted imported text inconsistent with its recorded stdout")
	}
	delete(oracle.results, argumentReference.NodeID)
	if _, ready, err := query(oracle); err == nil || ready {
		t.Fatal("supplemental replay accepted a missing transitive dependency")
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("symbolic supplemental discovery/replay changed ordinary registration")
	}
}

func TestKbuildCompilerGuardBatchRejectsMissingStaleAndSpoofedResults(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__A"}, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := batch.Plan()
	if err != nil {
		t.Fatal(err)
	}
	id := plan.Nodes[0].ID
	for _, failure := range []string{"missing", "stale request", "wrong scope", "wrong toolset", "wrong kind", "missing step", "failed step", "spoofed text", "nonboolean", "extra token"} {
		t.Run(failure, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-definedness": "0\n"})
			result := oracle.results[id]
			switch failure {
			case "stale request":
				result.RequestID = strings.Repeat("f", 64)
			case "wrong scope":
				result.Scope = "host"
			case "wrong toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "wrong kind":
				value := false
				result.Kind, result.Boolean, result.Text = "boolean", &value, ""
			case "missing step":
				result.Steps = nil
			case "failed step":
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			case "spoofed text":
				result.Steps[0].Stdout = "1\n"
			case "nonboolean":
				result.Text, result.Steps[0].Stdout = "false\n", "false\n"
			case "extra token":
				result.Text, result.Steps[0].Stdout = "0 0\n", "0 0\n"
			}
			oracle.results[id] = result
			if failure == "missing" {
				delete(oracle.results, id)
			}
			replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			values, ready, err := replay.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__A"}, nil)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if err == nil || ready || values != nil || errors.As(err, &unsupported) {
				t.Fatalf("invalid oracle supplied an answer: %#v %t %v", values, ready, err)
			}
			if _, err := replay.Plan(); err == nil {
				t.Fatal("failed replay published a completed plan")
			}
		})
	}
}

func TestKbuildCompilerGuardBatchSuccessiveRoundsRemainIndependent(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	var previousID string
	for _, name := range []string{"__REACHED_FIRST", "__REACHED_SECOND"} {
		batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if values, ready, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{name}, nil); err != nil || ready || values != nil {
			t.Fatalf("new round borrowed old answers: %#v %t %v", values, ready, err)
		}
		plan, err := batch.Plan()
		if err != nil || len(plan.Nodes) != 1 || plan.Nodes[0].ID == previousID {
			t.Fatalf("new name reused prior frozen batch: %#v %v", plan, err)
		}
		previousID = plan.Nodes[0].ID
		oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-definedness": "0\n"})
		replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			t.Fatal(err)
		}
		values, ready, err := replay.CompilerDefinedness("target", "cc", "c", nil, nil, []string{name}, nil)
		if err != nil || !ready || !maps.Equal(values, map[string]bool{name: false}) {
			t.Fatalf("round replay = %#v %t %v", values, ready, err)
		}
		values[name] = true
		values, ready, err = replay.CompilerDefinedness("target", "cc", "c", nil, nil, []string{name}, nil)
		if err != nil || !ready || values[name] {
			t.Fatal("caller changed a recorded negative fact")
		}
		if _, err := replay.Plan(); err != nil {
			t.Fatal(err)
		}
		missing, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			t.Fatal(err)
		}
		if _, ready, err := missing.CompilerDefinedness("target", "cc", "c", nil, nil, []string{name, "__NOT_IN_FROZEN_ROUND"}, nil); err == nil || ready {
			t.Fatal("strict replay silently became supplemental discovery")
		}
	}
	if len(compilerGuardBatchOrdinaryPlanForTest(t, scopes).Nodes) != 0 {
		t.Fatal("supplemental rounds polluted the ordinary Kbuild workload")
	}
}

func TestKbuildCompilerGuardBatchDoesNotRelaxOrdinaryMissingResults(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	scopes := compilerGuardBatchScopesForTest(t, options)
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__GUARD"}, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := batch.Plan()
	if err != nil {
		t.Fatal(err)
	}
	supplemental := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-definedness": "0\n"})
	ordinary := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(plan.Toolsets)}
	_, err = EvaluateKbuildProbeWorkload(options, ordinary, func(scopes *KbuildProbeScopes) (struct{}, error) {
		replay, err := NewKbuildCompilerGuardBatch(scopes, supplemental)
		if err != nil {
			return struct{}{}, err
		}
		if _, _, err := replay.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__GUARD"}, nil); err != nil {
			return struct{}{}, err
		}
		if _, err := replay.Plan(); err != nil {
			return struct{}{}, err
		}
		_, _, err = scopes.CompilerPredefines("target", "cc", "c", nil, nil, nil)
		return struct{}{}, err
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("ordinary missing-result failure was relaxed: %v", err)
	}
}

func TestKbuildCompilerGuardBatchRejectsUnsupportedBeforeRegistration(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	for _, name := range []string{"invalid name", "unbound environment"} {
		t.Run(name, func(t *testing.T) {
			batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
			if err != nil {
				t.Fatal(err)
			}
			names, environment := []string{"__VALID"}, map[string]string{}
			if name == "invalid name" {
				names = []string{"__INVALID\n#error injected"}
			} else {
				environment["MODE"] = "${work:root}/unbound"
			}
			values, ready, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, names, environment)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if !errors.As(err, &unsupported) || ready || values != nil {
				t.Fatalf("unsupported projection = %#v %t %v", values, ready, err)
			}
			plan, err := batch.Plan()
			if err != nil || len(plan.Nodes) != 0 {
				t.Fatalf("unsupported request partially registered: %#v %v", plan, err)
			}
		})
	}
	if _, err := NewKbuildCompilerGuardBatch(nil, nil); err == nil {
		t.Fatal("nil scopes accepted")
	}
	for _, test := range []struct{ scope, role, language string }{
		{"missing", "cc", "c"}, {"target", "cxx", "c++"}, {"target", "cc-link", "c"}, {"target", "cc", "rust"},
	} {
		batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ready, err := batch.CompilerDefinedness(test.scope, test.role, test.language, nil, nil, []string{"__GUARD"}, nil); err == nil || ready {
			t.Fatalf("unavailable configured compiler context accepted: %#v", test)
		}
		plan, err := batch.Plan()
		if err != nil || len(plan.Nodes) != 0 {
			t.Fatalf("unavailable context registered a partial plan: %#v %v", plan, err)
		}
	}
	wrongToolset := &ProbeResultOracle{toolsets: map[string]string{"target": "sha256-" + strings.Repeat("f", 64)}}
	if _, err := NewKbuildCompilerGuardBatch(scopes, wrongToolset); err == nil {
		t.Fatal("supplemental oracle with mismatched configured toolset accepted")
	}
}

func TestKbuildCompilerGuardOptionalDefinednessMixedResultsRemainAtomic(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"__A", "__B"} {
		if _, _, err := discovery.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, []string{name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != 2 {
		t.Fatalf("mixed plan: %#v %v", plan, err)
	}
	for _, malformed := range []bool{false, true} {
		oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{"compiler-definedness": "0"})
		for _, node := range plan.Nodes {
			if strings.Contains(plan.Requests[node.RequestID].Steps[0].Stdin, "defined(__B)") {
				result := oracle.results[node.ID]
				if malformed {
					result.Steps[0].Stdout = "not a Boolean"
				} else {
					*result.Boolean = false
					result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				}
				oracle.results[node.ID] = result
			}
		}
		replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			t.Fatal(err)
		}
		if values, state, err := replay.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, []string{"__A"}, nil); err != nil || state != OptionalCompilerDefinednessAnswered || !maps.Equal(values, map[string]bool{"__A": false}) {
			t.Fatalf("first answer: %#v %v %v", values, state, err)
		}
		values, state, err := replay.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, []string{"__B"}, nil)
		if values != nil || (err != nil) != malformed || !malformed && state != OptionalCompilerDefinednessUnqueryable {
			t.Fatalf("second attempt: %#v %v %v", values, state, err)
		}
		answers, err := replay.Answers()
		if malformed {
			if err == nil || answers != nil {
				t.Fatal("later malformed result published earlier partial facts")
			}
			continue
		}
		key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
		if err != nil || len(answers.entries) != 1 || !maps.Equal(answers.entries[key].definitions, map[string]bool{"__A": false}) {
			t.Fatalf("rejected result contaminated another measured vector: %#v %v", answers, err)
		}
	}
}

func optionalCompilerDefinednessOracleForTest(t *testing.T, plan *ProbePlan, stdout map[string]string) *ProbeResultOracle {
	t.Helper()
	oracle := compilerDefinednessTestOracle(t, plan, stdout)
	for _, node := range plan.Nodes {
		if plan.Requests[node.RequestID].Outcome.Kind == "boolean" {
			result := oracle.results[node.ID]
			positive := true
			result.Kind, result.Text, result.Boolean = "boolean", "", &positive
			oracle.results[node.ID] = result
		}
	}
	return oracle
}

func TestKbuildCompilerGuardOptionalDefinednessDiscoveryIdentity(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	args := []string{"-DFEATURE=7", "-UFEATURE", "-DFEATURE=8", "-include", "forced.h", "source.c"}
	units, names := []string{"source.c"}, []string{"__B", "__A", "__B"}
	environment := map[string]string{"MODE": "exact"}
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, vector := range [][]string{names, {"__A", "__B"}} {
		values, state, err := batch.OptionalCompilerDefinedness("target", "cc", "c", args, units, vector, environment)
		if err != nil || state != OptionalCompilerDefinednessPending || values != nil {
			t.Fatalf("optional discovery = %#v %v %v", values, state, err)
		}
	}
	plan, err := batch.Plan()
	if err != nil || len(plan.Nodes) != 1 || len(plan.Terminal) != 1 {
		t.Fatalf("optional plan = %#v %v", plan, err)
	}
	if _, err := batch.Answers(); err == nil {
		t.Fatal("discovery published an answer snapshot")
	}
	mandatory, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mandatory.CompilerDefinedness("target", "cc", "c", args, units, names, environment); err != nil {
		t.Fatal(err)
	}
	mandatoryPlan, err := mandatory.Plan()
	if err != nil {
		t.Fatal(err)
	}
	optionalRequest := plan.Requests[plan.Nodes[0].RequestID]
	mandatoryRequest := mandatoryPlan.Requests[mandatoryPlan.Nodes[0].RequestID]
	if optionalRequest.Outcome.Kind != "boolean" || optionalRequest.Outcome.Predicate == nil ||
		optionalRequest.Outcome.Predicate.Operator != "exit-zero" || optionalRequest.Outcome.Predicate.Step != "compiler-definedness" ||
		plan.Nodes[0].ID == mandatoryPlan.Nodes[0].ID {
		t.Fatal("optional request lost its separate exact Boolean outcome")
	}
	comparison := optionalRequest
	comparison.Outcome = mandatoryRequest.Outcome
	got, err := comparison.CanonicalJSON()
	want, wantErr := mandatoryRequest.CanonicalJSON()
	if err != nil || wantErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("optional query changed the compiler invocation: %v %v", err, wantErr)
	}
	args[0], units[0], names[0], environment["MODE"] = "changed", "changed", "changed", "changed"
	again, err := batch.Plan()
	if err != nil || !reflect.DeepEqual(plan, again) {
		t.Fatal("caller mutation changed the frozen optional plan")
	}
	if _, _, err := batch.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, []string{"__LATE"}, nil); err == nil {
		t.Fatal("frozen optional batch accepted a new demand")
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("optional discovery changed the ordinary builder")
	}
}

func TestKbuildCompilerGuardOptionalDefinednessStatesAndStrictFailures(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	names := []string{"__A", "__B"}
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := discovery.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, names, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	id := plan.Terminal[0]
	for _, test := range []string{
		"answered", "rejected with plausible stdout", "rejected with malformed stdout",
		"signal", "impossible exit", "skipped", "missing", "missing step", "extra step", "wrong step",
		"stale request", "wrong scope", "wrong toolset", "wrong kind", "stale node",
		"spoof success", "spoof rejection", "status mismatch", "missing token", "extra token", "nonboolean token",
	} {
		t.Run(test, func(t *testing.T) {
			oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{"compiler-definedness": "0 1\n"})
			result := oracle.results[id]
			wantState, wantError := OptionalCompilerDefinednessPending, true
			switch test {
			case "answered":
				wantState, wantError = OptionalCompilerDefinednessAnswered, false
			case "rejected with plausible stdout", "rejected with malformed stdout":
				wantState, wantError = OptionalCompilerDefinednessUnqueryable, false
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				if test == "rejected with malformed stdout" {
					result.Steps[0].Stdout = "not a Boolean trace"
				}
			case "signal", "impossible exit":
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", -1
				if test == "impossible exit" {
					result.Steps[0].ExitCode = 256
				}
			case "skipped":
				*result.Boolean = false
				result.Steps[0] = ProbeStepResult{Name: "compiler-definedness", Status: "skipped", ExitCode: -1}
			case "missing step":
				result.Steps = nil
			case "extra step":
				result.Steps = append(result.Steps, ProbeStepResult{Name: "extra", Status: "success"})
			case "wrong step":
				result.Steps[0].Name = "other"
			case "stale request":
				result.RequestID = strings.Repeat("f", 64)
			case "wrong scope":
				result.Scope = "host"
			case "wrong toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "wrong kind":
				result.Kind, result.Boolean, result.Text = "text", nil, "0 1\n"
			case "stale node":
				result.NodeID = strings.Repeat("f", 64)
			case "spoof success":
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			case "spoof rejection":
				*result.Boolean = false
			case "status mismatch":
				result.Steps[0].ExitCode = 1
			case "missing token":
				result.Steps[0].Stdout = "0"
			case "extra token":
				result.Steps[0].Stdout = "0 1 0"
			case "nonboolean token":
				result.Steps[0].Stdout = "0 false"
			}
			oracle.results[id] = result
			if test == "missing" {
				delete(oracle.results, id)
			}
			replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			values, state, reference, err := replay.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, names, nil)
			if wantError && reference != (ProbeReference{}) || !wantError &&
				(reference.NodeID != id || reference.RequestID != plan.Nodes[0].RequestID || reference.Scope != "target" || reference.Kind != "boolean") {
				t.Fatalf("optional attempt reference = %#v (error expected %t)", reference, wantError)
			}
			if (err != nil) != wantError || state != wantState {
				t.Fatalf("optional result = %#v %v %v; want state %v error %t", values, state, err, wantState, wantError)
			}
			if wantState == OptionalCompilerDefinednessAnswered {
				if !maps.Equal(values, map[string]bool{"__A": false, "__B": true}) {
					t.Fatalf("wrong measured answers: %#v", values)
				}
				values["__A"] = true
			} else if values != nil {
				t.Fatal("unfinished/rejected query supplied facts")
			}
			answers, answerErr := replay.Answers()
			if wantError {
				if answerErr == nil {
					t.Fatal("invalid replay published a partial snapshot")
				}
				return
			}
			if answerErr != nil {
				t.Fatal(answerErr)
			}
			key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
			if wantState == OptionalCompilerDefinednessUnqueryable {
				if len(answers.entries) != 0 || len(answers.intrinsics) != 0 {
					t.Fatal("rejected stdout reached the immutable fact snapshot")
				}
			} else if !maps.Equal(answers.entries[key].definitions, map[string]bool{"__A": false, "__B": true}) {
				t.Fatal("returned answer mutation changed recorded facts")
			}
			mandatory, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			if _, ready, err := mandatory.CompilerDefinedness("target", "cc", "c", nil, nil, names, nil); err == nil || ready {
				t.Fatal("mandatory query consumed optional result authority")
			}
		})
	}
}

func TestKbuildCompilerGuardOptionalDefinednessAuthenticatesCurrentDependencies(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	evaluator := scopes.evaluators["target"]
	argument, err := evaluator.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "flags", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "flags", Stream: "stdout", RequireSuccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, units, environment := []string{argument, "source.c"}, []string{"source.c"}, map[string]string{"MODE": argument}
	if _, _, err := discovery.OptionalCompilerDefinedness("target", "cc", "c", args, units, []string{"__A"}, environment); err != nil {
		t.Fatal(err)
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != 2 {
		t.Fatalf("symbolic optional plan: %#v %v", plan, err)
	}
	terminal := plan.Nodes[len(plan.Nodes)-1]
	step := plan.Requests[terminal.RequestID].Steps[0]
	if !slices.Equal(terminal.Inputs, []string{before.Nodes[0].ID}) || len(step.ArgumentFragments) == 0 || len(step.EnvironmentFragments) != 1 {
		t.Fatal("optional query lost exact argument/environment dependency closure")
	}
	for _, test := range []string{"answered", "rejected", "missing current", "changed current", "spoof current", "missing supplemental"} {
		t.Run(test, func(t *testing.T) {
			oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{"flags": "-std=gnu11", "compiler-definedness": "0"})
			dependencyID := before.Nodes[0].ID
			current := &ProbeResultOracle{results: map[string]ProbeResult{dependencyID: oracle.results[dependencyID]}, toolsets: maps.Clone(plan.Toolsets)}
			// Rejection must authenticate current dependencies just as strictly
			// as successful answers; it is not a missing-result escape hatch.
			if test != "answered" {
				result := oracle.results[terminal.ID]
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				oracle.results[terminal.ID] = result
			}
			switch test {
			case "missing current":
				delete(current.results, dependencyID)
			case "changed current", "spoof current":
				result := current.results[dependencyID]
				result.Steps = slices.Clone(result.Steps)
				result.Text = "-std=c11"
				if test == "changed current" {
					result.Steps[0].Stdout = result.Text
				}
				current.results[dependencyID] = result
			case "missing supplemental":
				delete(oracle.results, dependencyID)
			}
			evaluator.oracle = current
			replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			values, state, reference, err := replay.OptionalCompilerDefinednessAttempt("target", "cc", "c", args, units, []string{"__A"}, environment)
			valid := test == "answered" || test == "rejected"
			if !valid && reference != (ProbeReference{}) || valid &&
				(reference.NodeID != terminal.ID || reference.RequestID != terminal.RequestID || reference.Scope != "target" || reference.Kind != "boolean") {
				t.Fatalf("optional current-context reference = %#v", reference)
			}
			if (err == nil) != valid || !valid && (state != OptionalCompilerDefinednessPending || values != nil) {
				t.Fatalf("optional current-context replay = %#v %v %v", values, state, err)
			}
			if test == "answered" && (state != OptionalCompilerDefinednessAnswered || !maps.Equal(values, map[string]bool{"__A": false})) ||
				test == "rejected" && (state != OptionalCompilerDefinednessUnqueryable || values != nil) {
				t.Fatalf("wrong optional completion: %#v %v", values, state)
			}
			if _, err := replay.Answers(); (err == nil) != valid {
				t.Fatalf("optional snapshot authority = %v", err)
			}
		})
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("symbolic optional query changed ordinary registration")
	}
}

func TestKbuildCompilerGuardDependencyResourceErrorsRemainDistinct(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	evaluator := scopes.evaluators["target"]
	symbol, err := evaluator.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "flags", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "flags", Stream: "stdout", RequireSuccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	reference := evaluator.References()[0]
	newBatch := func() *KbuildCompilerGuardBatch {
		batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	fill := func(batch *KbuildCompilerGuardBatch) {
		// Isolate the cumulative traversal limit without constructing 4096
		// irrelevant requests. This synthetic map is never published.
		for index := range maxLinuxProbeResolveNodes {
			batch.imported[fmt.Sprint(index)] = ProbeReference{}
		}
	}
	for _, kind := range []string{"count", "depth", "cycle", "missing", "changed reference"} {
		t.Run(kind, func(t *testing.T) {
			batch := newBatch()
			visiting, current, depth := map[string]bool{}, reference, 0
			if kind != "depth" {
				fill(batch)
			}
			switch kind {
			case "depth":
				depth = maxLinuxProbeResolveDepth
			case "cycle":
				visiting[reference.NodeID] = true
			case "missing":
				current.NodeID = strings.Repeat("f", 64)
			case "changed reference":
				current.Kind = "boolean"
			}
			err := batch.importDependency(current, visiting, depth)
			var limit *CompilerGuardDependencyLimitError
			if err == nil || errors.As(fmt.Errorf("wrapped: %w", err), &limit) != (kind == "count" || kind == "depth") {
				t.Fatalf("malformed/cyclic input was confused with a resource ceiling: %v", err)
			}
		})
	}
	for _, optional := range []bool{false, true} {
		batch := newBatch()
		fill(batch)
		if optional {
			values, state, reference, err := batch.OptionalCompilerDefinednessAttempt("target", "cc", "c", []string{symbol}, nil, []string{"__A"}, nil)
			var limit *CompilerGuardDependencyLimitError
			if !errors.As(err, &limit) || values != nil || state != OptionalCompilerDefinednessPending || reference != (ProbeReference{}) {
				t.Fatalf("optional limit published pending authority: %#v %v %v", reference, state, err)
			}
		} else {
			values, ready, err := batch.CompilerDefinedness("target", "cc", "c", []string{symbol}, nil, []string{"__A"}, nil)
			var limit *CompilerGuardDependencyLimitError
			if !errors.As(err, &limit) || values != nil || ready {
				t.Fatalf("mandatory resource failure was relaxed: %#v %t %v", values, ready, err)
			}
		}
		if plan, err := batch.Plan(); err == nil || plan != nil {
			t.Fatal("failed dependency registration published a plan")
		}
		if answers, err := batch.Answers(); err == nil || answers != nil {
			t.Fatal("failed dependency registration published facts")
		}
	}
}

func TestKbuildCompilerGuardOptionalAttemptReferenceDeduplicates(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, state, first, err := batch.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"__B", "__A", "__B"}, nil)
	if err != nil || state != OptionalCompilerDefinednessPending || values != nil || first.NodeID == "" || first.RequestID == "" || first.Kind != "boolean" || first.Scope != "target" {
		t.Fatalf("discovery reference = %#v %#v %v %v", values, first, state, err)
	}
	_, _, second, err := batch.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"__A", "__B"}, nil)
	if err != nil || second != first {
		t.Fatalf("deduplicated reference = %#v %v; want %#v", second, err, first)
	}
	if values, state, err := batch.OptionalCompilerDefinedness("target", "cc", "c", nil, nil, []string{"__A", "__B"}, nil); err != nil || state != OptionalCompilerDefinednessPending || values != nil {
		t.Fatalf("legacy wrapper = %#v %v %v", values, state, err)
	}
	plan, err := batch.Plan()
	if err != nil || len(plan.Nodes) != 1 || len(plan.Requests) != 1 || !slices.Equal(plan.Terminal, []string{first.NodeID}) || plan.Nodes[0].RequestID != first.RequestID {
		t.Fatalf("one canonical terminal = %#v %v", plan, err)
	}
	if _, _, reference, err := batch.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"__A"}, nil); err == nil || reference != (ProbeReference{}) {
		t.Fatalf("frozen attempt published reference: %#v %v", reference, err)
	}
	for _, candidate := range []*KbuildCompilerGuardBatch{nil, {}} {
		if _, _, reference, err := candidate.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"__A"}, nil); err == nil || reference != (ProbeReference{}) {
			t.Fatalf("invalid batch published reference: %#v %v", reference, err)
		}
	}
	fresh, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, reference, err := fresh.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"not-a-name"}, nil); err == nil || reference != (ProbeReference{}) {
		t.Fatalf("invalid demand published reference: %#v %v", reference, err)
	}
}
