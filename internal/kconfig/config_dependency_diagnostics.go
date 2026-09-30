package kconfig

import (
	"strings"
	"time"
	"unicode/utf8"
)

// ConfigDependencyDiagnosticSnapshot is an optional observation of sequential
// family dependency analysis, not a planner input or cache identity. Values may
// be retained or published to another goroutine without retaining mutable plan
// state. Cache counts describe the publication instant: forced-header counts
// are analysis-local, while completed-compiler counts are family-cumulative.
type ConfigDependencyDiagnosticSnapshot struct {
	AnalysisSequence      uint64
	Event                 string
	TotalCompileNodes     int
	StartedCompileNodes   int
	CompletedCompileNodes int
	// These count completed compile-kind analysis results, including cache hits;
	// they are not executed object actions or the report's typed-compiler subset.
	PreciseCompileNodes int
	OpaqueCompileNodes  int
	LastOpaqueReason    string
	// CurrentOutput names stage:first-logical-output (or the node ID when no
	// output exists), truncated to at most 1024 bytes. Empty stages omit the
	// prefix. It is nonempty only during compiler_start; completion and
	// analysis-end events clear it.
	CurrentOutput                     string
	PublishedAt                       time.Time
	ForcedCacheHits                   int
	ForcedCacheMisses                 int
	HeaderCacheHits                   int
	HeaderCacheMisses                 int
	HeaderCacheStores                 int
	HeaderCacheRejected               int
	HeaderCacheExactHits              int
	HeaderCacheSpecializedHits        int
	HeaderCacheRequirementMismatches  int
	HeaderCacheReplayMismatches       int
	HeaderCacheEntries                int
	HeaderCacheRecords                int
	HeaderCacheBytes                  int
	HeaderCacheRejectedEntries        int
	HeaderCacheRejectedVariants       int
	HeaderCacheRejectedRecords        int
	HeaderCacheRejectedBytes          int
	HeaderCacheRejectedTrace          int
	HeaderCacheRejectedWholeNamespace int
	HeaderCacheRejectedInexact        int
	HeaderCacheRejectedMacro          int
	HeaderCacheCaptureAttempts        int
	HeaderCacheAdmissionStopped       bool
	CompletedCacheHits                int
	CompletedCacheMisses              int
}

type configDependencyDiagnostics struct {
	observer func(ConfigDependencyDiagnosticSnapshot)
	snapshot ConfigDependencyDiagnosticSnapshot
	family   *ActionPlanFamilyPlanningCache
	context  *configDependencyAnalysisContext
}

func newConfigDependencyDiagnostics(plan *ActionPlan) *configDependencyDiagnostics {
	if plan == nil || plan.familyPlanningCache == nil ||
		plan.familyPlanningCache.configDependencyDiagnosticObserver == nil {
		return nil
	}
	family := plan.familyPlanningCache
	family.configDependencyDiagnosticSequence++
	diagnostics := &configDependencyDiagnostics{
		observer: family.configDependencyDiagnosticObserver,
		family:   family,
		snapshot: ConfigDependencyDiagnosticSnapshot{
			AnalysisSequence: family.configDependencyDiagnosticSequence,
		},
	}
	// No observer means no additional plan traversal, label work, or timestamps.
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			diagnostics.snapshot.TotalCompileNodes++
		}
	}
	return diagnostics
}

func (d *configDependencyDiagnostics) publish(event string) {
	d.snapshot.Event = event
	d.snapshot.PublishedAt = time.Now()
	if d.context != nil && d.context.forcedHeaders != nil {
		d.snapshot.ForcedCacheHits = d.context.forcedHeaders.hits
		d.snapshot.ForcedCacheMisses = d.context.forcedHeaders.misses
		d.snapshot.HeaderCacheHits = d.context.forcedHeaders.ordinary.hits
		d.snapshot.HeaderCacheMisses = d.context.forcedHeaders.ordinary.misses
		d.snapshot.HeaderCacheStores = d.context.forcedHeaders.ordinary.stores
		d.snapshot.HeaderCacheRejected = d.context.forcedHeaders.ordinary.rejected
		header := &d.context.forcedHeaders.ordinary
		d.snapshot.HeaderCacheExactHits = header.exactHits
		d.snapshot.HeaderCacheSpecializedHits = header.specializedHits
		d.snapshot.HeaderCacheRequirementMismatches = header.requirementMismatches
		d.snapshot.HeaderCacheReplayMismatches = header.replayMismatches
		d.snapshot.HeaderCacheEntries = header.entryCount
		d.snapshot.HeaderCacheRecords = header.records
		d.snapshot.HeaderCacheBytes = header.bytes
		d.snapshot.HeaderCacheRejectedEntries = header.rejectedEntries
		d.snapshot.HeaderCacheRejectedVariants = header.rejectedVariants
		d.snapshot.HeaderCacheRejectedRecords = header.rejectedRecords
		d.snapshot.HeaderCacheRejectedBytes = header.rejectedBytes
		d.snapshot.HeaderCacheRejectedTrace = header.rejectedTrace
		d.snapshot.HeaderCacheRejectedWholeNamespace = header.rejectedWholeNamespace
		d.snapshot.HeaderCacheRejectedInexact = header.rejectedInexact
		d.snapshot.HeaderCacheRejectedMacro = header.rejectedMacro
		d.snapshot.HeaderCacheCaptureAttempts = header.captureAttempts
		d.snapshot.HeaderCacheAdmissionStopped = header.admissionStopped
	}
	if shared := d.family.configDependencies; shared != nil {
		d.snapshot.CompletedCacheHits = shared.completed.hits
		d.snapshot.CompletedCacheMisses = shared.completed.misses
	}
	d.observer(d.snapshot)
}

func (d *configDependencyDiagnostics) compilerStart(node ActionPlanNode) {
	d.snapshot.StartedCompileNodes++
	output := node.ID
	for _, candidate := range node.Outputs {
		if candidate.Path != "" {
			output = candidate.Path
			break
		}
	}
	if node.Stage != "" {
		output = node.Stage + ":" + output
	}
	d.snapshot.CurrentOutput = configDependencyDiagnosticText(output)
	d.publish("compiler_start")
}

func configDependencyDiagnosticText(value string) string {
	limit := min(len(value), 1024)
	// Preserve UTF-8 boundaries for valid paths without inspecting their contents
	// or retaining a potentially much larger string through a short substring.
	for limit > 0 && limit < len(value) && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return strings.Clone(value[:limit])
}

func (d *configDependencyDiagnostics) compilerComplete(set ConfigDependencySet) {
	d.snapshot.CompletedCompileNodes++
	if set.Opaque {
		d.snapshot.OpaqueCompileNodes++
		d.snapshot.LastOpaqueReason = configDependencyDiagnosticText(set.Reason)
	} else {
		d.snapshot.PreciseCompileNodes++
	}
	d.snapshot.CurrentOutput = ""
	d.publish("compiler_complete")
}

func (d *configDependencyDiagnostics) end() {
	// This also runs after an error or panic. Never manufacture completion for
	// an unfinished compiler node; the event lets observers restore labels.
	d.snapshot.CurrentOutput = ""
	d.publish("analysis_end")
}
