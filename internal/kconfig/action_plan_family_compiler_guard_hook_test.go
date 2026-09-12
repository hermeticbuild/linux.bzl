package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Keep the ordinary family-variant fixture intact, but bind its initial plan
// and detached answers to the same real configured probe toolset identities.
func familyCompilerGuardHookFixtureForTest(t *testing.T) (ActionPlanFamilyVariantPlanningOptions, *KbuildCompilerGuardAnswers, map[string]string) {
	t.Helper()
	workload := compilerDefinednessTestOptions(t)
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	workload.Host = &host
	scopes := compilerGuardBatchScopesForTest(t, workload)
	toolsets := compilerGuardBatchOrdinaryPlanForTest(t, scopes).Toolsets
	metadata := familyVariantMetadataForTest(t, nil)
	initial, err := metadata.ActionPlanFamilyVariant(toolsets["target"], toolsets["host"],
		ActionPlanFamilyVariantPlanningOptions{Variant: "base"})
	if err != nil {
		t.Fatal(err)
	}
	// Final plans intentionally detach metadata. A private fixture copy can use
	// the original metadata to recover the exact query context, without changing
	// the initial plan that will be authenticated by replay.
	probePlan := *initial.Plan
	probePlan.metadata = metadata
	var consumer ActionPlanNode
	for _, node := range probePlan.Nodes {
		for _, output := range node.Outputs {
			if output.Path != "drivers/variant.o" {
				continue
			}
			if consumer.ID != "" {
				t.Fatal("fixture has more than one compiler consumer")
			}
			consumer = node
		}
	}
	if consumer.ID == "" {
		t.Fatal("fixture has no compiler consumer")
	}
	scope, role, probe := configDependencyGuardAnswerProbeForTest(t, &probePlan, consumer)
	if scope != "target" || role != "cc" || probe.language != "c" {
		t.Fatalf("unexpected fixture compiler context: %s/%s/%s", scope, role, probe.language)
	}
	batch, _ := compilerGuardAnswerBatchForTest(t, scopes, probe.arguments, probe.translationUnits, probe.environment,
		[][]string{{"__HOOK_GUARD"}}, []string{"0\n"})
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	dependencies := maps.Clone(initial.Dependencies)
	for id := range dependencies {
		dependencies[id] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
	}
	files := familyTestConfig("1", "0")
	snapshot, err := canonicalActionPlanSnapshot(initial.Plan, dependencies, files)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	roots := executionCutRootsForTest(family, familyVariantTestHeader)
	if len(roots) != 1 {
		t.Fatalf("expected one generated-header root, got %v", roots)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	// The inactive nested guard is an unanswered discovery hint, not a
	// prerequisite for proving CONFIG_USED in the active source program.
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte(
		"#define PICK(x) CONFIG_ ## x\n#if defined(__HOOK_GUARD)\nPICK(WRONG)\n#else\nPICK(USED)\n#endif\n"+
			"#if 0\n#if defined(__HOOK_NEXT)\n#endif\n#endif\n")))
	if err != nil {
		t.Fatal(err)
	}
	return ActionPlanFamilyVariantPlanningOptions{
		Variant: "base", InitialSnapshot: &snapshot, Cut: cut, ObservedHeaders: observed, ResolvedConfigFiles: files,
	}, answers, toolsets
}

func TestActionPlanFamilyCompilerGuardHookPreparesBeforeAnalysis(t *testing.T) {
	options, answers, toolsets := familyCompilerGuardHookFixtureForTest(t)
	options.Cache = NewActionPlanFamilyPlanningCache()
	// Warm the same family cache without any supplemental facts. Authentic
	// observed bytes alone must not answer this reserved compiler guard.
	baseline, err := familyVariantMetadataForTest(t, nil).ActionPlanFamilyVariant(toolsets["target"], toolsets["host"], options)
	if err != nil {
		t.Fatal(err)
	}
	missingGuard := false
	for _, set := range baseline.Dependencies {
		missingGuard = missingGuard || set.Opaque && strings.Contains(set.Reason, "__HOOK_GUARD")
	}
	if !missingGuard {
		t.Fatalf("unmeasured compiler guard was accepted: %#v", baseline.Dependencies)
	}
	metadata := familyVariantMetadataForTest(t, nil)
	loweringCalls, hookCalls, compilerCalls := 0, 0, 0
	metadata.Config.KbuildProfiles[0].probeEnvironmentActivation = func() error {
		loweringCalls++
		return nil
	}
	metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		compilerCalls++
		if hookCalls != 1 || metadata.compilerGuardAnswers != answers || metadata.compilerGuardObserver == nil {
			t.Fatal("compiler namespace was created before guard preparation")
		}
		return "", true, nil
	}
	var observations []ConfigDependencyCompilerGuardObservation
	options.PrepareCompilerGuards = func() error {
		hookCalls++
		if hookCalls != 1 || loweringCalls == 0 || compilerCalls != 0 {
			t.Fatalf("hook ordering: lowering=%d hook=%d compiler=%d", loweringCalls, hookCalls, compilerCalls)
		}
		metadata.SetCompilerGuardAnswers(answers)
		metadata.SetCompilerGuardObserver(func(observation ConfigDependencyCompilerGuardObservation) error {
			if hookCalls != 1 || compilerCalls == 0 {
				t.Fatal("source scanner ran outside the prepared namespace")
			}
			observations = append(observations, observation)
			return nil
		})
		return nil
	}
	result, err := metadata.ActionPlanFamilyVariant(toolsets["target"], toolsets["host"], options)
	if err != nil {
		t.Fatal(err)
	}
	if hookCalls != 1 || compilerCalls == 0 || len(result.ObservedHeaderUses) != 1 {
		t.Fatalf("prepared replay: hook=%d compiler=%d uses=%#v", hookCalls, compilerCalls, result.ObservedHeaderUses)
	}
	use := result.ObservedHeaderUses[0]
	set := result.Dependencies[use.ConsumerNodeID]
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_USED"}) ||
		!slices.Contains(set.SourcePaths, "drivers/variant.c") || !slices.Contains(set.ObjectPaths, familyVariantTestHeader) {
		t.Fatalf("prepared answers did not enable a fresh complete source proof: %#v", set)
	}
	found := false
	for _, observation := range observations {
		if slices.Contains(observation.Names, "__HOOK_GUARD") {
			t.Fatalf("hook observer requested an already measured guard: %#v", observation)
		}
		if observation.Origin.LogicalPath == familyVariantTestHeader && slices.Equal(observation.Names, []string{"__HOOK_NEXT"}) {
			found = true
			if observation.Truncated || observation.Origin.Source || observation.Origin.ContentID != use.ContentID ||
				observation.Origin.OriginalProducerNodeID != use.OriginalProducerNodeID {
				t.Fatalf("hook observer lost authenticated generated-header origin: %#v", observation)
			}
		}
	}
	if !found {
		t.Fatalf("hook observer did not retain the unanswered generated-header hint: %#v", observations)
	}
}

func TestActionPlanFamilyCompilerGuardHookRejectsChangedReplay(t *testing.T) {
	for _, failure := range []string{"original-lowering", "resolved-config", "toolset", "variant", "uninitialized-cut"} {
		t.Run(failure, func(t *testing.T) {
			options := familyVariantReplayOptionsForTest(t)
			compilerCalls, hookCalls := 0, 0
			metadata := familyVariantMetadataForTest(t, &compilerCalls)
			target := actionPlanTestProbeIdentity
			switch failure {
			case "original-lowering":
				// Change source-owned Make text before this fresh evaluator lowers
				// anything. The changed command remains valid on its own.
				metadata.Config.KbuildProfiles[0].evaluator.template.setVariable("cmd_cc_o_c", kbuildVariable{
					value: "$(CC) -nostdinc -DCHANGED_ORIGINAL=1 -I$(objtree)/include/generated -c -o $@ $<", recursive: true,
				})
				if _, err := metadata.ActionPlan(target, actionPlanTestProbeIdentity); err != nil {
					t.Fatalf("changed ordinary lowering is invalid: %v", err)
				}
			case "resolved-config":
				options.ResolvedConfigFiles = maps.Clone(options.ResolvedConfigFiles)
				options.ResolvedConfigFiles["include/config/kernel.release"] = "different\n"
			case "toolset":
				target = "sha256-" + strings.Repeat("a", 64)
			case "variant":
				options.Variant = "other"
			case "uninitialized-cut":
				options.Cut = new(ActionPlanFamilyExecutionCut)
			}
			options.PrepareCompilerGuards = func() error { hookCalls++; return nil }
			result, err := metadata.ActionPlanFamilyVariant(target, actionPlanTestProbeIdentity, options)
			if err == nil || result != nil || hookCalls != 0 || compilerCalls != 0 {
				t.Fatalf("rejected replay reached guard preparation: result=%v hook=%d compiler=%d error=%v", result, hookCalls, compilerCalls, err)
			}
		})
	}
}

func TestActionPlanFamilyCompilerGuardHookRequiresCompleteReplay(t *testing.T) {
	var metadata *CompactMetadata
	for mask := 0; mask < 15; mask++ {
		t.Run(fmt.Sprintf("bundle-%02x", mask), func(t *testing.T) {
			calls := 0
			options := ActionPlanFamilyVariantPlanningOptions{
				Variant: "base", PrepareCompilerGuards: func() error { calls++; return nil },
			}
			if mask&1 != 0 {
				options.InitialSnapshot = new(ActionPlanSnapshot)
			}
			if mask&2 != 0 {
				options.Cut = new(ActionPlanFamilyExecutionCut)
			}
			if mask&4 != 0 {
				options.ObservedHeaders = new(ActionPlanFamilyObservedHeaders)
			}
			if mask&8 != 0 {
				options.ResolvedConfigFiles = map[string]string{}
			}
			want := "together"
			if mask == 0 {
				want = "require verified family replay"
			}
			result, err := metadata.ActionPlanFamilyVariant("invalid", "invalid", options)
			if err == nil || result != nil || calls != 0 || !strings.Contains(err.Error(), want) {
				t.Fatalf("incomplete replay reached lowering/hook: result=%v calls=%d error=%v", result, calls, err)
			}
		})
	}
}

func TestActionPlanFamilyCompilerGuardHookErrorStopsAnalysis(t *testing.T) {
	options := familyVariantReplayOptionsForTest(t)
	hookCalls, compilerCalls, observerCalls := 0, 0, 0
	metadata := familyVariantMetadataForTest(t, &compilerCalls)
	metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error { observerCalls++; return nil })
	want := errors.New("supplemental replay rejected")
	options.PrepareCompilerGuards = func() error { hookCalls++; return want }
	result, err := metadata.ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, options)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "prepare supplemental compiler guards") ||
		result != nil || hookCalls != 1 || compilerCalls != 0 || observerCalls != 0 {
		t.Fatalf("hook failure reached analysis: result=%v hook=%d compiler=%d observer=%d error=%v", result, hookCalls, compilerCalls, observerCalls, err)
	}
}
