package kconfig

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Generated object-tree content is not a compiler input until its producer has
// run. In particular a deferred word nested inside an authenticated shell-word
// fragment must not become literal argv in an initial compiler-state query.
func TestCompilerProjectedQueryDefersGeneratedContent(t *testing.T) {
	for _, projection := range []string{ProbeCandidateProjectionCompilerPredefines, ProbeCandidateProjectionCompilerIntrinsic} {
		for _, location := range []string{"argument", "conditional-argument", "nested-argument", "environment", "nested-environment"} {
			t.Run(projection+"/"+location, func(t *testing.T) {
				evaluator, first, _ := sourceShellWordsEvaluatorForTest(t, "target")
				evaluator.sourceRoot = "/fixture/linux"
				evaluator.sourceArchitecture = "arm"
				evaluator.tools = map[string]string{"cc": "compiler"}
				scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
				token := kbuildDeferredContentTokenPrefix + strings.Repeat("a", 64)
				value := "-mstack-protector-guard-offset=" + token
				arguments := []string{"-c", "unit.c"}
				environment := map[string]string{}
				var expression string
				if strings.HasPrefix(location, "nested-") {
					var err error
					text := value + " " + first
					expression, err = evaluator.renderMakeText("strip", []string{text}, text, linuxProbeMakeTextProtocolCanonicalWords)
					if err != nil {
						t.Fatal(err)
					}
				}
				switch location {
				case "argument":
					arguments = append(arguments, value)
				case "conditional-argument":
					conditional := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
					symbol := evaluator.symbols[first]
					symbol.trueText = value
					if err := evaluator.publishSymbol(conditional, symbol); err != nil {
						t.Fatal(err)
					}
					arguments = append(arguments, conditional)
				case "nested-argument":
					wrapped, err := evaluator.renderSourceShellWords(expression)
					if err != nil {
						t.Fatal(err)
					}
					arguments = append(arguments, wrapped)
				case "environment":
					environment["MODE"] = value
				case "nested-environment":
					environment["MODE"] = expression
				}
				originalArguments, originalEnvironment := slices.Clone(arguments), maps.Clone(environment)
				probe, err := scopes.compilerProjectedRequestWithProjection("target", "cc", "c", arguments,
					[]string{"unit.c"}, environment, "query", []string{"-dM", "-E", "-x", "c", "-"}, "",
					ProbeOutcome{Kind: "text", Step: "query", Stream: "stdout", RequireSuccess: true}, projection)
				var unsupported *compilerPredefineProjectionUnsupportedError
				if probe != nil || !errors.As(err, &unsupported) {
					t.Fatalf("generated content must defer the optional query before registration: probe=%t error=%v", probe != nil, err)
				}
				if strings.Contains(err.Error(), token) {
					t.Fatal("diagnostic should not expose source-owned generated content")
				}
				if !slices.Equal(arguments, originalArguments) || !maps.Equal(environment, originalEnvironment) {
					t.Fatal("query deferral mutated actual compiler inputs")
				}
			})
		}
	}
}

func TestOrdinaryProbeLoweringRetainsDeferredContentLiteral(t *testing.T) {
	evaluator, _, _ := sourceShellWordsEvaluatorForTest(t, "target")
	argument := "-mstack-protector-guard-offset=" + kbuildDeferredContentTokenPrefix + strings.Repeat("a", 64)
	base, conditional, fragments, err := newProbeSymbolicValueLowerer(evaluator).arguments([]string{argument})
	if err != nil || !slices.Equal(base, []string{argument}) || len(conditional) != 0 || len(fragments) != 0 {
		t.Fatalf("optional-query policy changed ordinary lowering: %v", err)
	}
}

// A concrete, source-selected numeric value remains an ordinary query. Do not
// fix the ARM failure by dropping or hardcoding this compiler flag.
func TestCompilerProjectedQueryRetainsConcreteGeneratedValue(t *testing.T) {
	evaluator, _, _ := sourceShellWordsEvaluatorForTest(t, "target")
	evaluator.tools = map[string]string{"cc": "compiler"}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	const argument = "-mstack-protector-guard-offset=24"
	probe, err := scopes.compilerProjectedRequest("target", "cc", "c", []string{argument}, nil, nil,
		"query", []string{"-dM", "-E", "-x", "c", "-"}, "",
		ProbeOutcome{Kind: "text", Step: "query", Stream: "stdout", RequireSuccess: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(probe.request.Steps[0].Arguments, argument) {
		t.Fatal("concrete compiler flag was lost")
	}
}
