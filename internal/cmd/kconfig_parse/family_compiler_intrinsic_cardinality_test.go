package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// This is a synthetic transport-size regression, not a production timing or
// Linux-context count. More than 8192 intrinsic values must not become more
// than 8192 compiler executions when each context owns a bounded vector. Two
// identical variants retain 8194 memberships but only 4097 complete requests.
func TestFamilyCompilerIntrinsicBatchesAboveOldValueLimit(t *testing.T) {
	const contexts = 4097
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	variants := []familyPlanVariantRequest{{name: "base"}, {name: "same"}}
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	calls := []kconfig.CompilerIntrinsicCall{
		{Operator: "__has_attribute", Operand: "deprecated"},
		{Operator: "__has_builtin", Operand: "deprecated"},
	}
	for variantIndex, variant := range variants {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		for index := range contexts {
			observation := kconfig.ConfigDependencyCompilerGuardObservation{
				Scope: "target", Role: "cc", Language: "c",
				Arguments: []string{"-nostdinc", fmt.Sprintf("-DORIGINAL_CONTEXT=%d", index)},
				Calls:     calls,
			}
			if err := p.observe(observation); err != nil {
				t.Fatal(err)
			}
			// Reordered duplicates share the same context and do not consume a
			// second executable or payload budget. Caller data stays caller-owned.
			observation.Calls = slices.Clone(calls)
			slices.Reverse(observation.Calls)
			if err := p.observe(observation); err != nil {
				t.Fatal(err)
			}
		}
		if p.truncated || p.count != (variantIndex+1)*contexts || p.callCount != 2*(variantIndex+1)*contexts || len(p.active) != contexts {
			t.Fatalf("context vectors were charged as executions: truncated=%t queries=%d values=%d active=%d",
				p.truncated, p.count, p.callCount, len(p.active))
		}
		if err := p.finishVariant(variant.name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.contexts) != contexts || len(p.uniqueQueries) != contexts || len(p.plans) != 2 {
		t.Fatal("identical variants did not share their interned contexts and complete requests")
	}
	for _, variant := range p.plans {
		if variant.Plan == nil || len(variant.Plan.Nodes) != contexts {
			t.Fatalf("batched frontier did not register exactly %d executable context requests", contexts)
		}
	}
	plan, err := kconfig.MergeProbePlans(p.plans)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(plan.Toolsets, toolsets) || len(plan.Requests) != contexts || len(plan.Nodes) != contexts || len(plan.Terminal) != contexts {
		t.Fatal("different original compiler contexts were conflated")
	}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if len(request.Steps) != 1 || strings.Count(request.Steps[0].Stdin, "\n__has_attribute(") != 1 || strings.Count(request.Steps[0].Stdin, "\n__has_builtin(") != 1 {
			t.Fatal("a context request did not contain both measured calls")
		}
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || manifest.Truncated || len(manifest.Variants["base"]) != contexts || len(manifest.Variants["same"]) != contexts {
		t.Fatalf("bounded vector manifest rejected: %v", err)
	}
	values := 0
	for _, variant := range variants {
		for _, query := range manifest.Variants[variant.name] {
			if query.Kind != familyCompilerIntrinsicQueryKind || len(query.Calls) != 2 || len(query.Names) != 0 {
				t.Fatal("serialized vector lost typed call membership")
			}
			values += len(query.Calls)
		}
	}
	if values != 16388 {
		t.Fatalf("serialized %d values, want 16388 across 8194 memberships", values)
	}
	// Execute the strict replay protocol with one frozen result per merged
	// request. Both variants must reproduce that same complete union.
	input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
	if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		step := plan.Requests[node.RequestID].Steps[0].Name
		writeTestProbeResult(t, input.target, kconfig.ProbeResult{
			Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: toolsets[node.Scope], Kind: "text", Text: "0 202311\n",
			Steps: []kconfig.ProbeStepResult{{Name: step, Status: "success", Stdout: "0 202311\n"}},
		})
	}
	finalFlags, _ := familyCompilerGuardFlagsForTest(t, "replay", 1)
	final, err := newFamilyCompilerGuardPipeline(&finalFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := final.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := final.finishVariant(variant.name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if err := final.publish(); err != nil {
		t.Fatalf("identical memberships failed exact frozen union replay: %v", err)
	}
	final.prior[0].replayed[1].Plan = familyCompilerGuardPlanForTest(t)
	// One variant's result cannot be used as evidence that the other variant
	// replayed anything. Lifecycle and per-variant replay are checked above;
	// removing all contributors must additionally fail exact union validation.
	final.prior[0].replayed[0].Plan = familyCompilerGuardPlanForTest(t)
	if err := final.publish(); err == nil {
		t.Fatal("missing frozen unique requests passed final publication")
	}
}

func TestFamilyCompilerIntrinsicMembershipBudgetIsStillIndependent(t *testing.T) {
	p := &familyCompilerGuardPipeline{
		active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{},
		count: maxFamilyCompilerGuardMemberships - 1,
	}
	observation := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DFEATURE=1"},
		Calls: []kconfig.CompilerIntrinsicCall{
			{Operator: "__has_attribute", Operand: "btf_type_tag"},
			{Operator: "__has_builtin", Operand: "btf_type_tag"},
		},
	}
	if err := p.observe(observation); err != nil || p.truncated || p.count != maxFamilyCompilerGuardMemberships || p.callCount != 2 {
		t.Fatalf("last bounded context rejected: %v", err)
	}
	observation.Arguments = []string{"-DFEATURE=2"}
	if err := p.observe(observation); err != nil || !p.truncated || len(p.active) != 0 {
		t.Fatalf("interned contexts erased the raw-membership limit: %v", err)
	}
}

func TestFamilyCompilerGuardUniqueQueriesUseOnlyCompleteFinalVectors(t *testing.T) {
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	variants := []familyPlanVariantRequest{{name: "base"}, {name: "same"}, {name: "subset"}}
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	for index, variant := range variants {
		if err := p.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		value := familyCompilerIntrinsicObservationForTest()
		value.Calls = []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "deprecated"}}
		if index == 1 {
			value.Calls[0].Operand = "btf_type_tag"
		}
		before := len(p.uniqueQueries)
		if err := p.observe(value); err != nil {
			t.Fatal(err)
		}
		if index != 2 {
			if index == 0 {
				value.Calls[0].Operand = "btf_type_tag"
			} else {
				value.Calls[0].Operand = "deprecated"
			}
			for range 2 {
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(p.uniqueQueries) != before {
			t.Fatal("growing or duplicated intermediate vectors consumed complete-query budget")
		}
		if err := p.finishVariant(variant.name, scopes); err != nil {
			t.Fatal(err)
		}
		want := 1
		if index == 2 {
			want = 2
		}
		if p.truncated || len(p.uniqueQueries) != want || len(p.contexts) != 1 || p.count != index+1 {
			t.Fatal("final query identity lost vector values, order independence or separate memberships")
		}
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 2 || len(plan.Terminal) != 2 {
		t.Fatalf("same context with different final vectors did not retain two requests: %v", err)
	}
}

func TestFamilyCompilerGuardLateBudgetOverflowClearsEarlierSiblings(t *testing.T) {
	for _, budget := range []string{"memberships", "unique queries"} {
		t.Run(budget, func(t *testing.T) {
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			variants := []familyPlanVariantRequest{{name: "base"}, {name: "later"}}
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			value := familyCompilerIntrinsicObservationForTest()
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			if err := p.finishVariant("base", scopes); err != nil || len(p.plans[0].Plan.Nodes) != 1 {
				t.Fatalf("earlier sibling did not register its actual request: %v", err)
			}
			if budget == "memberships" {
				p.count = maxFamilyCompilerGuardMemberships
			} else {
				// Seed only complete canonical descriptors, not active/growing
				// vectors. Retaining a duplicate at the exact ceiling is legal.
				for index := 1; index < maxFamilyCompilerGuardQueries; index++ {
					q := familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: value.Scope, Role: value.Role, Language: value.Language, Arguments: value.Arguments,
						Calls: []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: fmt.Sprintf("prior_%04d", index)}}}
					if !p.retainQuery(q) {
						t.Fatal("exact complete-query ceiling rejected")
					}
				}
				if !p.retainQuery(p.queries["base"][0]) || len(p.uniqueQueries) != maxFamilyCompilerGuardQueries {
					t.Fatal("duplicate complete vector consumed an extra query")
				}
			}
			if err := p.prepareVariant("later", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			value.Calls = []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "new_final_vector"}}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			if budget == "unique queries" && p.truncated {
				t.Fatal("unique budget was charged before the query vector was complete")
			}
			if err := p.finishVariant("later", scopes); err != nil {
				t.Fatal(err)
			}
			wantReason, wantLimit := "query_membership_limit", maxFamilyCompilerGuardMemberships
			if budget == "unique queries" {
				wantReason, wantLimit = "unique_query_limit", maxFamilyCompilerGuardQueries
			}
			if !p.truncated || p.limitReason != wantReason || p.limitCurrent != wantLimit+1 || p.limitMax != wantLimit {
				t.Fatal("late failure lost its exact independent budget")
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || !manifest.Truncated || len(manifest.Variants["base"])+len(manifest.Variants["later"]) != 0 {
				t.Fatalf("late overflow published earlier sibling query memberships: %v", err)
			}
			plan, err := kconfig.ReadProbePlan(flags.planOut)
			if err != nil || len(plan.Nodes)+len(plan.Requests)+len(plan.Terminal) != 0 {
				t.Fatalf("late overflow published a partial executable plan: %v", err)
			}
		})
	}
}
