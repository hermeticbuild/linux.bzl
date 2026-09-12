package kconfig

import (
	"cmp"
	"slices"
	"strings"
)

// ActionPlanFamilyObservedHeaderFrontier is non-authoritative replay telemetry.
// It describes encountered stops, not the complete transitive header closure.
// Neither these records nor their absence authorize execution or precision.
type ActionPlanFamilyObservedHeaderFrontier struct {
	DetailsTruncated bool                                    `json:"details_truncated"`
	Variants         []ActionPlanFamilyObservedHeaderVariant `json:"variants"`
}

type ActionPlanFamilyObservedHeaderVariant struct {
	Variant                                  string                               `json:"variant"`
	CollectionEnabled                        bool                                 `json:"collection_enabled"`
	CollectionTruncated                      bool                                 `json:"collection_truncated"`
	CompileNodes                             int                                  `json:"compile_nodes"`
	PreciseCompileNodes                      int                                  `json:"precise_compile_nodes"`
	OpaqueCompileNodes                       int                                  `json:"opaque_compile_nodes"`
	PinnedCompileNodes                       int                                  `json:"pinned_compile_nodes"`
	OpaqueCompileNodesWithDemands            int                                  `json:"opaque_compile_nodes_with_demands"`
	OpaqueCompileNodesWithoutRecordedDemands int                                  `json:"opaque_compile_nodes_without_recorded_demands"`
	DemandRecords                            int                                  `json:"demand_records"`
	DemandedConsumers                        int                                  `json:"demanded_consumers"`
	DemandedOutputs                          int                                  `json:"demanded_outputs"`
	ObservedDemandRecords                    int                                  `json:"observed_demand_records"`
	ExecutedUnobservedDemandRecords          int                                  `json:"executed_unobserved_demand_records"`
	UnexecutedDemandRecords                  int                                  `json:"unexecuted_demand_records"`
	Outputs                                  []ActionPlanFamilyHeaderDemandOutput `json:"outputs,omitempty"`

	// These are the current replay's separately admitted local tier, not a
	// claim that late demands changed the already frozen initial cut.
	ProspectiveDemandRecords         int  `json:"prospective_demand_records"`
	ProspectiveCollectionOmitted     bool `json:"prospective_collection_omitted"`
	ObservedProspectiveDemandRecords int  `json:"observed_prospective_demand_records"`
}

// IDs here are exact original variant-plan identities, not reduced family IDs.
// Executed distinguishes cheap, already-produced slots from a larger cut;
// Observed distinguishes a repeated conservative stop from missing bytes.
type ActionPlanFamilyHeaderDemandOutput struct {
	ProducerNodeID string                                `json:"producer_node_id"`
	Slot           int                                   `json:"slot"`
	Tree           string                                `json:"tree"`
	Path           string                                `json:"path"`
	ArtifactPath   string                                `json:"artifact_path"`
	Executed       bool                                  `json:"executed"`
	Observed       bool                                  `json:"observed"`
	ConsumerCount  int                                   `json:"consumer_count"`
	Bindings       []ActionPlanFamilyHeaderDemandBinding `json:"bindings"`
}

type ActionPlanFamilyHeaderDemandBinding struct {
	ConsumerNodeID string `json:"consumer_node_id"`
	LogicalPath    string `json:"logical_path"`
}

type actionPlanFamilyObservationDiagnostics struct {
	report                       ActionPlanFamilyObservedHeaderFrontier
	executed                     map[string]map[string]string
	observed                     map[configDependencyObservedOutputKey]bool
	records, bytes               int
	maximumRecords, maximumBytes int
}

func newActionPlanFamilyObservationDiagnostics(cut *ActionPlanFamilyExecutionCut, observed *ActionPlanFamilyObservedHeaders) *actionPlanFamilyObservationDiagnostics {
	d := &actionPlanFamilyObservationDiagnostics{
		executed: map[string]map[string]string{}, observed: map[configDependencyObservedOutputKey]bool{},
		maximumRecords: 16384, maximumBytes: 8 << 20,
	}
	for _, origin := range cut.contract.Origins {
		if d.executed[origin.Variant] == nil {
			d.executed[origin.Variant] = map[string]string{}
		}
		d.executed[origin.Variant][origin.OriginalNodeID] = origin.NodeID
	}
	for _, header := range observed.headers {
		d.observed[configDependencyObservedOutputKey{header.NodeID, header.Slot}] = true
	}
	return d
}

func (d *actionPlanFamilyObservationDiagnostics) addVariant(name string, plan *ActionPlan, dependencies map[string]ConfigDependencySet, collection ConfigDependencyGeneratedHeaderDemandCollection) {
	variant := ActionPlanFamilyObservedHeaderVariant{
		Variant: strings.Clone(name), CollectionEnabled: collection.Enabled, CollectionTruncated: collection.Truncated,
		DemandRecords:            len(collection.Demands),
		ProspectiveDemandRecords: len(collection.prospective), ProspectiveCollectionOmitted: collection.prospectiveTruncated,
	}
	for _, demand := range collection.prospective {
		executed := d.executed[name][demand.ProducerNodeID]
		if d.observed[configDependencyObservedOutputKey{executed, demand.Slot}] {
			variant.ObservedProspectiveDemandRecords++
		}
	}
	consumers := map[string]bool{}
	outputs := map[configDependencyObservedOutputKey]int{}
	bindings := map[configDependencyObservedOutputKey]map[string]bool{}
	for _, demand := range collection.Demands {
		consumers[demand.ConsumerNodeID] = true
		key := configDependencyObservedOutputKey{demand.ProducerNodeID, demand.Slot}
		executed := d.executed[name][demand.ProducerNodeID]
		observed := d.observed[configDependencyObservedOutputKey{executed, demand.Slot}]
		switch {
		case observed:
			variant.ObservedDemandRecords++
		case executed != "":
			variant.ExecutedUnobservedDemandRecords++
		default:
			variant.UnexecutedDemandRecords++
		}
		index, found := outputs[key]
		if !found {
			variant.DemandedOutputs++
			outputs[key] = len(variant.Outputs)
		}
		if d.report.DetailsTruncated {
			continue
		}
		size := 256 + len(demand.ConsumerNodeID) + len(demand.ProducerNodeID) + len(demand.Tree) +
			len(demand.Path) + len(demand.ArtifactPath) + len(demand.LogicalPath)
		if d.records >= d.maximumRecords || size > d.maximumBytes-d.bytes {
			// Drop every detail, rather than publishing a traversal-order prefix.
			// Aggregate counts and collector status still describe every variant.
			d.report.DetailsTruncated = true
			for index := range d.report.Variants {
				d.report.Variants[index].Outputs = nil
			}
			variant.Outputs, bindings = nil, nil
			continue
		}
		d.records++
		d.bytes += size
		if !found {
			index = len(variant.Outputs)
			variant.Outputs = append(variant.Outputs, ActionPlanFamilyHeaderDemandOutput{
				ProducerNodeID: strings.Clone(demand.ProducerNodeID), Slot: demand.Slot,
				Tree: strings.Clone(demand.Tree), Path: strings.Clone(demand.Path), ArtifactPath: strings.Clone(demand.ArtifactPath),
				Executed: executed != "", Observed: observed,
			})
			bindings[key] = map[string]bool{}
		}
		output := &variant.Outputs[index]
		if !bindings[key][demand.ConsumerNodeID] {
			bindings[key][demand.ConsumerNodeID] = true
			output.ConsumerCount++
		}
		output.Bindings = append(output.Bindings, ActionPlanFamilyHeaderDemandBinding{
			ConsumerNodeID: strings.Clone(demand.ConsumerNodeID), LogicalPath: strings.Clone(demand.LogicalPath),
		})
	}
	variant.DemandedConsumers = len(consumers)
	for _, node := range plan.Nodes {
		if node.Kind != "compile" {
			continue
		}
		variant.CompileNodes++
		if d.executed[name][node.ID] != "" {
			variant.PinnedCompileNodes++
		}
		if !dependencies[node.ID].Opaque {
			variant.PreciseCompileNodes++
		} else {
			variant.OpaqueCompileNodes++
			if consumers[node.ID] {
				variant.OpaqueCompileNodesWithDemands++
			} else {
				// Not proof of another cause if collection is disabled/truncated.
				// Existing opaque_reasons contains the actual conservative reasons.
				variant.OpaqueCompileNodesWithoutRecordedDemands++
			}
		}
	}
	for index := range variant.Outputs {
		slices.SortFunc(variant.Outputs[index].Bindings, func(a, b ActionPlanFamilyHeaderDemandBinding) int {
			if order := strings.Compare(a.ConsumerNodeID, b.ConsumerNodeID); order != 0 {
				return order
			}
			return strings.Compare(a.LogicalPath, b.LogicalPath)
		})
	}
	slices.SortFunc(variant.Outputs, func(a, b ActionPlanFamilyHeaderDemandOutput) int {
		if order := strings.Compare(a.ProducerNodeID, b.ProducerNodeID); order != 0 {
			return order
		}
		return cmp.Compare(a.Slot, b.Slot)
	})
	d.report.Variants = append(d.report.Variants, variant)
}

func (d *actionPlanFamilyObservationDiagnostics) finish() *ActionPlanFamilyObservedHeaderFrontier {
	slices.SortFunc(d.report.Variants, func(a, b ActionPlanFamilyObservedHeaderVariant) int { return strings.Compare(a.Variant, b.Variant) })
	return cloneActionPlanFamilyObservedHeaderFrontier(&d.report)
}

func cloneActionPlanFamilyObservedHeaderFrontier(value *ActionPlanFamilyObservedHeaderFrontier) *ActionPlanFamilyObservedHeaderFrontier {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Variants = slices.Clone(value.Variants)
	for index := range copy.Variants {
		copy.Variants[index].Outputs = slices.Clone(value.Variants[index].Outputs)
		for output := range copy.Variants[index].Outputs {
			copy.Variants[index].Outputs[output].Bindings = slices.Clone(value.Variants[index].Outputs[output].Bindings)
		}
	}
	return &copy
}
