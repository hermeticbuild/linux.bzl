package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// A resource bound, not an assumption about compiler arithmetic. Transport
// authenticates the exact toolchain/context; source analysis separately proves
// the original nontextual binding and ordered expansion history.
const maxCompilerCounterExpansions = 4096

// A distinct nontextual binding: an ordinary definition spelling __COUNTER__
// must not gain stateful behavior.
type configDependencyCompilerCounterBinding struct {
	context, origin, identity string
}

func compilerCounterBindingIdentity(context, origin string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("compiler-counter-binding-v1:%q:%q", context, origin))))
}

func (b *configDependencyCompilerCounterBinding) valid(name string) bool {
	return b != nil && name == "__COUNTER__" && b.context != "" && len(b.context) <= 4096 &&
		b.origin != "" && len(b.origin) <= 4096 && b.identity == compilerCounterBindingIdentity(b.context, b.origin)
}

// Caller must supply authenticated initial definedness and a measured sequence
// for this exact request, on a fresh branch of the cached initial state, before
// installing other context bindings or applying argv/source writes. Textual
// definitions and prior cursors are never promoted.
func (s *configDependencyMacroState) installInitialCompilerCounter(
	definitions map[string]bool, context configDependencyCompilerPredefineRequestKey, predefines, definednessID string,
	sequence *compilerCounterSequence,
) bool {
	if sequence == nil {
		return false
	}
	return s.installInitialCompilerCounterBinding(definitions, context, predefines, definednessID, sequence)
}

// A pending binding permits only observation of a future query demand. Without
// a measured vector, actual expansion still fails and publishes no source proof.
func (s *configDependencyMacroState) installInitialCompilerCounterBinding(
	definitions map[string]bool, context configDependencyCompilerPredefineRequestKey, predefines, definednessID string,
	sequence *compilerCounterSequence,
) bool {
	if !s.validSnapshotLineage() || s.macroReplacements == nil || context == "" || definednessID == "" ||
		s.parent == nil || s.parent.parent != nil || s.parent.compilerPredefinedSnapshot != s.parent.snapshot ||
		s.snapshotRevision != 0 || !s.snapshotChanges.empty() ||
		s.counter != (compilerCounterCursor{}) || !definitions["__COUNTER__"] ||
		s.definition("__COUNTER__") != configDependencyMacroDefined {
		return false
	}
	if _, present := s.macroReplacements["__COUNTER__"]; present {
		return false
	}
	contextID := fmt.Sprintf("%x", sha256.Sum256([]byte(context)))
	if sequence != nil && (sequence.context != contextID || validateCompilerCounterSequenceCount(len(sequence.values)) != nil) {
		return false
	}
	binding := &configDependencyCompilerCounterBinding{
		context: contextID,
		origin:  fmt.Sprintf("compiler-initial:%x:%s", sha256.Sum256([]byte(predefines)), definednessID),
	}
	binding.identity = compilerCounterBindingIdentity(binding.context, binding.origin)
	if !binding.valid("__COUNTER__") {
		return false
	}
	s.macroReplacements["__COUNTER__"] = configDependencyMacroReplacement{counter: binding}
	if sequence != nil {
		s.counter = compilerCounterCursor{sequence: sequence, known: true}
	}
	s.snapshotRevision++
	return true
}

func validateCompilerCounterSequenceCount(count int) error {
	if count < 1 || count > maxCompilerCounterExpansions {
		return fmt.Errorf("counter sequence requires 1 to %d expansions", maxCompilerCounterExpansions)
	}
	return nil
}

func compilerCounterSequenceSource(count int) (string, error) {
	if err := validateCompilerCounterSequenceCount(count); err != nil {
		return "", err
	}
	// An admitted object-macro spelling, not assumed availability, starting
	// value or increment. The configured compiler supplies every token.
	return strings.Repeat("__COUNTER__\n", count), nil
}

type compilerCounterSequence struct {
	context, identity string
	values            []string
}

func parseCompilerCounterSequence(context string, count int, contents string) (*compilerCounterSequence, error) {
	if context == "" || len(context) > 4096 {
		return nil, fmt.Errorf("counter sequence requires a bounded compiler context")
	}
	if err := validateCompilerCounterSequenceCount(count); err != nil {
		return nil, err
	}
	if len(contents) > MaxProbeInterpolatedBytes {
		return nil, fmt.Errorf("counter sequence exceeds result byte limit")
	}
	values := make([]string, 0, count)
	for token := range strings.FieldsFuncSeq(contents, func(c rune) bool {
		return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
	}) {
		if len(values) == count {
			return nil, fmt.Errorf("counter sequence contains excess tokens")
		}
		value, err := parseCompilerIntrinsicIntegerResult(token)
		if err != nil {
			return nil, fmt.Errorf("counter expansion %d: %w", len(values), err)
		}
		values = append(values, strings.Clone(value))
	}
	if len(values) != count {
		return nil, fmt.Errorf("counter sequence contains %d tokens, want %d", len(values), count)
	}
	var identity strings.Builder
	appendConfigDependencyCacheString(&identity, "compiler-counter-sequence-v1")
	appendConfigDependencyCacheString(&identity, context)
	for _, value := range values {
		appendConfigDependencyCacheString(&identity, value)
	}
	return &compilerCounterSequence{
		context: strings.Clone(context), values: values,
		identity: fmt.Sprintf("%x", sha256.Sum256([]byte(identity.String()))),
	}, nil
}

// Branch-local value over an immutable answer vector. Success returns a new
// cursor; failure cannot consume an expansion in its caller. Integration must
// bind this state into every replay, cache and branch identity.
type compilerCounterCursor struct {
	sequence *compilerCounterSequence
	next     int
	known    bool
}

type compilerCounterRead struct {
	SequenceID string
	Ordinal    int
	Token      string
}

func (c compilerCounterCursor) expand(context string) (compilerCounterRead, compilerCounterCursor, error) {
	if !c.known || c.sequence == nil || c.sequence.context != context || c.next < 0 || c.next >= len(c.sequence.values) {
		return compilerCounterRead{}, c, fmt.Errorf("counter expansion has no exact available state")
	}
	read := compilerCounterRead{SequenceID: c.sequence.identity, Ordinal: c.next, Token: c.sequence.values[c.next]}
	c.next++
	return read, c, nil
}

func joinCompilerCounterCursors(left, right compilerCounterCursor) compilerCounterCursor {
	if !left.validPosition() || !right.validPosition() || left.sequence.identity != right.sequence.identity || left.next != right.next {
		return compilerCounterCursor{}
	}
	return left
}

func (c compilerCounterCursor) validPosition() bool {
	return c.known && c.sequence != nil && c.next >= 0 && c.next <= len(c.sequence.values)
}

// The source observer must call this only AFTER all surrounding boundary and
// coverage checks pass. It validates the entire ordered transition first; a
// failed or stale transaction cannot publish a prefix or invalidate siblings.
// Callers cannot publish a successful source proof before this transition.
func (s *configDependencyMacroState) commitCounterExpansion(before compilerCounterCursor, result configDependencyMacroCallResult) bool {
	if !s.validSnapshotLineage() || s.counter != before || len(result.CounterReads) > maxCompilerCounterExpansions {
		return false
	}
	next := before
	for _, read := range result.CounterReads {
		if !next.validPosition() {
			return false
		}
		expected, advanced, err := next.expand(next.sequence.context)
		if err != nil || expected != read {
			return false
		}
		next = advanced
	}
	if next != result.Counter {
		return false
	}
	if s.counter != next {
		s.counter = next
		s.snapshotRevision++
	}
	return true
}
