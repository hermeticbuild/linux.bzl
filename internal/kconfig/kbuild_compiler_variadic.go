package kconfig

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const compilerVariadicCommaStep = "compiler-variadic-comma"
const compilerVariadicCommaMacro = "LINUX_BZL_VARIADIC_COMMA_PROBE"

// CompilerVariadicCommaSourceBytes charges the exact bounded managed input.
func CompilerVariadicCommaSourceBytes(syntax string) (int, error) {
	source, err := compilerVariadicCommaSource(syntax)
	return len(source), err
}

// Syntax is a source grammar, not a compiler-family or language-mode answer.
// Query the two spellings independently: accepting one need not accept both.
func compilerVariadicCommaSource(syntax string) (string, error) {
	var formal, argument string
	switch syntax {
	case "standard":
		formal, argument = "...", "__VA_ARGS__"
	case "named":
		formal, argument = "tail...", "tail"
	default:
		return "", fmt.Errorf("unsupported variadic comma syntax %q", syntax)
	}
	// Never erase a compiler/command-line binding to make the experiment work.
	// The candidate validator also rejects writes to this reserved probe name.
	return "#ifdef " + compilerVariadicCommaMacro + "\n#error probe_name_collision\n#endif\n" +
		"#define " + compilerVariadicCommaMacro + "(" + formal + ") 17 , ##" + argument + " 23\n" +
		compilerVariadicCommaMacro + "()\n", nil
}

func compilerVariadicCommaProbeSyntax(step ProbeStep) (string, error) {
	if step.Name != compilerVariadicCommaStep || step.Candidate == nil ||
		step.Candidate.Policy != ProbeCandidatePolicyCC ||
		step.Candidate.Projection != ProbeCandidateProjectionCompilerIntrinsic ||
		(step.Tool != "cc" && step.Tool != "cxx") || len(step.StdinFragments) != 0 {
		return "", fmt.Errorf("variadic comma probe requires its exact compiler policy, role and static input")
	}
	if err := validateCompilerIntrinsicManagedArguments(step); err != nil {
		return "", err
	}
	for _, syntax := range []string{"standard", "named"} {
		source, _ := compilerVariadicCommaSource(syntax)
		if step.Stdin == source {
			return syntax, nil
		}
	}
	return "", fmt.Errorf("variadic comma probe requires canonical managed source")
}

// Parse two fixed integer tokens with an optional comma, not arbitrary output
// text or truthiness. In particular, removing all whitespace would incorrectly
// accept broken integers such as "1 7 2 3" or merge "1723" into our sentinels.
func parseCompilerVariadicCommaResult(contents string) (bool, error) {
	if len(contents) > 128 {
		return false, fmt.Errorf("variadic comma result exceeds 128 bytes")
	}
	const space = " \t\r\n\v\f"
	text := strings.Trim(contents, space)
	rest, found := strings.CutPrefix(text, "17")
	if found {
		trimmed := strings.TrimLeft(rest, space)
		if tail, comma := strings.CutPrefix(trimmed, ","); comma {
			if strings.TrimLeft(tail, space) == "23" {
				return false, nil
			}
		} else if len(trimmed) < len(rest) && trimmed == "23" {
			return true, nil
		}
	}
	return false, fmt.Errorf("variadic comma result must be exactly 17 23 or 17 , 23")
}

type OptionalCompilerVariadicCommaState uint8

const (
	OptionalCompilerVariadicCommaPending OptionalCompilerVariadicCommaState = iota
	OptionalCompilerVariadicCommaUnqueryable
	OptionalCompilerVariadicCommaAnswered
)

// OptionalCompilerVariadicCommaAttempt measures the ambiguous empty invocation
// of a variadic-only macro, retaining the exact configured compiler context.
// A normal compiler rejection grants no answer; failed authentication or malformed
// successful output poisons the batch. The reference identifies an attempt, not
// a fact. Transport alone grants no source-closure or initial-binding authority.
// Answers() publishes a frozen syntax- and context-bound grammar witness.
func (b *KbuildCompilerGuardBatch) OptionalCompilerVariadicCommaAttempt(
	scope, role, language string,
	arguments, translationUnits []string,
	syntax string,
	environment map[string]string,
) (deleteComma bool, state OptionalCompilerVariadicCommaState, reference ProbeReference, err error) {
	if b == nil || b.builder == nil {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, fmt.Errorf("variadic comma batch is nil")
	}
	if b.err != nil {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, b.err
	}
	if b.frozen {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, fmt.Errorf("variadic comma batch is frozen")
	}
	defer func() {
		if err != nil {
			b.err = err
			deleteComma, state, reference = false, OptionalCompilerVariadicCommaPending, ProbeReference{}
		}
	}()
	if language != "c" && language != "c++" {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, unsupportedCompilerPredefineProjection(fmt.Errorf("variadic comma probe requires C or C++"))
	}
	source, err := compilerVariadicCommaSource(syntax)
	if err != nil {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, err
	}
	probe, err := b.scopes.compilerProjectedRequestWithProjection(scope, role, language, arguments, translationUnits, environment,
		compilerVariadicCommaStep, []string{"-E", "-P", "-x", language, "-"}, source,
		ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: compilerVariadicCommaStep}},
		ProbeCandidateProjectionCompilerIntrinsic)
	if err != nil {
		return false, OptionalCompilerVariadicCommaPending, ProbeReference{}, err
	}
	reference, err = b.registerCompilerGuardProbe(scope, probe)
	if err != nil || b.oracle == nil {
		return false, OptionalCompilerVariadicCommaPending, reference, err
	}
	positive, err := probe.evaluator.readBoolean(reference, probe.request, probe.dependencies...)
	if err != nil {
		return false, OptionalCompilerVariadicCommaPending, reference, err
	}
	result, err := probe.evaluator.readProbeResult(reference, probe.request, probe.dependencies...)
	if err != nil {
		return false, OptionalCompilerVariadicCommaPending, reference, err
	}
	if result.NodeID != reference.NodeID {
		return false, OptionalCompilerVariadicCommaPending, reference, fmt.Errorf("variadic comma attempt has stale node identity")
	}
	step, present := probeResultStep(result.Steps, compilerVariadicCommaStep)
	if !present || step.Status == "skipped" || step.ExitCode < 0 || step.ExitCode > 255 {
		return false, OptionalCompilerVariadicCommaPending, reference, fmt.Errorf("variadic comma attempt has no normal process completion")
	}
	success := step.Status == "success" && step.ExitCode == 0
	if positive != success {
		return false, OptionalCompilerVariadicCommaPending, reference, fmt.Errorf("variadic comma attempt disagrees with its exact process outcome")
	}
	if !success {
		return false, OptionalCompilerVariadicCommaUnqueryable, reference, nil
	}
	deleteComma, err = parseCompilerVariadicCommaResult(step.Stdout)
	if err == nil {
		key := compilerVariadicCommaKey{configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment), syntax}
		observation := compilerVariadicCommaObservation{reference, deleteComma}
		if previous, exists := b.variadics[key]; exists && previous != observation {
			err = fmt.Errorf("variadic comma observations disagree in the same exact context")
		} else {
			if b.variadics == nil {
				b.variadics = map[compilerVariadicCommaKey]compilerVariadicCommaObservation{}
			}
			b.variadics[key] = observation
		}
	}
	return deleteComma, OptionalCompilerVariadicCommaAnswered, reference, err
}

type compilerVariadicCommaKey struct {
	context configDependencyCompilerPredefineRequestKey
	syntax  string
}

type compilerVariadicCommaObservation struct {
	reference   ProbeReference
	deleteComma bool
}

type compilerVariadicCommaAnswer struct {
	deleteComma bool
	identity    string
}

func compilerVariadicCommaAnswerIdentity(key compilerVariadicCommaKey, deleted bool, contributors []string, toolsets map[string]string) string {
	var identity strings.Builder
	for _, value := range []string{"compiler-variadic-comma-answer-v1", string(key.context), key.syntax, fmt.Sprint(deleted), fmt.Sprint(len(contributors))} {
		appendConfigDependencyCacheString(&identity, value)
	}
	for _, value := range contributors {
		appendConfigDependencyCacheString(&identity, value)
	}
	appendConfigDependencyCacheString(&identity, fmt.Sprint(len(toolsets)))
	for _, scope := range slices.Sorted(maps.Keys(toolsets)) {
		appendConfigDependencyCacheString(&identity, scope)
		appendConfigDependencyCacheString(&identity, toolsets[scope])
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String())))
}

func (b *KbuildCompilerGuardBatch) variadicAnswers(toolsets map[string]string) map[compilerVariadicCommaKey]compilerVariadicCommaAnswer {
	if len(b.variadics) == 0 {
		return nil
	}
	answers := make(map[compilerVariadicCommaKey]compilerVariadicCommaAnswer, len(b.variadics))
	for key, value := range b.variadics {
		ref := value.reference
		answers[key] = compilerVariadicCommaAnswer{value.deleteComma, compilerVariadicCommaAnswerIdentity(key, value.deleteComma,
			[]string{ref.NodeID, ref.RequestID, ref.Scope, ref.Kind}, toolsets)}
	}
	return answers
}

func mergeCompilerVariadicCommaAnswers(snapshots []*KbuildCompilerGuardAnswers) (map[compilerVariadicCommaKey]compilerVariadicCommaAnswer, error) {
	type entry struct {
		deleted    bool
		identities map[string]bool
	}
	entries := map[compilerVariadicCommaKey]entry{}
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		for key, answer := range snapshot.variadics {
			value, found := entries[key]
			if found && value.deleted != answer.deleteComma {
				return nil, fmt.Errorf("compiler rounds disagree about variadic comma grammar")
			}
			if !found {
				value = entry{answer.deleteComma, map[string]bool{}}
			}
			value.identities[answer.identity] = true
			entries[key] = value
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	result := make(map[compilerVariadicCommaKey]compilerVariadicCommaAnswer, len(entries))
	for key, value := range entries {
		identities := slices.Sorted(maps.Keys(value.identities))
		identity := identities[0]
		if len(identities) > 1 {
			identity = compilerVariadicCommaAnswerIdentity(key, value.deleted, identities, nil)
		}
		result[key] = compilerVariadicCommaAnswer{value.deleted, identity}
	}
	return result, nil
}

func configDependencySupplementalCompilerVariadicComma(plan *ActionPlan, key compilerVariadicCommaKey) (compilerVariadicCommaAnswer, bool, error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerGuardAnswers == nil {
		return compilerVariadicCommaAnswer{}, false, nil
	}
	answers := plan.metadata.compilerGuardAnswers
	answer, found := answers.variadics[key]
	if !found {
		return compilerVariadicCommaAnswer{}, false, nil
	}
	for scope, expected := range answers.toolsets {
		if plan.Toolsets[scope] != expected {
			return compilerVariadicCommaAnswer{}, false, fmt.Errorf("variadic comma answers have a different %s toolset", scope)
		}
	}
	return answer, true, nil
}
