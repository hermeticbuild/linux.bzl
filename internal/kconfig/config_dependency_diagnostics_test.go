package kconfig

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func configDependencyDiagnosticPlanForTest(t *testing.T) *ActionPlan {
	t.Helper()
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/forced.h":   "#ifndef FORCED_H\n#define FORCED_H\nCONFIG_FORCED\n#endif\n",
	}, []string{
		"-nostdinc", "-include", "${tree:kernel}/include/linux/forced.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define __STDC__ 1\n", true, nil
	}
	second := node
	second.ID = "compile-second"
	second.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/second.o"}}
	for selection := range plan.selectionGraph.materializedProducers {
		selection.target = second.Outputs[0].Path
		plan.selectionGraph.selections[selection] = CompactKbuildSelection{Profile: selection.profile}
		plan.selectionGraph.materializedProducers[selection] = second.ID
		break
	}
	plan.Recipes["archive"] = ActionRecipe{Kind: "archive", Tool: "ar"}
	plan.Nodes = []ActionPlanNode{node, {
		ID: "archive", Stage: "target", Kind: "archive", Recipe: "archive", Tool: "ar",
	}, second}
	return plan
}

func configDependencyDiagnosticEventsForTest(events []ConfigDependencyDiagnosticSnapshot) []string {
	names := make([]string, len(events))
	for index, event := range events {
		names[index] = event.Event
	}
	return names
}

func TestConfigDependencyDiagnosticsDisabledAndEquivalent(t *testing.T) {
	var absent *ActionPlanFamilyPlanningCache
	absent.SetConfigDependencyDiagnosticObserver(nil)
	disabled := NewActionPlanFamilyPlanningCache()
	disabledPlan := configDependencyDiagnosticPlanForTest(t)
	want := configDependencyBuildWithFamilyCacheForTest(t, disabledPlan, disabled)
	if disabled.configDependencyDiagnosticSequence != 0 || newConfigDependencyDiagnostics(disabledPlan) != nil {
		t.Fatal("disabled diagnostics created an analysis observation")
	}

	enabled := NewActionPlanFamilyPlanningCache()
	var events []ConfigDependencyDiagnosticSnapshot
	enabled.SetConfigDependencyDiagnosticObserver(func(event ConfigDependencyDiagnosticSnapshot) {
		events = append(events, event)
	})
	plan := configDependencyDiagnosticPlanForTest(t)
	if newConfigDependencyDiagnostics(plan) != nil {
		t.Fatal("unattached plan acquired another family's observer")
	}
	got := configDependencyBuildWithFamilyCacheForTest(t, plan, enabled)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enabled dependency sets = %#v, disabled = %#v", got, want)
	}
	if got[plan.Nodes[0].ID].Opaque || got[plan.Nodes[2].ID].Opaque {
		t.Fatalf("forced-header fixture was not precise: %#v", got)
	}
	wantEvents := []string{"analysis_start", "compiler_start", "compiler_complete", "compiler_start", "compiler_complete", "analysis_end"}
	if !slices.Equal(configDependencyDiagnosticEventsForTest(events), wantEvents) {
		t.Fatalf("events = %#v", events)
	}
	for index, event := range events {
		wantStarted := []int{0, 1, 1, 2, 2, 2}[index]
		wantCompleted := []int{0, 0, 1, 1, 2, 2}[index]
		if event.AnalysisSequence != 1 || event.TotalCompileNodes != 2 ||
			event.StartedCompileNodes != wantStarted || event.CompletedCompileNodes != wantCompleted || event.PublishedAt.IsZero() ||
			event.PreciseCompileNodes != wantCompleted || event.OpaqueCompileNodes != 0 || event.LastOpaqueReason != "" {
			t.Fatalf("event %d = %#v", index, event)
		}
		if event.Event != "compiler_start" && event.CurrentOutput != "" {
			t.Fatalf("event %d retained an active output: %#v", index, event)
		}
		if index > 0 && event.PublishedAt.Before(events[index-1].PublishedAt) {
			t.Fatalf("publication time moved backwards at event %d", index)
		}
	}
	if events[1].CurrentOutput != "target:drivers/example/driver.o" ||
		events[3].CurrentOutput != "target:drivers/example/second.o" {
		t.Fatalf("compiler labels = %q, %q", events[1].CurrentOutput, events[3].CurrentOutput)
	}
	if end := events[len(events)-1]; end.ForcedCacheHits != 1 || end.ForcedCacheMisses != 1 {
		t.Fatalf("forced-prefix counters = %#v", end)
	}

	// A new analysis resets forced-prefix counters but advances the family sequence.
	configDependencyBuildWithFamilyCacheForTest(t, plan, enabled)
	if start := events[6]; start.AnalysisSequence != 2 || start.ForcedCacheHits != 0 || start.ForcedCacheMisses != 0 {
		t.Fatalf("second analysis start = %#v", start)
	}
	if end := events[len(events)-1]; end.AnalysisSequence != 2 || end.ForcedCacheHits != 1 || end.ForcedCacheMisses != 1 {
		t.Fatalf("second analysis end = %#v", end)
	}
	// Retained snapshots are independent values, including after observer removal.
	enabled.SetConfigDependencyDiagnosticObserver(nil)
	configDependencyBuildWithFamilyCacheForTest(t, plan, enabled)
	if len(events) != 12 || events[0].StartedCompileNodes != 0 || enabled.configDependencyDiagnosticSequence != 2 {
		t.Fatalf("disabled or retained observations changed: %#v", events)
	}
}

func TestConfigDependencyDiagnosticsCompletedCacheIsFamilyCumulative(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	var events []ConfigDependencyDiagnosticSnapshot
	cache.SetConfigDependencyDiagnosticObserver(func(event ConfigDependencyDiagnosticSnapshot) {
		events = append(events, event)
	})
	for index, unrelated := range []string{"n", "y"} {
		plan, _ := configDependencyCompletedCachePlanForTest(t, root,
			fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			"src-00000001", "image", "target-toolset",
			map[string]string{"CONFIG_RELEVANT": "y", "CONFIG_UNRELATED": unrelated})
		configDependencyBuildWithFamilyCacheForTest(t, plan, cache)
	}
	if len(events) != 8 {
		t.Fatalf("events = %#v", events)
	}
	if first := events[3]; first.CompletedCacheHits != 0 || first.CompletedCacheMisses != 1 {
		t.Fatalf("first analysis end = %#v", first)
	}
	if second := events[4]; second.AnalysisSequence != 2 || second.CompletedCacheHits != 0 || second.CompletedCacheMisses != 1 {
		t.Fatalf("second analysis start lost family counters: %#v", second)
	}
	if end := events[7]; end.CompletedCacheHits != 1 || end.CompletedCacheMisses != 1 || end.CompletedCompileNodes != 1 {
		t.Fatalf("completed-cache hit was not counted as a processed node: %#v", end)
	}
}

func TestConfigDependencyDiagnosticsErrorKeepsCompletedPrefix(t *testing.T) {
	plan := configDependencyDiagnosticPlanForTest(t)
	plan.Nodes[2].Recipe = "missing-recipe"
	trailing := plan.Nodes[2]
	trailing.ID = "unvisited-compiler"
	plan.Nodes = append(plan.Nodes, trailing)
	cache := NewActionPlanFamilyPlanningCache()
	var events []ConfigDependencyDiagnosticSnapshot
	cache.SetConfigDependencyDiagnosticObserver(func(event ConfigDependencyDiagnosticSnapshot) {
		events = append(events, event)
	})
	plan.attachFamilyPlanningCache(cache)
	_, err := BuildActionPlanConfigDependencyAnalysisWithCache(plan, cache.configDependencyCache())
	if err == nil || !strings.Contains(err.Error(), "missing-recipe") {
		t.Fatalf("missing recipe error = %v", err)
	}
	wantEvents := []string{"analysis_start", "compiler_start", "compiler_complete", "compiler_start", "analysis_end"}
	if !slices.Equal(configDependencyDiagnosticEventsForTest(events), wantEvents) {
		t.Fatalf("failure prefix = %#v", events)
	}
	if end := events[4]; end.TotalCompileNodes != 3 || end.StartedCompileNodes != 2 || end.CompletedCompileNodes != 1 || end.CurrentOutput != "" {
		t.Fatalf("failure end manufactured completion or retained its label: %#v", end)
	}
}

func TestConfigDependencyDiagnosticsStartsBeforeProbeAndEndsOnErrorOrPanic(t *testing.T) {
	for _, panicProbe := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic-%t", panicProbe), func(t *testing.T) {
			plan := configDependencyDiagnosticPlanForTest(t)
			plan.Nodes = plan.Nodes[:1]
			node := plan.Nodes[0]
			plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
				node.ID: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[node.Recipe].Arguments)},
			}
			cache := NewActionPlanFamilyPlanningCache()
			var events []ConfigDependencyDiagnosticSnapshot
			cache.SetConfigDependencyDiagnosticObserver(func(event ConfigDependencyDiagnosticSnapshot) {
				events = append(events, event)
			})
			sentinel := errors.New("diagnostic probe sentinel")
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				if len(events) != 2 || events[1].Event != "compiler_start" {
					t.Fatalf("probe ran before compiler-start observation: %#v", events)
				}
				if panicProbe {
					panic(sentinel)
				}
				return "", false, sentinel
			}
			plan.attachFamilyPlanningCache(cache)
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_, err = BuildActionPlanConfigDependencyAnalysisWithCache(plan, cache.configDependencyCache())
			}()
			if panicProbe && recovered != sentinel || !panicProbe && (recovered != nil || !errors.Is(err, sentinel)) {
				t.Fatalf("probe outcome changed: err=%v panic=%v", err, recovered)
			}
			if !slices.Equal(configDependencyDiagnosticEventsForTest(events), []string{"analysis_start", "compiler_start", "analysis_end"}) {
				t.Fatalf("probe-failure events = %#v", events)
			}
			if end := events[2]; end.StartedCompileNodes != 1 || end.CompletedCompileNodes != 0 || end.CurrentOutput != "" {
				t.Fatalf("probe-failure end = %#v", end)
			}
		})
	}
}

func TestConfigDependencyDiagnosticsCompletedClassifications(t *testing.T) {
	cache := NewActionPlanFamilyPlanningCache()
	var events []ConfigDependencyDiagnosticSnapshot
	cache.SetConfigDependencyDiagnosticObserver(func(value ConfigDependencyDiagnosticSnapshot) {
		events = append(events, value)
	})
	diagnostics := newConfigDependencyDiagnostics(&ActionPlan{familyPlanningCache: cache})
	reason := strings.Repeat("界", 600)
	diagnostics.compilerComplete(opaqueConfigDependency(reason))
	diagnostics.compilerComplete(ConfigDependencySet{})
	diagnostics.end()
	if len(events) != 3 || events[0].CompletedCompileNodes != 1 ||
		events[0].OpaqueCompileNodes != 1 || events[0].PreciseCompileNodes != 0 ||
		events[0].LastOpaqueReason != strings.Repeat("界", 341) ||
		!utf8.ValidString(events[0].LastOpaqueReason) {
		t.Fatalf("initial opaque classification = %#v", events)
	}
	for _, event := range events[1:] {
		if event.CompletedCompileNodes != 2 || event.OpaqueCompileNodes != 1 ||
			event.PreciseCompileNodes != 1 || event.LastOpaqueReason != events[0].LastOpaqueReason {
			t.Fatalf("completed classifications changed or lost last reason: %#v", event)
		}
	}
	// A new analysis starts with no retained classification or reason.
	second := newConfigDependencyDiagnostics(&ActionPlan{familyPlanningCache: cache})
	second.publish("analysis_start")
	if event := events[3]; event.CompletedCompileNodes != 0 || event.PreciseCompileNodes != 0 ||
		event.OpaqueCompileNodes != 0 || event.LastOpaqueReason != "" {
		t.Fatalf("new analysis retained old classifications: %#v", event)
	}
}

func TestConfigDependencyDiagnosticsHeaderAdmissionSnapshots(t *testing.T) {
	cache := NewActionPlanFamilyPlanningCache()
	var events []ConfigDependencyDiagnosticSnapshot
	cache.SetConfigDependencyDiagnosticObserver(func(value ConfigDependencyDiagnosticSnapshot) {
		events = append(events, value)
	})
	diagnostics := newConfigDependencyDiagnostics(&ActionPlan{familyPlanningCache: cache})
	forced := &configDependencyForcedHeaderCache{}
	diagnostics.context = &configDependencyAnalysisContext{forcedHeaders: forced}
	forced.ordinary = configDependencyHeaderCache{
		captureAttempts: 2, misses: 5, rejectedRecords: 4, admissionStopped: true,
	}
	diagnostics.publish("compiler_complete")
	forced.ordinary.captureAttempts++
	forced.ordinary.admissionStopped = false
	diagnostics.publish("analysis_end")
	if len(events) != 2 || events[0].HeaderCacheCaptureAttempts != 2 ||
		!events[0].HeaderCacheAdmissionStopped || events[0].HeaderCacheMisses != 5 ||
		events[0].HeaderCacheRejectedRecords != 4 || events[1].HeaderCacheCaptureAttempts != 3 ||
		events[1].HeaderCacheAdmissionStopped {
		t.Fatalf("header admission observations were lost or mutated: %#v", events)
	}
}

func TestConfigDependencyDiagnosticOutputIsBounded(t *testing.T) {
	for _, fixture := range []struct {
		name string
		node ActionPlanNode
		want string
	}{
		{name: "ascii", node: ActionPlanNode{Outputs: []ActionPlanOutput{{Path: strings.Repeat("a", 2048)}}}, want: strings.Repeat("a", 1024)},
		{name: "utf8", node: ActionPlanNode{Stage: "target", Outputs: []ActionPlanOutput{{Path: strings.Repeat("界", 600)}}}, want: "target:" + strings.Repeat("界", 339)},
		{name: "fallback", node: ActionPlanNode{ID: "compile-without-output", Stage: "host"}, want: "host:compile-without-output"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cache := NewActionPlanFamilyPlanningCache()
			var event ConfigDependencyDiagnosticSnapshot
			cache.SetConfigDependencyDiagnosticObserver(func(value ConfigDependencyDiagnosticSnapshot) { event = value })
			plan := &ActionPlan{familyPlanningCache: cache}
			diagnostics := newConfigDependencyDiagnostics(plan)
			diagnostics.compilerStart(fixture.node)
			if event.CurrentOutput != fixture.want || len(event.CurrentOutput) > 1024 || !utf8.ValidString(event.CurrentOutput) {
				t.Fatalf("bounded output = %q (%d bytes)", event.CurrentOutput, len(event.CurrentOutput))
			}
		})
	}
}
