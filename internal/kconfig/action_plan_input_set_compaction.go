package kconfig

import (
	"fmt"
	"sort"
)

// compactKbuildInputFrontier keeps GNU Make's ordered, semantic prerequisites
// separate from the canonical materialized filesystem state.  The latter is a
// persistent radix root shared by every consumer and configuration which sees
// the same exact source/producer projection.  No flattened mirror is retained
// in an ActionPlan or in a planner cache.
type compactKbuildInputFrontier struct {
	direct   []compactKbuildRuleInput
	inputSet string
}

func (frontier compactKbuildInputFrontier) clone() compactKbuildInputFrontier {
	frontier.direct = cloneCompactKbuildRuleInputs(frontier.direct)
	return frontier
}

func compactKbuildInputMayEnterSet(input compactKbuildRuleInput) bool {
	return input.path != "" && input.workingOnly && !input.recipeLocal &&
		!input.orderOnly && !input.overwriteLineage &&
		(input.sourceID != "" || input.producer != "")
}

func compactKbuildInputSetEntry(input compactKbuildRuleInput) (ActionPlanInputSetEntry, error) {
	if !compactKbuildInputMayEnterSet(input) {
		return ActionPlanInputSetEntry{}, fmt.Errorf("Kbuild input %q is not persistent-frontier material", input.path)
	}
	entry := ActionPlanInputSetEntry{
		Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: input.path},
	}
	if input.producer != "" {
		entry.ProducerID = input.producer
		entry.Slot = input.slot
	} else {
		entry.SourceID = input.sourceID
	}
	return entry, nil
}

func compactKbuildRuleInputFromSetEntry(entry ActionPlanInputSetEntry) (compactKbuildRuleInput, error) {
	if entry.Target.Kind != ActionPlanInputSetWorkTarget || entry.Target.Tree != "" {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"Kbuild working frontier contains unsupported target %s",
			actionPlanInputSetTargetDescription(entry.Target),
		)
	}
	return compactKbuildRuleInput{
		path:        entry.Target.Path,
		producer:    entry.ProducerID,
		slot:        entry.Slot,
		sourceID:    entry.SourceID,
		workingOnly: true,
	}, nil
}

func compactKbuildInputFrontierFromResolved(
	plan *ActionPlan,
	inputs []compactKbuildRuleInput,
) (compactKbuildInputFrontier, error) {
	if plan == nil {
		return compactKbuildInputFrontier{}, fmt.Errorf("persistent Kbuild input frontier requires an action plan")
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	frontier := compactKbuildInputFrontier{
		direct: make([]compactKbuildRuleInput, 0, len(inputs)),
	}
	for _, input := range inputs {
		if !compactKbuildInputMayEnterSet(input) {
			frontier.direct = append(frontier.direct, input)
			continue
		}
		entry, err := compactKbuildInputSetEntry(input)
		if err != nil {
			return compactKbuildInputFrontier{}, err
		}
		if previous, found, err := store.Lookup(frontier.inputSet, entry.Target); err != nil {
			return compactKbuildInputFrontier{}, err
		} else if found && previous != entry {
			return compactKbuildInputFrontier{}, fmt.Errorf(
				"Kbuild input frontier target %s has conflicting provenance",
				actionPlanInputSetTargetDescription(entry.Target),
			)
		}
		frontier.inputSet, err = store.Insert(frontier.inputSet, entry)
		if err != nil {
			return compactKbuildInputFrontier{}, err
		}
	}
	return frontier, nil
}

// compactKbuildInputFrontierOverlayResolved rebuilds only the monotonic delta
// above an already-persistent frontier. Callers may temporarily materialize a
// root for command analysis and then discover a small number of extra inputs;
// this helper prevents that analysis view from being reinserted path by path.
func compactKbuildInputFrontierOverlayResolved(
	plan *ActionPlan,
	base compactKbuildInputFrontier,
	inputs []compactKbuildRuleInput,
) (compactKbuildInputFrontier, error) {
	if base.inputSet == "" {
		return compactKbuildInputFrontierFromResolved(plan, inputs)
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	delta := make([]compactKbuildRuleInput, 0, len(inputs))
	for _, input := range inputs {
		if !compactKbuildInputMayEnterSet(input) {
			delta = append(delta, input)
			continue
		}
		entry, err := compactKbuildInputSetEntry(input)
		if err != nil {
			return compactKbuildInputFrontier{}, err
		}
		existing, found, err := store.Lookup(base.inputSet, entry.Target)
		if err != nil {
			return compactKbuildInputFrontier{}, err
		}
		if !found {
			delta = append(delta, input)
			continue
		}
		if existing.SourceID != entry.SourceID || existing.ProducerID != entry.ProducerID || existing.Slot != entry.Slot {
			return compactKbuildInputFrontier{}, fmt.Errorf(
				"resolved Kbuild input %q conflicts with persistent base provenance",
				input.path,
			)
		}
	}
	frontier, err := compactKbuildInputFrontierFromResolved(plan, delta)
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	frontier.inputSet, err = store.Union(base.inputSet, frontier.inputSet, func(
		target ActionPlanInputSetTarget,
		left, right ActionPlanInputSetEntry,
	) (ActionPlanInputSetEntry, error) {
		if left != right {
			return ActionPlanInputSetEntry{}, fmt.Errorf(
				"persistent Kbuild input target %s has conflicting overlay provenance",
				actionPlanInputSetTargetDescription(target),
			)
		}
		return left, nil
	})
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	return frontier, nil
}

// compactKbuildInputFrontierInputs is a bounded planner view, not retained
// state. It exists only for path-sensitive analysis which has not yet been
// converted to radix Lookup/Walk. Execution and caches retain the root alone.
func compactKbuildInputFrontierInputs(
	plan *ActionPlan,
	frontier compactKbuildInputFrontier,
) ([]compactKbuildRuleInput, error) {
	inputs := cloneCompactKbuildRuleInputs(frontier.direct)
	if frontier.inputSet == "" {
		return inputs, nil
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return nil, err
	}
	materialized := []compactKbuildRuleInput{}
	if err := store.Walk(frontier.inputSet, func(entry ActionPlanInputSetEntry) error {
		input, err := compactKbuildRuleInputFromSetEntry(entry)
		if err != nil {
			return err
		}
		materialized = append(materialized, input)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(materialized, func(i, j int) bool { return materialized[i].path < materialized[j].path })
	return append(inputs, materialized...), nil
}

func compactKbuildMapInputFrontierUses(
	plan *ActionPlan,
	frontier compactKbuildInputFrontier,
	compilerPaths, auxiliaryPaths map[string]bool,
) (compactKbuildInputFrontier, error) {
	if frontier.inputSet == "" {
		return frontier, nil
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	if plan.inputUseProjection == nil {
		plan.inputUseProjection = &compactKbuildInputUseProjection{}
	}
	return mapCompactKbuildPersistentInputUses(store, plan.inputUseProjection, frontier, compilerPaths, auxiliaryPaths)
}

type compactKbuildInputUseProjection struct {
	store   *ActionPlanInputSetStore
	clear   *ActionPlanInputSetTargetStableMapper
	indexed map[string]bool
	// Historical aliases are bounded by unique tree target keys, not by the
	// sum of root sizes. Looking up a path probes its historical namespaces;
	// it need not be constant time if that path has appeared in many trees.
	treeTargets map[string]map[ActionPlanInputSetTarget]bool
}

func (memo *compactKbuildInputUseProjection) ensureStore(store *ActionPlanInputSetStore) error {
	if memo.store == store && memo.clear != nil {
		return nil
	}
	clear, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		entry.CompilerUse, entry.AuxiliaryUse = false, false
		return entry, nil
	})
	if err != nil {
		return err
	}
	memo.store, memo.clear = store, clear
	memo.indexed = map[string]bool{}
	memo.treeTargets = map[string]map[ActionPlanInputSetTarget]bool{}
	return nil
}

func (memo *compactKbuildInputUseProjection) indexTreeTargets(store *ActionPlanInputSetStore, root string) error {
	if root == "" || memo.indexed[root] {
		return nil
	}
	node, err := store.nodeAt(root)
	if err != nil {
		return err
	}
	for _, entry := range node.Entries {
		if entry.Target.Kind != ActionPlanInputSetTreeTarget {
			continue
		}
		if memo.treeTargets[entry.Target.Path] == nil {
			memo.treeTargets[entry.Target.Path] = map[ActionPlanInputSetTarget]bool{}
		}
		memo.treeTargets[entry.Target.Path][entry.Target] = true
	}
	for _, child := range node.Children {
		if err := memo.indexTreeTargets(store, child.ID); err != nil {
			return err
		}
	}
	memo.indexed[root] = true
	return nil
}

// mapCompactKbuildPersistentInputUses clears inherited consumer flags through
// one plan-scoped pure mapper, then updates only positively selected targets.
// Clearing visits each immutable subtrie once across cumulative roots. Only
// tree target keys need an alias index; work and ambient use exact lookups.
func mapCompactKbuildPersistentInputUses(
	store *ActionPlanInputSetStore,
	memo *compactKbuildInputUseProjection,
	frontier compactKbuildInputFrontier,
	compilerPaths, auxiliaryPaths map[string]bool,
) (compactKbuildInputFrontier, error) {
	if frontier.inputSet == "" {
		return frontier, nil
	}
	if err := memo.ensureStore(store); err != nil {
		return compactKbuildInputFrontier{}, err
	}
	inputRoot := frontier.inputSet
	// Match the existing wrapper's failure contract: mapper errors blank the
	// persistent root but keep ordered direct inputs available to the caller.
	frontier.inputSet = ""
	root, err := memo.clear.Map(inputRoot)
	if err != nil {
		return frontier, err
	}
	paths := map[string]bool{}
	for pathname, used := range compilerPaths {
		if used {
			paths[pathname] = true
		}
	}
	for pathname, used := range auxiliaryPaths {
		if used {
			paths[pathname] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for pathname := range paths {
		ordered = append(ordered, pathname)
	}
	// Diagnostic ordering is deliberately lexical instead of radix traversal
	// order. The error class and returned frontier remain unchanged.
	sort.Strings(ordered)
	if len(ordered) != 0 {
		if err := memo.indexTreeTargets(store, inputRoot); err != nil {
			return frontier, err
		}
	}
	for _, pathname := range ordered {
		// An invalid map key cannot match any validated target entry.
		if validatePlanRelativePath("working input use", pathname) != nil {
			continue
		}
		targets := []ActionPlanInputSetTarget{
			{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
			{Kind: ActionPlanInputSetAmbientTarget, Path: pathname},
		}
		for target := range memo.treeTargets[pathname] {
			targets = append(targets, target)
		}
		sort.Slice(targets, func(i, j int) bool {
			return actionPlanInputSetTargetKey(targets[i]) < actionPlanInputSetTargetKey(targets[j])
		})
		for _, target := range targets {
			entry, found, err := store.Lookup(root, target)
			if err != nil {
				return frontier, err
			}
			if !found {
				continue
			}
			entry.CompilerUse = compilerPaths[pathname]
			entry.AuxiliaryUse = auxiliaryPaths[pathname]
			if entry.AuxiliaryUse && !entry.CompilerUse {
				return frontier, fmt.Errorf("Kbuild auxiliary input-set target %q is absent from complete compiler uses", pathname)
			}
			root, err = store.Insert(root, entry)
			if err != nil {
				return frontier, err
			}
		}
	}
	frontier.inputSet = root
	return frontier, nil
}
