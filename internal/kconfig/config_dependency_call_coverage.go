package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// configDependencyCallCoverage is owned by one complete translation unit. No
// prefix escapes on failure. The include scanner owns source/header authority;
// this collector owns the ordered expansion reads and cross-span boundaries.
type configDependencyCallCoverage struct {
	mode        configDependencyMacroCallMode
	reads       map[string]bool
	definitions []configDependencyMacroCallRead
	conditions  []configDependencyMacroCallRead
	intrinsic   func(CompilerIntrinsicCall) (token, identity, reason string)
	unknown     func(string)
	intrinsics  []configDependencyCompilerIntrinsicRead
	priorTail   bool
	work        int
	tokens      int
	complete    bool
	reason      string
}

func (s *configDependencyMacroState) resolveMacroCall(name string) (configDependencyMacroCallBinding, string) {
	if !s.validSnapshotLineage() || s.macroReplacements == nil {
		return configDependencyMacroCallBinding{}, "macro call has no exact current namespace"
	}
	binding := configDependencyMacroCallBinding{state: s.definition(name)}
	if binding.state == configDependencyMacroDefined {
		if value, modeled := s.macroReplacements[name]; modeled {
			if value.intrinsic != nil {
				if value.text != "" || value.origin != "" || !value.intrinsic.valid(name) {
					return configDependencyMacroCallBinding{}, "invalid initial compiler intrinsic replacement"
				}
				binding.intrinsic = value.intrinsic
				return binding, ""
			}
			var reason string
			binding.definition, reason = configDependencyMacroCallDefinitionFromText(value.text, value.origin)
			if reason != "" {
				return configDependencyMacroCallBinding{}, reason
			}
		}
	}
	return binding, ""
}

func (c *configDependencyCallCoverage) fail(reason string) string {
	if c.reason == "" {
		c.reason = reason
	}
	c.reads = nil
	c.definitions = nil
	c.conditions = nil
	c.intrinsics = nil
	c.complete = false
	return c.reason
}

// Observe only bindings actually requested by expansion. Return the unchanged
// unknown binding so the existing first-failure and all-or-nothing proof remain
// intact; a future optional compiler attempt grants no current source facts.
func (c *configDependencyCallCoverage) resolver(state *configDependencyMacroState) configDependencyMacroCallResolver {
	if c.unknown == nil {
		return state.resolveMacroCall
	}
	return func(name string) (configDependencyMacroCallBinding, string) {
		binding, reason := state.resolveMacroCall(name)
		if reason == "" && binding.state == configDependencyMacroUnknown && c.unknown != nil {
			c.unknown(name)
		}
		return binding, reason
	}
}

func (c *configDependencyCallCoverage) expand(text string, state *configDependencyMacroState) string {
	if c.reason != "" {
		return c.reason
	}
	if c.complete {
		return c.fail("call coverage appended after completion")
	}
	raw, err := configDependencyMacroCallLex(text, c.mode)
	if err != nil {
		return c.fail(err.Error())
	}
	if c.priorTail && len(raw) != 0 && raw[0].text == "(" {
		return c.fail("call coverage crosses a preprocessing event invocation boundary")
	}
	mode := c.mode
	mode.intrinsic = c.intrinsic
	result, reason := configDependencyMacroCallExpand(text, c.resolver(state), mode)
	if reason != "" {
		return c.fail(reason)
	}
	if c.priorTail && len(result.Tokens) != 0 && result.Tokens[0] == "(" {
		return c.fail("call coverage crosses an expanded preprocessing event invocation boundary")
	}
	if reason := c.recordExpansion(result); reason != "" {
		return reason
	}
	if len(result.Tokens) != 0 {
		last := result.Tokens[len(result.Tokens)-1]
		c.priorTail = last != "" && configDependencyMacroCallIdentifierStart(last[0])
	}
	return ""
}

func (c *configDependencyCallCoverage) recordExpansion(result configDependencyMacroCallResult) string {
	c.work += result.Work
	c.tokens += len(result.Tokens)
	if c.work > 65536 || c.tokens > 16384 || len(c.conditions)+len(c.definitions)+len(result.DefinitionReads)+len(c.intrinsics)+len(result.IntrinsicReads) > 16384 {
		return c.fail("translation-unit call coverage budget")
	}
	if c.reads == nil {
		c.reads = map[string]bool{}
	}
	for _, name := range result.ConfigReads {
		symbol := normalizeConfigDependencySymbol(name)
		if !isConfigKey(symbol) {
			return c.fail("macro call produced an invalid CONFIG identifier")
		}
		c.reads[symbol] = true
	}
	c.definitions = append(c.definitions, result.DefinitionReads...)
	c.intrinsics = append(c.intrinsics, result.IntrinsicReads...)
	return ""
}

// An active source write can depend on the previous autoconf definition even
// when no expansion reads that name: incompatible redefinitions diagnose and
// can fail under -Werror. Preserve the complete CONFIG value for writes, while
// keeping replacement bodies lazy and raw stringified operands unexpanded.
func (c *configDependencyCallCoverage) macroWrite(name string) string {
	if c.reason != "" {
		return c.reason
	}
	if c.complete {
		return c.fail("macro write appended after call coverage completion")
	}
	if !strings.HasPrefix(name, "CONFIG_") {
		return ""
	}
	symbol := normalizeConfigDependencySymbol(name)
	if !isConfigKey(symbol) {
		return c.fail("macro write produced an invalid CONFIG identifier")
	}
	if c.reads == nil {
		c.reads = map[string]bool{}
	}
	c.reads[symbol] = true
	if len(c.reads) > 16384 {
		return c.fail("translation-unit macro write coverage budget")
	}
	return ""
}

func (c *configDependencyCallCoverage) condition(text string, state *configDependencyMacroState) (configDependencyMacroDefinition, string) {
	if c.reason != "" {
		return configDependencyMacroUnknown, c.reason
	}
	if c.complete || !state.validSnapshotLineage() || state.macroReplacements == nil {
		return configDependencyMacroUnknown, c.fail("conditional call coverage has invalid state")
	}
	tokens, err := configDependencyMacroCallLex(text, c.mode)
	if err != nil {
		return configDependencyMacroUnknown, c.fail(err.Error())
	}
	var protected []configDependencyMacroCallToken
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.text != "defined" {
			protected = append(protected, token)
			continue
		}
		index++
		parenthesized := index < len(tokens) && tokens[index].text == "("
		if parenthesized {
			index++
		}
		if index >= len(tokens) || !tokens[index].identifier {
			return configDependencyMacroUnknown, c.fail("malformed defined operand in conditional call coverage")
		}
		name := tokens[index].text
		if parenthesized {
			index++
			if index >= len(tokens) || tokens[index].text != ")" {
				return configDependencyMacroUnknown, c.fail("unclosed defined operand in conditional call coverage")
			}
		}
		definition := state.definition(name)
		c.conditions = append(c.conditions, configDependencyMacroCallRead{Name: name, State: definition})
		if definition == configDependencyMacroUnknown {
			// defined does not expand its operand, so it bypasses resolver.
			// Record the same future demand without granting a current fact or
			// continuing beyond the first unresolved conditional operand.
			if c.unknown != nil {
				c.unknown(name)
			}
			return configDependencyMacroUnknown, c.fail("unresolved defined operand " + name)
		}
		value := "0"
		if definition == configDependencyMacroDefined {
			value = "1"
		}
		protected = append(protected, configDependencyMacroCallToken{text: value, whitespace: token.whitespace, spacing: token.spacing})
	}
	mode := c.mode
	mode.intrinsic = c.intrinsic
	result, reason := configDependencyMacroCallExpandTokens(protected, c.resolver(state), mode)
	if reason != "" {
		return configDependencyMacroUnknown, c.fail(reason)
	}
	if reason = c.recordExpansion(result); reason != "" {
		return configDependencyMacroUnknown, reason
	}
	undefined := map[string]bool{}
	for _, read := range result.DefinitionReads {
		if read.State == configDependencyMacroUndefined {
			undefined[read.Name] = true
		}
	}
	value, reason := configDependencyConditionalInteger(result.Tokens, func(name string) bool { return undefined[name] })
	if reason != "" {
		return configDependencyMacroUnknown, c.fail(reason)
	}
	return value, ""
}

func configDependencyCallCoverageCandidate(set ConfigDependencySet) bool {
	// A sound lexical projection can still contain raw stringified arguments.
	// Only schedule its refinement when a reachable definition uses #; ordinary
	// cached scans retain their existing cost and discovery behavior. Retain the
	// original set whenever the complete source/namespace proof fails.
	// A graph-reachable pragma can likewise be an unused or stringified
	// argument. This only schedules a complete proof; actual effects still fail.
	return !set.Opaque && set.refineMacroCalls ||
		set.Opaque && (strings.Contains(set.Reason, "through reachable token pasting macro ") ||
			set.Reason == "compiler source closure reaches an unmodeled _Pragma effect")
}

// The shared compiler probe canonicalizes source-selected object-like -D
// bodies to 1. Restore every final -D/-U, including configured prefix/suffix,
// from the actual compiler argv before interpreting forced files. Do not write
// these action-specific values into the immutable shared probe cache.
func (s *configDependencyMacroState) applyCompilerMacroReplacements(tool string, arguments []string) string {
	if !s.validSnapshotLineage() || s.macroReplacements == nil {
		return "compiler replacements require an exact mutable invocation state"
	}
	identity := fmt.Sprintf("compiler-argv:%x", sha256.Sum256([]byte(fmt.Sprintf("%q", arguments))))
	payloads := compactKbuildCompilerSeparatedOptionPayloads(tool, arguments)
	for index := 0; index < len(arguments); index++ {
		if payloads[index] {
			continue
		}
		argument := arguments[index]
		if argument == "--" {
			break
		}
		undef := false
		operand := ""
		switch {
		case argument == "-D" || argument == "-U":
			undef = argument == "-U"
			index++
			if index == len(arguments) || !payloads[index] {
				return "compiler macro option has no exact operand"
			}
			operand = arguments[index]
		case strings.HasPrefix(argument, "-D"):
			operand = argument[2:]
		case strings.HasPrefix(argument, "-U"):
			undef = true
			operand = argument[2:]
		default:
			continue
		}
		if operand == "" || strings.ContainsAny(operand, "\r\n\x00") {
			return "compiler macro option has unsupported logical lines"
		}
		if undef {
			name, valid := configDependencyMacroIdentifier(operand)
			if !valid || name != operand {
				return "compiler -U has an invalid identifier"
			}
			s.set(name, configDependencyMacroUndefined)
			continue
		}
		signature, replacement, explicit := strings.Cut(operand, "=")
		if !explicit {
			replacement = "1"
		}
		// Reuse the driver's accepted signature grammar, but normalize phase-3
		// comments before retaining the exact replacement tokens for expansion.
		if _, _, valid := configDependencyCompilerMacroExpansionDefinition(operand); !valid {
			return "compiler -D has an invalid signature"
		}
		text, reason := configDependencyPreprocessorText([]byte(signature + " " + replacement))
		if reason != "" || strings.ContainsAny(text, "\r\n") {
			return "compiler -D has unsupported preprocessing text: " + reason
		}
		if !s.setSourceMacroReplacement(text, fmt.Sprintf("%s:%d", identity, index), configDependencyMacroDefined) {
			return "compiler -D replacement cannot be installed"
		}
	}
	return ""
}
