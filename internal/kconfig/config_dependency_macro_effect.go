package kconfig

import "maps"

// configDependencyMacroEffect is a completed exact transform between immutable
// macro snapshots. Only captureConfigDependencyMacroEffect constructs nonzero
// effects: output is the proven materialization of changes applied to input.
// Mutable change maps are privately owned and copied again for each replay.
type configDependencyMacroEffect struct {
	input, output *configDependencyMacroSnapshot
	changes       configDependencyMacroSnapshotChangeSet
}

func cloneConfigDependencyMacroEffectChanges(changes configDependencyMacroSnapshotChangeSet) configDependencyMacroSnapshotChangeSet {
	changes.additional = maps.Clone(changes.additional)
	changes.unknownNames.additional = maps.Clone(changes.unknownNames.additional)
	return changes
}

func configDependencyMacroEffectStateReady(state *configDependencyMacroState) bool {
	if !state.validSnapshotLineage() {
		return false
	}
	for current := state; current != nil; current = current.parent {
		if current.macroReplacements != nil || current.forcedHeaderTrace != nil || current.forcedHeaderTouches.size() != 0 {
			return false
		}
	}
	return true
}

func captureConfigDependencyMacroEffect(branch *configDependencyMacroState) (configDependencyMacroEffect, bool) {
	if branch == nil || branch.parent == nil || !configDependencyMacroEffectStateReady(branch) {
		return configDependencyMacroEffect{}, false
	}
	input, valid := branch.parent.materializedSnapshot()
	if !valid || input != branch.snapshot || branch.snapshotForkRevision != branch.parent.snapshotRevision {
		return configDependencyMacroEffect{}, false
	}
	output, valid := branch.materializedSnapshot()
	if !valid {
		return configDependencyMacroEffect{}, false
	}
	return configDependencyMacroEffect{
		input: input, output: output,
		changes: cloneConfigDependencyMacroEffectChanges(branch.snapshotChanges),
	}, true
}

// apply is a cache probe: a mismatched or invalid parent is rejected without
// tainting it. The fresh child retains the current parent's ancestry, while its
// proven post-state lets commitBranch preserve the materialized tree directly.
// A hit still composes the finite overlay; it never substitutes a different
// fork snapshot into a live speculative parent.
func (effect configDependencyMacroEffect) apply(parent *configDependencyMacroState) bool {
	if effect.input == nil || effect.output == nil || !configDependencyMacroEffectStateReady(parent) {
		return false
	}
	input, valid := parent.materializedSnapshot()
	if !valid || input != effect.input {
		return false
	}
	branch := parent.branch()
	if branch == nil {
		return false
	}
	branch.snapshotChanges = cloneConfigDependencyMacroEffectChanges(effect.changes)
	branch.materializedSnapshotCache = effect.output
	return parent.commitBranch(branch)
}
