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
	visited := actionPlanNodeVisitSet{}
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

func TestActionPlanWorkingTreeTopologyVisitationScalesWithReachedNodes(t *testing.T) {
	const unrelatedNodeCount = 4096
	nodes := make([]ActionPlanNode, 0, unrelatedNodeCount+2)
	for index := 0; index < unrelatedNodeCount; index++ {
		nodes = append(nodes, workingTreeTopologyNodeForTest("unrelated-"+planOrdinal(index)))
	}
	nodes = append(nodes,
		workingTreeTopologyNodeForTest("leaf"),
		workingTreeTopologyNodeForTest("root", "leaf"),
	)
	plan := &ActionPlan{Nodes: nodes}
	visited := actionPlanNodeVisitSet{}
	visitedIDs := []string{}
	if err := plan.walkCompactKbuildWorkingTreeTopology("root", visited, func(index uint32) error {
		visitedIDs = append(visitedIDs, plan.Nodes[index].ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"leaf", "root"}; !slices.Equal(visitedIDs, want) {
		t.Fatalf("visited nodes = %q, want %q", visitedIDs, want)
	}
	// This is intentionally a storage-cardinality assertion rather than a wall
	// clock benchmark. It fails if traversal returns to allocating/clearing one
	// visitation entry per plan node for each sparse compiler closure.
	if got, want := len(visited), len(visitedIDs); got != want {
		t.Fatalf("visitation storage contains %d entries for %d reached nodes in a %d-node plan", got, want, len(plan.Nodes))
	}
	for _, index := range []uint32{unrelatedNodeCount, unrelatedNodeCount + 1} {
		if !visited.contains(index) {
			t.Fatalf("reached node index %d is absent from visitation storage", index)
		}
	}
}

func TestKbuildWorkingTreeMaterializedCoreReusesEffectiveQuery(t *testing.T) {
	plan := &ActionPlan{
		Nodes: []ActionPlanNode{
			workingTreeTopologyNodeForTest("leaf"),
			workingTreeTopologyNodeForTest("root", "leaf"),
		},
	}
	profile := CompactKbuildProfile{Name: "closure-result-cache"}
	firstBuilder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	first, err := firstBuilder.compactKbuildWorkingTreeClosureInputsFromRoots(
		"consumer", profile, nil,
		[]compactKbuildRuleInput{{producer: "root"}, {producer: "leaf"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("materialized core computations after first query = %d, want %d", got, want)
	}
	if got, want := plan.workingTreeMaterializedNodeProjections, 2; got != want {
		t.Fatalf("materialized node projections after first query = %d, want %d", got, want)
	}

	// A caller owns its returned slice. Mutating it must not corrupt the
	// persistent materialized core consumed by another builder.
	first[0].path = "caller-mutated"
	secondBuilder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	second, err := secondBuilder.compactKbuildWorkingTreeClosureInputsFromRoots(
		"consumer", profile, nil,
		[]compactKbuildRuleInput{{producer: "leaf"}, {producer: "root"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("identical effective root-set query recomputed materialized core %d times, want %d", got, want)
	}
	if got := []string{second[0].path, second[1].path}; !slices.Equal(got, []string{"leaf.o", "root.o"}) {
		t.Fatalf("reused materialized paths = %q, want [leaf.o root.o]", got)
	}
}

func TestKbuildWorkingTreePersistentFrontierReusesRootWithoutReinsertion(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		workingTreeTopologyNodeForTest("persistent-leaf"),
		workingTreeTopologyNodeForTest("persistent-root", "persistent-leaf"),
	}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	profile := CompactKbuildProfile{Name: "persistent-frontier"}
	roots := []compactKbuildRuleInput{{producer: "persistent-root"}}
	first, err := builder.compactKbuildWorkingTreeInputFrontierFromRoots("consumer", profile, nil, roots)
	if err != nil {
		t.Fatal(err)
	}
	if first.inputSet == "" || len(first.direct) != 0 {
		t.Fatalf("first persistent frontier = %#v, want a root and no direct inputs", first)
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	before := store.NodeCount()
	second, err := builder.compactKbuildWorkingTreeInputFrontierFromRoots("consumer", profile, nil, roots)
	if err != nil {
		t.Fatal(err)
	}
	if second.inputSet != first.inputSet || len(second.direct) != 0 {
		t.Fatalf("reused persistent frontier = %#v, want root %q and no direct inputs", second, first.inputSet)
	}
	if got := store.NodeCount(); got != before {
		t.Fatalf("reused frontier grew input-set store from %d to %d nodes", before, got)
	}
}

func TestKbuildWorkingTreeMaterializedCoreIsolatesPolicyAndPlanMutation(t *testing.T) {
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{
			workingTreeTopologyNodeForTest("leaf"),
			workingTreeTopologyNodeForTest("root", "leaf"),
		},
	}
	profile := CompactKbuildProfile{Name: "closure-result-cache-policy"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	query := func(current compactKbuildRulePlanBuilder, direct, roots []compactKbuildRuleInput) []compactKbuildRuleInput {
		t.Helper()
		inputs, err := current.compactKbuildWorkingTreeClosureInputsFromRoots(
			"consumer", profile, direct, roots,
		)
		if err != nil {
			t.Fatal(err)
		}
		return inputs
	}

	root := []compactKbuildRuleInput{{producer: "root"}}
	query(builder, nil, root)
	query(builder, nil, []compactKbuildRuleInput{{producer: "leaf"}})
	if got, want := plan.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("distinct root sets produced %d materialized cores, want %d", got, want)
	}
	if got, want := plan.workingTreeMaterializedNodeProjections, 2; got != want {
		t.Fatalf("overlapping root sets projected %d nodes, want each ancestor once (%d)", got, want)
	}

	hostBuilder := builder.forOutput("host", "host", "sdk")
	query(hostBuilder, nil, root)

	native := query(builder, []compactKbuildRuleInput{{path: "root.o", producer: "root"}}, root)
	if got, want := plan.workingTreeMaterializedCoreComputations, 3; got != want {
		t.Fatalf("native collision did not use one safe live replay: %d computations, want %d", got, want)
	}
	if got := native[len(native)-1]; got.path != "root.o" || got.workingOnly {
		t.Fatalf("native root result = %#v, want non-working-only root.o", got)
	}

	appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("unrelated"))
	if got, want := len(plan.workingTreeMaterializedCoreCache), 2; got != want {
		t.Fatalf("append-only growth retained %d materialized cores, want %d", got, want)
	}
	query(builder, nil, root)
	if got, want := plan.workingTreeMaterializedCoreComputations, 3; got != want {
		t.Fatalf("append-separated query recomputed materialized core %d times, want %d", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCoreScalesAcrossConsumersAndAppends(t *testing.T) {
	const ancestorCount = 256
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	previous := ""
	for index := 0; index < ancestorCount; index++ {
		id := "ancestor-" + planOrdinal(index)
		node := workingTreeTopologyNodeForTest(id)
		if previous != "" {
			node.Inputs = []ActionPlanNodeEdge{{Role: "input", ProducerID: previous}}
		}
		plan.Nodes = append(plan.Nodes, node)
		previous = id
	}
	profile := CompactKbuildProfile{Name: "materialized-core-scale"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	root := []compactKbuildRuleInput{{producer: previous}}
	query := func() {
		t.Helper()
		if _, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, nil, root); err != nil {
			t.Fatal(err)
		}
	}
	query()
	for index := 0; index < 32; index++ {
		appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("unrelated-"+planOrdinal(index)))
		query()
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("append-separated materialized core computations = %d, want %d", got, want)
	}
	if got, want := plan.workingTreeMaterializedNodeProjections, ancestorCount; got != want {
		t.Fatalf("append-separated ancestor projections = %d, want %d", got, want)
	}

	sibling := workingTreeTopologyNodeForTest("sibling", previous)
	appendWorkingTreeTopologyNodeForTest(t, plan, sibling)
	if _, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots(
		"consumer", profile, nil, []compactKbuildRuleInput{{producer: "sibling"}},
	); err != nil {
		t.Fatal(err)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("overlapping-root materialized core computations = %d, want %d", got, want)
	}
	if got, want := plan.workingTreeMaterializedNodeProjections, ancestorCount+1; got != want {
		t.Fatalf("overlapping-root ancestor projections = %d, want only sibling beyond %d cached ancestors", got, ancestorCount)
	}
}

func TestKbuildWorkingTreeMaterializedCoreSurvivesAppendThenArchiveMark(t *testing.T) {
	const (
		ancestorCount = 256
		archiveCount  = 64
	)
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	previous := ""
	for index := 0; index < ancestorCount; index++ {
		id := "ancestor-" + planOrdinal(index)
		node := workingTreeTopologyNodeForTest(id)
		if previous != "" {
			node.Inputs = []ActionPlanNodeEdge{{Role: "input", ProducerID: previous}}
		}
		plan.Nodes = append(plan.Nodes, node)
		previous = id
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	profile := CompactKbuildProfile{Name: "append-archive-scale"}
	root := []compactKbuildRuleInput{{producer: previous}}
	query := func() {
		t.Helper()
		if _, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots(
			"consumer", profile, nil, root,
		); err != nil {
			t.Fatal(err)
		}
	}
	query()
	for index := 0; index < archiveCount; index++ {
		id := "unrelated-archive-" + planOrdinal(index)
		appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest(id))
		if err := plan.markPathSensitiveArchiveOutput(id, 0); err != nil {
			t.Fatal(err)
		}
		query()
	}

	// The synchronous append-then-mark sequence cannot affect an ancestry
	// rooted in the preceding plan prefix. Cardinality counters keep this a
	// deterministic scale regression test: clearing on every archive would do
	// archiveCount+1 full core computations and ancestor projections.
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("append-then-mark performed %d materialized core computations, want %d", got, want)
	}
	if got, want := plan.workingTreeMaterializedNodeProjections, ancestorCount; got != want {
		t.Fatalf("append-then-mark projected %d ancestor nodes, want %d", got, want)
	}
	if got, want := len(plan.workingTreeMaterializedCoreCache), 1; got != want {
		t.Fatalf("append-then-mark retained %d materialized cores, want %d", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCoreInvalidatesOnLivePolicyMutation(t *testing.T) {
	const producer = "archive"
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{"archive-recipe": {
			Schema: LinuxKernelPlanSchema,
			Kind:   "generate",
			Tool:   "cc",
		}},
		Nodes: []ActionPlanNode{{
			ID: producer, Recipe: "archive-recipe",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "archive.a"}},
		}},
	}
	profile := CompactKbuildProfile{Name: "materialized-core-live-policy"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	root := []compactKbuildRuleInput{{producer: producer}}
	query := func() {
		t.Helper()
		if _, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, nil, root); err != nil {
			t.Fatal(err)
		}
	}
	query()
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("initial materialized core computations = %d, want %d", got, want)
	}
	if err := plan.markPathSensitiveArchiveOutput(producer, 0); err != nil {
		t.Fatal(err)
	}
	if plan.workingTreeMaterializedCoreCache != nil || plan.workingTreeMaterializedNodeInputSets != nil {
		t.Fatal("path-sensitive policy mutation retained materialized core state")
	}
	query()
	query()
	if got, want := plan.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("live-policy materialized core computations = %d, want one rebuild after policy mutation (%d)", got, want)
	}
	if got, want := len(plan.workingTreeMaterializedCoreCache), 1; got != want {
		t.Fatalf("path-sensitive ancestry retained %d replayable cores, want %d", got, want)
	}
	for _, core := range plan.workingTreeMaterializedCoreCache {
		if len(core.replayNodes) != 1 || core.replayNodes[0].index != 0 || !slices.Equal(core.replayNodes[0].slots, []int{0}) {
			t.Fatalf("path-sensitive core replay = %#v, want node 0 slot 0", core.replayNodes)
		}
	}
}

func TestKbuildWorkingTreeMaterializedCorePreservesNativeSlotAndRecipeLocalPolicy(t *testing.T) {
	const producer = "multi-output"
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{{
			ID: producer,
			Outputs: []ActionPlanOutput{
				{Tree: "objects", Path: "first.o"},
				{Tree: "objects", Path: "second.o"},
			},
		}},
	}
	profile := CompactKbuildProfile{Name: "materialized-core-native-slot"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	root := []compactKbuildRuleInput{{producer: producer}}
	if _, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, nil, root); err != nil {
		t.Fatal(err)
	}
	appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("unrelated-native-slot"))

	inputs, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots(
		"consumer", profile,
		[]compactKbuildRuleInput{{
			path: "first.o", producer: producer, slot: 0,
			recipeLocal: true, orderOnly: true,
		}},
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := inputs[0]; got.path != "first.o" || got.workingOnly || !got.recipeLocal || !got.orderOnly {
		t.Fatalf("recipe-local native slot = %#v, want native/order-only/recipe-local first.o", got)
	}
	if got := inputs[1]; got.path != "second.o" || !got.workingOnly {
		t.Fatalf("sibling output slot = %#v, want working-only second.o", got)
	}

	inputs, err = builder.compactKbuildWorkingTreeClosureInputsFromRoots(
		"consumer", profile,
		[]compactKbuildRuleInput{{path: "native-alias", producer: producer, slot: 1}},
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
	}
	if got := byPath["first.o"]; !got.workingOnly {
		t.Fatalf("non-native slot inherited sibling native policy: %#v", got)
	}
	if got := byPath["second.o"]; got.workingOnly {
		t.Fatalf("exact native slot remained working-only: %#v", got)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("native path collision performed %d computations, want one live replay beyond the shared core (%d)", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCoreClearsOnExplicitMutation(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{workingTreeTopologyNodeForTest("root")}}
	profile := CompactKbuildProfile{Name: "materialized-core-explicit-mutation"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	root := []compactKbuildRuleInput{{producer: "root"}}
	query := func() []compactKbuildRuleInput {
		t.Helper()
		inputs, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, nil, root)
		if err != nil {
			t.Fatal(err)
		}
		return inputs
	}
	if got := query()[0].path; got != "root.o" {
		t.Fatalf("initial output path = %q, want root.o", got)
	}
	plan.Nodes[0].Outputs[0].Path = "rebuilt.o"
	plan.invalidateLookupIndexes()
	if plan.workingTreeMaterializedCoreCache != nil || plan.workingTreeMaterializedNodeInputSets != nil {
		t.Fatal("explicit lookup invalidation retained materialized core state")
	}
	if got := query()[0].path; got != "rebuilt.o" {
		t.Fatalf("post-mutation output path = %q, want rebuilt.o", got)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("post-mutation materialized core computations = %d, want %d", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCoreRetainsDuplicateSlotFailure(t *testing.T) {
	const producer = "duplicate-slots"
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{{
			ID: producer,
			Outputs: []ActionPlanOutput{
				{Tree: "objects", Path: "duplicate.o"},
				{Tree: "objects", Path: "duplicate.o"},
			},
		}},
	}
	profile := CompactKbuildProfile{Name: "materialized-core-duplicate-slots"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	root := []compactKbuildRuleInput{{producer: producer}}
	query := func() error {
		_, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots("consumer", profile, nil, root)
		return err
	}
	if err := query(); err == nil || !strings.Contains(err.Error(), "multiple output slots") {
		t.Fatalf("initial duplicate-slot error = %v", err)
	}
	appendWorkingTreeTopologyNodeForTest(t, plan, workingTreeTopologyNodeForTest("unrelated-duplicate-slot"))
	if err := query(); err == nil || !strings.Contains(err.Error(), "multiple output slots") {
		t.Fatalf("cached-core duplicate-slot error = %v", err)
	}
	if got, want := plan.workingTreeMaterializedCoreComputations, 1; got != want {
		t.Fatalf("duplicate-slot queries computed %d cores, want %d", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCorePreservesConsumerConflictOrder(t *testing.T) {
	newPlan := func() *ActionPlan {
		z := workingTreeTopologyNodeForTest("z-producer")
		z.Outputs[0].Path = "z.out"
		a := workingTreeTopologyNodeForTest("a-producer")
		a.Outputs[0].Path = "a.out"
		return &ActionPlan{
			Recipes: map[string]ActionRecipe{},
			Nodes: []ActionPlanNode{
				z,
				a,
				workingTreeTopologyNodeForTest("root", z.ID, a.ID),
			},
		}
	}
	profile := CompactKbuildProfile{Name: "materialized-core-conflict-order"}
	root := []compactKbuildRuleInput{{producer: "root"}}
	conflicts := []compactKbuildRuleInput{
		{path: "z.out", sourceID: "immutable-z"},
		{path: "a.out", sourceID: "immutable-a"},
	}
	query := func(plan *ActionPlan, direct []compactKbuildRuleInput) error {
		t.Helper()
		builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
		_, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots(
			"consumer", profile, direct, root,
		)
		return err
	}

	cached := newPlan()
	if err := query(cached, nil); err != nil {
		t.Fatalf("prime materialized core: %v", err)
	}
	cachedErr := query(cached, conflicts)
	uncachedErr := query(newPlan(), conflicts)
	if cachedErr == nil || uncachedErr == nil || cachedErr.Error() != uncachedErr.Error() {
		t.Fatalf("consumer conflict order: cached error %v, uncached error %v", cachedErr, uncachedErr)
	}
	if !strings.Contains(cachedErr.Error(), `path "z.out"`) || strings.Contains(cachedErr.Error(), `path "a.out"`) {
		t.Fatalf("first consumer conflict = %v, want DFS-first z.out before lexical a.out", cachedErr)
	}
	if got, want := cached.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("consumer collision did not use safe live replay: %d computations, want %d", got, want)
	}
}

func TestKbuildWorkingTreeMaterializedCoreFallsBackForDifferentConflictRootOrder(t *testing.T) {
	newPlan := func() *ActionPlan {
		z := workingTreeTopologyNodeForTest("z-producer")
		z.Outputs[0].Path = "z.out"
		a := workingTreeTopologyNodeForTest("a-producer")
		a.Outputs[0].Path = "a.out"
		return &ActionPlan{Recipes: map[string]ActionRecipe{}, Nodes: []ActionPlanNode{z, a}}
	}
	profile := CompactKbuildProfile{Name: "materialized-core-root-order"}
	conflicts := []compactKbuildRuleInput{
		{path: "z.out", sourceID: "immutable-z"},
		{path: "a.out", sourceID: "immutable-a"},
	}
	query := func(plan *ActionPlan, direct, roots []compactKbuildRuleInput) error {
		t.Helper()
		builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
		_, err := builder.compactKbuildWorkingTreeClosureInputsFromRoots(
			"consumer", profile, direct, roots,
		)
		return err
	}

	cached := newPlan()
	if err := query(cached, nil, []compactKbuildRuleInput{{producer: "z-producer"}, {producer: "a-producer"}}); err != nil {
		t.Fatalf("prime materialized core: %v", err)
	}
	reversedRoots := []compactKbuildRuleInput{{producer: "a-producer"}, {producer: "z-producer"}}
	cachedErr := query(cached, conflicts, reversedRoots)
	uncachedErr := query(newPlan(), conflicts, reversedRoots)
	if cachedErr == nil || uncachedErr == nil || cachedErr.Error() != uncachedErr.Error() {
		t.Fatalf("reordered roots consumer conflict: cached error %v, uncached error %v", cachedErr, uncachedErr)
	}
	if !strings.Contains(cachedErr.Error(), `path "a.out"`) || strings.Contains(cachedErr.Error(), `path "z.out"`) {
		t.Fatalf("reordered-root first consumer conflict = %v, want live a.out order", cachedErr)
	}
	if got, want := cached.workingTreeMaterializedCoreComputations, 2; got != want {
		t.Fatalf("reordered conflicting roots performed %d core computations, want safe live fallback (%d)", got, want)
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
	visited := actionPlanNodeVisitSet{}
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
