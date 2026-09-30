package kconfig

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPossibleCompilerSourceWords(t *testing.T) {
	condition := &ProbePredicate{Operator: "result-true", Result: "00000000"}
	strip := ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}
	for _, test := range []struct {
		name      string
		fragments []ProbeValueFragment
		want      []string
	}{
		{"flags with included prerequisite", []ProbeValueFragment{{Fragments: []ProbeValueFragment{
			{Value: " -D__ASSEMBLY__ -mabi=aapcs-linux "},
			{Value: "-marm ", When: condition},
			{Value: "-include include/shared.S "},
		}, Transforms: []ProbeValueTransform{strip}}}, []string{"-D__ASSEMBLY__", "-mabi=aapcs-linux", "-marm", "-include", "include/shared.S"}},
		{"conditional actual input", []ProbeValueFragment{{Value: "driver.c ", When: condition}}, []string{"driver.c"}},
		{"input assembled across leaves", []ProbeValueFragment{{Value: "driver"}, {Value: ".c", When: condition}}, []string{"driver", "driver.c"}},
		{"quoted input", []ProbeValueFragment{{Value: "'driver'\".c\""}}, []string{"driver.c"}},
		{"transformed input", []ProbeValueFragment{{Value: "driver.o", Transforms: []ProbeValueTransform{
			{Function: "subst", Arguments: []string{".o", ".c", ""}, InputArgument: 2},
		}}}, []string{"driver.c"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := cloneLinuxProbeRequest(ProbeRequest{Steps: []ProbeStep{{ArgumentFragments: []ProbeArgumentFragments{{Fragments: test.fragments}}}}})
			got, complete := possibleCompilerSourceWords([]string{"-nostdinc", ""}, nil,
				[]ProbeArgumentFragments{{Index: 1, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: test.fragments}})
			if !complete {
				t.Fatal("finite source text was not enumerated")
			}
			want := append(slices.Clone(test.want), "-nostdinc")
			if len(got) != len(want) {
				t.Fatalf("words=%v, want=%v", got, want)
			}
			for _, word := range want {
				if !got[word] {
					t.Fatalf("lost possible word %q", word)
				}
			}
			if !reflect.DeepEqual(test.fragments, before.Steps[0].ArgumentFragments[0].Fragments) {
				t.Fatal("modified source-owned fragments")
			}
		})
	}
}

func TestPossibleCompilerSourceWordsFailsClosed(t *testing.T) {
	for _, value := range []string{"${result:00000000.text}", "$(unmodeled)", "`command`", "${source:input}"} {
		_, complete := possibleCompilerSourceWords([]string{""}, nil, []ProbeArgumentFragments{{
			Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: []ProbeValueFragment{{Value: value}},
		}})
		if complete {
			t.Fatalf("unknown text %q acquired an absence proof", value)
		}
	}
	fragments := []ProbeValueFragment{}
	for i := range 9 {
		fragments = append(fragments, ProbeValueFragment{Value: fmt.Sprint(i), When: &ProbePredicate{Operator: "result-true", Result: fmt.Sprintf("%08d", i)}})
	}
	for _, fragmentSet := range [][]ProbeValueFragment{fragments, {{Value: strings.Repeat("x", MaxProbeInterpolatedBytes+1)}}} {
		_, complete := possibleCompilerSourceWords([]string{""}, nil, []ProbeArgumentFragments{{Index: 0, Fragments: fragmentSet}})
		if complete {
			t.Fatal("exhausted proof budget was treated as an absence proof")
		}
	}
}

func TestPossibleCompilerSourceWordsRetainsScalarPayloads(t *testing.T) {
	words, complete := possibleCompilerSourceWords([]string{"-D", "driver.c", "-include", "header.C"},
		[]ProbeConditionalArguments{{Before: 0, Arguments: []string{"other.S"}}}, nil)
	if !complete || !words["driver.c"] || !words["header.C"] || !words["other.S"] {
		t.Fatal("absence proof inferred positional ownership from scalar/include syntax")
	}
}
