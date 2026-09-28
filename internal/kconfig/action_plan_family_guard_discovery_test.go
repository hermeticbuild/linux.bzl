package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverVerifiedCompilerGuardsMatchesFullReplay(t *testing.T) {
	options, answers, toolsets := familyCompilerGuardHookFixtureForTest(t)
	type compilerRequest struct {
		scope, role, language string
		arguments, units      []string
		environment           map[string]string
	}
	// The common immutable source root belongs to the parent test, so it
	// survives the cold subtest and actually exercises the warm parse cache.
	sourceRoots := maps.Clone(familyVariantMetadataForTest(t, nil).Config.KbuildProfiles[0].evaluator.template.sourceRoots)
	run := func(t *testing.T, discovery bool, cache *ActionPlanFamilyPlanningCache) ([]ConfigDependencyCompilerGuardObservation, []compilerRequest) {
		t.Helper()
		metadata := familyVariantMetadataForTest(t, nil)
		metadata.Config.KbuildProfiles[0].evaluator.template.sourceRoots = maps.Clone(sourceRoots)
		var observations []ConfigDependencyCompilerGuardObservation
		var requests []compilerRequest
		loweringCalls, hookCalls := 0, 0
		metadata.Config.KbuildProfiles[0].probeEnvironmentActivation = func() error { loweringCalls++; return nil }
		metadata.compilerPredefines = func(scope, role, language string, arguments, units []string, environment map[string]string) (string, bool, error) {
			if hookCalls != 1 || loweringCalls == 0 {
				t.Fatal("compiler state was initialized before fresh lowering and guard preparation")
			}
			requests = append(requests, compilerRequest{scope, role, language, slices.Clone(arguments), slices.Clone(units), maps.Clone(environment)})
			return "", true, nil
		}
		current := options
		current.Cache = cache
		current.PrepareCompilerGuards = func() error {
			hookCalls++
			if hookCalls != 1 || loweringCalls == 0 || len(requests) != 0 {
				t.Fatal("guard preparation changed its lowering/analysis order")
			}
			metadata.SetCompilerGuardAnswers(answers)
			metadata.SetCompilerGuardObserver(func(observation ConfigDependencyCompilerGuardObservation) error {
				observations = append(observations, observation)
				return nil
			})
			return nil
		}
		if discovery {
			// This API has no final plan, dependency annotation, or receipt result.
			if err := metadata.DiscoverVerifiedCompilerGuards(toolsets["target"], toolsets["host"], current); err != nil {
				t.Fatal(err)
			}
		} else {
			result, err := metadata.ActionPlanFamilyVariant(toolsets["target"], toolsets["host"], current)
			if err != nil {
				t.Fatal(err)
			}
			if result.Plan == nil || len(result.ObservedHeaderUses) != 1 || result.analysis == nil {
				t.Fatal("full replay lost final planning evidence")
			}
			use := result.ObservedHeaderUses[0]
			if set := result.Dependencies[use.ConsumerNodeID]; set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_USED"}) {
				t.Fatalf("full replay lost current source precision: %#v", set)
			}
		}
		if hookCalls != 1 || loweringCalls == 0 || len(requests) == 0 {
			t.Fatal("discovery skipped fresh lowering, preparation, or compiler registration")
		}
		found := false
		for _, observation := range observations {
			if observation.Origin.LogicalPath == familyVariantTestHeader && slices.Equal(observation.Names, []string{"__HOOK_NEXT"}) {
				found = true
				if observation.Truncated || observation.Origin.Source || observation.Origin.OriginalProducerNodeID == "" || observation.Origin.ContentID == "" {
					t.Fatal("discovery lost authenticated generated-header provenance")
				}
			}
		}
		if !found {
			t.Fatalf("discovery missed the unanswered reached guard: %#v", observations)
		}
		return observations, requests
	}
	fullCache, discoveryCache := NewActionPlanFamilyPlanningCache(), NewActionPlanFamilyPlanningCache()
	for _, pass := range []string{"cold", "warm"} {
		t.Run(pass, func(t *testing.T) {
			want, wantRequests := run(t, false, fullCache)
			got, gotRequests := run(t, true, discoveryCache)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotRequests, wantRequests) {
				t.Fatalf("guard-only discovery changed observations or exact compiler requests\ngot=%#v\nwant=%#v", got, want)
			}
		})
	}
}

func TestDiscoverVerifiedCompilerGuardsRequiresCompleteReplayAndPreparation(t *testing.T) {
	var metadata *CompactMetadata // Invalid options must fail before lowering.
	for mask := 0; mask < 16; mask++ {
		for _, hook := range []bool{false, true} {
			if mask == 15 && hook {
				continue
			}
			t.Run(fmt.Sprintf("bundle-%02x-hook-%t", mask, hook), func(t *testing.T) {
				calls := 0
				options := ActionPlanFamilyVariantPlanningOptions{Variant: "base"}
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
				if hook {
					options.PrepareCompilerGuards = func() error { calls++; return nil }
				}
				if err := metadata.DiscoverVerifiedCompilerGuards("invalid", "invalid", options); err == nil || calls != 0 {
					t.Fatalf("incomplete guard discovery reached preparation: calls=%d, error=%v", calls, err)
				}
			})
		}
	}
}

func TestDiscoverVerifiedCompilerGuardsRejectsChangedReplayBeforePreparation(t *testing.T) {
	for _, failure := range []string{"lowering", "config", "toolset", "variant", "cut"} {
		t.Run(failure, func(t *testing.T) {
			options := familyVariantReplayOptionsForTest(t)
			compilerCalls, hookCalls := 0, 0
			metadata := familyVariantMetadataForTest(t, &compilerCalls)
			target := actionPlanTestProbeIdentity
			switch failure {
			case "lowering":
				metadata.Config.KbuildProfiles[0].evaluator.template.setVariable("cmd_cc_o_c", kbuildVariable{
					value: "$(CC) -nostdinc -DCHANGED=1 -I$(objtree)/include/generated -c -o $@ $<", recursive: true,
				})
			case "config":
				options.ResolvedConfigFiles = maps.Clone(options.ResolvedConfigFiles)
				options.ResolvedConfigFiles["include/config/kernel.release"] = "changed\n"
			case "toolset":
				target = "sha256-" + strings.Repeat("a", 64)
			case "variant":
				options.Variant = "other"
			case "cut":
				options.Cut = new(ActionPlanFamilyExecutionCut)
			}
			options.PrepareCompilerGuards = func() error { hookCalls++; return nil }
			if err := metadata.DiscoverVerifiedCompilerGuards(target, actionPlanTestProbeIdentity, options); err == nil || hookCalls != 0 || compilerCalls != 0 {
				t.Fatalf("changed replay reached preparation or scan: hook=%d compiler=%d error=%v", hookCalls, compilerCalls, err)
			}
		})
	}
}

func TestDiscoverVerifiedCompilerGuardsPreservesHookAndObserverErrors(t *testing.T) {
	for _, failure := range []string{"preparation", "observer", "generated-content"} {
		t.Run(failure, func(t *testing.T) {
			options, answers, toolsets := familyCompilerGuardHookFixtureForTest(t)
			metadata := familyVariantMetadataForTest(t, nil)
			want := errors.New("rejected " + failure)
			observerCalls := 0
			if failure == "generated-content" {
				altered := *options.ObservedHeaders
				altered.contents = maps.Clone(altered.contents)
				for id := range altered.contents {
					altered.contents[id] = []byte("#ifdef __FORGED\n#endif\n")
				}
				options.ObservedHeaders = &altered
			}
			options.PrepareCompilerGuards = func() error {
				if failure == "preparation" {
					return want
				}
				metadata.SetCompilerGuardAnswers(answers)
				metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error { observerCalls++; return want })
				return nil
			}
			err := metadata.DiscoverVerifiedCompilerGuards(toolsets["target"], toolsets["host"], options)
			if err == nil || failure != "generated-content" && !errors.Is(err, want) {
				t.Fatalf("discovery lost real error: %v", err)
			}
			if failure == "observer" && observerCalls != 1 || failure != "observer" && observerCalls != 0 {
				t.Fatalf("invalid discovery observer calls: %d", observerCalls)
			}
		})
	}
}
