package kconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyProbeValueTransformSupportsPureMakeFunctionChains(t *testing.T) {
	tests := []struct {
		name      string
		transform ProbeValueTransform
		input     string
		want      string
	}{
		{"subst", ProbeValueTransform{Function: "subst", Arguments: []string{"a", "x", ""}, InputArgument: 2}, "a ba", "x bx"},
		{"addprefix", ProbeValueTransform{Function: "addprefix", Arguments: []string{"pre", ""}, InputArgument: 1}, "a b", "prea preb"},
		{"addsuffix", ProbeValueTransform{Function: "addsuffix", Arguments: []string{".o", ""}, InputArgument: 1}, "a b", "a.o b.o"},
		{"dir", ProbeValueTransform{Function: "dir", Arguments: []string{""}, InputArgument: 0}, "rust/core.o generated", "rust/ ./"},
		{"filter", ProbeValueTransform{Function: "filter", Arguments: []string{"%.c", ""}, InputArgument: 1}, "a.c b.o", "a.c"},
		{"filter-out", ProbeValueTransform{Function: "filter-out", Arguments: []string{"%.c", ""}, InputArgument: 1}, "a.c b.o", "b.o"},
		{"dynamic-filter-pattern", ProbeValueTransform{Function: "filter", Arguments: []string{"", "a.c b.o"}, InputArgument: 0}, "%.c", "a.c"},
		{"findstring", ProbeValueTransform{Function: "findstring", Arguments: []string{"-pg", ""}, InputArgument: 1}, "-Wall -pg -O2", "-pg"},
		{"patsubst", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%.c", "%.o", ""}, InputArgument: 2}, "a.c b.h", "a.o b.h"},
		{"patsubst-first-percent", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%", "x%y%", ""}, InputArgument: 2}, "a", "xay%"},
		{"patsubst-drops-empty", ProbeValueTransform{Function: "patsubst", Arguments: []string{"%.c", "", ""}, InputArgument: 2}, "a.c b.c z", "z"},
		{"sort", ProbeValueTransform{Function: "sort", Arguments: []string{""}, InputArgument: 0}, "b a b", "a b"},
		{"strip", ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}, "  a\t b  ", "a b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ApplyProbeValueTransform(test.transform, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("ApplyProbeValueTransform() = %q, want %q", got, test.want)
			}
		})
	}

	value := " -Wall   -DREMOVE -g "
	chain := []ProbeValueTransform{
		{Function: "filter-out", Arguments: []string{"-DREMOVE", ""}, InputArgument: 1},
		{Function: "filter-out", Arguments: []string{"-DOTHER", ""}, InputArgument: 1},
		{Function: "strip", Arguments: []string{""}, InputArgument: 0},
	}
	var err error
	for _, transform := range chain {
		value, err = ApplyProbeValueTransform(transform, value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if value != "-Wall -g" {
		t.Fatalf("filter/strip chain = %q, want %q", value, "-Wall -g")
	}
	_, err = ApplyProbeValueTransform(ProbeValueTransform{
		Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
		ArgumentFragments: []ProbeValueTransformArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{Value: "-fdrop"}}}},
	}, "-fdrop -fkeep")
	if err == nil || !strings.Contains(err.Error(), "unresolved dynamic arguments") {
		t.Fatalf("unrendered dynamic transform error = %v", err)
	}
}

func TestCanonicalSourceRequestDeepCopiesValueTransformArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "linux")
	evaluator := &LinuxProbeEvaluator{sourceRoot: root}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments: []ProbeArgumentFragments{{
				Index: 0, Fragments: []ProbeValueFragment{{
					Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
					Transforms: []ProbeValueTransform{{
						Function: "filter-out", Arguments: []string{filepath.Join(root, "include") + "/%", ""}, InputArgument: 1,
					}, {
						Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
						ArgumentFragments: []ProbeValueTransformArgumentFragments{{
							Index: 0, Fragments: []ProbeValueFragment{{Value: filepath.Join(root, "generated") + "/%"}},
						}},
					}},
				}},
			}},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	canonical := evaluator.canonicalSourceRequest(request)
	got := canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0]
	if got != "__LINUX_BZL_SOURCE_TREE__/include/%" {
		t.Fatalf("canonical transform argument = %q", got)
	}
	original := request.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[0].Arguments[0]
	if !strings.Contains(original, filepath.ToSlash(root)) {
		t.Fatalf("canonicalization mutated original transform argument: %q", original)
	}
	canonical.Steps[0].ArgumentFragments[0].Fragments[0].Fragments[0].Value = "mutated"
	if got := request.Steps[0].ArgumentFragments[0].Fragments[0].Fragments[0].Value; got != "${result:00000000.text}" {
		t.Fatalf("canonicalization retained nested fragment alias: %q", got)
	}
	dynamic := canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value
	if dynamic != "__LINUX_BZL_SOURCE_TREE__/generated/%" {
		t.Fatalf("canonical dynamic transform argument = %q", dynamic)
	}
	canonical.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value = "mutated"
	originalDynamic := request.Steps[0].ArgumentFragments[0].Fragments[0].Transforms[1].ArgumentFragments[0].Fragments[0].Value
	if !strings.Contains(originalDynamic, filepath.ToSlash(root)) {
		t.Fatalf("canonicalization retained dynamic transform argument alias: %q", originalDynamic)
	}
}

func TestApplyProbeValueTransformBoundsWordsWorkAndOutput(t *testing.T) {
	tests := []struct {
		name      string
		transform ProbeValueTransform
		input     string
		want      string
	}{
		{
			name:      "word count",
			transform: ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0},
			input:     strings.Repeat("x ", MaxProbeDynamicArgumentWords+1),
			want:      "Make words",
		},
		{
			name:      "filter comparisons",
			transform: ProbeValueTransform{Function: "filter", Arguments: []string{strings.Repeat("x ", 2048), ""}, InputArgument: 1},
			input:     strings.Repeat("x ", 2048),
			want:      "pattern comparisons",
		},
		{
			name:      "expanding output",
			transform: ProbeValueTransform{Function: "addprefix", Arguments: []string{strings.Repeat("p", 32), ""}, InputArgument: 1},
			input:     strings.Repeat("x ", 40000),
			want:      "result may exceed",
		},
		{
			name:      "empty subst search",
			transform: ProbeValueTransform{Function: "subst", Arguments: []string{"", "x", ""}, InputArgument: 2},
			input:     "abc",
			want:      "nonempty search string",
		},
		{
			name:      "unsupported pure function",
			transform: ProbeValueTransform{Function: "intcmp", Arguments: []string{"", "2"}, InputArgument: 0},
			input:     "1",
			want:      "proven probe value transform subset",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ApplyProbeValueTransform(test.transform, test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ApplyProbeValueTransform() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPureKbuildMakeFunctionMatchesGNUEmptyAndPercentEdges(t *testing.T) {
	tests := []struct {
		function  string
		arguments []string
		want      string
	}{
		{"subst", []string{"", "x", "abc"}, "abcx"},
		{"patsubst", []string{"%", "x%y%", "a"}, "xay%"},
		{"patsubst", []string{"%.c", "", "a.c b.c z"}, "z"},
		{"suffix", []string{"a.c b"}, ".c"},
		{"addprefix", []string{"-i ", "one.symvers two.symvers"}, "-i one.symvers -i two.symvers"},
		{"addsuffix", []string{" .stamp", "one two"}, "one .stamp two .stamp"},
	}
	for _, test := range tests {
		got, recognized, err := evalPureKbuildMakeFunction(test.function, test.arguments, "original")
		if err != nil || !recognized || got != test.want {
			t.Errorf("%s(%q) = %q, recognized=%v, error=%v; want %q", test.function, test.arguments, got, recognized, err, test.want)
		}
	}
}
