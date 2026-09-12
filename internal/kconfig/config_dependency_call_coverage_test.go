package kconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyCallCoverageStringificationReadsThroughProductionScanner(t *testing.T) {
	for _, paste := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("paste-%t/enabled-%t", paste, enabled), func(t *testing.T) {
				prefix := ""
				if paste {
					prefix = "#define PICK(x) CONFIG_ ## x\nint pasted = PICK(VALUE);\n"
				}
				plan, node := configDependencyCompilePlanForTest(t, map[string]string{
					"drivers/example/driver.c": "#include <strings.h>\n" + prefix + `
const char raw[] = RAW(CONFIG_RAW);
const char expanded[] = EXPAND(CONFIG_VALUE);
#ifdef CONFIG_IFDEF
int ifdef_on;
#endif
#ifndef CONFIG_IFNDEF
int ifndef_off;
#endif
#if defined(CONFIG_DEFINED)
int defined_on;
#endif
#if 0
#ifdef CONFIG_DEAD
int unreachable = CONFIG_DEAD_VALUE;
#endif
#elifdef CONFIG_ELIFDEF
int elifdef_on;
#elifndef CONFIG_ELIFNDEF
int elifndef_off;
#endif
`,
					"include/strings.h": "#define RAW(args...) #args\n#define EXPAND(args...) RAW(args)\n",
				}, []string{"-nostdinc", "-I${tree:kernel}/include", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, map[string]string{
					"CONFIG_VALUE": "7", "CONFIG_RAW": "y", "CONFIG_IFDEF": "y", "CONFIG_DEFINED": "y",
				})
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				if enabled {
					plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
						return "", true, nil
					}
				}
				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				if err != nil {
					t.Fatal(err)
				}
				if !enabled {
					if paste {
						if !set.Opaque || len(set.Symbols) != 0 {
							t.Fatalf("unprobed paste escaped: %#v", set)
						}
					} else if set.Opaque || !slices.Contains(set.Symbols, "CONFIG_RAW") {
						t.Fatalf("unprobed lexical fallback was narrowed: %#v", set)
					}
					return
				}
				want := []string{"CONFIG_DEFINED", "CONFIG_ELIFDEF", "CONFIG_ELIFNDEF", "CONFIG_IFDEF", "CONFIG_IFNDEF", "CONFIG_VALUE"}
				if set.Opaque || !slices.Equal(set.Symbols, want) ||
					!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", "include/strings.h"}) ||
					!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
					t.Fatalf("stringification lost exact reads/closure: %#v; want %q", set, want)
				}
			})
		}
	}
}

func TestConfigDependencyCallCoverageStringificationKeepsLexicalFallback(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#define RAW(x) #x\nconst char raw[] = RAW(CONFIG_RAW);\n__UNMEASURED_TAIL\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	context := newConfigDependencyAnalysisContext(plan)
	set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil || set.Opaque || set.Reason != "" || !slices.Equal(set.Symbols, []string{"CONFIG_RAW"}) ||
		!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) {
		t.Fatalf("incomplete refinement discarded conservative reads: %#v, %v", set, err)
	}
	completePrograms := 0
	for _, syntax := range context.conditionalSyntax {
		if syntax.conditionalProgram.callCoverage {
			completePrograms++
		}
	}
	if completePrograms != 1 {
		t.Fatalf("refinement was not attempted: %d complete programs", completePrograms)
	}
}

func TestConfigDependencyCallCoverageRetainsConfigMacroWrites(t *testing.T) {
	for _, test := range []struct {
		name, directive string
		want            []string
	}{
		{"object redefinition", "#define CONFIG_COLLISION 1\n", []string{"CONFIG_COLLISION"}},
		{"function redefinition", "#define CONFIG_COLLISION(x) x\n", []string{"CONFIG_COLLISION"}},
		{"unexpanded replacement", "#define CONFIG_COLLISION CONFIG_UNUSED_BODY\n", []string{"CONFIG_COLLISION"}},
		{"undefinition", "#undef CONFIG_COLLISION\n", []string{"CONFIG_COLLISION"}},
		{"inactive definition", "#if 0\n#define CONFIG_COLLISION 1\n#endif\n", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#define RAW(x) #x\nconst char raw[] = RAW(CONFIG_RAW);\n" + test.directive + "int value;\n",
			}, []string{"-nostdinc", "-Werror", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, map[string]string{
				"CONFIG_COLLISION": "2", "CONFIG_RAW": "y", "CONFIG_UNUSED_BODY": "y",
			})
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || set.Opaque || !slices.Equal(set.Symbols, test.want) ||
				!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) ||
				!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
				t.Fatalf("config write lost redefinition/diagnostic inputs: %#v, %v; want %q", set, err, test.want)
			}
		})
	}
}

func TestConfigDependencyCallCoverageStringificationCompilerReplacements(t *testing.T) {
	for _, commandLine := range []bool{false, true} {
		t.Run(fmt.Sprintf("argv-%t", commandLine), func(t *testing.T) {
			arguments := []string{"-nostdinc", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}
			predefines := "#define ALIAS CONFIG_VALUE\n#define UNUSED CONFIG_UNUSED_BODY\n"
			if commandLine {
				arguments = append(arguments, "-DALIAS=CONFIG_VALUE", "-DUNUSED=CONFIG_UNUSED_BODY")
				predefines = ""
			}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#define RAW(x) #x\n#define EXPAND(x) RAW(x)\nconst char raw[] = RAW(CONFIG_RAW);\nconst char expanded[] = EXPAND(ALIAS);\n",
			}, arguments, map[string]string{"CONFIG_VALUE": "7", "CONFIG_RAW": "y", "CONFIG_UNUSED_BODY": "y"})
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return predefines, true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_VALUE"}) ||
				!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) ||
				!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
				t.Fatalf("compiler replacement reads lost precision or authority: %#v, %v", set, err)
			}
		})
	}
}

func TestConfigDependencyCallCoverageSelfReferenceThroughProductionScanner(t *testing.T) {
	for _, tail := range []string{"", "__UNMEASURED_SELF_REFERENCE_TAIL\n"} {
		t.Run(fmt.Sprintf("opaque-tail-%t", tail != ""), func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <self-reference.h>\ninline int answer(void) { return PICK(DRIVER); }\n" + tail,
				"include/self-reference.h": "#define inline inline __gnu_inline __inline_maybe_unused notrace\n" +
					"#define __gnu_inline\n#define __inline_maybe_unused\n#define notrace\n#define PICK(x) CONFIG_ ## x\n",
			}, []string{
				"-nostdinc", "-I${tree:kernel}/include",
				"-include", "${tree:prep}/include/generated/autoconf.h",
				"-c", "drivers/example/driver.c",
			}, map[string]string{"CONFIG_DRIVER": "7", "CONFIG_UNRELATED": "y"})
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if tail != "" {
				if !set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
					t.Fatalf("self-reference prefix escaped after an unknown binding: %#v", set)
				}
				return
			}
			if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) ||
				!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", "include/self-reference.h"}) ||
				!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
				t.Fatalf("source-owned self-reference lost the complete compiler proof: %#v", set)
			}
		})
	}
}

func TestConfigDependencyCallCoverageSelfReferenceDoesNotLeakIntoNamespace(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define SELF SELF + CONFIG_FIRST\n")
	proof := &configDependencyCallCoverage{}
	if reason := proof.expand("SELF", state); reason != "" {
		t.Fatal(reason)
	}
	if state.definition("SELF") != configDependencyMacroDefined {
		t.Fatal("suppression changed the macro's global definedness")
	}
	state.set("SELF", configDependencyMacroUndefined)
	if reason := proof.expand("SELF", state); reason != "" {
		t.Fatal(reason)
	}
	if !state.setSourceMacroReplacement("SELF CONFIG_SECOND", "source:replacement", configDependencyMacroDefined) {
		t.Fatal("ordered replacement was rejected")
	}
	if reason := proof.expand("SELF", state); reason != "" {
		t.Fatal(reason)
	}
	if len(proof.reads) != 2 || !proof.reads["CONFIG_FIRST"] || !proof.reads["CONFIG_SECOND"] {
		t.Fatalf("suppression escaped its token occurrence: %#v", proof.reads)
	}
	var definitions []configDependencyMacroCallRead
	for _, read := range proof.definitions {
		if read.Name == "SELF" {
			definitions = append(definitions, read)
		}
	}
	if len(definitions) != 3 || definitions[0].State != configDependencyMacroDefined ||
		definitions[1].State != configDependencyMacroUndefined || definitions[1].DefinitionID != "" ||
		definitions[2].State != configDependencyMacroDefined || definitions[0].DefinitionID == definitions[2].DefinitionID ||
		definitions[2].Origin != "source:replacement" {
		t.Fatalf("ordered replacement lost exact witnesses: %#v", definitions)
	}

	state = macroReplacementStateForTest(t, "#define F(x) F\n")
	proof = &configDependencyCallCoverage{}
	if reason := proof.expand("F(0)", state); reason != "" {
		t.Fatal(reason)
	}
	if reason := proof.expand("(CONFIG_LATE)", state); reason == "" ||
		len(proof.reads) != 0 || len(proof.definitions) != 0 {
		t.Fatalf("unavailable token weakened the cross-event boundary: %#v, %q", proof, reason)
	}

	// The final public token list deliberately carries no new authority to
	// turn a suppressed but globally defined identifier into conditional zero.
	state = macroReplacementStateForTest(t, "#define SELF SELF\n")
	proof = &configDependencyCallCoverage{}
	value, reason := proof.condition("SELF", state)
	if reason == "" || value != configDependencyMacroUnknown ||
		state.definition("SELF") != configDependencyMacroDefined ||
		len(proof.reads) != 0 || len(proof.definitions) != 0 {
		t.Fatalf("suppressed conditional was guessed or leaked a prefix: value=%d reason=%q proof=%#v", value, reason, proof)
	}
}

func TestConfigDependencyCallCoverageSelfReferenceRetainsIntrinsicWitness(t *testing.T) {
	state := compilerIntrinsicStateWithDefinitionsForTest(t,
		"#define SELF SELF + CONFIG_READ + __has_builtin(__builtin_trap)\n",
		map[string]bool{"__has_builtin": true, "__builtin_trap": false})
	binding := state.macroReplacements["__has_builtin"].intrinsic
	calls := 0
	mode := configDependencyMacroCallMode{intrinsic: func(call CompilerIntrinsicCall) (string, string, string) {
		calls++
		if call != (CompilerIntrinsicCall{Operator: "__has_builtin", Operand: "__builtin_trap"}) {
			t.Fatalf("self-reference changed the intrinsic call: %#v", call)
		}
		return "7", "measured-answer", ""
	}}
	result, reason := configDependencyMacroCallExpand("SELF", state.resolveMacroCall, mode)
	if reason != "" || strings.Join(result.Tokens, " ") != "SELF + CONFIG_READ + 7" ||
		!slices.Equal(result.ConfigReads, []string{"CONFIG_READ"}) || calls != 1 || len(result.IntrinsicReads) != 1 {
		t.Fatalf("self-reference lost measured expansion: %#v, %q", result, reason)
	}
	read := result.IntrinsicReads[0]
	if binding == nil || read.BindingID != binding.identity || read.AnswerID != "measured-answer" || read.Token != "7" {
		t.Fatalf("self-reference changed intrinsic identity: %#v", read)
	}
	// A successful suppressed prefix is not a substitute for an answer.
	result, reason = configDependencyMacroCallExpand("SELF", state.resolveMacroCall, configDependencyMacroCallMode{})
	if reason == "" || len(result.Tokens) != 0 || len(result.ConfigReads) != 0 ||
		len(result.DefinitionReads) != 0 || len(result.IntrinsicReads) != 0 {
		t.Fatalf("missing intrinsic answer published a prefix: %#v, %q", result, reason)
	}
}

func TestConfigDependencyCallCoverageThroughProductionScanner(t *testing.T) {
	for _, member := range []string{"ordinary", "include", "incbin"} {
		t.Run(member, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <pick.h>\nstruct { int " + member + "; } s = { ." + member + " = PICK\n(DRIVER) };\n",
				"include/pick.h":           "#ifndef PICK_H\n#define PICK_H\n#define PICK(x) CONFIG_ ## x\n#endif\n",
			}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c"}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
				t.Fatalf("complete production call proof = %#v, %v", set, err)
			}
			if !slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", "include/pick.h"}) {
				t.Fatalf("lost authenticated source closure: %#v", set)
			}
		})
	}
}

func TestConfigDependencyCallCoverageFailureDoesNotPublishPrefix(t *testing.T) {
	for name, after := range map[string]string{
		"unmodeled_builtin":  "__UNMODELED_BUILTIN(UNKNOWN)\n",
		"unresolved_include": "#include <missing.h>\n",
		"unknown_condition":  "#if __UNQUERIED\nOTHER\n#endif\n",
		"dollar":             "$VALUE\n",
		"unknown_effect":     "#pragma custom effect\n",
	} {
		t.Run(name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#define PICK(x) CONFIG_ ## x\nPICK(DRIVER)\n" + after,
			}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
				t.Fatalf("partial proof escaped: %#v, %v", set, err)
			}
		})
	}
}

func TestConfigDependencyCallCoverageResolverKeepsUnknownAndUnmodeled(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define PICK(x) CONFIG_ ## x\n")
	state.set("BUILTIN", configDependencyMacroDefined)
	for _, input := range []string{"PICK(DRIVER) BUILTIN", "PICK(DRIVER) __UNQUERIED"} {
		proof := &configDependencyCallCoverage{}
		if reason := proof.expand(input, state); reason == "" || len(proof.reads) != 0 || len(proof.definitions) != 0 || proof.complete {
			t.Fatalf("unmodeled namespace admitted: %#v, %q", proof, reason)
		}
	}
}

func TestConfigDependencyCallCoverageCrossSpanBoundaries(t *testing.T) {
	for _, next := range []string{"(DRIVER)", "LP DRIVER )", "EMPTY LP DRIVER )"} {
		state := macroReplacementStateForTest(t, "#define PICK(x) CONFIG_ ## x\n#define LP (\n#define EMPTY\n")
		proof := &configDependencyCallCoverage{}
		if reason := proof.expand("PICK", state); reason != "" {
			t.Fatal(reason)
		}
		if reason := proof.expand("EMPTY", state); reason != "" {
			t.Fatal(reason)
		}
		if reason := proof.expand(next, state); reason == "" || len(proof.reads) != 0 || len(proof.definitions) != 0 {
			t.Fatalf("boundary admitted: %q %#v %q", next, proof, reason)
		}
	}
}

func TestConfigDependencyCallCoverageDollarRequiresCompilerEvidence(t *testing.T) {
	for _, test := range []struct {
		name                         string
		callback, value, ready, want bool
		flags                        []string
	}{
		{"measured assembly default", true, true, true, true, nil},
		{"missing evidence", false, false, false, false, nil},
		{"option alone", false, false, false, false, []string{"-fno-dollars-in-identifiers"}},
		{"negative evidence", true, false, true, false, []string{"-fno-dollars-in-identifiers"}},
		{"unready positive", true, true, false, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-nostdinc", "-x", "assembler-with-cpp"}, test.flags...)
			args = append(args, "-c", "drivers/example/driver.c")
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#define PICK(x) CONFIG_ ## x\nmov $PICK(DRIVER), %eax\n",
			}, args, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}
			if test.callback {
				plan.metadata.compilerDollarPunctuation = func(_, _, language string, _, _ []string, _ map[string]string) (bool, bool, error) {
					if language != "assembler-with-cpp" {
						t.Fatalf("lexical probe language = %q", language)
					}
					return test.value, test.ready, nil
				}
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || set.Opaque == test.want {
				t.Fatalf("compiler-measured call proof = %#v, %v", set, err)
			}
			if test.want && !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
				t.Fatalf("dollar-adjacent call lost config read: %#v", set)
			}
			if !test.want && (len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0) {
				t.Fatalf("unproved lexical mode published partial proof: %#v", set)
			}
		})
	}
}

func TestConfigDependencyMacroReplacementAutoconfCollisionMatchesSortedRenderer(t *testing.T) {
	for range 50 {
		state := macroReplacementStateForTest(t, "")
		state.applyResolvedConfigAutoconf(newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
			"CONFIG_COLLISION": "m", "CONFIG_COLLISION_MODULE": "7",
		}))
		if got := state.macroReplacements["CONFIG_COLLISION_MODULE"].text; got != "CONFIG_COLLISION_MODULE 7" {
			t.Fatalf("autoconf last-write order differs from sorted header renderer: %q", got)
		}
	}
}

func TestConfigDependencyCallCoverageSourceSpansNormalizeBeforeExpansion(t *testing.T) {
	state := macroReplacementStateForTest(t, "")
	program := compileConfigDependencyConditionalProgramWithCalls([]byte("#define PICK(x) CONFIG_ ## x\nPICK /* a\ncomment */ \\\n(DRIVER)\n"), "fixture:normalized", true)
	proof := &configDependencyCallCoverage{}
	_, reason := interpretConfigDependencyConditionalProgramWithText(program, state, nil, "c", proof.expand)
	if reason != "" || !proof.reads["CONFIG_DRIVER"] {
		t.Fatalf("phase normalization lost call: %#v %s", proof, reason)
	}
	if strings.Contains(state.macroReplacements["PICK"].text, "\n") {
		t.Fatal("definition retained unnormalized logical newline")
	}
}

func TestConfigDependencyCallCoverageRestoresProjectedCompilerDefinitions(t *testing.T) {
	for _, flags := range [][]string{
		{"-DPICK=SECRET"},
		{"-DPICK=WRONG", "-U", "PICK", "-D", "PICK=SECRET"},
		{"-DPICK=SECRET", "-DUNUSED=IGNORED"},
	} {
		plan, node := configDependencyCompilePlanForTest(t, map[string]string{
			"drivers/example/driver.c": "#define MAKE_(x) CONFIG_ ## x\n#define MAKE(x) MAKE_(x)\nint value = MAKE(PICK);\n",
		}, append(slices.Clone(flags), "-nostdinc", "-c", "drivers/example/driver.c"), nil)
		configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
			return "#define PICK 1\n#define UNUSED 1\n", true, nil
		}
		set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_SECRET"}) {
			t.Fatalf("projected compiler body leaked into call proof (%q): %#v, %v", flags, set, err)
		}
	}
}

func TestConfigDependencyCallCoverageProtectsDefinedOperand(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define NAME CONFIG_UNUSED_REPLACEMENT\n")
	proof := &configDependencyCallCoverage{}
	value, reason := proof.condition("defined(NAME) && !defined CONFIG_ABSENT", state)
	if reason != "" || value != configDependencyMacroDefined || proof.reads["CONFIG_UNUSED_REPLACEMENT"] {
		t.Fatalf("defined operand was macro-expanded: %d %q %#v", value, reason, proof)
	}
	if len(proof.conditions) != 2 || proof.conditions[0].Name != "NAME" || proof.conditions[1].Name != "CONFIG_ABSENT" {
		t.Fatalf("missing direct definedness reads: %#v", proof.conditions)
	}
}

func TestConfigDependencyCallCoverageDemandsUnknownDefinedOperand(t *testing.T) {
	for _, expression := range []string{"defined(__UNMEASURED)", "defined __UNMEASURED", "defined(__UNMEASURED) && defined(__LATER)"} {
		t.Run(expression, func(t *testing.T) {
			state := macroReplacementStateForTest(t, "")
			var demands []string
			proof := &configDependencyCallCoverage{unknown: func(name string) { demands = append(demands, name) }}
			value, reason := proof.condition(expression, state)
			if value != configDependencyMacroUnknown || reason != "unresolved defined operand __UNMEASURED" || !slices.Equal(demands, []string{"__UNMEASURED"}) {
				t.Fatalf("unknown conditional demand = %d, %q, %q", value, reason, demands)
			}
			if state.definition("__UNMEASURED") != configDependencyMacroUnknown || proof.complete || len(proof.conditions) != 0 || len(proof.reads) != 0 {
				t.Fatal("future definedness demand published partial current proof")
			}
			proof.condition("defined(__LATER)", state)
			if len(demands) != 1 {
				t.Fatal("failed coverage continued observing later operands")
			}
		})
	}
}

func TestConfigDependencyCallCoverageDefinedDemandDoesNotExpandOrGuess(t *testing.T) {
	for _, expression := range []string{"defined(NAME)", "!defined(CONFIG_ABSENT)", "defined(__UNMEASURED", "defined()"} {
		t.Run(expression, func(t *testing.T) {
			state := macroReplacementStateForTest(t, "#define NAME __UNMEASURED\n")
			proof := &configDependencyCallCoverage{unknown: func(name string) { t.Fatalf("unexpected demand %q", name) }}
			value, reason := proof.condition(expression, state)
			if expression == "defined(NAME)" || expression == "!defined(CONFIG_ABSENT)" {
				if value != configDependencyMacroDefined || reason != "" {
					t.Fatalf("known operand lost exact definedness: %d, %q", value, reason)
				}
			} else if value != configDependencyMacroUnknown || reason == "" {
				t.Fatal("malformed defined operand admitted")
			}
		})
	}
}

func TestConfigDependencyCallCoverageRejectsExpansionGeneratedDefined(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define DEFINED defined\n")
	proof := &configDependencyCallCoverage{}
	if value, reason := proof.condition("DEFINED(CONFIG_FEATURE)", state); reason == "" || value != configDependencyMacroUnknown || len(proof.reads) != 0 {
		t.Fatalf("expansion-generated operator admitted: %d %q %#v", value, reason, proof)
	}
}

func TestConfigDependencyCallCoverageConditionalMacroExpansion(t *testing.T) {
	const source = `#define READ(name) CONFIG_ ## name
#define PLACEHOLDER_1 0,
#define SECOND(ignored, value, ...) value
#define IS_SET(x) IS_SET_EXPAND(x)
#define IS_SET_EXPAND(x) IS_SET_ARGUMENT(PLACEHOLDER_ ## x)
#define IS_SET_ARGUMENT(x) SECOND(x 1, 0)
#if READ(VALUE) > 5 && IS_SET(CONFIG_ENABLED) && defined(CONFIG_VALUE)
int result = READ(SELECTED);
#elif __UNQUERIED_BUILTIN(0)
#error a taken branch must not expand later elif operands
#endif
`
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": source,
	}, []string{"-nostdinc", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, map[string]string{
		"CONFIG_VALUE": "7", "CONFIG_ENABLED": "y", "CONFIG_SELECTED": "9",
	})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_ENABLED", "CONFIG_SELECTED", "CONFIG_VALUE"}) {
		t.Fatalf("complete conditional macro proof = %#v, %v", set, err)
	}
	if !slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("conditional macro proof lost autoconf input: %#v", set)
	}
}

func TestConfigDependencyCallCoverageDoesNotGuessLanguageKeywords(t *testing.T) {
	for _, keyword := range []string{"true", "false", "not 0"} {
		plan, node := configDependencyCompilePlanForTest(t, map[string]string{
			"drivers/example/driver.c": "#define READ(name) CONFIG_ ## name\n#if " + keyword + "\nint result = READ(SELECTED);\n#else\nint result = READ(OTHER);\n#endif\n",
		}, []string{"-nostdinc", "-x", "c++", "-c", "drivers/example/driver.c"}, nil)
		configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
			return "", true, nil
		}
		set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil || !set.Opaque || len(set.Symbols) != 0 {
			t.Fatalf("language-dependent keyword guessed (%s): %#v %v", keyword, set, err)
		}
	}
}

func TestConfigDependencyCallCoverageRetainsSkippedConfigGuard(t *testing.T) {
	for _, paste := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			name := "literal"
			other, secret := "CONFIG_OTHER", "CONFIG_SECRET"
			if paste {
				name, other, secret = "pasted", "PICK(OTHER)", "PICK(SECRET)"
			}
			config := map[string]string{"CONFIG_OTHER": "7", "CONFIG_SECRET": "9"}
			if enabled {
				name += "/skipped"
				config["CONFIG_GATE"] = "y"
			} else {
				name += "/entered"
			}
			t.Run(name, func(t *testing.T) {
				plan, node := configDependencyCompilePlanForTest(t, map[string]string{
					"drivers/example/driver.c": "#define PICK(x) CONFIG_ ## x\n#include <guarded.h>\nint other = " + other + ";\n",
					"include/guarded.h":        "#ifndef CONFIG_GATE\n#define CONFIG_GATE 1\nint guarded = " + secret + ";\n#endif\n",
				}, []string{"-nostdinc", "-I${tree:kernel}/include", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, config)
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return "", true, nil
				}
				want := []string{"CONFIG_GATE", "CONFIG_OTHER"}
				if !enabled {
					want = append(want, "CONFIG_SECRET")
				}
				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				if err != nil || set.Opaque || !slices.Equal(set.Symbols, want) {
					t.Fatalf("include guard dependency = %#v, %v; want %q", set, err, want)
				}
			})
		}
	}
}

func TestConfigDependencyCallCoverageMergesGuardAndBodySymbols(t *testing.T) {
	guard := configDependencyParsedFile{symbols: []string{"CONFIG_GATE"}}
	body := configDependencyParsedFile{symbols: []string{"CONFIG_BODY", "CONFIG_GATE"}}
	for _, pair := range [][2]configDependencyParsedFile{{guard, body}, {body, guard}} {
		merged := mergeConfigDependencyConditionalParsed(pair[0], pair[1])
		if !slices.Equal(merged.symbols, []string{"CONFIG_BODY", "CONFIG_GATE"}) {
			t.Fatalf("lost symbols when merging guard and body: %q", merged.symbols)
		}
	}
}

func TestConfigDependencyCallCoverageRejectsTraditionalCommentPasting(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#define JOIN(x) CON/**/FIG_/**/x\nint result = JOIN(GATE);\n",
	}, []string{"-nostdinc", "-traditional-cpp", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, map[string]string{"CONFIG_GATE": "7"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || len(set.Symbols) != 0 || !strings.Contains(set.Reason, "unmodeled preprocessing mode") {
		t.Fatalf("traditional comment pasting was treated as ordinary tokens: %#v %v", set, err)
	}
}

func TestConfigDependencyCallCoverageRejectsNoncanonicalLineSplices(t *testing.T) {
	for _, separator := range []string{" \n", "\t\n", "\v\n", "\f\n", " \r\n", "\r"} {
		for _, directive := range []bool{false, true} {
			for _, paste := range []bool{false, true} {
				t.Run(fmt.Sprintf("separator=%q/directive=%t/paste=%t", separator, directive, paste), func(t *testing.T) {
					source := "int result = CONFI\\" + separator + "G_HIDDEN;\n"
					if directive {
						source = "#if CONFIG_GATE\nint result = 1;\n#el\\" + separator + "se\nint result = CONFIG_HIDDEN;\n#endif\n"
					}
					if paste {
						source = "#define PICK(x) CONFIG_ ## x\nint other = PICK(OTHER);\n" + source
					}
					plan, node := configDependencyCompilePlanForTest(t, map[string]string{
						"drivers/example/driver.c": source,
					}, []string{"-nostdinc", "-include", "${tree:prep}/include/generated/autoconf.h", "-c", "drivers/example/driver.c"}, map[string]string{
						"CONFIG_OTHER": "9", "CONFIG_HIDDEN": "7", "CONFIG_GATE": "y",
					})
					configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
					plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
						return "", true, nil
					}
					set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
					if err != nil || !set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 {
						t.Fatalf("noncanonical splice admitted a partial dependency proof: %#v %v", set, err)
					}
				})
			}
		}
	}
}
