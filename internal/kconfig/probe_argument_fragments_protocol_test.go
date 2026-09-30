package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestProbeRequestValidatesArgumentFragmentSlots(t *testing.T) {
	request := func() ProbeRequest {
		return ProbeRequest{
			Schema: LinuxProbeRequestSchema, InputCount: 1,
			Steps: []ProbeStep{{
				Name: "consume", Tool: "cc", Arguments: []string{"prefix", "", "", "suffix"},
				ArgumentFragments: []ProbeArgumentFragments{
					{Index: 1, Fragments: []ProbeValueFragment{{
						Value: "${result:00000000.text}",
						Transforms: []ProbeValueTransform{{
							Function: "filter-out", Arguments: []string{"-DREMOVE", ""}, InputArgument: 1,
						}},
					}}},
					{Index: 2, Fragments: []ProbeValueFragment{{
						Value: "selected", When: &ProbePredicate{Operator: "result-true", Result: "00000000"},
					}}},
				},
			}},
			Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
		}
	}
	if err := request().Validate(); err != nil {
		t.Fatalf("valid argument fragment request: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ProbeRequest)
		want string
	}{
		{
			name: "nonempty base slot",
			edit: func(value *ProbeRequest) { value.Steps[0].ArgumentFragments[0].Index = 0 },
			want: "must replace one empty base argument",
		},
		{
			name: "unsorted indexes",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0], value.Steps[0].ArgumentFragments[1] =
					value.Steps[0].ArgumentFragments[1], value.Steps[0].ArgumentFragments[0]
			},
			want: "invalid or unsorted index",
		},
		{
			name: "duplicate index",
			edit: func(value *ProbeRequest) { value.Steps[0].ArgumentFragments[1].Index = 1 },
			want: "invalid or unsorted index",
		},
		{
			name: "out of range index",
			edit: func(value *ProbeRequest) { value.Steps[0].ArgumentFragments[1].Index = 4 },
			want: "invalid or unsorted index",
		},
		{
			name: "empty fragment list",
			edit: func(value *ProbeRequest) { value.Steps[0].ArgumentFragments[0].Fragments = nil },
			want: "must replace one empty base argument",
		},
		{
			name: "unsupported mode",
			edit: func(value *ProbeRequest) { value.Steps[0].ArgumentFragments[0].Mode = "shell" },
			want: "unsupported mode",
		},
		{
			name: "unavailable result template",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Value = "${result:00000001.text}"
			},
			want: "references unavailable result",
		},
		{
			name: "unavailable result predicate",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[1].Fragments[0].When.Result = "00000001"
			},
			want: "invalid result/step fields",
		},
		{
			name: "unsupported transform",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Function = "shell"
			},
			want: "unsupported pure Make function",
		},
		{
			name: "nonempty transform input",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[1] = "not-empty"
			},
			want: "not an empty value slot",
		},
		{
			name: "invalid transform input",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].InputArgument = 2
			},
			want: "invalid input argument",
		},
		{
			name: "planner token in transform",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0] = linuxProbeSymbolPrefix + strings.Repeat("a", 64)
			},
			want: "planner-only probe token",
		},
		{
			name: "unavailable transform result",
			edit: func(value *ProbeRequest) {
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0] = "${result:00000001.text}"
			},
			want: "references unavailable result",
		},
		{
			name: "excessive transform chain",
			edit: func(value *ProbeRequest) {
				transforms := make([]ProbeValueTransform, maxProbeValueTransforms+1)
				for index := range transforms {
					transforms[index] = ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}
				}
				value.Steps[0].ArgumentFragments[0].Fragments[0].Transforms = transforms
			},
			want: "65 value transforms, maximum is 64",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := request()
			test.edit(&candidate)
			err := candidate.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}

	withTool := request()
	withTool.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0] = "${tool:ld}"
	if got, want := withTool.ToolRoles(), []string{"cc", "ld"}; !slices.Equal(got, want) {
		t.Fatalf("transform tool roles = %q, want %q", got, want)
	}
	firstID, err := request().ID()
	if err != nil {
		t.Fatal(err)
	}
	untypedData, err := request().CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(untypedData), `"mode"`) {
		t.Fatalf("canonical default argument fragments include an empty mode: %s", untypedData)
	}
	changed := request()
	changed.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0] = "-DOTHER"
	secondID, err := changed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatal("value transform did not participate in canonical request identity")
	}

	typed := request()
	typed.Steps[0].ArgumentFragments[0].Mode = ProbeArgumentFragmentsModeSignedDecimal
	typedID, err := typed.ID()
	if err != nil {
		t.Fatal(err)
	}
	if typedID == firstID {
		t.Fatal("argument fragment mode did not participate in canonical request identity")
	}
	data, err := typed.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"mode":"signed-decimal"`) {
		t.Fatalf("canonical typed argument fragments omit mode: %s", data)
	}
}

func TestValidateProbeSignedDecimalArgument(t *testing.T) {
	for _, value := range []string{"0", "-1", "9999999999999999999"} {
		if err := ValidateProbeSignedDecimalArgument(value); err != nil {
			t.Errorf("ValidateProbeSignedDecimalArgument(%q): %v", value, err)
		}
	}
	for _, value := range []string{
		"", "-", "+1", " 1", "1 ", "1 2", "1;id", "１２", "00000000000000000000",
	} {
		if err := ValidateProbeSignedDecimalArgument(value); err == nil {
			t.Errorf("ValidateProbeSignedDecimalArgument(%q) succeeded", value)
		}
	}
}
