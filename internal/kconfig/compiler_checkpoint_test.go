package kconfig

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompilerCheckpointReplaysPrunedPureDefinitions(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		e := scopes.evaluators["target"]
		if _, err := e.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: "measured", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{Kind: "text", Step: "measured", Stream: "stdout", RequireSuccess: true}}); err != nil {
			return "", err
		}
		if _, err := e.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema, InputCount: 1,
			Outcome: ProbeOutcome{Kind: "text", Result: "00000000", Word: 1}}, e.References()[0]); err != nil {
			return "", err
		}
		return e.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema, InputCount: 1,
			Outcome: ProbeOutcome{Kind: "text", Fragments: []ProbeValueFragment{{Value: "prefix/"}, {Value: "${result:00000000.text}"}}}}, e.References()[1])
	}
	scopes := compilerGuardBatchScopesForTest(t, options)
	if _, err := workload(scopes); err != nil {
		t.Fatal(err)
	}
	data, err := scopes.MarshalActionPlanCompilerCheckpoint([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var record kbuildCompilerCheckpoint
	if err := json.Unmarshal(data, &record); err != nil || len(record.Symbols) != 0 || len(record.Plan.Nodes) != 3 {
		t.Fatalf("fixture did not prune only symbolic aliases: %v", err)
	}
	root, terminal := record.Plan.Nodes[0], record.Plan.Nodes[2]
	newOracle := func(value string) *ProbeResultOracle {
		return &ProbeResultOracle{toolsets: maps.Clone(record.Plan.Toolsets), results: map[string]ProbeResult{
			root.ID: {Schema: LinuxProbeResultSchema, NodeID: root.ID, RequestID: root.RequestID,
				Scope: root.Scope, ToolsetIdentity: record.Plan.Toolsets[root.Scope], Kind: "text", Text: value,
				Steps: []ProbeStepResult{{Name: "measured", Status: "success", Stdout: value}}},
		}}
	}
	for _, value := range []string{"first value", "changed value"} {
		t.Run(value, func(t *testing.T) {
			freshOracle := newOracle(value)
			fresh, err := EvaluateKbuildProbeWorkload(options, freshOracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			current := newOracle(value)
			restored, err := EvaluateKbuildProbeWorkload(options, current, func(s *KbuildProbeScopes) (string, error) {
				return "", s.RestoreCompilerCheckpoint(data, nil)
			})
			if err != nil {
				t.Fatalf("restore lost pure results for pruned symbols: %v", err)
			}
			if !reflect.DeepEqual(restored.Plan, fresh.Plan) || !reflect.DeepEqual(current.results, freshOracle.results) {
				t.Fatal("checkpoint replay differs from fresh source evaluation")
			}
			if current.results[terminal.ID].Text != "prefix/"+strings.Fields(value)[0] {
				t.Fatal("checkpoint did not use the current measured value")
			}
		})
	}
	for _, defect := range []string{"missing", "stale request", "wrong scope"} {
		t.Run(defect, func(t *testing.T) {
			oracle := newOracle("value")
			result := oracle.results[root.ID]
			switch defect {
			case "missing":
				delete(oracle.results, root.ID)
			case "stale request":
				result.RequestID = strings.Repeat("a", 64)
				oracle.results[root.ID] = result
			case "wrong scope":
				result.Scope = "host"
				oracle.results[root.ID] = result
			}
			if _, err := EvaluateKbuildProbeWorkload(options, oracle, func(s *KbuildProbeScopes) (string, error) {
				return "", s.RestoreCompilerCheckpoint(data, nil)
			}); err == nil {
				t.Fatal("checkpoint accepted a missing or stale measured observation")
			}
		})
	}
}

func TestCompilerCheckpointPreservesScopeAdoption(t *testing.T) {
	for _, test := range []struct {
		producer, consumer string
		allowed            bool
	}{
		{"target", "target", true}, {"host", "target", true}, {"host", "host", true}, {"target", "host", false},
	} {
		t.Run(test.producer+"-to-"+test.consumer, func(t *testing.T) {
			options := compilerDefinednessTestOptions(t)
			host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
			options.Host = &host
			scopes := compilerGuardBatchScopesForTest(t, options)
			token, err := scopes.evaluators[test.producer].requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
				Steps:   []ProbeStep{{Name: "source-flags", Tool: "cc", Arguments: []string{"--version"}}},
				Outcome: ProbeOutcome{Kind: "text", Step: "source-flags", Stream: "stdout", RequireSuccess: true}})
			if err != nil {
				t.Fatal(err)
			}
			query := func(current *KbuildProbeScopes) (*ProbePlan, error) {
				batch, err := NewKbuildCompilerGuardBatch(current, nil)
				if err != nil {
					return nil, err
				}
				if _, _, err := batch.CompilerDefinedness(test.consumer, "cc", "c", []string{token}, nil, []string{"__GUARD"}, nil); err != nil {
					return nil, err
				}
				return batch.Plan()
			}
			want, wantErr := query(scopes)
			if (wantErr == nil) != test.allowed {
				t.Fatalf("invalid scope fixture: %v", wantErr)
			}
			restored := checkpointCompilerScopesForTest(t, scopes)
			got, gotErr := query(restored)
			if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("checkpoint changed scope admission: %v != %v", gotErr, wantErr)
			}
			if restored.evaluators[test.producer] == scopes.evaluators[test.producer] || restored.evaluators[test.producer].symbolRegistry == scopes.evaluators[test.producer].symbolRegistry {
				t.Fatal("checkpoint retained original evaluator or registry")
			}
		})
	}
}

func checkpointCompilerScopesForTest(t *testing.T, original *KbuildProbeScopes) *KbuildProbeScopes {
	t.Helper()
	data, err := original.MarshalCompilerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	scopeOptions := func(e *LinuxProbeEvaluator) KbuildProbeScopeOptions {
		return KbuildProbeScopeOptions{Architecture: e.architecture, SourceArchitecture: e.sourceArchitecture, SourceRoot: e.sourceRoot, ScriptEnvironment: maps.Clone(original.baseScriptEnvironments[e.scope]), Facts: e.facts, Tools: maps.Clone(e.tools), RustSourceRoot: e.rustSourceRoot}
	}
	options := KbuildProbeWorkloadOptions{Target: scopeOptions(original.evaluators["target"])}
	if host := original.evaluators["host"]; host != nil {
		h := scopeOptions(host)
		options.Host = &h
	}
	evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(fresh *KbuildProbeScopes) (*KbuildProbeScopes, error) {
		if err := fresh.RestoreCompilerCheckpoint(data, nil); err != nil {
			return nil, err
		}
		for name, previous := range original.evaluators {
			fresh.evaluators[name].oracle = previous.oracle
		}
		return fresh, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluation.Value
}

func TestCompilerCheckpointImportsSymbolicDependencyClosure(t *testing.T) {
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
	scopes = checkpointCompilerScopesForTest(t, scopes)
	evaluator = scopes.evaluators["target"]
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
