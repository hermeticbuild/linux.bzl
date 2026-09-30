package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestBoundedCounterHintPlanKeepsCompleteCanonicalPrefix(t *testing.T) {
	plan := familyCompilerGuardPlanForTest(t, "first", "second", "third")
	roots := slices.Clone(plan.Terminal)
	slices.Sort(roots)
	one, err := kconfig.SelectProbePlanTerminals(plan, roots[:1])
	if err != nil {
		t.Fatal(err)
	}
	oneBytes, fits := familyCompilerGuardOptionalPlanBytes(one, 1<<20)
	if !fits {
		t.Fatal("fixture too large")
	}
	for _, limits := range [][2]int{{oneBytes, 3}, {1 << 20, 1}} {
		input := []string{roots[2], roots[0], roots[1], roots[0]}
		got, size, err := boundedFamilyOptionalHintPlan(plan, input, limits[0], limits[1])
		if err != nil || !reflect.DeepEqual(got, one) || size != oneBytes || !slices.Equal(input, []string{roots[2], roots[0], roots[1], roots[0]}) {
			t.Fatalf("bounded selection changed closure or input: %#v size=%d error=%v", got, size, err)
		}
	}
	if got, _, err := boundedFamilyOptionalHintPlan(plan, roots, 0, 0); err != nil || got != nil {
		t.Fatalf("zero residual budget: %#v %v", got, err)
	}
	bad, err := kconfig.SelectProbePlanTerminals(plan, roots)
	if err != nil {
		t.Fatal(err)
	}
	bad.Nodes[len(bad.Nodes)-1].RequestID = strings.Repeat("a", 64)
	if _, _, err := boundedFamilyOptionalHintPlan(bad, roots, 0, 0); err == nil {
		t.Fatal("zero budget laundered malformed discarded branch")
	}
	if _, _, err := boundedFamilyOptionalHintPlan(plan, []string{strings.Repeat("a", 64)}, 0, 0); err == nil {
		t.Fatal("zero budget laundered unowned root")
	}
}

func TestBoundedCounterHintPlanChargesSharedDependenciesOnce(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	builder, err := kconfig.NewProbePlanBuilder(toolsets["target"], toolsets["host"])
	if err != nil {
		t.Fatal(err)
	}
	request := func(scope, source string, inputs ...kconfig.ProbeReference) kconfig.ProbeReference {
		t.Helper()
		ref, err := builder.Request(scope, kconfig.ProbeRequest{
			Schema: kconfig.LinuxProbeRequestSchema, InputCount: len(inputs),
			Steps:   []kconfig.ProbeStep{{Name: "guard", Tool: "cc", Arguments: []string{"-E", "-x", "c", "-"}, Stdin: source}},
			Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "guard", Stream: "stdout", RequireSuccess: true},
		}, inputs...)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	host, target := request("host", "common"), request("target", "common")
	first := request("target", "first", host, target, host)
	second := request("target", "second", host, target, host)
	plan, err := builder.Plan(first, second)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 2} {
		roots := slices.Sorted(slices.Values(plan.Terminal))[:count]
		want, err := kconfig.SelectProbePlanTerminals(plan, roots)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes, _ := familyCompilerGuardOptionalPlanBytes(want, 1<<20)
		got, size, err := boundedFamilyOptionalHintPlan(plan, plan.Terminal, wantBytes, count+2)
		if err != nil || !reflect.DeepEqual(got, want) || size != wantBytes || len(got.Requests) != count+1 {
			t.Fatalf("shared ordered dependencies: %#v %d %v", got, size, err)
		}
	}
}

func TestCounterHintStagingCannotDisplaceLiteralHints(t *testing.T) {
	plan := familyCompilerGuardPlanForTest(t, "first", "second", "third")
	roots := slices.Sorted(slices.Values(plan.Terminal))
	one, err := kconfig.SelectProbePlanTerminals(plan, roots[:1])
	if err != nil {
		t.Fatal(err)
	}
	oneBytes, _ := familyCompilerGuardOptionalPlanBytes(one, 1<<20)
	wholeBytes, _ := familyCompilerGuardOptionalPlanBytes(plan, 1<<20)
	stage := &familyCompilerGuardOptionalStage{kind: familyCompilerCounterHintQueryKind, plan: plan, planBytes: wholeBytes, planNodes: len(plan.Nodes)}
	for _, name := range []string{"base", "same"} {
		variant := familyCompilerGuardOptionalVariant{name: name, terminals: slices.Clone(roots)}
		for _, root := range roots {
			variant.queries = append(variant.queries, familyCompilerGuardOptionalQuery{terminal: root})
		}
		stage.variants = append(stage.variants, variant)
	}
	literal := &familyCompilerGuardOptionalStage{kind: familyCompilerLiteralHintQueryKind, planBytes: maxFamilyCompilerGuardBytes/2 - oneBytes}
	p := &familyCompilerGuardPipeline{literalHints: literal, counterHints: stage}
	if err := p.enforceOptionalStagingBudget(); err != nil {
		t.Fatal(err)
	}
	if literal.disabled || stage.disabled || stage.omitted != 1 || stage.planBytes != oneBytes || !reflect.DeepEqual(stage.plan, one) || len(stage.variants) != 2 {
		t.Fatal("counter overflow discarded stronger work or the complete retained prefix")
	}
	for _, variant := range stage.variants {
		if len(variant.queries) != 1 || variant.queries[0].terminal != roots[0] || !slices.Equal(variant.terminals, roots[:1]) {
			t.Fatal("shared root lost its exact per-variant ownership")
		}
	}
	// A later existing tier can consume all residual capacity; only counter
	// work is then dropped, and it cannot evict the literal stage on its way out.
	literal.planBytes = maxFamilyCompilerGuardBytes / 2
	if err := p.enforceOptionalStagingBudget(); err != nil || literal.disabled || !stage.disabled || stage.plan != nil {
		t.Fatalf("later stronger sibling lost its work: %v", err)
	}
}

func TestCounterHintSubsetCannotLaunderVariantOwnership(t *testing.T) {
	plan := familyCompilerGuardPlanForTest(t, "first", "second")
	stage := &familyCompilerGuardOptionalStage{kind: familyCompilerCounterHintQueryKind, plan: plan,
		variants: []familyCompilerGuardOptionalVariant{{name: "base", terminals: plan.Terminal[:1], queries: []familyCompilerGuardOptionalQuery{{terminal: plan.Terminal[1]}}}}}
	if err := stage.trimOptionalHintPlan(0, 0); err == nil {
		t.Fatal("discarded query selected another variant's terminal")
	}
}

func TestCounterHintVariantMergeRetainsSharedBoundedPrefix(t *testing.T) {
	plan := familyCompilerGuardPlanForTest(t, "first", "second", "third")
	roots := slices.Sorted(slices.Values(plan.Terminal))
	one, err := kconfig.SelectProbePlanTerminals(plan, roots[:1])
	if err != nil {
		t.Fatal(err)
	}
	oneBytes, _ := familyCompilerGuardOptionalPlanBytes(one, 1<<20)
	stage := &familyCompilerGuardOptionalStage{kind: familyCompilerCounterHintQueryKind}
	p := &familyCompilerGuardPipeline{
		literalHints: &familyCompilerGuardOptionalStage{kind: familyCompilerLiteralHintQueryKind, planBytes: maxFamilyCompilerGuardBytes/2 - oneBytes},
		counterHints: stage,
	}
	for index, name := range []string{"base", "btf", "debug", "lz4"} {
		variant := familyCompilerGuardOptionalVariant{name: name, terminals: slices.Clone(roots)}
		for _, root := range roots {
			variant.queries = append(variant.queries, familyCompilerGuardOptionalQuery{terminal: root})
		}
		if err := p.finishOptionalHintVariant(stage, variant, plan); err != nil {
			t.Fatal(err)
		}
		if p.literalHints.disabled || stage.disabled || stage.planBytes != oneBytes || !reflect.DeepEqual(stage.plan, one) || len(stage.variants) != index+1 {
			t.Fatal("a later variant discarded earlier shared roots or pre-existing hint work")
		}
		for _, kept := range stage.variants {
			if len(kept.queries) != 1 || kept.queries[0].terminal != roots[0] || !slices.Equal(kept.terminals, roots[:1]) {
				t.Fatal("shared closure lost its per-variant memberships")
			}
		}
	}
	if len(plan.Nodes) != 3 || len(plan.Terminal) != 3 {
		t.Fatal("bounded staging mutated the incoming plan")
	}
}

func familyCounterObservationForTest(count int) kconfig.ConfigDependencyCompilerGuardObservation {
	return kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-nostdinc", "-DOTHER=73"}, CounterCount: count}
}

func TestFamilyCounterHintsOnlySpendResidualBudgets(t *testing.T) {
	value := familyCounterObservationForTest(64)
	value.OptionalCounterHints = true
	p := familyCompilerOptionalLedgerForTest(0)
	p.active = map[string]*familyCompilerGuardQuery{}
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if p.count != 0 || p.callCount != 0 || len(p.active) != 0 || p.counterHints == nil || len(p.counterHints.ledger.active) != 1 {
		t.Fatal("weak hint consumed the live baseline ledger")
	}
	var query familyCompilerGuardQuery
	for _, q := range p.counterHints.ledger.active {
		query = *q
	}
	if query.Kind != familyCompilerCounterHintQueryKind {
		t.Fatal("hint became mandatory query")
	}
	for _, limit := range []string{"queries", "values", "memberships", "bytes", "expanded"} {
		t.Run(limit, func(t *testing.T) {
			p := familyCompilerOptionalLedgerForTest(0)
			stage := &familyCompilerGuardOptionalStage{kind: familyCompilerCounterHintQueryKind}
			switch limit {
			case "queries":
				p = familyCompilerOptionalLedgerForTest(maxFamilyCompilerGuardQueries)
			case "values":
				p.callCount = maxFamilyCompilerIntrinsicCalls - 63
			case "memberships":
				p.count = maxFamilyCompilerGuardMemberships
			case "bytes":
				p.bytes = maxFamilyCompilerGuardBytes / 2
			case "expanded":
				p.expandedBytes = maxFamilyCompilerGuardExpandedBytes
			}
			state := func() [7]int {
				return [7]int{p.count, p.bytes, p.expandedBytes, p.callCount, p.nameCount, len(p.contexts), len(p.uniqueQueries)}
			}
			before := state()
			if _, admitted, err := p.admitOptionalQueryForStage(query, stage); err != nil || admitted || p.truncated || state() != before || stage.omitted != 1 {
				t.Fatalf("optional counter displaced baseline: admitted=%t error=%v before=%v after=%v", admitted, err, before, state())
			}
		})
	}
	stage := &familyCompilerGuardOptionalStage{kind: familyCompilerCounterHintQueryKind}
	for range 2 {
		if _, admitted, err := p.admitOptionalQueryForStage(query, stage); err != nil || !admitted {
			t.Fatalf("admission: %t %v", admitted, err)
		}
	}
	if p.count != 2 || p.callCount != 128 || len(p.contexts) != 1 || len(p.uniqueQueries) != 1 {
		t.Fatal("shared descriptor hid per-variant counter replay work")
	}
}

func TestFamilyCounterHintsPreserveStrongStagePriority(t *testing.T) {
	p := &familyCompilerGuardPipeline{
		optional:     &familyCompilerGuardOptionalStage{planBytes: maxFamilyCompilerGuardBytes / 4},
		tokenHints:   &familyCompilerGuardOptionalStage{planBytes: maxFamilyCompilerGuardBytes / 4},
		counterHints: &familyCompilerGuardOptionalStage{planBytes: 1},
	}
	p.enforceOptionalStagingBudget()
	if p.optional.disabled || p.tokenHints.disabled || !p.counterHints.disabled || p.truncated {
		t.Fatal("weak counter stage displaced stronger work")
	}
}

func TestFamilyCounterHintReplayKeepsRejectionDistinctFromMeasuredPrefixes(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "answered"}[answered], func(t *testing.T) {
			toolsets := familyCompilerGuardPlanForTest(t).Toolsets
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			variants := []familyPlanVariantRequest{{name: "base"}, {name: "same"}}
			p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			value := familyCounterObservationForTest(16)
			value.OptionalCounterHints = true
			for _, variant := range variants {
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := p.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				if err := p.observe(value); err != nil {
					t.Fatal(err)
				}
				if err := p.finishVariant(variant.name, scopes); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.publish(); err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readFamilyCompilerGuardManifest(flags.manifestOut)
			if err != nil || len(manifest.Variants["base"]) != 1 || manifest.Variants["base"][0].Kind != familyCompilerCounterHintQueryKind {
				t.Fatalf("optional counter wire: %#v %v", manifest, err)
			}
			plan, err := kconfig.ReadProbePlan(flags.planOut)
			if err != nil || len(plan.Nodes) != 1 || p.count != 2 || p.callCount != 32 {
				t.Fatalf("optional counter plan/accounting: %#v %v", plan, err)
			}
			input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
			if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			node := plan.Nodes[0]
			request := plan.Requests[node.RequestID]
			if request.Outcome.Kind != "boolean" {
				t.Fatal("weak query received mandatory outcome")
			}
			status, code := "failure", 1
			if answered {
				status, code = "success", 0
			}
			writeTestProbeResult(t, input.target, kconfig.ProbeResult{Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, ToolsetIdentity: toolsets[node.Scope], Kind: "boolean", Boolean: &answered,
				Steps: []kconfig.ProbeStepResult{{Name: request.Steps[0].Name, Status: status, ExitCode: code, Stdout: strings.Repeat("7\n", 16)}}})
			nextFlags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
			next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
			if err != nil {
				t.Fatal(err)
			}
			for _, variant := range variants {
				scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
				if err := next.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
				if err := next.observe(value); err != nil {
					t.Fatal(err)
				}
				// Rejected weak attempts must not suppress actual source demands.
				if err := next.observe(familyCounterObservationForTest(16)); err != nil {
					t.Fatal(err)
				}
				if answered != (len(next.active) == 0) {
					t.Fatal("rejection was confused with a measured prefix")
				}
				if err := next.finishVariant(variant.name, scopes); err != nil {
					t.Fatal(err)
				}
			}
			if err := next.publish(); err != nil {
				t.Fatal(err)
			}
			for _, variant := range variants {
				queries := next.queries[variant.name]
				if answered && len(queries) != 0 || !answered && (len(queries) != 1 || queries[0].Kind != familyCompilerCounterQueryKind) {
					t.Fatal("weak replay changed mandatory scheduling")
				}
			}
		})
	}
}

func TestFamilyCounterCoalescesLargestDemandAndChargesGrowth(t *testing.T) {
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}}
	value := familyCounterObservationForTest(16)
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	bytesBefore, expandedBefore := p.bytes, p.expandedBytes
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if p.count != 1 || p.callCount != 16 || p.bytes != bytesBefore || p.expandedBytes != expandedBefore {
		t.Fatal("duplicate counter demand charged again")
	}
	value.CounterCount = 32
	if err := p.observe(value); err != nil {
		t.Fatal(err)
	}
	if p.count != 1 || p.callCount != 32 || p.bytes-bytesBefore != 16*len("__COUNTER__\n") || len(p.active) != 1 {
		t.Fatal("larger demand did not charge only vector growth")
	}
	for key, query := range p.active {
		if query.CounterCount != 32 {
			t.Fatal("did not retain largest requested prefix")
		}
		p.counterKnown = map[string]int{key: 64}
	}
	p.active = map[string]*familyCompilerGuardQuery{}
	if err := p.observe(familyCounterObservationForTest(64)); err != nil || len(p.active) != 0 {
		t.Fatal("already measured prefix rescheduled")
	}
	if err := p.observe(familyCounterObservationForTest(65)); err != nil || len(p.active) != 1 {
		t.Fatal("unmeasured suffix treated as measured")
	}
	if value.Arguments[1] != "-DOTHER=73" {
		t.Fatal("mutated caller arguments")
	}
}

func TestFamilyCounterPayloadAndLosslessWireIdentity(t *testing.T) {
	m := familyCompilerGuardTransportManifestForTest()
	q := m.Variants["base"][0]
	q.Kind, q.Names, q.CounterCount = familyCompilerCounterQueryKind, nil, 16
	if err := q.validatePayload(); err != nil {
		t.Fatal(err)
	}
	other := q
	other.CounterCount = 32
	if q.contextKey() != other.contextKey() || q.payloadKey() == other.payloadKey() {
		t.Fatal("count changed shared context or failed to change request payload identity")
	}
	m.Variants["base"] = []familyCompilerGuardQuery{q}
	m.Variants["other"] = []familyCompilerGuardQuery{other}
	data := familyCompilerGuardTransportBytesForTest(t, m)
	decoded, err := unmarshalFamilyCompilerGuardManifest(data)
	if err != nil || !reflect.DeepEqual(decoded, &m) || !bytes.Equal(data, familyCompilerGuardTransportBytesForTest(t, *decoded)) {
		t.Fatalf("counter manifest changed on round trip: %v", err)
	}
	for _, bad := range []familyCompilerGuardQuery{
		{Kind: familyCompilerCounterQueryKind, Language: "c", CounterCount: -1},
		{Kind: familyCompilerCounterQueryKind, Language: "c", CounterCount: 0},
		{Kind: familyCompilerCounterQueryKind, Language: "c", CounterCount: 4097},
		{Kind: familyCompilerCounterQueryKind, Language: "assembler-with-cpp", CounterCount: 16},
		{Kind: familyCompilerCounterQueryKind, Language: "c", CounterCount: 16, Names: []string{}},
		{Kind: familyCompilerCounterQueryKind, Language: "c", CounterCount: 16, Calls: []kconfig.CompilerIntrinsicCall{}},
		{Language: "c", CounterCount: 16, Names: []string{"A"}},
		{Kind: familyCompilerIntrinsicQueryKind, Language: "c", CounterCount: 16},
	} {
		if err := bad.validatePayload(); err == nil {
			t.Fatalf("accepted mixed/noncanonical counter payload: %#v", bad)
		}
	}
	legacy := familyCompilerGuardTransportManifestForTest()
	if bytes.Contains(familyCompilerGuardTransportBytesForTest(t, legacy), []byte("CounterCount")) {
		t.Fatal("new field changed ordinary canonical wire bytes")
	}
}

func TestFamilyCounterKeepsExistingSharedBudgets(t *testing.T) {
	p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}, callCount: maxFamilyCompilerIntrinsicCalls - 16}
	if err := p.observe(familyCounterObservationForTest(16)); err != nil || p.truncated || p.callCount != maxFamilyCompilerIntrinsicCalls {
		t.Fatalf("exact shared value budget rejected: %v", err)
	}
	if err := p.observe(familyCounterObservationForTest(32)); err != nil || !p.truncated || p.active != nil || p.limitReason != "intrinsic_value_limit" {
		t.Fatalf("counter growth bypassed shared value cap: %v", err)
	}
	for _, kind := range []string{"bytes", "expanded", "memberships"} {
		p := &familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}, known: map[string]map[string]bool{}}
		switch kind {
		case "bytes":
			p.bytes = maxFamilyCompilerGuardBytes / 2
		case "expanded":
			p.expandedBytes = maxFamilyCompilerGuardExpandedBytes
		case "memberships":
			p.count = maxFamilyCompilerGuardMemberships
		}
		if err := p.observe(familyCounterObservationForTest(16)); err != nil || !p.truncated || p.active != nil {
			t.Fatalf("counter demand bypassed %s cap: %v", kind, err)
		}
	}
}

func TestFamilyCounterFrozenReplayAcrossVariantsAndLargerNextDemand(t *testing.T) {
	toolsets := familyCompilerGuardPlanForTest(t).Toolsets
	flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
	variants := []familyPlanVariantRequest{{name: "base"}, {name: "same"}}
	p, err := newFamilyCompilerGuardPipeline(&flags, nil, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := p.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		for _, count := range []int{8, 16, 8, 16} {
			if err := p.observe(familyCounterObservationForTest(count)); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.finishVariant(variant.name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if p.count != 2 || p.callCount != 32 || len(p.contexts) != 1 || len(p.uniqueQueries) != 1 {
		t.Fatal("variant memberships were confused with unique compiler executions")
	}
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.ReadProbePlan(flags.planOut)
	if err != nil || len(plan.Nodes) != 1 {
		t.Fatalf("shared counter plan: %v", err)
	}
	input := familyCompilerGuardRoundInput{manifest: flags.manifestOut, plan: flags.planOut, host: t.TempDir(), target: t.TempDir()}
	if err := os.WriteFile(filepath.Join(input.host, ".empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		step := plan.Requests[node.RequestID].Steps[0]
		if step.Stdin != strings.Repeat("__COUNTER__\n", 16) {
			t.Fatal("counter plan lost exact bounded input")
		}
		text := strings.Repeat("7\n", 16)
		writeTestProbeResult(t, input.target, kconfig.ProbeResult{Schema: kconfig.LinuxProbeResultSchema,
			NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, ToolsetIdentity: toolsets[node.Scope], Kind: "text", Text: text,
			Steps: []kconfig.ProbeStepResult{{Name: step.Name, Status: "success", Stdout: text}},
		})
	}
	nextFlags, _ := familyCompilerGuardFlagsForTest(t, "guards", 1)
	next, err := newFamilyCompilerGuardPipeline(&nextFlags, []familyCompilerGuardRoundInput{input}, variants, toolsets)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		scopes := familyCompilerGuardCarryScopesForTest(t, toolsets)
		if err := next.prepareVariant(variant.name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := next.observe(familyCounterObservationForTest(16)); err != nil || len(next.active) != 0 {
			t.Fatal("authenticated covered prefix rescheduled")
		}
		if err := next.observe(familyCounterObservationForTest(32)); err != nil || len(next.active) != 1 {
			t.Fatal("new suffix was not scheduled")
		}
		if err := next.finishVariant(variant.name, scopes); err != nil {
			t.Fatal(err)
		}
	}
	if err := next.publish(); err != nil {
		t.Fatalf("exact frozen union replay: %v", err)
	}
	manifest, _, err := readFamilyCompilerGuardManifest(nextFlags.manifestOut)
	if err != nil || !maps.Equal(manifest.Toolsets, toolsets) || manifest.Variants["base"][0].CounterCount != 32 {
		t.Fatal("next round failed to carry exact counter count")
	}
}
