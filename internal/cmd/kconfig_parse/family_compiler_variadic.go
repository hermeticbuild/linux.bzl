package main

import (
	"fmt"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const familyCompilerVariadicStage = "optional-variadic-grammar"
const familyCompilerVariadicLookaheadStage = "optional-literal-variadic-grammar"

// The kind carries one of two bounded source grammars, not a capability value.
// Keeping them separate permits independent rejection and exact replay.
func (q familyCompilerGuardQuery) variadicSyntax() string {
	switch q.Kind {
	case "variadic-comma-standard":
		return "standard"
	case "variadic-comma-named":
		return "named"
	default:
		return ""
	}
}

func (q familyCompilerGuardQuery) variadicValues() int {
	if q.variadicSyntax() != "" {
		return 1
	}
	return 0
}

func (q familyCompilerGuardQuery) variadicBytes() int {
	if syntax := q.variadicSyntax(); syntax != "" {
		size, _ := kconfig.CompilerVariadicCommaSourceBytes(syntax)
		return size
	}
	return 0
}

func (p *familyCompilerGuardPipeline) observeVariadicComma(value kconfig.ConfigDependencyCompilerGuardObservation) error {
	query := familyCompilerGuardQuery{Kind: "variadic-comma-" + value.VariadicCommaSyntax,
		Scope: value.Scope, Role: value.Role, Language: value.Language,
		Arguments: value.Arguments, TranslationUnits: value.TranslationUnits, Environment: value.Environment}
	if query.variadicSyntax() == "" {
		return fmt.Errorf("invalid variadic comma syntax")
	}
	if err := query.validatePayload(); err != nil {
		return err
	}
	if err := query.compilerContext().validate(); err != nil {
		return err
	}
	key := query.contextKey()
	if _, answered := p.known[key]; answered || p.rejected[query.payloadKey()] || p.active[key] != nil {
		return nil
	}
	ledger := p
	var stage *familyCompilerGuardOptionalStage
	if value.OptionalVariadicHints {
		destination, kind := &p.variadicHints, familyCompilerVariadicStage
		if value.LiteralIncludeHints {
			if p.variadicHints != nil && p.variadicHints.ledger.active[key] != nil {
				return nil
			}
			destination, kind = &p.variadicLookahead, familyCompilerVariadicLookaheadStage
		} else if p.variadicLookahead != nil {
			// Promotion affects scheduling only. Accounting stays conservative;
			// the weaker tier cannot consume another executable membership.
			delete(p.variadicLookahead.ledger.active, key)
		}
		if *destination == nil {
			*destination = &familyCompilerGuardOptionalStage{kind: kind,
				ledger: familyCompilerGuardPipeline{active: map[string]*familyCompilerGuardQuery{}}}
		}
		stage = *destination
		if stage.disabled {
			return nil
		}
		ledger = &stage.ledger
	}
	if ledger.active[key] != nil {
		return nil
	}
	var err error
	query, err = ledger.internContext(query)
	if err != nil {
		return err
	}
	if ledger.active == nil {
		ledger.active = map[string]*familyCompilerGuardQuery{}
	}
	ledger.active[key] = &query
	ledger.callCount++
	ledger.bytes += query.variadicBytes()
	ledger.expandedBytes += query.variadicBytes()
	if ledger.exceedsLimit("query_membership_limit", ledger.count, maxFamilyCompilerGuardMemberships) ||
		ledger.exceedsLimit("context_limit", len(ledger.contexts), maxFamilyCompilerGuardQueries) ||
		ledger.exceedsLimit("string_limit", len(ledger.contextStrings), maxFamilyCompilerGuardStrings) ||
		ledger.exceedsLimit("reference_limit", ledger.contextReferences, maxFamilyCompilerGuardReferences) ||
		ledger.exceedsLimit("intrinsic_value_limit", ledger.callCount, maxFamilyCompilerIntrinsicCalls) ||
		ledger.exceedsLimit("estimated_bytes_limit", ledger.bytes, maxFamilyCompilerGuardBytes/2) ||
		ledger.exceedsLimit("expanded_bytes_limit", ledger.expandedBytes, maxFamilyCompilerGuardExpandedBytes) {
		if stage != nil {
			stage.disable("staging_" + ledger.limitReason)
		}
	}
	return nil
}
