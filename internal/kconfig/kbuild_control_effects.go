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

// KbuildControlEvaluation contains a profile whose process-local evaluator has
// all selected control effects applied, plus the deferred queries that back any
// opaque values in those effects.
type KbuildControlEvaluation struct {
	Profile CompactKbuildProfile
	Queries []KbuildDeferredContentQuery
}

type kbuildControlRecipeKey struct {
	target      string
	ruleIndex   int
	recipeIndex int
}

// KbuildControlEvaluationOptions binds selected recipe expansion to its
// invocation's inherited environment and source-selected exports.
type KbuildControlEvaluationOptions struct {
	// BindProbeEnvironment interns one complete target-scope exported process
	// environment and returns the process-local closure which restores it.
	// Control traversal invokes that closure before expanding the corresponding
	// recipe line and retains it on every evaluator snapshot created there.
	BindProbeEnvironment func(map[string]string) (func() error, error)
	// ResetProbeEnvironment restores the invocation's inherited environment
	// before expanding each recipe's exports. GNU Make uses the incoming value
	// when an exported recursive variable calls $(shell ...) while its own
	// environment entry is being constructed; a prior recipe's expansion must
	// not become the next expansion's process input.
	ResetProbeEnvironment func() error
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
	if condition, conditional := source.exportedWhen[name]; conditional {
		destination.exportedWhen[name] = condition
	} else {
		delete(destination.exportedWhen, name)
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
	if err := parser.resolveExportedMembership(); err != nil {
		return nil, fmt.Errorf("Kbuild control: %w", err)
	}
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

// CompactKbuildRecipeIsControlEffect distinguishes an exact Make eval line
// from an executable recipe when a later selection pass replays the selected
// source lines. The control traversal already applied the assignment; replay
// must use immutable line snapshots only for commands which actually execute.
func CompactKbuildRecipeIsControlEffect(line string) (bool, error) {
	_, control, err := exactKbuildEvalRecipeBody(line)
	return control, err
}
