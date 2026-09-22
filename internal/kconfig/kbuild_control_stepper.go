package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
)

// KbuildControlReadArtifact identifies the owner and version of one exact
// Make-visible file. Identity names a declared source or selected producer;
// Version names that owner's immutable contents. Both must be stable across
// invocations which see the same bytes from the same owner. Producer is the
// comparable selected action owner returned by the same frozen resolver as
// Identity and Version. Declared source and Kconfig baseline files leave it
// zero because no selected Kbuild action wrote them.
type KbuildControlReadArtifact struct {
	Tree     CompactKbuildInvocationTree
	Identity string
	Version  string
	Producer CompactKbuildVisibleArtifact
}

// KbuildControlRecipeFrontier is frozen immediately before one selected recipe
// line runs. ResolveArtifact performs on-demand owner resolution for an exact
// virtual read; it must capture the same immutable state as Files. No visible
// file map is copied for an invocation with thousands of source actions.
// ResolvePresenceArtifact identifies a known opaque selected writer only for
// literal object-tree wildcard membership, without claiming its file bytes.
type KbuildControlRecipeFrontier struct {
	ID                      string
	Files                   KbuildVirtualFileView
	ResolveArtifact         func(logicalPath string) (KbuildControlReadArtifact, bool, error)
	ResolvePresenceArtifact func(logicalPath string) (KbuildControlReadArtifact, bool, error)
}

// KbuildControlRecipeRead is an actual Make-visible file or wildcard lookup.
// For an absent object path/pattern, FrontierID fixes the state in which
// absence was observed. Exact reads depend on content Artifact. Literal
// wildcard membership of an opaque selected output depends on its declared
// producer and frozen frontier version; nonempty wildcard queries also record
// a MembershipVersion of their complete sorted member set.
type KbuildControlRecipeRead struct {
	Path              string
	Artifact          KbuildControlReadArtifact
	Exists            bool
	FrontierID        string
	Wildcard          bool
	MembershipVersion string
}

type kbuildControlRecipeReadView struct {
	frontier   KbuildControlRecipeFrontier
	mu         sync.Mutex
	reads      []KbuildControlRecipeRead
	byPath     map[string]KbuildControlReadArtifact
	byIdentity map[string]KbuildControlReadArtifact
	byProducer map[CompactKbuildVisibleArtifact]KbuildControlReadArtifact
}

func (v *kbuildControlRecipeReadView) Match(pattern string) []string {
	return v.frontier.Files.Match(pattern)
}

// MatchRead reports one owned virtual object wildcard's complete membership,
// including an empty result. The plain KbuildVirtualFileView.Match signature
// cannot return errors; selected recipe expansion calls this narrower method
// before accepting a matched path whose bytes/producer may be opaque or
// ambiguous. Matched files are resolved on demand, never by copying the
// frontier's full visible-file map.
func (v *kbuildControlRecipeReadView) MatchRead(pattern string) ([]string, error) {
	matches := slices.Clone(v.frontier.Files.Match(pattern))
	sort.Strings(matches)
	matches = slices.Compact(matches)
	literalObjectPath := strings.HasPrefix(pattern, "__LINUX_BZL_OBJECT_TREE__/") &&
		!strings.ContainsAny(pattern, "*?[]\\")
	parts := make([]string, 0, len(matches))
	observed := make([]KbuildControlRecipeRead, 0, len(matches)+1)
	for _, match := range matches {
		if strings.HasPrefix(pattern, "__LINUX_BZL_OBJECT_TREE__/") &&
			!strings.HasPrefix(match, "__LINUX_BZL_OBJECT_TREE__/") {
			return nil, fmt.Errorf("Kbuild wildcard %q match %q has no object-tree owner", pattern, match)
		}
		_, exists, exact, err := v.frontier.Files.Read(match)
		if err != nil {
			return nil, fmt.Errorf("Kbuild wildcard %q match %q: %w", pattern, match, err)
		}
		if !exists {
			return nil, fmt.Errorf("Kbuild wildcard %q match %q has absent or opaque contents", pattern, match)
		}
		var artifact KbuildControlReadArtifact
		if exact {
			artifact, err = v.exactArtifact(match)
		} else if literalObjectPath && match == pattern && len(matches) == 1 {
			artifact, err = v.presenceArtifact(match)
		} else {
			return nil, fmt.Errorf("Kbuild wildcard %q match %q has absent or opaque contents", pattern, match)
		}
		if err != nil {
			return nil, fmt.Errorf("Kbuild wildcard %q match %q: %w", pattern, match, err)
		}
		observed = append(observed, KbuildControlRecipeRead{
			Path: match, Artifact: artifact, Exists: true, Wildcard: true,
		})
		parts = append(parts, strings.Join([]string{
			match, string(artifact.Tree), artifact.Identity, artifact.Version,
			artifact.Producer.Path, artifact.Producer.Profile, artifact.Producer.Target,
		}, "\x00"))
	}
	patternRead := KbuildControlRecipeRead{Path: pattern, Wildcard: true}
	if len(matches) == 0 {
		patternRead.FrontierID = v.frontier.ID
	} else {
		digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		patternRead.Exists = true
		patternRead.MembershipVersion = hex.EncodeToString(digest[:])
	}
	observed = append(observed, patternRead)
	if err := v.recordExact(observed); err != nil {
		return nil, fmt.Errorf("Kbuild wildcard %q: %w", pattern, err)
	}
	return matches, nil
}

func (v *kbuildControlRecipeReadView) Read(logicalPath string) (string, bool, bool, error) {
	contents, exists, exact, err := v.frontier.Files.Read(logicalPath)
	if err != nil {
		return "", false, false, err
	}
	if exists && !exact {
		return "", true, false, fmt.Errorf("Kbuild read %q has opaque contents", logicalPath)
	}
	if !exists {
		// Source paths may also be declared physical inputs. Their final read
		// is recorded after the source-root fallback succeeds or fails.
		if logicalPath != "__LINUX_BZL_SOURCE_TREE__" &&
			!strings.HasPrefix(logicalPath, "__LINUX_BZL_SOURCE_TREE__/") {
			v.record(KbuildControlRecipeRead{Path: logicalPath, FrontierID: v.frontier.ID})
		}
		return "", false, false, nil
	}
	artifact, err := v.exactArtifact(logicalPath)
	if err != nil {
		return "", true, true, err
	}
	if err := v.recordExact([]KbuildControlRecipeRead{{Path: logicalPath, Artifact: artifact, Exists: true}}); err != nil {
		return "", true, true, err
	}
	return contents, true, true, nil
}

func (v *kbuildControlRecipeReadView) exactArtifact(logicalPath string) (KbuildControlReadArtifact, error) {
	return v.artifactFor(logicalPath, v.frontier.ResolveArtifact, "exact")
}

func (v *kbuildControlRecipeReadView) presenceArtifact(logicalPath string) (KbuildControlReadArtifact, error) {
	return v.artifactFor(logicalPath, v.frontier.ResolvePresenceArtifact, "presence")
}

func (v *kbuildControlRecipeReadView) artifactFor(
	logicalPath string,
	resolve func(string) (KbuildControlReadArtifact, bool, error),
	kind string,
) (KbuildControlReadArtifact, error) {
	if resolve == nil {
		return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s read %q has no artifact owner resolver", kind, logicalPath)
	}
	artifact, owned, err := resolve(logicalPath)
	if err != nil {
		return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s read %q owner: %w", kind, logicalPath, err)
	}
	if !owned || artifact.Identity == "" || artifact.Version == "" ||
		strings.ContainsRune(logicalPath, 0) || strings.ContainsRune(artifact.Identity, 0) ||
		strings.ContainsRune(artifact.Version, 0) ||
		(artifact.Tree != CompactKbuildInvocationSourceTree && artifact.Tree != CompactKbuildInvocationObjectTree) {
		return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s read %q has unknown artifact owner/version", kind, logicalPath)
	}
	if (logicalPath == "__LINUX_BZL_OBJECT_TREE__" || strings.HasPrefix(logicalPath, "__LINUX_BZL_OBJECT_TREE__/")) &&
		artifact.Tree != CompactKbuildInvocationObjectTree {
		return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s object read %q resolves to %q owner", kind, logicalPath, artifact.Tree)
	}
	if (logicalPath == "__LINUX_BZL_SOURCE_TREE__" || strings.HasPrefix(logicalPath, "__LINUX_BZL_SOURCE_TREE__/")) &&
		artifact.Tree != CompactKbuildInvocationSourceTree {
		return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s source read %q resolves to %q owner", kind, logicalPath, artifact.Tree)
	}
	if producer := artifact.Producer; producer != (CompactKbuildVisibleArtifact{}) {
		if producer.Path == "" || producer.Profile == "" || producer.Target == "" ||
			strings.ContainsRune(producer.Path, 0) || strings.ContainsRune(producer.Profile, 0) ||
			strings.ContainsRune(producer.Target, 0) ||
			CanonicalKbuildGraphTarget(producer.Path) != producer.Path ||
			CanonicalKbuildGraphTarget(producer.Target) != producer.Target {
			return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s read %q has incomplete or noncanonical selected producer provenance", kind, logicalPath)
		}
		for _, root := range []string{"__LINUX_BZL_OBJECT_TREE__/", "__LINUX_BZL_SOURCE_TREE__/"} {
			if strings.HasPrefix(logicalPath, root) &&
				producer.Path != CanonicalKbuildGraphTarget(strings.TrimPrefix(logicalPath, root)) {
				return KbuildControlReadArtifact{}, fmt.Errorf("Kbuild %s read %q selected producer path %q conflicts with its rooted alias", kind, logicalPath, producer.Path)
			}
		}
	}
	return artifact, nil
}

// recordExact commits a group of actual exact reads atomically. A frozen view
// cannot attribute one logical path, producer identity, or selected writer to
// two versions, even if a faulty resolver reports identical file bytes.
func (v *kbuildControlRecipeReadView) recordExact(reads []KbuildControlRecipeRead) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	var paths map[string]KbuildControlReadArtifact
	var identities map[string]KbuildControlReadArtifact
	var producers map[CompactKbuildVisibleArtifact]KbuildControlReadArtifact
	for _, read := range reads {
		if !read.Exists || read.Artifact.Identity == "" {
			continue
		}
		current := read.Artifact
		old, exists := paths[read.Path]
		if !exists {
			old, exists = v.byPath[read.Path]
		}
		if exists && old != current {
			return fmt.Errorf("Kbuild exact read %q has conflicting selected producer/version provenance", read.Path)
		}
		old, exists = identities[current.Identity]
		if !exists {
			old, exists = v.byIdentity[current.Identity]
		}
		if exists && (old.Version != current.Version || old.Producer != current.Producer) {
			return fmt.Errorf("Kbuild exact read %q has conflicting selected producer/version provenance", read.Path)
		}
		if current.Producer != (CompactKbuildVisibleArtifact{}) {
			old, exists = producers[current.Producer]
			if !exists {
				old, exists = v.byProducer[current.Producer]
			}
			if exists && (old.Identity != current.Identity || old.Version != current.Version) {
				return fmt.Errorf("Kbuild exact read %q has conflicting selected producer/version provenance", read.Path)
			}
		}
		if len(reads) > 1 {
			if paths == nil {
				paths = map[string]KbuildControlReadArtifact{}
				identities = map[string]KbuildControlReadArtifact{}
				producers = map[CompactKbuildVisibleArtifact]KbuildControlReadArtifact{}
			}
			paths[read.Path] = current
			identities[current.Identity] = current
			if current.Producer != (CompactKbuildVisibleArtifact{}) {
				producers[current.Producer] = current
			}
		}
	}
	if len(paths) == 0 && (len(reads) != 1 || !reads[0].Exists || reads[0].Artifact.Identity == "") {
		v.reads = append(v.reads, reads...)
		return nil
	}
	if v.byPath == nil {
		v.byPath = map[string]KbuildControlReadArtifact{}
		v.byIdentity = map[string]KbuildControlReadArtifact{}
		v.byProducer = map[CompactKbuildVisibleArtifact]KbuildControlReadArtifact{}
	}
	if len(reads) == 1 && reads[0].Exists && reads[0].Artifact.Identity != "" {
		read := reads[0]
		v.byPath[read.Path] = read.Artifact
		v.byIdentity[read.Artifact.Identity] = read.Artifact
		if read.Artifact.Producer != (CompactKbuildVisibleArtifact{}) {
			v.byProducer[read.Artifact.Producer] = read.Artifact
		}
	} else {
		for path, artifact := range paths {
			v.byPath[path] = artifact
		}
		for identity, artifact := range identities {
			v.byIdentity[identity] = artifact
		}
		for producer, artifact := range producers {
			v.byProducer[producer] = artifact
		}
	}
	v.reads = append(v.reads, reads...)
	return nil
}

func (v *kbuildControlRecipeReadView) observePhysicalSource(logicalPath, contents string, exists bool) error {
	read := KbuildControlRecipeRead{Path: logicalPath, Exists: exists}
	if exists {
		version := sha256.Sum256([]byte(contents))
		read.Artifact = KbuildControlReadArtifact{
			Tree: CompactKbuildInvocationSourceTree, Identity: logicalPath,
			Version: hex.EncodeToString(version[:]),
		}
	} else {
		// An absent declared source path is immutable across unrelated
		// object-tree actions. A later virtual source writer would report its
		// own exact selected artifact instead of falling through here.
		read.Artifact = KbuildControlReadArtifact{
			Tree: CompactKbuildInvocationSourceTree, Identity: logicalPath,
			Version: "absent",
		}
	}
	if exists {
		return v.recordExact([]KbuildControlRecipeRead{read})
	}
	v.record(read)
	return nil
}

func (v *kbuildControlRecipeReadView) record(read KbuildControlRecipeRead) {
	v.mu.Lock()
	v.reads = append(v.reads, read)
	v.mu.Unlock()
}

func (v *kbuildControlRecipeReadView) readsSnapshot() []KbuildControlRecipeRead {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.reads)
}

// KbuildSelectedControlRecipeLine names the exact rule and line selected by
// the invocation planner. Normal and OrderOnly retain GNU Make's lexical
// prerequisite words for automatic variables and target-specific expansion.
type KbuildSelectedControlRecipeLine struct {
	Target, LookupTarget, AutomaticTarget, Stem string
	RuleIndex, RecipeIndex                      int
	Normal, OrderOnly                           []string
}

// KbuildSelectedControlRecipeSnapshot binds one immutable file view to a
// source-ordered Make state. Its evaluator may be used to expand/lower an
// executable line before ApplyRecipe advances the control state. Reads remains
// available after expansion and returns copies rather than a mutable log.
type KbuildSelectedControlRecipeSnapshot struct {
	Line         KbuildSelectedControlRecipeLine
	Evaluation   KbuildControlEvaluation
	FrontierID   string
	Environment  map[string]string
	CommandShell string
	view         *kbuildControlRecipeReadView
	applied      bool
}

func (s *KbuildSelectedControlRecipeSnapshot) Reads() []KbuildControlRecipeRead {
	if s == nil || s.view == nil {
		return nil
	}
	return s.view.readsSnapshot()
}

// CompactKbuildSelectedControlRecipeSnapshots returns the recorded preline
// Make states for a selected target in its source recipe order. The final
// profile evaluator deliberately observes the invocation-completion frontier;
// callers needing the earlier optional-read state must use these snapshots.
func CompactKbuildSelectedControlRecipeSnapshots(
	profile CompactKbuildProfile, target string,
) []*KbuildSelectedControlRecipeSnapshot {
	return slices.Clone(profile.targetLineReadSnapshots[compactKbuildGraphTargetPath(target)])
}

// KbuildSelectedControlRuleEntry binds the selected rule's identity to the
// Make state when its target was first reached, before prerequisites ran.
// Recipe snapshots separately own their later environments and file views.
type KbuildSelectedControlRuleEntry struct {
	Line    KbuildSelectedControlRecipeLine
	Profile CompactKbuildProfile
}

func CompactKbuildSelectedControlRuleEntrySnapshot(
	profile CompactKbuildProfile, target string,
) *KbuildSelectedControlRuleEntry {
	return profile.targetRuleEntrySnapshots[compactKbuildGraphTargetPath(target)]
}

// ReadIdentity is empty if Make never read a file on this line. Exact reads
// contain the declared owner/version; an absent read also contains the causal
// frontier ID so a later writer cannot share the pre-writer selection.
func (s *KbuildSelectedControlRecipeSnapshot) ReadIdentity() string {
	reads := s.Reads()
	if len(reads) == 0 {
		return ""
	}
	parts := make([]string, 0, len(reads))
	for _, read := range reads {
		parts = append(parts, strings.Join([]string{
			read.Path, fmt.Sprintf("%t", read.Exists), string(read.Artifact.Tree),
			read.Artifact.Identity, read.Artifact.Version,
			read.Artifact.Producer.Path, read.Artifact.Producer.Profile, read.Artifact.Producer.Target,
			read.FrontierID,
			fmt.Sprintf("%t", read.Wildcard), read.MembershipVersion,
		}, "\x00"))
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

// SelectedKbuildControlStepper receives the same selected targets and lines
// in the same order as the invocation planner. The caller
// enters a target after its parent first reaches it, completes prerequisites,
// and supplies a frozen frontier for each concrete line.
type SelectedKbuildControlStepper struct {
	profile             CompactKbuildProfile
	options             KbuildControlEvaluationOptions
	cumulative          *kbuildParser
	generation          uint64
	targetEntries       map[string]kbuildControlTargetEntry
	queries             map[string]KbuildDeferredContentQuery
	queryOrder          []string
	targetEvaluators    map[string]*kbuildTargetEvaluator
	targetActivations   map[string]func() error
	targetGenerations   map[string]uint64
	targetEnvironments  map[string]*compactKbuildEnvironmentSnapshot
	targetShells        map[string]string
	environmentInterner compactKbuildEnvironmentInterner
	lines               map[kbuildControlRecipeKey]*KbuildSelectedControlRecipeSnapshot
	lineOrder           []*KbuildSelectedControlRecipeSnapshot
}

type kbuildControlTargetEntry struct {
	profile           CompactKbuildProfile
	lookupTarget      string
	prerequisiteScope *compactKbuildTargetVariableScope
}

func NewSelectedKbuildControlStepper(
	profile CompactKbuildProfile,
	options KbuildControlEvaluationOptions,
) (*SelectedKbuildControlStepper, error) {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return nil, fmt.Errorf("Kbuild profile %q has no source-derived target evaluator", profile.Name)
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
	profile.targetVariableScopes = maps.Clone(profile.targetVariableScopes)
	if profile.targetVariableScopes == nil {
		profile.targetVariableScopes = map[string]*compactKbuildTargetVariableScope{}
	}
	return &SelectedKbuildControlStepper{
		profile: profile, options: options,
		cumulative: cloneKbuildParserForEvaluation(profile.evaluator.template),
		queries:    queries, queryOrder: queryOrder,
		targetEntries:      map[string]kbuildControlTargetEntry{},
		targetEvaluators:   map[string]*kbuildTargetEvaluator{},
		targetActivations:  map[string]func() error{},
		targetGenerations:  map[string]uint64{},
		targetEnvironments: map[string]*compactKbuildEnvironmentSnapshot{},
		targetShells:       map[string]string{},
		lines:              map[kbuildControlRecipeKey]*KbuildSelectedControlRecipeSnapshot{},
	}, nil
}

// BeginTarget establishes GNU Make's first-reached target-variable scope.
// A prerequisite inherits its parent's non-private target assignments;
// reaching it later by another parent cannot replace the first scope.
func (s *SelectedKbuildControlStepper) BeginTarget(target, makeTarget, parentTarget string) (CompactKbuildProfile, error) {
	target = compactKbuildGraphTargetPath(target)
	if target == "" {
		return CompactKbuildProfile{}, fmt.Errorf("Kbuild control cannot begin an empty target")
	}
	if entry, reached := s.targetEntries[target]; reached {
		return entry.profile, nil
	}
	var inherited *compactKbuildTargetVariableScope
	if parentTarget != "" {
		parent := compactKbuildGraphTargetPath(parentTarget)
		parentEntry, reached := s.targetEntries[parent]
		if !reached {
			return CompactKbuildProfile{}, fmt.Errorf("Kbuild control target %q inherits from unreached parent %q", target, parentTarget)
		}
		inherited = parentEntry.prerequisiteScope
	}
	lookupTarget := compactKbuildRuleLookupTarget(s.profile, target, makeTarget)
	s.profile.targetVariableScopes[target] = inherited
	byPrerequisites := inherited
	if variables := compactKbuildInheritableTargetVariablesForMakeTarget(s.profile, target, lookupTarget); len(variables) != 0 {
		byPrerequisites = &compactKbuildTargetVariableScope{parent: inherited, variables: variables}
	}
	// Prerequisites (including second expansion) see this generation before
	// their recipes run. ApplyRecipe replaces cumulative before any mutation,
	// so the caller can retain its selected rule while descending into children.
	profile := s.profile
	profile.evaluator = &kbuildTargetEvaluator{template: s.cumulative}
	profile.controlGeneration = s.generation
	profile.deferredContentQueries = s.queries
	// Prerequisite probes must not inherit a sibling recipe's process scope.
	profile.probeEnvironmentActivation = s.options.ResetProbeEnvironment
	s.targetEntries[target] = kbuildControlTargetEntry{
		profile: profile, lookupTarget: lookupTarget, prerequisiteScope: byPrerequisites,
	}
	return profile, nil
}

func (s *SelectedKbuildControlStepper) BeforeRecipe(
	line KbuildSelectedControlRecipeLine,
	frontier KbuildControlRecipeFrontier,
) (*KbuildSelectedControlRecipeSnapshot, error) {
	line.Target = compactKbuildGraphTargetPath(line.Target)
	entry, reached := s.targetEntries[line.Target]
	if !reached {
		return nil, fmt.Errorf("Kbuild profile %q selected recipe target %q was not first reached", s.profile.Name, line.Target)
	}
	lookupTarget := entry.lookupTarget
	if line.LookupTarget != "" && line.LookupTarget != lookupTarget {
		return nil, fmt.Errorf("Kbuild profile %q target %q changed lexical lookup from %q to %q", s.profile.Name, line.Target, lookupTarget, line.LookupTarget)
	}
	line.LookupTarget = lookupTarget
	if line.RuleIndex < 0 || line.RuleIndex >= len(s.profile.Rules) ||
		line.RecipeIndex < 0 || line.RecipeIndex >= len(s.profile.Rules[line.RuleIndex].Recipe) {
		return nil, fmt.Errorf("Kbuild profile %q target %q selected invalid rule/recipe indexes %d/%d", s.profile.Name, line.Target, line.RuleIndex, line.RecipeIndex)
	}
	selected := false
	for _, candidate := range compactKbuildRuleCandidatesForMakeTarget(s.profile, line.Target, lookupTarget) {
		if candidate.ruleOrder == line.RuleIndex {
			selected = true
			break
		}
	}
	if !selected {
		return nil, fmt.Errorf("Kbuild profile %q target %q selected recipe rule %d does not match the lexical target", s.profile.Name, line.Target, line.RuleIndex)
	}
	key := kbuildControlRecipeKey{target: line.Target, ruleIndex: line.RuleIndex, recipeIndex: line.RecipeIndex}
	if _, exists := s.lines[key]; exists {
		return nil, fmt.Errorf("Kbuild profile %q target %q selected recipe %d/%d twice", s.profile.Name, line.Target, line.RuleIndex, line.RecipeIndex)
	}
	if frontier.ID == "" || frontier.Files == nil {
		return nil, fmt.Errorf("Kbuild profile %q target %q recipe %d has no immutable file frontier", s.profile.Name, line.Target, line.RecipeIndex)
	}
	if !s.profile.invocationLocationSet {
		return nil, fmt.Errorf("%s: Kbuild profile %q selected recipe %d has no typed invocation location for relative file reads", s.profile.Rules[line.RuleIndex].Position, s.profile.Name, line.RecipeIndex)
	}
	if s.options.ResetProbeEnvironment != nil {
		if err := s.options.ResetProbeEnvironment(); err != nil {
			return nil, fmt.Errorf("restore Kbuild profile %q target %q recipe %d inherited environment: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
		}
	}
	view := &kbuildControlRecipeReadView{frontier: frontier}
	// This evaluator shares the current control generation's immutable Make
	// variables. ApplyRecipe allocates a new generation before an eval changes
	// them, so one source invocation with many selected recipe lines does not
	// copy its complete variable map for every line.
	parser := cloneKbuildParserForTargetEvaluation(s.cumulative, 0, true)
	parser.environmentVariables = maps.Clone(s.cumulative.environmentVariables)
	parser.virtualFileView = view
	parser.invocationLocation = s.profile.invocationLocation
	parser.invocationLocationSet = true
	parser.sourceFileReadObserver = view.observePhysicalSource
	parser.currentPos = s.profile.Rules[line.RuleIndex].Position
	lineProfile := s.profile
	lineProfile.evaluator = &kbuildTargetEvaluator{template: parser}
	lineProfile.targetEvaluators = nil
	lineProfile.targetProbeEnvironmentActivations = nil
	lineProfile.targetControlGenerations = nil
	lineProfile.targetRecipeEnvironments = nil
	lineProfile.targetRecipeShells = nil
	lineProfile.controlGeneration = s.generation
	lineProfile.deferredContentQueries = maps.Clone(s.queries)
	lineEvaluation := KbuildControlEvaluation{Profile: lineProfile}
	for _, token := range s.queryOrder {
		lineEvaluation.Queries = append(lineEvaluation.Queries, s.queries[token])
	}
	if line.AutomaticTarget == "" {
		line.AutomaticTarget = line.LookupTarget
	}
	line.Normal = slices.Clone(line.Normal)
	line.OrderOnly = slices.Clone(line.OrderOnly)
	environment, err := evaluateKbuildControlTargetEnvironmentForMakeTarget(
		lineProfile, line.Target, lookupTarget, line.AutomaticTarget, line.Stem,
		line.Normal, line.OrderOnly, nil, false,
	)
	if err != nil {
		return nil, fmt.Errorf("Kbuild profile %q target %q recipe %d environment: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
	}
	commandValues, err := evaluateKbuildControlTargetForMakeTarget(
		lineProfile, line.Target, lookupTarget, line.AutomaticTarget, line.Stem,
		line.Normal, line.OrderOnly, nil, false, "CONFIG_SHELL",
	)
	if err != nil {
		return nil, fmt.Errorf("Kbuild profile %q target %q recipe %d CONFIG_SHELL: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
	}
	snapshot := &KbuildSelectedControlRecipeSnapshot{
		Line: line, Evaluation: lineEvaluation, FrontierID: frontier.ID,
		Environment: maps.Clone(environment), CommandShell: commandValues["CONFIG_SHELL"], view: view,
	}
	if s.options.BindProbeEnvironment != nil {
		activate, err := s.options.BindProbeEnvironment(environment)
		if err != nil {
			return nil, fmt.Errorf("bind Kbuild profile %q target %q recipe %d probe environment: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
		}
		if activate == nil {
			return nil, fmt.Errorf("bind Kbuild profile %q target %q recipe %d returned nil probe activation", s.profile.Name, line.Target, line.RecipeIndex)
		}
		snapshot.Evaluation.Profile.probeEnvironmentActivation = activate
		if err := activate(); err != nil {
			return nil, fmt.Errorf("activate Kbuild profile %q target %q recipe %d probe environment: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
		}
	}
	s.lines[key] = snapshot
	s.lineOrder = append(s.lineOrder, snapshot)
	return snapshot, nil
}

// ApplyRecipe advances the cumulative Make state only after the caller has
// selected and expanded this exact source line. Ordinary executable lines
// remain attached to their own preline evaluator and read log. An exact outer
// $(eval ...) performs GNU Make's first expansion and second assignment pass.
func (s *SelectedKbuildControlStepper) ApplyRecipe(snapshot *KbuildSelectedControlRecipeSnapshot) error {
	if snapshot == nil || snapshot.applied {
		return fmt.Errorf("Kbuild control recipe snapshot is nil or already applied")
	}
	line := snapshot.Line
	key := kbuildControlRecipeKey{target: line.Target, ruleIndex: line.RuleIndex, recipeIndex: line.RecipeIndex}
	if s.lines[key] != snapshot {
		return fmt.Errorf("Kbuild profile %q target %q recipe %d is not the selected snapshot", s.profile.Name, line.Target, line.RecipeIndex)
	}
	rawLine := s.profile.Rules[line.RuleIndex].Recipe[line.RecipeIndex]
	body, isEval, err := exactKbuildEvalRecipeBody(rawLine)
	if err != nil {
		return fmt.Errorf("%s: Kbuild profile %q target %q: %w", s.profile.Rules[line.RuleIndex].Position, s.profile.Name, line.Target, err)
	}
	if !isEval {
		generation := snapshot.Evaluation.Profile.controlGeneration
		environmentSnapshot, err := s.environmentInterner.intern(snapshot.Environment)
		if err != nil {
			return fmt.Errorf("Kbuild profile %q target %q recipe %d exported environment: %w", s.profile.Name, line.Target, line.RecipeIndex, err)
		}
		// These maps are a compatibility path for callers which still inspect
		// an entire target at once. Its first executable line is stable; the
		// target evaluator refuses a later target-wide read if any selected
		// line has a different generation, exports, shell, or file version.
		if _, exists := s.targetEvaluators[line.Target]; !exists {
			s.targetGenerations[line.Target] = generation
			s.targetEnvironments[line.Target] = environmentSnapshot
			s.targetShells[line.Target] = snapshot.CommandShell
			s.targetEvaluators[line.Target] = snapshot.Evaluation.Profile.evaluator
			if activate := snapshot.Evaluation.Profile.probeEnvironmentActivation; activate != nil {
				s.targetActivations[line.Target] = activate
			}
		}
		snapshot.applied = true
		return nil
	}
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		snapshot.Evaluation.Profile, line.Target, line.LookupTarget, line.AutomaticTarget,
		line.Stem, line.Normal, line.OrderOnly, nil, true, true,
	)
	if err != nil {
		return err
	}
	defer cleanup()
	// Eval's second assignment pass deletes an inherited variable's
	// environment origin. Keep those origin changes in this line until the
	// new cumulative generation is committed; earlier snapshots are immutable.
	parser.environmentVariables = maps.Clone(parser.environmentVariables)
	parser.shellResultAvailable = nil
	pendingQueries := maps.Clone(s.queries)
	pendingOrder := slices.Clone(s.queryOrder)
	parser.shell = func(command string) (string, error) {
		queryProfile := snapshot.Evaluation.Profile
		queryProfile.deferredContentQueries = pendingQueries
		token, err := registerKbuildDeferredContentQuery(
			queryProfile, line.Target, command, "", snapshot.CommandShell, snapshot.Environment,
		)
		if err != nil {
			return "", err
		}
		if !slices.Contains(pendingOrder, token) {
			pendingOrder = append(pendingOrder, token)
		}
		return token, nil
	}
	parser.pushLocal(map[string]string{"$": "$"})
	expanded, err := parser.expandDepth(body, 0)
	parser.popLocal()
	if err != nil {
		return fmt.Errorf("%s: Kbuild profile %q target %q eval expansion: %w", s.profile.Rules[line.RuleIndex].Position, s.profile.Name, line.Target, err)
	}
	var newGeneration *kbuildParser
	var assignments uint64
	for _, assignment := range strings.Split(expanded, "\n") {
		assignment = strings.TrimSpace(assignment)
		if assignment == "" {
			continue
		}
		variable, _, _, ok := splitKbuildAssignment(assignment)
		if !ok || variable == "" || containsMakeReference(variable) {
			return fmt.Errorf("%s: Kbuild profile %q target %q eval result is not an ordinary assignment: %q", s.profile.Rules[line.RuleIndex].Position, s.profile.Name, line.Target, assignment)
		}
		if err := parser.parseLine(assignment, s.profile.Rules[line.RuleIndex].Position); err != nil {
			return fmt.Errorf("Kbuild profile %q target %q eval assignment %q: %w", s.profile.Name, line.Target, assignment, err)
		}
		value, ok := parser.lookupVariable(variable)
		if !ok {
			return fmt.Errorf("Kbuild profile %q target %q eval assignment did not define %q", s.profile.Name, line.Target, variable)
		}
		if newGeneration == nil {
			newGeneration = cloneKbuildParserForEvaluation(s.cumulative)
		}
		copyKbuildControlVariableState(newGeneration, parser, variable, value)
		assignments++
	}
	if newGeneration != nil {
		s.cumulative = newGeneration
		s.generation += assignments
	}
	s.queries = pendingQueries
	s.queryOrder = pendingOrder
	snapshot.applied = true
	return nil
}

// Finish binds the invocation-completion frontier to lazy exported Make
// values, retaining the independent immutable frontier of every recipe line.
// A source := assignment already evaluated during parsing keeps its original
// parse-time bytes; recursive exports may now read files written by recipes.
func (s *SelectedKbuildControlStepper) Finish(
	finalFrontier KbuildControlRecipeFrontier,
) (KbuildControlEvaluation, error) {
	if finalFrontier.ID == "" || finalFrontier.Files == nil || !s.profile.invocationLocationSet {
		return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q has no typed immutable invocation-completion frontier", s.profile.Name)
	}
	for _, line := range s.lineOrder {
		if !line.applied {
			return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q target %q selected recipe %d/%d was not applied", s.profile.Name, line.Line.Target, line.Line.RuleIndex, line.Line.RecipeIndex)
		}
	}
	result := KbuildControlEvaluation{Profile: s.profile}
	finalView := &kbuildControlRecipeReadView{frontier: finalFrontier}
	finalParser := cloneKbuildParserForEvaluation(s.cumulative)
	finalParser.virtualFileView = finalView
	finalParser.invocationLocation = s.profile.invocationLocation
	finalParser.invocationLocationSet = true
	finalParser.sourceFileReadObserver = finalView.observePhysicalSource
	finalParser.currentPos = Position{Filename: s.profile.Path}
	result.Profile.evaluator = &kbuildTargetEvaluator{template: finalParser}
	result.Profile.controlGeneration = s.generation
	result.Profile.targetEvaluators = s.targetEvaluators
	result.Profile.targetProbeEnvironmentActivations = s.targetActivations
	result.Profile.targetControlGenerations = s.targetGenerations
	result.Profile.targetRecipeEnvironments = s.targetEnvironments
	result.Profile.targetRecipeShells = s.targetShells
	result.Profile.targetLineReadSnapshots = map[string][]*KbuildSelectedControlRecipeSnapshot{}
	result.Profile.targetRuleEntrySnapshots = map[string]*KbuildSelectedControlRuleEntry{}
	for _, line := range s.lineOrder {
		if _, exists := result.Profile.targetRuleEntrySnapshots[line.Line.Target]; !exists {
			result.Profile.targetRuleEntrySnapshots[line.Line.Target] = &KbuildSelectedControlRuleEntry{
				Line: line.Line, Profile: s.targetEntries[line.Line.Target].profile,
			}
		}
		if _, isEval, err := exactKbuildEvalRecipeBody(s.profile.Rules[line.Line.RuleIndex].Recipe[line.Line.RecipeIndex]); err == nil && !isEval {
			result.Profile.targetLineReadSnapshots[line.Line.Target] = append(result.Profile.targetLineReadSnapshots[line.Line.Target], line)
		}
	}
	result.Profile.deferredContentQueries = maps.Clone(s.queries)
	for _, token := range s.queryOrder {
		result.Queries = append(result.Queries, s.queries[token])
	}
	if s.options.BindProbeEnvironment != nil {
		if s.options.ResetProbeEnvironment != nil {
			if err := s.options.ResetProbeEnvironment(); err != nil {
				return KbuildControlEvaluation{}, fmt.Errorf("restore Kbuild profile %q final inherited environment: %w", s.profile.Name, err)
			}
		}
		environment, err := ExportedKbuildControlVariables(result)
		if err != nil {
			return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q final probe environment: %w", s.profile.Name, err)
		}
		activate, err := s.options.BindProbeEnvironment(environment)
		if err != nil || activate == nil {
			return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q final probe environment activation: %w", s.profile.Name, err)
		}
		if err := activate(); err != nil {
			return KbuildControlEvaluation{}, fmt.Errorf("Kbuild profile %q final probe environment: %w", s.profile.Name, err)
		}
		result.Profile.probeEnvironmentActivation = activate
	}
	return result, nil
}
