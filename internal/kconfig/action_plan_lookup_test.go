package kconfig

import (
	"fmt"
	"testing"
)

func TestActionPlanLookupsTrackIncrementalNodesAndDirectAppends(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "generate",
		Tool:   "cc",
		Arguments: []string{
			"-o", "${output:00000000}",
		},
		Outputs: []string{"00000000"},
	}
	const count = 4096
	for index := 0; index < count; index++ {
		node := ActionPlanNode{
			ID:      fmt.Sprintf("node-%08d", index),
			Stage:   "target",
			Kind:    "generate",
			Tool:    "cc",
			Product: "vmlinux",
			Outputs: []ActionPlanOutput{{
				Tree: "objects",
				Path: fmt.Sprintf("generated/%08d.o", index),
			}},
		}
		if _, err := appendActionPlanNode(plan, node, recipe); err != nil {
			t.Fatalf("append node %d: %v", index, err)
		}
	}
	if got, want := plan.nodeLookupCount, len(plan.Nodes); got != want {
		t.Fatalf("indexed node count = %d, want %d", got, want)
	}
	if got, want := len(plan.outputProducers), count; got != want {
		t.Fatalf("indexed output count = %d, want %d", got, want)
	}
	producer, slot, ok := planProducerByOutput(plan, "objects", "generated/00004095.o")
	if !ok || producer != "node-00004095" || slot != 0 {
		t.Fatalf("last producer = (%q, %d, %t)", producer, slot, ok)
	}

	// External-module planning may append to the public slice directly. The
	// next lookup notices the changed prefix and rebuilds once.
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "direct", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "direct.ko"}},
	})
	producer, slot, ok = planProducerByOutput(plan, "modules", "direct.ko")
	if !ok || producer != "direct" || slot != 0 {
		t.Fatalf("direct producer = (%q, %d, %t)", producer, slot, ok)
	}
	if got, want := plan.nodeLookupCount, len(plan.Nodes); got != want {
		t.Fatalf("rebuilt node count = %d, want %d", got, want)
	}
}

func TestActionPlanLogicalLookupIgnoresVersionedArtifacts(t *testing.T) {
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{
			ID: "versioned",
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: "generated/shared.o",
				ArtifactPath: ".linux-bzl-versions/first/generated/shared.o",
			}},
		},
		{
			ID:      "canonical",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/shared.o"}},
		},
	}}
	producer, slot, ok := planProducerByOutput(plan, "objects", "generated/shared.o")
	if !ok || producer != "canonical" || slot != 0 {
		t.Fatalf("logical producer = (%q, %d, %t), want canonical slot 0", producer, slot, ok)
	}
	if got, want := len(plan.outputProducers), 1; got != want {
		t.Fatalf("logical producer index size = %d, want %d", got, want)
	}
}

func TestActionPlanSourceLookupTracksSparseAndIncrementalIDs(t *testing.T) {
	plan := &ActionPlan{Sources: []ActionPlanSource{
		{ID: "src-00000001", Namespace: "kernel", Path: "first.c"},
		{ID: "src-00000027", Namespace: "kernel", Path: "sparse.c"},
	}}
	if got, err := ensureActionPlanSource(plan, "kernel", "first.c"); err != nil || got != "src-00000001" {
		t.Fatalf("existing source = %q, %v", got, err)
	}
	if got, err := ensureActionPlanSource(plan, "kernel", "next.c"); err != nil || got != "src-00000028" {
		t.Fatalf("new source = %q, %v", got, err)
	}
	const count = 4096
	for index := 0; index < count; index++ {
		if _, err := ensureActionPlanSource(plan, "kernel", fmt.Sprintf("generated/%08d.c", index)); err != nil {
			t.Fatalf("append source %d: %v", index, err)
		}
	}
	if got, want := plan.sourceLookupCount, len(plan.Sources); got != want {
		t.Fatalf("indexed source count = %d, want %d", got, want)
	}
	if got, want := len(plan.sourceIDs), len(plan.Sources); got != want {
		t.Fatalf("indexed source keys = %d, want %d", got, want)
	}
}
