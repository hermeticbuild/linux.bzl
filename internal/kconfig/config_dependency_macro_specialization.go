package kconfig

import "slices"

// A specialization records entry cells that influence execution or branch
// joins, and every explicit final write (including writes equal to the captured
// input). Its wildcard is a pointwise transform on the current namespace, not
// a captured fallback. Unread, unwritten cells therefore remain caller-owned.
type configDependencyMacroSpecialization struct {
	requirements []configDependencyForcedHeaderMacroRequirement
	transform    configDependencyMacroSnapshotChangeSet
	ready        bool
}

func captureConfigDependencyMacroSpecialization(
	state *configDependencyMacroState,
	input, output *configDependencyMacroSnapshot,
) (configDependencyMacroSpecialization, bool) {
	if state == nil || input == nil || output == nil || !state.validSnapshotLineage() {
		return configDependencyMacroSpecialization{}, false
	}
	trace := state.forcedHeaderTrace
	if trace == nil || trace.entrySnapshot != input || trace.invalid || trace.wholeNamespace {
		return configDependencyMacroSpecialization{}, false
	}
	actual, valid := state.materializedSnapshot()
	if !valid || actual != output {
		return configDependencyMacroSpecialization{}, false
	}
	result := configDependencyMacroSpecialization{
		transform: configDependencyMacroSnapshotChangeSet{nonConfigWildcard: trace.nonConfigWildcard},
		ready:     true,
	}
	state.forcedHeaderTouches.forEach(func(name string) {
		if !valid {
			return
		}
		var cell configDependencyMacroSnapshotCell
		cell, valid = output.lookup(name)
		if valid {
			result.transform.put(name, cell)
		}
	})
	if !valid {
		return configDependencyMacroSpecialization{}, false
	}
	readNames := make([]string, 0, trace.reads.size())
	trace.reads.forEach(func(name string) { readNames = append(readNames, name) })
	slices.Sort(readNames)
	for _, name := range readNames {
		cell, found := input.lookup(name)
		if !found {
			return configDependencyMacroSpecialization{}, false
		}
		result.requirements = append(result.requirements, configDependencyForcedHeaderMacroRequirement{name: name, cell: cell})
	}
	return result, true
}

func (specialization configDependencyMacroSpecialization) matches(input *configDependencyMacroSnapshot) bool {
	if !specialization.ready || input == nil {
		return false
	}
	for _, requirement := range specialization.requirements {
		cell, valid := input.lookup(requirement.name)
		if !valid || cell != requirement.cell {
			return false
		}
	}
	return true
}

// apply validates the current entry before mutating a fresh direct child. The
// explicit transform cannot be installed as an already normalized overlay:
// an equal write on this entry must disappear from that overlay, while a write
// equal only on the captured entry must still execute. Existing child setters
// perform this normalization without materializing between writes. One final
// materialization is then adopted by commitBranch without repeating tree puts.
func (specialization configDependencyMacroSpecialization) apply(parent *configDependencyMacroState) bool {
	if !configDependencyMacroEffectStateReady(parent) {
		return false
	}
	input, valid := parent.materializedSnapshot()
	if !valid || !specialization.matches(input) {
		return false
	}
	child := parent.branch()
	if child == nil {
		return false
	}
	if specialization.transform.nonConfigWildcard {
		valid = child.applySnapshotValidatedNumericMacroHeader()
	}
	specialization.transform.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		if valid {
			valid = child.putSnapshotCell(name, cell)
		}
	})
	if !valid {
		return false
	}
	if _, valid := child.materializedSnapshot(); !valid {
		return false
	}
	return parent.commitBranch(child)
}
