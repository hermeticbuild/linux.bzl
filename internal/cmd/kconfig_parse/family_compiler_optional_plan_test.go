package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestFamilyCompilerWeakHintSubsetPublication(t *testing.T) {
	for _, literal := range []bool{false, true} {
		t.Run(map[bool]string{false: "entered", true: "literal"}[literal], func(t *testing.T) {
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

func TestFamilyCompilerWeakHintSubsetKinds(t *testing.T) {
	for _, kind := range []string{familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind,
		familyCompilerCounterHintQueryKind, familyCompilerVariadicStage} {
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
