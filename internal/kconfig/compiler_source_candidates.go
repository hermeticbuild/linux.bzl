package kconfig

import (
	"slices"
	"strings"
)

// A bounded over-approximation of source-owned Make text. Conditional leaves
// independently include both possibilities; no compiler answer is invented and
// correlations may only add possibilities. Unknown result text, unsupported
// syntax, or exhausted work retain all original candidate claims.
type compilerSourcePossibilities struct {
	bytes int
	work  int
}

const maxCompilerSourcePossibilities = 128

func (p *compilerSourcePossibilities) charge(value string) bool {
	p.work++
	p.bytes += len(value)
	return p.work <= 8192 && p.bytes <= MaxProbeInterpolatedBytes
}

func (p *compilerSourcePossibilities) product(left, right []string) ([]string, bool) {
	seen := map[string]bool{}
	result := []string{}
	for _, a := range left {
		for _, b := range right {
			if len(a) > MaxProbeInterpolatedBytes-len(b) {
				return nil, false
			}
			value := a + b
			if !p.charge(value) {
				return nil, false
			}
			if !seen[value] {
				seen[value] = true
				result = append(result, value)
				if len(result) > maxCompilerSourcePossibilities {
					return nil, false
				}
			}
		}
	}
	return result, true
}

func (p *compilerSourcePossibilities) fragments(fragments []ProbeValueFragment, depth int) ([]string, bool) {
	if depth > MaxProbeValueFragmentDepth {
		return nil, false
	}
	result := []string{""}
	for _, fragment := range fragments {
		if !p.charge(fragment.Value) || strings.Contains(fragment.Value, "${") {
			return nil, false
		}
		values := []string{fragment.Value}
		if len(fragment.Fragments) != 0 {
			if fragment.Value != "" {
				return nil, false
			}
			var ok bool
			values, ok = p.fragments(fragment.Fragments, depth+1)
			if !ok {
				return nil, false
			}
		}
		for _, transform := range fragment.Transforms {
			// Dynamic transform operands need their own cross product. Keep the
			// conservative path until that extra language is explicitly modeled.
			if len(transform.ArgumentFragments) != 0 {
				return nil, false
			}
			for _, argument := range transform.Arguments {
				if strings.Contains(argument, "${") || !p.charge(argument) {
					return nil, false
				}
			}
			for i, value := range values {
				transformed, err := ApplyProbeValueTransform(transform, value)
				if err != nil || !p.charge(transformed) {
					return nil, false
				}
				values[i] = transformed
			}
		}
		if fragment.When != nil && !slices.Contains(values, "") {
			values = append(values, "")
		}
		var ok bool
		result, ok = p.product(result, values)
		if !ok {
			return nil, false
		}
	}
	return result, true
}

func possibleCompilerSourceWords(base []string, conditional []ProbeConditionalArguments, fragments []ProbeArgumentFragments) (map[string]bool, bool) {
	words := map[string]bool{}
	addLiteral := func(word string) bool {
		if strings.Contains(word, "${") || validateProbeToken(word) != nil {
			return false
		}
		words[word] = true
		return true
	}
	groups := map[int]bool{}
	budget := compilerSourcePossibilities{}
	for _, group := range fragments {
		groups[group.Index] = true
		values, ok := budget.fragments(group.Fragments, 0)
		if !ok {
			return nil, false
		}
		for _, value := range values {
			var rendered []string
			switch group.Mode {
			case "":
				rendered = strings.Fields(value)
			case ProbeArgumentFragmentsModeSignedDecimal:
				rendered = []string{value}
			case ProbeArgumentFragmentsModeSourceShellWords:
				var err error
				rendered, err = ParseProbeSourceShellWords(value)
				if err != nil {
					return nil, false
				}
			default:
				return nil, false
			}
			for _, word := range rendered {
				if !addLiteral(word) {
					return nil, false
				}
			}
		}
	}
	for i, word := range base {
		if !groups[i] && !addLiteral(word) {
			return nil, false
		}
	}
	for _, group := range conditional {
		for _, word := range group.Arguments {
			if !addLiteral(word) {
				return nil, false
			}
		}
	}
	return words, true
}

func (s *KbuildProbeScopes) compilerSourceCandidates(scope, role string, arguments, candidates []string) []string {
	if len(candidates) == 0 || s == nil || s.evaluators[scope] == nil || (role != "cc" && role != "cxx") {
		return candidates
	}
	lowerer := newProbeSymbolicValueLowerer(s.evaluators[scope])
	base, conditional, fragments, _, err := lowerProbeCandidateArguments(
		lowerer, arguments, probeCandidateArgumentMask(len(arguments)), ProbeCandidatePolicyCC,
	)
	if err != nil {
		return candidates
	}
	var words map[string]bool
	var enumerated, enumerationComplete bool
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		possible, complete := possibleCompilerSourceWord(base, conditional, fragments, candidate)
		if !complete {
			// The word automaton avoids exponential optional-flag products.
			// Keep the bounded exact renderer for syntax it cannot model.
			if !enumerated {
				words, enumerationComplete = possibleCompilerSourceWords(base, conditional, fragments)
				enumerated = true
			}
			possible = !enumerationComplete || words[candidate]
		}
		if possible {
			result = append(result, candidate)
		}
	}
	return result
}
