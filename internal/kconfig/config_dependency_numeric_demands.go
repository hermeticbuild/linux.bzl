package kconfig

import (
	"maps"
	"slices"
)

// Prospective numeric headers retain their own tier after local admission.
// Family-wide admission must still reserve every baseline header/executable
// record, root and selector attempt before considering this optional stage.
func (c *configDependencyGeneratedHeaderDemandCollector) acceptsProspective(
	prospective *configDependencyGeneratedHeaderDemandCollector,
) bool {
	if c == nil || c.truncated || prospective == nil || prospective.truncated {
		return false
	}
	maximumRecords, maximumBytes := c.maximumRecords, c.maximumBytes
	if maximumRecords <= 0 {
		maximumRecords = MaxActionPlanFamilyExecutionCutRecords
	}
	if maximumBytes <= 0 {
		maximumBytes = MaxActionPlanFamilyExecutionCutBytes
	}
	count, size := len(c.records), c.bytes
	for demand := range prospective.records {
		if _, exists := c.records[demand]; exists {
			continue
		}
		addition := configDependencyGeneratedHeaderDemandBytes(demand)
		if count >= maximumRecords || addition > maximumBytes-size {
			return false
		}
		count++
		size += addition
	}
	return true
}

func configDependencyGeneratedHeaderDemandBytes(demand ConfigDependencyGeneratedHeaderDemand) int {
	return 256 + len(demand.ConsumerNodeID) + len(demand.ProducerNodeID) +
		len(demand.Tree) + len(demand.Path) + len(demand.ArtifactPath) + len(demand.LogicalPath)
}

// An opaque compiler may stop before reaching any generated header. Collect
// its exact bound numeric candidates independently of scanner progress: these
// are scheduling opportunities, NOT reads or permission to continue expansion.
// The caller supplies the projection made with the original configured helper
// contract. No same-path or prior-stage tree fallback is used here.
func (c *configDependencyAnalysisContext) recordProspectiveNumericHeaderDemands(
	plan *ActionPlan, profile CompactKbuildProfile, selection compactKbuildSelectionKey,
	consumer ActionPlanNode, recipe ActionRecipe, generated map[string]configDependencyGeneratedText,
) {
	if c.prospectiveHeaderDemands == nil || c.prospectiveHeaderDemands.truncated {
		return
	}
	for _, logical := range slices.Sorted(maps.Keys(generated)) {
		projection := generated[logical]
		if !projection.macroTable || projection.producerID == "" ||
			configDependencyCompletedResolvedProjection(logical) {
			continue
		}
		exact, found := c.generatedOutputProjection(plan, profile, projection.producerID, projection.slot)
		if !found || exact != projection {
			continue
		}
		bound, explicitlyBound, err := lookupActionPlanConfigDependencyWorkInputProvenance(plan, consumer, logical)
		if err != nil {
			continue
		}
		if !explicitlyBound {
			direct := false
			for _, pathname := range recipe.WorkingInputs {
				direct = direct || canonicalKbuildRulePath(pathname) == logical
			}
			if !direct {
				continue
			}
		}
		c.recordGeneratedHeaderDemand(plan, profile, selection, consumer, recipe,
			configDependencyUnavailableGeneratedHeader{
				logical: logical, bound: bound, explicitlyBound: explicitlyBound,
			}, c.prospectiveHeaderDemands, &projection)
	}
}
