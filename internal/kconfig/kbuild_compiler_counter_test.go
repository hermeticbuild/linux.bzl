package kconfig

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func counterProbeTestRequest(t *testing.T, count int) ProbeRequest {
	t.Helper()
	request := intrinsicSourceShapeTestRequest()
	request.Steps[0].Name = compilerCounterSequenceStep
	source, err := compilerCounterSequenceSource(count)
	if err != nil {
		t.Fatal(err)
	}
	request.Steps[0].Stdin = source
	request.Outcome.Step = compilerCounterSequenceStep
	return request
}

func TestCompilerCounterProbeExactShapeAndCandidateBinding(t *testing.T) {
	for _, count := range []int{1, 16, maxCompilerCounterExpansions} {
		request := counterProbeTestRequest(t, count)
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		if got, err := compilerCounterProbeCount(request.Steps[0]); err != nil || got != count {
			t.Fatalf("count = %d, %v; want %d", got, err, count)
		}
	}
	for _, test := range []struct {
		name   string
		change func(*ProbeRequest)
	}{
		{"empty", func(r *ProbeRequest) { r.Steps[0].Stdin = "" }},
		{"over count", func(r *ProbeRequest) {
			r.Steps[0].Stdin = strings.Repeat("__COUNTER__\n", maxCompilerCounterExpansions+1)
		}},
		{"unterminated", func(r *ProbeRequest) { r.Steps[0].Stdin = "__COUNTER__" }},
		{"another spelling", func(r *ProbeRequest) { r.Steps[0].Stdin = "__VERSION__\n" }},
		{"effect", func(r *ProbeRequest) { r.Steps[0].Stdin = "__COUNTER__\n_Pragma(P)\n" }},
		{"text override", func(r *ProbeRequest) { r.Steps[0].Stdin = "#define __COUNTER__ 1\n__COUNTER__\n" }},
		{"intrinsic calls", func(r *ProbeRequest) { r.Steps[0].Stdin = intrinsicSourceShapeTestRequest().Steps[0].Stdin }},
		{"wrong step", func(r *ProbeRequest) {
			r.Steps[0].Name = "compiler-intrinsic-integer"
			r.Outcome.Step = r.Steps[0].Name
		}},
		{"wrong role", func(r *ProbeRequest) { r.Steps[0].Tool = "ld" }},
		{"wrong suffix", func(r *ProbeRequest) { r.Steps[0].Arguments[2] = "-c" }},
		{"unowned prefix", func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{0} }},
		{"owned suffix", func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{0, 1, 2} }},
		{"conditional suffix", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 3, true) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := counterProbeTestRequest(t, 16)
			test.change(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("accepted malformed counter request")
			}
			if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(request.Steps[0], []string{"-nostdinc"}); err == nil {
				t.Fatal("runtime validator admitted malformed managed input")
			}
		})
	}
	step := counterProbeTestRequest(t, 16).Steps[0]
	for _, argv := range [][]string{
		{"-D__COUNTER__=73"}, {"-D", "__COUNTER__=73"}, {"-D__COUNTER__(x)=x"},
		{"-U__COUNTER__"}, {"-U", "__COUNTER__"}, {"-Wp,-D__COUNTER__=73"},
	} {
		if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err == nil {
			t.Fatalf("accepted override %q", argv)
		}
	}
	for _, argv := range [][]string{{"-nostdinc"}, {"-DOTHER=73"}, {"-D__has_attribute(x)=7"}} {
		if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err != nil {
			t.Fatalf("rejected unrelated argument %q: %v", argv, err)
		}
	}
}

func TestCompilerCounterProbeDiscoveryAndExactReplay(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	arguments := []string{"-nostdinc", "-DOTHER=73", "-include", "forced.h", "source.c"}
	units := []string{"source.c"}
	workload := func(s *KbuildProbeScopes) (*compilerCounterSequence, error) {
		sequence, ready, err := s.compilerCounterSequence("target", "cc", "c", arguments, units, 3, nil)
		if ready != (sequence != nil) || err != nil && sequence != nil {
			t.Fatal("published partial sequence")
		}
		return sequence, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil || discovery.Value != nil || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("discovery = %#v, %v", discovery, err)
	}
	node := discovery.Plan.Nodes[0]
	step := discovery.Plan.Requests[node.RequestID].Steps[0]
	projected, _, err := ProjectProbeCandidateArguments(step.Candidate.Projection, arguments, units)
	if err != nil || !slices.Equal(projected, arguments[:2]) {
		t.Fatalf("projected argv lost exact initial macro values: %q, %v", projected, err)
	}
	if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, projected); err != nil {
		t.Fatal(err)
	}
	oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{compilerCounterSequenceStep: "7 42 9\n"})
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil || replay.Value == nil || !slices.Equal(replay.Value.values, []string{"7", "42", "9"}) || replay.Plan.Nodes[0].ID != node.ID {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	context := configDependencyCompilerPredefineKey("target", "cc", "c", arguments, units, nil)
	if replay.Value.context != fmt.Sprintf("%x", sha256.Sum256([]byte(context))) {
		t.Fatal("sequence lost its exact source request context")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ProbeResultOracle)
	}{
		{"missing", func(o *ProbeResultOracle) { delete(o.results, node.ID) }},
		{"wrong request", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.RequestID = strings.Repeat("0", 64)
			o.results[node.ID] = r
		}},
		{"wrong toolset", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.ToolsetIdentity = strings.Repeat("0", 64)
			o.results[node.ID] = r
		}},
		{"failure", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.Steps[0].Status = "failure"
			r.Steps[0].ExitCode = 1
			o.results[node.ID] = r
		}},
		{"truncated", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.Text = "7 42"
			r.Steps[0].Stdout = r.Text
			o.results[node.ID] = r
		}},
		{"extra", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.Text = "7 42 9 10"
			r.Steps[0].Stdout = r.Text
			o.results[node.ID] = r
		}},
		{"unexpanded", func(o *ProbeResultOracle) {
			r := o.results[node.ID]
			r.Text = "7 42 __COUNTER__"
			r.Steps[0].Stdout = r.Text
			o.results[node.ID] = r
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := compilerDefinednessTestOracle(t, discovery.Plan, map[string]string{compilerCounterSequenceStep: "7 42 9\n"})
			test.mutate(oracle)
			if _, err := EvaluateKbuildProbeWorkload(options, oracle, workload); err == nil {
				t.Fatal("accepted unusable counter result")
			}
		})
	}
	for _, count := range []int{1, 2, 4, 16} {
		next, err := EvaluateKbuildProbeWorkload(options, nil, func(s *KbuildProbeScopes) (*compilerCounterSequence, error) {
			sequence, _, err := s.compilerCounterSequence("target", "cc", "c", arguments, units, count, nil)
			return sequence, err
		})
		if err != nil || next.Plan.Nodes[0].ID == node.ID {
			t.Fatalf("count %d did not change request identity: %v", count, err)
		}
	}
}
