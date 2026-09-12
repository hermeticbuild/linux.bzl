package kconfig

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
)

// ActionPlanFamilyVerifiedReplay authenticates the original, still-unpruned
// variant plan before observed contents can influence dependency analysis.
// It retains provisional-to-snapshot identity only for this exact plan; it is
// not permission to emit a final graph, which still requires cut.Verify.
//
// Filesystem/tool payload identity is supplied by the Bazel action inputs:
// initial execution and replay must depend on the same declared immutable
// source, config, toolset and probe artifacts. Snapshot descriptors do not hash
// arbitrary external file contents or authenticate a caller's filesystem.
type ActionPlanFamilyVerifiedReplay struct {
	cutID          string
	variant        string
	plan           *ActionPlan
	originalIDs    map[string]string
	executedIDs    map[string]string
	opaqueNodes    map[string]bool
	configFiles    map[string]string
	contractDigest [sha256.Size]byte
	sourceCount    int
}

func canonicalFamilyReplayPlan(plan *ActionPlan, files map[string]string) ([]byte, error) {
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "execution-cut replay contract"}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, files)
	if err != nil {
		return nil, err
	}
	return marshalCanonicalActionPlanSnapshot(snapshot)
}

// Initial dependency analysis may append source descriptors for headers found
// only through compiler include paths. These are annotation routing metadata,
// not new inputs to the original lowering. Replay has not performed that scan
// yet. Remove only absent descriptors justified by a precise initial closure
// and the current immutable source-namespace routing. Canonical validation of
// the resulting plan still rejects removal of any direct or input-set source.
// No descriptor or dependency classification is imported into the live plan.
func canonicalFamilyReplayInitialPlan(initial ActionPlanSnapshot, current *ActionPlan) ([]byte, error) {
	paths := map[string]bool{}
	for _, dependencies := range initial.ConfigDependencies {
		if !dependencies.Opaque {
			for _, pathname := range dependencies.SourcePaths {
				paths[pathname] = true
			}
		}
	}
	present := make(map[string]bool, len(current.Sources))
	for _, source := range current.Sources {
		present[source.ID] = true
	}
	plan := snapshotActionPlan(initial)
	plan.Sources = slices.DeleteFunc(plan.Sources, func(source ActionPlanSource) bool {
		if present[source.ID] || !paths[source.Path] || current.metadata == nil {
			return false
		}
		namespace, err := current.metadata.actionPlanSourceNamespace(source.Path)
		return err == nil && namespace == source.Namespace
	})
	return canonicalFamilyReplayPlan(plan, initial.ConfigFiles)
}

// sealAnalysis permits the trusted scanner's append-only source catalog, but
// verifies that removing those new descriptors reproduces the complete
// pre-analysis contract. A new descriptor cannot acquire an execution edge:
// canonical validation would then reject its removal. Seal the full analyzed
// catalog afterwards so caller mutation cannot redirect a projected header.
func (replay *ActionPlanFamilyVerifiedReplay) sealAnalysis(plan *ActionPlan, files map[string]string) ([sha256.Size]byte, error) {
	if replay == nil || plan == nil || plan != replay.plan || len(plan.Sources) < replay.sourceCount {
		return [sha256.Size]byte{}, fmt.Errorf("observed analysis requires its exact replay plan and original source catalog")
	}
	before := *plan
	before.Sources = plan.Sources[:replay.sourceCount]
	original, err := canonicalFamilyReplayPlan(&before, files)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("observed analysis changed its original lowering: %w", err)
	}
	if sha256.Sum256(original) != replay.contractDigest {
		return [sha256.Size]byte{}, fmt.Errorf("observed analysis changed its original lowering/config contract")
	}
	complete, err := canonicalFamilyReplayPlan(plan, files)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(complete), nil
}

// validateCurrent is the public observation API's immediate-use guard. The
// exported ActionPlan fields may be mutated by a caller after the first gate;
// neither a pointer match nor unchanged asserted node IDs authenticates them.
// Recheck the full contract and provisional mapping before exposing any bytes.
func (replay *ActionPlanFamilyVerifiedReplay) validateCurrent(plan *ActionPlan) error {
	if replay == nil || plan == nil || replay.plan != plan || replay.cutID == "" || len(replay.originalIDs) != len(plan.Nodes) {
		return fmt.Errorf("observed header analysis requires its exact verified replay plan")
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		return err
	}
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		return err
	}
	for index, node := range plan.Nodes {
		if original, exists := replay.originalIDs[node.ID]; !exists || original != addressed.Nodes[index].ID {
			return fmt.Errorf("verified replay provisional node binding changed")
		}
	}
	data, err := canonicalFamilyReplayPlan(addressed, replay.configFiles)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != replay.contractDigest {
		return fmt.Errorf("verified replay contract changed before observation analysis")
	}
	return nil
}

// VerifyVariantReplay compares the complete current lowering against its
// original snapshot, normalizing only dependency-analysis annotations and
// their otherwise-unreferenced discovered source descriptors. It must
// run before analysis, while the live plan still owns its selection graph and
// provisional producer identities. A separate detached copy is content
// addressed for comparison; no prepared object-root reparse is performed.
func (cut *ActionPlanFamilyExecutionCut) VerifyVariantReplay(
	variant string,
	initial ActionPlanSnapshot,
	current *ActionPlan,
	configFiles map[string]string,
) (*ActionPlanFamilyVerifiedReplay, error) {
	if _, err := cut.CanonicalJSON(); err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("family replay requires a current action plan")
	}
	if err := validatePlanName("replay variant", variant); err != nil {
		return nil, err
	}
	if err := initial.validate(); err != nil {
		return nil, fmt.Errorf("initial family replay snapshot: %w", err)
	}
	knownVariant := false
	for _, name := range cut.contract.Variants {
		if name == variant {
			knownVariant = true
			break
		}
	}
	if !knownVariant {
		return nil, fmt.Errorf("execution cut has no variant %q", variant)
	}
	// Lowering may still own its reachable radix nodes in the shared planning
	// store. Export before cloning so the detached graph contains the complete
	// persistent input contract rather than an incomplete historical map.
	if err := current.exportReachableActionPlanInputSets(); err != nil {
		return nil, err
	}
	addressed := cloneActionPlan(current)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		return nil, err
	}
	originalIDs := make(map[string]string, len(current.Nodes))
	for index, node := range current.Nodes {
		if _, exists := originalIDs[node.ID]; exists {
			return nil, fmt.Errorf("replay repeats provisional node %s", node.ID)
		}
		originalIDs[node.ID] = addressed.Nodes[index].ID
	}
	want, err := canonicalFamilyReplayInitialPlan(initial, current)
	if err != nil {
		return nil, fmt.Errorf("canonical initial family replay: %w", err)
	}
	got, err := canonicalFamilyReplayPlan(addressed, maps.Clone(configFiles))
	if err != nil {
		return nil, fmt.Errorf("canonical current family replay: %w", err)
	}
	if !bytes.Equal(got, want) {
		return nil, fmt.Errorf("family variant %s replay changed the original lowering/config contract", variant)
	}
	pinnedOriginals := map[string]string{}
	for _, origin := range cut.Origins() {
		if origin.Variant == variant {
			if previous := pinnedOriginals[origin.OriginalNodeID]; previous != "" && previous != origin.NodeID {
				return nil, fmt.Errorf("family variant %s has ambiguous cut origin", variant)
			}
			pinnedOriginals[origin.OriginalNodeID] = origin.NodeID
		}
	}
	opaqueNodes := map[string]bool{}
	executedIDs := map[string]string{}
	for provisional, original := range originalIDs {
		if executed := pinnedOriginals[original]; executed != "" {
			opaqueNodes[provisional] = true
			executedIDs[provisional] = executed
			delete(pinnedOriginals, original)
		}
	}
	if len(pinnedOriginals) != 0 {
		return nil, fmt.Errorf("family variant %s replay is missing cut origins", variant)
	}
	return &ActionPlanFamilyVerifiedReplay{
		cutID: cut.ID(), variant: variant, plan: current, originalIDs: originalIDs, executedIDs: executedIDs, opaqueNodes: opaqueNodes,
		configFiles: maps.Clone(configFiles), contractDigest: sha256.Sum256(got),
		sourceCount: len(current.Sources),
	}, nil
}
