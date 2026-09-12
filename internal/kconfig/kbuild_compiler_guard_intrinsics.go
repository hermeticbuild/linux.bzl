package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Intrinsic answers deliberately live outside the definedness map. Presence
// of an operator is not its integer result, and a missing operand is not zero.
type kbuildCompilerIntrinsicObservation struct {
	reference ProbeReference
	token     string
}

type kbuildCompilerIntrinsicAnswer struct {
	token, identity string
}

// CompilerIntrinsicInteger discovers or strictly replays one bounded compiler
// predicate. Unlike definedness projection, its request preserves replacement
// text in ordered command-line macro definitions. Answers are pure initial
// compiler facts, not authority for a source-mutated intrinsic binding.
func (b *KbuildCompilerGuardBatch) CompilerIntrinsicInteger(
	scope, role, language string,
	arguments, translationUnits []string,
	operator, operand string,
	environment map[string]string,
) (string, bool, error) {
	call := CompilerIntrinsicCall{Operator: operator, Operand: operand}
	values, ready, err := b.CompilerIntrinsicIntegers(scope, role, language, arguments, translationUnits, []CompilerIntrinsicCall{call}, environment)
	if err != nil || !ready {
		return "", false, err
	}
	return values[call], true, nil
}

// CompilerIntrinsicIntegers measures one canonical vector in one exact
// compiler context. The returned map owns its storage. No prefix is published
// if any result or prior overlapping observation is invalid or inconsistent.
func (b *KbuildCompilerGuardBatch) CompilerIntrinsicIntegers(
	scope, role, language string,
	arguments, translationUnits []string,
	calls []CompilerIntrinsicCall,
	environment map[string]string,
) (map[CompilerIntrinsicCall]string, bool, error) {
	if b == nil || b.builder == nil {
		return nil, false, fmt.Errorf("compiler guard batch is nil")
	}
	if b.err != nil {
		return nil, false, b.err
	}
	if b.frozen {
		return nil, false, fmt.Errorf("compiler guard batch is frozen")
	}
	ordered, probe, err := b.scopes.compilerIntrinsicIntegersRequest(scope, role, language, arguments, translationUnits, calls, environment)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	for _, dependency := range probe.dependencies {
		if err := b.importDependency(dependency, map[string]bool{}, 0); err != nil {
			b.err = err
			return nil, false, err
		}
	}
	reference, err := b.builder.Request(scope, probe.request, probe.dependencies...)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	if !b.terminalIDs[reference.NodeID] {
		b.terminalIDs[reference.NodeID] = true
		b.terminals = append(b.terminals, reference)
	}
	if b.oracle == nil {
		return nil, false, nil
	}
	contents, err := probe.evaluator.readText(reference, probe.request, probe.dependencies...)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	values, err := parseCompilerIntrinsicIntegersResult(ordered, contents)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	key := configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment)
	if err := b.recordIntrinsicAnswers(key, reference, values); err != nil {
		b.err = err
		return nil, false, err
	}
	return values, true, nil
}

func (b *KbuildCompilerGuardBatch) recordIntrinsicAnswers(key configDependencyCompilerPredefineRequestKey, reference ProbeReference, values map[CompilerIntrinsicCall]string) error {
	// Validate every overlap before mutating any observation. The same call
	// may occur in different canonical vectors, but each complete reference
	// and its exact measured token must remain consistent.
	for call, token := range values {
		for nodeID, previous := range b.intrinsics[key][call] {
			if previous.token != token || nodeID == reference.NodeID && previous.reference != reference {
				return fmt.Errorf("compiler intrinsic query returned conflicting measured answers")
			}
		}
	}
	if b.intrinsics == nil {
		b.intrinsics = map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]map[string]kbuildCompilerIntrinsicObservation{}
	}
	if b.intrinsics[key] == nil {
		b.intrinsics[key] = map[CompilerIntrinsicCall]map[string]kbuildCompilerIntrinsicObservation{}
	}
	for call, token := range values {
		if b.intrinsics[key][call] == nil {
			b.intrinsics[key][call] = map[string]kbuildCompilerIntrinsicObservation{}
		}
		b.intrinsics[key][call][reference.NodeID] = kbuildCompilerIntrinsicObservation{reference: reference, token: token}
	}
	return nil
}

func (b *KbuildCompilerGuardBatch) intrinsicAnswers(toolsets map[string]string) map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer {
	if len(b.intrinsics) == 0 {
		return nil
	}
	answers := make(map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer, len(b.intrinsics))
	for key, observations := range b.intrinsics {
		entries := make(map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer, len(observations))
		for call, contributors := range observations {
			nodeIDs := slices.Sorted(maps.Keys(contributors))
			token := contributors[nodeIDs[0]].token
			var identity strings.Builder
			for _, value := range []string{"compiler-intrinsic-answer-v2", string(key), call.Operator, call.Operand, token, fmt.Sprint(len(nodeIDs))} {
				appendConfigDependencyCacheString(&identity, value)
			}
			for _, nodeID := range nodeIDs {
				reference := contributors[nodeID].reference
				for _, value := range []string{reference.NodeID, reference.RequestID, reference.Scope, reference.Kind} {
					appendConfigDependencyCacheString(&identity, value)
				}
			}
			appendConfigDependencyCacheString(&identity, fmt.Sprint(len(toolsets)))
			for _, scope := range slices.Sorted(maps.Keys(toolsets)) {
				appendConfigDependencyCacheString(&identity, scope)
				appendConfigDependencyCacheString(&identity, toolsets[scope])
			}
			entries[call] = kbuildCompilerIntrinsicAnswer{token: token, identity: fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String())))}
		}
		answers[key] = entries
	}
	return answers
}

func configDependencySupplementalCompilerIntrinsic(
	plan *ActionPlan,
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	call CompilerIntrinsicCall,
) (token, identity string, ready bool, err error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerGuardAnswers == nil {
		return "", "", false, nil
	}
	answers := plan.metadata.compilerGuardAnswers
	key := configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment)
	entry, found := answers.intrinsics[key][call]
	if !found {
		return "", "", false, nil
	}
	for configuredScope, expected := range answers.toolsets {
		if plan.Toolsets[configuredScope] != expected {
			return "", "", false, fmt.Errorf("supplemental compiler intrinsic answers have a different %s toolset", configuredScope)
		}
	}
	return entry.token, entry.identity, true, nil
}

func equalKbuildCompilerIntrinsicAnswers(left, right map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer) bool {
	return maps.EqualFunc(left, right, func(a, b map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer) bool { return maps.Equal(a, b) })
}

func mergeKbuildCompilerIntrinsicAnswers(snapshots []*KbuildCompilerGuardAnswers) (map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer, error) {
	type mergedEntry struct {
		token      string
		identities map[string]bool
	}
	merged := map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]mergedEntry{}
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		for key, entries := range snapshot.intrinsics {
			if merged[key] == nil {
				merged[key] = map[CompilerIntrinsicCall]mergedEntry{}
			}
			for call, answer := range entries {
				entry, found := merged[key][call]
				if found && entry.token != answer.token {
					return nil, fmt.Errorf("compiler intrinsic rounds disagree about %s(%s) in the same compiler context", call.Operator, call.Operand)
				}
				if !found {
					entry = mergedEntry{token: answer.token, identities: map[string]bool{}}
				}
				entry.identities[answer.identity] = true
				merged[key][call] = entry
			}
		}
	}
	if len(merged) == 0 {
		return nil, nil
	}
	result := make(map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer, len(merged))
	for key, entries := range merged {
		result[key] = make(map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer, len(entries))
		for call, entry := range entries {
			identities := slices.Sorted(maps.Keys(entry.identities))
			identity := identities[0]
			if len(identities) > 1 {
				var data strings.Builder
				for _, value := range []string{"compiler-intrinsic-answer-merge-v1", string(key), call.Operator, call.Operand, entry.token, fmt.Sprint(len(identities))} {
					appendConfigDependencyCacheString(&data, value)
				}
				for _, contributor := range identities {
					appendConfigDependencyCacheString(&data, contributor)
				}
				identity = fmt.Sprintf("%x", sha256.Sum256([]byte(data.String())))
			}
			result[key][call] = kbuildCompilerIntrinsicAnswer{token: entry.token, identity: identity}
		}
	}
	return result, nil
}
