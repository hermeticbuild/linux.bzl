package kconfig

import (
	"maps"
	"slices"
)

// planCheckpointSelectionGraph is source/owner provenance after complete
// lowering. It deliberately excludes Make statements, evaluators, rules and
// lowering caches. Physical roots are rebound by the enclosing carrier before
// restoration; these helpers are not standalone input decoders.
type planCheckpointProfile struct {
	Name, Path, Directory    string
	Location                 CompactKbuildInvocationLocation
	LocationSet, TemplateSet bool
	Roots                    map[string]string
	Predecessors             []string
}

type planCheckpointSelectionEntry[V any] struct {
	Key   [3]string
	Value V
}

func planCheckpointSelectionRecords[V any](values map[compactKbuildSelectionKey]V) []planCheckpointSelectionEntry[V] {
	keys := slices.SortedFunc(maps.Keys(values), func(a, b compactKbuildSelectionKey) int {
		if compactKbuildSelectionKeyLess(a, b) {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	})
	var out []planCheckpointSelectionEntry[V]
	for _, key := range keys {
		out = append(out, planCheckpointSelectionEntry[V]{[3]string{key.profile, key.target, key.stage}, values[key]})
	}
	return out
}

func planCheckpointSelectionMap[V any](records []planCheckpointSelectionEntry[V]) map[compactKbuildSelectionKey]V {
	out := map[compactKbuildSelectionKey]V{}
	for _, record := range records {
		out[compactKbuildSelectionKey{record.Key[0], record.Key[1], record.Key[2]}] = record.Value
	}
	return out
}

type planCheckpointSelectionGraph struct {
	Profiles           map[string]planCheckpointProfile
	Selections         []planCheckpointSelectionEntry[CompactKbuildSelection]
	Initial, Generated []planCheckpointSelectionEntry[[]CompactKbuildVisibleArtifact]
	Producers          []planCheckpointSelectionEntry[string]
	Forwarding         []planCheckpointSelectionEntry[bool]
	OutputOwners       map[string][][3]string
	ProfileTargets     map[string]map[string][3]string
	SelectionsByTarget map[string][][3]string
	TargetInvocations  map[string]map[string][]string
}

func capturePlanCheckpointGraph(original *compactKbuildSelectionGraph) planCheckpointSelectionGraph {
	record := planCheckpointSelectionGraph{
		Profiles:           map[string]planCheckpointProfile{},
		Selections:         planCheckpointSelectionRecords(original.selections),
		Initial:            planCheckpointSelectionRecords(original.selectionInitialArtifacts),
		Generated:          planCheckpointSelectionRecords(original.selectionGeneratedArtifacts),
		Producers:          planCheckpointSelectionRecords(original.materializedProducers),
		Forwarding:         planCheckpointSelectionRecords(original.forwardingSelections),
		OutputOwners:       map[string][][3]string{},
		ProfileTargets:     map[string]map[string][3]string{},
		SelectionsByTarget: map[string][][3]string{},
		TargetInvocations:  map[string]map[string][]string{},
	}
	for key, profile := range original.profiles {
		entry := planCheckpointProfile{Name: profile.Name, Path: profile.Path, Directory: profile.Directory,
			Location: profile.invocationLocation, LocationSet: profile.invocationLocationSet,
			Predecessors: profile.InvocationPredecessors}
		if profile.evaluator != nil && profile.evaluator.template != nil {
			entry.TemplateSet = true
			entry.Roots = profile.evaluator.template.sourceRoots
		}
		record.Profiles[key] = entry
	}
	for pathname, owners := range original.outputOwnersByPath {
		for _, owner := range owners {
			record.OutputOwners[pathname] = append(record.OutputOwners[pathname], [3]string{owner.profile, owner.target, owner.stage})
		}
	}
	for key, selection := range original.selectionsByProfileTarget {
		if record.ProfileTargets[key.profile] == nil {
			record.ProfileTargets[key.profile] = map[string][3]string{}
		}
		record.ProfileTargets[key.profile][key.target] = [3]string{selection.profile, selection.target, selection.stage}
	}
	for target, selections := range original.selectionsByTarget {
		for _, key := range selections {
			record.SelectionsByTarget[target] = append(record.SelectionsByTarget[target], [3]string{key.profile, key.target, key.stage})
		}
	}
	for key, children := range original.targetInvocations {
		if record.TargetInvocations[key.profile] == nil {
			record.TargetInvocations[key.profile] = map[string][]string{}
		}
		record.TargetInvocations[key.profile][key.target] = children
	}
	return record
}

func restorePlanCheckpointGraph(decoded planCheckpointSelectionGraph) *compactKbuildSelectionGraph {
	graph := &compactKbuildSelectionGraph{
		profiles:                    map[string]CompactKbuildProfile{},
		selections:                  planCheckpointSelectionMap(decoded.Selections),
		selectionInitialArtifacts:   planCheckpointSelectionMap(decoded.Initial),
		selectionGeneratedArtifacts: planCheckpointSelectionMap(decoded.Generated),
		materializedProducers:       planCheckpointSelectionMap(decoded.Producers),
		forwardingSelections:        planCheckpointSelectionMap(decoded.Forwarding),
		outputOwnersByPath:          map[string][]compactKbuildSelectionKey{},
		selectionsByProfileTarget:   map[compactKbuildProfileTargetKey]compactKbuildSelectionKey{},
		selectionsByTarget:          map[string][]compactKbuildSelectionKey{},
		targetInvocations:           map[compactKbuildProfileTargetKey][]string{},
	}
	for key, entry := range decoded.Profiles {
		profile := CompactKbuildProfile{Name: entry.Name, Path: entry.Path, Directory: entry.Directory,
			invocationLocation: entry.Location, invocationLocationSet: entry.LocationSet,
			InvocationPredecessors: entry.Predecessors}
		if entry.TemplateSet {
			// Source lookup currently reaches roots through this shell, but no
			// Make statements, variable state, rule cache or callback survives.
			profile.evaluator = newKbuildTargetEvaluator(&kbuildParser{sourceRoots: entry.Roots})
		}
		graph.profiles[key] = profile
	}
	for pathname, owners := range decoded.OutputOwners {
		for _, owner := range owners {
			graph.outputOwnersByPath[pathname] = append(graph.outputOwnersByPath[pathname], compactKbuildSelectionKey{owner[0], owner[1], owner[2]})
		}
	}
	for profile, targets := range decoded.ProfileTargets {
		for target, owner := range targets {
			graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{profile, target}] = compactKbuildSelectionKey{owner[0], owner[1], owner[2]}
		}
	}
	for target, selections := range decoded.SelectionsByTarget {
		for _, key := range selections {
			graph.selectionsByTarget[target] = append(graph.selectionsByTarget[target], compactKbuildSelectionKey{key[0], key[1], key[2]})
		}
	}
	for profile, targets := range decoded.TargetInvocations {
		for target, children := range targets {
			graph.targetInvocations[compactKbuildProfileTargetKey{profile, target}] = children
		}
	}
	return graph
}
