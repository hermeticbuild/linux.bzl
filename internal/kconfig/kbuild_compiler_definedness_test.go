package kconfig

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

type compilerDefinednessTestValue struct {
	definitions map[string]bool
	ready       bool
}

func compilerDefinednessTestOptions(t *testing.T) KbuildProbeWorkloadOptions {
	t.Helper()
	return KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[1])}
}

// The oracle contains execution-shaped results, not preinterpreted answers.
// Replay must still check the successful step's exact stdout and parse every
// Boolean token before it can return even one negative definedness witness.
func compilerDefinednessTestOracle(t *testing.T, plan *ProbePlan, textByStep map[string]string) *ProbeResultOracle {
	t.Helper()
	oracle := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(plan.Toolsets)}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if len(request.Steps) != 1 {
			t.Fatalf("fixture request has %d steps, want one", len(request.Steps))
		}
		name := request.Steps[0].Name
		contents, ok := textByStep[name]
		if !ok {
			t.Fatalf("fixture has no result for step %q", name)
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "text", Text: contents,
			Steps: []ProbeStepResult{{Name: name, Status: "success", ExitCode: 0, Stdout: contents}},
		}
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
		oracle.results[node.ID] = result
	}
	return oracle
}

func TestKbuildProbeScopesCompilerDefinednessCanonicalRequestAndReplay(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	arguments := []string{"--target=x86_64-linux-gnu", "-std=gnu11", "-DKBUILD_MODFILE=drivers/example/module", "-include", "forced.h", "source.c"}
	translationUnits := []string{"source.c", "source.c"}
	names := []string{"__B_PRESENT", "Z9", "__A_ABSENT", "__B_PRESENT"}
	environment := map[string]string{"COMPILER_MODE": "exact"}
	originalArguments, originalUnits, originalNames := slices.Clone(arguments), slices.Clone(translationUnits), slices.Clone(names)
	originalEnvironment := maps.Clone(environment)
	workload := func(scopes *KbuildProbeScopes) (compilerDefinednessTestValue, error) {
		definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", arguments, translationUnits, names, environment)
		return compilerDefinednessTestValue{definitions, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.ready || discovery.Value.definitions != nil || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("discovery = %#v, nodes = %d; want nil, unresolved, one request", discovery.Value, len(discovery.Plan.Nodes))
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	want := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "compiler-definedness", Tool: "cc",
			Arguments: []string{"--target=x86_64-linux-gnu", "-std=gnu11", "-DKBUILD_MODFILE=1", "-include", "forced.h", "source.c", "-E", "-P", "-x", "c", "-"},
			Stdin:     "#if defined(Z9)\n1\n#else\n0\n#endif\n#if defined(__A_ABSENT)\n1\n#else\n0\n#endif\n#if defined(__B_PRESENT)\n1\n#else\n0\n#endif\n",
			Candidate: &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerPredefines,
				Base: []int{0, 1, 2, 3, 4, 5}, TranslationUnits: []string{"source.c"}},
			Environment: map[string]string{"COMPILER_MODE": "exact"},
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "compiler-definedness", Stream: "stdout", RequireSuccess: true},
	}
	gotJSON, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := want.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("definedness request changed:\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
	projected, _, err := ProjectProbeCandidateArguments(request.Steps[0].Candidate.Projection,
		request.Steps[0].Arguments[:6], request.Steps[0].Candidate.TranslationUnits)
	if err != nil || !slices.Equal(projected, []string{"--target=x86_64-linux-gnu", "-std=gnu11", "-DKBUILD_MODFILE=1"}) {
		t.Fatalf("initial-state projection = %#v, err = %v", projected, err)
	}
	if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, projected); err != nil {
		t.Fatal(err)
	}
	// Every ASCII preprocessing whitespace separator is accepted. The negative
	// answer must be present in the map, not conflated with an unqueried name.
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-definedness": " \t1\r\n0\v1\f"})
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	wantDefinitions := map[string]bool{"Z9": true, "__A_ABSENT": false, "__B_PRESENT": true}
	if !replay.Value.ready || !maps.Equal(replay.Value.definitions, wantDefinitions) {
		t.Fatalf("replay = %#v, want ready with %#v", replay.Value, wantDefinitions)
	}
	if len(replay.Plan.Nodes) != 1 || replay.Plan.Nodes[0].ID != node.ID {
		t.Fatalf("replay changed discovery identity: %#v", replay.Plan.Nodes)
	}
	if !slices.Equal(arguments, originalArguments) || !slices.Equal(translationUnits, originalUnits) ||
		!slices.Equal(names, originalNames) || !maps.Equal(environment, originalEnvironment) {
		t.Fatal("initial-state query mutated caller-owned inputs")
	}
	// Names are a set in request identity, but changing the set cannot reuse
	// the old vector. Mutating a returned map cannot mutate the oracle either.
	names = []string{"__A_ABSENT", "__B_PRESENT", "Z9"}
	replay.Value.definitions["__A_ABSENT"] = true
	replay, err = EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || !maps.Equal(replay.Value.definitions, wantDefinitions) || replay.Plan.Nodes[0].ID != node.ID {
		t.Fatalf("canonical-name replay = %#v, err = %v", replay, err)
	}
	names = []string{"__A_ABSENT", "__B_PRESENT", "Z8"}
	changed, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Plan.Nodes[0].ID == node.ID {
		t.Fatal("changing a queried name retained the old request identity")
	}
}

func TestKbuildProbeScopesCompilerDefinednessEmptyNamesStillQueries(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	var names []string
	workload := func(scopes *KbuildProbeScopes) (compilerDefinednessTestValue, error) {
		definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", nil, nil, names, nil)
		return compilerDefinednessTestValue{definitions, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.ready || discovery.Value.definitions != nil || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("empty-name discovery = %#v", discovery)
	}
	node := discovery.Plan.Nodes[0]
	step := discovery.Plan.Requests[node.RequestID].Steps[0]
	if step.Stdin != "" || !slices.Equal(step.Arguments, []string{"-E", "-P", "-x", "c", "-"}) {
		t.Fatalf("empty-name request = %#v", step)
	}
	names = []string{}
	for _, contents := range []string{"", " \t\r\n\v\f"} {
		oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-definedness": contents})
		replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
		if err != nil {
			t.Fatal(err)
		}
		if !replay.Value.ready || replay.Value.definitions == nil || len(replay.Value.definitions) != 0 || replay.Plan.Nodes[0].ID != node.ID {
			t.Fatalf("empty-name replay = %#v", replay)
		}
	}
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-definedness": "0"})
	if _, err := EvaluateKbuildProbeWorkload(options, oracle, workload); err == nil {
		t.Fatal("empty-name query accepted an unexpected result token")
	}
}

func TestKbuildProbeScopesCompilerDefinednessRejectsNamesBeforeRegistration(t *testing.T) {
	tooMany := make([]string, MaxProbeDynamicArgumentWords+1)
	for index := range tooMany {
		tooMany[index] = "_VALID"
	}
	duplicateLargeName := strings.Repeat("A", MaxProbeInterpolatedBytes/2+1)
	tests := []struct {
		name  string
		names []string
	}{
		{"empty identifier", []string{""}},
		{"leading digit", []string{"0GUARD"}},
		{"embedded whitespace", []string{"A B"}},
		{"trailing whitespace", []string{"A "}},
		{"directive injection", []string{"A\n#error injected"}},
		{"expression injection", []string{"A) || defined(B"}},
		{"comment", []string{"A/**/B"}},
		{"NUL", []string{"A\x00B"}},
		{"non-ASCII", []string{"Å"}},
		{"punctuation", []string{"A-B"}},
		{"raw count before deduplication", tooMany},
		{"raw bytes before deduplication", []string{duplicateLargeName, duplicateLargeName}},
		{"canonical stdin bytes", []string{strings.Repeat("A", MaxProbeInterpolatedBytes)}},
	}
	options := compilerDefinednessTestOptions(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := slices.Clone(test.names)
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", nil, nil, test.names, nil)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if !errors.As(err, &unsupported) || definitions != nil || ready {
					t.Fatalf("invalid-name result = %#v, ready = %v, err = %v; want typed unavailable projection", definitions, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(evaluation.Plan.Nodes) != 0 || len(evaluation.Plan.Requests) != 0 {
				t.Fatal("invalid name vector registered a partial request")
			}
			if !slices.Equal(test.names, original) {
				t.Fatal("invalid-name validation mutated caller input")
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDefinednessRejectsMalformedResults(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	var observed compilerDefinednessTestValue
	workload := func(scopes *KbuildProbeScopes) (compilerDefinednessTestValue, error) {
		definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"A", "B"}, nil)
		observed = compilerDefinednessTestValue{definitions, ready}
		return observed, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, contents string }{
		{"empty", ""}, {"missing", "0"}, {"extra", "0 1 0"},
		{"non-Boolean", "0 2"}, {"negative", "0 -1"}, {"padded token", "0 01"},
		{"suffix", "0 1U"}, {"word", "0 true"}, {"diagnostic", "0 1\nwarning"},
		{"non-ASCII separator", "0\u00a01"}, {"non-ASCII trailing whitespace", "0 1\u2003"},
		{"invalid UTF-8", "0 \xff1"}, {"NUL", "0 1\x00"},
		{"retained comment", "/* license version 1 */\n0 1\n"},
		{"Boolean-looking comment", "/* 0 1 */\n0 1\n"},
		{"oversized", "0 1" + strings.Repeat(" ", MaxProbeInterpolatedBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-definedness": test.contents})
			_, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if err == nil || errors.As(err, &unsupported) || observed.ready || observed.definitions != nil {
				t.Fatalf("malformed result = %#v, err = %v; want genuine error without partial answers", observed, err)
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDefinednessSourceExactByteBoundary(t *testing.T) {
	const framingBytes = len("#if defined()\n1\n#else\n0\n#endif\n")
	name := strings.Repeat("A", MaxProbeInterpolatedBytes-framingBytes)
	ordered, source, err := compilerDefinednessSource([]string{name})
	if err != nil || len(source) != MaxProbeInterpolatedBytes || !slices.Equal(ordered, []string{name}) {
		t.Fatalf("exact-limit source: names = %d, bytes = %d, err = %v", len(ordered), len(source), err)
	}
	ordered, source, err = compilerDefinednessSource([]string{name + "A"})
	if err == nil || ordered != nil || source != "" {
		t.Fatalf("one-byte-over source: names = %d, bytes = %d, err = %v; want no partial program", len(ordered), len(source), err)
	}
}

func TestKbuildProbeScopesCompilerDefinednessUnavailableConfiguration(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, test := range []struct {
		name     string
		scopes   func(*KbuildProbeScopes) *KbuildProbeScopes
		role     string
		language string
	}{
		{"nil scopes", func(*KbuildProbeScopes) *KbuildProbeScopes { return nil }, "cc", "c"},
		{"missing target", func(*KbuildProbeScopes) *KbuildProbeScopes { return &KbuildProbeScopes{} }, "cc", "c"},
		{"non-compiler role", func(scopes *KbuildProbeScopes) *KbuildProbeScopes { return scopes }, "ld", "c"},
		{"unsupported language", func(scopes *KbuildProbeScopes) *KbuildProbeScopes { return scopes }, "cc", "rust"},
		{"unconfigured compiler role", func(scopes *KbuildProbeScopes) *KbuildProbeScopes { return scopes }, "cxx", "c++"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				definitions, ready, err := test.scopes(scopes).CompilerDefinedness("target", test.role, test.language, nil, nil, []string{"_GUARD"}, nil)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if err == nil || errors.As(err, &unsupported) || definitions != nil || ready {
					t.Fatalf("unavailable configuration = %#v, ready = %v, err = %v; want genuine configuration error", definitions, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(evaluation.Plan.Nodes) != 0 || len(evaluation.Plan.Requests) != 0 {
				t.Fatal("unavailable configuration registered a partial request")
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDefinednessUnavailableResultsAreErrors(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerDefinednessTestValue, error) {
		definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"_GUARD"}, nil)
		if err != nil && (ready || definitions != nil) {
			t.Fatalf("unavailable query returned partial answers: %#v, ready = %v", definitions, ready)
		}
		return compilerDefinednessTestValue{definitions, ready}, err
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
		{"missing oracle entry", func(oracle *ProbeResultOracle) { delete(oracle.results, nodeID) }},
		{"stale request", func(oracle *ProbeResultOracle) {
			result := oracle.results[nodeID]
			result.RequestID = strings.Repeat("0", 64)
			oracle.results[nodeID] = result
		}},
		{"wrong toolset", func(oracle *ProbeResultOracle) {
			result := oracle.results[nodeID]
			result.ToolsetIdentity = strings.Repeat("0", 64)
			oracle.results[nodeID] = result
		}},
		{"failed step", func(oracle *ProbeResultOracle) {
			result := oracle.results[nodeID]
			result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			oracle.results[nodeID] = result
		}},
		{"missing step", func(oracle *ProbeResultOracle) {
			result := oracle.results[nodeID]
			result.Steps = nil
			oracle.results[nodeID] = result
		}},
		{"mismatching summary", func(oracle *ProbeResultOracle) {
			result := oracle.results[nodeID]
			result.Text = "1"
			oracle.results[nodeID] = result
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-definedness": "0"})
			test.mutate(oracle)
			_, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if err == nil || errors.As(err, &unsupported) {
				t.Fatalf("unavailable result error = %v, want genuine replay error", err)
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDefinednessSymbolicArgumentsAndEnvironment(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerDefinednessTestValue, error) {
		measured, err := scopes.evaluators["target"].requestText(ProbeRequest{
			Schema:  LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: "query-input", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{Kind: "text", Step: "query-input", Stream: "stdout", RequireSuccess: true},
		})
		if err != nil {
			return compilerDefinednessTestValue{}, err
		}
		definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c",
			[]string{measured, "source.c"}, []string{"source.c"}, []string{"_GUARD"}, map[string]string{"MODE": measured})
		return compilerDefinednessTestValue{definitions, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.ready || discovery.Value.definitions != nil || len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("symbolic discovery = %#v", discovery)
	}
	found := false
	for _, request := range discovery.Plan.Requests {
		step := request.Steps[0]
		if step.Name != "compiler-definedness" {
			continue
		}
		found = true
		if request.InputCount != 1 || step.Candidate == nil || step.Candidate.Projection != ProbeCandidateProjectionCompilerPredefines ||
			len(step.ArgumentFragments) == 0 || len(step.Environment) != 0 || len(step.EnvironmentFragments) != 1 || step.EnvironmentFragments[0].Name != "MODE" {
			t.Fatalf("symbolic initial-state request lost provenance: %#v", request)
		}
	}
	if !found {
		t.Fatal("symbolic discovery omitted the definedness request")
	}
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"query-input": "-DSELECTED=1", "compiler-definedness": "0"})
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Value.ready || !maps.Equal(replay.Value.definitions, map[string]bool{"_GUARD": false}) || len(replay.Plan.Nodes) != 2 {
		t.Fatalf("symbolic replay = %#v", replay)
	}
}

func TestKbuildProbeScopesCompilerDefinednessRejectsUnboundEnvironment(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, input := range []string{"${work:root}/include", "${tree:prep}/include", "${input:source:00000000}", "${tree:prep"} {
		t.Run(input, func(t *testing.T) {
			environment := map[string]string{"INCLUDE_ROOT": input, "MODE": "exact"}
			original := maps.Clone(environment)
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				definitions, ready, err := scopes.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"_GUARD"}, environment)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if !errors.As(err, &unsupported) || ready || definitions != nil {
					t.Fatalf("unbound environment result = %#v, ready = %v, err = %v", definitions, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(evaluation.Plan.Nodes) != 0 || !maps.Equal(environment, original) {
				t.Fatal("unsupported environment registered a partial request or mutated the executable environment")
			}
		})
	}
}

func TestKbuildProbeScopesCompilerDefinednessMetadataBindingLifetime(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	var previous *configDependencyGuardInventory
	for iteration := 0; iteration < 2; iteration++ {
		evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
			first, second := &CompactMetadata{}, &CompactMetadata{}
			for _, metadata := range []*CompactMetadata{first, second} {
				if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
					return struct{}{}, err
				}
				if metadata.compilerDefinedness == nil || metadata.compilerPredefines == nil || metadata.sourceGuardInventory == nil {
					t.Fatal("metadata binding omitted an initial-state callback or guard inventory")
				}
				definitions, ready, err := metadata.compilerDefinedness("target", "cc", "c", nil, nil, []string{"_GUARD"}, nil)
				if err != nil || ready || definitions != nil {
					t.Fatalf("bound callback discovery = %#v, ready = %v, err = %v", definitions, ready, err)
				}
			}
			if first.sourceGuardInventory != second.sourceGuardInventory || first.sourceGuardInventory != scopes.sourceGuardInventory {
				t.Fatal("fresh metadata values did not share their workload's guard inventory")
			}
			if first.sourceGuardInventory == previous {
				t.Fatal("independent workloads shared a guard inventory")
			}
			previous = first.sourceGuardInventory
			return struct{}{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(evaluation.Plan.Nodes) != 1 {
			t.Fatalf("bound callbacks did not deduplicate within one workload: %#v", evaluation.Plan.Nodes)
		}
	}
}

func TestKbuildProbeScopesCompilerDefinednessFactoringPreservesPredefineRequestBytes(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		_, _, err := scopes.CompilerPredefines("target", "cc", "c", []string{"-std=gnu11", "-DMODE=original"}, nil, map[string]string{"MODE": "exact"})
		return struct{}{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 1 {
		t.Fatalf("predefine requests = %d, want one", len(evaluation.Plan.Nodes))
	}
	want := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "compiler-predefines", Tool: "cc", Arguments: []string{"-std=gnu11", "-DMODE=1", "-dM", "-E", "-x", "c", "/dev/null"},
			Candidate:   &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerPredefines, Base: []int{0, 1}},
			Environment: map[string]string{"MODE": "exact"},
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "compiler-predefines", Stream: "stdout", RequireSuccess: true},
	}
	wantJSON, err := want.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := evaluation.Plan.Requests[evaluation.Plan.Nodes[0].RequestID].CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("legacy predefine request bytes changed:\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
}
