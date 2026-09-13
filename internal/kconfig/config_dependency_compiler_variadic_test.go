package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompilerVariadicCommaSnapshotContextAndMerge(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	args := []string{"-nostdinc", "-std=gnu11"}
	makeAnswer := func(syntax, output string) *KbuildCompilerGuardAnswers {
		t.Helper()
		discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err = discovery.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", args, nil, syntax, nil); err != nil {
			t.Fatal(err)
		}
		plan, err := discovery.Plan()
		if err != nil {
			t.Fatal(err)
		}
		oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{compilerVariadicCommaStep: output})
		replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
		if err != nil {
			t.Fatal(err)
		}
		if _, state, _, err := replay.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", args, nil, syntax, nil); err != nil || state != OptionalCompilerVariadicCommaAnswered {
			t.Fatal(state, err)
		}
		answer, err := replay.Answers()
		if err != nil || len(answer.variadics) != 1 {
			t.Fatal("missing immutable grammar", err)
		}
		return answer
	}
	standard, named := makeAnswer("standard", "17 23"), makeAnswer("named", "17,23")
	merged, err := MergeKbuildCompilerGuardAnswers(standard, named, standard)
	if err != nil || len(merged.variadics) != 2 {
		t.Fatal("lost independent syntax", err)
	}
	permuted, err := MergeKbuildCompilerGuardAnswers(named, standard)
	if err != nil || !maps.Equal(merged.variadics, permuted.variadics) {
		t.Fatal("merge order changed identities", err)
	}
	plan := &ActionPlan{metadata: &CompactMetadata{compilerGuardAnswers: merged}, Toolsets: maps.Clone(merged.toolsets)}
	context := configDependencyCompilerPredefineKey("target", "cc", "c", args, nil, nil)
	for _, syntax := range []string{"standard", "named"} {
		answer, ready, err := configDependencySupplementalCompilerVariadicComma(plan, compilerVariadicCommaKey{context, syntax})
		if err != nil || !ready || answer.identity == "" || answer.deleteComma != (syntax == "standard") {
			t.Fatal("wrong measured answer", err)
		}
	}
	for _, key := range []compilerVariadicCommaKey{
		{context, "unmeasured"},
		{configDependencyCompilerPredefineKey("target", "cc", "c", []string{"-nostdinc", "-std=c11"}, nil, nil), "standard"},
		{configDependencyCompilerPredefineKey("target", "cc", "c", args, nil, map[string]string{"MODE": "different"}), "standard"},
	} {
		if _, ready, err := configDependencySupplementalCompilerVariadicComma(plan, key); err != nil || ready {
			t.Fatal("foreign context borrowed grammar", err)
		}
	}
	plan.Toolsets["target"] = "different"
	if _, ready, err := configDependencySupplementalCompilerVariadicComma(plan, compilerVariadicCommaKey{context, "standard"}); err == nil || ready {
		t.Fatal("foreign compiler borrowed grammar")
	}
	if _, err := MergeKbuildCompilerGuardAnswers(standard, makeAnswer("standard", "17,23")); err == nil {
		t.Fatal("contradictory grammar merged")
	}
}

func TestConfigDependencyVariadicCommaHintIsSyntaxOnly(t *testing.T) {
	for _, test := range []struct{ line, want string }{
		{"#define F(...) 17, ##__VA_ARGS__", "standard"},
		{"# define F(tail...) 17, ##tail", "named"},
		{"%:define F(tail...) 17 %:%: tail", ""},
		{"#define F(__VA_ARGS__...) 17, ##__VA_ARGS__", "named"},
		{"#define F(x,...) 17, ##__VA_ARGS__", ""},
		{"#define F(...) \"comma , ##__VA_ARGS__\"", ""},
		{"/* #define F(...) 17, ##__VA_ARGS__ */", ""},
		{"#define F(...) __VA_ARGS__", ""},
	} {
		if got := configDependencyVariadicCommaHint(test.line); got != test.want {
			t.Fatalf("%q = %q, want %q", test.line, got, test.want)
		}
	}
	hints := configDependencyCompilerGuardObservationHintsForContents([]byte("#define F(tail...) 17, ##tail\n"))
	if hints.variadicMentions != [2]bool{false, true} || len(hints.names) != 0 || len(hints.calls) != 0 {
		t.Fatal("grammar hint invented namespace facts")
	}
}

func TestCompilerVariadicLiteralLookaheadDoesNotProveSource(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname:              "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#include <first.h>\n#include <later.h>\n",
		"include/first.h":     "__FIRST_HEADER\n",
		"include/later.h":     "#define CLOBBERS(tail...) \"memory\", ##tail\nCLOBBERS()\n",
		"include/unrelated.h": "#define UNUSED(...) 17, ##__VA_ARGS__\n",
	}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", pathname})
	baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !baseline.Opaque || !strings.Contains(baseline.Reason, "__FIRST_HEADER") {
		t.Fatalf("fixture did not stop before the grammar header: %#v, %v", baseline, err)
	}
	context := newConfigDependencyAnalysisContext(plan)
	for range 2 {
		hints := 0
		plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
			if value.VariadicCommaSyntax == "" {
				return nil
			}
			if value.VariadicCommaSyntax != "named" || !value.OptionalVariadicHints ||
				!value.Origin.Source || value.Origin.LogicalPath != "include/later.h" || len(value.Origin.ContentID) != 64 ||
				len(value.Names)+len(value.Calls) != 0 || value.OptionalTokenHints || !value.LiteralIncludeHints ||
				value.OptionalDefinedness || value.CounterCount != 0 || value.Truncated {
				t.Fatalf("lookahead grammar lost its bounded hint-only origin: %#v", value)
			}
			hints++
			return nil
		})
		got, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil || !reflect.DeepEqual(got, baseline) {
			t.Fatalf("grammar hint changed the current source proof: %#v != %#v, %v", got, baseline, err)
		}
		if hints != 1 {
			t.Fatalf("got %d grammar hints before reaching the header, want one", hints)
		}
	}
	plan.metadata.SetCompilerGuardObserver(nil)
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__FIRST_HEADER"}, false))
	got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !got.Opaque || !strings.Contains(got.Reason, "variadic comma deletion") || len(got.Symbols) != 0 {
		t.Fatalf("unmeasured lookahead grammar granted a source proof: %#v, %v", got, err)
	}
}

func TestCompilerVariadicLookaheadPromotionPreservesStrength(t *testing.T) {
	for _, order := range [][]bool{{true, true, false, false, true}, {false, true, false}} {
		plan, node := compilerGuardObservationPlanForTest(t, map[string]string{"drivers/example/driver.c": ""}, nil)
		s := &configDependencyClosureScanner{collectCompilerGuards: true}
		var literal []bool
		s.configureCompilerVariadicQueries(plan, node, "cc", configDependencyCompilerPredefineProbe{language: "c"},
			func(_ configDependencyCompilerPredefineProbe, _ configDependencyScanFile, hints configDependencyCompilerGuardHints, _ string) {
				if !hints.optionalVariadicHints || hints.variadicSyntax != "named" {
					t.Fatal("hint changed grammar demand kind")
				}
				literal = append(literal, hints.literalIncludeHints)
			})
		for _, candidate := range order {
			s.compilerVariadicHint(configDependencyScanFile{}, "immutable-content", "named", candidate)
		}
		want := []bool{false}
		if order[0] {
			want = []bool{true, false}
		}
		if !slices.Equal(literal, want) {
			t.Fatalf("order %v emitted strengths %v, want %v", order, literal, want)
		}
	}
}

func TestConfigDependencyVariadicGrammarCannotBorrowDefinednessOnlyCache(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#if CONFIG_USED\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	plan, node := configDependencyCompletedCachePlanForTest(t, root, "base", "compile-base", "src-00000001", "image", "target-toolset",
		map[string]string{"CONFIG_USED": "y", "CONFIG_OTHER": "n"})
	plan.attachFamilyPlanningCache(cache)
	context := newConfigDependencyAnalysisContextWithCache(plan, cache.configDependencyCache())
	recipe := plan.Recipes[node.Recipe]
	if _, ok := configDependencyCompletedCompilerCandidateForNode(plan, node, recipe, context, cache.configDependencyCache()); !ok {
		t.Fatal("fixture did not establish eligibility for the definedness-only cache")
	}
	for _, deleted := range []bool{false, true} {
		// This cache-eligibility unit test supplies no source authority. Neither
		// a positive nor a negative grammar value may use the old cache key.
		plan.metadata.compilerGuardAnswers = &KbuildCompilerGuardAnswers{variadics: map[compilerVariadicCommaKey]compilerVariadicCommaAnswer{
			{context: "exact-context", syntax: "standard"}: {deleteComma: deleted, identity: "grammar-witness"},
		}}
		if _, ok := configDependencyCompletedCompilerCandidateForNode(plan, node, recipe, context, cache.configDependencyCache()); ok {
			t.Fatal("variadic answer borrowed an older definedness-only completed proof")
		}
	}
}

func TestConfigDependencyVariadicCommaRequiresMeasuredSyntax(t *testing.T) {
	for _, definition := range []string{"F(...) CONFIG_READ , ##__VA_ARGS__", "F(tail...) CONFIG_READ , ##tail"} {
		catalog, err := configDependencyMacroCallFixtureCatalogForTest(definition, "CONFIG_READ 7")
		if err != nil {
			t.Fatal(err)
		}
		for _, deleted := range []bool{false, true} {
			mode := configDependencyMacroCallMode{variadicComma: func(syntax string) (bool, string, string) {
				want := "standard"
				if strings.Contains(definition, "tail...") {
					want = "named"
				}
				if syntax != want {
					t.Fatal("lost source variadic syntax")
				}
				return deleted, "measured-context", ""
			}}
			got, reason := configDependencyMacroCallExpand("F()", configDependencyMacroCallFixtureResolver(catalog), mode)
			want := []string{"7"}
			if !deleted {
				want = append(want, ",")
			}
			if reason != "" || !slices.Equal(got.Tokens, want) || !slices.Equal(got.ConfigReads, []string{"CONFIG_READ"}) || len(got.VariadicReads) != 1 || got.VariadicReads[0].DeleteComma != deleted {
				t.Fatalf("expansion: %#v %s", got, reason)
			}
			bad, reason := configDependencyMacroCallExpand("F() _Pragma(P)", configDependencyMacroCallFixtureResolver(catalog), mode)
			if reason == "" || !reflect.DeepEqual(bad, configDependencyMacroCallResult{}) {
				t.Fatal("failed tail published grammar/CONFIG prefix")
			}
		}
		for _, mode := range []configDependencyMacroCallMode{{}, {variadicComma: func(string) (bool, string, string) { return true, "", "" }}, {variadicComma: func(string) (bool, string, string) { return false, "", "missing" }}} {
			got, reason := configDependencyMacroCallExpand("F()", configDependencyMacroCallFixtureResolver(catalog), mode)
			if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
				t.Fatal("unmeasured grammar granted a proof")
			}
		}
	}
}

func TestCompilerVariadicObserverDistinguishesExpansionFromDefinitionHints(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, test := range []struct {
		name, body string
		demand     bool
	}{
		{"reached", "F()\n", true},
		{"unused", "", false},
		{"inactive", "#if 0\nF()\n#endif\n", false},
		{"discarded", "#define DROP(x)\nDROP(F())\n", false},
		{"stringified", "#define RAW(x) #x\nRAW(F())\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				pathname: "#define PICK(x) CONFIG_ ## x\n#define F(tail...) 17, ##tail\nPICK(BASE)\n" + test.body,
			}, []string{"-nostdinc", "-c", pathname}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			context := newConfigDependencyAnalysisContext(plan)
			for range 2 {
				hints, demands := 0, 0
				plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
					if value.VariadicCommaSyntax == "" {
						return nil
					}
					if value.VariadicCommaSyntax != "named" || !value.Origin.Source || value.Origin.LogicalPath != pathname ||
						len(value.Origin.ContentID) != 64 || value.Truncated || len(value.Names)+len(value.Calls) != 0 ||
						value.CounterCount != 0 || value.OptionalCounterHints || value.OptionalDefinedness || value.OptionalTokenHints || value.LiteralIncludeHints {
						t.Fatalf("grammar observation lost its exact kind or source origin: %#v", value)
					}
					if value.OptionalVariadicHints {
						hints++
					} else {
						demands++
					}
					return nil
				})
				result, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil || hints == 0 || (demands > 0) != test.demand {
					t.Fatalf("grammar demand/hint boundary: hints=%d demands=%d result=%#v error=%v", hints, demands, result, err)
				}
				if test.demand && (!result.Opaque || len(result.Symbols)+len(result.SourcePaths)+len(result.ObjectPaths) != 0) {
					t.Fatal("missing grammar measurement published a source proof")
				}
			}
		})
	}
}
