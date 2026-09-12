package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

// Both nodes belong to the same immutable plan/profile and share one forced
// prefix. Only their translation unit and declared working-input bindings vary.
// Construct the whole plan before creating the analysis context, so these tests
// do not rely on mutating an immutable source snapshot or stale context indexes.
func configDependencyForcedResolutionPlanForTest(
	t *testing.T,
	files map[string]string,
) (*ActionPlan, ActionPlanNode, ActionPlanNode) {
	t.Helper()
	files = maps.Clone(files)
	if files == nil {
		files = map[string]string{}
	}
	files["drivers/example/driver.c"] = "CONFIG_DRIVER_A\n"
	files["drivers/example/driver_b.c"] = "CONFIG_DRIVER_B\n"
	files["include/linux/forced.h"] = "#include <selected.h>\nCONFIG_FORCED\n"
	plan, first := configDependencyCompilePlanForTest(t, files, []string{
		"-nostdinc", "-I${tree:prep}/include/early", "-I${tree:kernel}/include/fallback",
		"-include", "${tree:kernel}/include/linux/forced.h", "-c", "drivers/example/driver.c",
	}, nil)
	secondSource := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/driver_b.c"}
	secondRecipe := plan.Recipes[first.Recipe]
	secondRecipe.Arguments = slices.Clone(secondRecipe.Arguments)
	secondRecipe.Arguments[len(secondRecipe.Arguments)-1] = secondSource.Path
	second := first
	second.ID, second.Recipe = "compile-second", "compile-second-recipe"
	second.Sources = []ActionPlanSourceEdge{{Role: "source", SourceID: secondSource.ID}}
	second.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/driver_b.o"}}
	plan.Sources = append(plan.Sources, secondSource)
	plan.Recipes[second.Recipe] = secondRecipe
	plan.Nodes = append(plan.Nodes, second)
	selection := compactKbuildSelectionKey{profile: "config-dependency", target: second.Outputs[0].Path, stage: second.Stage}
	plan.selectionGraph.selections[selection] = CompactKbuildSelection{Profile: selection.profile}
	plan.selectionGraph.materializedProducers[selection] = second.ID
	configDependencySetCompilerContractForTest(plan, first, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define SHARED_COMPILER_PREDEFINE 1\n", true, nil
	}
	return plan, first, second
}

func configDependencyForcedResolutionBindSourceForTest(
	t *testing.T,
	plan *ActionPlan,
	node *ActionPlanNode,
	pathname string,
	sourcePath string,
) {
	t.Helper()
	source := ActionPlanSource{
		ID: fmt.Sprintf("src-%08d", len(plan.Sources)+1), Namespace: "kernel", Path: sourcePath,
	}
	plan.Sources = append(plan.Sources, source)
	node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
		SourceID: source.ID, CompilerUse: true,
	})
	for index := range plan.Nodes {
		if plan.Nodes[index].ID == node.ID {
			plan.Nodes[index] = *node
			return
		}
	}
	t.Fatal("bound compiler node is missing from plan")
}

func configDependencyForcedResolutionCompareForTest(
	t *testing.T,
	plan *ActionPlan,
	node ActionPlanNode,
	context *configDependencyAnalysisContext,
	wantSymbols []string,
	wantObjectPaths []string,
) ConfigDependencySet {
	t.Helper()
	freshContext := newConfigDependencyAnalysisContext(plan)
	freshContext.forcedHeaders = nil
	fresh, err := analyzeActionPlanNodeConfigDependencies(plan, node, freshContext)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Opaque || !slices.Equal(fresh.Symbols, wantSymbols) || !slices.Equal(fresh.ObjectPaths, wantObjectPaths) {
		t.Fatalf("cache-disabled node %q result = %#v, want symbols %q and objects %q", node.ID, fresh, wantSymbols, wantObjectPaths)
	}
	cached, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	if !configDependencySetsEqual(cached, fresh) {
		t.Fatalf("node %q cached forced prefix differs from cache-disabled analysis:\ncached: %#v\nfresh:  %#v", node.ID, cached, fresh)
	}
	return cached
}

func configDependencyForcedResolutionRequireSpecializationsForTest(t *testing.T, context *configDependencyAnalysisContext) {
	t.Helper()
	cache := context.forcedHeaders
	if cache == nil || cache.hits != 2 || cache.misses != 2 || len(cache.entries) != 1 {
		t.Fatalf("forced cache accounting = %#v, want two misses/two hits under one structural key", cache)
	}
	for _, entries := range cache.entries {
		if len(entries) != 2 {
			t.Fatalf("forced resolution specializations = %d, want two", len(entries))
		}
	}
}

func TestConfigDependencyForcedHeaderResolutionInvalidatesEarlierSourceShadow(t *testing.T) {
	plan, first, second := configDependencyForcedResolutionPlanForTest(t, map[string]string{
		"include/fallback/selected.h": "CONFIG_OLD\n", "immutable/shadow.h": "CONFIG_NEW\n",
	})
	const destination = "include/early/selected.h"
	configDependencyForcedResolutionBindSourceForTest(t, plan, &second, destination, "immutable/shadow.h")
	context := newConfigDependencyAnalysisContext(plan)
	if context.generated[destination] {
		t.Fatal("source-shadow fixture accidentally declared a generated output")
	}
	for range 2 {
		configDependencyForcedResolutionCompareForTest(t, plan, first, context,
			[]string{"CONFIG_DRIVER_A", "CONFIG_FORCED", "CONFIG_OLD"}, nil)
		configDependencyForcedResolutionCompareForTest(t, plan, second, context,
			[]string{"CONFIG_DRIVER_B", "CONFIG_FORCED", "CONFIG_NEW"}, []string{destination})
	}
	configDependencyForcedResolutionRequireSpecializationsForTest(t, context)
}

func TestConfigDependencyForcedHeaderResolutionInvalidatesSourceRebinding(t *testing.T) {
	plan, first, second := configDependencyForcedResolutionPlanForTest(t, map[string]string{
		"immutable/first.h": "CONFIG_SELECTED_A\n", "immutable/second.h": "CONFIG_SELECTED_B\n",
	})
	const destination = "include/early/selected.h"
	configDependencyForcedResolutionBindSourceForTest(t, plan, &first, destination, "immutable/first.h")
	configDependencyForcedResolutionBindSourceForTest(t, plan, &second, destination, "immutable/second.h")
	context := newConfigDependencyAnalysisContext(plan)
	for range 2 {
		configDependencyForcedResolutionCompareForTest(t, plan, first, context,
			[]string{"CONFIG_DRIVER_A", "CONFIG_FORCED", "CONFIG_SELECTED_A"}, []string{destination})
		configDependencyForcedResolutionCompareForTest(t, plan, second, context,
			[]string{"CONFIG_DRIVER_B", "CONFIG_FORCED", "CONFIG_SELECTED_B"}, []string{destination})
	}
	configDependencyForcedResolutionRequireSpecializationsForTest(t, context)
}

func TestConfigDependencyForcedHeaderResolutionRetainsGeneratedRebindingGuard(t *testing.T) {
	plan, first, second := configDependencyForcedResolutionPlanForTest(t, nil)
	const destination = "include/early/selected.h"
	for index, node := range []*ActionPlanNode{&first, &second} {
		configDependencyStageGeneratedHeaderForTest(t, plan, node, fmt.Sprintf("selected-producer-%d", index), destination, ActionRecipe{
			Tool: "actionfile", Arguments: []string{
				"-out", "${output:00000000}", "-line", fmt.Sprintf("CONFIG_SELECTED_%d", index),
			},
		})
	}
	context := newConfigDependencyAnalysisContext(plan)
	for range 2 {
		configDependencyForcedResolutionCompareForTest(t, plan, first, context,
			[]string{"CONFIG_DRIVER_A", "CONFIG_FORCED", "CONFIG_SELECTED_0"}, []string{destination})
		configDependencyForcedResolutionCompareForTest(t, plan, second, context,
			[]string{"CONFIG_DRIVER_B", "CONFIG_FORCED", "CONFIG_SELECTED_1"}, []string{destination})
	}
	configDependencyForcedResolutionRequireSpecializationsForTest(t, context)
}

func TestConfigDependencyForcedHeaderResolutionRequiresCompleteCaptureTrace(t *testing.T) {
	for _, test := range []struct {
		name  string
		trace func() *configDependencyHeaderTrace
	}{
		{name: "missing", trace: func() *configDependencyHeaderTrace { return nil }},
		{name: "wrong mode", trace: func() *configDependencyHeaderTrace { return &configDependencyHeaderTrace{} }},
		{name: "invalid", trace: func() *configDependencyHeaderTrace {
			trace := newConfigDependencyForcedHeaderResolutionTrace()
			trace.invalid = true
			return trace
		}},
		{name: "record overflow", trace: func() *configDependencyHeaderTrace {
			trace := newConfigDependencyForcedHeaderResolutionTrace()
			trace.maximumRecords = 1
			trace.record(configDependencyHeaderEvent{kind: configDependencyHeaderResolve})
			trace.record(configDependencyHeaderEvent{kind: configDependencyHeaderResolve})
			return trace
		}},
		{name: "byte overflow", trace: func() *configDependencyHeaderTrace {
			trace := newConfigDependencyForcedHeaderResolutionTrace()
			trace.maximumBytes = 1
			trace.record(configDependencyHeaderEvent{
				kind: configDependencyHeaderResolve, include: configDependencyLiteralInclude{name: "selected.h"},
			})
			return trace
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			state := root.branch()
			if !state.beginForcedHeaderTrace() {
				t.Fatal("cannot begin macro trace")
			}
			state.set("PREFIX_EFFECT", configDependencyMacroDefined)
			scanner := &configDependencyClosureScanner{headerTrace: test.trace()}
			if _, captured := captureConfigDependencyForcedHeaderCacheEntry(scanner, state, root, nil); captured {
				t.Fatal("incomplete include-resolution evidence was admitted")
			}
			if state.tainted || scanner.opaqueReason != "" || state.definition("PREFIX_EFFECT") != configDependencyMacroDefined {
				t.Fatal("declining incomplete cache evidence changed successful interpretation")
			}
		})
	}
}

func TestConfigDependencyForcedHeaderResolutionAllowsExplicitEmptyProof(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	entry := captureForcedHeaderMacroEntryForTest(t, root, func(state *configDependencyMacroState) {
		state.set("PREFIX_EFFECT", configDependencyMacroDefined)
	})
	if !entry.includeResolutionsReady || len(entry.includeResolutions) != 0 {
		t.Fatal("macro-only capture did not retain explicit empty resolution evidence")
	}
	restored, ok := entry.restore(&configDependencyClosureScanner{}, root)
	if !ok || restored.definition("PREFIX_EFFECT") != configDependencyMacroDefined {
		t.Fatal("explicitly complete empty witness did not preserve macro-only replay")
	}
}

func TestConfigDependencyForcedHeaderResolutionRejectsIncompleteEntry(t *testing.T) {
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	entry := captureForcedHeaderMacroEntryForTest(t, root, func(state *configDependencyMacroState) {
		state.set("PREFIX_EFFECT", configDependencyMacroDefined)
	})
	entry.includeResolutionsReady = false
	scanner := &configDependencyClosureScanner{}
	if _, ok := entry.restore(scanner, root); ok {
		t.Fatal("entry without complete include-resolution evidence was restored")
	}
	cache := &configDependencyForcedHeaderCache{}
	cache.store(configDependencyForcedHeaderCacheKey{}, entry)
	if len(cache.entries) != 0 {
		t.Fatal("entry without complete include-resolution evidence was retained")
	}
	if root.definition("PREFIX_EFFECT") != configDependencyMacroUndefined || scanner.opaqueReason != "" || len(scanner.symbols) != 0 {
		t.Fatal("rejecting an incomplete entry changed live macro/scanner state")
	}
}
