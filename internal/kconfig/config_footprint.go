package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// configSourceScanner follows each translation unit's transitive source-tree
// include closure. The legacy scanner conservatively counts CONFIG_* tokens in
// comments and inactive branches. V0.0.13 preprocesses logical directives,
// evaluates direct CONFIG_* gates per resolved config, records file digests,
// and fails when a potentially active include cannot be accounted for.
type configSourceScanner struct {
	sourceRoot   string
	sourceRoots  map[string]string
	includeRoots []string
	predefined   map[string]bool
	exact        bool
	sourceCache  *compactSourceCache

	files       map[string]scannedSourceFile
	fileErrors  map[string]error
	closure     map[string]sourceClosure
	closureErrs map[string]error
	configIDs   map[*ResolvedConfig]string
}

type compactSourceCache struct {
	exactFiles  map[string]exactSourceFile
	exactErrors map[string]error
	treePaths   map[string]sourceTreePathResolution
}

type sourceClosure struct {
	refs              []string
	sourceIncludes    []string
	generatedIncludes []string
	sourceInputs      []CompactSourceInput
}

type scannedSourceFile struct {
	refs     []string
	includes []sourceIncludeDirective
	digest   string
}

type exactSourceFile struct {
	lines        []sourceLogicalLine
	digest       string
	preprocessed bool
}

type sourceTreePathResolution struct {
	abs    string
	exists bool
}

type sourceLogicalLine struct {
	line int
	text string
}

type sourceIncludeDirective struct {
	line              int
	path              string
	spelling          string
	kind              sourceIncludeKind
	literal           bool
	potentiallyActive bool
}

type sourceClosureStackEntry struct {
	treePath                      string
	linuxLibfdtEnvironmentGuarded bool
}

type sourceIncludeKind uint8

const (
	sourceIncludeNonliteral sourceIncludeKind = iota
	sourceIncludeQuoted
	sourceIncludeAngled
	sourceIncludeNext
)

type sourceScanProfile string

const (
	sourceScanKernel          sourceScanProfile = ""
	sourceScanKernelModule    sourceScanProfile = "kernel-module"
	sourceScanArm64VDSO       sourceScanProfile = "arm64-vdso"
	sourceScanArm32CompatVDSO sourceScanProfile = "arm32-compat-vdso"
)

var sourceConfigPredefinedSymbols = []struct {
	predefined string
	config     string
}{
	{predefined: "GCC_PLUGINS", config: "CONFIG_GCC_PLUGINS"},
	{predefined: "RANDSTRUCT", config: "CONFIG_RANDSTRUCT"},
}

type sourceIncludeSearch struct {
	quoteRoots   []string
	includeRoots []string
	systemRoots  []string
}

func (s sourceIncludeSearch) cacheKey() string {
	return "quote=" + strings.Join(s.quoteRoots, "\x00") +
		"\x01include=" + strings.Join(s.includeRoots, "\x00") +
		"\x01system=" + strings.Join(s.systemRoots, "\x00")
}

func (s sourceIncludeSearch) roots(kind sourceIncludeKind) []string {
	var roots []string
	if kind == sourceIncludeQuoted || kind == sourceIncludeNext {
		roots = append(roots, s.quoteRoots...)
	}
	roots = append(roots, s.includeRoots...)
	roots = append(roots, s.systemRoots...)
	return roots
}

func newConfigSourceScanner(opts CompactMetadataOptions) *configSourceScanner {
	return newConfigSourceScannerWithCache(opts, newCompactSourceCache())
}

func newCompactSourceCache() *compactSourceCache {
	return &compactSourceCache{
		exactFiles:  map[string]exactSourceFile{},
		exactErrors: map[string]error{},
		treePaths:   map[string]sourceTreePathResolution{},
	}
}

func newConfigSourceScannerWithCache(opts CompactMetadataOptions, sourceCache *compactSourceCache) *configSourceScanner {
	roots := []string{}
	if opts.Srcarch != "" {
		roots = append(roots,
			"arch/"+opts.Srcarch+"/include",
		)
	}
	roots = append(roots, "include")
	if opts.Srcarch != "" {
		roots = append(roots, "arch/"+opts.Srcarch+"/include/uapi")
	}
	roots = append(roots, "include/uapi")
	return &configSourceScanner{
		sourceRoot:   opts.SourceRoot,
		sourceRoots:  opts.SourceRoots,
		includeRoots: roots,
		predefined:   sourcePredefinedSymbols(opts.Srcarch),
		exact:        opts.Schema.isV013(),
		sourceCache:  sourceCache,
		files:        map[string]scannedSourceFile{},
		fileErrors:   map[string]error{},
		closure:      map[string]sourceClosure{},
		closureErrs:  map[string]error{},
		configIDs:    map[*ResolvedConfig]string{},
	}
}

func (s *configSourceScanner) actionIncludeSearch(source string, flags []string) sourceIncludeSearch {
	flagSearch := sourceIncludeSearchFromFlags(flags)
	sourceDir := filepath.ToSlash(filepath.Dir(source))
	if sourceDir == "." {
		sourceDir = ""
	}
	includeRoots := appendUniqueIncludeRoots([]string{sourceDir}, s.includeRoots...)
	includeRoots = appendUniqueIncludeRoots(includeRoots, flagSearch.includeRoots...)
	return sourceIncludeSearch{
		quoteRoots:   flagSearch.quoteRoots,
		includeRoots: includeRoots,
		systemRoots:  flagSearch.systemRoots,
	}
}

func sourceIncludeSearchFromFlags(flags []string) sourceIncludeSearch {
	search := sourceIncludeSearch{}
	for i := 0; i < len(flags); i++ {
		kind := ""
		path := ""
		switch {
		case flags[i] == "-I" || flags[i] == "-iquote" || flags[i] == "-isystem":
			if i+1 >= len(flags) {
				continue
			}
			kind = flags[i]
			path = flags[i+1]
			i++
		case strings.HasPrefix(flags[i], "-iquote"):
			kind = "-iquote"
			path = strings.TrimPrefix(flags[i], "-iquote")
		case strings.HasPrefix(flags[i], "-isystem"):
			kind = "-isystem"
			path = strings.TrimPrefix(flags[i], "-isystem")
		case strings.HasPrefix(flags[i], "-I"):
			kind = "-I"
			path = strings.TrimPrefix(flags[i], "-I")
		default:
			continue
		}
		root, ok := sourceTreeIncludeRoot(path)
		if !ok {
			continue
		}
		switch kind {
		case "-iquote":
			search.quoteRoots = appendUniqueIncludeRoots(search.quoteRoots, root)
		case "-isystem":
			search.systemRoots = appendUniqueIncludeRoots(search.systemRoots, root)
		default:
			search.includeRoots = appendUniqueIncludeRoots(search.includeRoots, root)
		}
	}
	return search
}

func appendUniqueIncludeRoots(values []string, extra ...string) []string {
	seen := make(map[string]bool, len(values)+len(extra))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range extra {
		if seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	return values
}

func sourceTreeIncludeRoot(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "$(srctree)" {
		return "", true
	}
	if rel, ok := strings.CutPrefix(path, "$(srctree)/"); ok {
		return filepath.ToSlash(rel), true
	}
	return "", false
}

// refsForSource returns the sorted CONFIG_* symbols reachable from source.
// extraIncludeRoots contains tree-relative directories extracted from the
// object's Kbuild -I flags.
func (s *configSourceScanner) refsForSource(source string, extraIncludeRoots []string) []string {
	closure, _ := s.closureForSource(source, extraIncludeRoots)
	return append([]string(nil), closure.refs...)
}

// sourceIncludesForSource returns sorted source-like tree paths reached through
// literal includes. It follows the full include graph so source fragments
// included by a header are also explicit compile-action inputs.
func (s *configSourceScanner) sourceIncludesForSource(source string, extraIncludeRoots []string) []string {
	closure, _ := s.closureForSource(source, extraIncludeRoots)
	return append([]string(nil), closure.sourceIncludes...)
}

func (s *configSourceScanner) exactClosureForSource(source string, extraIncludeRoots []string) (sourceClosure, error) {
	return s.closureForSourceConfig(source, extraIncludeRoots, nil)
}

func (s *configSourceScanner) inputForTreePath(path string) (CompactSourceInput, error) {
	cleaned, ok := cleanSourceTreePath(path)
	if !ok {
		return CompactSourceInput{}, fmt.Errorf("invalid source-tree path %q", path)
	}
	abs, ok := s.absForTreePath(cleaned)
	if !ok {
		return CompactSourceInput{}, fmt.Errorf("source-tree input %q does not exist", cleaned)
	}
	if s.exact {
		digest, err := s.loadExactSourceDigest(abs)
		if err != nil {
			return CompactSourceInput{}, fmt.Errorf("%s: %w", cleaned, err)
		}
		return CompactSourceInput{Path: cleaned, Digest: digest}, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return CompactSourceInput{}, fmt.Errorf("%s: %w", cleaned, err)
	}
	return CompactSourceInput{
		Path:   cleaned,
		Digest: fileContentDigest(data),
	}, nil
}

func (s *configSourceScanner) closureForSource(source string, extraIncludeRoots []string) (sourceClosure, error) {
	return s.closureForSourceConfig(source, extraIncludeRoots, nil)
}

func (s *configSourceScanner) closureForSourceConfig(source string, extraIncludeRoots []string, config *ResolvedConfig) (sourceClosure, error) {
	return s.closureForSourceConfigMode(source, extraIncludeRoots, config, isAssemblySourcePath(source))
}

func (s *configSourceScanner) closureForSourceConfigMode(
	source string,
	extraIncludeRoots []string,
	config *ResolvedConfig,
	assembly bool,
) (sourceClosure, error) {
	return s.closureForSourceConfigInputs(source, extraIncludeRoots, config, assembly, nil)
}

func (s *configSourceScanner) closureForSourceConfigInputs(
	source string,
	extraIncludeRoots []string,
	config *ResolvedConfig,
	assembly bool,
	providedIncludes []string,
) (sourceClosure, error) {
	return s.closureForSourceConfigInputsProfile(
		source,
		extraIncludeRoots,
		config,
		assembly,
		providedIncludes,
		sourceScanKernel,
	)
}

func (s *configSourceScanner) closureForSourceConfigProfile(
	source string,
	extraIncludeRoots []string,
	config *ResolvedConfig,
	profile sourceScanProfile,
) (sourceClosure, error) {
	return s.closureForSourceConfigInputsProfile(
		source,
		extraIncludeRoots,
		config,
		isAssemblySourcePath(source),
		nil,
		profile,
	)
}

func (s *configSourceScanner) closureForSourceConfigInputsProfile(
	source string,
	extraIncludeRoots []string,
	config *ResolvedConfig,
	assembly bool,
	providedIncludes []string,
	profile sourceScanProfile,
) (sourceClosure, error) {
	if s == nil {
		return sourceClosure{}, nil
	}
	search := sourceIncludeSearch{
		includeRoots: append(append([]string(nil), s.includeRoots...), extraIncludeRoots...),
	}
	return s.closureForSourceConfigInputsSearchProfile(
		source,
		search,
		config,
		assembly,
		providedIncludes,
		profile,
	)
}

func (s *configSourceScanner) closureForSourceConfigInputsSearchProfile(
	source string,
	search sourceIncludeSearch,
	config *ResolvedConfig,
	assembly bool,
	providedIncludes []string,
	profile sourceScanProfile,
) (sourceClosure, error) {
	if s == nil || source == "" {
		if s != nil && s.exact {
			return sourceClosure{}, fmt.Errorf("exact input scan requires a source path")
		}
		return sourceClosure{}, nil
	}
	key := source + "\x00" + search.cacheKey()
	if s.exact && config != nil {
		key += "\x00" + s.configID(config)
	}
	if s.exact {
		key += "\x00assembly=" + strconv.FormatBool(assembly)
		key += "\x00profile=" + string(profile)
		if len(providedIncludes) != 0 {
			providedIncludes = append([]string(nil), providedIncludes...)
			sort.Strings(providedIncludes)
			key += "\x00provided=" + strings.Join(providedIncludes, "\x00")
		}
	}
	if cached, ok := s.closure[key]; ok {
		return cached, s.closureErrs[key]
	}
	rawSource := source
	source, ok := cleanSourceTreePath(source)
	if !ok {
		s.closure[key] = sourceClosure{}
		if s.exact {
			err := fmt.Errorf("invalid source-tree path %q", rawSource)
			s.closureErrs[key] = err
			return sourceClosure{}, err
		}
		return sourceClosure{}, nil
	}
	_, ok = s.absForTreePath(source)
	if !ok {
		s.closure[key] = sourceClosure{}
		if s.exact {
			err := fmt.Errorf("source-tree input %q does not exist", source)
			s.closureErrs[key] = err
			return sourceClosure{}, err
		}
		return sourceClosure{}, nil
	}
	refset := map[string]bool{}
	includeSet := map[string]bool{}
	generatedIncludeSet := map[string]bool{}
	inputs := map[string]CompactSourceInput{}
	provided := make(map[string]bool, len(providedIncludes))
	for _, include := range providedIncludes {
		provided[filepath.ToSlash(include)] = true
	}
	visited := map[sourceClosureStackEntry]bool{}
	stack := []sourceClosureStackEntry{{treePath: source}}
	for len(stack) > 0 {
		entry := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[entry] {
			continue
		}
		visited[entry] = true
		treePath := entry.treePath
		abs, ok := s.absForTreePath(treePath)
		if !ok {
			continue
		}
		scanned, err := s.scanFile(abs, treePath, search, config, assembly, profile)
		if err != nil {
			if !s.exact {
				continue
			}
			err = fmt.Errorf("%s: %w", treePath, err)
			s.closure[key] = sourceClosure{}
			s.closureErrs[key] = err
			return sourceClosure{}, err
		}
		if s.exact {
			inputs[treePath] = CompactSourceInput{Path: treePath, Digest: scanned.digest}
		}
		for _, ref := range scanned.refs {
			refset[ref] = true
		}
		linuxLibfdtEnvironmentIncluded := false
		for _, inc := range scanned.includes {
			if s.exact && !inc.potentiallyActive {
				continue
			}
			if !inc.literal {
				if modeledRecursiveTemplateInclude(treePath, inc.spelling) {
					continue
				}
				if !s.exact {
					continue
				}
				err := fmt.Errorf(
					"%s:%d: unresolved potentially-active nonliteral include %s",
					treePath,
					inc.line,
					inc.spelling,
				)
				s.closure[key] = sourceClosure{}
				s.closureErrs[key] = err
				return sourceClosure{}, err
			}
			// Linux wrappers define LIBFDT_ENV_H before entering the vendored
			// libfdt subtree, so its userspace environment header is inactive.
			if entry.linuxLibfdtEnvironmentGuarded &&
				strings.HasPrefix(treePath, "scripts/dtc/libfdt/") &&
				filepath.ToSlash(strings.TrimSpace(inc.path)) == "libfdt_env.h" {
				continue
			}
			resolvedIncludes := s.resolveInclude(treePath, inc.path, inc.kind, search)
			if len(resolvedIncludes) == 0 {
				includePath := filepath.ToSlash(strings.TrimSpace(inc.path))
				if provided[includePath] {
					continue
				}
				if generatedHeaderInclude(includePath) {
					generatedIncludeSet[includePath] = true
					continue
				}
				if s.exact && !compilerProvidedInclude(includePath) {
					err := fmt.Errorf(
						"%s:%d: unresolved potentially-active literal include %s",
						treePath,
						inc.line,
						inc.spelling,
					)
					s.closure[key] = sourceClosure{}
					s.closureErrs[key] = err
					return sourceClosure{}, err
				}
				continue
			}
			if includeUsesGeneratedAsmWrapper(inc.path, resolvedIncludes) {
				generatedIncludeSet[filepath.ToSlash(strings.TrimSpace(inc.path))] = true
			}
			for _, resolved := range resolvedIncludes {
				if linuxLibfdtEnvironmentWrapper(treePath) &&
					resolved == "include/linux/libfdt_env.h" {
					linuxLibfdtEnvironmentIncluded = true
				}
				if resolved != source && isSourceLikeInclude(resolved) {
					includeSet[resolved] = true
				}
				child := sourceClosureStackEntry{
					treePath: resolved,
					linuxLibfdtEnvironmentGuarded: strings.HasPrefix(
						resolved,
						"scripts/dtc/libfdt/",
					) && (entry.linuxLibfdtEnvironmentGuarded ||
						linuxLibfdtEnvironmentIncluded),
				}
				if !visited[child] {
					stack = append(stack, child)
				}
			}
		}
	}
	out := make([]string, 0, len(refset))
	for ref := range refset {
		out = append(out, ref)
	}
	sort.Strings(out)
	includes := make([]string, 0, len(includeSet))
	for include := range includeSet {
		includes = append(includes, include)
	}
	sort.Strings(includes)
	generatedIncludes := sortedStringSet(generatedIncludeSet)
	sourceInputs := make([]CompactSourceInput, 0, len(inputs))
	for _, input := range inputs {
		sourceInputs = append(sourceInputs, input)
	}
	sort.Slice(sourceInputs, func(i, j int) bool {
		return sourceInputs[i].Path < sourceInputs[j].Path
	})
	result := sourceClosure{
		refs:              out,
		sourceIncludes:    includes,
		generatedIncludes: generatedIncludes,
		sourceInputs:      sourceInputs,
	}
	s.closure[key] = result
	return result, nil
}

func isSourceLikeInclude(path string) bool {
	return strings.HasSuffix(path, ".c") ||
		strings.HasSuffix(path, ".S") ||
		strings.HasSuffix(path, ".s") ||
		strings.HasSuffix(path, ".inc")
}

// refsForSourceDir returns the union of CONFIG_* footprints of every C and
// assembly source directly under dir. Special image objects compile multiple
// payload sources inside one action, so scanning only their nominal primary
// source would be incomplete.
func (s *configSourceScanner) refsForSourceDir(dir string) []string {
	closure, _ := s.closureForSourceDir(dir)
	return closure.refs
}

func (s *configSourceScanner) exactClosureForSourceDir(dir string) (sourceClosure, error) {
	return s.closureForSourceDir(dir)
}

func (s *configSourceScanner) closureForSourceDir(dir string) (sourceClosure, error) {
	return s.closureForSourceDirConfig(dir, nil)
}

func (s *configSourceScanner) closureForSourceDirConfig(dir string, config *ResolvedConfig) (sourceClosure, error) {
	return s.closureForSourceDirConfigProfile(dir, config, sourceScanKernel)
}

func (s *configSourceScanner) closureForSourceDirConfigProfile(
	dir string,
	config *ResolvedConfig,
	profile sourceScanProfile,
) (sourceClosure, error) {
	if s == nil {
		return sourceClosure{}, nil
	}
	rawDir := dir
	dir, ok := cleanSourceTreePath(dir)
	if !ok {
		if s.exact {
			return sourceClosure{}, fmt.Errorf("invalid source-tree directory %q", rawDir)
		}
		return sourceClosure{}, nil
	}
	base, ok := s.absForTreeDirectory(dir)
	if !ok {
		if s.exact {
			return sourceClosure{}, fmt.Errorf("source-tree input directory %q does not exist", dir)
		}
		return sourceClosure{}, nil
	}
	refset := map[string]bool{}
	includeSet := map[string]bool{}
	generatedIncludeSet := map[string]bool{}
	inputs := map[string]CompactSourceInput{}
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		switch filepath.Ext(entry.Name()) {
		case ".c", ".S", ".s":
			rel, relErr := filepath.Rel(base, path)
			if relErr != nil {
				return relErr
			}
			treePath := filepath.ToSlash(filepath.Join(dir, rel))
			closure, closureErr := s.closureForSourceConfigProfile(treePath, nil, config, profile)
			if closureErr != nil {
				if !s.exact {
					return nil
				}
				return closureErr
			}
			for _, ref := range closure.refs {
				refset[ref] = true
			}
			for _, include := range closure.sourceIncludes {
				includeSet[include] = true
			}
			for _, include := range closure.generatedIncludes {
				generatedIncludeSet[include] = true
			}
			for _, input := range closure.sourceInputs {
				inputs[input.Path] = input
			}
		}
		return nil
	})
	if err != nil {
		if !s.exact {
			return sourceClosure{refs: sortedStringSet(refset)}, nil
		}
		return sourceClosure{}, err
	}
	out := make([]string, 0, len(refset))
	for ref := range refset {
		out = append(out, ref)
	}
	sort.Strings(out)
	includes := make([]string, 0, len(includeSet))
	for include := range includeSet {
		includes = append(includes, include)
	}
	sort.Strings(includes)
	generatedIncludes := sortedStringSet(generatedIncludeSet)
	sourceInputs := make([]CompactSourceInput, 0, len(inputs))
	for _, input := range inputs {
		sourceInputs = append(sourceInputs, input)
	}
	sort.Slice(sourceInputs, func(i, j int) bool {
		return sourceInputs[i].Path < sourceInputs[j].Path
	})
	return sourceClosure{
		refs:              out,
		sourceIncludes:    includes,
		generatedIncludes: generatedIncludes,
		sourceInputs:      sourceInputs,
	}, nil
}

// scanFile returns CONFIG_* tokens, include directives, and a content digest.
func (s *configSourceScanner) scanFile(
	abs string,
	treePath string,
	search sourceIncludeSearch,
	config *ResolvedConfig,
	assembly bool,
	profile sourceScanProfile,
) (scannedSourceFile, error) {
	cacheKey := abs
	if s.exact && config != nil {
		cacheKey += "\x00" + s.configID(config)
	}
	if s.exact {
		cacheKey += "\x00tree-path=" + treePath
		cacheKey += "\x00assembly=" + strconv.FormatBool(assembly)
		cacheKey += "\x00profile=" + string(profile)
		cacheKey += "\x00include-search=" + search.cacheKey()
	}
	if cached, ok := s.files[cacheKey]; ok {
		return cached, s.fileErrors[cacheKey]
	}
	var (
		digest string
		lines  []sourceLogicalLine
		text   string
	)
	if s.exact {
		file, err := s.loadExactSourceFile(abs)
		if err != nil {
			s.fileErrors[cacheKey] = err
			return scannedSourceFile{}, err
		}
		digest = file.digest
		lines = file.lines
	} else {
		data, err := os.ReadFile(abs)
		if err != nil {
			s.fileErrors[cacheKey] = err
			return scannedSourceFile{}, err
		}
		text = string(data)
		lines = rawSourceLines(text)
		digest = fileContentDigest(data)
	}
	scanned := scannedSourceFile{digest: digest}
	refset := map[string]bool{}
	if !s.exact {
		for _, ref := range configRefs(text) {
			refset[ref] = true
		}
	}
	predefined := make(map[string]bool, len(s.predefined)+1)
	for symbol, defined := range s.predefined {
		predefined[symbol] = defined
	}
	for symbol, defined := range sourceProfilePredefinedSymbols(profile) {
		predefined[symbol] = defined
	}
	if config != nil {
		for _, binding := range sourceConfigPredefinedSymbols {
			predefined[binding.predefined] =
				config.ShouldWrite(binding.config) && config.Value(binding.config) == "y"
		}
	}
	predefined["__ASSEMBLY__"] = assembly
	predefined["__ASSEMBLER__"] = assembly
	hasInclude := func(operand string) preprocessorBoolean {
		path, kind, literal := sourceIncludeOperand(operand)
		if !literal {
			return preprocessorUnknown
		}
		if len(s.resolveInclude(treePath, path, kind, search)) != 0 {
			return preprocessorTrue
		}
		if path == "cet.h" {
			return preprocessorTrue
		}
		if generatedHeaderInclude(path) || compilerProvidedInclude(path) {
			return preprocessorUnknown
		}
		return preprocessorFalse
	}
	addRefs := func(value string) {
		for _, ref := range configRefs(value) {
			refset[ref] = true
		}
	}
	conditionals := []sourceConditional{}
	potentiallyActive := true
	for _, line := range lines {
		directive, rest, ok := preprocessorDirective(line.text)
		if !ok {
			if s.exact && potentiallyActive {
				addRefs(line.text)
			}
			continue
		}
		switch directive {
		case "if":
			if s.exact && potentiallyActive {
				addRefs(rest)
			}
			mayBeTrue, definitelyTrue := preprocessorCondition(rest, config, predefined, hasInclude)
			conditionals = append(conditionals, sourceConditional{
				parentPotentiallyActive: potentiallyActive,
				definitelyTaken:         potentiallyActive && definitelyTrue,
			})
			potentiallyActive = potentiallyActive && mayBeTrue
		case "ifdef", "ifndef":
			if s.exact && potentiallyActive {
				addRefs(rest)
			}
			defined, known := preprocessorSymbolDefined(strings.Fields(rest), config, predefined)
			mayBeTrue := true
			definitelyTrue := false
			if known {
				if directive == "ifndef" {
					defined = !defined
				}
				mayBeTrue = defined
				definitelyTrue = defined
			}
			conditionals = append(conditionals, sourceConditional{
				parentPotentiallyActive: potentiallyActive,
				definitelyTaken:         potentiallyActive && definitelyTrue,
			})
			potentiallyActive = potentiallyActive && mayBeTrue
		case "elif":
			if len(conditionals) == 0 {
				continue
			}
			frame := &conditionals[len(conditionals)-1]
			conditionPotentiallyEvaluated := frame.parentPotentiallyActive && !frame.definitelyTaken
			if s.exact && conditionPotentiallyEvaluated {
				addRefs(rest)
			}
			mayBeTrue, definitelyTrue := preprocessorCondition(rest, config, predefined, hasInclude)
			potentiallyActive = conditionPotentiallyEvaluated && mayBeTrue
			if potentiallyActive && definitelyTrue {
				frame.definitelyTaken = true
			}
		case "else":
			if len(conditionals) == 0 {
				continue
			}
			frame := &conditionals[len(conditionals)-1]
			potentiallyActive = frame.parentPotentiallyActive && !frame.definitelyTaken
			frame.definitelyTaken = frame.parentPotentiallyActive
		case "endif":
			if len(conditionals) == 0 {
				continue
			}
			frame := conditionals[len(conditionals)-1]
			conditionals = conditionals[:len(conditionals)-1]
			potentiallyActive = frame.parentPotentiallyActive
		case "include", "include_next":
			if s.exact && potentiallyActive {
				addRefs(rest)
			}
			path, kind, literal := sourceIncludeOperand(rest)
			if directive == "include_next" {
				kind = sourceIncludeNext
			}
			if !literal {
				path, literal = configIncludePath(rest, config)
				if literal {
					kind = sourceIncludeQuoted
				}
			}
			scanned.includes = append(scanned.includes, sourceIncludeDirective{
				line:              line.line,
				path:              path,
				spelling:          strings.TrimSpace(rest),
				kind:              kind,
				literal:           literal,
				potentiallyActive: potentiallyActive,
			})
		default:
			if s.exact && potentiallyActive {
				addRefs(line.text)
			}
		}
	}
	scanned.refs = sortedStringSet(refset)
	s.files[cacheKey] = scanned
	return scanned, nil
}

func (s *configSourceScanner) loadExactSourceFile(abs string) (exactSourceFile, error) {
	if cached, ok := s.sourceCache.exactFiles[abs]; ok && cached.preprocessed {
		return cached, s.sourceCache.exactErrors[abs]
	}
	if err, ok := s.sourceCache.exactErrors[abs]; ok {
		return exactSourceFile{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		s.sourceCache.exactErrors[abs] = err
		return exactSourceFile{}, err
	}
	lines, _ := preprocessExactSource(string(data))
	file := exactSourceFile{
		lines:        lines,
		digest:       fileContentDigest(data),
		preprocessed: true,
	}
	s.sourceCache.exactFiles[abs] = file
	return file, nil
}

func (s *configSourceScanner) loadExactSourceDigest(abs string) (string, error) {
	if cached, ok := s.sourceCache.exactFiles[abs]; ok && cached.digest != "" {
		return cached.digest, s.sourceCache.exactErrors[abs]
	}
	if err, ok := s.sourceCache.exactErrors[abs]; ok {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		s.sourceCache.exactErrors[abs] = err
		return "", err
	}
	file := s.sourceCache.exactFiles[abs]
	file.digest = fileContentDigest(data)
	s.sourceCache.exactFiles[abs] = file
	return file.digest, nil
}

func (s *configSourceScanner) configID(config *ResolvedConfig) string {
	if config == nil {
		return ""
	}
	if id, ok := s.configIDs[config]; ok {
		return id
	}
	id := newCompactConfigPayload(compactFullConfigFragment(config)).ID
	s.configIDs[config] = id
	return id
}

type sourceConditional struct {
	parentPotentiallyActive bool
	definitelyTaken         bool
}

func preprocessorDirective(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(line[1:])
	if line == "" {
		return "", "", false
	}
	end := 0
	for end < len(line) && ((line[end] >= 'a' && line[end] <= 'z') || (line[end] >= 'A' && line[end] <= 'Z') || line[end] == '_') {
		end++
	}
	if end == 0 {
		return "", "", false
	}
	return line[:end], strings.TrimSpace(line[end:]), true
}

func preprocessorCondition(
	value string,
	config *ResolvedConfig,
	predefined map[string]bool,
	hasInclude func(string) preprocessorBoolean,
) (mayBeTrue, definitelyTrue bool) {
	value = strings.TrimSpace(strings.SplitN(value, "//", 2)[0])
	if comment := strings.Index(value, "/*"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	result := evaluatePreprocessorBoolean(value, config, predefined, hasInclude)
	switch result {
	case preprocessorFalse:
		return false, false
	case preprocessorTrue:
		return true, true
	default:
		return true, false
	}
}

func preprocessorSymbolDefined(
	fields []string,
	config *ResolvedConfig,
	predefined ...map[string]bool,
) (bool, bool) {
	if len(fields) == 0 {
		return false, false
	}
	symbol := strings.TrimSpace(fields[0])
	if symbols := firstPredefinedMap(predefined); symbols != nil {
		if defined, ok := symbols[symbol]; ok {
			return defined, true
		}
	} else if symbol == "__KERNEL__" {
		return true, true
	}
	if config == nil {
		return false, false
	}
	if !strings.HasPrefix(symbol, "CONFIG_") {
		return false, false
	}
	if base, ok := strings.CutSuffix(symbol, "_MODULE"); ok {
		return config.ShouldWrite(base) && config.Value(base) == "m", true
	}
	value := config.Value(symbol)
	return config.ShouldWrite(symbol) && value != "n" && value != "m", true
}

type preprocessorBoolean uint8

const (
	preprocessorUnknown preprocessorBoolean = iota
	preprocessorFalse
	preprocessorTrue
)

func evaluatePreprocessorBoolean(
	value string,
	config *ResolvedConfig,
	predefined map[string]bool,
	hasInclude func(string) preprocessorBoolean,
) preprocessorBoolean {
	value = strings.TrimSpace(value)
	for sourceOuterParens(value) {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if parts := splitTopLevelPreprocessor(value, "||"); len(parts) > 1 {
		result := preprocessorFalse
		for _, part := range parts {
			switch evaluatePreprocessorBoolean(part, config, predefined, hasInclude) {
			case preprocessorTrue:
				return preprocessorTrue
			case preprocessorUnknown:
				result = preprocessorUnknown
			}
		}
		return result
	}
	if parts := splitTopLevelPreprocessor(value, "&&"); len(parts) > 1 {
		result := preprocessorTrue
		for _, part := range parts {
			switch evaluatePreprocessorBoolean(part, config, predefined, hasInclude) {
			case preprocessorFalse:
				return preprocessorFalse
			case preprocessorUnknown:
				result = preprocessorUnknown
			}
		}
		return result
	}
	if strings.HasPrefix(value, "!") && !strings.HasPrefix(value, "!=") {
		switch evaluatePreprocessorBoolean(strings.TrimSpace(value[1:]), config, predefined, hasInclude) {
		case preprocessorTrue:
			return preprocessorFalse
		case preprocessorFalse:
			return preprocessorTrue
		default:
			return preprocessorUnknown
		}
	}
	if integer, ok := preprocessorInteger(value); ok {
		if integer == 0 {
			return preprocessorFalse
		}
		return preprocessorTrue
	}
	if operand, ok := preprocessorFunctionOperand(value, "__has_include"); ok && hasInclude != nil {
		return hasInclude(operand)
	}
	for _, predicate := range []string{"IS_BUILTIN", "IS_MODULE", "IS_ENABLED", "IS_REACHABLE"} {
		if operand, ok := preprocessorFunctionOperand(value, predicate); ok {
			return evaluateKernelConfigPredicate(predicate, operand, config, predefined)
		}
	}
	symbol := ""
	if strings.HasPrefix(value, "defined") {
		operand := strings.TrimSpace(strings.TrimPrefix(value, "defined"))
		if strings.HasPrefix(operand, "(") && strings.HasSuffix(operand, ")") {
			operand = strings.TrimSpace(operand[1 : len(operand)-1])
		}
		if sourceIdentifier(operand) {
			symbol = operand
		}
	} else if sourceIdentifier(value) {
		symbol = value
	}
	if symbol == "" {
		return preprocessorUnknown
	}
	defined, known := preprocessorSymbolDefined([]string{symbol}, config, predefined)
	if !known {
		return preprocessorUnknown
	}
	if defined {
		return preprocessorTrue
	}
	return preprocessorFalse
}

func evaluateKernelConfigPredicate(
	predicate string,
	operand string,
	config *ResolvedConfig,
	predefined map[string]bool,
) preprocessorBoolean {
	operand = strings.TrimSpace(operand)
	if config == nil || !strings.HasPrefix(operand, "CONFIG_") || !sourceIdentifier(operand) {
		return preprocessorUnknown
	}
	value := config.Value(operand)
	builtin := value == "y"
	module := value == "m"
	var enabled bool
	switch predicate {
	case "IS_BUILTIN":
		enabled = builtin
	case "IS_MODULE":
		enabled = module
	case "IS_ENABLED":
		enabled = builtin || module
	case "IS_REACHABLE":
		moduleAction, known := predefined["MODULE"]
		if !known {
			return preprocessorUnknown
		}
		enabled = builtin || module && moduleAction
	default:
		return preprocessorUnknown
	}
	if enabled {
		return preprocessorTrue
	}
	return preprocessorFalse
}

func preprocessorFunctionOperand(value, name string) (string, bool) {
	if !strings.HasPrefix(value, name) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(value, name))
	if !sourceOuterParens(rest) {
		return "", false
	}
	return strings.TrimSpace(rest[1 : len(rest)-1]), true
}

func firstPredefinedMap(values []map[string]bool) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func sourceOuterParens(value string) bool {
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return false
	}
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(value)-1 {
				return false
			}
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func splitTopLevelPreprocessor(value, operator string) []string {
	depth := 0
	start := 0
	var out []string
	for i := 0; i+len(operator) <= len(value); i++ {
		switch value[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && strings.HasPrefix(value[i:], operator) {
			out = append(out, strings.TrimSpace(value[start:i]))
			i += len(operator) - 1
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil
	}
	out = append(out, strings.TrimSpace(value[start:]))
	return out
}

func preprocessorInteger(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	for len(value) > 0 {
		last := value[len(value)-1]
		if last != 'u' && last != 'U' && last != 'l' && last != 'L' {
			break
		}
		value = value[:len(value)-1]
	}
	integer, err := strconv.ParseInt(value, 0, 64)
	return integer, err == nil
}

func sourceIdentifier(value string) bool {
	if value == "" || !sourceIdentifierStart(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !sourceIdentifierContinue(value[i]) {
			return false
		}
	}
	return true
}

func sourceIdentifierStart(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func sourceIdentifierContinue(value byte) bool {
	return sourceIdentifierStart(value) || value >= '0' && value <= '9'
}

func sourcePredefinedSymbols(srcarch string) map[string]bool {
	// ACPICA's aclinux.h defines its standard-header mode only in the
	// !__KERNEL__ branch. Record that kernel-action consequence explicitly so
	// scanning acenv.h does not treat libc includes as potentially active.
	symbols := map[string]bool{
		"__KERNEL__":                true,
		"__GNUC__":                  true,
		"__clang__":                 true,
		"__ELF__":                   true,
		"__has_include":             true,
		"__linux__":                 true,
		"linux":                     true,
		"_MSC_VER":                  false,
		"_WIN32":                    false,
		"__APPLE__":                 false,
		"_APPLE":                    false,
		"__DragonFly__":             false,
		"__FreeBSD__":               false,
		"__FreeBSD_kernel__":        false,
		"__NetBSD__":                false,
		"__sun":                     false,
		"__CYGWIN__":                false,
		"ACPI_APPLICATION":          false,
		"ACPI_ASL_COMPILER":         false,
		"ACPI_DISASSEMBLER":         false,
		"ACPI_EXEC_APP":             false,
		"ACPI_LIBRARY":              false,
		"ACPI_USE_SYSTEM_CLIBRARY":  true,
		"ACPI_USE_STANDARD_HEADERS": false,
		"MODULE":                    false,
	}
	switch srcarch {
	case "x86":
		symbols["__x86_64__"] = true
		symbols["__amd64__"] = true
		symbols["__aarch64__"] = false
		symbols["__arm__"] = false
		symbols["__ILP32__"] = false
	case "arm64":
		symbols["__x86_64__"] = false
		symbols["__amd64__"] = false
		symbols["__aarch64__"] = true
		symbols["__arm__"] = false
		symbols["__ILP32__"] = false
	}
	return symbols
}

func sourceProfilePredefinedSymbols(profile sourceScanProfile) map[string]bool {
	switch profile {
	case sourceScanKernelModule:
		return map[string]bool{
			"MODULE": true,
		}
	case sourceScanArm64VDSO:
		return map[string]bool{
			"BUILD_VDSO":               true,
			"DISABLE_BRANCH_PROFILING": true,
		}
	case sourceScanArm32CompatVDSO:
		return map[string]bool{
			"__aarch64__":              false,
			"__arm__":                  true,
			"__ILP32__":                true,
			"BUILD_VDSO":               true,
			"DISABLE_BRANCH_PROFILING": true,
		}
	default:
		return nil
	}
}

func configIncludePath(operand string, config *ResolvedConfig) (string, bool) {
	fields := strings.Fields(operand)
	if len(fields) != 1 || config == nil {
		return "", false
	}
	symbol := fields[0]
	if !strings.HasPrefix(symbol, "CONFIG_") || !config.ShouldWrite(symbol) {
		return "", false
	}
	value := strings.TrimSpace(config.Value(symbol))
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	path, err := strconv.Unquote(value)
	if err != nil || path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", false
	}
	return filepath.ToSlash(path), true
}

func modeledRecursiveTemplateInclude(treePath, operand string) bool {
	return strings.HasPrefix(treePath, "include/trace/") &&
		strings.TrimSpace(operand) == "TRACE_INCLUDE(TRACE_INCLUDE_FILE)"
}

func generatedHeaderInclude(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if strings.HasPrefix(path, "generated/") ||
		strings.HasPrefix(path, "uapi/generated/") ||
		strings.HasPrefix(path, "asm/") ||
		strings.HasPrefix(path, "uapi/asm/") {
		return true
	}
	switch path {
	case "kvm-asm-offsets.h", "linux/version.h", "linux/utsrelease.h":
		return true
	default:
		return false
	}
}

func compilerProvidedInclude(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	switch path {
	case "float.h", "iso646.h", "limits.h", "stdalign.h", "stdarg.h",
		"stdatomic.h", "stdbool.h", "stddef.h", "stdint.h", "stdnoreturn.h",
		"cet.h":
		return true
	default:
		return false
	}
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stripCComments(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	inBlock := false
	inString := byte(0)
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inBlock {
			if c == '*' && i+1 < len(text) && text[i+1] == '/' {
				out.WriteString("  ")
				i++
				inBlock = false
			} else if c == '\n' {
				out.WriteByte('\n')
			} else {
				out.WriteByte(' ')
			}
			continue
		}
		if inString != 0 {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == inString {
				inString = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inString = c
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(text) {
			switch text[i+1] {
			case '*':
				out.WriteString("  ")
				i++
				inBlock = true
				continue
			case '/':
				for i < len(text) && text[i] != '\n' {
					out.WriteByte(' ')
					i++
				}
				if i < len(text) {
					out.WriteByte('\n')
				}
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

func physicalSourceLines(text string) []sourceLogicalLine {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	physical := strings.Split(text, "\n")
	out := make([]sourceLogicalLine, 0, len(physical))
	for i, line := range physical {
		out = append(out, sourceLogicalLine{line: i + 1, text: line})
	}
	return out
}

func rawSourceLines(text string) []sourceLogicalLine {
	physical := strings.Split(text, "\n")
	out := make([]sourceLogicalLine, 0, len(physical))
	for i, line := range physical {
		out = append(out, sourceLogicalLine{line: i + 1, text: line})
	}
	return out
}

func preprocessExactSource(text string) ([]sourceLogicalLine, string) {
	physical := physicalSourceLines(text)
	logical := make([]sourceLogicalLine, 0, len(physical))
	var current strings.Builder
	startLine := 1
	continuing := false
	for i, line := range physical {
		if !continuing {
			startLine = line.line
		}
		if strings.HasSuffix(line.text, "\\") && i+1 < len(physical) {
			current.WriteString(strings.TrimSuffix(line.text, "\\"))
			continuing = true
			continue
		}
		current.WriteString(line.text)
		logical = append(logical, sourceLogicalLine{
			line: startLine,
			text: current.String(),
		})
		current.Reset()
		continuing = false
	}
	joined := make([]string, len(logical))
	for i := range logical {
		joined[i] = logical[i].text
	}
	processed := stripCComments(strings.Join(joined, "\n"))
	processedLines := strings.Split(processed, "\n")
	for i := range logical {
		if i < len(processedLines) {
			logical[i].text = processedLines[i]
		} else {
			logical[i].text = ""
		}
	}
	return logical, processed
}

func fileContentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// resolveInclude preserves the v0.0.12 scanner's conservative all-match
// behavior, while exact schemas stop at the first source-tree file in compiler
// search order.
func (s *configSourceScanner) resolveInclude(
	fromTreePath string,
	inc string,
	kind sourceIncludeKind,
	search sourceIncludeSearch,
) []string {
	inc = filepath.ToSlash(strings.TrimSpace(inc))
	if inc == "" {
		return nil
	}
	if !s.exact {
		return s.resolveLegacyIncludes(fromTreePath, inc, search.roots(kind))
	}
	roots := search.roots(kind)
	startRoot := 0
	if kind == sourceIncludeNext {
		startRoot = includeNextRootIndex(fromTreePath, roots)
	}
	try := func(treePath string) (string, bool) {
		treePath, ok := cleanSourceTreePath(treePath)
		if !ok {
			return "", false
		}
		if _, ok := s.absForTreePath(treePath); !ok {
			return "", false
		}
		return treePath, true
	}
	if kind == sourceIncludeQuoted {
		local := filepath.ToSlash(filepath.Join(filepath.Dir(fromTreePath), filepath.FromSlash(inc)))
		if resolved, ok := try(local); ok {
			return []string{resolved}
		}
	}
	for _, candidate := range includeCandidates(inc) {
		for _, root := range roots[startRoot:] {
			treePath := candidate
			if root != "" {
				treePath = root + "/" + candidate
			}
			if resolved, ok := try(treePath); ok {
				return []string{resolved}
			}
		}
	}
	return nil
}

func (s *configSourceScanner) resolveLegacyIncludes(fromTreePath, inc string, roots []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(treePath string) {
		treePath, ok := cleanSourceTreePath(treePath)
		if !ok || seen[treePath] {
			return
		}
		if _, ok := s.absForTreePath(treePath); !ok {
			return
		}
		seen[treePath] = true
		out = append(out, treePath)
	}
	if fromTreePath != "" {
		add(filepath.ToSlash(filepath.Join(filepath.Dir(fromTreePath), filepath.FromSlash(inc))))
	}
	for _, candidate := range includeCandidates(inc) {
		for _, root := range roots {
			if root == "" {
				add(candidate)
			} else {
				add(root + "/" + candidate)
			}
		}
	}
	return out
}

func includeNextRootIndex(fromTreePath string, roots []string) int {
	fromTreePath = filepath.ToSlash(fromTreePath)
	best := -1
	bestLength := -1
	for i, root := range roots {
		root = strings.TrimSuffix(filepath.ToSlash(root), "/")
		if root == "" {
			if best < 0 {
				best = i
			}
			continue
		}
		if fromTreePath == root || strings.HasPrefix(fromTreePath, root+"/") {
			if len(root) > bestLength {
				best = i
				bestLength = len(root)
			}
		}
	}
	if best < 0 {
		return 0
	}
	return best + 1
}

// includeCandidates maps generated per-arch asm wrappers to their source-tree
// asm-generic counterparts as an additional conservative scan target.
func includeCandidates(inc string) []string {
	inc = filepath.ToSlash(inc)
	candidates := []string{inc}
	if rest, ok := strings.CutPrefix(inc, "asm/"); ok {
		candidates = append(candidates, "asm-generic/"+rest)
	} else if rest, ok := strings.CutPrefix(inc, "uapi/asm/"); ok {
		candidates = append(candidates, "uapi/asm-generic/"+rest)
	}
	return candidates
}

func includeUsesGeneratedAsmWrapper(inc string, resolved []string) bool {
	inc = filepath.ToSlash(strings.TrimSpace(inc))
	generic := ""
	if rest, ok := strings.CutPrefix(inc, "asm/"); ok {
		generic = "asm-generic/" + rest
	} else if rest, ok := strings.CutPrefix(inc, "uapi/asm/"); ok {
		generic = "uapi/asm-generic/" + rest
	}
	if generic == "" {
		return false
	}
	for _, path := range resolved {
		path = filepath.ToSlash(path)
		if path == generic || strings.HasSuffix(path, "/"+generic) {
			return true
		}
	}
	return false
}

func linuxLibfdtEnvironmentWrapper(path string) bool {
	path = filepath.ToSlash(path)
	if path == "include/linux/libfdt.h" {
		return true
	}
	return strings.HasPrefix(path, "lib/fdt") && strings.HasSuffix(path, ".c")
}

func (s *configSourceScanner) absForTreePath(path string) (string, bool) {
	path, ok := cleanSourceTreePath(path)
	if !ok {
		return "", false
	}
	if cached, ok := s.sourceCache.treePaths[path]; ok {
		return cached.abs, cached.exists
	}
	resolved := sourceTreePathResolution{}
	if s.sourceRoot != "" {
		abs := filepath.Join(s.sourceRoot, filepath.FromSlash(path))
		if fileExists(abs) {
			resolved = sourceTreePathResolution{abs: abs, exists: true}
			s.sourceCache.treePaths[path] = resolved
			return resolved.abs, resolved.exists
		}
	}
	if mapped, ok := mappedSourceRootPath(path, s.sourceRoots); ok && fileExists(mapped) {
		resolved = sourceTreePathResolution{abs: mapped, exists: true}
	}
	s.sourceCache.treePaths[path] = resolved
	return resolved.abs, resolved.exists
}

func (s *configSourceScanner) absForTreeDirectory(path string) (string, bool) {
	path, ok := cleanSourceTreePath(path)
	if !ok {
		return "", false
	}
	if s.sourceRoot != "" {
		abs := filepath.Join(s.sourceRoot, filepath.FromSlash(path))
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			return abs, true
		}
	}
	if mapped, ok := mappedSourceRootPath(path, s.sourceRoots); ok {
		if info, err := os.Stat(mapped); err == nil && info.IsDir() {
			return mapped, true
		}
	}
	return "", false
}

func cleanSourceTreePath(path string) (string, bool) {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "" || path == "." || path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(filepath.FromSlash(path)) {
		return "", false
	}
	return path, true
}

// includePath extracts a literal quoted or angled C preprocessor include.
func includePath(line string) (string, bool) {
	directive, rest, ok := preprocessorDirective(line)
	if !ok || directive != "include" {
		return "", false
	}
	path, _, ok := sourceIncludeOperand(rest)
	return path, ok
}

func sourceIncludeOperand(rest string) (string, sourceIncludeKind, bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", sourceIncludeNonliteral, false
	}
	var closeByte byte
	kind := sourceIncludeNonliteral
	switch rest[0] {
	case '"':
		closeByte = '"'
		kind = sourceIncludeQuoted
	case '<':
		closeByte = '>'
		kind = sourceIncludeAngled
	default:
		return "", sourceIncludeNonliteral, false
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, closeByte)
	if end < 0 {
		return "", sourceIncludeNonliteral, false
	}
	return rest[:end], kind, true
}

func isAssemblySourcePath(path string) bool {
	return strings.HasSuffix(path, ".S") || strings.HasSuffix(path, ".s")
}
