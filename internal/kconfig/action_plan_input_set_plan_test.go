package kconfig

import (
	"fmt"
	"reflect"
	"testing"
)

func producerTraversalFixture(t testing.TB, count int) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	store := NewActionPlanInputSetStore()
	root := ""
	for index := 0; index < count; index++ {
		entry := actionPlanInputSetTestEntry(index)
		entry.SourceID = ""
		entry.ProducerID = actionPlanInputSetTestDigest(fmt.Sprint(index % 7))
		var err error
		root, err = store.Insert(root, entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	return &ActionPlan{inputSetStore: store}, ActionPlanNode{
		ID: "consumer", InputSet: root,
		Inputs: []ActionPlanNodeEdge{
			{ProducerID: actionPlanInputSetTestDigest("3")},
			{ProducerID: actionPlanInputSetTestDigest("3")},
			{ProducerID: actionPlanInputSetTestDigest("direct")},
		},
	}
}

func TestActionPlanProducerTraversal(t *testing.T) {
	plan, node := producerTraversalFixture(t, 128)
	want, err := plan.actionPlanNodeProducerIDs(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{0, 1, 8, 9, 32768} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			query := actionPlanProducerTraversal{plan: plan, remaining: budget}
			for repeat := 0; repeat < 3; repeat++ {
				got, err := query.producers(node)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("read %d: got %v, %v; want %v", repeat, got, err, want)
				}
			}
			retained := 0
			for _, producers := range query.byNode {
				retained += 1 + len(producers)
			}
			if retained > budget || query.remaining != budget-retained {
				t.Fatalf("budget %d: retained=%d remaining=%d", budget, retained, query.remaining)
			}
			if budget >= 1+len(want) && len(query.byNode) != 1 {
				t.Fatal("successful repeated read was not shared")
			}
		})
	}
}

func TestActionPlanProducerTraversalErrorsAndLifetime(t *testing.T) {
	plan, node := producerTraversalFixture(t, 32)
	query := actionPlanProducerTraversal{plan: plan, remaining: 100}
	validRoot := node.InputSet
	node.InputSet = actionPlanInputSetTestDigest("missing")
	_, wantErr := plan.actionPlanNodeProducerIDs(node)
	for repeat := 0; repeat < 2; repeat++ {
		_, err := query.producers(node)
		if wantErr == nil || err == nil || err.Error() != wantErr.Error() {
			t.Fatalf("error changed: got %v; want %v", err, wantErr)
		}
	}
	if len(query.byNode) != 0 || query.remaining != 100 {
		t.Fatal("failed read consumed cache budget")
	}
	node.InputSet = validRoot
	if _, err := query.producers(node); err != nil {
		t.Fatal(err)
	}
	// New traversals observe changed inputs even under the same public ID.
	node.InputSet = ""
	node.Inputs = nil
	next := actionPlanProducerTraversal{plan: plan, remaining: 100}
	got, err := next.producers(node)
	if err != nil || len(got) != 0 {
		t.Fatalf("new traversal reused stale edges: %v, %v", got, err)
	}
	if next.remaining != 99 || len(next.byNode) != 1 {
		t.Fatal("empty result was not charged and retained")
	}
}

func BenchmarkActionPlanProducerTraversal(b *testing.B) {
	plan, node := producerTraversalFixture(b, 2048)
	for _, shared := range []bool{false, true} {
		b.Run(fmt.Sprintf("shared=%v", shared), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				query := actionPlanProducerTraversal{plan: plan, remaining: 32768}
				for repeat := 0; repeat < 64; repeat++ {
					var err error
					if shared {
						_, err = query.producers(node)
					} else {
						_, err = plan.actionPlanNodeProducerIDs(node)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
