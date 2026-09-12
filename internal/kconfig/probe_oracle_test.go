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
