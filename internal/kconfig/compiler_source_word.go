package kconfig

import (
	"slices"
	"strings"
)

const (
	maxCompilerSourceWordLength = 4096
	maxCompilerSourceWordWork   = 1 << 20
	compilerWordQuoteModes      = 5
	compilerWordUnquoted        = 0
	compilerWordSingleQuoted    = 1
	compilerWordDoubleQuoted    = 2
	compilerWordUnquotedEscape  = 3
	compilerWordDoubleEscape    = 4
)

// This automaton recognizes one complete word across a language of independently
// optional fragments, retaining static source-shell quote/escape state. It keeps
// prefix states, not complete strings, so thirty conditional flags do not require
// a billion renderings. Unsupported shell or Make transformations stay unknown.
// A positive or unknown result retains the candidate; only absence removes it.
type compilerSourceWordMachine struct {
	word             string
	work             int
	sourceShellWords bool
	strippedGroup    bool
}

// States combine a word-prefix state with the shell quoting state. Retaining
// both across optional leaves avoids enumerating complete compiler argv strings.
func (m *compilerSourceWordMachine) advance(state int, c byte) (int, bool) {
	word, quote := state/compilerWordQuoteModes, state%compilerWordQuoteModes
	dead, found := len(m.word)+1, len(m.word)+2
	emit := func(c byte) {
		if word == found {
			return
		}
		if word < len(m.word) && m.word[word] == c {
			word++
		} else {
			word = dead
		}
	}
	if !m.sourceShellWords {
		if !plainCompilerSourceWordByte(c) {
			return 0, false
		}
	} else {
		if c < ' ' || c > 0x7e {
			// Outside quotes, tabs delimit words. Escaped newlines disappear.
			if c == '\n' && quote == compilerWordUnquotedEscape {
				if m.strippedGroup {
					return 0, false
				}
				quote = compilerWordUnquoted
				return word*compilerWordQuoteModes + quote, true
			}
			if c != '\t' || quote != compilerWordUnquoted {
				return 0, false
			}
		}
		// Expansion-bearing spellings stay on the complete renderer. In a
		// stripped Make group, quoted/escaped whitespace can change the word
		// itself, so stripping cannot be treated as a word-boundary identity.
		if c == '$' || c == '`' || m.strippedGroup && c == ' ' && quote != compilerWordUnquoted {
			return 0, false
		}
		switch quote {
		case compilerWordSingleQuoted:
			if c == '\'' {
				quote = compilerWordUnquoted
			} else {
				emit(c)
			}
			return word*compilerWordQuoteModes + quote, true
		case compilerWordDoubleQuoted:
			switch c {
			case '"':
				quote = compilerWordUnquoted
			case '\\':
				quote = compilerWordDoubleEscape
			default:
				emit(c)
			}
			return word*compilerWordQuoteModes + quote, true
		case compilerWordUnquotedEscape:
			emit(c)
			return word * compilerWordQuoteModes, true
		case compilerWordDoubleEscape:
			if c != '"' && c != '\\' {
				return 0, false
			}
			emit(c)
			return word*compilerWordQuoteModes + compilerWordDoubleQuoted, true
		}
		switch c {
		case '\'':
			return word*compilerWordQuoteModes + compilerWordSingleQuoted, true
		case '"':
			return word*compilerWordQuoteModes + compilerWordDoubleQuoted, true
		case '\\':
			return word*compilerWordQuoteModes + compilerWordUnquotedEscape, true
		}
		if !plainCompilerSourceWordByte(c) {
			return 0, false
		}
	}
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
		if word == len(m.word) || word == found {
			word = found
		} else {
			word = 0
		}
	} else {
		emit(c)
	}
	return word * compilerWordQuoteModes, true
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
	current := slices.Clone(states)
	next := make([]int, 0, (len(m.word)+3)*compilerWordQuoteModes)
	seen := make([]bool, (len(m.word)+3)*compilerWordQuoteModes)
	for i := range len(value) {
		c := value[i]
		if !m.charge() {
			return nil, false
		}
		for _, state := range current {
			if !m.charge() {
				return nil, false
			}
			n, complete := m.advance(state, c)
			if !complete {
				return nil, false
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
			seen := make([]bool, (len(m.word)+3)*compilerWordQuoteModes)
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
	m.strippedGroup = false
	fragments := group.Fragments
	depth := 0
	// Only a whole-group strip preserves word boundaries. An interior strip
	// can join adjacent pieces ("a" + strip(" b ") + "c") and stays unknown.
	// A conditional whole group additionally permits no words, never a new word.
	for len(fragments) == 1 && compilerSourceWordGroupWrapper(fragments[0]) {
		if !m.charge() || depth >= MaxProbeValueFragmentDepth {
			return true, false
		}
		m.strippedGroup = m.strippedGroup || len(fragments[0].Transforms) != 0
		fragments = fragments[0].Fragments
		depth++
	}
	states, complete := m.sequence(fragments, []int{0}, depth)
	if !complete {
		return true, false
	}
	possible := false
	for _, state := range states {
		if state%compilerWordQuoteModes != compilerWordUnquoted {
			return true, false
		}
		word := state / compilerWordQuoteModes
		possible = possible || word == len(m.word) || word == len(m.word)+2
	}
	return possible, true
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
