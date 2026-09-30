package kconfig

import (
	"fmt"
	"testing"
)

// Keep the original string-keyed DFS as an independent behavior and cost
// reference, including reverse-stack order and matches for missing nodes.
func referenceProducerDescendsFrom(q *actionPlanProducerTraversal, descendant, ancestor string) (bool, error) {
	pending := []string{descendant}
	seen := map[string]bool{}
	for len(pending) != 0 {
		last := len(pending) - 1
		producer := pending[last]
		pending = pending[:last]
		if producer == ancestor {
			return true, nil
		}
		if seen[producer] {
			continue
		}
		seen[producer] = true
		node, ok := q.plan.nodesByID[producer]
		if !ok {
			continue
		}
		producers, err := q.producers(node)
		if err != nil {
			return false, err
		}
		pending = append(pending, producers...)
	}
	return false, nil
}

func producerLineageTestPlan(edges map[string][]string) *ActionPlan {
	plan := &ActionPlan{inputSetStore: NewActionPlanInputSetStore()}
	for id, producers := range edges {
		node := ActionPlanNode{ID: id}
		for _, producer := range producers {
			node.Inputs = append(node.Inputs, ActionPlanNodeEdge{ProducerID: producer})
		}
		plan.Nodes = append(plan.Nodes, node)
	}
	plan.ensureNodeLookupIndexes()
	return plan
}

func TestProducerTraversalVisitSetAllThreeNodeGraphs(t *testing.T) {
	names := []string{"a", "b", "c", "missing", ""}
	for graph := 0; graph < 1<<9; graph++ {
		edges := map[string][]string{}
		for from := 0; from < 3; from++ {
			edges[names[from]] = nil
			for to := 0; to < 3; to++ {
				if graph&(1<<(from*3+to)) != 0 {
					edges[names[from]] = append(edges[names[from]], names[to])
				}
			}
		}
		plan := producerLineageTestPlan(edges)
		for _, budget := range []int{0, 1, 32} {
			candidate := actionPlanProducerTraversal{plan: plan, remaining: budget}
			reference := actionPlanProducerTraversal{plan: plan, remaining: budget}
			for repeat := 0; repeat < 2; repeat++ {
				for _, descendant := range names {
					for _, ancestor := range names {
						want, wantErr := referenceProducerDescendsFrom(&reference, descendant, ancestor)
						got, err := candidate.descendsFrom(descendant, ancestor)
						if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
							t.Fatalf("graph=%d budget=%d %q -> %q: got %v,%v want %v,%v", graph, budget, descendant, ancestor, got, err, want, wantErr)
						}
					}
				}
			}
		}
	}
}

func TestProducerTraversalVisitSetErrorsAndFreshLifetime(t *testing.T) {
	plan := producerLineageTestPlan(map[string][]string{
		"a": {"c", "b"}, "b": {"a"}, "c": {"target"}, "target": nil,
		"broken": nil, "error-first": {"target", "broken"}, "target-first": {"broken", "target"},
		"empty-edge": {""},
	})
	index := plan.nodeIndexesByID["broken"]
	plan.Nodes[index].InputSet = actionPlanInputSetTestDigest("missing")
	plan.invalidateLookupIndexes()
	plan.ensureNodeLookupIndexes()
	for _, pair := range [][2]string{{"a", "target"}, {"b", "absent"}, {"error-first", "target"}, {"target-first", "target"}, {"missing", "missing"}, {"missing", "a"}, {"empty-edge", ""}} {
		candidate := actionPlanProducerTraversal{plan: plan, remaining: 32}
		reference := actionPlanProducerTraversal{plan: plan, remaining: 32}
		want, wantErr := referenceProducerDescendsFrom(&reference, pair[0], pair[1])
		got, err := candidate.descendsFrom(pair[0], pair[1])
		if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Fatalf("error order %v: got %v,%v want %v,%v", pair, got, err, want, wantErr)
		}
	}
	// A fresh traversal must observe changed public node data after the normal
	// lookup invalidation, even when the root ID and node count stay unchanged.
	index = plan.nodeIndexesByID["c"]
	plan.Nodes[index].Inputs = append(plan.Nodes[index].Inputs, ActionPlanNodeEdge{ProducerID: "absent"})
	plan.invalidateLookupIndexes()
	fresh := actionPlanProducerTraversal{plan: plan, remaining: 32}
	if got, err := fresh.descendsFrom("b", "absent"); err != nil || !got {
		t.Fatalf("fresh traversal borrowed stale visitation: %v,%v", got, err)
	}
}

func TestProducerTraversalVisitSetWordBoundaries(t *testing.T) {
	plan := &ActionPlan{inputSetStore: NewActionPlanInputSetStore()}
	for index := 0; index < 130; index++ {
		plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: fmt.Sprint(index)})
	}
	for _, edge := range [][2]int{{0, 63}, {63, 64}, {64, 128}, {128, 129}, {129, 63}, {65, 127}} {
		plan.Nodes[edge[0]].Inputs = append(plan.Nodes[edge[0]].Inputs, ActionPlanNodeEdge{ProducerID: fmt.Sprint(edge[1])})
	}
	plan.ensureNodeLookupIndexes()
	for _, descendant := range []string{"0", "1", "62", "63", "64", "65", "127", "128", "129", "missing", ""} {
		for _, ancestor := range []string{"0", "1", "62", "63", "64", "65", "127", "128", "129", "missing", ""} {
			candidate := actionPlanProducerTraversal{plan: plan, remaining: 32}
			reference := actionPlanProducerTraversal{plan: plan, remaining: 32}
			want, wantErr := referenceProducerDescendsFrom(&reference, descendant, ancestor)
			got, err := candidate.descendsFrom(descendant, ancestor)
			if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("%q -> %q: got %v,%v want %v,%v", descendant, ancestor, got, err, want, wantErr)
			}
		}
	}
}

func BenchmarkProducerTraversalVisitSet(b *testing.B) {
	for _, shape := range []struct {
		name          string
		count, stride int
	}{{"clustered", 1024, 1}, {"sparse", 64, 127}} {
		plan := &ActionPlan{inputSetStore: NewActionPlanInputSetStore()}
		for index := 0; index < shape.count*shape.stride; index++ {
			plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: fmt.Sprintf("%064d", index)})
		}
		for index := 0; index < shape.count-1; index++ {
			plan.Nodes[index*shape.stride].Inputs = []ActionPlanNodeEdge{{ProducerID: plan.Nodes[(index+1)*shape.stride].ID}}
		}
		for root := 0; root < 64; root++ {
			plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: fmt.Sprintf("root%060d", root), Inputs: []ActionPlanNodeEdge{{ProducerID: plan.Nodes[0].ID}}})
		}
		plan.ensureNodeLookupIndexes()
		for _, words := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/words=%v", shape.name, words), func(b *testing.B) {
				b.ReportAllocs()
				for iteration := 0; iteration < b.N; iteration++ {
					query := actionPlanProducerTraversal{plan: plan, remaining: 32768}
					for root := 0; root < 64; root++ {
						descendant, ancestor := fmt.Sprintf("root%060d", root), fmt.Sprintf("absent%058d", root)
						var got bool
						var err error
						if words {
							got, err = query.descendsFrom(descendant, ancestor)
						} else {
							got, err = referenceProducerDescendsFrom(&query, descendant, ancestor)
						}
						if got || err != nil {
							b.Fatalf("unexpected reachability: %v,%v", got, err)
						}
					}
				}
			})
		}
	}
}
