package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

type compactKbuildOverwriteDependency struct {
	producer compactKbuildSelectionKey
	path     string
}

type compactKbuildOverwriteEdge struct {
	before compactKbuildSelectionKey
	after  compactKbuildSelectionKey
	path   string
}

// compactKbuildSelectionOwnsPath reports whether selection is one of the
// source-selected writers registered for path. Grouped output peers share one
// physical recipe, so any member owns every path published by that recipe.
func (g *compactKbuildSelectionGraph) compactKbuildSelectionOwnsPath(
	selection compactKbuildSelectionKey,
	target string,
) bool {
	if g == nil {
		return false
	}
	target = canonicalKbuildRulePath(target)
	for _, owner := range g.outputOwnersByPath[target] {
		if g.compactKbuildSelectionsShareProducer(selection, owner) {
			return true
		}
	}
	return false
}

// compactKbuildRegisterOutputOwners records every exact selected writer of a
// logical output, including opaque side outputs whose Path is not the selected
// target. Registration happens before materialization order is computed.
func (g *compactKbuildSelectionGraph) compactKbuildRegisterOutputOwners(
	logicalPath, tree string,
	owners []compactKbuildSelectionKey,
	primary bool,
) ([]compactKbuildSelectionKey, error) {
	logicalPath = canonicalKbuildRulePath(logicalPath)
	groupKey := actionPlanLookupKey(tree, logicalPath)
	merged := append([]compactKbuildSelectionKey(nil), g.outputOwnerCandidates[groupKey]...)
	seen := make(map[compactKbuildSelectionKey]bool, len(merged)+len(owners))
	for _, owner := range merged {
		seen[owner] = true
	}
	for _, owner := range owners {
		if !seen[owner] {
			seen[owner] = true
			merged = append(merged, owner)
		}
	}
	ordered, err := g.compactKbuildTotalOverwriteOrder(logicalPath, tree, merged)
	if err != nil {
		return nil, err
	}
	g.outputOwnerCandidates[groupKey] = append([]compactKbuildSelectionKey(nil), ordered...)
	g.publishedOwners[groupKey] = ordered[len(ordered)-1]
	byPath := append([]compactKbuildSelectionKey(nil), g.outputOwnersByPath[logicalPath]...)
	pathSeen := make(map[compactKbuildSelectionKey]bool, len(byPath)+len(ordered))
	for _, owner := range byPath {
		pathSeen[owner] = true
	}
	for _, owner := range ordered {
		if !pathSeen[owner] {
			pathSeen[owner] = true
			byPath = append(byPath, owner)
		}
	}
	sort.Slice(byPath, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(byPath[i], byPath[j])
	})
	g.outputOwnersByPath[logicalPath] = byPath
	if primary {
		for index, owner := range ordered {
			if index+1 != len(ordered) {
				g.shadowedPrimarySelections[owner] = true
			}
		}
	}
	edges := make([]compactKbuildOverwriteEdge, 0, len(ordered)-1)
	for index := 1; index < len(ordered); index++ {
		edges = append(edges, compactKbuildOverwriteEdge{
			before: ordered[index-1], after: ordered[index], path: logicalPath,
		})
	}
	if len(edges) == 0 {
		// Unique writers have no overwrite relationship to project. Omitting the
		// empty group keeps the one batch rebuild proportional to actual overwrite
		// chains instead of to every ordinary selected output.
		delete(g.overwriteEdgesByOutput, groupKey)
	} else {
		g.overwriteEdgesByOutput[groupKey] = edges
	}
	return ordered, nil
}

func (g *compactKbuildSelectionGraph) compactKbuildRebuildOverwriteDependencies() {
	g.overwriteDependencies = make(map[compactKbuildSelectionKey][]compactKbuildOverwriteDependency)
	keys := make([]string, 0, len(g.overwriteEdgesByOutput))
	for key := range g.overwriteEdgesByOutput {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := map[struct {
		after    compactKbuildSelectionKey
		producer compactKbuildSelectionKey
		path     string
	}]bool{}
	for _, key := range keys {
		for _, edge := range g.overwriteEdgesByOutput[key] {
			// An overwrite constrains the one grouped recipe, regardless of
			// which selected peer first causes that recipe to be materialized.
			for _, after := range g.compactKbuildGroupedSelectionMembers(edge.after) {
				identity := struct {
					after    compactKbuildSelectionKey
					producer compactKbuildSelectionKey
					path     string
				}{after: after, producer: edge.before, path: edge.path}
				if seen[identity] {
					continue
				}
				seen[identity] = true
				g.overwriteDependencies[after] = append(
					g.overwriteDependencies[after],
					compactKbuildOverwriteDependency{producer: edge.before, path: edge.path},
				)
			}
		}
	}
}

func (g *compactKbuildSelectionGraph) compactKbuildOverwriteReachability(
	target string,
	owners []compactKbuildSelectionKey,
) (func(compactKbuildSelectionKey, compactKbuildSelectionKey) bool, error) {
	ownerSet := make(map[compactKbuildSelectionKey]bool, len(owners))
	for _, owner := range owners {
		ownerSet[owner] = true
	}
	edges := make(map[compactKbuildSelectionKey]map[compactKbuildSelectionKey]bool, len(owners))
	addEdge := func(before, after compactKbuildSelectionKey) {
		if before == after || !ownerSet[before] || !ownerSet[after] {
			return
		}
		if edges[before] == nil {
			edges[before] = map[compactKbuildSelectionKey]bool{}
		}
		edges[before][after] = true
	}
	for _, after := range owners {
		if artifact, ok := g.compactKbuildInitialVisibleArtifact(after.profile, target); ok {
			before, err := g.compactKbuildVisibleArtifactOwner(artifact)
			if err != nil {
				return nil, fmt.Errorf(
					"Kbuild native artifact %q overwrite frontier for %s: %w",
					target, compactKbuildSelectionKeyString(after), err,
				)
			}
			addEdge(before, after)
		}
		for _, predecessorProfile := range g.compactKbuildInvocationPredecessorClosure(after.profile) {
			for _, before := range owners {
				if before.profile == predecessorProfile {
					addEdge(before, after)
				}
			}
		}
	}
	// The root prepare closure is a source-defined prefix of the target
	// closure. The same logical writer selected through both lifecycles can
	// therefore live in different immutable Bazel stage trees without an
	// ActionPlan edge, while its target-lifecycle version still overwrites the
	// preparation version in Make's one logical object tree.
	for _, before := range owners {
		beforeLifecycle, _, err := compactKbuildSelectionLifecycleScope(g.selections[before])
		if err != nil {
			return nil, err
		}
		if beforeLifecycle != "prep" {
			continue
		}
		for _, after := range owners {
			afterLifecycle, _, err := compactKbuildSelectionLifecycleScope(g.selections[after])
			if err != nil {
				return nil, err
			}
			if afterLifecycle == "target" {
				addEdge(before, after)
			}
		}
	}

	reachable := func(from, to compactKbuildSelectionKey) bool {
		pending := []compactKbuildSelectionKey{from}
		seen := map[compactKbuildSelectionKey]bool{}
		for len(pending) != 0 {
			last := len(pending) - 1
			candidate := pending[last]
			pending = pending[:last]
			if candidate == to {
				return true
			}
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			for successor := range edges[candidate] {
				pending = append(pending, successor)
			}
		}
		return false
	}
	return reachable, nil
}

// compactKbuildTotalOverwriteOrder accepts multiple writers in one physical
// output tree only when the source-derived invocation frontier proves a total
// order. A merely deterministic serialization order is not evidence: every
// pair must be comparable through an exact visible-artifact, invocation
// predecessor, or lifecycle-prefix edge.
func (g *compactKbuildSelectionGraph) compactKbuildTotalOverwriteOrder(
	target, tree string,
	owners []compactKbuildSelectionKey,
) ([]compactKbuildSelectionKey, error) {
	owners = append([]compactKbuildSelectionKey(nil), owners...)
	sort.Slice(owners, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(owners[i], owners[j])
	})
	if len(owners) < 2 {
		return owners, nil
	}
	reachable, err := g.compactKbuildOverwriteReachability(target, owners)
	if err != nil {
		return nil, err
	}
	for leftIndex, left := range owners {
		for _, right := range owners[leftIndex+1:] {
			leftBeforeRight := reachable(left, right)
			rightBeforeLeft := reachable(right, left)
			if leftBeforeRight && rightBeforeLeft {
				return nil, fmt.Errorf(
					"Kbuild native artifact %q overwrite provenance in physical tree %q contains a cycle between %s and %s",
					target, tree, compactKbuildSelectionKeyString(left), compactKbuildSelectionKeyString(right),
				)
			}
			if !leftBeforeRight && !rightBeforeLeft {
				labels := make([]string, 0, len(owners))
				for _, owner := range owners {
					labels = append(labels, compactKbuildSelectionKeyString(owner))
				}
				return nil, fmt.Errorf(
					"Kbuild native artifact %q has ambiguous owners in physical tree %q: %s; exact overwrite provenance leaves them unordered",
					target, tree, strings.Join(labels, " and "),
				)
			}
		}
	}
	sort.SliceStable(owners, func(i, j int) bool { return reachable(owners[i], owners[j]) })
	return owners, nil
}

// compactKbuildSourceOrderedPathProducer compares two materialized versions
// of one logical pathname using only Kbuild's recorded overwrite provenance.
// The versions may live in different physical stage trees, so they do not
// compete for one Bazel output and do not necessarily have an ActionPlan edge
// between them. A later consumer whose writable-tree closure contains both
// versions must nevertheless stage the source-ordered tail. If the profiles'
// exact visible frontiers and invocation predecessors do not order every
// matching selection pair, no winner is returned.
func (g *compactKbuildSelectionGraph) compactKbuildSourceOrderedPathProducer(
	target, leftProducer, rightProducer string,
) (string, bool, error) {
	if g == nil || leftProducer == "" || rightProducer == "" || leftProducer == rightProducer {
		return "", false, nil
	}
	target = canonicalKbuildRulePath(target)
	// Primary and statically known outputs are indexed by logical pathname.
	// Ordinary materialized side outputs are not: their physical observations
	// only exist after the selected recipe has been lowered. The caller has
	// already established that both producer nodes emit target, so augment this
	// comparison's owner set from the exact selection-to-producer mapping when
	// the pathname index cannot identify both versions. This is deliberately a
	// local view; publishing these owners would make an observed side output
	// alter global ownership and overwrite dependencies.
	owners := make([]compactKbuildSelectionKey, 0, len(g.outputOwnersByPath[target]))
	seen := make(map[compactKbuildSelectionKey]bool, len(g.outputOwnersByPath[target]))
	mapped := map[string]bool{}
	appendOwner := func(owner compactKbuildSelectionKey) {
		if seen[owner] {
			return
		}
		seen[owner] = true
		owners = append(owners, owner)
		producer := g.materializedProducers[owner]
		if producer == leftProducer || producer == rightProducer {
			mapped[producer] = true
		}
	}
	for _, owner := range g.outputOwnersByPath[target] {
		appendOwner(owner)
	}
	if !mapped[leftProducer] || !mapped[rightProducer] {
		for owner, producer := range g.materializedProducers {
			if producer == leftProducer || producer == rightProducer {
				appendOwner(owner)
			}
		}
	}
	sort.Slice(owners, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(owners[i], owners[j])
	})
	reachable, err := g.compactKbuildOverwriteReachability(target, owners)
	if err != nil {
		return "", false, err
	}
	left := []compactKbuildSelectionKey{}
	right := []compactKbuildSelectionKey{}
	for _, owner := range owners {
		producer := g.materializedProducers[owner]
		switch producer {
		case leftProducer:
			left = append(left, owner)
		case rightProducer:
			right = append(right, owner)
		}
	}
	if len(left) == 0 || len(right) == 0 {
		return "", false, nil
	}
	winner := ""
	for _, leftOwner := range left {
		for _, rightOwner := range right {
			leftBeforeRight := reachable(leftOwner, rightOwner)
			rightBeforeLeft := reachable(rightOwner, leftOwner)
			if leftBeforeRight && rightBeforeLeft {
				return "", false, fmt.Errorf(
					"Kbuild working object-tree path %q has cyclic overwrite provenance between %s and %s",
					target, compactKbuildSelectionKeyString(leftOwner), compactKbuildSelectionKeyString(rightOwner),
				)
			}
			if !leftBeforeRight && !rightBeforeLeft {
				return "", false, nil
			}
			candidate := rightProducer
			if rightBeforeLeft {
				candidate = leftProducer
			}
			if winner != "" && winner != candidate {
				return "", false, fmt.Errorf(
					"Kbuild working object-tree path %q has inconsistent overwrite provenance for producers %q and %q",
					target, leftProducer, rightProducer,
				)
			}
			winner = candidate
		}
	}
	return winner, winner != "", nil
}

func (g *compactKbuildSelectionGraph) compactKbuildInvocationPredecessorClosure(profileName string) []string {
	if g == nil {
		return nil
	}
	pending := append([]string(nil), g.profiles[profileName].InvocationPredecessors...)
	seen := map[string]bool{}
	result := []string{}
	for len(pending) != 0 {
		profile := pending[0]
		pending = pending[1:]
		if profile == "" || seen[profile] {
			continue
		}
		seen[profile] = true
		result = append(result, profile)
		pending = append(pending, g.profiles[profile].InvocationPredecessors...)
	}
	sort.Strings(result)
	return result
}

// compactKbuildSelectionPathOwner resolves a prerequisite as it was visible
// to one exact selected invocation. In particular, it never falls back to the
// canonical tail of a same-path overwrite chain: a consumer evaluated between
// two writes must continue to consume the earlier immutable version.
func (g *compactKbuildSelectionGraph) compactKbuildSelectionPathOwner(
	consumer compactKbuildSelectionKey,
	target string,
) (compactKbuildSelectionKey, bool, error) {
	return g.compactKbuildSelectionPathOwnerWithFallback(consumer, target, true)
}

func (g *compactKbuildSelectionGraph) compactKbuildSelectionRecordedPathOwner(
	consumer compactKbuildSelectionKey,
	target string,
) (compactKbuildSelectionKey, bool, error) {
	return g.compactKbuildSelectionPathOwnerWithFallback(consumer, target, false)
}

// compactKbuildSelectionRecordedPathOwnerWithPredecessorClosure is the
// analysis-context variant of compactKbuildSelectionRecordedPathOwner. The
// selection graph remains immutable while config dependency analysis runs, so
// that analysis may memoize its repeatedly requested invocation closures
// without adding mutable cache state to the graph itself.
func (g *compactKbuildSelectionGraph) compactKbuildSelectionRecordedPathOwnerWithPredecessorClosure(
	consumer compactKbuildSelectionKey,
	target string,
	predecessorClosure func(string) []string,
) (compactKbuildSelectionKey, bool, error) {
	return g.compactKbuildSelectionPathOwnerWithFallbackAndPredecessorClosure(
		consumer, target, false, predecessorClosure,
	)
}

type compactKbuildUnrecordedPathOwnerError struct {
	message string
}

func (e *compactKbuildUnrecordedPathOwnerError) Error() string {
	return e.message
}

func compactKbuildPathOwnerIsUnrecorded(err error) bool {
	_, ok := err.(*compactKbuildUnrecordedPathOwnerError)
	return ok
}

func (g *compactKbuildSelectionGraph) compactKbuildSelectionPathOwnerWithFallback(
	consumer compactKbuildSelectionKey,
	target string,
	allowUniqueWriterFallback bool,
) (compactKbuildSelectionKey, bool, error) {
	return g.compactKbuildSelectionPathOwnerWithFallbackAndPredecessorClosure(
		consumer, target, allowUniqueWriterFallback, nil,
	)
}

func (g *compactKbuildSelectionGraph) compactKbuildSelectionPathOwnerWithFallbackAndPredecessorClosure(
	consumer compactKbuildSelectionKey,
	target string,
	allowUniqueWriterFallback bool,
	predecessorClosure func(string) []string,
) (compactKbuildSelectionKey, bool, error) {
	if g == nil {
		return compactKbuildSelectionKey{}, false, nil
	}
	target = canonicalKbuildRulePath(target)
	candidates := make([]compactKbuildSelectionKey, 0, len(g.outputOwnersByPath[target]))
	for _, candidate := range g.outputOwnersByPath[target] {
		if candidate == consumer || g.forwardingSelections[candidate] {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return compactKbuildSelectionKey{}, false, nil
	}
	candidateSet := make(map[compactKbuildSelectionKey]bool, len(candidates))
	for _, candidate := range candidates {
		candidateSet[candidate] = true
	}
	if allowUniqueWriterFallback {
		if sameInvocation, ok := g.selectionsByProfileTarget[compactKbuildProfileTargetKey{
			profile: consumer.profile,
			target:  target,
		}]; ok && candidateSet[sameInvocation] && sameInvocation != consumer && !g.forwardingSelections[sameInvocation] {
			return sameInvocation, true, nil
		}
	}
	_, ok := g.selections[consumer]
	if !ok {
		return compactKbuildSelectionKey{}, false, fmt.Errorf(
			"missing Kbuild consumer selection %s",
			compactKbuildSelectionKeyString(consumer),
		)
	}
	var frontierOwner *compactKbuildSelectionKey
	for _, artifact := range compactKbuildVisibleArtifactsForPath(g.selectionInitialArtifacts[consumer], target) {
		owner, err := g.compactKbuildVisibleArtifactOwner(artifact)
		if err != nil {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q visible frontier: %w",
				compactKbuildSelectionKeyString(consumer), target, err,
			)
		}
		if owner == consumer {
			continue
		}
		if !candidateSet[owner] {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q visible owner %s is not a registered writer of that logical path",
				compactKbuildSelectionKeyString(consumer), target,
				compactKbuildSelectionKeyString(owner),
			)
		}
		if frontierOwner != nil && *frontierOwner != owner {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q has conflicting exact visible owners %s and %s",
				compactKbuildSelectionKeyString(consumer), target,
				compactKbuildSelectionKeyString(*frontierOwner), compactKbuildSelectionKeyString(owner),
			)
		}
		copy := owner
		frontierOwner = &copy
	}
	if frontierOwner != nil {
		return *frontierOwner, true, nil
	}
	var generatedOwner *compactKbuildSelectionKey
	for _, artifact := range compactKbuildVisibleArtifactsForPath(g.selectionGeneratedArtifacts[consumer], target) {
		owner, err := g.compactKbuildVisibleArtifactOwner(artifact)
		if err != nil {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q generated frontier: %w",
				compactKbuildSelectionKeyString(consumer), target, err,
			)
		}
		if owner == consumer {
			continue
		}
		if !candidateSet[owner] {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q generated owner %s is not a registered writer of that logical path",
				compactKbuildSelectionKeyString(consumer), target,
				compactKbuildSelectionKeyString(owner),
			)
		}
		if generatedOwner != nil && *generatedOwner != owner {
			return compactKbuildSelectionKey{}, false, fmt.Errorf(
				"Kbuild selection %s prerequisite %q has conflicting exact generated owners %s and %s",
				compactKbuildSelectionKeyString(consumer), target,
				compactKbuildSelectionKeyString(*generatedOwner), compactKbuildSelectionKeyString(owner),
			)
		}
		copy := owner
		generatedOwner = &copy
	}
	if generatedOwner != nil {
		return *generatedOwner, true, nil
	}
	if allowUniqueWriterFallback {
		// A declared native prerequisite proves that this target is observed by
		// the selected recipe. When the target has no writer in the current Make
		// invocation, the invocation's exact initial frontier identifies which
		// version was visible. This matters when a parent invocation rewrites a
		// path and then starts a child which names that path as a prerequisite:
		// unrelated earlier invocations may have selected the same logical target.
		var invocationOwner *compactKbuildSelectionKey
		if artifact, ok := g.compactKbuildInitialVisibleArtifact(consumer.profile, target); ok {
			owner, err := g.compactKbuildVisibleArtifactOwner(artifact)
			if err != nil {
				return compactKbuildSelectionKey{}, false, fmt.Errorf(
					"Kbuild selection %s prerequisite %q invocation frontier: %w",
					compactKbuildSelectionKeyString(consumer), target, err,
				)
			}
			if owner != consumer {
				if !candidateSet[owner] {
					return compactKbuildSelectionKey{}, false, fmt.Errorf(
						"Kbuild selection %s prerequisite %q invocation owner %s is not a registered writer of that logical path",
						compactKbuildSelectionKeyString(consumer), target,
						compactKbuildSelectionKeyString(owner),
					)
				}
				copy := owner
				invocationOwner = &copy
			}
		}
		if invocationOwner != nil {
			return *invocationOwner, true, nil
		}
	}

	predecessorProfiles := map[string]bool{}
	var closure []string
	if predecessorClosure != nil {
		closure = predecessorClosure(consumer.profile)
	} else {
		closure = g.compactKbuildInvocationPredecessorClosure(consumer.profile)
	}
	for _, profileName := range closure {
		predecessorProfiles[profileName] = true
	}
	predecessors := []compactKbuildSelectionKey{}
	for _, candidate := range candidates {
		if predecessorProfiles[candidate.profile] {
			predecessors = append(predecessors, candidate)
		}
	}
	if len(predecessors) == 1 {
		return predecessors[0], true, nil
	}
	if len(candidates) == 1 && allowUniqueWriterFallback {
		return candidates[0], true, nil
	}
	if len(candidates) == 1 {
		// A unique writer is not execution provenance. Command arguments may be
		// path-shaped data (for example archive member names consumed by printf),
		// and binding them to an unrelated selected target would manufacture an
		// ordering edge which GNU Make never declared.
		return compactKbuildSelectionKey{}, false, nil
	}
	labels := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		labels = append(labels, compactKbuildSelectionKeyString(candidate))
	}
	sort.Strings(labels)
	return compactKbuildSelectionKey{}, false, &compactKbuildUnrecordedPathOwnerError{message: fmt.Sprintf(
		"Kbuild selection %s prerequisite %q has %d selected owners without exact invocation provenance: %s",
		compactKbuildSelectionKeyString(consumer), target, len(candidates), strings.Join(labels, " and "),
	)}
}

func compactKbuildSelectionArtifactVersionID(key compactKbuildSelectionKey) string {
	digest := sha256.Sum256([]byte(
		"linux-bzl-kbuild-selection-v1\x00" + key.profile + "\x00" + key.target + "\x00" + key.stage,
	))
	return hex.EncodeToString(digest[:])
}

func (g *compactKbuildSelectionGraph) compactKbuildSelectionOutput(
	selection compactKbuildSelectionKey,
	output ActionPlanOutput,
) ActionPlanOutput {
	if g == nil || output.Path == "" || output.ArtifactPath != "" {
		return output
	}
	publisher, exists := g.publishedOwner(output.Tree, output.Path)
	if !exists || g.compactKbuildSelectionsShareProducer(publisher, selection) {
		return output
	}
	output.ArtifactPath = path.Join(
		".linux-bzl-versions",
		compactKbuildSelectionArtifactVersionID(selection),
		output.Path,
	)
	return output
}
