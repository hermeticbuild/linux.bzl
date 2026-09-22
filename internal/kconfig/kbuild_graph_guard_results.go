package kconfig

import (
	"fmt"
	"maps"
	"slices"
)

// KbuildGraphGuardResults grants a bounded, read-only subset of measured probe
// results to source graph selection during ordinary probe discovery. Its
// terminals came from a source-derived pregraph plan. Their exact measured
// dependency closure can select later source guards; every other compiler
// probe remains symbolic until the ordinary Kbuild probe batch executes.
type KbuildGraphGuardResults struct {
	plan     *ProbePlan
	oracle   *ProbeResultOracle
	selected map[string]ProbeReference
}

func NewKbuildGraphGuardResults(plan *ProbePlan, oracle *ProbeResultOracle) (*KbuildGraphGuardResults, error) {
	if plan == nil || oracle == nil {
		return nil, fmt.Errorf("graph guard probe plan and results are required")
	}
	if err := oracle.ValidatePlan(plan); err != nil {
		return nil, fmt.Errorf("validate declared graph guard producer closure: %w", err)
	}
	nodes := make(map[string]ProbePlanNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.ID] = node
	}
	// A compiler query selected as a terminal may depend on earlier compiler
	// queries which change its own argv. The prior round has measured those
	// dependencies, so they can select their source branches before the next
	// round constructs its terminal request. Derive authority from the actual
	// terminal closure; an unrelated result present in the oracle, or even a
	// detached node in an imported plan, grants no source-selection authority.
	selected := make(map[string]ProbeReference, len(nodes))
	work := slices.Clone(plan.Terminal)
	for len(work) != 0 {
		id := work[len(work)-1]
		work = work[:len(work)-1]
		if _, seen := selected[id]; seen {
			continue
		}
		node := nodes[id]
		request := plan.Requests[node.RequestID]
		selected[id] = ProbeReference{
			NodeID: id, RequestID: node.RequestID, Scope: node.Scope, Kind: request.Outcome.Kind,
		}
		work = append(work, node.Inputs...)
	}
	return &KbuildGraphGuardResults{plan: plan, oracle: oracle, selected: selected}, nil
}

func (g *KbuildGraphGuardResults) binds(references []ProbeReference) bool {
	if g == nil || len(references) == 0 {
		return false
	}
	for _, reference := range references {
		if g.selected[reference.NodeID] != reference {
			return false
		}
	}
	return true
}

func (g *KbuildGraphGuardResults) validateScopes(scopes *KbuildProbeScopes) error {
	if g == nil || scopes == nil {
		return fmt.Errorf("Kbuild graph guard probe source is unavailable")
	}
	toolsets := map[string]string{}
	for _, scope := range []string{"target", "host"} {
		if evaluator := scopes.evaluators[scope]; evaluator != nil {
			toolsets[scope] = evaluator.facts.ToolsetIdentity()
		}
	}
	if !maps.Equal(g.plan.Toolsets, toolsets) {
		return fmt.Errorf("pregraph probe plan changed configured target/host toolset identities")
	}
	return nil
}

// resolve constructs a separate replay-only evaluator. It shares authenticated
// source symbol definitions but cannot turn ordinary discovery into replay or
// consume an undeclared compiler result from the pregraph result tree.
func (g *KbuildGraphGuardResults) resolve(evaluator *LinuxProbeEvaluator, expression string) (string, error) {
	if g == nil || evaluator == nil {
		return "", fmt.Errorf("Kbuild graph guard source is unavailable")
	}
	replay, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: evaluator.scope, Architecture: evaluator.architecture,
		SourceArchitecture: evaluator.sourceArchitecture,
		SourceRoot:         evaluator.sourceRoot, SourceRootAliases: slices.Clone(evaluator.sourceRootAliases),
		ScriptEnvironment: maps.Clone(evaluator.scriptEnvironment),
		Facts:             evaluator.facts, Tools: maps.Clone(evaluator.tools),
		Discovery: evaluator.discovery, Oracle: g.oracle,
		RustSourceRoot:    evaluator.rustSourceRoot,
		PkgConfigManifest: evaluator.pkgConfigManifest,
	})
	if err != nil {
		return "", err
	}
	replay.symbolRegistry = evaluator.symbolRegistry
	return replay.ResolveSymbolic(expression)
}
