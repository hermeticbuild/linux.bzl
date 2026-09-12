// proberun executes one content-addressed compiler probe request. It is an
// argv-only execution primitive: the request supplies processes and generic
// reductions, while Bazel supplies exact configured action/tool contracts.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

const (
	defaultProbeTimeout = 20 * time.Second
	probeOutputLimit    = 64 << 10
)

var prohibitedProbeEnvironmentNames = map[string]bool{
	// Process-loader and language-runtime hooks can execute request-owned code
	// before the identity-bound tool reaches main.
	"BASH_ENV":       true,
	"ENV":            true,
	"GCONV_PATH":     true,
	"GLIBC_TUNABLES": true,
	"NODE_OPTIONS":   true,
	"NODE_PATH":      true,
	"PERL5LIB":       true,
	"PERL5OPT":       true,
	"PERLLIB":        true,
	"PYTHONHOME":     true,
	"PYTHONPATH":     true,
	"RUBYLIB":        true,
	"RUBYOPT":        true,

	// Compiler, linker, and helper discovery must remain part of the configured
	// action contract or the private identity-bound runtime-tool directory.
	"CCC_ADD_ARGS":                 true,
	"CCC_OVERRIDE_OPTIONS":         true,
	"CLANG_CONFIG_FILE_SYSTEM_DIR": true,
	"CLANG_CONFIG_FILE_USER_DIR":   true,
	"COLLECT_AS_OPTIONS":           true,
	"COLLECT_GCC":                  true,
	"COLLECT_GCC_OPTIONS":          true,
	"COLLECT_LD":                   true,
	"COLLECT_LTO_WRAPPER":          true,
	"COMPILER_PATH":                true,
	"CPATH":                        true,
	"CPLUS_INCLUDE_PATH":           true,
	"C_INCLUDE_PATH":               true,
	"GCC_COMPARE_DEBUG_EXEC":       true,
	"GCC_EXEC_PREFIX":              true,
	"HOME":                         true,
	"LIBRARY_PATH":                 true,
	"OBJC_INCLUDE_PATH":            true,
	"PATH":                         true,
	"PKG_CONFIG_LIBDIR":            true,
	"PKG_CONFIG_PATH":              true,
	"PKG_CONFIG_SYSROOT_DIR":       true,
	"RUSTC_WORKSPACE_WRAPPER":      true,
	"RUSTC_WRAPPER":                true,
	"RUSTDOCFLAGS":                 true,
	"RUSTFLAGS":                    true,
	"XDG_CONFIG_HOME":              true,
	"XDG_DATA_DIRS":                true,
	"XDG_DATA_HOME":                true,
}

var placeholderPattern = regexp.MustCompile(`\$\{(scratch|source|source_root|tool|result):([^}]+)\}`)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return fmt.Sprint([]string(*f)) }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type actionContract struct {
	path        string
	arguments   []string
	environment map[string]string
}

type probeOptions struct {
	request, result, nodeID, requestID, scope string
	toolsetManifest                           string
	toolsetAnchors                            map[string]string
	toolsetMarkers                            map[string]string
	tools                                     map[string]actionContract
	runtimeTools                              map[string]string
	inputs                                    map[string]string
	sources                                   map[string]string
	sourceRoots                               map[string]string
	sourceRootTrees                           map[string]string
	sourceRootAnchors                         map[string]string
	sourceRootWitnesses                       map[string]string
	tempDir                                   string
	timeout                                   time.Duration
	outputLimit                               int
}

func resolveProbeArtifactPaths(opts probeOptions) (probeOptions, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return probeOptions{}, fmt.Errorf("resolve original working directory: %w", err)
	}
	absolute := func(value string) string {
		if value == "" {
			return ""
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Join(workingDirectory, value)
	}
	opts.request = absolute(opts.request)
	opts.result = absolute(opts.result)
	opts.toolsetManifest = absolute(opts.toolsetManifest)
	toolsetAnchors := make(map[string]string, len(opts.toolsetAnchors))
	for root, anchor := range opts.toolsetAnchors {
		toolsetAnchors[root] = absolute(anchor)
	}
	opts.toolsetAnchors = toolsetAnchors
	if opts.tempDir != "" {
		opts.tempDir = absolute(opts.tempDir)
	}
	markers := make(map[string]string, len(opts.toolsetMarkers))
	for scope, marker := range opts.toolsetMarkers {
		markers[scope] = absolute(marker)
	}
	opts.toolsetMarkers = markers
	inputs := make(map[string]string, len(opts.inputs))
	for ordinal, input := range opts.inputs {
		inputs[ordinal] = absolute(input)
	}
	opts.inputs = inputs
	sources := make(map[string]string, len(opts.sources))
	for source, artifact := range opts.sources {
		sources[source] = absolute(artifact)
	}
	opts.sources = sources
	rootAnchors := make(map[string]string, len(opts.sourceRootAnchors))
	for name, anchor := range opts.sourceRootAnchors {
		rootAnchors[name] = absolute(anchor)
	}
	opts.sourceRootAnchors = rootAnchors
	rootWitnesses := make(map[string]string, len(opts.sourceRootWitnesses))
	for name, witness := range opts.sourceRootWitnesses {
		rootWitnesses[name] = absolute(witness)
	}
	opts.sourceRootWitnesses = rootWitnesses
	roots := make(map[string]string, len(opts.sourceRoots))
	for name, root := range opts.sourceRoots {
		if err := kconfig.ValidateProbeExecrootRelativePath(root); err != nil {
			return probeOptions{}, fmt.Errorf("source root %q: %w", name, err)
		}
		roots[name] = absolute(filepath.FromSlash(root))
	}
	opts.sourceRoots = roots
	treeRoots := make(map[string]string, len(opts.sourceRootTrees))
	for name, root := range opts.sourceRootTrees {
		if err := kconfig.ValidateProbeExecrootRelativePath(root); err != nil {
			return probeOptions{}, fmt.Errorf("source root tree %q: %w", name, err)
		}
		treeRoots[name] = absolute(filepath.FromSlash(root))
	}
	opts.sourceRootTrees = treeRoots
	tools := make(map[string]actionContract, len(opts.tools))
	for role, contract := range opts.tools {
		contract.path = absolute(contract.path)
		tools[role] = contract
	}
	opts.tools = tools
	runtimeTools := make(map[string]string, len(opts.runtimeTools))
	for role, tool := range opts.runtimeTools {
		runtimeTools[role] = absolute(tool)
	}
	opts.runtimeTools = runtimeTools
	return opts, nil
}

func expandProbeExecutionRootActionContracts(opts *probeOptions, executionRoot string) error {
	for role, contract := range opts.tools {
		arguments := make([]string, len(contract.arguments))
		for index, value := range contract.arguments {
			expanded, err := toolaction.ExpandExecutionRootValue(value, executionRoot)
			if err != nil {
				return fmt.Errorf("configured %s action argument %d: %w", role, index, err)
			}
			arguments[index] = expanded
		}
		environment := make(map[string]string, len(contract.environment))
		for name, value := range contract.environment {
			expanded, err := toolaction.ExpandExecutionRootValue(value, executionRoot)
			if err != nil {
				return fmt.Errorf("configured %s action environment %s: %w", role, name, err)
			}
			environment[name] = expanded
		}
		contract.arguments = arguments
		contract.environment = environment
		opts.tools[role] = contract
	}
	return nil
}

func exactStringBindings(kind string, want []string, got map[string]string) error {
	if len(want) != len(got) {
		return fmt.Errorf("probe %s bindings = %v, want %v", kind, sortedStringKeys(got), want)
	}
	for _, name := range want {
		if got[name] == "" {
			return fmt.Errorf("probe has no %s binding %q", kind, name)
		}
	}
	return nil
}

func selectProbeStepContractRole(step kconfig.ProbeStep, arguments []string) (string, error) {
	if step.Candidate == nil {
		return toolaction.InvocationContractRole(step.Tool, arguments), nil
	}
	switch step.Candidate.Policy {
	case kconfig.ProbeCandidatePolicyCC:
		if base, ok := toolaction.BaseContractRole(step.Tool); ok {
			return base, nil
		}
		return step.Tool, nil
	case kconfig.ProbeCandidatePolicyCCLink:
		if _, alreadyLink := toolaction.BaseContractRole(step.Tool); alreadyLink {
			return step.Tool, nil
		}
		if link, ok := toolaction.LinkContractRole(step.Tool); ok {
			return link, nil
		}
		return step.Tool, nil
	case kconfig.ProbeCandidatePolicyLD:
		return step.Tool, nil
	default:
		return "", fmt.Errorf("step %s candidate has unsupported contract policy %q", step.Name, step.Candidate.Policy)
	}
}

func resolveProbeWorkingDirectory(
	value, scratchRoot string,
	scratch map[string]string,
	sourceRoots map[string]string,
	expand func(string) (string, error),
) (string, error) {
	if value == "" {
		resolved, err := filepath.EvalSymlinks(scratchRoot)
		if err != nil {
			return "", fmt.Errorf("resolve private scratch working directory: %w", err)
		}
		return filepath.Clean(resolved), nil
	}

	const scratchPrefix = "${scratch:"
	if strings.HasPrefix(value, scratchPrefix) {
		end := strings.IndexByte(value[len(scratchPrefix):], '}')
		if end < 0 {
			return "", fmt.Errorf("working directory has an unterminated scratch placeholder")
		}
		end += len(scratchPrefix)
		name := value[len(scratchPrefix):end]
		if value[end+1:] != "" {
			return "", fmt.Errorf("scratch working directory must be exactly one placeholder")
		}
		working := scratch[name]
		if working == "" {
			return "", fmt.Errorf("working directory references unavailable scratch %q", name)
		}
		rootResolved, err := filepath.EvalSymlinks(scratchRoot)
		if err != nil {
			return "", fmt.Errorf("resolve private scratch root: %w", err)
		}
		workingResolved, err := filepath.EvalSymlinks(working)
		if err != nil {
			return "", fmt.Errorf("resolve scratch working directory %q: %w", name, err)
		}
		rootResolved = filepath.Clean(rootResolved)
		workingResolved = filepath.Clean(workingResolved)
		if !probePhysicalPathWithin(rootResolved, workingResolved) {
			return "", fmt.Errorf("scratch working directory %q resolves outside private scratch root", name)
		}
		info, err := os.Stat(workingResolved)
		if err != nil {
			return "", fmt.Errorf("inspect scratch working directory %q: %w", name, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("scratch working directory %q is not a directory", name)
		}
		return workingResolved, nil
	}

	const prefix = "${source_root:"
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("working directory does not begin with a declared source root")
	}
	end := strings.IndexByte(value[len(prefix):], '}')
	if end < 0 {
		return "", fmt.Errorf("working directory has an unterminated source-root placeholder")
	}
	end += len(prefix)
	name := value[len(prefix):end]
	root := sourceRoots[name]
	if root == "" {
		return "", fmt.Errorf("working directory references unavailable source root %q", name)
	}
	expanded, err := expand(value)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve working-directory source root %q: %w", name, err)
	}
	workingResolved, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", expanded, err)
	}
	rootResolved = filepath.Clean(rootResolved)
	workingResolved = filepath.Clean(workingResolved)
	if !probePhysicalPathWithin(rootResolved, workingResolved) {
		return "", fmt.Errorf("working directory %q resolves outside declared source root %q", expanded, name)
	}
	info, err := os.Stat(workingResolved)
	if err != nil {
		return "", fmt.Errorf("inspect working directory %q: %w", workingResolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory %q is not a directory", workingResolved)
	}
	return workingResolved, nil
}

func validateProbeRequestEnvironmentName(name string, contractEnvironment map[string]string) error {
	if _, owned := contractEnvironment[name]; owned {
		return fmt.Errorf("environment variable %s would override the identity-bound action contract", name)
	}
	if name == "LANG" || name == "LC_ALL" || probeRunnerOwnsEnvironmentName(name) {
		return fmt.Errorf("environment variable %s is runner-owned", name)
	}
	if prohibitedProbeEnvironmentNames[name] || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return fmt.Errorf("environment variable %s controls process loading or tool/helper search", name)
	}
	return nil
}

func probeRunnerOwnsEnvironmentName(name string) bool {
	return name == toolaction.EnvironmentName ||
		name == toolaction.RuntimeToolPathEnvironmentName ||
		name == toolsetpath.HandoffEnvironmentName
}

func applyProbeRequestEnvironmentValue(environment, contractEnvironment map[string]string, name, value string) error {
	if name == "LANG" || name == "LC_ALL" {
		if _, owned := contractEnvironment[name]; owned {
			return fmt.Errorf("environment variable %s would override the identity-bound action contract", name)
		}
		if value != "C" {
			return fmt.Errorf("environment variable %s is runner-owned and may only repeat value C", name)
		}
		// The runner already supplies this exact deterministic locale. Preserve
		// the request's semantics without giving it authority over final argv/env.
		return nil
	}
	if err := validateProbeRequestEnvironmentName(name, contractEnvironment); err != nil {
		return err
	}
	environment[name] = value
	return nil
}

func resolveProbeSourceRoots(want []string, sources, directories, trees, anchors, witnesses map[string]string) (map[string]string, error) {
	bindings := make(map[string]string, len(directories)+len(trees)+len(anchors))
	for name, directory := range directories {
		bindings[name] = directory
	}
	for name, tree := range trees {
		if _, exists := bindings[name]; exists {
			return nil, fmt.Errorf("probe source root %q has both directory and tree bindings", name)
		}
		bindings[name] = tree
	}
	for name, anchor := range anchors {
		if _, exists := bindings[name]; exists {
			return nil, fmt.Errorf("probe source root %q has both directory and anchor bindings", name)
		}
		bindings[name] = anchor
	}
	if err := exactStringBindings("source root", want, bindings); err != nil {
		return nil, err
	}
	for name := range witnesses {
		if directories[name] == "" {
			return nil, fmt.Errorf("probe source root witness %q has no directory binding", name)
		}
	}
	resolved := make(map[string]string, len(want))
	for _, name := range want {
		root := directories[name]
		_, treeBinding := trees[name]
		if treeBinding {
			root = trees[name]
		}
		if anchor := anchors[name]; anchor != "" {
			info, err := os.Stat(anchor)
			if err != nil {
				return nil, fmt.Errorf("inspect probe source root anchor %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("probe source root anchor %q is not a regular file", name)
			}
			declared := false
			for _, source := range sources {
				if filepath.Clean(source) == filepath.Clean(anchor) {
					declared = true
					break
				}
			}
			if !declared {
				return nil, fmt.Errorf("probe source root anchor %q is not a declared source artifact", name)
			}
			root = filepath.Dir(anchor)
		} else {
			info, err := os.Stat(root)
			if err != nil {
				return nil, fmt.Errorf("inspect probe source root %q: %w", name, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("probe source root %q is not a directory", name)
			}
		}
		containsSource := treeBinding
		for _, source := range sources {
			relative, err := filepath.Rel(root, source)
			if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
				containsSource = true
				break
			}
		}
		if witness := witnesses[name]; witness != "" {
			info, err := os.Stat(witness)
			if err != nil {
				return nil, fmt.Errorf("inspect probe source root witness %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("probe source root witness %q is not a regular file", name)
			}
			relative, err := filepath.Rel(root, witness)
			if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
				return nil, fmt.Errorf("probe source root witness %q is outside its declared root", name)
			}
			containsSource = true
		}
		if !containsSource {
			return nil, fmt.Errorf("probe source root %q contains no declared source artifact", name)
		}
		resolved[name] = filepath.Clean(root)
	}
	return resolved, nil
}

func runProbe(opts probeOptions) error {
	for name, value := range map[string]string{
		"request": opts.request, "result": opts.result, "node ID": opts.nodeID,
		"request ID": opts.requestID, "scope": opts.scope,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	// Bazel action inputs are commonly execroot-relative. Every process below
	// runs in a private scratch directory, so bind artifact paths to the
	// original execroot before setting exec.Cmd.Dir. This also keeps ${tool:*}
	// expansions and the post-execution result output independent of cwd.
	execroot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve probe execroot: %w", err)
	}
	opts, err = resolveProbeArtifactPaths(opts)
	if err != nil {
		return err
	}
	if err := expandProbeExecutionRootActionContracts(&opts, execroot); err != nil {
		return err
	}
	request, err := kconfig.ReadProbeRequest(opts.request)
	if err != nil {
		return err
	}
	actualID, _ := request.ID()
	if actualID != opts.requestID {
		return fmt.Errorf("probe request content ID = %s, want %s", actualID, opts.requestID)
	}
	if opts.scope != "target" && opts.scope != "host" {
		return fmt.Errorf("probe scope %q is invalid", opts.scope)
	}
	if err := exactStringBindings("source", request.Sources, opts.sources); err != nil {
		return err
	}
	for _, source := range request.Sources {
		info, err := os.Stat(opts.sources[source])
		if err != nil {
			return fmt.Errorf("inspect probe source %q: %w", source, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("probe source %q is not a regular file", source)
		}
	}
	resolvedSourceRoots, err := resolveProbeSourceRoots(request.SourceRoots, opts.sources, opts.sourceRoots, opts.sourceRootTrees, opts.sourceRootAnchors, opts.sourceRootWitnesses)
	if err != nil {
		return err
	}
	toolsetIdentities := map[string]string{}
	for scope, marker := range opts.toolsetMarkers {
		if scope != "target" && scope != "host" {
			return fmt.Errorf("toolset marker has invalid scope %q", scope)
		}
		identity := filepath.Base(marker)
		if !validIdentity(identity) {
			return fmt.Errorf("toolset marker %q has invalid identity name", marker)
		}
		if info, err := os.Stat(marker); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("inspect %s toolset marker: %w", scope, err)
		}
		toolsetIdentities[scope] = identity
	}
	toolsetIdentity := toolsetIdentities[opts.scope]
	if toolsetIdentity == "" {
		return fmt.Errorf("probe has no %s toolset marker", opts.scope)
	}
	toolsetPaths, err := loadToolsetPathResolver(
		execroot,
		opts.scope,
		toolsetIdentity,
		opts.toolsetManifest,
		opts.toolsetAnchors,
	)
	if err != nil {
		return err
	}
	wantRuntimeRoles := make([]string, 0, len(toolsetPaths.manifest.Tools))
	for role := range toolsetPaths.manifest.Tools {
		wantRuntimeRoles = append(wantRuntimeRoles, role)
	}
	sort.Strings(wantRuntimeRoles)
	if err := exactStringBindings("runtime tool", wantRuntimeRoles, opts.runtimeTools); err != nil {
		return err
	}
	for role, filename := range opts.runtimeTools {
		if err := toolsetPaths.verifyTool(role, filename); err != nil {
			return err
		}
	}
	for role, contract := range opts.tools {
		if err := toolsetPaths.verifyTool(role, contract.path); err != nil {
			return err
		}
		if err := toolsetPaths.verifyContract(role, contract); err != nil {
			return err
		}
	}
	wantRoles := request.ToolRoles()
	if err := exactRoles(wantRoles, opts.tools); err != nil {
		return err
	}
	wantInputs := make([]string, request.InputCount)
	inputResults := make(map[string]kconfig.ProbeResult, request.InputCount)
	inputNodeIDs := make([]string, request.InputCount)
	for index := 0; index < request.InputCount; index++ {
		name := fmt.Sprintf("%08d", index)
		wantInputs[index] = name
		filename := opts.inputs[name]
		if filename == "" {
			return fmt.Errorf("probe has no result input %s", name)
		}
		result, err := kconfig.ReadProbeResult(filename)
		if err != nil {
			return fmt.Errorf("read probe result input %s: %w", name, err)
		}
		if want := toolsetIdentities[result.Scope]; want == "" || result.ToolsetIdentity != want {
			return fmt.Errorf("probe result input %s has unavailable %s toolset %s", name, result.Scope, result.ToolsetIdentity)
		}
		if opts.scope == "host" && result.Scope != "host" {
			return fmt.Errorf("host probe cannot depend on %s result input %s", result.Scope, name)
		}
		inputResults[name] = *result
		inputNodeIDs[index] = result.NodeID
	}
	if len(opts.inputs) != len(wantInputs) {
		return fmt.Errorf("probe result inputs = %v, want %v", sortedStringKeys(opts.inputs), wantInputs)
	}
	node := kconfig.ProbePlanNode{Scope: opts.scope, RequestID: opts.requestID, Inputs: inputNodeIDs}
	if actualNodeID := node.ContentID(); actualNodeID != opts.nodeID {
		return fmt.Errorf("probe node content ID = %s, want %s", actualNodeID, opts.nodeID)
	}
	if opts.timeout <= 0 {
		opts.timeout = defaultProbeTimeout
	}
	if opts.outputLimit <= 0 {
		opts.outputLimit = probeOutputLimit
	}
	root, err := os.MkdirTemp(opts.tempDir, "linux-bzl-probe-")
	if err != nil {
		return fmt.Errorf("create probe scratch directory: %w", err)
	}
	defer os.RemoveAll(root)
	if err := toolsetPaths.setProjectionRoot(filepath.Join(root, "toolset")); err != nil {
		return fmt.Errorf("prepare probe toolset projection: %w", err)
	}
	runtimeToolDirectory, cleanupRuntimeTools, err := toolaction.PrepareRuntimeToolDirectory(root, opts.runtimeTools)
	if err != nil {
		return err
	}
	defer cleanupRuntimeTools()
	scratch := map[string]string{}
	for _, item := range request.Scratch {
		filename := filepath.Join(root, item.Name)
		scratch[item.Name] = filename
		if item.Kind == "directory" {
			if err := os.Mkdir(filename, 0o700); err != nil {
				return fmt.Errorf("create scratch directory %s: %w", item.Name, err)
			}
		} else if item.Present || item.Content != "" {
			if err := os.WriteFile(filename, []byte(item.Content), 0o600); err != nil {
				return fmt.Errorf("write scratch file %s: %w", item.Name, err)
			}
		}
	}
	contractEnvironmentNames := map[string]string{}
	for _, configured := range opts.tools {
		for name := range configured.environment {
			contractEnvironmentNames[name] = ""
		}
	}
	results := make([]kconfig.ProbeStepResult, 0, len(request.Steps))
	for _, step := range request.Steps {
		if step.When != nil {
			run, err := evaluatePredicate(*step.When, results, scratch, opts.sources, resolvedSourceRoots, inputResults, execroot, toolsetPaths)
			if err != nil {
				return fmt.Errorf("evaluate step %s condition: %w", step.Name, err)
			}
			if !run {
				results = append(results, kconfig.ProbeStepResult{Name: step.Name, Status: "skipped", ExitCode: -1})
				continue
			}
		}
		executableContract := opts.tools[step.Tool]
		expand := func(value string) (string, error) {
			return expandProbeValue(value, scratch, opts.sources, resolvedSourceRoots, opts.tools, inputResults)
		}
		renderFragments := func(owner string, fragments []kconfig.ProbeValueFragment, depth int) (string, error) {
			return renderProbeValueFragments(
				fmt.Sprintf("step %s %s", step.Name, owner), fragments, depth,
				results, scratch, opts.sources, resolvedSourceRoots, opts.tools, inputResults, execroot, toolsetPaths,
			)
		}
		arguments := []string{}
		candidateArguments := []string{}
		candidateArgumentIndexes := []int{}
		candidateBase := map[int]bool{}
		candidateConditional := map[int]bool{}
		if step.Candidate != nil {
			for _, index := range step.Candidate.Base {
				candidateBase[index] = true
			}
			for _, index := range step.Candidate.Conditional {
				candidateConditional[index] = true
			}
		}
		appendArgument := func(value string, candidate bool) {
			argumentIndex := len(arguments)
			arguments = append(arguments, value)
			if candidate {
				candidateArguments = append(candidateArguments, value)
				candidateArgumentIndexes = append(candidateArgumentIndexes, argumentIndex)
			}
		}
		groupIndex := 0
		argumentFragmentIndex := 0
		dynamicArgumentBytes := 0
		dynamicArgumentWords := 0
		for index := 0; index <= len(step.Arguments); index++ {
			for groupIndex < len(step.ConditionalArguments) && step.ConditionalArguments[groupIndex].Before == index {
				group := step.ConditionalArguments[groupIndex]
				include, err := evaluatePredicate(group.When, results, scratch, opts.sources, resolvedSourceRoots, inputResults, execroot, toolsetPaths)
				if err != nil {
					return fmt.Errorf("evaluate step %s conditional arguments %d: %w", step.Name, groupIndex, err)
				}
				if include {
					for conditionalIndex, argument := range group.Arguments {
						expanded, err := expand(argument)
						if err != nil {
							return fmt.Errorf("expand step %s conditional argument %d:%d: %w", step.Name, groupIndex, conditionalIndex, err)
						}
						appendArgument(expanded, candidateConditional[groupIndex])
					}
				}
				groupIndex++
			}
			if index < len(step.Arguments) {
				if argumentFragmentIndex < len(step.ArgumentFragments) && step.ArgumentFragments[argumentFragmentIndex].Index == index {
					group := step.ArgumentFragments[argumentFragmentIndex]
					var resultGuard func(string) error
					if group.Mode == kconfig.ProbeArgumentFragmentsModeSourceShellWords {
						resultGuard = validateProbeSourceShellResult
					}
					rendered, err := renderProbeValueFragmentsWithResultGuard(
						fmt.Sprintf("step %s argument fragments %d", step.Name, argumentFragmentIndex), group.Fragments, 0,
						results, scratch, opts.sources, resolvedSourceRoots, opts.tools, inputResults, execroot, toolsetPaths, resultGuard,
					)
					if err != nil {
						return err
					}
					dynamicArgumentBytes += len(rendered)
					if dynamicArgumentBytes > kconfig.MaxProbeInterpolatedBytes {
						return fmt.Errorf("step %s dynamic arguments exceed %d rendered bytes", step.Name, kconfig.MaxProbeInterpolatedBytes)
					}
					words := []string{}
					switch group.Mode {
					case "":
						words = strings.Fields(rendered)
					case kconfig.ProbeArgumentFragmentsModeSignedDecimal:
						if err := kconfig.ValidateProbeSignedDecimalArgument(rendered); err != nil {
							return fmt.Errorf("step %s argument fragment group %d: %w", step.Name, argumentFragmentIndex, err)
						}
						words = append(words, rendered)
					case kconfig.ProbeArgumentFragmentsModeSourceShellWords:
						words, err = kconfig.ParseProbeSourceShellWords(rendered)
						if err != nil {
							return fmt.Errorf("step %s argument fragment group %d: %w", step.Name, argumentFragmentIndex, err)
						}
					default:
						return fmt.Errorf("step %s argument fragment group %d has unsupported mode %q", step.Name, argumentFragmentIndex, group.Mode)
					}
					dynamicArgumentWords += len(words)
					if dynamicArgumentWords > kconfig.MaxProbeDynamicArgumentWords {
						return fmt.Errorf("step %s dynamic arguments exceed %d words", step.Name, kconfig.MaxProbeDynamicArgumentWords)
					}
					for _, word := range words {
						appendArgument(word, candidateBase[index])
					}
					argumentFragmentIndex++
					continue
				}
				expanded, err := expand(step.Arguments[index])
				if err != nil {
					return fmt.Errorf("expand step %s argument %d: %w", step.Name, index, err)
				}
				appendArgument(expanded, candidateBase[index])
			}
		}
		if argumentFragmentIndex != len(step.ArgumentFragments) {
			return fmt.Errorf("step %s did not consume every argument fragment group", step.Name)
		}
		workingDirectory, err := resolveProbeWorkingDirectory(step.WorkingDirectory, root, scratch, resolvedSourceRoots, expand)
		if err != nil {
			return fmt.Errorf("resolve step %s working directory: %w", step.Name, err)
		}
		if step.Candidate != nil {
			translationUnits := make([]string, len(step.Candidate.TranslationUnits))
			for index, translationUnit := range step.Candidate.TranslationUnits {
				translationUnits[index], err = expand(translationUnit)
				if err != nil {
					return fmt.Errorf("expand step %s translation unit %d: %w", step.Name, index, err)
				}
			}
			arguments, candidateArguments, candidateArgumentIndexes, err = projectRenderedProbeCandidateArguments(
				step.Name,
				step.Candidate.Projection,
				arguments,
				candidateArguments,
				candidateArgumentIndexes,
				translationUnits,
			)
			if err != nil {
				return err
			}
			arguments, err = validateAndRewriteProbeStepCandidateArguments(
				step,
				arguments,
				candidateArguments,
				candidateArgumentIndexes,
				root,
				workingDirectory,
				execroot,
				opts.sources,
				resolvedSourceRoots,
				toolsetPaths,
			)
			if err != nil {
				return err
			}
		}
		contractRole, err := selectProbeStepContractRole(step, arguments)
		if err != nil {
			return err
		}
		contract, exists := opts.tools[contractRole]
		if !exists {
			return fmt.Errorf("step %s selects unavailable configured action contract %q", step.Name, contractRole)
		}
		arguments, err = spliceActionArguments(contract.arguments, arguments)
		if err != nil {
			return fmt.Errorf("step %s: %w", step.Name, err)
		}
		stdin := ""
		if len(step.StdinFragments) != 0 {
			stdin, err = renderFragments("stdin", step.StdinFragments, 0)
		} else {
			stdin, err = expand(step.Stdin)
			if err != nil {
				return fmt.Errorf("expand step %s stdin: %w", step.Name, err)
			}
		}
		environment := cloneMap(contract.environment)
		for _, name := range []string{
			toolaction.EnvironmentName,
			toolaction.RuntimeToolPathEnvironmentName,
			toolsetpath.HandoffEnvironmentName,
		} {
			if _, exists := environment[name]; exists {
				return fmt.Errorf("step %s primary configured action environment uses reserved variable %s", step.Name, name)
			}
		}
		if _, configured := environment["LANG"]; !configured {
			environment["LANG"] = "C"
		}
		if _, configured := environment["LC_ALL"]; !configured {
			environment["LC_ALL"] = "C"
		}
		for name, value := range step.Environment {
			expanded, err := expand(value)
			if err != nil {
				return fmt.Errorf("expand step %s environment %s: %w", step.Name, name, err)
			}
			if err := applyProbeRequestEnvironmentValue(environment, contractEnvironmentNames, name, expanded); err != nil {
				return fmt.Errorf("step %s: %w", step.Name, err)
			}
		}
		for _, entry := range step.EnvironmentFragments {
			rendered, err := renderFragments("environment "+entry.Name, entry.Fragments, 0)
			if err != nil {
				return err
			}
			if err := applyProbeRequestEnvironmentValue(environment, contractEnvironmentNames, entry.Name, rendered); err != nil {
				return fmt.Errorf("step %s: %w", step.Name, err)
			}
		}
		if len(step.AuxiliaryTools) != 0 {
			contracts := make(map[string]toolaction.Contract, len(step.AuxiliaryTools)*2)
			for _, role := range step.AuxiliaryTools {
				configured := opts.tools[role]
				contracts[role] = toolaction.Contract{
					Arguments:   append([]string{}, configured.arguments...),
					Environment: cloneMap(configured.environment),
				}
				if linkRole, ok := toolaction.LinkContractRole(role); ok {
					if linkConfigured, exists := opts.tools[linkRole]; exists {
						contracts[linkRole] = toolaction.Contract{
							Arguments: append([]string{}, linkConfigured.arguments...), Environment: cloneMap(linkConfigured.environment),
						}
					}
				}
			}
			encoded, err := toolaction.Encode(contracts)
			if err != nil {
				return fmt.Errorf("step %s auxiliary action contracts: %w", step.Name, err)
			}
			environment[toolaction.EnvironmentName] = encoded
		}
		if runtimeToolDirectory != "" {
			if configuredPath := environment["PATH"]; configuredPath != "" {
				environment["PATH"] = runtimeToolDirectory + string(os.PathListSeparator) + configuredPath
			} else {
				environment["PATH"] = runtimeToolDirectory
			}
			environment[toolaction.RuntimeToolPathEnvironmentName] = runtimeToolDirectory
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
		command := exec.CommandContext(ctx, executableContract.path, arguments...)
		command.Dir = workingDirectory
		command.Env = environmentList(environment)
		command.Stdin = strings.NewReader(stdin)
		stdout, stderr := &limitedBuffer{remaining: opts.outputLimit}, &limitedBuffer{remaining: opts.outputLimit}
		command.Stdout, command.Stderr = stdout, stderr
		if step.DiscardStdout {
			command.Stdout = io.Discard
		}
		if step.DiscardStderr {
			command.Stderr = io.Discard
		}
		runErr := command.Run()
		contextErr := ctx.Err()
		cancel()
		if contextErr != nil {
			return fmt.Errorf("step %s timed out or was cancelled: %w", step.Name, contextErr)
		}
		if stdout.exceeded || stderr.exceeded {
			return fmt.Errorf("step %s output exceeded %d bytes per stream", step.Name, opts.outputLimit)
		}
		status, exitCode := "success", 0
		if runErr != nil {
			var exitError *exec.ExitError
			if !errors.As(runErr, &exitError) {
				return fmt.Errorf("execute step %s: %w", step.Name, runErr)
			}
			status, exitCode = "failure", exitError.ExitCode()
		}
		stdoutValue := stdout.String()
		stdoutPathKind := ""
		if step.StdoutExecrootRelative {
			if status != "success" {
				// A failed path query has no consumable value. Do not persist an
				// arbitrary absolute path it happened to print before failing.
				stdoutValue = ""
			} else {
				stdoutValue, err = kconfig.NormalizeProbeExecrootRelativePath(execroot, executableContract.path, stdoutValue)
				if err != nil {
					return fmt.Errorf("normalize step %s stdout: %w", step.Name, err)
				}
				stdoutValue, err = toolaction.CanonicalArtifactPath(stdoutValue)
				if err != nil {
					return fmt.Errorf("canonicalize step %s stdout: %w", step.Name, err)
				}
				if _, err := toolsetPaths.resolve(stdoutValue); err != nil {
					if step.StdoutFallbackPath != "" && stdoutValue == step.StdoutFallbackPath && errors.Is(err, errToolsetPathOutsideClosure) {
						stdoutValue = step.StdoutFallbackPath
						stdoutPathKind = kconfig.ProbeStdoutPathFallback
					} else {
						return fmt.Errorf("authorize step %s stdout: %w", step.Name, err)
					}
				} else {
					stdoutPathKind = kconfig.ProbeStdoutPathToolset
				}
			}
		}
		results = append(results, kconfig.ProbeStepResult{
			Name: step.Name, Status: status, ExitCode: exitCode,
			Stdout: stdoutValue, StdoutPathKind: stdoutPathKind, Stderr: stderr.String(),
		})
	}
	result := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: opts.nodeID, RequestID: opts.requestID,
		Scope: opts.scope, ToolsetIdentity: toolsetIdentity, Kind: request.Outcome.Kind, Steps: results,
	}
	if request.Outcome.Kind == "boolean" {
		value, err := evaluatePredicate(*request.Outcome.Predicate, results, scratch, opts.sources, resolvedSourceRoots, inputResults, execroot, toolsetPaths)
		if err != nil {
			return fmt.Errorf("evaluate probe outcome: %w", err)
		}
		result.Boolean = &value
	} else if len(request.Outcome.Fragments) != 0 {
		result.Text, err = kconfig.RenderProbeDependencyFragments(request.Outcome.Fragments, inputResults)
		if err != nil {
			return err
		}
	} else if request.Outcome.Result != "" {
		input, ok := inputResults[request.Outcome.Result]
		if !ok || input.Kind != "text" {
			return fmt.Errorf("text outcome result %q is not text", request.Outcome.Result)
		}
		fields := strings.Fields(input.Text)
		if request.Outcome.Word <= len(fields) {
			result.Text = fields[request.Outcome.Word-1]
		}
	} else {
		step, ok := findStep(results, request.Outcome.Step)
		if !ok || step.Status == "skipped" {
			for index := len(results) - 1; index >= 0; index-- {
				prior := results[index]
				if prior.Status == "failure" {
					return fmt.Errorf(
						"text outcome step %q did not run after %q failed with exit code %d: %s",
						request.Outcome.Step, prior.Name, prior.ExitCode, strings.TrimSpace(prior.Stderr),
					)
				}
			}
			return fmt.Errorf("text outcome step %q did not run", request.Outcome.Step)
		}
		if request.Outcome.RequireSuccess && (step.Status != "success" || step.ExitCode != 0) {
			return fmt.Errorf(
				"text outcome step %q failed with exit code %d: %s",
				request.Outcome.Step,
				step.ExitCode,
				strings.TrimSpace(step.Stderr),
			)
		}
		value, err := stepStream(step, request.Outcome.Stream)
		if err != nil {
			return err
		}
		if request.Outcome.GNUMakeShell {
			value = kconfig.NormalizeGNUMakeShellOutput(value)
		}
		if request.Outcome.TrimSpace {
			value = strings.TrimSpace(value)
		}
		if request.Outcome.FirstLine {
			value = strings.SplitN(value, "\n", 2)[0]
		}
		if request.Outcome.LastLine {
			lines := strings.Split(value, "\n")
			value = lines[len(lines)-1]
		}
		if request.Outcome.PathComponent {
			if err := kconfig.ValidateProbePathComponent(value); err != nil {
				return err
			}
		}
		if request.Outcome.SingleMakeWord {
			if err := kconfig.ValidateProbeSingleMakeWord(value); err != nil {
				return err
			}
		}
		result.Text = value
	}
	data, err := result.CanonicalJSON()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.result), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(opts.result, data, 0o644); err != nil {
		return fmt.Errorf("write probe result: %w", err)
	}
	return nil
}

func renderProbeValueFragments(
	owner string,
	fragments []kconfig.ProbeValueFragment,
	depth int,
	results []kconfig.ProbeStepResult,
	scratch, sources, sourceRoots map[string]string,
	tools map[string]actionContract,
	inputs map[string]kconfig.ProbeResult,
	execroot string,
	toolsetPaths *toolsetPathResolver,
) (string, error) {
	return renderProbeValueFragmentsWithResultGuard(owner, fragments, depth,
		results, scratch, sources, sourceRoots, tools, inputs, execroot, toolsetPaths, nil)
}

// A source-shell fragment may contain planner-owned marker literals. Guard
// each measured substitution before concatenation or Make transformations so
// result bytes cannot forge those markers, including by supplying only part.
func validateProbeSourceShellResult(value string) error {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < 0x20 && character != '\t' && character != '\n') || character == 0x7f {
			return fmt.Errorf("source-shell measured result contains a prohibited control byte")
		}
	}
	return nil
}

func renderProbeValueFragmentsWithResultGuard(
	owner string,
	fragments []kconfig.ProbeValueFragment,
	depth int,
	results []kconfig.ProbeStepResult,
	scratch, sources, sourceRoots map[string]string,
	tools map[string]actionContract,
	inputs map[string]kconfig.ProbeResult,
	execroot string,
	toolsetPaths *toolsetPathResolver,
	resultGuard func(string) error,
) (string, error) {
	if depth > kconfig.MaxProbeValueFragmentDepth {
		return "", fmt.Errorf("%s exceeds aggregate depth %d", owner, kconfig.MaxProbeValueFragmentDepth)
	}
	expand := func(value string) (string, error) {
		return expandProbeValueWithResultGuard(value, scratch, sources, sourceRoots, tools, inputs, resultGuard)
	}
	var rendered strings.Builder
	for fragmentIndex, fragment := range fragments {
		if fragment.When != nil {
			include, err := evaluatePredicate(*fragment.When, results, scratch, sources, sourceRoots, inputs, execroot, toolsetPaths)
			if err != nil {
				return "", fmt.Errorf("evaluate %s fragment %d: %w", owner, fragmentIndex, err)
			}
			if !include {
				continue
			}
		}
		var expanded string
		var err error
		if len(fragment.Fragments) != 0 {
			expanded, err = renderProbeValueFragmentsWithResultGuard(
				fmt.Sprintf("%s fragment %d aggregate", owner, fragmentIndex),
				fragment.Fragments, depth+1,
				results, scratch, sources, sourceRoots, tools, inputs, execroot, toolsetPaths, resultGuard,
			)
		} else {
			expanded, err = expand(fragment.Value)
		}
		if err != nil {
			return "", fmt.Errorf("render %s fragment %d: %w", owner, fragmentIndex, err)
		}
		for transformIndex, transform := range fragment.Transforms {
			transform.Arguments = append([]string(nil), transform.Arguments...)
			dynamicGroupIndex := 0
			for argumentIndex, argument := range transform.Arguments {
				if argumentIndex == transform.InputArgument {
					continue
				}
				if dynamicGroupIndex < len(transform.ArgumentFragments) && transform.ArgumentFragments[dynamicGroupIndex].Index == argumentIndex {
					group := transform.ArgumentFragments[dynamicGroupIndex]
					transform.Arguments[argumentIndex], err = renderProbeValueFragmentsWithResultGuard(
						fmt.Sprintf("%s fragment %d transform %d argument group %d", owner, fragmentIndex, transformIndex, dynamicGroupIndex),
						group.Fragments, depth+1,
						results, scratch, sources, sourceRoots, tools, inputs, execroot, toolsetPaths, resultGuard,
					)
					if err != nil {
						return "", err
					}
					dynamicGroupIndex++
					continue
				}
				transform.Arguments[argumentIndex], err = expand(argument)
				if err != nil {
					return "", fmt.Errorf("expand %s fragment %d transform %d argument %d: %w", owner, fragmentIndex, transformIndex, argumentIndex, err)
				}
			}
			if dynamicGroupIndex != len(transform.ArgumentFragments) {
				return "", fmt.Errorf("%s fragment %d transform %d did not consume every dynamic argument group", owner, fragmentIndex, transformIndex)
			}
			transform.ArgumentFragments = nil
			expanded, err = kconfig.ApplyProbeValueTransform(transform, expanded)
			if err != nil {
				return "", fmt.Errorf("apply %s fragment %d transform %d: %w", owner, fragmentIndex, transformIndex, err)
			}
		}
		if rendered.Len()+len(expanded) > kconfig.MaxProbeInterpolatedBytes {
			return "", fmt.Errorf("%s exceeds %d rendered bytes", owner, kconfig.MaxProbeInterpolatedBytes)
		}
		rendered.WriteString(expanded)
	}
	return rendered.String(), nil
}

func evaluatePredicate(predicate kconfig.ProbePredicate, results []kconfig.ProbeStepResult, scratch, sources, sourceRoots map[string]string, inputs map[string]kconfig.ProbeResult, execroot string, toolsetPaths *toolsetPathResolver) (bool, error) {
	if kconfig.IsProbeResultPredicate(predicate) {
		return kconfig.EvaluateProbeResultPredicate(predicate, inputs)
	}
	switch predicate.Operator {
	case "all", "any":
		want := predicate.Operator == "all"
		for _, operand := range predicate.Operands {
			value, err := evaluatePredicate(operand, results, scratch, sources, sourceRoots, inputs, execroot, toolsetPaths)
			if err != nil {
				return false, err
			}
			if value != want {
				return !want, nil
			}
		}
		return want, nil
	case "not":
		value, err := evaluatePredicate(predicate.Operands[0], results, scratch, sources, sourceRoots, inputs, execroot, toolsetPaths)
		return !value, err
	case "regular-file":
		info, err := os.Stat(scratch[predicate.Scratch])
		if os.IsNotExist(err) {
			return false, nil
		}
		return err == nil && info.Mode().IsRegular(), err
	case "execroot-exists", "execroot-regular-file":
		relative, err := expandProbeValue(predicate.Value, scratch, sources, sourceRoots, nil, inputs)
		if err != nil {
			return false, err
		}
		if err := toolaction.ValidateCanonicalArtifactPath(relative); err != nil {
			return false, err
		}
		physical, err := toolsetPaths.resolve(relative)
		if err != nil {
			return false, err
		}
		info, err := os.Stat(physical)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return predicate.Operator == "execroot-exists" || info.Mode().IsRegular(), nil
	}
	step, ok := findStep(results, predicate.Step)
	if !ok {
		return false, fmt.Errorf("missing step result %q", predicate.Step)
	}
	if predicate.Operator == "exit-zero" {
		return step.Status == "success", nil
	}
	stream, err := stepStream(step, predicate.Stream)
	if err != nil {
		return false, err
	}
	if predicate.Operator == "stream-empty" {
		return stream == "", nil
	}
	if predicate.Operator == "stream-trimmed-empty" {
		return strings.TrimSpace(stream) == "", nil
	}
	if predicate.Operator == "stream-matches" {
		expression, err := regexp.Compile(predicate.Value)
		if err != nil {
			return false, fmt.Errorf("invalid stream predicate pattern: %w", err)
		}
		return expression.MatchString(stream), nil
	}
	return strings.Contains(stream, predicate.Value), nil
}

func findStep(results []kconfig.ProbeStepResult, name string) (kconfig.ProbeStepResult, bool) {
	for _, result := range results {
		if result.Name == name {
			return result, true
		}
	}
	return kconfig.ProbeStepResult{}, false
}

func stepStream(step kconfig.ProbeStepResult, stream string) (string, error) {
	switch stream {
	case "stdout":
		return step.Stdout, nil
	case "stderr":
		return step.Stderr, nil
	case "combined":
		return step.Stdout + step.Stderr, nil
	default:
		return "", fmt.Errorf("unsupported probe stream %q", stream)
	}
}

func expandProbeValue(value string, scratch, sources, sourceRoots map[string]string, tools map[string]actionContract, inputs map[string]kconfig.ProbeResult) (string, error) {
	return expandProbeValueWithResultGuard(value, scratch, sources, sourceRoots, tools, inputs, nil)
}

func expandProbeValueWithResultGuard(value string, scratch, sources, sourceRoots map[string]string, tools map[string]actionContract, inputs map[string]kconfig.ProbeResult, resultGuard func(string) error) (string, error) {
	var expansionErr error
	out := placeholderPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := placeholderPattern.FindStringSubmatch(match)
		if parts[1] == "scratch" {
			if result := scratch[parts[2]]; result != "" {
				return result
			}
		} else if parts[1] == "source" {
			if result := sources[parts[2]]; result != "" {
				return result
			}
		} else if parts[1] == "source_root" {
			if result := sourceRoots[parts[2]]; result != "" {
				return result
			}
		} else if parts[1] == "tool" {
			if contract, ok := tools[parts[2]]; ok {
				return contract.path
			}
		} else {
			ordinal, field, ok := strings.Cut(parts[2], ".")
			result, exists := inputs[ordinal]
			if ok && exists {
				if field == "text" && result.Kind == "text" {
					if resultGuard != nil {
						if err := resultGuard(result.Text); err != nil {
							expansionErr = fmt.Errorf("result %s text: %w", ordinal, err)
							return match
						}
					}
					return result.Text
				}
				if field == "boolean" && result.Kind == "boolean" && result.Boolean != nil {
					value := fmt.Sprint(*result.Boolean)
					if resultGuard != nil {
						if err := resultGuard(value); err != nil {
							expansionErr = fmt.Errorf("result %s boolean: %w", ordinal, err)
							return match
						}
					}
					return value
				}
			}
		}
		expansionErr = fmt.Errorf("unbound %s placeholder %q", parts[1], parts[2])
		return match
	})
	if expansionErr != nil {
		return "", expansionErr
	}
	if strings.Contains(out, "${") {
		return "", fmt.Errorf("malformed placeholder in %q", value)
	}
	return out, nil
}

func spliceActionArguments(action, probe []string) ([]string, error) {
	return toolaction.SpliceArguments(action, probe)
}

func exactRoles(want []string, got map[string]actionContract) error {
	wanted := make(map[string]bool, len(want))
	for _, role := range want {
		wanted[role] = true
		if got[role].path == "" {
			return fmt.Errorf("probe has no configured %s tool", role)
		}
	}
	for role, contract := range got {
		if wanted[role] {
			continue
		}
		base, companion := toolaction.BaseContractRole(role)
		if !companion || !wanted[base] || got[base].path == "" || contract.path != got[base].path {
			return fmt.Errorf("probe tool roles = %v, configured roles = %v", want, sortedRoles(got))
		}
	}
	return nil
}

func sortedRoles(values map[string]actionContract) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func sortedStringKeys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func cloneMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values)+2)
	for key, value := range values {
		out[key] = value
	}
	return out
}

func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for index, key := range keys {
		out[index] = key + "=" + values[key]
	}
	return out
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
		b.exceeded = true
	}
	_, _ = b.buffer.Write(data)
	b.remaining -= len(data)
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

func parseNamed(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range raw {
		name, item, ok := strings.Cut(value, "=")
		if !ok || name == "" || item == "" || out[name] != "" {
			return nil, fmt.Errorf("invalid or repeated binding %q", value)
		}
		out[name] = item
	}
	return out, nil
}

func main() {
	var toolFlags, runtimeToolFlags, toolsetAnchorFlags, inputFlags, sourceFlags, sourceRootFlags, sourceRootTreeFlags, sourceRootAnchorFlags, sourceRootWitnessFlags, toolsetMarkerFlags, actionArgFlags, actionEnvFlags repeatedFlag
	request := flag.String("request", "", "canonical probe request JSON")
	result := flag.String("result", "", "canonical probe result JSON")
	nodeID := flag.String("node_id", "", "content-addressed probe node ID")
	requestID := flag.String("request_id", "", "content-addressed request ID")
	scope := flag.String("scope", "", "target or host toolset scope")
	toolsetManifest := flag.String("toolset_manifest", "", "current-scope identity-bound toolset manifest")
	flag.Var(&toolsetAnchorFlags, "toolset_anchor", "ROOT=PATH current-action typed toolset root anchor (repeatable)")
	flag.Var(&toolFlags, "tool", "ROLE=PATH configured tool (repeatable)")
	flag.Var(&runtimeToolFlags, "runtime_tool", "ROLE=PATH identity-bound runtime tool alias (repeatable)")
	flag.Var(&inputFlags, "input", "ORDINAL=RESULT configured dependency result (repeatable)")
	flag.Var(&sourceFlags, "source", "PATH=ARTIFACT declared source file (repeatable)")
	flag.Var(&sourceRootFlags, "source_root", "NAME=EXECROOT_RELATIVE_DIRECTORY declared logical source root (repeatable)")
	flag.Var(&sourceRootTreeFlags, "source_root_tree", "NAME=TREE_ARTIFACT declared logical source root (repeatable)")
	flag.Var(&sourceRootAnchorFlags, "source_root_anchor", "NAME=ARTIFACT declared logical source root anchor (repeatable)")
	flag.Var(&sourceRootWitnessFlags, "source_root_witness", "NAME=ARTIFACT declared logical source root directory witness (repeatable)")
	flag.Var(&toolsetMarkerFlags, "toolset_marker", "SCOPE=MARKER typed identity marker (repeatable)")
	flag.Var(&actionArgFlags, "action_arg", "ROLE=ARG configured action argv (repeatable)")
	flag.Var(&actionEnvFlags, "action_env", "ROLE=NAME=VALUE configured action environment (repeatable)")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "proberun: %v\n", err)
		os.Exit(2)
	}
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "proberun: positional arguments are not supported")
		os.Exit(2)
	}
	paths, err := parseNamed(toolFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: tools: %v\n", err)
		os.Exit(2)
	}
	runtimeTools, err := parseNamed(runtimeToolFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: runtime tools: %v\n", err)
		os.Exit(2)
	}
	toolsetAnchors, err := parseNamed(toolsetAnchorFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: toolset anchors: %v\n", err)
		os.Exit(2)
	}
	inputs, err := parseNamed(inputFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: result inputs: %v\n", err)
		os.Exit(2)
	}
	sources, err := parseNamed(sourceFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: sources: %v\n", err)
		os.Exit(2)
	}
	sourceRoots, err := parseNamed(sourceRootFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: source roots: %v\n", err)
		os.Exit(2)
	}
	sourceRootAnchors, err := parseNamed(sourceRootAnchorFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: source root anchors: %v\n", err)
		os.Exit(2)
	}
	sourceRootTrees, err := parseNamed(sourceRootTreeFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: source root trees: %v\n", err)
		os.Exit(2)
	}
	sourceRootWitnesses, err := parseNamed(sourceRootWitnessFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: source root witnesses: %v\n", err)
		os.Exit(2)
	}
	toolsetMarkers, err := parseNamed(toolsetMarkerFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proberun: toolset markers: %v\n", err)
		os.Exit(2)
	}
	contracts := map[string]actionContract{}
	for role, path := range paths {
		contracts[role] = actionContract{path: path, environment: map[string]string{}}
	}
	for _, raw := range actionArgFlags {
		role, argument, ok := strings.Cut(raw, "=")
		contract, exists := contracts[role]
		if !ok || !exists {
			fmt.Fprintf(os.Stderr, "proberun: invalid action argument %q\n", raw)
			os.Exit(2)
		}
		contract.arguments = append(contract.arguments, argument)
		contracts[role] = contract
	}
	for _, raw := range actionEnvFlags {
		role, rest, ok := strings.Cut(raw, "=")
		name, value, envOK := strings.Cut(rest, "=")
		contract, exists := contracts[role]
		if !ok || !envOK || !exists || name == "" {
			fmt.Fprintf(os.Stderr, "proberun: invalid action environment %q\n", raw)
			os.Exit(2)
		}
		contract.environment[name] = value
		contracts[role] = contract
	}
	if err := runProbe(probeOptions{
		request: *request, result: *result, nodeID: *nodeID, requestID: *requestID, scope: *scope,
		toolsetManifest: *toolsetManifest, toolsetAnchors: toolsetAnchors,
		toolsetMarkers: toolsetMarkers, tools: contracts, runtimeTools: runtimeTools, inputs: inputs, sources: sources,
		sourceRoots: sourceRoots, sourceRootTrees: sourceRootTrees, sourceRootAnchors: sourceRootAnchors, sourceRootWitnesses: sourceRootWitnesses,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "proberun: %v\n", err)
		os.Exit(1)
	}
}

var _ io.Writer = (*limitedBuffer)(nil)

func validIdentity(value string) bool {
	digest, ok := strings.CutPrefix(value, "sha256-")
	if !ok || len(digest) != 64 {
		return false
	}
	for _, character := range digest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
