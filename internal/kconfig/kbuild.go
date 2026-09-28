package kconfig

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type KbuildFile struct {
	Generated       []KbuildTarget
	Includes        []KbuildInclude
	Rules           []KbuildRule
	TargetVariables []KbuildTargetVariable
	// Variables is the fully expanded final Make variable environment when the
	// caller requests a profile snapshot. It is intentionally opt-in because a
	// parsed Make invocation contains many intermediate helper variables that
	// are irrelevant to source-derived action planning.
	Variables         map[string]string
	exportedVariables map[string]string
	evaluator         *kbuildTargetEvaluator
}

// ExportedEnvironment returns the exact variables exported by the parsed Make
// invocation. Values are expanded from the source-owned Make state, but probe
// atoms remain symbolic so a downstream Kconfig evaluation can retain their
// dependencies in the same discovery DAG.
func (f *KbuildFile) ExportedEnvironment() map[string]string {
	if f == nil {
		return nil
	}
	environment := make(map[string]string, len(f.exportedVariables))
	for name, value := range f.exportedVariables {
		environment[name] = value
	}
	return environment
}

type KbuildTarget struct {
	Kind      string
	Target    string
	Condition KbuildCondition
	Position  Position
}

type KbuildInclude struct {
	Path     string
	Optional bool
	Position Position
}

type KbuildRule struct {
	Targets       []string
	TargetPattern string
	Separator     string
	Prerequisites []string
	OrderOnly     []string
	Recipe        []string
	Condition     KbuildCondition
	Position      Position
}

type KbuildTargetVariable struct {
	Targets   []string
	Variable  string
	Operator  string
	Value     string
	Modifiers []string
	Position  Position
	rawValue  string
}

type KbuildCondition struct {
	Kind       string
	Symbol     string
	State      string
	Conditions []KbuildCondition
}

// KbuildVirtualFileView exposes a lazy, Make-visible filesystem snapshot.
// Match returns the slash-normalized paths matching pattern. Read distinguishes
// an absent path from a visible path whose contents are unknown: exact is only
// meaningful when exists is true. Read returns an error when one Make-visible
// alias resolves to conflicting exact producers.
//
// The parser retains the view in captured target evaluators. Implementations
// must therefore remain valid, immutable, and safe for concurrent calls for the
// lifetime of every evaluator derived from the parse.
type KbuildVirtualFileView interface {
	Match(pattern string) []string
	Read(path string) (content string, exists, exact bool, err error)
}

// KbuildVariableBase is an immutable snapshot of the variables shared by a
// family of Kbuild invocations. Construct it once and reuse it through
// KbuildOptions.VariableBase; Variables then contains only invocation-local
// overrides. The snapshot is safe for concurrent parses and does not retain or
// mutate the caller's map.
type KbuildVariableBase struct {
	variables *kbuildInitialVariables
}

// NewKbuildVariableBase snapshots and normalizes variables for reuse by
// multiple Kbuild parses. Path-valued variables receive the same normalization
// as ordinary KbuildOptions.Variables.
func NewKbuildVariableBase(variables map[string]string) *KbuildVariableBase {
	return &KbuildVariableBase{variables: newKbuildInitialVariables(variables)}
}

// NewKbuildVariableBaseWithRecursiveMakeDefault validates an ordinary
// invocation environment and installs the evaluator-owned recursive Make
// capability only when the caller did not configure MAKE. Keeping capability
// creation in this constructor lets source-derived child overlays propagate a
// genuine $(MAKE) value without treating arbitrary configured bytes as trusted.
func NewKbuildVariableBaseWithRecursiveMakeDefault(variables map[string]string) (*KbuildVariableBase, error) {
	if err := ValidateKbuildOrdinaryVariables("Kbuild invocation", variables); err != nil {
		return nil, err
	}
	base := NewKbuildVariableBase(variables)
	if _, configured := base.variables.lookup("MAKE"); !configured {
		base.variables = base.variables.withOverrides(map[string]string{
			"MAKE": CompactKbuildRecursiveMakeProvenanceToken,
		})
	}
	return base, nil
}

type KbuildOptions struct {
	RootDir string
	// WorkingDir is the directory in which GNU Make is invoked. It differs
	// from the directory containing a -f driver for invocations such as
	// tools/build/Makefile.build.
	WorkingDir  string
	SourceRoots map[string]string
	// VirtualFileView is the immutable lazy view of files produced by completed
	// predecessor invocations. Its wildcard matches are merged with physical
	// files. Reads consult it first, so an opaque generated file cannot fall
	// through to a stale physical object tree.
	VirtualFileView KbuildVirtualFileView
	// SourceCache optionally shares immutable Kbuild source reads and lexical
	// preprocessing across independent Make evaluations. The cache contains no
	// variables, environment, conditional state, include decisions, shell/probe
	// results, or target evaluators: every ParseKbuildFileTree call replays the
	// cached source program into a fresh parser. Only paths below the immutable
	// roots declared when the cache was constructed are eligible.
	SourceCache *KbuildSourceCache
	// VariableBase is an optional immutable shared variable snapshot. When it is
	// set, Variables is a sparse invocation-local overlay on that base. When it
	// is nil, Variables retains its traditional standalone behavior.
	VariableBase *KbuildVariableBase
	Variables    map[string]string
	// EnvironmentVariables are the exact variables inherited from the parent
	// GNU Make invocation.  They start with environment origin and remain
	// exported to recipes and recursive children unless the parsed Makefiles
	// explicitly unexport them.  Keep this distinct from Variables: callers use
	// that map for resolved CONFIG_* and other evaluator facts which are not, by
	// themselves, process environment.
	EnvironmentVariables map[string]string
	// CommandLineVariables are GNU Make command-line assignments. Ordinary
	// assignments in parsed Makefiles cannot replace them; an explicit
	// `override` assignment can. This is how the planner pins tool selection to
	// Bazel's configured target and execution toolchains.
	CommandLineVariables map[string]string
	// AutoExportCommandLineVariables optionally narrows which command-line
	// variables GNU Make automatically places in recipe environments. Nil uses
	// GNU Make's default (every eligible name). Planner-only precedence pins can
	// supply an explicit subset without pretending those internal capabilities
	// were user command-line assignments.
	AutoExportCommandLineVariables map[string]bool
	MaxIncludeDepth                int
	// ConfigVariablesComplete declares Variables to be the complete resolved
	// CONFIG_* Make environment. Missing CONFIG_* names then expand empty and
	// evaluate as unset instead of being retained as symbolic conditions.
	ConfigVariablesComplete bool
	// MakeVariablesComplete declares Variables to be the complete invocation
	// environment. Undefined ordinary Make variables then expand to empty, as
	// GNU Make does, rather than being retained symbolically for an incomplete
	// diagnostic parse.
	MakeVariablesComplete bool
	// Shell evaluates the deliberately small, hermetic subset of $(shell ...)
	// required by the selected Kbuild invocation. The caller binds it to the
	// selected toolchain; unsupported commands must return an error.
	Shell func(command string) (string, error)
	// shellResultAvailable reports whether the exact, fully expanded command
	// has already completed successfully through Shell's symbolic evaluator.
	// It must be a read-only cache lookup: the parser uses it only to prove that
	// revisiting a recursive variable while classifying a dynamic Make branch
	// cannot introduce a new branch-sensitive shell effect.
	shellResultAvailable func(command string) bool
	// probeEnvironmentIdentity returns the stable identity of the currently
	// activated compiler/source-probe process environment. Target-command
	// resolution uses it only as process-local memo provenance; it is never
	// serialized into the action graph.
	probeEnvironmentIdentity func() string
	// SourceShell lowers a non-tool shell query against the immutable Linux
	// source tree. It is consulted after Shell returns a narrowly typed
	// unhandled-command error, or directly when target replay deliberately
	// disables the general recipe Shell callback. Malformed compiler and source
	// probes cannot fall through to this bounded path. WorkingDirectory is the
	// exact Make invocation cwd selected by the source-derived driver.
	SourceShell func(command, workingDirectory string) (string, error)
	// ResolveSymbolic validates symbolic probe atoms during discovery and
	// resolves them from exact replay results. It is never called while forming
	// a later probe request, so result dependencies remain explicit in the DAG.
	ResolveSymbolic func(string) (string, error)
	// ResolveSymbolicWords resolves a value at a Make word-list boundary.
	// Discovery may unwrap an opaque whole-text node only through its proven
	// word-equivalent protocol form; replay uses the exact expression AST.
	ResolveSymbolicWords func(string) (string, error)
	// ResolveSymbolicStructure selects finite probe-controlled branches while
	// retaining nested probe values as symbolic atoms. Recipe discovery uses
	// this structural replay form to recover the selected shell command shape
	// without concretizing compiler arguments needed by recursive Make.
	ResolveSymbolicStructure func(string) (string, error)
	// SelectSymbolic retains a source-defined comparison against an unresolved
	// probe value.  The returned atom expands to trueText or falseText from the
	// exact result during replay and can itself become a conditional argument of
	// a later probe.  recognized is false when value and expected are ordinary
	// Make text rather than a probe-dependent comparison.
	SelectSymbolic func(value, expected string, equal bool, trueText, falseText string) (selected string, recognized bool, err error)
	// TransformSymbolic retains a pure Make transformation over symbolic
	// arguments. Discovery receives a distinct content-addressed exact AST;
	// replay resolves its inputs and applies the same transformation. A separate
	// protocol proof controls whether the value may cross a process boundary.
	TransformSymbolic func(function string, args []string) (transformed string, recognized bool, err error)
	// CaptureTargetEvaluator retains the parsed variable definitions for exact
	// target-context evaluation in the same planner process.
	CaptureTargetEvaluator bool
	// CaptureVariables expands only explicit outputs needed to derive the next
	// source-owned Make invocation. Arbitrary helper definitions are never
	// serialized or expanded.
	CaptureVariables []string
	// SkipExportedVariables avoids eagerly expanding every export while a
	// caller evaluates a bounded source-derived Make identity.
	SkipExportedVariables bool
}

// KbuildSourceCache is a process-local cache of immutable Kbuild source
// programs. Programs contain only continuation-folded, literal-protected
// source lines and their comment-stripped spelling. They are safe to replay
// against unrelated configs, environments, compiler probes, and object-tree
// views because none of those inputs participate in source tokenization.
//
// The cache is safe for concurrent callers. Its roots are snapshotted by the
// constructor; callers must keep files below those roots immutable for the
// cache's lifetime. Paths below an excluded root always bypass the cache.
type KbuildSourceCache struct {
	mu               sync.Mutex
	roots            []string
	excluded         []string
	resolvedExcluded []string
	paths            map[string]kbuildSourceCachePath
	programs         map[string]kbuildSourceProgram
	reads            int
	hits             int
}

type kbuildSourceCachePath struct {
	resolved string
	eligible bool
}

// KbuildSourceCacheStats reports source-program cache activity. SourceReads is
// the number of eligible files lexed from disk, CacheHits is the number of
// evaluations replayed from an existing program, and Entries is the number of
// retained immutable source programs.
type KbuildSourceCacheStats struct {
	SourceReads int
	CacheHits   int
	Entries     int
}

// NewKbuildSourceCache creates a source cache for immutableRoots. excludedRoots
// are useful when a writable or config-specific object tree is nested below an
// otherwise immutable source root.
func NewKbuildSourceCache(immutableRoots, excludedRoots []string) *KbuildSourceCache {
	return &KbuildSourceCache{
		roots:            canonicalKbuildSourceCacheRoots(immutableRoots),
		excluded:         canonicalKbuildSourceCacheRoots(excludedRoots),
		resolvedExcluded: resolvedKbuildSourceCacheRoots(excludedRoots),
		paths:            map[string]kbuildSourceCachePath{},
		programs:         map[string]kbuildSourceProgram{},
	}
}

// Stats returns a consistent snapshot of this cache's counters.
func (c *KbuildSourceCache) Stats() KbuildSourceCacheStats {
	if c == nil {
		return KbuildSourceCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return KbuildSourceCacheStats{
		SourceReads: c.reads,
		CacheHits:   c.hits,
		Entries:     len(c.programs),
	}
}

func canonicalKbuildSourceCacheRoots(roots []string) []string {
	canonical := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if !seen[absolute] {
			seen[absolute] = true
			canonical = append(canonical, absolute)
		}
	}
	sort.Strings(canonical)
	return canonical
}

func resolvedKbuildSourceCacheRoots(roots []string) []string {
	resolved := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		physical, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		absolute, err := filepath.Abs(physical)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if !seen[absolute] {
			seen[absolute] = true
			resolved = append(resolved, absolute)
		}
	}
	sort.Strings(resolved)
	return resolved
}

func kbuildPathBelowRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (c *KbuildSourceCache) eligible(path string) bool {
	if c == nil {
		return false
	}
	for _, root := range c.excluded {
		if kbuildPathBelowRoot(path, root) {
			return false
		}
	}
	for _, root := range c.roots {
		if kbuildPathBelowRoot(path, root) {
			return true
		}
	}
	return false
}

func (c *KbuildSourceCache) resolvedPathExcluded(path string) bool {
	if c == nil {
		return false
	}
	for _, root := range c.excluded {
		if kbuildPathBelowRoot(path, root) {
			return true
		}
	}
	for _, root := range c.resolvedExcluded {
		if kbuildPathBelowRoot(path, root) {
			return true
		}
	}
	return false
}

func normalizeKbuildSourceRoots(sourceRoots map[string]string) (map[string]string, error) {
	if sourceRoots == nil {
		return nil, nil
	}
	const sourceRoot = "__LINUX_BZL_SOURCE_TREE__"
	markers := make([]string, 0, len(sourceRoots))
	for marker := range sourceRoots {
		markers = append(markers, marker)
	}
	sort.Strings(markers)
	result := make(map[string]string, len(sourceRoots))
	original := make(map[string]string, len(sourceRoots))
	for _, marker := range markers {
		canonicalMarker := marker
		trimmed := filepath.ToSlash(strings.TrimSpace(marker))
		if strings.HasPrefix(trimmed, sourceRoot+"/") {
			cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(trimmed)))
			if !strings.HasPrefix(cleaned, sourceRoot+"/") {
				return nil, fmt.Errorf("nested Kbuild source root %q escapes %s", marker, sourceRoot)
			}
			relative := strings.TrimPrefix(cleaned, sourceRoot+"/")
			canonical, err := canonicalCompactKbuildInvocationPath(relative)
			if err != nil || canonical == "" || canonical == "." {
				if err == nil {
					err = fmt.Errorf("path is empty")
				}
				return nil, fmt.Errorf("nested Kbuild source root %q: %w", marker, err)
			}
			canonicalMarker = sourceRoot + "/" + canonical
		}
		if previous, exists := original[canonicalMarker]; exists {
			return nil, fmt.Errorf(
				"Kbuild source roots %q and %q have duplicate canonical marker %q",
				previous, marker, canonicalMarker,
			)
		}
		original[canonicalMarker] = marker
		result[canonicalMarker] = sourceRoots[marker]
	}
	return result, nil
}

func (opts KbuildOptions) hasVariable(name string) bool {
	if _, ok := opts.Variables[name]; ok {
		return true
	}
	if opts.VariableBase == nil {
		return false
	}
	_, ok := opts.VariableBase.variables.lookup(name)
	return ok
}

func ParseKbuildFileWithOptions(path string, opts KbuildOptions) (*KbuildFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseKbuildWithOptions(file, path, opts, filepath.Dir(path))
}

func ParseKbuildFileTree(path string, opts KbuildOptions) (*KbuildFile, error) {
	return parseKbuildFileTree(path, opts, nil)
}

func parseKbuildFileTree(path string, opts KbuildOptions, variableOverrides map[string]string) (*KbuildFile, error) {
	var err error
	opts.SourceRoots, err = normalizeKbuildSourceRoots(opts.SourceRoots)
	if err != nil {
		return nil, err
	}
	if opts.MaxIncludeDepth == 0 {
		opts.MaxIncludeDepth = 64
	}
	if opts.RootDir != "" {
		if _, overridden := variableOverrides["srctree"]; !overridden {
			if !opts.hasVariable("srctree") {
				if err := ValidateKbuildOrdinaryValue("Kbuild srctree root", opts.RootDir); err != nil {
					return nil, err
				}
				if variableOverrides == nil {
					variableOverrides = map[string]string{}
				}
				variableOverrides["srctree"] = opts.RootDir
			}
		}
	}
	treeParser := &kbuildTreeParser{
		opts:              opts,
		variableOverrides: variableOverrides,
		seen:              map[string]bool{},
		parsing:           map[string]bool{},
	}
	parser := newKbuildParserWithVariableBase(opts.VariableBase, opts.Variables, variableOverrides, "")
	parser.applyEnvironmentVariables(opts.EnvironmentVariables)
	parser.applyCommandLineVariables(opts.CommandLineVariables, opts.AutoExportCommandLineVariables)
	parser.configVariablesComplete = opts.ConfigVariablesComplete
	parser.makeVariablesComplete = opts.MakeVariablesComplete
	parser.shell = opts.Shell
	parser.shellResultAvailable = opts.shellResultAvailable
	parser.probeEnvironmentIdentity = opts.probeEnvironmentIdentity
	parser.sourceShell = opts.SourceShell
	parser.resolveSymbolic = opts.ResolveSymbolic
	parser.resolveSymbolicWords = opts.ResolveSymbolicWords
	parser.resolveSymbolicStructure = opts.ResolveSymbolicStructure
	parser.selectSymbolic = opts.SelectSymbolic
	parser.transformSymbolic = opts.TransformSymbolic
	parser.sourceRoots = opts.SourceRoots
	parser.virtualFileView = opts.VirtualFileView
	parser.workingDir = opts.WorkingDir
	parser.includeFunc = func(includes []KbuildInclude) error {
		return treeParser.parseIncludes(parser, includes)
	}
	if err := treeParser.parseInto(parser, path, 0); err != nil {
		return nil, err
	}
	if !opts.SkipExportedVariables {
		if err := parser.finalizeExportedVariables(); err != nil {
			return nil, err
		}
	}
	if len(opts.CaptureVariables) != 0 {
		if err := parser.finalizeSelectedVariableSnapshot(opts.CaptureVariables); err != nil {
			return nil, err
		}
	}
	if opts.CaptureTargetEvaluator {
		parser.kb.evaluator = newKbuildTargetEvaluator(parser)
	}
	return parser.kb, nil
}

func ParseKbuild(r io.Reader, filename string) (*KbuildFile, error) {
	return parseKbuild(r, filename, nil, "")
}

func parseKbuild(r io.Reader, filename string, vars map[string]string, baseDir string) (*KbuildFile, error) {
	return parseKbuildWithOptions(r, filename, KbuildOptions{Variables: vars}, baseDir)
}

func parseKbuildWithOptions(r io.Reader, filename string, opts KbuildOptions, baseDir string) (*KbuildFile, error) {
	var err error
	opts.SourceRoots, err = normalizeKbuildSourceRoots(opts.SourceRoots)
	if err != nil {
		return nil, err
	}
	parser := newKbuildParserWithVariableBase(opts.VariableBase, opts.Variables, nil, baseDir)
	parser.applyEnvironmentVariables(opts.EnvironmentVariables)
	parser.applyCommandLineVariables(opts.CommandLineVariables, opts.AutoExportCommandLineVariables)
	parser.configVariablesComplete = opts.ConfigVariablesComplete
	parser.makeVariablesComplete = opts.MakeVariablesComplete
	parser.shell = opts.Shell
	parser.shellResultAvailable = opts.shellResultAvailable
	parser.probeEnvironmentIdentity = opts.probeEnvironmentIdentity
	parser.sourceShell = opts.SourceShell
	parser.resolveSymbolic = opts.ResolveSymbolic
	parser.resolveSymbolicWords = opts.ResolveSymbolicWords
	parser.resolveSymbolicStructure = opts.ResolveSymbolicStructure
	parser.selectSymbolic = opts.SelectSymbolic
	parser.transformSymbolic = opts.TransformSymbolic
	parser.sourceRoots = opts.SourceRoots
	parser.virtualFileView = opts.VirtualFileView
	parser.workingDir = opts.WorkingDir
	if err := parser.parseReader(r, filename); err != nil {
		return nil, err
	}
	if !opts.SkipExportedVariables {
		if err := parser.finalizeExportedVariables(); err != nil {
			return nil, err
		}
	}
	if len(opts.CaptureVariables) != 0 {
		if err := parser.finalizeSelectedVariableSnapshot(opts.CaptureVariables); err != nil {
			return nil, err
		}
	}
	if opts.CaptureTargetEvaluator {
		parser.kb.evaluator = newKbuildTargetEvaluator(parser)
	}
	return parser.kb, nil
}

func (p *kbuildParser) finalizeSelectedVariableSnapshot(names []string) error {
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, ok, err := p.expandVariable(name, "$("+name+")", 0)
		if err != nil {
			return fmt.Errorf("expand selected Kbuild variable %s: %w", name, err)
		}
		value, err = p.resolveKbuildSymbolic(value)
		if err != nil {
			return fmt.Errorf("resolve selected Kbuild variable %s: %w", name, err)
		}
		value, _, err = restoreCompactKbuildLiteralActionMarkers(value)
		if err != nil {
			return fmt.Errorf("restore selected Kbuild variable %s literal markers: %w", name, err)
		}
		if ok && value != "" {
			values[name] = value
		}
	}
	p.kb.Variables = values
	return nil
}

func (p *kbuildParser) finalizeExportedVariables() error {
	names := make([]string, 0, len(p.exported))
	for name, exported := range p.exported {
		if exported {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, ok, err := p.expandVariable(name, "$("+name+")", 0)
		if err != nil {
			return err
		}
		if !ok {
			value = ""
		}
		values[name] = value
	}
	p.kb.exportedVariables = values
	return nil
}

type kbuildSourceLine struct {
	text         string
	uncommented  string
	physicalLine int
}

type kbuildSourceProgram struct {
	lines []kbuildSourceLine
}

func kbuildSourceLineForLogical(text, filename string, physicalLine int) (kbuildSourceLine, error) {
	protected, err := protectCompactKbuildSourceLiteralActionMarkers(text)
	if err != nil {
		return kbuildSourceLine{}, fmt.Errorf("%s:%d: %w", filename, physicalLine, err)
	}
	uncommented := stripKbuildComment(protected)
	if uncommented == protected {
		// Keep one backing string for the overwhelmingly common no-comment case.
		uncommented = protected
	}
	return kbuildSourceLine{
		text:         protected,
		uncommented:  uncommented,
		physicalLine: physicalLine,
	}, nil
}

// parseReaderAndCapture preserves the parser's source-order error and callback
// semantics on a cache miss: each logical line is evaluated as soon as it is
// read. The immutable lexical program is retained only after that independent
// evaluation succeeds.
func (p *kbuildParser) parseReaderAndCapture(r io.Reader, filename string) (kbuildSourceProgram, error) {
	if err := p.appendMakefileList(filename); err != nil {
		return kbuildSourceProgram{}, err
	}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	var logical strings.Builder
	logicalStart := 1
	program := kbuildSourceProgram{}
	for scanner.Scan() {
		lineNo++
		line := strings.TrimRight(scanner.Text(), " \t")
		if logical.Len() == 0 {
			logicalStart = lineNo
		}
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimRight(strings.TrimSuffix(line, "\\"), " \t")
		}
		logical.WriteString(line)
		if continued {
			logical.WriteByte(' ')
			continue
		}
		source, err := kbuildSourceLineForLogical(logical.String(), filename, logicalStart)
		if err != nil {
			return kbuildSourceProgram{}, err
		}
		program.lines = append(program.lines, source)
		if err := p.parseSourceLine(source, Position{Filename: filename, Line: logicalStart}); err != nil {
			return kbuildSourceProgram{}, err
		}
		logical.Reset()
	}
	if err := scanner.Err(); err != nil {
		return kbuildSourceProgram{}, err
	}
	if logical.Len() != 0 {
		source, err := kbuildSourceLineForLogical(logical.String(), filename, logicalStart)
		if err != nil {
			return kbuildSourceProgram{}, err
		}
		program.lines = append(program.lines, source)
		if err := p.parseSourceLine(source, Position{Filename: filename, Line: logicalStart}); err != nil {
			return kbuildSourceProgram{}, err
		}
	}
	if p.defineName != "" {
		return kbuildSourceProgram{}, fmt.Errorf("%s: unterminated define %q", p.definePos, p.defineName)
	}
	return program, nil
}

func (c *KbuildSourceCache) sourceProgram(path string) (kbuildSourceProgram, string, bool, bool, error) {
	if c == nil {
		return kbuildSourceProgram{}, path, false, false, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return kbuildSourceProgram{}, "", false, false, err
	}
	absolute = filepath.Clean(absolute)
	if !c.eligible(absolute) {
		return kbuildSourceProgram{}, absolute, false, false, nil
	}
	c.mu.Lock()
	if cachedPath, ok := c.paths[absolute]; ok {
		if !cachedPath.eligible {
			c.mu.Unlock()
			return kbuildSourceProgram{}, cachedPath.resolved, false, false, nil
		}
		if program, ok := c.programs[cachedPath.resolved]; ok {
			c.hits++
			c.mu.Unlock()
			return program, cachedPath.resolved, true, true, nil
		}
		c.mu.Unlock()
		return kbuildSourceProgram{}, cachedPath.resolved, true, false, nil
	}
	c.mu.Unlock()
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		// Cache eligibility must not replace the parser's ordinary open-path
		// diagnostic for missing, dangling, or looping inputs. Fail closed to the
		// uncached path and let os.Open below report the logical filename.
		return kbuildSourceProgram{}, absolute, false, false, nil
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return kbuildSourceProgram{}, absolute, false, false, nil
	}
	resolved = filepath.Clean(resolved)
	// Bazel commonly presents immutable inputs as a symlink forest whose final
	// files live in a content store outside the declared root. The caller's
	// immutable-root contract covers those targets. Excluded writable roots are
	// different: reject both their declared and resolved spellings so a source
	// symlink cannot smuggle a mutable object-tree file into this cache.
	eligible := !c.resolvedPathExcluded(resolved)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cachedPath, ok := c.paths[absolute]; ok {
		resolved = cachedPath.resolved
		eligible = cachedPath.eligible
	} else {
		c.paths[absolute] = kbuildSourceCachePath{resolved: resolved, eligible: eligible}
	}
	if !eligible {
		return kbuildSourceProgram{}, resolved, false, false, nil
	}
	if program, ok := c.programs[resolved]; ok {
		c.hits++
		return program, resolved, true, true, nil
	}
	return kbuildSourceProgram{}, resolved, true, false, nil
}

func (c *KbuildSourceCache) recordSourceRead() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
}

func (c *KbuildSourceCache) storeSourceProgram(path string, program kbuildSourceProgram) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, exists := c.programs[path]; !exists {
		c.programs[path] = program
	}
	c.mu.Unlock()
}

func (p *kbuildParser) parseSourceProgram(program kbuildSourceProgram, filename string) error {
	if err := p.appendMakefileList(filename); err != nil {
		return err
	}
	for _, line := range program.lines {
		if err := p.parseSourceLine(line, Position{Filename: filename, Line: line.physicalLine}); err != nil {
			return err
		}
	}
	if p.defineName != "" {
		return fmt.Errorf("%s: unterminated define %q", p.definePos, p.defineName)
	}
	return nil
}

func (p *kbuildParser) parseReader(r io.Reader, filename string) error {
	_, err := p.parseReaderAndCapture(r, filename)
	return err
}

func (p *kbuildParser) appendMakefileList(filename string) error {
	filename = filepath.ToSlash(filename)
	if err := ValidateKbuildOrdinaryValue("Kbuild MAKEFILE_LIST filename", filename); err != nil {
		return err
	}
	current := ""
	if variable, ok := p.lookupVariable("MAKEFILE_LIST"); ok {
		current = variable.value
	}
	p.setVariable("MAKEFILE_LIST", kbuildVariable{value: appendMakeValue(current, filename)})
	return nil
}

type kbuildParser struct {
	kb *KbuildFile
	// initialVars is an immutable, persistent variable layer. Evaluator clones
	// share it and applyEnvironmentVariables creates a small overlay instead of
	// copying or mutating the potentially very large invocation environment.
	initialVars *kbuildInitialVariables
	// baseVars is an immutable variable layer used only by target-context
	// evaluators.  Parsing and control-effect evaluation keep it nil and own a
	// complete vars map.  A target evaluation writes only its small overlay,
	// avoiding a full Make-environment clone for every selected object.
	baseVars          map[string]kbuildVariable
	vars              map[string]kbuildVariable
	exported          map[string]bool
	undefined         map[string]bool
	symbolicVariables map[string]kbuildSymbolicVariableState
	// renderedValueProjections preserve GNU Make's logical value when an action
	// evaluator temporarily replaces an invocation variable with a rooted
	// rendering value. Ordinary expansion keeps the rooted spelling selected by
	// the action lowerer; logical identity is restored only where Make observes
	// identity, such as computed variable lookup and filter matching.
	renderedValueProjections []kbuildValueProjection
	locals                   []map[string]string
	expanding                map[string]bool
	conds                    []kbuildConditionalFrame
	baseDir                  string
	workingDir               string
	currentPos               Position
	defineName               string
	defineOp                 string
	definePos                Position
	defineBody               []string
	currentRule              int
	includeFunc              func([]KbuildInclude) error
	includeDepth             int
	shell                    func(command string) (string, error)
	shellResultAvailable     func(command string) bool
	probeEnvironmentIdentity func() string
	sourceShell              func(command, workingDirectory string) (string, error)
	resolveSymbolic          func(string) (string, error)
	resolveSymbolicWords     func(string) (string, error)
	resolveSymbolicStructure func(string) (string, error)
	selectSymbolic           func(value, expected string, equal bool, trueText, falseText string) (string, bool, error)
	transformSymbolic        func(function string, args []string) (string, bool, error)
	// commandSelectionExpansion is installed only while recovering the leaf
	// cmd_<name> calls selected by a source-defined rule_<name> macro. The
	// observer replaces command text with an inert shell word while leaving the
	// ordinary Make evaluator responsible for calls, computed variable names,
	// conditionals, and target-specific values. It is deliberately parse-local
	// and is never captured by a CompactKbuildProfile.
	commandSelectionExpansion func(name, original string, depth int) (string, bool, error)
	// expandedReferencesAreLiteral is enabled only while replaying
	// a source wrapper around already-expanded command leaves. GNU Make does
	// not rescan variable expansion results: shell text such as $(command) in a
	// leaf is ordinary data when an outer conditional tests it or a pure text
	// function transforms it. The ordinary incomplete parser still uses
	// reference-shaped output to signal an unresolved source expression.
	expandedReferencesAreLiteral bool
	configVariablesComplete      bool
	makeVariablesComplete        bool
	commandLineVariables         map[string]bool
	environmentVariables         map[string]bool
	sourceRoots                  map[string]string
	virtualFileView              KbuildVirtualFileView
	// provisionalComputedNames is set only on a target-evaluation clone. A
	// probe-derived automatic target may make a computed variable name unknown
	// during discovery; GNU Make then observes that lookup as undefined until
	// replay binds the concrete target. Invocation parsing has no corresponding
	// replay boundary and must continue to reject such names.
	provisionalComputedNames bool
}

// kbuildInitialVariables is a persistent stack of immutable variable maps.
// The root owns the normalized Kbuild invocation variables and each later
// environment application adds one private override layer. Parser clones can
// therefore share the complete initial environment without either an O(n)
// copy or the risk that a later write contaminates a sibling evaluator.
type kbuildInitialVariables struct {
	parent *kbuildInitialVariables
	values map[string]string
}

func (variables *kbuildInitialVariables) withOverrides(values map[string]string) *kbuildInitialVariables {
	if len(values) == 0 {
		return variables
	}
	overrides := make(map[string]string, len(values))
	for name, value := range values {
		overrides[name] = normalizeKbuildPathVariable(name, value)
	}
	return &kbuildInitialVariables{parent: variables, values: overrides}
}

func (variables *kbuildInitialVariables) lookup(name string) (string, bool) {
	for current := variables; current != nil; current = current.parent {
		if value, ok := current.values[name]; ok {
			return value, true
		}
	}
	return "", false
}

type kbuildValueProjection struct {
	rendered string
	logical  string
}

// kbuildSymbolicVariableState records the finite parts of GNU Make variable
// identity that a guarded assignment may leave uncertain. The flattened value
// lives in vars; these predicates guard definedness, flavor, and origin so
// operations that observe one property can either use an exact invariant or
// fail closed until all branches converge.
type kbuildSymbolicVariableState struct {
	definedWhen     string
	simpleWhen      string
	recursiveWhen   string
	environmentWhen string
	fileWhen        string
}

func exactKbuildSymbolicPredicate(value string) (bool, bool) {
	switch strings.TrimSpace(value) {
	case "":
		return false, true
	case "1":
		return true, true
	default:
		return false, false
	}
}

func (state kbuildSymbolicVariableState) exactDefinedness() (bool, bool) {
	return exactKbuildSymbolicPredicate(state.definedWhen)
}

func (state kbuildSymbolicVariableState) exactFlavor() (string, bool) {
	defined, known := state.exactDefinedness()
	if !known {
		return "", false
	}
	if !defined {
		return "undefined", true
	}
	simple, simpleKnown := exactKbuildSymbolicPredicate(state.simpleWhen)
	recursive, recursiveKnown := exactKbuildSymbolicPredicate(state.recursiveWhen)
	if !simpleKnown || !recursiveKnown || simple == recursive {
		return "", false
	}
	if simple {
		return "simple", true
	}
	return "recursive", true
}

func (state kbuildSymbolicVariableState) exactOrigin() (string, bool) {
	defined, known := state.exactDefinedness()
	if !known {
		return "", false
	}
	if !defined {
		return "undefined", true
	}
	environment, environmentKnown := exactKbuildSymbolicPredicate(state.environmentWhen)
	file, fileKnown := exactKbuildSymbolicPredicate(state.fileWhen)
	if !environmentKnown || !fileKnown || environment == file {
		return "", false
	}
	if environment {
		return "environment", true
	}
	return "file", true
}

func normalizeKbuildSymbolicVariableState(state kbuildSymbolicVariableState) *kbuildSymbolicVariableState {
	defined, definedKnown := state.exactDefinedness()
	if !definedKnown || !defined {
		return &state
	}
	if _, flavorKnown := state.exactFlavor(); !flavorKnown {
		return &state
	}
	if _, originKnown := state.exactOrigin(); !originKnown {
		return &state
	}
	return nil
}

func (p *kbuildParser) applyEnvironmentVariables(values map[string]string) {
	if len(values) == 0 {
		return
	}
	p.initialVars = p.initialVars.withOverrides(values)
	if p.environmentVariables == nil {
		p.environmentVariables = make(map[string]bool, len(values))
	}
	for name := range values {
		p.environmentVariables[name] = true
		// GNU Make re-exports variables inherited from its process environment.
		// A later source assignment changes the value but not that membership;
		// only an explicit unexport directive removes it.
		p.exported[name] = true
	}
}

func (p *kbuildParser) applyCommandLineVariables(values map[string]string, autoExport map[string]bool) {
	if len(values) == 0 {
		return
	}
	if p.commandLineVariables == nil {
		p.commandLineVariables = make(map[string]bool, len(values))
	}
	for name, value := range values {
		// GNU Make's NAME=value command-line assignments have recursive flavor.
		// Keep the raw Make expression here so references are expanded in the
		// context where the value is consumed (including by an immediate source
		// assignment such as ccflags-y := $(SDK)).  Treating the value as an
		// initial/simple variable leaks nested $(...) syntax into recipes.
		p.vars[name] = kbuildVariable{
			value:     normalizeKbuildPathVariable(name, value),
			recursive: name != "MAKECMDGOALS",
		}
		delete(p.undefined, name)
		p.commandLineVariables[name] = true
		// GNU Make automatically places ordinary command-line variables in a
		// recipe's environment. MAKECMDGOALS is invocation state synthesized by
		// Make itself, not a command-line assignment, even though this evaluator
		// carries it through the same precedence layer.
		if name != "MAKECMDGOALS" && (autoExport == nil || autoExport[name]) && kbuildAutomaticEnvironmentName(name) {
			p.exported[name] = true
		}
	}
}

func kbuildAutomaticEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character == '_' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

type kbuildVariable struct {
	value     string
	recursive bool
	// deferredSimple preserves GNU Make's simple flavor while delaying a shell
	// expression until the variable is actually consumed by the selected graph.
	// This prevents disabled Kbuild collections from executing discovery probes.
	deferredSimple bool
}

// These are GNU Make built-ins, not ambient environment variables. The parser
// implements the output-sync-era semantics Linux uses as its Make >= 4.0
// feature gate, and exposes that capability through the same variables GNU
// Make injects before reading the first Makefile. Callers may still override
// them to model a different registered Make frontend.
var kbuildSemanticMakeBuiltins = map[string]string{
	".FEATURES":    "output-sync",
	"MAKE_VERSION": "4.4",
}

type kbuildConditionalFrame struct {
	parentActive            bool
	parentDefinitelyActive  bool
	previousKnown           bool
	previousTaken           bool
	previousCondition       KbuildCondition
	hasPreviousCondition    bool
	active                  bool
	definitelyActive        bool
	condition               KbuildCondition
	hasCondition            bool
	symbolicCondition       string
	symbolicNegated         bool
	previousSymbolic        string
	previousSymbolicNegated bool
	sawElse                 bool
}

func newKbuildParser(vars map[string]string, baseDir string) *kbuildParser {
	return newKbuildParserWithVariableBase(nil, vars, nil, baseDir)
}

func newKbuildInitialVariables(vars map[string]string) *kbuildInitialVariables {
	initial := make(map[string]string, len(vars)+len(kbuildSemanticMakeBuiltins))
	for key, value := range kbuildSemanticMakeBuiltins {
		initial[key] = value
	}
	for key, value := range vars {
		initial[key] = normalizeKbuildPathVariable(key, value)
	}
	return &kbuildInitialVariables{values: initial}
}

func newKbuildParserWithVariableBase(variableBase *KbuildVariableBase, vars, overrides map[string]string, baseDir string) *kbuildParser {
	var initial *kbuildInitialVariables
	if variableBase != nil && variableBase.variables != nil {
		initial = variableBase.variables.withOverrides(vars)
	} else {
		initial = newKbuildInitialVariables(vars)
	}
	local := make(map[string]kbuildVariable, len(overrides))
	for key, value := range overrides {
		local[key] = kbuildVariable{value: normalizeKbuildPathVariable(key, value)}
	}
	return &kbuildParser{
		kb:                &KbuildFile{},
		initialVars:       initial,
		vars:              local,
		exported:          map[string]bool{},
		symbolicVariables: map[string]kbuildSymbolicVariableState{},
		expanding:         map[string]bool{},
		baseDir:           baseDir,
		currentRule:       -1,
	}
}

func normalizeKbuildPathVariable(name, value string) string {
	switch name {
	case "obj", "objtree", "src", "srctree":
		return strings.ReplaceAll(value, `\`, "/")
	default:
		return value
	}
}

func (p *kbuildParser) lookupVariable(name string) (kbuildVariable, bool) {
	if p.undefined[name] {
		return kbuildVariable{}, false
	}
	if variable, ok := p.vars[name]; ok {
		return variable, true
	}
	if variable, ok := p.baseVars[name]; ok {
		return variable, true
	}
	value, ok := p.initialVars.lookup(name)
	return kbuildVariable{value: value}, ok
}

func (p *kbuildParser) setVariable(name string, variable kbuildVariable) {
	delete(p.undefined, name)
	delete(p.symbolicVariables, name)
	variable.value = normalizeKbuildPathVariable(name, variable.value)
	p.vars[name] = variable
}

func (p *kbuildParser) undefineVariable(name string) {
	delete(p.vars, name)
	delete(p.symbolicVariables, name)
	if p.undefined == nil {
		p.undefined = map[string]bool{}
	}
	p.undefined[name] = true
}

func (p *kbuildParser) parseLine(line string, pos Position) error {
	return p.parseSourceLine(kbuildSourceLine{
		text:        line,
		uncommented: stripKbuildComment(line),
	}, pos)
}

func (p *kbuildParser) parseSourceLine(source kbuildSourceLine, pos Position) error {
	previousPos := p.currentPos
	p.currentPos = pos
	defer func() {
		p.currentPos = previousPos
	}()

	if p.defineName != "" {
		if strings.TrimSpace(source.uncommented) == "endef" {
			return p.finishDefine()
		}
		p.defineBody = append(p.defineBody, source.text)
		return nil
	}

	line := source.text
	uncommented := source.uncommented
	if strings.HasPrefix(line, "\t") {
		if p.currentRule >= 0 {
			// GNU Make conditionals are evaluated before rule parsing. A recipe
			// can therefore span an if/else/endif block without losing its rule:
			// inactive recipe lines disappear, while later active lines still
			// belong to the declaration preceding the conditional.
			if p.active() {
				if _, guarded, err := p.activeSymbolicSelector(); err != nil {
					return err
				} else if guarded {
					return fmt.Errorf("%s: probe-dependent branch changes a rule recipe", pos)
				}
				p.kb.Rules[p.currentRule].Recipe = append(p.kb.Rules[p.currentRule].Recipe, strings.TrimPrefix(line, "\t"))
			}
			return nil
		}
		line = strings.TrimLeft(line, " \t")
		uncommented = strings.TrimLeft(uncommented, " \t")
	}

	rawLine := line
	line = uncommented
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if handled, err := p.parseConditional(line, pos); handled || err != nil {
		return err
	}
	if !p.active() {
		return nil
	}
	p.currentRule = -1
	// In GNU Make, the part after a leading ';' in a target-specific
	// assignment is kept as shell text.  In particular, an unescaped '#'
	// inside that text is not stripped as a Make comment.  Linux uses this for
	// bindgen sed expressions containing Rust attributes (`#[link_name]`).
	// Parse that one grammar shape from the original logical line.
	if kbuildTargetVariableHasShellSuffix(rawLine) {
		if handled, err := p.parseRule(rawLine, pos); handled || err != nil {
			return err
		}
	}
	if name, op, ok := splitKbuildDefine(line); ok {
		if _, guarded, err := p.activeSymbolicSelector(); err != nil {
			return err
		} else if guarded {
			return fmt.Errorf("%s: probe-dependent define %q is unsupported", pos, name)
		}
		p.defineName = name
		p.defineOp = op
		p.definePos = pos
		p.defineBody = nil
		return nil
	}
	if handled, err := p.parseKbuildInclude(line, pos); handled || err != nil {
		return err
	}
	if handled, err := p.parseVariableDirective(line); handled || err != nil {
		return err
	}
	if lhs, _, _, ok := splitKbuildAssignment(line); ok {
		if strings.Contains(lhs, ":") {
			if handled, err := p.parseRule(line, pos); handled || err != nil {
				return err
			}
		}
		return p.parseAssignment(line, pos)
	}
	if handled, err := p.parseRule(line, pos); handled || err != nil {
		return err
	}
	if containsMakeReference(line) {
		// A standalone $(shell ...) is a GNU Make parse-time side effect, most
		// commonly mkdir for an output directory. Planning never materializes
		// outputs while reading Makefiles, and the expression contributes no
		// variable, rule, prerequisite, or recipe to the selected graph. Keep it
		// unevaluated; concrete action lowering creates its declared outputs.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "$(shell ") || strings.HasPrefix(trimmed, "${shell ") {
			return nil
		}
		_, err := p.expand(line)
		return err
	}
	return nil
}

func kbuildTargetVariableHasShellSuffix(line string) bool {
	_, _, assignment, inlineRecipe, ok := splitKbuildRule(line)
	if !ok || inlineRecipe != "" {
		return false
	}
	_, _, value, _, ok := splitKbuildTargetVariable(assignment)
	return ok && strings.HasPrefix(strings.TrimSpace(value), ";")
}

func (p *kbuildParser) finishDefine() error {
	body := strings.Join(p.defineBody, "\n")
	expandedBody := body
	// GNU Make's plain `define NAME` has recursive (`=`) flavor. Keep its body
	// untouched until use so it can call helpers declared later in the same
	// included source file. Only simple-flavor defines expand at definition
	// time; += inherits the existing variable's flavor through the same helper
	// used for ordinary assignments.
	if p.assignmentRequiresImmediateExpansion(p.defineName, p.defineOp) {
		var err error
		expandedBody, err = p.expand(body)
		if err != nil {
			return err
		}
	}
	if _, uncertain := p.symbolicVariables[p.defineName]; uncertain && (p.defineOp == "+=" || p.defineOp == "?=") {
		return fmt.Errorf("%s: define %s %s observes probe-dependent variable identity", p.definePos, p.defineName, p.defineOp)
	}
	p.assign(p.defineName, p.defineOp, body, expandedBody)
	p.defineName = ""
	p.defineOp = ""
	p.definePos = Position{}
	p.defineBody = nil
	return nil
}

func (p *kbuildParser) active() bool {
	if len(p.conds) == 0 {
		return true
	}
	return p.conds[len(p.conds)-1].active
}

func (p *kbuildParser) definitelyActive() bool {
	if len(p.conds) == 0 {
		return true
	}
	return p.conds[len(p.conds)-1].definitelyActive
}

func (p *kbuildParser) activeCondition() KbuildCondition {
	conditions := []KbuildCondition{}
	for _, frame := range p.conds {
		if frame.active && frame.hasCondition {
			conditions = append(conditions, frame.condition)
		}
	}
	return combineKbuildConditions(conditions...)
}

func (p *kbuildParser) selectProbeText(condition string, negated bool, trueText, falseText string) (string, error) {
	if p.selectSymbolic == nil {
		return "", fmt.Errorf("symbolic selector is unavailable")
	}
	selected, recognized, err := p.selectSymbolic(condition, "", negated, trueText, falseText)
	if err != nil {
		return "", err
	}
	if !recognized {
		return "", fmt.Errorf("conditional value does not retain probe provenance")
	}
	return selected, nil
}

func (p *kbuildParser) symbolicNot(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "":
		return "1", nil
	case "1":
		return "", nil
	default:
		return p.selectProbeText(value, true, "1", "")
	}
}

func (p *kbuildParser) symbolicAnd(left, right string) (string, error) {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return "", nil
	}
	if left == "1" {
		return right, nil
	}
	if right == "1" {
		return left, nil
	}
	return p.selectProbeText(left, false, right, "")
}

func (p *kbuildParser) symbolicOr(left, right string) (string, error) {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "1" || right == "1" {
		return "1", nil
	}
	if left == "" {
		return right, nil
	}
	if right == "" {
		return left, nil
	}
	return p.selectProbeText(left, false, "1", right)
}

func (p *kbuildParser) activeSymbolicSelector() (string, bool, error) {
	combined := "1"
	found := false
	for _, frame := range p.conds {
		if !frame.active || frame.symbolicCondition == "" {
			continue
		}
		selector := frame.symbolicCondition
		var err error
		if frame.symbolicNegated {
			selector, err = p.symbolicNot(selector)
			if err != nil {
				return "", false, err
			}
		}
		combined, err = p.symbolicAnd(combined, selector)
		if err != nil {
			return "", false, err
		}
		found = true
	}
	return combined, found, nil
}

func (p *kbuildParser) withActiveCondition(condition KbuildCondition) KbuildCondition {
	conditions := []KbuildCondition{}
	active := p.activeCondition()
	if !active.isEmpty() {
		conditions = append(conditions, active)
	}
	conditions = append(conditions, condition)
	return combineKbuildConditions(conditions...)
}

func (p *kbuildParser) parseConditional(line string, pos Position) (bool, error) {
	line = strings.TrimSpace(line)
	for _, keyword := range []string{"ifeq", "ifneq", "ifdef", "ifndef"} {
		if rest, ok := makeDirectiveRest(line, keyword); ok {
			result, err := p.evalConditional(keyword, rest)
			if err != nil {
				return true, fmt.Errorf("%s: evaluate %s: %w", pos, keyword, err)
			}
			p.pushConditional(result)
			return true, nil
		}
	}
	if rest, ok := makeDirectiveRest(line, "else"); ok {
		if len(p.conds) == 0 {
			return true, fmt.Errorf("%s: else without matching if", pos)
		}
		frame := &p.conds[len(p.conds)-1]
		rest = strings.TrimSpace(rest)
		if rest == "" {
			if frame.sawElse {
				return true, fmt.Errorf("%s: duplicate else", pos)
			}
			frame.sawElse = true
			p.activateElse(frame)
			return true, nil
		}
		for _, keyword := range []string{"ifeq", "ifneq", "ifdef", "ifndef"} {
			if nestedRest, ok := makeDirectiveRest(rest, keyword); ok {
				if frame.sawElse {
					return true, fmt.Errorf("%s: else conditional after else", pos)
				}
				if err := p.activateElseIf(frame, keyword, nestedRest); err != nil {
					return true, fmt.Errorf("%s: evaluate else %s: %w", pos, keyword, err)
				}
				return true, nil
			}
		}
		return true, fmt.Errorf("%s: unsupported else directive %q", pos, line)
	}
	if _, ok := makeDirectiveRest(line, "endif"); ok {
		if len(p.conds) == 0 {
			return true, fmt.Errorf("%s: endif without matching if", pos)
		}
		p.conds = p.conds[:len(p.conds)-1]
		return true, nil
	}
	return false, nil
}

func (p *kbuildParser) pushConditional(result kbuildConditionalEval) {
	parentActive := p.active()
	parentDefinitelyActive := p.definitelyActive()
	hasCondition := !result.known && result.hasCondition
	p.conds = append(p.conds, kbuildConditionalFrame{
		parentActive:           parentActive,
		parentDefinitelyActive: parentDefinitelyActive,
		previousKnown:          result.known,
		previousTaken:          result.known && result.value,
		previousCondition:      result.condition,
		hasPreviousCondition:   hasCondition,
		active:                 parentActive && (!result.known || result.value),
		definitelyActive:       parentDefinitelyActive && result.known && result.value,
		condition:              result.condition,
		hasCondition:           hasCondition,
		symbolicCondition:      result.symbolicCondition,
		previousSymbolic:       result.symbolicCondition,
	})
}

func (p *kbuildParser) activateElse(frame *kbuildConditionalFrame) {
	switch {
	case !frame.parentActive:
		frame.active = false
		frame.definitelyActive = false
	case !frame.previousKnown:
		frame.active = true
		frame.definitelyActive = false
		frame.symbolicCondition = frame.previousSymbolic
		frame.symbolicNegated = !frame.previousSymbolicNegated
		if frame.hasPreviousCondition {
			frame.condition = invertKbuildCondition(frame.previousCondition)
			frame.hasCondition = true
			frame.previousCondition = KbuildCondition{}
			frame.hasPreviousCondition = false
		}
	case frame.previousTaken:
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		frame.symbolicCondition = ""
		frame.symbolicNegated = false
	default:
		frame.active = true
		frame.definitelyActive = frame.parentDefinitelyActive
		frame.previousKnown = true
		frame.previousTaken = true
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		frame.symbolicCondition = ""
		frame.symbolicNegated = false
	}
}

func (p *kbuildParser) activateElseIf(frame *kbuildConditionalFrame, keyword, rest string) error {
	if !frame.parentActive {
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		frame.symbolicCondition = ""
		frame.symbolicNegated = false
		return nil
	}
	if !frame.previousKnown {
		result, err := p.evalConditional(keyword, rest)
		if err != nil {
			return err
		}
		if result.known && !result.value {
			frame.active = false
			frame.definitelyActive = false
			frame.condition = KbuildCondition{}
			frame.hasCondition = false
			frame.symbolicCondition = ""
			frame.symbolicNegated = false
			return nil
		}
		if result.symbolicCondition != "" {
			if frame.previousSymbolic == "" || frame.hasPreviousCondition || result.hasCondition {
				return fmt.Errorf("mixed Kconfig and probe-dependent else-if conditions are unsupported")
			}
			remaining, err := p.symbolicNot(frame.previousSymbolic)
			if err != nil {
				return err
			}
			branch, err := p.symbolicAnd(remaining, result.symbolicCondition)
			if err != nil {
				return err
			}
			taken, err := p.symbolicOr(frame.previousSymbolic, result.symbolicCondition)
			if err != nil {
				return err
			}
			// A correlated predicate can make an ordered branch concretely
			// impossible (for example, `if A; else if A`).  The empty selector
			// is boolean false here, not the absence of a symbolic guard.  Do
			// not parse that branch as an unguarded active branch.
			frame.active = branch != ""
			frame.definitelyActive = branch == "1" && frame.parentDefinitelyActive
			frame.condition = KbuildCondition{}
			frame.hasCondition = false
			if branch == "" || branch == "1" {
				frame.symbolicCondition = ""
			} else {
				frame.symbolicCondition = branch
			}
			frame.symbolicNegated = false
			switch taken {
			case "":
				frame.previousKnown = true
				frame.previousTaken = false
				frame.previousSymbolic = ""
			case "1":
				frame.previousKnown = true
				frame.previousTaken = true
				frame.previousSymbolic = ""
			default:
				frame.previousKnown = false
				frame.previousTaken = false
				frame.previousSymbolic = taken
			}
			frame.previousSymbolicNegated = false
			return nil
		}
		conditions := []KbuildCondition{}
		if frame.hasPreviousCondition {
			conditions = append(conditions, invertKbuildCondition(frame.previousCondition))
		}
		if !result.known && result.hasCondition {
			conditions = append(conditions, result.condition)
		}
		frame.condition = combineKbuildConditions(conditions...)
		frame.hasCondition = len(conditions) != 0
		frame.symbolicCondition = frame.previousSymbolic
		frame.symbolicNegated = !frame.previousSymbolicNegated
		frame.active = true
		frame.definitelyActive = false
		branchCondition := frame.condition
		if branchCondition.isEmpty() {
			branchCondition = KbuildCondition{Kind: "const", State: "y"}
		}
		frame.previousCondition = combineKbuildAny(frame.previousCondition, branchCondition)
		frame.hasPreviousCondition = true
		if result.known && result.value {
			// The ordered chain is now exhaustive: either an earlier unknown
			// branch was selected, or this unconditional remainder branch was.
			// A following else must therefore stay inactive even though the
			// current branch still carries the inverse symbolic selector.
			frame.previousKnown = true
			frame.previousTaken = true
			frame.hasPreviousCondition = false
			frame.previousSymbolic = ""
			frame.previousSymbolicNegated = false
		}
		return nil
	}
	if frame.previousTaken {
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		frame.symbolicCondition = ""
		frame.symbolicNegated = false
		return nil
	}
	result, err := p.evalConditional(keyword, rest)
	if err != nil {
		return err
	}
	frame.previousKnown = result.known
	frame.previousTaken = result.known && result.value
	frame.active = frame.parentActive && (!result.known || result.value)
	frame.definitelyActive = frame.parentDefinitelyActive && result.known && result.value
	frame.condition = result.condition
	frame.hasCondition = !result.known && result.hasCondition
	frame.previousCondition = result.condition
	frame.hasPreviousCondition = !result.known && result.hasCondition
	frame.symbolicCondition = result.symbolicCondition
	frame.symbolicNegated = false
	frame.previousSymbolic = result.symbolicCondition
	frame.previousSymbolicNegated = false
	return nil
}

func (p *kbuildParser) parseKbuildInclude(line string, pos Position) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false, nil
	}
	optional := false
	switch fields[0] {
	case "include":
	case "-include", "sinclude":
		optional = true
	default:
		return false, nil
	}
	selector, guarded, err := p.activeSymbolicSelector()
	if err != nil {
		return true, err
	}
	resolvedGuard := false
	if guarded {
		resolved, resolveErr := p.resolveKbuildSymbolic(selector)
		if resolveErr != nil {
			return true, fmt.Errorf("%s: resolve probe-dependent include: %w", pos, resolveErr)
		}
		if linuxProbeSymbolPattern.MatchString(resolved) {
			// Discovery records the guard's probe DAG but cannot select source
			// topology before configured actions run. The replay parse below will
			// either consume the include or skip it from the measured result.
			return true, nil
		}
		switch strings.TrimSpace(resolved) {
		case "":
			return true, nil
		case "1":
			resolvedGuard = true
		default:
			return true, fmt.Errorf("%s: probe-dependent include selector resolved to non-boolean text %q", pos, resolved)
		}
	}
	expandedPaths, err := p.expand(strings.Join(fields[1:], " "))
	if err != nil {
		return true, err
	}
	if linuxProbeSymbolPattern.MatchString(expandedPaths) {
		resolved, resolveErr := p.resolveKbuildSymbolic(expandedPaths)
		if resolveErr != nil {
			return true, fmt.Errorf("%s: resolve probe-dependent include paths: %w", pos, resolveErr)
		}
		if linuxProbeSymbolPattern.MatchString(resolved) {
			// The include name itself depends on configured probe data. As with
			// a probe-guarded include above, discovery records the dependency DAG
			// but defers source topology. Replay expands the concrete name and
			// parses its contents; ProbeResultOracle.ValidatePlan rejects any
			// probe that appears only in that replay-selected file.
			return true, nil
		}
		expandedPaths = resolved
	}
	expandedPaths, err = p.resolveKbuildSymbolicWords(expandedPaths)
	if err != nil {
		return true, fmt.Errorf("%s: resolve probe-dependent include paths: %w", pos, err)
	}
	paths := strings.Fields(expandedPaths)
	includes := make([]KbuildInclude, 0, len(paths))
	for _, path := range paths {
		includes = append(includes, KbuildInclude{
			Path:     filepath.ToSlash(path),
			Optional: optional,
			Position: pos,
		})
	}
	p.kb.Includes = append(p.kb.Includes, includes...)
	if p.includeFunc != nil {
		if resolvedGuard {
			return true, p.parseResolvedSymbolicIncludes(includes)
		}
		return true, p.includeFunc(includes)
	}
	return true, nil
}

// parseResolvedSymbolicIncludes parses a replay-selected include as ordinary
// Make input. The enclosing symbolic frames remain on the parent parser so the
// following else/endif directives retain their discovery identity, but their
// already measured selectors must not guard the included file a second time.
// Unknown Kconfig conditions remain intact. Any newly discovered probe inside
// the include is rejected later by ProbeResultOracle.ValidatePlan because it
// was not part of the discovery DAG.
func (p *kbuildParser) parseResolvedSymbolicIncludes(includes []KbuildInclude) error {
	saved := slices.Clone(p.conds)
	parentDefinitelyActive := true
	for index := range p.conds {
		frame := &p.conds[index]
		frame.parentDefinitelyActive = parentDefinitelyActive
		if !frame.active {
			frame.definitelyActive = false
			parentDefinitelyActive = false
			continue
		}
		frame.symbolicCondition = ""
		frame.symbolicNegated = false
		frame.definitelyActive = parentDefinitelyActive && !frame.hasCondition
		parentDefinitelyActive = frame.definitelyActive
	}
	err := p.includeFunc(includes)
	p.conds = saved
	return err
}

func (p *kbuildParser) parseVariableDirective(line string) (bool, error) {
	if _, _, _, ok := splitKbuildAssignment(line); ok {
		return false, nil
	}
	line = strings.TrimSpace(line)

	_, stripped := splitMakeAssignmentModifiers(line)
	if rest, ok := makeDirectiveRest(stripped, "undefine"); ok {
		if _, guarded, err := p.activeSymbolicSelector(); err != nil {
			return true, err
		} else if guarded {
			return true, fmt.Errorf("probe-dependent undefine is unsupported")
		}
		names, err := p.expandVariableDirectiveNames(rest)
		if err != nil {
			return true, err
		}
		for _, name := range names {
			p.undefineVariable(name)
		}
		return true, nil
	}

	if rest, ok := makeDirectiveRest(line, "unexport"); ok {
		if _, guarded, err := p.activeSymbolicSelector(); err != nil {
			return true, err
		} else if guarded {
			return true, fmt.Errorf("probe-dependent unexport is unsupported")
		}
		names, err := p.expandVariableDirectiveNames(rest)
		if err != nil {
			return true, err
		}
		for _, name := range names {
			delete(p.exported, name)
		}
		return true, nil
	}

	if rest, ok := makeDirectiveRest(line, "export"); ok {
		if _, guarded, err := p.activeSymbolicSelector(); err != nil {
			return true, err
		} else if guarded {
			return true, fmt.Errorf("probe-dependent export is unsupported")
		}
		names, err := p.expandVariableDirectiveNames(rest)
		if err != nil {
			return true, err
		}
		for _, name := range names {
			if _, ok := p.lookupVariable(name); !ok {
				p.setVariable(name, kbuildVariable{})
			}
			p.exported[name] = true
		}
		return true, nil
	}

	return false, nil
}

func (p *kbuildParser) expandVariableDirectiveNames(value string) ([]string, error) {
	expanded, err := p.expand(value)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Fields(expanded) {
		if containsMakeReference(name) {
			continue
		}
		if linuxProbeSymbolPattern.MatchString(name) {
			return nil, fmt.Errorf("variable directive name depends on an unresolved probe")
		}
		if _, uncertain := p.symbolicVariables[name]; uncertain {
			return nil, fmt.Errorf("variable directive observes probe-dependent identity of %q", name)
		}
		names = append(names, name)
	}
	return names, nil
}

func (p *kbuildParser) parseAssignment(line string, pos Position) error {
	modifiers, _ := splitMakeAssignmentModifiers(line)
	lhs, op, rhs, ok := splitKbuildAssignment(line)
	if !ok {
		return nil
	}
	rawLHS := strings.TrimSpace(lhs)
	expandedLHS, err := p.expand(lhs)
	if err != nil {
		return err
	}
	probeDependentLHS := linuxProbeSymbolPattern.MatchString(expandedLHS)
	expandedLHS, err = p.resolveKbuildSymbolic(expandedLHS)
	if err != nil {
		return fmt.Errorf("%s: resolve Kbuild assignment name: %w", p.currentPos, err)
	}
	if !containsMakeReference(expandedLHS) {
		lhs = strings.TrimSpace(expandedLHS)
	}
	_, wasDefined := p.lookupVariable(lhs)
	generatedKind, generatedCondition, generated := generatedTargetCondition(rawLHS)
	if !generated {
		generatedKind, generatedCondition, generated = generatedTargetCondition(lhs)
	}
	_, guarded, guardErr := p.activeSymbolicSelector()
	if guardErr != nil {
		return fmt.Errorf("%s: evaluate probe-dependent assignment guard: %w", pos, guardErr)
	}
	if guarded && probeDependentLHS {
		return fmt.Errorf("%s: probe-dependent branch changes a symbolic assignment name", pos)
	}
	if guarded && generated {
		return fmt.Errorf("%s: probe-dependent branch changes generated-target topology", pos)
	}
	if guarded && len(modifiers) != 0 {
		return fmt.Errorf("%s: probe-dependent assignment has stateful modifiers %q", pos, modifiers)
	}
	if p.commandLineVariables[lhs] && !slices.Contains(modifiers, "override") {
		// A command-line value wins over the assignment, but GNU Make still
		// applies the export attribute from `export NAME := ...`. This matters
		// for Kbuild's root aliases: the planner pins their canonical tree
		// values at command-line precedence while the source Makefile remains
		// responsible for deciding which aliases enter script environments.
		for _, modifier := range modifiers {
			switch modifier {
			case "export":
				p.exported[lhs] = true
			case "unexport":
				delete(p.exported, lhs)
			}
		}
		return nil
	}
	if _, uncertain := p.symbolicVariables[lhs]; uncertain && !guarded && (op == "+=" || op == "?=") {
		return fmt.Errorf("%s: assignment %s %s observes probe-dependent variable identity", pos, lhs, op)
	}
	materializeGenerated := generated
	if enabled, known := p.concreteConditionEnabled(generatedCondition); generated && known && !enabled {
		materializeGenerated = false
	}
	expandedRHS := rhs
	deferredSimple := op == ":=" && containsMakeShell(rhs) && (lhs == "targets" || !materializeGenerated)
	immediate := p.assignmentRequiresImmediateExpansion(lhs, op)
	if immediate && !deferredSimple {
		expandedRHS, err = p.expand(rhs)
		if err != nil {
			return err
		}
	}
	rhs, expandedRHS, symbolicState, err := p.retainSymbolicConditionalAssignment(lhs, op, rhs, expandedRHS)
	if err != nil {
		return fmt.Errorf("%s: retain probe-dependent assignment %s %s: %w", pos, lhs, op, err)
	}
	p.assign(lhs, op, rhs, expandedRHS)
	if symbolicState != nil {
		p.symbolicVariables[lhs] = *symbolicState
	}
	if deferredSimple {
		variable, ok := p.lookupVariable(lhs)
		if ok {
			variable.deferredSimple = true
			p.setVariable(lhs, variable)
		}
	}
	for _, modifier := range modifiers {
		switch modifier {
		case "export":
			p.exported[lhs] = true
		case "unexport":
			delete(p.exported, lhs)
		}
	}
	if deferredSimple {
		return nil
	}
	if !materializeGenerated || (op == "?=" && wasDefined) {
		return nil
	}
	semanticRHS := expandedRHS
	if !immediate {
		semanticRHS, err = p.expand(rhs)
		if err != nil {
			return err
		}
	}
	semanticRHS, err = p.resolveKbuildSymbolicWords(semanticRHS)
	if err != nil {
		return fmt.Errorf("%s: resolve Kbuild assignment value: %w", p.currentPos, err)
	}
	values := kbuildFields(semanticRHS)
	if len(values) == 0 {
		return nil
	}
	return p.parseGeneratedTargetAssignment(generatedKind, generatedCondition, values, pos)
}

// retainSymbolicConditionalAssignment turns branch-local scalar updates into
// one finite selection. It tracks definedness, flavor, and origin independently
// from the flattened value: mutually exclusive branches can converge, while
// identity-sensitive operations fail closed only for the property that remains
// unresolved.
func (p *kbuildParser) retainSymbolicConditionalAssignment(lhs, op, rhs, expandedRHS string) (string, string, *kbuildSymbolicVariableState, error) {
	selector, guarded, err := p.activeSymbolicSelector()
	if err != nil {
		return "", "", nil, err
	}
	if !guarded {
		return rhs, expandedRHS, nil, nil
	}
	current, exists := p.lookupVariable(lhs)
	state, uncertain := p.symbolicVariables[lhs]
	if !uncertain {
		if exists {
			state.definedWhen = "1"
			if current.recursive {
				state.recursiveWhen = "1"
			} else {
				state.simpleWhen = "1"
			}
			if p.environmentVariables[lhs] {
				state.environmentWhen = "1"
			} else {
				state.fileWhen = "1"
			}
		}
	}
	if op == "?=" {
		if uncertain {
			return "", "", nil, fmt.Errorf("conditional ?= observes probe-dependent definedness")
		}
		if exists {
			return rhs, expandedRHS, nil, nil
		}
		return "", "", nil, fmt.Errorf("conditional ?= changes definedness")
	}
	if op == "=" {
		if exists && !uncertain && current.recursive && rhs == current.value && !p.environmentVariables[lhs] {
			return rhs, expandedRHS, nil, nil
		}
		// A recursive assignment must remain deferred until each later use. A
		// finite probe selection can represent a new recursive value exactly
		// only when it is dollar-free. A preserved recursive value must have the
		// same property because it still needs late expansion; a simple value is
		// already frozen and can safely be selected verbatim, including dollars.
		if strings.Contains(rhs, "$") {
			return "", "", nil, fmt.Errorf("recursive assignment contains deferred Make expansion %q", rhs)
		}
		currentRaw := ""
		if exists {
			if current.deferredSimple {
				return "", "", nil, fmt.Errorf("recursive assignment preserves a deferred simple expansion on the unselected path")
			}
			if current.recursive && strings.Contains(current.value, "$") {
				return "", "", nil, fmt.Errorf("recursive assignment preserves deferred Make expansion %q on the unselected path", current.value)
			}
			currentRaw = current.value
		}
		selected, selectErr := p.selectProbeText(selector, false, rhs, currentRaw)
		if selectErr != nil {
			return "", "", nil, selectErr
		}
		next, stateErr := p.mergeGuardedAssignmentState(selector, state, kbuildGuardedRecursiveAssignment)
		if stateErr != nil {
			return "", "", nil, stateErr
		}
		return selected, selected, next, nil
	}
	currentValue := ""
	if exists {
		currentValue, _, err = p.expandVariable(lhs, "$("+lhs+")", 0)
		if err != nil {
			return "", "", nil, err
		}
	}
	if op == "+=" {
		appended := rhs
		if uncertain {
			switch flavor, known := state.exactFlavor(); {
			case known && flavor == "simple":
				appended = expandedRHS
			case known && flavor == "recursive":
				appended = rhs
			case rhs != expandedRHS:
				return "", "", nil, fmt.Errorf("conditional append observes probe-dependent assignment flavor")
			}
		} else if exists && !current.recursive {
			appended = expandedRHS
		}
		if strings.Contains(appended, "$") {
			return "", "", nil, fmt.Errorf("conditional append contains deferred Make expansion %q", appended)
		}
		selected, selectErr := p.selectProbeText(selector, false, appended, "")
		if selectErr != nil {
			return "", "", nil, selectErr
		}
		next, stateErr := p.mergeGuardedAssignmentState(selector, state, kbuildGuardedAppendAssignment)
		if stateErr != nil {
			return "", "", nil, stateErr
		}
		return selected, selected, next, nil
	}
	if op != ":=" {
		return "", "", nil, fmt.Errorf("assignment flavor %q is unsupported in a probe-dependent branch", op)
	}
	if exists && current.recursive {
		return "", "", nil, fmt.Errorf("conditional := would replace a recursive variable")
	}
	if current.deferredSimple || containsMakeShell(rhs) {
		return "", "", nil, fmt.Errorf("conditional := contains stateful deferred expansion")
	}
	if exists && !uncertain && expandedRHS == currentValue && !p.environmentVariables[lhs] {
		return rhs, expandedRHS, nil, nil
	}
	selected, selectErr := p.selectProbeText(selector, false, expandedRHS, currentValue)
	if selectErr != nil {
		return "", "", nil, selectErr
	}
	next, stateErr := p.mergeGuardedAssignmentState(selector, state, kbuildGuardedSimpleAssignment)
	if stateErr != nil {
		return "", "", nil, stateErr
	}
	return selected, selected, next, nil
}

func (p *kbuildParser) selectKnownOrSymbolicText(predicate, trueText, falseText string) (string, error) {
	switch strings.TrimSpace(predicate) {
	case "":
		return falseText, nil
	case "1":
		return trueText, nil
	default:
		return p.selectProbeText(predicate, false, trueText, falseText)
	}
}

type kbuildGuardedAssignmentKind int

const (
	kbuildGuardedAppendAssignment kbuildGuardedAssignmentKind = iota
	kbuildGuardedSimpleAssignment
	kbuildGuardedRecursiveAssignment
)

// mergeGuardedAssignmentState composes the identity after one guarded source
// assignment. An append preserves the flavor of an already-defined variable
// and creates a recursive variable only on paths where it was undefined. A :=
// assignment makes selected paths simple, while = makes them recursive. Every
// source assignment changes environment origin to file origin on precisely the
// selected paths.
func (p *kbuildParser) mergeGuardedAssignmentState(selector string, old kbuildSymbolicVariableState, kind kbuildGuardedAssignmentKind) (*kbuildSymbolicVariableState, error) {
	notSelector, err := p.symbolicNot(selector)
	if err != nil {
		return nil, err
	}
	preservedDefined, err := p.symbolicAnd(notSelector, old.definedWhen)
	if err != nil {
		return nil, err
	}
	definedWhen, err := p.symbolicOr(selector, preservedDefined)
	if err != nil {
		return nil, err
	}
	preservedEnvironment, err := p.symbolicAnd(notSelector, old.environmentWhen)
	if err != nil {
		return nil, err
	}
	preservedFile, err := p.symbolicAnd(notSelector, old.fileWhen)
	if err != nil {
		return nil, err
	}
	fileWhen, err := p.symbolicOr(selector, preservedFile)
	if err != nil {
		return nil, err
	}

	next := kbuildSymbolicVariableState{
		definedWhen:     definedWhen,
		environmentWhen: preservedEnvironment,
		fileWhen:        fileWhen,
	}
	if kind == kbuildGuardedAppendAssignment {
		next.simpleWhen = old.simpleWhen
		previouslyUndefined, notErr := p.symbolicNot(old.definedWhen)
		if notErr != nil {
			return nil, notErr
		}
		createdRecursive, andErr := p.symbolicAnd(selector, previouslyUndefined)
		if andErr != nil {
			return nil, andErr
		}
		next.recursiveWhen, err = p.symbolicOr(old.recursiveWhen, createdRecursive)
		if err != nil {
			return nil, err
		}
	} else {
		preservedSimple, andErr := p.symbolicAnd(notSelector, old.simpleWhen)
		if andErr != nil {
			return nil, andErr
		}
		selectedSimple := ""
		if kind == kbuildGuardedSimpleAssignment {
			selectedSimple = selector
		}
		next.simpleWhen, err = p.symbolicOr(selectedSimple, preservedSimple)
		if err != nil {
			return nil, err
		}
		preservedRecursive, andErr := p.symbolicAnd(notSelector, old.recursiveWhen)
		if andErr != nil {
			return nil, andErr
		}
		selectedRecursive := ""
		if kind == kbuildGuardedRecursiveAssignment {
			selectedRecursive = selector
		}
		next.recursiveWhen, err = p.symbolicOr(selectedRecursive, preservedRecursive)
		if err != nil {
			return nil, err
		}
	}
	return normalizeKbuildSymbolicVariableState(next), nil
}

func (p *kbuildParser) concreteConditionEnabled(condition KbuildCondition) (bool, bool) {
	switch condition.Kind {
	case "const":
		return condition.State != "n" && condition.State != "-" && condition.State != "", true
	case "config", "config_eq", "config_ne":
		value, exists := p.lookupRawVar(condition.Symbol)
		if !exists && !p.configVariablesComplete {
			return false, false
		}
		if value != "y" && value != "m" {
			value = "n"
		}
		switch condition.Kind {
		case "config":
			return value != "n", true
		case "config_eq":
			return value == condition.State, true
		default:
			return value != condition.State, true
		}
	default:
		return false, false
	}
}

func containsMakeShell(value string) bool {
	return strings.Contains(value, "$(shell ") || strings.Contains(value, "${shell ") ||
		strings.Contains(value, "$(shell,") || strings.Contains(value, "${shell,")
}

func (p *kbuildParser) assignmentRequiresImmediateExpansion(lhs, op string) bool {
	if op == ":=" {
		return true
	}
	if op == "+=" {
		current, ok := p.lookupVariable(lhs)
		if ok && !current.recursive {
			return true
		}
	}
	return false
}

func (p *kbuildParser) parseGeneratedTargetAssignment(kind string, cond KbuildCondition, values []string, pos Position) error {
	cond = p.withActiveCondition(cond)
	for _, value := range values {
		target, ok := kbuildGeneratedToken(value)
		if !ok {
			continue
		}
		p.kb.Generated = append(p.kb.Generated, KbuildTarget{
			Kind:      kind,
			Target:    target,
			Condition: cond,
			Position:  pos,
		})
	}
	return nil
}

func (p *kbuildParser) parseRule(line string, pos Position) (bool, error) {
	targetsText, separator, prerequisitesText, inlineRecipe, ok := splitKbuildRule(line)
	if !ok {
		return false, nil
	}
	if _, guarded, err := p.activeSymbolicSelector(); err != nil {
		return true, err
	} else if guarded {
		return true, fmt.Errorf("%s: probe-dependent branch changes rule or target-specific assignment topology", pos)
	}
	targets, err := p.expandFields(targetsText)
	if err != nil {
		return true, err
	}
	if len(targets) == 0 {
		return false, nil
	}

	if variable, op, value, modifiers, ok := splitKbuildTargetVariable(prerequisitesText); ok {
		expandedValue := value
		// Recursive target-specific assignments retain their source text until
		// the selected target actually observes the variable.  Expanding every
		// RHS here is observably different from GNU Make: it executes $(shell ...)
		// while merely reading the Makefile, even when no selected target ever
		// references that variable.  Keep the existing definition-time snapshot
		// for the immediate operators; assign() decides whether += consumes that
		// snapshot from the flavor visible in the target context.
		if op == ":=" || op == "+=" {
			expandedValue, err = p.expand(value)
			if err != nil {
				return true, err
			}
		}
		p.kb.TargetVariables = append(p.kb.TargetVariables, KbuildTargetVariable{
			Targets:   targets,
			Variable:  variable,
			Operator:  op,
			Value:     expandedValue,
			Modifiers: modifiers,
			Position:  pos,
			rawValue:  value,
		})
		return true, nil
	}

	targetPattern := ""
	if rawPattern, remainder, static := splitKbuildStaticPattern(prerequisitesText); static {
		expanded, expandErr := p.expandFields(rawPattern)
		if expandErr != nil {
			return true, expandErr
		}
		if len(expanded) != 1 || strings.Count(expanded[0], "%") != 1 {
			return true, fmt.Errorf("%s: static pattern rule requires one target pattern, got %q", pos, expanded)
		}
		targetPattern = expanded[0]
		prerequisitesText = remainder
	}
	prerequisites, orderOnly, err := p.expandPrerequisites(prerequisitesText)
	if err != nil {
		return true, err
	}
	rule := KbuildRule{
		Targets:       targets,
		TargetPattern: targetPattern,
		Separator:     separator,
		Prerequisites: prerequisites,
		OrderOnly:     orderOnly,
		Condition:     p.withActiveCondition(KbuildCondition{Kind: "const", State: "y"}),
		Position:      pos,
	}
	if strings.TrimSpace(inlineRecipe) != "" {
		rule.Recipe = append(rule.Recipe, strings.TrimSpace(inlineRecipe))
	}
	p.kb.Rules = append(p.kb.Rules, rule)
	p.currentRule = len(p.kb.Rules) - 1
	return true, nil
}

func splitKbuildStaticPattern(value string) (string, string, bool) {
	depth := 0
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth != 0 || (index+1 < len(value) && value[index+1] == '=') {
				continue
			}
			pattern := strings.TrimSpace(value[:index])
			if !strings.Contains(pattern, "%") {
				return "", "", false
			}
			return pattern, strings.TrimSpace(value[index+1:]), true
		}
	}
	return "", "", false
}

func (p *kbuildParser) expandFields(value string) ([]string, error) {
	expanded, err := p.expand(value)
	if err != nil {
		return nil, err
	}
	expanded, err = p.resolveKbuildSymbolicWords(expanded)
	if err != nil {
		return nil, err
	}
	return strings.Fields(expanded), nil
}

func (p *kbuildParser) resolveKbuildSymbolic(value string) (string, error) {
	if p.resolveSymbolic == nil {
		return value, nil
	}
	return p.resolveSymbolic(value)
}

func (p *kbuildParser) resolveKbuildSymbolicWords(value string) (string, error) {
	if p.resolveSymbolicWords != nil {
		return p.resolveSymbolicWords(value)
	}
	return p.resolveKbuildSymbolic(value)
}

func (p *kbuildParser) resolveKbuildSymbolicStructure(value string) (string, error) {
	if p.resolveSymbolicStructure != nil {
		return p.resolveSymbolicStructure(value)
	}
	return p.resolveKbuildSymbolic(value)
}

func (p *kbuildParser) expandPrerequisites(value string) ([]string, []string, error) {
	fields, err := p.expandFields(value)
	if err != nil {
		return nil, nil, err
	}
	var prerequisites, orderOnly []string
	current := &prerequisites
	for _, field := range fields {
		if field == "|" {
			current = &orderOnly
			continue
		}
		*current = append(*current, field)
	}
	return prerequisites, orderOnly, nil
}

func (p *kbuildParser) assign(lhs, op, rhs, expandedRHS string) {
	_, alreadyDefined := p.lookupVariable(lhs)
	assigned := op != "?=" || !alreadyDefined
	switch op {
	case "+=":
		current, ok := p.lookupVariable(lhs)
		switch {
		case !ok:
			p.setVariable(lhs, kbuildVariable{value: rhs, recursive: true})
		case current.recursive:
			current.value = appendMakeValue(current.value, rhs)
			p.setVariable(lhs, current)
		default:
			current.value = appendMakeValue(current.value, expandedRHS)
			p.setVariable(lhs, current)
		}
	case "?=":
		if _, ok := p.lookupVariable(lhs); !ok {
			p.setVariable(lhs, kbuildVariable{value: rhs, recursive: true})
		}
	case "=":
		p.setVariable(lhs, kbuildVariable{value: rhs, recursive: true})
	default:
		p.setVariable(lhs, kbuildVariable{value: expandedRHS})
	}
	if assigned {
		// A Makefile assignment replaces an inherited variable's environment
		// origin, while its automatic export membership remains in force.  This
		// distinction matters to $(origin ...) without changing what a later
		// recursive invocation receives.
		delete(p.environmentVariables, lhs)
	}
}

func appendMakeValue(current, appended string) string {
	if appended == "" {
		return current
	}
	if current == "" {
		return appended
	}
	return current + " " + appended
}

func kbuildFields(value string) []string {
	var fields []string
	var field strings.Builder
	inSingle := false
	inDouble := false
	started := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
			started = true
		case ch == '"' && !inSingle:
			inDouble = !inDouble
			started = true
		case ch == '\\' && i+1 < len(value):
			i++
			field.WriteByte(value[i])
			started = true
		case (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r') && !inSingle && !inDouble:
			if started {
				fields = append(fields, field.String())
				field.Reset()
				started = false
			}
		default:
			field.WriteByte(ch)
			started = true
		}
	}
	if started {
		fields = append(fields, field.String())
	}
	return fields
}

func (p *kbuildParser) expand(value string) (string, error) {
	return p.expandDepth(value, 0)
}

func (p *kbuildParser) expandDepth(value string, depth int) (string, error) {
	if depth > 100 {
		return "", fmt.Errorf("too deep Kbuild variable expansion")
	}
	if !strings.ContainsRune(value, '$') {
		return value, nil
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 >= len(value) {
			out.WriteByte(value[i])
			i++
			continue
		}
		// GNU Make collapses an escaped dollar before handing function arguments
		// to $(shell). This is significant for scripts/Makefile.compiler:
		// TMPOUT's $$$$ becomes the shell's $$ PID token, while $$TMP becomes
		// the shell variable $TMP in the compiler command.
		if value[i+1] == compactKbuildLiteralTreeEscapeByte[0] {
			out.WriteString(compactKbuildLiteralTreeEscapeByte)
			i += 2
			continue
		}
		if value[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if value[i+1] != '(' && value[i+1] != '{' {
			name := value[i+1 : i+2]
			expanded, ok, err := p.expandVariable(name, "$"+name, depth+1)
			if err != nil {
				return "", err
			}
			if !ok {
				out.WriteByte(value[i])
				out.WriteByte(value[i+1])
			} else {
				out.WriteString(expanded)
			}
			i += 2
			continue
		}
		open := i + 1
		end, err := matchingKbuildReference(value, open)
		if err != nil {
			return "", err
		}
		expanded, err := p.evalReference(value[i:end+1], value[i+2:end], depth+1)
		if err != nil {
			return "", err
		}
		out.WriteString(expanded)
		i = end + 1
	}
	return out.String(), nil
}

func (p *kbuildParser) evalReference(original, clause string, depth int) (string, error) {
	name, args, ok := splitMakeFunction(clause)
	if ok {
		switch name {
		case "call":
			return p.evalCall(args, original, depth)
		case "foreach":
			return p.evalForeach(args, original, depth)
		case "eval":
			return p.evalEval(args, original, depth)
		case "error":
			return p.evalDiagnostic(args, original, depth, true)
		case "if":
			return p.evalIf(args, original, depth)
		case "and":
			return p.evalAnd(args, original, depth)
		case "or":
			return p.evalOr(args, original, depth)
		case "let":
			return p.evalLet(args, original, depth)
		case "origin":
			return p.evalOrigin(args, original, depth)
		case "flavor":
			return p.evalFlavor(args, original, depth)
		case "value":
			return p.evalValue(args, original, depth)
		case "shell":
			return p.evalShell(args, original, depth)
		case "warning", "info":
			return p.evalDiagnostic(args, original, depth, false)
		default:
			for i, arg := range args {
				expanded, err := p.expandDepth(arg, depth)
				if err != nil {
					return "", err
				}
				args[i] = expanded
			}
			if makeArgsContainProbeSymbol(args) {
				if name == "wildcard" && len(args) == 1 {
					// wildcard is a parser-context operation: unlike the pure
					// Make transforms below, it observes this invocation's exact
					// source roots and visible predecessor artifacts. Discovery
					// retains an opaque replay expression. Once configured probe
					// results are available, resolve its pattern list first and run
					// the ordinary wildcard evaluator against the same filesystem
					// view instead of trying to execute a filesystem query inside a
					// compiler probe action.
					resolved, resolveErr := p.resolveKbuildSymbolic(args[0])
					if resolveErr != nil {
						return "", fmt.Errorf("resolve probe-dependent wildcard patterns: %w", resolveErr)
					}
					args[0] = resolved
					if !makeArgsContainProbeSymbol(args) {
						if makeArgsContainReference(args) && !p.expandedReferencesAreLiteral {
							return original, nil
						}
						return p.expandWildcard(args[0])
					}
				}
				if p.transformSymbolic == nil {
					return "", fmt.Errorf("Make function %q consumes an unresolved probe value", name)
				}
				transformed, recognized, transformErr := p.transformSymbolic(name, args)
				if transformErr != nil {
					return "", transformErr
				}
				if !recognized {
					return "", fmt.Errorf("Make function %q does not retain probe provenance", name)
				}
				return transformed, nil
			}
			if name == "file" {
				if len(args) != 1 {
					return original, nil
				}
				return p.makeFile(args[0], original)
			}
			if name == "wildcard" {
				if len(args) != 1 {
					return original, nil
				}
				if makeArgsContainReference(args) && !p.expandedReferencesAreLiteral {
					return original, nil
				}
				return p.expandWildcard(args[0])
			}
			if name == "realpath" {
				if len(args) != 1 {
					return original, nil
				}
				if makeArgsContainReference(args) && !p.expandedReferencesAreLiteral {
					return original, nil
				}
				return p.expandRealPaths(args[0])
			}
			if name == "abspath" {
				if len(args) != 1 {
					return original, nil
				}
				if makeArgsContainReference(args) && !p.expandedReferencesAreLiteral {
					return original, nil
				}
				return p.expandAbsPaths(args[0])
			}
			return p.evalMakeFunction(name, args, original), nil
		}
	}
	if variable, pattern, replacement, ok := splitMakeSubstitution(clause); ok {
		varName, computedName, complete, err := p.expandComputedVariableName(variable, depth)
		if err != nil {
			return "", err
		}
		if !complete {
			return original, nil
		}
		if linuxProbeSymbolPattern.MatchString(varName) {
			resolvedName, resolveErr := p.resolveKbuildSymbolic(varName)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve probe-dependent computed Make variable name %q: %w", varName, resolveErr)
			}
			varName = resolvedName
			if linuxProbeSymbolPattern.MatchString(varName) {
				if p.provisionalComputedNames {
					// Discovery cannot select a computed variable keyed by a
					// measured target name. Treat it as undefined for this
					// provisional target parse; replay resolves the automatic
					// variable first and performs the exact lookup. Any probe
					// introduced only by the selected value is rejected by
					// ProbeResultOracle.ValidatePlan.
					return "", nil
				}
				return "", fmt.Errorf("probe-dependent computed Make variable name %q is unsupported outside target replay", varName)
			}
		}
		value, ok, err := p.expandVariable(varName, original, depth)
		if err != nil {
			return "", err
		}
		if !ok {
			if computedName {
				return "", nil
			}
			return original, nil
		}
		pattern, patternErr := p.expandDepth(pattern, depth)
		replacement, replacementErr := p.expandDepth(replacement, depth)
		if patternErr != nil {
			return "", patternErr
		}
		if replacementErr != nil {
			return "", replacementErr
		}
		if (containsMakeReference(pattern) || containsMakeReference(replacement)) &&
			!p.expandedReferencesAreLiteral {
			return original, nil
		}
		// GNU Make's suffix substitution reference $(var:suffix=replacement)
		// is shorthand for $(patsubst %suffix,%replacement,$(var)).  A
		// pattern that already contains an unescaped '%' keeps the ordinary
		// patsubst semantics.  Treating a suffix as a complete-word pattern
		// leaves expressions such as $(m:.o=) unchanged and corrupts Kbuild's
		// multi-object module names.
		if _, _, wildcard := splitMakePercent(pattern); !wildcard {
			pattern = "%" + pattern
			replacement = "%" + replacement
		}
		if linuxProbeSymbolPattern.MatchString(value) || linuxProbeSymbolPattern.MatchString(pattern) || linuxProbeSymbolPattern.MatchString(replacement) {
			if p.transformSymbolic == nil {
				return "", fmt.Errorf("Make substitution consumes an unresolved probe value")
			}
			transformed, recognized, transformErr := p.transformSymbolic("patsubst", []string{pattern, replacement, value})
			if transformErr != nil {
				return "", transformErr
			}
			if !recognized {
				return "", fmt.Errorf("Make substitution does not retain probe provenance")
			}
			return transformed, nil
		}
		return mapMakeWords(value, func(word string) string {
			return makePatsubst(strings.TrimSpace(pattern), strings.TrimSpace(replacement), word)
		}), nil
	}
	varName, computedName, complete, err := p.expandComputedVariableName(clause, depth)
	if err != nil {
		return "", err
	}
	if !complete {
		return original, nil
	}
	if linuxProbeSymbolPattern.MatchString(varName) {
		resolvedName, resolveErr := p.resolveKbuildSymbolic(varName)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve probe-dependent computed Make variable name %q: %w", varName, resolveErr)
		}
		varName = resolvedName
		if linuxProbeSymbolPattern.MatchString(varName) {
			if p.provisionalComputedNames {
				return "", nil
			}
			return "", fmt.Errorf("probe-dependent computed Make variable name %q is unsupported outside target replay", varName)
		}
	}
	value, ok, err := p.expandVariable(varName, original, depth)
	if err != nil {
		return "", err
	}
	if !ok {
		if computedName {
			return "", nil
		}
		return original, nil
	}
	return value, nil
}

// GNU Make expands a computed variable name exactly once before looking it up.
// Dollars escaped in that spelling therefore become literal name bytes: in a
// target with stem 32, $(foo_$$*) names foo_$*, while $(foo_$*) names foo_32.
// Protect escaped pairs while expanding the active references so the resulting
// literal dollar is not mistaken for an unresolved automatic variable.
func (p *kbuildParser) expandComputedVariableName(name string, depth int) (string, bool, bool, error) {
	name = strings.TrimSpace(name)
	const escapedDollar = "\x00"
	if strings.Contains(name, escapedDollar) {
		return "", false, false, fmt.Errorf("computed Make variable name contains a NUL byte")
	}
	protected := strings.ReplaceAll(name, "$$", escapedDollar)
	computed := protected != name || containsMakeVariableReference(protected)
	if !computed {
		return name, false, true, nil
	}
	expanded, err := p.expandDepth(protected, depth)
	if err != nil {
		return "", true, false, err
	}
	if containsMakeVariableReference(expanded) {
		return "", true, false, nil
	}
	expanded = strings.ReplaceAll(expanded, escapedDollar, "$")
	return p.projectComputedVariableName(strings.TrimSpace(expanded)), true, true, nil
}

// projectComputedVariableName maps an action-only rendering value back to the
// logical Make value from which it was projected. A source-defined variable
// whose exact expanded name exists remains authoritative. Otherwise every
// recognized rendering is projected even when the resulting logical variable
// is undefined: the rendering is an evaluator implementation detail, while
// GNU Make's undefined-variable behavior belongs to the logical name.
func (p *kbuildParser) projectComputedVariableName(name string) string {
	if p.exactKbuildVariableDefined(name) {
		return name
	}
	projected := name
	for _, projection := range p.renderedValueProjections {
		if projection.rendered == "" || !strings.Contains(projected, projection.rendered) {
			continue
		}
		projected = strings.ReplaceAll(projected, projection.rendered, projection.logical)
	}
	return projected
}

func (p *kbuildParser) exactKbuildVariableDefined(name string) bool {
	for index := len(p.locals) - 1; index >= 0; index-- {
		if _, ok := p.locals[index][name]; ok {
			return true
		}
	}
	_, ok := p.lookupVariable(name)
	return ok
}

func makeArgsContainProbeSymbol(args []string) bool {
	for _, arg := range args {
		if linuxProbeSymbolPattern.MatchString(arg) {
			return true
		}
	}
	return false
}

func (p *kbuildParser) expandVariable(name, original string, depth int) (string, bool, error) {
	for i := len(p.locals) - 1; i >= 0; i-- {
		value, ok := p.locals[i][name]
		if ok {
			return value, true, nil
		}
	}
	if p.commandSelectionExpansion != nil &&
		(strings.HasPrefix(name, "cmd_") || strings.HasPrefix(name, "rule_")) {
		return p.commandSelectionExpansion(name, original, depth)
	}
	variable, ok := p.lookupVariable(name)
	if !ok {
		// Automatic variables are bound only after Make has selected a concrete
		// rule target. Preserve them in evaluated command-variable snapshots so
		// the action planner can bind the selected target and prerequisite list.
		// This must precede MakeVariablesComplete: "undefined in this parse
		// phase" does not mean empty for $@, $<, $^, and their peers.
		if isKbuildAutomaticVariable(name) {
			return original, true, nil
		}
		if p.configVariablesComplete && strings.HasPrefix(name, "CONFIG_") {
			return "", true, nil
		}
		if p.makeVariablesComplete {
			return "", true, nil
		}
		if p.knownEmptyConditionalVariable(name) {
			return "", true, nil
		}
		return "", false, nil
	}
	if !variable.recursive && !variable.deferredSimple {
		return variable.value, true, nil
	}
	if p.expanding[name] {
		return original, true, nil
	}
	p.expanding[name] = true
	defer delete(p.expanding, name)
	expanded, err := p.expandDepth(variable.value, depth)
	if err != nil {
		return "", false, err
	}
	if variable.deferredSimple {
		// GNU Make's := variables are immutable expanded values. Our graph
		// evaluator delays shell-bearing definitions until first use, then
		// caches that result so subsequent references retain simple flavor.
		variable.value = expanded
		variable.deferredSimple = false
		p.setVariable(name, variable)
	}
	return expanded, true, nil
}

func isKbuildAutomaticVariable(name string) bool {
	if name == "" || !strings.ContainsRune("@%<?^+*|", rune(name[0])) {
		return false
	}
	return len(name) == 1 || (len(name) == 2 && (name[1] == 'D' || name[1] == 'F'))
}

func (p *kbuildParser) knownEmptyConditionalVariable(name string) bool {
	if len(name) < 3 || name[len(name)-2] != '-' || !strings.Contains("ymn", name[len(name)-1:]) {
		return false
	}
	prefix := name[:len(name)-1]
	for _, state := range []string{"", "y", "m", "n"} {
		candidate := prefix + state
		if candidate == name {
			continue
		}
		if _, ok := p.lookupVariable(candidate); ok {
			return true
		}
	}
	return false
}

func (p *kbuildParser) lookupRawVar(name string) (string, bool) {
	for i := len(p.locals) - 1; i >= 0; i-- {
		value, ok := p.locals[i][name]
		if ok {
			return value, true
		}
	}
	variable, ok := p.lookupVariable(name)
	if !ok {
		return "", false
	}
	return variable.value, true
}

func (p *kbuildParser) pushLocal(values map[string]string) {
	p.locals = append(p.locals, values)
}

func (p *kbuildParser) popLocal() {
	p.locals = p.locals[:len(p.locals)-1]
}

func (p *kbuildParser) evalCall(args []string, original string, depth int) (string, error) {
	if len(args) == 0 {
		return original, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	callArgs := make([]string, 0, len(args)-1)
	for _, arg := range args[1:] {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		callArgs = append(callArgs, expanded)
	}
	body, ok := p.lookupRawVar(name)
	if !ok || name == "" {
		if p.makeVariablesComplete && name != "" {
			return "", fmt.Errorf(
				"%s: Kbuild call target %q is not defined by the parsed Make workload",
				p.currentPos,
				name,
			)
		}
		if p.makeVariablesComplete {
			return "", nil
		}
		return original, nil
	}
	locals := kbuildCallLocals(name, body, callArgs)
	p.pushLocal(locals)
	defer p.popLocal()
	return p.expandDepth(body, depth)
}

func kbuildCallLocals(name, body string, callArgs []string) map[string]string {
	locals := map[string]string{}
	for _, positional := range positionalMakeReferences(body) {
		locals[positional] = ""
	}
	locals["0"] = name
	for index, expanded := range callArgs {
		locals[fmt.Sprintf("%d", index+1)] = expanded
	}
	return locals
}

func positionalMakeReferences(value string) []string {
	names := map[string]bool{}
	for i := 0; i+1 < len(value); i++ {
		if value[i] != '$' {
			continue
		}
		if value[i+1] >= '0' && value[i+1] <= '9' {
			names[value[i+1:i+2]] = true
			i++
			continue
		}
		if value[i+1] != '(' && value[i+1] != '{' {
			continue
		}
		end, err := matchingKbuildReference(value, i+1)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(value[i+2 : end])
		if isPositionalMakeName(name) {
			names[name] = true
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func isPositionalMakeName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (p *kbuildParser) evalForeach(args []string, original string, depth int) (string, error) {
	if len(args) != 3 {
		return original, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	list, err := p.expandDepth(args[1], depth)
	if err != nil {
		return "", err
	}
	if linuxProbeSymbolPattern.MatchString(list) {
		resolved, resolveErr := p.resolveKbuildSymbolic(list)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve probe-dependent foreach list: %w", resolveErr)
		}
		list = resolved
	}
	if linuxProbeSymbolPattern.MatchString(name) {
		return "", fmt.Errorf("Make foreach cannot use an unresolved probe variable name")
	}
	if linuxProbeSymbolPattern.MatchString(list) {
		if p.transformSymbolic == nil {
			return "", fmt.Errorf("Make foreach cannot retain an unresolved probe list")
		}
		transformed, recognized, transformErr := p.transformSymbolic("foreach", []string{name, list, args[2]})
		if transformErr != nil {
			return "", transformErr
		}
		if !recognized {
			return "", fmt.Errorf("Make foreach does not retain probe provenance")
		}
		return transformed, nil
	}
	if name == "" || containsMakeReference(name) || containsMakeReference(list) {
		return original, nil
	}
	var out []string
	hasMultiline := false
	for _, word := range strings.Fields(list) {
		p.pushLocal(map[string]string{name: word})
		expanded, err := p.expandDepth(args[2], depth)
		p.popLocal()
		if err != nil {
			return "", err
		}
		expanded = strings.TrimSpace(expanded)
		if expanded != "" {
			hasMultiline = hasMultiline || strings.Contains(expanded, "\n")
			out = append(out, expanded)
		}
	}
	if hasMultiline {
		return strings.Join(out, "\n"), nil
	}
	return strings.Join(out, " "), nil
}

func (p *kbuildParser) evalLet(args []string, original string, depth int) (string, error) {
	if len(args) != 3 {
		return original, nil
	}
	valuesText, err := p.expandDepth(args[1], depth)
	if err != nil {
		return "", err
	}
	namesText := strings.TrimSpace(args[0])
	if linuxProbeSymbolPattern.MatchString(namesText) || linuxProbeSymbolPattern.MatchString(valuesText) {
		return "", fmt.Errorf("Make let cannot split an unresolved probe value")
	}
	if containsMakeReference(namesText) || containsMakeReference(valuesText) {
		return original, nil
	}
	names := strings.Fields(namesText)
	if len(names) == 0 {
		return original, nil
	}
	values := strings.Fields(valuesText)
	locals := map[string]string{}
	for i, name := range names {
		switch {
		case i == len(names)-1:
			if i < len(values) {
				locals[name] = strings.Join(values[i:], " ")
			} else {
				locals[name] = ""
			}
		case i < len(values):
			locals[name] = values[i]
		default:
			locals[name] = ""
		}
	}
	p.pushLocal(locals)
	defer p.popLocal()
	return p.expandDepth(args[2], depth)
}

func (p *kbuildParser) evalEval(args []string, original string, depth int) (string, error) {
	if len(args) != 1 {
		return original, nil
	}
	expanded, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(expanded, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := p.parseLine(line, p.currentPos); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (p *kbuildParser) evalDiagnostic(args []string, original string, depth int, fatal bool) (string, error) {
	message, err := p.expandDepth(strings.Join(args, ","), depth)
	if err != nil {
		return "", err
	}
	if fatal {
		if !p.definitelyActive() {
			return original, nil
		}
		if strings.TrimSpace(message) == "" {
			message = original
		}
		return "", fmt.Errorf("%s: %s", p.currentPos, strings.TrimSpace(message))
	}
	return "", nil
}

func (p *kbuildParser) evalOrigin(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	if state, uncertain := p.symbolicVariables[name]; uncertain {
		if origin, known := state.exactOrigin(); known {
			return origin, nil
		}
		if defined, known := state.exactDefinedness(); known && defined {
			origin, selectErr := p.selectKnownOrSymbolicText(state.environmentWhen, "environment", "file")
			if selectErr != nil {
				return "", fmt.Errorf("retain probe-dependent Make origin of %q: %w", name, selectErr)
			}
			return origin, nil
		}
		return "", fmt.Errorf("Make origin observes probe-dependent definedness of %q", name)
	}
	for i := len(p.locals) - 1; i >= 0; i-- {
		if _, ok := p.locals[i][name]; ok {
			return "automatic", nil
		}
	}
	if _, ok := p.lookupVariable(name); ok {
		if p.commandLineVariables[name] {
			return "command line", nil
		}
		if p.environmentVariables[name] {
			return "environment", nil
		}
		return "file", nil
	}
	return "undefined", nil
}

func (p *kbuildParser) evalFlavor(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	if state, uncertain := p.symbolicVariables[name]; uncertain {
		if flavor, known := state.exactFlavor(); known {
			return flavor, nil
		}
		if defined, known := state.exactDefinedness(); known && defined {
			flavor, selectErr := p.selectKnownOrSymbolicText(state.simpleWhen, "simple", "recursive")
			if selectErr != nil {
				return "", fmt.Errorf("retain probe-dependent Make flavor of %q: %w", name, selectErr)
			}
			return flavor, nil
		}
		return "", fmt.Errorf("Make flavor observes probe-dependent flavor of %q", name)
	}
	for i := len(p.locals) - 1; i >= 0; i-- {
		if _, ok := p.locals[i][name]; ok {
			return "simple", nil
		}
	}
	variable, ok := p.lookupVariable(name)
	if !ok {
		return "undefined", nil
	}
	if variable.recursive {
		return "recursive", nil
	}
	return "simple", nil
}

func (p *kbuildParser) evalValue(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	if _, uncertain := p.symbolicVariables[name]; uncertain {
		return "", fmt.Errorf("Make value observes probe-dependent variable identity of %q", name)
	}
	value, ok := p.lookupRawVar(name)
	if !ok {
		return "", nil
	}
	return value, nil
}

func (p *kbuildParser) evalShell(args []string, original string, depth int) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%s: shell function requires exactly one command", p.currentPos)
	}
	command, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "", nil
	}
	// Action evaluation carries private tree markers so path joins retain
	// provenance until recipe lowering. Shell/probe callbacks are a separate
	// stable Make boundary and must observe the public source/object sentinels
	// captured by their declared filesystem maps.
	command = compactKbuildMaterializeActionTreeMarkers(command)
	if p.shell == nil {
		if p.sourceShell != nil {
			value, sourceErr := p.sourceShell(command, p.workingDir)
			if sourceErr == nil {
				return normalizeKbuildShellOutput(value)
			}
			if !IsLinuxProbeUnsupportedCommand(sourceErr) {
				return "", fmt.Errorf("%s: evaluate Kbuild source shell command %q: %w", p.currentPos, command, sourceErr)
			}
		}
		return "", fmt.Errorf("%s: Kbuild shell command %q requires a hermetic evaluator", p.currentPos, command)
	}
	value, err := p.shell(command)
	evaluatorOwned := err == nil
	if err != nil && p.sourceShell != nil && IsLinuxProbeUnsupportedCommand(err) {
		if sourceValue, sourceErr := p.sourceShell(command, p.workingDir); sourceErr == nil {
			value, err = sourceValue, nil
			evaluatorOwned = false
		} else if !IsLinuxProbeUnsupportedCommand(sourceErr) {
			return "", fmt.Errorf("%s: evaluate Kbuild source shell command %q: %w", p.currentPos, command, sourceErr)
		}
	}
	if err != nil {
		return "", fmt.Errorf("%s: evaluate Kbuild shell command %q: %w", p.currentPos, command, err)
	}
	if evaluatorOwned {
		return normalizeKbuildEvaluatorShellOutput(value)
	}
	return normalizeKbuildShellOutput(value)
}

func normalizeKbuildShellOutput(value string) (string, error) {
	if compactKbuildContainsPrivateProvenanceByte(value) || compactKbuildContainsPrivateToolsetPathByte(value) {
		return "", fmt.Errorf("Kbuild shell output contains a reserved provenance byte")
	}
	return NormalizeGNUMakeShellOutput(value), nil
}

func normalizeKbuildEvaluatorShellOutput(value string) (string, error) {
	if compactKbuildContainsPrivateProvenanceByte(value) {
		return "", fmt.Errorf("Kbuild shell output contains a reserved provenance byte")
	}
	if err := toolaction.ValidateExecutionRootProvenanceValue(value); err != nil {
		return "", fmt.Errorf("Kbuild evaluator shell output: %w", err)
	}
	return NormalizeGNUMakeShellOutput(value), nil
}

// NormalizeGNUMakeShellOutput applies GNU Make's $(shell ...) stdout
// reduction: all trailing line terminators disappear and embedded line breaks
// become spaces. Probe replay uses the same function before symbolic text can
// participate in Make topology.
func NormalizeGNUMakeShellOutput(value string) string {
	value = strings.TrimRight(value, "\r\n")
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(value)
}

func (p *kbuildParser) evalVariableIntrospectionName(args []string, depth int) (string, bool, error) {
	if len(args) != 1 {
		return "", false, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", false, err
	}
	if containsMakeReference(name) {
		return "", false, nil
	}
	if linuxProbeSymbolPattern.MatchString(name) {
		return "", false, fmt.Errorf("Make variable introspection name depends on an unresolved probe")
	}
	return strings.TrimSpace(name), true, nil
}

type kbuildSymbolicIfClassificationError struct {
	cause error
}

func (e *kbuildSymbolicIfClassificationError) Error() string {
	return "classify symbolic Make if: " + e.cause.Error()
}

func (e *kbuildSymbolicIfClassificationError) Unwrap() error {
	return e.cause
}

func (p *kbuildParser) evalIf(args []string, original string, depth int) (string, error) {
	if len(args) < 2 || len(args) > 3 {
		return original, nil
	}
	condition, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	if containsMakeReference(condition) && !p.expandedReferencesAreLiteral {
		return original, nil
	}
	if p.selectSymbolic != nil && linuxProbeSymbolPattern.MatchString(condition) {
		// Preserve GNU Make's lazy branch expansion when a symbolic condition
		// still has an invariant truth value. In particular, Kbuild's cmd
		// helper tests a complete command string: probe-selected flags may be
		// present in that string, but fixed compiler/output arguments prove it
		// non-empty for every probe outcome. Classify the truth first so only
		// the selected branch is expanded, including any legitimate expansion
		// effects in that branch.
		truth, recognized, truthErr := p.selectSymbolic(
			strings.TrimSpace(condition), "", false, "1", "",
		)
		if truthErr != nil {
			return "", &kbuildSymbolicIfClassificationError{cause: truthErr}
		}
		if !recognized {
			return "", fmt.Errorf("probe-dependent Make if does not retain exact symbolic truth")
		}
		if !linuxProbeSymbolPattern.MatchString(truth) {
			if strings.TrimSpace(truth) != "" {
				return p.expandDepth(args[1], depth)
			}
			if len(args) == 3 {
				return p.expandDepth(args[2], depth)
			}
			return "", nil
		}

		// A genuinely probe-dependent decision would expand both branches in
		// discovery. That is exact only for expansion-pure branches; effects
		// such as eval, shell, and file must remain lazy and therefore fail
		// closed here.
		for _, branch := range args[1:] {
			if p.makeExpansionHasStatefulEffect(branch, map[string]bool{}, depth) {
				return "", fmt.Errorf("probe-dependent Make if has a stateful branch in %q", original)
			}
		}
		trueValue, trueErr := p.expandDepth(args[1], depth)
		falseValue := ""
		var falseErr error
		if len(args) == 3 {
			falseValue, falseErr = p.expandDepth(args[2], depth)
		}
		if trueErr == nil && falseErr == nil {
			selected, recognized, selectErr := p.selectSymbolic(
				strings.TrimSpace(truth), "", false, trueValue, falseValue,
			)
			if selectErr != nil {
				return "", fmt.Errorf("retain symbolic Make if: %w", selectErr)
			}
			if recognized {
				return selected, nil
			}
		}
		if trueErr != nil {
			return "", trueErr
		}
		if falseErr != nil {
			return "", falseErr
		}
		return "", fmt.Errorf("probe-dependent Make if does not retain exact symbolic provenance")
	}
	if strings.TrimSpace(condition) != "" {
		return p.expandDepth(args[1], depth)
	}
	if len(args) == 3 {
		return p.expandDepth(args[2], depth)
	}
	return "", nil
}

func (p *kbuildParser) makeExpansionHasStatefulEffect(value string, visiting map[string]bool, depth int) bool {
	if depth > 100 {
		return true
	}
	for index := 0; index+1 < len(value); index++ {
		if value[index] != '$' {
			continue
		}
		if value[index+1] == '$' {
			index++
			continue
		}
		if value[index+1] != '(' && value[index+1] != '{' {
			if p.makeVariableReferenceHasStatefulEffect(value[index+1:index+2], visiting, depth+1) {
				return true
			}
			index++
			continue
		}
		end, err := matchingKbuildReference(value, index+1)
		if err != nil {
			return true
		}
		clause := value[index+2 : end]
		name, args, function := splitMakeFunction(clause)
		if function {
			if name == "foreach" {
				if p.makeForeachExpansionHasStatefulEffect(args, visiting, depth+1) {
					return true
				}
				index = end
				continue
			}
			if name == "let" {
				if p.makeLetExpansionHasStatefulEffect(args, visiting, depth+1) {
					return true
				}
				index = end
				continue
			}
			if name == "shell" {
				if len(args) != 1 || p.makeExpansionHasStatefulEffect(args[0], visiting, depth+1) {
					return true
				}
				command, expandErr := p.expandDepth(args[0], depth+1)
				if expandErr != nil {
					return true
				}
				command = strings.TrimSpace(command)
				if command == "" {
					index = end
					continue
				}
				command = compactKbuildMaterializeActionTreeMarkers(command)
				if p.shell == nil || p.shellResultAvailable == nil || !p.shellResultAvailable(command) {
					return true
				}
				index = end
				continue
			}
			switch name {
			case "eval", "file", "error":
				return true
			case "warning", "info":
				// Non-fatal diagnostics expand to the empty string and do not
				// mutate Make's command graph. Their arguments still need the
				// recursive scan below: an embedded eval, shell, file, or error
				// would remain branch-sensitive even though the diagnostic itself
				// is value-neutral.
			}
			for _, arg := range args {
				if p.makeExpansionHasStatefulEffect(arg, visiting, depth+1) {
					return true
				}
			}
			if name == "call" && len(args) != 0 {
				callName, expandErr := p.expandDepth(args[0], depth+1)
				callName = strings.TrimSpace(callName)
				if expandErr != nil || containsMakeVariableReference(callName) ||
					linuxProbeSymbolPattern.MatchString(callName) {
					return true
				}
				if callName == "" {
					index = end
					continue
				}
				body, ok := p.lookupRawVar(callName)
				if !ok {
					return true
				}
				callArgs := make([]string, 0, len(args)-1)
				for _, arg := range args[1:] {
					expanded, argErr := p.expandDepth(arg, depth+1)
					if argErr != nil {
						return true
					}
					callArgs = append(callArgs, expanded)
				}
				locals := kbuildCallLocals(callName, body, callArgs)
				visitKey := "call:" + callName + "\x00" + strings.Join(callArgs, "\x00")
				if visiting[visitKey] {
					return true
				}
				visiting[visitKey] = true
				p.pushLocal(locals)
				stateful := p.makeExpansionHasStatefulEffect(body, visiting, depth+1)
				p.popLocal()
				delete(visiting, visitKey)
				if stateful {
					return true
				}
			}
		} else if variable, pattern, replacement, substitution := splitMakeSubstitution(clause); substitution {
			if p.makeExpansionHasStatefulEffect(pattern, visiting, depth+1) ||
				p.makeExpansionHasStatefulEffect(replacement, visiting, depth+1) ||
				p.makeVariableReferenceHasStatefulEffect(variable, visiting, depth+1) {
				return true
			}
		} else if p.makeVariableReferenceHasStatefulEffect(clause, visiting, depth+1) {
			return true
		}
		index = end
	}
	return false
}

// makeVariableReferenceHasStatefulEffect resolves one ordinary or computed
// variable name without executing any stateful name expression, then scans the
// exact raw value selected in the current target/call-local context. Undefined
// variables are expansion-pure and evaluate to the empty string.
func (p *kbuildParser) makeVariableReferenceHasStatefulEffect(variable string, visiting map[string]bool, depth int) bool {
	if depth > 100 {
		return true
	}
	variable = strings.TrimSpace(variable)
	// Inspect the spelling before expanding it so a shell/eval/file hidden in a
	// computed name never executes merely because this classifier looked at an
	// unselected branch.
	if p.makeExpansionHasStatefulEffect(variable, visiting, depth+1) {
		return true
	}
	expandedName, _, complete, nameErr := p.expandComputedVariableName(variable, depth+1)
	if nameErr != nil || !complete || containsMakeVariableReference(expandedName) ||
		linuxProbeSymbolPattern.MatchString(expandedName) {
		return true
	}
	for index := len(p.locals) - 1; index >= 0; index-- {
		if _, ok := p.locals[index][expandedName]; ok {
			// Call/foreach/let locals contain values which their owning Make
			// function already expanded before binding the local.
			return false
		}
	}
	variableValue, ok := p.lookupVariable(expandedName)
	if !ok || !variableValue.recursive && !variableValue.deferredSimple {
		return false
	}
	raw := variableValue.value
	visitKey := "variable:" + expandedName + "\x00" + raw
	if visiting[visitKey] {
		return false
	}
	visiting[visitKey] = true
	stateful := p.makeExpansionHasStatefulEffect(raw, visiting, depth+1)
	delete(visiting, visitKey)
	return stateful
}

func (p *kbuildParser) makeForeachExpansionHasStatefulEffect(args []string, visiting map[string]bool, depth int) bool {
	if depth > 100 {
		return true
	}
	if len(args) != 3 {
		// evalForeach returns the original expression without expanding any
		// operands when the arity is invalid.
		return false
	}
	if p.makeExpansionHasStatefulEffect(args[0], visiting, depth+1) ||
		p.makeExpansionHasStatefulEffect(args[1], visiting, depth+1) {
		return true
	}
	name, nameErr := p.expandDepth(args[0], depth+1)
	list, listErr := p.expandDepth(args[1], depth+1)
	name = strings.TrimSpace(name)
	if nameErr != nil || listErr != nil || name == "" ||
		containsMakeVariableReference(name) || containsMakeVariableReference(list) ||
		linuxProbeSymbolPattern.MatchString(name) || linuxProbeSymbolPattern.MatchString(list) {
		return true
	}
	for _, word := range strings.Fields(list) {
		p.pushLocal(map[string]string{name: word})
		stateful := p.makeExpansionHasStatefulEffect(args[2], visiting, depth+1)
		p.popLocal()
		if stateful {
			return true
		}
	}
	return false
}

func (p *kbuildParser) makeLetExpansionHasStatefulEffect(args []string, visiting map[string]bool, depth int) bool {
	if depth > 100 {
		return true
	}
	if len(args) != 3 {
		// evalLet returns the original expression without expanding any
		// operands when the arity is invalid.
		return false
	}
	if p.makeExpansionHasStatefulEffect(args[1], visiting, depth+1) {
		return true
	}
	valuesText, valuesErr := p.expandDepth(args[1], depth+1)
	namesText := strings.TrimSpace(args[0])
	if valuesErr != nil || namesText == "" ||
		containsMakeVariableReference(namesText) || containsMakeVariableReference(valuesText) ||
		linuxProbeSymbolPattern.MatchString(namesText) || linuxProbeSymbolPattern.MatchString(valuesText) {
		return true
	}
	names := strings.Fields(namesText)
	if len(names) == 0 {
		return true
	}
	values := strings.Fields(valuesText)
	locals := map[string]string{}
	for index, name := range names {
		switch {
		case index == len(names)-1 && index < len(values):
			locals[name] = strings.Join(values[index:], " ")
		case index == len(names)-1:
			locals[name] = ""
		case index < len(values):
			locals[name] = values[index]
		default:
			locals[name] = ""
		}
	}
	p.pushLocal(locals)
	stateful := p.makeExpansionHasStatefulEffect(args[2], visiting, depth+1)
	p.popLocal()
	return stateful
}

func (p *kbuildParser) evalAnd(args []string, original string, depth int) (string, error) {
	result := ""
	for i, arg := range args {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		if i == len(args)-1 {
			// GNU Make returns the final expanded argument without testing it.
			// An unresolved probe value is therefore the exact result, not a
			// branch decision.
			return expanded, nil
		}
		if containsMakeReference(expanded) && !p.expandedReferencesAreLiteral {
			return original, nil
		}
		if linuxProbeSymbolPattern.MatchString(expanded) {
			return "", fmt.Errorf("Make and cannot decide an unresolved probe value")
		}
		if strings.TrimSpace(expanded) == "" {
			return "", nil
		}
		result = expanded
	}
	return result, nil
}

func (p *kbuildParser) evalOr(args []string, original string, depth int) (string, error) {
	for i, arg := range args {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		if i == len(args)-1 {
			// GNU Make returns the final expanded argument even when it is
			// empty. Preserve a final probe atom directly: no truth decision is
			// required at this position.
			return expanded, nil
		}
		if containsMakeReference(expanded) && !p.expandedReferencesAreLiteral {
			return original, nil
		}
		if linuxProbeSymbolPattern.MatchString(expanded) {
			return "", fmt.Errorf("Make or cannot decide an unresolved probe value")
		}
		if strings.TrimSpace(expanded) != "" {
			return expanded, nil
		}
	}
	return "", nil
}

type kbuildTreeParser struct {
	opts              KbuildOptions
	variableOverrides map[string]string
	seen              map[string]bool
	parsing           map[string]bool
}

func (p *kbuildTreeParser) parseInto(parser *kbuildParser, path string, depth int) error {
	if depth > p.opts.MaxIncludeDepth {
		return fmt.Errorf("%s: maximum Kbuild include depth exceeded", path)
	}
	resolved := p.resolvePath(path, parser.baseDir)
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if p.parsing[abs] {
		return fmt.Errorf("%s: recursive Kbuild include", path)
	}
	if p.seen[abs] {
		return nil
	}
	program, sourcePath, eligible, cached, err := p.opts.SourceCache.sourceProgram(abs)
	if err != nil {
		return err
	}
	var file *os.File
	if !cached {
		file, err = os.Open(sourcePath)
		if err != nil {
			return err
		}
		defer file.Close()
	}

	baseDir := filepath.Dir(abs)
	previousBaseDir := parser.baseDir
	previousDepth := parser.includeDepth
	parser.baseDir = baseDir
	parser.includeDepth = depth
	p.parsing[abs] = true
	if cached {
		err = parser.parseSourceProgram(program, abs)
	} else {
		if eligible {
			p.opts.SourceCache.recordSourceRead()
		}
		program, err = parser.parseReaderAndCapture(file, abs)
		if err == nil && eligible {
			p.opts.SourceCache.storeSourceProgram(sourcePath, program)
		}
	}
	delete(p.parsing, abs)
	parser.baseDir = previousBaseDir
	parser.includeDepth = previousDepth
	if err != nil {
		return err
	}
	p.seen[abs] = true
	return nil
}

func (p *kbuildTreeParser) parseIncludes(parser *kbuildParser, includes []KbuildInclude) error {
	baseDir := parser.baseDir
	depth := parser.includeDepth + 1
	for _, include := range includes {
		includePath, ok := p.resolveInclude(include.Path, baseDir)
		if !ok {
			continue
		}
		err := p.parseInto(parser, includePath, depth)
		if err != nil {
			if os.IsNotExist(err) && (include.Optional || (p.opts.ConfigVariablesComplete && generatedConfigMakeInclude(includePath))) {
				continue
			}
			return err
		}
	}
	return nil
}

func generatedConfigMakeInclude(value string) bool {
	value = filepath.ToSlash(filepath.Clean(value))
	return strings.HasSuffix(value, "/include/config/auto.conf") ||
		strings.HasSuffix(value, "/include/config/auto.conf.cmd") ||
		value == "include/config/auto.conf" ||
		value == "include/config/auto.conf.cmd"
}

func (p *kbuildTreeParser) resolvePath(path, baseDir string) string {
	path = p.expand(path)
	if filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if p.opts.WorkingDir != "" {
		candidate := filepath.Join(p.opts.WorkingDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if baseDir != "" {
		candidate := filepath.Join(baseDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p.opts.RootDir != "" {
		candidate := filepath.Join(p.opts.RootDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if mapped, ok := mappedSourceRootPath(path, p.opts.SourceRoots); ok {
		return mapped
	}
	if p.opts.WorkingDir != "" {
		return filepath.Join(p.opts.WorkingDir, path)
	}
	if baseDir != "" {
		return filepath.Join(baseDir, path)
	}
	if p.opts.RootDir != "" {
		return filepath.Join(p.opts.RootDir, path)
	}
	return path
}

func (p *kbuildTreeParser) resolveInclude(path, baseDir string) (string, bool) {
	expanded := p.expand(path)
	if strings.Contains(expanded, "$") || expanded == "" {
		return "", false
	}
	return p.resolvePath(expanded, baseDir), true
}

func (p *kbuildTreeParser) expand(value string) string {
	if !containsMakeReference(value) {
		return value
	}
	for key, val := range p.variableOverrides {
		value = strings.ReplaceAll(value, "$("+key+")", val)
		value = strings.ReplaceAll(value, "${"+key+"}", val)
	}
	for key, val := range p.opts.Variables {
		if _, overridden := p.variableOverrides[key]; overridden {
			continue
		}
		value = strings.ReplaceAll(value, "$("+key+")", val)
		value = strings.ReplaceAll(value, "${"+key+"}", val)
	}
	return value
}

func stripKbuildComment(line string) string {
	var output strings.Builder
	output.Grow(len(line))
	var closers []byte
	for i := 0; i < len(line); {
		// Outside a Make reference, backslashes immediately before '#' are
		// consumed in pairs. An odd final backslash quotes the hash and is
		// itself removed; with an even count, the hash starts a comment. This
		// is why Linux can define `pound := \#` and later use a one-byte '#'
		// subst pattern in scripts/Kbuild.include.
		if len(closers) == 0 && line[i] == '\\' {
			end := i
			for end < len(line) && line[end] == '\\' {
				end++
			}
			if end < len(line) && line[end] == '#' {
				output.WriteString(strings.Repeat("\\", (end-i)/2))
				if (end-i)%2 == 0 {
					return output.String()
				}
				output.WriteByte('#')
				i = end + 1
				continue
			}
			output.WriteString(line[i:end])
			i = end
			continue
		}
		if line[i] == '#' && len(closers) == 0 {
			return output.String()
		}
		if line[i] == '$' && i+1 < len(line) && !makeEscaped(line, i) {
			switch line[i+1] {
			case '(':
				closers = append(closers, ')')
				output.WriteString(line[i : i+2])
				i += 2
				continue
			case '{':
				closers = append(closers, '}')
				output.WriteString(line[i : i+2])
				i += 2
				continue
			}
		}
		if len(closers) != 0 {
			switch {
			case line[i] == '(' && closers[len(closers)-1] == ')':
				closers = append(closers, ')')
			case line[i] == '{' && closers[len(closers)-1] == '}':
				closers = append(closers, '}')
			case line[i] == closers[len(closers)-1]:
				closers = closers[:len(closers)-1]
			}
		}
		output.WriteByte(line[i])
		i++
	}
	return output.String()
}

func makeEscaped(line string, index int) bool {
	backslashes := 0
	for i := index - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func splitKbuildAssignment(line string) (string, string, string, bool) {
	line = stripMakeAssignmentModifiers(line)
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '(', '{':
			depth++
			continue
		case ')', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		for _, op := range []string{"+=", ":=", "?=", "="} {
			if strings.HasPrefix(line[i:], op) {
				return strings.TrimSpace(line[:i]), op, strings.TrimSpace(line[i+len(op):]), true
			}
		}
	}
	return "", "", "", false
}

func splitKbuildRule(line string) (string, string, string, string, bool) {
	colon := -1
	separator := ""
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case '&':
			if depth == 0 && i+1 < len(line) && line[i+1] == ':' {
				colon = i
				separator = "&:"
				i = len(line)
			}
		case ':':
			if depth != 0 {
				continue
			}
			if i+1 < len(line) && line[i+1] == '=' {
				return "", "", "", "", false
			}
			colon = i
			separator = ":"
			if i+1 < len(line) && line[i+1] == ':' {
				separator = "::"
			}
			i = len(line)
		}
	}
	if colon < 0 {
		return "", "", "", "", false
	}
	targets := strings.TrimSpace(line[:colon])
	if targets == "" {
		return "", "", "", "", false
	}
	rest := line[colon+len(separator):]
	// A target-specific variable value may itself begin with a shell command
	// separator.  GNU Make uses this form for postprocessors, for example:
	//
	//   output: private command_extra = ; sed -i ... $@
	//
	// Recognize the assignment before looking for an inline rule recipe so the
	// separator remains part of the variable value.  Splitting first would turn
	// the value into an empty assignment and silently discard the postprocessor
	// when parseRule returns after recording the target variable.
	if assignment := strings.TrimSpace(rest); assignment != "" {
		if _, _, _, _, targetVariable := splitKbuildTargetVariable(assignment); targetVariable {
			return targets, separator, assignment, "", true
		}
	}
	prerequisites, recipe := splitKbuildInlineRecipe(rest)
	return targets, separator, strings.TrimSpace(prerequisites), recipe, true
}

func splitKbuildInlineRecipe(value string) (string, string) {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return value[:i], value[i+1:]
			}
		}
	}
	return value, ""
}

func splitKbuildTargetVariable(value string) (string, string, string, []string, bool) {
	modifiers, assignment := splitMakeAssignmentModifiers(value)
	lhs, op, rhs, ok := splitKbuildAssignment(assignment)
	if !ok || lhs == "" || strings.ContainsAny(lhs, " \t") {
		return "", "", "", nil, false
	}
	return lhs, op, rhs, modifiers, true
}

func splitKbuildDefine(line string) (string, string, bool) {
	rest, ok := makeDirectiveRest(stripMakeAssignmentModifiers(line), "define")
	if !ok {
		return "", "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", false
	}
	if len(fields) > 2 {
		return "", "", false
	}
	name := fields[0]
	op := "="
	if len(fields) > 1 {
		switch fields[1] {
		case "=", ":=", "+=", "?=":
			op = fields[1]
		default:
			return "", "", false
		}
	}
	return name, op, true
}

func stripMakeAssignmentModifiers(line string) string {
	_, rest := splitMakeAssignmentModifiers(line)
	return rest
}

func splitMakeAssignmentModifiers(line string) ([]string, string) {
	line = strings.TrimSpace(line)
	var modifiers []string
	for {
		stripped := false
		for _, modifier := range []string{"export", "unexport", "override", "private"} {
			rest, ok := makeDirectiveRest(line, modifier)
			if ok {
				modifiers = append(modifiers, modifier)
				line = rest
				stripped = true
				break
			}
		}
		if !stripped {
			return modifiers, line
		}
	}
}

func makeDirectiveRest(line, keyword string) (string, bool) {
	if line == keyword {
		return "", true
	}
	rest, ok := strings.CutPrefix(line, keyword)
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t' && rest[0] != '(') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func splitMakeSubstitution(clause string) (string, string, string, bool) {
	colon := -1
	depth := 0
	for i := 0; i < len(clause); i++ {
		switch clause[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				colon = i
				i = len(clause)
			}
		}
	}
	if colon <= 0 {
		return "", "", "", false
	}
	equals := -1
	depth = 0
	for i := colon + 1; i < len(clause); i++ {
		switch clause[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				equals = i
				i = len(clause)
			}
		}
	}
	if equals < 0 {
		return "", "", "", false
	}
	return clause[:colon], clause[colon+1 : equals], clause[equals+1:], true
}

type kbuildConditionalEval struct {
	known        bool
	value        bool
	condition    KbuildCondition
	hasCondition bool
	// symbolicCondition is nonempty when the Make conditional depends on an
	// exact compiler probe. It is intentionally retained in replay as well as
	// discovery so later probes see one stable conditional-argument DAG.
	symbolicCondition string
}

func (p *kbuildParser) evalConditional(keyword, rest string) (kbuildConditionalEval, error) {
	complete := p.active() && p.configVariablesComplete && p.makeVariablesComplete
	switch keyword {
	case "ifeq", "ifneq":
		left, right, ok := parseMakeConditionArgs(rest)
		if !ok {
			return kbuildConditionalEval{}, nil
		}
		if !p.configVariablesComplete {
			if condition, ok := makeConfigComparisonCondition(keyword, left, right); ok {
				return kbuildConditionalEval{condition: condition, hasCondition: true}, nil
			}
		}
		leftExpanded, leftErr := p.expand(left)
		rightExpanded, rightErr := p.expand(right)
		if leftErr == nil && rightErr == nil && p.selectSymbolic != nil {
			equal := keyword == "ifeq"
			selected, recognized, selectErr := p.selectSymbolic(
				strings.TrimSpace(leftExpanded), strings.TrimSpace(rightExpanded), equal, "1", "",
			)
			if selectErr != nil {
				return kbuildConditionalEval{}, fmt.Errorf("retain symbolic conditional: %w", selectErr)
			}
			if recognized {
				if linuxProbeSymbolPattern.MatchString(selected) {
					return kbuildConditionalEval{symbolicCondition: selected}, nil
				}
				return kbuildConditionalEval{known: true, value: strings.TrimSpace(selected) != ""}, nil
			}
		}
		if leftErr == nil {
			leftExpanded, leftErr = p.resolveKbuildSymbolic(leftExpanded)
			if leftErr != nil {
				return kbuildConditionalEval{}, leftErr
			}
		}
		if rightErr == nil {
			rightExpanded, rightErr = p.resolveKbuildSymbolic(rightExpanded)
			if rightErr != nil {
				return kbuildConditionalEval{}, rightErr
			}
		}
		if leftErr != nil || rightErr != nil {
			if complete {
				if leftErr != nil {
					return kbuildConditionalEval{}, fmt.Errorf("expand left conditional operand: %w", leftErr)
				}
				return kbuildConditionalEval{}, fmt.Errorf("expand right conditional operand: %w", rightErr)
			}
			return kbuildConditionalEval{}, nil
		}
		if containsMakeReference(leftExpanded) || containsMakeReference(rightExpanded) ||
			linuxProbeSymbolPattern.MatchString(leftExpanded) || linuxProbeSymbolPattern.MatchString(rightExpanded) {
			return kbuildConditionalEval{}, nil
		}
		equal := strings.TrimSpace(leftExpanded) == strings.TrimSpace(rightExpanded)
		if keyword == "ifneq" {
			return kbuildConditionalEval{known: true, value: !equal}, nil
		}
		return kbuildConditionalEval{known: true, value: equal}, nil
	case "ifdef", "ifndef":
		name, err := p.expand(strings.TrimSpace(rest))
		if err != nil {
			if complete {
				return kbuildConditionalEval{}, fmt.Errorf("expand conditional variable name: %w", err)
			}
		}
		if linuxProbeSymbolPattern.MatchString(name) {
			return kbuildConditionalEval{}, fmt.Errorf("probe-dependent ifdef variable name %q is unsupported", name)
		}
		if err != nil || containsMakeReference(name) {
			rawName := strings.TrimSpace(rest)
			if strings.HasPrefix(rawName, "CONFIG_") {
				condition := KbuildCondition{Kind: "config_ne", Symbol: rawName, State: "n"}
				if keyword == "ifndef" {
					condition = invertKbuildCondition(condition)
				}
				return kbuildConditionalEval{condition: condition, hasCondition: true}, nil
			}
			return kbuildConditionalEval{}, nil
		}
		name = strings.TrimSpace(name)
		if state, uncertain := p.symbolicVariables[name]; uncertain {
			defined, known := state.exactDefinedness()
			if !known {
				return kbuildConditionalEval{}, fmt.Errorf("%s observes probe-dependent definedness of %q", keyword, name)
			}
			if !defined {
				return kbuildConditionalEval{known: true, value: keyword == "ifndef"}, nil
			}
		}
		value, ok := p.lookupRawVar(name)
		if !ok && strings.HasPrefix(name, "CONFIG_") {
			if p.configVariablesComplete {
				return kbuildConditionalEval{known: true, value: keyword == "ifndef"}, nil
			}
			condition := KbuildCondition{Kind: "config_ne", Symbol: name, State: "n"}
			if keyword == "ifndef" {
				condition = invertKbuildCondition(condition)
			}
			return kbuildConditionalEval{condition: condition, hasCondition: true}, nil
		}
		if ok && p.selectSymbolic != nil && linuxProbeSymbolPattern.MatchString(value) {
			selected, recognized, selectErr := p.selectSymbolic(value, "", keyword == "ifndef", "1", "")
			if selectErr != nil {
				return kbuildConditionalEval{}, fmt.Errorf("retain symbolic %s: %w", keyword, selectErr)
			}
			if recognized {
				if linuxProbeSymbolPattern.MatchString(selected) {
					return kbuildConditionalEval{symbolicCondition: selected}, nil
				}
				return kbuildConditionalEval{known: true, value: strings.TrimSpace(selected) != ""}, nil
			}
		}
		if ok {
			value, err = p.resolveKbuildSymbolic(value)
			if err != nil {
				return kbuildConditionalEval{}, err
			}
			if linuxProbeSymbolPattern.MatchString(value) {
				return kbuildConditionalEval{}, nil
			}
		}
		set := ok && strings.TrimSpace(value) != ""
		if keyword == "ifndef" {
			return kbuildConditionalEval{known: true, value: !set}, nil
		}
		return kbuildConditionalEval{known: true, value: set}, nil
	default:
		return kbuildConditionalEval{}, nil
	}
}

func makeConfigComparisonCondition(keyword, left, right string) (KbuildCondition, bool) {
	leftSymbol, leftConfig := unwrapConfigReference(strings.TrimSpace(left))
	rightSymbol, rightConfig := unwrapConfigReference(strings.TrimSpace(right))
	if leftConfig == rightConfig {
		return KbuildCondition{}, false
	}
	symbol := leftSymbol
	state := strings.TrimSpace(right)
	if rightConfig {
		symbol = rightSymbol
		state = strings.TrimSpace(left)
	}
	state = strings.Trim(state, `"'`)
	if state == "" {
		state = "n"
	}
	if state != "y" && state != "m" && state != "n" {
		return KbuildCondition{}, false
	}
	kind := "config_eq"
	if keyword == "ifneq" {
		kind = "config_ne"
	}
	return KbuildCondition{Kind: kind, Symbol: symbol, State: state}, true
}

func parseMakeConditionArgs(rest string) (string, string, bool) {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		end, err := matchingParen(rest, 0)
		if err != nil || strings.TrimSpace(rest[end+1:]) != "" {
			return "", "", false
		}
		args := splitFunctionArgs(rest[1:end])
		if len(args) != 2 {
			return "", "", false
		}
		return unquoteMakeConditionArg(args[0]), unquoteMakeConditionArg(args[1]), true
	}
	left, remaining, ok := readMakeConditionWord(rest)
	if !ok {
		return "", "", false
	}
	right, remaining, ok := readMakeConditionWord(strings.TrimSpace(remaining))
	if !ok || strings.TrimSpace(remaining) != "" {
		return "", "", false
	}
	return left, right, true
}

func readMakeConditionWord(rest string) (string, string, bool) {
	if rest == "" {
		return "", "", false
	}
	if rest[0] == '"' || rest[0] == '\'' {
		quote := rest[0]
		for i := 1; i < len(rest); i++ {
			if rest[i] == quote {
				return rest[1:i], rest[i+1:], true
			}
		}
		return "", "", false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' || rest[i] == '\t' {
			return rest[:i], rest[i:], true
		}
	}
	return rest, "", true
}

func unquoteMakeConditionArg(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func containsMakeReference(value string) bool {
	return strings.Contains(value, "$(") || strings.Contains(value, "${")
}

// containsMakeVariableReference recognizes both bracketed references and
// Make's single-character form ($@, $<, $*, ...). Keep this narrower than a
// raw dollar check: $$ is an escaped dollar, not a variable reference.
//
// Most parser call sites intentionally use containsMakeReference because a
// shell command may contain ordinary shell-dollar syntax. Computed Make
// variable names, however, must expand every form GNU Make accepts.
func containsMakeVariableReference(value string) bool {
	for i := 0; i+1 < len(value); i++ {
		if value[i] != '$' {
			continue
		}
		if value[i+1] == '$' {
			i++
			continue
		}
		return true
	}
	return false
}

func matchingKbuildReference(in string, open int) (int, error) {
	if in[open] == '(' {
		return matchingParen(in, open)
	}
	depth := 0
	for i := open + 1; i < len(in); i++ {
		switch in[i] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated reference %q: missing '}'", in[open:])
}

func splitMakeFunction(clause string) (string, []string, bool) {
	clause = strings.TrimSpace(clause)
	for _, name := range []string{
		"abspath",
		"addprefix",
		"addsuffix",
		"and",
		"basename",
		"call",
		"dir",
		"error",
		"eval",
		"file",
		"filter",
		"filter-out",
		"findstring",
		"foreach",
		"firstword",
		"flavor",
		"if",
		"info",
		"intcmp",
		"join",
		"lastword",
		"let",
		"notdir",
		"or",
		"origin",
		"patsubst",
		"realpath",
		"shell",
		"sort",
		"strip",
		"subst",
		"suffix",
		"value",
		"warning",
		"wildcard",
		"word",
		"wordlist",
		"words",
	} {
		rest, ok := strings.CutPrefix(clause, name)
		if ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			return name, splitFunctionArgs(strings.TrimSpace(rest)), true
		}
	}
	return "", nil, false
}

func (p *kbuildParser) evalMakeFunction(name string, args []string, original string) string {
	// subst is the one pure function that GNU Make can still apply when the
	// text argument contains a reference that this bounded evaluator retained.
	switch name {
	case "subst":
		if len(args) != 3 {
			return original
		}
		if (containsMakeReference(args[0]) || containsMakeReference(args[1])) &&
			!p.expandedReferencesAreLiteral {
			return original
		}
		return strings.ReplaceAll(args[2], args[0], args[1])
	case "filter", "filter-out":
		if len(args) == 2 && len(p.renderedValueProjections) != 0 {
			return p.filterMakeWordsWithProjections(args[0], args[1], name == "filter-out")
		}
	}
	if makeArgsContainReference(args) && !p.expandedReferencesAreLiteral {
		return original
	}
	if value, recognized, err := evalPureKbuildMakeFunction(name, args, original); recognized {
		if err != nil {
			return original
		}
		return value
	}
	return original
}

// evalPureKbuildMakeFunction is the context-free subset that can be replayed
// over a measured text result without consulting the filesystem or parser
// state. The symbolic evaluator uses the same implementation as ordinary Make
// expansion so a derived topology atom cannot drift between the two phases.
func evalPureKbuildMakeFunction(name string, args []string, original string) (string, bool, error) {
	badArity := func() (string, bool, error) {
		return "", true, fmt.Errorf("Make function %q has invalid argument count %d", name, len(args))
	}
	switch name {
	case "subst":
		if len(args) != 3 {
			return badArity()
		}
		if args[0] == "" {
			// GNU Make treats the empty search string as one match after the
			// complete input, not as a match at every character boundary.
			return args[2] + args[1], true, nil
		}
		return strings.ReplaceAll(args[2], args[0], args[1]), true, nil
	case "addprefix":
		if len(args) != 2 {
			return badArity()
		}
		return mapMakeWords(args[1], func(word string) string { return args[0] + word }), true, nil
	case "addsuffix":
		if len(args) != 2 {
			return badArity()
		}
		return mapMakeWords(args[1], func(word string) string { return word + args[0] }), true, nil
	case "basename":
		if len(args) != 1 {
			return badArity()
		}
		return mapMakeWords(args[0], makeBasename), true, nil
	case "dir":
		if len(args) != 1 {
			return badArity()
		}
		return mapMakeWords(args[0], makeDir), true, nil
	case "filter", "filter-out":
		if len(args) != 2 {
			return badArity()
		}
		return filterMakeWords(args[0], args[1], name == "filter-out"), true, nil
	case "findstring":
		if len(args) != 2 {
			return badArity()
		}
		if strings.Contains(args[1], args[0]) {
			return args[0], true, nil
		}
		return "", true, nil
	case "firstword", "lastword":
		if len(args) != 1 {
			return badArity()
		}
		words := strings.Fields(args[0])
		if len(words) == 0 {
			return "", true, nil
		}
		if name == "firstword" {
			return words[0], true, nil
		}
		return words[len(words)-1], true, nil
	case "intcmp":
		if len(args) < 2 || len(args) > 5 {
			return badArity()
		}
		value, ok := makeIntcmp(args)
		if !ok {
			return original, true, nil
		}
		return value, true, nil
	case "join":
		if len(args) != 2 {
			return badArity()
		}
		return makeJoin(args[0], args[1]), true, nil
	case "notdir":
		if len(args) != 1 {
			return badArity()
		}
		return mapMakeWords(args[0], makeNotdir), true, nil
	case "patsubst":
		if len(args) != 3 {
			return badArity()
		}
		pattern := strings.TrimSpace(args[0])
		replacement := strings.TrimSpace(args[1])
		return mapMakeWordsDropEmpty(args[2], func(word string) string { return makePatsubst(pattern, replacement, word) }), true, nil
	case "sort":
		if len(args) != 1 {
			return badArity()
		}
		words := strings.Fields(args[0])
		sort.Strings(words)
		out := words[:0]
		for _, word := range words {
			if len(out) == 0 || out[len(out)-1] != word {
				out = append(out, word)
			}
		}
		return strings.Join(out, " "), true, nil
	case "strip":
		if len(args) != 1 {
			return badArity()
		}
		return strings.Join(strings.Fields(args[0]), " "), true, nil
	case "suffix":
		if len(args) != 1 {
			return badArity()
		}
		return mapMakeWordsDropEmpty(args[0], makeSuffix), true, nil
	case "word":
		if len(args) != 2 {
			return badArity()
		}
		return makeWord(args[0], args[1]), true, nil
	case "wordlist":
		if len(args) != 3 {
			return badArity()
		}
		return makeWordList(args[0], args[1], args[2]), true, nil
	case "words":
		if len(args) != 1 {
			return badArity()
		}
		return fmt.Sprintf("%d", len(strings.Fields(args[0]))), true, nil
	default:
		return "", false, nil
	}
}

// ValidateKbuildOrdinaryValue closes an ordinary-text-to-Make boundary over
// the private recursive-Make delimiters. Reject each delimiter independently,
// rather than only the complete token: Make string functions can copy, delete,
// reorder, and concatenate inputs, but cannot synthesize either reserved byte
// from delimiter-free ordinary data.
func ValidateKbuildOrdinaryValue(operation, value string) error {
	if compactKbuildContainsPrivateProvenanceByte(value) {
		return fmt.Errorf("%s contains a reserved recursive Make provenance byte", operation)
	}
	if compactKbuildContainsPrivateToolsetPathByte(value) {
		return fmt.Errorf("%s contains a reserved toolset-path provenance byte", operation)
	}
	return nil
}

// ValidateKbuildOrdinaryVariables validates both names and values before a
// configured variable map crosses into the Kbuild evaluator.
func ValidateKbuildOrdinaryVariables(operation string, variables map[string]string) error {
	names := make([]string, 0, len(variables))
	for name := range variables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ValidateKbuildOrdinaryValue(operation+" variable name", name); err != nil {
			return err
		}
		if err := ValidateKbuildOrdinaryValue(fmt.Sprintf("%s variable %q", operation, name), variables[name]); err != nil {
			return err
		}
	}
	return nil
}

func (p *kbuildParser) expandWildcard(patterns string) (string, error) {
	var out []string
	for _, pattern := range strings.Fields(patterns) {
		matches, relBase := p.glob(pattern)
		pattern = filepath.ToSlash(pattern)
		query := compactKbuildMaterializeActionTreeMarkers(pattern)
		_, _, actionRooted := compactKbuildActionTreeRoot(pattern)
		var lazyMatches []string
		if p.virtualFileView != nil {
			lazyMatches = p.virtualFileView.Match(query)
			for _, match := range lazyMatches {
				if err := ValidateKbuildOrdinaryValue("Kbuild virtual wildcard result", match); err != nil {
					return "", err
				}
				if compactKbuildContainsPrivateActionMarker(match) {
					return "", fmt.Errorf("Kbuild virtual wildcard result contains a reserved private action marker")
				}
			}
			if actionRooted {
				for index := range lazyMatches {
					lazyMatches[index] = compactKbuildRestoreActionTreeMarkers(lazyMatches[index], pattern)
				}
			}
		}
		visible := make([]string, 0, len(matches)+len(lazyMatches))
		for _, match := range matches {
			if relBase != "" {
				if rel, err := filepath.Rel(relBase, match); err == nil {
					match = rel
				}
			}
			visible = append(visible, filepath.ToSlash(match))
		}
		for _, candidate := range lazyMatches {
			visible = append(visible, filepath.ToSlash(candidate))
		}
		for _, match := range visible {
			// Private action roots in a result can only come from this parser's
			// planner-owned query or mapped physical lookup. Validate their public
			// spelling so arbitrary virtual-file contents still cannot introduce a
			// reserved provenance byte.
			ordinary := compactKbuildMaterializeActionTreeRoot(match, pattern)
			if err := ValidateKbuildOrdinaryValue("Kbuild wildcard result", ordinary); err != nil {
				return "", err
			}
		}
		sort.Strings(visible)
		out = append(out, slices.Compact(visible)...)
	}
	return strings.Join(out, " "), nil
}

func (p *kbuildParser) makeAbsPath(word string) string {
	// Stable source-root names are Make-visible paths, even though they are not
	// absolute paths according to the host filesystem.  Do not accidentally
	// anchor them below the directory containing the Makefile: that would bake
	// the analysis execroot into every $(abspath ...) result.
	if _, prefix, ok := p.mappedFilesystemPath(word); ok {
		cleaned := filepath.ToSlash(filepath.Clean(word))
		// Action lowering normally projects an object-root path relative to the
		// typed Make cwd. GNU Make's abspath is observably different: its result
		// must stay absolute even when it is carried in a command-local environment
		// assignment (Linux's Rust OBJTREE contract relies on that distinction).
		// Preserve that provenance without changing public parse-time sentinels or
		// assigning meaning to the receiving environment-variable name.
		if prefix == compactKbuildActionObjectTreeMarker {
			if cleaned == prefix {
				return compactKbuildActionAbsoluteObjectTreeMarker
			}
			if suffix, rooted := strings.CutPrefix(cleaned, prefix+"/"); rooted {
				return compactKbuildActionAbsoluteObjectTreeMarker + "/" + suffix
			}
		}
		return cleaned
	}
	if filepath.IsAbs(word) {
		return filepath.ToSlash(filepath.Clean(word))
	}
	if p.workingDir != "" {
		return filepath.ToSlash(filepath.Clean(filepath.Join(p.workingDir, word)))
	}
	if p.baseDir != "" {
		return filepath.ToSlash(filepath.Clean(filepath.Join(p.baseDir, word)))
	}
	abs, err := filepath.Abs(word)
	if err != nil {
		return filepath.ToSlash(filepath.Clean(word))
	}
	return filepath.ToSlash(abs)
}

func (p *kbuildParser) expandAbsPaths(words string) (string, error) {
	resolved := make([]string, 0, len(strings.Fields(words)))
	for _, word := range strings.Fields(words) {
		value := p.makeAbsPath(word)
		if err := ValidateKbuildOrdinaryValue("Kbuild abspath result", value); err != nil {
			return "", err
		}
		resolved = append(resolved, value)
	}
	return strings.Join(resolved, " "), nil
}

func (p *kbuildParser) makeRealPath(word string) string {
	abs := p.makeAbsPath(word)
	filesystemPath, prefix, mapped := p.mappedFilesystemPath(abs)
	if !mapped {
		filesystemPath = abs
	}
	resolved, err := filepath.EvalSymlinks(filesystemPath)
	if err != nil {
		return ""
	}
	if mapped {
		root, ok := p.sourceRoots[prefix]
		if !ok {
			return ""
		}
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			resolvedRoot = filepath.Clean(root)
		}
		relative, relErr := filepath.Rel(resolvedRoot, resolved)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			// A source-root symlink escaping its declared tree is not a stable
			// action input and therefore has no hermetic realpath spelling.
			return ""
		}
		if relative == "." {
			return prefix
		}
		return filepath.ToSlash(filepath.Join(prefix, relative))
	}
	return filepath.ToSlash(resolved)
}

func (p *kbuildParser) expandRealPaths(words string) (string, error) {
	resolved := make([]string, 0, len(strings.Fields(words)))
	for _, word := range strings.Fields(words) {
		value := p.makeRealPath(word)
		if value == "" {
			continue
		}
		if err := ValidateKbuildOrdinaryValue("Kbuild realpath result", value); err != nil {
			return "", err
		}
		resolved = append(resolved, value)
	}
	return strings.Join(resolved, " "), nil
}

func (p *kbuildParser) makeFile(arg, original string) (string, error) {
	path, ok := strings.CutPrefix(strings.TrimSpace(arg), "<")
	if !ok {
		return original, nil
	}
	path = strings.TrimSpace(path)
	if path == "" || containsMakeReference(path) {
		return "", nil
	}
	virtualPath := filepath.ToSlash(filepath.Clean(path))
	if p.virtualFileView != nil {
		// Action lowering replaces evaluator-owned roots with private control-byte
		// markers. The virtual view models the Make-visible filesystem and speaks
		// the public sentinel namespace, so normalize only its query. Keep path
		// unchanged for the physical fallback below, whose source-root map owns the
		// private aliases.
		query := compactKbuildMaterializeActionTreeMarkers(virtualPath)
		contents, exists, exact, err := p.virtualFileView.Read(query)
		if err != nil {
			return "", err
		}
		if exists {
			if !exact {
				return "", fmt.Errorf("Kbuild file read of visible virtual file %q requires exact contents", virtualPath)
			}
			contents = strings.TrimSuffix(contents, "\n")
			if err := ValidateKbuildOrdinaryValue("Kbuild virtual file contents", contents); err != nil {
				return "", err
			}
			return contents, nil
		}
	}
	if mapped, _, ok := p.mappedFilesystemPath(path); ok {
		path = mapped
	} else if !filepath.IsAbs(path) && p.workingDir != "" {
		path = filepath.Join(p.workingDir, path)
	} else if !filepath.IsAbs(path) && p.baseDir != "" {
		path = filepath.Join(p.baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	contents := strings.TrimSuffix(string(data), "\n")
	if err := ValidateKbuildOrdinaryValue("Kbuild file contents", contents); err != nil {
		return "", err
	}
	return contents, nil
}

// mappedFilesystemPath resolves a stable Make-visible source-root path solely
// for a filesystem operation.  Callers must keep the returned prefix and map
// results back to it; the physical path is never part of the evaluated Make
// value or the serialized action recipe.
func (p *kbuildParser) mappedFilesystemPath(path string) (string, string, bool) {
	if len(p.sourceRoots) == 0 {
		return "", "", false
	}
	type mapping struct {
		key    string
		prefix string
	}
	mappings := make([]mapping, 0, len(p.sourceRoots))
	for key := range p.sourceRoots {
		prefix := strings.Trim(filepath.ToSlash(key), "/")
		if prefix != "" && prefix != "." {
			mappings = append(mappings, mapping{key: key, prefix: prefix})
		}
	}
	sort.Slice(mappings, func(i, j int) bool {
		if len(mappings[i].prefix) == len(mappings[j].prefix) {
			return mappings[i].prefix < mappings[j].prefix
		}
		return len(mappings[i].prefix) > len(mappings[j].prefix)
	})
	path = filepath.ToSlash(path)
	for _, candidate := range mappings {
		if path != candidate.prefix && !strings.HasPrefix(path, candidate.prefix+"/") {
			continue
		}
		relative := strings.TrimPrefix(path, candidate.prefix)
		relative = strings.TrimPrefix(relative, "/")
		return filepath.Join(p.sourceRoots[candidate.key], filepath.FromSlash(relative)), candidate.key, true
	}
	return "", "", false
}

func (p *kbuildParser) glob(pattern string) ([]string, string) {
	prefixes := make([]string, 0, len(p.sourceRoots))
	for prefix := range p.sourceRoots {
		prefix = strings.Trim(filepath.ToSlash(prefix), "/")
		if prefix != "" && (pattern == prefix || strings.HasPrefix(filepath.ToSlash(pattern), prefix+"/")) {
			prefixes = append(prefixes, prefix)
		}
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	if len(prefixes) != 0 {
		prefix := prefixes[0]
		relPattern := strings.TrimPrefix(filepath.ToSlash(pattern), prefix)
		relPattern = strings.TrimPrefix(relPattern, "/")
		matches, err := filepath.Glob(filepath.Join(p.sourceRoots[prefix], filepath.FromSlash(relPattern)))
		if err != nil {
			return nil, ""
		}
		virtual := make([]string, 0, len(matches))
		for _, match := range matches {
			rel, relErr := filepath.Rel(p.sourceRoots[prefix], match)
			if relErr != nil {
				continue
			}
			virtual = append(virtual, filepath.ToSlash(filepath.Join(prefix, rel)))
		}
		return virtual, ""
	}
	if filepath.IsAbs(pattern) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, ""
		}
		return matches, ""
	}
	if p.workingDir == "" && p.baseDir == "" {
		return nil, ""
	}
	relativeBase := p.workingDir
	if relativeBase == "" {
		relativeBase = p.baseDir
	}
	matches, err := filepath.Glob(filepath.Join(relativeBase, pattern))
	if err != nil {
		return nil, ""
	}
	return matches, relativeBase
}

func makeArgsContainReference(args []string) bool {
	for _, arg := range args {
		if containsMakeReference(arg) {
			return true
		}
	}
	return false
}

func mapMakeWords(value string, mapWord func(string) string) string {
	words := strings.Fields(value)
	for i, word := range words {
		words[i] = mapWord(word)
	}
	return strings.Join(words, " ")
}

func mapMakeWordsDropEmpty(value string, mapWord func(string) string) string {
	var out []string
	for _, word := range strings.Fields(value) {
		mapped := mapWord(word)
		if mapped != "" {
			out = append(out, mapped)
		}
	}
	return strings.Join(out, " ")
}

func filterMakeWords(patterns string, words string, invert bool) string {
	patternList := strings.Fields(patterns)
	var out []string
	for _, word := range strings.Fields(words) {
		matched := false
		for _, pattern := range patternList {
			if makePatternMatch(pattern, word) {
				matched = true
				break
			}
		}
		if matched != invert {
			out = append(out, word)
		}
	}
	return strings.Join(out, " ")
}

// filterMakeWordsWithProjections preserves GNU Make's logical comparisons
// when action lowering renders an invocation-local path into a stable tree
// namespace. Parsed simple variables retain their original logical spelling,
// while automatic variables such as $@ must carry the rendered spelling into
// the final action. Both spellings denote the same Make word for matching; the
// selected result remains the original word from the filter text, exactly as
// GNU Make requires.
//
// Try the literal comparison first. Projection is only an equivalence fallback,
// so ordinary source values and patterns keep their native Make semantics.
func (p *kbuildParser) filterMakeWordsWithProjections(patterns, words string, invert bool) string {
	patternList := strings.Fields(patterns)
	var out []string
	for _, word := range strings.Fields(words) {
		matched := false
		for _, pattern := range patternList {
			if makePatternMatch(pattern, word) || makePatternMatch(
				p.projectKbuildRenderedValue(pattern),
				p.projectKbuildRenderedValue(word),
			) {
				matched = true
				break
			}
		}
		if matched != invert {
			out = append(out, word)
		}
	}
	return strings.Join(out, " ")
}

func (p *kbuildParser) projectKbuildRenderedValue(value string) string {
	for _, projection := range p.renderedValueProjections {
		if projection.rendered == "" || !strings.Contains(value, projection.rendered) {
			continue
		}
		value = strings.ReplaceAll(value, projection.rendered, projection.logical)
	}
	return value
}

func makePatternMatch(pattern, word string) bool {
	prefix, suffix, wildcard := splitMakePercent(pattern)
	if !wildcard {
		return prefix == word
	}
	return len(word) >= len(prefix)+len(suffix) && strings.HasPrefix(word, prefix) && strings.HasSuffix(word, suffix)
}

func makePatsubst(pattern, replacement, word string) string {
	prefix, suffix, wildcard := splitMakePercent(pattern)
	replacementPrefix, replacementSuffix, replacementWildcard := splitMakePercent(replacement)
	if !wildcard {
		if prefix != word {
			return word
		}
		if replacementWildcard {
			return replacementPrefix + "%" + replacementSuffix
		}
		return replacementPrefix
	}
	if len(word) < len(prefix)+len(suffix) || !strings.HasPrefix(word, prefix) || !strings.HasSuffix(word, suffix) {
		return word
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(word, prefix), suffix)
	if !replacementWildcard {
		return replacementPrefix
	}
	return replacementPrefix + stem + replacementSuffix
}

// splitMakePercent removes GNU Make's quoting backslashes and splits at the
// first unescaped percent. Later unescaped percents are literal. A run of
// backslashes immediately before percent contributes one literal backslash
// per pair; an odd final backslash quotes the percent itself.
func splitMakePercent(value string) (string, string, bool) {
	var prefix, suffix strings.Builder
	current := &prefix
	wildcard := false
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			if value[index] == '%' && !wildcard {
				wildcard = true
				current = &suffix
			} else {
				current.WriteByte(value[index])
			}
			index++
			continue
		}
		start := index
		for index < len(value) && value[index] == '\\' {
			index++
		}
		count := index - start
		if index >= len(value) || value[index] != '%' {
			current.WriteString(value[start:index])
			continue
		}
		current.WriteString(strings.Repeat("\\", count/2))
		if count%2 != 0 || wildcard {
			current.WriteByte('%')
		} else {
			wildcard = true
			current = &suffix
		}
		index++
	}
	return prefix.String(), suffix.String(), wildcard
}

func makeWord(index string, words string) string {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(index), "%d", &n); err != nil || n < 1 {
		return ""
	}
	fields := strings.Fields(words)
	if n > len(fields) {
		return ""
	}
	return fields[n-1]
}

func makeWordList(start string, end string, words string) string {
	var startIndex, endIndex int
	if _, err := fmt.Sscanf(strings.TrimSpace(start), "%d", &startIndex); err != nil {
		return ""
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(end), "%d", &endIndex); err != nil {
		return ""
	}
	if startIndex < 1 || endIndex < startIndex {
		return ""
	}
	fields := strings.Fields(words)
	if startIndex > len(fields) {
		return ""
	}
	if endIndex > len(fields) {
		endIndex = len(fields)
	}
	return strings.Join(fields[startIndex-1:endIndex], " ")
}

func makeJoin(left, right string) string {
	leftFields := strings.Fields(left)
	rightFields := strings.Fields(right)
	n := len(leftFields)
	if len(rightFields) > n {
		n = len(rightFields)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		value := ""
		if i < len(leftFields) {
			value += leftFields[i]
		}
		if i < len(rightFields) {
			value += rightFields[i]
		}
		out = append(out, value)
	}
	return strings.Join(out, " ")
}

func makeIntcmp(args []string) (string, bool) {
	left, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil {
		return "", false
	}
	right, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
	if err != nil {
		return "", false
	}
	lt := ""
	if len(args) >= 3 {
		lt = args[2]
	}
	eq := ""
	if len(args) >= 4 {
		eq = args[3]
	}
	gt := eq
	if len(args) >= 5 {
		gt = args[4]
	}
	switch {
	case left < right:
		return lt, true
	case left > right:
		return gt, true
	default:
		return eq, true
	}
}

func makeBasename(word string) string {
	ext := filepath.Ext(word)
	if ext == "" {
		return word
	}
	return strings.TrimSuffix(word, ext)
}

func makeDir(word string) string {
	idx := strings.LastIndexByte(word, '/')
	if idx < 0 {
		return "./"
	}
	return word[:idx+1]
}

func makeNotdir(word string) string {
	idx := strings.LastIndexByte(word, '/')
	if idx < 0 {
		return word
	}
	return word[idx+1:]
}

func makeSuffix(word string) string {
	return filepath.Ext(word)
}

func generatedTargetCondition(lhs string) (string, KbuildCondition, bool) {
	for _, name := range []string{"targets", "always", "extra", "hostprogs", "userprogs"} {
		if lhs == name {
			return name, KbuildCondition{Kind: "const", State: "y"}, true
		}
	}
	for _, prefix := range []string{"always-", "extra-", "hostprogs-", "userprogs-", "hostprogs-always-", "userprogs-always-"} {
		if rest, ok := strings.CutPrefix(lhs, prefix); ok {
			cond, ok := parseKbuildCondition(rest)
			if !ok {
				return "", KbuildCondition{}, false
			}
			return strings.TrimSuffix(prefix, "-"), cond, true
		}
	}
	return "", KbuildCondition{}, false
}

func parseKbuildCondition(value string) (KbuildCondition, bool) {
	switch value {
	case "y", "m", "-":
		return KbuildCondition{Kind: "const", State: value}, true
	}
	if sym, ok := unwrapConfigReference(value); ok {
		return KbuildCondition{Kind: "config", Symbol: sym}, true
	}
	return KbuildCondition{}, false
}

func unwrapConfigReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, wrapper := range [][2]string{{"$(", ")"}, {"${", "}"}} {
		if strings.HasPrefix(value, wrapper[0]) && strings.HasSuffix(value, wrapper[1]) {
			inner := strings.TrimSuffix(strings.TrimPrefix(value, wrapper[0]), wrapper[1])
			if strings.HasPrefix(inner, "CONFIG_") && len(inner) > len("CONFIG_") {
				return inner, true
			}
		}
	}
	return "", false
}

func kbuildGeneratedToken(value string) (string, bool) {
	if value == "" || strings.Contains(value, "$") || strings.HasSuffix(value, "/") {
		return "", false
	}
	return filepath.ToSlash(value), true
}

func (c KbuildCondition) isEmpty() bool {
	return c.Kind == "" && c.Symbol == "" && c.State == "" && len(c.Conditions) == 0
}

func combineKbuildConditions(conditions ...KbuildCondition) KbuildCondition {
	out := make([]KbuildCondition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.isEmpty() {
			continue
		}
		if condition.Kind == "const" && condition.State == "y" {
			continue
		}
		if condition.Kind == "all" {
			out = append(out, condition.Conditions...)
			continue
		}
		out = append(out, condition)
	}
	if len(out) == 0 {
		return KbuildCondition{Kind: "const", State: "y"}
	}
	if len(out) == 1 {
		return out[0]
	}
	return KbuildCondition{Kind: "all", Conditions: out}
}

func combineKbuildAny(conditions ...KbuildCondition) KbuildCondition {
	out := make([]KbuildCondition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.isEmpty() {
			continue
		}
		if condition.Kind == "any" {
			out = append(out, condition.Conditions...)
			continue
		}
		out = append(out, condition)
	}
	if len(out) == 0 {
		return KbuildCondition{}
	}
	if len(out) == 1 {
		return out[0]
	}
	return KbuildCondition{Kind: "any", Conditions: out}
}

func invertKbuildCondition(condition KbuildCondition) KbuildCondition {
	switch condition.Kind {
	case "config_eq":
		condition.Kind = "config_ne"
		return condition
	case "config_ne":
		condition.Kind = "config_eq"
		return condition
	case "const":
		if condition.State == "y" {
			return KbuildCondition{Kind: "const", State: "n"}
		}
		return KbuildCondition{Kind: "const", State: "y"}
	default:
		return KbuildCondition{Kind: "not", Conditions: []KbuildCondition{condition}}
	}
}
