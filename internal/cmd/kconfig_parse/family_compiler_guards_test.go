package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func familyCompilerOptionalRoundForTest(t *testing.T, answered bool) (familyCompilerGuardRoundInput, *familyCompilerGuardManifest, kconfig.ProbeResult) {
	return familyCompilerOptionalRoundKindForTest(t, answered, false)
}

func familyCompilerOptionalRoundKindForTest(t *testing.T, answered, hints bool) (familyCompilerGuardRoundInput, *familyCompilerGuardManifest, kconfig.ProbeResult) {
	t.Helper()
	kind := familyCompilerOptionalDefinednessQueryKind
	if hints {
		kind = familyCompilerTokenHintQueryKind
	}
	return familyCompilerOptionalRoundQueryKindForTest(t, answered, kind)
}

func familyCompilerOptionalRoundQueryKindForTest(t *testing.T, answered bool, kind string) (familyCompilerGuardRoundInput, *familyCompilerGuardManifest, kconfig.ProbeResult) {
	t.Helper()
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
	observation := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DA=1", "-UA"},
		OptionalDefinedness: true, Names: []string{"__B", "__A", "__B"},
	}
	observation.OptionalDefinedness = kind == familyCompilerOptionalDefinednessQueryKind
	observation.OptionalTokenHints = kind == familyCompilerTokenHintQueryKind || kind == familyCompilerLiteralHintQueryKind
	observation.LiteralIncludeHints = kind == familyCompilerLiteralHintQueryKind
	if err := p.observe(observation); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || len(manifest.Variants["base"]) != 1 {
		t.Fatalf("optional manifest: %#v %v", manifest, err)
	}
	query := manifest.Variants["base"][0]
	if query.Kind != kind || !slices.Equal(query.Names, []string{"__A", "__B"}) {
		t.Fatalf("optional vector was not canonical or distinct: %#v", query)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("optional discovery plan: %#v %v", plan, err)
	}
	node := plan.Nodes[0]
	request := plan.Requests[node.RequestID]
	if request.Outcome.Kind != "boolean" || request.Outcome.Predicate == nil || request.Outcome.Predicate.Operator != "exit-zero" {
		t.Fatal("optional status was lowered as a mandatory text query")
	}
	input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
	if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	status, code := "failure", 1
	if answered {
		status, code = "success", 0
	}
	result := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope,
		ToolsetIdentity: toolsets[node.Scope], Kind: "boolean", Boolean: &answered,
		// Plausible failed stdout deliberately cannot become measured facts.
		Steps: []kconfig.ProbeStepResult{{Name: request.Steps[0].Name, Status: status, ExitCode: code, Stdout: "0 1\n"}},
	}
	writeTestProbeResult(t, input.target, result)
	return input, manifest, result
}

func TestFamilyCompilerOptionalDefinednessRejectsOnlyExactCompletedVector(t *testing.T) {
	for _, answered := range []bool{false, true} {
		input, manifest, _ := familyCompilerOptionalRoundForTest(t, answered)
		for _, change := range []string{"same", "singleton A", "singleton B", "larger vector", "argv", "environment", "translation units", "scope", "language", "mandatory"} {
			t.Run(fmt.Sprintf("answered=%t/%s", answered, change), func(t *testing.T) {
				flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
				p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
				if err != nil {
					t.Fatal(err)
				}
				scopes := familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
				if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				prior := manifest.Variants["base"][0]
				if p.rejected[prior.payloadKey()] != !answered || len(p.known[prior.contextKey()]) != map[bool]int{true: 2, false: 0}[answered] {
					t.Fatal("failed vector became per-name knowledge or lost its exact attempt key")
				}
				value := kconfig.ConfigDependencyCompilerGuardObservation{
					Scope: prior.Scope, Role: prior.Role, Language: prior.Language, Arguments: slices.Clone(prior.Arguments),
					OptionalDefinedness: true, Names: []string{"__B", "__A", "__B"},
				}
				switch change {
				case "singleton A":
					value.Names = []string{"__A"}
				case "singleton B":
					value.Names = []string{"__B"}
				case "larger vector":
					value.Names = append(value.Names, "__C")
				case "argv":
					value.Arguments[0], value.Arguments[1] = value.Arguments[1], value.Arguments[0]
				case "environment":
					value.Environment = map[string]string{"CONTEXT": "changed"}
				case "translation units":
					value.TranslationUnits = []string{"different.c"}
				case "scope":
					value.Scope = "host"
				case "language":
					value.Language = "assembler-with-cpp"
				case "mandatory":
					value.OptionalDefinedness = false
				}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				if err := p.finishVariant("base", scopes); err != nil {
					t.Fatal(err)
				}
				if err := p.publish(); err != nil {
					t.Fatal(err)
				}
				got, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
				if err != nil {
					t.Fatal(err)
				}
				suppressed := change == "same" || answered && (change == "singleton A" || change == "singleton B")
				if (len(got.Variants["base"]) == 0) != suppressed || got.Truncated {
					t.Fatalf("exact attempt suppression changed another vector: %#v", got.Variants)
				}
				if !suppressed && answered && change == "larger vector" && !slices.Equal(got.Variants["base"][0].Names, []string{"__C"}) {
					t.Fatal("answered facts failed to reduce the new residual vector")
				}
			})
		}
	}
}

func TestFamilyCompilerOptionalDefinednessRejectedPairThenAnsweredSingleton(t *testing.T) {
	first, manifest, _ := familyCompilerOptionalRoundForTest(t, false)
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
	variants := []familyPlanVariantRequest{{name: "base"}}
	p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{first}, variants, manifest.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	value := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DA=1", "-UA"},
		OptionalDefinedness: true, Names: []string{"__A"},
	}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("singleton discovery: %#v %v", plan, err)
	}
	second := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
	if err := os.WriteFile(filepath.Join(second.host, ".empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	node, positive := plan.Nodes[0], true
	writeTestProbeResult(t, second.target, kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: manifest.Toolsets[node.Scope], Kind: "boolean", Boolean: &positive,
		Steps: []kconfig.ProbeStepResult{{Name: plan.Requests[node.RequestID].Steps[0].Name, Status: "success", Stdout: "0"}},
	})
	nextFlags, _ := familyCompilerGuardFlagsForTest(t, "guards", 2)
	next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{first, second}, variants, manifest.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes = familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
	if err := next.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	value.Names = []string{"__A", "__B"}
	if err := next.observe(value); err != nil {
		t.Fatal(err)
	}
	if err := next.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := next.publish(); err != nil {
		t.Fatal(err)
	}
	current, _, err := readFamilyCompilerGuardManifest(nextFlags.manifestOut)
	if err != nil || len(current.Variants["base"]) != 1 || !slices.Equal(current.Variants["base"][0].Names, []string{"__B"}) {
		t.Fatalf("old failed pair suppressed a new residual vector: %#v %v", current, err)
	}
}

func TestFamilyCompilerOptionalDefinednessStrictReplayAndNoPartialPublication(t *testing.T) {
	input, manifest, original := familyCompilerOptionalRoundForTest(t, false)
	for _, failure := range []string{"toolset", "current toolset", "request", "signal", "spoof summary", "malformed success", "changed vector", "missing result"} {
		t.Run(failure, func(t *testing.T) {
			result := original
			result.Steps = slices.Clone(original.Steps)
			positive := *original.Boolean
			result.Boolean = &positive
			current := maps.Clone(manifest.Toolsets)
			changed := *manifest
			changed.Variants = map[string][]familyCompilerGuardQuery{"base": slices.Clone(manifest.Variants["base"])}
			switch failure {
			case "toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "current toolset":
				current["target"] = "sha256-" + strings.Repeat("f", 64)
			case "request":
				result.RequestID = strings.Repeat("f", 64)
			case "signal":
				result.Steps[0].ExitCode = -1
			case "spoof summary":
				positive = true
			case "malformed success":
				positive = true
				result.Steps[0].Status, result.Steps[0].ExitCode, result.Steps[0].Stdout = "success", 0, "0 invalid"
			case "changed vector":
				changed.Variants["base"][0].Names = []string{"__A"}
			case "missing result":
				input.target = t.TempDir()
			}
			familyCompilerGuardWriteManifestForTest(t, input.manifest, changed)
			if failure != "missing result" {
				writeTestProbeResult(t, input.target, result)
			}
			flags, _ := familyCompilerGuardFlagsForTest(t, "replay", 1)
			p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
			metadata := &kconfig.CompactMetadata{}
			if err == nil {
				// Exhausted/disabled fresh optional admission cannot skip strict
				// replay of an earlier attempted vector or publish partial facts.
				p.optional = &familyCompilerGuardOptionalStage{disabled: true}
				p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries).uniqueQueries
				err = p.prepareVariant("base", familyCompilerGuardCarryScopesForTest(t, current), metadata)
			}
			if err == nil || !reflect.ValueOf(metadata).Elem().IsZero() {
				t.Fatal("invalid optional attempt published complete or partial compiler facts")
			}
		})
	}
}

func TestFamilyCompilerOptionalDefinednessUsesExistingBudgetsAndKindValidation(t *testing.T) {
	value := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", OptionalDefinedness: true, Names: []string{"__A"},
	}
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}, nameCount: (1 << 20) - 1}
	if err := p.observe(value); err != nil || p.truncated || p.nameCount != (1<<20)-1 {
		t.Fatalf("optional staging spent baseline capacity: %v", err)
	}
	value.Names = []string{"__B"}
	if err := p.observe(value); err != nil || p.truncated || len(p.active) != 0 || p.optional.ledger.nameCount != 2 {
		t.Fatalf("optional staging changed baseline discovery: %v", err)
	}
	for _, calls := range [][]kconfig.CompilerIntrinsicCall{nil, {{Operator: "__has_attribute", Operand: "deprecated"}}} {
		p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}}
		value.Names, value.Calls = nil, calls
		if err := p.observe(value); err == nil {
			t.Fatal("optional expansion tag accepted empty or intrinsic-only payload")
		}
	}
}

func familyCompilerOptionalLedgerForTest(unique int) *familyCompilerGuardPipeline {
	p := &familyCompilerGuardPipeline{uniqueQueries: map[string]struct{}{}, optional: &familyCompilerGuardOptionalStage{}}
	// Synthetic occupied slots isolate admission arithmetic without creating
	// thousands of compiler plans. These keys are never written to a manifest.
	for index := range unique {
		p.uniqueQueries[fmt.Sprintf("occupied-%08d", index)] = struct{}{}
	}
	return p
}

func TestFamilyCompilerPendingPrioritySurvivesDiscardAndResets(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}, {name: "next"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	for _, variant := range []string{"base", "next"} {
		if err := p.prepareVariant(variant, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if variant == "base" {
			if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
				Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DUNIT=demand"},
				Names: []string{"__DEMAND"}, OptionalDefinedness: true,
			}); err != nil {
				t.Fatal(err)
			}
			p.optional.disable("test-discard")
			if p.optional.ledger.pendingNames != nil || len(p.priority.contexts) != 1 {
				t.Fatal("discard retained pending reservations or lost demand priority")
			}
		}
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DUNIT=weak"},
			Names: []string{"__DEMAND", "__WEAK"}, OptionalTokenHints: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(variant, scopes); err != nil {
			t.Fatal(err)
		}
		if p.priority != nil || p.optional.ledger.priority != nil || p.tokenHints.ledger.pendingNames != nil || p.tokenHints.ledger.priority != nil {
			t.Fatal("finished variant retained scheduling state")
		}
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	for variant, want := range map[string][]string{"base": {"__WEAK"}, "next": {"__DEMAND", "__WEAK"}} {
		queries := p.queries[variant]
		if len(queries) != 1 || !slices.Equal(queries[0].Names, want) {
			t.Fatalf("%s lost priority/reset: %#v, want %v", variant, queries, want)
		}
	}
}

func TestFamilyCompilerPendingPriorityIndexIsBounded(t *testing.T) {
	index := &familyCompilerGuardPriorityIndex{}
	for item := range maxFamilyCompilerGuardMemberships {
		query := familyCompilerOptionalQueryForTest(item)
		index.record(query.contextKey(), query)
	}
	if index.truncated || len(index.contexts) != maxFamilyCompilerGuardMemberships {
		t.Fatal("priority index did not retain its exact bound")
	}
	index.rejectPending(familyCompilerOptionalQueryForTest(maxFamilyCompilerGuardMemberships))
	if !index.truncated || index.contexts != nil || index.rejected != nil {
		t.Fatal("priority/rejection index exceeded its combined bound")
	}
	p := &familyCompilerGuardPipeline{priority: index, tokenHints: &familyCompilerGuardOptionalStage{}}
	p.filterTokenHintDemands()
	if !p.tokenHints.disabled || p.tokenHints.reason != "priority_context_limit" || p.truncated || p.optional != nil {
		t.Fatal("priority-index overflow affected stronger discovery")
	}
}

func TestFamilyCompilerPendingRejectedVectorDoesNotReserveSingleton(t *testing.T) {
	for _, hints := range []bool{false, true} {
		t.Run(fmt.Sprintf("hints=%t", hints), func(t *testing.T) {
			input, manifest, _ := familyCompilerOptionalRoundKindForTest(t, false, hints)
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
			p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			prior := manifest.Variants["base"][0]
			if !p.rejected[prior.payloadKey()] || len(p.known[prior.contextKey()]) != 0 {
				t.Fatal("prior rejected vector became per-name knowledge")
			}
			value := kconfig.ConfigDependencyCompilerGuardObservation{
				Scope: prior.Scope, Role: prior.Role, Language: prior.Language,
				Arguments: slices.Clone(prior.Arguments), Names: slices.Clone(prior.Names),
				OptionalDefinedness: !hints, OptionalTokenHints: hints,
			}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			value.Arguments = slices.Clone(value.Arguments)
			value.Arguments[0] = "-DA=2"
			value.Names = []string{"__A"}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			queries := p.queries["base"]
			if len(queries) != 1 || !slices.Equal(queries[0].Names, []string{"__A"}) || !slices.Equal(queries[0].Arguments, value.Arguments) {
				t.Fatalf("rejected larger vector suppressed the equivalent-context singleton: %#v", queries)
			}
		})
	}
}

func TestFamilyCompilerPendingDefinednessEquivalentContexts(t *testing.T) {
	for _, hints := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, overlap := range []string{"identical", "overlapping", "disjoint"} {
				t.Run(fmt.Sprintf("hints=%t/reverse=%t/%s", hints, reverse, overlap), func(t *testing.T) {
					toolsets := familyCompilerGuardPlanForTest(t).Toolsets
					flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
					p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}, {name: "next"}}, toolsets)
					if err != nil {
						t.Fatal(err)
					}
					scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
					left, right := []string{"__SHARED"}, []string{"__SHARED"}
					if overlap == "overlapping" {
						left, right = []string{"__LEFT", "__SHARED"}, []string{"__RIGHT", "__SHARED"}
					} else if overlap == "disjoint" {
						left, right = []string{"__LEFT"}, []string{"__RIGHT"}
					}
					observations := []kconfig.ConfigDependencyCompilerGuardObservation{
						{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DPENDING_CONTEXT=left"}, Names: left, OptionalDefinedness: !hints, OptionalTokenHints: hints},
						{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DPENDING_CONTEXT=right"}, Names: right, OptionalDefinedness: !hints, OptionalTokenHints: hints},
					}
					if reverse {
						slices.Reverse(observations)
					}
					want := map[string][]string{}
					pending := map[string]bool{}
					for _, value := range observations {
						for _, name := range value.Names {
							if !pending[name] {
								want[value.Arguments[0]] = append(want[value.Arguments[0]], name)
								pending[name] = true
							}
						}
					}
					// A fresh variant must not inherit another variant's pending
					// names as if they were measured compiler answers.
					for _, variant := range []string{"base", "next"} {
						if err := p.prepareVariant(variant, scopes, &kconfig.CompactMetadata{}); err != nil {
							t.Fatal(err)
						}
						for _, value := range observations {
							before := slices.Clone(value.Names)
							if err := p.observe(value); err != nil {
								t.Fatal(err)
							}
							if !slices.Equal(before, value.Names) {
								t.Fatal("pending admission mutated the source observer's names")
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
					for _, variant := range []string{"base", "next"} {
						got := map[string][]string{}
						for _, query := range manifest.Variants[variant] {
							if len(query.Arguments) != 1 || query.Scope != "target" || query.Role != "cc" || query.Language != "c" {
								t.Fatal("pending selection invented a compiler context")
							}
							got[query.Arguments[0]] = query.Names
						}
						if !reflect.DeepEqual(got, want) || manifest.Truncated {
							t.Fatalf("%s pending vectors = %v, want original residual vectors %v", variant, got, want)
						}
					}
				})
			}
		}
	}
}

func TestFamilyCompilerTokenHintPriorityEquivalentContexts(t *testing.T) {
	for _, weakFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("weak-first=%t", weakFirst), func(t *testing.T) {
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
			values := []kconfig.ConfigDependencyCompilerGuardObservation{
				{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DPENDING_CONTEXT=core"}, Names: []string{"__CORE"}},
				{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DPENDING_CONTEXT=demand"}, Names: []string{"__DEMAND"}, OptionalDefinedness: true},
				{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DPENDING_CONTEXT=weak"}, Names: []string{"__CORE", "__DEMAND", "__WEAK"}, OptionalTokenHints: true},
			}
			if weakFirst {
				slices.Reverse(values)
			}
			for _, value := range values {
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]string{}
			for _, query := range manifest.Variants["base"] {
				got[query.Kind] = query.Names
			}
			want := map[string][]string{"": {"__CORE"}, familyCompilerOptionalDefinednessQueryKind: {"__DEMAND"}, familyCompilerTokenHintQueryKind: {"__WEAK"}}
			if !reflect.DeepEqual(got, want) || manifest.Truncated {
				t.Fatalf("equivalent-context priority = %v, want %v", got, want)
			}
		})
	}
}

func TestFamilyCompilerTokenHintsKeepThreeIndependentVectors(t *testing.T) {
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
	value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", OptionalTokenHints: true, Names: []string{"__CORE", "__DEMAND", "__WEAK"}}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	value.OptionalTokenHints, value.Names = false, []string{"__CORE"}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	value.OptionalDefinedness, value.Names = true, []string{"__DEMAND"}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, query := range manifest.Variants["base"] {
		got[query.Kind] = query.Names
	}
	want := map[string][]string{"": {"__CORE"}, familyCompilerOptionalDefinednessQueryKind: {"__DEMAND"}, familyCompilerTokenHintQueryKind: {"__WEAK"}}
	if !reflect.DeepEqual(got, want) || manifest.Truncated {
		t.Fatalf("query tiers mixed: %#v", got)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 3 {
		t.Fatalf("partitioned executable plan: %#v %v", plan, err)
	}
}

func TestFamilyCompilerOptionalStagingSharesPlansAcrossVariants(t *testing.T) {
	for _, kind := range []string{familyCompilerOptionalDefinednessQueryKind, familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind} {
		t.Run(kind, func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "b"}, {name: "c"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			firstBytes := 0
			for index, name := range []string{"a", "b", "c"} {
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				value := kconfig.ConfigDependencyCompilerGuardObservation{
					Scope: "target", Role: "cc", Language: "c", Names: []string{"__SHARED"},
					OptionalDefinedness: kind == familyCompilerOptionalDefinednessQueryKind,
					OptionalTokenHints:  kind != familyCompilerOptionalDefinednessQueryKind,
					LiteralIncludeHints: kind == familyCompilerLiteralHintQueryKind,
				}
				if name == "c" {
					value.Arguments = []string{"-DDISTINCT_CONTEXT=1"}
				}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				if err := p.finishVariant(name, scopes); err != nil {
					t.Fatal(err)
				}
				stage := p.optional
				if kind == familyCompilerTokenHintQueryKind {
					stage = p.tokenHints
				} else if kind == familyCompilerLiteralHintQueryKind {
					stage = p.literalHints
				}
				wantNodes := 1
				if name == "c" {
					wantNodes = 2
				}
				if stage == nil || stage.disabled || stage.planNodes != wantNodes || len(stage.variants) != index+1 {
					t.Fatalf("%s duplicated retained probe storage or lost variant bindings: %#v", name, stage)
				}
				if name == "a" {
					firstBytes = stage.planBytes
				} else if name == "b" && stage.planBytes != firstBytes || name == "c" && stage.planBytes <= firstBytes {
					t.Fatalf("%s charged the wrong unique plan storage: %d, first %d", name, stage.planBytes, firstBytes)
				}
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			plan, err := kconfig.ReadProbePlan(flags.planOut)
			if err != nil || len(plan.Nodes) != 2 || len(plan.Terminal) != 2 {
				t.Fatalf("shared staging changed the emitted union: %#v %v", plan, err)
			}
			for _, variant := range p.plans {
				if len(variant.Plan.Nodes) != 1 || len(variant.Plan.Terminal) != 1 || len(p.queries[variant.Name]) != 1 {
					t.Fatalf("%s inherited another variant's terminal", variant.Name)
				}
			}
		})
	}
}

func TestFamilyCompilerOptionalSharedStoragePreservesPrivateDependencies(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "b"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for index, name := range []string{"a", "b"} {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		options, err := scopes.Options("target", kconfig.KbuildOptions{})
		if err != nil {
			t.Fatal(err)
		}
		environment := map[string]string{}
		private := []string{"LC_ALL=C /configured/target/cc --version", "/configured/target/cc --version | head -n 1"}
		for key, command := range map[string]string{"COMMON": "/configured/target/cc --version", "OWN": private[index]} {
			symbol, err := options.Shell(command)
			if err != nil || symbol == "" {
				t.Fatalf("register dependency: %q %v", symbol, err)
			}
			environment[key] = symbol
		}
		// Build an independent, unshared reference closure for this variant.
		batch, err := kconfig.NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = batch.OptionalCompilerDefinednessAttempt("target", "cc", "c", nil, nil, []string{"__OPTIONAL"}, environment)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := batch.Plan()
		if err != nil || len(plan.Nodes) != 3 {
			t.Fatalf("expected shared dependency, private dependency and terminal: %#v %v", plan, err)
		}
		expected[name], err = familyCompilerGuardPlanID(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", OptionalDefinedness: true,
			Names: []string{"__OPTIONAL"}, Environment: environment,
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if p.optional.planNodes != 5 || len(p.optional.plan.Nodes) != 5 {
		t.Fatal("overlapping dependency was retained twice")
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	for _, variant := range p.plans {
		id, err := familyCompilerGuardPlanID(variant.Plan)
		if err != nil || id != expected[variant.Name] {
			t.Fatalf("%s changed its exact unshared dependency closure: %s, want %s: %v", variant.Name, id, expected[variant.Name], err)
		}
	}
}

func TestFamilyCompilerOptionalSharedStorageAtCapacity(t *testing.T) {
	for _, limit := range []string{"bytes", "nodes"} {
		t.Run(limit, func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "b"}, {name: "c"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b", "c"} {
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__BASELINE"}}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				value.OptionalDefinedness, value.Names = true, []string{"__OPTIONAL"}
				if name == "c" {
					value.Arguments = []string{"-DDISTINCT_CONTEXT=1"}
				}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				if err := p.finishVariant(name, scopes); err != nil {
					t.Fatal(err)
				}
				if name == "a" {
					// Reserve all remaining capacity, as in the existing limit tests.
					if limit == "bytes" {
						p.optional.planBytes = maxFamilyCompilerGuardBytes / 2
					} else {
						p.optional.planNodes = maxFamilyCompilerGuardMemberships
					}
				} else if name == "b" {
					if p.optional.disabled || len(p.optional.variants) != 2 || len(p.optional.plan.Nodes) != 1 {
						t.Fatal("duplicate closure consumed capacity or lost variant membership")
					}
				} else if !p.optional.disabled || p.optional.plan != nil || len(p.optional.variants) != 0 || p.optional.planBytes != 0 || p.optional.planNodes != 0 {
					t.Fatal("new unique closure overflow did not release all retained optional plans")
				}
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || manifest.Truncated {
				t.Fatalf("optional overflow damaged baseline: %v", err)
			}
			for _, name := range []string{"a", "b", "c"} {
				if len(manifest.Variants[name]) != 1 || manifest.Variants[name][0].Kind != "" {
					t.Fatalf("%s lost mandatory work or retained partial optional work", name)
				}
			}
		})
	}
}

func TestFamilyCompilerOptionalSharedStorageRejectsForeignVariantTerminal(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "b"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", OptionalDefinedness: true,
			Names: []string{"__" + strings.ToUpper(name)},
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if p.optional.plan == nil || len(p.optional.plan.Nodes) != 2 {
		t.Fatal("fixture did not retain distinct terminals in shared storage")
	}
	p.optional.variants[0].queries[0].terminal = p.optional.variants[1].queries[0].terminal
	if err := p.publish(); err == nil || !strings.Contains(err.Error(), "outside its original variant") {
		t.Fatalf("shared storage granted foreign terminal membership: %v", err)
	}
	if _, err := os.Stat(flags.manifestOut); !os.IsNotExist(err) {
		t.Fatalf("invalid membership published a manifest: %v", err)
	}
}

func TestFamilyCompilerLookaheadKeepsFourIndependentVectors(t *testing.T) {
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
	// Observe weakest first, so stronger observations must subtract their names
	// before any vector is frozen into an executable compiler request.
	value := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c", OptionalTokenHints: true,
		LiteralIncludeHints: true, Names: []string{"__CORE", "__DEMAND", "__ENTERED", "__LITERAL"},
	}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	value.LiteralIncludeHints, value.Names = false, []string{"__CORE", "__DEMAND", "__ENTERED"}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	value.OptionalTokenHints, value.Names = false, []string{"__CORE"}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	value.OptionalDefinedness, value.Names = true, []string{"__DEMAND"}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, query := range manifest.Variants["base"] {
		if _, duplicate := got[query.Kind]; duplicate {
			t.Fatalf("duplicate vector for the same kind and compiler context: %#v", query)
		}
		got[query.Kind] = query.Names
	}
	want := map[string][]string{
		"": {"__CORE"}, familyCompilerOptionalDefinednessQueryKind: {"__DEMAND"},
		familyCompilerTokenHintQueryKind: {"__ENTERED"}, familyCompilerLiteralHintQueryKind: {"__LITERAL"},
	}
	if !reflect.DeepEqual(got, want) || manifest.Truncated {
		t.Fatalf("query tiers mixed: %#v, want %#v", got, want)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 4 {
		t.Fatalf("partitioned executable plan: %#v %v", plan, err)
	}
}

func TestFamilyCompilerLookaheadStagingBudgetProtectsStrongerTiers(t *testing.T) {
	for _, dimension := range []string{"bytes", "nodes"} {
		t.Run(dimension, func(t *testing.T) {
			p := &familyCompilerGuardPipeline{
				optional: &familyCompilerGuardOptionalStage{}, tokenHints: &familyCompilerGuardOptionalStage{},
				literalHints: &familyCompilerGuardOptionalStage{},
			}
			// Bookkeeping reservations only: no fake plan is emitted. Literal work
			// fits exactly until a later variant adds stronger entered-file work.
			if dimension == "bytes" {
				p.optional.planBytes = 1
				p.tokenHints.planBytes = 1
				p.literalHints.planBytes = maxFamilyCompilerGuardBytes/2 - 2
			} else {
				p.optional.planNodes = 1
				p.tokenHints.planNodes = 1
				p.literalHints.planNodes = maxFamilyCompilerGuardMemberships - 2
			}
			p.enforceOptionalStagingBudget()
			if p.optional.disabled || p.tokenHints.disabled || p.literalHints.disabled {
				t.Fatal("exact shared boundary discarded a tier")
			}
			if dimension == "bytes" {
				p.tokenHints.planBytes++
			} else {
				p.tokenHints.planNodes++
			}
			p.enforceOptionalStagingBudget()
			if p.optional.disabled || p.tokenHints.disabled || !p.literalHints.disabled ||
				p.literalHints.reason != "staging_plan_limit" || p.literalHints.planBytes != 0 || p.literalHints.planNodes != 0 {
				t.Fatalf("shared limit failed to discard only speculative work: %#v", p)
			}
		})
	}
}

func TestFamilyCompilerLookaheadCannotDisplaceLaterEnteredHint(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "z"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "z"} {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		value := kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: "target", Role: "cc", Language: "c", OptionalTokenHints: true,
			LiteralIncludeHints: name == "a", Names: []string{"__ENTERED_HINT"},
		}
		if value.LiteralIncludeHints {
			value.Names = []string{"__LOOKAHEAD_HINT"}
		}
		if err := p.observe(value); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	// Reserve the existing frontier, leaving exactly one complete descriptor.
	// These ledger reservations never enter the emitted manifest or plan.
	p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries - 1).uniqueQueries
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	if len(p.queries["a"]) != 0 || len(p.queries["z"]) != 1 || p.truncated {
		t.Fatalf("lookahead displaced an entered-file hint: %#v", p.queries)
	}
	if got := p.queries["z"][0]; got.Kind != familyCompilerTokenHintQueryKind || !slices.Equal(got.Names, []string{"__ENTERED_HINT"}) {
		t.Fatalf("entered-file query lost its original tier or names: %#v", got)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("retained hint lacks its exact executable plan: %#v %v", plan, err)
	}
}

func TestFamilyCompilerTokenHintsCannotDisplaceLaterVariantDemand(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "z"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "z"} {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__UNKNOWN"}, OptionalTokenHints: name == "a", OptionalDefinedness: name == "z"}
		if err := p.observe(value); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the existing frontier occupying all but one descriptor slot.
	// No fake requests enter the emitted manifest or plan.
	p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries - 1).uniqueQueries
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	if len(p.queries["a"]) != 0 || len(p.queries["z"]) != 1 || p.queries["z"][0].Kind != familyCompilerOptionalDefinednessQueryKind || p.truncated {
		t.Fatalf("early weaker hint displaced later demand: %#v", p.queries)
	}
}

func TestFamilyCompilerRejectedLiteralHintsDoNotSuppressStrongerQueries(t *testing.T) {
	for _, entered := range []bool{false, true} {
		t.Run(fmt.Sprintf("entered=%t", entered), func(t *testing.T) {
			input, manifest, _ := familyCompilerOptionalRoundQueryKindForTest(t, false, familyCompilerLiteralHintQueryKind)
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
			p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
			if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			prior := manifest.Variants["base"][0]
			if !p.rejected[prior.payloadKey()] || len(p.known[prior.contextKey()]) != 0 {
				t.Fatal("failed literal vector granted facts")
			}
			value := kconfig.ConfigDependencyCompilerGuardObservation{
				Scope: prior.Scope, Role: prior.Role, Language: prior.Language, Arguments: prior.Arguments,
				Names: slices.Clone(prior.Names), OptionalDefinedness: !entered, OptionalTokenHints: entered,
			}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			wantKind := familyCompilerOptionalDefinednessQueryKind
			if entered {
				wantKind = familyCompilerTokenHintQueryKind
			}
			queries := p.queries["base"]
			if len(queries) != 1 || queries[0].Kind != wantKind || !slices.Equal(queries[0].Names, prior.Names) {
				t.Fatalf("failed literal hint poisoned a stronger attempt: %#v", queries)
			}
		})
	}
}

func TestFamilyCompilerRejectedTokenHintsDoNotAnswerOrSuppressDemand(t *testing.T) {
	input, manifest, _ := familyCompilerOptionalRoundKindForTest(t, false, true)
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
	p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, manifest.Toolsets)
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	prior := manifest.Variants["base"][0]
	if !p.rejected[prior.payloadKey()] || len(p.known[prior.contextKey()]) != 0 {
		t.Fatal("failed hinted vector granted facts")
	}
	value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: prior.Scope, Role: prior.Role, Language: prior.Language, Arguments: prior.Arguments, Names: []string{"__A"}, OptionalDefinedness: true}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	queries := p.queries["base"]
	if len(queries) != 1 || queries[0].Kind != familyCompilerOptionalDefinednessQueryKind || !slices.Equal(queries[0].Names, []string{"__A"}) {
		t.Fatalf("failed hint poisoned demand: %#v", queries)
	}
}

func familyCompilerOptionalQueryForTest(index int) familyCompilerGuardQuery {
	return familyCompilerGuardQuery{Kind: familyCompilerOptionalDefinednessQueryKind, Scope: "target", Role: "cc", Language: "c",
		Arguments: []string{fmt.Sprintf("-DCASE_%04d=1", index)}, Names: []string{"__UNKNOWN"}}
}

func TestFamilyCompilerOptionalAdmission958DoesNotDiscard7235BaselineQueries(t *testing.T) {
	p := familyCompilerOptionalLedgerForTest(7235)
	p.count, p.nameCount, p.bytes, p.expandedBytes = 9578, 176110, 30230129, 67094521
	admitted := 0
	for index := range 958 {
		_, accepted, err := p.admitOptionalQuery(familyCompilerOptionalQueryForTest(index))
		if err != nil {
			t.Fatal(err)
		}
		if accepted {
			admitted++
		}
	}
	if admitted != 957 || p.truncated || len(p.uniqueQueries) != maxFamilyCompilerGuardQueries ||
		p.count != 9578+957 || p.nameCount != 176110+957 || p.optional.omitted != 1 || p.optional.reason != "unique_query_limit" {
		t.Fatalf("optional overflow displaced baseline queries: admitted=%d counts=%d/%d reason=%s", admitted, p.count, len(p.uniqueQueries), p.optional.reason)
	}
	for index := range 7235 {
		if _, found := p.uniqueQueries[fmt.Sprintf("occupied-%08d", index)]; !found {
			t.Fatal("a baseline query was evicted")
		}
	}
}

func TestFamilyCompilerOptionalAdmissionBoundsAreAtomic(t *testing.T) {
	for _, test := range []struct {
		reason    string
		configure func(*familyCompilerGuardPipeline, *familyCompilerGuardQuery)
	}{
		{"query_membership_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) {
			p.count = maxFamilyCompilerGuardMemberships
		}},
		{"name_value_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) { p.nameCount = 1 << 20 }},
		{"intrinsic_value_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) {
			p.callCount = maxFamilyCompilerIntrinsicCalls + 1
		}},
		{"estimated_bytes_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) {
			p.bytes = maxFamilyCompilerGuardBytes / 2
		}},
		{"expanded_bytes_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) {
			p.expandedBytes = maxFamilyCompilerGuardExpandedBytes
		}},
		{"context_limit", func(p *familyCompilerGuardPipeline, _ *familyCompilerGuardQuery) {
			p.contexts = map[string]familyCompilerGuardContext{}
			for index := range maxFamilyCompilerGuardQueries {
				p.contexts[fmt.Sprint(index)] = familyCompilerGuardContext{}
			}
		}},
		{"query_name_limit", func(_ *familyCompilerGuardPipeline, q *familyCompilerGuardQuery) {
			q.Names = nil
			for index := range 4097 {
				q.Names = append(q.Names, fmt.Sprintf("__NAME_%04d", index))
			}
		}},
		{"query_stdin_limit", func(_ *familyCompilerGuardPipeline, q *familyCompilerGuardQuery) {
			q.Names = nil
			for index := range 4096 {
				q.Names = append(q.Names, fmt.Sprintf("__%04d_%s", index, strings.Repeat("x", 256)))
			}
		}},
	} {
		t.Run(test.reason, func(t *testing.T) {
			p, query := familyCompilerOptionalLedgerForTest(0), familyCompilerOptionalQueryForTest(0)
			test.configure(p, &query)
			state := func() [7]int {
				return [7]int{p.count, p.bytes, p.expandedBytes, p.nameCount, p.callCount, len(p.contexts), len(p.uniqueQueries)}
			}
			before := state()
			if _, admitted, err := p.admitOptionalQuery(query); err != nil || admitted || p.truncated || state() != before || p.optional.reason != test.reason {
				t.Fatalf("optional limit mutated the baseline: admitted=%t err=%v before=%v after=%v reason=%s", admitted, err, before, state(), p.optional.reason)
			}
		})
	}
	// A full intrinsic value budget does not block a Names-only query: it
	// spends zero Calls and cannot enlarge or truncate the existing call set.
	p := familyCompilerOptionalLedgerForTest(0)
	p.callCount = maxFamilyCompilerIntrinsicCalls
	if _, admitted, err := p.admitOptionalQuery(familyCompilerOptionalQueryForTest(0)); err != nil || !admitted || p.callCount != maxFamilyCompilerIntrinsicCalls {
		t.Fatalf("optional query changed intrinsic accounting: %t %v", admitted, err)
	}
}

func TestFamilyCompilerOptionalAdmissionMatchesBaselineAccounting(t *testing.T) {
	query := familyCompilerOptionalQueryForTest(0)
	query.Names = []string{"__A", "__B"}
	p := familyCompilerOptionalLedgerForTest(0)
	baseline := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}}
	for range 2 {
		if _, admitted, err := p.admitOptionalQuery(query); err != nil || !admitted {
			t.Fatalf("admission: %t %v", admitted, err)
		}
		baseline.active, baseline.known = map[string]*familyCompilerGuardQuery{}, map[string]map[string]bool{}
		// This reference loop models separate variants, as prepareVariant does.
		baseline.pendingNames = nil
		if err := baseline.observeDefinedness(kconfig.ConfigDependencyCompilerGuardObservation{
			Scope: query.Scope, Role: query.Role, Language: query.Language, Arguments: query.Arguments,
			OptionalDefinedness: true, Names: query.Names,
		}); err != nil {
			t.Fatal(err)
		}
		if !baseline.retainQuery(query) {
			t.Fatal("unexpected baseline limit")
		}
	}
	if p.bytes != baseline.bytes || p.expandedBytes != baseline.expandedBytes || p.count != 2 || p.nameCount != 4 ||
		p.count != baseline.count || p.nameCount != baseline.nameCount || len(p.contexts) != 1 || len(p.uniqueQueries) != 1 {
		t.Fatalf("shared descriptors discounted membership work: optional=%d/%d baseline=%d/%d", p.bytes, p.expandedBytes, baseline.bytes, baseline.expandedBytes)
	}
}

func TestFamilyCompilerOptionalAdmissionIsGlobalAcrossVariants(t *testing.T) {
	for _, order := range [][]string{{"a", "z"}, {"z", "a"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "z"}}, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries - 1).uniqueQueries
			var log bytes.Buffer
			p.diagnostics = &log
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			var baselineID string
			for _, name := range order {
				if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__OPTIONAL"}, OptionalDefinedness: true}
				if name == "z" {
					value.OptionalDefinedness, value.Names = false, []string{"__BASELINE"}
				}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				if err := p.finishVariant(name, scopes); err != nil {
					t.Fatal(err)
				}
				if name == "z" {
					baselineID, err = familyCompilerGuardPlanID(p.plans[len(p.plans)-1].Plan)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if p.truncated || len(p.queries["a"]) != 0 || len(p.queries["z"]) != 1 || len(p.optional.variants) != 1 {
				t.Fatal("optional discovery ran admission before the global baseline completed")
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := kconfig.ReadProbePlan(flags.planOut)
			if err != nil {
				t.Fatal(err)
			}
			id, err := familyCompilerGuardPlanID(plan)
			if err != nil || id != baselineID || len(plan.Nodes) != 1 || len(plan.Requests) != 1 || manifest.Truncated ||
				len(manifest.Variants["a"]) != 0 || !slices.Equal(manifest.Variants["z"][0].Names, []string{"__BASELINE"}) ||
				!strings.Contains(log.String(), "compiler_guard_optional") || strings.Contains(log.String(), "event=limit") {
				t.Fatalf("later baseline query or its exact DAG was displaced: %v %s", err, log.String())
			}
		})
	}
}

func TestFamilyCompilerOptionalAdmissionSelectsOnlyEmittedTerminals(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries - 1).uniqueQueries
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		query := familyCompilerOptionalQueryForTest(index)
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{Scope: query.Scope, Role: query.Role, Language: query.Language,
			Arguments: query.Arguments, Names: query.Names, OptionalDefinedness: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	if len(p.optional.variants) != 1 || len(p.optional.plan.Nodes) != 2 {
		t.Fatal("fixture did not stage both requests")
	}
	want := p.optional.variants[0].queries[0]
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || manifest.Truncated || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].payloadKey() != want.query.payloadKey() ||
		len(plan.Nodes) != 1 || len(plan.Requests) != 1 || !slices.Equal(plan.Terminal, []string{want.terminal}) {
		t.Fatalf("omitted terminal/request leaked into the frozen plan: %#v %v", plan, err)
	}
}

func TestFamilyCompilerOptionalSharedVectorRetainsBothMemberships(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "a"}, {name: "b"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	for _, name := range p.names {
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__SHARED"}, OptionalDefinedness: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
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
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 || len(plan.Terminal) != 1 || len(plan.Requests) != 1 ||
		len(manifest.Variants["a"]) != 1 || len(manifest.Variants["b"]) != 1 || p.count != 2 || p.nameCount != 2 || len(p.uniqueQueries) != 1 || len(p.contexts) != 1 {
		t.Fatalf("shared optional vector lost membership or executed duplicate DAGs: %#v %v", plan, err)
	}
}

func TestFamilyCompilerOptionalOmissionPreservesSharedBaselineDependencies(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	p.uniqueQueries = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries - 2).uniqueQueries
	scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
	options, err := scopes.Options("target", kconfig.KbuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var symbols, dependencies []string
	for _, command := range []string{"/configured/target/cc --version", "LC_ALL=C /configured/target/cc --version", "/configured/target/cc --version | head -n 1"} {
		previous := scopes.References()
		symbol, err := options.Shell(command)
		if err != nil {
			t.Fatal(err)
		}
		current := scopes.References()
		var added []string
		for _, reference := range current {
			if !slices.ContainsFunc(previous, func(old kconfig.ProbeReference) bool { return old.NodeID == reference.NodeID }) {
				added = append(added, reference.NodeID)
			}
		}
		if len(added) != 1 || symbol == "" {
			t.Fatal("fixture did not register a distinct authenticated symbolic dependency")
		}
		symbols, dependencies = append(symbols, symbol), append(dependencies, added[0])
	}
	if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
		t.Fatal(err)
	}
	baseline := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__BASELINE"}, Environment: map[string]string{"COMMON": symbols[0]}}
	if err := p.observe(baseline); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		value := baseline
		value.OptionalDefinedness, value.Names = true, []string{"__OPTIONAL"}
		value.Environment = map[string]string{"COMMON": symbols[0], "OWN": symbols[index+1]}
		if err := p.observe(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.finishVariant("base", scopes); err != nil {
		t.Fatal(err)
	}
	staged := p.optional.variants[0]
	if len(p.optional.plan.Nodes) != 5 || len(staged.queries) != 2 || len(p.plans[0].Plan.Nodes) != 2 {
		t.Fatal("fixture did not retain shared and private dependency closure")
	}
	first, excluded := staged.queries[0], staged.queries[1]
	excludedPrivate := dependencies[1]
	if excluded.query.Environment["OWN"] == symbols[2] {
		excludedPrivate = dependencies[2]
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, node := range plan.Nodes {
		ids[node.ID] = true
	}
	if len(plan.Nodes) != 4 || len(plan.Requests) != 4 || len(plan.Terminal) != 2 ||
		!ids[dependencies[0]] || !ids[first.terminal] || ids[excluded.terminal] || ids[excludedPrivate] {
		t.Fatalf("omission lost shared mandatory input or retained dead optional DAG: %#v", ids)
	}
}

func TestFamilyCompilerOptionalRetainedPlanBudget(t *testing.T) {
	plan := familyCompilerGuardPlanForTest(t, "one", "two")
	size, fits := familyCompilerGuardOptionalPlanBytes(plan, maxFamilyCompilerGuardBytes/2)
	if !fits || size == 0 {
		t.Fatal("small plan did not fit staging")
	}
	if exact, fits := familyCompilerGuardOptionalPlanBytes(plan, size); !fits || exact != size {
		t.Fatal("exact staging budget changed")
	}
	if _, fits := familyCompilerGuardOptionalPlanBytes(plan, size-1); fits {
		t.Fatal("staging ignored its byte ceiling")
	}
	data, err := json.Marshal(plan)
	if err != nil || size < len(data) {
		t.Fatalf("staging estimate undercounts retained plan: %d < %d %v", size, len(data), err)
	}
}

func TestFamilyCompilerOptionalStageLimitsKeepBaseline(t *testing.T) {
	for _, kind := range []string{"source", "descriptor", "plan bytes", "plan nodes"} {
		t.Run(kind, func(t *testing.T) {
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
			value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", OptionalDefinedness: true, Names: []string{"__OPTIONAL"}}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source":
				value.Names, value.Truncated = nil, true
			case "descriptor":
				p.optional.ledger.bytes = maxFamilyCompilerGuardBytes / 2
				value.Names = []string{"__SECOND"}
			case "plan bytes":
				p.optional.planBytes = maxFamilyCompilerGuardBytes / 2
			case "plan nodes":
				p.optional.planNodes = maxFamilyCompilerGuardMemberships
			}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			value.OptionalDefinedness, value.Truncated, value.Names = false, false, []string{"__BASELINE"}
			if err := p.observe(value); err != nil {
				t.Fatal(err)
			}
			if err := p.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if p.truncated || !p.optional.disabled || len(p.optional.variants) != 0 || len(p.optional.ledger.contexts) != 0 {
				t.Fatal("optional storage overflow retained partial state or truncated baseline")
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || manifest.Truncated || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].Kind != "" {
				t.Fatalf("baseline lost after optional %s overflow: %#v %v", kind, manifest, err)
			}
		})
	}
}

func familyCompilerGuardFlagsForTest(t *testing.T, mode string, rounds int) (familyCompilerGuardFlags, *familyExecutionRequest) {
	t.Helper()
	root := t.TempDir()
	f := familyCompilerGuardFlags{}
	if mode == "guards" {
		f.manifestOut, f.planOut = filepath.Join(root, "new-manifest.json"), filepath.Join(root, "new-plan")
	}
	for index := range rounds {
		name := strconv.Itoa(index)
		for kind, values := range map[string]*namedPathFlag{
			"manifest": &f.manifests, "plan": &f.plans, "host": &f.hosts, "target": &f.targets,
		} {
			*values = append(*values, namedPath{Name: name, Path: filepath.Join(root, "prior", name, kind)})
		}
	}
	return f, &familyExecutionRequest{mode: mode, cutIn: filepath.Join(root, "initial-cut.json")}
}

func TestFamilyCompilerGuardFlagsCompleteSequentialRounds(t *testing.T) {
	for _, mode := range []string{"guards", "replay"} {
		maximum := maxFamilyCompilerGuardRounds
		if mode == "guards" {
			maximum--
		}
		for count := 0; count <= maximum; count++ {
			f, execution := familyCompilerGuardFlagsForTest(t, mode, count)
			inputs, err := f.validate(execution)
			if err != nil || len(inputs) != count {
				t.Fatalf("%s with %d prior rounds = %#v/%v", mode, count, inputs, err)
			}
			for index, input := range inputs {
				if input.manifest != f.manifests[index].Path || input.plan != f.plans[index].Path ||
					input.host != f.hosts[index].Path || input.target != f.targets[index].Path {
					t.Fatalf("round %d lost its complete input quartet: %#v", index, input)
				}
			}
		}
		f, execution := familyCompilerGuardFlagsForTest(t, mode, maximum+1)
		if _, err := f.validate(execution); err == nil {
			t.Fatalf("%s accepted an unbounded extra round", mode)
		}
	}
	for _, field := range []string{"manifest", "plan", "host", "target"} {
		for _, failure := range []string{"missing", "duplicate", "gap", "empty"} {
			t.Run(field+"/"+failure, func(t *testing.T) {
				f, execution := familyCompilerGuardFlagsForTest(t, "guards", 2)
				values := map[string]*namedPathFlag{"manifest": &f.manifests, "plan": &f.plans, "host": &f.hosts, "target": &f.targets}[field]
				switch failure {
				case "missing":
					*values = (*values)[:1]
				case "duplicate":
					(*values)[1].Name = "0"
				case "gap":
					(*values)[1].Name = "2"
				case "empty":
					(*values)[1].Path = " "
				}
				if _, err := f.validate(execution); err == nil {
					t.Fatal("accepted an incomplete or nonsequential prior quartet")
				}
			})
		}
	}
}

func TestFamilyCompilerGuardFlagsModeOutputsAndOverlap(t *testing.T) {
	for _, change := range []string{"missing manifest", "missing plan", "replay outputs", "initial", "no execution", "same outputs", "nested outputs", "cut overlap", "prior overlap", "result directory overlap"} {
		t.Run(change, func(t *testing.T) {
			f, execution := familyCompilerGuardFlagsForTest(t, "guards", 1)
			switch change {
			case "missing manifest":
				f.manifestOut = ""
			case "missing plan":
				f.planOut = ""
			case "replay outputs":
				execution.mode = "replay"
			case "initial":
				execution.mode = "initial"
			case "no execution":
				execution = nil
			case "same outputs":
				f.planOut = f.manifestOut
			case "nested outputs":
				f.manifestOut = filepath.Join(f.planOut, "manifest.json")
			case "cut overlap":
				f.manifestOut = execution.cutIn
			case "prior overlap":
				f.planOut = f.plans[0].Path
			case "result directory overlap":
				f.manifestOut = filepath.Join(f.targets[0].Path, "manifest.json")
			}
			if _, err := f.validate(execution); err == nil {
				t.Fatal("invalid guard phase or overlapping immutable input accepted")
			}
		})
	}
	var disabled familyCompilerGuardFlags
	if disabled.requested() {
		t.Fatal("zero flags requested guard work")
	}
	if inputs, err := disabled.validate(nil); err != nil || inputs != nil {
		t.Fatalf("disabled guards changed an ordinary invocation: %#v/%v", inputs, err)
	}
	if _, err := disabled.validate(&familyExecutionRequest{mode: "guards"}); err == nil {
		t.Fatal("guard mode accepted no outputs")
	}
}

func TestFamilyCompilerGuardFlagsRegistrationRejectsDuplicateAndEmptyOutputs(t *testing.T) {
	for _, arguments := range [][]string{
		{"-family_compiler_guard_manifest_out="},
		{"-family_compiler_guard_plan_out= "},
		{"-family_compiler_guard_manifest_out=a", "-family_compiler_guard_manifest_out=b"},
		{"-family_compiler_guard_plan_out=a", "-family_compiler_guard_plan_out=b"},
	} {
		flags := flag.NewFlagSet("guard-test", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		var guards familyCompilerGuardFlags
		guards.register(flags)
		if err := flags.Parse(arguments); err == nil {
			t.Fatalf("invalid guard output flags accepted: %q", arguments)
		}
	}
}

func familyCompilerGuardPlanForTest(t *testing.T, sources ...string) *kconfig.ProbePlan {
	t.Helper()
	builder, err := kconfig.NewProbePlanBuilder("sha256-"+strings.Repeat("1", 64), "sha256-"+strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	var references []kconfig.ProbeReference
	for _, source := range sources {
		reference, err := builder.Request("target", kconfig.ProbeRequest{
			Schema:  kconfig.LinuxProbeRequestSchema,
			Steps:   []kconfig.ProbeStep{{Name: "guard", Tool: "cc", Arguments: []string{"-E", "-P", "-x", "c", "-"}, Stdin: source}},
			Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "guard", Stream: "stdout", RequireSuccess: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		references = append(references, reference)
	}
	plan, err := builder.Plan(references...)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func familyCompilerGuardWriteManifestForTest(t *testing.T, filename string, manifest familyCompilerGuardManifest) string {
	t.Helper()
	data, err := marshalFamilyCompilerGuardManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func familyCompilerGuardRoundForTest(t *testing.T, round int, previous []string) (familyCompilerGuardRoundInput, familyCompilerGuardManifest, string) {
	t.Helper()
	root := t.TempDir()
	input := familyCompilerGuardRoundInput{
		manifest: filepath.Join(root, "manifest.json"), plan: filepath.Join(root, "plan"),
		host: filepath.Join(root, "host"), target: filepath.Join(root, "target"),
	}
	plan := familyCompilerGuardPlanForTest(t)
	if err := plan.Write(input.plan); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{input.host, input.target} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, ".empty"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, err := familyCompilerGuardPlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	manifest := familyCompilerGuardManifest{
		Schema: familyCompilerGuardSchema, Round: round, Previous: slices.Clone(previous),
		Toolsets: maps.Clone(plan.Toolsets), Variants: map[string][]familyCompilerGuardQuery{"base": nil}, PlanID: id,
	}
	digest := familyCompilerGuardWriteManifestForTest(t, input.manifest, manifest)
	return input, manifest, digest
}

func TestFamilyCompilerGuardManifestCanonicalRead(t *testing.T) {
	input, manifest, wantDigest := familyCompilerGuardRoundForTest(t, 0, nil)
	got, digest, err := readFamilyCompilerGuardManifest(input.manifest)
	if err != nil || digest != wantDigest || !reflect.DeepEqual(got, &manifest) {
		t.Fatalf("canonical manifest = %#v/%s/%v", got, digest, err)
	}
	canonical, err := os.ReadFile(input.manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, malformed := range map[string][]byte{
		"leading whitespace": append([]byte(" "), canonical...),
		"missing newline":    slices.Clone(canonical[:len(canonical)-1]),
		"trailing JSON":      append(slices.Clone(canonical), []byte("{}\n")...),
		"unknown field":      []byte(strings.Replace(string(canonical), "{", "{\"Unexpected\":1,", 1)),
		"duplicate field":    []byte(strings.Replace(string(canonical), "{", "{\"Round\":0,", 1)),
		"malformed JSON":     []byte("{\n"),
	} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(filename, malformed, 0o644); err != nil {
				t.Fatal(err)
			}
			if got, digest, err := readFamilyCompilerGuardManifest(filename); err == nil || got != nil || digest != "" {
				t.Fatalf("noncanonical manifest accepted: %#v/%q/%v", got, digest, err)
			}
		})
	}
}

func TestFamilyCompilerGuardManifestChainAndMembership(t *testing.T) {
	first, manifest, firstID := familyCompilerGuardRoundForTest(t, 0, nil)
	second, secondManifest, secondID := familyCompilerGuardRoundForTest(t, 1, []string{firstID})
	f, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	variants := []familyPlanVariantRequest{{name: "base"}}
	inputs := []familyCompilerGuardRoundInput{first, second}
	pipeline, err := newFamilyCompilerGuardPipeline(&f, inputs, variants, manifest.Toolsets)
	if err != nil || !slices.Equal(pipeline.previous, []string{firstID, secondID}) || len(pipeline.prior) != 2 {
		t.Fatalf("valid frozen chain = %#v/%v", pipeline, err)
	}
	for _, change := range []string{"schema", "round", "previous", "toolsets", "variants", "plan", "duplicate names", "unsorted names", "duplicate contexts", "truncated prefix"} {
		t.Run(change, func(t *testing.T) {
			altered := secondManifest
			altered.Toolsets = maps.Clone(secondManifest.Toolsets)
			altered.Variants = maps.Clone(secondManifest.Variants)
			query := familyCompilerGuardQuery{Scope: "target", Role: "cc", Language: "c", Names: []string{"__A"}}
			switch change {
			case "schema":
				altered.Schema = "different"
			case "round":
				altered.Round++
			case "previous":
				altered.Previous = []string{strings.Repeat("0", 64)}
			case "toolsets":
				delete(altered.Toolsets, "host")
			case "variants":
				altered.Variants["unknown"] = nil
			case "plan":
				altered.PlanID = strings.Repeat("0", 64)
			case "duplicate names":
				query.Names = []string{"__A", "__A"}
				altered.Variants["base"] = []familyCompilerGuardQuery{query}
			case "unsorted names":
				query.Names = []string{"__B", "__A"}
				altered.Variants["base"] = []familyCompilerGuardQuery{query}
			case "duplicate contexts":
				altered.Variants["base"] = []familyCompilerGuardQuery{query, query}
			case "truncated prefix":
				altered.Truncated = true
				altered.Variants["base"] = []familyCompilerGuardQuery{query}
			}
			filename := filepath.Join(t.TempDir(), "altered.json")
			familyCompilerGuardWriteManifestForTest(t, filename, altered)
			input := second
			input.manifest = filename
			if got, err := newFamilyCompilerGuardPipeline(&f, []familyCompilerGuardRoundInput{first, input}, variants, manifest.Toolsets); got != nil || err == nil {
				t.Fatalf("changed %s accepted: %#v/%v", change, got, err)
			}
		})
	}
}

func TestFamilyCompilerGuardObservationDeduplicatesPriorNamesByExactContext(t *testing.T) {
	query := familyCompilerGuardQuery{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DFEATURE=1", "-UFEATURE"}}
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{
		query.contextKey(): {"__PRIOR": true},
	}}
	observation := kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: query.Scope, Role: query.Role, Language: query.Language, Arguments: query.Arguments,
		Names: []string{"__PRIOR", "__SECOND", "__FIRST", "__SECOND"},
	}
	for range 2 {
		if err := p.observe(observation); err != nil {
			t.Fatal(err)
		}
	}
	if p.count != 1 || len(p.active) != 1 || !slices.Equal(slices.Sorted(slices.Values(p.active[query.contextKey()].Names)), []string{"__FIRST", "__SECOND"}) {
		t.Fatalf("same-context observations duplicated prior/new names: %#v", p.active)
	}
	observation.Arguments = []string{"-UFEATURE", "-DFEATURE=1"}
	observation.Names = []string{"__PRIOR"}
	if err := p.observe(observation); err != nil || p.count != 2 {
		t.Fatalf("changed compiler context incorrectly filtered a prior name: %#v/%v", p.active, err)
	}
}

func TestFamilyCompilerGuardObservationTruncationDiscardsWholePublishedFrontier(t *testing.T) {
	for _, mode := range []string{"explicit", "queries", "bytes", "names", "aggregate names", "stdin"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			empty := familyCompilerGuardPlanForTest(t)
			populated := familyCompilerGuardPlanForTest(t, "earlier sibling")
			var diagnostics bytes.Buffer
			p := &familyCompilerGuardPipeline{
				flags: &f, names: []string{"base", "later"}, toolsets: maps.Clone(empty.Toolsets),
				queries: map[string][]familyCompilerGuardQuery{"base": {{Scope: "target", Role: "cc", Language: "c", Names: []string{"__EARLIER"}}}},
				plans:   []kconfig.ProbePlanVariant{{Name: "base", Plan: populated}},
				active:  map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}, finished: map[string]bool{"base": true, "later": true},
			}
			p.diagnostics = &diagnostics
			value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__NEW"}}
			switch mode {
			case "explicit":
				value.Truncated = true
			case "queries":
				p.count = maxFamilyCompilerGuardMemberships
			case "bytes":
				p.bytes = maxFamilyCompilerGuardBytes / 2
			case "names":
				value.Names = nil
				for index := range 4097 {
					value.Names = append(value.Names, fmt.Sprintf("__NAME_%04d", index))
				}
			case "aggregate names":
				p.nameCount = 1 << 20
			case "stdin":
				key := (familyCompilerGuardQuery{Scope: value.Scope, Role: value.Role, Language: value.Language}).contextKey()
				p.queryBytes = map[string]int{key: kconfig.MaxProbeInterpolatedBytes}
			}
			if err := p.observe(value); err != nil || !p.truncated || len(p.active) != 0 {
				t.Fatalf("overflow retained a partial active frontier: %#v/%v", p, err)
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			wantSummary := fmt.Sprintf("compiler_guard_round round=0 event=published reason=%s current=%d limit=%d query_memberships=%d unique_queries=%d contexts=%d names=%d calls=%d estimated_bytes=%d expanded_bytes=%d finished_variants=2 variants=2\n",
				p.limitReason, p.limitCurrent, p.limitMax, p.count, len(p.uniqueQueries), len(p.contexts), p.nameCount, p.callCount, p.bytes, p.expandedBytes)
			if !strings.HasSuffix(diagnostics.String(), wantSummary) || strings.Count(diagnostics.String(), "\n") != 2 {
				t.Fatal("truncated round lost its first-limit counters at publication")
			}
			manifest, _, err := readFamilyCompilerGuardManifest(f.manifestOut)
			if err != nil || !manifest.Truncated || len(manifest.Variants) != 2 || len(manifest.Variants["base"])+len(manifest.Variants["later"]) != 0 {
				t.Fatalf("overflow retained an earlier sibling's queries: %#v/%v", manifest, err)
			}
			plan, err := kconfig.ReadProbePlan(f.planOut)
			if err != nil || len(plan.Nodes) != 0 || len(plan.Requests) != 0 || len(plan.Terminal) != 0 {
				t.Fatalf("overflow published a partial executable plan: %#v/%v", plan, err)
			}
		})
	}
}

func TestFamilyCompilerGuardJSONPreflightStructuralLimitsAndAliases(t *testing.T) {
	for _, field := range []string{"Contexts", "contexts", "cOnTeXtS"} {
		t.Run("contexts/"+field, func(t *testing.T) {
			// Repeated keys still consume structural allocation budget before
			// JSON decoding can collapse them into one map entry.
			prefix := fmt.Sprintf("{%q:{", field)
			exact := []byte(prefix + strings.Repeat(`"same":{},`, maxFamilyCompilerGuardQueries-1) + `"same":{}}}`)
			if err := preflightFamilyCompilerGuardJSON(exact); err != nil {
				t.Fatalf("exact context allocation limit rejected: %v", err)
			}
			overflow := []byte(prefix + strings.Repeat(`"same":{},`, maxFamilyCompilerGuardQueries) + `"same":{}}}`)
			if err := preflightFamilyCompilerGuardJSON(overflow); err == nil {
				t.Fatal("repeated context keys bypassed the allocation bound")
			}
		})
	}
	for _, alias := range []string{"Contexts", "contexts"} {
		payload := []byte(`{"Contexts":{` + strings.Repeat(`"same":{},`, maxFamilyCompilerGuardQueries-1) +
			`"same":{}},` + fmt.Sprintf("%q", alias) + `:{"extra":{}}}`)
		if err := preflightFamilyCompilerGuardJSON(payload); err == nil {
			t.Fatal("a duplicate or aliased context table reset the structural budget")
		}
	}
	for _, field := range []string{"Variants", "variants", "VaRiAnTs"} {
		t.Run("queries/"+field, func(t *testing.T) {
			prefix := fmt.Sprintf("{%q:{\"base\":[", field)
			exact := []byte(prefix + strings.Repeat("{},", maxFamilyCompilerGuardMemberships-1) + "{}]}}")
			if err := preflightFamilyCompilerGuardJSON(exact); err != nil {
				t.Fatalf("exact query limit rejected before typed validation: %v", err)
			}
			overflow := []byte(prefix + strings.Repeat("{},", maxFamilyCompilerGuardMemberships) + "{}]}}")
			if err := preflightFamilyCompilerGuardJSON(overflow); err == nil || !strings.Contains(err.Error(), "query/name budget") {
				t.Fatalf("case-folded query array bypassed pre-decode limit: %v", err)
			}
		})
	}
	for _, field := range []string{"Names", "names"} {
		t.Run("names/"+field, func(t *testing.T) {
			// Keep this in memory: tiny names exercise the allocation bound with
			// only about 3 MiB of JSON, well below the manifest byte budget.
			payload := []byte(fmt.Sprintf("{\"Variants\":{\"base\":[{%q:[", field) +
				strings.Repeat("\"\",", 1<<20) + "\"\"]}]}}")
			if err := preflightFamilyCompilerGuardJSON(payload); err == nil || !strings.Contains(err.Error(), "query/name budget") {
				t.Fatalf("case-folded name array bypassed pre-decode limit: %v", err)
			}
		})
	}
	for _, depth := range []int{32, 33} {
		payload := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
		if err := preflightFamilyCompilerGuardJSON(payload); (err == nil) != (depth == 32) {
			t.Fatalf("nesting depth %d validity = %t: %v", depth, err == nil, err)
		}
	}
	for _, payload := range []string{"{", "[}", "{} {}", "{} garbage", "{\"Variants\":]"} {
		if err := preflightFamilyCompilerGuardJSON([]byte(payload)); err == nil {
			t.Fatalf("malformed or trailing JSON accepted: %q", payload)
		}
	}
}

func TestFamilyCompilerGuardInternedManifestRejectsContextAliasesAndForgery(t *testing.T) {
	_, manifest, _ := familyCompilerGuardRoundForTest(t, 0, nil)
	manifest.Variants["base"] = []familyCompilerGuardQuery{{
		Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DFEATURE=7"}, Names: []string{"__ONE"},
	}}
	canonical, err := marshalFamilyCompilerGuardManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &wire); err != nil {
		t.Fatal(err)
	}
	var contexts map[string]json.RawMessage
	if err := json.Unmarshal(wire["Contexts"], &contexts); err != nil || len(contexts) != 1 {
		t.Fatalf("expected one interned context: %v", err)
	}
	id := slices.Collect(maps.Keys(contexts))[0]
	context := string(contexts[id])
	contextPrefix := `"Contexts":{`
	for name, malformed := range map[string]string{
		"duplicate context key":     strings.Replace(string(canonical), contextPrefix, contextPrefix+fmt.Sprintf("%q:%s,", id, context), 1),
		"duplicate context table":   `{"Contexts":` + string(wire["Contexts"]) + "," + string(canonical[1:]),
		"aliased context table":     `{"contexts":` + string(wire["Contexts"]) + "," + string(canonical[1:]),
		"aliased context field":     strings.Replace(string(canonical), `"Scope":`, `"scope":`, 1),
		"changed context identity":  strings.ReplaceAll(string(canonical), id, strings.Repeat("0", 64)),
		"missing context reference": strings.Replace(string(canonical), `"Context":"`+id+`"`, `"Context":"`+strings.Repeat("1", 64)+`"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if malformed == string(canonical) {
				t.Fatal("fixture did not alter the context transport")
			}
			filename := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(filename, []byte(malformed+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, digest, err := readFamilyCompilerGuardManifest(filename); err == nil || got != nil || digest != "" {
				t.Fatal("noncanonical or forged context transport accepted")
			}
		})
	}
}

func TestFamilyCompilerGuardCompactPriorRejectsExcessExpandedWorkBeforeReplay(t *testing.T) {
	input, manifest, _ := familyCompilerGuardRoundForTest(t, 0, nil)
	query := familyCompilerGuardQuery{Scope: "target", Role: "cc", Language: "c",
		Arguments: []string{"-DVALUE=" + strings.Repeat("a", 64<<10)}, Names: []string{"__VALUE_0000"}}
	oneExpanded := familyCompilerGuardExpandedQueryBytes(query)
	count := maxFamilyCompilerGuardExpandedBytes/oneExpanded + 1
	if count >= maxFamilyCompilerGuardQueries {
		t.Fatal("fixture would hit the unique-query bound before expansion")
	}
	manifest.Variants = map[string][]familyCompilerGuardQuery{}
	variants := make([]familyPlanVariantRequest, count)
	for index := range count {
		name := fmt.Sprintf("variant_%04d", index)
		variants[index] = familyPlanVariantRequest{name: name}
		member := query
		member.Names = []string{fmt.Sprintf("__VALUE_%04d", index)}
		if familyCompilerGuardExpandedQueryBytes(member) != oneExpanded {
			t.Fatal("fixture changed its per-membership expansion")
		}
		manifest.Variants[name] = []familyCompilerGuardQuery{member}
	}
	familyCompilerGuardWriteManifestForTest(t, input.manifest, manifest)
	contents, err := os.ReadFile(input.manifest)
	if err != nil || len(contents) > 1<<20 {
		t.Fatalf("fixture was not a small context-interned transport: %v", err)
	}
	if _, _, err := readFamilyCompilerGuardManifest(input.manifest); err != nil {
		t.Fatalf("fixture failed structural or canonical validation before admission: %v", err)
	}
	// No plan or oracle can be loaded from these paths. Admission must reject
	// expansion first, despite the compact bytes and every other bound fitting.
	input.plan = filepath.Join(t.TempDir(), "absent-plan")
	input.host = filepath.Join(t.TempDir(), "absent-host")
	input.target = filepath.Join(t.TempDir(), "absent-target")
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
	if p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, variants, manifest.Toolsets); p != nil || err == nil || !strings.Contains(err.Error(), "over-budget frontier") {
		t.Fatalf("small wire bypassed expanded-work admission or reached replay: %v", err)
	}
}

func TestFamilyCompilerGuardManifestRejectsNonregularAndMalformedFields(t *testing.T) {
	if manifest, digest, err := readFamilyCompilerGuardManifest(t.TempDir()); err == nil || manifest != nil || digest != "" {
		t.Fatalf("directory accepted as a frozen manifest: %#v/%q/%v", manifest, digest, err)
	}
	for _, payload := range []string{
		"null\n", "[]\n", "{\"Round\":\"0\"}\n",
		"{\"Variants\":{\"base\":[{\"Names\":[1]}]}}\n",
		"{\"Variants\":{\"base\":[{\"Names\":{}}]}}\n",
		"{\"Variants\":{\"base\":[null]}}\n",
	} {
		filename := filepath.Join(t.TempDir(), "malformed.json")
		if err := os.WriteFile(filename, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		if manifest, digest, err := readFamilyCompilerGuardManifest(filename); err == nil || manifest != nil || digest != "" {
			t.Fatalf("malformed typed manifest accepted: %#v/%q/%v (%s)", manifest, digest, err, payload)
		}
	}
}

func TestFamilyCompilerGuardObservationAggregateAndStdinExactBoundaries(t *testing.T) {
	for _, mode := range []string{"aggregate names", "stdin"} {
		t.Run(mode, func(t *testing.T) {
			value := kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Names: []string{"__FIRST"}}
			key := (familyCompilerGuardQuery{Scope: value.Scope, Role: value.Role, Language: value.Language}).contextKey()
			p := &familyCompilerGuardPipeline{
				active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}, queryBytes: map[string]int{},
			}
			if mode == "aggregate names" {
				p.nameCount = (1 << 20) - 1
			} else {
				p.queryBytes[key] = kconfig.MaxProbeInterpolatedBytes - len("__FIRST") - 64
			}
			if err := p.observe(value); err != nil || p.truncated || len(p.active) != 1 {
				t.Fatalf("exact %s limit rejected: %#v/%v", mode, p, err)
			}
			beforeNames, beforeBytes := p.nameCount, p.queryBytes[key]
			if err := p.observe(value); err != nil || p.truncated || p.nameCount != beforeNames || p.queryBytes[key] != beforeBytes {
				t.Fatalf("duplicate name consumed %s budget: %#v/%v", mode, p, err)
			}
			value.Names = []string{"__SECOND"}
			if err := p.observe(value); err != nil || !p.truncated || len(p.active) != 0 {
				t.Fatalf("%s overflow retained partial queries: %#v/%v", mode, p, err)
			}
		})
	}
}

func TestFamilyCompilerGuardVariantLifecycleIsSequential(t *testing.T) {
	p := &familyCompilerGuardPipeline{
		flags: &familyCompilerGuardFlags{}, names: []string{"base", "other"},
		prepared: map[string]bool{}, finished: map[string]bool{},
	}
	metadata := &kconfig.CompactMetadata{}
	if err := p.prepareVariant("unknown", nil, metadata); err == nil {
		t.Fatal("unknown variant started a guard round")
	}
	if err := p.prepareVariant("base", nil, metadata); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"base", "other"} {
		if err := p.prepareVariant(name, nil, metadata); err == nil {
			t.Fatal("overlapping variant preparation replaced active context")
		}
	}
	if err := p.finishVariant("other", nil); err == nil {
		t.Fatal("wrong variant finished an active context")
	}
	if err := p.finishVariant("base", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("base", nil); err == nil {
		t.Fatal("variant finished twice")
	}
	if err := p.prepareVariant("other", nil, metadata); err != nil {
		t.Fatal(err)
	}
	if err := p.finishVariant("other", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		t.Fatalf("completed replay-only variant lifecycle failed: %v", err)
	}
}

func TestFamilyCompilerGuardPublishRequiresExactFrozenPlanUnion(t *testing.T) {
	empty := familyCompilerGuardPlanForTest(t)
	first := familyCompilerGuardPlanForTest(t, "first")
	second := familyCompilerGuardPlanForTest(t, "second")
	union, err := kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{{Name: "base", Plan: first}, {Name: "other", Plan: second}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		frozen   *kconfig.ProbePlan
		replayed []kconfig.ProbePlanVariant
		valid    bool
	}{
		{"exact", union, []kconfig.ProbePlanVariant{{Name: "base", Plan: first}, {Name: "other", Plan: second}}, true},
		{"empty", empty, []kconfig.ProbePlanVariant{{Name: "base", Plan: empty}}, true},
		{"missing node", union, []kconfig.ProbePlanVariant{{Name: "base", Plan: first}}, false},
		{"extra node", first, []kconfig.ProbePlanVariant{{Name: "base", Plan: union}}, false},
		{"wrong node", first, []kconfig.ProbePlanVariant{{Name: "base", Plan: second}}, false},
		{"nonempty becomes empty", first, []kconfig.ProbePlanVariant{{Name: "base", Plan: empty}}, false},
		{"empty becomes nonempty", empty, []kconfig.ProbePlanVariant{{Name: "base", Plan: first}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, err := familyCompilerGuardPlanID(test.frozen)
			if err != nil {
				t.Fatal(err)
			}
			p := &familyCompilerGuardPipeline{
				flags: &familyCompilerGuardFlags{}, names: []string{"base"}, finished: map[string]bool{"base": true},
				prior: []familyCompilerGuardPriorRound{{manifest: &familyCompilerGuardManifest{PlanID: id}, replayed: test.replayed}},
			}
			if err := p.publish(); (err == nil) != test.valid {
				t.Fatalf("exact frozen union validity = %t, want %t: %v", err == nil, test.valid, err)
			}
		})
	}
	p := &familyCompilerGuardPipeline{flags: &familyCompilerGuardFlags{}, names: []string{"base"}, finished: map[string]bool{}}
	if err := p.publish(); err == nil {
		t.Fatal("incomplete variant scan published its round")
	}
}

func TestFamilyCompilerGuardLimitDiagnosticsPreserveBoundaries(t *testing.T) {
	for _, intrinsic := range []bool{false, true} {
		for _, budget := range []string{"queries", "contexts", "values", "bytes", "expanded bytes", "per query", "stdin"} {
			t.Run(fmt.Sprintf("intrinsic=%t/%s", intrinsic, budget), func(t *testing.T) {
				var output bytes.Buffer
				p := &familyCompilerGuardPipeline{
					active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{},
					queryBytes: map[string]int{}, diagnostics: &output,
				}
				value := kconfig.ConfigDependencyCompilerGuardObservation{
					Scope: "target", Role: "cc", Language: "c",
					Arguments: []string{"-DSENSITIVE_ARGUMENT=1"}, TranslationUnits: []string{"SENSITIVE_SOURCE.c"},
					Environment: map[string]string{"SENSITIVE_ENV": "SENSITIVE_VALUE"}, Names: []string{"__SENSITIVE_NAME"},
				}
				query := familyCompilerGuardQuery{
					Scope: value.Scope, Role: value.Role, Language: value.Language,
					Arguments: value.Arguments, TranslationUnits: value.TranslationUnits, Environment: value.Environment,
				}
				if intrinsic {
					value.Names = nil
					value.Calls = []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "SENSITIVE_OPERAND"}}
					query.Kind = familyCompilerIntrinsicQueryKind
				}
				key := query.contextKey()
				contextJSON, err := json.Marshal(query.compilerContext())
				if err != nil {
					t.Fatal(err)
				}
				itemBytes, stdinBytes := len("__SENSITIVE_NAME")+4, len("__SENSITIVE_NAME")+64
				if intrinsic {
					itemBytes = len(value.Calls[0].Operator) + len(value.Calls[0].Operand) + 48
					stdinBytes = familyCompilerIntrinsicStdinBytes(value.Calls[0])
				}
				reason, maximum := "", 0
				switch budget {
				case "queries":
					reason, maximum = "query_membership_limit", maxFamilyCompilerGuardMemberships
					p.count = maximum - 1
				case "contexts":
					reason, maximum = "context_limit", maxFamilyCompilerGuardQueries
					p.contexts = map[string]familyCompilerGuardContext{}
					for index := range maximum - 1 {
						prior := query.compilerContext()
						prior.Arguments = []string{fmt.Sprintf("-DPRIOR_CONTEXT=%d", index)}
						p.contexts[prior.id()] = prior
					}
				case "values":
					reason, maximum = "name_value_limit", 1<<20
					p.nameCount = maximum - 1
					if intrinsic {
						reason, maximum = "intrinsic_value_limit", maxFamilyCompilerIntrinsicCalls
						p.nameCount, p.callCount = 0, maximum-1
					}
				case "bytes":
					reason, maximum = "estimated_bytes_limit", maxFamilyCompilerGuardBytes/2
					p.bytes = maximum - len(contextJSON) - 64 - 4 - familyCompilerGuardQueryReferenceBytes - itemBytes
				case "expanded bytes":
					reason, maximum = "expanded_bytes_limit", maxFamilyCompilerGuardExpandedBytes
					p.expandedBytes = maximum - len(contextJSON) - familyCompilerGuardQueryReferenceBytes - itemBytes
				case "per query":
					reason, maximum = "query_name_limit", 4096
					p.active[key] = &query
					p.known[key] = map[string]bool{}
					p.count = 1
					for index := range maximum - 1 {
						name := fmt.Sprintf("__PREVIOUS_%04d", index)
						if intrinsic {
							call := kconfig.CompilerIntrinsicCall{Operator: "__has_attribute", Operand: name}
							query.Calls = append(query.Calls, call)
							p.known[key][familyCompilerIntrinsicCallKey(call)] = true
						} else {
							query.Names = append(query.Names, name)
							p.known[key][name] = true
						}
					}
					p.nameCount = maximum - 1
					if intrinsic {
						reason = "query_call_limit"
						p.nameCount, p.callCount = 0, maximum-1
					}
				case "stdin":
					reason, maximum = "query_stdin_limit", kconfig.MaxProbeInterpolatedBytes
					p.queryBytes[key] = maximum - stdinBytes
				}
				if err := p.observe(value); err != nil || p.truncated || output.Len() != 0 {
					t.Fatalf("exact boundary rejected or reported as overflow: %v", err)
				}
				before := []int{p.count, p.nameCount, p.callCount, p.bytes, p.expandedBytes, p.queryBytes[key]}
				if err := p.observe(value); err != nil || p.truncated || output.Len() != 0 ||
					!slices.Equal(before, []int{p.count, p.nameCount, p.callCount, p.bytes, p.expandedBytes, p.queryBytes[key]}) {
					t.Fatalf("duplicate observation consumed budget or emitted diagnostics: %v", err)
				}
				if intrinsic {
					value.Calls = []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "SENSITIVE_ANOTHER"}}
				} else {
					value.Names = []string{"__SENSITIVE_NEXT"}
				}
				if budget == "queries" || budget == "contexts" {
					value.Arguments = []string{"-DSENSITIVE_OTHER=1"}
				}
				current := maximum + 1
				if budget == "bytes" || budget == "expanded bytes" {
					current = maximum + itemBytes
				} else if budget == "stdin" {
					current = maximum + stdinBytes
				}
				if err := p.observe(value); err != nil || !p.truncated || p.active != nil {
					t.Fatalf("overflow did not discard the entire active frontier: %v", err)
				}
				if p.limitReason != reason || p.limitCurrent != current || p.limitMax != maximum {
					t.Fatalf("first limit = %s/%d/%d, want %s/%d/%d",
						p.limitReason, p.limitCurrent, p.limitMax, reason, current, maximum)
				}
				want := fmt.Sprintf("compiler_guard_round round=0 event=limit reason=%s current=%d limit=%d query_memberships=%d unique_queries=%d contexts=%d names=%d calls=%d estimated_bytes=%d expanded_bytes=%d finished_variants=0 variants=0\n",
					reason, current, maximum, p.count, len(p.uniqueQueries), len(p.contexts), p.nameCount, p.callCount, p.bytes, p.expandedBytes)
				if output.String() != want || output.Len() > 512 || strings.Contains(output.String(), "SENSITIVE") {
					t.Fatal("limit diagnostics are incomplete, unbounded or expose an input")
				}
				before = []int{p.count, p.nameCount, p.callCount, p.bytes, p.expandedBytes}
				if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{Truncated: true}); err != nil ||
					output.String() != want || p.limitReason != reason ||
					!slices.Equal(before, []int{p.count, p.nameCount, p.callCount, p.bytes, p.expandedBytes}) {
					t.Fatal("a later failure changed the first reason, counters or diagnostics")
				}
			})
		}
	}
}

type familyCompilerGuardDiagnosticErrorWriter struct{}

func (familyCompilerGuardDiagnosticErrorWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("diagnostic writer unavailable")
}

func TestFamilyCompilerGuardRoundDiagnosticsDoNotChangeCanonicalOutputs(t *testing.T) {
	for _, mode := range []string{"published", "source limit", "carried_empty"} {
		t.Run(mode, func(t *testing.T) {
			var inputs []familyCompilerGuardRoundInput
			if mode == "carried_empty" {
				input, _, _ := familyCompilerGuardRoundForTest(t, 0, nil)
				inputs = []familyCompilerGuardRoundInput{input}
			}
			var baselineManifest []byte
			var baselinePlan map[string]string
			for _, writer := range []string{"disabled", "enabled", "failed"} {
				p := familyCompilerGuardCarryPipelineForTest(t, "guards", inputs, []string{"base"})
				if p.diagnostics != os.Stderr {
					t.Fatal("production constructor did not wire stderr diagnostics")
				}
				var output bytes.Buffer
				p.diagnostics = nil
				if writer == "enabled" {
					p.diagnostics = &output
				} else if writer == "failed" {
					p.diagnostics = familyCompilerGuardDiagnosticErrorWriter{}
				}
				if mode == "carried_empty" {
					if done, err := p.carryForwardEmptyRound("guards"); err != nil || !done {
						t.Fatalf("empty carry failed: %t/%v", done, err)
					}
				} else {
					scopes := familyCompilerGuardCarryScopesForTest(t, p.toolsets)
					if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
						t.Fatal(err)
					}
					if mode == "source limit" {
						value := kconfig.ConfigDependencyCompilerGuardObservation{Truncated: true}
						for range 2 {
							if err := p.observe(value); err != nil {
								t.Fatal(err)
							}
						}
						if !p.truncated || p.active != nil || p.limitReason != "source_hint_limit" || p.limitCurrent != 0 || p.limitMax != 0 {
							t.Fatal("source overflow lost its fixed reason or invented unavailable counts")
						}
					}
					if err := p.finishVariant("base", scopes); err != nil {
						t.Fatal(err)
					}
					if err := p.publish(); err != nil {
						t.Fatal(err)
					}
				}
				manifest, err := os.ReadFile(p.flags.manifestOut)
				if err != nil {
					t.Fatal(err)
				}
				plan := familyCompilerGuardCarryTreeForTest(t, p.flags.planOut)
				if writer == "disabled" {
					baselineManifest, baselinePlan = manifest, plan
				} else if !bytes.Equal(manifest, baselineManifest) || !reflect.DeepEqual(plan, baselinePlan) {
					t.Fatal("diagnostic output changed canonical manifest or probe plan bytes")
				}
				if writer != "enabled" {
					continue
				}
				want := "compiler_guard_round round=0 event=published reason=none current=0 limit=0 query_memberships=0 unique_queries=0 contexts=0 names=0 calls=0 estimated_bytes=0 expanded_bytes=0 finished_variants=1 variants=1\n"
				switch mode {
				case "source limit":
					want = "compiler_guard_round round=0 event=limit reason=source_hint_limit current=0 limit=0 query_memberships=0 unique_queries=0 contexts=0 names=0 calls=0 estimated_bytes=0 expanded_bytes=0 finished_variants=0 variants=1\n" +
						"compiler_guard_round round=0 event=published reason=source_hint_limit current=0 limit=0 query_memberships=0 unique_queries=0 contexts=0 names=0 calls=0 estimated_bytes=0 expanded_bytes=0 finished_variants=1 variants=1\n"
				case "carried_empty":
					want = "compiler_guard_round round=1 event=carried_empty reason=none current=0 limit=0 query_memberships=0 unique_queries=0 contexts=0 names=0 calls=0 estimated_bytes=0 expanded_bytes=0 finished_variants=0 variants=1\n"
				}
				if output.String() != want {
					t.Fatal("round summary did not distinguish discovery, source truncation and empty carry")
				}
			}
		})
	}
}
