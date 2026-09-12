package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyProjectedGuardAnswersRestoreOriginalReplacement(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const source = "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#if defined(__ANSWER)\nPICK(ON)\n#else\nVALUE\n#endif\n"
	var answers *KbuildCompilerGuardAnswers
	var toolsets map[string]string
	for _, value := range []string{"CONFIG_FIRST", "CONFIG_SECOND"} {
		plan, node := configDependencyCompilePlanForTest(t, map[string]string{pathname: source},
			[]string{"-nostdinc", "-DVALUE=" + value, "-c", pathname}, nil)
		configDependencyGuardAnswerCompilerForTest(plan, node)
		if answers == nil {
			answers = configDependencyGuardAnswersForTest(t, plan, node, []string{"__ANSWER"}, false)
			toolsets = maps.Clone(plan.Toolsets)
		} else {
			plan.Toolsets = maps.Clone(toolsets)
		}
		plan.metadata.SetCompilerGuardAnswers(answers)
		got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		want := []string{"CONFIG_PREFIX", value}
		slices.Sort(want)
		if err != nil || got.Opaque || !slices.Equal(got.Symbols, want) || !slices.Equal(got.SourcePaths, []string{pathname}) {
			t.Fatalf("%s lost its original replacement or borrowed a source receipt: %#v, %v", value, got, err)
		}
	}
}

func configDependencyGuardAnswerProbeForTest(
	t *testing.T, plan *ActionPlan, node ActionPlanNode,
) (string, string, configDependencyCompilerPredefineProbe) {
	t.Helper()
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, plan.Recipes[node.Recipe])
	if reason != "" {
		t.Fatal(reason)
	}
	sources, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	sources = configDependencyCompilerPredefineSourcePaths(invocation, sources)
	if invocation.hasPredefineProjection {
		invocation.arguments = invocation.predefineArguments
		invocation.kbuildStart = invocation.predefineKbuildStart
		invocation.kbuildEnd = invocation.predefineKbuildEnd
		invocation.probeEnvironment = invocation.predefineProbeEnvironment
	}
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(invocation, sources)
	if reason != "" {
		t.Fatal(reason)
	}
	return actionPlanConfigDependencyScope(node), invocation.tool, probe
}

// Every answer comes from an independently frozen supplemental discovery and
// replay plan with execution-shaped stdout. Multiple names are separate queries
// so a later cumulative round retains the previous query's exact request while
// adding the newly reached name; Answers must merge those measured batches.
func configDependencyGuardAnswersForTest(
	t *testing.T, plan *ActionPlan, node ActionPlanNode, names []string, value bool,
) *KbuildCompilerGuardAnswers {
	t.Helper()
	scope, role, probe := configDependencyGuardAnswerProbeForTest(t, plan, node)
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		values, ready, err := discovery.CompilerDefinedness(scope, role, probe.language,
			probe.arguments, probe.translationUnits, []string{name}, probe.environment)
		if err != nil || ready || values != nil {
			t.Fatalf("supplemental discovery = %v/%t/%v", values, ready, err)
		}
	}
	frozen, err := discovery.Plan()
	if err != nil || len(frozen.Nodes) != len(names) {
		t.Fatalf("frozen guard batch = %#v/%v, want %d requests", frozen, err, len(names))
	}
	// Production snapshots bind the actual configured identities, not the
	// arbitrary toolset placeholders used by generic dependency test fixtures.
	plan.Toolsets = maps.Clone(frozen.Toolsets)
	stdout := "0\n"
	if value {
		stdout = "1\n"
	}
	oracle := compilerDefinednessTestOracle(t, frozen, map[string]string{"compiler-definedness": stdout})
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		values, ready, err := replay.CompilerDefinedness(scope, role, probe.language,
			probe.arguments, probe.translationUnits, []string{name}, probe.environment)
		actual, found := values[name]
		if err != nil || !ready || len(values) != 1 || !found || actual != value {
			t.Fatalf("supplemental replay for %s = %v/%t/%v", name, values, ready, err)
		}
	}
	answers, err := replay.Answers()
	if err != nil {
		t.Fatal(err)
	}
	return answers
}

func configDependencyGuardAnswerCompilerForTest(plan *ActionPlan, node ActionPlanNode) {
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	// Deliberately do not install the ordinary inventory callback. Supplemental
	// state must work, and participate in completed-cache witnesses, on its own.
	plan.metadata.compilerDefinedness = nil
}

func TestConfigDependencySupplementalGuardAnswersRestartCompleteSourceProof(t *testing.T) {
	const source = `#define PICK(x) CONFIG_ ## x
#if defined(__FIRST)
int first = PICK(FIRST_ON);
#else
int first = PICK(FIRST_OFF);
#endif
#if defined(__SECOND)
int second = PICK(SECOND_ON);
#else
int second = PICK(SECOND_OFF);
#endif
`
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": source,
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencyGuardAnswerCompilerForTest(plan, node)
	for index, names := range [][]string{nil, {"__FIRST"}, {"__FIRST", "__SECOND"}} {
		if names != nil {
			plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, names, false))
		}
		// Each invocation constructs fresh initial compiler state and interprets
		// the original TU again; no preprocessed/branch-elided text is substituted.
		set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 {
			if !set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
				t.Fatalf("incomplete round %d published a partial source proof: %#v", index, set)
			}
			missing := "__FIRST"
			if index == 1 {
				missing = "__SECOND"
			}
			if !strings.Contains(set.Reason, missing) {
				t.Fatalf("round %d did not advance to the remaining guard %s: %#v", index, missing, set)
			}
			continue
		}
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_FIRST_OFF", "CONFIG_SECOND_OFF"}) ||
			!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) {
			t.Fatalf("completed supplemental round lost original-source proof: %#v", set)
		}
	}
}

func TestConfigDependencySupplementalEmptyGuardRoundsDoNotCertifyNewSource(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	arguments := []string{"-nostdinc", "-c", pathname}
	initial, initialNode := configDependencyCompilePlanForTest(t, map[string]string{
		pathname: "CONFIG_BEFORE\n",
	}, arguments, nil)
	configDependencyGuardAnswerCompilerForTest(initial, initialNode)
	// Freeze and replay an actual empty supplemental plan, then merge empty
	// rounds through the same API used by the coordinator. This carries no
	// compiler-definedness result, including no negative result for later names.
	empty := configDependencyGuardAnswersForTest(t, initial, initialNode, nil, false)
	carried, err := MergeKbuildCompilerGuardAnswers(empty, empty, empty)
	if err != nil {
		t.Fatal(err)
	}
	initial.metadata.SetCompilerGuardAnswers(carried)
	initialHints := 0
	initial.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		initialHints += len(value.Names)
		return nil
	})
	before, err := AnalyzeActionPlanNodeConfigDependencies(initial, initialNode)
	if err != nil || before.Opaque || !slices.Equal(before.Symbols, []string{"CONFIG_BEFORE"}) || initialHints != 0 {
		t.Fatalf("initial empty frontier = %#v, hints %d, %v", before, initialHints, err)
	}
	// A different immutable source snapshot retains the same logical source
	// path, argv and compiler identities. An old empty round is not a receipt
	// that the current source has no new reserved guards.
	current, currentNode := configDependencyCompilePlanForTest(t, map[string]string{
		pathname: "#define PICK(x) CONFIG_ ## x\n#if defined(__NEW_SOURCE_GUARD)\nPICK(NEW_ON)\n#else\nPICK(NEW_OFF)\n#endif\n",
	}, arguments, nil)
	configDependencyGuardAnswerCompilerForTest(current, currentNode)
	current.Toolsets = maps.Clone(initial.Toolsets)
	current.metadata.SetCompilerGuardAnswers(carried)
	key := func(plan *ActionPlan, node ActionPlanNode) configDependencyCompilerPredefineRequestKey {
		scope, role, probe := configDependencyGuardAnswerProbeForTest(t, plan, node)
		return configDependencyCompilerPredefineKey(scope, role, probe.language, probe.arguments, probe.translationUnits, probe.environment)
	}
	if key(initial, initialNode) != key(current, currentNode) {
		t.Fatal("fixture changed the compiler context instead of only source bytes")
	}
	// Final replay has no observer. It still must inspect the current original
	// source and refuse a dependency proof for an unanswered conditional.
	after, err := AnalyzeActionPlanNodeConfigDependencies(current, currentNode)
	if err != nil || !after.Opaque || !strings.Contains(after.Reason, "__NEW_SOURCE_GUARD") ||
		len(after.Symbols) != 0 || len(after.SourcePaths) != 0 || len(after.ObjectPaths) != 0 {
		t.Fatalf("empty rounds certified changed source or supplied implicit absence: %#v, %v", after, err)
	}
	// Only a newly replayed measured answer may make that source precise.
	measured := configDependencyGuardAnswersForTest(t, current, currentNode, []string{"__NEW_SOURCE_GUARD"}, false)
	completed, err := MergeKbuildCompilerGuardAnswers(carried, measured)
	if err != nil {
		t.Fatal(err)
	}
	current.metadata.SetCompilerGuardAnswers(completed)
	resolved, err := AnalyzeActionPlanNodeConfigDependencies(current, currentNode)
	if err != nil || resolved.Opaque || !slices.Equal(resolved.Symbols, []string{"CONFIG_NEW_OFF"}) ||
		!slices.Equal(resolved.SourcePaths, []string{pathname}) {
		t.Fatalf("measured new guard did not enable a fresh original-source proof: %#v, %v", resolved, err)
	}
}

func TestConfigDependencySupplementalGuardAnswersPrecedeOrderedSourceEffects(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"include/forced.h": `#define PICK(x) CONFIG_ ## x
#ifdef __SWITCH
int forced = PICK(FORCED_ON);
#else
int forced = PICK(FORCED_OFF);
#endif
#define __SWITCH 1
`,
		"drivers/example/driver.c": `#ifdef __SWITCH
int initial = PICK(SOURCE_ON);
#else
int initial = PICK(SOURCE_OFF);
#endif
#undef __SWITCH
#ifdef __SWITCH
int removed = PICK(AFTER_UNDEF_ON);
#else
int removed = PICK(AFTER_UNDEF_OFF);
#endif
#define __SWITCH 1
#ifdef __SWITCH
int restored = PICK(AFTER_DEFINE_ON);
#else
int restored = PICK(AFTER_DEFINE_OFF);
#endif
`,
	}, []string{"-nostdinc", "-include", "${tree:kernel}/include/forced.h", "-c", "drivers/example/driver.c"}, nil)
	configDependencyGuardAnswerCompilerForTest(plan, node)
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__SWITCH"}, false))
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	want := []string{"CONFIG_AFTER_DEFINE_ON", "CONFIG_AFTER_UNDEF_OFF", "CONFIG_FORCED_OFF", "CONFIG_SOURCE_ON"}
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, want) ||
		!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", "include/forced.h"}) {
		t.Fatalf("supplemental initial fact overrode ordered source effects: %#v/%v, want %v", set, err, want)
	}
}

func TestConfigDependencySupplementalGuardAnswersDoNotBorrowChangedContext(t *testing.T) {
	for _, change := range []string{"argument order", "language", "environment"} {
		t.Run(change, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#define PICK(x) CONFIG_ ## x\n#ifdef __SWITCH\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n",
			}, []string{"-nostdinc", "-DORDERED=1", "-UORDERED", "-c", "drivers/example/driver.c"}, nil)
			configDependencyGuardAnswerCompilerForTest(plan, node)
			plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__SWITCH"}, false))
			before, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || before.Opaque || !slices.Equal(before.Symbols, []string{"CONFIG_OFF"}) {
				t.Fatalf("original guard context = %#v/%v", before, err)
			}
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			switch change {
			case "argument order":
				recipe.Arguments[1], recipe.Arguments[2] = recipe.Arguments[2], recipe.Arguments[1]
			case "language":
				recipe.Arguments = append([]string{"-x", "assembler-with-cpp"}, recipe.Arguments...)
			case "environment":
				recipe.Environment = map[string]string{"COMPILER_MODE": "changed"}
			}
			plan.Recipes[node.Recipe] = recipe
			after, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !after.Opaque || len(after.Symbols) != 0 || len(after.SourcePaths) != 0 {
				t.Fatalf("changed %s borrowed measured initial namespace: %#v/%v", change, after, err)
			}
		})
	}
}

func TestConfigDependencySupplementalGuardAnswersCompletedFamilyCache(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c",
		"#define PICK(x) CONFIG_ ## x\n#ifdef __SWITCH\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index, value := range []bool{false, true, false} {
		plan, node := configDependencyCompletedCachePlanForTest(t, root, "guard-profile",
			fmt.Sprintf("guard-%d", index), "src-00000001", "image", "target-toolset", map[string]string{})
		configDependencyGuardAnswerCompilerForTest(plan, node)
		plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
			node.ID: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[node.Recipe].Arguments)},
		}
		plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__SWITCH"}, value))
		if plan.metadata.compilerDefinedness != nil {
			t.Fatal("fixture no longer tests supplemental-only cache authority")
		}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		want := "CONFIG_OFF"
		if value {
			want = "CONFIG_ON"
		}
		if set.Opaque || !slices.Equal(set.Symbols, []string{want}) {
			t.Fatalf("guard cache variant %d = %#v, want %s", index, set, want)
		}
		if index < 2 && cache.configDependencies.completed.hits != 0 {
			t.Fatal("different supplemental guard answer reused a completed source closure")
		}
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 2 {
		t.Fatalf("supplemental completed cache hits/stores = %d/%d, want 1/2",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencySupplementalGuardAnswersRejectChangedToolset(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#define PICK(x) CONFIG_ ## x\n#ifdef __SWITCH\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencyGuardAnswerCompilerForTest(plan, node)
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__SWITCH"}, false))
	plan.Toolsets["target"] = "other-configured-compiler"
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || len(set.Symbols) != 0 || !strings.Contains(set.Reason, "different target toolset") {
		t.Fatalf("changed toolset supplied scanner authority: %#v/%v", set, err)
	}
	plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
		node.ID: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[node.Recipe].Arguments)},
	}
	if _, err := BuildActionPlanConfigDependencyAnalysis(plan); err == nil || !strings.Contains(err.Error(), "different target toolset") {
		t.Fatalf("full-plan registration accepted changed-toolset answers: %v", err)
	}
}

func TestConfigDependencySupplementalGuardAnswersCompletedCacheForcedSourceHeader(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c",
		"#define PICK(x) CONFIG_ ## x\n#ifdef __SWITCH\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n")
	mustWriteSource(t, root, "include/forced.h",
		"#define PICK(x) CONFIG_ ## x\n#ifdef __SWITCH\nPICK(FORCED_ON)\n#else\nPICK(FORCED_OFF)\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index, value := range []bool{false, true, false} {
		plan, node := configDependencyCompletedCachePlanForTest(t, root, "forced-guard-profile",
			fmt.Sprintf("forced-guard-%d", index), "src-00000001", "image", "target-toolset", map[string]string{})
		header := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: "include/forced.h"}
		plan.Sources = append(plan.Sources, header)
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "forced", SourceID: header.ID})
		plan.Nodes[0] = node
		recipe := plan.Recipes[node.Recipe]
		recipe.Arguments = append([]string{"-include", "${source:forced:00000001}"}, recipe.Arguments...)
		recipe.Sources = append(recipe.Sources, "forced:00000001")
		plan.Recipes[node.Recipe] = recipe
		configDependencyGuardAnswerCompilerForTest(plan, node)
		plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
			node.ID: {Tool: "cc", Arguments: slices.Clone(recipe.Arguments)},
		}
		plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__SWITCH"}, value))
		if plan.metadata.compilerDefinedness != nil {
			t.Fatal("forced-header fixture no longer tests supplemental-only cache authority")
		}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		want := []string{"CONFIG_FORCED_OFF", "CONFIG_OFF"}
		if value {
			want = []string{"CONFIG_FORCED_ON", "CONFIG_ON"}
		}
		if set.Opaque || !slices.Equal(set.Symbols, want) ||
			!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", "include/forced.h"}) {
			t.Fatalf("forced-header guard variant %d = %#v, want %v and both source inputs", index, set, want)
		}
		if index < 2 && cache.configDependencies.completed.hits != 0 {
			t.Fatal("forced-header cache borrowed a different measured guard answer")
		}
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 2 {
		t.Fatalf("forced-source-header completed cache hits/stores = %d/%d, want 1/2",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}
