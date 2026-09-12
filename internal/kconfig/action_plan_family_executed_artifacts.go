package kconfig

// Immutable execution observations are created only from completed cut stores.
// They authorize no publication until the complete final cut is verified.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type actionPlanFamilyExecutedArtifact struct {
	output    ActionPlanFamilyExecutionCutOutput
	contentID string
}

// Substitute only complete compiler envelopes with exact executable staging
// or exclusively internal sidecar-base uses. The caller must retain the private
// scanner evidence and complete replay seal; persistent bindings move atomically.
func substituteExecutedArtifacts(plan *ActionPlan, dependencies map[string]ConfigDependencySet, replay *ActionPlanFamilyVerifiedReplay, observed *ActionPlanFamilyExecutedArtifacts) (*ActionPlan, map[string]ConfigDependencySet, error) {
	if replay == nil || observed == nil || observed.cut == nil || replay.cutID != observed.cutID || observed.cut.ID() != observed.cutID {
		return nil, nil, fmt.Errorf("artifact substitution requires one verified cut")
	}
	if _, err := observed.cut.CanonicalJSON(); err != nil {
		return nil, nil, err
	}
	origins := map[string]string{}
	for _, origin := range observed.cut.Origins() {
		if origin.Variant == replay.variant {
			if previous := origins[origin.OriginalNodeID]; previous != "" && previous != origin.NodeID {
				return nil, nil, fmt.Errorf("ambiguous executed artifact origin")
			}
			origins[origin.OriginalNodeID] = origin.NodeID
		}
	}
	sealedOutputs := map[ActionPlanFamilyExecutionCutRoot]ActionPlanOutput{}
	for _, output := range observed.cut.Outputs() {
		sealedOutputs[ActionPlanFamilyExecutionCutRoot{NodeID: output.NodeID, Slot: output.Slot}] = output.Output
	}
	if err := replay.validateCurrent(plan); err != nil {
		return nil, nil, err
	}
	cloned := cloneActionPlan(plan)
	sets := maps.Clone(dependencies)
	if err := cloned.ensureSourceLookupIndex(); err != nil {
		return nil, nil, err
	}
	store, err := cloned.planningActionPlanInputSetStore()
	if err != nil {
		return nil, nil, err
	}
	originals := map[string]ActionPlanNode{}
	privatePrehostCopyAllowed := true
	for _, node := range plan.Nodes {
		originals[node.ID] = node
		// A whole prior-tree consumer would acquire newly introduced prehost
		// copies as inputs. In particular, that must not alter a pinned cut
		// action's input frontier during final verification.
		if slices.Contains(node.Trees, "prehost") {
			privatePrehostCopyAllowed = false
		}
	}
	copies := map[string]ActionPlanNode{}
	var added []ActionPlanNode
	for index := range cloned.Nodes {
		node := &cloned.Nodes[index]
		dependency, classified := dependencies[node.ID]
		if !classified {
			return nil, nil, fmt.Errorf("artifact consumer has no dependency classification")
		}
		if replay.executedIDs[node.ID] != "" || !preciseFamilyCompilerNode(plan, *node, dependency) {
			continue
		}
		recipe := cloned.Recipes[node.Recipe]
		if recipe.CompilerInvocation == nil || !recipe.CompilerInvocation.WorkingInputUsesComplete || len(recipe.ArgumentTransforms) != 0 {
			continue
		}
		withoutBases := cloneActionRecipe(recipe)
		withoutBases.ObservedOutputBases = nil
		otherSemantic := familyRecipeSemanticBindings(withoutBases)
		written := familyRecipeSemanticWorkingPaths(recipe)
		for ordinal, edge := range node.Inputs {
			if edge.Role == "sequence" {
				continue
			}
			executedID := replay.executedIDs[edge.ProducerID]
			if executedID == "" || !replay.opaqueNodes[edge.ProducerID] {
				continue
			}
			artifact, found := observed.outputs[ActionPlanFamilyExecutionCutRoot{NodeID: executedID, Slot: edge.Slot}]
			if !found {
				continue
			}
			originalID := replay.originalIDs[edge.ProducerID]
			if originalID == "" || origins[originalID] != executedID {
				return nil, nil, fmt.Errorf("artifact changed sealed original producer ownership")
			}
			sealedOutput, sealed := sealedOutputs[ActionPlanFamilyExecutionCutRoot{NodeID: executedID, Slot: edge.Slot}]
			if !sealed || artifact.output.NodeID != executedID || artifact.output.Slot != edge.Slot || artifact.output.Output != sealedOutput {
				return nil, nil, fmt.Errorf("artifact changed sealed executed output ownership")
			}
			producer := originals[edge.ProducerID]
			if edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
				return nil, nil, fmt.Errorf("invalid original artifact slot")
			}
			output := producer.Outputs[edge.Slot]
			// Ordinary public outputs retain their original producer provenance.
			// A planner-owned metadata sidecar can also have an exclusively
			// internal consumer: keep its public producer/view, but stage the
			// authenticated envelope in the private prehost tree. The observation
			// catalog is available before final expansion of every stage; this
			// content-only copy has no edge back to the target-stage generator.
			privateMetadataState := output.Tree == "metadata" && plannerOwnedObservedFamilyState(output)
			if familyViewTree(output.Tree) && (!privateMetadataState || !privatePrehostCopyAllowed) {
				continue
			}
			if !executedArtifactOutputOwnership(output, artifact.output.Output, edge.Slot) {
				return nil, nil, fmt.Errorf("artifact changed original output ownership")
			}
			binding := recipe.Inputs[ordinal]
			reference := "input:" + binding
			if output.ObservedPath == "" {
				if !slices.Contains(recipe.ExecutableInputs, binding) || recipe.WorkingInputs[reference] != output.Path || written[output.Path] {
					continue
				}
			} else {
				if otherSemantic[reference] || recipe.WorkingInputs[reference] != "" {
					continue
				}
				used := false
				for base, states := range recipe.ObservedOutputBases {
					if slices.Contains(states, binding) {
						if recipe.ObservedOutputs[base] != output.ObservedPath {
							return nil, nil, fmt.Errorf("sidecar base changed its logical path")
						}
						used = true
					}
				}
				if !used {
					continue
				}
			}
			// Update only exact matching logical work/tree materializations. A
			// different owner at either target declines the entire replacement;
			// any other alias stays on its original edge and prevents sharing.
			var replacements []ActionPlanInputSetEntry
			eligible := true
			for _, target := range []ActionPlanInputSetTarget{{Kind: ActionPlanInputSetWorkTarget, Path: output.Path}, {Kind: ActionPlanInputSetTreeTarget, Tree: output.Tree, Path: output.Path}} {
				entry, found, err := store.Lookup(node.InputSet, target)
				if err != nil {
					return nil, nil, err
				}
				if !found {
					continue
				}
				if output.ObservedPath != "" || entry.SourceID != "" || entry.ProducerID != edge.ProducerID || entry.Slot != edge.Slot {
					eligible = false
					break
				}
				replacements = append(replacements, entry)
			}
			if !eligible {
				continue
			}
			copyStage, copyTree := producer.Stage, output.Tree
			if privateMetadataState {
				copyStage, copyTree = "prehost", "prehost"
			}
			copyKey := copyStage + "\x00" + copyTree + "\x00" + node.Product + "\x00" + artifact.contentID
			allocation := sha256.Sum256([]byte("linux-kernel-executed-artifact-copy-v1\x00" + copyKey))
			allocationID := hex.EncodeToString(allocation[:])
			copyNode, exists := copies[copyKey]
			if !exists {
				sourceID, err := ensureActionPlanSource(cloned, LinuxKernelExecutedArtifactSourceNamespace, "content/"+artifact.contentID)
				if err != nil {
					return nil, nil, err
				}
				copyRecipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile", Arguments: []string{"-input", "${source:content:00000000}", "-preserve_mode", "-out", "${output:00000000}"}, Sources: []string{"content:00000000"}, Outputs: []string{"00000000"}}
				recipeID, err := copyRecipe.ID()
				if err != nil {
					return nil, nil, err
				}
				copyNode = ActionPlanNode{Stage: copyStage, Kind: "metadata", Tool: "actionfile", Recipe: recipeID, Product: node.Product, Sources: []ActionPlanSourceEdge{{Role: "content", SourceID: sourceID}}, Outputs: []ActionPlanOutput{{Tree: copyTree, Path: "executed-artifacts/" + allocationID, ArtifactPath: ".linux-bzl-executed-artifacts/" + allocationID}}}
				copyNode.ID = copyNode.ContentID()
				cloned.Recipes[recipeID] = copyRecipe
				copies[copyKey] = copyNode
				added = append(added, copyNode)
				sets[copyNode.ID] = ConfigDependencySet{}
			}
			node.Inputs[ordinal].ProducerID, node.Inputs[ordinal].Slot = copyNode.ID, 0
			for _, entry := range replacements {
				entry.ProducerID, entry.Slot = copyNode.ID, 0
				node.InputSet, err = store.Replace(node.InputSet, entry)
				if err != nil {
					return nil, nil, err
				}
			}
		}
	}
	cloned.Nodes = append(cloned.Nodes, added...)
	if err := cloned.exportReachableActionPlanInputSets(); err != nil {
		return nil, nil, err
	}
	cloned.invalidateLookupIndexes()
	oldSets := make([]ConfigDependencySet, len(cloned.Nodes))
	for index, node := range cloned.Nodes {
		oldSets[index] = sets[node.ID]
	}
	if err := contentAddressActionPlanNodes(cloned); err != nil {
		return nil, nil, err
	}
	sets = map[string]ConfigDependencySet{}
	for index, node := range cloned.Nodes {
		sets[node.ID] = oldSets[index]
	}
	return cloned, sets, nil
}

// Private observed-state filenames are deliberately relocated by the family
// reducer. This descriptor check is only used AFTER exact original-node ->
// executed-node / slot authentication against the sealed cut above. It is not
// an alternate ownership proof based on matching a logical filename.
func executedArtifactOutputOwnership(original, executed ActionPlanOutput, slot int) bool {
	if original.Tree != executed.Tree || original.ObservedPath != executed.ObservedPath {
		return false
	}
	if original.Path == executed.Path {
		return true
	}
	if !plannerOwnedObservedFamilyState(original) || executed.ArtifactPath != "" {
		return false
	}
	relative, owned := strings.CutPrefix(executed.Path, familyOwnedObservedStateDirectory+"/")
	if !owned {
		return false
	}
	structuralID, filename, complete := strings.Cut(relative, "/")
	return complete && validatePlanDigest("executed observed-state structural identity", structuralID) == nil &&
		filename == planOrdinal(slot)+".state"
}

// ActionPlanFamilyExecutedArtifacts owns a bounded immutable observation of
// every slot in a completed cut. Only ObserveArtifacts constructs it; callers
// cannot replace execution evidence with asserted hashes or snapshot fields.
type ActionPlanFamilyExecutedArtifacts struct {
	cut      *ActionPlanFamilyExecutionCut
	cutID    string
	outputs  map[ActionPlanFamilyExecutionCutRoot]actionPlanFamilyExecutedArtifact
	contents map[string][]byte
	modes    map[string]uint32
}

func executedArtifactContentID(content []byte, mode uint32) string {
	hash := sha256.New()
	hash.Write([]byte("linux-kernel-executed-artifact-content-v1\x00" + strconv.FormatUint(uint64(mode), 8) + "\x00"))
	hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

// Every slot is read from the exact completed immutable execution stores. A
// sidecar is retained byte-for-byte, including its writer. In particular this
// neither turns equal content from different writers into equal states nor
// treats a sidecar as an ordinary-header include receipt.
func observeExecutedArtifacts(cut *ActionPlanFamilyExecutionCut, stores map[string]string, perFile, total int) (observed *ActionPlanFamilyExecutedArtifacts, returnedErr error) {
	if _, err := cut.CanonicalJSON(); err != nil {
		return nil, err
	}
	if perFile <= 0 || total <= 0 {
		return nil, fmt.Errorf("invalid observation budget")
	}
	result := &ActionPlanFamilyExecutedArtifacts{cut: cut, cutID: cut.ID(), outputs: map[ActionPlanFamilyExecutionCutRoot]actionPlanFamilyExecutedArtifact{}, contents: map[string][]byte{}, modes: map[string]uint32{}}
	roots := map[string]*os.Root{}
	defer func() {
		for _, root := range roots {
			if err := root.Close(); err != nil && returnedErr == nil {
				observed, returnedErr = nil, err
			}
		}
	}()
	writers := map[string]bool{}
	for _, id := range cut.NodeIDs() {
		writers[id] = true
	}
	used := 0
	for _, output := range cut.Outputs() {
		root := roots[output.Output.Tree]
		if root == nil {
			directory := stores[output.Output.Tree]
			if directory == "" {
				return nil, fmt.Errorf("missing output store %s", output.Output.Tree)
			}
			var err error
			root, err = os.OpenRoot(directory)
			if err != nil {
				return nil, err
			}
			roots[output.Output.Tree] = root
		}
		filename := path.Join("nodes", output.NodeID, planOrdinal(output.Slot))
		info, err := observedHeaderRegularFile(root, filename)
		if err != nil {
			return nil, err
		}
		if info.Size() < 0 || info.Size() > int64(perFile) || info.Size() > int64(total-used) {
			return nil, fmt.Errorf("executed artifact exceeds observation budget")
		}
		content, err := readObservedHeader(root, filename, info, min(perFile, total-used))
		if err != nil {
			return nil, err
		}
		used += len(content)
		if output.Output.ObservedPath != "" {
			state, err := toolaction.DecodeObservedOutputState(content)
			if err != nil {
				return nil, fmt.Errorf("executed sidecar: %w", err)
			}
			if state.Writer != "" && !writers[state.Writer] {
				return nil, fmt.Errorf("executed sidecar writer is outside its sealed dependency closure")
			}
		}
		mode := uint32(info.Mode().Perm() & 0o111)
		id := executedArtifactContentID(content, mode)
		if previous, found := result.contents[id]; found {
			if !bytes.Equal(previous, content) || result.modes[id] != mode {
				return nil, fmt.Errorf("executed artifact identity collision")
			}
		} else {
			result.contents[id], result.modes[id] = content, mode
		}
		key := ActionPlanFamilyExecutionCutRoot{NodeID: output.NodeID, Slot: output.Slot}
		if _, found := result.outputs[key]; found {
			return nil, fmt.Errorf("repeated executed artifact")
		}
		result.outputs[key] = actionPlanFamilyExecutedArtifact{output: output, contentID: id}
	}
	return result, nil
}

const (
	LinuxKernelExecutedArtifactSourceNamespace = "observed-artifacts"
	MaxActionPlanFamilyExecutedArtifactBytes   = 64 << 20
	MaxActionPlanFamilyExecutedArtifactsBytes  = 128 << 20
)

// ObserveArtifacts reads every output of the exact completed execution cut,
// including executable permissions and sidecar writer identities. A digest in
// a marker or snapshot cannot replace these immutable action output bytes.
func (cut *ActionPlanFamilyExecutionCut) ObserveArtifacts(stores map[string]string) (*ActionPlanFamilyExecutedArtifacts, error) {
	return observeExecutedArtifacts(cut, stores, MaxActionPlanFamilyExecutedArtifactBytes, MaxActionPlanFamilyExecutedArtifactsBytes)
}

// Header and artifact projections must describe the same completed stores.
// Reusing a cut ID with bytes observed from another execution is not authority.
func (observed *ActionPlanFamilyExecutedArtifacts) verifyHeaders(headers *ActionPlanFamilyObservedHeaders) error {
	if observed == nil || headers == nil || observed.cut == nil || observed.cutID != headers.cutID || observed.cut.ID() != observed.cutID {
		return fmt.Errorf("executed artifacts and headers require the same sealed execution")
	}
	if _, err := observed.cut.CanonicalJSON(); err != nil {
		return err
	}
	roots := observed.cut.Roots()
	sealed := make(map[ActionPlanFamilyExecutionCutRoot]ActionPlanFamilyExecutionCutOutput)
	for _, output := range observed.cut.Outputs() {
		sealed[ActionPlanFamilyExecutionCutRoot{NodeID: output.NodeID, Slot: output.Slot}] = output
	}
	if len(headers.headers) != len(roots) {
		return fmt.Errorf("executed header projection lost selected roots")
	}
	byRoot := map[ActionPlanFamilyExecutionCutRoot]ActionPlanFamilyObservedHeader{}
	for _, header := range headers.headers {
		key := ActionPlanFamilyExecutionCutRoot{NodeID: header.NodeID, Slot: header.Slot}
		if _, duplicate := byRoot[key]; duplicate {
			return fmt.Errorf("executed header projection repeats an output")
		}
		byRoot[key] = header
	}
	for _, root := range roots {
		artifact, haveArtifact := observed.outputs[root]
		output, haveOutput := sealed[root]
		header, haveHeader := byRoot[root]
		content, haveContent := observed.contents[artifact.contentID]
		mode, haveMode := observed.modes[artifact.contentID]
		headerContent, haveHeaderContent := headers.contents[header.ContentID]
		headerMode, haveHeaderMode := headers.modes[header.ContentID]
		if !haveArtifact || !haveOutput || !haveHeader || !haveContent || !haveMode || !haveHeaderContent || !haveHeaderMode ||
			artifact.output != output || header.Output != output.Output || header.Output.ObservedPath != "" ||
			executedArtifactContentID(content, mode) != artifact.contentID ||
			observedHeaderContentID(content, mode) != header.ContentID ||
			header.ExecutableMode != mode || headerMode != mode || !bytes.Equal(content, headerContent) {
			return fmt.Errorf("executed header and artifact observations disagree")
		}
	}
	return nil
}
