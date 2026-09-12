package kconfig

import (
	"slices"
	"strings"
)

const (
	maxCompilerSourceWordLength = 4096
	maxCompilerSourceWordWork   = 1 << 20
)

// This automaton recognizes one complete unquoted word across a language of
// independently optional fragments. It retains prefix states, not complete
// strings, so thirty conditional flags do not require a billion renderings.
// A positive or unknown result retains the candidate; only absence removes it.
type compilerSourceWordMachine struct {
	word             string
	work             int
	sourceShellWords bool
}

func (m *compilerSourceWordMachine) charge() bool {
	m.work++
	return m.work <= maxCompilerSourceWordWork
}

func plainCompilerSourceWordByte(c byte) bool {
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
		return true
	}
	if c < 0x20 || c > 0x7e {
		return false
	}
	switch c {
	case '\'', '"', '\\', '`', '$', ';', '|', '&', '<', '>', '(', ')', '{', '}', '*', '?', '[', ']', '#', '~', '!':
		return false
	}
	return true
}

func (m *compilerSourceWordMachine) literal(states []int, value string) ([]int, bool) {
	if strings.ContainsRune(value, '\x01') {
		if !m.sourceShellWords || len(value) > maxCompilerSourceWordWork-m.work {
			return nil, false
		}
		m.work += len(value)
		// The source-shell parser materializes authenticated private roots before
		// lexing. Only do that leaf-locally when no root can participate in path
		// joining: a leading marker may join the previous leaf, and a slash
		// before a marker may collapse an outer or embedded object root. Such
		// cases still require the complete renderer, as do partial/unknown markers.
		for _, marker := range []string{compactKbuildActionSourceTreeMarker, compactKbuildActionObjectTreeMarker,
			compactKbuildActionAbsoluteObjectTreeMarker, compactKbuildActionHostDepsTreeMarker} {
			if strings.HasPrefix(value, marker) || strings.Contains(value, "/"+marker) {
				return nil, false
			}
		}
		value = compactKbuildMaterializeActionTreeMarkers(value)
	}
	dead, found := len(m.word)+1, len(m.word)+2
	current := slices.Clone(states)
	next := make([]int, 0, found+1)
	seen := make([]bool, found+1)
	for i := range len(value) {
		c := value[i]
		if !m.charge() || !plainCompilerSourceWordByte(c) {
			return nil, false
		}
		for _, state := range current {
			if !m.charge() {
				return nil, false
			}
			n := dead
			switch {
			case state == found:
				n = found
			case c == ' ' || c == '\t' || c == '\r' || c == '\n':
				n = 0
				if state == len(m.word) {
					n = found
				}
			case state < len(m.word) && m.word[state] == c:
				n = state + 1
			}
			if !seen[n] {
				seen[n] = true
				next = append(next, n)
			}
		}
		for _, state := range next {
			seen[state] = false
		}
		current, next = next, current[:0]
	}
	return current, true
}

func (m *compilerSourceWordMachine) sequence(fragments []ProbeValueFragment, states []int, depth int) ([]int, bool) {
	if depth > MaxProbeValueFragmentDepth {
		return nil, false
	}
	for _, fragment := range fragments {
		if !m.charge() || len(fragment.Transforms) != 0 {
			return nil, false
		}
		before := states
		var complete bool
		if len(fragment.Fragments) != 0 {
			if fragment.Value != "" {
				return nil, false
			}
			states, complete = m.sequence(fragment.Fragments, states, depth+1)
		} else {
			states, complete = m.literal(states, fragment.Value)
		}
		if !complete {
			return nil, false
		}
		if fragment.When != nil {
			seen := make([]bool, len(m.word)+3)
			for _, state := range states {
				seen[state] = true
			}
			for _, state := range before {
				if !m.charge() {
					return nil, false
				}
				if !seen[state] {
					seen[state] = true
					states = append(states, state)
				}
			}
		}
	}
	return states, true
}

func compilerSourceWordGroupWrapper(fragment ProbeValueFragment) bool {
	if fragment.Value != "" || len(fragment.Fragments) == 0 {
		return false
	}
	for _, transform := range fragment.Transforms {
		if transform.Function != "strip" || transform.InputArgument != 0 ||
			len(transform.Arguments) != 1 || transform.Arguments[0] != "" || len(transform.ArgumentFragments) != 0 {
			return false
		}
	}
	return true
}

func (m *compilerSourceWordMachine) group(group ProbeArgumentFragments) (bool, bool) {
	if group.Mode != "" && group.Mode != ProbeArgumentFragmentsModeSourceShellWords {
		return true, false
	}
	m.sourceShellWords = group.Mode == ProbeArgumentFragmentsModeSourceShellWords
	fragments := group.Fragments
	depth := 0
	// Only a whole-group strip preserves word boundaries. An interior strip
	// can join adjacent pieces ("a" + strip(" b ") + "c") and stays unknown.
	// A conditional whole group additionally permits no words, never a new word.
	for len(fragments) == 1 && compilerSourceWordGroupWrapper(fragments[0]) {
		if !m.charge() || depth >= MaxProbeValueFragmentDepth {
			return true, false
		}
		fragments = fragments[0].Fragments
		depth++
	}
	states, complete := m.sequence(fragments, []int{0}, depth)
	if !complete {
		return true, false
	}
	return slices.Contains(states, len(m.word)) || slices.Contains(states, len(m.word)+2), true
}

func possibleCompilerSourceWord(base []string, conditional []ProbeConditionalArguments, fragments []ProbeArgumentFragments, candidate string) (bool, bool) {
	if candidate == "" || len(candidate) > maxCompilerSourceWordLength {
		return true, false
	}
	machine := compilerSourceWordMachine{word: candidate}
	groups := make(map[int]bool, len(fragments))
	for _, group := range fragments {
		groups[group.Index] = true
		possible, complete := machine.group(group)
		if possible || !complete {
			return possible, complete
		}
	}
	// Literal argv is already split by the authenticated lowerer. It is not
	// shell text: an exact scalar payload must retain its candidate too, leaving
	// positional ownership to the existing compiler-argument parser.
	check := func(word string) (bool, bool) {
		if !machine.charge() || validateProbeToken(word) != nil {
			return true, false
		}
		for i := range len(word) {
			if !machine.charge() || word[i] == '$' {
				return true, false
			}
		}
		return word == candidate, true
	}
	for i, word := range base {
		if !groups[i] {
			if possible, complete := check(word); possible || !complete {
				return possible, complete
			}
		}
	}
	for _, group := range conditional {
		for _, word := range group.Arguments {
			if possible, complete := check(word); possible || !complete {
				return possible, complete
			}
		}
	}
	return false, true
}
