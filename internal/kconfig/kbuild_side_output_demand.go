package kconfig

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// compactKbuildSideOutputDemand records one otherwise-unruled prerequisite and
// every materialized source-recipe action which completed before its consumer.
// Source ordering proves availability, not ownership: execution observes which
// candidate actually changed the opaque path and a resolver action publishes
// that exact result. No command, target, extension, or module naming convention
// participates in candidate discovery.
type compactKbuildSideOutputDemand struct {
	candidates []compactKbuildSelectionKey
	consumer   compactKbuildSelectionKey
	output     ActionPlanOutput
	stage      string
	product    string
}

// compactKbuildSideOutputDemands finds demanded files which are neither source
// inputs nor independently selected/rule-produced artifacts. Such a path is a
// side output only when the consumer invocation records one or more completed
// source-recipe actions. Candidate ambiguity is preserved for
// execution-time observation; it must never be resolved from traversal order.
func (g *compactKbuildSelectionGraph) compactKbuildSideOutputDemands(
	metadata *CompactMetadata,
	config CompactConfig,
) ([]compactKbuildSideOutputDemand, error) {
	if g == nil || metadata == nil {
		return nil, fmt.Errorf("cannot derive Kbuild side outputs without selections and metadata")
	}

	byOutput := map[struct {
		consumer compactKbuildSelectionKey
		tree     string
		path     string
	}]compactKbuildSideOutputDemand{}
	for _, consumer := range g.ordered {
		_, ok := g.profile(consumer.profile)
		if !ok {
			return nil, fmt.Errorf("side-output consumer references missing profile %q", consumer.profile)
		}
		paths, err := g.compactKbuildUnruledPrerequisites(metadata, consumer)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			continue
		}

		// Candidate discovery follows the consumer's complete selected dependency
		// closure. That includes peer recipes in this Make invocation, completed
		// predecessor invocations, and target-local recursive invocations. Runtime
		// byte observation decides ownership; traversal position never does.
		candidates, err := g.compactKbuildSelectionPredecessorRecipeSelections(metadata, consumer)
		if err != nil {
			return nil, err
		}
		uniqueCandidates := make([]compactKbuildSelectionKey, 0, len(candidates))
		seenCandidates := map[compactKbuildSelectionKey]bool{}
		for _, candidate := range candidates {
			candidateProfile, exists := g.profile(candidate.profile)
			if !exists {
				return nil, fmt.Errorf("side-output candidate references missing profile %q", candidate.profile)
			}
			if g.compactKbuildProfileTargetIsPhony(candidateProfile, candidate.target) {
				return nil, fmt.Errorf(
					"selected Kbuild consumer %s may receive an opaque side output from phony recipe candidate %s; the phony action has no materialized artifact to observe",
					compactKbuildSelectionKeyString(consumer), compactKbuildSelectionKeyString(candidate),
				)
			}
			if g.forwardingSelections[candidate] {
				owner, owned := g.owner(candidate.target)
				if !owned {
					return nil, fmt.Errorf(
						"selected Kbuild consumer %s side-output forwarding candidate %s has no unique materialized terminal owner",
						compactKbuildSelectionKeyString(consumer), compactKbuildSelectionKeyString(candidate),
					)
				}
				candidate = owner
			}
			candidate = g.compactKbuildGroupedSelectionRepresentative(candidate)
			if !seenCandidates[candidate] {
				seenCandidates[candidate] = true
				uniqueCandidates = append(uniqueCandidates, candidate)
			}
		}
		sort.Slice(uniqueCandidates, func(i, j int) bool {
			return compactKbuildSelectionKeyLess(uniqueCandidates[i], uniqueCandidates[j])
		})
		for _, candidate := range uniqueCandidates {
			if compactKbuildSelectionStageOrder(candidate.stage) > compactKbuildSelectionStageOrder(consumer.stage) {
				return nil, fmt.Errorf(
					"selected Kbuild consumer %s demands unruled prerequisites %q from later-stage invocation terminal candidate %s; cannot observe a backward side output",
					compactKbuildSelectionKeyString(consumer), paths, compactKbuildSelectionKeyString(candidate),
				)
			}
		}
		if len(uniqueCandidates) == 0 {
			return nil, fmt.Errorf(
				"selected Kbuild consumer %s demands unruled prerequisites %q without a source-recipe predecessor action to observe",
				compactKbuildSelectionKeyString(consumer), paths,
			)
		}

		selection := g.selections[consumer]
		// The resolver is a prerequisite of this exact consumer and therefore
		// belongs to the consumer's physical stage. In particular a bootstrap
		// consumer must consume a bootstrap resolver; projecting the result into
		// its later lifecycle tree would manufacture a backward edge.
		context := compactKbuildSelectionPlanContext(config, selection)
		for _, sideOutput := range paths {
			demand := compactKbuildSideOutputDemand{
				candidates: append([]compactKbuildSelectionKey(nil), uniqueCandidates...),
				consumer:   consumer,
				stage:      context.Stage,
				product:    context.Product,
				output: ActionPlanOutput{
					Tree: context.OutputTree,
					Path: sideOutput,
				},
			}
			key := struct {
				consumer compactKbuildSelectionKey
				tree     string
				path     string
			}{consumer: demand.consumer, tree: demand.output.Tree, path: demand.output.Path}
			if previous, exists := byOutput[key]; exists && (previous.stage != demand.stage || previous.product != demand.product) {
				return nil, fmt.Errorf(
					"demanded Kbuild side output %q consumer %s has conflicting contexts %s/%s/%s and %s/%s/%s",
					demand.output.Path, compactKbuildSelectionKeyString(demand.consumer),
					previous.stage, previous.output.Tree, previous.product,
					demand.stage, demand.output.Tree, demand.product,
				)
			}
			byOutput[key] = demand
		}
	}

	demands := make([]compactKbuildSideOutputDemand, 0, len(byOutput))
	for _, demand := range byOutput {
		demands = append(demands, demand)
	}
	sort.Slice(demands, func(i, j int) bool {
		if demands[i].consumer != demands[j].consumer {
			return compactKbuildSelectionKeyLess(demands[i].consumer, demands[j].consumer)
		}
		if demands[i].output.Tree != demands[j].output.Tree {
			return demands[i].output.Tree < demands[j].output.Tree
		}
		return demands[i].output.Path < demands[j].output.Path
	})
	return demands, nil
}

// compactKbuildRegisterSideOutputCandidateDependencies installs only the
// execution-order edges implied by runtime observation. It deliberately does
// not mutate output-owner indexes: the actionfile resolver, not source order,
// decides which candidate produced the demanded bytes.
func (g *compactKbuildSelectionGraph) compactKbuildRegisterSideOutputCandidateDependencies(
	demands []compactKbuildSideOutputDemand,
) error {
	if g == nil {
		return fmt.Errorf("cannot register Kbuild side-output candidates on a nil selection graph")
	}
	g.invalidateResolvedDependencies()
	g.sideOutputCandidateDependencies = make(map[compactKbuildSelectionKey][]compactKbuildSelectionKey)
	seen := map[struct {
		consumer  compactKbuildSelectionKey
		candidate compactKbuildSelectionKey
	}]bool{}
	for _, demand := range demands {
		if _, exists := g.selections[demand.consumer]; !exists {
			return fmt.Errorf("Kbuild side-output demand references missing consumer %s", compactKbuildSelectionKeyString(demand.consumer))
		}
		consumers := g.compactKbuildGroupedSelectionMembers(demand.consumer)
		for _, candidate := range demand.candidates {
			if _, exists := g.selections[candidate]; !exists {
				return fmt.Errorf("Kbuild side-output demand references missing candidate %s", compactKbuildSelectionKeyString(candidate))
			}
			if g.forwardingSelections[candidate] {
				return fmt.Errorf("Kbuild side-output demand retains non-materialized forwarding candidate %s", compactKbuildSelectionKeyString(candidate))
			}
			profile := g.profiles[candidate.profile]
			if g.compactKbuildProfileTargetIsPhony(profile, candidate.target) {
				return fmt.Errorf("Kbuild side-output demand retains non-materialized phony candidate %s", compactKbuildSelectionKeyString(candidate))
			}
			for _, consumer := range consumers {
				if candidate == consumer {
					return fmt.Errorf("Kbuild side-output demand for %q makes %s observe itself", demand.output.Path, compactKbuildSelectionKeyString(consumer))
				}
				identity := struct {
					consumer  compactKbuildSelectionKey
					candidate compactKbuildSelectionKey
				}{consumer: consumer, candidate: candidate}
				if seen[identity] {
					continue
				}
				seen[identity] = true
				g.sideOutputCandidateDependencies[consumer] = append(g.sideOutputCandidateDependencies[consumer], candidate)
			}
		}
	}
	for consumer := range g.sideOutputCandidateDependencies {
		sort.Slice(g.sideOutputCandidateDependencies[consumer], func(i, j int) bool {
			return compactKbuildSelectionKeyLess(
				g.sideOutputCandidateDependencies[consumer][i],
				g.sideOutputCandidateDependencies[consumer][j],
			)
		})
	}
	return nil
}

func (g *compactKbuildSelectionGraph) compactKbuildUnruledPrerequisites(
	metadata *CompactMetadata,
	consumer compactKbuildSelectionKey,
) ([]string, error) {
	cacheKey := compactKbuildSelectionMetadataKey{metadata: metadata, selection: consumer}
	if cached, ok := g.unruledPrerequisites[cacheKey]; ok {
		return append([]string(nil), cached.paths...), cached.err
	}
	g.cacheMisses.unruledPrerequisite++
	paths, err := g.computeCompactKbuildUnruledPrerequisites(metadata, consumer)
	g.unruledPrerequisites[cacheKey] = compactKbuildPrerequisitePaths{
		paths: append([]string(nil), paths...),
		err:   err,
	}
	return paths, err
}

// compactKbuildOrderingOnlyRuleContextForMakeTarget classifies a source rule
// which has no artifact-producing recipe and returns the prerequisite context
// selected through the same lexical target spelling. The canonical graph path
// alone cannot recover Linux declarations such as $(obj)/%.o reached through
// $(obj)/../../../virt/... parent traversal.
func (g *compactKbuildSelectionGraph) compactKbuildOrderingOnlyRuleContextForMakeTarget(
	metadata *CompactMetadata,
	profile CompactKbuildProfile,
	target, makeTarget string,
) (bool, []compactKbuildEvaluatedPath, []compactKbuildEvaluatedPath, error) {
	if _, matched, err := g.compactKbuildRuleForProfileMakeTarget(metadata, profile, target, makeTarget); err != nil {
		return false, nil, nil, err
	} else if matched {
		return false, nil, nil, nil
	}
	candidates := compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget)
	if len(candidates) == 0 {
		return false, nil, nil, nil
	}
	selected := candidates[0]
	selectedSpecificity := len(strings.ReplaceAll(selected.target, "%", ""))
	for _, candidate := range candidates {
		match := compactKbuildRuleMatch{
			profile: profile, rule: candidate.rule, stem: candidate.stem, lookupTarget: candidate.lookupTarget,
			explicit: !strings.Contains(candidate.target, "%"), stemLength: len(candidate.stem),
			ruleOrder: candidate.ruleOrder, targetOrder: candidate.targetOrder, resolved: true,
		}
		if len(kbuildRecipeCommandExpressions(match.rule.Recipe)) == 0 {
			action, _, err := evaluatedKbuildDirectRecipeEffects(target, match)
			if err != nil {
				return false, nil, nil, err
			}
			if action {
				return false, nil, nil, nil
			}
		}
		specificity := len(strings.ReplaceAll(candidate.target, "%", ""))
		if specificity > selectedSpecificity {
			selected = candidate
			selectedSpecificity = specificity
		}
	}
	selectedMatch := compactKbuildRuleMatch{
		profile: profile, rule: selected.rule, stem: selected.stem, lookupTarget: selected.lookupTarget,
		explicit: !strings.Contains(selected.target, "%"), stemLength: len(selected.stem),
		ruleOrder: selected.ruleOrder, targetOrder: selected.targetOrder, resolved: true,
	}
	normal, orderOnly, _, err := g.compactKbuildTargetRuleContext(profile, target, selectedMatch)
	if err != nil {
		return false, nil, nil, err
	}
	return true, normal, orderOnly, nil
}

func (g *compactKbuildSelectionGraph) computeCompactKbuildUnruledPrerequisites(
	metadata *CompactMetadata,
	consumer compactKbuildSelectionKey,
) ([]string, error) {
	selection, ok := g.selection(consumer)
	if !ok {
		return nil, fmt.Errorf("missing side-output consumer selection %s", compactKbuildSelectionKeyString(consumer))
	}
	profile, ok := g.profile(selection.Profile)
	if !ok {
		return nil, fmt.Errorf("side-output consumer references missing profile %q", selection.Profile)
	}
	if g.compactKbuildProfileTargetIsPhony(profile, selection.Target) {
		return nil, nil
	}
	match, matched, err := g.compactKbuildRuleForProfileMakeTarget(
		metadata, profile, selection.Target, selection.MakeTarget,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve side-output consumer %s: %w", compactKbuildSelectionKeyString(consumer), err)
	}
	if !matched {
		return nil, nil
	}
	matchViable := match.explicit
	if !matchViable {
		matchViable, err = metadata.compactKbuildRuleMatchViableInProfile(selection.Target, match, profile)
		if err != nil {
			return nil, err
		}
	}
	if !matchViable && !g.compactKbuildProfileGenerates(profile, selection.Target) {
		return nil, nil
	}

	resolvedConfig := map[string]bool{}
	for _, output := range ResolvedConfigProjectionOutputs() {
		resolvedConfig[canonicalKbuildRulePath(output)] = true
	}
	demands := map[string]bool{}
	expanded := map[string]bool{}
	var collect func(compactKbuildEvaluatedPath) error
	collect = func(evaluated compactKbuildEvaluatedPath) error {
		target := compactKbuildGraphTargetPath(evaluated.graphPath)
		if target == "" || target == "FORCE" || target == selection.Target || resolvedConfig[target] {
			return nil
		}
		source, err := g.compactKbuildSourcePathExists(metadata, profile, target)
		if err != nil {
			return err
		}
		if _, selected := g.owner(target); selected || source {
			return nil
		}
		if g.compactKbuildProfileGenerates(profile, target) || expanded[target] {
			return nil
		}
		expanded[target] = true

		dependency, found, err := g.compactKbuildRuleForProfileMakeTarget(
			metadata, profile, target, evaluated.makeWord,
		)
		if err != nil {
			return err
		}
		if !found {
			orderingOnly, normal, orderOnly, orderingErr := g.compactKbuildOrderingOnlyRuleContextForMakeTarget(
				metadata, profile, target, evaluated.makeWord,
			)
			if orderingErr != nil {
				return orderingErr
			}
			if orderingOnly {
				// A prerequisite-only, diagnostic, or mkdir-only Make target is an
				// ordering boundary, not an opaque file emitted by the preceding
				// recursive invocation. Rule lookup intentionally omits these
				// non-actions; preserve that distinction while following the exact
				// merged Make prerequisite closure behind the boundary.
				for _, prerequisite := range normal {
					if err := collect(prerequisite); err != nil {
						return err
					}
				}
				for _, prerequisite := range orderOnly {
					if err := collect(prerequisite); err != nil {
						return err
					}
				}
				return nil
			}
			demands[target] = true
			return nil
		}
		// GNU Make rejects an implicit-rule candidate whose prerequisite search
		// would have to reuse the same implicit rule. Rule resolution retains a
		// non-viable candidate as a diagnostic fallback when no viable candidate
		// exists, but side-output discovery must treat that target as the missing
		// leaf. Descending through generic rules such as `%: %_shipped` would
		// otherwise manufacture an unbounded chain of distinct target names.
		dependencyViable := dependency.explicit
		if !dependencyViable {
			dependencyViable, err = metadata.compactKbuildRuleMatchViableInProfile(target, dependency, profile)
			if err != nil {
				return err
			}
		}
		if !dependencyViable {
			demands[target] = true
			return nil
		}
		normal, orderOnly, _, contextErr := g.compactKbuildTargetRuleContext(profile, target, dependency)
		if contextErr != nil {
			return contextErr
		}
		for _, prerequisite := range normal {
			if compactKbuildRuleCommandSequenceContains(dependency, path.Base(prerequisite.graphPath)) {
				continue
			}
			if err := collect(prerequisite); err != nil {
				return err
			}
		}
		for _, prerequisite := range orderOnly {
			if compactKbuildRuleCommandSequenceContains(dependency, path.Base(prerequisite.graphPath)) {
				continue
			}
			if err := collect(prerequisite); err != nil {
				return err
			}
		}
		return nil
	}
	normal, orderOnly, _, contextErr := g.compactKbuildTargetRuleContext(profile, selection.Target, match)
	if contextErr != nil {
		return nil, contextErr
	}
	for _, prerequisite := range normal {
		if compactKbuildRuleCommandSequenceContains(match, path.Base(prerequisite.graphPath)) {
			continue
		}
		if err := collect(prerequisite); err != nil {
			return nil, fmt.Errorf("resolve unruled prerequisites for %s: %w", compactKbuildSelectionKeyString(consumer), err)
		}
	}
	for _, prerequisite := range orderOnly {
		if compactKbuildRuleCommandSequenceContains(match, path.Base(prerequisite.graphPath)) {
			continue
		}
		if err := collect(prerequisite); err != nil {
			return nil, fmt.Errorf("resolve unruled prerequisites for %s: %w", compactKbuildSelectionKeyString(consumer), err)
		}
	}
	paths := make([]string, 0, len(demands))
	for target := range demands {
		paths = append(paths, target)
	}
	sort.Strings(paths)
	return paths, nil
}

// compactKbuildTerminalRecipeSelections identify every terminal materialized
// source recipe in one predecessor invocation. A recursive Make invocation is
// complete only after all independent terminal recipes complete.
func (g *compactKbuildSelectionGraph) compactKbuildTerminalRecipeSelections(
	metadata *CompactMetadata,
	profileName, consumerStage string,
) ([]compactKbuildSelectionKey, error) {
	cacheKey := compactKbuildTerminalSelectionKey{
		metadata: metadata,
		profile:  profileName,
		stage:    compactKbuildSelectionStageOrder(consumerStage),
	}
	if cached, ok := g.terminalSelections[cacheKey]; ok {
		return append([]compactKbuildSelectionKey(nil), cached.dependencies...), cached.err
	}
	g.cacheMisses.terminalSelection++
	selections, err := g.computeCompactKbuildTerminalRecipeSelections(metadata, profileName, consumerStage)
	g.terminalSelections[cacheKey] = compactKbuildSelectionDependencies{
		dependencies: append([]compactKbuildSelectionKey(nil), selections...), err: err,
	}
	return selections, err
}

// compactKbuildInvocationPredecessorRecipeSelections expands each completed
// predecessor invocation terminal through its exact selected dependency
// closure. An opaque side effect may be produced by any materialized recipe in
// that closure; observing only the terminal recreates the old traversal-order
// guess and loses intermediate generator outputs in action-private work trees.
func (g *compactKbuildSelectionGraph) compactKbuildInvocationPredecessorRecipeSelections(
	metadata *CompactMetadata,
	profileName, consumerStage string,
) ([]compactKbuildSelectionKey, error) {
	cacheKey := compactKbuildInvocationPredecessorKey{
		metadata: metadata,
		profile:  profileName,
		stage:    compactKbuildSelectionStageOrder(consumerStage),
	}
	if cached, ok := g.invocationRecipeSelections[cacheKey]; ok {
		return append([]compactKbuildSelectionKey(nil), cached.dependencies...), cached.err
	}
	roots, err := g.compactKbuildInvocationPredecessorSelections(metadata, profileName, consumerStage)
	if err != nil {
		g.invocationRecipeSelections[cacheKey] = compactKbuildSelectionDependencies{err: err}
		return nil, err
	}
	selections, err := g.compactKbuildRecipeSelectionsFromRoots(metadata, roots)
	g.invocationRecipeSelections[cacheKey] = compactKbuildSelectionDependencies{
		dependencies: append([]compactKbuildSelectionKey(nil), selections...), err: err,
	}
	return selections, err
}

// compactKbuildSelectionPredecessorRecipeSelections expands the complete
// source-derived dependency closure which executes before one exact consumer.
// Unlike invocation-only discovery this also includes earlier recipes from the
// same invocation and target-local recursive invocations.
func (g *compactKbuildSelectionGraph) compactKbuildSelectionPredecessorRecipeSelections(
	metadata *CompactMetadata,
	consumer compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	roots, err := g.selectionDependencies(metadata, consumer)
	if err != nil {
		return nil, err
	}
	// Stage-bounded materialization dependencies deliberately omit later work.
	// Side-output ownership must still see that work and reject it instead of
	// misclassifying an earlier intermediate as the completed invocation owner.
	selected := make(map[compactKbuildSelectionKey]bool, len(roots))
	canonicalRoots := make([]compactKbuildSelectionKey, 0, len(roots))
	appendRoot := func(root compactKbuildSelectionKey) {
		root = g.compactKbuildGroupedSelectionRepresentative(root)
		if g.compactKbuildSelectionsShareProducer(root, consumer) || selected[root] {
			return
		}
		selected[root] = true
		canonicalRoots = append(canonicalRoots, root)
	}
	for _, root := range roots {
		appendRoot(root)
	}
	for _, member := range g.compactKbuildGroupedSelectionMembers(consumer) {
		profile, ok := g.profile(member.profile)
		if !ok {
			return nil, fmt.Errorf("side-output consumer references missing profile %q", member.profile)
		}
		predecessors, predecessorErr := g.compactKbuildInvocationPredecessorSelections(metadata, profile.Name, "target")
		if predecessorErr != nil {
			return nil, predecessorErr
		}
		for _, root := range predecessors {
			appendRoot(root)
		}
		invocationKey := compactKbuildProfileTargetKey{profile: profile.Name, target: member.target}
		for _, dependencyProfile := range g.targetInvocations[invocationKey] {
			terminals, terminalErr := g.compactKbuildTerminalRecipeSelections(metadata, dependencyProfile, "target")
			if terminalErr != nil {
				return nil, terminalErr
			}
			for _, root := range terminals {
				appendRoot(root)
			}
		}
	}
	return g.compactKbuildRecipeSelectionsFromRoots(metadata, canonicalRoots)
}

func (g *compactKbuildSelectionGraph) compactKbuildRecipeSelectionsFromRoots(
	metadata *CompactMetadata,
	roots []compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := map[compactKbuildSelectionKey]int{}
	selected := map[compactKbuildSelectionKey]bool{}
	stack := []compactKbuildSelectionKey{}
	var visit func(compactKbuildSelectionKey) error
	visit = func(key compactKbuildSelectionKey) error {
		switch state[key] {
		case visited:
			return nil
		case visiting:
			cycle := []string{}
			start := 0
			for index, candidate := range stack {
				if candidate == key {
					start = index
					break
				}
			}
			for _, candidate := range append(stack[start:], key) {
				cycle = append(cycle, compactKbuildSelectionKeyString(candidate))
			}
			return fmt.Errorf("Kbuild side-output candidate dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		state[key] = visiting
		stack = append(stack, key)
		dependencies, dependencyErr := g.selectionDependencies(metadata, key)
		if dependencyErr != nil {
			return dependencyErr
		}
		for _, dependency := range dependencies {
			if dependencyErr := visit(dependency); dependencyErr != nil {
				return dependencyErr
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = visited

		// A forwarding selection is a recursive-Make boundary. Its materialized
		// descendants were traversed above; the forwarding node itself is skipped
		// by action lowering and therefore cannot own an observation capture.
		if g.forwardingSelections[key] {
			return nil
		}
		profile, ok := g.profile(key.profile)
		if !ok {
			return fmt.Errorf("side-output candidate references missing profile %q", key.profile)
		}
		selection, ok := g.selection(key)
		if !ok {
			return fmt.Errorf("side-output candidate references missing selection %s", compactKbuildSelectionKeyString(key))
		}
		match, matched, matchErr := g.compactKbuildRuleForProfileMakeTarget(
			metadata, profile, key.target, selection.MakeTarget,
		)
		if matchErr != nil {
			return matchErr
		}
		if !matched || len(match.commandSequence()) == 0 {
			return nil
		}
		if g.compactKbuildProfileTargetIsPhony(profile, key.target) {
			// A phony selection is a recursive-Make/control boundary. Action
			// lowering does not materialize it, so only the exact dependency closure
			// traversed above can carry observable filesystem effects.
			return nil
		}
		selected[g.compactKbuildGroupedSelectionRepresentative(key)] = true
		return nil
	}
	for _, root := range roots {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	selections := make([]compactKbuildSelectionKey, 0, len(selected))
	for candidate := range selected {
		selections = append(selections, candidate)
	}
	sort.Slice(selections, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(selections[i], selections[j])
	})
	return selections, nil
}

func (g *compactKbuildSelectionGraph) computeCompactKbuildTerminalRecipeSelections(
	metadata *CompactMetadata,
	profileName, consumerStage string,
) ([]compactKbuildSelectionKey, error) {
	candidates := []compactKbuildSelectionKey{}
	maximumStage := compactKbuildSelectionStageOrder(consumerStage)
	for stage := 0; stage <= maximumStage; stage++ {
		indexed := g.selectionsByProfileStage[compactKbuildProfileStageKey{profile: profileName, stage: stage}]
		for _, key := range indexed {
			profile := g.profiles[key.profile]
			selection, ok := g.selection(key)
			if !ok {
				return nil, fmt.Errorf("terminal side-output candidate references missing selection %s", compactKbuildSelectionKeyString(key))
			}
			match, matched, err := g.compactKbuildRuleForProfileMakeTarget(
				metadata, profile, key.target, selection.MakeTarget,
			)
			if err != nil {
				return nil, err
			}
			if matched && len(match.commandSequence()) != 0 {
				candidates = append(candidates, key)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	consumed := map[compactKbuildSelectionKey]bool{}
	visitedGroups := map[compactKbuildSelectionKey]bool{}
	for _, candidate := range candidates {
		candidateGroup := g.compactKbuildGroupedSelectionRepresentative(candidate)
		if visitedGroups[candidateGroup] {
			continue
		}
		visitedGroups[candidateGroup] = true
		dependencies, err := g.groupedSelectionNativeDependencies(metadata, candidateGroup)
		if err != nil {
			return nil, err
		}
		for _, dependency := range dependencies {
			dependencyGroup := g.compactKbuildGroupedSelectionRepresentative(dependency)
			if dependency.profile == profileName && dependencyGroup != candidateGroup {
				consumed[dependencyGroup] = true
			}
		}
	}
	terminals := []compactKbuildSelectionKey{}
	seen := map[compactKbuildSelectionKey]bool{}
	for _, candidate := range candidates {
		candidate = g.compactKbuildGroupedSelectionRepresentative(candidate)
		if !consumed[candidate] && !seen[candidate] {
			seen[candidate] = true
			terminals = append(terminals, candidate)
		}
	}
	return terminals, nil
}
