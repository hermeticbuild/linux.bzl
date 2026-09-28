package main

import (
	"fmt"
	"sort"
	"strings"
)

// frontierReplayResult is one materialized object-tree view together with the
// paths causally changed since the invocation's initial view.
type frontierReplayResult struct {
	state   kbuildFrontierState
	touched kbuildPathSet
}

func sameFrontierOrigin(left, right *kbuildRecursiveMakeFrontier) bool {
	return left == right || (left != nil && right != nil && left.id == right.id)
}

func sameFrontierValue(left, right kbuildFrontierValue) bool {
	return left.artifact == right.artifact && left.exact == right.exact &&
		(!left.exact || left.content == right.content)
}

func frontierValueLabel(value kbuildFrontierValue, present bool) string {
	if !present {
		return "initial absence"
	}
	return strings.Join([]string{
		value.artifact.Profile, value.artifact.Target, value.artifact.Path,
	}, ":")
}

// mergeKbuildFrontierReplayResults joins prerequisite views without comparing
// every parent against every touched path. A parent which did not touch a path
// necessarily still exposes the invocation's initial value there, so all such
// parents collapse to one implicit initial candidate. Actual writers remain in
// parent order and retain their precise causal origins.
func mergeKbuildFrontierReplayResults(
	initialState kbuildFrontierState,
	parents []frontierReplayResult,
	ancestryCache map[string]bool,
) (frontierReplayResult, error) {
	merged := frontierReplayResult{
		state: initialState, touched: emptyKbuildPathSet(),
	}
	if len(parents) == 0 {
		return merged, nil
	}

	type candidate struct {
		value   kbuildFrontierValue
		present bool
	}
	type pathCandidates struct {
		values         []candidate
		touchedParents int
	}

	byPath := map[string]*pathCandidates{}
	paths := []string{}
	for _, parent := range parents {
		kbuildPathSetRange(parent.touched, func(path string) bool {
			candidates := byPath[path]
			if candidates == nil {
				candidates = &pathCandidates{}
				byPath[path] = candidates
				paths = append(paths, path)
			}
			candidates.touchedParents++
			value, present := kbuildFrontierGet(parent.state, path)
			candidates.values = append(candidates.values, candidate{
				value: value, present: present,
			})
			return true
		})
	}
	// The former path-set union visited paths in sorted order. Retain that
	// deterministic error and insertion order even though collection is now
	// sparse and parent-local.
	sort.Strings(paths)

	for _, path := range paths {
		pathGroup := byPath[path]
		candidates := make([]candidate, 0, len(pathGroup.values)+1)
		addCandidate := func(next candidate) error {
			for _, previous := range candidates {
				if !sameFrontierOrigin(previous.value.origin, next.value.origin) {
					continue
				}
				if previous.present != next.present ||
					(next.present && !sameFrontierValue(previous.value, next.value)) {
					return fmt.Errorf(
						"causal Kbuild frontier %q has conflicting values with the same origin",
						path,
					)
				}
				return nil
			}
			candidates = append(candidates, next)
			return nil
		}
		for _, next := range pathGroup.values {
			if err := addCandidate(next); err != nil {
				return frontierReplayResult{}, err
			}
		}

		initial, initiallyPresent := kbuildFrontierGet(initialState, path)
		if pathGroup.touchedParents < len(parents) {
			// Untouched parents expose this value but do not own its causal
			// history within the current invocation.
			initial.origin = nil
			if err := addCandidate(candidate{value: initial, present: initiallyPresent}); err != nil {
				return frontierReplayResult{}, err
			}
		}

		maximal := make([]candidate, 0, len(candidates))
		for candidateIndex, candidate := range candidates {
			shadowed := false
			for laterIndex, later := range candidates {
				if candidateIndex == laterIndex ||
					sameFrontierOrigin(candidate.value.origin, later.value.origin) {
					continue
				}
				if kbuildRecursiveMakeFrontierDescendsFrom(
					later.value.origin, candidate.value.origin, ancestryCache,
				) {
					shadowed = true
					break
				}
			}
			if !shadowed {
				maximal = append(maximal, candidate)
			}
		}
		if len(maximal) != 1 {
			labels := make([]string, 0, len(maximal))
			for _, candidate := range maximal {
				labels = append(labels, frontierValueLabel(candidate.value, candidate.present))
			}
			sort.Strings(labels)
			return frontierReplayResult{}, fmt.Errorf(
				"causal Kbuild prerequisite frontier has incomparable versions of %q from %q",
				path, labels,
			)
		}

		winner := maximal[0]
		if winner.present {
			merged.state = kbuildFrontierSet(merged.state, path, winner.value)
		}
		causallyChanged := initiallyPresent != winner.present
		if initiallyPresent && winner.present {
			causallyChanged = !sameFrontierValue(initial, winner.value) || winner.value.origin != nil
		}
		if causallyChanged {
			merged.touched = kbuildPathSetAdd(merged.touched, path)
		}
	}
	return merged, nil
}
