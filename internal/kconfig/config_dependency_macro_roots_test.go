package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyReachableStringificationSchedulesOnlyRefinement(t *testing.T) {
	for _, test := range []struct {
		definition string
		want       bool
	}{
		{"RAW(x) #x", true},
		{"RAW(x) %:x", true},
		{"RAW(...) #__VA_ARGS__", true},
		{"RAW(args...) #args", true},
		{"RAW(x) \"#x %:x\"", false},
		{"RAW(x) /* #x */ x", false},
		{"RAW(x) '#'", false},
		{"RAW(x) safe_ ## x", false},
		{"RAW(x) safe_ %:%: x", false},
	} {
		t.Run(test.definition, func(t *testing.T) {
			definition, _, ok := configDependencySourceMacroExpansionDefinition(test.definition)
			if !ok {
				t.Fatal("invalid fixture definition")
			}
			graph := configDependencyMacroExpansionGraph{}
			graph.addDefinitions(definition, configDependencyMacroExpansionDefinition{name: "WRAP", identifiers: []string{"RAW"}})
			if graph.reachableStringification() != "" {
				t.Fatal("unreachable definition scheduled refinement")
			}
			graph.addRoots("WRAP")
			cloned := graph.clone()
			if (cloned.reachableStringification() != "") != test.want || cloned.reachableTokenPaste() != "" {
				t.Fatalf("stringification confused with paste hazard: %#v", cloned)
			}
		})
	}
}

func TestConfigDependencyEarlyMacroHazardStopsBeforeLaterIncludes(t *testing.T) {
	for name, definition := range map[string]string{
		"token_paste": "#define HAZARD(a, b) a ## b\n",
		"pragma":      "#define HAZARD(a, b) _Pragma(#a)\n",
		"ms_pragma":   "#define HAZARD(a, b) __pragma(a)\n",
	} {
		for _, crossHeader := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/local", true: "/cross_header"}[crossHeader], func(t *testing.T) {
				source := definition
				if crossHeader {
					source = "#include <hazard.h>\n"
				}
				// The existing whole-file summary already sees this invocation.
				// A later unavailable header must not consume more interpretation
				// work (or replace the known hazard with an include diagnostic).
				source += "HAZARD(CONFIG_, DRIVER)\n#include <unavailable.h>\n"
				plan, node := configDependencyCompilePlanForTest(t, map[string]string{
					"drivers/example/driver.c": source,
					"include/hazard.h":         definition,
				}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c"}, nil)
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return "", true, nil
				}
				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				if err != nil {
					t.Fatal(err)
				}
				want := "token pasting"
				if name == "pragma" {
					want = "_Pragma"
				} else if name == "ms_pragma" {
					want = "__pragma"
				}
				if !set.Opaque || !strings.Contains(set.Reason, want) {
					t.Fatalf("known hazard did not stop interpretation: %#v", set)
				}
			})
		}
	}
}

func TestConfigDependencyEarlyMacroSummarySkipsDefinedGuard(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\nCONFIG_PRECISE\n",
		"include/common.h":         "#ifndef ALREADY_INCLUDED\n#define ALREADY_INCLUDED\n#define HAZARD(a, b) a ## b\nHAZARD(CONFIG_, DRIVER)\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("ALREADY_INCLUDED", configDependencyMacroDefined)
		})
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_PRECISE"}) {
			t.Fatalf("skipped guarded body lost precise reuse: %#v", result.set)
		}
	}
}

func TestConfigDependencyEarlyMacroReplayIsTransactional(t *testing.T) {
	scanner := configDependencyClosureScanner{}
	scanner.macroExpansions.addDefinitions(configDependencyMacroExpansionDefinition{
		name: "ALIAS", identifiers: []string{"SAFE"}, safeTokenPastePrefixes: []string{"safe_"},
	})
	scanner.macroExpansions.addRoots("ALIAS")
	scratch := scanner.headerReplayScanner()
	file := configDependencyScanFile{logical: "hazard.h", physical: "/hazard.h", source: true}
	scratch.checkEarlyMacroSummary(file, configDependencyParsedFile{
		macroDefinitions: []configDependencyMacroExpansionDefinition{
			{name: "ALIAS", identifiers: []string{"HAZARD"}, safeTokenPastePrefixes: []string{"unsafe_"}},
			{name: "HAZARD", tokenPaste: true},
		},
		macroRoots: []string{"ADDED_ROOT"},
	})
	if scratch.opaqueReason == "" {
		t.Fatal("speculative hazard was not rejected")
	}
	scanner.checkMacroExpansionHazards()
	alias := scanner.macroExpansions.definitions["ALIAS"]
	if scanner.opaqueReason != "" || len(scanner.earlyMacroSummaries) != 0 ||
		scanner.macroExpansions.roots["ADDED_ROOT"] || alias.identifiers["HAZARD"] || alias.safeTokenPastePrefixes["unsafe_"] {
		t.Fatal("failed speculative replay leaked macro expansion state")
	}
}

func TestConfigDependencyEarlyMacroHazardRechecksCachedHeader(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/safe.c":   "#include <common.h>\nCONFIG_PRECISE\n",
		"drivers/example/unsafe.c": "#include <common.h>\nHAZARD(CONFIG_, DRIVER)\n#include <unavailable.h>\n",
		"include/common.h":         "#ifndef COMMON_H\n#define COMMON_H\n#define HAZARD(a, b) a ## b\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	safe := fixture.compare(t, cache, "drivers/example/safe.c", nil)
	if safe.set.Opaque || cache.stores == 0 {
		t.Fatalf("safe definition did not produce a reusable header: %#v, stores=%d", safe.set, cache.stores)
	}
	unsafe := fixture.compare(t, cache, "drivers/example/unsafe.c", nil)
	if !unsafe.set.Opaque || !strings.Contains(unsafe.set.Reason, "token pasting") {
		t.Fatalf("cached definition bypassed early reachability check: %#v", unsafe.set)
	}
	before := cache.hits
	safe = fixture.compare(t, cache, "drivers/example/safe.c", nil)
	if safe.set.Opaque || cache.hits <= before || !slices.Equal(safe.set.Symbols, []string{"CONFIG_PRECISE"}) {
		t.Fatalf("rejected speculative replay poisoned a safe cache entry: %#v, hits=%d/%d", safe.set, before, cache.hits)
	}
}

func TestConfigDependencyMacroRootsExcludeValidatedNonexpandingOperands(t *testing.T) {
	for _, directive := range []string{
		"#ifdef HAZARD\n#endif\n",
		"#ifndef HAZARD\n#endif\n",
		"#undef HAZARD\n",
		"%:ifndef HAZARD /* comment */\n%:endif\n",
		"#if defined(HAZARD)\n#endif\n",
		"#if !defined HAZARD && (1 || defined(CONFIG_OTHER))\n#endif\n",
		"#if 0\n#elif defined(HAZARD)\n#endif\n",
		"#if defined(HAZARD) && \\\n !defined(CONFIG_OTHER)\n#endif\n",
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(directive), "\n", ";"), func(t *testing.T) {
			contents := []byte("#define HAZARD(x) _Pragma(#x)\n" + directive)
			definitions, roots := configDependencySourceMacroExpansions(contents)
			if len(definitions) != 1 || definitions[0].name != "HAZARD" {
				t.Fatalf("macro definition was changed: %#v", definitions)
			}
			if slices.Contains(roots, "HAZARD") || slices.Contains(roots, "CONFIG_OTHER") {
				t.Fatalf("non-expanding directive operands became expansion roots: %q", roots)
			}
			graph := configDependencyMacroExpansionGraph{}
			graph.addDefinitions(definitions...)
			graph.addRoots(roots...)
			if graph.reachableIdentifier("_Pragma") {
				t.Fatal("definedness-only/undef operand made an unused replacement reachable")
			}
		})
	}
}

func TestConfigDependencyMacroRootsSeparateConditionalKeywordsFromOperands(t *testing.T) {
	for name, directive := range map[string]string{
		"if":            "#if SELECT(CONFIG_CHOICE) > 0\n#endif\n",
		"elif":          "#if 0\n#elif SELECT(CONFIG_CHOICE) > 0\n#endif\n",
		"digraph":       "%:if SELECT(CONFIG_CHOICE) > 0\n%:endif\n",
		"spacing":       "  #  if /* comment */ SELECT(CONFIG_CHOICE) > 0\n#endif\n",
		"line_splice":   "#i\\\nf SELECT(CONFIG_CHOICE) > 0\n#endif\n",
		"unsupported":   "#if SELECT(CONFIG_CHOICE) +\n#endif\n",
		"short_circuit": "#if 0 && SELECT(CONFIG_CHOICE)\n#endif\n",
	} {
		t.Run(name, func(t *testing.T) {
			definitions, roots := configDependencySourceMacroExpansions([]byte(
				"#define if(x) _Pragma(#x)\n#define elif(x) x ## _hidden\n" + directive))
			for _, keyword := range []string{"if", "elif"} {
				if slices.Contains(roots, keyword) {
					t.Fatalf("directive keyword %q became a macro expansion root: %q", keyword, roots)
				}
			}
			for _, operand := range []string{"SELECT", "CONFIG_CHOICE"} {
				if !slices.Contains(roots, operand) {
					t.Fatalf("unproven conditional lost operand %q: %q", operand, roots)
				}
			}
			graph := configDependencyMacroExpansionGraph{}
			graph.addDefinitions(definitions...)
			graph.addRoots(roots...)
			if graph.reachableIdentifier("_Pragma") || graph.reachableTokenPaste() != "" {
				t.Fatal("directive syntax made an unused macro replacement reachable")
			}
		})
	}
	for _, source := range []string{
		"#if if(CONFIG_CHOICE)\n#endif\n",
		"#if 0\n#elif elif(CONFIG_CHOICE)\n#endif\n",
		"if (CONFIG_CHOICE) {}\n",
		"#if 0 && if(CONFIG_CHOICE)\n#endif\n",
	} {
		definitions, roots := configDependencySourceMacroExpansions([]byte(
			"#define if(x) _Pragma(#x)\n#define elif(x) _Pragma(#x)\n" + source))
		graph := configDependencyMacroExpansionGraph{}
		graph.addDefinitions(definitions...)
		graph.addRoots(roots...)
		if !graph.reachableIdentifier("_Pragma") {
			t.Fatalf("actual keyword-named macro invocation lost its effect: %q", source)
		}
	}
}

func TestConfigDependencyConditionalKeywordDoesNotPoisonCachedHeader(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/safe.c":   "#include <common.h>\nCONFIG_PRECISE\n",
		"drivers/example/unsafe.c": "#include <common.h>\nif(CONFIG_CHOICE)\n",
		"include/common.h": "#ifndef COMMON_H\n#define COMMON_H\n" +
			"#define if(x) x ## _hidden\n#if UNKNOWN > 0\nint chosen;\n#endif\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/safe.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_PRECISE"}) {
			t.Fatalf("directive-only header lost precise reuse: %#v", result.set)
		}
		if iteration > 0 && cache.hits == before {
			t.Fatal("safe repeat did not exercise header cache replay")
		}
		unsafe := fixture.compare(t, cache, "drivers/example/unsafe.c", nil)
		if !unsafe.set.Opaque || !strings.Contains(unsafe.set.Reason, "token pasting") {
			t.Fatalf("actual invocation lost its conservative rejection: %#v", unsafe.set)
		}
	}
}

func TestConfigDependencyMacroEffectsFollowPastedNames(t *testing.T) {
	for _, test := range []struct {
		name, definitions, want string
		reachable               bool
	}{
		{"direct builtin", "#define ROOT(x) _Prag ## ma(x)\n", "_Pragma", true},
		{"direct Microsoft builtin", "#define ROOT(x) __prag ## ma(x)\n", "__pragma", true},
		{"alias", "#define ROOT(x) safe_ ## x\n#define safe_effect(x) _Pragma(x)\n", "_Pragma", true},
		{"nested prefixes", "#define ROOT(x) first_ ## x\n#define first_next(x) second_ ## x\n#define second_effect(x) __pragma(x)\n", "__pragma", true},
		{"prefix cycle", "#define ROOT(x) node_ ## x\n#define node_cycle(x) ROOT(x)\n", "_Pragma", false},
		{"prefix cycle with exit", "#define ROOT(x) node_ ## x\n#define node_cycle(x) ROOT(x)\n#define node_exit(x) _Prag ## ma(x)\n", "_Pragma", true},
		{"ordinary alias cycle with exit", "#define ROOT(x) cycle(x)\n#define cycle(x) ROOT(x) effect(x)\n#define effect(x) __prag ## ma(x)\n", "__pragma", true},
		{"unrelated safe prefix", "#define ROOT(x) safe_ ## x\n#define unsafe_effect(x) _Pragma(x)\n", "_Pragma", false},
		{"builtin is shorter than prefix", "#define ROOT(x) _Pragma_extra_ ## x\n", "_Pragma", false},
		{"unused paste definition", "#define ROOT(x) x\n#define UNUSED(x) _Prag ## ma(x)\n", "_Pragma", false},
		{"quoted builtin", "#define ROOT(x) safe_ ## x\n#define safe_text \"_Pragma\"\n", "_Pragma", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			definitions, roots := configDependencySourceMacroExpansions([]byte(test.definitions + "ROOT(value)\n"))
			graph := configDependencyMacroExpansionGraph{}
			graph.addDefinitions(definitions...)
			graph.addRoots(roots...)
			for iteration := 0; iteration < 2; iteration++ {
				if got := graph.reachableIdentifier(test.want); got != test.reachable {
					t.Fatalf("reachableIdentifier(%q) = %t, want %t", test.want, got, test.reachable)
				}
			}
			if got := graph.reachableTokenPaste(); got != "" {
				t.Fatalf("fixture should use only CONFIG-safe prefixes, got %q", got)
			}
		})
	}
}

func TestConfigDependencyPastedMacroEffectCannotHideRestoredGuard(t *testing.T) {
	for name, definitions := range map[string]string{
		"direct builtin": "#define EFFECT(x) _Prag ## ma(#x)\n",
		"rescan alias":   "#define SELECT(suffix) safe_ ## suffix\n#define safe_effect(x) _Pragma(#x)\n#define EFFECT(x) SELECT(effect)(x)\n",
	} {
		t.Run(name, func(t *testing.T) {
			// Both paste stages matter: the pragma operator is constructed, and
			// its stringified operand is assembled without a literal macro-stack
			// spelling that could trigger the independent syntax rejection.
			contents := definitions + "#define APPLY(x) EFFECT(x)\n#define PUSH(x) push_ ## x\n#define POP(x) pop_ ## x\n#define SELECTED 1\nAPPLY(PUSH(macro)(\"SELECTED\"))\n#undef SELECTED\nAPPLY(POP(macro)(\"SELECTED\"))\n#if defined(SELECTED)\n#include <chosen.h>\n#endif\n"
			if strings.Contains(contents, "push_macro") || strings.Contains(contents, "pop_macro") {
				t.Fatal("fixture accidentally uses the independent macro-stack spelling check")
			}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": contents,
				"include/chosen.h":         "CONFIG_HIDDEN\n",
			}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c"}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) { return "", true, nil }
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "_Pragma") {
				t.Fatalf("pasted pragma lost the restored guard and reachable header: %#v", set)
			}
		})
	}
}

func TestConfigDependencyPastedMacroEffectRespectsSkippedGuard(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\nCONFIG_PRECISE\n",
		"include/common.h":         "#ifndef ALREADY_INCLUDED\n#define ALREADY_INCLUDED\n#define EFFECT(x) _Prag ## ma(x)\nEFFECT(\"effect\")\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("ALREADY_INCLUDED", configDependencyMacroDefined)
		})
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_PRECISE"}) {
			t.Fatalf("skipped guarded paste lost precision: %#v", result.set)
		}
	}
}

func TestConfigDependencyPastedMacroEffectRechecksCachedHeader(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/safe.c":   "#include <common.h>\nCONFIG_PRECISE\n",
		"drivers/example/unsafe.c": "#include <common.h>\nSELECT(effect)(\"effect\")\n#include <unavailable.h>\n",
		"include/common.h":         "#ifndef COMMON_H\n#define COMMON_H\n#define SELECT(x) safe_ ## x\n#define safe_effect(x) _Pragma(x)\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	safe := fixture.compare(t, cache, "drivers/example/safe.c", nil)
	if safe.set.Opaque || cache.stores == 0 {
		t.Fatalf("unused pasted effect prevented caching: %#v, stores=%d", safe.set, cache.stores)
	}
	unsafe := fixture.compare(t, cache, "drivers/example/unsafe.c", nil)
	if !unsafe.set.Opaque || !strings.Contains(unsafe.set.Reason, "_Pragma") {
		t.Fatalf("cached prefix-to-effect edge was lost: %#v", unsafe.set)
	}
	before := cache.hits
	safe = fixture.compare(t, cache, "drivers/example/safe.c", nil)
	if safe.set.Opaque || cache.hits <= before || !slices.Equal(safe.set.Symbols, []string{"CONFIG_PRECISE"}) {
		t.Fatalf("unsafe speculative replay poisoned safe cache state: %#v, hits=%d/%d", safe.set, before, cache.hits)
	}
}

func TestConfigDependencyPastedMacroEffectReplayIsTransactional(t *testing.T) {
	scanner := configDependencyClosureScanner{}
	scanner.macroExpansions.addDefinitions(configDependencyMacroExpansionDefinition{
		name: "ROOT", safeTokenPastePrefixes: []string{"safe_"},
	})
	scanner.macroExpansions.addRoots("ROOT")
	scratch := scanner.headerReplayScanner()
	scratch.checkEarlyMacroSummary(configDependencyScanFile{logical: "effect.h", physical: "/effect.h", source: true}, configDependencyParsedFile{
		macroDefinitions: []configDependencyMacroExpansionDefinition{
			{name: "safe_effect", safeTokenPastePrefixes: []string{"_Prag"}},
		},
	})
	if !strings.Contains(scratch.opaqueReason, "_Pragma") {
		t.Fatalf("speculative pasted effect was not rejected: %q", scratch.opaqueReason)
	}
	scanner.checkMacroExpansionHazards()
	if scanner.opaqueReason != "" || len(scanner.earlyMacroSummaries) != 0 || len(scanner.macroExpansions.definitions) != 1 {
		t.Fatal("failed speculative replay mutated the parent macro graph")
	}
}

func TestConfigDependencyMacroRootsKeepActualUsesAndUnprovenSyntax(t *testing.T) {
	for name, source := range map[string]string{
		"body_invocation":      "HAZARD(once)\n",
		"body_identifier":      "HAZARD;\n",
		"condition_invocation": "#if HAZARD(once)\n#endif\n",
		"guarded_body":         "#ifdef CONFIG_OTHER\nHAZARD(once)\n#endif\n",
		"malformed_ifdef":      "#ifdef HAZARD extra\n#endif\n",
		"malformed_ifndef":     "#ifndef (HAZARD)\n#endif\n",
		"malformed_undef":      "#undef HAZARD extra\n",
		"unknown_directive":    "#unknown HAZARD\n",
		"unknown_keyword_case": "#IFDEF HAZARD\n",
		"unknown_condition":    "#if defined(HAZARD) && UNKNOWN(1)\n#endif\n",
		"numeric_comparison":   "#if defined(HAZARD) == 1\n#endif\n",
		"malformed_defined":    "#if defined(HAZARD, OTHER)\n#endif\n",
		"trailing_tokens":      "#if defined(HAZARD) 0\n#endif\n",
		"nondefined_keyword":   "#if definedness(HAZARD)\n#endif\n",
		"deep_condition":       "#if " + strings.Repeat("(", configDependencyConditionalMaximumDepth+1) + "defined(HAZARD)" + strings.Repeat(")", configDependencyConditionalMaximumDepth+1) + "\n#endif\n",
		"overlong_condition":   "#if " + strings.Repeat("defined(HAZARD) || ", configDependencyConditionalMaximumBytes/10) + "1\n#endif\n",
	} {
		t.Run(name, func(t *testing.T) {
			definitions, roots := configDependencySourceMacroExpansions([]byte("#define HAZARD(x) _Pragma(#x)\n" + source))
			graph := configDependencyMacroExpansionGraph{}
			graph.addDefinitions(definitions...)
			graph.addRoots(roots...)
			if !slices.Contains(roots, "HAZARD") || !graph.reachableIdentifier("_Pragma") {
				t.Fatalf("actual use or unproven syntax lost its conservative expansion root: %q", roots)
			}
		})
	}
}

func TestConfigDependencyMacroRootsPreserveReadsWritesAndConfigEvidence(t *testing.T) {
	state, reason := parseConfigDependencyCompilerPredefines("#define HAZARD(x) _Pragma(#x)\n#define CONFIG_DRIVER 1\n")
	if reason != "" || !state.beginForcedHeaderTrace() {
		t.Fatalf("initialize traced macro state: %s", reason)
	}
	const contents = `#define HAZARD(x) _Pragma(#x)
#ifdef HAZARD
#define SEEN 1
#endif
#if defined(CONFIG_DRIVER) && !defined(MISSING)
#define CONFIG_SEEN 1
#endif
#undef HAZARD
`
	if _, reason := configDependencyLiteralIncludesWithMacroEffects([]byte(contents), &state,
		func(configDependencyLiteralInclude, configDependencyMacroDefinition) (bool, string) { return true, "" }); reason != "" {
		t.Fatalf("non-expanding directive operands rejected by ordered interpreter: %s", reason)
	}
	for _, name := range []string{"HAZARD", "CONFIG_DRIVER", "MISSING"} {
		if !state.forcedHeaderTrace.reads.contains(name) {
			t.Errorf("definedness read %s was lost from specialization requirements", name)
		}
	}
	if !state.forcedHeaderTouches.contains("HAZARD") || state.definition("HAZARD") != configDependencyMacroUndefined || state.definition("SEEN") != configDependencyMacroDefined {
		t.Fatal("root filtering changed executed macro writes or undef effects")
	}
	for _, name := range []string{"CONFIG_DRIVER", "CONFIG_SEEN"} {
		if !slices.Contains(ExtractConfigDependencySymbols([]byte(contents)), name) {
			t.Errorf("config dependency %s was lost", name)
		}
	}
}

func TestConfigDependencyMacroRootsCrossHeaderDefinednessNotInvocation(t *testing.T) {
	for _, actualUse := range []bool{false, true} {
		name := "definedness_only"
		body := "#ifdef CONFIG_DRIVER\nint enabled;\n#endif\n"
		if actualUse {
			name = "actual_pragma_invocation"
			body += "APPLY_PRAGMA(once)\n"
		}
		t.Run(name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c":          body,
				"include/linux/pragma-definition.h": "#define APPLY_PRAGMA(x) _Pragma(#x)\n",
				"include/linux/pragma-guard.h":      "#ifndef APPLY_PRAGMA\n#define APPLY_PRAGMA(x)\n#endif\n",
			}, []string{
				"-nostdinc", "-include", "${tree:kernel}/include/linux/pragma-definition.h",
				"-include", "${tree:kernel}/include/linux/pragma-guard.h", "-c", "drivers/example/driver.c",
			}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if actualUse {
				if !set.Opaque || !strings.Contains(set.Reason, "_Pragma") {
					t.Fatalf("actual cross-header _Pragma invocation lost its rejection: %#v", set)
				}
			} else if set.Opaque || !slices.Contains(set.Symbols, "CONFIG_DRIVER") || !slices.Equal(set.SourcePaths, []string{
				"drivers/example/driver.c", "include/linux/pragma-definition.h", "include/linux/pragma-guard.h",
			}) {
				t.Fatalf("cross-header definedness guard acquired effects or lost evidence: %#v", set)
			}
		})
	}
}
