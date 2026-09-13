package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func familyCounterObservationForTest(count int) kconfig.ConfigDependencyCompilerGuardObservation {
	return kconfig.ConfigDependencyCompilerGuardObservation{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-nostdinc", "-DOTHER=73"}, CounterCount: count}
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
