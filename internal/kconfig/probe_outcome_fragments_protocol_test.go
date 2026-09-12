package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func probeOutcomeFragmentsRequest() ProbeRequest {
	return ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: 2,
		Outcome: ProbeOutcome{
			Kind: "text",
			Fragments: []ProbeValueFragment{
				{Value: "prefix:"},
				{
					Fragments: []ProbeValueFragment{{
						Value: "${result:00000000.text}",
						When:  &ProbePredicate{Operator: "result-true", Result: "00000001"},
					}},
					Transforms: []ProbeValueTransform{{
						Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
						ArgumentFragments: []ProbeValueTransformArgumentFragments{{
							Index: 0,
							Fragments: []ProbeValueFragment{{
								Value: "${result:00000000.text}",
							}},
						}},
					}},
				},
			},
		},
	}
}

func TestProbeOutcomeFragmentsAcceptsDependencyOnlyRequest(t *testing.T) {
	request := probeOutcomeFragmentsRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid derived text outcome request: %v", err)
	}
	if roles := request.ToolRoles(); len(roles) != 0 {
		t.Fatalf("derived text outcome tool roles = %q, want none", roles)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	if !strings.Contains(encoded, `"schema":"linux-probe-request-v13"`) ||
		!strings.Contains(encoded, `"fragments"`) || strings.Contains(encoded, `"steps":[`) {
		t.Fatalf("derived text outcome canonical JSON has the wrong envelope: %s", encoded)
	}
}

func TestProbeOutcomeFragmentsParticipatesInCanonicalIdentity(t *testing.T) {
	first := probeOutcomeFragmentsRequest()
	firstID, err := first.ID()
	if err != nil {
		t.Fatal(err)
	}

	second := probeOutcomeFragmentsRequest()
	second.Outcome.Fragments[0].Value = "different-prefix:"
	secondID, err := second.ID()
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatal("derived text outcome fragments did not participate in request identity")
	}
}

func TestCanonicalSourceRequestDeepClonesProbeOutcomeFragments(t *testing.T) {
	original := probeOutcomeFragmentsRequest()
	canonical := (&LinuxProbeEvaluator{}).canonicalSourceRequest(original)

	canonical.Outcome.Fragments[0].Value = "changed"
	canonical.Outcome.Fragments[1].Fragments[0].Value = "changed"
	canonical.Outcome.Fragments[1].Fragments[0].When.Result = "00000000"
	canonical.Outcome.Fragments[1].Transforms[0].Arguments[0] = "changed"
	canonical.Outcome.Fragments[1].Transforms[0].ArgumentFragments[0].Fragments[0].Value = "changed"

	if got := original.Outcome.Fragments[0].Value; got != "prefix:" {
		t.Fatalf("canonical request aliases top-level outcome fragment: %q", got)
	}
	inner := original.Outcome.Fragments[1]
	if got := inner.Fragments[0].Value; got != "${result:00000000.text}" {
		t.Fatalf("canonical request aliases nested outcome fragment: %q", got)
	}
	if got := inner.Fragments[0].When.Result; got != "00000001" {
		t.Fatalf("canonical request aliases outcome fragment predicate: %q", got)
	}
	if got := inner.Transforms[0].Arguments[0]; got != "" {
		t.Fatalf("canonical request aliases outcome transform argument: %q", got)
	}
	if got := inner.Transforms[0].ArgumentFragments[0].Fragments[0].Value; got != "${result:00000000.text}" {
		t.Fatalf("canonical request aliases nested outcome transform fragment: %q", got)
	}
}

func TestProbeOutcomeFragmentsRejectsNonDependencyRequestState(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ProbeRequest)
	}{
		{
			name: "step",
			edit: func(request *ProbeRequest) {
				request.Steps = []ProbeStep{{Name: "consume", Tool: "cc"}}
			},
		},
		{
			name: "scratch",
			edit: func(request *ProbeRequest) {
				request.Scratch = []ProbeScratch{{Name: "value", Kind: "file"}}
			},
		},
		{
			name: "source",
			edit: func(request *ProbeRequest) {
				request.Sources = []string{"Kconfig"}
			},
		},
		{
			name: "source root",
			edit: func(request *ProbeRequest) {
				request.SourceRoots = []string{"linux"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := probeOutcomeFragmentsRequest()
			test.edit(&request)
			err := request.Validate()
			if err == nil || !strings.Contains(err.Error(), "dependency-only, zero-step") {
				t.Fatalf("Validate() error = %v, want dependency-only rejection", err)
			}
		})
	}
}

func TestProbeOutcomeFragmentsRequiresInput(t *testing.T) {
	request := probeOutcomeFragmentsRequest()
	request.InputCount = 0
	err := request.Validate()
	if err == nil || !strings.Contains(err.Error(), "neither steps nor inputs") {
		t.Fatalf("Validate() error = %v, want no-input rejection", err)
	}
}

func TestProbeOutcomeFragmentsRejectsNonResultPlaceholders(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "tool", value: "${tool:cc}", want: "non-result placeholder"},
		{name: "scratch", value: "${scratch:value}", want: "undeclared scratch"},
		{name: "source", value: "${source:Kconfig}", want: "undeclared source"},
		{name: "source root", value: "${source_root:linux}", want: "undeclared source root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := probeOutcomeFragmentsRequest()
			request.Outcome.Fragments = []ProbeValueFragment{{Value: test.value}}
			err := request.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
			if test.name == "tool" && !slices.Equal(request.ToolRoles(), []string{"cc"}) {
				t.Fatalf("invalid tool-backed outcome roles = %q, want [cc] before validation", request.ToolRoles())
			}
		})
	}
}

func TestProbeOutcomeFragmentsRequiresAvailableTextResults(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "malformed ordinal", value: "${result:not-an-ordinal.text}", want: "references unavailable result"},
		{name: "out of range", value: "${result:00000002.text}", want: "references unavailable result"},
		{name: "non-text", value: "${result:00000000.boolean}", want: "unavailable text result"},
		{name: "missing field", value: "${result:00000000}", want: "references unavailable result"},
		{name: "unterminated", value: "${result:00000000.text", want: "malformed or unsupported placeholder"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := probeOutcomeFragmentsRequest()
			request.InputCount = 1
			request.Outcome.Fragments = []ProbeValueFragment{{Value: test.value}}
			err := request.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestProbeOutcomeFragmentsRejectsRuntimePredicates(t *testing.T) {
	tests := []struct {
		name      string
		predicate ProbePredicate
		want      string
	}{
		{
			name:      "step",
			predicate: ProbePredicate{Operator: "exit-zero", Step: "compile"},
			want:      "unavailable step",
		},
		{
			name:      "execroot",
			predicate: ProbePredicate{Operator: "execroot-exists", Value: "${result:00000000.text}"},
			want:      `non-boolean-result predicate "execroot-exists"`,
		},
		{
			name:      "text result",
			predicate: ProbePredicate{Operator: "result-text-empty", Result: "00000000"},
			want:      `non-boolean-result predicate "result-text-empty"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := probeOutcomeFragmentsRequest()
			request.Outcome.Fragments = []ProbeValueFragment{{
				Value: "${result:00000000.text}", When: &test.predicate,
			}}
			err := request.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestProbeOutcomeFragmentsRejectsMixedSelectorsAndReductions(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ProbeOutcome)
		want string
	}{
		{name: "predicate", edit: func(outcome *ProbeOutcome) {
			outcome.Predicate = &ProbePredicate{Operator: "result-true", Result: "00000000"}
		}, want: "text probe outcome cannot have predicate"},
		{name: "step", edit: func(outcome *ProbeOutcome) { outcome.Step = "consume" }, want: "requires only fragments"},
		{name: "stream", edit: func(outcome *ProbeOutcome) { outcome.Stream = "stdout" }, want: "requires only fragments"},
		{name: "result", edit: func(outcome *ProbeOutcome) { outcome.Result = "00000000" }, want: "requires only fragments"},
		{name: "word", edit: func(outcome *ProbeOutcome) { outcome.Word = 1 }, want: "requires only fragments"},
		{name: "trim space", edit: func(outcome *ProbeOutcome) { outcome.TrimSpace = true }, want: "requires only fragments"},
		{name: "first line", edit: func(outcome *ProbeOutcome) { outcome.FirstLine = true }, want: "requires only fragments"},
		{name: "last line", edit: func(outcome *ProbeOutcome) { outcome.LastLine = true }, want: "requires only fragments"},
		{name: "GNU Make shell", edit: func(outcome *ProbeOutcome) { outcome.GNUMakeShell = true }, want: "requires only fragments"},
		{name: "require success", edit: func(outcome *ProbeOutcome) { outcome.RequireSuccess = true }, want: "requires only fragments"},
		{name: "single Make word", edit: func(outcome *ProbeOutcome) { outcome.SingleMakeWord = true }, want: "requires only fragments"},
		{name: "path component", edit: func(outcome *ProbeOutcome) { outcome.PathComponent = true }, want: "requires only fragments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := probeOutcomeFragmentsRequest()
			test.edit(&request.Outcome)
			err := request.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestProbeOutcomeFragmentsRejectsInvalidNestedTransformArguments(t *testing.T) {
	request := func() ProbeRequest {
		value := probeOutcomeFragmentsRequest()
		value.InputCount = 1
		value.Outcome.Fragments = []ProbeValueFragment{{
			Value: "${result:00000000.text}",
			Transforms: []ProbeValueTransform{{
				Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
				ArgumentFragments: []ProbeValueTransformArgumentFragments{{
					Index: 0, Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
				}},
			}},
		}}
		return value
	}

	tests := []struct {
		name string
		edit func(*ProbeRequest)
		want string
	}{
		{
			name: "transform input index",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].ArgumentFragments[0].Index = 1
			},
			want: "replaces the transform input",
		},
		{
			name: "nonempty backing argument",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].Arguments[0] = "fixed"
			},
			want: "must replace one empty argument",
		},
		{
			name: "empty nested fragments",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].ArgumentFragments[0].Fragments = nil
			},
			want: "must replace one empty argument",
		},
		{
			name: "nested tool",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].ArgumentFragments[0].Fragments[0].Value = "${tool:cc}"
			},
			want: "non-result placeholder",
		},
		{
			name: "nested non-text result",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].ArgumentFragments[0].Fragments[0].Value = "${result:00000000.boolean}"
			},
			want: "unavailable text result",
		},
		{
			name: "nested execroot predicate",
			edit: func(value *ProbeRequest) {
				value.Outcome.Fragments[0].Transforms[0].ArgumentFragments[0].Fragments[0].When = &ProbePredicate{
					Operator: "execroot-exists", Value: "${result:00000000.text}",
				}
			},
			want: `non-boolean-result predicate "execroot-exists"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := request()
			test.edit(&value)
			err := value.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}
