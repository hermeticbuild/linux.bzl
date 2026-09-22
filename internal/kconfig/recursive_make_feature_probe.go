package kconfig

// A source Makefile can ask $(shell $(MAKE) ... && echo 1 || echo 0) while it
// is being parsed. The selected child is a compiler capability query: only
// its exit status enters Make, and its outputs belong to that child process.
// Parse the child Makefile to choose the recipe, then measure that recipe with
// the ordinary content-addressed compiler probe protocol.

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	featureProbeSourceTree = "__LINUX_BZL_SOURCE_TREE__"
	featureProbeObjectTree = "__LINUX_BZL_OBJECT_TREE__"
)

type recursiveMakeFeatureInvocation struct {
	directory string
	goal      string
	output    string
	variables map[string]string
	success   string
	failure   string
}

func (s *KbuildProbeScopes) recursiveMakeFeatureProbe(ctx context.Context, command string) (string, bool, error) {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, CompactKbuildRecursiveMakeProvenanceToken) {
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	invocation, err := parseRecursiveMakeFeatureInvocation(command)
	if err != nil {
		return "", true, fmt.Errorf("selected recursive Make feature query: %w", err)
	}
	cc, ok := invocation.variables["CC"]
	if !ok {
		return "", true, fmt.Errorf("selected recursive Make feature query has no CC assignment")
	}
	ref, configured := parseKbuildActionRoleToken(cc)
	if !configured || ref.Role != "cc" || ref.Scope == KbuildActionRoleAutoScope {
		return "", true, fmt.Errorf("selected recursive Make feature query has no explicitly scoped configured CC")
	}
	e := s.evaluators[ref.Scope]
	if e == nil || e.tools["cc"] == "" || e.sourceRoot == "" {
		return "", true, fmt.Errorf("selected recursive Make feature query requires the configured %s CC and Linux source root", ref.Scope)
	}
	profile, target, err := s.recursiveMakeFeatureProfile(ctx, e, invocation)
	if err != nil {
		return "", true, err
	}
	value, err := e.recursiveMakeFeatureTargetProbe(profile, target, invocation)
	return value, true, err
}

func parseRecursiveMakeFeatureInvocation(command string) (recursiveMakeFeatureInvocation, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("lex child Make command: %w", err)
	}
	if len(tokens) < 14 || tokens[0].operator || tokens[0].value != CompactKbuildRecursiveMakeProvenanceToken {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make query has no authenticated command head")
	}
	for _, token := range tokens {
		if token.shellExpansion || token.pathnameExpansion || strings.Contains(token.value, compactKbuildLiteralDollarToken) {
			return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make query retains active shell expansion")
		}
	}
	and := -1
	for index, token := range tokens {
		if token.operator && token.value == "&&" {
			and = index
			break
		}
	}
	if and < 0 || and+5 >= len(tokens) || !tokens[and+3].operator || tokens[and+3].value != "||" ||
		tokens[and+1].operator || tokens[and+1].value != "echo" || tokens[and+2].operator ||
		tokens[and+4].operator || tokens[and+4].value != "echo" || tokens[and+5].operator || and+6 != len(tokens) {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make query must reduce one status with && echo VALUE || echo VALUE")
	}
	// The source suppresses both streams from the child. A different shell
	// program, pipeline, or output redirection needs its own typed lowering.
	makeWords := tokens[:and]
	if len(makeWords) < 8 || !makeWords[len(makeWords)-5].operator || makeWords[len(makeWords)-5].value != ">" ||
		makeWords[len(makeWords)-4].operator || makeWords[len(makeWords)-4].value != "/dev/null" ||
		makeWords[len(makeWords)-3].operator || makeWords[len(makeWords)-3].value != "2" ||
		!makeWords[len(makeWords)-2].operator || makeWords[len(makeWords)-2].value != ">" ||
		makeWords[len(makeWords)-1].operator || makeWords[len(makeWords)-1].value != "/dev/null" ||
		makeWords[len(makeWords)-3].end != makeWords[len(makeWords)-2].start {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make status must discard its stdout and stderr")
	}
	makeWords = makeWords[:len(makeWords)-5]
	result := recursiveMakeFeatureInvocation{
		variables: map[string]string{}, success: tokens[and+2].value, failure: tokens[and+5].value,
	}
	if result.success == result.failure || len(result.success) > 64 || len(result.failure) > 64 ||
		strings.TrimSpace(result.success) != result.success || strings.TrimSpace(result.failure) != result.failure ||
		strings.ContainsAny(result.success+result.failure, "\x00\r\n") {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make status has invalid success or failure text")
	}
	for index := 1; index < len(makeWords); index++ {
		word := makeWords[index]
		if word.operator {
			return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make argv contains shell operator %q", word.value)
		}
		if word.value == "-C" {
			if index+1 >= len(makeWords) || result.directory != "" || makeWords[index+1].operator {
				return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make has missing or repeated -C directory")
			}
			result.directory = makeWords[index+1].value
			index++
			continue
		}
		if name, value, assignment := strings.Cut(word.value, "="); assignment {
			if !validKbuildCommandEnvironmentName(name) {
				return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make has invalid assignment name %q", name)
			}
			// These names are interpreted by GNU Make itself when starting the
			// child or choosing its shell. Merely recording them as Make variable
			// data would select a different recipe or shell than the real child.
			switch name {
			case "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEOVERRIDES", "MAKEFILES", "MFLAGS", "SHELL":
				return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make has unsupported interpreter-control assignment %q", name)
			}
			if _, duplicate := result.variables[name]; duplicate {
				return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make repeats assignment %q", name)
			}
			result.variables[name] = value
			continue
		}
		if result.goal != "" {
			return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make query has multiple goals or an unsupported option")
		}
		result.goal = word.value
	}
	if !strings.HasPrefix(result.directory, featureProbeSourceTree+"/") ||
		!strings.HasPrefix(result.goal, featureProbeObjectTree+"/") {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make must select an immutable source directory and an object-tree goal")
	}
	result.directory = strings.TrimPrefix(result.directory, featureProbeSourceTree+"/")
	result.goal = strings.TrimPrefix(result.goal, featureProbeObjectTree+"/")
	if err := validateProbeSourcePath(result.directory); err != nil {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make source directory: %w", err)
	}
	if err := validateProbeSourcePath(result.goal); err != nil {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make object-tree goal: %w", err)
	}
	result.output = result.variables["OUTPUT"]
	if !strings.HasPrefix(result.output, featureProbeObjectTree+"/") || !strings.HasSuffix(result.output, "/") {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make OUTPUT must be an object-tree directory ending in /")
	}
	output := strings.TrimSuffix(strings.TrimPrefix(result.output, featureProbeObjectTree+"/"), "/")
	if err := validateProbeSourcePath(output); err != nil {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make OUTPUT directory: %w", err)
	}
	if !strings.HasPrefix(result.goal, output+"/") ||
		strings.ContainsRune(strings.TrimPrefix(result.goal, output+"/"), '/') ||
		!strings.HasSuffix(result.goal, ".bin") {
		return recursiveMakeFeatureInvocation{}, fmt.Errorf("child Make goal is outside its private OUTPUT directory or is not a binary")
	}
	return result, nil
}

func (s *KbuildProbeScopes) recursiveMakeFeatureProfile(
	ctx context.Context,
	e *LinuxProbeEvaluator,
	invocation recursiveMakeFeatureInvocation,
) (CompactKbuildProfile, string, error) {
	makefile := path.Join(invocation.directory, "Makefile")
	if _, err := e.immutableLinuxSourcePath(makefile, false); err != nil {
		return CompactKbuildProfile{}, "", fmt.Errorf("selected child Makefile: %w", err)
	}
	root, err := filepath.Abs(e.sourceRoot)
	if err != nil {
		return CompactKbuildProfile{}, "", err
	}
	// This is an unmaterialized object tree, not a writable output base. It
	// ensures optional child includes cannot read stale physical source files.
	// No file or directory is created here.
	absentObjectRoot := filepath.Join(root, "__linux_bzl_unmaterialized_feature_objects__")
	if _, err := os.Lstat(absentObjectRoot); err == nil || !os.IsNotExist(err) {
		return CompactKbuildProfile{}, "", fmt.Errorf("selected feature probe object-view path is not proven absent: %v", err)
	}
	working := featureProbeSourceTree + "/" + invocation.directory
	base := KbuildOptions{
		RootDir: root, WorkingDir: filepath.Join(root, filepath.FromSlash(invocation.directory)),
		SourceRoots: map[string]string{featureProbeSourceTree: root, featureProbeObjectTree: absentObjectRoot},
		Variables: map[string]string{
			"CURDIR": working, "PWD": working, "srctree": featureProbeSourceTree,
			"objtree": featureProbeObjectTree, "MAKECMDGOALS": featureProbeObjectTree + "/" + invocation.goal,
		},
		EnvironmentVariables:  maps.Clone(e.scriptEnvironment),
		CommandLineVariables:  maps.Clone(invocation.variables),
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
		CaptureTargetEvaluator: true, SkipExportedVariables: true,
	}
	// A selected child inherits its source-owned Make argv. The exact scoped
	// parser callbacks retain new probe dependencies without running GNU Make.
	options, err := s.Options(e.scope, base)
	if err != nil {
		return CompactKbuildProfile{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return CompactKbuildProfile{}, "", err
	}
	parsed, err := ParseKbuildFileTree(filepath.Join(root, filepath.FromSlash(makefile)), options)
	if err != nil {
		return CompactKbuildProfile{}, "", fmt.Errorf("parse selected feature child %s: %w", makefile, err)
	}
	profile, err := NewCompactKbuildProfile("feature:"+makefile, makefile, root, parsed)
	if err != nil {
		return CompactKbuildProfile{}, "", err
	}
	SetCompactKbuildProfileDirectory(&profile, invocation.directory)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: invocation.directory,
	}); err != nil {
		return CompactKbuildProfile{}, "", err
	}
	return profile, invocation.goal, nil
}

func (e *LinuxProbeEvaluator) recursiveMakeFeatureTargetProbe(
	profile CompactKbuildProfile,
	target string,
	invocation recursiveMakeFeatureInvocation,
) (string, error) {
	makeTarget := featureProbeObjectTree + "/" + target
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	if err != nil {
		return "", fmt.Errorf("select feature child goal %q: %w", target, err)
	}
	if !resolved.HasSelectedRule() {
		return "", fmt.Errorf("feature child goal %q has no selected source recipe", target)
	}
	match := resolved.match
	if len(match.rule.Recipe) != 1 {
		return "", fmt.Errorf("%s: feature child goal %q requires one selected compiler recipe", match.rule.Position, target)
	}
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, match, nil)
	if err != nil {
		return "", err
	}
	// Recipe expansion happens against the child's actual selected Make state;
	// this also exposes lazy $(shell ...) flag queries as explicit probes.
	command, err := evaluateCompactKbuildTextForMakeTarget(
		profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, nil, match.rule.Recipe[0], false,
	)
	if err != nil {
		return "", fmt.Errorf("%s: expand selected feature compiler recipe: %w", match.rule.Position, err)
	}
	command = compactKbuildMaterializeActionTreeMarkers(compactKbuildRecipeExecutionText(command))
	request, dependencies, err := e.recursiveMakeFeatureLinkRequest(invocation, command)
	if err != nil {
		return "", fmt.Errorf("%s: selected feature compiler recipe: %w", match.rule.Position, err)
	}
	truth, err := e.requestTruth(request, dependencies...)
	if err != nil {
		return "", err
	}
	return e.renderTruth(truth, invocation.success, invocation.failure)
}

func (e *LinuxProbeEvaluator) recursiveMakeFeatureLinkRequest(
	invocation recursiveMakeFeatureInvocation,
	command string,
) (ProbeRequest, []ProbeReference, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return ProbeRequest{}, nil, err
	}
	if len(tokens) < 8 || tokens[0].operator || tokens[0].value != KbuildActionRoleToken(e.scope, "cc") {
		return ProbeRequest{}, nil, fmt.Errorf("selected child recipe does not start with configured %s CC", e.scope)
	}
	for _, token := range tokens {
		if token.shellExpansion || token.pathnameExpansion || strings.Contains(token.value, compactKbuildLiteralDollarToken) {
			return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe retains active shell expansion")
		}
	}
	// GNU Make's BUILD helper redirects diagnostic output into an OUTPUT-local
	// .make.output sibling. That file is private to the child and has no role
	// in the status result; proberun discards both streams instead.
	redirect := -1
	for index := 1; index < len(tokens); index++ {
		if tokens[index].operator && tokens[index].value == ">" {
			redirect = index
			break
		}
	}
	if redirect < 0 || redirect+5 >= len(tokens) ||
		tokens[redirect].value != ">" ||
		tokens[redirect+1].operator || tokens[redirect+1].value != invocation.output+strings.TrimSuffix(path.Base(invocation.goal), ".bin")+".make.output" ||
		tokens[redirect+2].operator || tokens[redirect+2].value != "2" ||
		!tokens[redirect+3].operator || tokens[redirect+3].value != ">" ||
		!tokens[redirect+4].operator || tokens[redirect+4].value != "&" ||
		tokens[redirect+5].operator || tokens[redirect+5].value != "1" ||
		tokens[redirect+2].end != tokens[redirect+3].start || tokens[redirect+3].end != tokens[redirect+4].start ||
		tokens[redirect+4].end != tokens[redirect+5].start {
		return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe has an undeclared shell operation or diagnostic path")
	}
	words := make([]compactKbuildRecipeToken, 0, len(tokens)-7)
	words = append(words, tokens[1:redirect]...)
	words = append(words, tokens[redirect+6:]...)
	flags := make([]string, 0, len(words))
	candidateMask := make([]bool, 0, len(words))
	source := ""
	output := ""
	for index := 0; index < len(words); index++ {
		word := words[index]
		if word.operator {
			return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe contains shell operator %q", word.value)
		}
		if word.value == "-o" {
			if output != "" || index+1 >= len(words) || words[index+1].operator {
				return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe has missing or repeated -o output")
			}
			output = words[index+1].value
			index++
			continue
		}
		if strings.HasSuffix(word.value, ".c") && !strings.HasPrefix(word.value, "-") {
			if source != "" || strings.ContainsRune(word.value, '/') {
				return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe has multiple or nonlocal C sources")
			}
			source = path.Join(invocation.directory, word.value)
			continue
		}
		candidate := probeCompilerRootArgument(word.value)
		// A Make $(shell ...) result is text, not one compiler argv word.
		// Preserve the selected query as a dependency and ask the probe runner
		// to split its sealed stdout with the same shell-word rules that the
		// source recipe applies when it expands that result. Boolean and
		// already-annotated symbolic words retain their existing ownership.
		if symbol, symbolic, symbolErr := e.symbolArgument(candidate); symbolErr != nil {
			return ProbeRequest{}, nil, symbolErr
		} else if symbolic && symbol.kind != "boolean" && symbol.kind != "selection" && symbol.kind != "source-shell-words" {
			candidate, err = e.renderSourceShellWords(candidate)
			if err != nil {
				return ProbeRequest{}, nil, err
			}
		}
		flags = append(flags, candidate)
		// A source-selected -MD/-MMD has no path operand: GCC places its .d
		// sibling beside the managed -o output inside this private scratch.
		candidateMask = append(candidateMask, word.value != "-MD" && word.value != "-MMD")
	}
	if output != featureProbeObjectTree+"/"+invocation.goal || source == "" {
		return ProbeRequest{}, nil, fmt.Errorf("selected child compiler recipe does not write its requested binary from one C source")
	}
	if _, err := e.immutableLinuxSourcePath(source, false); err != nil {
		return ProbeRequest{}, nil, fmt.Errorf("selected feature C source: %w", err)
	}
	// Reject unbound absolute host paths before a process is registered. The
	// runner repeats validation after rendering dynamic dependency fragments.
	staticCandidates := make([]string, 0, len(flags))
	for index, flag := range flags {
		if candidateMask[index] && !linuxProbeSymbolPattern.MatchString(flag) {
			staticCandidates = append(staticCandidates, flag)
		}
	}
	if err := validateFeatureProbeStaticPaths(staticCandidates, invocation.directory); err != nil {
		return ProbeRequest{}, nil, err
	}
	arguments, conditional, fragments, owned, dependencies, err := e.lowerSymbolicCandidateArguments(flags, candidateMask, ProbeCandidatePolicyCCLink)
	if err != nil {
		return ProbeRequest{}, nil, err
	}
	usesHostDeps, err := e.candidateHostDependencyRoot(arguments, conditional, fragments, owned, dependencies)
	if err != nil {
		return ProbeRequest{}, nil, err
	}
	arguments = append(arguments, "-o", "${scratch:output}", "${source:"+source+"}")
	sources, err := e.featureProbeSourceClosure(source)
	if err != nil {
		return ProbeRequest{}, nil, err
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Sources: sources, SourceRoots: []string{linuxProbeSourceRootName},
		Scratch: []ProbeScratch{{Name: "output", Kind: "file"}},
		Steps: []ProbeStep{{
			Name: "compiler-link", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, ArgumentFragments: fragments, Candidate: owned,
			WorkingDirectory: "${source_root:linux}/" + invocation.directory,
			DiscardStdout:    true, DiscardStderr: true,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "exit-zero", Step: "compiler-link",
		}},
	}
	if usesHostDeps {
		request.SourceRoots = []string{linuxProbeHostDepsRootName, linuxProbeSourceRootName}
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, nil, fmt.Errorf("validate selected feature compiler probe: %w", err)
	}
	return request, dependencies, nil
}

func validateFeatureProbeStaticPaths(flags []string, sourceDirectory string) error {
	operands, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCCLink, flags)
	if err != nil {
		return err
	}
	for _, operand := range operands {
		value := flags[operand.Argument][operand.Start:operand.End]
		if strings.HasPrefix(value, "${source_root:linux}/") || strings.HasPrefix(value, "${source_root:"+linuxProbeHostDepsRootName+"}/") {
			continue
		}
		if filepath.IsAbs(value) || strings.HasPrefix(value, "${") {
			return fmt.Errorf("feature compiler path %q is outside declared source, scratch, or host-dependency inputs", value)
		}
		if err := validateProbeSourcePath(path.Join(sourceDirectory, value)); err != nil {
			return fmt.Errorf("feature compiler path %q escapes selected source: %w", value, err)
		}
	}
	return nil
}

func (e *LinuxProbeEvaluator) featureProbeSourceClosure(source string) ([]string, error) {
	seen := map[string]bool{linuxProbeRootAnchor: true}
	pending := []string{source}
	for len(pending) != 0 {
		if len(seen)+len(pending) > maxProbeSources {
			return nil, fmt.Errorf("selected feature source include closure exceeds %d files", maxProbeSources)
		}
		current := pending[0]
		pending = pending[1:]
		if seen[current] {
			continue
		}
		if _, err := e.immutableLinuxSourcePath(current, false); err != nil {
			return nil, fmt.Errorf("feature source include %q: %w", current, err)
		}
		seen[current] = true
		contents, err := os.ReadFile(filepath.Join(e.sourceRoot, filepath.FromSlash(current)))
		if err != nil || len(contents) > 1<<20 {
			return nil, fmt.Errorf("read bounded feature source %q: %v", current, err)
		}
		for _, line := range strings.Split(string(contents), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if !strings.HasPrefix(line, "include") || len(line) > len("include") && line[len("include")] != ' ' && line[len("include")] != '\t' {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, "include"))
			if strings.HasPrefix(value, "<") {
				if !strings.ContainsRune(value, '>') {
					return nil, fmt.Errorf("feature source %q has unterminated system include", current)
				}
				continue
			}
			if !strings.HasPrefix(value, "\"") {
				return nil, fmt.Errorf("feature source %q has dynamic include %q", current, value)
			}
			end := strings.IndexByte(value[1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("feature source %q has unterminated quoted include", current)
			}
			quoted := value[1 : 1+end]
			if quoted == "" || path.IsAbs(quoted) || strings.ContainsAny(quoted, "\\\x00") {
				return nil, fmt.Errorf("feature source %q has invalid quoted include %q", current, quoted)
			}
			included := path.Join(path.Dir(current), quoted)
			if err := validateProbeSourcePath(included); err != nil {
				return nil, fmt.Errorf("feature source %q includes escaping path %q: %w", current, value, err)
			}
			pending = append(pending, included)
		}
	}
	sources := make([]string, 0, len(seen))
	for source := range seen {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	return sources, nil
}
