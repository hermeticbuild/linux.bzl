package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// MergeKbuildCompilerGuardAnswers combines independently validated replay
// rounds without changing their exact compiler-context boundaries. It does not
// authenticate round manifests or reached-file provenance; callers must obtain
// each immutable input through a successful KbuildCompilerGuardBatch.Answers.
//
// Nil inputs are ignored. Equivalent inputs may return an existing immutable
// snapshot, preserving its cache identity. Otherwise the merged snapshot owns
// its maps, and each context identity commits to the sorted, distinct input
// entry identities and all merged values. Order and duplicate inputs do not
// matter. Nested grouping is not guaranteed to preserve identity: a coordinator
// should call once with all of its frozen round snapshots.
func MergeKbuildCompilerGuardAnswers(snapshots ...*KbuildCompilerGuardAnswers) (*KbuildCompilerGuardAnswers, error) {
	var first *KbuildCompilerGuardAnswers
	equivalent := true
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		if first == nil {
			first = snapshot
			continue
		}
		if !maps.Equal(first.toolsets, snapshot.toolsets) {
			return nil, fmt.Errorf("cannot merge compiler guard answers with different complete toolset bindings")
		}
		if equivalent && !maps.EqualFunc(first.entries, snapshot.entries, func(left, right kbuildCompilerGuardAnswerEntry) bool {
			return left.identity == right.identity && maps.Equal(left.definitions, right.definitions)
		}) {
			equivalent = false
		}
		if equivalent && !equalKbuildCompilerIntrinsicAnswers(first.intrinsics, snapshot.intrinsics) {
			equivalent = false
		}
	}
	if first == nil || equivalent {
		return first, nil
	}
	type contextAnswers struct {
		definitions map[string]bool
		identities  map[string]bool
	}
	contexts := map[configDependencyCompilerPredefineRequestKey]contextAnswers{}
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		for key, entry := range snapshot.entries {
			merged, found := contexts[key]
			if !found {
				merged = contextAnswers{definitions: map[string]bool{}, identities: map[string]bool{}}
			}
			for name, value := range entry.definitions {
				if previous, found := merged.definitions[name]; found && previous != value {
					return nil, fmt.Errorf("compiler guard rounds disagree about %q in the same compiler context", name)
				}
				merged.definitions[name] = value
			}
			merged.identities[entry.identity] = true
			contexts[key] = merged
		}
	}
	result := &KbuildCompilerGuardAnswers{
		toolsets: maps.Clone(first.toolsets),
		entries:  make(map[configDependencyCompilerPredefineRequestKey]kbuildCompilerGuardAnswerEntry, len(contexts)),
	}
	var err error
	result.intrinsics, err = mergeKbuildCompilerIntrinsicAnswers(snapshots)
	if err != nil {
		return nil, err
	}
	for key, merged := range contexts {
		var identity strings.Builder
		appendConfigDependencyCacheString(&identity, "compiler-guard-answer-merge-v1")
		appendConfigDependencyCacheString(&identity, string(key))
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(result.toolsets)))
		for _, scope := range slices.Sorted(maps.Keys(result.toolsets)) {
			appendConfigDependencyCacheString(&identity, scope)
			appendConfigDependencyCacheString(&identity, result.toolsets[scope])
		}
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(merged.identities)))
		for _, contributor := range slices.Sorted(maps.Keys(merged.identities)) {
			appendConfigDependencyCacheString(&identity, contributor)
		}
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(merged.definitions)))
		for _, name := range slices.Sorted(maps.Keys(merged.definitions)) {
			appendConfigDependencyCacheString(&identity, name)
			if merged.definitions[name] {
				identity.WriteByte('1')
			} else {
				identity.WriteByte('0')
			}
		}
		result.entries[key] = kbuildCompilerGuardAnswerEntry{
			definitions: merged.definitions,
			identity:    fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String()))),
		}
	}
	return result, nil
}
