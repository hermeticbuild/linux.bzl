package kconfig

// This file parses Linux's bounded Kconfig probe command shapes and describes each
// capability check as a generic ProbeRequest instead of touching a tool or the
// filesystem. The same evaluator is run twice: discovery preserves opaque
// symbolic text, while replay resolves that text from exact ProbeResults.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	linuxProbeSymbolPrefix     = "LINUX_BZL_PROBE_"
	linuxProbeHostDepsSentinel = "__LINUX_BZL_HOST_DEPS__"
	linuxProbeHostDepsRootName = "host_deps"
)

var (
	linuxProbeSymbolRegexp         = regexp.MustCompile(`LINUX_BZL_PROBE_[0-9a-f]{64}`)
	linuxProbeSymbolPattern        linuxProbeSymbolMatcher
	linuxProbePreprocessorSymbol   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	linuxProbeSafePathComponent    = regexp.MustCompile(`^[A-Za-z0-9_+.-]{1,255}$`)
	linuxProbeSafeEnvironmentValue = regexp.MustCompile(`^[A-Za-z0-9_+.,:/=@%-]*$`)
	linuxProbeUnresolvedVariable   = regexp.MustCompile(`\$(?:[A-Za-z_][A-Za-z0-9_]*|\([A-Za-z_][A-Za-z0-9_]*\)|\{[A-Za-z_][A-Za-z0-9_]*\})`)
)

// ProbeResultLookup is the read-only result surface required during replay.
// ProbeResultOracle implements this interface.
type ProbeResultLookup interface {
	Result(ProbeReference) (ProbeResult, error)
}

// LinuxProbeUnsupportedCommandError reports a shell command outside the
// symbolic evaluator's compiler/tool probe grammar. A caller that already has
// a pure, source-defined shell implementation may delegate only this error;
// malformed commands in evaluator-owned probe shapes remain ordinary errors.
type LinuxProbeUnsupportedCommandError struct {
	Architecture string
	Command      string
}

func (e *LinuxProbeUnsupportedCommandError) Error() string {
	return fmt.Sprintf("unsupported symbolic Linux Kconfig probe command for architecture %q: %q", e.Architecture, e.Command)
}

// IsLinuxProbeUnsupportedCommand reports whether err is safe for a caller to
// offer to its existing non-compiler shell callback.
func IsLinuxProbeUnsupportedCommand(err error) bool {
	var unsupported *LinuxProbeUnsupportedCommandError
	return errors.As(err, &unsupported)
}

// LinuxProbeOwnedUnsupportedCommandError reports a command which selected the
// compiler/source-script probe grammar but was not one of its bounded analysis-
// time forms. Kconfig evaluation must continue to reject it rather than falling
// through to another scope or an ambient shell. A selected Kbuild recipe may,
// however, lower the same source-owned command into its hermetic execution DAG.
type LinuxProbeOwnedUnsupportedCommandError struct {
	Architecture string
	Command      string
}

func (e *LinuxProbeOwnedUnsupportedCommandError) Error() string {
	return fmt.Sprintf("unsupported symbolic Linux Kconfig probe command for architecture %q: %q", e.Architecture, e.Command)
}

// IsLinuxProbeDeferredRecipeCommand is deliberately broader than
// IsLinuxProbeUnsupportedCommand. It is used only while expanding a selected
// recipe, where an unsupported shell command becomes an explicit action-plan
// node. Probe/Kconfig callers keep using the narrower predicate so a malformed
// owned compiler command cannot fall through to another evaluator.
func IsLinuxProbeDeferredRecipeCommand(err error) bool {
	if IsLinuxProbeUnsupportedCommand(err) {
		return true
	}
	var owned *LinuxProbeOwnedUnsupportedCommandError
	return errors.As(err, &owned)
}

// LinuxProbeEvaluatorOptions binds command-shape parsing to one configured
// toolset. Tools are exact command tokens used only to validate Kconfig input;
// ProbeRequests name configured action roles and never execute these paths.
type LinuxProbeEvaluatorOptions struct {
	Scope        string
	Architecture string
	// SourceRoot is the selected Linux source directory as seen by the
	// planner. SourceArchitecture is Linux SRCARCH; it may differ from ARCH.
	// ScriptEnvironment contains only explicitly selected, source-visible
	// values inherited by declared Kconfig scripts.
	SourceRoot         string
	SourceArchitecture string
	ScriptEnvironment  map[string]string
	Facts              *LinuxCompilerFacts
	Tools              map[string]string
	Discovery          ProbeDiscovery
	Oracle             ProbeResultLookup
	RustSourceRoot     string
}

// LinuxProbeEvaluator emits the capability-only Kconfig probe plan. Shell and
// ResolveSymbolic are intended to be installed together in Options. During
// discovery Oracle is nil and ResolveSymbolic preserves known atoms. During
// replay Oracle is non-nil and it substitutes their exact measured values.
type LinuxProbeEvaluator struct {
	scope              string
	architecture       string
	sourceRoot         string
	sourceArchitecture string
	scriptEnvironment  map[string]string
	facts              *LinuxCompilerFacts
	tools              map[string]string
	discovery          ProbeDiscovery
	oracle             ProbeResultLookup
	rustSourceRoot     string
	symbols            map[string]linuxProbeSymbol
	symbolRegistry     *linuxProbeSymbolRegistry
	references         []ProbeReference
	seen               map[string]bool
	shellResults       successfulStringMemo
	kbuildShellResults successfulStringMemo
	resolvedValues     successfulStringMemo
	resolvedSymbols    successfulStringMemo
	structuredValues   successfulStringMemo
	structuredSymbols  successfulStringMemo
}

// successfulStringMemo is a process-local, concurrency-safe cache which only
// records completed evaluations. Errors are deliberately never retained, so a
// transient cancellation or malformed partial discovery cannot poison a later
// evaluation. Each owner supplies the complete semantic scope for its keys.
type successfulStringMemo struct {
	mu     sync.RWMutex
	values map[string]string
}

func (m *successfulStringMemo) load(key string) (string, bool) {
	m.mu.RLock()
	value, ok := m.values[key]
	m.mu.RUnlock()
	return value, ok
}

func (m *successfulStringMemo) store(key, value string) {
	m.mu.Lock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[key] = value
	m.mu.Unlock()
}

func (m *successfulStringMemo) clear() {
	m.mu.Lock()
	m.values = nil
	m.mu.Unlock()
}

type linuxProbeSymbol struct {
	kind         string
	reference    ProbeReference
	request      ProbeRequest
	dependencies []ProbeReference
	trueText     string
	falseText    string
	// selectionInputs and selectionValues encode a finite source-owned Make
	// selection over one or more completed boolean probes. Values are indexed
	// by the raw result-bit state of selectionInputs. Unlike nested trueText /
	// falseText atoms, this form can be lowered to exact composite predicates.
	selectionInputs []linuxProbeSelectionInput
	selectionValues []string
	// A transformed text atom is topology-only during discovery. Replay first
	// resolves its source text and then evaluates the same pure Make function.
	textTransform *linuxProbeTextTransform
	// makeText is an opaque, content-addressed whole-Make-text expression. Its
	// exact arguments are the replay AST. protocolValue is separate lowering
	// evidence whose mode says whether it is byte-exact, only argv-word-
	// equivalent, or deliberately unusable outside exact Make replay.
	makeText *linuxProbeMakeText
}

type linuxProbeSelectionInput struct {
	reference    ProbeReference
	request      ProbeRequest
	dependencies []ProbeReference
}

type linuxProbeTextTransform struct {
	sourceToken   string
	function      string
	arguments     []string
	inputArgument int
}

type linuxProbeMakeText struct {
	function           string
	arguments          []string
	protocolValue      string
	protocolTransforms []linuxProbeMakeTextProtocolTransform
	protocolMode       linuxProbeMakeTextProtocolMode
}

// linuxProbeMakeTextProtocolTransform is the planner-side form of a runtime
// pure-Make transform. Arguments has one empty InputArgument slot for the
// preceding value; every other argument may still contain symbolic probe
// values and is lowered into v6 argument fragments with the source value.
type linuxProbeMakeTextProtocolTransform struct {
	function      string
	arguments     []string
	inputArgument int
}

type linuxProbeMakeTextProtocolMode string

const (
	linuxProbeMakeTextProtocolExact          linuxProbeMakeTextProtocolMode = "exact"
	linuxProbeMakeTextProtocolCanonicalWords linuxProbeMakeTextProtocolMode = "canonical-words"
	linuxProbeMakeTextProtocolArgvWords      linuxProbeMakeTextProtocolMode = "argv-words"
	linuxProbeMakeTextProtocolUnusable       linuxProbeMakeTextProtocolMode = "unusable"
)

type linuxProbeTruth struct {
	known          bool
	value          bool
	reference      ProbeReference
	request        ProbeRequest
	dependencies   []ProbeReference
	trueWhenResult bool
}

func NewLinuxProbeEvaluator(opts LinuxProbeEvaluatorOptions) (*LinuxProbeEvaluator, error) {
	if opts.Scope != "target" && opts.Scope != "host" {
		return nil, fmt.Errorf("Linux probe evaluator scope %q is invalid", opts.Scope)
	}
	if opts.Facts == nil {
		return nil, fmt.Errorf("Linux probe evaluator compiler facts are required")
	}
	if opts.Facts.Scope() != opts.Scope {
		return nil, fmt.Errorf("Linux probe evaluator facts have scope %q, want %q", opts.Facts.Scope(), opts.Scope)
	}
	if strings.TrimSpace(opts.Architecture) == "" || strings.ContainsAny(opts.Architecture, "\x00\r\n/\\") {
		return nil, fmt.Errorf("Linux probe evaluator requires a source-derived architecture")
	}
	if strings.ContainsRune(opts.SourceRoot, 0) {
		return nil, fmt.Errorf("Linux probe evaluator has an invalid selected source root")
	}
	sourceArchitecture := strings.TrimSpace(opts.SourceArchitecture)
	if sourceArchitecture == "" {
		sourceArchitecture = opts.Architecture
	}
	if strings.ContainsAny(sourceArchitecture, "\x00\r\n/\\") {
		return nil, fmt.Errorf("Linux probe evaluator has invalid source architecture %q", sourceArchitecture)
	}
	scriptEnvironment := make(map[string]string, len(opts.ScriptEnvironment)+2)
	for name, value := range opts.ScriptEnvironment {
		if !validKbuildCommandEnvironmentName(name) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("Linux probe evaluator has invalid script environment %q", name)
		}
		scriptEnvironment[name] = value
	}
	for name, value := range map[string]string{"ARCH": opts.Architecture, "SRCARCH": sourceArchitecture} {
		if configured, exists := scriptEnvironment[name]; exists && configured != value {
			return nil, fmt.Errorf("Linux probe evaluator script environment %s=%q, want %q", name, configured, value)
		}
		scriptEnvironment[name] = value
	}
	if opts.Discovery == nil {
		return nil, fmt.Errorf("Linux probe evaluator discovery is required")
	}
	tools := make(map[string]string, len(opts.Tools))
	for role, value := range opts.Tools {
		if !validKbuildActionRolePart(role) {
			return nil, fmt.Errorf("Linux probe evaluator has invalid tool role %q", role)
		}
		if value == "" || strings.ContainsAny(value, "\x00\r\n\t ") {
			return nil, fmt.Errorf("Linux probe evaluator %s command token %q is invalid", role, value)
		}
		tools[role] = value
	}
	if tools["cc"] == "" {
		return nil, fmt.Errorf("Linux probe evaluator requires configured cc role")
	}
	rustSourceRoot := cleanOptionalProbeSourceRoot(opts.RustSourceRoot)
	return &LinuxProbeEvaluator{
		scope: opts.Scope, architecture: opts.Architecture,
		sourceRoot: cleanOptionalProbeSourceRoot(opts.SourceRoot), sourceArchitecture: sourceArchitecture,
		scriptEnvironment: scriptEnvironment, facts: opts.Facts,
		tools: tools, discovery: opts.Discovery, oracle: opts.Oracle, rustSourceRoot: rustSourceRoot,
		symbols: map[string]linuxProbeSymbol{}, symbolRegistry: newLinuxProbeSymbolRegistry(), seen: map[string]bool{},
	}, nil
}

// References returns every distinct capability node in first-encounter order.
// They can be passed as ProbePlanBuilder.Plan terminals for the Kconfig phase.
func (e *LinuxProbeEvaluator) References() []ProbeReference {
	if e == nil {
		return nil
	}
	return slices.Clone(e.references)
}

// WithScriptEnvironment returns an evaluator for a newly source-exported
// process environment. Capability symbols and their first-encounter reference
// order remain part of the same discovery DAG, while every environment-sensitive
// shell and resolution memo starts empty.
func (e *LinuxProbeEvaluator) WithScriptEnvironment(environment map[string]string) (*LinuxProbeEvaluator, error) {
	if e == nil {
		return nil, fmt.Errorf("Linux probe evaluator is nil")
	}
	refreshed, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: e.scope, Architecture: e.architecture,
		SourceRoot: e.sourceRoot, SourceArchitecture: e.sourceArchitecture,
		ScriptEnvironment: environment,
		Facts:             e.facts, Tools: e.tools,
		Discovery: e.discovery, Oracle: e.oracle, RustSourceRoot: e.rustSourceRoot,
	})
	if err != nil {
		return nil, err
	}
	// Every published symbol already lives in the workload-wide registry.
	// Keep only an evaluator-local adoption cache here: copying the complete
	// symbol graph on every source-ordered environment switch made repeated
	// Kbuild invocations quadratic in both allocation volume and GC work.
	// ResolveSymbolic and every symbolic transform adopt registry entries on
	// demand, while publishSymbol still checks the shared registry for identity
	// collisions before populating this fresh local cache.
	refreshed.symbolRegistry = e.symbolRegistry
	refreshed.references = slices.Clone(e.references)
	refreshed.seen = maps.Clone(e.seen)
	return refreshed, nil
}

// Shell parses one Linux Kconfig $(shell,...) command. It never invokes a
// process. Capability results remain symbolic in both discovery and replay so
// their provenance is available if a later request consumes them.
func (e *LinuxProbeEvaluator) Shell(ctx context.Context, command string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("Linux probe evaluator is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if value, ok := e.shellResults.load(command); ok {
		return value, nil
	}
	var value string
	var err error
	if match := ifSuccessPattern.FindStringSubmatch(command); match != nil {
		var truth linuxProbeTruth
		truth, err = e.commandSucceeds(strings.TrimSpace(match[1]))
		if err != nil {
			return "", err
		}
		value, err = e.renderTruth(truth, match[2], match[3])
	} else {
		value, err = e.output(command)
	}
	if err != nil {
		return "", err
	}
	e.shellResults.store(command, value)
	return value, nil
}

func (e *LinuxProbeEvaluator) canonicalSourceCommand(command string) string {
	if e == nil || e.sourceRoot == "" {
		return command
	}
	roots := []string{e.sourceRoot}
	if absolute, err := filepath.Abs(e.sourceRoot); err == nil {
		roots = append(roots, absolute)
		if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
			roots = append(roots, resolved)
		}
	}
	seen := map[string]bool{}
	for len(roots) != 0 {
		// Replace longest spellings first in case one checkout path is nested
		// beneath another. Only complete rooted prefixes are canonicalized.
		longest := 0
		for index := 1; index < len(roots); index++ {
			if len(roots[index]) > len(roots[longest]) {
				longest = index
			}
		}
		root := filepath.ToSlash(filepath.Clean(roots[longest]))
		roots = append(roots[:longest], roots[longest+1:]...)
		if root == "" || root == "." || root == "/" || seen[root] {
			continue
		}
		seen[root] = true
		command = strings.ReplaceAll(command, root+"/", "__LINUX_BZL_SOURCE_TREE__/")
	}
	return command
}

func (e *LinuxProbeEvaluator) canonicalSourceRequest(request ProbeRequest) ProbeRequest {
	request.Scratch = slices.Clone(request.Scratch)
	for index := range request.Scratch {
		request.Scratch[index].Content = e.canonicalSourceCommand(request.Scratch[index].Content)
	}
	request.Steps = slices.Clone(request.Steps)
	for index := range request.Steps {
		step := request.Steps[index]
		step.WorkingDirectory = e.canonicalSourceCommand(step.WorkingDirectory)
		step.Arguments = slices.Clone(step.Arguments)
		for argument := range step.Arguments {
			step.Arguments[argument] = e.canonicalSourceCommand(step.Arguments[argument])
		}
		step.ConditionalArguments = slices.Clone(step.ConditionalArguments)
		for conditional := range step.ConditionalArguments {
			step.ConditionalArguments[conditional].Arguments = slices.Clone(step.ConditionalArguments[conditional].Arguments)
			for argument := range step.ConditionalArguments[conditional].Arguments {
				step.ConditionalArguments[conditional].Arguments[argument] = e.canonicalSourceCommand(step.ConditionalArguments[conditional].Arguments[argument])
			}
		}
		step.ArgumentFragments = slices.Clone(step.ArgumentFragments)
		for groupIndex := range step.ArgumentFragments {
			group := step.ArgumentFragments[groupIndex]
			group.Fragments = e.canonicalSourceFragments(group.Fragments)
			step.ArgumentFragments[groupIndex] = group
		}
		step.Environment = maps.Clone(step.Environment)
		for name, value := range step.Environment {
			step.Environment[name] = e.canonicalSourceCommand(value)
		}
		step.EnvironmentFragments = slices.Clone(step.EnvironmentFragments)
		for environmentIndex := range step.EnvironmentFragments {
			entry := step.EnvironmentFragments[environmentIndex]
			entry.Fragments = e.canonicalSourceFragments(entry.Fragments)
			step.EnvironmentFragments[environmentIndex] = entry
		}
		step.Stdin = e.canonicalSourceCommand(step.Stdin)
		step.StdinFragments = e.canonicalSourceFragments(step.StdinFragments)
		request.Steps[index] = step
	}
	request.Outcome.Fragments = e.canonicalSourceFragments(request.Outcome.Fragments)
	return request
}

func (e *LinuxProbeEvaluator) canonicalSourceFragments(fragments []ProbeValueFragment) []ProbeValueFragment {
	fragments = slices.Clone(fragments)
	for fragmentIndex := range fragments {
		fragment := fragments[fragmentIndex]
		fragment.Value = e.canonicalSourceCommand(fragment.Value)
		fragment.When = e.canonicalSourcePredicate(fragment.When)
		fragment.Fragments = e.canonicalSourceFragments(fragment.Fragments)
		fragment.Transforms = slices.Clone(fragment.Transforms)
		for transformIndex := range fragment.Transforms {
			transform := fragment.Transforms[transformIndex]
			transform.Arguments = slices.Clone(transform.Arguments)
			for argumentIndex := range transform.Arguments {
				transform.Arguments[argumentIndex] = e.canonicalSourceCommand(transform.Arguments[argumentIndex])
			}
			transform.ArgumentFragments = slices.Clone(transform.ArgumentFragments)
			for groupIndex := range transform.ArgumentFragments {
				group := transform.ArgumentFragments[groupIndex]
				group.Fragments = e.canonicalSourceFragments(group.Fragments)
				transform.ArgumentFragments[groupIndex] = group
			}
			fragment.Transforms[transformIndex] = transform
		}
		fragments[fragmentIndex] = fragment
	}
	return fragments
}

func (e *LinuxProbeEvaluator) canonicalSourcePredicate(predicate *ProbePredicate) *ProbePredicate {
	if predicate == nil {
		return nil
	}
	clone := *predicate
	clone.Value = e.canonicalSourceCommand(clone.Value)
	clone.Operands = slices.Clone(clone.Operands)
	for index := range clone.Operands {
		operand := e.canonicalSourcePredicate(&clone.Operands[index])
		clone.Operands[index] = *operand
	}
	return &clone
}

// ResolveSymbolic substitutes every known probe atom from exact replay
// results. With no Oracle it validates and preserves the atoms for discovery.
func (e *LinuxProbeEvaluator) ResolveSymbolic(value string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("Linux probe evaluator is nil")
	}
	return e.resolveSymbolicWithState(value, &linuxProbeResolveState{visiting: map[string]bool{}})
}

// ResolveSymbolicStructure selects one source-visible layer of finite
// boolean/selection branches from replay results. Every atom nested inside a
// selected branch remains symbolic, allowing callers to observe its shell
// structure without erasing the dependency DAG which a recursive Kbuild
// invocation must reconstruct. Discovery validates and preserves every atom
// unchanged.
func (e *LinuxProbeEvaluator) ResolveSymbolicStructure(value string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("Linux probe evaluator is nil")
	}
	return e.resolveSymbolicStructureWithState(value, &linuxProbeResolveState{visiting: map[string]bool{}})
}

const (
	maxLinuxProbeResolveNodes = 4096
	maxLinuxProbeResolveDepth = 32
)

type linuxProbeResolveState struct {
	visiting map[string]bool
	nodes    int
	depth    int
}

func (e *LinuxProbeEvaluator) resolveSymbolicWithState(value string, state *linuxProbeResolveState) (string, error) {
	original := value
	if len(value) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("Linux probe symbolic value exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	if resolved, ok := e.resolvedValues.load(original); ok {
		return resolved, nil
	}
	for token := range linuxProbeSymbolPattern.AllString(value) {
		if _, ok, err := e.adoptSymbol(token); err != nil {
			return "", err
		} else if !ok {
			return "", fmt.Errorf("unknown Linux probe symbolic value %q", token)
		}
	}
	if e.oracle == nil {
		e.resolvedValues.store(original, value)
		return value, nil
	}
	for depth := 0; depth < maxLinuxProbeResolveDepth && linuxProbeSymbolPattern.MatchString(value); depth++ {
		before := value
		var err error
		value, err = e.resolveSymbolicPass(value, state)
		if err != nil {
			return "", err
		}
		if value == before {
			return "", fmt.Errorf("cyclic Linux probe symbolic value %q", value)
		}
	}
	if linuxProbeSymbolPattern.MatchString(value) {
		return "", fmt.Errorf("Linux probe symbolic expansion is too deep")
	}
	e.resolvedValues.store(original, value)
	return value, nil
}

func (e *LinuxProbeEvaluator) resolveSymbolicPass(value string, state *linuxProbeResolveState) (string, error) {
	if !linuxProbeSymbolPattern.MatchString(value) {
		return value, nil
	}
	var resolved strings.Builder
	appendValue := func(fragment string) error {
		if len(fragment) > MaxProbeInterpolatedBytes-resolved.Len() {
			return fmt.Errorf("Linux probe symbolic expansion exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
		resolved.WriteString(fragment)
		return nil
	}
	last := 0
	for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
		if err := appendValue(value[last:start]); err != nil {
			return "", err
		}
		token := value[start:end]
		result, err := e.resolveSymbolWithState(token, state)
		if err != nil {
			return "", err
		}
		if err := appendValue(result); err != nil {
			return "", err
		}
		last = end
	}
	if err := appendValue(value[last:]); err != nil {
		return "", err
	}
	return resolved.String(), nil
}

func (e *LinuxProbeEvaluator) resolveSymbolicStructureWithState(value string, state *linuxProbeResolveState) (string, error) {
	original := value
	if len(value) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("Linux probe symbolic structure exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	if resolved, ok := e.structuredValues.load(original); ok {
		return resolved, nil
	}
	var resolved strings.Builder
	appendValue := func(fragment string) error {
		if len(fragment) > MaxProbeInterpolatedBytes-resolved.Len() {
			return fmt.Errorf("Linux probe symbolic structure exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
		resolved.WriteString(fragment)
		return nil
	}
	last := 0
	for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
		if err := appendValue(value[last:start]); err != nil {
			return "", err
		}
		token := value[start:end]
		selected, err := e.resolveSymbolicStructureToken(token, state)
		if err != nil {
			return "", err
		}
		if err := appendValue(selected); err != nil {
			return "", err
		}
		last = end
	}
	if err := appendValue(value[last:]); err != nil {
		return "", err
	}
	value = resolved.String()
	e.structuredValues.store(original, value)
	return value, nil
}

func (e *LinuxProbeEvaluator) resolveSymbolicStructureToken(token string, state *linuxProbeResolveState) (string, error) {
	if value, ok := e.structuredSymbols.load(token); ok {
		return value, nil
	}
	symbol, ok, err := e.adoptSymbol(token)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown Linux probe symbolic value %q", token)
	}
	if e.oracle == nil || (symbol.kind != "boolean" && symbol.kind != "selection") {
		e.structuredSymbols.store(token, token)
		return token, nil
	}
	if state.visiting[token] {
		return "", fmt.Errorf("cyclic Linux probe symbolic value %q during structural replay", token)
	}
	if state.nodes >= maxLinuxProbeResolveNodes {
		return "", fmt.Errorf("Linux probe symbolic structural replay exceeds %d nodes", maxLinuxProbeResolveNodes)
	}
	if state.depth >= maxLinuxProbeResolveDepth {
		return "", fmt.Errorf("Linux probe symbolic structural replay exceeds depth %d", maxLinuxProbeResolveDepth)
	}
	state.nodes++
	state.depth++
	defer func() { state.depth-- }()
	state.visiting[token] = true
	defer delete(state.visiting, token)

	selected := ""
	switch symbol.kind {
	case "boolean":
		result, err := e.readBoolean(symbol.reference, symbol.request, symbol.dependencies...)
		if err != nil {
			return "", err
		}
		selected = symbol.falseText
		if result {
			selected = symbol.trueText
		}
	case "selection":
		selectionState := 0
		for index, input := range symbol.selectionInputs {
			result, err := e.readBoolean(input.reference, input.request, input.dependencies...)
			if err != nil {
				return "", err
			}
			if result {
				selectionState |= 1 << index
			}
		}
		if selectionState >= len(symbol.selectionValues) {
			return "", fmt.Errorf("Linux probe symbolic selection %q has invalid state %d", token, selectionState)
		}
		selected = symbol.selectionValues[selectionState]
	}
	e.structuredSymbols.store(token, selected)
	return selected, nil
}

func (e *LinuxProbeEvaluator) resolveSymbolWithState(token string, state *linuxProbeResolveState) (string, error) {
	if value, ok := e.resolvedSymbols.load(token); ok {
		return value, nil
	}
	symbol, ok, err := e.adoptSymbol(token)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown Linux probe symbolic value %q", token)
	}
	if e.oracle == nil {
		return token, nil
	}
	if state.visiting[token] {
		return "", fmt.Errorf("cyclic Linux probe symbolic value %q during replay", token)
	}
	if state.nodes >= maxLinuxProbeResolveNodes {
		return "", fmt.Errorf("Linux probe symbolic replay exceeds %d nodes", maxLinuxProbeResolveNodes)
	}
	if state.depth >= maxLinuxProbeResolveDepth {
		return "", fmt.Errorf("Linux probe symbolic replay exceeds depth %d", maxLinuxProbeResolveDepth)
	}
	state.nodes++
	state.depth++
	defer func() { state.depth-- }()
	state.visiting[token] = true
	defer delete(state.visiting, token)
	var value string
	switch symbol.kind {
	case "text":
		result, err := e.readText(symbol.reference, symbol.request, symbol.dependencies...)
		if err != nil {
			return "", err
		}
		value = result
	case "boolean":
		result, err := e.readBoolean(symbol.reference, symbol.request, symbol.dependencies...)
		if err != nil {
			return "", err
		}
		value = symbol.falseText
		if result {
			value = symbol.trueText
		}
	case "selection":
		state := 0
		for index, input := range symbol.selectionInputs {
			result, err := e.readBoolean(input.reference, input.request, input.dependencies...)
			if err != nil {
				return "", err
			}
			if result {
				state |= 1 << index
			}
		}
		if state >= len(symbol.selectionValues) {
			return "", fmt.Errorf("Linux probe symbolic selection %q has invalid state %d", token, state)
		}
		value = symbol.selectionValues[state]
	case "transformed-text":
		if symbol.textTransform == nil {
			return "", fmt.Errorf("Linux transformed text symbolic value %q has no transform", token)
		}
		input, err := e.resolveSymbolWithState(symbol.textTransform.sourceToken, state)
		if err != nil {
			return "", err
		}
		arguments := slices.Clone(symbol.textTransform.arguments)
		arguments[symbol.textTransform.inputArgument] = input
		transformed, transformErr := applyBoundedPureKbuildTextTransform(symbol.textTransform.function, arguments)
		if transformErr != nil {
			return "", transformErr
		}
		value = transformed
	case "make-text":
		if symbol.makeText == nil {
			return "", fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
		}
		arguments := slices.Clone(symbol.makeText.arguments)
		for index, argument := range arguments {
			resolved, err := e.resolveSymbolicWithState(argument, state)
			if err != nil {
				return "", fmt.Errorf("resolve Linux whole Make text %q argument %d: %w", token, index, err)
			}
			arguments[index] = resolved
		}
		transformed, transformErr := applyBoundedPureKbuildTextTransform(symbol.makeText.function, arguments)
		if transformErr != nil {
			return "", fmt.Errorf("evaluate Linux whole Make text %q: %w", token, transformErr)
		}
		value = transformed
	default:
		return "", fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", token, symbol.kind)
	}
	if len(value) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("Linux probe symbolic value %q exceeds %d bytes", token, MaxProbeInterpolatedBytes)
	}
	e.resolvedSymbols.store(token, value)
	return value, nil
}

func (e *LinuxProbeEvaluator) output(command string) (string, error) {
	if value, recognized, err := EvaluateLinuxCompilerMachineShell(
		command,
		e.facts.Machine(),
		e.compilerMachineTokens(),
	); recognized || err != nil {
		return value, err
	}
	if value, recognized, err := e.linuxGetconfOutput(command); recognized || err != nil {
		return value, err
	}
	if value, recognized, err := e.configuredPkgConfigQuery(command); recognized || err != nil {
		return value, err
	}
	if value, recognized, err := e.selectedToolSourceQuery(command); recognized || err != nil {
		return value, err
	}
	if request, dependencies, recognized, err := e.sourceScriptRequest(command, "text"); recognized || err != nil {
		if err != nil {
			return "", err
		}
		return e.requestText(request, dependencies...)
	}
	if value, recognized, err := e.commandLookup(command); recognized || err != nil {
		return value, err
	}
	if firstLine, localeC, recognized := e.compilerVersionCommand(command); recognized {
		if firstLine && localeC {
			return e.facts.VersionText(), nil
		}
		environment := map[string]string(nil)
		if localeC {
			environment = map[string]string{"LC_ALL": "C"}
		}
		return e.requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema,
			Steps: []ProbeStep{{
				Name: "version", Tool: "cc", Arguments: []string{"--version"}, Environment: environment,
			}},
			Outcome: ProbeOutcome{Kind: "text", Step: "version", Stream: "stdout", TrimSpace: true, FirstLine: firstLine},
		})
	}
	if name, recognized, err := e.compilerPrintFileCommand(command); recognized || err != nil {
		if err != nil {
			return "", err
		}
		return e.compilerPrintFileName(name)
	}
	if value, recognized, err := e.directToolVersionOutput(command); recognized || err != nil {
		return value, err
	}
	if value, recognized, err := e.rustcPrintFileNames(command); recognized || err != nil {
		return value, err
	}
	if value, recognized, err := e.compilerMacroGrepStatus(command); recognized || err != nil {
		return value, err
	}
	switch {
	case strings.HasPrefix(command, "echo ") && strings.Contains(command, "|"):
		if value, recognized, err := e.compilerPreprocessorTailOutput(command); recognized || err != nil {
			return value, err
		}
		if value, recognized, err := e.compilerPreprocessorGrep(command); recognized || err != nil {
			return value, err
		}
		if value, recognized, err := e.compilerPreprocessorOutput(command); recognized || err != nil {
			return value, err
		}
	case strings.HasPrefix(command, "set -- "):
		return e.shellSetEcho(command)
	case strings.HasPrefix(command, "expr "):
		if value, recognized, err := e.dynamicShellExpr(command); recognized || err != nil {
			return value, err
		}
		return shellExpr(command)
	default:
		return "", e.unhandledCommand(command)
	}
	return "", e.unhandledCommand(command)
}

// compilerMacroGrepStatus lowers the source-owned compiler-predefine query
// used by tools Makefiles. The shell pipeline reports grep's status, not the
// compiler's, so the request reduces the selected compiler's stdout directly
// and renders that boolean as the shell status text expected by Make.
//
// The macro name and complete compiler argv remain source data. This parser
// knows only the bounded preprocessor protocol and the fixed, quiet grep
// reduction; it carries no compiler-family or capability table.
func (e *LinuxProbeEvaluator) compilerMacroGrepStatus(command string) (string, bool, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		fields := strings.Fields(command)
		if len(fields) != 0 && e.isToolToken(fields[0], "cc") {
			return "", true, e.unsupportedCommand(command)
		}
		return "", false, nil
	}
	if len(tokens) == 0 || tokens[0].operator || !e.isToolToken(tokens[0].value, "cc") {
		return "", false, nil
	}
	// Once the command selects this evaluator's compiler, malformed variants of
	// the owned shape must not fall through to an arbitrary shell callback.
	if len(command) > 4096 || len(tokens) > 128 {
		return "", true, e.unsupportedCommand(command)
	}

	pipe, semicolon := -1, -1
	for index, token := range tokens {
		if !token.operator {
			continue
		}
		switch {
		case token.value == "|" && pipe < 0 && semicolon < 0:
			pipe = index
		case token.value == ";" && pipe >= 0 && semicolon < 0:
			semicolon = index
		default:
			return "", true, e.unsupportedCommand(command)
		}
	}
	if pipe < 2 || semicolon != pipe+4 || len(tokens) != pipe+7 ||
		tokens[pipe+1].operator || tokens[pipe+1].value != "grep" ||
		tokens[pipe+2].operator || !parseLinuxProbeFixedQuietGrepOptions(tokens[pipe+2].value) ||
		tokens[pipe+3].operator || tokens[semicolon+1].operator || tokens[semicolon+1].value != "echo" ||
		tokens[semicolon+2].operator || tokens[semicolon+2].value != "$?" {
		return "", true, e.unsupportedCommand(command)
	}
	literal, ok := parseLinuxProbeGrepLiteral(tokens[pipe+3].value)
	if !ok {
		return "", true, e.unsupportedCommand(command)
	}

	arguments := make([]string, 0, pipe-1)
	candidateArguments := make([]bool, 0, pipe-1)
	dumpMacros, preprocess, languageC, nullInput := 0, 0, 0, 0
	for index := 1; index < pipe; index++ {
		if tokens[index].operator {
			return "", true, e.unsupportedCommand(command)
		}
		argument := tokens[index].value
		arguments = append(arguments, argument)
		candidateArguments = append(candidateArguments, false)
		switch argument {
		case "-dM":
			dumpMacros++
		case "-E":
			preprocess++
		case "-x":
			if index+1 >= pipe || tokens[index+1].operator || tokens[index+1].value != "c" {
				return "", true, e.unsupportedCommand(command)
			}
			arguments = append(arguments, tokens[index+1].value)
			candidateArguments = append(candidateArguments, false)
			languageC++
			index++
		case "/dev/null":
			nullInput++
		default:
			candidateArguments[len(candidateArguments)-1] = true
		}
	}
	if dumpMacros != 1 || preprocess != 1 || languageC != 1 || nullInput != 1 {
		return "", true, e.unsupportedCommand(command)
	}
	// The compiler bootstrap executes this exact source-owned query under the
	// same configured action contract. Reuse its byte-for-byte stdout instead
	// of creating a second node: in addition to avoiding redundant work, this
	// lets target compiler identity select flags used by an immediately
	// following host probe without introducing a target-to-host DAG edge.
	// Any source-selected flag, reordering, or alternate input remains a normal
	// probe request below.
	if slices.Equal(arguments, []string{"-dM", "-E", "-x", "c", "/dev/null"}) {
		if strings.Contains(e.facts.Predefines(), literal) {
			return "0", true, nil
		}
		return "1", true, nil
	}
	arguments, conditional, argumentFragments, candidate, dependencies, err := e.lowerSymbolicCandidateArguments(
		arguments, candidateArguments, ProbeCandidatePolicyCC,
	)
	if err != nil {
		return "", true, err
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "macro-preprocess", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidate,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "stream-contains", Step: "macro-preprocess", Stream: "stdout", Value: literal,
		}},
	}
	truth, err := e.requestTruth(request, dependencies...)
	if err != nil {
		return "", true, err
	}
	value, err := e.renderTruth(truth, "0", "1")
	return value, true, err
}

func parseLinuxProbeFixedQuietGrepOptions(value string) bool {
	if len(value) != 3 || value[0] != '-' {
		return false
	}
	return value[1:] == "Fq" || value[1:] == "qF"
}

func (e *LinuxProbeEvaluator) compilerMachineTokens() []string {
	if e == nil {
		return nil
	}
	candidates := []string{
		e.tools["cc"],
		e.scriptEnvironment["CC"],
		KbuildActionRoleToken(e.scope, "cc"),
		KbuildActionRoleToken(KbuildActionRoleAutoScope, "cc"),
	}
	seen := map[string]bool{}
	tokens := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		tokens = append(tokens, candidate)
	}
	return tokens
}

func (e *LinuxProbeEvaluator) commandSucceeds(command string) (linuxProbeTruth, error) {
	if request, dependencies, recognized, err := e.sourceScriptRequest(command, "boolean"); recognized || err != nil {
		if err != nil {
			return linuxProbeTruth{}, err
		}
		return e.requestTruth(request, dependencies...)
	}
	if truth, recognized, err := e.pythonLXMLProbe(command); recognized || err != nil {
		return truth, err
	}
	switch {
	case strings.HasPrefix(command, "command -v "):
		value, err := e.commandExists(command)
		return knownLinuxProbeTruth(value), err
	case strings.HasPrefix(command, "test "):
		return e.shellTest(strings.TrimSpace(strings.TrimPrefix(command, "test ")))
	}
	if truth, recognized, err := e.rustCompilerOptionProbe(command); recognized || err != nil {
		return truth, err
	}
	if truth, recognized, err := e.auxiliaryToolGrepProbe(command); recognized || err != nil {
		return truth, err
	}
	if truth, recognized, err := e.compilerSourceProbe(command); recognized || err != nil {
		return truth, err
	}
	if truth, recognized, err := e.assemblerSourceProbe(command); recognized || err != nil {
		return truth, err
	}
	if truth, recognized, err := e.linkerOptionProbe(command); recognized || err != nil {
		return truth, err
	}
	if truth, recognized, err := e.compilerOptionProbe(command); recognized || err != nil {
		return truth, err
	}
	return linuxProbeTruth{}, e.unhandledCommand(command)
}

func knownLinuxProbeTruth(value bool) linuxProbeTruth {
	return linuxProbeTruth{known: true, value: value}
}

func (e *LinuxProbeEvaluator) renderTruth(truth linuxProbeTruth, trueText, falseText string) (string, error) {
	if truth.known {
		if truth.value {
			return trueText, nil
		}
		return falseText, nil
	}
	if !truth.trueWhenResult {
		trueText, falseText = falseText, trueText
	}
	hash := sha256.New()
	for _, field := range []string{truth.reference.NodeID, truth.reference.RequestID, trueText, falseText} {
		fmt.Fprintf(hash, "%d:%s", len(field), field)
	}
	token := linuxProbeSymbolPrefix + hex.EncodeToString(hash.Sum(nil))
	symbol := linuxProbeSymbol{
		kind:      "boolean",
		reference: truth.reference, request: truth.request,
		dependencies: slices.Clone(truth.dependencies),
		trueText:     trueText, falseText: falseText,
	}
	if existing, ok := e.symbols[token]; ok {
		if existing.kind != symbol.kind || existing.reference != symbol.reference || !slices.Equal(existing.dependencies, symbol.dependencies) || existing.trueText != symbol.trueText || existing.falseText != symbol.falseText {
			return "", fmt.Errorf("Linux probe symbolic value collision %q", token)
		}
	}
	if err := e.publishSymbol(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

func (e *LinuxProbeEvaluator) renderSelection(inputs []linuxProbeSelectionInput, values []string) (string, error) {
	if len(inputs) == 0 || len(inputs) > maxKbuildSymbolicComparisonReferences || len(values) != 1<<len(inputs) {
		return "", fmt.Errorf("invalid Linux probe symbolic selection with %d inputs and %d values", len(inputs), len(values))
	}
	invariant := true
	for _, value := range values[1:] {
		if value != values[0] {
			invariant = false
			break
		}
	}
	if invariant {
		return values[0], nil
	}
	hash := sha256.New()
	for _, field := range []string{"selection-v1", e.scope} {
		fmt.Fprintf(hash, "%d:%s", len(field), field)
	}
	for _, input := range inputs {
		for _, field := range []string{input.reference.NodeID, input.reference.RequestID, input.reference.Scope, input.reference.Kind} {
			fmt.Fprintf(hash, "%d:%s", len(field), field)
		}
	}
	for _, value := range values {
		fmt.Fprintf(hash, "%d:%s", len(value), value)
	}
	token := linuxProbeSymbolPrefix + hex.EncodeToString(hash.Sum(nil))
	symbol := linuxProbeSymbol{
		kind: "selection", selectionInputs: slices.Clone(inputs), selectionValues: slices.Clone(values),
	}
	for index := range symbol.selectionInputs {
		symbol.selectionInputs[index].dependencies = slices.Clone(symbol.selectionInputs[index].dependencies)
	}
	if existing, ok := e.symbols[token]; ok {
		if existing.kind != symbol.kind || !slices.Equal(existing.selectionValues, symbol.selectionValues) || !equalLinuxProbeSelectionInputs(existing.selectionInputs, symbol.selectionInputs) {
			return "", fmt.Errorf("Linux probe symbolic value collision %q", token)
		}
	}
	if err := e.publishSymbol(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

func equalLinuxProbeSelectionInputs(left, right []linuxProbeSelectionInput) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].reference != right[index].reference || left[index].request.Schema != right[index].request.Schema ||
			!slices.Equal(left[index].dependencies, right[index].dependencies) {
			return false
		}
		leftID, leftErr := left[index].request.ID()
		rightID, rightErr := right[index].request.ID()
		if leftErr != nil || rightErr != nil || leftID != rightID {
			return false
		}
	}
	return true
}

func (e *LinuxProbeEvaluator) renderTextTransform(sourceToken, function string, arguments []string, inputArgument int) (string, error) {
	if inputArgument < 0 || inputArgument >= len(arguments) || arguments[inputArgument] != sourceToken {
		return "", fmt.Errorf("invalid Linux symbolic Make transform input")
	}
	source, ok, err := e.adoptSymbol(sourceToken)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("unknown Linux probe symbolic value %q", sourceToken)
	}
	if source.kind != "text" && source.kind != "transformed-text" {
		return "", fmt.Errorf("Make function %q cannot transform Linux %s probe value", function, source.kind)
	}
	probeArguments := slices.Clone(arguments)
	probeArguments[inputArgument] = "${text-input}"
	if _, recognized, err := evalPureKbuildMakeFunction(function, probeArguments, ""); err != nil {
		return "", err
	} else if !recognized {
		return "", fmt.Errorf("Make function %q has no hermetic symbolic text transform", function)
	}
	hash := sha256.New()
	for _, field := range append([]string{"make-text-transform-v1", sourceToken, function, strconv.Itoa(inputArgument)}, probeArguments...) {
		fmt.Fprintf(hash, "%d:%s", len(field), field)
	}
	token := linuxProbeSymbolPrefix + hex.EncodeToString(hash.Sum(nil))
	transform := &linuxProbeTextTransform{
		sourceToken: sourceToken, function: function, arguments: slices.Clone(arguments), inputArgument: inputArgument,
	}
	symbol := linuxProbeSymbol{kind: "transformed-text", textTransform: transform}
	if existing, ok := e.symbols[token]; ok {
		if existing.kind != symbol.kind || existing.textTransform == nil || existing.textTransform.sourceToken != transform.sourceToken ||
			existing.textTransform.function != transform.function || existing.textTransform.inputArgument != transform.inputArgument ||
			!slices.Equal(existing.textTransform.arguments, transform.arguments) {
			return "", fmt.Errorf("Linux probe symbolic value collision %q", token)
		}
	}
	if err := e.publishSymbol(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

func (e *LinuxProbeEvaluator) renderMakeText(function string, arguments []string, protocolValue string, protocolMode linuxProbeMakeTextProtocolMode) (string, error) {
	return e.renderMakeTextWithProtocolTransforms(function, arguments, protocolValue, nil, protocolMode)
}

func (e *LinuxProbeEvaluator) renderMakeTextWithProtocolTransforms(
	function string,
	arguments []string,
	protocolValue string,
	protocolTransforms []linuxProbeMakeTextProtocolTransform,
	protocolMode linuxProbeMakeTextProtocolMode,
) (string, error) {
	contextualReplay := contextualKbuildReplayFunction(function, len(arguments))
	if !pureKbuildMakeFunctionArity(function, len(arguments)) && !contextualReplay {
		return "", fmt.Errorf("Make function %q has no hermetic whole-text transform with %d arguments", function, len(arguments))
	}
	if contextualReplay && (protocolValue != "" || len(protocolTransforms) != 0 || protocolMode != linuxProbeMakeTextProtocolUnusable) {
		return "", fmt.Errorf("Make function %q may only be retained as an opaque parser-context replay expression", function)
	}
	if protocolMode != linuxProbeMakeTextProtocolExact && protocolMode != linuxProbeMakeTextProtocolCanonicalWords &&
		protocolMode != linuxProbeMakeTextProtocolArgvWords && protocolMode != linuxProbeMakeTextProtocolUnusable {
		return "", fmt.Errorf("Linux whole Make text has invalid protocol mode %q", protocolMode)
	}
	totalBytes := len(function) + len(protocolValue)
	for _, argument := range arguments {
		if totalBytes > MaxProbeInterpolatedBytes-len(argument) {
			return "", fmt.Errorf("Linux whole Make text exceeds %d provenance bytes", MaxProbeInterpolatedBytes)
		}
		totalBytes += len(argument)
	}
	for transformIndex, transform := range protocolTransforms {
		if !pureKbuildMakeFunctionArity(transform.function, len(transform.arguments)) ||
			!supportedProbeValueTransformFunction(transform.function) {
			return "", fmt.Errorf("Linux whole Make text protocol transform %d has unsupported function %q with %d arguments", transformIndex, transform.function, len(transform.arguments))
		}
		if transform.inputArgument < 0 || transform.inputArgument >= len(transform.arguments) || transform.arguments[transform.inputArgument] != "" {
			return "", fmt.Errorf("Linux whole Make text protocol transform %d has invalid input argument %d", transformIndex, transform.inputArgument)
		}
		if totalBytes > MaxProbeInterpolatedBytes-len(transform.function) {
			return "", fmt.Errorf("Linux whole Make text exceeds %d provenance bytes", MaxProbeInterpolatedBytes)
		}
		totalBytes += len(transform.function)
		for _, argument := range transform.arguments {
			if totalBytes > MaxProbeInterpolatedBytes-len(argument) {
				return "", fmt.Errorf("Linux whole Make text exceeds %d provenance bytes", MaxProbeInterpolatedBytes)
			}
			totalBytes += len(argument)
		}
	}
	if totalBytes > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("Linux whole Make text exceeds %d provenance bytes", MaxProbeInterpolatedBytes)
	}
	values := append(append([]string(nil), arguments...), protocolValue)
	for _, transform := range protocolTransforms {
		values = append(values, transform.arguments...)
	}
	for _, value := range values {
		for nested := range linuxProbeSymbolPattern.AllString(value) {
			if _, exists, err := e.adoptSymbol(nested); err != nil {
				return "", err
			} else if !exists {
				return "", fmt.Errorf("Linux whole Make text references unknown nested value %q", nested)
			}
		}
	}
	hash := sha256.New()
	version := "make-text-v1"
	if contextualReplay {
		version = "make-text-context-v1"
	} else if len(protocolTransforms) != 0 {
		version = "make-text-v2"
	}
	fields := []string{version, function, string(protocolMode), protocolValue}
	fields = append(fields, arguments...)
	for _, transform := range protocolTransforms {
		fields = append(fields, transform.function, strconv.Itoa(transform.inputArgument))
		fields = append(fields, transform.arguments...)
	}
	for _, field := range fields {
		fmt.Fprintf(hash, "%d:%s", len(field), field)
	}
	token := linuxProbeSymbolPrefix + hex.EncodeToString(hash.Sum(nil))
	makeText := &linuxProbeMakeText{
		function: function, arguments: slices.Clone(arguments),
		protocolValue: protocolValue, protocolMode: protocolMode,
		protocolTransforms: cloneLinuxProbeMakeTextProtocolTransforms(protocolTransforms),
	}
	symbol := linuxProbeSymbol{kind: "make-text", makeText: makeText}
	if existing, ok := e.symbols[token]; ok {
		if existing.kind != symbol.kind || existing.makeText == nil ||
			existing.makeText.function != makeText.function ||
			existing.makeText.protocolValue != makeText.protocolValue ||
			existing.makeText.protocolMode != makeText.protocolMode ||
			!equalLinuxProbeMakeTextProtocolTransforms(existing.makeText.protocolTransforms, makeText.protocolTransforms) ||
			!slices.Equal(existing.makeText.arguments, makeText.arguments) {
			return "", fmt.Errorf("Linux probe symbolic value collision %q", token)
		}
	}
	if err := e.publishSymbol(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

func cloneLinuxProbeMakeTextProtocolTransforms(transforms []linuxProbeMakeTextProtocolTransform) []linuxProbeMakeTextProtocolTransform {
	transforms = slices.Clone(transforms)
	for index := range transforms {
		transforms[index].arguments = slices.Clone(transforms[index].arguments)
	}
	return transforms
}

func equalLinuxProbeMakeTextProtocolTransforms(left, right []linuxProbeMakeTextProtocolTransform) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].function != right[index].function || left[index].inputArgument != right[index].inputArgument ||
			!slices.Equal(left[index].arguments, right[index].arguments) {
			return false
		}
	}
	return true
}

func (e *LinuxProbeEvaluator) requestTruth(request ProbeRequest, dependencies ...ProbeReference) (linuxProbeTruth, error) {
	request = e.canonicalSourceRequest(request)
	reference, err := e.discovery.Request(e.scope, request, dependencies...)
	if err != nil {
		return linuxProbeTruth{}, err
	}
	if err := e.symbolRegistry.publishDefinition(reference, request, dependencies); err != nil {
		return linuxProbeTruth{}, err
	}
	if !e.seen[reference.NodeID] {
		e.seen[reference.NodeID] = true
		e.references = append(e.references, reference)
	}
	if err := e.closeReplayPureResult(reference, request, dependencies...); err != nil {
		return linuxProbeTruth{}, err
	}
	return linuxProbeTruth{
		reference: reference, request: request,
		dependencies: slices.Clone(dependencies), trueWhenResult: true,
	}, nil
}

type linuxProbeDerivedResultState struct {
	visiting map[string]bool
	nodes    int
	depth    int
}

// readProbeResult accepts durable map_directory results first. During replay,
// parser-context operations such as wildcard may expose a new reduction over
// already measured values. Such a node needs no configured action: reproduce
// only the protocol's dependency-only, zero-step outcome in the planner and
// record it in the oracle so the replayed DAG remains strictly validated.
// Any missing process, source, scratch, or filesystem observation still fails
// closed and must have appeared in the discovery plan.
func (e *LinuxProbeEvaluator) readProbeResult(
	reference ProbeReference,
	request ProbeRequest,
	dependencies ...ProbeReference,
) (ProbeResult, error) {
	result, err := e.oracle.Result(reference)
	if err == nil {
		return result, nil
	}
	oracle, ok := e.oracle.(*ProbeResultOracle)
	if !ok || oracle.hasResult(reference.NodeID) {
		return ProbeResult{}, err
	}
	return e.derivePureProbeResult(
		oracle,
		linuxProbeRequestDefinition{
			reference:    reference,
			request:      request,
			dependencies: append([]ProbeReference(nil), dependencies...),
		},
		&linuxProbeDerivedResultState{visiting: map[string]bool{}},
	)
}

func (e *LinuxProbeEvaluator) closeReplayPureResult(
	reference ProbeReference,
	request ProbeRequest,
	dependencies ...ProbeReference,
) error {
	oracle, ok := e.oracle.(*ProbeResultOracle)
	if !ok || oracle.hasResult(reference.NodeID) || !isPureDependencyProbeRequest(request) {
		return nil
	}
	_, err := e.derivePureProbeResult(
		oracle,
		linuxProbeRequestDefinition{
			reference:    reference,
			request:      request,
			dependencies: append([]ProbeReference(nil), dependencies...),
		},
		&linuxProbeDerivedResultState{visiting: map[string]bool{}},
	)
	return err
}

func isPureDependencyProbeRequest(request ProbeRequest) bool {
	if request.InputCount == 0 || len(request.Steps) != 0 || len(request.Scratch) != 0 ||
		len(request.Sources) != 0 || len(request.SourceRoots) != 0 {
		return false
	}
	switch request.Outcome.Kind {
	case "text":
		return len(request.Outcome.Fragments) != 0 || request.Outcome.Result != ""
	case "boolean":
		return request.Outcome.Predicate != nil && IsProbeResultPredicate(*request.Outcome.Predicate)
	default:
		return false
	}
}

func (e *LinuxProbeEvaluator) derivePureProbeResult(
	oracle *ProbeResultOracle,
	definition linuxProbeRequestDefinition,
	state *linuxProbeDerivedResultState,
) (ProbeResult, error) {
	reference, request, dependencies := definition.reference, definition.request, definition.dependencies
	if result, err := oracle.Result(reference); err == nil {
		return result, nil
	} else if oracle.hasResult(reference.NodeID) {
		return ProbeResult{}, err
	}
	if state.visiting[reference.NodeID] {
		return ProbeResult{}, fmt.Errorf("cyclic derived Linux probe result %s", reference.NodeID)
	}
	if state.nodes >= maxLinuxProbeResolveNodes {
		return ProbeResult{}, fmt.Errorf("derived Linux probe replay exceeds %d nodes", maxLinuxProbeResolveNodes)
	}
	if state.depth >= maxLinuxProbeResolveDepth {
		return ProbeResult{}, fmt.Errorf("derived Linux probe replay exceeds depth %d", maxLinuxProbeResolveDepth)
	}
	if err := request.Validate(); err != nil {
		return ProbeResult{}, fmt.Errorf("invalid derived Linux probe request %s: %w", reference.NodeID, err)
	}
	requestID, err := request.ID()
	if err != nil {
		return ProbeResult{}, err
	}
	if requestID != reference.RequestID || len(dependencies) != request.InputCount {
		return ProbeResult{}, fmt.Errorf("derived Linux probe node %s has stale request identity or dependencies", reference.NodeID)
	}
	inputs := make([]string, len(dependencies))
	for index, dependency := range dependencies {
		inputs[index] = dependency.NodeID
	}
	node := ProbePlanNode{Scope: reference.Scope, RequestID: reference.RequestID, Inputs: inputs}
	if node.ContentID() != reference.NodeID {
		return ProbeResult{}, fmt.Errorf("derived Linux probe node %s has stale graph identity", reference.NodeID)
	}
	if len(request.Steps) != 0 || len(request.Scratch) != 0 || len(request.Sources) != 0 || len(request.SourceRoots) != 0 {
		return ProbeResult{}, fmt.Errorf("missing result for non-pure Linux probe node %s", reference.NodeID)
	}

	state.nodes++
	state.depth++
	state.visiting[reference.NodeID] = true
	defer func() {
		delete(state.visiting, reference.NodeID)
		state.depth--
	}()
	inputResults := make(map[string]ProbeResult, len(dependencies))
	for index, dependency := range dependencies {
		result, resultErr := oracle.Result(dependency)
		if resultErr != nil && !oracle.hasResult(dependency.NodeID) {
			dependencyDefinition, exists := e.symbolRegistry.lookupDefinition(dependency.NodeID)
			if !exists {
				return ProbeResult{}, fmt.Errorf(
					"derive Linux probe node %s: missing dependency definition %s: %w",
					reference.NodeID,
					dependency.NodeID,
					resultErr,
				)
			}
			if dependencyDefinition.reference != dependency {
				return ProbeResult{}, fmt.Errorf(
					"derive Linux probe node %s dependency %s has stale definition identity",
					reference.NodeID,
					dependency.NodeID,
				)
			}
			result, resultErr = e.derivePureProbeResult(oracle, dependencyDefinition, state)
			if resultErr == nil {
				result, resultErr = oracle.Result(dependency)
			}
		}
		if resultErr != nil {
			return ProbeResult{}, fmt.Errorf("derive Linux probe node %s dependency %s: %w", reference.NodeID, dependency.NodeID, resultErr)
		}
		if err := result.Validate(); err != nil {
			return ProbeResult{}, fmt.Errorf("derive Linux probe node %s invalid dependency %s: %w", reference.NodeID, dependency.NodeID, err)
		}
		inputResults[fmt.Sprintf("%08d", index)] = result
	}

	result := ProbeResult{
		Schema:          LinuxProbeResultSchema,
		NodeID:          reference.NodeID,
		RequestID:       reference.RequestID,
		Scope:           reference.Scope,
		ToolsetIdentity: oracle.toolsetIdentity(reference.Scope),
		Kind:            request.Outcome.Kind,
	}
	switch request.Outcome.Kind {
	case "text":
		switch {
		case len(request.Outcome.Fragments) != 0:
			result.Text, err = RenderProbeDependencyFragments(request.Outcome.Fragments, inputResults)
		case request.Outcome.Result != "":
			input, exists := inputResults[request.Outcome.Result]
			if !exists || input.Kind != "text" {
				err = fmt.Errorf("text outcome result %q is not text", request.Outcome.Result)
				break
			}
			fields := strings.Fields(input.Text)
			if request.Outcome.Word <= len(fields) {
				result.Text = fields[request.Outcome.Word-1]
			}
		default:
			err = fmt.Errorf("text outcome is not a dependency-only reduction")
		}
	case "boolean":
		if request.Outcome.Predicate == nil {
			err = fmt.Errorf("boolean outcome has no predicate")
			break
		}
		var value bool
		value, err = EvaluateProbeResultPredicate(*request.Outcome.Predicate, inputResults)
		if err == nil {
			result.Boolean = &value
		}
	default:
		err = fmt.Errorf("unsupported outcome kind %q", request.Outcome.Kind)
	}
	if err != nil {
		return ProbeResult{}, fmt.Errorf("derive Linux probe node %s: %w", reference.NodeID, err)
	}
	if err := oracle.recordDerivedResult(result); err != nil {
		return ProbeResult{}, err
	}
	return oracle.Result(reference)
}

func (e *LinuxProbeEvaluator) readBoolean(reference ProbeReference, request ProbeRequest, dependencies ...ProbeReference) (bool, error) {
	result, err := e.readProbeResult(reference, request, dependencies...)
	if err != nil {
		requestData, _ := request.CanonicalJSON()
		return false, fmt.Errorf(
			"read Linux capability result %s for request %s with dependencies %v: %w",
			reference.NodeID,
			strings.TrimSpace(string(requestData)),
			dependencies,
			err,
		)
	}
	if err := result.Validate(); err != nil {
		return false, fmt.Errorf("invalid Linux capability result %s: %w", reference.NodeID, err)
	}
	requestID, err := request.ID()
	if err != nil {
		return false, err
	}
	if requestID != reference.RequestID || result.RequestID != requestID {
		return false, fmt.Errorf("Linux capability result %s has stale request identity", reference.NodeID)
	}
	if result.Kind != "boolean" || result.Boolean == nil {
		return false, fmt.Errorf("Linux capability result %s is not boolean", reference.NodeID)
	}
	if len(request.Steps) != len(result.Steps) {
		return false, fmt.Errorf("Linux capability result %s does not match its exact step plan", reference.NodeID)
	}
	for index := range request.Steps {
		if result.Steps[index].Name != request.Steps[index].Name {
			return false, fmt.Errorf("Linux capability result %s does not match its exact step plan", reference.NodeID)
		}
	}
	if len(request.Steps) == 0 && request.InputCount != 0 && request.Outcome.Predicate != nil && IsProbeResultPredicate(*request.Outcome.Predicate) {
		if len(dependencies) != request.InputCount {
			return false, fmt.Errorf("Linux capability result %s has unavailable dependency reduction", reference.NodeID)
		}
		inputs := make(map[string]ProbeResult, len(dependencies))
		for index, dependencyReference := range dependencies {
			dependency, dependencyErr := e.oracle.Result(dependencyReference)
			if dependencyErr != nil {
				return false, dependencyErr
			}
			if err := dependency.Validate(); err != nil {
				return false, fmt.Errorf("Linux capability result %s has invalid dependency %s: %w", reference.NodeID, dependencyReference.NodeID, err)
			}
			inputs[fmt.Sprintf("%08d", index)] = dependency
		}
		want, evaluateErr := EvaluateProbeResultPredicate(*request.Outcome.Predicate, inputs)
		if evaluateErr != nil {
			return false, fmt.Errorf("recompute Linux derived boolean probe result %s: %w", reference.NodeID, evaluateErr)
		}
		if *result.Boolean != want {
			return false, fmt.Errorf("Linux capability result %s disagrees with its exact dependency reduction", reference.NodeID)
		}
	}
	return *result.Boolean, nil
}

func (e *LinuxProbeEvaluator) readText(reference ProbeReference, request ProbeRequest, dependencies ...ProbeReference) (string, error) {
	result, err := e.readProbeResult(reference, request, dependencies...)
	if err != nil {
		return "", err
	}
	if err := result.Validate(); err != nil {
		return "", fmt.Errorf("invalid Linux text probe result %s: %w", reference.NodeID, err)
	}
	requestID, err := request.ID()
	if err != nil {
		return "", err
	}
	if requestID != reference.RequestID || result.RequestID != requestID || result.Kind != "text" {
		return "", fmt.Errorf("Linux text probe result %s has stale request identity or kind", reference.NodeID)
	}
	if len(request.Steps) != len(result.Steps) {
		return "", fmt.Errorf("Linux text probe result %s does not match its exact step plan", reference.NodeID)
	}
	for index := range request.Steps {
		if result.Steps[index].Name != request.Steps[index].Name {
			return "", fmt.Errorf("Linux text probe result %s does not match its exact step plan", reference.NodeID)
		}
	}
	want := ""
	if len(request.Outcome.Fragments) != 0 {
		if len(dependencies) != request.InputCount {
			return "", fmt.Errorf("Linux text probe result %s has unavailable derived-text dependencies", reference.NodeID)
		}
		inputs := make(map[string]ProbeResult, len(dependencies))
		for index, dependencyReference := range dependencies {
			dependency, err := e.oracle.Result(dependencyReference)
			if err != nil {
				return "", err
			}
			if err := dependency.Validate(); err != nil {
				return "", fmt.Errorf("Linux text probe result %s has invalid dependency %s: %w", reference.NodeID, dependencyReference.NodeID, err)
			}
			inputs[fmt.Sprintf("%08d", index)] = dependency
		}
		want, err = RenderProbeDependencyFragments(request.Outcome.Fragments, inputs)
		if err != nil {
			return "", fmt.Errorf("recompute Linux derived text probe result %s: %w", reference.NodeID, err)
		}
	} else if request.Outcome.Result != "" {
		if len(dependencies) != request.InputCount {
			return "", fmt.Errorf("Linux text probe result %s has unavailable dependency reduction", reference.NodeID)
		}
		dependencyIndex, parseErr := strconv.Atoi(request.Outcome.Result)
		if parseErr != nil || dependencyIndex < 0 || dependencyIndex >= len(dependencies) {
			return "", fmt.Errorf("Linux text probe result %s has unavailable dependency result %q", reference.NodeID, request.Outcome.Result)
		}
		dependency, err := e.oracle.Result(dependencies[dependencyIndex])
		if err != nil {
			return "", err
		}
		if dependency.Kind != "text" {
			return "", fmt.Errorf("Linux text probe result %s depends on a non-text result", reference.NodeID)
		}
		fields := strings.Fields(dependency.Text)
		if request.Outcome.Word <= len(fields) {
			want = fields[request.Outcome.Word-1]
		}
	} else {
		step, ok := probeResultStep(result.Steps, request.Outcome.Step)
		if !ok || step.Status == "skipped" {
			return "", fmt.Errorf("Linux text probe result %s omits outcome step %q", reference.NodeID, request.Outcome.Step)
		}
		if request.Outcome.RequireSuccess && (step.Status != "success" || step.ExitCode != 0) {
			return "", fmt.Errorf(
				"Linux text probe result %s outcome step %q failed with exit code %d",
				reference.NodeID, request.Outcome.Step, step.ExitCode,
			)
		}
		if request.Steps[0].StdoutExecrootRelative && (step.Status != "success" || step.ExitCode != 0 || strings.TrimSpace(step.Stderr) != "") {
			return "", fmt.Errorf("Linux path probe result %s process did not succeed cleanly", reference.NodeID)
		}
		want = step.Stdout
		if request.Outcome.GNUMakeShell {
			want = NormalizeGNUMakeShellOutput(want)
		}
		if request.Outcome.TrimSpace {
			want = strings.TrimSpace(want)
		}
		if request.Outcome.FirstLine {
			want = strings.SplitN(want, "\n", 2)[0]
		}
		if request.Outcome.LastLine {
			lines := strings.Split(want, "\n")
			want = lines[len(lines)-1]
		}
		if request.Outcome.PathComponent {
			if err := ValidateProbePathComponent(want); err != nil {
				return "", fmt.Errorf("Linux text probe result %s: %w", reference.NodeID, err)
			}
		}
		if request.Outcome.SingleMakeWord {
			if err := ValidateProbeSingleMakeWord(want); err != nil {
				return "", fmt.Errorf("Linux text probe result %s: %w", reference.NodeID, err)
			}
		}
	}
	if result.Text != want {
		return "", fmt.Errorf("Linux text probe result %s disagrees with its exact reduction", reference.NodeID)
	}
	if strings.ContainsRune(result.Text, 0) {
		return "", fmt.Errorf("Linux text probe result %s contains NUL", reference.NodeID)
	}
	if len(request.Steps) != 0 && request.Steps[0].StdoutExecrootRelative {
		if err := ValidateProbeExecrootRelativePath(result.Text); err != nil {
			return "", fmt.Errorf("Linux text probe result %s: %w", reference.NodeID, err)
		}
	}
	return result.Text, nil
}

func probeResultStep(steps []ProbeStepResult, name string) (ProbeStepResult, bool) {
	for _, step := range steps {
		if step.Name == name {
			return step, true
		}
	}
	return ProbeStepResult{}, false
}

func (e *LinuxProbeEvaluator) requestText(request ProbeRequest, dependencies ...ProbeReference) (string, error) {
	return e.requestTextInScope(e.scope, request, dependencies...)
}

// requestTextInScope records a text query against the toolset which owns the
// queried runtime policy. Most compiler queries use the evaluator's own scope;
// intrinsically host queries such as getconf use the host toolset even when a
// target-first source phase encounters them.
func (e *LinuxProbeEvaluator) requestTextInScope(scope string, request ProbeRequest, dependencies ...ProbeReference) (string, error) {
	request = e.canonicalSourceRequest(request)
	reference, err := e.discovery.Request(scope, request, dependencies...)
	if err != nil {
		return "", err
	}
	if err := e.symbolRegistry.publishDefinition(reference, request, dependencies); err != nil {
		return "", err
	}
	if !e.seen[reference.NodeID] {
		e.seen[reference.NodeID] = true
		e.references = append(e.references, reference)
	}
	if err := e.closeReplayPureResult(reference, request, dependencies...); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte("text:" + reference.NodeID))
	token := linuxProbeSymbolPrefix + hex.EncodeToString(hash[:])
	symbol := linuxProbeSymbol{
		kind: "text", reference: reference, request: request,
		dependencies: slices.Clone(dependencies),
	}
	if err := e.publishSymbol(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

func (e *LinuxProbeEvaluator) shellSetEcho(command string) (string, error) {
	before, after, ok := strings.Cut(command, "&&")
	if !ok {
		return "", fmt.Errorf("unsupported set command %q", command)
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(before, "set -- ")))
	echo := strings.TrimSpace(after)
	if !strings.HasPrefix(echo, "echo $") {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	index, err := strconv.Atoi(strings.TrimPrefix(echo, "echo $"))
	if err != nil || index < 1 {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	if len(fields) == 1 && linuxProbeSymbolPattern.MatchString(fields[0]) {
		symbol, exists, err := e.adoptSymbol(fields[0])
		if err != nil {
			return "", err
		}
		if !exists || symbol.kind != "text" {
			return "", fmt.Errorf("set echo input is not a measured text result: %q", fields[0])
		}
		return e.requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema, InputCount: 1,
			Outcome: ProbeOutcome{Kind: "text", Result: "00000000", Word: index},
		}, symbol.reference)
	}
	if index > len(fields) {
		return "", nil
	}
	return fields[index-1], nil
}

func (e *LinuxProbeEvaluator) compilerPrintFileName(name string) (string, error) {
	if !safeLinuxProbePathComponent(name) {
		return "", fmt.Errorf("unsupported compiler print-file-name value %q", name)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "print-file", Tool: "cc", Arguments: []string{"-print-file-name=" + name},
			StdoutExecrootRelative: true, StdoutFallbackPath: name,
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "print-file", Stream: "stdout", TrimSpace: true,
		},
	}
	return e.requestText(request)
}

func (e *LinuxProbeEvaluator) compilerPrintFileCommand(command string) (string, bool, error) {
	fields := strings.Fields(command)
	if len(fields) == 3 && fields[2] == "2>/dev/null" {
		fields = fields[:2]
	}
	if len(fields) != 2 || !e.isToolToken(fields[0], "cc") {
		return "", false, nil
	}
	name, recognized := strings.CutPrefix(fields[1], "-print-file-name=")
	if !recognized {
		return "", false, nil
	}
	if !safeLinuxProbePathComponent(name) {
		return "", true, fmt.Errorf("unsupported compiler print-file-name value %q", name)
	}
	return name, true, nil
}

func safeLinuxProbePathComponent(value string) bool {
	return value != "." && value != ".." && linuxProbeSafePathComponent.MatchString(value)
}

// rustcPrintFileNames lowers Linux's source-owned procedural-macro filename
// query. The result is intentionally constrained to one safe component before
// it may become an always-y entry or a rule target.
func (e *LinuxProbeEvaluator) rustcPrintFileNames(command string) (string, bool, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false, nil
	}
	environment := map[string]string(nil)
	if fields[0] == "MAKEFLAGS=" {
		environment = map[string]string{"MAKEFLAGS": ""}
		fields = fields[1:]
	}
	if len(fields) == 0 || !e.isToolToken(fields[0], "rustc") {
		return "", false, nil
	}
	// Once the selected rustc owns the command, malformed variants fail closed.
	if len(fields) == 9 && fields[7] == "-" && fields[8] == "</dev/null" {
		fields = fields[:8]
	} else if len(fields) == 10 && fields[7] == "-" && fields[8] == "<" && fields[9] == "/dev/null" {
		fields = fields[:8]
	} else {
		return "", true, e.unsupportedCommand(command)
	}
	if !slices.Equal(fields[1:3], []string{"--print", "file-names"}) ||
		fields[3] != "--crate-name" || !safeLinuxProbePathComponent(fields[4]) ||
		fields[5] != "--crate-type" || !safeLinuxProbePathComponent(fields[6]) || fields[7] != "-" {
		return "", true, e.unsupportedCommand(command)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "print-file-names", Tool: "rustc", Arguments: slices.Clone(fields[1:]), Environment: environment,
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "print-file-names", Stream: "stdout", TrimSpace: true, PathComponent: true,
		},
	}
	value, err := e.requestText(request)
	return value, true, err
}

// directToolVersionOutput lowers a source-selected direct `--version` query
// without carrying a table of optional tool names or compatibility arguments.
// An exact configured token becomes an identity-bound action. A safe bare
// command outside the selected tool manifest is deterministically absent from
// the planner's intentionally empty PATH and therefore has empty stdout. All
// other program spellings and argv fail closed.
func (e *LinuxProbeEvaluator) directToolVersionOutput(command string) (string, bool, error) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[1] != "--version" {
		return "", false, nil
	}
	if len(fields) > 18 {
		return "", true, e.unsupportedCommand(command)
	}
	if fields[len(fields)-1] == "2>/dev/null" {
		fields = fields[:len(fields)-1]
	}
	for _, argument := range fields[2:] {
		// Compatibility operands after --version are opaque safe names, not
		// another option surface. In particular, do not let this generic query
		// admit output, plugin, response-file, or filesystem selection flags.
		if strings.HasPrefix(argument, "-") || !safeLinuxProbePathComponent(argument) {
			return "", true, e.unsupportedCommand(command)
		}
	}
	role, selected, err := e.configuredToolRole(fields[0])
	if err != nil {
		return "", true, err
	}
	if !selected {
		program := fields[0]
		if name, environmentToken := linuxProbeShellEnvironmentToken(program); environmentToken {
			value, exists := e.scriptEnvironment[name]
			if !exists {
				return "", true, e.unsupportedCommand(command)
			}
			program = value
		}
		program, static := linuxProbeStaticShellWord(program)
		if !static || strings.HasPrefix(program, kbuildActionRoleTokenPrefix) || !safeLinuxSourceScriptCommandName(program) {
			return "", true, e.unsupportedCommand(command)
		}
		return "", true, nil
	}
	arguments := slices.Clone(fields[1:])
	value, err := e.requestText(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "version", Tool: role, Arguments: arguments,
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "version", Stream: "stdout", TrimSpace: true},
	})
	return value, true, err
}

// compilerPreprocessorGrep models source-owned feature tests which preprocess
// an inline header and use grep only as a non-empty/empty reduction.  The
// complete compiler argv remains source-derived; the evaluator merely lowers
// the pipeline into one identity-bound compiler action and a generic stdout
// predicate.
func (e *LinuxProbeEvaluator) compilerPreprocessorGrep(command string) (string, bool, error) {
	segments := strings.Split(command, "|")
	if len(segments) != 3 {
		return "", false, nil
	}
	producer := strings.TrimSpace(segments[0])
	if !strings.HasPrefix(producer, "echo ") {
		return "", false, nil
	}
	source, err := unquoteLinuxProbeSource(strings.TrimSpace(strings.TrimPrefix(producer, "echo ")))
	if err != nil {
		return "", true, err
	}
	// Kbuild spells a literal make-comment marker as \# before the shell
	// quoting layer.  The shell-visible stdin contains the preprocessor marker.
	if withoutEscape := strings.TrimLeft(source, `\`); strings.HasPrefix(withoutEscape, "#") {
		source = withoutEscape
	}

	grep := strings.Fields(strings.TrimSpace(segments[2]))
	if len(grep) != 2 || grep[0] != "grep" || !linuxProbePreprocessorSymbol.MatchString(grep[1]) {
		return "", true, e.unsupportedCommand(command)
	}
	fields := strings.Fields(strings.TrimSpace(segments[1]))
	if len(fields) < 5 || !e.isToolToken(fields[0], "cc") {
		return "", true, e.unsupportedCommand(command)
	}
	if fields[len(fields)-1] == "2>/dev/null" {
		fields = fields[:len(fields)-1]
	}
	preprocess, stdin, language := false, false, ""
	candidate := make([]string, 0, len(fields))
	for index := 1; index < len(fields); index++ {
		argument := fields[index]
		switch argument {
		case "-E":
			preprocess = true
		case "-":
			stdin = true
		case "-x":
			if index+1 >= len(fields) {
				return "", true, e.unsupportedCommand(command)
			}
			index++
			language = fields[index]
		case "-I", "-iquote", "-isystem":
			if index+1 >= len(fields) {
				return "", true, e.unsupportedCommand(command)
			}
			index++
			candidate = append(candidate, argument+probeCompilerRootArgument(fields[index]))
		default:
			candidate = append(candidate, probeCompilerRootArgument(argument))
		}
	}
	if !preprocess || !stdin || language != "c" {
		return "", true, e.unsupportedCommand(command)
	}
	if err := validatePreprocessorGrepIncludes(candidate); err != nil {
		return "", true, fmt.Errorf("invalid preprocessor grep candidate: %w", err)
	}
	arguments, conditional, argumentFragments, candidateOwnership, stdinValue, stdinFragments, dependencies, err := e.lowerSymbolicCandidateProcessInputs(
		candidate, probeCandidateArgumentMask(len(candidate)), ProbeCandidatePolicyCC, source+"\n",
	)
	if err != nil {
		return "", true, err
	}
	arguments = append(arguments, "-x", language, "-E", "-")
	sourceRoots := []string{linuxProbeSourceRootName}
	for _, argument := range arguments {
		if strings.Contains(argument, "${source_root:"+linuxProbeHostDepsRootName+"}") {
			sourceRoots = append(sourceRoots, linuxProbeHostDepsRootName)
			break
		}
	}
	slices.Sort(sourceRoots)
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Sources: []string{linuxProbeRootAnchor}, SourceRoots: sourceRoots,
		Steps: []ProbeStep{{
			Name: "preprocess", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidateOwnership,
			Stdin: stdinValue, StdinFragments: stdinFragments,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "all",
			Operands: []ProbePredicate{
				{Operator: "exit-zero", Step: "preprocess"},
				{Operator: "stream-contains", Step: "preprocess", Stream: "stdout", Value: grep[1]},
			},
		}},
	}
	truth, err := e.requestTruth(request, dependencies...)
	if err != nil {
		return "", true, err
	}
	value, err := e.renderTruth(truth, "y", "")
	return value, true, err
}

var probeCompilerRootArgumentReplacer = strings.NewReplacer(
	"__LINUX_BZL_SOURCE_TREE__", "${source_root:"+linuxProbeSourceRootName+"}",
	linuxProbeHostDepsSentinel, "${source_root:"+linuxProbeHostDepsRootName+"}",
)

func probeCompilerRootArgument(value string) string {
	return probeCompilerRootArgumentReplacer.Replace(value)
}

func validatePreprocessorGrepIncludes(arguments []string) error {
	for _, argument := range arguments {
		include := ""
		for _, prefix := range []string{"-isystem", "-iquote", "-I"} {
			if strings.HasPrefix(argument, prefix) && len(argument) > len(prefix) {
				include = argument[len(prefix):]
				break
			}
		}
		if include == "" {
			continue
		}
		sourcePrefix := "${source_root:" + linuxProbeSourceRootName + "}/"
		if relative, ok := strings.CutPrefix(include, sourcePrefix); ok {
			if err := validateProbeSourcePath(relative); err != nil {
				return err
			}
			continue
		}
		hostDepsPrefix := "${source_root:" + linuxProbeHostDepsRootName + "}/"
		if relative, ok := strings.CutPrefix(include, hostDepsPrefix); ok {
			if err := validateProbeSourcePath(relative); err != nil {
				return err
			}
			continue
		}
		if err := ValidateProbeExecrootRelativePath(include); err != nil {
			return fmt.Errorf("unsafe preprocessor include directory: %w", err)
		}
	}
	return nil
}

// compilerPreprocessorTailOutput models a source-selected preprocessor query
// whose shell reduction is `tail -n 1`. The safe stdin token, exact compiler
// argv, and last-line operation all come from the Makefile shape; the request
// contains no compiler-family, architecture, macro, or expected-value table.
func (e *LinuxProbeEvaluator) compilerPreprocessorTailOutput(command string) (string, bool, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		if strings.Contains(command, "tail") && e.ownsProbeCommand(command) {
			return "", true, e.unsupportedCommand(command)
		}
		return "", false, nil
	}
	compiler := -1
	pipeCount := 0
	hasTail := false
	for index, token := range tokens {
		if token.operator {
			if token.value == "|" {
				pipeCount++
			}
			continue
		}
		if compiler < 0 && e.isToolToken(token.value, "cc") {
			compiler = index
		}
		if token.value == "tail" {
			hasTail = true
		}
	}
	if compiler < 0 || pipeCount < 2 || !hasTail {
		return "", false, nil
	}
	// A selected compiler inside a multi-stage echo pipeline makes this an
	// evaluator-owned probe. Every deviation from the admitted shape fails
	// closed instead of becoming an ambient shell command.
	if len(command) > 4096 || len(tokens) > 128 || len(tokens) < 11 ||
		tokens[0].operator || tokens[0].value != "echo" || tokens[1].operator ||
		len(tokens[1].value) > 256 || !linuxProbePreprocessorSymbol.MatchString(tokens[1].value) ||
		!tokens[2].operator || tokens[2].value != "|" || compiler != 3 {
		return "", true, e.unsupportedCommand(command)
	}
	secondPipe := -1
	for index := 3; index < len(tokens); index++ {
		if !tokens[index].operator {
			continue
		}
		if tokens[index].value != "|" || secondPipe >= 0 {
			return "", true, e.unsupportedCommand(command)
		}
		secondPipe = index
	}
	if secondPipe <= compiler+1 {
		return "", true, e.unsupportedCommand(command)
	}
	tail := tokens[secondPipe+1:]
	validTail := len(tail) == 3 && !tail[0].operator && tail[0].value == "tail" &&
		!tail[1].operator && tail[1].value == "-n" && !tail[2].operator && tail[2].value == "1"
	validCompactTail := len(tail) == 2 && !tail[0].operator && tail[0].value == "tail" &&
		!tail[1].operator && tail[1].value == "-n1"
	if !validTail && !validCompactTail {
		return "", true, e.unsupportedCommand(command)
	}

	arguments := make([]string, 0, secondPipe-compiler-1)
	candidateArguments := make([]bool, 0, secondPipe-compiler-1)
	preprocess, languageC, stdin := 0, 0, 0
	for index := compiler + 1; index < secondPipe; index++ {
		if tokens[index].operator {
			return "", true, e.unsupportedCommand(command)
		}
		argument := tokens[index].value
		arguments = append(arguments, argument)
		candidateArguments = append(candidateArguments, false)
		switch argument {
		case "-E":
			preprocess++
		case "-x":
			if index+1 >= secondPipe || tokens[index+1].operator || tokens[index+1].value != "c" {
				return "", true, e.unsupportedCommand(command)
			}
			arguments = append(arguments, tokens[index+1].value)
			candidateArguments = append(candidateArguments, false)
			languageC++
			index++
		case "-":
			stdin++
		default:
			candidateArguments[len(candidateArguments)-1] = true
		}
	}
	if preprocess != 1 || languageC != 1 || stdin != 1 {
		return "", true, e.unsupportedCommand(command)
	}
	arguments, conditional, argumentFragments, candidate, stdinValue, stdinFragments, dependencies, err := e.lowerSymbolicCandidateProcessInputs(
		arguments, candidateArguments, ProbeCandidatePolicyCC, tokens[1].value+"\n",
	)
	if err != nil {
		return "", true, err
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "preprocess", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidate,
			Stdin: stdinValue, StdinFragments: stdinFragments,
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "preprocess", Stream: "stdout", TrimSpace: true, LastLine: true,
		},
	}
	value, err := e.requestText(request, dependencies...)
	return value, true, err
}

// compilerPreprocessorOutput models a source-selected preprocessor macro query.
// Architecture Kconfig files use this shape to inspect target-specific driver
// predefines. The identifier and candidate flags come from the selected source;
// the planner does not carry an architecture or macro-name inventory.
func (e *LinuxProbeEvaluator) compilerPreprocessorOutput(command string) (string, bool, error) {
	left, right, ok := strings.Cut(command, "|")
	if !ok {
		return "", false, nil
	}
	producer := strings.Fields(strings.TrimSpace(left))
	if len(producer) != 2 || producer[0] != "echo" ||
		len(producer[1]) > 256 || !linuxProbePreprocessorSymbol.MatchString(producer[1]) {
		return "", false, nil
	}
	fields := strings.Fields(strings.TrimSpace(right))
	if len(fields) < 4 || !e.isToolToken(fields[0], "cc") {
		return "", true, e.unsupportedCommand(command)
	}
	if fields[len(fields)-1] == "2>/dev/null" {
		fields = fields[:len(fields)-1]
	}
	if len(fields) < 4 || !slices.Equal(fields[len(fields)-3:], []string{"-E", "-P", "-"}) {
		return "", true, e.unsupportedCommand(command)
	}
	candidate := slices.Clone(fields[1 : len(fields)-3])
	arguments, conditional, argumentFragments, candidateOwnership, stdinValue, stdinFragments, dependencies, err := e.lowerSymbolicCandidateProcessInputs(
		candidate, probeCandidateArgumentMask(len(candidate)), ProbeCandidatePolicyCC, producer[1]+"\n",
	)
	if err != nil {
		return "", true, err
	}
	arguments = append(arguments, "-E", "-P", "-")
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "preprocess", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidateOwnership,
			Stdin: stdinValue, StdinFragments: stdinFragments,
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "preprocess", Stream: "stdout", TrimSpace: true},
	}
	value, err := e.requestText(request, dependencies...)
	return value, true, err
}

func (e *LinuxProbeEvaluator) rustCompilerOptionProbe(command string) (linuxProbeTruth, bool, error) {
	if !strings.Contains(command, "--crate-type=rlib") {
		return linuxProbeTruth{}, false, nil
	}
	if e.tools["rustc"] == "" {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	fields := strings.Fields(command)
	compiler := -1
	for index, field := range fields {
		if e.isToolToken(field, "rustc") {
			compiler = index
			break
		}
	}
	if compiler < 0 {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	inlineEnvironment := map[string]string{}
	for index := compiler - 1; index >= 0; index-- {
		name, raw, assignment := strings.Cut(fields[index], "=")
		if !assignment {
			break
		}
		if !validKbuildCommandEnvironmentName(name) {
			return linuxProbeTruth{}, true, fmt.Errorf("invalid rustc-option environment assignment %q", fields[index])
		}
		value := ""
		if raw != "" {
			var static bool
			value, static = linuxProbeStaticShellWord(raw)
			if !static || !linuxProbeSafeEnvironmentValue.MatchString(value) {
				return linuxProbeTruth{}, true, fmt.Errorf("unsafe rustc-option environment assignment %q", fields[index])
			}
		}
		if _, exists := inlineEnvironment[name]; !exists {
			// Shell assignment words are applied from left to right. Walking
			// backwards therefore preserves the value nearest the command.
			inlineEnvironment[name] = value
		}
	}
	crate := -1
	for index := compiler + 1; index < len(fields); index++ {
		if fields[index] == "--crate-type=rlib" {
			crate = index
			break
		}
	}
	if crate < 0 || crate+4 >= len(fields) || fields[crate+1] != "/dev/null" ||
		!strings.HasPrefix(fields[crate+2], "--out-dir=") || fields[crate+3] != "-o" ||
		!strings.HasPrefix(strings.TrimSuffix(fields[crate+4], ";"), ".tmp_") {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	for _, field := range fields[crate+5:] {
		if strings.TrimSpace(strings.Trim(field, ";{}")) != "" {
			return linuxProbeTruth{}, true, e.unsupportedCommand(command)
		}
	}
	candidate := slices.Clone(fields[compiler+1 : crate])
	if err := validateRustProbeCandidate(candidate); err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("invalid rustc-option candidate: %w", err)
	}
	environment, auxiliaryTools, sourceRoots, err := e.probeStepEnvironment("rustc", inlineEnvironment)
	if err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("invalid rustc-option environment: %w", err)
	}
	arguments := append(candidate, "--crate-type=rlib", "/dev/null", "--out-dir=${scratch:out}", "-o", "${scratch:out}/tmp.rlib")
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, Scratch: []ProbeScratch{{Name: "out", Kind: "directory"}},
		SourceRoots: sourceRoots,
		Steps: []ProbeStep{{
			Name: "probe", Tool: "rustc", Arguments: arguments,
			AuxiliaryTools: auxiliaryTools, Environment: environment,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "probe"}},
	}
	truth, err := e.requestTruth(request)
	return truth, true, err
}

func (e *LinuxProbeEvaluator) probeStepEnvironment(primaryRole string, inline map[string]string) (map[string]string, []string, []string, error) {
	merged := maps.Clone(e.scriptEnvironment)
	if merged == nil {
		merged = map[string]string{}
	}
	for name, value := range inline {
		merged[name] = value
	}
	environment := make(map[string]string, len(merged))
	auxiliarySet := map[string]bool{}
	sourceRootSet := map[string]bool{}
	for name, value := range merged {
		if !validKbuildCommandEnvironmentName(name) || strings.ContainsRune(value, 0) {
			return nil, nil, nil, fmt.Errorf("invalid environment variable %q", name)
		}
		if err := validateSourceScriptProtocolLiteral(value); err != nil {
			return nil, nil, nil, fmt.Errorf("environment %s: %w", name, err)
		}
		if e.rustSourceRoot != "" && value == e.rustSourceRoot {
			environment[name] = "${source_root:" + rustProbeSourceRootName + "}"
			sourceRootSet[rustProbeSourceRootName] = true
			continue
		}
		role, selected, err := e.configuredSourceScriptToolRole(value)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("environment %s: %w", name, err)
		}
		if selected {
			environment[name] = "${tool:" + role + "}"
			if role != primaryRole {
				auxiliarySet[role] = true
			}
			continue
		}
		environment[name] = value
	}
	auxiliaryTools := make([]string, 0, len(auxiliarySet))
	for role := range auxiliarySet {
		auxiliaryTools = append(auxiliaryTools, role)
	}
	slices.Sort(auxiliaryTools)
	sourceRoots := make([]string, 0, len(sourceRootSet))
	for root := range sourceRootSet {
		sourceRoots = append(sourceRoots, root)
	}
	slices.Sort(sourceRoots)
	return environment, auxiliaryTools, sourceRoots, nil
}

func validateRustProbeCandidate(candidate []string) error {
	if len(candidate) == 0 {
		return fmt.Errorf("empty candidate")
	}
	for _, argument := range candidate {
		if err := validateProbeToken(argument); err != nil {
			return err
		}
		lower := strings.ToLower(argument)
		for _, forbidden := range []string{"-o", "--out-dir", "--extern", "--sysroot", "-l", "-clinker", "-clink-arg", "-clink-args"} {
			if lower == forbidden || strings.HasPrefix(lower, forbidden+"=") {
				return fmt.Errorf("file, output, or linker selection option is prohibited: %q", argument)
			}
		}
		if !strings.HasPrefix(argument, "-") || strings.ContainsAny(argument, `/\\`) || !regexp.MustCompile(`^[-A-Za-z0-9_=+.,:]+$`).MatchString(argument) {
			return fmt.Errorf("unsafe rustc option %q", argument)
		}
	}
	return nil
}

func (e *LinuxProbeEvaluator) auxiliaryToolGrepProbe(command string) (linuxProbeTruth, bool, error) {
	tokens, lexErr := lexCompactKbuildRecipe(command)
	if lexErr != nil {
		if strings.Contains(command, kbuildActionRoleTokenPrefix) {
			return linuxProbeTruth{}, true, e.unsupportedCommand(command)
		}
		return linuxProbeTruth{}, false, nil
	}
	if len(tokens) < 7 {
		return linuxProbeTruth{}, false, nil
	}
	fields := make([]string, len(tokens))
	for index, token := range tokens {
		fields[index] = token.value
	}
	tool := fields[0]
	ref, selected := parseKbuildActionRoleToken(tool)
	if !selected {
		return linuxProbeTruth{}, false, nil
	}
	ref, matches := kbuildActionRoleRefForScope(ref, e.scope)
	if !matches || e.tools[ref.Role] == "" {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	if tokens[0].operator || tokens[1].operator || (fields[1] != "--help" && fields[1] != "--version") || !tokens[2].operator || fields[2] != "|" || tokens[3].operator || fields[3] != "head" || (fields[4] != "-n" && fields[4] != "-n1") {
		return linuxProbeTruth{}, false, nil
	}
	grep := 5
	if fields[4] == "-n" {
		if len(fields) < 8 || tokens[5].operator || fields[5] != "1" || !tokens[6].operator || fields[6] != "|" {
			return linuxProbeTruth{}, true, e.unsupportedCommand(command)
		}
		grep = 7
	} else if !tokens[5].operator || fields[5] != "|" {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	} else {
		grep = 6
	}
	if len(fields) != grep+3 || tokens[grep].operator || fields[grep] != "grep" || tokens[grep+1].operator || tokens[grep+2].operator {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	caseInsensitive, invert, validOptions := parseLinuxProbeGrepOptions(fields[grep+1])
	literal, validLiteral := parseLinuxProbeGrepLiteral(fields[grep+2])
	if !validOptions || !validLiteral {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	// The source pattern is admitted only when it is a BRE literal, so quoting
	// it for RE2 preserves grep's matching semantics instead of silently
	// changing a supported regular expression into a literal.
	expression := `^[^\n]*` + regexp.QuoteMeta(literal)
	if caseInsensitive {
		expression = "(?i)" + expression
	}
	match := ProbePredicate{Operator: "stream-matches", Step: "identify", Stream: "stdout", Value: expression}
	if invert {
		match = ProbePredicate{Operator: "all", Operands: []ProbePredicate{
			{Operator: "not", Operands: []ProbePredicate{{Operator: "stream-empty", Step: "identify", Stream: "stdout"}}},
			{Operator: "not", Operands: []ProbePredicate{match}},
		}}
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "identify", Tool: ref.Role, Arguments: []string{fields[1]}}},
		// A shell pipeline without pipefail reports grep's status, not the
		// producer's. The stream predicate models grep directly, including tools
		// that print useful identification text and then exit non-zero.
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &match},
	}
	truth, err := e.requestTruth(request)
	return truth, true, err
}

func parseLinuxProbeGrepOptions(value string) (caseInsensitive, invert, ok bool) {
	if len(value) < 2 || value[0] != '-' {
		return false, false, false
	}
	seen := map[byte]bool{}
	for index := 1; index < len(value); index++ {
		option := value[index]
		if seen[option] || option != 'q' && option != 'i' && option != 'v' {
			return false, false, false
		}
		seen[option] = true
	}
	return seen['i'], seen['v'], seen['q']
}

func parseLinuxProbeGrepLiteral(value string) (string, bool) {
	if value == "" || len(value) > 128 || value[0] == '-' {
		return "", false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] > 0x7e {
			return "", false
		}
	}
	// This lowering intentionally supports only literal POSIX BRE patterns.
	// Reject BRE operators, escapes, and shell expansion syntax instead of
	// quoting them and thereby changing the probe's meaning. A leading dash is
	// rejected above because the source grep invocation does not use `--`.
	if strings.ContainsAny(value, ".^$[]*\\`") {
		return "", false
	}
	return value, true
}

func (e *LinuxProbeEvaluator) compilerOptionProbe(command string) (linuxProbeTruth, bool, error) {
	fields := strings.Fields(command)
	compiler := -1
	for index, field := range fields {
		if e.isToolToken(field, "cc") {
			compiler = index
			break
		}
	}
	if compiler < 0 {
		return linuxProbeTruth{}, false, nil
	}
	hasInput, mode, language, preprocessOnly := false, "", "c", false
	var candidate []string
	for index := compiler + 1; index < len(fields); index++ {
		field := strings.TrimSuffix(fields[index], ";")
		switch field {
		case "-c":
			mode = "-c"
		case "-E":
			mode, preprocessOnly = "-E", true
		case "-Werror":
			candidate = append(candidate, field)
		case "-x":
			if index+1 >= len(fields) {
				return linuxProbeTruth{}, true, e.unsupportedCommand(command)
			}
			language = strings.TrimSuffix(fields[index+1], ";")
			if language != "c" && language != "assembler-with-cpp" {
				return linuxProbeTruth{}, true, e.unsupportedCommand(command)
			}
			index++
		case "-o":
			if index+1 >= len(fields) {
				return linuxProbeTruth{}, true, e.unsupportedCommand(command)
			}
			index++
		case "/dev/null", "-":
			hasInput = true
		case "{", "}":
		default:
			if strings.HasPrefix(field, ".tmp_") {
				continue
			}
			candidate = append(candidate, field)
		}
	}
	if mode == "" || !hasInput {
		return linuxProbeTruth{}, false, nil
	}
	if len(candidate) == 0 {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	if preprocessOnly {
		if language != "c" {
			return linuxProbeTruth{}, true, e.unsupportedCommand(command)
		}
		truth, err := e.compileRequest(candidate, "c", "-E", "\n")
		return truth, true, err
	}
	truth, err := e.compileRequest(candidate, language, "-c", "\n")
	return truth, true, err
}

func (e *LinuxProbeEvaluator) linkerOptionProbe(command string) (linuxProbeTruth, bool, error) {
	fields := strings.Fields(command)
	if len(fields) < 3 || !e.isToolToken(fields[0], "ld") {
		return linuxProbeTruth{}, false, nil
	}
	fields = slices.Clone(fields)
	fields[len(fields)-1] = strings.TrimSuffix(fields[len(fields)-1], ";")
	leadingVersion := fields[1] == "-v"
	trailingVersion := fields[len(fields)-1] == "-v"
	if leadingVersion == trailingVersion {
		return linuxProbeTruth{}, false, nil
	}
	candidate := slices.Clone(fields[1:])
	if leadingVersion {
		candidate = candidate[1:]
	} else {
		candidate = candidate[:len(candidate)-1]
	}
	arguments, conditional, argumentFragments, candidateOwnership, dependencies, err := e.lowerSymbolicCandidateArguments(
		candidate, probeCandidateArgumentMask(len(candidate)), ProbeCandidatePolicyLD,
	)
	if err != nil {
		return linuxProbeTruth{}, true, err
	}
	if leadingVersion {
		arguments = append([]string{"-v"}, arguments...)
		if candidateOwnership != nil {
			for index := range candidateOwnership.Base {
				candidateOwnership.Base[index]++
			}
		}
		for index := range conditional {
			conditional[index].Before++
		}
		for index := range argumentFragments {
			argumentFragments[index].Index++
		}
	} else {
		arguments = append(arguments, "-v")
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "probe", Tool: "ld", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidateOwnership,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "probe"}},
	}
	truth, err := e.requestTruth(request, dependencies...)
	return truth, true, err
}

func (e *LinuxProbeEvaluator) compilerSourceProbe(command string) (linuxProbeTruth, bool, error) {
	if !strings.Contains(command, "|") || !strings.Contains(command, " -x c ") ||
		(!strings.Contains(command, " -c ") && !strings.Contains(command, " -S ")) {
		return linuxProbeTruth{}, false, nil
	}
	source, mode, candidate, err := e.parseLinuxSourceProbe(command)
	if err != nil {
		return linuxProbeTruth{}, true, err
	}
	truth, err := e.compileRequest(candidate, "c", mode, source)
	return truth, true, err
}

func (e *LinuxProbeEvaluator) assemblerSourceProbe(command string) (linuxProbeTruth, bool, error) {
	if !strings.HasPrefix(command, `printf "%b\n" `) || !strings.Contains(command, " -x assembler-with-cpp ") {
		return linuxProbeTruth{}, false, nil
	}
	source, mode, candidate, err := e.parseLinuxSourceProbe(command)
	if err != nil {
		return linuxProbeTruth{}, true, err
	}
	source, err = decodeKbuildPrintfB(source)
	if err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("invalid Linux assembler source probe: %w", err)
	}
	if err := validateKbuildAssemblerProbeSource(source); err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("invalid Linux assembler source probe: %w", err)
	}
	truth, err := e.compileRequest(candidate, "assembler-with-cpp", mode, source)
	return truth, true, err
}

func (e *LinuxProbeEvaluator) compileRequest(candidate []string, language, mode, source string) (linuxProbeTruth, error) {
	arguments, conditional, argumentFragments, candidateOwnership, stdinValue, stdinFragments, dependencies, err := e.lowerSymbolicCandidateProcessInputs(
		candidate, probeCandidateArgumentMask(len(candidate)), ProbeCandidatePolicyCC, source,
	)
	if err != nil {
		return linuxProbeTruth{}, err
	}
	arguments = append(arguments, "-x", language, mode, "-o", "${scratch:output}", "-")
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Scratch: []ProbeScratch{{Name: "output", Kind: "file"}},
		Steps: []ProbeStep{{
			Name: "probe", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: argumentFragments, Candidate: candidateOwnership,
			Stdin: stdinValue, StdinFragments: stdinFragments,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "probe"}},
	}
	return e.requestTruth(request, dependencies...)
}

func (e *LinuxProbeEvaluator) parseLinuxSourceProbe(command string) (string, string, []string, error) {
	left, right, ok := strings.Cut(command, "|")
	if !ok {
		return "", "", nil, fmt.Errorf("unsupported Linux source probe %q", command)
	}
	left = strings.TrimSpace(left)
	var quoted string
	if strings.HasPrefix(left, "echo ") {
		quoted = strings.TrimSpace(strings.TrimPrefix(left, "echo "))
	} else if strings.HasPrefix(left, `printf "%b\n" `) {
		quoted = strings.TrimSpace(strings.TrimPrefix(left, `printf "%b\n" `))
	} else {
		return "", "", nil, fmt.Errorf("unsupported Linux source producer %q", left)
	}
	source, err := unquoteLinuxProbeSource(quoted)
	if err != nil {
		return "", "", nil, err
	}
	fields := strings.Fields(strings.TrimSpace(right))
	compiler := -1
	for index, field := range fields {
		if e.isToolToken(field, "cc") {
			compiler = index
			break
		}
	}
	if compiler < 0 {
		return "", "", nil, fmt.Errorf("Linux source probe has no selected compiler invocation")
	}
	mode := ""
	var candidate []string
	for index := compiler + 1; index < len(fields); index++ {
		field := strings.TrimSuffix(fields[index], ";")
		switch field {
		case "-c", "-S":
			if mode != "" && mode != field {
				return "", "", nil, fmt.Errorf("Linux source probe has conflicting compiler modes %q and %q", mode, field)
			}
			mode = field
		case "-", "/dev/null":
		case "-x":
			index++
		case "-o":
			index++
		default:
			candidate = append(candidate, field)
		}
	}
	if mode == "" {
		return "", "", nil, fmt.Errorf("Linux source probe has no compile mode")
	}
	return source, mode, candidate, nil
}

func (e *LinuxProbeEvaluator) lowerSymbolicArguments(arguments []string) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, []ProbeReference, error) {
	lowerer := newProbeSymbolicValueLowerer(e)
	base, conditional, argumentFragments, _, err := lowerProbeCandidateArguments(lowerer, arguments, nil, "")
	return base, conditional, argumentFragments, slices.Clone(lowerer.dependencies), err
}

// lowerProbeCandidateArguments lowers one source argv sequence while retaining
// which resulting slots came from compiler or linker capability candidates.
// Candidate ownership uses the unexpanded protocol coordinates: a dynamic
// fragment owns its empty base slot, and a finite symbolic branch owns each
// conditional group emitted for that source argument.
func lowerProbeCandidateArguments(
	lowerer *probeSymbolicValueLowerer,
	arguments []string,
	candidateArguments []bool,
	policy string,
) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, *ProbeCandidateArguments, error) {
	if candidateArguments != nil && len(candidateArguments) != len(arguments) {
		return nil, nil, nil, nil, fmt.Errorf(
			"probe candidate ownership has %d entries for %d arguments",
			len(candidateArguments), len(arguments),
		)
	}

	base := make([]string, 0, len(arguments))
	var conditional []ProbeConditionalArguments
	var argumentFragments []ProbeArgumentFragments
	var candidateBase, candidateConditional []int
	for argumentIndex, argument := range arguments {
		baseOffset := len(base)
		conditionalOffset := len(conditional)
		loweredBase, loweredConditional, loweredFragments, err := lowerer.arguments([]string{argument})
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for index := range loweredConditional {
			loweredConditional[index].Before += baseOffset
		}
		for index := range loweredFragments {
			loweredFragments[index].Index += baseOffset
		}
		base = append(base, loweredBase...)
		conditional = append(conditional, loweredConditional...)
		argumentFragments = append(argumentFragments, loweredFragments...)
		if candidateArguments == nil || !candidateArguments[argumentIndex] {
			continue
		}
		for index := baseOffset; index < len(base); index++ {
			candidateBase = append(candidateBase, index)
		}
		for index := conditionalOffset; index < len(conditional); index++ {
			candidateConditional = append(candidateConditional, index)
		}
	}

	var candidate *ProbeCandidateArguments
	if len(candidateBase) != 0 || len(candidateConditional) != 0 {
		candidate = &ProbeCandidateArguments{
			Policy: policy, Base: candidateBase, Conditional: candidateConditional,
		}
	}
	return base, conditional, argumentFragments, candidate, nil
}

func probeCandidateArgumentMask(count int) []bool {
	mask := make([]bool, count)
	for index := range mask {
		mask[index] = true
	}
	return mask
}

func (e *LinuxProbeEvaluator) lowerSymbolicCandidateArguments(
	arguments []string,
	candidateArguments []bool,
	policy string,
) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, *ProbeCandidateArguments, []ProbeReference, error) {
	lowerer := newProbeSymbolicValueLowerer(e)
	base, conditional, argumentFragments, candidate, err := lowerProbeCandidateArguments(
		lowerer, arguments, candidateArguments, policy,
	)
	return base, conditional, argumentFragments, candidate, slices.Clone(lowerer.dependencies), err
}

func (e *LinuxProbeEvaluator) lowerSymbolicProcessInputs(
	arguments []string,
	stdin string,
) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, string, []ProbeValueFragment, []ProbeReference, error) {
	lowerer := newProbeSymbolicValueLowerer(e)
	base, conditional, argumentFragments, _, err := lowerProbeCandidateArguments(lowerer, arguments, nil, "")
	if err != nil {
		return nil, nil, nil, "", nil, nil, err
	}
	fragments, symbolic, err := lowerer.value(stdin)
	if err != nil {
		return nil, nil, nil, "", nil, nil, err
	}
	if symbolic {
		stdin = ""
	}
	return base, conditional, argumentFragments, stdin, fragments, slices.Clone(lowerer.dependencies), nil
}

func (e *LinuxProbeEvaluator) lowerSymbolicCandidateProcessInputs(
	arguments []string,
	candidateArguments []bool,
	policy string,
	stdin string,
) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, *ProbeCandidateArguments, string, []ProbeValueFragment, []ProbeReference, error) {
	lowerer := newProbeSymbolicValueLowerer(e)
	base, conditional, argumentFragments, candidate, err := lowerProbeCandidateArguments(
		lowerer, arguments, candidateArguments, policy,
	)
	if err != nil {
		return nil, nil, nil, nil, "", nil, nil, err
	}
	fragments, symbolic, err := lowerer.value(stdin)
	if err != nil {
		return nil, nil, nil, nil, "", nil, nil, err
	}
	if symbolic {
		stdin = ""
	}
	return base, conditional, argumentFragments, candidate, stdin, fragments, slices.Clone(lowerer.dependencies), nil
}

func probeSelectionStatePredicate(inputs []linuxProbeSelectionInput, state int, ordinals map[ProbeReference]int) ProbePredicate {
	operands := make([]ProbePredicate, 0, len(inputs))
	for index, input := range inputs {
		operator := "result-false"
		if state&(1<<index) != 0 {
			operator = "result-true"
		}
		operands = append(operands, ProbePredicate{
			Operator: operator, Result: fmt.Sprintf("%08d", ordinals[input.reference]),
		})
	}
	if len(operands) == 1 {
		return operands[0]
	}
	return ProbePredicate{Operator: "all", Operands: operands}
}

func (e *LinuxProbeEvaluator) symbolArgument(argument string) (linuxProbeSymbol, bool, error) {
	matches := linuxProbeSymbolPattern.FindAllString(argument, -1)
	if len(matches) == 0 {
		return linuxProbeSymbol{}, false, nil
	}
	if len(matches) != 1 || matches[0] != argument {
		return linuxProbeSymbol{}, false, fmt.Errorf("Linux probe symbolic value must occupy one complete argument: %q", argument)
	}
	symbol, ok, err := e.adoptSymbol(argument)
	if err != nil {
		return linuxProbeSymbol{}, false, err
	}
	if !ok {
		return linuxProbeSymbol{}, false, fmt.Errorf("unknown Linux probe symbolic argument %q", argument)
	}
	return symbol, true, nil
}

func (e *LinuxProbeEvaluator) shellTest(expr string) (linuxProbeTruth, error) {
	if value, ok := strings.CutPrefix(expr, "-z "); ok {
		return e.compareSymbolicString(unquoteShell(value), "", true)
	}
	if value, ok := strings.CutPrefix(expr, "-e "); ok {
		return e.symbolicExistingPath(unquoteShell(value))
	}
	fields := strings.Fields(expr)
	if len(fields) != 3 || (fields[1] != "=" && fields[1] != "!=") {
		return linuxProbeTruth{}, e.unhandledCommand("test " + expr)
	}
	right := unquoteShell(fields[2])
	equal := fields[1] == "="
	return e.compareSymbolicString(unquoteShell(fields[0]), right, equal)
}

func (e *LinuxProbeEvaluator) compareSymbolicString(value, expected string, equal bool) (linuxProbeTruth, error) {
	matches := linuxProbeSymbolPattern.FindAllString(value, -1)
	if len(matches) == 0 {
		return knownLinuxProbeTruth((value == expected) == equal), nil
	}
	if len(matches) != 1 || matches[0] != value {
		return linuxProbeTruth{}, fmt.Errorf("Linux probe symbolic comparison requires one complete value: %q", value)
	}
	symbol, ok, err := e.adoptSymbol(value)
	if err != nil {
		return linuxProbeTruth{}, err
	}
	if !ok {
		return linuxProbeTruth{}, fmt.Errorf("unknown Linux probe symbolic value %q", value)
	}
	if symbol.kind == "selection" {
		return linuxProbeTruth{}, fmt.Errorf("Linux probe symbolic selection cannot be compared by an unmodeled shell expression: %q", value)
	}
	if symbol.kind == "transformed-text" {
		return linuxProbeTruth{}, fmt.Errorf("Linux transformed text probe value cannot be compared by an unmodeled shell expression: %q", value)
	}
	if symbol.kind == "make-text" {
		return linuxProbeTruth{}, fmt.Errorf("Linux whole Make text value cannot be compared by an unmodeled shell expression: %q", value)
	}
	if symbol.kind == "text" {
		predicate := ProbePredicate{Operator: "result-text-equals", Result: "00000000", Value: expected}
		if expected == "" {
			predicate.Operator, predicate.Value = "result-text-empty", ""
		}
		truth, err := e.requestTruth(ProbeRequest{
			Schema: LinuxProbeRequestSchema, InputCount: 1,
			Outcome: ProbeOutcome{Kind: "boolean", Predicate: &predicate},
		}, symbol.reference)
		if !equal {
			truth.trueWhenResult = false
		}
		return truth, err
	}
	whenTrue := (symbol.trueText == expected) == equal
	whenFalse := (symbol.falseText == expected) == equal
	if whenTrue == whenFalse {
		return knownLinuxProbeTruth(whenTrue), nil
	}
	return linuxProbeTruth{
		reference: symbol.reference, request: symbol.request,
		dependencies:   slices.Clone(symbol.dependencies),
		trueWhenResult: whenTrue,
	}, nil
}

func (e *LinuxProbeEvaluator) symbolicExistingPath(path string) (linuxProbeTruth, error) {
	matches := linuxProbeSymbolPattern.FindAllString(path, -1)
	if len(matches) != 1 || !strings.HasPrefix(path, matches[0]+"/") {
		return linuxProbeTruth{}, fmt.Errorf("unsupported Linux Kconfig test path %q", path)
	}
	token := matches[0]
	symbol, ok, err := e.adoptSymbol(token)
	if err != nil {
		return linuxProbeTruth{}, err
	}
	if !ok || symbol.kind != "text" {
		return linuxProbeTruth{}, fmt.Errorf("Linux Kconfig file test does not use a measured text result: %q", path)
	}
	suffix := strings.TrimPrefix(path, token+"/")
	if !safeLinuxProbeRelativePath(suffix) {
		return linuxProbeTruth{}, fmt.Errorf("Linux compiler path probe suffix %q is not canonical", suffix)
	}
	exists := ProbePredicate{
		Operator: "execroot-exists", Value: "${result:00000000.text}/" + suffix,
	}
	predicate := exists
	if len(symbol.request.Steps) == 1 && symbol.request.Steps[0].StdoutFallbackPath != "" {
		predicate = ProbePredicate{Operator: "all", Operands: []ProbePredicate{
			{Operator: "not", Operands: []ProbePredicate{{
				Operator: "result-text-equals", Result: "00000000", Value: symbol.request.Steps[0].StdoutFallbackPath,
			}}},
			exists,
		}}
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &predicate},
	}
	return e.requestTruth(request, symbol.reference)
}

func safeLinuxProbeRelativePath(value string) bool {
	if len(value) > maxProbeSourcePathBytes || ValidateProbeExecrootRelativePath(value) != nil {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if !safeLinuxProbePathComponent(component) {
			return false
		}
	}
	return true
}

func (e *LinuxProbeEvaluator) commandExists(command string) (bool, error) {
	value, recognized, err := e.commandLookup(command)
	if err != nil {
		return false, err
	}
	if !recognized {
		return false, e.unhandledCommand(command)
	}
	return value != "", nil
}

// commandLookup models command -v against the explicitly configured tool
// universe. scripts/Makefile.compiler's cc-cross-prefix uses the `--` form and
// only consumes whether the output is empty; an unselected prefix is therefore
// deterministically absent rather than looked up in the planner's PATH.
func (e *LinuxProbeEvaluator) commandLookup(command string) (string, bool, error) {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[0] != "command" || fields[1] != "-v" {
		return "", false, nil
	}
	if fields[len(fields)-1] == "2>/dev/null" {
		fields = fields[:len(fields)-1]
	}
	allowUnselected := false
	valueIndex := 2
	if len(fields) > valueIndex && fields[valueIndex] == "--" {
		allowUnselected = true
		valueIndex++
	}
	if len(fields) != valueIndex+1 {
		return "", true, e.unsupportedCommand(command)
	}
	value := strings.Trim(fields[valueIndex], `"'`)
	if value == "" || strings.ContainsAny(value, "\x00\r\n;&|<>()`$") {
		return "", true, e.unsupportedCommand(command)
	}
	if ref, ok := parseKbuildActionRoleToken(value); ok {
		ref, matches := kbuildActionRoleRefForScope(ref, e.scope)
		if !matches || e.tools[ref.Role] == "" {
			return "", true, e.unsupportedCommand(command)
		}
		return value, true, nil
	}
	if strings.HasPrefix(value, kbuildActionRoleTokenPrefix) {
		return "", true, e.unsupportedCommand(command)
	}
	for _, selected := range e.tools {
		if value == selected {
			return selected, true, nil
		}
	}
	// PATH is intentionally not part of the planner. Any safe bare command
	// which is absent from the selected manifest is therefore known absent,
	// including cross-prefix candidates queried with `command -v --`.
	if !strings.ContainsAny(value, `/\`) && safeLinuxProbePathComponent(value) {
		return "", true, nil
	}
	if allowUnselected {
		return "", true, nil
	}
	return "", true, e.unhandledCommand(command)
}

func (e *LinuxProbeEvaluator) isToolToken(field, role string) bool {
	// Kconfig $(shell,...) commands execute with the exact source-derived
	// environment.  Most upstream probes use $(CC), which Kconfig expands
	// before calling Shell, but a command may deliberately leave $NAME or
	// ${NAME} for the shell.  Resolve only a complete shell parameter token,
	// and only through that selected environment; never expand arbitrary shell
	// text or infer a tool from the variable name.
	if name, ok := linuxProbeShellEnvironmentToken(field); ok {
		value, exists := e.scriptEnvironment[name]
		if !exists {
			return false
		}
		field = value
	}
	field, static := linuxProbeStaticShellWord(field)
	if !static {
		return false
	}
	// Final recipe lowering can replay the same source-defined probe with an
	// inert role token to retain configured-tool provenance. The probe request
	// is keyed by the role contract, not by the executable spelling, so this is
	// equivalent to the configured path and keeps the replay generic.
	if field == KbuildActionRoleToken(e.scope, role) || field == KbuildActionRoleToken(KbuildActionRoleAutoScope, role) {
		return true
	}
	return field == e.tools[role]
}

// linuxProbeStaticShellWord removes at most one exact matching quote pair from
// a word which has no shell expansion or concatenation syntax. It deliberately
// rejects the permissive behavior of strings.Trim: an unmatched or mismatched
// quote does not name the same executable as a valid selected path.
func linuxProbeStaticShellWord(field string) (string, bool) {
	if field == "" {
		return "", false
	}
	if field[0] == '\'' || field[0] == '"' {
		quote := field[0]
		if len(field) < 2 || field[len(field)-1] != quote {
			return "", false
		}
		field = field[1 : len(field)-1]
		if strings.ContainsRune(field, rune(quote)) || quote == '"' && strings.ContainsAny(field, "\\$`") {
			return "", false
		}
		return field, field != ""
	}
	if strings.ContainsAny(field, "\\\"'$`") {
		return "", false
	}
	return field, true
}

// linuxProbeShellEnvironmentToken recognizes the shell forms which consist of
// exactly one environment expansion. Double quotes preserve expansion; single
// quotes and every composite/modifier form remain literal and therefore cannot
// select a configured tool.
func linuxProbeShellEnvironmentToken(field string) (string, bool) {
	if len(field) >= 2 && field[0] == '"' && field[len(field)-1] == '"' {
		field = field[1 : len(field)-1]
	} else if strings.ContainsAny(field, `"'`) {
		return "", false
	}
	if strings.HasPrefix(field, "${") && strings.HasSuffix(field, "}") {
		name := field[2 : len(field)-1]
		return name, validKbuildCommandEnvironmentName(name)
	}
	if strings.HasPrefix(field, "$") {
		name := field[1:]
		return name, validKbuildCommandEnvironmentName(name)
	}
	return "", false
}

func (e *LinuxProbeEvaluator) compilerVersionCommand(command string) (firstLine, localeC, recognized bool) {
	fields := strings.Fields(command)
	if len(fields) != 0 && fields[0] == "LC_ALL=C" {
		localeC = true
		fields = fields[1:]
	}
	if len(fields) < 2 || !e.isToolToken(fields[0], "cc") || fields[1] != "--version" {
		return false, false, false
	}
	fields = fields[2:]
	if len(fields) != 0 && fields[0] == "2>/dev/null" {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false, localeC, true
	}
	if len(fields) == 3 && fields[0] == "|" && fields[1] == "head" && fields[2] == "-n1" {
		return true, localeC, true
	}
	if len(fields) == 4 && fields[0] == "|" && fields[1] == "head" && fields[2] == "-n" && fields[3] == "1" {
		return true, localeC, true
	}
	return false, false, false
}

func (e *LinuxProbeEvaluator) unsupportedCommand(command string) error {
	return &LinuxProbeOwnedUnsupportedCommandError{Architecture: e.architecture, Command: command}
}

func (e *LinuxProbeEvaluator) unhandledCommand(command string) error {
	if e.ownsProbeCommand(command) {
		return e.unsupportedCommand(command)
	}
	return &LinuxProbeUnsupportedCommandError{Architecture: e.architecture, Command: command}
}

func (e *LinuxProbeEvaluator) ownsProbeCommand(command string) bool {
	if e.looksLikeSourceScript(command) {
		return true
	}
	for _, tool := range e.tools {
		if tool != "" && strings.Contains(command, tool) {
			return true
		}
	}
	if strings.Contains(command, kbuildActionRoleTokenPrefix) || linuxProbeUnresolvedVariable.MatchString(command) {
		return true
	}
	if strings.Contains(command, linuxProbeSymbolPrefix) {
		return true
	}
	fields := strings.Fields(command)
	if len(fields) >= 2 && strings.ContainsAny(strings.Trim(fields[0], `"'`), `/\`) && fields[1] == "--version" {
		return true
	}
	return false
}
