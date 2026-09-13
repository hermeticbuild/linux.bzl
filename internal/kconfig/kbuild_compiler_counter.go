package kconfig

import (
	"crypto/sha256"
	"fmt"
)

const compilerCounterSequenceStep = "compiler-counter-sequence"

// CompilerCounterSequenceSourceBytes validates a resource-bounded query before
// coordinators charge its generated input without allocating that input.
func CompilerCounterSequenceSourceBytes(count int) (int, error) {
	if err := validateCompilerCounterSequenceCount(count); err != nil {
		return 0, err
	}
	return count * len("__COUNTER__\n"), nil
}

// The count is authenticated by the exact static input in the request identity.
// Only this distinct step admits ordered object expansions; ordinary intrinsic
// steps retain their sorted unique function-call grammar.
func compilerCounterProbeCount(step ProbeStep) (int, error) {
	if step.Name != compilerCounterSequenceStep || step.Candidate == nil ||
		step.Candidate.Policy != ProbeCandidatePolicyCC ||
		step.Candidate.Projection != ProbeCandidateProjectionCompilerIntrinsic ||
		(step.Tool != "cc" && step.Tool != "cxx") {
		return 0, fmt.Errorf("counter sequence requires its compiler candidate policy, role and step")
	}
	const expansion = "__COUNTER__\n"
	if len(step.StdinFragments) != 0 || len(step.Stdin) > MaxProbeInterpolatedBytes ||
		len(step.Stdin)%len(expansion) != 0 {
		return 0, fmt.Errorf("counter sequence requires bounded static canonical input")
	}
	count := len(step.Stdin) / len(expansion)
	if err := validateCompilerCounterSequenceCount(count); err != nil {
		return 0, err
	}
	for offset := 0; offset < len(step.Stdin); offset += len(expansion) {
		if step.Stdin[offset:offset+len(expansion)] != expansion {
			return 0, fmt.Errorf("counter sequence input contains something other than its measured spelling")
		}
	}
	if err := validateCompilerIntrinsicManagedArguments(step); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *KbuildProbeScopes) compilerCounterSequenceRequest(
	scope, role, language string,
	arguments, translationUnits []string,
	count int,
	environment map[string]string,
) (*compilerProjectedProbe, error) {
	if language != "c" && language != "c++" {
		return nil, unsupportedCompilerPredefineProjection(fmt.Errorf("counter sequence requires c or c++ language"))
	}
	stdin, err := compilerCounterSequenceSource(count)
	if err != nil {
		return nil, unsupportedCompilerPredefineProjection(err)
	}
	return s.compilerProjectedRequestWithProjection(scope, role, language, arguments, translationUnits, environment,
		compilerCounterSequenceStep, []string{"-E", "-P", "-x", language, "-"}, stdin,
		ProbeOutcome{Kind: "text", Step: compilerCounterSequenceStep, Stream: "stdout", RequireSuccess: true},
		ProbeCandidateProjectionCompilerIntrinsic)
}

// Query transport only: callers must independently prove the original binding
// and ordered source effects before installing a branch-local cursor. Discovery
// publishes no vector; replay requires the exact successful request/toolset.
func (s *KbuildProbeScopes) compilerCounterSequence(
	scope, role, language string,
	arguments, translationUnits []string,
	count int,
	environment map[string]string,
) (*compilerCounterSequence, bool, error) {
	probe, err := s.compilerCounterSequenceRequest(scope, role, language, arguments, translationUnits, count, environment)
	if err != nil {
		return nil, false, err
	}
	token, err := probe.evaluator.requestText(probe.request, probe.dependencies...)
	if err != nil {
		return nil, false, err
	}
	resolved, err := probe.evaluator.ResolveSymbolic(token)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %s %s counter sequence: %w", scope, role, err)
	}
	if resolved == token {
		return nil, false, nil
	}
	context := configDependencyCompilerPredefineKey(scope, role, language, arguments, translationUnits, environment)
	sequence, err := parseCompilerCounterSequence(fmt.Sprintf("%x", sha256.Sum256([]byte(context))), count, resolved)
	return sequence, err == nil, err
}
