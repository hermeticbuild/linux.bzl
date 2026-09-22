package kconfig

// A source Makefile may select include $(FEATURES_DUMP) instead of the normal
// tools/build/Makefile.feature include. Construct the dump's individual feature
// values from the normal include's source-defined compiler checks. This keeps
// the alternate include's input tied to measured compiler results without
// parsing the normal include's unrelated test-all fast path.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type SelectedFeatureDumpProbe struct {
	Feature string
	Command string
	// DumpPath is the canonical object-tree-relative FEATURES_DUMP input.
	DumpPath string
}

// MeasureSelectedFeatureDumpStatus registers one source-derived child compiler
// status in discovery, then optionally resolves it from sealed results whose
// selected references match this exact request. A discovery round may leave a
// newly selected status symbolic until its next measured round; ordinary
// replay rejects it rather than interpreting a missing result as false.
func (s *KbuildProbeScopes) MeasureSelectedFeatureDumpStatus(
	ctx context.Context, command string, results *KbuildGraphGuardResults,
	registerOnly ...bool,
) (string, []string, error) {
	if len(registerOnly) > 1 {
		return "", nil, fmt.Errorf("selected feature status accepts only one discovery mode")
	}
	discovery := len(registerOnly) == 1 && registerOnly[0]
	invocation, err := parseRecursiveMakeFeatureInvocation(command)
	if err != nil {
		return "", nil, fmt.Errorf("selected feature status command: %w", err)
	}
	cc, ok := invocation.variables["CC"]
	role, configured := parseKbuildActionRoleToken(cc)
	if !ok || !configured || role.Scope != "host" || role.Role != "cc" ||
		invocation.success != "1" || invocation.failure != "0" {
		return "", nil, fmt.Errorf("selected feature status requires the configured host CC and Boolean 1/0 reduction")
	}
	if s == nil {
		return "", nil, fmt.Errorf("selected feature status requires configured probe scopes")
	}
	value, err := s.kbuildShell(ctx, "host", command, nil)
	if err != nil {
		return "", nil, fmt.Errorf("measure selected feature status: %w", err)
	}
	references, err := s.GraphGuardReferences([]string{value})
	if err != nil {
		return "", nil, fmt.Errorf("selected feature status references: %w", err)
	}
	if len(references) == 0 || !linuxProbeSymbolPattern.MatchString(value) {
		return "", nil, fmt.Errorf("selected feature status has no measured probe reference")
	}
	terminalIDs := make([]string, 0, len(references))
	for _, reference := range references {
		if reference.NodeID == "" || reference.Scope != "host" {
			return "", nil, fmt.Errorf("selected feature status has an unbound or foreign probe reference")
		}
		terminalIDs = append(terminalIDs, reference.NodeID)
	}
	if results == nil {
		return value, terminalIDs, nil
	}
	if err := results.validateScopes(s); err != nil {
		return "", nil, fmt.Errorf("selected feature status result scope: %w", err)
	}
	if !results.binds(references) {
		if discovery {
			return value, terminalIDs, nil
		}
		return "", nil, fmt.Errorf("selected feature status probe references differ from the sealed pregraph plan")
	}
	host := s.evaluators["host"]
	if host == nil {
		return "", nil, fmt.Errorf("selected feature status has no host evaluator")
	}
	value, err = results.resolve(host, value)
	if err != nil {
		return "", nil, fmt.Errorf("resolve selected feature status from sealed pregraph results: %w", err)
	}
	if value != "0" && value != "1" {
		return "", nil, fmt.Errorf("selected feature status result %q is not a Boolean 0/1", value)
	}
	return value, terminalIDs, nil
}

// DeriveSelectedFeatureDumpProbeCommands validates the selected Makefile's
// alternate include and expands exactly the individual feature_check_code
// query from its immutable Makefile.feature. The profile must be captured
// from that selected Makefile with FEATURES_DUMP set to an exact object-tree
// input. Each returned Command must be passed to the configured Kbuild shell
// evaluator: a command's status is unknown until its compiler probe is run.
func DeriveSelectedFeatureDumpProbeCommands(
	sourceRoot, selectedMakefile string,
	profile CompactKbuildProfile,
	featureTests []string,
) ([]SelectedFeatureDumpProbe, error) {
	if err := validateProbeSourcePath(selectedMakefile); err != nil {
		return nil, fmt.Errorf("selected feature Makefile: %w", err)
	}
	if profile.Path != selectedMakefile || profile.evaluator == nil {
		return nil, fmt.Errorf("feature dump profile must capture selected source Makefile %q", selectedMakefile)
	}
	if profile.evaluator.template == nil || !filepath.IsAbs(sourceRoot) ||
		filepath.Clean(profile.evaluator.template.sourceRoots[featureProbeSourceTree]) != filepath.Clean(sourceRoot) {
		return nil, fmt.Errorf("feature dump source root differs from the selected Makefile profile")
	}
	selected, err := readSelectedFeatureSource(sourceRoot, selectedMakefile)
	if err != nil {
		return nil, err
	}
	hooked, err := validateSelectedFeatureDumpAlternate(selected)
	if err != nil {
		return nil, fmt.Errorf("selected Makefile %q: %w", selectedMakefile, err)
	}
	if !hooked {
		return nil, nil
	}
	featureSource, err := readSelectedFeatureSource(sourceRoot, "tools/build/Makefile.feature")
	if err != nil {
		return nil, err
	}
	macro, err := selectedFeatureCheckMacro(featureSource)
	if err != nil {
		return nil, fmt.Errorf("selected tools/build/Makefile.feature: %w", err)
	}
	variables := make(map[string]string, 5)
	for _, name := range []string{"check_feat", "FEATURES_DUMP", "FEATURE_TESTS", "OUTPUT", "srctree"} {
		variables[name], err = selectedFeatureProfileValue(profile, "$("+name+")")
		if err != nil {
			return nil, fmt.Errorf("selected %s: %w", name, err)
		}
	}
	if variables["check_feat"] != "1" ||
		!strings.HasPrefix(variables["FEATURES_DUMP"], featureProbeObjectTree+"/") ||
		variables["srctree"] != featureProbeSourceTree {
		return nil, fmt.Errorf("FEATURES_DUMP must select an object-tree file under the active check_feat source branch")
	}
	if err := validateProbeSourcePath(strings.TrimPrefix(variables["FEATURES_DUMP"], featureProbeObjectTree+"/")); err != nil {
		return nil, fmt.Errorf("selected FEATURES_DUMP path: %w", err)
	}
	dumpPath := strings.TrimPrefix(variables["FEATURES_DUMP"], featureProbeObjectTree+"/")
	if len(featureTests) == 0 || len(featureTests) > 256 ||
		!slices.Equal(strings.Fields(variables["FEATURE_TESTS"]), featureTests) {
		return nil, fmt.Errorf("selected FEATURE_TESTS differ from the source-derived feature list")
	}
	if !strings.HasPrefix(variables["OUTPUT"], featureProbeObjectTree+"/") ||
		!strings.HasSuffix(variables["OUTPUT"], "/") {
		return nil, fmt.Errorf("selected OUTPUT must be an object-tree directory ending in /")
	}
	if err := validateProbeSourcePath(strings.TrimSuffix(strings.TrimPrefix(variables["OUTPUT"], featureProbeObjectTree+"/"), "/")); err != nil {
		return nil, fmt.Errorf("selected OUTPUT directory: %w", err)
	}
	outputFeatures := variables["OUTPUT"] + "feature/"
	injected := map[string]string{
		"feature_dir":     featureProbeSourceTree + "/tools/build/feature",
		"OUTPUT_FEATURES": outputFeatures,
	}
	for name, want := range injected {
		got, err := selectedFeatureProfileValue(profile, "$("+name+")")
		if err != nil {
			return nil, fmt.Errorf("existing %s: %w", name, err)
		}
		if got != "" && got != want {
			return nil, fmt.Errorf("selected Makefile overrides source feature variable %s", name)
		}
	}
	probes := make([]SelectedFeatureDumpProbe, 0, len(featureTests))
	seen := map[string]bool{}
	for _, feature := range featureTests {
		if !selectedFeatureName(feature) || feature == "all" || seen[feature] {
			return nil, fmt.Errorf("selected FEATURE_TESTS contains unsupported or repeated feature %q", feature)
		}
		seen[feature] = true
		// GNU Make substitutes the one call parameter before expanding the
		// resulting macro. In this source macro, $(1) names the status and
		// per-feature flags, while $1 forms the private test-$1.bin goal.
		expression := strings.ReplaceAll(strings.ReplaceAll(macro, "$(1)", feature), "$1", feature)
		command, err := evaluateCompactKbuildTextForMakeTarget(
			profile, "", "", "", "", nil, nil, injected, expression, false,
		)
		if err != nil {
			return nil, fmt.Errorf("expand source feature %q: %w", feature, err)
		}
		invocation, err := parseRecursiveMakeFeatureInvocation(command)
		if err != nil {
			return nil, fmt.Errorf("source feature %q child status: %w", feature, err)
		}
		if invocation.directory != "tools/build/feature" ||
			invocation.goal != strings.TrimPrefix(outputFeatures, featureProbeObjectTree+"/")+"test-"+feature+".bin" ||
			invocation.output != outputFeatures || invocation.success != "1" || invocation.failure != "0" ||
			len(invocation.variables) != 6 {
			return nil, fmt.Errorf("source feature %q changed its private compiler query or Boolean status", feature)
		}
		cc, exists := invocation.variables["CC"]
		ref, configured := parseKbuildActionRoleToken(cc)
		if !exists || !configured || ref.Scope != "host" || ref.Role != "cc" {
			return nil, fmt.Errorf("source feature %q must use the configured host CC", feature)
		}
		for _, name := range []string{"OUTPUT", "CC", "CXX", "CFLAGS", "CXXFLAGS", "LDFLAGS"} {
			if _, exists := invocation.variables[name]; !exists {
				return nil, fmt.Errorf("source feature %q has no %s assignment", feature, name)
			}
		}
		probes = append(probes, SelectedFeatureDumpProbe{Feature: feature, Command: command, DumpPath: dumpPath})
	}
	return probes, nil
}

func readSelectedFeatureSource(root, relative string) ([]string, error) {
	if err := validateProbeSourcePath(relative); err != nil {
		return nil, err
	}
	filename := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(filename)
	if err != nil {
		return nil, fmt.Errorf("read selected feature source %q: %w", relative, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("selected feature source %q must be a regular file of at most 1 MiB", relative)
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read selected feature source %q: %w", relative, err)
	}
	lines := make([]string, 0, strings.Count(string(contents), "\n")+1)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(stripKbuildComment(line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// SelectedFeatureDumpSourceAlternate checks the same cleaned source lines
// used to authenticate a selected feature include. A Makefile comment which
// merely mentions FEATURES_DUMP does not request a compiler probe.
func SelectedFeatureDumpSourceAlternate(root, relative string) (bool, error) {
	lines, err := readSelectedFeatureSource(root, relative)
	if err != nil {
		return false, err
	}
	return validateSelectedFeatureDumpAlternate(lines)
}

func validateSelectedFeatureDumpAlternate(lines []string) (bool, error) {
	type condition struct {
		text     string
		elseSeen bool
	}
	var stack []condition
	selected, references := 0, 0
	for index, line := range lines {
		if line == "ifeq ($(FEATURES_DUMP),)" {
			selected++
			if selected != 1 || len(stack) == 0 || stack[len(stack)-1].text != "ifeq ($(check_feat),1)" || stack[len(stack)-1].elseSeen ||
				index+4 >= len(lines) || lines[index+1] != "include $(srctree)/tools/build/Makefile.feature" ||
				lines[index+2] != "else" || lines[index+3] != "include $(FEATURES_DUMP)" || lines[index+4] != "endif" {
				return false, fmt.Errorf("FEATURES_DUMP alternate must include only the source Makefile.feature or the selected dump")
			}
		}
		if strings.Contains(line, "FEATURES_DUMP") {
			references++
			if line != "ifeq ($(FEATURES_DUMP),)" && line != "include $(FEATURES_DUMP)" {
				return false, fmt.Errorf("FEATURES_DUMP is referenced outside its authenticated include branch")
			}
		}
		switch {
		case strings.HasPrefix(line, "ifeq ") || strings.HasPrefix(line, "ifneq ") ||
			strings.HasPrefix(line, "ifdef ") || strings.HasPrefix(line, "ifndef "):
			stack = append(stack, condition{text: line})
		case line == "else":
			if len(stack) == 0 || stack[len(stack)-1].elseSeen {
				return false, fmt.Errorf("FEATURES_DUMP source has unmatched else")
			}
			stack[len(stack)-1].elseSeen = true
		case line == "endif":
			if len(stack) == 0 {
				return false, fmt.Errorf("FEATURES_DUMP source has unmatched endif")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if selected == 0 && references == 0 {
		return false, nil
	}
	if selected != 1 || references != 2 || len(stack) != 0 {
		return false, fmt.Errorf("FEATURES_DUMP alternate include is malformed or has an unclosed source guard")
	}
	return true, nil
}

func selectedFeatureCheckMacro(lines []string) (string, error) {
	const head = "feature_dir := $(srctree)/tools/build/feature"
	const output = "OUTPUT_FEATURES = $(OUTPUT)feature/"
	const assignment = "feature_check = $(eval $(feature_check_code))"
	if len(lines) < 5 || lines[0] != head || lines[1] != "ifneq ($(OUTPUT),)" ||
		lines[2] != output || lines[3] != "$(shell mkdir -p $(OUTPUT_FEATURES))" || lines[4] != "endif" {
		return "", fmt.Errorf("feature output and source directory are not defined by the selected source header")
	}
	counts := map[string]int{}
	var macro string
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		// The normal include is not parsed when FEATURES_DUMP is selected.
		// Inspect every source line, including inactive conditional arms, so
		// a later definition cannot silently replace the checked macro.
		if strings.Contains(line, "feature_check_code") &&
			line != assignment && line != "define feature_check_code" {
			return "", fmt.Errorf("feature source redefines or undefines feature_check_code")
		}
		words := strings.Fields(line)
		if len(words) >= 2 && (words[0] == "define" || words[0] == "undefine") && words[1] == "feature_check" ||
			len(words) >= 3 && words[0] == "override" && words[1] == "undefine" && words[2] == "feature_check" ||
			selectedFeatureDirectEvalWrite(line) {
			return "", fmt.Errorf("feature source redefines or undefines feature_check")
		}
		if name, _, _, assignmentLine := splitKbuildAssignment(line); assignmentLine {
			switch name {
			case "feature_dir":
				if line != head {
					return "", fmt.Errorf("feature source redefines its source directory")
				}
			case "OUTPUT_FEATURES":
				if line != output {
					return "", fmt.Errorf("feature source redefines its private output directory")
				}
			case "feature_check":
				if line != assignment {
					return "", fmt.Errorf("feature source redefines its compiler status macro")
				}
			}
		}
		for _, expected := range []string{head, output, assignment} {
			if line == expected {
				counts[expected]++
			}
		}
		if line != "define feature_check_code" {
			continue
		}
		counts[line]++
		if index+2 >= len(lines) || lines[index+2] != "endef" {
			return "", fmt.Errorf("feature_check_code must define exactly one compiler status assignment")
		}
		name, operator, value, ok := splitKbuildAssignment(lines[index+1])
		if !ok || strings.TrimSpace(name) != "feature-$(1)" || operator != ":=" ||
			!strings.HasPrefix(strings.TrimSpace(value), "$(shell ") || !strings.HasSuffix(strings.TrimSpace(value), ")") {
			return "", fmt.Errorf("feature_check_code changed its compiler status expression")
		}
		macro = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "$(shell "), ")"))
		index += 2
	}
	for _, required := range []string{head, output, assignment, "define feature_check_code"} {
		if counts[required] != 1 {
			return "", fmt.Errorf("feature source must define one %q", required)
		}
	}
	if !strings.HasPrefix(macro, "$(MAKE) ") ||
		!strings.Contains(macro, "$(OUTPUT_FEATURES)test-$1.bin") ||
		!strings.Contains(macro, "$(FEATURE_CHECK_CFLAGS-$(1))") ||
		!strings.Contains(macro, "$(FEATURE_CHECK_CXXFLAGS-$(1))") ||
		!strings.Contains(macro, "$(FEATURE_CHECK_LDFLAGS-$(1))") {
		return "", fmt.Errorf("feature_check_code no longer selects its source-defined per-feature query")
	}
	return macro, nil
}

func selectedFeatureDirectEvalWrite(line string) bool {
	for _, prefix := range []string{"$(eval feature_check", "$(eval override feature_check"} {
		index := strings.Index(line, prefix)
		if index < 0 {
			continue
		}
		rest := line[index+len(prefix):]
		if rest == "" || strings.ContainsRune(" \t:+?=)", rune(rest[0])) {
			return true
		}
	}
	return false
}

func selectedFeatureProfileValue(profile CompactKbuildProfile, expression string) (string, error) {
	return evaluateCompactKbuildTextForMakeTarget(profile, "", "", "", "", nil, nil, nil, expression, false)
}

func selectedFeatureName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character == '-' || character == '_' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}
