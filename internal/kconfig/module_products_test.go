package kconfig

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func TestSelectedModuleProductsUseOnlySelectedKbuildOutputs(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "Module.symvers", MakeTarget: "Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "arch/x86/modules.order", MakeTarget: "arch/x86/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "drivers/zeta.ko", MakeTarget: "drivers/zeta.ko", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "drivers/alpha.ko", MakeTarget: "drivers/alpha.ko", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		Config: config,
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{
			{ID: "symvers", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "Module.symvers"}}},
			{ID: "arch-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "arch/x86/modules.order"}}},
			{
				ID:      "order",
				Inputs:  []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "arch-order"}},
				Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}},
			},
			{ID: "zeta", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "drivers/zeta.ko"}}},
			{ID: "alpha", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "drivers/alpha.ko"}}},
		},
	}

	if err := metadata.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		t.Fatal(err)
	}

	wantProducts := []ActionPlanProduct{
		{Name: "module_symvers", Tree: "metadata", Path: "Module.symvers"},
		{Name: "modules_order", Tree: "modules", Path: "modules.order"},
		{Name: "modules", Tree: "modules", Path: LinuxKernelTreeRootMarker},
	}
	for _, want := range wantProducts {
		if !slices.Contains(plan.Products, want) {
			t.Errorf("products = %#v, want %#v", plan.Products, want)
		}
	}

	_, _, ok := planProducerByOutput(plan, "metadata", "modules.manifest")
	if !ok {
		t.Fatal("selected module products omit metadata/modules.manifest")
	}
	var manifest ActionRecipe
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output == (ActionPlanOutput{Tree: "metadata", Path: "modules.manifest"}) {
				manifest = plan.Recipes[node.Recipe]
			}
		}
	}
	if len(manifest.Arguments) != 4 {
		t.Fatalf("manifest recipe = %#v", manifest)
	}
	contents, err := base64.StdEncoding.DecodeString(manifest.Arguments[3])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "drivers/alpha.ko\ndrivers/zeta.ko\n"; got != want {
		t.Fatalf("modules manifest = %q, want %q", got, want)
	}
}

func TestSelectedModuleProductsPreserveExternalKbuildDirectory(t *testing.T) {
	const prefix = ".linux-bzl/external/demo"
	profile := CompactKbuildProfile{Name: "external:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: prefix + "/Module.symvers", MakeTarget: prefix + "/Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: prefix + "/subdir/modules.order", MakeTarget: prefix + "/subdir/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: prefix + "/modules.order", MakeTarget: prefix + "/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: prefix + "/demo.ko", MakeTarget: prefix + "/demo.ko", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config, selectedProductsOnly: true}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{
			{ID: "symvers", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: prefix + "/Module.symvers"}}},
			{ID: "subdir-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: prefix + "/subdir/modules.order"}}},
			{
				ID:      "order",
				Inputs:  []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "subdir-order"}},
				Outputs: []ActionPlanOutput{{Tree: "modules", Path: prefix + "/modules.order"}},
			},
			{ID: "module", Outputs: []ActionPlanOutput{{Tree: "modules", Path: prefix + "/demo.ko"}}},
		},
	}
	if err := metadata.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		t.Fatal(err)
	}
	for _, want := range []ActionPlanProduct{
		{Name: "module_symvers", Tree: "metadata", Path: prefix + "/Module.symvers"},
		{Name: "modules_order", Tree: "modules", Path: prefix + "/modules.order"},
		{Name: "modules", Tree: "modules", Path: LinuxKernelTreeRootMarker},
	} {
		if !slices.Contains(plan.Products, want) {
			t.Errorf("products = %#v, want %#v", plan.Products, want)
		}
	}
}

func TestSelectedModuleProductTargetRejectsIndependentAggregates(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: ".linux-bzl/external/demo/modules.order", MakeTarget: ".linux-bzl/external/demo/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "kernel-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}}},
		{ID: "external-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: ".linux-bzl/external/demo/modules.order"}}},
	}}
	_, _, err = selectedModuleProductTarget(plan, config, selections, "modules.order")
	if err == nil || !strings.Contains(err.Error(), "independent downstream modules.order products") ||
		!strings.Contains(err.Error(), `"modules.order"`) ||
		!strings.Contains(err.Error(), `".linux-bzl/external/demo/modules.order"`) {
		t.Fatalf("independent module aggregates error = %v", err)
	}
}

func TestSelectedModuleProductTargetIgnoresOrderingAndStateEdges(t *testing.T) {
	for _, role := range []string{
		compactKbuildWorkingClosureInputRole,
		compactKbuildOverwriteInputRole,
		"sequence",
		"order-only",
	} {
		t.Run(role, func(t *testing.T) {
			profile := CompactKbuildProfile{Name: "selected:modules"}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{profile},
				KbuildSelections: []CompactKbuildSelection{
					{Profile: profile.Name, Target: "drivers/a/modules.order", MakeTarget: "drivers/a/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
					{Profile: profile.Name, Target: "net/b/modules.order", MakeTarget: "net/b/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
				},
			}
			selections, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{Nodes: []ActionPlanNode{
				{ID: "drivers-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "drivers/a/modules.order"}}},
				{
					ID: "net-order",
					Inputs: []ActionPlanNodeEdge{{
						Role: role, ProducerID: "drivers-order",
					}},
					Outputs: []ActionPlanOutput{{Tree: "modules", Path: "net/b/modules.order"}},
				},
			}}
			_, _, err = selectedModuleProductTarget(plan, config, selections, "modules.order")
			if err == nil || !strings.Contains(err.Error(), "independent downstream modules.order products") {
				t.Fatalf("%s-only module aggregates error = %v", role, err)
			}
		})
	}
}

func TestSelectedModuleProductTargetRequiresEveryNativeCandidate(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "arch/x86/modules.order", MakeTarget: "arch/x86/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "kernel-order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}}},
	}}
	_, _, err = selectedModuleProductTarget(plan, config, selections, "modules.order")
	if err == nil || !strings.Contains(err.Error(), "arch/x86/modules.order") ||
		!strings.Contains(err.Error(), "has no native output modules/arch/x86/modules.order") {
		t.Fatalf("missing nested native module aggregate error = %v", err)
	}
}

func TestSelectedModuleProductTargetKeepsDependentModuleSymversAmbiguous(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:module-symvers"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "Module.symvers", MakeTarget: "Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: ".linux-bzl/external/demo/Module.symvers", MakeTarget: ".linux-bzl/external/demo/Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "kernel-symvers", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "Module.symvers"}}},
		{
			ID: "external-symvers",
			Inputs: []ActionPlanNodeEdge{{
				Role: "prerequisite", ProducerID: "kernel-symvers",
			}},
			Outputs: []ActionPlanOutput{{Tree: "metadata", Path: ".linux-bzl/external/demo/Module.symvers"}},
		},
	}}
	_, _, err = selectedModuleProductTarget(plan, config, selections, "Module.symvers")
	if err == nil || !strings.Contains(err.Error(), "ambiguous Module.symvers products") {
		t.Fatalf("dependent Module.symvers products error = %v", err)
	}
}

func TestSelectedModuleProductTargetTracksGroupedOutputSlots(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:grouped-orders"}
	for _, test := range []struct {
		name          string
		selections    []string
		outputs       []ActionPlanOutput
		inputs        []ActionPlanNodeEdge
		want          string
		wantError     bool
		wantDetails   []string
		rejectDetails []string
	}{
		{
			name:       "unrelated grouped slot does not aggregate candidate",
			selections: []string{"drivers/modules.order", "modules.order"},
			outputs: []ActionPlanOutput{
				{Tree: "modules", Path: "drivers/modules.order"},
				{Tree: "objects", Path: "generated.marker"},
			},
			inputs:      []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "grouped", Slot: 1}},
			wantError:   true,
			wantDetails: []string{`"drivers/modules.order"`, `"modules.order"`},
		},
		{
			name:       "one grouped aggregate remains independent",
			selections: []string{"drivers/modules.order", "net/modules.order", "modules.order"},
			outputs: []ActionPlanOutput{
				{Tree: "modules", Path: "drivers/modules.order"},
				{Tree: "modules", Path: "net/modules.order"},
			},
			inputs:        []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "grouped", Slot: 0}},
			wantError:     true,
			wantDetails:   []string{`"net/modules.order"`, `"modules.order"`},
			rejectDetails: []string{`"drivers/modules.order"`},
		},
		{
			name:       "both grouped aggregates feed root",
			selections: []string{"drivers/modules.order", "net/modules.order", "modules.order"},
			outputs: []ActionPlanOutput{
				{Tree: "modules", Path: "drivers/modules.order"},
				{Tree: "modules", Path: "net/modules.order"},
			},
			inputs: []ActionPlanNodeEdge{
				{Role: "prerequisite", ProducerID: "grouped", Slot: 0},
				{Role: "prerequisite", ProducerID: "grouped", Slot: 1},
			},
			want: "modules.order",
		},
		{
			name:       "two products from one frontier producer remain ambiguous",
			selections: []string{"drivers/modules.order", "net/modules.order"},
			outputs: []ActionPlanOutput{
				{Tree: "modules", Path: "drivers/modules.order"},
				{Tree: "modules", Path: "net/modules.order"},
			},
			wantError:   true,
			wantDetails: []string{`"drivers/modules.order"`, `"net/modules.order"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
			for _, target := range test.selections {
				config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
					Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target",
				})
			}
			selections, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{Nodes: []ActionPlanNode{{ID: "grouped", Outputs: test.outputs}}}
			if slices.Contains(test.selections, "modules.order") {
				plan.Nodes = append(plan.Nodes, ActionPlanNode{
					ID: "root", Inputs: test.inputs,
					Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}},
				})
			}
			got, selected, err := selectedModuleProductTarget(plan, config, selections, "modules.order")
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "independent downstream modules.order products") {
					t.Fatalf("grouped output error = %v", err)
				}
				for _, detail := range test.wantDetails {
					if !strings.Contains(err.Error(), detail) {
						t.Errorf("grouped output error = %v, want detail %s", err, detail)
					}
				}
				for _, detail := range test.rejectDetails {
					if strings.Contains(err.Error(), detail) {
						t.Errorf("grouped output error = %v, unexpectedly contains consumed detail %s", err, detail)
					}
				}
				return
			}
			if err != nil || !selected || got != test.want {
				t.Fatalf("selected grouped product = (%q,%t,%v), want (%q,true,nil)", got, selected, err, test.want)
			}
		})
	}
}

func TestSelectedModuleProductTargetFollowsTransitiveAggregateInputs(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:transitive-orders"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "drivers/modules.order", MakeTarget: "drivers/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "drivers", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "drivers/modules.order"}}},
		{
			ID: "manifest", Inputs: []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "drivers"}},
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "generated/module-manifest"}},
		},
		{
			ID: "root", Inputs: []ActionPlanNodeEdge{{Role: "stdin", ProducerID: "manifest"}},
			Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}},
		},
	}}
	got, selected, err := selectedModuleProductTarget(plan, config, selections, "modules.order")
	if err != nil || !selected || got != "modules.order" {
		t.Fatalf("transitive module aggregate = (%q,%t,%v), want root product", got, selected, err)
	}
}

func TestNativeModuleProductOutputFrontierValidatesReferences(t *testing.T) {
	node := ActionPlanNode{ID: "producer", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}}}
	for _, test := range []struct {
		name    string
		nodes   []ActionPlanNode
		indexes map[string]int
		output  actionPlanOutputRef
		want    string
	}{
		{
			name: "missing producer", indexes: map[string]int{},
			output: actionPlanOutputRef{producerID: "missing"}, want: `dependency "missing" is absent`,
		},
		{
			name: "invalid output slot", nodes: []ActionPlanNode{node}, indexes: map[string]int{"producer": 0},
			output: actionPlanOutputRef{producerID: "producer", slot: 1}, want: "output slot 1 is outside its 1 outputs",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := nativeModuleProductOutputFrontier(test.nodes, test.indexes, []actionPlanOutputRef{test.output})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("frontier validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSelectedModuleProductTargetIgnoresOrderingEdges(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:ordered-modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "drivers/modules.order", MakeTarget: "drivers/modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"sequence", "order-only"} {
		t.Run(role, func(t *testing.T) {
			plan := &ActionPlan{Nodes: []ActionPlanNode{
				{ID: "drivers", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "drivers/modules.order"}}},
				{
					ID: "root", Inputs: []ActionPlanNodeEdge{{Role: role, ProducerID: "drivers"}},
					Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}},
				},
			}}
			_, _, err := selectedModuleProductTarget(plan, config, selections, "modules.order")
			if err == nil || !strings.Contains(err.Error(), "independent downstream modules.order products") {
				t.Fatalf("%s-only aggregate edge error = %v", role, err)
			}
		})
	}
}

func TestSelectedModuleProductsRequireGenericNativeOutputs(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "Module.symvers", MakeTarget: "Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "drivers/demo.ko", MakeTarget: "drivers/demo.ko", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "symvers", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "Module.symvers"}}},
		{ID: "order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}}},
	}}

	err = metadata.appendSelectedModuleActionPlanProducts(plan, selections)
	if err == nil || !strings.Contains(err.Error(), `selected owner (selected:modules, drivers/demo.ko, target) has no native output modules/drivers/demo.ko`) {
		t.Fatalf("missing generic .ko output error = %v", err)
	}
}

func TestSelectedModuleProductsWriteEmptyManifestWhenNoKOIsSelected(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected:modules"}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "Module.symvers", MakeTarget: "Module.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.order", MakeTarget: "modules.order", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "symvers", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "Module.symvers"}}},
		{ID: "order", Outputs: []ActionPlanOutput{{Tree: "modules", Path: "modules.order"}}},
	}}

	if err := metadata.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output != (ActionPlanOutput{Tree: "metadata", Path: "modules.manifest"}) {
				continue
			}
			encoded := plan.Recipes[node.Recipe].Arguments[3]
			contents, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) != 0 {
				t.Fatalf("empty modules manifest = %q", contents)
			}
			return
		}
	}
	t.Fatal("selected module products omit empty manifest")
}

func TestSelectedModuleProductsProjectEmptyFacadeWhenKbuildSelectsNoModules(t *testing.T) {
	profile := CompactKbuildProfile{Name: "modules-disabled"}
	config := CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}
	metadata := &CompactMetadata{Config: config}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := metadata.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		t.Fatal(err)
	}
	for _, output := range []ActionPlanOutput{
		{Tree: "metadata", Path: "Module.symvers"},
		{Tree: "modules", Path: "modules.order"},
	} {
		producer, _, ok := planProducerByOutput(plan, output.Tree, output.Path)
		if !ok {
			t.Fatalf("empty module facade omits %#v", output)
		}
		node, ok := compactKbuildPlanNode(plan, producer)
		if !ok || node.Tool != "actionfile" {
			t.Fatalf("empty module facade producer = %#v, want actionfile", node)
		}
	}
}
