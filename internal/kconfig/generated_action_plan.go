package kconfig

// Preparatory and host actions are selected by the evaluated Kbuild profiles.
// This file intentionally contains no architecture, compiler, configuration,
// or generated-file catalogue.  Every demanded target is recursively lowered
// from the captured rule/evaluator graph by compactKbuildRulePlanBuilder.

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
)

type compactKbuildSideOutputObservationKey struct {
	candidate compactKbuildSelectionKey
	path      string
}

type compactKbuildSideOutputDemandStateKey struct {
	consumer compactKbuildSelectionKey
	tree     string
	path     string
}

// appendGeneratedActionPlan materializes every profile-exact Kbuild selection.
// Generated assignments are Kbuild's own declaration of the demanded closure;
// the generic rule builder follows their concrete rules and prerequisites.
// Config artifacts are input projections, not generated policy, and are copied
// into the object-tree layout expected by Kbuild.
func (m *CompactMetadata) appendGeneratedActionPlan(
	plan *ActionPlan,
) (*compactKbuildSelectionGraph, error) {
	if m == nil {
		return nil, fmt.Errorf("selected Kbuild action plan requires one resolved config")
	}
	selectionGraph, err := newCompactKbuildSelectionGraph(m.Config)
	if err != nil {
		return nil, err
	}
	plan.selectionGraph = selectionGraph
	if m.configFragment == nil {
		return nil, fmt.Errorf("selected Kbuild action plan requires a resolved config fragment")
	}
	if err := selectionGraph.prepareGroupedSelections(m, m.Config); err != nil {
		return nil, err
	}
	b := generatedPlanBuilder{metadata: m, plan: plan, config: m.Config, fragment: maps.Clone(m.configFragment)}
	if !m.preconfiguredObjectTree {
		if err := b.internConfigProjectionSources(); err != nil {
			return nil, err
		}
	}
	sideOutputDemands, err := selectionGraph.compactKbuildSideOutputDemands(m, m.Config)
	if err != nil {
		return nil, err
	}
	if err := selectionGraph.compactKbuildRegisterSideOutputCandidateDependencies(sideOutputDemands); err != nil {
		return nil, err
	}
	demandsByConsumer := map[compactKbuildSelectionKey][]compactKbuildSideOutputDemand{}
	observationsByCandidate := map[compactKbuildSelectionKey][]compactKbuildObservedOutput{}
	observationByKey := map[compactKbuildSideOutputObservationKey]compactKbuildObservedOutput{}
	candidateSetsByPath := map[string]map[compactKbuildSelectionKey]bool{}
	for _, demand := range sideOutputDemands {
		demandsByConsumer[demand.consumer] = append(demandsByConsumer[demand.consumer], demand)
		for _, candidate := range demand.candidates {
			candidate = selectionGraph.compactKbuildGroupedSelectionRepresentative(candidate)
			observation, err := compactKbuildSideOutputStateObservation(demand, candidate)
			if err != nil {
				return nil, err
			}
			key := compactKbuildSideOutputObservationKey{candidate: candidate, path: demand.output.Path}
			if previous, exists := observationByKey[key]; exists {
				if previous.path != observation.path || previous.output != observation.output {
					return nil, fmt.Errorf("side-output observation %s/%q has conflicting capture identities", compactKbuildSelectionKeyString(candidate), demand.output.Path)
				}
			} else {
				observationByKey[key] = observation
				observationsByCandidate[candidate] = append(observationsByCandidate[candidate], observation)
			}
			if candidateSetsByPath[demand.output.Path] == nil {
				candidateSetsByPath[demand.output.Path] = map[compactKbuildSelectionKey]bool{}
			}
			candidateSetsByPath[demand.output.Path][candidate] = true
		}
	}
	for candidate := range observationsByCandidate {
		sort.Slice(observationsByCandidate[candidate], func(i, j int) bool {
			left, right := observationsByCandidate[candidate][i], observationsByCandidate[candidate][j]
			if left.path != right.path {
				return left.path < right.path
			}
			return actionPlanLookupKey(left.output.Tree, left.output.Path) < actionPlanLookupKey(right.output.Tree, right.output.Path)
		})
	}
	stateGraphsByPath := map[string]compactKbuildSideOutputStateGraph{}
	for observedPath, candidateSet := range candidateSetsByPath {
		candidates := make([]compactKbuildSelectionKey, 0, len(candidateSet))
		for candidate := range candidateSet {
			candidates = append(candidates, candidate)
		}
		stateGraph, graphErr := selectionGraph.compactKbuildSideOutputStateGraph(m, candidates)
		if graphErr != nil {
			return nil, fmt.Errorf("project Kbuild side-output state for %q: %w", observedPath, graphErr)
		}
		stateGraphsByPath[observedPath] = stateGraph
	}
	maximalCandidatesByDemand := map[compactKbuildSideOutputDemandStateKey][]compactKbuildSelectionKey{}
	for _, demand := range sideOutputDemands {
		stateGraph, graphErr := selectionGraph.compactKbuildSideOutputStateGraph(m, demand.candidates)
		if graphErr != nil {
			return nil, fmt.Errorf("project final Kbuild side-output state for %q: %w", demand.output.Path, graphErr)
		}
		key := compactKbuildSideOutputDemandStateKey{
			consumer: selectionGraph.compactKbuildGroupedSelectionRepresentative(demand.consumer),
			tree:     demand.output.Tree,
			path:     demand.output.Path,
		}
		if previous, exists := maximalCandidatesByDemand[key]; exists && !slices.Equal(previous, stateGraph.maximal) {
			return nil, fmt.Errorf("grouped Kbuild side-output demand for %q has conflicting maximal state frontiers", demand.output.Path)
		}
		maximalCandidatesByDemand[key] = append([]compactKbuildSelectionKey(nil), stateGraph.maximal...)
	}
	observedStates := map[string]compactKbuildRuleInput{}

	ordered, err := selectionGraph.materializationOrder(m)
	if err != nil {
		return nil, err
	}
	// Rule/closure caches are only needed to derive side-output ownership and
	// topological order. Do not retain that duplicate graph while the complete
	// action plan is lowered; the structural selection indexes remain intact
	// for product validation and can repopulate the caches on demand.
	selectionGraph.releasePlanningCaches()
	defer func() {
		// Some selected roots are control-only, forwarding, grouped peers, or
		// otherwise skipped below. Do not retain their one-shot rule graphs in
		// the completed ActionPlan after all materializable roots have had a
		// chance to consume their exact match.
		selectionGraph.selectedRootRuleResolutions = make(
			map[compactKbuildSelectedRuleResolutionKey]compactKbuildRuleResolution,
		)
	}()
	for _, selection := range ordered {
		profile, ok := selectionGraph.profile(selection.Profile)
		if !ok {
			return nil, fmt.Errorf("Kbuild demand references missing profile %q", selection.Profile)
		}
		target := compactKbuildGraphTargetPath(selection.Target)
		if target == "" {
			return nil, fmt.Errorf("profile %q contains an empty selected target", profile.Name)
		}
		selectionKey := compactKbuildSelectionKey{profile: selection.Profile, target: target, stage: selection.Stage}
		if selectionGraph.forwardingSelections[selectionKey] {
			continue
		}
		if _, materialized := selectionGraph.materializedProducers[selectionKey]; materialized {
			// A grouped rule records its one producer against every selected peer.
			// The first peer in dependency order already published this target.
			continue
		}
		// A .PHONY prerequisite is an ordering/control-flow node, not a
		// filesystem artifact. Its prerequisite and recursive-Make closures
		// are ordered above by their selected native artifact owners.
		if selectionGraph.compactKbuildProfileTargetIsPhony(profile, target) {
			continue
		}
		setupOnly := false
		if !selectionGraph.hasSelectedRootRuleResolution(
			m, selectionKey, profile, target, selection.MakeTarget,
		) {
			setupOnly, err = m.compactKbuildTargetIsOrderingOnlyInProfileForMakeTarget(
				profile, target, selection.MakeTarget,
			)
			if err != nil {
				return nil, fmt.Errorf("classify evaluated %s target %q: %w", selection.Stage, target, err)
			}
		}
		if setupOnly {
			// Preserve the selected Make ordering edge without inventing a file
			// for a source-declared control target.
			continue
		}
		context := compactKbuildSelectionPlanContext(m.Config, selection)
		initialArtifacts, err := compactKbuildSelectionInitialObjectTreeArtifacts(selection)
		if err != nil {
			return nil, fmt.Errorf("decode evaluated %s target %q initial object-tree artifacts: %w", selection.Stage, target, err)
		}
		generatedArtifacts, err := compactKbuildSelectionGeneratedObjectTreeArtifacts(selection)
		if err != nil {
			return nil, fmt.Errorf("decode evaluated %s target %q generated object-tree artifacts: %w", selection.Stage, target, err)
		}
		groupMembers := selectionGraph.compactKbuildGroupedSelectionMembers(selectionKey)
		resolvedSideOutputs := map[string]compactKbuildRuleInput{}
		groupDemands := map[string]compactKbuildSideOutputDemand{}
		for _, member := range groupMembers {
			for _, demand := range demandsByConsumer[member] {
				canonical := demand
				canonical.consumer = selectionGraph.compactKbuildGroupedSelectionRepresentative(selectionKey)
				if previous, exists := groupDemands[demand.output.Path]; exists {
					sameCandidates := len(previous.candidates) == len(canonical.candidates)
					if sameCandidates {
						for index := range previous.candidates {
							if previous.candidates[index] != canonical.candidates[index] {
								sameCandidates = false
								break
							}
						}
					}
					if previous.stage != canonical.stage || previous.product != canonical.product ||
						previous.output != canonical.output || !sameCandidates {
						return nil, fmt.Errorf(
							"grouped Kbuild selection %s has incompatible side-output demands for %q",
							compactKbuildSelectionKeyString(selectionKey), demand.output.Path,
						)
					}
					continue
				}
				groupDemands[demand.output.Path] = canonical
			}
		}
		groupDemandPaths := make([]string, 0, len(groupDemands))
		for sideOutputPath := range groupDemands {
			groupDemandPaths = append(groupDemandPaths, sideOutputPath)
		}
		sort.Strings(groupDemandPaths)
		for _, sideOutputPath := range groupDemandPaths {
			demand := groupDemands[sideOutputPath]
			demandStateKey := compactKbuildSideOutputDemandStateKey{
				consumer: selectionGraph.compactKbuildGroupedSelectionRepresentative(demand.consumer),
				tree:     demand.output.Tree,
				path:     demand.output.Path,
			}
			maximalCandidates, exists := maximalCandidatesByDemand[demandStateKey]
			if !exists || len(maximalCandidates) == 0 {
				return nil, fmt.Errorf("Kbuild side output %q has no maximal candidate state frontier", demand.output.Path)
			}
			states := make([]compactKbuildSideOutputCandidateState, 0, len(maximalCandidates))
			for _, candidate := range maximalCandidates {
				observation, observationErr := compactKbuildSideOutputStateObservation(demand, candidate)
				if observationErr != nil {
					return nil, observationErr
				}
				state, available := observedStates[actionPlanLookupKey(observation.output.Tree, observation.output.Path)]
				if !available {
					return nil, fmt.Errorf(
						"Kbuild side output %q consumer %s reached materialization before candidate %s capture %s/%s",
						demand.output.Path, compactKbuildSelectionKeyString(demand.consumer),
						compactKbuildSelectionKeyString(candidate), observation.output.Tree, observation.output.Path,
					)
				}
				states = append(states, compactKbuildSideOutputCandidateState{
					candidate: candidate,
					input:     state,
				})
			}
			nativeDemand := demand
			if demand.consumer.stage == "prehost" || demand.consumer.stage == "bootstrap" {
				nativeDemand.stage = demand.consumer.stage
				nativeDemand.product = "sdk"
				nativeDemand.output.Tree = demand.consumer.stage
			}
			resolved, resolveErr := appendCompactKbuildSideOutputResolver(
				plan, selectionGraph, m, nativeDemand, states,
			)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if previous, exists := resolvedSideOutputs[demand.output.Path]; exists &&
				(previous.producer != resolved.producer || previous.slot != resolved.slot) {
				return nil, fmt.Errorf(
					"grouped Kbuild selection %s has multiple side-output resolvers for %q",
					compactKbuildSelectionKeyString(selectionKey), demand.output.Path,
				)
			}
			resolvedSideOutputs[demand.output.Path] = resolved
		}
		observations := []compactKbuildObservedOutput{}
		seenObservations := map[string]bool{}
		for _, member := range groupMembers {
			candidate := selectionGraph.compactKbuildGroupedSelectionRepresentative(member)
			for _, observation := range observationsByCandidate[candidate] {
				key := actionPlanLookupKey(observation.output.Tree, observation.output.Path)
				if seenObservations[key] {
					continue
				}
				stateGraph, exists := stateGraphsByPath[observation.path]
				if !exists {
					return nil, fmt.Errorf("observed Kbuild path %q has no candidate state graph", observation.path)
				}
				for _, parent := range stateGraph.parents[candidate] {
					parentObservation, exists := observationByKey[compactKbuildSideOutputObservationKey{
						candidate: parent, path: observation.path,
					}]
					if !exists {
						return nil, fmt.Errorf(
							"observed Kbuild path %q candidate %s has unregistered parent %s",
							observation.path, compactKbuildSelectionKeyString(candidate), compactKbuildSelectionKeyString(parent),
						)
					}
					state, available := observedStates[actionPlanLookupKey(parentObservation.output.Tree, parentObservation.output.Path)]
					if !available {
						return nil, fmt.Errorf(
							"observed Kbuild path %q candidate %s reached materialization before parent %s state",
							observation.path, compactKbuildSelectionKeyString(candidate), compactKbuildSelectionKeyString(parent),
						)
					}
					observation.baseInputs = append(observation.baseInputs, state)
				}
				seenObservations[key] = true
				observations = append(observations, observation)
			}
		}
		builder := newCompactKbuildRulePlanBuilder(m, plan).
			withSelectionGraph(selectionGraph).
			forSelection(selectionKey, profile).
			forOutput(context.Stage, context.OutputTree, context.Product).
			withInitialObjectTree(selection.UsesInitialObjectTree, initialArtifacts...).
			withGeneratedObjectTreeArtifacts(generatedArtifacts...).
			withResolvedSideOutputs(resolvedSideOutputs)
		builder, err = builder.forObservedOutputs(target, observations)
		if err != nil {
			return nil, fmt.Errorf("declare evaluated %s target %q side-output observations: %w", selection.Stage, target, err)
		}
		producer, buildErr := builder.buildSelectedTarget(target, selection.MakeTarget)
		if buildErr != nil {
			return nil, fmt.Errorf("materialize evaluated %s target %q: %w", selection.Stage, target, buildErr)
		}
		if err := selectionGraph.recordMaterializedProducer(selectionKey, producer); err != nil {
			return nil, err
		}
		producerNode, exists := compactKbuildPlanNode(plan, producer)
		if !exists {
			return nil, fmt.Errorf("materialized Kbuild selection %s has no action-plan node %q", compactKbuildSelectionKeyString(selectionKey), producer)
		}
		for slot, output := range producerNode.Outputs {
			if output.ObservedPath == "" {
				continue
			}
			key := actionPlanLookupKey(output.Tree, output.Path)
			state := compactKbuildRuleInput{path: output.Path, producer: producer, slot: slot}
			if previous, exists := observedStates[key]; exists &&
				(previous.producer != state.producer || previous.slot != state.slot) {
				return nil, fmt.Errorf("observed Kbuild state %s/%s has multiple producers", output.Tree, output.Path)
			}
			observedStates[key] = state
		}
		if err := appendCompactKbuildBootstrapProjections(plan, m.Config, selection, producer); err != nil {
			return nil, err
		}
		if err := appendCompactKbuildHostPrepMirrors(plan, selection, producer); err != nil {
			return nil, err
		}
	}
	if !m.preconfiguredObjectTree {
		if err := b.appendMissingConfigProjections(); err != nil {
			return nil, err
		}
	}
	plan.releaseProbeDiscoveryPayloads()
	return selectionGraph, nil
}

func compactKbuildSelectionPlanContext(config CompactConfig, selection CompactKbuildSelection) compactKbuildRulePlanContext {
	switch selection.Stage {
	case "prehost":
		return compactKbuildRulePlanContext{Stage: "prehost", OutputTree: "prehost", Product: "sdk"}
	case "bootstrap":
		return compactKbuildRulePlanContext{Stage: "bootstrap", OutputTree: "bootstrap", Product: "sdk"}
	case "host":
		return compactKbuildRulePlanContext{Stage: "host", OutputTree: "host", Product: "sdk"}
	case "prep":
		return compactKbuildRulePlanContext{Stage: "prep", OutputTree: "prep", Product: "sdk"}
	}
	context := defaultCompactKbuildRulePlanContext
	switch {
	case selection.Target == "vmlinux" || selection.Target == "System.map":
		context.OutputTree = "vmlinux"
		context.Product = "vmlinux"
	case selection.Target == config.imageTarget:
		context.OutputTree = "image"
		context.Product = "image"
	case strings.HasSuffix(selection.Target, ".ko"), selection.Target == "modules.order", strings.HasSuffix(selection.Target, "/modules.order"):
		context.OutputTree = "modules"
		context.Product = "modules"
	case selection.Target == "Module.symvers", strings.HasSuffix(selection.Target, "/Module.symvers"):
		context.OutputTree = "metadata"
		context.Product = "module_symvers"
	case selection.Target == "modules.builtin":
		context.OutputTree = "metadata"
		context.Product = "modules_builtin"
	case selection.Target == "modules.builtin.modinfo":
		context.OutputTree = "metadata"
		context.Product = "modules_builtin_modinfo"
	}
	return context
}

// ensureActionPlanSource interns a declared execution input. Kernel paths are
// resolved from the full source_files depset by the callback; config paths are
// exact planner outputs staged under the config namespace.
func ensureActionPlanSource(plan *ActionPlan, namespace, sourcePath string) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("cannot intern a source into a nil action plan")
	}
	if err := validatePlanName("source namespace", namespace); err != nil {
		return "", err
	}
	if err := validatePlanRelativePath("source", sourcePath); err != nil {
		return "", err
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return "", err
	}
	key := actionPlanLookupKey(namespace, sourcePath)
	if id, ok := plan.sourceIDs[key]; ok {
		return id, nil
	}
	if plan.maximumSourceID >= 99999999 {
		return "", fmt.Errorf("action plan source ID space is exhausted")
	}
	id := fmt.Sprintf("src-%08d", plan.maximumSourceID+1)
	plan.Sources = append(plan.Sources, ActionPlanSource{ID: id, Namespace: namespace, Path: sourcePath})
	plan.sourceIDs[key] = id
	plan.sourcesByID[id] = plan.Sources[len(plan.Sources)-1]
	plan.maximumSourceID++
	plan.sourceLookupCount = len(plan.Sources)
	return id, nil
}

func (m *CompactMetadata) ensureActionPlanSource(plan *ActionPlan, sourcePath string) (string, error) {
	namespace, err := m.actionPlanSourceNamespace(sourcePath)
	if err != nil {
		return "", err
	}
	return ensureActionPlanSource(plan, namespace, sourcePath)
}

func (m *CompactMetadata) actionPlanSourceNamespace(sourcePath string) (string, error) {
	namespace, selected, err := m.selectedActionPlanSourceNamespace(sourcePath)
	if err != nil {
		return "", err
	}
	if !selected {
		return "kernel", nil
	}
	return namespace, nil
}

// selectedActionPlanSourceNamespace returns the explicitly configured source
// namespace whose root contains sourcePath. The boolean distinguishes an
// explicit namespace from the implicit kernel-source default. A namespace is
// input-tree routing metadata only; physical profile roots provide existence
// evidence during graph discovery.
func (m *CompactMetadata) selectedActionPlanSourceNamespace(sourcePath string) (string, bool, error) {
	if err := validatePlanRelativePath("source", sourcePath); err != nil {
		return "", false, err
	}
	if err := m.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return "", false, err
	}
	// An exact object-tree leaf is a one-path overlay: it wins over a prefix at
	// the same graph path without claiming any of that prefix's descendants.
	if namespace, ok := m.exactSourcePaths[sourcePath]; ok {
		return namespace, true, nil
	}
	for prefix := sourcePath; prefix != "."; prefix = path.Dir(prefix) {
		if namespace, ok := m.sourceNamespacePrefixes[prefix]; ok {
			return namespace, true, nil
		}
	}
	return "", false, nil
}

func actionPlanSourceNamespacePrefix(rawPrefix string) (string, error) {
	if rawPrefix != strings.TrimSpace(rawPrefix) {
		return "", fmt.Errorf("invalid prefix %q", rawPrefix)
	}
	const sourceTreePrefix = "__LINUX_BZL_SOURCE_TREE__/"
	prefix := strings.TrimPrefix(rawPrefix, sourceTreePrefix)
	if prefix == rawPrefix && rawPrefix == strings.TrimSuffix(sourceTreePrefix, "/") {
		return "", fmt.Errorf("invalid prefix %q", rawPrefix)
	}
	if err := validatePlanRelativePath("source namespace prefix", prefix); err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(prefix, "/")
	if strings.HasPrefix(first, "__LINUX_BZL_") {
		return "", fmt.Errorf("source prefix %q uses reserved tree marker %q", rawPrefix, first)
	}
	return prefix, nil
}
func actionPlanExactSourceNamespacePath(rawPath string) (string, error) {
	if rawPath != strings.TrimSpace(rawPath) {
		return "", fmt.Errorf("invalid path %q", rawPath)
	}
	if err := validatePlanRelativePath("exact source namespace", rawPath); err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(rawPath, "/")
	if strings.HasPrefix(first, "__LINUX_BZL_") {
		return "", fmt.Errorf("exact source path %q uses reserved tree marker %q", rawPath, first)
	}
	return rawPath, nil
}

func (m *CompactMetadata) ensureActionPlanSourceNamespaceIndex() error {
	if m == nil {
		return nil
	}
	if m.sourceNamespaceIndexed {
		return m.sourceNamespaceIndexErr
	}
	m.sourceNamespaceIndexed = true
	m.sourceNamespacePrefixes = make(map[string]string, len(m.sourceNamespaces))
	m.exactSourcePaths = make(map[string]string, len(m.exactSourceNamespaces))
	index := func(
		kind string,
		configured, indexed map[string]string,
		normalize func(string) (string, error),
	) error {
		for rawPrefix, namespace := range configured {
			prefix, err := normalize(rawPrefix)
			if err != nil {
				return fmt.Errorf("%s namespace %q: %w", kind, namespace, err)
			}
			if err := validatePlanName(kind+" namespace", namespace); err != nil {
				return err
			}
			if existing, ok := indexed[prefix]; ok && existing != namespace {
				return fmt.Errorf("%s source path %q has ambiguous namespaces %q and %q", kind, prefix, existing, namespace)
			}
			indexed[prefix] = namespace
		}
		return nil
	}
	if err := index("source prefix", m.sourceNamespaces, m.sourceNamespacePrefixes, actionPlanSourceNamespacePrefix); err != nil {
		m.sourceNamespaceIndexErr = err
		return err
	}
	if err := index("exact source", m.exactSourceNamespaces, m.exactSourcePaths, actionPlanExactSourceNamespacePath); err != nil {
		m.sourceNamespaceIndexErr = err
		return err
	}
	return nil
}

func (m *CompactMetadata) hasExactActionPlanSourceNamespacePrefix(sourcePath string) (bool, error) {
	if err := validatePlanRelativePath("source namespace", sourcePath); err != nil {
		return false, err
	}
	if err := m.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return false, err
	}
	_, exact := m.exactSourcePaths[sourcePath]
	return exact, nil
}

func planProducerByOutput(plan *ActionPlan, tree, outputPath string) (string, int, bool) {
	if plan == nil {
		return "", 0, false
	}
	plan.ensureNodeLookupIndexes()
	if producer, ok := plan.outputProducers[actionPlanLookupKey(tree, outputPath)]; ok {
		return producer.producerID, producer.slot, true
	}
	return "", 0, false
}

type generatedPlanBuilder struct {
	metadata *CompactMetadata
	plan     *ActionPlan
	config   CompactConfig
	fragment map[string]string
}

type resolvedConfigProjection struct {
	input  string
	output string
}

func resolvedConfigProjections() []resolvedConfigProjection {
	return []resolvedConfigProjection{
		{input: ".config", output: ".config"},
		{input: "auto.conf", output: "include/config/auto.conf"},
		{input: "auto.conf.cmd", output: "include/config/auto.conf.cmd"},
		{input: "autoconf.h", output: "include/generated/autoconf.h"},
		{input: "rustc_cfg", output: "include/generated/rustc_cfg"},
		{input: "kernel.release", output: "include/config/kernel.release"},
	}
}

// ResolvedConfigProjectionOutputs returns the object-tree paths already
// supplied by the Kconfig replay action. Kbuild goal discovery treats them as
// satisfied nodes, exactly as GNU Make would treat existing generated config
// files, so it cannot descend into the obsolete syncconfig/conf tool graph.
func ResolvedConfigProjectionOutputs() []string {
	projections := resolvedConfigProjections()
	outputs := make([]string, 0, len(projections))
	for _, projection := range projections {
		outputs = append(outputs, projection.output)
	}
	return outputs
}

func (b *generatedPlanBuilder) add(node ActionPlanNode, recipe ActionRecipe) (string, error) {
	return appendActionPlanNode(b.plan, node, recipe)
}

func (b *generatedPlanBuilder) addSource(node *ActionPlanNode, recipe *ActionRecipe, role, namespace, sourcePath string) (string, error) {
	id, err := ensureActionPlanSource(b.plan, namespace, sourcePath)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s:%08d", role, len(node.Sources))
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: role, SourceID: id})
	recipe.Sources = append(recipe.Sources, key)
	return key, nil
}

func appendReferencedPlanTrees(plan *ActionPlan, node *ActionPlanNode, recipe *ActionRecipe, values ...string) error {
	// Callers may supply source text before every recipe field has been folded,
	// but the final contract is the authoritative set. In particular recursive
	// Make replay argv can contain source/object-tree bindings even when the
	// wrapper command's own argv does not.
	values = append(values, recipe.Arguments...)
	values = append(values, recipe.WorkingDirectory)
	values = append(values, sortedStringMapValues(recipe.Environment)...)
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			values = append(values, invocation.Arguments...)
			values = append(values, invocation.Outputs...)
		}
	}
	seen := make(map[string]bool, len(node.Trees))
	for _, tree := range node.Trees {
		seen[tree] = true
	}
	bindTree := func(tree string) {
		if !seen[tree] {
			seen[tree] = true
			node.Trees = append(node.Trees, tree)
		}
		if !slices.Contains(recipe.Trees, tree) {
			recipe.Trees = append(recipe.Trees, tree)
		}
	}
	commandMetadataTrees, commandMetadataWorkingTrees, err := actionPlanCommandMetadataSourceTreeClosure(plan, node.Inputs)
	if err != nil {
		return err
	}
	// fixdep writes physical compiler source/dependency paths into Kbuild .cmd
	// files, and modpost's srcversion calculation opens those paths later. Keep
	// only the source namespaces behind a consumed .cmd producer. An ancestor
	// WorkingTree means that compiler paths may be relative to its private object
	// overlay, so replay that same namespace into the consumer's private root.
	for _, tree := range commandMetadataWorkingTrees {
		if !slices.Contains(recipe.WorkingTrees, tree) {
			recipe.WorkingTrees = append(recipe.WorkingTrees, tree)
		}
	}
	sort.Strings(recipe.WorkingTrees)
	recipe.WorkingTrees = slices.Compact(recipe.WorkingTrees)
	for _, tree := range commandMetadataTrees {
		bindTree(tree)
	}
	// WorkingTrees are input-only tree bindings whose complete contents are
	// staged below the recipe's private writable root. They need not appear in
	// argv or environment placeholders, but they are still part of the exact
	// sandbox/RBE closure of both the node and its interned recipe.
	for _, tree := range recipe.WorkingTrees {
		bindTree(tree)
	}
	// A translation unit can include a sibling by a relative quoted path even
	// when no -I flag spells the source root (for example mkcpustr.c includes a
	// source .c file). Exact source edges still bind argv placeholders, while
	// this tree edge closes the compiler's source-relative filesystem view for
	// sandboxed and remote execution. Namespace comes from the plan source, not
	// from the logical path: config/object projections may also use source edges.
	if len(node.Sources) != 0 {
		if err := plan.ensureSourceLookupIndex(); err != nil {
			return err
		}
		for _, edge := range node.Sources {
			source, ok := plan.sourcesByID[edge.SourceID]
			if !ok {
				return fmt.Errorf("node source edge references unknown source %q", edge.SourceID)
			}
			if source.Namespace == "kernel" {
				bindTree("kernel")
			}
		}
	}
	for _, value := range values {
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if match[1] != "tree" {
				continue
			}
			bindTree(match[2])
		}
	}
	return nil
}

// actionPlanCommandMetadataSourceTreeClosure returns source namespaces whose
// physical paths may be retained by a consumed generated Kbuild .cmd file.
// Only that output's producer ancestry participates. The source-edge and tree
// intersection identifies a selected source namespace without forwarding
// unrelated object/preparation trees. A namespace is also returned as writable
// only when the producing ancestry staged it as a WorkingTree; this preserves
// relative compiler paths without treating immutable source roots the same way.
func actionPlanCommandMetadataSourceTreeClosure(
	plan *ActionPlan,
	inputs []ActionPlanNodeEdge,
) ([]string, []string, error) {
	if plan == nil {
		return nil, nil, fmt.Errorf("Kbuild command metadata source-tree closure requires an action plan")
	}
	plan.ensureNodeLookupIndexes()
	roots := []string{}
	seenRoots := map[string]bool{}
	for _, input := range inputs {
		producer, ok := plan.nodesByID[input.ProducerID]
		if !ok {
			return nil, nil, fmt.Errorf("Kbuild command metadata input references absent producer %q", input.ProducerID)
		}
		if input.Slot < 0 || input.Slot >= len(producer.Outputs) {
			return nil, nil, fmt.Errorf("Kbuild command metadata input references absent producer %q slot %d", input.ProducerID, input.Slot)
		}
		output := producer.Outputs[input.Slot]
		logicalPath := output.Path
		if output.ObservedPath != "" {
			logicalPath = output.ObservedPath
		}
		if !strings.HasSuffix(canonicalKbuildRulePath(logicalPath), ".cmd") || seenRoots[input.ProducerID] {
			continue
		}
		seenRoots[input.ProducerID] = true
		roots = append(roots, input.ProducerID)
	}
	if len(roots) == 0 {
		return nil, nil, nil
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return nil, nil, err
	}
	retained := map[string]bool{}
	staged := map[string]bool{}
	visited := make([]bool, len(plan.Nodes))
	for _, root := range roots {
		err := plan.walkCompactKbuildWorkingTreeTopology(root, visited, func(nodeIndex uint32) error {
			producer := plan.Nodes[nodeIndex]
			declaredTrees := make(map[string]bool, len(producer.Trees))
			for _, tree := range producer.Trees {
				declaredTrees[tree] = true
			}
			if producer.Recipe != "" {
				recipe, ok := plan.Recipes[producer.Recipe]
				if !ok {
					return fmt.Errorf("Kbuild command metadata producer %q references unknown recipe %q", producer.ID, producer.Recipe)
				}
				for _, tree := range recipe.WorkingTrees {
					if declaredTrees[tree] {
						staged[tree] = true
					}
				}
			}
			for _, edge := range producer.Sources {
				source, ok := plan.sourcesByID[edge.SourceID]
				if !ok {
					return fmt.Errorf("Kbuild command metadata producer %q references unknown source %q", producer.ID, edge.SourceID)
				}
				if declaredTrees[source.Namespace] {
					retained[source.Namespace] = true
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	working := map[string]bool{}
	for tree := range retained {
		if staged[tree] {
			working[tree] = true
		}
	}
	return slices.Sorted(maps.Keys(retained)), slices.Sorted(maps.Keys(working)), nil
}

func (b *generatedPlanBuilder) internConfigProjectionSources() error {
	for _, spec := range resolvedConfigProjections() {
		if _, err := ensureActionPlanSource(b.plan, "config", spec.input); err != nil {
			return err
		}
	}
	return nil
}

// appendMissingConfigProjections publishes the final Kconfig-owned prep
// interface after selected Kbuild writers have been lowered. The immutable
// config sources are interned before lowering so a selected FORCE/filechk
// writer can read the initial state without racing an unconditional copy. If
// Kbuild does not select a writer, the copy remains the canonical final state.
func (b *generatedPlanBuilder) appendMissingConfigProjections() error {
	for _, spec := range resolvedConfigProjections() {
		if _, _, exists := planProducerByOutput(b.plan, "prep", spec.output); exists {
			continue
		}
		node := ActionPlanNode{
			Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: spec.output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
			Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
			Outputs:   []string{"00000000"},
		}
		if _, err := b.addSource(&node, &recipe, "input", "config", spec.input); err != nil {
			return err
		}
		if _, err := b.add(node, recipe); err != nil {
			return err
		}
	}
	return nil
}
