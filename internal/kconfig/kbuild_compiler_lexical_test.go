package kconfig

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

type compilerLexicalTestValue struct {
	punctuation, ready bool
}

func compilerLexicalTestOracle(t *testing.T, plan *ProbePlan, step ProbeStepResult, positive bool) *ProbeResultOracle {
	t.Helper()
	oracle := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(plan.Toolsets)}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if len(request.Steps) != 1 {
			t.Fatal("lexical fixture requires one-step requests")
		}
		name := request.Steps[0].Name
		result := ProbeResult{Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope]}
		if name == "compiler-dollar-punctuation" {
			step.Name = name
			value := positive
			result.Kind, result.Boolean, result.Steps = "boolean", &value, []ProbeStepResult{step}
		} else if name == "query-input" {
			result.Kind, result.Text = "text", "-fno-dollars-in-identifiers"
			result.Steps = []ProbeStepResult{{Name: name, Status: "success", Stdout: result.Text}}
		} else {
			t.Fatalf("unexpected lexical fixture step %q", name)
		}
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
		oracle.results[node.ID] = result
	}
	return oracle
}

func TestKbuildProbeScopesCompilerDollarPunctuationCanonicalRequestAndReplay(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	arguments := []string{"-nostdinc", "-DKBUILD_MODFILE=drivers/example", "-fdollars-in-identifiers", "-fno-dollars-in-identifiers", "-include", "forced.h", "source.c"}
	units := []string{"source.c", "source.c"}
	environment := map[string]string{"COMPILER_MODE": "exact"}
	originalArguments, originalUnits, originalEnvironment := slices.Clone(arguments), slices.Clone(units), maps.Clone(environment)
	workload := func(scopes *KbuildProbeScopes) (compilerLexicalTestValue, error) {
		value, ready, err := scopes.CompilerDollarPunctuation("target", "cc", "assembler-with-cpp", arguments, units, environment)
		return compilerLexicalTestValue{value, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.punctuation || discovery.Value.ready || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("lexical discovery = %#v", discovery)
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	want := ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{
		Name: "compiler-dollar-punctuation", Tool: "cc",
		Arguments: []string{"-nostdinc", "-DKBUILD_MODFILE=1", "-fdollars-in-identifiers", "-fno-dollars-in-identifiers", "-include", "forced.h", "source.c", "-E", "-P", "-x", "assembler-with-cpp", "-"},
		Stdin:     "#undef linux_bzl_dollar_punctuation_v1\n#define linux_bzl_dollar_punctuation_v1(a) #a$\nlinux_bzl_dollar_punctuation_v1(731)\n",
		Candidate: &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerPredefines,
			Base: []int{0, 1, 2, 3, 4, 5, 6}, TranslationUnits: []string{"source.c"}},
		Environment: map[string]string{"COMPILER_MODE": "exact"},
	}}, Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
		{Operator: "exit-zero", Step: "compiler-dollar-punctuation"},
		{Operator: "stream-matches", Step: "compiler-dollar-punctuation", Stream: "stdout", Value: `^[ \t\r\n\v\f]*"731"[ \t\r\n\v\f]*\$[ \t\r\n\v\f]*$`},
	}}}}
	gotJSON, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := want.CanonicalJSON()
	if err != nil || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("lexical request changed: %v\ngot %s\nwant %s", err, gotJSON, wantJSON)
	}
	projected, _, err := ProjectProbeCandidateArguments(request.Steps[0].Candidate.Projection, request.Steps[0].Arguments[:7], request.Steps[0].Candidate.TranslationUnits)
	if err != nil || !slices.Equal(projected, []string{"-nostdinc", "-DKBUILD_MODFILE=1", "-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}) {
		t.Fatalf("lexical projection changed original flag ordering: %q %v", projected, err)
	}
	if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, projected); err != nil {
		t.Fatal(err)
	}
	oracle := compilerLexicalTestOracle(t, discovery.Plan, ProbeStepResult{Status: "success", Stdout: " \t\"731\"\v$\r\n\f"}, true)
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || !replay.Value.punctuation || !replay.Value.ready || replay.Plan.Nodes[0].ID != node.ID {
		t.Fatalf("lexical replay = %#v %v", replay, err)
	}
	if !slices.Equal(arguments, originalArguments) || !slices.Equal(units, originalUnits) || !maps.Equal(environment, originalEnvironment) {
		t.Fatal("lexical query mutated caller inputs")
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationIdentity(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	options.Target.Tools = maps.Clone(options.Target.Tools)
	options.Target.Tools["cxx"] = options.Target.Tools["cc"]
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	seen := map[string]string{}
	for _, test := range []struct {
		name, scope, role, language string
		arguments                   []string
		environment                 map[string]string
	}{
		{"C", "target", "cc", "c", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, nil},
		{"assembly", "target", "cc", "assembler-with-cpp", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, nil},
		{"C++", "target", "cc", "c++", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, nil},
		{"reversed", "target", "cc", "c", []string{"-fno-dollars-in-identifiers", "-fdollars-in-identifiers"}, nil},
		{"environment", "target", "cc", "c", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, map[string]string{"MODE": "alternate"}},
		{"role", "target", "cxx", "c", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, nil},
		{"scope", "host", "cc", "c", []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (compilerLexicalTestValue, error) {
				value, ready, err := scopes.CompilerDollarPunctuation(test.scope, test.role, test.language, test.arguments, nil, test.environment)
				return compilerLexicalTestValue{value, ready}, err
			})
			if err != nil || evaluation.Value.ready || len(evaluation.Plan.Nodes) != 1 {
				t.Fatalf("identity discovery = %#v %v", evaluation, err)
			}
			id := evaluation.Plan.Nodes[0].ID
			if previous, exists := seen[id]; exists {
				t.Fatalf("%s shares lexical request identity with %s", test.name, previous)
			}
			seen[id] = test.name
		})
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationNegativeAndSpoofedResults(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	var observed compilerLexicalTestValue
	workload := func(scopes *KbuildProbeScopes) (compilerLexicalTestValue, error) {
		value, ready, err := scopes.CompilerDollarPunctuation("target", "cc", "c", nil, nil, nil)
		observed = compilerLexicalTestValue{value, ready}
		return observed, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		step ProbeStepResult
		want bool
	}{
		{"positive", ProbeStepResult{Status: "success", Stdout: "\"731\"$\n"}, true},
		{"syntax rejection", ProbeStepResult{Status: "failure", ExitCode: 1, Stderr: "unsupported definition"}, false},
		{"failed with matching stdout", ProbeStepResult{Status: "failure", ExitCode: 1, Stdout: "\"731\"$\n"}, false},
		{"assembler unchanged operator", ProbeStepResult{Status: "success", Stdout: "#a$\n"}, false},
		{"empty", ProbeStepResult{Status: "success"}, false},
		{"extra token", ProbeStepResult{Status: "success", Stdout: "\"731\"$ EXTRA\n"}, false},
		{"inside literal whitespace", ProbeStepResult{Status: "success", Stdout: "\"7 31\"$\n"}, false},
		{"non-ASCII whitespace", ProbeStepResult{Status: "success", Stdout: "\"731\"$\u00a0"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, spoof := range []bool{false, true} {
				summary := test.want != spoof
				oracle := compilerLexicalTestOracle(t, discovery.Plan, test.step, summary)
				replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
				if spoof {
					if err == nil || observed.ready || observed.punctuation {
						t.Fatalf("spoofed result admitted: %#v %v", observed, err)
					}
				} else if err != nil || !replay.Value.ready || replay.Value.punctuation != test.want {
					t.Fatalf("measurement = %#v %v, want ready=%t", replay, err, test.want)
				}
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationUnavailableOracle(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerLexicalTestValue, error) {
		value, ready, err := scopes.CompilerDollarPunctuation("target", "cc", "c", nil, nil, nil)
		if err != nil && (value || ready) {
			t.Fatal("oracle error returned partial lexical authority")
		}
		return compilerLexicalTestValue{value, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := discovery.Plan.Nodes[0].ID
	for _, test := range []struct {
		name   string
		mutate func(*ProbeResultOracle)
	}{
		{"missing", func(oracle *ProbeResultOracle) { delete(oracle.results, nodeID) }},
		{"stale request", func(oracle *ProbeResultOracle) {
			r := oracle.results[nodeID]
			r.RequestID = strings.Repeat("0", 64)
			oracle.results[nodeID] = r
		}},
		{"wrong toolset", func(oracle *ProbeResultOracle) {
			r := oracle.results[nodeID]
			r.ToolsetIdentity = strings.Repeat("0", 64)
			oracle.results[nodeID] = r
		}},
		{"missing step", func(oracle *ProbeResultOracle) {
			r := oracle.results[nodeID]
			r.Steps = nil
			oracle.results[nodeID] = r
		}},
		{"skipped step", func(oracle *ProbeResultOracle) {
			r := oracle.results[nodeID]
			r.Steps[0].Status = "skipped"
			oracle.results[nodeID] = r
		}},
		{"missing Boolean", func(oracle *ProbeResultOracle) {
			r := oracle.results[nodeID]
			r.Boolean = nil
			oracle.results[nodeID] = r
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := compilerLexicalTestOracle(t, discovery.Plan, ProbeStepResult{Status: "success", Stdout: "\"731\"$\n"}, true)
			test.mutate(oracle)
			_, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if err == nil || errors.As(err, &unsupported) {
				t.Fatalf("malformed oracle was not a real error: %v", err)
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationSymbolicArgumentsAndEnvironment(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerLexicalTestValue, error) {
		measured, err := scopes.evaluators["target"].requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "query-input", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{Kind: "text", Step: "query-input", Stream: "stdout", RequireSuccess: true},
		})
		if err != nil {
			return compilerLexicalTestValue{}, err
		}
		value, ready, err := scopes.CompilerDollarPunctuation("target", "cc", "c", []string{measured, "source.c"}, []string{"source.c"}, map[string]string{"MODE": measured})
		return compilerLexicalTestValue{value, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || discovery.Value.ready || len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("symbolic lexical discovery = %#v %v", discovery, err)
	}
	for _, request := range discovery.Plan.Requests {
		step := request.Steps[0]
		if step.Name != "compiler-dollar-punctuation" {
			continue
		}
		if request.InputCount != 1 || step.Candidate == nil || step.Candidate.Projection != ProbeCandidateProjectionCompilerPredefines ||
			len(step.ArgumentFragments) == 0 || len(step.Environment) != 0 || len(step.EnvironmentFragments) != 1 || step.EnvironmentFragments[0].Name != "MODE" {
			t.Fatalf("symbolic lexical request lost ownership: %#v", request)
		}
	}
	oracle := compilerLexicalTestOracle(t, discovery.Plan, ProbeStepResult{Status: "success", Stdout: "\"731\"$\n"}, true)
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || !replay.Value.punctuation || !replay.Value.ready || !slices.EqualFunc(discovery.Plan.Nodes, replay.Plan.Nodes, func(a, b ProbePlanNode) bool { return a.ID == b.ID }) {
		t.Fatalf("symbolic lexical replay changed request identity: %#v %v", replay, err)
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationRejectsUnboundEnvironment(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, input := range []string{"${work:root}/include", "${tree:prep}/include", "${input:source:00000000}"} {
		t.Run(input, func(t *testing.T) {
			environment := map[string]string{"MODE": input}
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				value, ready, err := scopes.CompilerDollarPunctuation("target", "cc", "c", nil, nil, environment)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if !errors.As(err, &unsupported) || value || ready {
					t.Fatalf("unbound lexical result: %t %t %v", value, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil || len(evaluation.Plan.Nodes) != 0 || environment["MODE"] != input {
				t.Fatalf("unbound environment registered request or mutated input: %#v %v", evaluation, err)
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationUnavailableConfiguration(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, test := range []struct{ scope, role, language string }{
		{"unconfigured", "cc", "c"}, {"target", "ld", "c"}, {"target", "cc", "rust"}, {"target", "cxx", "c++"},
	} {
		t.Run(test.scope+"/"+test.role+"/"+test.language, func(t *testing.T) {
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				value, ready, err := scopes.CompilerDollarPunctuation(test.scope, test.role, test.language, nil, nil, nil)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if err == nil || errors.As(err, &unsupported) || value || ready {
					t.Fatalf("invalid lexical configuration = %t %t %v", value, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil || len(evaluation.Plan.Nodes) != 0 {
				t.Fatalf("invalid configuration registered partial request: %#v %v", evaluation, err)
			}
		})
	}
	var missing *KbuildProbeScopes
	if value, ready, err := missing.CompilerDollarPunctuation("target", "cc", "c", nil, nil, nil); err == nil || value || ready {
		t.Fatalf("nil lexical scope = %t %t %v", value, ready, err)
	}
}

func TestKbuildProbeScopesCompilerDollarPunctuationMetadataBinding(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		for range 2 {
			metadata := &CompactMetadata{}
			if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
				return struct{}{}, err
			}
			if metadata.compilerDollarPunctuation == nil {
				t.Fatal("metadata lacks lexical capability callback")
			}
			value, ready, err := metadata.compilerDollarPunctuation("target", "cc", "assembler-with-cpp", nil, nil, nil)
			if err != nil || value || ready {
				t.Fatalf("bound lexical discovery = %t %t %v", value, ready, err)
			}
		}
		return struct{}{}, nil
	})
	if err != nil || len(evaluation.Plan.Nodes) != 1 {
		t.Fatalf("bound lexical requests did not deduplicate: %#v %v", evaluation, err)
	}
}
