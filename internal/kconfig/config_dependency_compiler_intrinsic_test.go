package kconfig

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compilerIntrinsicStateForTest(t *testing.T, predefines string) *configDependencyMacroState {
	t.Helper()
	return compilerIntrinsicStateWithDefinitionsForTest(t, predefines, map[string]bool{"__has_attribute": true, "__retain__": false})
}

func compilerIntrinsicStateWithDefinitionsForTest(t *testing.T, predefines string, definitions map[string]bool) *configDependencyMacroState {
	t.Helper()
	state := macroReplacementStateForTest(t, predefines)
	updated, valid := configDependencyApplyCompilerDefinedness(*state, definitions, "measured-definedness")
	if !valid {
		t.Fatal("initial definedness rejected")
	}
	state = &updated
	state.installInitialCompilerIntrinsics(definitions, "exact-compiler-context", predefines, "measured-definedness")
	return state
}

func TestCompilerIntrinsicInitialOperatorsAreIndependent(t *testing.T) {
	for _, attributeMode := range []string{"unknown", "absent", "textual", "measured"} {
		for _, builtinMode := range []string{"unknown", "absent", "textual", "measured"} {
			t.Run(attributeMode+"/"+builtinMode, func(t *testing.T) {
				modes := map[string]string{"__has_attribute": attributeMode, "__has_builtin": builtinMode}
				definitions := map[string]bool{}
				var predefines strings.Builder
				for _, operator := range []string{"__has_attribute", "__has_builtin"} {
					mode := modes[operator]
					if mode != "unknown" {
						definitions[operator] = mode != "absent"
					}
					if mode == "textual" {
						fmt.Fprintf(&predefines, "#define %s(x) 0\n", operator)
					}
				}
				state := compilerIntrinsicStateWithDefinitionsForTest(t, predefines.String(), definitions)
				for operator, mode := range modes {
					binding, reason := state.resolveMacroCall(operator)
					if reason != "" || (binding.intrinsic != nil) != (mode == "measured") {
						t.Fatalf("%s %s binding = %#v / %q", operator, mode, binding, reason)
					}
					if mode == "textual" && binding.definition == nil {
						t.Fatalf("textual %s fallback was discarded", operator)
					}
				}
				if attributeMode == "measured" && builtinMode == "measured" &&
					state.macroReplacements["__has_attribute"].intrinsic.identity == state.macroReplacements["__has_builtin"].intrinsic.identity {
					t.Fatal("different operators aliased their binding witnesses")
				}
			})
		}
	}
}

func TestBuiltinCompilerIntrinsicOrderedWritesPreserveOtherOperator(t *testing.T) {
	for name, mutate := range map[string]func(*configDependencyMacroState){
		"undef":            func(s *configDependencyMacroState) { s.set("__has_builtin", configDependencyMacroUndefined) },
		"same definedness": func(s *configDependencyMacroState) { s.set("__has_builtin", configDependencyMacroDefined) },
		"source": func(s *configDependencyMacroState) {
			s.setSourceMacroReplacement("__has_builtin(x) 7", "source:1", configDependencyMacroDefined)
		},
		"uncertain source": func(s *configDependencyMacroState) {
			s.setSourceMacroReplacement("__has_builtin(x) 7", "source:1", configDependencyMacroUnknown)
		},
		"numeric header": func(s *configDependencyMacroState) { s.applyValidatedNumericMacroHeader() },
		"argv define": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-D__has_builtin(x)=7"})
		},
		"argv undef": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-U__has_builtin"})
		},
		"argv restore": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-U__has_builtin", "-D__has_builtin(x)=7"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := compilerIntrinsicStateWithDefinitionsForTest(t, "", map[string]bool{"__has_attribute": true, "__has_builtin": true})
			child := root.branch()
			mutate(child)
			if child.macroReplacements["__has_builtin"].intrinsic != nil {
				t.Fatal("write retained builtin operator authority")
			}
			if name != "numeric header" && child.macroReplacements["__has_attribute"].intrinsic == nil {
				t.Fatal("builtin-only write revoked unrelated attribute authority")
			}
			if root.macroReplacements["__has_builtin"].intrinsic == nil || root.macroReplacements["__has_attribute"].intrinsic == nil {
				t.Fatal("child write mutated the parent's bindings")
			}
			root.mergePossibleBranch(child)
			if root.macroReplacements["__has_builtin"].intrinsic != nil {
				t.Fatal("uncertain join restored revoked builtin authority")
			}
		})
	}
}

func TestBuiltinCompilerIntrinsicExactIntegerWrapperAndCollision(t *testing.T) {
	state := compilerIntrinsicStateWithDefinitionsForTest(t,
		"#define WRAP(x) __has_builtin(x)\n#define ARG __builtin_dynamic_object_size\n",
		map[string]bool{"__has_builtin": true, "__has_attribute": true, "__builtin_dynamic_object_size": false})
	proof := &configDependencyCallCoverage{intrinsic: func(call CompilerIntrinsicCall) (string, string, string) {
		if call != (CompilerIntrinsicCall{"__has_builtin", "__builtin_dynamic_object_size"}) {
			t.Fatalf("unexpected query %#v", call)
		}
		return "7", "builtin-answer", ""
	}}
	value, reason := proof.condition("WRAP(ARG) > 1 && WRAP(ARG) - 6 == 1", state)
	if reason != "" || value != configDependencyMacroDefined || len(proof.intrinsics) != 2 {
		t.Fatalf("exact builtin integer/wrapper proof = %d/%q/%#v", value, reason, proof)
	}
	if !slices.ContainsFunc(proof.definitions, func(read configDependencyMacroCallRead) bool { return read.Name == "ARG" && read.DefinitionID != "" }) {
		t.Fatal("builtin wrapper lost the operand expansion witness")
	}
	for _, expression := range []string{"__has_builtin(__has_attribute)", "__has_attribute(__has_builtin)"} {
		got, reason := configDependencyMacroCallExpand(expression, state.resolveMacroCall, configDependencyMacroCallMode{
			intrinsic: func(CompilerIntrinsicCall) (string, string, string) {
				t.Fatal("operator collision reached oracle")
				return "", "", ""
			},
		})
		if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
			t.Fatalf("operator collision published a proof: %#v/%q", got, reason)
		}
	}
}

func TestCompilerIntrinsicInitialWitnessRequiresMeasuredNontextualBinding(t *testing.T) {
	for _, test := range []struct {
		name, predefines, context, identity string
		definitions                         map[string]bool
		want                                bool
	}{
		{"measured", "", "context", "measured", map[string]bool{"__has_attribute": true}, true},
		{"unmeasured", "", "context", "measured", nil, false},
		{"negative", "", "context", "measured", map[string]bool{"__has_attribute": false}, false},
		{"no context", "", "", "measured", map[string]bool{"__has_attribute": true}, false},
		{"no identity", "", "context", "", map[string]bool{"__has_attribute": true}, false},
		{"textual", "#define __has_attribute(x) 1\n", "context", "measured", map[string]bool{"__has_attribute": true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := macroReplacementStateForTest(t, test.predefines)
			if test.definitions["__has_attribute"] {
				// Preserve textual definitions; only absent cells receive the
				// independently measured definedness in this constructor test.
				if state.definition("__has_attribute") != configDependencyMacroDefined {
					state.set("__has_attribute", configDependencyMacroDefined)
				}
			}
			state.installInitialCompilerIntrinsics(test.definitions, configDependencyCompilerPredefineRequestKey(test.context), test.predefines, test.identity)
			binding, reason := state.resolveMacroCall("__has_attribute")
			if reason != "" || (binding.intrinsic != nil) != test.want {
				t.Fatalf("initial binding = %#v / %q", binding, reason)
			}
		})
	}
}

func TestFeatureIntrinsicWitnessUsesMeasuredAnswersAndOrdinaryFallbacks(t *testing.T) {
	for _, operator := range []string{"__has_feature", "__has_extension"} {
		for _, token := range []string{"0", "1"} {
			t.Run(operator+"/"+token, func(t *testing.T) {
				state := compilerIntrinsicStateWithDefinitionsForTest(t, "",
					map[string]bool{operator: true, "address_sanitizer": false})
				calls := 0
				proof := &configDependencyCallCoverage{intrinsic: func(call CompilerIntrinsicCall) (string, string, string) {
					calls++
					if call != (CompilerIntrinsicCall{operator, "address_sanitizer"}) {
						t.Fatalf("unexpected feature request %#v", call)
					}
					return token, "independent-measured-answer-" + token, ""
				}}
				value, reason := proof.condition(operator+"(address_sanitizer)", state)
				want := configDependencyMacroUndefined
				if token == "1" {
					want = configDependencyMacroDefined
				}
				if reason != "" || value != want || calls != 1 || len(proof.intrinsics) != 1 {
					t.Fatalf("feature answer = %v/%q, calls=%d, witnesses=%v", value, reason, calls, proof.intrinsics)
				}
				// A source fallback is an ordinary function macro, even when
				// the selected compiler also has a builtin with this name.
				state.setSourceMacroReplacement(operator+"(x) 0", "source-fallback", configDependencyMacroDefined)
				binding, reason := state.resolveMacroCall(operator)
				if reason != "" || binding.intrinsic != nil {
					t.Fatalf("source replacement kept builtin authority: %#v/%q", binding, reason)
				}
				proof = &configDependencyCallCoverage{intrinsic: func(CompilerIntrinsicCall) (string, string, string) {
					t.Fatal("ordinary source fallback reached compiler oracle")
					return "", "", ""
				}}
				value, reason = proof.condition(operator+"(address_sanitizer)", state)
				if reason != "" || value != configDependencyMacroUndefined {
					t.Fatalf("source fallback = %v/%q", value, reason)
				}
			})
		}
	}
}

func TestCompilerIntrinsicWitnessRevokedByEveryOrderedWrite(t *testing.T) {
	for name, mutate := range map[string]func(*configDependencyMacroState){
		"undef":            func(s *configDependencyMacroState) { s.set("__has_attribute", configDependencyMacroUndefined) },
		"same definedness": func(s *configDependencyMacroState) { s.set("__has_attribute", configDependencyMacroDefined) },
		"source": func(s *configDependencyMacroState) {
			s.setSourceMacroReplacement("__has_attribute(x) 202311", "source:1", configDependencyMacroDefined)
		},
		"uncertain source": func(s *configDependencyMacroState) {
			s.setSourceMacroReplacement("__has_attribute(x) 202311", "source:1", configDependencyMacroUnknown)
		},
		"numeric header": func(s *configDependencyMacroState) { s.applyValidatedNumericMacroHeader() },
		"argv define": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-D__has_attribute(x)=202311"})
		},
		"argv undef": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-U__has_attribute"})
		},
		"argv restore": func(s *configDependencyMacroState) {
			s.applyCompilerMacroReplacements("cc", []string{"-U__has_attribute", "-D__has_attribute(x)=202311"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := compilerIntrinsicStateForTest(t, "")
			child := root.branch()
			mutate(child)
			if child.macroReplacements["__has_attribute"].intrinsic != nil {
				t.Fatal("write retained initial binding authority")
			}
			if root.macroReplacements["__has_attribute"].intrinsic == nil {
				t.Fatal("child revoked immutable parent binding")
			}
			root.mergePossibleBranch(child)
			if root.macroReplacements["__has_attribute"].intrinsic != nil {
				t.Fatal("uncertain join restored initial binding authority")
			}
		})
	}
}

func TestCompilerIntrinsicExactIntegerAndReads(t *testing.T) {
	state := compilerIntrinsicStateForTest(t, "#define WRAP(x) __has_attribute(x)\n#define ARG deprecated\n")
	for _, expression := range []string{
		"__has_attribute(deprecated) == 202311",
		"WRAP(ARG) > 1 && WRAP(ARG) - 202310 == 1",
		"__has_attribute(deprecated) / 100 == 2023",
	} {
		proof := &configDependencyCallCoverage{intrinsic: func(call CompilerIntrinsicCall) (string, string, string) {
			if call != (CompilerIntrinsicCall{"__has_attribute", "deprecated"}) {
				t.Fatalf("query = %#v", call)
			}
			return "202311", "measured-call-identity", ""
		}}
		value, reason := proof.condition(expression, state)
		if reason != "" || value != configDependencyMacroDefined || len(proof.intrinsics) == 0 {
			t.Fatalf("exact numeric condition %q = %d/%q/%#v", expression, value, reason, proof)
		}
		for _, read := range proof.intrinsics {
			if read.BindingID == "" || read.AnswerID != "measured-call-identity" || read.Token != "202311" {
				t.Fatalf("lost binding/result witness: %#v", read)
			}
		}
		if strings.Contains(expression, "WRAP") && !slices.ContainsFunc(proof.definitions, func(read configDependencyMacroCallRead) bool { return read.Name == "ARG" && read.DefinitionID != "" }) {
			t.Fatal("wrapper expansion lost operand definition read")
		}
	}
}

func TestCompilerIntrinsicOperandAndAnswerFailuresPublishNoPrefix(t *testing.T) {
	for _, test := range []struct{ input, token, identity, reason string }{
		{"__has_attribute(ARG)", "1", "answer", ""},
		{"__has_attribute(__UNKNOWN)", "1", "answer", ""},
		{"__has_attribute(__has_attribute)", "1", "answer", ""},
		{"__has_attribute(a,b)", "1", "answer", ""},
		{"__has_attribute(a b)", "1", "answer", ""},
		{"__has_attribute(\"deprecated\")", "1", "answer", ""},
		{"__has_attribute(deprecated)", "1\n", "answer", ""},
		{"__has_attribute(deprecated)", "1 + 1", "answer", ""},
		{"__has_attribute(deprecated)", "1", "", ""},
		{"__has_attribute(deprecated)", "", "", "pending measured answer"},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s/%s", test.input, test.token, test.identity, test.reason), func(t *testing.T) {
			state := compilerIntrinsicStateForTest(t, "#define ARG deprecated\n#define PICK(x) CONFIG_ ## x\n")
			result, reason := configDependencyMacroCallExpand("PICK(PREFIX) "+test.input, state.resolveMacroCall, configDependencyMacroCallMode{
				intrinsic: func(CompilerIntrinsicCall) (string, string, string) { return test.token, test.identity, test.reason },
			})
			if reason == "" || !reflect.DeepEqual(result, configDependencyMacroCallResult{}) {
				t.Fatalf("partial intrinsic proof escaped: %#v / %q", result, reason)
			}
		})
	}
}

func TestCompilerIntrinsicLocallyUndefinedOperandIsNotReexpanded(t *testing.T) {
	state := compilerIntrinsicStateForTest(t, "#define ARG deprecated\n")
	state.set("ARG", configDependencyMacroUndefined)
	proof := &configDependencyCallCoverage{intrinsic: func(call CompilerIntrinsicCall) (string, string, string) {
		if call.Operand != "ARG" {
			t.Fatalf("locally undefined operand reexpanded: %#v", call)
		}
		return "0", "measured-with-frozen-operand", ""
	}}
	value, reason := proof.condition("!__has_attribute(ARG)", state)
	if reason != "" || value != configDependencyMacroDefined {
		t.Fatalf("condition = %d/%q", value, reason)
	}
}

func compilerIntrinsicAnswersForPlanTest(t *testing.T, plan *ActionPlan, node ActionPlanNode, call CompilerIntrinsicCall, token string) *KbuildCompilerGuardAnswers {
	t.Helper()
	scope, role, probe := configDependencyGuardAnswerProbeForTest(t, plan, node)
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := discovery.CompilerIntrinsicInteger(scope, role, probe.language, probe.arguments, probe.translationUnits, call.Operator, call.Operand, probe.environment); err != nil || ready {
		t.Fatalf("discovery = %t/%v", ready, err)
	}
	frozen, err := discovery.Plan()
	if err != nil {
		t.Fatal(err)
	}
	plan.Toolsets = maps.Clone(frozen.Toolsets)
	oracle := compilerDefinednessTestOracle(t, frozen, map[string]string{"compiler-intrinsic-integer": token + "\n"})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	if got, ready, err := replay.CompilerIntrinsicInteger(scope, role, probe.language, probe.arguments, probe.translationUnits, call.Operator, call.Operand, probe.environment); err != nil || !ready || got != token {
		t.Fatalf("replay = %q/%t/%v", got, ready, err)
	}
	answers, err := replay.Answers()
	if err != nil {
		t.Fatal(err)
	}
	return answers
}

func TestCompilerIntrinsicProductionScannerRestartsWithMeasuredInteger(t *testing.T) {
	const source = "#define PICK(x) CONFIG_ ## x\n#if __has_attribute(deprecated) >= 202311\nPICK(NEW)\n#else\nPICK(OLD)\n#endif\n"
	for _, token := range []string{"0", "1", "202311"} {
		t.Run(token, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{"drivers/example/driver.c": source}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			definedness := configDependencyGuardAnswersForTest(t, plan, node, []string{"__has_attribute"}, true)
			plan.metadata.SetCompilerGuardAnswers(definedness)
			pending, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !pending.Opaque {
				t.Fatalf("unmeasured call granted precision: %#v/%v", pending, err)
			}
			calls := compilerIntrinsicAnswersForPlanTest(t, plan, node, CompilerIntrinsicCall{"__has_attribute", "deprecated"}, token)
			answers, err := MergeKbuildCompilerGuardAnswers(definedness, calls)
			if err != nil {
				t.Fatal(err)
			}
			plan.metadata.SetCompilerGuardAnswers(answers)
			result, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			want := "CONFIG_OLD"
			if token == "202311" {
				want = "CONFIG_NEW"
			}
			if err != nil || result.Opaque || !slices.Equal(result.Symbols, []string{want}) || !slices.Equal(result.SourcePaths, []string{"drivers/example/driver.c"}) {
				t.Fatalf("measured call source proof = %#v/%v", result, err)
			}
		})
	}
}

func TestBuiltinProductionScannerRestartAndSourceFallback(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const source = `#define PICK(x) CONFIG_ ## x
#ifndef __has_builtin
#define __has_builtin(x) 0
#endif
#define HAS(x) __has_builtin(x)
#if HAS(__builtin_dynamic_object_size) > 1
PICK(NEW)
#else
PICK(OLD)
#endif
int value = HAS(__builtin_dynamic_object_size);
`
	for _, token := range []string{"0", "1", "7", "absent"} {
		t.Run(token, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-c", pathname}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			presence := configDependencyGuardAnswersForTest(t, plan, node, []string{"__has_builtin"}, token != "absent")
			operand := configDependencyGuardAnswersForTest(t, plan, node, []string{"__builtin_dynamic_object_size"}, false)
			definedness, err := MergeKbuildCompilerGuardAnswers(presence, operand)
			if err != nil {
				t.Fatal(err)
			}
			plan.metadata.SetCompilerGuardAnswers(definedness)
			pending, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if token == "absent" {
				if pending.Opaque || !slices.Equal(pending.Symbols, []string{"CONFIG_OLD"}) {
					t.Fatalf("measured absence did not retain source fallback: %#v", pending)
				}
				return // No intrinsic call result may be required for this macro.
			}
			if !pending.Opaque {
				t.Fatal("unanswered builtin query granted precision")
			}
			calls := compilerIntrinsicAnswersForPlanTest(t, plan, node, CompilerIntrinsicCall{"__has_builtin", "__builtin_dynamic_object_size"}, token)
			answers, err := MergeKbuildCompilerGuardAnswers(definedness, calls)
			if err != nil {
				t.Fatal(err)
			}
			plan.metadata.SetCompilerGuardAnswers(answers)
			result, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			want := "CONFIG_OLD"
			if token == "7" {
				want = "CONFIG_NEW"
			}
			if err != nil || result.Opaque || !slices.Equal(result.Symbols, []string{want}) || !slices.Equal(result.SourcePaths, []string{pathname}) {
				t.Fatalf("measured builtin conditional/body proof = %#v/%v", result, err)
			}
		})
	}
}
