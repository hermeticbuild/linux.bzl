package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type kbuildCompilerCounterObservation struct {
	reference ProbeReference
	sequence  *compilerCounterSequence
}

type kbuildCompilerCounterAnswer struct {
	sequence *compilerCounterSequence
	identity string
}

// CompilerCounterSequence records a bounded measured vector in this detached
// replay round. It grants no initial definedness or source-binding authority.
// Discovery and failed replay cannot publish a partial vector or snapshot.
func (b *KbuildCompilerGuardBatch) CompilerCounterSequence(
	scope, role, language string,
	arguments, translationUnits []string,
	count int,
	environment map[string]string,
) (bool, error) {
	if b == nil || b.builder == nil {
		return false, fmt.Errorf("compiler counter batch is nil")
	}
	if b.err != nil {
		return false, b.err
	}
	if b.frozen {
		return false, fmt.Errorf("compiler counter batch is frozen")
	}
	probe, err := b.scopes.compilerCounterSequenceRequest(scope, role, language, arguments, translationUnits, count, environment)
	if err != nil {
		b.err = err
		return false, err
	}
	reference, err := b.registerCompilerGuardProbe(scope, probe)
	if err != nil {
		return false, err
	}
	if b.oracle == nil {
		return false, nil
	}
	contents, err := probe.evaluator.readText(reference, probe.request, probe.dependencies...)
	if err != nil {
		b.err = err
		return false, err
	}
	key := configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment)
	sequence, err := parseCompilerCounterSequence(fmt.Sprintf("%x", sha256.Sum256([]byte(key))), count, contents)
	if err == nil {
		err = b.recordCounterAnswer(key, reference, sequence)
	}
	if err != nil {
		b.err = err
		return false, err
	}
	return true, nil
}

func compatibleCompilerCounterSequences(left, right *compilerCounterSequence) bool {
	return left != nil && right != nil && left.context == right.context &&
		slices.Equal(left.values[:min(len(left.values), len(right.values))], right.values[:min(len(left.values), len(right.values))])
}

func (b *KbuildCompilerGuardBatch) recordCounterAnswer(key configDependencyCompilerPredefineRequestKey, reference ProbeReference, sequence *compilerCounterSequence) error {
	for nodeID, previous := range b.counters[key] {
		if !compatibleCompilerCounterSequences(previous.sequence, sequence) ||
			nodeID == reference.NodeID && (previous.reference != reference || previous.sequence.identity != sequence.identity) {
			return fmt.Errorf("compiler counter queries disagree in the same exact context")
		}
	}
	if b.counters == nil {
		b.counters = map[configDependencyCompilerPredefineRequestKey]map[string]kbuildCompilerCounterObservation{}
	}
	if b.counters[key] == nil {
		b.counters[key] = map[string]kbuildCompilerCounterObservation{}
	}
	b.counters[key][reference.NodeID] = kbuildCompilerCounterObservation{reference, sequence}
	return nil
}

func (b *KbuildCompilerGuardBatch) counterAnswers(toolsets map[string]string) map[configDependencyCompilerPredefineRequestKey]kbuildCompilerCounterAnswer {
	if len(b.counters) == 0 {
		return nil
	}
	result := make(map[configDependencyCompilerPredefineRequestKey]kbuildCompilerCounterAnswer, len(b.counters))
	for key, observations := range b.counters {
		var identity strings.Builder
		appendConfigDependencyCacheString(&identity, "compiler-counter-answer-v1")
		appendConfigDependencyCacheString(&identity, string(key))
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(observations)))
		var sequence *compilerCounterSequence
		for _, nodeID := range slices.Sorted(maps.Keys(observations)) {
			observation := observations[nodeID]
			for _, value := range []string{observation.reference.NodeID, observation.reference.RequestID,
				observation.reference.Scope, observation.reference.Kind, observation.sequence.identity} {
				appendConfigDependencyCacheString(&identity, value)
			}
			if sequence == nil || len(observation.sequence.values) > len(sequence.values) {
				sequence = observation.sequence
			}
		}
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(toolsets)))
		for _, scope := range slices.Sorted(maps.Keys(toolsets)) {
			appendConfigDependencyCacheString(&identity, scope)
			appendConfigDependencyCacheString(&identity, toolsets[scope])
		}
		result[key] = kbuildCompilerCounterAnswer{sequence, fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String())))}
	}
	return result
}

func equalKbuildCompilerCounterAnswers(left, right map[configDependencyCompilerPredefineRequestKey]kbuildCompilerCounterAnswer) bool {
	return maps.EqualFunc(left, right, func(a, b kbuildCompilerCounterAnswer) bool {
		return a.identity == b.identity && a.sequence != nil && b.sequence != nil && a.sequence.identity == b.sequence.identity
	})
}

func mergeKbuildCompilerCounterAnswers(snapshots []*KbuildCompilerGuardAnswers) (map[configDependencyCompilerPredefineRequestKey]kbuildCompilerCounterAnswer, error) {
	type mergedCounter struct {
		sequence   *compilerCounterSequence
		identities map[string]bool
	}
	contexts := map[configDependencyCompilerPredefineRequestKey]mergedCounter{}
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		for key, answer := range snapshot.counters {
			entry, found := contexts[key]
			if found && !compatibleCompilerCounterSequences(entry.sequence, answer.sequence) {
				return nil, fmt.Errorf("compiler counter rounds disagree in the same exact context")
			}
			if !found {
				entry = mergedCounter{answer.sequence, map[string]bool{}}
			} else if len(answer.sequence.values) > len(entry.sequence.values) {
				entry.sequence = answer.sequence
			}
			entry.identities[answer.identity] = true
			contexts[key] = entry
		}
	}
	if len(contexts) == 0 {
		return nil, nil
	}
	result := make(map[configDependencyCompilerPredefineRequestKey]kbuildCompilerCounterAnswer, len(contexts))
	for key, entry := range contexts {
		var identity strings.Builder
		appendConfigDependencyCacheString(&identity, "compiler-counter-answer-merge-v1")
		appendConfigDependencyCacheString(&identity, string(key))
		appendConfigDependencyCacheString(&identity, entry.sequence.identity)
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(entry.identities)))
		for _, contributor := range slices.Sorted(maps.Keys(entry.identities)) {
			appendConfigDependencyCacheString(&identity, contributor)
		}
		result[key] = kbuildCompilerCounterAnswer{entry.sequence, fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String())))}
	}
	return result, nil
}

func configDependencySupplementalCompilerCounter(
	plan *ActionPlan, scope, role, language string,
	arguments, translationUnits []string, environment map[string]string,
) (*compilerCounterSequence, string, error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerGuardAnswers == nil {
		return nil, "", nil
	}
	answers := plan.metadata.compilerGuardAnswers
	key := configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment)
	entry, found := answers.counters[key]
	if !found {
		return nil, "", nil
	}
	for scope, expected := range answers.toolsets {
		if plan.Toolsets[scope] != expected {
			return nil, "", fmt.Errorf("supplemental compiler counter answers have a different %s toolset", scope)
		}
	}
	return entry.sequence, entry.identity, nil
}
