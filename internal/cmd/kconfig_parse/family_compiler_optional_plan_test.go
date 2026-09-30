package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestFamilyCompilerWeakHintSubsetPublication(t *testing.T) {
	for _, tc := range []struct{ literal, ranked bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		literal := tc.literal
		t.Run(fmt.Sprintf("literal=%t/ranked=%t", literal, tc.ranked), func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil,
				[]familyPlanVariantRequest{{name: "a"}, {name: "b"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			for _, variant := range []string{"a", "b"} {
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := p.prepareVariant(variant, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				for _, dialect := range []string{"c11", "gnu11", "c17"} {
					if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
						Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-std=" + dialect},
						Names: []string{"__FIRST", "__SECOND"}, OptionalTokenHints: true, LiteralIncludeHints: literal,
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := p.finishVariant(variant, scopes); err != nil {
					t.Fatal(err)
				}
			}
			stage := p.tokenHints
			if literal {
				stage = p.literalHints
			}
			if stage == nil || stage.plan == nil || len(stage.plan.Terminal) != 3 || len(stage.variants) != 2 {
				t.Fatal("fixture did not retain three shared query roots")
			}
			root := slices.Min(stage.plan.Terminal)
			if tc.ranked {
				root = slices.Max(stage.plan.Terminal)
				stage.consumers = &familyCompilerHintConsumers{}
				for _, variant := range stage.variants {
					for _, candidate := range variant.queries {
						if candidate.terminal == root {
							stage.consumers.observe(familyCompilerGuardSchedulingKey(candidate.query), variant.name, "consumer")
						}
					}
				}
				if stage.rootConsumerScores()[root] != 2 {
					t.Fatal("shared root multiplied its consumer score by variant membership")
				}
			}
			one, err := kconfig.SelectProbePlanTerminals(stage.plan, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			oneBytes, fits := familyCompilerGuardOptionalPlanBytes(one, maxFamilyCompilerGuardBytes/2)
			if !fits || oneBytes >= stage.planBytes {
				t.Fatal("fixture lacks a strict nonempty subset")
			}
			// Reserve all but one complete closure for stronger work. Neither
			// reservation nor partial name vectors may enter the emitted plan.
			strong := &familyCompilerGuardOptionalStage{planBytes: maxFamilyCompilerGuardBytes/2 - oneBytes}
			p.optional = strong
			if err := p.enforceOptionalStagingBudget(); err != nil {
				t.Fatal(err)
			}
			if stage.disabled || strong.disabled || stage.omitted == 0 || stage.reason != "staging_hint_subset_limit" ||
				stage.planBytes != oneBytes || !reflect.DeepEqual(stage.plan, one) || len(stage.variants) != 2 || p.truncated {
				t.Fatal("storage pressure discarded a complete fitting weak subset")
			}
			for _, variant := range stage.variants {
				if len(variant.queries) != 1 || variant.queries[0].terminal != root ||
					!slices.Equal(variant.queries[0].query.Names, []string{"__FIRST", "__SECOND"}) ||
					!slices.Equal(variant.terminals, []string{root}) {
					t.Fatal("subset selection changed vector contents or variant ownership")
				}
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || manifest.Truncated {
				t.Fatal("retained subset failed manifest publication", err)
			}
			for _, variant := range []string{"a", "b"} {
				queries := manifest.Variants[variant]
				if len(queries) != 1 || queries[0].Kind != stage.kind || !slices.Equal(queries[0].Names, []string{"__FIRST", "__SECOND"}) {
					t.Fatal("publication lost a retained complete vector")
				}
			}
			plan, err := kconfig.ReadProbePlan(flags.planOut)
			if err != nil {
				t.Fatal(err)
			}
			// Reading the directory transport materializes empty dependency
			// slices. Compare both graphs across the same serialization boundary.
			expectedPath := t.TempDir()
			if err := one.Write(expectedPath); err != nil {
				t.Fatal(err)
			}
			expected, err := kconfig.ReadProbePlan(expectedPath)
			if err != nil || !reflect.DeepEqual(plan, expected) {
				t.Fatal("published probe closure differs from the retained exact subset", err)
			}
		})
	}
}

func TestFamilyCompilerHintConsumerCounts(t *testing.T) {
	class := [32]byte{1}
	var forward, reverse familyCompilerHintConsumers
	observations := [][2]string{{"a", "one"}, {"a", "two"}, {"b", "one"}, {"a", "one"}, {"a", "two"}}
	for _, value := range observations {
		forward.observe(class, value[0], value[1])
	}
	slices.Reverse(observations)
	for _, value := range observations {
		reverse.observe(class, value[0], value[1])
	}
	if !reflect.DeepEqual(forward, reverse) || forward.counts[class] != 3 || len(forward.seen) != 3 {
		t.Fatal("consumer counts depend on observation order or duplicate visits")
	}
	// Framing prevents ambiguous variant/consumer concatenations.
	forward.observe(class, "ab", "c")
	forward.observe(class, "a", "bc")
	if forward.counts[class] != 5 {
		t.Fatal("consumer identity framing collided")
	}
	for _, tc := range []struct{ variant, consumer string }{
		{"", "c"}, {"a", ""}, {strings.Repeat("a", 257), "c"}, {"a", strings.Repeat("c", 257)},
	} {
		c := familyCompilerHintConsumers{}
		c.observe(class, "a", "valid")
		c.observe(class, tc.variant, tc.consumer)
		c.observe(class, "a", "later")
		if !c.disabled || c.counts != nil || c.seen != nil {
			t.Fatal("invalid identity retained partial scores or reenabled ranking")
		}
	}
	for _, classes := range []bool{false, true} {
		c := familyCompilerHintConsumers{}
		limit := maxFamilyCompilerGuardMemberships
		if classes {
			limit = maxFamilyCompilerGuardQueries
		}
		for n := range limit {
			key := class
			if classes {
				key = [32]byte{byte(n), byte(n >> 8)}
			}
			c.observe(key, "a", fmt.Sprint(n))
			c.observe(key, "a", fmt.Sprint(n))
		}
		if c.disabled || len(c.seen) != limit {
			t.Fatal("exact bound or duplicate consumer rejected")
		}
		c.observe([32]byte{255, 255}, "a", "overflow")
		if !c.disabled || c.counts != nil || c.seen != nil {
			t.Fatal("overflow kept order-dependent partial scores")
		}
	}
}

func TestFamilyCompilerHintConsumersBeforeNameDeduplication(t *testing.T) {
	p := &familyCompilerGuardPipeline{activeVariant: "base", known: map[string]map[string]bool{}}
	for _, consumer := range []string{"one", "two", "one"} {
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", ConsumerNodeID: consumer,
			Names: []string{"__NAME"}, OptionalTokenHints: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stage := p.tokenHints
	if stage == nil || stage.consumers.disabled || stage.ledger.nameCount != 1 || len(stage.consumers.counts) != 1 {
		t.Fatal("fixture did not deduplicate the pending name")
	}
	for _, count := range stage.consumers.counts {
		if count != 2 {
			t.Fatal("name deduplication lost distinct compile consumers")
		}
	}
	stage.disable("test")
	if stage.consumers != nil {
		t.Fatal("discarded stage retained its scheduling index")
	}
}

func TestFamilyCompilerWeakHintSubsetKinds(t *testing.T) {
	for _, kind := range []string{familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind,
		familyCompilerCounterHintQueryKind, familyCompilerVariadicStage, familyCompilerVariadicLookaheadStage} {
		if !(&familyCompilerGuardOptionalStage{kind: kind}).supportsPlanSubset() {
			t.Fatalf("weak tier %s cannot retain a subset", kind)
		}
	}
	for _, kind := range []string{"", familyCompilerOptionalDefinednessQueryKind, "unknown"} {
		if (&familyCompilerGuardOptionalStage{kind: kind}).supportsPlanSubset() {
			t.Fatalf("non-hint tier %s entered weak subset admission", kind)
		}
	}
}
