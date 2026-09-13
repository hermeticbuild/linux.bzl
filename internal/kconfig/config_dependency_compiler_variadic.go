package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type compilerVariadicCommaRead struct {
	Syntax, AnswerID string
	DeleteComma      bool
}

// A bounded hint from a complete normalized definition, never evidence that
// the definition executes or that this dialect deletes a comma.
func configDependencyVariadicCommaHint(line string) string {
	if !strings.Contains(line, "...") {
		return ""
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "%:") {
		line = "#" + line[2:]
	}
	line, directive := strings.CutPrefix(line, "#")
	if !directive {
		return ""
	}
	line = strings.TrimSpace(line)
	line, define := strings.CutPrefix(line, "define")
	if !define || line == "" || !strings.ContainsRune(" \t", rune(line[0])) {
		return ""
	}
	_, parsed, err := configDependencyMacroCallParse(strings.TrimSpace(line), configDependencyMacroCallMode{})
	if err != nil || !parsed.function || len(parsed.formals) != 0 || parsed.variadic == "" {
		return ""
	}
	for index := 0; index+2 < len(parsed.replacement); index++ {
		if parsed.replacement[index].text == "," && parsed.replacement[index+1].text == "##" && parsed.replacement[index+2].text == parsed.variadic {
			return parsed.variadicSyntax
		}
	}
	return ""
}

func (s *configDependencyClosureScanner) configureCompilerVariadicQueries(
	plan *ActionPlan, node ActionPlanNode, role string, probe configDependencyCompilerPredefineProbe,
	observe func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string),
) {
	s.compilerVariadicHint = nil
	if s.callCoverage != nil {
		s.callCoverage.mode.variadicComma = nil
	}
	if probe.language != "c" && probe.language != "c++" {
		return
	}
	context := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), role, probe.language, probe.arguments, probe.translationUnits, probe.environment)
	answers := map[string]compilerVariadicCommaAnswer{}
	reasons := map[string]string{}
	for _, syntax := range []string{"standard", "named"} {
		answer, ready, err := configDependencySupplementalCompilerVariadicComma(plan, compilerVariadicCommaKey{context, syntax})
		if err != nil {
			reasons[syntax] = err.Error()
		} else if ready {
			answers[syntax] = answer
		}
	}
	if s.collectCompilerGuards && observe != nil {
		s.compilerVariadicHint = func(file configDependencyScanFile, contentID, syntax string) {
			if _, answered := answers[syntax]; answered || reasons[syntax] != "" {
				return
			}
			observe(probe, file, configDependencyCompilerGuardHints{variadicSyntax: syntax, optionalVariadicHints: true}, contentID)
		}
	}
	if s.callCoverage == nil {
		return
	}
	s.callCoverage.mode.variadicComma = func(syntax string) (bool, string, string) {
		if reason := reasons[syntax]; reason != "" {
			return false, "", reason
		}
		if answer, ready := answers[syntax]; ready {
			return answer.deleteComma, answer.identity, ""
		}
		if s.compilerVariadicHint != nil {
			file := s.compilerIntrinsicFile
			if file.logical != "" {
				if contents, err := file.contents(); err == nil {
					// An actually reached expansion owns a demand independently
					// of any earlier, discardable definition inventory.
					observe(probe, file, configDependencyCompilerGuardHints{variadicSyntax: syntax}, fmt.Sprintf("%x", sha256.Sum256(contents)))
				}
			}
		}
		return false, "", "no measured answer for this compiler context and variadic syntax"
	}
}
