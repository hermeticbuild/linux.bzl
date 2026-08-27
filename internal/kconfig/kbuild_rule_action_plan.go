package kconfig

// This file lowers object-to-object transformations from the evaluated Kbuild
// profiles.  Linux uses pattern rules for these actions (for example, a
// position-independent object compiled as %.o and transformed into %.pi.o).
// The planner deliberately keys off the selected rule and command, never an
// architecture or output filename.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type compactKbuildRulePlanBuilder struct {
	metadata                     *CompactMetadata
	plan                         *ActionPlan
	selectionGraph               *compactKbuildSelectionGraph
	selection                    compactKbuildSelectionKey
	selectionBound               bool
	memo                         map[string]string
	stack                        map[string]bool
	context                      compactKbuildRulePlanContext
	profile                      *CompactKbuildProfile
	initialObjectTreeArtifacts   []CompactKbuildVisibleArtifact
	generatedObjectTreeArtifacts []CompactKbuildVisibleArtifact
	// observedOutputs are internal, always-present capture artifacts for opaque
	// paths which a selected command may create as side effects. They are not
	// attributed to the command statically: execution records whether the path
	// changed, and a later resolver publishes the unique observed result.
	observedOutputs map[string][]compactKbuildObservedOutput
	// resolvedSideOutputs binds one consumer to execution-resolved opaque
	// prerequisites. These edges are deliberately consumer-local: a resolver
	// result is not retroactively registered as a source selection owner.
	resolvedSideOutputs map[string]compactKbuildRuleInput
}

type compactKbuildObservedOutput struct {
	output     ActionPlanOutput
	path       string
	baseInputs []compactKbuildRuleInput
}

func (b compactKbuildRulePlanBuilder) withSelectionGraph(graph *compactKbuildSelectionGraph) compactKbuildRulePlanBuilder {
	b.selectionGraph = graph
	return b
}

type compactKbuildRulePlanContext struct {
	Stage                 string
	OutputTree            string
	Product               string
	UsesInitialObjectTree bool
}

var defaultCompactKbuildRulePlanContext = compactKbuildRulePlanContext{
	Stage: "target", OutputTree: "objects", Product: "vmlinux",
}

func newCompactKbuildRulePlanBuilder(metadata *CompactMetadata, plan *ActionPlan) compactKbuildRulePlanBuilder {
	builder := compactKbuildRulePlanBuilder{
		metadata:            metadata,
		plan:                plan,
		memo:                map[string]string{},
		stack:               map[string]bool{},
		context:             defaultCompactKbuildRulePlanContext,
		observedOutputs:     map[string][]compactKbuildObservedOutput{},
		resolvedSideOutputs: map[string]compactKbuildRuleInput{},
	}
	if plan != nil {
		builder.selectionGraph = plan.selectionGraph
	}
	return builder
}

func (b compactKbuildRulePlanBuilder) withResolvedSideOutputs(
	inputs map[string]compactKbuildRuleInput,
) compactKbuildRulePlanBuilder {
	b.resolvedSideOutputs = make(map[string]compactKbuildRuleInput, len(inputs))
	for target, input := range inputs {
		b.resolvedSideOutputs[canonicalKbuildRulePath(target)] = input
	}
	return b
}

func (b compactKbuildRulePlanBuilder) forObservedOutputs(
	target string,
	observations []compactKbuildObservedOutput,
) (compactKbuildRulePlanBuilder, error) {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return b, fmt.Errorf("observed Kbuild outputs require a concrete candidate target")
	}
	cloned := make(map[string][]compactKbuildObservedOutput, len(b.observedOutputs)+1)
	for producer, existing := range b.observedOutputs {
		cloned[producer] = append([]compactKbuildObservedOutput(nil), existing...)
	}
	seen := map[string]compactKbuildObservedOutput{}
	for _, existing := range cloned[target] {
		seen[existing.output.Tree+"\x00"+actionPlanOutputArtifactPath(existing.output)] = existing
	}
	for _, observation := range observations {
		observation.baseInputs = append([]compactKbuildRuleInput(nil), observation.baseInputs...)
		observation.path = canonicalKbuildRulePath(observation.path)
		observation.output.Path = canonicalKbuildRulePath(observation.output.Path)
		if observation.path == "" {
			return b, fmt.Errorf("observed Kbuild output has an empty working-tree path")
		}
		if err := validatePlanRelativePath("observed Kbuild path", observation.path); err != nil {
			return b, err
		}
		if !LinuxKernelPlanTrees[observation.output.Tree] {
			return b, fmt.Errorf("observed Kbuild capture %q uses unsupported tree %q", observation.output.Path, observation.output.Tree)
		}
		if err := validatePlanRelativePath("observed Kbuild capture", observation.output.Path); err != nil {
			return b, err
		}
		if observation.output.ArtifactPath != "" {
			if err := validatePlanRelativePath("observed Kbuild physical capture", observation.output.ArtifactPath); err != nil {
				return b, err
			}
		}
		seenBaseInputs := map[string]bool{}
		for _, input := range observation.baseInputs {
			if input.producer == "" || input.slot < 0 {
				return b, fmt.Errorf("observed Kbuild output %q has invalid base input %#v", observation.path, input)
			}
			identity := fmt.Sprintf("%s\x00%d", input.producer, input.slot)
			if seenBaseInputs[identity] {
				return b, fmt.Errorf("observed Kbuild output %q repeats base input %s slot %d", observation.path, input.producer, input.slot)
			}
			seenBaseInputs[identity] = true
		}
		identity := observation.output.Tree + "\x00" + actionPlanOutputArtifactPath(observation.output)
		if existing, exists := seen[identity]; exists {
			if existing.path != observation.path || !slices.Equal(existing.baseInputs, observation.baseInputs) {
				return b, fmt.Errorf("observed Kbuild capture %s has conflicting path or base state inputs", identity)
			}
			continue
		}
		seen[identity] = observation
		observation.output.ObservedPath = observation.path
		cloned[target] = append(cloned[target], observation)
	}
	b.observedOutputs = cloned
	return b, nil
}

// forOutput returns an independent builder which lowers the same evaluated
// Kbuild graph into the requested plan stage and output namespace.
func (b compactKbuildRulePlanBuilder) forOutput(stage, outputTree, product string) compactKbuildRulePlanBuilder {
	b.context = compactKbuildRulePlanContext{Stage: stage, OutputTree: outputTree, Product: product}
	b.memo = map[string]string{}
	b.stack = map[string]bool{}
	return b
}

func (b compactKbuildRulePlanBuilder) withInitialObjectTree(
	uses bool,
	artifacts ...CompactKbuildVisibleArtifact,
) compactKbuildRulePlanBuilder {
	b.context.UsesInitialObjectTree = uses
	b.initialObjectTreeArtifacts = append([]CompactKbuildVisibleArtifact(nil), artifacts...)
	return b
}

func (b compactKbuildRulePlanBuilder) withGeneratedObjectTreeArtifacts(
	artifacts ...CompactKbuildVisibleArtifact,
) compactKbuildRulePlanBuilder {
	b.generatedObjectTreeArtifacts = append([]CompactKbuildVisibleArtifact(nil), artifacts...)
	return b
}

// forProfile binds lowering to the exact Make invocation recorded by a
// CompactKbuildSelection. The profile is an authoritative foreign key, not a
// hint for a global longest-directory search.
func (b compactKbuildRulePlanBuilder) forProfile(profile CompactKbuildProfile) compactKbuildRulePlanBuilder {
	b.profile = &profile
	b.selection = compactKbuildSelectionKey{}
	b.selectionBound = false
	b.memo = map[string]string{}
	b.stack = map[string]bool{}
	return b
}

// forSelection binds production lowering to one concrete selected Make
// target.  The profile, overwrite position, and every cross-selection input
// then come from the same validated graph key.
func (b compactKbuildRulePlanBuilder) forSelection(
	key compactKbuildSelectionKey,
	profile CompactKbuildProfile,
) compactKbuildRulePlanBuilder {
	b = b.forProfile(profile)
	b.selection = key
	b.selectionBound = true
	return b
}

func (b *compactKbuildRulePlanBuilder) planContext() compactKbuildRulePlanContext {
	context := b.context
	if context.Stage == "" {
		context.Stage = defaultCompactKbuildRulePlanContext.Stage
	}
	if context.OutputTree == "" {
		context.OutputTree = defaultCompactKbuildRulePlanContext.OutputTree
	}
	if context.Product == "" {
		context.Product = defaultCompactKbuildRulePlanContext.Product
	}
	return context
}

func (b *compactKbuildRulePlanBuilder) actionScope() string {
	if b != nil && (b.planContext().Stage == "prehost" || b.planContext().Stage == "host") {
		return "host"
	}
	return "target"
}

func (b *compactKbuildRulePlanBuilder) configuredActionRole(ref KbuildActionRoleRef) bool {
	if b == nil || b.metadata == nil {
		return false
	}
	return slices.Contains(b.metadata.actionRoles, ref)
}

func (b *compactKbuildRulePlanBuilder) actionNode(kind, tool, target string) ActionPlanNode {
	context := b.planContext()
	return ActionPlanNode{
		Stage: context.Stage, Kind: kind, Tool: tool, Product: context.Product,
		Outputs: []ActionPlanOutput{{Tree: context.OutputTree, Path: target}},
	}
}

// bindCompactKbuildSourceOverlayWorkingTree closes the complete, selected
// out-of-tree source namespace into a concrete recipe's private writable view.
// The namespace is planner routing metadata supplied by the module rule; no
// executor-facing tree name is inferred from M=, a compiler role, or a module
// filename.
func (b *compactKbuildRulePlanBuilder) bindCompactKbuildSourceOverlayWorkingTree(
	profile CompactKbuildProfile,
	recipe *ActionRecipe,
) error {
	root, overlay, err := compactKbuildSourceOverlayRoot(profile)
	if err != nil {
		return err
	}
	if !overlay {
		return nil
	}
	if b == nil || b.metadata == nil {
		return fmt.Errorf("Kbuild source overlay %q requires source namespace metadata", root)
	}
	namespace, selected, err := b.metadata.selectedActionPlanSourceNamespace(root)
	if err != nil {
		return fmt.Errorf("Kbuild source overlay %q namespace: %w", root, err)
	}
	if !selected {
		return fmt.Errorf("Kbuild source overlay %q has no selected source namespace", root)
	}
	if !slices.Contains(recipe.WorkingTrees, namespace) {
		recipe.WorkingTrees = append(recipe.WorkingTrees, namespace)
		sort.Strings(recipe.WorkingTrees)
	}
	return nil
}

func compactKbuildPlanNode(plan *ActionPlan, id string) (ActionPlanNode, bool) {
	if plan == nil {
		return ActionPlanNode{}, false
	}
	plan.ensureNodeLookupIndexes()
	node, ok := plan.nodesByID[id]
	return node, ok
}

func compactKbuildRuleEvaluationContext(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (string, []string, []string, error) {
	if !match.resolved {
		// Focused lowering tests may supply an already-expanded synthetic match
		// without a parsed rule declaration. Production matches are always marked
		// resolved and retain Make's raw prerequisite context below.
		return match.stem, compactKbuildInputPaths(inputs, false), compactKbuildInputPaths(inputs, true), nil
	}
	normal, orderOnly, stem, err := evaluatedKbuildSelectedTargetRuleContext(match.profile, target, &match)
	if err != nil {
		return "", nil, nil, err
	}
	return stem, normal, orderOnly, nil
}

// compactKbuildRuleAutomaticEvaluationContext returns the exact logical Make
// spellings retained beside canonical graph identities by the selected rule
// context. Tree-root projection happens only when injected $(obj)/$(src)
// variables require automatic words in those same namespaces.
func compactKbuildRuleAutomaticEvaluationContext(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (compactKbuildAutomaticContext, error) {
	if !match.resolved {
		stem, normal, orderOnly, err := compactKbuildRuleEvaluationContext(target, match, inputs)
		if err != nil {
			return compactKbuildAutomaticContext{}, err
		}
		return compactKbuildAutomaticContext{target: target, stem: stem, normal: normal, order: orderOnly}, nil
	}
	context, err := evaluatedKbuildSelectedTargetMakeContext(match.profile, target, &match)
	if err != nil {
		return compactKbuildAutomaticContext{}, err
	}
	normal := make([]string, len(context.normal))
	for index, prerequisite := range context.normal {
		normal[index] = prerequisite.makeWord
	}
	orderOnly := make([]string, len(context.orderOnly))
	for index, prerequisite := range context.orderOnly {
		orderOnly[index] = prerequisite.makeWord
	}
	return compactKbuildAutomaticContext{
		target: context.target.makeWord,
		stem:   context.stem,
		normal: normal,
		order:  orderOnly,
	}, nil
}

func compactKbuildAutomaticEvaluationUsesTreeRoots(injected map[string]string) bool {
	for _, value := range injected {
		if strings.Contains(value, "__LINUX_BZL_SOURCE_TREE__") ||
			strings.Contains(value, "__LINUX_BZL_OBJECT_TREE__") ||
			strings.Contains(value, compactKbuildActionSourceTreeMarker) ||
			strings.Contains(value, compactKbuildActionObjectTreeMarker) ||
			strings.Contains(value, compactKbuildActionHostDepsTreeMarker) {
			return true
		}
	}
	return false
}

func compactKbuildAutomaticEvaluationUsesPrivateActionRoots(injected map[string]string) bool {
	for _, value := range injected {
		if strings.Contains(value, compactKbuildActionSourceTreeMarker) ||
			strings.Contains(value, compactKbuildActionObjectTreeMarker) ||
			strings.Contains(value, compactKbuildActionHostDepsTreeMarker) {
			return true
		}
	}
	return false
}

var compactKbuildPrivateActionRootMarkerReplacer = strings.NewReplacer(
	"__LINUX_BZL_SOURCE_TREE__", compactKbuildActionSourceTreeMarker,
	"__LINUX_BZL_OBJECT_TREE__", compactKbuildActionObjectTreeMarker,
	linuxProbeHostDepsSentinel, compactKbuildActionHostDepsTreeMarker,
	"${tree:kernel}", compactKbuildActionSourceTreeMarker,
	"${tree:prep}", compactKbuildActionObjectTreeMarker,
	"${tree:"+linuxProbeHostDepsRootName+"}", compactKbuildActionHostDepsTreeMarker,
)

func compactKbuildPrivateActionRootMarker(marker string) string {
	return compactKbuildPrivateActionRootMarkerReplacer.Replace(marker)
}

func compactKbuildRootedAutomaticWord(word, graphPath, marker string) string {
	word = strings.TrimSpace(word)
	graphPath = compactKbuildGraphTargetPath(graphPath)
	if word == "" || graphPath == "" || graphPath == "FORCE" {
		return word
	}
	if compactKbuildEvaluatedPathNamespace(word) != "" {
		return word
	}
	if compactKbuildGraphTargetPath(word) != graphPath {
		// Recursive Make drivers may expose a declaration-local automatic word
		// (for example "objtool") while its graph identity is rooted below the
		// invocation directory ("tools/objtool"). Keep that exact Make word so
		// source expressions such as $(OUTPUT)$@ concatenate once; the evaluated
		// result is rooted when physical recipe roots are canonicalized.
		return word
	}
	// Keep GNU Make's lexical target spelling through textual functions. Final
	// action rewriting projects this rooted alias back to graphPath only when it
	// remains a complete automatic-variable pathname.
	for strings.HasPrefix(word, "./") {
		word = strings.TrimPrefix(word, "./")
	}
	return marker + "/" + word
}

func compactKbuildAutomaticGraphPathMarker(
	profile CompactKbuildProfile,
	graphPath string,
	inputs []compactKbuildRuleInput,
) (string, error) {
	graphPath = compactKbuildGraphTargetPath(graphPath)
	for _, input := range inputs {
		if compactKbuildGraphTargetPath(input.path) != graphPath {
			continue
		}
		if input.producer == "" && input.sourceID != "" && !input.objectTree {
			overlay, err := compactKbuildGraphPathUsesSourceOverlay(profile, graphPath)
			if err != nil {
				return "", err
			}
			if !overlay {
				return "__LINUX_BZL_SOURCE_TREE__", nil
			}
		}
		return "__LINUX_BZL_OBJECT_TREE__", nil
	}
	if compactKbuildProfileSourcePathExists(profile, graphPath) {
		overlay, err := compactKbuildGraphPathUsesSourceOverlay(profile, graphPath)
		if err != nil {
			return "", err
		}
		if !overlay {
			return "__LINUX_BZL_SOURCE_TREE__", nil
		}
	}
	return "__LINUX_BZL_OBJECT_TREE__", nil
}

// compactKbuildRuleRootedAutomaticEvaluationContext moves automatic-variable
// words into the same stable namespaces as injected $(obj) and $(src). This is
// part of Make evaluation, not argv rewriting: textual functions such as
//
//	$(patsubst $(obj)/%,%,$(real-prereqs))
//
// must compare like-for-like values before their result is bound to concrete
// action inputs. Selected edges carry the authoritative tree provenance. A
// source lookup is used only by discovery callers that have not materialized
// those edges yet.
func compactKbuildRuleRootedAutomaticEvaluationContext(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	injected map[string]string,
) (compactKbuildAutomaticContext, error) {
	logical, err := compactKbuildRuleAutomaticEvaluationContext(target, match, inputs)
	if err != nil || !compactKbuildAutomaticEvaluationUsesTreeRoots(injected) {
		return logical, err
	}
	privateRoots := compactKbuildAutomaticEvaluationUsesPrivateActionRoots(injected)
	rootMarker := func(marker string) string {
		if privateRoots {
			return compactKbuildPrivateActionRootMarker(marker)
		}
		return marker
	}
	root := func(word, graphPath string) (string, error) {
		marker, err := compactKbuildAutomaticGraphPathMarker(match.profile, graphPath, inputs)
		if err != nil {
			return "", err
		}
		return compactKbuildRootedAutomaticWord(word, graphPath, rootMarker(marker)), nil
	}
	if !match.resolved {
		logical.target = compactKbuildRootedAutomaticWord(logical.target, target, rootMarker("__LINUX_BZL_OBJECT_TREE__"))
		for index := range logical.normal {
			logical.normal[index], err = root(logical.normal[index], logical.normal[index])
			if err != nil {
				return compactKbuildAutomaticContext{}, err
			}
		}
		for index := range logical.order {
			logical.order[index], err = root(logical.order[index], logical.order[index])
			if err != nil {
				return compactKbuildAutomaticContext{}, err
			}
		}
		return logical, nil
	}
	detailed, err := evaluatedKbuildSelectedTargetMakeContext(match.profile, target, &match)
	if err != nil {
		return compactKbuildAutomaticContext{}, err
	}
	logical.target = compactKbuildRootedAutomaticWord(
		detailed.target.makeWord, detailed.target.graphPath, rootMarker("__LINUX_BZL_OBJECT_TREE__"),
	)
	logical.stem = detailed.stem
	logical.normal = make([]string, len(detailed.normal))
	for index, prerequisite := range detailed.normal {
		logical.normal[index], err = root(prerequisite.makeWord, prerequisite.graphPath)
		if err != nil {
			return compactKbuildAutomaticContext{}, err
		}
	}
	logical.order = make([]string, len(detailed.orderOnly))
	for index, prerequisite := range detailed.orderOnly {
		logical.order[index], err = root(prerequisite.makeWord, prerequisite.graphPath)
		if err != nil {
			return compactKbuildAutomaticContext{}, err
		}
	}
	return logical, nil
}

var compactKbuildMaterializeActionTreeMarkerReplacer = strings.NewReplacer(
	compactKbuildActionSourceTreeMarker, "__LINUX_BZL_SOURCE_TREE__",
	compactKbuildActionObjectTreeMarker, "__LINUX_BZL_OBJECT_TREE__",
	compactKbuildActionHostDepsTreeMarker, linuxProbeHostDepsSentinel,
)

func compactKbuildMaterializeActionTreeMarkers(value string) string {
	return compactKbuildMaterializeActionTreeMarkerReplacer.Replace(value)
}

// compactKbuildActionTreeInjections changes only planner-owned invocation
// roots into unforgeable action markers. Keeping this conversion at the
// action-lowering boundary lets path algebra distinguish injected roots from
// identical bytes authored in a Make recipe.
func compactKbuildActionTreeInjections(injected map[string]string) map[string]string {
	rooted := make(map[string]string, len(injected))
	for name, value := range injected {
		switch name {
		case "abs_output", "abs_srctree", "obj", "objtree", "src", "srcroot", "srctree":
			value = compactKbuildPrivateActionRootMarker(value)
		}
		rooted[name] = value
	}
	return rooted
}

// compactKbuildFinalizeRootedActionRecipeText projects planner-owned private
// roots directly onto plan tree bindings. Avoiding a private -> public
// sentinel round trip keeps identical source-authored sentinel bytes inert.
var compactKbuildFinalizeRootedActionRecipeReplacer = strings.NewReplacer(
	compactKbuildActionSourceTreeMarker, "${tree:kernel}",
	compactKbuildActionObjectTreeMarker, "${tree:prep}",
	compactKbuildActionHostDepsTreeMarker, "${tree:"+linuxProbeHostDepsRootName+"}",
)

func compactKbuildFinalizeRootedActionRecipeText(value string) string {
	value = compactKbuildCollapseActionRootJoins(value)
	return compactKbuildFinalizeRootedActionRecipeReplacer.Replace(value)
}

// compactKbuildCollapseActionRootJoins applies path algebra to roots injected
// by this lowering pass. Kbuild frequently joins $(objtree)/$(obj) or
// $(srctree)/$(obj), while the evaluator represents both operands as rooted
// paths. The left operand owns the joined path's tree; the second root denotes
// the same relative directory and is therefore idempotent. Private control-byte
// markers make this safe from source-authored placeholder-like text.
func compactKbuildCollapseActionRootJoins(value string) string {
	markers := []string{
		compactKbuildActionSourceTreeMarker,
		compactKbuildActionObjectTreeMarker,
		compactKbuildActionHostDepsTreeMarker,
	}
	for {
		previous := value
		for _, outer := range markers {
			for _, inner := range markers {
				value = strings.ReplaceAll(value, outer+"/"+inner, outer)
			}
		}
		if value == previous {
			return value
		}
	}
}

// protectCompactKbuildSourceLiteralActionMarkers tags reserved tree spellings
// authored as Makefile payload. It runs while reading the source, before Make
// expansion, so an expanded $(objtree) still retains the planner provenance
// carried by its injected value while identical literal bytes cannot turn into
// a tree capability after shell parsing. GNU Make ignores shell quotes while
// expanding recipes, so protection is intentionally independent of quoting.
// The protected bytes are restored only after the final script and its literal
// offsets have been assembled.
func protectCompactKbuildSourceLiteralActionMarkers(value string) (string, error) {
	if strings.Contains(value, compactKbuildLiteralTreeEscapeByte) ||
		strings.Contains(value, compactKbuildLiteralSentinelEscapeByte) {
		return "", fmt.Errorf("source action text contains a reserved literal-marker byte")
	}
	var protected strings.Builder
	for index := 0; index < len(value); {
		if value[index] == '$' {
			dollars := index
			for dollars < len(value) && value[dollars] == '$' {
				dollars++
			}
			// Only an even run ends in an escaped dollar which can reach the
			// shell as literal ${tree:...} data. Keep the preceding dollars in
			// their original Make phase and replace only the final dollar. This
			// preserves the raw spelling observed by $(value ...): $${tree:x}
			// becomes $<marker>{tree:x}, then one ordinary expansion collapses
			// that pair to <marker>{tree:x}.
			if count := dollars - index; count >= 2 && count%2 == 0 &&
				dollars < len(value) && strings.HasPrefix(value[dollars:], "{tree:") {
				end := strings.IndexByte(value[dollars+len("{tree:"):], '}')
				if end >= 0 {
					end += dollars + len("{tree:") + 1
					protected.WriteString(value[index : dollars-1])
					protected.WriteString(compactKbuildLiteralTreeEscapeByte)
					protected.WriteString(value[dollars:end])
					index = end
					continue
				}
			}
		}
		matched := false
		for _, sentinel := range []string{
			"__LINUX_BZL_SOURCE_TREE__",
			"__LINUX_BZL_OBJECT_TREE__",
			linuxProbeHostDepsSentinel,
		} {
			if !strings.HasPrefix(value[index:], sentinel) {
				continue
			}
			protected.WriteString(compactKbuildLiteralSentinelEscapeByte)
			protected.WriteString(sentinel[1:])
			index += len(sentinel)
			matched = true
			break
		}
		if matched {
			continue
		}
		protected.WriteByte(value[index])
		index++
	}
	return protected.String(), nil
}

func compactKbuildContainsProtectedLiteralActionMarker(value string) bool {
	return strings.Contains(value, compactKbuildLiteralTreeEscapeByte) ||
		strings.Contains(value, compactKbuildLiteralSentinelEscapeByte)
}

func evaluateCompactKbuildRuleVariables(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	injected map[string]string,
	names ...string,
) (map[string]string, error) {
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, match, inputs)
	if err != nil {
		return nil, err
	}
	values, err := evaluateCompactKbuildRecipeVariablesForMakeTarget(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem, automatic.normal, automatic.order,
		injected, names...,
	)
	if err != nil {
		return nil, err
	}
	return values, nil
}

func evaluateCompactKbuildRuleVariablesRooted(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	injected map[string]string,
	names ...string,
) (map[string]string, error) {
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return nil, err
	}
	values, err := evaluateCompactKbuildRecipeVariablesForMakeTarget(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem, automatic.normal, automatic.order,
		injected, names...,
	)
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (b *compactKbuildRulePlanBuilder) existingProducer(target string) (string, int, bool) {
	target = canonicalKbuildRulePath(target)
	consumerStage := b.planContext().Stage
	trees := []string{"vmlinux", "objects", "prep", "host", "bootstrap", "prehost", "metadata", "modules", "image"}
	switch b.planContext().Stage {
	case "prehost":
		trees = []string{"prehost", "bootstrap", "host", "prep", "vmlinux", "objects", "metadata", "modules", "image"}
	case "bootstrap":
		trees = []string{"bootstrap", "prehost", "vmlinux", "objects", "prep", "host", "metadata", "modules", "image"}
	case "host":
		trees = []string{"host", "bootstrap", "prehost", "prep", "vmlinux", "objects", "metadata", "modules", "image"}
	}
	for _, tree := range trees {
		if producer, slot, ok := planProducerByOutput(b.plan, tree, target); ok {
			node, exists := compactKbuildPlanNode(b.plan, producer)
			if exists && compactKbuildPlanStageVisible(node.Stage, consumerStage) {
				return producer, slot, true
			}
		}
	}
	if target == "vmlinux" {
		if producer, slot, ok := planProducerByOutput(b.plan, "vmlinux", "vmlinux"); ok {
			node, exists := compactKbuildPlanNode(b.plan, producer)
			if exists && compactKbuildPlanStageVisible(node.Stage, consumerStage) {
				return producer, slot, true
			}
		}
	}
	return "", 0, false
}

func compactKbuildPlanStageVisible(producerStage, consumerStage string) bool {
	return compactKbuildSelectionStage(producerStage) &&
		compactKbuildSelectionStage(consumerStage) &&
		compactKbuildSelectionStageOrder(producerStage) <= compactKbuildSelectionStageOrder(consumerStage)
}

// existingInput resolves a logical object-tree pathname at the consumer's
// physical stage. Native producers are preferred. A later internal copy
// projection is not itself visible, but its source or earlier-stage input may
// be staged at the projection's logical pathname. No generated later-stage
// action is pulled backward across the bounded prehost -> bootstrap -> host ->
// prep -> target graph.
func (b *compactKbuildRulePlanBuilder) existingInput(target string) (compactKbuildRuleInput, bool, error) {
	return b.existingInputWithSelectedPathFallback(target, true)
}

func (b *compactKbuildRulePlanBuilder) existingRecordedInput(target string) (compactKbuildRuleInput, bool, error) {
	return b.existingInputWithSelectedPathFallback(target, false)
}

func (b *compactKbuildRulePlanBuilder) existingInputWithSelectedPathFallback(
	target string,
	allowUniqueWriterFallback bool,
) (compactKbuildRuleInput, bool, error) {
	target = canonicalKbuildRulePath(target)
	if input, ok := b.resolvedSideOutputs[target]; ok {
		if input.path == "" {
			input.path = target
		}
		return input, true, nil
	}
	if b.selectionBound && b.selectionGraph != nil {
		resolve := b.selectionGraph.compactKbuildSelectionRecordedPathOwner
		if allowUniqueWriterFallback {
			resolve = b.selectionGraph.compactKbuildSelectionPathOwner
		}
		owner, selected, err := resolve(b.selection, target)
		if err != nil {
			return compactKbuildRuleInput{}, false, err
		}
		if selected {
			producer, materialized := b.selectionGraph.materializedProducers[owner]
			if !materialized {
				return compactKbuildRuleInput{}, false, fmt.Errorf(
					"Kbuild selection %s input %q exact owner %s has not been materialized",
					compactKbuildSelectionKeyString(b.selection), target,
					compactKbuildSelectionKeyString(owner),
				)
			}
			node, exists := compactKbuildPlanNode(b.plan, producer)
			if !exists {
				return compactKbuildRuleInput{}, false, fmt.Errorf(
					"Kbuild selection %s input %q exact owner %s references absent producer %q",
					compactKbuildSelectionKeyString(b.selection), target,
					compactKbuildSelectionKeyString(owner), producer,
				)
			}
			if !compactKbuildPlanStageVisible(node.Stage, b.planContext().Stage) {
				return compactKbuildRuleInput{}, false, fmt.Errorf(
					"Kbuild selection %s input %q exact owner %s is not visible from %s stage",
					compactKbuildSelectionKeyString(b.selection), target,
					compactKbuildSelectionKeyString(owner), b.planContext().Stage,
				)
			}
			slot := -1
			for index, output := range node.Outputs {
				if output.Path != target {
					continue
				}
				if slot >= 0 {
					return compactKbuildRuleInput{}, false, fmt.Errorf(
						"Kbuild selection %s input %q exact producer %q publishes that path more than once",
						compactKbuildSelectionKeyString(b.selection), target, producer,
					)
				}
				slot = index
			}
			if slot < 0 {
				return compactKbuildRuleInput{}, false, fmt.Errorf(
					"Kbuild selection %s input %q exact owner %s producer %q does not publish that logical path",
					compactKbuildSelectionKeyString(b.selection), target,
					compactKbuildSelectionKeyString(owner), producer,
				)
			}
			return compactKbuildRuleInput{path: target, producer: producer, slot: slot}, true, nil
		}
		if !allowUniqueWriterFallback && len(b.selectionGraph.outputOwnersByPath[target]) != 0 {
			return compactKbuildRuleInput{}, false, nil
		}
	}
	if producer, slot, ok := b.existingProducer(target); ok {
		return compactKbuildRuleInput{path: target, producer: producer, slot: slot}, true, nil
	}
	trees := []string{"vmlinux", "objects", "prep", "host", "bootstrap", "prehost", "metadata", "modules", "image"}
	for _, tree := range trees {
		producer, slot, ok := planProducerByOutput(b.plan, tree, target)
		if !ok {
			continue
		}
		input, visible, err := b.stageVisibleProjectionInput(producer, slot, target, map[string]bool{})
		if err != nil {
			return compactKbuildRuleInput{}, false, err
		}
		if visible {
			return input, true, nil
		}
	}
	return compactKbuildRuleInput{}, false, nil
}

// exactObjectTreeArtifactInput resolves a source-recorded artifact through its
// exact profile+target selection. It deliberately does not consult the
// path-only producer index: unrelated invocations may have written the same
// logical pathname in another physical stage.
func (b *compactKbuildRulePlanBuilder) exactObjectTreeArtifactInput(
	artifact CompactKbuildVisibleArtifact,
) (compactKbuildRuleInput, error) {
	if b == nil || b.selectionGraph == nil {
		return compactKbuildRuleInput{}, fmt.Errorf("object-tree artifact %q requires the exact Kbuild selection graph", artifact.Path)
	}
	owner, err := b.selectionGraph.compactKbuildVisibleArtifactOwner(artifact)
	if err != nil {
		return compactKbuildRuleInput{}, err
	}
	producer, ok := b.selectionGraph.materializedProducers[owner]
	if !ok {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"object-tree artifact %q exact owner %s has not been materialized",
			artifact.Path, compactKbuildSelectionKeyString(owner),
		)
	}
	node, ok := compactKbuildPlanNode(b.plan, producer)
	if !ok {
		return compactKbuildRuleInput{}, fmt.Errorf("object-tree artifact %q references absent exact producer %q", artifact.Path, producer)
	}
	if node.Stage != owner.stage {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"object-tree artifact %q exact producer %q has stage %q, want selected stage %q",
			artifact.Path, producer, node.Stage, owner.stage,
		)
	}
	if !compactKbuildPlanStageVisible(node.Stage, b.planContext().Stage) {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"object-tree artifact %q exact owner %s is not visible from %s stage",
			artifact.Path, compactKbuildSelectionKeyString(owner), b.planContext().Stage,
		)
	}
	slot := -1
	for index, output := range node.Outputs {
		if output.Path != artifact.Path {
			continue
		}
		if slot >= 0 {
			return compactKbuildRuleInput{}, fmt.Errorf("object-tree artifact %q exact producer %q publishes the path more than once", artifact.Path, producer)
		}
		slot = index
	}
	if slot < 0 {
		return compactKbuildRuleInput{}, fmt.Errorf(
			"object-tree artifact %q exact owner %s producer %q does not publish that path",
			artifact.Path, compactKbuildSelectionKeyString(owner), producer,
		)
	}
	return compactKbuildRuleInput{path: artifact.Path, producer: producer, slot: slot}, nil
}

// stageVisibleProjectionInput peels only linux.bzl's own one-file copy
// projections. Kbuild-generated nodes remain opaque: if one is scheduled
// after the consumer, an explicit Make dependency must classify it into an
// earlier stage instead of this helper guessing how to recreate it.
func (b *compactKbuildRulePlanBuilder) stageVisibleProjectionInput(
	producer string,
	slot int,
	logicalPath string,
	visited map[string]bool,
) (compactKbuildRuleInput, bool, error) {
	node, ok := compactKbuildPlanNode(b.plan, producer)
	if !ok {
		return compactKbuildRuleInput{}, false, fmt.Errorf("logical input %q references absent producer %q", logicalPath, producer)
	}
	if slot < 0 || slot >= len(node.Outputs) {
		return compactKbuildRuleInput{}, false, fmt.Errorf("logical input %q references output slot %d of producer %q with %d outputs", logicalPath, slot, producer, len(node.Outputs))
	}
	if compactKbuildPlanStageVisible(node.Stage, b.planContext().Stage) {
		return compactKbuildRuleInput{path: logicalPath, producer: producer, slot: slot}, true, nil
	}
	key := fmt.Sprintf("%s:%d", producer, slot)
	if visited[key] {
		return compactKbuildRuleInput{}, false, fmt.Errorf("logical input %q has a cyclic projection chain at %s", logicalPath, key)
	}
	visited[key] = true
	defer delete(visited, key)
	if node.Kind != "copy" || node.Tool != "actionfile" || len(node.Outputs) != 1 || slot != 0 {
		return compactKbuildRuleInput{}, false, nil
	}
	switch {
	case len(node.Sources) == 1 && len(node.Inputs) == 0:
		objectTree := false
		if err := b.plan.ensureSourceLookupIndex(); err != nil {
			return compactKbuildRuleInput{}, false, err
		}
		if source, ok := b.plan.sourcesByID[node.Sources[0].SourceID]; ok {
			objectTree = source.Namespace == "config"
		}
		return compactKbuildRuleInput{
			path: logicalPath, sourceID: node.Sources[0].SourceID, objectTree: objectTree,
		}, true, nil
	case len(node.Inputs) == 1 && len(node.Sources) == 0:
		edge := node.Inputs[0]
		return b.stageVisibleProjectionInput(edge.ProducerID, edge.Slot, logicalPath, visited)
	default:
		return compactKbuildRuleInput{}, false, nil
	}
}

type compactKbuildRuleMatch struct {
	profile CompactKbuildProfile
	rule    KbuildRule
	stem    string
	// lookupTarget is GNU Make's lexical target identity for implicit rules and
	// local target-specific variables. The action graph continues to use the
	// separately supplied canonical target path.
	lookupTarget string
	command      string
	targetOrder  int
	// capturedEnvironment is non-nil only for deferred shell queries. Those
	// actions execute in the source-ordered recipe environment which created the
	// query, not in a synthetic content-output target context.
	capturedEnvironment map[string]string
	// capturedEnvironmentUsage is the query-wide shell/helper usage projection.
	// The argv fast path uses it even when an unexported CONFIG_SHELL prevents
	// ordinary command classification from recognizing the helper directly.
	capturedEnvironmentUsage compactKbuildSourceScriptEnvironmentUsage
	// commands preserves every source-recipe command call in execution order.
	// command remains the primary command for callers that select one
	// source-defined command.
	commands []string
	// commandTemplates retains the occurrence-level command expansion selected
	// through the source recipe wrappers. The same cmd_<name> may be called more
	// than once with different GNU Make call-local arguments, so template text
	// must never be recovered later from command names alone.
	commandTemplates []CompactKbuildCommandTemplate
	// explicit is true when the rule's expanded target list names this target
	// directly. GNU Make considers explicit and static-pattern rules before its
	// implicit-rule search, even when an implicit pattern would produce an
	// equally short stem.
	explicit bool
	// GNU Make prefers the pattern rule with the shortest stem. Preserve that
	// selection before comparing profiles so an ordinary %.o fallback cannot
	// conflict with a selected transformation such as %.pi.o.
	stemLength int
	ruleOrder  int
	resolved   bool
}

type compactKbuildRecipeCommandCall struct {
	wrapper    string
	expression string
	arguments  []string
}

func (m compactKbuildRuleMatch) commandSequence() []string {
	if len(m.commandTemplates) != 0 {
		commands := make([]string, 0, len(m.commandTemplates))
		for _, template := range m.commandTemplates {
			command := template.Name
			if command == "" {
				command = compactKbuildDirectRecipeCommand
			}
			commands = append(commands, command)
		}
		return commands
	}
	if len(m.commands) != 0 {
		return append([]string(nil), m.commands...)
	}
	if m.command == "" {
		return nil
	}
	return []string{m.command}
}

func compactKbuildRuleCommandSequenceContains(match compactKbuildRuleMatch, command string) bool {
	for _, candidate := range match.commandSequence() {
		if candidate == command {
			return true
		}
	}
	return false
}

const compactKbuildDirectRecipeCommand = "__direct_recipe__"

// compactKbuildSelectedDirectRecipe projects a sole exact filechk occurrence
// recovered from inside rule_<name> back onto the specialized direct-helper
// boundary. Every other direct occurrence stays in the ordered generic command
// stream and is lowered from its evaluated Text.
func compactKbuildSelectedDirectRecipe(match compactKbuildRuleMatch) (compactKbuildRuleMatch, error) {
	if len(match.commandTemplates) != 1 || match.commandTemplates[0].Name != "" {
		return compactKbuildRuleMatch{}, fmt.Errorf(
			"target profile %q does not select one direct Kbuild source occurrence",
			match.profile.Name,
		)
	}
	recipe := strings.TrimSpace(match.commandTemplates[0].Source)
	if _, _, ok := kbuildFilechkCall(recipe); !ok {
		return compactKbuildRuleMatch{}, fmt.Errorf(
			"target profile %q direct Kbuild source occurrence %q is not an exact filechk call",
			match.profile.Name, recipe,
		)
	}
	match.rule.Recipe = []string{recipe}
	return match, nil
}

type compactKbuildRuleInput struct {
	path     string
	producer string
	slot     int
	sourceID string
	// recipeLocal marks the version emitted by an earlier physical command in
	// the recipe currently being lowered. A later command observes that version,
	// not the selected invocation's entry snapshot of the same pathname. This is
	// transient planner provenance: outputs exposed to another selection are
	// reconstructed without it.
	recipeLocal bool
	// objectTree distinguishes an immutable configured-object-tree projection
	// from an ordinary immutable source. Both use Bazel source edges, while
	// $(obj) and $(src) remain distinct namespaces during Make evaluation.
	objectTree bool
	orderOnly  bool
	// workingOnly marks an ancestor materialized solely so a source-owned
	// script sees the complete writable object-tree closure. It remains a real
	// action input, but it is not a native Make prerequisite of this rule and
	// therefore must not participate in producer-lineage queries.
	workingOnly bool
}

// sourcePathExists consults the exact physical roots captured by the selected
// Make invocation. An unbound builder has no source evidence at all. A source
// namespace assigns an action-plan input tree but is not by itself existence
// evidence for every generated-looking path below that prefix.
func (b *compactKbuildRulePlanBuilder) sourcePathExists(sourcePath string) (bool, error) {
	evidence, err := b.sourcePathEvidence(sourcePath)
	return evidence.exists, err
}

func (b *compactKbuildRulePlanBuilder) sourcePathEvidence(sourcePath string) (compactKbuildGraphSourceEvidence, error) {
	if b == nil || b.profile == nil {
		return compactKbuildGraphSourceEvidence{}, nil
	}
	return b.metadata.compactKbuildGraphSourcePathExists(*b.profile, sourcePath)
}

type compactKbuildGraphSourceEvidence struct {
	exists     bool
	objectTree bool
}

// compactKbuildGraphSourcePathExists is the single graph-discovery source
// predicate. Ordinary configured sources must exist below an exact physical
// profile root. In preconfigured mode, an exact namespaced object-tree leaf is
// also immutable source evidence; a namespace prefix alone never is.
func (m *CompactMetadata) compactKbuildGraphSourcePathExists(
	profile CompactKbuildProfile,
	sourcePath string,
) (compactKbuildGraphSourceEvidence, error) {
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if sourcePath == "" {
		return compactKbuildGraphSourceEvidence{}, nil
	}
	preconfigured, err := m.preconfiguredObjectTreeSourcePathExists(profile, sourcePath)
	if err != nil {
		return compactKbuildGraphSourceEvidence{}, err
	}
	if preconfigured {
		return compactKbuildGraphSourceEvidence{exists: true, objectTree: true}, nil
	}
	if compactKbuildProfileSourcePathExists(profile, sourcePath) {
		return compactKbuildGraphSourceEvidence{exists: true}, nil
	}
	return compactKbuildGraphSourceEvidence{}, nil
}

func (m *CompactMetadata) preconfiguredObjectTreeSourcePathExists(
	profile CompactKbuildProfile,
	sourcePath string,
) (bool, error) {
	if m == nil || !m.preconfiguredObjectTree || profile.evaluator == nil || profile.evaluator.template == nil {
		return false, nil
	}
	exact, err := m.hasExactActionPlanSourceNamespacePrefix(sourcePath)
	if err != nil || !exact {
		return false, err
	}
	root := strings.TrimSpace(profile.evaluator.template.sourceRoots["__LINUX_BZL_OBJECT_TREE__"])
	if root == "" {
		return false, nil
	}
	return fileExists(filepath.Join(root, filepath.FromSlash(sourcePath))), nil
}

func (m *CompactMetadata) compactKbuildRuleForProfile(profile CompactKbuildProfile, target string) (compactKbuildRuleMatch, bool, error) {
	return m.compactKbuildRuleForProfileMakeTarget(profile, target, target)
}

func (m *CompactMetadata) compactKbuildRuleForProfileMakeTarget(
	profile CompactKbuildProfile,
	target, makeTarget string,
) (compactKbuildRuleMatch, bool, error) {
	target = canonicalKbuildRulePath(target)
	matches := []compactKbuildRuleMatch{}
	candidates := compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget)
	explicitRecipeBarrier := false
	for _, candidate := range candidates {
		if !strings.Contains(candidate.target, "%") && len(candidate.rule.Recipe) != 0 {
			explicitRecipeBarrier = true
			break
		}
	}
	for _, candidate := range candidates {
		match := compactKbuildRuleMatch{
			profile: profile, rule: candidate.rule, stem: candidate.stem, lookupTarget: candidate.lookupTarget,
			explicit: !strings.Contains(candidate.target, "%"), stemLength: len(candidate.stem),
			ruleOrder: candidate.ruleOrder, targetOrder: candidate.targetOrder, resolved: true,
		}
		if explicitRecipeBarrier && !match.explicit {
			// An explicit recipe owns the target even when it is an ordering-only
			// control recipe or cannot otherwise be lowered as an artifact action.
			// GNU Make never expands an implicit fallback for that target, so do not
			// let an unselected recipe register probes or fail symbolic evaluation.
			continue
		}
		selections, commandFound, err := evaluatedKbuildRuleCommands(target, match)
		if err != nil {
			return compactKbuildRuleMatch{}, false, err
		}
		if commandFound {
			match.commandTemplates = selections
			match.command = selections[0].Name
			match.commands = match.commandSequence()
			matches = append(matches, match)
		}
	}
	if len(matches) == 0 {
		return compactKbuildRuleMatch{}, false, nil
	}
	// GNU Make only considers an implicit rule when each non-phony
	// prerequisite can itself be made. Captured Makefiles commonly contain
	// equal-specificity %.o-from-%.c and %.o-from-%.S rules; the selected source
	// (including a source generated by another evaluated rule) decides between
	// them.
	viable := matches[:0]
	for _, match := range matches {
		matchViable, err := m.compactKbuildRuleMatchViableInProfile(target, match, profile)
		if err != nil {
			return compactKbuildRuleMatch{}, false, err
		}
		if matchViable {
			viable = append(viable, match)
		}
	}
	if len(viable) != 0 {
		matches = viable
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].explicit != matches[j].explicit {
			return matches[i].explicit
		}
		if matches[i].stemLength != matches[j].stemLength {
			return matches[i].stemLength < matches[j].stemLength
		}
		if matches[i].ruleOrder != matches[j].ruleOrder {
			if matches[i].explicit {
				// Multiple explicit recipes override in source order; GNU Make uses
				// the last recipe while merging every ordinary prerequisite list.
				return matches[i].ruleOrder > matches[j].ruleOrder
			}
			return matches[i].ruleOrder < matches[j].ruleOrder
		}
		return false
	})
	return matches[0], true, nil
}

func (m *CompactMetadata) compactKbuildRuleMatchViableInProfile(target string, match compactKbuildRuleMatch, profile CompactKbuildProfile) (bool, error) {
	activeImplicitRules := map[int]bool{}
	if !match.explicit {
		activeImplicitRules[match.ruleOrder] = true
	}
	context, err := evaluatedKbuildTargetMakeContext(profile, target, &match, false)
	if err != nil {
		return false, nil
	}
	for _, prerequisites := range [][]compactKbuildEvaluatedPath{context.normal, context.orderOnly} {
		for _, evaluated := range prerequisites {
			candidate := canonicalKbuildRulePath(evaluated.graphPath)
			source, err := m.compactKbuildGraphSourcePathExists(profile, candidate)
			if err != nil {
				return false, err
			}
			if candidate == "" || candidate == "FORCE" || candidate == target || source.exists {
				continue
			}
			if compactKbuildRuleCommandSequenceContains(match, path.Base(candidate)) {
				continue
			}
			viable, err := m.compactKbuildTargetHasViableRuleInProfile(candidate, evaluated.makeWord, map[string]bool{target: true}, activeImplicitRules, profile)
			if err != nil {
				return false, err
			}
			if viable {
				continue
			}
			return false, nil
		}
	}
	return true, nil
}

func (m *CompactMetadata) compactKbuildTargetHasViableRuleInProfile(
	target, makeTarget string,
	targetStack map[string]bool,
	implicitRuleStack map[int]bool,
	profile CompactKbuildProfile,
) (bool, error) {
	target = canonicalKbuildRulePath(target)
	source, err := m.compactKbuildGraphSourcePathExists(profile, target)
	if err != nil {
		return false, err
	}
	if target == "" || targetStack[target] || source.exists {
		return source.exists, nil
	}
	targetStack[target] = true
	defer delete(targetStack, target)
	candidates := compactKbuildRuleCandidatesForMakeTarget(profile, target, makeTarget)
	for _, resolved := range candidates {
		if !strings.Contains(resolved.target, "%") {
			// GNU Make calls an explicitly declared target a file that ought
			// to exist while searching an outer implicit rule. Its own build
			// later starts a fresh implicit search for its prerequisites.
			return true, nil
		}
	}
	for _, resolved := range candidates {
		implicit := strings.Contains(resolved.target, "%")
		if implicit && implicitRuleStack[resolved.ruleOrder] {
			// GNU Make removes an implicit rule from consideration while it is
			// recursively checking that rule's prerequisites. Without this guard,
			// generic fallbacks such as `%: %_shipped` can manufacture an
			// unbounded `_shipped_shipped...` chain for a missing source.
			continue
		}
		match := compactKbuildRuleMatch{
			profile: profile, rule: resolved.rule, stem: resolved.stem, lookupTarget: resolved.lookupTarget,
			explicit: !implicit, stemLength: len(resolved.stem), ruleOrder: resolved.ruleOrder,
			targetOrder: resolved.targetOrder, resolved: true,
		}
		selections, commandFound, commandErr := evaluatedKbuildRuleCommands(target, match)
		if commandErr != nil || !commandFound {
			continue
		}
		match.commandTemplates = selections
		match.command = selections[0].Name
		match.commands = match.commandSequence()
		if implicit {
			implicitRuleStack[resolved.ruleOrder] = true
		}
		viable := true
		context, contextErr := evaluatedKbuildTargetMakeContext(profile, target, &match, false)
		if contextErr != nil {
			viable = false
		}
		prerequisites := append(append([]compactKbuildEvaluatedPath(nil), context.normal...), context.orderOnly...)
		for _, prerequisite := range prerequisites {
			candidate := canonicalKbuildRulePath(prerequisite.graphPath)
			source, sourceErr := m.compactKbuildGraphSourcePathExists(profile, candidate)
			if sourceErr != nil {
				return false, sourceErr
			}
			if candidate == "" || candidate == "FORCE" || candidate == target || source.exists {
				continue
			}
			if compactKbuildRuleCommandSequenceContains(match, path.Base(candidate)) {
				continue
			}
			candidateViable, candidateErr := m.compactKbuildTargetHasViableRuleInProfile(candidate, prerequisite.makeWord, targetStack, implicitRuleStack, profile)
			if candidateErr != nil {
				return false, candidateErr
			}
			if !candidateViable {
				viable = false
				break
			}
		}
		if implicit {
			delete(implicitRuleStack, resolved.ruleOrder)
		}
		if viable {
			return true, nil
		}
	}
	return false, nil
}

func (b *compactKbuildRulePlanBuilder) build(target string) (string, error) {
	return b.buildResolved(target, nil)
}

// buildSelectedTarget starts final lowering from the exact lexical Make target
// retained by selection discovery.  The action graph continues to use target's
// canonical identity; makeTarget is used only to repeat GNU Make's implicit-rule
// lookup and automatic-variable context for this root edge.
func (b *compactKbuildRulePlanBuilder) buildSelectedTarget(target, makeTarget string) (string, error) {
	target = canonicalKbuildRulePath(target)
	if b == nil || b.metadata == nil || b.profile == nil {
		return "", fmt.Errorf("selected Kbuild target %q requires an exact evaluated profile", target)
	}
	resolution, cached := b.selectionGraph.takeSelectedRootRuleResolution(
		b.metadata, b.selection, *b.profile, target, makeTarget,
	)
	match, matched, err := resolution.match, resolution.matched, resolution.err
	if !cached {
		match, matched, err = b.metadata.compactKbuildRuleForProfileMakeTarget(*b.profile, target, makeTarget)
	}
	if err != nil {
		return "", err
	}
	if !matched {
		return "", fmt.Errorf("selected target %q has no evaluated Kbuild rule in profile %q", target, b.profile.Name)
	}
	return b.buildResolved(target, &match)
}

func (b *compactKbuildRulePlanBuilder) buildResolved(
	target string,
	resolved *compactKbuildRuleMatch,
) (string, error) {
	target = canonicalKbuildRulePath(target)
	if b == nil || b.metadata == nil || b.profile == nil {
		return "", fmt.Errorf("Kbuild target %q requires an exact evaluated profile", target)
	}
	selectedTarget := b.selectionBound && canonicalKbuildRulePath(b.selection.target) == target
	if !selectedTarget {
		if producer, _, ok := b.existingProducer(target); ok {
			return producer, nil
		}
	}
	if producer := b.memo[target]; producer != "" {
		return producer, nil
	}
	if b.stack[target] {
		return "", fmt.Errorf("evaluated Kbuild rule cycle at %q", target)
	}
	b.stack[target] = true
	defer delete(b.stack, target)

	match := compactKbuildRuleMatch{}
	if resolved != nil {
		match = *resolved
		if match.profile.Name != b.profile.Name {
			return "", fmt.Errorf("target %q resolved through profile %q, want %q", target, match.profile.Name, b.profile.Name)
		}
	} else {
		var matched bool
		var err error
		match, matched, err = b.ruleForTarget(target)
		if err != nil {
			return "", err
		}
		if !matched {
			return "", fmt.Errorf("target %q has no evaluated Kbuild rule in profile %q", target, b.profile.Name)
		}
	}
	inputs, err := b.ruleInputs(target, match)
	if err != nil {
		return "", err
	}
	if b.selectionBound && target == b.selection.target {
		for _, overwrite := range b.selectionGraph.overwriteDependencies[b.selection] {
			producer, ok := b.selectionGraph.materializedProducers[overwrite.producer]
			if !ok {
				return "", fmt.Errorf(
					"Kbuild overwrite predecessor %s for %s has not been materialized",
					compactKbuildSelectionKeyString(overwrite.producer), compactKbuildSelectionKeyString(b.selection),
				)
			}
			node, ok := compactKbuildPlanNode(b.plan, producer)
			if !ok {
				return "", fmt.Errorf("Kbuild overwrite predecessor %s references absent producer %q", compactKbuildSelectionKeyString(overwrite.producer), producer)
			}
			slot := -1
			for index, output := range node.Outputs {
				if output.Path == overwrite.path {
					slot = index
					break
				}
			}
			if slot < 0 {
				return "", fmt.Errorf("Kbuild overwrite predecessor %s does not publish logical path %q", compactKbuildSelectionKeyString(overwrite.producer), overwrite.path)
			}
			alreadyBound := false
			for _, input := range inputs {
				if input.path != overwrite.path {
					continue
				}
				if input.producer != producer || input.slot != slot {
					return "", fmt.Errorf(
						"Kbuild overwrite path %q for %s is already bound to a different exact producer",
						overwrite.path, compactKbuildSelectionKeyString(b.selection),
					)
				}
				alreadyBound = true
				break
			}
			if !alreadyBound {
				inputs = append(inputs, compactKbuildRuleInput{
					path: overwrite.path, producer: producer, slot: slot, workingOnly: true,
				})
			}
		}
		inputs, err = b.compactKbuildGroupedPeerInputs(inputs)
		if err != nil {
			return "", err
		}
	}
	match, err = activeCompactKbuildRuleCommands(target, match, inputs)
	if err != nil {
		return "", err
	}
	producer, err := b.buildCommandTemplate(target, match, inputs)
	if err != nil {
		return "", err
	}
	b.memo[target] = producer
	return producer, nil
}

// compactKbuildGroupedPeerInputs binds dependencies contributed by non-trigger
// outputs of one GNU Make grouped rule. They are real execution/object-tree
// inputs of the shared physical producer, but GNU automatic variables retain
// the first-reached trigger's prerequisite context.
func (b *compactKbuildRulePlanBuilder) compactKbuildGroupedPeerInputs(
	inputs []compactKbuildRuleInput,
) ([]compactKbuildRuleInput, error) {
	if b == nil || b.selectionGraph == nil || b.metadata == nil {
		return inputs, nil
	}
	members := b.selectionGraph.compactKbuildGroupedSelectionMembers(b.selection)
	if len(members) < 2 {
		return inputs, nil
	}
	representative := b.selectionGraph.compactKbuildGroupedSelectionRepresentative(b.selection)
	if representative != b.selection {
		return nil, fmt.Errorf(
			"grouped Kbuild selection %s is lowered through non-trigger %s",
			compactKbuildSelectionKeyString(b.selection), compactKbuildSelectionKeyString(representative),
		)
	}
	appendWorkingInput := func(candidate compactKbuildRuleInput) error {
		for _, input := range inputs {
			if input.path != candidate.path {
				continue
			}
			if input.producer != candidate.producer || input.slot != candidate.slot || input.sourceID != candidate.sourceID {
				return fmt.Errorf("grouped Kbuild peer dependency path %q is bound to conflicting inputs", candidate.path)
			}
			return nil
		}
		candidate.workingOnly = true
		inputs = append(inputs, candidate)
		return nil
	}
	// GNU Make evaluates every peer's merged prerequisite declarations before
	// running the shared recipe, even though only the trigger's context contributes
	// $<, $^, and $|. Resolve those exact contexts so direct source prerequisites
	// are Bazel inputs as well as selected generated producers.
	for _, member := range members {
		if member == representative {
			continue
		}
		match, matched, matchErr := b.ruleForTarget(member.target)
		if matchErr != nil {
			return nil, matchErr
		}
		if !matched || !compactKbuildRuleHasGroupedOutputs(match.rule) {
			return nil, fmt.Errorf("grouped Kbuild peer %s has no matching grouped rule", compactKbuildSelectionKeyString(member))
		}
		peerInputs, inputErr := b.ruleInputs(member.target, match)
		if inputErr != nil {
			return nil, fmt.Errorf("resolve grouped Kbuild peer %s inputs: %w", compactKbuildSelectionKeyString(member), inputErr)
		}
		for _, peerInput := range peerInputs {
			if err := appendWorkingInput(peerInput); err != nil {
				return nil, err
			}
		}
	}
	triggerDependencies, err := b.selectionGraph.selectionDependenciesSingle(b.metadata, representative)
	if err != nil {
		return nil, err
	}
	triggerSet := make(map[compactKbuildSelectionKey]bool, len(triggerDependencies))
	for _, dependency := range triggerDependencies {
		triggerSet[b.selectionGraph.compactKbuildGroupedSelectionRepresentative(dependency)] = true
	}
	groupDependencies, err := b.selectionGraph.selectionDependencies(b.metadata, representative)
	if err != nil {
		return nil, err
	}
	for _, dependency := range groupDependencies {
		dependency = b.selectionGraph.compactKbuildGroupedSelectionRepresentative(dependency)
		if triggerSet[dependency] {
			continue
		}
		producer, materialized := b.selectionGraph.materializedProducers[dependency]
		if !materialized {
			return nil, fmt.Errorf(
				"grouped Kbuild trigger %s peer dependency %s has not been materialized",
				compactKbuildSelectionKeyString(representative), compactKbuildSelectionKeyString(dependency),
			)
		}
		node, exists := compactKbuildPlanNode(b.plan, producer)
		if !exists {
			return nil, fmt.Errorf("grouped Kbuild peer dependency %s references absent producer %q", compactKbuildSelectionKeyString(dependency), producer)
		}
		slot := -1
		for index, output := range node.Outputs {
			if output.Path == dependency.target {
				slot = index
				break
			}
		}
		if slot < 0 {
			return nil, fmt.Errorf(
				"grouped Kbuild peer dependency %s producer %q does not publish its target",
				compactKbuildSelectionKeyString(dependency), producer,
			)
		}
		alreadyBound := false
		for _, input := range inputs {
			if input.path != dependency.target {
				continue
			}
			if input.producer != producer || input.slot != slot {
				return nil, fmt.Errorf(
					"grouped Kbuild peer dependency path %q is bound to conflicting producers",
					dependency.target,
				)
			}
			alreadyBound = true
			break
		}
		if !alreadyBound {
			inputs = append(inputs, compactKbuildRuleInput{
				path: dependency.target, producer: producer, slot: slot, workingOnly: true,
			})
		}
	}
	resolvedPaths := make([]string, 0, len(b.resolvedSideOutputs))
	for inputPath := range b.resolvedSideOutputs {
		resolvedPaths = append(resolvedPaths, inputPath)
	}
	sort.Strings(resolvedPaths)
	for _, inputPath := range resolvedPaths {
		resolved := b.resolvedSideOutputs[inputPath]
		alreadyBound := false
		for _, input := range inputs {
			if input.path != inputPath {
				continue
			}
			if input.producer != resolved.producer || input.slot != resolved.slot {
				return nil, fmt.Errorf("grouped Kbuild side-output path %q is bound to conflicting producers", inputPath)
			}
			alreadyBound = true
			break
		}
		if !alreadyBound {
			resolved.path = inputPath
			resolved.workingOnly = true
			inputs = append(inputs, resolved)
		}
	}
	return inputs, nil
}

// activeCompactKbuildRuleCommands removes source-declared optional command
// calls whose cmd_<name> template evaluates empty in this exact target
// context. Kbuild uses this for conditional follow-up work such as checksrc;
// the remaining calls retain their source execution order for generic
// lowering.
func activeCompactKbuildRuleCommands(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (compactKbuildRuleMatch, error) {
	commands := match.commandSequence()
	if len(commands) == 1 && commands[0] == compactKbuildDirectRecipeCommand &&
		(len(match.commandTemplates) == 0 ||
			len(match.commandTemplates) == 1 && match.commandTemplates[0].Name == compactKbuildDirectRecipeCommand) {
		return match, nil
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
	if err != nil {
		return compactKbuildRuleMatch{}, err
	}
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return compactKbuildRuleMatch{}, err
	}
	selections, err := evaluatedKbuildRuleCommandSelectionsForMakeTarget(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected, match.rule.Recipe, true,
	)
	if err != nil {
		return compactKbuildRuleMatch{}, err
	}
	if len(selections) == 0 {
		names := make([]string, 0, len(commands))
		for _, command := range commands {
			names = append(names, "cmd_"+command)
		}
		return compactKbuildRuleMatch{}, fmt.Errorf(
			"target %q profile %q selected only empty Kbuild command templates %s",
			target, match.profile.Name, strings.Join(names, ","),
		)
	}
	match.commandTemplates = selections
	match.command = selections[0].Name
	match.commands = match.commandSequence()
	return match, nil
}

func (b *compactKbuildRulePlanBuilder) ruleForTarget(target string) (compactKbuildRuleMatch, bool, error) {
	if b == nil || b.metadata == nil || b.profile == nil {
		return compactKbuildRuleMatch{}, false, fmt.Errorf(
			"Kbuild rule resolution for %q requires an exact evaluated profile",
			target,
		)
	}
	return b.metadata.compactKbuildRuleForProfile(*b.profile, target)
}

func (b *compactKbuildRulePlanBuilder) ruleForEvaluatedTarget(
	target compactKbuildEvaluatedPath,
) (compactKbuildRuleMatch, bool, error) {
	if b == nil || b.metadata == nil || b.profile == nil {
		return compactKbuildRuleMatch{}, false, fmt.Errorf(
			"Kbuild evaluated rule resolution for %q requires an exact evaluated profile",
			target.graphPath,
		)
	}
	return b.metadata.compactKbuildRuleForProfileMakeTarget(*b.profile, target.graphPath, target.makeWord)
}

func (b *compactKbuildRulePlanBuilder) ruleInputs(target string, match compactKbuildRuleMatch) ([]compactKbuildRuleInput, error) {
	out := []compactKbuildRuleInput{}
	seen := map[struct {
		path      string
		orderOnly bool
	}]bool{}
	appendPrerequisites := func(prerequisites []compactKbuildEvaluatedPath, orderOnly bool) error {
		for _, evaluated := range prerequisites {
			// The selected target context has already expanded the rule stem and
			// resolved raw Make paths against the invocation. These are canonical
			// graph identities, so applying cwd scope here a second time would
			// corrupt explicitly source/object-rooted prerequisites.
			prerequisite := compactKbuildGraphTargetPath(evaluated.graphPath)
			if prerequisite == "" || prerequisite == "FORCE" {
				continue
			}
			// A rooted prerequisite can share the selected output's graph path
			// without naming the same file. External modpost writes its local
			// Module.symvers while reading $(objtree)/Module.symvers from the
			// configured kernel SDK. Preserve that immutable cross-tree input;
			// only an unrooted same-path prerequisite is a true local self-edge.
			if prerequisite == target && compactKbuildEvaluatedPathNamespace(evaluated.makeWord) == "" {
				continue
			}
			// A command-named prerequisite is the host program selected by Kbuild.
			// The map-directory tool binding supplies the same typed program role.
			if path.Base(prerequisite) == match.command {
				continue
			}
			key := struct {
				path      string
				orderOnly bool
			}{path: prerequisite, orderOnly: orderOnly}
			if seen[key] {
				continue
			}
			seen[key] = true
			// A selected plan producer is exact execution provenance and wins
			// over a same-path file in a preconfigured object tree. External
			// module prep overlays deliberately replace SDK leaves this way.
			if input, ok, err := b.existingInput(prerequisite); err != nil {
				return err
			} else if ok {
				input.orderOnly = orderOnly
				out = append(out, input)
				continue
			}
			// Kconfig replay publishes its resolved files only after selected
			// Kbuild actions have been lowered. A selected action can nevertheless
			// name one of those existing object-tree files as a native Make
			// prerequisite. Bind that exact logical path to the already-interned
			// immutable config source until a selected writer has materialized.
			if input, ok, err := b.compactKbuildConfigProjectionBaselineInput(prerequisite); err != nil {
				return err
			} else if ok {
				input.orderOnly = orderOnly
				out = append(out, input)
				continue
			}
			if evidence, err := b.sourcePathEvidence(prerequisite); err != nil {
				return err
			} else if evidence.exists {
				sourceID, err := b.metadata.ensureActionPlanSource(b.plan, prerequisite)
				if err != nil {
					return err
				}
				out = append(out, compactKbuildRuleInput{
					path: prerequisite, sourceID: sourceID, objectTree: evidence.objectTree, orderOnly: orderOnly,
				})
				continue
			}
			dependency, matched, matchErr := b.ruleForEvaluatedTarget(evaluated)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				setupOnly, setupErr := b.metadata.compactKbuildTargetIsOrderingOnlyInProfile(*b.profile, prerequisite)
				if setupErr != nil {
					return setupErr
				}
				if setupOnly {
					// Preserve the evaluated ordering edge without manufacturing
					// a file producer for a source-declared control target.
					continue
				}
			}
			dependencyViable := dependency.explicit
			if matched && !dependencyViable {
				dependencyViable, matchErr = b.metadata.compactKbuildRuleMatchViableInProfile(prerequisite, dependency, *b.profile)
				if matchErr != nil {
					return matchErr
				}
			}
			if !matched || !dependencyViable {
				return fmt.Errorf(
					"target %q prerequisite %q has no source evidence, viable evaluated rule, or existing producer in profile %q",
					target, prerequisite, b.profile.Name,
				)
			}
			producer, err := b.buildResolved(prerequisite, &dependency)
			if err != nil {
				return fmt.Errorf("target %q prerequisite %q: %w", target, prerequisite, err)
			}
			_, slot, ok := b.existingProducer(prerequisite)
			if !ok {
				return fmt.Errorf("target %q prerequisite %q was built without a declared output", target, prerequisite)
			}
			out = append(out, compactKbuildRuleInput{path: prerequisite, producer: producer, slot: slot, orderOnly: orderOnly})
		}
		return nil
	}
	context, err := evaluatedKbuildSelectedTargetMakeContext(match.profile, target, &match)
	if err != nil {
		return nil, err
	}
	if err := appendPrerequisites(context.normal, false); err != nil {
		return nil, err
	}
	if err := appendPrerequisites(context.orderOnly, true); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *CompactMetadata) compactKbuildTargetIsOrderingOnlyInProfile(
	profile CompactKbuildProfile,
	target string,
) (bool, error) {
	target = canonicalKbuildRulePath(target)
	if _, matched, err := m.compactKbuildRuleForProfile(profile, target); err != nil {
		return false, err
	} else if matched {
		return false, nil
	}
	candidates := compactKbuildRuleCandidates(profile, target)
	for _, candidate := range candidates {
		match := compactKbuildRuleMatch{
			profile: profile, rule: candidate.rule, stem: candidate.stem, lookupTarget: candidate.lookupTarget,
			explicit: !strings.Contains(candidate.target, "%"), stemLength: len(candidate.stem),
			ruleOrder: candidate.ruleOrder, targetOrder: candidate.targetOrder, resolved: true,
		}
		if len(kbuildRecipeCommandExpressions(match.rule.Recipe)) != 0 {
			// Executable command templates were already admitted by
			// compactKbuildRuleForProfile above. A remaining source candidate has
			// a defined empty/control-only template and is an ordering boundary;
			// do not reinterpret its $(call ...) wrapper as a direct shell recipe.
			continue
		}
		action, _, err := evaluatedKbuildDirectRecipeEffects(target, match)
		if err != nil {
			return false, err
		}
		if action {
			return false, nil
		}
	}
	// A source-declared target with no evaluated artifact-producing action is
	// an ordering boundary. This includes prerequisite-only targets, mkdir-only
	// setup, and diagnostic recipes such as a quiet-status printf. Its exact
	// prerequisite closure remains selected, but no nonexistent file is
	// manufactured for the control target itself.
	return len(candidates) != 0, nil
}

func containsUnmodeledKbuildDollar(value string) bool {
	value = withoutActionPlanTreePlaceholders(value)
	for offset := 0; ; {
		index := strings.IndexByte(value[offset:], '$')
		if index < 0 {
			return false
		}
		index += offset
		if index+1 >= len(value) {
			// A dollar that is not followed by a shell parameter name or
			// expansion operator is literal. This is common in quoted regular
			// expressions, for example sed's end-of-line anchor.
			return false
		}
		next := value[index+1]
		if next == '{' || next == '(' || next == '$' || next == '\'' || next == '"' ||
			(next >= '0' && next <= '9') ||
			(next >= 'A' && next <= 'Z') ||
			(next >= 'a' && next <= 'z') || next == '_' ||
			strings.ContainsRune("@*#?!-", rune(next)) {
			return true
		}
		offset = index + 1
	}
}

// compactKbuildRecipeCommand is one argv invocation from an evaluated Kbuild
// command variable. connector records the shell operator following the
// invocation. The planner lowers that structure into ordinary dependency
// edges, stdin/stdout bindings, and private working paths; no shell reaches an
// action.
type compactKbuildRecipeCommand struct {
	environment map[string]string
	program     string
	// programStart/programEnd identify the exact source-text command-head token
	// for compound recipes. Parsed linear commands leave them zero.
	programStart int
	programEnd   int
	arguments    []string
	// argumentTokens retain the exact source-text extent of each argv word for
	// compound recipes. They let output analysis project compiler-created paths
	// onto the private working tree without reconstructing or reformatting the
	// surrounding source-owned shell program. Parsed linear commands leave this
	// field nil.
	argumentTokens []compactKbuildRecipeToken
	// programToolRole is selected from the scoped token carried by Make from
	// source parse time. This disambiguates toolchains that intentionally use
	// one executable for several actions without comparing executable paths.
	programToolRole string
	stdin           string
	stdout          string
	// stdoutRooted records that stdout's source token carried an explicit
	// planner-owned object-tree namespace before path validation canonicalized
	// it to a graph-relative path.
	stdoutRooted bool
	connector    string
	// outputAlias is the logical result path after folding a validated
	// `mv temporary $@` command into the action that creates temporary.
	outputAlias string
}

type compactKbuildRecipeToken struct {
	value    string
	operator bool
	start    int
	end      int
}

const (
	compactKbuildLiteralDollarToken = "\x01linux-bzl-literal-dollar\x02"
	// Action-only canonicalization markers carry physical-root provenance
	// without colliding with source-owned bytes which happen to spell one of the
	// public Linux.bzl tree sentinels. They are consumed before the shell runs.
	compactKbuildActionSourceTreeMarker    = "\x01linux-bzl-action-source-tree\x02"
	compactKbuildActionObjectTreeMarker    = "\x01linux-bzl-action-object-tree\x02"
	compactKbuildActionHostDepsTreeMarker  = "\x01linux-bzl-action-host-deps-tree\x02"
	compactKbuildLiteralTreeEscapeByte     = "\x03"
	compactKbuildLiteralSentinelEscapeByte = "\x04"
)

// buildCommandTemplate lowers the evaluated cmd_<name> recipe without
// interpreting command names. A source pipeline remains one compound action
// with a live byte stream and one typed Make cwd; linear statement sequences
// become ordinary dependency edges, redirects become output/input bindings,
// grouped rules can publish multiple files, and a program produced by Kbuild
// becomes a generated executable input.
func (b *compactKbuildRulePlanBuilder) buildCommandTemplate(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (string, error) {
	commandNames := match.commandSequence()
	if len(commandNames) == 0 {
		return "", fmt.Errorf("target %q has no selected Kbuild command", target)
	}
	if len(commandNames) == 1 && commandNames[0] == compactKbuildDirectRecipeCommand {
		if len(match.commandTemplates) == 0 ||
			len(match.commandTemplates) == 1 && match.commandTemplates[0].Name == compactKbuildDirectRecipeCommand {
			return b.buildDirectRecipe(target, match, inputs)
		}
		if len(match.commandTemplates) == 1 {
			if _, _, filechk := kbuildFilechkCall(match.commandTemplates[0].Source); filechk {
				directMatch, err := compactKbuildSelectedDirectRecipe(match)
				if err != nil {
					return "", fmt.Errorf("target %q: %w", target, err)
				}
				return b.buildDirectRecipe(target, directMatch, inputs)
			}
		}
	}
	// Keep action-plan placeholders out of GNU Make evaluation. Their ${...}
	// spelling is deliberately meaningful only to the action-plan renderer;
	// exposing it to the Make evaluator makes nested source expressions such as
	// $(addprefix -I,$(src) $(obj)) look unresolved. The stable invocation-tree
	// markers are ordinary Make text and are translated after evaluation.
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
	if err != nil {
		return "", err
	}
	injected = compactKbuildActionTreeInjections(injected)
	automaticContext, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return "", err
	}
	selections := append([]CompactKbuildCommandTemplate(nil), match.commandTemplates...)
	if match.resolved && len(kbuildRecipeCommandExpressions(match.rule.Recipe)) != 0 {
		selections, err = evaluatedKbuildRuleCommandSelectionsForMakeTarget(
			match.profile, target, match.lookupTarget, automaticContext.target, automaticContext.stem,
			automaticContext.normal, automaticContext.order, injected, match.rule.Recipe, true,
		)
		if err != nil {
			return "", err
		}
		if len(selections) == 0 {
			return "", fmt.Errorf("target %q profile %q selected no active Kbuild command templates", target, match.profile.Name)
		}
		match.commandTemplates = selections
		match.command = selections[0].Name
		match.commands = match.commandSequence()
		commandNames = match.commandSequence()
	}
	templateNames := make([]string, 0, len(commandNames))
	if len(selections) != 0 {
		for index, selection := range selections {
			if selection.Name == "" {
				templateNames = append(templateNames, fmt.Sprintf("direct[%d]", index))
			} else {
				templateNames = append(templateNames, "cmd_"+selection.Name)
			}
		}
	} else {
		for _, command := range commandNames {
			templateNames = append(templateNames, "cmd_"+command)
		}
	}
	variableNames := []string{"target-stem", "CONFIG_SHELL"}
	if len(selections) == 0 {
		// Synthetic lowering fixtures which do not retain a source rule still use
		// the historical direct cmd_<name> boundary.
		variableNames = append(templateNames, variableNames...)
	}
	values, err := evaluateCompactKbuildRuleVariablesRooted(target, match, inputs, injected, variableNames...)
	if err != nil {
		return "", err
	}
	templates := make([]string, 0, len(templateNames))
	rootedTemplates := make([]string, 0, len(templateNames))
	for index, templateName := range templateNames {
		template := ""
		if len(selections) != 0 {
			template = strings.TrimSpace(selections[index].Text)
		} else {
			template = strings.TrimSpace(values[templateName])
		}
		if template == "" {
			return "", fmt.Errorf(
				"target %q selected Kbuild occurrence %s from profile %q, but its evaluated text is empty",
				target, templateName, match.profile.Name,
			)
		}
		// A selected cmd_<name> value is still a GNU Make recipe line. Prefixes
		// such as $(Q) -> '@' are expanded into that value, but Make removes them
		// before invoking the shell. Apply the same generic per-line lowering at
		// this final source-to-action boundary so compound script fallbacks never
		// try to execute a literal "@program".
		rooted, err := compactKbuildRootedActionDirectRecipeText(match.profile, template)
		if err != nil {
			return "", fmt.Errorf("target %q protect literal action marker: %w", target, err)
		}
		rootedTemplates = append(rootedTemplates, rooted)
		templates = append(templates, compactKbuildFinalizeRootedActionRecipeText(rooted))
	}
	// Preserve the exact rooted Make result for compound execution.  The argv
	// lowerer below projects automatic-variable paths onto graph identities, but
	// words produced by Make text functions are not necessarily path operands.
	// In particular, cmd_ar_builtin deliberately turns object paths into basename
	// data before piping them to xargs.  One source-owned pipeline must therefore
	// retain the evaluated shell text and one shared object-tree cwd.
	for index := range templates {
		templates[index], err = rewriteCompactKbuildEvaluatedAutomaticTreePaths(match.profile, target, match, inputs, templates[index])
		if err != nil {
			return "", fmt.Errorf("target %q automatic tree paths: %w", target, err)
		}
	}
	template := strings.Join(templates, "\n")
	rootedTemplate := strings.Join(rootedTemplates, "\n")
	// Side-effect ownership needs the rooted evaluation, not the public argv
	// projection below. The latter intentionally removes automatic-variable tree
	// markers for direct command binding; doing that before classifying a .cmd
	// redirection makes an already object-rooted path look invocation-relative
	// in split source/object builds.
	sideEffectCommands := compactKbuildRecipeSideEffectProjection(rootedTemplates, automaticContext)
	if compactKbuildHasDeferredShellSingleWord(match.profile, template) {
		compoundTemplate := compactKbuildRecipeLineShells(rootedTemplates)
		commands, discoveryErr := compactKbuildCompoundProgramCommands(compoundTemplate)
		if discoveryErr != nil {
			return "", fmt.Errorf(
				"target %q profile %q variables %s deferred-content program discovery: %w",
				target, match.profile.Name, strings.Join(templateNames, ","), discoveryErr,
			)
		}
		producer, compoundErr := b.buildHermeticKbuildCompoundWithSideEffects(
			target, match, inputs, compoundTemplate, commands, sideEffectCommands,
		)
		if compoundErr != nil {
			return "", fmt.Errorf(
				"target %q profile %q variables %s deferred-content compound: %w",
				target, match.profile.Name, strings.Join(templateNames, ","), compoundErr,
			)
		}
		return producer, nil
	}
	if compactKbuildRecipeTextHasShellGroup(rootedTemplate) {
		compoundTemplate := compactKbuildRecipeLineShells(rootedTemplates)
		commands, discoveryErr := compactKbuildCompoundProgramCommands(compoundTemplate)
		if discoveryErr != nil {
			return "", fmt.Errorf(
				"target %q profile %q variables %s shell-group program discovery: %w",
				target, match.profile.Name, strings.Join(templateNames, ","), discoveryErr,
			)
		}
		producer, compoundErr := b.buildHermeticKbuildCompoundWithSideEffects(
			target, match, inputs, compoundTemplate, commands, sideEffectCommands,
		)
		if compoundErr != nil {
			return "", fmt.Errorf(
				"target %q profile %q variables %s shell-group compound: %w",
				target, match.profile.Name, strings.Join(templateNames, ","), compoundErr,
			)
		}
		return producer, nil
	}
	commands := []compactKbuildRecipeCommand{}
	var commandErr error
	for index := range templates {
		parsed, parseErr := parseCompactKbuildRecipe(templates[index], automaticContext)
		if parseErr != nil {
			commandErr = fmt.Errorf("%s: %w", templateNames[index], parseErr)
			break
		}
		commands = append(commands, parsed...)
	}
	if commandErr != nil {
		fallbackTemplate := compactKbuildRecipeLineShells(rootedTemplates)
		compound := []compactKbuildRecipeCommand(nil)
		discovered, discoveryErr := compactKbuildCompoundProgramCommands(fallbackTemplate)
		archiveCompound, archiveSource, archiveOpaque := false, false, false
		if discoveryErr == nil {
			archiveCompound, archiveSource, archiveOpaque, err = b.compactKbuildPathArchiveExecution(target, match, inputs, match.profile, values, discovered)
			if err != nil {
				return "", err
			}
		}
		needsCompound := compactKbuildRecipeTextHasPipeline(rootedTemplate) || archiveCompound || archiveOpaque
		if needsCompound {
			if discoveryErr != nil {
				return "", fmt.Errorf(
					"target %q profile %q variables %s evaluated to %q: argv parse: %v; compound program discovery: %w",
					target, match.profile.Name, strings.Join(templateNames, ","), template, commandErr, discoveryErr,
				)
			}
			compound = discovered
		} else if archiveSource {
			return "", fmt.Errorf(
				"target %q profile %q path-sensitive source archive wrapper cannot be safely lowered after argv parse failure: %w",
				target, match.profile.Name, commandErr,
			)
		}
		producer, fallbackErr := b.buildHermeticKbuildScriptContext(
			target, match, inputs, fallbackTemplate, compound, sideEffectCommands,
		)
		if fallbackErr != nil {
			return "", fmt.Errorf(
				"target %q profile %q variables %s evaluated to %q: argv parse: %v; hermetic script: %w",
				target, match.profile.Name, strings.Join(templateNames, ","), template, commandErr, fallbackErr,
			)
		}
		return producer, nil
	}
	if len(commands) == 0 {
		return "", fmt.Errorf("target %q profile %q variables %s expand to no command", target, match.profile.Name, strings.Join(templateNames, ","))
	}
	archiveCompound, _, _, err := b.compactKbuildPathArchiveExecution(target, match, inputs, match.profile, values, commands)
	if err != nil {
		return "", err
	}
	needsCompound := compactKbuildRecipeHasPipeline(commands) || archiveCompound ||
		compactKbuildContainsProtectedLiteralActionMarker(rootedTemplate)
	if !needsCompound {
		atomicRecipe, atomicErr := compactKbuildRecipeRequiresAtomicExecution(target, match, commands)
		if atomicErr != nil {
			return "", atomicErr
		}
		needsCompound = atomicRecipe
	}
	if needsCompound {
		producer, err := b.buildHermeticKbuildCompoundWithSideEffects(
			target, match, inputs, compactKbuildRecipeLineShells(rootedTemplates), commands, sideEffectCommands,
		)
		if err != nil {
			return "", fmt.Errorf("target %q profile %q variables %s atomic compound: %w", target, match.profile.Name, strings.Join(templateNames, ","), err)
		}
		return producer, nil
	}
	producer, err := b.appendCompactKbuildRecipe(target, match, inputs, values, commands)
	if err != nil {
		return "", fmt.Errorf("target %q profile %q variables %s: %w", target, match.profile.Name, strings.Join(templateNames, ","), err)
	}
	return producer, nil
}

func compactKbuildRecipeHasPipeline(commands []compactKbuildRecipeCommand) bool {
	for _, command := range commands {
		if command.connector == "|" {
			return true
		}
	}
	return false
}

// compactKbuildRecipeTextHasShellGroup recognizes an unquoted, standalone
// POSIX brace-group delimiter. The lexer intentionally leaves braces inside
// words alone so action-plan placeholders and quoted payload remain ordinary
// argv data. A group-level redirection belongs to the complete source command
// and therefore cannot be distributed across linear per-command actions.
func compactKbuildRecipeTextHasShellGroup(value string) bool {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return false
	}
	for _, token := range tokens {
		if token.operator || token.value != "{" && token.value != "}" ||
			token.start < 0 || token.end > len(value) {
			continue
		}
		if value[token.start:token.end] == token.value {
			return true
		}
	}
	return false
}

// compactKbuildRecipeRequiresAtomicExecution reports whether a multi-command
// recipe has a filesystem boundary which cannot be represented by independent
// Bazel actions. Before the rule target exists, an outputless command has no
// artifact edge on which split lowering could order it and may carry shell or
// hidden filesystem state into the target-producing command. Likewise, a side
// output written after the rule target can depend on undeclared state created by
// that command. Preserve either shape as the exact evaluated Make sequence in
// one writable cwd. A trailing outputless command remains independently
// lowerable as an in-place validation or mutation of the established target.
//
// This decision is intentionally made from the evaluated command structure,
// not from compiler families or tool-specific flag inventories.
func compactKbuildRecipeRequiresAtomicExecution(
	target string,
	match compactKbuildRuleMatch,
	commands []compactKbuildRecipeCommand,
) (bool, error) {
	if len(commands) < 2 {
		return false, nil
	}
	normalized, err := normalizeCompactKbuildRecipeCommands(target, commands)
	if err != nil {
		return false, err
	}
	// Folding a temporary-to-target move proves that the temporary is an
	// implementation detail of the target-producing action, not a trailing
	// side output which requires a shared cwd.
	commands = normalized
	ruleOutputs := compactKbuildRecipeRuleOutputs(target, match)
	ruleOutputSet := map[string]bool{}
	for _, output := range ruleOutputs {
		ruleOutputSet[output] = true
	}
	targetProduced := false
	for commandIndex, command := range commands {
		command, err := rewriteCompactKbuildCommandAutomaticPaths(match.profile, target, match, command)
		if err != nil {
			return false, fmt.Errorf("command %d automatic paths: %w", commandIndex, err)
		}
		output, declared, err := compactKbuildRecipeExplicitOutputForProfile(match.profile, command, ruleOutputs...)
		if err != nil {
			return false, fmt.Errorf("command %d compiler output: %w", commandIndex, err)
		}
		if !declared {
			for ruleOutput := range ruleOutputSet {
				if positional, ok := compactKbuildRecipePositionalOutput(command, ruleOutput); ok {
					output, declared = positional, true
					break
				}
			}
		}
		if !declared {
			// Once the target has a producer, split lowering can model a
			// trailing outputless command as an in-place validation or mutation
			// of that target. Before then there is no artifact edge on which to
			// order the command, so the source sequence must remain atomic.
			if !targetProduced {
				return true, nil
			}
			continue
		}
		if command.outputAlias != "" {
			output = command.outputAlias
		}
		output = canonicalKbuildRulePath(output)
		if ruleOutputSet[output] {
			targetProduced = true
			continue
		}
		if targetProduced {
			return true, nil
		}
	}
	return false, nil
}

// Thin archives and the P (preserve full paths) modifier retain member path
// spellings. Run those invocations from the typed Make cwd so an action-private
// input path can never be serialized into the archive. Ordinary direct ar
// invocations copy inputs under basename member names and do not need the
// compound tree.
//
// A configured ar can also be passed to another executable (for example
// `xargs $(AR)`) or hidden behind a declared source/generated wrapper. Those
// programs are opaque to argv lowering: even when no archive modifier is
// visible after the configured role, the wrapper can add one internally. Fail
// closed by selecting the typed cwd whenever ar is an indirect capability or
// the executable is a declared source/generated path whose behavior is opaque
// at argv lowering. Source inspection later exempts immutable helpers that do
// not carry the ar capability. This is deliberately based on executable
// provenance and argv, not on a catalogue of wrapper or target names.
func compactKbuildRecipeUsesConfiguredPathArchive(commands []compactKbuildRecipeCommand) bool {
	for _, command := range commands {
		ref, configured := parseKbuildActionRoleToken(command.program)
		directArchive := configured && ref.Role == "ar"
		if !directArchive && compactKbuildCommandCarriesActionRole(command, "ar") {
			return true
		}
		if directArchive && compactKbuildCommandUsesPathArchiveMode(command.arguments) {
			return true
		}
		if !configured && path.Base(command.program) != command.program {
			if _, runtimeApplet := compactKbuildAbsoluteRuntimeApplet(command.program); !runtimeApplet {
				return true
			}
		}
	}
	return false
}

// compactKbuildRecipeRetainsArchiveMemberPaths is narrower than the typed-cwd
// predicate above. GNU ar's P mode makes archive member names path-sensitive,
// but only T/--thin leaves the member payload outside the archive. Direct ar
// invocations therefore require explicit thin-mode evidence. Indirect ar
// capabilities remain opaque and fail closed; generated wrappers without an ar
// capability must at least expose an archive operation cluster containing T.
func compactKbuildRecipeRetainsArchiveMemberPaths(commands []compactKbuildRecipeCommand) bool {
	for _, command := range commands {
		ref, configured := parseKbuildActionRoleToken(command.program)
		directArchive := configured && ref.Role == "ar"
		if directArchive && compactKbuildCommandUsesThinArchiveMode(command.arguments) {
			return true
		}
		if !directArchive && compactKbuildCommandCarriesActionRole(command, "ar") {
			return true
		}
		if !configured && path.Base(command.program) != command.program && compactKbuildCommandSuggestsThinArchiveMode(command.arguments) {
			return true
		}
	}
	return false
}

// compactKbuildPathArchiveExecution separates declared source scripts from
// other path-sensitive archive commands. Source scripts retain their inspected
// environment and replay contract in appendCompactKbuildRecipe; generated
// programs and nested runtime tools instead execute as one exact compound
// script. Both paths mirror the typed Make cwd inside the same private writable
// action tree.
func (b *compactKbuildRulePlanBuilder) compactKbuildPathArchiveExecution(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	profile CompactKbuildProfile,
	values map[string]string,
	commands []compactKbuildRecipeCommand,
) (compound, sourceScript, opaqueProgram bool, err error) {
	for index, command := range commands {
		if !compactKbuildRecipeUsesConfiguredPathArchive([]compactKbuildRecipeCommand{command}) {
			continue
		}
		_, configuredProgram := parseKbuildActionRoleToken(command.program)
		_, runtimeApplet := compactKbuildAbsoluteRuntimeApplet(command.program)
		opaque := !configuredProgram && !runtimeApplet && path.Base(command.program) != command.program
		invocation, source, sourceErr := compactKbuildSourceScriptCommand(
			profile, command, values, b.actionScope(), b.metadata.actionRoles,
		)
		if sourceErr != nil {
			return false, false, false, fmt.Errorf("path-sensitive archive command %d source wrapper: %w", index, sourceErr)
		}
		if source {
			_, environmentRoles, environmentErr := compactKbuildSourceScriptEnvironment(
				target, match, inputs, invocation.environment, invocation.environmentUsage,
				b.actionScope(), b.metadata.actionRoles,
			)
			if environmentErr != nil {
				return false, false, false, fmt.Errorf("path-sensitive archive command %d source wrapper environment: %w", index, environmentErr)
			}
			if slices.Contains(invocation.toolRoles, "ar") || slices.Contains(environmentRoles, "ar") {
				sourceScript = true
			}
		} else if opaque {
			opaqueProgram = true
		} else {
			compound = true
		}
	}
	return compound, sourceScript, opaqueProgram, nil
}

func compactKbuildCommandCarriesActionRole(command compactKbuildRecipeCommand, role string) bool {
	fields := append([]string(nil), command.arguments...)
	for _, name := range sortedStringMapKeys(command.environment) {
		fields = append(fields, command.environment[name])
	}
	for _, field := range fields {
		refs, err := KbuildActionRoleRefs(field)
		if err != nil {
			// The ordinary role annotator reports malformed source tokens with
			// context. Detection must not reinterpret them as executable identity.
			continue
		}
		for _, ref := range refs {
			if ref.Role == role {
				return true
			}
		}
	}
	return false
}

func compactKbuildCommandCarriesAnyActionRole(command compactKbuildRecipeCommand) bool {
	fields := append([]string(nil), command.arguments...)
	for _, name := range sortedStringMapKeys(command.environment) {
		fields = append(fields, command.environment[name])
	}
	for _, field := range fields {
		refs, err := KbuildActionRoleRefs(field)
		if err == nil && len(refs) != 0 {
			return true
		}
	}
	return false
}

func compactKbuildCommandUsesPathArchiveMode(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--thin" || argument == "--full-paths" {
			return true
		}
		operation := strings.TrimPrefix(argument, "-")
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") && strings.ContainsAny(operation, "PT") {
			return true
		}
		if compactKbuildArchiveOperationCluster(operation) && strings.ContainsAny(operation, "PT") {
			return true
		}
	}
	return false
}

func compactKbuildCommandUsesThinArchiveMode(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--thin" {
			return true
		}
		operation := strings.TrimPrefix(argument, "-")
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") && strings.ContainsRune(operation, 'T') {
			return true
		}
		if compactKbuildArchiveOperationCluster(operation) && strings.ContainsRune(operation, 'T') {
			return true
		}
	}
	return false
}

func compactKbuildCommandSuggestsThinArchiveMode(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--thin" {
			return true
		}
		operation := strings.TrimPrefix(argument, "-")
		if compactKbuildArchiveOperationCluster(operation) && strings.ContainsRune(operation, 'T') {
			return true
		}
	}
	return false
}

func compactKbuildArchiveOperationCluster(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	const operations = "dmpqrstxM"
	const modifiers = operations + "abcDfilNnoOPSTuUvV"
	hasOperation := false
	for _, character := range value {
		if !strings.ContainsRune(modifiers, character) {
			return false
		}
		hasOperation = hasOperation || strings.ContainsRune(operations, character)
	}
	return hasOperation
}

// compactKbuildTypedPrivateExecution projects Make's typed process cwd into an
// action's private writable tree. The source/object distinction describes the
// real Make process and remains important for resolving source operands, but it
// does not select the backing tree used by an ActionRecipe: both forms execute
// below the recipe's private WorkingDirectory. This is how tools builds can
// faithfully preserve `make -C $(srctree)/... O=$(objtree)/...` without either
// relabeling Make's cwd or writing the immutable source checkout.
func compactKbuildTypedPrivateExecution(profile CompactKbuildProfile) (directory, objectRoot string, err error) {
	location, ok := CompactKbuildProfileInvocationLocation(profile)
	if !ok {
		return "", "", fmt.Errorf("typed Kbuild execution requires an invocation location")
	}
	switch location.Tree {
	case CompactKbuildInvocationSourceTree, CompactKbuildInvocationObjectTree:
	default:
		return "", "", fmt.Errorf("typed Kbuild execution has invalid invocation tree %q", location.Tree)
	}
	directory = canonicalKbuildRulePath(location.Directory)
	if location.Directory != "" && directory == "" {
		return "", "", fmt.Errorf("typed Kbuild execution has invalid invocation directory %q", location.Directory)
	}
	if directory != "" {
		if err := validatePlanRelativePath("typed Kbuild invocation directory", directory); err != nil {
			return "", "", err
		}
	}
	from := directory
	if from == "" {
		from = "."
	}
	root, err := filepath.Rel(filepath.FromSlash(from), ".")
	if err != nil {
		return "", "", fmt.Errorf("project object root from invocation directory %q: %w", directory, err)
	}
	// filepath.Rel historically returned either "../.." or "../../." for a
	// relative target spelled as ".", depending on the Go SDK.  Action recipes
	// are content addressed, so canonicalize that equivalent spelling before it
	// becomes command data.
	objectRoot = path.Clean(filepath.ToSlash(root))
	if objectRoot == "" {
		objectRoot = "."
	}
	return directory, objectRoot, nil
}

// compactKbuildCommandUsesExactResponseFile recognizes a response-file argv
// operand only when it resolves to an exact declared input. Response-file
// contents are tool-owned and may retain paths relative to the Make process
// which created them, so their consumer must preserve that invocation cwd.
func compactKbuildCommandUsesExactResponseFile(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	inputs []compactKbuildRuleInput,
) bool {
	hasInput := func(pathname string) bool {
		pathname = canonicalKbuildRulePath(pathname)
		if pathname == "" {
			return false
		}
		for _, input := range inputs {
			if canonicalKbuildRulePath(input.path) == pathname {
				return true
			}
		}
		return false
	}
	for _, argument := range command.arguments {
		if len(argument) < 2 || argument[0] != '@' {
			continue
		}
		candidate := argument[1:]
		if strings.TrimSpace(candidate) != candidate || strings.ContainsAny(candidate, "$`\x00\r\n") {
			continue
		}
		// A graph-rooted spelling containing a directory is already exact.
		// Bare response files remain invocation-relative and are resolved below.
		if strings.Contains(candidate, "/") {
			if direct := canonicalKbuildRulePath(candidate); direct == candidate && hasInput(direct) {
				return true
			}
		}
		resolved, _, pathLike := compactKbuildProfileCommandPath(profile, candidate)
		if pathLike && hasInput(resolved) {
			return true
		}
	}
	return false
}

func compactKbuildTypedWorkingArgument(value, objectRoot string, logicalPaths map[string]bool) string {
	prefix := ""
	candidate := value
	if index := strings.IndexByte(value, '='); index >= 0 {
		prefix, candidate = value[:index+1], value[index+1:]
	}
	if strings.HasPrefix(candidate, "@") {
		prefix += "@"
		candidate = strings.TrimPrefix(candidate, "@")
	}
	projected := replaceCompactKbuildTreePathPrefix(candidate, "${tree:prep}", objectRoot)
	projected = replaceCompactKbuildTreePathPrefix(projected, "${work:root}", objectRoot)
	projected = strings.NewReplacer(
		"${tree:prep}", objectRoot,
		"${work:root}", objectRoot,
	).Replace(projected)
	if projected != candidate {
		return prefix + projected
	}
	pathname := canonicalKbuildRulePath(candidate)
	if pathname == "" || pathname != candidate || !logicalPaths[pathname] {
		return value
	}
	return prefix + path.Join(objectRoot, pathname)
}

// compactKbuildRecipeTextHasPipeline recognizes an unquoted shell pipe even
// when the full recipe uses other control flow which the argv parser cannot
// lower. The shell lexer still distinguishes operator tokens from literal
// `|` bytes in quoted data.
func compactKbuildRecipeTextHasPipeline(value string) bool {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return false
	}
	for _, token := range tokens {
		if token.operator && token.value == "|" {
			return true
		}
	}
	return false
}

// compactKbuildCompoundProgramCommands extracts the executable word of every
// simple command in shell text which cannot be lowered by
// parseCompactKbuildRecipe. The shell connectors are grammar, not policy: no
// executable name is assigned special behavior here. The returned commands are
// used solely to validate and materialize path-based programs before scriptrun
// executes the original text.
func compactKbuildCompoundProgramCommands(value string) ([]compactKbuildRecipeCommand, error) {
	ranges, arithmetic, err := compactKbuildCommandSubstitutionRanges(value)
	if err != nil {
		return nil, err
	}
	masked := []byte(value)
	for _, substitution := range append(append([]compactKbuildCommandSubstitutionRange(nil), ranges...), arithmetic...) {
		for index := substitution.start; index < substitution.end; index++ {
			masked[index] = 'x'
		}
	}
	commands, err := compactKbuildFlatCompoundProgramCommands(string(masked))
	if err != nil {
		return nil, err
	}
	for _, command := range commands {
		for _, substitution := range append(append([]compactKbuildCommandSubstitutionRange(nil), ranges...), arithmetic...) {
			if command.programStart < substitution.end && substitution.start < command.programEnd {
				return nil, fmt.Errorf("compound command has dynamic program at byte %d", command.programStart)
			}
		}
	}
	for _, substitution := range ranges {
		contentStart := substitution.start + 2
		nested, nestedErr := compactKbuildCompoundProgramCommands(value[contentStart : substitution.end-1])
		if nestedErr != nil {
			return nil, fmt.Errorf("command substitution at byte %d: %w", substitution.start, nestedErr)
		}
		for index := range nested {
			nested[index].programStart += contentStart
			nested[index].programEnd += contentStart
			for argumentIndex := range nested[index].argumentTokens {
				nested[index].argumentTokens[argumentIndex].start += contentStart
				nested[index].argumentTokens[argumentIndex].end += contentStart
			}
		}
		commands = append(commands, nested...)
	}
	sort.SliceStable(commands, func(i, j int) bool {
		return commands[i].programStart < commands[j].programStart
	})
	return commands, nil
}

// CompactKbuildObjectTreeCommandPrograms returns the canonical object-tree
// paths which the evaluated shell command proves it executes as programs. A
// bare command name is supplied by the selected tool/runtime environment and
// a source-tree path is immutable source, so neither is returned. Callers may
// use the result to distinguish generated executables from generated data,
// without assigning meaning to an output filename or compiler family.
//
// An error means the command-head grammar was not statically decidable. The
// caller must retain its conservative behavior in that case.
func compactKbuildCommandObservationText(profile CompactKbuildProfile, value string) string {
	value = compactKbuildProfileCanonicalRecipeText(profile, value)
	return strings.NewReplacer(
		"${tree:kernel}", "__LINUX_BZL_SOURCE_TREE__",
		"${tree:prep}", "__LINUX_BZL_OBJECT_TREE__",
		"${tree:host}", "__LINUX_BZL_OBJECT_TREE__",
		"${tree:bootstrap}", "__LINUX_BZL_OBJECT_TREE__",
		"${tree:prehost}", "__LINUX_BZL_OBJECT_TREE__",
		"${work:root}", "__LINUX_BZL_OBJECT_TREE__",
		"${tree:"+linuxProbeHostDepsRootName+"}", "__LINUX_BZL_EXTERNAL_TOOL_TREE__",
	).Replace(value)
}

func CompactKbuildObjectTreeCommandPrograms(profile CompactKbuildProfile, value string) ([]string, error) {
	commands, err := compactKbuildCompoundProgramCommands(compactKbuildCommandObservationText(profile, value))
	if err != nil {
		return nil, err
	}
	programs := map[string]bool{}
	for _, command := range commands {
		// Bare names are resolved by the hermetic action-role/runtime toolset.
		// Generated programs must carry path provenance so their producer can be
		// bound as an executable input by final action lowering.
		if path.Base(command.program) == command.program ||
			strings.Contains(command.program, "__LINUX_BZL_EXTERNAL_TOOL_TREE__") {
			continue
		}
		programPath, sourceProgram, pathLike := compactKbuildProfileCommandPath(profile, command.program)
		if !pathLike || sourceProgram {
			continue
		}
		programs[programPath] = true
	}
	result := make([]string, 0, len(programs))
	for program := range programs {
		result = append(result, program)
	}
	sort.Strings(result)
	return result, nil
}

// CompactKbuildRecipeProducesNonIncludeOutput reports whether the final bytes
// written to target are proven to be a configured compiler driver's primary
// object/link output or a configured archiver's mutating archive operand. The
// classification follows action-role and argv provenance selected from
// Kbuild; it does not inspect an output suffix, executable name, compiler
// family, or architecture. Preprocessor, dependency, assembly-text, and
// archive read operations remain potential generated source data. An
// undecidable recipe returns an error so callers can retain conservative data
// semantics.
func CompactKbuildRecipeProducesNonIncludeOutput(profile CompactKbuildProfile, value, target string) (bool, error) {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return false, nil
	}
	commands, err := parseCompactKbuildRecipe(
		compactKbuildCommandObservationText(profile, value),
		compactKbuildAutomaticContext{target: target},
	)
	if err != nil {
		return false, err
	}
	nonIncludeByPath := map[string]bool{}
	for _, command := range commands {
		if command.program == "mv" && len(command.arguments) == 2 && command.stdout == "" {
			source, sourceOK := compactKbuildRecipePath(command.arguments[0])
			destination, destinationOK := compactKbuildRecipePath(command.arguments[1])
			if sourceOK && destinationOK {
				nonIncludeByPath[destination] = nonIncludeByPath[source]
				continue
			}
		}
		role, configured := parseKbuildActionRoleToken(command.program)
		if configured && (role.Role == "cc" || role.Role == "cxx") {
			analysis, analysisErr := analyzeCompactKbuildCompilerOutputs(profile, role.Role, command.arguments, target)
			if analysisErr != nil {
				return false, analysisErr
			}
			output, declared := analysis.PrimaryOutput, analysis.PrimaryOutput != ""
			if declared {
				nonIncludeByPath[output] = toolaction.CompilerInvocationProducesBinaryOutput(role.Role, command.arguments)
			}
			if command.stdout != "" {
				if output, ok := compactKbuildRecipePath(command.stdout); ok {
					nonIncludeByPath[output] = false
				}
			}
			continue
		}
		archiveArguments, archiveCommand := compactKbuildCommandActionRoleArguments(command, "ar")
		if archiveCommand {
			output, declared := compactKbuildArchivePrimaryOutput(archiveArguments)
			if declared {
				nonIncludeByPath[output] = true
			}
			if command.stdout != "" {
				if output, ok := compactKbuildRecipePath(command.stdout); ok {
					nonIncludeByPath[output] = false
				}
			}
			continue
		}
		output, declared, outputErr := compactKbuildRecipeDeclaredOutputForProfile(profile, command, target)
		if outputErr != nil {
			return false, outputErr
		}
		if declared {
			output = canonicalKbuildRulePath(output)
			if command.stdout == "" && nonIncludeByPath[output] {
				_, configuredProgram := parseKbuildActionRoleToken(command.program)
				if configuredProgram || path.Base(command.program) != command.program {
					// A configured or source/generated path command which reuses an
					// existing compiler product as its declared output is an opaque
					// in-place postprocessor. It retains the compiler-originated product
					// provenance unless a later command explicitly replaces the bytes.
					continue
				}
			}
			nonIncludeByPath[output] = false
		}
	}
	return nonIncludeByPath[target], nil
}

func compactKbuildCommandActionRoleArguments(command compactKbuildRecipeCommand, role string) ([]string, bool) {
	if ref, configured := parseKbuildActionRoleToken(command.program); configured && ref.Role == role {
		return command.arguments, true
	}
	if path.Base(command.program) != "xargs" || len(command.arguments) == 0 {
		return nil, false
	}
	index := 0
	if command.arguments[index] == "--" {
		index++
	}
	if index < len(command.arguments) {
		if ref, configured := parseKbuildActionRoleToken(command.arguments[index]); configured && ref.Role == role {
			return command.arguments[index+1:], true
		}
	}
	return nil, false
}

func compactKbuildArchivePrimaryOutput(arguments []string) (string, bool) {
	if len(arguments) < 2 {
		return "", false
	}
	options := strings.TrimPrefix(arguments[0], "-")
	if options == "" || strings.HasPrefix(options, "-") || strings.ContainsAny(options, "ptxM") || !strings.ContainsAny(options, "dmqrs") {
		return "", false
	}
	index := 1
	// Position/count modifiers consume one operand before the archive. Keep the
	// grammar conservative: malformed or unsupported invocations remain data.
	if strings.ContainsAny(options, "abiN") {
		index++
	}
	if index >= len(arguments) {
		return "", false
	}
	return compactKbuildRecipePath(arguments[index])
}

type compactKbuildCommandSubstitutionRange struct {
	start int
	end   int
}

func compactKbuildCommandSubstitutionRanges(value string) ([]compactKbuildCommandSubstitutionRange, []compactKbuildCommandSubstitutionRange, error) {
	ranges := []compactKbuildCommandSubstitutionRange{}
	arithmetic := []compactKbuildCommandSubstitutionRange{}
	quote := byte(0)
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			continue
		}
		if character == '`' {
			return nil, nil, fmt.Errorf("compound command contains unsupported backtick substitution at byte %d", index)
		}
		if character != '$' || index+1 >= len(value) || value[index+1] != '(' {
			continue
		}
		if index+2 < len(value) && value[index+2] == '(' {
			end, err := compactKbuildArithmeticEnd(value, index+3)
			if err != nil {
				return nil, nil, err
			}
			arithmetic = append(arithmetic, compactKbuildCommandSubstitutionRange{start: index, end: end})
			index = end - 1
			continue
		}
		end, err := compactKbuildCompoundCommandSubstitutionEnd(value, index)
		if err != nil {
			return nil, nil, err
		}
		ranges = append(ranges, compactKbuildCommandSubstitutionRange{start: index, end: end})
		index = end - 1
	}
	return ranges, arithmetic, nil
}

func compactKbuildCompoundCommandSubstitutionEnd(value string, start int) (int, error) {
	depth := 1
	quote := byte(0)
	for index := start + 2; index < len(value); index++ {
		character := value[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			continue
		}
		if character == '`' {
			return 0, fmt.Errorf("command substitution contains unsupported backtick substitution at byte %d", index)
		}
		if character == '$' && index+1 < len(value) {
			switch value[index+1] {
			case '{':
				end, err := compactKbuildBracedExpansionEnd(value, index+2)
				if err != nil {
					return 0, err
				}
				index = end - 1
				continue
			case '(':
				end := 0
				var err error
				if index+2 < len(value) && value[index+2] == '(' {
					end, err = compactKbuildArithmeticEnd(value, index+3)
				} else {
					end, err = compactKbuildCompoundCommandSubstitutionEnd(value, index)
				}
				if err != nil {
					return 0, err
				}
				index = end - 1
				continue
			}
		}
		if quote != 0 {
			continue
		}
		switch character {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated command substitution at byte %d", start)
}

func compactKbuildFlatCompoundProgramCommands(value string) ([]compactKbuildRecipeCommand, error) {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, err
	}
	commands := []compactKbuildRecipeCommand{}
	words := []compactKbuildRecipeToken{}
	redirect := ""
	redirectDup := false
	redirectRecordsPath := false
	redirectInput := false
	stdin, stdout := "", ""
	finish := func(connector string) error {
		defer func() {
			words = nil
			redirect = ""
			redirectDup = false
			redirectRecordsPath = false
			redirectInput = false
			stdin, stdout = "", ""
		}()
		// Loop headers bind shell variables and enumerate data; none of their
		// words is an executable identity. Commands nested in substitutions have
		// already been discovered recursively before this flat pass.
		if len(words) != 0 {
			header := strings.TrimLeft(words[0].value, "+@-")
			if header == "for" || header == "select" || header == "case" {
				return nil
			}
			if len(words) == 1 && (header == "done" || header == "fi" || header == "esac") {
				return nil
			}
		}
		for len(words) != 0 {
			word := strings.TrimLeft(words[0].value, "+@-")
			if name, _, assignment := strings.Cut(word, "="); assignment && validKbuildCommandEnvironmentName(name) {
				words = words[1:]
				continue
			}
			// These are POSIX shell grammar words, not executable identities. A
			// command following one of them remains the first executable word in
			// this simple-command segment.
			switch word {
			case "", "!", "if", "then", "elif", "else", "while", "until", "do", "time", "{", "}":
				words = words[1:]
				continue
			}
			break
		}
		if len(words) == 0 {
			return nil
		}
		program := strings.TrimLeft(strings.ReplaceAll(words[0].value, compactKbuildLiteralDollarToken, "$"), "+@-")
		if strings.ContainsAny(program, "$`") {
			return fmt.Errorf("compound command has dynamic program %q", program)
		}
		arguments := make([]string, 0, len(words)-1)
		argumentTokens := make([]compactKbuildRecipeToken, 0, len(words)-1)
		for _, word := range words[1:] {
			arguments = append(arguments, strings.ReplaceAll(word.value, compactKbuildLiteralDollarToken, "$"))
			argumentTokens = append(argumentTokens, word)
		}
		commands = append(commands, compactKbuildRecipeCommand{
			program: program, programStart: words[0].start, programEnd: words[0].end,
			arguments: arguments, argumentTokens: argumentTokens,
			stdin: stdin, stdout: stdout, connector: connector,
		})
		return nil
	}
	isIONumber := func(value string) bool {
		if value == "" {
			return false
		}
		for _, character := range value {
			if character < '0' || character > '9' {
				return false
			}
		}
		return true
	}
	for tokenIndex := 0; tokenIndex < len(tokens); tokenIndex++ {
		token := tokens[tokenIndex]
		if !token.operator && redirect != "" {
			value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
			if redirectDup {
				if value != "-" && !isIONumber(value) {
					return nil, fmt.Errorf("compound descriptor duplication %q has invalid operand %q", redirect, value)
				}
			} else if redirectRecordsPath {
				// The original source script retains the redirection. /dev/null is
				// a process-local sink/source supplied by the execution sandbox,
				// not a generated artifact whose bytes need a graph edge.
				if value != "/dev/null" {
					if redirectInput {
						stdin = value
					} else {
						stdout = value
					}
				}
			}
			redirect = ""
			redirectDup = false
			redirectRecordsPath = false
			redirectInput = false
			continue
		}
		if token.operator {
			operator := token.value
			if (operator == "<" || operator == ">") && tokenIndex+1 < len(tokens) &&
				tokens[tokenIndex+1].operator && tokens[tokenIndex+1].value == "&" &&
				token.end == tokens[tokenIndex+1].start {
				operator += "&"
				tokenIndex++
			}
			switch operator {
			case "<", ">", ">>", "<&", ">&":
				if redirect != "" {
					return nil, fmt.Errorf("compound redirection %q has no operand before %q", redirect, operator)
				}
				fd := ""
				if len(words) != 0 {
					candidate := words[len(words)-1]
					if candidate.start >= 0 && candidate.end <= len(value) &&
						candidate.end == token.start && isIONumber(candidate.value) &&
						isIONumber(value[candidate.start:candidate.end]) {
						fd = candidate.value
						words = words[:len(words)-1]
					}
				}
				redirect = operator
				redirectDup = operator == "<&" || operator == ">&"
				redirectInput = strings.HasPrefix(operator, "<")
				defaultFD := "1"
				if redirectInput {
					defaultFD = "0"
				}
				redirectRecordsPath = !redirectDup && (fd == "" || fd == defaultFD)
			case ";", "&&", "||", "|", "&", "(", ")":
				if redirect != "" {
					return nil, fmt.Errorf("compound redirection %q has no operand before %q", redirect, operator)
				}
				if err := finish(operator); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unsupported compound shell operator %q", operator)
			}
			continue
		}
		words = append(words, token)
	}
	if redirect != "" {
		return nil, fmt.Errorf("compound command has redirection %q without an operand", redirect)
	}
	if err := finish(""); err != nil {
		return nil, err
	}
	return commands, nil
}

// compactKbuildCompoundSurvivingExplicitOutputs returns statically named
// filesystem outputs created by a source-owned compound recipe which remain
// after that recipe completes. Rule outputs are already declared by the
// selected Make rule. The additional paths are side effects whose bytes can be
// consumed implicitly by a later source tool, even when Make uses them for its
// own incremental state as well.
//
// The projection follows source order. An exact rename transfers ownership and
// an exact removal drops the path, so private compiler scratch files do not
// become Bazel outputs merely because an earlier command created them.
func compactKbuildRecipePathExplicitlyRooted(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, compactKbuildActionObjectTreeMarker+"/") ||
		strings.Contains(value, "__LINUX_BZL_OBJECT_TREE__/") ||
		strings.Contains(value, "${tree:prep}/") ||
		strings.Contains(value, "${work:root}/")
}

type compactKbuildRecipeRemovalPath struct {
	path   string
	rooted bool
}

// compactKbuildRecipeRemovalPaths accepts only the bounded, source-explicit
// cleanup form whose complete filesystem effect can be projected statically.
// Shell glob and home-directory expansion are intentionally excluded: the
// cooked argv no longer records whether those bytes were quoted, so treating
// them as one literal path could disagree with the script that actually runs.
func compactKbuildRecipeRemovalPaths(command compactKbuildRecipeCommand) ([]compactKbuildRecipeRemovalPath, error) {
	if path.Base(command.program) != "rm" {
		return nil, fmt.Errorf("command %q is not rm", command.program)
	}
	if len(command.environment) != 0 || command.stdin != "" || command.stdout != "" || command.connector == "|" {
		return nil, fmt.Errorf("rm command has unsupported environment, redirection, or pipeline")
	}
	if len(command.arguments) < 2 || command.arguments[0] != "-f" {
		return nil, fmt.Errorf("rm command is not a bounded -f path removal")
	}
	removed := make([]compactKbuildRecipeRemovalPath, 0, len(command.arguments)-1)
	for _, argument := range command.arguments[1:] {
		if strings.HasPrefix(argument, "~") || strings.ContainsAny(argument, "*?[") {
			return nil, fmt.Errorf("rm command removes dynamically expanded path %q", argument)
		}
		pathname, ok := compactKbuildRecipePath(argument)
		if !ok {
			return nil, fmt.Errorf("rm command removes non-canonical path %q", argument)
		}
		removed = append(removed, compactKbuildRecipeRemovalPath{
			path: pathname, rooted: compactKbuildRecipePathExplicitlyRooted(argument),
		})
	}
	return removed, nil
}

func compactKbuildCompoundSurvivingExplicitOutputs(
	profile CompactKbuildProfile,
	commands []compactKbuildRecipeCommand,
	ruleOutputs ...string,
) ([]string, error) {
	ruleOutputSet := map[string]bool{}
	for _, output := range ruleOutputs {
		output = canonicalKbuildRulePath(output)
		if output != "" {
			ruleOutputSet[output] = true
		}
	}
	canonicalOutput := func(value string, rooted bool) (string, bool) {
		output, ok := compactKbuildRecipePath(value)
		if !ok {
			return "", false
		}
		output = canonicalKbuildRulePath(output)
		if !rooted {
			if scoped, scopedOK := compactKbuildProfileInvocationRelativePath(profile, output); scopedOK {
				output = canonicalKbuildRulePath(scoped)
			}
		}
		return output, output != ""
	}
	explicitOutputRooted := func(command compactKbuildRecipeCommand, output string) bool {
		if command.stdout != "" {
			return command.stdoutRooted || compactKbuildRecipePathExplicitlyRooted(command.stdout)
		}
		for _, argument := range command.arguments {
			if !compactKbuildRecipePathExplicitlyRooted(argument) {
				continue
			}
			candidate := argument
			if index := strings.IndexByte(candidate, '='); index >= 0 {
				candidate = candidate[index+1:]
			}
			candidate = strings.TrimPrefix(candidate, "-o")
			if resolved, ok := compactKbuildRecipePath(candidate); ok && canonicalKbuildRulePath(resolved) == canonicalKbuildRulePath(output) {
				return true
			}
		}
		return false
	}

	for _, command := range commands {
		if command.programEnd != 0 || command.argumentTokens != nil ||
			command.connector != "" && command.connector != ";" {
			return nil, nil
		}
	}

	outputs := map[string]bool{}
	for commandIndex, command := range commands {
		output, declared, err := compactKbuildRecipeExplicitOutputForProfile(profile, command, ruleOutputs...)
		if err != nil {
			return nil, fmt.Errorf("compound command %d explicit output: %w", commandIndex, err)
		}
		if declared {
			if output, ok := canonicalOutput(output, explicitOutputRooted(command, output)); ok {
				outputs[output] = true
			}
		}

		program := path.Base(command.program)
		if program == "mv" && len(command.environment) == 0 && command.stdin == "" && command.stdout == "" &&
			command.connector != "|" && len(command.arguments) == 2 {
			source, sourceOK := canonicalOutput(command.arguments[0], compactKbuildRecipePathExplicitlyRooted(command.arguments[0]))
			destination, destinationOK := canonicalOutput(command.arguments[1], compactKbuildRecipePathExplicitlyRooted(command.arguments[1]))
			if sourceOK && destinationOK && outputs[source] {
				delete(outputs, source)
				outputs[destination] = true
			}
		}
		if program == "rm" {
			removed, removalErr := compactKbuildRecipeRemovalPaths(command)
			if removalErr != nil {
				return nil, fmt.Errorf("compound command %d cleanup: %w", commandIndex, removalErr)
			}
			for _, removal := range removed {
				if removedPath, ok := canonicalOutput(removal.path, removal.rooted); ok {
					delete(outputs, removedPath)
				}
			}
		}
	}
	for output := range ruleOutputSet {
		delete(outputs, output)
	}
	result := make([]string, 0, len(outputs))
	for output := range outputs {
		result = append(result, output)
	}
	sort.Strings(result)
	return result, nil
}

// compactKbuildRecipeLineShells preserves GNU Make's default one-shell-per-
// recipe-line contract when several selected cmd_<name> calls are fused into
// one atomic Bazel action. Each evaluated template remains internally intact,
// so a live pipeline still shares one shell while cwd and shell-local state do
// not leak into the next source recipe line. Prefix lowering is deliberately
// repeated at this final Make-to-shell boundary: nested Kbuild wrappers can
// introduce a control prefix after an inner command template was normalized.
func compactKbuildRecipeLineShells(lines []string) string {
	groups := make([]string, 0, len(lines))
	for _, line := range lines {
		line = compactKbuildRecipeExecutionText(line)
		groups = append(groups, "(\n"+line+"\n)")
	}
	return strings.Join(groups, "\n")
}

func compactKbuildSourceTokenIsExactUnquotedWord(
	value string,
	token compactKbuildRecipeToken,
	want string,
) bool {
	return !token.operator && token.value == want &&
		token.start >= 0 && token.end <= len(value) && token.start <= token.end &&
		value[token.start:token.end] == want
}

// compactKbuildRecipeHasShellComment identifies only an unquoted, unescaped
// # at a shell word boundary. The compact lexer intentionally retains comments
// as ordinary words for executable discovery, but a side-effect projection
// must not interpret separators or redirects which the real shell never sees.
func compactKbuildRecipeHasShellComment(value string) bool {
	quote := byte(0)
	wordStarted := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			switch {
			case character == quote:
				quote = 0
			case character == '\\' && quote == '"' && index+1 < len(value):
				index++
			}
			continue
		}
		switch {
		case character == '\'' || character == '"':
			quote = character
			wordStarted = true
		case character == '\\':
			if index+1 == len(value) {
				return false
			}
			index++
			if value[index] != '\n' {
				wordStarted = true
			}
		case character == '#':
			if !wordStarted {
				return true
			}
		case character == '\n' || strings.ContainsRune(" \t\r;|&<>()>", rune(character)):
			wordStarted = false
		default:
			wordStarted = true
		}
	}
	return false
}

// compactKbuildShellGroupClosingToken recognizes only braces which the shell
// lexer proves were standalone, unquoted source words at list-unit boundaries.
// A quoted or escaped brace has the same cooked token value, but remains
// ordinary argv data and must not affect structural nesting.
func compactKbuildShellGroupClosingToken(
	value string,
	tokens []compactKbuildRecipeToken,
) (int, bool) {
	if len(tokens) == 0 || !compactKbuildSourceTokenIsExactUnquotedWord(value, tokens[0], "{") {
		return 0, false
	}
	depth := 1
	unitBoundary := true
	groupCloseBoundary := false
	for index, token := range tokens[1:] {
		index++
		if compactKbuildSourceTokenIsExactUnquotedWord(value, token, "{") && unitBoundary {
			depth++
			unitBoundary = true
			groupCloseBoundary = false
			continue
		}
		if compactKbuildSourceTokenIsExactUnquotedWord(value, token, "}") && unitBoundary {
			if !groupCloseBoundary {
				return 0, false
			}
			depth--
			if depth == 0 {
				return index, true
			}
			unitBoundary = false
			groupCloseBoundary = false
			continue
		}
		if !token.operator {
			unitBoundary = false
			groupCloseBoundary = false
			continue
		}
		switch token.value {
		case ";":
			unitBoundary = true
			groupCloseBoundary = true
		case "&&", "||", "|", "&", "(":
			unitBoundary = true
			groupCloseBoundary = false
		default:
			unitBoundary = false
			groupCloseBoundary = false
		}
	}
	return 0, false
}

func compactKbuildStaticShellOutputPath(
	token compactKbuildRecipeToken,
	automatic compactKbuildAutomaticContext,
) (string, bool) {
	fields, err := expandKbuildAutomaticCommandField(token.value, automatic)
	if err != nil || len(fields) != 1 {
		return "", false
	}
	field, err := expandPlanKbuildCommandField(fields[0], automatic.target)
	if err != nil || containsUnresolvedPlanMakeReference(field) || containsUnmodeledKbuildDollar(field) {
		return "", false
	}
	field = strings.ReplaceAll(field, compactKbuildLiteralDollarToken, "$")
	// Quote provenance is no longer available after lexical cooking. Decline
	// every spelling which could undergo pathname or home-directory expansion
	// instead of manufacturing a graph path that differs from the shell path.
	if strings.Contains(field, `\`) || strings.HasPrefix(field, "~") || strings.ContainsAny(field, "*?[") {
		return "", false
	}
	if validateShellFreeKbuildCommandField(field) != nil {
		return "", false
	}
	return compactKbuildCommandPath(field)
}

// compactKbuildStaticShellGroupOutput projects the output opened by one exact
// top-level shell group. Redirection setup happens before the group body runs,
// so a static > or >> operand is an unconditional filesystem effect even when
// the body itself contains control flow which the linear argv parser rejects.
func compactKbuildStaticShellGroupOutput(
	value string,
	automatic compactKbuildAutomaticContext,
) (compactKbuildRecipeCommand, bool) {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return compactKbuildRecipeCommand{}, false
	}
	for len(tokens) != 0 && tokens[len(tokens)-1].operator && tokens[len(tokens)-1].value == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) < 4 {
		return compactKbuildRecipeCommand{}, false
	}
	closing, ok := compactKbuildShellGroupClosingToken(value, tokens)
	if !ok || closing != len(tokens)-3 ||
		!tokens[closing+1].operator || tokens[closing+1].value != ">" && tokens[closing+1].value != ">>" ||
		tokens[closing+2].operator {
		return compactKbuildRecipeCommand{}, false
	}
	output, ok := compactKbuildStaticShellOutputPath(tokens[closing+2], automatic)
	if !ok {
		return compactKbuildRecipeCommand{}, false
	}
	return compactKbuildRecipeCommand{
		program: ":", stdout: output,
		stdoutRooted: compactKbuildRecipePathExplicitlyRooted(tokens[closing+2].value),
	}, true
}

const compactKbuildSideEffectProjectionMaxUnits = 1024

// compactKbuildTopLevelSemicolonUnits partitions only the bounded shell shape
// needed by Kbuild's compound-command wrappers. Top-level sequencing must use
// semicolons. Control-flow connectors remain available inside an exact brace
// group, but are rejected between units rather than being approximated.
func compactKbuildTopLevelSemicolonUnits(value string) ([]string, bool) {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, false
	}
	units := make([]string, 0)
	unitStart := 0
	braceDepth := 0
	unitBoundary := true
	groupCloseBoundary := false
	appendUnit := func(end int) bool {
		if unitStart == end {
			return true
		}
		if len(units) == compactKbuildSideEffectProjectionMaxUnits {
			return false
		}
		first, last := tokens[unitStart], tokens[end-1]
		units = append(units, value[first.start:last.end])
		return true
	}
	for index, token := range tokens {
		if compactKbuildSourceTokenIsExactUnquotedWord(value, token, "{") && unitBoundary {
			braceDepth++
			unitBoundary = true
			groupCloseBoundary = false
			continue
		}
		if compactKbuildSourceTokenIsExactUnquotedWord(value, token, "}") && unitBoundary {
			if braceDepth == 0 || !groupCloseBoundary {
				return nil, false
			}
			braceDepth--
			unitBoundary = false
			groupCloseBoundary = false
			continue
		}
		if !token.operator {
			unitBoundary = false
			groupCloseBoundary = false
			continue
		}
		if braceDepth == 0 {
			switch token.value {
			case ";":
				if !appendUnit(index) {
					return nil, false
				}
				unitStart = index + 1
				unitBoundary = true
				groupCloseBoundary = true
				continue
			case "&&", "||", "|", "&", "(", ")":
				return nil, false
			}
			continue
		}
		switch token.value {
		case ";":
			unitBoundary = true
			groupCloseBoundary = true
		case "&&", "||", "|", "&", "(":
			unitBoundary = true
			groupCloseBoundary = false
		default:
			unitBoundary = false
			groupCloseBoundary = false
		}
	}
	if braceDepth != 0 || !appendUnit(len(tokens)) {
		return nil, false
	}
	return units, true
}

func compactKbuildSemicolonSideEffectProjection(
	value string,
	automatic compactKbuildAutomaticContext,
) ([]compactKbuildRecipeCommand, bool) {
	if compactKbuildRecipeHasShellComment(value) {
		return nil, false
	}
	units, ok := compactKbuildTopLevelSemicolonUnits(value)
	if !ok {
		return nil, false
	}
	commands := make([]compactKbuildRecipeCommand, 0, len(units))
	for _, unit := range units {
		parsed, err := parseCompactKbuildRecipe(unit, automatic)
		if err == nil {
			straightLine := true
			for _, command := range parsed {
				if command.connector != "" && command.connector != ";" {
					straightLine = false
					break
				}
			}
			if straightLine && len(parsed) != 0 {
				commands = append(commands, parsed...)
				continue
			}
		}
		groupOutput, groupOK := compactKbuildStaticShellGroupOutput(unit, automatic)
		if !groupOK {
			return nil, false
		}
		commands = append(commands, groupOutput)
	}
	return commands, true
}

// compactKbuildRecipeSideEffectProjection retains only per-line filesystem
// effects whose source grammar is statically straight-line. The executable
// scanner remains responsible for an opaque compound script's tool closure;
// this separate projection prevents its lossy control-flow model from either
// manufacturing conditional outputs or erasing exact sibling-line outputs.
func compactKbuildRecipeSideEffectProjection(
	templates []string,
	automatic compactKbuildAutomaticContext,
) []compactKbuildRecipeCommand {
	commands := make([]compactKbuildRecipeCommand, 0)
	for _, template := range templates {
		if compactKbuildRecipeHasShellComment(template) {
			continue
		}
		parsed, err := parseCompactKbuildRecipe(template, automatic)
		if err == nil {
			straightLine := true
			for _, command := range parsed {
				if command.connector != "" && command.connector != ";" {
					straightLine = false
					break
				}
			}
			if straightLine {
				commands = append(commands, parsed...)
				continue
			}
		}
		if projected, ok := compactKbuildSemicolonSideEffectProjection(template, automatic); ok {
			commands = append(commands, projected...)
		}
	}
	return commands
}

type compactKbuildScriptSourceReplacement struct {
	start int
	end   int
	value string
}

type compactKbuildCompoundCompilerOutputAnalysis struct {
	Replacements               []compactKbuildScriptSourceReplacement
	PreparedObjectInputs       []string
	PreparedObjectIncludeFiles []string
	PreparedObjectDirectories  []string
	PersistentOutputs          []string
	LibrarySearchDirectories   []string
	WorkingDirectories         []string
}

func compactKbuildCompilerSeparatedOptionPayloads(role string, arguments []string) map[int]bool {
	payloads := map[int]bool{}
	for index := 0; index+1 < len(arguments); index++ {
		argument := arguments[index]
		consumes := false
		switch role {
		case "cc", "cxx":
			switch argument {
			case "-o", "-MF", "-MT", "-MQ", "-MJ", "-x",
				"-I", "-idirafter", "-imacros", "-include", "-iquote", "-isystem",
				"-D", "-U", "-B", "-isysroot", "--sysroot", "-target", "--target",
				"-Xassembler", "-Xclang", "-Xlinker":
				consumes = true
			}
		case "rustc", "clippy":
			switch argument {
			case "-o", "-L", "--cfg", "--codegen", "--crate-name", "--crate-type",
				"--edition", "--emit", "--extern", "--out-dir", "--target",
				"-A", "-C", "-D", "-F", "-W", "-Z":
				consumes = true
			}
		}
		if consumes {
			payloads[index+1] = true
			index++
		}
	}
	return payloads
}

func compactKbuildCompilerSourceRoot(
	profile CompactKbuildProfile,
	plan *ActionPlan,
	metadata *CompactMetadata,
	input compactKbuildRuleInput,
) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("compound compiler source %q has no action plan", input.path)
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return "", err
	}
	source, ok := plan.sourcesByID[input.sourceID]
	if !ok {
		return "", fmt.Errorf("compound compiler source %q references unknown source %q", input.path, input.sourceID)
	}
	if canonicalKbuildRulePath(source.Path) != canonicalKbuildRulePath(input.path) {
		return "", fmt.Errorf(
			"compound compiler source %q resolves source %q at a different path %q",
			input.path, input.sourceID, source.Path,
		)
	}
	overlay, err := compactKbuildGraphPathUsesSourceOverlay(profile, input.path)
	if err != nil {
		return "", err
	}
	if !overlay {
		if source.Namespace != "kernel" {
			return "", fmt.Errorf(
				"compound compiler source %q uses unbound immutable namespace %q",
				input.path, source.Namespace,
			)
		}
		return "__LINUX_BZL_SOURCE_TREE__", nil
	}
	if metadata == nil {
		return "", fmt.Errorf("compound compiler source overlay %q has no namespace metadata", input.path)
	}
	overlayRoot, _, err := compactKbuildSourceOverlayRoot(profile)
	if err != nil {
		return "", err
	}
	namespace, selected, err := metadata.selectedActionPlanSourceNamespace(overlayRoot)
	if err != nil {
		return "", err
	}
	if !selected || source.Namespace != namespace {
		return "", fmt.Errorf(
			"compound compiler source %q uses namespace %q, want selected overlay namespace %q",
			input.path, source.Namespace, namespace,
		)
	}
	return "__LINUX_BZL_OBJECT_TREE__", nil
}

// compactKbuildRootImmutableCompilerInputs preserves the physical provenance
// of exact compiler translation-unit operands. Kernel sources retain their
// immutable checkout so quoted includes begin beside the original file;
// external-overlay sources remain in the private work tree which stages their
// complete namespace. Split option payloads such as `-MT $<` are data, not
// compiler inputs, even when their bytes equal a declared source path.
func compactKbuildRootImmutableCompilerInputs(
	profile CompactKbuildProfile,
	plan *ActionPlan,
	metadata *CompactMetadata,
	role string,
	command compactKbuildRecipeCommand,
	inputs []compactKbuildRuleInput,
) (compactKbuildRecipeCommand, error) {
	sourceRoots := map[string]string{}
	for _, input := range inputs {
		if input.sourceID == "" || input.producer != "" || input.objectTree {
			continue
		}
		inputPath := canonicalKbuildRulePath(input.path)
		if inputPath == "" {
			continue
		}
		if err := validatePlanRelativePath("compound compiler source input", inputPath); err != nil {
			return compactKbuildRecipeCommand{}, err
		}
		root, err := compactKbuildCompilerSourceRoot(profile, plan, metadata, input)
		if err != nil {
			return compactKbuildRecipeCommand{}, err
		}
		sourceRoots[inputPath] = root
	}
	if len(sourceRoots) == 0 {
		return command, nil
	}
	command.arguments = slices.Clone(command.arguments)
	payloads := compactKbuildCompilerSeparatedOptionPayloads(role, command.arguments)
	for index, argument := range command.arguments {
		if payloads[index] || argument == "" || strings.HasPrefix(argument, "-") || strings.ContainsAny(argument, "$`\x00\r\n\t ") {
			continue
		}
		argumentPath := canonicalKbuildRulePath(argument)
		if root := sourceRoots[argumentPath]; root != "" {
			command.arguments[index] = root + "/" + argumentPath
		}
	}
	return command, nil
}

// compactKbuildCompoundCompilerOutputs applies the same configured-compiler
// path and output analysis used by linear argv lowering to every compiler
// invocation in a source-owned script. Compiler argv words whose physical
// provenance changes are replaced; the surrounding shell program, pipelines,
// command substitutions, and control flow retain their exact source-selected
// structure.
func compactKbuildCompoundCompilerOutputs(
	profile CompactKbuildProfile,
	plan *ActionPlan,
	metadata *CompactMetadata,
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	typedObjectRoot string,
	template string,
	commands []compactKbuildRecipeCommand,
	knownGraphOutputs ...string,
) (compactKbuildCompoundCompilerOutputAnalysis, error) {
	result := compactKbuildCompoundCompilerOutputAnalysis{}
	workingDirectories := map[string]bool{}
	preparedObjectInputs := map[string]bool{}
	preparedObjectIncludeFiles := map[string]bool{}
	preparedObjectDirectories := map[string]bool{}
	persistentOutputs := map[string]bool{}
	librarySearchDirectories := map[string]bool{}
	for commandIndex, command := range commands {
		role, compiler := compactKbuildCommandCompilerRole(command)
		if !compiler {
			continue
		}
		if len(command.argumentTokens) != len(command.arguments) {
			return compactKbuildCompoundCompilerOutputAnalysis{}, fmt.Errorf(
				"compound compiler command %d has %d argv words but %d source extents",
				commandIndex, len(command.arguments), len(command.argumentTokens),
			)
		}
		arguments := slices.Clone(command.arguments)
		for index := range arguments {
			extent := command.argumentTokens[index]
			if extent.start >= 0 && extent.end > extent.start && extent.end <= len(template) &&
				strings.Contains(template[extent.start:extent.end], "$(") {
				// The compound command scanner masks nested substitutions while it
				// discovers command heads. Do not mistake those mask bytes for a
				// statically declared compiler pathname.
				arguments[index] = template[extent.start:extent.end]
			}
			arguments[index] = compactKbuildMaterializeActionTreeMarkers(arguments[index])
		}
		projected := command
		projected.arguments = arguments
		projected, err := rewriteCompactKbuildCommandAutomaticPaths(profile, target, match, projected)
		if err != nil {
			return compactKbuildCompoundCompilerOutputAnalysis{}, fmt.Errorf("compound compiler command %d automatic paths: %w", commandIndex, err)
		}
		projected = rewriteCompactKbuildCommandInvocationInputs(profile, projected, inputs)
		projected, err = compactKbuildRootImmutableCompilerInputs(
			profile, plan, metadata, role, projected, inputs,
		)
		if err != nil {
			return compactKbuildCompoundCompilerOutputAnalysis{}, fmt.Errorf(
				"compound compiler command %d source inputs: %w", commandIndex, err,
			)
		}
		analysis, err := analyzeCompactKbuildCompilerOutputs(profile, role, projected.arguments, knownGraphOutputs...)
		if err != nil {
			return compactKbuildCompoundCompilerOutputAnalysis{}, fmt.Errorf("compound compiler command %d outputs: %w", commandIndex, err)
		}
		// Compiler outputs retain their private work-root spelling in a compound
		// script. Only arguments which output analysis left untouched may project
		// rooted automatic-target data such as -MT $@ to a logical graph path.
		logicalArguments := rewriteCompactKbuildCommandRootedRuleOutputs(target, match, projected).arguments
		for argumentIndex := range analysis.Arguments {
			if analysis.Arguments[argumentIndex] == projected.arguments[argumentIndex] {
				analysis.Arguments[argumentIndex] = logicalArguments[argumentIndex]
			}
		}
		if typedObjectRoot != "" {
			// Output analysis needs canonical graph paths, but this compound
			// script executes in Make's typed nested cwd. Project exact generated
			// inputs and analyzed private-work outputs through the cwd-to-root
			// spelling only after their graph semantics have been derived.
			// Immutable source operands were already rooted by
			// compactKbuildRootImmutableCompilerInputs and retain their separate
			// source/overlay provenance.
			generatedInputs := map[string]bool{}
			for _, input := range inputs {
				if input.producer == "" && !input.objectTree {
					continue
				}
				if pathname := canonicalKbuildRulePath(input.path); pathname != "" {
					generatedInputs[pathname] = true
				}
			}
			for index, argument := range analysis.Arguments {
				analysis.Arguments[index] = compactKbuildTypedWorkingArgument(
					argument, typedObjectRoot, generatedInputs,
				)
			}
		}
		for _, directory := range analysis.WorkingDirectories {
			workingDirectories[directory] = true
		}
		for _, input := range analysis.PreparedObjectInputs {
			preparedObjectInputs[input] = true
		}
		for _, input := range analysis.PreparedObjectIncludeFiles {
			preparedObjectIncludeFiles[input] = true
		}
		for _, directory := range analysis.PreparedObjectDirectories {
			preparedObjectDirectories[directory] = true
		}
		for _, output := range analysis.PersistentOutputs {
			persistentOutputs[output] = true
		}
		for _, directory := range analysis.LibrarySearchDirectories {
			librarySearchDirectories[directory] = true
		}
		for argumentIndex, rewritten := range analysis.Arguments {
			if rewritten == arguments[argumentIndex] {
				continue
			}
			// This replacement is embedded in an encoded source-owned script,
			// outside mapdirectoryrecipe's ActionRecipe placeholder expansion.
			// Preserve writable-object provenance with the private planner marker;
			// final script lowering projects it to a typed tree binding or a path
			// relative to the selected Kbuild invocation cwd.
			rewritten = strings.ReplaceAll(rewritten, "${work:root}", compactKbuildActionObjectTreeMarker)
			rewritten = compactKbuildPrivateActionRootMarker(rewritten)
			extent := command.argumentTokens[argumentIndex]
			if extent.start < 0 || extent.end <= extent.start || extent.end > len(template) {
				return compactKbuildCompoundCompilerOutputAnalysis{}, fmt.Errorf(
					"compound compiler command %d argument %d has invalid source extent [%d,%d)",
					commandIndex, argumentIndex, extent.start, extent.end,
				)
			}
			result.Replacements = append(result.Replacements, compactKbuildScriptSourceReplacement{
				start: extent.start,
				end:   extent.end,
				value: compactKbuildShellLiteralWord(rewritten),
			})
		}
	}
	for directory := range workingDirectories {
		result.WorkingDirectories = append(result.WorkingDirectories, directory)
	}
	for input := range preparedObjectInputs {
		result.PreparedObjectInputs = append(result.PreparedObjectInputs, input)
	}
	for input := range preparedObjectIncludeFiles {
		result.PreparedObjectIncludeFiles = append(result.PreparedObjectIncludeFiles, input)
	}
	for directory := range preparedObjectDirectories {
		result.PreparedObjectDirectories = append(result.PreparedObjectDirectories, directory)
	}
	for output := range persistentOutputs {
		result.PersistentOutputs = append(result.PersistentOutputs, output)
	}
	for directory := range librarySearchDirectories {
		result.LibrarySearchDirectories = append(result.LibrarySearchDirectories, directory)
	}
	sort.Strings(result.WorkingDirectories)
	sort.Strings(result.PreparedObjectInputs)
	sort.Strings(result.PreparedObjectIncludeFiles)
	sort.Strings(result.PreparedObjectDirectories)
	sort.Strings(result.PersistentOutputs)
	sort.Strings(result.LibrarySearchDirectories)
	return result, nil
}

// compactKbuildPersistentCompilerOutputs publishes compiler results which are
// not ordinary Kbuild rule outputs but remain part of the configured object
// tree (for example Rust metadata and proc-macro libraries). If a compiler
// result is already the physical command output, annotate that exact slot
// rather than silently dropping its SDK provenance. Observed-state envelopes
// are snapshots owned by mapdirectoryrecipe, not compiler files, and must
// never be reclassified as persistent compiler output.
func compactKbuildPersistentCompilerOutputs(
	outputs []ActionPlanOutput,
	persistentPaths []string,
	outputTree string,
	artifactPath func(string) string,
) ([]ActionPlanOutput, error) {
	result := append([]ActionPlanOutput(nil), outputs...)
	additional := make([]ActionPlanOutput, 0, len(persistentPaths))
	seen := map[string]bool{}
	for _, persistent := range persistentPaths {
		if persistent == "" || seen[persistent] {
			continue
		}
		seen[persistent] = true
		matching := -1
		for index, output := range result {
			if output.Path != persistent {
				continue
			}
			if output.ObservedPath != "" {
				return nil, fmt.Errorf(
					"persistent compiler output %q aliases observed-state capture %q",
					persistent, output.ObservedPath,
				)
			}
			if output.Tree != outputTree {
				return nil, fmt.Errorf(
					"persistent compiler output %q is already declared in %s tree, want %s",
					persistent, output.Tree, outputTree,
				)
			}
			if matching >= 0 {
				return nil, fmt.Errorf("persistent compiler output %q has repeated descriptors", persistent)
			}
			matching = index
		}
		if matching >= 0 {
			result[matching].persistent = true
			continue
		}
		if artifactPath == nil {
			return nil, fmt.Errorf("persistent compiler output %q has no physical path allocator", persistent)
		}
		additional = append(additional, ActionPlanOutput{
			Tree:         outputTree,
			Path:         persistent,
			ArtifactPath: artifactPath(persistent),
			persistent:   true,
		})
	}
	if len(additional) == 0 {
		return result, nil
	}

	// Observed-state output ordinals are resolved against their descriptor
	// slots. Keep ordinary and persistent command outputs before the first
	// envelope so filtering observations does not renumber working outputs.
	insert := len(result)
	for index, output := range result {
		if output.ObservedPath != "" {
			insert = index
			break
		}
	}
	withPersistent := make([]ActionPlanOutput, 0, len(result)+len(additional))
	withPersistent = append(withPersistent, result[:insert]...)
	withPersistent = append(withPersistent, additional...)
	withPersistent = append(withPersistent, result[insert:]...)
	return withPersistent, nil
}

func compactKbuildShellLiteralWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func applyCompactKbuildScriptSourceReplacements(
	template string,
	replacements []compactKbuildScriptSourceReplacement,
) (string, error) {
	sort.Slice(replacements, func(i, j int) bool {
		if replacements[i].start != replacements[j].start {
			return replacements[i].start > replacements[j].start
		}
		return replacements[i].end > replacements[j].end
	})
	lastStart := len(template)
	for index, replacement := range replacements {
		if replacement.start < 0 || replacement.end <= replacement.start || replacement.end > lastStart {
			return "", fmt.Errorf(
				"script source replacement %d has invalid or overlapping extent [%d,%d)",
				index, replacement.start, replacement.end,
			)
		}
		template = template[:replacement.start] + replacement.value + template[replacement.end:]
		lastStart = replacement.start
	}
	return template, nil
}

func restoreCompactKbuildLiteralActionMarkers(value string) (string, []int, error) {
	var out strings.Builder
	offsets := []int{}
	for cursor := 0; cursor < len(value); cursor++ {
		switch value[cursor] {
		case compactKbuildLiteralTreeEscapeByte[0]:
			candidate := "$" + value[cursor+1:]
			match := actionRecipePlaceholder.FindStringSubmatchIndex(candidate)
			if len(match) < 6 || match[0] != 0 || match[2] < 0 || match[3] < 0 ||
				candidate[match[2]:match[3]] != "tree" {
				return "", nil, fmt.Errorf("reserved literal-tree byte does not prefix a complete tree marker")
			}
			offsets = append(offsets, out.Len())
			out.WriteByte('$')
		case compactKbuildLiteralSentinelEscapeByte[0]:
			restored := "_" + value[cursor+1:]
			valid := false
			for _, sentinel := range []string{
				"__LINUX_BZL_SOURCE_TREE__",
				"__LINUX_BZL_OBJECT_TREE__",
				linuxProbeHostDepsSentinel,
			} {
				if strings.HasPrefix(restored, sentinel) {
					valid = true
					break
				}
			}
			if !valid {
				return "", nil, fmt.Errorf("reserved literal-sentinel byte does not prefix a known sentinel")
			}
			out.WriteByte('_')
		default:
			out.WriteByte(value[cursor])
		}
	}
	return out.String(), offsets, nil
}

func replaceCompactKbuildTreePathPrefix(value, marker, replacement string) string {
	if marker == "" || !strings.Contains(value, marker+"/") {
		return value
	}
	prefix := strings.TrimSuffix(replacement, "/")
	if prefix == "." {
		prefix = ""
	} else if prefix != "" {
		prefix += "/"
	}
	return strings.ReplaceAll(value, marker+"/", prefix)
}

// buildHermeticKbuildScript is the generic fallback for a selected Kbuild
// command whose shell control flow cannot be represented as a linear argv
// graph. The evaluated recipe remains source-owned; configured action-role
// tokens are rewritten to private scriptrun proxies and every prerequisite is
// staged in a private working directory. PATH contains the selected multicall
// runtime's applets and explicitly bound configured-tool proxies. A runtime
// applet may share a configured role's basename (BusyBox supplies ar and awk),
// but it is not that configured role; selected proxies replace such applets.
func (b *compactKbuildRulePlanBuilder) buildHermeticKbuildScript(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	template string,
) (string, error) {
	return b.buildHermeticKbuildScriptContext(target, match, inputs, template, nil, nil)
}

// buildHermeticKbuildPipeline preserves a parsed source pipeline as one
// compound action.  Splitting a pipe into Bazel actions changes both its byte
// stream and cwd: arbitrary data can contain object-looking words, and tools
// such as ar intentionally persist path spellings received over stdin.  The
// parsed commands are used only to declare executable program inputs; their
// argv is still the exact source-evaluated script.
func (b *compactKbuildRulePlanBuilder) buildHermeticKbuildPipeline(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	template string,
	commands []compactKbuildRecipeCommand,
) (string, error) {
	if !compactKbuildRecipeHasPipeline(commands) {
		return "", fmt.Errorf("compound Kbuild pipeline has no pipe")
	}
	return b.buildHermeticKbuildCompound(target, match, inputs, template, commands)
}

func (b *compactKbuildRulePlanBuilder) buildHermeticKbuildCompound(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	template string,
	commands []compactKbuildRecipeCommand,
) (string, error) {
	if len(commands) == 0 {
		return "", fmt.Errorf("compound Kbuild recipe has no parsed commands")
	}
	return b.buildHermeticKbuildCompoundWithSideEffects(target, match, inputs, template, commands, commands)
}

func (b *compactKbuildRulePlanBuilder) buildHermeticKbuildCompoundWithSideEffects(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	template string,
	commands []compactKbuildRecipeCommand,
	sideEffectCommands []compactKbuildRecipeCommand,
) (string, error) {
	if len(commands) == 0 {
		return "", fmt.Errorf("compound Kbuild recipe has no discovered commands")
	}
	return b.buildHermeticKbuildScriptContext(
		target, match, inputs, template, commands, sideEffectCommands,
	)
}

func (b *compactKbuildRulePlanBuilder) buildHermeticKbuildScriptContext(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	template string,
	compoundCommands []compactKbuildRecipeCommand,
	sideEffectCommands []compactKbuildRecipeCommand,
) (string, error) {
	directInputs := slices.Clone(inputs)
	sourceCompoundCommands := slices.Clone(sideEffectCommands)
	straightLineCompound := sideEffectCommands != nil
	for _, command := range sourceCompoundCommands {
		if command.programEnd != 0 || command.argumentTokens != nil ||
			command.connector != "" && command.connector != ";" {
			straightLineCompound = false
			break
		}
	}
	if template == "" || len(template) > 1<<20 || strings.ContainsRune(template, 0) {
		return "", fmt.Errorf("evaluated script is empty, invalid, or exceeds 1 MiB")
	}
	if err := validateKbuildDeferredShellSingleWordPlacements(match.profile, template); err != nil {
		return "", err
	}
	runtimeApplets, err := compactKbuildScriptRuntimeApplets(b.metadata.actionRoles, b.actionScope())
	if err != nil {
		return "", err
	}
	executionDirectory, objectRoot := "", ""
	executableProgramPaths := map[string]bool{}
	literalTreeOffsets := []int{}
	authorizedScriptTrees := map[string]bool{}
	recordPrivateScriptTrees := func(value string) {
		if strings.Contains(value, compactKbuildActionSourceTreeMarker) {
			authorizedScriptTrees["kernel"] = true
		}
		if strings.Contains(value, compactKbuildActionObjectTreeMarker) {
			authorizedScriptTrees["prep"] = true
		}
		if strings.Contains(value, compactKbuildActionHostDepsTreeMarker) {
			authorizedScriptTrees[linuxProbeHostDepsRootName] = true
		}
	}
	pathSensitiveArchiveOutput := false
	compilerCommands := compoundCommands
	if compoundCommands != nil {
		// Re-scan the exact compound text so command-head provenance and static
		// arguments refer to the bytes that will execute, regardless of whether
		// the caller first reached this path through the linear parser or its
		// control-flow fallback.
		compoundCommands, err = compactKbuildCompoundProgramCommands(template)
		if err != nil {
			return "", fmt.Errorf("atomic compound program discovery: %w", err)
		}
		if len(compoundCommands) == 0 {
			return "", fmt.Errorf("atomic compound has no source-selected program")
		}
		compilerCommands = compoundCommands
	} else {
		// Direct hermetic-script fallback retains no parsed argv graph, but its
		// configured compiler commands still have typed action-role tokens. Use
		// the same bounded compound scanner solely for compiler output analysis.
		discovered, discoveryErr := compactKbuildCompoundProgramCommands(template)
		if discoveryErr == nil {
			compilerCommands = discovered
		} else {
			refs, refsErr := KbuildActionRoleRefs(template)
			if refsErr != nil {
				return "", refsErr
			}
			for _, ref := range refs {
				switch ref.Role {
				case "cc", "cxx", "rustc", "clippy":
					return "", fmt.Errorf("hermetic compiler command discovery: %w", discoveryErr)
				}
			}
		}
	}
	if compoundCommands != nil {
		executionDirectory, objectRoot, err = compactKbuildTypedPrivateExecution(match.profile)
		if err != nil {
			return "", fmt.Errorf("atomic compound typed invocation execution: %w", err)
		}
	}
	compilerOutputs, err := compactKbuildCompoundCompilerOutputs(
		match.profile, b.plan, b.metadata, target, match, inputs, objectRoot, template, compilerCommands,
		compactKbuildRecipeRuleOutputs(target, match)...,
	)
	if err != nil {
		return "", err
	}
	sideOutputs := []string{}
	if straightLineCompound {
		sideOutputs, err = compactKbuildCompoundSurvivingExplicitOutputs(
			match.profile, sourceCompoundCommands, compactKbuildRecipeRuleOutputs(target, match)...,
		)
		if err != nil {
			return "", err
		}
	}
	scriptReplacements := compilerOutputs.Replacements
	inputs, err = b.compactKbuildCompilerLibraryInputs(
		target, match.profile, inputs, compilerOutputs.LibrarySearchDirectories,
	)
	if err != nil {
		return "", fmt.Errorf("compound compiler library inputs: %w", err)
	}
	inputs, err = b.compactKbuildCompilerPreparedObjectDirectoryInputs(
		match.profile, inputs, compilerOutputs.PreparedObjectDirectories,
	)
	if err != nil {
		return "", fmt.Errorf("compound compiler prepared-object directory inputs: %w", err)
	}
	if b.metadata != nil && b.metadata.preconfiguredObjectTree {
		inputs, err = b.compactKbuildCompilerPreparedObjectInputs(inputs, compilerOutputs.PreparedObjectIncludeFiles)
		if err != nil {
			return "", fmt.Errorf("compound compiler prepared-object include files: %w", err)
		}
	}
	inputs, err = b.compactKbuildCompilerPreparedObjectInputs(inputs, compilerOutputs.PreparedObjectInputs)
	if err != nil {
		return "", fmt.Errorf("compound compiler prepared-object inputs: %w", err)
	}
	if compoundCommands != nil {
		pathSensitiveArchiveOutput = compactKbuildRecipeRetainsArchiveMemberPaths(compoundCommands)
		// Programs selected from the source or object tree remain ordinary staged
		// files in the script text.  Mark their exact producer bindings executable;
		// configured role tokens and bare multicall applets need no file edge.
		localOutputs := map[string]bool{}
		pendingLocalOutputs := map[string]bool{}
		for commandIndex, command := range compoundCommands {
			_, configuredProgram := parseKbuildActionRoleToken(command.program)
			if configuredProgram || path.Base(command.program) == command.program {
				// Configured tools and runtime applets are supplied by the action
				// toolset rather than by an artifact edge.
			} else if applet, ok := compactKbuildAbsoluteRuntimeApplet(command.program); ok {
				if command.programStart < 0 || command.programEnd <= command.programStart || command.programEnd > len(template) {
					return "", fmt.Errorf("pipeline command %d runtime applet %q has invalid source provenance", commandIndex, command.program)
				}
				scriptReplacements = append(scriptReplacements, compactKbuildScriptSourceReplacement{
					start: command.programStart, end: command.programEnd, value: applet,
				})
			} else {
				programPath, sourceProgram, ok := compactKbuildProfileCommandPath(match.profile, command.program)
				if !ok {
					return "", fmt.Errorf("pipeline command %d has non-hermetic program %q", commandIndex, command.program)
				}
				// Commands in one atomic recipe share the same private object tree.
				// An earlier command's declared output is therefore already present
				// when a later command executes it. Looking that path up in the global
				// target graph would rediscover the recipe currently being built and
				// introduce a false self-cycle (Linux's cmd_and_fixdep has exactly
				// this shape while bootstrapping scripts/basic/fixdep).
				if !localOutputs[programPath] {
					var programIndex int
					inputs, programIndex, err = b.ensureCommandProgramInput(inputs, programPath, sourceProgram)
					if err != nil {
						return "", fmt.Errorf(
							"pipeline command %d program %q with prior local outputs %q: %w",
							commandIndex, programPath, slices.Sorted(maps.Keys(localOutputs)), err,
						)
					}
					if inputs[programIndex].producer == "" {
						return "", fmt.Errorf("pipeline command %d program %q is not a generated executable", commandIndex, programPath)
					}
					executableProgramPaths[programPath] = true
				}
			}
			output, declared, outputErr := compactKbuildRecipeExplicitOutputForProfile(
				match.profile, command, compactKbuildRecipeRuleOutputs(target, match)...,
			)
			if outputErr != nil {
				return "", fmt.Errorf("pipeline command %d compiler output: %w", commandIndex, outputErr)
			}
			if declared {
				rawOutput := output
				rootedOutput := compactKbuildRecipePathExplicitlyRooted(rawOutput)
				var valid bool
				output, valid = compactKbuildRecipePath(rawOutput)
				if !valid {
					return "", fmt.Errorf("pipeline command %d has invalid explicit output %q", commandIndex, rawOutput)
				}
				if !rootedOutput {
					if scoped, ok := compactKbuildProfileInvocationRelativePath(match.profile, output); ok {
						output = scoped
					}
				}
				output = canonicalKbuildRulePath(output)
				if output != "" {
					pendingLocalOutputs[output] = true
				}
			}
			if command.connector != "|" {
				for output := range pendingLocalOutputs {
					localOutputs[output] = true
				}
				clear(pendingLocalOutputs)
			}
		}
		var replacementErr error
		template, replacementErr = applyCompactKbuildScriptSourceReplacements(template, scriptReplacements)
		if replacementErr != nil {
			return "", replacementErr
		}
		scriptReplacements = nil
		recordPrivateScriptTrees(template)
		template = replaceCompactKbuildTreePathPrefix(template, compactKbuildActionObjectTreeMarker, objectRoot)
		template = strings.NewReplacer(
			compactKbuildActionSourceTreeMarker, "${tree:kernel}",
			compactKbuildActionObjectTreeMarker, objectRoot,
			compactKbuildActionHostDepsTreeMarker, "${tree:"+linuxProbeHostDepsRootName+"}",
		).Replace(template)
	}
	if len(scriptReplacements) != 0 {
		template, err = applyCompactKbuildScriptSourceReplacements(template, scriptReplacements)
		if err != nil {
			return "", err
		}
	}
	if compoundCommands == nil {
		recordPrivateScriptTrees(template)
		// Generic hermetic scripts do not have a typed invocation cwd onto which
		// action-object markers can be projected. Carry the writable root through
		// scriptrun's decoded-script tree protocol instead; the recipe binds prep
		// to its private ${work:root} below. Keep the private source marker until
		// after generic declared-input rewriting so a compiler operand is not
		// redirected back to its staged copy and lose quoted-include adjacency.
		template = strings.ReplaceAll(template, compactKbuildActionObjectTreeMarker, "${tree:prep}")
	}
	environmentUsage, err := compactKbuildHermeticScriptEnvironmentUsage(match.profile, template, compoundCommands)
	if err != nil {
		return "", fmt.Errorf("inspect hermetic script environment: %w", err)
	}
	environment, environmentRoles, err := compactKbuildSourceScriptEnvironment(
		target, match, directInputs, nil, environmentUsage, b.actionScope(), b.metadata.actionRoles,
	)
	if err != nil {
		return "", fmt.Errorf("hermetic script exported environment: %w", err)
	}
	replayTargets := append([]string{target, match.lookupTarget}, compactKbuildRecipeRuleOutputs(target, match)...)
	commandReplays, err := compactKbuildSourceScriptCommandReplays(match.profile, replayTargets...)
	if err != nil {
		return "", fmt.Errorf("hermetic script recursive replay: %w", err)
	}
	if environmentUsage.uses("MAKE") {
		makeValue := environment["MAKE"]
		switch {
		case len(commandReplays) != 0 &&
			(makeValue == "__LINUX_BZL_MAKE__" || makeValue == commandReplays[0].Name):
			environment["MAKE"] = commandReplays[0].Name
		case makeValue == "__LINUX_BZL_MAKE__":
			return "", fmt.Errorf("hermetic script reads recursive MAKE but has no discovered replay")
		case len(commandReplays) != 0:
			return "", fmt.Errorf(
				"hermetic script recursive replay has unexpected MAKE value %q", makeValue,
			)
		}
	} else {
		commandReplays = nil
	}
	// Stage solving derives object-tree visibility from this exact evaluated
	// recipe. Preserve the same frontier when argv lowering falls back to a
	// hermetic shell action. A compound command also needs the transitive files
	// behind direct inputs (for example the members of a thin archive), even
	// when the selected recipe does not consume the invocation's initial tree.
	// Otherwise `${tree:prep}` and relative object paths would see only the
	// immutable prep base and direct producer outputs.
	if compoundCommands != nil || b.planContext().UsesInitialObjectTree {
		inputs, err = b.compactKbuildWorkingTreeClosureInputs(target, match.profile, inputs)
		if err != nil {
			return "", fmt.Errorf("writable object-tree closure: %w", err)
		}
	} else {
		inputs, _, err = b.compactKbuildPathSensitiveArchiveClosureInputs(target, match.profile, inputs)
		if err != nil {
			return "", fmt.Errorf("path-sensitive archive closure: %w", err)
		}
	}
	refs, err := KbuildActionRoleRefs(template)
	if err != nil {
		return "", err
	}
	usedRoles := make([]string, 0, len(refs))
	script := template
	for _, sourceRef := range refs {
		ref, binding, valid := kbuildActionRoleBinding(sourceRef, b.actionScope())
		if !valid || !b.configuredActionRole(ref) {
			return "", fmt.Errorf("%s-scoped script references unavailable %s action role %q", b.actionScope(), sourceRef.Scope, sourceRef.Role)
		}
		usedRoles = append(usedRoles, binding)
		script = strings.ReplaceAll(script, KbuildActionRoleToken(sourceRef.Scope, sourceRef.Role), binding)
	}
	usedRoles = append(usedRoles, environmentRoles...)
	sort.Strings(usedRoles)
	usedRoles = slices.Compact(usedRoles)
	// Exact declared prerequisites are staged at their canonical relative paths
	// below the private working directory. Remaining source-selected tree paths
	// are passed as typed bindings to scriptrun, which substitutes only complete
	// ${tree:NAME} markers after decoding the evaluated script.
	if compoundCommands == nil {
		script, err = rewriteCompactKbuildScriptDeclaredTreeInputs(script, inputs)
		if err != nil {
			return "", err
		}
		script, err = rewriteCompactKbuildScriptInvocationInputs(script, match.profile, inputs)
		if err != nil {
			return "", err
		}
		script = strings.NewReplacer(
			compactKbuildActionSourceTreeMarker, "${tree:kernel}",
			compactKbuildActionHostDepsTreeMarker, "${tree:"+linuxProbeHostDepsRootName+"}",
		).Replace(script)
	}
	if strings.Contains(script, "${work:root}") ||
		strings.Contains(script, compactKbuildActionSourceTreeMarker) ||
		strings.Contains(script, compactKbuildActionObjectTreeMarker) ||
		strings.Contains(script, compactKbuildActionHostDepsTreeMarker) {
		return "", fmt.Errorf("hermetic script retains an unlowered private action placeholder")
	}
	scriptTrees, err := compactKbuildScriptTreeNames(script)
	if err != nil {
		return "", err
	}
	for _, tree := range scriptTrees {
		if !authorizedScriptTrees[tree] {
			return "", fmt.Errorf("evaluated script references unproven tree capability %q", tree)
		}
	}
	script = "#!/bin/sh\nset -e\n" + script + "\n"
	script, literalTreeOffsets, err = restoreCompactKbuildLiteralActionMarkers(script)
	if err != nil {
		return "", err
	}

	declaredOutputs, err := b.compactKbuildDeclaredOutputs(target, match)
	if err != nil {
		return "", err
	}
	declaredOutputs, err = compactKbuildPersistentCompilerOutputs(
		declaredOutputs,
		compilerOutputs.PersistentOutputs,
		b.planContext().OutputTree,
		func(persistent string) string {
			return b.compactKbuildRecipeIntermediateArtifactPath(
				match.profile, target, len(compilerCommands), persistent,
			)
		},
	)
	if err != nil {
		return "", err
	}
	observedWorkingPaths := map[string]bool{}
	for _, observation := range b.observedOutputs[target] {
		if observedPath := canonicalKbuildRulePath(observation.path); observedPath != "" {
			observedWorkingPaths[observedPath] = true
		}
	}
	additionalSideOutputs := make([]ActionPlanOutput, 0, len(sideOutputs))
	for _, sideOutput := range sideOutputs {
		// A runtime observation already owns this writable pathname and resolves
		// its actual producer after execution. Publishing the same path as a
		// command-owned output would bind one working file to two output slots.
		if observedWorkingPaths[sideOutput] {
			continue
		}
		duplicate := false
		for _, output := range declaredOutputs {
			if output.Tree == b.planContext().OutputTree && output.Path == sideOutput {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if err := validatePlanRelativePath("Kbuild compound side output", sideOutput); err != nil {
			return "", err
		}
		output := ActionPlanOutput{Tree: b.planContext().OutputTree, Path: sideOutput}
		if b.selectionBound {
			// Semantic side outputs are discovered only while materializing the
			// selected command, after the selection graph's native target owners
			// have been indexed. Give every selected writer an immutable physical
			// identity eagerly; the logical Path is still staged at the source-
			// visible pathname for downstream Kbuild commands.
			output.ArtifactPath = path.Join(
				".linux-bzl-side-outputs",
				compactKbuildSelectionArtifactVersionID(b.selection),
				sideOutput,
			)
		}
		additionalSideOutputs = append(additionalSideOutputs, output)
	}
	if len(additionalSideOutputs) != 0 {
		// Ordinary working outputs precede observed-state envelopes so adding or
		// filtering an observation never renumbers command-owned output slots.
		insert := len(declaredOutputs)
		for index, output := range declaredOutputs {
			if output.ObservedPath != "" {
				insert = index
				break
			}
		}
		withSideOutputs := make([]ActionPlanOutput, 0, len(declaredOutputs)+len(additionalSideOutputs))
		withSideOutputs = append(withSideOutputs, declaredOutputs[:insert]...)
		withSideOutputs = append(withSideOutputs, additionalSideOutputs...)
		withSideOutputs = append(withSideOutputs, declaredOutputs[insert:]...)
		declaredOutputs = withSideOutputs
	}
	node := b.actionNode("generate", compactKbuildScriptRunnerRole, declaredOutputs[0].Path)
	node.Outputs[0] = declaredOutputs[0]
	for _, output := range declaredOutputs[1:] {
		node.Outputs = append(node.Outputs, output)
	}
	scriptArgument := "-script_content_base64"
	scriptValue := base64.StdEncoding.EncodeToString([]byte(script))
	deferredScriptContent := kbuildDeferredContentTokenPattern.MatchString(script)
	if deferredScriptContent {
		if len(literalTreeOffsets) != 0 {
			return "", fmt.Errorf("deferred Kbuild content cannot precede offset-addressed literal tree markers")
		}
		// Keep the content template visible in the recipe. Its typed argument
		// transform substitutes only generated content, preserves downstream
		// tree placeholders as script bytes, and then base64-encodes the result.
		scriptValue = script
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "generate",
		Tool:   compactKbuildScriptRunnerRole,
		Arguments: []string{
			"-interpreter", "${tool:" + compactKbuildScriptRuntimeRole + "}",
			"-interpreter_arg", "sh",
			"-multicall", "${tool:" + compactKbuildScriptRuntimeRole + "}",
			"-tool", compactKbuildScriptRuntimeRole + "=${tool:" + compactKbuildScriptRuntimeRole + "}",
			scriptArgument, scriptValue,
		},
		WorkingDirectory:   "kbuild-script-" + escapeKbuildIdentifier(target),
		ExecutionDirectory: executionDirectory,
		WorkingDirectories: compilerOutputs.WorkingDirectories,
		WorkingInputs:      map[string]string{},
		WorkingOutputs:     map[string]string{},
		Environment:        environment,
		CommandReplays:     commandReplays,
	}
	if deferredScriptContent {
		recipe.ArgumentTransforms = []ActionRecipeArgumentTransform{{
			Index: len(recipe.Arguments) - 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
		}}
	}
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, match, nil)
	if err != nil {
		return "", fmt.Errorf("hermetic script automatic target: %w", err)
	}
	lexicalTarget, valid := ResolveCompactKbuildMakeTarget(match.profile, target, automatic.target)
	if !valid {
		return "", fmt.Errorf("hermetic script automatic target %q does not resolve graph target %q", automatic.target, target)
	}
	lexicalDirectories, err := compactKbuildLexicalTraversalWorkingDirectories(lexicalTarget, target)
	if err != nil {
		return "", fmt.Errorf("hermetic script automatic target: %w", err)
	}
	recipe.WorkingDirectories = append(recipe.WorkingDirectories, lexicalDirectories...)
	sort.Strings(recipe.WorkingDirectories)
	recipe.WorkingDirectories = slices.Compact(recipe.WorkingDirectories)
	recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, compactKbuildScriptRuntimeRole)
	for _, offset := range literalTreeOffsets {
		recipe.Arguments = append(recipe.Arguments, "-literal_tree_offset", strconv.Itoa(offset))
	}
	for _, applet := range runtimeApplets {
		recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, applet.role)
		recipe.Arguments = append(recipe.Arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
	}
	for _, role := range usedRoles {
		recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, role)
		recipe.Arguments = append(recipe.Arguments, "-tool", role+"=${tool:"+role+"}")
	}
	for _, tree := range scriptTrees {
		binding := "${tree:" + tree + "}"
		if tree == "prep" {
			// The captured object tree is mutable recipe state. Exact visible
			// prerequisites have already been staged below this private root;
			// directory tests and writes must observe that same root rather than
			// modifying the immutable preparation TreeArtifact.
			binding = "${work:root}"
		}
		recipe.Arguments = append(recipe.Arguments, "-tree", tree+"="+binding)
	}
	node.AuxiliaryTools = slices.Clone(recipe.AuxiliaryTools)
	for _, input := range inputs {
		if input.path == "" {
			continue
		}
		role := compactKbuildRuleInputEdgeRole(input, "prerequisite")
		key, err := appendCompactKbuildRecipeInput(&node, &recipe, input, role)
		if err != nil {
			return "", err
		}
		prefix := "source:"
		if input.producer != "" {
			prefix = "input:"
		}
		recipe.WorkingInputs[prefix+key] = input.path
		if executableProgramPaths[input.path] && input.producer != "" {
			recipe.ExecutableInputs = append(recipe.ExecutableInputs, key)
		}
	}
	for slot, output := range declaredOutputs {
		binding := fmt.Sprintf("%08d", slot)
		recipe.Outputs = append(recipe.Outputs, binding)
		recipe.WorkingOutputs[binding] = output.Path
	}
	if err := b.bindCompactKbuildSourceOverlayWorkingTree(match.profile, &recipe); err != nil {
		return "", fmt.Errorf("source overlay working tree: %w", err)
	}
	if err := appendReferencedPlanTrees(b.plan, &node, &recipe, recipe.Arguments...); err != nil {
		return "", err
	}
	producer, err := appendActionPlanNode(b.plan, node, recipe)
	if err != nil {
		return "", err
	}
	if pathSensitiveArchiveOutput {
		if err := b.plan.markPathSensitiveArchiveOutput(producer, 0); err != nil {
			return "", err
		}
	}
	for _, output := range declaredOutputs {
		b.memo[output.Path] = producer
	}
	return producer, nil
}

// Linux recipes occasionally spell standard utilities as /bin/name or
// /usr/bin/name. Those paths refer to the execution image, not a declared
// input. Project any such command head onto the selected script runtime's
// source-named applet; unsupported names then fail deterministically inside
// that runtime instead of consulting the worker image.
func compactKbuildAbsoluteRuntimeApplet(program string) (string, bool) {
	clean := path.Clean(program)
	directory := path.Dir(clean)
	if directory != "/bin" && directory != "/usr/bin" {
		return "", false
	}
	applet := path.Base(clean)
	if applet == "" || applet == "." || applet == ".." {
		return "", false
	}
	return applet, true
}

func kbuildDirectFilechkRecipeSupported(recipe []string) bool {
	filechk := ""
	for _, line := range recipe {
		line = strings.TrimSpace(line)
		if line == "" || isKbuildRecipeDirectorySetupExpression(line) {
			continue
		}
		name, _, ok := kbuildFilechkCall(line)
		if !ok || filechk != "" {
			return false
		}
		filechk = name
	}
	return filechk != ""
}

// compactKbuildDirectRecipeText lowers GNU Make's per-recipe execution
// prefixes after expansion. Kbuild commonly obtains '@' from $(Q), so looking
// only at the source spelling would leave a non-existent "@tool" executable
// in the evaluated action. '+' and '@' affect Make's own scheduling/logging and
// disappear; '-' is preserved as shell control flow because it changes failure
// semantics.
func compactKbuildDirectRecipeText(profile CompactKbuildProfile, value string) string {
	return compactKbuildDirectRecipeTextWithCanonicalizer(profile, value, compactKbuildProfileCanonicalRecipeText)
}

func compactKbuildRootedActionDirectRecipeText(profile CompactKbuildProfile, value string) (string, error) {
	return compactKbuildDirectRecipeTextWithCanonicalizer(profile, value, compactKbuildProfileEvaluatedRootedActionRecipeText), nil
}

func compactKbuildDirectRecipeTextWithCanonicalizer(
	profile CompactKbuildProfile,
	value string,
	canonicalize func(CompactKbuildProfile, string) string,
) string {
	return compactKbuildRecipeExecutionText(canonicalize(profile, value))
}

// compactKbuildRecipeExecutionText removes GNU Make's recipe-control prefix
// bytes from evaluated lines before they become shell source. @ and + control
// Make itself and disappear; - changes command failure semantics, which the
// generated shell preserves explicitly.
func compactKbuildRecipeExecutionText(value string) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		line = strings.TrimSpace(line)
		prefixLength := 0
		ignoreFailure := false
		for prefixLength < len(line) && strings.ContainsRune("+@-", rune(line[prefixLength])) {
			ignoreFailure = ignoreFailure || line[prefixLength] == '-'
			prefixLength++
		}
		line = line[prefixLength:]
		line = strings.TrimSpace(line)
		if ignoreFailure && line != "" {
			line = "{ " + line + "; } || true"
		}
		lines[index] = line
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// evaluatedKbuildDirectRecipeHasAction distinguishes a source-owned direct
// action from recipe-only control effects and directory setup. The decision is
// made from the exact evaluated recipe; no macro names, targets, or compiler
// families are encoded here.
func evaluatedKbuildDirectRecipeEffects(target string, match compactKbuildRuleMatch) (action, directorySetup bool, err error) {
	if kbuildDirectFilechkRecipeSupported(match.rule.Recipe) {
		return true, false, nil
	}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, nil)
	if err != nil {
		return false, false, err
	}
	context, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
	if err != nil {
		return false, false, err
	}
	for _, raw := range match.rule.Recipe {
		if isKbuildRecipeDirectorySetupExpression(raw) {
			directorySetup = true
			continue
		}
		if _, controlEffect, err := exactKbuildEvalRecipeBody(raw); err != nil {
			return false, false, err
		} else if controlEffect {
			continue
		}
		evaluated, err := evaluateCompactKbuildTextForMakeTarget(
			match.profile, target, match.lookupTarget, context.target, context.stem,
			context.normal, context.order, injected, raw, true,
		)
		if err != nil {
			return false, false, fmt.Errorf("target %q profile %q evaluates direct recipe %q: %w", target, match.profile.Name, raw, err)
		}
		evaluated = compactKbuildDirectRecipeText(match.profile, evaluated)
		for _, line := range strings.Split(evaluated, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == ":" || line == "true" {
				continue
			}
			commands, parseErr := parseCompactKbuildRecipe(line, context)
			if parseErr != nil {
				// It is still a selected direct action. The lowering site can
				// preserve shell structure through the hermetic script fallback.
				return true, directorySetup, nil
			}
			lineAction, lineDirectory, effectsErr := evaluatedKbuildParsedRecipeEffects(target, match, commands)
			if effectsErr != nil {
				return false, false, effectsErr
			}
			directorySetup = directorySetup || lineDirectory
			if lineAction {
				return true, directorySetup, nil
			}
		}
	}
	return false, directorySetup, nil
}

func evaluatedKbuildParsedRecipeEffects(
	target string,
	match compactKbuildRuleMatch,
	commands []compactKbuildRecipeCommand,
) (action, directorySetup bool, err error) {
	for _, command := range commands {
		command, err = rewriteCompactKbuildCommandAutomaticPaths(match.profile, target, match, command)
		if err != nil {
			return false, false, err
		}
		setup, setupErr := compactKbuildRecipeDirectorySetup(command)
		if setupErr != nil {
			return false, false, setupErr
		}
		if setup {
			directorySetup = true
			continue
		}
		for _, output := range compactKbuildRecipeRuleOutputs(target, match) {
			declared, ok, outputErr := compactKbuildRecipeDeclaredOutputForProfile(match.profile, command, output)
			if outputErr != nil {
				return false, false, outputErr
			}
			if ok && declared == output {
				return true, directorySetup, nil
			}
		}
		_, runtimeApplet := compactKbuildAbsoluteRuntimeApplet(command.program)
		if _, configured := parseKbuildActionRoleToken(command.program); configured || path.Base(command.program) != command.program && !runtimeApplet {
			// A configured action or selected source/generated program may carry
			// an output protocol opaque to argv lowering. Bare hermetic runtime
			// applets without rule-output evidence are control effects.
			return true, directorySetup, nil
		}
		if path.Base(command.program) == "xargs" && compactKbuildCommandCarriesAnyActionRole(command) {
			// xargs executes the configured capability carried in argv. The role
			// token is executable provenance here rather than diagnostic data.
			return true, directorySetup, nil
		}
	}
	return false, directorySetup, nil
}

func evaluatedKbuildDirectRecipeHasAction(target string, match compactKbuildRuleMatch) (bool, error) {
	action, _, err := evaluatedKbuildDirectRecipeEffects(target, match)
	return action, err
}

func isKbuildRecipeDirectorySetupExpression(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "$(shell ") || !strings.HasSuffix(line, ")") {
		return false
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "$(shell "), ")"))
	return strings.HasPrefix(inner, "mkdir -p ")
}

func expandKbuildDirectDirectorySetup(expression, target string) (compactKbuildRecipeCommand, error) {
	prefix := "mkdir -p "
	if !strings.HasPrefix(strings.TrimSpace(expression), prefix) {
		return compactKbuildRecipeCommand{}, fmt.Errorf("not mkdir -p")
	}
	arguments := []string{"-p"}
	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expression), prefix))
	payload = strings.NewReplacer("$(dir $@)", "\x01linux-bzl-target-dir\x02", "${dir $@}", "\x01linux-bzl-target-dir\x02").Replace(payload)
	for _, field := range kbuildFields(payload) {
		switch field {
		case "\x01linux-bzl-target-dir\x02":
			directory := path.Dir(target)
			if directory != "." {
				arguments = append(arguments, directory+"/")
			}
		default:
			return compactKbuildRecipeCommand{}, fmt.Errorf("unsupported mkdir directory expression %q", field)
		}
	}
	return compactKbuildRecipeCommand{program: "mkdir", arguments: arguments}, nil
}

func kbuildFilechkCall(line string) (string, []string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "$(") || !strings.HasSuffix(line, ")") {
		return "", nil, false
	}
	function, arguments, ok := splitMakeFunction(line[2 : len(line)-1])
	if !ok || function != "call" || len(arguments) < 2 || strings.TrimSpace(arguments[0]) != "filechk" {
		return "", nil, false
	}
	name := strings.TrimSpace(arguments[1])
	if name == "" || strings.ContainsAny(name, "$,() \t\r\n") {
		return "", nil, false
	}
	return name, append([]string(nil), arguments[2:]...), true
}

func (b *compactKbuildRulePlanBuilder) buildDirectRecipe(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (string, error) {
	if kbuildDirectFilechkRecipeSupported(match.rule.Recipe) {
		return b.buildDirectFilechkRecipe(target, match, inputs)
	}
	return b.buildGenericDirectRecipe(target, match, inputs)
}

func (b *compactKbuildRulePlanBuilder) buildGenericDirectRecipe(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (string, error) {
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
	if err != nil {
		return "", err
	}
	injected = compactKbuildActionTreeInjections(injected)
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return "", err
	}
	rootedLines := make([]string, 0, len(match.rule.Recipe))
	actualLines := make([]string, 0, len(match.rule.Recipe))
	for _, raw := range match.rule.Recipe {
		if isKbuildRecipeDirectorySetupExpression(raw) {
			continue
		}
		if _, controlEffect, err := exactKbuildEvalRecipeBody(raw); err != nil {
			return "", err
		} else if controlEffect {
			// The profile already contains this source-selected Make mutation.
			// Replaying it in the execution shell would be both redundant and
			// semantically invalid.
			continue
		}
		actual, err := evaluateCompactKbuildTextForMakeTarget(
			match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected, raw, true,
		)
		if err != nil {
			return "", fmt.Errorf("evaluate direct recipe line %q: %w", raw, err)
		}
		rooted, err := compactKbuildRootedActionDirectRecipeText(match.profile, actual)
		if err != nil {
			return "", fmt.Errorf("protect direct recipe line %q: %w", raw, err)
		}
		rootedLines = append(rootedLines, rooted)
		actualLines = append(actualLines, compactKbuildFinalizeRootedActionRecipeText(rooted))
	}
	template := strings.TrimSpace(strings.Join(actualLines, "\n"))
	rootedTemplate := strings.TrimSpace(strings.Join(rootedLines, "\n"))
	if template == "" {
		return "", fmt.Errorf("direct recipe evaluates to no executable action")
	}
	sideEffectCommands := compactKbuildRecipeSideEffectProjection(rootedLines, automatic)
	commands, parseErr := parseCompactKbuildRecipe(template, automatic)
	archiveCompound, archiveSource, archiveOpaque := false, false, false
	if parseErr == nil {
		archiveCompound, archiveSource, archiveOpaque, err = b.compactKbuildPathArchiveExecution(target, match, inputs, match.profile, nil, commands)
		if err != nil {
			return "", err
		}
	}
	if parseErr == nil && (compactKbuildRecipeHasPipeline(commands) || archiveCompound ||
		compactKbuildContainsProtectedLiteralActionMarker(rootedTemplate)) {
		return b.buildHermeticKbuildCompoundWithSideEffects(
			target, match, inputs, compactKbuildRecipeLineShells(rootedLines), commands, sideEffectCommands,
		)
	}
	if parseErr != nil {
		compound := compactKbuildRecipeLineShells(rootedLines)
		programs, programErr := compactKbuildCompoundProgramCommands(compound)
		if programErr == nil {
			archiveCompound, archiveSource, archiveOpaque, err = b.compactKbuildPathArchiveExecution(target, match, inputs, match.profile, nil, programs)
			if err != nil {
				return "", err
			}
		}
		needsCompound := compactKbuildRecipeTextHasPipeline(template) || archiveCompound || archiveOpaque
		if needsCompound && programErr != nil {
			return "", fmt.Errorf("direct recipe compound program discovery: %w", programErr)
		}
		if needsCompound {
			return b.buildHermeticKbuildScriptContext(
				target, match, inputs, compound, programs, sideEffectCommands,
			)
		}
		if archiveSource {
			return "", fmt.Errorf("direct path-sensitive source archive wrapper cannot be safely lowered after argv parse failure: %w", parseErr)
		}
	}
	return b.buildHermeticKbuildScript(target, match, inputs, rootedTemplate)
}

func (b *compactKbuildRulePlanBuilder) buildDirectFilechkRecipe(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
) (string, error) {
	filechk := ""
	var filechkArguments []string
	for _, line := range match.rule.Recipe {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isKbuildRecipeDirectorySetupExpression(line) {
			expression := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "$(shell "), ")"))
			command, err := expandKbuildDirectDirectorySetup(expression, target)
			if err != nil {
				return "", fmt.Errorf("direct recipe directory setup %q: %w", line, err)
			}
			if setup, setupErr := compactKbuildRecipeDirectorySetup(command); setupErr != nil || !setup {
				return "", fmt.Errorf("direct recipe directory setup %q: %v", line, setupErr)
			}
			continue
		}
		name, arguments, ok := kbuildFilechkCall(line)
		if !ok || filechk != "" {
			return "", fmt.Errorf("unsupported direct recipe line %q", line)
		}
		filechk = name
		filechkArguments = arguments
	}
	if filechk == "" {
		return "", fmt.Errorf("direct recipe has no filechk call")
	}
	variableName := "filechk_" + filechk
	variableNames := []string{variableName}
	injected, err := compactKbuildSourceScriptInjectionsForRuleTarget(target, match, inputs)
	if err != nil {
		return "", err
	}
	injected = compactKbuildActionTreeInjections(injected)
	injected["0"] = "filechk"
	injected["1"] = filechk
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, inputs, injected)
	if err != nil {
		return "", err
	}
	for index, argument := range filechkArguments {
		value, err := evaluateCompactKbuildTextForMakeTarget(
			match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
			automatic.normal, automatic.order, injected, argument, true,
		)
		if err != nil {
			return "", fmt.Errorf("evaluate filechk argument %d: %w", index+2, err)
		}
		injected[strconv.Itoa(index+2)] = value
	}
	values, err := evaluateCompactKbuildRuleVariablesRooted(target, match, inputs, injected, variableNames...)
	if err != nil {
		return "", err
	}
	rootedTemplate := strings.TrimSpace(values[variableName])
	if rootedTemplate == "" {
		return "", fmt.Errorf("evaluated variable %s is empty", variableName)
	}
	rootedTemplate = compactKbuildProfileEvaluatedRootedActionRecipeText(match.profile, rootedTemplate)
	template := compactKbuildFinalizeRootedActionRecipeText(rootedTemplate)
	parsedAutomatic := automatic
	lines, literal, err := parseCompactKbuildLiteralFilechk(template, parsedAutomatic)
	if err != nil {
		return "", fmt.Errorf("evaluated variable %s literal output: %w", variableName, err)
	}
	if literal && !compactKbuildContainsProtectedLiteralActionMarker(rootedTemplate) {
		return b.appendCompactKbuildLiteralFilechk(target, match, inputs, lines)
	}
	commands, parseErr := parseCompactKbuildRecipe(template, parsedAutomatic)
	if parseErr != nil {
		if !compactKbuildRecipeTextHasPipeline(template) {
			return "", fmt.Errorf("evaluated variable %s: %w", variableName, parseErr)
		}
		programs, programErr := compactKbuildCompoundProgramCommands(template)
		if programErr != nil {
			return "", fmt.Errorf("evaluated variable %s: argv parse: %v; compound program discovery: %w", variableName, parseErr, programErr)
		}
		compound := "{\n" + rootedTemplate + "\n} > " + compactKbuildActionObjectTreeMarker + "/" + target
		return b.buildHermeticKbuildScriptContext(target, match, inputs, compound, programs, programs)
	}
	if compactKbuildRecipeHasPipeline(commands) || compactKbuildContainsProtectedLiteralActionMarker(rootedTemplate) {
		// filechk owns the combined stdout of its source-defined command body.
		// Preserve a live pipeline and capture that stream inside the same staged
		// object tree instead of serializing it through intermediate actions.
		compound := "{\n" + rootedTemplate + "\n} > " + compactKbuildActionObjectTreeMarker + "/" + target
		return b.buildHermeticKbuildPipeline(target, match, inputs, compound, commands)
	}
	commands, err = captureCompactKbuildFilechkOutput(target, commands)
	if err != nil {
		return "", err
	}
	return b.appendCompactKbuildRecipe(target, match, inputs, values, commands)
}

func captureCompactKbuildFilechkOutput(
	target string,
	commands []compactKbuildRecipeCommand,
) ([]compactKbuildRecipeCommand, error) {
	streams := []int{}
	for index := range commands {
		if commands[index].connector == "|" || commands[index].stdout != "" {
			continue
		}
		streams = append(streams, index)
	}
	if len(streams) == 0 {
		return nil, fmt.Errorf("filechk recipe has no unredirected stdout stream")
	}
	if len(streams) == 1 {
		commands[streams[0]].stdout = target
		return commands, nil
	}
	parts := make([]string, 0, len(streams))
	for ordinal, index := range streams {
		part := path.Join(
			".linux-bzl-filechk", escapeKbuildIdentifier(target),
			fmt.Sprintf("part-%08d", ordinal),
		)
		commands[index].stdout = part
		parts = append(parts, part)
	}
	arguments := []string{}
	for _, part := range parts {
		arguments = append(arguments, "-input", part)
	}
	arguments = append(arguments, "-out", target)
	commands = append(commands, compactKbuildRecipeCommand{
		program: "actionfile", arguments: arguments, programToolRole: "actionfile",
	})
	return commands, nil
}

type compactKbuildSourceOverlayProjection struct {
	virtual  string
	prefix   string
	physical []string
}

func compactKbuildProfileSourceOverlayProjections(profile CompactKbuildProfile) []compactKbuildSourceOverlayProjection {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return nil
	}
	const sourceRoot = "__LINUX_BZL_SOURCE_TREE__"
	projections := []compactKbuildSourceOverlayProjection{}
	for virtual, rawRoot := range profile.evaluator.template.sourceRoots {
		prefix, nested := strings.CutPrefix(filepath.ToSlash(virtual), sourceRoot+"/")
		if !nested {
			continue
		}
		rawRoot = strings.TrimSpace(rawRoot)
		if rawRoot == "" {
			continue
		}
		canonical, err := canonicalCompactKbuildInvocationPath(prefix)
		if err != nil || canonical != prefix {
			continue
		}
		root, err := filepath.Abs(rawRoot)
		if err != nil {
			continue
		}
		physical := []string{filepath.ToSlash(filepath.Clean(root))}
		if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			resolved = filepath.ToSlash(filepath.Clean(resolved))
			if resolved != physical[0] {
				physical = append(physical, resolved)
			}
		}
		projections = append(projections, compactKbuildSourceOverlayProjection{
			virtual: sourceRoot + "/" + canonical, prefix: canonical, physical: physical,
		})
	}
	sort.Slice(projections, func(i, j int) bool {
		if len(projections[i].virtual) != len(projections[j].virtual) {
			return len(projections[i].virtual) > len(projections[j].virtual)
		}
		return projections[i].virtual < projections[j].virtual
	})
	return projections
}

// replaceCompactKbuildCanonicalRoot replaces a complete root or one followed
// by a slash. This also covers assignment words such as M=<root> while leaving
// a longer pathname component which merely shares the root's byte prefix
// untouched.
func replaceCompactKbuildCanonicalRoot(value, root, replacement string) string {
	if root == "" || !strings.Contains(value, root) {
		return value
	}
	var out strings.Builder
	for cursor := 0; cursor < len(value); {
		relative := strings.Index(value[cursor:], root)
		if relative < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start := cursor + relative
		end := start + len(root)
		out.WriteString(value[cursor:start])
		if end == len(value) || value[end] == '/' {
			out.WriteString(replacement)
		} else {
			out.WriteString(root)
		}
		cursor = end
	}
	return out.String()
}

// compactKbuildProfileCanonicalSourceOverlayRoots preserves the graph prefix
// of every configured nested source root. Final actions execute against the
// staged writable overlay, while values replayed into GNU Make retain the
// original virtual source-root spelling understood by its filesystem mapper.
func compactKbuildProfileCanonicalSourceOverlayRoots(
	profile CompactKbuildProfile,
	value, objectMarker string,
	physicalToVirtual, rewriteVirtual bool,
) string {
	type replacement struct {
		root   string
		marker string
	}
	replacements := []replacement{}
	for _, projection := range compactKbuildProfileSourceOverlayProjections(profile) {
		marker := projection.virtual
		if !physicalToVirtual {
			marker = strings.TrimSuffix(objectMarker, "/") + "/" + projection.prefix
		}
		if rewriteVirtual {
			replacements = append(replacements, replacement{root: projection.virtual, marker: marker})
		}
		for _, root := range projection.physical {
			if root == "" || root == "." || root == "/" {
				continue
			}
			replacements = append(replacements, replacement{root: root, marker: marker})
		}
	}
	sort.SliceStable(replacements, func(i, j int) bool {
		return len(replacements[i].root) > len(replacements[j].root)
	})
	for _, item := range replacements {
		value = replaceCompactKbuildCanonicalRoot(value, item.root, item.marker)
	}
	return value
}

// compactKbuildProfileCanonicalRecipeText removes the executor-specific
// spelling of captured source/object roots before a recipe is tokenized. Make
// variables can retain an absolute $(srctree) value even when target-context
// variables are injected with plan placeholders (notably direct filechk
// definitions). Replacing only complete root prefixes keeps arbitrary argv
// text untouched while making paths stable across local and remote execroots.
var compactKbuildCanonicalRecipeSentinelReplacer = strings.NewReplacer(
	"__LINUX_BZL_SOURCE_TREE__", "${tree:kernel}",
	"__LINUX_BZL_OBJECT_TREE__", "${tree:prep}",
	linuxProbeHostDepsSentinel, "${tree:"+linuxProbeHostDepsRootName+"}",
)

func compactKbuildProfileCanonicalRecipeText(profile CompactKbuildProfile, value string) string {
	value = compactKbuildProfileCanonicalSourceOverlayRoots(
		profile, value, "${tree:prep}", false, true,
	)
	value = compactKbuildCanonicalRecipeSentinelReplacer.Replace(value)
	return compactKbuildProfileCanonicalRecipeRoots(
		profile, value, "${tree:kernel}", "${tree:prep}", "${tree:"+linuxProbeHostDepsRootName+"}",
	)
}

// compactKbuildProfileCanonicalMakeValue removes physical checkout paths while
// retaining the evaluator sentinels understood by a recursively parsed Make
// invocation. Unlike final recipe canonicalization, this must not introduce
// ${tree:...} syntax because GNU Make would interpret it as a variable name.
func compactKbuildProfileCanonicalMakeValue(profile CompactKbuildProfile, value string) string {
	value = compactKbuildProfileCanonicalSourceOverlayRoots(profile, value, "", true, false)
	return compactKbuildProfileCanonicalRecipeRoots(
		profile, value, "__LINUX_BZL_SOURCE_TREE__", "__LINUX_BZL_OBJECT_TREE__", linuxProbeHostDepsSentinel,
	)
}

// compactKbuildProfileRootedActionRecipeText canonicalizes physical checkout
// paths while retaining action-only tree provenance. Public sentinel and
// ${tree:...} spellings in the Make source stay ordinary payload bytes; only
// the control-byte markers injected by this lowering pass are later projected
// onto execution paths.
func compactKbuildProfileRootedActionRecipeText(profile CompactKbuildProfile, value string) string {
	value = compactKbuildProfileCanonicalSourceOverlayRoots(
		profile, value, compactKbuildActionObjectTreeMarker, false, false,
	)
	value = compactKbuildProfileCanonicalRecipeRoots(
		profile, value, compactKbuildActionSourceTreeMarker, compactKbuildActionObjectTreeMarker,
		compactKbuildActionHostDepsTreeMarker,
	)
	return compactKbuildCollapseActionRootJoins(value)
}

// compactKbuildProfileEvaluatedRootedActionRecipeText is the source-evaluator
// boundary. Exact literal marker spellings were tagged while reading the
// Makefile, while invocation-environment and planner injections remain public
// roots which may be converted to private action provenance. Keep the
// lower-level rooted canonicalizer physical/private-only so arbitrary callers
// cannot grant authority with public bytes.
func compactKbuildProfileEvaluatedRootedActionRecipeText(profile CompactKbuildProfile, value string) string {
	value = compactKbuildProfileCanonicalSourceOverlayRoots(
		profile, value, compactKbuildActionObjectTreeMarker, false, true,
	)
	value = compactKbuildPrivateActionRootMarker(value)
	return compactKbuildProfileRootedActionRecipeText(profile, value)
}

func compactKbuildProfileCanonicalRecipeRoots(
	profile CompactKbuildProfile,
	value, sourceMarker, objectMarker, hostDepsMarker string,
) string {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return value
	}
	profile.evaluator.canonicalRecipeRootsOnce.Do(func() {
		profile.evaluator.canonicalRecipeRoots = compactKbuildCanonicalRecipeRoots(
			profile.evaluator.template.sourceRoots,
		)
	})
	for _, item := range profile.evaluator.canonicalRecipeRoots {
		marker := sourceMarker
		switch item.kind {
		case compactKbuildCanonicalRecipeRootObject:
			marker = objectMarker
		case compactKbuildCanonicalRecipeRootHostDeps:
			marker = hostDepsMarker
		}
		value = replaceCompactKbuildCanonicalRoot(value, item.root, marker)
	}
	return value
}

type compactKbuildCanonicalRecipeRootKind uint8

const (
	compactKbuildCanonicalRecipeRootHostDeps compactKbuildCanonicalRecipeRootKind = iota
	compactKbuildCanonicalRecipeRootObject
	compactKbuildCanonicalRecipeRootSource
)

type compactKbuildCanonicalRecipeRoot struct {
	root string
	kind compactKbuildCanonicalRecipeRootKind
}

func compactKbuildCanonicalRecipeRoots(sourceRoots map[string]string) []compactKbuildCanonicalRecipeRoot {
	lexical := map[string]compactKbuildCanonicalRecipeRootKind{}
	for name, rawRoot := range sourceRoots {
		kind := compactKbuildCanonicalRecipeRootSource
		if name == "__LINUX_BZL_OBJECT_TREE__" {
			kind = compactKbuildCanonicalRecipeRootObject
		} else if name == linuxProbeHostDepsSentinel {
			kind = compactKbuildCanonicalRecipeRootHostDeps
		}
		root := rawRoot
		if absolute, err := filepath.Abs(root); err == nil {
			root = absolute
		}
		root = filepath.ToSlash(filepath.Clean(root))
		if root == "" || root == "." || root == "/" {
			continue
		}
		// A planning checkout commonly represents both trees with the same
		// directory. Source evidence wins; generated object paths are already
		// rendered through the injected ${tree:prep} variables. The explicit
		// kind ordering also makes the otherwise-degenerate host/object overlap
		// deterministic.
		if previous, ok := lexical[root]; !ok || kind > previous {
			lexical[root] = kind
		}
	}

	canonical := maps.Clone(lexical)
	for root, kind := range lexical {
		resolved, err := filepath.EvalSymlinks(filepath.FromSlash(root))
		if err != nil {
			continue
		}
		resolved = filepath.ToSlash(filepath.Clean(resolved))
		if resolved == "" || resolved == "." || resolved == "/" {
			continue
		}
		if previous, ok := canonical[resolved]; !ok || kind > previous {
			canonical[resolved] = kind
		}
	}

	roots := make([]compactKbuildCanonicalRecipeRoot, 0, len(canonical))
	for root, kind := range canonical {
		roots = append(roots, compactKbuildCanonicalRecipeRoot{root: root, kind: kind})
	}
	sort.Slice(roots, func(i, j int) bool {
		if len(roots[i].root) != len(roots[j].root) {
			return len(roots[i].root) > len(roots[j].root)
		}
		if roots[i].root != roots[j].root {
			return roots[i].root < roots[j].root
		}
		return roots[i].kind > roots[j].kind
	})
	return roots
}

// parseCompactKbuildLiteralFilechk recognizes the shell fragment as a pure
// sequence of echo lines, optionally guarded by constant test expressions.
// Quoted shell metacharacters are argv data here, not control syntax: lowering
// directly to actionfile preserves those bytes without invoking a shell or an
// ambient echo binary. The grammar is deliberately generic and bounded; a
// non-literal filechk falls through to ordinary recipe lowering.
func parseCompactKbuildLiteralFilechk(value string, context compactKbuildAutomaticContext) ([]string, bool, error) {
	if lines, matched, err := parseCompactKbuildLiteralScalarPipeline(value, context); matched || err != nil {
		return lines, matched, err
	}
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, true, err
	}
	expanded := make([]compactKbuildRecipeToken, 0, len(tokens))
	for _, token := range tokens {
		if token.operator {
			expanded = append(expanded, token)
			continue
		}
		fields, err := expandKbuildAutomaticCommandField(token.value, context)
		if err != nil {
			return nil, false, nil
		}
		for _, field := range fields {
			field, err = expandPlanKbuildCommandField(field, context.target)
			if err != nil {
				return nil, false, nil
			}
			if containsUnresolvedPlanMakeReference(field) || containsUnmodeledKbuildDollar(field) {
				return nil, false, nil
			}
			field = strings.ReplaceAll(field, compactKbuildLiteralDollarToken, "$")
			if strings.ContainsAny(field, "\x00\r\n") {
				return nil, true, fmt.Errorf("literal output field contains a control character")
			}
			expanded = append(expanded, compactKbuildRecipeToken{value: field})
		}
	}
	first := 0
	for first < len(expanded) && expanded[first].operator && expanded[first].value == ";" {
		first++
	}
	if first == len(expanded) || expanded[first].operator || (expanded[first].value != "echo" && expanded[first].value != "if") {
		return nil, false, nil
	}
	parser := compactKbuildLiteralFilechkParser{tokens: expanded}
	lines, stop, err := parser.sequence(nil)
	if err != nil {
		return nil, false, nil
	}
	if stop != "" || parser.index != len(parser.tokens) {
		return nil, false, nil
	}
	if len(lines) == 0 {
		return nil, false, nil
	}
	return lines, true, nil
}

// parseCompactKbuildLiteralScalarPipeline evaluates the bounded shell form
// used when a filechk first computes literal text and then echoes it:
//
//	name=$(echo ... | cut -b -N); echo ... "${name}" ...
//
// Both programs are provided by the hermetic script runtime, but evaluating
// this pure byte projection directly avoids creating a shell action for data
// which is already fixed by the evaluated Make environment. The grammar is
// intentionally structural: variable names, payloads, and byte limits all
// come from source rather than a generated-header catalogue.
func parseCompactKbuildLiteralScalarPipeline(value string, context compactKbuildAutomaticContext) ([]string, bool, error) {
	trimmed := strings.TrimSpace(value)
	assignment := strings.Index(trimmed, "=$(")
	if assignment <= 0 {
		return nil, false, nil
	}
	name := strings.TrimSpace(trimmed[:assignment])
	if !validKbuildCommandEnvironmentName(name) || strings.ContainsAny(name, " \t\r\n") {
		return nil, false, nil
	}
	open := assignment + 1
	close, err := matchingShellCommandSubstitution(trimmed, open)
	if err != nil {
		return nil, true, err
	}
	inner := trimmed[open+2 : close]
	words, err := kbuildLiteralShellWords(inner)
	if err != nil {
		return nil, true, err
	}
	pipe := slices.Index(words, "|")
	if pipe < 2 || pipe+2 >= len(words) || slices.Index(words[pipe+1:], "|") >= 0 {
		return nil, true, fmt.Errorf("literal scalar assignment is not one echo-to-cut pipeline")
	}
	if words[0] != "echo" || words[pipe+1] != "cut" {
		return nil, true, fmt.Errorf("literal scalar assignment uses unsupported pipeline %q", words)
	}
	payload := strings.Join(words[1:pipe], " ")
	limit, ok := compactKbuildLiteralCutByteLimit(words[pipe+2:])
	if !ok {
		return nil, true, fmt.Errorf("literal scalar assignment has unsupported cut arguments %q", words[pipe+2:])
	}
	if len(payload) > limit {
		payload = payload[:limit]
	}
	tail := strings.TrimSpace(trimmed[close+1:])
	if !strings.HasPrefix(tail, ";") {
		return nil, true, fmt.Errorf("literal scalar assignment has no statement terminator")
	}
	tail = strings.TrimSpace(strings.TrimPrefix(tail, ";"))
	tail = strings.ReplaceAll(tail, "${"+name+"}", payload)
	tail = strings.ReplaceAll(tail, "$"+name, payload)
	lines, literal, err := parseCompactKbuildLiteralFilechk(tail, context)
	if err != nil {
		return nil, true, err
	}
	if !literal {
		return nil, true, fmt.Errorf("literal scalar assignment is not followed by literal output")
	}
	return lines, true, nil
}

func matchingShellCommandSubstitution(value string, start int) (int, error) {
	if start < 0 || start+1 >= len(value) || value[start:start+2] != "$(" {
		return 0, fmt.Errorf("invalid command substitution start")
	}
	depth := 1
	quote := byte(0)
	for index := start + 2; index < len(value); index++ {
		character := value[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '$' && index+1 < len(value) && value[index+1] == '(' {
			depth++
			index++
			continue
		}
		if character == ')' {
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated shell command substitution")
}

func kbuildLiteralShellWords(value string) ([]string, error) {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, err
	}
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.operator && token.value != "|" {
			return nil, fmt.Errorf("literal scalar pipeline uses shell operator %q", token.value)
		}
		words = append(words, strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$"))
	}
	return words, nil
}

func compactKbuildLiteralCutByteLimit(arguments []string) (int, bool) {
	selector := ""
	switch {
	case len(arguments) == 2 && arguments[0] == "-b":
		selector = arguments[1]
	case len(arguments) == 1 && strings.HasPrefix(arguments[0], "-b"):
		selector = strings.TrimPrefix(arguments[0], "-b")
	default:
		return 0, false
	}
	selector = strings.TrimSpace(selector)
	selector = strings.TrimPrefix(selector, "1-")
	selector = strings.TrimPrefix(selector, "-")
	limit, err := strconv.Atoi(selector)
	return limit, err == nil && limit > 0
}

type compactKbuildLiteralFilechkParser struct {
	tokens []compactKbuildRecipeToken
	index  int
}

func (p *compactKbuildLiteralFilechkParser) sequence(stops map[string]bool) ([]string, string, error) {
	lines := []string{}
	for p.index < len(p.tokens) {
		for p.index < len(p.tokens) && p.tokens[p.index].operator && p.tokens[p.index].value == ";" {
			p.index++
		}
		if p.index == len(p.tokens) {
			break
		}
		token := p.tokens[p.index]
		if !token.operator && stops[token.value] {
			return lines, token.value, nil
		}
		if token.operator {
			return nil, "", fmt.Errorf("unexpected shell operator %q", token.value)
		}
		if token.value == "if" {
			branch, err := p.conditional()
			if err != nil {
				return nil, "", err
			}
			lines = append(lines, branch...)
			continue
		}
		if token.value != "echo" {
			return nil, "", fmt.Errorf("unsupported literal-output program %q", token.value)
		}
		p.index++
		arguments := []string{}
		for p.index < len(p.tokens) && !p.tokens[p.index].operator {
			arguments = append(arguments, p.tokens[p.index].value)
			p.index++
		}
		if p.index < len(p.tokens) && p.tokens[p.index].value != ";" {
			return nil, "", fmt.Errorf("echo uses unsupported shell operator %q", p.tokens[p.index].value)
		}
		if len(arguments) != 0 && (arguments[0] == "-n" || arguments[0] == "-e" || arguments[0] == "-E") {
			return nil, "", fmt.Errorf("echo option %q is not literal line output", arguments[0])
		}
		lines = append(lines, strings.Join(arguments, " "))
	}
	return lines, "", nil
}

func (p *compactKbuildLiteralFilechkParser) conditional() ([]string, error) {
	p.index++ // if
	condition := []string{}
	for p.index < len(p.tokens) && !(p.tokens[p.index].operator && p.tokens[p.index].value == ";") {
		condition = append(condition, p.tokens[p.index].value)
		p.index++
	}
	if p.index == len(p.tokens) || p.tokens[p.index].value != ";" {
		return nil, fmt.Errorf("literal if condition has no statement terminator")
	}
	p.index++
	if p.index == len(p.tokens) || p.tokens[p.index].operator || p.tokens[p.index].value != "then" {
		return nil, fmt.Errorf("literal if condition has no then branch")
	}
	p.index++
	selected, err := evaluateCompactKbuildLiteralTest(condition)
	if err != nil {
		return nil, err
	}
	if !selected {
		stop, err := p.skipConditionalBranch()
		if err != nil {
			return nil, err
		}
		if stop == "fi" {
			p.index++
			return nil, nil
		}
		p.index++ // else
		elseLines, stop, err := p.sequence(map[string]bool{"fi": true})
		if err != nil {
			return nil, err
		}
		if stop != "fi" {
			return nil, fmt.Errorf("literal if condition has no fi")
		}
		p.index++
		return elseLines, nil
	}
	thenLines, stop, err := p.sequence(map[string]bool{"else": true, "fi": true})
	if err != nil {
		return nil, err
	}
	if stop == "else" {
		p.index++
		stop, err = p.skipConditionalBranch()
		if err != nil {
			return nil, err
		}
	}
	if stop != "fi" {
		return nil, fmt.Errorf("literal if condition has no fi")
	}
	p.index++
	return thenLines, nil
}

// skipConditionalBranch advances over a branch which a constant condition has
// proved unreachable. This mirrors shell control-flow without interpreting or
// executing the skipped commands. Nested conditionals remain balanced, and
// only a top-level else or fi terminates the skip.
func (p *compactKbuildLiteralFilechkParser) skipConditionalBranch() (string, error) {
	depth := 0
	for p.index < len(p.tokens) {
		token := p.tokens[p.index]
		if token.operator {
			p.index++
			continue
		}
		switch token.value {
		case "if":
			depth++
		case "fi":
			if depth == 0 {
				return "fi", nil
			}
			depth--
		case "else":
			if depth == 0 {
				return "else", nil
			}
		}
		p.index++
	}
	return "", fmt.Errorf("literal if condition has no fi")
}

func evaluateCompactKbuildLiteralTest(fields []string) (bool, error) {
	if len(fields) < 5 || fields[0] != "[" || fields[len(fields)-1] != "]" {
		return false, fmt.Errorf("unsupported literal if condition %q", fields)
	}
	operatorIndex := len(fields) - 3
	leftFields, operator, right := fields[1:operatorIndex], fields[operatorIndex], fields[operatorIndex+1]
	left, err := evaluateCompactKbuildLiteralTestOperand(leftFields)
	if err != nil {
		return false, fmt.Errorf("unsupported literal if condition %q: %w", fields, err)
	}
	switch operator {
	case "=", "==":
		return left == right, nil
	case "!=":
		return left != right, nil
	case "-eq", "-ne", "-gt", "-ge", "-lt", "-le":
		leftNumber, err := strconv.ParseInt(left, 10, 64)
		if err != nil {
			return false, fmt.Errorf("literal if left operand %q is not an integer", left)
		}
		rightNumber, err := strconv.ParseInt(right, 10, 64)
		if err != nil {
			return false, fmt.Errorf("literal if right operand %q is not an integer", right)
		}
		switch operator {
		case "-eq":
			return leftNumber == rightNumber, nil
		case "-ne":
			return leftNumber != rightNumber, nil
		case "-gt":
			return leftNumber > rightNumber, nil
		case "-ge":
			return leftNumber >= rightNumber, nil
		case "-lt":
			return leftNumber < rightNumber, nil
		default:
			return leftNumber <= rightNumber, nil
		}
	default:
		return false, fmt.Errorf("literal if has unsupported operator %q", operator)
	}
}

func evaluateCompactKbuildLiteralTestOperand(fields []string) (string, error) {
	if len(fields) == 1 {
		return fields[0], nil
	}
	// Linux uses this fully literal command substitution when generating
	// utsrelease.h. Evaluate the byte count directly so neither a shell nor host
	// echo/wc leaks into the build action.
	if len(fields) >= 7 && (fields[0] == "`echo" || (fields[0] == "`" && fields[1] == "echo")) {
		echo := 1
		if fields[0] == "`" {
			echo = 2
		}
		if echo >= len(fields) || fields[echo] != "-n" {
			return "", fmt.Errorf("command substitution is not echo -n")
		}
		pipe := slices.Index(fields, "|")
		if pipe <= echo+1 || pipe+3 >= len(fields) || fields[pipe+1] != "wc" || fields[pipe+2] != "-c" || fields[pipe+3] != "`" || pipe+4 != len(fields) {
			return "", fmt.Errorf("command substitution is not echo -n ... | wc -c")
		}
		payload := strings.Join(fields[echo+1:pipe], " ")
		return strconv.Itoa(len([]byte(payload))), nil
	}
	return "", fmt.Errorf("unsupported left operand %q", fields)
}

func (b *compactKbuildRulePlanBuilder) appendCompactKbuildLiteralFilechk(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	lines []string,
) (string, error) {
	declaredOutputs, err := b.compactKbuildDeclaredOutputs(target, match)
	if err != nil {
		return "", err
	}
	for _, output := range declaredOutputs[1:] {
		if output.ObservedPath == "" {
			return "", fmt.Errorf("literal filechk target %q cannot publish additional native output %q", target, output.Path)
		}
	}
	node := b.actionNode("generate", "actionfile", declaredOutputs[0].Path)
	node.Outputs = append([]ActionPlanOutput(nil), declaredOutputs...)
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"},
	}
	for slot := range declaredOutputs {
		recipe.Outputs = append(recipe.Outputs, planOrdinal(slot))
	}
	if len(declaredOutputs) > 1 {
		recipe.WorkingDirectory = "kbuild-literal-filechk-" + escapeKbuildIdentifier(target)
		recipe.WorkingOutputs = map[string]string{"00000000": target}
	}
	for _, input := range inputs {
		role := compactKbuildRuleInputEdgeRole(input, "prerequisite")
		if _, err := appendCompactKbuildRecipeInput(&node, &recipe, input, role); err != nil {
			return "", err
		}
	}
	for _, line := range lines {
		recipe.Arguments = append(recipe.Arguments, "-line", line)
	}
	return appendActionPlanNode(b.plan, node, recipe)
}

func lexCompactKbuildRecipe(value string) ([]compactKbuildRecipeToken, error) {
	tokens := []compactKbuildRecipeToken{}
	var word strings.Builder
	quote := byte(0)
	started := false
	wordStart := -1
	startWord := func(index int) {
		if !started {
			wordStart = index
		}
		started = true
	}
	flush := func(end int) {
		if !started {
			return
		}
		tokens = append(tokens, compactKbuildRecipeToken{value: word.String(), start: wordStart, end: end})
		word.Reset()
		started = false
		wordStart = -1
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			switch {
			case character == quote:
				quote = 0
				startWord(index)
			case character == '$' && quote == '\'':
				startWord(index)
				word.WriteString(compactKbuildLiteralDollarToken)
			case character == '\\' && quote == '"':
				startWord(index)
				if index+1 == len(value) {
					return nil, fmt.Errorf("trailing escape in quoted word")
				}
				next := value[index+1]
				switch next {
				case '\n':
					index++
				case '$':
					index++
					word.WriteString(compactKbuildLiteralDollarToken)
				case '`', '"', '\\':
					index++
					word.WriteByte(next)
				default:
					// POSIX preserves a backslash inside double quotes unless it
					// quotes one of the bytes handled above.
					word.WriteByte('\\')
				}
			default:
				startWord(index)
				word.WriteByte(character)
			}
			continue
		}
		switch {
		case character == '\'' || character == '"':
			startWord(index)
			quote = character
		case character == '\\':
			if index+1 == len(value) {
				return nil, fmt.Errorf("trailing escape")
			}
			index++
			// POSIX shells remove an unquoted backslash-newline pair before
			// tokenization. In particular, Kbuild command variables use it to
			// wrap one argv across Make source lines; it is not a command
			// boundary and contributes no byte to the word.
			if value[index] == '\n' {
				continue
			}
			startWord(index - 1)
			if value[index] == '$' {
				word.WriteString(compactKbuildLiteralDollarToken)
			} else {
				word.WriteByte(value[index])
			}
		case character == '\n':
			// A recursive Make variable can inject complete recipe lines into
			// one outer invocation. The shell executes its unquoted newlines as
			// command separators; treating them as ordinary whitespace can, for
			// example, turn a following `:` no-op into an operand of `rm -f`.
			flush(index)
			tokens = append(tokens, compactKbuildRecipeToken{
				value: ";", operator: true, start: index, end: index + 1,
			})
		case strings.ContainsRune(" \t\r", rune(character)):
			flush(index)
		case strings.ContainsRune(";|&<>()>", rune(character)):
			flush(index)
			operatorStart := index
			operator := string(character)
			if index+1 < len(value) {
				next := value[index+1]
				if (character == '&' && next == '&') || (character == '|' && next == '|') || (character == '>' && next == '>') {
					operator += string(next)
					index++
				}
			}
			tokens = append(tokens, compactKbuildRecipeToken{value: operator, operator: true, start: operatorStart, end: index + 1})
		default:
			startWord(index)
			word.WriteByte(character)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quoted word")
	}
	flush(len(value))
	return tokens, nil
}

// expandPlanKbuildCommandField expands the target-local Make path variables
// while preserving action-plan tree placeholders which have already been
// introduced by evaluated recipe canonicalization.
func expandPlanKbuildCommandField(value, target string) (string, error) {
	replacements := map[string]string{}
	protected := actionRecipePlaceholder.ReplaceAllStringFunc(value, func(placeholder string) string {
		match := actionRecipePlaceholder.FindStringSubmatch(placeholder)
		if len(match) != 3 || match[1] != "tree" {
			return placeholder
		}
		token := fmt.Sprintf("\x01linux-bzl-plan-tree-%08d\x02", len(replacements))
		replacements[token] = placeholder
		return token
	})
	expanded, err := expandPlanKbuildFlag(protected, nil, target)
	if err != nil {
		return "", err
	}
	for token, placeholder := range replacements {
		expanded = strings.ReplaceAll(expanded, token, placeholder)
	}
	return expanded, nil
}

func validKbuildCommandEnvironmentName(name string) bool {
	if name == "" || (name[0] < 'A' || name[0] > 'Z') && (name[0] < 'a' || name[0] > 'z') && name[0] != '_' {
		return false
	}
	for _, character := range name[1:] {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' {
			continue
		}
		return false
	}
	return true
}

func compactKbuildShellReservedCommand(value string) bool {
	switch value {
	case "{", "}", "!", "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "select", "function", "time":
		return true
	default:
		return false
	}
}

func parseCompactKbuildRecipe(value string, context compactKbuildAutomaticContext) ([]compactKbuildRecipeCommand, error) {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, err
	}
	expanded := []compactKbuildRecipeToken{}
	for _, token := range tokens {
		if token.operator {
			expanded = append(expanded, token)
			continue
		}
		fields, expandErr := expandKbuildAutomaticCommandField(token.value, context)
		if expandErr != nil {
			return nil, expandErr
		}
		for _, field := range fields {
			field, expandErr = expandPlanKbuildCommandField(field, context.target)
			if expandErr != nil {
				return nil, expandErr
			}
			if containsUnresolvedPlanMakeReference(field) || containsUnmodeledKbuildDollar(field) {
				return nil, fmt.Errorf("evaluated command retains unresolved field %q", field)
			}
			field = strings.ReplaceAll(field, compactKbuildLiteralDollarToken, "$")
			if err := validateShellFreeKbuildCommandField(field); err != nil {
				return nil, err
			}
			expanded = append(expanded, compactKbuildRecipeToken{value: field})
		}
	}

	commands := []compactKbuildRecipeCommand{}
	fields := []string{}
	stdin, stdout := "", ""
	stdoutRooted := false
	redirect := ""
	finish := func(connector string) error {
		if redirect != "" {
			return fmt.Errorf("redirection %q has no path", redirect)
		}
		if len(fields) == 0 {
			if connector == ";" || connector == "&&" {
				return nil
			}
			if connector == "" && len(commands) != 0 && commands[len(commands)-1].connector == ";" {
				// A terminal semicolon ends the preceding complete command; it
				// does not introduce an empty command after it.
				return nil
			}
			return fmt.Errorf("operator %q has no command", connector)
		}
		environment := map[string]string{}
		programIndex := 0
		for programIndex < len(fields) {
			name, assignment, ok := strings.Cut(fields[programIndex], "=")
			if !ok || !validKbuildCommandEnvironmentName(name) {
				break
			}
			environment[name] = assignment
			programIndex++
		}
		if programIndex == len(fields) {
			return fmt.Errorf("evaluated command has environment but no program")
		}
		if fields[programIndex] == "" {
			return fmt.Errorf("evaluated command has an empty program")
		}
		if compactKbuildShellReservedCommand(fields[programIndex]) {
			return fmt.Errorf("evaluated command uses shell reserved word %q", fields[programIndex])
		}
		commands = append(commands, compactKbuildRecipeCommand{
			environment:  environment,
			program:      fields[programIndex],
			arguments:    append([]string(nil), fields[programIndex+1:]...),
			stdin:        stdin,
			stdout:       stdout,
			stdoutRooted: stdoutRooted,
			connector:    connector,
		})
		fields, stdin, stdout, stdoutRooted = nil, "", "", false
		return nil
	}
	for _, token := range expanded {
		if !token.operator {
			if redirect != "" {
				rooted := compactKbuildRecipePathExplicitlyRooted(token.value)
				candidate, ok := compactKbuildCommandPath(token.value)
				if !ok {
					return nil, fmt.Errorf("redirection %s has unsafe path %q", redirect, token.value)
				}
				if redirect == "<" {
					stdin = candidate
				} else {
					stdout = candidate
					stdoutRooted = rooted
				}
				redirect = ""
				continue
			}
			fields = append(fields, token.value)
			continue
		}
		switch token.value {
		case "<", ">":
			if redirect != "" {
				return nil, fmt.Errorf("redirection %q has no path before %q", redirect, token.value)
			}
			if (token.value == "<" && stdin != "") || (token.value == ">" && stdout != "") {
				return nil, fmt.Errorf("command repeats %s redirection", token.value)
			}
			redirect = token.value
		case ";", "&&", "|":
			if err := finish(token.value); err != nil {
				return nil, err
			}
		case ">>":
			return nil, fmt.Errorf("append redirection is not reproducible")
		case "||", "&", "(", ")":
			return nil, fmt.Errorf("unsupported shell operator %q", token.value)
		default:
			return nil, fmt.Errorf("unsupported shell operator %q", token.value)
		}
	}
	if err := finish(""); err != nil {
		return nil, err
	}
	for index, command := range commands {
		if command.connector == "|" && index+1 == len(commands) {
			return nil, fmt.Errorf("pipeline has no command after pipe")
		}
		if index > 0 && commands[index-1].connector == "|" && command.stdin != "" {
			return nil, fmt.Errorf("pipeline command also has stdin redirection")
		}
		if command.connector == "|" && command.stdout != "" {
			return nil, fmt.Errorf("pipeline command also has stdout redirection")
		}
	}
	return commands, nil
}

func compactKbuildRecipeDeclaredOutput(command compactKbuildRecipeCommand, target string) (string, bool) {
	output, ok, err := compactKbuildRecipeDeclaredOutputForProfile(CompactKbuildProfile{}, command, target)
	return output, ok && err == nil
}

func compactKbuildRecipeDeclaredOutputForProfile(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	target string,
) (string, bool, error) {
	if output, ok, err := compactKbuildRecipeExplicitOutputForProfile(profile, command, target); err != nil || ok {
		return output, ok, err
	}
	if _, compiler := compactKbuildCommandCompilerRole(command); compiler {
		return "", false, nil
	}
	output, ok := compactKbuildRecipePositionalOutput(command, target)
	return output, ok, nil
}

// compactKbuildRecipePositionalOutput recognizes the filesystem contract of
// standard utilities whose destination is an ordinary operand. Merely
// mentioning $@ is not output evidence: status commands such as printf often
// display the target path without creating it. Configured tools and declared
// source/generated programs retain their opaque source-selected argv contract;
// their executable provenance distinguishes them from runtime applets.
func compactKbuildRecipePositionalOutput(command compactKbuildRecipeCommand, target string) (string, bool) {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return "", false
	}
	argumentIsTarget := func(argument string) (string, bool) {
		output, ok := compactKbuildCommandPath(argument)
		return output, ok && output == target
	}
	lastArgumentIsTarget := func() (string, bool) {
		if len(command.arguments) == 0 {
			return "", false
		}
		return argumentIsTarget(command.arguments[len(command.arguments)-1])
	}

	program := path.Base(command.program)
	if applet, runtime := compactKbuildAbsoluteRuntimeApplet(command.program); runtime {
		program = applet
	} else if _, configured := parseKbuildActionRoleToken(command.program); configured || program != command.program {
		for _, argument := range command.arguments {
			if output, ok := argumentIsTarget(argument); ok {
				return output, true
			}
		}
		return "", false
	}

	switch program {
	case "touch":
		// touch creates every file operand. Options cannot be mistaken for a
		// validated relative Kbuild target path.
		for _, argument := range command.arguments {
			if output, ok := argumentIsTarget(argument); ok {
				return output, true
			}
		}
		return "", false
	case "cp", "install", "ln", "mv":
		if program == "install" && compactKbuildRecipeInstallDirectoryMode(command) || compactKbuildRecipeTargetDirectoryMode(command) {
			return "", false
		}
		// These utilities spell the destination as their final operand.
		return lastArgumentIsTarget()
	case "mkfifo", "mknod":
		// These utilities create the first non-option path. A target match is
		// sufficient because option fields are not valid Kbuild paths.
		for _, argument := range command.arguments {
			if output, ok := argumentIsTarget(argument); ok {
				return output, true
			}
		}
		return "", false
	case "tee":
		// tee writes every non-option operand as well as stdout.
		for _, argument := range command.arguments {
			if output, ok := argumentIsTarget(argument); ok {
				return output, true
			}
		}
		return "", false
	default:
		return "", false
	}
}

func compactKbuildRecipeInstallDirectoryMode(command compactKbuildRecipeCommand) bool {
	if path.Base(command.program) != "install" {
		return false
	}
	for _, argument := range command.arguments {
		if argument == "-d" || argument == "--directory" {
			return true
		}
		if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") && strings.Contains(strings.TrimPrefix(argument, "-"), "d") {
			return true
		}
	}
	return false
}

func compactKbuildRecipeTargetDirectoryMode(command compactKbuildRecipeCommand) bool {
	for _, argument := range command.arguments {
		if argument == "-t" || argument == "--target-directory" || strings.HasPrefix(argument, "--target-directory=") {
			return true
		}
	}
	return false
}

// CompactKbuildRecipeWritesTarget reports whether evaluated recipe text
// contains a source-derived output binding for target. It reuses the generic
// recipe lowerer's argv, redirection, automatic-variable, and output-path
// parser. Unsupported or ambiguous shell text fails closed: discovery must not
// invent a file merely because an unrelated command ran before a sub-Make.
func CompactKbuildRecipeWritesTarget(recipe, target string) bool {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return false
	}
	if compactKbuildGeneratedTextRedirectsTarget(recipe, target) {
		return true
	}
	commands, err := parseCompactKbuildRecipe(recipe, compactKbuildAutomaticContext{target: target})
	if err != nil {
		return false
	}
	for _, command := range commands {
		if output, declared := compactKbuildRecipeDeclaredOutput(command, target); declared && canonicalKbuildRulePath(output) == target {
			return true
		}
	}
	return false
}

// CompactKbuildRecipeLiteralOutput returns the exact bytes written by a
// source-evaluated recipe consisting solely of a plain echo redirected to
// target. It deliberately recognizes only the runtime's bare echo applet and
// its two conventional absolute spellings. Environment assignments, echo
// options, input redirection, shell connectors, and source-owned executables
// all fail closed.
func CompactKbuildRecipeLiteralOutput(recipe, target string) (string, bool) {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return "", false
	}
	commands, err := parseCompactKbuildRecipe(recipe, compactKbuildAutomaticContext{target: target})
	if err != nil || len(commands) != 1 {
		return "", false
	}
	command := commands[0]
	if len(command.environment) != 0 || command.stdin != "" || command.stdout != target || command.connector != "" {
		return "", false
	}
	switch command.program {
	case "echo", "/bin/echo", "/usr/bin/echo":
	default:
		return "", false
	}
	if len(command.arguments) != 0 && strings.HasPrefix(command.arguments[0], "-") {
		return "", false
	}
	for _, argument := range command.arguments {
		// POSIX leaves echo's handling of backslashes implementation-defined.
		// Such text therefore cannot provide byte-exact content provenance.
		if strings.Contains(argument, `\`) {
			return "", false
		}
	}
	return strings.Join(command.arguments, " ") + "\n", true
}

// CompactKbuildRecipeSourceProjection reports when evaluated recipe text
// copies one exact immutable source candidate to target without changing its
// bytes. The returned source is content provenance only; callers must still
// retain the generated target's producer edge for execution ordering.
//
// A destination directory is accepted when the source and target basenames
// match, which models the standard cp/install directory form used by Kbuild's
// install_headers rules. Unsupported shell structure, ambiguous sources, and
// content-transforming options fail closed.
func CompactKbuildRecipeSourceProjection(recipe, target string, sourceCandidates []string) (string, bool) {
	target = canonicalKbuildRulePath(target)
	if target == "" {
		return "", false
	}
	sources := map[string]bool{}
	for _, candidate := range sourceCandidates {
		candidate = canonicalKbuildRulePath(candidate)
		if candidate != "" {
			sources[candidate] = true
		}
	}
	if len(sources) == 0 {
		return "", false
	}
	commands, err := parseCompactKbuildRecipe(recipe, compactKbuildAutomaticContext{target: target})
	if err != nil {
		return "", false
	}
	projection := ""
	for _, command := range commands {
		source, projected := compactKbuildRecipeCommandSourceProjection(command, target, sources)
		if projected {
			if projection != "" && projection != source {
				return "", false
			}
			projection = source
			continue
		}
		if output, declared := compactKbuildRecipeDeclaredOutput(command, target); declared && canonicalKbuildRulePath(output) == target {
			return "", false
		}
	}
	return projection, projection != ""
}

func compactKbuildRecipeCommandSourceProjection(
	command compactKbuildRecipeCommand,
	target string,
	sources map[string]bool,
) (string, bool) {
	if command.stdin != "" || command.connector == "|" {
		return "", false
	}
	program := command.program
	if applet, runtime := compactKbuildAbsoluteRuntimeApplet(command.program); runtime {
		program = applet
	} else if path.Base(program) != program {
		// A source or generated executable whose basename happens to be cp or
		// install does not inherit the selected runtime applet's semantics.
		return "", false
	}
	if program == "cat" {
		if command.stdout != target || len(command.arguments) != 1 {
			return "", false
		}
		source, ok := compactKbuildRecipePath(command.arguments[0])
		return source, ok && sources[source]
	}
	if (program != "cp" && program != "install") || command.stdout != "" {
		return "", false
	}
	if program == "install" && (compactKbuildRecipeInstallDirectoryMode(command) || compactKbuildRecipeTargetDirectoryMode(command)) {
		return "", false
	}
	operands, ok := compactKbuildSourceProjectionOperands(program, command.arguments)
	if !ok || len(operands) != 2 {
		return "", false
	}
	source, sourceOK := compactKbuildRecipePath(operands[0])
	destination, destinationOK := compactKbuildRecipePath(operands[1])
	if !sourceOK || !sources[source] || !destinationOK {
		return "", false
	}
	if destination == target {
		return source, true
	}
	if destination == path.Dir(target) && path.Base(source) == path.Base(target) {
		return source, true
	}
	return "", false
}

func compactKbuildSourceProjectionOperands(program string, arguments []string) ([]string, bool) {
	operands := []string{}
	options := true
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			options = false
			continue
		}
		if !options || !strings.HasPrefix(argument, "-") || argument == "-" {
			operands = append(operands, argument)
			continue
		}
		if program == "cp" {
			// These options affect replacement policy or metadata, not file bytes.
			if argument == "-f" || argument == "-n" || argument == "-p" || argument == "-u" || argument == "-v" ||
				argument == "--force" || argument == "--no-clobber" || argument == "--update" || argument == "--verbose" {
				continue
			}
			return nil, false
		}
		switch {
		case argument == "-m" || argument == "--mode" || argument == "-o" || argument == "--owner" || argument == "-g" || argument == "--group":
			if index+1 >= len(arguments) {
				return nil, false
			}
			index++
		case strings.HasPrefix(argument, "-m") && len(argument) > 2,
			strings.HasPrefix(argument, "--mode="),
			strings.HasPrefix(argument, "--owner="),
			strings.HasPrefix(argument, "--group="),
			argument == "-D", argument == "-p", argument == "-C", argument == "-T", argument == "-v",
			argument == "--preserve-timestamps", argument == "--compare", argument == "--no-target-directory", argument == "--verbose":
		default:
			// In particular, reject --strip/-s and directory/target-directory
			// modes. They either change bytes or do not identify one output file.
			return nil, false
		}
	}
	if len(operands) != 2 {
		return nil, false
	}
	return operands, true
}

func compactKbuildRecipeExplicitOutput(command compactKbuildRecipeCommand) (string, bool) {
	output, ok, err := compactKbuildRecipeExplicitOutputForProfile(CompactKbuildProfile{}, command)
	return output, ok && err == nil
}

func compactKbuildCommandCompilerRole(command compactKbuildRecipeCommand) (string, bool) {
	role := command.programToolRole
	if role == "" {
		if ref, configured := parseKbuildActionRoleToken(command.program); configured {
			role = ref.Role
		}
	}
	switch role {
	case "cc", "cxx", "rustc", "clippy":
		return role, true
	default:
		return "", false
	}
}

func compactKbuildRecipeExplicitOutputForProfile(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	knownGraphOutputs ...string,
) (string, bool, error) {
	if command.stdout != "" {
		return command.stdout, true, nil
	}
	if role, compiler := compactKbuildCommandCompilerRole(command); compiler {
		analysis, err := analyzeCompactKbuildCompilerOutputs(profile, role, command.arguments, knownGraphOutputs...)
		if err != nil {
			return "", false, err
		}
		return analysis.PrimaryOutput, analysis.PrimaryOutput != "", nil
	}
	options := true
	for index := 0; index < len(command.arguments); index++ {
		argument := command.arguments[index]
		if argument == "--" {
			options = false
			continue
		}
		if !options || (argument != "-o" && argument != "-out") || index+1 >= len(command.arguments) {
			continue
		}
		index++
		if output, ok := compactKbuildRecipePath(command.arguments[index]); ok {
			return output, true, nil
		}
	}
	return "", false, nil
}

func compactKbuildRecipePath(value string) (string, bool) {
	if strings.TrimSpace(value) != value {
		return "", false
	}
	for _, prefix := range []string{"${tree:kernel}/", "${tree:prep}/", "${tree:host}/", "${tree:bootstrap}/", "${tree:prehost}/", "${work:root}/"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	return compactKbuildCommandPath(value)
}

// normalizeCompactKbuildRecipeCommands removes only operations whose complete
// filesystem effect can be represented by the action graph. Directory setup
// is validated by compactKbuildRecipeDirectorySetup. A move is folded only
// when it renames the immediately preceding command's single declared output
// to the concrete rule target; arbitrary host filesystem moves remain
// rejected instead of becoming ambient tools.
func normalizeCompactKbuildRecipeCommands(target string, commands []compactKbuildRecipeCommand) ([]compactKbuildRecipeCommand, error) {
	normalized := make([]compactKbuildRecipeCommand, 0, len(commands))
	for commandIndex, command := range commands {
		if setup, err := compactKbuildRecipeDirectorySetup(command); err != nil {
			return nil, fmt.Errorf("command %d: %w", commandIndex, err)
		} else if setup {
			continue
		}
		if removal, err := compactKbuildRecipeStaleOutputRemoval(command, target, len(normalized) == 0); err != nil {
			return nil, fmt.Errorf("command %d: %w", commandIndex, err)
		} else if removal {
			continue
		}
		if command.program != "mv" {
			normalized = append(normalized, command)
			continue
		}
		if len(command.environment) != 0 || command.stdin != "" || command.stdout != "" || command.connector == "|" || len(command.arguments) != 2 {
			return nil, fmt.Errorf("command %d has unsupported move", commandIndex)
		}
		source, sourceOK := compactKbuildCommandPath(command.arguments[0])
		destination, destinationOK := compactKbuildCommandPath(command.arguments[1])
		if !sourceOK || !destinationOK || source == target || destination != target || len(normalized) == 0 {
			return nil, fmt.Errorf("command %d is not a temporary-to-target move", commandIndex)
		}
		previous := &normalized[len(normalized)-1]
		previousOutput, declared := compactKbuildRecipeDeclaredOutput(*previous, target)
		if !declared || previousOutput != source || previous.outputAlias != "" || previous.connector == "|" {
			return nil, fmt.Errorf("command %d move source %q is not the immediately preceding command output", commandIndex, source)
		}
		previous.outputAlias = destination
		previous.connector = command.connector
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("evaluated recipe has no filesystem-producing command")
	}
	return normalized, nil
}

// A declared output is absent when its first Bazel producer starts, so Linux'
// defensive `rm -f $@` before archive creation has no observable effect. Other
// exact `rm -f PATH` commands remain in the source-ordered command graph. They
// run below a private writable object-tree root and can therefore clean compiler
// depfiles (as cmd_and_fixdep does) without mutating an ambient filesystem.
func compactKbuildRecipeStaleOutputRemoval(command compactKbuildRecipeCommand, target string, beforeProducer bool) (bool, error) {
	if path.Base(command.program) != "rm" {
		return false, nil
	}
	removed, err := compactKbuildRecipeRemovalPaths(command)
	if err != nil {
		return false, err
	}
	return beforeProducer && len(removed) == 1 && removed[0].path == target, nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildRecipeIntermediateArtifactPath(
	profile CompactKbuildProfile,
	target string,
	commandIndex int,
	outputPath string,
) string {
	context := b.planContext()
	selectionProfile := profile.Name
	selectionTarget := target
	selectionStage := context.Stage
	if b != nil && b.selectionBound {
		selectionProfile = b.selection.profile
		selectionTarget = b.selection.target
		selectionStage = b.selection.stage
	}
	digest := sha256.Sum256([]byte(
		"linux-bzl-kbuild-command-output-v1\x00" +
			selectionProfile + "\x00" + selectionTarget + "\x00" + selectionStage + "\x00" +
			profile.Path + "\x00" + context.Stage + "\x00" + context.OutputTree + "\x00" + context.Product + "\x00" +
			target + "\x00" + strconv.Itoa(commandIndex) + "\x00" + outputPath,
	))
	return path.Join(
		".linux-bzl-intermediate", hex.EncodeToString(digest[:]),
		fmt.Sprintf("command-%08d%s", commandIndex, path.Ext(outputPath)),
	)
}

// compactKbuildRecipeCommandObservedOutputs gives every physical command in a
// split Make recipe an absolute-state output. The first command starts from the
// candidate's external frontier, each later command starts from the preceding
// command's state, and only the last command publishes the candidate-level
// capture consumed by side-output resolution.
func compactKbuildRecipeCommandObservedOutputs(
	target string,
	commandIndex int,
	final bool,
	observations []compactKbuildObservedOutput,
	previous []compactKbuildRuleInput,
) ([]compactKbuildObservedOutput, error) {
	if commandIndex < 0 {
		return nil, fmt.Errorf("negative command index %d", commandIndex)
	}
	if commandIndex == 0 && len(previous) != 0 {
		return nil, fmt.Errorf("first command unexpectedly has %d predecessor states", len(previous))
	}
	if commandIndex > 0 && len(previous) != len(observations) {
		return nil, fmt.Errorf(
			"command has %d predecessor states for %d observed paths",
			len(previous), len(observations),
		)
	}
	result := make([]compactKbuildObservedOutput, len(observations))
	for ordinal, original := range observations {
		observation := original
		observation.baseInputs = append([]compactKbuildRuleInput(nil), original.baseInputs...)
		if commandIndex > 0 {
			observation.baseInputs = []compactKbuildRuleInput{previous[ordinal]}
		}
		if !final {
			digest := sha256.Sum256([]byte(
				"linux-bzl-observed-command-state-v1\x00" + target + "\x00" +
					original.output.Tree + "\x00" + original.output.Path + "\x00" +
					actionPlanOutputArtifactPath(original.output) + "\x00" + original.path + "\x00" +
					strconv.Itoa(commandIndex) + "\x00" + strconv.Itoa(ordinal),
			))
			observation.output = ActionPlanOutput{
				Tree: original.output.Tree,
				Path: path.Join(
					compactKbuildSideOutputStateDirectory,
					"commands",
					hex.EncodeToString(digest[:])+".state",
				),
			}
		}
		result[ordinal] = observation
	}
	return result, nil
}

func compactKbuildRecipeObservedStateInputs(
	producer string,
	outputs []ActionPlanOutput,
	observations []compactKbuildObservedOutput,
) ([]compactKbuildRuleInput, error) {
	if len(observations) == 0 {
		return nil, nil
	}
	if producer == "" {
		return nil, fmt.Errorf("observed command has an empty producer")
	}
	states := make([]compactKbuildRuleInput, len(observations))
	for ordinal, observation := range observations {
		slot := -1
		for candidateSlot, output := range outputs {
			if output.Tree != observation.output.Tree || output.Path != observation.output.Path || output.ObservedPath != observation.path {
				continue
			}
			if slot >= 0 {
				return nil, fmt.Errorf("observed path %q has repeated output slots", observation.path)
			}
			slot = candidateSlot
		}
		if slot < 0 {
			return nil, fmt.Errorf(
				"observed path %q has no %s/%s state output",
				observation.path, observation.output.Tree, observation.output.Path,
			)
		}
		states[ordinal] = compactKbuildRuleInput{
			path:     actionPlanOutputArtifactPath(outputs[slot]),
			producer: producer,
			slot:     slot,
		}
	}
	return states, nil
}

func upsertCompactKbuildRuleInput(inputs []compactKbuildRuleInput, result compactKbuildRuleInput) []compactKbuildRuleInput {
	for index := range inputs {
		if inputs[index].path == result.path {
			inputs[index] = result
			return inputs
		}
	}
	return append(inputs, result)
}

func compactKbuildConfiguredToolRole(command compactKbuildRecipeCommand) (string, bool) {
	return command.programToolRole, command.programToolRole != ""
}

func (b *compactKbuildRulePlanBuilder) annotateSourceActionRoles(command *compactKbuildRecipeCommand) error {
	if command == nil {
		return fmt.Errorf("cannot annotate a nil Kbuild command")
	}
	expectedScope := b.actionScope()
	fields := []string{command.program, command.stdin, command.stdout}
	fields = append(fields, command.arguments...)
	for _, name := range sortedStringMapKeys(command.environment) {
		fields = append(fields, command.environment[name])
	}
	for _, field := range fields {
		refs, err := KbuildActionRoleRefs(field)
		if err != nil {
			return err
		}
		for _, sourceRef := range refs {
			ref, _, valid := kbuildActionRoleBinding(sourceRef, expectedScope)
			if !valid {
				return fmt.Errorf("%s-scoped recipe retains invalid %s action role %q", expectedScope, sourceRef.Scope, sourceRef.Role)
			}
			if !b.configuredActionRole(ref) {
				return fmt.Errorf("%s-scoped recipe references unconfigured %s action role %q", expectedScope, ref.Scope, ref.Role)
			}
		}
	}
	if sourceRef, ok := parseKbuildActionRoleToken(command.program); ok {
		ref, matches := kbuildActionRoleRefForScope(sourceRef, expectedScope)
		if !matches || !b.configuredActionRole(ref) {
			return fmt.Errorf("%s-scoped recipe selects unavailable %s action role %q", expectedScope, sourceRef.Scope, sourceRef.Role)
		}
		command.programToolRole = ref.Role
	}
	return nil
}

// rewriteCompactKbuildCommandActionRoleTokens turns configured tools embedded
// in argv or environment values into typed action-plan bindings. Program-role
// provenance is handled separately by compactKbuildConfiguredToolRole.
func rewriteCompactKbuildCommandActionRoleTokens(
	command *compactKbuildRecipeCommand,
	expectedScope string,
	configured []KbuildActionRoleRef,
) ([]string, error) {
	used := map[string]bool{}
	rewrite := func(value string) (string, error) {
		value, roles, err := rewriteKbuildActionRoleRefs(value, expectedScope, configured, true)
		if err != nil {
			return "", err
		}
		for _, role := range roles {
			used[role] = true
		}
		return value, nil
	}
	arguments := make([]string, len(command.arguments))
	for index, argument := range command.arguments {
		var err error
		arguments[index], err = rewrite(argument)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", index, err)
		}
	}
	environment := make(map[string]string, len(command.environment))
	for name, value := range command.environment {
		var err error
		environment[name], err = rewrite(value)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %w", name, err)
		}
	}
	command.arguments = arguments
	command.environment = environment
	roles := make([]string, 0, len(used))
	for role := range used {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles, nil
}

func appendCompactKbuildRecipeInput(
	node *ActionPlanNode,
	recipe *ActionRecipe,
	input compactKbuildRuleInput,
	role string,
) (string, error) {
	if input.producer != "" {
		key := fmt.Sprintf("%s:%08d", role, len(node.Inputs))
		node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: role, ProducerID: input.producer, Slot: input.slot})
		recipe.Inputs = append(recipe.Inputs, key)
		return key, nil
	}
	if input.sourceID != "" {
		key := fmt.Sprintf("%s:%08d", role, len(node.Sources))
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: role, SourceID: input.sourceID})
		recipe.Sources = append(recipe.Sources, key)
		return key, nil
	}
	return "", fmt.Errorf("input %q has no source or producer", input.path)
}

func compactKbuildRuleInputEdgeRole(input compactKbuildRuleInput, fallback string) string {
	if input.workingOnly {
		return compactKbuildWorkingClosureInputRole
	}
	if input.orderOnly {
		return "order-only"
	}
	return fallback
}

func (b *compactKbuildRulePlanBuilder) compactKbuildPathSensitiveArchiveClosureInputs(
	target string,
	profile CompactKbuildProfile,
	inputs []compactKbuildRuleInput,
) ([]compactKbuildRuleInput, bool, error) {
	if b == nil || b.plan == nil {
		return nil, false, fmt.Errorf("path-sensitive archive closure requires an action plan")
	}
	roots := make([]compactKbuildRuleInput, 0, len(inputs))
	for _, input := range inputs {
		if input.producer != "" && b.plan.isPathSensitiveArchiveOutput(input.producer, input.slot) {
			roots = append(roots, input)
		}
	}
	if len(roots) == 0 {
		return inputs, false, nil
	}
	expanded, err := b.compactKbuildWorkingTreeClosureInputsFromRoots(target, profile, inputs, roots)
	if err != nil {
		return nil, false, err
	}
	return expanded, true, nil
}

// compactKbuildCompilerLibraryInputs closes an evaluated compiler directory
// search over the ancestry of its declared prerequisite producers. Rustc may
// consume metadata emitted beside a prerequisite object without spelling the
// metadata file in argv or Make prerequisites, even after a later postprocessor
// has replaced that object. Reuse the working-tree lineage resolver, then retain
// only exact paths in the evaluated -L directories.
func (b *compactKbuildRulePlanBuilder) compactKbuildCompilerLibraryInputs(
	target string,
	profile CompactKbuildProfile,
	inputs []compactKbuildRuleInput,
	directories []string,
) ([]compactKbuildRuleInput, error) {
	if len(directories) == 0 {
		return inputs, nil
	}
	directorySet := make(map[string]bool, len(directories))
	for _, directory := range directories {
		directorySet[canonicalKbuildRulePath(directory)] = true
	}
	existingPaths := make(map[string]bool, len(inputs))
	roots := []compactKbuildRuleInput{}
	for _, input := range inputs {
		existingPaths[canonicalKbuildRulePath(input.path)] = true
		if input.producer != "" && !input.workingOnly {
			roots = append(roots, input)
		}
	}
	expanded, err := b.compactKbuildWorkingTreeClosureInputsFromRoots(target, profile, inputs, roots)
	if err != nil {
		return nil, err
	}
	for _, candidate := range expanded {
		candidatePath := canonicalKbuildRulePath(candidate.path)
		directory := canonicalKbuildRulePath(path.Dir(candidatePath))
		if directory == "." {
			directory = ""
		}
		if candidatePath == "" || existingPaths[candidatePath] || !directorySet[directory] {
			continue
		}
		candidate.workingOnly = true
		inputs = append(inputs, candidate)
		existingPaths[candidatePath] = true
	}
	// A preconfigured SDK is immutable source evidence rather than a producer
	// graph. In particular, rustc discovers sysroot crates through evaluated -L
	// directories without naming each rlib/rmeta in argv or Make prerequisites.
	// Close every exact, existing SDK file whose direct parent is one of those
	// directories; the evaluated compiler search path, not a crate/file table,
	// remains the authority for this dependency set.
	if b != nil && b.metadata != nil && b.metadata.preconfiguredObjectTree {
		if err := b.metadata.ensureActionPlanSourceNamespaceIndex(); err != nil {
			return nil, err
		}
		for _, candidatePath := range sortedStringMapKeys(b.metadata.exactSourcePaths) {
			candidatePath = canonicalKbuildRulePath(candidatePath)
			directory := canonicalKbuildRulePath(path.Dir(candidatePath))
			if directory == "." {
				directory = ""
			}
			if candidatePath == "" || existingPaths[candidatePath] || !directorySet[directory] {
				continue
			}
			exists, err := b.metadata.preconfiguredObjectTreeSourcePathExists(profile, candidatePath)
			if err != nil {
				return nil, fmt.Errorf("compiler library input %q: %w", candidatePath, err)
			}
			if !exists {
				continue
			}
			sourceID, err := b.metadata.ensureActionPlanSource(b.plan, candidatePath)
			if err != nil {
				return nil, fmt.Errorf("compiler library input %q: %w", candidatePath, err)
			}
			inputs = append(inputs, compactKbuildRuleInput{
				path: candidatePath, sourceID: sourceID, objectTree: true, workingOnly: true,
			})
			existingPaths[candidatePath] = true
		}
	}
	return inputs, nil
}

// compactKbuildCompilerPreparedObjectInputs adds exact compiler-read files to
// a private writable object tree without changing GNU Make's prerequisite
// semantics. The typed compiler operand proves the logical prepared-tree path;
// the current plan/profile still supplies the authoritative source or producer.
func (b *compactKbuildRulePlanBuilder) compactKbuildCompilerPreparedObjectInputs(
	inputs []compactKbuildRuleInput,
	paths []string,
) ([]compactKbuildRuleInput, error) {
	for _, pathname := range paths {
		pathname = canonicalKbuildRulePath(pathname)
		if err := validatePlanRelativePath("compiler prepared-object input", pathname); err != nil {
			return nil, err
		}
		present := false
		for _, input := range inputs {
			if canonicalKbuildRulePath(input.path) == pathname {
				present = true
				break
			}
		}
		var index int
		var err error
		inputs, index, err = b.ensureCommandInput(inputs, pathname, true)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", pathname, err)
		}
		if !present {
			inputs[index].workingOnly = true
		}
	}
	return inputs, nil
}

// compactKbuildCompilerPreparedObjectDirectoryInputs closes the exact
// preconfigured object-tree leaves below evaluated compiler include roots.
// The compiler flags are the authority for this boundary: unlike literal
// source scanning, it remains sound for macro-expanded include operands while
// avoiding an unconditional projection of the complete configured SDK.
func (b *compactKbuildRulePlanBuilder) compactKbuildCompilerPreparedObjectDirectoryInputs(
	profile CompactKbuildProfile,
	inputs []compactKbuildRuleInput,
	directories []string,
) ([]compactKbuildRuleInput, error) {
	if len(directories) == 0 || b == nil || b.metadata == nil || !b.metadata.preconfiguredObjectTree {
		return inputs, nil
	}
	if err := b.metadata.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return nil, err
	}
	canonicalDirectories := make([]string, 0, len(directories))
	for _, directory := range directories {
		directory = canonicalKbuildRulePath(directory)
		if directory != "" {
			if err := validatePlanRelativePath("compiler prepared-object directory", directory); err != nil {
				return nil, err
			}
		}
		canonicalDirectories = append(canonicalDirectories, directory)
	}
	sort.Strings(canonicalDirectories)
	canonicalDirectories = slices.Compact(canonicalDirectories)
	existingPaths := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		existingPaths[canonicalKbuildRulePath(input.path)] = true
	}
	for _, candidatePath := range sortedStringMapKeys(b.metadata.exactSourcePaths) {
		candidatePath = canonicalKbuildRulePath(candidatePath)
		if candidatePath == "" || existingPaths[candidatePath] || !slices.ContainsFunc(canonicalDirectories, func(directory string) bool {
			return directory == "" || strings.HasPrefix(candidatePath, directory+"/")
		}) {
			continue
		}
		exists, err := b.metadata.preconfiguredObjectTreeSourcePathExists(profile, candidatePath)
		if err != nil {
			return nil, fmt.Errorf("compiler prepared-object directory input %q: %w", candidatePath, err)
		}
		if !exists {
			continue
		}
		var index int
		inputs, index, err = b.ensureCommandInput(inputs, candidatePath, true)
		if err != nil {
			return nil, fmt.Errorf("compiler prepared-object directory input %q: %w", candidatePath, err)
		}
		inputs[index].workingOnly = true
		existingPaths[candidatePath] = true
	}
	return inputs, nil
}

func (b *compactKbuildRulePlanBuilder) appendCompactKbuildRecipe(
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	values map[string]string,
	commands []compactKbuildRecipeCommand,
) (string, error) {
	if compactKbuildRecipeHasPipeline(commands) {
		return "", fmt.Errorf("pipeline must be lowered as one compound action")
	}
	var err error
	commands, err = normalizeCompactKbuildRecipeCommands(target, commands)
	if err != nil {
		return "", err
	}
	_, sourceOverlay, err := compactKbuildSourceOverlayRoot(match.profile)
	if err != nil {
		return "", fmt.Errorf("resolve Kbuild source overlay: %w", err)
	}
	available := []compactKbuildRuleInput{}
	for _, input := range inputs {
		available = upsertCompactKbuildRuleInput(available, input)
	}
	ruleOutputs := compactKbuildRecipeRuleOutputs(target, match)
	ruleOutputSet := map[string]bool{}
	for _, output := range ruleOutputs {
		ruleOutputSet[output] = true
	}
	pathProducers := map[string]compactKbuildRuleInput{}
	for _, input := range available {
		pathProducers[input.path] = input
	}
	previousProducer := ""
	previousSlot := 0
	previousObservedStates := []compactKbuildRuleInput{}
	finalProducer := ""
	for commandIndex, command := range commands {
		// Preserve Make's exact argv before either automatic-variable binding or
		// invocation-relative input projection replaces a source-script spelling
		// with its canonical graph path. Script discovery must resolve that source
		// spelling in the Make invocation which produced it; execution still uses
		// the fully rewritten command below.
		sourceScriptArguments := slices.Clone(command.arguments)
		command, err = rewriteCompactKbuildCommandAutomaticPaths(match.profile, target, match, command)
		if err != nil {
			return "", fmt.Errorf("command %d automatic paths: %w", commandIndex, err)
		}
		command = rewriteCompactKbuildCommandInvocationInputs(match.profile, command, available)
		if err := b.annotateSourceActionRoles(&command); err != nil {
			return "", fmt.Errorf("command %d configured action-role provenance: %w", commandIndex, err)
		}
		compilerOutputs := compactKbuildCompilerOutputAnalysis{}
		compilerOutputsAnalyzed := false
		if command.programToolRole == "cc" || command.programToolRole == "cxx" || command.programToolRole == "bindgen" {
			command.arguments, err = rewriteCompactKbuildCompilerRelativeIncludes(
				match.profile, command.programToolRole, command.arguments,
			)
			if err != nil {
				return "", fmt.Errorf("command %d compiler include paths: %w", commandIndex, err)
			}
		}
		if command.programToolRole == "cc" || command.programToolRole == "cxx" {
			compilerOutputs, err = analyzeCompactKbuildCompilerOutputs(
				match.profile, command.programToolRole, command.arguments, ruleOutputs...,
			)
			if err != nil {
				return "", fmt.Errorf("command %d compiler outputs: %w", commandIndex, err)
			}
			compilerOutputsAnalyzed = true
			command.arguments = compilerOutputs.Arguments
			command = rewriteCompactKbuildCommandRootedRuleOutputs(target, match, command)
		} else if command.programToolRole == "rustc" || command.programToolRole == "clippy" {
			compilerOutputs, err = analyzeCompactKbuildCompilerOutputs(
				match.profile, command.programToolRole, command.arguments, ruleOutputs...,
			)
			if err != nil {
				return "", fmt.Errorf("command %d Rust compiler outputs: %w", commandIndex, err)
			}
			compilerOutputsAnalyzed = true
			command.arguments = compilerOutputs.Arguments
			command = rewriteCompactKbuildCommandRootedRuleOutputs(target, match, command)
			available, err = b.compactKbuildCompilerLibraryInputs(
				target, match.profile, available, compilerOutputs.LibrarySearchDirectories,
			)
			if err != nil {
				return "", fmt.Errorf("command %d compiler library inputs: %w", commandIndex, err)
			}
			for _, input := range available {
				if input.path != "" {
					pathProducers[input.path] = input
				}
			}
		} else {
			command = rewriteCompactKbuildCommandRootedRuleOutputs(target, match, command)
		}
		projectionCandidates := map[string]bool{}
		for _, input := range available {
			if input.path != "" {
				projectionCandidates[input.path] = true
			}
		}
		projectedInputPath, byteProjection := compactKbuildRecipeCommandSourceProjection(command, target, projectionCandidates)
		scriptInvocation, sourceScript, err := compactKbuildSourceScriptCommandWithSourceArguments(
			match.profile, command, sourceScriptArguments, values, b.actionScope(), b.metadata.actionRoles,
		)
		if err != nil {
			return "", fmt.Errorf("command %d: %w", commandIndex, err)
		}
		if sourceScript && scriptInvocation.interpreterRole != "" &&
			scriptInvocation.interpreterRole != compactKbuildScriptRuntimeRole &&
			!b.configuredActionRole(KbuildActionRoleRef{Scope: b.actionScope(), Role: scriptInvocation.interpreterRole}) {
			return "", fmt.Errorf(
				"command %d source script %q selects unavailable %s action role %q",
				commandIndex, scriptInvocation.scriptPath, b.actionScope(), scriptInvocation.interpreterRole,
			)
		}
		pathSensitiveArchiveOutput := !sourceScript && compactKbuildRecipeRetainsArchiveMemberPaths([]compactKbuildRecipeCommand{command})
		typedSourceArchive := false
		typedExecutionDirectory, typedObjectRoot := "", ""
		auxiliaryActionRoles := []string{}
		if !sourceScript {
			auxiliaryActionRoles, err = rewriteCompactKbuildCommandActionRoleTokens(&command, b.actionScope(), b.metadata.actionRoles)
			if err != nil {
				return "", fmt.Errorf("command %d configured action-role references: %w", commandIndex, err)
			}
		}
		builtinKind := ""
		if command.program == "cat" && len(command.arguments) == 1 && len(command.environment) == 0 && command.stdin == "" && command.stdout != "" {
			// A one-input cat redirected to one output is exactly a file copy.
			// Lower the filesystem semantics to the declared helper instead of
			// depending on an ambient coreutils installation.
			command.program = "actionfile"
			command.programToolRole = "actionfile"
			command.arguments = []string{"-input", command.arguments[0], "-out", command.stdout}
			command.stdout = ""
			builtinKind = "copy"
		}
		tool, configured := compactKbuildConfiguredToolRole(command)
		runtimeApplets, err := compactKbuildScriptRuntimeApplets(b.metadata.actionRoles, b.actionScope())
		if err != nil {
			return "", fmt.Errorf("command %d script runtime: %w", commandIndex, err)
		}
		programPath := ""
		programInput := compactKbuildRuleInput{}
		scriptInput := compactKbuildRuleInput{}
		if sourceScript {
			tool, configured = compactKbuildScriptRunnerRole, true
			var scriptIndex int
			available, scriptIndex, err = b.ensureDeclaredSourceInput(available, scriptInvocation.scriptPath)
			if err != nil {
				return "", fmt.Errorf("command %d source script %q: %w", commandIndex, scriptInvocation.scriptPath, err)
			}
			scriptInput = available[scriptIndex]
			if scriptInput.sourceID == "" || scriptInput.producer != "" {
				return "", fmt.Errorf("command %d CONFIG_SHELL payload %q is not an immutable source", commandIndex, scriptInvocation.scriptPath)
			}
			pathProducers[scriptInvocation.scriptPath] = scriptInput
		} else if !configured && path.Base(command.program) == command.program {
			if b.configuredActionRole(KbuildActionRoleRef{Scope: b.actionScope(), Role: command.program}) {
				return "", fmt.Errorf(
					"command %d invokes configured action role %q as a bare program without scoped source provenance",
					commandIndex, command.program,
				)
			}
			selectedAppletRole := ""
			for _, applet := range runtimeApplets {
				if applet.name == command.program {
					selectedAppletRole = applet.role
					break
				}
			}
			if selectedAppletRole != "" {
				// The selected executable is a multicall payload, so preserve the
				// source command name as its first argument. Selection is still
				// driven by the bare command read from the source recipe.
				tool, configured = selectedAppletRole, true
				command.arguments = append([]string{command.program}, command.arguments...)
			} else {
				// Kbuild recipes conventionally invoke POSIX utilities by applet
				// name. Execute any such bare name through the selected hermetic
				// base multicall runtime.
				tool, configured = compactKbuildScriptRuntimeRole, true
				command.arguments = append([]string{command.program}, command.arguments...)
			}
		} else if !configured {
			var ok, sourceProgram bool
			programPath, sourceProgram, ok = compactKbuildProfileCommandPath(match.profile, command.program)
			if !ok {
				return "", fmt.Errorf("command %d has non-hermetic program %q", commandIndex, command.program)
			}
			var programIndex int
			var err error
			available, programIndex, err = b.ensureCommandProgramInput(available, programPath, sourceProgram)
			if err != nil {
				return "", fmt.Errorf("command %d program %q: %w", commandIndex, programPath, err)
			}
			programInput = available[programIndex]
			if programInput.producer == "" {
				return "", fmt.Errorf("command %d program %q is not a generated executable", commandIndex, programPath)
			}
			tool = "generated"
		}
		typedOpaqueProgram := tool == "generated"
		typedResponseFile := !sourceScript && compactKbuildCommandUsesExactResponseFile(match.profile, command, available)
		if typedOpaqueProgram || typedResponseFile {
			typedExecutionDirectory, typedObjectRoot, err = compactKbuildTypedPrivateExecution(match.profile)
			if err != nil {
				return "", fmt.Errorf("command %d typed invocation execution: %w", commandIndex, err)
			}
		}
		// Source scripts always require a private writable cwd. The selected
		// recipe bit is the sole contract for consuming the invocation's initial
		// object-tree frontier; physical stage names carry no such policy. Opaque
		// generated programs also receive the exact staged tree because their path
		// behavior cannot be inferred from outer argv.
		preconfiguredCompilerInputs := b.metadata != nil && b.metadata.preconfiguredObjectTree &&
			(len(compilerOutputs.PreparedObjectInputs) != 0 || len(compilerOutputs.PreparedObjectIncludeFiles) != 0 ||
				len(compilerOutputs.PreparedObjectDirectories) != 0)
		workingObjectTree := sourceScript || typedOpaqueProgram || typedResponseFile || sourceOverlay ||
			b.planContext().UsesInitialObjectTree || len(b.generatedObjectTreeArtifacts) != 0 ||
			preconfiguredCompilerInputs
		if workingObjectTree {
			available, err = b.compactKbuildCompilerPreparedObjectDirectoryInputs(
				match.profile, available, compilerOutputs.PreparedObjectDirectories,
			)
			if err != nil {
				return "", fmt.Errorf("command %d compiler prepared-object directory inputs: %w", commandIndex, err)
			}
			if b.metadata != nil && b.metadata.preconfiguredObjectTree {
				available, err = b.compactKbuildCompilerPreparedObjectInputs(
					available, compilerOutputs.PreparedObjectIncludeFiles,
				)
				if err != nil {
					return "", fmt.Errorf("command %d compiler prepared-object include files: %w", commandIndex, err)
				}
			}
			available, err = b.compactKbuildCompilerPreparedObjectInputs(
				available, compilerOutputs.PreparedObjectInputs,
			)
			if err != nil {
				return "", fmt.Errorf("command %d compiler prepared-object inputs: %w", commandIndex, err)
			}
			available, err = b.compactKbuildWorkingTreeClosureInputs(target, match.profile, available)
			if err != nil {
				return "", fmt.Errorf("command %d writable object-tree closure: %w", commandIndex, err)
			}
			for _, input := range available {
				if input.path != "" {
					pathProducers[input.path] = input
				}
			}
		}

		writtenOutputPath := command.stdout
		stdout := writtenOutputPath != ""
		validationOnly := false
		if writtenOutputPath == "" {
			if compilerOutputsAnalyzed {
				writtenOutputPath = compilerOutputs.PrimaryOutput
			} else {
				writtenOutputPath, _, err = compactKbuildRecipeExplicitOutputForProfile(
					match.profile, command, ruleOutputs...,
				)
				if err != nil {
					return "", fmt.Errorf("command %d explicit output: %w", commandIndex, err)
				}
			}
		}
		if writtenOutputPath == "" && previousProducer != "" {
			if priorTarget, ok := pathProducers[target]; ok && priorTarget.producer != "" {
				// A command after the logical target already exists, with no
				// explicit output of its own, is an in-place mutation or validation.
				// Carry the staged target through it and give the command a private
				// stdout stamp so success remains an observable action result.
				writtenOutputPath = target
				validationOnly = true
			}
		}
		if writtenOutputPath == "" {
			writtenOutputPath, _, err = compactKbuildRecipeDeclaredOutputForProfile(match.profile, command, target)
			if err != nil {
				return "", fmt.Errorf("command %d declared output: %w", commandIndex, err)
			}
		}
		if writtenOutputPath == "" && sourceScript {
			// An immutable source script owns the selected rule target even when
			// the wrapper recipe does not repeat $@ in argv. The script bytes and
			// concrete rule, rather than a script-name table, establish ownership.
			writtenOutputPath = target
		}
		if writtenOutputPath == "" && len(commands) == 1 && (sourceScript || typedOpaqueProgram || command.programToolRole != "") {
			// The selected Make rule itself declares the recipe's target. Some
			// configured or source-owned tools carry that target inside an opaque
			// option instead of a standalone argv word. Runtime applets require
			// concrete filesystem-effect evidence and cannot use this fallback.
			writtenOutputPath = target
		}
		if writtenOutputPath == "" {
			return "", fmt.Errorf("command %d does not declare an output", commandIndex)
		}

		for _, field := range append(append([]string(nil), command.arguments...), command.stdin) {
			candidate, ok := compactKbuildProfileCommandOperandPath(match.profile, field)
			if !ok || ruleOutputSet[candidate] || candidate == writtenOutputPath {
				continue
			}
			if _, exists := pathProducers[candidate]; exists {
				continue
			}
			var index int
			var added bool
			var err error
			available, index, added, err = b.ensureOptionalCommandInput(available, candidate)
			if err != nil {
				return "", fmt.Errorf("command %d input %q: %w", commandIndex, candidate, err)
			}
			if added {
				pathProducers[candidate] = available[index]
			}
		}
		// Invocation-relative operands can only be bound after their canonical
		// graph path has been discovered above. Project those newly exact inputs
		// into argv/stdin before recipe bindings are emitted.
		command = rewriteCompactKbuildCommandInvocationInputs(match.profile, command, available)
		stagedInputs := available
		stagedInputs, _, err = b.compactKbuildPathSensitiveArchiveClosureInputs(target, match.profile, stagedInputs)
		if err != nil {
			return "", fmt.Errorf("command %d path-sensitive archive closure: %w", commandIndex, err)
		}
		projectedPathSensitiveArchive := false
		if byteProjection {
			for _, input := range stagedInputs {
				if input.path == projectedInputPath && input.producer != "" && b.plan.isPathSensitiveArchiveOutput(input.producer, input.slot) {
					projectedPathSensitiveArchive = true
					break
				}
			}
		}

		if err := validatePlanRelativePath("Kbuild command output", writtenOutputPath); err != nil {
			return "", err
		}
		logicalOutputPath := writtenOutputPath
		if command.outputAlias != "" {
			logicalOutputPath = command.outputAlias
		}
		if err := validatePlanRelativePath("Kbuild logical command output", logicalOutputPath); err != nil {
			return "", err
		}
		publishesTarget := commandIndex+1 == len(commands) && slices.Contains(ruleOutputs, logicalOutputPath)
		observedTarget := canonicalKbuildRulePath(target)
		commandObservations, err := compactKbuildRecipeCommandObservedOutputs(
			observedTarget,
			commandIndex,
			commandIndex+1 == len(commands),
			b.observedOutputs[observedTarget],
			previousObservedStates,
		)
		if err != nil {
			return "", fmt.Errorf("command %d observed state: %w", commandIndex, err)
		}
		declaredOutputs := []ActionPlanOutput{}
		if publishesTarget || len(commandObservations) != 0 {
			// compactKbuildDeclaredOutputs also binds each observed state's exact
			// predecessor frontier. Give it this physical command's state output,
			// leaving the builder's candidate-level final observation untouched.
			commandBuilder := *b
			commandBuilder.observedOutputs = make(map[string][]compactKbuildObservedOutput, len(b.observedOutputs))
			for candidate, observations := range b.observedOutputs {
				commandBuilder.observedOutputs[candidate] = observations
			}
			commandBuilder.observedOutputs[observedTarget] = commandObservations
			declaredOutputs, err = commandBuilder.compactKbuildDeclaredOutputs(target, match)
			if err != nil {
				return "", err
			}
		}
		observedOutputDescriptors := make([]ActionPlanOutput, 0, len(commandObservations))
		for _, output := range declaredOutputs {
			if output.ObservedPath != "" {
				observedOutputDescriptors = append(observedOutputDescriptors, output)
			}
		}
		if len(observedOutputDescriptors) != len(commandObservations) {
			return "", fmt.Errorf(
				"command %d declares %d observed states for %d requested paths",
				commandIndex, len(observedOutputDescriptors), len(commandObservations),
			)
		}
		graphOutputPath := logicalOutputPath
		graphArtifactPath := ""
		if !publishesTarget {
			graphArtifactPath = b.compactKbuildRecipeIntermediateArtifactPath(
				match.profile, target, commandIndex, logicalOutputPath,
			)
		}
		primaryOutputDescriptor := ActionPlanOutput{
			Tree: b.planContext().OutputTree, Path: graphOutputPath, ArtifactPath: graphArtifactPath,
		}
		outputDescriptors := []ActionPlanOutput{primaryOutputDescriptor}
		if publishesTarget {
			outputDescriptors = append([]ActionPlanOutput(nil), declaredOutputs...)
		} else {
			for _, output := range observedOutputDescriptors {
				outputDescriptors = append(outputDescriptors, output)
			}
		}
		outputDescriptors, err = compactKbuildPersistentCompilerOutputs(
			outputDescriptors,
			compilerOutputs.PersistentOutputs,
			b.planContext().OutputTree,
			func(persistent string) string {
				return b.compactKbuildRecipeIntermediateArtifactPath(
					match.profile, target, commandIndex, persistent,
				)
			},
		)
		if err != nil {
			return "", fmt.Errorf("command %d persistent compiler outputs: %w", commandIndex, err)
		}
		outputPaths := make([]string, 0, len(outputDescriptors))
		logicalOutputPaths := make([]string, 0, len(outputDescriptors))
		for _, output := range outputDescriptors {
			outputPaths = append(outputPaths, output.Path)
			if output.ObservedPath == "" {
				logicalOutputPaths = append(logicalOutputPaths, output.Path)
			}
		}
		if publishesTarget {
			for _, grouped := range logicalOutputPaths {
				if err := validatePlanRelativePath("grouped Kbuild output", grouped); err != nil {
					return "", err
				}
			}
			if stdout && compactKbuildRuleHasGroupedOutputs(match.rule) && len(ruleOutputs) != 1 {
				return "", fmt.Errorf("grouped Kbuild recipe cannot direct one stdout stream to %d outputs", len(logicalOutputPaths))
			}
		}
		if validationOnly && len(logicalOutputPaths) != 1 {
			return "", fmt.Errorf("command %d cannot validate %d grouped Kbuild outputs", commandIndex, len(logicalOutputPaths))
		}
		inPlaceInput, inPlace := pathProducers[writtenOutputPath]
		inPlacePathSensitiveArchive := inPlace && inPlaceInput.producer != "" && b.plan.isPathSensitiveArchiveOutput(inPlaceInput.producer, inPlaceInput.slot)

		kind := "generate"
		if builtinKind != "" {
			kind = builtinKind
		} else if (tool == "cc" || tool == "cxx") && toolaction.InvocationContractRole(tool, command.arguments) != tool {
			kind = "link-driver"
		} else if tool == "cc" || tool == "cxx" || tool == "as" {
			kind = "compile"
		} else if tool == "ld" {
			kind = "link-relocatable"
		} else if tool == "ar" {
			kind = "archive"
		} else if tool == "objcopy" {
			kind = "copy"
		}
		nodeTool := tool
		if tool == "generated" {
			nodeTool = "generated"
		}
		node := b.actionNode(kind, nodeTool, outputPaths[0])
		node.Outputs[0] = outputDescriptors[0]
		for _, output := range outputDescriptors[1:] {
			node.Outputs = append(node.Outputs, output)
		}
		recipe := ActionRecipe{
			Schema:             LinuxKernelPlanSchema,
			Kind:               kind,
			Tool:               tool,
			WorkingDirectory:   "kbuild-command-" + escapeKbuildIdentifier(target) + fmt.Sprintf("-%08d", commandIndex),
			ExecutionDirectory: typedExecutionDirectory,
			WorkingDirectories: slices.Clone(compilerOutputs.WorkingDirectories),
			WorkingInputs:      map[string]string{},
		}
		if sourceScript {
			var environmentRoles []string
			recipe.Environment, environmentRoles, err = compactKbuildSourceScriptEnvironment(
				target, match, inputs, scriptInvocation.environment, scriptInvocation.environmentUsage,
				b.actionScope(), b.metadata.actionRoles,
			)
			if err != nil {
				return "", fmt.Errorf("command %d source-script environment: %w", commandIndex, err)
			}
			typedSourceArchive = slices.Contains(scriptInvocation.toolRoles, "ar") || slices.Contains(environmentRoles, "ar")
			if typedSourceArchive {
				typedExecutionDirectory, typedObjectRoot, err = compactKbuildTypedPrivateExecution(match.profile)
				if err != nil {
					return "", fmt.Errorf("command %d path-sensitive source archive wrapper: %w", commandIndex, err)
				}
				recipe.ExecutionDirectory = typedExecutionDirectory
				for name, value := range recipe.Environment {
					recipe.Environment[name] = compactKbuildTypedWorkingArgument(value, typedObjectRoot, nil)
				}
			}
			scriptInvocation.toolRoles = append(scriptInvocation.toolRoles, environmentRoles...)
			sort.Strings(scriptInvocation.toolRoles)
			scriptInvocation.toolRoles = slices.Compact(scriptInvocation.toolRoles)
			recipe.CommandReplays, err = compactKbuildSourceScriptCommandReplays(match.profile, target)
			if err != nil {
				return "", fmt.Errorf("command %d source-script recursive replay: %w", commandIndex, err)
			}
			if typedSourceArchive {
				for replayIndex := range recipe.CommandReplays {
					for invocationIndex := range recipe.CommandReplays[replayIndex].Invocations {
						arguments := recipe.CommandReplays[replayIndex].Invocations[invocationIndex].Arguments
						for argumentIndex, argument := range arguments {
							arguments[argumentIndex] = compactKbuildTypedWorkingArgument(argument, typedObjectRoot, nil)
						}
					}
				}
			}
			if len(recipe.CommandReplays) != 0 {
				recipe.Environment["MAKE"] = recipe.CommandReplays[0].Name
			}
			recipe.AuxiliaryTools = scriptInvocation.auxiliaryToolRoles(runtimeApplets)
			node.AuxiliaryTools = slices.Clone(recipe.AuxiliaryTools)
		} else {
			var environmentRoles []string
			recipe.Environment, environmentRoles, err = compactKbuildActionEnvironment(
				target, match, inputs, command.environment, b.actionScope(), b.metadata.actionRoles,
			)
			if err != nil {
				return "", fmt.Errorf("command %d source-exported environment: %w", commandIndex, err)
			}
			auxiliaryActionRoles = append(auxiliaryActionRoles, environmentRoles...)
			sort.Strings(auxiliaryActionRoles)
			auxiliaryActionRoles = slices.Compact(auxiliaryActionRoles)
			if workingObjectTree {
				for name, value := range recipe.Environment {
					value = compactKbuildSourceScriptWorkingValue(value)
					if typedOpaqueProgram || typedResponseFile {
						value = compactKbuildTypedWorkingArgument(value, typedObjectRoot, nil)
					}
					recipe.Environment[name] = value
				}
			}
			for _, role := range auxiliaryActionRoles {
				if role != tool {
					recipe.AuxiliaryTools = append(recipe.AuxiliaryTools, role)
				}
			}
			node.AuxiliaryTools = slices.Clone(recipe.AuxiliaryTools)
		}
		for slot := range outputPaths {
			recipe.Outputs = append(recipe.Outputs, fmt.Sprintf("%08d", slot))
		}

		bindings := map[string]string{}
		for _, input := range stagedInputs {
			if sourceScript && scriptInvocation.describesSource(input.path) && input.sourceID == scriptInput.sourceID {
				continue
			}
			if tool == "generated" && input.path == programPath && input.producer == programInput.producer && input.slot == programInput.slot {
				continue
			}
			if input.path == "" || bindings[input.path] != "" {
				continue
			}
			fallbackRole := "prerequisite"
			if len(commands) == 1 && len(inputs) == 1 && input.path == inputs[0].path {
				fallbackRole = "object"
			}
			role := compactKbuildRuleInputEdgeRole(input, fallbackRole)
			key, err := appendCompactKbuildRecipeInput(&node, &recipe, input, role)
			if err != nil {
				return "", err
			}
			prefix := "source:"
			if input.producer != "" {
				prefix = "input:"
			}
			recipe.WorkingInputs[prefix+key] = input.path
			if sourceScript && input.producer != "" && b.compactKbuildInputIsHostToolOutput(input) {
				recipe.ExecutableInputs = append(recipe.ExecutableInputs, key)
			}
			bindings[input.path] = "${" + prefix + key + "}"
		}
		if previousProducer != "" {
			alreadyOrdered := false
			for _, input := range node.Inputs {
				if input.ProducerID == previousProducer && input.Slot == previousSlot {
					alreadyOrdered = true
					break
				}
			}
			if !alreadyOrdered {
				key := fmt.Sprintf("sequence:%08d", len(node.Inputs))
				node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: "sequence", ProducerID: previousProducer, Slot: previousSlot})
				recipe.Inputs = append(recipe.Inputs, key)
			}
		}
		if command.stdin != "" {
			input, ok := pathProducers[command.stdin]
			if !ok {
				return "", fmt.Errorf("command %d stdin %q has no evaluated input", commandIndex, command.stdin)
			}
			key, err := appendCompactKbuildRecipeInput(&node, &recipe, input, "stdin")
			if err != nil {
				return "", err
			}
			if input.producer != "" {
				recipe.Stdin = "input:" + key
			} else {
				recipe.Stdin = "source:" + key
			}
		}
		if tool == "generated" {
			key, err := appendCompactKbuildRecipeInput(&node, &recipe, programInput, "program")
			if err != nil {
				return "", err
			}
			recipe.Tool = "input:" + key
			recipe.ExecutableInputs = append(recipe.ExecutableInputs, key)
		}

		if sourceScript {
			scriptBinding, err := appendCompactKbuildRecipeInput(&node, &recipe, scriptInput, "script")
			if err != nil {
				return "", err
			}
			recipe.Arguments, err = scriptInvocation.recipeArguments(scriptBinding, values, runtimeApplets)
			if err != nil {
				return "", fmt.Errorf("command %d source-script arguments: %w", commandIndex, err)
			}
		} else {
			recipe.Arguments = append([]string(nil), command.arguments...)
		}
		typedWorkingExecution := typedSourceArchive || typedOpaqueProgram || typedResponseFile
		typedLogicalPaths := map[string]bool{}
		typedArgumentStart := len(recipe.Arguments)
		if typedWorkingExecution {
			for _, input := range stagedInputs {
				if pathname := canonicalKbuildRulePath(input.path); pathname != "" {
					typedLogicalPaths[pathname] = true
				}
			}
			for _, pathname := range logicalOutputPaths {
				typedLogicalPaths[pathname] = true
			}
			typedLogicalPaths[writtenOutputPath] = true
			typedArgumentStart = 0
			if typedSourceArchive {
				if delimiter := slices.Index(recipe.Arguments, "--"); delimiter >= 0 {
					typedArgumentStart = delimiter + 1
				}
			}
		}
		for index, argument := range recipe.Arguments {
			if typedWorkingExecution && index >= typedArgumentStart {
				recipe.Arguments[index] = compactKbuildTypedWorkingArgument(argument, typedObjectRoot, typedLogicalPaths)
				continue
			}
			if workingObjectTree {
				argument = compactKbuildSourceScriptWorkingValue(argument)
				recipe.Arguments[index] = argument
			}
			argumentPath, pathArgument := compactKbuildRecipePath(argument)
			switch {
			case sourceScript && pathArgument && argumentPath == writtenOutputPath:
				recipe.Arguments[index] = writtenOutputPath
			case pathArgument && argumentPath == writtenOutputPath && !stdout && len(outputPaths) == 1 && !inPlace && command.outputAlias == "":
				recipe.Arguments[index] = "${output:00000000}"
			case pathArgument && argumentPath == writtenOutputPath:
				// The immutable producer was staged at its logical path above.
				// Keep aliased and same-path argv relative to the private working
				// directory; the distinct graph output is collected afterwards.
				recipe.Arguments[index] = writtenOutputPath
			case bindings[argument] != "":
				recipe.Arguments[index] = bindings[argument]
			case pathArgument && bindings[argumentPath] != "":
				// Evaluated Kbuild variables spell object inputs through $(obj),
				// which canonicalizes to ${tree:prep}/path before tokenization.
				// A concrete producer edge for that exact logical path is more
				// specific than the immutable prep tree and must win in argv.
				recipe.Arguments[index] = bindings[argumentPath]
			}
		}
		if validationOnly {
			recipe.WorkingOutputs = map[string]string{"00000000": writtenOutputPath}
		} else {
			// Every selected Kbuild command observes its outputs at the logical
			// object-tree path.  Besides keeping $@ independent of Bazel's physical
			// output location, this materializes the parent directory which upstream
			// rule_mkdir actions establish for compiler-owned side effects such as
			// .foo.o.d.  Collect the declared files only after the command exits so
			// undeclared scratch remains private to the action.
			recipe.WorkingOutputs = map[string]string{}
			for slot, logical := range logicalOutputPaths {
				workingOutput := logical
				if logical == logicalOutputPath {
					workingOutput = writtenOutputPath
				}
				recipe.WorkingOutputs[fmt.Sprintf("%08d", slot)] = workingOutput
			}
			if stdout {
				recipe.Stdout = "00000000"
			}
		}
		if sourceOverlay {
			if err := b.bindCompactKbuildSourceOverlayWorkingTree(match.profile, &recipe); err != nil {
				return "", fmt.Errorf("command %d source overlay working tree: %w", commandIndex, err)
			}
		}
		if err := appendReferencedPlanTrees(
			b.plan, &node, &recipe,
			append(recipe.Arguments, sortedStringMapValues(recipe.Environment)...)...,
		); err != nil {
			return "", fmt.Errorf("command %d source-tree closure: %w", commandIndex, err)
		}
		producer, err := appendActionPlanNode(b.plan, node, recipe)
		if err != nil {
			return "", fmt.Errorf("command %d: %w", commandIndex, err)
		}
		previousObservedStates, err = compactKbuildRecipeObservedStateInputs(
			producer, outputDescriptors, commandObservations,
		)
		if err != nil {
			return "", fmt.Errorf("command %d observed state outputs: %w", commandIndex, err)
		}
		propagatedPathSensitiveArchive := inPlacePathSensitiveArchive
		if byteProjection {
			// A proven byte-for-byte replacement takes its archive semantics from
			// the copied source, not from the previous bytes at the destination.
			propagatedPathSensitiveArchive = projectedPathSensitiveArchive
		}
		if typedSourceArchive || pathSensitiveArchiveOutput || propagatedPathSensitiveArchive {
			archiveSlot := slices.Index(logicalOutputPaths, logicalOutputPath)
			if archiveSlot < 0 {
				return "", fmt.Errorf("command %d path-sensitive output %q has no declared output slot", commandIndex, logicalOutputPath)
			}
			if err := b.plan.markPathSensitiveArchiveOutput(producer, archiveSlot); err != nil {
				return "", fmt.Errorf("command %d path-sensitive archive output: %w", commandIndex, err)
			}
		}
		for slot, logical := range logicalOutputPaths {
			result := compactKbuildRuleInput{
				path: logical, producer: producer, slot: slot, recipeLocal: true,
			}
			available = upsertCompactKbuildRuleInput(available, result)
			pathProducers[logical] = result
			b.memo[logical] = producer
		}
		previousProducer, previousSlot = producer, 0
		if publishesTarget {
			finalProducer = producer
		}
	}
	if finalProducer == "" {
		return "", fmt.Errorf("evaluated recipe does not produce concrete target %q from commands %#v", target, commands)
	}
	return finalProducer, nil
}

func compactKbuildRecipeRuleOutputs(target string, match compactKbuildRuleMatch) []string {
	outputs := []string{target}
	if !compactKbuildRuleHasGroupedOutputs(match.rule) {
		return outputs
	}
	seen := map[string]bool{target: true}
	for _, pattern := range match.rule.Targets {
		grouped := compactKbuildProfileTargetPath(match.profile, instantiateKbuildRulePattern(pattern, match.stem))
		if grouped == "" || seen[grouped] {
			continue
		}
		seen[grouped] = true
		outputs = append(outputs, grouped)
	}
	return outputs
}

// GNU Make's multi-target implicit pattern rules have one recipe invocation
// which is expected to update every peer target, including the historical
// colon form used by Linux's yacc/lex rules. Ordinary explicit colon rules and
// static-pattern rules remain independent; only &: groups those forms.
func compactKbuildRuleHasGroupedOutputs(rule KbuildRule) bool {
	if rule.Separator == "&:" {
		return true
	}
	if rule.Separator != ":" || rule.TargetPattern != "" || len(rule.Targets) < 2 {
		return false
	}
	for _, target := range rule.Targets {
		if strings.Count(target, "%") != 1 {
			return false
		}
	}
	return true
}

func (b *compactKbuildRulePlanBuilder) compactKbuildDeclaredOutputs(
	target string,
	match compactKbuildRuleMatch,
) ([]ActionPlanOutput, error) {
	target = canonicalKbuildRulePath(target)
	outputs := []ActionPlanOutput{}
	byPath := map[string]string{}
	appendOutput := func(output ActionPlanOutput) error {
		output.Path = canonicalKbuildRulePath(output.Path)
		if !LinuxKernelPlanTrees[output.Tree] {
			return fmt.Errorf("Kbuild output %q uses unsupported tree %q", output.Path, output.Tree)
		}
		if err := validatePlanRelativePath("Kbuild declared output", output.Path); err != nil {
			return err
		}
		if tree, exists := byPath[output.Path]; exists {
			if tree != output.Tree {
				return fmt.Errorf("Kbuild output %q is declared in both %q and %q trees", output.Path, tree, output.Tree)
			}
			return nil
		}
		byPath[output.Path] = output.Tree
		outputs = append(outputs, output)
		return nil
	}
	for _, outputPath := range compactKbuildRecipeRuleOutputs(target, match) {
		if err := appendOutput(ActionPlanOutput{Tree: b.planContext().OutputTree, Path: outputPath}); err != nil {
			return nil, err
		}
	}
	for _, observation := range b.observedOutputs[target] {
		output := observation.output
		output.ObservedPath = observation.path
		if err := appendOutput(output); err != nil {
			return nil, err
		}
	}
	if b.selectionBound && target == b.selection.target && b.selectionGraph != nil {
		for index := range outputs {
			outputs[index] = b.selectionGraph.compactKbuildSelectionOutput(b.selection, outputs[index])
		}
	}
	for _, observation := range b.observedOutputs[target] {
		if len(observation.baseInputs) == 0 {
			continue
		}
		outputIndex := -1
		for index, output := range outputs {
			if output.Tree == observation.output.Tree && output.Path == observation.output.Path && output.ObservedPath == observation.path {
				outputIndex = index
				break
			}
		}
		if outputIndex < 0 {
			return nil, fmt.Errorf("observed Kbuild capture %s/%s has no declared output", observation.output.Tree, observation.output.Path)
		}
		if b.plan == nil {
			return nil, fmt.Errorf("observed Kbuild capture %s/%s has no action plan", observation.output.Tree, observation.output.Path)
		}
		base := make([]ActionPlanNodeEdge, 0, len(observation.baseInputs))
		for _, input := range observation.baseInputs {
			base = append(base, ActionPlanNodeEdge{Role: "observed-state", ProducerID: input.producer, Slot: input.slot})
		}
		key := actionPlanLookupKey(outputs[outputIndex].Tree, actionPlanOutputArtifactPath(outputs[outputIndex]))
		if b.plan.observedOutputBases == nil {
			b.plan.observedOutputBases = map[string][]ActionPlanNodeEdge{}
		}
		if previous, exists := b.plan.observedOutputBases[key]; exists && !slices.Equal(previous, base) {
			return nil, fmt.Errorf("observed Kbuild capture %s has conflicting base state frontiers", key)
		}
		b.plan.observedOutputBases[key] = base
	}
	return outputs, nil
}

// Bazel and mapdirectoryrecipe create every declared output parent before a
// tool runs. A source-selected `mkdir -p ...` statement therefore has no
// observable action result. Validate its complete shape and erase it instead
// of depending on a host mkdir binary or encoding a target-name exception.
func compactKbuildRecipeDirectorySetup(command compactKbuildRecipeCommand) (bool, error) {
	program := path.Base(command.program)
	installDirectory := compactKbuildRecipeInstallDirectoryMode(command)
	if program != "mkdir" && !installDirectory {
		return false, nil
	}
	if len(command.environment) != 0 || command.stdin != "" || command.stdout != "" || command.connector == "|" {
		return false, fmt.Errorf("%s directory command has unsupported environment, redirection, or pipeline", program)
	}
	if program == "mkdir" && (len(command.arguments) < 2 || command.arguments[0] != "-p") {
		return false, fmt.Errorf("mkdir command is not an idempotent -p directory setup")
	}
	directories := command.arguments[1:]
	if installDirectory {
		directories = nil
		options := true
		for _, argument := range command.arguments {
			if options && argument == "--" {
				options = false
				continue
			}
			if options && strings.HasPrefix(argument, "-") {
				continue
			}
			directories = append(directories, argument)
		}
		if len(directories) == 0 {
			return false, fmt.Errorf("install --directory command has no directory operand")
		}
	}
	for _, directory := range directories {
		directory = strings.TrimSuffix(directory, "/")
		if _, ok := compactKbuildRecipePath(directory); !ok {
			return false, fmt.Errorf("kernel action plan Kbuild %s directory path %q is not a canonicalizable relative path", program, directory)
		}
	}
	return true, nil
}

func sortedStringMapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, key := range sortedStringMapKeys(values) {
		result = append(result, values[key])
	}
	return result
}

type compactKbuildAutomaticContext struct {
	target string
	stem   string
	normal []string
	order  []string
}

func compactKbuildInputPaths(inputs []compactKbuildRuleInput, orderOnly bool) []string {
	paths := []string{}
	for _, input := range inputs {
		if !input.workingOnly && input.orderOnly == orderOnly {
			paths = append(paths, input.path)
		}
	}
	return paths
}

func (b *compactKbuildRulePlanBuilder) ensureCommandInput(
	inputs []compactKbuildRuleInput,
	inputPath string,
	required bool,
) ([]compactKbuildRuleInput, int, error) {
	for index, input := range inputs {
		if input.path == inputPath {
			return inputs, index, nil
		}
	}
	resolveExisting := b.existingInput
	if !required {
		resolveExisting = b.existingRecordedInput
	}
	if input, ok, err := resolveExisting(inputPath); err != nil {
		return nil, -1, err
	} else if ok {
		return append(inputs, input), len(inputs), nil
	}
	if evidence, err := b.sourcePathEvidence(inputPath); err != nil {
		return nil, -1, err
	} else if evidence.exists {
		sourceID, err := b.metadata.ensureActionPlanSource(b.plan, inputPath)
		if err != nil {
			return nil, -1, err
		}
		return append(inputs, compactKbuildRuleInput{
			path: inputPath, sourceID: sourceID, objectTree: evidence.objectTree,
		}), len(inputs), nil
	}
	match, matched, err := b.ruleForTarget(inputPath)
	if err != nil {
		return nil, -1, err
	}
	matchViable := match.explicit
	if matched && !matchViable {
		matchViable, err = b.metadata.compactKbuildRuleMatchViableInProfile(inputPath, match, *b.profile)
		if err != nil {
			return nil, -1, err
		}
	}
	if matched && matchViable {
		producer, err := b.build(inputPath)
		if err != nil {
			return nil, -1, err
		}
		_, slot, ok := b.existingProducer(inputPath)
		if !ok {
			return nil, -1, fmt.Errorf("evaluated target was built without a declared output")
		}
		return append(inputs, compactKbuildRuleInput{path: inputPath, producer: producer, slot: slot}), len(inputs), nil
	}
	if required {
		return nil, -1, fmt.Errorf(
			"evaluated graph has no source, rule, or existing producer for %q in profile %q",
			inputPath, b.profile.Name,
		)
	}
	return inputs, -1, nil
}

// ensureDeclaredSourceInput interns a path whose physical presence in the
// source tree was already established by the evaluated-profile matcher. It is
// intentionally separate from ensureCommandInput: scripts need not also be an
// object variant or a rule prerequisite merely to become an immutable source
// edge in the action graph.
func (b *compactKbuildRulePlanBuilder) ensureDeclaredSourceInput(
	inputs []compactKbuildRuleInput,
	inputPath string,
) ([]compactKbuildRuleInput, int, error) {
	for index, input := range inputs {
		if input.path != inputPath {
			continue
		}
		if input.sourceID == "" || input.producer != "" {
			return nil, -1, fmt.Errorf("evaluated source path is already bound to a generated input")
		}
		return inputs, index, nil
	}
	sourceID, err := b.metadata.ensureActionPlanSource(b.plan, inputPath)
	if err != nil {
		return nil, -1, err
	}
	return append(inputs, compactKbuildRuleInput{path: inputPath, sourceID: sourceID}), len(inputs), nil
}

func (b *compactKbuildRulePlanBuilder) ensureCommandProgramInput(
	inputs []compactKbuildRuleInput,
	programPath string,
	sourceProgram bool,
) ([]compactKbuildRuleInput, int, error) {
	for index, input := range inputs {
		if input.path != programPath {
			continue
		}
		if input.producer != "" {
			return inputs, index, nil
		}
		if input.sourceID != "" {
			producer, err := b.materializeSourceExecutable(programPath)
			if err != nil {
				return nil, -1, err
			}
			inputs[index] = compactKbuildRuleInput{path: programPath, producer: producer}
			return inputs, index, nil
		}
	}
	if producer, slot, ok := b.existingProducer(programPath); ok {
		result := compactKbuildRuleInput{path: programPath, producer: producer, slot: slot}
		inputs = upsertCompactKbuildRuleInput(inputs, result)
		for index := range inputs {
			if inputs[index].path == programPath {
				return inputs, index, nil
			}
		}
	}
	programIsSource := sourceProgram
	if !programIsSource {
		var err error
		programIsSource, err = b.sourcePathExists(programPath)
		if err != nil {
			return nil, -1, err
		}
	}
	if programIsSource {
		producer, err := b.materializeSourceExecutable(programPath)
		if err != nil {
			return nil, -1, err
		}
		result := compactKbuildRuleInput{path: programPath, producer: producer}
		inputs = upsertCompactKbuildRuleInput(inputs, result)
		for index := range inputs {
			if inputs[index].path == programPath {
				return inputs, index, nil
			}
		}
	}
	return b.ensureCommandInput(inputs, programPath, true)
}

func (b *compactKbuildRulePlanBuilder) materializeSourceExecutable(sourcePath string) (string, error) {
	// Source files are not executable inputs by themselves under Bazel. Stage a
	// private executable copy no later than its consumer: ordinary prep/target
	// consumers share the host copy, while the two phases which precede host
	// require a stage-local copy to avoid a backward action-plan edge.
	stage := "host"
	if consumerStage := b.planContext().Stage; consumerStage == "prehost" || consumerStage == "bootstrap" {
		stage = consumerStage
	}
	if producer, _, ok := planProducerByOutput(b.plan, stage, sourcePath); ok {
		return producer, nil
	}
	sourceID, err := b.metadata.ensureActionPlanSource(b.plan, sourcePath)
	if err != nil {
		return "", err
	}
	node := ActionPlanNode{
		Stage: stage, Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "program", SourceID: sourceID}},
		Outputs: []ActionPlanOutput{{Tree: stage, Path: sourcePath}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:program:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"program:00000000"}, Outputs: []string{"00000000"},
	}
	return appendActionPlanNode(b.plan, node, recipe)
}

func (b *compactKbuildRulePlanBuilder) ensureOptionalCommandInput(
	inputs []compactKbuildRuleInput,
	inputPath string,
) ([]compactKbuildRuleInput, int, bool, error) {
	before := len(inputs)
	inputs, index, err := b.ensureCommandInput(inputs, inputPath, false)
	return inputs, index, len(inputs) != before, err
}

// compactKbuildProfileCommandOperandPath resolves a path-bearing argv or stdin
// field in the selected recipe's invocation directory. Preserve the existing
// command-path grammar for ordinary operands, but additionally accept an exact
// parent-relative operand (including an option value after '=') so an external
// Kbuild invocation can name a prepared object-tree input without retaining the
// whole tree.
func compactKbuildProfileCommandOperandPath(profile CompactKbuildProfile, value string) (string, bool) {
	candidate := value
	if index := strings.IndexByte(candidate, '='); strings.HasPrefix(candidate, "-") && index >= 0 {
		candidate = candidate[index+1:]
	}
	if candidate == "" || strings.TrimSpace(candidate) != candidate {
		return "", false
	}
	if resolved, ok := compactKbuildCommandPath(value); ok {
		// Preserve the preexisting graph-root interpretation for every operand
		// already accepted by compactKbuildCommandPath. In particular, a bare
		// symbolic probe result must not acquire the invocation directory and
		// turn into a generated-file target during probe planning.
		return resolved, true
	}
	cleaned := filepath.ToSlash(filepath.Clean(candidate))
	if cleaned != ".." && !strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	resolved, _, ok := compactKbuildProfileCommandPath(profile, candidate)
	return resolved, ok
}

func compactKbuildCommandPath(value string) (string, bool) {
	// Target-context evaluation uses private control-byte tree roots so Make
	// path algebra cannot confuse injected roots with source-authored literals.
	// Command lookup is the graph boundary: project those private roots back to
	// the public sentinel namespace before classifying source versus generated
	// executables and canonicalizing their graph paths.
	value = compactKbuildMaterializeActionTreeMarkers(value)
	if strings.TrimSpace(value) != value {
		return "", false
	}
	for _, marker := range []string{"__LINUX_BZL_SOURCE_TREE__", "__LINUX_BZL_OBJECT_TREE__"} {
		if index := strings.Index(value, marker+"/"); index >= 0 {
			value = value[index+len(marker)+1:]
			break
		}
	}
	for _, prefix := range []string{"${tree:kernel}/", "${tree:prep}/", "${tree:host}/", "${tree:bootstrap}/", "${tree:prehost}/"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = canonicalKbuildRulePath(value)
	if value == "" || path.IsAbs(value) || value == ".." || strings.HasPrefix(value, "../") {
		return "", false
	}
	if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "=,:()[]{}") {
		return "", false
	}
	if err := validatePlanRelativePath("Kbuild command path", value); err != nil {
		return "", false
	}
	return value, true
}

func compactKbuildProfileCommandPath(profile CompactKbuildProfile, value string) (string, bool, bool) {
	value = compactKbuildMaterializeActionTreeMarkers(value)
	trimmed := strings.TrimSpace(value)
	if commandPath, ok := compactKbuildCommandPath(value); ok {
		sourceProgram := strings.Contains(trimmed, "__LINUX_BZL_SOURCE_TREE__/") ||
			strings.HasPrefix(trimmed, "${tree:kernel}/")
		objectProgram := strings.Contains(trimmed, "__LINUX_BZL_OBJECT_TREE__/") ||
			strings.HasPrefix(trimmed, "${tree:prep}/") ||
			strings.HasPrefix(trimmed, "${tree:host}/") ||
			strings.HasPrefix(trimmed, "${tree:bootstrap}/") ||
			strings.HasPrefix(trimmed, "${tree:prehost}/")
		if !sourceProgram && !objectProgram {
			if scoped, evaluated := compactKbuildProfileInvocationRelativePath(profile, commandPath); evaluated {
				commandPath = scoped
			}
			sourceProgram = compactKbuildProfileSourcePathExists(profile, commandPath)
		}
		return commandPath, sourceProgram, true
	}
	if !filepath.IsAbs(trimmed) && !strings.ContainsAny(trimmed, "$`\x00\r\n") {
		if commandPath, evaluated := compactKbuildProfileInvocationRelativePath(profile, filepath.ToSlash(filepath.Clean(trimmed))); evaluated {
			commandPath = canonicalKbuildRulePath(commandPath)
			if err := validatePlanRelativePath("Kbuild invocation-relative command program", commandPath); err == nil {
				return commandPath, compactKbuildProfileSourcePathExists(profile, commandPath), true
			}
		}
	}
	value = filepath.Clean(trimmed)
	if !filepath.IsAbs(value) || profile.evaluator == nil || profile.evaluator.template == nil {
		return "", false, false
	}
	type commandRoot struct {
		path   string
		source bool
	}
	rootKinds := map[string]bool{}
	for marker, root := range profile.evaluator.template.sourceRoots {
		root, err := filepath.Abs(root)
		if err == nil {
			root = filepath.Clean(root)
			rootKinds[root] = rootKinds[root] || marker != "__LINUX_BZL_OBJECT_TREE__"
		}
	}
	roots := make([]commandRoot, 0, len(rootKinds))
	for root, source := range rootKinds {
		roots = append(roots, commandRoot{path: root, source: source})
	}
	sort.Slice(roots, func(i, j int) bool { return len(roots[i].path) > len(roots[j].path) })
	for _, root := range roots {
		relative, err := filepath.Rel(root.path, value)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		commandPath := canonicalKbuildRulePath(relative)
		if err := validatePlanRelativePath("Kbuild command program", commandPath); err == nil {
			return commandPath, root.source, true
		}
	}
	return "", false, false
}

// ResolveCompactKbuildProfileSourcePath maps a canonical Kbuild graph path to
// the physical source root selected for this exact Make invocation. More
// specific source-root overlays win over the kernel root, matching the
// parser's source-root lookup without leaking physical paths into the graph.
// Direct non-reserved roots cover configured source trees such as rules_rs;
// reserved object-tree and host-dependency roots are deliberately not source
// evidence.
func ResolveCompactKbuildProfileSourcePath(profile CompactKbuildProfile, sourcePath string) (string, bool) {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return "", false
	}
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if err := validatePlanRelativePath("Kbuild source path", sourcePath); err != nil {
		return "", false
	}
	type sourceRoot struct {
		graphPrefix string
		physical    string
	}
	selected := sourceRoot{}
	selectedSet := false
	for rawPrefix, root := range profile.evaluator.template.sourceRoots {
		prefix := filepath.ToSlash(rawPrefix)
		graphPrefix := ""
		switch {
		case prefix == "__LINUX_BZL_SOURCE_TREE__":
		case strings.HasPrefix(prefix, "__LINUX_BZL_SOURCE_TREE__/"):
			graphPrefix = strings.TrimPrefix(prefix, "__LINUX_BZL_SOURCE_TREE__/")
			if validatePlanRelativePath("Kbuild source-tree overlay", graphPrefix) != nil {
				continue
			}
		case strings.HasPrefix(prefix, "__LINUX_BZL_"):
			continue
		default:
			graphPrefix = prefix
			if validatePlanRelativePath("Kbuild configured source root", graphPrefix) != nil {
				continue
			}
		}
		if graphPrefix != "" && sourcePath != graphPrefix && !strings.HasPrefix(sourcePath, graphPrefix+"/") {
			continue
		}
		if !selectedSet || len(graphPrefix) > len(selected.graphPrefix) {
			selected = sourceRoot{graphPrefix: graphPrefix, physical: root}
			selectedSet = true
		} else if len(graphPrefix) == len(selected.graphPrefix) && graphPrefix == selected.graphPrefix && filepath.Clean(root) != filepath.Clean(selected.physical) {
			// Two physical roots for the same logical prefix have no stable source
			// identity. Fail closed instead of depending on map iteration order.
			return "", false
		}
	}
	if !selectedSet || strings.TrimSpace(selected.physical) == "" {
		return "", false
	}
	relative := strings.TrimPrefix(sourcePath, selected.graphPrefix)
	relative = strings.TrimPrefix(relative, "/")
	return filepath.Join(selected.physical, filepath.FromSlash(relative)), true
}

func compactKbuildProfileSourcePathExists(profile CompactKbuildProfile, sourcePath string) bool {
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if err := validatePlanRelativePath("Kbuild source program", sourcePath); err != nil {
		return false
	}
	runtime.sourceMu.Lock()
	defer runtime.sourceMu.Unlock()
	if exists, ok := runtime.sources[sourcePath]; ok {
		return exists
	}
	filename, resolved := ResolveCompactKbuildProfileSourcePath(profile, sourcePath)
	exists := resolved && fileExists(filename)
	runtime.sources[sourcePath] = exists
	return exists
}

func validateShellFreeKbuildCommandField(value string) error {
	// Unquoted shell operators were emitted as operator tokens by
	// lexCompactKbuildRecipe. Any operator character that remains in a field
	// was quoted or escaped and is therefore literal argv data (sed programs
	// commonly contain semicolons). Backticks still denote command
	// substitution in the source grammar and are not modeled here.
	if strings.ContainsAny(value, "\x00\n\r`") {
		return fmt.Errorf("shell/control syntax is not supported in argv field %q", value)
	}
	return nil
}

func expandKbuildAutomaticCommandField(field string, context compactKbuildAutomaticContext) ([]string, error) {
	values := compactKbuildAutomaticValues{context: context}

	if name, ok := exactKbuildAutomaticReference(field); ok {
		paths, supported := values.resolve(name)
		if !supported {
			return nil, fmt.Errorf("unsupported Kbuild automatic variable %q", field)
		}
		return append([]string(nil), paths...), nil
	}
	remaining := field
	for {
		start, end, name, ok := nextKbuildAutomaticReference(remaining)
		if !ok {
			break
		}
		paths, supported := values.resolve(name)
		if !supported {
			return nil, fmt.Errorf("unsupported Kbuild automatic variable %q", remaining[start:end])
		}
		if len(paths) > 1 {
			return nil, fmt.Errorf("automatic variable %q expands to multiple argv fields inside %q", remaining[start:end], field)
		}
		replacement := ""
		if len(paths) == 1 {
			replacement = paths[0]
		}
		remaining = remaining[:start] + replacement + remaining[end:]
	}
	if containsUnmodeledKbuildDollar(remaining) {
		return nil, fmt.Errorf("unmodeled Kbuild automatic or shell variable in %q", remaining)
	}
	return []string{remaining}, nil
}

// compactKbuildAutomaticValues resolves only the automatic variables observed
// in one command field. A Kbuild recipe overwhelmingly references one or two
// automatic variables, so eagerly deriving every D and F variant needlessly
// walks every prerequisite many times. The memo also makes repeated references
// within a field share the same derived value without exposing the cached slice
// to the caller.
type compactKbuildAutomaticValues struct {
	context compactKbuildAutomaticContext
	values  map[string][]string
}

func (v *compactKbuildAutomaticValues) resolve(name string) ([]string, bool) {
	if paths, ok := v.values[name]; ok {
		return paths, true
	}

	var paths []string
	switch name {
	case "@":
		paths = []string{v.context.target}
	case "<":
		paths = firstCompactKbuildPath(v.context.normal)
	case "^":
		paths = uniqueCompactKbuildPaths(v.context.normal)
	case "+":
		paths = append([]string(nil), v.context.normal...)
	case "?":
		// Every input is freshly staged for a hermetic action, so every normal
		// prerequisite has Make's "newer than target" semantics.
		paths = uniqueCompactKbuildPaths(v.context.normal)
	case "|":
		paths = uniqueCompactKbuildPaths(v.context.order)
	case "*":
		paths = compactKbuildOptionalPath(v.context.stem)
	default:
		if len(name) != 2 || (name[1] != 'D' && name[1] != 'F') {
			return nil, false
		}
		base, ok := v.resolve(name[:1])
		if !ok {
			return nil, false
		}
		transform := path.Dir
		if name[1] == 'F' {
			transform = path.Base
		}
		paths = mapCompactKbuildPaths(base, transform)
	}

	if v.values == nil {
		v.values = map[string][]string{}
	}
	v.values[name] = paths
	return paths, true
}

func exactKbuildAutomaticReference(value string) (string, bool) {
	start, end, name, ok := nextKbuildAutomaticReference(value)
	return name, ok && start == 0 && end == len(value)
}

func nextKbuildAutomaticReference(value string) (int, int, string, bool) {
	for start := strings.IndexByte(value, '$'); start >= 0; {
		if start+1 >= len(value) {
			return start, len(value), "", true
		}
		next := value[start+1]
		if strings.ContainsRune("@%<?^+*|", rune(next)) {
			return start, start + 2, string(next), true
		}
		if next == '(' || next == '{' {
			closer := byte(')')
			if next == '{' {
				closer = '}'
			}
			if offset := strings.IndexByte(value[start+2:], closer); offset >= 0 {
				end := start + 2 + offset
				name := value[start+2 : end]
				if name != "" && strings.ContainsRune("@%<?^+*|", rune(name[0])) {
					return start, end + 1, name, true
				}
			}
		}
		nextStart := strings.IndexByte(value[start+1:], '$')
		if nextStart < 0 {
			break
		}
		start += nextStart + 1
	}
	return 0, 0, "", false
}

func firstCompactKbuildPath(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	return []string{paths[0]}
}

func compactKbuildOptionalPath(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func uniqueCompactKbuildPaths(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func mapCompactKbuildPaths(values []string, transform func(string) string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = transform(value)
	}
	return out
}

func appendActionPlanNode(plan *ActionPlan, node ActionPlanNode, recipe ActionRecipe) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("cannot append an object action to a nil plan")
	}
	// markPathSensitiveArchiveOutput is called synchronously after the append
	// which creates an archive. On the following append every earlier node has
	// therefore acquired all provenance it can ever need, and discovery may
	// discard its executor-only payload before constructing another recipe.
	plan.releaseProbeDiscoveryPayloads()
	if plan.Recipes == nil {
		plan.Recipes = map[string]ActionRecipe{}
	}
	// The plan owns every recipe it interns. Deferred-content binding mutates
	// the recipe, and callers may retain or reuse their original value.
	recipe = cloneActionRecipe(recipe)
	if err := bindActionPlanObservedOutputs(plan, &node, &recipe); err != nil {
		return "", err
	}
	if err := bindVersionedActionPlanWorkingOutputs(node, &recipe); err != nil {
		return "", err
	}
	if err := bindActionRecipeDeferredKbuildContent(plan, &node, &recipe); err != nil {
		return "", err
	}
	recipeData, err := recipe.CanonicalJSON()
	if err != nil {
		return "", err
	}
	recipeDigest := sha256.Sum256(recipeData)
	recipeID := hex.EncodeToString(recipeDigest[:])
	if plan.probeDiscoveryOnly {
		if plan.probeDiscoveryRecipeCanonical == nil {
			plan.probeDiscoveryRecipeCanonical = map[string][]byte{}
		}
		if existing, ok := plan.probeDiscoveryRecipeCanonical[recipeID]; ok {
			if !bytes.Equal(existing, recipeData) {
				return "", fmt.Errorf("recipe content ID collision %s", recipeID)
			}
		} else {
			// Canonical JSON is the exact collision witness used by normal plans,
			// but is considerably denser than retaining every Go recipe graph.
			plan.probeDiscoveryRecipeCanonical[recipeID] = recipeData
		}
	} else if existing, ok := plan.Recipes[recipeID]; ok {
		left, _ := existing.CanonicalJSON()
		if !bytes.Equal(left, recipeData) {
			return "", fmt.Errorf("recipe content ID collision %s", recipeID)
		}
	}
	node.Recipe = recipeID
	if node.ID == "" {
		node.ID = node.ContentID()
	}
	plan.ensureNodeLookupIndexes()
	if _, exists := plan.nodesByID[node.ID]; exists {
		return "", fmt.Errorf("object action repeats provisional node %s", node.ID)
	}
	plan.Recipes[recipeID] = recipe
	plan.Nodes = append(plan.Nodes, node)
	plan.recordNodeLookup(node)
	return node.ID, nil
}

// releaseProbeDiscoveryPayloads compacts only the newly appended suffix. It
// retains the structural graph required by later producer lookup, topology,
// source-script closure, side-output state, and identity validation. Complete
// recipes survive only for path-sensitive archives, whose working source
// bindings are consulted by later script lowering. Canonical bytes remain as
// exact content-ID collision witnesses for every discarded recipe.
func (plan *ActionPlan) releaseProbeDiscoveryPayloads() {
	if plan == nil || !plan.probeDiscoveryOnly {
		return
	}
	if plan.probeDiscoveryRetainedRecipes == nil {
		plan.probeDiscoveryRetainedRecipes = map[string]bool{}
	}
	for index := plan.probeDiscoveryCompactedNodes; index < len(plan.Nodes); index++ {
		node := &plan.Nodes[index]
		if plan.hasPathSensitiveArchiveOutput(node.ID) {
			plan.probeDiscoveryRetainedRecipes[node.Recipe] = true
		} else {
			if !plan.probeDiscoveryRetainedRecipes[node.Recipe] {
				delete(plan.Recipes, node.Recipe)
			}
			node.Recipe = ""
		}
		// These fields have already contributed to the provisional content ID
		// and recipe validation. No later discovery operation reads them.
		node.Product = ""
		node.Trees = nil
		node.AuxiliaryTools = nil
		if plan.nodesByID != nil {
			plan.nodesByID[node.ID] = *node
		}
	}
	plan.probeDiscoveryCompactedNodes = len(plan.Nodes)
}

// bindActionPlanObservedOutputs converts planner-only output annotations into
// the recipe runtime protocol. Recipe builders initially treat every declared
// output as a command-owned working file; an observed capture instead belongs
// to mapdirectoryrecipe and must never be exposed as that command's pathname.
func bindActionPlanObservedOutputs(plan *ActionPlan, node *ActionPlanNode, recipe *ActionRecipe) error {
	if plan == nil || node == nil {
		return fmt.Errorf("cannot bind observed outputs without an action plan node")
	}
	if recipe == nil {
		return fmt.Errorf("cannot bind observed outputs to a nil recipe")
	}
	for slot, output := range node.Outputs {
		if output.ObservedPath == "" {
			continue
		}
		if recipe.WorkingDirectory == "" {
			return fmt.Errorf("observed output %q requires a private recipe working directory", output.ObservedPath)
		}
		if err := validatePlanRelativePath("observed output working path", output.ObservedPath); err != nil {
			return err
		}
		binding := planOrdinal(slot)
		if recipe.Stdout == binding {
			return fmt.Errorf("observed output binding %s is also recipe stdout", binding)
		}
		if recipe.ObservedOutputs == nil {
			recipe.ObservedOutputs = map[string]string{}
		}
		if existing := recipe.ObservedOutputs[binding]; existing != "" && existing != output.ObservedPath {
			return fmt.Errorf("observed output binding %s has conflicting paths %q and %q", binding, existing, output.ObservedPath)
		}
		recipe.ObservedOutputs[binding] = output.ObservedPath
		delete(recipe.WorkingOutputs, binding)
		baseKey := actionPlanLookupKey(output.Tree, actionPlanOutputArtifactPath(output))
		base := plan.observedOutputBases[baseKey]
		if len(base) == 0 {
			continue
		}
		if recipe.ObservedOutputBases == nil {
			recipe.ObservedOutputBases = map[string][]string{}
		}
		for _, parent := range base {
			if parent.ProducerID == "" || parent.Slot < 0 {
				return fmt.Errorf("observed output binding %s has invalid base state edge %#v", binding, parent)
			}
			inputBinding := fmt.Sprintf("observed-state:%08d", len(node.Inputs))
			node.Inputs = append(node.Inputs, ActionPlanNodeEdge{
				Role: "observed-state", ProducerID: parent.ProducerID, Slot: parent.Slot,
			})
			recipe.Inputs = append(recipe.Inputs, inputBinding)
			recipe.ObservedOutputBases[binding] = append(recipe.ObservedOutputBases[binding], inputBinding)
		}
	}
	return nil
}

// bindVersionedActionPlanWorkingOutputs keeps the source-visible pathname
// independent from the reserved immutable ArtifactPath. It runs before recipe
// and node IDs are computed; versioning is therefore part of the ordinary
// content-addressed contract, never a post-hoc node mutation.
func bindVersionedActionPlanWorkingOutputs(node ActionPlanNode, recipe *ActionRecipe) error {
	if recipe == nil {
		return fmt.Errorf("cannot bind versioned outputs to a nil recipe")
	}
	versioned := false
	for _, output := range node.Outputs {
		if !actionPlanOutputIsCanonical(output) {
			versioned = true
			break
		}
	}
	if !versioned {
		return nil
	}
	if recipe.WorkingDirectory == "" {
		recipe.WorkingDirectory = "linux-bzl-versioned-output"
	}
	if recipe.WorkingOutputs == nil {
		recipe.WorkingOutputs = map[string]string{}
	}
	for slot, output := range node.Outputs {
		if output.ObservedPath != "" {
			continue
		}
		if actionPlanOutputIsCanonical(output) {
			continue
		}
		binding := planOrdinal(slot)
		if existing := recipe.WorkingOutputs[binding]; existing != "" {
			continue
		}
		recipe.WorkingOutputs[binding] = output.Path
	}
	return nil
}

const (
	compactKbuildRuleSelectionDepthLimit = 64
	compactKbuildCommandSelectionMarker  = "__LINUX_BZL_SELECTED_KBUILD_COMMAND_"
)

// evaluatedKbuildRuleCommandSelections follows the Make macros selected by a
// rule recipe and returns the leaf cmd_<name> templates which actually occupy
// shell command-head positions. This distinction matters for if_changed_rule:
// its argument names rule_<name>, not cmd_<name>, and that rule may recursively
// select further rules before reaching one or more command templates.
//
// The ordinary target evaluator remains authoritative for calls, computed
// variable names, conditionals, target-specific values, and optional empty
// commands. A parse-local observer substitutes one inert word for each
// non-empty cmd_<name>. Inspecting the expanded shell command heads then
// distinguishes the command execution from bookkeeping references such as the
// quoted copy of cmd_<name> embedded by make-cmd in a .cmd file.
func evaluatedKbuildRuleCommandSelections(
	profile CompactKbuildProfile,
	target, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	recipe []string,
	resolveSymbolic bool,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluatedKbuildRuleCommandSelectionsForMakeTarget(
		profile, target, target, automaticTarget, stem, normal, orderOnly,
		injected, recipe, resolveSymbolic,
	)
}

func evaluatedKbuildRuleCommandSelectionsForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	recipe []string,
	resolveSymbolic bool,
) ([]CompactKbuildCommandTemplate, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly, injected, true, false,
	)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	preserveUnsupportedKbuildRecipeShell(parser, profile, target)
	incompleteRuleWrapper := false
	incompleteCommandWrapper := false

	// Focused rule fixtures historically provide only cmd_<name> variables and
	// the recipe call, while a complete Linux invocation imports the wrapper
	// definitions from scripts/Kbuild.include. Preserve that useful incomplete-
	// profile behavior at the DSL boundary. Once a wrapper exists, including
	// cmd_and_fixdep/cmd_and_savecmd reached through rule_<name>, its source body
	// is always authoritative and no planner-side spelling is interpreted.
	for _, call := range kbuildRecipeCommandCalls(recipe) {
		if _, defined := parser.lookupVariable(call.wrapper); defined {
			continue
		}
		body := "$(cmd_$(1))"
		if call.wrapper == "if_changed_rule" {
			body = "$(rule_$(1))"
			incompleteRuleWrapper = true
		} else {
			incompleteCommandWrapper = true
		}
		parser.setVariable(call.wrapper, kbuildVariable{value: body, recursive: true})
	}

	for _, line := range recipe {
		if strings.Contains(line, compactKbuildCommandSelectionMarker) {
			return nil, fmt.Errorf(
				"target %q profile %q recipe contains reserved command-selection marker",
				target, profile.Name,
			)
		}
	}
	cloneLocals := func(locals []map[string]string) []map[string]string {
		cloned := make([]map[string]string, len(locals))
		for index, local := range locals {
			cloned[index] = maps.Clone(local)
		}
		return cloned
	}
	// Observer expansion may cache a deferred variable while its selected leaf
	// is still represented by a marker. Keep one pristine target-context parser
	// for exact wrapper replay after selection; it includes automatic variables
	// and source-defined wrapper fallbacks but none of those marker-era caches.
	exactReplayParser := *parser
	exactReplayParser.vars = maps.Clone(parser.vars)
	exactReplayParser.locals = cloneLocals(parser.locals)
	exactReplayParser.expanding = map[string]bool{}
	exactReplayParser.commandSelectionExpansion = nil

	selectionByMarker := map[string]CompactKbuildCommandTemplate{}
	ruleStack := []string{}
	ruleStackIndex := map[string]int{}
	commandStack := []string{}
	commandStackIndex := map[string]int{}
	var lineCommandAccesses *[]CompactKbuildCommandTemplate
	newSelectionMarker := func(selection CompactKbuildCommandTemplate, preserveEmpty bool) (string, error) {
		// The marker deliberately hides selected command text from the outer
		// recipe expansion. Resolve replay-time probe atoms before hiding that
		// text, or the outer resolver can see only the inert marker and a planner
		// token can escape into an executed argv. Discovery keeps the same text
		// symbolic so it can still collect the complete probe workload.
		if resolveSymbolic && linuxProbeSymbolPattern.MatchString(selection.Text) {
			resolved, err := parser.resolveKbuildSymbolic(selection.Text)
			if err != nil {
				owner := selection.Name
				if owner == "" {
					owner = "direct recipe"
				} else {
					owner = "cmd_" + owner
				}
				return "", fmt.Errorf(
					"target %q profile %q selected Kbuild command %s symbolic result: %w",
					target, profile.Name, owner, err,
				)
			}
			selection.Text = resolved
		}
		if !preserveEmpty && strings.TrimSpace(selection.Text) == "" {
			return "", nil
		}
		marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, len(selectionByMarker))
		selectionByMarker[marker] = selection
		if lineCommandAccesses != nil {
			*lineCommandAccesses = append(*lineCommandAccesses, selection)
		}
		return marker, nil
	}
	directExpansionIsControlOnly := func(value string) bool {
		value = compactKbuildDirectRecipeText(profile, value)
		allNoops := true
		for _, line := range strings.Split(value, "\n") {
			switch strings.TrimSpace(line) {
			case "", ":", "true":
			default:
				allNoops = false
			}
		}
		if allNoops {
			return true
		}
		commands, parseErr := parseCompactKbuildRecipe(value, compactKbuildAutomaticContext{
			target: automaticTarget,
			stem:   stem,
			normal: append([]string(nil), normal...),
			order:  append([]string(nil), orderOnly...),
		})
		if parseErr != nil || len(commands) == 0 {
			return false
		}
		for _, command := range commands {
			setup, setupErr := compactKbuildRecipeDirectorySetup(command)
			if setupErr != nil || !setup {
				return false
			}
		}
		return true
	}
	preserveDirectSelection := func(owner, source, expanded string) (string, error) {
		if directExpansionIsControlOnly(expanded) {
			return expanded, nil
		}
		source = strings.TrimSpace(source)
		if _, _, filechk := kbuildFilechkCall(source); filechk {
			end, matchErr := matchingKbuildReference(source, 1)
			function, arguments, call := "", []string(nil), false
			if matchErr == nil && end == len(source)-1 {
				function, arguments, call = splitMakeFunction(source[2:end])
			}
			if !call || function != "call" {
				return "", fmt.Errorf(
					"target %q profile %q direct Kbuild filechk source %q from %s is not an exact call",
					target, profile.Name, source, owner,
				)
			}
			selectedArguments := make([]string, len(arguments))
			for index, argument := range arguments {
				value, argumentErr := parser.expand(argument)
				if argumentErr == nil && resolveSymbolic {
					value, argumentErr = parser.resolveKbuildSymbolic(value)
				}
				if argumentErr != nil {
					return "", fmt.Errorf(
						"target %q profile %q direct Kbuild filechk argument %d from %s: %w",
						target, profile.Name, index, owner, argumentErr,
					)
				}
				if strings.Contains(value, ",") {
					return "", fmt.Errorf(
						"target %q profile %q direct Kbuild filechk argument %d from %s contains an unrepresentable comma",
						target, profile.Name, index, owner,
					)
				}
				selectedArguments[index] = value
			}
			source = "$(call " + strings.Join(selectedArguments, ",") + ")"
		}
		expanded = compactKbuildDirectRecipeText(profile, expanded)
		// One preserved helper is one source recipe command. Terminate its inert
		// marker explicitly because the compact shell lexer treats a Make define's
		// newline as whitespace; without the separator the next rule line would be
		// misclassified as an argument to this occurrence.
		marker, err := newSelectionMarker(CompactKbuildCommandTemplate{Source: source, Text: expanded}, false)
		if err != nil {
			return "", err
		}
		return marker + ";", nil
	}

	var observer func(name, original string, depth int) (string, bool, error)
	expandWithoutObserver := func(name, original string, depth int) (string, bool, error) {
		saved := parser.commandSelectionExpansion
		parser.commandSelectionExpansion = nil
		defer func() { parser.commandSelectionExpansion = saved }()
		value, ok, expandErr := parser.expandVariable(name, original, depth)
		if expandErr != nil || !ok || !resolveSymbolic {
			return value, ok, expandErr
		}
		value, expandErr = parser.resolveKbuildSymbolic(value)
		return value, ok, expandErr
	}
	encapsulateExpandedSelectionLine := func(
		sourceLine, expanded string,
		markerStart, accessStart int,
	) (string, bool, error) {
		markerEnd := len(selectionByMarker)
		if markerEnd == markerStart {
			return expanded, false, nil
		}
		// A rule_<name> reference can expand several independently authored
		// recipe lines into its caller. Each nested source line has already
		// passed through this wrapper-preservation boundary. Do not fuse those
		// lines again at the outer call: an empty `$(call cmd,...)` commonly
		// expands to `@:` on a following line and must not become an operand of
		// the preceding command.
		if tokens, tokenErr := lexCompactKbuildRecipe(expanded); tokenErr == nil {
			for _, token := range tokens {
				if token.operator && token.start >= 0 && token.start < len(expanded) && expanded[token.start] == '\n' {
					return expanded, false, nil
				}
			}
		}
		programs, programErr := compactKbuildCompoundProgramCommands(expanded)
		if programErr != nil {
			return expanded, false, nil
		}
		selectedHeadMarkers := []string{}
		selectedHeadSeen := false
		wrapperEffects := false
		for _, program := range programs {
			if _, selected := selectionByMarker[program.program]; selected {
				selectedHeadSeen = true
				selectedHeadMarkers = append(selectedHeadMarkers, program.program)
				wrapperEffects = wrapperEffects || program.stdin != "" || program.stdout != ""
				continue
			}
			if selectedHeadSeen && program.program != ":" && program.program != "true" {
				wrapperEffects = true
			}
		}
		if !selectedHeadSeen || !wrapperEffects {
			return expanded, false, nil
		}
		// A symbolic Make branch can hide a selected command marker entirely.
		// Keep the individual speculative selections in that case so discovery
		// still observes every possible leaf.
		if linuxProbeSymbolPattern.MatchString(expanded) {
			for ordinal := markerStart; ordinal < markerEnd; ordinal++ {
				marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, ordinal)
				if !strings.Contains(expanded, marker) {
					return expanded, false, nil
				}
			}
		}

		selectionName := ""
		if len(selectedHeadMarkers) == 1 {
			selectionName = selectionByMarker[selectedHeadMarkers[0]].Name
		}
		leafValues := map[string]string{}
		leafConflict := false
		for ordinal := markerStart; ordinal < markerEnd; ordinal++ {
			marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, ordinal)
			for variable, value := range selectionByMarker[marker].leaves {
				if previous, exists := leafValues[variable]; exists && previous != value {
					leafConflict = true
					continue
				}
				leafValues[variable] = value
			}
		}
		var composite strings.Builder
		quote := byte(0)
		for offset := 0; offset < len(expanded); {
			if strings.HasPrefix(expanded[offset:], compactKbuildCommandSelectionMarker) {
				end := offset + len(compactKbuildCommandSelectionMarker) + 10
				if end <= len(expanded) {
					marker := expanded[offset:end]
					if selection, exists := selectionByMarker[marker]; exists {
						replacement := selection.Text
						switch quote {
						case '\'':
							replacement = strings.ReplaceAll(replacement, "'", "'\\''")
						case '"':
							replacement = strings.NewReplacer(
								"\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`",
							).Replace(replacement)
						}
						composite.WriteString(replacement)
						offset = end
						continue
					}
				}
			}
			character := expanded[offset]
			composite.WriteByte(character)
			if character == '\\' && quote != '\'' && offset+1 < len(expanded) {
				offset++
				composite.WriteByte(expanded[offset])
			} else if character == '\'' && quote != '"' {
				if quote == '\'' {
					quote = 0
				} else {
					quote = '\''
				}
			} else if character == '"' && quote != '\'' {
				if quote == '"' {
					quote = 0
				} else {
					quote = '"'
				}
			}
			offset++
		}
		compositeText := composite.String()
		for ordinal := markerStart; ordinal < markerEnd; ordinal++ {
			marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, ordinal)
			delete(selectionByMarker, marker)
		}
		if lineCommandAccesses != nil && accessStart >= 0 {
			*lineCommandAccesses = (*lineCommandAccesses)[:accessStart]
		}
		compositeLeaves := maps.Clone(leafValues)
		if leafConflict {
			compositeLeaves = nil
		}
		marker, markerErr := newSelectionMarker(CompactKbuildCommandTemplate{
			Name:   selectionName,
			Source: sourceLine,
			Text:   compositeText,
			leaves: compositeLeaves,
		}, false)
		return marker, true, markerErr
	}
	replayExactSelection := func(selection CompactKbuildCommandTemplate) (CompactKbuildCommandTemplate, error) {
		if selection.Source == "" || len(selection.leaves) == 0 {
			return selection, nil
		}
		// Replay only after the outer source expansion has returned and all of
		// its recursive-variable guards have unwound. Selected leaf values are
		// supplied as already-expanded locals: surrounding Make text functions
		// therefore see and transform their real bytes, while shell $() inside a
		// leaf is never parsed as a new Make reference.
		replay := exactReplayParser
		replay.vars = maps.Clone(exactReplayParser.vars)
		replay.locals = cloneLocals(exactReplayParser.locals)
		replay.expanding = map[string]bool{}
		replay.commandSelectionExpansion = nil
		replay.expandedReferencesAreLiteral = true
		replay.pushLocal(maps.Clone(selection.leaves))
		exact, err := replay.expand(selection.Source)
		replay.popLocal()
		if err != nil {
			return selection, fmt.Errorf("exact selected-wrapper replay: %w", err)
		}
		if strings.Contains(exact, "$(cmd_") {
			return selection, fmt.Errorf(
				"exact selected-wrapper replay retained a command reference in %q from %q",
				exact, selection.Source,
			)
		}
		if resolveSymbolic && linuxProbeSymbolPattern.MatchString(exact) {
			exact, err = replay.resolveKbuildSymbolic(exact)
			if err != nil {
				return selection, fmt.Errorf("exact selected-wrapper symbolic replay: %w", err)
			}
		}
		if !linuxProbeSymbolPattern.MatchString(exact) {
			selection.Text = exact
		}
		return selection, nil
	}
	observer = func(name, original string, depth int) (string, bool, error) {
		if strings.HasPrefix(name, "cmd_") {
			command := strings.TrimPrefix(name, "cmd_")
			variable, sourceDefined := parser.lookupVariable(name)
			selectedUndefined := !sourceDefined && incompleteCommandWrapper
			value := ""
			if !selectedUndefined {
				// A cmd_<name> variable is not necessarily a leaf command. Follow
				// source-defined delegation before assigning command identity. When a
				// wrapper adds executable or redirection effects after the selected
				// leaf, preserve the complete evaluated wrapper as one occurrence:
				// later source tools may consume those side effects even when Make also
				// uses them for incremental state. No helper name, output suffix, or
				// command body spelling is interpreted here.
				if variable.recursive {
					if index, cyclic := commandStackIndex[name]; cyclic {
						cycle := append(append([]string(nil), commandStack[index:]...), name)
						return "", true, fmt.Errorf(
							"target %q profile %q Kbuild command macro expansion cycle: %s",
							target, profile.Name, strings.Join(cycle, " -> "),
						)
					}
					markerStart := len(selectionByMarker)
					accessStart := -1
					if lineCommandAccesses != nil {
						accessStart = len(*lineCommandAccesses)
					}
					commandStackIndex[name] = len(commandStack)
					commandStack = append(commandStack, name)
					parser.expanding[name] = true
					nestedValue, nestedErr := parser.expandDepth(variable.value, depth)
					delete(parser.expanding, name)
					commandStack = commandStack[:len(commandStack)-1]
					delete(commandStackIndex, name)
					if nestedErr != nil {
						return "", true, nestedErr
					}
					markerEnd := len(selectionByMarker)
					if markerEnd != markerStart {
						programs, programErr := compactKbuildCompoundProgramCommands(nestedValue)
						nestedCommandHead := false
						if programErr == nil {
							for _, program := range programs {
								if _, selected := selectionByMarker[program.program]; selected {
									nestedCommandHead = true
									break
								}
							}
						}
						// A symbolic Make conditional can replace a complete branch,
						// including its command-selection marker, with one probe atom.
						// Keep those hidden leaves for discovery; a probe atom merely
						// adjacent to every still-visible marker is ordinary argv data.
						symbolicHidNestedSelection := false
						if linuxProbeSymbolPattern.MatchString(nestedValue) {
							for ordinal := markerStart; ordinal < markerEnd; ordinal++ {
								marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, ordinal)
								if !strings.Contains(nestedValue, marker) {
									symbolicHidNestedSelection = true
									break
								}
							}
						}
						if programErr != nil || nestedCommandHead || symbolicHidNestedSelection {
							return nestedValue, true, nil
						}
						// Nested command text used only as data does not make this
						// variable a wrapper. This remains true when the outer command
						// contains unresolved compiler-probe words: marker position, not
						// unrelated symbolic argv, establishes shell-command ownership.
						// Restore the exact nested text before rolling the speculative
						// markers back and selecting the outer command.
						for ordinal := markerStart; ordinal < markerEnd; ordinal++ {
							marker := fmt.Sprintf("%s%08d__", compactKbuildCommandSelectionMarker, ordinal)
							selection := selectionByMarker[marker]
							nestedValue = strings.ReplaceAll(nestedValue, marker, selection.Text)
							delete(selectionByMarker, marker)
						}
						if lineCommandAccesses != nil && accessStart >= 0 {
							*lineCommandAccesses = (*lineCommandAccesses)[:accessStart]
						}
					}
					value = nestedValue
				} else {
					var ok bool
					var commandErr error
					value, ok, commandErr = expandWithoutObserver(name, original, depth)
					if commandErr != nil {
						return value, ok, commandErr
					}
					if !ok {
						if !incompleteCommandWrapper {
							return value, false, nil
						}
						selectedUndefined = true
					}
				}
			}
			if !selectedUndefined && strings.TrimSpace(value) == "" {
				return "", true, nil
			}
			if command == "" || strings.ContainsAny(command, "$(), \t\r\n") {
				return "", true, fmt.Errorf(
					"target %q profile %q selected invalid Kbuild command variable %q",
					target, profile.Name, name,
				)
			}
			selection := CompactKbuildCommandTemplate{
				Name: command,
				Text: value,
				leaves: map[string]string{
					name: value,
				},
			}
			marker, markerErr := newSelectionMarker(selection, selectedUndefined)
			return marker, true, markerErr
		}

		variable, ok := parser.lookupVariable(name)
		if !ok {
			if incompleteRuleWrapper {
				commandName := "cmd_" + strings.TrimPrefix(name, "rule_")
				if _, commandDefined := parser.lookupVariable(commandName); commandDefined {
					return observer(commandName, "$("+commandName+")", depth)
				}
			}
			return expandWithoutObserver(name, original, depth)
		}
		if index, cyclic := ruleStackIndex[name]; cyclic {
			cycle := append(append([]string(nil), ruleStack[index:]...), name)
			return "", true, fmt.Errorf(
				"target %q profile %q Kbuild rule macro expansion cycle: %s",
				target, profile.Name, strings.Join(cycle, " -> "),
			)
		}
		if len(ruleStack) >= compactKbuildRuleSelectionDepthLimit {
			return "", true, fmt.Errorf(
				"target %q profile %q Kbuild rule macro expansion exceeds %d levels at %q",
				target, profile.Name, compactKbuildRuleSelectionDepthLimit, name,
			)
		}
		if structuralErr := validateCompactKbuildRuleMacroBody(name, variable.value); structuralErr != nil {
			return "", true, fmt.Errorf("target %q profile %q: %w", target, profile.Name, structuralErr)
		}
		ruleStackIndex[name] = len(ruleStack)
		ruleStack = append(ruleStack, name)
		defer func() {
			ruleStack = ruleStack[:len(ruleStack)-1]
			delete(ruleStackIndex, name)
		}()
		if !variable.recursive && !variable.deferredSimple {
			return variable.value, true, nil
		}
		parser.expanding[name] = true
		expandedLines := make([]string, 0, strings.Count(variable.value, "\n")+1)
		for index, sourceLine := range strings.Split(variable.value, "\n") {
			markerStart := len(selectionByMarker)
			accessStart := -1
			if lineCommandAccesses != nil {
				accessStart = len(*lineCommandAccesses)
			}
			value, expandErr := parser.expandDepth(sourceLine, depth)
			if expandErr != nil {
				delete(parser.expanding, name)
				return "", true, expandErr
			}
			value, _, expandErr = encapsulateExpandedSelectionLine(
				sourceLine, value, markerStart, accessStart,
			)
			if expandErr != nil {
				delete(parser.expanding, name)
				return "", true, expandErr
			}
			if !strings.Contains(value, compactKbuildCommandSelectionMarker) && !directExpansionIsControlOnly(value) {
				value, expandErr = preserveDirectSelection(
					fmt.Sprintf("Kbuild rule macro %q line %d", name, index+1), sourceLine, value,
				)
				if expandErr != nil {
					delete(parser.expanding, name)
					return "", true, expandErr
				}
			}
			expandedLines = append(expandedLines, value)
		}
		delete(parser.expanding, name)
		value := strings.Join(expandedLines, "\n")
		if variable.deferredSimple {
			variable.value = value
			variable.deferredSimple = false
			parser.setVariable(name, variable)
		}
		return value, true, nil
	}
	parser.commandSelectionExpansion = observer
	defer func() { parser.commandSelectionExpansion = nil }()

	selections := []CompactKbuildCommandTemplate{}
	appendExpandedSelections := func(expanded, line string, accesses []CompactKbuildCommandTemplate) (bool, error) {
		if linuxProbeSymbolPattern.MatchString(expanded) && len(accesses) != 0 {
			for _, selection := range accesses {
				replayed, replayErr := replayExactSelection(selection)
				if replayErr != nil {
					return false, replayErr
				}
				selections = append(selections, replayed)
			}
			return true, nil
		}
		if !strings.Contains(expanded, compactKbuildCommandSelectionMarker) {
			return false, nil
		}
		programs, programErr := compactKbuildCompoundProgramCommands(expanded)
		if programErr != nil {
			return false, fmt.Errorf(
				"target %q profile %q command-selection recipe %q has undecidable shell provenance: %w",
				target, profile.Name, line, programErr,
			)
		}
		selected := false
		for _, program := range programs {
			if selection, found := selectionByMarker[program.program]; found {
				replayed, replayErr := replayExactSelection(selection)
				if replayErr != nil {
					return false, replayErr
				}
				selections = append(selections, replayed)
				selected = true
				continue
			}
			if strings.Contains(program.program, compactKbuildCommandSelectionMarker) {
				return false, fmt.Errorf(
					"target %q profile %q selected Kbuild command is not a complete shell command head in %q",
					target, profile.Name, line,
				)
			}
		}
		return selected, nil
	}
	recoverExactCommandWrapper := func(line string) error {
		calls := kbuildRecipeCommandCalls([]string{line})
		if len(calls) != 1 {
			return fmt.Errorf("target %q profile %q exact command wrapper has %d calls", target, profile.Name, len(calls))
		}
		callArguments := make([]string, len(calls[0].arguments))
		var commandErr error
		for index, expression := range calls[0].arguments {
			callArguments[index], commandErr = parser.expand(expression)
			if commandErr == nil && resolveSymbolic {
				callArguments[index], commandErr = parser.resolveKbuildSymbolic(callArguments[index])
			}
			if commandErr != nil {
				break
			}
		}
		command := ""
		if len(callArguments) != 0 {
			command = callArguments[0]
		}
		command = strings.TrimSpace(command)
		if commandErr != nil || command == "" || strings.ContainsAny(command, "$(), \t\r\n") {
			if commandErr != nil {
				return fmt.Errorf("target %q profile %q command wrapper argument: %w", target, profile.Name, commandErr)
			}
			return fmt.Errorf("target %q profile %q command wrapper has invalid argument %q", target, profile.Name, command)
		}
		fallbackAccesses := []CompactKbuildCommandTemplate{}
		lineCommandAccesses = &fallbackAccesses
		variableName := "cmd_" + command
		if calls[0].wrapper == "if_changed_rule" {
			variableName = "rule_" + command
		}
		locals := map[string]string{"0": calls[0].wrapper}
		for index, argument := range callArguments {
			locals[fmt.Sprintf("%d", index+1)] = argument
		}
		parser.pushLocal(locals)
		fallback, _, fallbackErr := observer(variableName, "$("+variableName+")", 0)
		parser.popLocal()
		lineCommandAccesses = nil
		if fallbackErr != nil {
			return fallbackErr
		}
		_, fallbackErr = appendExpandedSelections(fallback, line, fallbackAccesses)
		return fallbackErr
	}
	for _, line := range recipe {
		if isKbuildRecipeDirectorySetupExpression(line) {
			continue
		}
		if _, controlEffect, controlErr := exactKbuildEvalRecipeBody(line); controlErr != nil {
			return nil, controlErr
		} else if controlEffect {
			continue
		}
		lineAccesses := []CompactKbuildCommandTemplate{}
		lineCommandAccesses = &lineAccesses
		markerStart := len(selectionByMarker)
		expanded, expandErr := parser.expand(line)
		if expandErr != nil {
			lineCommandAccesses = nil
			var classificationErr *kbuildSymbolicIfClassificationError
			if CompactKbuildRecipeIsExactCommandTemplateCall(line) && errors.As(expandErr, &classificationErr) {
				// Kbuild's if_changed family is an incremental rebuild guard. Bazel
				// always runs the selected hermetic action when its declared inputs
				// require it, so a compiler-dependent command comparison must not
				// become an action-graph condition. Recover the exact source-selected
				// leaf while retaining strict symbolic lowering everywhere else.
				if fallbackErr := recoverExactCommandWrapper(line); fallbackErr != nil {
					return nil, fallbackErr
				}
				continue
			}
			return nil, fmt.Errorf(
				"target %q profile %q expands command-selection recipe %q: %w",
				target, profile.Name, line, expandErr,
			)
		}
		expanded, _, expandErr = encapsulateExpandedSelectionLine(line, expanded, markerStart, 0)
		lineCommandAccesses = nil
		if expandErr != nil {
			return nil, fmt.Errorf(
				"target %q profile %q preserves command-selection recipe %q: %w",
				target, profile.Name, line, expandErr,
			)
		}
		if resolveSymbolic && linuxProbeSymbolPattern.MatchString(expanded) {
			expanded, expandErr = parser.resolveKbuildSymbolic(expanded)
			if expandErr != nil {
				return nil, fmt.Errorf(
					"target %q profile %q resolves command-selection recipe %q: %w",
					target, profile.Name, line, expandErr,
				)
			}
		}
		selected, selectionErr := appendExpandedSelections(expanded, line, lineAccesses)
		if selectionErr != nil {
			return nil, selectionErr
		}
		if selected {
			continue
		}
		if !CompactKbuildRecipeIsExactCommandTemplateCall(line) {
			if directExpansionIsControlOnly(expanded) {
				continue
			}
			direct, directErr := preserveDirectSelection("recipe", line, expanded)
			if directErr != nil {
				return nil, directErr
			}
			if _, directErr = appendExpandedSelections(direct, line, nil); directErr != nil {
				return nil, directErr
			}
			continue
		}

		// Kbuild's if_changed family is a rebuild guard, not an action-graph
		// selector: hermetic actions always receive freshly staged inputs. If an
		// exact wrapper suppresses its body because the captured Make state says
		// the target is current, recover the source-selected leaf directly. Keep
		// this fallback restricted to an exact wrapper-only recipe so a command in
		// an unselected surrounding Make branch is not manufactured.
		if fallbackErr := recoverExactCommandWrapper(line); fallbackErr != nil {
			return nil, fallbackErr
		}
	}
	return selections, nil
}

// validateCompactKbuildRuleMacroBody rejects direct shell text in rule_<name>
// definitions. Leaf shell programs belong in cmd_<name>; accepting opaque text
// here would silently drop it while flattening the nested rule into commands.
func validateCompactKbuildRuleMacroBody(name, body string) error {
	for index, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		for line != "" && strings.ContainsRune("@+-", rune(line[0])) {
			line = strings.TrimSpace(line[1:])
		}
		for line != "" {
			if len(line) < 3 || line[0] != '$' || (line[1] != '(' && line[1] != '{') {
				return fmt.Errorf(
					"Kbuild rule macro %q line %d contains direct shell text %q",
					name, index+1, line,
				)
			}
			end, err := matchingKbuildReference(line, 1)
			if err != nil {
				return fmt.Errorf("Kbuild rule macro %q line %d: %w", name, index+1, err)
			}
			line = strings.TrimSpace(line[end+1:])
		}
	}
	return nil
}

func evaluatedKbuildRuleCommands(
	target string,
	match compactKbuildRuleMatch,
) ([]CompactKbuildCommandTemplate, bool, error) {
	if len(kbuildRecipeCommandExpressions(match.rule.Recipe)) == 0 {
		direct, err := evaluatedKbuildDirectRecipeHasAction(target, match)
		if err != nil {
			return nil, false, err
		}
		if direct {
			return []CompactKbuildCommandTemplate{{Name: compactKbuildDirectRecipeCommand}}, true, nil
		}
		return nil, false, nil
	}
	automatic, err := compactKbuildRuleAutomaticEvaluationContext(target, match, nil)
	if err != nil {
		return nil, false, err
	}
	selections, err := evaluatedKbuildRuleCommandSelectionsForMakeTarget(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, nil, match.rule.Recipe, true,
	)
	if err != nil {
		return nil, false, err
	}
	hasAction := false
	for _, selection := range selections {
		if selection.Name == "" {
			// Direct occurrences are source-selected executable text. Whether the
			// command writes the declared target, only a side output, or requires an
			// unconfigured tool is a final action-lowering decision; graph selection
			// must not erase it merely because it is not a cmd_<name> leaf.
			hasAction = true
			continue
		}
		template := selection.Text
		if strings.TrimSpace(template) != "" {
			evaluated := compactKbuildDirectRecipeText(match.profile, template)
			parsed, parseErr := parseCompactKbuildRecipe(evaluated, automatic)
			if parseErr == nil {
				templateAction, _, effectsErr := evaluatedKbuildParsedRecipeEffects(target, match, parsed)
				if effectsErr != nil {
					return nil, false, fmt.Errorf("target %q profile %q command %q effects: %w", target, match.profile.Name, selection.Name, effectsErr)
				}
				if !templateAction {
					scripts, scriptsErr := ReadCompactKbuildCommandSourceScripts(
						match.profile, target, automatic.stem,
						automatic.normal, automatic.order, nil, evaluated,
					)
					if scriptsErr != nil {
						return nil, false, fmt.Errorf("target %q profile %q command %q source scripts: %w", target, match.profile.Name, selection.Name, scriptsErr)
					}
					templateAction = len(scripts) != 0
				}
				hasAction = hasAction || templateAction
			} else {
				// Unsupported compound shell remains a selected source action. The
				// hermetic lowering site will either model it or reject it with the
				// complete evaluated recipe.
				hasAction = true
			}
		} else {
			// Preserve an omitted fixture/source definition so lowering reports
			// the missing cmd_<name> variable at the selected rule.
			hasAction = true
		}
	}
	if len(selections) == 0 || !hasAction {
		return nil, false, nil
	}
	return selections, true, nil
}

func evaluatedKbuildRuleCommandTemplate(
	target string,
	match compactKbuildRuleMatch,
	automatic compactKbuildAutomaticContext,
	command string,
) (string, bool, error) {
	parser, cleanup, err := compactKbuildTargetParserWithExportsForLookup(
		match.profile, target, match.lookupTarget, automatic.target, automatic.stem,
		automatic.normal, automatic.order, nil, true, false,
	)
	if err != nil {
		return "", false, err
	}
	defer cleanup()
	preserveUnsupportedKbuildRecipeShell(parser, match.profile, target)
	name := "cmd_" + command
	if _, defined := parser.lookupVariable(name); !defined {
		// Resolution tests and incomplete discovery fixtures may intentionally
		// omit the command definition. Preserve the selected source call so the
		// eventual lowering site reports that missing template; only a defined,
		// empty variable is a Kbuild-selected no-op.
		return "", false, nil
	}
	value, ok, err := parser.expandVariable(name, "$("+name+")", 0)
	if err != nil {
		return "", true, fmt.Errorf("target %q profile %q variable %s: %w", target, match.profile.Name, name, err)
	}
	if !ok {
		return "", true, nil
	}
	value, err = parser.resolveKbuildSymbolic(value)
	if err != nil {
		return "", true, fmt.Errorf("target %q profile %q variable %s symbolic result: %w", target, match.profile.Name, name, err)
	}
	return value, true, nil
}

// kbuildRecipeCommandExpressions extracts the ordered command arguments from
// Kbuild's source-level command-call DSL. Linux extensions use names such as
// if_changed_except in addition to the stock if_changed variants; the macro
// family, rather than a planner-side list of spellings, determines whether a
// call selects a cmd_<name> template.
func kbuildRecipeCommandCalls(recipe []string) []compactKbuildRecipeCommandCall {
	calls := []compactKbuildRecipeCommandCall{}
	for _, line := range recipe {
		for cursor := 0; cursor < len(line); {
			relative := strings.Index(line[cursor:], "$(call")
			if relative < 0 {
				break
			}
			start := cursor + relative
			end, err := matchingKbuildReference(line, start+1)
			if err != nil {
				break
			}
			function, arguments, ok := splitMakeFunction(line[start+2 : end])
			if !ok || function != "call" || len(arguments) < 2 {
				cursor = start + 2
				continue
			}
			wrapper := strings.TrimSpace(arguments[0])
			if wrapper == "cmd" || wrapper == "if_changed" || strings.HasPrefix(wrapper, "if_changed_") {
				if expression := strings.TrimSpace(arguments[1]); expression != "" {
					calls = append(calls, compactKbuildRecipeCommandCall{
						wrapper: wrapper, expression: expression,
						arguments: append([]string(nil), arguments[1:]...),
					})
				}
			}
			cursor = end + 1
		}
	}
	return calls
}

func kbuildRecipeCommandExpressions(recipe []string) []string {
	calls := kbuildRecipeCommandCalls(recipe)
	expressions := make([]string, 0, len(calls))
	for _, call := range calls {
		expressions = append(expressions, call.expression)
	}
	return expressions
}

func canonicalKbuildRulePath(value string) string {
	value = strings.TrimSpace(filepathToSlash(value))
	value = strings.TrimPrefix(value, "./")
	if value == "" || value == "." {
		return ""
	}
	trailingSlash := strings.HasSuffix(value, "/") && value != "/"
	value = path.Clean(value)
	if value == "." {
		return ""
	}
	if trailingSlash && value != "/" {
		value += "/"
	}
	return value
}

// compactKbuildLexicalTraversalWorkingDirectories returns the canonical
// directories which a lexical Make target must traverse before each `..` path
// component can be resolved.  Cleaning the target first would erase exactly
// the directories whose existence POSIX path lookup requires.  The final
// component stack is also checked against the graph target so these declarative
// directories can never widen a recipe beyond its already-selected output.
func compactKbuildLexicalTraversalWorkingDirectories(lexicalTarget, graphTarget string) ([]string, error) {
	lexicalTarget = strings.TrimSpace(filepathToSlash(lexicalTarget))
	graphTarget = canonicalKbuildRulePath(graphTarget)
	if lexicalTarget == "" || graphTarget == "" || strings.HasPrefix(lexicalTarget, "/") || strings.ContainsAny(lexicalTarget, "\x00\r\n") {
		return nil, fmt.Errorf("lexical target %q cannot resolve graph target %q below the working root", lexicalTarget, graphTarget)
	}

	components := make([]string, 0, strings.Count(lexicalTarget, "/")+1)
	directories := map[string]bool{}
	for _, component := range strings.Split(lexicalTarget, "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(components) == 0 {
				return nil, fmt.Errorf("lexical target %q escapes the working root", lexicalTarget)
			}
			directory := strings.Join(components, "/")
			if err := validatePlanRelativePath("lexical target traversal directory", directory); err != nil {
				return nil, err
			}
			directories[directory] = true
			components = components[:len(components)-1]
		default:
			components = append(components, component)
		}
	}
	resolved := strings.Join(components, "/")
	if resolved != graphTarget {
		return nil, fmt.Errorf("lexical target %q resolves to %q, want graph target %q", lexicalTarget, resolved, graphTarget)
	}
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result, nil
}

func filepathToSlash(value string) string {
	return strings.ReplaceAll(value, "\\", "/")
}

func matchKbuildRulePattern(pattern, target string) (string, bool) {
	prefix, suffix, hasPattern := strings.Cut(pattern, "%")
	if !hasPattern {
		return "", pattern == target
	}
	if strings.Contains(suffix, "%") || len(target) < len(prefix)+len(suffix) ||
		!strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, suffix) {
		return "", false
	}
	return target[len(prefix) : len(target)-len(suffix)], true
}

func instantiateKbuildRulePattern(pattern, stem string) string {
	if strings.Count(pattern, "%") != 1 {
		return pattern
	}
	return strings.Replace(pattern, "%", stem, 1)
}
