package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestFamilyCompilerVariadicDemandAndHintRoundTrip(t *testing.T) {
	for _, optional := range []bool{false, true} {
		kind := "demand"
		if optional {
			kind = "hint"
		}
		for _, rejected := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "answered", true: "rejected"}[rejected], func(t *testing.T) {
				toolsets := familyCompilerGuardPlanForTest(t).Toolsets
				flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
				variants := []familyPlanVariantRequest{{name: "base"}}
				p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
				if err != nil {
					t.Fatal(err)
				}
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				for _, syntax := range []string{"standard", "named", "standard"} {
					if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: syntax, OptionalVariadicHints: optional}); err != nil {
						t.Fatal(err)
					}
				}
				if optional {
					if p.count != 0 || len(p.active) != 0 || p.variadicHints == nil || p.variadicHints.ledger.count != 2 {
						t.Fatal("definition inventory entered the actual-demand frontier")
					}
				} else if p.count != 2 || len(p.active) != 2 || p.variadicHints != nil {
					t.Fatal("actual grammar demands lost baseline priority or deduplication")
				}
				if err := p.finishVariant("base", scopes); err != nil {
					t.Fatal(err)
				}
				if err := p.publish(); err != nil {
					t.Fatal(err)
				}
				manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
				if err != nil || len(manifest.Variants["base"]) != 2 || p.callCount != 2 {
					t.Fatal("lost admitted grammar membership or accounting", err)
				}
				plan, err := kconfig.ReadProbePlan(flags.planOut)
				if err != nil || len(plan.Nodes) != 2 {
					t.Fatal("lost separate syntax requests", err)
				}
				input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
				if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				for _, node := range plan.Nodes {
					positive := !rejected
					step := kconfig.ProbeStepResult{Name: plan.Requests[node.RequestID].Steps[0].Name, Status: "success", Stdout: "17 23"}
					if rejected {
						step.Status, step.ExitCode, step.Stdout = "failure", 1, "not a grammar fact"
					}
					writeTestProbeResult(t, input.target, kconfig.ProbeResult{Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
						Scope: node.Scope, ToolsetIdentity: toolsets[node.Scope], Kind: "boolean", Boolean: &positive, Steps: []kconfig.ProbeStepResult{step}})
				}
				nextFlags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
				next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
				if err != nil {
					t.Fatal(err)
				}
				scopes = familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := next.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				for _, syntax := range []string{"standard", "named"} {
					for _, optional := range []bool{false, true} {
						if err := next.observe(kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: syntax, OptionalVariadicHints: optional}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if next.variadicHints != nil || len(next.active) != 0 || next.count != 0 {
					t.Fatal("repeated exact answered/rejected attempt")
				}
				if err := next.finishVariant("base", scopes); err != nil {
					t.Fatal(err)
				}
				if err := next.publish(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestFamilyCompilerVariadicRejectsMixedPayload(t *testing.T) {
	p := &familyCompilerGuardPipeline{}
	for _, value := range []kconfig.ConfigDependencyCompilerGuardObservation{
		{Language: "c", OptionalVariadicHints: true},
		{Language: "c", OptionalVariadicHints: true, CounterCount: 1},
		{Language: "c", OptionalVariadicHints: true, Names: []string{"__A"}},
		{Language: "c", VariadicCommaSyntax: "invented"},
		{Language: "c", VariadicCommaSyntax: "standard", Names: []string{"__A"}},
		{Language: "c", VariadicCommaSyntax: "standard", OptionalTokenHints: true},
		{Language: "c", VariadicCommaSyntax: "standard", CounterCount: 1},
		{Language: "c", VariadicCommaSyntax: "standard", LiteralIncludeHints: true},
		{Language: "c", VariadicCommaSyntax: "standard", OptionalVariadicHints: true, LiteralIncludeHints: true, OptionalTokenHints: true},
		{Language: "assembler-with-cpp", VariadicCommaSyntax: "standard"},
	} {
		if err := p.observe(value); err == nil {
			t.Fatalf("admitted mixed grammar observation %#v", value)
		}
	}
	for _, q := range []familyCompilerGuardQuery{
		{Kind: "variadic-comma-standard", Language: "c", Names: []string{}},
		{Kind: "variadic-comma-named", Language: "c", Calls: []kconfig.CompilerIntrinsicCall{}},
		{Kind: "variadic-comma-named", Language: "c", CounterCount: 1},
	} {
		if err := q.validatePayload(); err == nil {
			t.Fatal("admitted mixed grammar payload")
		}
	}
}

func TestFamilyCompilerVariadicDemandSurvivesHintDiscard(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "named",
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "standard", OptionalVariadicHints: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	// A fully exhausted optional staging budget must not erase a query
	// required by an actually reached expansion.
	if p.variadicHints == nil || p.variadicHints.plan == nil {
		t.Fatal("fixture did not retain its independent weak grammar plan")
	}
	if err := p.variadicHints.trimOptionalHintPlan(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].Kind != "variadic-comma-named" {
		t.Fatalf("actual grammar demand was discarded with optional hints: %#v %v", manifest.Variants, err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatal("actual demand lost its executable probe", err)
	}
}

func TestFamilyCompilerVariadicDemandDeduplicatesHintInEitherOrder(t *testing.T) {
	for _, hintFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "demand first", true: "hint first"}[hintFirst], func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			for _, optional := range []bool{hintFirst, !hintFirst, hintFirst} {
				if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
					Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "named", OptionalVariadicHints: optional,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if p.count != 1 || p.callCount != 1 {
				t.Fatal("duplicate observation changed actual-demand accounting")
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || len(manifest.Variants["base"]) != 1 || p.count != 1 || p.callCount != 1 {
				t.Fatal("hint duplicated an actual demand during publication", err)
			}
		})
	}
}

func TestFamilyCompilerVariadicDemandKeepsSharedBounds(t *testing.T) {
	for _, limit := range []string{"values", "memberships", "bytes", "expanded"} {
		p := &familyCompilerGuardPipeline{}
		switch limit {
		case "values":
			p.callCount = maxFamilyCompilerIntrinsicCalls
		case "memberships":
			p.count = maxFamilyCompilerGuardMemberships
		case "bytes":
			p.bytes = maxFamilyCompilerGuardBytes / 2
		case "expanded":
			p.expandedBytes = maxFamilyCompilerGuardExpandedBytes
		}
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "named",
		}); err != nil || !p.truncated || p.active != nil {
			t.Fatalf("actual grammar demand bypassed shared %s bound: %v", limit, err)
		}
	}
}

func TestFamilyCompilerVariadicHintStagingPriority(t *testing.T) {
	for _, demandFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "grammar before inventories", true: "demand before grammar"}[demandFirst], func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
				Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "named", OptionalVariadicHints: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			grammar := p.variadicHints
			if grammar == nil || grammar.plan == nil {
				t.Fatal("missing executable grammar hint")
			}
			// Simulate a later sibling consuming the original storage cap. A
			// broad inventory cannot evict grammar; actual demands still can.
			other := &familyCompilerGuardOptionalStage{planBytes: maxFamilyCompilerGuardBytes / 2}
			if demandFirst {
				p.optional = other
			} else {
				p.tokenHints = other
			}
			if err := p.enforceOptionalStagingBudget(); err != nil {
				t.Fatal(err)
			}
			if grammar.disabled != demandFirst || other.disabled == demandFirst || p.truncated {
				t.Fatal("staging violated demand/grammar/inventory priority")
			}
			if !demandFirst {
				if err := p.publish(); err != nil {
					t.Fatal(err)
				}
				manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
				if err != nil || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].Kind != "variadic-comma-named" {
					t.Fatal("retained grammar hint lost publication", err)
				}
			}
		})
	}
}

func TestFamilyCompilerVariadicLookaheadPreservesLiteralCapacity(t *testing.T) {
	for _, pressure := range []string{"none", "publication", "staging"} {
		t.Run(pressure, func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			for _, value := range []kconfig.ConfigDependencyCompilerGuardObservation{
				{Scope: "target", Role: "cc", Language: "c", OptionalVariadicHints: true, LiteralIncludeHints: true, VariadicCommaSyntax: "named"},
				{Scope: "target", Role: "cc", Language: "c", OptionalTokenHints: true, LiteralIncludeHints: true, Names: []string{"__BINDING"}},
			} {
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if pressure == "staging" {
				// A later sibling leaves exactly the existing binding plan's
				// storage. Speculative grammar must not evict that plan.
				p.optional = &familyCompilerGuardOptionalStage{planBytes: maxFamilyCompilerGuardBytes/2 - p.literalHints.planBytes}
				if err := p.enforceOptionalStagingBudget(); err != nil {
					t.Fatal(err)
				}
				if p.literalHints.disabled {
					t.Fatal("lookahead grammar evicted literal binding probes")
				}
			}
			if pressure == "publication" {
				p.count = maxFamilyCompilerGuardMemberships - 1
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil {
				t.Fatal(err)
			}
			kinds := map[string]int{}
			for _, q := range manifest.Variants["base"] {
				kinds[q.Kind]++
			}
			wantGrammar := 0
			if pressure == "none" {
				wantGrammar = 1
			}
			if p.truncated || kinds[familyCompilerLiteralHintQueryKind] != 1 || kinds["variadic-comma-named"] != wantGrammar || len(manifest.Variants["base"]) != 1+wantGrammar {
				t.Fatalf("lookahead displaced binding probes or disappeared without pressure: %v", kinds)
			}
		})
	}
}

func TestFamilyCompilerVariadicHintPublicationPriority(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []kconfig.ConfigDependencyCompilerGuardObservation{
		{Scope: "target", Role: "cc", Language: "c", OptionalTokenHints: true, Names: []string{"__INVENTORY"}},
		{Scope: "target", Role: "cc", Language: "c", OptionalVariadicHints: true, VariadicCommaSyntax: "named"},
		{Scope: "target", Role: "cc", Language: "c", OptionalVariadicHints: true, VariadicCommaSyntax: "standard"},
		{Scope: "target", Role: "cc", Language: "c", OptionalDefinedness: true, Names: []string{"__DEMAND"}},
	} {
		if err := p.observe(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	// Leave room for one demand and one grammar hint, but not the inventory.
	// Storage and publication are separate budgets and must agree on priority.
	p.count = maxFamilyCompilerGuardMemberships - 2
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || len(manifest.Variants["base"]) != 2 || p.truncated {
		t.Fatal("optional publication exceeded its remaining capacity", err)
	}
	kinds := map[string]int{}
	for _, q := range manifest.Variants["base"] {
		kinds[q.Kind]++
	}
	if kinds[familyCompilerOptionalDefinednessQueryKind] != 1 || kinds["variadic-comma-named"]+kinds["variadic-comma-standard"] != 1 {
		t.Fatalf("publication violated demand/grammar/inventory priority: %v", kinds)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 2 {
		t.Fatal("publication lost exact executable membership", err)
	}
}

func TestFamilyCompilerVariadicLookaheadPromotionDeduplicates(t *testing.T) {
	// 0 = literal candidate, 1 = entered definition, 2 = reached expansion.
	for _, order := range [][]int{{0, 1}, {1, 0}, {0, 2}, {2, 0}, {0, 1, 2}, {2, 1, 0}} {
		toolsets := familyCompilerGuardPlanForTest(t).Toolsets
		flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
		p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}, {name: "overlay"}}, toolsets)
		if err != nil {
			t.Fatal(err)
		}
		for _, variant := range []string{"base", "overlay"} {
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			if err := p.prepareVariant(variant, scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				for _, strength := range order {
					if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
						Scope: "target", Role: "cc", Language: "c", VariadicCommaSyntax: "named",
						OptionalVariadicHints: strength != 2, LiteralIncludeHints: strength == 0,
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := p.finishVariant(variant, scopes); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.publish(); err != nil {
			t.Fatal(err)
		}
		manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
		if err != nil {
			t.Fatal(err)
		}
		if p.truncated || p.count != 2 || p.callCount != 2 || len(p.contexts) != 1 {
			t.Fatalf("promotion order %v lost deduplication or variant membership", order)
		}
		for _, variant := range []string{"base", "overlay"} {
			queries := manifest.Variants[variant]
			if len(queries) != 1 || queries[0].Kind != "variadic-comma-named" {
				t.Fatalf("order %v duplicated %s queries", order, variant)
			}
		}
		plan, err := kconfig.ReadProbePlan(flags.planOut)
		if err != nil || len(plan.Terminal) != 1 {
			t.Fatal("promotion duplicated executable grammar attempt", err)
		}
	}
}
