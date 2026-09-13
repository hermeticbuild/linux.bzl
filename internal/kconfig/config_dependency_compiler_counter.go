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
	if s.callCoverage == nil {
		return
	}
	s.callCoverage.mode.counterMissing = nil
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
