package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestTerminalImageEntryTargetUsesEvaluatedDemand(t *testing.T) {
	config := CompactConfig{imageTarget: "arch/arm64/boot/Image"}
	got, err := terminalImageEntryTarget(config)
	if err != nil {
		t.Fatal(err)
	}
	if want := "arch/arm64/boot/Image"; got != want {
		t.Fatalf("terminalImageEntryTarget() = %q, want %q", got, want)
	}
}

func TestTerminalImageEntryTargetRejectsMissingRootValue(t *testing.T) {
	config := CompactConfig{}
	if _, err := terminalImageEntryTarget(config); err == nil {
		t.Fatal("terminalImageEntryTarget() accepted missing KBUILD_IMAGE")
	}
}

func TestSelectedKbuildPlanOutputUsesCanonicalBootstrapProjection(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected", Path: "Makefile"}
	selection := CompactKbuildSelection{
		Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "bootstrap",
	}
	config := CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{selection},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "native-bootstrap", Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: "vmlinux"}}},
		{ID: "canonical-projection", Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}}},
	}}
	producer, slot, tree, err := selectedKbuildPlanOutput(plan, config, selections, "vmlinux")
	if err != nil {
		t.Fatal(err)
	}
	if producer != "canonical-projection" || slot != 0 || tree != "vmlinux" {
		t.Fatalf("selected bootstrap product = (%q, %d, %q), want canonical projection", producer, slot, tree)
	}
}

func TestTerminalActionPlanOnlyProjectsSelectedNativeOutputs(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected", Path: "Makefile"}
	config := CompactConfig{
		imageTarget:    "arch/x86/boot/bzImage",
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.builtin.modinfo", MakeTarget: "modules.builtin.modinfo", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.builtin", MakeTarget: "modules.builtin", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "arch/x86/boot/bzImage", MakeTarget: "arch/x86/boot/bzImage", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{
			{ID: "selected-vmlinux", Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}}},
			{ID: "selected-system-map", Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "System.map"}}},
			{ID: "selected-builtin-modinfo", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "modules.builtin.modinfo"}}},
			{ID: "selected-builtin", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "modules.builtin"}}},
			{ID: "selected-image", Outputs: []ActionPlanOutput{{Tree: "image", Path: config.imageTarget}}},
		},
	}
	metadata := &CompactMetadata{Config: config}
	before := len(plan.Nodes)
	if err := metadata.appendTerminalActionPlanNodes(plan, selections); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), before+1; got != want {
		t.Fatalf("terminal nodes = %d, want one stable image projection: %#v", got-before, plan.Nodes[before:])
	}
	projection := plan.Nodes[before]
	if projection.Kind != "copy" || projection.Tool != "actionfile" ||
		len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != "selected-image" ||
		len(projection.Outputs) != 1 || projection.Outputs[0] != (ActionPlanOutput{Tree: "image", Path: "kernel"}) {
		t.Fatalf("image projection = %#v", projection)
	}
	wantProducts := map[string]ActionPlanProduct{
		"vmlinux":                 {Name: "vmlinux", Tree: "vmlinux", Path: "vmlinux"},
		"system_map":              {Name: "system_map", Tree: "vmlinux", Path: "System.map"},
		"modules_builtin":         {Name: "modules_builtin", Tree: "metadata", Path: "modules.builtin"},
		"modules_builtin_modinfo": {Name: "modules_builtin_modinfo", Tree: "metadata", Path: "modules.builtin.modinfo"},
		"image":                   {Name: "image", Tree: "image", Path: "kernel"},
	}
	for _, product := range plan.Products {
		want, ok := wantProducts[product.Name]
		if !ok || product != want {
			t.Fatalf("unexpected terminal product %#v", product)
		}
		delete(wantProducts, product.Name)
	}
	if len(wantProducts) != 0 {
		t.Fatalf("missing terminal products %#v", wantProducts)
	}
}

func TestTerminalActionPlanRejectsArtifactWithoutExactSelectionOwner(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected", Path: "Makefile"}
	config := CompactConfig{
		imageTarget:    "arch/x86/boot/Image",
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "modules.builtin.modinfo", MakeTarget: "modules.builtin.modinfo", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.builtin", MakeTarget: "modules.builtin", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "arch/x86/boot/Image", MakeTarget: "arch/x86/boot/Image", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: "unselected-vmlinux", Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}}},
	}}
	metadata := &CompactMetadata{Config: config}
	err = metadata.appendTerminalActionPlanNodes(plan, selections)
	if err == nil || !strings.Contains(err.Error(), `target "vmlinux" has no selected owner`) {
		t.Fatalf("unselected vmlinux error = %v", err)
	}
}

func TestTerminalActionPlanBindsSystemMapToNearestSourceScript(t *testing.T) {
	profile := CompactKbuildProfile{Name: "selected", Path: "Makefile"}
	config := CompactConfig{
		imageTarget:    "arch/x86/boot/bzImage",
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.builtin.modinfo", MakeTarget: "modules.builtin.modinfo", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "modules.builtin", MakeTarget: "modules.builtin", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: configImageTargetForTest, MakeTarget: configImageTargetForTest, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	config.imageTarget = configImageTargetForTest
	selections, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	scriptRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{"--"}, WorkingDirectory: "link",
		WorkingOutputs: map[string]string{"00000000": "vmlinux.unstripped"},
		Outputs:        []string{"00000000"},
	}
	scriptRecipeID, err := scriptRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	closureRecipe := scriptRecipe
	closureRecipe.WorkingDirectory = "unrelated"
	closureRecipe.WorkingOutputs = map[string]string{"00000000": "unrelated.generated"}
	closureRecipeID, err := closureRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	toolRecipe := scriptRecipe
	toolRecipe.WorkingDirectory = "resolve-btfids"
	toolRecipe.WorkingOutputs = map[string]string{"00000000": "tools/bpf/resolve_btfids/resolve_btfids"}
	toolRecipeID, err := toolRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{
			scriptRecipeID:  scriptRecipe,
			closureRecipeID: closureRecipe,
			toolRecipeID:    toolRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "link-script", Recipe: scriptRecipeID,
				Inputs:  []ActionPlanNodeEdge{{Role: "prerequisite", ProducerID: "resolve-btfids"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "vmlinux.unstripped"}},
			},
			{ID: "resolve-btfids", Recipe: toolRecipeID, Outputs: []ActionPlanOutput{{Tree: "host", Path: "tools/bpf/resolve_btfids/resolve_btfids"}}},
			{ID: "unrelated-script", Recipe: closureRecipeID, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "unrelated.generated"}}},
			{ID: "selected-vmlinux", Inputs: []ActionPlanNodeEdge{
				{Role: "object", ProducerID: "link-script"},
				{Role: "prerequisite", ProducerID: "resolve-btfids"},
				{Role: compactKbuildWorkingClosureInputRole, ProducerID: "unrelated-script"},
			}, Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}}},
			{ID: "selected-builtin-modinfo", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "modules.builtin.modinfo"}}},
			{ID: "selected-builtin", Outputs: []ActionPlanOutput{{Tree: "metadata", Path: "modules.builtin"}}},
			{ID: "selected-image", Outputs: []ActionPlanOutput{{Tree: "image", Path: config.imageTarget}}},
		},
	}
	metadata := &CompactMetadata{Config: config}
	if err := metadata.appendTerminalActionPlanNodes(plan, selections); err != nil {
		t.Fatal(err)
	}
	producer, slot, ok := planProducerByOutput(plan, "vmlinux", "System.map")
	if !ok || producer != "link-script" || slot != 1 {
		t.Fatalf("System.map producer = (%q,%d,%t), want link-script slot 1", producer, slot, ok)
	}
	recipe := plan.Recipes[plan.Nodes[0].Recipe]
	if got, want := recipe.WorkingOutputs["00000001"], "System.map"; got != want {
		t.Fatalf("source-script System.map working output = %q, want %q", got, want)
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("extended source-script recipe is invalid: %v", err)
	}
	originalRecipe := plan.Recipes[scriptRecipeID]
	if _, mutated := originalRecipe.WorkingOutputs["00000001"]; mutated {
		t.Fatalf("content-addressed source-script recipe %s was mutated in place: %#v", scriptRecipeID, originalRecipe)
	}
	if got, want := originalRecipe.Outputs, []string{"00000000"}; !slices.Equal(got, want) {
		t.Fatalf("original source-script outputs = %q, want %q", got, want)
	}
	if err := originalRecipe.Validate(); err != nil {
		t.Fatalf("original source-script recipe was corrupted: %v", err)
	}
	for recipeID, stored := range plan.Recipes {
		if got, err := stored.ID(); err != nil || got != recipeID {
			t.Fatalf("stored recipe %s has identity (%s,%v)", recipeID, got, err)
		}
	}
}

const configImageTargetForTest = "arch/x86/boot/bzImage"

func TestTerminalNativeInputProducersIgnoreWorkingClosure(t *testing.T) {
	node := ActionPlanNode{Inputs: []ActionPlanNodeEdge{
		{Role: "object", ProducerID: "used"},
		{Role: compactKbuildWorkingClosureInputRole, ProducerID: "closure-only"},
		{Role: "order-only", ProducerID: "ordered-only"},
		{Role: "sequence", ProducerID: "sequenced"},
	}}
	if got, want := terminalNativeInputProducers(node), []string{"used", "sequenced"}; !slices.Equal(got, want) {
		t.Fatalf("terminal native producers = %q, want Make/sequence inputs %q", got, want)
	}
}
