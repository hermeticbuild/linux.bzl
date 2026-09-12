package kconfig

import (
	"bytes"
	"iter"
	"strings"
	"unicode"
)

const linuxProbeSymbolDigestLength = 64

// linuxProbeSymbolMatcher recognizes the fixed-width tokens carried through
// symbolic Kconfig and Kbuild evaluation. These tokens occur in very large Make
// expansions, where the general regexp VM dominated planner CPU. The scanner is
// deliberately API-compatible with the small regexp surface used by this
// package; replacement keeps using regexp so Go's replacement-string semantics
// remain exact on the uncommon rewrite paths.
type linuxProbeSymbolMatcher struct{}

// HasGuaranteedNonWhitespace recognizes a literal rune that cannot be consumed
// by any symbolic substitution. Checking only gaps between today's tokens is
// insufficient: replacements may form another token across a fragment boundary.
// A non-space rune outside the token alphabet survives even that recursive
// case. False is deliberately inconclusive, not evidence of an empty result.
func (linuxProbeSymbolMatcher) HasGuaranteedNonWhitespace(value string) bool {
	for _, character := range value {
		if !unicode.IsSpace(character) &&
			!strings.ContainsRune(linuxProbeSymbolPrefix, character) &&
			!(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return true
		}
	}
	return false
}

func linuxProbeSymbolDigestByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f'
}

func (linuxProbeSymbolMatcher) nextString(value string, offset int) (int, int, bool) {
	width := len(linuxProbeSymbolPrefix) + linuxProbeSymbolDigestLength
	for offset+width <= len(value) {
		relative := strings.Index(value[offset:], linuxProbeSymbolPrefix)
		if relative < 0 {
			return 0, 0, false
		}
		start := offset + relative
		end := start + width
		if end <= len(value) {
			valid := true
			for index := start + len(linuxProbeSymbolPrefix); index < end; index++ {
				if !linuxProbeSymbolDigestByte(value[index]) {
					valid = false
					break
				}
			}
			if valid {
				return start, end, true
			}
		}
		// Advance one byte so a later prefix is found even when malformed input
		// embeds it inside the rejected candidate.
		offset = start + 1
	}
	return 0, 0, false
}

func (linuxProbeSymbolMatcher) nextBytes(value []byte, offset int) (int, int, bool) {
	width := len(linuxProbeSymbolPrefix) + linuxProbeSymbolDigestLength
	prefix := []byte(linuxProbeSymbolPrefix)
	for offset+width <= len(value) {
		relative := bytes.Index(value[offset:], prefix)
		if relative < 0 {
			return 0, 0, false
		}
		start := offset + relative
		end := start + width
		if end <= len(value) {
			valid := true
			for index := start + len(prefix); index < end; index++ {
				if !linuxProbeSymbolDigestByte(value[index]) {
					valid = false
					break
				}
			}
			if valid {
				return start, end, true
			}
		}
		offset = start + 1
	}
	return 0, 0, false
}

func (matcher linuxProbeSymbolMatcher) MatchString(value string) bool {
	_, _, ok := matcher.nextString(value, 0)
	return ok
}

func (matcher linuxProbeSymbolMatcher) Match(value []byte) bool {
	_, _, ok := matcher.nextBytes(value, 0)
	return ok
}

func (matcher linuxProbeSymbolMatcher) Find(value []byte) []byte {
	start, end, ok := matcher.nextBytes(value, 0)
	if !ok {
		return nil
	}
	return value[start:end]
}

func (matcher linuxProbeSymbolMatcher) FindAllString(value string, limit int) []string {
	if limit == 0 {
		return nil
	}
	var matches []string
	for offset := 0; offset <= len(value); {
		start, end, ok := matcher.nextString(value, offset)
		if !ok {
			break
		}
		matches = append(matches, value[start:end])
		if limit > 0 && len(matches) == limit {
			break
		}
		offset = end
	}
	return matches
}

// AllString returns a sequence over the non-overlapping symbolic tokens in
// value. Unlike FindAllString it does not allocate a result slice, and ranging
// may stop early without scanning the remainder of value.
func (matcher linuxProbeSymbolMatcher) AllString(value string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for offset := 0; offset <= len(value); {
			start, end, ok := matcher.nextString(value, offset)
			if !ok || !yield(value[start:end]) {
				return
			}
			offset = end
		}
	}
}

func (matcher linuxProbeSymbolMatcher) FindAllStringIndex(value string, limit int) [][]int {
	if limit == 0 {
		return nil
	}
	var matches [][]int
	for offset := 0; offset <= len(value); {
		start, end, ok := matcher.nextString(value, offset)
		if !ok {
			break
		}
		matches = append(matches, []int{start, end})
		if limit > 0 && len(matches) == limit {
			break
		}
		offset = end
	}
	return matches
}

// AllStringIndex returns a sequence over the non-overlapping symbolic token
// ranges in value without allocating per-match index slices.
func (matcher linuxProbeSymbolMatcher) AllStringIndex(value string) iter.Seq2[int, int] {
	return func(yield func(int, int) bool) {
		for offset := 0; offset <= len(value); {
			start, end, ok := matcher.nextString(value, offset)
			if !ok || !yield(start, end) {
				return
			}
			offset = end
		}
	}
}

func (linuxProbeSymbolMatcher) ReplaceAllString(value, replacement string) string {
	return linuxProbeSymbolRegexp.ReplaceAllString(value, replacement)
}

func (linuxProbeSymbolMatcher) ReplaceAllStringFunc(value string, replace func(string) string) string {
	return linuxProbeSymbolRegexp.ReplaceAllStringFunc(value, replace)
}

var _ interface {
	MatchString(string) bool
	Match([]byte) bool
	Find([]byte) []byte
	FindAllString(string, int) []string
	FindAllStringIndex(string, int) [][]int
	AllString(string) iter.Seq[string]
	AllStringIndex(string) iter.Seq2[int, int]
	ReplaceAllString(string, string) string
	ReplaceAllStringFunc(string, func(string) string) string
} = linuxProbeSymbolMatcher{}
