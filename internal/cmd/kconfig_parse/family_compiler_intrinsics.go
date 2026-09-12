package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const familyCompilerIntrinsicQueryKind = "intrinsic-integer"
const familyCompilerOptionalDefinednessQueryKind = "optional-definedness"
const familyCompilerTokenHintQueryKind = "optional-token-hints"
const familyCompilerLiteralHintQueryKind = "optional-literal-hints"

// Values are a bounded serialized payload, not separate compiler executions.
// Complete unique query descriptors and raw memberships have separate limits.
const maxFamilyCompilerIntrinsicCalls = 1 << 20

func familyCompilerIntrinsicStdinBytes(call kconfig.CompilerIntrinsicCall) int {
	// Exact framing of compilerIntrinsicIntegersSource's canonical snippet:
	// "#undef " + operand + "\n" + operator + "(" + operand + ")\n".
	return len(call.Operator) + 2*len(call.Operand) + 11
}

func compareFamilyCompilerIntrinsicCalls(left, right kconfig.CompilerIntrinsicCall) int {
	if order := strings.Compare(left.Operator, right.Operator); order != 0 {
		return order
	}
	return strings.Compare(left.Operand, right.Operand)
}

func familyCompilerIntrinsicCallKey(call kconfig.CompilerIntrinsicCall) string {
	// Both fields have already passed the bounded identifier grammar. Query
	// context keys include Kind, keeping these keys separate from Names.
	return call.Operator + "\x00" + call.Operand
}

func (q familyCompilerGuardQuery) validatePayload() error {
	switch q.Kind {
	case "", familyCompilerOptionalDefinednessQueryKind, familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind: // Ordinary definedness retains its original shape.
		if len(q.Calls) != 0 || len(q.Names) == 0 || !slices.IsSorted(q.Names) || len(slices.Compact(slices.Clone(q.Names))) != len(q.Names) {
			return fmt.Errorf("compiler definedness query has a noncanonical payload")
		}
	case familyCompilerIntrinsicQueryKind:
		if q.Names != nil || len(q.Calls) == 0 || len(q.Calls) > 4096 || q.Language != "c" && q.Language != "c++" {
			return fmt.Errorf("compiler intrinsic query has a noncanonical payload")
		}
		stdinBytes := 0
		for index, call := range q.Calls {
			if err := kconfig.ValidateCompilerIntrinsicCall(call); err != nil {
				return err
			}
			if index != 0 && compareFamilyCompilerIntrinsicCalls(q.Calls[index-1], call) >= 0 {
				return fmt.Errorf("compiler intrinsic query calls must be sorted and unique")
			}
			stdinBytes += familyCompilerIntrinsicStdinBytes(call)
			if stdinBytes > kconfig.MaxProbeInterpolatedBytes {
				return fmt.Errorf("compiler intrinsic query exceeds its stdin budget")
			}
		}
	default:
		return fmt.Errorf("unsupported compiler guard query kind %q", q.Kind)
	}
	return nil
}

func (q familyCompilerGuardQuery) evaluate(batch *kconfig.KbuildCompilerGuardBatch) (bool, error) {
	state, err := q.evaluateCompletion(batch)
	return state != kconfig.OptionalCompilerDefinednessPending, err
}

func (q familyCompilerGuardQuery) evaluateCompletion(batch *kconfig.KbuildCompilerGuardBatch) (kconfig.OptionalCompilerDefinednessState, error) {
	if err := q.validatePayload(); err != nil {
		return kconfig.OptionalCompilerDefinednessPending, err
	}
	if q.Kind == familyCompilerOptionalDefinednessQueryKind || q.Kind == familyCompilerTokenHintQueryKind || q.Kind == familyCompilerLiteralHintQueryKind {
		_, state, err := batch.OptionalCompilerDefinedness(q.Scope, q.Role, q.Language, q.Arguments, q.TranslationUnits, q.Names, q.Environment)
		return state, err
	}
	var ready bool
	var err error
	if q.Kind == "" {
		_, ready, err = batch.CompilerDefinedness(q.Scope, q.Role, q.Language, q.Arguments, q.TranslationUnits, q.Names, q.Environment)
	} else {
		_, ready, err = batch.CompilerIntrinsicIntegers(q.Scope, q.Role, q.Language, q.Arguments, q.TranslationUnits, q.Calls, q.Environment)
	}
	if err != nil || !ready {
		return kconfig.OptionalCompilerDefinednessPending, err
	}
	return kconfig.OptionalCompilerDefinednessAnswered, nil
}

func (p *familyCompilerGuardPipeline) observeIntrinsicCalls(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	query := familyCompilerGuardQuery{
		Kind: familyCompilerIntrinsicQueryKind, Scope: value.Scope, Role: value.Role, Language: value.Language,
		Arguments: slices.Clone(value.Arguments), TranslationUnits: slices.Clone(value.TranslationUnits), Environment: maps.Clone(value.Environment),
	}
	if err := query.compilerContext().validate(); err != nil {
		return err
	}
	key := query.contextKey()
	for _, call := range value.Calls {
		if err := kconfig.ValidateCompilerIntrinsicCall(call); err != nil {
			return err
		}
		callKey := familyCompilerIntrinsicCallKey(call)
		if p.known[key][callKey] {
			continue
		}
		if p.known[key] == nil {
			p.known[key] = map[string]bool{}
		}
		p.known[key][callKey] = true
		entry := p.active[key]
		if entry == nil {
			var err error
			query, err = p.internContext(query)
			if err != nil {
				return err
			}
			entry = &query
			p.active[key] = entry
		}
		entry.Calls = append(entry.Calls, call)
		p.callCount++
		p.bytes += len(call.Operator) + len(call.Operand) + 48
		p.expandedBytes += len(call.Operator) + len(call.Operand) + 48
		if p.queryBytes == nil {
			p.queryBytes = map[string]int{}
		}
		p.queryBytes[key] += familyCompilerIntrinsicStdinBytes(call)
		// One context's canonical vector is one executable request. Retain
		// separate value/transport and exact generated-stdin bounds so an
		// overflow discards discovery before constructing mandatory actions.
		if p.exceedsLimit("query_membership_limit", p.count, maxFamilyCompilerGuardMemberships) ||
			p.exceedsLimit("context_limit", len(p.contexts), maxFamilyCompilerGuardQueries) ||
			p.exceedsLimit("string_limit", len(p.contextStrings), maxFamilyCompilerGuardStrings) ||
			p.exceedsLimit("reference_limit", p.contextReferences, maxFamilyCompilerGuardReferences) ||
			p.exceedsLimit("intrinsic_value_limit", p.callCount, maxFamilyCompilerIntrinsicCalls) ||
			p.exceedsLimit("estimated_bytes_limit", p.bytes, maxFamilyCompilerGuardBytes/2) ||
			p.exceedsLimit("expanded_bytes_limit", p.expandedBytes, maxFamilyCompilerGuardExpandedBytes) ||
			p.exceedsLimit("query_call_limit", len(entry.Calls), 4096) ||
			p.exceedsLimit("query_stdin_limit", p.queryBytes[key], kconfig.MaxProbeInterpolatedBytes) {
			return nil
		}
	}
	return nil
}
