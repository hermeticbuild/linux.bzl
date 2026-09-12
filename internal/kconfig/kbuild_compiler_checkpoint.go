package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

const kbuildCompilerCheckpointSchema = "linux-kbuild-compiler-checkpoint-v3"

// MaxKbuildCompilerCheckpointBytes bounds compiler request/expression transport.
const MaxKbuildCompilerCheckpointBytes = 64 << 20
const maxKbuildCompilerCheckpointBytes = MaxKbuildCompilerCheckpointBytes

type kbuildCheckpointDefinition struct {
	Reference ProbeReference `json:",omitzero"`
	// The wire format interns requests once in Plan.Requests. This field is
	// checked before capture and rebound after decoding, including nil/empty
	// shape. References and ordered dependencies retain their exact values.
	Request      ProbeRequest     `json:"-"`
	Dependencies []ProbeReference `json:",omitzero"`
}
type kbuildCheckpointTransform struct {
	SourceToken   string   `json:",omitzero"`
	Function      string   `json:",omitzero"`
	Arguments     []string `json:",omitzero"`
	InputArgument int      `json:",omitzero"`
}
type kbuildCheckpointMakeText struct {
	Function           string                         `json:",omitzero"`
	Arguments          []string                       `json:",omitzero"`
	ProtocolValue      string                         `json:",omitzero"`
	ProtocolMode       linuxProbeMakeTextProtocolMode `json:",omitzero"`
	ProtocolTransforms []kbuildCheckpointTransform    `json:",omitzero"`
}
type kbuildCheckpointSymbol struct {
	Kind               string
	Definition         kbuildCheckpointDefinition   `json:",omitzero"`
	TrueText           string                       `json:",omitzero"`
	FalseText          string                       `json:",omitzero"`
	SelectionInputs    []kbuildCheckpointDefinition `json:",omitzero"`
	SelectionValues    []string                     `json:",omitzero"`
	TextTransform      *kbuildCheckpointTransform   `json:",omitzero"`
	MakeText           *kbuildCheckpointMakeText    `json:",omitzero"`
	SourceShellWords   string                       `json:",omitzero"`
	ToolsetPathLiteral string                       `json:",omitzero"`
	ToolsetPathScopes  []string                     `json:",omitzero"`
}
type kbuildCheckpointScope struct {
	Architecture, SourceArchitecture string
	Environment                      map[string]string
	References                       []ProbeReference
}
type kbuildCompilerCheckpoint struct {
	EmptyContainers  [][]string
	Schema           string
	Plan             *ProbePlan
	Definitions      map[string]kbuildCheckpointDefinition
	Symbols          map[string]kbuildCheckpointSymbol
	Scopes           map[string]kbuildCheckpointScope
	BaseEnvironments map[string]map[string]string
}

func packKbuildCheckpointSymbol(symbol linuxProbeSymbol) kbuildCheckpointSymbol {
	r := kbuildCheckpointSymbol{Kind: symbol.kind, Definition: kbuildCheckpointDefinition{symbol.reference, symbol.request, symbol.dependencies}, TrueText: symbol.trueText, FalseText: symbol.falseText, SelectionValues: symbol.selectionValues, SourceShellWords: symbol.sourceShellWords, ToolsetPathLiteral: symbol.toolsetPathLiteral, ToolsetPathScopes: symbol.toolsetPathScopes}
	for _, input := range symbol.selectionInputs {
		r.SelectionInputs = append(r.SelectionInputs, kbuildCheckpointDefinition{input.reference, input.request, input.dependencies})
	}
	if v := symbol.textTransform; v != nil {
		r.TextTransform = &kbuildCheckpointTransform{v.sourceToken, v.function, v.arguments, v.inputArgument}
	}
	if v := symbol.makeText; v != nil {
		r.MakeText = &kbuildCheckpointMakeText{Function: v.function, Arguments: v.arguments, ProtocolValue: v.protocolValue, ProtocolMode: v.protocolMode}
		for _, x := range v.protocolTransforms {
			r.MakeText.ProtocolTransforms = append(r.MakeText.ProtocolTransforms, kbuildCheckpointTransform{Function: x.function, Arguments: x.arguments, InputArgument: x.inputArgument})
		}
	}
	return r
}
func unpackKbuildCheckpointSymbol(r kbuildCheckpointSymbol) linuxProbeSymbol {
	s := linuxProbeSymbol{kind: r.Kind, reference: r.Definition.Reference, request: r.Definition.Request, dependencies: r.Definition.Dependencies, trueText: r.TrueText, falseText: r.FalseText, selectionValues: r.SelectionValues, sourceShellWords: r.SourceShellWords, toolsetPathLiteral: r.ToolsetPathLiteral, toolsetPathScopes: r.ToolsetPathScopes}
	for _, d := range r.SelectionInputs {
		s.selectionInputs = append(s.selectionInputs, linuxProbeSelectionInput{d.Reference, d.Request, d.Dependencies})
	}
	if v := r.TextTransform; v != nil {
		s.textTransform = &linuxProbeTextTransform{v.SourceToken, v.Function, v.Arguments, v.InputArgument}
	}
	if v := r.MakeText; v != nil {
		s.makeText = &linuxProbeMakeText{function: v.Function, arguments: v.Arguments, protocolValue: v.ProtocolValue, protocolMode: v.ProtocolMode}
		for _, x := range v.ProtocolTransforms {
			s.makeText.protocolTransforms = append(s.makeText.protocolTransforms, linuxProbeMakeTextProtocolTransform{x.Function, x.Arguments, x.InputArgument})
		}
	}
	return s
}

// MarshalCompilerCheckpoint records ordinary request and symbolic-source state.
// Measured answers, source-read receipts, physical root bindings and capability
// codec keys are not exported. The enclosing plan checkpoint must bind these
// bytes to its producer's declared source/config/tool/probe action inputs.
func (s *KbuildProbeScopes) MarshalCompilerCheckpoint() ([]byte, error) {
	return s.marshalCompilerCheckpoint(nil)
}

// MarshalActionPlanCompilerCheckpoint keeps the symbolic closure of a lowered
// action-plan checkpoint and the ordinary compiler workload. Make evaluation
// also publishes intermediate expressions which the lowered program no longer
// references; those are not replay inputs. Requests, scope environments and all
// transitive expression dependencies remain exact, with the same size limits.
func (s *KbuildProbeScopes) MarshalActionPlanCompilerCheckpoint(actionPlan []byte) ([]byte, error) {
	if len(actionPlan) == 0 || len(actionPlan) > MaxActionPlanSnapshotBytes || !json.Valid(actionPlan) {
		return nil, fmt.Errorf("compiler checkpoint requires a valid lowered action-plan payload")
	}
	return s.marshalCompilerCheckpoint(actionPlan)
}

func (s *KbuildProbeScopes) marshalCompilerCheckpoint(actionPlan []byte) ([]byte, error) {
	if s == nil || s.evaluators["target"] == nil {
		return nil, fmt.Errorf("compiler checkpoint requires a target scope")
	}
	target := s.evaluators["target"]
	builder, ok := target.discovery.(*ProbePlanBuilder)
	if !ok {
		return nil, fmt.Errorf("compiler checkpoint requires an ordinary probe plan builder")
	}
	plan, err := builder.Plan(s.References()...)
	if err != nil {
		return nil, err
	}
	r := kbuildCompilerCheckpoint{Schema: kbuildCompilerCheckpointSchema, Plan: plan, Definitions: map[string]kbuildCheckpointDefinition{}, Symbols: map[string]kbuildCheckpointSymbol{}, Scopes: map[string]kbuildCheckpointScope{}, BaseEnvironments: s.baseScriptEnvironments}
	registry := target.symbolRegistry
	if registry == nil {
		return nil, fmt.Errorf("compiler checkpoint requires a symbol registry")
	}
	for name, e := range s.evaluators {
		if e.symbolRegistry != registry || e.discovery != builder {
			return nil, fmt.Errorf("compiler checkpoint scopes do not share a workload")
		}
		r.Scopes[name] = kbuildCheckpointScope{e.architecture, e.sourceArchitecture, e.scriptEnvironment, e.References()}
	}
	registry.mu.RLock()
	for id, d := range registry.definitions {
		r.Definitions[id] = kbuildCheckpointDefinition{d.reference, d.request, d.dependencies}
	}
	for token, symbol := range registry.symbols {
		r.Symbols[token] = packKbuildCheckpointSymbol(symbol)
	}
	if err := bindCompilerCheckpointRequests(&r, true); err != nil {
		registry.mu.RUnlock()
		return nil, err
	}
	if actionPlan != nil {
		if err := retainCompilerCheckpointSymbols(&r, actionPlan); err != nil {
			registry.mu.RUnlock()
			return nil, err
		}
	}
	r.EmptyContainers, err = compilerCheckpointEmptyContainers(r)
	if err != nil {
		registry.mu.RUnlock()
		return nil, err
	}
	data, err := json.Marshal(r)
	registry.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if len(data) > maxKbuildCompilerCheckpointBytes {
		return nil, compilerCheckpointByteBudgetError(r, len(data))
	}
	return data, nil
}

// RestoreCompilerCheckpoint installs source expressions in fresh configured
// scopes. Existing constructors revalidate every symbolic identity and grammar;
// current result oracles remain in use. Imported path literals require fresh
// upstream authorization, never an asserted path set or an old capability key.
// No scope or registry becomes visible on failure. The enclosing workload must
// abort if importing requests into its builder fails.
func (s *KbuildProbeScopes) RestoreCompilerCheckpoint(data []byte, normalizeUpstream func(string) (string, error)) error {
	if s == nil || s.evaluators["target"] == nil || len(data) > maxKbuildCompilerCheckpointBytes {
		return fmt.Errorf("invalid compiler checkpoint input")
	}
	var r kbuildCompilerCheckpoint
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return err
	}
	if err := restoreCompilerCheckpointEmptyContainers(&r); err != nil {
		return err
	}
	canonical, err := json.Marshal(r)
	if err != nil || !bytes.Equal(data, canonical) {
		return fmt.Errorf("noncanonical compiler checkpoint")
	}
	if r.Schema != kbuildCompilerCheckpointSchema || r.Plan == nil || r.Definitions == nil || r.Symbols == nil {
		return fmt.Errorf("incomplete compiler checkpoint")
	}
	if err := bindCompilerCheckpointRequests(&r, false); err != nil {
		return err
	}
	if _, err := r.Plan.entries(); err != nil {
		return err
	}
	if len(r.Scopes) != len(s.evaluators) || !reflect.DeepEqual(r.BaseEnvironments, s.baseScriptEnvironments) {
		return fmt.Errorf("compiler checkpoint changed configured scopes or base environments")
	}
	builder, ok := s.evaluators["target"].discovery.(*ProbePlanBuilder)
	if !ok {
		return fmt.Errorf("compiler checkpoint requires an ordinary probe plan builder")
	}
	registry := newLinuxProbeSymbolRegistry()
	staged := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{}, baseScriptEnvironments: s.baseScriptEnvironments, exactScriptEnvironmentBindings: map[string]map[string]map[string]string{}, exactScriptEnvironmentActivations: map[string]func() error{}}
	for name, current := range s.evaluators {
		record, found := r.Scopes[name]
		if !found || current.facts == nil || r.Plan.Toolsets[name] != current.facts.ToolsetIdentity() || current.discovery != builder || record.Architecture != current.architecture || record.SourceArchitecture != current.sourceArchitecture {
			return fmt.Errorf("compiler checkpoint changed scope %s", name)
		}
		if len(current.references) != 0 || len(current.symbols) != 0 || current.symbolRegistry == nil || len(current.symbolRegistry.symbols) != 0 {
			return fmt.Errorf("compiler checkpoint requires unused scopes")
		}
		fresh, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{Scope: name, Architecture: current.architecture, SourceArchitecture: current.sourceArchitecture, SourceRoot: current.sourceRoot, ScriptEnvironment: maps.Clone(record.Environment), Facts: current.facts, Tools: maps.Clone(current.tools), Discovery: builder, Oracle: current.oracle, RustSourceRoot: current.rustSourceRoot})
		if err != nil {
			return err
		}
		fresh.symbolRegistry = registry
		staged.evaluators[name] = fresh
	}
	if len(r.Plan.Toolsets) != len(staged.evaluators) {
		return fmt.Errorf("compiler checkpoint has extra toolsets")
	}
	refs := map[string]ProbeReference{}
	for _, node := range r.Plan.Nodes {
		var deps []ProbeReference
		for _, id := range node.Inputs {
			ref, exists := refs[id]
			if !exists {
				return fmt.Errorf("compiler checkpoint probe graph is not topological")
			}
			deps = append(deps, ref)
		}
		ref, err := builder.Request(node.Scope, r.Plan.Requests[node.RequestID], deps...)
		if err != nil {
			return err
		}
		if ref.NodeID != node.ID || ref.RequestID != node.RequestID {
			return fmt.Errorf("compiler checkpoint changed request identity")
		}
		refs[node.ID] = ref
	}
	for id, definition := range r.Definitions {
		ref, exists := refs[id]
		if !exists || ref != definition.Reference || !reflect.DeepEqual(definition.Request, r.Plan.Requests[ref.RequestID]) {
			return fmt.Errorf("compiler checkpoint definition differs from original request")
		}
		for _, dependency := range definition.Dependencies {
			if refs[dependency.NodeID] != dependency {
				return fmt.Errorf("compiler checkpoint has unknown definition dependency")
			}
		}
		ref, err := builder.Request(ref.Scope, definition.Request, definition.Dependencies...)
		if err != nil || ref != definition.Reference {
			return fmt.Errorf("compiler checkpoint changed definition dependency order: %v", err)
		}
		if err := registry.publishDefinition(definition.Reference, definition.Request, definition.Dependencies); err != nil {
			return err
		}
	}
	// Populate an isolated namespace so constructors can validate nested source
	// expressions regardless of lexical token order. Nothing is installed in the
	// caller until every record validates, including cycles and scope adoption.
	for token, record := range r.Symbols {
		if len(token) != len(linuxProbeSymbolPrefix)+linuxProbeSymbolDigestLength || !linuxProbeSymbolPattern.MatchString(token) {
			return fmt.Errorf("invalid checkpoint symbol token")
		}
		if err := registry.publish(token, unpackKbuildCheckpointSymbol(record)); err != nil {
			return err
		}
	}
	for _, token := range slices.Sorted(maps.Keys(r.Symbols)) {
		symbol := registry.symbols[token]
		if err := staged.validateCheckpointSymbol(token, symbol, normalizeUpstream); err != nil {
			return fmt.Errorf("compiler checkpoint symbol %s: %w", token, err)
		}
	}
	for name, record := range r.Scopes {
		e := staged.evaluators[name]
		e.references, e.seen = nil, map[string]bool{}
		for _, ref := range record.References {
			if refs[ref.NodeID] != ref || e.seen[ref.NodeID] {
				return fmt.Errorf("compiler checkpoint has invalid scope references")
			}
			e.references = append(e.references, ref)
			e.seen[ref.NodeID] = true
		}
	}
	plan, err := builder.Plan(staged.References()...)
	if err != nil || !reflect.DeepEqual(plan, r.Plan) {
		return fmt.Errorf("compiler checkpoint lost ordinary probe closure: %v", err)
	}
	identity, err := staged.exactScriptEnvironmentsKey(staged.currentScriptEnvironments())
	if err != nil {
		return err
	}
	staged.activeScriptEnvironmentIdentity = identity
	s.evaluators = staged.evaluators
	s.activeScriptEnvironmentIdentity = identity
	s.activeExactScriptEnvironment = ""
	return nil
}

func (s *KbuildProbeScopes) validateCheckpointSymbol(token string, symbol linuxProbeSymbol, normalizeUpstream func(string) (string, error)) error {
	e := s.evaluators["target"]
	canonical := linuxProbeSymbol{kind: symbol.kind}
	switch symbol.kind {
	case "text", "boolean":
		canonical.reference, canonical.request, canonical.dependencies = symbol.reference, symbol.request, symbol.dependencies
		if symbol.kind == "boolean" {
			canonical.trueText, canonical.falseText = symbol.trueText, symbol.falseText
		}
	case "selection":
		canonical.selectionInputs, canonical.selectionValues = symbol.selectionInputs, symbol.selectionValues
	case "transformed-text":
		canonical.textTransform = symbol.textTransform
	case "make-text":
		canonical.makeText = symbol.makeText
	case "source-shell-words":
		canonical.sourceShellWords = symbol.sourceShellWords
	case "toolset-path-literal":
		canonical.toolsetPathLiteral, canonical.toolsetPathScopes = symbol.toolsetPathLiteral, symbol.toolsetPathScopes
	}
	if !reflect.DeepEqual(canonical, symbol) {
		return fmt.Errorf("symbol mixes incompatible payload kinds")
	}
	var got string
	var err error
	switch symbol.kind {
	case "text", "boolean":
		definition, ok := e.symbolRegistry.lookupDefinition(symbol.reference.NodeID)
		if !ok || definition.reference != symbol.reference || !reflect.DeepEqual(definition.request, symbol.request) || !slices.Equal(definition.dependencies, symbol.dependencies) {
			return fmt.Errorf("symbol has no exact original request definition")
		}
		if symbol.kind == "text" {
			got, err = e.requestTextInScope(symbol.reference.Scope, symbol.request, symbol.dependencies...)
		} else {
			got, err = e.renderTruth(linuxProbeTruth{reference: symbol.reference, request: symbol.request, dependencies: symbol.dependencies, trueWhenResult: true}, symbol.trueText, symbol.falseText)
		}
	case "selection":
		for _, input := range symbol.selectionInputs {
			d, ok := e.symbolRegistry.lookupDefinition(input.reference.NodeID)
			if !ok || d.reference != input.reference || !reflect.DeepEqual(d.request, input.request) || !slices.Equal(d.dependencies, input.dependencies) {
				return fmt.Errorf("selection lost original probe input")
			}
		}
		// The selection identity includes its original scope. Trying another
		// scope must not publish a new alias in the restored namespace.
		for _, scope := range []string{"target", "host"} {
			if s.evaluators[scope] == nil {
				continue
			}
			validator := &LinuxProbeEvaluator{scope: scope, symbols: map[string]linuxProbeSymbol{}, symbolRegistry: newLinuxProbeSymbolRegistry()}
			got, err = validator.renderSelection(symbol.selectionInputs, symbol.selectionValues)
			if err != nil || got == token {
				break
			}
		}
	case "transformed-text":
		if symbol.textTransform == nil {
			return fmt.Errorf("missing text transform")
		}
		v := symbol.textTransform
		got, err = e.renderTextTransform(v.sourceToken, v.function, v.arguments, v.inputArgument)
	case "make-text":
		if symbol.makeText == nil {
			return fmt.Errorf("missing Make expression")
		}
		v := symbol.makeText
		got, err = e.renderMakeTextWithProtocolTransforms(v.function, v.arguments, v.protocolValue, v.protocolTransforms, v.protocolMode)
	case "source-shell-words":
		got, err = e.renderSourceShellWords(symbol.sourceShellWords)
	case "toolset-path-literal":
		if normalizeUpstream == nil {
			return fmt.Errorf("toolset literal requires current upstream authorization")
		}
		got, err = s.ImportToolsetPathCapabilities(symbol.toolsetPathLiteral, normalizeUpstream)
	default:
		return fmt.Errorf("unknown checkpoint symbol kind %q", symbol.kind)
	}
	if err != nil {
		return err
	}
	if got != token || !equalLinuxProbeSymbol(e.symbolRegistry.symbols[got], symbol) {
		return fmt.Errorf("checkpoint symbol identity or contents changed")
	}
	return nil
}
