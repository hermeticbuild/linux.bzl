package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func familyCompilerIntrinsicObservationForTest() kconfig.ConfigDependencyCompilerGuardObservation {
	return kconfig.ConfigDependencyCompilerGuardObservation{
		Scope: "target", Role: "cc", Language: "c",
		Arguments: []string{"-DFEATURE=7", "-UFEATURE", "-DFEATURE=8"},
		Calls:     []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "__retain__"}},
	}
}

func TestFamilyCompilerIntrinsicObservationUsesDistinctExactContext(t *testing.T) {
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}}
	observation := familyCompilerIntrinsicObservationForTest()
	observation.Environment = map[string]string{"MODE": "exact"}
	originalArgs := slices.Clone(observation.Arguments)
	for range 2 {
		if err := p.observe(observation); err != nil {
			t.Fatal(err)
		}
	}
	if p.count != 1 || p.callCount != 1 || p.nameCount != 0 || len(p.active) != 1 {
		t.Fatalf("intrinsic was duplicated or counted as definedness: %#v", p)
	}
	key := (familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: observation.Scope, Role: observation.Role, Language: observation.Language, Arguments: observation.Arguments, Environment: observation.Environment}).contextKey()
	query := p.active[key]
	if query == nil || query.Kind != familyCompilerIntrinsicQueryKind || len(query.Names) != 0 || !slices.Equal(query.Calls, observation.Calls) {
		t.Fatalf("intrinsic transport lost its type: %#v", query)
	}
	firstBytes := p.bytes
	firstExpandedBytes := familyCompilerGuardExpandedQueryBytes(*query)
	if p.expandedBytes != firstExpandedBytes {
		t.Fatal("live intrinsic expansion differs from prior-manifest admission accounting")
	}
	observation.Arguments[0], observation.Environment["MODE"], observation.Calls[0].Operand = "mutated", "mutated", "mutated"
	if !slices.Equal(query.Arguments, originalArgs) || query.Environment["MODE"] != "exact" || query.Calls[0].Operand != "__retain__" {
		t.Fatal("observer-owned data changed a queued intrinsic query")
	}
	definedness := familyCompilerIntrinsicObservationForTest()
	definedness.Environment = map[string]string{"MODE": "exact"}
	definedness.Names, definedness.Calls = []string{"__has_attribute"}, nil
	if err := p.observe(definedness); err != nil || p.count != 2 || p.callCount != 1 || p.nameCount != 1 {
		t.Fatalf("definedness conflated with same-context predicate: %v", err)
	}
	if len(p.contexts) != 1 {
		t.Fatal("definedness and intrinsic kinds duplicated one immutable compiler context")
	}
	definednessQuery := familyCompilerGuardQuery{Scope: definedness.Scope, Role: definedness.Role, Language: definedness.Language,
		Arguments: definedness.Arguments, Environment: definedness.Environment, Names: definedness.Names}
	if p.bytes != firstBytes+familyCompilerGuardQueryReferenceBytes+len("__has_attribute")+4 ||
		p.expandedBytes != firstExpandedBytes+familyCompilerGuardExpandedQueryBytes(definednessQuery) {
		t.Fatal("shared context was charged twice or repeated replay work was not charged")
	}
	changed := familyCompilerIntrinsicObservationForTest()
	changed.Environment = map[string]string{"MODE": "exact"}
	changed.Arguments[2] = "-DFEATURE=9"
	if err := p.observe(changed); err != nil || p.count != 3 || p.callCount != 2 {
		t.Fatalf("replacement text was canonicalized away from intrinsic context: %v", err)
	}
	if len(p.contexts) != 2 {
		t.Fatal("different original replacement bytes shared an interned context")
	}
	mixed := familyCompilerIntrinsicObservationForTest()
	mixed.Names = []string{"__has_attribute"}
	if err := p.observe(mixed); err == nil {
		t.Fatal("mixed definedness/call observation accepted")
	}
}

func TestFamilyCompilerIntrinsicManifestRejectsMalformedTypedQueries(t *testing.T) {
	input, original, _ := familyCompilerGuardRoundForTest(t, 0, nil)
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	for _, change := range []string{"missing kind", "unknown kind", "names and calls", "empty names alias", "empty calls", "duplicate calls", "unsorted calls", "unsorted operators", "attribute operand is builtin operator", "builtin operand is attribute operator", "operator", "operand", "assembly"} {
		t.Run(change, func(t *testing.T) {
			query := familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: "target", Role: "cc", Language: "c",
				Calls: []kconfig.CompilerIntrinsicCall{{Operator: "__has_attribute", Operand: "btf_type_tag"}}}
			switch change {
			case "missing kind":
				query.Kind = ""
			case "unknown kind":
				query.Kind = "definedness"
			case "names and calls":
				query.Names = []string{"__has_attribute"}
			case "empty names alias":
				query.Names = []string{}
			case "empty calls":
				query.Calls = nil
			case "duplicate calls":
				query.Calls = append(query.Calls, query.Calls[0])
			case "unsorted calls":
				query.Calls = append(query.Calls, kconfig.CompilerIntrinsicCall{Operator: "__has_attribute", Operand: "__retain__"})
			case "unsorted operators":
				query.Calls = append([]kconfig.CompilerIntrinsicCall{{Operator: "__has_builtin", Operand: "btf_type_tag"}}, query.Calls...)
			case "attribute operand is builtin operator":
				query.Calls[0].Operand = "__has_builtin"
			case "builtin operand is attribute operator":
				query.Calls[0] = kconfig.CompilerIntrinsicCall{Operator: "__has_builtin", Operand: "__has_attribute"}
			case "operator":
				query.Calls[0].Operator = "__has_include"
			case "operand":
				query.Calls[0].Operand = "btf_type_tag)\n#error injected"
			case "assembly":
				query.Language = "assembler-with-cpp"
			}
			manifest := original
			manifest.Variants = map[string][]familyCompilerGuardQuery{"base": {query}}
			familyCompilerGuardWriteManifestForTest(t, input.manifest, manifest)
			if _, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{input}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets); err == nil {
				t.Fatal("noncanonical typed intrinsic manifest accepted")
			}
		})
	}
}

func TestFamilyCompilerIntrinsicPreflightAndFrontierBudgets(t *testing.T) {
	for _, field := range []string{"Calls", "cAlLs"} {
		// Structural allocation is bounded before typed call validation; tiny
		// entries exercise the aggregate limit without a large valid payload.
		entry := `null`
		payload := []byte(`{"Variants":{"base":[{"` + field + `":[` + strings.Repeat(entry+",", maxFamilyCompilerIntrinsicCalls) + entry + `]}]}}`)
		if err := preflightFamilyCompilerGuardJSON(payload); err == nil || !strings.Contains(err.Error(), "intrinsic call budget") {
			t.Fatalf("%s allocation preflight accepted excess calls: %v", field, err)
		}
	}
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	empty := familyCompilerGuardPlanForTest(t)
	p := &familyCompilerGuardPipeline{
		flags: &flags, names: []string{"base", "later"}, toolsets: maps.Clone(empty.Toolsets),
		queries: map[string][]familyCompilerGuardQuery{"base": {{Scope: "target", Role: "cc", Language: "c", Names: []string{"__EARLIER"}}}},
		plans:   []kconfig.ProbePlanVariant{{Name: "base", Plan: empty}, {Name: "later", Plan: empty}},
		active:  map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}, finished: map[string]bool{"base": true, "later": true},
		callCount: maxFamilyCompilerIntrinsicCalls - 1,
	}
	value := familyCompilerIntrinsicObservationForTest()
	if err := p.observe(value); err != nil || p.truncated || p.callCount != maxFamilyCompilerIntrinsicCalls {
		t.Fatalf("exact intrinsic call budget rejected: %v", err)
	}
	if err := p.observe(value); err != nil || p.truncated || p.callCount != maxFamilyCompilerIntrinsicCalls {
		t.Fatal("duplicate call consumed extra budget")
	}
	value.Calls[0].Operand = "btf_type_tag"
	if err := p.observe(value); err != nil || !p.truncated || len(p.active) != 0 {
		t.Fatalf("excess calls retained a partial frontier: %v", err)
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
	if err != nil || !manifest.Truncated || len(manifest.Variants["base"])+len(manifest.Variants["later"]) != 0 {
		t.Fatalf("call overflow failed to discard sibling definedness queries: %#v %v", manifest, err)
	}
}

func TestFamilyCompilerIntrinsicRoundTripFiltersAnsweredCallsAndRetainsStrictReplay(t *testing.T) {
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
	value := familyCompilerIntrinsicObservationForTest()
	// Identical operands must retain independent operator answers and replay
	// witnesses, even when both calls share one request and compiler context.
	value.Calls = append(value.Calls, kconfig.CompilerIntrinsicCall{Operator: "__has_builtin", Operand: value.Calls[0].Operand})
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
	if err != nil || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].Kind != familyCompilerIntrinsicQueryKind || !slices.Equal(manifest.Variants["base"][0].Calls, value.Calls) {
		t.Fatalf("published intrinsic manifest: %#v %v", manifest, err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("published intrinsic plan: %#v %v", plan, err)
	}
	input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
	if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	node := plan.Nodes[0]
	step := plan.Requests[node.RequestID].Steps[0].Name
	result := kconfig.ProbeResult{Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: toolsets[node.Scope], Kind: "text", Text: "202311 0x1\n",
		Steps: []kconfig.ProbeStepResult{{Name: step, Status: "success", Stdout: "202311 0x1\n"}}}
	writeTestProbeResult(t, input.target, result)
	for _, mode := range []string{"guards", "replay"} {
		t.Run(mode, func(t *testing.T) {
			nextFlags, _ := familyCompilerGuardFlagsForTest(t, mode, 1)
			next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			if carried, err := next.carryForwardEmptyRound(mode); err != nil || carried {
				t.Fatalf("unreplayed intrinsic query was mistaken for empty work: %t %v", carried, err)
			}
			scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
			if err := next.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
				t.Fatal(err)
			}
			if err := next.observe(value); err != nil || next.count != 0 || next.callCount != 0 {
				t.Fatalf("answered exact call repeated in new frontier: %v", err)
			}
			if err := next.finishVariant("base", scopes); err != nil {
				t.Fatal(err)
			}
			if err := next.publish(); err != nil {
				t.Fatal(err)
			}
			if mode == "guards" {
				got, _, err := readFamilyCompilerGuardManifest(nextFlags.manifestOut)
				if err != nil || got.Truncated || !reflect.DeepEqual(got.Variants, map[string][]familyCompilerGuardQuery{"base": nil}) {
					t.Fatalf("answered call failed to converge: %#v %v", got, err)
				}
			}
		})
	}
	for _, failure := range []string{"result toolset", "current toolset", "request", "stdout", "missing value", "failed step"} {
		t.Run(failure, func(t *testing.T) {
			changed := result
			changed.Steps = slices.Clone(result.Steps)
			currentToolsets := maps.Clone(toolsets)
			switch failure {
			case "result toolset":
				changed.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "current toolset":
				currentToolsets["target"] = "sha256-" + strings.Repeat("f", 64)
			case "request":
				changed.RequestID = strings.Repeat("f", 64)
			case "stdout":
				changed.Steps[0].Stdout = "0 0\n"
			case "missing value":
				changed.Text, changed.Steps[0].Stdout = "202311\n", "202311\n"
			case "failed step":
				changed.Steps[0].Status, changed.Steps[0].ExitCode = "failure", 1
			}
			writeTestProbeResult(t, input.target, changed)
			nextFlags, _ := familyCompilerGuardFlagsForTest(t, "replay", 1)
			next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
			metadata := &kconfig.CompactMetadata{}
			if err == nil {
				err = next.prepareVariant("base", familyCompilerGuardCarryScopesForTest(t, currentToolsets), metadata)
			}
			if err == nil || !reflect.ValueOf(metadata).Elem().IsZero() {
				t.Fatal("invalid mixed-operator witness published complete or partial answers")
			}
		})
	}
	writeTestProbeResult(t, input.target, result)
	// A structurally valid result for the original call cannot satisfy a
	// different operand merely because the previous manifest is parseable.
	changed := *manifest
	changed.Variants = map[string][]familyCompilerGuardQuery{"base": {manifest.Variants["base"][0]}}
	query := &changed.Variants["base"][0]
	query.Calls = slices.Clone(query.Calls)
	query.Calls[0].Operand = "__always_inline__"
	familyCompilerGuardWriteManifestForTest(t, input.manifest, changed)
	finalFlags, _ := familyCompilerGuardFlagsForTest(t, "replay", 1)
	final, err := newFamilyCompilerGuardPipeline(&finalFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	if err := final.prepareVariant("base", familyCompilerGuardCarryScopesForTest(t, toolsets), &kconfig.CompactMetadata{}); err == nil {
		t.Fatal("changed operand replayed another call's measured result")
	}
}

func TestFamilyCompilerIntrinsicMixedOperatorOrderingAndIdentity(t *testing.T) {
	attribute := kconfig.CompilerIntrinsicCall{Operator: "__has_attribute", Operand: "shared_operand"}
	builtin := kconfig.CompilerIntrinsicCall{Operator: "__has_builtin", Operand: attribute.Operand}
	if familyCompilerIntrinsicCallKey(attribute) == familyCompilerIntrinsicCallKey(builtin) {
		t.Fatal("same operand erased the operator from answer lookup identity")
	}
	query := familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: "target", Role: "cc", Language: "c", Calls: []kconfig.CompilerIntrinsicCall{attribute}}
	other := query
	other.Calls = []kconfig.CompilerIntrinsicCall{builtin}
	if query.contextKey() != other.contextKey() || query.payloadKey() == other.payloadKey() {
		t.Fatal("operator must change the complete query identity, not the shared compiler context")
	}
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	var ids []string
	for _, calls := range [][]kconfig.CompilerIntrinsicCall{{attribute}, {builtin}, {builtin, attribute, builtin}, {attribute, builtin}} {
		flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
		p, err := newFamilyCompilerGuardPipeline(&flags, nil, []familyPlanVariantRequest{{name: "base"}}, toolsets)
		if err != nil {
			t.Fatal(err)
		}
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant("base", scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		original := slices.Clone(calls)
		if err := p.observe(kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Calls: calls}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant("base", scopes); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(calls, original) || len(p.plans) != 1 || len(p.plans[0].Plan.Nodes) != 1 {
			t.Fatal("canonical mixed batch mutated callers or split one compiler context")
		}
		id, err := familyCompilerGuardPlanID(p.plans[0].Plan)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if len(calls) > 1 && !slices.Equal(p.queries["base"][0].Calls, []kconfig.CompilerIntrinsicCall{attribute, builtin}) {
			t.Fatal("mixed calls were not deduplicated and sorted by operator then operand")
		}
	}
	if ids[0] == ids[1] || ids[0] == ids[2] || ids[1] == ids[2] || ids[2] != ids[3] {
		t.Fatal("operator changes aliased exact request plans, or reordered duplicates changed canonical bytes")
	}
}

func TestFamilyCompilerIntrinsicStdinBoundaryTruncatesBeforeConstruction(t *testing.T) {
	value := familyCompilerIntrinsicObservationForTest()
	key := (familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: value.Scope, Role: value.Role, Language: value.Language, Arguments: value.Arguments}).contextKey()
	size := familyCompilerIntrinsicStdinBytes(value.Calls[0])
	wantSnippet := "#undef " + value.Calls[0].Operand + "\n" + value.Calls[0].Operator + "(" + value.Calls[0].Operand + ")\n"
	if size != len(wantSnippet) {
		t.Fatal("coordinator stdin accounting differs from intrinsic source framing")
	}
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{},
		queryBytes: map[string]int{key: kconfig.MaxProbeInterpolatedBytes - size}}
	if err := p.observe(value); err != nil || p.truncated || p.queryBytes[key] != kconfig.MaxProbeInterpolatedBytes {
		t.Fatalf("exact stdin boundary rejected: %v", err)
	}
	if err := p.observe(value); err != nil || p.truncated || p.queryBytes[key] != kconfig.MaxProbeInterpolatedBytes {
		t.Fatal("duplicate call consumed additional stdin budget")
	}
	value.Calls[0].Operand = "btf_type_tag"
	if err := p.observe(value); err != nil || !p.truncated || len(p.active) != 0 {
		t.Fatalf("oversized new stdin retained optional actions: %v", err)
	}
	// A frozen manifest must enforce the same actual source limit, not just
	// accept 4,096 individually valid identifiers and fail during execution.
	query := familyCompilerGuardQuery{Kind: familyCompilerIntrinsicQueryKind, Scope: "target", Role: "cc", Language: "c"}
	for index := range 4096 {
		operand := strings.Repeat("x", 124) + fmt.Sprintf("%04x", index)
		query.Calls = append(query.Calls, kconfig.CompilerIntrinsicCall{Operator: "__has_attribute", Operand: operand})
	}
	if err := query.validatePayload(); err == nil || !strings.Contains(err.Error(), "stdin budget") {
		t.Fatalf("manifest accepted source beyond the actual stdin limit: %v", err)
	}
}
