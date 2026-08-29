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

func TestTerminalActionPlanBindsSystemMapToSelectedAtomicSourceScript(t *testing.T) {
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
		WorkingOutputs: map[string]string{"00000000": "vmlinux"},
		Inputs:         []string{"program:00000000"},
		ExecutableInputs: []string{
			"program:00000000",
		},
		Outputs: []string{"00000000"},
	}
	scriptRecipeID, err := scriptRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	closureRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{"--"}, WorkingDirectory: "unrelated",
		WorkingOutputs: map[string]string{"00000000": "unrelated.generated"},
		Outputs:        []string{"00000000"},
	}
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
	programRecipe := immutableSourceExecutableProjectionRecipe()
	programRecipeID, err := programRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	const programSourceID = "src-00000001"
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: programSourceID, Namespace: "kernel", Path: "scripts/link-vmlinux.sh",
		}},
		Recipes: map[string]ActionRecipe{
			scriptRecipeID:  scriptRecipe,
			closureRecipeID: closureRecipe,
			toolRecipeID:    toolRecipe,
			programRecipeID: programRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "link-script", Recipe: scriptRecipeID,
				Inputs: []ActionPlanNodeEdge{
					{Role: "program", ProducerID: "link-program"},
					{Role: "object", ProducerID: "export-object"},
					{Role: "object", ProducerID: "version-timestamp"},
					{Role: "prerequisite", ProducerID: "resolve-btfids"},
					{Role: compactKbuildWorkingClosureInputRole, ProducerID: "unrelated-script"},
				},
				Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
			},
			{
				ID: "link-program", Stage: "host", Kind: "copy", Tool: "actionfile", Product: "sdk",
				Recipe:  programRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "program", SourceID: programSourceID}},
				Outputs: []ActionPlanOutput{{Tree: "host", Path: "scripts/link-vmlinux.sh"}},
			},
			{ID: "resolve-btfids", Recipe: toolRecipeID, Outputs: []ActionPlanOutput{{Tree: "host", Path: "tools/bpf/resolve_btfids/resolve_btfids"}}},
			{ID: "export-object", Recipe: closureRecipeID, Outputs: []ActionPlanOutput{{Tree: "objects", Path: ".vmlinux.export.o"}}},
			{ID: "version-timestamp", Recipe: closureRecipeID, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "init/version-timestamp.o"}}},
			{ID: "unrelated-script", Recipe: closureRecipeID, Outputs: []ActionPlanOutput{{Tree: "objects", Path: "unrelated.generated"}}},
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

func TestNearestSourceScriptSideOutputRejectsSourceTaggedGeneratedExecutable(t *testing.T) {
	const sourceID = "src-00000001"
	programRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{
			"-c", "${source:program:00000000}", "-o", "${output:00000000}",
		},
		Sources: []string{"program:00000000"}, Outputs: []string{"00000000"},
	}
	programRecipeID, err := programRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	scriptRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{"--"}, WorkingDirectory: "link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux"},
		Inputs:           []string{"program:00000000"},
		ExecutableInputs: []string{"program:00000000"},
		Outputs:          []string{"00000000"},
	}
	scriptRecipeID, err := scriptRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: sourceID, Namespace: "kernel", Path: "scripts/generated-linker",
		}},
		Recipes: map[string]ActionRecipe{
			programRecipeID: programRecipe,
			scriptRecipeID:  scriptRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "selected-vmlinux", Stage: "target", Kind: "generate", Tool: "scriptrun", Recipe: scriptRecipeID,
				Inputs:  []ActionPlanNodeEdge{{Role: "program", ProducerID: "generated-linker"}},
				Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
			},
			{
				ID: "generated-linker", Stage: "host", Kind: "compile", Tool: "cc", Product: "sdk", Recipe: programRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "program", SourceID: sourceID}},
				Outputs: []ActionPlanOutput{{Tree: "host", Path: "scripts/generated-linker"}},
			},
		},
	}
	err = appendNearestSourceScriptSideOutput(plan, "selected-vmlinux", ActionPlanOutput{
		Tree: "vmlinux", Path: "System.map",
	})
	if err == nil || !strings.Contains(err.Error(), "terminal dependency chain has no source-owned script action") {
		t.Fatalf("source-tagged generated executable error = %v, want missing source-script producer", err)
	}
	if producer, slot, exists := planProducerByOutput(plan, "vmlinux", "System.map"); exists {
		t.Fatalf("source-tagged generated executable acquired System.map producer (%q,%d)", producer, slot)
	}
	if plan.Nodes[0].Recipe != scriptRecipeID || len(plan.Nodes[0].Outputs) != 1 {
		t.Fatalf("failed System.map lookup mutated terminal node: %#v", plan.Nodes[0])
	}
}

func TestActionPlanNodeExecutesImmutableSourceScriptRequiresExactScriptOrdinal(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:        []string{"--", "${source:script:00000000}"},
		Sources:          []string{"script:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux"},
	}
	node := ActionPlanNode{Sources: []ActionPlanSourceEdge{
		{Role: "data", SourceID: "src-00000001"},
		{Role: "script", SourceID: "src-00000002"},
	}}
	plan := &ActionPlan{Nodes: []ActionPlanNode{node}}
	if actionPlanNodeExecutesImmutableSourceScript(plan, map[string]int{}, node, recipe) {
		t.Fatal("source script with a mismatched recipe ordinal was accepted")
	}
}

func TestNearestSourceScriptSideOutputCrossesNativePostLinkAction(t *testing.T) {
	linkRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:        []string{"--", "${source:script:00000000}"},
		Sources:          []string{"script:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux.unstripped"},
	}
	linkRecipeID, err := linkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	compoundRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{"--"}, Outputs: []string{"00000000"},
		WorkingDirectory: "compound",
		WorkingOutputs:   map[string]string{"00000000": ".vmlinux.export.o"},
	}
	compoundRecipeID, err := compoundRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	postLinkRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy",
		Arguments: []string{"--strip-relocs", "${input:object:00000000}", "${output:00000000}"},
		Inputs:    []string{"object:00000000"}, Outputs: []string{"00000000"},
	}
	postLinkRecipeID, err := postLinkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: "src-00000001", Namespace: "kernel", Path: "scripts/link-vmlinux.sh",
		}},
		Recipes: map[string]ActionRecipe{
			linkRecipeID:     linkRecipe,
			compoundRecipeID: compoundRecipe,
			postLinkRecipeID: postLinkRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "link-script", Recipe: linkRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "script", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "vmlinux.unstripped"}},
			},
			{ID: "export-object", Recipe: compoundRecipeID, Outputs: []ActionPlanOutput{{Tree: "objects", Path: ".vmlinux.export.o"}}},
			{
				ID: "selected-vmlinux", Recipe: postLinkRecipeID,
				Inputs: []ActionPlanNodeEdge{
					{Role: "object", ProducerID: "link-script"},
					{Role: "prerequisite", ProducerID: "export-object"},
				},
				Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
			},
		},
	}
	if err := appendNearestSourceScriptSideOutput(plan, "selected-vmlinux", ActionPlanOutput{
		Tree: "vmlinux", Path: "System.map",
	}); err != nil {
		t.Fatal(err)
	}
	producer, slot, ok := planProducerByOutput(plan, "vmlinux", "System.map")
	if !ok || producer != "link-script" || slot != 1 {
		t.Fatalf("System.map producer = (%q,%d,%t), want upstream link-script slot 1", producer, slot, ok)
	}
}

func TestNearestSourceScriptSideOutputCrossesSelectedOverwriteAction(t *testing.T) {
	linkRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:        []string{"--", "${source:script:00000000}"},
		Sources:          []string{"script:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux"},
	}
	linkRecipeID, err := linkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	postLinkRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy",
		Arguments: []string{
			"--remove-section=.rela.*", "${input:overwrite:00000000}",
		},
		Inputs:           []string{"overwrite:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "post-link",
		WorkingInputs:    map[string]string{"input:overwrite:00000000": "vmlinux"},
		WorkingOutputs:   map[string]string{"00000000": "vmlinux"},
	}
	postLinkRecipeID, err := postLinkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: "src-00000001", Namespace: "kernel", Path: "scripts/link-vmlinux.sh",
		}},
		Recipes: map[string]ActionRecipe{
			linkRecipeID:     linkRecipe,
			postLinkRecipeID: postLinkRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "link-script", Recipe: linkRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "script", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "vmlinux"}},
			},
			{
				ID: "selected-vmlinux", Recipe: postLinkRecipeID,
				Inputs: []ActionPlanNodeEdge{{
					Role: compactKbuildOverwriteInputRole, ProducerID: "link-script",
				}},
				Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
			},
		},
	}
	if err := appendNearestSourceScriptSideOutput(plan, "selected-vmlinux", ActionPlanOutput{
		Tree: "vmlinux", Path: "System.map",
	}); err != nil {
		t.Fatal(err)
	}
	producer, slot, ok := planProducerByOutput(plan, "vmlinux", "System.map")
	if !ok || producer != "link-script" || slot != 1 {
		t.Fatalf("System.map producer = (%q,%d,%t), want selected overwrite predecessor slot 1", producer, slot, ok)
	}
}

func TestNearestSourceScriptSideOutputSkipsImmutablePostLinkWrapper(t *testing.T) {
	linkRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:        []string{"--", "${source:script:00000000}"},
		Sources:          []string{"script:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux.unstripped"},
	}
	linkRecipeID, err := linkRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	wrapperRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:        []string{"--", "${source:script:00000000}", "${input:object:00000000}"},
		Sources:          []string{"script:00000000"},
		Inputs:           []string{"object:00000000"},
		Outputs:          []string{"00000000"},
		WorkingDirectory: "post-link",
		WorkingOutputs:   map[string]string{"00000000": "vmlinux"},
	}
	wrapperRecipeID, err := wrapperRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "scripts/link-vmlinux.sh"},
			{ID: "src-00000002", Namespace: "kernel", Path: "scripts/post-link-wrapper.sh"},
		},
		Recipes: map[string]ActionRecipe{
			linkRecipeID:    linkRecipe,
			wrapperRecipeID: wrapperRecipe,
		},
		Nodes: []ActionPlanNode{
			{
				ID: "selected-vmlinux", Recipe: wrapperRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "script", SourceID: "src-00000002"}},
				Inputs:  []ActionPlanNodeEdge{{Role: "object", ProducerID: "link-script"}},
				Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux"}},
			},
			{
				ID: "link-script", Recipe: linkRecipeID,
				Sources: []ActionPlanSourceEdge{{Role: "script", SourceID: "src-00000001"}},
				Outputs: []ActionPlanOutput{{Tree: "objects", Path: "vmlinux.unstripped"}},
			},
		},
	}
	if err := appendNearestSourceScriptSideOutput(plan, "selected-vmlinux", ActionPlanOutput{
		Tree: "vmlinux", Path: "System.map",
	}); err != nil {
		t.Fatal(err)
	}
	producer, slot, ok := planProducerByOutput(plan, "vmlinux", "System.map")
	if !ok || producer != "link-script" || slot != 1 {
		t.Fatalf("System.map producer = (%q,%d,%t), want upstream link-vmlinux script slot 1", producer, slot, ok)
	}
	if len(plan.Nodes[0].Outputs) != 1 {
		t.Fatalf("post-link wrapper acquired System.map output: %#v", plan.Nodes[0])
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
