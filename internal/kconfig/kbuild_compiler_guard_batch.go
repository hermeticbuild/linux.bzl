package kconfig

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
)

// KbuildCompilerGuardBatch owns one frozen round of supplemental initial-state
// queries. It never registers requests with, or changes the oracle of, the
// ordinary Kbuild workload. A nil oracle discovers requests; a nonnil oracle
// must contain every demanded result, including the exact dependency closure.
// Call Plan before publishing or consuming the completed round. The caller
// separately authenticates reached source/header provenance and restarts each
// translation unit from its initial namespace when applying these answers.
type KbuildCompilerGuardBatch struct {
	scopes       *KbuildProbeScopes
	ordinary     map[string]*LinuxProbeEvaluator
	builder      *ProbePlanBuilder
	oracle       *ProbeResultOracle
	registry     *linuxProbeSymbolRegistry
	imported     map[string]ProbeReference
	terminals    []ProbeReference
	terminalIDs  map[string]bool
	observations map[configDependencyCompilerPredefineRequestKey]map[string]kbuildCompilerGuardAnswerObservation
	intrinsics   map[configDependencyCompilerPredefineRequestKey]map[CompilerIntrinsicCall]map[string]kbuildCompilerIntrinsicObservation
	counters     map[configDependencyCompilerPredefineRequestKey]map[string]kbuildCompilerCounterObservation
	frozen       bool
	err          error
}

// NewKbuildCompilerGuardBatch snapshots the configured scope bindings while
// retaining read-only access to the owning workload's symbolic definitions.
// The existing compiler projection and configured-contract checks still apply;
// this API does not make a different original driver-link contract equivalent.
func NewKbuildCompilerGuardBatch(scopes *KbuildProbeScopes, oracle *ProbeResultOracle) (*KbuildCompilerGuardBatch, error) {
	if scopes == nil || scopes.evaluators["target"] == nil || scopes.evaluators["target"].facts == nil {
		return nil, fmt.Errorf("compiler guard batch requires a configured target scope")
	}
	targetIdentity := scopes.evaluators["target"].facts.ToolsetIdentity()
	hostIdentity := ""
	if host := scopes.evaluators["host"]; host != nil {
		if host.facts == nil {
			return nil, fmt.Errorf("compiler guard batch has no host compiler facts")
		}
		hostIdentity = host.facts.ToolsetIdentity()
	}
	builder, err := NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		return nil, err
	}
	batch := &KbuildCompilerGuardBatch{
		scopes:   &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{}},
		ordinary: map[string]*LinuxProbeEvaluator{},
		builder:  builder, oracle: oracle, imported: map[string]ProbeReference{}, terminalIDs: map[string]bool{},
	}
	for _, scope := range []string{"target", "host"} {
		original := scopes.evaluators[scope]
		if original == nil {
			continue
		}
		if original.symbolRegistry == nil || batch.registry != nil && batch.registry != original.symbolRegistry {
			return nil, fmt.Errorf("compiler guard batch requires one authenticated symbol registry")
		}
		batch.registry = original.symbolRegistry
		if oracle != nil && oracle.toolsetIdentity(scope) != original.facts.ToolsetIdentity() {
			return nil, fmt.Errorf("compiler guard batch %s oracle has a different toolset", scope)
		}
		var lookup ProbeResultLookup
		if oracle != nil {
			lookup = oracle
		}
		evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
			Scope: scope, Architecture: original.architecture,
			SourceRoot: original.sourceRoot, SourceArchitecture: original.sourceArchitecture,
			ScriptEnvironment: maps.Clone(original.scriptEnvironment),
			Facts:             original.facts, Tools: maps.Clone(original.tools),
			Discovery: builder, Oracle: lookup, RustSourceRoot: original.rustSourceRoot,
		})
		if err != nil {
			return nil, fmt.Errorf("create detached %s compiler guard evaluator: %w", scope, err)
		}
		evaluator.symbolRegistry = batch.registry
		batch.scopes.evaluators[scope] = evaluator
		batch.ordinary[scope] = original
	}
	return batch, nil
}

// CompilerDefinedness has the same byte-level request contract as the ordinary
// API. Names are canonicalized as a set; argv and dependency input order are
// preserved. Discovery returns no answers. Missing or malformed replay results
// are errors, never a new discovery round or a measured absence.
func (b *KbuildCompilerGuardBatch) CompilerDefinedness(
	scope, role, language string,
	arguments, translationUnits, names []string,
	environment map[string]string,
) (map[string]bool, bool, error) {
	if b == nil || b.builder == nil {
		return nil, false, fmt.Errorf("compiler guard batch is nil")
	}
	if b.err != nil {
		return nil, false, b.err
	}
	if b.frozen {
		return nil, false, fmt.Errorf("compiler guard batch is frozen")
	}
	ordered, stdin, err := compilerDefinednessSource(names)
	if err != nil {
		return nil, false, unsupportedCompilerPredefineProjection(err)
	}
	probe, err := b.scopes.compilerProjectedRequest(scope, role, language, arguments, translationUnits, environment,
		"compiler-definedness", []string{"-E", "-P", "-x", language, "-"}, stdin,
		ProbeOutcome{Kind: "text", Step: "compiler-definedness", Stream: "stdout", RequireSuccess: true})
	if err != nil {
		return nil, false, err
	}
	reference, err := b.registerCompilerGuardProbe(scope, probe)
	if err != nil {
		return nil, false, err
	}
	if b.oracle == nil {
		return nil, false, nil
	}
	contents, err := probe.evaluator.readText(reference, probe.request, probe.dependencies...)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	values, err := parseCompilerDefinednessResult(ordered, contents)
	if err != nil {
		b.err = err
		return nil, false, err
	}
	if err := b.recordAnswers(scope, role, language, arguments, translationUnits, environment, reference, values); err != nil {
		b.err = err
		return nil, false, err
	}
	return values, true, nil
}

// OptionalCompilerDefinednessState separates discovery from a completed attempt
// that grants no facts. Callers must not treat Unqueryable as measured absence.
type OptionalCompilerDefinednessState uint8

const (
	OptionalCompilerDefinednessPending OptionalCompilerDefinednessState = iota
	OptionalCompilerDefinednessUnqueryable
	OptionalCompilerDefinednessAnswered
)

// OptionalCompilerDefinedness attempts the same initial-state query as
// CompilerDefinedness, with a distinct Boolean outcome recording process
// success. Mandatory queries and their request bytes remain unchanged.
//
// Only a normal nonzero exit yields Unqueryable, with no values. Signals,
// missing/malformed results, unavailable dependencies and malformed successful
// stdout remain errors. The caller must retain rejected attempts by their exact
// canonical vector and authenticated compiler context, never by individual
// names. This method neither suppresses future queries nor records failed facts.
func (b *KbuildCompilerGuardBatch) OptionalCompilerDefinedness(
	scope, role, language string,
	arguments, translationUnits, names []string,
	environment map[string]string,
) (values map[string]bool, state OptionalCompilerDefinednessState, err error) {
	values, state, _, err = b.OptionalCompilerDefinednessAttempt(scope, role, language, arguments, translationUnits, names, environment)
	return values, state, err
}

// OptionalCompilerDefinednessAttempt also returns the exact registered terminal,
// even during discovery or after deduplication. A reference identifies an attempt,
// not an answer: Pending and Unqueryable still grant no initial-state facts.
// Every error returns a zero reference. Current imported dependencies and replay
// results are authenticated by the same path as OptionalCompilerDefinedness.
func (b *KbuildCompilerGuardBatch) OptionalCompilerDefinednessAttempt(
	scope, role, language string,
	arguments, translationUnits, names []string,
	environment map[string]string,
) (values map[string]bool, state OptionalCompilerDefinednessState, reference ProbeReference, err error) {
	if b == nil || b.builder == nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, fmt.Errorf("compiler guard batch is nil")
	}
	if b.err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, b.err
	}
	if b.frozen {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, fmt.Errorf("compiler guard batch is frozen")
	}
	ordered, stdin, err := compilerDefinednessSource(names)
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, unsupportedCompilerPredefineProjection(err)
	}
	const stepName = "compiler-definedness"
	probe, err := b.scopes.compilerProjectedRequest(scope, role, language, arguments, translationUnits, environment,
		stepName, []string{"-E", "-P", "-x", language, "-"}, stdin,
		ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: stepName}})
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	reference, err = b.registerCompilerGuardProbe(scope, probe)
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	if b.oracle == nil {
		return nil, OptionalCompilerDefinednessPending, reference, nil
	}
	// A failed replay may never publish a partial answer snapshot or plan.
	defer func() {
		if err != nil {
			b.err = err
		}
	}()
	positive, err := probe.evaluator.readBoolean(reference, probe.request, probe.dependencies...)
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	result, err := probe.evaluator.readProbeResult(reference, probe.request, probe.dependencies...)
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	if result.NodeID != reference.NodeID {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, fmt.Errorf("optional compiler definedness has stale node identity")
	}
	step, present := probeResultStep(result.Steps, stepName)
	if !present || step.Status == "skipped" || step.ExitCode < 0 || step.ExitCode > 255 {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, fmt.Errorf("optional compiler definedness has no normal process completion")
	}
	// readBoolean checks the envelope and exact step plan, but process-based
	// predicates need this independent reduction check before granting facts.
	success := step.Status == "success" && step.ExitCode == 0
	if positive != success {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, fmt.Errorf("optional compiler definedness disagrees with its exact process outcome")
	}
	if !success {
		return nil, OptionalCompilerDefinednessUnqueryable, reference, nil
	}
	values, err = parseCompilerDefinednessResult(ordered, step.Stdout)
	if err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	if err := b.recordAnswers(scope, role, language, arguments, translationUnits, environment, reference, values); err != nil {
		return nil, OptionalCompilerDefinednessPending, ProbeReference{}, err
	}
	return values, OptionalCompilerDefinednessAnswered, reference, nil
}

// Both detached query kinds use the same exact imported-dependency/current-
// ordinary validation and terminal registration. Neither changes ordinary
// Kbuild discovery, registry definitions, or its result oracle.
func (b *KbuildCompilerGuardBatch) registerCompilerGuardProbe(scope string, probe *compilerProjectedProbe) (ProbeReference, error) {
	for _, dependency := range probe.dependencies {
		if err := b.importDependency(dependency, map[string]bool{}, 0); err != nil {
			b.err = err
			return ProbeReference{}, err
		}
	}
	reference, err := b.builder.Request(scope, probe.request, probe.dependencies...)
	if err != nil {
		b.err = err
		return ProbeReference{}, err
	}
	if !b.terminalIDs[reference.NodeID] {
		b.terminalIDs[reference.NodeID] = true
		b.terminals = append(b.terminals, reference)
	}
	return reference, nil
}

// CompilerGuardDependencyLimitError identifies only the bounded traversal
// resource ceiling. Fresh optional discovery may discard its whole staged
// frontier on this error. Mandatory queries and every prior-round replay must
// still fail; this type never makes missing, stale or cyclic inputs optional.
type CompilerGuardDependencyLimitError struct{}

func (*CompilerGuardDependencyLimitError) Error() string {
	return "compiler guard dependency graph exceeds its bounded budget"
}

func (b *KbuildCompilerGuardBatch) importDependency(reference ProbeReference, visiting map[string]bool, depth int) error {
	if previous, found := b.imported[reference.NodeID]; found {
		if previous != reference {
			return fmt.Errorf("compiler guard dependency has contradictory reference metadata")
		}
		return nil
	}
	if visiting[reference.NodeID] {
		return fmt.Errorf("compiler guard dependency graph is cyclic")
	}
	definition, found := b.registry.lookupDefinition(reference.NodeID)
	if !found || definition.reference != reference {
		return fmt.Errorf("compiler guard dependency lacks its exact registered definition")
	}
	// Check the exact registered reference before a full budget can obscure a
	// malformed or missing current definition as an optional omission.
	if depth >= maxLinuxProbeResolveDepth || len(b.imported) >= maxLinuxProbeResolveNodes {
		return &CompilerGuardDependencyLimitError{}
	}
	visiting[reference.NodeID] = true
	defer delete(visiting, reference.NodeID)
	for _, dependency := range definition.dependencies {
		if err := b.importDependency(dependency, visiting, depth+1); err != nil {
			return err
		}
	}
	actual, err := b.builder.Request(reference.Scope, cloneLinuxProbeRequest(definition.request), definition.dependencies...)
	if err != nil {
		return err
	}
	if actual != reference {
		return fmt.Errorf("compiler guard dependency changed its canonical request or input order")
	}
	if b.oracle != nil {
		actual, err := compilerGuardDependencyResult(b.scopes.evaluators[reference.Scope], definition)
		if err != nil {
			return err
		}
		current, err := compilerGuardDependencyResult(b.ordinary[reference.Scope], definition)
		if err != nil {
			return fmt.Errorf("current ordinary compiler context dependency: %w", err)
		}
		// Request/node IDs commit to source paths, not their execution-time
		// contents. The same symbolic atom can therefore render different
		// compiler flags in a later source snapshot. Match the current measured
		// dependency values as well as the immutable request graph before any
		// supplemental answer can become initial-namespace authority.
		if !bytes.Equal(actual, current) {
			return fmt.Errorf("supplemental compiler guard dependency differs from the current ordinary compiler context")
		}
	}
	b.imported[reference.NodeID] = reference
	return nil
}

func compilerGuardDependencyResult(evaluator *LinuxProbeEvaluator, definition linuxProbeRequestDefinition) ([]byte, error) {
	if evaluator == nil || evaluator.oracle == nil {
		return nil, fmt.Errorf("compiler guard dependency has no current result oracle")
	}
	// Keep missing results strict even for a pure reduction that the general
	// evaluator could otherwise derive from its dependencies.
	result, err := evaluator.oracle.Result(definition.reference)
	if err != nil {
		return nil, err
	}
	switch definition.reference.Kind {
	case "text":
		_, err = evaluator.readText(definition.reference, definition.request, definition.dependencies...)
	case "boolean":
		_, err = evaluator.readBoolean(definition.reference, definition.request, definition.dependencies...)
	default:
		err = fmt.Errorf("compiler guard dependency has an unsupported result kind")
	}
	if err != nil {
		return nil, err
	}
	return result.CanonicalJSON()
}

// Plan seals this round. Replay requires the independent oracle to validate
// the complete imported plan. Returned requests and dependency lists are owned
// by the caller; mutating them cannot change this batch or its original scopes.
func (b *KbuildCompilerGuardBatch) Plan() (*ProbePlan, error) {
	if b == nil || b.builder == nil {
		return nil, fmt.Errorf("compiler guard batch is nil")
	}
	b.frozen = true
	if b.err != nil {
		return nil, b.err
	}
	plan, err := b.builder.Plan(b.terminals...)
	if err != nil {
		b.err = err
		return nil, err
	}
	if b.oracle != nil {
		if err := b.oracle.ValidatePlan(plan); err != nil {
			b.err = err
			return nil, err
		}
	}
	for id, request := range plan.Requests {
		plan.Requests[id] = cloneLinuxProbeRequest(request)
	}
	for index := range plan.Nodes {
		plan.Nodes[index].Inputs = slices.Clone(plan.Nodes[index].Inputs)
	}
	return plan, nil
}
