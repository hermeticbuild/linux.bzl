package kconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Scan the complete serialized lowered payload, not a hand-maintained list of
// action fields. Tokens use a JSON-invariant ASCII alphabet. Compiler requests,
// both kinds of scope environment and original definitions are independent roots.
// Scanning each retained record recursively preserves all seven symbol kinds,
// including references inside Make transforms, protocol values and shell text.
func retainCompilerCheckpointSymbols(record *kbuildCompilerCheckpoint, actionPlan []byte) error {
	roots := *record
	roots.Symbols = nil
	data, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	retained := map[string]kbuildCheckpointSymbol{}
	var pending []string
	visit := func(data []byte) error {
		for token := range linuxProbeSymbolPattern.AllString(string(data)) {
			if _, seen := retained[token]; seen {
				continue
			}
			symbol, found := record.Symbols[token]
			if !found {
				return fmt.Errorf("compiler checkpoint has an unknown reachable symbol")
			}
			retained[token] = symbol
			pending = append(pending, token)
		}
		return nil
	}
	if err := visit(actionPlan); err != nil {
		return err
	}
	if err := visit(data); err != nil {
		return err
	}
	for index := 0; index < len(pending); index++ {
		data, err := json.Marshal(retained[pending[index]])
		if err != nil {
			return err
		}
		if err := visit(data); err != nil {
			return err
		}
	}
	record.Symbols = retained
	return nil
}

// Size-only diagnostics identify the remaining transport cost without logging
// source programs, compiler arguments, environments or result payloads.
func compilerCheckpointByteBudgetError(record kbuildCompilerCheckpoint, total int) error {
	size := func(value any) int {
		data, err := json.Marshal(value)
		if err != nil {
			return -1
		}
		return len(data)
	}
	return fmt.Errorf("compiler checkpoint exceeds byte budget: bytes=%d limit=%d plan_bytes=%d definition_bytes=%d symbol_bytes=%d shape_bytes=%d definitions=%d symbols=%d", total, maxKbuildCompilerCheckpointBytes, size(record.Plan), size(record.Definitions), size(record.Symbols), size(record.EmptyContainers), len(record.Definitions), len(record.Symbols))
}

// A definition's request is fully determined by its original reference.
// Keeping another copy in each symbol/selection scales with memberships and
// can duplicate large source/compiler argument programs thousands of times.
// Only the validated plan owns request bytes and their omitted-container shape.
func bindCompilerCheckpointRequests(record *kbuildCompilerCheckpoint, capture bool) error {
	if record.Plan == nil {
		return fmt.Errorf("compiler checkpoint requests require a plan")
	}
	bind := func(definition *kbuildCheckpointDefinition) error {
		var request ProbeRequest
		if definition.Reference != (ProbeReference{}) {
			var exists bool
			request, exists = record.Plan.Requests[definition.Reference.RequestID]
			if !exists {
				return fmt.Errorf("compiler checkpoint definition has no interned request")
			}
		}
		if capture && !reflect.DeepEqual(definition.Request, request) {
			return fmt.Errorf("compiler checkpoint definition differs from its interned request")
		}
		definition.Request = request
		return nil
	}
	for id, definition := range record.Definitions {
		if err := bind(&definition); err != nil {
			return err
		}
		record.Definitions[id] = definition
	}
	for token, symbol := range record.Symbols {
		if err := bind(&symbol.Definition); err != nil {
			return err
		}
		for index := range symbol.SelectionInputs {
			if err := bind(&symbol.SelectionInputs[index]); err != nil {
				return err
			}
		}
		record.Symbols[token] = symbol
	}
	return nil
}
