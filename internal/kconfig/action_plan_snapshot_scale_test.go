package kconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestActionPlanSnapshotSectionSizes(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	for filename := range snapshot.ConfigFiles {
		snapshot.ConfigFiles[filename] += "# private diagnostic fixture: <>&\n"
		break
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	sizes, err := actionPlanSnapshotSectionSizes(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != len(fields) {
		t.Fatalf("counted %d sections, want %d", len(sizes), len(fields))
	}
	for name, contents := range fields {
		if sizes[name] != int64(len(contents)) {
			t.Errorf("section %s contains %d bytes, want %d", name, sizes[name], len(contents))
		}
	}
	message := actionPlanSnapshotSizeError(snapshot, len(encoded)+1, len(encoded)).Error()
	if !strings.Contains(message, "action plan snapshot contains") || !strings.Contains(message, fmt.Sprintf("recipes=%d", sizes["recipes"])) {
		t.Fatalf("missing size diagnostic: %s", message)
	}
	if strings.Contains(message, "private diagnostic fixture") || strings.Contains(message, "CONFIG_USED") {
		t.Fatalf("size diagnostic leaked snapshot contents: %s", message)
	}
}

func TestActionPlanSnapshotValidationDoesNotGenerateMarkerEntries(t *testing.T) {
	snapshot := familyTestSnapshot(
		t,
		1,
		"",
		familyTestConfig("y", "n"),
		ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
	)

	validationStats := &actionPlanValidationStats{}
	if err := snapshot.validateWithStats(validationStats); err != nil {
		t.Fatal(err)
	}
	if validationStats.markerEntries != 0 {
		t.Fatalf("snapshot structural validation generated %d marker entries, want zero", validationStats.markerEntries)
	}

	emissionStats := &actionPlanValidationStats{}
	entries, err := snapshotActionPlan(snapshot).actionPlanEntries(true, emissionStats)
	if err != nil {
		t.Fatal(err)
	}
	if emissionStats.markerEntries == 0 || emissionStats.markerEntries != len(entries) {
		t.Fatalf("emitting validation counted %d marker entries for %d emitted entries", emissionStats.markerEntries, len(entries))
	}
}

func benchmarkActionPlanSnapshotFixture(b *testing.B, nodeCount int) (*ActionPlan, map[string]ConfigDependencySet) {
	b.Helper()
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "generate",
		Tool:   "actionfile",
		Arguments: []string{
			"-out", "${output:00000000}",
			"-content_base64", "",
		},
		Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		b.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes:  map[string]ActionRecipe{recipeID: recipe},
		Nodes:    make([]ActionPlanNode, 0, nodeCount),
	}
	dependencies := make(map[string]ConfigDependencySet, nodeCount)
	for ordinal := 0; ordinal < nodeCount; ordinal++ {
		node := ActionPlanNode{
			Stage: "target", Kind: "generate", Recipe: recipeID, Tool: "actionfile", Product: "image",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: fmt.Sprintf("generated/%08d.h", ordinal)}},
		}
		node.ID = node.ContentID()
		plan.Nodes = append(plan.Nodes, node)
		dependencies[node.ID] = ConfigDependencySet{}
	}
	return plan, dependencies
}

func BenchmarkActionPlanSnapshotScale(b *testing.B) {
	const nodeCount = 10000
	plan, dependencies := benchmarkActionPlanSnapshotFixture(b, nodeCount)
	config := familyTestConfig("y", "n")
	b.ReportMetric(nodeCount, "nodes/op")
	b.ResetTimer()
	for range b.N {
		snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := marshalCanonicalActionPlanSnapshot(snapshot); err != nil {
			b.Fatal(err)
		}
	}
}
