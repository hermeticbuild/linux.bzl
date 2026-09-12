package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type kbuildCompilerGuardAnswerObservation struct {
	reference   ProbeReference
	definitions map[string]bool
}

type kbuildCompilerGuardAnswerEntry struct {
	definitions map[string]bool
	identity    string
}

// KbuildCompilerGuardAnswers is an immutable process-local snapshot of measured
// initial compiler facts. It does not authenticate source bytes, a generated
// producer, or an execution cut. The round coordinator must separately validate
// its frozen request manifest and reached-file provenance before attaching it.
type KbuildCompilerGuardAnswers struct {
	toolsets   map[string]string
	entries    map[configDependencyCompilerPredefineRequestKey]kbuildCompilerGuardAnswerEntry
	intrinsics map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]kbuildCompilerIntrinsicAnswer
}

// Definedness queries already normalize recognized object-like -D replacement
// values before constructing their requests and symbolic dependencies. Index
// their measured answers using that SAME projection, so equivalent requests
// do not leave a sibling consumer's initial namespace unknown. Preserve all
// other context fields and option ownership through the existing helpers.
// This is not an intrinsic or source-proof key: original replacement values
// are reapplied separately before each translation unit is interpreted.
func configDependencyCompilerGuardDefinednessKey(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) configDependencyCompilerPredefineRequestKey {
	return configDependencyCompilerPredefineKey(scope, role, language,
		canonicalCompilerPredefineArguments(role, arguments), translationUnits, environment)
}

// CompilerGuardDefinednessSchedulingKey groups optional name requests using the
// same projection as measured definedness answers. It is only a process-local
// scheduling hint within ONE fixed variant's current scope/toolset bindings.
// It is not a request identity, intrinsic key, measured answer, or source proof.
// Callers must retain original query descriptors and authenticate their complete
// requests, dependencies and results before applying any compiler facts.
func CompilerGuardDefinednessSchedulingKey(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) [32]byte {
	return sha256.Sum256([]byte(configDependencyCompilerGuardDefinednessKey(scope, role, language, arguments, translationUnits, environment)))
}

func (b *KbuildCompilerGuardBatch) recordAnswers(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	reference ProbeReference,
	definitions map[string]bool,
) error {
	key := configDependencyCompilerGuardDefinednessKey(scope, role, language, arguments, translationUnits, environment)
	if b.observations == nil {
		b.observations = map[configDependencyCompilerPredefineRequestKey]map[string]kbuildCompilerGuardAnswerObservation{}
	}
	if b.observations[key] == nil {
		b.observations[key] = map[string]kbuildCompilerGuardAnswerObservation{}
	}
	if previous, found := b.observations[key][reference.NodeID]; found {
		if previous.reference != reference || !maps.Equal(previous.definitions, definitions) {
			return fmt.Errorf("compiler guard query returned conflicting measured answers")
		}
		return nil
	}
	b.observations[key][reference.NodeID] = kbuildCompilerGuardAnswerObservation{
		reference: reference, definitions: maps.Clone(definitions),
	}
	return nil
}

// Answers freezes and validates this replay round. Discovery and failed replay
// cannot publish even a partial answer snapshot. Different name batches in one
// projected definedness context are merged only when all shared answers agree.
func (b *KbuildCompilerGuardBatch) Answers() (*KbuildCompilerGuardAnswers, error) {
	if b == nil {
		return nil, fmt.Errorf("compiler guard answers require a completed supplemental oracle")
	}
	plan, err := b.Plan()
	if err != nil {
		return nil, err
	}
	if b.oracle == nil {
		return nil, fmt.Errorf("compiler guard answers require a completed supplemental oracle")
	}
	answers := &KbuildCompilerGuardAnswers{
		toolsets: maps.Clone(plan.Toolsets),
		entries:  map[configDependencyCompilerPredefineRequestKey]kbuildCompilerGuardAnswerEntry{},
	}
	for _, key := range slices.Sorted(maps.Keys(b.observations)) {
		definitions := map[string]bool{}
		var identity strings.Builder
		appendConfigDependencyCacheString(&identity, "compiler-guard-answers-v2")
		appendConfigDependencyCacheString(&identity, string(key))
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(answers.toolsets)))
		for _, scope := range slices.Sorted(maps.Keys(answers.toolsets)) {
			appendConfigDependencyCacheString(&identity, scope)
			appendConfigDependencyCacheString(&identity, answers.toolsets[scope])
		}
		appendConfigDependencyCacheString(&identity, fmt.Sprint(len(b.observations[key])))
		for _, nodeID := range slices.Sorted(maps.Keys(b.observations[key])) {
			observation := b.observations[key][nodeID]
			for _, value := range []string{observation.reference.NodeID, observation.reference.RequestID, observation.reference.Scope, observation.reference.Kind} {
				appendConfigDependencyCacheString(&identity, value)
			}
			appendConfigDependencyCacheString(&identity, fmt.Sprint(len(observation.definitions)))
			for _, name := range slices.Sorted(maps.Keys(observation.definitions)) {
				value := observation.definitions[name]
				if previous, found := definitions[name]; found && previous != value {
					return nil, fmt.Errorf("compiler guard batches disagree about %q in the same compiler context", name)
				}
				definitions[strings.Clone(name)] = value
				appendConfigDependencyCacheString(&identity, name)
				if value {
					identity.WriteByte('1')
				} else {
					identity.WriteByte('0')
				}
			}
		}
		answers.entries[key] = kbuildCompilerGuardAnswerEntry{
			definitions: definitions, identity: fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String()))),
		}
	}
	answers.intrinsics = b.intrinsicAnswers(plan.Toolsets)
	return answers, nil
}

func configDependencySupplementalCompilerDefinedness(
	plan *ActionPlan,
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (map[string]bool, string, error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerGuardAnswers == nil {
		return nil, "", nil
	}
	answers := plan.metadata.compilerGuardAnswers
	key := configDependencyCompilerGuardDefinednessKey(scope, role, language, arguments, translationUnits, environment)
	entry, found := answers.entries[key]
	if !found {
		return nil, "", nil
	}
	// Target queries may depend on host-scoped compiler-context probes. Bind
	// the complete configured toolset vector, not just the query's final scope.
	for configuredScope, identity := range answers.toolsets {
		if plan.Toolsets[configuredScope] != identity {
			return nil, "", fmt.Errorf("supplemental compiler guard answers have a different %s toolset", configuredScope)
		}
	}
	return maps.Clone(entry.definitions), entry.identity, nil
}
