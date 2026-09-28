package kconfig

import (
	"fmt"
	"strings"
)

// ValidateProbeSourceShellLiteral validates one planner-owned leaf before
// interpolation or Make transforms. Marker framing must be complete inside
// that leaf: a measured value cannot supply the middle of a literal marker.
// This validates a copy and never changes the bytes seen by Make operations.
func ValidateProbeSourceShellLiteral(value string) error {
	value = compactKbuildMaterializeActionTreeMarkers(value)
	value, err := RestoreCompactKbuildLiteralActionMarkers(value)
	if err != nil {
		return fmt.Errorf("source-shell literal marker: %w", err)
	}
	for index := range len(value) {
		character := value[index]
		if (character < ' ' && character != '\t' && character != '\n') || character == 127 {
			return fmt.Errorf("source-shell literal contains an incomplete marker or unsupported control byte")
		}
	}
	return nil
}

// ParseProbeSourceShellWords lowers a completed, explicitly source-recipe-owned
// Make value to compiler argv. The caller must reject private marker bytes in
// measured-result substitutions BEFORE evaluating the fragment graph. Only
// authenticated planner literals may contribute the markers consumed here.
//
// Root finalization follows all Make transforms: even filter-out can distinguish
// the private and public spelling. Protected source literals stay protected
// through lexing and are restored only in final argv, without interpolation.
// This function never invokes a shell or performs shell expansion.
func ParseProbeSourceShellWords(value string) ([]string, error) {
	if len(value) > MaxProbeInterpolatedBytes {
		return nil, fmt.Errorf("source-shell argument text exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	value = compactKbuildCollapseActionRootJoins(value)
	value = compactKbuildCollapseEmbeddedActionObjectRoots(value)
	value = compactKbuildMaterializeActionTreeMarkers(value)
	// Validate protected literal framing without exposing it to lexing as
	// fresh placeholder or expansion syntax.
	if _, err := RestoreCompactKbuildLiteralActionMarkers(value); err != nil {
		return nil, fmt.Errorf("source-shell literal marker: %w", err)
	}
	for index := range len(value) {
		character := value[index]
		if (character < ' ' && character != '\t' && character != '\n' &&
			character != compactKbuildLiteralTreeEscapeByte[0] && character != compactKbuildLiteralSentinelEscapeByte[0]) || character == 127 {
			return nil, fmt.Errorf("source-shell argument text contains an unsupported control byte")
		}
	}
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, fmt.Errorf("source-shell argument words: %w", err)
	}
	if len(tokens) > MaxProbeDynamicArgumentWords {
		return nil, fmt.Errorf("source-shell arguments exceed %d words", MaxProbeDynamicArgumentWords)
	}
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.operator || token.pathnameExpansion || token.shellExpansion {
			return nil, fmt.Errorf("source-shell argument text requires shell execution or expansion")
		}
		if token.start < 0 || token.end < token.start || token.end > len(value) {
			return nil, fmt.Errorf("source-shell argument token has invalid source extent")
		}
		if err := validateActionRecipeStaticShellWord(value[token.start:token.end]); err != nil {
			return nil, fmt.Errorf("source-shell argument quoting: %w", err)
		}
		word := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
		word, err = RestoreCompactKbuildLiteralActionMarkers(word)
		if err != nil {
			return nil, fmt.Errorf("source-shell argument literal: %w", err)
		}
		for index := range len(word) {
			if word[index] < ' ' || word[index] == 127 {
				return nil, fmt.Errorf("source-shell argument retains a control byte")
			}
		}
		words = append(words, word)
	}
	return words, nil
}
