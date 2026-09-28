package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ActionPlanFamilyInitialExecution retains the complete conservative reduction
// alongside its bounded execution selection. Family is exposed for inspection
// and normal complete-plan emission; Cut owns a detached immutable contract.
// Never reduce only the selected raw ancestors: implicit prior-tree edges are
// determined against the complete family before the cut is selected.
type ActionPlanFamilyInitialExecution struct {
	Family *ActionPlanFamily
	Cut    *ActionPlanFamilyExecutionCut
}

// BuildConservativeActionPlanFamily reconstructs the initial execution family
// from the original complete snapshots. All nodes use full-config annotations,
// including nodes which an earlier analysis classified precisely. Input
// snapshots and their dependency maps are not modified. This is also the
// reconstruction entrypoint for ReadActionPlanFamilyExecutionCut.
func BuildConservativeActionPlanFamily(variants []ActionPlanFamilyVariant) (*ActionPlanFamily, error) {
	inputs := make([]actionPlanFamilyBuildVariant, 0, len(variants))
	for _, variant := range variants {
		if err := variant.Snapshot.validate(); err != nil {
			return nil, fmt.Errorf("initial family variant %s: %w", variant.Name, err)
		}
		snapshot := variant.Snapshot
		snapshot.ConfigDependencies = make(map[string]ConfigDependencySet, len(snapshot.Nodes))
		for _, node := range snapshot.Nodes {
			snapshot.ConfigDependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "conservative initial family execution"}
		}
		inputs = append(inputs, actionPlanFamilyBuildVariant{
			variant: ActionPlanFamilyVariant{Name: variant.Name, Snapshot: snapshot}, snapshotValidated: true,
		})
	}
	validated, err := buildValidatedActionPlanFamily(inputs)
	if err != nil {
		return nil, err
	}
	return validated.family, nil
}

// NewActionPlanFamilyInitialExecution reduces complete full-config variants,
// then maps encountered header and declared executable demands through exact node
// provenance. Demands select work to execute, not authority for later include
// resolution or precision. Those require authenticated observed bytes, original
// lowering replay, actual-consumer receipts, and final cut verification.
//
// An unavailable/truncated or over-budget demand collection abandons this
// optimization with an empty cut. Otherwise select bounded, beneficial groups
// from its exact roots, retaining the complete conservative family throughout.
// A malformed record is an error. Known config projections stay on the ordinary
// config-provenance path and are never selected as observed-header roots.
func NewActionPlanFamilyInitialExecution(
	variants []ActionPlanFamilyVariant,
	demands map[string]ConfigDependencyGeneratedHeaderDemandCollection,
) (*ActionPlanFamilyInitialExecution, error) {
	return newActionPlanFamilyInitialExecution(variants, demands,
		MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes)
}

func newActionPlanFamilyInitialExecution(
	variants []ActionPlanFamilyVariant,
	demands map[string]ConfigDependencyGeneratedHeaderDemandCollection,
	maximumRecords, maximumBytes int,
) (*ActionPlanFamilyInitialExecution, error) {
	if maximumRecords <= 0 || maximumBytes <= 0 {
		return nil, fmt.Errorf("initial execution requires positive demand limits")
	}
	family, err := BuildConservativeActionPlanFamily(variants)
	if err != nil {
		return nil, err
	}
	finish := func(roots []ActionPlanFamilyExecutionCutRoot) (*ActionPlanFamilyInitialExecution, error) {
		cut, err := NewActionPlanFamilyExecutionCut(family, roots)
		if err != nil {
			return nil, err
		}
		return &ActionPlanFamilyInitialExecution{Family: family, Cut: cut}, nil
	}
	snapshots := make(map[string]ActionPlanSnapshot, len(variants))
	for _, variant := range variants {
		snapshots[variant.Name] = variant.Snapshot
	}
	for _, name := range slices.Sorted(maps.Keys(demands)) {
		if _, found := snapshots[name]; !found {
			return nil, fmt.Errorf("initial header demands name unknown variant %q", name)
		}
	}
	unavailable, records := false, 0
	for _, name := range family.Variants {
		collection, found := demands[name]
		if !collection.Enabled && (collection.prospectiveTruncated || len(collection.prospective) != 0) ||
			(collection.Truncated || collection.prospectiveTruncated) && len(collection.prospective) != 0 {
			return nil, fmt.Errorf("initial prospective header demands for %s have inconsistent collection state", name)
		}
		if !collection.Enabled && (collection.Truncated || len(collection.Demands) != 0) || collection.Truncated && len(collection.Demands) != 0 {
			return nil, fmt.Errorf("initial header demands for %s have inconsistent collection state", name)
		}
		if !found || !collection.Enabled || collection.Truncated {
			unavailable = true
		}
		if len(collection.Demands) > maximumRecords-records {
			unavailable = true
		} else {
			records += len(collection.Demands)
		}
	}
	if unavailable {
		return finish(nil)
	}
	finalNodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		finalNodes[node.ID] = node
	}
	configPaths := map[string]bool{}
	for _, projection := range resolvedConfigProjections() {
		configPaths[projection.output] = true
	}
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	groups := map[string]*actionPlanFamilyHeaderDemandGroup{}
	bytes := 0
	for _, name := range family.Variants {
		originals := make(map[string]ActionPlanNode, len(snapshots[name].Nodes))
		for _, node := range snapshots[name].Nodes {
			originals[node.ID] = node
		}
		for _, demand := range demands[name].Demands {
			size := 256 + len(demand.ConsumerNodeID) + len(demand.ProducerNodeID) + len(demand.Tree) +
				len(demand.Path) + len(demand.ArtifactPath) + len(demand.LogicalPath)
			if size > maximumBytes-bytes {
				return finish(nil)
			}
			bytes += size
			if err := validateInitialFamilyHeaderDemand(demand, originals); err != nil {
				return nil, fmt.Errorf("initial header demand for %s: %w", name, err)
			}
			// Reaching a configured projection must not turn config bytes into
			// apparently config-independent source contents, including aliases.
			if configPaths[demand.Path] || configPaths[demand.ArtifactPath] || configPaths[demand.LogicalPath] {
				continue
			}
			originalIDs := family.originalNodeIDs[name]
			if originalIDs[demand.ConsumerNodeID] == "" || originalIDs[demand.ProducerNodeID] == "" {
				// The complete reducer may remove internal outputs with no live
				// consumers. Do not resurrect them merely for optimization.
				continue
			}
			id := originalIDs[demand.ProducerNodeID]
			node, found := finalNodes[id]
			if !found || demand.Slot >= len(node.Outputs) {
				return nil, fmt.Errorf("initial header demand lost reduced output %s[%d]", id, demand.Slot)
			}
			original := originals[demand.ProducerNodeID].Outputs[demand.Slot]
			if err := validateInitialFamilyHeaderOutput(original, node.Outputs[demand.Slot], demand.Slot); err != nil {
				return nil, fmt.Errorf("initial header demand %s/%s[%d]: %w", name, id, demand.Slot, err)
			}
			root := ActionPlanFamilyExecutionCutRoot{NodeID: id, Slot: demand.Slot}
			if !roots[root] && len(roots) >= MaxActionPlanFamilyExecutionCutOutputs {
				return finish(nil)
			}
			roots[root] = true
			// Group corresponding logical outputs across configurations only
			// for scheduling policy. Every exact original producer/slot above
			// remains a separate root in the authenticated execution contract.
			key := demand.Tree + "\x00" + demand.Path
			group := groups[key]
			if group == nil {
				group = &actionPlanFamilyHeaderDemandGroup{key: key,
					roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
				groups[key] = group
			}
			group.roots[root] = true
			group.consumers[name+"\x00"+demand.ConsumerNodeID] = originalIDs[demand.ConsumerNodeID]
		}
	}
	var usage actionPlanFamilyDemandUsage
	overflow, err := addInitialFamilyExecutableGroupsWithUsage(family, variants, groups, records, bytes, maximumRecords, maximumBytes, &usage)
	if err != nil {
		return nil, err
	}
	if overflow {
		return finish(nil)
	}
	// Unlike the primary demand collection, optional sidecar discovery must
	// not abandon useful header/executable groups when its own budget is full.
	if _, err := addInitialFamilySidecarGroups(family, variants, groups, maximumRecords, maximumBytes); err != nil {
		return nil, err
	}
	limits := defaultActionPlanFamilyExecutionCutLimits()
	selection, err := selectActionPlanFamilyHeaderExecutionFrom(family, slices.Collect(maps.Values(groups)),
		limits, maxActionPlanFamilyHeaderExecutionAttempts, nil)
	if err != nil {
		return nil, err
	}
	baseline := &ActionPlanFamilyInitialExecution{Family: family, Cut: selection.cut}
	// Baseline header, executable and sidecar eligibility and priority are now
	// fixed. Admit the entire numeric tier before spending any remaining trials.
	optional, admitted, err := initialFamilyProspectiveHeaderGroups(family, variants, demands, groups, usage,
		maximumRecords, maximumBytes, limits.outputs)
	if err != nil {
		return nil, err
	}
	if !admitted || len(optional) == 0 {
		return baseline, nil
	}
	extended, err := selectActionPlanFamilyHeaderExecutionFrom(family, optional, limits,
		maxActionPlanFamilyHeaderExecutionAttempts, selection)
	if err != nil {
		return nil, err
	}
	return &ActionPlanFamilyInitialExecution{Family: family, Cut: extended.cut}, nil
}

type actionPlanFamilyHeaderDemandGroup struct {
	key       string
	roots     map[ActionPlanFamilyExecutionCutRoot]bool
	consumers map[string]string // Original variant/consumer instance -> reduced node.
	numeric   bool              // Scheduling-only, exact bound numeric candidates.
}

const maxActionPlanFamilyHeaderExecutionAttempts = 128

// Prefer headers which unblock more prospective compiler instances, without
// executing those same compilers merely to observe a later header. For example,
// a header derived from a linked image must not force the entire kernel into
// the opaque pre-execution cut. This policy knows no filenames or tool roles.
// Declined roots remain ordinary opaque inputs during replay.
func selectActionPlanFamilyHeaderExecution(family *ActionPlanFamily, groups []*actionPlanFamilyHeaderDemandGroup,
	limits actionPlanFamilyExecutionCutLimits, maximumAttempts int,
) (*ActionPlanFamilyExecutionCut, error) {
	selection, err := selectActionPlanFamilyHeaderExecutionFrom(family, groups, limits, maximumAttempts, nil)
	if err != nil {
		return nil, err
	}
	return selection.cut, nil
}

// A seed is the completed baseline policy, not a second reduction. The optional
// phase retains its roots and shares its total attempt allowance. Numeric groups
// may pin prerequisite compilers only under the separate bounded benefit test.
type actionPlanFamilyHeaderSelection struct {
	cut               *ActionPlanFamilyExecutionCut
	roots             map[ActionPlanFamilyExecutionCutRoot]bool
	consumers         map[string]string
	benefit, attempts int
}

func selectActionPlanFamilyHeaderExecutionFrom(family *ActionPlanFamily, groups []*actionPlanFamilyHeaderDemandGroup,
	limits actionPlanFamilyExecutionCutLimits, maximumAttempts int, seed *actionPlanFamilyHeaderSelection,
) (*actionPlanFamilyHeaderSelection, error) {
	if maximumAttempts <= 0 || limits.nodes <= 0 || limits.outputs <= 0 || limits.inputSets <= 0 || limits.records <= 0 || limits.bytes <= 0 {
		return nil, fmt.Errorf("header execution selection requires positive limits")
	}
	if seed == nil {
		if err := family.validate(); err != nil {
			return nil, fmt.Errorf("header execution selection family: %w", err)
		}
		cut, err := captureActionPlanFamilyExecutionCut(family, nil, limits)
		if err != nil {
			return nil, err
		}
		seed = &actionPlanFamilyHeaderSelection{cut: cut,
			roots: map[ActionPlanFamilyExecutionCutRoot]bool{}, consumers: map[string]string{}}
	}
	state := *seed
	state.roots, state.consumers = maps.Clone(seed.roots), maps.Clone(seed.consumers)
	protected := map[string]bool{}
	previouslyPinned := map[string]bool{}
	for _, node := range seed.cut.contract.Nodes {
		previouslyPinned[node.Node.ID] = true
	}
	for _, id := range seed.consumers {
		if !previouslyPinned[id] {
			protected[id] = true
		}
	}
	selected, retainedRoots, retainedConsumers, benefit := state.cut, state.roots, state.consumers, state.benefit
	ordered := slices.Clone(groups)
	slices.SortFunc(ordered, func(a, b *actionPlanFamilyHeaderDemandGroup) int {
		if len(a.consumers) > len(b.consumers) {
			return -1
		}
		if len(a.consumers) < len(b.consumers) {
			return 1
		}
		return strings.Compare(a.key, b.key)
	})
	for _, group := range ordered {
		if state.attempts >= maximumAttempts {
			break
		}
		state.attempts++
		trialRoots := maps.Clone(retainedRoots)
		maps.Copy(trialRoots, group.roots)
		if len(trialRoots) > limits.outputs {
			continue
		}
		trial, err := captureActionPlanFamilyExecutionCut(family, slices.Collect(maps.Keys(trialRoots)), limits)
		if err != nil {
			var budget *actionPlanFamilyExecutionCutBudgetError
			if errors.As(err, &budget) {
				continue
			}
			return nil, err
		}
		consumers := maps.Clone(retainedConsumers)
		maps.Copy(consumers, group.consumers)
		pinned := map[string]bool{}
		for _, node := range trial.contract.Nodes {
			pinned[node.Node.ID] = true
		}
		preservesBaseline := true
		for id := range protected {
			if pinned[id] {
				preservesBaseline = false
				break
			}
		}
		if !preservesBaseline && !group.numeric {
			continue
		}
		prospective := 0
		for _, nodeID := range consumers {
			if !pinned[nodeID] {
				prospective++
			}
		}
		if group.numeric {
			if !numericHeaderExecutionBenefitsConsumers(group, selected, trial, retainedRoots) {
				continue
			}
			selected, retainedRoots, retainedConsumers, benefit = trial, trialRoots, consumers, prospective
			continue
		}
		// Multiple prerequisite groups may unblock the same compiler. A tie
		// in distinct consumers is useful only when this group adds exact
		// roots for a consumer which remains outside the opaque cut.
		complements := false
		if prospective == benefit && len(trialRoots) > len(retainedRoots) {
			previouslyPinned := map[string]bool{}
			for _, node := range selected.contract.Nodes {
				previouslyPinned[node.Node.ID] = true
			}
			preservesConsumers := true
			for _, nodeID := range retainedConsumers {
				if !previouslyPinned[nodeID] && pinned[nodeID] {
					preservesConsumers = false
					break
				}
			}
			if preservesConsumers {
				for consumer, nodeID := range group.consumers {
					if retainedConsumers[consumer] == nodeID && !pinned[nodeID] {
						complements = true
						break
					}
				}
			}
		}
		if prospective < benefit || prospective == benefit && !complements {
			continue
		}
		selected, retainedRoots, retainedConsumers, benefit = trial, trialRoots, consumers, prospective
	}
	state.cut, state.roots, state.consumers, state.benefit = selected, retainedRoots, retainedConsumers, benefit
	return &state, nil
}

func validateInitialFamilyHeaderDemand(demand ConfigDependencyGeneratedHeaderDemand, nodes map[string]ActionPlanNode) error {
	if _, found := nodes[demand.ConsumerNodeID]; !found {
		return fmt.Errorf("unknown original consumer %q", demand.ConsumerNodeID)
	}
	producer, found := nodes[demand.ProducerNodeID]
	if !found || demand.Slot < 0 || demand.Slot >= len(producer.Outputs) {
		return fmt.Errorf("unknown original producer output %s[%d]", demand.ProducerNodeID, demand.Slot)
	}
	output := producer.Outputs[demand.Slot]
	if output.ObservedPath != "" {
		return fmt.Errorf("observed-state output cannot be a header demand")
	}
	if demand.Tree != output.Tree || demand.Path != output.Path || demand.ArtifactPath != actionPlanOutputArtifactPath(output) {
		return fmt.Errorf("header demand does not match its original complete output descriptor")
	}
	return validatePlanRelativePath("initial header demand logical", demand.LogicalPath)
}

func validateInitialFamilyHeaderOutput(original, final ActionPlanOutput, slot int) error {
	if original.Tree != final.Tree || original.Path != final.Path || final.ObservedPath != "" {
		return fmt.Errorf("reduced ordinary output changed logical identity")
	}
	if _, private := plannerOwnedPrivateFamilyArtifact(original); !private {
		if original.ArtifactPath != final.ArtifactPath {
			return fmt.Errorf("reduced ordinary output changed nonprivate artifact path")
		}
		return nil
	}
	// This family was just built from the exact originals above. The only
	// admitted descriptor change is its existing private allocation rewrite;
	// node/slot correspondence comes from provenance, never this path shape.
	relative, owned := strings.CutPrefix(final.ArtifactPath, familyOwnedArtifactDirectory+"/")
	id, ordinal, complete := strings.Cut(relative, "/")
	if !owned || !complete || ordinal != planOrdinal(slot) || validatePlanDigest("family artifact", id) != nil {
		return fmt.Errorf("reduced private artifact lost its family allocation")
	}
	return nil
}
