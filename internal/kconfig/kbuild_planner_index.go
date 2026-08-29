package kconfig

// This file contains process-local indexes for the final Kbuild action
// planner. They are intentionally absent from action-plan output: the parsed
// Make graph remains the source of truth, while exact and pattern indexes make
// repeated target-context queries proportional to the matching declarations
// rather than to every declaration in the invocation.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type compactKbuildIndexedRule struct {
	ruleOrder   int
	targetOrder int
	// target is populated only for pattern declarations. Exact declarations
	// already have the query target as their canonical declared target, so
	// retaining another string header per rule would duplicate the exact index.
	target string
}

type compactKbuildResolvedRule struct {
	ruleOrder    int
	targetOrder  int
	rule         KbuildRule
	target       string
	lookupTarget string
	stem         string
}

type compactKbuildIndexedTargetVariable struct {
	variableOrder int
	target        string
}

// compactKbuildDeclarationBucket combines rule and target-variable indexes for
// one exact target or one literal pattern prefix. Linux usually declares both
// kinds for the same object family; one key/index pair avoids maintaining four
// independent hash tables over the same path strings.
type compactKbuildDeclarationBucket struct {
	rules     []compactKbuildIndexedRule
	variables []compactKbuildIndexedTargetVariable
}

type compactKbuildPlannerRuntime struct {
	exactIndex   map[string]int
	exact        []compactKbuildDeclarationBucket
	patternIndex map[string]int
	patterns     []compactKbuildDeclarationBucket

	sourceRoot string
	sourceMu   sync.Mutex
	sources    map[string]bool
	// sourceShellScripts memoizes whether an immutable source path carries a
	// shell shebang. Kbuild routinely executes extensionless helpers (for
	// example scripts/mkcompile_h) directly; their source content, not a
	// filename suffix, determines whether they use the hermetic script runner.
	sourceShellScripts map[string]bool
}

type compactKbuildPlannerRuntimeKey struct {
	name                string
	directory           string
	ruleCount           int
	targetVariableCount int
}

func compactKbuildPlannerRuntimeForProfile(profile CompactKbuildProfile) *compactKbuildPlannerRuntime {
	if profile.evaluator == nil {
		return newCompactKbuildPlannerRuntime(profile)
	}
	evaluator := profile.evaluator
	key := compactKbuildPlannerRuntimeKey{
		name: profile.Name, directory: profile.Directory,
		ruleCount: len(profile.Rules), targetVariableCount: len(profile.TargetVariables),
	}
	evaluator.plannerMu.Lock()
	defer evaluator.plannerMu.Unlock()
	if runtime := evaluator.plannerRuntimes[key]; runtime != nil {
		return runtime
	}
	if evaluator.plannerRuntimes == nil {
		evaluator.plannerRuntimes = map[compactKbuildPlannerRuntimeKey]*compactKbuildPlannerRuntime{}
	}
	runtime := newCompactKbuildPlannerRuntime(profile)
	evaluator.plannerRuntimes[key] = runtime
	return runtime
}

func newCompactKbuildPlannerRuntime(profile CompactKbuildProfile) *compactKbuildPlannerRuntime {
	runtime := &compactKbuildPlannerRuntime{
		exactIndex:         map[string]int{},
		patternIndex:       map[string]int{},
		sources:            map[string]bool{},
		sourceShellScripts: map[string]bool{},
	}
	exactBucket := func(target string) int {
		if index, ok := runtime.exactIndex[target]; ok {
			return index
		}
		index := len(runtime.exact)
		runtime.exactIndex[target] = index
		runtime.exact = append(runtime.exact, compactKbuildDeclarationBucket{})
		return index
	}
	patternBucket := func(pattern string) int {
		prefix, _, _ := strings.Cut(pattern, "%")
		if index, ok := runtime.patternIndex[prefix]; ok {
			return index
		}
		index := len(runtime.patterns)
		runtime.patternIndex[prefix] = index
		runtime.patterns = append(runtime.patterns, compactKbuildDeclarationBucket{})
		return index
	}
	for ruleOrder, rule := range profile.Rules {
		for targetOrder, rawTarget := range rule.Targets {
			target := compactKbuildProfileTargetPath(profile, rawTarget)
			if target == "" {
				continue
			}
			candidate := compactKbuildIndexedRule{ruleOrder: ruleOrder, targetOrder: targetOrder}
			if strings.Contains(target, "%") {
				candidate.target = target
				index := patternBucket(target)
				runtime.patterns[index].rules = append(runtime.patterns[index].rules, candidate)
			} else {
				index := exactBucket(target)
				runtime.exact[index].rules = append(runtime.exact[index].rules, candidate)
			}
		}
	}
	for variableOrder, variable := range profile.TargetVariables {
		seenExact := map[string]bool{}
		seenPattern := map[string]bool{}
		for _, rawTarget := range variable.Targets {
			target := normalizeCompactKbuildTargetVariableTarget(rawTarget)
			if strings.Contains(target, "%") {
				if seenPattern[target] {
					continue
				}
				seenPattern[target] = true
				index := patternBucket(target)
				runtime.patterns[index].variables = append(
					runtime.patterns[index].variables,
					compactKbuildIndexedTargetVariable{variableOrder: variableOrder, target: target},
				)
			} else if target != "" && !seenExact[target] {
				seenExact[target] = true
				index := exactBucket(target)
				runtime.exact[index].variables = append(
					runtime.exact[index].variables,
					compactKbuildIndexedTargetVariable{variableOrder: variableOrder},
				)
			}
		}
	}
	if profile.evaluator != nil && profile.evaluator.template != nil {
		if root := strings.TrimSpace(profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]); root != "" {
			if absolute, err := filepath.Abs(root); err == nil {
				runtime.sourceRoot = absolute
			}
		}
	}
	return runtime
}

// compactKbuildProfileSourceAncestryDepth reports how much of a logical path
// is backed by existing source-tree ancestry. Evaluated Make variables can
// produce both invocation-relative paths and source-root-relative generated
// paths; comparing the two candidates preserves that distinction without a
// catalogue of variable names or kernel directories.
func compactKbuildProfileSourceAncestryDepth(profile CompactKbuildProfile, logical string) int {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return 0
	}
	root := strings.TrimSpace(profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"])
	if root == "" {
		return 0
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return 0
	}
	logical = canonicalKbuildRulePath(logical)
	if logical == "" {
		return 0
	}
	evaluator := profile.evaluator
	evaluator.pathScopeMu.Lock()
	defer evaluator.pathScopeMu.Unlock()
	if evaluator.sourceAncestry == nil {
		evaluator.sourceAncestry = map[string]int{}
	}
	key := root + "\x00" + logical
	if depth, ok := evaluator.sourceAncestry[key]; ok {
		return depth
	}
	depth := 0
	parts := strings.Split(logical, "/")
	for index := range parts {
		candidate := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:index+1], "/")))
		if _, err := os.Stat(candidate); err != nil {
			break
		}
		depth = index + 1
	}
	evaluator.sourceAncestry[key] = depth
	return depth
}

func (r *compactKbuildPlannerRuntime) exactDeclarations(target string) *compactKbuildDeclarationBucket {
	index, ok := r.exactIndex[target]
	if !ok {
		return nil
	}
	return &r.exact[index]
}

// Pattern lookups consult only literal prefixes of target. Their cost is
// bounded by the target path length and prefix-compatible declarations rather
// than by the total number of patterns in the invocation.
func (r *compactKbuildPlannerRuntime) matchingPatternRules(target string) []compactKbuildIndexedRule {
	var out []compactKbuildIndexedRule
	for end := 0; end <= len(target); end++ {
		index, ok := r.patternIndex[target[:end]]
		if !ok {
			continue
		}
		for _, candidate := range r.patterns[index].rules {
			if _, matched := matchKbuildRulePattern(candidate.target, target); matched {
				out = append(out, candidate)
			}
		}
	}
	return out
}

func (r *compactKbuildPlannerRuntime) matchingPatternVariables(target string) []compactKbuildIndexedTargetVariable {
	var out []compactKbuildIndexedTargetVariable
	for end := 0; end <= len(target); end++ {
		index, ok := r.patternIndex[target[:end]]
		if !ok {
			continue
		}
		for _, candidate := range r.patterns[index].variables {
			if compactKbuildTargetVariablePatternMatches(candidate.target, target) {
				out = append(out, candidate)
			}
		}
	}
	return out
}

func compactKbuildTargetVariablePatternMatches(pattern, target string) bool {
	prefix, suffix, hasPattern := strings.Cut(pattern, "%")
	if !hasPattern {
		return pattern == target
	}
	return len(target) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(target, prefix) && strings.HasSuffix(target, suffix)
}

func compactKbuildRuleCandidates(profile CompactKbuildProfile, target string) []compactKbuildResolvedRule {
	return compactKbuildRuleCandidatesForMakeTarget(profile, target, target)
}

// ResolveCompactKbuildMakeTarget binds GNU Make's lexical lookup word to an
// already-canonical graph target. Graph-rooted spellings are considered before
// invocation-relative spellings: OUTPUT, objtree, and parent-traversal words
// can name an object-root target even when Make executes below a different
// source directory. The returned word is suitable for rule and target-variable
// lookup; valid reports whether makeTarget actually aliases target. Callers
// without a distinct lexical spelling must pass target explicitly.
func ResolveCompactKbuildMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) (string, bool) {
	target = compactKbuildGraphTargetPath(target)
	makeTarget = strings.TrimSpace(filepathToSlash(makeTarget))
	if makeTarget == "" {
		return target, false
	}
	for _, marker := range []string{
		"__LINUX_BZL_SOURCE_TREE__",
		"__LINUX_BZL_OBJECT_TREE__",
		compactKbuildActionSourceTreeMarker,
		compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker,
	} {
		if makeTarget == marker {
			makeTarget = ""
			break
		}
		if strings.HasPrefix(makeTarget, marker+"/") {
			makeTarget = strings.TrimPrefix(makeTarget, marker+"/")
			break
		}
	}
	for strings.HasPrefix(makeTarget, "./") {
		makeTarget = strings.TrimPrefix(makeTarget, "./")
	}
	if makeTarget == "" {
		return target, false
	}
	if target == "" || filepath.IsAbs(filepath.FromSlash(makeTarget)) || strings.ContainsAny(makeTarget, "\x00\r\n") {
		return target, false
	}
	if compactKbuildGraphTargetPath(makeTarget) == target {
		return makeTarget, true
	}
	// Some declarations retain an invocation-relative automatic word while the
	// captured rule has already expanded $(obj). Reattach only a proven profile
	// directory, without path-cleaning the lexical alias used for matching.
	directories := []string{profile.Directory}
	if location, ok := CompactKbuildProfileInvocationLocation(profile); ok {
		directories = append(directories, location.Directory)
	}
	for _, directory := range directories {
		directory = strings.Trim(canonicalKbuildRulePath(directory), "/")
		if directory == "" {
			continue
		}
		candidate := directory + "/" + makeTarget
		if compactKbuildGraphTargetPath(candidate) == target {
			return candidate, true
		}
	}
	return target, false
}

// compactKbuildRuleLookupTarget preserves the lexical filename GNU Make used
// for implicit-rule search when that spelling names the same canonical graph
// target. Linux deliberately uses parent traversal in composite object lists
// (for example arch/x86/kvm/../../../virt/kvm/kvm_main.o); cleaning that word
// before matching loses the $(obj)/%.o pattern and its corresponding stem.
func compactKbuildRuleLookupTarget(profile CompactKbuildProfile, target, makeTarget string) string {
	lookupTarget, valid := ResolveCompactKbuildMakeTarget(profile, target, makeTarget)
	if !valid {
		return ""
	}
	return lookupTarget
}

func compactKbuildRuleCandidatesForMakeTarget(profile CompactKbuildProfile, target, makeTarget string) []compactKbuildResolvedRule {
	target = compactKbuildGraphTargetPath(target)
	lookupTarget := compactKbuildRuleLookupTarget(profile, target, makeTarget)
	if lookupTarget == "" {
		return nil
	}
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	resolved := []compactKbuildResolvedRule{}
	seen := map[struct{ rule, target int }]bool{}
	appendResolved := func(values []compactKbuildResolvedRule) {
		for _, value := range values {
			key := struct{ rule, target int }{value.ruleOrder, value.targetOrder}
			if seen[key] {
				continue
			}
			seen[key] = true
			resolved = append(resolved, value)
		}
	}
	// Exact declarations are indexed by canonical graph identity. They still
	// merge into a recipe selected through a lexical implicit-rule alias.
	if exact := runtime.exactDeclarations(target); exact != nil {
		appendResolved(resolveCompactKbuildRuleCandidates(profile, target, exact.rules))
	}
	// Pattern search uses GNU Make's lexical target. Also retain canonical
	// pattern matches as a fallback for declarations whose own parent traversal
	// was canonicalized while the profile index was built.
	appendResolved(resolveCompactKbuildRuleCandidates(
		profile, lookupTarget, runtime.matchingPatternRules(lookupTarget),
	))
	if lookupTarget != target {
		appendResolved(resolveCompactKbuildRuleCandidates(
			profile, target, runtime.matchingPatternRules(target),
		))
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		if resolved[i].ruleOrder != resolved[j].ruleOrder {
			return resolved[i].ruleOrder < resolved[j].ruleOrder
		}
		return resolved[i].targetOrder < resolved[j].targetOrder
	})
	return resolved
}

func resolveCompactKbuildRuleCandidates(
	profile CompactKbuildProfile,
	target string,
	indexed []compactKbuildIndexedRule,
) []compactKbuildResolvedRule {
	out := make([]compactKbuildResolvedRule, 0, len(indexed))
	for _, candidate := range indexed {
		if candidate.ruleOrder < 0 || candidate.ruleOrder >= len(profile.Rules) {
			continue
		}
		rule := profile.Rules[candidate.ruleOrder]
		declaredTarget := candidate.target
		if declaredTarget == "" {
			declaredTarget = target
		}
		stem, matched := compactKbuildRuleTargetStem(profile, rule, declaredTarget, target)
		if !matched {
			continue
		}
		out = append(out, compactKbuildResolvedRule{
			ruleOrder: candidate.ruleOrder, targetOrder: candidate.targetOrder,
			rule: rule, target: declaredTarget, lookupTarget: target, stem: stem,
		})
	}
	return out
}

func normalizeCompactKbuildTargetVariableTarget(target string) string {
	return filepath.ToSlash(strings.TrimPrefix(target, "./"))
}

func compactKbuildTargetVariables(profile CompactKbuildProfile, target string) []KbuildTargetVariable {
	return compactKbuildTargetVariablesForMakeTarget(profile, target, target)
}

func compactKbuildTargetVariablesForMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) []KbuildTargetVariable {
	target = compactKbuildGraphTargetPath(target)
	lookupTarget := compactKbuildRuleLookupTarget(profile, target, makeTarget)
	if lookupTarget == "" {
		return nil
	}
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	orders := []int{}
	lookupTarget = normalizeCompactKbuildTargetVariableTarget(lookupTarget)
	if exact := runtime.exactDeclarations(lookupTarget); exact != nil {
		for _, candidate := range exact.variables {
			orders = append(orders, candidate.variableOrder)
		}
	}
	for _, candidate := range runtime.matchingPatternVariables(lookupTarget) {
		orders = append(orders, candidate.variableOrder)
	}
	sort.Ints(orders)
	out := make([]KbuildTargetVariable, 0, len(orders))
	previous := -1
	for _, order := range orders {
		if order == previous || order < 0 || order >= len(profile.TargetVariables) {
			continue
		}
		previous = order
		out = append(out, profile.TargetVariables[order])
	}
	return out
}
