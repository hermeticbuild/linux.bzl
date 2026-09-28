package kconfig

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func familyTestConfig(used, other string) map[string]string {
	return map[string]string{
		".config":                       "CONFIG_USED=" + used + "\nCONFIG_OTHER=" + other + "\n",
		"include/config/auto.conf":      "CONFIG_USED=" + used + "\nCONFIG_OTHER=" + other + "\n",
		"include/config/auto.conf.cmd":  "cmd_auto_conf := true\n",
		"include/generated/autoconf.h":  "#ifndef __GENERATED_AUTOCONF_H__\n#define __GENERATED_AUTOCONF_H__\n#define CONFIG_USED " + used + "\n#define CONFIG_OTHER " + other + "\n#endif\n",
		"include/generated/rustc_cfg":   "--cfg=CONFIG_USED=\"" + used + "\"\n--cfg=CONFIG_OTHER=\"" + other + "\"\n",
		"include/config/kernel.release": "6.12.0-test\n",
	}
}

func familyTestSnapshot(t *testing.T, sourceOrdinal int, artifactPath string, config map[string]string, dependencies ConfigDependencySet) ActionPlanSnapshot {
	t.Helper()
	kernelSourceID := "src-" + planOrdinal(sourceOrdinal)
	configSourceID := "src-" + planOrdinal(sourceOrdinal+1)
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "compile-example",
		WorkingInputs:    map[string]string{"source:config:00000001": "include/generated/autoconf.h"},
		Sources:          []string{"source:00000000", "config:00000001"},
		Outputs:          []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	node := ActionPlanNode{
		Stage: "target", Kind: "compile", Recipe: recipeID, Tool: "cc", Product: "image",
		Sources: []ActionPlanSourceEdge{
			{Role: "source", SourceID: kernelSourceID},
			{Role: "config", SourceID: configSourceID},
		},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o", ArtifactPath: artifactPath}},
	}
	node.ID = node.ContentID()
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{
			{ID: kernelSourceID, Namespace: "kernel", Path: "drivers/example.c"},
			{ID: configSourceID, Namespace: "config", Path: "autoconf.h"},
		},
		Recipes: map[string]ActionRecipe{recipeID: recipe},
		Nodes:   []ActionPlanNode{node},
		Products: []ActionPlanProduct{{
			Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker,
		}},
	}
	if !dependencies.Opaque && !slices.Contains(dependencies.ObjectPaths, "include/generated/autoconf.h") {
		dependencies.ObjectPaths = append(dependencies.ObjectPaths, "include/generated/autoconf.h")
	}
	snapshot, err := canonicalActionPlanSnapshot(
		plan, map[string]ConfigDependencySet{node.ID: dependencies}, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestMixedPrecisionSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	snapshot := familyTestSnapshot(
		t, 1, "", config,
		ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
	)
	plan := snapshotActionPlan(snapshot)
	precise := plan.Nodes[0]
	opaque := precise
	opaque.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/opaque.o"}}
	opaque.ID = opaque.ContentID()
	if opaque.ID == precise.ID {
		t.Fatal("mixed-precision compiler nodes have the same standalone identity")
	}
	plan.Nodes = append(plan.Nodes, opaque)
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	snapshot, err := canonicalActionPlanSnapshot(
		plan,
		map[string]ConfigDependencySet{
			precise.ID: snapshot.ConfigDependencies[precise.ID],
			opaque.ID:  {Opaque: true, Reason: "mixed opaque compiler"},
		},
		config,
	)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestChainSnapshot(t *testing.T, salt, artifactPath string) ActionPlanSnapshot {
	t.Helper()
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}", salt},
		WorkingDirectory: "compile-chain",
		WorkingInputs:    map[string]string{"source:config:00000001": "include/generated/autoconf.h"},
		Sources:          []string{"source:00000000", "config:00000001"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${input:object:00000000}", "-out", "${output:00000000}", salt},
		Inputs:    []string{"object:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "drivers/example.c"},
			{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"},
		},
		Recipes: map[string]ActionRecipe{producerRecipeID: producerRecipe, consumerRecipeID: consumerRecipe},
		Nodes: []ActionPlanNode{
			{
				ID: "producer", Stage: "target", Kind: "compile", Recipe: producerRecipeID, Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}, {Role: "config", SourceID: "src-00000002"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o", ArtifactPath: artifactPath}},
			},
			{
				ID: "consumer", Stage: "target", Kind: "copy", Recipe: consumerRecipeID, Tool: "actionfile", Product: "image",
				Inputs:  []ActionPlanNodeEdge{{Role: "object", ProducerID: "producer", Slot: 0}},
				Outputs: []ActionPlanOutput{{Tree: "image", Path: "arch/x86/boot/bzImage"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "image", Path: "arch/x86/boot/bzImage"}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		if len(node.Sources) != 0 {
			dependencies[node.ID] = ConfigDependencySet{
				Symbols: []string{"CONFIG_USED"}, ObjectPaths: []string{"include/generated/autoconf.h"},
			}
		} else {
			dependencies[node.ID] = ConfigDependencySet{}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestInputSetSnapshot(t *testing.T, sourceOrdinal int) ActionPlanSnapshot {
	t.Helper()
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	sourceID := "src-" + planOrdinal(sourceOrdinal)
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{{ID: sourceID, Namespace: "kernel", Path: "include/seed.h"}},
		Recipes: map[string]ActionRecipe{producerRecipeID: producerRecipe, consumerRecipeID: consumerRecipe},
		Nodes: []ActionPlanNode{
			{
				ID: "producer", Stage: "prehost", Kind: "generate", Recipe: producerRecipeID,
				Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prehost", Path: "generated/prehost"}},
			},
			{
				ID: "consumer", Stage: "target", Kind: "generate", Recipe: consumerRecipeID,
				Tool: "actionfile", Product: "image",
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/final.o"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: "drivers/final.o"}},
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	consumer := &plan.Nodes[1]
	consumer.InputSet, err = store.Insert(consumer.InputSet, ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "include/seed.h"},
		SourceID: sourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer.InputSet, err = store.Insert(consumer.InputSet, ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "generated/prehost"},
		ProducerID: "producer", Slot: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestSegmentChainSnapshot(t *testing.T) ActionPlanSnapshot {
	t.Helper()
	sourceRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:source:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"source:00000000"}, Outputs: []string{"00000000"},
	}
	inputRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${input:input:00000000}", "-out", "${output:00000000}"},
		Inputs:    []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	sourceRecipeID, err := sourceRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	inputRecipeID, err := inputRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	type stageOutput struct {
		stage string
		tree  string
		path  string
	}
	stages := []stageOutput{
		{stage: "prehost", tree: "prehost", path: "generated/prehost"},
		{stage: "bootstrap", tree: "bootstrap", path: "generated/bootstrap"},
		{stage: "host", tree: "host", path: "generated/host"},
		{stage: "prep", tree: "prep", path: "generated/prep"},
		{stage: "target", tree: "objects", path: "drivers/final.o"},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host":   "sha256-" + strings.Repeat("2", 64),
			"target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "seed"}},
		Recipes: map[string]ActionRecipe{sourceRecipeID: sourceRecipe, inputRecipeID: inputRecipe},
		Products: []ActionPlanProduct{{
			Name: "image", Tree: "objects", Path: "drivers/final.o",
		}},
	}
	for index, stage := range stages {
		node := ActionPlanNode{
			ID: stage.stage, Stage: stage.stage, Kind: "copy", Recipe: inputRecipeID,
			Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: stage.tree, Path: stage.path}},
		}
		if index == 0 {
			node.Recipe = sourceRecipeID
			node.Sources = []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}}
		} else {
			node.Inputs = []ActionPlanNodeEdge{{Role: "input", ProducerID: stages[index-1].stage, Slot: 0}}
		}
		if stage.stage == "target" {
			node.Product = "image"
		}
		plan.Nodes = append(plan.Nodes, node)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestPrepCopyCompileSnapshot(t *testing.T, config map[string]string, producerMode string) ActionPlanSnapshot {
	t.Helper()
	copyArguments := []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"}
	artifactPath := ""
	switch producerMode {
	case "fallback":
	case "noncanonical":
		artifactPath = ".linux-bzl-versions/selected/include/generated/autoconf.h"
	case "selected":
		copyArguments = append(copyArguments, "-preserve_mode")
	default:
		t.Fatalf("unknown config producer mode %q", producerMode)
	}
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: copyArguments, Sources: []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	copyRecipeID, err := copyRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "compile-example",
		WorkingInputs:    map[string]string{"input:config:00000000": "include/generated/autoconf.h"},
		Sources:          []string{"source:00000000"},
		Inputs:           []string{"config:00000000"},
		Outputs:          []string{"00000000"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host":   "sha256-" + strings.Repeat("2", 64),
			"target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "config", Path: "autoconf.h"},
			{ID: "src-00000002", Namespace: "kernel", Path: "drivers/example.c"},
		},
		Recipes: map[string]ActionRecipe{copyRecipeID: copyRecipe, compileRecipeID: compileRecipe},
		Nodes: []ActionPlanNode{
			{
				ID: "config-copy", Stage: "prep", Kind: "copy", Recipe: copyRecipeID, Tool: "actionfile", Product: "sdk",
				Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{
					Tree: "prep", Path: "include/generated/autoconf.h", ArtifactPath: artifactPath,
				}},
			},
			{
				ID: "compile", Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000002"}},
				Inputs:  []ActionPlanNodeEdge{{Role: "config", ProducerID: "config-copy", Slot: 0}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{
				Symbols: []string{"CONFIG_USED"}, ObjectPaths: []string{"include/generated/autoconf.h"},
			}
		} else {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "direct config projection copy"}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestGeneratedCompileInputSnapshot(t *testing.T, config map[string]string, included bool) ActionPlanSnapshot {
	t.Helper()
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments:   []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Environment: map[string]string{"CONFIG_WITNESS": config[".config"]},
		Sources:     []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	copyRecipeID, err := copyRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "compile-generated-input",
		WorkingInputs: map[string]string{
			"input:generated:00000000": "include/generated/full-config.h",
		},
		Sources: []string{"source:00000000"}, Inputs: []string{"generated:00000000"}, Outputs: []string{"00000000"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "config", Path: ".config"},
			{ID: "src-00000002", Namespace: "kernel", Path: "drivers/example.c"},
		},
		Recipes: map[string]ActionRecipe{copyRecipeID: copyRecipe, compileRecipeID: compileRecipe},
		Nodes: []ActionPlanNode{
			{
				ID: "generated", Stage: "prep", Kind: "copy", Recipe: copyRecipeID, Tool: "actionfile", Product: "sdk",
				Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/full-config.h"}},
			},
			{
				ID: "compile", Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000002"}},
				Inputs:  []ActionPlanNodeEdge{{Role: "generated", ProducerID: "generated", Slot: 0}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind != "compile" {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full-config generated input"}
			continue
		}
		dependencies[node.ID] = ConfigDependencySet{SourcePaths: []string{"drivers/example.c"}}
		if included {
			dependencies[node.ID] = ConfigDependencySet{
				SourcePaths: []string{"drivers/example.c"},
				ObjectPaths: []string{"include/generated/full-config.h"},
			}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestSymmetricGeneratedObjectClosureSnapshot(t *testing.T, config map[string]string, selectedPath string) ActionPlanSnapshot {
	t.Helper()
	generatedPaths := []string{
		"include/generated/base-only.h",
		"include/generated/overlay-only.h",
	}
	if !slices.Contains(generatedPaths, selectedPath) {
		t.Fatalf("unknown selected generated path %q", selectedPath)
	}
	generateRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-line", "#define GENERATED 1", "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	generateRecipeID, err := generateRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "compile-symmetric-generated-closure",
		WorkingInputs:    map[string]string{"input:generated:00000000": selectedPath},
		Sources:          []string{"source:00000000"},
		Inputs:           []string{"generated:00000000"},
		Trees:            []string{"prep"},
		WorkingTrees:     []string{"prep"},
		Outputs:          []string{"00000000"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources:  []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "drivers/example.c"}},
		Recipes:  map[string]ActionRecipe{generateRecipeID: generateRecipe, compileRecipeID: compileRecipe},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	selectedProducer := ""
	for index, pathname := range generatedPaths {
		producerID := "generated-" + planOrdinal(index)
		plan.Nodes = append(plan.Nodes, ActionPlanNode{
			ID: producerID, Stage: "prep", Kind: "generate", Recipe: generateRecipeID,
			Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
		})
		if pathname == selectedPath {
			selectedProducer = producerID
		}
	}
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "compile", Stage: "target", Kind: "compile", Recipe: compileRecipeID,
		Tool: "cc", Product: "image",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}},
		Inputs:  []ActionPlanNodeEdge{{Role: "generated", ProducerID: selectedProducer, Slot: 0}},
		Trees:   []string{"prep"},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o"}},
	})
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{
				SourcePaths: []string{"drivers/example.c"}, ObjectPaths: []string{selectedPath},
			}
		} else {
			dependencies[node.ID] = ConfigDependencySet{}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

const (
	familyCompoundUsesComplete         = "complete"
	familyCompoundUsesIncompleteGlob   = "incomplete-glob"
	familyCompoundUsesIncompleteSearch = "incomplete-search"
	familyCompoundUsesIncompleteRoot   = "incomplete-tool-root"
)

func familyTestCompoundCompilerExtraInputSnapshot(t *testing.T, config map[string]string, useMode string) ActionPlanSnapshot {
	t.Helper()
	const (
		sourcePath  = "drivers/example.c"
		extraPath   = "drivers/example/extra.o"
		targetPath  = "drivers/example.o"
		releasePath = "include/config/kernel.release"
		commandPath = "scripts/basic/.fixdep.cmd"
	)
	extraRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-content_base64", base64.StdEncoding.EncodeToString([]byte("extra object\n")), "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	extraRecipeID, err := extraRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compilerArguments := []string{"-nostdinc", "-c", sourcePath, "-o", targetPath}
	compilerExtra := ""
	extraOperand := extraPath
	switch useMode {
	case familyCompoundUsesComplete:
	case familyCompoundUsesIncompleteGlob:
		extraOperand = "drivers/example/*.o"
	case familyCompoundUsesIncompleteSearch:
		extraOperand = "-L drivers/example -lextra"
	case familyCompoundUsesIncompleteRoot:
		compilerArguments = append([]string{"--sysroot", "drivers/example"}, compilerArguments...)
		compilerExtra = " --sysroot drivers/example"
		extraOperand = ""
	default:
		t.Fatalf("unknown compound use mode %q", useMode)
	}
	outerScript := strings.Join([]string{
		"#!/bin/sh",
		"set -e",
		"cc" + compilerExtra + " -nostdinc -c " + sourcePath + " -o " + targetPath,
		strings.TrimSpace("ld -r -o drivers/.tmp_example.o " + targetPath + " " + extraOperand),
		"mv drivers/.tmp_example.o " + targetPath,
		"",
	}, "\n")
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(outerScript))},
		CompilerInvocation: &ActionRecipeCompilerInvocation{
			Tool: "cc", Arguments: compilerArguments,
		},
		WorkingDirectory: "compound-compile-extra-input",
		WorkingInputs: map[string]string{
			"source:source:00000000": sourcePath,
			"input:extra:00000000":   extraPath,
		},
		WorkingOutputs: map[string]string{"00000000": targetPath},
		Sources:        []string{"source:00000000"},
		Inputs:         []string{"extra:00000000"},
		Outputs:        []string{"00000000"},
	}
	if useMode == familyCompoundUsesComplete {
		compileRecipe.CompilerInvocation.WorkingInputUsesComplete = true
		compileRecipe.CompilerInvocation.WorkingInputUses = []string{
			"input:extra:00000000", "source:source:00000000",
		}
		compileRecipe.WorkingInputs["input:ambient-command:00000002"] = commandPath
		compileRecipe.WorkingInputs["input:ambient-release:00000001"] = releasePath
		compileRecipe.Inputs = []string{"extra:00000000", "ambient-release:00000001", "ambient-command:00000002"}
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	nodes := []ActionPlanNode{
		{
			ID: "extra", Stage: "prep", Kind: "generate", Recipe: extraRecipeID, Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: extraPath}},
		},
	}
	compileInputs := []ActionPlanNodeEdge{{Role: "extra", ProducerID: "extra", Slot: 0}}
	if useMode == familyCompoundUsesComplete {
		nodes = append(nodes,
			ActionPlanNode{
				ID: "ambient-release", Stage: "prep", Kind: "generate", Recipe: extraRecipeID, Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: releasePath}},
			},
			ActionPlanNode{
				ID: "ambient-command", Stage: "prep", Kind: "generate", Recipe: extraRecipeID, Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: commandPath}},
			},
		)
		compileInputs = append(compileInputs,
			ActionPlanNodeEdge{Role: "ambient-release", ProducerID: "ambient-release", Slot: 0},
			ActionPlanNodeEdge{Role: "ambient-command", ProducerID: "ambient-command", Slot: 0},
		)
	}
	nodes = append(nodes, ActionPlanNode{
		ID: "compile", Stage: "target", Kind: "compile", Recipe: compileRecipeID,
		Tool: compactKbuildScriptRunnerRole, Product: "image",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}},
		Inputs:  compileInputs,
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: targetPath}},
	})
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources:  []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: sourcePath}},
		Recipes:  map[string]ActionRecipe{extraRecipeID: extraRecipe, compileRecipeID: compileRecipe},
		Nodes:    nodes,
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: targetPath}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{SourcePaths: []string{sourcePath}}
		} else {
			dependencies[node.ID] = ConfigDependencySet{}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestWholePrepTreeSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	base := familyTestGeneratedCompileInputSnapshot(t, config, false)
	plan := snapshotActionPlan(base)
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.Kind != "compile" {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		recipe.Trees = []string{"prep"}
		recipe.WorkingTrees = []string{"prep"}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
		node.Trees = []string{"prep"}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{SourcePaths: []string{"drivers/example.c"}}
		} else {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full-config generated input"}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestOpaqueWholePrepTreeSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	base := familyTestGeneratedCompileInputSnapshot(t, config, false)
	plan := snapshotActionPlan(base)
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.Kind != "compile" {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		recipe.Inputs = nil
		recipe.WorkingInputs = nil
		recipe.Trees = []string{"prep"}
		recipe.WorkingTrees = []string{"prep"}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
		node.Inputs = nil
		node.Trees = []string{"prep"}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "opaque prep-tree consumer"}
		} else {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full-config generated input"}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestOpaquePrepConfigWriterSnapshot(t *testing.T, config map[string]string, producerMode string) ActionPlanSnapshot {
	t.Helper()
	base := familyTestPrepCopyCompileSnapshot(t, config, producerMode)
	plan := snapshotActionPlan(base)
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.Kind != "compile" {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		recipe.Inputs = nil
		recipe.WorkingInputs = nil
		recipe.Trees = []string{"prep"}
		recipe.WorkingTrees = []string{"prep"}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
		node.Inputs = nil
		node.Trees = []string{"prep"}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "opaque selected-config prep-tree consumer"}
		} else {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "selected config projection writer"}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestProjectedGeneratorWholePrepTreeSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	const projectedPath = "include/generated/projected.h"
	generatorRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "awk",
		Arguments: []string{
			"-f", "${source:script:00000000}", "${source:config:00000001}",
		},
		WorkingDirectory: "generate-projected-header",
		Sources:          []string{"script:00000000", "config:00000001"},
		Outputs:          []string{"00000000"},
		Stdout:           "00000000",
	}
	generatorRecipeID, err := generatorRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "link-driver", Tool: "cc",
		Arguments:        []string{"-o", "${output:00000000}"},
		WorkingDirectory: "link-whole-prep-tree",
		Trees:            []string{"prep"},
		WorkingTrees:     []string{"prep"},
		Outputs:          []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "scripts/projected.awk"},
			{ID: "src-00000002", Namespace: "config", Path: ".config"},
		},
		Recipes: map[string]ActionRecipe{
			generatorRecipeID: generatorRecipe,
			consumerRecipeID:  consumerRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "projected-generator", Stage: "prep", Kind: "generate", Recipe: generatorRecipeID,
				Tool: "awk", Product: "sdk",
				Sources: []ActionPlanSourceEdge{
					{Role: "script", SourceID: "src-00000001"},
					{Role: "config", SourceID: "src-00000002"},
				},
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: projectedPath}},
			},
			{
				ID: "whole-tree-consumer", Stage: "target", Kind: "link-driver", Recipe: consumerRecipeID,
				Tool: "cc", Product: "image", Trees: []string{"prep"},
				Outputs: []ActionPlanOutput{{Tree: "image", Path: "kernel"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "image", Path: "kernel"}},
		projectedGeneratorCandidates: map[string]projectedGeneratorCandidate{"projected-generator": {
			TargetPath: projectedPath, TargetSlot: 0,
			ConfigProjectionPrefixes: []string{"CONFIG_USED"},
		}},
	}
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		recipe := plan.Recipes[node.Recipe]
		switch {
		case len(recipe.ConfigProjectionPrefixes) != 0:
			dependencies[node.ID] = ConfigDependencySet{Symbols: []string{"CONFIG_USED"}}
		case node.Tool == "awk":
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "full projected-generator replay"}
		default:
			dependencies[node.ID] = ConfigDependencySet{}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestNonCompilerConfigSourceSnapshot(t *testing.T, config map[string]string, kind, tool string, semanticUse bool) ActionPlanSnapshot {
	t.Helper()
	arguments := []string{"-o", "${output:00000000}"}
	if semanticUse {
		arguments = append(arguments, "--config", "${source:config:00000000}")
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: kind, Tool: tool,
		Arguments:        arguments,
		WorkingDirectory: "known-config-free",
		WorkingInputs:    map[string]string{"source:config:00000000": ".config"},
		Sources:          []string{"config:00000000"}, Outputs: []string{"00000000"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host":   "sha256-" + strings.Repeat("2", 64),
			"target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "config", Path: ".config"}},
		Recipes: map[string]ActionRecipe{recipeID: recipe},
		Nodes: []ActionPlanNode{{
			ID: "known-config-free", Stage: "target", Kind: kind, Recipe: recipeID, Tool: tool, Product: "image",
			Sources: []ActionPlanSourceEdge{{Role: "config", SourceID: "src-00000001"}},
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/config-free.o"}},
		}},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	dependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, plan.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := dependencies.Opaque; got != semanticUse {
		t.Fatalf("semantic config use opaque = %t, want %t: %#v", got, semanticUse, dependencies)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, map[string]ConfigDependencySet{
		plan.Nodes[0].ID: dependencies,
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyTestObservedStateSnapshot(t *testing.T, salt string, observed bool) ActionPlanSnapshot {
	t.Helper()
	statePath := path.Join(compactKbuildSideOutputStateDirectory, salt+".state")
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""},
		Outputs:   []string{"00000000"},
	}
	observedPath := ""
	if observed {
		observedPath = "drivers/example.generated"
		producerRecipe.Arguments[1] = "${work:root}/" + observedPath
		producerRecipe.WorkingDirectory = "observed-state"
		producerRecipe.ObservedOutputs = map[string]string{"00000000": observedPath}
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${input:state:00000000}", "-out", "${output:00000000}"},
		Inputs:    []string{"state:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "producer", Stage: "target", Kind: "generate", Recipe: producerRecipeID,
				Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{
					Tree: "metadata", Path: statePath, ObservedPath: observedPath,
				}},
			},
			{
				ID: "consumer", Stage: "target", Kind: "copy", Recipe: consumerRecipeID,
				Tool: "actionfile", Product: "image",
				Inputs:  []ActionPlanNodeEdge{{Role: "state", ProducerID: "producer", Slot: 0}},
				Outputs: []ActionPlanOutput{{Tree: "image", Path: "arch/x86/boot/bzImage"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "image", Path: "arch/x86/boot/bzImage"}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestActionPlanSnapshotCanonicalRoundTrip(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	plan := snapshotActionPlan(snapshot)
	filename := filepath.Join(t.TempDir(), "variant.snapshot.json.gz")
	if err := WriteActionPlanSnapshot(filename, plan, snapshot.ConfigDependencies, snapshot.ConfigFiles); err != nil {
		t.Fatal(err)
	}
	secondFilename := filepath.Join(t.TempDir(), "variant-again.snapshot.json.gz")
	if err := WriteActionPlanSnapshot(secondFilename, plan, snapshot.ConfigDependencies, snapshot.ConfigFiles); err != nil {
		t.Fatal(err)
	}
	firstTransport, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	secondTransport, err := os.ReadFile(secondFilename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstTransport, secondTransport) {
		t.Fatal("snapshot gzip transport is not deterministic")
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(firstTransport))
	if err != nil {
		t.Fatal(err)
	}
	if !gzipReader.ModTime.IsZero() || gzipReader.Name != "" || gzipReader.Comment != "" || gzipReader.OS != 255 {
		t.Fatalf("snapshot gzip header is not deterministic: %#v", gzipReader.Header)
	}
	if err := gzipReader.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadActionPlanSnapshot(filename)
	if err != nil {
		t.Fatal(err)
	}
	left, err := snapshot.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	right, err := decoded.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, right) {
		t.Fatal("snapshot changed across canonical round trip")
	}
	trailingJSON, err := compressCanonicalActionPlanSnapshot(
		append(slices.Clone(left), []byte("{}\n")...),
		MaxActionPlanSnapshotCompressedBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, trailingJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadActionPlanSnapshot(filename); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("non-canonical snapshot error = %v", err)
	}
}

func TestActionPlanSnapshotRoundTripPreservesPersistentInputSets(t *testing.T) {
	snapshot := familyTestInputSetSnapshot(t, 1)
	if len(snapshot.InputSets) == 0 {
		t.Fatal("snapshot omitted its reachable persistent input-set nodes")
	}
	filename := filepath.Join(t.TempDir(), "input-set.snapshot.json.gz")
	if err := WriteActionPlanSnapshot(
		filename, snapshotActionPlan(snapshot), snapshot.ConfigDependencies, snapshot.ConfigFiles,
	); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadActionPlanSnapshot(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.InputSets, snapshot.InputSets) {
		t.Fatalf("decoded input sets differ:\n got %#v\nwant %#v", decoded.InputSets, snapshot.InputSets)
	}
	var consumer ActionPlanNode
	for _, node := range decoded.Nodes {
		if node.InputSet != "" {
			consumer = node
		}
	}
	if consumer.ID == "" {
		t.Fatal("decoded snapshot has no input-set consumer")
	}
	store, err := NewActionPlanInputSetStoreFromNodes(decoded.InputSets)
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	if err := store.Walk(consumer.InputSet, func(ActionPlanInputSetEntry) error {
		entries++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entries != 2 {
		t.Fatalf("decoded consumer input-set entries = %d, want 2", entries)
	}
}

func TestActionPlanSnapshotStreamingCanonicalEncodingMatchesJSON(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	for pathname := range snapshot.ConfigFiles {
		snapshot.ConfigFiles[pathname] += "# escaping: <>& \\t     �\n"
		break
	}
	want, err := marshalCanonicalActionPlanSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeActionPlanSnapshotCanonicalJSON(&got, snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("streaming canonical encoding differs from encoding/json:\n got %q\nwant %q", got.Bytes(), want)
	}
}

func TestActionPlanSnapshotTransportBounds(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	canonical, err := snapshot.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := compressCanonicalActionPlanSnapshot(canonical, MaxActionPlanSnapshotCompressedBytes)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "bounded.action-plan.json.gz")
	if err := os.WriteFile(filename, compressed, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("compressed reader", func(t *testing.T) {
		_, err := readActionPlanSnapshotWithLimits(filename, int64(len(compressed)-1), int64(len(canonical)))
		if err == nil || !strings.Contains(err.Error(), "compressed action plan snapshot contains") {
			t.Fatalf("compressed size error = %v", err)
		}
	})
	t.Run("decompressed reader", func(t *testing.T) {
		_, err := readActionPlanSnapshotWithLimits(filename, int64(len(compressed)), int64(len(canonical)-1))
		if err == nil || !strings.Contains(err.Error(), "decompressed action plan snapshot contains") {
			t.Fatalf("decompressed size error = %v", err)
		}
	})
	t.Run("compressed writer", func(t *testing.T) {
		_, err := compressCanonicalActionPlanSnapshot(canonical, int64(len(compressed)-1))
		if err == nil || !strings.Contains(err.Error(), "compressed action plan snapshot exceeds") {
			t.Fatalf("compressed writer size error = %v", err)
		}
	})
}

func TestActionPlanSnapshotRejectsMalformedOrTrailingGzip(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	canonical, err := snapshot.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := compressCanonicalActionPlanSnapshot(canonical, MaxActionPlanSnapshotCompressedBytes)
	if err != nil {
		t.Fatal(err)
	}
	badChecksum := slices.Clone(compressed)
	badChecksum[len(badChecksum)-8] ^= 1
	tests := []struct {
		name    string
		data    []byte
		message string
	}{
		{name: "invalid header", data: []byte("not a gzip stream"), message: "open action plan snapshot gzip stream"},
		{name: "truncated member", data: slices.Clone(compressed[:len(compressed)-4]), message: "decompress action plan snapshot"},
		{name: "bad checksum", data: badChecksum, message: "decompress action plan snapshot"},
		{name: "trailing bytes", data: append(slices.Clone(compressed), []byte("trailing")...), message: "trailing compressed bytes"},
		{name: "concatenated member", data: append(slices.Clone(compressed), compressed...), message: "trailing compressed bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "invalid.action-plan.json.gz")
			if err := os.WriteFile(filename, test.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadActionPlanSnapshot(filename); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ReadActionPlanSnapshot() error = %v, want substring %q", err, test.message)
			}
		})
	}
}

func TestActionPlanSnapshotRejectsUnknownFieldsWhileStreaming(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	canonical, err := snapshot.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) < 2 || !bytes.Equal(canonical[len(canonical)-2:], []byte("}\n")) {
		t.Fatalf("unexpected canonical suffix %q", canonical)
	}
	withUnknown := append(slices.Clone(canonical[:len(canonical)-2]), []byte(",\"unknown_streaming_field\":true}\n")...)
	compressed, err := compressCanonicalActionPlanSnapshot(withUnknown, MaxActionPlanSnapshotCompressedBytes)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "unknown.action-plan.json.gz")
	if err := os.WriteFile(filename, compressed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadActionPlanSnapshot(filename); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("ReadActionPlanSnapshot() error = %v, want unknown-field rejection", err)
	}
}

func TestActionPlanSnapshotRejectsNoncanonicalDecompressedJSON(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	canonical, err := snapshot.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if canonical[len(canonical)-1] != '\n' {
		t.Fatal("test snapshot is not newline-terminated")
	}
	compressed, err := compressCanonicalActionPlanSnapshot(canonical[:len(canonical)-1], MaxActionPlanSnapshotCompressedBytes)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "noncanonical.action-plan.json.gz")
	if err := os.WriteFile(filename, compressed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadActionPlanSnapshot(filename); err == nil || !strings.Contains(err.Error(), "not canonically encoded") {
		t.Fatalf("ReadActionPlanSnapshot() error = %v, want canonical-encoding rejection", err)
	}
}

func familyTestKernelSourceTreeSnapshot(t *testing.T, headers ...string) ActionPlanSnapshot {
	t.Helper()
	paths := append([]string{"drivers/example.c"}, headers...)
	sort.Strings(paths)
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{
			"-I", "${tree:kernel}/include", "-c", "${tree:kernel}/drivers/example.c",
			"-o", "${output:00000000}",
		},
		Trees: []string{"kernel"}, Sources: make([]string, len(paths)), Outputs: []string{"00000000"},
	}
	node := ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "image",
		Trees: []string{"kernel"}, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o"}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes:  map[string]ActionRecipe{},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	for ordinal, pathname := range paths {
		sourceID := "src-" + planOrdinal(ordinal+1)
		plan.Sources = append(plan.Sources, ActionPlanSource{ID: sourceID, Namespace: "kernel", Path: pathname})
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "source", SourceID: sourceID})
		recipe.Sources[ordinal] = "source:" + planOrdinal(ordinal)
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[recipeID] = recipe
	node.Recipe = recipeID
	node.ID = node.ContentID()
	plan.Nodes = []ActionPlanNode{node}
	snapshot, err := canonicalActionPlanSnapshot(
		plan,
		map[string]ConfigDependencySet{node.ID: {SourcePaths: paths}},
		familyTestConfig("y", "n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestActionPlanFamilyAttachesSymmetricExactKernelSourceClosure(t *testing.T) {
	base := ActionPlanFamilyVariant{Name: "base", Snapshot: familyTestKernelSourceTreeSnapshot(t, "include/a.h")}
	overlay := ActionPlanFamilyVariant{Name: "overlay", Snapshot: familyTestKernelSourceTreeSnapshot(t, "include/b.h")}
	var baselineID string
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		if len(family.Nodes) != 1 {
			t.Fatalf("permutation %d family nodes = %#v, want one shared compiler", permutation, family.Nodes)
		}
		node := family.Nodes[0]
		if got, want := family.Memberships[node.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
			t.Fatalf("permutation %d memberships = %q, want %q", permutation, got, want)
		}
		if permutation == 0 {
			baselineID = node.ID
		} else if node.ID != baselineID {
			t.Fatalf("source-closure identity changed with variant order: %s != %s", node.ID, baselineID)
		}
		if got, want := len(node.familySourceProjections), 3; got != want {
			t.Fatalf("permutation %d source projections = %#v, want %d", permutation, node.familySourceProjections, want)
		}
		manifest, err := familyNodeSourceProjections(node)
		if err != nil {
			t.Fatal(err)
		}
		projected := map[string]bool{}
		for _, binding := range manifest.Bindings {
			if binding.Tree != "kernel" {
				t.Fatalf("permutation %d projection tree = %q, want kernel", permutation, binding.Tree)
			}
			projected[binding.Path] = true
		}
		for _, pathname := range []string{"drivers/example.c", "include/a.h", "include/b.h"} {
			if !projected[pathname] {
				t.Fatalf("permutation %d exact source closure omits %q: %#v", permutation, pathname, manifest)
			}
		}
		if len(family.Sources) != 3 {
			t.Fatalf("permutation %d retained sources = %#v, want exact three-file closure", permutation, family.Sources)
		}
		entries, err := family.entries()
		if err != nil {
			t.Fatal(err)
		}
		markers := map[string]bool{}
		for _, entry := range entries {
			markers[entry.path] = true
		}
		if !markers["schema/"+LinuxKernelFamilyPlanSchema] {
			t.Fatalf("permutation %d entries omit v6 schema marker", permutation)
		}
		root := path.Join("nodes", node.Stage, node.ID, "in")
		if !markers[path.Join(root, "source-tree-pack", "kernel", "00000000.0,1,2")] {
			t.Fatalf("permutation %d entries omit packed source ordinals below %s", permutation, root)
		}
		bindingMarkers := 0
		for marker := range markers {
			if strings.HasPrefix(marker, path.Join(root, "source-tree-bindings")+"/") {
				bindingMarkers++
			}
		}
		if bindingMarkers != 1 {
			t.Fatalf("permutation %d source binding markers = %d, want one", permutation, bindingMarkers)
		}
	}
}

func familyValidationSourceProjectionFixture(t *testing.T, projectionCount int) *ActionPlanFamily {
	t.Helper()
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{
			"-I", "${tree:kernel}/include", "-c", "${tree:kernel}/drivers/example.c",
			"-o", "${output:00000000}",
		},
		Trees: []string{"kernel"}, Sources: make([]string, projectionCount), Outputs: []string{"00000000"},
	}
	node := ActionPlanNode{
		ID: "projection-scale", Stage: "target", Kind: "compile", Tool: "cc", Product: "image",
		Trees: []string{"kernel"}, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example.o"}},
	}
	sources := make([]ActionPlanSource, 0, projectionCount)
	for ordinal := 0; ordinal < projectionCount; ordinal++ {
		pathname := fmt.Sprintf("include/generated/scale/%08d.h", ordinal)
		sourceID := semanticFamilySourceID("kernel", pathname)
		sources = append(sources, ActionPlanSource{ID: sourceID, Namespace: "kernel", Path: pathname})
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "kernel-source", SourceID: sourceID})
		node.familySourceProjections = append(node.familySourceProjections, ActionPlanFamilySourceProjection{
			SourceOrdinal: ordinal, Tree: "kernel", Path: pathname,
		})
		recipe.Sources[ordinal] = "kernel-source:" + planOrdinal(ordinal)
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	node.Recipe = recipeID
	ids, _, _, err := familyNodeIDs(
		&ActionPlan{Sources: sources, Nodes: []ActionPlanNode{node}},
		func(source ActionPlanSource) string { return source.ID },
	)
	if err != nil {
		t.Fatal(err)
	}
	node.ID = ids[node.ID]
	return &ActionPlanFamily{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources:  sources, Recipes: map[string]ActionRecipe{recipeID: recipe}, Nodes: []ActionPlanNode{node},
		Products: []ActionPlanFamilyProduct{{Variant: "variant", Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
		Views:    []ActionPlanFamilyView{{Variant: "variant", Tree: "objects", NodeID: node.ID, Slot: 0, ArtifactPath: "drivers/example.o"}},
		Capsules: map[string]map[string]string{}, Memberships: map[string][]string{node.ID: {"variant"}}, Variants: []string{"variant"},
	}
}

func TestActionPlanFamilyValidationIndexesLargeSourceProjectionClosureOnce(t *testing.T) {
	const projectionCount = 16 << 10
	family := familyValidationSourceProjectionFixture(t, projectionCount)
	stats := &actionPlanFamilyValidationStats{}
	if err := family.validateWithStats(stats); err != nil {
		t.Fatal(err)
	}
	if got := stats.recipeSourceBindingsIndexed; got != projectionCount {
		t.Fatalf("indexed recipe source bindings = %d, want %d", got, projectionCount)
	}
	if got := stats.sourceProjectionBindingLookups; got != projectionCount {
		t.Fatalf("source projection binding lookups = %d, want %d", got, projectionCount)
	}
}

func TestActionPlanFamilyValidationRejectsMissingSourceProjectionBinding(t *testing.T) {
	family := familyValidationSourceProjectionFixture(t, 8)
	node := &family.Nodes[0]
	oldRecipeID := node.Recipe
	recipe := family.Recipes[oldRecipeID]
	recipe.Sources = slices.Clone(recipe.Sources[:len(recipe.Sources)-1])
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	delete(family.Recipes, oldRecipeID)
	family.Recipes[recipeID] = recipe
	node.Recipe = recipeID
	if err := family.validate(); err == nil || !strings.Contains(err.Error(), `source projection binding "kernel-source:00000007" is absent from its recipe`) {
		t.Fatalf("family.validate() error = %v, want missing source-projection binding rejection", err)
	}
}

func TestBuiltActionPlanFamilyValidationSkipsDuplicateSemanticRehash(t *testing.T) {
	stats := &actionPlanFamilyValidationStats{}
	validated, err := buildValidatedActionPlanFamilyWithStats(
		[]actionPlanFamilyBuildVariant{{
			variant: ActionPlanFamilyVariant{
				Name:     "variant",
				Snapshot: familyTestChainSnapshot(t, "", ""),
			},
		}},
		stats,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stats.semanticNodesRehashed != 0 || stats.semanticSourceEdgesRehashed != 0 || stats.semanticInputEdgesRehashed != 0 {
		t.Fatalf(
			"sealed builder validation rehashed %d nodes, %d source edges, and %d input edges",
			stats.semanticNodesRehashed, stats.semanticSourceEdgesRehashed, stats.semanticInputEdgesRehashed,
		)
	}

	family := validated.family
	wantSourceEdges, wantInputEdges := 0, 0
	for _, node := range family.Nodes {
		wantSourceEdges += len(node.Sources)
		wantInputEdges += len(node.Inputs)
	}
	publicStats := &actionPlanFamilyValidationStats{}
	if err := family.validateWithStats(publicStats); err != nil {
		t.Fatal(err)
	}
	if got, want := publicStats.semanticNodesRehashed, len(family.Nodes); got != want {
		t.Fatalf("public validation rehashed %d nodes, want %d", got, want)
	}
	if got := publicStats.semanticSourceEdgesRehashed; got != wantSourceEdges {
		t.Fatalf("public validation rehashed %d source edges, want %d", got, wantSourceEdges)
	}
	if got := publicStats.semanticInputEdgesRehashed; got != wantInputEdges {
		t.Fatalf("public validation rehashed %d input edges, want %d", got, wantInputEdges)
	}
}

func TestExportedActionPlanFamilyValidationStillRejectsSemanticMutation(t *testing.T) {
	validated, err := buildValidatedActionPlanFamily([]actionPlanFamilyBuildVariant{{
		variant: ActionPlanFamilyVariant{
			Name:     "variant",
			Snapshot: familyTestChainSnapshot(t, "", ""),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	family := validated.family
	family.Nodes[0].Outputs[0].Path = path.Join(family.Nodes[0].Outputs[0].Path, "tampered")
	if err := family.validate(); err == nil || !strings.Contains(err.Error(), "does not match semantic content") {
		t.Fatalf("family.validate() error = %v, want semantic mutation rejection", err)
	}
}

func TestActionPlanFamilySourceEditKeepsUnrelatedDeclaredInputStable(t *testing.T) {
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "compile",
		Tool:   "cc",
		Arguments: []string{
			"-c", "${source:source:00000000}", "-o", "${output:00000000}",
		},
		Sources: []string{"source:00000000"},
		Outputs: []string{"00000000"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "drivers/affected.c"},
			{ID: "src-00000002", Namespace: "kernel", Path: "drivers/unaffected.c"},
		},
		Recipes: map[string]ActionRecipe{compileRecipeID: compileRecipe},
		Nodes: []ActionPlanNode{
			{
				ID:    "affected",
				Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/affected.o"}},
			},
			{
				ID:    "unaffected",
				Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000002"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/unaffected.o"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{}
	}
	snapshot := func(config map[string]string) ActionPlanSnapshot {
		result, err := canonicalActionPlanSnapshot(plan, dependencies, config)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: snapshot(familyTestConfig("y", "n"))},
		{Name: "overlay", Snapshot: snapshot(familyTestConfig("y", "m"))},
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("family nodes = %d, want %d shared compile nodes", got, want)
	}
	for _, node := range family.Nodes {
		if got, want := family.Memberships[node.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
			t.Fatalf("node %s memberships = %q, want %q", node.ID, got, want)
		}
	}

	// Read the executor handoff instead of the reuse report. These are the
	// exact source markers consumed by expand_linux_family_plan when it builds
	// each map_directory action's input list.
	segments, err := family.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	sourcePaths := map[string]string{}
	nodeSources := map[string][]string{}
	nodeIDsByOutput := map[string]string{}
	var outputMarkers []string
	for _, entry := range segments["target"] {
		parts := strings.Split(entry.path, "/")
		if len(parts) >= 4 && parts[0] == "sources" && parts[2] == "kernel" {
			sourcePaths[parts[1]] = strings.Join(parts[3:], "/")
		}
		if len(parts) == 8 && parts[0] == "nodes" && parts[1] == "target" &&
			parts[3] == "in" && parts[4] == "source" {
			nodeSources[parts[2]] = append(nodeSources[parts[2]], parts[7])
		}
		if len(parts) >= 8 && parts[0] == "nodes" && parts[1] == "target" &&
			parts[3] == "out" && parts[4] == "objects" && parts[6] == "at" {
			outputMarkers = append(outputMarkers, entry.path)
			nodeIDsByOutput[strings.Join(parts[7:], "/")] = parts[2]
		}
	}
	for _, pathname := range []string{"drivers/affected.o", "drivers/unaffected.o"} {
		if nodeIDsByOutput[pathname] == "" {
			t.Fatalf("target family shard has no producer for %q; output markers: %q", pathname, outputMarkers)
		}
	}

	sourceRoot := t.TempDir()
	writeSource := func(pathname, content string) {
		filename := filepath.Join(sourceRoot, filepath.FromSlash(pathname))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSource("drivers/affected.c", "int affected(void) { return 1; }\n")
	writeSource("drivers/unaffected.c", "int unaffected(void) { return 2; }\n")

	// This is deliberately not a reimplementation of Bazel's action key. It
	// observes its relevant invariant here: every declared source File's digest
	// participates in the expanded action key, while undeclared source files do
	// not. Including the stable semantic node ID also catches planner churn.
	declaredInputFingerprint := func(nodeID string) string {
		digest := sha256.New()
		digest.Write([]byte(nodeID))
		sourceIDs := slices.Clone(nodeSources[nodeID])
		sort.Strings(sourceIDs)
		if len(sourceIDs) != 1 {
			t.Fatalf("node %s declared sources = %q, want one exact source", nodeID, sourceIDs)
		}
		for _, sourceID := range sourceIDs {
			pathname := sourcePaths[sourceID]
			if pathname == "" {
				t.Fatalf("node %s references unknown source %s", nodeID, sourceID)
			}
			content, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(pathname)))
			if err != nil {
				t.Fatal(err)
			}
			digest.Write([]byte{0})
			digest.Write([]byte(sourceID))
			digest.Write([]byte{0})
			digest.Write(content)
		}
		return fmt.Sprintf("%x", digest.Sum(nil))
	}
	affectedID := nodeIDsByOutput["drivers/affected.o"]
	unaffectedID := nodeIDsByOutput["drivers/unaffected.o"]
	beforeAffected := declaredInputFingerprint(affectedID)
	beforeUnaffected := declaredInputFingerprint(unaffectedID)

	writeSource("drivers/affected.c", "int affected(void) { return 3; }\n")
	afterAffected := declaredInputFingerprint(affectedID)
	afterUnaffected := declaredInputFingerprint(unaffectedID)
	if afterAffected == beforeAffected {
		t.Fatal("editing a declared source did not change its compile input fingerprint")
	}
	if afterUnaffected != beforeUnaffected {
		t.Fatal("editing an unrelated source changed the unaffected compile input fingerprint")
	}

	// Source bytes are Bazel File inputs, not planner metadata. Re-reducing the
	// same snapshots after the edit must therefore preserve the node IDs that
	// let the unaffected action retain its cache identity.
	afterFamily, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	beforeIDs := make([]string, 0, len(family.Nodes))
	afterIDs := make([]string, 0, len(afterFamily.Nodes))
	for _, node := range family.Nodes {
		beforeIDs = append(beforeIDs, node.ID)
	}
	for _, node := range afterFamily.Nodes {
		afterIDs = append(afterIDs, node.ID)
	}
	if !slices.Equal(beforeIDs, afterIDs) {
		t.Fatalf("source edit churned semantic family node IDs: %q != %q", beforeIDs, afterIDs)
	}
}

func TestActionPlanFamilySourceProjectionProtocolIsBoundedAndCanonical(t *testing.T) {
	const projectionCount = 130
	node := ActionPlanNode{ID: strings.Repeat("a", 64)}
	for ordinal := range projectionCount {
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "kernel-source", SourceID: semanticFamilySourceID("kernel", fmt.Sprintf("include/%03d.h", ordinal))})
		node.familySourceProjections = append(node.familySourceProjections, ActionPlanFamilySourceProjection{
			SourceOrdinal: ordinal, Tree: "kernel", Path: fmt.Sprintf("include/%03d.h", ordinal),
		})
	}
	entries, err := actionPlanFamilySourceProjectionEntries(node, "node")
	if err != nil {
		t.Fatal(err)
	}
	packCount, manifestCount := 0, 0
	for _, entry := range entries {
		if strings.Contains(entry.path, "/source-tree-pack/") {
			packCount++
			if filename := path.Base(entry.path); len(filename) > maximumActionPlanInputMarkerFilename {
				t.Fatalf("packed source projection filename has %d bytes: %q", len(filename), filename)
			}
		}
		if strings.Contains(entry.path, "/source-tree-bindings/") {
			manifestCount++
			decoded, err := DecodeActionPlanSourceProjections(entry.data)
			if err != nil || len(decoded.Bindings) != projectionCount {
				t.Fatalf("decode source projection manifest = %d bindings, %v", len(decoded.Bindings), err)
			}
		}
	}
	if packCount != 3 || manifestCount != 1 {
		t.Fatalf("source projection entries have %d packs/%d manifests, want 3/1", packCount, manifestCount)
	}
	if _, err := actionPlanPackedSourceProjectionMarkerFilenames([]int{maximumActionPlanOrdinal + 1}); err == nil {
		t.Fatal("packed source projections accepted an out-of-range ordinal")
	}
	invalid := ActionPlanSourceProjections{
		Schema: LinuxKernelSourceProjectionsSchema,
		Bindings: map[string]ActionPlanSourceProjectionBinding{
			"source:00000000": {Tree: "kernel", Path: "../escape.h"},
		},
	}
	if _, err := invalid.CanonicalJSON(); err == nil {
		t.Fatal("source projections accepted an escaping path")
	}
}

func TestActionPlanFamilySharesIrrelevantConfigAndCanonicalizesArtifactPath(t *testing.T) {
	base := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	overlay := familyTestSnapshot(t, 27, "drivers/example.o", familyTestConfig("y", "m"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "overlay", Snapshot: overlay},
		{Name: "base", Snapshot: base},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 1; got != want {
		t.Fatalf("unique nodes = %d, want %d", got, want)
	}
	if got, want := family.Memberships[family.Nodes[0].ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("memberships = %q, want %q", got, want)
	}
	if got, want := len(family.Capsules), 1; got != want {
		t.Fatalf("capsules = %d, want %d", got, want)
	}
	for _, source := range family.Sources {
		if source.Namespace == "config" {
			t.Fatalf("family retained external config source %#v", source)
		}
		if source.Namespace == "capsule" && !strings.Contains(source.Path, "/include/generated/autoconf.h") {
			t.Fatalf("family capsule source did not preserve projection location: %#v", source)
		}
	}
	if len(family.Views) != 2 || family.Views[0].ArtifactPath != "drivers/example.o" || family.Views[1].ArtifactPath != "drivers/example.o" {
		t.Fatalf("variant views did not preserve the canonical artifact path: %#v", family.Views)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.Schema != LinuxKernelFamilyReuseReportSchema || !maps.Equal(report.Toolsets, family.Toolsets) {
		t.Fatalf("reuse report schema/toolsets = %q/%q, want %q/%q", report.Schema, report.Toolsets, LinuxKernelFamilyReuseReportSchema, family.Toolsets)
	}
	if report.UniqueNodes != 1 || report.VariantInstances != 2 || report.ReusedInstances != 1 || len(report.Pairs) != 1 {
		t.Fatalf("unexpected reuse report: %#v", report)
	}
	if got, want := report.Nodes, []ActionPlanFamilyReuseNode{{
		NodeID: family.Nodes[0].ID, Kind: "compile", Memberships: []string{"base", "overlay"},
		TypedCompiler: true, PreciseCompile: true,
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reuse node evidence = %#v, want %#v", got, want)
	}
	if report.Pairs[0].EligibleSharedNodes != 1 || report.Pairs[0].EligibleLeftReuseBasisPoints != 10000 || report.Pairs[0].EligibleRightReuseBasisPoints != 10000 {
		t.Fatalf("unexpected eligible reuse pair: %#v", report.Pairs[0])
	}
	if report.Variants[0].TypedCompilerNodes != 1 || report.Variants[1].TypedCompilerNodes != 1 ||
		report.Variants[0].PreciseCompileNodes != 1 || report.Variants[1].PreciseCompileNodes != 1 ||
		report.Variants[0].PreciseCompilerCoverageBasisPoints != 10000 || report.Variants[1].PreciseCompilerCoverageBasisPoints != 10000 ||
		report.Pairs[0].TypedLeftCompilerNodes != 1 || report.Pairs[0].TypedRightCompilerNodes != 1 ||
		report.Pairs[0].TypedSharedCompilerNodes != 1 || report.Pairs[0].TypedLeftReuseBasisPoints != 10000 ||
		report.Pairs[0].TypedRightReuseBasisPoints != 10000 ||
		report.Pairs[0].PreciseSharedCompileNodes != 1 || report.Pairs[0].PreciseLeftReuseBasisPoints != 10000 ||
		report.Pairs[0].PreciseRightReuseBasisPoints != 10000 ||
		report.Pairs[0].EffectivePreciseLeftReuseBasisPoints != 10000 ||
		report.Pairs[0].EffectivePreciseRightReuseBasisPoints != 10000 || len(report.OpaqueReasons) != 0 {
		t.Fatalf("unexpected precise compiler reuse diagnostics: %#v", report)
	}
	if got, want := report.PreciseCompiles, []ActionPlanFamilyPreciseCompile{{
		NodeID: family.Nodes[0].ID, Outputs: []string{"objects:drivers/example.o"},
		OutputDetails: []ActionPlanFamilyPreciseCompileOutput{{
			Slot: 0, Tree: "objects", LogicalPath: "drivers/example.o", ArtifactPath: "drivers/example.o",
			StorePath: familyStorePath(family.Nodes[0].ID, 0),
		}},
		Memberships: []string{"base", "overlay"},
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("precise compiler node diagnostics = %#v, want %#v", got, want)
	}
}

func TestActionPlanFamilyReuseReportSeparatesTypedCompilerCoverageFromPreciseReuse(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyTestMixedPrecisionSnapshot(t, familyTestConfig("y", "n"))},
		{Name: "overlay", Snapshot: familyTestMixedPrecisionSnapshot(t, familyTestConfig("y", "m"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Variants) != 2 || len(report.Pairs) != 1 {
		t.Fatalf("mixed-precision report shape = %#v", report)
	}
	for _, variant := range report.Variants {
		if variant.TypedCompilerNodes != 2 || variant.PreciseCompileNodes != 1 || variant.PreciseCompilerCoverageBasisPoints != 5000 {
			t.Fatalf("variant %s mixed compiler coverage = %#v, want 1/2 precise", variant.Name, variant)
		}
	}
	pair := report.Pairs[0]
	if pair.TypedLeftCompilerNodes != 2 || pair.TypedRightCompilerNodes != 2 || pair.TypedSharedCompilerNodes != 1 ||
		pair.TypedLeftReuseBasisPoints != 5000 || pair.TypedRightReuseBasisPoints != 5000 {
		t.Fatalf("mixed typed compiler reuse = %#v, want 1/2 shared", pair)
	}
	if pair.PreciseLeftCompileNodes != 1 || pair.PreciseRightCompileNodes != 1 || pair.PreciseSharedCompileNodes != 1 ||
		pair.PreciseLeftReuseBasisPoints != 10000 || pair.PreciseRightReuseBasisPoints != 10000 {
		t.Fatalf("mixed conditional precise compiler reuse = %#v, want 1/1 shared", pair)
	}
	if pair.EffectivePreciseLeftReuseBasisPoints != 5000 || pair.EffectivePreciseRightReuseBasisPoints != 5000 {
		t.Fatalf("mixed effective precise compiler reuse = %#v, want 1/2 shared", pair)
	}
	if got, want := len(report.PreciseCompiles), 1; got != want {
		t.Fatalf("mixed precise compiler diagnostics = %d, want %d: %#v", got, want, report.PreciseCompiles)
	}
	preciseEvidence, opaqueEvidence := 0, 0
	for _, node := range report.Nodes {
		if node.PreciseCompile {
			preciseEvidence++
		}
		if node.TypedCompiler && !node.PreciseCompile {
			opaqueEvidence++
		}
	}
	if preciseEvidence != 1 || opaqueEvidence != 2 {
		t.Fatalf("mixed node evidence has %d precise/%d opaque typed compilers, want 1/2: %#v", preciseEvidence, opaqueEvidence, report.Nodes)
	}
}

func TestActionPlanFamilyReuseReportGroupsOpaqueReasons(t *testing.T) {
	base := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{
		Opaque: true, Reason: "dynamic include closure",
	})
	overlay := familyTestSnapshot(t, 1, "", familyTestConfig("y", "m"), ConfigDependencySet{
		Opaque: true, Reason: "dynamic include closure",
	})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base},
		{Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.OpaqueReasons) != 2 || report.OpaqueReasons[0] != (ActionPlanFamilyOpaqueReason{
		Variant: "base", Kind: "compile", Reason: "dynamic include closure", Nodes: 1,
	}) || report.OpaqueReasons[1] != (ActionPlanFamilyOpaqueReason{
		Variant: "overlay", Kind: "compile", Reason: "dynamic include closure", Nodes: 1,
	}) {
		t.Fatalf("opaque reason diagnostics = %#v", report.OpaqueReasons)
	}
	if report.Variants[0].PreciseCompileNodes != 0 || report.Variants[1].PreciseCompileNodes != 0 ||
		report.Pairs[0].PreciseSharedCompileNodes != 0 {
		t.Fatalf("opaque compiler was reported precise: %#v", report)
	}
}

func TestActionPlanFamilyReuseReportIsolatesOpaqueReasonFromPreciseSibling(t *testing.T) {
	base := familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{
		Symbols: []string{"CONFIG_USED"},
	})
	overlay := familyTestSnapshot(t, 1, "", familyTestConfig("y", "m"), ConfigDependencySet{
		Opaque: true, Reason: "dynamic include closure",
	})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base},
		{Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	want := []ActionPlanFamilyOpaqueReason{
		{Variant: "overlay", Kind: "compile", Reason: "dynamic include closure", Nodes: 1},
	}
	if !slices.Equal(report.OpaqueReasons, want) {
		t.Fatalf("isolated opaque reason diagnostics = %#v, want %#v", report.OpaqueReasons, want)
	}
	if report.Variants[0].PreciseCompileNodes != 1 || report.Variants[1].PreciseCompileNodes != 0 ||
		report.Pairs[0].PreciseSharedCompileNodes != 0 {
		t.Fatalf("opaque sibling changed precise compiler classification: %#v", report)
	}
}

func TestActionPlanFamilyReuseReportDoesNotCountAssemblerAsPreciseCompiler(t *testing.T) {
	asAssembler := func(snapshot ActionPlanSnapshot) ActionPlanSnapshot {
		node := snapshot.Nodes[0]
		oldNodeID := node.ID
		recipe := snapshot.Recipes[node.Recipe]
		delete(snapshot.Recipes, node.Recipe)
		recipe.Tool = "as"
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Recipes[recipeID] = recipe
		node.Tool = "as"
		node.Recipe = recipeID
		node.ID = node.ContentID()
		snapshot.Nodes[0] = node
		dependencies := snapshot.ConfigDependencies[oldNodeID]
		delete(snapshot.ConfigDependencies, oldNodeID)
		snapshot.ConfigDependencies[node.ID] = dependencies
		return snapshot
	}
	base := asAssembler(familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{}))
	overlay := asAssembler(familyTestSnapshot(t, 1, "", familyTestConfig("y", "m"), ConfigDependencySet{}))
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base},
		{Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.Variants[0].PreciseCompileNodes != 0 || report.Variants[1].PreciseCompileNodes != 0 ||
		report.Pairs[0].PreciseSharedCompileNodes != 0 {
		t.Fatalf("assembler compile node was reported as a precise cc/cxx compiler: %#v", report)
	}
	if report.Variants[0].TypedCompilerNodes != 0 || report.Variants[1].TypedCompilerNodes != 0 ||
		report.Pairs[0].TypedSharedCompilerNodes != 0 {
		t.Fatalf("assembler compile node was reported as a typed cc/cxx compiler: %#v", report)
	}
}

func TestActionPlanFamilyRejectsMalformedReuseMemberships(t *testing.T) {
	sharedCompileFamily := func(t *testing.T) *ActionPlanFamily {
		t.Helper()
		family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
			{Name: "base", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{})},
			{Name: "overlay", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("y", "m"), ConfigDependencySet{})},
		})
		if err != nil {
			t.Fatal(err)
		}
		return family
	}
	exclusiveCompileFamily := func(t *testing.T) *ActionPlanFamily {
		t.Helper()
		family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
			{Name: "base", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})},
			{Name: "overlay", Snapshot: familyTestSnapshot(t, 1, "", familyTestConfig("n", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})},
		})
		if err != nil {
			t.Fatal(err)
		}
		return family
	}
	sharedChainFamily := func(t *testing.T) *ActionPlanFamily {
		t.Helper()
		family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
			{Name: "base", Snapshot: familyTestChainSnapshot(t, "same", "")},
			{Name: "overlay", Snapshot: familyTestChainSnapshot(t, "same", "")},
		})
		if err != nil {
			t.Fatal(err)
		}
		return family
	}

	tests := []struct {
		name   string
		build  func(*testing.T) *ActionPlanFamily
		mutate func(*testing.T, *ActionPlanFamily)
		want   string
	}{
		{
			name:  "duplicate execution membership",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.Memberships[family.Nodes[0].ID] = []string{"base", "base", "overlay"}
			},
			want: `repeats variant membership "base"`,
		},
		{
			name:  "unknown execution membership",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.Memberships[family.Nodes[0].ID] = []string{"base", "missing"}
			},
			want: `unknown variant membership "missing"`,
		},
		{
			name:  "noncanonical execution membership",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.Memberships[family.Nodes[0].ID] = []string{"overlay", "base"}
			},
			want: "variant memberships are not in canonical order",
		},
		{
			name:  "duplicate precise membership",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.preciseCompileMemberships[family.Nodes[0].ID] = []string{"base", "base", "overlay"}
			},
			want: `repeats variant membership "base"`,
		},
		{
			name:  "unknown precise membership",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.preciseCompileMemberships[family.Nodes[0].ID] = []string{"base", "missing"}
			},
			want: `unknown variant membership "missing"`,
		},
		{
			name:  "precise membership differs from execution membership",
			build: exclusiveCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				for _, node := range family.Nodes {
					if slices.Equal(family.Memberships[node.ID], []string{"base"}) {
						family.preciseCompileMemberships = map[string][]string{node.ID: {"overlay"}}
						return
					}
				}
				t.Fatal("exclusive family has no base compiler node")
			},
			want: "do not match execution memberships",
		},
		{
			name:  "precise marker on noncompiler",
			build: sharedChainFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				for _, node := range family.Nodes {
					if node.Kind == "copy" {
						family.preciseCompileMemberships = map[string][]string{node.ID: {"base", "overlay"}}
						return
					}
				}
				t.Fatal("shared chain family has no copy node")
			},
			want: "is not a typed cc/cxx compile",
		},
		{
			name:  "unknown precise node",
			build: sharedCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				family.preciseCompileMemberships = map[string][]string{strings.Repeat("f", 64): {"base"}}
			},
			want: "precise compiler diagnostic references unknown node",
		},
		{
			name:  "membership not derived from roots",
			build: exclusiveCompileFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				for _, node := range family.Nodes {
					if slices.Equal(family.Memberships[node.ID], []string{"base"}) {
						family.Memberships[node.ID] = []string{"base", "overlay"}
						family.preciseCompileMemberships[node.ID] = []string{"base", "overlay"}
						return
					}
				}
				t.Fatal("exclusive family has no base compiler node")
			},
			want: "do not match root-derived memberships",
		},
		{
			name:  "consumer membership not closed over producer",
			build: sharedChainFamily,
			mutate: func(t *testing.T, family *ActionPlanFamily) {
				producerID := ""
				for _, node := range family.Nodes {
					if node.Kind == "compile" {
						producerID = node.ID
						break
					}
				}
				if producerID == "" {
					t.Fatal("shared chain family has no compiler producer")
				}
				family.Memberships[producerID] = []string{"base"}
				family.preciseCompileMemberships[producerID] = []string{"base"}
				family.Views = slices.DeleteFunc(family.Views, func(view ActionPlanFamilyView) bool {
					return view.Variant == "overlay" && view.NodeID == producerID
				})
			},
			want: "without membership",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			family := test.build(t)
			test.mutate(t, family)
			_, err := family.ReuseReport()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReuseReport() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestActionPlanFamilySeparatesPrivateArtifactLayoutsIndependentOfOrder(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestChainSnapshot(
			t, "same-recipe", ".linux-bzl-versions/base/drivers/example.o",
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestChainSnapshot(
			t, "same-recipe", ".linux-bzl-versions/overlay/drivers/example.o",
		),
	}
	wantPaths := map[string]string{
		"base":    ".linux-bzl-versions/base/drivers/example.o",
		"overlay": ".linux-bzl-versions/overlay/drivers/example.o",
	}
	baselineProducerIDs := map[string]string{}
	baselineConsumerIDs := map[string]string{}
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(family.Nodes), 4; got != want {
			t.Fatalf("permutation %d unique nodes = %d, want %d", permutation, got, want)
		}
		nodes := make(map[string]ActionPlanNode, len(family.Nodes))
		for _, node := range family.Nodes {
			nodes[node.ID] = node
			if memberships := family.Memberships[node.ID]; len(memberships) != 1 {
				t.Fatalf("permutation %d private-layout node %s memberships = %q", permutation, node.ID, memberships)
			}
		}
		for _, consumer := range family.Nodes {
			if len(consumer.Inputs) != 1 {
				continue
			}
			variant := family.Memberships[consumer.ID][0]
			producer := nodes[consumer.Inputs[0].ProducerID]
			wantPath := wantPaths[variant]
			if got := actionPlanOutputArtifactPath(producer.Outputs[0]); got != wantPath {
				t.Fatalf("permutation %d variant %s artifact path = %q, want %q", permutation, variant, got, wantPath)
			}
			bindings, err := familyNodeInputBindings(consumer, nodes)
			if err != nil {
				t.Fatal(err)
			}
			if got := bindings.Bindings["object:00000000"].ProjectionPath; got != wantPath {
				t.Fatalf("permutation %d variant %s projection path = %q, want %q", permutation, variant, got, wantPath)
			}
			if previous := baselineProducerIDs[variant]; previous != "" && previous != producer.ID {
				t.Fatalf("variant %s producer ID changed across permutations: %s != %s", variant, previous, producer.ID)
			}
			if previous := baselineConsumerIDs[variant]; previous != "" && previous != consumer.ID {
				t.Fatalf("variant %s consumer ID changed across permutations: %s != %s", variant, previous, consumer.ID)
			}
			baselineProducerIDs[variant] = producer.ID
			baselineConsumerIDs[variant] = consumer.ID
		}
		if baselineProducerIDs["base"] == baselineProducerIDs["overlay"] {
			t.Fatalf("permutation %d deduplicated producers with different private layouts as %s", permutation, baselineProducerIDs["base"])
		}
		if baselineConsumerIDs["base"] == baselineConsumerIDs["overlay"] {
			t.Fatalf("permutation %d deduplicated consumers with different projection paths as %s", permutation, baselineConsumerIDs["base"])
		}
	}
}

func TestActionPlanFamilyNormalizesSelectionArtifactAllocationsIndependentOfOrder(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestChainSnapshot(
			t, "same-recipe", familySelectionArtifactDirectory+"/"+strings.Repeat("a", 64)+"/drivers/example.o",
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestChainSnapshot(
			t, "same-recipe", familySelectionArtifactDirectory+"/"+strings.Repeat("b", 64)+"/drivers/example.o",
		),
	}
	var baselineIDs []string
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(family.Nodes), 2; got != want {
			t.Fatalf("permutation %d unique nodes = %d, want shared producer and consumer", permutation, got)
		}
		nodes := make(map[string]ActionPlanNode, len(family.Nodes))
		nodeIDs := make([]string, 0, len(family.Nodes))
		for _, node := range family.Nodes {
			nodes[node.ID] = node
			nodeIDs = append(nodeIDs, node.ID)
			if got, want := family.Memberships[node.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
				t.Fatalf("permutation %d node %s memberships = %q, want %q", permutation, node.ID, got, want)
			}
		}
		if permutation == 0 {
			baselineIDs = nodeIDs
		} else if !slices.Equal(nodeIDs, baselineIDs) {
			t.Fatalf("selection allocation normalization changed IDs across permutations: %q != %q", nodeIDs, baselineIDs)
		}
		for _, consumer := range family.Nodes {
			if len(consumer.Inputs) != 1 {
				continue
			}
			producer := nodes[consumer.Inputs[0].ProducerID]
			artifactPath := actionPlanOutputArtifactPath(producer.Outputs[0])
			if !strings.HasPrefix(artifactPath, familyOwnedArtifactDirectory+"/") {
				t.Fatalf("normalized selection artifact path = %q, want family-owned allocation", artifactPath)
			}
			bindings, err := familyNodeInputBindings(consumer, nodes)
			if err != nil {
				t.Fatal(err)
			}
			if got := bindings.Bindings["object:00000000"].ProjectionPath; got != artifactPath {
				t.Fatalf("normalized selection projection = %q, want %q", got, artifactPath)
			}
		}
	}
}

func TestActionPlanFamilyNormalizesPlannerOwnedPrivateArtifactsIndependentOfOrder(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
		tail   string
	}{
		{name: "intermediate", prefix: familyIntermediateArtifactDirectory, tail: "command-00000000.o"},
		{name: "side output", prefix: familySideOutputArtifactDirectory, tail: "drivers/example.o"},
		{name: "side output resolution", prefix: familySideOutputResolutionArtifactDirectory, tail: "drivers/example.o"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := ActionPlanFamilyVariant{
				Name: "base",
				Snapshot: familyTestChainSnapshot(t, "same-recipe", strings.Join([]string{
					test.prefix, strings.Repeat("a", 64), test.tail,
				}, "/")),
			}
			overlay := ActionPlanFamilyVariant{
				Name: "overlay",
				Snapshot: familyTestChainSnapshot(t, "same-recipe", strings.Join([]string{
					test.prefix, strings.Repeat("b", 64), test.tail,
				}, "/")),
			}

			var baselineNodeIDs []string
			for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
				family, err := BuildActionPlanFamily(variants)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := len(family.Nodes), 2; got != want {
					t.Fatalf("permutation %d unique nodes = %d, want %d", permutation, got, want)
				}
				nodes := make(map[string]ActionPlanNode, len(family.Nodes))
				nodeIDs := make([]string, 0, len(family.Nodes))
				for _, node := range family.Nodes {
					nodes[node.ID] = node
					nodeIDs = append(nodeIDs, node.ID)
					if got, want := family.Memberships[node.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
						t.Fatalf("permutation %d node %s memberships = %q, want %q", permutation, node.ID, got, want)
					}
				}
				if permutation == 0 {
					baselineNodeIDs = nodeIDs
				} else if !slices.Equal(nodeIDs, baselineNodeIDs) {
					t.Fatalf("node IDs changed across permutations: %q != %q", nodeIDs, baselineNodeIDs)
				}

				var producer, consumer ActionPlanNode
				for _, node := range family.Nodes {
					if len(node.Inputs) == 0 {
						producer = node
					} else {
						consumer = node
					}
				}
				wantProjection := actionPlanOutputArtifactPath(producer.Outputs[0])
				parts := strings.Split(wantProjection, "/")
				if len(parts) != 3 || parts[0] != familyOwnedArtifactDirectory || validatePlanDigest("private output allocation", parts[1]) != nil || parts[2] != planOrdinal(0) {
					t.Fatalf("permutation %d producer artifact path is not a family allocation: %q", permutation, wantProjection)
				}
				bindings, err := familyNodeInputBindings(consumer, nodes)
				if err != nil {
					t.Fatal(err)
				}
				if got := bindings.Bindings["object:00000000"].ProjectionPath; got != wantProjection {
					t.Fatalf("permutation %d consumer projection path = %q, want %q", permutation, got, wantProjection)
				}
			}
		})
	}
}

func TestActionPlanFamilyNormalizesPlannerOwnedObservedStateIndependentOfOrder(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base", Snapshot: familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), true),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay", Snapshot: familyTestObservedStateSnapshot(t, strings.Repeat("b", 64), true),
	}
	var baselineNodeIDs []string
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(family.Nodes), 2; got != want {
			t.Fatalf("permutation %d unique nodes = %d, want %d", permutation, got, want)
		}
		nodes := make(map[string]ActionPlanNode, len(family.Nodes))
		nodeIDs := make([]string, 0, len(family.Nodes))
		var state, consumer ActionPlanNode
		for _, node := range family.Nodes {
			nodes[node.ID] = node
			nodeIDs = append(nodeIDs, node.ID)
			if got, want := family.Memberships[node.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
				t.Fatalf("permutation %d node %s memberships = %q, want %q", permutation, node.ID, got, want)
			}
			if len(node.Outputs) == 1 && node.Outputs[0].ObservedPath != "" {
				state = node
			} else if len(node.Inputs) == 1 {
				consumer = node
			}
		}
		if permutation == 0 {
			baselineNodeIDs = nodeIDs
		} else if !slices.Equal(nodeIDs, baselineNodeIDs) {
			t.Fatalf("observed-state node IDs changed across permutations: %q != %q", nodeIDs, baselineNodeIDs)
		}
		if state.ID == "" || consumer.ID == "" {
			t.Fatalf("permutation %d family omits state or consumer: %#v", permutation, family.Nodes)
		}
		stateOutput := state.Outputs[0]
		if stateOutput.ObservedPath != "drivers/example.generated" || stateOutput.ArtifactPath != "" ||
			!strings.HasPrefix(stateOutput.Path, familyOwnedObservedStateDirectory+"/") {
			t.Fatalf("permutation %d relocated observed state = %#v", permutation, stateOutput)
		}
		bindings, err := familyNodeInputBindings(consumer, nodes)
		if err != nil {
			t.Fatal(err)
		}
		binding := bindings.Bindings["state:00000000"]
		if binding.ProjectionTree != "metadata" || binding.ProjectionPath != stateOutput.Path {
			t.Fatalf("permutation %d observed-state binding = %#v, want stable metadata projection %q", permutation, binding, stateOutput.Path)
		}
		for _, view := range family.Views {
			if view.Tree == "metadata" && view.ArtifactPath != stateOutput.Path {
				t.Fatalf("permutation %d metadata view = %#v, want relocated path %q", permutation, view, stateOutput.Path)
			}
		}
	}
}

func TestActionPlanFamilyDoesNotNormalizeLookalikeObservedStatePath(t *testing.T) {
	base := familyTestObservedStateSnapshot(t, strings.Repeat("a", 64), false)
	overlay := familyTestObservedStateSnapshot(t, strings.Repeat("b", 64), false)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 4; got != want {
		t.Fatalf("lookalike ordinary outputs reduced to %d nodes, want %d: %#v", got, want, family.Nodes)
	}
	for _, node := range family.Nodes {
		for _, output := range node.Outputs {
			if output.ObservedPath == "" && strings.HasPrefix(output.Path, familyOwnedObservedStateDirectory+"/") {
				t.Fatalf("ordinary output was relocated as observed state: %#v", output)
			}
		}
	}
}

func TestPlannerOwnedObservedFamilyStateRequiresExactAllocationShape(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		name   string
		output ActionPlanOutput
		want   bool
	}{
		{
			name: "candidate state",
			output: ActionPlanOutput{
				Path: path.Join(compactKbuildSideOutputStateDirectory, digest+".state"), ObservedPath: "generated.h",
			},
			want: true,
		},
		{
			name: "command state",
			output: ActionPlanOutput{
				Path: path.Join(compactKbuildSideOutputStateDirectory, "commands", digest+".state"), ObservedPath: "generated.h",
			},
			want: true,
		},
		{
			name: "ordinary output", output: ActionPlanOutput{
				Path: path.Join(compactKbuildSideOutputStateDirectory, digest+".state"),
			},
		},
		{
			name: "non digest", output: ActionPlanOutput{
				Path: path.Join(compactKbuildSideOutputStateDirectory, "named.state"), ObservedPath: "generated.h",
			},
		},
		{
			name: "nested lookalike", output: ActionPlanOutput{
				Path: path.Join(compactKbuildSideOutputStateDirectory, "other", digest+".state"), ObservedPath: "generated.h",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := plannerOwnedObservedFamilyState(test.output); got != test.want {
				t.Fatalf("plannerOwnedObservedFamilyState(%#v) = %t, want %t", test.output, got, test.want)
			}
		})
	}
}

func TestActionPlanFamilyDeduplicatesExactViewsFromSameVariantPrivateWriters(t *testing.T) {
	firstPath := path.Join(
		familyIntermediateArtifactDirectory,
		strings.Repeat("a", 64),
		"command-00000000.o",
	)
	snapshot := familyTestChainSnapshot(t, "same-recipe", firstPath)
	plan := snapshotActionPlan(snapshot)
	dependencies := make(map[string]ConfigDependencySet, len(snapshot.ConfigDependencies)+1)
	for id, set := range snapshot.ConfigDependencies {
		dependencies[id] = set
	}

	var duplicate ActionPlanNode
	for _, node := range plan.Nodes {
		if len(node.Inputs) != 0 {
			continue
		}
		duplicate = node
		duplicate.Outputs = slices.Clone(node.Outputs)
		duplicate.Outputs[0].ArtifactPath = path.Join(
			familyIntermediateArtifactDirectory,
			strings.Repeat("b", 64),
			"command-00000000.o",
		)
		duplicate.ID = duplicate.ContentID()
		dependencies[duplicate.ID] = snapshot.ConfigDependencies[node.ID]
		break
	}
	if duplicate.ID == "" {
		t.Fatal("test chain has no producer to duplicate")
	}
	plan.Nodes = append(plan.Nodes, duplicate)
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}

	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("unique nodes = %d, want %d", got, want)
	}
	producerViews := 0
	for _, view := range family.Views {
		if view.Tree == "objects" {
			producerViews++
		}
	}
	if got, want := producerViews, 1; got != want {
		t.Fatalf("object views = %d, want %d: %#v", got, want, family.Views)
	}
}

func familyCapsuleIDForVariant(t *testing.T, family *ActionPlanFamily, variant string) string {
	t.Helper()
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	for _, node := range family.Nodes {
		if !slices.Contains(family.Memberships[node.ID], variant) {
			continue
		}
		for _, edge := range node.Sources {
			source := sources[edge.SourceID]
			if source.Namespace != "capsule" {
				continue
			}
			digest, _, ok := strings.Cut(source.Path, "/")
			if !ok {
				t.Fatalf("variant %s capsule source path = %q", variant, source.Path)
			}
			return digest
		}
	}
	t.Fatalf("variant %s has no capsule source", variant)
	return ""
}

func TestActionPlanFamilyUnionsMemberDependencySetsIndependentOfOrder(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestSnapshot(
			t, 1, "drivers/example.o", familyTestConfig("y", "n"),
			ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestSnapshot(
			t, 27, "drivers/example.o", familyTestConfig("n", "m"),
			ConfigDependencySet{Symbols: []string{"CONFIG_OTHER"}},
		),
	}
	union := ConfigDependencySet{Symbols: []string{"CONFIG_OTHER", "CONFIG_USED"}}
	wantCapsules := map[string]string{}
	for _, variant := range []ActionPlanFamilyVariant{base, overlay} {
		capsule, err := RenderConfigCapsule(variant.Snapshot.ConfigFiles, union)
		if err != nil {
			t.Fatal(err)
		}
		wantCapsules[variant.Name] = capsule.ID
	}
	baselineNodeIDs := []string{}
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		for _, variant := range []string{"base", "overlay"} {
			if got, want := familyCapsuleIDForVariant(t, family, variant), wantCapsules[variant]; got != want {
				t.Fatalf("permutation %d variant %s capsule = %s, want {CONFIG_OTHER,CONFIG_USED} capsule %s", permutation, variant, got, want)
			}
		}
		nodeIDs := make([]string, 0, len(family.Nodes))
		for _, node := range family.Nodes {
			nodeIDs = append(nodeIDs, node.ID)
		}
		if permutation == 0 {
			baselineNodeIDs = nodeIDs
		} else if !slices.Equal(nodeIDs, baselineNodeIDs) {
			t.Fatalf("family node IDs changed across permutations: %q != %q", nodeIDs, baselineNodeIDs)
		}
	}
}

func TestActionPlanFamilyIsolatesOpaqueMemberIndependentOfOrder(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestSnapshot(
			t, 1, "drivers/example.o", familyTestConfig("y", "n"),
			ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestSnapshot(
			t, 27, "drivers/example.o", familyTestConfig("y", "m"),
			ConfigDependencySet{Opaque: true, Reason: "generated input cannot be inspected"},
		),
	}
	wantCapsules := map[string]string{}
	baseNode := base.Snapshot.Nodes[0]
	baseCapsule, err := RenderConfigCapsule(
		base.Snapshot.ConfigFiles, base.Snapshot.ConfigDependencies[baseNode.ID],
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCapsules[base.Name] = baseCapsule.ID
	overlayCapsule, err := RenderConfigCapsule(
		overlay.Snapshot.ConfigFiles,
		ConfigDependencySet{Opaque: true, Reason: "full configuration expected"},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCapsules[overlay.Name] = overlayCapsule.ID
	baselineNodeIDs := []string{}
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(family.Nodes), 2; got != want {
			t.Fatalf("permutation %d unique nodes = %d, want %d isolated nodes", permutation, got, want)
		}
		for _, variant := range []string{"base", "overlay"} {
			if got, want := familyCapsuleIDForVariant(t, family, variant), wantCapsules[variant]; got != want {
				t.Fatalf("permutation %d variant %s capsule = %s, want isolated capsule %s", permutation, variant, got, want)
			}
		}
		nodeIDs := make([]string, 0, len(family.Nodes))
		for _, node := range family.Nodes {
			nodeIDs = append(nodeIDs, node.ID)
		}
		if permutation == 0 {
			baselineNodeIDs = nodeIDs
		} else if !slices.Equal(nodeIDs, baselineNodeIDs) {
			t.Fatalf("opaque family node IDs changed across permutations: %q != %q", nodeIDs, baselineNodeIDs)
		}
	}
}

func TestActionPlanFamilyAddingOpaqueVariantPreservesPreciseIDsAndReuse(t *testing.T) {
	preciseConfig := familyTestConfig("y", "n")
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestSnapshot(
			t, 1, "", preciseConfig,
			ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
		),
	}
	peer := ActionPlanFamilyVariant{
		Name: "peer",
		Snapshot: familyTestSnapshot(
			t, 27, "", preciseConfig,
			ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
		),
	}
	opaque := ActionPlanFamilyVariant{
		Name: "opaque",
		Snapshot: familyTestSnapshot(
			t, 53, "", familyTestConfig("y", "m"),
			ConfigDependencySet{Opaque: true, Reason: "generated input cannot be inspected"},
		),
	}
	baseline, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{base, peer})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(baseline.Nodes), 1; got != want {
		t.Fatalf("precise baseline nodes = %d, want %d", got, want)
	}
	baselineID := baseline.Nodes[0].ID
	if got, want := baseline.Memberships[baselineID], []string{"base", "peer"}; !slices.Equal(got, want) {
		t.Fatalf("precise baseline memberships = %q, want %q", got, want)
	}
	baselineCapsule := familyCapsuleIDForVariant(t, baseline, "base")

	for permutation, variants := range [][]ActionPlanFamilyVariant{
		{base, peer, opaque},
		{opaque, base, peer},
		{peer, opaque, base},
	} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		memberships, ok := family.Memberships[baselineID]
		if !ok {
			t.Fatalf("permutation %d dropped precise semantic node %s", permutation, baselineID)
		}
		if want := []string{"base", "peer"}; !slices.Equal(memberships, want) {
			t.Fatalf("permutation %d precise memberships = %q, want %q", permutation, memberships, want)
		}
		if got := familyCapsuleIDForVariant(t, family, "base"); got != baselineCapsule {
			t.Fatalf("permutation %d precise capsule = %s, want baseline %s", permutation, got, baselineCapsule)
		}
		if got := familyCapsuleIDForVariant(t, family, "peer"); got != baselineCapsule {
			t.Fatalf("permutation %d peer capsule = %s, want baseline %s", permutation, got, baselineCapsule)
		}
		if slices.Contains(family.Memberships[baselineID], "opaque") {
			t.Fatalf("permutation %d opaque variant reused precise node %s", permutation, baselineID)
		}
	}
}

func TestActionPlanFamilySharesOpaqueMembersWithIdenticalFullExecutionIdentity(t *testing.T) {
	config := familyTestConfig("y", "m")
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestSnapshot(
			t, 1, "", config,
			ConfigDependencySet{Opaque: true, Reason: "dynamic include closure"},
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestSnapshot(
			t, 27, "", config,
			ConfigDependencySet{Opaque: true, Reason: "uninspectable generated input"},
		),
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{overlay, base})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 1; got != want {
		t.Fatalf("identical opaque execution nodes = %d, want %d", got, want)
	}
	if got, want := family.Memberships[family.Nodes[0].ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("identical opaque memberships = %q, want %q", got, want)
	}
}

func familyOpaquePrepReaderSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "generate",
		Tool:   compactKbuildScriptRunnerRole,
		Arguments: []string{
			"-script_content_base64", "dHJ1ZQo=",
		},
		WorkingDirectory: "opaque-prep-reader",
		WorkingTrees:     []string{"prep"},
		WorkingOutputs:   map[string]string{"00000000": "drivers/opaque.generated"},
		Outputs:          []string{"00000000"},
		Trees:            []string{"prep"},
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes:  map[string]ActionRecipe{recipeID: recipe},
		Nodes: []ActionPlanNode{{
			ID: "opaque-prep-reader", Stage: "target", Kind: "generate", Recipe: recipeID,
			Tool: compactKbuildScriptRunnerRole, Product: "image", Trees: []string{"prep"},
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/opaque.generated"}},
		}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{
		plan.Nodes[0].ID: {Opaque: true, Reason: "source script may inspect staged prep tree"},
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyOpaqueSharedRecipeSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	snapshot := familyOpaquePrepReaderSnapshot(t, config)
	plan := snapshotActionPlan(snapshot)
	if len(plan.Nodes) != 1 {
		t.Fatalf("opaque reader fixture has %d nodes, want one", len(plan.Nodes))
	}
	// Keep a non-nil shared map in the interned recipe. A shallow recipe copy
	// aliases this map even though the empty map is omitted from its content ID,
	// so the first consumer's ambient bindings would poison the second consumer.
	sharedRecipe := cloneActionRecipe(plan.Recipes[plan.Nodes[0].Recipe])
	sharedRecipe.WorkingInputs = map[string]string{}
	plan.Recipes[plan.Nodes[0].Recipe] = sharedRecipe
	peer := plan.Nodes[0]
	peer.Outputs = slices.Clone(peer.Outputs)
	peer.Outputs[0].Path = "drivers/opaque-peer.generated"
	peer.ID = peer.ContentID()
	plan.Nodes = append(plan.Nodes, peer)
	dependencies := map[string]ConfigDependencySet{}
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "source script may inspect staged prep tree"}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func familyCanonicalFallbackOpaqueSnapshot(t *testing.T, config map[string]string) ActionPlanSnapshot {
	t.Helper()
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	copyRecipeID, err := copyRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "opaque-fallback-consumer",
		WorkingInputs:    map[string]string{},
		WorkingTrees:     []string{"prep"},
		Sources:          []string{"source:00000000"},
		Trees:            []string{"prep"},
		Outputs:          []string{"00000000"},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes:  map[string]ActionRecipe{copyRecipeID: copyRecipe},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	producerIDs := make([]string, 0, len(resolvedConfigProjections()))
	for index, projection := range resolvedConfigProjections() {
		sourceID := "src-" + planOrdinal(index+1)
		producerID := "config-projection-" + planOrdinal(index)
		plan.Sources = append(plan.Sources, ActionPlanSource{
			ID: sourceID, Namespace: "config", Path: projection.input,
		})
		plan.Nodes = append(plan.Nodes, ActionPlanNode{
			ID: producerID, Stage: "prep", Kind: "copy", Recipe: copyRecipeID,
			Tool: "actionfile", Product: "sdk",
			Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: sourceID}},
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: projection.output}},
		})
		producerIDs = append(producerIDs, producerID)
		binding := "config:" + planOrdinal(index)
		consumerRecipe.Inputs = append(consumerRecipe.Inputs, binding)
		consumerRecipe.WorkingInputs["input:"+binding] = projection.output
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan.Recipes[consumerRecipeID] = consumerRecipe
	kernelSourceID := "src-" + planOrdinal(len(resolvedConfigProjections())+1)
	plan.Sources = append(plan.Sources, ActionPlanSource{
		ID: kernelSourceID, Namespace: "kernel", Path: "drivers/opaque.c",
	})
	consumer := ActionPlanNode{
		ID: "opaque-consumer", Stage: "target", Kind: "compile", Recipe: consumerRecipeID,
		Tool: "cc", Product: "image",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: kernelSourceID}},
		Trees:   []string{"prep"},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/opaque.o"}},
	}
	for _, producerID := range producerIDs {
		consumer.Inputs = append(consumer.Inputs, ActionPlanNodeEdge{
			Role: "config", ProducerID: producerID, Slot: 0,
		})
	}
	plan.Nodes = append(plan.Nodes, consumer)
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "opaque canonical-fallback consumer"}
		} else {
			// Canonical fallback projections must remain full even when their
			// local dependency classifier supplies no selected symbols.
			dependencies[node.ID] = ConfigDependencySet{}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestActionPlanFamilyStagesFullConfigForOpaquePrepReader(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyOpaquePrepReaderSnapshot(t, familyTestConfig("y", "n"))},
		{Name: "overlay", Snapshot: familyOpaquePrepReaderSnapshot(t, familyTestConfig("y", "m"))},
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("opaque prep-reader nodes = %d, want %d variant-specific nodes", got, want)
	}
	if got, want := len(family.Capsules), 2; got != want {
		t.Fatalf("opaque prep-reader capsules = %d, want %d full-config capsules", got, want)
	}
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	for _, node := range family.Nodes {
		members := family.Memberships[node.ID]
		if len(members) != 1 {
			t.Fatalf("opaque prep-reader node %s memberships = %q, want one variant", node.ID, members)
		}
		variant := members[0]
		var snapshot ActionPlanSnapshot
		for _, candidate := range variants {
			if candidate.Name == variant {
				snapshot = candidate.Snapshot
				break
			}
		}
		capsule, err := RenderConfigCapsule(
			snapshot.ConfigFiles,
			ConfigDependencySet{Opaque: true, Reason: "full config expected"},
		)
		if err != nil {
			t.Fatal(err)
		}
		recipe := family.Recipes[node.Recipe]
		if got, want := len(node.Sources), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("variant %s ambient config sources = %d, want %d", variant, got, want)
		}
		if got, want := len(recipe.WorkingInputs), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("variant %s ambient config working inputs = %d, want %d", variant, got, want)
		}
		seenPaths := map[string]bool{}
		for ordinal, edge := range node.Sources {
			source := sources[edge.SourceID]
			if edge.Role != "ambient-config" || source.Namespace != "capsule" ||
				!strings.HasPrefix(source.Path, capsule.ID+"/") {
				t.Fatalf("variant %s ambient source %d = %#v/%#v", variant, ordinal, edge, source)
			}
			binding := edge.Role + ":" + planOrdinal(ordinal)
			pathname := recipe.WorkingInputs["source:"+binding]
			if pathname == "" {
				t.Fatalf("variant %s ambient source %d has no working input", variant, ordinal)
			}
			if got, want := source.Path, path.Join(capsule.ID, pathname); got != want {
				t.Fatalf("variant %s ambient source path = %q, want %q", variant, got, want)
			}
			seenPaths[pathname] = true
		}
		for _, projection := range ResolvedConfigProjectionOutputs() {
			if !seenPaths[projection] {
				t.Errorf("variant %s did not stage full-config projection %q", variant, projection)
			}
		}
	}
}

func TestActionPlanFamilyStagesFullConfigForEveryOpaqueSharedRecipeConsumer(t *testing.T) {
	config := familyTestConfig("y", "n")
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyOpaqueSharedRecipeSnapshot(t, config),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("opaque shared-recipe nodes = %d, want %d", got, want)
	}
	capsule, err := RenderConfigCapsule(config, ConfigDependencySet{Opaque: true, Reason: "full config expected"})
	if err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	recipeID := ""
	for _, node := range family.Nodes {
		if recipeID == "" {
			recipeID = node.Recipe
		} else if node.Recipe != recipeID {
			t.Fatalf("opaque consumers do not share their localized recipe: %s != %s", node.Recipe, recipeID)
		}
		recipe := family.Recipes[node.Recipe]
		if got, want := len(node.Sources), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("opaque consumer %s ambient sources = %d, want %d", node.ID, got, want)
		}
		if got, want := len(recipe.WorkingInputs), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("opaque consumer %s working inputs = %d, want %d", node.ID, got, want)
		}
		for ordinal, edge := range node.Sources {
			binding := edge.Role + ":" + planOrdinal(ordinal)
			projection := recipe.WorkingInputs["source:"+binding]
			source := sources[edge.SourceID]
			if edge.Role != "ambient-config" || projection == "" || source.Namespace != "capsule" ||
				source.Path != path.Join(capsule.ID, projection) {
				t.Fatalf("opaque consumer %s source %d = %#v, projection %q, descriptor %#v", node.ID, ordinal, edge, projection, source)
			}
		}
	}
}

func TestActionPlanFamilyOpaqueCanonicalFallbackInputsUseFullCapsuleProducers(t *testing.T) {
	configs := map[string]map[string]string{
		"base":    familyTestConfig("y", "n"),
		"overlay": familyTestConfig("y", "m"),
	}
	variants := make([]ActionPlanFamilyVariant, 0, len(configs))
	for _, variant := range []string{"base", "overlay"} {
		variants = append(variants, ActionPlanFamilyVariant{
			Name: variant, Snapshot: familyCanonicalFallbackOpaqueSnapshot(t, configs[variant]),
		})
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Capsules), len(variants); got != want {
		t.Fatalf("opaque fallback capsules = %d, want %d full variant capsules", got, want)
	}
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		nodes[node.ID] = node
	}
	consumers := 0
	for _, node := range family.Nodes {
		if node.Kind != "compile" {
			continue
		}
		consumers++
		members := family.Memberships[node.ID]
		if len(members) != 1 {
			t.Fatalf("opaque fallback consumer %s memberships = %q, want one variant", node.ID, members)
		}
		variant := members[0]
		capsule, err := RenderConfigCapsule(
			configs[variant],
			ConfigDependencySet{Opaque: true, Reason: "full config expected"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := family.Capsules[capsule.ID]; !maps.Equal(got, configs[variant]) {
			t.Fatalf("variant %s capsule = %#v, want exact full config", variant, got)
		}
		recipe := family.Recipes[node.Recipe]
		if got, want := len(node.Inputs), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("variant %s fallback inputs = %d, want %d", variant, got, want)
		}
		if got, want := len(node.Sources), 1; got != want {
			t.Fatalf("variant %s sources = %d, want only the kernel source", variant, got)
		}
		if got, want := len(recipe.WorkingInputs), len(ResolvedConfigProjectionOutputs()); got != want {
			t.Fatalf("variant %s fallback working inputs = %d, want %d", variant, got, want)
		}
		seen := map[string]bool{}
		for ordinal, edge := range node.Inputs {
			projection := recipe.WorkingInputs["input:"+edge.Role+":"+planOrdinal(ordinal)]
			if projection == "" {
				t.Fatalf("variant %s fallback input %d has no staged projection", variant, ordinal)
			}
			producer, ok := nodes[edge.ProducerID]
			if !ok || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
				t.Fatalf("variant %s fallback input %d has invalid producer edge %#v", variant, ordinal, edge)
			}
			if got := producer.Outputs[edge.Slot].Path; got != projection {
				t.Fatalf("variant %s fallback producer output = %q, want staged projection %q", variant, got, projection)
			}
			if got, want := family.Memberships[producer.ID], []string{variant}; !slices.Equal(got, want) {
				t.Fatalf("variant %s fallback producer %s memberships = %q, want %q", variant, producer.ID, got, want)
			}
			if got, want := len(producer.Sources), 1; got != want {
				t.Fatalf("variant %s fallback producer %s sources = %d, want %d", variant, producer.ID, got, want)
			}
			source := sources[producer.Sources[0].SourceID]
			if source.Namespace != "capsule" || source.Path != path.Join(capsule.ID, projection) {
				t.Fatalf("variant %s fallback producer %s source = %#v, want full capsule %s/%s", variant, producer.ID, source, capsule.ID, projection)
			}
			seen[projection] = true
		}
		for _, projection := range ResolvedConfigProjectionOutputs() {
			if !seen[projection] {
				t.Errorf("variant %s did not stage full-capsule producer projection %q", variant, projection)
			}
		}
	}
	if got, want := consumers, len(variants); got != want {
		t.Fatalf("opaque fallback consumers = %d, want %d", got, want)
	}
}

func TestActionPlanFamilySeparatesRelevantConfigValues(t *testing.T) {
	base := familyTestSnapshot(t, 1, "drivers/example.o", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	overlay := familyTestSnapshot(t, 1, "drivers/example.o", familyTestConfig("n", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base},
		{Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("unique nodes = %d, want %d", got, want)
	}
	if got, want := len(family.Capsules), 2; got != want {
		t.Fatalf("capsules = %d, want %d", got, want)
	}
	for _, node := range family.Nodes {
		if len(family.Memberships[node.ID]) != 1 {
			t.Fatalf("config-specific node %s memberships = %q", node.ID, family.Memberships[node.ID])
		}
	}
}

func familySegmentEntrySignature(entries []actionPlanEntry) string {
	var signature strings.Builder
	for _, entry := range entries {
		signature.WriteString(entry.path)
		signature.WriteByte(0)
		signature.Write(entry.data)
		signature.WriteByte(0)
	}
	return signature.String()
}

func TestActionPlanFamilyMapsAndSegmentsPersistentInputSets(t *testing.T) {
	left := familyTestInputSetSnapshot(t, 1)
	right := familyTestInputSetSnapshot(t, 9)
	var leftConsumer, rightConsumer ActionPlanNode
	for _, node := range left.Nodes {
		if node.InputSet != "" {
			leftConsumer = node
		}
	}
	for _, node := range right.Nodes {
		if node.InputSet != "" {
			rightConsumer = node
		}
	}
	if leftConsumer.ID == rightConsumer.ID {
		t.Fatal("standalone consumers unexpectedly share an ID before source-leaf localization")
	}

	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "left", Snapshot: left}, {Name: "right", Snapshot: right},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(family.Nodes) != 2 {
		t.Fatalf("family nodes = %d, want one producer and one consumer", len(family.Nodes))
	}
	var producer, consumer ActionPlanNode
	for _, node := range family.Nodes {
		if node.InputSet == "" {
			producer = node
		} else {
			consumer = node
		}
		if !slices.Equal(family.Memberships[node.ID], []string{"left", "right"}) {
			t.Fatalf("node %s memberships = %v, want both variants", node.ID, family.Memberships[node.ID])
		}
	}
	if producer.ID == "" || consumer.ID == "" {
		t.Fatalf("family producer/consumer = %#v / %#v", producer, consumer)
	}
	store, err := NewActionPlanInputSetStoreFromNodes(family.InputSets)
	if err != nil {
		t.Fatal(err)
	}
	var sourceLeaf, producerLeaf ActionPlanInputSetEntry
	if err := store.Walk(consumer.InputSet, func(entry ActionPlanInputSetEntry) error {
		if entry.SourceID != "" {
			sourceLeaf = entry
		} else {
			producerLeaf = entry
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if producerLeaf.ProducerID != producer.ID || producerLeaf.Slot != 0 {
		t.Fatalf("family input-set producer leaf = %#v, want %s[0]", producerLeaf, producer.ID)
	}
	wantSourceID := semanticFamilySourceID("kernel", "include/seed.h")
	if sourceLeaf.SourceID != wantSourceID {
		t.Fatalf("family input-set source leaf = %q, want %q", sourceLeaf.SourceID, wantSourceID)
	}

	entries, err := family.entries()
	if err != nil {
		t.Fatal(err)
	}
	markers := map[string]bool{}
	for _, entry := range entries {
		markers[entry.path] = true
	}
	for _, marker := range []string{
		path.Join("nodes", consumer.Stage, consumer.ID, "in", "input-set", consumer.InputSet),
		path.Join("input-sets", consumer.InputSet, "manifest", consumer.InputSet+".json"),
		path.Join("sources", wantSourceID, "kernel", "include/seed.h"),
	} {
		if !markers[marker] {
			t.Errorf("family entries omit %q", marker)
		}
	}

	segments, err := family.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	targetMarkers := map[string]bool{}
	for _, entry := range segments["target"] {
		targetMarkers[entry.path] = true
	}
	closure, err := store.ReachableNodes(consumer.InputSet)
	if err != nil {
		t.Fatal(err)
	}
	for id := range closure {
		if !targetMarkers[path.Join("input-sets", id, "manifest", id+".json")] {
			t.Errorf("target segment omits reachable input-set node %s", id)
		}
	}
	descriptor, err := familyPriorOutputMarker(
		func() map[string]int {
			ordinals, _, ordinalErr := actionPlanNodeOrdinalIndex(family.Nodes)
			if ordinalErr != nil {
				t.Fatal(ordinalErr)
			}
			return ordinals
		}(),
		producer, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !targetMarkers[descriptor.path] {
		t.Errorf("target segment omits input-set prior descriptor %q", descriptor.path)
	}
	for _, entry := range segments["prehost"] {
		if strings.HasPrefix(entry.path, "input-sets/") {
			t.Errorf("prehost segment retains target-only input-set marker %q", entry.path)
		}
	}
}

func TestActionPlanFamilyRejectsUnreachablePersistentInputSetNode(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyTestInputSetSnapshot(t, 1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewActionPlanInputSetStore()
	unreachableRoot, err := store.Insert("", ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "unreachable"},
		SourceID: semanticFamilySourceID("kernel", "include/seed.h"),
	})
	if err != nil {
		t.Fatal(err)
	}
	unreachable, err := store.ReachableNodes(unreachableRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeActionPlanFamilyInputSetNodes(family.InputSets, unreachable); err != nil {
		t.Fatal(err)
	}
	if err := family.validate(); err == nil || !strings.Contains(err.Error(), "input-set nodes, but") {
		t.Fatalf("family validation error = %v, want unreachable input-set rejection", err)
	}
}

func TestActionPlanFamilySegmentsAreClosedAndFiltered(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyTestSegmentChainSnapshot(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Retain valid but unreachable metadata in the in-memory family to prove
	// that sharding is reference-filtered rather than merely stage-filtered.
	unusedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	unusedRecipeID, err := unusedRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	family.Recipes[unusedRecipeID] = unusedRecipe
	unusedSource := ActionPlanSource{
		ID: semanticFamilySourceID("kernel", "unused"), Namespace: "kernel", Path: "unused",
	}
	family.Sources = append(family.Sources, unusedSource)
	segments, err := family.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	nodeOrdinals, _, err := actionPlanNodeOrdinalIndex(family.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		nodes[node.ID] = node
	}

	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		markers := map[string]bool{}
		indexMarkers := 0
		nodeIDs := map[string]bool{}
		priorMarkers := map[string]bool{}
		hasVariantMetadata := false
		for _, entry := range segments[segment] {
			markers[entry.path] = true
			parts := strings.Split(entry.path, "/")
			switch parts[0] {
			case "index":
				indexMarkers++
			case "nodes":
				nodeIDs[parts[2]] = true
				wantSegment, ok := familyPlanSegmentForStage(parts[1])
				if !ok || wantSegment != segment {
					t.Fatalf("%s shard contains %s node marker %q", segment, wantSegment, entry.path)
				}
			case "prior":
				priorMarkers[entry.path] = true
			case "variants":
				hasVariantMetadata = true
			}
		}
		if !markers["schema/"+LinuxKernelFamilyPlanSchema] {
			t.Errorf("%s shard omits v6 schema", segment)
		}
		if indexMarkers != len(family.Nodes) {
			t.Errorf("%s shard index entries = %d, want %d", segment, indexMarkers, len(family.Nodes))
		}
		for scope, identity := range family.Toolsets {
			if !markers[path.Join("toolsets", scope, identity)] {
				t.Errorf("%s shard omits %s toolset identity", segment, scope)
			}
		}
		if hasVariantMetadata != (segment == "target") {
			t.Errorf("%s shard variant metadata presence = %t", segment, hasVariantMetadata)
		}

		wantNodes := map[string]bool{}
		wantRecipes := map[string]bool{}
		wantSources := map[string]bool{}
		wantPrior := map[string]bool{}
		for _, node := range family.Nodes {
			nodeSegment, _ := familyPlanSegmentForStage(node.Stage)
			if nodeSegment != segment {
				continue
			}
			wantNodes[node.ID] = true
			wantRecipes[node.Recipe] = true
			for _, source := range node.Sources {
				wantSources[source.SourceID] = true
			}
			for _, input := range node.Inputs {
				producer := nodes[input.ProducerID]
				producerSegment, _ := familyPlanSegmentForStage(producer.Stage)
				if producerSegment == segment {
					continue
				}
				descriptor, err := familyPriorOutputMarker(nodeOrdinals, producer, input.Slot)
				if err != nil {
					t.Fatal(err)
				}
				wantPrior[descriptor.path] = true
			}
		}
		if !maps.Equal(nodeIDs, wantNodes) {
			t.Errorf("%s shard nodes = %v, want %v", segment, nodeIDs, wantNodes)
		}
		if !maps.Equal(priorMarkers, wantPrior) {
			t.Errorf("%s shard prior descriptors = %v, want %v", segment, priorMarkers, wantPrior)
		}

		gotRecipes := map[string]bool{}
		gotSources := map[string]bool{}
		for marker := range markers {
			parts := strings.Split(marker, "/")
			if parts[0] == "recipes" {
				gotRecipes[strings.TrimSuffix(parts[1], ".json")] = true
			}
			if parts[0] == "sources" {
				gotSources[parts[1]] = true
			}
		}
		if !maps.Equal(gotRecipes, wantRecipes) {
			t.Errorf("%s shard recipes = %v, want %v", segment, gotRecipes, wantRecipes)
		}
		if !maps.Equal(gotSources, wantSources) {
			t.Errorf("%s shard sources = %v, want %v", segment, gotSources, wantSources)
		}
		if markers[path.Join("recipes", unusedRecipeID+".json")] || markers[path.Join("sources", unusedSource.ID, "kernel", "unused")] {
			t.Errorf("%s shard retains unreachable recipe/source metadata", segment)
		}
	}

	// Prep and target share a segment, so their direct edge stays local. The
	// only target-shard prior descriptor is the host output consumed by prep.
	priorCount := 0
	for _, entry := range segments["target"] {
		if strings.HasPrefix(entry.path, "prior/") {
			priorCount++
		}
	}
	if priorCount != 1 {
		t.Fatalf("target shard prior descriptors = %d, want one host-to-prep handoff", priorCount)
	}
}

func TestActionPlanFamilySegmentsAreDeterministicAcrossVariantOrder(t *testing.T) {
	snapshot := familyTestSegmentChainSnapshot(t)
	var baseline map[string]string
	for permutation, variants := range [][]ActionPlanFamilyVariant{
		{{Name: "base", Snapshot: snapshot}, {Name: "overlay", Snapshot: snapshot}},
		{{Name: "overlay", Snapshot: snapshot}, {Name: "base", Snapshot: snapshot}},
	} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		segments, err := family.segmentEntries()
		if err != nil {
			t.Fatal(err)
		}
		signatures := map[string]string{}
		for _, segment := range linuxKernelFamilyPlanSegmentOrder {
			signatures[segment] = familySegmentEntrySignature(segments[segment])
		}
		if permutation == 0 {
			baseline = signatures
		} else if !maps.Equal(signatures, baseline) {
			t.Fatal("family segment marker trees depend on variant flag order")
		}
	}
}

func TestActionPlanFamilySegmentsStageOnlyReferencedCapsuleBytes(t *testing.T) {
	snapshot := familyTestSnapshot(
		t, 1, "drivers/example.o", familyTestConfig("y", "n"),
		ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
	)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	segments, err := family.segmentEntries()
	if err != nil {
		t.Fatal(err)
	}
	referencedCapsules := map[string]bool{}
	sources := map[string]ActionPlanSource{}
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	for _, node := range family.Nodes {
		for _, edge := range node.Sources {
			source := sources[edge.SourceID]
			if source.Namespace == "capsule" {
				referencedCapsules[source.Path] = true
			}
		}
	}
	if len(referencedCapsules) == 0 {
		t.Fatal("test family has no config-capsule source")
	}
	for _, segment := range []string{"prehost", "bootstrap", "host"} {
		for _, entry := range segments[segment] {
			if strings.HasPrefix(entry.path, "capsules/") {
				t.Fatalf("%s shard retains unreferenced capsule byte %q", segment, entry.path)
			}
		}
	}
	gotCapsules := map[string]bool{}
	for _, entry := range segments["target"] {
		if strings.HasPrefix(entry.path, "capsules/") {
			gotCapsules[strings.TrimPrefix(entry.path, "capsules/")] = true
		}
	}
	if !maps.Equal(gotCapsules, referencedCapsules) {
		t.Fatalf("target capsule markers = %v, want exact referenced bytes %v", gotCapsules, referencedCapsules)
	}
	allCapsuleBytes := 0
	for _, files := range family.Capsules {
		allCapsuleBytes += len(files)
	}
	if len(gotCapsules) >= allCapsuleBytes {
		t.Fatalf("target shard retained %d/%d capsule bytes; test expected strict filtering", len(gotCapsules), allCapsuleBytes)
	}
}

func TestActionPlanFamilyWriteSegmentsRejectsAdversarialOutputs(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyTestSegmentChainSnapshot(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	valid := map[string]string{
		"prehost": filepath.Join(root, "prehost"), "bootstrap": filepath.Join(root, "bootstrap"),
		"host": filepath.Join(root, "host"), "target": filepath.Join(root, "target"),
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing": func(outputs map[string]string) { delete(outputs, "host") },
		"unknown": func(outputs map[string]string) {
			delete(outputs, "host")
			outputs["later"] = filepath.Join(root, "later")
		},
		"alias": func(outputs map[string]string) { outputs["host"] = outputs["bootstrap"] },
	} {
		t.Run(name, func(t *testing.T) {
			outputs := maps.Clone(valid)
			mutate(outputs)
			if err := family.WriteSegments(outputs); err == nil {
				t.Fatal("adversarial segment outputs succeeded")
			}
		})
	}

	nodes := map[string]int{}
	producer := family.Nodes[0]
	if _, err := familyPriorOutputMarker(nodes, producer, 0); err == nil {
		t.Fatal("unindexed prior producer succeeded")
	}
	nodes[producer.ID] = 0
	if _, err := familyPriorOutputMarker(nodes, producer, len(producer.Outputs)); err == nil {
		t.Fatal("out-of-range prior producer slot succeeded")
	}
}

func TestActionPlanFamilyWriteSegmentsPublishesAllFourShards(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyTestSegmentChainSnapshot(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	outputs := map[string]string{}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		outputs[segment] = filepath.Join(root, segment)
	}
	if err := family.WriteSegments(outputs); err != nil {
		t.Fatal(err)
	}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		if _, err := os.Stat(filepath.Join(outputs[segment], "schema", LinuxKernelFamilyPlanSchema)); err != nil {
			t.Errorf("%s segment schema: %v", segment, err)
		}
	}
}

func actionPlanFamilySegmentOutputsForTest(root string) map[string]string {
	outputs := make(map[string]string, len(linuxKernelFamilyPlanSegmentOrder))
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		outputs[segment] = filepath.Join(root, segment)
	}
	return outputs
}

func readActionPlanFamilyVariantForTest(
	t *testing.T,
	name string,
	snapshot ActionPlanSnapshot,
) ValidatedActionPlanFamilyVariant {
	t.Helper()
	filename := filepath.Join(t.TempDir(), name+".snapshot.json.gz")
	if err := WriteActionPlanSnapshot(
		filename,
		snapshotActionPlan(snapshot),
		snapshot.ConfigDependencies,
		snapshot.ConfigFiles,
	); err != nil {
		t.Fatal(err)
	}
	variant, err := ReadActionPlanFamilyVariant(name, filename)
	if err != nil {
		t.Fatal(err)
	}
	return variant
}

func TestBuildAndWriteActionPlanFamilyMatchesStandaloneEmission(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{
			Name: "base",
			Snapshot: familyTestSnapshot(
				t, 1, "drivers/example.o", familyTestConfig("y", "n"),
				ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
			),
		},
		{
			Name: "irrelevant",
			Snapshot: familyTestSnapshot(
				t, 1, "drivers/example.o", familyTestConfig("y", "y"),
				ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
			),
		},
	}

	standaloneRoot := filepath.Join(t.TempDir(), "standalone")
	standaloneOutputs := actionPlanFamilySegmentOutputsForTest(standaloneRoot)
	standaloneReport := filepath.Join(standaloneRoot, "reuse.json")
	standalone, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.WriteSegments(standaloneOutputs); err != nil {
		t.Fatal(err)
	}
	if err := standalone.WriteReuseReport(standaloneReport); err != nil {
		t.Fatal(err)
	}

	combinedRoot := filepath.Join(t.TempDir(), "combined")
	combinedOutputs := actionPlanFamilySegmentOutputsForTest(combinedRoot)
	combinedReport := filepath.Join(combinedRoot, "reuse.json")
	validatedVariants := make([]ValidatedActionPlanFamilyVariant, 0, len(variants))
	for _, variant := range variants {
		validatedVariants = append(validatedVariants, readActionPlanFamilyVariantForTest(
			t, variant.Name, variant.Snapshot,
		))
	}
	if err := BuildAndWriteActionPlanFamily(validatedVariants, combinedOutputs, combinedReport); err != nil {
		t.Fatal(err)
	}

	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		got, err := readActionPlanFilesForTest(combinedOutputs[segment])
		if err != nil {
			t.Fatal(err)
		}
		want, err := readActionPlanFilesForTest(standaloneOutputs[segment])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s combined segment differs from standalone emission", segment)
		}
	}
	gotReport, err := os.ReadFile(combinedReport)
	if err != nil {
		t.Fatal(err)
	}
	wantReport, err := os.ReadFile(standaloneReport)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotReport, wantReport) {
		t.Fatal("combined reuse report differs from standalone emission")
	}
}

func TestBuildAndWriteActionPlanFamilyPreparesReportBeforePublishingSegments(t *testing.T) {
	snapshot := familyTestSnapshot(
		t, 1, "drivers/example.o", familyTestConfig("y", "n"),
		ConfigDependencySet{Symbols: []string{"CONFIG_USED"}},
	)
	delete(snapshot.Toolsets, "host")
	root := t.TempDir()
	outputs := actionPlanFamilySegmentOutputsForTest(root)
	err := BuildAndWriteActionPlanFamily(
		[]ValidatedActionPlanFamilyVariant{readActionPlanFamilyVariantForTest(t, "base", snapshot)},
		outputs,
		filepath.Join(root, "reuse.json"),
	)
	if err == nil || !strings.Contains(err.Error(), "no host toolset identity") {
		t.Fatalf("BuildAndWriteActionPlanFamily() error = %v, want missing host diagnostic", err)
	}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		if _, statErr := os.Stat(outputs[segment]); !os.IsNotExist(statErr) {
			t.Errorf("%s segment was published before report preparation: %v", segment, statErr)
		}
	}
}

func TestBuildAndWriteActionPlanFamilyRejectsUnvalidatedOpaqueVariant(t *testing.T) {
	root := t.TempDir()
	err := BuildAndWriteActionPlanFamily(
		[]ValidatedActionPlanFamilyVariant{{}},
		actionPlanFamilySegmentOutputsForTest(root),
		filepath.Join(root, "reuse.json"),
	)
	if err == nil || !strings.Contains(err.Error(), "ReadActionPlanFamilyVariant") {
		t.Fatalf("BuildAndWriteActionPlanFamily() error = %v, want opaque-variant rejection", err)
	}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		if _, statErr := os.Stat(filepath.Join(root, segment)); !os.IsNotExist(statErr) {
			t.Errorf("%s segment was published for unvalidated opaque variant: %v", segment, statErr)
		}
	}
}

func TestStandaloneActionPlanFamilyEmissionRevalidatesMutableFamily(t *testing.T) {
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
		Name: "base", Snapshot: familyTestSegmentChainSnapshot(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	delete(family.Memberships, family.Nodes[0].ID)
	if err := family.WriteSegments(actionPlanFamilySegmentOutputsForTest(t.TempDir())); err == nil {
		t.Fatal("WriteSegments() accepted a mutated family")
	}
	if err := family.WriteReuseReport(filepath.Join(t.TempDir(), "reuse.json")); err == nil {
		t.Fatal("WriteReuseReport() accepted a mutated family")
	}
}

func TestActionPlanFamilyWritesV6MarkersAndSharedStoreBindings(t *testing.T) {
	snapshot := familyTestSnapshot(t, 1, "drivers/example.o", familyTestConfig("y", "n"), ConfigDependencySet{Symbols: []string{"CONFIG_USED"}})
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "family-plan")
	if err := family.Write(output); err != nil {
		t.Fatal(err)
	}
	node := family.Nodes[0]
	for _, marker := range []string{
		filepath.Join(output, "schema", LinuxKernelFamilyPlanSchema),
		filepath.Join(output, "nodes", node.Stage, node.ID, "out", "objects", "00000000", "at", "drivers", "example.o"),
		filepath.Join(output, "variants", "base", "view", "objects", "from", node.ID, "00000000", "at", "drivers", "example.o"),
	} {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("missing family marker %s: %v", marker, err)
		}
	}
	bindings, err := familyNodeInputBindings(node, map[string]ActionPlanNode{node.ID: node})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.Bindings) != 0 {
		t.Fatalf("source-only node unexpectedly has producer bindings: %#v", bindings)
	}
}

func TestFamilyNodeInputBindingsRetainLogicalProjection(t *testing.T) {
	producer := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "target",
		Outputs: []ActionPlanOutput{{
			Tree: "objects", Path: "drivers/example.o",
			ArtifactPath: ".linux-bzl-versions/first/drivers/example.o",
		}},
	}
	consumer := ActionPlanNode{
		ID:     strings.Repeat("b", 64),
		Inputs: []ActionPlanNodeEdge{{Role: "object", ProducerID: producer.ID, Slot: 0}},
	}
	bindings, err := familyNodeInputBindings(consumer, map[string]ActionPlanNode{producer.ID: producer})
	if err != nil {
		t.Fatal(err)
	}
	got := bindings.Bindings["object:00000000"]
	want := ActionPlanInputBinding{
		Tree: "objects", Path: "nodes/" + producer.ID + "/00000000",
		ProjectionTree: "objects",
		ProjectionPath: ".linux-bzl-versions/first/drivers/example.o",
	}
	if got != want {
		t.Fatalf("family input binding = %#v, want %#v", got, want)
	}
	data, err := bindings.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeActionPlanInputBindings(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bindings["object:00000000"] != want {
		t.Fatalf("decoded family binding = %#v, want %#v", decoded.Bindings["object:00000000"], want)
	}
}

func TestActionPlanFamilyEntriesBindProducerSortingAfterConsumer(t *testing.T) {
	for ordinal := 0; ordinal < 256; ordinal++ {
		snapshot := familyTestChainSnapshot(t, planOrdinal(ordinal), "")
		family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
		if err != nil {
			t.Fatal(err)
		}
		var producerID, consumerID string
		for _, node := range family.Nodes {
			if len(node.Inputs) == 0 {
				producerID = node.ID
			} else {
				consumerID = node.ID
			}
		}
		if consumerID >= producerID {
			continue
		}
		if _, err := family.entries(); err != nil {
			t.Fatalf("consumer %s sorted before producer %s: %v", consumerID, producerID, err)
		}
		return
	}
	t.Fatal("failed to construct a consumer digest sorting before its producer")
}

func familyCompileNodes(family *ActionPlanFamily) []ActionPlanNode {
	nodes := []ActionPlanNode{}
	for _, node := range family.Nodes {
		if node.Kind == "compile" && node.Tool == "cc" {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func TestActionPlanFamilyMaterializesPreciseCapsuleThroughPrepCopy(t *testing.T) {
	base := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "n"), "fallback")
	overlay := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "m"), "fallback")
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 1 {
		t.Fatalf("compile nodes = %d, want one shared compile: %#v", len(compiles), compiles)
	}
	compile := compiles[0]
	if got, want := family.Memberships[compile.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("compile memberships = %q, want %q", got, want)
	}
	if len(compile.Inputs) != 1 {
		t.Fatalf("compile inputs = %#v, want one materialized config projection", compile.Inputs)
	}
	producers := map[string]ActionPlanNode{}
	sourceByID := map[string]ActionPlanSource{}
	for _, node := range family.Nodes {
		producers[node.ID] = node
	}
	for _, source := range family.Sources {
		sourceByID[source.ID] = source
	}
	projection := producers[compile.Inputs[0].ProducerID]
	if projection.Kind != "copy" || len(projection.Sources) != 1 {
		t.Fatalf("compile projection producer = %#v, want cloned config copy", projection)
	}
	if got, want := family.Memberships[projection.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("projection memberships = %q, want %q", got, want)
	}
	source := sourceByID[projection.Sources[0].SourceID]
	if source.Namespace != "capsule" {
		t.Fatalf("projection source = %#v, want capsule", source)
	}
	digest, _, ok := strings.Cut(source.Path, "/")
	if !ok {
		t.Fatalf("capsule source path = %q", source.Path)
	}
	autoconf := family.Capsules[digest]["include/generated/autoconf.h"]
	if !strings.Contains(autoconf, "CONFIG_USED") || strings.Contains(autoconf, "CONFIG_OTHER") {
		t.Fatalf("precise autoconf capsule = %q", autoconf)
	}
	// The internal compiler projection is the only reachable prep producer.
	// Original full-config fallback copies are planner alternatives after the
	// compiler edge is redirected and must not enter the execution family.
	variantLocalCopies := 0
	for _, node := range family.Nodes {
		if node.Kind == "copy" && len(family.Memberships[node.ID]) == 1 {
			variantLocalCopies++
		}
	}
	if variantLocalCopies != 0 || len(family.Nodes) != 2 {
		t.Fatalf("variant-local full config copies/nodes = %d/%d, want 0/2", variantLocalCopies, len(family.Nodes))
	}
}

func TestActionPlanFamilyPrivateCompileOutputsFollowFinalConfigInputs(t *testing.T) {
	withPrivateOutput := func(config map[string]string, mode, allocation string) ActionPlanSnapshot {
		t.Helper()
		producerMode := mode
		if mode == "opaque" || mode == "lookalike" {
			producerMode = "fallback"
		}
		snapshot := familyTestPrepCopyCompileSnapshot(t, config, producerMode)
		plan := snapshotActionPlan(snapshot)
		sets := make([]ConfigDependencySet, len(plan.Nodes))
		compilerID := ""
		for index := range plan.Nodes {
			node := &plan.Nodes[index]
			sets[index] = snapshot.ConfigDependencies[node.ID]
			if node.Kind != "compile" {
				continue
			}
			compilerID = node.ID
			if mode == "opaque" {
				sets[index] = ConfigDependencySet{Opaque: true, Reason: "fixture unknown compiler inputs"}
			}
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			recipe.Arguments = append(recipe.Arguments, "-MD", "-MF", "${output:00000001}")
			recipe.Outputs = append(recipe.Outputs, "00000001", "00000002")
			recipe.ObservedOutputs = map[string]string{"00000002": "drivers/example.generated"}
			recipeID, err := recipe.ID()
			if err != nil {
				t.Fatal(err)
			}
			plan.Recipes[recipeID] = recipe
			node.Recipe = recipeID
			prefix := familyIntermediateArtifactDirectory
			if mode == "lookalike" {
				prefix = "ordinary-private-path"
			}
			node.Outputs = append(node.Outputs, ActionPlanOutput{
				Tree: "objects", Path: "drivers/.example.o.d",
				ArtifactPath: path.Join(prefix, allocation, "dependency-file"),
			}, ActionPlanOutput{
				Tree: "metadata", Path: path.Join(compactKbuildSideOutputStateDirectory, allocation+".state"),
				ObservedPath: "drivers/example.generated",
			})
		}
		consumerRecipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
			Arguments: []string{"-input", "${input:state:00000000}", "-out", "${output:00000000}"},
			Inputs:    []string{"state:00000000"}, Outputs: []string{"00000000"},
		}
		consumerRecipeID, err := consumerRecipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[consumerRecipeID] = consumerRecipe
		consumer := ActionPlanNode{
			ID: "state-consumer", Stage: "target", Kind: "copy", Tool: "actionfile", Recipe: consumerRecipeID, Product: "image",
			Inputs:  []ActionPlanNodeEdge{{Role: "state", ProducerID: compilerID, Slot: 2}},
			Outputs: []ActionPlanOutput{{Tree: "image", Path: "compiler-state"}},
		}
		store, err := plan.planningActionPlanInputSetStore()
		if err != nil {
			t.Fatal(err)
		}
		consumer.InputSet, err = store.Insert("", ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "state-input"},
			ProducerID: compilerID, Slot: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		plan.Nodes = append(plan.Nodes, consumer)
		sets = append(sets, ConfigDependencySet{})
		if err := plan.exportReachableActionPlanInputSets(); err != nil {
			t.Fatal(err)
		}
		if err := contentAddressActionPlanNodes(plan); err != nil {
			t.Fatal(err)
		}
		dependencies := map[string]ConfigDependencySet{}
		for index, node := range plan.Nodes {
			dependencies[node.ID] = sets[index]
		}
		result, err := canonicalActionPlanSnapshot(plan, dependencies, config)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, mode := range []string{"fallback", "selected", "noncanonical", "opaque", "lookalike"} {
		for _, used := range []string{"y", "n"} {
			t.Run(mode+"/used="+used, func(t *testing.T) {
				base := ActionPlanFamilyVariant{Name: "base", Snapshot: withPrivateOutput(familyTestConfig("y", "n"), mode, strings.Repeat("a", 64))}
				overlay := ActionPlanFamilyVariant{Name: "overlay", Snapshot: withPrivateOutput(familyTestConfig(used, "m"), mode, strings.Repeat("b", 64))}
				var baseline []string
				for _, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
					family, err := BuildActionPlanFamily(variants)
					if err != nil {
						t.Fatal(err)
					}
					compiles := familyCompileNodes(family)
					want := 2
					if mode == "fallback" && used == "y" {
						want = 1
					}
					if len(compiles) != want {
						t.Fatalf("private-output compiles = %d, want %d after final config projection", len(compiles), want)
					}
					ids := make([]string, 0, len(compiles))
					for _, compile := range compiles {
						ids = append(ids, compile.ID)
						if want == 1 && !slices.Equal(family.Memberships[compile.ID], []string{"base", "overlay"}) {
							t.Fatalf("private compiler is not shared: %v", family.Memberships[compile.ID])
						}
					}
					nodes := map[string]ActionPlanNode{}
					for _, node := range family.Nodes {
						nodes[node.ID] = node
					}
					for _, node := range family.Nodes {
						if len(node.Inputs) != 1 || node.Inputs[0].Role != "state" {
							continue
						}
						bindings, err := familyNodeInputBindings(node, nodes)
						if err != nil {
							t.Fatal(err)
						}
						producer := nodes[node.Inputs[0].ProducerID]
						if bindings.Bindings["state:00000000"].ProjectionPath != producer.Outputs[2].Path {
							t.Fatal("consumer retained the pre-localization state allocation")
						}
						if want == 1 && !slices.Equal(family.Memberships[node.ID], []string{"base", "overlay"}) {
							t.Fatal("state consumer or persistent input-set edge did not follow the shared compiler")
						}
					}
					sort.Strings(ids)
					if baseline == nil {
						baseline = ids
					} else if !slices.Equal(baseline, ids) {
						t.Fatal("private compiler identity depends on variant order")
					}
				}
			})
		}
	}
}

func TestActionPlanFamilyPrepCopyRelevantConfigSplitsCompile(t *testing.T) {
	base := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "n"), "fallback")
	overlay := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("n", "n"), "fallback")
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 2 {
		t.Fatalf("compile nodes = %d, want two config-specific compiles: %#v", len(compiles), compiles)
	}
	for _, compile := range compiles {
		if len(family.Memberships[compile.ID]) != 1 {
			t.Fatalf("relevant-config compile %s memberships = %q", compile.ID, family.Memberships[compile.ID])
		}
	}
}

func TestActionPlanFamilyPrunesUnrelatedFullConfigGeneratedCompilerInput(t *testing.T) {
	base := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("y", "n"), false)
	overlay := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("y", "m"), false)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 1 || !slices.Equal(family.Memberships[compiles[0].ID], []string{"base", "overlay"}) {
		t.Fatalf("unrelated generated input compiles/memberships = %#v/%#v, want one shared compile", compiles, family.Memberships)
	}
	if len(compiles[0].Inputs) != 0 {
		t.Fatalf("shared compile retained unrelated generated edge: %#v", compiles[0].Inputs)
	}
	recipe := family.Recipes[compiles[0].Recipe]
	if len(recipe.Inputs) != 0 || len(recipe.WorkingInputs) != 0 {
		t.Fatalf("shared compile retained unrelated generated bindings: %#v", recipe)
	}
	if len(family.Nodes) != 1 || len(family.Memberships) != 1 || len(family.Recipes) != 1 || len(family.Sources) != 1 || len(family.Capsules) != 0 {
		t.Fatalf("unreachable generated work survived: nodes=%d memberships=%d recipes=%d sources=%d capsules=%d", len(family.Nodes), len(family.Memberships), len(family.Recipes), len(family.Sources), len(family.Capsules))
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.UniqueNodes != 1 || report.VariantInstances != 2 || report.SharedNodes != 1 || report.ReusedInstances != 1 {
		t.Fatalf("pruned family reuse summary = %#v", report)
	}
	if len(report.OpaqueReasons) != 0 {
		t.Fatalf("pruned generated producers survived in opaque diagnostics: %#v", report.OpaqueReasons)
	}
}

func TestActionPlanFamilyRetainsAndRenumbersWorkingDirectoryBindings(t *testing.T) {
	generateRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-line", "directory", "-out", "${output:00000000}"},
		Outputs:   []string{"00000000"},
	}
	generateRecipeID, err := generateRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"},
		WorkingDirectory: strings.Join([]string{
			"${source:cwd-source:00000001}",
			"${input:cwd-input:00000001}",
		}, "/"),
		Sources: []string{"unused-source:00000000", "cwd-source:00000001"},
		Inputs:  []string{"unused-input:00000000", "cwd-input:00000001"},
		Outputs: []string{"00000000"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "unused-source"},
			{ID: "src-00000002", Namespace: "kernel", Path: "cwd-source"},
		},
		Recipes: map[string]ActionRecipe{generateRecipeID: generateRecipe, compileRecipeID: compileRecipe},
		Nodes: []ActionPlanNode{
			{
				ID: "unused-input", Stage: "prep", Kind: "generate", Recipe: generateRecipeID,
				Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: "unused-input"}},
			},
			{
				ID: "cwd-input", Stage: "prep", Kind: "generate", Recipe: generateRecipeID,
				Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: "cwd-input"}},
			},
			{
				ID: "compile", Stage: "target", Kind: "compile", Recipe: compileRecipeID,
				Tool: "cc", Product: "image",
				Sources: []ActionPlanSourceEdge{
					{Role: "unused-source", SourceID: "src-00000001"},
					{Role: "cwd-source", SourceID: "src-00000002"},
				},
				Inputs: []ActionPlanNodeEdge{
					{Role: "unused-input", ProducerID: "unused-input", Slot: 0},
					{Role: "cwd-input", ProducerID: "cwd-input", Slot: 0},
				},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "result.o"}},
			},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		dependencies[node.ID] = ConfigDependencySet{}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	var compile ActionPlanNode
	for _, node := range family.Nodes {
		if node.Kind == "compile" {
			compile = node
		}
	}
	if compile.ID == "" || len(compile.Sources) != 1 || compile.Sources[0].Role != "cwd-source" ||
		len(compile.Inputs) != 1 || compile.Inputs[0].Role != "cwd-input" {
		t.Fatalf("working-directory closure = %#v, want only cwd source/input", compile)
	}
	recipe := family.Recipes[compile.Recipe]
	if got, want := recipe.WorkingDirectory,
		"${source:cwd-source:00000000}/${input:cwd-input:00000000}"; got != want {
		t.Fatalf("working directory = %q, want %q", got, want)
	}
	if !slices.Equal(recipe.Sources, []string{"cwd-source:00000000"}) ||
		!slices.Equal(recipe.Inputs, []string{"cwd-input:00000000"}) {
		t.Fatalf("renumbered cwd recipe bindings = sources %q inputs %q", recipe.Sources, recipe.Inputs)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("working-directory family nodes = %d, want retained producer and compiler: %#v", got, family.Nodes)
	}
}

func TestActionPlanFamilyReattachesSymmetricGeneratedObjectClosure(t *testing.T) {
	base := ActionPlanFamilyVariant{
		Name: "base",
		Snapshot: familyTestSymmetricGeneratedObjectClosureSnapshot(
			t, familyTestConfig("y", "n"), "include/generated/base-only.h",
		),
	}
	overlay := ActionPlanFamilyVariant{
		Name: "overlay",
		Snapshot: familyTestSymmetricGeneratedObjectClosureSnapshot(
			t, familyTestConfig("y", "m"), "include/generated/overlay-only.h",
		),
	}
	var baselineID string
	for permutation, variants := range [][]ActionPlanFamilyVariant{{base, overlay}, {overlay, base}} {
		family, err := BuildActionPlanFamily(variants)
		if err != nil {
			t.Fatal(err)
		}
		compiles := familyCompileNodes(family)
		if len(compiles) != 1 {
			t.Fatalf("permutation %d generated-closure compiles = %#v, want one shared compile", permutation, compiles)
		}
		compile := compiles[0]
		if got, want := family.Memberships[compile.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
			t.Fatalf("permutation %d compile memberships = %q, want %q", permutation, got, want)
		}
		if permutation == 0 {
			baselineID = compile.ID
		} else if compile.ID != baselineID {
			t.Fatalf("generated object-closure identity changed with variant order: %s != %s", compile.ID, baselineID)
		}
		if got, want := len(compile.Inputs), 2; got != want {
			t.Fatalf("permutation %d shared compile inputs = %#v, want %d union inputs", permutation, compile.Inputs, want)
		}
		recipe := family.Recipes[compile.Recipe]
		if got, want := len(recipe.Inputs), 2; got != want || len(recipe.WorkingInputs) != 0 {
			t.Fatalf("permutation %d symmetric closure recipe inputs/working inputs = %#v/%#v, want %d/empty", permutation, recipe.Inputs, recipe.WorkingInputs, want)
		}
		nodes := make(map[string]ActionPlanNode, len(family.Nodes))
		for _, node := range family.Nodes {
			nodes[node.ID] = node
		}
		bindings, err := familyNodeInputBindings(compile, nodes)
		if err != nil {
			t.Fatal(err)
		}
		projected := map[string]bool{}
		for _, binding := range bindings.Bindings {
			if binding.ProjectionTree != "prep" {
				t.Fatalf("permutation %d generated closure projection tree = %q, want prep", permutation, binding.ProjectionTree)
			}
			projected[binding.ProjectionPath] = true
		}
		for _, pathname := range []string{"include/generated/base-only.h", "include/generated/overlay-only.h"} {
			if !projected[pathname] {
				t.Fatalf("permutation %d symmetric generated closure omits %q: %#v", permutation, pathname, bindings)
			}
		}
	}
}

func TestActionPlanFamilyRetainsStagedCompoundCompilerInputs(t *testing.T) {
	base := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "n"), familyCompoundUsesIncompleteGlob)
	overlay := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "m"), familyCompoundUsesIncompleteGlob)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	compounds := []ActionPlanNode{}
	for _, node := range family.Nodes {
		nodes[node.ID] = node
		recipe := family.Recipes[node.Recipe]
		if node.Kind == "compile" && recipe.CompilerInvocation != nil {
			compounds = append(compounds, node)
		}
	}
	if len(compounds) != 1 {
		t.Fatalf("compound compiler nodes = %#v, want one shared compile", compounds)
	}
	compile := compounds[0]
	if got, want := family.Memberships[compile.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("compound compile memberships = %q, want %q", got, want)
	}
	if len(compile.Inputs) != 1 {
		t.Fatalf("compound compile inputs = %#v, want generated extra.o producer", compile.Inputs)
	}
	extra := nodes[compile.Inputs[0].ProducerID]
	if compile.Inputs[0].Slot != 0 || len(extra.Outputs) != 1 || extra.Outputs[0].Path != "drivers/example/extra.o" {
		t.Fatalf("compound compile extra input = edge %#v producer %#v", compile.Inputs[0], extra)
	}
	if got, want := family.Memberships[extra.ID], []string{"base", "overlay"}; !slices.Equal(got, want) {
		t.Fatalf("extra.o producer memberships = %q, want %q", got, want)
	}
	recipe := family.Recipes[compile.Recipe]
	if recipe.CompilerInvocation == nil || recipe.CompilerInvocation.WorkingInputUsesComplete ||
		len(recipe.Inputs) != 1 || recipe.WorkingInputs["input:"+recipe.Inputs[0]] != "drivers/example/extra.o" {
		t.Fatalf("compound compile recipe lost staged extra.o: %#v", recipe)
	}
}

func TestActionPlanFamilyIncompleteCompoundLibrarySearchRetainsHiddenArchive(t *testing.T) {
	base := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "n"), familyCompoundUsesIncompleteSearch)
	overlay := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "m"), familyCompoundUsesIncompleteSearch)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range family.Nodes {
		recipe := family.Recipes[node.Recipe]
		if node.Kind != "compile" || recipe.CompilerInvocation == nil {
			continue
		}
		if recipe.CompilerInvocation.WorkingInputUsesComplete || len(node.Inputs) != 1 ||
			recipe.WorkingInputs["input:"+recipe.Inputs[0]] != "drivers/example/extra.o" {
			t.Fatalf("library-search compound did not retain hidden archive fail closed: node=%#v recipe=%#v", node, recipe)
		}
		return
	}
	t.Fatal("library-search family has no typed compound compile")
}

func TestActionPlanFamilyIncompleteCompoundToolRootRetainsHiddenInput(t *testing.T) {
	base := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "n"), familyCompoundUsesIncompleteRoot)
	overlay := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "m"), familyCompoundUsesIncompleteRoot)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range family.Nodes {
		recipe := family.Recipes[node.Recipe]
		if node.Kind != "compile" || recipe.CompilerInvocation == nil {
			continue
		}
		if recipe.CompilerInvocation.WorkingInputUsesComplete || len(node.Inputs) != 1 ||
			recipe.WorkingInputs["input:"+recipe.Inputs[0]] != "drivers/example/extra.o" {
			t.Fatalf("tool-root compound did not retain hidden input fail closed: node=%#v recipe=%#v", node, recipe)
		}
		return
	}
	t.Fatal("tool-root family has no typed compound compile")
}

func TestActionPlanFamilyCompleteCompoundUsesPruneAmbientInputs(t *testing.T) {
	base := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "n"), familyCompoundUsesComplete)
	overlay := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "m"), familyCompoundUsesComplete)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := []ActionPlanNode{}
	for _, node := range family.Nodes {
		if node.Kind == "compile" && family.Recipes[node.Recipe].CompilerInvocation != nil {
			compiles = append(compiles, node)
		}
	}
	if len(compiles) != 1 || !slices.Equal(family.Memberships[compiles[0].ID], []string{"base", "overlay"}) {
		t.Fatalf("complete-use compound compiles/memberships = %#v/%#v, want one shared compile", compiles, family.Memberships)
	}
	compile := compiles[0]
	if len(compile.Inputs) != 1 {
		t.Fatalf("complete-use compound inputs = %#v, want only hidden middle-command extra.o", compile.Inputs)
	}
	producer, ok := compactKbuildPlanNode(&ActionPlan{Nodes: family.Nodes}, compile.Inputs[0].ProducerID)
	if !ok || len(producer.Outputs) != 1 || producer.Outputs[0].Path != "drivers/example/extra.o" {
		t.Fatalf("complete-use retained producer = %#v, want extra.o", producer)
	}
	recipe := family.Recipes[compile.Recipe]
	if recipe.CompilerInvocation == nil || !recipe.CompilerInvocation.WorkingInputUsesComplete ||
		len(recipe.Inputs) != 1 || recipe.WorkingInputs["input:"+recipe.Inputs[0]] != "drivers/example/extra.o" {
		t.Fatalf("complete-use localized recipe = %#v, want exact extra.o input", recipe)
	}
	for _, node := range family.Nodes {
		for _, output := range node.Outputs {
			if output.Path == "include/config/kernel.release" || output.Path == "scripts/basic/.fixdep.cmd" {
				t.Fatalf("ambient compound input producer survived pruning: %#v", node)
			}
		}
	}
}

func TestActionPlanFamilyPreciseWholeTreePrunesUnrelatedPrepProducers(t *testing.T) {
	base := familyTestWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))
	overlay := familyTestWholePrepTreeSnapshot(t, familyTestConfig("y", "m"))
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 1 || !slices.Equal(family.Memberships[compiles[0].ID], []string{"base", "overlay"}) {
		t.Fatalf("whole-tree compiles/memberships = %#v/%#v, want one shared compile", compiles, family.Memberships)
	}
	if len(compiles[0].Inputs) != 0 || !slices.Equal(compiles[0].Trees, []string{"prep"}) {
		t.Fatalf("whole-tree compile dependencies = inputs %#v trees %#v", compiles[0].Inputs, compiles[0].Trees)
	}
	if len(family.Nodes) != 1 {
		t.Fatalf("unrelated prep-tree producers survived precise closure: %#v", family.Nodes)
	}
}

func TestActionPlanFamilyPreciseWholeTreeMaterializesReferencedPriorStageProducer(t *testing.T) {
	snapshot := familyTestOpaqueWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))
	for _, node := range snapshot.Nodes {
		if node.Kind == "compile" {
			snapshot.ConfigDependencies[node.ID] = ConfigDependencySet{
				SourcePaths: []string{"drivers/example.c"},
				ObjectPaths: []string{"include/generated/full-config.h"},
			}
		}
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(family.Nodes) != 2 {
		t.Fatalf("precise prep-tree closure has %d nodes, want producer and consumer: %#v", len(family.Nodes), family.Nodes)
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	var producer, consumer ActionPlanNode
	for _, node := range family.Nodes {
		nodes[node.ID] = node
		switch node.Kind {
		case "copy":
			producer = node
		case "compile":
			consumer = node
		}
	}
	if producer.ID == "" || consumer.ID == "" || len(consumer.Inputs) != 1 || consumer.Inputs[0] != (ActionPlanNodeEdge{
		Role: "tree-prep", ProducerID: producer.ID, Slot: 0,
	}) {
		t.Fatalf("precise prep-tree producer/consumer closure = %#v", family.Nodes)
	}
	recipe := family.Recipes[consumer.Recipe]
	if !slices.Equal(recipe.Inputs, []string{"tree-prep:00000000"}) {
		t.Fatalf("precise prep-tree recipe inputs = %q", recipe.Inputs)
	}
	bindings, err := familyNodeInputBindings(consumer, nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := ActionPlanInputBinding{
		Tree: "prep", Path: "nodes/" + producer.ID + "/00000000",
		ProjectionTree: "prep", ProjectionPath: "include/generated/full-config.h",
	}
	if got := bindings.Bindings["tree-prep:00000000"]; got != want {
		t.Fatalf("precise prep-tree input binding = %#v, want %#v", got, want)
	}
}

func TestActionPlanFamilyOpaqueWholeTreeMaterializesPriorStageProducer(t *testing.T) {
	snapshot := familyTestOpaqueWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(family.Nodes) != 2 {
		t.Fatalf("opaque prep-tree closure has %d nodes, want producer and consumer: %#v", len(family.Nodes), family.Nodes)
	}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	var producer, consumer ActionPlanNode
	for _, node := range family.Nodes {
		nodes[node.ID] = node
		switch node.Kind {
		case "copy":
			producer = node
		case "compile":
			consumer = node
		}
	}
	if producer.ID == "" || consumer.ID == "" {
		t.Fatalf("opaque prep-tree family omits producer or consumer: %#v", family.Nodes)
	}
	if got := family.Memberships[producer.ID]; !slices.Equal(got, []string{"base"}) {
		t.Fatalf("producer memberships = %q, want base", got)
	}
	if got := family.Memberships[consumer.ID]; !slices.Equal(got, []string{"base"}) {
		t.Fatalf("consumer memberships = %q, want base", got)
	}
	if len(consumer.Inputs) != 1 || consumer.Inputs[0] != (ActionPlanNodeEdge{
		Role: "tree-prep", ProducerID: producer.ID, Slot: 0,
	}) {
		t.Fatalf("opaque prep-tree consumer inputs = %#v, want exact synthetic producer edge", consumer.Inputs)
	}
	if !slices.Equal(consumer.Trees, []string{"prep"}) {
		t.Fatalf("opaque prep-tree consumer trees = %q", consumer.Trees)
	}
	recipe := family.Recipes[consumer.Recipe]
	if !slices.Equal(recipe.Inputs, []string{"tree-prep:00000000"}) ||
		!slices.Equal(recipe.Trees, []string{"prep"}) || !slices.Equal(recipe.WorkingTrees, []string{"prep"}) {
		t.Fatalf("opaque prep-tree recipe = %#v", recipe)
	}
	bindings, err := familyNodeInputBindings(consumer, nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := ActionPlanInputBinding{
		Tree: "prep", Path: "nodes/" + producer.ID + "/00000000",
		ProjectionTree: "prep", ProjectionPath: "include/generated/full-config.h",
	}
	if got := bindings.Bindings["tree-prep:00000000"]; got != want {
		t.Fatalf("opaque prep-tree input binding = %#v, want %#v", got, want)
	}
}

func TestActionPlanFamilyOpaqueWholeTreeDoesNotOverwriteConfigWriter(t *testing.T) {
	for _, producerMode := range []string{"selected", "noncanonical"} {
		t.Run(producerMode, func(t *testing.T) {
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{
				Name: "base", Snapshot: familyTestOpaquePrepConfigWriterSnapshot(
					t, familyTestConfig("y", "n"), producerMode,
				),
			}})
			if err != nil {
				t.Fatal(err)
			}
			nodes := make(map[string]ActionPlanNode, len(family.Nodes))
			var producer, consumer ActionPlanNode
			for _, node := range family.Nodes {
				nodes[node.ID] = node
				switch node.Kind {
				case "copy":
					producer = node
				case "compile":
					consumer = node
				}
			}
			if producer.ID == "" || consumer.ID == "" || len(consumer.Inputs) != 1 {
				t.Fatalf("%s family omits exact producer/consumer closure: %#v", producerMode, family.Nodes)
			}
			bindings, err := familyNodeInputBindings(consumer, nodes)
			if err != nil {
				t.Fatal(err)
			}
			bindingName := consumer.Inputs[0].Role + ":" + planOrdinal(0)
			projection := bindings.Bindings[bindingName]
			if projection.ProjectionTree != "prep" {
				t.Fatalf("%s producer projection = %#v, want prep tree", producerMode, projection)
			}
			recipe := family.Recipes[consumer.Recipe]
			staged := map[string]bool{}
			for _, pathname := range recipe.WorkingInputs {
				staged[pathname] = true
			}
			if staged[projection.ProjectionPath] {
				t.Fatalf(
					"%s producer projection %q is overwritten by an ambient working input: %#v",
					producerMode, projection.ProjectionPath, recipe.WorkingInputs,
				)
			}
			const autoconf = "include/generated/autoconf.h"
			if producerMode == "selected" {
				if projection.ProjectionPath != autoconf || staged[autoconf] {
					t.Fatalf("selected writer projection/staged paths = %q/%#v", projection.ProjectionPath, staged)
				}
			} else if projection.ProjectionPath == autoconf || !staged[autoconf] {
				t.Fatalf("noncanonical writer projection/staged paths = %q/%#v", projection.ProjectionPath, staged)
			}
		})
	}
}

func TestActionPlanFamilyProjectedInternalsDoNotPoisonWholeTreeConsumerReuse(t *testing.T) {
	base := familyTestProjectedGeneratorWholePrepTreeSnapshot(t, familyTestConfig("y", "n"))

	// Exercise the complete-tree path directly with an opaque classification.
	// Even a fail-closed consumer may observe only the public canonical header;
	// differential raw/replay/check artifacts are validation provenance, not
	// logical Kbuild tree contents.
	original := snapshotActionPlan(base)
	localized := cloneActionPlan(original)
	structuralIDs := make(map[string]string, len(original.Nodes))
	unionSets := make(map[string]ConfigDependencySet, len(original.Nodes))
	consumerID := ""
	for _, node := range original.Nodes {
		structuralIDs[node.ID] = node.ID
		unionSets[node.ID] = base.ConfigDependencies[node.ID]
		if node.Kind == "link-driver" {
			consumerID = node.ID
			unionSets[node.ID] = ConfigDependencySet{Opaque: true, Reason: "opaque whole prep tree"}
		}
	}
	if consumerID == "" {
		t.Fatal("projected-generator fixture has no whole-tree consumer")
	}
	if _, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, unionSets); err != nil {
		t.Fatal(err)
	}
	localizedConsumer, ok := compactKbuildPlanNode(localized, consumerID)
	if !ok || len(localizedConsumer.Inputs) != 1 {
		t.Fatalf("opaque whole-tree consumer inputs = %#v, want only the canonical projected header", localizedConsumer.Inputs)
	}
	if projectedGeneratorOutputIsInternal(
		original, localizedConsumer.Inputs[0].ProducerID, localizedConsumer.Inputs[0].Slot,
	) {
		t.Fatalf("opaque whole-tree consumer inherited internal projected-generator edge %#v", localizedConsumer.Inputs[0])
	}
	localizedProducer, ok := compactKbuildPlanNode(localized, localizedConsumer.Inputs[0].ProducerID)
	if !ok || len(localizedProducer.Outputs) != 1 ||
		localizedProducer.Outputs[0] != (ActionPlanOutput{Tree: "prep", Path: "include/generated/projected.h"}) {
		t.Fatalf("opaque whole-tree consumer producer = %#v, want public canonical validator", localizedProducer)
	}

	overlay := familyTestProjectedGeneratorWholePrepTreeSnapshot(t, familyTestConfig("y", "m"))
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	consumers := []ActionPlanNode{}
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		nodes[node.ID] = node
		if node.Kind == "link-driver" {
			consumers = append(consumers, node)
		}
	}
	if len(consumers) != 1 || !slices.Equal(family.Memberships[consumers[0].ID], []string{"base", "overlay"}) {
		t.Fatalf("whole-tree consumers/memberships = %#v/%#v, want one consumer shared across the irrelevant config", consumers, family.Memberships)
	}
	consumer := consumers[0]
	if len(consumer.Inputs) != 1 {
		t.Fatalf("shared whole-tree consumer inputs = %#v, want only the canonical projected header", consumer.Inputs)
	}
	producer := nodes[consumer.Inputs[0].ProducerID]
	if len(producer.Outputs) != 1 || producer.Outputs[0] != (ActionPlanOutput{Tree: "prep", Path: "include/generated/projected.h"}) {
		t.Fatalf("shared whole-tree consumer producer = %#v, want public canonical validator", producer)
	}
	for _, output := range producer.Outputs {
		artifactPath := actionPlanOutputArtifactPath(output)
		if strings.HasPrefix(artifactPath, compactKbuildProjectedFilechkRoot+"/") ||
			strings.HasPrefix(artifactPath, compactKbuildProjectionValidateRoot+"/") {
			t.Fatalf("shared whole-tree consumer inherited private projected-generator artifact %q", artifactPath)
		}
	}
}

func TestAddFamilyPriorTreeInputsSelectsCanonicalVisibleProducer(t *testing.T) {
	const logicalPath = "include/generated/visible.h"
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-line", "value", "-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-c", "source.c", "-o", "${output:00000000}"}, WorkingDirectory: "compile-visible",
		Outputs: []string{"00000000"}, Trees: []string{"prep"}, WorkingTrees: []string{"prep"},
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	makePlan := func(includeWinner bool, opaque bool) (*ActionPlan, map[string]string, map[string]ConfigDependencySet) {
		nodes := []ActionPlanNode{{
			ID: "shadow", Stage: "prep", Kind: "generate", Recipe: producerRecipeID, Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{
				Tree: "prep", Path: logicalPath,
				ArtifactPath: familySelectionArtifactDirectory + "/" + strings.Repeat("a", 64) + "/" + logicalPath,
			}},
		}}
		if includeWinner {
			nodes = append(nodes, ActionPlanNode{
				ID: "winner", Stage: "prep", Kind: "generate", Recipe: producerRecipeID, Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: logicalPath}},
			})
		}
		nodes = append(nodes, ActionPlanNode{
			ID: "consumer", Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
			Trees: []string{"prep"}, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "source.o"}},
		})
		plan := &ActionPlan{
			Recipes: map[string]ActionRecipe{producerRecipeID: producerRecipe, compileRecipeID: compileRecipe},
			Nodes:   nodes,
		}
		structuralIDs := map[string]string{"shadow": "struct-shadow", "consumer": "struct-consumer"}
		if includeWinner {
			structuralIDs["winner"] = "struct-winner"
		}
		dependencies := ConfigDependencySet{ObjectPaths: []string{logicalPath}}
		if opaque {
			dependencies = ConfigDependencySet{Opaque: true, Reason: "opaque prep tree"}
		}
		return plan, structuralIDs, map[string]ConfigDependencySet{"struct-consumer": dependencies}
	}
	bindShadow := func(t *testing.T, plan *ActionPlan, stageAtLogicalPath bool) {
		t.Helper()
		consumer := &plan.Nodes[len(plan.Nodes)-1]
		consumer.Inputs = []ActionPlanNodeEdge{{Role: "sequence", ProducerID: "shadow", Slot: 0}}
		recipe := cloneActionRecipe(plan.Recipes[consumer.Recipe])
		recipe.Inputs = []string{"sequence:00000000"}
		if stageAtLogicalPath {
			recipe.WorkingInputs = map[string]string{"input:sequence:00000000": logicalPath}
		} else {
			recipe.WorkingInputs = nil
		}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[recipeID] = recipe
		consumer.Recipe = recipeID
	}

	t.Run("precise canonical winner", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(true, false)
		localized := cloneActionPlan(original)
		if _, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies); err != nil {
			t.Fatal(err)
		}
		consumer := localized.Nodes[len(localized.Nodes)-1]
		if got, want := consumer.Inputs, []ActionPlanNodeEdge{{Role: "tree-prep", ProducerID: "winner", Slot: 0}}; !slices.Equal(got, want) {
			t.Fatalf("precise prior-tree inputs = %#v, want canonical winner %#v", got, want)
		}
		bindings, err := familyNodeInputBindings(consumer, map[string]ActionPlanNode{
			"shadow": localized.Nodes[0], "winner": localized.Nodes[1],
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := bindings.Bindings["tree-prep:00000000"].ProjectionPath; got != logicalPath {
			t.Fatalf("precise winner projection = %q, want %q", got, logicalPath)
		}
	})

	t.Run("precise noncanonical only", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(false, false)
		localized := cloneActionPlan(original)
		_, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies)
		if err == nil || !strings.Contains(err.Error(), "no canonical prior-stage producer") {
			t.Fatalf("noncanonical-only precise tree error = %v", err)
		}
	})

	t.Run("explicit shadow logical projection supersedes canonical winner", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(true, false)
		bindShadow(t, original, true)
		localized := cloneActionPlan(original)
		if _, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies); err != nil {
			t.Fatal(err)
		}
		consumer := localized.Nodes[len(localized.Nodes)-1]
		want := []ActionPlanNodeEdge{{Role: "sequence", ProducerID: "shadow", Slot: 0}}
		if !slices.Equal(consumer.Inputs, want) {
			t.Fatalf("explicit logical projection inputs = %#v, want only shadow %#v", consumer.Inputs, want)
		}
		if got := localized.Recipes[consumer.Recipe].WorkingInputs["input:sequence:00000000"]; got != logicalPath {
			t.Fatalf("explicit shadow projection = %q, want %q", got, logicalPath)
		}
	})

	t.Run("sequencing shadow does not project logical path", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(false, false)
		bindShadow(t, original, false)
		localized := cloneActionPlan(original)
		_, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies)
		if err == nil || !strings.Contains(err.Error(), "no canonical prior-stage producer") {
			t.Fatalf("sequencing-only shadow error = %v, want missing canonical producer", err)
		}
	})

	t.Run("explicit shadow logical projection needs no canonical winner", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(false, false)
		bindShadow(t, original, true)
		localized := cloneActionPlan(original)
		if _, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies); err != nil {
			t.Fatal(err)
		}
		consumer := localized.Nodes[len(localized.Nodes)-1]
		want := []ActionPlanNodeEdge{{Role: "sequence", ProducerID: "shadow", Slot: 0}}
		if !slices.Equal(consumer.Inputs, want) {
			t.Fatalf("noncanonical explicit projection inputs = %#v, want %#v", consumer.Inputs, want)
		}
	})

	t.Run("opaque complete baseline", func(t *testing.T) {
		original, structuralIDs, dependencies := makePlan(true, true)
		localized := cloneActionPlan(original)
		if _, err := addFamilyPriorTreeInputs(original, localized, structuralIDs, dependencies); err != nil {
			t.Fatal(err)
		}
		consumer := localized.Nodes[len(localized.Nodes)-1]
		if len(consumer.Inputs) != 2 {
			t.Fatalf("opaque prior-tree inputs = %#v, want shadow and winner", consumer.Inputs)
		}
		nodes := map[string]ActionPlanNode{"shadow": localized.Nodes[0], "winner": localized.Nodes[1]}
		bindings, err := familyNodeInputBindings(consumer, nodes)
		if err != nil {
			t.Fatal(err)
		}
		projections := map[string]bool{}
		for _, binding := range bindings.Bindings {
			projections[binding.ProjectionPath] = true
		}
		shadowPath := familySelectionArtifactDirectory + "/" + strings.Repeat("a", 64) + "/" + logicalPath
		if !projections[logicalPath] || !projections[shadowPath] {
			t.Fatalf("opaque prior-tree projections = %#v, want logical winner and versioned shadow", projections)
		}
	})
}

func TestActionPlanFamilyRetainsIncludedFullConfigGeneratedCompilerInput(t *testing.T) {
	base := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("y", "n"), true)
	overlay := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("y", "m"), true)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 2 {
		t.Fatalf("included generated input compiles = %#v, want one per config", compiles)
	}
	for _, compile := range compiles {
		if len(family.Memberships[compile.ID]) != 1 || len(compile.Inputs) != 1 {
			t.Fatalf("included generated compile %s memberships/inputs = %q/%#v", compile.ID, family.Memberships[compile.ID], compile.Inputs)
		}
		recipe := family.Recipes[compile.Recipe]
		if len(recipe.Inputs) != 1 || recipe.WorkingInputs["input:generated:00000000"] != "include/generated/full-config.h" {
			t.Fatalf("included generated compile recipe = %#v", recipe)
		}
	}
}

func TestActionPlanFamilyDoesNotRepoisonKnownConfigFreeDirectSources(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		tool string
	}{
		{name: "archive", kind: "archive", tool: "ar"},
		{name: "relocatable-link", kind: "link-relocatable", tool: "ld"},
		{name: "assembler", kind: "compile", tool: "as"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := familyTestNonCompilerConfigSourceSnapshot(t, familyTestConfig("y", "n"), test.kind, test.tool, false)
			overlay := familyTestNonCompilerConfigSourceSnapshot(t, familyTestConfig("y", "m"), test.kind, test.tool, false)
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
				{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(family.Nodes) != 1 || !slices.Equal(family.Memberships[family.Nodes[0].ID], []string{"base", "overlay"}) {
				t.Fatalf("known config-free family = nodes %#v memberships %#v", family.Nodes, family.Memberships)
			}
		})
	}
	base := familyTestNonCompilerConfigSourceSnapshot(t, familyTestConfig("y", "n"), "copy", "actionfile", false)
	overlay := familyTestNonCompilerConfigSourceSnapshot(t, familyTestConfig("y", "m"), "copy", "actionfile", false)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(family.Nodes) != 2 {
		t.Fatalf("direct config-copy family nodes = %#v, want one full-config copy per variant", family.Nodes)
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	wantReasons := []ActionPlanFamilyOpaqueReason{
		{Variant: "base", Kind: "copy", Reason: directNonCompilerConfigProjectionReason, Nodes: 1},
		{Variant: "overlay", Kind: "copy", Reason: directNonCompilerConfigProjectionReason, Nodes: 1},
	}
	if !slices.Equal(report.OpaqueReasons, wantReasons) {
		t.Fatalf("direct config-copy opaque diagnostics = %#v, want %#v", report.OpaqueReasons, wantReasons)
	}
}

func TestActionPlanFamilyForcedDirectPrepConfigConsumerIsOpaqueEverywhere(t *testing.T) {
	withPrepTree := func(snapshot ActionPlanSnapshot) ActionPlanSnapshot {
		t.Helper()
		plan := snapshotActionPlan(snapshot)
		if len(plan.Nodes) != 1 {
			t.Fatalf("direct config fixture nodes = %d, want one", len(plan.Nodes))
		}
		node := &plan.Nodes[0]
		oldNodeID := node.ID
		oldRecipeID := node.Recipe
		recipe := cloneActionRecipe(plan.Recipes[oldRecipeID])
		recipe.Trees = []string{"prep"}
		recipe.WorkingTrees = []string{"prep"}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		delete(plan.Recipes, oldRecipeID)
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
		node.Trees = []string{"prep"}
		if err := contentAddressActionPlanNodes(plan); err != nil {
			t.Fatal(err)
		}
		dependencies := snapshot.ConfigDependencies[oldNodeID]
		if dependencies.Opaque {
			t.Fatalf("direct config fixture is already opaque: %#v", dependencies)
		}
		result, err := canonicalActionPlanSnapshot(
			plan, map[string]ConfigDependencySet{plan.Nodes[0].ID: dependencies}, snapshot.ConfigFiles,
		)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{
			Name: "base",
			Snapshot: withPrepTree(familyTestNonCompilerConfigSourceSnapshot(
				t, familyTestConfig("y", "n"), "copy", "actionfile", false,
			)),
		},
		{
			Name: "overlay",
			Snapshot: withPrepTree(familyTestNonCompilerConfigSourceSnapshot(
				t, familyTestConfig("y", "m"), "copy", "actionfile", false,
			)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(family.Nodes), 2; got != want {
		t.Fatalf("forced opaque prep consumers = %d, want %d", got, want)
	}
	for _, node := range family.Nodes {
		recipe := family.Recipes[node.Recipe]
		staged := map[string]bool{}
		for _, pathname := range recipe.WorkingInputs {
			staged[canonicalKbuildRulePath(pathname)] = true
		}
		for _, projection := range ResolvedConfigProjectionOutputs() {
			if !staged[canonicalKbuildRulePath(projection)] {
				t.Errorf("forced opaque prep consumer %s did not stage %q", node.ID, projection)
			}
		}
	}
	report, err := family.ReuseReport()
	if err != nil {
		t.Fatal(err)
	}
	wantReasons := []ActionPlanFamilyOpaqueReason{
		{Variant: "base", Kind: "copy", Reason: directNonCompilerConfigProjectionReason, Nodes: 1},
		{Variant: "overlay", Kind: "copy", Reason: directNonCompilerConfigProjectionReason, Nodes: 1},
	}
	if !slices.Equal(report.OpaqueReasons, wantReasons) {
		t.Fatalf("forced opaque prep diagnostics = %#v, want %#v", report.OpaqueReasons, wantReasons)
	}
}

func TestActionPlanFamilySplitsArchiveAndLinkNodesThatReadConfigSources(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		tool string
	}{
		{name: "archive", kind: "archive", tool: "ar"},
		{name: "relocatable-link", kind: "link-relocatable", tool: "ld"},
		{name: "driver-link", kind: "link-driver", tool: "cc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			configs := map[string]map[string]string{
				"base":    familyTestConfig("y", "n"),
				"overlay": familyTestConfig("y", "m"),
			}
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
				{Name: "base", Snapshot: familyTestNonCompilerConfigSourceSnapshot(t, configs["base"], test.kind, test.tool, true)},
				{Name: "overlay", Snapshot: familyTestNonCompilerConfigSourceSnapshot(t, configs["overlay"], test.kind, test.tool, true)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(family.Nodes), 2; got != want {
				t.Fatalf("semantic config consumers = %d, want %d variant-local nodes: %#v", got, want, family.Nodes)
			}
			if got, want := len(family.Capsules), 2; got != want {
				t.Fatalf("semantic config consumer capsules = %d, want %d full capsules", got, want)
			}
			sources := make(map[string]ActionPlanSource, len(family.Sources))
			for _, source := range family.Sources {
				sources[source.ID] = source
			}
			for _, node := range family.Nodes {
				members := family.Memberships[node.ID]
				if len(members) != 1 {
					t.Fatalf("semantic config consumer %s memberships = %q, want one variant", node.ID, members)
				}
				if len(node.Sources) != 1 {
					t.Fatalf("semantic config consumer %s sources = %#v, want one config source", node.ID, node.Sources)
				}
				source := sources[node.Sources[0].SourceID]
				if source.Namespace != "capsule" {
					t.Fatalf("semantic config consumer %s source = %#v, want capsule", node.ID, source)
				}
				digest, projection, ok := strings.Cut(source.Path, "/")
				if !ok || projection != ".config" {
					t.Fatalf("semantic config consumer %s capsule source path = %q", node.ID, source.Path)
				}
				if got, want := family.Capsules[digest], configs[members[0]]; !maps.Equal(got, want) {
					t.Fatalf("semantic config consumer %s capsule = %#v, want full %s config %#v", node.ID, got, members[0], want)
				}
			}
		})
	}
}

func TestActionPlanFamilyKeepsSelectedAndNoncanonicalConfigWritersFull(t *testing.T) {
	for _, producerMode := range []string{"selected", "noncanonical"} {
		t.Run(producerMode, func(t *testing.T) {
			base := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "n"), producerMode)
			overlay := familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "m"), producerMode)
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
				{Name: "base", Snapshot: base}, {Name: "overlay", Snapshot: overlay},
			})
			if err != nil {
				t.Fatal(err)
			}
			compiles := familyCompileNodes(family)
			if len(compiles) != 2 {
				t.Fatalf("compile nodes = %d, want conservative split for %s producer", len(compiles), producerMode)
			}
		})
	}
}

func familyBenchmarkSnapshot(tb testing.TB, config map[string]string, compileCount int) ActionPlanSnapshot {
	return familyBenchmarkSnapshotMode(tb, config, compileCount, "explicit-precise")
}

func familyBenchmarkSnapshotMode(tb testing.TB, config map[string]string, compileCount int, mode string) ActionPlanSnapshot {
	tb.Helper()
	if mode != "explicit-precise" && mode != "prior-tree-precise" && mode != "prior-tree-opaque" {
		tb.Fatalf("unknown family benchmark mode %q", mode)
	}
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	}
	copyRecipeID, err := copyRecipe.ID()
	if err != nil {
		tb.Fatal(err)
	}
	compileInputs := []string(nil)
	workingInputs := map[string]string(nil)
	if mode == "explicit-precise" {
		compileInputs = make([]string, len(resolvedConfigProjections()))
		workingInputs = make(map[string]string, len(resolvedConfigProjections()))
		for index, projection := range resolvedConfigProjections() {
			binding := "config:" + planOrdinal(index)
			compileInputs[index] = binding
			workingInputs["input:"+binding] = projection.output
		}
	}
	compileRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:        []string{"-c", "${source:source:00000000}", "-o", "${output:00000000}"},
		WorkingDirectory: "compile-family-benchmark",
		WorkingInputs:    workingInputs,
		Sources:          []string{"source:00000000"}, Inputs: compileInputs, Outputs: []string{"00000000"},
	}
	if mode != "explicit-precise" {
		compileRecipe.Trees = []string{"prep"}
		compileRecipe.WorkingTrees = []string{"prep"}
	}
	compileRecipeID, err := compileRecipe.ID()
	if err != nil {
		tb.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": "sha256-" + strings.Repeat("1", 64)},
		Recipes:  map[string]ActionRecipe{copyRecipeID: copyRecipe, compileRecipeID: compileRecipe},
		Products: []ActionPlanProduct{{Name: "image", Tree: "objects", Path: LinuxKernelTreeRootMarker}},
	}
	projectionProducerIDs := make([]string, 0, len(resolvedConfigProjections()))
	for index, projection := range resolvedConfigProjections() {
		sourceID := "src-" + planOrdinal(index+1)
		producerID := "config-projection-" + planOrdinal(index)
		plan.Sources = append(plan.Sources, ActionPlanSource{ID: sourceID, Namespace: "config", Path: projection.input})
		plan.Nodes = append(plan.Nodes, ActionPlanNode{
			ID: producerID, Stage: "prep", Kind: "copy", Recipe: copyRecipeID, Tool: "actionfile", Product: "sdk",
			Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: sourceID}},
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: projection.output}},
		})
		projectionProducerIDs = append(projectionProducerIDs, producerID)
	}
	for index := 0; index < compileCount; index++ {
		sourceID := "src-" + planOrdinal(len(resolvedConfigProjections())+index+1)
		plan.Sources = append(plan.Sources, ActionPlanSource{
			ID: sourceID, Namespace: "kernel", Path: fmt.Sprintf("drivers/bench/file_%04d.c", index),
		})
		inputs := []ActionPlanNodeEdge(nil)
		if mode == "explicit-precise" {
			inputs = make([]ActionPlanNodeEdge, len(projectionProducerIDs))
			for inputIndex, producerID := range projectionProducerIDs {
				inputs[inputIndex] = ActionPlanNodeEdge{Role: "config", ProducerID: producerID, Slot: 0}
			}
		}
		plan.Nodes = append(plan.Nodes, ActionPlanNode{
			ID: fmt.Sprintf("compile-%08d", index), Stage: "target", Kind: "compile", Recipe: compileRecipeID, Tool: "cc", Product: "image",
			Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: sourceID}}, Inputs: inputs,
			Trees:   slices.Clone(compileRecipe.Trees),
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: fmt.Sprintf("drivers/bench/file_%04d.o", index)}},
		})
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		tb.Fatal(err)
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	dependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		if node.Kind == "compile" {
			if mode == "prior-tree-opaque" {
				dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "benchmark opaque compiler"}
			} else {
				dependencies[node.ID] = ConfigDependencySet{
					Symbols: []string{"CONFIG_USED"}, ObjectPaths: []string{"include/generated/autoconf.h"},
				}
			}
		} else {
			dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "fallback config projection"}
		}
	}
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, config)
	if err != nil {
		tb.Fatal(err)
	}
	return snapshot
}

func familyBenchmarkConfig(variant int, symbols int) map[string]string {
	config := familyTestConfig("y", fmt.Sprintf("variant_%d", variant))
	var dotConfig, autoConf, autoconf, rustc strings.Builder
	for index := 0; index < symbols; index++ {
		symbol := fmt.Sprintf("CONFIG_BENCH_%05d", index)
		fmt.Fprintf(&dotConfig, "%s=y\n", symbol)
		fmt.Fprintf(&autoConf, "%s=y\n", symbol)
		fmt.Fprintf(&autoconf, "#define %s 1\n", symbol)
		fmt.Fprintf(&rustc, "--cfg=%s=\"y\"\n", symbol)
	}
	config[".config"] += dotConfig.String()
	config["include/config/auto.conf"] += autoConf.String()
	config["include/generated/autoconf.h"] = strings.TrimSuffix(config["include/generated/autoconf.h"], "#endif\n") + autoconf.String() + "#endif\n"
	config["include/generated/rustc_cfg"] += rustc.String()
	return config
}

func BenchmarkBuildActionPlanFamilyCrossConfig(b *testing.B) {
	const (
		variantCount = 4
		compileCount = 64
	)
	variants := make([]ActionPlanFamilyVariant, 0, variantCount)
	for index := 0; index < variantCount; index++ {
		variants = append(variants, ActionPlanFamilyVariant{
			Name: fmt.Sprintf("variant_%d", index),
			Snapshot: familyBenchmarkSnapshot(
				b,
				familyTestConfig("y", fmt.Sprintf("variant_%d", index)),
				compileCount,
			),
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	var family *ActionPlanFamily
	for iteration := 0; iteration < b.N; iteration++ {
		var err error
		family, err = BuildActionPlanFamily(variants)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	sharedCompiles := 0
	for _, node := range family.Nodes {
		if node.Kind == "compile" && len(family.Memberships[node.ID]) == variantCount {
			sharedCompiles++
		}
	}
	variantNodes := variantCount * (compileCount + len(resolvedConfigProjections()))
	b.ReportMetric(float64(variantNodes), "variant-nodes/op")
	b.ReportMetric(float64(len(family.Nodes)), "unique-nodes/op")
	b.ReportMetric(float64(variantNodes-len(family.Nodes)), "reused-instances/op")
	b.ReportMetric(float64(sharedCompiles), "shared-compiles/op")
}

func BenchmarkBuildActionPlanFamilyPriorTreeScale(b *testing.B) {
	const (
		variantCount  = 4
		compileCount  = 512
		configSymbols = 2048
	)
	for _, mode := range []string{"prior-tree-precise", "prior-tree-opaque"} {
		b.Run(mode, func(b *testing.B) {
			variants := make([]ActionPlanFamilyVariant, 0, variantCount)
			for index := 0; index < variantCount; index++ {
				variants = append(variants, ActionPlanFamilyVariant{
					Name: fmt.Sprintf("variant_%d", index),
					Snapshot: familyBenchmarkSnapshotMode(
						b, familyBenchmarkConfig(index, configSymbols), compileCount, mode,
					),
				})
			}
			b.ReportAllocs()
			b.ResetTimer()
			var family *ActionPlanFamily
			for iteration := 0; iteration < b.N; iteration++ {
				var err error
				family, err = BuildActionPlanFamily(variants)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(variantCount*(compileCount+len(resolvedConfigProjections()))), "variant-nodes/op")
			b.ReportMetric(float64(len(family.Nodes)), "unique-nodes/op")
		})
	}
}

func BenchmarkWriteActionPlanFamilySegments(b *testing.B) {
	const (
		variantCount = 4
		compileCount = 512
	)
	variants := make([]ActionPlanFamilyVariant, 0, variantCount)
	for index := range variantCount {
		variants = append(variants, ActionPlanFamilyVariant{
			Name: fmt.Sprintf("variant_%d", index),
			Snapshot: familyBenchmarkSnapshotMode(
				b, familyBenchmarkConfig(index, 2048), compileCount, "prior-tree-precise",
			),
		})
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(family.Nodes)), "family-nodes/op")
	b.ResetTimer()
	for range b.N {
		root := b.TempDir()
		outputs := map[string]string{}
		for _, segment := range linuxKernelFamilyPlanSegmentOrder {
			outputs[segment] = filepath.Join(root, segment)
		}
		if err := family.WriteSegments(outputs); err != nil {
			b.Fatal(err)
		}
	}
}
