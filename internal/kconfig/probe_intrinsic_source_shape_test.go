package kconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func intrinsicSourceShapeTestRequest() ProbeRequest {
	return ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "compiler-intrinsic-integer", Tool: "cc",
			Arguments: []string{"-nostdinc", "-O2", "-E", "-P", "-x", "c", "-"},
			Stdin:     "#undef deprecated\n__has_attribute(deprecated)\n#undef unused\n__has_attribute(unused)\n",
			Candidate: &ProbeCandidateArguments{
				Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerIntrinsic,
				Base: []int{0, 1},
			},
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "compiler-intrinsic-integer", Stream: "stdout", RequireSuccess: true},
	}
}

func intrinsicSourceShapeConditional(request *ProbeRequest, before int, owned bool) {
	request.InputCount = 1
	request.Steps[0].ConditionalArguments = []ProbeConditionalArguments{{
		Before: before, Arguments: []string{"-DVALUE=7"},
		When: ProbePredicate{Operator: "result-true", Result: "00000000"},
	}}
	if owned {
		request.Steps[0].Candidate.Conditional = []int{0}
	}
}

func TestProbeCompilerIntrinsicSourceShapePreservesCanonicalRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ProbeRequest)
	}{
		{"C vector", func(*ProbeRequest) {}},
		{"C++ vector", func(r *ProbeRequest) { r.Steps[0].Tool, r.Steps[0].Arguments[5] = "cxx", "c++" }},
		{"singleton", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef deprecated\n__has_attribute(deprecated)\n" }},
		{"builtin singleton", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef __builtin_trap\n__has_builtin(__builtin_trap)\n" }},
		{"mixed same operand", func(r *ProbeRequest) {
			r.Steps[0].Stdin = "#undef shared\n__has_attribute(shared)\n#undef shared\n__has_builtin(shared)\n"
		}},
		{"symbolic prefix", func(r *ProbeRequest) {
			r.InputCount = 1
			r.Steps[0].Arguments[0] = ""
			r.Steps[0].ArgumentFragments = []ProbeArgumentFragments{{
				Index: 0, Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
			}}
		}},
		{"conditional at prefix start", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 0, true) }},
		{"conditional at prefix boundary", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 2, true) }},
		{"conditional only", func(r *ProbeRequest) {
			r.Steps[0].Arguments = r.Steps[0].Arguments[2:]
			r.Steps[0].Candidate.Base = nil
			intrinsicSourceShapeConditional(r, 0, true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := intrinsicSourceShapeTestRequest()
			test.change(&request)
			before, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := request.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			want := append(bytes.Clone(before), '\n')
			if !bytes.Equal(canonical, want) {
				t.Fatal("shape validation changed canonical request bytes")
			}
			identity, err := request.ID()
			if err != nil || identity != fmt.Sprintf("%x", sha256.Sum256(want)) {
				t.Fatalf("shape validation changed request identity: %s, %v", identity, err)
			}
			after, err := json.Marshal(request)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("shape validation mutated the caller's request")
			}
		})
	}
}

func TestProbeCompilerIntrinsicSourceShapeRejectsUnboundInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ProbeRequest)
	}{
		{"arbitrary pragma source", func(r *ProbeRequest) { r.Steps[0].Stdin = "_Pragma(P)\n" }},
		{"missing undef", func(r *ProbeRequest) { r.Steps[0].Stdin = "__has_attribute(deprecated)\n" }},
		{"mismatched operand", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef unused\n__has_attribute(deprecated)\n" }},
		{"operator collision", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef __has_attribute\n__has_attribute(__has_attribute)\n" }},
		{"attribute operand is builtin operator", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef __has_builtin\n__has_attribute(__has_builtin)\n" }},
		{"builtin operand is attribute operator", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef __has_attribute\n__has_builtin(__has_attribute)\n" }},
		{"reverse operator order", func(r *ProbeRequest) {
			r.Steps[0].Stdin = "#undef shared\n__has_builtin(shared)\n#undef shared\n__has_attribute(shared)\n"
		}},
		{"duplicate builtin", func(r *ProbeRequest) {
			r.Steps[0].Stdin = strings.Repeat("#undef __builtin_trap\n__has_builtin(__builtin_trap)\n", 2)
		}},
		{"different operator", func(r *ProbeRequest) { r.Steps[0].Stdin = "#undef deprecated\n__has_include(deprecated)\n" }},
		{"missing final newline", func(r *ProbeRequest) { r.Steps[0].Stdin = strings.TrimSuffix(r.Steps[0].Stdin, "\n") }},
		{"extra source", func(r *ProbeRequest) { r.Steps[0].Stdin += "_Pragma(P)\n" }},
		{"extra whitespace", func(r *ProbeRequest) { r.Steps[0].Stdin = " " + r.Steps[0].Stdin }},
		{"CRLF source", func(r *ProbeRequest) { r.Steps[0].Stdin = strings.ReplaceAll(r.Steps[0].Stdin, "\n", "\r\n") }},
		{"comments", func(r *ProbeRequest) {
			r.Steps[0].Stdin = "#undef deprecated\n__has_attribute(deprecated) /* comment */\n"
		}},
		{"duplicate calls", func(r *ProbeRequest) { r.Steps[0].Stdin += r.Steps[0].Stdin }},
		{"unsorted calls", func(r *ProbeRequest) {
			r.Steps[0].Stdin = "#undef unused\n__has_attribute(unused)\n#undef deprecated\n__has_attribute(deprecated)\n"
		}},
		{"too many calls", func(r *ProbeRequest) {
			r.Steps[0].Stdin = strings.Repeat("#undef deprecated\n__has_attribute(deprecated)\n", maxCompilerIntrinsicCalls+1)
		}},
		{"source interpolation", func(r *ProbeRequest) {
			r.InputCount = 1
			r.Steps[0].Stdin = "#undef ${result:00000000.text}\n__has_attribute(${result:00000000.text})\n"
		}},
		{"stdin fragments", func(r *ProbeRequest) {
			r.InputCount = 1
			r.Steps[0].Stdin = ""
			r.Steps[0].StdinFragments = []ProbeValueFragment{{Value: "${result:00000000.text}"}}
		}},
		{"unmanaged macro override", func(r *ProbeRequest) {
			r.Steps[0].Arguments[1] = "-D__has_attribute(x)=_Pragma(P)"
			r.Steps[0].Candidate.Base = []int{0}
		}},
		{"ownership in managed suffix", func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{0, 2} }},
		{"missing suffix", func(r *ProbeRequest) { r.Steps[0].Arguments = r.Steps[0].Arguments[:2] }},
		{"wrong mode", func(r *ProbeRequest) { r.Steps[0].Arguments[2] = "-c" }},
		{"assembler language", func(r *ProbeRequest) { r.Steps[0].Arguments[5] = "assembler-with-cpp" }},
		{"extra managed operand", func(r *ProbeRequest) { r.Steps[0].Arguments = append(r.Steps[0].Arguments, "other.c") }},
		{"managed template", func(r *ProbeRequest) {
			r.InputCount = 1
			r.Steps[0].Arguments[5] = "${result:00000000.text}"
		}},
		{"managed fragment", func(r *ProbeRequest) {
			r.InputCount = 1
			r.Steps[0].Arguments[5] = ""
			r.Steps[0].ArgumentFragments = []ProbeArgumentFragments{{
				Index: 5, Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
			}}
		}},
		{"unowned conditional", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 2, false) }},
		{"conditional inside suffix", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 3, true) }},
		{"conditional after suffix", func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 7, true) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := intrinsicSourceShapeTestRequest()
			test.change(&request)
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "compiler intrinsic") {
				t.Fatalf("unbound intrinsic input was not rejected by its shape contract: %v", err)
			}
		})
	}
}

func TestProbeCompilerIntrinsicSourceShapeDoesNotRestrictOtherProjections(t *testing.T) {
	for _, projection := range []string{"", ProbeCandidateProjectionCompilerPredefines} {
		request := intrinsicSourceShapeTestRequest()
		request.Steps[0].Candidate.Projection = projection
		request.Steps[0].Stdin = "int value;\n"
		request.Steps[0].Arguments[2] = "-c"
		if err := request.Validate(); err != nil {
			t.Fatalf("intrinsic shape restricted projection %q: %v", projection, err)
		}
	}
}

func TestCompilerIntrinsicCandidateValidationRequiresActualCanonicalStep(t *testing.T) {
	for _, change := range []string{"unchanged", "builtin", "mixed", "nil candidate", "wrong projection", "wrong policy", "wrong role", "arbitrary source", "unrelated source", "reversed mixed source", "cross-operator collision", "stdin fragments", "wrong suffix", "unowned prefix", "unowned conditional", "conditional after suffix", "managed fragment"} {
		t.Run(change, func(t *testing.T) {
			request := intrinsicSourceShapeTestRequest()
			step := &request.Steps[0]
			argv := []string{"-nostdinc", `-DHEADER="/not/an/input"`}
			step.Arguments[1] = argv[1]
			valid := false
			switch change {
			case "unchanged":
				valid = true
			case "builtin":
				valid = true
				step.Stdin = "#undef __builtin_trap\n__has_builtin(__builtin_trap)\n"
			case "mixed":
				valid = true
				step.Stdin += "#undef __builtin_trap\n__has_builtin(__builtin_trap)\n"
			case "nil candidate":
				step.Candidate = nil
			case "wrong projection":
				step.Candidate.Projection = ProbeCandidateProjectionCompilerPredefines
			case "wrong policy":
				step.Candidate.Policy = ProbeCandidatePolicyLD
			case "wrong role":
				step.Tool = "ld"
			case "arbitrary source":
				step.Stdin = "_Pragma(HEADER)\n"
			case "unrelated source":
				step.Stdin = "#undef HEADER\n__has_include(HEADER)\n"
			case "reversed mixed source":
				step.Stdin = "#undef __builtin_trap\n__has_builtin(__builtin_trap)\n" + step.Stdin
			case "cross-operator collision":
				step.Stdin = "#undef __has_attribute\n__has_builtin(__has_attribute)\n"
			case "stdin fragments":
				step.StdinFragments = []ProbeValueFragment{{Value: step.Stdin}}
			case "wrong suffix":
				step.Arguments[2] = "-c"
			case "unowned prefix":
				step.Candidate.Base = []int{0}
			case "unowned conditional":
				intrinsicSourceShapeConditional(&request, 2, false)
			case "conditional after suffix":
				intrinsicSourceShapeConditional(&request, 7, true)
			case "managed fragment":
				step.ArgumentFragments = []ProbeArgumentFragments{{Index: 5, Fragments: []ProbeValueFragment{{Value: "c"}}}}
			}
			before, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := ValidateCompilerIntrinsicProbeCandidateArguments(*step, argv)
			if valid && (err != nil || len(paths) != 0) {
				t.Fatalf("canonical source-bound literal rejected or treated as a file: %#v %v", paths, err)
			}
			if !valid && (err == nil || len(paths) != 0) {
				t.Fatalf("unbound step granted projected macro/path authority: %#v %v", paths, err)
			}
			after, marshalErr := json.Marshal(request)
			if marshalErr != nil || !bytes.Equal(before, after) {
				t.Fatal("source-aware candidate validation mutated the original step")
			}
		})
	}
}
