package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Inventories are only bounded hints for real compiler queries. Discovering a
// name here never proves it defined or absent, and missing/unreadable files do
// not supply negative facts. Immutable source-root bindings make this list
// independent of configuration evaluation and identical in discovery/replay.
type configDependencyGuardInventory struct {
	entries map[string][]string
}

const (
	configDependencyGuardInventoryMaximumFiles = 1 << 18
	configDependencyGuardInventoryMaximumNames = 1 << 16
	configDependencyGuardInventoryPrefixBytes  = 8192
)

func (inventory *configDependencyGuardInventory) names(profile CompactKbuildProfile) []string {
	if inventory == nil || profile.evaluator == nil || profile.evaluator.template == nil {
		return nil
	}
	key := configDependencySourceIncludeCacheKeyForProfile(profile, "").bindings
	if inventory.entries == nil {
		inventory.entries = map[string][]string{}
	}
	if names, found := inventory.entries[key]; found {
		return names
	}
	roots := map[string]bool{}
	for _, binding := range compactKbuildProfileSourceRootBindings(profile.evaluator.template.sourceRoots) {
		if !binding.ambiguous && strings.TrimSpace(binding.physical) != "" {
			roots[filepath.Clean(binding.physical)] = true
		}
	}
	names := map[string]bool{}
	files := 0
	// Each hint comes from the same bounded prefix. Reuse its storage across
	// the sequential walk instead of growing a fresh ReadAll buffer per file.
	// Retained names are cloned below, so no cached hint aliases this buffer.
	prefix := make([]byte, configDependencyGuardInventoryPrefixBytes)
	for _, root := range slices.Sorted(maps.Keys(roots)) {
		// Root paths may themselves be repository symlinks. Nested symlinks are
		// skipped: no outside filesystem traversal is needed for an optional hint.
		physical, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(physical, func(filename string, entry fs.DirEntry, walkErr error) error {
			if files >= configDependencyGuardInventoryMaximumFiles || len(names) >= configDependencyGuardInventoryMaximumNames {
				return fs.SkipAll
			}
			if walkErr != nil || entry == nil || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			files++
			file, err := os.Open(filename)
			if err != nil {
				return nil
			}
			size, err := io.ReadFull(file, prefix)
			_ = file.Close()
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return nil
			}
			text, reason := configDependencyPreprocessorText(prefix[:size])
			if reason != "" {
				return nil
			}
			for _, original := range strings.Split(text, "\n") {
				if len(names) >= configDependencyGuardInventoryMaximumNames {
					break
				}
				line := strings.TrimSpace(original)
				if strings.HasPrefix(line, "%:") {
					line = "#" + line[2:]
				}
				if !strings.HasPrefix(line, "#") {
					continue
				}
				fields := strings.Fields(strings.TrimSpace(line[1:]))
				if len(fields) != 2 || fields[0] != "ifndef" && fields[0] != "ifdef" {
					continue
				}
				name, valid := configDependencyMacroIdentifier(fields[1])
				// Ordinary absent names already have a definedness answer. The
				// reserved namespace is intentionally Unknown after a plain -dM.
				if valid && name == fields[1] && strings.HasPrefix(name, "_") && !names[name] {
					// Identifier substrings must not retain whole file prefixes.
					names[strings.Clone(name)] = true
				}
			}
			return nil
		})
	}
	result := slices.Sorted(maps.Keys(names))
	inventory.entries[key] = result
	return result
}

func configDependencyGuardNamesForPlan(plan *ActionPlan) []string {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerDefinedness == nil {
		return nil
	}
	metadata := plan.metadata
	if metadata.sourceGuardNamesReady {
		return metadata.sourceGuardNames
	}
	if metadata.sourceGuardInventory == nil {
		metadata.sourceGuardInventory = &configDependencyGuardInventory{}
	}
	// Use all source-root inventories, not the selected closure or rendered
	// compiler flags. Discovery must register the same requests as final replay.
	names := map[string]bool{}
	seen := map[string]bool{}
	for _, profile := range metadata.Config.KbuildProfiles {
		key := configDependencySourceIncludeCacheKeyForProfile(profile, "").bindings
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, name := range metadata.sourceGuardInventory.names(profile) {
			names[name] = true
		}
	}
	result := slices.Sorted(maps.Keys(names))
	metadata.sourceGuardNames = result[:min(len(result), configDependencyGuardInventoryMaximumNames)]
	metadata.sourceGuardNamesReady = true
	return metadata.sourceGuardNames
}

func configDependencyCompilerDefinednessIdentity(names []string, definitions map[string]bool, ready bool) string {
	var encoded strings.Builder
	appendConfigDependencyCacheString(&encoded, "compiler-definedness-v1")
	if ready {
		encoded.WriteByte('1')
	} else {
		encoded.WriteByte('0')
	}
	for _, name := range names {
		appendConfigDependencyCacheString(&encoded, name)
		value, found := definitions[name]
		switch {
		case !found:
			encoded.WriteByte('?')
		case value:
			encoded.WriteByte('1')
		default:
			encoded.WriteByte('0')
		}
	}
	digest := sha256.Sum256([]byte(encoded.String()))
	return hex.EncodeToString(digest[:])
}

func configDependencyCompilerPredefineParseKey(contents, guardIdentity string) string {
	var encoded strings.Builder
	appendConfigDependencyCacheString(&encoded, contents)
	appendConfigDependencyCacheString(&encoded, guardIdentity)
	return encoded.String()
}

// Register all bounded chunks even when discovery has not produced results.
// Returned maps are private immutable copies, never caller-owned callback maps.
func actionPlanCompilerDefinedness(
	plan *ActionPlan,
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (map[string]bool, string, bool, error) {
	if plan.metadata.compilerDefinedness == nil {
		return nil, "", true, nil
	}
	names := configDependencyGuardNamesForPlan(plan)
	definitions := map[string]bool{}
	ready := true
	for start := 0; start < len(names); {
		end, bytes := start, 0
		for end < len(names) && len(names[end])+31 <= MaxProbeInterpolatedBytes-bytes {
			bytes += len(names[end]) + 31
			end++
		}
		if end == start {
			// A source hint too large for the query is not an absence proof.
			start++
			continue
		}
		chunk := slices.Clone(names[start:end])
		values, available, err := plan.metadata.compilerDefinedness(
			scope, role, language, arguments, translationUnits, chunk, environment,
		)
		if err != nil {
			var unsupported *compilerPredefineProjectionUnsupportedError
			if !errors.As(err, &unsupported) {
				return nil, "", false, err
			}
			// Unsupported optional queries retain Unknown instead of making
			// their requested guard names implicitly absent.
		} else if !available {
			ready = false
		} else {
			if len(values) != len(chunk) {
				return nil, "", false, fmt.Errorf("compiler definedness result has %d names, want %d", len(values), len(chunk))
			}
			for _, name := range names[start:end] {
				value, found := values[name]
				if !found {
					return nil, "", false, fmt.Errorf("compiler definedness result omits %q", name)
				}
				definitions[name] = value
			}
		}
		start = end
	}
	return definitions, configDependencyCompilerDefinednessIdentity(names, definitions, ready), ready, nil
}

func configDependencyApplyCompilerDefinedness(
	state configDependencyMacroState,
	definitions map[string]bool,
	identity string,
) (configDependencyMacroState, bool) {
	if identity == "" {
		return state, true
	}
	var changes [3][]configDependencyMacroSnapshotEntry
	for name, defined := range definitions {
		identifier, valid := configDependencyMacroIdentifier(name)
		if !valid || identifier != name || state.snapshot == nil {
			return configDependencyMacroState{}, false
		}
		kind := configDependencyMacroSnapshotNamespaceForName(name)
		// The measured names are unique. Validate against the original state,
		// without allocating a lookup memo and persistent snapshot per answer.
		previous := state.snapshot.namespace(kind).lookup(name)
		if previous.definition == configDependencyMacroDefined && !defined {
			return configDependencyMacroState{}, false
		}
		cell := configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined}
		if defined {
			cell.definition = configDependencyMacroDefined
			cell.recorded = true
		}
		// A measured absence is not an explicit #undef: the synthetic
		// autoconf guard's recorded bit must remain false for negative facts.
		cell = configDependencyMacroStateCanonicalCell(name, cell)
		if previous != cell {
			changes[kind] = append(changes[kind], configDependencyMacroSnapshotEntry{name: name, cell: cell})
		}
	}
	if len(changes[0])+len(changes[1])+len(changes[2]) != 0 {
		// Validation only consulted the original state. Canonicalize the
		// effective updates now, without collecting or sorting no-op names.
		for _, entries := range changes {
			slices.SortFunc(entries, func(left, right configDependencyMacroSnapshotEntry) int {
				return strings.Compare(left.name, right.name)
			})
		}
		// All answers have passed validation. Publish one immutable snapshot;
		// namespaces with no changes retain their original persistent roots.
		// Never copy the snapshot's sync.Map, sync.Once or cached projections.
		state.snapshot = &configDependencyMacroSnapshot{
			ordinary: state.snapshot.ordinary.withSortedCells(changes[configDependencyMacroSnapshotOrdinaryNamespace]),
			config:   state.snapshot.config.withSortedCells(changes[configDependencyMacroSnapshotConfigNamespace]),
			reserved: state.snapshot.reserved.withSortedCells(changes[configDependencyMacroSnapshotReservedNamespace]),
		}
	}
	digest := sha256.New()
	digest.Write(state.compilerPredefinedDigest[:])
	digest.Write([]byte(identity))
	copy(state.compilerPredefinedDigest[:], digest.Sum(nil))
	state.compilerPredefinedSnapshot = state.snapshot
	return state, true
}

// A completed compiler result may depend on measured negative initial facts.
// Lookup only a request already registered for this exact normalized invocation;
// a missing witness disables this optional cache instead of trusting a sibling.
func configDependencyCompilerDefinednessWitness(
	plan *ActionPlan,
	node ActionPlanNode,
	invocation configDependencyCompilerInvocation,
	context *configDependencyAnalysisContext,
) (string, bool) {
	if plan.metadata.compilerDefinedness == nil && plan.metadata.compilerGuardAnswers == nil {
		return "", true
	}
	sources, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		return "", false
	}
	// Match registration and the scanner: a source-bound forced header is
	// consumed by the ordered include pass, not an additional translation unit.
	// Inspect concrete operands before swapping in the preserved projection.
	sources = configDependencyCompilerPredefineSourcePaths(invocation, sources)
	if invocation.hasPredefineProjection {
		invocation.arguments = invocation.predefineArguments
		invocation.kbuildStart = invocation.predefineKbuildStart
		invocation.kbuildEnd = invocation.predefineKbuildEnd
		invocation.probeEnvironment = invocation.predefineProbeEnvironment
	}
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(invocation, sources)
	if reason != "" {
		return "", false
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	result, found := context.compilerPredefineRequests[key]
	return result.guardIdentity, found && result.ready && result.guardIdentity != ""
}
