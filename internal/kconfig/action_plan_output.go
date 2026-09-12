package kconfig

// ActionPlan bridges the semantic Kconfig/Kbuild graph into the
// execution-time v5 action plan.  It intentionally does not emit BUILD syntax
// or select a compiler family.  All flags in the recipes are the concrete
// values produced by the Kbuild parser with the selected tool probe.

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// lowerSelectedActionPlan performs the source-derived action traversal shared
// by probe discovery and final action-plan generation. The provisional graph
// is sufficient to force every lazy Kbuild rule, command, source script, and
// deferred-content expression which can register a compiler probe. Products
// and serialized node identities are deliberately a later phase.
func (m *CompactMetadata) lowerSelectedActionPlan(
	targetToolsetIdentity, hostToolsetIdentity string,
	probeDiscoveryOnly bool,
	familyCache *ActionPlanFamilyPlanningCache,
) (*ActionPlan, *compactKbuildSelectionGraph, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("kernel action plan requires semantic Kbuild metadata")
	}
	if err := validateProbeIdentity(targetToolsetIdentity); err != nil {
		return nil, nil, fmt.Errorf("target toolset identity: %w", err)
	}
	if err := validateProbeIdentity(hostToolsetIdentity); err != nil {
		return nil, nil, fmt.Errorf("host toolset identity: %w", err)
	}
	if m.configFragment == nil {
		return nil, nil, fmt.Errorf("kernel action plan requires a resolved config fragment")
	}

	plan := &ActionPlan{
		Toolsets:           map[string]string{"target": targetToolsetIdentity, "host": hostToolsetIdentity},
		Recipes:            map[string]ActionRecipe{},
		InputSets:          map[string]ActionPlanInputSetNode{},
		metadata:           m,
		probeDiscoveryOnly: probeDiscoveryOnly,
	}
	if familyCache != nil {
		plan.inputSetStore = familyCache.inputSetStore()
	} else {
		plan.inputSetStore = newPlanningActionPlanInputSetStore()
	}
	plan.attachFamilyPlanningCache(familyCache)
	selections, err := m.appendGeneratedActionPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	return plan, selections, nil
}

// DiscoverActionPlanProbes traverses every selected native Kbuild action so
// lazy compiler and source-script expressions register their probe requests.
// The provisional action graph is discarded: product facades, module SDK
// projections, final transitive content IDs, and serialization order belong
// only to replay, after probe results have made the graph concrete.
func (m *CompactMetadata) DiscoverActionPlanProbes(
	targetToolsetIdentity, hostToolsetIdentity string,
) error {
	plan, _, err := m.lowerSelectedActionPlan(targetToolsetIdentity, hostToolsetIdentity, true, nil)
	if err != nil {
		return err
	}
	if m.compilerPredefines == nil {
		return nil
	}
	return discoverActionPlanCompilerProbes(plan)
}

func (m *CompactMetadata) ActionPlan(targetToolsetIdentity, hostToolsetIdentity string) (*ActionPlan, error) {
	plan, _, err := m.actionPlan(targetToolsetIdentity, hostToolsetIdentity, false, nil)
	return plan, err
}

// ActionPlanWithConfigDependencies returns the final content-addressed plan
// together with conservative configuration annotations keyed by those final
// node IDs. Classification happens while the provisional selection graph still
// names every exact Kbuild profile, before transitive node IDs are rewritten.
func (m *CompactMetadata) ActionPlanWithConfigDependencies(targetToolsetIdentity, hostToolsetIdentity string) (*ActionPlan, map[string]ConfigDependencySet, error) {
	return m.actionPlan(targetToolsetIdentity, hostToolsetIdentity, true, nil)
}

// ActionPlanWithFamilyPlanningCache is the family snapshot form. The explicit
// wrapper shares immutable config-dependency observations and the persistent
// input-set store while keeping every resolved graph and local producer
// binding private to this plan.
func (m *CompactMetadata) ActionPlanWithFamilyPlanningCache(
	targetToolsetIdentity, hostToolsetIdentity string,
	cache *ActionPlanFamilyPlanningCache,
) (*ActionPlan, map[string]ConfigDependencySet, error) {
	return m.actionPlan(targetToolsetIdentity, hostToolsetIdentity, true, cache)
}

// ActionPlanFamilyVariantPlanningOptions opts into generated-header frontier
// reporting. Replay inputs must be either all absent (initial planning) or all
// present. ResolvedConfigFiles must describe this metadata's actual resolved
// outputs, not be copied from InitialSnapshot to make replay verification pass.
// The caller retains the original immutable source/tool/probe action inputs.
type ActionPlanFamilyVariantPlanningOptions struct {
	Cache               *ActionPlanFamilyPlanningCache
	Variant             string
	InitialSnapshot     *ActionPlanSnapshot
	Cut                 *ActionPlanFamilyExecutionCut
	ObservedHeaders     *ActionPlanFamilyObservedHeaders
	ResolvedConfigFiles map[string]string
	// PrepareCompilerGuards runs after original lowering and cut verification,
	// before creating any compiler namespace. It may attach separately replayed
	// compiler facts/observers; it does not license source or generated bytes.
	PrepareCompilerGuards func() error
	// CaptureCheckpoint runs once after complete initial lowering/projection
	// preparation and before source analysis mutates the provisional catalog.
	// The callback must freeze bytes, not retain or mutate the live plan.
	CaptureCheckpoint func(*ActionPlan) error
}

// ActionPlanFamilyVariantPlanningResult separates optimization evidence from
// snapshot v3. Every dependency, demand and observed use refers to Plan's final
// content-addressed IDs. Observed bytes do not license final emission: callers
// must still reduce the complete family and verify its execution cut.
type ActionPlanFamilyVariantPlanningResult struct {
	Plan                   *ActionPlan
	Dependencies           map[string]ConfigDependencySet
	GeneratedHeaderDemands ConfigDependencyGeneratedHeaderDemandCollection
	ObservedHeaderUses     []ConfigDependencyObservedHeaderUse

	// Public evidence is diagnostic, not authority for graph substitution.
	// The family handoff must revalidate these retained immutable observations
	// and analysis witnesses against the exact finalized Plan before sealing.
	analysis             *ActionPlanConfigDependencyAnalysis
	observed             *ActionPlanFamilyObservedHeaders
	cut                  *ActionPlanFamilyExecutionCut
	variant              string
	replayConfigFiles    map[string]string
	replayContractDigest [sha256.Size]byte
}

func (options ActionPlanFamilyVariantPlanningOptions) validate() error {
	if err := validatePlanName("family planning variant", options.Variant); err != nil {
		return err
	}
	present := 0
	for _, exists := range []bool{options.InitialSnapshot != nil, options.Cut != nil,
		options.ObservedHeaders != nil, options.ResolvedConfigFiles != nil} {
		if exists {
			present++
		}
	}
	if present != 0 && present != 4 {
		return fmt.Errorf("family planning replay requires initial snapshot, execution cut, observed headers and resolved config files together")
	}
	if options.PrepareCompilerGuards != nil && options.Cut == nil {
		return fmt.Errorf("supplemental compiler guards require verified family replay")
	}
	if options.CaptureCheckpoint != nil && options.Cut != nil {
		return fmt.Errorf("checkpoint capture is only valid during initial family planning")
	}
	return nil
}

// ActionPlanFamilyVariant uses the ordinary lowering/finalization pipeline,
// adding encountered generated-header demands initially or authenticated
// observed-header analysis during replay. Compiler probe discovery remains a
// separate unchanged phase; this method never substitutes a prepared reparse.
func (m *CompactMetadata) ActionPlanFamilyVariant(
	targetToolsetIdentity, hostToolsetIdentity string,
	options ActionPlanFamilyVariantPlanningOptions,
) (*ActionPlanFamilyVariantPlanningResult, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	return m.actionPlanWithPlanningOptions(targetToolsetIdentity, hostToolsetIdentity, true, options.Cache, &options)
}

// DiscoverVerifiedCompilerGuards runs the same fresh lowering, replay gates and
// original-source scanner as ActionPlanFamilyVariant, but returns no finalized
// plan or source-closure receipt. The preparation callback must install the
// separately replayed compiler facts and discovery observer before analysis.
func (m *CompactMetadata) DiscoverVerifiedCompilerGuards(
	targetToolsetIdentity, hostToolsetIdentity string,
	options ActionPlanFamilyVariantPlanningOptions,
) error {
	if err := options.validate(); err != nil {
		return err
	}
	if options.Cut == nil || options.PrepareCompilerGuards == nil {
		return fmt.Errorf("compiler guard discovery requires complete verified replay inputs and a preparation callback")
	}
	_, err := m.prepareActionPlanWithPlanningOptions(targetToolsetIdentity, hostToolsetIdentity, true, options.Cache, &options)
	return err
}

func (m *CompactMetadata) actionPlan(
	targetToolsetIdentity, hostToolsetIdentity string,
	analyzeConfigDependencies bool,
	familyCache *ActionPlanFamilyPlanningCache,
) (*ActionPlan, map[string]ConfigDependencySet, error) {
	result, err := m.actionPlanWithPlanningOptions(targetToolsetIdentity, hostToolsetIdentity, analyzeConfigDependencies, familyCache, nil)
	if err != nil {
		return nil, nil, err
	}
	return result.Plan, result.Dependencies, nil
}

// actionPlanPreparedAnalysis is private, provisional planning state. It cannot
// be published as an ActionPlanFamilyVariantPlanningResult before the ordinary
// content-addressing, replay sealing and final-ID annotation pass below.
type actionPlanPreparedAnalysis struct {
	plan           *ActionPlan
	analysis       *ActionPlanConfigDependencyAnalysis
	verifiedReplay *ActionPlanFamilyVerifiedReplay
}

func (m *CompactMetadata) prepareActionPlanWithPlanningOptions(
	targetToolsetIdentity, hostToolsetIdentity string,
	analyzeConfigDependencies bool,
	familyCache *ActionPlanFamilyPlanningCache,
	variantOptions *ActionPlanFamilyVariantPlanningOptions,
) (*actionPlanPreparedAnalysis, error) {
	plan, selections, err := m.lowerSelectedActionPlan(targetToolsetIdentity, hostToolsetIdentity, false, familyCache)
	if err != nil {
		return nil, err
	}
	if err := m.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		return nil, err
	}
	if !m.selectedProductsOnly {
		if err := m.appendTerminalActionPlanNodes(plan, selections); err != nil {
			return nil, err
		}
		if err := m.appendModuleSDKActionPlanNodes(plan); err != nil {
			return nil, err
		}
	}
	if analyzeConfigDependencies {
		if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
			return nil, err
		}
	} else {
		plan.projectedGeneratorCandidates = nil
	}
	if variantOptions != nil && variantOptions.CaptureCheckpoint != nil {
		if err := variantOptions.CaptureCheckpoint(plan); err != nil {
			return nil, fmt.Errorf("capture lowered family checkpoint: %w", err)
		}
	}
	return analyzePreparedActionPlan(plan, analyzeConfigDependencies, familyCache, variantOptions)
}

func analyzePreparedActionPlan(
	plan *ActionPlan,
	analyzeConfigDependencies bool,
	familyCache *ActionPlanFamilyPlanningCache,
	variantOptions *ActionPlanFamilyVariantPlanningOptions,
) (*actionPlanPreparedAnalysis, error) {
	var analysis *ActionPlanConfigDependencyAnalysis
	var verifiedReplay *ActionPlanFamilyVerifiedReplay
	var err error
	if analyzeConfigDependencies {
		var dependencyCache *ActionPlanConfigDependencySharedCache
		if familyCache != nil {
			dependencyCache = familyCache.configDependencyCache()
		}
		switch {
		case variantOptions != nil && variantOptions.Cut != nil:
			// Validate the complete original lowering before any observed bytes
			// can affect scanner state. Verification addresses only a detached
			// copy; the live selection graph remains provisional for analysis.
			replay, verifyErr := variantOptions.Cut.VerifyVariantReplay(variantOptions.Variant,
				*variantOptions.InitialSnapshot, plan, variantOptions.ResolvedConfigFiles)
			if verifyErr != nil {
				return nil, verifyErr
			}
			verifiedReplay = replay
			if variantOptions.PrepareCompilerGuards != nil {
				if err := variantOptions.PrepareCompilerGuards(); err != nil {
					return nil, fmt.Errorf("prepare supplemental compiler guards: %w", err)
				}
			}
			analysis, err = BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, dependencyCache, replay, variantOptions.ObservedHeaders)
		case variantOptions != nil:
			analysis, err = BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, dependencyCache)
		default:
			analysis, err = BuildActionPlanConfigDependencyAnalysisWithCache(plan, dependencyCache)
		}
		if err != nil {
			return nil, err
		}
	}
	return &actionPlanPreparedAnalysis{plan: plan, analysis: analysis, verifiedReplay: verifiedReplay}, nil
}

func (m *CompactMetadata) actionPlanWithPlanningOptions(
	targetToolsetIdentity, hostToolsetIdentity string,
	analyzeConfigDependencies bool,
	familyCache *ActionPlanFamilyPlanningCache,
	variantOptions *ActionPlanFamilyVariantPlanningOptions,
) (*ActionPlanFamilyVariantPlanningResult, error) {
	prepared, err := m.prepareActionPlanWithPlanningOptions(targetToolsetIdentity, hostToolsetIdentity, analyzeConfigDependencies, familyCache, variantOptions)
	if err != nil {
		return nil, err
	}
	return finalizePreparedActionPlan(prepared, variantOptions)
}

func finalizePreparedActionPlan(prepared *actionPlanPreparedAnalysis, variantOptions *ActionPlanFamilyVariantPlanningOptions) (*ActionPlanFamilyVariantPlanningResult, error) {
	plan, analysis, verifiedReplay := prepared.plan, prepared.analysis, prepared.verifiedReplay
	var replayContractDigest [sha256.Size]byte
	var err error
	if err := contentAddressActionPlanNodes(plan); err != nil {
		return nil, err
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	if verifiedReplay != nil {
		replayContractDigest, err = verifiedReplay.sealAnalysis(plan, variantOptions.ResolvedConfigFiles)
		if err != nil {
			return nil, err
		}
	}
	result := &ActionPlanFamilyVariantPlanningResult{Plan: plan, analysis: analysis, replayContractDigest: replayContractDigest}
	if analysis == nil {
		return result, nil
	}
	result.Dependencies, err = analysis.ByNodeID(plan)
	if err != nil {
		return nil, err
	}
	if variantOptions != nil {
		result.observed, result.cut = variantOptions.ObservedHeaders, variantOptions.Cut
		if variantOptions.Cut != nil {
			result.variant = variantOptions.Variant
			result.replayConfigFiles = maps.Clone(variantOptions.ResolvedConfigFiles)
		}
		result.GeneratedHeaderDemands, err = analysis.GeneratedHeaderDemandsByNodeID(plan)
		if err != nil {
			return nil, err
		}
		result.ObservedHeaderUses, err = analysis.ObservedHeaderUsesByNodeID(plan)
		if err != nil {
			return nil, err
		}
		if variantOptions.Cut != nil {
			// Replay retains all variant results until symmetric reduction. Do
			// not keep four source evaluators, selection graphs and topology
			// caches alive just to retain their finalized execution contracts.
			// The public graph and projection commitments own those contracts;
			// the immutable radix store can still be shared across variants.
			result.Plan = &ActionPlan{
				Toolsets: plan.Toolsets, Sources: plan.Sources, Recipes: plan.Recipes,
				Nodes: plan.Nodes, InputSets: plan.InputSets, Products: plan.Products,
				inputSetStore:                     plan.inputSetStore,
				projectedGeneratorValidations:     plan.projectedGeneratorValidations,
				projectedGeneratorInternalNodes:   plan.projectedGeneratorInternalNodes,
				projectedGeneratorInternalOutputs: plan.projectedGeneratorInternalOutputs,
				projectedGeneratorOriginalOutputs: plan.projectedGeneratorOriginalOutputs,
			}
		}
	}
	return result, nil
}

func contentAddressActionPlanNodes(plan *ActionPlan) error {
	inputSets, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return fmt.Errorf("load action-plan input sets: %w", err)
	}
	indexByOldID := make(map[string]int, len(plan.Nodes))
	oldIDs := make([]string, len(plan.Nodes))
	for index, node := range plan.Nodes {
		if _, exists := indexByOldID[node.ID]; exists {
			return fmt.Errorf("semantic graph repeats provisional node ID %q", node.ID)
		}
		indexByOldID[node.ID] = index
		oldIDs[index] = node.ID
	}
	knownIDs := func() string {
		known := slices.Clone(oldIDs)
		sort.Strings(known)
		return strings.Join(known, ",")
	}

	// Find one dependency-first action order in the combined action/input-set
	// graph. Treating persistent radix nodes as graph vertices visits every
	// shared subtree once; flattening each consumer's producer closure would turn
	// cumulative working-tree roots back into quadratic planner work.
	const (
		unvisited uint8 = iota
		visiting
		resolved
	)
	const (
		actionVertex uint8 = iota
		inputSetVertex
	)
	type addressVertex struct {
		kind      uint8
		nodeIndex int
		inputSet  string
	}
	type addressFrame struct {
		vertex    addressVertex
		neighbors []addressVertex
		next      int
	}
	actionStates := make([]uint8, len(plan.Nodes))
	inputSetStates := map[string]uint8{}
	vertexState := func(vertex addressVertex) uint8 {
		if vertex.kind == actionVertex {
			return actionStates[vertex.nodeIndex]
		}
		return inputSetStates[vertex.inputSet]
	}
	setVertexState := func(vertex addressVertex, state uint8) {
		if vertex.kind == actionVertex {
			actionStates[vertex.nodeIndex] = state
		} else {
			inputSetStates[vertex.inputSet] = state
		}
	}
	neighbors := func(vertex addressVertex) ([]addressVertex, error) {
		if vertex.kind == actionVertex {
			node := plan.Nodes[vertex.nodeIndex]
			result := make([]addressVertex, 0, len(node.Inputs)+1)
			for _, input := range node.Inputs {
				producerIndex, ok := indexByOldID[input.ProducerID]
				if !ok {
					return nil, fmt.Errorf("node references unknown provisional producer %s (known %s)", input.ProducerID, knownIDs())
				}
				result = append(result, addressVertex{kind: actionVertex, nodeIndex: producerIndex})
			}
			if node.InputSet != "" {
				result = append(result, addressVertex{kind: inputSetVertex, inputSet: node.InputSet})
			}
			return result, nil
		}
		inputSetNode, ok := inputSets.Node(vertex.inputSet)
		if !ok {
			return nil, fmt.Errorf("semantic graph references unknown input-set node %s", vertex.inputSet)
		}
		result := make([]addressVertex, 0, len(inputSetNode.Children)+len(inputSetNode.Entries))
		for _, child := range inputSetNode.Children {
			result = append(result, addressVertex{kind: inputSetVertex, inputSet: child.ID})
		}
		for _, entry := range inputSetNode.Entries {
			if entry.ProducerID == "" {
				continue
			}
			producerIndex, ok := indexByOldID[entry.ProducerID]
			if !ok {
				return nil, fmt.Errorf("input set references unknown provisional producer %s (known %s)", entry.ProducerID, knownIDs())
			}
			result = append(result, addressVertex{kind: actionVertex, nodeIndex: producerIndex})
		}
		return result, nil
	}
	order := make([]int, 0, len(plan.Nodes))
	for root := range plan.Nodes {
		rootVertex := addressVertex{kind: actionVertex, nodeIndex: root}
		if vertexState(rootVertex) == resolved {
			continue
		}
		rootNeighbors, err := neighbors(rootVertex)
		if err != nil {
			return err
		}
		setVertexState(rootVertex, visiting)
		stack := []addressFrame{{vertex: rootVertex, neighbors: rootNeighbors}}
		for len(stack) != 0 {
			frame := &stack[len(stack)-1]
			if frame.next == len(frame.neighbors) {
				setVertexState(frame.vertex, resolved)
				if frame.vertex.kind == actionVertex {
					order = append(order, frame.vertex.nodeIndex)
				}
				stack = stack[:len(stack)-1]
				continue
			}
			next := frame.neighbors[frame.next]
			frame.next++
			switch vertexState(next) {
			case visiting:
				if next.kind == actionVertex {
					return fmt.Errorf("semantic graph contains a cycle at %s", oldIDs[next.nodeIndex])
				}
				return fmt.Errorf("semantic graph contains a cycle through input-set node %s", next.inputSet)
			case resolved:
				continue
			}
			nextNeighbors, err := neighbors(next)
			if err != nil {
				return err
			}
			setVertexState(next, visiting)
			stack = append(stack, addressFrame{vertex: next, neighbors: nextNeighbors})
		}
	}

	// Assign identities in that order. Only Inputs change while identities are
	// assigned, so retain the planner's existing backing storage for every other
	// (often large) descriptor.
	finalIDs := make([]string, len(plan.Nodes))
	addressed := make([]bool, len(plan.Nodes))
	inputSetMapper, err := inputSets.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if entry.ProducerID == "" {
			return entry, nil
		}
		producerIndex, ok := indexByOldID[entry.ProducerID]
		if !ok || !addressed[producerIndex] {
			return ActionPlanInputSetEntry{}, fmt.Errorf("input set references unresolved producer %s", entry.ProducerID)
		}
		entry.ProducerID = finalIDs[producerIndex]
		return entry, nil
	})
	if err != nil {
		return err
	}
	for _, nodeIndex := range order {
		node := &plan.Nodes[nodeIndex]
		node.Inputs = slices.Clone(node.Inputs)
		for inputIndex := range node.Inputs {
			producerIndex := indexByOldID[node.Inputs[inputIndex].ProducerID]
			if !addressed[producerIndex] {
				return fmt.Errorf("node %s references unresolved producer %s", oldIDs[nodeIndex], node.Inputs[inputIndex].ProducerID)
			}
			node.Inputs[inputIndex].ProducerID = finalIDs[producerIndex]
		}
		if node.InputSet != "" {
			root, err := inputSetMapper.Map(node.InputSet)
			if err != nil {
				return err
			}
			node.InputSet = root
		}
		node.ID = node.ContentID()
		finalIDs[nodeIndex] = node.ID
		addressed[nodeIndex] = true
	}
	resolvedNode := func(oldID string) (ActionPlanNode, bool) {
		index, ok := indexByOldID[oldID]
		if !ok || !addressed[index] {
			return ActionPlanNode{}, false
		}
		return plan.Nodes[index], true
	}
	validations := make([]string, 0, len(plan.projectedGeneratorValidations))
	for _, oldID := range plan.projectedGeneratorValidations {
		node, ok := resolvedNode(oldID)
		if !ok {
			return fmt.Errorf("projected-generator validation references unknown node %s", oldID)
		}
		validations = append(validations, node.ID)
	}
	internal := make(map[string]bool, len(plan.projectedGeneratorInternalNodes))
	for oldID := range plan.projectedGeneratorInternalNodes {
		node, ok := resolvedNode(oldID)
		if !ok {
			return fmt.Errorf("projected-generator internal set references unknown node %s", oldID)
		}
		internal[node.ID] = true
	}
	internalOutputs := make(map[actionPlanOutputRef]bool, len(plan.projectedGeneratorInternalOutputs))
	for ref := range plan.projectedGeneratorInternalOutputs {
		node, ok := resolvedNode(ref.producerID)
		if !ok {
			return fmt.Errorf("projected-generator internal output references unknown node %s", ref.producerID)
		}
		if ref.slot < 0 || ref.slot >= len(node.Outputs) {
			return fmt.Errorf("projected-generator internal output references absent slot %s[%d]", ref.producerID, ref.slot)
		}
		internalOutputs[actionPlanOutputRef{producerID: node.ID, slot: ref.slot}] = true
	}
	commitments := make(map[string]projectedGeneratorOriginalOutputCommitment, len(plan.projectedGeneratorOriginalOutputs))
	for oldID, commitment := range plan.projectedGeneratorOriginalOutputs {
		node, ok := resolvedNode(oldID)
		if !ok {
			return fmt.Errorf("projected-generator output commitment references unknown node %s", oldID)
		}
		commitment.Outputs = slices.Clone(commitment.Outputs)
		commitments[node.ID] = commitment
	}
	plan.projectedGeneratorValidations = validations
	plan.projectedGeneratorInternalNodes = internal
	plan.projectedGeneratorInternalOutputs = internalOutputs
	plan.projectedGeneratorOriginalOutputs = commitments
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		return fmt.Errorf("export action-plan input sets: %w", err)
	}
	plan.invalidateLookupIndexes()
	return nil
}

func containsUnresolvedPlanMakeReference(value string) bool {
	if !strings.Contains(value, "$") {
		return false
	}
	remaining := withoutActionPlanTreePlaceholders(value)
	return strings.Contains(remaining, "$(") || strings.Contains(remaining, "${")
}

func withoutActionPlanTreePlaceholders(value string) string {
	if !strings.Contains(value, "${tree:") {
		return value
	}
	return actionRecipePlaceholder.ReplaceAllStringFunc(value, func(placeholder string) string {
		match := actionRecipePlaceholder.FindStringSubmatch(placeholder)
		if len(match) == 3 && match[1] == "tree" {
			return ""
		}
		return placeholder
	})
}

func escapeKbuildIdentifier(value string) string {
	var out strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' {
			out.WriteRune(char)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func expandPlanKbuildFlag(value string, config map[string]string, object string) (string, error) {
	if !strings.Contains(value, "$") {
		return value, nil
	}
	remaining := value
	var out strings.Builder
	for {
		start := strings.Index(remaining, "$(")
		brace := strings.Index(remaining, "${")
		if start < 0 || (brace >= 0 && brace < start) {
			start = brace
		}
		if start < 0 {
			out.WriteString(remaining)
			return out.String(), nil
		}
		out.WriteString(remaining[:start])
		closer := byte(')')
		if remaining[start+1] == '{' {
			closer = '}'
		}
		end := strings.IndexByte(remaining[start+2:], closer)
		if end < 0 {
			return "", fmt.Errorf("unterminated make variable")
		}
		end += start + 2
		name := remaining[start+2 : end]
		replacement := ""
		switch name {
		case "obj":
			replacement = "${tree:prep}"
			if dir := pathDir(object); dir != "" && dir != "." {
				replacement += "/" + dir
			}
		case "src":
			replacement = "${tree:kernel}"
			if dir := pathDir(object); dir != "" && dir != "." {
				replacement += "/" + dir
			}
		case "srctree", "srcroot", "abs_srctree":
			replacement = "${tree:kernel}"
		case "objtree", "abs_output":
			replacement = "${tree:prep}"
		default:
			var ok bool
			replacement, ok = config[name]
			if !ok {
				return "", fmt.Errorf("unknown make variable %q", name)
			}
			if replacement == "n" {
				replacement = ""
			}
		}
		out.WriteString(replacement)
		remaining = remaining[end+1:]
	}
}

func pathDir(value string) string {
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[:index]
	}
	return "."
}
