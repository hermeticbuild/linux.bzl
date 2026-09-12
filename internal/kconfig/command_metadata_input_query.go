package kconfig

import (
	"fmt"
	"strings"
)

type commandMetadataInputKind uint8

const (
	commandMetadataSourceInput commandMetadataInputKind = 1 << iota
	commandMetadataGeneratedInput
	commandMetadataInputSummaryLimit = 32768
)

// Summaries contain only two presence bits per immutable trie node. This avoids
// sorting or retaining flattened transitive closures for every generated rule.
// The cache is plan-local because provisional producer IDs are plan-local too.
type commandMetadataInputQuery struct {
	store     *ActionPlanInputSetStore
	summaries map[string]commandMetadataInputKind
}

func commandMetadataProducerOutput(plan *ActionPlan, producerID string, slot int) (bool, error) {
	producer, ok := plan.nodesByID[producerID]
	if !ok {
		return false, fmt.Errorf("Kbuild command metadata input references absent producer %q", producerID)
	}
	if slot < 0 || slot >= len(producer.Outputs) {
		return false, fmt.Errorf("Kbuild command metadata input references absent producer %q slot %d", producerID, slot)
	}
	output := producer.Outputs[slot]
	logicalPath := output.Path
	if output.ObservedPath != "" {
		logicalPath = output.ObservedPath
	}
	return strings.HasSuffix(canonicalKbuildRulePath(logicalPath), ".cmd"), nil
}

func (q *commandMetadataInputQuery) entryKind(plan *ActionPlan, entry ActionPlanInputSetEntry) (commandMetadataInputKind, error) {
	if entry.SourceID != "" {
		return commandMetadataSourceInput, nil
	}
	metadata, err := commandMetadataProducerOutput(plan, entry.ProducerID, entry.Slot)
	if err != nil || !metadata {
		return 0, err
	}
	return commandMetadataGeneratedInput, nil
}

func (q *commandMetadataInputQuery) summarize(plan *ActionPlan, id string) (commandMetadataInputKind, error) {
	if summary, ok := q.summaries[id]; ok {
		return summary, nil
	}
	// The store accepts only validated, content-addressed immutable tries.
	node, err := q.store.nodeAt(id)
	if err != nil {
		return 0, err
	}
	var summary commandMetadataInputKind
	for _, entry := range node.Entries {
		kind, err := q.entryKind(plan, entry)
		if err != nil {
			return 0, err
		}
		summary |= kind
	}
	for _, child := range node.Children {
		kind, err := q.summarize(plan, child.ID)
		if err != nil {
			return 0, err
		}
		summary |= kind
	}
	if len(q.summaries) < commandMetadataInputSummaryLimit {
		q.summaries[id] = summary
	}
	return summary, nil
}

// walkCommandMetadataInputs visits relevant provenance in trie order. Callers
// accumulate sets and sort their final result; staging aliases never classify
// generated metadata, since only the producer's actual output owns that fact.
func (plan *ActionPlan) walkCommandMetadataInputs(root string, wanted commandMetadataInputKind, visit func(ActionPlanInputSetEntry) error) error {
	if root == "" {
		return nil
	}
	plan.ensureNodeLookupIndexes()
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return err
	}
	if err := store.ready(); err != nil {
		return err
	}
	if err := store.validateRootReference(root); err != nil {
		return err
	}
	q := plan.commandMetadataInputQuery
	if q == nil || q.store != store || len(q.summaries) >= commandMetadataInputSummaryLimit {
		q = &commandMetadataInputQuery{store: store, summaries: map[string]commandMetadataInputKind{}}
		plan.commandMetadataInputQuery = q
	}
	var walk func(string) error
	walk = func(id string) error {
		kind, err := q.summarize(plan, id)
		if err != nil || kind&wanted == 0 {
			return err
		}
		node, err := store.nodeAt(id)
		if err != nil {
			return err
		}
		for _, entry := range node.Entries {
			kind, err := q.entryKind(plan, entry)
			if err != nil {
				return err
			}
			if kind&wanted != 0 {
				if err := visit(entry); err != nil {
					return err
				}
			}
		}
		for _, child := range node.Children {
			if err := walk(child.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}
