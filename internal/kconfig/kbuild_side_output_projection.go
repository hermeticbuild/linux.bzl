package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// A projection is immutable after construction. Paths with the same candidate
// set may share it; consumers receive owned parent slices, never the stored DAG.
type compactKbuildSideOutputProjection struct {
	graph compactKbuildSideOutputStateGraph
}

type compactKbuildSideOutputProjections struct {
	byPath          map[string]*compactKbuildSideOutputProjection
	maximalByDemand map[compactKbuildSideOutputDemandStateKey][]compactKbuildSelectionKey
}

func (p *compactKbuildSideOutputProjection) parentsFor(candidate compactKbuildSelectionKey) ([]compactKbuildSelectionKey, bool) {
	parents, exists := p.graph.parents[candidate]
	return slices.Clone(parents), exists
}

func (g *compactKbuildSelectionGraph) canonicalSideOutputCandidates(candidates []compactKbuildSelectionKey) ([]compactKbuildSelectionKey, error) {
	if g == nil {
		return nil, fmt.Errorf("cannot canonicalize side-output candidates without selections")
	}
	seen := make(map[compactKbuildSelectionKey]bool, len(candidates))
	ordered := make([]compactKbuildSelectionKey, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = g.compactKbuildGroupedSelectionRepresentative(candidate)
		if _, exists := g.selections[candidate]; !exists {
			return nil, fmt.Errorf("side-output state candidate %s is not selected", compactKbuildSelectionKeyString(candidate))
		}
		if !seen[candidate] {
			seen[candidate] = true
			ordered = append(ordered, candidate)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(ordered[i], ordered[j])
	})
	return ordered, nil
}

// Encode exact, length-prefixed fields: diagnostic selection spellings are
// ambiguous, and JSON would normalize invalid UTF-8. No digest collision or
// source-name delimiter can merge distinct candidate sets here.
func compactKbuildSideOutputProjectionKey(candidates []compactKbuildSelectionKey) string {
	var key strings.Builder
	for _, candidate := range candidates {
		for _, field := range []string{candidate.profile, candidate.target, candidate.stage} {
			key.WriteString(strconv.Itoa(len(field)))
			key.WriteByte(':')
			key.WriteString(field)
		}
	}
	return key.String()
}

// Intern only within this one structurally fixed planning phase. Nothing is
// cached on the mutable selection graph or across configurations/metadata.
// Every new candidate set still undergoes complete graph/cycle validation.
func (g *compactKbuildSelectionGraph) sideOutputStateProjections(metadata *CompactMetadata, candidatesByPath map[string]map[compactKbuildSelectionKey]bool, demands []compactKbuildSideOutputDemand) (compactKbuildSideOutputProjections, error) {
	if g == nil || metadata == nil {
		return compactKbuildSideOutputProjections{}, fmt.Errorf("cannot project side-output state without selections and metadata")
	}
	byCandidates := map[string]*compactKbuildSideOutputProjection{}
	project := func(candidates []compactKbuildSelectionKey, retain bool) (*compactKbuildSideOutputProjection, error) {
		candidates, err := g.canonicalSideOutputCandidates(candidates)
		if err != nil {
			return nil, err
		}
		key := compactKbuildSideOutputProjectionKey(candidates)
		projection, exists := byCandidates[key]
		if !exists {
			graph, err := g.compactKbuildSideOutputStateGraph(metadata, candidates)
			if err != nil {
				return nil, err
			}
			projection = &compactKbuildSideOutputProjection{graph: graph}
			if retain {
				byCandidates[key] = projection
			}
		}
		return projection, nil
	}
	result := compactKbuildSideOutputProjections{
		byPath:          make(map[string]*compactKbuildSideOutputProjection, len(candidatesByPath)),
		maximalByDemand: make(map[compactKbuildSideOutputDemandStateKey][]compactKbuildSelectionKey, len(demands)),
	}
	for _, observedPath := range slices.Sorted(maps.Keys(candidatesByPath)) {
		projection, err := project(slices.Collect(maps.Keys(candidatesByPath[observedPath])), true)
		if err != nil {
			return compactKbuildSideOutputProjections{}, fmt.Errorf("project Kbuild side-output state for %q: %w", observedPath, err)
		}
		result.byPath[observedPath] = projection
	}
	// A demand may observe only a subset of the path's producers. Intern its
	// exact set when already retained for a path, never borrow the maximal
	// frontier of a larger path-wide union. Do not cache demand-only graphs:
	// keep only their owned maximal frontiers, as the original loop did. This
	// avoids retaining one full projection per distinct consumer subset.
	for _, demand := range demands {
		projection, err := project(demand.candidates, false)
		if err != nil {
			return compactKbuildSideOutputProjections{}, fmt.Errorf("project final Kbuild side-output state for %q: %w", demand.output.Path, err)
		}
		key := compactKbuildSideOutputDemandStateKey{
			consumer: g.compactKbuildGroupedSelectionRepresentative(demand.consumer),
			tree:     demand.output.Tree,
			path:     demand.output.Path,
		}
		if previous, exists := result.maximalByDemand[key]; exists && !slices.Equal(previous, projection.graph.maximal) {
			return compactKbuildSideOutputProjections{}, fmt.Errorf("grouped Kbuild side-output demand for %q has conflicting maximal state frontiers", demand.output.Path)
		}
		result.maximalByDemand[key] = slices.Clone(projection.graph.maximal)
	}
	return result, nil
}
