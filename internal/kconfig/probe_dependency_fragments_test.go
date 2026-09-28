package kconfig

import (
	"strings"
	"testing"
)

func TestProbeResultPathFallbackPredicateUsesExplicitProvenance(t *testing.T) {
	predicate := ProbePredicate{Operator: "result-path-fallback", Result: "00000000"}
	for _, test := range []struct {
		name, kind string
		want       bool
	}{
		{name: "real artifact named like sentinel", kind: ProbeStdoutPathToolset, want: false},
		{name: "compiler fallback", kind: ProbeStdoutPathFallback, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateProbeResultPredicate(predicate, map[string]ProbeResult{
				"00000000": {
					Kind: "text", Text: "include",
					Steps: []ProbeStepResult{{Name: "query", Status: "success", Stdout: "include", StdoutPathKind: test.kind}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("fallback predicate = %t, want %t", got, test.want)
			}
		})
	}
}

func dependencyTextResult(value string) ProbeResult {
	return ProbeResult{Kind: "text", Text: value}
}

func dependencyBooleanResult(value bool) ProbeResult {
	return ProbeResult{Kind: "boolean", Boolean: &value}
}

func TestRenderProbeDependencyFragmentsPreservesExactTextAndConditions(t *testing.T) {
	inputs := map[string]ProbeResult{
		"00000000": dependencyTextResult("\talpha  beta\n"),
		"00000001": dependencyBooleanResult(true),
		"00000002": dependencyBooleanResult(false),
	}
	fragments := []ProbeValueFragment{
		{Value: "prefix["},
		{Value: "${result:00000000.text}"},
		{
			Value: "]\n",
			When: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
				{Operator: "result-true", Result: "00000001"},
				{Operator: "not", Operands: []ProbePredicate{{Operator: "result-true", Result: "00000002"}}},
			}},
		},
		{
			Value: "not rendered",
			When: &ProbePredicate{Operator: "any", Operands: []ProbePredicate{
				{Operator: "result-false", Result: "00000001"},
				{Operator: "result-true", Result: "00000002"},
			}},
		},
	}

	got, err := RenderProbeDependencyFragments(fragments, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if want := "prefix[\talpha  beta\n]\n"; got != want {
		t.Fatalf("rendered text = %q, want exact %q", got, want)
	}
}

func TestEvaluateProbeResultPredicateMatchesRunnerShortCircuit(t *testing.T) {
	falseValue := false
	inputs := map[string]ProbeResult{
		"00000000": {Kind: "boolean", Boolean: &falseValue},
		"00000001": {Kind: "boolean", Boolean: &falseValue},
	}
	predicate := ProbePredicate{Operator: "all", Operands: []ProbePredicate{
		{Operator: "result-true", Result: "00000000"},
		{Operator: "result-text-empty", Result: "00000001"},
	}}
	if !IsProbeResultPredicate(predicate) {
		t.Fatal("result-only predicate was not recognized")
	}
	got, err := EvaluateProbeResultPredicate(predicate, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("short-circuited all predicate = true, want false")
	}
	if IsProbeResultPredicate(ProbePredicate{Operator: "execroot-exists", Value: "generated"}) {
		t.Fatal("filesystem predicate was classified as a pure result reduction")
	}
}

func TestRenderProbeDependencyFragmentsDoesNotRecursivelyExpandResultText(t *testing.T) {
	inputs := map[string]ProbeResult{
		"00000000": dependencyTextResult("${result:00000001.text}"),
		"00000001": dependencyTextResult("unexpected recursive expansion"),
	}
	got, err := RenderProbeDependencyFragments(
		[]ProbeValueFragment{{Value: "${result:00000000.text}"}},
		inputs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "${result:00000001.text}"; got != want {
		t.Fatalf("rendered text = %q, want opaque dependency text %q", got, want)
	}
}

func TestRenderProbeDependencyFragmentsAggregatesAndDynamicTransformArguments(t *testing.T) {
	fragments := []ProbeValueFragment{{
		Fragments: []ProbeValueFragment{
			{Value: "${result:00000000.text}"},
			{Value: " drivers/c.dtb.o"},
		},
		Transforms: []ProbeValueTransform{
			{
				Function:      "filter",
				Arguments:     []string{"", ""},
				InputArgument: 1,
				ArgumentFragments: []ProbeValueTransformArgumentFragments{{
					Index: 0,
					Fragments: []ProbeValueFragment{{
						Fragments: []ProbeValueFragment{
							{Value: "%.dtb"},
							{Value: " %.dtbo %.dtb.o"},
						},
					}},
				}},
			},
			{
				Function:      "addprefix",
				Arguments:     []string{"obj/", ""},
				InputArgument: 1,
			},
		},
	}}
	inputs := map[string]ProbeResult{
		"00000000": dependencyTextResult("drivers/a.dtb drivers/a.o drivers/b.dtbo"),
	}

	got, err := RenderProbeDependencyFragments(fragments, inputs)
	if err != nil {
		t.Fatal(err)
	}
	want := "obj/drivers/a.dtb obj/drivers/b.dtbo obj/drivers/c.dtb.o"
	if got != want {
		t.Fatalf("rendered transformed aggregate = %q, want %q", got, want)
	}
}

func TestRenderProbeDependencyFragmentsPreservesEmptyTransformResult(t *testing.T) {
	fragments := []ProbeValueFragment{{
		Value: "drivers/a.c",
		Transforms: []ProbeValueTransform{{
			Function: "filter", Arguments: []string{"%.o", ""}, InputArgument: 1,
		}},
	}}
	got, err := RenderProbeDependencyFragments(fragments, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("rendered filtered text = %q, want empty", got)
	}
}

func TestRenderProbeDependencyFragmentsRejectsInvalidDependencies(t *testing.T) {
	tests := []struct {
		name      string
		fragments []ProbeValueFragment
		inputs    map[string]ProbeResult
		wantError string
	}{
		{
			name:      "unbound text result",
			fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
			wantError: "unbound",
		},
		{
			name:      "text result has boolean kind",
			fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
			inputs:    map[string]ProbeResult{"00000000": dependencyBooleanResult(true)},
			wantError: "not text",
		},
		{
			name:      "boolean result has text kind",
			fragments: []ProbeValueFragment{{Value: "x", When: &ProbePredicate{Operator: "result-true", Result: "00000000"}}},
			inputs:    map[string]ProbeResult{"00000000": dependencyTextResult("true")},
			wantError: "not boolean",
		},
		{
			name:      "boolean result has nil value",
			fragments: []ProbeValueFragment{{Value: "x", When: &ProbePredicate{Operator: "result-true", Result: "00000000"}}},
			inputs:    map[string]ProbeResult{"00000000": {Kind: "boolean"}},
			wantError: "not boolean",
		},
		{
			name:      "literal NUL",
			fragments: []ProbeValueFragment{{Value: "x\x00y"}},
			wantError: "NUL",
		},
		{
			name:      "dependency NUL",
			fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
			inputs:    map[string]ProbeResult{"00000000": dependencyTextResult("x\x00y")},
			wantError: "NUL",
		},
		{
			name:      "non-result placeholder",
			fragments: []ProbeValueFragment{{Value: "${tool:cc}"}},
			wantError: "non-result placeholder",
		},
		{
			name:      "boolean result placeholder",
			fragments: []ProbeValueFragment{{Value: "${result:00000000.boolean}"}},
			wantError: "unsupported dependency result placeholder",
		},
		{
			name:      "malformed placeholder",
			fragments: []ProbeValueFragment{{Value: "${result:00000000.text"}},
			wantError: "malformed or unsupported",
		},
		{
			name:      "unsupported predicate",
			fragments: []ProbeValueFragment{{Value: "x", When: &ProbePredicate{Operator: "result-text-empty", Result: "00000000"}}},
			wantError: "unsupported dependency predicate",
		},
		{
			name:      "empty fragment",
			fragments: []ProbeValueFragment{{}},
			wantError: "exactly one nonempty value or aggregate",
		},
		{
			name: "value and aggregate",
			fragments: []ProbeValueFragment{{
				Value: "x", Fragments: []ProbeValueFragment{{Value: "y"}},
			}},
			wantError: "exactly one nonempty value or aggregate",
		},
		{
			name: "unbound dynamic transform argument",
			fragments: []ProbeValueFragment{{
				Value: "a.o",
				Transforms: []ProbeValueTransform{{
					Function: "filter", Arguments: []string{"", ""}, InputArgument: 1,
					ArgumentFragments: []ProbeValueTransformArgumentFragments{{
						Index: 0, Fragments: []ProbeValueFragment{{Value: "${result:00000000.text}"}},
					}},
				}},
			}},
			wantError: "unbound",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RenderProbeDependencyFragments(test.fragments, test.inputs)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestRenderProbeDependencyFragmentsEnforcesBounds(t *testing.T) {
	t.Run("rendered bytes", func(t *testing.T) {
		_, err := RenderProbeDependencyFragments(
			[]ProbeValueFragment{{Value: "${result:00000000.text}"}},
			map[string]ProbeResult{"00000000": dependencyTextResult(strings.Repeat("x", MaxProbeInterpolatedBytes+1))},
		)
		if err == nil || !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "rendered bytes") {
			t.Fatalf("error = %v, want rendered-byte bound", err)
		}
	})

	t.Run("fragment count", func(t *testing.T) {
		fragments := make([]ProbeValueFragment, maxProbeValueFragments+1)
		for index := range fragments {
			fragments[index].Value = "x"
		}
		_, err := RenderProbeDependencyFragments(fragments, nil)
		if err == nil || !strings.Contains(err.Error(), "fragments") {
			t.Fatalf("error = %v, want fragment-count bound", err)
		}
	})

	t.Run("aggregate depth", func(t *testing.T) {
		fragment := ProbeValueFragment{Value: "x"}
		for range MaxProbeValueFragmentDepth + 1 {
			fragment = ProbeValueFragment{Fragments: []ProbeValueFragment{fragment}}
		}
		_, err := RenderProbeDependencyFragments([]ProbeValueFragment{fragment}, nil)
		if err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("error = %v, want aggregate-depth bound", err)
		}
	})

	t.Run("transform count", func(t *testing.T) {
		transforms := make([]ProbeValueTransform, maxProbeValueTransforms+1)
		for index := range transforms {
			transforms[index] = ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}
		}
		_, err := RenderProbeDependencyFragments([]ProbeValueFragment{{Value: "x", Transforms: transforms}}, nil)
		if err == nil || !strings.Contains(err.Error(), "transforms") {
			t.Fatalf("error = %v, want transform-count bound", err)
		}
	})
}

func TestRenderProbeDependencyFragmentsValidatesAllDeclaredBranches(t *testing.T) {
	falseResult := dependencyBooleanResult(false)
	trueResult := dependencyBooleanResult(true)

	t.Run("excluded aggregate still observes fragment bound", func(t *testing.T) {
		children := make([]ProbeValueFragment, maxProbeValueFragments+1)
		for index := range children {
			children[index].Value = "x"
		}
		fragments := []ProbeValueFragment{{
			Fragments: children,
			When:      &ProbePredicate{Operator: "result-true", Result: "00000000"},
		}}
		_, err := RenderProbeDependencyFragments(fragments, map[string]ProbeResult{"00000000": falseResult})
		if err == nil || !strings.Contains(err.Error(), "fragments") {
			t.Fatalf("error = %v, want excluded branch to enforce fragment bound", err)
		}
	})

	t.Run("excluded fragment still rejects unbound reference", func(t *testing.T) {
		fragments := []ProbeValueFragment{{
			Value: "${result:00000001.text}",
			When:  &ProbePredicate{Operator: "result-true", Result: "00000000"},
		}}
		_, err := RenderProbeDependencyFragments(fragments, map[string]ProbeResult{"00000000": falseResult})
		if err == nil || !strings.Contains(err.Error(), "unbound") {
			t.Fatalf("error = %v, want excluded branch to reject unbound reference", err)
		}
	})

	t.Run("excluded fragment still rejects NUL dependency text", func(t *testing.T) {
		fragments := []ProbeValueFragment{{
			Value: "${result:00000001.text}",
			When:  &ProbePredicate{Operator: "result-true", Result: "00000000"},
		}}
		_, err := RenderProbeDependencyFragments(fragments, map[string]ProbeResult{
			"00000000": falseResult,
			"00000001": dependencyTextResult("hidden\x00text"),
		})
		if err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Fatalf("error = %v, want excluded branch to reject NUL text", err)
		}
	})

	t.Run("short-circuited predicate still rejects unbound reference", func(t *testing.T) {
		fragments := []ProbeValueFragment{{
			Value: "x",
			When: &ProbePredicate{Operator: "any", Operands: []ProbePredicate{
				{Operator: "result-true", Result: "00000000"},
				{Operator: "result-true", Result: "00000001"},
			}},
		}}
		_, err := RenderProbeDependencyFragments(fragments, map[string]ProbeResult{"00000000": trueResult})
		if err == nil || !strings.Contains(err.Error(), "unbound") {
			t.Fatalf("error = %v, want every predicate operand to be validated", err)
		}
	})

	t.Run("predicate depth", func(t *testing.T) {
		predicate := ProbePredicate{Operator: "result-true", Result: "00000000"}
		for range MaxProbeValueFragmentDepth + 1 {
			predicate = ProbePredicate{Operator: "not", Operands: []ProbePredicate{predicate}}
		}
		_, err := RenderProbeDependencyFragments(
			[]ProbeValueFragment{{Value: "x", When: &predicate}},
			map[string]ProbeResult{"00000000": trueResult},
		)
		if err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("error = %v, want predicate-depth bound", err)
		}
	})
}
