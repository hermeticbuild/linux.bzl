package kconfig

// This file models GNU Make recipe expansion effects separately from commands
// that create files. Linux uses selected phony/control targets to mutate its
// exported Kbuild variables with $(eval ...), sometimes using $(shell ...) to
// read an earlier generated file. Discovery below performs no I/O: it preserves
// each shell result as an opaque token and applies assignments in the same
// prerequisite/recipe order as Make.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const kbuildDeferredContentTokenPrefix = "LINUX_BZL_KBUILD_CONTENT_"

// KbuildDeferredContentQuery is an execution-time text query encountered while
// expanding one selected control recipe. Command is source-derived and still
// uses the invocation's stable source/object-tree sentinels; later lowering
// turns it into an ordinary action-plan node with exact input edges.
type KbuildDeferredContentQuery struct {
	Token     string
	Command   string
	Target    string
	Profile   CompactKbuildProfile
	Transform string
	// Generation and Environment identify the exact source-ordered Make process
	// state in which Command was expanded. Environment deliberately retains
	// symbolic probe and configured-action tokens until action-plan lowering.
	Generation uint64
	// CommandShell retains CONFIG_SHELL for immutable-helper classification.
	// GNU Make commonly does not export it, so Environment cannot recover it.
	CommandShell string
	Environment  map[string]string
	Origin       KbuildDeferredContentOrigin
	ActionRoles  []KbuildActionRoleRef
	ObjectTree   CompactKbuildObjectTreeObservation
	Selection    KbuildDeferredContentSelection
}

// KbuildDeferredContentOrigin identifies the Make target whose expansion
// created a query. It is distinct from every later action that consumes the
// resulting value, including consumers in descendant Make invocations.
type KbuildDeferredContentOrigin struct {
	Profile string
	Target  string
}

// KbuildDeferredContentSelection is the independently solved physical action
// contract for one deferred query. Artifact encodings use the same exact
// profile+target provenance as ordinary selected Kbuild actions.
type KbuildDeferredContentSelection struct {
	Token                        string
	Profile                      string
	Target                       string
	Lifecycle                    string
	Scope                        string
	Stage                        string
	UsesInitialObjectTree        bool
	InitialObjectTreeArtifacts   string
	GeneratedObjectTreeArtifacts string
}

func registerKbuildDeferredContentQuery(
	profile CompactKbuildProfile,
	target, command, transform, commandShell string,
	environment map[string]string,
) (string, error) {
	// Action lowering evaluates the same source query with private tree markers
	// so later recipe path algebra cannot rewrite source-owned placeholder-like
	// bytes. Deferred queries are selected earlier from their stable public Make
	// spelling, so materialize only those unforgeable markers before hashing and
	// storing the query contract.
	command = compactKbuildMaterializeActionTreeMarkers(command)
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 1<<20 || strings.ContainsRune(command, 0) {
		return "", fmt.Errorf("deferred Kbuild content query is empty, invalid, or exceeds 1 MiB")
	}
	if profile.deferredContentQueries == nil {
		return "", fmt.Errorf("Kbuild profile %q has no deferred-content provenance registry", profile.Name)
	}
	if transform == "" {
		transform = ActionRecipeContentTransformMakeShellWord
	}
	actionRoles, err := KbuildActionRoleRefs(command)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query action-role provenance: %w", err)
	}
	target = compactKbuildGraphTargetPath(target)
	generation := profile.controlGeneration
	if selected, ok := profile.targetControlGenerations[target]; ok {
		generation = selected
	}
	if environment == nil {
		if snapshot := profile.targetRecipeEnvironments[target]; snapshot != nil {
			environment = snapshot.values
		}
	}
	if commandShell == "" {
		commandShell = profile.targetRecipeShells[target]
	}
	commandShell = compactKbuildMaterializeActionTreeMarkers(commandShell)
	if environment != nil {
		environment = maps.Clone(environment)
		for name, value := range environment {
			environment[name] = compactKbuildMaterializeActionTreeMarkers(value)
		}
	}
	canonicalEnvironment, err := canonicalKbuildDeferredContentEnvironment(environment)
	if err != nil {
		return "", err
	}
	canonicalCommandShell, err := toolaction.CanonicalizeExecutionRootProvenanceCapabilityIdentity(commandShell)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query command shell: %w", err)
	}
	canonicalCommand, err := toolaction.CanonicalizeExecutionRootProvenanceCapabilityIdentity(command)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query command: %w", err)
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		profile.Name,
		profile.Path,
		target,
		transform,
		fmt.Sprintf("%d", generation),
		canonicalEnvironment,
		canonicalCommandShell,
		canonicalCommand,
	}, "\x00")))
	token := kbuildDeferredContentTokenPrefix + hex.EncodeToString(digest[:])
	queryProfile := profile
	// A query registered while expanding an ordinary executable recipe starts
	// from the final profile value, whose per-target maps retain the earlier
	// evaluator and process environment. Promote those exact entries to the
	// query snapshot's fallbacks before clearing the mutable registries.
	if selected := queryProfile.targetEvaluators[target]; selected != nil {
		queryProfile.evaluator = selected
	}
	if activate := queryProfile.targetProbeEnvironmentActivations[target]; activate != nil {
		queryProfile.probeEnvironmentActivation = activate
	}
	queryProfile.controlGeneration = generation
	queryProfile.targetEvaluators = nil
	queryProfile.targetProbeEnvironmentActivations = nil
	queryProfile.targetControlGenerations = nil
	queryProfile.targetRecipeEnvironments = nil
	queryProfile.targetRecipeShells = nil
	// The query is a snapshot of Make state, not another owner of the mutable
	// registry in which that snapshot is recorded. Give later target-context
	// matching a private registry so recursive-variable evaluation can still
	// classify shell expressions without creating a map cycle here.
	queryProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
	query := KbuildDeferredContentQuery{
		Token: token, Command: command, Target: target, Profile: queryProfile, Transform: transform,
		Generation: generation, CommandShell: commandShell, Environment: maps.Clone(environment),
		Origin:      KbuildDeferredContentOrigin{Profile: profile.Name, Target: target},
		ActionRoles: canonicalKbuildActionRoleRefs(actionRoles),
		ObjectTree:  ObserveCompactKbuildObjectTree(command),
	}
	if previous, ok := profile.deferredContentQueries[token]; ok {
		previous, err = normalizedKbuildDeferredContentQuery(previous)
		if err != nil {
			return "", err
		}
		if !sameKbuildDeferredContentQuerySource(previous, query) {
			return "", fmt.Errorf("deferred Kbuild content query token collision")
		}
		return token, nil
	}
	profile.deferredContentQueries[token] = query
	return token, nil
}

// canonicalKbuildDeferredContentEnvironment is both the token preimage and a
// validation boundary for process environment snapshots. NUL separators are
// unambiguous because neither an environment name nor value may contain NUL;
// the leading presence byte keeps a legacy/unsnapshotted nil map distinct from
// an exact empty environment.
func canonicalKbuildDeferredContentEnvironment(environment map[string]string) (string, error) {
	if environment == nil {
		return "0", nil
	}
	names := sortedStringMapKeys(environment)
	var canonical strings.Builder
	canonical.WriteByte('1')
	for _, name := range names {
		value := environment[name]
		if !validKbuildCommandEnvironmentName(name) || strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("deferred Kbuild content query has invalid environment variable %q", name)
		}
		value, err := toolaction.CanonicalizeExecutionRootProvenanceCapabilityIdentity(value)
		if err != nil {
			return "", fmt.Errorf("deferred Kbuild content query environment %q: %w", name, err)
		}
		canonical.WriteByte(0)
		canonical.WriteString(name)
		canonical.WriteByte(0)
		canonical.WriteString(value)
	}
	return canonical.String(), nil
}

// compactKbuildEnvironmentInterner owns immutable copies of exact exported
// environments. Callers may reuse or mutate the maps they submit; equal later
// environments reuse the first canonical snapshot without another map clone.
type compactKbuildEnvironmentInterner struct {
	byDigest map[[sha256.Size]byte]*compactKbuildEnvironmentSnapshot
}

type compactKbuildEnvironmentSnapshot struct {
	values map[string]string
}

func (i *compactKbuildEnvironmentInterner) intern(
	environment map[string]string,
) (*compactKbuildEnvironmentSnapshot, error) {
	canonical, err := canonicalKbuildDeferredContentEnvironment(environment)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(canonical))
	if previous := i.byDigest[digest]; previous != nil {
		// Retain an explicit equality check so a digest collision or a future
		// canonicalization change cannot silently alias two process environments.
		if !maps.Equal(previous.values, environment) {
			return nil, fmt.Errorf("Kbuild exported environment digest collision")
		}
		return previous, nil
	}
	if i.byDigest == nil {
		i.byDigest = map[[sha256.Size]byte]*compactKbuildEnvironmentSnapshot{}
	}
	snapshot := &compactKbuildEnvironmentSnapshot{
		values: maps.Clone(environment),
	}
	i.byDigest[digest] = snapshot
	return snapshot, nil
}

// KbuildControlEffect records one assignment emitted by GNU Make's eval
// function. Assignment is the once-expanded text parsed by eval's second pass;
// Variable, Operator, and Flavor make ordering/flavor assertions explicit.
type KbuildControlEffect struct {
	Target     string
	Assignment string
	Variable   string
	Operator   string
	Flavor     string
}

// KbuildControlEvaluation contains a profile whose process-local evaluator has
// all selected control effects applied, plus the deferred queries that back any
// opaque values in those effects.
type KbuildControlEvaluation struct {
	Profile CompactKbuildProfile
	Effects []KbuildControlEffect
	Queries []KbuildDeferredContentQuery
	// recipeSnapshots retains the exact cumulative Make state immediately
	// before each selected recipe line is expanded. Recursive invocation
	// discovery consumes these snapshots so an export changed by a later
	// $(eval ...) cannot leak backward into an earlier child Make process.
	recipeSnapshots map[kbuildControlRecipeKey]KbuildControlEvaluation
}

type kbuildControlRecipeKey struct {
	target      string
	ruleIndex   int
	recipeIndex int
}

// KbuildControlEvaluationOptions binds control traversal to the same concrete
// rule selection used by the invocation planner. GNU Make chooses one viable
// implicit rule after merging ordinary explicit declarations; evaluating
// control recipes from every syntactic pattern candidate would apply effects
// from a rule which never runs.
type KbuildControlEvaluationOptions struct {
	// SelectedRuleIndexes receives both the canonical graph identity and GNU
	// Make's lexical lookup spelling. The latter may intentionally contain
	// parent traversal and is required to select the same implicit pattern rule
	// as the invocation planner.
	SelectedRuleIndexes func(target, makeTarget string) []int
	// TargetIsSatisfied reports immutable source/config targets which the
	// selected invocation treats as already updated. It must return false for
	// phony targets and source targets with an always-run normal prerequisite.
	// Group peers are forced only after another, unsatisfied peer has selected
	// their one shared recipe.
	TargetIsSatisfied func(target string) bool
	// BindProbeEnvironment interns one complete target-scope exported process
	// environment and returns the process-local closure which restores it.
	// Control traversal invokes that closure before expanding the corresponding
	// recipe line and retains it on every evaluator snapshot created there.
	BindProbeEnvironment func(map[string]string) (func() error, error)
}

// KbuildControlEvaluationBeforeRecipe returns the source-ordered Make state
// immediately before one selected recipe line. Position and recipeIndex name
// the declaration and line exactly; target disambiguates pattern and
// multi-target rule executions.
func KbuildControlEvaluationBeforeRecipe(
	evaluation KbuildControlEvaluation,
	target string,
	rule KbuildRule,
	recipeIndex int,
) (KbuildControlEvaluation, bool) {
	if recipeIndex < 0 || recipeIndex >= len(rule.Recipe) {
		return KbuildControlEvaluation{}, false
	}
	for ruleIndex, candidate := range evaluation.Profile.Rules {
		if candidate.Position == rule.Position && candidate.TargetPattern == rule.TargetPattern &&
			candidate.Separator == rule.Separator && slices.Equal(candidate.Targets, rule.Targets) &&
			slices.Equal(candidate.Prerequisites, rule.Prerequisites) &&
			slices.Equal(candidate.OrderOnly, rule.OrderOnly) && slices.Equal(candidate.Recipe, rule.Recipe) {
			return KbuildControlEvaluationBeforeRecipeIndex(evaluation, target, ruleIndex, recipeIndex)
		}
	}
	return KbuildControlEvaluation{}, false
}

// KbuildControlEvaluationBeforeRecipeIndex is the collision-free production
// accessor used when the selected traversal retains declaration indexes.
func KbuildControlEvaluationBeforeRecipeIndex(
	evaluation KbuildControlEvaluation,
	target string,
	ruleIndex int,
	recipeIndex int,
) (KbuildControlEvaluation, bool) {
	if ruleIndex < 0 || ruleIndex >= len(evaluation.Profile.Rules) ||
		recipeIndex < 0 || recipeIndex >= len(evaluation.Profile.Rules[ruleIndex].Recipe) {
		return KbuildControlEvaluation{}, false
	}
	snapshot, ok := evaluation.recipeSnapshots[kbuildControlRecipeKey{
		target:    compactKbuildGraphTargetPath(target),
		ruleIndex: ruleIndex, recipeIndex: recipeIndex,
	}]
	return snapshot, ok
}

// EvaluateSelectedKbuildControlEffects walks only EntryTargets and their
// prerequisite closure. It interprets exact outer $(eval ...) recipe lines,
// preserving normal/order-only prerequisite order and recipe line order. Other
// recipe commands remain action-plan work. An eval hidden inside another shell
// construct is rejected because silently changing its expansion order would be
// different from GNU Make.
func EvaluateSelectedKbuildControlEffects(profile CompactKbuildProfile) (KbuildControlEvaluation, error) {
	return EvaluateSelectedKbuildControlEffectsWithOptions(profile, KbuildControlEvaluationOptions{})
}

func EvaluateSelectedKbuildControlEffectsWithOptions(
	profile CompactKbuildProfile,
	options KbuildControlEvaluationOptions,
) (KbuildControlEvaluation, error) {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q has no source-derived target evaluator", profile.Name)
	}
	cumulative := cloneKbuildParserForEvaluation(profile.evaluator.template)
	result := KbuildControlEvaluation{
		Profile: profile, recipeSnapshots: map[kbuildControlRecipeKey]KbuildControlEvaluation{},
	}
	queries := maps.Clone(profile.deferredContentQueries)
	if queries == nil {
		queries = map[string]KbuildDeferredContentQuery{}
	}
	queryOrder := make([]string, 0, len(queries))
	for token := range queries {
		queryOrder = append(queryOrder, token)
	}
	sort.Strings(queryOrder)
	var generationEvaluator *kbuildTargetEvaluator
	snapshot := func() KbuildControlEvaluation {
		if generationEvaluator == nil {
			generationEvaluator = &kbuildTargetEvaluator{template: cloneKbuildParserForEvaluation(cumulative)}
		}
		profileSnapshot := profile
		profileSnapshot.evaluator = generationEvaluator
		profileSnapshot.targetEvaluators = nil
		profileSnapshot.targetProbeEnvironmentActivations = nil
		profileSnapshot.controlGeneration = uint64(len(result.Effects))
		profileSnapshot.deferredContentQueries = maps.Clone(queries)
		evaluationSnapshot := KbuildControlEvaluation{
			Profile: profileSnapshot,
			Effects: slices.Clone(result.Effects),
		}
		for _, token := range queryOrder {
			evaluationSnapshot.Queries = append(evaluationSnapshot.Queries, queries[token])
		}
		return evaluationSnapshot
	}
	registerQuery := func(target, command, commandShell string, environment map[string]string, queryProfile CompactKbuildProfile) (string, error) {
		queryProfile.deferredContentQueries = queries
		token, err := registerKbuildDeferredContentQuery(
			queryProfile, target, command, ActionRecipeContentTransformMakeShellWord, commandShell,
			environment,
		)
		if err != nil {
			return "", err
		}
		if !slices.Contains(queryOrder, token) {
			queryOrder = append(queryOrder, token)
		}
		return token, nil
	}

	seen := map[string]bool{}
	visiting := map[string]bool{}
	groupVisiting := map[string]bool{}
	groupDone := map[string]bool{}
	targetEvaluators := map[string]*kbuildTargetEvaluator{}
	targetProbeEnvironmentActivations := map[string]func() error{}
	targetControlGenerations := map[string]uint64{}
	targetRecipeEnvironments := map[string]*compactKbuildEnvironmentSnapshot{}
	targetRecipeEnvironmentInterner := compactKbuildEnvironmentInterner{}
	targetRecipeShells := map[string]string{}
	targetVariableScopes := map[string]*compactKbuildTargetVariableScope{}
	profile.targetControlGenerations = targetControlGenerations
	profile.targetRecipeEnvironments = targetRecipeEnvironments
	profile.targetRecipeShells = targetRecipeShells
	profile.targetVariableScopes = targetVariableScopes
	type selectedRule struct {
		rule         KbuildRule
		ruleIndex    int
		targetOrder  int
		target       string
		lookupTarget string
		stem         string
	}
	type selectedTargetContext struct {
		rules                           []selectedRule
		normal, orderOnly               []string
		automaticNormal, automaticOrder []string
		automaticTarget                 string
		stem                            string
		effectiveRecipes                map[int]bool
		recipeContexts                  map[int]compactKbuildEvaluatedRuleContext
		groupRuleIndex                  int
		groupOutputs                    []string
		groupMakeTargets                map[string]string
		grouped                         bool
	}
	resolveTarget := func(target, makeTarget string) (selectedTargetContext, error) {
		context := selectedTargetContext{groupRuleIndex: -1}
		var selectedRuleIndexes map[int]bool
		selectedRuleOrder := []int{}
		if options.SelectedRuleIndexes != nil {
			selectedRuleIndexes = map[int]bool{}
			for _, ruleIndex := range options.SelectedRuleIndexes(target, makeTarget) {
				if ruleIndex < 0 || ruleIndex >= len(profile.Rules) {
					return context, fmt.Errorf("Kbuild profile %q target %q selected invalid rule index %d", profile.Name, target, ruleIndex)
				}
				selectedRuleIndexes[ruleIndex] = true
				selectedRuleOrder = append(selectedRuleOrder, ruleIndex)
			}
		}
		previousRule := -1
		for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget) {
			if selectedRuleIndexes != nil && !selectedRuleIndexes[candidate.ruleOrder] {
				continue
			}
			// A multi-target declaration participates once in one target context.
			if candidate.ruleOrder == previousRule {
				continue
			}
			previousRule = candidate.ruleOrder
			context.rules = append(context.rules, selectedRule{
				rule: candidate.rule, ruleIndex: candidate.ruleOrder,
				targetOrder: candidate.targetOrder, target: candidate.target,
				lookupTarget: candidate.lookupTarget,
				stem:         candidate.stem,
			})
		}
		if selectedRuleIndexes != nil && len(context.rules) != len(selectedRuleIndexes) {
			return context, fmt.Errorf(
				"Kbuild profile %q target %q selected %d rule indexes but matched %d declarations",
				profile.Name, target, len(selectedRuleIndexes), len(context.rules),
			)
		}
		if len(context.rules) == 0 {
			return context, nil
		}
		if selectedRuleIndexes == nil {
			selectedRuleOrder = make([]int, 0, len(context.rules))
			for _, selected := range context.rules {
				selectedRuleOrder = append(selectedRuleOrder, selected.ruleIndex)
			}
		}
		effectiveRecipes, err := EffectiveCompactKbuildRecipeRuleIndexes(profile, selectedRuleOrder)
		if err != nil {
			return context, err
		}
		context.effectiveRecipes = make(map[int]bool, len(effectiveRecipes))
		context.recipeContexts = make(map[int]compactKbuildEvaluatedRuleContext, len(effectiveRecipes))
		for _, ruleIndex := range effectiveRecipes {
			context.effectiveRecipes[ruleIndex] = true
		}
		selectedByIndex := func(ruleIndex int) (selectedRule, bool) {
			for _, selected := range context.rules {
				if selected.ruleIndex == ruleIndex {
					return selected, true
				}
			}
			return selectedRule{}, false
		}
		appendDetailed := func(detailed compactKbuildEvaluatedRuleContext) {
			if context.automaticTarget == "" {
				context.automaticTarget = detailed.target.makeWord
				context.stem = detailed.stem
			}
			for _, prerequisite := range detailed.normal {
				context.normal = append(context.normal, prerequisite.graphPath)
				context.automaticNormal = append(context.automaticNormal, prerequisite.makeWord)
			}
			for _, prerequisite := range detailed.orderOnly {
				context.orderOnly = append(context.orderOnly, prerequisite.graphPath)
				context.automaticOrder = append(context.automaticOrder, prerequisite.makeWord)
			}
		}
		doubleColon := context.rules[0].rule.Separator == "::"
		if doubleColon {
			for _, selected := range context.rules {
				match := &compactKbuildRuleMatch{
					profile: profile, rule: selected.rule, stem: selected.stem,
					lookupTarget: selected.lookupTarget,
					targetOrder:  selected.targetOrder, ruleOrder: selected.ruleIndex, resolved: true,
				}
				detailed, contextErr := evaluatedKbuildTargetMakeContext(profile, target, match, false)
				if contextErr != nil {
					return context, fmt.Errorf("evaluate Kbuild profile %q target %q double-colon context: %w", profile.Name, target, contextErr)
				}
				appendDetailed(detailed)
				if context.effectiveRecipes[selected.ruleIndex] {
					context.recipeContexts[selected.ruleIndex] = detailed
				}
			}
		} else {
			var selectedMatch *compactKbuildRuleMatch
			var selected selectedRule
			var found bool
			if len(effectiveRecipes) != 0 {
				selected, found = selectedByIndex(effectiveRecipes[len(effectiveRecipes)-1])
				if !found {
					return context, fmt.Errorf("effective Kbuild recipe rule %d is absent from target %q context", effectiveRecipes[len(effectiveRecipes)-1], target)
				}
			} else {
				selected, found = context.rules[0], true
				selectedSpecificity := len(strings.ReplaceAll(selected.target, "%", ""))
				for _, candidate := range context.rules[1:] {
					if specificity := len(strings.ReplaceAll(candidate.target, "%", "")); specificity > selectedSpecificity {
						selected, selectedSpecificity = candidate, specificity
					}
				}
			}
			if found {
				selectedMatch = &compactKbuildRuleMatch{
					profile: profile, rule: selected.rule, stem: selected.stem,
					lookupTarget: selected.lookupTarget,
					targetOrder:  selected.targetOrder, ruleOrder: selected.ruleIndex, resolved: true,
				}
			}
			detailed, contextErr := evaluatedKbuildSelectedTargetMakeContext(profile, target, selectedMatch)
			if contextErr != nil {
				return context, fmt.Errorf("evaluate Kbuild profile %q target %q control prerequisite context: %w", profile.Name, target, contextErr)
			}
			appendDetailed(detailed)
			for _, ruleIndex := range effectiveRecipes {
				context.recipeContexts[ruleIndex] = detailed
			}
		}
		context.groupRuleIndex, context.groupOutputs, context.grouped, err = ResolveCompactKbuildGroupedRule(
			profile, selectedRuleOrder, target, context.stem,
		)
		if err == nil && context.grouped {
			selected, found := selectedByIndex(context.groupRuleIndex)
			if !found {
				return context, fmt.Errorf("grouped Kbuild recipe rule %d is absent from target %q context", context.groupRuleIndex, target)
			}
			context.groupMakeTargets = make(map[string]string, len(selected.rule.Targets))
			for _, pattern := range selected.rule.Targets {
				rawPeer := instantiateKbuildRulePattern(pattern, context.stem)
				makePeer, makeErr := compactKbuildStableMakeWord(profile, rawPeer)
				if makeErr != nil {
					return context, fmt.Errorf("evaluate Kbuild profile %q target %q grouped output %q: %w", profile.Name, target, rawPeer, makeErr)
				}
				graphPeer := compactKbuildGraphTargetPath(compactKbuildProfileTargetPath(profile, rawPeer))
				if graphPeer != "" {
					context.groupMakeTargets[graphPeer] = makePeer
				}
			}
		}
		return context, err
	}
	var visit func(string, string, *compactKbuildTargetVariableScope, bool) error
	visit = func(rawTarget, rawMakeTarget string, inherited *compactKbuildTargetVariableScope, includeSatisfied bool) error {
		target := compactKbuildGraphTargetPath(rawTarget)
		lookupTarget := compactKbuildRuleLookupTarget(profile, target, rawMakeTarget)
		if target == "" || target == "FORCE" || seen[target] {
			return nil
		}
		if !includeSatisfied && options.TargetIsSatisfied != nil && options.TargetIsSatisfied(target) {
			return nil
		}
		if visiting[target] {
			return fmt.Errorf("selected Kbuild control target cycle at %q", target)
		}
		visiting[target] = true
		defer delete(visiting, target)

		context, err := resolveTarget(target, lookupTarget)
		if err != nil {
			return err
		}
		groupKey := ""
		groupOutputs := []string(nil)
		if context.grouped {
			if _, _, trigger, outputs, exists := CompactKbuildGroupedActionForTarget(profile, target); exists {
				if err := BindCompactKbuildGroupedAction(&profile, context.groupRuleIndex, context.stem, trigger, context.groupOutputs); err != nil {
					return err
				}
				groupOutputs = outputs
				if target != trigger {
					triggerMakeTarget := context.groupMakeTargets[trigger]
					if triggerMakeTarget == "" {
						triggerMakeTarget = trigger
					}
					return visit(trigger, triggerMakeTarget, inherited, true)
				}
			} else {
				if err := BindCompactKbuildGroupedAction(&profile, context.groupRuleIndex, context.stem, target, context.groupOutputs); err != nil {
					return err
				}
				groupOutputs = context.groupOutputs
			}
			groupKey = fmt.Sprintf("%d\x00%s", context.groupRuleIndex, context.stem)
			if groupDone[groupKey] {
				return nil
			}
			if groupVisiting[groupKey] {
				return nil
			}
			groupVisiting[groupKey] = true
			defer delete(groupVisiting, groupKey)
		}
		targetVariableScopes[target] = inherited
		inheritedByPrerequisites := inherited
		if variables := compactKbuildInheritableTargetVariablesForMakeTarget(profile, target, lookupTarget); len(variables) != 0 {
			inheritedByPrerequisites = &compactKbuildTargetVariableScope{
				parent: inherited, variables: variables,
			}
		}
		visitPrerequisites := func(
			owner string,
			prerequisites, makeWords []string,
			scope *compactKbuildTargetVariableScope,
		) error {
			for index, prerequisite := range prerequisites {
				makeWord := ""
				if index < len(makeWords) {
					makeWord = makeWords[index]
				}
				// A rooted prerequisite can have the same tree-relative path as
				// the local output without being the same file. External modpost,
				// for example, writes its own Module.symvers while reading
				// $(objtree)/Module.symvers from the configured kernel SDK. The
				// compact producer key intentionally omits the tree namespace, so
				// do not turn that cross-tree input into a local self-cycle.
				if prerequisite == compactKbuildGraphTargetPath(owner) &&
					compactKbuildEvaluatedPathNamespace(makeWord) != "" {
					continue
				}
				if err := visit(prerequisite, makeWord, scope, false); err != nil {
					return err
				}
			}
			return nil
		}
		if len(context.rules) != 0 {
			if err := visitPrerequisites(
				target,
				append(context.normal, context.orderOnly...),
				append(context.automaticNormal, context.automaticOrder...),
				inheritedByPrerequisites,
			); err != nil {
				return err
			}
		}
		for _, peer := range groupOutputs {
			if peer == target {
				continue
			}
			peerMakeTarget := context.groupMakeTargets[peer]
			if peerMakeTarget == "" {
				peerMakeTarget = peer
			}
			peerContext, peerErr := resolveTarget(peer, peerMakeTarget)
			if peerErr != nil {
				return peerErr
			}
			if !peerContext.grouped || peerContext.groupRuleIndex != context.groupRuleIndex || peerContext.stem != context.stem {
				return fmt.Errorf("grouped Kbuild peer %q resolves to a different effective recipe than trigger %q", peer, target)
			}
			if err := BindCompactKbuildGroupedAction(&profile, peerContext.groupRuleIndex, peerContext.stem, target, peerContext.groupOutputs); err != nil {
				return err
			}
			// GNU Make updates every peer's merged prerequisites as part of the
			// triggered action even if that peer is an immutable source file. Those
			// prerequisites inherit the trigger's target-specific context; peer-only
			// variables and prerequisites never enter the trigger recipe's automatics.
			if err := visitPrerequisites(
				peer,
				append(peerContext.normal, peerContext.orderOnly...),
				append(peerContext.automaticNormal, peerContext.automaticOrder...),
				inheritedByPrerequisites,
			); err != nil {
				return err
			}
		}
		for _, selected := range context.rules {
			if !context.effectiveRecipes[selected.ruleIndex] {
				continue
			}
			recipeContext := context.recipeContexts[selected.ruleIndex]
			recipeNormal := make([]string, 0, len(recipeContext.normal))
			for _, prerequisite := range recipeContext.normal {
				recipeNormal = append(recipeNormal, prerequisite.makeWord)
			}
			recipeOrderOnly := make([]string, 0, len(recipeContext.orderOnly))
			for _, prerequisite := range recipeContext.orderOnly {
				recipeOrderOnly = append(recipeOrderOnly, prerequisite.makeWord)
			}
			effectProfile := profile
			effectProfile.evaluator = &kbuildTargetEvaluator{template: cumulative}
			effectProfile.targetEvaluators = nil
			parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
				effectProfile, target, selected.lookupTarget, recipeContext.target.makeWord, recipeContext.stem,
				recipeNormal, recipeOrderOnly, nil, true, true,
			)
			if err != nil {
				return err
			}
			var activeLineProfile CompactKbuildProfile
			var activeLineEnvironment map[string]string
			var activeLineCommandShell string
			// Registering a deferred query mutates the action graph. A cached
			// compiler-shell proof from the captured parser must not make this
			// replacement callback safe to visit speculatively.
			parser.shellResultAvailable = nil
			parser.shell = func(command string) (string, error) {
				// Tool selection for the query belongs to the exact Make state at
				// expansion time. Snapshot it: later eval lines may change exported
				// variables, but cannot retroactively change this command.
				queryProfile := activeLineProfile
				if queryProfile.evaluator == nil {
					return "", fmt.Errorf("Kbuild profile %q target %q deferred query has no active recipe snapshot", profile.Name, target)
				}
				queryProfile.targetEvaluators = nil
				queryProfile.targetProbeEnvironmentActivations = nil
				return registerQuery(target, command, activeLineCommandShell, activeLineEnvironment, queryProfile)
			}
			for recipeIndex, rawLine := range selected.rule.Recipe {
				lineSnapshot := snapshot()
				environment, environmentErr := evaluateKbuildControlTargetEnvironmentForMakeTarget(
					lineSnapshot.Profile, target, selected.lookupTarget, recipeContext.target.makeWord, recipeContext.stem,
					recipeNormal, recipeOrderOnly, nil, false,
				)
				if environmentErr != nil {
					cleanup()
					return fmt.Errorf(
						"Kbuild profile %q target %q recipe %d environment: %w",
						profile.Name, target, recipeIndex, environmentErr,
					)
				}
				commandValues, commandValuesErr := evaluateKbuildControlTargetForMakeTarget(
					lineSnapshot.Profile, target, selected.lookupTarget, recipeContext.target.makeWord, recipeContext.stem,
					recipeNormal, recipeOrderOnly, nil, false, "CONFIG_SHELL",
				)
				if commandValuesErr != nil {
					cleanup()
					return fmt.Errorf(
						"Kbuild profile %q target %q recipe %d CONFIG_SHELL: %w",
						profile.Name, target, recipeIndex, commandValuesErr,
					)
				}
				commandShell := commandValues["CONFIG_SHELL"]
				var activateProbeEnvironment func() error
				if options.BindProbeEnvironment != nil {
					activateProbeEnvironment, environmentErr = options.BindProbeEnvironment(environment)
					if environmentErr != nil {
						cleanup()
						return fmt.Errorf(
							"bind Kbuild profile %q target %q recipe %d probe environment: %w",
							profile.Name, target, recipeIndex, environmentErr,
						)
					}
					if activateProbeEnvironment == nil {
						cleanup()
						return fmt.Errorf(
							"bind Kbuild profile %q target %q recipe %d probe environment returned nil activation",
							profile.Name, target, recipeIndex,
						)
					}
					lineSnapshot.Profile.probeEnvironmentActivation = activateProbeEnvironment
					if environmentErr = activateProbeEnvironment(); environmentErr != nil {
						cleanup()
						return fmt.Errorf(
							"activate Kbuild profile %q target %q recipe %d probe environment: %w",
							profile.Name, target, recipeIndex, environmentErr,
						)
					}
				}
				activeLineProfile = lineSnapshot.Profile
				activeLineEnvironment = environment
				activeLineCommandShell = commandShell
				result.recipeSnapshots[kbuildControlRecipeKey{
					target: target, ruleIndex: selected.ruleIndex, recipeIndex: recipeIndex,
				}] = lineSnapshot
				body, isEval, err := exactKbuildEvalRecipeBody(rawLine)
				if err != nil {
					cleanup()
					return fmt.Errorf("Kbuild profile %q target %q: %w", profile.Name, target, err)
				}
				if !isEval {
					generation := uint64(len(lineSnapshot.Effects))
					environmentSnapshot, environmentErr := targetRecipeEnvironmentInterner.intern(environment)
					if environmentErr != nil {
						cleanup()
						return fmt.Errorf(
							"Kbuild profile %q target %q recipe %d exported environment: %w",
							profile.Name, target, recipeIndex, environmentErr,
						)
					}
					if previous, exists := targetControlGenerations[target]; exists && previous != generation {
						cleanup()
						return fmt.Errorf(
							"Kbuild profile %q target %q has executable recipe lines across control-state generations %d and %d",
							profile.Name, target, previous, generation,
						)
					}
					if previous, exists := targetRecipeEnvironments[target]; exists && previous != environmentSnapshot {
						cleanup()
						return fmt.Errorf(
							"Kbuild profile %q target %q has executable recipe lines with different exported environments",
							profile.Name, target,
						)
					}
					if previous, exists := targetRecipeShells[target]; exists && previous != commandShell {
						cleanup()
						return fmt.Errorf(
							"Kbuild profile %q target %q has executable recipe lines with different CONFIG_SHELL values",
							profile.Name, target,
						)
					}
					targetControlGenerations[target] = generation
					targetRecipeEnvironments[target] = environmentSnapshot
					targetRecipeShells[target] = commandShell
					targetEvaluators[target] = lineSnapshot.Profile.evaluator
					if activateProbeEnvironment != nil {
						targetProbeEnvironmentActivations[target] = activateProbeEnvironment
					}
					continue
				}
				// During eval's first expansion, $$ becomes one literal dollar for
				// the nested shell/second parse. The ordinary parser intentionally
				// does not assume this recipe-only context, so bind it explicitly.
				parser.pushLocal(map[string]string{"$": "$"})
				expanded, err := parser.expandDepth(body, 0)
				parser.popLocal()
				if err != nil {
					cleanup()
					return fmt.Errorf("Kbuild profile %q target %q eval expansion: %w", profile.Name, target, err)
				}
				for _, assignment := range strings.Split(expanded, "\n") {
					assignment = strings.TrimSpace(assignment)
					if assignment == "" {
						continue
					}
					variable, operator, _, ok := splitKbuildAssignment(assignment)
					if !ok || variable == "" || containsMakeReference(variable) {
						cleanup()
						return fmt.Errorf("Kbuild profile %q target %q eval result is not an ordinary assignment: %q", profile.Name, target, assignment)
					}
					if err := parser.parseLine(assignment, selected.rule.Position); err != nil {
						cleanup()
						return fmt.Errorf("Kbuild profile %q target %q eval assignment %q: %w", profile.Name, target, assignment, err)
					}
					value, ok := parser.lookupVariable(variable)
					if !ok {
						cleanup()
						return fmt.Errorf("Kbuild profile %q target %q eval assignment did not define %q", profile.Name, target, variable)
					}
					// Copy the exact post-assignment variable into the cumulative
					// global state. Target-specific context influenced expansion, but
					// does not itself escape the selected recipe.
					copyKbuildControlVariableState(cumulative, parser, variable, value)
					generationEvaluator = nil
					flavor := "simple"
					if value.recursive {
						flavor = "recursive"
					}
					result.Effects = append(result.Effects, KbuildControlEffect{
						Target: target, Assignment: assignment, Variable: variable,
						Operator: operator, Flavor: flavor,
					})
				}
			}
			cleanup()
		}
		seen[target] = true
		for _, peer := range groupOutputs {
			seen[peer] = true
		}
		if groupKey != "" {
			groupDone[groupKey] = true
		}
		return nil
	}
	for _, target := range profile.EntryTargets {
		if err := visit(target, target, nil, false); err != nil {
			return KbuildControlEvaluation{}, err
		}
	}
	for _, token := range queryOrder {
		result.Queries = append(result.Queries, queries[token])
	}
	finalSnapshot := snapshot()
	result.Profile.evaluator = finalSnapshot.Profile.evaluator
	result.Profile.controlGeneration = finalSnapshot.Profile.controlGeneration
	result.Profile.groupedActions = profile.groupedActions
	result.Profile.targetVariableScopes = profile.targetVariableScopes
	result.Profile.targetEvaluators = targetEvaluators
	result.Profile.targetProbeEnvironmentActivations = targetProbeEnvironmentActivations
	result.Profile.targetControlGenerations = targetControlGenerations
	result.Profile.targetRecipeEnvironments = targetRecipeEnvironments
	result.Profile.targetRecipeShells = targetRecipeShells
	result.Profile.probeEnvironmentActivation = finalSnapshot.Profile.probeEnvironmentActivation
	if options.BindProbeEnvironment != nil {
		environment, err := ExportedKbuildControlVariables(finalSnapshot)
		if err != nil {
			return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q final probe environment: %w", profile.Name, err)
		}
		activate, err := options.BindProbeEnvironment(environment)
		if err != nil {
			return KbuildControlEvaluation{}, fmt.Errorf("bind Kbuild profile %q final probe environment: %w", profile.Name, err)
		}
		if activate == nil {
			return KbuildControlEvaluation{}, fmt.Errorf("bind Kbuild profile %q final probe environment returned nil activation", profile.Name)
		}
		if err := activate(); err != nil {
			return KbuildControlEvaluation{}, fmt.Errorf("activate Kbuild profile %q final probe environment: %w", profile.Name, err)
		}
		result.Profile.probeEnvironmentActivation = activate
	}
	result.Profile.deferredContentQueries = maps.Clone(queries)
	return result, nil
}

// compactKbuildInheritableTargetVariablesForMakeTarget applies GNU Make's
// final-binding privacy rule to the lexical target spelling used for pattern-
// specific variables. The returned program is stored in a canonical graph-
// target scope, but its local declaration lookup must not path-clean away a
// parent-traversal alias before matching $(obj)/%.
func compactKbuildInheritableTargetVariablesForMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) []KbuildTargetVariable {
	variables := compactKbuildTargetVariablesForMakeTarget(profile, target, makeTarget)
	if len(variables) == 0 {
		return nil
	}
	private := make(map[string]bool, len(variables))
	for _, variable := range variables {
		private[variable.Variable] = slices.Contains(variable.Modifiers, "private")
	}
	inherited := make([]KbuildTargetVariable, 0, len(variables))
	for _, variable := range variables {
		if !private[variable.Variable] {
			inherited = append(inherited, variable)
		}
	}
	return inherited
}

func evaluateKbuildControlTargetForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
	names ...string,
) (map[string]string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, true, false,
	)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	values := make(map[string]string, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("Kbuild target %q requested an empty variable", target)
		}
		value, ok, err := parser.expandVariable(name, "$("+name+")", 0)
		if err != nil {
			return nil, fmt.Errorf("Kbuild target %q variable %s: %w", target, name, err)
		}
		if !ok {
			value = ""
		}
		if resolveSymbolic {
			value, err = parser.resolveKbuildSymbolic(value)
			if err != nil {
				return nil, fmt.Errorf("Kbuild target %q variable %s symbolic result: %w", target, name, err)
			}
		}
		values[name] = value
	}
	return values, nil
}

func evaluateKbuildControlTargetEnvironmentForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
) (map[string]string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, true, true,
	)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	names := make([]string, 0, len(parser.exported))
	for name, exported := range parser.exported {
		if exported {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, ok, err := parser.expandVariable(name, "$("+name+")", 0)
		if err != nil {
			return nil, fmt.Errorf("Kbuild target %q exported variable %s: %w", target, name, err)
		}
		if !ok {
			value = ""
		}
		if resolveSymbolic {
			value, err = parser.resolveKbuildSymbolic(value)
			if err != nil {
				return nil, fmt.Errorf("Kbuild target %q exported variable %s symbolic result: %w", target, name, err)
			}
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("Kbuild target %q exported variable %s contains NUL", target, name)
		}
		values[name] = value
	}
	return values, nil
}

func copyKbuildControlVariableState(
	destination *kbuildParser,
	source *kbuildParser,
	name string,
	value kbuildVariable,
) {
	destination.setVariable(name, value)
	if source.undefined[name] {
		destination.undefineVariable(name)
	}
	if symbolic, ok := source.symbolicVariables[name]; ok {
		destination.symbolicVariables[name] = symbolic
	} else {
		delete(destination.symbolicVariables, name)
	}
	for _, state := range []struct {
		source      map[string]bool
		destination map[string]bool
	}{
		{source: source.exported, destination: destination.exported},
		{source: source.environmentVariables, destination: destination.environmentVariables},
		{source: source.commandLineVariables, destination: destination.commandLineVariables},
	} {
		if state.source[name] {
			state.destination[name] = true
		} else {
			delete(state.destination, name)
		}
	}
}

// ExportedKbuildControlVariables expands the environment GNU Make would pass
// to a recursive child after the selected control recipes have run. The
// returned values retain both deferred-content tokens and compiler-probe
// atoms. Recursive child invocations use those atoms to form exact dependent
// requests; resolving them here would make replay construct a different DAG
// from discovery. Concrete action fields resolve them later.
func ExportedKbuildControlVariables(evaluation KbuildControlEvaluation) (map[string]string, error) {
	if evaluation.Profile.evaluator == nil || evaluation.Profile.evaluator.template == nil {
		return nil, fmt.Errorf("Kbuild control evaluation has no source-derived target evaluator")
	}
	parser := cloneKbuildParserForEvaluation(evaluation.Profile.evaluator.template)
	names := make([]string, 0, len(parser.exported))
	for name, exported := range parser.exported {
		if exported {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, ok, err := parser.expandVariable(name, "$("+name+")", 0)
		if err != nil {
			return nil, fmt.Errorf("expand post-control exported Kbuild variable %s: %w", name, err)
		}
		if !ok {
			value = ""
		}
		values[name] = value
	}
	return values, nil
}

// AttachKbuildDeferredContentQueries carries execution provenance from an
// explicitly selected parent invocation to one child parsed with the parent's
// post-control exported values. It deliberately does not infer lineage from a
// profile name or path; the invocation walker owns that relationship.
func AttachKbuildDeferredContentQueries(profile CompactKbuildProfile, evaluation KbuildControlEvaluation) (CompactKbuildProfile, error) {
	queries := maps.Clone(profile.deferredContentQueries)
	if queries == nil {
		queries = map[string]KbuildDeferredContentQuery{}
	}
	for _, query := range evaluation.Queries {
		if query.Token == "" || query.Command == "" || !strings.HasPrefix(query.Token, kbuildDeferredContentTokenPrefix) {
			return CompactKbuildProfile{}, fmt.Errorf("Kbuild control evaluation contains an incomplete deferred query")
		}
		query, err := normalizedKbuildDeferredContentQuery(query)
		if err != nil {
			return CompactKbuildProfile{}, err
		}
		if previous, ok := queries[query.Token]; ok {
			previous, err = normalizedKbuildDeferredContentQuery(previous)
			if err != nil {
				return CompactKbuildProfile{}, err
			}
			if !sameKbuildDeferredContentQuerySource(previous, query) {
				return CompactKbuildProfile{}, fmt.Errorf("deferred Kbuild content token %q has conflicting provenance", query.Token)
			}
		}
		queries[query.Token] = query
	}
	profile.deferredContentQueries = queries
	return profile, nil
}

func exactKbuildEvalRecipeBody(line string) (string, bool, error) {
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "@+-")
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "$(eval") && !strings.Contains(line, "${eval") {
		return "", false, nil
	}
	if len(line) < 4 || line[0] != '$' || (line[1] != '(' && line[1] != '{') {
		return "", false, fmt.Errorf("eval is embedded in unsupported recipe text %q", line)
	}
	end, err := matchingKbuildReference(line, 1)
	if err != nil {
		return "", false, err
	}
	if end != len(line)-1 {
		return "", false, fmt.Errorf("eval is embedded in unsupported recipe text %q", line)
	}
	name, args, ok := splitMakeFunction(line[2:end])
	if !ok || name != "eval" || len(args) != 1 {
		return "", false, fmt.Errorf("unsupported eval recipe %q", line)
	}
	return args[0], true, nil
}
