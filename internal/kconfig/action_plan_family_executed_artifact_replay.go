package kconfig

// Accept only retained production planning results, never caller-authored
// dependency maps or diagnostic observed-use records.
import (
	"crypto/sha256"
	"fmt"
	"maps"
)

func substitutePlannedExecutedArtifacts(result *ActionPlanFamilyVariantPlanningResult, observed *ActionPlanFamilyExecutedArtifacts) (*ActionPlan, map[string]ConfigDependencySet, error) {
	if result == nil || result.Plan == nil || result.analysis == nil || result.cut == nil || result.variant == "" || observed == nil {
		return nil, nil, fmt.Errorf("executed artifacts require authenticated variant planning results")
	}
	if _, err := result.cut.CanonicalJSON(); err != nil {
		return nil, nil, err
	}
	if result.cut.ID() != observed.cutID {
		return nil, nil, fmt.Errorf("executed artifacts refer to a different planning execution")
	}
	contract, err := canonicalFamilyReplayPlan(result.Plan, result.replayConfigFiles)
	if err != nil {
		return nil, nil, err
	}
	if sha256.Sum256(contract) != result.replayContractDigest {
		return nil, nil, fmt.Errorf("executed artifacts lost the sealed analyzed replay contract")
	}
	dependencies, err := result.analysis.ByNodeID(result.Plan)
	if err != nil {
		return nil, nil, err
	}
	// The production result is already finalized and content-addressed. Its
	// private digest authenticates the scanner's append-only catalog against
	// the original replay. Rebind only the original cut's exact node origins;
	// neither public annotations nor path-based matching can add an execution.
	replay := &ActionPlanFamilyVerifiedReplay{
		cutID: result.cut.ID(), variant: result.variant, plan: result.Plan,
		originalIDs: map[string]string{}, executedIDs: map[string]string{}, opaqueNodes: map[string]bool{},
		configFiles: maps.Clone(result.replayConfigFiles), contractDigest: result.replayContractDigest,
		sourceCount: len(result.Plan.Sources),
	}
	for _, node := range result.Plan.Nodes {
		if _, duplicate := replay.originalIDs[node.ID]; duplicate {
			return nil, nil, fmt.Errorf("executed artifact planning result repeats a node")
		}
		replay.originalIDs[node.ID] = node.ID
	}
	knownVariant := false
	for _, name := range result.cut.contract.Variants {
		knownVariant = knownVariant || name == result.variant
	}
	if !knownVariant {
		return nil, nil, fmt.Errorf("executed artifacts have no original variant")
	}
	for _, origin := range result.cut.Origins() {
		if origin.Variant != result.variant {
			continue
		}
		if replay.originalIDs[origin.OriginalNodeID] == "" {
			return nil, nil, fmt.Errorf("executed artifact planning result lost an original producer")
		}
		if previous := replay.executedIDs[origin.OriginalNodeID]; previous != "" && previous != origin.NodeID {
			return nil, nil, fmt.Errorf("executed artifact planning result has ambiguous origins")
		}
		if !dependencies[origin.OriginalNodeID].Opaque {
			return nil, nil, fmt.Errorf("executed artifact planning result pruned a pinned producer")
		}
		replay.executedIDs[origin.OriginalNodeID] = origin.NodeID
		replay.opaqueNodes[origin.OriginalNodeID] = true
	}
	return substituteExecutedArtifacts(result.Plan, dependencies, replay, observed)
}
