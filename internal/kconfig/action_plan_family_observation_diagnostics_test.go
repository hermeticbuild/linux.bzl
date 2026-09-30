package kconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func observationDiagnosticFixtureForTest() (*ActionPlan, map[string]ConfigDependencySet, ConfigDependencyGeneratedHeaderDemandCollection) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "precise", Kind: "compile"}, {ID: "blocked", Kind: "compile"},
		{ID: "other", Kind: "compile"}, {ID: "pinned", Kind: "compile"}, {ID: "generator", Kind: "generate"},
	}}
	sets := map[string]ConfigDependencySet{
		"precise": {}, "blocked": {Opaque: true}, "other": {Opaque: true}, "pinned": {Opaque: true}, "generator": {Opaque: true},
	}
	collection := ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true}
	for _, request := range []struct {
		producer, consumer, path string
		slot                     int
	}{
		{"observed", "blocked", "header.h", 0}, {"observed", "blocked", "alias.h", 0},
		{"observed", "other", "header.h", 0}, {"executed", "blocked", "second.h", 1}, {"new", "blocked", "nested.h", 0},
	} {
		collection.Demands = append(collection.Demands, ConfigDependencyGeneratedHeaderDemand{
			ConsumerNodeID: request.consumer, ProducerNodeID: request.producer, Slot: request.slot,
			Tree: "objects", Path: request.producer + ".h", ArtifactPath: request.producer + ".h", LogicalPath: request.path,
		})
	}
	return plan, sets, collection
}

func observationDiagnosticBuilderForTest() *actionPlanFamilyObservationDiagnostics {
	return newActionPlanFamilyObservationDiagnostics(&ActionPlanFamilyExecutionCut{contract: actionPlanFamilyExecutionCutContract{
		Origins: []ActionPlanFamilyExecutionCutOrigin{
			{Variant: "base", OriginalNodeID: "observed", NodeID: "executed-observed"},
			{Variant: "base", OriginalNodeID: "executed", NodeID: "executed-other"},
			{Variant: "base", OriginalNodeID: "pinned", NodeID: "executed-compiler"},
		},
	}}, &ActionPlanFamilyObservedHeaders{headers: []ActionPlanFamilyObservedHeader{{NodeID: "executed-observed", Slot: 0}}})
}

func TestFamilyObservationDiagnosticsExactGroupingAndClassification(t *testing.T) {
	plan, sets, demands := observationDiagnosticFixtureForTest()
	var first *ActionPlanFamilyObservedHeaderFrontier
	for _, reverse := range []bool{false, true} {
		if reverse {
			slices.Reverse(demands.Demands)
			slices.Reverse(plan.Nodes)
		}
		d := observationDiagnosticBuilderForTest()
		d.addVariant("base", plan, sets, demands)
		got := d.finish()
		if first != nil && !reflect.DeepEqual(first, got) {
			t.Fatal("diagnostic order depends on traversal")
		}
		first = got
		v := got.Variants[0]
		if !v.CollectionEnabled || v.CollectionTruncated || v.CompileNodes != 4 || v.PreciseCompileNodes != 1 ||
			v.OpaqueCompileNodes != 3 || v.PinnedCompileNodes != 1 || v.OpaqueCompileNodesWithDemands != 2 ||
			v.OpaqueCompileNodesWithoutRecordedDemands != 1 || v.DemandRecords != 5 || v.DemandedOutputs != 3 ||
			v.DemandedConsumers != 2 || v.ObservedDemandRecords != 3 || v.ExecutedUnobservedDemandRecords != 1 || v.UnexecutedDemandRecords != 1 {
			t.Fatalf("incorrect frontier classification: %#v", v)
		}
		output := v.Outputs[2]
		if output.ProducerNodeID != "observed" || output.Slot != 0 || !output.Executed || !output.Observed ||
			output.ConsumerCount != 2 || len(output.Bindings) != 3 || output.Bindings[0].LogicalPath != "alias.h" {
			t.Fatalf("lost exact alias/consumer grouping: %#v", output)
		}
	}
}

func TestFamilyObservationDiagnosticsBoundsAndIncompleteCollection(t *testing.T) {
	plan, sets, demands := observationDiagnosticFixtureForTest()
	for _, limit := range []string{"records", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			d := observationDiagnosticBuilderForTest()
			if limit == "records" {
				d.maximumRecords = 1
			} else {
				d.maximumBytes = 1
			}
			d.addVariant("base", plan, sets, demands)
			d.addVariant("later", plan, sets, demands)
			got := d.finish()
			if !got.DetailsTruncated || len(got.Variants) != 2 {
				t.Fatal(got)
			}
			for _, variant := range got.Variants {
				if len(variant.Outputs) != 0 || variant.DemandRecords != 5 || variant.DemandedOutputs != 3 || variant.CompileNodes != 4 {
					t.Fatalf("budget silently lost totals or retained partial details: %#v", variant)
				}
			}
		})
	}
	for _, collection := range []ConfigDependencyGeneratedHeaderDemandCollection{{}, {Enabled: true, Truncated: true}} {
		d := observationDiagnosticBuilderForTest()
		d.addVariant("base", plan, sets, collection)
		got := d.finish().Variants[0]
		if got.CollectionEnabled != collection.Enabled || got.CollectionTruncated != collection.Truncated ||
			got.OpaqueCompileNodesWithoutRecordedDemands != 3 || got.DemandRecords != 0 {
			t.Fatalf("incomplete collection looked complete: %#v", got)
		}
	}
}

func TestFamilyObservationDiagnosticsReportOnlyAndDefensive(t *testing.T) {
	options := familyVariantReplayOptionsForTest(t)
	result, err := familyVariantMetadataForTest(t, nil).ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, options)
	if err != nil {
		t.Fatal(err)
	}
	// Public diagnostics are never the source of report evidence, either.
	result.GeneratedHeaderDemands = ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true, Truncated: true}
	result.Dependencies = map[string]ConfigDependencySet{"forged": {Opaque: true}}
	validated, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result})
	if err != nil {
		t.Fatal(err)
	}
	report, err := validated.family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservedHeaderFrontier == nil || len(report.ObservedHeaderFrontier.Variants) != 1 {
		t.Fatal(report)
	}
	variant := report.ObservedHeaderFrontier.Variants[0]
	if !variant.CollectionEnabled || variant.CollectionTruncated || variant.DemandRecords != 0 || variant.PreciseCompileNodes == 0 {
		t.Fatalf("report trusted public evidence or lost private scan: %#v", variant)
	}
	before, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	report.ObservedHeaderFrontier.Variants[0].Variant = "caller mutation"
	again, err := validated.family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(again)
	if err != nil || string(before) != string(after) {
		t.Fatal("report aliases retained diagnostic evidence")
	}
	// The optional field cannot alter execution validation or ordinary reports.
	validated.family.observedHeaderFrontier = nil
	ordinary, err := validated.family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ordinary)
	if err != nil || strings.Contains(string(encoded), "observed_header_frontier") {
		t.Fatal("disabled field was serialized")
	}
	if !reflect.DeepEqual(ordinary.Nodes, again.Nodes) || !reflect.DeepEqual(ordinary.Pairs, again.Pairs) {
		t.Fatal("diagnostics changed reuse identity")
	}
	plan, sets, demands := observationDiagnosticFixtureForTest()
	d := observationDiagnosticBuilderForTest()
	d.addVariant("base", plan, sets, demands)
	first := d.finish()
	first.Variants[0].Outputs[0].Bindings[0].LogicalPath = "mutated"
	if d.finish().Variants[0].Outputs[0].Bindings[0].LogicalPath == "mutated" {
		t.Fatal("nested binding slice aliases builder")
	}
}
