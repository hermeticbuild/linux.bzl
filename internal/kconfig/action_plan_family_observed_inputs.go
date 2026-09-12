package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// executedCutRoots retains prior executions even when precise consumers no
// longer need their producer edges. The authority is the immutable cut, not a
// caller-authored membership, product, or observed-use record. Check each exact
// original-to-final binding before liveness can preserve the corresponding node;
// the complete contract is still checked by cut.Verify before publication.
func (family *ActionPlanFamily) executedCutRoots() (map[string][]string, error) {
	if family.executionCut == nil {
		return nil, nil
	}
	cut := family.executionCut
	if cut.id == "" {
		return nil, fmt.Errorf("family execution retention requires an initialized cut")
	}
	roots := map[string][]string{}
	for _, origin := range cut.contract.Origins {
		if family.originalNodeIDs[origin.Variant][origin.OriginalNodeID] != origin.NodeID {
			return nil, fmt.Errorf("family executed generator %s/%s changed its original node binding", origin.Variant, origin.OriginalNodeID)
		}
		roots[origin.Variant] = append(roots[origin.Variant], origin.NodeID)
	}
	return roots, nil
}

// buildObservedActionPlanFamily consumes in-process replay results. Public
// result annotations are deliberately ignored: the private scanner witnesses
// and original replay contract are checked again before graph substitution.
func buildObservedActionPlanFamily(results []*ActionPlanFamilyVariantPlanningResult) (*validatedActionPlanFamily, *ActionPlanFamilyExecutionCut, *ActionPlanFamilyObservedHeaders, error) {
	return buildObservedActionPlanFamilyWithArtifacts(results, nil)
}

func buildObservedActionPlanFamilyWithArtifacts(results []*ActionPlanFamilyVariantPlanningResult, artifacts *ActionPlanFamilyExecutedArtifacts) (*validatedActionPlanFamily, *ActionPlanFamilyExecutionCut, *ActionPlanFamilyObservedHeaders, error) {
	if len(results) == 0 {
		return nil, nil, nil, fmt.Errorf("observed family requires replay results")
	}
	var cut *ActionPlanFamilyExecutionCut
	var observed *ActionPlanFamilyObservedHeaders
	var diagnostics *actionPlanFamilyObservationDiagnostics
	inputs := make([]actionPlanFamilyBuildVariant, 0, len(results))
	for _, result := range results {
		if result == nil || result.Plan == nil || result.analysis == nil || result.cut == nil || result.observed == nil || result.variant == "" {
			return nil, nil, nil, fmt.Errorf("observed family requires authenticated variant planning results")
		}
		if cut == nil {
			cut, observed = result.cut, result.observed
			diagnostics = newActionPlanFamilyObservationDiagnostics(cut, observed)
		}
		if result.cut.ID() != cut.ID() || result.observed != observed || observed.cutID != cut.ID() {
			return nil, nil, nil, fmt.Errorf("observed family results refer to different executions")
		}
		if artifacts != nil {
			if err := artifacts.verifyHeaders(observed); err != nil {
				return nil, nil, nil, err
			}
		}
		data, err := canonicalFamilyReplayPlan(result.Plan, result.replayConfigFiles)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("observed family variant %s replay contract: %w", result.variant, err)
		}
		if sha256.Sum256(data) != result.replayContractDigest {
			return nil, nil, nil, fmt.Errorf("observed family variant %s changed its replay contract", result.variant)
		}
		dependencies, err := result.analysis.ByNodeID(result.Plan)
		if err != nil {
			return nil, nil, nil, err
		}
		uses, err := result.analysis.ObservedHeaderUsesByNodeID(result.Plan)
		if err != nil {
			return nil, nil, nil, err
		}
		demands, err := result.analysis.GeneratedHeaderDemandsByNodeID(result.Plan)
		if err != nil {
			return nil, nil, nil, err
		}
		diagnostics.addVariant(result.variant, result.Plan, dependencies, demands)
		plan := result.Plan
		if artifacts != nil {
			plan, dependencies, err = substitutePlannedExecutedArtifacts(result, artifacts)
			if err != nil {
				return nil, nil, nil, err
			}
			uses, err = remapExecutedArtifactHeaderUses(result.Plan, plan, uses)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, result.replayConfigFiles)
		if err != nil {
			return nil, nil, nil, err
		}
		inputs = append(inputs, actionPlanFamilyBuildVariant{
			variant: ActionPlanFamilyVariant{Name: result.variant, Snapshot: snapshot}, snapshotValidated: true,
			observedHeaderUses: uses, observedHeaders: observed, executionCut: cut,
		})
	}
	validated, err := buildValidatedActionPlanFamily(inputs)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := cut.Verify(validated.family); err != nil {
		return nil, nil, nil, fmt.Errorf("observed family changed an executed generator: %w", err)
	}
	validated.family.observedHeaderFrontier = diagnostics.finish()
	return validated, cut, observed, nil
}

func remapExecutedArtifactHeaderUses(before, after *ActionPlan, uses []ConfigDependencyObservedHeaderUse) ([]ConfigDependencyObservedHeaderUse, error) {
	if before == nil || after == nil || len(after.Nodes) < len(before.Nodes) {
		return nil, fmt.Errorf("artifact substitution lost original nodes")
	}
	// The private substitution preserves the original vector and appends only
	// immutable copies. Re-key consumer witnesses mechanically; their exact
	// executed producer origins, paths, slots and content identities cannot move.
	ids := make(map[string]string, len(before.Nodes))
	for index, node := range before.Nodes {
		if _, duplicate := ids[node.ID]; duplicate {
			return nil, fmt.Errorf("artifact substitution repeats an original node")
		}
		ids[node.ID] = after.Nodes[index].ID
	}
	result := make([]ConfigDependencyObservedHeaderUse, 0, len(uses))
	for _, use := range uses {
		consumer := ids[use.ConsumerNodeID]
		if consumer == "" || ids[use.ProducerNodeID] != use.ProducerNodeID {
			return nil, fmt.Errorf("artifact substitution changed an executed header producer")
		}
		use.ConsumerNodeID = consumer
		result = append(result, use)
	}
	return result, nil
}

// BuildAndWriteObservedActionPlanFamily is the final publication boundary.
// It verifies the complete final family before emitting any executable shard,
// copy-forward permission, or immutable header source. Snapshots stay v3;
// observation receipts never become caller-authored transport authority.
func BuildAndWriteObservedActionPlanFamily(
	results []*ActionPlanFamilyVariantPlanningResult,
	segmentOutputDirs map[string]string,
	reuseReportOutput, pinnedOutputDirectory, headerOutputDirectory, artifactOutputDirectory string,
	artifacts *ActionPlanFamilyExecutedArtifacts,
) error {
	outputs := maps.Clone(segmentOutputDirs)
	if err := validateFamilyPlanSegmentOutputs(outputs); err != nil {
		return err
	}
	for _, output := range []string{reuseReportOutput, pinnedOutputDirectory, headerOutputDirectory, artifactOutputDirectory} {
		if strings.TrimSpace(output) == "" {
			return fmt.Errorf("observed family output path must not be empty")
		}
	}
	if artifacts == nil {
		return fmt.Errorf("observed family publication requires executed artifact observations")
	}
	validated, cut, observed, err := buildObservedActionPlanFamilyWithArtifacts(results, artifacts)
	if err != nil {
		return err
	}
	if err := cut.WritePinnedMarkers(validated.family, pinnedOutputDirectory); err != nil {
		return err
	}
	if err := os.MkdirAll(headerOutputDirectory, 0o755); err != nil {
		return err
	}
	if err := observed.WriteContentTree(headerOutputDirectory); err != nil {
		return err
	}
	if err := os.MkdirAll(artifactOutputDirectory, 0o755); err != nil {
		return err
	}
	if err := writeExecutedArtifactContentTree(artifacts, artifactOutputDirectory); err != nil {
		return err
	}
	return validated.writeSegmentsAndReuseReport(outputs, reuseReportOutput)
}

// substituteFamilyObservedHeaderInputs runs only on a private variant clone,
// before structural matching. Uses must come from the retained dependency
// analysis, never from public diagnostic records or a caller-authored snapshot.
// Only preprocessing data bindings change: executable, sequence, sidecar and
// auxiliary command inputs retain their original producer identities.
func substituteFamilyObservedHeaderInputs(
	plan *ActionPlan,
	dependencies map[string]ConfigDependencySet,
	uses []ConfigDependencyObservedHeaderUse,
	observed *ActionPlanFamilyObservedHeaders,
) error {
	if len(uses) == 0 {
		return nil
	}
	if plan == nil || observed == nil {
		return fmt.Errorf("observed family inputs require a plan and authenticated contents")
	}
	plan.ensureNodeLookupIndexes()
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return err
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return err
	}
	byConsumer := map[string][]ConfigDependencyObservedHeaderUse{}
	for _, use := range uses {
		byConsumer[use.ConsumerNodeID] = append(byConsumer[use.ConsumerNodeID], use)
	}
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		reads := byConsumer[node.ID]
		if len(reads) == 0 {
			continue
		}
		if !preciseFamilyCompilerNode(plan, *node, dependencies[node.ID]) {
			return fmt.Errorf("observed family consumer %s is not a precise compiler", node.ID)
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		if len(recipe.Inputs) != len(node.Inputs) {
			return fmt.Errorf("observed family consumer %s has inconsistent input bindings", node.ID)
		}
		// An incomplete compound envelope may inspect arbitrary staged files.
		// Its compiler's read receipt does not prove the rest of the script.
		if recipe.CompilerInvocation != nil && !recipe.CompilerInvocation.WorkingInputUsesComplete {
			continue
		}
		semantic := familyRecipeSemanticBindings(recipe)
		written := familyRecipeSemanticWorkingPaths(recipe)
		auxiliary := map[string]bool{}
		if recipe.CompilerInvocation != nil {
			for _, reference := range recipe.CompilerInvocation.AuxiliaryWorkingInputUses {
				auxiliary[reference] = true
			}
		}
		removed := map[int]bool{}
		for _, use := range reads {
			producer, exists := plan.nodesByID[use.ProducerNodeID]
			if !exists || use.Slot < 0 || use.Slot >= len(producer.Outputs) {
				return fmt.Errorf("observed family input has no producer slot")
			}
			output := producer.Outputs[use.Slot]
			if output.Tree != use.Tree || output.Path != use.Path || actionPlanOutputArtifactPath(output) != use.ArtifactPath ||
				output.ObservedPath != "" || configDependencyCompletedResolvedProjection(output.Path) ||
				!slices.Contains(dependencies[node.ID].ObjectPaths, use.LogicalPath) {
				return fmt.Errorf("observed family input changed its precise output contract")
			}
			if _, exists := observed.contents[use.ContentID]; !exists {
				return fmt.Errorf("observed family input has no authenticated contents")
			}
			if written[use.LogicalPath] {
				continue
			}
			// Exact declared staging destinations, not an output's path spelling,
			// decide which bindings may be substituted. A different alias or an
			// outer-command use conservatively keeps this receipt unoptimized.
			eligible, staged := true, false
			var direct []int
			for ordinal, edge := range node.Inputs {
				if edge.ProducerID != use.ProducerNodeID || edge.Slot != use.Slot {
					continue
				}
				reference := "input:" + recipe.Inputs[ordinal]
				workingPath, hasWorkingPath := recipe.WorkingInputs[reference]
				if edge.Role == "sequence" || semantic[reference] || auxiliary[reference] ||
					hasWorkingPath && workingPath != use.LogicalPath {
					eligible = false
					break
				}
				staged = staged || hasWorkingPath
				direct = append(direct, ordinal)
			}
			if !eligible {
				continue
			}
			work := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: use.LogicalPath}
			tree := ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: use.Tree, Path: use.LogicalPath}
			projectsTree := slices.Contains(node.Trees, use.Tree) && slices.Contains(recipe.Trees, use.Tree)
			projectsWork := staged || slices.Contains(recipe.WorkingTrees, use.Tree)
			descriptor := ActionPlanSource{Namespace: LinuxKernelObservedHeaderSourceNamespace, Path: "content/" + use.ContentID}
			descriptor.ID = plan.sourceIDs[actionPlanLookupKey(descriptor.Namespace, descriptor.Path)]
			var replacements []ActionPlanInputSetEntry
			for _, target := range []ActionPlanInputSetTarget{work, tree} {
				entry, found, err := store.Lookup(node.InputSet, target)
				if err != nil {
					return err
				}
				if found && (entry.AuxiliaryUse || entry.SourceID != "" && entry.SourceID != descriptor.ID ||
					entry.ProducerID != "" && (entry.ProducerID != use.ProducerNodeID || entry.Slot != use.Slot)) {
					eligible = false
					break
				}
				if !found && !(target == work && projectsWork || target == tree && projectsTree) {
					continue
				}
				entry.Target, entry.SourceID, entry.ProducerID, entry.Slot = target, descriptor.ID, "", 0
				entry.CompilerUse = recipe.CompilerInvocation != nil
				replacements = append(replacements, entry)
			}
			if !eligible || len(replacements) == 0 {
				continue
			}
			// This is still an ordinary ActionPlan: its lookup/pruning passes
			// require ordinal source IDs. Family localization derives the final
			// semantic source identity from this namespace/content path later.
			descriptor.ID, err = ensureActionPlanSource(plan, descriptor.Namespace, descriptor.Path)
			if err != nil {
				return err
			}
			for _, entry := range replacements {
				entry.SourceID = descriptor.ID
				node.InputSet, err = store.Insert(node.InputSet, entry)
				if err != nil {
					return err
				}
			}
			for _, ordinal := range direct {
				removed[ordinal] = true
			}
		}
		if len(removed) == 0 {
			continue
		}
		remap := map[string]string{}
		working := map[string]string{}
		retained := make([]ActionPlanNodeEdge, 0, len(node.Inputs)-len(removed))
		removedReferences := map[string]bool{}
		for ordinal, edge := range node.Inputs {
			reference := "input:" + recipe.Inputs[ordinal]
			if removed[ordinal] {
				removedReferences[reference] = true
				continue
			}
			binding := edge.Role + ":" + planOrdinal(len(retained))
			remap[reference] = binding
			retained = append(retained, edge)
		}
		for reference, pathname := range recipe.WorkingInputs {
			if removedReferences[reference] {
				continue
			}
			if binding := remap[reference]; binding != "" {
				reference = "input:" + binding
			}
			working[reference] = pathname
		}
		if recipe.CompilerInvocation != nil {
			recipe.CompilerInvocation.WorkingInputUses = slices.DeleteFunc(recipe.CompilerInvocation.WorkingInputUses,
				func(reference string) bool { return removedReferences[reference] })
		}
		recipe = rewriteFamilyRecipeBindings(recipe, remap, working)
		node.Inputs = retained
		recipe.Inputs = make([]string, len(retained))
		for ordinal, edge := range retained {
			recipe.Inputs[ordinal] = edge.Role + ":" + planOrdinal(ordinal)
		}
		id, err := recipe.ID()
		if err != nil {
			return err
		}
		plan.Recipes[id], node.Recipe = recipe, id
	}
	plan.invalidateLookupIndexes()
	return plan.exportReachableActionPlanInputSets()
}
