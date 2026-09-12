package kconfig

import (
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// KbuildCompilerIncludeOperand describes one compiler include option without
// changing whether its operand was joined to the option or passed as the next
// argv word. Discovery and action lowering share this parser so they cannot
// disagree about which relative paths are invocation-scoped.
type KbuildCompilerIncludeOperand struct {
	Flag          string
	Operand       string
	ArgumentIndex int
	Joined        bool
}

// KbuildCPreprocessorArgumentRange selects the argv interpreted by the C
// preprocessor embedded in one configured Kbuild action role. cc/cxx consume
// their ordinary argv and retain the compiler's normal `--` termination
// semantics. bindgen passes its libclang argv after exactly one `--`; a missing
// or ambiguous delimiter is intentionally left unmodeled.
func KbuildCPreprocessorArgumentRange(role string, arguments []string) (start, end int, ok bool) {
	switch role {
	case "cc", "cxx":
		return 0, len(arguments), true
	case "bindgen":
		delimiter := -1
		for index, argument := range arguments {
			if argument != "--" {
				continue
			}
			if delimiter >= 0 {
				return 0, 0, false
			}
			delimiter = index
		}
		if delimiter < 0 {
			return 0, 0, false
		}
		return delimiter + 1, len(arguments), true
	default:
		return 0, 0, false
	}
}

// KbuildCompilerIncludeOperands returns the include search and forced-include
// operands selected by a compiler argv. The obsolete standalone -I- option is
// deliberately not treated as a pathname.
func KbuildCompilerIncludeOperands(arguments []string) []KbuildCompilerIncludeOperand {
	flags := []string{"-idirafter", "-isystem", "-iquote", "-imacros", "-include", "-I"}
	operands := []KbuildCompilerIncludeOperand{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		if argument == "-I-" {
			continue
		}
		for _, flag := range flags {
			if argument == flag {
				if index+1 < len(arguments) && !strings.HasPrefix(arguments[index+1], "-") {
					index++
					operands = append(operands, KbuildCompilerIncludeOperand{
						Flag: flag, Operand: arguments[index], ArgumentIndex: index,
					})
				}
				break
			}
			if operand, ok := strings.CutPrefix(argument, flag); ok && operand != "" {
				operands = append(operands, KbuildCompilerIncludeOperand{
					Flag: flag, Operand: operand, ArgumentIndex: index, Joined: true,
				})
				break
			}
		}
	}
	return operands
}

// ResolveCompactKbuildCompilerIncludePath resolves a compiler include operand
// to its exact source/object tree location. relative reports whether the
// operand inherited the Make process cwd, as opposed to carrying an explicit
// tree marker. GCC/Clang =foo sysroot forms and unresolved absolute/dynamic
// paths are returned as unhandled so their spelling remains intact.
func ResolveCompactKbuildCompilerIncludePath(
	profile CompactKbuildProfile,
	value string,
) (location CompactKbuildInvocationLocation, relative, ok bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "=") {
		return CompactKbuildInvocationLocation{}, false, false, nil
	}
	if compactKbuildContainsPrivateToolsetPathByte(value) {
		// Capability tags are transient planning data. Strip only their
		// structural identity here so the exact-token check accepts both a
		// planning capability and its normalized runtime core; authenticity is
		// still verified by the workload codec before action replay.
		core, err := toolaction.CanonicalizeExecutionRootProvenanceCapabilityIdentity(value)
		if err != nil {
			return CompactKbuildInvocationLocation{}, false, false, fmt.Errorf("compiler include has invalid toolset-path provenance: %w", err)
		}
		if _, _, err := toolaction.DecodeExecutionRootProvenancePath(core); err != nil {
			return CompactKbuildInvocationLocation{}, false, false, fmt.Errorf("compiler include has invalid toolset-path provenance: %w", err)
		}
		// The identity-bound probe owns this root. It is neither source-tree nor
		// object-tree relative and is resolved only by the action runner through
		// the exact selected toolset closure.
		return CompactKbuildInvocationLocation{}, false, false, nil
	}
	value = compactKbuildCollapseCompilerTreeRootJoins(value)
	type rootMarker struct {
		marker string
		tree   CompactKbuildInvocationTree
	}
	for _, root := range []rootMarker{
		{marker: "__LINUX_BZL_SOURCE_TREE__", tree: CompactKbuildInvocationSourceTree},
		{marker: "${tree:kernel}", tree: CompactKbuildInvocationSourceTree},
		{marker: compactKbuildActionSourceTreeMarker, tree: CompactKbuildInvocationSourceTree},
		{marker: "__LINUX_BZL_OBJECT_TREE__", tree: CompactKbuildInvocationObjectTree},
		{marker: "${tree:prep}", tree: CompactKbuildInvocationObjectTree},
		{marker: "${work:root}", tree: CompactKbuildInvocationObjectTree},
		{marker: compactKbuildActionObjectTreeMarker, tree: CompactKbuildInvocationObjectTree},
		{marker: compactKbuildActionAbsoluteObjectTreeMarker, tree: CompactKbuildInvocationObjectTree},
	} {
		if value == root.marker {
			return CompactKbuildInvocationLocation{Tree: root.tree}, false, true, nil
		}
		if suffix, found := strings.CutPrefix(value, root.marker+"/"); found {
			directory, pathErr := canonicalCompactKbuildInvocationPath(suffix)
			if pathErr != nil {
				return CompactKbuildInvocationLocation{}, false, false, fmt.Errorf("compiler include path %q: %w", value, pathErr)
			}
			return CompactKbuildInvocationLocation{Tree: root.tree, Directory: directory}, false, true, nil
		}
	}
	if filepath.IsAbs(value) || strings.ContainsAny(value, "$\x00") {
		return CompactKbuildInvocationLocation{}, false, false, nil
	}
	if strings.Contains(value, `\`) {
		return CompactKbuildInvocationLocation{}, false, false, fmt.Errorf("compiler include path %q contains a non-portable separator", value)
	}
	base, found := CompactKbuildProfileInvocationLocation(profile)
	if !found {
		return CompactKbuildInvocationLocation{}, false, false, nil
	}
	directory, pathErr := canonicalCompactKbuildInvocationPath(path.Join(base.Directory, value))
	if pathErr != nil {
		return CompactKbuildInvocationLocation{}, false, false, fmt.Errorf(
			"resolve compiler include path %q from %s tree directory %q: %w",
			value, base.Tree, base.Directory, pathErr,
		)
	}
	return CompactKbuildInvocationLocation{Tree: base.Tree, Directory: directory}, true, true, nil
}

// compactKbuildCollapseCompilerTreeRootJoins normalizes only a compiler path
// operand's leading tree-root run. Action lowering normally performs this
// while roots still carry private provenance, but accepting the materialized
// form here keeps configured compiler analysis canonical at every entry point.
// The left root retains the tree identity; each following root represents the
// already-rooted Kbuild directory operand and is idempotent.
func compactKbuildCollapseCompilerTreeRootJoins(value string) string {
	markers := []string{
		"__LINUX_BZL_SOURCE_TREE__",
		"__LINUX_BZL_OBJECT_TREE__",
		"${tree:kernel}",
		"${tree:prep}",
		"${work:root}",
		compactKbuildActionSourceTreeMarker,
		compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker,
	}
	outer := ""
	for _, marker := range markers {
		if value == marker || strings.HasPrefix(value, marker+"/") {
			outer = marker
			break
		}
	}
	if outer == "" {
		return value
	}
	remainder := strings.TrimPrefix(value, outer)
	for {
		collapsed := false
		for _, marker := range markers {
			prefix := "/" + marker
			if remainder == prefix {
				remainder = ""
				collapsed = true
				break
			}
			if strings.HasPrefix(remainder, prefix+"/") {
				remainder = strings.TrimPrefix(remainder, prefix)
				collapsed = true
				break
			}
		}
		if !collapsed {
			return outer + remainder
		}
	}
}

func canonicalCompactKbuildInvocationPath(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	canonical := path.Clean(value)
	if canonical == "." {
		return "", nil
	}
	if value == "" || path.IsAbs(canonical) || canonical == ".." || strings.HasPrefix(canonical, "../") ||
		strings.Contains(canonical, `\`) || strings.ContainsRune(canonical, 0) {
		return "", fmt.Errorf("path %q escapes its declared tree", value)
	}
	return canonical, nil
}

func compactKbuildCompilerIncludeLocationValue(location CompactKbuildInvocationLocation) string {
	root := "${tree:kernel}"
	if location.Tree == CompactKbuildInvocationObjectTree {
		root = "${work:root}"
	}
	if location.Directory == "" {
		return root
	}
	return root + "/" + location.Directory
}

func rewriteCompactKbuildCompilerRelativeIncludes(
	profile CompactKbuildProfile,
	role string,
	objectRoot string,
	arguments []string,
) ([]string, error) {
	rewritten := append([]string(nil), arguments...)
	start, end, ok := KbuildCPreprocessorArgumentRange(role, arguments)
	if !ok {
		return rewritten, nil
	}
	for _, operand := range KbuildCompilerIncludeOperands(arguments[start:end]) {
		location, relative, ok, err := ResolveCompactKbuildCompilerIncludePath(profile, operand.Operand)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		value := ""
		if location.Tree == CompactKbuildInvocationObjectTree {
			usesOverlay, overlayErr := compactKbuildGraphPathUsesSourceOverlay(profile, location.Directory)
			if overlayErr != nil {
				return nil, overlayErr
			}
			if usesOverlay {
				// External sources are copied to the same logical path in every
				// action's private root. Keep their include operands relative to
				// the action's actual execution cwd so fixdep's generated .cmd
				// metadata remains replayable by later actions instead of retaining
				// one sandbox's absolute ${work:root} expansion. Most compiler
				// actions execute at the private root (objectRoot == "."); typed
				// compound and response-file actions execute below it.
				value = path.Join(objectRoot, location.Directory)
			}
		}
		if value == "" {
			if !relative {
				continue
			}
			value = compactKbuildCompilerIncludeLocationValue(location)
		}
		if operand.Joined {
			value = operand.Flag + value
		}
		rewritten[start+operand.ArgumentIndex] = value
	}
	return rewritten, nil
}

// compactKbuildCompilerOutputAnalysis is the complete role-specific filesystem
// contract derived from one configured compiler argv. Arguments contains the
// writable-path projection, PrimaryOutput is the logical object-tree path used
// by graph lowering, PreparedObjectInputs contains exact immutable object-tree
// files which must be staged when the command executes against a private
// writable root, PreparedObjectDirectories contains object-tree include roots
// whose exact immutable leaves must be staged, and WorkingDirectories contains
// every directory which the compiler may need to exist before execution.
type compactKbuildCompilerOutputAnalysis struct {
	Arguments                  []string
	PrimaryOutput              string
	PreparedObjectInputs       []string
	PreparedObjectIncludeFiles []string
	PreparedObjectDirectories  []string
	PersistentOutputs          []string
	LibrarySearchDirectories   []string
	WorkingDirectories         []string
}

type compactKbuildCompilerOutputAnalyzer struct {
	profile              CompactKbuildProfile
	includeArguments     []string
	analysis             compactKbuildCompilerOutputAnalysis
	workingDirectories   map[string]bool
	preparedInputs       map[string]bool
	preparedIncludeFiles map[string]bool
	preparedDirectories  map[string]bool
	persistentOutputs    map[string]bool
	libraryDirectories   map[string]bool
	knownGraphOutputs    map[string]bool
	primaryOutputRank    int
}

const (
	compactKbuildCompilerSideOutputRank = iota
	compactKbuildCompilerEmitOutputRank
	compactKbuildCompilerPrimaryOutputRank
)

// analyzeCompactKbuildCompilerOutputs derives output semantics from the
// selected action role, never from an executable spelling. cc/cxx dependency
// outputs and rustc/clippy emit paths share the same path resolver, so relative
// paths, prepared-tree paths, parent directory creation, and `--` termination
// cannot diverge between discovery and rewriting.
func analyzeCompactKbuildCompilerOutputs(
	profile CompactKbuildProfile,
	role string,
	arguments []string,
	knownGraphOutputs ...string,
) (compactKbuildCompilerOutputAnalysis, error) {
	return analyzeCompactKbuildCompilerOutputsWithIncludeProvenance(
		profile, role, arguments, arguments, knownGraphOutputs...,
	)
}

// analyzeCompactKbuildCompilerOutputsWithIncludeProvenance separates the
// compiler argv which will execute from the include argv whose tree provenance
// was captured from Make. Source-overlay includes are projected relative to an
// action's eventual cwd, which can differ from the Make process cwd; resolving
// those projected spellings against the capture cwd a second time would stage
// the wrong prepared-object directory.
func analyzeCompactKbuildCompilerOutputsWithIncludeProvenance(
	profile CompactKbuildProfile,
	role string,
	arguments []string,
	includeArguments []string,
	knownGraphOutputs ...string,
) (compactKbuildCompilerOutputAnalysis, error) {
	switch role {
	case "cc", "cxx", "rustc", "clippy":
	default:
		return compactKbuildCompilerOutputAnalysis{}, fmt.Errorf("unsupported compiler action role %q", role)
	}
	analyzer := compactKbuildCompilerOutputAnalyzer{
		profile:          profile,
		includeArguments: append([]string(nil), includeArguments...),
		analysis: compactKbuildCompilerOutputAnalysis{
			Arguments: append([]string(nil), arguments...),
		},
		workingDirectories:   map[string]bool{},
		preparedInputs:       map[string]bool{},
		preparedIncludeFiles: map[string]bool{},
		preparedDirectories:  map[string]bool{},
		persistentOutputs:    map[string]bool{},
		libraryDirectories:   map[string]bool{},
		knownGraphOutputs:    map[string]bool{},
	}
	for _, rawOutput := range knownGraphOutputs {
		output := canonicalKbuildRulePath(rawOutput)
		if err := validatePlanRelativePath("known compiler graph output", output); err != nil {
			return compactKbuildCompilerOutputAnalysis{}, err
		}
		analyzer.knownGraphOutputs[output] = true
	}
	if role == "cc" || role == "cxx" {
		if err := analyzer.recordCPreprocessorInputs(); err != nil {
			return compactKbuildCompilerOutputAnalysis{}, err
		}
	}
	for index := 0; index < len(analyzer.analysis.Arguments); index++ {
		argument := analyzer.analysis.Arguments[index]
		if argument == "--" {
			break
		}
		if role == "rustc" || role == "clippy" {
			consumed, err := analyzer.recordRustTargetSpecInput(index)
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			if consumed != 0 {
				index += consumed - 1
				continue
			}
			consumed, err = analyzer.rewriteRustLibrarySearch(index)
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			if consumed != 0 {
				index += consumed - 1
				continue
			}
		}
		switch {
		case argument == "-o":
			if index+1 >= len(analyzer.analysis.Arguments) {
				continue
			}
			value, err := analyzer.rewriteFileOutput(
				analyzer.analysis.Arguments[index+1], "compiler primary output", compactKbuildCompilerPrimaryOutputRank,
			)
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index+1] = value
			index++
			continue
		case strings.HasPrefix(argument, "-o") && len(argument) > len("-o"):
			value, err := analyzer.rewriteFileOutput(
				strings.TrimPrefix(argument, "-o"), "compiler primary output", compactKbuildCompilerPrimaryOutputRank,
			)
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index] = "-o" + value
			continue
		}

		if role == "cc" || role == "cxx" {
			if err := analyzer.rewriteCCompilerOutput(index); err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			if (argument == "-MF" || argument == "-MJ") && index+1 < len(analyzer.analysis.Arguments) {
				index++
			}
			continue
		}

		switch {
		case argument == "--out-dir":
			if index+1 >= len(analyzer.analysis.Arguments) {
				continue
			}
			value, err := analyzer.rewriteOutputDirectory(analyzer.analysis.Arguments[index+1], "rustc output directory")
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index+1] = value
			index++
		case strings.HasPrefix(argument, "--out-dir="):
			value, err := analyzer.rewriteOutputDirectory(strings.TrimPrefix(argument, "--out-dir="), "rustc output directory")
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index] = "--out-dir=" + value
		case argument == "--emit":
			if index+1 >= len(analyzer.analysis.Arguments) {
				continue
			}
			value, err := analyzer.rewriteRustEmit(analyzer.analysis.Arguments[index+1])
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index+1] = value
			index++
		case strings.HasPrefix(argument, "--emit="):
			value, err := analyzer.rewriteRustEmit(strings.TrimPrefix(argument, "--emit="))
			if err != nil {
				return compactKbuildCompilerOutputAnalysis{}, err
			}
			analyzer.analysis.Arguments[index] = "--emit=" + value
		}
	}
	if role == "cc" || role == "cxx" {
		if err := analyzer.recordCImplicitPersistentOutputs(); err != nil {
			return compactKbuildCompilerOutputAnalysis{}, err
		}
	}
	for directory := range analyzer.workingDirectories {
		analyzer.analysis.WorkingDirectories = append(analyzer.analysis.WorkingDirectories, directory)
	}
	for input := range analyzer.preparedInputs {
		analyzer.analysis.PreparedObjectInputs = append(analyzer.analysis.PreparedObjectInputs, input)
	}
	for input := range analyzer.preparedIncludeFiles {
		analyzer.analysis.PreparedObjectIncludeFiles = append(analyzer.analysis.PreparedObjectIncludeFiles, input)
	}
	for directory := range analyzer.preparedDirectories {
		analyzer.analysis.PreparedObjectDirectories = append(analyzer.analysis.PreparedObjectDirectories, directory)
	}
	for output := range analyzer.persistentOutputs {
		analyzer.analysis.PersistentOutputs = append(analyzer.analysis.PersistentOutputs, output)
	}
	for directory := range analyzer.libraryDirectories {
		analyzer.analysis.LibrarySearchDirectories = append(analyzer.analysis.LibrarySearchDirectories, directory)
	}
	sort.Strings(analyzer.analysis.WorkingDirectories)
	sort.Strings(analyzer.analysis.PreparedObjectInputs)
	sort.Strings(analyzer.analysis.PreparedObjectIncludeFiles)
	sort.Strings(analyzer.analysis.PreparedObjectDirectories)
	sort.Strings(analyzer.analysis.PersistentOutputs)
	sort.Strings(analyzer.analysis.LibrarySearchDirectories)
	return analyzer.analysis, nil
}

// recordCPreprocessorInputs derives the immutable prepared-tree view from the
// selected compiler argv. Directory searches deliberately retain every exact
// preconfigured leaf below the selected root: macro-expanded include operands
// cannot be closed by parsing source text, while the compiler's own -I family
// is an exact, architecture- and toolchain-independent boundary. Forced
// includes and macro files remain exact single-file inputs.
func (a *compactKbuildCompilerOutputAnalyzer) recordCPreprocessorInputs() error {
	start, end, ok := KbuildCPreprocessorArgumentRange("cc", a.includeArguments)
	if !ok {
		return nil
	}
	for _, operand := range KbuildCompilerIncludeOperands(a.includeArguments[start:end]) {
		location, _, resolved, err := ResolveCompactKbuildCompilerIncludePath(a.profile, operand.Operand)
		if err != nil {
			return fmt.Errorf("compiler %s path %q: %w", operand.Flag, operand.Operand, err)
		}
		if !resolved || location.Tree != CompactKbuildInvocationObjectTree {
			continue
		}
		switch operand.Flag {
		case "-include", "-imacros":
			if location.Directory != "" {
				a.preparedIncludeFiles[location.Directory] = true
			}
		default:
			a.preparedDirectories[location.Directory] = true
		}
	}
	return nil
}

// recordRustTargetSpecInput records only a target-spec operand with explicit
// object-tree provenance. A bare value may be a built-in Rust target triple,
// so neither its spelling nor incidental same-named files can establish an
// action input. Kbuild's generated target specifications are rooted through
// $(objtree), which survives evaluation as a typed prepared-tree path.
func (a *compactKbuildCompilerOutputAnalyzer) recordRustTargetSpecInput(index int) (int, error) {
	argument := a.analysis.Arguments[index]
	value := ""
	consumed := 0
	switch {
	case argument == "--target":
		consumed = 1
		if index+1 >= len(a.analysis.Arguments) {
			return consumed, nil
		}
		value = a.analysis.Arguments[index+1]
		consumed = 2
	case strings.HasPrefix(argument, "--target="):
		value = strings.TrimPrefix(argument, "--target=")
		consumed = 1
	default:
		return 0, nil
	}

	location, relative, ok, err := ResolveCompactKbuildCompilerIncludePath(a.profile, value)
	if err != nil {
		return 0, fmt.Errorf("rustc target specification %q: %w", value, err)
	}
	if !ok || relative || location.Tree != CompactKbuildInvocationObjectTree || location.Directory == "" {
		return consumed, nil
	}
	a.preparedInputs[location.Directory] = true
	return consumed, nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteRustLibrarySearch(index int) (int, error) {
	argument := a.analysis.Arguments[index]
	value := ""
	consumed := 0
	joined := false
	switch {
	case argument == "-L":
		if index+1 >= len(a.analysis.Arguments) {
			return 1, nil
		}
		value = a.analysis.Arguments[index+1]
		consumed = 2
	case strings.HasPrefix(argument, "-L") && len(argument) > len("-L"):
		value = strings.TrimPrefix(argument, "-L")
		consumed = 1
		joined = true
	default:
		return 0, nil
	}

	kind := ""
	if prefix, pathValue, found := strings.Cut(value, "="); found {
		switch prefix {
		case "all", "crate", "dependency", "framework", "native":
			kind = prefix + "="
			value = pathValue
		}
	}
	location, _, ok, err := ResolveCompactKbuildCompilerIncludePath(a.profile, value)
	if err != nil {
		return 0, fmt.Errorf("rustc library search path %q: %w", value, err)
	}
	if !ok {
		return consumed, nil
	}
	rewritten := compactKbuildCompilerIncludeLocationValue(location)
	if location.Tree == CompactKbuildInvocationObjectTree {
		a.libraryDirectories[location.Directory] = true
	}
	if joined {
		a.analysis.Arguments[index] = "-L" + kind + rewritten
	} else {
		a.analysis.Arguments[index+1] = kind + rewritten
	}
	return consumed, nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteCCompilerOutput(index int) error {
	argument := a.analysis.Arguments[index]
	switch {
	case argument == "-MJ":
		if index+1 >= len(a.analysis.Arguments) {
			return nil
		}
		value, err := a.rewriteCPersistentOutput(a.analysis.Arguments[index+1], "compiler JSON output")
		if err != nil {
			return err
		}
		a.analysis.Arguments[index+1] = value
	case strings.HasPrefix(argument, "-MJ") && len(argument) > len("-MJ"):
		value, err := a.rewriteCPersistentOutput(strings.TrimPrefix(argument, "-MJ"), "compiler JSON output")
		if err != nil {
			return err
		}
		a.analysis.Arguments[index] = "-MJ" + value
	case argument == "-MF":
		if index+1 >= len(a.analysis.Arguments) {
			return nil
		}
		value, err := a.rewriteCDependencyOutput(a.analysis.Arguments[index+1])
		if err != nil {
			return err
		}
		a.analysis.Arguments[index+1] = value
	case strings.HasPrefix(argument, "-MF") && len(argument) > len("-MF"):
		value, err := a.rewriteCDependencyOutput(strings.TrimPrefix(argument, "-MF"))
		if err != nil {
			return err
		}
		a.analysis.Arguments[index] = "-MF" + value
	case strings.HasPrefix(argument, "-Wp,"):
		forwarded := strings.Split(argument, ",")
		for forwardedIndex := 1; forwardedIndex+1 < len(forwarded); forwardedIndex++ {
			switch forwarded[forwardedIndex] {
			case "-MD", "-MMD", "-MF":
			default:
				continue
			}
			value, err := a.rewriteCDependencyOutput(forwarded[forwardedIndex+1])
			if err != nil {
				return err
			}
			forwarded[forwardedIndex+1] = value
			forwardedIndex++
		}
		a.analysis.Arguments[index] = strings.Join(forwarded, ",")
	}
	return nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteCPersistentOutput(value, description string) (string, error) {
	output, ok, err := resolveCompactKbuildCompilerOutputPath(a.profile, value, description)
	if err != nil {
		return "", err
	}
	if ok && output.logical != "" {
		a.persistentOutputs[canonicalKbuildRulePath(output.logical)] = true
	}
	return a.rewriteFileOutput(value, description, compactKbuildCompilerSideOutputRank)
}

func (a *compactKbuildCompilerOutputAnalyzer) recordCImplicitPersistentOutputs() error {
	primary := canonicalKbuildRulePath(a.analysis.PrimaryOutput)
	if primary == "" {
		return nil
	}
	stackUsage, coverage, splitDwarf := false, false, false
	for _, argument := range a.analysis.Arguments {
		if argument == "--" {
			break
		}
		switch {
		case argument == "-fstack-usage":
			stackUsage = true
		case argument == "-fno-stack-usage":
			stackUsage = false
		case argument == "--coverage" || argument == "-ftest-coverage":
			coverage = true
		case argument == "-fno-test-coverage":
			coverage = false
		case argument == "-gsplit-dwarf" || strings.HasPrefix(argument, "-gsplit-dwarf="):
			splitDwarf = true
		case argument == "-gno-split-dwarf":
			splitDwarf = false
		}
	}
	stem := strings.TrimSuffix(primary, path.Ext(primary))
	outputs := []string{}
	if stackUsage {
		outputs = append(outputs, stem+".su")
	}
	if coverage {
		outputs = append(outputs, stem+".gcno")
	}
	if splitDwarf {
		outputs = append(outputs, stem+".dwo")
	}
	for _, output := range outputs {
		if err := validatePlanRelativePath("implicit compiler persistent output", output); err != nil {
			return err
		}
		a.persistentOutputs[output] = true
		if directory := path.Dir(output); directory != "." && directory != "" {
			a.workingDirectories[directory] = true
		}
	}
	return nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteCDependencyOutput(value string) (string, error) {
	return a.rewriteFileOutput(value, "compiler dependency output", compactKbuildCompilerSideOutputRank)
}

// compactKbuildExactCDependencyOutputs returns only explicitly named C-family
// depfiles.  They remain compiler-private scratch; the cmd_and_fixdep seam uses
// this parser solely to prove that its final rm removes the same one depfile.
func compactKbuildExactCDependencyOutputs(profile CompactKbuildProfile, arguments []string) ([]string, error) {
	outputs := map[string]bool{}
	record := func(value string) error {
		output, ok, err := resolveCompactKbuildCompilerOutputPath(profile, value, "compiler dependency output")
		if err != nil {
			return err
		}
		if ok && output.logical != "" {
			outputs[canonicalKbuildRulePath(output.logical)] = true
		}
		return nil
	}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			break
		}
		switch {
		case argument == "-MF" && index+1 < len(arguments):
			if err := record(arguments[index+1]); err != nil {
				return nil, err
			}
			index++
		case strings.HasPrefix(argument, "-MF") && len(argument) > len("-MF"):
			if err := record(strings.TrimPrefix(argument, "-MF")); err != nil {
				return nil, err
			}
		case strings.HasPrefix(argument, "-Wp,"):
			forwarded := strings.Split(argument, ",")
			for forwardedIndex := 1; forwardedIndex+1 < len(forwarded); forwardedIndex++ {
				switch forwarded[forwardedIndex] {
				case "-MD", "-MMD", "-MF":
				default:
					continue
				}
				if err := record(forwarded[forwardedIndex+1]); err != nil {
					return nil, err
				}
				forwardedIndex++
			}
		}
	}
	return slices.Sorted(maps.Keys(outputs)), nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteRustEmit(value string) (string, error) {
	emissions := strings.Split(value, ",")
	for index, emission := range emissions {
		kind, output, explicit := strings.Cut(emission, "=")
		if !explicit || kind == "" || output == "" {
			continue
		}
		rank := compactKbuildCompilerEmitOutputRank
		if kind == "dep-info" || kind == "metadata" {
			rank = compactKbuildCompilerSideOutputRank
		}
		rewritten, err := a.rewriteFileOutput(output, "rustc emit output", rank)
		if err != nil {
			return "", err
		}
		if kind == "link" || kind == "metadata" {
			resolved, ok, err := resolveCompactKbuildCompilerOutputPath(
				a.profile, output, "rustc persistent emit output",
			)
			if err != nil {
				return "", err
			}
			if ok && resolved.logical != "" {
				a.persistentOutputs[resolved.logical] = true
			}
		}
		emissions[index] = kind + "=" + rewritten
	}
	return strings.Join(emissions, ","), nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteFileOutput(value, description string, rank int) (string, error) {
	output, ok, err := resolveCompactKbuildCompilerOutputPath(a.profile, value, description)
	if err != nil || !ok {
		return value, err
	}
	if directory := path.Dir(output.logical); directory != "." && directory != "" {
		a.workingDirectories[directory] = true
	}
	if a.knownGraphOutputs[output.logical] {
		rank = compactKbuildCompilerPrimaryOutputRank
	}
	if output.logical != "" && rank > compactKbuildCompilerSideOutputRank && rank >= a.primaryOutputRank {
		a.analysis.PrimaryOutput = output.logical
		a.primaryOutputRank = rank
	}
	return output.argument, nil
}

func (a *compactKbuildCompilerOutputAnalyzer) rewriteOutputDirectory(value, description string) (string, error) {
	output, ok, err := resolveCompactKbuildCompilerOutputPath(a.profile, value, description)
	if err != nil || !ok {
		return value, err
	}
	if output.logical != "" {
		a.workingDirectories[output.logical] = true
	}
	return output.argument, nil
}

type compactKbuildCompilerOutputPath struct {
	argument string
	logical  string
}

func resolveCompactKbuildCompilerOutputPath(
	profile CompactKbuildProfile,
	value string,
	description string,
) (compactKbuildCompilerOutputPath, bool, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return compactKbuildCompilerOutputPath{}, false, nil
	}
	for _, marker := range []string{
		"${tree:kernel}", "__LINUX_BZL_SOURCE_TREE__",
		"${tree:prep}", "${tree:host}", "${tree:bootstrap}", "${tree:prehost}",
		"${work:root}", "__LINUX_BZL_OBJECT_TREE__",
		compactKbuildActionSourceTreeMarker, compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker,
	} {
		if value == marker {
			return compactKbuildCompilerOutputPath{argument: "${work:root}"}, true, nil
		}
		if suffix, rooted := strings.CutPrefix(value, marker+"/"); rooted {
			if suffix == "" {
				return compactKbuildCompilerOutputPath{argument: "${work:root}/"}, true, nil
			}
			trailingSlash := strings.HasSuffix(suffix, "/")
			logical, err := canonicalCompactKbuildInvocationPath(suffix)
			if err != nil {
				return compactKbuildCompilerOutputPath{}, false, fmt.Errorf("%s %q: %w", description, value, err)
			}
			argument := "${work:root}/" + logical
			if trailingSlash && logical != "" {
				argument += "/"
			}
			return compactKbuildCompilerOutputPath{argument: argument, logical: logical}, true, nil
		}
	}
	if filepath.IsAbs(value) || strings.ContainsAny(value, "$\x00") || strings.Contains(value, `\`) {
		return compactKbuildCompilerOutputPath{}, false, nil
	}
	trailingSlash := strings.HasSuffix(value, "/")
	logical := ""
	location, located := CompactKbuildProfileInvocationLocation(profile)
	if located && location.Directory != "" {
		// Automatic variables and expanded $(obj) values have already been
		// canonicalized to tree-root-relative paths. Preserve that provenance
		// when the spelling is at or below the invocation directory; every other
		// relative operand inherits the actual Make cwd, including valid ../
		// traversal which remains inside the declared tree.
		canonical, canonicalErr := canonicalCompactKbuildInvocationPath(value)
		if canonicalErr == nil && (canonical == location.Directory || strings.HasPrefix(canonical, location.Directory+"/")) {
			logical = canonical
		} else {
			var err error
			logical, err = canonicalCompactKbuildInvocationPath(path.Join(location.Directory, value))
			if err != nil {
				return compactKbuildCompilerOutputPath{}, false, fmt.Errorf(
					"resolve %s %q from %s tree directory %q: %w",
					description, value, location.Tree, location.Directory, err,
				)
			}
		}
	} else {
		var err error
		logical, err = canonicalCompactKbuildInvocationPath(value)
		if err != nil {
			return compactKbuildCompilerOutputPath{}, false, fmt.Errorf("%s %q: %w", description, value, err)
		}
	}
	argument := value
	canonicalArgument, _ := canonicalCompactKbuildInvocationPath(value)
	if logical != canonicalArgument {
		argument = "${work:root}/" + logical
		if trailingSlash && logical != "" {
			argument += "/"
		}
	}
	return compactKbuildCompilerOutputPath{argument: argument, logical: logical}, true, nil
}
