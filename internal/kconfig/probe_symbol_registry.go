package kconfig

// A Kbuild workload evaluates target- and host-selected source fragments in
// one symbolic graph.  The source phase which first encounters a query is not
// necessarily the scope which will later consume its value: Linux computes
// host getconf flags early and subsequently embeds them in HOSTCC commands.
// Keep the evaluator-local maps as the ownership surface used by Make
// comparisons, while publishing immutable symbols to this workload registry
// so the evaluator selected by a later command can adopt a compatible graph.

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type linuxProbeSymbolRegistry struct {
	mu          sync.RWMutex
	symbols     map[string]linuxProbeSymbol
	definitions map[string]linuxProbeRequestDefinition
	// toolsetPaths is the exact set of scope/path results authenticated by this
	// workload's replay oracle. It permits a stable deterministic provenance
	// token emitted in a prior resolved config to re-enter through the same
	// source-owned Kconfig probe graph without trusting arbitrary raw tokens.
	toolsetPaths map[string]bool

	toolsetPathCapabilityOnce  sync.Once
	toolsetPathCapabilityCodec *toolaction.ExecutionRootProvenanceCapabilityCodec
	toolsetPathCapabilityErr   error
}

func (r *linuxProbeSymbolRegistry) executionRootProvenanceCapabilityCodec() (*toolaction.ExecutionRootProvenanceCapabilityCodec, error) {
	if r == nil {
		return nil, fmt.Errorf("Linux probe symbol registry is nil")
	}
	r.toolsetPathCapabilityOnce.Do(func() {
		r.toolsetPathCapabilityCodec, r.toolsetPathCapabilityErr = toolaction.NewExecutionRootProvenanceCapabilityCodec()
	})
	if r.toolsetPathCapabilityErr != nil {
		return nil, r.toolsetPathCapabilityErr
	}
	if r.toolsetPathCapabilityCodec == nil {
		return nil, fmt.Errorf("Linux probe symbol registry has no toolset-path capability codec")
	}
	return r.toolsetPathCapabilityCodec, nil
}

func newLinuxProbeSymbolRegistry() *linuxProbeSymbolRegistry {
	return &linuxProbeSymbolRegistry{
		symbols:      map[string]linuxProbeSymbol{},
		definitions:  map[string]linuxProbeRequestDefinition{},
		toolsetPaths: map[string]bool{},
	}
}

func linuxProbeToolsetPathKey(scope, canonical string) string {
	return scope + "\x00" + canonical
}

func (r *linuxProbeSymbolRegistry) authorizeToolsetPath(scope, canonical string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.toolsetPaths == nil {
		r.toolsetPaths = map[string]bool{}
	}
	r.toolsetPaths[linuxProbeToolsetPathKey(scope, canonical)] = true
	r.mu.Unlock()
}

func (r *linuxProbeSymbolRegistry) authorizesToolsetPath(scope, canonical string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	authorized := r.toolsetPaths[linuxProbeToolsetPathKey(scope, canonical)]
	r.mu.RUnlock()
	return authorized
}

type linuxProbeRequestDefinition struct {
	reference    ProbeReference
	request      ProbeRequest
	dependencies []ProbeReference
}

func (r *linuxProbeSymbolRegistry) publishDefinition(
	reference ProbeReference,
	request ProbeRequest,
	dependencies []ProbeReference,
) error {
	if r == nil {
		return nil
	}
	definition := linuxProbeRequestDefinition{
		reference:    reference,
		request:      request,
		dependencies: append([]ProbeReference(nil), dependencies...),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.definitions[reference.NodeID]; ok && !reflect.DeepEqual(existing, definition) {
		return fmt.Errorf("Linux probe node definition collision %q", reference.NodeID)
	}
	r.definitions[reference.NodeID] = definition
	return nil
}

func (r *linuxProbeSymbolRegistry) lookupDefinition(nodeID string) (linuxProbeRequestDefinition, bool) {
	if r == nil {
		return linuxProbeRequestDefinition{}, false
	}
	r.mu.RLock()
	definition, ok := r.definitions[nodeID]
	r.mu.RUnlock()
	definition.dependencies = append([]ProbeReference(nil), definition.dependencies...)
	return definition, ok
}

func (r *linuxProbeSymbolRegistry) publish(token string, symbol linuxProbeSymbol) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.symbols[token]; ok && !reflect.DeepEqual(existing, symbol) {
		return fmt.Errorf("Linux probe symbolic value collision %q", token)
	}
	r.symbols[token] = symbol
	return nil
}

func (r *linuxProbeSymbolRegistry) lookup(token string) (linuxProbeSymbol, bool) {
	if r == nil {
		return linuxProbeSymbol{}, false
	}
	r.mu.RLock()
	symbol, ok := r.symbols[token]
	r.mu.RUnlock()
	return symbol, ok
}

func (e *LinuxProbeEvaluator) publishSymbol(token string, symbol linuxProbeSymbol) error {
	if err := e.symbolRegistry.publish(token, symbol); err != nil {
		return err
	}
	e.symbols[token] = symbol
	return nil
}

// adoptSymbol copies one registry symbol and its nested symbolic text into the
// selected evaluator's local ownership map.  A host action may only adopt
// host-result graphs; target actions may consume either target results or the
// host prerequisites permitted by ProbePlanBuilder.  This check happens
// before argv lowering, giving cross-scope mistakes a deterministic planning
// error instead of relying on an eventual runner failure.
func (e *LinuxProbeEvaluator) adoptSymbol(token string) (linuxProbeSymbol, bool, error) {
	return e.adoptSymbolRecursive(token, &linuxProbeAdoptionState{visiting: map[string]bool{}})
}

type linuxProbeAdoptionState struct {
	visiting map[string]bool
	nodes    int
	depth    int
}

func (e *LinuxProbeEvaluator) adoptSymbolRecursive(token string, state *linuxProbeAdoptionState) (linuxProbeSymbol, bool, error) {
	if symbol, ok := e.symbols[token]; ok {
		return symbol, true, nil
	}
	symbol, ok := e.symbolRegistry.lookup(token)
	if !ok {
		return linuxProbeSymbol{}, false, nil
	}
	if state.visiting[token] {
		return linuxProbeSymbol{}, false, fmt.Errorf("cyclic Linux probe symbolic value %q during scope adoption", token)
	}
	if state.nodes >= maxLinuxProbeResolveNodes {
		return linuxProbeSymbol{}, false, fmt.Errorf("Linux probe symbolic scope adoption exceeds %d nodes", maxLinuxProbeResolveNodes)
	}
	if state.depth >= maxLinuxProbeResolveDepth {
		return linuxProbeSymbol{}, false, fmt.Errorf("Linux probe symbolic scope adoption exceeds depth %d", maxLinuxProbeResolveDepth)
	}
	state.nodes++
	state.depth++
	defer func() { state.depth-- }()
	state.visiting[token] = true
	defer delete(state.visiting, token)

	compatibleReference := func(reference ProbeReference) error {
		if reference.NodeID == "" {
			return nil
		}
		if reference.Scope != "target" && reference.Scope != "host" {
			return fmt.Errorf("Linux probe symbolic value %q has invalid result scope %q", token, reference.Scope)
		}
		if e.scope == "host" && reference.Scope != "host" {
			return fmt.Errorf("host Linux probe evaluator cannot adopt target-scoped symbolic value %q", token)
		}
		return nil
	}
	if err := compatibleReference(symbol.reference); err != nil {
		return linuxProbeSymbol{}, false, err
	}
	for _, dependency := range symbol.dependencies {
		if err := compatibleReference(dependency); err != nil {
			return linuxProbeSymbol{}, false, err
		}
	}
	for _, input := range symbol.selectionInputs {
		if err := compatibleReference(input.reference); err != nil {
			return linuxProbeSymbol{}, false, err
		}
		for _, dependency := range input.dependencies {
			if err := compatibleReference(dependency); err != nil {
				return linuxProbeSymbol{}, false, err
			}
		}
	}
	for _, scope := range symbol.toolsetPathScopes {
		if scope != "target" && scope != "host" {
			return linuxProbeSymbol{}, false, fmt.Errorf("Linux probe symbolic value %q has invalid toolset-path scope %q", token, scope)
		}
		if e.scope == "host" && scope != "host" {
			return linuxProbeSymbol{}, false, fmt.Errorf("host Linux probe evaluator cannot adopt target-scoped toolset path %q", token)
		}
	}

	values := []string{symbol.trueText, symbol.falseText}
	values = append(values, symbol.selectionValues...)
	if symbol.textTransform != nil {
		values = append(values, symbol.textTransform.sourceToken)
		values = append(values, symbol.textTransform.arguments...)
	}
	if symbol.makeText != nil {
		values = append(values, symbol.makeText.arguments...)
		values = append(values, symbol.makeText.protocolValue)
		for _, transform := range symbol.makeText.protocolTransforms {
			values = append(values, transform.arguments...)
		}
	}
	for _, value := range values {
		for nested := range linuxProbeSymbolPattern.AllString(value) {
			if _, exists, err := e.adoptSymbolRecursive(nested, state); err != nil {
				return linuxProbeSymbol{}, false, err
			} else if !exists {
				return linuxProbeSymbol{}, false, fmt.Errorf("Linux probe symbolic value %q references unknown nested value %q", token, nested)
			}
		}
	}
	e.symbols[token] = symbol
	return symbol, true, nil
}
