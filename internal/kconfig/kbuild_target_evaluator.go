package kconfig

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const kbuildActionRoleTokenPrefix = "__LINUX_BZL_ACTION_ROLE_"

// KbuildActionRoleAutoScope marks a source-carried tool role whose concrete
// identity follows the action scope selected from the Kbuild dependency
// closure. It deliberately carries no host/target selection signal itself.
const KbuildActionRoleAutoScope = "auto"

// KbuildActionRoleRef identifies one configured action without relying on its
// executable spelling. Scope is normally explicit because native builds may
// use the same artifact for host and target actions; source values shared by
// both manifests use the neutral auto scope until their closure is selected.
type KbuildActionRoleRef struct {
	Scope string
	Role  string
}

// KbuildActionRoleToken returns the inert GNU Make value used at source parse
// time.  It is data for the evaluator and probe grammar; no planner process
// executes or resolves it as a path.
func KbuildActionRoleToken(scope, role string) string {
	return kbuildActionRoleTokenPrefix + scope + "_" + role + "__"
}

func validKbuildActionRolePart(value string) bool {
	return toolaction.ValidRole(value)
}

func parseKbuildActionRoleToken(value string) (KbuildActionRoleRef, bool) {
	if !strings.HasPrefix(value, kbuildActionRoleTokenPrefix) || !strings.HasSuffix(value, "__") {
		return KbuildActionRoleRef{}, false
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(value, kbuildActionRoleTokenPrefix), "__")
	for _, scope := range []string{"target", "host", KbuildActionRoleAutoScope} {
		role, ok := strings.CutPrefix(payload, scope+"_")
		if ok && validKbuildActionRolePart(role) {
			return KbuildActionRoleRef{Scope: scope, Role: role}, true
		}
	}
	return KbuildActionRoleRef{}, false
}

// kbuildActionRoleRefForScope resolves a scope-neutral role reference against
// one already-selected concrete action scope. Explicitly scoped references
// continue to fail closed when they disagree with that scope.
func kbuildActionRoleRefForScope(ref KbuildActionRoleRef, expectedScope string) (KbuildActionRoleRef, bool) {
	if ref.Scope == KbuildActionRoleAutoScope {
		ref.Scope = expectedScope
	}
	return ref, ref.Scope == expectedScope
}

// kbuildActionRoleBinding resolves one source-carried role to the canonical
// binding name used by an action in expectedScope. Same-scope roles retain
// their ordinary Kbuild name; an explicitly opposite-scope role is qualified
// so both configured toolsets can coexist in one argv without collisions.
// Auto-scoped roles always follow the selected action scope.
func kbuildActionRoleBinding(ref KbuildActionRoleRef, expectedScope string) (KbuildActionRoleRef, string, bool) {
	if ref.Scope == KbuildActionRoleAutoScope {
		ref.Scope = expectedScope
	}
	if (ref.Scope != "target" && ref.Scope != "host") || !validKbuildActionRolePart(ref.Role) {
		return KbuildActionRoleRef{}, "", false
	}
	binding := ref.Role
	if ref.Scope != expectedScope {
		var ok bool
		binding, ok = toolaction.ScopedBinding(ref.Scope, ref.Role)
		if !ok {
			return KbuildActionRoleRef{}, "", false
		}
	}
	return ref, binding, true
}

// KbuildActionRoleRefs returns every source-traced role token embedded in text
// and fails closed if the reserved prefix does not form a canonical token.
func KbuildActionRoleRefs(value string) ([]KbuildActionRoleRef, error) {
	seen := map[KbuildActionRoleRef]bool{}
	for offset := 0; ; {
		start := strings.Index(value[offset:], kbuildActionRoleTokenPrefix)
		if start < 0 {
			break
		}
		start += offset
		end := strings.Index(value[start+len(kbuildActionRoleTokenPrefix):], "__")
		if end < 0 {
			return nil, fmt.Errorf("unterminated configured action-role token")
		}
		end += start + len(kbuildActionRoleTokenPrefix) + 2
		token := value[start:end]
		ref, ok := parseKbuildActionRoleToken(token)
		if !ok {
			return nil, fmt.Errorf("invalid configured action-role token %q", token)
		}
		seen[ref] = true
		offset = end
	}
	refs := make([]KbuildActionRoleRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Scope != refs[j].Scope {
			return refs[i].Scope < refs[j].Scope
		}
		return refs[i].Role < refs[j].Role
	})
	return refs, nil
}

func rewriteKbuildActionRoleRefs(
	value, expectedScope string,
	configured []KbuildActionRoleRef,
	toolPlaceholder bool,
) (string, []string, error) {
	refs, err := KbuildActionRoleRefs(value)
	if err != nil {
		return "", nil, err
	}
	roles := make([]string, 0, len(refs))
	for _, sourceRef := range refs {
		ref, binding, valid := kbuildActionRoleBinding(sourceRef, expectedScope)
		if !valid {
			return "", nil, fmt.Errorf("%s-scoped value retains invalid %s action role %q", expectedScope, sourceRef.Scope, sourceRef.Role)
		}
		if !slices.Contains(configured, ref) {
			return "", nil, fmt.Errorf("%s-scoped value references unconfigured %s action role %q", expectedScope, ref.Scope, ref.Role)
		}
		replacement := binding
		if toolPlaceholder {
			replacement = "${tool:" + binding + "}"
		}
		value = strings.ReplaceAll(value, KbuildActionRoleToken(sourceRef.Scope, sourceRef.Role), replacement)
		roles = append(roles, binding)
	}
	return value, roles, nil
}

// kbuildTargetEvaluator retains the parsed Make variable definitions and the
// selected capability callbacks for one concrete invocation. It is deliberately
// process-local: action-plan construction retains only the evaluated commands that
// become actions, never the entire Make environment.
type kbuildTargetEvaluator struct {
	template *kbuildParser

	invocationDirectoryOnce sync.Once
	invocationDirectory     string
	invocationDirectoryOK   bool

	canonicalRecipeRootsOnce sync.Once
	canonicalRecipeRoots     []compactKbuildCanonicalRecipeRoot

	// A parser can be bound to more than one invocation directory while the
	// recursive Make graph is assembled.  Cache the immutable declaration index
	// by that final profile shape; copies of one profile still share one bounded
	// runtime without letting an earlier directory binding poison later paths.
	plannerMu       sync.Mutex
	plannerRuntimes map[compactKbuildPlannerRuntimeKey]*compactKbuildPlannerRuntime

	// commandMemo owns successful source-rule command resolutions for this
	// immutable evaluator. The key also carries the activated probe environment
	// and profile-local control provenance, so source-order environment changes
	// cannot reuse a result from another compiler setup.
	commandMemoMu     sync.Mutex
	commandMemo       map[kbuildRuleCommandMemoKey]kbuildRuleCommandMemoValue
	commandMemoHits   uint64
	commandMemoMisses uint64

	pathScopeMu    sync.Mutex
	sourceAncestry map[string]int
}

func newKbuildTargetEvaluator(parser *kbuildParser) *kbuildTargetEvaluator {
	if parser == nil {
		return nil
	}
	return &kbuildTargetEvaluator{template: parser}
}

// EvaluateCompactKbuildTarget expands named Make variables in one concrete
// target context. Matching target-specific and pattern-specific assignments
// are applied in declaration order, and GNU Make automatic variables are bound
// from the selected rule. injected contains invocation facts such as modname or
// part-of-module; it must never contain inferred compiler policy.
func EvaluateCompactKbuildTarget(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	names ...string,
) (map[string]string, error) {
	return evaluateCompactKbuildTarget(profile, target, stem, normal, orderOnly, injected, true, names...)
}

// EvaluateCompactKbuildTargetSymbolic expands target variables without
// reducing compiler-probe atoms. Graph discovery uses this form so replay
// reconstructs the same dependent probe DAG as discovery; concrete action
// lowering uses EvaluateCompactKbuildTarget after the oracle is available.
func EvaluateCompactKbuildTargetSymbolic(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	names ...string,
) (map[string]string, error) {
	return evaluateCompactKbuildTarget(profile, target, stem, normal, orderOnly, injected, false, names...)
}

func evaluateCompactKbuildTarget(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
	names ...string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetForAutomaticTarget(
		profile, target, target, stem, normal, orderOnly, injected, resolveSymbolic, names...,
	)
}

// evaluateCompactKbuildTargetForAutomaticTarget keeps the canonical graph
// identity used for target-specific assignment lookup separate from GNU
// Make's physical automatic-variable spellings. The action lowerer uses this
// distinction after it has resolved every prerequisite to immutable source or
// generated-object provenance; graph selection continues to use the logical
// target identity through evaluateCompactKbuildTarget above.
func evaluateCompactKbuildTargetForAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
	names ...string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly,
		injected, resolveSymbolic, names...,
	)
}

func evaluateCompactKbuildTargetForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
	names ...string,
) (map[string]string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, false,
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
			// GNU Make expands an undefined variable to the empty string. Preserve
			// that behavior so callers can request optional target flag families
			// without consulting a serialized variable snapshot first.
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

func evaluateCompactKbuildRecipeVariablesForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	names ...string,
) (map[string]string, error) {
	return evaluateCompactKbuildRecipeVariablesForAutomaticTargetMode(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, names...,
	)
}

func evaluateCompactKbuildRecipeVariablesForAutomaticTargetMode(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
	names ...string,
) (map[string]string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, false,
	)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	preserveUnsupportedKbuildRecipeShell(parser, profile, target)

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

func preserveUnsupportedKbuildRecipeShell(
	parser *kbuildParser,
	profile CompactKbuildProfile,
	target string,
) {
	if parser == nil {
		return
	}
	original := parser.shell
	originalSource := parser.sourceShell
	parser.shell = func(command string) (string, error) {
		if original != nil {
			value, err := original(command)
			if err == nil {
				return value, nil
			}
			if IsLinuxProbeUnsupportedCommand(err) {
				// Let kbuildParser.evalShell offer the command to the bounded
				// immutable-source query grammar before classifying it as
				// execution-time recipe work.
				return "", err
			}
			if !IsLinuxProbeDeferredRecipeCommand(err) {
				return "", err
			}
		} else {
			return "", &LinuxProbeUnsupportedCommandError{Command: command}
		}
		return registerKbuildDeferredContentQuery(
			profile, target, command, ActionRecipeContentTransformMakeShellSingleWord, "", nil,
		)
	}
	parser.sourceShell = func(command, workingDirectory string) (string, error) {
		if originalSource != nil {
			value, err := originalSource(command, workingDirectory)
			if err == nil {
				return value, nil
			}
			if !IsLinuxProbeUnsupportedCommand(err) {
				return "", err
			}
		}
		return registerKbuildDeferredContentQuery(
			profile, target, command, ActionRecipeContentTransformMakeShellSingleWord, "", nil,
		)
	}
}

// EvaluateCompactKbuildText expands one source-derived Make expression in the
// exact target context. It is used to interpret the argv of a selected
// recursive Make recipe; the resulting invocation, not a filename table,
// determines which child driver and concrete goal receive a named profile.
//
// Recipe discovery must not execute recipe-side $(shell ...) expressions. The
// source parser may use a hermetic shell evaluator while reading Makefiles, but
// replaying that callback here would turn action discovery into action
// execution (for example, Linux' cpufeaturemasks recipe creates its output
// directory with $(shell mkdir ...)). Disable the callback before applying
// target-specific variables as well as before expanding the recipe itself.
func EvaluateCompactKbuildText(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, error) {
	return evaluateCompactKbuildText(profile, target, stem, normal, orderOnly, injected, expression, true)
}

// EvaluateCompactKbuildTextSymbolic retains compiler-probe atoms while
// interpreting source-selected recursive Make argv.
func EvaluateCompactKbuildTextSymbolic(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, error) {
	return evaluateCompactKbuildText(profile, target, stem, normal, orderOnly, injected, expression, false)
}

// EvaluateCompactKbuildTextSymbolicForAutomaticTarget evaluates target-specific
// variables against the canonical target while binding GNU Make automatic
// variables to the declaration-local spelling used by that invocation.
func EvaluateCompactKbuildTextSymbolicForAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget, stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, error) {
	return evaluateCompactKbuildTextWithAutomaticTarget(
		profile, target, automaticTarget, stem, normal, orderOnly, injected, expression, false,
	)
}

// EvaluateCompactKbuildTextSymbolicForMakeTarget expands text with separate
// canonical, implicit-rule lookup, and automatic-target identities.
func EvaluateCompactKbuildTextSymbolicForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, error) {
	return evaluateCompactKbuildTextForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, expression, false,
	)
}

// EvaluateCompactKbuildTextActionRolesForAutomaticTarget expands one selected
// recipe from the source-time traced evaluator and returns the exact embedded
// configured actions.  In particular, a simple assignment such as
// HOST_OVERRIDES := CC="$(HOSTCC)" remains host-scoped even when HOSTCC and CC
// name the same executable artifact.
func EvaluateCompactKbuildTextActionRolesForAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget, stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, []KbuildActionRoleRef, error) {
	return evaluateCompactKbuildTextActionRolesForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly, injected, expression,
	)
}

// EvaluateCompactKbuildTextActionRolesForMakeTarget is the lexical lookup
// counterpart of EvaluateCompactKbuildTextActionRolesForAutomaticTarget.
func EvaluateCompactKbuildTextActionRolesForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, []KbuildActionRoleRef, error) {
	return evaluateCompactKbuildTextActionRolesForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, expression,
	)
}

func evaluateCompactKbuildTextActionRolesForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
) (string, []KbuildActionRoleRef, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, false,
	)
	if err != nil {
		return "", nil, err
	}
	defer cleanup()
	var refs []KbuildActionRoleRef
	collectRefs := func(value, context string) error {
		found, err := KbuildActionRoleRefs(value)
		if err != nil {
			return fmt.Errorf("%s action-role provenance: %w", context, err)
		}
		refs = append(refs, found...)
		return nil
	}
	// A probe-dependent Make selection is represented by one opaque planner
	// token during discovery. Its selected branch becomes concrete only during
	// replay, but map_directory must declare the configured tools required by
	// every possible branch before that replay runs. Observe the selector's
	// source operands and branches here, where their action-role tokens are
	// still visible, rather than coupling provenance discovery to the probe
	// symbol registry's internal representation.
	if selectSymbolic := parser.selectSymbolic; selectSymbolic != nil {
		parser.selectSymbolic = func(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
			for _, candidate := range []struct {
				name  string
				value string
			}{
				{name: "comparison value", value: value},
				{name: "comparison expected value", value: expected},
				{name: "true branch", value: trueText},
				{name: "false branch", value: falseText},
			} {
				if err := collectRefs(candidate.value, fmt.Sprintf("Kbuild target %q symbolic selection %s", target, candidate.name)); err != nil {
					return "", true, err
				}
			}
			return selectSymbolic(value, expected, equal, trueText, falseText)
		}
	}
	// Recipe-side $(shell ...) is an action, not something graph selection may
	// execute. Expand its command solely to retain role provenance, then model
	// its textual result as empty for the structural recursive-Make scan.
	observedShellCommands := map[string]bool{}
	parser.shell = func(command string) (string, error) {
		if err := collectRefs(command, fmt.Sprintf("Kbuild target %q shell", target)); err != nil {
			return "", err
		}
		observedShellCommands[command] = true
		return "", nil
	}
	// Re-expanding an exact command already visited through the callback above
	// only repeats an idempotent role union and returns the same empty value.
	// An unseen command remains stateful: it may live only in an unselected
	// branch, so evaluating it speculatively would violate GNU Make laziness.
	parser.shellResultAvailable = func(command string) bool {
		return observedShellCommands[command]
	}
	value, err := parser.expand(expression)
	if err != nil {
		return "", nil, fmt.Errorf("Kbuild target %q expression: %w", target, err)
	}
	if err := collectRefs(value, fmt.Sprintf("Kbuild target %q", target)); err != nil {
		return "", nil, err
	}
	return value, canonicalKbuildActionRoleRefs(refs), nil
}

// CompactKbuildSelectedTargetEffects is the execution provenance hidden in one
// materialized target's exact command and projected process environment.
// DeferredContentQueries remain source snapshots; callers inspect their typed
// object-tree operands but never execute them during graph discovery.
type CompactKbuildSelectedTargetEffects struct {
	ActionRoles []KbuildActionRoleRef
	// PrimaryActionRoles are the configured command-head roles selected by the
	// exact recipe. They choose the action's execution scope independently from
	// opposite-scope tools passed through argv or the environment.
	PrimaryActionRoles     []KbuildActionRoleRef
	DeferredContentQueries []KbuildDeferredContentQuery
}

// EvaluateCompactKbuildSelectedTargetEffects classifies one materialized
// target from the exact rule and active command templates used by action-plan
// lowering. Pattern prerequisites, merged explicit declarations, automatic
// variables, target-specific variables, projected exports, and symbolic probe
// results therefore cannot select a different tool scope or generated-file
// frontier during graph discovery than during materialization.
func EvaluateCompactKbuildSelectedTargetEffects(
	profile CompactKbuildProfile,
	target string,
) (CompactKbuildSelectedTargetEffects, bool, error) {
	return EvaluateCompactKbuildSelectedTargetEffectsForMakeTarget(profile, target, target)
}

// EvaluateCompactKbuildSelectedTargetEffectsForMakeTarget classifies a
// selected action while preserving the lexical filename GNU Make used for its
// implicit-rule search and target-local variable scope.
func EvaluateCompactKbuildSelectedTargetEffectsForMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) (CompactKbuildSelectedTargetEffects, bool, error) {
	resolved, err := ResolveCompactKbuildTargetForMakeTarget(profile, target, makeTarget)
	if err != nil {
		return CompactKbuildSelectedTargetEffects{}, false, err
	}
	return resolved.SelectedTargetEffects()
}

// SelectedTargetEffects classifies the selected action retained by r without
// repeating target resolution. Returned slices are owned by the caller.
func (r *CompactKbuildResolvedTarget) SelectedTargetEffects() (CompactKbuildSelectedTargetEffects, bool, error) {
	if r == nil {
		return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf("nil resolved Kbuild target")
	}
	if r.effects == nil {
		effects, selected, err := r.selectedTargetEffectsUncached()
		return cloneCompactKbuildSelectedTargetEffects(effects), selected, err
	}
	r.effects.once.Do(func() {
		r.effects.effects, r.effects.selected, r.effects.err = r.selectedTargetEffectsUncached()
	})
	return cloneCompactKbuildSelectedTargetEffects(r.effects.effects), r.effects.selected, r.effects.err
}

func (r *CompactKbuildResolvedTarget) selectedTargetEffectsUncached() (CompactKbuildSelectedTargetEffects, bool, error) {
	if !r.matched {
		return CompactKbuildSelectedTargetEffects{}, false, nil
	}
	profile := r.profile
	target := r.target
	match := r.match
	var err error
	match, err = activeCompactKbuildRuleCommands(target, match, nil)
	if err != nil {
		return CompactKbuildSelectedTargetEffects{}, false, err
	}
	commands := match.commandSequence()
	effects := CompactKbuildSelectedTargetEffects{
		ActionRoles:            []KbuildActionRoleRef{},
		PrimaryActionRoles:     []KbuildActionRoleRef{},
		DeferredContentQueries: []KbuildDeferredContentQuery{},
	}
	appendQueries := func(values ...string) error {
		for _, value := range values {
			queries, queryErr := compactKbuildDeferredContentQueries(profile, value)
			if queryErr != nil {
				return queryErr
			}
			effects.DeferredContentQueries = append(effects.DeferredContentQueries, queries...)
		}
		return nil
	}
	if len(commands) == 1 && commands[0] == compactKbuildDirectRecipeCommand &&
		(len(match.commandTemplates) == 0 ||
			len(match.commandTemplates) == 1 && match.commandTemplates[0].Name == compactKbuildDirectRecipeCommand) {
		injected, injectionErr := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
		if injectionErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, injectionErr
		}
		automatic, automaticErr := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
		if automaticErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, automaticErr
		}
		values, valuesErr := evaluateCompactKbuildTargetForMakeTarget(
			profile, target, match.lookupTarget, automatic.target, automatic.stem, automatic.normal,
			automatic.order, injected, true, "CONFIG_SHELL",
		)
		if valuesErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, valuesErr
		}
		for _, expression := range match.rule.Recipe {
			if isKbuildRecipeDirectorySetupExpression(expression) {
				continue
			}
			if _, controlEffect, controlErr := exactKbuildEvalRecipeBody(expression); controlErr != nil {
				return CompactKbuildSelectedTargetEffects{}, false, controlErr
			} else if controlEffect {
				continue
			}
			text, expressionRefs, expressionErr := evaluateCompactKbuildTextActionRolesForMakeTarget(
				profile, target, match.lookupTarget, automatic.target, automatic.stem,
				automatic.normal, automatic.order, injected, expression,
			)
			if expressionErr != nil {
				return CompactKbuildSelectedTargetEffects{}, false, expressionErr
			}
			effects.ActionRoles = append(effects.ActionRoles, expressionRefs...)
			if queryErr := appendQueries(text); queryErr != nil {
				return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf("Kbuild target %q direct recipe: %w", target, queryErr)
			}
			// Recursive Make is graph control flow rather than a process lowered by
			// this target. Child invocation discovery projects its exported
			// environment separately; attempting compound-command discovery here
			// would also misclassify the intentionally unresolved $(MAKE) word as a
			// dynamic executable.
			if strings.Contains(expression, "$(MAKE)") || strings.Contains(expression, "${MAKE}") {
				continue
			}
			primaryRefs, environmentRefs, environmentQueries, environmentErr := compactKbuildSelectedCommandEnvironmentEffects(
				profile, target, match.lookupTarget, automatic, injected, values, text,
			)
			if environmentErr != nil {
				return CompactKbuildSelectedTargetEffects{}, false, environmentErr
			}
			effects.PrimaryActionRoles = append(effects.PrimaryActionRoles, primaryRefs...)
			effects.ActionRoles = append(effects.ActionRoles, environmentRefs...)
			effects.DeferredContentQueries = append(effects.DeferredContentQueries, environmentQueries...)
		}
		effects.ActionRoles = canonicalKbuildActionRoleRefs(effects.ActionRoles)
		effects.PrimaryActionRoles = canonicalKbuildActionRoleRefs(effects.PrimaryActionRoles)
		effects.DeferredContentQueries = canonicalKbuildDeferredContentQueries(effects.DeferredContentQueries)
		return effects, true, nil
	}
	injected, injectionErr := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
	if injectionErr != nil {
		return CompactKbuildSelectedTargetEffects{}, false, injectionErr
	}
	automatic, automaticErr := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
	if automaticErr != nil {
		return CompactKbuildSelectedTargetEffects{}, false, automaticErr
	}
	selections := match.commandTemplates
	if len(selections) == 0 {
		return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf(
			"Kbuild target %q has no active source-selected command templates", target,
		)
	}
	values, err := evaluateCompactKbuildRuleVariablesRooted(target, match, nil, injected, "CONFIG_SHELL")
	if err != nil {
		return CompactKbuildSelectedTargetEffects{}, false, err
	}
	for _, selection := range selections {
		name := "direct recipe"
		if selection.Name != "" {
			name = "cmd_" + selection.Name
		}
		templateRefs, refsErr := KbuildActionRoleRefs(selection.Text)
		if refsErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf("Kbuild target %q variable %s action-role provenance: %w", target, name, refsErr)
		}
		effects.ActionRoles = append(effects.ActionRoles, templateRefs...)
		if queryErr := appendQueries(selection.Text); queryErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf("Kbuild target %q variable %s: %w", target, name, queryErr)
		}
		primaryRefs, environmentRefs, environmentQueries, environmentErr := compactKbuildSelectedCommandEnvironmentEffects(
			profile, target, match.lookupTarget, automatic, injected, values, selection.Text,
		)
		if environmentErr != nil {
			return CompactKbuildSelectedTargetEffects{}, false, fmt.Errorf("Kbuild target %q variable %s command environment: %w", target, name, environmentErr)
		}
		effects.PrimaryActionRoles = append(effects.PrimaryActionRoles, primaryRefs...)
		effects.ActionRoles = append(effects.ActionRoles, environmentRefs...)
		effects.DeferredContentQueries = append(effects.DeferredContentQueries, environmentQueries...)
	}
	effects.ActionRoles = canonicalKbuildActionRoleRefs(effects.ActionRoles)
	effects.PrimaryActionRoles = canonicalKbuildActionRoleRefs(effects.PrimaryActionRoles)
	effects.DeferredContentQueries = canonicalKbuildDeferredContentQueries(effects.DeferredContentQueries)
	return effects, true, nil
}

// compactKbuildSelectedCommandEnvironmentEffects uses the same process-
// environment projection as final lowering. Immutable scripts retain only
// statically observable exports; ordinary opaque configured tools retain every
// role-free export, including deferred values they may read through getenv.
func compactKbuildSelectedCommandEnvironmentEffects(
	profile CompactKbuildProfile,
	target, lookupTarget string,
	automatic compactKbuildAutomaticContext,
	injected map[string]string,
	values map[string]string,
	text string,
) ([]KbuildActionRoleRef, []KbuildActionRoleRef, []KbuildDeferredContentQuery, error) {
	parsed, err := parseCompactKbuildRecipe(
		compactKbuildProfileCanonicalRecipeText(profile, text),
		automatic,
	)
	if err != nil {
		canonical := compactKbuildProfileCanonicalRecipeText(profile, text)
		commands, commandErr := compactKbuildCompoundProgramCommands(canonical)
		if commandErr != nil {
			// Program discovery is an environment-projection refinement, not a
			// prerequisite for observing the shell text itself. Legacy backticks
			// and intentionally unresolved Make command prefixes still expose
			// their direct exported-variable reads to the shell scanner; final
			// action lowering remains responsible for validating executable
			// identities once the command is concrete.
			commands = nil
		}
		primaryRefs := compactKbuildCommandPrimaryActionRoles(commands)
		usage, usageErr := compactKbuildHermeticScriptEnvironmentUsage(profile, canonical, commands)
		if usageErr != nil {
			return nil, nil, nil, usageErr
		}
		environment, refs, environmentErr := compactKbuildProjectedSourceScriptEnvironmentForMakeTarget(
			profile, target, lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected, nil, usage,
		)
		if environmentErr != nil {
			return nil, nil, nil, environmentErr
		}
		queries := []KbuildDeferredContentQuery{}
		for _, value := range sortedStringMapValues(environment) {
			found, queryErr := compactKbuildDeferredContentQueries(profile, value)
			if queryErr != nil {
				return nil, nil, nil, queryErr
			}
			queries = append(queries, found...)
		}
		return canonicalKbuildActionRoleRefs(primaryRefs), canonicalKbuildActionRoleRefs(refs), canonicalKbuildDeferredContentQueries(queries), nil
	}
	primaryRefs := compactKbuildCommandPrimaryActionRoles(parsed)
	refs := []KbuildActionRoleRef{}
	queries := []KbuildDeferredContentQuery{}
	for _, command := range parsed {
		commandRefs := []KbuildActionRoleRef{}
		fields := append(append([]string{command.program}, command.arguments...), sortedStringMapValues(command.environment)...)
		for _, field := range fields {
			fieldRefs, refsErr := KbuildActionRoleRefs(field)
			if refsErr != nil {
				return nil, nil, nil, refsErr
			}
			commandRefs = append(commandRefs, fieldRefs...)
		}
		explicitHost, explicitTarget := false, false
		for _, ref := range commandRefs {
			explicitHost = explicitHost || ref.Scope == "host"
			explicitTarget = explicitTarget || ref.Scope == "target"
		}
		if explicitHost && explicitTarget {
			// The command text already proves the contradiction. Let the normal
			// selected-action diagnostic own it without attempting one scope's
			// concrete source-script rewrite first.
			continue
		}
		scope := "target"
		if explicitHost {
			scope = "host"
		}
		configured := []KbuildActionRoleRef{}
		for _, ref := range commandRefs {
			selected, matches := kbuildActionRoleRefForScope(ref, scope)
			if matches {
				configured = append(configured, selected)
			}
		}
		invocation, sourceScript, invocationErr := compactKbuildSourceScriptCommand(
			profile, command, values, scope, canonicalKbuildActionRoleRefs(configured),
		)
		if invocationErr != nil {
			return nil, nil, nil, invocationErr
		}
		inline := command.environment
		usage := compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}}
		if sourceScript {
			inline = invocation.environment
			usage = invocation.environmentUsage
		}
		environment, environmentRefs, environmentErr := compactKbuildProjectedSourceScriptEnvironmentForMakeTarget(
			profile, target, lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected,
			inline, usage,
		)
		if environmentErr != nil {
			return nil, nil, nil, environmentErr
		}
		refs = append(refs, environmentRefs...)
		for _, value := range sortedStringMapValues(environment) {
			found, queryErr := compactKbuildDeferredContentQueries(profile, value)
			if queryErr != nil {
				return nil, nil, nil, queryErr
			}
			queries = append(queries, found...)
		}
	}
	return canonicalKbuildActionRoleRefs(primaryRefs), canonicalKbuildActionRoleRefs(refs), canonicalKbuildDeferredContentQueries(queries), nil
}

func compactKbuildCommandPrimaryActionRoles(commands []compactKbuildRecipeCommand) []KbuildActionRoleRef {
	refs := make([]KbuildActionRoleRef, 0, len(commands))
	for _, command := range commands {
		if ref, ok := parseKbuildActionRoleToken(command.program); ok {
			refs = append(refs, ref)
		}
	}
	return canonicalKbuildActionRoleRefs(refs)
}

func canonicalKbuildActionRoleRefs(refs []KbuildActionRoleRef) []KbuildActionRoleRef {
	seen := map[KbuildActionRoleRef]bool{}
	unique := refs[:0]
	for _, ref := range refs {
		if !seen[ref] {
			seen[ref] = true
			unique = append(unique, ref)
		}
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].Scope != unique[j].Scope {
			return unique[i].Scope < unique[j].Scope
		}
		return unique[i].Role < unique[j].Role
	})
	return unique
}

func evaluateCompactKbuildText(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
	resolveSymbolic bool,
) (string, error) {
	return evaluateCompactKbuildTextWithAutomaticTarget(
		profile, target, target, stem, normal, orderOnly, injected, expression, resolveSymbolic,
	)
}

func evaluateCompactKbuildTextWithAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
	resolveSymbolic bool,
) (string, error) {
	return evaluateCompactKbuildTextForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly,
		injected, expression, resolveSymbolic,
	)
}

func evaluateCompactKbuildTextForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	expression string,
	resolveSymbolic bool,
) (string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, false, false,
	)
	if err != nil {
		return "", err
	}
	defer cleanup()
	value, err := parser.expand(expression)
	if err != nil {
		return "", fmt.Errorf("Kbuild target %q expression: %w", target, err)
	}
	if resolveSymbolic {
		value, err = parser.resolveKbuildSymbolic(value)
		if err != nil {
			return "", fmt.Errorf("Kbuild target %q expression symbolic result: %w", target, err)
		}
	}
	return value, nil
}

func compactKbuildTargetParserWithExports(
	profile CompactKbuildProfile,
	target, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	allowShell bool,
	collectExports bool,
) (*kbuildParser, func(), error) {
	return compactKbuildTargetParserWithExportsForLookup(
		profile, target, target, automaticTarget, stem, normal, orderOnly,
		injected, allowShell, collectExports,
	)
}

func compactKbuildTargetParserWithExportsForLookup(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	allowShell bool,
	collectExports bool,
) (*kbuildParser, func(), error) {
	evaluator, err := compactKbuildProfileTargetEvaluator(profile, target)
	if err != nil {
		return nil, nil, err
	}
	variablePrograms := compactKbuildTargetVariablePrograms(profile, target, lookupTarget)
	variableCount := 0
	for _, variables := range variablePrograms {
		variableCount += len(variables)
	}
	parser := cloneKbuildParserForTargetEvaluation(
		evaluator.template, len(injected)+variableCount, collectExports,
	)
	// Action lowering uses private control-byte roots so it can simplify
	// Kbuild joins such as $(objtree)/$(obj) without rewriting an identical
	// source-authored literal. Give filesystem-aware Make functions aliases for
	// those roots while keeping the captured evaluator immutable.
	if compactKbuildAutomaticEvaluationUsesPrivateActionRoots(injected) {
		parser.sourceRoots = maps.Clone(parser.sourceRoots)
		if root, ok := parser.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]; ok {
			parser.sourceRoots[compactKbuildActionSourceTreeMarker] = root
		}
		if root, ok := parser.sourceRoots["__LINUX_BZL_OBJECT_TREE__"]; ok {
			parser.sourceRoots[compactKbuildActionObjectTreeMarker] = root
			parser.sourceRoots[compactKbuildActionAbsoluteObjectTreeMarker] = root
		}
	}
	if !allowShell {
		parser.shell = nil
		parser.shellResultAvailable = nil
	}
	if renderedObj, injectedObj := injected["obj"]; injectedObj {
		if _, defined := parser.lookupVariable("obj"); defined {
			logicalObj, _, expansionErr := parser.expandVariable("obj", "$(obj)", 0)
			if expansionErr != nil {
				return nil, nil, fmt.Errorf("Kbuild target %q logical obj value: %w", target, expansionErr)
			}
			renderedObj = strings.TrimSpace(renderedObj)
			logicalObj = strings.TrimSpace(logicalObj)
			if renderedObj != "" && renderedObj != logicalObj {
				parser.renderedValueProjections = append(
					parser.renderedValueProjections,
					kbuildValueProjection{rendered: renderedObj, logical: logicalObj},
				)
			}
		}
	}
	for name, value := range injected {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "=\x00") {
			return nil, nil, fmt.Errorf("Kbuild target %q has invalid injected variable %q", target, name)
		}
		parser.setVariable(name, kbuildVariable{value: value})
	}
	parser.pushLocal(kbuildAutomaticVariables(automaticTarget, stem, normal, orderOnly))
	cleanup := func() { parser.popLocal() }
	if variableCount != 0 {
		// Target-specific assignments may replace an inherited variable's
		// environment origin.  Keep that mutation local to this target view.
		parser.environmentVariables = maps.Clone(parser.environmentVariables)
	}
	commandLineValues := map[string]kbuildVariable{}
	commandLineDefined := map[string]bool{}
	commandLineExported := map[string]bool{}
	commandLineCaptured := map[string]bool{}
	for _, variables := range variablePrograms {
		for _, variable := range variables {
			name := variable.Variable
			if !parser.commandLineVariables[name] || commandLineCaptured[name] {
				continue
			}
			commandLineCaptured[name] = true
			commandLineValues[name], commandLineDefined[name] = parser.lookupVariable(name)
			commandLineExported[name] = parser.exported[name]
		}
	}
	type targetVariableFrameState struct {
		established bool
		override    bool
	}
	for _, variables := range variablePrograms {
		states := map[string]targetVariableFrameState{}
		for _, variable := range variables {
			name := variable.Variable
			state := states[name]
			override := slices.Contains(variable.Modifiers, "override")
			if state.override && !override {
				if collectExports {
					applyKbuildTargetVariableExportModifiers(parser, variable)
				}
				continue
			}
			if parser.commandLineVariables[name] && !override {
				if !state.established {
					if commandLineDefined[name] {
						parser.setVariable(name, commandLineValues[name])
					} else {
						parser.undefineVariable(name)
					}
					if collectExports {
						if commandLineExported[name] {
							parser.exported[name] = true
						} else {
							delete(parser.exported, name)
						}
					}
					state.established = true
					states[name] = state
				}
				if collectExports {
					applyKbuildTargetVariableExportModifiers(parser, variable)
				}
				continue
			}
			raw := variable.rawValue
			if raw == "" {
				raw = variable.Value
			}
			// Recursive target-specific RHS text remains lazy. Immediate snapshots
			// were captured while parsing the declaration; recomputing one after
			// installing unrelated target-local bindings changes GNU Make's +=
			// timing and flavor semantics.
			parser.assign(name, variable.Operator, raw, variable.Value)
			state.established = true
			state.override = state.override || override
			states[name] = state
			if collectExports {
				applyKbuildTargetVariableExportModifiers(parser, variable)
			}
		}
	}
	return parser, cleanup, nil
}

func applyKbuildTargetVariableExportModifiers(parser *kbuildParser, variable KbuildTargetVariable) {
	for _, modifier := range variable.Modifiers {
		switch modifier {
		case "export":
			parser.exported[variable.Variable] = true
		case "unexport":
			delete(parser.exported, variable.Variable)
		}
	}
}

func compactKbuildTargetVariablePrograms(
	profile CompactKbuildProfile,
	target, lookupTarget string,
) [][]KbuildTargetVariable {
	scope := profile.targetVariableScopes[compactKbuildGraphTargetPath(target)]
	local := compactKbuildTargetVariablesForMakeTarget(profile, target, lookupTarget)
	if scope == nil {
		if len(local) == 0 {
			return nil
		}
		return [][]KbuildTargetVariable{local}
	}
	frames := []*compactKbuildTargetVariableScope{}
	for current := scope; current != nil; current = current.parent {
		frames = append(frames, current)
	}
	programs := make([][]KbuildTargetVariable, 0, len(frames)+1)
	for index := len(frames) - 1; index >= 0; index-- {
		programs = append(programs, frames[index].variables)
	}
	if len(local) != 0 {
		programs = append(programs, local)
	}
	return programs
}

// ActivateCompactKbuildProfileTargetProbeEnvironment restores the exact
// source-ordered process environment captured for one target. Probe evaluators
// are shared across lazily evaluated profiles, so callers which execute a
// target-owned probe after evaluating its Make text must re-establish this
// target boundary explicitly rather than falling back to the invocation's
// incoming environment.
func ActivateCompactKbuildProfileTargetProbeEnvironment(
	profile CompactKbuildProfile,
	target string,
) error {
	graphTarget := compactKbuildGraphTargetPath(target)
	activate := profile.probeEnvironmentActivation
	if graphTarget != "" {
		if selected := profile.targetProbeEnvironmentActivations[graphTarget]; selected != nil {
			activate = selected
		}
	}
	if activate != nil {
		if err := activate(); err != nil {
			return fmt.Errorf(
				"activate Kbuild profile %q target %q probe environment: %w",
				profile.Name, target, err,
			)
		}
	}
	return nil
}

// compactKbuildProfileTargetEvaluator is the single activation boundary for
// lazy target evaluation. Make parser callbacks close over the workload's
// mutable probe scopes, so selecting a captured evaluator must also restore
// the exact source-ordered process environment attached to that capture.
func compactKbuildProfileTargetEvaluator(
	profile CompactKbuildProfile,
	target string,
) (*kbuildTargetEvaluator, error) {
	if err := ActivateCompactKbuildProfileTargetProbeEnvironment(profile, target); err != nil {
		return nil, err
	}
	graphTarget := compactKbuildGraphTargetPath(target)
	evaluator := profile.evaluator
	if graphTarget != "" {
		if selected := profile.targetEvaluators[graphTarget]; selected != nil {
			evaluator = selected
		}
	}
	if evaluator == nil || evaluator.template == nil {
		return nil, fmt.Errorf("Kbuild profile %q has no source-derived target evaluator", profile.Name)
	}
	return evaluator, nil
}

// EvaluateCompactKbuildTargetEnvironment expands the exact variables GNU Make
// exports for one selected target. Export membership comes from the parsed
// invocation plus target-specific `export` assignments; callers may inject
// stable tree markers or configured action-role tokens, but no environment
// variable names are maintained outside the source-owned Make state.
func EvaluateCompactKbuildTargetEnvironment(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironment(profile, target, stem, normal, orderOnly, injected, true)
}

// EvaluateCompactKbuildTargetEnvironmentForMakeTarget expands the selected
// target's exports with separate canonical, rule-lookup, and automatic-target
// identities.
func EvaluateCompactKbuildTargetEnvironmentForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironmentForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, true,
	)
}

// EvaluateCompactKbuildTargetEnvironmentSymbolic preserves probe atoms in the
// environment used to discover recursive Make invocations inside immutable
// source scripts. It is not used for an executable action environment.
func EvaluateCompactKbuildTargetEnvironmentSymbolic(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironment(profile, target, stem, normal, orderOnly, injected, false)
}

// EvaluateCompactKbuildTargetEnvironmentSymbolicForMakeTarget is the
// probe-preserving counterpart of the lexical concrete environment evaluator.
func EvaluateCompactKbuildTargetEnvironmentSymbolicForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironmentForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, false,
	)
}

func evaluateCompactKbuildTargetEnvironment(
	profile CompactKbuildProfile,
	target string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironmentForAutomaticTarget(
		profile, target, target, stem, normal, orderOnly, injected, resolveSymbolic,
	)
}

func evaluateCompactKbuildTargetEnvironmentForAutomaticTarget(
	profile CompactKbuildProfile,
	target, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
) (map[string]string, error) {
	return evaluateCompactKbuildTargetEnvironmentForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly, injected, resolveSymbolic,
	)
}

func evaluateCompactKbuildTargetEnvironmentForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget string,
	stem string,
	normal []string,
	orderOnly []string,
	injected map[string]string,
	resolveSymbolic bool,
) (map[string]string, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, true,
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

func cloneKbuildParserForEvaluation(template *kbuildParser) *kbuildParser {
	parser := *template
	parser.kb = &KbuildFile{}
	parser.environmentVariables = maps.Clone(template.environmentVariables)
	parser.vars = make(map[string]kbuildVariable, len(template.baseVars)+len(template.vars))
	for name, variable := range template.baseVars {
		parser.vars[name] = variable
	}
	for name, variable := range template.vars {
		parser.vars[name] = variable
	}
	parser.baseVars = nil
	parser.exported = maps.Clone(template.exported)
	parser.undefined = maps.Clone(template.undefined)
	parser.symbolicVariables = maps.Clone(template.symbolicVariables)
	parser.renderedValueProjections = nil
	parser.commandLineVariables = maps.Clone(template.commandLineVariables)
	parser.sourceRoots = maps.Clone(template.sourceRoots)
	parser.locals = nil
	parser.expanding = map[string]bool{}
	parser.conds = nil
	parser.currentRule = -1
	parser.includeFunc = nil
	parser.provisionalComputedNames = false
	return &parser
}

// cloneKbuildParserForTargetEvaluation creates a copy-on-write view over one
// immutable parsed invocation.  Expanding a target may cache a deferred simple
// variable or apply target-specific assignments, so those writes go to vars;
// the thousands of source-defined variables remain shared through baseVars.
// Maps which are read-only during target expansion are shared as well.
func cloneKbuildParserForTargetEvaluation(
	template *kbuildParser,
	overlayCapacity int,
	collectExports bool,
) *kbuildParser {
	parser := *template
	parser.kb = &KbuildFile{}
	if len(template.baseVars) == 0 {
		parser.baseVars = template.vars
	} else if len(template.vars) == 0 {
		parser.baseVars = template.baseVars
	} else {
		// This path is reserved for an evaluator made from another overlay.  The
		// normal parsed and control-effect templates each have one flat layer.
		parser.baseVars = maps.Clone(template.baseVars)
		for name, variable := range template.vars {
			parser.baseVars[name] = variable
		}
	}
	parser.vars = make(map[string]kbuildVariable, overlayCapacity)
	if collectExports {
		parser.exported = maps.Clone(template.exported)
	} else {
		// Expansion may evaluate an `export` directive even when the caller does
		// not consume the resulting environment. Keep a small writable map, but
		// do not clone the invocation's complete export set for every command.
		parser.exported = map[string]bool{}
	}
	parser.undefined = maps.Clone(template.undefined)
	// Target-specific assignment and lazy expansion both invalidate symbolic
	// identity entries. Sharing this mutable map would let a later target or
	// control generation retroactively mutate an earlier snapshot.
	parser.symbolicVariables = maps.Clone(template.symbolicVariables)
	parser.renderedValueProjections = nil
	parser.locals = nil
	parser.expanding = map[string]bool{}
	parser.conds = nil
	parser.currentRule = -1
	parser.includeFunc = nil
	parser.provisionalComputedNames = true
	return &parser
}

func kbuildAutomaticVariables(target, stem string, normal, orderOnly []string) map[string]string {
	unique := make([]string, 0, len(normal))
	seen := map[string]bool{}
	for _, value := range normal {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	first := ""
	if len(normal) != 0 {
		first = normal[0]
	}
	values := map[string]string{
		"@": target,
		"<": first,
		"^": strings.Join(unique, " "),
		"+": strings.Join(normal, " "),
		"?": strings.Join(normal, " "),
		"*": stem,
		"|": strings.Join(orderOnly, " "),
	}
	for _, name := range []string{"@", "<", "^", "+", "?", "*", "|"} {
		value := values[name]
		directory, file := filepath.ToSlash(filepath.Dir(value)), filepath.Base(value)
		if directory == "." || value == "" {
			directory = "."
		}
		values[name+"D"] = directory
		values[name+"F"] = file
	}
	return values
}
