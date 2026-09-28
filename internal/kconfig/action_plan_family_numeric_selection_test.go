package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
)

func prospectiveFamilyVariantForTest(t *testing.T, other string) ActionPlanFamilyVariant {
	t.Helper()
	return prospectiveFamilyVariantWithNumericForTest(t, selectionArtifactVariant(t, other))
}

func prospectiveFamilyVariantWithNumericForTest(t *testing.T, variant ActionPlanFamilyVariant) ActionPlanFamilyVariant {
	t.Helper()
	return selectionMutateVariant(t, variant, func(plan *ActionPlan, consumer *ActionPlanNode, recipe *ActionRecipe, _ *ActionPlanNode) {
		generator := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"}}
		id, err := generator.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[id] = generator
		binding := "numeric:" + planOrdinal(len(consumer.Inputs))
		consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{Role: "numeric", ProducerID: "numeric-header", Slot: 0})
		recipe.Inputs = append(recipe.Inputs, binding)
		recipe.WorkingInputs["input:"+binding] = "include/generated/numeric.h"
		recipe.CompilerInvocation.WorkingInputUses = append(recipe.CompilerInvocation.WorkingInputUses, "input:"+binding)
		slices.Sort(recipe.CompilerInvocation.WorkingInputUses)
		plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: "numeric-header", Stage: "prep", Kind: "generate", Tool: "actionfile",
			Recipe: id, Product: "vmlinux", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/numeric.h"}}})
	})
}

func prospectiveFamilyDemandsForTest(t *testing.T, variants []ActionPlanFamilyVariant) map[string]ConfigDependencyGeneratedHeaderDemandCollection {
	t.Helper()
	demands := selectionEnabledDemands(variants)
	for _, variant := range variants {
		collection := demands[variant.Name]
		collection.prospective = []ConfigDependencyGeneratedHeaderDemand{initialExecutionDemandForTest(t, variant.Snapshot, "include/generated/numeric.h")}
		demands[variant.Name] = collection
	}
	return demands
}

// This fixture's numeric generator has no config inputs. The complete reducer
// must share its exact producer/slot while preserving both consumer instances.
func prospectiveSharedNumericRootForTest(t *testing.T, family *ActionPlanFamily, variants []ActionPlanFamilyVariant, demands map[string]ConfigDependencyGeneratedHeaderDemandCollection) (ActionPlanFamilyExecutionCutRoot, map[string]string) {
	t.Helper()
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	consumers := map[string]bool{}
	memberships := map[string]string{}
	for _, variant := range variants {
		collection := demands[variant.Name]
		if len(collection.prospective) != 1 {
			t.Fatal("fixture must have one numeric membership per variant")
		}
		demand := collection.prospective[0]
		producer := family.originalNodeIDs[variant.Name][demand.ProducerNodeID]
		consumer := family.originalNodeIDs[variant.Name][demand.ConsumerNodeID]
		if producer == "" || consumer == "" {
			t.Fatal("numeric fixture lost exact reduced provenance")
		}
		roots[ActionPlanFamilyExecutionCutRoot{NodeID: producer, Slot: demand.Slot}] = true
		consumers[consumer] = true
		memberships[variant.Name+"\x00"+demand.ConsumerNodeID] = consumer
	}
	if len(variants) != 2 || len(roots) != 1 || len(consumers) != 2 || len(memberships) != 2 {
		t.Fatalf("fixture roots/consumers/memberships=%d/%d/%d, want 1/2/2", len(roots), len(consumers), len(memberships))
	}
	return slices.Collect(maps.Keys(roots))[0], memberships
}

func requireProspectiveRootSetForTest(t *testing.T, got []ActionPlanFamilyExecutionCutRoot, want map[ActionPlanFamilyExecutionCutRoot]bool) {
	t.Helper()
	actual := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, root := range got {
		actual[root] = true
	}
	if len(actual) != len(got) || !maps.Equal(actual, want) {
		t.Fatalf("exact root set=%v, want %v", actual, want)
	}
}

func requireSameProspectiveBaselineForTest(t *testing.T, before, after *ActionPlanFamilyInitialExecution) {
	t.Helper()
	a, err := json.Marshal(before.Cut.contract)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(after.Cut.contract)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(before.Family)
	y, _ := json.Marshal(after.Family)
	if before.Cut.ID() != after.Cut.ID() || !bytes.Equal(a, b) || !bytes.Equal(x, y) {
		t.Fatal("omitted prospective tier changed baseline family or complete cut bytes")
	}
	if _, err := after.Cut.Verify(after.Family); err != nil {
		t.Fatal(err)
	}
}

func TestInitialProspectiveNumericGlobalAdmissionPreservesExecutableBaseline(t *testing.T) {
	variants := []ActionPlanFamilyVariant{prospectiveFamilyVariantForTest(t, "0"), prospectiveFamilyVariantForTest(t, "1")}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	var usage actionPlanFamilyDemandUsage
	if overflow, err := addInitialFamilyExecutableGroupsWithUsage(family, variants, groups, 0, 0,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, &usage); err != nil || overflow || usage.records != 2 {
		t.Fatalf("fixture executable accounting=%#v overflow=%t error=%v", usage, overflow, err)
	}
	for _, mode := range []string{"record_overflow", "byte_overflow", "local_omission"} {
		t.Run(mode, func(t *testing.T) {
			demands := prospectiveFamilyDemandsForTest(t, variants)
			maximumRecords, maximumBytes := MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes
			size := 0
			for _, variant := range variants {
				collection := demands[variant.Name]
				// Both local stages fit their unchanged collector limits; only the
				// family-wide combination with executable demands is too large.
				stage := &configDependencyGeneratedHeaderDemandCollector{records: map[ConfigDependencyGeneratedHeaderDemand]configDependencyGeneratedHeaderDemandWitness{collection.prospective[0]: {}}}
				stage.bytes = configDependencyGeneratedHeaderDemandBytes(collection.prospective[0])
				if !(&configDependencyGeneratedHeaderDemandCollector{}).acceptsProspective(stage) {
					t.Fatal("local fixture stage does not fit")
				}
				size += stage.bytes
			}
			switch mode {
			case "record_overflow":
				maximumRecords = usage.records + 1
			case "byte_overflow":
				maximumBytes = usage.bytes + size - 1
			case "local_omission":
				collection := demands[variants[1].Name]
				collection.prospective, collection.prospectiveTruncated = nil, true
				demands[variants[1].Name] = collection
			}
			baseline, err := newActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants), maximumRecords, maximumBytes)
			if err != nil {
				t.Fatal(err)
			}
			selectionRequireValid(t, baseline, 2, 2)
			got, err := newActionPlanFamilyInitialExecution(variants, demands, maximumRecords, maximumBytes)
			if err != nil {
				t.Fatal(err)
			}
			requireSameProspectiveBaselineForTest(t, baseline, got)
			slices.Reverse(variants)
			reversed, err := newActionPlanFamilyInitialExecution(variants, demands, maximumRecords, maximumBytes)
			if err != nil {
				t.Fatal(err)
			}
			requireSameProspectiveBaselineForTest(t, baseline, reversed)
			slices.Reverse(variants)
		})
	}
}

func TestInitialProspectiveNumericExactBoundsAndRootAdmission(t *testing.T) {
	variants := []ActionPlanFamilyVariant{prospectiveFamilyVariantForTest(t, "0"), prospectiveFamilyVariantForTest(t, "1")}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	demands := prospectiveFamilyDemandsForTest(t, variants)
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	var usage actionPlanFamilyDemandUsage
	if overflow, err := addInitialFamilyExecutableGroupsWithUsage(family, variants, groups, 0, 0,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, &usage); err != nil || overflow {
		t.Fatalf("%t %v", overflow, err)
	}
	maximumBytes := usage.bytes
	for _, variant := range variants {
		maximumBytes += configDependencyGeneratedHeaderDemandBytes(demands[variant.Name].prospective[0])
	}
	beforeGroups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	for key, group := range groups {
		copy := *group
		copy.roots, copy.consumers = maps.Clone(group.roots), maps.Clone(group.consumers)
		beforeGroups[key] = &copy
	}
	numericRoot, numericConsumers := prospectiveSharedNumericRootForTest(t, family, variants, demands)
	allRoots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, group := range groups {
		maps.Copy(allRoots, group.roots)
	}
	if len(allRoots) != 2 || allRoots[numericRoot] {
		t.Fatal("fixture needs two baseline roots and one distinct shared numeric root")
	}
	allRoots[numericRoot] = true
	for _, roots := range []int{len(allRoots) - 1, len(allRoots)} {
		optional, admitted, err := initialFamilyProspectiveHeaderGroups(family, variants, demands, groups, usage, usage.records+2, maximumBytes, roots)
		if err != nil || admitted != (roots == len(allRoots)) || !admitted && len(optional) != 0 {
			t.Fatalf("root limit %d admission=%t groups=%d error=%v", roots, admitted, len(optional), err)
		}
		if admitted {
			if len(optional) != 1 || !maps.Equal(optional[0].consumers, numericConsumers) {
				t.Fatal("shared numeric root lost a variant/consumer membership")
			}
			requireProspectiveRootSetForTest(t, slices.Collect(maps.Keys(optional[0].roots)), map[ActionPlanFamilyExecutionCutRoot]bool{numericRoot: true})
		}
	}
	if !reflect.DeepEqual(beforeGroups, groups) {
		t.Fatal("transaction modified baseline scheduling groups")
	}
	initial, err := newActionPlanFamilyInitialExecution(variants, demands, usage.records+2, maximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 3, 3)
	requireProspectiveRootSetForTest(t, initial.Cut.Roots(), allRoots)
}

func TestInitialProspectiveNumericRejectsMalformedEvidence(t *testing.T) {
	variants := []ActionPlanFamilyVariant{prospectiveFamilyVariantForTest(t, "0"), prospectiveFamilyVariantForTest(t, "1")}
	for _, mode := range []string{"slot", "owner", "consumer", "path", "disabled", "baseline_truncated", "prospective_truncated"} {
		t.Run(mode, func(t *testing.T) {
			demands := prospectiveFamilyDemandsForTest(t, variants)
			collection := demands[variants[1].Name]
			switch mode {
			case "slot":
				collection.prospective[0].Slot = 999
			case "owner":
				collection.prospective[0].ProducerNodeID = "unknown"
			case "consumer":
				collection.prospective[0].ConsumerNodeID = "unknown"
			case "path":
				collection.prospective[0].Path = "different.h"
			case "disabled":
				collection.Enabled = false
			case "baseline_truncated":
				collection.Truncated = true
			case "prospective_truncated":
				collection.prospectiveTruncated = true
			}
			demands[variants[1].Name] = collection
			if got, err := NewActionPlanFamilyInitialExecution(variants, demands); err == nil || got != nil {
				t.Fatal("malformed prospective evidence was silently treated as an optimization omission")
			}
		})
	}
	family, groups := headerExecutionSelectionFamilyForTest(t)
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups[:1], defaultActionPlanFamilyExecutionCutLimits(), 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(seed.cut.contract)
	bad := *groups[0]
	bad.roots = map[ActionPlanFamilyExecutionCutRoot]bool{}
	for root := range groups[0].roots {
		root.Slot = 999
		bad.roots[root] = true
	}
	if got, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&bad}, defaultActionPlanFamilyExecutionCutLimits(), 128, seed); err == nil || got != nil {
		t.Fatal("non-budget selector error was silently omitted")
	}
	after, _ := json.Marshal(seed.cut.contract)
	if !bytes.Equal(before, after) || seed.attempts != 1 {
		t.Fatal("failed optional trial mutated baseline")
	}
}

func TestInitialProspectiveNumericDoesNotExpandExecutableEligibility(t *testing.T) {
	variant := prospectiveFamilyVariantForTest(t, "0")
	for id := range variant.Snapshot.ConfigDependencies {
		variant.Snapshot.ConfigDependencies[id] = ConfigDependencySet{Opaque: true, Reason: "opaque fixture"}
	}
	variants := []ActionPlanFamilyVariant{variant}
	demands := prospectiveFamilyDemandsForTest(t, variants)
	baseline, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, baseline, 0, 0)
	initial, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, initial, 1, 1)
	for _, node := range initial.Cut.contract.Nodes {
		if node.Node.Outputs[0].Path != "include/generated/numeric.h" {
			t.Fatal("optional consumer expanded baseline executable/sidecar eligibility")
		}
	}
}

func TestInitialProspectiveNumericPreservesRealMetadataSidecarSelection(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		prospectiveFamilyVariantWithNumericForTest(t, sidecarSelectionWithExecutableForTest(t, "0")),
		prospectiveFamilyVariantWithNumericForTest(t, sidecarSelectionWithExecutableForTest(t, "1")),
	}
	demands := prospectiveFamilyDemandsForTest(t, variants)
	baseline, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, baseline, 4, 4)
	metadata := 0
	for _, output := range baseline.Cut.Outputs() {
		if output.Output.Tree == "metadata" && output.Output.ObservedPath != "" {
			metadata++
		}
	}
	if metadata != 2 {
		t.Fatal("fixture did not exercise actual metadata-sidecar selection")
	}
	// Two executable associations plus two numeric associations exceed three;
	// both sidecars still fit their existing independent descriptor budget.
	limited, err := newActionPlanFamilyInitialExecution(variants, demands, 3, MaxActionPlanFamilyExecutionCutBytes)
	if err != nil {
		t.Fatal(err)
	}
	requireSameProspectiveBaselineForTest(t, baseline, limited)
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	var usage actionPlanFamilyDemandUsage
	if overflow, err := addInitialFamilyExecutableGroupsWithUsage(baseline.Family, variants, groups, 0, 0,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, &usage); err != nil || overflow {
		t.Fatalf("%t %v", overflow, err)
	}
	if overflow, err := addInitialFamilySidecarGroups(baseline.Family, variants, groups,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes); err != nil || overflow {
		t.Fatalf("%t %v", overflow, err)
	}
	numericRoot, numericConsumers := prospectiveSharedNumericRootForTest(t, baseline.Family, variants, demands)
	allRoots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, group := range groups {
		maps.Copy(allRoots, group.roots)
	}
	if len(allRoots) != 4 || allRoots[numericRoot] {
		t.Fatal("fixture needs four baseline executable/sidecar roots and one shared numeric root")
	}
	// The baseline alone fills four roots; both variant memberships require
	// only one additional exact producer/slot, not two numeric roots.
	for _, roots := range []int{4, 5} {
		optional, admitted, err := initialFamilyProspectiveHeaderGroups(baseline.Family, variants, demands, groups, usage,
			MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, roots)
		if err != nil || admitted != (roots == 5) || !admitted && len(optional) != 0 {
			t.Fatalf("sidecar root limit %d admission=%t groups=%d error=%v", roots, admitted, len(optional), err)
		}
		if admitted {
			if len(optional) != 1 || !maps.Equal(optional[0].consumers, numericConsumers) {
				t.Fatal("shared numeric root lost a variant/consumer membership")
			}
			requireProspectiveRootSetForTest(t, slices.Collect(maps.Keys(optional[0].roots)), map[ActionPlanFamilyExecutionCutRoot]bool{numericRoot: true})
		}
	}
	allRoots[numericRoot] = true
	full, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, full, 5, 5)
	requireProspectiveRootSetForTest(t, full.Cut.Roots(), allRoots)
	// Now make the same consumers opaque: the existence of numeric scheduling
	// evidence alone must not awaken either metadata-sidecar or executable roots.
	for index := range variants {
		for id := range variants[index].Snapshot.ConfigDependencies {
			variants[index].Snapshot.ConfigDependencies[id] = ConfigDependencySet{Opaque: true, Reason: "opaque numeric consumer"}
		}
	}
	opaqueBaseline, err := NewActionPlanFamilyInitialExecution(variants, selectionEnabledDemands(variants))
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, opaqueBaseline, 0, 0)
	opaqueNumeric, err := NewActionPlanFamilyInitialExecution(variants, demands)
	if err != nil {
		t.Fatal(err)
	}
	selectionRequireValid(t, opaqueNumeric, 1, 1)
	requireProspectiveRootSetForTest(t, opaqueNumeric.Cut.Roots(), map[ActionPlanFamilyExecutionCutRoot]bool{numericRoot: true})
	for _, output := range opaqueNumeric.Cut.Outputs() {
		if output.Output.Path != "include/generated/numeric.h" {
			t.Fatal("optional numeric consumers changed metadata-sidecar eligibility")
		}
	}
}

func TestInitialProspectiveNumericMultiVariantRemainingAttemptBoundary(t *testing.T) {
	variants := []ActionPlanFamilyVariant{prospectiveFamilyVariantForTest(t, "0"), prospectiveFamilyVariantForTest(t, "1")}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	var usage actionPlanFamilyDemandUsage
	if overflow, err := addInitialFamilyExecutableGroupsWithUsage(family, variants, groups, 0, 0,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, &usage); err != nil || overflow {
		t.Fatalf("%t %v", overflow, err)
	}
	demands := prospectiveFamilyDemandsForTest(t, variants)
	numericRoot, numericConsumers := prospectiveSharedNumericRootForTest(t, family, variants, demands)
	optional, admitted, err := initialFamilyProspectiveHeaderGroups(family, variants, demands, groups, usage,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes, MaxActionPlanFamilyExecutionCutOutputs)
	if err != nil || !admitted || len(optional) != 1 || !maps.Equal(optional[0].consumers, numericConsumers) {
		t.Fatal("fixture lacks one shared numeric root with both variant consumers")
	}
	requireProspectiveRootSetForTest(t, slices.Collect(maps.Keys(optional[0].roots)), map[ActionPlanFamilyExecutionCutRoot]bool{numericRoot: true})
	if len(groups) != 1 {
		t.Fatal("fixture lacks one baseline executable group")
	}
	primary := slices.Collect(maps.Values(groups))[0]
	for _, count := range []int{127, 128} {
		baseline := make([]*actionPlanFamilyHeaderDemandGroup, count)
		for index := range baseline {
			copy := *primary
			copy.key = fmt.Sprintf("%03d", index)
			baseline[index] = &copy
		}
		seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, baseline, defaultActionPlanFamilyExecutionCutLimits(), 128, nil)
		if err != nil {
			t.Fatal(err)
		}
		continued, err := selectActionPlanFamilyHeaderExecutionFrom(family, optional, defaultActionPlanFamilyExecutionCutLimits(), 128, seed)
		if err != nil || continued.attempts != 128 || seed.attempts != count {
			t.Fatalf("cross-variant total attempt allowance changed: %v", err)
		}
		want := map[ActionPlanFamilyExecutionCutRoot]bool{}
		for _, root := range seed.cut.Roots() {
			want[root] = true
		}
		if len(want) != 2 || want[numericRoot] {
			t.Fatal("attempt fixture lost its two baseline roots")
		}
		if count == 127 {
			want[numericRoot] = true
			if !maps.Equal(continued.consumers, numericConsumers) {
				t.Fatal("remaining attempt lost a variant/consumer membership")
			}
		}
		requireProspectiveRootSetForTest(t, continued.cut.Roots(), want)
		if count == 128 && continued.cut != seed.cut {
			t.Fatal("exhausted attempt allowance replaced the baseline cut")
		}
	}
}

func TestInitialProspectiveNumericSharesAttemptBudgetAndProtectsBaseline(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	limits := defaultActionPlanFamilyExecutionCutLimits()
	baseline := make([]*actionPlanFamilyHeaderDemandGroup, maxActionPlanFamilyHeaderExecutionAttempts)
	for index := range baseline {
		copy := *groups[0]
		copy.key = fmt.Sprintf("baseline-%03d", index)
		baseline[index] = &copy
	}
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, baseline, limits, maxActionPlanFamilyHeaderExecutionAttempts, nil)
	if err != nil || seed.attempts != maxActionPlanFamilyHeaderExecutionAttempts {
		t.Fatalf("attempt seed=%#v %v", seed, err)
	}
	continued, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups[1:], limits, maxActionPlanFamilyHeaderExecutionAttempts, seed)
	if err != nil || continued.cut != seed.cut || continued.attempts != seed.attempts {
		t.Fatalf("optional phase reset attempt budget: %v", err)
	}
	seed, err = selectActionPlanFamilyHeaderExecutionFrom(family, groups[:1], limits, maxActionPlanFamilyHeaderExecutionAttempts, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Inflate the image-derived group's distinct instances so the original
	// policy would accept its larger net benefit despite pinning earlier users.
	// The prospective phase must protect every outside-cut baseline beneficiary.
	late := *groups[1]
	late.consumers = maps.Clone(late.consumers)
	id := slices.Collect(maps.Values(late.consumers))[0]
	for index := 0; index < 5; index++ {
		late.consumers[fmt.Sprintf("additional-%d", index)] = id
	}
	unseeded, err := selectActionPlanFamilyHeaderExecution(family, []*actionPlanFamilyHeaderDemandGroup{&late}, limits, 128)
	if err != nil || len(unseeded.NodeIDs()) <= len(seed.cut.NodeIDs()) {
		t.Fatalf("fixture lacks beneficial pinning contrast: %v", err)
	}
	before, _ := json.Marshal(seed.cut.contract)
	continued, err = selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&late}, limits, 128, seed)
	after, _ := json.Marshal(seed.cut.contract)
	if err != nil || continued.cut != seed.cut || !bytes.Equal(before, after) || seed.attempts != 1 || continued.attempts != 2 {
		t.Fatalf("optional group displaced or mutated baseline: %v", err)
	}
}

func TestProspectiveNumericDiagnosticsKeepBaselineFrontierDistinct(t *testing.T) {
	plan, sets, baseline := observationDiagnosticFixtureForTest()
	first := observationDiagnosticBuilderForTest()
	first.addVariant("base", plan, sets, baseline)
	want := first.finish().Variants[0]
	for _, omitted := range []bool{false, true} {
		collection := baseline
		collection.prospectiveTruncated = omitted
		if !omitted {
			collection.prospective = slices.Clone(baseline.Demands)
		}
		d := observationDiagnosticBuilderForTest()
		d.addVariant("base", plan, sets, collection)
		got := d.finish().Variants[0]
		if got.ProspectiveCollectionOmitted != omitted || got.ProspectiveDemandRecords != len(collection.prospective) {
			t.Fatal("lost separate tier status")
		}
		if !omitted && got.ObservedProspectiveDemandRecords != 3 {
			t.Fatal("lost exact already-observed numeric slots")
		}
		got.ProspectiveCollectionOmitted, got.ProspectiveDemandRecords, got.ObservedProspectiveDemandRecords = false, 0, 0
		if !reflect.DeepEqual(got, want) {
			t.Fatal("prospective telemetry changed baseline frontier")
		}
	}
}

func TestNumericSelectionAllowsHelperCompilerForExistingBeneficiaries(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyWithPrerequisitesForTest(t, 1)
	limits := defaultActionPlanFamilyExecutionCutLimits()
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups[:1], limits, 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The early header benefits S, C1, C2. S generates the numeric header
	// needed by C1 and C2. Its selection must be useful even though the old
	// distinct-beneficiary metric drops from three to two.
	numeric := *groups[1]
	numeric.numeric = true
	numeric.consumers = maps.Clone(groups[0].consumers)
	before, _ := json.Marshal(seed.cut.contract)
	got, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, seed)
	if err != nil {
		t.Fatal(err)
	}
	if got.benefit != 2 || seed.benefit != 3 || len(got.cut.Roots()) != 2 || len(got.cut.NodeIDs()) != 3 {
		t.Fatalf("numeric helper was not selected: benefits %d/%d, roots %v, nodes %v", seed.benefit, got.benefit, got.cut.Roots(), got.cut.NodeIDs())
	}
	after, _ := json.Marshal(seed.cut.contract)
	if !bytes.Equal(before, after) || seed.attempts != 1 || got.attempts != 2 {
		t.Fatal("numeric selection mutated the baseline or reset its attempt budget")
	}
	if _, err := got.cut.Verify(family); err != nil {
		t.Fatal(err)
	}
	// The helper is still subject to the complete closure budget, not just a
	// root-count check or a count of the selected header's immediate inputs.
	limits.nodes = 2
	bounded, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, seed)
	if err != nil || bounded.cut != seed.cut {
		t.Fatalf("numeric helper bypassed the closure bound: %v", err)
	}
}

func TestNumericSelectionRejectsImageClosureAndAliasedBenefit(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyForTest(t)
	limits := defaultActionPlanFamilyExecutionCutLimits()
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups[:1], limits, 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	numeric := *groups[1]
	numeric.numeric = true
	numeric.consumers = maps.Clone(numeric.consumers)
	id := slices.Collect(maps.Values(numeric.consumers))[0]
	for index := 0; index < 100; index++ {
		numeric.consumers[fmt.Sprintf("alias-%d", index)] = id
	}
	got, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, seed)
	if err != nil || got.cut != seed.cut {
		t.Fatalf("one remaining compiler paid for three pinned compilers through aliases: %v", err)
	}
}

func TestNumericSelectionAllowsOneHelperPerConsumerButNotZeroBenefit(t *testing.T) {
	family, groups := headerExecutionSelectionFamilyWithPrerequisitesForTest(t, 1)
	limits := defaultActionPlanFamilyExecutionCutLimits()
	seed, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups[:1], limits, 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	numeric := *groups[1]
	numeric.numeric = true
	got, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, seed)
	if err != nil || len(got.cut.Roots()) != 2 || len(got.cut.NodeIDs()) != 3 {
		t.Fatalf("one helper per remaining consumer was rejected: %v", err)
	}
	// Repeating a root offers no additional opportunity, and every useful
	// consumer being in the closure is not a zero-cost scheduling win.
	again, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, got)
	if err != nil || again.cut != got.cut {
		t.Fatalf("repeated numeric root was selected again: %v", err)
	}
	numeric.consumers = nil
	empty, err := selectActionPlanFamilyHeaderExecutionFrom(family, []*actionPlanFamilyHeaderDemandGroup{&numeric}, limits, 128, seed)
	if err != nil || empty.cut != seed.cut {
		t.Fatalf("numeric root with no remaining consumer was selected: %v", err)
	}
}
