package kconfig

// This file contains the configuration-dependency contract used by
// cross-configuration action-plan reduction.  It deliberately does not alter
// ActionPlanNode: callers may build the annotations while node IDs are still
// provisional and re-key them after transitive content addressing.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// ConfigDependencySet is the complete planner-side statement about one
// action's direct Kconfig input. Symbols use their CONFIG_* spelling.
// SourcePaths and ObjectPaths are the exact logical preprocessing closure in
// the immutable source and writable object trees respectively; they let the
// family reducer discard evaluation-wide staged prerequisites which the
// compiler cannot read. Opaque means that neither a symbol projection nor
// input-closure pruning is sound and the action must receive the complete
// resolved Kconfig projection set.
type ConfigDependencySet struct {
	Symbols     []string
	SourcePaths []string
	ObjectPaths []string
	Opaque      bool
	Reason      string

	// Scanner-local scheduling hint, not dependency authority or serialized
	// identity. Canonicalization deliberately discards it after refinement.
	refineMacroCalls bool
}

// CanonicalConfigDependencySet validates, sorts, and de-duplicates one set.
func CanonicalConfigDependencySet(set ConfigDependencySet) (ConfigDependencySet, error) {
	out := ConfigDependencySet{Opaque: set.Opaque, Reason: strings.TrimSpace(set.Reason)}
	if out.Opaque && out.Reason == "" {
		return ConfigDependencySet{}, fmt.Errorf("opaque config dependency set requires a reason")
	}
	for _, symbol := range set.Symbols {
		symbol = normalizeConfigDependencySymbol(symbol)
		if !isConfigKey(symbol) {
			return ConfigDependencySet{}, fmt.Errorf("invalid config dependency symbol %q", symbol)
		}
		out.Symbols = append(out.Symbols, symbol)
	}
	sort.Strings(out.Symbols)
	out.Symbols = slices.Compact(out.Symbols)
	canonicalPaths := func(kind string, paths []string) ([]string, error) {
		if len(paths) == 0 {
			return nil, nil
		}
		out := make([]string, 0, len(paths))
		for _, pathname := range paths {
			if err := validatePlanRelativePath(kind, pathname); err != nil {
				return nil, err
			}
			out = append(out, pathname)
		}
		sort.Strings(out)
		return slices.Compact(out), nil
	}
	var err error
	out.SourcePaths, err = canonicalPaths("config dependency source", set.SourcePaths)
	if err != nil {
		return ConfigDependencySet{}, err
	}
	out.ObjectPaths, err = canonicalPaths("config dependency object", set.ObjectPaths)
	if err != nil {
		return ConfigDependencySet{}, err
	}
	return out, nil
}

// UnionConfigDependencySets forms the symmetric dependency set used when two
// independently planned configurations are compared. An opaque member keeps
// the union opaque; reasons are canonicalized so iteration order cannot affect
// an eventual capsule identity.
func UnionConfigDependencySets(sets ...ConfigDependencySet) (ConfigDependencySet, error) {
	symbols := []string{}
	sourcePaths := []string{}
	objectPaths := []string{}
	reasons := []string{}
	opaque := false
	for _, set := range sets {
		canonical, err := CanonicalConfigDependencySet(set)
		if err != nil {
			return ConfigDependencySet{}, err
		}
		symbols = append(symbols, canonical.Symbols...)
		sourcePaths = append(sourcePaths, canonical.SourcePaths...)
		objectPaths = append(objectPaths, canonical.ObjectPaths...)
		if canonical.Opaque {
			opaque = true
			reasons = append(reasons, canonical.Reason)
		}
	}
	sort.Strings(symbols)
	symbols = slices.Compact(symbols)
	sort.Strings(reasons)
	reasons = slices.Compact(reasons)
	sort.Strings(sourcePaths)
	sourcePaths = slices.Compact(sourcePaths)
	sort.Strings(objectPaths)
	objectPaths = slices.Compact(objectPaths)
	return ConfigDependencySet{
		Symbols:     symbols,
		SourcePaths: sourcePaths,
		ObjectPaths: objectPaths,
		Opaque:      opaque,
		Reason:      strings.Join(reasons, "; "),
	}, nil
}

func opaqueConfigDependency(reason string) ConfigDependencySet {
	return ConfigDependencySet{Opaque: true, Reason: reason}
}

func normalizeConfigDependencySymbol(symbol string) string {
	if tail, ok := strings.CutPrefix(symbol, "CONFIG_"); ok && strings.HasSuffix(tail, "_MODULE") {
		tail = strings.TrimSuffix(tail, "_MODULE")
		symbol = "CONFIG_" + tail
	}
	return symbol
}

func configDependencyIdentifierByte(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' || char == '_'
}

func configDependencyRawStringPrefix(contents []byte, index int) bool {
	if index < 0 || index >= len(contents) ||
		index > 0 && configDependencyIdentifierByte(contents[index-1]) {
		return false
	}
	for _, prefix := range []string{`R"`, `u8R"`, `uR"`, `UR"`, `LR"`} {
		if bytes.HasPrefix(contents[index:], []byte(prefix)) {
			return true
		}
	}
	return false
}

// extractConfigDependencySymbolsInto implements scripts/basic/fixdep.c's
// CONFIG_ token recognition over one byte representation of a source file.
func extractConfigDependencySymbolsInto(contents []byte, seen map[string]bool) {
	const prefix = "CONFIG_"
	for offset := 0; offset+len(prefix) <= len(contents); {
		relative := bytes.Index(contents[offset:], []byte(prefix))
		if relative < 0 {
			break
		}
		start := offset + relative
		if start != 0 && configDependencyIdentifierByte(contents[start-1]) {
			offset = start + len(prefix)
			continue
		}
		end := start + len(prefix)
		for end < len(contents) && configDependencyIdentifierByte(contents[end]) {
			end++
		}
		name := string(contents[start:end])
		name = normalizeConfigDependencySymbol(name)
		if isConfigKey(name) {
			seen[name] = true
		}
		offset = end
	}
}

// ExtractConfigDependencySymbols retains fixdep's conservative raw-byte
// recognition (including comments and inactive branches), and also scans the
// phase-2 line-spliced representation seen by the C preprocessor. The latter is
// required for legal identifiers such as CONFI\<newline>G_FOO.
func ExtractConfigDependencySymbols(contents []byte) []string {
	seen := map[string]bool{}
	extractConfigDependencySymbolsInto(contents, seen)
	spliced := bytes.ReplaceAll(contents, []byte("\\\r\n"), nil)
	spliced = bytes.ReplaceAll(spliced, []byte("\\\n"), nil)
	if !bytes.Equal(spliced, contents) {
		extractConfigDependencySymbolsInto(spliced, seen)
	}
	symbols := make([]string, 0, len(seen))
	for symbol := range seen {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

type configDependencyLiteralInclude struct {
	name   string
	quoted bool
}

type configDependencyMacroDefinition int8

const (
	configDependencyMacroUnknown   configDependencyMacroDefinition = -1
	configDependencyMacroUndefined configDependencyMacroDefinition = iota
	configDependencyMacroDefined
)

const configDependencyResolvedAutoconfGuard = "__GENERATED_AUTOCONF_H__"

// recorded is observable only for the synthetic autoconf guard: it tells the
// generated-header model whether absence still needs to be treated as an
// explicit undef. Retaining write provenance for every ordinary include guard
// creates semantically inert Unknown/recorded facts at successive possible
// includes, making sparse branch support grow with include depth. Keep the
// six-state snapshot primitive exact, but canonicalize production cells at the
// state boundary.
func configDependencyMacroStateCanonicalCell(
	name string,
	cell configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotCell {
	if name != configDependencyResolvedAutoconfGuard {
		cell.recorded = false
	}
	return cell
}

func configDependencyMacroStateMaybeDefineCell(
	name string,
	cell configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotCell {
	return configDependencyMacroStateCanonicalCell(
		name,
		configDependencyMacroSnapshotMaybeDefineCell(cell),
	)
}

func configDependencyMacroStateMaybeDefineTransform() configDependencyMacroSnapshotTransform {
	return configDependencyMacroSnapshotTransform{0, 0, 0, 0, 4, 4}
}

func configDependencyMacroStateWithValidatedNumericMacroHeader(
	snapshot *configDependencyMacroSnapshot,
) (*configDependencyMacroSnapshot, bool) {
	if snapshot == nil {
		return nil, false
	}
	guard, valid := snapshot.lookup(configDependencyResolvedAutoconfGuard)
	if !valid {
		return nil, false
	}
	transform := configDependencyMacroStateMaybeDefineTransform()
	ordinary := snapshot.ordinary.transformAll(transform)
	reserved := snapshot.reserved.transformAll(transform)
	reserved = reserved.withCell(
		configDependencyResolvedAutoconfGuard,
		configDependencyMacroSnapshotMaybeDefineCell(guard),
	)
	if ordinary == snapshot.ordinary && reserved == snapshot.reserved {
		return snapshot, true
	}
	return &configDependencyMacroSnapshot{
		config:   snapshot.config,
		ordinary: ordinary,
		reserved: reserved,
	}, true
}

func configDependencyMacroStateWithConfigNameSet(
	snapshot *configDependencyMacroSnapshot,
	names []string,
	maybe bool,
) (*configDependencyMacroSnapshot, bool) {
	if snapshot == nil {
		return nil, false
	}
	result := snapshot
	for _, name := range names {
		if name == "" || !strings.HasPrefix(name, "CONFIG_") {
			return nil, false
		}
		cell := configDependencyMacroStateCanonicalCell(name, configDependencyMacroSnapshotCell{
			definition: configDependencyMacroDefined,
			recorded:   true,
		})
		if maybe {
			current, valid := result.lookup(name)
			if !valid {
				return nil, false
			}
			cell = configDependencyMacroStateMaybeDefineCell(name, current)
		}
		var valid bool
		result, valid = result.withCell(name, cell)
		if !valid {
			return nil, false
		}
	}
	return result, true
}

type configDependencyMacroState struct {
	// snapshot is the complete immutable semantic macro namespace at a production
	// root, or the materialized parent namespace captured when a speculative
	// descendant was forked. A descendant's normalized overlay is consequently
	// relative to this pointer, not to a recursively evaluated parent chain.
	snapshot *configDependencyMacroSnapshot
	// snapshotChanges is the complete normalized transform relative to snapshot
	// for one speculative state. Exact final cells stay inline for ordinary
	// include guards. A numeric generated header is the idempotent namespace-wide
	// maybe-define transform plus exact overrides written after it.
	snapshotChanges configDependencyMacroSnapshotChangeSet
	// materializedSnapshotCache is an immutable checkpoint. Writes after it
	// update only materializedSnapshotPending; materializedSnapshot flushes that
	// suffix before publishing the complete current namespace to a child.
	// Keeping the checkpoint avoids rebuilding the original cumulative overlay
	// between sequential sibling branches.
	materializedSnapshotCache *configDependencyMacroSnapshot
	// Pending exact cells are last writes relative to the checkpoint, not a
	// normalized overlay: a write equal to the original fork may still have to
	// undo a different checkpoint cell. These maps never alias snapshotChanges.
	materializedSnapshotPending configDependencyMacroSnapshotChangeSet
	// snapshotRevision changes on every semantic mutation. A child records its
	// parent's revision at fork so a parent mutation while that child is live is
	// rejected rather than interpreted against a different entry state.
	snapshotRevision     uint64
	snapshotForkRevision uint64
	symbols              map[string]bool
	macroExpansions      []configDependencyMacroExpansionDefinition
	// macroReplacements is opt-in exact replacement state for complete call
	// coverage. The ordinary definedness scanner leaves it nil. Each speculative
	// branch owns its map; replacement records themselves are immutable values.
	macroReplacements map[string]configDependencyMacroReplacement
	// compilerPredefinedDigest is the canonical defined-name projection of a
	// successfully parsed compiler -dM result. compilerPredefinedSnapshot pins it
	// to that exact immutable root: branches and later mutations must not use the
	// projection as evidence that two complete input snapshots are equal.
	compilerPredefinedDigest   [sha256.Size]byte
	compilerPredefinedSnapshot *configDependencyMacroSnapshot
	// forcedHeaderTrace is non-nil only while one cache miss interprets the
	// command-line forced prefix. Descendants share its input-read recorder but
	// retain an independent touched-name set until they are committed or joined.
	forcedHeaderTrace   *configDependencyForcedHeaderTrace
	forcedHeaderTouches configDependencyMacroSnapshotNameSet
	tainted             bool
	// consumed marks a speculative child whose state has been committed or
	// joined into its parent. The parent never retains the child after snapshot
	// materialization, so subsequent use is invalid rather than a mutation of a
	// shared historical delta.
	consumed bool
	// parent is set only on a short-lived speculative preprocessing branch. It is
	// retained for ancestry/revision validation and child consumption; semantic
	// reads use the immutable fork snapshot above directly.
	parent *configDependencyMacroState
}

func (s *configDependencyMacroState) validSnapshotLineage() bool {
	if s == nil {
		return false
	}
	for current := s; current != nil; current = current.parent {
		if current.tainted || current.consumed || current.snapshot == nil {
			return false
		}
		if current.parent != nil &&
			current.snapshotForkRevision != current.parent.snapshotRevision {
			return false
		}
	}
	return true
}

func (s *configDependencyMacroState) rejectInvalidMutation() bool {
	if s == nil {
		return true
	}
	if s.tainted || s.consumed || s.snapshot == nil {
		s.tainted = true
		return true
	}
	return false
}

type configDependencyMacroSnapshotNameSet struct {
	first      string
	additional map[string]struct{}
}

// configDependencyForcedHeaderTrace records the finite part of a forced
// prefix's macro transform. Reads are names whose entry cells can affect the
// interpreted path or a joined output. The validated numeric-header effect is
// kept as one pointwise wildcard instead of expanding the non-CONFIG namespace.
// A recursive whole-state projection deliberately disables capture: proving a
// sparse dependency for that operation would require retaining the full input.
type configDependencyForcedHeaderTrace struct {
	entrySnapshot     *configDependencyMacroSnapshot
	reads             configDependencyMacroSnapshotNameSet
	nonConfigWildcard bool
	wholeNamespace    bool
	invalid           bool
}

func (s *configDependencyMacroState) beginForcedHeaderTrace() bool {
	if s == nil || s.macroReplacements != nil || s.forcedHeaderTrace != nil || s.forcedHeaderTouches.size() != 0 {
		return false
	}
	entry, valid := s.materializedSnapshot()
	if !valid {
		return false
	}
	s.forcedHeaderTrace = &configDependencyForcedHeaderTrace{entrySnapshot: entry}
	return true
}

func (s *configDependencyMacroState) endForcedHeaderTrace() {
	if s == nil {
		return
	}
	s.forcedHeaderTrace = nil
	s.forcedHeaderTouches = configDependencyMacroSnapshotNameSet{}
}

func (s *configDependencyMacroState) recordForcedHeaderRead(name string) {
	if s == nil || name == "" || s.forcedHeaderTrace == nil {
		return
	}
	s.forcedHeaderTrace.reads.put(name)
}

func (s *configDependencyMacroState) recordForcedHeaderTouch(name string) {
	if s == nil || name == "" || s.forcedHeaderTrace == nil {
		return
	}
	s.forcedHeaderTouches.put(name)
}

func (s *configDependencyMacroState) mergeForcedHeaderTouches(
	branch *configDependencyMacroState,
	possible bool,
) {
	if s == nil || s.forcedHeaderTrace == nil || branch == nil {
		return
	}
	if branch.forcedHeaderTrace != s.forcedHeaderTrace {
		s.forcedHeaderTrace.invalid = true
		return
	}
	if possible {
		branch.forcedHeaderTouches.forEach(s.recordForcedHeaderRead)
	}
	s.forcedHeaderTouches = configDependencyMacroSnapshotUnionNameSets(
		s.forcedHeaderTouches,
		branch.forcedHeaderTouches,
	)
}

func (names *configDependencyMacroSnapshotNameSet) put(name string) {
	if names == nil || name == "" {
		return
	}
	if names.first == "" || names.first == name {
		names.first = name
		return
	}
	if names.additional == nil {
		names.additional = map[string]struct{}{}
	}
	names.additional[name] = struct{}{}
}

func (names configDependencyMacroSnapshotNameSet) contains(name string) bool {
	if names.first == name && name != "" {
		return true
	}
	_, found := names.additional[name]
	return found
}

func (names *configDependencyMacroSnapshotNameSet) delete(name string) {
	if names == nil {
		return
	}
	if names.first == name {
		names.first = ""
		for replacement := range names.additional {
			names.first = replacement
			delete(names.additional, replacement)
			break
		}
		return
	}
	delete(names.additional, name)
}

func (names configDependencyMacroSnapshotNameSet) size() int {
	size := len(names.additional)
	if names.first != "" {
		size++
	}
	return size
}

func (names configDependencyMacroSnapshotNameSet) forEach(visit func(string)) {
	if names.first != "" {
		visit(names.first)
	}
	for name := range names.additional {
		visit(name)
	}
}

func configDependencyMacroSnapshotUnionNameSets(
	left, right configDependencyMacroSnapshotNameSet,
) configDependencyMacroSnapshotNameSet {
	if left.size() < right.size() {
		left, right = right, left
	}
	result := left
	right.forEach(func(name string) {
		result.put(name)
	})
	return result
}

type configDependencyMacroSnapshotChangeSet struct {
	// Canonical Unknown/recorded=false cells are absorbing under another
	// possible-branch join. Keep their names separate so a consumed branch's
	// potentially large support can be adopted without walking it.
	unknownNames      configDependencyMacroSnapshotNameSet
	firstName         string
	firstCell         configDependencyMacroSnapshotCell
	additional        map[string]configDependencyMacroSnapshotCell
	nonConfigWildcard bool
}

func configDependencyMacroSnapshotCanonicalUnknownCell() configDependencyMacroSnapshotCell {
	return configDependencyMacroSnapshotCell{
		definition: configDependencyMacroUnknown,
		recorded:   false,
	}
}

func configDependencyMacroSnapshotIsCanonicalUnknownCell(
	cell configDependencyMacroSnapshotCell,
) bool {
	return cell == configDependencyMacroSnapshotCanonicalUnknownCell()
}

func (changes *configDependencyMacroSnapshotChangeSet) deleteConcrete(name string) {
	if changes.firstName == name {
		changes.firstName = ""
		changes.firstCell = configDependencyMacroSnapshotCell{}
		for replacement, cell := range changes.additional {
			changes.firstName = replacement
			changes.firstCell = cell
			delete(changes.additional, replacement)
			break
		}
		return
	}
	delete(changes.additional, name)
}

func (changes *configDependencyMacroSnapshotChangeSet) put(
	name string,
	cell configDependencyMacroSnapshotCell,
) {
	if changes == nil || name == "" {
		return
	}
	if configDependencyMacroSnapshotIsCanonicalUnknownCell(cell) {
		changes.deleteConcrete(name)
		changes.unknownNames.put(name)
		return
	}
	changes.unknownNames.delete(name)
	if changes.firstName == "" || changes.firstName == name {
		changes.firstName = name
		changes.firstCell = cell
		return
	}
	if changes.additional == nil {
		changes.additional = map[string]configDependencyMacroSnapshotCell{}
	}
	changes.additional[name] = cell
}

func (changes configDependencyMacroSnapshotChangeSet) lookup(
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	if changes.firstName == name && name != "" {
		return changes.firstCell, true
	}
	if cell, found := changes.additional[name]; found {
		return cell, true
	}
	if changes.unknownNames.contains(name) {
		return configDependencyMacroSnapshotCanonicalUnknownCell(), true
	}
	return configDependencyMacroSnapshotCell{}, false
}

func (changes *configDependencyMacroSnapshotChangeSet) delete(name string) {
	if changes == nil {
		return
	}
	changes.deleteConcrete(name)
	changes.unknownNames.delete(name)
}

func (changes configDependencyMacroSnapshotChangeSet) empty() bool {
	return changes.firstName == "" && len(changes.additional) == 0 &&
		changes.unknownNames.size() == 0 && !changes.nonConfigWildcard
}

func (changes configDependencyMacroSnapshotChangeSet) exactSize() int {
	size := len(changes.additional)
	if changes.firstName != "" {
		size++
	}
	return size + changes.unknownNames.size()
}

func (changes configDependencyMacroSnapshotChangeSet) forEachConcrete(
	visit func(string, configDependencyMacroSnapshotCell),
) {
	if changes.firstName != "" {
		visit(changes.firstName, changes.firstCell)
	}
	for name, cell := range changes.additional {
		visit(name, cell)
	}
}

func (changes configDependencyMacroSnapshotChangeSet) forEach(
	visit func(string, configDependencyMacroSnapshotCell),
) {
	changes.forEachConcrete(visit)
	unknown := configDependencyMacroSnapshotCanonicalUnknownCell()
	changes.unknownNames.forEach(func(name string) {
		visit(name, unknown)
	})
}

func (changes *configDependencyMacroSnapshotChangeSet) applyValidatedNumericMacroHeader() {
	if changes == nil {
		return
	}
	updates := []struct {
		name string
		cell configDependencyMacroSnapshotCell
	}{}
	changes.forEachConcrete(func(name string, cell configDependencyMacroSnapshotCell) {
		if !strings.HasPrefix(name, "CONFIG_") {
			updates = append(updates, struct {
				name string
				cell configDependencyMacroSnapshotCell
			}{name: name, cell: configDependencyMacroStateMaybeDefineCell(name, cell)})
		}
	})
	for _, update := range updates {
		changes.put(update.name, update.cell)
	}
	changes.nonConfigWildcard = true
}

func (changes *configDependencyMacroSnapshotChangeSet) applyValidatedNumericMacroHeaderNormalized(
	base *configDependencyMacroSnapshot,
) bool {
	if changes == nil || base == nil {
		return false
	}
	changes.applyValidatedNumericMacroHeader()
	remove := []string{}
	valid := true
	changes.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		if !valid || strings.HasPrefix(name, "CONFIG_") {
			return
		}
		inherited, found := base.lookup(name)
		if !found {
			valid = false
			return
		}
		inherited = configDependencyMacroStateMaybeDefineCell(name, inherited)
		if cell == inherited {
			remove = append(remove, name)
		}
	})
	if !valid {
		return false
	}
	for _, name := range remove {
		changes.delete(name)
	}
	return true
}

// composeExactAfter composes a wildcard-free exact transform after changes.
// The larger consumed support becomes the result and only the smaller support
// is visited. Delta cells win on overlap and are renormalized against base.
func (changes *configDependencyMacroSnapshotChangeSet) composeExactAfter(
	base *configDependencyMacroSnapshot,
	delta configDependencyMacroSnapshotChangeSet,
) bool {
	if changes == nil || base == nil || changes.nonConfigWildcard || delta.nonConfigWildcard {
		return false
	}
	if changes.exactSize() >= delta.exactSize() {
		valid := true
		delta.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
			if valid {
				valid = changes.putNormalized(base, name, cell)
			}
		})
		return valid
	}

	result := delta
	valid := true
	changes.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		if !valid {
			return
		}
		if final, found := delta.lookup(name); found {
			valid = result.putNormalized(base, name, final)
			return
		}
		result.put(name, cell)
	})
	if !valid {
		return false
	}
	*changes = result
	return true
}

func (changes configDependencyMacroSnapshotChangeSet) cell(
	base *configDependencyMacroSnapshot,
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	if base == nil {
		return configDependencyMacroSnapshotCell{}, false
	}
	cell, valid := base.lookup(name)
	if !valid {
		return configDependencyMacroSnapshotCell{}, false
	}
	if changes.nonConfigWildcard && !strings.HasPrefix(name, "CONFIG_") {
		cell = configDependencyMacroStateMaybeDefineCell(name, cell)
	}
	if exact, found := changes.lookup(name); found {
		cell = exact
	}
	return cell, true
}

func (changes *configDependencyMacroSnapshotChangeSet) putNormalized(
	base *configDependencyMacroSnapshot,
	name string,
	cell configDependencyMacroSnapshotCell,
) bool {
	if changes == nil || base == nil || name == "" {
		return false
	}
	if _, valid := configDependencyMacroSnapshotCellIndex(cell); !valid {
		return false
	}
	inherited, valid := base.lookup(name)
	if !valid {
		return false
	}
	if changes.nonConfigWildcard && !strings.HasPrefix(name, "CONFIG_") {
		inherited = configDependencyMacroStateMaybeDefineCell(name, inherited)
	}
	if cell == inherited {
		changes.delete(name)
	} else {
		changes.put(name, cell)
	}
	return true
}

func (changes configDependencyMacroSnapshotChangeSet) materialize(
	base *configDependencyMacroSnapshot,
) (*configDependencyMacroSnapshot, bool) {
	if base == nil {
		return nil, false
	}
	result := base
	if changes.nonConfigWildcard {
		var valid bool
		result, valid = configDependencyMacroStateWithValidatedNumericMacroHeader(result)
		if !valid {
			return nil, false
		}
	}
	valid := true
	changes.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
		if !valid {
			return
		}
		result, valid = result.withCell(name, cell)
	})
	if !valid {
		return nil, false
	}
	return result, true
}

func (s *configDependencyMacroState) snapshotCellUnchecked(
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	if s == nil {
		return configDependencyMacroSnapshotCell{}, false
	}
	return s.snapshotChanges.cell(s.snapshot, name)
}

func (s *configDependencyMacroState) snapshotCell(
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	if s == nil || s.snapshot == nil || s.tainted || s.consumed {
		return configDependencyMacroSnapshotCell{}, false
	}
	return s.snapshotCellUnchecked(name)
}

func (s *configDependencyMacroState) putSnapshotCell(
	name string,
	cell configDependencyMacroSnapshotCell,
) bool {
	if s == nil || s.snapshot == nil || s.tainted || s.consumed || name == "" {
		return false
	}
	if _, valid := configDependencyMacroSnapshotCellIndex(cell); !valid {
		return false
	}
	if s.parent == nil {
		next, valid := s.snapshot.withCell(name, cell)
		if !valid {
			return false
		}
		s.snapshot = next
		s.materializedSnapshotCache = nil
		s.materializedSnapshotPending = configDependencyMacroSnapshotChangeSet{}
	} else {
		if !s.snapshotChanges.putNormalized(s.snapshot, name, cell) {
			return false
		}
		if s.materializedSnapshotCache != nil {
			s.materializedSnapshotPending.put(name, cell)
		}
	}
	s.snapshotRevision++
	delete(s.macroReplacements, name)
	return true
}

func (s *configDependencyMacroState) applySnapshotValidatedNumericMacroHeader() bool {
	if s == nil || s.snapshot == nil || s.tainted || s.consumed {
		return false
	}
	if s.parent == nil {
		next, valid := configDependencyMacroStateWithValidatedNumericMacroHeader(s.snapshot)
		if !valid {
			return false
		}
		s.snapshot = next
		s.materializedSnapshotCache = nil
		s.materializedSnapshotPending = configDependencyMacroSnapshotChangeSet{}
	} else {
		if !s.snapshotChanges.applyValidatedNumericMacroHeaderNormalized(s.snapshot) {
			return false
		}
		if s.materializedSnapshotCache != nil {
			// The wildcard executes after earlier pending writes, while later
			// exact writes override it. Do not normalize against the original
			// fork: this suffix executes against the materialized checkpoint.
			s.materializedSnapshotPending.applyValidatedNumericMacroHeader()
			// Canonical Unknown is absorbing for ordinary names, but the
			// synthetic autoconf guard also records explicit write provenance.
			if s.materializedSnapshotPending.unknownNames.contains(configDependencyResolvedAutoconfGuard) {
				s.materializedSnapshotPending.put(configDependencyResolvedAutoconfGuard,
					configDependencyMacroSnapshotMaybeDefineCell(configDependencyMacroSnapshotCanonicalUnknownCell()))
			}
		}
	}
	s.snapshotRevision++
	return true
}

func (s *configDependencyMacroState) materializedSnapshot() (
	*configDependencyMacroSnapshot,
	bool,
) {
	if s == nil || s.snapshot == nil || s.tainted || s.consumed {
		return nil, false
	}
	if s.parent == nil {
		return s.snapshot, true
	}
	if s.materializedSnapshotCache != nil {
		if !s.materializedSnapshotPending.empty() {
			materialized, valid := s.materializedSnapshotPending.materialize(s.materializedSnapshotCache)
			if !valid {
				return nil, false
			}
			s.materializedSnapshotCache = materialized
			s.materializedSnapshotPending = configDependencyMacroSnapshotChangeSet{}
		}
		return s.materializedSnapshotCache, true
	}
	if s.snapshotChanges.empty() {
		s.materializedSnapshotCache = s.snapshot
		return s.snapshot, true
	}
	materialized, valid := s.snapshotChanges.materialize(s.snapshot)
	if !valid {
		return nil, false
	}
	s.materializedSnapshotCache = materialized
	return materialized, true
}

// applySnapshotChanges composes one normalized transform after the current
// state. The transform's exact cells are final values after its wildcard. A
// speculative state updates only its finite overlay; a production root first
// constructs the complete next snapshot and then swaps one pointer. Either
// path advances the revision once, independently of exact-support cardinality.
func (s *configDependencyMacroState) applySnapshotChanges(
	current *configDependencyMacroSnapshot,
	changes configDependencyMacroSnapshotChangeSet,
) bool {
	return s.applySnapshotChangesWithMaterializedResult(current, changes, nil)
}

// materialized is trusted only after commitBranch has checked that its direct
// child's immutable entry and revision match current and flushed any pending
// suffix from an existing child checkpoint. Reusing that exact result avoids
// writing every changed macro into the persistent tree again during commit.
func (s *configDependencyMacroState) applySnapshotChangesWithMaterializedResult(
	current *configDependencyMacroSnapshot,
	changes configDependencyMacroSnapshotChangeSet,
	materialized *configDependencyMacroSnapshot,
) bool {
	if changes.empty() {
		return true
	}
	if s == nil || s.snapshot == nil || current == nil || s.tainted || s.consumed {
		return false
	}
	// Commit/join callers obtain current through materializedSnapshot. A dirty
	// checkpoint cannot serve as the complete input to this transform.
	if !s.materializedSnapshotPending.empty() {
		return false
	}
	// A speculative result is usually consumed immediately by its enclosing
	// include. Keep it sparse and materialize only if execution subsequently
	// forks or requests a recursive-state key. When current is that cached
	// materialization, retain it by applying only this delta. Sequential sibling
	// joins can then fork from the updated cache instead of rebuilding the
	// growing accumulated overlay. Roots require the complete tree.
	keepMaterialized := s.parent == nil ||
		s.materializedSnapshotCache != nil && current == s.materializedSnapshotCache
	var nextMaterialized *configDependencyMacroSnapshot
	if keepMaterialized {
		nextMaterialized = materialized
		if nextMaterialized == nil {
			var valid bool
			nextMaterialized, valid = changes.materialize(current)
			if !valid {
				return false
			}
		}
	}
	if s.parent == nil {
		if current != s.snapshot {
			return false
		}
		s.snapshot = nextMaterialized
	} else {
		if !changes.nonConfigWildcard && !s.snapshotChanges.nonConfigWildcard {
			if !s.snapshotChanges.composeExactAfter(s.snapshot, changes) {
				return false
			}
		} else {
			if changes.nonConfigWildcard &&
				!s.snapshotChanges.applyValidatedNumericMacroHeaderNormalized(s.snapshot) {
				return false
			}
			valid := true
			changes.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
				if valid {
					valid = s.snapshotChanges.putNormalized(s.snapshot, name, cell)
				}
			})
			if !valid {
				return false
			}
		}
	}
	if s.parent == nil || !keepMaterialized {
		s.materializedSnapshotCache = nil
	} else {
		s.materializedSnapshotCache = nextMaterialized
	}
	s.materializedSnapshotPending = configDependencyMacroSnapshotChangeSet{}
	s.snapshotRevision++
	return true
}

type configDependencyConditionalMacroStateProjection struct {
	key string
	// facts contains only the finite deviations from the projection's
	// implicit default.  When unknownNonConfig is set, an absent non-CONFIG
	// name is Unknown because a validated generated numeric-macro header may
	// have defined it; otherwise ordinary absent names are Undefined.  Reserved
	// absent names remain Unknown in either domain.
	facts            map[string]configDependencyMacroDefinition
	unknownNonConfig bool
}

// configDependencyConditionalMacroStateKey is a collision-free canonical
// serialization of the complete three-valued modeled macro namespace. A
// validated generated numeric-macro header contributes a uniform may-define
// transformer for all non-CONFIG names, represented by unknownNonConfig plus
// finite exact overrides. This keeps the infinite wildcard domain canonical
// and allows later exact include-guard writes to prove recursive progress.
func configDependencyConditionalMacroStateKey(
	state *configDependencyMacroState,
) (configDependencyConditionalMacroStateProjection, bool) {
	if state == nil {
		return configDependencyConditionalMacroStateProjection{}, false
	}
	if state.forcedHeaderTrace != nil {
		state.forcedHeaderTrace.wholeNamespace = true
	}
	snapshot, valid := state.materializedSnapshot()
	if !valid {
		return configDependencyConditionalMacroStateProjection{}, false
	}
	return snapshot.conditionalProjection()
}

func configDependencyConditionalMacroDefault(
	name string,
	unknownNonConfig bool,
) configDependencyMacroDefinition {
	if unknownNonConfig && !strings.HasPrefix(name, "CONFIG_") {
		return configDependencyMacroUnknown
	}
	return configDependencyAbsentMacroDefinition(name)
}

func (p configDependencyConditionalMacroStateProjection) definition(
	name string,
) configDependencyMacroDefinition {
	if definition, ok := p.facts[name]; ok {
		return definition
	}
	return configDependencyConditionalMacroDefault(name, p.unknownNonConfig)
}

// configDependencyConditionalMacroStateProgress requires one new definite
// fact in current relative to the immediately enclosing occurrence. Unknown
// additions and loss of knowledge are not progress. The finite union of
// non-default facts is sufficient: every other name has the uniform default in
// both projections, including the validated-header wildcard domain.
func configDependencyConditionalMacroStateProgress(
	previous, current configDependencyConditionalMacroStateProjection,
) bool {
	names := map[string]bool{}
	for name := range previous.facts {
		names[name] = true
	}
	for name := range current.facts {
		names[name] = true
	}
	for name := range names {
		definition := current.definition(name)
		if definition != configDependencyMacroUnknown && previous.definition(name) != definition {
			return true
		}
	}
	return false
}

// configDependencyMacroExpansionDefinition retains the part of one macro
// definition needed to decide whether token pasting can affect the reachable
// preprocessing stream. Identifiers names replacement-body macro candidates;
// function-like parameters are deliberately excluded because they are supplied
// by the caller rather than resolved in the definition's macro namespace.
type configDependencyMacroExpansionDefinition struct {
	name                   string
	identifiers            []string
	tokenPaste             bool
	stringification        bool
	safeTokenPastePrefixes []string
}

type configDependencyMacroExpansion struct {
	identifiers            map[string]bool
	tokenPaste             bool
	stringification        bool
	safeTokenPastePrefixes map[string]bool
}

// configDependencyMacroExpansionGraph is a conservative union of definitions
// visible to one compiler action. Roots are identifiers in reachable source
// text outside macro-definition bodies. Replacement identifiers form directed
// alias/expansion edges; a token-pasting definition matters only when a root can
// reach its macro name through those edges.
type configDependencyMacroExpansionGraph struct {
	definitions map[string]configDependencyMacroExpansion
	roots       map[string]bool
}

func (g configDependencyMacroExpansionGraph) clone() configDependencyMacroExpansionGraph {
	result := configDependencyMacroExpansionGraph{
		definitions: maps.Clone(g.definitions),
		roots:       maps.Clone(g.roots),
	}
	for name, definition := range result.definitions {
		definition.identifiers = maps.Clone(definition.identifiers)
		definition.safeTokenPastePrefixes = maps.Clone(definition.safeTokenPastePrefixes)
		result.definitions[name] = definition
	}
	return result
}

// configDependencySortedPrefixRange returns the exact half-open range of
// values which begin with prefix. values must already be sorted. Macro names
// are ASCII identifiers, so every matching string is contiguous in Go's byte
// lexical order and the lower-bound search can skip every unrelated name.
func configDependencySortedPrefixRange(values []string, prefix string) (int, int) {
	start := sort.SearchStrings(values, prefix)
	end := start
	for end < len(values) && strings.HasPrefix(values[end], prefix) {
		end++
	}
	return start, end
}

func (g *configDependencyMacroExpansionGraph) addDefinitions(definitions ...configDependencyMacroExpansionDefinition) {
	if g == nil {
		return
	}
	if g.definitions == nil {
		g.definitions = map[string]configDependencyMacroExpansion{}
	}
	for _, definition := range definitions {
		if definition.name == "" {
			continue
		}
		expansion := g.definitions[definition.name]
		if expansion.identifiers == nil {
			expansion.identifiers = map[string]bool{}
		}
		for _, identifier := range definition.identifiers {
			expansion.identifiers[identifier] = true
		}
		expansion.tokenPaste = expansion.tokenPaste || definition.tokenPaste
		expansion.stringification = expansion.stringification || definition.stringification
		if len(definition.safeTokenPastePrefixes) != 0 {
			if expansion.safeTokenPastePrefixes == nil {
				expansion.safeTokenPastePrefixes = map[string]bool{}
			}
			for _, prefix := range definition.safeTokenPastePrefixes {
				expansion.safeTokenPastePrefixes[prefix] = true
			}
		}
		g.definitions[definition.name] = expansion
	}
}

func (g *configDependencyMacroExpansionGraph) addRoots(roots ...string) {
	if g == nil {
		return
	}
	if g.roots == nil {
		g.roots = map[string]bool{}
	}
	for _, root := range roots {
		if root != "" {
			g.roots[root] = true
		}
	}
}

func (g *configDependencyMacroExpansionGraph) reachableTokenPaste() string {
	return g.reachableMacroOperator(false)
}

func (g *configDependencyMacroExpansionGraph) reachableStringification() string {
	return g.reachableMacroOperator(true)
}

func (g *configDependencyMacroExpansionGraph) reachableMacroOperator(stringification bool) string {
	if g == nil || len(g.roots) == 0 || len(g.definitions) == 0 {
		return ""
	}
	queue := slices.Sorted(maps.Keys(g.roots))
	queued := make(map[string]bool, len(queue))
	for _, name := range queue {
		queued[name] = true
	}
	definitionNames := slices.Sorted(maps.Keys(g.definitions))
	seen := map[string]bool{}
	enqueue := func(name string) {
		if !seen[name] && !queued[name] {
			queue = append(queue, name)
			queued[name] = true
		}
	}
	for len(queue) != 0 {
		name := queue[0]
		queue = queue[1:]
		delete(queued, name)
		if seen[name] {
			continue
		}
		seen[name] = true
		expansion, ok := g.definitions[name]
		if !ok {
			continue
		}
		if stringification && expansion.stringification || !stringification && expansion.tokenPaste {
			return name
		}
		for _, identifier := range slices.Sorted(maps.Keys(expansion.identifiers)) {
			enqueue(identifier)
		}
		// A proven-safe paste cannot directly form CONFIG_*, but its result is
		// rescanned and may name another macro. Conservatively connect its fixed
		// literal prefix to every definition it could construct; aliases and an
		// eventual generic/CONFIG paste then remain reachable and opaque.
		for _, prefix := range slices.Sorted(maps.Keys(expansion.safeTokenPastePrefixes)) {
			start, end := configDependencySortedPrefixRange(definitionNames, prefix)
			for _, candidate := range definitionNames[start:end] {
				enqueue(candidate)
			}
		}
	}
	return ""
}

func (g *configDependencyMacroExpansionGraph) reachableIdentifier(want string) bool {
	if g == nil || want == "" || len(g.roots) == 0 {
		return false
	}
	queue := slices.Sorted(maps.Keys(g.roots))
	queued := make(map[string]bool, len(queue))
	for _, name := range queue {
		queued[name] = true
	}
	var definitionNames []string
	for len(queue) != 0 {
		name := queue[0]
		queue = queue[1:]
		if name == want {
			return true
		}
		expansion := g.definitions[name]
		for identifier := range expansion.identifiers {
			if !queued[identifier] {
				queued[identifier] = true
				queue = append(queue, identifier)
			}
		}
		// A CONFIG-safe paste is still rescanned. It can form the wanted
		// builtin directly (which need not have an ordinary macro definition),
		// or an alias whose expansion eventually reaches that builtin.
		for prefix := range expansion.safeTokenPastePrefixes {
			if strings.HasPrefix(want, prefix) {
				return true
			}
			if definitionNames == nil {
				definitionNames = slices.Sorted(maps.Keys(g.definitions))
			}
			start, end := configDependencySortedPrefixRange(definitionNames, prefix)
			for _, candidate := range definitionNames[start:end] {
				if !queued[candidate] {
					queued[candidate] = true
					queue = append(queue, candidate)
				}
			}
		}
	}
	return false
}

func configDependencyMacroIdentifier(value string) (string, bool) {
	if value == "" || !((value[0] >= 'a' && value[0] <= 'z') ||
		(value[0] >= 'A' && value[0] <= 'Z') || value[0] == '_') {
		return "", false
	}
	end := 1
	for end < len(value) && configDependencyIdentifierByte(value[end]) {
		end++
	}
	return value[:end], true
}

// configDependencyMacroLex scans preprocessing tokens which can name another
// macro in a replacement or reachable source stream. Comments, string
// literals, and character literals are not macro expansion sites. It also
// recognizes both spellings of the token-paste preprocessing token.
func configDependencyMacroLex(value string) ([]string, bool) {
	const (
		macroLexNormal = iota
		macroLexBlockComment
		macroLexLineComment
		macroLexString
		macroLexCharacter
	)
	state := macroLexNormal
	identifiers := map[string]bool{}
	tokenPaste := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch state {
		case macroLexNormal:
			switch {
			case strings.HasPrefix(value[index:], "##"):
				tokenPaste = true
				index++
			case strings.HasPrefix(value[index:], "%:%:"):
				tokenPaste = true
				index += len("%:%:") - 1
			case character == '/' && index+1 < len(value) && value[index+1] == '*':
				state = macroLexBlockComment
				index++
			case character == '/' && index+1 < len(value) && value[index+1] == '/':
				state = macroLexLineComment
				index++
			case character == '"':
				state = macroLexString
			case character == '\'':
				state = macroLexCharacter
			default:
				identifier, ok := configDependencyMacroIdentifier(value[index:])
				if ok {
					identifiers[identifier] = true
					index += len(identifier) - 1
				}
			}
		case macroLexBlockComment:
			if character == '*' && index+1 < len(value) && value[index+1] == '/' {
				state = macroLexNormal
				index++
			}
		case macroLexLineComment:
			if character == '\n' {
				state = macroLexNormal
			}
		case macroLexString, macroLexCharacter:
			if character == '\\' && index+1 < len(value) {
				index++
				continue
			}
			if state == macroLexString && character == '"' ||
				state == macroLexCharacter && character == '\'' {
				state = macroLexNormal
			}
		}
	}
	return slices.Sorted(maps.Keys(identifiers)), tokenPaste
}

func configDependencyMacroSignature(value string) (
	name string,
	parameters map[string]bool,
	rest string,
	ok bool,
) {
	name, ok = configDependencyMacroIdentifier(value)
	if !ok {
		return "", nil, "", false
	}
	rest = value[len(name):]
	if !strings.HasPrefix(rest, "(") {
		return name, nil, rest, true
	}
	close := strings.IndexByte(rest, ')')
	if close < 0 {
		return "", nil, "", false
	}
	parameters = map[string]bool{}
	for _, raw := range strings.Split(rest[1:close], ",") {
		parameter := strings.TrimSpace(raw)
		if parameter == "" {
			continue
		}
		if parameter == "..." {
			parameters["__VA_ARGS__"] = true
			continue
		}
		parameter = strings.TrimSpace(strings.TrimSuffix(parameter, "..."))
		identifier, valid := configDependencyMacroIdentifier(parameter)
		if !valid || identifier != parameter {
			return "", nil, "", false
		}
		parameters[identifier] = true
	}
	return name, parameters, rest[close+1:], true
}

type configDependencyMacroPasteToken struct {
	identifier      string
	paste           bool
	stringification bool
}

// configDependencyMacroPasteTokens retains only the preprocessing-token
// structure needed to identify the left edge of each ## chain. Unknown or
// malformed lexical input fails closed in configDependencyMacroTokenPasteRisk.
func configDependencyMacroPasteTokens(value string) ([]configDependencyMacroPasteToken, bool) {
	const (
		macroPasteNormal = iota
		macroPasteBlockComment
		macroPasteLineComment
		macroPasteString
		macroPasteCharacter
	)
	state := macroPasteNormal
	tokens := []configDependencyMacroPasteToken{}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch state {
		case macroPasteNormal:
			switch {
			case character == ' ' || character == '\t' || character == '\r' || character == '\n' || character == '\f' || character == '\v':
				continue
			case strings.HasPrefix(value[index:], "##"):
				tokens = append(tokens, configDependencyMacroPasteToken{paste: true})
				index++
			case strings.HasPrefix(value[index:], "%:%:"):
				tokens = append(tokens, configDependencyMacroPasteToken{paste: true})
				index += len("%:%:") - 1
			case character == '#':
				tokens = append(tokens, configDependencyMacroPasteToken{stringification: true})
			case strings.HasPrefix(value[index:], "%:"):
				tokens = append(tokens, configDependencyMacroPasteToken{stringification: true})
				index++
			case character == '/' && index+1 < len(value) && value[index+1] == '*':
				state = macroPasteBlockComment
				index++
			case character == '/' && index+1 < len(value) && value[index+1] == '/':
				state = macroPasteLineComment
				index++
			case character == '"':
				tokens = append(tokens, configDependencyMacroPasteToken{})
				state = macroPasteString
			case character == '\'':
				tokens = append(tokens, configDependencyMacroPasteToken{})
				state = macroPasteCharacter
			default:
				identifier, ok := configDependencyMacroIdentifier(value[index:])
				if ok {
					tokens = append(tokens, configDependencyMacroPasteToken{identifier: identifier})
					index += len(identifier) - 1
				} else {
					tokens = append(tokens, configDependencyMacroPasteToken{})
				}
			}
		case macroPasteBlockComment:
			if character == '*' && index+1 < len(value) && value[index+1] == '/' {
				state = macroPasteNormal
				index++
			}
		case macroPasteLineComment:
			if character == '\n' {
				state = macroPasteNormal
			}
		case macroPasteString, macroPasteCharacter:
			if character == '\\' && index+1 < len(value) {
				index++
				continue
			}
			if state == macroPasteString && character == '"' ||
				state == macroPasteCharacter && character == '\'' {
				state = macroPasteNormal
			}
		}
	}
	if state == macroPasteLineComment {
		state = macroPasteNormal
	}
	return tokens, state == macroPasteNormal
}

// configDependencyMacroTokenPasteRisk proves a narrow but general safe case:
// every paste chain begins with a literal identifier whose bytes already rule
// out CONFIG_ as both a complete prefix and a prefix still being assembled.
// A parameter at the left edge, a partial CONFIG_ prefix, malformed tokens, or
// any other shape remains unsafe. Safe prefixes are retained because the
// pasted result is rescanned and can dynamically name another known macro.
func configDependencyMacroTokenPasteRisk(
	replacement string,
	parameters map[string]bool,
) (bool, []string) {
	tokens, ok := configDependencyMacroPasteTokens(replacement)
	if !ok {
		return true, nil
	}
	prefixes := map[string]bool{}
	sawPaste := false
	for index, token := range tokens {
		if !token.paste {
			continue
		}
		sawPaste = true
		if index == 0 || index+1 >= len(tokens) || tokens[index-1].paste || tokens[index+1].paste {
			return true, nil
		}
		left := index - 1
		for left >= 2 && tokens[left-1].paste {
			left -= 2
		}
		prefix := tokens[left].identifier
		if prefix == "" || parameters[prefix] ||
			strings.HasPrefix("CONFIG_", prefix) || strings.HasPrefix(prefix, "CONFIG_") {
			return true, nil
		}
		prefixes[prefix] = true
	}
	if !sawPaste {
		return false, nil
	}
	return false, slices.Sorted(maps.Keys(prefixes))
}

func configDependencyMacroExpansionDefinitionForReplacement(
	name string,
	parameters map[string]bool,
	replacement string,
) configDependencyMacroExpansionDefinition {
	identifiers, hasTokenPaste := configDependencyMacroLex(replacement)
	if len(parameters) != 0 {
		filtered := identifiers[:0]
		for _, identifier := range identifiers {
			if !parameters[identifier] {
				filtered = append(filtered, identifier)
			}
		}
		identifiers = filtered
	}
	tokenPaste := false
	safeTokenPastePrefixes := []string{}
	if hasTokenPaste {
		tokenPaste, safeTokenPastePrefixes = configDependencyMacroTokenPasteRisk(replacement, parameters)
	}
	stringification := false
	if len(parameters) != 0 && (strings.Contains(replacement, "#") || strings.Contains(replacement, "%:")) {
		// This only schedules a possible precision improvement. It grants no
		// permission to remove reads; the ordered complete-call proof does that.
		tokens, valid := configDependencyMacroPasteTokens(replacement)
		if valid {
			for _, token := range tokens {
				stringification = stringification || token.stringification
			}
		}
	}
	return configDependencyMacroExpansionDefinition{
		name: name, identifiers: identifiers, tokenPaste: tokenPaste,
		stringification:        stringification,
		safeTokenPastePrefixes: safeTokenPastePrefixes,
	}
}

func configDependencySourceMacroExpansionDefinition(value string) (
	configDependencyMacroExpansionDefinition,
	string,
	bool,
) {
	name, parameters, replacement, ok := configDependencyMacroSignature(value)
	if !ok {
		return configDependencyMacroExpansionDefinition{}, "", false
	}
	replacement = strings.TrimSpace(replacement)
	return configDependencyMacroExpansionDefinitionForReplacement(name, parameters, replacement), replacement, true
}

func configDependencyCompilerMacroExpansionDefinition(value string) (
	configDependencyMacroExpansionDefinition,
	string,
	bool,
) {
	signature, replacement, hasReplacement := strings.Cut(value, "=")
	if !hasReplacement {
		replacement = "1"
	}
	name, parameters, rest, ok := configDependencyMacroSignature(strings.TrimSpace(signature))
	if !ok || strings.TrimSpace(rest) != "" {
		return configDependencyMacroExpansionDefinition{}, "", false
	}
	return configDependencyMacroExpansionDefinitionForReplacement(name, parameters, replacement), replacement, true
}

func parseConfigDependencyCompilerPredefines(contents string) (configDependencyMacroState, string) {
	return parseConfigDependencyCompilerPredefinesWithReplacements(contents, false)
}

func parseConfigDependencyCompilerPredefinesWithReplacements(contents string, replacements bool) (configDependencyMacroState, string) {
	state := configDependencyMacroState{
		snapshot: newConfigDependencyMacroSnapshot(),
		symbols:  map[string]bool{},
	}
	origin := ""
	if replacements {
		state.macroReplacements = map[string]configDependencyMacroReplacement{}
		origin = fmt.Sprintf("compiler-predefines:%x", sha256.Sum256([]byte(contents)))
	}
	predefinedNames := map[string]bool{}
	for lineIndex, raw := range strings.Split(contents, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#define ") {
			return configDependencyMacroState{}, "compiler predefine dump contains a non-define line"
		}
		definition := strings.TrimSpace(strings.TrimPrefix(line, "#define "))
		expansion, replacement, ok := configDependencySourceMacroExpansionDefinition(definition)
		if !ok {
			return configDependencyMacroState{}, "compiler predefine dump contains an invalid macro name"
		}
		symbols, reason := configDependencyMacroReplacementSymbols(replacement)
		if reason != "" {
			return configDependencyMacroState{}, "compiler predefine dump " + reason
		}
		for _, symbol := range symbols {
			state.symbols[symbol] = true
		}
		state.macroExpansions = append(state.macroExpansions, expansion)
		// Conditional pruning consumes only macro definedness. Replacement-body
		// CONFIG_* references are retained separately above because an alias such
		// as `#define ENABLED CONFIG_FOO` still consumes the filtered capsule.
		state.set(expansion.name, configDependencyMacroDefined)
		if replacements {
			state.macroReplacements[expansion.name] = configDependencyMacroReplacement{
				text: definition, origin: origin + ":" + strconv.Itoa(lineIndex),
			}
		}
		predefinedNames[expansion.name] = true
	}
	state.compilerPredefinedDigest = configDependencyAutoconfNamesDigest(
		slices.Sorted(maps.Keys(predefinedNames)),
	)
	state.compilerPredefinedSnapshot = state.snapshot
	return state, ""
}

func configDependencyMacroReplacementSymbols(replacement string) ([]string, string) {
	identifiers, tokenPaste := configDependencyMacroLex(replacement)
	symbols := map[string]bool{}
	for _, identifier := range identifiers {
		if !strings.HasPrefix(identifier, "CONFIG_") {
			continue
		}
		normalized := normalizeConfigDependencySymbol(identifier)
		if !isConfigKey(normalized) {
			if tokenPaste {
				// Reachability analysis below decides whether this incomplete token
				// can participate in an executed paste. An unused definition must not
				// make every GCC/Clang compilation opaque.
				continue
			}
			return nil, "macro replacement contains an incomplete CONFIG_* identifier"
		}
		symbols[normalized] = true
	}
	return slices.Sorted(maps.Keys(symbols)), ""
}

func configDependencyCompilerMacroExpansionDefinitions(arguments []string) (
	[]configDependencyMacroExpansionDefinition,
	string,
) {
	definitions := []configDependencyMacroExpansionDefinition{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		definitionText := ""
		switch {
		case argument == "-D":
			if index+1 >= len(arguments) {
				return nil, "compiler -D option has no definition"
			}
			index++
			definitionText = arguments[index]
		case strings.HasPrefix(argument, "-D") && len(argument) > 2:
			definitionText = argument[2:]
		default:
			continue
		}
		definition, _, ok := configDependencyCompilerMacroExpansionDefinition(definitionText)
		if !ok {
			return nil, "compiler -D option has an invalid macro definition"
		}
		definitions = append(definitions, definition)
	}
	return definitions, ""
}

func configDependencyCompilerDefineReplacementSymbols(arguments []string) ([]string, string) {
	seen := map[string]bool{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		definition := ""
		switch {
		case argument == "-D":
			if index+1 >= len(arguments) {
				return nil, "compiler -D option has no definition"
			}
			index++
			definition = arguments[index]
		case strings.HasPrefix(argument, "-D") && len(argument) > 2:
			definition = argument[2:]
		default:
			continue
		}
		_, replacement, ok := configDependencyCompilerMacroExpansionDefinition(definition)
		if !ok {
			return nil, "compiler -D option has an invalid macro definition"
		}
		symbols, reason := configDependencyMacroReplacementSymbols(replacement)
		if reason != "" {
			return nil, "compiler -D " + reason
		}
		for _, symbol := range symbols {
			seen[symbol] = true
		}
	}
	return slices.Sorted(maps.Keys(seen)), ""
}

func (s *configDependencyMacroState) definition(name string) configDependencyMacroDefinition {
	s.recordForcedHeaderRead(name)
	cell, valid := s.snapshotCell(name)
	if !valid {
		return configDependencyMacroUnknown
	}
	return cell.definition
}

func configDependencyAbsentMacroDefinition(name string) configDependencyMacroDefinition {
	if strings.HasPrefix(name, "_") {
		return configDependencyMacroUnknown
	}
	return configDependencyMacroUndefined
}

// explicitDefinition reports the exact modeled cell for name and whether an
// explicit definition was recorded in the immutable snapshot or branch overlay.
func (s *configDependencyMacroState) explicitDefinition(
	name string,
) (configDependencyMacroDefinition, bool) {
	s.recordForcedHeaderRead(name)
	cell, valid := s.snapshotCell(name)
	if !valid {
		return configDependencyMacroUnknown, true
	}
	return cell.definition, cell.recorded
}

func (s *configDependencyMacroState) set(name string, definition configDependencyMacroDefinition) {
	if s == nil || name == "" {
		return
	}
	if s.rejectInvalidMutation() {
		return
	}
	if !s.putSnapshotCell(name, configDependencyMacroStateCanonicalCell(name, configDependencyMacroSnapshotCell{
		definition: definition,
		recorded:   true,
	})) {
		s.tainted = true
		return
	}
	s.recordForcedHeaderTouch(name)
	delete(s.macroReplacements, name)
}

// applyValidatedNumericMacroHeader applies the execution validator's exact
// abstract effect. The unavailable header may define arbitrary non-CONFIG
// names to literal integers, but it cannot undefine anything, define CONFIG_*
// names, add an include edge, or introduce an identifier-valued expansion.
func (s *configDependencyMacroState) applyValidatedNumericMacroHeader() {
	if s == nil {
		return
	}
	if s.rejectInvalidMutation() {
		return
	}
	if !s.applySnapshotValidatedNumericMacroHeader() {
		s.tainted = true
		return
	}
	if s.forcedHeaderTrace != nil {
		s.forcedHeaderTrace.nonConfigWildcard = true
	}
	// The validator proves integer syntax, not exact names or values. An
	// existing non-CONFIG replacement may have been overwritten even when its
	// definedness is unchanged.
	for name := range s.macroReplacements {
		if !strings.HasPrefix(name, "CONFIG_") {
			delete(s.macroReplacements, name)
		}
	}
}

// applyConfigNameSet applies the exact positive definitions emitted by one
// generated CONFIG header. maybe joins execution with the path which skipped
// the header. Production roots use immutable snapshots; speculative children
// retain only the normalized exact overlay relative to their parent.
func (s *configDependencyMacroState) applyConfigNameSet(names []string, maybe bool) bool {
	if s == nil || s.rejectInvalidMutation() {
		return false
	}
	for _, name := range names {
		if name == "" || !strings.HasPrefix(name, "CONFIG_") {
			s.tainted = true
			return false
		}
	}
	if len(names) == 0 {
		return true
	}
	for _, name := range names {
		s.recordForcedHeaderTouch(name)
		delete(s.macroReplacements, name)
		if maybe {
			s.recordForcedHeaderRead(name)
		}
	}
	if s.parent == nil {
		next, valid := configDependencyMacroStateWithConfigNameSet(s.snapshot, names, maybe)
		if !valid {
			s.tainted = true
			return false
		}
		s.snapshot = next
		s.materializedSnapshotCache = nil
		s.snapshotRevision++
		return true
	}
	for _, name := range names {
		cell := configDependencyMacroStateCanonicalCell(name, configDependencyMacroSnapshotCell{
			definition: configDependencyMacroDefined,
			recorded:   true,
		})
		if maybe {
			current, valid := s.snapshotCellUnchecked(name)
			if !valid {
				s.tainted = true
				return false
			}
			cell = configDependencyMacroStateMaybeDefineCell(name, current)
		}
		if !s.putSnapshotCell(name, cell) {
			s.tainted = true
			return false
		}
	}
	return true
}

// branch starts one speculative preprocessing path. The parent's complete
// namespace is materialized once per revision and shared by every sibling.
// Child reads and normalization are then independent of include-stack depth.
func (s *configDependencyMacroState) branch() *configDependencyMacroState {
	if s == nil || s.snapshot == nil || s.tainted || s.consumed {
		return nil
	}
	forkSnapshot, valid := s.materializedSnapshot()
	if !valid {
		return nil
	}
	branch := &configDependencyMacroState{
		snapshot:             forkSnapshot,
		snapshotForkRevision: s.snapshotRevision,
		symbols:              s.symbols,
		macroExpansions:      s.macroExpansions,
		macroReplacements:    maps.Clone(s.macroReplacements),
		forcedHeaderTrace:    s.forcedHeaderTrace,
		tainted:              s.tainted,
		parent:               s,
	}
	return branch
}

func (s *configDependencyMacroState) consume() {
	if s == nil {
		return
	}
	s.parent = nil
	s.snapshot = nil
	s.snapshotChanges = configDependencyMacroSnapshotChangeSet{}
	s.materializedSnapshotCache = nil
	s.materializedSnapshotPending = configDependencyMacroSnapshotChangeSet{}
	s.symbols = nil
	s.macroExpansions = nil
	s.macroReplacements = nil
	s.compilerPredefinedDigest = [sha256.Size]byte{}
	s.compilerPredefinedSnapshot = nil
	s.forcedHeaderTrace = nil
	s.forcedHeaderTouches = configDependencyMacroSnapshotNameSet{}
	s.consumed = true
}

// commitBranch applies one direct child's normalized overlay to s. A branch is
// committed only after its preprocessing path completed exactly. An ancestry
// or snapshot-revision mismatch fails closed instead of materializing changes
// against a different entry state.
func (s *configDependencyMacroState) commitBranch(
	branch *configDependencyMacroState,
) bool {
	if s != nil && s.rejectInvalidMutation() {
		return false
	}
	if s == nil || branch == nil || branch.parent != s {
		if s != nil {
			s.tainted = true
		}
		return false
	}
	if s.tainted || branch.tainted {
		s.tainted = true
		return false
	}
	forkSnapshot, valid := s.materializedSnapshot()
	if !valid || branch.snapshot == nil || branch.snapshot != forkSnapshot ||
		branch.snapshotForkRevision != s.snapshotRevision {
		s.tainted = true
		return false
	}
	materialized := branch.materializedSnapshotCache
	if materialized != nil {
		// A child may have written since its last fork. Flush only an existing
		// checkpoint; a never-materialized sparse child must stay sparse while
		// unwinding through its ancestors.
		materialized, valid = branch.materializedSnapshot()
		if !valid {
			s.tainted = true
			return false
		}
	}
	revision := s.snapshotRevision
	replacementsChanged := !maps.Equal(s.macroReplacements, branch.macroReplacements)
	if !s.applySnapshotChangesWithMaterializedResult(forkSnapshot, branch.snapshotChanges, materialized) {
		s.tainted = true
		return false
	}
	s.mergeForcedHeaderTouches(branch, false)
	s.macroReplacements = branch.macroReplacements
	if replacementsChanged && s.snapshotRevision == revision {
		s.snapshotRevision++
	}
	branch.consume()
	return true
}

// mergePossibleBranch joins a speculative path with the path which did not
// execute it.
func (s *configDependencyMacroState) mergePossibleBranch(branch *configDependencyMacroState) {
	s.mergePossibleBranches(nil, branch)
}

func configDependencyJoinMacroSnapshotChangeSetsWithoutWildcard(
	base *configDependencyMacroSnapshot,
	left, right configDependencyMacroSnapshotChangeSet,
) (configDependencyMacroSnapshotChangeSet, bool) {
	if base == nil || left.nonConfigWildcard || right.nonConfigWildcard {
		return configDependencyMacroSnapshotChangeSet{}, false
	}
	result := configDependencyMacroSnapshotChangeSet{
		unknownNames: configDependencyMacroSnapshotUnionNameSets(
			left.unknownNames, right.unknownNames,
		),
	}
	valid := true
	joinConcrete := func(name string, _ configDependencyMacroSnapshotCell) {
		if !valid || result.unknownNames.contains(name) {
			return
		}
		baseCell, found := base.lookup(name)
		if !found {
			valid = false
			return
		}
		leftCell := baseCell
		if exact, found := left.lookup(name); found {
			leftCell = exact
		}
		rightCell := baseCell
		if exact, found := right.lookup(name); found {
			rightCell = exact
		}
		joined := configDependencyMacroStateCanonicalCell(
			name,
			configDependencyMacroSnapshotJoinCells(leftCell, rightCell),
		)
		valid = result.putNormalized(base, name, joined)
	}
	left.forEachConcrete(joinConcrete)
	right.forEachConcrete(func(name string, cell configDependencyMacroSnapshotCell) {
		if _, alreadyVisited := left.lookup(name); !alreadyVisited {
			joinConcrete(name, cell)
		}
	})
	if !valid {
		return configDependencyMacroSnapshotChangeSet{}, false
	}
	return result, true
}

// mergePossibleBranches retains the macro state common to two speculative
// paths. nil denotes the unchanged parent path. Both non-nil branches must be
// direct children of s, and s must remain immutable until every joined value
// has been computed. A violated ancestry invariant taints the result instead
// of risking an unsound configuration projection.
func (s *configDependencyMacroState) mergePossibleBranches(
	left, right *configDependencyMacroState,
) {
	if s == nil || s.rejectInvalidMutation() {
		return
	}
	for _, branch := range []*configDependencyMacroState{left, right} {
		if branch != nil && (branch.parent != s || branch.snapshot == nil ||
			branch.tainted || branch.consumed ||
			branch.snapshotForkRevision != s.snapshotRevision) {
			s.tainted = true
			return
		}
	}
	if left == nil && right == nil {
		return
	}
	forkSnapshot, valid := s.materializedSnapshot()
	if !valid || left != nil && left.snapshot != forkSnapshot ||
		right != nil && right.snapshot != forkSnapshot {
		s.tainted = true
		return
	}

	leftChanges := configDependencyMacroSnapshotChangeSet{}
	if left != nil {
		leftChanges = left.snapshotChanges
	}
	rightChanges := configDependencyMacroSnapshotChangeSet{}
	if right != nil {
		rightChanges = right.snapshotChanges
	}
	joinedExact := configDependencyMacroSnapshotChangeSet{}
	if !leftChanges.nonConfigWildcard && !rightChanges.nonConfigWildcard {
		joinedExact, valid = configDependencyJoinMacroSnapshotChangeSetsWithoutWildcard(
			forkSnapshot, leftChanges, rightChanges,
		)
	} else {
		joinedWildcard := leftChanges.nonConfigWildcard || rightChanges.nonConfigWildcard
		joinedExact.nonConfigWildcard = joinedWildcard
		valid = true
		joinName := func(name string, _ configDependencyMacroSnapshotCell) {
			if !valid {
				return
			}
			baseCell, baseValid := forkSnapshot.lookup(name)
			if !baseValid {
				valid = false
				return
			}
			leftCell, leftValid := leftChanges.cell(forkSnapshot, name)
			rightCell, rightValid := rightChanges.cell(forkSnapshot, name)
			if !leftValid || !rightValid {
				valid = false
				return
			}
			joined := configDependencyMacroStateCanonicalCell(
				name,
				configDependencyMacroSnapshotJoinCells(leftCell, rightCell),
			)
			postWildcardBase := baseCell
			if joinedWildcard && !strings.HasPrefix(name, "CONFIG_") {
				postWildcardBase = configDependencyMacroStateMaybeDefineCell(name, postWildcardBase)
			}
			if joined != postWildcardBase {
				joinedExact.put(name, joined)
			} else {
				joinedExact.delete(name)
			}
		}
		leftChanges.forEach(joinName)
		rightChanges.forEach(func(name string, cell configDependencyMacroSnapshotCell) {
			if _, alreadyVisited := leftChanges.lookup(name); !alreadyVisited {
				joinName(name, cell)
			}
		})
	}
	if !valid {
		s.tainted = true
		return
	}
	replacements := s.joinMacroReplacements(left, right)
	revision := s.snapshotRevision
	replacementsChanged := !maps.Equal(s.macroReplacements, replacements)
	if !s.applySnapshotChanges(forkSnapshot, joinedExact) {
		s.tainted = true
		return
	}
	s.macroReplacements = replacements
	if replacementsChanged && s.snapshotRevision == revision {
		s.snapshotRevision++
	}
	s.mergeForcedHeaderTouches(left, true)
	if right != left {
		s.mergeForcedHeaderTouches(right, true)
	}
	left.consume()
	if right != left {
		right.consume()
	}
}

// configDependencyResolvedAutoconfDefinitions is the immutable positive macro
// effect of one resolved synthetic autoconf header. Building it filters the
// complete config fragment once; translation units apply the sorted immutable
// name set with one persistent-snapshot batch operation.
type configDependencyResolvedAutoconfDefinitions struct {
	names            []string
	replacements     map[string]configDependencyMacroReplacement
	cacheDigest      [sha256.Size]byte
	cacheDigestReady bool
}

func newConfigDependencyResolvedAutoconfDefinitions(
	fragment map[string]string,
) configDependencyResolvedAutoconfDefinitions {
	names := map[string]bool{}
	replacements := map[string]configDependencyMacroReplacement{}
	// The physical header renderer writes sorted original keys. Tristate m
	// adds a _MODULE suffix, which can collide with a later original key; keep
	// the same last definition rather than depending on Go map iteration order.
	for _, key := range slices.Sorted(maps.Keys(fragment)) {
		value := fragment[key]
		if !isConfigKey(key) {
			continue
		}
		switch value {
		case "", "n":
			continue
		case "m":
			names[key+"_MODULE"] = true
			replacements[key+"_MODULE"] = configDependencyMacroReplacement{text: key + "_MODULE 1", origin: configDependencyAutoconfPath}
		default:
			names[key] = true
			if value == "y" {
				value = "1"
			}
			replacements[key] = configDependencyMacroReplacement{text: key + " " + value, origin: configDependencyAutoconfPath}
		}
	}
	ordered := slices.Sorted(maps.Keys(names))
	return configDependencyResolvedAutoconfDefinitions{
		names:            ordered,
		replacements:     replacements,
		cacheDigest:      configDependencyAutoconfNamesDigest(ordered),
		cacheDigestReady: true,
	}
}

func configDependencyAutoconfNamesDigest(names []string) [sha256.Size]byte {
	var key strings.Builder
	key.WriteString(strconv.Itoa(len(names)))
	key.WriteByte('[')
	for _, name := range names {
		appendConfigDependencyCacheString(&key, name)
	}
	key.WriteByte(']')
	return sha256.Sum256([]byte(key.String()))
}

// applyResolvedConfigAutoconf models exactly the definitions emitted by
// configValueToHeaderLine. The generated header never undefines a name: an
// omitted or n-valued symbol therefore leaves a command-line or earlier header
// definition intact. definitions is a context-owned immutable value; this path
// never rescans the resolved fragment. The synthetic guard remains an ordinary
// exact write immediately before the body batch, preserving preprocessing order.
func (s *configDependencyMacroState) applyResolvedConfigAutoconf(
	definitions configDependencyResolvedAutoconfDefinitions,
) {
	if s == nil {
		return
	}
	const guard = configDependencyResolvedAutoconfGuard
	guardState, recorded := s.explicitDefinition(guard)
	if !recorded {
		// This guard is emitted by linux.bzl itself, not a context-sensitive
		// implementation identifier such as __FILE__. A complete -dM result which
		// omits this exact synthetic name proves it initially undefined.
		guardState = configDependencyMacroUndefined
	}
	if guardState == configDependencyMacroDefined {
		return
	}
	previous := s.macroReplacements
	if previous != nil && guardState == configDependencyMacroUnknown {
		previous = maps.Clone(previous)
	}
	s.set(guard, configDependencyMacroDefined)
	if guardState == configDependencyMacroUnknown {
		// If the guard was already defined, the real header is a no-op; if it was
		// undefined, the body executes. Both paths leave the guard defined, while
		// each emitted CONFIG name joins its previous state with Defined.
		if len(definitions.names) != 0 {
			s.applyConfigNameSet(definitions.names, true)
		}
		s.applyAutoconfReplacements(definitions, previous, true)
		return
	}
	if len(definitions.names) != 0 {
		s.applyConfigNameSet(definitions.names, false)
	}
	s.applyAutoconfReplacements(definitions, previous, false)
}

func configDependencyStripComments(contents []byte) ([]byte, string) {
	const (
		configDependencyLexNormal = iota
		configDependencyLexBlockComment
		configDependencyLexLineComment
		configDependencyLexString
		configDependencyLexCharacter
	)
	state := configDependencyLexNormal
	out := make([]byte, 0, len(contents))
	for index := 0; index < len(contents); index++ {
		char := contents[index]
		switch state {
		case configDependencyLexNormal:
			switch {
			case configDependencyRawStringPrefix(contents, index):
				return nil, "source contains an unmodeled C++ raw string"
			case char == '/' && index+1 < len(contents) && contents[index+1] == '*':
				out = append(out, ' ')
				index++
				state = configDependencyLexBlockComment
			case char == '/' && index+1 < len(contents) && contents[index+1] == '/':
				out = append(out, ' ')
				index++
				state = configDependencyLexLineComment
			case char == '"':
				out = append(out, char)
				state = configDependencyLexString
			case char == '\'':
				out = append(out, char)
				state = configDependencyLexCharacter
			default:
				out = append(out, char)
			}
		case configDependencyLexBlockComment:
			// The opening delimiter already emitted the comment's one space.
			// Its embedded newlines do not end a preprocessing directive:
			// retaining them could fold a false prefix and omit real inputs.
			if char == '*' && index+1 < len(contents) && contents[index+1] == '/' {
				index++
				state = configDependencyLexNormal
			}
		case configDependencyLexLineComment:
			if char == '\n' {
				out = append(out, char)
				state = configDependencyLexNormal
			}
		case configDependencyLexString, configDependencyLexCharacter:
			out = append(out, char)
			if char == '\\' && index+1 < len(contents) {
				index++
				out = append(out, contents[index])
				continue
			}
			if (state == configDependencyLexString && char == '"') ||
				(state == configDependencyLexCharacter && char == '\'') {
				state = configDependencyLexNormal
			}
		}
	}
	if state == configDependencyLexBlockComment {
		return nil, "source has an unterminated block comment"
	}
	return out, ""
}

func configDependencyPreprocessorText(contents []byte) (string, string) {
	for index, value := range contents {
		if value == '\r' && (index+1 == len(contents) || contents[index+1] != '\n') {
			return "", "source uses an unmodeled bare carriage-return line ending"
		}
		if value != '\\' {
			continue
		}
		end := index + 1
		for end < len(contents) && (contents[end] == ' ' || contents[end] == '\t' || contents[end] == '\v' || contents[end] == '\f') {
			end++
		}
		if end > index+1 && end < len(contents) && (contents[end] == '\n' || contents[end] == '\r') {
			// GCC and Clang can splice these with only a warning. Treating the
			// backslash as punctuation would miss joined identifiers/directives.
			return "", "source uses an unmodeled whitespace-separated line splice"
		}
	}
	if bytes.Contains(contents, []byte("__has_include")) {
		return "", "source uses __has_include header-existence introspection"
	}
	if bytes.Contains(contents, []byte("__has_embed")) {
		return "", "source uses __has_embed file-existence introspection"
	}
	for _, trigraph := range []string{"??=", "??/", "??'", "??(", "??)", "??!", "??<", "??>", "??-"} {
		if bytes.Contains(contents, []byte(trigraph)) {
			return "", "source contains a trigraph whose preprocessing semantics are unmodeled"
		}
	}
	spliced := bytes.ReplaceAll(contents, []byte("\\\r\n"), nil)
	spliced = bytes.ReplaceAll(spliced, []byte("\\\n"), nil)
	withoutComments, reason := configDependencyStripComments(spliced)
	if reason != "" {
		return "", reason
	}
	return string(withoutComments), ""
}

// configDependencySourceMacroMutations returns the literal macro effects which
// can invalidate a previously proven include-guard no-op. It deliberately
// scans the complete reachable file rather than trying to repeat conditional
// evaluation: a false positive only makes the node opaque, while overlooking
// an #undef or macro-stack restoration could make cross-config reuse unsound.
func configDependencySourceMacroMutations(contents []byte) ([]string, bool) {
	text, reason := configDependencyPreprocessorText(contents)
	if reason != "" {
		return nil, false
	}
	undefined := map[string]bool{}
	// This intentionally includes GNU _Pragma, MS __pragma, direct #pragma,
	// and macro replacement text. Exact reachability is unnecessary here: the
	// check only guards the exceptional repeated-forced-header optimization.
	macroStackMutation := strings.Contains(text, "push_macro") || strings.Contains(text, "pop_macro")
	for _, original := range strings.Split(text, "\n") {
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if !strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		directiveEnd := 0
		for directiveEnd < len(line) && configDependencyIdentifierByte(line[directiveEnd]) {
			directiveEnd++
		}
		directive := line[:directiveEnd]
		rest := strings.TrimSpace(line[directiveEnd:])
		switch directive {
		case "undef":
			name, ok := configDependencyMacroIdentifier(rest)
			if ok {
				undefined[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(undefined)), macroStackMutation
}

// configDependencyConventionalIncludeGuard recognizes the exact whole-file
// guard shape used by Linux headers:
//
//	#ifndef GUARD
//	#define GUARD [replacement]
//	...
//	#endif
//
// The matching outer #endif must be the final non-whitespace token and may not
// have an #else/#elif peer. Recognition alone does not make a repeated include
// safe: the ordered interpreter records that only when its post-state proves
// GUARD defined after all nested effects.
func configDependencyConventionalIncludeGuard(contents []byte) string {
	text, reason := configDependencyPreprocessorText(contents)
	if reason != "" {
		return ""
	}
	type directiveLine struct {
		name string
		rest string
	}
	parseDirective := func(original string) (directiveLine, bool) {
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if !strings.HasPrefix(line, "#") {
			return directiveLine{}, false
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		end := 0
		for end < len(line) && configDependencyIdentifierByte(line[end]) {
			end++
		}
		return directiveLine{name: line[:end], rest: strings.TrimSpace(line[end:])}, true
	}
	lines := strings.Split(text, "\n")
	first := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		first = index
		break
	}
	if first < 0 {
		return ""
	}
	opening, ok := parseDirective(lines[first])
	if !ok || opening.name != "ifndef" {
		return ""
	}
	guard, ok := configDependencyMacroIdentifier(opening.rest)
	if !ok || guard != opening.rest {
		return ""
	}
	defineIndex := -1
	for index := first + 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "" {
			continue
		}
		defineIndex = index
		break
	}
	if defineIndex < 0 {
		return ""
	}
	definition, ok := parseDirective(lines[defineIndex])
	if !ok || definition.name != "define" {
		return ""
	}
	definedName, parameters, _, ok := configDependencyMacroSignature(definition.rest)
	if !ok || definedName != guard || parameters != nil {
		return ""
	}

	depth := 0
	for index := first; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		directive, directiveOK := parseDirective(line)
		if directiveOK {
			switch directive.name {
			case "if", "ifdef", "ifndef":
				depth++
			case "elif", "elifdef", "elifndef", "else":
				if depth == 1 {
					return ""
				}
			case "endif":
				depth--
				if depth < 0 {
					return ""
				}
				if depth == 0 {
					for _, trailing := range lines[index+1:] {
						if strings.TrimSpace(trailing) != "" {
							return ""
						}
					}
					return guard
				}
			}
		}
	}
	return ""
}

func configDependencySourceMacroReplacementReason(text string) string {
	for _, original := range strings.Split(text, "\n") {
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if !strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		directiveEnd := 0
		for directiveEnd < len(line) && configDependencyIdentifierByte(line[directiveEnd]) {
			directiveEnd++
		}
		if line[:directiveEnd] != "define" {
			continue
		}
		rest := strings.TrimSpace(line[directiveEnd:])
		_, replacement, ok := configDependencySourceMacroExpansionDefinition(rest)
		if !ok {
			continue
		}
		if _, reason := configDependencyMacroReplacementSymbols(replacement); reason != "" {
			return "source " + reason
		}
	}
	return ""
}

// configDependencySourceMacroExpansions separates definitions from expansion
// roots in one reachable preprocessor stream. A definition contributes its
// macro name and replacement-identifier edges, but neither its defining name
// nor anything in its replacement body is a root. Validated definedness and
// undef operands name macros without expanding their replacement lists; their
// state reads/writes and CONFIG evidence are tracked independently. Comments
// and quoted literals are ignored by configDependencyMacroLex.
func configDependencySourceMacroExpansions(contents []byte) (
	[]configDependencyMacroExpansionDefinition,
	[]string,
) {
	text, reason := configDependencyPreprocessorText(contents)
	if reason != "" {
		// The ordinary include parser will surface the lexical reason and make the
		// node opaque. Do not replace that more specific diagnostic here.
		return nil, nil
	}
	definitions := []configDependencyMacroExpansionDefinition{}
	roots := map[string]bool{}
	for _, original := range strings.Split(text, "\n") {
		expansionText := original
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if strings.HasPrefix(line, "#") {
			directiveText := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			directiveEnd := 0
			for directiveEnd < len(directiveText) && configDependencyIdentifierByte(directiveText[directiveEnd]) {
				directiveEnd++
			}
			directive := directiveText[:directiveEnd]
			rest := strings.TrimSpace(directiveText[directiveEnd:])
			switch directive {
			case "define":
				definition, _, ok := configDependencySourceMacroExpansionDefinition(rest)
				if ok {
					definitions = append(definitions, definition)
				}
				continue
			case "ifdef", "ifndef", "undef":
				if name, valid := configDependencyMacroIdentifier(rest); valid && name == rest {
					continue
				}
			case "if", "elif":
				// The directive keyword is never macro-expanded, even when the
				// operand is outside our bounded conditional grammar. In particular,
				// #if must not make a C macro named "if" reachable. Keep every
				// operand token: macros there can still expand to arbitrary syntax.
				expansionText = rest
				// The complete bounded grammar accepts only Boolean literals,
				// operators and defined operands, none of which expand macros.
				// Unknown/malformed expressions retain every old root: a raw
				// macro may expand to operators or further invocation tokens.
				if len(rest) <= configDependencyConditionalMaximumBytes {
					parser := configDependencyConditionalParser{text: rest}
					if _, valid := parser.parse(); valid {
						continue
					}
				}
			}
		}
		identifiers, _ := configDependencyMacroLex(expansionText)
		for _, identifier := range identifiers {
			roots[identifier] = true
		}
	}
	return definitions, slices.Sorted(maps.Keys(roots))
}

// configDependencyLiteralIncludes recognizes the preprocessing directives
// whose path can be followed without executing a compiler. Conditional
// branches are intentionally unioned. A non-literal include is reported as
// opaque instead of being silently omitted.
func configDependencyLiteralIncludes(contents []byte) ([]configDependencyLiteralInclude, string) {
	text, reason := configDependencyPreprocessorText(contents)
	if reason != "" {
		return nil, reason
	}
	if reason := configDependencySourceMacroReplacementReason(text); reason != "" {
		return nil, reason
	}
	includes := []configDependencyLiteralInclude{}
	for _, original := range strings.Split(text, "\n") {
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if !strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		directiveEnd := 0
		for directiveEnd < len(line) && configDependencyIdentifierByte(line[directiveEnd]) {
			directiveEnd++
		}
		directive := line[:directiveEnd]
		if directive == "embed" {
			return nil, "source uses #embed"
		}
		if directive != "include" && directive != "import" {
			if directive == "include_next" {
				return nil, "source uses #include_next"
			}
			continue
		}
		rest := strings.TrimSpace(line[directiveEnd:])
		if len(rest) < 2 || rest[0] != '"' && rest[0] != '<' {
			return nil, "source uses a non-literal #include operand"
		}
		terminator := byte('>')
		quoted := rest[0] == '"'
		if quoted {
			terminator = '"'
		}
		end := strings.IndexByte(rest[1:], terminator)
		if end < 0 {
			return nil, "source has an unterminated literal #include operand"
		}
		name := rest[1 : end+1]
		if name == "" || path.IsAbs(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) {
			return nil, "source has a non-canonical literal #include operand"
		}
		includes = append(includes, configDependencyLiteralInclude{name: name, quoted: quoted})
	}
	return includes, ""
}

func configDependencyMacroNot(value configDependencyMacroDefinition) configDependencyMacroDefinition {
	switch value {
	case configDependencyMacroDefined:
		return configDependencyMacroUndefined
	case configDependencyMacroUndefined:
		return configDependencyMacroDefined
	default:
		return configDependencyMacroUnknown
	}
}

func configDependencyMacroAnd(left, right configDependencyMacroDefinition) configDependencyMacroDefinition {
	if left == configDependencyMacroUndefined || right == configDependencyMacroUndefined {
		return configDependencyMacroUndefined
	}
	if left == configDependencyMacroDefined && right == configDependencyMacroDefined {
		return configDependencyMacroDefined
	}
	return configDependencyMacroUnknown
}

type configDependencyConditionalFrame struct {
	parent    configDependencyMacroDefinition
	remaining configDependencyMacroDefinition
	elseSeen  bool
}

// The conditional interpreter is deliberately bounded to the literal-include
// subset needed by compiler source streams. It starts from an execution-probed
// predefine dump plus exact command-line -D/-U state and carries local macro
// effects through forced headers, each translation unit, and ordinary nested
// includes in preprocessing order. Unknown conditions join both possible
// states instead of pruning either branch.
type configDependencyConditionalIncludeEffect func(
	include configDependencyLiteralInclude,
	active configDependencyMacroDefinition,
) (bool, string)

type configDependencyConditionalDirective struct {
	directive string
	rest      string
	// text retains complete adjacent ordinary lines only for call coverage.
	// Splitting a macro invocation at a newline would lose preprocessing tokens.
	text               string
	origin             string
	assemblerDirective string
	assemblerQuoted    bool
	assemblerMacro     bool
}

// configDependencyConditionalProgram is syntax-only and safe to share across
// action nodes. Macro state, conditional activity, nested-include resolution,
// and branch joins remain per invocation in the interpreter below.
type configDependencyConditionalProgram struct {
	directives      []configDependencyConditionalDirective
	reason          string
	macroStack      bool
	reachablePragma bool
	callCoverage    bool
}

// configDependencyAssemblerFileDirectives returns assembler file-input
// spellings from one phase-3 logical line. Comments and line splices have
// already been handled by configDependencyPreprocessorText. The quoted bit
// distinguishes a real assembler token from text which can only reach the
// assembler through a C/C++ string (for example an inline-asm template).
func configDependencyAssemblerFileDirectives(line string) []configDependencyConditionalDirective {
	const (
		assemblerLexNormal = iota
		assemblerLexString
		assemblerLexCharacter
	)
	state := assemblerLexNormal
	directives := []configDependencyConditionalDirective{}
	for index := 0; index < len(line); index++ {
		char := line[index]
		switch state {
		case assemblerLexNormal:
			switch char {
			case '"':
				state = assemblerLexString
				continue
			case '\'':
				state = assemblerLexCharacter
				continue
			}
		case assemblerLexString, assemblerLexCharacter:
			if char == '\\' && index+1 < len(line) {
				index++
				continue
			}
			if state == assemblerLexString && char == '"' ||
				state == assemblerLexCharacter && char == '\'' {
				state = assemblerLexNormal
				continue
			}
		}
		if char != '.' {
			continue
		}
		for _, directive := range []string{".include", ".incbin"} {
			end := index + len(directive)
			if end > len(line) || line[index:end] != directive {
				continue
			}
			beforeOK := index == 0 || !configDependencyIdentifierByte(line[index-1])
			afterOK := end == len(line) || !configDependencyIdentifierByte(line[end])
			if beforeOK && afterOK {
				directives = append(directives, configDependencyConditionalDirective{
					assemblerDirective: directive,
					assemblerQuoted:    state != assemblerLexNormal,
				})
			}
		}
	}
	return directives
}

func configDependencyAssemblerDirectiveApplies(
	entry configDependencyConditionalDirective,
	language string,
) bool {
	// A replacement list can be expanded either into assembler-with-cpp input
	// or into a C/C++ inline-asm template. Without expanding the complete C and
	// assembler macro languages, every reachable spelling remains conservative.
	if entry.assemblerMacro || language == "" {
		return true
	}
	if entry.assemblerQuoted {
		return language == "c" || language == "c++"
	}
	return language == "assembler-with-cpp"
}

func compileConfigDependencyConditionalProgram(contents []byte) configDependencyConditionalProgram {
	return compileConfigDependencyConditionalProgramWithCalls(contents, "", false)
}

func compileConfigDependencyConditionalProgramWithCalls(contents []byte, origin string, retainText bool) configDependencyConditionalProgram {
	text, reason := configDependencyPreprocessorText(contents)
	if reason == "" {
		reason = configDependencySourceMacroReplacementReason(text)
	}
	program := configDependencyConditionalProgram{reason: reason, callCoverage: retainText}
	if reason != "" {
		return program
	}
	if retainText && origin == "" {
		program.reason = "call coverage source has no authenticated origin"
		return program
	}
	program.macroStack = strings.Contains(text, "push_macro") || strings.Contains(text, "pop_macro")
	definitions, roots := configDependencySourceMacroExpansions(contents)
	graph := configDependencyMacroExpansionGraph{}
	graph.addDefinitions(definitions...)
	graph.addRoots(roots...)
	program.reachablePragma = graph.reachableIdentifier("_Pragma")
	var ordinary strings.Builder
	var ordinaryAssembler []configDependencyConditionalDirective
	flushOrdinary := func() {
		program.directives = append(program.directives, ordinaryAssembler...)
		ordinaryAssembler = nil
		if ordinary.Len() != 0 {
			program.directives = append(program.directives, configDependencyConditionalDirective{text: ordinary.String()})
			ordinary.Reset()
		}
	}
	for lineIndex, original := range strings.Split(text, "\n") {
		line := strings.TrimSpace(original)
		if strings.HasPrefix(line, "%:") {
			line = "#" + strings.TrimPrefix(line, "%:")
		}
		if !strings.HasPrefix(line, "#") {
			if retainText {
				ordinary.WriteString(original)
				ordinary.WriteByte('\n')
			}
			assembler := configDependencyAssemblerFileDirectives(line)
			if retainText {
				// Diagnostics do not divide preprocessing text: .include may be a
				// C member name, and a macro call can continue on the next line.
				ordinaryAssembler = append(ordinaryAssembler, assembler...)
			} else {
				program.directives = append(program.directives, assembler...)
			}
			continue
		}
		flushOrdinary()
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		directiveEnd := 0
		for directiveEnd < len(line) && configDependencyIdentifierByte(line[directiveEnd]) {
			directiveEnd++
		}
		directive := line[:directiveEnd]
		rest := strings.TrimSpace(line[directiveEnd:])
		entry := configDependencyConditionalDirective{
			directive: directive,
			rest:      rest,
		}
		if retainText {
			entry.origin = origin + ":" + strconv.Itoa(lineIndex)
		}
		program.directives = append(program.directives, entry)
		if directive == "define" {
			for _, entry := range configDependencyAssemblerFileDirectives(rest) {
				entry.assemblerMacro = true
				program.directives = append(program.directives, entry)
			}
		}
	}
	flushOrdinary()
	return program
}

func configDependencyLiteralIncludesWithMacros(
	contents []byte,
	state *configDependencyMacroState,
) ([]configDependencyLiteralInclude, string) {
	return configDependencyLiteralIncludesWithMacroEffects(contents, state, nil)
}

// configDependencyLiteralIncludesWithMacroEffects interprets the bounded
// conditional language above while allowing an ordered compiler-source scanner
// to apply the exact macro effects of one inspectable nested include. The
// callback returns true only when it modeled the include synchronously. Every
// unknown or deferred effect taints later conditions, preserving the old
// branch-union behavior for callers which do not provide a callback.
func configDependencyLiteralIncludesWithMacroEffects(
	contents []byte,
	state *configDependencyMacroState,
	effect configDependencyConditionalIncludeEffect,
) ([]configDependencyLiteralInclude, string) {
	return interpretConfigDependencyConditionalProgram(
		compileConfigDependencyConditionalProgram(contents), state, effect, "",
	)
}

func interpretConfigDependencyConditionalProgram(
	program configDependencyConditionalProgram,
	state *configDependencyMacroState,
	effect configDependencyConditionalIncludeEffect,
	language string,
) ([]configDependencyLiteralInclude, string) {
	return interpretConfigDependencyConditionalProgramWithText(program, state, effect, language, nil)
}

// textEffect consumes every complete active ordinary span in preprocessing
// order, including spans reached through the synchronous include callback.
// Unlike the definedness-only interpreter, this path refuses unresolved
// control flow: a successful prefix is not complete call coverage.
func interpretConfigDependencyConditionalProgramWithText(
	program configDependencyConditionalProgram,
	state *configDependencyMacroState,
	effect configDependencyConditionalIncludeEffect,
	language string,
	textEffect func(string, *configDependencyMacroState) string,
) ([]configDependencyLiteralInclude, string) {
	return interpretConfigDependencyConditionalProgramWithCalls(program, state, effect, language, textEffect, nil, nil)
}

func interpretConfigDependencyConditionalProgramWithCalls(
	program configDependencyConditionalProgram,
	state *configDependencyMacroState,
	effect configDependencyConditionalIncludeEffect,
	language string,
	textEffect func(string, *configDependencyMacroState) string,
	conditionEffect func(string, *configDependencyMacroState) (configDependencyMacroDefinition, string),
	macroWriteEffect func(string) string,
) ([]configDependencyLiteralInclude, string) {
	if program.reason != "" {
		return nil, program.reason
	}
	if textEffect != nil && (!program.callCoverage || state == nil || state.macroReplacements == nil || !state.validSnapshotLineage()) {
		return nil, "call coverage requires complete source spans and exact replacement state"
	}
	// GCC and Clang macro-stack pragmas can restore a command-line or local
	// definition after an intervening #undef. Modeling that stack is outside this
	// bounded compiler-source interpreter. The token check also catches the _Pragma
	// spelling; false positives in strings or inactive branches only lose reuse.
	if program.macroStack {
		return nil, "source uses an unmodeled macro-stack pragma"
	}
	if effect != nil && program.reachablePragma {
		return nil, "conditionally interpreted compiler source uses an unmodeled reachable _Pragma effect"
	}
	active := configDependencyMacroDefined
	stack := []configDependencyConditionalFrame{}
	includes := []configDependencyLiteralInclude{}
	for _, entry := range program.directives {
		if entry.text != "" {
			if textEffect != nil && active != configDependencyMacroUndefined {
				if active != configDependencyMacroDefined {
					return nil, "call coverage reaches unresolved conditional text"
				}
				if reason := textEffect(entry.text, state); reason != "" {
					return nil, reason
				}
			}
			continue
		}
		if entry.assemblerDirective != "" {
			if active != configDependencyMacroUndefined && configDependencyAssemblerDirectiveApplies(entry, language) {
				return nil, "compiler source closure uses assembler file-input directive " + entry.assemblerDirective
			}
			continue
		}
		directive := entry.directive
		rest := entry.rest
		switch directive {
		case "ifdef", "ifndef":
			name, ok := configDependencyMacroIdentifier(rest)
			if !ok || name != rest {
				return nil, "source has a malformed #" + directive
			}
			condition := state.definition(name)
			if conditionEffect != nil && active != configDependencyMacroUndefined {
				var reason string
				condition, reason = conditionEffect("defined("+name+")", state)
				if reason != "" {
					return nil, reason
				}
			}
			if directive == "ifndef" {
				condition = configDependencyMacroNot(condition)
			}
			stack = append(stack, configDependencyConditionalFrame{
				parent: active, remaining: configDependencyMacroAnd(active, configDependencyMacroNot(condition)),
			})
			active = configDependencyMacroAnd(active, condition)
		case "if":
			condition := configDependencyConditionalDefinition(rest, state)
			if conditionEffect != nil && active != configDependencyMacroUndefined {
				var reason string
				condition, reason = conditionEffect(rest, state)
				if reason != "" {
					return nil, reason
				}
			}
			stack = append(stack, configDependencyConditionalFrame{
				parent: active, remaining: configDependencyMacroAnd(active, configDependencyMacroNot(condition)),
			})
			active = configDependencyMacroAnd(active, condition)
		case "elif", "elifdef", "elifndef":
			if len(stack) == 0 || stack[len(stack)-1].elseSeen {
				return nil, "source has an unmatched #" + directive
			}
			frame := &stack[len(stack)-1]
			condition := configDependencyMacroUnknown
			if directive == "elif" {
				condition = configDependencyConditionalDefinition(rest, state)
				if conditionEffect != nil && frame.remaining != configDependencyMacroUndefined {
					var reason string
					condition, reason = conditionEffect(rest, state)
					if reason != "" {
						return nil, reason
					}
				}
			} else {
				name, ok := configDependencyMacroIdentifier(rest)
				if !ok || name != rest {
					return nil, "source has a malformed #" + directive
				}
				condition = state.definition(name)
				if conditionEffect != nil && frame.remaining != configDependencyMacroUndefined {
					var reason string
					condition, reason = conditionEffect("defined("+name+")", state)
					if reason != "" {
						return nil, reason
					}
				}
				if directive == "elifndef" {
					condition = configDependencyMacroNot(condition)
				}
			}
			active = configDependencyMacroAnd(frame.remaining, condition)
			frame.remaining = configDependencyMacroAnd(frame.remaining, configDependencyMacroNot(condition))
		case "else":
			if len(stack) == 0 || stack[len(stack)-1].elseSeen || rest != "" {
				return nil, "source has an unmatched #else"
			}
			frame := &stack[len(stack)-1]
			active = frame.remaining
			frame.remaining = configDependencyMacroUndefined
			frame.elseSeen = true
		case "endif":
			if len(stack) == 0 || rest != "" {
				return nil, "source has an unmatched #endif"
			}
			active = stack[len(stack)-1].parent
			stack = stack[:len(stack)-1]
		case "define", "undef":
			if active == configDependencyMacroUndefined {
				continue
			}
			name, ok := configDependencyMacroIdentifier(rest)
			if !ok {
				return nil, "source has a malformed #" + directive
			}
			if macroWriteEffect != nil {
				if reason := macroWriteEffect(name); reason != "" {
					return nil, reason
				}
			}
			if directive == "define" && state != nil && state.macroReplacements != nil && entry.origin != "" {
				if !state.setSourceMacroReplacement(rest, entry.origin, active) {
					return nil, "source macro replacement cannot be applied in order"
				}
				continue
			}
			definition := configDependencyMacroDefined
			if directive == "undef" {
				definition = configDependencyMacroUndefined
			}
			// Join the executed write with the unchanged path. Reading even
			// an idempotent write retains its input requirement for replay.
			if active == configDependencyMacroUnknown && state.definition(name) != definition {
				definition = configDependencyMacroUnknown
			}
			state.set(name, definition)
		case "include", "import":
			if active == configDependencyMacroUndefined {
				continue
			}
			if directive == "import" && effect != nil {
				return nil, "conditionally interpreted compiler source uses once-only #import semantics"
			}
			if len(rest) < 2 || rest[0] != '"' && rest[0] != '<' {
				return nil, "source uses a non-literal #include operand"
			}
			terminator := byte('>')
			quoted := rest[0] == '"'
			if quoted {
				terminator = '"'
			}
			end := strings.IndexByte(rest[1:], terminator)
			if end < 0 {
				return nil, "source has an unterminated literal #include operand"
			}
			name := rest[1 : end+1]
			if name == "" || path.IsAbs(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) {
				return nil, "source has a non-canonical literal #include operand"
			}
			include := configDependencyLiteralInclude{name: name, quoted: quoted}
			includes = append(includes, include)
			exact := false
			if effect != nil {
				var effectReason string
				exact, effectReason = effect(include, active)
				if effectReason != "" {
					return nil, effectReason
				}
			}
			if !exact {
				if textEffect != nil {
					return nil, "call coverage has an unresolved include effect"
				}
				// The selected header may define or undefine any identifier before a
				// later directive in this file. Keep following branches conservative.
				state.tainted = true
			}
		case "include_next":
			if active != configDependencyMacroUndefined {
				return nil, "source uses #include_next"
			}
		case "embed":
			if active != configDependencyMacroUndefined {
				return nil, "source uses #embed"
			}
		case "pragma":
			if textEffect != nil && active != configDependencyMacroUndefined {
				return nil, "call coverage uses an unmodeled #pragma effect"
			}
			if active != configDependencyMacroUndefined && effect != nil && strings.TrimSpace(rest) == "once" {
				return nil, "conditionally interpreted compiler source uses unmodeled #pragma once semantics"
			}
		default:
			if textEffect != nil && active != configDependencyMacroUndefined && (directive != "" || rest != "") {
				return nil, "call coverage uses an unmodeled preprocessing directive"
			}
		}
		if textEffect != nil && active == configDependencyMacroUnknown {
			return nil, "call coverage uses unresolved conditional control flow"
		}
	}
	if len(stack) != 0 {
		return nil, "source has an unterminated conditional directive"
	}
	if textEffect != nil && !state.validSnapshotLineage() {
		return nil, "call coverage completed with invalid macro state"
	}
	return includes, ""
}

type configDependencyIncludeDirectory struct {
	logical string
	source  bool
	// external means that the planner cannot inspect this compiler-owned search
	// root. Encountering one before an inspectable match is opaque: the unknown
	// directory may contain the header and shadow every later search root.
	external bool
}

type configDependencyScanFile struct {
	logical          string
	physical         string
	source           bool
	configProjection bool
	exactContents    string
	exactIdentity    string
	macroTable       bool
}

func (f configDependencyScanFile) identity() string {
	if f.exactIdentity != "" {
		return "generated\x00" + f.logical + "\x00" + f.exactIdentity
	}
	if f.physical == "" {
		return ""
	}
	return "file\x00" + filepath.Clean(f.physical)
}

func (f configDependencyScanFile) parseIdentity() string {
	if f.exactIdentity != "" {
		digest := sha256.Sum256([]byte(f.exactContents))
		return "generated-bytes\x00" + hex.EncodeToString(digest[:])
	}
	return f.identity()
}

func configDependencyParsedFileCacheKey(file configDependencyScanFile, language string) string {
	// The branch-union parser also recognizes language-specific assembler input
	// directives. The same immutable bytes can therefore have different exact
	// dependency summaries when compiled as C and assembler-with-cpp. Keep the
	// reusable family cache content-addressed by both dimensions.
	return file.parseIdentity() + "\x00language\x00" + language
}

func (f configDependencyScanFile) contents() ([]byte, error) {
	if f.exactIdentity != "" {
		return []byte(f.exactContents), nil
	}
	if f.physical == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(f.physical)
}

type configDependencyGeneratedText struct {
	contents   string
	identity   string
	macroTable bool
	producerID string
	slot       int
}

// configDependencyPhysicalFileStatus is the immutable filesystem observation
// needed by the include scanner. exists preserves fileExists' historical
// non-directory contract; regular is the stricter queue admission check.
type configDependencyPhysicalFileStatus struct {
	exists  bool
	regular bool
	err     error
}

// configDependencyPhysicalFileCache belongs to one immutable planning input
// snapshot. Bazel input trees are immutable for an action, so every compiler
// node and every configuration planned by that action can safely reuse the
// first stat result without carrying ambient state into another action.
type configDependencyPhysicalFileCache struct {
	entries map[string]configDependencyPhysicalFileStatus
	stat    func(string) (os.FileInfo, error)
}

func newConfigDependencyPhysicalFileCache() *configDependencyPhysicalFileCache {
	return &configDependencyPhysicalFileCache{
		entries: map[string]configDependencyPhysicalFileStatus{},
		stat:    os.Stat,
	}
}

func (c *configDependencyPhysicalFileCache) status(filename string) configDependencyPhysicalFileStatus {
	filename = filepath.Clean(filename)
	if c == nil {
		info, err := os.Stat(filename)
		return configDependencyPhysicalFileStatus{
			exists: err == nil && !info.IsDir(), regular: err == nil && info.Mode().IsRegular(), err: err,
		}
	}
	if status, ok := c.entries[filename]; ok {
		return status
	}
	stat := c.stat
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(filename)
	status := configDependencyPhysicalFileStatus{err: err}
	if err == nil {
		status.exists = !info.IsDir()
		status.regular = info.Mode().IsRegular()
	}
	if c.entries == nil {
		c.entries = map[string]configDependencyPhysicalFileStatus{}
	}
	c.entries[filename] = status
	return status
}

const configDependencyMaxGeneratedTextBytes = 64 << 20

const configDependencyAutoconfPath = "include/generated/autoconf.h"

const configDependencyMaxConditionalActiveStatesPerFile = 64

// configDependencyConditionalActiveFile bounds recursive preprocessing of one
// physical input. entryState is the immutable parent of the first unguarded
// entry's mutation delta. Its full namespace is intentionally not projected
// unless the file actually recurses. At the first recursive re-entry, states
// and projections are initialized with that entry namespace; seeing one twice
// proves that the modeled recursion made no progress. Conventional whole-file
// guards use their cheaper proven-defined fast path and have no entryState.
type configDependencyConditionalActiveFile struct {
	states      map[string]bool
	projections []configDependencyConditionalMacroStateProjection
	entryState  *configDependencyMacroState
}

type configDependencyClosureScanner struct {
	// Value-only pending state does not escape speculative cache replay clones.
	unavailableGeneratedHeader  configDependencyUnavailableGeneratedHeader
	collectGeneratedHeaders     bool
	collectCompilerGuards       bool
	compilerGuardFiles          map[string]configDependencyScanFile
	compilerGuardFilesTruncated bool
	// Borrowed immutable initial probe facts, never the source-mutated namespace.
	compilerGuardInitialDefinitions   map[string]bool
	compilerIntrinsicFile             configDependencyScanFile
	compilerIntrinsicInitialAvailable map[string]bool
	compilerIntrinsicInitialSnapshot  *configDependencyMacroSnapshot
	compilerIntrinsicAnswered         func(CompilerIntrinsicCall) (bool, error)

	sourceLookup           configDependencySourceLookup
	language               string
	generated              map[string]bool
	generatedText          map[string]configDependencyGeneratedText
	resolveGeneratedText   func(string) (configDependencyGeneratedText, bool)
	lookupWorkingInput     func(string) (configDependencyInputSetProvenance, bool, error)
	resolvedGeneratedFiles map[string]configDependencyScanFile
	preconfigured          map[string]string
	physicalFiles          *configDependencyPhysicalFileCache
	quoteDirectories       []configDependencyIncludeDirectory
	includeDirectories     []configDependencyIncludeDirectory
	systemDirectories      []configDependencyIncludeDirectory
	afterDirectories       []configDependencyIncludeDirectory
	symbols                map[string]bool
	sourcePaths            map[string]bool
	objectPaths            map[string]bool
	queued                 map[string]bool
	queuedPhysical         map[string]bool
	pending                []configDependencyScanFile
	parsed                 map[string]configDependencyParsedFile
	conditionalFiles       map[string]configDependencyScanFile
	conditionalParsed      map[string]configDependencyParsedFile
	// conditionalPending records which conditionally interpreted physical
	// inputs still have an entry in pending.  A restored forced-header cache
	// entry has already aggregated its cached parse summaries and therefore
	// starts with this set empty even though conditionalFiles is populated.  If
	// the translation unit later re-enters one of those files under a different
	// macro state, queueConditional must put the merged summary back on pending
	// so newly reachable nested headers and CONFIG references are not lost.
	conditionalPending        map[string]bool
	conditionalSyntax         map[string]configDependencyConditionalSyntax
	conditionalRepeatGuards   map[string]string
	repeatedConditionalGuards map[string]bool
	macroUndefinitions        map[string]bool
	macroStackMutation        bool
	macroExpansions           configDependencyMacroExpansionGraph
	earlyMacroSummaries       map[string]bool
	opaqueReason              string
	headerCache               *configDependencyHeaderCache
	headerTrace               *configDependencyHeaderTrace
	callCoverage              *configDependencyCallCoverage
}

type configDependencyConditionalSyntax struct {
	sourceBytes                  int
	lookaheadTokenInventory      configDependencyOpenedHeaderHintInventory
	lookaheadTokenInventoryReady bool
	compilerGuardHints           configDependencyCompilerGuardHints
	compilerGuardHintsReady      bool
	compilerGuardContentID       string
	compilerTokenInventory       configDependencyOpenedHeaderHintInventory
	compilerTokenInventoryReady  bool
	reason                       string
	symbols                      []string
	macroDefinitions             []configDependencyMacroExpansionDefinition
	macroRoots                   []string
	macroUndefinitions           []string
	macroStackMutation           bool
	conventionalGuard            string
	conditionalProgram           configDependencyConditionalProgram
}

type configDependencyCompilerPredefineParse struct {
	state  configDependencyMacroState
	reason string
}

type configDependencyCompilerPredefineRequestKey string

type configDependencyCompilerPredefineRequestResult struct {
	contents         string
	ready            bool
	guardDefinitions map[string]bool
	guardIdentity    string
	lexical          configDependencyCompilerLexicalEvidence
}

// configDependencyForcedHeaderCache is scoped to one immutable action-plan
// analysis. Compiler actions commonly share an identical predefine namespace,
// header-search order, and command-line forced-header prefix. Interpreting that
// prefix for every translation unit is redundant, but caching merely by header
// pathname would be unsound: both conditional state and object-tree provenance
// can differ between otherwise similar actions.
type configDependencyForcedHeaderCache struct {
	entries           map[configDependencyForcedHeaderCacheKey][]configDependencyForcedHeaderCacheEntry
	hits              int
	misses            int
	ordinary          configDependencyHeaderCache
	resolutionRecords int
	resolutionBytes   int
}

type configDependencyForcedHeaderCacheKey struct {
	profile             string
	language            string
	searchDirectories   string
	forcedFiles         string
	autoconfDefinitions [sha256.Size]byte
}

type configDependencyForcedHeaderMacroRequirement struct {
	name string
	cell configDependencyMacroSnapshotCell
}

// configDependencyForcedHeaderCacheEntry is one dependency-specialized
// post-prefix state. Equal compiler defined-name projections can share snapshot
// directly even when replacement text such as KBUILD_BASENAME differs. Other
// roots validate only the macro cells actually consumed by the prefix and
// replay its finite exact writes plus an optional namespace-wide numeric-header
// transform. Scanner fields retain every dependency/path and repeat-guard
// observation which the final closure checks consume.
type configDependencyForcedHeaderCacheEntry struct {
	includeResolutions        []configDependencyHeaderEvent
	includeResolutionsReady   bool
	inputSnapshot             *configDependencyMacroSnapshot
	snapshot                  *configDependencyMacroSnapshot
	inputPredefinedDigest     [sha256.Size]byte
	inputPredefinedReady      bool
	macroRequirements         []configDependencyForcedHeaderMacroRequirement
	macroTransform            configDependencyMacroSnapshotChangeSet
	macroTransformReady       bool
	symbols                   []string
	macroDefinitions          []configDependencyMacroExpansionDefinition
	macroRoots                []string
	macroUndefinitions        map[string]bool
	macroStackMutation        bool
	sourcePaths               map[string]bool
	objectPaths               map[string]bool
	queued                    map[string]bool
	queuedPhysical            map[string]bool
	conditionalFiles          map[string]configDependencyScanFile
	conditionalParsed         map[string]configDependencyParsedFile
	conditionalRepeatGuards   map[string]string
	repeatedConditionalGuards map[string]bool
	generatedInputs           map[string]configDependencyScanFile
}

// configDependencyCompilerPredefineKey is an injective framing of every input
// to CompactMetadata.compilerPredefines. It deliberately retains argv order and
// canonicalizes only map iteration order: compiler option order is semantic,
// while environment map order is not.
func configDependencyCompilerPredefineKey(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) configDependencyCompilerPredefineRequestKey {
	var key strings.Builder
	appendString := func(value string) {
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
	}
	appendStrings := func(values []string) {
		key.WriteString(strconv.Itoa(len(values)))
		key.WriteByte('[')
		for _, value := range values {
			appendString(value)
		}
		key.WriteByte(']')
	}
	appendString(scope)
	appendString(role)
	appendString(language)
	appendStrings(arguments)
	appendStrings(translationUnits)
	names := slices.Sorted(maps.Keys(environment))
	key.WriteString(strconv.Itoa(len(names)))
	key.WriteByte('{')
	for _, name := range names {
		appendString(name)
		appendString(environment[name])
	}
	key.WriteByte('}')
	return configDependencyCompilerPredefineRequestKey(key.String())
}

func actionPlanCompilerPredefines(
	plan *ActionPlan,
	cache map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult,
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (configDependencyCompilerPredefineRequestResult, error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerPredefines == nil {
		return configDependencyCompilerPredefineRequestResult{}, fmt.Errorf("action plan has no compiler-predefine probe workload")
	}
	var key configDependencyCompilerPredefineRequestKey
	if cache != nil {
		key = configDependencyCompilerPredefineKey(
			scope, role, language, arguments, translationUnits, environment,
		)
		if result, ok := cache[key]; ok {
			return result, nil
		}
	}
	contents, ready, err := plan.metadata.compilerPredefines(
		scope, role, language, arguments, translationUnits, environment,
	)
	if err != nil {
		return configDependencyCompilerPredefineRequestResult{}, err
	}
	definitions, identity, guardsReady, err := actionPlanCompilerDefinedness(plan, scope, role, language, arguments, translationUnits, environment)
	if err != nil {
		return configDependencyCompilerPredefineRequestResult{}, err
	}
	supplemental, supplementalIdentity, err := configDependencySupplementalCompilerDefinedness(plan, scope, role, language, arguments, translationUnits, environment)
	if err != nil {
		return configDependencyCompilerPredefineRequestResult{}, err
	}
	if supplementalIdentity != "" {
		definitions = maps.Clone(definitions)
		if definitions == nil {
			definitions = map[string]bool{}
		}
		for name, value := range supplemental {
			if previous, exists := definitions[name]; exists && previous != value {
				return configDependencyCompilerPredefineRequestResult{}, fmt.Errorf("supplemental compiler definedness contradicts original result for %q", name)
			}
			definitions[name] = value
		}
		var combined strings.Builder
		appendConfigDependencyCacheString(&combined, "compiler-initial-guards-v2")
		appendConfigDependencyCacheString(&combined, identity)
		appendConfigDependencyCacheString(&combined, supplementalIdentity)
		identity = fmt.Sprintf("%x", sha256.Sum256([]byte(combined.String())))
	}
	lexical, err := actionPlanCompilerLexicalEvidence(plan, scope, role, language, arguments, translationUnits, environment)
	result := configDependencyCompilerPredefineRequestResult{
		contents: contents, ready: ready && guardsReady && lexical.ready,
		guardDefinitions: definitions, guardIdentity: identity,
		lexical: lexical,
	}
	if err == nil && cache != nil {
		cache[key] = result
	}
	return result, err
}

type configDependencyParsedFile struct {
	symbols            []string
	includes           []configDependencyLiteralInclude
	macroDefinitions   []configDependencyMacroExpansionDefinition
	macroRoots         []string
	macroUndefinitions []string
	macroStackMutation bool
	includesResolved   bool
	reason             string
}

func appendConfigDependencyCacheString(key *strings.Builder, value string) {
	key.WriteString(strconv.Itoa(len(value)))
	key.WriteByte(':')
	key.WriteString(value)
}

func configDependencyForcedHeaderSearchKey(
	scanner *configDependencyClosureScanner,
) string {
	var key strings.Builder
	appendDirectories := func(class byte, directories []configDependencyIncludeDirectory) {
		key.WriteByte(class)
		key.WriteString(strconv.Itoa(len(directories)))
		key.WriteByte('[')
		for _, directory := range directories {
			appendConfigDependencyCacheString(&key, directory.logical)
			if directory.source {
				key.WriteByte('s')
			} else {
				key.WriteByte('o')
			}
			if directory.external {
				key.WriteByte('e')
			} else {
				key.WriteByte('i')
			}
		}
		key.WriteByte(']')
	}
	appendDirectories('q', scanner.quoteDirectories)
	appendDirectories('i', scanner.includeDirectories)
	appendDirectories('s', scanner.systemDirectories)
	appendDirectories('a', scanner.afterDirectories)
	return key.String()
}

func configDependencyForcedHeaderFilesKey(files []configDependencyScanFile) string {
	var key strings.Builder
	key.WriteString(strconv.Itoa(len(files)))
	key.WriteByte('[')
	for _, file := range files {
		appendConfigDependencyCacheString(&key, file.logical)
		physical := file.physical
		if physical != "" {
			physical = filepath.Clean(physical)
		}
		appendConfigDependencyCacheString(&key, physical)
		appendConfigDependencyCacheString(&key, file.exactIdentity)
		appendConfigDependencyCacheString(&key, file.exactContents)
		for _, value := range []bool{file.source, file.configProjection, file.macroTable} {
			if value {
				key.WriteByte('1')
			} else {
				key.WriteByte('0')
			}
		}
	}
	key.WriteByte(']')
	return key.String()
}

func configDependencyForcedHeaderAutoconfKey(
	definitions configDependencyResolvedAutoconfDefinitions,
) [sha256.Size]byte {
	if definitions.cacheDigestReady {
		return definitions.cacheDigest
	}
	// Preserve exact behavior for hand-constructed values. Every
	// production analysis context uses newConfigDependencyResolvedAutoconfDefinitions
	// and therefore takes the constant-time path above.
	return configDependencyAutoconfNamesDigest(definitions.names)
}

func newConfigDependencyForcedHeaderCacheKey(
	scanner *configDependencyClosureScanner,
	forcedFiles []configDependencyScanFile,
	autoconfDefinitions configDependencyResolvedAutoconfDefinitions,
) configDependencyForcedHeaderCacheKey {
	return configDependencyForcedHeaderCacheKey{
		profile:             scanner.sourceLookup.profileName,
		language:            scanner.language,
		searchDirectories:   configDependencyForcedHeaderSearchKey(scanner),
		forcedFiles:         configDependencyForcedHeaderFilesKey(forcedFiles),
		autoconfDefinitions: configDependencyForcedHeaderAutoconfKey(autoconfDefinitions),
	}
}

func cloneConfigDependencyParsedFile(parsed configDependencyParsedFile) configDependencyParsedFile {
	parsed.symbols = slices.Clone(parsed.symbols)
	parsed.includes = slices.Clone(parsed.includes)
	parsed.macroDefinitions = slices.Clone(parsed.macroDefinitions)
	parsed.macroRoots = slices.Clone(parsed.macroRoots)
	parsed.macroUndefinitions = slices.Clone(parsed.macroUndefinitions)
	return parsed
}

func cloneConfigDependencyMacroExpansionDefinitions(
	definitions []configDependencyMacroExpansionDefinition,
) []configDependencyMacroExpansionDefinition {
	cloned := slices.Clone(definitions)
	for index := range cloned {
		cloned[index].identifiers = slices.Clone(cloned[index].identifiers)
		cloned[index].safeTokenPastePrefixes = slices.Clone(cloned[index].safeTokenPastePrefixes)
	}
	return cloned
}

func cloneConfigDependencyParsedFiles(
	files map[string]configDependencyParsedFile,
) map[string]configDependencyParsedFile {
	if files == nil {
		return nil
	}
	cloned := make(map[string]configDependencyParsedFile, len(files))
	for identity, parsed := range files {
		cloned[identity] = cloneConfigDependencyParsedFile(parsed)
	}
	return cloned
}

func configDependencyForcedHeaderCacheScannerPristine(
	scanner *configDependencyClosureScanner,
) bool {
	return scanner != nil && scanner.opaqueReason == "" &&
		len(scanner.sourcePaths) == 0 && len(scanner.objectPaths) == 0 &&
		len(scanner.queued) == 0 && len(scanner.queuedPhysical) == 0 &&
		len(scanner.pending) == 0 && len(scanner.conditionalFiles) == 0 &&
		len(scanner.conditionalPending) == 0 &&
		len(scanner.conditionalParsed) == 0 && len(scanner.conditionalRepeatGuards) == 0 &&
		len(scanner.repeatedConditionalGuards) == 0 && len(scanner.macroUndefinitions) == 0 &&
		!scanner.macroStackMutation
}

func configDependencyForcedHeaderGeneratedInputs(
	scanner *configDependencyClosureScanner,
	forcedFiles []configDependencyScanFile,
) (map[string]configDependencyScanFile, bool) {
	inputs := map[string]configDependencyScanFile{}
	record := func(file configDependencyScanFile) bool {
		if file.logical == "" || file.source || file.configProjection || !scanner.generated[file.logical] {
			return true
		}
		if previous, exists := inputs[file.logical]; exists && previous != file {
			return false
		}
		inputs[file.logical] = file
		return true
	}
	for _, file := range forcedFiles {
		if !record(file) {
			return nil, false
		}
	}
	for _, file := range scanner.resolvedGeneratedFiles {
		if !record(file) {
			return nil, false
		}
	}
	for _, file := range scanner.conditionalFiles {
		if !record(file) {
			return nil, false
		}
	}
	for _, file := range scanner.pending {
		if !record(file) {
			return nil, false
		}
	}
	return inputs, true
}

func captureConfigDependencyForcedHeaderCacheEntry(
	scanner *configDependencyClosureScanner,
	state, root *configDependencyMacroState,
	forcedFiles []configDependencyScanFile,
) (configDependencyForcedHeaderCacheEntry, bool) {
	if scanner == nil || state == nil || state.parent != root || state.tainted ||
		state.macroReplacements != nil || root.macroReplacements != nil ||
		!state.validSnapshotLineage() || scanner.opaqueReason != "" || scanner.headerTrace == nil ||
		!scanner.headerTrace.resolutionsOnly || scanner.headerTrace.invalid {
		return configDependencyForcedHeaderCacheEntry{}, false
	}
	generatedInputs, exact := configDependencyForcedHeaderGeneratedInputs(scanner, forcedFiles)
	if !exact {
		return configDependencyForcedHeaderCacheEntry{}, false
	}
	// scan consumes these syntax-only summaries after conditional interpretation.
	// Pre-aggregate the already-resolved forced closure once so a hit does not
	// need to walk every forced file again. A later translation unit can still
	// re-enter one of these files: conditionalFiles/conditionalParsed retain its
	// exact interpreter state, while any newly reached nested file is queued and
	// scanned normally. The per-file syntax facts below are raw-file unions and
	// therefore do not depend on which conditional path caused that re-entry.
	symbols := map[string]bool{}
	macroRoots := map[string]bool{}
	macroUndefinitions := map[string]bool{}
	macroDefinitions := []configDependencyMacroExpansionDefinition{}
	macroStackMutation := false
	seenPending := map[string]bool{}
	for _, file := range scanner.pending {
		identity := file.identity()
		if identity == "" || seenPending[identity] {
			continue
		}
		seenPending[identity] = true
		parsed, ok := scanner.conditionalParsed[identity]
		if !ok || parsed.reason != "" || !parsed.includesResolved {
			return configDependencyForcedHeaderCacheEntry{}, false
		}
		for _, symbol := range parsed.symbols {
			symbols[symbol] = true
		}
		macroDefinitions = append(macroDefinitions, parsed.macroDefinitions...)
		for _, root := range parsed.macroRoots {
			macroRoots[root] = true
		}
		for _, name := range parsed.macroUndefinitions {
			macroUndefinitions[name] = true
		}
		macroStackMutation = macroStackMutation || parsed.macroStackMutation
	}
	snapshot, valid := state.materializedSnapshot()
	if !valid {
		return configDependencyForcedHeaderCacheEntry{}, false
	}
	inputSnapshot, valid := root.materializedSnapshot()
	if !valid {
		return configDependencyForcedHeaderCacheEntry{}, false
	}
	macroTransform := configDependencyMacroSnapshotChangeSet{}
	macroRequirements := []configDependencyForcedHeaderMacroRequirement{}
	macroTransformReady := false
	if trace := state.forcedHeaderTrace; trace != nil {
		specialization, valid := captureConfigDependencyMacroSpecialization(state, inputSnapshot, snapshot)
		if !valid {
			return configDependencyForcedHeaderCacheEntry{}, false
		}
		macroTransform = specialization.transform
		macroRequirements = specialization.requirements
		macroTransformReady = true
	}
	predefinedReady := root.parent == nil && root.compilerPredefinedSnapshot == inputSnapshot
	return configDependencyForcedHeaderCacheEntry{
		includeResolutions:        slices.Clone(scanner.headerTrace.events),
		includeResolutionsReady:   true,
		inputSnapshot:             inputSnapshot,
		snapshot:                  snapshot,
		inputPredefinedDigest:     root.compilerPredefinedDigest,
		inputPredefinedReady:      predefinedReady,
		macroRequirements:         macroRequirements,
		macroTransform:            macroTransform,
		macroTransformReady:       macroTransformReady,
		symbols:                   slices.Sorted(maps.Keys(symbols)),
		macroDefinitions:          cloneConfigDependencyMacroExpansionDefinitions(macroDefinitions),
		macroRoots:                slices.Sorted(maps.Keys(macroRoots)),
		macroUndefinitions:        macroUndefinitions,
		macroStackMutation:        macroStackMutation,
		sourcePaths:               maps.Clone(scanner.sourcePaths),
		objectPaths:               maps.Clone(scanner.objectPaths),
		queued:                    maps.Clone(scanner.queued),
		queuedPhysical:            maps.Clone(scanner.queuedPhysical),
		conditionalFiles:          maps.Clone(scanner.conditionalFiles),
		conditionalParsed:         cloneConfigDependencyParsedFiles(scanner.conditionalParsed),
		conditionalRepeatGuards:   maps.Clone(scanner.conditionalRepeatGuards),
		repeatedConditionalGuards: maps.Clone(scanner.repeatedConditionalGuards),
		generatedInputs:           generatedInputs,
	}, true
}

func (entry configDependencyForcedHeaderCacheEntry) matchesGeneratedInputs(
	scanner *configDependencyClosureScanner,
) bool {
	if len(entry.generatedInputs) == 0 {
		return true
	}
	// objectFile can invoke the node/profile-specific generated-output resolver.
	// Keep any fail-closed diagnostic on the scratch scanner: a provenance
	// mismatch is a cache miss, after which the ordinary path decides whether the
	// current action is precise or opaque.
	scratch := scanner.headerReplayScanner()
	scratch.opaqueReason = ""
	for logical, expected := range entry.generatedInputs {
		actual, ok := scratch.objectFile(logical)
		if !ok || scratch.opaqueReason != "" || actual != expected {
			return false
		}
	}
	return true
}

func (entry configDependencyForcedHeaderCacheEntry) restore(
	scanner *configDependencyClosureScanner,
	root *configDependencyMacroState,
) (*configDependencyMacroState, bool) {
	if root == nil || root.snapshot == nil || root.tainted || root.consumed ||
		root.macroReplacements != nil ||
		entry.inputSnapshot == nil || entry.snapshot == nil ||
		!configDependencyForcedHeaderCacheScannerPristine(scanner) || !entry.includeResolutionsReady {
		return nil, false
	}
	currentSnapshot, valid := root.materializedSnapshot()
	if !valid {
		return nil, false
	}
	for _, requirement := range entry.macroRequirements {
		cell, found := currentSnapshot.lookup(requirement.name)
		if !found || cell != requirement.cell {
			return nil, false
		}
	}
	if len(entry.includeResolutions) != 0 {
		// A selected source header can be replaced or newly shadowed without
		// changing the forced-file key or any previously selected generated file.
		// Re-run the complete ordered lookup, not just positive closure checks.
		scratch := scanner.headerReplayScanner()
		for _, resolution := range entry.includeResolutions {
			if !resolution.matchesResolution(&scratch) {
				return nil, false
			}
		}
	}
	if !entry.matchesGeneratedInputs(scanner) {
		return nil, false
	}
	restoredSnapshot := (*configDependencyMacroSnapshot)(nil)
	switch {
	case currentSnapshot == entry.inputSnapshot:
		restoredSnapshot = entry.snapshot
	case entry.inputPredefinedReady && root.parent == nil &&
		root.compilerPredefinedSnapshot == currentSnapshot &&
		entry.inputPredefinedDigest == root.compilerPredefinedDigest:
		// Compiler replacement values are retained by the current scanner's
		// macro-expansion graph, but the conditional state models only
		// definedness. Equal canonical name sets therefore make the immutable
		// input namespaces exactly equivalent and permit O(1) snapshot reuse.
		restoredSnapshot = entry.snapshot
	case entry.macroTransformReady:
		restoredSnapshot, valid = entry.macroTransform.materialize(currentSnapshot)
		if !valid {
			return nil, false
		}
	default:
		return nil, false
	}
	if restoredSnapshot == nil {
		return nil, false
	}
	scanner.sourcePaths = maps.Clone(entry.sourcePaths)
	scanner.objectPaths = maps.Clone(entry.objectPaths)
	scanner.queued = maps.Clone(entry.queued)
	scanner.queuedPhysical = maps.Clone(entry.queuedPhysical)
	scanner.conditionalFiles = maps.Clone(entry.conditionalFiles)
	scanner.conditionalParsed = cloneConfigDependencyParsedFiles(entry.conditionalParsed)
	scanner.conditionalRepeatGuards = maps.Clone(entry.conditionalRepeatGuards)
	scanner.repeatedConditionalGuards = maps.Clone(entry.repeatedConditionalGuards)
	scanner.resolvedGeneratedFiles = maps.Clone(entry.generatedInputs)
	if scanner.symbols == nil {
		scanner.symbols = map[string]bool{}
	}
	for _, symbol := range entry.symbols {
		scanner.symbols[symbol] = true
	}
	scanner.macroExpansions.addDefinitions(entry.macroDefinitions...)
	scanner.macroExpansions.addRoots(entry.macroRoots...)
	scanner.macroUndefinitions = maps.Clone(entry.macroUndefinitions)
	scanner.macroStackMutation = entry.macroStackMutation
	// The restored unit is disposable, but it still needs speculative-state
	// behavior: later optional includes must accumulate a sparse overlay instead
	// of rewriting the complete cached forced-header snapshot. A private anchor
	// supplies the ancestry invariant without exposing or mutating the cache.
	anchor := &configDependencyMacroState{snapshot: restoredSnapshot}
	return anchor.branch(), true
}

func (cache *configDependencyForcedHeaderCache) lookup(
	key configDependencyForcedHeaderCacheKey,
	scanner *configDependencyClosureScanner,
	root *configDependencyMacroState,
) (*configDependencyMacroState, bool) {
	if cache == nil {
		return nil, false
	}
	if cache.entries == nil {
		cache.misses++
		return nil, false
	}
	for _, entry := range cache.entries[key] {
		if state, ok := entry.restore(scanner, root); ok {
			cache.hits++
			return state, true
		}
	}
	cache.misses++
	return nil, false
}

func (cache *configDependencyForcedHeaderCache) store(
	key configDependencyForcedHeaderCacheKey,
	entry configDependencyForcedHeaderCacheEntry,
) {
	if cache == nil || !entry.includeResolutionsReady {
		return
	}
	records, bytes := 0, 0
	for _, event := range entry.includeResolutions {
		if event.kind != configDependencyHeaderResolve {
			return
		}
		eventRecords, eventBytes := event.retainedSize()
		records += eventRecords
		bytes += eventBytes
		if records > (1<<18)-cache.resolutionRecords || bytes > (32<<20)-cache.resolutionBytes {
			return
		}
	}
	if cache.entries == nil {
		cache.entries = map[configDependencyForcedHeaderCacheKey][]configDependencyForcedHeaderCacheEntry{}
	}
	cache.entries[key] = append(cache.entries[key], entry)
	cache.resolutionRecords += records
	cache.resolutionBytes += bytes
}

func (s *configDependencyClosureScanner) setOpaque(reason string) {
	if s.opaqueReason == "" {
		s.opaqueReason = reason
	}
}

func (s *configDependencyClosureScanner) conditionalSyntaxCacheKey(file configDependencyScanFile) string {
	key := file.parseIdentity()
	if s.callCoverage != nil {
		key = file.identity() + "\x00" + key + "\x00complete-call-coverage"
	}
	return key
}

func (s *configDependencyClosureScanner) conditionalSyntaxForFile(
	file configDependencyScanFile,
) configDependencyConditionalSyntax {
	s.rememberCompilerGuardFile(file)
	// Exact definition origins keep equal bytes with different generated
	// owners in distinct complete-call programs and weak inventories.
	parseIdentity := s.conditionalSyntaxCacheKey(file)
	if syntax, ok := s.conditionalSyntax[parseIdentity]; ok {
		return syntax
	}
	contents, err := file.contents()
	return s.cacheConditionalSyntaxForContents(file, contents, err)
}

// Syntax caching does not record entry, lookup, macro state or source reads.
// Optional candidate discovery may populate the same complete immutable parse;
// only conditionalSyntaxForFile records a file entered by the real scanner.
func (s *configDependencyClosureScanner) cacheConditionalSyntaxForContents(
	file configDependencyScanFile, contents []byte, err error,
) configDependencyConditionalSyntax {
	parseIdentity := s.conditionalSyntaxCacheKey(file)
	syntax := configDependencyConditionalSyntax{sourceBytes: len(contents)}
	if err != nil {
		syntax.reason = "cannot read conditionally interpreted compiler source closure"
	} else {
		if s.collectCompilerGuards {
			syntax.compilerGuardHints = configDependencyCompilerGuardObservationHintsForContents(contents)
			syntax.compilerGuardHintsReady = true
			syntax.compilerGuardContentID = fmt.Sprintf("%x", sha256.Sum256(contents))
		}
		syntax.symbols = ExtractConfigDependencySymbols(contents)
		syntax.macroDefinitions, syntax.macroRoots = configDependencySourceMacroExpansions(contents)
		syntax.macroUndefinitions, syntax.macroStackMutation = configDependencySourceMacroMutations(contents)
		syntax.conventionalGuard = configDependencyConventionalIncludeGuard(contents)
		if s.callCoverage != nil {
			origin := fmt.Sprintf("%s:%x", file.identity(), sha256.Sum256(contents))
			syntax.conditionalProgram = compileConfigDependencyConditionalProgramWithCalls(contents, origin, true)
		} else {
			syntax.conditionalProgram = compileConfigDependencyConditionalProgram(contents)
		}
		if s.collectCompilerGuards {
			syntax.compilerTokenInventory = configDependencyOpenedHeaderTokenHintInventoryForProgram(len(contents), syntax.conditionalProgram)
			syntax.compilerTokenInventoryReady = true
		}
	}
	if s.conditionalSyntax == nil {
		s.conditionalSyntax = map[string]configDependencyConditionalSyntax{}
	}
	s.conditionalSyntax[parseIdentity] = syntax
	return syntax
}

func (s *configDependencyClosureScanner) queue(file configDependencyScanFile) {
	if s.opaqueReason != "" || file.logical == "" {
		return
	}
	if file.configProjection {
		// The resolved-config capsule owns these object-tree bytes. Record the
		// logical input without trying to scan the full generated header.
		if file.logical != configDependencyAutoconfPath || file.physical != "" || file.source {
			s.setOpaque("invalid virtual resolved-config projection")
			return
		}
		s.objectPaths[file.logical] = true
		return
	}
	if file.macroTable {
		if file.source || file.physical != "" || file.exactIdentity == "" {
			s.setOpaque("invalid validated generated macro-header projection")
			return
		}
		identity := file.identity()
		key := strings.Join([]string{identity, file.logical, "false"}, "\x00")
		if s.queued[key] {
			return
		}
		s.objectPaths[file.logical] = true
		s.queued[key] = true
		s.queuedPhysical[identity] = true
		return
	}
	identity := file.identity()
	if identity == "" {
		return
	}
	if previous, conditional := s.conditionalFiles[identity]; s.queuedPhysical[identity] && conditional {
		guard := s.conditionalRepeatGuards[identity]
		if guard != "" && previous.logical == file.logical && previous.source == file.source {
			if s.repeatedConditionalGuards == nil {
				s.repeatedConditionalGuards = map[string]bool{}
			}
			s.repeatedConditionalGuards[guard] = true
			return
		}
		s.setOpaque("conditionally interpreted compiler source is reachable through an inexact deferred include: " + file.logical)
		return
	}
	key := strings.Join([]string{identity, file.logical, fmt.Sprint(file.source)}, "\x00")
	if s.queued[key] {
		return
	}
	if file.exactIdentity == "" {
		status := s.physicalFiles.status(file.physical)
		if status.err != nil {
			if !os.IsNotExist(status.err) {
				s.setOpaque("cannot stat literal include closure")
			}
			return
		}
		if !status.regular {
			s.setOpaque("literal include closure contains a non-regular file")
			return
		}
	}
	if file.source {
		s.sourcePaths[file.logical] = true
	} else {
		s.objectPaths[file.logical] = true
	}
	s.queued[key] = true
	s.queuedPhysical[identity] = true
	s.pending = append(s.pending, file)
}

func (s *configDependencyClosureScanner) sourceFile(logical string) (configDependencyScanFile, bool) {
	logical = canonicalKbuildRulePath(logical)
	if logical == "" {
		return configDependencyScanFile{}, false
	}
	physical, ok := s.sourceLookup.sourcePath(logical)
	if !ok || !s.physicalFiles.status(physical).exists {
		return configDependencyScanFile{}, false
	}
	return configDependencyScanFile{logical: logical, physical: physical, source: true}, true
}

func (s *configDependencyClosureScanner) boundWorkingInput(
	logical string,
) (configDependencyInputSetProvenance, bool) {
	if s.lookupWorkingInput == nil {
		return configDependencyInputSetProvenance{}, false
	}
	provenance, found, err := s.lookupWorkingInput(logical)
	if err != nil {
		s.setOpaque("cannot resolve persistent compiler working input: " + err.Error())
		return configDependencyInputSetProvenance{}, false
	}
	return provenance, found
}

func (s *configDependencyClosureScanner) objectFile(logical string) (configDependencyScanFile, bool) {
	logical = canonicalKbuildRulePath(logical)
	if logical == "" {
		return configDependencyScanFile{}, false
	}
	bound, explicitlyBound := s.boundWorkingInput(logical)
	if s.opaqueReason != "" {
		return configDependencyScanFile{}, false
	}
	if explicitlyBound && bound.config {
		if bound.projection == configDependencyAutoconfPath {
			return configDependencyScanFile{logical: logical, configProjection: true}, true
		}
		s.setOpaque("compiler object-tree input has non-autoconf config provenance: " + logical)
		return configDependencyScanFile{}, false
	}
	if explicitlyBound && bound.entry.SourceID != "" {
		physical, ok := s.sourceLookup.sourcePath(bound.source.Path)
		if !ok || !s.physicalFiles.status(physical).exists {
			s.setOpaque("persistent compiler source input is unavailable to the planner: " + bound.source.Path)
			return configDependencyScanFile{}, false
		}
		// The source is materialized below the private writable tree at logical.
		// Keep that namespace for quoted-include resolution while reading the
		// immutable source provenance directly.
		return configDependencyScanFile{logical: logical, physical: physical}, true
	}
	generated, exactGenerated := s.generatedText[logical]
	if exactGenerated || s.generated[logical] || explicitlyBound && bound.entry.ProducerID != "" {
		// Snapshot planning can expose the exact evaluated object tree. When it
		// does, inspect those bytes but retain the logical object-tree path so the
		// family reducer keeps the corresponding producer edge. A generated file
		// which is not available to the planner remains an opaque boundary: its
		// contents could introduce further includes or CONFIG_* tokens.
		if !exactGenerated && s.resolveGeneratedText != nil {
			generated, exactGenerated = s.resolveGeneratedText(logical)
			if exactGenerated {
				s.generatedText[logical] = generated
			}
		}
		if exactGenerated {
			file := configDependencyScanFile{
				logical: logical, exactContents: generated.contents, exactIdentity: generated.identity,
				macroTable: generated.macroTable,
			}
			s.recordResolvedGeneratedFile(file)
			return file, true
		}
		physical := s.preconfigured[logical]
		if physical != "" && s.physicalFiles.status(physical).exists {
			file := configDependencyScanFile{logical: logical, physical: physical}
			s.recordResolvedGeneratedFile(file)
			return file, true
		}
		if s.collectGeneratedHeaders {
			s.unavailableGeneratedHeader = configDependencyUnavailableGeneratedHeader{
				logical: logical, bound: bound, explicitlyBound: explicitlyBound,
			}
		}
		s.setOpaque("literal include closure reaches unavailable generated object-tree input " + logical)
		return configDependencyScanFile{}, false
	}
	// External Kbuild sources are copied into a private writable object-tree
	// overlay. Follow its immutable origin for inspection, but keep this file
	// classified as object-tree input so nested quoted includes still notice a
	// generated output shadowing the overlay.
	overlay, err := s.sourceLookup.usesSourceOverlay(logical)
	if err != nil {
		s.setOpaque("cannot resolve source overlay for literal include " + logical)
		return configDependencyScanFile{}, false
	}
	if overlay {
		physical, ok := s.sourceLookup.sourcePath(logical)
		if ok && s.physicalFiles.status(physical).exists {
			return configDependencyScanFile{logical: logical, physical: physical}, true
		}
	}
	physical := s.preconfigured[logical]
	if physical == "" || !s.physicalFiles.status(physical).exists {
		return configDependencyScanFile{}, false
	}
	return configDependencyScanFile{logical: logical, physical: physical}, true
}

func (s *configDependencyClosureScanner) objectFileOrConfigProjection(
	logical string,
) (configDependencyScanFile, bool) {
	logical = canonicalKbuildRulePath(logical)
	if logical == "" {
		return configDependencyScanFile{}, false
	}
	if logical != configDependencyAutoconfPath {
		return s.objectFile(logical)
	}
	bound, found := s.boundWorkingInput(logical)
	if s.opaqueReason != "" {
		return configDependencyScanFile{}, false
	}
	if !found {
		return configDependencyScanFile{logical: logical, configProjection: true}, true
	}
	if bound.config {
		if bound.projection != configDependencyAutoconfPath {
			s.setOpaque("compiler autoconf path has non-autoconf config provenance")
			return configDependencyScanFile{}, false
		}
		return configDependencyScanFile{logical: logical, configProjection: true}, true
	}
	// A concrete persistent binding shadows the normal resolved-config capsule.
	// Inspect its exact source/producer provenance instead of inferring config
	// bytes solely from the destination pathname.
	return s.objectFile(logical)
}

func (s *configDependencyClosureScanner) recordResolvedGeneratedFile(file configDependencyScanFile) {
	if s == nil || file.logical == "" {
		return
	}
	if s.resolvedGeneratedFiles == nil {
		s.resolvedGeneratedFiles = map[string]configDependencyScanFile{}
	}
	s.resolvedGeneratedFiles[file.logical] = file
}

func (s *configDependencyClosureScanner) resolveAt(directory configDependencyIncludeDirectory, name string) (configDependencyScanFile, bool) {
	candidate := path.Clean(path.Join(directory.logical, name))
	if candidate == ".." || strings.HasPrefix(candidate, "../") {
		s.setOpaque("literal include escapes its modeled source/object tree: " + name)
		return configDependencyScanFile{}, false
	}
	if candidate == "." || canonicalKbuildRulePath(candidate) != candidate {
		return configDependencyScanFile{}, false
	}
	if directory.external {
		return configDependencyScanFile{}, false
	}
	// `${tree:kernel}` remains the immutable source tree at runtime. Only an
	// object-rooted include directory names the private staged Kbuild view in
	// which the resolved-config capsule supplies generated/autoconf.h.
	if directory.source {
		return s.sourceFile(candidate)
	}
	if file, ok := s.objectFileOrConfigProjection(candidate); ok || s.opaqueReason != "" {
		return file, ok
	}
	return configDependencyScanFile{}, false
}

func (s *configDependencyClosureScanner) resolveIncludeFile(
	including configDependencyScanFile,
	include configDependencyLiteralInclude,
) (resolved configDependencyScanFile, found bool) {
	if s.headerTrace != nil {
		defer func() {
			if found {
				s.recordHeaderResolution(including, include, resolved)
			}
		}()
	}
	if s.opaqueReason != "" {
		return configDependencyScanFile{}, false
	}
	if include.quoted {
		directory := configDependencyIncludeDirectory{
			logical: path.Dir(including.logical), source: including.source,
		}
		if directory.logical == "." {
			directory.logical = ""
		}
		if file, ok := s.resolveAt(directory, include.name); ok {
			return file, true
		}
		if s.opaqueReason != "" {
			return configDependencyScanFile{}, false
		}
	}
	directoryGroups := [][]configDependencyIncludeDirectory{}
	if include.quoted {
		directoryGroups = append(directoryGroups, s.quoteDirectories)
	}
	// GCC and Clang search include classes in this order regardless of how the
	// different option classes are interleaved in argv. Order within one class
	// remains the original command-line order.
	directoryGroups = append(directoryGroups,
		s.includeDirectories,
		s.systemDirectories,
		s.afterDirectories,
	)
	for _, directories := range directoryGroups {
		for _, directory := range directories {
			if directory.external {
				s.setOpaque("literal include may be shadowed by an uninspectable compiler search root: " + include.name)
				return configDependencyScanFile{}, false
			}
			if file, ok := s.resolveAt(directory, include.name); ok {
				return file, true
			}
			if s.opaqueReason != "" {
				return configDependencyScanFile{}, false
			}
		}
	}
	return configDependencyScanFile{}, false
}

func (s *configDependencyClosureScanner) resolveInclude(including configDependencyScanFile, include configDependencyLiteralInclude) {
	if file, ok := s.resolveIncludeFile(including, include); ok {
		s.queue(file)
	}
}

func (s *configDependencyClosureScanner) appendIncludeDirectory(flag string, directory configDependencyIncludeDirectory) {
	switch flag {
	case "-iquote":
		s.quoteDirectories = append(s.quoteDirectories, directory)
	case "-I":
		s.includeDirectories = append(s.includeDirectories, directory)
	case "-isystem":
		s.systemDirectories = append(s.systemDirectories, directory)
	case "-idirafter":
		s.afterDirectories = append(s.afterDirectories, directory)
	}
}

func (s *configDependencyClosureScanner) queueConditional(file configDependencyScanFile) bool {
	if file.configProjection {
		s.queue(file)
		return s.opaqueReason == ""
	}
	identity := file.identity()
	if identity == "" {
		s.setOpaque("conditionally interpreted compiler source has no exact file identity")
		return false
	}
	if previous, ok := s.conditionalFiles[identity]; ok {
		if previous.logical != file.logical || previous.source != file.source {
			s.setOpaque("conditionally interpreted compiler source reaches one physical file through multiple logical aliases")
			return false
		}
		if !s.conditionalPending[identity] {
			if s.conditionalPending == nil {
				s.conditionalPending = map[string]bool{}
			}
			s.pending = append(s.pending, file)
			s.conditionalPending[identity] = true
		}
		return true
	}
	if s.queuedPhysical[identity] {
		s.setOpaque("conditionally interpreted compiler source aliases another compiler input")
		return false
	}
	s.queue(file)
	if s.opaqueReason != "" || !s.queuedPhysical[identity] {
		return false
	}
	if s.conditionalFiles == nil {
		s.conditionalFiles = map[string]configDependencyScanFile{}
	}
	s.conditionalFiles[identity] = file
	if s.conditionalPending == nil {
		s.conditionalPending = map[string]bool{}
	}
	s.conditionalPending[identity] = true
	return true
}

func mergeConfigDependencyConditionalParsed(
	left configDependencyParsedFile,
	right configDependencyParsedFile,
) configDependencyParsedFile {
	// A skipped guard contributes only its controlling CONFIG symbol. A later
	// visit can execute the body, so retain both summaries rather than assuming
	// every nonempty summary already describes the entire physical file.
	left.symbols = configDependencySortedStringUnion(left.symbols, right.symbols)
	if len(left.macroDefinitions) == 0 {
		left.macroDefinitions = slices.Clone(right.macroDefinitions)
	}
	if len(left.macroRoots) == 0 {
		left.macroRoots = slices.Clone(right.macroRoots)
	}
	// macroUndefinitions is a sorted raw-file summary. Reinterpreting one
	// physical input under another macro state normally supplies the identical
	// slice, so avoid rebuilding and sorting a map while unwinding every nested
	// include. Retain a linear sorted union for defensive callers which merge
	// distinct summaries.
	left.macroUndefinitions = configDependencySortedStringUnion(
		left.macroUndefinitions, right.macroUndefinitions,
	)
	left.macroStackMutation = left.macroStackMutation || right.macroStackMutation
	// Literal include lists are short and preserve preprocessing order. A linear
	// duplicate check avoids allocating a throwaway map on every repeated visit.
	for _, include := range right.includes {
		if !slices.Contains(left.includes, include) {
			left.includes = append(left.includes, include)
		}
	}
	left.includesResolved = left.includesResolved || right.includesResolved
	if left.reason == "" {
		left.reason = right.reason
	}
	return left
}

func configDependencySortedStringUnion(left, right []string) []string {
	if len(left) == 0 {
		return slices.Clone(right)
	}
	if len(right) == 0 || slices.Equal(left, right) || configDependencySortedStringsContainAll(left, right) {
		return left
	}
	merged := make([]string, 0, len(left)+len(right))
	leftIndex, rightIndex := 0, 0
	for leftIndex < len(left) && rightIndex < len(right) {
		switch {
		case left[leftIndex] < right[rightIndex]:
			merged = append(merged, left[leftIndex])
			leftIndex++
		case right[rightIndex] < left[leftIndex]:
			merged = append(merged, right[rightIndex])
			rightIndex++
		default:
			merged = append(merged, left[leftIndex])
			leftIndex++
			rightIndex++
		}
	}
	merged = append(merged, left[leftIndex:]...)
	merged = append(merged, right[rightIndex:]...)
	return merged
}

func configDependencySortedStringsContainAll(superset, subset []string) bool {
	supersetIndex := 0
	for _, value := range subset {
		for supersetIndex < len(superset) && superset[supersetIndex] < value {
			supersetIndex++
		}
		if supersetIndex == len(superset) || superset[supersetIndex] != value {
			return false
		}
	}
	return true
}

// interpretConditionalCompilerFile advances the exact compiler macro namespace
// through one physical forced header, translation unit, or ordinary nested
// include. Literal nested includes are resolved and interpreted synchronously
// in preprocessing order. A virtual autoconf input applies only its emitted
// positive definitions at that exact point; omitted symbols do not erase
// earlier -D or header definitions.
func (s *configDependencyClosureScanner) interpretConditionalCompilerFile(
	file configDependencyScanFile,
	state *configDependencyMacroState,
	autoconfDefinitions configDependencyResolvedAutoconfDefinitions,
	activePhysical map[string]*configDependencyConditionalActiveFile,
) bool {
	if s.opaqueReason != "" || state == nil {
		return false
	}
	s.headerTrace.record(configDependencyHeaderEvent{kind: configDependencyHeaderEnter, file: file})
	if file.configProjection {
		s.queue(file)
		if s.opaqueReason != "" {
			return false
		}
		state.applyResolvedConfigAutoconf(autoconfDefinitions)
		return true
	}
	if file.macroTable {
		s.queue(file)
		if s.opaqueReason != "" {
			return false
		}
		// The producer-owned names and values are unavailable here, but the
		// execution validator proves a much narrower effect than arbitrary macro
		// state: only non-CONFIG literal numeric definitions, with no undefs or
		// file edges. Preserve exact CONFIG and already-defined guard state while
		// making every possibly-added non-CONFIG name unknown.
		state.applyValidatedNumericMacroHeader()
		return true
	}
	if state.tainted {
		s.queue(file)
		return false
	}
	identity := file.identity()
	if identity == "" {
		s.setOpaque("conditionally interpreted compiler source has no exact file identity")
		return false
	}
	syntax := s.conditionalSyntaxForFile(file)
	if syntax.reason != "" {
		s.setOpaque(syntax.reason)
		return false
	}
	if !s.queueConditional(file) {
		return false
	}
	if guard := syntax.conventionalGuard; strings.HasPrefix(guard, "CONFIG_") {
		// Skipping the guarded body still consumes the guard's config value.
		// Record this before either recursion or defined-guard shortcuts, using
		// a parsed event so ordinary-header cache replay retains the same read.
		symbol := normalizeConfigDependencySymbol(guard)
		if !isConfigKey(symbol) {
			s.setOpaque("compiler source uses an invalid CONFIG include guard")
			return false
		}
		s.mergeConditionalParsed(file, configDependencyParsedFile{symbols: []string{symbol}})
	}
	active := activePhysical[identity]
	var entryParent *configDependencyMacroState
	if active != nil {
		// A conventional guard is defined before any nested directive in the
		// recognized whole-file shape. Re-entering that same active file is
		// therefore an exact no-op when the current namespace proves the guard
		// defined, without serializing the otherwise irrelevant namespace.
		guard := syntax.conventionalGuard
		if guard != "" && state.definition(guard) == configDependencyMacroDefined {
			return true
		}
		if active.entryState == nil {
			s.setOpaque("conditionally interpreted compiler source recursion has tainted or unknown macro-state progress")
			return false
		}
		if len(active.projections) == 0 {
			entryProjection, untainted := configDependencyConditionalMacroStateKey(active.entryState)
			if !untainted {
				s.setOpaque("conditionally interpreted compiler source recursion has tainted or unknown macro-state progress")
				return false
			}
			active.states = map[string]bool{entryProjection.key: true}
			active.projections = []configDependencyConditionalMacroStateProjection{entryProjection}
		}
		projection, untainted := configDependencyConditionalMacroStateKey(state)
		if !untainted {
			s.setOpaque("conditionally interpreted compiler source recursion has tainted or unknown macro-state progress")
			return false
		}
		if active.states[projection.key] {
			s.setOpaque("conditionally interpreted compiler source closure contains a recursive include with repeated macro state")
			return false
		}
		if len(active.projections) == 0 || !configDependencyConditionalMacroStateProgress(
			active.projections[len(active.projections)-1], projection,
		) {
			s.setOpaque("conditionally interpreted compiler source recursion has tainted or unknown macro-state progress")
			return false
		}
		if len(active.states) >= configDependencyMaxConditionalActiveStatesPerFile {
			s.setOpaque("conditionally interpreted compiler source recursive include exceeds the macro-state progress bound")
			return false
		}
		active.states[projection.key] = true
		active.projections = append(active.projections, projection)
		defer func() {
			delete(active.states, projection.key)
			active.projections = active.projections[:len(active.projections)-1]
		}()
	} else {
		active = &configDependencyConditionalActiveFile{}
		if syntax.conventionalGuard == "" {
			// Keep the complete compiler/config namespace immutable and record this
			// file's effects in a child delta. Most unguarded files are acyclic, so
			// their entry namespace never needs an O(namespace) canonical projection.
			entryParent = state
			active.entryState = entryParent
			state = entryParent.branch()
		}
		activePhysical[identity] = active
		defer delete(activePhysical, identity)
	}

	interpretContents := func(current *configDependencyMacroState) configDependencyParsedFile {
		parsed := configDependencyParsedFile{
			symbols:            syntax.symbols,
			macroDefinitions:   syntax.macroDefinitions,
			macroRoots:         syntax.macroRoots,
			macroUndefinitions: syntax.macroUndefinitions,
			macroStackMutation: syntax.macroStackMutation,
			includesResolved:   true,
		}
		// These are the same whole-file summaries scan will eventually union.
		// Their expansion graph only grows: a reachable unsupported effect can
		// never become safe after interpreting more includes. Check it before
		// paying for nested conditional macro-state interpretation. In particular,
		// do not add a guarded body's summary on the proven-skipped path.
		if s.callCoverage == nil && !syntax.conditionalProgram.macroStack {
			s.checkEarlyMacroSummary(file, parsed)
		}
		if s.opaqueReason != "" {
			parsed.reason = s.opaqueReason
			return parsed
		}
		var textEffect func(string, *configDependencyMacroState) string
		var conditionEffect func(string, *configDependencyMacroState) (configDependencyMacroDefinition, string)
		var macroWriteEffect func(string) string
		if s.callCoverage != nil {
			// Complete expansion observes actual CONFIG reads, including control
			// flow. Whole-file lexical names also include raw stringified operands,
			// unused definitions and dead branches, which are not compiler inputs.
			// No narrowed result escapes unless the entire closure succeeds.
			parsed.symbols = nil
			macroWriteEffect = s.callCoverage.macroWrite
			textEffect = func(text string, current *configDependencyMacroState) string {
				previous := s.compilerIntrinsicFile
				s.compilerIntrinsicFile = file
				defer func() { s.compilerIntrinsicFile = previous }()
				return s.callCoverage.expand(text, current)
			}
			conditionEffect = func(text string, current *configDependencyMacroState) (configDependencyMacroDefinition, string) {
				previous := s.compilerIntrinsicFile
				s.compilerIntrinsicFile = file
				defer func() { s.compilerIntrinsicFile = previous }()
				return s.callCoverage.condition(text, current)
			}
		}
		parsed.includes, parsed.reason = interpretConfigDependencyConditionalProgramWithCalls(
			syntax.conditionalProgram,
			current,
			func(include configDependencyLiteralInclude, active configDependencyMacroDefinition) (bool, string) {
				included, ok := s.resolveIncludeFile(file, include)
				if !ok {
					if s.opaqueReason != "" {
						return false, s.opaqueReason
					}
					return false, "literal include is unavailable to the conditional compiler-source interpreter: " + include.name
				}
				if current.tainted {
					s.queue(included)
					return false, s.opaqueReason
				}
				if active == configDependencyMacroDefined {
					exact := s.interpretDirectCompilerHeader(included, current, autoconfDefinitions, activePhysical)
					return exact, s.opaqueReason
				}
				// The including conditional may or may not execute. Interpret the
				// include on one branch, then join it with the no-include state. This
				// retains exact unrelated names (notably n-valued CONFIG symbols) while
				// marking every actual disagreement unknown.
				includedState := current.branch()
				exact := s.interpretConditionalCompilerFile(included, includedState, autoconfDefinitions, activePhysical)
				if !exact {
					return false, s.opaqueReason
				}
				current.mergePossibleBranch(includedState)
				return !current.tainted, ""
			},
			s.language,
			textEffect,
			conditionEffect,
			macroWriteEffect,
		)
		return parsed
	}

	guard := syntax.conventionalGuard
	parsed := configDependencyParsedFile{
		macroUndefinitions: syntax.macroUndefinitions,
		macroStackMutation: syntax.macroStackMutation,
		includesResolved:   true,
	}
	if guard == "" {
		parsed = interpretContents(state)
	} else {
		guardState := state.definition(guard)
		switch guardState {
		case configDependencyMacroDefined:
			// The compiler still opens the file, but its complete guarded body is
			// skipped and contributes no macro/include effects.
		case configDependencyMacroUndefined:
			parsed = interpretContents(state)
		default:
			// Split the unknown initial guard state. The skipped path assumes the
			// guard was defined; the executing path assumes it was undefined and
			// interprets the entire guarded body. Their join is exact and, for a
			// conventional guard which is not later undone, proves the post-state
			// guard defined on both paths.
			if s.callCoverage != nil {
				s.setOpaque("call coverage has an unresolved initial include guard " + guard)
				return false
			}
			skippedState := state.branch()
			skippedState.set(guard, configDependencyMacroDefined)
			executedState := state.branch()
			executedState.set(guard, configDependencyMacroUndefined)
			parsed = interpretContents(executedState)
			state.mergePossibleBranches(skippedState, executedState)
		}
		if !state.tainted && state.definition(guard) == configDependencyMacroDefined {
			s.recordConditionalRepeatGuard(file, guard)
		}
	}
	if parsed.reason != "" {
		s.setOpaque(parsed.reason + " in " + file.logical)
	}
	s.mergeConditionalParsed(file, parsed)
	if entryParent != nil {
		exact := s.opaqueReason == "" && !state.tainted
		if exact {
			if !entryParent.commitBranch(state) {
				s.setOpaque("conditionally interpreted compiler source entry-state commit is invalid")
				return false
			}
		} else if state.tainted {
			// Direct interpretation historically tainted the caller namespace. Keep
			// that fail-closed behavior even though the precise child delta is not
			// committed after an inexact traversal.
			entryParent.tainted = true
		}
		state = entryParent
	}
	return s.opaqueReason == "" && !state.tainted
}

// interpretConditionalCompilerFiles advances one preprocessing namespace
// through an ordered sequence of compiler inputs. Callers use one fresh state
// per translation unit: command-line forced headers execute before that unit,
// while definitions and undefs in the unit and its ordinary include closure
// remain visible in exact preprocessing order.
func (s *configDependencyClosureScanner) interpretConditionalCompilerFiles(
	files []configDependencyScanFile,
	state *configDependencyMacroState,
	autoconfDefinitions configDependencyResolvedAutoconfDefinitions,
) {
	activePhysical := map[string]*configDependencyConditionalActiveFile{}
	for _, file := range files {
		if state == nil || state.tainted {
			s.queue(file)
			continue
		}
		s.interpretConditionalCompilerFile(file, state, autoconfDefinitions, activePhysical)
		if s.opaqueReason != "" {
			return
		}
	}
}

func (s *configDependencyClosureScanner) scan() ConfigDependencySet {
	for len(s.pending) != 0 && s.opaqueReason == "" {
		file := s.pending[0]
		s.pending = s.pending[1:]
		identity := file.identity()
		delete(s.conditionalPending, identity)
		parsed, ok := s.conditionalParsed[identity]
		if !ok {
			if s.callCoverage != nil {
				s.setOpaque("call coverage has an uninterpreted source input")
				break
			}
			parseIdentity := configDependencyParsedFileCacheKey(file, s.language)
			parsed, ok = s.parsed[parseIdentity]
			if !ok {
				contents, err := file.contents()
				if err != nil {
					s.setOpaque("cannot read literal include closure")
					break
				}
				parsed.symbols = ExtractConfigDependencySymbols(contents)
				// A hand-constructed plan without an execution-probed macro table can
				// still prove literal #if 0/#if 1 regions. Every identifier-dependent
				// condition remains unknown, so active or possibly-active assembler file
				// directives fail closed while comments and proven-dead regions do not.
				unknownState := &configDependencyMacroState{tainted: true}
				parsed.includes, parsed.reason = interpretConfigDependencyConditionalProgram(
					compileConfigDependencyConditionalProgram(contents), unknownState, nil, s.language,
				)
				parsed.macroDefinitions, parsed.macroRoots = configDependencySourceMacroExpansions(contents)
				parsed.macroUndefinitions, parsed.macroStackMutation = configDependencySourceMacroMutations(contents)
				s.parsed[parseIdentity] = parsed
			}
		}
		if s.macroUndefinitions == nil {
			s.macroUndefinitions = map[string]bool{}
		}
		for _, name := range parsed.macroUndefinitions {
			s.macroUndefinitions[name] = true
		}
		s.macroStackMutation = s.macroStackMutation || parsed.macroStackMutation
		s.macroExpansions.addDefinitions(parsed.macroDefinitions...)
		s.macroExpansions.addRoots(parsed.macroRoots...)
		for _, symbol := range parsed.symbols {
			s.symbols[symbol] = true
		}
		if parsed.reason != "" {
			s.setOpaque(parsed.reason + " in " + file.logical)
			break
		}
		if !parsed.includesResolved {
			for _, include := range parsed.includes {
				s.resolveInclude(file, include)
				if s.opaqueReason != "" {
					break
				}
			}
		}
	}
	if s.opaqueReason == "" {
		if len(s.repeatedConditionalGuards) != 0 && s.macroStackMutation {
			s.setOpaque("compiler source closure may invalidate a repeated forced-header guard through the macro stack")
		}
	}
	if s.opaqueReason == "" {
		for _, guard := range slices.Sorted(maps.Keys(s.repeatedConditionalGuards)) {
			if s.macroUndefinitions[guard] {
				s.setOpaque("compiler source closure may undefine repeated forced-header guard " + guard)
				break
			}
		}
	}
	if s.callCoverage == nil {
		s.checkMacroExpansionHazards()
	} else if s.callCoverage.reason != "" || !s.callCoverage.complete {
		s.setOpaque("translation-unit call coverage is incomplete: " + s.callCoverage.reason)
	} else {
		for name := range s.callCoverage.reads {
			s.symbols[name] = true
		}
		for _, read := range s.callCoverage.conditions {
			if strings.HasPrefix(read.Name, "CONFIG_") {
				symbol := normalizeConfigDependencySymbol(read.Name)
				if !isConfigKey(symbol) {
					s.setOpaque("conditional call coverage produced an invalid CONFIG identifier")
					break
				}
				s.symbols[symbol] = true
			}
		}
	}
	if s.opaqueReason != "" {
		return opaqueConfigDependency(s.opaqueReason)
	}
	symbols := make([]string, 0, len(s.symbols))
	for symbol := range s.symbols {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	sourcePaths := slices.Sorted(maps.Keys(s.sourcePaths))
	objectPaths := slices.Sorted(maps.Keys(s.objectPaths))
	return ConfigDependencySet{
		Symbols: symbols, SourcePaths: sourcePaths, ObjectPaths: objectPaths,
		refineMacroCalls: s.callCoverage == nil && len(symbols) != 0 && s.macroExpansions.reachableStringification() != "",
	}
}

func (s *configDependencyClosureScanner) checkMacroExpansionHazards() {
	if s.opaqueReason != "" {
		return
	}
	if s.macroExpansions.reachableIdentifier("_Pragma") {
		s.setOpaque("compiler source closure reaches an unmodeled _Pragma effect")
	} else if s.macroExpansions.reachableIdentifier("__pragma") {
		s.setOpaque("compiler source closure reaches an unmodeled __pragma effect")
	} else if macro := s.macroExpansions.reachableTokenPaste(); macro != "" {
		s.setOpaque("compiler source closure dynamically constructs a CONFIG_* identifier through reachable token pasting macro " + macro)
	}
}

func (s *configDependencyClosureScanner) checkEarlyMacroSummary(file configDependencyScanFile, parsed configDependencyParsedFile) {
	if s.callCoverage != nil || s.opaqueReason != "" || len(parsed.macroDefinitions) == 0 && len(parsed.macroRoots) == 0 {
		return
	}
	identity := file.identity()
	if s.earlyMacroSummaries[identity] {
		return
	}
	if s.earlyMacroSummaries == nil {
		s.earlyMacroSummaries = map[string]bool{}
	}
	s.earlyMacroSummaries[identity] = true
	s.macroExpansions.addDefinitions(parsed.macroDefinitions...)
	s.macroExpansions.addRoots(parsed.macroRoots...)
	s.checkMacroExpansionHazards()
}

// ActionPlanConfigDependencyAnalysis retains one annotation per structural
// node witness. ByNodeID can expose the same statements under provisional or
// final IDs, and before or after node sorting, without storing configuration
// metadata in ActionPlanNode.
type ActionPlanConfigDependencyAnalysis struct {
	sets                     []ConfigDependencySet
	witnesses                []configDependencyNodeWitness
	generatedHeaderDemands   *configDependencyGeneratedHeaderDemandCollector
	prospectiveHeaderDemands *configDependencyGeneratedHeaderDemandCollector
	observedHeaderUses       *configDependencyObservedHeaderUses
}

// ConfigDependencyGeneratedHeaderDemand identifies one ordinary output which
// the existing compiler scanner encountered but could not inspect as exact bytes.
// Numeric-summary demands are exact bound scheduling candidates for opaque
// compilers, not a promise of precision or a complete generated-file closure.
type ConfigDependencyGeneratedHeaderDemand struct {
	ConsumerNodeID string
	ProducerNodeID string
	Slot           int
	Tree           string
	Path           string
	ArtifactPath   string
	LogicalPath    string
}

// ConfigDependencyGeneratedHeaderDemandCollection is an encountered frontier.
// Truncated discards the entire frontier: callers must conservatively stop,
// never infer that no further generated inputs are required.
type ConfigDependencyGeneratedHeaderDemandCollection struct {
	Enabled   bool
	Truncated bool
	Demands   []ConfigDependencyGeneratedHeaderDemand

	// Scheduling-only numeric summaries remain a separate tier through family
	// admission. They must never consume or displace the unavailable frontier.
	prospective          []ConfigDependencyGeneratedHeaderDemand
	prospectiveTruncated bool
}

type configDependencyUnavailableGeneratedHeader struct {
	logical         string
	bound           configDependencyInputSetProvenance
	explicitlyBound bool
}

type configDependencyGeneratedHeaderDemandWitness struct {
	consumer, producer [sha256.Size]byte
}

type configDependencyGeneratedHeaderDemandCollector struct {
	records                      map[ConfigDependencyGeneratedHeaderDemand]configDependencyGeneratedHeaderDemandWitness
	bytes                        int
	truncated                    bool
	maximumRecords, maximumBytes int
}

func (c *configDependencyGeneratedHeaderDemandCollector) record(
	consumer, producer ActionPlanNode, slot int, logical string,
) {
	if c == nil || c.truncated || consumer.ID == "" || producer.ID == "" ||
		slot < 0 || slot >= len(producer.Outputs) {
		return
	}
	output := producer.Outputs[slot]
	if output.ObservedPath != "" || output.Tree == "" || output.Path == "" ||
		validatePlanRelativePath("generated-header demand", logical) != nil {
		return
	}
	demand := ConfigDependencyGeneratedHeaderDemand{
		ConsumerNodeID: strings.Clone(consumer.ID), ProducerNodeID: strings.Clone(producer.ID), Slot: slot,
		Tree: strings.Clone(output.Tree), Path: strings.Clone(output.Path),
		ArtifactPath: strings.Clone(actionPlanOutputArtifactPath(output)), LogicalPath: strings.Clone(logical),
	}
	if _, exists := c.records[demand]; exists {
		return
	}
	maximumRecords, maximumBytes := c.maximumRecords, c.maximumBytes
	if maximumRecords <= 0 {
		maximumRecords = 4096
	}
	if maximumBytes <= 0 {
		maximumBytes = 4 << 20
	}
	size := 256 + len(demand.ConsumerNodeID) + len(demand.ProducerNodeID) +
		len(demand.Tree) + len(demand.Path) + len(demand.ArtifactPath) + len(demand.LogicalPath)
	if len(c.records) >= maximumRecords || size > maximumBytes-c.bytes {
		// All-or-nothing overflow is deterministic under traversal permutation
		// and does not retain an unbounded set merely to deduplicate later reads.
		c.truncated, c.records, c.bytes = true, nil, 0
		return
	}
	if c.records == nil {
		c.records = map[ConfigDependencyGeneratedHeaderDemand]configDependencyGeneratedHeaderDemandWitness{}
	}
	c.records[demand] = configDependencyGeneratedHeaderDemandWitness{}
	c.bytes += size
}

func configDependencyGeneratedHeaderNodeKey(
	plan *ActionPlan, node ActionPlanNode, witnesses *configDependencyWitnessBuilder,
) ([sha256.Size]byte, error) {
	witness, err := witnesses.witness(node)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	// The ordinary witness handles ID renaming in direct/persistent edges.
	// Bind the actual recipe value and complete output vector as well: observed
	// state must never acquire ordinary-file authority beneath a stable ID.
	data, err := json.Marshal(struct {
		Fields  []string
		Recipe  ActionRecipe
		Outputs []ActionPlanOutput
	}{
		Fields: []string{witness.stage, witness.kind, witness.recipe, witness.tool, witness.product,
			witness.sources, witness.inputs, witness.inputSet, witness.trees, witness.outputs},
		Recipe: plan.Recipes[node.Recipe], Outputs: node.Outputs,
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (c *configDependencyGeneratedHeaderDemandCollector) seal(
	plan *ActionPlan, witnesses *configDependencyWitnessBuilder,
) error {
	if c.truncated || len(c.records) == 0 {
		return nil
	}
	plan.ensureNodeLookupIndexes()
	for demand, record := range c.records {
		consumer, consumerFound := plan.nodesByID[demand.ConsumerNodeID]
		producer, producerFound := plan.nodesByID[demand.ProducerNodeID]
		if !consumerFound || !producerFound {
			return fmt.Errorf("generated-header demand references an absent action")
		}
		var err error
		record.consumer, err = configDependencyGeneratedHeaderNodeKey(plan, consumer, witnesses)
		if err != nil {
			return err
		}
		record.producer, err = configDependencyGeneratedHeaderNodeKey(plan, producer, witnesses)
		if err != nil {
			return err
		}
		c.records[demand] = record
	}
	return nil
}

// GeneratedHeaderDemandsByNodeID returns a defensive frontier using this plan's
// current IDs, before/after content addressing and sorting. Rebinding refuses
// ambiguous distinct-node witnesses rather than choosing a same-path producer.
// This is optimization evidence; the later original-lowering/execution receipt
// must still validate complete producer inputs and immutable runtime bindings.
func (a *ActionPlanConfigDependencyAnalysis) GeneratedHeaderDemandsByNodeID(
	plan *ActionPlan,
) (ConfigDependencyGeneratedHeaderDemandCollection, error) {
	if _, err := a.ByNodeID(plan); err != nil {
		return ConfigDependencyGeneratedHeaderDemandCollection{}, err
	}
	c := a.generatedHeaderDemands
	if c == nil {
		return ConfigDependencyGeneratedHeaderDemandCollection{}, nil
	}
	result := ConfigDependencyGeneratedHeaderDemandCollection{Enabled: true, Truncated: c.truncated}
	prospective := a.prospectiveHeaderDemands
	if prospective != nil {
		result.prospectiveTruncated = prospective.truncated
	}
	collectors := []*configDependencyGeneratedHeaderDemandCollector{c, prospective}
	if c.truncated || len(c.records) == 0 && (prospective == nil || len(prospective.records) == 0) {
		return result, nil
	}
	witnesses, err := newConfigDependencyWitnessBuilder(plan)
	if err != nil {
		return ConfigDependencyGeneratedHeaderDemandCollection{}, err
	}
	wanted := map[[sha256.Size]byte]bool{}
	for _, collector := range collectors {
		if collector == nil || collector.truncated {
			continue
		}
		for _, record := range collector.records {
			wanted[record.consumer], wanted[record.producer] = true, true
		}
	}
	ids := map[[sha256.Size]byte]string{}
	for _, node := range plan.Nodes {
		key, err := configDependencyGeneratedHeaderNodeKey(plan, node, witnesses)
		if err != nil {
			return ConfigDependencyGeneratedHeaderDemandCollection{}, err
		}
		if !wanted[key] {
			continue
		}
		if previous := ids[key]; previous != "" && previous != node.ID {
			return ConfigDependencyGeneratedHeaderDemandCollection{}, fmt.Errorf("generated-header demand has ambiguous action witness")
		}
		ids[key] = node.ID
	}
	for tier, collector := range collectors {
		if collector == nil || collector.truncated {
			continue
		}
		var rebound []ConfigDependencyGeneratedHeaderDemand
		for demand, record := range collector.records {
			consumerID, producerID := ids[record.consumer], ids[record.producer]
			if consumerID == "" || producerID == "" {
				return ConfigDependencyGeneratedHeaderDemandCollection{}, fmt.Errorf("generated-header demand no longer matches action plan")
			}
			demand.ConsumerNodeID, demand.ProducerNodeID = consumerID, producerID
			if tier != 0 {
				plan.ensureNodeLookupIndexes()
				producer := plan.nodesByID[producerID]
				recipe := plan.Recipes[producer.Recipe]
				// Structural witnesses do not include configured role metadata.
				// Recheck it before handing scheduling evidence to snapshots,
				// which deliberately omit that metadata.
				if !configDependencyGeneratedOutputHasExactToolContract(plan, producer, recipe) {
					return ConfigDependencyGeneratedHeaderDemandCollection{}, fmt.Errorf("numeric header demand changed configured helper contract")
				}
			}
			rebound = append(rebound, demand)
		}
		sort.Slice(rebound, func(i, j int) bool {
			left, right := rebound[i], rebound[j]
			if left.ConsumerNodeID != right.ConsumerNodeID {
				return left.ConsumerNodeID < right.ConsumerNodeID
			}
			if left.ProducerNodeID != right.ProducerNodeID {
				return left.ProducerNodeID < right.ProducerNodeID
			}
			if left.Slot != right.Slot {
				return left.Slot < right.Slot
			}
			if left.Tree != right.Tree {
				return left.Tree < right.Tree
			}
			if left.Path != right.Path {
				return left.Path < right.Path
			}
			if left.ArtifactPath != right.ArtifactPath {
				return left.ArtifactPath < right.ArtifactPath
			}
			return left.LogicalPath < right.LogicalPath
		})
		if tier == 0 {
			result.Demands = rebound
		} else {
			result.prospective = rebound
		}
	}
	return result, nil
}

// ConfigDependencyObservedHeaderUse is a precise consumer's actually reached
// ordinary generated input. Content identity does not replace producer authority:
// the final cut gate must still validate the original lowering before substitution.
type ConfigDependencyObservedHeaderUse struct {
	ConfigDependencyGeneratedHeaderDemand
	OriginalProducerNodeID string
	ContentID              string
}

type configDependencyObservedOutputKey struct {
	producer string
	slot     int
}

type configDependencyObservedHeaderProjection struct {
	text       configDependencyGeneratedText
	producer   ActionPlanNode
	output     ActionPlanOutput
	originalID string
	contentID  string
}

type configDependencyObservedHeaderUses struct {
	witnesses configDependencyGeneratedHeaderDemandCollector
	records   map[ConfigDependencyGeneratedHeaderDemand]ConfigDependencyObservedHeaderUse
}

type configDependencyObservedHeaders struct {
	replay      *ActionPlanFamilyVerifiedReplay
	projections map[configDependencyObservedOutputKey]configDependencyObservedHeaderProjection
	identities  map[string]configDependencyObservedHeaderProjection
	pending     map[ConfigDependencyObservedHeaderUse]bool
	uses        configDependencyObservedHeaderUses
	err         error
}

func newConfigDependencyObservedHeaders(
	plan *ActionPlan, replay *ActionPlanFamilyVerifiedReplay, observed *ActionPlanFamilyObservedHeaders,
) (*configDependencyObservedHeaders, error) {
	if replay == nil || observed == nil || replay.cutID == "" || replay.cutID != observed.cutID {
		return nil, fmt.Errorf("observed dependency analysis requires the same verified execution cut")
	}
	if err := replay.validateCurrent(plan); err != nil {
		return nil, err
	}
	headers := map[configDependencyObservedOutputKey]ActionPlanFamilyObservedHeader{}
	contents := map[string]string{}
	for _, header := range observed.Headers() {
		if header.Output.ObservedPath != "" || configDependencyCompletedResolvedProjection(header.Output.Path) {
			return nil, fmt.Errorf("observed dependency analysis cannot replace config projections or observed-state slots")
		}
		key := configDependencyObservedOutputKey{header.NodeID, header.Slot}
		if _, duplicate := headers[key]; duplicate {
			return nil, fmt.Errorf("observed dependency analysis repeats an executed output")
		}
		headers[key] = header
		if _, exists := contents[header.ContentID]; !exists {
			content, found := observed.Content(header.ContentID)
			if !found || observedHeaderContentID(content, header.ExecutableMode) != header.ContentID {
				return nil, fmt.Errorf("observed dependency analysis has inconsistent authenticated contents")
			}
			contents[header.ContentID] = string(content)
		}
	}
	result := &configDependencyObservedHeaders{
		replay: replay, projections: map[configDependencyObservedOutputKey]configDependencyObservedHeaderProjection{},
		identities: map[string]configDependencyObservedHeaderProjection{},
		uses: configDependencyObservedHeaderUses{
			witnesses: configDependencyGeneratedHeaderDemandCollector{maximumRecords: 1 << 16, maximumBytes: 64 << 20},
			records:   map[ConfigDependencyGeneratedHeaderDemand]ConfigDependencyObservedHeaderUse{},
		},
	}
	for _, node := range plan.Nodes {
		executedID := replay.executedIDs[node.ID]
		if executedID == "" {
			continue
		}
		if !replay.opaqueNodes[node.ID] || replay.originalIDs[node.ID] == "" {
			return nil, fmt.Errorf("observed dependency analysis has an unpinned producer")
		}
		for slot, output := range node.Outputs {
			header, observed := headers[configDependencyObservedOutputKey{executedID, slot}]
			if !observed {
				continue
			}
			// Family reduction may relocate ArtifactPath, but may not change
			// the ordinary original slot's tree or logical output destination.
			if output.Tree != header.Output.Tree || output.Path != header.Output.Path ||
				output.ObservedPath != "" || configDependencyCompletedResolvedProjection(output.Path) {
				return nil, fmt.Errorf("observed output does not match the original producer slot")
			}
			identity := "observed-header:" + node.ID + ":" + planOrdinal(slot) + ":" + header.ContentID
			projection := configDependencyObservedHeaderProjection{
				text: configDependencyGeneratedText{
					contents: contents[header.ContentID], identity: identity, producerID: node.ID, slot: slot,
				},
				producer: node, output: output, originalID: replay.originalIDs[node.ID], contentID: header.ContentID,
			}
			result.projections[configDependencyObservedOutputKey{node.ID, slot}] = projection
			result.identities[identity] = projection
		}
	}
	return result, nil
}

func (o *configDependencyObservedHeaders) readResolved(consumer ActionPlanNode, files map[string]configDependencyScanFile) {
	if o == nil || o.err != nil {
		return
	}
	for _, file := range files {
		projection, observed := o.identities[file.exactIdentity]
		if !observed {
			if strings.HasPrefix(file.exactIdentity, "observed-header:") {
				o.err = fmt.Errorf("observed scanner result lost authenticated producer identity")
			}
			continue
		}
		if file.source || file.configProjection || file.macroTable || file.exactContents != projection.text.contents {
			o.err = fmt.Errorf("observed scanner result changed authenticated contents")
			return
		}
		if o.pending == nil {
			o.pending = map[ConfigDependencyObservedHeaderUse]bool{}
		}
		use := ConfigDependencyObservedHeaderUse{
			ConfigDependencyGeneratedHeaderDemand: ConfigDependencyGeneratedHeaderDemand{
				ConsumerNodeID: consumer.ID, ProducerNodeID: projection.producer.ID, Slot: projection.text.slot,
				Tree: projection.output.Tree, Path: projection.output.Path,
				ArtifactPath: actionPlanOutputArtifactPath(projection.output), LogicalPath: file.logical,
			},
			OriginalProducerNodeID: projection.originalID, ContentID: projection.contentID,
		}
		if !o.pending[use] && len(o.pending) >= o.uses.witnesses.maximumRecords {
			o.err = fmt.Errorf("observed compiler read frontier exceeds its bounded budget")
			return
		}
		o.pending[use] = true
	}
}

func (o *configDependencyObservedHeaders) commitUses(consumer ActionPlanNode, set ConfigDependencySet) error {
	if o == nil {
		return nil
	}
	defer func() { o.pending = nil }()
	if o.err != nil {
		return o.err
	}
	if set.Opaque {
		return nil
	}
	paths := map[string]bool{}
	for _, pathname := range set.ObjectPaths {
		paths[pathname] = true
	}
	for use := range o.pending {
		if !paths[use.LogicalPath] {
			continue
		}
		projection := o.projections[configDependencyObservedOutputKey{use.ProducerNodeID, use.Slot}]
		o.uses.witnesses.record(consumer, projection.producer, use.Slot, use.LogicalPath)
		if o.uses.witnesses.truncated {
			return fmt.Errorf("precise observed-header use metadata exceeds its bounded budget")
		}
		if _, recorded := o.uses.witnesses.records[use.ConfigDependencyGeneratedHeaderDemand]; !recorded {
			return fmt.Errorf("precise observed-header use cannot retain its original producer witness")
		}
		// Do not retain substrings backed by complete source or recipe text.
		use.ConsumerNodeID = strings.Clone(use.ConsumerNodeID)
		use.ProducerNodeID = strings.Clone(use.ProducerNodeID)
		use.Tree = strings.Clone(use.Tree)
		use.Path = strings.Clone(use.Path)
		use.ArtifactPath = strings.Clone(use.ArtifactPath)
		use.LogicalPath = strings.Clone(use.LogicalPath)
		use.OriginalProducerNodeID = strings.Clone(use.OriginalProducerNodeID)
		use.ContentID = strings.Clone(use.ContentID)
		o.uses.records[use.ConfigDependencyGeneratedHeaderDemand] = use
	}
	return nil
}

// ObservedHeaderUsesByNodeID returns defensive, sorted uses only for successful
// precise consumers, rebound with the same ambiguity checks as header demands.
// An ordinary analysis returns no uses. These receipts alone do not authorize
// graph rewriting or skipping execution.
func (a *ActionPlanConfigDependencyAnalysis) ObservedHeaderUsesByNodeID(plan *ActionPlan) ([]ConfigDependencyObservedHeaderUse, error) {
	if _, err := a.ByNodeID(plan); err != nil {
		return nil, err
	}
	if a.observedHeaderUses == nil || len(a.observedHeaderUses.records) == 0 {
		return nil, nil
	}
	uses := a.observedHeaderUses
	proxy := &ActionPlanConfigDependencyAnalysis{
		sets: a.sets, witnesses: a.witnesses, generatedHeaderDemands: &uses.witnesses,
	}
	rebound, err := proxy.GeneratedHeaderDemandsByNodeID(plan)
	if err != nil {
		return nil, err
	}
	type useKey struct {
		witness configDependencyGeneratedHeaderDemandWitness
		output  ConfigDependencyGeneratedHeaderDemand
	}
	byWitness := map[useKey]ConfigDependencyObservedHeaderUse{}
	for demand, use := range uses.records {
		witness := uses.witnesses.records[demand]
		demand.ConsumerNodeID, demand.ProducerNodeID = "", ""
		key := useKey{witness, demand}
		if previous, exists := byWitness[key]; exists && previous != use {
			return nil, fmt.Errorf("observed-header use has ambiguous original receipt")
		}
		byWitness[key] = use
	}
	witnesses, err := newConfigDependencyWitnessBuilder(plan)
	if err != nil {
		return nil, err
	}
	plan.ensureNodeLookupIndexes()
	keys := map[string][sha256.Size]byte{}
	keyForID := func(id string) ([sha256.Size]byte, error) {
		if key, exists := keys[id]; exists {
			return key, nil
		}
		key, err := configDependencyGeneratedHeaderNodeKey(plan, plan.nodesByID[id], witnesses)
		if err == nil {
			keys[id] = key
		}
		return key, err
	}
	result := make([]ConfigDependencyObservedHeaderUse, 0, len(rebound.Demands))
	for _, demand := range rebound.Demands {
		consumer, err := keyForID(demand.ConsumerNodeID)
		if err != nil {
			return nil, err
		}
		producer, err := keyForID(demand.ProducerNodeID)
		if err != nil {
			return nil, err
		}
		output := demand
		output.ConsumerNodeID, output.ProducerNodeID = "", ""
		use, exists := byWitness[useKey{configDependencyGeneratedHeaderDemandWitness{consumer, producer}, output}]
		if !exists {
			return nil, fmt.Errorf("observed-header uses lost their original receipts")
		}
		use.ConfigDependencyGeneratedHeaderDemand = demand
		result = append(result, use)
	}
	return result, nil
}

type configDependencyNodeWitness struct {
	stage    string
	kind     string
	recipe   string
	tool     string
	product  string
	sources  string
	inputs   string
	inputSet string
	trees    string
	outputs  string
}

func walkActionPlanConfigDependencyInputSet(
	plan *ActionPlan,
	node ActionPlanNode,
	visit func(ActionPlanInputSetEntry) error,
) error {
	if node.InputSet == "" {
		return nil
	}
	if plan == nil {
		return fmt.Errorf("node %q input set requires an action plan", node.ID)
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	if err := store.Walk(node.InputSet, visit); err != nil {
		return fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	return nil
}

func lookupActionPlanConfigDependencyWorkInput(
	plan *ActionPlan,
	node ActionPlanNode,
	pathname string,
) (ActionPlanInputSetEntry, bool, error) {
	if node.InputSet == "" {
		return ActionPlanInputSetEntry{}, false, nil
	}
	if plan == nil {
		return ActionPlanInputSetEntry{}, false, fmt.Errorf("node %q input set requires an action plan", node.ID)
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return ActionPlanInputSetEntry{}, false, fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	entry, found, err := store.Lookup(node.InputSet, ActionPlanInputSetTarget{
		Kind: ActionPlanInputSetWorkTarget,
		Path: pathname,
	})
	if err != nil {
		return ActionPlanInputSetEntry{}, false, fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	return entry, found, nil
}

// A witness ignores producer names, which change during content addressing,
// but retains targets, source identities, slots, and use classifications. Hash
// that projection as a Merkle DAG instead of retaining a flattened string for
// every consumer. The cache belongs to one Build/ByNodeID observation, never to
// the mutable plan; immutable subtries are visited once within that observation.
type configDependencyWitnessBuilder struct {
	store  *ActionPlanInputSetStore
	roots  map[string]string
	active map[string]bool
}

func newConfigDependencyWitnessBuilder(plan *ActionPlan) (*configDependencyWitnessBuilder, error) {
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return nil, err
	}
	return &configDependencyWitnessBuilder{
		store: store, roots: map[string]string{}, active: map[string]bool{},
	}, nil
}

func (b *configDependencyWitnessBuilder) inputSet(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	if witness, ok := b.roots[id]; ok {
		return witness, nil
	}
	if b.active[id] {
		return "", fmt.Errorf("config dependency witness has an input-set cycle at %s", id)
	}
	node, ok := b.store.Node(id)
	if !ok {
		return "", fmt.Errorf("config dependency witness references missing input-set node %s", id)
	}
	b.active[id] = true
	defer delete(b.active, id)
	for index := range node.Children {
		child, err := b.inputSet(node.Children[index].ID)
		if err != nil {
			return "", err
		}
		node.Children[index].ID = child
	}
	for index := range node.Entries {
		if node.Entries[index].ProducerID != "" {
			// This is a witness encoding, not an executable input-set node.
			// Keep producer-vs-source provenance without a mutable producer ID.
			node.Entries[index].ProducerID = "producer"
		}
	}
	encoded, err := actionPlanInputSetCanonicalNode(node)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("linux-kernel-config-input-set-witness-v1\x00"), encoded...))
	witness := hex.EncodeToString(digest[:])
	b.roots[id] = witness
	return witness, nil
}

func (b *configDependencyWitnessBuilder) witness(node ActionPlanNode) (configDependencyNodeWitness, error) {
	sources := make([]string, 0, len(node.Sources))
	for _, source := range node.Sources {
		sources = append(sources, source.Role+"\x00"+source.SourceID)
	}
	inputs := make([]string, 0, len(node.Inputs))
	for _, input := range node.Inputs {
		// Producer IDs become transitive content IDs. Role and slot retain the
		// stable input shape needed to match an annotation after that rewrite.
		inputs = append(inputs, input.Role+"\x00"+fmt.Sprint(input.Slot))
	}
	if node.InputSet != "" {
		if err := b.store.validateRootReference(node.InputSet); err != nil {
			return configDependencyNodeWitness{}, err
		}
	}
	inputSet, err := b.inputSet(node.InputSet)
	if err != nil {
		return configDependencyNodeWitness{}, err
	}
	outputs := make([]string, 0, len(node.Outputs))
	for _, output := range node.Outputs {
		outputs = append(outputs, output.Tree+"\x00"+output.Path+"\x00"+output.ArtifactPath)
	}
	return configDependencyNodeWitness{
		stage: node.Stage, kind: node.Kind, recipe: node.Recipe, tool: node.Tool, product: node.Product,
		sources: strings.Join(sources, "\x01"), inputs: strings.Join(inputs, "\x01"),
		inputSet: inputSet,
		trees:    strings.Join(node.Trees, "\x01"), outputs: strings.Join(outputs, "\x01"),
	}, nil
}

// ByNodeID returns a defensive annotation map for the plan's current node IDs.
// It accepts the same plan before or after transitive content addressing.
func (a *ActionPlanConfigDependencyAnalysis) ByNodeID(plan *ActionPlan) (map[string]ConfigDependencySet, error) {
	if a == nil || plan == nil || len(a.sets) != len(plan.Nodes) || len(a.witnesses) != len(plan.Nodes) {
		return nil, fmt.Errorf("config dependency analysis does not match action plan node count")
	}
	witnesses, err := newConfigDependencyWitnessBuilder(plan)
	if err != nil {
		return nil, err
	}
	byWitness := make(map[configDependencyNodeWitness]ConfigDependencySet, len(a.sets))
	counts := make(map[configDependencyNodeWitness]int, len(a.sets))
	for index, witness := range a.witnesses {
		set := a.sets[index]
		if previous, exists := byWitness[witness]; exists {
			if previous.Opaque != set.Opaque || previous.Reason != set.Reason ||
				!slices.Equal(previous.Symbols, set.Symbols) ||
				!slices.Equal(previous.SourcePaths, set.SourcePaths) ||
				!slices.Equal(previous.ObjectPaths, set.ObjectPaths) {
				return nil, fmt.Errorf("config dependency analysis has ambiguous duplicate action-plan witnesses")
			}
		} else {
			byWitness[witness] = set
		}
		counts[witness]++
	}
	out := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		witness, err := witnesses.witness(node)
		if err != nil {
			return nil, err
		}
		set, exists := byWitness[witness]
		if !exists || counts[witness] == 0 {
			return nil, fmt.Errorf("config dependency analysis no longer matches action-plan node %q", node.ID)
		}
		counts[witness]--
		set.Symbols = slices.Clone(set.Symbols)
		set.SourcePaths = slices.Clone(set.SourcePaths)
		set.ObjectPaths = slices.Clone(set.ObjectPaths)
		out[node.ID] = set
	}
	for _, remaining := range counts {
		if remaining != 0 {
			return nil, fmt.Errorf("config dependency analysis contains unmatched action-plan witnesses")
		}
	}
	return out, nil
}

func configDependencyProjectionMention(value string) (string, bool) {
	containsPath := func(value, candidate string) bool {
		for offset := 0; ; {
			index := strings.Index(value[offset:], candidate)
			if index < 0 {
				return false
			}
			index += offset
			beforeOK := index == 0 || strings.ContainsRune("/=:, \t\r\n\"'(", rune(value[index-1]))
			after := index + len(candidate)
			afterOK := after == len(value) || strings.ContainsRune(" \t\r\n\"',);]}", rune(value[after]))
			if beforeOK && afterOK {
				return true
			}
			offset = index + 1
		}
	}
	for _, projection := range resolvedConfigProjections() {
		for _, candidate := range []string{projection.output, "${tree:prep}/" + projection.output,
			"${tree:host}/" + projection.output, "${tree:bootstrap}/" + projection.output,
			"${tree:prehost}/" + projection.output} {
			if value == candidate || containsPath(value, candidate) {
				return projection.output, true
			}
		}
	}
	return "", false
}

type configDependencyInputSetProvenance struct {
	entry      ActionPlanInputSetEntry
	source     ActionPlanSource
	producer   ActionPlanNode
	projection string
	config     bool
}

func actionPlanConfigDependencyInputSetProvenance(
	plan *ActionPlan,
	entry ActionPlanInputSetEntry,
) (configDependencyInputSetProvenance, error) {
	provenance := configDependencyInputSetProvenance{entry: entry}
	if plan == nil {
		return provenance, fmt.Errorf("input-set provenance requires an action plan")
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return provenance, err
	}
	if entry.SourceID != "" {
		source, ok := plan.sourcesByID[entry.SourceID]
		if !ok {
			return provenance, fmt.Errorf("input set references unknown source %s", entry.SourceID)
		}
		provenance.source = source
		provenance.projection, provenance.config = configDependencyResolvedProjectionSource(source)
		if source.Namespace == "config" || source.Namespace == "capsule" {
			provenance.config = true
		}
		return provenance, nil
	}
	plan.ensureNodeLookupIndexes()
	producer, ok := plan.nodesByID[entry.ProducerID]
	if !ok {
		return provenance, fmt.Errorf("input set references unknown producer %s", entry.ProducerID)
	}
	if entry.Slot < 0 || entry.Slot >= len(producer.Outputs) {
		return provenance, fmt.Errorf(
			"input set references output slot %d of producer %s with %d outputs",
			entry.Slot, entry.ProducerID, len(producer.Outputs),
		)
	}
	provenance.producer = producer
	projection, fallback := canonicalFallbackConfigProjection(plan, plan.sourcesByID, producer)
	if fallback && entry.Slot == 0 {
		provenance.projection = projection
		provenance.config = true
	}
	return provenance, nil
}

func lookupActionPlanConfigDependencyWorkInputProvenance(
	plan *ActionPlan,
	node ActionPlanNode,
	pathname string,
) (configDependencyInputSetProvenance, bool, error) {
	entry, found, err := lookupActionPlanConfigDependencyWorkInput(plan, node, pathname)
	if err != nil || !found {
		return configDependencyInputSetProvenance{}, found, err
	}
	provenance, err := actionPlanConfigDependencyInputSetProvenance(plan, entry)
	if err != nil {
		return configDependencyInputSetProvenance{}, false, err
	}
	return provenance, true, nil
}

func actionPlanNodeStagesConfigProjection(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
) (bool, error) {
	return actionPlanNodeStagesConfigProjectionWithQuery(plan, node, recipe, nil)
}

func actionPlanNodeStagesConfigProjectionWithQuery(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	inputSets *configDependencyInputSetQuery,
) (bool, error) {
	for _, pathname := range recipe.WorkingInputs {
		if _, ok := configDependencyProjectionMention(pathname); ok {
			return true, nil
		}
	}
	if inputSets == nil {
		inputSets = newConfigDependencyInputSetQuery(plan)
	}
	staged, err := inputSets.stagesConfig(node.InputSet)
	if err != nil {
		return false, fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	return staged, nil
}

func actionRecipeExplicitConfigProjection(recipe ActionRecipe) (string, bool) {
	values := append([]string(nil), recipe.Arguments...)
	if recipe.CompilerInvocation != nil {
		values = append(values, recipe.CompilerInvocation.Arguments...)
	}
	values = append(values, sortedStringMapValues(recipe.Environment)...)
	values = append(values, recipe.Stdin)
	for _, substitution := range recipe.ContentSubstitutions {
		values = append(values, substitution.Input)
	}
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			values = append(values, invocation.Arguments...)
			values = append(values, invocation.Outputs...)
		}
	}
	for _, value := range values {
		if pathname, ok := configDependencyProjectionMention(value); ok {
			return pathname, true
		}
	}
	return "", false
}

// actionRecipeCompilerExplicitConfigProjection restricts the path test to the
// argv which the compiler actually receives. Kbuild exports KCONFIG_CONFIG and
// stages the resolved projections for every command in a private working tree;
// neither fact makes those bytes a compiler input. The include/source closure
// analysis below separately models the compiler-sensitive environment and every
// effective prefix/Kbuild/suffix argument.
func actionRecipeCompilerExplicitConfigProjection(recipe ActionRecipe) (string, bool) {
	values := recipe.Arguments
	if recipe.CompilerInvocation != nil {
		values = recipe.CompilerInvocation.Arguments
	}
	autoconf := false
	for _, value := range values {
		if pathname, ok := configDependencyProjectionMention(value); ok {
			if pathname != "include/generated/autoconf.h" {
				return pathname, true
			}
			autoconf = true
		}
	}
	return "include/generated/autoconf.h", autoconf
}

func actionPlanGeneratedConfigDependencyPaths(plan *ActionPlan) map[string]bool {
	generated := map[string]bool{}
	if plan == nil {
		return generated
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			generated[canonicalKbuildRulePath(output.Path)] = true
		}
	}
	for _, projection := range resolvedConfigProjections() {
		delete(generated, projection.output)
	}
	return generated
}

func actionPlanPreconfiguredConfigDependencyPaths(
	profile CompactKbuildProfile,
	metadata *CompactMetadata,
	generated map[string]bool,
) map[string]string {
	paths := map[string]string{}
	root := actionPlanPreconfiguredConfigDependencyRoot(profile, metadata)
	if root == "" {
		return paths
	}
	for pathname := range metadata.exactSourcePaths {
		pathname = canonicalKbuildRulePath(pathname)
		if pathname != "" {
			paths[pathname] = filepath.Join(root, filepath.FromSlash(pathname))
		}
	}
	for pathname := range generated {
		pathname = canonicalKbuildRulePath(pathname)
		if pathname != "" {
			paths[pathname] = filepath.Join(root, filepath.FromSlash(pathname))
		}
	}
	return paths
}

func actionPlanPreconfiguredConfigDependencyRoot(
	profile CompactKbuildProfile,
	metadata *CompactMetadata,
) string {
	if metadata == nil || !metadata.preconfiguredObjectTree || profile.evaluator == nil || profile.evaluator.template == nil {
		return ""
	}
	root := strings.TrimSpace(profile.evaluator.template.sourceRoots["__LINUX_BZL_OBJECT_TREE__"])
	if root == "" {
		return ""
	}
	return filepath.Clean(root)
}

func actionPlanConfigDependencySourcePaths(plan *ActionPlan, node ActionPlanNode) ([]string, error) {
	if plan == nil {
		return nil, fmt.Errorf("config dependency source paths require an action plan")
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		// Preserve the classifier's historical fail-closed behavior for compact
		// hand-built/probe plans whose source IDs are not serializable plan IDs.
		return nil, nil
	}
	paths := []string{}
	appendSource := func(sourceID string) {
		source, ok := plan.sourcesByID[sourceID]
		if !ok || source.Namespace == "config" {
			return
		}
		extension := strings.ToLower(path.Ext(source.Path))
		switch extension {
		case ".c", ".cc", ".cp", ".cpp", ".cxx", ".s", ".sx":
			paths = append(paths, source.Path)
		}
		// path.Ext lower-cases .S to .s above.
	}
	for _, edge := range node.Sources {
		appendSource(edge.SourceID)
	}
	if err := walkActionPlanConfigDependencyInputSet(plan, node, func(entry ActionPlanInputSetEntry) error {
		// Persistent work inputs replace the former flat working-closure source
		// edges. Preserve the same conservative translation-unit projection: all
		// staged source-like inputs are scanned, while exact argv classification
		// below decides which spellings are actual compiler operands.
		if entry.Target.Kind == ActionPlanInputSetWorkTarget && entry.SourceID != "" {
			appendSource(entry.SourceID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return slices.Compact(paths), nil
}

func actionPlanConfigDependencySourceBinding(
	plan *ActionPlan,
	node ActionPlanNode,
	binding string,
) (ActionPlanSource, bool) {
	role, ordinal, ok := configDependencyBindingOrdinal(binding)
	if !ok || ordinal >= len(node.Sources) || plan == nil || plan.ensureSourceLookupIndex() != nil {
		return ActionPlanSource{}, false
	}
	edge := node.Sources[ordinal]
	if edge.Role != role {
		return ActionPlanSource{}, false
	}
	source, ok := plan.sourcesByID[edge.SourceID]
	return source, ok
}

// configDependencyBindingOrdinal resolves the canonical role:%08d spelling
// used by recipes directly into its node slice.  Dependency analysis normally
// needs only the handful of bindings named by compiler argv or wrapper
// metadata; indexing those ordinals avoids rebuilding a map for a node's
// complete (and potentially very large) working-tree closure.
func configDependencyBindingOrdinal(binding string) (string, int, bool) {
	separator := strings.LastIndexByte(binding, ':')
	if separator <= 0 || separator+9 != len(binding) {
		return "", 0, false
	}
	ordinalText := binding[separator+1:]
	for _, character := range ordinalText {
		if character < '0' || character > '9' {
			return "", 0, false
		}
	}
	ordinal, err := strconv.Atoi(ordinalText)
	if err != nil || ordinal < 0 || ordinal > maximumActionPlanOrdinal {
		return "", 0, false
	}
	return binding[:separator], ordinal, true
}

func configDependencyResolvedProjectionSource(source ActionPlanSource) (string, bool) {
	sourcePath := canonicalKbuildRulePath(source.Path)
	if sourcePath == "" || sourcePath != source.Path {
		return "", false
	}
	for _, projection := range resolvedConfigProjections() {
		switch source.Namespace {
		case "config":
			if sourcePath == projection.input {
				return projection.output, true
			}
		case "capsule":
			if strings.HasSuffix(sourcePath, "/"+projection.output) {
				return projection.output, true
			}
		}
	}
	return "", false
}

type configDependencyInputBinding struct {
	binding    string
	producer   ActionPlanNode
	slot       int
	projection string
	fallback   bool
}

func actionPlanConfigDependencyInputBinding(
	plan *ActionPlan,
	node ActionPlanNode,
	binding string,
) (configDependencyInputBinding, bool) {
	role, ordinal, ok := configDependencyBindingOrdinal(binding)
	if !ok || ordinal >= len(node.Inputs) || plan == nil || plan.ensureSourceLookupIndex() != nil {
		return configDependencyInputBinding{}, false
	}
	plan.ensureNodeLookupIndexes()
	edge := node.Inputs[ordinal]
	producer, ok := plan.nodesByID[edge.ProducerID]
	if !ok || edge.Role != role || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
		return configDependencyInputBinding{}, false
	}
	projection, fallback := canonicalFallbackConfigProjection(plan, plan.sourcesByID, producer)
	return configDependencyInputBinding{
		binding: binding, producer: producer, slot: edge.Slot,
		projection: projection, fallback: fallback && edge.Slot == 0,
	}, true
}

func configDependencyRecipeConfigSourceUse(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	scanAllArguments bool,
) (string, bool, error) {
	return configDependencyRecipeConfigSourceUseWithQuery(plan, node, recipe, scanAllArguments, nil)
}

func configDependencyRecipeConfigSourceUseWithQuery(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	scanAllArguments bool,
	inputSets *configDependencyInputSetQuery,
) (string, bool, error) {
	usesPlaceholder := func(value string) (string, bool) {
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if len(match) != 3 || match[1] != "source" {
				continue
			}
			source, ok := actionPlanConfigDependencySourceBinding(plan, node, match[2])
			if ok && (source.Namespace == "config" || source.Namespace == "capsule") {
				return match[0], true
			}
		}
		return "", false
	}
	usesReference := func(value string) (string, bool) {
		binding, ok := strings.CutPrefix(value, "source:")
		if !ok {
			return "", false
		}
		source, ok := actionPlanConfigDependencySourceBinding(plan, node, binding)
		if !ok || (source.Namespace != "config" && source.Namespace != "capsule") {
			return "", false
		}
		return "${source:" + binding + "}", true
	}
	// For a direct cc/cxx recipe, Arguments are the compiler argv and the
	// binding-aware compiler classifier below owns them. A typed compound recipe
	// has a separate CompilerInvocation; any use by its outer wrapper is an
	// additional semantic read which the compiler projection does not model.
	// Non-compiler callers scan both argument carriers because neither has a
	// narrower config-dependency model.
	if scanAllArguments || recipe.CompilerInvocation != nil {
		for _, argument := range recipe.Arguments {
			if marker, used := usesPlaceholder(argument); used {
				return marker, true, nil
			}
		}
	}
	if scanAllArguments && recipe.CompilerInvocation != nil {
		for _, argument := range recipe.CompilerInvocation.Arguments {
			if marker, used := usesPlaceholder(argument); used {
				return marker, true, nil
			}
		}
	}
	if recipe.CompilerInvocation != nil {
		for _, reference := range recipe.CompilerInvocation.AuxiliaryWorkingInputUses {
			if marker, used := usesReference(reference); used {
				return marker, true, nil
			}
		}
	}
	for _, value := range recipe.Environment {
		if marker, used := usesPlaceholder(value); used {
			return marker, true, nil
		}
	}
	for _, value := range []string{recipe.Stdin, recipe.Stdout} {
		if marker, used := usesPlaceholder(value); used {
			return marker, true, nil
		}
		if marker, used := usesReference(value); used {
			return marker, true, nil
		}
	}
	for _, substitution := range recipe.ContentSubstitutions {
		if marker, used := usesReference(substitution.Input); used {
			return marker, true, nil
		}
	}
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			for _, value := range append(slices.Clone(invocation.Arguments), invocation.Outputs...) {
				if marker, used := usesPlaceholder(value); used {
					return marker, true, nil
				}
			}
		}
	}
	if inputSets == nil {
		inputSets = newConfigDependencyInputSetQuery(plan)
	}
	use, found, err := inputSets.configUse(node.InputSet, true, scanAllArguments)
	if err != nil {
		return "", false, fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	return use, found, nil
}

func configDependencyRecipeConfigInputUse(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	scanAllArguments bool,
) (string, bool, error) {
	return configDependencyRecipeConfigInputUseWithQuery(plan, node, recipe, scanAllArguments, nil)
}

func configDependencyRecipeConfigInputUseWithQuery(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	scanAllArguments bool,
	inputSets *configDependencyInputSetQuery,
) (string, bool, error) {
	usesPlaceholder := func(value string) (string, bool) {
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if len(match) != 3 || match[1] != "input" {
				continue
			}
			input, ok := actionPlanConfigDependencyInputBinding(plan, node, match[2])
			if ok && input.fallback {
				return match[0], true
			}
		}
		return "", false
	}
	usesReference := func(value string, prefixed bool) (string, bool) {
		binding := value
		if prefixed {
			var ok bool
			binding, ok = strings.CutPrefix(value, "input:")
			if !ok {
				return "", false
			}
		}
		input, ok := actionPlanConfigDependencyInputBinding(plan, node, binding)
		if ok && input.fallback {
			return "${input:" + binding + "}", true
		}
		return "", false
	}
	if scanAllArguments || recipe.CompilerInvocation != nil {
		for _, argument := range recipe.Arguments {
			if marker, used := usesPlaceholder(argument); used {
				return marker, true, nil
			}
		}
	}
	if scanAllArguments && recipe.CompilerInvocation != nil {
		for _, argument := range recipe.CompilerInvocation.Arguments {
			if marker, used := usesPlaceholder(argument); used {
				return marker, true, nil
			}
		}
	}
	if recipe.CompilerInvocation != nil {
		for _, reference := range recipe.CompilerInvocation.AuxiliaryWorkingInputUses {
			if marker, used := usesReference(reference, true); used {
				return marker, true, nil
			}
		}
	}
	for _, value := range recipe.Environment {
		if marker, used := usesPlaceholder(value); used {
			return marker, true, nil
		}
	}
	for _, value := range []string{recipe.Stdin, recipe.Stdout} {
		if marker, used := usesPlaceholder(value); used {
			return marker, true, nil
		}
		if marker, used := usesReference(value, true); used {
			return marker, true, nil
		}
	}
	for _, substitution := range recipe.ContentSubstitutions {
		if marker, used := usesReference(substitution.Input, true); used {
			return marker, true, nil
		}
	}
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			for _, value := range append(slices.Clone(invocation.Arguments), invocation.Outputs...) {
				if marker, used := usesPlaceholder(value); used {
					return marker, true, nil
				}
			}
		}
	}
	if binding, ok := strings.CutPrefix(recipe.Tool, "input:"); ok {
		if marker, used := usesReference(binding, false); used {
			return marker, true, nil
		}
	}
	for _, binding := range recipe.ExecutableInputs {
		if marker, used := usesReference(binding, false); used {
			return marker, true, nil
		}
	}
	for _, bases := range recipe.ObservedOutputBases {
		for _, binding := range bases {
			if marker, used := usesReference(binding, false); used {
				return marker, true, nil
			}
		}
	}
	if inputSets == nil {
		inputSets = newConfigDependencyInputSetQuery(plan)
	}
	use, found, err := inputSets.configUse(node.InputSet, false, scanAllArguments)
	if err != nil {
		return "", false, fmt.Errorf("node %q input set: %w", node.ID, err)
	}
	return use, found, nil
}

type configDependencyCompilerInvocation struct {
	tool                      string
	arguments                 []string
	kbuildStart               int
	kbuildEnd                 int
	environment               map[string]string
	probeEnvironment          map[string]string
	configuredContract        bool
	predefineContractReason   string
	predefineArguments        []string
	predefineKbuildStart      int
	predefineKbuildEnd        int
	predefineProbeEnvironment map[string]string
	hasPredefineProjection    bool
	predefineExplicitSources  bool
	sourceBindings            map[string]ActionPlanSource
	inputBindings             map[string]configDependencyInputBinding
}

func configDependencyCompilerReferencedBindings(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	argumentSets ...[]string,
) (map[string]ActionPlanSource, map[string]configDependencyInputBinding) {
	sources := map[string]ActionPlanSource{}
	inputs := map[string]configDependencyInputBinding{}
	bind := func(kind, binding string) {
		marker := "${" + kind + ":" + binding + "}"
		switch kind {
		case "source":
			if _, exists := sources[marker]; exists {
				return
			}
			if source, ok := actionPlanConfigDependencySourceBinding(plan, node, binding); ok {
				sources[marker] = source
			}
		case "input":
			if _, exists := inputs[marker]; exists {
				return
			}
			if input, ok := actionPlanConfigDependencyInputBinding(plan, node, binding); ok {
				inputs[marker] = input
			}
		}
	}
	for _, arguments := range argumentSets {
		for _, argument := range arguments {
			for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(argument, -1) {
				if len(match) == 3 && (match[1] == "source" || match[1] == "input") {
					bind(match[1], match[2])
				}
			}
		}
	}
	if recipe.CompilerInvocation != nil {
		for _, reference := range recipe.CompilerInvocation.WorkingInputUses {
			kind, binding, ok := strings.Cut(reference, ":")
			if ok {
				bind(kind, binding)
			}
		}
	}
	return sources, inputs
}

func bindConfigDependencyCompilerInvocationInputs(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	invocation *configDependencyCompilerInvocation,
) {
	if invocation == nil {
		return
	}
	invocation.sourceBindings, invocation.inputBindings = configDependencyCompilerReferencedBindings(
		plan, node, recipe, invocation.arguments, invocation.predefineArguments,
	)
}

func actionPlanConfigDependencyScope(node ActionPlanNode) string {
	if node.Stage == "prehost" || node.Stage == "host" {
		return "host"
	}
	return "target"
}

func actionPlanConfigDependencyCompilerInvocation(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
) (configDependencyCompilerInvocation, string) {
	var projection *actionRecipeCompilerProbeInvocation
	if plan != nil {
		if projected, ok := plan.compilerProbeInvocations[node.ID]; ok {
			projection = &projected
		}
	}
	return actionPlanConfigDependencyCompilerInvocationWithProjection(plan, node, recipe, projection)
}

func actionPlanConfigDependencyCompilerInvocationWithProjection(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	projection *actionRecipeCompilerProbeInvocation,
) (configDependencyCompilerInvocation, string) {
	recipeEnvironment := map[string]string{}
	for encodedName, encodedValue := range recipe.Environment {
		name, err := RestoreCompactKbuildLiteralActionMarkers(encodedName)
		if err != nil || name == "" || strings.ContainsAny(name, "=\x00") {
			return configDependencyCompilerInvocation{}, "compiler recipe has an invalid protected environment name"
		}
		value, err := RestoreCompactKbuildLiteralActionMarkers(encodedValue)
		if err != nil || strings.ContainsRune(value, 0) {
			return configDependencyCompilerInvocation{}, "compiler recipe has an invalid protected environment value"
		}
		if value != encodedValue {
			// mapdirectoryrecipe restores protected source literals only after its
			// placeholder scanner has run. Restoring them here would let a literal
			// `${tool:...}` spelling become ProbeStep capability syntax.
			return configDependencyCompilerInvocation{}, "compiler recipe environment contains an unrepresentable protected literal"
		}
		if _, exists := recipeEnvironment[name]; exists {
			return configDependencyCompilerInvocation{}, "compiler recipe has duplicate restored environment names"
		}
		recipeEnvironment[name] = value
	}
	invocation := configDependencyCompilerInvocation{
		tool:             recipe.Tool,
		arguments:        slices.Clone(recipe.Arguments),
		environment:      maps.Clone(recipeEnvironment),
		probeEnvironment: maps.Clone(recipeEnvironment),
	}
	if recipe.CompilerInvocation != nil {
		invocation.tool = recipe.CompilerInvocation.Tool
		invocation.arguments = slices.Clone(recipe.CompilerInvocation.Arguments)
	}
	invocation.kbuildEnd = len(invocation.arguments)
	invocation.predefineArguments = slices.Clone(invocation.arguments)
	invocation.predefineKbuildEnd = len(invocation.predefineArguments)
	invocation.predefineProbeEnvironment = maps.Clone(recipeEnvironment)
	invocation.hasPredefineProjection = true
	if projection != nil {
		invocation.predefineExplicitSources = projection.RequireExplicitSources
		if projection.OpaqueReason != "" {
			return configDependencyCompilerInvocation{}, projection.OpaqueReason
		}
		if projection.Tool != invocation.tool {
			return configDependencyCompilerInvocation{}, "compiler-probe projection changes the configured tool role"
		}
		invocation.predefineArguments = slices.Clone(projection.Arguments)
		invocation.predefineKbuildEnd = len(invocation.predefineArguments)
		invocation.predefineProbeEnvironment = maps.Clone(projection.Environment)
	}
	if plan == nil || plan.metadata == nil {
		bindConfigDependencyCompilerInvocationInputs(plan, node, recipe, &invocation)
		return invocation, ""
	}
	if plan.metadata.actionContracts == nil {
		if len(plan.metadata.actionRoles) != 0 {
			return configDependencyCompilerInvocation{}, "compiler action has no configured action-contract metadata"
		}
		bindConfigDependencyCompilerInvocationInputs(plan, node, recipe, &invocation)
		return invocation, ""
	}
	// cc-link/cxx-link are semantic contracts for the same source-selected
	// cc/cxx executable. Select the contract from the concrete driver mode while
	// preserving invocation.tool as the executable role used by preprocessing
	// and compiler-predefine probes.
	contractRole := toolaction.InvocationContractRole(invocation.tool, invocation.arguments)
	ref := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: contractRole}
	contract, ok := plan.metadata.actionContracts[ref]
	if !ok {
		return configDependencyCompilerInvocation{}, "compiler action has no exact configured action contract"
	}
	if contractRole != invocation.tool {
		// The managed -E query selects the base cc/cxx proxy contract even
		// when the original source-bearing invocation selects cc-link/cxx-link.
		// Until the probe protocol carries that original contract explicitly,
		// only identical configured envelopes can witness its initial state.
		probeContract, exists := plan.metadata.actionContracts[KbuildActionRoleRef{Scope: ref.Scope, Role: invocation.tool}]
		if !exists || !slices.Equal(contract.PrefixArguments, probeContract.PrefixArguments) ||
			!slices.Equal(contract.SuffixArguments, probeContract.SuffixArguments) ||
			!maps.Equal(contract.Environment, probeContract.Environment) {
			invocation.predefineContractReason = "compiler predefine probe would select a different configured action contract"
		}
	}
	arguments := make([]string, 0, len(contract.PrefixArguments)+len(invocation.arguments)+len(contract.SuffixArguments))
	arguments = append(arguments, contract.PrefixArguments...)
	invocation.kbuildStart = len(arguments)
	arguments = append(arguments, invocation.arguments...)
	invocation.kbuildEnd = len(arguments)
	arguments = append(arguments, contract.SuffixArguments...)
	invocation.arguments = arguments
	predefineArguments := make([]string, 0, len(contract.PrefixArguments)+len(invocation.predefineArguments)+len(contract.SuffixArguments))
	predefineArguments = append(predefineArguments, contract.PrefixArguments...)
	invocation.predefineKbuildStart = len(predefineArguments)
	predefineArguments = append(predefineArguments, invocation.predefineArguments...)
	invocation.predefineKbuildEnd = len(predefineArguments)
	predefineArguments = append(predefineArguments, contract.SuffixArguments...)
	invocation.predefineArguments = predefineArguments
	if invocation.environment == nil {
		invocation.environment = map[string]string{}
	}
	// InstallToolActionProxy exports the configured environment immediately
	// before exec, after the outer action runner has installed recipe.Environment.
	// Mirror that last-writer-wins behavior for include-search variables.
	for name, value := range contract.Environment {
		invocation.environment[name] = value
		delete(invocation.probeEnvironment, name)
		delete(invocation.predefineProbeEnvironment, name)
	}
	// proberun protects every configured action-contract environment name, not
	// only the selected primary role. If another role owns a recipe-provided
	// name, the exact effective primary environment cannot be represented by a
	// ProbeStep overlay and must remain opaque.
	for otherRef, otherContract := range plan.metadata.actionContracts {
		if otherRef == ref {
			continue
		}
		for name := range otherContract.Environment {
			if _, exists := invocation.probeEnvironment[name]; exists {
				return configDependencyCompilerInvocation{}, "compiler recipe environment overlaps another configured action contract"
			}
			if _, exists := invocation.predefineProbeEnvironment[name]; exists {
				return configDependencyCompilerInvocation{}, "compiler-probe environment overlaps another configured action contract"
			}
		}
	}
	invocation.configuredContract = true
	bindConfigDependencyCompilerInvocationInputs(plan, node, recipe, &invocation)
	return invocation, ""
}

func configDependencyCompilerArgumentsSetConfigMacro(arguments []string) bool {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		definition := ""
		switch {
		case argument == "-D" || argument == "-U":
			if index+1 >= len(arguments) {
				return true
			}
			index++
			definition = arguments[index]
		case strings.HasPrefix(argument, "-D") || strings.HasPrefix(argument, "-U"):
			definition = argument[2:]
		default:
			continue
		}
		// This intentionally checks the complete definition, not only its name.
		// An alias such as -DENABLED=CONFIG_FOO also carries configuration into
		// the preprocessing namespace.
		if strings.Contains(definition, "CONFIG_") {
			return true
		}
	}
	return false
}

func configDependencyWorkingPathBelow(directory, pathname string) bool {
	directory = canonicalKbuildRulePath(directory)
	pathname = canonicalKbuildRulePath(pathname)
	return directory == "" || pathname == directory || strings.HasPrefix(pathname, directory+"/")
}

type configDependencySourceIncludeMount struct {
	prefix       string
	logicalStart string
	physical     string
}

type configDependencySourceIncludeCacheKey struct {
	directory string
	bindings  string
}

type configDependencySourceIncludeCacheResult struct {
	paths []string
	err   error
}

type configDependencyExplicitSourceIncludeCacheKey struct {
	bindings    string
	directories string
}

type configDependencySourceIncludeSnapshot struct {
	key   configDependencyExplicitSourceIncludeCacheKey
	paths []string
}

// configDependencySourceIncludeCache memoizes complete directory snapshots,
// including failures. The key commits the complete logical-to-physical mount
// table, rather than only the include spelling, so accidentally sharing a
// family cache across distinct immutable source snapshots cannot reuse an
// incompatible namespace decision.
type configDependencySourceIncludeCache struct {
	entries         map[configDependencySourceIncludeCacheKey]configDependencySourceIncludeCacheResult
	explicitEntries map[configDependencyExplicitSourceIncludeCacheKey]configDependencySourceIncludeCacheResult
	walkDir         func(string, fs.WalkDirFunc) error
}

func newConfigDependencySourceIncludeCache() *configDependencySourceIncludeCache {
	return &configDependencySourceIncludeCache{
		entries:         map[configDependencySourceIncludeCacheKey]configDependencySourceIncludeCacheResult{},
		explicitEntries: map[configDependencyExplicitSourceIncludeCacheKey]configDependencySourceIncludeCacheResult{},
		walkDir:         filepath.WalkDir,
	}
}

func (c *configDependencySourceIncludeCache) initialize() {
	if c.entries == nil {
		c.entries = map[configDependencySourceIncludeCacheKey]configDependencySourceIncludeCacheResult{}
	}
	if c.explicitEntries == nil {
		c.explicitEntries = map[configDependencyExplicitSourceIncludeCacheKey]configDependencySourceIncludeCacheResult{}
	}
	if c.walkDir == nil {
		c.walkDir = filepath.WalkDir
	}
}

func configDependencyStringListIdentity(schema string, values []string) string {
	var identity strings.Builder
	identity.WriteString(schema)
	identity.WriteByte(':')
	for _, value := range values {
		identity.WriteString(strconv.Itoa(len(value)))
		identity.WriteByte(':')
		identity.WriteString(value)
	}
	return identity.String()
}

func configDependencySourceIncludeCacheKeyForProfile(
	profile CompactKbuildProfile,
	directory string,
) configDependencySourceIncludeCacheKey {
	return configDependencySourceIncludeCacheKeyForLookup(newConfigDependencySourceLookup(profile), directory)
}

func configDependencySourceIncludeCacheKeyForLookup(
	lookup configDependencySourceLookup,
	directory string,
) configDependencySourceIncludeCacheKey {
	// Keep the original spelling in the key. A missing source template reports
	// that spelling before canonicalization, so collapsing two spellings here
	// could change a cached diagnostic even though valid profiles later resolve
	// them to the same logical directory.
	key := configDependencySourceIncludeCacheKey{directory: directory}
	if !lookup.templateSet {
		key.bindings = "missing-template"
		return key
	}
	bindings := lookup.bindings
	prefixes := slices.Sorted(maps.Keys(bindings))
	parts := make([]string, 0, len(prefixes)*3)
	for _, prefix := range prefixes {
		binding := bindings[prefix]
		parts = append(parts, prefix)
		if binding.ambiguous {
			// Ambiguous bindings always fail closed before their selected physical
			// spelling is observed. compactKbuildProfileSourceRootBindings keeps
			// whichever alias happened to be visited first, so excluding that
			// nondeterministic detail also makes the cache identity stable.
			parts = append(parts, "ambiguous", "")
		} else {
			physical := binding.physical
			if strings.TrimSpace(physical) != "" {
				physical = filepath.Clean(physical)
			}
			parts = append(parts, "unambiguous", physical)
		}
	}
	key.bindings = configDependencyStringListIdentity("bindings-v1", parts)
	return key
}

// configDependencySourceBindingAt returns the longest source-root binding
// which owns logical. Source-root bindings form a logical mount table: a more
// specific entry shadows its ancestor even when the ancestor's physical tree
// also contains that pathname.
func configDependencySourceBindingAt(
	bindings map[string]compactKbuildSourceRootBinding,
	logical string,
) (string, compactKbuildSourceRootBinding, bool) {
	for prefix := logical; ; {
		if binding, ok := bindings[prefix]; ok {
			return prefix, binding, true
		}
		if prefix == "" {
			break
		}
		separator := strings.LastIndexByte(prefix, '/')
		if separator < 0 {
			prefix = ""
		} else {
			prefix = prefix[:separator]
		}
	}
	return "", compactKbuildSourceRootBinding{}, false
}

// configDependencyImmutableSourceIncludePaths enumerates the exact regular
// immutable files visible below one logical source-tree include directory.
// Each nested source root is walked independently and shadows the same logical
// subtree in its ancestor. Symlinks and other non-regular entries fail closed:
// following one could escape the declared immutable roots, while ignoring one
// could omit bytes which the compiler can read.
func configDependencyImmutableSourceIncludePaths(
	lookup configDependencySourceLookup,
	directory string,
	cache *configDependencySourceIncludeCache,
) ([]string, error) {
	key := configDependencySourceIncludeCacheKeyForLookup(lookup, directory)
	walkDir := filepath.WalkDir
	if cache != nil {
		cache.initialize()
		if result, ok := cache.entries[key]; ok {
			return slices.Clone(result.paths), result.err
		}
		walkDir = cache.walkDir
	}
	paths, err := configDependencyImmutableSourceIncludePathsUncached(lookup, directory, walkDir)
	if cache != nil {
		cache.entries[key] = configDependencySourceIncludeCacheResult{
			paths: slices.Clone(paths),
			err:   err,
		}
	}
	return paths, err
}

func configDependencyImmutableSourceIncludePathsUncached(
	lookup configDependencySourceLookup,
	directory string,
	walkDir func(string, fs.WalkDirFunc) error,
) ([]string, error) {
	if !lookup.templateSet {
		return nil, fmt.Errorf("source include directory %q has no Kbuild source-root mapping", directory)
	}
	directory = canonicalKbuildRulePath(directory)
	if directory != "" {
		if err := validatePlanRelativePath("compiler source include directory", directory); err != nil {
			return nil, err
		}
	}
	bindings := lookup.bindings
	basePrefix, base, ok := configDependencySourceBindingAt(bindings, directory)
	if !ok || base.ambiguous || strings.TrimSpace(base.physical) == "" {
		return nil, fmt.Errorf("source include directory %q has no unambiguous immutable source root", directory)
	}

	mounts := map[string]configDependencySourceIncludeMount{}
	baseRelative := strings.TrimPrefix(directory, basePrefix)
	baseRelative = strings.TrimPrefix(baseRelative, "/")
	mounts[basePrefix] = configDependencySourceIncludeMount{
		prefix:       basePrefix,
		logicalStart: directory,
		physical:     filepath.Join(base.physical, filepath.FromSlash(baseRelative)),
	}
	for prefix, binding := range bindings {
		if prefix == basePrefix || prefix == directory ||
			(directory != "" && !strings.HasPrefix(prefix, directory+"/")) ||
			(directory == "" && prefix == "") {
			continue
		}
		if directory == "" || strings.HasPrefix(prefix, directory+"/") {
			if binding.ambiguous || strings.TrimSpace(binding.physical) == "" {
				return nil, fmt.Errorf("source include directory %q contains ambiguous source root %q", directory, prefix)
			}
			mounts[prefix] = configDependencySourceIncludeMount{
				prefix: prefix, logicalStart: prefix, physical: binding.physical,
			}
		}
	}

	ordered := make([]configDependencySourceIncludeMount, 0, len(mounts))
	for _, mount := range mounts {
		ordered = append(ordered, mount)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i].prefix) != len(ordered[j].prefix) {
			return len(ordered[i].prefix) < len(ordered[j].prefix)
		}
		return ordered[i].prefix < ordered[j].prefix
	})

	paths := map[string]bool{}
	for _, mount := range ordered {
		physical, err := filepath.Abs(mount.physical)
		if err != nil {
			return nil, fmt.Errorf("resolve source include root %q: %w", mount.logicalStart, err)
		}
		physical, err = filepath.EvalSymlinks(physical)
		if err != nil {
			return nil, fmt.Errorf("resolve source include root %q: %w", mount.logicalStart, err)
		}
		info, err := os.Lstat(physical)
		if err != nil {
			return nil, fmt.Errorf("inspect source include root %q: %w", mount.logicalStart, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("source include root %q is not an immutable directory", mount.logicalStart)
		}
		err = walkDir(physical, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(physical, filename)
			if err != nil {
				return err
			}
			logical := mount.logicalStart
			if relative != "." {
				logical = path.Join(logical, filepath.ToSlash(relative))
			}
			ownerPrefix, owner, owned := configDependencySourceBindingAt(bindings, logical)
			if !owned || owner.ambiguous {
				return fmt.Errorf("source include path %q has no unambiguous immutable source root", logical)
			}
			if ownerPrefix != mount.prefix {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("source include path %q is a symlink", logical)
			}
			if entry.IsDir() {
				return nil
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if !entryInfo.Mode().IsRegular() {
				return fmt.Errorf("source include path %q is not a regular file", logical)
			}
			if err := validatePlanRelativePath("compiler source include", logical); err != nil {
				return err
			}
			paths[logical] = true
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk source include root %q: %w", mount.logicalStart, err)
		}
	}
	return slices.Sorted(maps.Keys(paths)), nil
}

// configDependencyExplicitSourceIncludePaths closes only explicitly rooted
// source-tree include directories from Kbuild's argv. Configured compiler
// prefix/suffix roots and unresolved external/system roots are deliberately
// outside this filesystem walk and remain represented by the toolset identity.
func configDependencyExplicitSourceIncludePaths(
	profile CompactKbuildProfile,
	arguments []string,
) ([]string, error) {
	return configDependencyExplicitSourceIncludePathsWithCache(profile, arguments, nil)
}

func configDependencyExplicitSourceIncludePathsWithCache(
	profile CompactKbuildProfile,
	arguments []string,
	cache *configDependencySourceIncludeCache,
) ([]string, error) {
	snapshot, err := configDependencyExplicitSourceIncludeSnapshotWithCache(profile, arguments, cache)
	return slices.Clone(snapshot.paths), err
}

// configDependencyExplicitSourceIncludeSnapshotWithCache returns a shared,
// immutable path vector. Internal planner callers may use its stable key to
// avoid repeating both the directory union and the plan-local source interning
// pass. Slice-returning compatibility helpers above clone this value so their
// historical caller-ownership contract remains unchanged.
func configDependencyExplicitSourceIncludeSnapshotWithCache(
	profile CompactKbuildProfile,
	arguments []string,
	cache *configDependencySourceIncludeCache,
) (configDependencySourceIncludeSnapshot, error) {
	lookup := newConfigDependencySourceLookup(profile)
	directories := map[string]bool{}
	for _, operand := range KbuildCompilerIncludeOperands(arguments) {
		location, relative, resolved, err := lookup.includePath(operand.Operand)
		if err != nil {
			return configDependencySourceIncludeSnapshot{}, err
		}
		if !resolved || relative || location.Tree != CompactKbuildInvocationSourceTree {
			continue
		}
		directories[location.Directory] = true
	}
	orderedDirectories := slices.Sorted(maps.Keys(directories))
	key := configDependencyExplicitSourceIncludeCacheKey{
		bindings: configDependencySourceIncludeCacheKeyForLookup(lookup, "").bindings,
		directories: configDependencyStringListIdentity(
			"explicit-source-includes-v1", orderedDirectories,
		),
	}
	if cache != nil {
		cache.initialize()
		if result, ok := cache.explicitEntries[key]; ok {
			return configDependencySourceIncludeSnapshot{key: key, paths: result.paths}, result.err
		}
	}
	paths := map[string]bool{}
	var snapshotErr error
	for _, directory := range orderedDirectories {
		included, err := configDependencyImmutableSourceIncludePaths(lookup, directory, cache)
		if err != nil {
			snapshotErr = err
			break
		}
		for _, pathname := range included {
			paths[pathname] = true
		}
	}
	snapshot := configDependencySourceIncludeSnapshot{
		key: key, paths: slices.Sorted(maps.Keys(paths)),
	}
	if snapshotErr != nil {
		snapshot.paths = nil
	}
	if cache != nil {
		cache.explicitEntries[key] = configDependencySourceIncludeCacheResult{
			paths: snapshot.paths, err: snapshotErr,
		}
	}
	return snapshot, snapshotErr
}

// actionPlanConfigDependencyInternSourcePaths preserves the namespace routing
// selected by the standalone planner for source files discovered only by an
// include-directory walk. The family reducer later receives SourcePaths as
// logical names; retaining these otherwise-unreferenced descriptors is what
// lets it project a file from a more-specific immutable source namespace
// instead of incorrectly defaulting that path to the kernel repository.
func actionPlanConfigDependencyInternSourcePaths(plan *ActionPlan, paths []string) bool {
	snapshot := configDependencySourceIncludeSnapshot{
		key: configDependencyExplicitSourceIncludeCacheKey{
			bindings:    "standalone-paths",
			directories: configDependencyStringListIdentity("source-paths-v1", paths),
		},
		paths: paths,
	}
	return newConfigDependencyPlanSourcePathInternCache().intern(plan, snapshot)
}

// configDependencyPlanSourcePathInternCache is variant-local mutable state. It
// indexes the plan's existing immutable namespaces once and remembers which
// family-shared include snapshots have already been admitted into this plan.
// The source count guard mirrors ActionPlan's own append-oriented lookup
// indexes: an unexpected append invalidates completed results before reuse.
type configDependencyPlanSourcePathInternCache struct {
	plan             *ActionPlan
	sourceCount      int
	namespacesByPath map[string]string
	ambiguousPaths   map[string]bool
	completed        map[configDependencyExplicitSourceIncludeCacheKey]bool
	passes           int
	sourceVisits     int
}

func newConfigDependencyPlanSourcePathInternCache() *configDependencyPlanSourcePathInternCache {
	return &configDependencyPlanSourcePathInternCache{
		namespacesByPath: map[string]string{},
		ambiguousPaths:   map[string]bool{},
		completed:        map[configDependencyExplicitSourceIncludeCacheKey]bool{},
	}
}

func (c *configDependencyPlanSourcePathInternCache) addSource(source ActionPlanSource) {
	if source.Namespace == "config" || source.Namespace == "capsule" {
		return
	}
	pathname := canonicalKbuildRulePath(source.Path)
	if namespace, ok := c.namespacesByPath[pathname]; ok && namespace != source.Namespace {
		c.ambiguousPaths[pathname] = true
		return
	}
	c.namespacesByPath[pathname] = source.Namespace
}

func (c *configDependencyPlanSourcePathInternCache) sync(plan *ActionPlan) {
	if c.plan != plan || c.namespacesByPath == nil || c.ambiguousPaths == nil ||
		c.completed == nil || c.sourceCount > len(plan.Sources) {
		c.plan = plan
		c.sourceCount = 0
		c.namespacesByPath = map[string]string{}
		c.ambiguousPaths = map[string]bool{}
		c.completed = map[configDependencyExplicitSourceIncludeCacheKey]bool{}
	}
	if c.sourceCount == len(plan.Sources) {
		return
	}
	// These entries were appended outside this cache. Even config/capsule
	// entries can expose malformed duplicate IDs through the next source-index
	// rebuild, so invalidate all prior admissions before incorporating them.
	clear(c.completed)
	for _, source := range plan.Sources[c.sourceCount:] {
		c.sourceVisits++
		c.addSource(source)
	}
	c.sourceCount = len(plan.Sources)
}

type configDependencySourcePathInternSpec struct {
	key       string
	namespace string
	pathname  string
}

// internPaths atomically admits one compiler-discovered immutable source
// closure. The plan-local namespace index is synchronized only with appended
// entries, while ActionPlan's own lookup index remains append-current after a
// successful admission. Every pathname, namespace, existing conflict, and
// allocated source ID is checked before the first append, so a false result
// cannot leave a partially admitted closure behind.
func (c *configDependencyPlanSourcePathInternCache) internPaths(
	plan *ActionPlan,
	paths []string,
) bool {
	if plan == nil {
		return false
	}
	if c == nil {
		c = newConfigDependencyPlanSourcePathInternCache()
	}
	c.sync(plan)
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return false
	}

	specs := make([]configDependencySourcePathInternSpec, 0, len(paths))
	seenSpecs := make(map[string]bool, len(paths))
	for _, pathname := range paths {
		if pathname != canonicalKbuildRulePath(pathname) ||
			validatePlanRelativePath("config dependency source", pathname) != nil {
			return false
		}
		namespace := "kernel"
		if plan.metadata != nil {
			var err error
			namespace, err = plan.metadata.actionPlanSourceNamespace(pathname)
			if err != nil {
				return false
			}
		}
		if validatePlanName("source namespace", namespace) != nil {
			return false
		}
		key := actionPlanLookupKey(namespace, pathname)
		if seenSpecs[key] {
			continue
		}
		seenSpecs[key] = true
		specs = append(specs, configDependencySourcePathInternSpec{
			key: key, namespace: namespace, pathname: pathname,
		})
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].pathname != specs[j].pathname {
			return specs[i].pathname < specs[j].pathname
		}
		return specs[i].namespace < specs[j].namespace
	})

	additions := make([]ActionPlanSource, 0, len(specs))
	nextMaximumSourceID := plan.maximumSourceID
	for _, spec := range specs {
		if c.ambiguousPaths[spec.pathname] {
			return false
		}
		if namespace, exists := c.namespacesByPath[spec.pathname]; exists && namespace != spec.namespace {
			return false
		}
		if _, exists := plan.sourceIDs[spec.key]; exists {
			continue
		}
		if nextMaximumSourceID >= 99999999 {
			return false
		}
		nextMaximumSourceID++
		source := ActionPlanSource{
			ID:        fmt.Sprintf("src-%08d", nextMaximumSourceID),
			Namespace: spec.namespace,
			Path:      spec.pathname,
		}
		if _, collision := plan.sourcesByID[source.ID]; collision {
			return false
		}
		additions = append(additions, source)
	}

	// All operations which can reject the closure precede this append. Updating
	// the already-built indexes in place avoids cloning the complete, growing
	// source table and its two lookup maps for every compiler node.
	plan.Sources = append(plan.Sources, additions...)
	for _, source := range additions {
		plan.sourceIDs[actionPlanLookupKey(source.Namespace, source.Path)] = source.ID
		plan.sourcesByID[source.ID] = source
		c.addSource(source)
	}
	plan.maximumSourceID = nextMaximumSourceID
	plan.sourceLookupCount = len(plan.Sources)
	c.sourceCount = len(plan.Sources)
	return true
}

func (c *configDependencyPlanSourcePathInternCache) intern(
	plan *ActionPlan,
	snapshot configDependencySourceIncludeSnapshot,
) bool {
	if plan == nil {
		return false
	}
	if c == nil {
		c = newConfigDependencyPlanSourcePathInternCache()
	}
	c.sync(plan)
	if result, ok := c.completed[snapshot.key]; ok {
		return result
	}
	c.passes++
	result := c.internPaths(plan, snapshot.paths)
	c.completed[snapshot.key] = result
	return result
}

type configDependencyCompilerPredefineProbe struct {
	language         string
	arguments        []string
	translationUnits []string
	environment      map[string]string
}

func configDependencyCompilerSourceArgument(
	argument string,
	sourcePaths []string,
	sourceBindings map[string]ActionPlanSource,
) (string, bool) {
	if strings.HasPrefix(argument, "${source:") && strings.HasSuffix(argument, "}") {
		source, ok := sourceBindings[argument]
		if !ok || source.Namespace == "config" || source.Namespace == "capsule" {
			return "", false
		}
		if slices.Contains(sourcePaths, source.Path) {
			return source.Path, true
		}
		return "", false
	}
	// A deferred word can render multiple arguments or a different path. Do
	// not let either suffix matching or lexical cleaning erase that uncertainty.
	if strings.Contains(argument, "${result:") || strings.Contains(argument, "$(") || linuxProbeSymbolPattern.MatchString(argument) {
		return "", false
	}
	positional := !strings.HasPrefix(argument, "-") && !strings.ContainsRune(argument, '=')
	canonical := argument
	if positional && !strings.Contains(argument, "${") {
		// Kbuild can reuse a source through a parent-relative pattern stem (for
		// example a private object compiled from ../shared/unit.S). Source edges
		// already carry canonical paths, while the preserved probe argv retains
		// that spelling. Match their lexical paths before dropping this operand;
		// never normalize option payloads or the actual compilation's argv.
		canonical = path.Clean(argument)
	}
	for _, pathname := range sourcePaths {
		if argument == pathname {
			return pathname, true
		}
		// A joined compiler option can legitimately end in the translation-unit
		// pathname (KBUILD_MODFILE and prefix-map flags commonly do). Only a
		// positional path may use the rooted-path suffix fallback.
		if !positional {
			continue
		}
		if canonical == pathname || strings.HasSuffix(canonical, "/"+pathname) {
			return pathname, true
		}
	}
	return "", false
}

func configDependencyCompilerSourceBindingReason(
	tool string,
	arguments []string,
	sourcePaths []string,
	sourceBindings map[string]ActionPlanSource,
) string {
	modeledForced := map[int]bool{}
	for _, operand := range KbuildCompilerIncludeOperands(arguments) {
		if operand.Flag != "-include" && operand.Flag != "-imacros" {
			continue
		}
		if _, proven := sourceBindings[operand.Operand]; proven {
			modeledForced[operand.ArgumentIndex] = true
		}
	}
	payloads := compactKbuildCompilerSeparatedOptionPayloads(tool, arguments)
	for index, argument := range arguments {
		if !strings.Contains(argument, "${source:") {
			continue
		}
		if modeledForced[index] {
			continue
		}
		source, exact := sourceBindings[argument]
		if !exact {
			return "compiler argv uses an embedded or unproven source binding"
		}
		if source.Namespace == "config" || source.Namespace == "capsule" {
			return "compiler argv uses a config source binding outside a modeled forced include"
		}
		if payloads[index] || !slices.Contains(sourcePaths, source.Path) {
			return "compiler argv uses a non-translation-unit source binding outside a modeled forced include"
		}
	}
	return ""
}

func configDependencyCompilerInputBindingReason(
	arguments []string,
	inputBindings map[string]configDependencyInputBinding,
) string {
	modeledForced := map[int]string{}
	for _, operand := range KbuildCompilerIncludeOperands(arguments) {
		if operand.Flag != "-include" && operand.Flag != "-imacros" {
			continue
		}
		input, proven := inputBindings[operand.Operand]
		if !proven || !input.fallback {
			continue
		}
		if input.projection != configDependencyAutoconfPath {
			return "compiler explicitly consumes non-autoconf config projection " + input.projection
		}
		modeledForced[operand.ArgumentIndex] = operand.Operand
	}
	for index, argument := range arguments {
		for marker, input := range inputBindings {
			if !input.fallback || !strings.Contains(argument, marker) {
				continue
			}
			if modeledForced[index] == marker {
				continue
			}
			return "compiler argv uses a config input binding outside a modeled forced autoconf include"
		}
	}
	return ""
}

func configDependencyCompilerUnmodeledForwarder(argument string) bool {
	if strings.HasPrefix(argument, "-Wa,") || strings.HasPrefix(argument, "-Wl,") ||
		probeCandidateOptionMatches(argument, "-mllvm", false) {
		return true
	}
	// -Xclang's one configured internal-system-root envelope is accepted by
	// configDependencyUnsupportedCompilerArgument before normalization. Every
	// source-selected -X* spelling has operand arity/semantics which this
	// projection intentionally does not reinterpret.
	return strings.HasPrefix(argument, "-X")
}

func configDependencyCompilerLanguageOption(argument string) bool {
	return argument == "-x" || (strings.HasPrefix(argument, "-x") && len(argument) > 2)
}

// configDependencyCompilerPredefineSourcePaths separates exact forced inputs
// from positional translation-unit candidates before the concrete invocation is
// replaced by its discovery-form symbolic argv. Replay may stage source-like
// forced headers which were hidden inside a probe-dependent flag word during
// discovery. Those headers must not become new mandatory positional operands in
// the otherwise identical initial-state request.
func configDependencyCompilerPredefineSourcePaths(
	invocation configDependencyCompilerInvocation,
	sourcePaths []string,
) []string {
	if invocation.kbuildStart < 0 || invocation.kbuildEnd < invocation.kbuildStart || invocation.kbuildEnd > len(invocation.arguments) {
		return sourcePaths
	}
	arguments := invocation.arguments[invocation.kbuildStart:invocation.kbuildEnd]
	for _, argument := range arguments {
		// An unresolved word could contain a second positional occurrence of a
		// forced file. Do not infer its absence from only the visible operands.
		if linuxProbeSymbolPattern.MatchString(argument) || strings.Contains(argument, "${result:") || strings.Contains(argument, "$(") {
			return sourcePaths
		}
	}
	forced := map[string]bool{}
	forcedOperands := map[int]bool{}
	scalarPayloads := compactKbuildCompilerSeparatedOptionPayloads(invocation.tool, arguments)
	for _, operand := range KbuildCompilerIncludeOperands(arguments) {
		if operand.Flag != "-include" && operand.Flag != "-imacros" {
			continue
		}
		flagIndex := operand.ArgumentIndex
		if !operand.Joined {
			flagIndex--
		}
		if scalarPayloads[flagIndex] {
			continue
		}
		if source, exact := configDependencyCompilerSourceArgument(operand.Operand, sourcePaths, invocation.sourceBindings); exact {
			forced[source] = true
			forcedOperands[operand.ArgumentIndex] = true
		}
	}
	for index, argument := range arguments {
		if !forcedOperands[index] {
			if source, possible := configDependencyCompilerSourceArgument(argument, sourcePaths, invocation.sourceBindings); possible {
				// Preserve a file used both as a forced header and a positional
				// input, including a TU hidden in the preserved symbolic argv.
				delete(forced, source)
			}
		}
	}
	if len(forced) == 0 {
		return sourcePaths
	}
	result := make([]string, 0, len(sourcePaths)-len(forced))
	for _, source := range sourcePaths {
		if !forced[source] {
			result = append(result, source)
		}
	}
	return result
}

func configDependencyCompilerPredefineProbeForInvocation(
	invocation configDependencyCompilerInvocation,
	sourcePaths []string,
) (configDependencyCompilerPredefineProbe, string) {
	if invocation.predefineContractReason != "" {
		return configDependencyCompilerPredefineProbe{}, invocation.predefineContractReason
	}
	if !invocation.configuredContract {
		return configDependencyCompilerPredefineProbe{}, "compiler predefine probe has no exact configured action contract"
	}
	if invocation.tool != "cc" && invocation.tool != "cxx" {
		return configDependencyCompilerPredefineProbe{}, "compiler predefine probe requires cc or cxx role"
	}
	if invocation.kbuildStart < 0 || invocation.kbuildEnd < invocation.kbuildStart || invocation.kbuildEnd > len(invocation.arguments) {
		return configDependencyCompilerPredefineProbe{}, "compiler predefine probe has an invalid Kbuild argument range"
	}
	for _, argument := range invocation.arguments[:invocation.kbuildStart] {
		if configDependencyCompilerLanguageOption(argument) {
			return configDependencyCompilerPredefineProbe{}, "configured compiler prefix contains a language override"
		}
	}
	arguments := invocation.arguments[invocation.kbuildStart:invocation.kbuildEnd]
	for _, argument := range arguments {
		if configDependencyCompilerUnmodeledForwarder(argument) {
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe cannot normalize a forwarding option"
		}
	}
	skip := map[int]bool{}
	separatedPayloads := compactKbuildCompilerSeparatedOptionPayloads(invocation.tool, arguments)
	for _, operand := range KbuildCompilerIncludeOperands(arguments) {
		skip[operand.ArgumentIndex] = true
		if !operand.Joined {
			skip[operand.ArgumentIndex-1] = true
		}
	}
	language := "c"
	if invocation.tool == "cxx" {
		language = "c++"
	}
	for _, pathname := range sourcePaths {
		switch path.Ext(pathname) {
		case ".C", ".cc", ".cp", ".cpp", ".cxx":
			language = "c++"
		case ".S", ".sx":
			language = "assembler-with-cpp"
		case ".s":
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe cannot preprocess a plain assembler source"
		}
	}
	probeEnvironment, reason := configDependencyCompilerPredefineEnvironmentProjection(invocation.probeEnvironment)
	if reason != "" {
		return configDependencyCompilerPredefineProbe{}, reason
	}
	probe := configDependencyCompilerPredefineProbe{
		language: language, environment: probeEnvironment,
	}
	sourceSeen := false
	seenSourcePaths := map[string]bool{}
	for index := 0; index < len(arguments); index++ {
		if skip[index] {
			continue
		}
		argument := arguments[index]
		if separatedPayloads[index] {
			// The owning option was retained on the previous iteration. Preserve
			// this word as its operand even when it lexically resembles a top-level
			// -D/-U/-x option.
			probe.arguments = append(probe.arguments, argument)
			continue
		}
		sourcePath, sourceArgument := configDependencyCompilerSourceArgument(argument, sourcePaths, invocation.sourceBindings)
		if strings.HasPrefix(argument, "${source:") && strings.HasSuffix(argument, "}") && !sourceArgument {
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe cannot disambiguate a translation-unit source binding"
		}
		switch {
		case probeCandidateOptionRequiresScalar(argument, ProbeCandidatePolicyCC):
			if index+1 >= len(arguments) {
				return configDependencyCompilerPredefineProbe{}, "compiler scalar option has no operand"
			}
			probe.arguments = append(probe.arguments, argument, arguments[index+1])
			index++
		case argument == "-x":
			if sourceSeen {
				return configDependencyCompilerPredefineProbe{}, "compiler language override follows the translation unit"
			}
			if index+1 >= len(arguments) {
				return configDependencyCompilerPredefineProbe{}, "compiler language option has no operand"
			}
			index++
			language = arguments[index]
			probe.language = language
		case strings.HasPrefix(argument, "-x") && len(argument) > 2:
			if sourceSeen {
				return configDependencyCompilerPredefineProbe{}, "compiler language override follows the translation unit"
			}
			probe.language = argument[2:]
		case argument == "-o" || argument == "-MF" || argument == "-MT" || argument == "-MQ" || argument == "-MJ" || argument == "--dependency-file":
			if index+1 >= len(arguments) {
				return configDependencyCompilerPredefineProbe{}, "compiler output option has no operand"
			}
			index++
		case (strings.HasPrefix(argument, "-MF") && len(argument) > 3) ||
			(strings.HasPrefix(argument, "-MT") && len(argument) > 3) ||
			(strings.HasPrefix(argument, "-MQ") && len(argument) > 3) ||
			(strings.HasPrefix(argument, "-MJ") && len(argument) > 3) ||
			strings.HasPrefix(argument, "--dependency-file=") ||
			(strings.HasPrefix(argument, "-o") && len(argument) > 2 && !strings.HasPrefix(argument, "-O")):
			// Output-only option; the predefine probe owns its stdout.
		case argument == "-c" || argument == "-S" || argument == "-E" ||
			argument == "-M" || argument == "-MM" || argument == "-MD" || argument == "-MMD" ||
			argument == "-MP" || argument == "-MG" || argument == "-I-":
		case strings.HasPrefix(argument, "-Wp,") && configDependencySafeWpDependencyArgument(argument):
		case sourceArgument:
			sourceSeen = true
			seenSourcePaths[sourcePath] = true
		case argument == "-":
			// This invocation reads a stream, not the immutable source-like
			// prerequisites staged for its surrounding action. Do not claim those
			// paths as missing dynamic argv operands. Scalar/include/output option
			// payloads have already been consumed above and are not stdin here.
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe consumes an uninspectable stdin translation unit"
		case argument == "--":
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe cannot normalize an argv delimiter"
		case strings.Contains(argument, "${output:") || strings.Contains(argument, "${input:") || strings.Contains(argument, "${tree:prep}"):
			return configDependencyCompilerPredefineProbe{}, "compiler predefine probe has an unmodeled generated-path argument"
		default:
			probe.arguments = append(probe.arguments, argument)
		}
	}
	for _, sourcePath := range sourcePaths {
		if !seenSourcePaths[sourcePath] {
			probe.translationUnits = append(probe.translationUnits, sourcePath)
		}
	}
	if invocation.predefineExplicitSources && len(probe.translationUnits) != 0 {
		return configDependencyCompilerPredefineProbe{}, "multi-compiler probe cannot assign unresolved translation-unit ownership"
	}
	switch probe.language {
	case "c", "c++", "assembler-with-cpp":
	default:
		return configDependencyCompilerPredefineProbe{}, "compiler predefine probe has an unsupported language"
	}
	return probe, ""
}

func configDependencySafeWpDependencyArgument(argument string) bool {
	payload, ok := strings.CutPrefix(argument, "-Wp,")
	if !ok {
		return false
	}
	fields := strings.Split(payload, ",")
	if len(fields) == 0 {
		return false
	}
	for index := 0; index < len(fields); {
		switch fields[index] {
		case "-MD", "-MMD":
			index++
			// Linux normally passes the depfile as the following forwarded
			// operand, but the driver also accepts the option without one.
			if index < len(fields) && !strings.HasPrefix(fields[index], "-") {
				if fields[index] == "" {
					return false
				}
				index++
			}
		case "-MF", "-MT", "-MQ":
			if index+1 >= len(fields) || fields[index+1] == "" {
				return false
			}
			index += 2
		case "-MP", "-MG":
			index++
		default:
			return false
		}
	}
	return true
}

// configDependencyMacroDebugArgumentsReason excludes compiler modes whose output
// can observe macro definitions that the source never expands or tests. This is
// a conservative proof gate, not a compiler-capability answer or option rewrite.
// Only canonical levels 0, 1 and 2 are admitted: alternative numeric spellings
// and oversized values must not depend on reproducing a driver's integer parser.
// Presence is sufficient even when a later argument might disable macro output.
func configDependencyMacroDebugArgumentsReason(arguments []string) string {
	for _, argument := range arguments {
		// Scan operands too: "--" can be an -MT/-MQ payload rather than a
		// delimiter. False positives after a real delimiter remain conservative.
		if argument == "-fdebug-macro" {
			return "compiler argv requests unmodeled macro debug output"
		}
		for _, prefix := range []string{"-gcodeview", "-ggdb", "-gvms", "-g"} {
			level, matched := strings.CutPrefix(argument, prefix)
			if !matched || level == "" || level[0] < '0' || level[0] > '9' {
				continue
			}
			if level != "0" && level != "1" && level != "2" {
				return "compiler argv requests unmodeled macro debug output"
			}
		}
	}
	return ""
}

func configDependencyUnsupportedCompilerArgument(arguments []string, configuredSuffixStart int) (int, string) {
	if reason := configDependencyMacroDebugArgumentsReason(arguments); reason != "" {
		return 0, reason
	}
	afterDelimiter := false
	configuredInternalSystemRoots := 0
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		// GCC and Clang expand response files before ordinary option parsing,
		// including a response token spelled after `--`.
		if strings.HasPrefix(argument, "@") {
			return 0, "compiler argv uses an unexpanded response file"
		}
		if argument == "--" {
			afterDelimiter = true
			continue
		}
		if afterDelimiter {
			continue
		}
		if option, _, matched := matchProbeCandidatePathOption(ProbeCandidatePolicyCC, argument); matched && option.kind == ProbeCandidatePathRegularFile {
			return 0, "compiler argv uses unmodeled regular-file input option " + option.name
		}
		switch {
		case probeCandidateOptionMatches(argument, "-B", true) ||
			probeCandidateOptionMatches(argument, "-specs", false) ||
			probeCandidateOptionMatches(argument, "--specs", false) ||
			probeCandidateOptionMatches(argument, "-wrapper", false) ||
			probeCandidateOptionMatches(argument, "--config", false) ||
			probeCandidateOptionMatches(argument, "--config-system-dir", false) ||
			probeCandidateOptionMatches(argument, "--config-user-dir", false) ||
			probeCandidateOptionMatches(argument, "-working-directory", false) ||
			probeCandidateOptionMatches(argument, "-cc1", true):
			return 0, "compiler argv uses an opaque driver or tool-selection escape hatch"
		case probeCandidateOptionMatches(argument, "-fplugin", true) ||
			probeCandidateOptionMatches(argument, "-fpass-plugin", true) ||
			probeCandidateOptionMatches(argument, "-load", true) ||
			probeCandidateOptionMatches(argument, "--load", true) ||
			probeCandidateOptionMatches(argument, "-load-pass-plugin", true):
			return 0, "compiler argv loads uninspectable compiler code"
		case strings.HasPrefix(argument, "-Wp,") && !configDependencySafeWpDependencyArgument(argument):
			return 0, "compiler argv uses unsupported -Wp preprocessor forwarding"
		case strings.HasPrefix(argument, "-Xpreprocessor"):
			return 0, "compiler argv uses unsupported -Xpreprocessor forwarding"
		case argument == "-Xclang":
			// rules_cc's Clang action config appends the resource directory as a
			// cc1 system include. It is an ordinary, compiler-owned search root,
			// but the driver has no direct spelling for the option. Accept only
			// the exact configured suffix envelope; Kbuild-provided forwarding and
			// every other cc1 option remain opaque.
			if index < configuredSuffixStart || index+3 >= len(arguments) ||
				arguments[index+1] != "-internal-isystem" || arguments[index+2] != "-Xclang" ||
				arguments[index+3] == "" || strings.HasPrefix(arguments[index+3], "@") {
				return 0, "compiler argv uses unsupported -Xclang forwarding"
			}
			configuredInternalSystemRoots++
			index += 3
		case configDependencyCompilerUnmodeledForwarder(argument):
			return 0, "compiler argv uses unsupported forwarding semantics"
		case probeCandidateOptionMatches(argument, "-F", true) ||
			strings.HasPrefix(argument, "--include-directory") ||
			strings.HasPrefix(argument, "-iprefix") ||
			strings.HasPrefix(argument, "-iwithprefix") ||
			strings.HasPrefix(argument, "-iwithsysroot") ||
			strings.HasPrefix(argument, "-imultilib") ||
			strings.HasPrefix(argument, "-isystem-after") ||
			strings.HasPrefix(argument, "-iframework") ||
			strings.HasPrefix(argument, "-internal-isystem") ||
			strings.HasPrefix(argument, "-internal-externc-isystem"):
			return 0, "compiler argv uses an unsupported include-search option"
		case strings.HasPrefix(argument, "-include-pch") ||
			strings.HasPrefix(argument, "-include-pth") ||
			strings.HasPrefix(argument, "-fmodule") || strings.HasPrefix(argument, "-fcxx-module") ||
			strings.HasPrefix(argument, "-fimplicit-module") || strings.HasPrefix(argument, "-fprebuilt-module-path") ||
			strings.HasPrefix(argument, "-ivfsoverlay"):
			return 0, "compiler argv uses an opaque precompiled-header or module input"
		case argument == "-trigraphs":
			return 0, "compiler argv enables unmodeled trigraph preprocessing"
		case argument == "-traditional" || argument == "-traditional-cpp" ||
			argument == "-fpreprocessed" || argument == "-fdirectives-only":
			// These modes change phase ordering, comment handling or expansion
			// itself. An ordinary C token proof does not describe their reads.
			return 0, "compiler argv uses an unmodeled preprocessing mode"
		}
	}
	return configuredInternalSystemRoots, ""
}

func configDependencyUnsupportedCompilerEnvironment(environment map[string]string) string {
	for _, name := range []string{
		"CCC_OVERRIDE_OPTIONS",
		"COMPILER_PATH",
		"GCC_SPECS",
	} {
		if environment[name] != "" {
			return "compiler environment uses opaque driver control " + name
		}
	}
	return ""
}

// configDependencyCompilerPredefineEnvironmentProjection retains every
// recipe-provided environment variable except values whose irrelevance follows
// from the bounded GCC/Clang defined-name query. Keeping unknown names exact is
// deliberate: a compiler launcher may be a wrapper with an environment contract
// that this package does not know. Known driver, loader, and script-injection
// controls are rejected below instead of being silently omitted. Native values
// which can select defined names, including OBJECT_MODE, SDKROOT, and Darwin
// deployment targets, consequently remain byte-exact in the probe request.
//
// The Kbuild names in configDependencyCompilerPredefineEnvironmentIsIrrelevant
// are Make/script inputs, not GCC or Clang process inputs. Their already-expanded
// effects are present in the ActionRecipe argv. Locale affects diagnostics (and
// no source bytes are read by the /dev/null query), while SOURCE_DATE_EPOCH only
// changes replacement bodies of always-defined date/time macros. The consumer
// records macro names and definedness, never replacement bodies.
//
// This projection assumes GCC/Clang-compatible process semantics. A wrapper
// which gives one of the explicitly omitted names an additional hidden meaning
// must expose that behavior in its configured action argv; arbitrary hidden argv
// synthesis cannot be modeled by the source/include closure scanner.
func configDependencyCompilerPredefineEnvironmentProjection(environment map[string]string) (map[string]string, string) {
	projected := make(map[string]string, len(environment))
	for name, value := range environment {
		if configDependencyCompilerPredefineEnvironmentIsIrrelevant(name) {
			continue
		}
		projected[name] = value
	}
	if reason := configDependencyUnsupportedCompilerProbeEnvironment(projected); reason != "" {
		return nil, reason
	}
	return projected, ""
}

func configDependencyCompilerPredefineEnvironmentIsIrrelevant(name string) bool {
	switch name {
	case
		// Linux Make/script variables. Kbuild consumes these while constructing
		// the command; neither GCC nor Clang reads them as process controls.
		"CLIPPY_CONF_DIR",
		"INSTALL_HDR_PATH",
		"KBUILD_AFLAGS",
		"KBUILD_CFLAGS",
		"KBUILD_CPPFLAGS",
		"KBUILD_HOSTCFLAGS",
		"KBUILD_HOSTCXXFLAGS",
		"KBUILD_RUSTFLAGS",
		"KBUILD_USERCFLAGS",
		"KBUILD_USERLDFLAGS",
		"KERNELDOC",
		"LIBELF_FLAGS",
		"LIBELF_LIBS",
		"LINUXINCLUDE",
		"M",
		"RESOLVE_BTFIDS",
		"VPATH",
		"obj",
		"objtree",
		"srcroot",
		"srctree",
		// GCC documents locale as diagnostic/informational state rather than an
		// input/source encoding control. Clang has the same defined-name behavior.
		"LANG",
		"LANGUAGE",
		"LC_ALL",
		"LC_CTYPE",
		"LC_MESSAGES",
		// GCC/Clang use this only for __DATE__/__TIME__ replacement text.
		"SOURCE_DATE_EPOCH":
		return true
	default:
		return false
	}
}

func configDependencyUnsupportedCompilerProbeEnvironment(environment map[string]string) string {
	prohibited := map[string]bool{
		"BASH_ENV": true, "ENV": true, "GCONV_PATH": true, "GLIBC_TUNABLES": true,
		"NODE_OPTIONS": true, "NODE_PATH": true, "PERL5LIB": true, "PERL5OPT": true,
		"PERLLIB": true, "PYTHONHOME": true, "PYTHONPATH": true, "RUBYLIB": true,
		"RUBYOPT":      true,
		"CCC_ADD_ARGS": true, "CCC_OVERRIDE_OPTIONS": true, "CL": true, "_CL_": true,
		"CLANG_CONFIG_PATH": true, "CLANG_NO_DEFAULT_CONFIG": true,
		"CLANG_CONFIG_FILE_SYSTEM_DIR": true, "CLANG_CONFIG_FILE_USER_DIR": true,
		"COLLECT_AS_OPTIONS": true, "COLLECT_GCC": true, "COLLECT_GCC_OPTIONS": true,
		"COLLECT_LD": true, "COLLECT_LTO_WRAPPER": true, "COMPILER_PATH": true,
		"CPATH": true, "CPLUS_INCLUDE_PATH": true, "C_INCLUDE_PATH": true,
		"GCC_COMPARE_DEBUG": true, "GCC_COMPARE_DEBUG_EXEC": true, "GCC_EXEC_PREFIX": true,
		"GCC_SPECS": true, "HOME": true,
		"LIBRARY_PATH": true, "OBJC_INCLUDE_PATH": true, "PATH": true,
		"PKG_CONFIG_LIBDIR": true, "PKG_CONFIG_PATH": true, "PKG_CONFIG_SYSROOT_DIR": true,
		"RUSTC_WORKSPACE_WRAPPER": true, "RUSTC_WRAPPER": true, "RUSTDOCFLAGS": true,
		"RUSTFLAGS": true, "XDG_CONFIG_HOME": true, "XDG_DATA_DIRS": true,
		"XDG_DATA_HOME":                      true,
		"LINUX_BZL_TOOL_ACTION_CONTRACTS_V1": true,
		"LINUX_BZL_RUNTIME_TOOL_PATH_V1":     true,
		"LINUX_BZL_TOOLSET_BINDINGS_V1":      true,
	}
	for name := range environment {
		if prohibited[name] || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			return "compiler recipe environment cannot be represented by a safe probe: " + name
		}
	}
	return ""
}

func configDependencyLanguageEnvironmentIsAmbiguous(
	tool string,
	arguments []string,
	sourcePaths []string,
	environment map[string]string,
) bool {
	if environment["C_INCLUDE_PATH"] == "" && environment["CPLUS_INCLUDE_PATH"] == "" {
		return false
	}
	for index, argument := range arguments {
		if argument == "--" {
			break
		}
		if (argument == "-x" && index+1 < len(arguments)) ||
			(strings.HasPrefix(argument, "-x") && len(argument) > len("-x")) {
			return true
		}
	}
	if tool != "cc" {
		return false
	}
	for _, pathname := range sourcePaths {
		if path.Ext(pathname) == ".C" {
			return true
		}
		switch strings.ToLower(path.Ext(pathname)) {
		case ".cc", ".cp", ".cpp", ".cxx":
			return true
		}
	}
	return false
}

func configDependencyCompilerArgumentBeforeDelimiter(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			return false
		}
		if argument == want {
			return true
		}
	}
	return false
}

func configDependencyCompilerOperandIsConfigured(
	operand KbuildCompilerIncludeOperand,
	kbuildStart int,
	kbuildEnd int,
) bool {
	indexes := []int{operand.ArgumentIndex}
	if !operand.Joined {
		indexes = append(indexes, operand.ArgumentIndex-1)
	}
	for _, index := range indexes {
		if index < kbuildStart || index >= kbuildEnd {
			return true
		}
	}
	return false
}

func appendConfigDependencyEnvironmentSearchRoots(
	scanner *configDependencyClosureScanner,
	tool string,
	environment map[string]string,
) {
	appendExternal := func(flag, value string) {
		if value == "" {
			return
		}
		// One unknown root is sufficient to fail closed if lookup reaches this
		// class. Preserve one entry per path-list member so empty components and
		// ordering remain conservative without interpreting host path syntax.
		for range strings.Split(value, string(os.PathListSeparator)) {
			scanner.appendIncludeDirectory(flag, configDependencyIncludeDirectory{external: true})
		}
	}
	appendExternal("-I", environment["CPATH"])
	if tool == "cxx" {
		appendExternal("-isystem", environment["CPLUS_INCLUDE_PATH"])
	} else {
		appendExternal("-isystem", environment["C_INCLUDE_PATH"])
	}
	appendExternal("-isystem", environment["GCC_EXEC_PREFIX"])
	appendExternal("-isystem", environment["SDKROOT"])
}

func actionPlanNodeConfigDependenciesForSourceLookup(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	invocation configDependencyCompilerInvocation,
	sourceLookup configDependencySourceLookup,
	generated map[string]bool,
	generatedText map[string]configDependencyGeneratedText,
	resolveGeneratedText func(string) (configDependencyGeneratedText, bool),
	preconfigured map[string]string,
	autoconfDefinitions configDependencyResolvedAutoconfDefinitions,
	physicalFiles *configDependencyPhysicalFileCache,
	parsed map[string]configDependencyParsedFile,
	conditionalSyntax map[string]configDependencyConditionalSyntax,
	compilerPredefineRequests map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult,
	compilerPredefines *configDependencyPredefineCache,
	forcedHeaders *configDependencyForcedHeaderCache,
	unresolvedGenerated func(configDependencyUnavailableGeneratedHeader),
	resolvedGenerated func(map[string]configDependencyScanFile),
	compilerGuards func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string),
	callCoverage bool,
) (result ConfigDependencySet) {
	scanner := configDependencyClosureScanner{
		sourceLookup:         sourceLookup,
		generated:            generated,
		generatedText:        generatedText,
		resolveGeneratedText: resolveGeneratedText,
		lookupWorkingInput: func(pathname string) (configDependencyInputSetProvenance, bool, error) {
			return lookupActionPlanConfigDependencyWorkInputProvenance(plan, node, pathname)
		},
		preconfigured:     preconfigured,
		physicalFiles:     physicalFiles,
		symbols:           map[string]bool{},
		sourcePaths:       map[string]bool{},
		objectPaths:       map[string]bool{},
		queued:            map[string]bool{},
		queuedPhysical:    map[string]bool{},
		parsed:            parsed,
		conditionalSyntax: conditionalSyntax,
	}
	if callCoverage {
		scanner.callCoverage = &configDependencyCallCoverage{}
	}
	var guardProbe configDependencyCompilerPredefineProbe
	if compilerGuards != nil {
		scanner.collectCompilerGuards = true
		defer func() {
			if guardProbe.language != "" {
				scanner.emitCompilerGuardHints(guardProbe, compilerGuards)
			}
		}()
	}
	if unresolvedGenerated != nil {
		scanner.collectGeneratedHeaders = true
		defer func() {
			if scanner.unavailableGeneratedHeader.logical != "" {
				unresolvedGenerated(scanner.unavailableGeneratedHeader)
			}
		}()
	}
	if resolvedGenerated != nil {
		defer func() {
			if !result.Opaque {
				resolvedGenerated(scanner.resolvedGeneratedFiles)
			}
		}()
	}
	sourcePaths, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		return opaqueConfigDependency("compiler input-set source closure cannot be resolved: " + err.Error())
	}
	if len(sourcePaths) == 0 {
		return opaqueConfigDependency("compiler action has no inspectable immutable translation unit")
	}
	translationUnits := make([]configDependencyScanFile, 0, len(sourcePaths))
	for _, pathname := range sourcePaths {
		overlay, err := sourceLookup.usesSourceOverlay(pathname)
		if err != nil {
			return opaqueConfigDependency("compiler translation-unit source overlay cannot be resolved: " + pathname)
		}
		var file configDependencyScanFile
		var ok bool
		if overlay {
			// External module sources execute from their writable object-tree
			// overlay. Quoted lookup therefore starts in that tree, where a generated
			// header can shadow the immutable overlay bytes.
			file, ok = scanner.objectFile(pathname)
		} else {
			file, ok = scanner.sourceFile(pathname)
		}
		if scanner.opaqueReason != "" {
			return opaqueConfigDependency(scanner.opaqueReason)
		}
		if !ok {
			return opaqueConfigDependency("compiler translation unit is not an inspectable immutable source: " + pathname)
		}
		translationUnits = append(translationUnits, file)
	}
	start, end, ok := KbuildCPreprocessorArgumentRange(invocation.tool, invocation.arguments)
	if !ok {
		return opaqueConfigDependency("compiler argv has no exact preprocessing range")
	}
	compilerArguments := invocation.arguments[start:end]
	if callCoverage {
		if len(translationUnits) != 1 {
			return opaqueConfigDependency("call coverage requires one complete translation unit")
		}
		if !configDependencyCompilerArgumentBeforeDelimiter(compilerArguments, "-nostdinc") {
			return opaqueConfigDependency("call coverage has unmodeled implicit compiler header effects")
		}
	}
	if reason := configDependencyCompilerSourceBindingReason(
		invocation.tool, compilerArguments, sourcePaths, invocation.sourceBindings,
	); reason != "" {
		return opaqueConfigDependency(reason)
	}
	if reason := configDependencyCompilerInputBindingReason(compilerArguments, invocation.inputBindings); reason != "" {
		return opaqueConfigDependency(reason)
	}
	configuredInternalSystemRoots, reason := configDependencyUnsupportedCompilerArgument(
		compilerArguments, invocation.kbuildEnd-start,
	)
	if reason != "" {
		return opaqueConfigDependency(reason)
	}
	if reason := configDependencyUnsupportedCompilerEnvironment(invocation.environment); reason != "" {
		return opaqueConfigDependency(reason)
	}
	if configDependencyLanguageEnvironmentIsAmbiguous(
		invocation.tool, compilerArguments, sourcePaths, invocation.environment,
	) {
		return opaqueConfigDependency("compiler language selection makes configured include-path environment ambiguous")
	}
	for _, argument := range compilerArguments {
		if argument == "-I-" {
			return opaqueConfigDependency("compiler argv uses unsupported -I- search semantics")
		}
	}
	defineSymbols, reason := configDependencyCompilerDefineReplacementSymbols(compilerArguments)
	if reason != "" {
		return opaqueConfigDependency(reason)
	}
	compilerMacroExpansions, reason := configDependencyCompilerMacroExpansionDefinitions(compilerArguments)
	if reason != "" {
		return opaqueConfigDependency(reason)
	}
	scanner.macroExpansions.addDefinitions(compilerMacroExpansions...)
	if !callCoverage {
		for _, symbol := range defineSymbols {
			scanner.symbols[symbol] = true
		}
	}
	compilerLanguage := ""
	compilerMacroState := func() (*configDependencyMacroState, string) {
		if plan == nil || plan.metadata == nil || plan.metadata.compilerPredefines == nil {
			// Hand-constructed plans which have no owning probe workload retain the
			// conservative branch-union scanner.
			return nil, ""
		}
		probe, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, sourcePaths)
		if reason != "" {
			return nil, reason
		}
		compilerLanguage = probe.language
		guardProbe = probe
		result, err := actionPlanCompilerPredefines(
			plan, compilerPredefineRequests,
			actionPlanConfigDependencyScope(node), invocation.tool, probe.language,
			probe.arguments, probe.translationUnits, probe.environment,
		)
		if err != nil {
			return nil, "compiler predefine probe cannot be registered or replayed: " + err.Error()
		}
		if !result.ready {
			return nil, "compiler predefine probe result is unavailable during discovery"
		}
		if scanner.collectCompilerGuards {
			scanner.compilerGuardInitialDefinitions = result.guardDefinitions
		}
		if callCoverage {
			scanner.callCoverage.mode = configDependencyMacroCallMode{dollarAsPunctuation: result.lexical.dollarPunctuation}
		}
		parseKey := configDependencyCompilerPredefineParseKey(result.contents, result.guardIdentity)
		if callCoverage {
			parseKey += "\x00complete-call-coverage"
		}
		parsed, ok := compilerPredefines.get(parseKey)
		if !ok {
			parsed.state, parsed.reason = parseConfigDependencyCompilerPredefinesWithReplacements(result.contents, callCoverage)
			if parsed.reason == "" {
				var valid bool
				parsed.state, valid = configDependencyApplyCompilerDefinedness(parsed.state, result.guardDefinitions, result.guardIdentity)
				if !valid {
					parsed.reason = "compiler definedness result cannot extend initial macro state"
				}
			}
			compilerPredefines.put(parseKey, result.contents, parsed)
		}
		if parsed.reason != "" {
			return nil, parsed.reason
		}
		// Operator readiness is independent: a textual fallback must neither
		// gain builtin authority nor suppress another measured operator.
		scanner.compilerIntrinsicInitialAvailable = make(map[string]bool)
		for operator, defined := range result.guardDefinitions {
			if defined && isCompilerIntrinsicOperator(operator) {
				scanner.compilerIntrinsicInitialAvailable[operator] = true
			}
		}
		scanner.compilerIntrinsicInitialSnapshot = parsed.state.snapshot
		for _, expansion := range parsed.state.macroExpansions {
			delete(scanner.compilerIntrinsicInitialAvailable, expansion.name)
		}
		// A parsed predefine dump is immutable after entering the analysis cache.
		// Translation units and every speculative path inherit it through branch
		// deltas; no caller may write this shared root directly.
		if callCoverage {
			initial := parsed.state.branch()
			initial.installInitialCompilerIntrinsics(result.guardDefinitions,
				configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
					probe.language, probe.arguments, probe.translationUnits, probe.environment),
				result.contents, result.guardIdentity)
			return initial, ""
		}
		return &parsed.state, ""
	}
	macroState, reason := compilerMacroState()
	if reason != "" {
		return opaqueConfigDependency(reason)
	}
	scanner.configureCompilerIntrinsicQueries(plan, node, invocation.tool, guardProbe, compilerGuards)
	scanner.language = compilerLanguage
	if callCoverage && macroState == nil {
		return opaqueConfigDependency("call coverage requires execution-probed compiler state")
	}
	if macroState != nil {
		scanner.macroExpansions.addDefinitions(macroState.macroExpansions...)
		if !callCoverage {
			for symbol := range macroState.symbols {
				scanner.symbols[symbol] = true
			}
		}
	}
	includeOperands := KbuildCompilerIncludeOperands(compilerArguments)
	type forcedConfigDependencyInclude struct {
		flag string
		file configDependencyScanFile
	}
	forcedIncludes := []forcedConfigDependencyInclude{}
	for _, operand := range includeOperands {
		forcedInclude := operand.Flag == "-include" || operand.Flag == "-imacros"
		configuredOperand := invocation.configuredContract && configDependencyCompilerOperandIsConfigured(
			operand,
			invocation.kbuildStart-start,
			invocation.kbuildEnd-start,
		)
		if forcedInclude {
			if configuredOperand {
				return opaqueConfigDependency("configured compiler contract forces an uninspectable include")
			}
			// Source placeholders remain bound to their immutable source File at
			// runtime even when WorkingInputs also stages that File in the private
			// object tree. Recover the exact source edge instead of inferring
			// provenance from the staged destination pathname.
			if binding, ok := strings.CutPrefix(operand.Operand, "${source:"); ok && strings.HasSuffix(binding, "}") {
				binding = strings.TrimSuffix(binding, "}")
				source, proven := actionPlanConfigDependencySourceBinding(plan, node, binding)
				if !proven {
					return opaqueConfigDependency("forced compiler include uses an unproven source binding")
				}
				if projection, configSource := configDependencyResolvedProjectionSource(source); configSource {
					if projection != configDependencyAutoconfPath {
						return opaqueConfigDependency("compiler explicitly consumes non-autoconf config projection " + projection)
					}
					forcedIncludes = append(forcedIncludes, forcedConfigDependencyInclude{
						flag: operand.Flag,
						file: configDependencyScanFile{logical: projection, configProjection: true},
					})
					continue
				}
				if source.Namespace == "config" || source.Namespace == "capsule" {
					return opaqueConfigDependency("forced compiler include uses an unknown config source projection")
				}
				file, found := scanner.sourceFile(source.Path)
				if scanner.opaqueReason != "" {
					return opaqueConfigDependency(scanner.opaqueReason)
				}
				if !found {
					return opaqueConfigDependency("forced compiler include source binding is unavailable to the planner: " + source.Path)
				}
				forcedIncludes = append(forcedIncludes, forcedConfigDependencyInclude{flag: operand.Flag, file: file})
				continue
			}
			if input, proven := invocation.inputBindings[operand.Operand]; proven {
				if !input.fallback {
					if scanner.collectGeneratedHeaders {
						output := input.producer.Outputs[input.slot]
						logical := recipe.WorkingInputs["input:"+input.binding]
						if logical == "" {
							logical = output.Path
						}
						scanner.unavailableGeneratedHeader = configDependencyUnavailableGeneratedHeader{
							logical: logical, explicitlyBound: true,
							bound: configDependencyInputSetProvenance{
								entry:    ActionPlanInputSetEntry{ProducerID: input.producer.ID, Slot: input.slot},
								producer: input.producer,
							},
						}
					}
					return opaqueConfigDependency("forced compiler include uses an unmodeled generated input binding")
				}
				if input.projection != configDependencyAutoconfPath {
					return opaqueConfigDependency("compiler explicitly consumes non-autoconf config projection " + input.projection)
				}
				forcedIncludes = append(forcedIncludes, forcedConfigDependencyInclude{
					flag: operand.Flag,
					file: configDependencyScanFile{logical: input.projection, configProjection: true},
				})
				continue
			}
		}
		if configuredOperand {
			scanner.appendIncludeDirectory(operand.Flag, configDependencyIncludeDirectory{external: true})
			continue
		}
		location, _, resolved, err := sourceLookup.includePath(operand.Operand)
		if err != nil {
			return opaqueConfigDependency("compiler include operand cannot be resolved")
		}
		if !resolved {
			if operand.Flag == "-include" || operand.Flag == "-imacros" {
				return opaqueConfigDependency("forced compiler include is outside inspectable source/object roots")
			}
			scanner.appendIncludeDirectory(operand.Flag, configDependencyIncludeDirectory{external: true})
			continue
		}
		pathname := canonicalKbuildRulePath(location.Directory)
		if forcedInclude {
			var file configDependencyScanFile
			var found bool
			if location.Tree == CompactKbuildInvocationSourceTree {
				file, found = scanner.sourceFile(pathname)
			} else {
				file, found = scanner.objectFileOrConfigProjection(pathname)
			}
			if scanner.opaqueReason != "" {
				return opaqueConfigDependency(scanner.opaqueReason)
			}
			if !found {
				return opaqueConfigDependency("forced compiler include is unavailable to the planner: " + pathname)
			}
			forcedIncludes = append(forcedIncludes, forcedConfigDependencyInclude{flag: operand.Flag, file: file})
			continue
		}
		scanner.appendIncludeDirectory(operand.Flag, configDependencyIncludeDirectory{
			logical: pathname, source: location.Tree == CompactKbuildInvocationSourceTree,
		})
	}
	// The accepted configured Clang cc1 forwarding contributes a search root with
	// exact ordering, but the planner still cannot inspect that root's headers.
	// A custom resource header can mention CONFIG_* or include back into a modeled
	// kernel root, so reaching it must fail closed just like any other external
	// system root. Earlier modeled roots remain precise.
	for range configuredInternalSystemRoots {
		scanner.systemDirectories = append(scanner.systemDirectories, configDependencyIncludeDirectory{external: true})
	}
	appendConfigDependencyEnvironmentSearchRoots(&scanner, invocation.tool, invocation.environment)
	// Without -nostdinc, compiler-owned builtin and system directories sit after
	// explicit -isystem/CPATH classes and before -idirafter. Their header closure
	// is not inspectable by the planner. Keep the invocation precise while all
	// literal lookups resolve before this search class, but fail closed as soon as
	// a lookup can reach an implicit root.
	if !configDependencyCompilerArgumentBeforeDelimiter(compilerArguments, "-nostdinc") {
		scanner.systemDirectories = append(scanner.systemDirectories, configDependencyIncludeDirectory{external: true})
	}
	for _, includeDirectory := range scanner.includeDirectories {
		if includeDirectory.external {
			continue
		}
		for _, systemDirectory := range scanner.systemDirectories {
			if !systemDirectory.external &&
				includeDirectory.logical == systemDirectory.logical &&
				includeDirectory.source == systemDirectory.source {
				// GCC ignores an -I spelling duplicated by -isystem and searches it
				// at the latter class position. Clang has similarly characteristic-aware
				// lookup. Do not guess which earlier directory then wins.
				return opaqueConfigDependency("compiler argv repeats one include root across -I and -isystem classes")
			}
		}
	}
	// GCC and Clang process all -imacros files before all -include files while
	// preserving argv order within each class. Advance the compiler namespace in
	// that real preprocessing order only after every search-root class has been
	// constructed, because a nested literal include must observe the complete
	// driver lookup order.
	orderedForcedFiles := make([]configDependencyScanFile, 0, len(forcedIncludes))
	for _, flag := range []string{"-imacros", "-include"} {
		for _, forced := range forcedIncludes {
			if forced.flag == flag {
				orderedForcedFiles = append(orderedForcedFiles, forced.file)
			}
		}
	}
	if macroState != nil {
		// A driver invocation can name more than one translation unit. Each is a
		// separate preprocessing stream: replay command-line forced headers from
		// the same probed initial namespace, then carry their exact resulting
		// state through the unit and every ordinary nested include.
		for _, translationUnit := range translationUnits {
			scanner.headerCache = nil
			// The unit branch is disposable and never merged into the immutable
			// predefine root. This preserves per-TU macro isolation without copying
			// the complete compiler namespace before every source file.
			unitState := macroState.branch()
			if callCoverage {
				if reason := unitState.applyCompilerMacroReplacements(invocation.tool, compilerArguments); reason != "" {
					return opaqueConfigDependency(reason)
				}
			}
			cacheCandidate := !callCoverage && forcedHeaders != nil && len(translationUnits) == 1 && len(orderedForcedFiles) != 0 &&
				configDependencyForcedHeaderCacheScannerPristine(&scanner)
			var cacheKey configDependencyForcedHeaderCacheKey
			if cacheCandidate {
				cacheKey = newConfigDependencyForcedHeaderCacheKey(
					&scanner, orderedForcedFiles, autoconfDefinitions,
				)
				if cachedState, hit := forcedHeaders.lookup(cacheKey, &scanner, macroState); hit {
					unitState = cachedState
				} else {
					if !unitState.beginForcedHeaderTrace() {
						return opaqueConfigDependency("compiler forced-header cache trace cannot capture the initial macro state")
					}
					entry, exact := func() (configDependencyForcedHeaderCacheEntry, bool) {
						scanner.headerTrace = newConfigDependencyForcedHeaderResolutionTrace()
						defer func() {
							scanner.headerTrace = nil
							unitState.endForcedHeaderTrace()
						}()
						scanner.interpretConditionalCompilerFiles(orderedForcedFiles, unitState, autoconfDefinitions)
						return captureConfigDependencyForcedHeaderCacheEntry(
							&scanner, unitState, macroState, orderedForcedFiles,
						)
					}()
					if exact {
						forcedHeaders.store(cacheKey, entry)
					}
				}
			} else {
				scanner.interpretConditionalCompilerFiles(orderedForcedFiles, unitState, autoconfDefinitions)
			}
			if scanner.opaqueReason != "" {
				break
			}
			if forcedHeaders != nil && !callCoverage {
				scanner.headerCache = &forcedHeaders.ordinary
			}
			scanner.interpretConditionalCompilerFiles(
				[]configDependencyScanFile{translationUnit}, unitState, autoconfDefinitions,
			)
			if scanner.opaqueReason != "" {
				break
			}
		}
	} else {
		for _, file := range orderedForcedFiles {
			scanner.queue(file)
		}
		for _, file := range translationUnits {
			scanner.queue(file)
		}
	}
	if callCoverage && scanner.opaqueReason == "" {
		scanner.callCoverage.complete = true
	}
	return scanner.scan()
}

type configDependencyAnalysisContext struct {
	compilerGuardError            error
	generatedHeaderDemands        *configDependencyGeneratedHeaderDemandCollector
	prospectiveHeaderDemands      *configDependencyGeneratedHeaderDemandCollector
	observedHeaders               *configDependencyObservedHeaders
	inputSets                     *configDependencyInputSetQuery
	generated                     map[string]bool
	preconfiguredByRoot           map[string]map[string]string
	autoconfDefinitions           configDependencyResolvedAutoconfDefinitions
	physicalFiles                 *configDependencyPhysicalFileCache
	sourceIncludePaths            *configDependencySourceIncludeCache
	sourcePathInterns             *configDependencyPlanSourcePathInternCache
	parsed                        map[string]configDependencyParsedFile
	conditionalSyntax             map[string]configDependencyConditionalSyntax
	compilerPredefineRequests     map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult
	compilerPredefines            *configDependencyPredefineCache
	forcedHeaders                 *configDependencyForcedHeaderCache
	nodes                         map[string]ActionPlanNode
	canonicalOutputsByPath        map[string][]configDependencyCanonicalOutput
	invocationPredecessorClosures map[string][]string
	generatedText                 map[configDependencyGeneratedTextKey]configDependencyGeneratedTextResult
	selectionsByProducer          map[string][]compactKbuildSelectionKey
	selectionsByOutput            map[configDependencySelectionOutputKey][]compactKbuildSelectionKey
	pathSensitiveAncestors        map[string]bool
	pathSensitiveAncestorsKnown   map[string]bool
	pathSensitiveAncestorsActive  map[string]bool
}

// ActionPlanConfigDependencySharedCache owns observations whose meaning is
// independent of a resolved kernel configuration: physical-file status,
// parsing of immutable source bytes, complete immutable source-include
// directory snapshots, compiled conditional syntax, and parsed
// compiler-predefine text. It also retains bounded, completed precise compiler
// results. Those results are usable only through a family-attached plan whose
// config-neutral structural identity, compiler/toolset/source/generated-input
// identities, discovered CONFIG_* values, and exact scanned closure all match.
// Plan topology, local node/source IDs, resolved autoconf state, and
// forced-header specialization deliberately remain variant-local.
//
// The cache is intended for sequential planning in one action and is not safe
// for concurrent use.
type ActionPlanConfigDependencySharedCache struct {
	physicalFiles      *configDependencyPhysicalFileCache
	sourceIncludePaths *configDependencySourceIncludeCache
	parsed             map[string]configDependencyParsedFile
	conditionalSyntax  map[string]configDependencyConditionalSyntax
	compilerPredefines *configDependencyPredefineCache
	completed          configDependencyCompletedCompilerCache
}

// NewActionPlanConfigDependencySharedCache creates an empty family-scoped
// dependency-analysis cache.
func NewActionPlanConfigDependencySharedCache() *ActionPlanConfigDependencySharedCache {
	cache := &ActionPlanConfigDependencySharedCache{}
	cache.initialize()
	return cache
}

func (c *ActionPlanConfigDependencySharedCache) initialize() {
	if c.physicalFiles == nil {
		c.physicalFiles = newConfigDependencyPhysicalFileCache()
	}
	if c.sourceIncludePaths == nil {
		c.sourceIncludePaths = newConfigDependencySourceIncludeCache()
	} else {
		c.sourceIncludePaths.initialize()
	}
	if c.parsed == nil {
		c.parsed = map[string]configDependencyParsedFile{}
	}
	if c.conditionalSyntax == nil {
		c.conditionalSyntax = map[string]configDependencyConditionalSyntax{}
	}
	if c.compilerPredefines == nil {
		c.compilerPredefines = newConfigDependencyPredefineCache()
	}
	c.completed.initialize()
}

const (
	configDependencyMaximumCompletedCompilerEntries = 1 << 15
	configDependencyMaximumCompletedCompilerRecords = 1 << 20
	configDependencyMaximumCompletedCompilerBytes   = 64 << 20
)

type configDependencyCompletedSymbolValue struct {
	symbol string
	value  string
}

type configDependencyCompletedCompilerEntry struct {
	set             ConfigDependencySet
	symbolValues    []configDependencyCompletedSymbolValue
	closureIdentity string
}

func (e configDependencyCompletedCompilerEntry) retainedSize() (records, bytes int) {
	records = len(e.symbolValues) + len(e.set.SourcePaths) + len(e.set.ObjectPaths)
	bytes = len(e.closureIdentity)
	for _, witness := range e.symbolValues {
		bytes += len(witness.symbol) + len(witness.value)
	}
	for _, value := range e.set.SourcePaths {
		bytes += len(value)
	}
	for _, value := range e.set.ObjectPaths {
		bytes += len(value)
	}
	return records, bytes
}

// configDependencyCompletedCompilerCache is family-scoped and sequential. A
// base identity may retain more than one relevant-config witness so a later
// sibling can reuse whichever previously analyzed configuration it matches.
type configDependencyCompletedCompilerCache struct {
	entries map[string][]configDependencyCompletedCompilerEntry

	entryCount int
	records    int
	bytes      int

	maximumEntries int
	maximumRecords int
	maximumBytes   int

	hits   int
	misses int
	stores int
}

func (c *configDependencyCompletedCompilerCache) initialize() {
	if c.entries == nil {
		c.entries = map[string][]configDependencyCompletedCompilerEntry{}
	}
	if c.maximumEntries == 0 {
		c.maximumEntries = configDependencyMaximumCompletedCompilerEntries
	}
	if c.maximumRecords == 0 {
		c.maximumRecords = configDependencyMaximumCompletedCompilerRecords
	}
	if c.maximumBytes == 0 {
		c.maximumBytes = configDependencyMaximumCompletedCompilerBytes
	}
}

type configDependencyCompletedCompilerSelection struct {
	key      compactKbuildSelectionKey
	profile  CompactKbuildProfile
	identity string
}

type configDependencyCompletedCompilerCandidate struct {
	key        string
	selections []configDependencyCompletedCompilerSelection
}

func configDependencyCompletedWrite(hash *bytes.Buffer, values ...string) {
	for _, value := range values {
		hash.WriteString(strconv.Itoa(len(value)))
		hash.WriteByte(':')
		hash.WriteString(value)
	}
	hash.WriteByte(0)
}

func configDependencyCompletedWriteStringMap(hash *bytes.Buffer, name string, values map[string]string) {
	configDependencyCompletedWrite(hash, name)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		configDependencyCompletedWrite(hash, key, values[key])
	}
}

func configDependencyCompletedProfileIdentity(
	profile CompactKbuildProfile,
	metadata *CompactMetadata,
) (string, bool) {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return "", false
	}
	var encoded bytes.Buffer
	location, locationSet := CompactKbuildProfileInvocationLocation(profile)
	configDependencyCompletedWrite(
		&encoded,
		"linux-kernel-config-dependency-profile-v1",
		profile.Path,
		profile.Directory,
		fmt.Sprint(locationSet),
		string(location.Tree),
		location.Directory,
		configDependencySourceIncludeCacheKeyForProfile(profile, "").bindings,
		actionPlanPreconfiguredConfigDependencyRoot(profile, metadata),
	)
	digest := sha256.Sum256(encoded.Bytes())
	return hex.EncodeToString(digest[:]), true
}

func (c *configDependencyAnalysisContext) reachesPathSensitiveArchive(
	plan *ActionPlan,
	nodeID string,
) bool {
	if c == nil || plan == nil {
		return true
	}
	if c.pathSensitiveAncestorsKnown[nodeID] {
		return c.pathSensitiveAncestors[nodeID]
	}
	if c.pathSensitiveAncestorsActive[nodeID] {
		return true
	}
	node, ok := c.nodes[nodeID]
	if !ok {
		return true
	}
	c.pathSensitiveAncestorsActive[nodeID] = true
	pathSensitive := plan.hasPathSensitiveArchiveOutput(nodeID)
	for _, edge := range node.Inputs {
		if plan.isPathSensitiveArchiveOutput(edge.ProducerID, edge.Slot) ||
			c.reachesPathSensitiveArchive(plan, edge.ProducerID) {
			pathSensitive = true
			break
		}
	}
	if !pathSensitive {
		if err := walkActionPlanConfigDependencyInputSet(plan, node, func(entry ActionPlanInputSetEntry) error {
			if entry.ProducerID != "" &&
				(plan.isPathSensitiveArchiveOutput(entry.ProducerID, entry.Slot) ||
					c.reachesPathSensitiveArchive(plan, entry.ProducerID)) {
				pathSensitive = true
			}
			return nil
		}); err != nil {
			// Completed-cache reuse is optional. A malformed or unavailable
			// persistent closure must miss rather than bypass ancestry safety.
			pathSensitive = true
		}
	}
	delete(c.pathSensitiveAncestorsActive, nodeID)
	c.pathSensitiveAncestorsKnown[nodeID] = true
	c.pathSensitiveAncestors[nodeID] = pathSensitive
	return pathSensitive
}

func configDependencyCompletedCompilerCandidateForNode(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	context *configDependencyAnalysisContext,
	shared *ActionPlanConfigDependencySharedCache,
) (configDependencyCompletedCompilerCandidate, bool) {
	if plan == nil || plan.metadata == nil || plan.metadata.configFragment == nil || context == nil || shared == nil ||
		plan.familyPlanningCache == nil || plan.familyPlanningCache.configDependencies != shared ||
		node.Kind != "compile" ||
		(recipe.Tool != "cc" && recipe.Tool != "cxx" && recipe.CompilerInvocation == nil) {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	// Intrinsic calls carry independent value-sensitive request/result witnesses.
	// Until this optional cache commits to those witnesses, fresh source/binding
	// replay is mandatory; the older definedness-only key is insufficient.
	if answers := plan.metadata.compilerGuardAnswers; answers != nil && len(answers.intrinsics) != 0 {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	if node.Stage != "prehost" && node.Stage != "host" {
		if value := plan.metadata.configFragment["CONFIG_MODVERSIONS"]; value != "" && value != "n" {
			return configDependencyCompletedCompilerCandidate{}, false
		}
	}
	if context.reachesPathSensitiveArchive(plan, node.ID) {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	// ContentID excludes the mutable node ID while committing to the complete
	// current node semantics, including the persistent input-set root. A plan
	// which has already been transitively addressed yields its final ID here;
	// an earlier planner snapshot still gets an exact (and safely more local)
	// semantic witness without rebuilding the deleted flattened family graph.
	semanticID := node.ContentID()
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
	if reason != "" {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	// The retained probe twin is not the executed compiler argv. Exclude
	// namespace-observing debug modes before any completed-cache lookup.
	if configDependencyMacroDebugArgumentsReason(invocation.arguments) != "" {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	guardWitness, guarded := configDependencyCompilerDefinednessWitness(plan, node, invocation, context)
	if !guarded {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	lexicalWitness, lexicalReady := configDependencyCompilerLexicalWitness(plan, node, invocation, context)
	if !lexicalReady {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	// The structural family identity contains node.Recipe, which is sufficient
	// for plans produced by the append-only builder but not for a public or test
	// plan whose Recipes value was mutated beneath a stable map key. Commit the
	// live canonical recipe digest so that such a mutation can only miss; never
	// let an earlier precise result bypass the live recipe classifier.
	recipeData, err := recipe.CanonicalJSON()
	if err != nil {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	recipeDigest := sha256.Sum256(recipeData)

	selections := context.selectionsForNode(node)
	if len(selections) == 0 {
		return configDependencyCompletedCompilerCandidate{}, false
	}
	stableSelections := make([]configDependencyCompletedCompilerSelection, 0, len(selections))
	for _, selection := range selections {
		profile, ok := plan.selectionGraph.profiles[selection.profile]
		if !ok {
			return configDependencyCompletedCompilerCandidate{}, false
		}
		profileIdentity, exact := configDependencyCompletedProfileIdentity(profile, plan.metadata)
		if !exact {
			return configDependencyCompletedCompilerCandidate{}, false
		}
		var selectionBytes bytes.Buffer
		configDependencyCompletedWrite(
			&selectionBytes,
			"linux-kernel-config-dependency-selection-v1",
			profileIdentity,
			selection.target,
			selection.stage,
		)
		selectionDigest := sha256.Sum256(selectionBytes.Bytes())
		stableSelections = append(stableSelections, configDependencyCompletedCompilerSelection{
			key: selection, profile: profile, identity: hex.EncodeToString(selectionDigest[:]),
		})
	}
	sort.Slice(stableSelections, func(i, j int) bool {
		if stableSelections[i].identity != stableSelections[j].identity {
			return stableSelections[i].identity < stableSelections[j].identity
		}
		return compactKbuildSelectionKeyLess(stableSelections[i].key, stableSelections[j].key)
	})
	for index := 1; index < len(stableSelections); index++ {
		if stableSelections[index-1].identity == stableSelections[index].identity {
			// Two local selection names cannot be safely rebound through one
			// config-neutral profile/target identity.
			return configDependencyCompletedCompilerCandidate{}, false
		}
	}

	var encoded bytes.Buffer
	configDependencyCompletedWrite(
		&encoded,
		"linux-kernel-config-dependency-completed-compiler-v5",
		semanticID,
		guardWitness,
		lexicalWitness,
		hex.EncodeToString(recipeDigest[:]),
		invocation.tool,
		fmt.Sprint(invocation.configuredContract),
		fmt.Sprint(invocation.kbuildStart),
		fmt.Sprint(invocation.kbuildEnd),
		fmt.Sprint(invocation.predefineKbuildStart),
		fmt.Sprint(invocation.predefineKbuildEnd),
		fmt.Sprint(invocation.hasPredefineProjection),
		fmt.Sprint(invocation.predefineExplicitSources),
		fmt.Sprint(plan.metadata.preconfiguredObjectTree),
	)
	for _, argument := range invocation.arguments {
		configDependencyCompletedWrite(&encoded, "argument", argument)
	}
	for _, argument := range invocation.predefineArguments {
		configDependencyCompletedWrite(&encoded, "predefine-argument", argument)
	}
	configDependencyCompletedWriteStringMap(&encoded, "environment", invocation.environment)
	configDependencyCompletedWriteStringMap(&encoded, "probe-environment", invocation.probeEnvironment)
	configDependencyCompletedWriteStringMap(&encoded, "predefine-probe-environment", invocation.predefineProbeEnvironment)
	configDependencyCompletedWriteStringMap(&encoded, "toolsets", plan.Toolsets)
	for _, selection := range stableSelections {
		configDependencyCompletedWrite(&encoded, "selection", selection.identity)
	}
	// semanticID already commits to the canonical recipe ID, every direct edge,
	// output shape, and the content-addressed input-set root. Re-encoding the
	// complete persistent closure here would flatten the trie on every lookup.
	// Keep only state not owned by ActionPlanNode.ContentID: the normalized probe
	// invocation, selected profiles, toolsets, and preconfigured-tree mode.
	digest := sha256.Sum256(encoded.Bytes())
	return configDependencyCompletedCompilerCandidate{
		key: hex.EncodeToString(digest[:]), selections: stableSelections,
	}, true
}

func configDependencyCompletedResolvedProjection(pathname string) bool {
	for _, projection := range resolvedConfigProjections() {
		if pathname == projection.output {
			return true
		}
	}
	return false
}

func configDependencyCompletedGeneratedIdentity(
	plan *ActionPlan,
	logical string,
	projection configDependencyGeneratedText,
	candidate configDependencyCompletedCompilerCandidate,
) (string, bool) {
	if plan == nil || projection.producerID == "" {
		return "", false
	}
	plan.ensureNodeLookupIndexes()
	producer, ok := plan.nodesByID[projection.producerID]
	if !ok || projection.slot < 0 || projection.slot >= len(producer.Outputs) {
		return "", false
	}
	output := producer.Outputs[projection.slot]
	if canonicalKbuildRulePath(output.Path) != logical || output.ObservedPath != "" {
		return "", false
	}
	contentsDigest := sha256.Sum256([]byte(projection.contents))
	var encoded bytes.Buffer
	configDependencyCompletedWrite(
		&encoded,
		"linux-kernel-config-dependency-generated-input-v2",
		producer.ContentID(),
		planOrdinal(projection.slot),
		output.Tree,
		structuralFamilyOutputPath(output),
		structuralFamilyArtifactPath(output),
		fmt.Sprint(projection.macroTable),
		hex.EncodeToString(contentsDigest[:]),
	)
	digest := sha256.Sum256(encoded.Bytes())
	return hex.EncodeToString(digest[:]), true
}

func configDependencyCompletedRegularPhysicalIdentity(
	context *configDependencyAnalysisContext,
	physical string,
) (string, bool) {
	if context == nil || context.physicalFiles == nil || strings.TrimSpace(physical) == "" {
		return "", false
	}
	physical = filepath.Clean(physical)
	status := context.physicalFiles.status(physical)
	if status.err != nil || !status.regular {
		return "", false
	}
	return physical, true
}

func configDependencyCompletedCompilerClosureIdentity(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	context *configDependencyAnalysisContext,
	candidate configDependencyCompletedCompilerCandidate,
	set ConfigDependencySet,
) (string, bool) {
	if plan == nil || plan.metadata == nil || context == nil || set.Opaque {
		return "", false
	}
	var encoded bytes.Buffer
	configDependencyCompletedWrite(&encoded, "linux-kernel-config-dependency-completed-closure-v1")
	for _, pathname := range set.SourcePaths {
		namespace, err := plan.metadata.actionPlanSourceNamespace(pathname)
		if err != nil {
			return "", false
		}
		configDependencyCompletedWrite(
			&encoded, "source-path", pathname, namespace, fmt.Sprint(context.generated[pathname]),
		)
		for _, selection := range candidate.selections {
			physical, resolved := ResolveCompactKbuildProfileSourcePath(selection.profile, pathname)
			if !resolved {
				return "", false
			}
			physicalIdentity, regular := configDependencyCompletedRegularPhysicalIdentity(context, physical)
			if !regular {
				return "", false
			}
			configDependencyCompletedWrite(
				&encoded, "source-file", selection.identity, pathname, physicalIdentity,
			)
		}
	}
	boundGenerated := make([]map[string]configDependencyGeneratedText, len(candidate.selections))
	for index, selection := range candidate.selections {
		boundGenerated[index] = context.boundGeneratedText(plan, selection.profile, node, recipe)
	}
	for _, pathname := range set.ObjectPaths {
		if configDependencyCompletedResolvedProjection(pathname) {
			// Resolved config projections are intentionally witnessed by the exact
			// discovered symbol values rather than their complete bytes. This is the
			// boundary which permits an unrelated CONFIG_* change to hit.
			configDependencyCompletedWrite(&encoded, "config-projection", pathname)
			continue
		}
		configDependencyCompletedWrite(&encoded, "object-path", pathname)
		for selectionIndex, selection := range candidate.selections {
			if context.generated[pathname] {
				projection, exact := boundGenerated[selectionIndex][pathname]
				if !exact {
					projection, exact = context.treeGeneratedText(
						plan, selection.profile, selection.key, node, recipe, pathname,
					)
				}
				if exact {
					identity, stable := configDependencyCompletedGeneratedIdentity(
						plan, pathname, projection, candidate,
					)
					if !stable {
						return "", false
					}
					configDependencyCompletedWrite(
						&encoded, "generated-file", selection.identity, pathname, identity,
					)
					continue
				}
				physical := context.preconfiguredConfigDependencyPaths(
					selection.profile, plan.metadata,
				)[pathname]
				physicalIdentity, regular := configDependencyCompletedRegularPhysicalIdentity(context, physical)
				if !regular {
					return "", false
				}
				configDependencyCompletedWrite(
					&encoded, "preconfigured-object-file", selection.identity, pathname, physicalIdentity,
				)
				continue
			}
			overlay, err := compactKbuildGraphPathUsesSourceOverlay(selection.profile, pathname)
			if err != nil || !overlay {
				return "", false
			}
			physical, resolved := ResolveCompactKbuildProfileSourcePath(selection.profile, pathname)
			if !resolved {
				return "", false
			}
			physicalIdentity, regular := configDependencyCompletedRegularPhysicalIdentity(context, physical)
			if !regular {
				return "", false
			}
			configDependencyCompletedWrite(
				&encoded, "object-source-overlay", selection.identity, pathname, physicalIdentity,
			)
		}
	}
	digest := sha256.Sum256(encoded.Bytes())
	return hex.EncodeToString(digest[:]), true
}

// configDependencyCompletedInternSourcePaths installs the immutable source
// descriptors which a live or cached precise compiler closure discovered.
// Reusing the analysis context's append-aware cache keeps admission linear in
// newly observed sources rather than repeatedly copying the complete plan
// source table. internPaths retains atomic, fail-closed admission.
func configDependencyCompletedInternSourcePaths(
	plan *ActionPlan,
	interns *configDependencyPlanSourcePathInternCache,
	paths []string,
) bool {
	if plan == nil || plan.metadata == nil {
		return false
	}
	if interns == nil {
		interns = newConfigDependencyPlanSourcePathInternCache()
	}
	return interns.internPaths(plan, paths)
}

func configDependencyCompletedSymbolValues(
	plan *ActionPlan,
	set ConfigDependencySet,
) ([]configDependencyCompletedSymbolValue, bool) {
	if plan == nil || plan.metadata == nil || plan.metadata.configFragment == nil || set.Opaque {
		return nil, false
	}
	witnesses := make([]configDependencyCompletedSymbolValue, len(set.Symbols))
	for index, symbol := range set.Symbols {
		value, written := plan.metadata.configFragment[symbol]
		if !written {
			value = "n"
		}
		witnesses[index] = configDependencyCompletedSymbolValue{symbol: symbol, value: value}
	}
	return witnesses, true
}

func configDependencyCompletedSymbolValuesMatch(
	plan *ActionPlan,
	witnesses []configDependencyCompletedSymbolValue,
) bool {
	if plan == nil || plan.metadata == nil || plan.metadata.configFragment == nil {
		return false
	}
	for _, witness := range witnesses {
		value, written := plan.metadata.configFragment[witness.symbol]
		if !written {
			value = "n"
		}
		if value != witness.value {
			return false
		}
	}
	return true
}

func configDependencySetsEqual(left, right ConfigDependencySet) bool {
	return left.Opaque == right.Opaque && left.Reason == right.Reason &&
		slices.Equal(left.Symbols, right.Symbols) &&
		slices.Equal(left.SourcePaths, right.SourcePaths) &&
		slices.Equal(left.ObjectPaths, right.ObjectPaths)
}

func (c *configDependencyCompletedCompilerCache) lookup(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	context *configDependencyAnalysisContext,
	candidate configDependencyCompletedCompilerCandidate,
) (ConfigDependencySet, bool) {
	if c == nil || candidate.key == "" {
		return ConfigDependencySet{}, false
	}
	c.initialize()
	for _, entry := range c.entries[candidate.key] {
		if !configDependencyCompletedSymbolValuesMatch(plan, entry.symbolValues) {
			continue
		}
		closureIdentity, exact := configDependencyCompletedCompilerClosureIdentity(
			plan, node, recipe, context, candidate, entry.set,
		)
		if !exact || closureIdentity != entry.closureIdentity {
			continue
		}
		if !configDependencyCompletedInternSourcePaths(plan, context.sourcePathInterns, entry.set.SourcePaths) {
			continue
		}
		c.hits++
		return ConfigDependencySet{
			Symbols:     slices.Clone(entry.set.Symbols),
			SourcePaths: slices.Clone(entry.set.SourcePaths),
			ObjectPaths: slices.Clone(entry.set.ObjectPaths),
		}, true
	}
	c.misses++
	return ConfigDependencySet{}, false
}

func (c *configDependencyCompletedCompilerCache) store(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	context *configDependencyAnalysisContext,
	candidate configDependencyCompletedCompilerCandidate,
	set ConfigDependencySet,
) bool {
	if c == nil || candidate.key == "" || set.Opaque {
		return false
	}
	c.initialize()
	symbolValues, exact := configDependencyCompletedSymbolValues(plan, set)
	if !exact {
		return false
	}
	closureIdentity, exact := configDependencyCompletedCompilerClosureIdentity(
		plan, node, recipe, context, candidate, set,
	)
	if !exact {
		return false
	}
	entry := configDependencyCompletedCompilerEntry{
		set: ConfigDependencySet{
			Symbols:     slices.Clone(set.Symbols),
			SourcePaths: slices.Clone(set.SourcePaths),
			ObjectPaths: slices.Clone(set.ObjectPaths),
		},
		symbolValues:    slices.Clone(symbolValues),
		closureIdentity: closureIdentity,
	}
	for _, previous := range c.entries[candidate.key] {
		if previous.closureIdentity == entry.closureIdentity &&
			slices.Equal(previous.symbolValues, entry.symbolValues) &&
			configDependencySetsEqual(previous.set, entry.set) {
			return false
		}
	}
	records, retainedBytes := entry.retainedSize()
	if len(c.entries[candidate.key]) == 0 {
		retainedBytes += len(candidate.key)
	}
	if c.entryCount >= c.maximumEntries || records > c.maximumRecords ||
		c.records > c.maximumRecords-records || retainedBytes > c.maximumBytes ||
		c.bytes > c.maximumBytes-retainedBytes {
		return false
	}
	c.entries[candidate.key] = append(c.entries[candidate.key], entry)
	c.entryCount++
	c.records += records
	c.bytes += retainedBytes
	c.stores++
	return true
}

func (c *configDependencyAnalysisContext) preconfiguredConfigDependencyPaths(
	profile CompactKbuildProfile,
	metadata *CompactMetadata,
) map[string]string {
	if c == nil {
		return nil
	}
	root := actionPlanPreconfiguredConfigDependencyRoot(profile, metadata)
	if root == "" {
		return nil
	}
	if paths, ok := c.preconfiguredByRoot[root]; ok {
		return paths
	}
	paths := actionPlanPreconfiguredConfigDependencyPaths(profile, metadata, c.generated)
	if c.preconfiguredByRoot == nil {
		c.preconfiguredByRoot = map[string]map[string]string{}
	}
	c.preconfiguredByRoot[root] = paths
	return paths
}

type configDependencySelectionOutputKey struct {
	stage  string
	target string
}

type configDependencyGeneratedTextKey struct {
	profile  string
	producer string
	slot     int
}

type configDependencyGeneratedTextResult struct {
	projection configDependencyGeneratedText
	projected  bool
}

type configDependencyCanonicalOutput struct {
	producerID string
	stage      string
	tree       string
	slot       int
}

func (c *configDependencyAnalysisContext) invocationPredecessorClosure(
	graph *compactKbuildSelectionGraph,
	profileName string,
) []string {
	if c == nil {
		return graph.compactKbuildInvocationPredecessorClosure(profileName)
	}
	if closure, ok := c.invocationPredecessorClosures[profileName]; ok {
		return closure
	}
	closure := graph.compactKbuildInvocationPredecessorClosure(profileName)
	c.invocationPredecessorClosures[profileName] = closure
	return closure
}

func (c *configDependencyAnalysisContext) uniqueCanonicalOutput(
	logical string,
	node ActionPlanNode,
	recipe ActionRecipe,
) (configDependencyCanonicalOutput, bool) {
	var selected configDependencyCanonicalOutput
	found := false
	for _, candidate := range c.canonicalOutputsByPath[logical] {
		if !slices.Contains(recipe.Trees, candidate.tree) || !slices.Contains(node.Trees, candidate.tree) {
			continue
		}
		if found {
			return configDependencyCanonicalOutput{}, false
		}
		selected = candidate
		found = true
	}
	return selected, found
}

func configDependencyRecipePlaceholder(value, kind string) (string, bool) {
	prefix := "${" + kind + ":"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, "}") {
		return "", false
	}
	binding := strings.TrimSuffix(strings.TrimPrefix(value, prefix), "}")
	return binding, binding != "" && !strings.ContainsAny(binding, "${}\x00\r\n")
}

func (c *configDependencyAnalysisContext) exactActionfileOutputContents(
	recipe ActionRecipe,
	outputBinding string,
) (string, bool) {
	if recipe.Tool != "actionfile" || recipe.Stdin != "" || recipe.Stdout != "" ||
		recipe.ArgumentsFile ||
		len(recipe.ArgumentTransforms) != 0 || len(recipe.ContentSubstitutions) != 0 ||
		len(recipe.CommandReplays) != 0 || recipe.CompilerInvocation != nil {
		return "", false
	}
	output := ""
	form := ""
	lines := []string{}
	projectedBytes := 0
	for index := 0; index < len(recipe.Arguments); index++ {
		argument := recipe.Arguments[index]
		switch argument {
		case "-preserve_mode":
			// Mode preservation is only valid for an input-copy form. Input copies
			// are deliberately not projected: their producer may belong to a
			// different Kbuild profile and must be resolved through exact graph
			// provenance rather than the consumer's source view.
			return "", false
		case "-out", "-line", "-content_base64":
			if index+1 >= len(recipe.Arguments) {
				return "", false
			}
			value := recipe.Arguments[index+1]
			index++
			if compactKbuildContainsProtectedLiteralActionMarker(value) {
				return "", false
			}
			switch argument {
			case "-out":
				if output != "" {
					return "", false
				}
				output = value
			case "-line":
				if form != "" && form != "line" || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "${") {
					return "", false
				}
				if len(value)+1 > configDependencyMaxGeneratedTextBytes-projectedBytes {
					return "", false
				}
				projectedBytes += len(value) + 1
				form = "line"
				lines = append(lines, value)
			case "-content_base64":
				if form != "" || strings.Contains(value, "${") {
					return "", false
				}
				form = "content"
				if base64.StdEncoding.DecodedLen(len(value)) > configDependencyMaxGeneratedTextBytes {
					return "", false
				}
				decoded, err := base64.StdEncoding.DecodeString(value)
				if err != nil {
					return "", false
				}
				if compactKbuildContainsProtectedLiteralActionMarker(string(decoded)) {
					return "", false
				}
				lines = []string{string(decoded)}
			}
		default:
			return "", false
		}
	}
	if binding, ok := configDependencyRecipePlaceholder(output, "output"); !ok || binding != outputBinding || form == "" {
		return "", false
	}
	switch form {
	case "line":
		return strings.Join(lines, "\n") + "\n", true
	case "content":
		return lines[0], true
	default:
		return "", false
	}
}

func configDependencyLiteralRuntimeOutputContents(recipe ActionRecipe, outputBinding string) (string, bool) {
	if recipe.Tool != compactKbuildScriptRuntimeRole || recipe.Stdout != outputBinding || recipe.Stdin != "" ||
		recipe.ArgumentsFile ||
		len(recipe.ArgumentTransforms) != 0 || len(recipe.ContentSubstitutions) != 0 ||
		len(recipe.CommandReplays) != 0 || recipe.CompilerInvocation != nil || len(recipe.Arguments) == 0 {
		return "", false
	}
	for _, argument := range recipe.Arguments {
		if strings.ContainsAny(argument, "\x00\r\n") || strings.Contains(argument, "${") ||
			compactKbuildContainsProtectedLiteralActionMarker(argument) {
			return "", false
		}
	}
	switch recipe.Arguments[0] {
	case "echo":
		arguments := recipe.Arguments[1:]
		if len(arguments) != 0 && strings.HasPrefix(arguments[0], "-") {
			return "", false
		}
		for _, argument := range arguments {
			if strings.Contains(argument, `\`) {
				return "", false
			}
		}
		projectedBytes := len(arguments)
		for _, argument := range arguments {
			projectedBytes += len(argument)
		}
		if projectedBytes > configDependencyMaxGeneratedTextBytes {
			return "", false
		}
		return strings.Join(arguments, " ") + "\n", true
	case "printf":
		if len(recipe.Arguments) < 2 || recipe.Arguments[1] != `%s\n` {
			return "", false
		}
		arguments := recipe.Arguments[2:]
		if len(arguments) == 0 {
			return "\n", true
		}
		projectedBytes := len(arguments)
		for _, argument := range arguments {
			projectedBytes += len(argument)
		}
		if projectedBytes > configDependencyMaxGeneratedTextBytes {
			return "", false
		}
		return strings.Join(arguments, "\n") + "\n", true
	default:
		return "", false
	}
}

func configDependencyGeneratedOutputHasExactToolContract(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
) bool {
	if plan == nil || plan.metadata == nil {
		return recipe.Tool != compactKbuildScriptRuntimeRole
	}
	ref := KbuildActionRoleRef{
		Scope: actionPlanConfigDependencyScope(node),
		Role:  recipe.Tool,
	}
	declared := slices.Contains(plan.metadata.actionRoles, ref)
	if plan.metadata.actionContracts == nil {
		return !declared && recipe.Tool != compactKbuildScriptRuntimeRole
	}
	contract, configured := plan.metadata.actionContracts[ref]
	if declared != configured || recipe.Tool == compactKbuildScriptRuntimeRole && !configured {
		return false
	}
	if !configured {
		return true
	}
	// Prefix/suffix argv or a proxy-owned environment can change even a
	// seemingly literal helper invocation. Only the bare internal helper
	// contract is sufficiently exact for planner-side byte projection.
	return len(contract.PrefixArguments) == 0 &&
		len(contract.SuffixArguments) == 0 &&
		len(contract.Environment) == 0
}

func configDependencyGeneratedOutputRecipeIsLiteral(recipe ActionRecipe) bool {
	if err := recipe.Validate(); err != nil || len(recipe.Environment) != 0 {
		return false
	}
	scopes, err := actionRecipeToolsetScopes(recipe)
	return err == nil && len(scopes) == 0
}

func configDependencyValidatedMacroHeaderOutput(
	node ActionPlanNode,
	recipe ActionRecipe,
	outputBinding string,
) bool {
	if recipe.Tool != "actionfile" || recipe.Stdin != "" || recipe.Stdout != "" || recipe.ArgumentsFile ||
		len(recipe.ArgumentTransforms) != 0 || len(recipe.ContentSubstitutions) != 0 ||
		len(recipe.CommandReplays) != 0 || recipe.CompilerInvocation != nil || len(recipe.Inputs) != len(node.Inputs) {
		return false
	}
	declaredInputs := map[string]bool{}
	for index, binding := range recipe.Inputs {
		if binding != node.Inputs[index].Role+":"+planOrdinal(index) {
			return false
		}
		declaredInputs[binding] = true
	}
	output := ""
	validated := false
	inputs := 0
	for index := 0; index < len(recipe.Arguments); index++ {
		switch recipe.Arguments[index] {
		case "-validate_config_independent_macro_header_v1", "-validate_closed_integer_macro_header_v1":
			if validated {
				return false
			}
			validated = true
		case "-out", "-input":
			if index+1 >= len(recipe.Arguments) {
				return false
			}
			value := recipe.Arguments[index+1]
			index++
			if recipe.Arguments[index-1] == "-out" {
				if output != "" {
					return false
				}
				output = value
				continue
			}
			binding, ok := configDependencyRecipePlaceholder(value, "input")
			if !ok || !declaredInputs[binding] {
				return false
			}
			inputs++
		default:
			return false
		}
	}
	outputMatches := false
	if binding, placeholder := configDependencyRecipePlaceholder(output, "output"); placeholder {
		outputMatches = binding == outputBinding
	} else if validatePlanRelativePath("validated macro header output", output) == nil {
		// A staged Kbuild command writes the logical object-tree pathname and
		// lets WorkingOutputs collect those exact bytes into the declared output
		// binding.  This is equivalent to passing the binding placeholder only
		// when all three identities agree; accepting an arbitrary literal here
		// would make the planner project bytes from the wrong file.
		outputIndex := -1
		for index, binding := range recipe.Outputs {
			if binding != outputBinding {
				continue
			}
			if outputIndex >= 0 {
				return false
			}
			outputIndex = index
		}
		outputMatches = outputIndex >= 0 && outputIndex < len(node.Outputs) &&
			recipe.WorkingOutputs[outputBinding] == output &&
			node.Outputs[outputIndex].Path == output
	}
	return validated && inputs != 0 && outputMatches
}

func (c *configDependencyAnalysisContext) generatedOutputProjection(
	plan *ActionPlan,
	profile CompactKbuildProfile,
	producerID string,
	slot int,
) (projection configDependencyGeneratedText, projected bool) {
	key := configDependencyGeneratedTextKey{profile: profile.Name, producer: producerID, slot: slot}
	if cached, ok := c.generatedText[key]; ok {
		return cached.projection, cached.projected
	}
	defer func() {
		c.generatedText[key] = configDependencyGeneratedTextResult{projection: projection, projected: projected}
	}()

	node, ok := c.nodes[producerID]
	if c.observedHeaders != nil {
		if observed, found := c.observedHeaders.projections[configDependencyObservedOutputKey{producerID, slot}]; found {
			return observed.text, true
		}
	}
	if !ok || node.Kind != "generate" || slot < 0 || slot >= len(node.Outputs) || node.Outputs[slot].ObservedPath != "" {
		return configDependencyGeneratedText{}, false
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok || recipe.Schema != LinuxKernelPlanSchema || recipe.Kind != "generate" ||
		node.Tool != recipe.Tool || slot >= len(recipe.Outputs) {
		return configDependencyGeneratedText{}, false
	}
	if !configDependencyGeneratedOutputHasExactToolContract(plan, node, recipe) {
		return configDependencyGeneratedText{}, false
	}
	if !configDependencyGeneratedOutputRecipeIsLiteral(recipe) {
		return configDependencyGeneratedText{}, false
	}
	outputBinding := recipe.Outputs[slot]
	if outputBinding != planOrdinal(slot) {
		return configDependencyGeneratedText{}, false
	}
	contents, exact := c.exactActionfileOutputContents(recipe, outputBinding)
	if !exact {
		contents, exact = configDependencyLiteralRuntimeOutputContents(recipe, outputBinding)
	}
	if exact && len(contents) <= configDependencyMaxGeneratedTextBytes {
		digest := sha256.Sum256([]byte(contents))
		return configDependencyGeneratedText{
			contents:   contents,
			identity:   producerID + ":" + fmt.Sprint(slot) + ":" + hex.EncodeToString(digest[:]),
			producerID: producerID,
			slot:       slot,
		}, true
	}
	if configDependencyValidatedMacroHeaderOutput(node, recipe, outputBinding) {
		return configDependencyGeneratedText{
			identity:   producerID + ":" + fmt.Sprint(slot) + ":validated-macro-header-v1",
			macroTable: true,
			producerID: producerID,
			slot:       slot,
		}, true
	}
	return configDependencyGeneratedText{}, false
}

func (c *configDependencyAnalysisContext) boundGeneratedText(
	plan *ActionPlan,
	profile CompactKbuildProfile,
	node ActionPlanNode,
	recipe ActionRecipe,
) map[string]configDependencyGeneratedText {
	projected := map[string]configDependencyGeneratedText{}
	ambiguous := map[string]bool{}
	record := func(logical, producerID string, slot int) {
		logical = canonicalKbuildRulePath(logical)
		if logical == "" {
			return
		}
		producer, ok := c.nodes[producerID]
		if !ok || slot < 0 || slot >= len(producer.Outputs) ||
			canonicalKbuildRulePath(producer.Outputs[slot].Path) != logical {
			return
		}
		candidate, exact := c.generatedOutputProjection(plan, profile, producerID, slot)
		if !exact {
			return
		}
		if previous, exists := projected[logical]; exists &&
			(previous.identity != candidate.identity || previous.macroTable != candidate.macroTable || previous.contents != candidate.contents) {
			delete(projected, logical)
			ambiguous[logical] = true
			return
		}
		if !ambiguous[logical] {
			projected[logical] = candidate
		}
	}
	if len(recipe.Inputs) == len(node.Inputs) {
		for index, binding := range recipe.Inputs {
			edge := node.Inputs[index]
			if binding != edge.Role+":"+planOrdinal(index) {
				continue
			}
			logical, staged := recipe.WorkingInputs["input:"+binding]
			if staged {
				record(logical, edge.ProducerID, edge.Slot)
			}
		}
	}
	if err := walkActionPlanConfigDependencyInputSet(plan, node, func(entry ActionPlanInputSetEntry) error {
		if entry.Target.Kind == ActionPlanInputSetWorkTarget && entry.ProducerID != "" {
			record(entry.Target.Path, entry.ProducerID, entry.Slot)
		}
		return nil
	}); err != nil {
		// An unavailable persistent closure cannot contribute a partial exact
		// generated-text view. The include scanner will fail closed when it reaches
		// any generated path.
		return map[string]configDependencyGeneratedText{}
	}
	return projected
}

type configDependencyGeneratedOutputOwner struct {
	profile  CompactKbuildProfile
	producer ActionPlanNode
	slot     int
}

func (c *configDependencyAnalysisContext) treeGeneratedText(
	plan *ActionPlan,
	profile CompactKbuildProfile,
	consumer compactKbuildSelectionKey,
	node ActionPlanNode,
	recipe ActionRecipe,
	logical string,
) (configDependencyGeneratedText, bool) {
	owner, found := c.treeGeneratedOutputOwner(plan, profile, consumer, node, recipe, logical)
	if !found {
		return configDependencyGeneratedText{}, false
	}
	return c.generatedOutputProjection(plan, owner.profile, owner.producer.ID, owner.slot)
}

// treeGeneratedOutputOwner is the existing selected/visible publisher proof,
// separated from whether that publisher's bytes can be projected statically.
func (c *configDependencyAnalysisContext) treeGeneratedOutputOwner(
	plan *ActionPlan,
	profile CompactKbuildProfile,
	consumer compactKbuildSelectionKey,
	node ActionPlanNode,
	recipe ActionRecipe,
	logical string,
) (configDependencyGeneratedOutputOwner, bool) {
	logical = canonicalKbuildRulePath(logical)
	if plan == nil || plan.selectionGraph == nil || logical == "" || !c.generated[logical] {
		return configDependencyGeneratedOutputOwner{}, false
	}
	graph := plan.selectionGraph
	consumerProfile, ok := graph.profiles[consumer.profile]
	if !ok || consumerProfile.Name != profile.Name || consumer.stage != node.Stage {
		return configDependencyGeneratedOutputOwner{}, false
	}
	owner, selected, err := graph.compactKbuildSelectionRecordedPathOwnerWithPredecessorClosure(
		consumer,
		logical,
		func(profileName string) []string {
			return c.invocationPredecessorClosure(graph, profileName)
		},
	)
	if !selected {
		if err != nil && !compactKbuildPathOwnerIsUnrecorded(err) {
			return configDependencyGeneratedOutputOwner{}, false
		}
		// Make's prerequisite frontier does not contain headers discovered only
		// by the compiler. At this point the closure scanner has proved an actual
		// read through a concrete declared tree. Overwrite lowering has already
		// selected the sole canonical publisher in that tree, while older writers
		// remain in the plan at noncanonical immutable ArtifactPaths. Resolve that
		// publisher directly instead of asking the generic selection-owner
		// fallback, whose multiple historical owners are intentionally ambiguous.
		candidate, unique := c.uniqueCanonicalOutput(logical, node, recipe)
		if !unique {
			return configDependencyGeneratedOutputOwner{}, false
		}
		if familyStageOrdinal(candidate.stage) >= familyStageOrdinal(node.Stage) {
			return configDependencyGeneratedOutputOwner{}, false
		}
		return configDependencyGeneratedOutputOwner{profile: profile, producer: c.nodes[candidate.producerID], slot: candidate.slot}, true
	}
	if err != nil {
		return configDependencyGeneratedOutputOwner{}, false
	}
	producerID, materialized := graph.materializedProducers[owner]
	producer, exists := c.nodes[producerID]
	if !materialized || !exists || producer.Stage != owner.stage {
		return configDependencyGeneratedOutputOwner{}, false
	}
	slot := -1
	for index, output := range producer.Outputs {
		if canonicalKbuildRulePath(output.Path) != logical {
			continue
		}
		if slot >= 0 {
			return configDependencyGeneratedOutputOwner{}, false
		}
		slot = index
	}
	if slot < 0 {
		return configDependencyGeneratedOutputOwner{}, false
	}
	output := producer.Outputs[slot]
	if output.Tree == "" || !actionPlanOutputIsCanonical(output) ||
		!slices.Contains(recipe.Trees, output.Tree) || !slices.Contains(node.Trees, output.Tree) {
		return configDependencyGeneratedOutputOwner{}, false
	}
	explicitInput := false
	for _, edge := range node.Inputs {
		if edge.ProducerID == producerID && edge.Slot == slot {
			explicitInput = true
			break
		}
	}
	if !explicitInput {
		entry, found, err := lookupActionPlanConfigDependencyWorkInput(plan, node, logical)
		if err != nil {
			return configDependencyGeneratedOutputOwner{}, false
		}
		if found {
			if entry.ProducerID != producerID || entry.Slot != slot {
				// A different exact working-path binding shadows the selected tree
				// writer; it is not sound to project the latter's bytes.
				return configDependencyGeneratedOutputOwner{}, false
			}
			explicitInput = true
		}
	}
	if !explicitInput && familyStageOrdinal(producer.Stage) >= familyStageOrdinal(node.Stage) {
		return configDependencyGeneratedOutputOwner{}, false
	}
	ownerProfile, ok := graph.profiles[owner.profile]
	if !ok {
		return configDependencyGeneratedOutputOwner{}, false
	}
	return configDependencyGeneratedOutputOwner{profile: ownerProfile, producer: producer, slot: slot}, true
}

func (c *configDependencyAnalysisContext) recordGeneratedHeaderDemand(
	plan *ActionPlan, profile CompactKbuildProfile, selection compactKbuildSelectionKey,
	consumer ActionPlanNode, recipe ActionRecipe, unavailable configDependencyUnavailableGeneratedHeader,
	collector *configDependencyGeneratedHeaderDemandCollector, expected *configDependencyGeneratedText,
) {
	record := func(producer ActionPlanNode, slot int) {
		if expected != nil && (producer.ID != expected.producerID || slot != expected.slot) {
			return
		}
		collector.record(consumer, producer, slot, unavailable.logical)
	}
	if unavailable.explicitlyBound {
		if !unavailable.bound.config && unavailable.bound.entry.ProducerID != "" {
			record(unavailable.bound.producer, unavailable.bound.entry.Slot)
		}
		return
	}
	// Use the exact direct working binding, never a global same-path producer.
	// Conflicting/unknown direct bindings cannot authorize an execution demand.
	var bound configDependencyInputBinding
	found := false
	for binding, logical := range recipe.WorkingInputs {
		if canonicalKbuildRulePath(logical) != unavailable.logical {
			continue
		}
		name, input := strings.CutPrefix(binding, "input:")
		if !input {
			return
		}
		candidate, exact := actionPlanConfigDependencyInputBinding(plan, consumer, name)
		if !exact || candidate.fallback || found && (bound.producer.ID != candidate.producer.ID || bound.slot != candidate.slot) {
			return
		}
		bound, found = candidate, true
	}
	if found {
		record(bound.producer, bound.slot)
		return
	}
	owner, found := c.treeGeneratedOutputOwner(plan, profile, selection, consumer, recipe, unavailable.logical)
	if found {
		record(owner.producer, owner.slot)
	}
}

func (c *configDependencyAnalysisContext) selectionsForNode(node ActionPlanNode) []compactKbuildSelectionKey {
	if c == nil {
		return nil
	}
	if selections := c.selectionsByProducer[node.ID]; len(selections) != 0 {
		return selections
	}
	selectionSet := map[compactKbuildSelectionKey]bool{}
	for _, output := range node.Outputs {
		key := configDependencySelectionOutputKey{
			stage: node.Stage, target: canonicalKbuildRulePath(output.Path),
		}
		for _, selection := range c.selectionsByOutput[key] {
			selectionSet[selection] = true
		}
	}
	selections := make([]compactKbuildSelectionKey, 0, len(selectionSet))
	for selection := range selectionSet {
		selections = append(selections, selection)
	}
	sort.Slice(selections, func(i, j int) bool {
		return compactKbuildSelectionKeyLess(selections[i], selections[j])
	})
	return selections
}

func newConfigDependencyAnalysisContext(plan *ActionPlan) *configDependencyAnalysisContext {
	return newConfigDependencyAnalysisContextWithCache(plan, nil)
}

func newConfigDependencyAnalysisContextWithCache(
	plan *ActionPlan,
	shared *ActionPlanConfigDependencySharedCache,
) *configDependencyAnalysisContext {
	if shared == nil {
		shared = NewActionPlanConfigDependencySharedCache()
	} else {
		shared.initialize()
	}
	context := &configDependencyAnalysisContext{
		inputSets:                     newConfigDependencyInputSetQuery(plan),
		generated:                     actionPlanGeneratedConfigDependencyPaths(plan),
		preconfiguredByRoot:           map[string]map[string]string{},
		physicalFiles:                 shared.physicalFiles,
		sourceIncludePaths:            shared.sourceIncludePaths,
		sourcePathInterns:             newConfigDependencyPlanSourcePathInternCache(),
		parsed:                        shared.parsed,
		conditionalSyntax:             shared.conditionalSyntax,
		compilerPredefines:            shared.compilerPredefines,
		forcedHeaders:                 &configDependencyForcedHeaderCache{},
		nodes:                         map[string]ActionPlanNode{},
		canonicalOutputsByPath:        map[string][]configDependencyCanonicalOutput{},
		invocationPredecessorClosures: map[string][]string{},
		generatedText:                 map[configDependencyGeneratedTextKey]configDependencyGeneratedTextResult{},
		selectionsByProducer:          map[string][]compactKbuildSelectionKey{},
		selectionsByOutput:            map[configDependencySelectionOutputKey][]compactKbuildSelectionKey{},
		pathSensitiveAncestors:        map[string]bool{},
		pathSensitiveAncestorsKnown:   map[string]bool{},
		pathSensitiveAncestorsActive:  map[string]bool{},
	}
	if plan != nil {
		if plan.metadata != nil {
			context.autoconfDefinitions = newConfigDependencyResolvedAutoconfDefinitions(
				plan.metadata.configFragment,
			)
		}
		for _, node := range plan.Nodes {
			context.nodes[node.ID] = node
		}
		// Build this index from nodes rather than directly from plan.Nodes. The
		// analysis context has always treated the final occurrence of a repeated
		// node ID as authoritative; retaining that behavior keeps this purely an
		// acceleration of the former full-map scan.
		for producerID, node := range context.nodes {
			for slot, output := range node.Outputs {
				logical := canonicalKbuildRulePath(output.Path)
				if logical == "" || output.Tree == "" || !actionPlanOutputIsCanonical(output) {
					continue
				}
				context.canonicalOutputsByPath[logical] = append(
					context.canonicalOutputsByPath[logical],
					configDependencyCanonicalOutput{
						producerID: producerID,
						stage:      node.Stage,
						tree:       output.Tree,
						slot:       slot,
					},
				)
			}
		}
		if graph := plan.selectionGraph; graph != nil {
			for selection, producer := range graph.materializedProducers {
				context.selectionsByProducer[producer] = append(context.selectionsByProducer[producer], selection)
			}
			for selection := range graph.selections {
				key := configDependencySelectionOutputKey{stage: selection.stage, target: selection.target}
				context.selectionsByOutput[key] = append(context.selectionsByOutput[key], selection)
			}
			for producer := range context.selectionsByProducer {
				sort.Slice(context.selectionsByProducer[producer], func(i, j int) bool {
					return compactKbuildSelectionKeyLess(context.selectionsByProducer[producer][i], context.selectionsByProducer[producer][j])
				})
			}
			for key := range context.selectionsByOutput {
				sort.Slice(context.selectionsByOutput[key], func(i, j int) bool {
					return compactKbuildSelectionKeyLess(context.selectionsByOutput[key][i], context.selectionsByOutput[key][j])
				})
			}
		}
	}
	return context
}

func analyzeActionPlanNodeConfigDependencies(
	plan *ActionPlan,
	node ActionPlanNode,
	context *configDependencyAnalysisContext,
) (ConfigDependencySet, error) {
	if plan == nil {
		return ConfigDependencySet{}, fmt.Errorf("config dependency analysis requires an action plan")
	}
	if context == nil {
		context = newConfigDependencyAnalysisContext(plan)
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok {
		return ConfigDependencySet{}, fmt.Errorf("node %q references missing recipe %q", node.ID, node.Recipe)
	}
	explicitPath, explicitConfig := actionRecipeExplicitConfigProjection(recipe)
	compilerRecipe := recipe.Tool == "cc" || recipe.Tool == "cxx" || recipe.CompilerInvocation != nil
	if node.Kind != "compile" || !compilerRecipe {
		if len(recipe.ConfigProjectionPrefixes) != 0 {
			set, reason := projectedGeneratorConfigDependencies(plan, node, recipe)
			if reason != "" {
				return opaqueConfigDependency(reason), nil
			}
			return set, nil
		}
		if binding, used, err := configDependencyRecipeConfigSourceUseWithQuery(plan, node, recipe, true, context.inputSets); err != nil {
			return ConfigDependencySet{}, err
		} else if used {
			return opaqueConfigDependency("non-compiler recipe uses config source binding: " + binding), nil
		}
		if binding, used, err := configDependencyRecipeConfigInputUseWithQuery(plan, node, recipe, true, context.inputSets); err != nil {
			return ConfigDependencySet{}, err
		} else if used {
			return opaqueConfigDependency("non-compiler recipe uses config input binding: " + binding), nil
		}
		if explicitConfig {
			return opaqueConfigDependency("action explicitly consumes resolved config projection " + explicitPath), nil
		}
		if strings.HasPrefix(recipe.Tool, "input:") || recipe.Tool == "generated" || recipe.Tool == compactKbuildScriptRunnerRole || len(recipe.CommandReplays) != 0 {
			return opaqueConfigDependency("opaque program or source script may inspect the staged object tree"), nil
		}
		// A compiler driver can preprocess source operands while linking; its
		// action kind alone does not prove staged config is irrelevant.
		knownConfigFree := node.Kind == "archive" || node.Kind == "link-relocatable" ||
			node.Kind == "copy" || recipe.Tool == "as"
		if !knownConfigFree {
			stagesConfig, err := actionPlanNodeStagesConfigProjectionWithQuery(plan, node, recipe, context.inputSets)
			if err != nil {
				return ConfigDependencySet{}, err
			}
			if stagesConfig {
				return opaqueConfigDependency("unclassified action may inspect staged resolved config projections"), nil
			}
		}
		return ConfigDependencySet{}, nil
	}
	if binding, used, err := configDependencyRecipeConfigSourceUseWithQuery(plan, node, recipe, false, context.inputSets); err != nil {
		return ConfigDependencySet{}, err
	} else if used {
		return opaqueConfigDependency("compiler recipe uses config source binding outside its modeled compiler invocation: " + binding), nil
	}
	if binding, used, err := configDependencyRecipeConfigInputUseWithQuery(plan, node, recipe, false, context.inputSets); err != nil {
		return ConfigDependencySet{}, err
	} else if used {
		return opaqueConfigDependency("compiler recipe uses config input binding outside its modeled compiler invocation: " + binding), nil
	}
	if recipe.CompilerInvocation != nil && !recipe.CompilerInvocation.WorkingInputUsesComplete {
		stagesConfig, err := actionPlanNodeStagesConfigProjectionWithQuery(plan, node, recipe, context.inputSets)
		if err != nil {
			return ConfigDependencySet{}, err
		}
		if stagesConfig {
			return opaqueConfigDependency("compound compiler has an incomplete working-input projection and may inspect staged resolved config data"), nil
		}
	}
	// GCC and Clang can preprocess bytes supplied on stdin when argv contains
	// `-`. The source-closure scanner has no immutable pathname for that stream,
	// so even a non-config source binding can introduce otherwise invisible
	// CONFIG_* references. Keep all compiler stdin recipes on the full-config
	// fallback until stdin participates in the exact closure model.
	if recipe.Stdin != "" {
		return opaqueConfigDependency("compiler recipe consumes an unmodeled stdin stream"), nil
	}
	if len(recipe.ContentSubstitutions) != 0 {
		return opaqueConfigDependency("compiler recipe uses an unmodeled content substitution"), nil
	}
	if len(recipe.ArgumentTransforms) != 0 {
		return opaqueConfigDependency("compiler recipe uses an unmodeled argument transform"), nil
	}
	if plan.metadata != nil && node.Stage != "prehost" && node.Stage != "host" {
		if value := plan.metadata.configFragment["CONFIG_MODVERSIONS"]; value != "" && value != "n" {
			return opaqueConfigDependency("CONFIG_MODVERSIONS requires the complete config for genksyms command metadata"), nil
		}
	}
	compilerExplicitPath, compilerExplicitConfig := actionRecipeCompilerExplicitConfigProjection(recipe)
	if compilerExplicitConfig && compilerExplicitPath != "include/generated/autoconf.h" {
		return opaqueConfigDependency("compiler explicitly consumes non-autoconf config projection " + compilerExplicitPath), nil
	}
	invocation, invocationReason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
	if invocationReason != "" {
		return opaqueConfigDependency(invocationReason), nil
	}
	selections := context.selectionsForNode(node)
	if len(selections) == 0 {
		return opaqueConfigDependency("compiler action has no exact Kbuild profile"), nil
	}
	// Compiler-driver links preprocess their source inputs just like -c
	// invocations. An argv-only isolation proof misses quoted-relative headers,
	// generated autoconf includes, and dynamic preprocessing effects. Use the
	// same full scanner; uninspectable toolchain headers retain the opaque
	// source-tree and complete-config fallback instead of a partial projection.
	sets := make([]ConfigDependencySet, 0, len(selections))
	for _, selection := range selections {
		profile, ok := plan.selectionGraph.profiles[selection.profile]
		if !ok {
			sets = append(sets, opaqueConfigDependency("compiler action references a missing exact Kbuild profile"))
			continue
		}
		generatedText := context.boundGeneratedText(plan, profile, node, recipe)
		resolveGeneratedText := func(logical string) (configDependencyGeneratedText, bool) {
			return context.treeGeneratedText(plan, profile, selection, node, recipe, logical)
		}
		var unresolvedGenerated func(configDependencyUnavailableGeneratedHeader)
		if context.generatedHeaderDemands != nil {
			unresolvedGenerated = func(unavailable configDependencyUnavailableGeneratedHeader) {
				context.recordGeneratedHeaderDemand(plan, profile, selection, node, recipe, unavailable, context.generatedHeaderDemands, nil)
			}
		}
		var resolvedGenerated func(map[string]configDependencyScanFile)
		if context.observedHeaders != nil {
			resolvedGenerated = func(files map[string]configDependencyScanFile) {
				context.observedHeaders.readResolved(node, files)
			}
		}
		sourceLookup := newConfigDependencySourceLookup(profile)
		analyze := func(calls bool) ConfigDependencySet {
			var compilerGuards func(configDependencyCompilerPredefineProbe, configDependencyScanFile, configDependencyCompilerGuardHints, string)
			if plan.metadata != nil && plan.metadata.compilerGuardObserver != nil {
				compilerGuards = func(probe configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
					context.recordCompilerGuardHints(plan, node, invocation.tool, probe, file, hints, contentID)
				}
			}
			return actionPlanNodeConfigDependenciesForSourceLookup(
				plan, node, recipe, invocation, sourceLookup, context.generated, generatedText, resolveGeneratedText,
				context.preconfiguredConfigDependencyPaths(profile, plan.metadata), context.autoconfDefinitions,
				context.physicalFiles, context.parsed,
				context.conditionalSyntax, context.compilerPredefineRequests, context.compilerPredefines,
				context.forcedHeaders, unresolvedGenerated, resolvedGenerated, compilerGuards, calls,
			)
		}
		set := analyze(false)
		if configDependencyCallCoverageCandidate(set) && plan.metadata != nil && plan.metadata.compilerPredefines != nil {
			candidate := analyze(true)
			if !candidate.Opaque {
				set = candidate
			} else if set.Opaque {
				set.Reason += "; complete call proof: " + candidate.Reason
			}
		}
		if context.compilerGuardError != nil {
			return ConfigDependencySet{}, context.compilerGuardError
		}
		if set.Opaque {
			context.recordProspectiveNumericHeaderDemands(plan, profile, selection, node, recipe, generatedText)
		}
		sets = append(sets, set)
	}
	return UnionConfigDependencySets(sets...)
}

// AnalyzeActionPlanNodeConfigDependencies classifies one action and scans an
// exact compiler source/include closure when possible. Failure to understand a
// filesystem effect is represented by an opaque result, not by a partial set.
func AnalyzeActionPlanNodeConfigDependencies(plan *ActionPlan, node ActionPlanNode) (ConfigDependencySet, error) {
	return analyzeActionPlanNodeConfigDependencies(plan, node, nil)
}

// registerActionPlanCompilerProbeProjection records the predefine request for
// statically identified compiler invocations before config classification.
// A probe-dependent compound recipe can remain opaque during discovery because
// a whole Make-text argument hides output/dependency flags, then become a typed
// compile after replay resolves those flags.  Retaining this planner-only
// projection closes that fixed point without weakening the final action
// classifier: it registers compiler state, but does not make the opaque action
// reusable across configurations.
func registerActionPlanCompilerProbeProjection(plan *ActionPlan, node ActionPlanNode) error {
	return registerActionPlanCompilerProbeProjectionWithContext(plan, node, nil)
}

func registerActionPlanCompilerProbeProjectionWithContext(
	plan *ActionPlan,
	node ActionPlanNode,
	context *configDependencyAnalysisContext,
) error {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerPredefines == nil {
		return nil
	}
	projected, single := plan.compilerProbeInvocations[node.ID]
	if projected.OpaqueReason != "" {
		single = false
	}
	compound := plan.compoundCompilerProbes[node.ID]
	if !single && len(compound) == 0 {
		return nil
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok {
		return fmt.Errorf("node %q references missing recipe %q", node.ID, node.Recipe)
	}
	if single {
		if err := registerActionPlanCompilerProbeInvocation(plan, node, recipe, projected, context); err != nil {
			return err
		}
	}
	for _, command := range compound {
		// Preserve this command's concrete operand ownership independently of
		// its siblings. These registration-only twins never replace the
		// serialized recipe's single typed compiler contract.
		commandRecipe := recipe
		commandRecipe.CompilerInvocation = &command.Invocation
		if err := registerActionPlanCompilerProbeInvocation(plan, node, commandRecipe, command.Projection, context); err != nil {
			return err
		}
	}
	return nil
}

func registerActionPlanCompilerProbeInvocation(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	projected actionRecipeCompilerProbeInvocation,
	context *configDependencyAnalysisContext,
) error {
	if projected.OpaqueReason != "" {
		return nil
	}
	// Preserve concrete operand ownership when a typed invocation exists. The
	// synthetic invocation is needed only for an untyped discovery/compound
	// action; it must not replace concrete -include/-imacros operands on replay.
	recipe = cloneActionRecipe(recipe)
	recipe.Environment = maps.Clone(projected.Environment)
	if recipe.CompilerInvocation == nil && recipe.Tool != projected.Tool {
		recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
			Tool: projected.Tool, Arguments: slices.Clone(projected.Arguments),
		}
	}
	invocation, reason := actionPlanConfigDependencyCompilerInvocationWithProjection(plan, node, recipe, &projected)
	if reason != "" {
		return nil
	}
	sourcePaths, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		return err
	}
	if len(sourcePaths) == 0 {
		return nil
	}
	probe, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, sourcePaths)
	if reason != "" {
		return nil
	}
	var cache map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult
	if context != nil {
		cache = context.compilerPredefineRequests
	}
	_, err = actionPlanCompilerPredefines(
		plan, cache,
		actionPlanConfigDependencyScope(node), invocation.tool, probe.language,
		probe.arguments, probe.translationUnits, probe.environment,
	)
	if err != nil {
		var projectionError *compilerPredefineProjectionUnsupportedError
		if errors.As(err, &projectionError) {
			// The source action remains valid, but this optional optimization
			// cannot express its source-selected symbolic argv/environment in the
			// probe protocol. The final dependency analysis sees the same preserved
			// projection and classifies the compiler node opaque; the typed error is
			// raised before any incomplete predefine request enters the probe DAG.
			return nil
		}
		return fmt.Errorf("node %q compiler-probe projection: %w", node.ID, err)
	}
	return nil
}

// Keep the compiler-state request identical in projection registration, the
// source scanner, and discovery's exact-request memo check. Concrete operand
// ownership must be resolved before replacing argv with its symbolic twin.
func configDependencyCompilerPredefineProbeForActionInvocation(
	invocation configDependencyCompilerInvocation,
	sourcePaths []string,
) (configDependencyCompilerPredefineProbe, string) {
	sourcePaths = configDependencyCompilerPredefineSourcePaths(invocation, sourcePaths)
	if invocation.hasPredefineProjection {
		invocation.arguments = slices.Clone(invocation.predefineArguments)
		invocation.kbuildStart = invocation.predefineKbuildStart
		invocation.kbuildEnd = invocation.predefineKbuildEnd
		invocation.probeEnvironment = maps.Clone(invocation.predefineProbeEnvironment)
	}
	return configDependencyCompilerPredefineProbeForInvocation(invocation, sourcePaths)
}

// discoverActionPlanCompilerProbes registers the same requests as full
// annotation analysis, without constructing discarded dependency witnesses.
// Only an exact memoized, not-ready request permits skipping a compiler scan:
// that scan would stop at the unavailable compiler state before entering any
// source/header contents. Typed fallbacks and unsupported projections retain
// the original analyzer and all of its admission gates.
func discoverActionPlanCompilerProbes(plan *ActionPlan) error {
	if plan == nil {
		return fmt.Errorf("compiler probe discovery requires an action plan")
	}
	registration := &configDependencyAnalysisContext{
		compilerPredefineRequests: map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{},
	}
	var fallback *configDependencyAnalysisContext
	for _, node := range plan.Nodes {
		if err := registerActionPlanCompilerProbeProjectionWithContext(plan, node, registration); err != nil {
			return err
		}
		recipe, ok := plan.Recipes[node.Recipe]
		if !ok {
			return fmt.Errorf("node %q references missing recipe %q", node.ID, node.Recipe)
		}
		if node.Kind != "compile" || (recipe.Tool != "cc" && recipe.Tool != "cxx" && recipe.CompilerInvocation == nil) {
			continue
		}
		if actionPlanCompilerDiscoveryRequestPending(plan, node, recipe, registration.compilerPredefineRequests) {
			continue
		}
		if fallback == nil {
			fallback = newConfigDependencyAnalysisContext(plan)
			fallback.compilerPredefineRequests = registration.compilerPredefineRequests
		}
		if _, err := analyzeActionPlanNodeConfigDependencies(plan, node, fallback); err != nil {
			return err
		}
	}
	return nil
}

func actionPlanCompilerDiscoveryRequestPending(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	requests map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult,
) bool {
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
	if reason != "" {
		return false
	}
	sourcePaths, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil || len(sourcePaths) == 0 {
		return false
	}
	probe, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, sourcePaths)
	if reason != "" {
		return false
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	result, exists := requests[key]
	return exists && !result.ready
}

// BuildActionPlanConfigDependencyAnalysis analyzes every node in plan order.
// Callers normally invoke it after lowering and before content addressing,
// while the selection graph still maps provisional producers to profiles.
func BuildActionPlanConfigDependencyAnalysis(plan *ActionPlan) (*ActionPlanConfigDependencyAnalysis, error) {
	return BuildActionPlanConfigDependencyAnalysisWithCache(plan, nil)
}

// BuildActionPlanConfigDependencyAnalysisWithCache analyzes one plan while
// reusing immutable source observations and fully witnessed precise compiler
// results from sibling configurations. A nil cache preserves the isolated
// behavior of BuildActionPlanConfigDependencyAnalysis.
func BuildActionPlanConfigDependencyAnalysisWithCache(
	plan *ActionPlan,
	cache *ActionPlanConfigDependencySharedCache,
) (*ActionPlanConfigDependencyAnalysis, error) {
	return buildActionPlanConfigDependencyAnalysis(plan, cache, nil)
}

// BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands preserves
// ordinary annotations and additionally records the encountered unresolved
// generated-header frontier. It never continues scanning beyond an opaque stop.
func BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(
	plan *ActionPlan,
	cache *ActionPlanConfigDependencySharedCache,
) (*ActionPlanConfigDependencyAnalysis, error) {
	return buildActionPlanConfigDependencyAnalysis(plan, cache, &configDependencyGeneratedHeaderDemandCollector{})
}

func buildActionPlanConfigDependencyAnalysis(
	plan *ActionPlan,
	cache *ActionPlanConfigDependencySharedCache,
	demands *configDependencyGeneratedHeaderDemandCollector,
) (*ActionPlanConfigDependencyAnalysis, error) {
	return buildActionPlanConfigDependencyAnalysisWithObservations(plan, cache, demands, nil)
}

// BuildActionPlanConfigDependencyAnalysisWithObservedHeaders exposes only
// authenticated ordinary cut-root bytes to the existing include-owner proof.
// Every original cut node stays opaque. Completed-compiler caching is disabled
// in this mode because those entries do not carry actual-read receipts; the
// independently witnessed forced/ordinary-header caches remain available.
func BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(
	plan *ActionPlan,
	cache *ActionPlanConfigDependencySharedCache,
	replay *ActionPlanFamilyVerifiedReplay,
	observed *ActionPlanFamilyObservedHeaders,
) (*ActionPlanConfigDependencyAnalysis, error) {
	observations, err := newConfigDependencyObservedHeaders(plan, replay, observed)
	if err != nil {
		return nil, err
	}
	return buildActionPlanConfigDependencyAnalysisWithObservations(
		plan, cache, &configDependencyGeneratedHeaderDemandCollector{}, observations,
	)
}

func buildActionPlanConfigDependencyAnalysisWithObservations(
	plan *ActionPlan,
	cache *ActionPlanConfigDependencySharedCache,
	demands *configDependencyGeneratedHeaderDemandCollector,
	observations *configDependencyObservedHeaders,
) (*ActionPlanConfigDependencyAnalysis, error) {
	if plan == nil {
		return nil, fmt.Errorf("config dependency analysis requires an action plan")
	}
	diagnostics := newConfigDependencyDiagnostics(plan)
	if diagnostics != nil {
		defer diagnostics.end()
		diagnostics.publish("analysis_start")
	}
	witnesses, err := newConfigDependencyWitnessBuilder(plan)
	if err != nil {
		return nil, err
	}
	analysis := &ActionPlanConfigDependencyAnalysis{
		sets:      make([]ConfigDependencySet, 0, len(plan.Nodes)),
		witnesses: make([]configDependencyNodeWitness, 0, len(plan.Nodes)),
	}
	context := newConfigDependencyAnalysisContextWithCache(plan, cache)
	context.generatedHeaderDemands = demands
	if demands != nil {
		// The baseline retains its small encountered-frontier limits. Optional
		// bound candidates can have many consumers per producer, so collect
		// them under the existing family descriptor caps. Admission below and
		// across variants reserves all baseline records before this tier.
		maximumRecords, maximumBytes := demands.maximumRecords, demands.maximumBytes
		if maximumRecords <= 0 {
			maximumRecords = MaxActionPlanFamilyExecutionCutRecords
		}
		if maximumBytes <= 0 {
			maximumBytes = MaxActionPlanFamilyExecutionCutBytes
		}
		context.prospectiveHeaderDemands = &configDependencyGeneratedHeaderDemandCollector{
			maximumRecords: maximumRecords, maximumBytes: maximumBytes,
		}
	}
	context.observedHeaders = observations
	if diagnostics != nil {
		diagnostics.context = context
	}
	// Build owns one immutable plan/metadata snapshot. Cache exact successful
	// compiler projections across the registration fixed point and dependency
	// scan; standalone per-node analysis deliberately leaves this nil so callers
	// which replace a hand-constructed callback between calls retain its behavior.
	context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
	for _, node := range plan.Nodes {
		if diagnostics != nil && node.Kind == "compile" {
			diagnostics.compilerStart(node)
		}
		if err := registerActionPlanCompilerProbeProjectionWithContext(plan, node, context); err != nil {
			return nil, err
		}
		recipe, recipeExists := plan.Recipes[node.Recipe]
		candidate, cacheable := configDependencyCompletedCompilerCandidate{}, false
		if observations == nil && (plan.metadata == nil || plan.metadata.compilerGuardObserver == nil) {
			candidate, cacheable = configDependencyCompletedCompilerCandidateForNode(
				plan, node, recipe, context, cache,
			)
		}
		canonical, hit := ConfigDependencySet{}, false
		if observations != nil && observations.replay.opaqueNodes[node.ID] {
			// Probe registration above still runs for pinned compiler nodes.
			// Never let a precise cache entry prune their original cut contract.
			canonical, hit = opaqueConfigDependency("observed execution cut retains original configuration contract"), true
		} else if recipeExists && cacheable {
			canonical, hit = cache.completed.lookup(plan, node, recipe, context, candidate)
		}
		if !hit {
			set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil {
				return nil, err
			}
			canonical, err = CanonicalConfigDependencySet(set)
			if err != nil {
				return nil, fmt.Errorf("node %q config dependencies: %w", node.ID, err)
			}
			sourceClosureInterned := canonical.Opaque || len(canonical.SourcePaths) == 0 ||
				configDependencyCompletedInternSourcePaths(plan, context.sourcePathInterns, canonical.SourcePaths)
			if recipeExists && cacheable && sourceClosureInterned {
				cache.completed.store(plan, node, recipe, context, candidate, canonical)
			}
		}
		witness, err := witnesses.witness(node)
		if err != nil {
			return nil, err
		}
		if observations != nil {
			if err := observations.commitUses(node, canonical); err != nil {
				return nil, err
			}
		}
		analysis.sets = append(analysis.sets, canonical)
		analysis.witnesses = append(analysis.witnesses, witness)
		if diagnostics != nil && node.Kind == "compile" {
			diagnostics.compilerComplete(canonical)
		}
	}
	if demands != nil {
		// Finish all baseline evidence first. Retain prospective records in a
		// distinct sealed tier, so later family-wide limits cannot erase it.
		prospective := context.prospectiveHeaderDemands
		if !demands.acceptsProspective(prospective) {
			prospective = &configDependencyGeneratedHeaderDemandCollector{truncated: true}
		}
		if !prospective.truncated {
			for demand := range demands.records {
				if _, found := prospective.records[demand]; found {
					delete(prospective.records, demand)
					prospective.bytes -= configDependencyGeneratedHeaderDemandBytes(demand)
				}
			}
		}
		if err := demands.seal(plan, witnesses); err != nil {
			return nil, err
		}
		if err := prospective.seal(plan, witnesses); err != nil {
			return nil, err
		}
		analysis.generatedHeaderDemands = demands
		analysis.prospectiveHeaderDemands = prospective
	}
	if observations != nil {
		if err := observations.uses.witnesses.seal(plan, witnesses); err != nil {
			return nil, err
		}
		// Detach the receipt value: retaining a pointer into observations would
		// keep every authenticated content string and producer graph alive too.
		uses := observations.uses
		analysis.observedHeaderUses = &uses
	}
	return analysis, nil
}

// ConfigCapsule is one content-addressed six-file resolved-config projection.
// Files are keyed by their logical object-tree path.
type ConfigCapsule struct {
	ID    string
	Files map[string]string
}

func configCapsuleSelectedSymbols(set ConfigDependencySet) map[string]bool {
	selected := make(map[string]bool, len(set.Symbols))
	for _, symbol := range set.Symbols {
		selected[normalizeConfigDependencySymbol(symbol)] = true
	}
	return selected
}

func configProjectionLineSymbol(pathname, line string) string {
	trimmed := strings.TrimSpace(line)
	switch pathname {
	case ".config":
		if symbol, ok := parseUnsetConfig(trimmed); ok {
			return normalizeConfigDependencySymbol(symbol)
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if ok && isConfigKey(key) {
			return normalizeConfigDependencySymbol(key)
		}
	case "include/config/auto.conf":
		key, _, ok := strings.Cut(trimmed, "=")
		if ok && isConfigKey(key) {
			return normalizeConfigDependencySymbol(key)
		}
	case "include/generated/autoconf.h":
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && fields[0] == "#define" && isConfigKey(fields[1]) {
			return normalizeConfigDependencySymbol(fields[1])
		}
	case "include/generated/rustc_cfg":
		value, ok := strings.CutPrefix(trimmed, "--cfg=")
		if !ok {
			return ""
		}
		if end := strings.IndexByte(value, '='); end >= 0 {
			value = value[:end]
		}
		if isConfigKey(value) {
			return normalizeConfigDependencySymbol(value)
		}
	}
	return ""
}

func filterConfigProjection(pathname, contents string, selected map[string]bool) string {
	if pathname == ".config" && contents == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(contents, "\n"), "\n")
	out := []string{}
	configPreamble := pathname == ".config"
	for index, line := range lines {
		symbol := configProjectionLineSymbol(pathname, line)
		if symbol != "" {
			configPreamble = false
			if selected[symbol] {
				out = append(out, line)
			} else if pathname == ".config" && index == 0 {
				// Keep the first-record boundary even when its symbol is not
				// selected. AWK can change FS in FNR == 1 after that record's
				// fields were already split. Promoting a later selected record
				// to first would change its value. The projection proof requires
				// this omitted record to have no other observable effect.
				out = append(out, "")
			}
			continue
		}
		if configPreamble || pathname == "include/generated/autoconf.h" {
			// Retain the original .config preamble in order; retain every
			// generated autoconf comment and include guard as before.
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

func configCapsuleID(files map[string]string) string {
	hash := sha256.New()
	for _, pathname := range ResolvedConfigProjectionOutputs() {
		contents := files[pathname]
		fmt.Fprintf(hash, "%08x:%s%016x:", len(pathname), pathname, len(contents))
		hash.Write([]byte(contents))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// RenderConfigCapsule projects the six resolved Kconfig files to one action's
// symbols. Opaque sets copy every input byte exactly. Non-opaque sets retain
// only selected symbol records, the autoconf include guard, and the
// configuration-independent auto.conf command record. kernel.release is
// retained only when CONFIG_LOCALVERSION participates in the dependency set.
func RenderConfigCapsule(full map[string]string, dependencies ConfigDependencySet) (ConfigCapsule, error) {
	set, err := CanonicalConfigDependencySet(dependencies)
	if err != nil {
		return ConfigCapsule{}, err
	}
	for _, pathname := range ResolvedConfigProjectionOutputs() {
		if _, ok := full[pathname]; !ok {
			return ConfigCapsule{}, fmt.Errorf("resolved config projection %q is missing", pathname)
		}
	}
	files := make(map[string]string, len(resolvedConfigProjections()))
	if set.Opaque {
		for _, pathname := range ResolvedConfigProjectionOutputs() {
			files[pathname] = full[pathname]
		}
	} else {
		selected := configCapsuleSelectedSymbols(set)
		files[".config"] = filterConfigProjection(".config", full[".config"], selected)
		files["include/config/auto.conf"] = filterConfigProjection(
			"include/config/auto.conf", full["include/config/auto.conf"], selected,
		)
		files["include/config/auto.conf.cmd"] = full["include/config/auto.conf.cmd"]
		files["include/generated/autoconf.h"] = filterConfigProjection(
			"include/generated/autoconf.h", full["include/generated/autoconf.h"], selected,
		)
		files["include/generated/rustc_cfg"] = filterConfigProjection(
			"include/generated/rustc_cfg", full["include/generated/rustc_cfg"], selected,
		)
		files["include/config/kernel.release"] = "\n"
		if selected["CONFIG_LOCALVERSION"] {
			files["include/config/kernel.release"] = full["include/config/kernel.release"]
		}
	}
	return ConfigCapsule{ID: configCapsuleID(files), Files: files}, nil
}
