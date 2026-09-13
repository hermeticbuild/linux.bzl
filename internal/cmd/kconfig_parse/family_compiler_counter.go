package main

import (
	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const familyCompilerCounterQueryKind = "counter-sequence"

// Count carries no per-token wire array, but charge both its framing and the
// generated input. The total value budget is shared with intrinsic calls.
// Call only after count validation; untrusted integers never reach arithmetic.
func familyCompilerCounterPayloadBytes(count int) int {
	size, err := kconfig.CompilerCounterSequenceSourceBytes(count)
	if err != nil {
		return maxFamilyCompilerGuardExpandedBytes + 1
	}
	return size + 48
}

func (p *familyCompilerGuardPipeline) observeCounterSequence(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	query := familyCompilerGuardQuery{Kind: familyCompilerCounterQueryKind,
		Scope: value.Scope, Role: value.Role, Language: value.Language,
		Arguments: value.Arguments, TranslationUnits: value.TranslationUnits, Environment: value.Environment,
		CounterCount: value.CounterCount,
	}
	if err := query.validatePayload(); err != nil {
		return err
	}
	if err := query.compilerContext().validate(); err != nil {
		return err
	}
	key := query.contextKey()
	if p.counterKnown[key] >= query.CounterCount {
		return nil
	}
	entry := p.active[key]
	if entry != nil && entry.CounterCount >= query.CounterCount {
		return nil
	}
	previousCount, previousBytes := 0, 0
	if entry == nil {
		var err error
		query, err = p.internContext(query)
		if err != nil {
			return err
		}
		entry = &query
		p.active[key] = entry
	} else {
		previousCount = entry.CounterCount
		previousBytes = familyCompilerCounterPayloadBytes(previousCount)
	}
	entry.CounterCount = value.CounterCount
	p.callCount += entry.CounterCount - previousCount
	delta := familyCompilerCounterPayloadBytes(entry.CounterCount) - previousBytes
	p.bytes += delta
	p.expandedBytes += delta
	// As for mandatory intrinsic frontiers, overflow discards all newly
	// discovered work; it never publishes a partly charged vector or facts.
	if p.exceedsLimit("query_membership_limit", p.count, maxFamilyCompilerGuardMemberships) ||
		p.exceedsLimit("context_limit", len(p.contexts), maxFamilyCompilerGuardQueries) ||
		p.exceedsLimit("string_limit", len(p.contextStrings), maxFamilyCompilerGuardStrings) ||
		p.exceedsLimit("reference_limit", p.contextReferences, maxFamilyCompilerGuardReferences) ||
		p.exceedsLimit("intrinsic_value_limit", p.callCount, maxFamilyCompilerIntrinsicCalls) ||
		p.exceedsLimit("estimated_bytes_limit", p.bytes, maxFamilyCompilerGuardBytes/2) ||
		p.exceedsLimit("expanded_bytes_limit", p.expandedBytes, maxFamilyCompilerGuardExpandedBytes) {
		return nil
	}
	return nil
}
