package kconfig

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
)

// ProbeReference is the symbolic value retained by Kconfig/Kbuild discovery.
// A reference does not guess a capability: its value exists only after the
// corresponding map_directory action has produced and the final planner has
// loaded its ProbeResult.
type ProbeReference struct {
	NodeID    string
	RequestID string
	Scope     string
	Kind      string
}

// ProbeDiscovery is the monadic boundary used by symbolic Kconfig/Kbuild
// evaluation. Dependencies are explicit result references, so a later request
// may retain the precise compiler context selected by earlier probes.
type ProbeDiscovery interface {
	Request(scope string, request ProbeRequest, dependencies ...ProbeReference) (ProbeReference, error)
}

// ProbePlanBuilder content-addresses requests and their dependency DAG. It is
// deliberately unaware of compiler families and Kconfig symbols.
type ProbePlanBuilder struct {
	plan ProbePlan
}

func NewProbePlanBuilder(targetIdentity, hostIdentity string) (*ProbePlanBuilder, error) {
	toolsets := map[string]string{"target": targetIdentity}
	if hostIdentity != "" {
		toolsets["host"] = hostIdentity
	}
	for scope, identity := range toolsets {
		if err := validateProbeIdentity(identity); err != nil {
			return nil, fmt.Errorf("%s probe toolset: %w", scope, err)
		}
	}
	return &ProbePlanBuilder{plan: ProbePlan{Toolsets: toolsets, Requests: map[string]ProbeRequest{}}}, nil
}

func (b *ProbePlanBuilder) Request(scope string, request ProbeRequest, dependencies ...ProbeReference) (ProbeReference, error) {
	if b == nil {
		return ProbeReference{}, fmt.Errorf("probe plan builder is nil")
	}
	if (scope != "target" && scope != "host") || b.plan.Toolsets[scope] == "" {
		return ProbeReference{}, fmt.Errorf("probe request has unavailable scope %q", scope)
	}
	requestID, err := request.ID()
	if err != nil {
		return ProbeReference{}, err
	}
	inputs := make([]string, len(dependencies))
	if len(dependencies) != request.InputCount {
		return ProbeReference{}, fmt.Errorf("probe request requires %d dependencies, got %d", request.InputCount, len(dependencies))
	}
	for index, dependency := range dependencies {
		if dependency.NodeID == "" || (scope == "host" && dependency.Scope != "host") || (dependency.Scope != "host" && dependency.Scope != "target") {
			return ProbeReference{}, fmt.Errorf("probe dependency %d has incompatible scope %q", index, dependency.Scope)
		}
		inputs[index] = dependency.NodeID
	}
	node := ProbePlanNode{Scope: scope, RequestID: requestID, Inputs: inputs}
	node.ID = node.ContentID()
	b.plan.Requests[requestID] = request
	for _, existing := range b.plan.Nodes {
		if existing.ID == node.ID {
			return ProbeReference{NodeID: node.ID, RequestID: requestID, Scope: scope, Kind: request.Outcome.Kind}, nil
		}
	}
	b.plan.Nodes = append(b.plan.Nodes, node)
	return ProbeReference{NodeID: node.ID, RequestID: requestID, Scope: scope, Kind: request.Outcome.Kind}, nil
}

func (b *ProbePlanBuilder) Plan(terminals ...ProbeReference) (*ProbePlan, error) {
	if b == nil {
		return nil, fmt.Errorf("probe plan builder is nil")
	}
	plan := b.plan
	plan.Toolsets = maps.Clone(b.plan.Toolsets)
	plan.Requests = maps.Clone(b.plan.Requests)
	plan.Nodes = append([]ProbePlanNode(nil), b.plan.Nodes...)
	plan.Terminal = make([]string, 0, len(terminals))
	seenTerminal := make(map[string]bool, len(terminals))
	for _, terminal := range terminals {
		// Terminals are graph roots, not ordered requests. Independent source
		// evaluation passes may retain the same content-addressed probe; expose
		// that root once while preserving first-seen order for distinct roots.
		if terminal.NodeID != "" && seenTerminal[terminal.NodeID] {
			continue
		}
		seenTerminal[terminal.NodeID] = true
		plan.Terminal = append(plan.Terminal, terminal.NodeID)
	}
	if _, err := plan.entries(); err != nil {
		return nil, err
	}
	return &plan, nil
}

// ProbeResultOracle is the execution-backed half of ProbeDiscovery. Missing,
// stale, cross-toolset, or wrong-kind results are errors; there is no default
// capability answer and no compiler-family fallback table.
type ProbeResultOracle struct {
	mu       sync.RWMutex
	results  map[string]ProbeResult
	toolsets map[string]string
}

// NewProbeResultOracleFromTrees loads the separate host and target result
// TreeArtifacts produced by their respective map_directory calls.
func NewProbeResultOracleFromTrees(resultRoots, toolsets map[string]string) (*ProbeResultOracle, error) {
	oracle := &ProbeResultOracle{results: map[string]ProbeResult{}, toolsets: maps.Clone(toolsets)}
	for scope, root := range resultRoots {
		if scope != "target" && scope != "host" {
			return nil, fmt.Errorf("probe result tree has invalid scope %q", scope)
		}
		if oracle.toolsets[scope] == "" {
			return nil, fmt.Errorf("probe result tree %s has no expected toolset identity", scope)
		}
		results, err := ReadProbeResultTree(root)
		if err != nil {
			return nil, fmt.Errorf("read %s probe result tree: %w", scope, err)
		}
		for id, result := range results {
			if result.Scope != scope {
				return nil, fmt.Errorf("%s probe result tree contains %s node %s", scope, result.Scope, id)
			}
			if result.ToolsetIdentity != oracle.toolsets[scope] {
				return nil, fmt.Errorf("probe node %s result toolset %s, want %s", id, result.ToolsetIdentity, oracle.toolsets[scope])
			}
			if _, exists := oracle.results[id]; exists {
				return nil, fmt.Errorf("probe result trees repeat node %s", id)
			}
			oracle.results[id] = result
		}
	}
	return oracle, nil
}

// ValidatePlan requires one matching result for every concretely replayed
// node. Symbolic discovery can conservatively execute nodes from conditional
// branches which concrete replay proves unused, so its result tree may be a
// strict superset. Unused results are inert; missing or stale demanded results
// still fail closed.
func (o *ProbeResultOracle) ValidatePlan(plan *ProbePlan) error {
	if o == nil || plan == nil {
		return fmt.Errorf("probe result oracle/plan is nil")
	}
	if _, err := plan.entries(); err != nil {
		return err
	}
	for _, node := range plan.Nodes {
		o.mu.RLock()
		result, ok := o.results[node.ID]
		o.mu.RUnlock()
		if !ok {
			requestData, _ := plan.Requests[node.RequestID].CanonicalJSON()
			return fmt.Errorf(
				"missing result for probe node %s request %s with inputs %v",
				node.ID, strings.TrimSpace(string(requestData)), node.Inputs,
			)
		}
		request := plan.Requests[node.RequestID]
		reference := ProbeReference{NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, Kind: request.Outcome.Kind}
		if _, err := o.Result(reference); err != nil {
			return err
		}
		if result.ToolsetIdentity != plan.Toolsets[node.Scope] {
			return fmt.Errorf("probe node %s result toolset %s differs from plan %s", node.ID, result.ToolsetIdentity, plan.Toolsets[node.Scope])
		}
	}
	return nil
}

func (o *ProbeResultOracle) Result(reference ProbeReference) (ProbeResult, error) {
	if o == nil {
		return ProbeResult{}, fmt.Errorf("probe result oracle is nil")
	}
	o.mu.RLock()
	result, ok := o.results[reference.NodeID]
	o.mu.RUnlock()
	if !ok {
		return ProbeResult{}, fmt.Errorf("missing result for probe node %s", reference.NodeID)
	}
	if result.RequestID != reference.RequestID {
		return ProbeResult{}, fmt.Errorf("probe node %s result request %s, want %s", reference.NodeID, result.RequestID, reference.RequestID)
	}
	if result.Kind != reference.Kind {
		return ProbeResult{}, fmt.Errorf("probe node %s result kind %s, want %s", reference.NodeID, result.Kind, reference.Kind)
	}
	if result.Scope != reference.Scope {
		return ProbeResult{}, fmt.Errorf("probe node %s result scope %s, want %s", reference.NodeID, result.Scope, reference.Scope)
	}
	if want := o.toolsets[reference.Scope]; want == "" || result.ToolsetIdentity != want {
		return ProbeResult{}, fmt.Errorf("probe node %s result toolset %s, want %s %s", reference.NodeID, result.ToolsetIdentity, reference.Scope, want)
	}
	return result, nil
}

func (o *ProbeResultOracle) hasResult(nodeID string) bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	_, ok := o.results[nodeID]
	o.mu.RUnlock()
	return ok
}

func (o *ProbeResultOracle) toolsetIdentity(scope string) string {
	if o == nil {
		return ""
	}
	return o.toolsets[scope]
}

func (o *ProbeResultOracle) recordDerivedResult(result ProbeResult) error {
	if o == nil {
		return fmt.Errorf("probe result oracle is nil")
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("invalid derived probe result %s: %w", result.NodeID, err)
	}
	if want := o.toolsets[result.Scope]; want == "" || result.ToolsetIdentity != want {
		return fmt.Errorf(
			"derived probe node %s result toolset %s, want %s %s",
			result.NodeID,
			result.ToolsetIdentity,
			result.Scope,
			want,
		)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if existing, ok := o.results[result.NodeID]; ok {
		if !reflect.DeepEqual(existing, result) {
			return fmt.Errorf("derived probe node %s disagrees with an existing result", result.NodeID)
		}
		return nil
	}
	o.results[result.NodeID] = result
	return nil
}

func (o *ProbeResultOracle) Boolean(reference ProbeReference) (bool, error) {
	result, err := o.Result(reference)
	if err != nil {
		return false, err
	}
	if result.Boolean == nil {
		return false, fmt.Errorf("probe node %s has no boolean result", reference.NodeID)
	}
	return *result.Boolean, nil
}

func (o *ProbeResultOracle) Text(reference ProbeReference) (string, error) {
	result, err := o.Result(reference)
	if err != nil {
		return "", err
	}
	if result.Kind != "text" {
		return "", fmt.Errorf("probe node %s is not a text result", reference.NodeID)
	}
	return result.Text, nil
}
