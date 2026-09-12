package kconfig

import (
	"fmt"
	"slices"
	"strings"
)

const maxCompilerIntrinsicCalls = 4096

// CompilerIntrinsicCall is one bounded, value-sensitive initial compiler query.
// It does not prove source reachability or that a source-level occurrence still
// names the initial binding. The ordered source interpreter owns those proofs.
type CompilerIntrinsicCall struct {
	Operator string `json:"operator"`
	Operand  string `json:"operand"`
}

// This is an admitted operator grammar, not a compiler capability table.
// Availability and results must be measured independently for each operator.
func isCompilerIntrinsicOperator(name string) bool {
	return name == "__has_attribute" || name == "__has_builtin"
}

// ValidateCompilerIntrinsicCall accepts the supported call grammar, never a
// capability table. Unknown identifiers remain valid queries whose integer
// result must be measured by the configured compiler.
func ValidateCompilerIntrinsicCall(call CompilerIntrinsicCall) error {
	if !isCompilerIntrinsicOperator(call.Operator) {
		return fmt.Errorf("unsupported compiler intrinsic operator %q", call.Operator)
	}
	if len(call.Operand) == 0 || len(call.Operand) > 128 ||
		!configDependencyConditionalIntegerIdentifier(call.Operand) {
		return fmt.Errorf("compiler intrinsic operand must be one bounded ASCII identifier")
	}
	if isCompilerIntrinsicOperator(call.Operand) {
		// Every query undefines its operand. A different operator must not be
		// disabled by an earlier call when independently admitted calls batch.
		return fmt.Errorf("compiler intrinsic operand collides with a managed operator")
	}
	return nil
}

func compilerIntrinsicIntegerSource(operator, operand string) (string, error) {
	_, source, err := compilerIntrinsicIntegersSource([]CompilerIntrinsicCall{{operator, operand}})
	return source, err
}

func compareCompilerIntrinsicCalls(left, right CompilerIntrinsicCall) int {
	if order := strings.Compare(left.Operator, right.Operator); order != 0 {
		return order
	}
	return strings.Compare(left.Operand, right.Operand)
}

// This checks an already canonical call vector without building its source or
// allocating a result. Parsers use it before trusting positional result mapping.
func compilerIntrinsicOrderedSourceSize(ordered []CompilerIntrinsicCall) (int, error) {
	if len(ordered) == 0 || len(ordered) > maxCompilerIntrinsicCalls {
		return 0, fmt.Errorf("compiler intrinsic query requires 1 to %d calls", maxCompilerIntrinsicCalls)
	}
	size := 0
	for index, call := range ordered {
		if err := ValidateCompilerIntrinsicCall(call); err != nil {
			return 0, err
		}
		if index != 0 && compareCompilerIntrinsicCalls(ordered[index-1], call) >= 0 {
			return 0, fmt.Errorf("compiler intrinsic calls must be sorted and unique")
		}
		bytes := len(call.Operator) + 2*len(call.Operand) + 11
		if bytes > MaxProbeInterpolatedBytes-size {
			return 0, fmt.Errorf("compiler intrinsic stdin exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
		size += bytes
	}
	return size, nil
}

func compilerIntrinsicIntegersSource(calls []CompilerIntrinsicCall) ([]CompilerIntrinsicCall, string, error) {
	// Bound caller-owned data before cloning/sorting or allocating source. Each
	// identifier is bounded even when duplicate calls will later be removed.
	if len(calls) == 0 || len(calls) > maxCompilerIntrinsicCalls {
		return nil, "", fmt.Errorf("compiler intrinsic query requires 1 to %d calls", maxCompilerIntrinsicCalls)
	}
	for _, call := range calls {
		if err := ValidateCompilerIntrinsicCall(call); err != nil {
			return nil, "", err
		}
	}
	ordered := slices.Clone(calls)
	slices.SortFunc(ordered, compareCompilerIntrinsicCalls)
	ordered = slices.Compact(ordered)
	size, err := compilerIntrinsicOrderedSourceSize(ordered)
	if err != nil {
		return nil, "", err
	}
	// The caller has already proven this terminal operand in its source state.
	// It must not expand again through an initial command-line/compiler macro.
	// Only the operand is undefined; the operator is never redefined or supplied
	// with a fallback. Unsupported operators consequently fail or remain text.
	// Interleaving is justified for the admitted nontextual initial intrinsic,
	// not for arbitrary source replacements which might reference another operand.
	var source strings.Builder
	source.Grow(size)
	for _, call := range ordered {
		source.WriteString("#undef ")
		source.WriteString(call.Operand)
		source.WriteByte('\n')
		source.WriteString(call.Operator)
		source.WriteByte('(')
		source.WriteString(call.Operand)
		source.WriteString(")\n")
	}
	return ordered, source.String(), nil
}

// parseCompilerIntrinsicIntegerResult retains the compiler's exact integer
// token, not its truth value. In particular, dated attribute versions such as
// 202311 are not interchangeable with 1. The accepted literal domain is the
// existing portable signed preprocessing-integer subset; unsupported types,
// signs/expressions, extra tokens and non-preprocessing whitespace fail closed.
func parseCompilerIntrinsicIntegerResult(contents string) (string, error) {
	if len(contents) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("compiler intrinsic result exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	token := strings.Trim(contents, " \t\r\n\v\f")
	if len(token) == 0 || len(token) > 128 {
		return "", fmt.Errorf("compiler intrinsic result is not one bounded integer token")
	}
	if _, reason := configDependencyConditionalIntegerLiteral(token); reason != "" {
		return "", fmt.Errorf("compiler intrinsic result: %s", reason)
	}
	return token, nil
}

func parseCompilerIntrinsicIntegersResult(ordered []CompilerIntrinsicCall, contents string) (map[CompilerIntrinsicCall]string, error) {
	if _, err := compilerIntrinsicOrderedSourceSize(ordered); err != nil {
		return nil, err
	}
	if len(contents) > MaxProbeInterpolatedBytes {
		return nil, fmt.Errorf("compiler intrinsic result exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	// A hostile stream can contain many more tokens than calls. Iterate rather
	// than splitting into an unbounded slice; reject the first surplus token.
	tokens := make([]string, 0, len(ordered))
	for token := range strings.FieldsFuncSeq(contents, func(character rune) bool {
		return character == ' ' || character == '\t' || character == '\r' ||
			character == '\n' || character == '\v' || character == '\f'
	}) {
		if len(tokens) == len(ordered) {
			return nil, fmt.Errorf("compiler intrinsic result has more than %d values", len(ordered))
		}
		value, err := parseCompilerIntrinsicIntegerResult(token)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, value)
	}
	if len(tokens) != len(ordered) {
		return nil, fmt.Errorf("compiler intrinsic result has %d values, want %d", len(tokens), len(ordered))
	}
	// Publish nothing until every token and the complete positional map pass.
	values := make(map[CompilerIntrinsicCall]string, len(ordered))
	for index, call := range ordered {
		values[call] = tokens[index]
	}
	return values, nil
}

func (s *KbuildProbeScopes) compilerIntrinsicIntegerRequest(
	scope, role, language string,
	arguments, translationUnits []string,
	operator, operand string,
	environment map[string]string,
) (*compilerProjectedProbe, error) {
	_, probe, err := s.compilerIntrinsicIntegersRequest(scope, role, language, arguments, translationUnits,
		[]CompilerIntrinsicCall{{operator, operand}}, environment)
	return probe, err
}

func (s *KbuildProbeScopes) compilerIntrinsicIntegersRequest(
	scope, role, language string,
	arguments, translationUnits []string,
	calls []CompilerIntrinsicCall,
	environment map[string]string,
) ([]CompilerIntrinsicCall, *compilerProjectedProbe, error) {
	if language != "c" && language != "c++" {
		return nil, nil, unsupportedCompilerPredefineProjection(fmt.Errorf("compiler intrinsic query requires c or c++ language"))
	}
	ordered, stdin, err := compilerIntrinsicIntegersSource(calls)
	if err != nil {
		return nil, nil, unsupportedCompilerPredefineProjection(err)
	}
	const step = "compiler-intrinsic-integer"
	probe, err := s.compilerProjectedRequestWithProjection(scope, role, language, arguments, translationUnits, environment,
		step, []string{"-E", "-P", "-x", language, "-"}, stdin,
		ProbeOutcome{Kind: "text", Step: step, Stream: "stdout", RequireSuccess: true},
		ProbeCandidateProjectionCompilerIntrinsic)
	if err != nil {
		return nil, nil, err
	}
	return ordered, probe, nil
}

// CompilerIntrinsicInteger measures an admitted call in the exact projected
// initial compiler invocation. Macro replacements remain byte-exact, including
// symbolic -D operands. The bool is ready, never the measured truth value.
// Discovery returns ("", false, nil); missing/malformed/failed replay is an error.
// Source-level binding and operand-expansion proofs are deliberately separate.
func (s *KbuildProbeScopes) CompilerIntrinsicInteger(
	scope, role, language string,
	arguments, translationUnits []string,
	operator, operand string,
	environment map[string]string,
) (string, bool, error) {
	call := CompilerIntrinsicCall{operator, operand}
	values, ready, err := s.CompilerIntrinsicIntegers(scope, role, language, arguments, translationUnits, []CompilerIntrinsicCall{call}, environment)
	return values[call], ready, err
}

// CompilerIntrinsicIntegers measures a canonical batch in one compiler process.
// Results retain exact integer tokens and are caller-owned. No partial map is
// returned during discovery or after any failed/malformed replay result. The
// caller must prove the initial nontextual binding and each terminal operand;
// this transport does not establish source-level expansion semantics.
func (s *KbuildProbeScopes) CompilerIntrinsicIntegers(
	scope, role, language string,
	arguments, translationUnits []string,
	calls []CompilerIntrinsicCall,
	environment map[string]string,
) (map[CompilerIntrinsicCall]string, bool, error) {
	ordered, probe, err := s.compilerIntrinsicIntegersRequest(scope, role, language, arguments, translationUnits, calls, environment)
	if err != nil {
		return nil, false, err
	}
	token, err := probe.evaluator.requestText(probe.request, probe.dependencies...)
	if err != nil {
		return nil, false, err
	}
	resolved, err := probe.evaluator.ResolveSymbolic(token)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %s %s compiler intrinsic: %w", scope, role, err)
	}
	if resolved == token {
		return nil, false, nil
	}
	value, err := parseCompilerIntrinsicIntegersResult(ordered, resolved)
	return value, err == nil, err
}
