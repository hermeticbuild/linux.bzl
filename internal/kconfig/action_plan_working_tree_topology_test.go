package kconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func workingTreeTopologyNodeForTest(id string, inputs ...string) ActionPlanNode {
	edges := make([]ActionPlanNodeEdge, 0, len(inputs))
	for _, input := range inputs {
		edges = append(edges, ActionPlanNodeEdge{Role: "input", ProducerID: input})
	}
	return ActionPlanNode{
		ID:      id,
		Stage:   "target",
		Kind:    "generate",
		Tool:    "cc",
		Product: "vmlinux",
		Inputs:  edges,
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: id + ".o"}},
	}
}

func walkWorkingTreeTopologyForTest(plan *ActionPlan, roots ...string) ([]string, error) {
	visited := make([]bool, len(plan.Nodes))
	visitedIDs := []string{}
	for _, root := range roots {
		err := plan.walkCompactKbuildWorkingTreeTopology(root, visited, func(index uint32) error {
			visitedIDs = append(visitedIDs, plan.Nodes[index].ID)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return visitedIDs, nil
}

func appendWorkingTreeTopologyNodeForTest(t *testing.T, plan *ActionPlan, node ActionPlanNode) {
	t.Helper()
	_, err := appendActionPlanNode(plan, node, ActionRecipe{
		Schema:    LinuxKernelPlanSchema,
		Kind:      "generate",
		Tool:      "cc",
		Arguments: []string{"-o", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestActionPlanWorkingTreeTopologyCacheHitsAndSurvivesAppendGrowth(t *testing.T) {
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{
			workingTreeTopologyNodeForTest("leaf"),
			workingTreeTopologyNodeForTest("middle", "leaf"),
			workingTreeTopologyNodeForTest("root", "middle"),
		},
	}
	want := []string{"leaf", "middle", "root"}
	got, err := walkWorkingTreeTopologyForTest(plan, "root")
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("first traversal = %q, %v, want %q", got, err, want)
	}
	cached := plan.workingTreeTopologyCache["root"]
	if !slices.Equal(cached, []uint32{0, 1, 2}) || plan.workingTreeTopologyCacheIndexes != len(cached) {
		t.Fatalf("cached root topology = %v (%d indexes), want [0 1 2]", cached, plan.workingTreeTopologyCacheIndexes)
	}
	cachedStart := &cached[0]

	got, err = walkWorkingTreeTopologyForTest(plan, "root")
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("cached traversal = %q, %v, want %q", got, err, want)
	}
	if repeated := plan.workingTreeTopologyCache["root"]; &repeated[0] != cachedStart || plan.workingTreeTopologyCacheIndexes != 3 {
		t.Fatalf("cache hit replaced or recounted root topology: %v (%d indexes)", repeated, plan.workingTreeTopologyCacheIndexes)
	}

	appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("next", "root"))
	if repeated := plan.workingTreeTopologyCache["root"]; &repeated[0] != cachedStart {
		t.Fatalf("append-only growth discarded cached root topology: %v", repeated)
	}
	got, err = walkWorkingTreeTopologyForTest(plan, "next")
	want = []string{"leaf", "middle", "root", "next"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("grown traversal = %q, %v, want %q", got, err, want)
	}
	if cachedNext := plan.workingTreeTopologyCache["next"]; !slices.Equal(cachedNext, []uint32{0, 1, 2, 3}) {
		t.Fatalf("cached grown topology = %v, want [0 1 2 3]", cachedNext)
	}
	if got, want := plan.workingTreeTopologyCacheIndexes, 7; got != want {
		t.Fatalf("cached topology indexes = %d, want %d", got, want)
	}
}

func TestActionPlanWorkingTreeTopologyCacheRecoversMissingProducer(t *testing.T) {
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes:   []ActionPlanNode{workingTreeTopologyNodeForTest("root", "late")},
	}
	_, err := walkWorkingTreeTopologyForTest(plan, "root")
	if err == nil || err.Error() != `working object-tree ancestor "late" is absent from the action plan` {
		t.Fatalf("missing traversal error = %v", err)
	}
	if _, cached := plan.workingTreeTopologyCache["root"]; cached || plan.workingTreeTopologyCacheIndexes != 0 {
		t.Fatalf("failed traversal was cached: %#v (%d indexes)", plan.workingTreeTopologyCache, plan.workingTreeTopologyCacheIndexes)
	}

	appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("late"))
	got, err := walkWorkingTreeTopologyForTest(plan, "root")
	want := []string{"late", "root"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("recovered traversal = %q, %v, want %q", got, err, want)
	}
	if cached := plan.workingTreeTopologyCache["root"]; !slices.Equal(cached, []uint32{1, 0}) {
		t.Fatalf("recovered cached topology = %v, want [1 0]", cached)
	}
}

func TestActionPlanWorkingTreeTopologyCachePreservesErrorOrder(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		workingTreeTopologyNodeForTest("policy"),
		workingTreeTopologyNodeForTest("root", "policy", "missing"),
	}}
	if _, err := walkWorkingTreeTopologyForTest(plan, "policy"); err != nil {
		t.Fatal(err)
	}
	policyErr := errors.New("earlier policy error")
	visited := make([]bool, len(plan.Nodes))
	err := plan.walkCompactKbuildWorkingTreeTopology("root", visited, func(index uint32) error {
		if plan.Nodes[index].ID == "policy" {
			return policyErr
		}
		return nil
	})
	if !errors.Is(err, policyErr) {
		t.Fatalf("cached earlier branch error = %v, want %v before missing-producer error", err, policyErr)
	}
	if _, cached := plan.workingTreeTopologyCache["root"]; cached {
		t.Fatalf("erroring root traversal was cached: %#v", plan.workingTreeTopologyCache["root"])
	}
}

func TestActionPlanWorkingTreeTopologyCacheClearsOnRebuildAndInvalidation(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		workingTreeTopologyNodeForTest("first"),
		workingTreeTopologyNodeForTest("root", "first"),
	}}
	if _, err := walkWorkingTreeTopologyForTest(plan, "root"); err != nil {
		t.Fatal(err)
	}
	plan.Nodes = append(plan.Nodes, workingTreeTopologyNodeForTest("direct"))
	plan.ensureNodeLookupIndexes()
	if plan.workingTreeTopologyCache != nil || plan.workingTreeTopologyCacheIndexes != 0 {
		t.Fatalf("full lookup rebuild retained topology cache: %#v (%d indexes)", plan.workingTreeTopologyCache, plan.workingTreeTopologyCacheIndexes)
	}
	if got := plan.nodeIndexesByID["direct"]; got != 2 {
		t.Fatalf("direct node index = %d, want 2", got)
	}

	if _, err := walkWorkingTreeTopologyForTest(plan, "root"); err != nil {
		t.Fatal(err)
	}
	plan.Nodes[1].Inputs = []ActionPlanNodeEdge{{Role: "input", ProducerID: "direct"}}
	plan.invalidateLookupIndexes()
	if plan.workingTreeTopologyCache != nil || plan.workingTreeTopologyCacheIndexes != 0 {
		t.Fatalf("lookup invalidation retained topology cache: %#v (%d indexes)", plan.workingTreeTopologyCache, plan.workingTreeTopologyCacheIndexes)
	}
	got, err := walkWorkingTreeTopologyForTest(plan, "root")
	want := []string{"direct", "root"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("invalidated traversal = %q, %v, want %q", got, err, want)
	}
}

func TestActionPlanWorkingTreeTopologyCacheIsBounded(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		workingTreeTopologyNodeForTest("leaf"),
		workingTreeTopologyNodeForTest("root", "leaf"),
	}}
	plan.ensureNodeLookupIndexes()
	plan.workingTreeTopologyCache = map[string][]uint32{}
	plan.workingTreeTopologyCacheIndexes = maximumWorkingTreeTopologyCacheIndexes - 1
	got, err := walkWorkingTreeTopologyForTest(plan, "root")
	if err != nil || !slices.Equal(got, []string{"leaf", "root"}) {
		t.Fatalf("bounded traversal = %q, %v", got, err)
	}
	if _, cached := plan.workingTreeTopologyCache["root"]; cached {
		t.Fatal("topology exceeding the remaining cache budget was stored")
	}
	if got, want := plan.workingTreeTopologyCacheIndexes, maximumWorkingTreeTopologyCacheIndexes-1; got != want {
		t.Fatalf("bounded cache count = %d, want %d", got, want)
	}
}

func TestKbuildWorkingTreeTopologyCacheRereadsLiveNodePolicy(t *testing.T) {
	const (
		producer = "archive"
		sourceID = "src-00000001"
		recipeID = "archive-recipe"
	)
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{ID: sourceID, Namespace: "kernel", Path: "vendor/member.o"}},
		Recipes: map[string]ActionRecipe{
			recipeID: {
				Sources:       []string{"object:00000000"},
				WorkingInputs: map[string]string{"source:object:00000000": "vendor/member.o"},
			},
		},
		Nodes: []ActionPlanNode{{
			ID: producer, Recipe: recipeID,
			Sources: []ActionPlanSourceEdge{{Role: "object", SourceID: sourceID}},
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "built-in.a"}},
		}},
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	profile := CompactKbuildProfile{Name: "topology-cache"}
	root := []compactKbuildRuleInput{{producer: producer}}
	closure := func(direct []compactKbuildRuleInput) map[string]compactKbuildRuleInput {
		t.Helper()
		inputs, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, direct, root)
		if err != nil {
			t.Fatal(err)
		}
		byPath := make(map[string]compactKbuildRuleInput, len(inputs))
		for _, input := range inputs {
			byPath[input.path] = input
		}
		return byPath
	}

	first := closure(nil)
	if got := first["built-in.a"]; got.producer != producer || !got.workingOnly {
		t.Fatalf("first archive output = %#v, want working-only producer %q", got, producer)
	}
	if _, found := first["vendor/member.o"]; found {
		t.Fatalf("unmarked archive unexpectedly retained source member: %#v", first)
	}
	cached := plan.workingTreeTopologyCache[producer]
	if len(cached) != 1 {
		t.Fatalf("archive topology cache = %v, want one node", cached)
	}
	cachedStart := &cached[0]

	if err := plan.markPathSensitiveArchiveOutput(producer, 0); err != nil {
		t.Fatal(err)
	}
	second := closure(nil)
	if got := second["vendor/member.o"]; got.sourceID != sourceID || !got.workingOnly {
		t.Fatalf("late path-sensitive source member = %#v, want source %q", got, sourceID)
	}
	if repeated := plan.workingTreeTopologyCache[producer]; &repeated[0] != cachedStart {
		t.Fatal("late path-sensitive marking discarded the topology cache")
	}

	recipe := plan.Recipes[recipeID]
	recipe.WorkingInputs["source:object:00000000"] = "vendor/rebound.o"
	plan.Recipes[recipeID] = recipe
	plan.Nodes[0].Outputs[0].Path = "rebuilt.a"
	third := closure(nil)
	if _, found := third["built-in.a"]; found {
		t.Fatalf("cached topology retained stale output policy: %#v", third)
	}
	if got := third["rebuilt.a"]; got.producer != producer || !got.workingOnly {
		t.Fatalf("live archive output = %#v, want working-only producer %q", got, producer)
	}
	if got := third["vendor/rebound.o"]; got.sourceID != sourceID || !got.workingOnly {
		t.Fatalf("live recipe source member = %#v, want source %q", got, sourceID)
	}
	for pathname := range third {
		if strings.Contains(pathname, "member.o") {
			t.Fatalf("cached topology retained stale recipe path %q: %#v", pathname, third)
		}
	}

	native := closure([]compactKbuildRuleInput{{path: "rebuilt.a", producer: producer}})
	if got := native["rebuilt.a"]; got.producer != producer || got.workingOnly {
		t.Fatalf("per-call native root policy = %#v, want native producer %q", got, producer)
	}
}
