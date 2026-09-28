package kconfig

import (
	"crypto/sha256"
	"fmt"
)

// Batch size is a bounded scheduling choice, not an assumed compiler value or
// recurrence. Grow only after the actual source interpreter needs an unmeasured
// position. Every consumed token must still come from a successful compiler run.
func compilerCounterDemandCount(required int) int {
	if required < 1 || required > maxCompilerCounterExpansions {
		return 0
	}
	count := 64
	for count < required {
		count *= 2
	}
	return count
}

func (s *configDependencyClosureScanner) configureCompilerCounterQueries(
	plan *ActionPlan, node ActionPlanNode, role string,
	probe configDependencyCompilerPredefineProbe,
	observe func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string),
) {
	s.compilerCounterHint = nil
	if s.callCoverage != nil {
		s.callCoverage.mode.counterMissing = nil
	}
	if !s.collectCompilerGuards || observe == nil || probe.language != "c" && probe.language != "c++" {
		return
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), role,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	contextID := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	sequence, _, err := configDependencySupplementalCompilerCounter(plan, actionPlanConfigDependencyScope(node), role,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	if err != nil {
		return
	} // Initial-state construction reports replay errors.
	// An entered file can mention the counter before complete call expansion
	// reaches it. Prefetch is only a weaker scheduling hint, never an expansion
	// read or a source-state transition. It needs the measured nontextual initial
	// binding and does not grow an already measured prefix speculatively.
	if s.compilerCounterInitialAvailable && sequence == nil {
		s.compilerCounterHint = func(file configDependencyScanFile, contentID string) {
			observe(probe, file, configDependencyCompilerGuardHints{counterCount: compilerCounterDemandCount(1), optionalCounterHints: true}, contentID)
		}
	} else if s.compilerIntrinsicInitialSnapshot != nil {
		// Availability itself can be hidden behind an earlier source failure.
		// Ask through the existing weak definedness tier before requesting values;
		// a spelling, dump omission or failed attempt never means undefined.
		initial, valid := s.compilerIntrinsicInitialSnapshot.lookup("__COUNTER__")
		_, measured := s.compilerGuardInitialDefinitions["__COUNTER__"]
		if valid && initial.definition == configDependencyMacroUnknown && !measured {
			s.compilerCounterHint = func(file configDependencyScanFile, contentID string) {
				observe(probe, file, configDependencyCompilerGuardHints{names: []string{"__COUNTER__"}, optionalTokenHints: true}, contentID)
			}
		}
	}
	if s.callCoverage == nil {
		return
	}
	s.callCoverage.mode.counterMissing = func(context string, required int) {
		if context != contextID || sequence != nil && required <= len(sequence.values) {
			return
		}
		count := compilerCounterDemandCount(required)
		if count == 0 {
			return
		}
		file := s.compilerIntrinsicFile
		if file.logical == "" {
			return
		}
		contents, err := file.contents()
		if err != nil {
			return
		}
		observe(probe, file, configDependencyCompilerGuardHints{counterCount: count}, fmt.Sprintf("%x", sha256.Sum256(contents)))
	}
}
