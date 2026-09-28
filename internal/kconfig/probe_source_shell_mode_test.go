package kconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func sourceShellModeTestRequest() ProbeRequest {
	request := intrinsicSourceShapeTestRequest()
	request.InputCount = 1
	request.Steps[0].Arguments[0] = ""
	request.Steps[0].ArgumentFragments = []ProbeArgumentFragments{{
		Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords,
		Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
	}}
	return request
}

func TestProbeSourceShellModeRequiresOwnedCompilerProjection(t *testing.T) {
	for _, projection := range []string{ProbeCandidateProjectionCompilerPredefines, ProbeCandidateProjectionCompilerIntrinsic} {
		request := sourceShellModeTestRequest()
		request.Steps[0].Candidate.Projection = projection
		before, _ := json.Marshal(request)
		if err := request.Validate(); err != nil {
			t.Fatalf("valid %s source-shell mode: %v", projection, err)
		}
		after, _ := json.Marshal(request)
		if string(before) != string(after) {
			t.Fatal("mode validation mutated the request")
		}
		withMode, err := request.ID()
		if err != nil {
			t.Fatal(err)
		}
		request.Steps[0].ArgumentFragments[0].Mode = ""
		withoutMode, err := request.ID()
		if err != nil || withMode == withoutMode {
			t.Fatalf("source-shell mode is absent from replay identity: %v", err)
		}
	}
	for _, test := range []struct {
		name   string
		change func(*ProbeRequest)
	}{
		{"no candidate", func(r *ProbeRequest) { r.Steps[0].Candidate = nil }},
		{"ordinary candidate", func(r *ProbeRequest) { r.Steps[0].Candidate.Projection = "" }},
		{"unknown projection", func(r *ProbeRequest) { r.Steps[0].Candidate.Projection = "source-source" }},
		{"linker", func(r *ProbeRequest) { r.Steps[0].Candidate.Policy = ProbeCandidatePolicyLD }},
		{"compiler linker", func(r *ProbeRequest) { r.Steps[0].Candidate.Policy = ProbeCandidatePolicyCCLink }},
		{"unowned slot", func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{1} }},
		{"conditional ownership is insufficient", func(r *ProbeRequest) {
			r.Steps[0].Candidate.Base = nil
			intrinsicSourceShapeConditional(r, 0, true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := sourceShellModeTestRequest()
			test.change(&request)
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "source-shell-words requires") {
				t.Fatalf("mode escaped its candidate ownership gate: %v", err)
			}
		})
	}
}

func TestProbeSourceShellModeValidatesEveryLiteralBoundary(t *testing.T) {
	const marker = "\x01linux-bzl-action-object-tree\x02"
	for _, test := range []struct {
		name      string
		fragments []ProbeValueFragment
	}{
		{"partial literal", []ProbeValueFragment{{Value: "\x01"}, {Value: "${result:00000000.text}"}, {Value: "\x02"}}},
		{"nested partial literal", []ProbeValueFragment{{Fragments: []ProbeValueFragment{{Value: "\x01"}}}}},
		{"static transform argument", []ProbeValueFragment{{Value: marker, Transforms: []ProbeValueTransform{{
			Function: "filter-out", Arguments: []string{"\x02", ""}, InputArgument: 1,
		}}}}},
		{"nested transform argument", []ProbeValueFragment{{Value: marker, Transforms: []ProbeValueTransform{{
			Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
			ArgumentFragments: []ProbeValueTransformArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{Value: "\x01"}}}},
		}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := sourceShellModeTestRequest()
			request.Steps[0].ArgumentFragments[0].Fragments = test.fragments
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "source-shell") {
				t.Fatalf("partial literal marker escaped validation: %v", err)
			}
		})
	}
	request := sourceShellModeTestRequest()
	request.Steps[0].ArgumentFragments[0].Fragments = []ProbeValueFragment{{Value: marker + "/file"}}
	if err := request.Validate(); err != nil {
		t.Fatalf("complete literal marker was rejected: %v", err)
	}
}
