package kconfig

import "fmt"

// configDependencyInputSetQuery belongs to one immutable analysis snapshot.
// Its summaries follow unique persistent subtrees, not each root's flattened
// closure. Do not retain it across mutations of a public ActionPlan.
type configDependencyInputSetQuery struct {
	plan        *ActionPlan
	store       *ActionPlanInputSetStore
	storeLoaded bool
	storeErr    error
	summaries   map[string]configDependencyInputSetSummary
	visiting    map[string]bool
}

type configDependencyInputSetEvent struct {
	key         string
	description string
	err         error
}

type configDependencyInputSetSummary struct {
	staged        bool
	stageError    configDependencyInputSetEvent
	uses          [4]configDependencyInputSetEvent
	structuralErr error
}

func newConfigDependencyInputSetQuery(plan *ActionPlan) *configDependencyInputSetQuery {
	return &configDependencyInputSetQuery{
		plan: plan, summaries: map[string]configDependencyInputSetSummary{}, visiting: map[string]bool{},
	}
}

func (q *configDependencyInputSetQuery) stagesConfig(root string) (bool, error) {
	summary := q.rootSummary(root)
	if summary.structuralErr != nil {
		return false, summary.structuralErr
	}
	if summary.stageError.err != nil {
		return false, summary.stageError.err
	}
	return summary.staged, nil
}

func (q *configDependencyInputSetQuery) configUse(root string, source, scanAllArguments bool) (string, bool, error) {
	summary := q.rootSummary(root)
	if summary.structuralErr != nil {
		return "", false, summary.structuralErr
	}
	index := 0
	if source {
		index += 2
	}
	if scanAllArguments {
		index++
	}
	event := summary.uses[index]
	if event.err != nil {
		return "", false, event.err
	}
	return event.description, event.description != "", nil
}

func (q *configDependencyInputSetQuery) rootSummary(root string) configDependencyInputSetSummary {
	if root == "" {
		return configDependencyInputSetSummary{}
	}
	if !q.storeLoaded {
		q.storeLoaded = true
		q.store, q.storeErr = q.plan.planningActionPlanInputSetStore()
		if q.storeErr == nil {
			q.storeErr = q.store.ready()
		}
	}
	if q.storeErr != nil {
		return configDependencyInputSetSummary{structuralErr: q.storeErr}
	}
	if err := q.store.validateRootReference(root); err != nil {
		return configDependencyInputSetSummary{structuralErr: err}
	}
	return q.summarize(root)
}

func earlierConfigDependencyInputSetEvent(left, right configDependencyInputSetEvent) configDependencyInputSetEvent {
	if left.key == "" || right.key != "" && right.key < left.key {
		return right
	}
	return left
}

func (q *configDependencyInputSetQuery) summarize(id string) configDependencyInputSetSummary {
	if summary, ok := q.summaries[id]; ok {
		return summary
	}
	if q.visiting[id] {
		return configDependencyInputSetSummary{structuralErr: fmt.Errorf("input-set node cycle at %q", id)}
	}
	q.visiting[id] = true
	defer delete(q.visiting, id)
	summary := configDependencyInputSetSummary{}
	node, err := q.store.nodeAt(id)
	if err != nil {
		summary.structuralErr = err
	} else if len(node.Entries) != 0 {
		for _, entry := range node.Entries {
			key := actionPlanInputSetTargetKey(entry.Target)
			_, pathStagesConfig := configDependencyProjectionMention(entry.Target.Path)
			// Path matches bypass provenance only for stage detection. A use
			// query must still resolve its exact source or producer provenance.
			var provenance configDependencyInputSetProvenance
			var provenanceErr error
			if !pathStagesConfig || entry.CompilerUse || entry.AuxiliaryUse {
				provenance, provenanceErr = actionPlanConfigDependencyInputSetProvenance(q.plan, entry)
			}
			if pathStagesConfig {
				summary.staged = true
			} else if provenanceErr != nil {
				summary.stageError = earlierConfigDependencyInputSetEvent(summary.stageError,
					configDependencyInputSetEvent{key: key, err: provenanceErr})
			} else {
				summary.staged = summary.staged || provenance.config
			}
			if provenanceErr == nil && !provenance.config {
				continue
			}
			event := configDependencyInputSetEvent{key: key, err: provenanceErr}
			if provenanceErr == nil {
				event.description = actionPlanInputSetTargetDescription(entry.Target)
			}
			for index := range summary.uses {
				source, scanAllArguments := index >= 2, index%2 != 0
				if !(entry.AuxiliaryUse || scanAllArguments && entry.CompilerUse) ||
					source && entry.SourceID == "" || !source && entry.ProducerID == "" {
					continue
				}
				summary.uses[index] = earlierConfigDependencyInputSetEvent(summary.uses[index], event)
			}
		}
	} else {
		for _, child := range node.Children {
			childSummary := q.summarize(child.ID)
			// Walk collects the whole trie before invoking any callbacks, so
			// structural errors cannot be hidden by an earlier config match.
			if childSummary.structuralErr != nil {
				summary.structuralErr = childSummary.structuralErr
				break
			}
			summary.staged = summary.staged || childSummary.staged
			summary.stageError = earlierConfigDependencyInputSetEvent(summary.stageError, childSummary.stageError)
			for index := range summary.uses {
				summary.uses[index] = earlierConfigDependencyInputSetEvent(summary.uses[index], childSummary.uses[index])
			}
		}
	}
	q.summaries[id] = summary
	return summary
}
