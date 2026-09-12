package kconfig

import (
	"slices"
	"strings"
)

// Guard hints select names to ask the configured compiler about. They are not
// evidence of definedness, absence, or complete conditional coverage. In
// particular, unsupported source syntax may omit hints without proving anything
// about the omitted names.
type configDependencyCompilerGuardHints struct {
	names     []string
	calls     []CompilerIntrinsicCall
	truncated bool
	// Only a typed, actually reached expansion demand may opt into a
	// nonfatal compiler attempt. Static source inventories leave this false.
	optionalDefinedness bool
	optionalTokenHints  bool
	literalIncludeHints bool
}

const (
	configDependencyCompilerGuardHintsMaximumBytes     = 1 << 20
	configDependencyCompilerGuardHintsMaximumNames     = 4096
	configDependencyCompilerGuardHintsMaximumNameBytes = 256 << 10
)

// configDependencyCompilerGuardHintsForContents reads complete immutable file
// bytes, never a prefix or compiler-preprocessed output. Exceeding an inventory
// budget discards the entire result rather than returning a partial inventory.
// The caller must separately bind each queried name to an actual compiler
// result before using it for dependency analysis.
func configDependencyCompilerGuardHintsForContents(contents []byte) configDependencyCompilerGuardHints {
	if len(contents) > configDependencyCompilerGuardHintsMaximumBytes {
		return configDependencyCompilerGuardHints{truncated: true}
	}
	text, reason := configDependencyPreprocessorText(contents)
	if reason != "" {
		return configDependencyCompilerGuardHints{}
	}
	names := map[string]bool{}
	nameBytes := 0
	add := func(name string) bool {
		if !strings.HasPrefix(name, "_") || names[name] {
			return true
		}
		if len(names) == configDependencyCompilerGuardHintsMaximumNames ||
			len(name) > configDependencyCompilerGuardHintsMaximumNameBytes-nameBytes {
			return false
		}
		// A short retained name must not keep the complete file alive.
		names[strings.Clone(name)] = true
		nameBytes += len(name)
		return true
	}
	for original := range strings.SplitSeq(text, "\n") {
		for _, name := range configDependencyCompilerDirectiveDefinedHints(original) {
			if !add(name) {
				return configDependencyCompilerGuardHints{truncated: true}
			}
		}
	}
	result := configDependencyCompilerGuardHints{}
	for name := range names {
		result.names = append(result.names, name)
	}
	slices.Sort(result.names)
	return result
}

// Both the initial bounded inventory and entered-file discovery use the same
// syntax. Call only after comment removal and line splicing. These are names
// to query, never facts about the current macro state or reached source.
func configDependencyCompilerDirectiveDefinedHints(line string) []string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "%:") {
		line = "#" + line[2:]
	}
	if !strings.HasPrefix(line, "#") {
		return nil
	}
	line = strings.TrimSpace(line[1:])
	end := 0
	for end < len(line) && configDependencyIdentifierByte(line[end]) {
		end++
	}
	directive, rest := line[:end], strings.TrimSpace(line[end:])
	switch directive {
	case "ifdef", "ifndef":
		name, valid := configDependencyMacroIdentifier(rest)
		if valid && name == rest {
			return []string{name}
		}
	case "if", "elif":
		return configDependencyCompilerLiteralDefinedHints(rest)
	}
	return nil
}

// This recognizes literal defined operands, not macro-expanded expressions.
// The bounded lexer protects literals and rejects dialect-dependent spellings;
// an unsupported expression contributes no hints. Malformed defined operands
// discard this expression's hints, including any valid prefix.
func configDependencyCompilerLiteralDefinedHints(text string) []string {
	tokens, err := configDependencyMacroCallLex(text, configDependencyMacroCallMode{})
	if err != nil {
		return nil
	}
	var names []string
	depth := 0
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.text != "defined" {
			switch token.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return nil
				}
			}
			continue
		}
		index++
		parenthesized := index < len(tokens) && tokens[index].text == "("
		if parenthesized {
			index++
		}
		if index >= len(tokens) || !tokens[index].identifier {
			return nil
		}
		name := tokens[index].text
		if parenthesized {
			index++
			if index >= len(tokens) || tokens[index].text != ")" {
				return nil
			}
		}
		if strings.HasPrefix(name, "_") {
			names = append(names, name)
		}
	}
	if depth != 0 {
		return nil
	}
	return names
}
