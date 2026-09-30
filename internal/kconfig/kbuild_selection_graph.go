package kconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// compactKbuildSelectionKey identifies one concrete target in one evaluated
// Make invocation. A target pathname alone is not an identity: two invocations
// can evaluate the same declaration with different command-line variables or
// exported state.
type compactKbuildSelectionKey struct {
	profile         string
	target          string
	stage           string
	phonyStatusLine int
}

// compactKbuildGroupedSelectionID identifies one concrete invocation of a
// grouped Make rule. The selected target is deliberately absent: every peer
// instantiated from the same rule and stem is produced by one recipe run.
type compactKbuildGroupedSelectionID struct {
	profile   string
	ruleOrder int
	stem      string
}

// compactKbuildSelectionGraph is the validated, profile-exact index consumed
// by action lowering. owners contains only paths with one unambiguous terminal
// selected owner. Exact visible-artifact consumers resolve the recorded
// profile+target lineage instead, so unrelated invocations may use the same
// logical path when their physical output trees do not collide.
type compactKbuildSelectionGraph struct {
	profiles                     map[string]CompactKbuildProfile
	selections                   map[compactKbuildSelectionKey]CompactKbuildSelection
	selectionInitialArtifacts    map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact
	selectionNativePrerequisites map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact
	selectionGeneratedArtifacts  map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact
	deferredContentSelections    map[string]KbuildDeferredContentSelection
	owners                       map[string]compactKbuildSelectionKey
	// publishedOwners names the one canonical writer for each physical
	// output-tree/logical-path pair.  Earlier writers in a source-proven total
	// overwrite chain remain materialized, but publish at reserved immutable
	// ArtifactPaths instead of competing for the canonical Bazel output.
	publishedOwners           map[string]compactKbuildSelectionKey
	outputOwnerCandidates     map[string][]compactKbuildSelectionKey
	outputOwnersByPath        map[string][]compactKbuildSelectionKey
	shadowedPrimarySelections map[compactKbuildSelectionKey]bool
	overwriteDependencies     map[compactKbuildSelectionKey][]compactKbuildOverwriteDependency
	// sideOutputCandidateDependencies are ordering-only execution-provenance
	// edges. They force every action whose opaque side effects will be observed
	// to materialize before the exact consumer resolver, without registering
	// that action as the static owner of any logical path.
	sideOutputCandidateDependencies map[compactKbuildSelectionKey][]compactKbuildSelectionKey
	overwriteEdgesByOutput          map[string][]compactKbuildOverwriteEdge
	nativeOwners                    map[string][]compactKbuildSelectionKey
	selectionsByTarget              map[string][]compactKbuildSelectionKey
	selectionsByProfileTarget       map[compactKbuildProfileTargetKey]compactKbuildSelectionKey
	forwardingSelections            map[compactKbuildSelectionKey]bool
	materializedProducers           map[compactKbuildSelectionKey]string
	phonyStatusLines                map[compactKbuildSelectionKey]*compactKbuildSelectedPhonyStatus
	phonyStatusByOwner              map[compactKbuildSelectionKey]compactKbuildSelectionKey
	phonyStatusBeforeChild          map[string][]compactKbuildSelectionKey
	// materializedProducerGeneration invalidates process-local planner memos
	// whose result depends on exact selected producer ownership. Structural
	// selection policy is immutable before lowering; materialization is the one
	// ownership relation which advances while the ActionPlan is assembled.
	materializedProducerGeneration  uint64
	groupedSelectionIDs             map[compactKbuildSelectionKey]compactKbuildGroupedSelectionID
	groupedSelectionMembers         map[compactKbuildGroupedSelectionID][]compactKbuildSelectionKey
	groupedSelectionRepresentatives map[compactKbuildGroupedSelectionID]compactKbuildSelectionKey
	ordered                         []compactKbuildSelectionKey
	selectionsByProfileStage        map[compactKbuildProfileStageKey][]compactKbuildSelectionKey
	phonyTargets                    map[compactKbuildProfileTargetKey]bool
	generatedTargets                map[compactKbuildProfileTargetKey]bool
	targetInvocations               map[compactKbuildProfileTargetKey][]string
	// Source phases are file writers within one selected Make recipe. Their
	// selections have separate output identities, while this index retains the
	// real Make owner and the exact child boundary which follows each write.
	sourcePhasesBySelection map[compactKbuildSelectionKey]CompactKbuildSelectedSourcePhase
	sourcePhaseOwners       map[compactKbuildSelectionKey]compactKbuildSelectionKey
	sourcePhaseChildren     map[compactKbuildSelectionKey]string
	sourcePhaseBeforeChild  map[string]compactKbuildSelectionKey
	sourcePhasesByOwner     map[compactKbuildSelectionKey][]compactKbuildSelectionKey
	// invocationParents records the exact selected parent goal whose recipe
	// started a recursive Make child. Its prerequisites complete before that
	// child starts even when the goal is PHONY and has no selected file action.
	invocationParents map[string][]compactKbuildProfileTargetKey
	ruleResolutions   map[compactKbuildRuleResolutionKey]compactKbuildRuleResolution
	// selectedRootRuleResolutions is the bounded handoff between dependency
	// planning and final lowering. Planning already resolved every selected
	// root through its exact lexical Make target; retain only those successful
	// matches when the broader closure caches are released, then consume each
	// entry once from buildSelectedTarget.
	selectedRootRuleResolutions map[compactKbuildSelectedRuleResolutionKey]compactKbuildRuleResolution
	targetRuleContexts          map[compactKbuildTargetRuleContextKey]compactKbuildTargetRuleContext
	sourceEvidence              map[compactKbuildRuleResolutionKey]bool
	nativeDependencies          map[compactKbuildSelectionMetadataKey]compactKbuildSelectionDependencies
	// resolvedDependencies memoizes the final grouped dependency union. The
	// graph is planned serially, and the cache is invalidated at each structural
	// mutation boundary before action lowering begins. Only successful results
	// are retained; callers always receive a copy.
	resolvedDependencies         map[compactKbuildSelectionMetadataKey][]compactKbuildSelectionKey
	invocationPredecessors       map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies
	invocationRecipeSelections   map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies
	parentPrerequisiteSelections map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies
	terminalSelections           map[compactKbuildTerminalSelectionKey]compactKbuildSelectionDependencies
	unruledPrerequisites         map[compactKbuildSelectionMetadataKey]compactKbuildPrerequisitePaths
	cacheMisses                  compactKbuildSelectionGraphCacheMisses
}

type compactKbuildSelectionGraphCacheMisses struct {
	ruleResolution        int
	ruleEvaluation        int
	targetRuleContext     int
	sourceEvidence        int
	nativeDependencies    int
	invocationPredecessor int
	terminalSelection     int
	unruledPrerequisite   int
}

// SelectKbuildOutput narrows an evaluated invocation graph to the dependency
// closure of one unambiguous file output. Invocation profiles remain intact:
// their parent goals provide the source-defined ordering of recursive Make
// prerequisites even when the parent command is outside the selected closure.
func (m *CompactMetadata) SelectKbuildOutput(target string) error {
	graph, err := newCompactKbuildSelectionGraph(m.Config)
	if err != nil {
		return err
	}
	if err := graph.prepareGroupedSelections(m, m.Config); err != nil {
		return err
	}
	owner, exists := graph.owners[target]
	if !exists {
		return fmt.Errorf("Kbuild output %q has no unambiguous selected owner", target)
	}
	if graph.compactKbuildProfileTargetIsPhony(graph.profiles[owner.profile], target) {
		return fmt.Errorf("Kbuild output %q is PHONY", target)
	}
	selected := map[compactKbuildSelectionKey]bool{}
	var visit func(compactKbuildSelectionKey) error
	visit = func(key compactKbuildSelectionKey) error {
		key = graph.compactKbuildGroupedSelectionRepresentative(key)
		if selected[key] {
			return nil
		}
		for _, member := range graph.compactKbuildGroupedSelectionMembers(key) {
			selected[member] = true
		}
		dependencies, err := graph.selectionDependencies(m, key)
		if err != nil {
			return err
		}
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(owner); err != nil {
		return fmt.Errorf("select Kbuild output %q: %w", target, err)
	}
	selections := make([]CompactKbuildSelection, 0, len(selected))
	for _, key := range graph.ordered {
		if selected[key] {
			selections = append(selections, graph.selections[key])
		}
	}
	m.Config.KbuildSelections = selections
	m.validatedSelectionGraph, err = newCompactKbuildSelectionGraph(m.Config)
	return err
}

type compactKbuildProfileStageKey struct {
	profile string
	stage   int
}

type compactKbuildProfileTargetKey struct {
	profile string
	target  string
}

type compactKbuildRuleResolutionKey struct {
	metadata *CompactMetadata
	compactKbuildProfileTargetKey
	makeTarget string
}

type compactKbuildRuleResolution struct {
	match   compactKbuildRuleMatch
	matched bool
	err     error
}

type compactKbuildSelectedRuleResolutionKey struct {
	metadata   *CompactMetadata
	selection  compactKbuildSelectionKey
	makeTarget string
}

type compactKbuildTargetRuleContext struct {
	normal    []compactKbuildEvaluatedPath
	orderOnly []compactKbuildEvaluatedPath
	stem      string
	err       error
}

// compactKbuildTargetRuleContextKey retains the lexical target GNU Make used
// to select an implicit rule. Several parent-traversal spellings can name one
// canonical graph target while selecting different pattern declarations and
// stems, so neither the canonical target nor the selected rule alone is a
// sufficient evaluation-cache identity.
type compactKbuildTargetRuleContextKey struct {
	compactKbuildProfileTargetKey
	makeTarget  string
	ruleOrder   int
	targetOrder int
	stem        string
}

type compactKbuildSelectionMetadataKey struct {
	metadata  *CompactMetadata
	selection compactKbuildSelectionKey
}

type compactKbuildSelectionDependencies struct {
	dependencies []compactKbuildSelectionKey
	err          error
}

type compactKbuildInvocationPredecessorKey struct {
	metadata *CompactMetadata
	profile  string
	stage    int
}

type compactKbuildTerminalSelectionKey struct {
	metadata *CompactMetadata
	profile  string
	stage    int
}

type compactKbuildPrerequisitePaths struct {
	paths []string
	err   error
}

// compactKbuildProfileHasCanonicalTargetEvidence verifies that a serialized
// root-relative graph identity is supported by its evaluated Make invocation.
// The ordinary profile canonicalizer remains useful as a validation oracle for
// cwd-local values, but it must not be used to rewrite an identity which was
// explicitly source/object-rooted before serialization.
func compactKbuildProfileHasCanonicalTargetEvidence(profile CompactKbuildProfile, target string) bool {
	if target == "" || compactKbuildGraphTargetPath(target) != target {
		return false
	}
	if compactKbuildProfileTargetPath(profile, target) == target {
		return true
	}
	for _, entry := range profile.EntryTargets {
		if entry == target {
			return true
		}
	}
	for _, generated := range profile.Generated {
		if compactKbuildProfileGeneratedTargetPath(profile, generated.Target) == target {
			return true
		}
	}
	for _, rule := range profile.Rules {
		for _, rawTarget := range rule.Targets {
			pattern := compactKbuildProfileTargetPath(profile, rawTarget)
			if _, ok := matchKbuildRulePattern(pattern, target); ok {
				return true
			}
		}
		if rule.TargetPattern != "" {
			pattern := compactKbuildProfileTargetPath(profile, rule.TargetPattern)
			if _, ok := matchKbuildRulePattern(pattern, target); ok {
				return true
			}
		}
	}
	return false
}

// compactKbuildProfileHasSelectionTargetEvidence accepts either an ordinary
// canonical declaration or a declaration selected through Make's lexical root
// spelling. The latter is required for $(obj)/% rules reached through parent
// traversal: cleaning makeTarget first would erase the only matching stem.
func compactKbuildProfileHasSelectionTargetEvidence(
	profile CompactKbuildProfile,
	target, makeTarget string,
) bool {
	if compactKbuildProfileHasCanonicalTargetEvidence(profile, target) {
		return true
	}
	return len(compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget)) != 0
}

// compactKbuildVisibleArtifactsForPath returns the exact provenance records in
// a serialized selection subset. Profile frontiers use the process-local view
// accessors below instead.
func compactKbuildVisibleArtifactsForPath(
	artifacts []CompactKbuildVisibleArtifact,
	target string,
) []CompactKbuildVisibleArtifact {
	first := sort.Search(len(artifacts), func(index int) bool {
		return artifacts[index].Path >= target
	})
	if first == len(artifacts) || artifacts[first].Path != target {
		return nil
	}
	last := first + 1
	for last < len(artifacts) && artifacts[last].Path == target {
		last++
	}
	return artifacts[first:last]
}

func compactKbuildProfileInitialVisibleArtifact(
	profile CompactKbuildProfile,
	target string,
) (CompactKbuildVisibleArtifact, bool) {
	return CompactKbuildProfileInitialVisibleArtifact(profile, target)
}

func compactKbuildProfileHasInitialVisibleArtifact(
	profile CompactKbuildProfile,
	artifact CompactKbuildVisibleArtifact,
) bool {
	candidate, ok := compactKbuildProfileInitialVisibleArtifact(profile, artifact.Path)
	return ok && candidate == artifact
}

func (g *compactKbuildSelectionGraph) compactKbuildInitialVisibleArtifact(
	profileName, target string,
) (CompactKbuildVisibleArtifact, bool) {
	if g == nil {
		return CompactKbuildVisibleArtifact{}, false
	}
	profile, ok := g.profiles[profileName]
	if !ok {
		return CompactKbuildVisibleArtifact{}, false
	}
	return compactKbuildProfileInitialVisibleArtifact(profile, target)
}

func newCompactKbuildSelectionGraph(config CompactConfig) (*compactKbuildSelectionGraph, error) {
	graph := &compactKbuildSelectionGraph{
		profiles:                        make(map[string]CompactKbuildProfile, len(config.KbuildProfiles)),
		selections:                      make(map[compactKbuildSelectionKey]CompactKbuildSelection, len(config.KbuildSelections)),
		selectionInitialArtifacts:       make(map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact, len(config.KbuildSelections)),
		selectionNativePrerequisites:    make(map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact, len(config.KbuildSelections)),
		selectionGeneratedArtifacts:     make(map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact, len(config.KbuildSelections)),
		deferredContentSelections:       make(map[string]KbuildDeferredContentSelection, len(config.KbuildDeferredContentSelections)),
		owners:                          make(map[string]compactKbuildSelectionKey, len(config.KbuildSelections)),
		publishedOwners:                 make(map[string]compactKbuildSelectionKey, len(config.KbuildSelections)),
		outputOwnerCandidates:           make(map[string][]compactKbuildSelectionKey),
		outputOwnersByPath:              make(map[string][]compactKbuildSelectionKey),
		shadowedPrimarySelections:       make(map[compactKbuildSelectionKey]bool),
		overwriteDependencies:           make(map[compactKbuildSelectionKey][]compactKbuildOverwriteDependency),
		sideOutputCandidateDependencies: make(map[compactKbuildSelectionKey][]compactKbuildSelectionKey),
		overwriteEdgesByOutput:          make(map[string][]compactKbuildOverwriteEdge),
		nativeOwners:                    make(map[string][]compactKbuildSelectionKey),
		selectionsByTarget:              make(map[string][]compactKbuildSelectionKey),
		selectionsByProfileTarget:       make(map[compactKbuildProfileTargetKey]compactKbuildSelectionKey),
		forwardingSelections:            make(map[compactKbuildSelectionKey]bool),
		materializedProducers:           make(map[compactKbuildSelectionKey]string),
		groupedSelectionIDs:             make(map[compactKbuildSelectionKey]compactKbuildGroupedSelectionID),
		groupedSelectionMembers:         make(map[compactKbuildGroupedSelectionID][]compactKbuildSelectionKey),
		groupedSelectionRepresentatives: make(map[compactKbuildGroupedSelectionID]compactKbuildSelectionKey),
		selectionsByProfileStage:        make(map[compactKbuildProfileStageKey][]compactKbuildSelectionKey),
		phonyTargets:                    make(map[compactKbuildProfileTargetKey]bool),
		generatedTargets:                make(map[compactKbuildProfileTargetKey]bool),
		targetInvocations:               make(map[compactKbuildProfileTargetKey][]string),
		sourcePhasesBySelection:         make(map[compactKbuildSelectionKey]CompactKbuildSelectedSourcePhase),
		sourcePhaseOwners:               make(map[compactKbuildSelectionKey]compactKbuildSelectionKey),
		sourcePhaseChildren:             make(map[compactKbuildSelectionKey]string),
		sourcePhaseBeforeChild:          make(map[string]compactKbuildSelectionKey),
		sourcePhasesByOwner:             make(map[compactKbuildSelectionKey][]compactKbuildSelectionKey),
		invocationParents:               make(map[string][]compactKbuildProfileTargetKey),
		ruleResolutions:                 make(map[compactKbuildRuleResolutionKey]compactKbuildRuleResolution),
		selectedRootRuleResolutions:     make(map[compactKbuildSelectedRuleResolutionKey]compactKbuildRuleResolution),
		targetRuleContexts:              make(map[compactKbuildTargetRuleContextKey]compactKbuildTargetRuleContext),
		sourceEvidence:                  make(map[compactKbuildRuleResolutionKey]bool),
		nativeDependencies:              make(map[compactKbuildSelectionMetadataKey]compactKbuildSelectionDependencies),
		resolvedDependencies:            make(map[compactKbuildSelectionMetadataKey][]compactKbuildSelectionKey),
		invocationPredecessors:          make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies),
		invocationRecipeSelections:      make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies),
		parentPrerequisiteSelections:    make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies),
		terminalSelections:              make(map[compactKbuildTerminalSelectionKey]compactKbuildSelectionDependencies),
		unruledPrerequisites:            make(map[compactKbuildSelectionMetadataKey]compactKbuildPrerequisitePaths),
	}
	selectedTargets := make(map[compactKbuildProfileTargetKey]compactKbuildSelectionKey, len(config.KbuildSelections))
	for _, profile := range config.KbuildProfiles {
		if profile.Name == "" {
			return nil, fmt.Errorf("Kbuild profile has an empty identity")
		}
		if _, exists := graph.profiles[profile.Name]; exists {
			return nil, fmt.Errorf("Kbuild profile identity %q is repeated", profile.Name)
		}
		graph.profiles[profile.Name] = profile
		for _, generated := range profile.Generated {
			candidate := compactKbuildProfileGeneratedTargetPath(profile, generated.Target)
			graph.generatedTargets[compactKbuildProfileTargetKey{profile: profile.Name, target: candidate}] = true
		}
		for _, rule := range profile.Rules {
			compactKbuildVisitRulePhonyTargets(profile, rule, func(target string) {
				graph.phonyTargets[compactKbuildProfileTargetKey{profile: profile.Name, target: target}] = true
			})
		}
	}
	for _, selection := range config.KbuildDeferredContentSelections {
		if selection.Token == "" || kbuildDeferredContentTokenPattern.FindString(selection.Token) != selection.Token {
			return nil, fmt.Errorf("deferred Kbuild content selection has invalid token %q", selection.Token)
		}
		if _, exists := graph.deferredContentSelections[selection.Token]; exists {
			return nil, fmt.Errorf("deferred Kbuild content selection token %q is repeated", selection.Token)
		}
		profile, ok := graph.profiles[selection.Profile]
		if !ok {
			return nil, fmt.Errorf("deferred Kbuild content selection %q references missing profile %q", selection.Token, selection.Profile)
		}
		query, ok := profile.deferredContentQueries[selection.Token]
		if !ok || query.Target != selection.Target {
			return nil, fmt.Errorf(
				"deferred Kbuild content selection %q origin %s:%s has no matching source query",
				selection.Token, selection.Profile, selection.Target,
			)
		}
		if selection.Lifecycle != "prep" && selection.Lifecycle != "target" {
			return nil, fmt.Errorf("deferred Kbuild content selection %q has invalid lifecycle %q", selection.Token, selection.Lifecycle)
		}
		if selection.Scope != "host" && selection.Scope != "target" {
			return nil, fmt.Errorf("deferred Kbuild content selection %q has invalid scope %q", selection.Token, selection.Scope)
		}
		if !compactKbuildSelectionStage(selection.Stage) {
			return nil, fmt.Errorf("deferred Kbuild content selection %q has invalid stage %q", selection.Token, selection.Stage)
		}
		graph.deferredContentSelections[selection.Token] = selection
	}
	for _, profile := range config.KbuildProfiles {
		seen := map[string]bool{}
		controls := map[CompactKbuildInvocationControlPrerequisite]bool{}
		for _, prerequisite := range profile.InvocationControlPrerequisites {
			parent, exists := graph.profiles[prerequisite.Profile]
			if !exists || prerequisite.Profile == profile.Name || controls[prerequisite] ||
				compactKbuildGraphTargetPath(prerequisite.Target) != prerequisite.Target ||
				!graph.compactKbuildProfileTargetIsPhony(parent, prerequisite.Target) {
				return nil, fmt.Errorf("Kbuild invocation %q has invalid completed control prerequisite %#v", profile.Name, prerequisite)
			}
			controls[prerequisite] = true
		}
		for _, predecessor := range profile.InvocationPredecessors {
			if predecessor == "" || predecessor == profile.Name || seen[predecessor] {
				return nil, fmt.Errorf("Kbuild profile %q has invalid invocation predecessor %q", profile.Name, predecessor)
			}
			if _, exists := graph.profiles[predecessor]; !exists {
				return nil, fmt.Errorf("Kbuild profile %q references missing invocation predecessor %q", profile.Name, predecessor)
			}
			seen[predecessor] = true
		}
		for _, dependency := range profile.TargetInvocationDependencies {
			target := compactKbuildGraphTargetPath(dependency.Target)
			if target == "" || target != dependency.Target || !compactKbuildProfileHasCanonicalTargetEvidence(profile, target) {
				return nil, fmt.Errorf("Kbuild profile %q has invalid recursive invocation owner target %q", profile.Name, dependency.Target)
			}
			if dependency.Profile == "" || dependency.Profile == profile.Name {
				return nil, fmt.Errorf("Kbuild profile %q target %q has invalid recursive invocation dependency %q", profile.Name, target, dependency.Profile)
			}
			_, exists := graph.profiles[dependency.Profile]
			if !exists {
				return nil, fmt.Errorf("Kbuild profile %q target %q references missing recursive invocation %q", profile.Name, target, dependency.Profile)
			}
			for _, goal := range dependency.Goals {
				if canonical := compactKbuildGraphTargetPath(goal); canonical == "" || canonical != goal || !compactKbuildProfileHasCanonicalTargetEvidence(graph.profiles[dependency.Profile], goal) {
					return nil, fmt.Errorf("Kbuild profile %q target %q recursive invocation %q has invalid canonical goal %q (want %q)", profile.Name, target, dependency.Profile, goal, canonical)
				}
			}
			key := compactKbuildProfileTargetKey{profile: profile.Name, target: target}
			graph.targetInvocations[key] = append(graph.targetInvocations[key], dependency.Profile)
			if !dependency.Prerequisite {
				graph.invocationParents[dependency.Profile] = append(graph.invocationParents[dependency.Profile], key)
			}
		}
	}

	for _, selection := range config.KbuildSelections {
		profile, ok := graph.profiles[selection.Profile]
		if !ok {
			return nil, fmt.Errorf("Kbuild selection references missing profile %q", selection.Profile)
		}
		normalized, normalizeErr := normalizeCompactKbuildSelectionMakeTarget(profile, selection)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		selection.MakeTarget = normalized.MakeTarget
		if _, _, err := compactKbuildSelectionLifecycleScope(selection); err != nil {
			return nil, err
		}
		if !compactKbuildSelectionStage(selection.Stage) {
			return nil, fmt.Errorf(
				"Kbuild selection %s target %q has unsupported stage %q",
				selection.Profile, selection.Target, selection.Stage,
			)
		}
		initialArtifacts, artifactsErr := compactKbuildSelectionInitialObjectTreeArtifacts(selection)
		if artifactsErr != nil {
			return nil, fmt.Errorf("Kbuild selection %s target %q has invalid initial object-tree artifacts: %w", selection.Profile, selection.Target, artifactsErr)
		}
		nativePrerequisites, nativeErr := compactKbuildSelectionNativePrerequisiteArtifacts(selection)
		if nativeErr != nil {
			return nil, fmt.Errorf("Kbuild selection %s target %q has invalid native prerequisites: %w", selection.Profile, selection.Target, nativeErr)
		}
		generatedArtifacts, generatedErr := compactKbuildSelectionGeneratedObjectTreeArtifacts(selection)
		if generatedErr != nil {
			return nil, fmt.Errorf("Kbuild selection %s target %q has invalid generated object-tree artifacts: %w", selection.Profile, selection.Target, generatedErr)
		}
		if !selection.UsesInitialObjectTree && len(initialArtifacts) != 0 {
			return nil, fmt.Errorf("Kbuild selection %s target %q records initial object-tree artifacts without using the initial object tree", selection.Profile, selection.Target)
		}
		previousInitialPath := ""
		for _, artifact := range initialArtifacts {
			canonical := canonicalKbuildRulePath(artifact.Path)
			pathErr := validatePlanRelativePath("initial visible Kbuild artifact", artifact.Path)
			if canonical == "" || canonical != artifact.Path || strings.ContainsAny(artifact.Path, "%$") ||
				strings.HasSuffix(artifact.Path, "/") || pathErr != nil ||
				(previousInitialPath != "" && artifact.Path <= previousInitialPath) {
				return nil, fmt.Errorf("Kbuild selection %s target %q has invalid, unsorted, or duplicate initial object-tree artifact path %q", selection.Profile, selection.Target, artifact.Path)
			}
			if !compactKbuildProfileHasInitialVisibleArtifact(profile, artifact) {
				return nil, fmt.Errorf(
					"Kbuild selection %s target %q initial object-tree artifact %#v is outside its exact visible frontier",
					selection.Profile, selection.Target, artifact,
				)
			}
			previousInitialPath = artifact.Path
		}
		previousNativePath := ""
		for _, artifact := range nativePrerequisites {
			canonical := canonicalKbuildRulePath(artifact.Path)
			producerProfile, producerExists := graph.profiles[artifact.Profile]
			producerTarget := compactKbuildGraphTargetPath(artifact.Target)
			pathErr := validatePlanRelativePath("native Kbuild prerequisite", artifact.Path)
			if canonical == "" || canonical != artifact.Path || strings.ContainsAny(artifact.Path, "%$") ||
				strings.HasSuffix(artifact.Path, "/") || pathErr != nil ||
				(previousNativePath != "" && artifact.Path <= previousNativePath) || artifact.Path != artifact.Target ||
				!producerExists || producerTarget != artifact.Target ||
				!compactKbuildProfileHasCanonicalTargetEvidence(producerProfile, producerTarget) {
				return nil, fmt.Errorf(
					"Kbuild selection %s target %q has invalid, ambiguous, or noncanonical native prerequisite artifact %#v",
					selection.Profile, selection.Target, artifact,
				)
			}
			previousNativePath = artifact.Path
		}
		previousGeneratedPath := ""
		for _, artifact := range generatedArtifacts {
			canonical := canonicalKbuildRulePath(artifact.Path)
			producerProfile, producerExists := graph.profiles[artifact.Profile]
			producerTarget := compactKbuildGraphTargetPath(artifact.Target)
			if canonical == "" || canonical != artifact.Path || strings.ContainsAny(artifact.Path, "%$") ||
				(previousGeneratedPath != "" && artifact.Path <= previousGeneratedPath) || artifact.Path != artifact.Target ||
				!producerExists || producerTarget != artifact.Target ||
				!compactKbuildProfileHasCanonicalTargetEvidence(producerProfile, producerTarget) {
				return nil, fmt.Errorf(
					"Kbuild selection %s target %q has invalid, ambiguous, or noncanonical generated object-tree artifact %#v",
					selection.Profile, selection.Target, artifact,
				)
			}
			previousGeneratedPath = artifact.Path
		}
		if err := validatePlanRelativePath("Kbuild selection target", selection.Target); err != nil {
			return nil, err
		}
		if strings.ContainsAny(selection.Target, "%$") {
			return nil, fmt.Errorf(
				"Kbuild selection %s target %q is not a concrete target",
				selection.Profile, selection.Target,
			)
		}
		target := compactKbuildGraphTargetPath(selection.Target)
		makeTarget := selection.MakeTarget
		if target != selection.Target ||
			selection.SourceScriptPhase == "" && !compactKbuildProfileHasSelectionTargetEvidence(profile, target, makeTarget) {
			want := compactKbuildProfileTargetPath(profile, selection.Target)
			return nil, fmt.Errorf(
				"Kbuild selection %s target %q is not canonical for profile directory %q (want %q)",
				selection.Profile, selection.Target, profile.Directory, want,
			)
		}
		if selection.GroupedTrigger != "" {
			trigger := compactKbuildGraphTargetPath(selection.GroupedTrigger)
			triggerEvidence := compactKbuildProfileHasCanonicalTargetEvidence(profile, trigger)
			if !triggerEvidence {
				for _, candidate := range config.KbuildSelections {
					if candidate.Profile == selection.Profile && candidate.Target == trigger &&
						compactKbuildProfileHasSelectionTargetEvidence(
							profile, trigger, candidate.MakeTarget,
						) {
						triggerEvidence = true
						break
					}
				}
			}
			if trigger != selection.GroupedTrigger || !triggerEvidence {
				return nil, fmt.Errorf(
					"Kbuild selection %s target %q has invalid grouped trigger %q",
					selection.Profile, selection.Target, selection.GroupedTrigger,
				)
			}
		}
		key := compactKbuildSelectionKey{
			profile: selection.Profile,
			target:  target,
			stage:   selection.Stage,
		}
		if _, duplicate := graph.selections[key]; duplicate {
			continue
		}
		selectionTargetKey := compactKbuildProfileTargetKey{profile: selection.Profile, target: target}
		if previous, exists := selectedTargets[selectionTargetKey]; exists {
			// Stage is traversal ordering, not part of a Make artifact's native
			// identity. The same exact invocation can reach one target through
			// several parent closures (for example a generated prerequisite used
			// by both a host program and prepare). Materialize it once at the
			// earliest demanded stage.
			if compactKbuildSelectionStageOrder(previous.stage) <= compactKbuildSelectionStageOrder(key.stage) {
				continue
			}
			delete(graph.selections, previous)
			delete(graph.selectionInitialArtifacts, previous)
			delete(graph.selectionNativePrerequisites, previous)
			delete(graph.selectionGeneratedArtifacts, previous)
		}
		selection.Target = target
		graph.selections[key] = selection
		graph.selectionInitialArtifacts[key] = initialArtifacts
		graph.selectionNativePrerequisites[key] = nativePrerequisites
		graph.selectionGeneratedArtifacts[key] = generatedArtifacts
		selectedTargets[selectionTargetKey] = key
	}
	if err := graph.indexSelectedSourcePhases(config); err != nil {
		return nil, err
	}
	for profileTarget, key := range selectedTargets {
		graph.selectionsByProfileTarget[profileTarget] = key
		profile := graph.profiles[key.profile]
		if graph.compactKbuildProfileTargetIsPhony(profile, key.target) {
			continue
		}
		graph.selectionsByTarget[key.target] = append(graph.selectionsByTarget[key.target], key)
	}
	for target, candidates := range graph.selectionsByTarget {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].profile != candidates[j].profile {
				return candidates[i].profile < candidates[j].profile
			}
			return compactKbuildSelectionStageOrder(candidates[i].stage) < compactKbuildSelectionStageOrder(candidates[j].stage)
		})
		terminal := make([]compactKbuildSelectionKey, 0, len(candidates))
		for _, candidate := range candidates {
			hasDescendant := false
			materializesBeforeDescendant := false
			for _, other := range candidates {
				if candidate != other && graph.compactKbuildInvocationDescendsTo(candidate.profile, target, other.profile) {
					hasDescendant = true
					artifact, ok := graph.compactKbuildInitialVisibleArtifact(other.profile, target)
					if ok && artifact == (CompactKbuildVisibleArtifact{
						Path: target, Profile: candidate.profile, Target: candidate.target,
					}) {
						// The descendant started from this exact same-path version, so
						// the parent is a real writer followed by an overwrite, not a
						// recursive-Make forwarding target.
						materializesBeforeDescendant = true
						break
					}
				}
			}
			if hasDescendant && !materializesBeforeDescendant {
				graph.forwardingSelections[candidate] = true
				continue
			}
			terminal = append(terminal, candidate)
		}
		if len(terminal) == 0 {
			return nil, fmt.Errorf("Kbuild native artifact %q has no terminal selected owner", target)
		}
		ownersByTree := map[string][]compactKbuildSelectionKey{}
		for _, owner := range terminal {
			selection := graph.selections[owner]
			tree := compactKbuildSelectionPlanContext(config, selection).OutputTree
			ownersByTree[tree] = append(ownersByTree[tree], owner)
		}
		published := make([]compactKbuildSelectionKey, 0, len(ownersByTree))
		trees := make([]string, 0, len(ownersByTree))
		for tree := range ownersByTree {
			trees = append(trees, tree)
		}
		sort.Strings(trees)
		for _, tree := range trees {
			orderedOwners, orderErr := graph.compactKbuildRegisterOutputOwners(target, tree, ownersByTree[tree], true)
			if orderErr != nil {
				return nil, orderErr
			}
			publisher := orderedOwners[len(orderedOwners)-1]
			published = append(published, publisher)
		}
		// Prehost and host each own a separate physical tree, but selected
		// preparation outputs from both are copied into one prep tree. Decide
		// that tree's publisher from the same source overwrite provenance
		// before either native action or mirror is materialized. Its earlier
		// versions keep private prep paths even when each native stage has a
		// canonical output of its own.
		mirrorOwners := []compactKbuildSelectionKey{}
		for _, owner := range terminal {
			selection := graph.selections[owner]
			if selection.Lifecycle == "prep" && selection.Scope == "host" &&
				(selection.Stage == "prehost" || selection.Stage == "host") {
				mirrorOwners = append(mirrorOwners, owner)
			}
		}
		if len(mirrorOwners) != 0 {
			if _, orderErr := graph.compactKbuildRegisterOutputOwners(target, "prep", mirrorOwners, false); orderErr != nil {
				return nil, fmt.Errorf("select host preparation mirror publisher for %q: %w", target, orderErr)
			}
		}
		graph.nativeOwners[target] = published
		if len(published) == 1 {
			graph.owners[target] = published[0]
		}
	}
	// Owner registration above is a structural batch. Rebuild the dependency
	// projection once after every path has its final overwrite chain; rebuilding
	// the union after each independent path makes unique-output graphs quadratic.
	graph.compactKbuildRebuildOverwriteDependencies()
	for key := range graph.selections {
		graph.ordered = append(graph.ordered, key)
	}

	sort.Slice(graph.ordered, func(i, j int) bool {
		left, right := graph.ordered[i], graph.ordered[j]
		if compactKbuildSelectionStageOrder(left.stage) != compactKbuildSelectionStageOrder(right.stage) {
			return compactKbuildSelectionStageOrder(left.stage) < compactKbuildSelectionStageOrder(right.stage)
		}
		if left.profile != right.profile {
			return left.profile < right.profile
		}
		return left.target < right.target
	})
	for _, key := range graph.ordered {
		index := compactKbuildProfileStageKey{
			profile: key.profile,
			stage:   compactKbuildSelectionStageOrder(key.stage),
		}
		graph.selectionsByProfileStage[index] = append(graph.selectionsByProfileStage[index], key)
	}
	return graph, nil
}

// indexSelectedSourcePhases binds each intermediate source-script write to the
// selected Make rule which invoked its exact source and to the child Make
// process which starts after that write. No phase is inferred from its output
// filename or promoted into a fabricated Make rule.
func (g *compactKbuildSelectionGraph) indexSelectedSourcePhases(config CompactConfig) error {
	seenOutputs := map[string]compactKbuildSelectionKey{}
	for _, profile := range config.KbuildProfiles {
		if len(profile.SelectedSourceScriptPhases) == 0 {
			continue
		}
		groups := map[string][]CompactKbuildSelectedSourcePhase{}
		for _, phase := range profile.SelectedSourceScriptPhases {
			if phase.OwnerTarget == "" || compactKbuildGraphTargetPath(phase.OwnerTarget) != phase.OwnerTarget ||
				phase.OutputPath == "" || compactKbuildGraphTargetPath(phase.OutputPath) != phase.OutputPath ||
				validatePlanRelativePath("source script phase output", phase.OutputPath) != nil {
				return fmt.Errorf("Kbuild profile %q has invalid source script phase output %q or owner %q", profile.Name, phase.OutputPath, phase.OwnerTarget)
			}
			groups[phase.OwnerTarget] = append(groups[phase.OwnerTarget], phase)
		}
		for ownerTarget, phases := range groups {
			var ownerKey compactKbuildSelectionKey
			selected := false
			for key := range g.selections {
				if key.profile == profile.Name && key.target == ownerTarget {
					if selected {
						return fmt.Errorf("source script phase owner %s:%s has ambiguous selected stages", profile.Name, ownerTarget)
					}
					ownerKey, selected = key, true
				}
			}
			if !selected || g.selections[ownerKey].SourceScriptPhase != "" ||
				g.compactKbuildProfileTargetIsPhony(profile, ownerTarget) {
				return fmt.Errorf("source script phases in profile %q have no real selected Make owner %q", profile.Name, ownerTarget)
			}
			if len(phases) != 2 {
				return fmt.Errorf("selected source script owner %s:%s has %d phases, want two ordered writes", profile.Name, ownerTarget, len(phases))
			}
			sort.Slice(phases, func(i, j int) bool { return phases[i].Ordinal < phases[j].Ordinal })
			if phases[0].Ordinal != 0 || phases[1].Ordinal != 1 ||
				phases[0].SourcePath != "scripts/link-vmlinux.sh" || phases[1].SourcePath != phases[0].SourcePath {
				return fmt.Errorf("selected source script owner %s:%s has unsupported phase order or source path", profile.Name, ownerTarget)
			}
			if len(phases[0].SourceArguments) == 0 ||
				!slices.Equal(phases[0].SourceArguments, phases[1].SourceArguments) {
				return fmt.Errorf("selected source script owner %s:%s has missing or inconsistent source arguments", profile.Name, ownerTarget)
			}
			source, err := readCompactKbuildProfileSource(profile, phases[0].SourcePath)
			if err != nil {
				return fmt.Errorf("read selected phase source for %s:%s: %w", profile.Name, ownerTarget, err)
			}
			analyzed, recognized, err := AnalyzeCompactKbuildLinkVmlinuxPhases(string(source))
			if err != nil || !recognized {
				return fmt.Errorf("authenticate selected phase source for %s:%s: recognized=%t, error=%v", profile.Name, ownerTarget, recognized, err)
			}
			wantPaths := [2]string{".version", "vmlinux.o"}
			wantSpans := [2][]CompactKbuildLinkVmlinuxSourceSpan{analyzed.VersionSpans, analyzed.ObjectSpans}
			for ordinal, phase := range phases {
				if phase.OutputPath != wantPaths[ordinal] || phase.SourceSHA256 != analyzed.SourceSHA256 ||
					!slices.Equal(phase.Spans, wantSpans[ordinal]) {
					return fmt.Errorf("selected source phase %s:%s output %q does not match exact source write %d", profile.Name, ownerTarget, phase.OutputPath, ordinal)
				}
			}
			owner := g.selections[ownerKey]
			candidates := compactKbuildRuleCandidatesForMakeTarget(profile, ownerTarget, owner.MakeTarget)
			matchingRules := 0
			var selectedRule compactKbuildResolvedRule
			entry := CompactKbuildSelectedControlRuleEntrySnapshot(profile, ownerTarget)
			for _, candidate := range candidates {
				if entry != nil && candidate.ruleOrder != entry.Line.RuleIndex ||
					!slices.Contains(candidate.rule.Prerequisites, phases[0].SourcePath) &&
						!slices.Contains(candidate.rule.OrderOnly, phases[0].SourcePath) ||
					len(candidate.rule.Recipe) == 0 {
					continue
				}
				matchingRules++
				selectedRule = candidate
			}
			if matchingRules != 1 {
				return fmt.Errorf("selected source phase owner %s:%s has %d exact Make rules declaring %q, want one", profile.Name, ownerTarget, matchingRules, phases[0].SourcePath)
			}
			arguments, err := compactKbuildSelectedPhaseRecipeArguments(profile, ownerTarget, selectedRule, phases[0].SourcePath)
			if err != nil {
				return fmt.Errorf("selected source phase owner %s:%s recipe invocation: %w", profile.Name, ownerTarget, err)
			}
			if !slices.Equal(arguments, phases[0].SourceArguments) {
				return fmt.Errorf("selected source phase owner %s:%s source arguments differ from the exact selected recipe", profile.Name, ownerTarget)
			}
			children := map[int]string{}
			for _, dependency := range profile.TargetInvocationDependencies {
				if dependency.Prerequisite || dependency.Target != ownerTarget {
					continue
				}
				ordinal := -1
				for index, phase := range phases {
					if dependency.SourcePhaseBefore == phase.OutputPath {
						ordinal = index
					}
				}
				if ordinal < 0 || children[ordinal] != "" {
					return fmt.Errorf("selected source phase owner %s:%s has unbound or repeated child boundary %q", profile.Name, ownerTarget, dependency.SourcePhaseBefore)
				}
				child, exists := g.profiles[dependency.Profile]
				wantMakefiles := [2]string{"scripts/Makefile.build", "scripts/Makefile.modpost"}
				if !exists || canonicalKbuildRulePath(child.Path) != wantMakefiles[ordinal] ||
					len(dependency.Goals) == 0 {
					return fmt.Errorf("selected source phase owner %s:%s child %q does not match source Make boundary %d", profile.Name, ownerTarget, dependency.Profile, ordinal)
				}
				children[ordinal] = dependency.Profile
			}
			if len(children) != len(phases) || children[0] == children[1] {
				return fmt.Errorf("selected source phase owner %s:%s has incomplete or ambiguous Make child boundaries", profile.Name, ownerTarget)
			}
			for ordinal, phase := range phases {
				key := compactKbuildSelectionKey{profile: profile.Name, target: phase.OutputPath, stage: ownerKey.stage}
				selection, exists := g.selections[key]
				kind := [2]string{"version", "object"}[ordinal]
				if !exists || selection.SourceScriptPhase != kind || selection.MakeTarget != phase.OutputPath ||
					selection.Lifecycle != owner.Lifecycle || selection.Scope != owner.Scope ||
					selection.GroupedTrigger != "" || selection.UsesInitialObjectTree ||
					selection.InitialObjectTreeArtifacts != "" || selection.NativePrerequisiteArtifacts != "" ||
					selection.GeneratedObjectTreeArtifacts != "" || selection.DeferredContentQueries != "" {
					return fmt.Errorf("selected source phase %s:%s output %q has no exact phase selection", profile.Name, ownerTarget, phase.OutputPath)
				}
				if previous, duplicate := seenOutputs[phase.OutputPath]; duplicate {
					return fmt.Errorf("source script output %q has ambiguous owners %s and %s", phase.OutputPath, compactKbuildSelectionKeyString(previous), compactKbuildSelectionKeyString(key))
				}
				if previous, duplicate := g.sourcePhaseBeforeChild[children[ordinal]]; duplicate {
					return fmt.Errorf("source script child %q has ambiguous predecessors %s and %s", children[ordinal], compactKbuildSelectionKeyString(previous), compactKbuildSelectionKeyString(key))
				}
				seenOutputs[phase.OutputPath] = key
				g.sourcePhasesBySelection[key] = phase
				g.sourcePhaseOwners[key] = ownerKey
				g.sourcePhaseChildren[key] = children[ordinal]
				g.sourcePhaseBeforeChild[children[ordinal]] = key
				g.sourcePhasesByOwner[ownerKey] = append(g.sourcePhasesByOwner[ownerKey], key)
			}
		}
	}
	for key, selection := range g.selections {
		if selection.SourceScriptPhase != "" {
			if _, recognized := g.sourcePhasesBySelection[key]; !recognized {
				return fmt.Errorf("Kbuild selection %s has an unauthenticated source script phase %q", compactKbuildSelectionKeyString(key), selection.SourceScriptPhase)
			}
		}
	}
	return nil
}

// A phase inherits argv from the one selected Make command that executed its
// parent source script. Evaluate recipe occurrences using their recorded
// line-local Make states, then require precisely one source-script invocation.
func compactKbuildSelectedPhaseRecipeArguments(
	profile CompactKbuildProfile, ownerTarget string, rule compactKbuildResolvedRule, sourcePath string,
) ([]string, error) {
	match := compactKbuildRuleMatch{
		profile: profile, rule: rule.rule, lookupTarget: rule.lookupTarget,
		stem: rule.stem, targetOrder: rule.targetOrder, ruleOrder: rule.ruleOrder, resolved: true,
	}
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(ownerTarget, match, nil)
	if err != nil {
		return nil, err
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(ownerTarget, match, nil)
	if err != nil {
		return nil, err
	}
	commands, indices, err := evaluatedKbuildRuleCommandSelectionsBySourceLine(
		ownerTarget, match, automatic, injected, true,
	)
	if err != nil {
		return nil, err
	}
	snapshots, err := compactKbuildSelectedRuleRecipeSnapshots(ownerTarget, match)
	if err != nil {
		return nil, err
	}
	var selectedArguments []string
	selected := false
	for index, command := range commands {
		lineProfile := profile
		if len(indices) != 0 {
			lineProfile = snapshots[indices[index]].Evaluation.Profile
		}
		values, err := evaluateCompactKbuildRecipeVariablesForMakeTarget(
			lineProfile, ownerTarget, rule.lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected, "CONFIG_SHELL",
		)
		if err != nil {
			return nil, err
		}
		arguments, err := CompactKbuildSelectedSourceScriptArguments(
			lineProfile, compactKbuildDirectRecipeText(lineProfile, command.Text), sourcePath,
			values["CONFIG_SHELL"],
		)
		if err != nil {
			if err.Error() == fmt.Sprintf("source script %q has no exact selected invocation", sourcePath) {
				continue
			}
			return nil, err
		}
		if selected {
			return nil, fmt.Errorf("source script %q has more than one selected recipe invocation", sourcePath)
		}
		selectedArguments, selected = arguments, true
	}
	if !selected {
		return nil, fmt.Errorf("source script %q has no exact selected recipe invocation", sourcePath)
	}
	return selectedArguments, nil
}

func (g *compactKbuildSelectionGraph) selectedSourcePhase(
	key compactKbuildSelectionKey,
) (CompactKbuildSelectedSourcePhase, compactKbuildSelectionKey, bool) {
	phase, ok := g.sourcePhasesBySelection[key]
	return phase, g.sourcePhaseOwners[key], ok
}

func (g *compactKbuildSelectionGraph) compactKbuildInvocationDescendsTo(parentProfile, target, descendantProfile string) bool {
	if g == nil || parentProfile == descendantProfile {
		return false
	}
	queue := append([]string(nil), g.targetInvocations[compactKbuildProfileTargetKey{profile: parentProfile, target: target}]...)
	seen := map[string]bool{parentProfile: true}
	for len(queue) != 0 {
		profile := queue[0]
		queue = queue[1:]
		if profile == descendantProfile {
			return true
		}
		if seen[profile] {
			continue
		}
		seen[profile] = true
		queue = append(queue, g.targetInvocations[compactKbuildProfileTargetKey{profile: profile, target: target}]...)
	}
	return false
}

// A child that starts from the parent's same-path version overwrites that
// version after the parent recipe. It must not become an input of its own
// source writer, whether that writer is selected directly or reached through
// an unselected subordinate rule.
func (g *compactKbuildSelectionGraph) compactKbuildChildOverwritesParentVersion(
	parentProfile, target, childProfile string,
) bool {
	artifact, visible := g.compactKbuildInitialVisibleArtifact(childProfile, target)
	return visible && artifact == (CompactKbuildVisibleArtifact{
		Path: target, Profile: parentProfile, Target: target,
	})
}

func compactKbuildSelectionStage(stage string) bool {
	switch stage {
	case "prehost", "bootstrap", "host", "prep", "target":
		return true
	default:
		return false
	}
}

func compactKbuildSelectionStageOrder(stage string) int {
	switch stage {
	case "prehost":
		return 0
	case "bootstrap":
		return 1
	case "host":
		return 2
	case "prep":
		return 3
	case "target":
		return 4
	default:
		return 5
	}
}

func compactKbuildSelectionKeyString(key compactKbuildSelectionKey) string {
	if key.phonyStatusLine != 0 {
		return fmt.Sprintf("(%s, %s, %s, recipe %d)", key.profile, key.target, key.stage, key.phonyStatusLine-1)
	}
	return fmt.Sprintf("(%s, %s, %s)", key.profile, key.target, key.stage)
}

// compactKbuildSelectionKeyLess orders graph identities directly instead of
// formatting them in sort comparators. Besides avoiding one allocation per
// comparison, field-wise ordering distinguishes keys whose diagnostic string
// happens to be ambiguous because a field contains the ", " separator.
func compactKbuildSelectionKeyLess(left, right compactKbuildSelectionKey) bool {
	if left.profile != right.profile {
		return left.profile < right.profile
	}
	if left.target != right.target {
		return left.target < right.target
	}
	if left.stage != right.stage {
		return left.stage < right.stage
	}
	return left.phonyStatusLine < right.phonyStatusLine
}

func (g *compactKbuildSelectionGraph) selection(key compactKbuildSelectionKey) (CompactKbuildSelection, bool) {
	if g == nil {
		return CompactKbuildSelection{}, false
	}
	selection, ok := g.selections[key]
	return selection, ok
}

// compactKbuildSelectedPhonyPrerequisite retains an execution-only target in
// the same Make invocation as its consumer. PHONY names do not own regular
// output paths and therefore cannot be resolved through path-owner indexes.
func (g *compactKbuildSelectionGraph) compactKbuildSelectedPhonyPrerequisite(
	consumer compactKbuildSelectionKey, target string,
) (compactKbuildSelectionKey, bool) {
	if g == nil {
		return compactKbuildSelectionKey{}, false
	}
	key, selected := g.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: consumer.profile, target: compactKbuildGraphTargetPath(target),
	}]
	if !selected || key == consumer || !g.compactKbuildProfileTargetIsPhony(g.profiles[key.profile], key.target) {
		return compactKbuildSelectionKey{}, false
	}
	return key, true
}

// A source-selected PHONY recipe may publish only shell status, so the
// artifact-oriented rule matcher can decline it. Its recorded rule entry still
// owns the exact prerequisite context, including a second-expanded rule stem.
func (g *compactKbuildSelectionGraph) compactKbuildSelectedPhonyRuleForMakeTarget(
	metadata *CompactMetadata, profile CompactKbuildProfile, target, makeTarget string,
) (compactKbuildRuleMatch, bool, error) {
	entry := CompactKbuildSelectedControlRuleEntrySnapshot(profile, target)
	if entry == nil {
		return g.compactKbuildRuleForProfileMakeTarget(metadata, profile, target, makeTarget)
	}
	if entry.Line.Target != target || entry.Profile.Name != profile.Name ||
		entry.Line.RuleIndex < 0 || entry.Line.RuleIndex >= len(profile.Rules) {
		return compactKbuildRuleMatch{}, false, fmt.Errorf("selected PHONY target %q in profile %q has no valid source rule entry", target, profile.Name)
	}
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget) {
		if candidate.ruleOrder != entry.Line.RuleIndex || candidate.stem != entry.Line.Stem ||
			candidate.lookupTarget != entry.Line.LookupTarget {
			continue
		}
		return compactKbuildRuleMatch{
			profile: profile, rule: profile.Rules[candidate.ruleOrder],
			lookupTarget: candidate.lookupTarget, stem: candidate.stem,
			targetOrder: candidate.targetOrder, ruleOrder: candidate.ruleOrder,
			explicit: !strings.Contains(candidate.target, "%"), resolved: true,
		}, true, nil
	}
	return compactKbuildRuleMatch{}, false, fmt.Errorf("selected PHONY target %q in profile %q has no evaluated source rule for %q", target, profile.Name, makeTarget)
}

func (g *compactKbuildSelectionGraph) profile(name string) (CompactKbuildProfile, bool) {
	if g == nil {
		return CompactKbuildProfile{}, false
	}
	profile, ok := g.profiles[name]
	return profile, ok
}

func (g *compactKbuildSelectionGraph) owner(target string) (compactKbuildSelectionKey, bool) {
	if g == nil {
		return compactKbuildSelectionKey{}, false
	}
	target = canonicalKbuildRulePath(target)
	owner, ok := g.owners[target]
	return owner, ok
}

func (g *compactKbuildSelectionGraph) publishedOwner(
	tree, target string,
) (compactKbuildSelectionKey, bool) {
	if g == nil {
		return compactKbuildSelectionKey{}, false
	}
	target = canonicalKbuildRulePath(target)
	owner, ok := g.publishedOwners[actionPlanLookupKey(tree, target)]
	return owner, ok
}

// compactKbuildVisibleArtifactOwner resolves the exact selected terminal action
// named by a visible-artifact provenance record. Recursive Make forwarding may
// move ownership to a selected descendant, but unrelated same-path selections
// are never candidates.
func (g *compactKbuildSelectionGraph) compactKbuildVisibleArtifactOwner(
	artifact CompactKbuildVisibleArtifact,
) (compactKbuildSelectionKey, error) {
	if g == nil {
		return compactKbuildSelectionKey{}, fmt.Errorf("resolve visible artifact %#v in a nil selection graph", artifact)
	}
	if _, ok := g.profiles[artifact.Profile]; !ok {
		return compactKbuildSelectionKey{}, fmt.Errorf(
			"visible artifact %q references missing provenance profile %q",
			artifact.Path, artifact.Profile,
		)
	}
	target := compactKbuildGraphTargetPath(artifact.Target)
	if target == "" || target != artifact.Target {
		return compactKbuildSelectionKey{}, fmt.Errorf(
			"visible artifact %q has invalid provenance target %q in profile %q",
			artifact.Path, artifact.Target, artifact.Profile,
		)
	}
	if exact, ok := g.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: artifact.Profile, target: target,
	}]; ok && !g.forwardingSelections[exact] {
		// The frontier names one exact materialized version. Only a pure
		// recursive forwarding origin is resolved through its descendants.
		return exact, nil
	}
	candidates := []compactKbuildSelectionKey{}
	for _, candidate := range g.selectionsByTarget[target] {
		if candidate.profile != artifact.Profile && !g.compactKbuildInvocationDescendsTo(artifact.Profile, target, candidate.profile) {
			continue
		}
		if g.forwardingSelections[candidate] {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) != 1 {
		labels := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			labels = append(labels, compactKbuildSelectionKeyString(candidate))
		}
		sort.Strings(labels)
		return compactKbuildSelectionKey{}, fmt.Errorf(
			"visible artifact %q from %s:%s resolves to %d terminal selected owners %q",
			artifact.Path, artifact.Profile, artifact.Target, len(candidates), labels,
		)
	}
	return candidates[0], nil
}

// prepareGroupedSelections binds every selected peer of one grouped Make rule
// to a shared action identity. GNU Make executes a grouped rule once even when
// several of its outputs are requested; treating each selected target as an
// independent action would give those logical peers different file contents.
func (g *compactKbuildSelectionGraph) prepareGroupedSelections(
	metadata *CompactMetadata,
	config CompactConfig,
) error {
	if g == nil || metadata == nil {
		return fmt.Errorf("cannot prepare grouped Kbuild selections without a graph and metadata")
	}
	g.invalidateResolvedDependencies()
	g.groupedSelectionIDs = make(map[compactKbuildSelectionKey]compactKbuildGroupedSelectionID)
	g.groupedSelectionMembers = make(map[compactKbuildGroupedSelectionID][]compactKbuildSelectionKey)
	g.groupedSelectionRepresentatives = make(map[compactKbuildGroupedSelectionID]compactKbuildSelectionKey)

	type candidate struct {
		key             compactKbuildSelectionKey
		outputSignature string
		outputCount     int
	}
	candidates := map[compactKbuildGroupedSelectionID][]candidate{}
	groupOrder := []compactKbuildGroupedSelectionID{}
	groupedEvidence := map[compactKbuildSelectionKey]bool{}
	for _, key := range g.ordered {
		if g.forwardingSelections[key] {
			continue
		}
		selection := g.selections[key]
		if selection.SourceScriptPhase != "" {
			continue
		}
		profile, ok := g.profile(key.profile)
		if !ok {
			return fmt.Errorf("grouped Kbuild selection references missing profile %q", key.profile)
		}
		if g.compactKbuildProfileTargetIsPhony(profile, key.target) {
			continue
		}
		match, matched, err := g.compactKbuildRuleForProfileMakeTarget(
			metadata, profile, key.target, selection.MakeTarget,
		)
		if err != nil {
			return fmt.Errorf("resolve grouped Kbuild selection %s: %w", compactKbuildSelectionKeyString(key), err)
		}
		if !matched || !compactKbuildRuleHasGroupedOutputs(match.rule) {
			continue
		}
		outputs := compactKbuildRecipeRuleOutputs(key.target, match)
		if len(outputs) < 2 {
			continue
		}
		sort.Strings(outputs)
		id := compactKbuildGroupedSelectionID{
			profile: key.profile, ruleOrder: match.ruleOrder, stem: match.stem,
		}
		if len(candidates[id]) == 0 {
			groupOrder = append(groupOrder, id)
		}
		candidates[id] = append(candidates[id], candidate{
			key: key, outputSignature: strings.Join(outputs, "\x00"), outputCount: len(outputs),
		})
		groupedEvidence[key] = true
		if selection.GroupedTrigger == "" {
			return fmt.Errorf(
				"grouped Kbuild selection %s has no source-order trigger authority",
				compactKbuildSelectionKeyString(key),
			)
		}
	}
	for key, selection := range g.selections {
		if selection.GroupedTrigger != "" && !groupedEvidence[key] {
			return fmt.Errorf(
				"Kbuild selection %s declares grouped trigger %q but does not resolve to a grouped recipe",
				compactKbuildSelectionKeyString(key), selection.GroupedTrigger,
			)
		}
	}

	for _, id := range groupOrder {
		group := candidates[id]
		if len(group) != group[0].outputCount {
			return fmt.Errorf(
				"grouped Kbuild selection %s records trigger %q but only %d of %d recipe outputs were selected",
				compactKbuildSelectionKeyString(group[0].key), g.selections[group[0].key].GroupedTrigger,
				len(group), group[0].outputCount,
			)
		}
		trigger := group[0].key
		declaredTrigger := ""
		for _, peer := range group {
			selection := g.selections[peer.key]
			if declaredTrigger != "" && declaredTrigger != selection.GroupedTrigger {
				return fmt.Errorf(
					"grouped Kbuild rule in profile %q has conflicting triggers %q and %q",
					id.profile, declaredTrigger, selection.GroupedTrigger,
				)
			}
			declaredTrigger = selection.GroupedTrigger
		}
		if declaredTrigger != "" {
			found := false
			for _, peer := range group {
				if peer.key.target == declaredTrigger {
					trigger = peer.key
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf(
					"grouped Kbuild rule in profile %q trigger %q is not a selected peer",
					id.profile, declaredTrigger,
				)
			}
		}
		baseline := g.selections[trigger]
		baselineContext := compactKbuildSelectionPlanContext(config, baseline)
		baselineComparable := baseline
		baselineComparable.Target = ""
		baselineComparable.MakeTarget = ""
		baselineComparable.GroupedTrigger = ""
		// Peers can declare different native prerequisites. Their exact producer
		// versions are joined by selection dependencies, not recipe context.
		baselineComparable.NativePrerequisiteArtifacts = ""
		for _, peer := range group {
			selection := g.selections[peer.key]
			comparable := selection
			comparable.Target = ""
			comparable.MakeTarget = ""
			comparable.GroupedTrigger = ""
			comparable.NativePrerequisiteArtifacts = ""
			context := compactKbuildSelectionPlanContext(config, selection)
			if peer.outputSignature != group[0].outputSignature ||
				comparable != baselineComparable || context != baselineContext {
				return fmt.Errorf(
					"grouped Kbuild rule in profile %q (rule %d, stem %q) selects peers %s and %s with incompatible outputs or action contexts",
					id.profile, id.ruleOrder, id.stem,
					compactKbuildSelectionKeyString(trigger), compactKbuildSelectionKeyString(peer.key),
				)
			}
		}
		members := make([]compactKbuildSelectionKey, 0, len(group))
		members = append(members, trigger)
		for _, peer := range group {
			if peer.key == trigger {
				continue
			}
			members = append(members, peer.key)
		}
		for _, member := range members {
			g.groupedSelectionIDs[member] = id
		}
		g.groupedSelectionMembers[id] = members
		g.groupedSelectionRepresentatives[id] = trigger
	}
	// Primary-output overwrite edges are registered while the structural graph
	// is built, before rule instances can be resolved. Rebuild their dependency
	// projection now so every selected peer can trigger the shared action safely.
	g.compactKbuildRebuildOverwriteDependencies()
	g.nativeDependencies = make(map[compactKbuildSelectionMetadataKey]compactKbuildSelectionDependencies)
	g.invocationPredecessors = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.invocationRecipeSelections = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.parentPrerequisiteSelections = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.terminalSelections = make(map[compactKbuildTerminalSelectionKey]compactKbuildSelectionDependencies)
	return nil
}

func (g *compactKbuildSelectionGraph) compactKbuildGroupedSelectionMembers(
	key compactKbuildSelectionKey,
) []compactKbuildSelectionKey {
	if g != nil {
		if id, ok := g.groupedSelectionIDs[key]; ok {
			return g.groupedSelectionMembers[id]
		}
	}
	return []compactKbuildSelectionKey{key}
}

func (g *compactKbuildSelectionGraph) compactKbuildGroupedSelectionRepresentative(
	key compactKbuildSelectionKey,
) compactKbuildSelectionKey {
	if g != nil {
		if id, ok := g.groupedSelectionIDs[key]; ok {
			if representative, exists := g.groupedSelectionRepresentatives[id]; exists {
				return representative
			}
		}
	}
	return key
}

func (g *compactKbuildSelectionGraph) compactKbuildSelectionsShareProducer(
	left, right compactKbuildSelectionKey,
) bool {
	if left == right {
		return true
	}
	if g == nil {
		return false
	}
	leftID, leftGrouped := g.groupedSelectionIDs[left]
	rightID, rightGrouped := g.groupedSelectionIDs[right]
	return leftGrouped && rightGrouped && leftID == rightID
}

func (g *compactKbuildSelectionGraph) recordMaterializedProducer(
	key compactKbuildSelectionKey,
	producer string,
) error {
	if g == nil || producer == "" {
		return fmt.Errorf("record Kbuild selection %s with empty materialized producer", compactKbuildSelectionKeyString(key))
	}
	members := g.compactKbuildGroupedSelectionMembers(key)
	for _, member := range members {
		if _, ok := g.selections[member]; !ok {
			return fmt.Errorf("record materialized producer for missing Kbuild selection %s", compactKbuildSelectionKeyString(member))
		}
		if g.forwardingSelections[member] {
			return fmt.Errorf("record materialized producer for forwarding Kbuild selection %s", compactKbuildSelectionKeyString(member))
		}
		if existing, ok := g.materializedProducers[member]; ok && existing != producer {
			return fmt.Errorf("Kbuild selection %s has conflicting materialized producers %q and %q", compactKbuildSelectionKeyString(member), existing, producer)
		}
	}
	changed := false
	for _, member := range members {
		if existing, ok := g.materializedProducers[member]; !ok || existing != producer {
			changed = true
		}
		g.materializedProducers[member] = producer
	}
	if changed {
		g.materializedProducerGeneration++
	}
	return nil
}

func (g *compactKbuildSelectionGraph) orderedSelections() []CompactKbuildSelection {
	if g == nil {
		return nil
	}
	selections := make([]CompactKbuildSelection, 0, len(g.ordered))
	for _, key := range g.ordered {
		if g.forwardingSelections[key] || g.shadowedPrimarySelections[key] {
			continue
		}
		selections = append(selections, g.selections[key])
	}
	return selections
}

// releasePlanningCaches drops closure-local analysis once ordering and side
// outputs have been derived. Preserve only successful resolutions for exact
// selected roots: final lowering immediately repeats those same expensive
// queries, while every other cached traversal can be recomputed on demand.
// Reinitialize rather than nil the maps so a caller which asks the returned
// graph to derive an order again gets an exact recomputation instead of a
// write-to-nil-map panic.
func (g *compactKbuildSelectionGraph) releasePlanningCaches() {
	if g == nil {
		return
	}
	selectedRootRuleResolutions := make(
		map[compactKbuildSelectedRuleResolutionKey]compactKbuildRuleResolution,
		len(g.selections),
	)
	for resolutionKey, resolution := range g.ruleResolutions {
		if resolution.err != nil || !resolution.matched {
			continue
		}
		selectionKey, selected := g.selectionsByProfileTarget[resolutionKey.compactKbuildProfileTargetKey]
		if !selected || g.forwardingSelections[selectionKey] ||
			g.compactKbuildGroupedSelectionRepresentative(selectionKey) != selectionKey {
			continue
		}
		selection, selected := g.selections[selectionKey]
		profile, profiled := g.profile(selectionKey.profile)
		if !selected || !profiled || g.compactKbuildProfileTargetIsPhony(profile, selectionKey.target) {
			continue
		}
		makeTarget := compactKbuildRuleLookupTarget(
			profile,
			selectionKey.target,
			selection.MakeTarget,
		)
		if resolutionKey.makeTarget != makeTarget {
			continue
		}
		selectedRootRuleResolutions[compactKbuildSelectedRuleResolutionKey{
			metadata:   resolutionKey.metadata,
			selection:  selectionKey,
			makeTarget: makeTarget,
		}] = resolution
	}
	g.selectedRootRuleResolutions = selectedRootRuleResolutions
	g.ruleResolutions = make(map[compactKbuildRuleResolutionKey]compactKbuildRuleResolution)
	g.targetRuleContexts = make(map[compactKbuildTargetRuleContextKey]compactKbuildTargetRuleContext)
	g.sourceEvidence = make(map[compactKbuildRuleResolutionKey]bool)
	g.nativeDependencies = make(map[compactKbuildSelectionMetadataKey]compactKbuildSelectionDependencies)
	// Keep the much smaller final dependency projection. Materialization order
	// populated it for every selected producer, and action lowering repeatedly
	// asks for those exact immutable graph/metadata pairs.
	g.invocationPredecessors = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.invocationRecipeSelections = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.parentPrerequisiteSelections = make(map[compactKbuildInvocationPredecessorKey]compactKbuildSelectionDependencies)
	g.terminalSelections = make(map[compactKbuildTerminalSelectionKey]compactKbuildSelectionDependencies)
	g.unruledPrerequisites = make(map[compactKbuildSelectionMetadataKey]compactKbuildPrerequisitePaths)
}

func (g *compactKbuildSelectionGraph) selectedRootRuleResolutionKey(
	metadata *CompactMetadata,
	selectionKey compactKbuildSelectionKey,
	profile CompactKbuildProfile,
	target, makeTarget string,
) (compactKbuildSelectedRuleResolutionKey, bool) {
	if g == nil || metadata == nil {
		return compactKbuildSelectedRuleResolutionKey{}, false
	}
	target = canonicalKbuildRulePath(target)
	if selectionKey.profile != profile.Name || selectionKey.target != target {
		return compactKbuildSelectedRuleResolutionKey{}, false
	}
	selection, selected := g.selections[selectionKey]
	graphProfile, profiled := g.profile(selectionKey.profile)
	if !selected || !profiled {
		return compactKbuildSelectedRuleResolutionKey{}, false
	}
	expectedMakeTarget := compactKbuildRuleLookupTarget(
		graphProfile,
		target,
		selection.MakeTarget,
	)
	makeTarget = compactKbuildRuleLookupTarget(graphProfile, target, makeTarget)
	if makeTarget != expectedMakeTarget {
		return compactKbuildSelectedRuleResolutionKey{}, false
	}
	return compactKbuildSelectedRuleResolutionKey{
		metadata:   metadata,
		selection:  selectionKey,
		makeTarget: makeTarget,
	}, true
}

// hasSelectedRootRuleResolution checks the exact successful root match retained
// across releasePlanningCaches without consuming the one-shot lowering entry.
func (g *compactKbuildSelectionGraph) hasSelectedRootRuleResolution(
	metadata *CompactMetadata,
	selectionKey compactKbuildSelectionKey,
	profile CompactKbuildProfile,
	target, makeTarget string,
) bool {
	key, valid := g.selectedRootRuleResolutionKey(metadata, selectionKey, profile, target, makeTarget)
	if !valid {
		return false
	}
	_, cached := g.selectedRootRuleResolutions[key]
	return cached
}

// takeSelectedRootRuleResolution returns the successful root match retained
// across releasePlanningCaches. The cache is exact to one metadata instance,
// graph selection (including stage), and lexical Make target. A hit is deleted
// before lowering so the retained profile/rule graph cannot accumulate in the
// completed action plan.
func (g *compactKbuildSelectionGraph) takeSelectedRootRuleResolution(
	metadata *CompactMetadata,
	selectionKey compactKbuildSelectionKey,
	profile CompactKbuildProfile,
	target, makeTarget string,
) (compactKbuildRuleResolution, bool) {
	key, valid := g.selectedRootRuleResolutionKey(metadata, selectionKey, profile, target, makeTarget)
	if !valid {
		return compactKbuildRuleResolution{}, false
	}
	resolution, cached := g.selectedRootRuleResolutions[key]
	if !cached {
		return compactKbuildRuleResolution{}, false
	}
	delete(g.selectedRootRuleResolutions, key)
	return resolution, true
}

func (g *compactKbuildSelectionGraph) compactKbuildRuleForProfile(
	metadata *CompactMetadata,
	profile CompactKbuildProfile,
	target string,
) (compactKbuildRuleMatch, bool, error) {
	return g.compactKbuildRuleForProfileMakeTarget(metadata, profile, target, target)
}

func (g *compactKbuildSelectionGraph) compactKbuildRuleForProfileMakeTarget(
	metadata *CompactMetadata,
	profile CompactKbuildProfile,
	target, makeTarget string,
) (compactKbuildRuleMatch, bool, error) {
	// A rule target may be an invocation-local control goal such as `.`.
	// File-path normalization would erase that goal before rule resolution.
	target = compactKbuildGraphTargetPath(target)
	makeTarget = compactKbuildRuleLookupTarget(profile, target, makeTarget)
	key := compactKbuildRuleResolutionKey{
		metadata: metadata,
		compactKbuildProfileTargetKey: compactKbuildProfileTargetKey{
			profile: profile.Name,
			target:  target,
		},
		makeTarget: makeTarget,
	}
	if cached, ok := g.ruleResolutions[key]; ok {
		return cached.match, cached.matched, cached.err
	}
	g.cacheMisses.ruleResolution++
	// The planner runtime owns the canonical exact/pattern rule index. Do not
	// mirror its pattern list here: a linear preflight scan for every queried
	// target turns selection ordering back into O(targets * patterns), even
	// when the rule resolver can answer the lookup from that index. Calling the
	// resolver directly also avoids performing the indexed candidate query
	// twice for targets which do match.
	g.cacheMisses.ruleEvaluation++
	match, matched, err := metadata.compactKbuildRuleForProfileMakeTarget(profile, target, makeTarget)
	g.ruleResolutions[key] = compactKbuildRuleResolution{match: match, matched: matched, err: err}
	return match, matched, err
}

func (g *compactKbuildSelectionGraph) compactKbuildTargetRuleContext(
	profile CompactKbuildProfile,
	target string,
	match compactKbuildRuleMatch,
) ([]compactKbuildEvaluatedPath, []compactKbuildEvaluatedPath, string, error) {
	target = compactKbuildGraphTargetPath(target)
	makeTarget := compactKbuildRuleLookupTarget(profile, target, match.lookupTarget)
	key := compactKbuildTargetRuleContextKey{
		compactKbuildProfileTargetKey: compactKbuildProfileTargetKey{profile: profile.Name, target: target},
		makeTarget:                    makeTarget,
		ruleOrder:                     match.ruleOrder,
		targetOrder:                   match.targetOrder,
		stem:                          match.stem,
	}
	if cached, ok := g.targetRuleContexts[key]; ok {
		return cached.normal, cached.orderOnly, cached.stem, cached.err
	}
	g.cacheMisses.targetRuleContext++
	context, err := evaluatedKbuildSelectedTargetMakeContext(profile, target, &match)
	normal := compactKbuildUniqueEvaluatedPrerequisites(context.normal)
	orderOnly := compactKbuildUniqueEvaluatedPrerequisites(context.orderOnly)
	g.targetRuleContexts[key] = compactKbuildTargetRuleContext{
		normal: normal, orderOnly: orderOnly, stem: context.stem, err: err,
	}
	return normal, orderOnly, context.stem, err
}

func compactKbuildUniqueEvaluatedPrerequisites(values []compactKbuildEvaluatedPath) []compactKbuildEvaluatedPath {
	out := make([]compactKbuildEvaluatedPath, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value.graphPath = compactKbuildGraphTargetPath(value.graphPath)
		if value.graphPath != "" && !seen[value.graphPath] {
			seen[value.graphPath] = true
			out = append(out, value)
		}
	}
	return out
}

func (g *compactKbuildSelectionGraph) compactKbuildSourcePathExists(
	metadata *CompactMetadata,
	profile CompactKbuildProfile,
	target string,
) (bool, error) {
	target = canonicalKbuildRulePath(target)
	key := compactKbuildRuleResolutionKey{
		metadata: metadata,
		compactKbuildProfileTargetKey: compactKbuildProfileTargetKey{
			profile: profile.Name,
			target:  target,
		},
	}
	if source, ok := g.sourceEvidence[key]; ok {
		return source, nil
	}
	g.cacheMisses.sourceEvidence++
	evidence, err := metadata.compactKbuildGraphSourcePathExists(profile, target)
	if err != nil {
		return false, err
	}
	source := evidence.exists
	g.sourceEvidence[key] = source
	return source, nil
}

func (g *compactKbuildSelectionGraph) compactKbuildProfileGenerates(
	profile CompactKbuildProfile,
	target string,
) bool {
	target = canonicalKbuildRulePath(target)
	return g.generatedTargets[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
}

func (g *compactKbuildSelectionGraph) compactKbuildProfileTargetIsPhony(
	profile CompactKbuildProfile,
	target string,
) bool {
	target = compactKbuildGraphTargetPath(target)
	return g.phonyTargets[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
}

// The evaluated .PHONY prerequisite words identify Make control targets.
// Reuse the same source rule interpretation when a selected recipe evaluates
// its automatic $@ and when the selection graph indexes those controls.
func compactKbuildVisitRulePhonyTargets(profile CompactKbuildProfile, rule KbuildRule, visit func(string)) {
	for _, rawTarget := range rule.Targets {
		if canonicalKbuildRulePath(rawTarget) != ".PHONY" {
			continue
		}
		for _, prerequisite := range rule.Prerequisites {
			visit(compactKbuildProfileTargetPath(profile, prerequisite))
		}
		return
	}
}

func compactKbuildProfileTargetDeclaredPhony(profile CompactKbuildProfile, target string) bool {
	return compactKbuildPlannerRuntimeForProfile(profile).phonyTargets[compactKbuildGraphTargetPath(target)]
}

// GNU Make does not search implicit rules for a target declared .PHONY. An
// otherwise unruled PHONY prerequisite completes without publishing a file;
// an explicit rule still owns its prerequisite and recipe status semantics.
func compactKbuildPhonyHasExplicitRule(profile CompactKbuildProfile, target, makeTarget string) bool {
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget) {
		if !strings.Contains(candidate.target, "%") {
			return true
		}
	}
	return false
}

func compactKbuildPhonyHasUnselectedRecipe(profile CompactKbuildProfile, target, makeTarget string) bool {
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget) {
		if !strings.Contains(candidate.target, "%") && len(candidate.rule.Recipe) != 0 {
			return true
		}
	}
	return false
}

// A PHONY target with no selected action can still be a source-selected
// recursive Make boundary. The frozen recipe must contain only authenticated
// child invocations (and inert shell lines); the child actions own the actual
// output files and their completion status. A local shell command instead
// requires its own selected PHONY status action.
func (g *compactKbuildSelectionGraph) compactKbuildUnselectedPhonyForwardsChildren(
	metadata *CompactMetadata, profile CompactKbuildProfile, target, makeTarget string,
) (bool, error) {
	if !g.compactKbuildProfileTargetIsPhony(profile, target) ||
		!compactKbuildPhonyHasUnselectedRecipe(profile, target, makeTarget) {
		return false, nil
	}
	status, err := metadata.compactKbuildSelectedPhonySourceStatus(profile, target, makeTarget)
	if err != nil {
		return false, err
	}
	if status == nil || status.command != ":" {
		return false, nil
	}
	children := g.targetInvocations[compactKbuildProfileTargetKey{profile: profile.Name, target: target}]
	if len(children) == 0 {
		return false, fmt.Errorf("PHONY forwarding target %q in profile %q has no selected recursive child", target, profile.Name)
	}
	return true, nil
}

// materializationOrder returns the selected artifacts in dependency order.
// A builder is deliberately bound to one exact Make invocation, so it must
// never recursively lower a prerequisite that another selection owns. Build
// the owning selection first; the parent builder will then bind the existing
// producer instead of searching its own profile for an unrelated rule.
func (g *compactKbuildSelectionGraph) materializationOrder(metadata *CompactMetadata) ([]CompactKbuildSelection, error) {
	if g == nil {
		return nil, nil
	}
	if metadata == nil {
		return nil, fmt.Errorf("cannot order Kbuild selections without metadata")
	}

	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[compactKbuildSelectionKey]int, len(g.selections))
	stack := make([]compactKbuildSelectionKey, 0, len(g.selections))
	ordered := make([]CompactKbuildSelection, 0, len(g.selections))
	var visit func(compactKbuildSelectionKey) error
	visit = func(key compactKbuildSelectionKey) error {
		key = g.compactKbuildGroupedSelectionRepresentative(key)
		switch state[key] {
		case visited:
			return nil
		case visiting:
			cycle := []string{}
			start := 0
			for index, candidate := range stack {
				if candidate == key {
					start = index
					break
				}
			}
			for _, candidate := range append(stack[start:], key) {
				cycle = append(cycle, compactKbuildSelectionKeyString(candidate))
			}
			return fmt.Errorf("selected Kbuild artifact dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		state[key] = visiting
		stack = append(stack, key)
		dependencies, err := g.selectionDependencies(metadata, key)
		if err != nil {
			return err
		}
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = visited
		ordered = append(ordered, g.selections[key])
		return nil
	}
	for _, key := range g.ordered {
		if err := visit(key); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func (g *compactKbuildSelectionGraph) selectionDependencies(
	metadata *CompactMetadata,
	key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	representative := g.compactKbuildGroupedSelectionRepresentative(key)
	cacheKey := compactKbuildSelectionMetadataKey{metadata: metadata, selection: representative}
	if cached, ok := g.resolvedDependencies[cacheKey]; ok {
		return append([]compactKbuildSelectionKey(nil), cached...), nil
	}
	dependencies := []compactKbuildSelectionKey{}
	selected := map[compactKbuildSelectionKey]bool{}
	for _, member := range g.compactKbuildGroupedSelectionMembers(representative) {
		memberDependencies, err := g.selectionDependenciesSingle(metadata, member)
		if err != nil {
			return nil, err
		}
		for _, dependency := range memberDependencies {
			dependency = g.compactKbuildGroupedSelectionRepresentative(dependency)
			if g.compactKbuildSelectionsShareProducer(representative, dependency) || selected[dependency] {
				continue
			}
			selected[dependency] = true
			dependencies = append(dependencies, dependency)
		}
	}
	if g.resolvedDependencies == nil {
		g.resolvedDependencies = make(map[compactKbuildSelectionMetadataKey][]compactKbuildSelectionKey)
	}
	g.resolvedDependencies[cacheKey] = append([]compactKbuildSelectionKey(nil), dependencies...)
	return dependencies, nil
}

// invalidateResolvedDependencies marks a construction-phase graph mutation.
// Metadata identity is part of each key, while graph identity is provided by
// owning the memo on the graph itself.
func (g *compactKbuildSelectionGraph) invalidateResolvedDependencies() {
	if g != nil {
		g.resolvedDependencies = make(map[compactKbuildSelectionMetadataKey][]compactKbuildSelectionKey)
	}
}

// selectionDependenciesSingle resolves one logical target's dependency
// declaration. selectionDependencies unions these declarations for grouped
// targets because GNU Make combines peer-specific prerequisite-only rules
// before executing the group's one shared recipe.
func (g *compactKbuildSelectionGraph) selectionDependenciesSingle(
	metadata *CompactMetadata,
	key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	if status := g.phonyStatusLines[key]; key.phonyStatusLine != 0 && status != nil {
		return g.phonyStatusDependencies(metadata, key, status)
	}
	if _, phase := g.sourcePhasesBySelection[key]; phase {
		return g.sourcePhaseSelectionDependencies(metadata, key)
	}
	dependencies, err := g.selectionNativeDependencies(metadata, key)
	if err != nil {
		return nil, err
	}
	for _, dependency := range dependencies {
		if compactKbuildSelectionStageOrder(dependency.stage) > compactKbuildSelectionStageOrder(key.stage) {
			return nil, fmt.Errorf(
				"Kbuild selection %s has native artifact dependency %s in later physical stage",
				compactKbuildSelectionKeyString(key), compactKbuildSelectionKeyString(dependency),
			)
		}
	}
	profile, ok := g.profile(key.profile)
	if !ok {
		return nil, fmt.Errorf("Kbuild selection references missing profile %q", key.profile)
	}
	selected := map[compactKbuildSelectionKey]bool{}
	for _, dependency := range dependencies {
		selected[dependency] = true
	}
	parentPrerequisites, parentErr := g.compactKbuildParentPrerequisiteSelections(metadata, profile.Name, key.stage)
	if parentErr != nil {
		return nil, fmt.Errorf("resolve parent prerequisite frontier for %s: %w", compactKbuildSelectionKeyString(key), parentErr)
	}
	for _, parent := range parentPrerequisites {
		if parent != key && !selected[parent] {
			selected[parent] = true
			dependencies = append(dependencies, parent)
		}
	}
	if precedingPhase, phasedChild := g.sourcePhaseBeforeChild[profile.Name]; phasedChild {
		if compactKbuildSelectionStageOrder(precedingPhase.stage) > compactKbuildSelectionStageOrder(key.stage) {
			return nil, fmt.Errorf("recursive Make child %s has later-stage source phase %s", compactKbuildSelectionKeyString(key), compactKbuildSelectionKeyString(precedingPhase))
		}
		if precedingPhase != key && !selected[precedingPhase] {
			selected[precedingPhase] = true
			dependencies = append(dependencies, precedingPhase)
		}
	}
	for _, candidate := range g.sideOutputCandidateDependencies[key] {
		if candidate == key || selected[candidate] {
			continue
		}
		if compactKbuildSelectionStageOrder(candidate.stage) > compactKbuildSelectionStageOrder(key.stage) {
			return nil, fmt.Errorf(
				"Kbuild selection %s observes side-output candidate %s in later physical stage",
				compactKbuildSelectionKeyString(key), compactKbuildSelectionKeyString(candidate),
			)
		}
		selected[candidate] = true
		dependencies = append(dependencies, candidate)
	}
	if len(profile.InvocationPredecessors) != 0 {
		predecessors, err := g.compactKbuildInvocationPredecessorSelections(metadata, profile.Name, key.stage)
		if err != nil {
			return nil, fmt.Errorf("resolve invocation predecessors for %s: %w", compactKbuildSelectionKeyString(key), err)
		}
		for _, terminal := range predecessors {
			if terminal == key || selected[terminal] {
				continue
			}
			selected[terminal] = true
			dependencies = append(dependencies, terminal)
		}
	}
	for _, status := range g.phonyStatusBeforeChild[profile.Name] {
		if !selected[status] {
			selected[status] = true
			dependencies = append(dependencies, status)
		}
	}
	if status, exists := g.phonyStatusByOwner[key]; exists && !selected[status] {
		dependencies = append(dependencies, status)
	}
	invocationKey := compactKbuildProfileTargetKey{profile: profile.Name, target: key.target}
	finalChild := ""
	if phases := g.sourcePhasesByOwner[key]; len(phases) != 0 {
		finalChild = g.sourcePhaseChildren[phases[len(phases)-1]]
	}
	for _, dependencyProfile := range g.targetInvocations[invocationKey] {
		if finalChild != "" && dependencyProfile != finalChild {
			continue
		}
		if g.compactKbuildChildOverwritesParentVersion(key.profile, key.target, dependencyProfile) {
			// The child invocation starts from this action's same-path output.
			// It is a source-ordered successor overwrite, not a dependency of
			// the action which produced that initial version.
			continue
		}
		terminals, err := g.compactKbuildTerminalRecipeSelections(metadata, dependencyProfile, key.stage)
		if err != nil {
			return nil, fmt.Errorf("resolve recursive invocation dependency %q for %s: %w", dependencyProfile, compactKbuildSelectionKeyString(key), err)
		}
		for _, terminal := range terminals {
			if terminal != key && !selected[terminal] {
				selected[terminal] = true
				dependencies = append(dependencies, terminal)
			}
		}
	}
	return dependencies, nil
}

// Source phases execute inside the real owner's already selected rule. The
// first phase starts after that rule's native prerequisites, while the second
// starts after the first recursive child completes. Giving either phase the
// complete owner's dependencies would make the first child depend on itself.
func (g *compactKbuildSelectionGraph) sourcePhaseSelectionDependencies(
	metadata *CompactMetadata, key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	phase := g.sourcePhasesBySelection[key]
	owner, selected := g.sourcePhaseOwners[key]
	if !selected {
		return nil, fmt.Errorf("selected source phase %s has no real Make owner", compactKbuildSelectionKeyString(key))
	}
	if phase.Ordinal == 0 {
		dependencies, err := g.selectionNativeDependencies(metadata, owner)
		if err != nil {
			return nil, fmt.Errorf("selected source phase %s owner prerequisites: %w", compactKbuildSelectionKeyString(key), err)
		}
		parents, err := g.compactKbuildParentPrerequisiteSelections(metadata, owner.profile, key.stage)
		if err != nil {
			return nil, fmt.Errorf("selected source phase %s parent prerequisite frontier: %w", compactKbuildSelectionKeyString(key), err)
		}
		predecessors, err := g.compactKbuildInvocationPredecessorSelections(metadata, owner.profile, owner.stage)
		if err != nil {
			return nil, fmt.Errorf("selected source phase %s invocation predecessors: %w", compactKbuildSelectionKeyString(key), err)
		}
		selected := map[compactKbuildSelectionKey]bool{}
		ordered := make([]compactKbuildSelectionKey, 0, len(dependencies)+len(parents)+len(predecessors))
		for _, dependency := range append(append(dependencies, parents...), predecessors...) {
			if compactKbuildSelectionStageOrder(dependency.stage) > compactKbuildSelectionStageOrder(key.stage) {
				return nil, fmt.Errorf("selected source phase %s depends on later-stage prerequisite %s", compactKbuildSelectionKeyString(key), compactKbuildSelectionKeyString(dependency))
			}
			if dependency != key && !selected[dependency] {
				selected[dependency] = true
				ordered = append(ordered, dependency)
			}
		}
		return ordered, nil
	}
	if phase.Ordinal != 1 {
		return nil, fmt.Errorf("selected source phase %s has unsupported ordinal %d", compactKbuildSelectionKeyString(key), phase.Ordinal)
	}
	phases := g.sourcePhasesByOwner[owner]
	if len(phases) != 2 || phases[1] != key {
		return nil, fmt.Errorf("selected source phase %s has no ordered predecessor", compactKbuildSelectionKeyString(key))
	}
	child := g.sourcePhaseChildren[phases[0]]
	terminals, err := g.compactKbuildTerminalRecipeSelections(metadata, child, key.stage)
	if err != nil {
		return nil, fmt.Errorf("selected source phase %s first child %q: %w", compactKbuildSelectionKeyString(key), child, err)
	}
	if len(terminals) == 0 {
		return nil, fmt.Errorf("selected source phase %s first child %q has no selected terminal", compactKbuildSelectionKeyString(key), child)
	}
	return append([]compactKbuildSelectionKey{phases[0]}, terminals...), nil
}

// compactKbuildInvocationPredecessorSelections resolves the source-ordered
// invocation boundary once per consumer profile and stage. Side-output
// discovery and every selected target in that invocation share this boundary;
// rescanning all predecessor profiles per target is quadratic when a captured
// invocation contains many selected outputs.
func (g *compactKbuildSelectionGraph) compactKbuildInvocationPredecessorSelections(
	metadata *CompactMetadata,
	profileName, consumerStage string,
) ([]compactKbuildSelectionKey, error) {
	cacheKey := compactKbuildInvocationPredecessorKey{
		metadata: metadata,
		profile:  profileName,
		stage:    compactKbuildSelectionStageOrder(consumerStage),
	}
	if cached, ok := g.invocationPredecessors[cacheKey]; ok {
		return cached.dependencies, cached.err
	}
	g.cacheMisses.invocationPredecessor++
	profile, ok := g.profile(profileName)
	if !ok {
		err := fmt.Errorf("missing Kbuild invocation profile %q", profileName)
		g.invocationPredecessors[cacheKey] = compactKbuildSelectionDependencies{err: err}
		return nil, err
	}
	predecessors := make([]compactKbuildSelectionKey, 0, len(profile.InvocationPredecessors))
	for _, predecessor := range profile.InvocationPredecessors {
		terminals, err := g.compactKbuildTerminalRecipeSelections(metadata, predecessor, consumerStage)
		if err != nil {
			err = fmt.Errorf("predecessor %q: %w", predecessor, err)
			g.invocationPredecessors[cacheKey] = compactKbuildSelectionDependencies{err: err}
			return nil, err
		}
		predecessors = append(predecessors, terminals...)
	}
	g.invocationPredecessors[cacheKey] = compactKbuildSelectionDependencies{dependencies: predecessors}
	return predecessors, nil
}

func (g *compactKbuildSelectionGraph) selectionNativeDependencies(
	metadata *CompactMetadata,
	key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	cacheKey := compactKbuildSelectionMetadataKey{metadata: metadata, selection: key}
	if cached, ok := g.nativeDependencies[cacheKey]; ok {
		return append([]compactKbuildSelectionKey(nil), cached.dependencies...), cached.err
	}
	g.cacheMisses.nativeDependencies++
	dependencies, err := g.computeSelectionNativeDependencies(metadata, key)
	if err == nil {
		selected := make(map[compactKbuildSelectionKey]bool, len(dependencies))
		for _, dependency := range dependencies {
			selected[dependency] = true
		}
		for _, overwrite := range g.overwriteDependencies[key] {
			if overwrite.producer != key && !selected[overwrite.producer] {
				selected[overwrite.producer] = true
				dependencies = append(dependencies, overwrite.producer)
			}
		}
	}
	g.nativeDependencies[cacheKey] = compactKbuildSelectionDependencies{
		dependencies: append([]compactKbuildSelectionKey(nil), dependencies...),
		err:          err,
	}
	return dependencies, err
}

// groupedSelectionNativeDependencies returns the native prerequisite union of
// a physical producer. A grouped recipe may gain additional prerequisites
// from ordinary rules declared against any one of its logical output peers.
func (g *compactKbuildSelectionGraph) groupedSelectionNativeDependencies(
	metadata *CompactMetadata,
	key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	representative := g.compactKbuildGroupedSelectionRepresentative(key)
	dependencies := []compactKbuildSelectionKey{}
	selected := map[compactKbuildSelectionKey]bool{}
	for _, member := range g.compactKbuildGroupedSelectionMembers(representative) {
		memberDependencies, err := g.selectionNativeDependencies(metadata, member)
		if err != nil {
			return nil, err
		}
		for _, dependency := range memberDependencies {
			dependency = g.compactKbuildGroupedSelectionRepresentative(dependency)
			if g.compactKbuildSelectionsShareProducer(representative, dependency) || selected[dependency] {
				continue
			}
			selected[dependency] = true
			dependencies = append(dependencies, dependency)
		}
	}
	return dependencies, nil
}

func (g *compactKbuildSelectionGraph) computeSelectionNativeDependencies(
	metadata *CompactMetadata,
	key compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	selection, ok := g.selection(key)
	if !ok {
		return nil, fmt.Errorf("missing indexed Kbuild selection %s", compactKbuildSelectionKeyString(key))
	}
	profile, ok := g.profile(selection.Profile)
	if !ok {
		return nil, fmt.Errorf("Kbuild selection references missing profile %q", selection.Profile)
	}

	dependencies := []compactKbuildSelectionKey{}
	selected := map[compactKbuildSelectionKey]bool{}
	if selection.UsesInitialObjectTree {
		for _, artifact := range g.selectionInitialArtifacts[key] {
			if !compactKbuildProfileHasInitialVisibleArtifact(profile, artifact) {
				return nil, fmt.Errorf(
					"Kbuild selection %s has initial object-tree artifact %#v outside its exact visible frontier",
					compactKbuildSelectionKeyString(key), artifact,
				)
			}
			owner, ownerErr := g.compactKbuildVisibleArtifactOwner(artifact)
			if ownerErr != nil {
				return nil, fmt.Errorf("Kbuild selection %s initial visible artifact: %w", compactKbuildSelectionKeyString(key), ownerErr)
			}
			if owner == key {
				return nil, fmt.Errorf(
					"Kbuild selection %s consumes itself through initial visible artifact %q",
					compactKbuildSelectionKeyString(key), artifact.Path,
				)
			}
			if !selected[owner] {
				selected[owner] = true
				dependencies = append(dependencies, owner)
			}
		}
	}
	for _, artifact := range g.selectionGeneratedArtifacts[key] {
		owner, ownerErr := g.compactKbuildVisibleArtifactOwner(artifact)
		if ownerErr != nil {
			return nil, fmt.Errorf("Kbuild selection %s generated source artifact: %w", compactKbuildSelectionKeyString(key), ownerErr)
		}
		if owner == key {
			return nil, fmt.Errorf(
				"Kbuild selection %s consumes itself through generated source artifact %q",
				compactKbuildSelectionKeyString(key), artifact.Path,
			)
		}
		if !selected[owner] {
			selected[owner] = true
			dependencies = append(dependencies, owner)
		}
	}
	queryTokens, err := compactKbuildSelectionDeferredContentQueries(selection)
	if err != nil {
		return nil, fmt.Errorf("Kbuild selection %s has invalid deferred-content uses: %w", compactKbuildSelectionKeyString(key), err)
	}
	for _, token := range queryTokens {
		querySelection, ok := g.deferredContentSelections[token]
		if !ok {
			return nil, fmt.Errorf("Kbuild selection %s references unselected deferred-content query %q", compactKbuildSelectionKeyString(key), token)
		}
		initial, generated, artifactsErr := compactKbuildDeferredContentSelectionArtifacts(querySelection)
		if artifactsErr != nil {
			return nil, fmt.Errorf("deferred-content query %q artifacts: %w", token, artifactsErr)
		}
		for _, artifact := range append(initial, generated...) {
			owner, ownerErr := g.compactKbuildVisibleArtifactOwner(artifact)
			if ownerErr != nil {
				return nil, fmt.Errorf("deferred-content query %q artifact: %w", token, ownerErr)
			}
			if owner == key {
				return nil, fmt.Errorf("Kbuild selection %s consumes itself through deferred-content query %q artifact %q", compactKbuildSelectionKeyString(key), token, artifact.Path)
			}
			if !selected[owner] {
				selected[owner] = true
				dependencies = append(dependencies, owner)
			}
		}
	}
	expanded := map[string]bool{}
	appendChildTerminals := func(target string) error {
		invocationKey := compactKbuildProfileTargetKey{profile: profile.Name, target: target}
		for _, childProfile := range g.targetInvocations[invocationKey] {
			if g.compactKbuildChildOverwritesParentVersion(profile.Name, target, childProfile) {
				continue
			}
			terminals, err := g.compactKbuildTerminalRecipeSelections(metadata, childProfile, key.stage)
			if err != nil {
				return fmt.Errorf("resolve recursive invocation dependency %q for unselected target %q of %s: %w",
					childProfile, target, compactKbuildSelectionKeyString(key), err)
			}
			for _, terminal := range terminals {
				if terminal != key && !selected[terminal] {
					selected[terminal] = true
					dependencies = append(dependencies, terminal)
				}
			}
		}
		return nil
	}
	var collect func(compactKbuildEvaluatedPath) error
	collect = func(evaluated compactKbuildEvaluatedPath) error {
		target := compactKbuildGraphTargetPath(evaluated.graphPath)
		if target == "" || target == "FORCE" || target == selection.Target {
			return nil
		}
		phony, selectedPhony := g.compactKbuildSelectedPhonyPrerequisite(key, target)
		if selectedPhony && !selected[phony] {
			selected[phony] = true
			dependencies = append(dependencies, phony)
		}
		if !selectedPhony {
			if !g.compactKbuildProfileTargetIsPhony(profile, target) {
				owner, exists, ownerErr := g.compactKbuildSelectionNativePrerequisiteOwner(key, target)
				if ownerErr != nil {
					return ownerErr
				}
				if exists {
					if owner != key && !selected[owner] {
						selected[owner] = true
						dependencies = append(dependencies, owner)
					}
					return nil
				}
				source, sourceErr := g.compactKbuildSourcePathExists(metadata, profile, target)
				if sourceErr != nil {
					return sourceErr
				}
				if source {
					return nil
				}
			}
		}
		if expanded[target] {
			return nil
		}
		expanded[target] = true
		if !selectedPhony && g.compactKbuildProfileTargetIsPhony(profile, target) {
			if compactKbuildPhonyHasUnselectedRecipe(profile, target, evaluated.makeWord) {
				forwarding, err := g.compactKbuildUnselectedPhonyForwardsChildren(
					metadata, profile, target, evaluated.makeWord,
				)
				if err != nil {
					return err
				}
				if !forwarding {
					return fmt.Errorf("Kbuild selection %s PHONY prerequisite %q has an unselected executable source rule", compactKbuildSelectionKeyString(key), target)
				}
				match, matched, err := g.compactKbuildSelectedPhonyRuleForMakeTarget(metadata, profile, target, evaluated.makeWord)
				if err != nil {
					return err
				}
				if !matched || !match.explicit {
					return fmt.Errorf("Kbuild selection %s forwarding PHONY prerequisite %q has no exact source rule", compactKbuildSelectionKeyString(key), target)
				}
				normal, orderOnly, _, err := g.compactKbuildTargetRuleContext(profile, target, match)
				if err != nil {
					return err
				}
				for _, prerequisite := range append(normal, orderOnly...) {
					if err := collect(prerequisite); err != nil {
						return err
					}
				}
				return appendChildTerminals(target)
			}
			orderingOnly, normal, orderOnly, err := g.compactKbuildOrderingOnlyRuleContextForMakeTarget(
				metadata, profile, target, evaluated.makeWord,
			)
			if err != nil {
				return err
			}
			if orderingOnly {
				for _, prerequisite := range append(normal, orderOnly...) {
					if err := collect(prerequisite); err != nil {
						return err
					}
				}
				return appendChildTerminals(target)
			}
			if compactKbuildPhonyHasExplicitRule(profile, target, evaluated.makeWord) {
				return fmt.Errorf("Kbuild selection %s PHONY prerequisite %q has an unselected executable source rule", compactKbuildSelectionKeyString(key), target)
			}
			// A .PHONY declaration without an explicit rule completes without
			// a file. GNU Make never searches implicit rules for that target.
			return nil
		}
		var match compactKbuildRuleMatch
		var matched bool
		var err error
		if selectedPhony {
			match, matched, err = g.compactKbuildSelectedPhonyRuleForMakeTarget(metadata, profile, target, evaluated.makeWord)
		} else {
			match, matched, err = g.compactKbuildRuleForProfileMakeTarget(metadata, profile, target, evaluated.makeWord)
		}
		if err != nil {
			return err
		}
		matchViable := match.explicit
		if matched && !matchViable {
			matchViable, err = metadata.compactKbuildRuleMatchViableWithDeclaredProducer(
				target, match, profile,
				func(candidate string) (bool, error) {
					owner, selected, ownerErr := g.compactKbuildSelectionNativePrerequisiteOwner(key, candidate)
					if ownerErr != nil || !selected {
						return selected, ownerErr
					}
					if owner == key {
						return false, fmt.Errorf("Kbuild selection %s implicitly depends on itself through %q", compactKbuildSelectionKeyString(key), candidate)
					}
					return true, nil
				},
			)
			if err != nil {
				return err
			}
		}
		if !matched || (!matchViable && !g.compactKbuildProfileGenerates(profile, target)) {
			// A source-like prerequisite which has no selected artifact owner and
			// no rule in this invocation is resolved by ordinary action lowering.
			// It does not impose an inter-selection scheduling edge.
			return nil
		}
		normal, orderOnly, _, err := g.compactKbuildTargetRuleContext(profile, target, match)
		if err != nil {
			return err
		}
		for _, prerequisite := range normal {
			if err := collect(prerequisite); err != nil {
				return err
			}
		}
		for _, prerequisite := range orderOnly {
			if err := collect(prerequisite); err != nil {
				return err
			}
		}
		return appendChildTerminals(target)
	}

	makeTarget := selection.MakeTarget
	var match compactKbuildRuleMatch
	var matched bool
	var matchErr error
	if g.compactKbuildProfileTargetIsPhony(profile, selection.Target) {
		match, matched, matchErr = g.compactKbuildSelectedPhonyRuleForMakeTarget(metadata, profile, selection.Target, makeTarget)
	} else {
		match, matched, matchErr = g.compactKbuildRuleForProfileMakeTarget(metadata, profile, selection.Target, makeTarget)
	}
	if matchErr != nil {
		return nil, matchErr
	}
	if g.compactKbuildProfileTargetIsPhony(profile, selection.Target) {
		var normal, orderOnly []compactKbuildEvaluatedPath
		if matched {
			normal, orderOnly, _, matchErr = g.compactKbuildTargetRuleContext(profile, selection.Target, match)
		} else {
			var orderingOnly bool
			orderingOnly, normal, orderOnly, matchErr = g.compactKbuildOrderingOnlyRuleContextForMakeTarget(
				metadata, profile, selection.Target, makeTarget,
			)
			if !orderingOnly {
				return dependencies, nil
			}
		}
		if matchErr != nil {
			return dependencies, nil
		}
		for _, prerequisite := range append(normal, orderOnly...) {
			if err := collect(prerequisite); err != nil {
				return nil, err
			}
		}
		return dependencies, nil
	}
	if !matched {
		return dependencies, nil
	}
	normal, orderOnly, _, contextErr := g.compactKbuildTargetRuleContext(profile, selection.Target, match)
	if contextErr != nil {
		// Generated host/user programs and phony control nodes do not necessarily
		// have a rule in this exact invocation.
		return dependencies, nil
	}
	for _, prerequisite := range normal {
		if err := collect(prerequisite); err != nil {
			return nil, err
		}
	}
	for _, prerequisite := range orderOnly {
		if err := collect(prerequisite); err != nil {
			return nil, err
		}
	}
	return dependencies, nil
}

// preparePhonyStatusLines splits a local check from the completion of its Make
// target when a recursive child follows it. Both retain the real source owner;
// only the private completion is new, never a visible object-tree pathname.
func (g *compactKbuildSelectionGraph) preparePhonyStatusLines(metadata *CompactMetadata) error {
	g.phonyStatusLines = map[compactKbuildSelectionKey]*compactKbuildSelectedPhonyStatus{}
	g.phonyStatusByOwner = map[compactKbuildSelectionKey]compactKbuildSelectionKey{}
	g.phonyStatusBeforeChild = map[string][]compactKbuildSelectionKey{}
	for _, owner := range slices.Clone(g.ordered) {
		profile := g.profiles[owner.profile]
		if !g.compactKbuildProfileTargetIsPhony(profile, owner.target) ||
			!slices.ContainsFunc(profile.TargetInvocationDependencies, func(dependency CompactKbuildInvocationDependency) bool {
				return !dependency.Prerequisite && compactKbuildGraphTargetPath(dependency.Target) == owner.target
			}) {
			continue
		}
		selection := g.selections[owner]
		status, err := metadata.compactKbuildSelectedPhonySourceStatus(profile, owner.target, selection.MakeTarget)
		if err != nil {
			return err
		}
		if status == nil || status.recipeIndex < 0 {
			continue
		}
		if len(status.followingChildren) == 0 {
			g.phonyStatusLines[owner] = status
			continue
		}
		key := owner
		key.phonyStatusLine = status.recipeIndex + 1
		selection.phonyStatusLine = key.phonyStatusLine
		// The owner includes references from later recipe lines. Resolve their
		// pathnames against this line's frozen frontier, so a following child
		// cannot become either a dependency or an ambient input of the check.
		generatedByPath := map[string]CompactKbuildVisibleArtifact{}
		for _, artifact := range g.selectionGeneratedArtifacts[owner] {
			visible, found, err := status.snapshot.visibleProducer(artifact.Path)
			if err != nil {
				return fmt.Errorf("PHONY %s recipe %d generated input %q: %w", compactKbuildSelectionKeyString(owner), status.recipeIndex, artifact.Path, err)
			}
			if found {
				if visible.Path != artifact.Path {
					return fmt.Errorf("PHONY recipe frontier changes generated pathname %q to %q", artifact.Path, visible.Path)
				}
				generatedByPath[artifact.Path] = visible
			}
		}
		for _, read := range status.snapshot.Reads() {
			if read.Exists && read.Artifact.Producer != (CompactKbuildVisibleArtifact{}) {
				artifact := read.Artifact.Producer
				if previous, exists := generatedByPath[artifact.Path]; exists && previous != artifact {
					return fmt.Errorf("PHONY recipe read %q conflicts with its frozen generated owner", artifact.Path)
				}
				generatedByPath[artifact.Path] = artifact
			}
		}
		generated := make([]CompactKbuildVisibleArtifact, 0, len(generatedByPath))
		for _, artifact := range generatedByPath {
			generated = append(generated, artifact)
		}
		sort.Slice(generated, func(i, j int) bool { return generated[i].Path < generated[j].Path })
		selection.GeneratedObjectTreeArtifacts = EncodeCompactKbuildInitialObjectTreeArtifacts(generated)
		queries, err := compactKbuildSelectionDeferredContentQueries(selection)
		if err != nil {
			return err
		}
		lineQueries := kbuildDeferredContentTokenPattern.FindAllString(status.command, -1)
		for _, token := range queries {
			if _, visible := status.snapshot.Evaluation.Profile.deferredContentQueries[token]; visible {
				lineQueries = append(lineQueries, token)
			}
		}
		selection.DeferredContentQueries = EncodeCompactKbuildDeferredContentQueries(lineQueries)
		g.selections[key] = selection
		g.selectionInitialArtifacts[key] = g.selectionInitialArtifacts[owner]
		g.selectionNativePrerequisites[key] = g.selectionNativePrerequisites[owner]
		g.selectionGeneratedArtifacts[key] = generated
		g.phonyStatusLines[key] = status
		g.phonyStatusByOwner[owner] = key
		g.ordered = append(g.ordered, key)
		for _, child := range status.followingChildren {
			if slices.Contains(status.precedingChildren, child) {
				return fmt.Errorf("PHONY %s reuses recursive child %q across a local status", compactKbuildSelectionKeyString(owner), child)
			}
			g.phonyStatusBeforeChild[child] = append(g.phonyStatusBeforeChild[child], key)
		}
	}
	g.invalidateResolvedDependencies()
	return nil
}

func (g *compactKbuildSelectionGraph) phonyStatusDependencies(
	metadata *CompactMetadata, key compactKbuildSelectionKey, status *compactKbuildSelectedPhonyStatus,
) ([]compactKbuildSelectionKey, error) {
	owner := key
	owner.phonyStatusLine = 0
	dependencies, err := g.selectionNativeDependencies(metadata, key)
	if err != nil {
		return nil, err
	}
	parents, err := g.compactKbuildParentPrerequisiteSelections(metadata, owner.profile, owner.stage)
	if err != nil {
		return nil, err
	}
	predecessors, err := g.compactKbuildInvocationPredecessorSelections(metadata, owner.profile, owner.stage)
	if err != nil {
		return nil, err
	}
	dependencies = append(dependencies, parents...)
	dependencies = append(dependencies, predecessors...)
	dependencies = append(dependencies, g.phonyStatusBeforeChild[owner.profile]...)
	for _, child := range status.precedingChildren {
		terminals, err := g.compactKbuildTerminalRecipeSelections(metadata, child, owner.stage)
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, terminals...)
	}
	seen := map[compactKbuildSelectionKey]bool{}
	result := make([]compactKbuildSelectionKey, 0, len(dependencies))
	for _, dependency := range dependencies {
		if dependency == key || compactKbuildSelectionStageOrder(dependency.stage) > compactKbuildSelectionStageOrder(key.stage) {
			return nil, fmt.Errorf("selected PHONY status %s has invalid prerequisite %s", compactKbuildSelectionKeyString(key), compactKbuildSelectionKeyString(dependency))
		}
		if !seen[dependency] {
			seen[dependency] = true
			result = append(result, dependency)
		}
	}
	return result, nil
}
