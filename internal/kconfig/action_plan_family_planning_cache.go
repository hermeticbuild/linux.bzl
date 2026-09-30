package kconfig

import "fmt"

// ActionPlanFamilyPlanningCache is the explicit mutable state shared while one
// image-family snapshot lowers several resolved configurations. Source/config
// observations have their own content cache, while every plan writes directly
// into one content-addressed persistent input-set store. Unchanged roots and
// subtries are therefore reused by identity; there is no parallel flattened
// working-tree graph to keep synchronized or rebind between variants.
//
// The cache is intended for sequential family planning and is not safe for
// concurrent use.
type ActionPlanFamilyPlanningCache struct {
	configDependencies *ActionPlanConfigDependencySharedCache
	// Only immutable source-derived query names are shared, never compiler
	// answers or a variant's profile union. Existing binding keys distinguish
	// physical roots, logical shadows and ambiguous bindings. This has the
	// same sequential, immutable-input lifetime as the rest of the cache.
	sourceGuardInventory               *configDependencyGuardInventory
	inputSets                          *ActionPlanInputSetStore
	materializedCores                  map[string]*compactKbuildWorkingTreeMaterializedCore
	materializedHits                   int
	materializedStores                 int
	configDependencyDiagnosticObserver func(ConfigDependencyDiagnosticSnapshot)
	configDependencyDiagnosticSequence uint64
}

// SetConfigDependencyDiagnosticObserver enables optional node-transition
// diagnostics for plans attached to this family cache. A nil observer disables
// diagnostics. Set it only between analyses. Calls occur synchronously on the
// planning goroutine; observers must not mutate or reenter planning, block on a
// profile timer, or panic. Snapshots are values safe to publish independently.
// Events are analysis_start, compiler_start, compiler_complete, and analysis_end.
// Compile-node counts include precise, opaque, and completed-cache-hit nodes;
// they do not count individual translation units or selected profiles.
func (c *ActionPlanFamilyPlanningCache) SetConfigDependencyDiagnosticObserver(
	observer func(ConfigDependencyDiagnosticSnapshot),
) {
	if c != nil {
		c.configDependencyDiagnosticObserver = observer
	}
}

// NewActionPlanFamilyPlanningCache creates an empty cache for one family
// planning action.
func NewActionPlanFamilyPlanningCache() *ActionPlanFamilyPlanningCache {
	return &ActionPlanFamilyPlanningCache{
		configDependencies:   NewActionPlanConfigDependencySharedCache(),
		sourceGuardInventory: &configDependencyGuardInventory{},
		inputSets:            newPlanningActionPlanInputSetStore(),
		materializedCores:    map[string]*compactKbuildWorkingTreeMaterializedCore{},
	}
}

func (c *ActionPlanFamilyPlanningCache) initialize() {
	if c == nil {
		return
	}
	if c.sourceGuardInventory == nil {
		c.sourceGuardInventory = &configDependencyGuardInventory{}
	}
	if c.configDependencies == nil {
		c.configDependencies = NewActionPlanConfigDependencySharedCache()
	} else {
		c.configDependencies.initialize()
	}
	if c.inputSets == nil {
		c.inputSets = newPlanningActionPlanInputSetStore()
	}
	if c.materializedCores == nil {
		c.materializedCores = map[string]*compactKbuildWorkingTreeMaterializedCore{}
	}
}

func (c *ActionPlanFamilyPlanningCache) inputSetStore() *ActionPlanInputSetStore {
	if c == nil {
		return nil
	}
	c.initialize()
	return c.inputSets
}

func (c *ActionPlanFamilyPlanningCache) configDependencyCache() *ActionPlanConfigDependencySharedCache {
	if c == nil {
		return nil
	}
	c.initialize()
	return c.configDependencies
}

func (p *ActionPlan) attachFamilyPlanningCache(cache *ActionPlanFamilyPlanningCache) {
	if p == nil || cache == nil {
		return
	}
	cache.initialize()
	p.familyPlanningCache = cache
	p.inputSetStore = cache.inputSets
	if p.metadata != nil {
		// Attach before lowering can request compiler definedness. Keep the
		// metadata's own name union/readiness and compiler callbacks untouched.
		p.metadata.sourceGuardInventory = cache.sourceGuardInventory
	}
}

func validateFamilyPersistentMaterializedEntry(plan *ActionPlan, entry ActionPlanInputSetEntry) error {
	if entry.Target.Kind != ActionPlanInputSetWorkTarget || entry.SourceID != "" || entry.ProducerID == "" {
		return fmt.Errorf("family materialized core has non-producer work entry %s", actionPlanInputSetTargetDescription(entry.Target))
	}
	node, ok := plan.nodesByID[entry.ProducerID]
	if !ok || entry.Slot < 0 || entry.Slot >= len(node.Outputs) {
		return fmt.Errorf("family materialized core references absent output %s:%d", entry.ProducerID, entry.Slot)
	}
	if plan.hasPathSensitiveArchiveOutput(entry.ProducerID) {
		return fmt.Errorf("family materialized core references path-sensitive producer %s", entry.ProducerID)
	}
	output := node.Outputs[entry.Slot]
	if output.ObservedPath != "" || output.Path != entry.Target.Path {
		return fmt.Errorf("family materialized core output %s:%d does not own %q", entry.ProducerID, entry.Slot, entry.Target.Path)
	}
	return nil
}

// lookupMaterializedCore shares only an ordinary, content-addressed core whose
// exact producer IDs exist in the current plan. Unlike the deleted structural
// cache, this performs no rebinding and stores no second graph.
func (c *ActionPlanFamilyPlanningCache) lookupMaterializedCore(
	plan *ActionPlan,
	key string,
) (*compactKbuildWorkingTreeMaterializedCore, bool, error) {
	if c == nil || plan == nil {
		return nil, false, nil
	}
	c.initialize()
	core, ok := c.materializedCores[key]
	if !ok {
		return nil, false, nil
	}
	if len(core.replayNodes) != 0 || core.replayRootOrder != "" {
		return nil, false, nil
	}
	plan.ensureNodeLookupIndexes()
	if err := c.inputSets.Walk(core.inputSet, func(entry ActionPlanInputSetEntry) error {
		return validateFamilyPersistentMaterializedEntry(plan, entry)
	}); err != nil {
		return nil, false, nil
	}
	for _, conflict := range core.conflicts {
		for _, version := range conflict.versions {
			if err := validateFamilyPersistentMaterializedEntry(plan, ActionPlanInputSetEntry{
				Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: conflict.path},
				ProducerID: version.producer,
				Slot:       version.slot,
			}); err != nil {
				return nil, false, nil
			}
		}
	}
	c.materializedHits++
	return core, true, nil
}

func (c *ActionPlanFamilyPlanningCache) storeMaterializedCore(
	key string,
	core *compactKbuildWorkingTreeMaterializedCore,
) {
	if c == nil || core == nil || len(core.replayNodes) != 0 || core.replayRootOrder != "" {
		return
	}
	c.initialize()
	if _, exists := c.materializedCores[key]; exists || len(c.materializedCores) >= maximumWorkingTreeMaterializedCoreEntries {
		return
	}
	c.materializedCores[key] = core
	c.materializedStores++
}
