package kconfig

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyOptionalDefinednessObservesOnlyFirstExpandedUnknown(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, test := range []struct {
		name, source, demand string
	}{
		{"first failure", "__FIRST __SECOND\n", "__FIRST"},
		{"defined operand", "#if defined(__CONDITION)\n0\n#endif\n", "__CONDITION"},
		{"bare defined operand", "#if defined __CONDITION\n0\n#endif\n", "__CONDITION"},
		{"ifdef operand", "#ifdef __CONDITION\n0\n#endif\n", "__CONDITION"},
		{"ifndef operand", "#ifndef __CONDITION\n0\n#endif\n", "__CONDITION"},
		{"inactive defined operand", "#if 0\n#if defined(__INACTIVE)\n0\n#endif\n#endif\n", ""},
		{"defined local operand", "#define __LOCAL 1\n#if defined(__LOCAL)\n0\n#endif\n", ""},
		{"undefined local operand", "#undef __REMOVED\n#if defined(__REMOVED)\n0\n#endif\n", ""},
		{"protected defined replacement", "#define __KNOWN __NOT_EXPANDED\n#if defined(__KNOWN)\n0\n#endif\n", ""},
		{"wrapper prescan", "#define ID(x) x\nID(__WRAPPED)\n", "__WRAPPED"},
		{"pasted name", "#define JOIN(a,b) a ## b\nJOIN(__PA,STED)\n", "__PASTED"},
		{"variadic substitution", "#define V(first, ...) __VA_ARGS__\nV(0, __VARIADIC)\n", "__VARIADIC"},
		{"unused argument", "#define DROP(x) 0\nDROP(__UNUSED)\n", ""},
		{"unused variadic", "#define DROP(first, ...) 0\nDROP(0, __UNUSED)\n", ""},
		{"only variadic", "#define V(...) __VA_ARGS__\nV(__VARIADIC)\n", "__VARIADIC"},
		{"only named variadic", "#define V(args...) args\nV(__VARIADIC)\n", "__VARIADIC"},
		{"unused only variadic", "#define DROP(...) 0\nDROP(__UNUSED)\n", ""},
		{"stringified only variadic", "#define STR(args...) #args\nSTR(__UNUSED)\n", ""},
		{"formal", "#define F(__formal) __formal\nF(7)\n", ""},
		{"unused replacement", "#define UNUSED __HIDDEN\n0\n", ""},
		{"inactive", "#if 0\n__INACTIVE\n#endif\n__ACTIVE\n", "__ACTIVE"},
		{"comments strings", "/* __COMMENT */ \"__STRING\"\n", ""},
		{"source definition", "#define __LOCAL 7\n__LOCAL\n", ""},
		{"source undefinition", "#undef __REMOVED\n__REMOVED\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n" + test.source
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source},
				[]string{"-nostdinc", "-c", pathname})
			baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			var observations []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				if value.OptionalDefinedness {
					observations = append(observations, cloneCompilerGuardObservationForTest(value))
				}
				return nil
			})
			context := newConfigDependencyAnalysisContext(plan)
			for range 2 {
				observations = nil
				set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil {
					t.Fatal(err)
				}
				if set.Opaque != baseline.Opaque || set.Reason != baseline.Reason ||
					!slices.Equal(set.Symbols, baseline.Symbols) || !slices.Equal(set.SourcePaths, baseline.SourcePaths) ||
					!slices.Equal(set.ObjectPaths, baseline.ObjectPaths) {
					t.Fatalf("optional observation changed the current proof: %#v != %#v", set, baseline)
				}
				if test.demand == "" {
					if set.Opaque || len(observations) != 0 || !slices.Equal(set.Symbols, []string{"CONFIG_PREFIX"}) {
						t.Fatalf("structurally consumed tokens became demands: %#v %#v", set, observations)
					}
					continue
				}
				if !set.Opaque || !strings.Contains(set.Reason, test.demand) ||
					len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 || len(observations) != 1 {
					t.Fatalf("first failure published a prefix or skipped demand: %#v %#v", set, observations)
				}
				got := observations[0]
				if !slices.Equal(got.Names, []string{test.demand}) || len(got.Calls) != 0 || got.Truncated ||
					got.ConsumerNodeID != node.ID || got.Scope != "target" || got.Role != "cc" || got.Language != "c" ||
					!slices.Equal(got.Arguments, []string{"-nostdinc"}) ||
					!got.Origin.Source || got.Origin.LogicalPath != pathname ||
					got.Origin.ContentID != fmt.Sprintf("%x", sha256.Sum256([]byte(source))) {
					t.Fatalf("typed demand lost exact reached-source context: %#v", got)
				}
			}
		})
	}
}

func TestConfigDependencyOptionalDefinednessUnsupportedVariadicStaysOpaque(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, suffix := range []string{
		"#define V(..., x) x\nV(__VARIADIC, 0)\n",
		"#define DROP(args..., x) 0\nDROP(__UNUSED, 0)\n",
	} {
		source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n" + suffix
		plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-c", pathname})
		baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil {
			t.Fatal(err)
		}
		var observations []ConfigDependencyCompilerGuardObservation
		plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
			observations = append(observations, cloneCompilerGuardObservationForTest(value))
			return nil
		})
		context := newConfigDependencyAnalysisContext(plan)
		for range 2 {
			observations = nil
			set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil || !baseline.Opaque || !set.Opaque || set.Reason != baseline.Reason ||
				!strings.Contains(set.Reason, "unsupported variadic signature") ||
				len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 {
				t.Fatalf("unsupported signature became a partial expansion proof: %#v %v", set, err)
			}
			for _, observation := range observations {
				if observation.OptionalDefinedness || slices.Contains(observation.Names, "__VA_ARGS__") {
					t.Fatal("unsupported variadic signature emitted an expansion demand or queried a macro formal")
				}
			}
		}
	}
}

func TestConfigDependencyOptionalDefinednessFinalReplayStillNeedsAnswer(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n__LAST_ROUND_UNKNOWN\n"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source},
		[]string{"-nostdinc", "-c", pathname})
	// Three completed empty rounds provide no negative fact for a binding
	// first reached during final replay. The coordinator cannot add a round.
	empty := configDependencyGuardAnswersForTest(t, plan, node, nil, false)
	answers, err := MergeKbuildCompilerGuardAnswers(empty, empty, empty)
	if err != nil {
		t.Fatal(err)
	}
	plan.metadata.SetCompilerGuardAnswers(answers)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || !strings.Contains(set.Reason, "__LAST_ROUND_UNKNOWN") ||
		len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 {
		t.Fatalf("exhausted rounds supplied implicit absence: %#v %v", set, err)
	}
	// A separate future measured answer may enable a fresh original-source
	// proof. This does not claim that the fixed production chain reaches it.
	measured := configDependencyGuardAnswersForTest(t, plan, node, []string{"__LAST_ROUND_UNKNOWN"}, false)
	plan.metadata.SetCompilerGuardAnswers(measured)
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_PREFIX"}) {
		t.Fatalf("measured absence failed fresh replay: %#v %v", set, err)
	}
}

func TestConfigDependencyOptionalDefinednessCoexistsWithBaselineFirstRound(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n__FIRST\n#if defined(__LATE_STATIC)\n#endif\n"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-c", pathname})
	var observations []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		observations = append(observations, cloneCompilerGuardObservationForTest(value))
		return nil
	})
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 {
		t.Fatalf("optional demand granted current precision: %#v %v", set, err)
	}
	optional, baseline := 0, 0
	for _, observation := range observations {
		if observation.Truncated {
			t.Fatal("bounded fixture unexpectedly truncated")
		}
		if observation.OptionalDefinedness {
			optional++
			if !slices.Equal(observation.Names, []string{"__FIRST"}) {
				t.Fatal("optional demand changed")
			}
		} else if slices.Contains(observation.Names, "__LATE_STATIC") {
			if !observation.Origin.Source || observation.Origin.LogicalPath != pathname ||
				observation.Origin.ContentID != fmt.Sprintf("%x", sha256.Sum256([]byte(source))) ||
				observation.Scope != "target" || observation.Role != "cc" || observation.Language != "c" ||
				!slices.Equal(observation.Arguments, []string{"-nostdinc"}) {
				t.Fatal("baseline observation changed its reached-source/compiler context")
			}
			baseline++
		}
	}
	// Fast-path analysis and its complete-call fallback may both emit the same
	// valid baseline hint. The coordinator deduplicates it by exact context;
	// observation multiplicity is not an invariant of the scanner.
	if optional != 1 || baseline == 0 {
		t.Fatalf("first-round failure lost baseline or optional observation: optional=%d baseline=%d", optional, baseline)
	}
}

func TestConfigDependencyOptionalDefinednessRejectsFailedVectorFacts(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname: "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n__UNQUERYABLE\n",
	}, []string{"-nostdinc", "-c", pathname})
	scope, role, probe := configDependencyGuardAnswerProbeForTest(t, plan, node)
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"__UNQUERYABLE"}
	if _, _, err := batch.OptionalCompilerDefinedness(scope, role, probe.language, probe.arguments, probe.translationUnits, names, probe.environment); err != nil {
		t.Fatal(err)
	}
	frozen, err := batch.Plan()
	if err != nil {
		t.Fatal(err)
	}
	plan.Toolsets = maps.Clone(frozen.Toolsets)
	oracle := optionalCompilerDefinednessOracleForTest(t, frozen, map[string]string{"compiler-definedness": "0"})
	for id, result := range oracle.results {
		*result.Boolean = false
		result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
		oracle.results[id] = result
	}
	batch, err = NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if values, state, err := batch.OptionalCompilerDefinedness(scope, role, probe.language, probe.arguments, probe.translationUnits, names, probe.environment); err != nil || values != nil || state != OptionalCompilerDefinednessUnqueryable {
		t.Fatalf("failed vector status: %#v %v %v", values, state, err)
	}
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	plan.metadata.SetCompilerGuardAnswers(answers)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || !strings.Contains(set.Reason, "__UNQUERYABLE") ||
		len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 {
		t.Fatalf("failed parseable stdout granted a namespace or closure: %#v %v", set, err)
	}
}

func cloneCompilerGuardObservationForTest(value ConfigDependencyCompilerGuardObservation) ConfigDependencyCompilerGuardObservation {
	value.Arguments = slices.Clone(value.Arguments)
	value.TranslationUnits = slices.Clone(value.TranslationUnits)
	value.Environment = maps.Clone(value.Environment)
	value.Names = slices.Clone(value.Names)
	value.Calls = slices.Clone(value.Calls)
	return value
}

func TestConfigDependencyCompilerIntrinsicLiteralObservationHints(t *testing.T) {
	contents := []byte("#ifdef _BASE\n#endif\n" + strings.Repeat(" ", 9000) + `
/* __has_attribute(comment) */
const char *text = "__has_attribute(quoted)";
#if __has_attribute(zebra) || __has_attribute(deprecated)
#endif
%:if __has_attribute(__retain__)
%:endif
__has_attribute(zebra)
__has_attribute()
__has_attribute(a,b)
__has_attribute(42)
__has_attribute(a::b)
__has_attribute(__has_attribute)
` + "#if __has_att\\\nribute(__ret\\\nain__)\n#endif\n")
	hints := configDependencyCompilerGuardObservationHintsForContents(contents)
	wantCalls := []CompilerIntrinsicCall{
		{Operator: "__has_attribute", Operand: "__retain__"},
		{Operator: "__has_attribute", Operand: "deprecated"},
		{Operator: "__has_attribute", Operand: "zebra"},
	}
	if hints.truncated || !slices.Equal(hints.names, []string{"_BASE", "__has_attribute", "__retain__"}) ||
		!slices.Equal(hints.calls, wantCalls) {
		t.Fatalf("literal intrinsic hints = %#v, want bounded sorted distinct calls %#v", hints, wantCalls)
	}
	// The original guard parser remains a reserved definedness-hint parser.
	ordinary := configDependencyCompilerGuardHintsForContents(contents)
	if !slices.Equal(ordinary.names, []string{"_BASE"}) || len(ordinary.calls) != 0 {
		t.Fatalf("intrinsic hints changed the original parser contract: %#v", ordinary)
	}
}

func TestConfigDependencyCompilerIntrinsicHintsRejectUnmodeledFileIntrospection(t *testing.T) {
	for _, operator := range []string{"__has_include", "__has_embed"} {
		t.Run(operator, func(t *testing.T) {
			contents := []byte("#ifdef _PREFIX\n#endif\n#if __has_attribute(deprecated)\n#endif\n#if " + operator + "(file)\n#endif\n")
			// Shared phase normalization rejects the whole immutable input;
			// neither a valid prefix nor another intrinsic is evidence that the
			// remaining preprocessing effects can be modeled.
			hints := configDependencyCompilerGuardObservationHintsForContents(contents)
			if hints.truncated || len(hints.names) != 0 || len(hints.calls) != 0 {
				t.Fatalf("unmodeled file introspection retained partial hints: %#v", hints)
			}
		})
	}
}

func TestCompilerIntrinsicMixedLiteralHintsAndOperatorCollisions(t *testing.T) {
	contents := []byte(`
/* __has_builtin(comment) */
const char *text = "__has_builtin(quoted)";
#if __has_builtin(__builtin_dynamic_object_size) || __has_attribute(deprecated)
#endif
__has_builtin(unknown_builtin)
__has_builtin(__builtin_dynamic_object_size)
__has_builtin(__has_attribute)
__has_attribute(__has_builtin)
__has_builtin(a,b)
__has_builtin(42)
` + "__has_bui\\\nltin(unknown_builtin)\n")
	hints := configDependencyCompilerGuardObservationHintsForContents(contents)
	want := []CompilerIntrinsicCall{
		{"__has_attribute", "deprecated"},
		{"__has_builtin", "__builtin_dynamic_object_size"},
		{"__has_builtin", "unknown_builtin"},
	}
	if hints.truncated || !slices.Equal(hints.calls, want) ||
		!slices.Equal(hints.names, []string{"__builtin_dynamic_object_size", "__has_attribute", "__has_builtin"}) {
		t.Fatalf("mixed hints = %#v, want %#v", hints, want)
	}
}

func TestCompilerIntrinsicStaticReadinessIsPerOperator(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const source = "#include UNMODELED_HEADER\n#if __has_attribute(deprecated) || __has_builtin(__builtin_dynamic_object_size)\n#endif\n"
	for _, attributeMode := range []string{"unknown", "absent", "textual", "measured"} {
		for _, builtinMode := range []string{"unknown", "absent", "textual", "measured"} {
			t.Run(attributeMode+"/"+builtinMode, func(t *testing.T) {
				plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-Werror", "-c", pathname})
				definitions := map[string]bool{"__builtin_dynamic_object_size": false}
				var predefines strings.Builder
				modes := map[string]string{"__has_attribute": attributeMode, "__has_builtin": builtinMode}
				var want []CompilerIntrinsicCall
				for _, call := range []CompilerIntrinsicCall{{"__has_attribute", "deprecated"}, {"__has_builtin", "__builtin_dynamic_object_size"}} {
					mode := modes[call.Operator]
					if mode != "unknown" {
						definitions[call.Operator] = mode != "absent"
					}
					if mode == "textual" {
						fmt.Fprintf(&predefines, "#define %s(x) 0\n", call.Operator)
					}
					if mode == "measured" {
						want = append(want, call)
					}
				}
				plan.metadata.sourceGuardNames = slices.Sorted(maps.Keys(definitions))
				plan.metadata.sourceGuardNamesReady = true
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return predefines.String(), true, nil
				}
				plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
					return maps.Clone(definitions), true, nil
				}
				var calls []CompilerIntrinsicCall
				plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
					calls = append(calls, value.Calls...)
					return nil
				})
				context := newConfigDependencyAnalysisContext(plan)
				for range 2 {
					calls = nil
					set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
					if err != nil || !set.Opaque || !slices.Equal(calls, want) {
						t.Fatalf("independent warm/cold readiness = %#v/%v, calls %#v, want %#v", set, err, calls, want)
					}
				}
			})
		}
	}
}

func TestConfigDependencyCompilerIntrinsicLiteralHintBudgets(t *testing.T) {
	const operator, operand = "__has_attribute", "deprecated"
	call := operator + "(" + operand + ")\n"
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("items-overflow-%d", extra), func(t *testing.T) {
			var contents strings.Builder
			for index := range configDependencyCompilerGuardHintsMaximumNames - 2 + extra {
				fmt.Fprintf(&contents, "#ifdef _BOUND_%04d\n#endif\n", index)
			}
			contents.WriteString(call)
			hints := configDependencyCompilerGuardObservationHintsForContents([]byte(contents.String()))
			if extra == 0 {
				if hints.truncated || len(hints.names)+len(hints.calls) != configDependencyCompilerGuardHintsMaximumNames {
					t.Fatalf("exact item bound rejected: names=%d calls=%d truncated=%t", len(hints.names), len(hints.calls), hints.truncated)
				}
			} else if !hints.truncated || len(hints.names) != 0 || len(hints.calls) != 0 {
				t.Fatalf("item overflow published a partial batch: %#v", hints)
			}
		})
		t.Run(fmt.Sprintf("bytes-overflow-%d", extra), func(t *testing.T) {
			nameBytes := configDependencyCompilerGuardHintsMaximumNameBytes - 2*len(operator) - len(operand) + extra
			contents := "#ifdef _" + strings.Repeat("B", nameBytes-1) + "\n#endif\n" + call
			hints := configDependencyCompilerGuardObservationHintsForContents([]byte(contents))
			if extra == 0 {
				if hints.truncated || len(hints.names) != 2 || len(hints.calls) != 1 {
					t.Fatalf("exact byte bound rejected: names=%d calls=%d truncated=%t", len(hints.names), len(hints.calls), hints.truncated)
				}
			} else if !hints.truncated || len(hints.names) != 0 || len(hints.calls) != 0 {
				t.Fatal("retained byte overflow published a partial batch")
			}
		})
	}
	contents := call + strings.Repeat(" ", configDependencyCompilerGuardHintsMaximumBytes-len(call))
	if hints := configDependencyCompilerGuardObservationHintsForContents([]byte(contents)); hints.truncated || len(hints.calls) != 1 {
		t.Fatal("exact source byte limit rejected")
	}
	if hints := configDependencyCompilerGuardObservationHintsForContents([]byte(contents + " ")); !hints.truncated || len(hints.names)+len(hints.calls) != 0 {
		t.Fatal("source byte overflow published a partial batch")
	}
}

func TestConfigDependencyCompilerIntrinsicObservationReachedOpaqueSource(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	source := "#include UNMODELED_HEADER\n#if __has_attribute(deprecated)\n#endif\n#if __has_attribute(__retain__)\n#endif\n#ifdef __EXTRA_UNKNOWN\n#endif\n"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname:              source,
		"include/unreached.h": "#if __has_attribute(unreached)\n#endif\n",
	}, []string{"-nostdinc", "-DDATE=202311", "-c", pathname})
	plan.metadata.sourceGuardNames = []string{"__has_attribute", "__retain__"}
	plan.metadata.sourceGuardNamesReady = true
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
		return map[string]bool{"__has_attribute": true, "__retain__": false}, true, nil
	}
	context := newConfigDependencyAnalysisContext(plan)
	var observations []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		observations = append(observations, cloneCompilerGuardObservationForTest(value))
		if len(value.Calls) != 0 {
			value.Calls[0].Operand = "changed"
		}
		return nil
	})
	for range 2 {
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil || !set.Opaque || len(set.Symbols)+len(set.SourcePaths)+len(set.ObjectPaths) != 0 {
			t.Fatalf("static intrinsic hints became authority: %#v, %v", set, err)
		}
	}
	if len(observations) != 4 || !reflect.DeepEqual(observations[:2], observations[2:]) {
		t.Fatalf("callback mutation or unreached file changed observed hints: %#v", observations)
	}
	if !slices.Equal(observations[0].Names, []string{"__EXTRA_UNKNOWN"}) || len(observations[0].Calls) != 0 || observations[0].Origin != observations[1].Origin {
		t.Fatalf("mixed hints were not split into separate query kinds: %#v", observations[:2])
	}
	got := observations[1]
	if !slices.Equal(got.Calls, []CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "__retain__"}, {Operator: "__has_attribute", Operand: "deprecated"}}) ||
		len(got.Names) != 0 ||
		!slices.Equal(got.Arguments, []string{"-nostdinc", "-DDATE=202311"}) ||
		!got.Origin.Source || got.Origin.LogicalPath != pathname || got.Origin.ContentID != fmt.Sprintf("%x", sha256.Sum256([]byte(source))) {
		t.Fatalf("intrinsic observation lost exact calls/context/bytes: %#v", got)
	}
}

func TestConfigDependencyCompilerIntrinsicStaticSchedulingRequiresInitialBinding(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, mode := range []string{"unmeasured", "absent", "textual", "builtin"} {
		t.Run(mode, func(t *testing.T) {
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				pathname: "#include UNMODELED_HEADER\n#if __has_attribute(deprecated) || __has_attribute(__retain__) || __has_attribute(__LINE__)\n#endif\n",
			}, []string{"-nostdinc", "-Werror", "-c", pathname})
			predefines := "#define __LINE__ 1\n"
			if mode == "textual" {
				predefines += "#define __has_attribute(x) 0\n"
			}
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return predefines, true, nil
			}
			if mode != "unmeasured" {
				plan.metadata.sourceGuardNames = []string{"__has_attribute"}
				plan.metadata.sourceGuardNamesReady = true
				plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
					return map[string]bool{"__has_attribute": mode != "absent"}, true, nil
				}
			}
			var calls []CompilerIntrinsicCall
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				calls = append(calls, value.Calls...)
				return nil
			})
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !set.Opaque {
				t.Fatalf("static binding gate changed source proof: %#v, %v", set, err)
			}
			var want []CompilerIntrinsicCall
			if mode == "builtin" {
				want = []CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "deprecated"}}
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("static query admitted an unproved operator/operand: got %#v, want %#v", calls, want)
			}
		})
	}
}

func TestConfigDependencyCompilerIntrinsicStaticHintsSkipMeasuredCalls(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, _ := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname: "#if __has_attribute(deprecated) || __has_attribute(retain)\n#endif\n",
	}, []string{"-nostdinc", "-c", pathname})
	state, reason := parseConfigDependencyCompilerPredefines("")
	if reason != "" {
		t.Fatal(reason)
	}
	scanner := configDependencyClosureScanner{
		sourceLookup: newConfigDependencySourceLookup(plan.selectionGraph.profiles["config-dependency"]), physicalFiles: &configDependencyPhysicalFileCache{},
		collectCompilerGuards: true, compilerIntrinsicInitialAvailable: map[string]bool{"__has_attribute": true},
		compilerIntrinsicInitialSnapshot: state.snapshot,
		compilerGuardInitialDefinitions:  map[string]bool{"__has_attribute": true},
	}
	file, found := scanner.sourceFile(pathname)
	if !found {
		t.Fatal("missing immutable literal-call fixture")
	}
	scanner.rememberCompilerGuardFile(file)
	scanner.compilerIntrinsicAnswered = func(call CompilerIntrinsicCall) (bool, error) {
		return call.Operand == "deprecated", nil
	}
	var got []CompilerIntrinsicCall
	for range 2 {
		got = nil
		scanner.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
			if len(hints.names) != 0 {
				t.Fatal("measured initial operator was queried again")
			}
			got = append(got, hints.calls...)
		})
		if !slices.Equal(got, []CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "retain"}}) {
			t.Fatalf("measured-call filtering changed outstanding hints: %#v", got)
		}
	}
	scanner.compilerIntrinsicAnswered = func(CompilerIntrinsicCall) (bool, error) { return true, nil }
	scanner.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, _ configDependencyCompilerGuardHints, _ string) {
		t.Fatal("completed static calls kept the next round nonempty")
	})
}

func TestConfigDependencyCompilerIntrinsicPendingDemandUsesCurrentFile(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname:           "#include <nested.h>\n",
		"include/nested.h": "#if __has_attribute(deprecated)\n#endif\n",
	}, []string{"-nostdinc", "-c", pathname})
	context := newConfigDependencyAnalysisContext(plan)
	scanner := configDependencyClosureScanner{
		sourceLookup: newConfigDependencySourceLookup(plan.selectionGraph.profiles["config-dependency"]), physicalFiles: context.physicalFiles,
		callCoverage: &configDependencyCallCoverage{},
	}
	file, found := scanner.sourceFile("include/nested.h")
	if !found {
		t.Fatal("missing immutable current-file fixture")
	}
	scanner.compilerIntrinsicFile = file
	probe := configDependencyCompilerPredefineProbe{language: "c", arguments: []string{"-nostdinc", "-DVALUE=202311"}}
	var observations []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		observations = append(observations, value)
		return nil
	})
	scanner.configureCompilerIntrinsicQueries(plan, node, "cc", probe, func(probe configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
		context.recordCompilerGuardHints(plan, node, "cc", probe, file, hints, contentID)
	})
	call := CompilerIntrinsicCall{Operator: "__has_attribute", Operand: "deprecated"}
	token, identity, reason := scanner.callCoverage.intrinsic(call)
	if token != "" || identity != "" || !strings.Contains(reason, "unavailable") || context.compilerGuardError != nil {
		t.Fatalf("pending intrinsic demand supplied an answer: %q, %q, %q, %v", token, identity, reason, context.compilerGuardError)
	}
	if len(observations) != 1 || len(observations[0].Names) != 0 || !slices.Equal(observations[0].Calls, []CompilerIntrinsicCall{call}) ||
		observations[0].Origin.LogicalPath != file.logical || !slices.Equal(observations[0].Arguments, probe.arguments) {
		t.Fatalf("pending call used the wrong reached-file/context: %#v", observations)
	}
	// Final replay cannot turn the same missing answer into a discovery round.
	scanner.configureCompilerIntrinsicQueries(plan, node, "cc", probe, nil)
	if token, _, reason := scanner.callCoverage.intrinsic(call); token != "" || reason == "" || len(observations) != 1 {
		t.Fatal("final replay supplied or discovered an unmeasured intrinsic answer")
	}
	probe.language = "assembler-with-cpp"
	scanner.configureCompilerIntrinsicQueries(plan, node, "cc", probe, nil)
	if scanner.callCoverage.intrinsic != nil {
		t.Fatal("assembler source retained an unsupported intrinsic oracle")
	}
	scanner.collectCompilerGuards = true
	scanner.rememberCompilerGuardFile(file)
	scanner.emitCompilerGuardHints(probe, func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
		if len(hints.calls) != 0 {
			t.Fatal("assembler source scheduled an unsupported intrinsic call")
		}
	})
}

func compilerGuardObservationPlanForTest(t *testing.T, files map[string]string, arguments []string) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	plan, node := configDependencyCompilePlanForTest(t, files, arguments, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	return plan, node
}

func TestConfigDependencyCompilerGuardObservationLateOpaqueSource(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	source := "#include UNMODELED_HEADER\n" + strings.Repeat(" ", 9000) + "\n#ifndef __LATE_SOURCE_GUARD\n#endif\n"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname:                  source,
		"include/never-entered.h": "#ifndef __UNREACHED_DIRECTORY_GUARD\n#endif\n",
	}, []string{"-nostdinc", "-std=gnu11", "-DCONTEXT=1", "-U", "CONTEXT", "-c", pathname})
	recipe := plan.Recipes[node.Recipe]
	recipe.Environment = map[string]string{"GUARD_WRAPPER_CONTEXT": "ordered-context"}
	plan.Recipes[node.Recipe] = recipe
	var observations []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		observations = append(observations, value)
		return nil
	})
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
		t.Fatalf("opaque source published a partial proof: %#v, %v", set, err)
	}
	if len(observations) != 1 {
		t.Fatalf("only the entered source should emit a hint: %#v", observations)
	}
	want := ConfigDependencyCompilerGuardObservation{
		ConsumerNodeID: node.ID, Scope: "target", Role: "cc", Language: "c",
		Arguments:   []string{"-nostdinc", "-std=gnu11", "-DCONTEXT=1", "-U", "CONTEXT"},
		Environment: map[string]string{"GUARD_WRAPPER_CONTEXT": "ordered-context"},
		Names:       []string{"__LATE_SOURCE_GUARD"},
		Origin: ConfigDependencyCompilerGuardOrigin{
			Source: true, LogicalPath: pathname, ContentID: fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
		},
	}
	got := observations[0]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("late source observation lost exact compiler or byte context:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestConfigDependencyCompilerGuardObservationCallbackCannotMutateWarmState(t *testing.T) {
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifndef __CALLBACK_GUARD\n#include UNMODELED_HEADER\n#endif\n",
	}, []string{"-nostdinc", "-DCONTEXT=1", "-c", "drivers/example/driver.c"})
	recipe := plan.Recipes[node.Recipe]
	recipe.Environment = map[string]string{"GUARD_WRAPPER_CONTEXT": "original"}
	plan.Recipes[node.Recipe] = recipe
	context := newConfigDependencyAnalysisContext(plan)
	var before []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		before = append(before, cloneCompilerGuardObservationForTest(value))
		value.Arguments[0] = "-DCHANGED"
		value.Names[0] = "__CALLBACK_MUTATION"
		value.Environment["GUARD_WRAPPER_CONTEXT"] = "changed"
		return nil
	})
	for range 2 {
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil || !set.Opaque {
			t.Fatalf("callback changed analysis result: %#v, %v", set, err)
		}
	}
	if len(before) != 2 || !reflect.DeepEqual(before[0], before[1]) ||
		!slices.Equal(before[0].Names, []string{"__CALLBACK_GUARD"}) {
		t.Fatalf("callback mutated cached hints or compiler context: %#v", before)
	}
	if !slices.Equal(plan.Recipes[node.Recipe].Arguments, recipe.Arguments) ||
		!maps.Equal(plan.Recipes[node.Recipe].Environment, recipe.Environment) {
		t.Fatal("observer modified the original recipe")
	}
	// A symbolic projection may retain translation units even though the
	// static production fixture above removes its concrete source argument.
	// Exercise defensive ownership of that field at the same emission hook.
	scanner := configDependencyClosureScanner{
		sourceLookup: newConfigDependencySourceLookup(plan.selectionGraph.profiles["config-dependency"]), physicalFiles: context.physicalFiles,
	}
	file, found := scanner.sourceFile("drivers/example/driver.c")
	if !found {
		t.Fatal("immutable callback fixture source missing")
	}
	contents, err := file.contents()
	if err != nil {
		t.Fatal(err)
	}
	probe := configDependencyCompilerPredefineProbe{
		language: "c", arguments: []string{"-nostdinc"}, translationUnits: []string{"symbolic-source.c"},
		environment: map[string]string{"GUARD_WRAPPER_CONTEXT": "original"},
	}
	hints := configDependencyCompilerGuardHints{names: []string{"__CALLBACK_GUARD"}}
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		value.TranslationUnits[0] = "changed.c"
		value.Arguments[0] = "-DCHANGED"
		value.Environment["GUARD_WRAPPER_CONTEXT"] = "changed"
		value.Names[0] = "__CHANGED"
		return nil
	})
	context.recordCompilerGuardHints(plan, node, "cc", probe, file, hints, fmt.Sprintf("%x", sha256.Sum256(contents)))
	if context.compilerGuardError != nil || probe.translationUnits[0] != "symbolic-source.c" ||
		probe.arguments[0] != "-nostdinc" || probe.environment["GUARD_WRAPPER_CONTEXT"] != "original" || hints.names[0] != "__CALLBACK_GUARD" {
		t.Fatalf("emission borrowed caller storage: %#v, %#v, %v", probe, hints, context.compilerGuardError)
	}
}

func TestConfigDependencyCompilerGuardObservationWarmHeaderCaches(t *testing.T) {
	for _, mode := range []string{"ordinary", "forced"} {
		t.Run(mode, func(t *testing.T) {
			source := "#include <bridge.h>\nCONFIG_UNIT\n"
			args := []string{"-nostdinc", "-I${tree:kernel}/include"}
			if mode == "forced" {
				source = "CONFIG_UNIT\n"
				args = append(args, "-include", "${tree:kernel}/include/bridge.h")
			}
			args = append(args, "-c", "drivers/example/driver.c")
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				"drivers/example/driver.c": source,
				"include/bridge.h":         "#ifndef __BRIDGE_GUARD\n#define __BRIDGE_GUARD\n#include \"nested.h\"\n#if 0\n#ifdef __BRIDGE_HINT\n#endif\n#endif\n#endif\n",
				"include/nested.h":         "#ifndef __NESTED_GUARD\n#define __NESTED_GUARD\nCONFIG_NESTED\n#if 0\n#ifdef __NESTED_HINT\n#endif\n#endif\n#endif\n",
				"include/unreached.h":      "#ifndef __UNREACHED_GUARD\n#endif\n",
			}, args)
			plan.metadata.sourceGuardNames = []string{"__BRIDGE_GUARD", "__NESTED_GUARD"}
			plan.metadata.sourceGuardNamesReady = true
			plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
				values := make(map[string]bool, len(names))
				for _, name := range names {
					values[name] = false
				}
				return values, true, nil
			}
			context := newConfigDependencyAnalysisContext(plan)
			// Warm the production caches before enabling optional discovery, so
			// cached syntax has no precomputed guard-hint payload to borrow.
			seed, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil || seed.Opaque {
				t.Fatalf("cache seed = %#v, %v", seed, err)
			}
			var got []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				got = append(got, value)
				return nil
			})
			hits := func(c *configDependencyAnalysisContext) int {
				if mode == "forced" {
					return c.forcedHeaders.hits
				}
				return c.forcedHeaders.ordinary.hits
			}
			before := hits(context)
			warm, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil || !configDependencySetsEqual(seed, warm) || hits(context) <= before {
				t.Fatalf("did not replay a warm %s header cache: %#v, hits %d -> %d, %v", mode, warm, before, hits(context), err)
			}
			warmHints := slices.Clone(got)
			got = nil
			cold, err := analyzeActionPlanNodeConfigDependencies(plan, node, newConfigDependencyAnalysisContext(plan))
			if err != nil || !configDependencySetsEqual(warm, cold) || !reflect.DeepEqual(warmHints, got) {
				t.Fatalf("warm %s cache lost nested hints:\nwarm %#v\ncold %#v\n%v", mode, warmHints, got, err)
			}
			if len(got) != 2 {
				t.Fatalf("unentered file was inventoried or nested entered file lost: %#v", got)
			}
			byPath := map[string][]string{}
			for _, observation := range got {
				byPath[observation.Origin.LogicalPath] = observation.Names
			}
			if !reflect.DeepEqual(byPath, map[string][]string{
				"include/bridge.h": {"__BRIDGE_HINT"}, "include/nested.h": {"__NESTED_HINT"},
			}) {
				t.Fatalf("unexpected reached guard closure: %#v", byPath)
			}
		})
	}
}

func TestConfigDependencyCompilerGuardObservationSkipsOnlyMeasuredInitialNames(t *testing.T) {
	for _, mutateSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("source_mutations_%t", mutateSource), func(t *testing.T) {
			source := "#if 0\n#if defined(__KNOWN_TRUE) || defined(__KNOWN_FALSE)\n#endif\n#endif\nCONFIG_UNIT\n"
			var want []string
			if mutateSource {
				// These writes change the interpreted namespace, not what the
				// compiler has measured about its initial reserved-name state.
				source = "#undef __KNOWN_TRUE\n#define __KNOWN_FALSE 1\n" +
					"#define __SOURCE_DEFINED 1\n#undef __SOURCE_UNDEFINED\n" +
					"#if 0\n#if defined(__KNOWN_TRUE) || defined(__KNOWN_FALSE) || defined(__SOURCE_DEFINED) || defined(__SOURCE_UNDEFINED) || defined(__UNKNOWN)\n#endif\n#endif\nCONFIG_UNIT\n"
				want = []string{"__SOURCE_DEFINED", "__SOURCE_UNDEFINED", "__UNKNOWN"}
			}
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				"drivers/example/driver.c": source,
			}, []string{"-nostdinc", "-c", "drivers/example/driver.c"})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "#define __KNOWN_TRUE 1\n", true, nil
			}
			plan.metadata.sourceGuardNames = []string{"__KNOWN_FALSE", "__KNOWN_TRUE"}
			plan.metadata.sourceGuardNamesReady = true
			measured := map[string]bool{"__KNOWN_FALSE": false, "__KNOWN_TRUE": true}
			calls := 0
			plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
				calls++
				if !slices.Equal(names, plan.metadata.sourceGuardNames) {
					t.Fatalf("observer changed the ordinary probe inventory: %q", names)
				}
				return measured, true, nil
			}
			context := newConfigDependencyAnalysisContext(plan)
			// Whole-plan Build initializes this replay cache explicitly; the
			// standalone per-node entry point deliberately leaves it disabled.
			// Exercise the production cached-query path, not three uncached scans.
			context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
			seed, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil || seed.Opaque {
				t.Fatalf("ordinary scan = %#v, %v", seed, err)
			}
			for _, syntax := range context.conditionalSyntax {
				if syntax.compilerGuardHintsReady {
					t.Fatal("nil observer inventoried optional guard hints")
				}
			}
			for range 2 {
				var got []string
				plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
					got = append(got, value.Names...)
					return nil
				})
				set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil || !configDependencySetsEqual(seed, set) || !slices.Equal(got, want) {
					t.Fatalf("measured-name filter changed proof or unknown queries: %#v, got %q, want %q, %v", set, got, want, err)
				}
			}
			if calls != 1 || !maps.Equal(measured, map[string]bool{"__KNOWN_FALSE": false, "__KNOWN_TRUE": true}) {
				t.Fatalf("filter repeated a cached query or mutated initial facts: calls %d, %#v", calls, measured)
			}
		})
	}
}

func TestConfigDependencyCompilerGuardObservationErrorsAreAnalysisErrors(t *testing.T) {
	plan, _ := compilerGuardObservationPlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifndef __OBSERVER_ERROR\n#include UNMODELED_HEADER\n#endif\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"})
	want := errors.New("reject supplemental guard ledger")
	calls := 0
	plan.metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error {
		calls++
		return want
	})
	analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if analysis != nil || !errors.Is(err, want) || calls != 1 {
		t.Fatalf("observer error became an opaque success: %#v, calls %d, %v", analysis, calls, err)
	}
}

func TestConfigDependencyCompilerGuardObservationBypassesCompletedCache(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#ifdef __COMPILER_MARK\nCONFIG_RELEVANT\n#endif\n")
	plan, node := configDependencyCompletedCachePlanForTest(t, root, "guard-observation", "compile-guard", "src-00000001", "image", "target-toolset", map[string]string{"CONFIG_RELEVANT": "y"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define __COMPILER_MARK 1\n", true, nil
	}
	familyCache := NewActionPlanFamilyPlanningCache()
	first := configDependencyBuildWithFamilyCacheForTest(t, plan, familyCache)[node.ID]
	second := configDependencyBuildWithFamilyCacheForTest(t, plan, familyCache)[node.ID]
	cache := familyCache.configDependencyCache()
	if first.Opaque || !configDependencySetsEqual(first, second) || cache.completed.stores == 0 || cache.completed.hits == 0 {
		t.Fatalf("test did not seed and hit a completed compiler entry: %#v, %#v", first, cache.completed)
	}
	before := [3]int{cache.completed.hits, cache.completed.misses, cache.completed.stores}
	var observations []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		observations = append(observations, value)
		return nil
	})
	third := configDependencyBuildWithFamilyCacheForTest(t, plan, familyCache)[node.ID]
	after := [3]int{cache.completed.hits, cache.completed.misses, cache.completed.stores}
	if !configDependencySetsEqual(first, third) || before != after || len(observations) != 1 ||
		!slices.Equal(observations[0].Names, []string{"__COMPILER_MARK"}) {
		t.Fatalf("completed cache suppressed active discovery: stats %v -> %v, hints %#v, set %#v", before, after, observations, third)
	}
	plan.metadata.SetCompilerGuardObserver(nil)
	configDependencyBuildWithFamilyCacheForTest(t, plan, familyCache)
	if cache.completed.hits != before[0]+1 {
		t.Fatal("disabling optional observer did not restore ordinary completed-cache reuse")
	}
}

func TestConfigDependencyCompilerGuardObservationVerifiedGeneratedOrigin(t *testing.T) {
	for _, mode := range []string{"direct", "work", "tree"} {
		t.Run(mode, func(t *testing.T) {
			plan, consumer, producer := observedDependencyPlanForTest(t, mode, "#include <selected.h>\n")
			contents := "#ifndef __OBSERVED_GENERATED_GUARD\n#include UNMODELED_HEADER\n#endif\n"
			replay, observed := observedDependencyGateForTest(t, plan, producer, contents)
			var got []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				got = append(got, value)
				return nil
			})
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || !sets[consumer].Opaque || len(got) != 1 {
				t.Fatalf("opaque generated source lost hint: sets %#v, hints %#v, %v", sets, got, err)
			}
			wantOrigin := ConfigDependencyCompilerGuardOrigin{
				LogicalPath: "include/generated/selected.h", ContentID: observed.Headers()[0].ContentID,
				OriginalProducerNodeID: replay.originalIDs[producer], Slot: 0,
				Tree: "prep", OutputPath: "include/generated/selected.h",
			}
			if got[0].ConsumerNodeID != consumer || got[0].Origin != wantOrigin ||
				!slices.Equal(got[0].Names, []string{"__OBSERVED_GENERATED_GUARD"}) {
				t.Fatalf("generated hint lost verified owner: %#v, want %#v", got, wantOrigin)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(uses) != 0 {
				t.Fatalf("pending hint became a successful actual-read receipt: %#v, %v", uses, err)
			}
		})
	}
}

func TestConfigDependencyCompilerGuardObservationRejectsForgedGeneratedInputs(t *testing.T) {
	for _, failure := range []string{"producer", "slot", "contents", "content-id"} {
		t.Run(failure, func(t *testing.T) {
			plan, consumer, producer := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
			replay, observed := observedDependencyGateForTest(t, plan, producer, "#ifndef __OBSERVED_GUARD\n#endif\n")
			altered := *observed
			altered.headers = observed.Headers()
			altered.contents = maps.Clone(observed.contents)
			switch failure {
			case "producer":
				altered.headers[0].NodeID = strings.Repeat("9", 64)
			case "slot":
				altered.headers[0].Slot++
			case "contents":
				altered.contents[altered.headers[0].ContentID] = []byte("#ifndef __FORGED_GUARD\n#endif\n")
			case "content-id":
				altered.headers[0].ContentID = strings.Repeat("8", 64)
			}
			calls := 0
			plan.metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error { calls++; return nil })
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, &altered)
			if calls != 0 {
				t.Fatalf("%s admitted unauthenticated guard origin: %#v, calls %d, %v", failure, analysis, calls, err)
			}
			if failure == "contents" || failure == "content-id" {
				if err == nil || analysis != nil {
					t.Fatalf("%s did not reject contradictory authenticated bytes: %#v, %v", failure, analysis, err)
				}
				return
			}
			// An unrelated producer/slot may be ignored rather than becoming an
			// authority error, but it must leave this consumer opaque.
			if err == nil {
				if analysis == nil {
					t.Fatal("analysis returned neither a result nor an error")
				}
				sets, lookupErr := analysis.ByNodeID(plan)
				if lookupErr != nil || !sets[consumer].Opaque {
					t.Fatalf("%s provided an absent observed slot: %#v, %v", failure, sets, lookupErr)
				}
			}
		})
	}
}

func TestConfigDependencyCompilerGuardObservationRequiresObservedByteAuthority(t *testing.T) {
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	contents := "#ifndef __OBSERVED_GUARD\n#endif\n#if __has_attribute(deprecated)\n#endif\n"
	replay, observed := observedDependencyGateForTest(t, plan, producerID, contents)
	state, err := newConfigDependencyObservedHeaders(plan, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	projection := state.projections[configDependencyObservedOutputKey{producerID, 0}]
	var consumer ActionPlanNode
	for _, node := range plan.Nodes {
		if node.ID == consumerID {
			consumer = node
		}
	}
	for _, mode := range []string{"unobserved-exact", "forged-prefix", "numeric-summary", "config-projection", "changed-bytes"} {
		t.Run(mode, func(t *testing.T) {
			file := configDependencyScanFile{logical: "include/generated/selected.h", exactIdentity: projection.text.identity, exactContents: contents}
			switch mode {
			case "unobserved-exact":
				file.exactIdentity = "literal-producer:0"
			case "forged-prefix":
				file.exactIdentity = "observed-header:forged:00000000:" + projection.contentID
			case "numeric-summary":
				file.macroTable = true
			case "config-projection":
				file.configProjection = true
			case "changed-bytes":
				file.exactContents += "#ifndef __FORGED_GUARD\n#endif\n"
			}
			calls := 0
			plan.metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error { calls++; return nil })
			context := newConfigDependencyAnalysisContext(plan)
			context.observedHeaders = state
			scanner := configDependencyClosureScanner{collectCompilerGuards: true}
			scanner.rememberCompilerGuardFile(file)
			scanner.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(probe configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
				context.recordCompilerGuardHints(plan, consumer, "cc", probe, file, hints, contentID)
			})
			if calls != 0 || (context.compilerGuardError != nil) != (mode == "changed-bytes") {
				t.Fatalf("%s emitted unproved generated hints: calls %d, %v", mode, calls, context.compilerGuardError)
			}
		})
	}
}

func TestConfigDependencyCompilerGuardObservationTruncationIsAllOrNothing(t *testing.T) {
	var source strings.Builder
	source.WriteString("#include UNMODELED_HEADER\n")
	measured := map[string]bool{}
	for index := 0; index <= configDependencyCompilerGuardHintsMaximumNames; index++ {
		fmt.Fprintf(&source, "#if defined(__GUARD_%04d)\n#endif\n", index)
		measured[fmt.Sprintf("__GUARD_%04d", index)] = false
	}
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		"drivers/example/driver.c": source.String(),
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"})
	var got []ConfigDependencyCompilerGuardObservation
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error { got = append(got, value); return nil })
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || len(got) != 1 || !got[0].Truncated || len(got[0].Names) != 0 || !got[0].Origin.Source {
		t.Fatalf("name budget published partial inventory: %#v, %#v, %v", set, got, err)
	}
	// Even a caller which already measured every listed name must preserve
	// the all-or-nothing truncation event; filtering cannot complete it.
	measuredScanner := configDependencyClosureScanner{
		collectCompilerGuards: true, compilerGuardInitialDefinitions: measured,
		sourceLookup: newConfigDependencySourceLookup(plan.selectionGraph.profiles["config-dependency"]),
	}
	file, found := measuredScanner.sourceFile("drivers/example/driver.c")
	if !found {
		t.Fatal("truncated source fixture missing")
	}
	measuredScanner.rememberCompilerGuardFile(file)
	emitted := 0
	measuredScanner.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
		emitted++
		if !hints.truncated || len(hints.names) != 0 {
			t.Fatalf("measured names hid truncation: %#v", hints)
		}
	})
	if emitted != 1 {
		t.Fatalf("measured truncated inventory emitted %d events", emitted)
	}
	// The separate reached-file budget must discard all file hints too, not
	// publish a prefix of apparently complete per-file observations.
	scanner := configDependencyClosureScanner{collectCompilerGuards: true}
	for index := 0; index <= 4096; index++ {
		scanner.rememberCompilerGuardFile(configDependencyScanFile{source: true, logical: fmt.Sprintf("f%d.h", index), physical: fmt.Sprintf("/immutable-fixture/f%d.h", index)})
	}
	got = nil
	context := newConfigDependencyAnalysisContext(plan)
	scanner.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(probe configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
		context.recordCompilerGuardHints(plan, node, "cc", probe, file, hints, contentID)
	})
	if context.compilerGuardError != nil || len(got) != 1 || !got[0].Truncated || len(got[0].Names) != 0 || got[0].Origin != (ConfigDependencyCompilerGuardOrigin{}) {
		t.Fatalf("file budget published partial origins: %#v, %v", got, context.compilerGuardError)
	}
}

func TestConfigDependencyCompilerGuardObservationScratchReplayDoesNotLeak(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifndef __PARENT_GUARD\n#endif\n",
		"include/speculative.h":    "#ifndef __SPECULATIVE_GUARD\n#endif\n",
	})
	for _, saturated := range []bool{false, true} {
		t.Run(fmt.Sprintf("saturated_%t", saturated), func(t *testing.T) {
			scanner := fixture.scanner(nil)
			scanner.collectCompilerGuards = true
			parent, ok := scanner.sourceFile("drivers/example/driver.c")
			if !ok {
				t.Fatal("parent source missing")
			}
			scanner.rememberCompilerGuardFile(parent)
			if saturated {
				for index := 1; index < 4096; index++ {
					scanner.rememberCompilerGuardFile(configDependencyScanFile{source: true, physical: fmt.Sprintf("/immutable-fixture/%d.h", index)})
				}
			}
			before := maps.Clone(scanner.compilerGuardFiles)
			beforeConditional := maps.Clone(scanner.conditionalFiles)
			file, ok := scanner.sourceFile("include/speculative.h")
			if !ok {
				t.Fatal("speculative source missing")
			}
			entry := configDependencyHeaderCacheEntry{events: []configDependencyHeaderEvent{
				{kind: configDependencyHeaderEnter, file: file},
				{kind: configDependencyHeaderResolve, including: parent, include: configDependencyLiteralInclude{name: "missing.h"}},
			}}
			if _, hit := entry.replay(scanner, nil); hit {
				t.Fatal("invalid speculative suffix unexpectedly hit")
			}
			if !maps.Equal(before, scanner.compilerGuardFiles) || scanner.compilerGuardFilesTruncated ||
				!maps.Equal(beforeConditional, scanner.conditionalFiles) {
				t.Fatal("failed replay changed parent pending guard hints or truncation")
			}
			entry.events = entry.events[:1]
			committed, hit := entry.replay(scanner, nil)
			if !hit || committed.conditionalFiles[file.identity()] != file {
				t.Fatal("successful scratch replay lost the entered file")
			}
			// Header-enter replay restores conditionalFiles without reinterpreting
			// syntax. The production deferred emission hook folds that entered
			// set into its bounded guard inventory, including cache-hit files.
			got := map[string][]string{}
			truncated := 0
			committed.emitCompilerGuardHints(configDependencyCompilerPredefineProbe{language: "c"}, func(_ configDependencyCompilerPredefineProbe, observedFile configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
				if hints.truncated {
					truncated++
					if observedFile.identity() != "" || len(hints.names) != 0 {
						t.Fatal("scratch file budget emitted a partial authoritative origin")
					}
					return
				}
				got[observedFile.logical] = slices.Clone(hints.names)
			})
			if saturated {
				if !committed.compilerGuardFilesTruncated || truncated != 1 || len(got) != 0 {
					t.Fatalf("successful scratch replay lost all-or-nothing bound: %#v, truncated %d", got, truncated)
				}
			} else if committed.compilerGuardFilesTruncated || truncated != 0 || !reflect.DeepEqual(got, map[string][]string{
				"drivers/example/driver.c": {"__PARENT_GUARD"}, "include/speculative.h": {"__SPECULATIVE_GUARD"},
			}) {
				t.Fatalf("successful replay omitted entered file's guard hints: %#v, truncated %d", got, truncated)
			}
			if !maps.Equal(before, scanner.compilerGuardFiles) || scanner.compilerGuardFilesTruncated ||
				!maps.Equal(beforeConditional, scanner.conditionalFiles) {
				t.Fatal("scratch replay or emission mutated parent before commit")
			}
		})
	}
}
