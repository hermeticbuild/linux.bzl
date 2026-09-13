package kconfig

import (
	"fmt"
	"path"
	"sort"
)

func cloneActionPlanInputSetNodes(nodes map[string]ActionPlanInputSetNode) map[string]ActionPlanInputSetNode {
	out := make(map[string]ActionPlanInputSetNode, len(nodes))
	for id, node := range nodes {
		out[id] = cloneActionPlanInputSetNode(node)
	}
	return out
}

// planningActionPlanInputSetStore returns the persistent store used while a
// plan is assembled. A deserialized plan imports its public canonical node map
// once; a newly lowered plan starts with an empty store.
func (p *ActionPlan) planningActionPlanInputSetStore() (*ActionPlanInputSetStore, error) {
	if p == nil {
		return nil, fmt.Errorf("input-set store requires an action plan")
	}
	if p.inputSetStore != nil {
		return p.inputSetStore, nil
	}
	store, err := NewActionPlanInputSetStoreFromNodes(p.InputSets)
	if err != nil {
		return nil, err
	}
	store.allowProvisionalProducerIDs = true
	p.inputSetStore = store
	return store, nil
}

// serializedActionPlanInputSetStore deliberately imports the public map on
// every validation boundary. ActionPlan is a public mutable test/API value;
// validation must observe a caller mutation instead of trusting planner-only
// cached nodes or collision witnesses.
func (p *ActionPlan) serializedActionPlanInputSetStore() (*ActionPlanInputSetStore, error) {
	if p == nil {
		return nil, fmt.Errorf("input-set store requires an action plan")
	}
	return NewActionPlanInputSetStoreFromNodes(p.InputSets)
}

// exportReachableActionPlanInputSets drops historical persistent roots before
// serialization. Sharing remains represented by repeated child IDs, while a
// plan never ships nodes reachable only from a sibling configuration.
func (p *ActionPlan) exportReachableActionPlanInputSets() error {
	if p == nil {
		return fmt.Errorf("cannot export input sets from a nil action plan")
	}
	store, err := p.planningActionPlanInputSetStore()
	if err != nil {
		return err
	}
	roots := make([]string, 0, len(p.Nodes))
	for _, node := range p.Nodes {
		roots = append(roots, node.InputSet)
	}
	reachable, err := store.ReachableNodesForRoots(roots)
	if err != nil {
		return fmt.Errorf("action-plan input sets: %w", err)
	}
	p.InputSets = reachable
	return nil
}

func actionPlanInputSetProducerIDs(store *ActionPlanInputSetStore, root string) ([]string, error) {
	producers := []string{}
	seen := map[string]bool{}
	err := store.Walk(root, func(entry ActionPlanInputSetEntry) error {
		if entry.ProducerID != "" && !seen[entry.ProducerID] {
			seen[entry.ProducerID] = true
			producers = append(producers, entry.ProducerID)
		}
		return nil
	})
	return producers, err
}

func (p *ActionPlan) actionPlanNodeProducerIDs(node ActionPlanNode) ([]string, error) {
	producers := make([]string, 0, len(node.Inputs))
	seen := map[string]bool{}
	for _, input := range node.Inputs {
		if !seen[input.ProducerID] {
			seen[input.ProducerID] = true
			producers = append(producers, input.ProducerID)
		}
	}
	store, err := p.planningActionPlanInputSetStore()
	if err != nil {
		return nil, err
	}
	setProducers, err := actionPlanInputSetProducerIDs(store, node.InputSet)
	if err != nil {
		return nil, err
	}
	for _, producer := range setProducers {
		if !seen[producer] {
			seen[producer] = true
			producers = append(producers, producer)
		}
	}
	return producers, nil
}

// actionPlanProducerTraversal shares adjacency reads only within one immutable
// traversal. Discard it before changing nodes or input sets. Neither the plan
// nor the family cache retains a flattened graph. Returned slices are read-only.
type actionPlanProducerTraversal struct {
	plan      *ActionPlan
	byNode    map[string][]string
	remaining int
}

func (q *actionPlanProducerTraversal) producers(node ActionPlanNode) ([]string, error) {
	if producers, ok := q.byNode[node.ID]; ok {
		return producers, nil
	}
	producers, err := q.plan.actionPlanNodeProducerIDs(node)
	if err != nil {
		return nil, err
	}
	// Charge empty lists too, bounding both records and producer references.
	// Failed reads are not cached; saturation changes cost, not semantics.
	cost := 1 + len(producers)
	if cost <= q.remaining {
		if q.byNode == nil {
			q.byNode = map[string][]string{}
		}
		q.byNode[node.ID] = producers
		q.remaining -= cost
	}
	return producers, nil
}

// descendsFrom preserves the producer DFS's reverse-stack order and early
// ancestor match, including references to missing nodes. Visit tracking uses
// existing immutable node ordinals, packed into sparse 64-node words. Unlike a
// dense plan-sized bitmap, storage grows only with the blocks actually reached.
// This is per-query visitation, not a retained reachability or absence proof.
func (q *actionPlanProducerTraversal) descendsFrom(descendant, ancestor string) (bool, error) {
	q.plan.ensureNodeLookupIndexes()
	pending := []string{descendant}
	seen := map[uint32]uint64{}
	for len(pending) != 0 {
		last := len(pending) - 1
		producer := pending[last]
		pending = pending[:last]
		if producer == ancestor {
			return true, nil
		}
		index, ok := q.plan.nodeIndexesByID[producer]
		if !ok {
			continue
		}
		word, bit := index/64, uint64(1)<<(index%64)
		if seen[word]&bit != 0 {
			continue
		}
		seen[word] |= bit
		producers, err := q.producers(q.plan.Nodes[index])
		if err != nil {
			return false, err
		}
		pending = append(pending, producers...)
	}
	return false, nil
}

// validateAndEncodeActionPlanInputSets checks the complete serialized store,
// validates every provenance edge in each consumer's stage, and emits the
// path-only marker graph understood by map_directory. The manifest is the
// canonical content witness; leaf provenance remains encoded in marker paths
// so the callback never needs to read generated file contents.
func validateAndEncodeActionPlanInputSets(
	p *ActionPlan,
	nodes map[string]ActionPlanNode,
	sources map[string]ActionPlanSource,
	emitMarkers bool,
) (*ActionPlanInputSetStore, []actionPlanEntry, error) {
	store, err := p.serializedActionPlanInputSetStore()
	if err != nil {
		return nil, nil, err
	}
	stageOrder := map[string]int{"prehost": 0, "bootstrap": 1, "host": 2, "prep": 3, "target": 4}
	roots := make([]string, 0, len(p.Nodes))
	for _, consumer := range p.Nodes {
		roots = append(roots, consumer.InputSet)
	}
	reachable, err := store.ReachableNodesForRoots(roots)
	if err != nil {
		return nil, nil, fmt.Errorf("action-plan input sets: %w", err)
	}
	type producerStageSummary struct {
		ordinal  int
		stage    string
		producer string
		valid    bool
	}
	summaries := make(map[string]producerStageSummary, len(reachable))
	var summarize func(string) (producerStageSummary, error)
	summarize = func(inputSetID string) (producerStageSummary, error) {
		if inputSetID == "" {
			return producerStageSummary{}, nil
		}
		if summary, ok := summaries[inputSetID]; ok {
			return summary, nil
		}
		inputSetNode, ok := reachable[inputSetID]
		if !ok {
			return producerStageSummary{}, fmt.Errorf("input-set node %s is not reachable", inputSetID)
		}
		summary := producerStageSummary{}
		for _, entry := range inputSetNode.Entries {
			if entry.SourceID != "" {
				if _, ok := sources[entry.SourceID]; !ok {
					return producerStageSummary{}, fmt.Errorf("input set references unknown source %s", entry.SourceID)
				}
				continue
			}
			producer, ok := nodes[entry.ProducerID]
			if !ok {
				return producerStageSummary{}, fmt.Errorf("input set references unknown producer %s", entry.ProducerID)
			}
			if entry.Slot < 0 || entry.Slot >= len(producer.Outputs) {
				return producerStageSummary{}, fmt.Errorf("input set references output slot %d of producer %s with %d outputs", entry.Slot, producer.ID, len(producer.Outputs))
			}
			ordinal := stageOrder[producer.Stage]
			if !summary.valid || ordinal > summary.ordinal {
				summary = producerStageSummary{ordinal: ordinal, stage: producer.Stage, producer: producer.ID, valid: true}
			}
		}
		for _, child := range inputSetNode.Children {
			childSummary, err := summarize(child.ID)
			if err != nil {
				return producerStageSummary{}, err
			}
			if childSummary.valid && (!summary.valid || childSummary.ordinal > summary.ordinal) {
				summary = childSummary
			}
		}
		summaries[inputSetID] = summary
		return summary, nil
	}
	for _, consumer := range p.Nodes {
		summary, err := summarize(consumer.InputSet)
		if err != nil {
			return nil, nil, fmt.Errorf("node %s input set: %w", consumer.ID, err)
		}
		if summary.valid && summary.ordinal > stageOrder[consumer.Stage] {
			return nil, nil, fmt.Errorf("node %s in %s stage has backward input-set dependency from %s node %s", consumer.ID, consumer.Stage, summary.stage, summary.producer)
		}
	}
	if len(reachable) != len(p.InputSets) {
		return nil, nil, fmt.Errorf("kernel action plan serializes %d input-set nodes, but %d are reachable", len(p.InputSets), len(reachable))
	}
	for id := range p.InputSets {
		if _, ok := reachable[id]; !ok {
			return nil, nil, fmt.Errorf("kernel action plan serializes unreachable input-set node %s", id)
		}
	}
	if !emitMarkers {
		return store, nil, nil
	}

	entries := []actionPlanEntry{}
	ids := make([]string, 0, len(p.InputSets))
	for id := range p.InputSets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node, _ := store.Node(id)
		witness, ok := store.CanonicalWitness(id)
		if !ok {
			return nil, nil, fmt.Errorf("input-set node %s has no canonical manifest", id)
		}
		root := path.Join("input-sets", id)
		entries = append(entries, actionPlanEntry{path: path.Join(root, "manifest", id+".json"), data: witness})
		for _, child := range node.Children {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "child", child.Nibble, child.ID)})
		}
		for ordinal, entry := range node.Entries {
			if entry.SourceID != "" {
				entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "source", planOrdinal(ordinal), entry.SourceID)})
			} else {
				entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "node", planOrdinal(ordinal), entry.ProducerID, planOrdinal(entry.Slot))})
			}
		}
	}
	return store, entries, nil
}
