package kconfig

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type CompactMetadata struct {
	Config                  CompactConfig
	configFragment          map[string]string
	sourceNamespaces        map[string]string
	sourceNamespacePrefixes map[string]string
	exactSourceNamespaces   map[string]string
	exactSourcePaths        map[string]string
	sourceNamespaceIndexErr error
	sourceNamespaceIndexed  bool
	actionRoles             []KbuildActionRoleRef
	preconfiguredObjectTree bool
	selectedProductsOnly    bool
}

type CompactConfig struct {
	KbuildProfiles                  []CompactKbuildProfile
	KbuildSelections                []CompactKbuildSelection
	KbuildDeferredContentSelections []KbuildDeferredContentSelection
	imageTarget                     string
}

// CompactKbuildSelection records one materialized target in the exact Make
// invocation that selected it. Profile names are diagnostic identities; rule
// lowering uses this provenance instead of manufacturing cloned target views.
type CompactKbuildSelection struct {
	Profile string
	Target  string
	// MakeTarget preserves the concrete lexical filename GNU Make used while
	// matching declarations for Target. It may contain parent traversals which
	// disappear from the canonical graph identity in Target. It is mandatory;
	// callers without a distinct lexical spelling must provide Target explicitly.
	MakeTarget string
	// GroupedTrigger names the first-reached logical output which caused one
	// GNU Make grouped recipe to execute. It is empty for ordinary actions and
	// identical across every selected peer of one physical grouped producer.
	// Lowering retains this target as the automatic-variable context even when a
	// different peer sorts first lexically.
	GroupedTrigger string
	// UsesInitialObjectTree is derived from the exact evaluated recipe. It
	// preserves whether this action consumes its invocation's initial visible
	// artifact frontier through materialization ordering and final lowering.
	UsesInitialObjectTree bool
	// InitialObjectTreeArtifacts is the canonical comparable encoding of the
	// invocation's exact visible-artifact frontier subset reachable through
	// source-derived object-tree references in this action. Every record retains
	// its source-proven producer profile and target; a pathname alone is never
	// used to rediscover ownership later.
	InitialObjectTreeArtifacts string
	// GeneratedObjectTreeArtifacts encodes exact selected producers referenced
	// by immutable source text but not exported through the invocation's initial
	// frontier. Each record is bound by profile+target provenance before
	// lowering; it is never resolved through a global path-only guess.
	GeneratedObjectTreeArtifacts string
	// Lifecycle records when the source Make graph demands this target. It is
	// independent from Stage: a prepare prerequisite can itself be a host
	// action, in which case Lifecycle is "prep" while Stage is "host".
	Lifecycle string
	// Scope is the compiler/tool contract selected from source-time action-role
	// provenance. It is always target or host and is deliberately independent
	// from the prep/target lifecycle.
	Scope string
	// Stage is the physical action-plan stage selected from compiler-action
	// provenance. Initial host actions required by a bootstrap closure use
	// prehost; target actions required by a host closure use bootstrap; later
	// host-scoped actions use host; remaining target-scoped actions use prep or
	// target according to Lifecycle.
	Stage string
	// DeferredContentQueries is the canonical encoding of first-class query
	// effects consumed by this action. The query owns its scope, stage, and
	// object-tree frontier; the consumer retains only this explicit value edge.
	DeferredContentQueries string
}

func normalizeCompactKbuildSelectionMakeTargets(
	profiles []CompactKbuildProfile,
	selections []CompactKbuildSelection,
) ([]CompactKbuildSelection, error) {
	profilesByName := make(map[string]CompactKbuildProfile, len(profiles))
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}
	out := append([]CompactKbuildSelection(nil), selections...)
	for index := range out {
		profile, ok := profilesByName[out[index].Profile]
		if !ok {
			return nil, fmt.Errorf("Kbuild selection references missing profile %q", out[index].Profile)
		}
		normalized, err := normalizeCompactKbuildSelectionMakeTarget(profile, out[index])
		if err != nil {
			return nil, err
		}
		out[index] = normalized
	}
	return out, nil
}

// normalizeCompactKbuildSelectionMakeTarget validates one lexical target
// against its already-resolved profile. Keeping this operation separate from
// the slice-level profile lookup lets graph construction validate each
// selection without rebuilding an all-profile index for every target.
func normalizeCompactKbuildSelectionMakeTarget(
	profile CompactKbuildProfile,
	selection CompactKbuildSelection,
) (CompactKbuildSelection, error) {
	if selection.Profile != profile.Name {
		return CompactKbuildSelection{}, fmt.Errorf(
			"Kbuild selection references profile %q through resolved profile %q",
			selection.Profile, profile.Name,
		)
	}
	if selection.MakeTarget == "" {
		return CompactKbuildSelection{}, fmt.Errorf(
			"Kbuild selection %s target %q has empty lexical Make target",
			selection.Profile, selection.Target,
		)
	}
	if strings.TrimSpace(selection.MakeTarget) != selection.MakeTarget ||
		filepath.IsAbs(filepath.FromSlash(selection.MakeTarget)) ||
		strings.ContainsAny(selection.MakeTarget, "%$\x00\r\n") {
		return CompactKbuildSelection{}, fmt.Errorf(
			"Kbuild selection %s target %q has invalid lexical Make target %q",
			selection.Profile, selection.Target, selection.MakeTarget,
		)
	}
	if _, aliasesTarget := ResolveCompactKbuildMakeTarget(
		profile, selection.Target, selection.MakeTarget,
	); !aliasesTarget {
		graphCanonical := CanonicalKbuildGraphTarget(selection.MakeTarget)
		profileCanonical := CanonicalKbuildProfileTarget(profile, selection.MakeTarget)
		return CompactKbuildSelection{}, fmt.Errorf(
			"Kbuild selection %s lexical Make target %q resolves to root target %q and invocation target %q, want canonical target %q",
			selection.Profile, selection.MakeTarget, graphCanonical, profileCanonical, selection.Target,
		)
	}
	return selection, nil
}

// EncodeCompactKbuildDeferredContentQueries returns a deterministic comparable
// representation for the query-use edges of one materialized action.
func EncodeCompactKbuildDeferredContentQueries(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	tokens = append([]string(nil), tokens...)
	sort.Strings(tokens)
	tokens = slices.Compact(tokens)
	encoded, err := json.Marshal(tokens)
	if err != nil {
		panic(fmt.Sprintf("encode deferred Kbuild content queries: %v", err))
	}
	return string(encoded)
}

func compactKbuildSelectionDeferredContentQueries(selection CompactKbuildSelection) ([]string, error) {
	if selection.DeferredContentQueries == "" {
		return nil, nil
	}
	var tokens []string
	if err := json.Unmarshal([]byte(selection.DeferredContentQueries), &tokens); err != nil {
		return nil, fmt.Errorf("decode deferred Kbuild content queries: %w", err)
	}
	if len(tokens) == 0 || EncodeCompactKbuildDeferredContentQueries(tokens) != selection.DeferredContentQueries {
		return nil, fmt.Errorf("deferred Kbuild content query encoding is not canonical")
	}
	return tokens, nil
}

// CompactKbuildVisibleArtifact identifies the source-proven target boundary
// which made one object-root-relative path visible to a recursive Make
// invocation. A boundary implemented only by recursive $(MAKE) may forward
// ownership to a selected descendant; both stage solving and final lowering
// resolve that forwarding through TargetInvocationDependencies.
type CompactKbuildVisibleArtifact struct {
	Path    string
	Profile string
	Target  string
}

// EncodeCompactKbuildInitialObjectTreeArtifacts returns the deterministic,
// comparable selection encoding of exact visible-artifact records. Empty input
// uses the selection zero value. Callers do not need to pre-sort the subset.
func EncodeCompactKbuildInitialObjectTreeArtifacts(artifacts []CompactKbuildVisibleArtifact) string {
	if len(artifacts) == 0 {
		return ""
	}
	canonical := append([]CompactKbuildVisibleArtifact(nil), artifacts...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Path != canonical[j].Path {
			return canonical[i].Path < canonical[j].Path
		}
		if canonical[i].Profile != canonical[j].Profile {
			return canonical[i].Profile < canonical[j].Profile
		}
		return canonical[i].Target < canonical[j].Target
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// CompactKbuildVisibleArtifact contains only strings, so encoding cannot
		// fail. Keep this API total because it is used while constructing a
		// comparable selection identity.
		panic(fmt.Sprintf("encode initial Kbuild object-tree artifacts: %v", err))
	}
	return string(encoded)
}

func compactKbuildSelectionInitialObjectTreeArtifacts(selection CompactKbuildSelection) ([]CompactKbuildVisibleArtifact, error) {
	if selection.InitialObjectTreeArtifacts == "" {
		return nil, nil
	}
	var artifacts []CompactKbuildVisibleArtifact
	if err := json.Unmarshal([]byte(selection.InitialObjectTreeArtifacts), &artifacts); err != nil {
		return nil, fmt.Errorf("decode initial object-tree artifacts: %w", err)
	}
	if len(artifacts) == 0 || EncodeCompactKbuildInitialObjectTreeArtifacts(artifacts) != selection.InitialObjectTreeArtifacts {
		return nil, fmt.Errorf("initial object-tree artifact encoding is not canonical")
	}
	return artifacts, nil
}

func compactKbuildSelectionGeneratedObjectTreeArtifacts(selection CompactKbuildSelection) ([]CompactKbuildVisibleArtifact, error) {
	if selection.GeneratedObjectTreeArtifacts == "" {
		return nil, nil
	}
	var artifacts []CompactKbuildVisibleArtifact
	if err := json.Unmarshal([]byte(selection.GeneratedObjectTreeArtifacts), &artifacts); err != nil {
		return nil, fmt.Errorf("decode generated object-tree artifacts: %w", err)
	}
	if len(artifacts) == 0 || EncodeCompactKbuildInitialObjectTreeArtifacts(artifacts) != selection.GeneratedObjectTreeArtifacts {
		return nil, fmt.Errorf("generated object-tree artifact encoding is not canonical")
	}
	return artifacts, nil
}

// CompactKbuildProfile binds one terminal, preparation, or host Kbuild
// invocation to its source-derived evaluator. Consumers request only the
// targets they need instead of serializing the invocation's variable state.
type CompactKbuildProfile struct {
	Name      string
	Path      string
	Directory string
	// invocationLocation is the typed working directory of the concrete Make
	// process which produced this profile. Directory above remains Kbuild's
	// logical obj= scope; the two intentionally differ for drivers such as
	// scripts/Makefile.build and for recursive make -C invocations.
	invocationLocation    CompactKbuildInvocationLocation
	invocationLocationSet bool
	// initialVisibleArtifactView is immutable, process-local provenance. The
	// selected action graph serializes only queried artifact subsets, never this
	// backing representation.
	initialVisibleArtifactView CompactKbuildInitialVisibleArtifactView
	// InvocationPredecessors are the recursive Make invocations which must
	// complete before this invocation starts. They are derived from the
	// prerequisite and recipe order of the selected parent rule. The edge is
	// execution provenance, not a guessed relationship between output names;
	// lowering uses it to attribute demanded files written as side effects by
	// an earlier source-owned recipe.
	InvocationPredecessors []string
	// TargetInvocationDependencies record recursive Make invocations selected
	// while expanding one concrete target recipe, including invocations found
	// inside an immutable source script executed by that recipe.  The child
	// profile and goals come from the evaluated Make argv; action ordering never
	// infers this edge from an output or script filename.
	TargetInvocationDependencies []CompactKbuildInvocationDependency
	// EntryTargets are the concrete goals requested from this Make invocation.
	// They are derived from the selected parent rule/recipe, never from every
	// target that happens to be declared in the parsed file.
	EntryTargets    []string
	Generated       []KbuildTarget
	Rules           []KbuildRule
	TargetVariables []KbuildTargetVariable
	evaluator       *kbuildTargetEvaluator
	// targetEvaluators retain the exact cumulative Make state in effect when a
	// selected target's executable recipe was expanded. Linux mutates exported
	// variables from prerequisite recipes with $(eval ...); using only the
	// invocation's final evaluator would leak a later mutation backward into an
	// earlier action in the same Make process. This map is process-local for the
	// same reason as evaluator and deferredContentQueries.
	targetEvaluators map[string]*kbuildTargetEvaluator
	// probeEnvironmentActivation restores the exact exported process
	// environment attached to this profile snapshot before one of its lazy Make
	// evaluators performs a compiler or immutable-source probe. Control-effect
	// snapshots and deferred-content query profiles use this fallback directly.
	// The closure is process-local and is never serialized as graph policy.
	probeEnvironmentActivation func() error
	// targetProbeEnvironmentActivations retain the source-ordered, target-
	// specific exported environments of executable recipe lines. Keep these
	// separate from targetEvaluators: many targets can share one immutable Make
	// evaluator generation while target-specific export assignments still give
	// their subprocesses different environments.
	targetProbeEnvironmentActivations map[string]func() error
	// controlGeneration is the deterministic source-order control state of this
	// profile snapshot. The targetRecipe* maps retain the corresponding state
	// for executable target recipes so deferred recipe-side shell queries cannot
	// be rebound to the invocation's final Make state. These fields are
	// process-local execution provenance.
	controlGeneration        uint64
	targetControlGenerations map[string]uint64
	targetRecipeEnvironments map[string]*compactKbuildEnvironmentSnapshot
	targetRecipeShells       map[string]string
	// groupedActions is the process-local, source-order authority for GNU Make
	// grouped targets. Every logical output points at the same immutable record;
	// the trigger is selected once by the DFS control walk and reused by recursive
	// discovery, completion-frontier evaluation, selection, and lowering.
	groupedActions map[string]*compactKbuildGroupedAction
	// targetVariableScopes retains GNU Make's first-reached target-specific
	// variable context for every selected target. Immutable linked frames keep
	// long prerequisite chains linear in memory; later parents cannot replace a
	// target's scope after it has started updating.
	targetVariableScopes map[string]*compactKbuildTargetVariableScope
	// deferredContentQueries is process-local execution provenance for opaque
	// values introduced by selected recipe-side $(eval ... $(shell ...)). It is
	// never serialized as policy; the query command and its exact producer edge
	// are lowered into the content-addressed action plan in the same process.
	deferredContentQueries map[string]KbuildDeferredContentQuery
}

type compactKbuildGroupedAction struct {
	ruleIndex int
	stem      string
	trigger   string
	outputs   []string
}

type compactKbuildTargetVariableScope struct {
	parent    *compactKbuildTargetVariableScope
	variables []KbuildTargetVariable
}

// BindCompactKbuildGroupedAction records one grouped recipe at the first target
// reached by the source-order walk. internal/cmd/kconfig_parse is a separate Go
// package, so these accessors form the narrow internal bridge while the stored
// representation remains private and process-local.
func BindCompactKbuildGroupedAction(
	profile *CompactKbuildProfile,
	ruleIndex int,
	stem, trigger string,
	outputs []string,
) error {
	if profile == nil || ruleIndex < 0 {
		return fmt.Errorf("cannot bind grouped Kbuild action with invalid profile or rule index %d", ruleIndex)
	}
	trigger = compactKbuildGraphTargetPath(trigger)
	if trigger == "" {
		return fmt.Errorf("grouped Kbuild action has an empty trigger")
	}
	canonical := make([]string, 0, len(outputs))
	seen := make(map[string]bool, len(outputs))
	triggerFound := false
	for _, output := range outputs {
		output = compactKbuildGraphTargetPath(output)
		if output == "" || seen[output] {
			return fmt.Errorf("grouped Kbuild action has invalid or duplicate output %q", output)
		}
		seen[output] = true
		triggerFound = triggerFound || output == trigger
		canonical = append(canonical, output)
	}
	if len(canonical) < 2 || !triggerFound {
		return fmt.Errorf("grouped Kbuild action trigger %q is not one of at least two outputs %q", trigger, canonical)
	}
	if profile.groupedActions == nil {
		profile.groupedActions = map[string]*compactKbuildGroupedAction{}
	}
	var existing *compactKbuildGroupedAction
	for _, output := range canonical {
		previous := profile.groupedActions[output]
		if previous == nil {
			continue
		}
		if previous.ruleIndex != ruleIndex || previous.stem != stem || previous.trigger != trigger ||
			!slices.Equal(previous.outputs, canonical) {
			return fmt.Errorf("grouped Kbuild output %q has conflicting source-order identities", output)
		}
		if existing != nil && previous != existing {
			return fmt.Errorf("grouped Kbuild outputs %q do not share one canonical action", canonical)
		}
		existing = previous
	}
	if existing != nil {
		for _, output := range canonical {
			if profile.groupedActions[output] != existing {
				return fmt.Errorf("grouped Kbuild outputs %q are only partially bound", canonical)
			}
		}
		return nil
	}
	action := &compactKbuildGroupedAction{
		ruleIndex: ruleIndex,
		stem:      stem,
		trigger:   trigger,
		outputs:   append([]string(nil), canonical...),
	}
	for _, output := range canonical {
		profile.groupedActions[output] = action
	}
	return nil
}

// CompactKbuildGroupedActionForTarget returns the one DFS-selected physical
// action for target. Returned outputs are cloned so callers cannot mutate the
// canonical shared record.
func CompactKbuildGroupedActionForTarget(
	profile CompactKbuildProfile,
	target string,
) (ruleIndex int, stem, trigger string, outputs []string, ok bool) {
	target = compactKbuildGraphTargetPath(target)
	action := profile.groupedActions[target]
	if action == nil {
		return 0, "", "", nil, false
	}
	return action.ruleIndex, action.stem, action.trigger, append([]string(nil), action.outputs...), true
}

// EffectiveCompactKbuildRecipeRuleIndexes returns the recipe declarations GNU
// Make executes for one already-selected target context. Ordinary colon and
// grouped-colon declarations have one effective recipe (the last declaration
// wins); double-colon declarations are independent and all execute.
func EffectiveCompactKbuildRecipeRuleIndexes(
	profile CompactKbuildProfile,
	ruleIndexes []int,
) ([]int, error) {
	recipeIndexes := make([]int, 0, len(ruleIndexes))
	separator := ""
	seen := map[int]bool{}
	for _, ruleIndex := range ruleIndexes {
		if ruleIndex < 0 || ruleIndex >= len(profile.Rules) {
			return nil, fmt.Errorf("selected Kbuild recipe has invalid rule index %d", ruleIndex)
		}
		if seen[ruleIndex] {
			continue
		}
		seen[ruleIndex] = true
		rule := profile.Rules[ruleIndex]
		candidateSeparator := rule.Separator
		if candidateSeparator == "" {
			candidateSeparator = ":"
		}
		if separator == "" {
			separator = candidateSeparator
		} else if (separator == "::") != (candidateSeparator == "::") {
			return nil, fmt.Errorf("selected Kbuild target mixes ordinary and double-colon declarations")
		}
		if len(rule.Recipe) != 0 {
			recipeIndexes = append(recipeIndexes, ruleIndex)
		}
	}
	if separator == "::" && len(ruleIndexes) > 1 {
		for _, ruleIndex := range ruleIndexes {
			rule := profile.Rules[ruleIndex]
			if len(rule.Prerequisites) != 0 || len(rule.OrderOnly) != 0 {
				return nil, fmt.Errorf(
					"independent double-colon recipes with prerequisites require an interleaved prerequisite/recipe timeline",
				)
			}
		}
	}
	if len(recipeIndexes) <= 1 || separator == "::" {
		return recipeIndexes, nil
	}
	return recipeIndexes[len(recipeIndexes)-1:], nil
}

// ResolveCompactKbuildGroupedRule returns the concrete peer outputs of the
// effective recipe selected for target. GNU &: rules are grouped, as are the
// historical multi-target implicit colon rules used by Linux's yacc/lex rules.
func ResolveCompactKbuildGroupedRule(
	profile CompactKbuildProfile,
	ruleIndexes []int,
	target, stem string,
) (ruleIndex int, outputs []string, grouped bool, err error) {
	effective, err := EffectiveCompactKbuildRecipeRuleIndexes(profile, ruleIndexes)
	if err != nil {
		return 0, nil, false, err
	}
	if len(effective) != 1 {
		return 0, nil, false, nil
	}
	selectedRuleIndex := effective[0]
	rule := profile.Rules[selectedRuleIndex]
	if rule.Separator == "::" {
		return 0, nil, false, nil
	}
	if !compactKbuildRuleHasGroupedOutputs(rule) {
		return 0, nil, false, nil
	}
	target = compactKbuildGraphTargetPath(target)
	seen := make(map[string]bool, len(rule.Targets))
	for _, pattern := range rule.Targets {
		peer := compactKbuildProfileTargetPath(profile, instantiateKbuildRulePattern(pattern, stem))
		if peer == "" || strings.ContainsAny(peer, "%$") {
			return 0, nil, false, fmt.Errorf("rule %d has non-concrete grouped output %q", selectedRuleIndex, peer)
		}
		if !seen[peer] {
			seen[peer] = true
			outputs = append(outputs, peer)
		}
	}
	if !seen[target] {
		return 0, nil, false, fmt.Errorf(
			"rule %d grouped outputs %q do not contain selected target %q",
			selectedRuleIndex, outputs, target,
		)
	}
	if len(outputs) < 2 {
		return 0, nil, false, nil
	}
	return selectedRuleIndex, outputs, true, nil
}

// CompactKbuildInvocationTree identifies which declared tree owns a Make
// process's working directory. Relative compiler paths inherit this identity;
// reducing it to a bare graph path would make source- and object-rooted -C
// invocations indistinguishable.
type CompactKbuildInvocationTree string

const (
	CompactKbuildInvocationSourceTree CompactKbuildInvocationTree = "source"
	CompactKbuildInvocationObjectTree CompactKbuildInvocationTree = "object"
)

// CompactKbuildInvocationLocation is a canonical directory beneath one
// declared invocation tree. The empty directory denotes that tree's root.
type CompactKbuildInvocationLocation struct {
	Tree      CompactKbuildInvocationTree
	Directory string
}

type CompactKbuildInvocationDependency struct {
	Target          string
	Profile         string
	Goals           []string
	ReplayArguments []string
}

type CompactMetadataOptions struct {
	// SourceNamespaces maps canonical source-path prefixes to ActionPlan source
	// namespaces.  This keeps non-kernel inputs (for example a selected Rust
	// source toolchain) distinct from files in the Linux source repository.
	SourceNamespaces map[string]string
	// ExactSourceNamespaces maps individual immutable source paths to their
	// ActionPlan namespaces. Unlike SourceNamespaces, these entries never own
	// descendants. Preconfigured object-tree indexing uses this form so an SDK
	// file cannot accidentally become a source-tree prefix.
	ExactSourceNamespaces map[string]string
	// ActionRoles is the exact scoped role registry from the identity-bound
	// target and host toolset manifests. Make values carry these refs from
	// source parse time; this registry only rejects forged/reserved tokens.
	ActionRoles []KbuildActionRoleRef
	// PreconfiguredObjectTree treats resolved config and preparation artifacts
	// as inputs from an existing object tree instead of producing them again.
	PreconfiguredObjectTree bool
	// SelectedProductsOnly retains the selected native graph and module
	// products without requiring the kernel image/vmlinux facade.
	SelectedProductsOnly bool
}

// CompactConfigGraph binds one resolved configuration to its exact evaluated
// Kbuild invocations and selected materialized targets.
type CompactConfigGraph struct {
	KbuildProfiles                  []CompactKbuildProfile
	KbuildSelections                []CompactKbuildSelection
	KbuildDeferredContentSelections []KbuildDeferredContentSelection
	ImageTarget                     string
}

func normalizeActionRoles(roles []KbuildActionRoleRef) ([]KbuildActionRoleRef, error) {
	seen := map[KbuildActionRoleRef]bool{}
	out := append([]KbuildActionRoleRef(nil), roles...)
	for _, ref := range out {
		if ref.Scope != "target" && ref.Scope != "host" {
			return nil, fmt.Errorf("action role %q has invalid scope %q", ref.Role, ref.Scope)
		}
		if err := validatePlanName(ref.Scope+" action role", ref.Role); err != nil {
			return nil, err
		}
		if seen[ref] {
			return nil, fmt.Errorf("%s action role %q is repeated", ref.Scope, ref.Role)
		}
		seen[ref] = true
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

// CompactMetadataWithOptions resolves one configuration and the exact
// source-derived Kbuild selections consumed by action lowering.
func (t *Tree) CompactMetadataWithOptions(
	flags map[string]string,
	resolveOpts ResolveConfigOptions,
	opts CompactMetadataOptions,
	graphForConfig func(*ResolvedConfig) (CompactConfigGraph, error),
) (*CompactMetadata, error) {
	if graphForConfig == nil {
		return nil, fmt.Errorf("action-plan config graph resolver must not be nil")
	}
	actionRoles, err := normalizeActionRoles(opts.ActionRoles)
	if err != nil {
		return nil, err
	}
	out := &CompactMetadata{
		sourceNamespaces:        maps.Clone(opts.SourceNamespaces),
		exactSourceNamespaces:   maps.Clone(opts.ExactSourceNamespaces),
		actionRoles:             actionRoles,
		preconfiguredObjectTree: opts.PreconfiguredObjectTree,
		selectedProductsOnly:    opts.SelectedProductsOnly,
	}
	if err := out.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return nil, err
	}
	resolved, err := t.ResolveConfigWithOptions(flags, resolveOpts)
	if err != nil {
		return nil, err
	}
	graph, err := graphForConfig(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve Kbuild: %w", err)
	}
	if err := validateCompactKbuildProfiles(graph.KbuildProfiles); err != nil {
		return nil, fmt.Errorf("resolve Kbuild profiles: %w", err)
	}
	selections, err := normalizeCompactKbuildSelectionMakeTargets(graph.KbuildProfiles, graph.KbuildSelections)
	if err != nil {
		return nil, fmt.Errorf("resolve Kbuild selections: %w", err)
	}
	out.Config = CompactConfig{
		KbuildProfiles:                  graph.KbuildProfiles,
		KbuildSelections:                selections,
		KbuildDeferredContentSelections: graph.KbuildDeferredContentSelections,
		imageTarget:                     graph.ImageTarget,
	}
	if _, err := newCompactKbuildSelectionGraph(out.Config); err != nil {
		return nil, fmt.Errorf("resolve Kbuild selections: %w", err)
	}
	out.configFragment = resolvedConfigFragment(resolved)
	return out, nil
}

func resolvedConfigFragment(config *ResolvedConfig) map[string]string {
	fragment := map[string]string{}
	if config == nil {
		return fragment
	}
	for key := range config.Effective {
		if config.ShouldWrite(key) {
			fragment[key] = config.Value(key)
		}
	}
	return fragment
}

// NewCompactKbuildProfile binds an evaluated KbuildFile to its source-tree
// invocation path. Target commands remain lazy in the process-local evaluator.
func NewCompactKbuildProfile(name, makefile, sourceRoot string, kb *KbuildFile) (CompactKbuildProfile, error) {
	if kb == nil {
		return CompactKbuildProfile{}, fmt.Errorf("Kbuild profile %q has nil file", name)
	}
	makefile = filepath.Clean(makefile)
	root := filepath.Clean(sourceRoot)
	if root != "" && filepath.IsAbs(makefile) {
		rel, err := filepath.Rel(root, makefile)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return CompactKbuildProfile{}, fmt.Errorf("Kbuild profile path %q is outside source root %q", makefile, sourceRoot)
		}
		makefile = rel
	}
	makefile = filepath.ToSlash(makefile)
	if makefile == "." || makefile == "" || strings.HasPrefix(makefile, "../") || filepath.IsAbs(makefile) {
		return CompactKbuildProfile{}, fmt.Errorf("Kbuild profile %q has invalid source-tree path %q", name, makefile)
	}
	if name == "" {
		name = makefile
	}
	profile := CompactKbuildProfile{
		Name:            name,
		Path:            makefile,
		Directory:       filepath.ToSlash(filepath.Dir(makefile)),
		Generated:       append([]KbuildTarget(nil), kb.Generated...),
		Rules:           cloneKbuildRules(kb.Rules),
		TargetVariables: cloneKbuildTargetVariables(kb.TargetVariables),
		evaluator:       kb.evaluator,
		// Profiles are intentionally copied by value while walking recursive
		// Make invocations. Keep one process-local registry behind those copies so
		// a target-context evaluator can record execution-time shell provenance
		// without mutating the serialized Kbuild graph.
		deferredContentQueries: map[string]KbuildDeferredContentQuery{},
	}
	// The profile owns immutable copies of the parsed declarations.  Target
	// evaluation needs only the parser's variable state; retaining parser.kb
	// would keep a second complete Rules/Generated/TargetVariables graph alive
	// through evaluator -> template -> kb for every recursive invocation.
	if profile.evaluator != nil && profile.evaluator.template != nil {
		profile.evaluator.template.kb = &KbuildFile{}
	}
	if profile.Directory == "." {
		profile.Directory = ""
	}
	return profile, nil
}

// SetCompactKbuildProfileDirectory records the working-directory portion of a
// concrete Make invocation. The parsed Makefile path and invocation directory
// intentionally differ for drivers such as scripts/Makefile.build.
func SetCompactKbuildProfileDirectory(profile *CompactKbuildProfile, directory string) {
	if profile == nil {
		return
	}
	directory = filepath.ToSlash(filepath.Clean(directory))
	if directory == "." {
		directory = ""
	}
	profile.Directory = directory
}

// SetCompactKbuildProfileInvocationLocation records the typed working
// directory selected by recursive Make. This is separate from Directory,
// which is the logical obj= directory used to scope Kbuild targets.
func SetCompactKbuildProfileInvocationLocation(
	profile *CompactKbuildProfile,
	location CompactKbuildInvocationLocation,
) error {
	if profile == nil {
		return fmt.Errorf("cannot set invocation location on a nil Kbuild profile")
	}
	if location.Tree != CompactKbuildInvocationSourceTree && location.Tree != CompactKbuildInvocationObjectTree {
		return fmt.Errorf("Kbuild invocation location has invalid tree %q", location.Tree)
	}
	directory := filepath.ToSlash(filepath.Clean(location.Directory))
	if directory == "." {
		directory = ""
	}
	if directory != "" {
		if err := validatePlanRelativePath("Kbuild invocation directory", directory); err != nil {
			return err
		}
	}
	profile.invocationLocation = CompactKbuildInvocationLocation{Tree: location.Tree, Directory: directory}
	profile.invocationLocationSet = true
	return nil
}

func validateCompactKbuildProfiles(profiles []CompactKbuildProfile) error {
	names := map[string]bool{}
	for _, profile := range profiles {
		if profile.Name == "" || profile.Path == "" {
			return fmt.Errorf("Kbuild profile name and path must not be empty")
		}
		if names[profile.Name] {
			return fmt.Errorf("duplicate Kbuild profile name %q", profile.Name)
		}
		names[profile.Name] = true
	}
	return nil
}

func cloneKbuildRules(rules []KbuildRule) []KbuildRule {
	out := append([]KbuildRule(nil), rules...)
	for i := range out {
		out[i].Targets = append([]string(nil), out[i].Targets...)
		out[i].Prerequisites = append([]string(nil), out[i].Prerequisites...)
		out[i].OrderOnly = append([]string(nil), out[i].OrderOnly...)
		out[i].Recipe = append([]string(nil), out[i].Recipe...)
		out[i].Condition = cloneKbuildCondition(out[i].Condition)
	}
	return out
}

func cloneKbuildTargetVariables(variables []KbuildTargetVariable) []KbuildTargetVariable {
	out := append([]KbuildTargetVariable(nil), variables...)
	for i := range out {
		out[i].Targets = append([]string(nil), out[i].Targets...)
		out[i].Modifiers = append([]string(nil), out[i].Modifiers...)
	}
	return out
}

func cloneKbuildCondition(condition KbuildCondition) KbuildCondition {
	condition.Conditions = append([]KbuildCondition(nil), condition.Conditions...)
	for i := range condition.Conditions {
		condition.Conditions[i] = cloneKbuildCondition(condition.Conditions[i])
	}
	return condition
}

func evaluatedKbuildTargetRuleContext(profile CompactKbuildProfile, target string) ([]string, []string, string, error) {
	// The root directory goal is source-defined control flow, not an artifact.
	// Its evaluator match may come from a normalized duplicate declaration whose
	// rule index is intentionally absent from the compact artifact-rule index;
	// resolve the merged declaration context directly instead.
	if compactKbuildGraphTargetPath(target) == "." {
		return evaluatedKbuildSelectedTargetRuleContext(profile, target, nil)
	}
	if profile.evaluator != nil {
		selected, found, err := (&CompactMetadata{}).compactKbuildRuleForProfile(profile, target)
		if err != nil {
			return nil, nil, "", err
		}
		if found {
			return evaluatedKbuildSelectedTargetRuleContext(profile, target, &selected)
		}
	}
	return evaluatedKbuildSelectedTargetRuleContext(profile, target, nil)
}

// EvaluateCompactKbuildTargetRuleContext returns the canonical prerequisites
// and pattern stem of the exact GNU Make rule context selected for target.
// Unlike the exported declaration snapshot, this includes prerequisites merged
// from explicit declarations into the selected implicit or explicit recipe.
func EvaluateCompactKbuildTargetRuleContext(profile CompactKbuildProfile, target string) ([]string, []string, string, error) {
	return evaluatedKbuildTargetRuleContext(profile, target)
}

type compactKbuildEvaluatedPath struct {
	graphPath string
	makeWord  string
}

type compactKbuildEvaluatedRuleContext struct {
	target    compactKbuildEvaluatedPath
	normal    []compactKbuildEvaluatedPath
	orderOnly []compactKbuildEvaluatedPath
	stem      string
}

var compactKbuildStableMakeWordReplacer = strings.NewReplacer(
	"${tree:kernel}", "__LINUX_BZL_SOURCE_TREE__",
	"${tree:prep}", "__LINUX_BZL_OBJECT_TREE__",
	"${tree:host}", "__LINUX_BZL_OBJECT_TREE__",
	"${tree:bootstrap}", "__LINUX_BZL_OBJECT_TREE__",
	"${tree:prehost}", "__LINUX_BZL_OBJECT_TREE__",
)

func compactKbuildStableMakeWord(profile CompactKbuildProfile, value string) (string, error) {
	value = compactKbuildStableMakeWordReplacer.Replace(strings.TrimSpace(filepathToSlash(value)))
	// GNU Make interns relative file names without a leading ./ before binding
	// automatic variables. The parser sees the expanded declaration text, so
	// mirror that one normalization without path-cleaning the remaining word.
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if !filepath.IsAbs(filepath.FromSlash(value)) || profile.evaluator == nil || profile.evaluator.template == nil {
		return value, nil
	}
	type rootMatch struct {
		marker   string
		root     string
		relative string
	}
	matches := []rootMatch{}
	for marker, rawRoot := range profile.evaluator.template.sourceRoots {
		root, err := filepath.Abs(rawRoot)
		if err != nil {
			continue
		}
		candidates := []string{filepath.Clean(root)}
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != candidates[0] {
			candidates = append(candidates, filepath.Clean(resolved))
		}
		for _, candidate := range candidates {
			relative, ok := compactKbuildPathRelativeToRoot(candidate, filepath.FromSlash(value))
			if !ok {
				continue
			}
			matches = append(matches, rootMatch{marker: marker, root: candidate, relative: relative})
		}
	}
	if len(matches) == 0 {
		return value, nil
	}
	priority := func(marker string) int {
		switch marker {
		case "__LINUX_BZL_SOURCE_TREE__":
			return 0
		case "__LINUX_BZL_OBJECT_TREE__":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].root) != len(matches[j].root) {
			return len(matches[i].root) > len(matches[j].root)
		}
		if priority(matches[i].marker) != priority(matches[j].marker) {
			return priority(matches[i].marker) < priority(matches[j].marker)
		}
		return matches[i].marker < matches[j].marker
	})
	selected := matches[0]
	if selected.relative == "" {
		return selected.marker, nil
	}
	return selected.marker + "/" + filepathToSlash(selected.relative), nil
}

func compactKbuildEvaluatedPathNamespace(value string) string {
	value = strings.TrimSpace(filepathToSlash(value))
	for _, candidate := range []struct {
		marker    string
		namespace string
	}{
		{"__LINUX_BZL_SOURCE_TREE__", "source"},
		{"__LINUX_BZL_OBJECT_TREE__", "object"},
	} {
		if value == candidate.marker || strings.HasPrefix(value, candidate.marker+"/") {
			return candidate.namespace
		}
	}
	return ""
}

// evaluatedKbuildSelectedTargetRuleContext expands the prerequisites of the
// exact rule selected by compactKbuildRuleForProfile. Its public shape remains
// canonical graph paths for dependency traversal; the paired Make spellings
// are retained by evaluatedKbuildSelectedTargetMakeContext for automatic
// variables and exact textual functions such as filter/filter-out.
func evaluatedKbuildSelectedTargetRuleContext(
	profile CompactKbuildProfile,
	target string,
	selectedMatch *compactKbuildRuleMatch,
) ([]string, []string, string, error) {
	context, err := evaluatedKbuildSelectedTargetMakeContext(profile, target, selectedMatch)
	if err != nil {
		return nil, nil, "", err
	}
	normal := make([]string, len(context.normal))
	for index, prerequisite := range context.normal {
		normal[index] = prerequisite.graphPath
	}
	orderOnly := make([]string, len(context.orderOnly))
	for index, prerequisite := range context.orderOnly {
		orderOnly[index] = prerequisite.graphPath
	}
	return normal, orderOnly, context.stem, nil
}

// evaluatedKbuildSelectedTargetMakeContext retains both identities of every
// selected rule word: graphPath is the canonical producer key, while makeWord
// is the already-expanded spelling GNU Make placed in $@/$</$^/$+/$|. The two
// must not be reconstructed from the eventual Bazel input edge: a source edge
// may transport an object-tree config projection, and a relative prerequisite
// deliberately remains relative during Make functions.
func evaluatedKbuildSelectedTargetMakeContext(
	profile CompactKbuildProfile,
	target string,
	selectedMatch *compactKbuildRuleMatch,
) (compactKbuildEvaluatedRuleContext, error) {
	return evaluatedKbuildTargetMakeContext(profile, target, selectedMatch, true)
}

// evaluatedKbuildSelectedRuleDeclarationContext expands only the
// prerequisites declared by the candidate which owns the selected recipe.
// GNU Make uses that declaration-local context while deciding whether an
// implicit rule can be applied. Prerequisites from recipe-less explicit
// declarations are merged only after the implicit recipe has been selected.
func evaluatedKbuildSelectedRuleDeclarationContext(
	profile CompactKbuildProfile,
	target string,
	selectedMatch compactKbuildRuleMatch,
) ([]string, []string, string, error) {
	context, err := evaluatedKbuildTargetMakeContext(profile, target, &selectedMatch, false)
	if err != nil {
		return nil, nil, "", err
	}
	normal := make([]string, len(context.normal))
	for index, prerequisite := range context.normal {
		normal[index] = prerequisite.graphPath
	}
	orderOnly := make([]string, len(context.orderOnly))
	for index, prerequisite := range context.orderOnly {
		orderOnly[index] = prerequisite.graphPath
	}
	return normal, orderOnly, context.stem, nil
}

func evaluatedKbuildTargetMakeContext(
	profile CompactKbuildProfile,
	target string,
	selectedMatch *compactKbuildRuleMatch,
	mergeExplicitPrerequisites bool,
) (compactKbuildEvaluatedRuleContext, error) {
	target = compactKbuildGraphTargetPath(target)
	lookupTarget := target
	if selectedMatch != nil {
		lookupTarget = selectedMatch.lookupTarget
	}
	type match struct {
		rule        KbuildRule
		target      string
		targetOrder int
		makeTarget  string
		stem        string
		specificity int
		ruleOrder   int
	}
	matches := []match{}
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, lookupTarget) {
		rawTarget := target
		if candidate.targetOrder >= 0 && candidate.targetOrder < len(candidate.rule.Targets) {
			rawTarget = instantiateKbuildRulePattern(candidate.rule.Targets[candidate.targetOrder], candidate.stem)
		}
		makeTarget, err := compactKbuildStableMakeWord(profile, rawTarget)
		if err != nil {
			return compactKbuildEvaluatedRuleContext{}, err
		}
		matches = append(matches, match{
			rule: candidate.rule, target: candidate.target, targetOrder: candidate.targetOrder,
			makeTarget: makeTarget, stem: candidate.stem,
			specificity: len(strings.ReplaceAll(candidate.target, "%", "")), ruleOrder: candidate.ruleOrder,
		})
	}
	if len(matches) == 0 {
		return compactKbuildEvaluatedRuleContext{}, fmt.Errorf("target %q profile %q has no evaluated rule", target, profile.Name)
	}
	selected := matches[0]
	if selectedMatch != nil {
		found := false
		for _, candidate := range matches {
			if candidate.ruleOrder == selectedMatch.ruleOrder && candidate.targetOrder == selectedMatch.targetOrder && candidate.stem == selectedMatch.stem {
				selected = candidate
				found = true
				break
			}
		}
		if !found {
			return compactKbuildEvaluatedRuleContext{}, fmt.Errorf(
				"target %q profile %q selected rule %d with stem %q is absent from evaluated candidates",
				target, profile.Name, selectedMatch.ruleOrder, selectedMatch.stem,
			)
		}
	} else {
		for _, candidate := range matches[1:] {
			if candidate.specificity > selected.specificity {
				selected = candidate
			}
		}
	}
	selectedRules := []match{selected}
	// GNU Make combines every ordinary explicit declaration with the selected
	// recipe even when that recipe comes from an implicit pattern. Kbuild's
	// multi_depend macro relies on the explicit/explicit form, while ordinary
	// suffix fallback uses the explicit-prerequisite/implicit-recipe form. This
	// merge happens after implicit-rule viability has selected the recipe.
	if mergeExplicitPrerequisites && selected.rule.Separator != "::" {
		doubleColon := selected.rule.Separator == "::"
		for _, candidate := range matches {
			if candidate.ruleOrder == selected.ruleOrder && candidate.stem == selected.stem {
				continue
			}
			if strings.Contains(candidate.target, "%") {
				continue
			}
			if doubleColon || candidate.rule.Separator == "::" {
				return compactKbuildEvaluatedRuleContext{}, fmt.Errorf(
					"target %q profile %q mixes independent double-colon and merged rule contexts",
					target, profile.Name,
				)
			}
			selectedRules = append(selectedRules, candidate)
		}
	}
	expand := func(orderOnly bool) ([]compactKbuildEvaluatedPath, error) {
		out := []compactKbuildEvaluatedPath{}
		for _, candidate := range selectedRules {
			values := candidate.rule.Prerequisites
			if orderOnly {
				values = candidate.rule.OrderOnly
			}
			for _, value := range values {
				makeWord, err := compactKbuildStableMakeWord(
					profile, instantiateKbuildRulePattern(value, candidate.stem),
				)
				if err != nil {
					return nil, err
				}
				graphPath := instantiateKbuildRulePattern(compactKbuildProfileTargetPath(profile, value), candidate.stem)
				graphPath = compactKbuildGraphTargetPath(graphPath)
				if graphPath != "" {
					out = append(out, compactKbuildEvaluatedPath{graphPath: graphPath, makeWord: makeWord})
				}
			}
		}
		return out, nil
	}
	normal, err := expand(false)
	if err != nil {
		return compactKbuildEvaluatedRuleContext{}, err
	}
	orderOnly, err := expand(true)
	if err != nil {
		return compactKbuildEvaluatedRuleContext{}, err
	}
	if len(orderOnly) != 0 {
		normalSet := make(map[string]bool, len(normal))
		for _, prerequisite := range normal {
			normalSet[prerequisite.graphPath] = true
		}
		filtered := orderOnly[:0]
		for _, prerequisite := range orderOnly {
			if !normalSet[prerequisite.graphPath] {
				filtered = append(filtered, prerequisite)
			}
		}
		orderOnly = filtered
	}
	return compactKbuildEvaluatedRuleContext{
		target: compactKbuildEvaluatedPath{graphPath: target, makeWord: selected.makeTarget},
		normal: normal, orderOnly: orderOnly, stem: selected.stem,
	}, nil
}

func compactKbuildRuleTargetStem(
	profile CompactKbuildProfile,
	rule KbuildRule,
	declaredTarget string,
	target string,
) (string, bool) {
	stem, ok := matchKbuildRulePattern(declaredTarget, target)
	if !ok {
		return "", false
	}
	if rule.TargetPattern == "" {
		return stem, true
	}
	return matchKbuildRulePattern(compactKbuildProfileTargetPath(profile, rule.TargetPattern), target)
}

// CompactKbuildProfileInvocationLocation returns the evaluated Make process's
// typed working directory. A profile's Directory is the logical obj= directory
// and is not necessarily the process cwd (scripts/Makefile.build is normally
// evaluated from the object root). Profiles assembled by recursive discovery
// must carry an explicit location so physical parser paths cannot leak into
// invocation-tree semantics.
func CompactKbuildProfileInvocationLocation(profile CompactKbuildProfile) (CompactKbuildInvocationLocation, bool) {
	if !profile.invocationLocationSet {
		return CompactKbuildInvocationLocation{}, false
	}
	return profile.invocationLocation, true
}

func compactKbuildPathRelativeToRoot(root, candidate string) (string, bool) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if relative == "." {
		return "", true
	}
	return filepath.ToSlash(relative), true
}

func compactKbuildProfileInvocationRelativePath(profile CompactKbuildProfile, value string) (string, bool) {
	location, ok := CompactKbuildProfileInvocationLocation(profile)
	workingDirectory := location.Directory
	if !ok || workingDirectory == "" || value == workingDirectory || strings.HasPrefix(value, workingDirectory+"/") {
		return value, ok
	}
	return path.Join(workingDirectory, value), true
}

func compactKbuildProfileTargetPath(profile CompactKbuildProfile, value string) string {
	// Make expands $(srctree), $(objtree), $(src), and $(obj) before captured
	// rules reach the action planner.  Strip the physical executor root here as
	// well as the symbolic spellings below so prerequisites and targets cannot
	// retain a local or remote execroot path.  The generated/source distinction
	// is resolved later from the evaluated producer graph; at this boundary we
	// only need the stable logical path within the invocation tree.
	trimmed := strings.TrimSpace(value)
	// The root Makefile uses `.` as an invocation-local phony directory goal.
	// Preserve it as control-flow identity; it is never emitted as an artifact
	// path, but recursive invocation dependencies may legitimately be owned by
	// that selected source target.
	if trimmed == "." || trimmed == "./" {
		return "."
	}
	rootRelative := false
	if filepath.IsAbs(trimmed) && profile.evaluator != nil && profile.evaluator.template != nil {
		roots := map[string]bool{}
		for _, configured := range profile.evaluator.template.sourceRoots {
			if configured == "" {
				continue
			}
			root, err := filepath.Abs(configured)
			if err != nil {
				continue
			}
			root = filepath.Clean(root)
			roots[root] = true
			if resolved, err := filepath.EvalSymlinks(root); err == nil {
				roots[filepath.Clean(resolved)] = true
			}
		}
		ordered := make([]string, 0, len(roots))
		for root := range roots {
			ordered = append(ordered, root)
		}
		sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
		for _, root := range ordered {
			relative, err := filepath.Rel(root, filepath.Clean(trimmed))
			if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			value = filepath.ToSlash(relative)
			rootRelative = true
			break
		}
	}
	value = canonicalKbuildRulePath(value)
	if value == "" || value == "FORCE" {
		return value
	}
	for _, prefix := range []string{
		"$(srctree)/", "${srctree}/", "$(objtree)/", "${objtree}/",
		"${tree:kernel}/", "${tree:prep}/", "${tree:host}/", "${tree:bootstrap}/", "${tree:prehost}/",
		"__LINUX_BZL_SOURCE_TREE__/", "__LINUX_BZL_OBJECT_TREE__/",
	} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	invocationLocal := false
	for _, prefix := range []string{"$(obj)/", "${obj}/", "$(src)/", "${src}/"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			invocationLocal = true
			break
		}
	}
	if strings.HasPrefix(value, "$(") || strings.HasPrefix(value, "${") {
		return value
	}
	if rootRelative {
		return value
	}
	directory := strings.Trim(canonicalKbuildRulePath(profile.Directory), "/")
	if !invocationLocal {
		if scoped, ok := compactKbuildProfileInvocationRelativePath(profile, value); ok {
			return scoped
		}
	}
	if directory == "" || value == directory || strings.HasPrefix(value, directory+"/") {
		return value
	}
	joined := path.Join(directory, value)
	joinedEvidence := compactKbuildProfileSourceAncestryDepth(profile, joined) - len(strings.Split(directory, "/"))
	if joinedEvidence < 0 {
		joinedEvidence = 0
	}
	rootEvidence := compactKbuildProfileSourceAncestryDepth(profile, value)
	if rootEvidence > joinedEvidence {
		return value
	}
	return joined
}

// compactKbuildGraphTargetPath normalizes a target which has already crossed
// the Make-invocation boundary. CompactKbuildProfile entry targets, recursive
// dependency goals, and selections are source/object-tree-relative graph
// identities; interpreting them relative to the profile cwd a second time can
// silently turn an OUTPUT-rooted goal such as tools/objtool/fixdep into
// tools/build/tools/objtool/fixdep.
func compactKbuildGraphTargetPath(value string) string {
	trimmed := strings.TrimSpace(filepathToSlash(value))
	if trimmed == "." || trimmed == "./" {
		return "."
	}
	return canonicalKbuildRulePath(trimmed)
}

// CanonicalKbuildGraphTarget returns the stable target identity after a raw
// Make declaration or argv goal has already been resolved by its profile.
// Unlike CanonicalKbuildProfileTarget it never applies invocation-cwd scope.
func CanonicalKbuildGraphTarget(value string) string {
	return compactKbuildGraphTargetPath(value)
}

// compactKbuildProfileGeneratedTargetPath canonicalizes values captured from
// Kbuild's obj-y, hostprogs, always-y, and related generated-target
// collections. Those collections are object-directory-relative even though
// their source assignment commonly contains a bare filename; rule targets and
// prerequisites, by contrast, are relative to Make's process cwd.
func compactKbuildProfileGeneratedTargetPath(profile CompactKbuildProfile, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return compactKbuildProfileTargetPath(profile, "$(obj)/"+value)
}

// CanonicalKbuildProfileGeneratedTarget returns the stable object-tree path
// of a target captured from a Kbuild generated-target collection.
func CanonicalKbuildProfileGeneratedTarget(profile CompactKbuildProfile, value string) string {
	return compactKbuildProfileGeneratedTargetPath(profile, value)
}

// CanonicalKbuildProfileTarget returns the stable source/object-tree-relative
// identity of a target in one evaluated Make invocation.
func CanonicalKbuildProfileTarget(profile CompactKbuildProfile, value string) string {
	return compactKbuildProfileTargetPath(profile, value)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
