package kconfig

// configDependencyMacroReplacement retains a normalized #define operand and
// its authenticated source origin. Bodies are parsed lazily by the call engine;
// an unused unsupported body does not invalidate an otherwise exact stream.
// Equality includes both bytes and origin, never just macro definedness.
type configDependencyMacroReplacement struct {
	text, origin string
	intrinsic    *configDependencyCompilerIntrinsicBinding
	counter      *configDependencyCompilerCounterBinding
}

func (s *configDependencyMacroState) joinMacroReplacements(left, right *configDependencyMacroState) map[string]configDependencyMacroReplacement {
	if s.macroReplacements == nil {
		return nil
	}
	leftValues, rightValues := s.macroReplacements, s.macroReplacements
	if left != nil {
		leftValues = left.macroReplacements
	}
	if right != nil {
		rightValues = right.macroReplacements
	}
	joined := map[string]configDependencyMacroReplacement{}
	for name, value := range leftValues {
		if other, present := rightValues[name]; present && value == other {
			joined[name] = value
		}
	}
	return joined
}

// setSourceMacroReplacement is an ordered #define. An uncertain write can
// preserve a replacement only when executing and skipping it yield exactly the
// same record. A defined-but-unmodeled macro remains unmodeled after the join.
func (s *configDependencyMacroState) setSourceMacroReplacement(text, origin string, active configDependencyMacroDefinition) bool {
	name, valid := configDependencyMacroIdentifier(text)
	if !valid || origin == "" || s == nil || !s.validSnapshotLineage() {
		return false
	}
	if active == configDependencyMacroUndefined {
		return true
	}
	if active != configDependencyMacroDefined && active != configDependencyMacroUnknown {
		return false
	}
	value := configDependencyMacroReplacement{text: text, origin: origin}
	previous, modeled := s.macroReplacements[name]
	definition := configDependencyMacroDefined
	if active == configDependencyMacroUnknown && s.definition(name) != definition {
		definition = configDependencyMacroUnknown
	}
	s.set(name, definition)
	if s.tainted {
		return false
	}
	if s.macroReplacements != nil && definition == configDependencyMacroDefined &&
		(active == configDependencyMacroDefined || modeled && previous == value) {
		s.macroReplacements[name] = value
	}
	return true
}

func (s *configDependencyMacroState) applyAutoconfReplacements(
	definitions configDependencyResolvedAutoconfDefinitions,
	previous map[string]configDependencyMacroReplacement,
	maybe bool,
) {
	if s.macroReplacements == nil || s.tainted {
		return
	}
	guard := configDependencyMacroReplacement{
		text: configDependencyResolvedAutoconfGuard, origin: configDependencyAutoconfPath,
	}
	// An unknown guard may already have an arbitrary replacement. Its final
	// definedness does not prove that this invocation wrote our empty body.
	if !maybe || previous[configDependencyResolvedAutoconfGuard] == guard {
		s.macroReplacements[configDependencyResolvedAutoconfGuard] = guard
	}
	for name, value := range definitions.replacements {
		if !maybe || previous[name] == value {
			s.macroReplacements[name] = value
		}
	}
}
