package kconfig

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func sideOutputProjectionFixture(t testing.TB, source string, targets ...string) (*compactKbuildSelectionGraph, *CompactMetadata, []compactKbuildSelectionKey) {
	t.Helper()
	kbuild, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", KbuildOptions{
		ConfigVariablesComplete: true, MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:projection", "scripts/Makefile.build", "", kbuild)
	if err != nil {
		t.Fatal(err)
	}
	profile.Directory = ""
	config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
	var candidates []compactKbuildSelectionKey
	for _, target := range targets {
		config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
			Profile: profile.Name, Target: target, MakeTarget: target,
			Lifecycle: "target", Scope: "target", Stage: "target",
		})
		candidates = append(candidates, compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"})
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	return graph, &CompactMetadata{Config: config}, candidates
}

const sideOutputProjectionFork = `
cmd_emit = touch $@
a: FORCE
	$(call if_changed,emit)
b: a FORCE
	$(call if_changed,emit)
c: a FORCE
	$(call if_changed,emit)
d: a b c FORCE
	$(call if_changed,emit)
`

func TestSideOutputProjectionsInternExactSetsAndOwnParentSlices(t *testing.T) {
	graph, metadata, candidates := sideOutputProjectionFixture(t, sideOutputProjectionFork, "a", "b", "c", "d")
	all := map[compactKbuildSelectionKey]bool{}
	for _, candidate := range candidates {
		all[candidate] = true
	}
	inputs := map[string]map[compactKbuildSelectionKey]bool{
		"one.h": all, "two.h": maps.Clone(all),
		"sparse.h": {candidates[0]: true, candidates[3]: true},
	}
	prepared, err := graph.sideOutputStateProjections(metadata, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	projections := prepared.byPath
	if projections["one.h"] != projections["two.h"] || projections["one.h"] == projections["sparse.h"] {
		t.Fatal("projections do not intern exactly equal candidate sets")
	}
	for path, set := range inputs {
		want, err := graph.compactKbuildSideOutputStateGraph(metadata, slices.Collect(maps.Keys(set)))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(projections[path].graph, want) {
			t.Fatalf("%s differs from independent projection", path)
		}
	}
	parents, ok := projections["one.h"].parentsFor(candidates[3])
	if !ok || !slices.Equal(parents, candidates[1:3]) {
		t.Fatalf("fork parents = %v, want %v", parents, candidates[1:3])
	}
	parents[0] = candidates[3]
	clear(all)
	again, ok := projections["two.h"].parentsFor(candidates[3])
	if !ok || !slices.Equal(again, candidates[1:3]) {
		t.Fatal("caller mutation changed shared projection")
	}
	if _, ok := projections["one.h"].parentsFor(compactKbuildSelectionKey{target: "missing"}); ok {
		t.Fatal("unknown candidate was accepted")
	}
}

func TestSideOutputProjectionCacheDoesNotEscapePlanningPhase(t *testing.T) {
	graph, metadata, candidates := sideOutputProjectionFixture(t, sideOutputProjectionFork, "a", "b", "c", "d")
	set := map[compactKbuildSelectionKey]bool{}
	for _, candidate := range candidates {
		set[candidate] = true
	}
	paths := map[string]map[compactKbuildSelectionKey]bool{"observed.h": set}
	first, err := graph.sideOutputStateProjections(metadata, paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graph.sideOutputStateProjections(metadata, paths, nil)
	if err != nil || first.byPath["observed.h"] == second.byPath["observed.h"] {
		t.Fatalf("projection retained across phases: %v", err)
	}
	if _, err := graph.sideOutputStateProjections(nil, paths, nil); err == nil {
		t.Fatal("missing metadata reused a projection")
	}
	invalid := map[string]map[compactKbuildSelectionKey]bool{"invalid.h": {{target: "missing"}: true}}
	if _, err := graph.sideOutputStateProjections(metadata, invalid, nil); err == nil {
		t.Fatal("unknown candidate accepted")
	}
	graph.sideOutputCandidateDependencies = map[compactKbuildSelectionKey][]compactKbuildSelectionKey{candidates[0]: {candidates[3]}}
	graph.invalidateResolvedDependencies()
	if _, err := graph.sideOutputStateProjections(metadata, paths, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("changed cyclic graph reused previous projection: %v", err)
	}
}

func TestSideOutputProjectionCanonicalizesGroupedCandidates(t *testing.T) {
	graph, _, candidates := sideOutputProjectionFixture(t, sideOutputProjectionFork, "a", "b", "c", "d")
	group := compactKbuildGroupedSelectionID{profile: candidates[1].profile, ruleOrder: 7}
	graph.groupedSelectionIDs = map[compactKbuildSelectionKey]compactKbuildGroupedSelectionID{candidates[2]: group}
	graph.groupedSelectionRepresentatives = map[compactKbuildGroupedSelectionID]compactKbuildSelectionKey{group: candidates[1]}
	got, err := graph.canonicalSideOutputCandidates([]compactKbuildSelectionKey{candidates[2], candidates[0], candidates[1], candidates[2]})
	if err != nil || !slices.Equal(got, candidates[:2]) {
		t.Fatalf("grouped candidates = %v, %v; want %v", got, err, candidates[:2])
	}
}

func TestSideOutputDemandProjectionsKeepExactSubsetsAndOwnedFrontiers(t *testing.T) {
	graph, metadata, candidates := sideOutputProjectionFixture(t, sideOutputProjectionFork, "a", "b", "c", "d")
	paths := map[string]map[compactKbuildSelectionKey]bool{
		"observed.h": {candidates[0]: true, candidates[1]: true, candidates[2]: true, candidates[3]: true},
	}
	demands := []compactKbuildSideOutputDemand{
		{consumer: candidates[3], output: ActionPlanOutput{Tree: "target", Path: "observed.h"}, candidates: slices.Clone(candidates[:3])},
		{consumer: candidates[2], output: ActionPlanOutput{Tree: "target", Path: "observed.h"}, candidates: slices.Clone(candidates[:2])},
		{consumer: candidates[3], output: ActionPlanOutput{Tree: "prep", Path: "observed.h"}, candidates: slices.Clone(candidates)},
		{consumer: candidates[3], output: ActionPlanOutput{Tree: "target", Path: "other.h"}, candidates: slices.Clone(candidates[:3])},
	}
	prepared, err := graph.sideOutputStateProjections(metadata, paths, demands)
	if err != nil {
		t.Fatal(err)
	}
	for _, demand := range demands {
		want, err := graph.compactKbuildSideOutputStateGraph(metadata, demand.candidates)
		if err != nil {
			t.Fatal(err)
		}
		key := compactKbuildSideOutputDemandStateKey{consumer: demand.consumer, tree: demand.output.Tree, path: demand.output.Path}
		if !slices.Equal(prepared.maximalByDemand[key], want.maximal) {
			t.Fatalf("%+v frontier = %v, want %v", key, prepared.maximalByDemand[key], want.maximal)
		}
	}
	firstKey := compactKbuildSideOutputDemandStateKey{consumer: candidates[3], tree: "target", path: "observed.h"}
	otherKey := compactKbuildSideOutputDemandStateKey{consumer: candidates[3], tree: "target", path: "other.h"}
	prepared.maximalByDemand[firstKey][0] = candidates[0]
	demands[0].candidates[0] = candidates[3]
	clear(paths["observed.h"])
	if !slices.Equal(prepared.maximalByDemand[otherKey], candidates[1:3]) {
		t.Fatal("a returned or input slice mutated another demand's frontier")
	}
	if !slices.Equal(prepared.byPath["observed.h"].graph.maximal, candidates[3:]) {
		t.Fatal("demand projection mutated path-wide projection")
	}
}

func TestSideOutputDemandProjectionsRejectConflictsAndInvalidCandidates(t *testing.T) {
	graph, metadata, candidates := sideOutputProjectionFixture(t, sideOutputProjectionFork, "a", "b", "c", "d")
	group := compactKbuildGroupedSelectionID{profile: candidates[2].profile, ruleOrder: 9}
	graph.groupedSelectionIDs = map[compactKbuildSelectionKey]compactKbuildGroupedSelectionID{candidates[3]: group}
	graph.groupedSelectionRepresentatives = map[compactKbuildGroupedSelectionID]compactKbuildSelectionKey{group: candidates[2]}
	first := compactKbuildSideOutputDemand{
		consumer: candidates[2], output: ActionPlanOutput{Tree: "target", Path: "observed.h"}, candidates: candidates[:1],
	}
	alias := first
	alias.consumer = candidates[3]
	if prepared, err := graph.sideOutputStateProjections(metadata, nil, []compactKbuildSideOutputDemand{first, alias}); err != nil || len(prepared.maximalByDemand) != 1 {
		t.Fatalf("equivalent grouped demands were not coalesced: %v", err)
	}
	alias.candidates = candidates[:2]
	if _, err := graph.sideOutputStateProjections(metadata, nil, []compactKbuildSideOutputDemand{first, alias}); err == nil || !strings.Contains(err.Error(), "conflicting maximal state frontiers") {
		t.Fatalf("conflicting grouped demands were accepted: %v", err)
	}
	first.candidates = []compactKbuildSelectionKey{{target: "missing"}}
	if _, err := graph.sideOutputStateProjections(metadata, nil, []compactKbuildSideOutputDemand{first}); err == nil || !strings.Contains(err.Error(), "not selected") {
		t.Fatalf("invalid demand-only candidate was accepted: %v", err)
	}
}

func TestSideOutputProjectionKeysPreserveExactFields(t *testing.T) {
	cases := [][]compactKbuildSelectionKey{
		nil, {{}},
		{{profile: "p, q", target: "r", stage: "target"}},
		{{profile: "p", target: "q, r", stage: "target"}},
		{{profile: "a\x00b", target: "c"}},
		{{profile: "a", target: "b\x00c"}},
		{{profile: "\xff"}}, {{profile: "\xfe"}}, {{profile: "\ufffd"}},
		{{profile: "1:a", target: ""}}, {{profile: "", target: "1:a"}},
		{{profile: "a"}, {profile: "b"}}, {{profile: "ab"}},
	}
	seen := map[string]bool{}
	for _, candidates := range cases {
		key := compactKbuildSideOutputProjectionKey(candidates)
		if seen[key] {
			t.Fatalf("projection key collision for %#v", candidates)
		}
		seen[key] = true
	}
}

func BenchmarkSideOutputProjectionSharing(b *testing.B) {
	const count = 128
	var source strings.Builder
	source.WriteString("cmd_emit = touch $@\n")
	targets := make([]string, count)
	for index := range count {
		targets[index] = fmt.Sprintf("node%03d", count-index)
		fmt.Fprintf(&source, "%s: FORCE", targets[index])
		for _, parent := range targets[:index] {
			fmt.Fprintf(&source, " %s", parent)
		}
		source.WriteString("\n\t$(call if_changed,emit)\n")
	}
	graph, metadata, candidates := sideOutputProjectionFixture(b, source.String(), targets...)
	paths := map[string]map[compactKbuildSelectionKey]bool{}
	for path := range 28 {
		set := map[compactKbuildSelectionKey]bool{}
		for _, candidate := range candidates {
			set[candidate] = true
		}
		paths[fmt.Sprintf("generated%d.h", path)] = set
	}
	if _, err := graph.compactKbuildSideOutputStateGraph(metadata, candidates); err != nil {
		b.Fatal(err)
	}
	var demands []compactKbuildSideOutputDemand
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		demands = append(demands, compactKbuildSideOutputDemand{
			consumer:   candidates[len(candidates)-1],
			output:     ActionPlanOutput{Tree: "target", Path: path},
			candidates: slices.Clone(candidates),
		})
	}
	for _, demandCount := range []int{0, len(demands)} {
		for _, shared := range []bool{false, true} {
			b.Run(fmt.Sprintf("demands=%d/shared=%v", demandCount, shared), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if shared {
						if _, err := graph.sideOutputStateProjections(metadata, paths, demands[:demandCount]); err != nil {
							b.Fatal(err)
						}
					} else {
						projections := make(map[string]compactKbuildSideOutputStateGraph, len(paths))
						for path := range paths {
							projection, err := graph.compactKbuildSideOutputStateGraph(metadata, candidates)
							if err != nil {
								b.Fatal(err)
							}
							projections[path] = projection
						}
						if len(projections) != len(paths) {
							b.Fatal("missing independent projection")
						}
						frontiers := make(map[compactKbuildSideOutputDemandStateKey][]compactKbuildSelectionKey, demandCount)
						for _, demand := range demands[:demandCount] {
							projection, err := graph.compactKbuildSideOutputStateGraph(metadata, demand.candidates)
							if err != nil {
								b.Fatal(err)
							}
							key := compactKbuildSideOutputDemandStateKey{consumer: demand.consumer, tree: demand.output.Tree, path: demand.output.Path}
							frontiers[key] = slices.Clone(projection.maximal)
						}
						if len(frontiers) != demandCount {
							b.Fatal("missing independent demand frontier")
						}
					}
				}
			})
		}
	}
}
