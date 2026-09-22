package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbePlanBuilderPreservesOrderedDependencies(t *testing.T) {
	identity := "sha256-" + strings.Repeat("a", 64)
	builder, err := NewProbePlanBuilder(identity, "")
	if err != nil {
		t.Fatal(err)
	}
	request := testProbeRequest()
	first, err := builder.Request("target", request)
	if err != nil {
		t.Fatal(err)
	}
	dependent := request
	dependent.InputCount = 1
	second, err := builder.Request("target", dependent, first)
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeID == second.NodeID {
		t.Fatal("dependency edge did not affect node identity")
	}
	plan, err := builder.Plan(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || len(plan.Nodes[1].Inputs) != 1 || plan.Nodes[1].Inputs[0] != first.NodeID {
		t.Fatalf("plan nodes = %#v", plan.Nodes)
	}
}

func TestProbePlanBuilderCanonicalizesRepeatedTerminals(t *testing.T) {
	identity := "sha256-" + strings.Repeat("a", 64)
	builder, err := NewProbePlanBuilder(identity, "")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := builder.Request("target", testProbeRequest())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := builder.Plan(reference, reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Terminal) != 1 || plan.Terminal[0] != reference.NodeID {
		t.Fatalf("terminal roots = %q, want [%s]", plan.Terminal, reference.NodeID)
	}
}

func TestProbePlanBuilderDeduplicatesAtScaleInFirstSeenOrder(t *testing.T) {
	const requestCount = 2048
	identity := "sha256-" + strings.Repeat("a", 64)
	builder, err := NewProbePlanBuilder(identity, "")
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]ProbeRequest, requestCount)
	references := make([]ProbeReference, requestCount)
	for index := range requests {
		request := testProbeRequest()
		request.Steps[0].Stdin = fmt.Sprintf("int value_%08d;\n", index)
		requests[index] = request
		references[index], err = builder.Request("target", request)
		if err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
	}
	for index := requestCount - 1; index >= 0; index-- {
		duplicate, err := builder.Request("target", requests[index])
		if err != nil {
			t.Fatalf("duplicate request %d: %v", index, err)
		}
		if duplicate != references[index] {
			t.Fatalf("duplicate request %d = %#v, want %#v", index, duplicate, references[index])
		}
	}
	if len(builder.plan.Nodes) != requestCount {
		t.Fatalf("deduplicated plan has %d nodes, want %d", len(builder.plan.Nodes), requestCount)
	}
	for index, node := range builder.plan.Nodes {
		if node.ID != references[index].NodeID {
			t.Fatalf("plan node %d = %s, want first-seen %s", index, node.ID, references[index].NodeID)
		}
	}
}

func BenchmarkProbePlanBuilderRequestDedupScale(b *testing.B) {
	identity := "sha256-" + strings.Repeat("a", 64)
	for _, requestCount := range []int{128, 1024, 4096} {
		requests := make([]ProbeRequest, requestCount)
		for index := range requests {
			request := testProbeRequest()
			request.Steps[0].Stdin = fmt.Sprintf("int value_%08d;\n", index)
			requests[index] = request
		}
		b.Run(fmt.Sprintf("requests-%d", requestCount), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(requestCount), "unique-requests/op")
			for range b.N {
				builder, err := NewProbePlanBuilder(identity, "")
				if err != nil {
					b.Fatal(err)
				}
				for _, request := range requests {
					if _, err := builder.Request("target", request); err != nil {
						b.Fatal(err)
					}
				}
				for index := len(requests) - 1; index >= 0; index-- {
					if _, err := builder.Request("target", requests[index]); err != nil {
						b.Fatal(err)
					}
				}
				if len(builder.plan.Nodes) != requestCount {
					b.Fatalf("deduplicated plan has %d nodes, want %d", len(builder.plan.Nodes), requestCount)
				}
			}
		})
	}
}

func TestProbeResultOracleFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("b", 64)
	requestID := strings.Repeat("c", 64)
	nodeID := strings.Repeat("d", 64)
	value := true
	result := ProbeResult{Schema: LinuxProbeResultSchema, NodeID: nodeID, RequestID: requestID, Scope: "target", ToolsetIdentity: identity, Kind: "boolean", Boolean: &value}
	data, err := result.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "results", nodeID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"target": root},
		map[string]string{"target": identity},
	)
	if err != nil {
		t.Fatal(err)
	}
	reference := ProbeReference{NodeID: nodeID, RequestID: requestID, Scope: "target", Kind: "boolean"}
	if got, err := oracle.Boolean(reference); err != nil || !got {
		t.Fatalf("Boolean() = %v, %v", got, err)
	}
	reference.RequestID = strings.Repeat("e", 64)
	if _, err := oracle.Boolean(reference); err == nil || !strings.Contains(err.Error(), "result request") {
		t.Fatalf("stale result error = %v", err)
	}
	missing := reference
	missing.NodeID = strings.Repeat("f", 64)
	if _, err := oracle.Boolean(missing); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("missing result error = %v", err)
	}
}

func TestProbeResultOracleAllowsUnusedDiscoverySuperset(t *testing.T) {
	identity := "sha256-" + strings.Repeat("a", 64)
	builder, err := NewProbePlanBuilder(identity, "")
	if err != nil {
		t.Fatal(err)
	}
	request := testProbeRequest()
	reference, err := builder.Request("target", request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := builder.Plan(reference)
	if err != nil {
		t.Fatal(err)
	}
	value := true
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
		Scope: "target", ToolsetIdentity: identity, Kind: reference.Kind, Boolean: &value,
	}
	oracle := &ProbeResultOracle{
		results: map[string]ProbeResult{
			reference.NodeID: result,
			strings.Repeat("f", 64): {
				Schema: LinuxProbeResultSchema, NodeID: strings.Repeat("f", 64),
				RequestID: strings.Repeat("e", 64), Scope: "target",
				ToolsetIdentity: identity, Kind: "boolean", Boolean: &value,
			},
		},
		toolsets: map[string]string{"target": identity},
	}
	if err := oracle.ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan(discovery superset) failed: %v", err)
	}
	delete(oracle.results, reference.NodeID)
	if err := oracle.ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("ValidatePlan(missing demanded result) error = %v", err)
	}
}

func TestProbeResultOracleRejectsPreviousHostToolsetResult(t *testing.T) {
	targetIdentity := "sha256-" + strings.Repeat("a", 64)
	hostA := "sha256-" + strings.Repeat("b", 64)
	hostB := "sha256-" + strings.Repeat("c", 64)
	request := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "version", Tool: "host@cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "version", Stream: "stdout"},
	}
	build := func(hostIdentity string) (*ProbePlan, ProbeReference) {
		t.Helper()
		builder, err := NewProbePlanBuilder(targetIdentity, hostIdentity)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := builder.Request("target", request)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := builder.Plan(reference)
		if err != nil {
			t.Fatal(err)
		}
		return plan, reference
	}
	planA, referenceA := build(hostA)
	planB, referenceB := build(hostB)
	if referenceA.RequestID == referenceB.RequestID || referenceA.NodeID == referenceB.NodeID {
		t.Fatalf("host compiler change reused request/node identity: A=%#v B=%#v", referenceA, referenceB)
	}
	resultA := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: referenceA.NodeID, RequestID: referenceA.RequestID,
		Scope: "target", ToolsetIdentity: targetIdentity, Kind: "text", Text: "compiler version A\n",
		Steps: []ProbeStepResult{{Name: "version", Status: "success", Stdout: "compiler version A\n"}},
	}
	if err := resultA.Validate(); err != nil {
		t.Fatal(err)
	}
	oracle := &ProbeResultOracle{
		results:  map[string]ProbeResult{referenceA.NodeID: resultA},
		toolsets: map[string]string{"target": targetIdentity, "host": hostA},
	}
	if err := oracle.ValidatePlan(planA); err != nil {
		t.Fatalf("original host compiler result rejected: %v", err)
	}
	oracle.toolsets["host"] = hostB
	if err := oracle.ValidatePlan(planB); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("previous host compiler result satisfied replacement plan: %v", err)
	}
	request.HostToolsetIdentity = hostA
	builderB, err := NewProbePlanBuilder(targetIdentity, hostB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builderB.Request("target", request); err == nil || !strings.Contains(err.Error(), "host toolset identity") {
		t.Fatalf("builder accepted explicit stale host toolset identity: %v", err)
	}
}

func TestProbeResultOracleRejectsCaptureModeDisagreement(t *testing.T) {
	identity := "sha256-" + strings.Repeat("a", 64)
	builder, err := NewProbePlanBuilder(identity, "")
	if err != nil {
		t.Fatal(err)
	}
	merged := "warning\nclang version 22\n"
	results := map[string]ProbeResult{}
	references := []ProbeReference{}
	for _, capture := range []bool{false, true} {
		stream := "stdout"
		step := ProbeStepResult{Name: "version", Status: "success", Stdout: "clang version 22\n"}
		text := "clang version 22"
		if capture {
			stream = "combined"
			step.Stdout = ""
			step.Combined = &merged
			text = "warning"
		}
		request := ProbeRequest{
			Schema:  LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: "version", Tool: "cc", Arguments: []string{"--version"}, CaptureCombined: capture}},
			Outcome: ProbeOutcome{Kind: "text", Step: "version", Stream: stream, FirstLine: true},
		}
		reference, err := builder.Request("target", request)
		if err != nil {
			t.Fatal(err)
		}
		references = append(references, reference)
		results[reference.NodeID] = ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
			Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: text,
			Steps: []ProbeStepResult{step},
		}
	}
	plan, err := builder.Plan(references...)
	if err != nil {
		t.Fatal(err)
	}
	oracle := &ProbeResultOracle{results: results, toolsets: map[string]string{"target": identity}}
	if err := oracle.ValidatePlan(plan); err != nil {
		t.Fatalf("matching capture modes rejected: %v", err)
	}

	originalSplit := results[references[0].NodeID]
	split := originalSplit
	split.Steps = append([]ProbeStepResult(nil), split.Steps...)
	split.Steps[0].Stdout = ""
	split.Steps[0].Combined = &merged
	results[references[0].NodeID] = split
	if err := split.Validate(); err != nil {
		t.Fatalf("canonical merged result rejected without request context: %v", err)
	}
	if err := oracle.ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "capture mode disagrees") {
		t.Fatalf("split request accepted merged result: %v", err)
	}
	results[references[0].NodeID] = originalSplit
	wrongSplit := results[references[1].NodeID]
	wrongSplit.Steps = append([]ProbeStepResult(nil), wrongSplit.Steps...)
	wrongSplit.Steps[0].Combined = nil
	wrongSplit.Steps[0].Stdout = "clang version 22\n"
	results[references[1].NodeID] = wrongSplit
	if err := wrongSplit.Validate(); err != nil {
		t.Fatalf("canonical split result rejected without request context: %v", err)
	}
	if err := oracle.ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "capture mode disagrees") {
		t.Fatalf("merged request accepted split result: %v", err)
	}
}

func TestReadProbeResultTreeAcceptsOnlyExplicitEmptyMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	results, err := ReadProbeResultTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("empty result tree = %#v", results)
	}
	if err := os.WriteFile(filepath.Join(root, ".empty"), []byte("unexpected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProbeResultTree(root); err == nil || !strings.Contains(err.Error(), "invalid empty marker") {
		t.Fatalf("nonempty marker error = %v", err)
	}
}
