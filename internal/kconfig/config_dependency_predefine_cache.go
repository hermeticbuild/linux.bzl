package kconfig

const (
	configDependencyMaximumPredefineEntries = 64
	configDependencyMaximumPredefineRecords = 262144
	configDependencyMaximumPredefineBytes   = 64 << 20
)

type configDependencyPredefineCacheCost struct {
	records int
	bytes   int
}

type configDependencyPredefineCacheEntry struct {
	parsed configDependencyCompilerPredefineParse
	cost   configDependencyPredefineCacheCost
}

// This sequential, family-owned memo is not compiler evidence. Exact lookup
// remains downstream of current probe registration/replay and readiness checks.
// FIFO eviction drops only cache-owned references; borrowed immutable roots and
// variant-local forced-header specializations remain valid.
//
// These are admission payload/work limits, not a total heap ceiling. Snapshot
// lookup memos and lazy projections can grow after admission, and independent
// request/forced-header caches or live scanners may retain the same backing data.
type configDependencyPredefineCache struct {
	entries map[string]configDependencyPredefineCacheEntry
	order   []string
	cost    configDependencyPredefineCacheCost

	maximumEntries int
	maximumRecords int
	maximumBytes   int
}

func newConfigDependencyPredefineCache() *configDependencyPredefineCache {
	return &configDependencyPredefineCache{
		maximumEntries: configDependencyMaximumPredefineEntries,
		maximumRecords: configDependencyMaximumPredefineRecords,
		maximumBytes:   configDependencyMaximumPredefineBytes,
	}
}

func (c *configDependencyPredefineCache) get(key string) (configDependencyCompilerPredefineParse, bool) {
	if c == nil {
		return configDependencyCompilerPredefineParse{}, false
	}
	entry, found := c.entries[key]
	return entry.parsed, found
}

func (c *configDependencyPredefineCache) put(key, contents string, parsed configDependencyCompilerPredefineParse) {
	if c == nil || c.maximumEntries <= 0 || c.maximumRecords <= 0 || c.maximumBytes <= 0 {
		return
	}
	if _, found := c.entries[key]; found {
		// An exact-key hit neither replaces its immutable root nor renews its
		// FIFO position. This memo's caller never publishes different values
		// for the same complete dump/guard/mode key.
		return
	}
	cost, fits := configDependencyPredefineCacheMeasure(key, contents, parsed, c.maximumRecords, c.maximumBytes)
	if !fits {
		// One oversized parse must not evict useful entries or alter analysis.
		return
	}
	for len(c.entries) >= c.maximumEntries || cost.records > c.maximumRecords-c.cost.records ||
		cost.bytes > c.maximumBytes-c.cost.bytes {
		c.evictOldest()
	}
	if c.entries == nil {
		c.entries = map[string]configDependencyPredefineCacheEntry{}
	}
	c.entries[key] = configDependencyPredefineCacheEntry{parsed: parsed, cost: cost}
	c.order = append(c.order, key)
	c.cost.records += cost.records
	c.cost.bytes += cost.bytes
}

func (c *configDependencyPredefineCache) evictOldest() {
	key := c.order[0]
	entry := c.entries[key]
	delete(c.entries, key)
	c.cost.records -= entry.cost.records
	c.cost.bytes -= entry.cost.bytes
	copy(c.order, c.order[1:])
	// A shortened slice alone would retain the last key in its backing array.
	c.order[len(c.order)-1] = ""
	c.order = c.order[:len(c.order)-1]
}

func (cost *configDependencyPredefineCacheCost) add(records, bytes, maximumRecords, maximumBytes int) bool {
	if records < 0 || bytes < 0 || records > maximumRecords-cost.records || bytes > maximumBytes-cost.bytes {
		return false
	}
	cost.records += records
	cost.bytes += bytes
	return true
}

// Measure only fresh parse/apply results. Unexpected speculative state or
// context-bound intrinsic state declines memoization rather than traversing
// arbitrary parent/trace graphs. This is never an analysis rejection.
//
// Charge the full key and original dump separately: parsed substrings may pin
// the latter's backing string. Each retained tree/map/slice record and its string
// payload is also charged independently, even when storage is shared. Early
// refusal bounds the accounting walk; no semantic lookup or lazy projection is
// performed while counting.
func configDependencyPredefineCacheMeasure(
	key, contents string, parsed configDependencyCompilerPredefineParse,
	maximumRecords, maximumBytes int,
) (configDependencyPredefineCacheCost, bool) {
	var cost configDependencyPredefineCacheCost
	add := func(records int, text string) bool {
		return cost.add(records, len(text), maximumRecords, maximumBytes)
	}
	state := &parsed.state
	if state.parent != nil || state.materializedSnapshotCache != nil || state.forcedHeaderTrace != nil ||
		!state.snapshotChanges.empty() || !state.materializedSnapshotPending.empty() ||
		state.forcedHeaderTouches.first != "" || len(state.forcedHeaderTouches.additional) != 0 ||
		state.compilerPredefinedSnapshot != nil && state.compilerPredefinedSnapshot != state.snapshot {
		return cost, false
	}
	if !add(1, key) || !add(0, contents) || !add(0, parsed.reason) {
		return cost, false
	}
	if snapshot := state.snapshot; snapshot != nil {
		if !add(1, "") {
			return cost, false
		}
		stack := []*configDependencyMacroSnapshotTree{
			snapshot.config.facts.root, snapshot.ordinary.facts.root, snapshot.reserved.facts.root,
		}
		for len(stack) != 0 {
			last := len(stack) - 1
			node := stack[last]
			stack[last] = nil
			stack = stack[:last]
			if node == nil {
				continue
			}
			if !add(1, node.name) {
				return cost, false
			}
			if node.left.root != nil {
				stack = append(stack, node.left.root)
			}
			if node.right.root != nil {
				stack = append(stack, node.right.root)
			}
		}
	}
	for name := range state.symbols {
		if !add(1, name) {
			return cost, false
		}
	}
	for _, expansion := range state.macroExpansions {
		if !add(1, expansion.name) {
			return cost, false
		}
		for _, name := range expansion.identifiers {
			if !add(1, name) {
				return cost, false
			}
		}
		for _, prefix := range expansion.safeTokenPastePrefixes {
			if !add(1, prefix) {
				return cost, false
			}
		}
	}
	for name, replacement := range state.macroReplacements {
		if replacement.intrinsic != nil || !add(1, name) || !add(0, replacement.text) || !add(0, replacement.origin) {
			return cost, false
		}
	}
	return cost, true
}
