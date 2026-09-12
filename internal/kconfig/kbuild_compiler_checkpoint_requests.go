package kconfig

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

const compilerCheckpointStdinThreshold = 256
const compilerCheckpointStdinReferenceLimit = 1 << 20

// Large query programs are identical across many distinct compiler contexts.
// Intern only their exact bytes; request IDs, ordered inputs and scope bindings
// still refer to the complete original protocol request. The checkpoint's 64 MiB
// wire budget remains unchanged. Expanded validation work is independently
// bounded by the existing action-plan byte ceiling, not dictionary size alone.
func packCompilerCheckpointStdin(record *kbuildCompilerCheckpoint) error {
	if record.Plan == nil {
		return fmt.Errorf("compiler stdin dictionary requires a plan")
	}
	words := map[string]bool{}
	for _, request := range record.Plan.Requests {
		for _, step := range request.Steps {
			if len(step.Stdin) >= compilerCheckpointStdinThreshold {
				words[step.Stdin] = true
			}
		}
	}
	record.StdinPrograms = slices.Sorted(maps.Keys(words))
	indices := make(map[string]int, len(words))
	for index, value := range record.StdinPrograms {
		indices[value] = index
	}
	plan := *record.Plan
	plan.Requests = maps.Clone(plan.Requests)
	record.StdinReferences = map[string][]int{}
	for id, request := range plan.Requests {
		var refs []int
		for index, step := range request.Steps {
			if len(step.Stdin) < compilerCheckpointStdinThreshold {
				continue
			}
			if refs == nil {
				refs = make([]int, len(request.Steps))
				for i := range refs {
					refs[i] = -1
				}
				request.Steps = slices.Clone(request.Steps)
			}
			refs[index] = indices[step.Stdin]
			request.Steps[index].Stdin = ""
		}
		if refs != nil {
			record.StdinReferences[id] = refs
			plan.Requests[id] = request
		}
	}
	record.Plan = &plan
	return validateCompilerCheckpointStdin(record)
}

// Check all extents and expansion before rebinding any request. A malformed
// dictionary cannot supply an alternative payload, unused words or amplification.
func validateCompilerCheckpointStdin(record *kbuildCompilerCheckpoint) error {
	if record.Plan == nil || record.StdinReferences == nil || len(record.StdinPrograms) > compilerCheckpointStdinReferenceLimit || len(record.StdinReferences) > compilerCheckpointStdinReferenceLimit {
		return fmt.Errorf("compiler stdin dictionary exceeds table budget")
	}
	used := make([]bool, len(record.StdinPrograms))
	for index, value := range record.StdinPrograms {
		if len(value) < compilerCheckpointStdinThreshold || len(value) > MaxProbeInterpolatedBytes || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || index > 0 && record.StdinPrograms[index-1] >= value {
			return fmt.Errorf("compiler stdin dictionary has a noncanonical program")
		}
	}
	references, expanded := 0, 0
	for id, refs := range record.StdinReferences {
		request, exists := record.Plan.Requests[id]
		if !exists || len(refs) != len(request.Steps) || len(refs) == 0 {
			return fmt.Errorf("compiler stdin dictionary has an invalid request binding")
		}
		references += len(refs)
		if references > compilerCheckpointStdinReferenceLimit {
			return fmt.Errorf("compiler stdin dictionary exceeds reference budget")
		}
		bound := false
		for index, ref := range refs {
			if ref == -1 {
				continue
			}
			if ref < 0 || ref >= len(used) || request.Steps[index].Stdin != "" {
				return fmt.Errorf("compiler stdin dictionary has an invalid program binding")
			}
			if len(record.StdinPrograms[ref]) > MaxActionPlanSnapshotBytes-expanded {
				return fmt.Errorf("compiler stdin dictionary exceeds expanded plan budget")
			}
			expanded += len(record.StdinPrograms[ref])
			used[ref], bound = true, true
		}
		if !bound {
			return fmt.Errorf("compiler stdin dictionary has an empty request binding")
		}
	}
	for _, request := range record.Plan.Requests {
		for _, step := range request.Steps {
			if len(step.Stdin) >= compilerCheckpointStdinThreshold {
				return fmt.Errorf("compiler stdin dictionary omitted a program binding")
			}
		}
	}
	if slices.Contains(used, false) {
		return fmt.Errorf("compiler stdin dictionary has an unused program")
	}
	return nil
}

func unpackCompilerCheckpointStdin(record *kbuildCompilerCheckpoint) error {
	if err := validateCompilerCheckpointStdin(record); err != nil {
		return err
	}
	for id, refs := range record.StdinReferences {
		request := record.Plan.Requests[id]
		for index, ref := range refs {
			if ref >= 0 {
				request.Steps[index].Stdin = record.StdinPrograms[ref]
			}
		}
		record.Plan.Requests[id] = request
	}
	return nil
}

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
