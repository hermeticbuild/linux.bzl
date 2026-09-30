package kconfig

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func compilerCounterAnswersForPlanTest(t *testing.T, plan *ActionPlan, node ActionPlanNode, count int, output string) *KbuildCompilerGuardAnswers {
	t.Helper()
	scope, role, probe := configDependencyGuardAnswerProbeForTest(t, plan, node)
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := discovery.CompilerCounterSequence(scope, role, probe.language, probe.arguments, probe.translationUnits, count, probe.environment); err != nil || ready {
		t.Fatalf("counter discovery: %t %v", ready, err)
	}
	frozen, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	plan.Toolsets = maps.Clone(frozen.Toolsets)
	oracle := compilerDefinednessTestOracle(t, frozen, map[string]string{compilerCounterSequenceStep: output})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := replay.CompilerCounterSequence(scope, role, probe.language, probe.arguments, probe.translationUnits, count, probe.environment); err != nil || !ready {
		t.Fatalf("counter replay: %t %v", ready, err)
	}
	answers, err := replay.Answers()
	if err != nil {
		t.Fatal(err)
	}
	return answers
}

func TestCompilerCounterHintsPrecedeFailedSourceExpansionWithoutGrantingFacts(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const blocked = "#define READ_COUNTER __COUNTER__\n#include <missing.h>\nREAD_COUNTER\n"
	for _, tc := range []struct {
		name, source, dump           string
		measured, positive, wantHint bool
	}{
		{"blocked source", blocked, "", true, true, true},
		{"no initial measurement", blocked, "", false, false, false},
		{"measured absent", blocked, "", true, false, false},
		{"text replacement", blocked, "#define __COUNTER__ 7\n", true, true, false},
		{"unentered header", "#if 0\n#include <unentered.h>\n#endif\nPICK(BASE)\n", "", true, true, false},
		{"string and comment", "const char *text = \"__COUNTER__\"; /* __COUNTER__ */\nPICK(BASE)\n", "", true, true, false},
		{"discarded and stringified", "#define DROP(x)\n#define RAW(x) #x\nDROP(__COUNTER__)\nRAW(__COUNTER__)\nPICK(BASE)\n", "", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				pathname:              "#define PICK(x) CONFIG_ ## x\n" + tc.source,
				"include/unentered.h": "__COUNTER__\n",
			}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", pathname}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return tc.dump, true, nil
			}
			if tc.measured {
				plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__COUNTER__"}, tc.positive))
			}
			context := newConfigDependencyAnalysisContext(plan)
			for range 2 {
				hints, demands, bindingHints := 0, 0, 0
				plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
					if value.OptionalTokenHints && slices.Contains(value.Names, "__COUNTER__") {
						bindingHints++
					}
					if value.CounterCount == 0 {
						return nil
					}
					if !value.OptionalCounterHints {
						demands++
						return nil
					}
					hints++
					if value.CounterCount != 64 || !value.Origin.Source || value.Origin.LogicalPath != pathname || len(value.Origin.ContentID) != 64 || value.Truncated || value.OptionalDefinedness || value.OptionalTokenHints || value.LiteralIncludeHints {
						t.Fatalf("hint lost its immutable entered origin or separate kind: %#v", value)
					}
					return nil
				})
				result, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil || demands != 0 || (hints > 0) != tc.wantHint {
					t.Fatalf("hint readiness or demand boundary: hints=%d demands=%d result=%#v error=%v", hints, demands, result, err)
				}
				if tc.source == blocked && (!result.Opaque || len(result.Symbols)+len(result.SourcePaths)+len(result.ObjectPaths) != 0) {
					t.Fatal("hint supplied a partial source proof")
				}
				if !tc.measured && tc.source == blocked && bindingHints == 0 {
					t.Fatal("early source failure prevented asking about counter availability")
				}
			}
		})
	}
}

func TestCompilerCounterHintLexerOnlyRecognizesIdentifierTokens(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"__COUNTER__\n", true},
		{"#define ID __COUNTER__\n", true},
		{"#if __COUNTER__\n#endif\n", true},
		{"__COUN\\\nTER__\n", true},
		{"\"__COUNTER__\" /* __COUNTER__ */\n", false},
		{"x__COUNTER__ __COUNTER__suffix\n", false},
		{strings.Repeat(" ", configDependencyCompilerGuardHintsMaximumBytes) + "__COUNTER__\n", false},
	} {
		if got := configDependencyCompilerGuardObservationHintsForContents([]byte(tc.source)).counterMention; got != tc.want {
			t.Fatalf("counter spelling %q: %t want %t", tc.source[:min(len(tc.source), 80)], got, tc.want)
		}
	}
}

func TestCompilerCounterProductionScannerDemandsAndRestarts(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const source = "#define PICK(x) CONFIG_ ## x\n#define DROP(x)\n#define RAW(x) #x\nDROP(__COUNTER__)\nRAW(__COUNTER__)\n#if __COUNTER__ == 7\nPICK(NEW)\n#else\nPICK(OLD)\n#endif\n#if __COUNTER__ == 42\nPICK(SECOND)\n#endif\n"
	for _, first := range []string{"1", "7"} {
		t.Run(first, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-c", pathname}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			definedness := configDependencyGuardAnswersForTest(t, plan, node, []string{"__COUNTER__"}, true)
			plan.metadata.SetCompilerGuardAnswers(definedness)
			var demands []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				if value.CounterCount != 0 && !value.OptionalCounterHints {
					demands = append(demands, value)
				}
				return nil
			})
			pending, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !pending.Opaque || len(demands) != 1 || demands[0].CounterCount != 64 || demands[0].Origin.LogicalPath != pathname || !demands[0].Origin.Source {
				t.Fatalf("missing measured sequence did not produce exact reached demand: %#v %#v %v", pending, demands, err)
			}
			counter := compilerCounterAnswersForPlanTest(t, plan, node, demands[0].CounterCount, first+" 42 "+strings.Repeat("9 ", 62))
			answers, err := MergeKbuildCompilerGuardAnswers(definedness, counter)
			if err != nil {
				t.Fatal(err)
			}
			plan.metadata.SetCompilerGuardAnswers(answers)
			demands = nil
			context := newConfigDependencyAnalysisContext(plan)
			for repeat := 0; repeat < 2; repeat++ {
				result, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				want := []string{"CONFIG_OLD", "CONFIG_SECOND"}
				if first == "7" {
					want[0] = "CONFIG_NEW"
				}
				if err != nil || result.Opaque || !slices.Equal(result.Symbols, want) || !slices.Equal(result.SourcePaths, []string{pathname}) || len(demands) != 0 {
					t.Fatalf("fresh measured source proof: %#v demands=%#v error=%v", result, demands, err)
				}
			}
		})
	}
}

func TestCompilerCounterProductionScannerOnlyRequestsActualExpansions(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, body := range []string{"DROP(__COUNTER__)\n", "RAW(__COUNTER__)\n", "#if 0\n__COUNTER__\n#endif\n", "#define __COUNTER__ 73\n#if __COUNTER__ == 73\nPICK(LOCAL)\n#endif\n"} {
		plan, node := configDependencyCompilePlanForTest(t, map[string]string{pathname: "#define PICK(x) CONFIG_ ## x\n#define DROP(x)\n#define RAW(x) #x\nPICK(BASE)\n" + body}, []string{"-nostdinc", "-c", pathname}, nil)
		configDependencyGuardAnswerCompilerForTest(plan, node)
		plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__COUNTER__"}, true))
		demands := 0
		plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
			// Keep actual-expansion assertions separate from speculative hints.
			if value.CounterCount != 0 && !value.OptionalCounterHints {
				demands++
			}
			return nil
		})
		result, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil || result.Opaque || demands != 0 {
			t.Fatalf("unused/inactive/textual counter demanded compiler values: %q %#v %d %v", body, result, demands, err)
		}
	}
}

func TestCompilerCounterProductionScannerExhaustionRequestsBoundedGrowth(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{pathname: "#define PICK(x) CONFIG_ ## x\nPICK(BASE)\n" + strings.Repeat("__COUNTER__\n", 65)}, []string{"-nostdinc", "-c", pathname}, nil)
	configDependencyGuardAnswerCompilerForTest(plan, node)
	definedness := configDependencyGuardAnswersForTest(t, plan, node, []string{"__COUNTER__"}, true)
	counter := compilerCounterAnswersForPlanTest(t, plan, node, 64, strings.Repeat("7 ", 64))
	answers, err := MergeKbuildCompilerGuardAnswers(definedness, counter)
	if err != nil {
		t.Fatal(err)
	}
	plan.metadata.SetCompilerGuardAnswers(answers)
	var demands []int
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		if value.CounterCount != 0 && !value.OptionalCounterHints {
			demands = append(demands, value.CounterCount)
		}
		return nil
	})
	result, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !result.Opaque || len(result.Symbols) != 0 || !slices.Equal(demands, []int{128}) {
		t.Fatalf("exhaustion inferred unmeasured values or lost demand: %#v %v %v", result, demands, err)
	}
	for required, want := range map[int]int{-1: 0, 0: 0, 1: 64, 64: 64, 65: 128, 4096: 4096, 4097: 0} {
		if got := compilerCounterDemandCount(required); got != want {
			t.Fatalf("demand %d=%d want%d", required, got, want)
		}
	}
}
