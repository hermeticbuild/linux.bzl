package kconfig

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

type compilerIntrinsicTestValue struct {
	token string
	ready bool
}

func TestCompilerIntrinsicIntegerSourceAndResult(t *testing.T) {
	for _, operand := range []string{"deprecated", "__retain__", "unknown_attribute_9", strings.Repeat("a", 128)} {
		source, err := compilerIntrinsicIntegerSource("__has_attribute", operand)
		if err != nil || source != "#undef "+operand+"\n__has_attribute("+operand+")\n" {
			t.Fatalf("source %q = %q, %v", operand, source, err)
		}
	}
	for _, call := range []CompilerIntrinsicCall{
		{"__has_include", "header"}, {"", "name"}, {"__has_attribute", "__has_attribute"},
		{"__has_attribute", "__has_builtin"}, {"__has_builtin", "__has_attribute"}, {"__has_builtin", "__has_builtin"},
		{"__has_attribute", ""}, {"__has_attribute", "1name"}, {"__has_attribute", "a b"},
		{"__has_attribute", "a,b"}, {"__has_attribute", "a(x)"}, {"__has_attribute", "a\n#error injected"},
		{"__has_attribute", "a$"}, {"__has_attribute", "é"}, {"__has_attribute", strings.Repeat("a", 129)},
	} {
		if err := ValidateCompilerIntrinsicCall(call); err == nil {
			t.Fatalf("accepted unsupported call %#v", call)
		}
		if source, err := compilerIntrinsicIntegerSource(call.Operator, call.Operand); err == nil || source != "" {
			t.Fatalf("invalid call returned source %q, %v", source, err)
		}
	}
	for _, token := range []string{"0", "1", "202311", "0x31647", "077", "202311L", "9223372036854775807LL"} {
		value, err := parseCompilerIntrinsicIntegerResult(" \t\r\n\v\f" + token + "\n")
		if err != nil || value != token {
			t.Fatalf("integer %q changed to %q, %v", token, value, err)
		}
	}
	for _, output := range []string{"", " ", "1 0", "202311\nwarning", "__has_attribute(deprecated)", "(1)", "-1", "+1", "1+2", "1u", "1.0", "0b1", "09", "9223372036854775808", "1\u00a0", "1\x00", strings.Repeat("1", 129), strings.Repeat(" ", MaxProbeInterpolatedBytes+1)} {
		if value, err := parseCompilerIntrinsicIntegerResult(output); err == nil || value != "" {
			t.Fatalf("accepted invalid integer output of length %d: %q, %v", len(output), value, err)
		}
	}
}

func TestCompilerIntrinsicBuiltinMixedVectorsAndIdentity(t *testing.T) {
	attribute := CompilerIntrinsicCall{"__has_attribute", "shared_name"}
	builtin := CompilerIntrinsicCall{"__has_builtin", "shared_name"}
	unknown := CompilerIntrinsicCall{"__has_builtin", "unknown_builtin_name"}
	calls := []CompilerIntrinsicCall{unknown, builtin, attribute, builtin}
	original := slices.Clone(calls)
	ordered, source, err := compilerIntrinsicIntegersSource(calls)
	wantSource := "#undef shared_name\n__has_attribute(shared_name)\n#undef shared_name\n__has_builtin(shared_name)\n#undef unknown_builtin_name\n__has_builtin(unknown_builtin_name)\n"
	if err != nil || !slices.Equal(ordered, []CompilerIntrinsicCall{attribute, builtin, unknown}) || source != wantSource || !slices.Equal(calls, original) {
		t.Fatalf("mixed source changed input or ordering: %#v %q %v", ordered, source, err)
	}
	values, err := parseCompilerIntrinsicIntegersResult(ordered, "202311 0x7 0\n")
	if err != nil || !maps.Equal(values, map[CompilerIntrinsicCall]string{attribute: "202311", builtin: "0x7", unknown: "0"}) {
		t.Fatalf("mixed operators lost exact integer values: %#v %v", values, err)
	}
	for _, contents := range []string{"202311 7", "202311 7 BAD", "202311 7 0 1"} {
		if got, err := parseCompilerIntrinsicIntegersResult(ordered, contents); err == nil || got != nil {
			t.Fatalf("mixed vector published a prefix: %#v %v", got, err)
		}
	}
	for _, collision := range []CompilerIntrinsicCall{
		{"__has_attribute", "__has_builtin"}, {"__has_builtin", "__has_attribute"},
	} {
		for _, vector := range [][]CompilerIntrinsicCall{{collision}, {attribute, collision, builtin}, {builtin, collision, attribute}} {
			if got, source, err := compilerIntrinsicIntegersSource(vector); err == nil || got != nil || source != "" {
				t.Fatalf("cross-operator collision published source: %#v %q %v", got, source, err)
			}
			if got, err := parseCompilerIntrinsicIntegersResult(vector, "1 1 1"); err == nil || got != nil {
				t.Fatalf("collision published parsed answers: %#v %v", got, err)
			}
		}
	}

	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (struct{}, error) {
		for _, call := range []CompilerIntrinsicCall{attribute, builtin} {
			_, _, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", []string{"-nostdinc"}, nil, call.Operator, call.Operand, nil)
			if err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || len(discovery.Plan.Nodes) != 2 || len(discovery.Plan.Requests) != 2 {
		t.Fatalf("different operators aliased one request: %#v %v", discovery, err)
	}
}

func TestCompilerIntrinsicBuiltinPreservesUnrelatedOperatorDefinitions(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, test := range []struct {
		call  CompilerIntrinsicCall
		other string
	}{
		{CompilerIntrinsicCall{"__has_builtin", "__builtin_dynamic_object_size"}, "__has_attribute"},
		{CompilerIntrinsicCall{"__has_attribute", "deprecated"}, "__has_builtin"},
	} {
		for _, macro := range [][]string{{"-D" + test.other + "(x)=0"}, {"-D", test.other + "(x)=0"}, {"-U", test.other}} {
			t.Run(test.call.Operator+"/"+strings.Join(macro, " "), func(t *testing.T) {
				arguments := append(slices.Clone(macro), "-nostdinc", "source.c")
				workload := func(scopes *KbuildProbeScopes) (compilerIntrinsicTestValue, error) {
					value, ready, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", arguments, []string{"source.c"}, test.call.Operator, test.call.Operand, nil)
					return compilerIntrinsicTestValue{value, ready}, err
				}
				discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
				if err != nil || len(discovery.Plan.Nodes) != 1 || discovery.Value.ready {
					t.Fatalf("unrelated operator prevented discovery: %#v %v", discovery, err)
				}
				node := discovery.Plan.Nodes[0]
				step := discovery.Plan.Requests[node.RequestID].Steps[0]
				projected, _, err := ProjectProbeCandidateArguments(step.Candidate.Projection, arguments, step.Candidate.TranslationUnits)
				if err != nil || !slices.Equal(projected, arguments[:len(arguments)-1]) {
					t.Fatalf("unrelated macro changed during projection: %q %v", projected, err)
				}
				if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, projected); err != nil {
					t.Fatal(err)
				}
				oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-intrinsic-integer": "7\n"})
				replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
				if err != nil || !replay.Value.ready || replay.Value.token != "7" || replay.Plan.Nodes[0].ID != node.ID {
					t.Fatalf("operator-aware replay changed exact value or identity: %#v %v", replay, err)
				}
			})
		}
	}
}

func TestCompilerIntrinsicIntegerCanonicalRequestAndReplay(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	arguments := []string{"-nostdinc", "-DVALUE=202311", "-D", "deprecated=other", "-UOTHER", "-DOTHER=7", "-include", "forced.h", "source.c"}
	units := []string{"source.c", "source.c"}
	environment := map[string]string{"MODE": "exact"}
	originalArguments, originalUnits, originalEnvironment := slices.Clone(arguments), slices.Clone(units), maps.Clone(environment)
	workload := func(scopes *KbuildProbeScopes) (compilerIntrinsicTestValue, error) {
		value, ready, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", arguments, units, "__has_attribute", "deprecated", environment)
		return compilerIntrinsicTestValue{value, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || discovery.Value.ready || discovery.Value.token != "" || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("discovery = %#v, %v", discovery, err)
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	want := ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{
		Name: "compiler-intrinsic-integer", Tool: "cc",
		Arguments: append(slices.Clone(arguments), "-E", "-P", "-x", "c", "-"),
		Stdin:     "#undef deprecated\n__has_attribute(deprecated)\n",
		Candidate: &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerIntrinsic,
			Base: []int{0, 1, 2, 3, 4, 5, 6, 7, 8}, TranslationUnits: []string{"source.c"}},
		Environment: map[string]string{"MODE": "exact"},
	}}, Outcome: ProbeOutcome{Kind: "text", Step: "compiler-intrinsic-integer", Stream: "stdout", RequireSuccess: true}}
	gotJSON, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := want.CanonicalJSON()
	if err != nil || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("intrinsic request changed: %v\ngot %s\nwant %s", err, gotJSON, wantJSON)
	}
	projected, origins, err := ProjectProbeCandidateArguments(request.Steps[0].Candidate.Projection, arguments, []string{"source.c"})
	if err != nil || !slices.Equal(projected, arguments[:6]) || !slices.Equal(origins, []int{0, 1, 2, 3, 4, 5}) {
		t.Fatalf("intrinsic projection lost macro bytes/order: %q %v, %v", projected, origins, err)
	}
	if _, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, projected); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"0", "1", "202311"} {
		oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-intrinsic-integer": output + "\n"})
		replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
		if err != nil || !replay.Value.ready || replay.Value.token != output || replay.Plan.Nodes[0].ID != node.ID {
			t.Fatalf("replay %q = %#v, %v", output, replay, err)
		}
	}
	if !slices.Equal(arguments, originalArguments) || !slices.Equal(units, originalUnits) || !maps.Equal(environment, originalEnvironment) {
		t.Fatal("intrinsic query mutated caller inputs")
	}
	legacy, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerPredefines, arguments, []string{"source.c"})
	if err != nil || !slices.Equal(legacy, []string{"-nostdinc", "-DVALUE=1", "-D", "deprecated=1", "-UOTHER", "-DOTHER=1"}) {
		t.Fatalf("existing predefine projection changed: %q, %v", legacy, err)
	}
}

func TestCompilerIntrinsicIntegerIdentity(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	options.Target.Tools = maps.Clone(options.Target.Tools)
	options.Target.Tools["cxx"] = options.Target.Tools["cc"]
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	seen := map[string]string{}
	for _, test := range []struct {
		name, scope, role, language, operand string
		arguments, units                     []string
		environment                          map[string]string
	}{
		{"base", "target", "cc", "c", "deprecated", []string{"-DVALUE=2", "-UOTHER"}, nil, nil},
		{"macro value", "target", "cc", "c", "deprecated", []string{"-DVALUE=3", "-UOTHER"}, nil, nil},
		{"order", "target", "cc", "c", "deprecated", []string{"-UOTHER", "-DVALUE=2"}, nil, nil},
		{"language", "target", "cc", "c++", "deprecated", []string{"-DVALUE=2", "-UOTHER"}, nil, nil},
		{"role", "target", "cxx", "c", "deprecated", []string{"-DVALUE=2", "-UOTHER"}, nil, nil},
		{"scope", "host", "cc", "c", "deprecated", []string{"-DVALUE=2", "-UOTHER"}, nil, nil},
		{"operand", "target", "cc", "c", "__retain__", []string{"-DVALUE=2", "-UOTHER"}, nil, nil},
		{"environment", "target", "cc", "c", "deprecated", []string{"-DVALUE=2", "-UOTHER"}, nil, map[string]string{"MODE": "alternate"}},
		{"source ownership", "target", "cc", "c", "deprecated", []string{"-DVALUE=2", "-UOTHER", "source.c"}, []string{"source.c"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			discovery, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (compilerIntrinsicTestValue, error) {
				value, ready, err := scopes.CompilerIntrinsicInteger(test.scope, test.role, test.language, test.arguments, test.units, "__has_attribute", test.operand, test.environment)
				return compilerIntrinsicTestValue{value, ready}, err
			})
			if err != nil || discovery.Value.ready || len(discovery.Plan.Nodes) != 1 {
				t.Fatalf("identity discovery = %#v, %v", discovery, err)
			}
			id := discovery.Plan.Nodes[0].ID
			if previous, found := seen[id]; found {
				t.Fatalf("%s shares identity with %s", test.name, previous)
			}
			seen[id] = test.name
		})
	}
}

func TestCompilerIntrinsicIntegerRejectsUnusableOracle(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerIntrinsicTestValue, error) {
		value, ready, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", nil, nil, "__has_attribute", "deprecated", nil)
		if err != nil && (ready || value != "") {
			t.Fatalf("error returned partial authority: %q %t %v", value, ready, err)
		}
		return compilerIntrinsicTestValue{value, ready}, err
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
		{"missing", func(o *ProbeResultOracle) { delete(o.results, nodeID) }},
		{"wrong request", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.RequestID = strings.Repeat("0", 64)
			o.results[nodeID] = r
		}},
		{"wrong toolset", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.ToolsetIdentity = strings.Repeat("0", 64)
			o.results[nodeID] = r
		}},
		{"missing step", func(o *ProbeResultOracle) { r := o.results[nodeID]; r.Steps = nil; o.results[nodeID] = r }},
		{"failed with numeric stdout", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.Steps[0].Status = "failure"
			r.Steps[0].ExitCode = 1
			o.results[nodeID] = r
		}},
		{"skipped", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.Steps[0].Status = "skipped"
			o.results[nodeID] = r
		}},
		{"spoofed summary", func(o *ProbeResultOracle) { r := o.results[nodeID]; r.Text = "1"; o.results[nodeID] = r }},
		{"unexpanded call", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.Text = "__has_attribute(deprecated)"
			r.Steps[0].Stdout = r.Text
			o.results[nodeID] = r
		}},
		{"extra token", func(o *ProbeResultOracle) {
			r := o.results[nodeID]
			r.Text = "202311 0"
			r.Steps[0].Stdout = r.Text
			o.results[nodeID] = r
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-intrinsic-integer": "202311"})
			test.mutate(oracle)
			_, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if err == nil || errors.As(err, &unsupported) {
				t.Fatalf("unusable oracle was not a real error: %v", err)
			}
		})
	}
}

func TestCompilerIntrinsicIntegerSymbolicArgumentsAndEnvironment(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	workload := func(scopes *KbuildProbeScopes) (compilerIntrinsicTestValue, error) {
		measured, err := scopes.evaluators["target"].requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "query-input", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{Kind: "text", Step: "query-input", Stream: "stdout", RequireSuccess: true},
		})
		if err != nil {
			return compilerIntrinsicTestValue{}, err
		}
		value, ready, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", []string{measured, "source.c"}, []string{"source.c"}, "__has_attribute", "deprecated", map[string]string{"MODE": measured})
		return compilerIntrinsicTestValue{value, ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || discovery.Value.ready || len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("symbolic discovery = %#v, %v", discovery, err)
	}
	for _, request := range discovery.Plan.Requests {
		step := request.Steps[0]
		if step.Name == "compiler-intrinsic-integer" && (request.InputCount != 1 || step.Candidate == nil ||
			step.Candidate.Projection != ProbeCandidateProjectionCompilerIntrinsic || len(step.ArgumentFragments) == 0 ||
			len(step.Environment) != 0 || len(step.EnvironmentFragments) != 1 || step.EnvironmentFragments[0].Name != "MODE") {
			t.Fatalf("symbolic query lost ownership: %#v", request)
		}
	}
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"query-input": "-DVALUE=202311 -Ddeprecated=other", "compiler-intrinsic-integer": "202311"})
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || !replay.Value.ready || replay.Value.token != "202311" ||
		!slices.EqualFunc(discovery.Plan.Nodes, replay.Plan.Nodes, func(a, b ProbePlanNode) bool { return a.ID == b.ID }) {
		t.Fatalf("symbolic replay changed identity: %#v, %v", replay, err)
	}
}

func TestCompilerIntrinsicProjectionPreservesSafetyGates(t *testing.T) {
	for _, arguments := range [][]string{
		{"-include"}, {"-x", "c"}, {"-Xclang", "-load", "plugin.so"},
		{"-include-pch", "header.pch"}, {"-Wp,-DVALUE=7"}, {"@response"}, {"-D"}, {"source.c", "source.c"},
	} {
		units := []string(nil)
		if slices.Contains(arguments, "source.c") {
			units = []string{"source.c"}
		}
		projected, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerIntrinsic, arguments, units)
		if err == nil {
			_, err = ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, projected)
		}
		if err == nil {
			t.Fatalf("intrinsic projection admitted unsafe arguments %q", arguments)
		}
	}
	request := ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "probe", Tool: "cc", Arguments: []string{"-DVALUE=2", "-E", "-P", "-x", "c", "-"},
		Stdin:     "#undef deprecated\n__has_attribute(deprecated)\n",
		Candidate: &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerIntrinsic, Base: []int{0}},
	}}, Outcome: ProbeOutcome{Kind: "text", Step: "probe", Stream: "stdout", RequireSuccess: true}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Steps[0].Tool = "ld"
	if err := request.Validate(); err == nil {
		t.Fatal("intrinsic projection accepted linker role")
	}
	request.Steps[0].Tool = "cc"
	request.Steps[0].Candidate.Policy = ProbeCandidatePolicyLD
	if err := request.Validate(); err == nil {
		t.Fatal("intrinsic projection accepted linker candidate policy")
	}
}

func TestCompilerIntrinsicIntegerRejectsInvalidQueryWithoutRegistration(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	for _, test := range []struct {
		name, scope, role, language, operator, operand string
		environment                                    map[string]string
	}{
		{"operator", "target", "cc", "c", "__has_include", "header", nil},
		{"collision", "target", "cc", "c", "__has_attribute", "__has_attribute", nil},
		{"scope", "missing", "cc", "c", "__has_attribute", "deprecated", nil},
		{"role", "target", "ld", "c", "__has_attribute", "deprecated", nil},
		{"missing role", "target", "cxx", "c++", "__has_attribute", "deprecated", nil},
		{"language", "target", "cc", "rust", "__has_attribute", "deprecated", nil},
		{"assembly", "target", "cc", "assembler-with-cpp", "__has_attribute", "deprecated", nil},
		{"unbound environment", "target", "cc", "c", "__has_attribute", "deprecated", map[string]string{"MODE": "${work:root}/include"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
				value, ready, err := scopes.CompilerIntrinsicInteger(test.scope, test.role, test.language, nil, nil, test.operator, test.operand, test.environment)
				if err == nil || ready || value != "" {
					t.Fatalf("invalid query returned %q %t %v", value, ready, err)
				}
				return struct{}{}, nil
			})
			if err != nil || len(evaluation.Plan.Nodes) != 0 {
				t.Fatalf("invalid query registered work: %#v %v", evaluation, err)
			}
		})
	}
	var missing *KbuildProbeScopes
	if value, ready, err := missing.CompilerIntrinsicInteger("target", "cc", "c", nil, nil, "__has_attribute", "deprecated", nil); err == nil || ready || value != "" {
		t.Fatalf("nil workload returned %q %t %v", value, ready, err)
	}
}

func TestCompilerIntrinsicIntegersCanonicalSourceAndWholeResult(t *testing.T) {
	a := CompilerIntrinsicCall{"__has_attribute", "__retain__"}
	b := CompilerIntrinsicCall{"__has_attribute", "deprecated"}
	c := CompilerIntrinsicCall{"__has_attribute", "unknown_attribute_9"}
	calls := []CompilerIntrinsicCall{c, b, a, b}
	original := slices.Clone(calls)
	ordered, source, err := compilerIntrinsicIntegersSource(calls)
	wantSource := "#undef __retain__\n__has_attribute(__retain__)\n#undef deprecated\n__has_attribute(deprecated)\n#undef unknown_attribute_9\n__has_attribute(unknown_attribute_9)\n"
	if err != nil || !slices.Equal(ordered, []CompilerIntrinsicCall{a, b, c}) || source != wantSource || !slices.Equal(calls, original) {
		t.Fatalf("canonical source changed input/order: %v %q %v", ordered, source, err)
	}
	values, err := parseCompilerIntrinsicIntegersResult(ordered, "\n0x1\t202311\v0\r\f")
	if err != nil || !maps.Equal(values, map[CompilerIntrinsicCall]string{a: "0x1", b: "202311", c: "0"}) {
		t.Fatalf("integer vector = %#v, %v", values, err)
	}
	for _, output := range []string{"", "1 202311", "1 202311 0 1", "1 202311 BAD", "1 202311 0u", "1 202311 0\u00a0", "1 202311 -1", "1 202311 9223372036854775808"} {
		if got, err := parseCompilerIntrinsicIntegersResult(ordered, output); err == nil || got != nil {
			t.Fatalf("invalid vector published prefix: %#v, %v", got, err)
		}
	}
	for _, invalid := range [][]CompilerIntrinsicCall{nil, {b, a}, {a, a}, {{"__has_include", "file"}}} {
		if got, err := parseCompilerIntrinsicIntegersResult(invalid, "1 1"); err == nil || got != nil {
			t.Fatalf("noncanonical call order published values: %#v, %v", got, err)
		}
	}
	ordered[0] = c
	if !slices.Equal(calls, original) {
		t.Fatal("canonical calls alias caller slice")
	}
}

func TestCompilerIntrinsicIntegersBudgets(t *testing.T) {
	call := CompilerIntrinsicCall{"__has_attribute", "deprecated"}
	for _, count := range []int{0, maxCompilerIntrinsicCalls + 1} {
		calls := make([]CompilerIntrinsicCall, count)
		for index := range calls {
			calls[index] = call
		}
		if ordered, source, err := compilerIntrinsicIntegersSource(calls); err == nil || ordered != nil || source != "" {
			t.Fatalf("invalid count %d returned partial source", count)
		}
	}
	longCalls := make([]CompilerIntrinsicCall, maxCompilerIntrinsicCalls)
	for index := range longCalls {
		longCalls[index] = CompilerIntrinsicCall{"__has_attribute", fmt.Sprintf("a%04d", index) + strings.Repeat("x", 123)}
	}
	if ordered, source, err := compilerIntrinsicIntegersSource(longCalls); err == nil || ordered != nil || source != "" {
		t.Fatal("maximum call count bypassed actual source byte bound")
	}
	// 3718 maximum-length snippets plus a 37-character operand fill exactly
	// one MiB. The next byte-sized grammar change increases source by two bytes.
	boundary := append(slices.Clone(longCalls[:3718]), CompilerIntrinsicCall{"__has_attribute", strings.Repeat("z", 37)})
	ordered, source, err := compilerIntrinsicIntegersSource(boundary)
	if err != nil || len(source) != MaxProbeInterpolatedBytes || len(ordered) != len(boundary) {
		t.Fatalf("source boundary = %d bytes, %d calls, %v", len(source), len(ordered), err)
	}
	boundary[len(boundary)-1].Operand += "z"
	if ordered, source, err := compilerIntrinsicIntegersSource(boundary); err == nil || ordered != nil || source != "" {
		t.Fatal("oversize source published a prefix")
	}
	duplicates := make([]CompilerIntrinsicCall, maxCompilerIntrinsicCalls)
	for index := range duplicates {
		duplicates[index] = longCalls[0]
	}
	if ordered, _, err := compilerIntrinsicIntegersSource(duplicates); err != nil || len(ordered) != 1 {
		t.Fatalf("source budget counted duplicates after canonicalization: %d, %v", len(ordered), err)
	}
	contents := "0" + strings.Repeat(" ", MaxProbeInterpolatedBytes-1)
	if got, err := parseCompilerIntrinsicIntegersResult([]CompilerIntrinsicCall{call}, contents); err != nil || got[call] != "0" {
		t.Fatalf("stdout boundary = %#v, %v", got, err)
	}
	if got, err := parseCompilerIntrinsicIntegersResult([]CompilerIntrinsicCall{call}, contents+" "); err == nil || got != nil {
		t.Fatal("oversize stdout published a result")
	}
	if got, err := parseCompilerIntrinsicIntegersResult([]CompilerIntrinsicCall{call}, strings.Repeat("0 ", MaxProbeInterpolatedBytes/2)); err == nil || got != nil {
		t.Fatal("surplus token stream published a result")
	}
}

func TestCompilerIntrinsicIntegersSingletonWireParity(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	call := CompilerIntrinsicCall{"__has_attribute", "deprecated"}
	arguments := []string{"-DVALUE=202311", "source.c"}
	units := []string{"source.c"}
	environment := map[string]string{"MODE": "exact"}
	evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		single, err := scopes.compilerIntrinsicIntegerRequest("target", "cc", "c", arguments, units, call.Operator, call.Operand, environment)
		if err != nil {
			return struct{}{}, err
		}
		ordered, plural, err := scopes.compilerIntrinsicIntegersRequest("target", "cc", "c", arguments, units, []CompilerIntrinsicCall{call}, environment)
		if err != nil {
			return struct{}{}, err
		}
		singleJSON, err := single.request.CanonicalJSON()
		if err != nil {
			return struct{}{}, err
		}
		pluralJSON, err := plural.request.CanonicalJSON()
		if err != nil || !bytes.Equal(singleJSON, pluralJSON) || !slices.Equal(single.dependencies, plural.dependencies) ||
			!slices.Equal(ordered, []CompilerIntrinsicCall{call}) || plural.request.Steps[0].Stdin != "#undef deprecated\n__has_attribute(deprecated)\n" {
			t.Fatalf("singleton wire parity failed: %v", err)
		}
		if value, ready, err := scopes.CompilerIntrinsicInteger("target", "cc", "c", arguments, units, call.Operator, call.Operand, environment); err != nil || ready || value != "" {
			t.Fatalf("singular discovery = %q %t %v", value, ready, err)
		}
		if values, ready, err := scopes.CompilerIntrinsicIntegers("target", "cc", "c", arguments, units, []CompilerIntrinsicCall{call}, environment); err != nil || ready || values != nil {
			t.Fatalf("plural discovery = %#v %t %v", values, ready, err)
		}
		return struct{}{}, nil
	})
	if err != nil || len(evaluation.Plan.Nodes) != 1 {
		t.Fatalf("singleton request did not deduplicate: %#v %v", evaluation, err)
	}
}

func TestCompilerIntrinsicIntegersCanonicalRequestReplayAndInvalidTail(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	a := CompilerIntrinsicCall{"__has_attribute", "__retain__"}
	b := CompilerIntrinsicCall{"__has_attribute", "deprecated"}
	calls := []CompilerIntrinsicCall{b, a, b}
	original := slices.Clone(calls)
	workload := func(scopes *KbuildProbeScopes) (map[CompilerIntrinsicCall]string, error) {
		values, ready, err := scopes.CompilerIntrinsicIntegers("target", "cc", "c", []string{"-DVALUE=202311"}, nil, calls, nil)
		if !ready && values != nil {
			t.Fatal("unready vector published values")
		}
		return values, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || discovery.Value != nil || len(discovery.Plan.Nodes) != 1 || !slices.Equal(calls, original) {
		t.Fatalf("plural discovery = %#v, %v", discovery, err)
	}
	calls = []CompilerIntrinsicCall{a, b}
	reordered, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || reordered.Plan.Nodes[0].ID != discovery.Plan.Nodes[0].ID {
		t.Fatalf("reorder changed request identity: %v", err)
	}
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-intrinsic-integer": "1\n202311\n"})
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || !maps.Equal(replay.Value, map[CompilerIntrinsicCall]string{a: "1", b: "202311"}) {
		t.Fatalf("plural replay = %#v %v", replay, err)
	}
	replay.Value[a] = "changed"
	replayedAgain, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || replayedAgain.Value[a] != "1" {
		t.Fatal("caller mutation changed replay result")
	}
	for _, contents := range []string{"1", "1 202311 0", "1 INVALID"} {
		oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{"compiler-intrinsic-integer": contents})
		if _, err := EvaluateKbuildProbeWorkload(options, oracle, workload); err == nil {
			t.Fatalf("accepted invalid vector %q", contents)
		}
	}
	calls = []CompilerIntrinsicCall{a}
	changed, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || changed.Plan.Nodes[0].ID == discovery.Plan.Nodes[0].ID {
		t.Fatalf("changed call set reused request: %v", err)
	}
}
