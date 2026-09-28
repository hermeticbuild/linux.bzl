package kconfig

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func candidateProtocolRequest() ProbeRequest {
	dependency := ProbePredicate{Operator: "result-true", Result: "00000000"}
	return ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: 1,
		Steps: []ProbeStep{{
			Name:           "compile",
			Tool:           "cc",
			AuxiliaryTools: []string{"ld"},
			Arguments:      []string{"-DSTATIC", "", "-c"},
			ArgumentFragments: []ProbeArgumentFragments{{
				Index: 1,
				Fragments: []ProbeValueFragment{{
					Value: "${tool:ld}",
				}},
			}},
			ConditionalArguments: []ProbeConditionalArguments{
				{Before: 2, When: dependency, Arguments: []string{"-DCOND_A"}},
				{Before: 3, When: dependency, Arguments: []string{"-DCOND_B"}},
			},
			Candidate: &ProbeCandidateArguments{
				Policy:      ProbeCandidatePolicyCC,
				Base:        []int{0, 1},
				Conditional: []int{0, 1},
			},
		}},
		Outcome: ProbeOutcome{
			Kind:      "boolean",
			Predicate: &ProbePredicate{Operator: "exit-zero", Step: "compile"},
		},
	}
}

func TestProbeCandidateProtocolSchemaAndNilCompatibility(t *testing.T) {
	if got, want := LinuxProbeRequestSchema, "linux-probe-request-v13"; got != want {
		t.Fatalf("probe request schema = %q, want %q", got, want)
	}

	request := candidateProtocolRequest()
	request.Steps[0].Candidate = nil
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"candidate"`) {
		t.Fatalf("nil candidate was serialized: %s", data)
	}
	firstID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID {
		t.Fatalf("nil-candidate identity is unstable: %q != %q", firstID, secondID)
	}

	legacy := request
	legacy.Schema = "linux-probe-request-v8"
	if err := legacy.Validate(); err == nil || !strings.Contains(err.Error(), LinuxProbeRequestSchema) {
		t.Fatalf("legacy schema validation error = %v", err)
	}
}

func TestProbeCandidateProtocolPoliciesAndOwnedFragments(t *testing.T) {
	for _, policy := range []string{
		ProbeCandidatePolicyCC,
		ProbeCandidatePolicyLD,
		ProbeCandidatePolicyCCLink,
	} {
		t.Run(policy, func(t *testing.T) {
			request := candidateProtocolRequest()
			request.Steps[0].Candidate.Policy = policy
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			data, err := request.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"candidate":{"policy":"`+policy+`","base":[0,1],"conditional":[0,1]}`) {
				t.Fatalf("canonical request does not contain candidate ownership: %s", data)
			}
		})
	}

	// Base ownership is expressed in the unexpanded Arguments coordinate
	// system. Index 1 therefore owns every argv word rendered from its fragment
	// group, and those fragments retain ordinary tool-role traversal.
	request := candidateProtocolRequest()
	if got, want := request.ToolRoles(), []string{"cc", "ld"}; !slices.Equal(got, want) {
		t.Fatalf("tool roles = %v, want %v", got, want)
	}
}

func TestProbeCandidateProtocolCompilerPredefineProjection(t *testing.T) {
	request := candidateProtocolRequest()
	request.Steps[0].Candidate.Projection = ProbeCandidateProjectionCompilerPredefines
	request.Steps[0].Candidate.TranslationUnits = []string{"source.c"}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"candidate":{"policy":"cc","projection":"compiler-predefines","base":[0,1],"conditional":[0,1],"translation_units":["source.c"]}`) {
		t.Fatalf("canonical request does not contain compiler-predefine projection: %s", data)
	}
	request.Steps[0].Tool = "nm"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "cc or cxx tool role") {
		t.Fatalf("non-compiler projection validation error = %v", err)
	}
}

func TestProbeCandidateProtocolRejectsInvalidOwnership(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ProbeCandidateArguments)
		want string
	}{
		{
			name: "missing policy",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Policy = "" },
			want: "unsupported policy",
		},
		{
			name: "unknown policy",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Policy = "gcc" },
			want: "unsupported policy",
		},
		{
			name: "unknown projection",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Projection = "strip-inputs" },
			want: "unsupported projection",
		},
		{
			name: "translation units without projection",
			edit: func(candidate *ProbeCandidateArguments) { candidate.TranslationUnits = []string{"source.c"} },
			want: "translation units require",
		},
		{
			name: "duplicate translation units",
			edit: func(candidate *ProbeCandidateArguments) {
				candidate.Projection = ProbeCandidateProjectionCompilerPredefines
				candidate.TranslationUnits = []string{"source.c", "source.c"}
			},
			want: "strictly sorted",
		},
		{
			name: "unsorted translation units",
			edit: func(candidate *ProbeCandidateArguments) {
				candidate.Projection = ProbeCandidateProjectionCompilerPredefines
				candidate.TranslationUnits = []string{"two.c", "one.c"}
			},
			want: "strictly sorted",
		},
		{
			name: "compiler projection requires cc policy",
			edit: func(candidate *ProbeCandidateArguments) {
				candidate.Policy = ProbeCandidatePolicyLD
				candidate.Projection = ProbeCandidateProjectionCompilerPredefines
			},
			want: "requires policy",
		},
		{
			name: "empty ownership",
			edit: func(candidate *ProbeCandidateArguments) {
				candidate.Base = nil
				candidate.Conditional = nil
			},
			want: "must own at least one",
		},
		{
			name: "negative base",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Base = []int{-1} },
			want: "invalid or unsorted argument index",
		},
		{
			name: "base out of range",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Base = []int{3} },
			want: "invalid or unsorted argument index",
		},
		{
			name: "duplicate base",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Base = []int{0, 0} },
			want: "invalid or unsorted argument index",
		},
		{
			name: "unsorted base",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Base = []int{1, 0} },
			want: "invalid or unsorted argument index",
		},
		{
			name: "negative conditional group",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Conditional = []int{-1} },
			want: "invalid or unsorted group index",
		},
		{
			name: "conditional group out of range",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Conditional = []int{2} },
			want: "invalid or unsorted group index",
		},
		{
			name: "duplicate conditional group",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Conditional = []int{0, 0} },
			want: "invalid or unsorted group index",
		},
		{
			name: "unsorted conditional groups",
			edit: func(candidate *ProbeCandidateArguments) { candidate.Conditional = []int{1, 0} },
			want: "invalid or unsorted group index",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := candidateProtocolRequest()
			test.edit(request.Steps[0].Candidate)
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestProbeCandidateProtocolCanonicalIdentity(t *testing.T) {
	request := candidateProtocolRequest()
	wantID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProbeRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	gotID, err := decoded.ID()
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("round-trip ID = %q, want %q", gotID, wantID)
	}

	withoutCandidate := candidateProtocolRequest()
	withoutCandidate.Steps[0].Candidate = nil
	withoutCandidateID, err := withoutCandidate.ID()
	if err != nil {
		t.Fatal(err)
	}
	if withoutCandidateID == wantID {
		t.Fatal("candidate provenance did not participate in request identity")
	}

	differentPolicy := candidateProtocolRequest()
	differentPolicy.Steps[0].Candidate.Policy = ProbeCandidatePolicyLD
	differentPolicyID, err := differentPolicy.ID()
	if err != nil {
		t.Fatal(err)
	}
	if differentPolicyID == wantID {
		t.Fatal("candidate policy did not participate in request identity")
	}

	differentOwnership := candidateProtocolRequest()
	differentOwnership.Steps[0].Candidate.Base = []int{1}
	differentOwnershipID, err := differentOwnership.ID()
	if err != nil {
		t.Fatal(err)
	}
	if differentOwnershipID == wantID {
		t.Fatal("candidate ownership did not participate in request identity")
	}

	differentProjection := candidateProtocolRequest()
	differentProjection.Steps[0].Candidate.Projection = ProbeCandidateProjectionCompilerPredefines
	differentProjectionID, err := differentProjection.ID()
	if err != nil {
		t.Fatal(err)
	}
	if differentProjectionID == wantID {
		t.Fatal("candidate projection did not participate in request identity")
	}
}
