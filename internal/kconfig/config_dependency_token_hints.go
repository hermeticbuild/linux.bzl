package kconfig

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// Work counts are retained with cached inventories so cache hits cannot evade
// the TU-wide scan-token budget. They are not a CPU-time or heap measurement.
type configDependencyOpenedHeaderHintInventory struct {
	hints configDependencyCompilerGuardHints
	work  int
}

type configDependencyOpenedHeaderHintBudget struct {
	work, nameBytes int
	names           map[string]bool
	disabled        bool
}

func (b *configDependencyOpenedHeaderHintBudget) add(inventory configDependencyOpenedHeaderHintInventory) ([]string, bool) {
	if b.disabled {
		return nil, false
	}
	truncate := func() ([]string, bool) {
		b.disabled = true
		b.names = nil
		return nil, true
	}
	if inventory.work < len(inventory.hints.names) || inventory.hints.truncated || inventory.work > 65536-b.work {
		return truncate()
	}
	b.work += inventory.work
	// Preflight this complete file observation before retaining any new names.
	// On failure the caller stops selection, retaining earlier complete files.
	// No partial file inventory may escape; other consumers and the independent
	// baseline and actual-demand tiers remain intact.
	newNames := map[string]bool{}
	newBytes := 0
	for _, name := range inventory.hints.names {
		if b.names[name] || newNames[name] {
			continue
		}
		if len(b.names)+len(newNames) >= configDependencyCompilerGuardHintsMaximumNames ||
			len(name) > configDependencyCompilerGuardHintsMaximumNameBytes-b.nameBytes-newBytes {
			return truncate()
		}
		newNames[name] = true
		newBytes += len(name)
	}
	if b.names == nil {
		b.names = map[string]bool{}
	}
	result := make([]string, 0, len(newNames))
	for name := range newNames {
		name = strings.Clone(name)
		b.names[name] = true
		result = append(result, name)
	}
	b.nameBytes += newBytes
	slices.Sort(result)
	return result, false
}

func configDependencyHintLexicalBudget(err error) bool {
	// These are private, fixed errors from the existing bounded lexer/parser,
	// not compiler diagnostics or text read from a probe result.
	return err != nil && (err.Error() == "lexical byte budget" || err.Error() == "lexical token budget" || err.Error() == "formal budget")
}

// Reuse the cached complete conditional program rather than parsing the same
// header again just to collect weak names. These are optional compiler-query
// names, NOT statements about expansion or reads. The caller must retain the
// authenticated file origin and freshly replay measured compiler answers
// before granting precision.
func configDependencyOpenedHeaderTokenHintInventoryForProgram(sourceBytes int, program configDependencyConditionalProgram) configDependencyOpenedHeaderHintInventory {
	return configDependencyTokenHintInventoryForProgram(sourceBytes, program, false)
}

func configDependencyTokenHintInventoryForProgram(sourceBytes int, program configDependencyConditionalProgram, directiveOperands bool) configDependencyOpenedHeaderHintInventory {
	result := configDependencyOpenedHeaderHintInventory{}
	finish := func(hints configDependencyCompilerGuardHints) configDependencyOpenedHeaderHintInventory {
		result.hints = hints
		return result
	}
	truncate := func() configDependencyOpenedHeaderHintInventory {
		return finish(configDependencyCompilerGuardHints{truncated: true, optionalTokenHints: true})
	}
	charge := func(work int) bool {
		if work < 0 || work > 65536-result.work {
			return false
		}
		result.work += work
		return true
	}
	if sourceBytes < 0 || sourceBytes > configDependencyCompilerGuardHintsMaximumBytes {
		return truncate()
	}
	if program.reason != "" || !program.callCoverage {
		return finish(configDependencyCompilerGuardHints{})
	}
	names := map[string]bool{}
	nameBytes := 0
	for _, entry := range program.directives {
		var tokens []configDependencyMacroCallToken
		formals := map[string]bool{}
		switch {
		case entry.text != "":
			var err error
			tokens, err = configDependencyMacroCallLex(entry.text, configDependencyMacroCallMode{})
			if err != nil {
				// The lexer stops after at most 4097 emitted tokens. Failed
				// inventory work is charged even though no names are retained.
				if !charge(min(len(entry.text), 4097)) || configDependencyHintLexicalBudget(err) {
					return truncate()
				}
				// Unsupported lexical syntax supplies no inventory; no prefix is
				// promoted into an apparently complete file inventory.
				return finish(configDependencyCompilerGuardHints{})
			}
		case entry.directive == "define":
			_, definition, err := configDependencyMacroCallParse(entry.rest, configDependencyMacroCallMode{})
			if err != nil {
				// Byte length conservatively bounds tokens processed across
				// all formal and replacement lexes in a failed definition.
				if !charge(len(entry.rest)) || configDependencyHintLexicalBudget(err) {
					return truncate()
				}
				// Do not query raw formal/variadic/stringification tokens from
				// unsupported definitions. Such a definition supplies no hints;
				// complete expansion remains the existing proof engine's job.
				continue
			}
			for _, formal := range definition.formals {
				formals[formal] = true
			}
			if definition.variadic != "" {
				formals[definition.variadic] = true
			}
			if !charge(len(formals)) {
				return truncate()
			}
			tokens = definition.replacement
		case directiveOperands && (entry.directive == "if" || entry.directive == "elif" ||
			entry.directive == "ifdef" || entry.directive == "ifndef" || entry.directive == "include" ||
			entry.directive == "include_next" || entry.directive == "import"):
			var err error
			tokens, err = configDependencyMacroCallLex(entry.rest, configDependencyMacroCallMode{})
			if err != nil {
				return truncate()
			}
		default:
			// Literal conditional queries retain their existing higher-priority
			// observer. Include operands do not authorize opening another file.
			continue
		}
		if !charge(len(tokens)) {
			return truncate()
		}
		for _, token := range tokens {
			name := token.text
			if !token.identifier || !strings.HasPrefix(name, "_") || formals[name] || names[name] {
				continue
			}
			if len(names) >= configDependencyCompilerGuardHintsMaximumNames ||
				len(name) > configDependencyCompilerGuardHintsMaximumNameBytes-nameBytes {
				return truncate()
			}
			names[strings.Clone(name)] = true
			nameBytes += len(name)
		}
	}
	return finish(configDependencyCompilerGuardHints{
		names: slices.Sorted(maps.Keys(names)), optionalTokenHints: true,
	})
}

// The conditional-syntax cache retains exact source/generated-owner identity.
// Nothing in this observer invokes a resolver or mutates a macro snapshot.
func (s *configDependencyClosureScanner) emitCompilerOpenedHeaderTokenHints(
	probe configDependencyCompilerPredefineProbe,
	observe func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string),
) {
	if !s.collectCompilerGuards || observe == nil || s.callCoverage == nil || s.compilerIntrinsicInitialSnapshot == nil || probe.language == "" {
		return
	}
	budget := configDependencyOpenedHeaderHintBudget{}
	// Select complete file inventories within the unchanged TU-wide limits.
	// An unusable file supplies no hints, but cannot invalidate another file.
	// Prefer inexpensive inventories, with exact file identity breaking ties;
	// map iteration order and cache hits must not affect admission.
	type candidateHint struct {
		identity  string
		file      configDependencyScanFile
		inventory configDependencyOpenedHeaderHintInventory
		contentID string
	}
	var candidates []candidateHint
	for identity, file := range s.compilerGuardFiles {
		syntax, found := s.conditionalSyntax[s.conditionalSyntaxCacheKey(file)]
		if !found || !syntax.compilerTokenInventoryReady || syntax.compilerGuardContentID == "" {
			continue
		}
		inventory := syntax.compilerTokenInventory
		// Validate original work before filtering already-known names. Cached
		// or truncated inventories cannot evade the original per-file bounds.
		if inventory.hints.truncated || inventory.work < len(inventory.hints.names) || inventory.work > 65536 {
			continue
		}
		candidates = append(candidates, candidateHint{identity, file, inventory, syntax.compilerGuardContentID})
	}
	slices.SortFunc(candidates, func(a, b candidateHint) int {
		if order := cmp.Compare(a.inventory.work, b.inventory.work); order != 0 {
			return order
		}
		return strings.Compare(a.identity, b.identity)
	})
	type pendingHint struct {
		file      configDependencyScanFile
		names     []string
		contentID string
	}
	var pending []pendingHint
	for _, candidate := range candidates {
		inventory := candidate.inventory
		var names []string
		for _, name := range inventory.hints.names {
			initial, valid := s.compilerIntrinsicInitialSnapshot.lookup(name)
			if !valid || initial.definition != configDependencyMacroUnknown {
				continue
			}
			if _, measured := s.compilerGuardInitialDefinitions[name]; !measured {
				names = append(names, name)
			}
		}
		// This copy replaces only the per-consumer slice, never cached names.
		// Original inventory work remains charged, including filtered names.
		inventory.hints.names = names
		names, truncated := budget.add(inventory)
		if truncated {
			// This file emits nothing. Previously selected complete files remain
			// valid hints, never proof that omitted names are absent or unread.
			break
		}
		if len(names) != 0 {
			pending = append(pending, pendingHint{file: candidate.file, names: names, contentID: candidate.contentID})
		}
	}
	for _, hint := range pending {
		observe(probe, hint.file, configDependencyCompilerGuardHints{names: hint.names, optionalTokenHints: true}, hint.contentID)
	}
	s.emitCompilerLiteralIncludeHintLookahead(probe, observe, &budget)
}
