package kconfig

import (
	"fmt"
	"maps"
	"slices"
)

// Admit scheduling-only records transactionally across the complete family,
// after baseline executable accounting and sidecar discovery. Budget rejection leaves
// every baseline group and the already selected contract untouched. Sidecars
// retain their existing independent descriptor budget; their roots still count.
// No optional consumer is passed back through executable/sidecar discovery.
func initialFamilyProspectiveHeaderGroups(
	family *ActionPlanFamily, variants []ActionPlanFamilyVariant,
	demands map[string]ConfigDependencyGeneratedHeaderDemandCollection,
	baseline map[string]*actionPlanFamilyHeaderDemandGroup, usage actionPlanFamilyDemandUsage,
	maximumRecords, maximumBytes, maximumRoots int,
) ([]*actionPlanFamilyHeaderDemandGroup, bool, error) {
	if maximumRecords <= 0 || maximumBytes <= 0 || maximumRoots <= 0 {
		return nil, false, fmt.Errorf("initial prospective header selection requires valid remaining limits")
	}
	if usage.records > maximumRecords || usage.bytes > maximumBytes {
		return nil, false, nil
	}
	optionalRecords := 0
	for _, name := range family.Variants {
		collection := demands[name]
		if collection.prospectiveTruncated || len(collection.prospective) > maximumRecords-usage.records {
			return nil, false, nil
		}
		usage.records += len(collection.prospective)
		optionalRecords += len(collection.prospective)
		for _, demand := range collection.prospective {
			size := configDependencyGeneratedHeaderDemandBytes(demand)
			if size > maximumBytes-usage.bytes {
				return nil, false, nil
			}
			usage.bytes += size
		}
	}
	if optionalRecords == 0 {
		return nil, true, nil
	}
	finalNodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		finalNodes[node.ID] = node
	}
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, group := range baseline {
		maps.Copy(roots, group.roots)
	}
	if len(roots) > maximumRoots {
		return nil, false, nil
	}
	configPaths := map[string]bool{}
	for _, projection := range resolvedConfigProjections() {
		configPaths[projection.output] = true
	}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	for _, variant := range variants {
		originals := make(map[string]ActionPlanNode, len(variant.Snapshot.Nodes))
		for _, node := range variant.Snapshot.Nodes {
			originals[node.ID] = node
		}
		originalIDs := family.originalNodeIDs[variant.Name]
		for _, demand := range demands[variant.Name].prospective {
			if err := validateInitialFamilyHeaderDemand(demand, originals); err != nil {
				return nil, false, fmt.Errorf("initial prospective header demand for %s: %w", variant.Name, err)
			}
			if configPaths[demand.Path] || configPaths[demand.ArtifactPath] || configPaths[demand.LogicalPath] {
				continue
			}
			consumerID, producerID := originalIDs[demand.ConsumerNodeID], originalIDs[demand.ProducerNodeID]
			if consumerID == "" || producerID == "" {
				continue
			}
			node, found := finalNodes[producerID]
			if !found || demand.Slot >= len(node.Outputs) {
				return nil, false, fmt.Errorf("initial prospective header demand lost reduced output %s[%d]", producerID, demand.Slot)
			}
			output := originals[demand.ProducerNodeID].Outputs[demand.Slot]
			if err := validateInitialFamilyHeaderOutput(output, node.Outputs[demand.Slot], demand.Slot); err != nil {
				return nil, false, fmt.Errorf("initial prospective header demand %s/%s[%d]: %w", variant.Name, producerID, demand.Slot, err)
			}
			root := ActionPlanFamilyExecutionCutRoot{NodeID: producerID, Slot: demand.Slot}
			if !roots[root] && len(roots) >= maximumRoots {
				return nil, false, nil
			}
			roots[root] = true
			key := demand.Tree + "\x00" + demand.Path
			group := groups[key]
			if group == nil {
				group = &actionPlanFamilyHeaderDemandGroup{key: key, numeric: true,
					roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
				groups[key] = group
			}
			group.roots[root] = true
			group.consumers[variant.Name+"\x00"+demand.ConsumerNodeID] = consumerID
		}
	}
	return slices.Collect(maps.Values(groups)), true, nil
}

// Distinct-consumer union size cannot value a second prerequisite for the same
// compiler. In particular, compiling a small offsets generator would reduce
// that union even when its header is needed by thousands of remaining actions.
// Count this group's unique outside-cut compiler opportunities against ALL new
// compiler actions pinned by its complete closure. Aliased memberships cannot
// inflate benefit. A linked-image-derived header which requires the kernel's
// compilers therefore cannot buy its way into the cut with those same users.
// Permit a one-for-one trade: the helper was already required by the build,
// and materializing it can recover sharing of its otherwise opaque consumer.
// This is only scheduling policy; neither a bound path nor this score proves a
// read, precision, equal output bytes, or eventual reuse.
func numericHeaderExecutionBenefitsConsumers(
	group *actionPlanFamilyHeaderDemandGroup,
	selected, trial *ActionPlanFamilyExecutionCut,
	retainedRoots map[ActionPlanFamilyExecutionCutRoot]bool,
) bool {
	addsRoot := false
	for root := range group.roots {
		addsRoot = addsRoot || !retainedRoots[root]
	}
	if !addsRoot {
		return false
	}
	previous := map[string]bool{}
	for _, node := range selected.contract.Nodes {
		previous[node.Node.ID] = true
	}
	pinned := map[string]bool{}
	cost := 0
	for _, node := range trial.contract.Nodes {
		pinned[node.Node.ID] = true
		if !previous[node.Node.ID] && node.Node.Kind == "compile" {
			cost++
		}
	}
	opportunities := map[string]bool{}
	for _, id := range group.consumers {
		if !pinned[id] {
			opportunities[id] = true
		}
	}
	return len(opportunities) != 0 && len(opportunities) >= cost
}
