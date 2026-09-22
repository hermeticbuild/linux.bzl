package kconfig

import (
	"strings"
	"testing"
)

func TestModuleSDKProjectsOnlyPreparationClosure(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	prehost := seedModuleSDKPlanOutputForTest(t, plan, "prehost", "prehost", "scripts/basic/fixdep", "sdk")
	prepFixdep := seedDependentModuleSDKPlanArtifactForTest(
		t, plan, "prep", ActionPlanOutput{Tree: "prep", Path: "scripts/basic/fixdep"}, "sdk", prehost,
	)
	seedModuleSDKPlanOutputForTest(t, plan, "prehost", "prehost", "scripts/mod/modpost", "sdk")
	prep := seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", "scripts/mod/modpost", "sdk")
	seedModuleSDKPlanOutputForTest(t, plan, "host", "host", "scripts/mod/modpost", "sdk")
	seedModuleSDKPlanOutputForTest(t, plan, "host", "host", "scripts/target-lifecycle-only", "vmlinux")
	generated := seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", "include/generated/autoconf.h", "sdk")
	moduleLinkerScript := seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", "scripts/module.lds", "sdk")
	release := seedModuleSDKReleaseForTest(t, plan)
	seedModuleSDKPlanOutputForTest(t, plan, "target", "objects", "vmlinux.o", "vmlinux")
	persistentMetadata := seedModuleSDKPlanArtifactForTest(t, plan, "prep", ActionPlanOutput{
		Tree: "prep", Path: "rust/libkernel.rmeta",
		ArtifactPath: ".linux-bzl-intermediate/rust/libkernel.rmeta", persistent: true,
	}, "sdk")
	hostPersistent := seedModuleSDKPlanArtifactForTest(t, plan, "host", ActionPlanOutput{
		Tree: "host", Path: "scripts/host-persistent.o",
		ArtifactPath: ".linux-bzl-versions/host-persistent/scripts/host-persistent.o", persistent: true,
	}, "sdk")
	if err := appendCompactKbuildHostPrepMirrors(plan, CompactKbuildSelection{
		Profile: "source-host-profile", Target: "scripts/host-persistent.o", MakeTarget: "scripts/host-persistent.o",
		Lifecycle: "prep", Scope: "host", Stage: "host",
	}, hostPersistent.producer); err != nil {
		t.Fatal(err)
	}
	var mirroredPersistent moduleSDKSelectedArtifact
	for _, node := range plan.Nodes {
		if node.Stage != "prep" || len(node.Inputs) != 1 || node.Inputs[0].ProducerID != hostPersistent.producer ||
			len(node.Outputs) != 1 || node.Outputs[0].Path != hostPersistent.path {
			continue
		}
		if !node.Outputs[0].persistent || node.Outputs[0].ArtifactPath != ".linux-bzl-versions/host-persistent/scripts/host-persistent.o" {
			t.Fatalf("private persistent host mirror = %#v, want exact native output version and SDK membership", node)
		}
		mirroredPersistent = moduleSDKSelectedArtifact{producer: node.ID, slot: 0, tree: "prep", path: hostPersistent.path}
	}
	if mirroredPersistent.producer == "" {
		t.Fatal("source-selected private persistent host output has no prep mirror")
	}
	seedModuleSDKPlanArtifactForTest(t, plan, "prep", ActionPlanOutput{
		Tree: "prep", Path: "rust/private-scratch",
		ArtifactPath: ".linux-bzl-intermediate/rust/private-scratch",
	}, "sdk")
	symvers := seedModuleSDKPlanOutputForTest(t, plan, "target", "metadata", "Module.symvers", "module_symvers")
	plan.Products = append(plan.Products, ActionPlanProduct{
		Name: "module_symvers", Tree: "metadata", Path: "Module.symvers",
	})
	vmlinux := seedModuleSDKVmlinuxForTest(t, plan)

	if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatalf("appendModuleSDKActionPlanNodes() failed: %v", err)
	}
	for destination, source := range map[string]moduleSDKSelectedArtifact{
		"include/config/kernel.release": release,
		"scripts/basic/fixdep":          prepFixdep,
		"scripts/mod/modpost":           prep,
		"include/generated/autoconf.h":  generated,
		"scripts/module.lds":            moduleLinkerScript,
		"rust/libkernel.rmeta":          persistentMetadata,
		"scripts/host-persistent.o":     mirroredPersistent,
		"Module.symvers":                symvers,
		"vmlinux":                       vmlinux,
	} {
		projection := moduleSDKProjectionNodeForTest(t, plan, destination)
		if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != source.producer {
			t.Errorf("sdk/%s projection input = %#v, want producer %s", destination, projection.Inputs, source.producer)
		}
	}
	if _, _, ok := planProducerByOutput(plan, "sdk", "rust/private-scratch"); ok {
		t.Fatal("module SDK projected an untagged private scratch output")
	}
	if _, _, ok := planProducerByOutput(plan, "sdk", "vmlinux.o"); ok {
		t.Fatal("module SDK projected an unrelated target-lifecycle output")
	}
	if _, _, ok := planProducerByOutput(plan, "sdk", "scripts/target-lifecycle-only"); ok {
		t.Fatal("module SDK projected a naked target-lifecycle host output")
	}
	if _, _, ok := planProducerByOutput(plan, "sdk", "linux-bzl-sdk.json"); ok {
		t.Fatal("generic module SDK unexpectedly serializes an external-module recipe manifest")
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("projected action plan is invalid: %v", err)
	}
}

func TestModuleSDKRequiresNativeModuleSymversProduct(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", ".config", "sdk")
	seedModuleSDKReleaseForTest(t, plan)
	before := len(plan.Nodes)
	err := metadata.appendModuleSDKActionPlanNodes(plan)
	if err == nil || !strings.Contains(err.Error(), `requires selected product "module_symvers"`) {
		t.Fatalf("error = %v, want missing module_symvers product", err)
	}
	if got := len(plan.Nodes); got != before {
		t.Fatalf("failed SDK projection added %d nodes", got-before)
	}
}

func TestModuleSDKRequiresNativeVmlinuxProduct(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", ".config", "sdk")
	seedModuleSDKReleaseForTest(t, plan)
	seedModuleSDKSymversForTest(t, plan)
	before := len(plan.Nodes)
	err := metadata.appendModuleSDKActionPlanNodes(plan)
	if err == nil || !strings.Contains(err.Error(), `requires selected product "vmlinux"`) {
		t.Fatalf("error = %v, want missing vmlinux product", err)
	}
	if got := len(plan.Nodes); got != before {
		t.Fatalf("failed SDK projection added %d nodes", got-before)
	}
}

func TestModuleSDKRequiresSourceSelectedKernelReleaseWriter(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	before := len(plan.Nodes)
	err := metadata.appendModuleSDKActionPlanNodes(plan)
	if err == nil || !strings.Contains(err.Error(), "source-selected Kbuild writer for include/config/kernel.release") {
		t.Fatalf("missing source-owned kernel release writer error = %v", err)
	}
	if got := len(plan.Nodes); got != before {
		t.Fatalf("failed kernel release SDK projection added %d nodes", got-before)
	}
}

func TestModuleSDKUsesDependencyOrderForCanonicalAndPrivatePersistentVersions(t *testing.T) {
	const destination = "rust/libkernel.rmeta"
	for _, test := range []struct {
		name             string
		canonicalFirst   bool
		wantCanonicalSDK bool
	}{
		{name: "later canonical", wantCanonicalSDK: true},
		{name: "later private persistent", canonicalFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := &CompactMetadata{Config: CompactConfig{}}
			plan := &ActionPlan{
				Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
				Recipes:  map[string]ActionRecipe{},
			}
			privateOutput := ActionPlanOutput{
				Tree: "prep", Path: destination,
				ArtifactPath: ".linux-bzl-intermediate/rust/libkernel.rmeta", persistent: true,
			}
			var canonical, private moduleSDKSelectedArtifact
			if test.canonicalFirst {
				canonical = seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", destination, "sdk")
				private = seedDependentModuleSDKPlanArtifactForTest(t, plan, "prep", privateOutput, "sdk", canonical)
			} else {
				private = seedModuleSDKPlanArtifactForTest(t, plan, "prep", privateOutput, "sdk")
				canonical = seedDependentModuleSDKPlanArtifactForTest(
					t, plan, "prep", ActionPlanOutput{Tree: "prep", Path: destination}, "sdk", private,
				)
			}
			seedModuleSDKSymversForTest(t, plan)
			seedModuleSDKVmlinuxForTest(t, plan)
			seedModuleSDKReleaseForTest(t, plan)

			if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
				t.Fatalf("appendModuleSDKActionPlanNodes() failed: %v", err)
			}
			want := private
			if test.wantCanonicalSDK {
				want = canonical
			}
			projection := moduleSDKProjectionNodeForTest(t, plan, destination)
			if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != want.producer {
				t.Fatalf("sdk/%s projection input = %#v, want producer %s", destination, projection.Inputs, want.producer)
			}
			if _, err := plan.entries(); err != nil {
				t.Fatalf("projected action plan is invalid: %v", err)
			}
		})
	}
}

func TestModuleSDKUsesKbuildOverwriteOrderWithoutActionDependency(t *testing.T) {
	destination := "rust/" + linuxProbeSymbolPrefix + strings.Repeat("1", 64)
	for _, test := range []struct {
		name           string
		canonicalFirst bool
	}{
		{name: "later canonical"},
		{name: "later private persistent", canonicalFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph, firstKey, secondKey := moduleSDKProbeOverwriteGraphForTest(t, destination, false)
			metadata := &CompactMetadata{Config: CompactConfig{}}
			plan := &ActionPlan{
				Toolsets:       map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
				Recipes:        map[string]ActionRecipe{},
				selectionGraph: graph,
			}
			privateOutput := ActionPlanOutput{
				Tree: "prep", Path: destination,
				ArtifactPath: ".linux-bzl-versions/module-sdk-probe/" + destination, persistent: true,
			}
			var earlier, later moduleSDKSelectedArtifact
			if test.canonicalFirst {
				earlier = seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", destination, "sdk")
				later = seedModuleSDKPlanArtifactForTest(t, plan, "prep", privateOutput, "sdk")
			} else {
				earlier = seedModuleSDKPlanArtifactForTest(t, plan, "prep", privateOutput, "sdk")
				later = seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", destination, "sdk")
			}
			if err := graph.recordMaterializedProducer(firstKey, earlier.producer); err != nil {
				t.Fatal(err)
			}
			if err := graph.recordMaterializedProducer(secondKey, later.producer); err != nil {
				t.Fatal(err)
			}
			for _, order := range [][2]string{
				{earlier.producer, later.producer},
				{later.producer, earlier.producer},
			} {
				depends, err := moduleSDKProducerDependsOn(plan, order[0], order[1])
				if err != nil {
					t.Fatal(err)
				}
				if depends {
					t.Fatalf("probe writers unexpectedly have an action dependency: %s after %s", order[0], order[1])
				}
			}
			seedModuleSDKSymversForTest(t, plan)
			seedModuleSDKVmlinuxForTest(t, plan)
			seedModuleSDKReleaseForTest(t, plan)

			if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
				t.Fatalf("appendModuleSDKActionPlanNodes() failed: %v", err)
			}
			projection := moduleSDKProjectionNodeForTest(t, plan, destination)
			if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != later.producer {
				t.Fatalf("sdk/%s projection input = %#v, want source-ordered producer %s", destination, projection.Inputs, later.producer)
			}
			if _, err := plan.entries(); err != nil {
				t.Fatalf("projected action plan is invalid: %v", err)
			}
		})
	}
}

func TestModuleSDKOrdersMirroredHostPrepWritersBySourceNativeProducer(t *testing.T) {
	const destination = "tools/objtool/fixdep.o"
	privatePath := ".linux-bzl-versions/sdk-test/" + destination
	graph, firstKey, secondKey := moduleSDKProbeOverwriteGraphForTest(t, destination, true)
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, selectionGraph: graph,
	}
	first := seedModuleSDKPlanArtifactForTest(t, plan, "host", ActionPlanOutput{
		Tree: "host", Path: destination, ArtifactPath: privatePath, persistent: true,
	}, "sdk")
	second := seedModuleSDKPlanOutputForTest(t, plan, "host", "host", destination, "sdk")
	if err := graph.recordMaterializedProducer(firstKey, first.producer); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(secondKey, second.producer); err != nil {
		t.Fatal(err)
	}
	for _, writer := range []struct {
		artifact moduleSDKSelectedArtifact
		profile  string
	}{{first, "module-sdk-probe-first"}, {second, "module-sdk-probe-second"}} {
		selection := CompactKbuildSelection{
			Profile: writer.profile, Target: destination, MakeTarget: destination,
			Lifecycle: "prep", Scope: "host", Stage: "host",
		}
		if err := appendCompactKbuildHostPrepMirrors(plan, selection, writer.artifact.producer); err != nil {
			t.Fatal(err)
		}
	}
	laterMirror, _, ok := planProducerByOutput(plan, "prep", destination)
	if !ok {
		t.Fatal("source-ordered final host writer has no canonical prep publisher")
	}
	if got := moduleSDKNativePrepMirrorProducer(plan, destination, laterMirror); got != second.producer {
		t.Fatalf("final prep mirror source = %q, want host producer %q", got, second.producer)
	}
	seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	seedModuleSDKReleaseForTest(t, plan)
	if err := (&CompactMetadata{Config: CompactConfig{}}).appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatalf("source ordered host mirrors cannot publish module SDK: %v", err)
	}
	projection := moduleSDKProjectionNodeForTest(t, plan, destination)
	if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != laterMirror {
		t.Fatalf("SDK final %s projection = %#v, want later exact prep mirror %q", destination, projection.Inputs, laterMirror)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("mirrored SDK graph is invalid: %v", err)
	}

	// An unrelated copy of the earlier host artifact has no declared mirror
	// contract. The source graph cannot order that prep artifact by guessing
	// its native producer from a generic copy edge.
	unsafeGraph, unsafeFirstKey, unsafeSecondKey := moduleSDKProbeOverwriteGraphForTest(t, destination, true)
	unsafe := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, selectionGraph: unsafeGraph,
	}
	unsafeFirst := seedModuleSDKPlanArtifactForTest(t, unsafe, "host", ActionPlanOutput{
		Tree: "host", Path: destination, ArtifactPath: privatePath, persistent: true,
	}, "sdk")
	unsafeSecond := seedModuleSDKPlanOutputForTest(t, unsafe, "host", "host", destination, "sdk")
	if err := unsafeGraph.recordMaterializedProducer(unsafeFirstKey, unsafeFirst.producer); err != nil {
		t.Fatal(err)
	}
	if err := unsafeGraph.recordMaterializedProducer(unsafeSecondKey, unsafeSecond.producer); err != nil {
		t.Fatal(err)
	}
	unsafeCopy := seedDependentModuleSDKPlanArtifactForTest(t, unsafe, "prep", ActionPlanOutput{
		Tree: "prep", Path: destination, ArtifactPath: privatePath, persistent: true,
	}, "sdk", unsafeFirst)
	if got := moduleSDKNativePrepMirrorProducer(unsafe, destination, unsafeCopy.producer); got != unsafeCopy.producer {
		t.Fatalf("unowned copy %q was incorrectly adopted as native host writer %q", unsafeCopy.producer, got)
	}
	if err := appendCompactKbuildHostPrepMirrors(unsafe, CompactKbuildSelection{
		Profile: "module-sdk-probe-second", Target: destination, MakeTarget: destination,
		Lifecycle: "prep", Scope: "host", Stage: "host",
	}, unsafeSecond.producer); err != nil {
		t.Fatal(err)
	}
	seedModuleSDKSymversForTest(t, unsafe)
	seedModuleSDKVmlinuxForTest(t, unsafe)
	seedModuleSDKReleaseForTest(t, unsafe)
	if err := (&CompactMetadata{Config: CompactConfig{}}).appendModuleSDKActionPlanNodes(unsafe); err == nil || !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("unowned prep copy was source-ordered without provenance: %v", err)
	}
}

func TestModuleSDKRejectsUnorderedCanonicalAndPrivatePersistentVersions(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	const destination = "rust/libkernel.rmeta"
	seedModuleSDKPlanArtifactForTest(t, plan, "prep", ActionPlanOutput{
		Tree: "prep", Path: destination,
		ArtifactPath: ".linux-bzl-intermediate/rust/libkernel.rmeta", persistent: true,
	}, "sdk")
	seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", destination, "sdk")
	seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	seedModuleSDKReleaseForTest(t, plan)

	err := metadata.appendModuleSDKActionPlanNodes(plan)
	if err == nil || !strings.Contains(err.Error(), "is claimed by") {
		t.Fatalf("unordered SDK writer error = %v", err)
	}
}

func TestModuleSDKSelectedModuleSymversOverridesGenericObjectTreePath(t *testing.T) {
	metadata := &CompactMetadata{Config: CompactConfig{}}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", "Module.symvers", "sdk")
	symvers := seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	seedModuleSDKReleaseForTest(t, plan)

	if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatalf("appendModuleSDKActionPlanNodes() failed: %v", err)
	}
	projection := moduleSDKProjectionNodeForTest(t, plan, "Module.symvers")
	if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != symvers.producer {
		t.Fatalf("sdk/Module.symvers projection input = %#v, want selected producer %s", projection.Inputs, symvers.producer)
	}
}

func TestModuleSDKProjectsPersistentOutputFromLinearCompilerLowering(t *testing.T) {
	const (
		target     = "rust/helper-wrapper.o"
		persistent = "rust/libhelper.so"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:rust-sdk", "scripts/Makefile.build", "rust", `
cmd_rust_helper = $(RUSTC) --emit=link=rust/libhelper.so --crate-type proc-macro --crate-name helper $<
cmd_publish_helper = $(OBJCOPY) rust/libhelper.so $@
rust/helper-wrapper.o: rust/helper.rs FORCE
	$(call if_changed,rust_helper)
	$(call cmd,publish_helper)
`, map[string]string{
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
		"RUSTC":   KbuildActionRoleToken("target", "rustc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "rust/helper.rs")
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles(append(testConfiguredActionRoles, "rustc")...),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("prep", "prep", "sdk")
	if _, err := builder.build(target); err != nil {
		t.Fatal(err)
	}

	persistentSource := moduleSDKSelectedArtifact{}
	for _, node := range plan.Nodes {
		for slot, output := range node.Outputs {
			if output.Path != persistent {
				continue
			}
			if !output.persistent || actionPlanOutputIsCanonical(output) {
				t.Fatalf("linear compiler persistent output = %#v, want tagged private artifact", output)
			}
			persistentSource = moduleSDKSelectedArtifact{
				producer: node.ID, slot: slot, tree: output.Tree, path: output.Path,
			}
		}
	}
	if persistentSource.producer == "" {
		t.Fatalf("linear compiler lowering omits persistent output %q", persistent)
	}
	seedModuleSDKSymversForTest(t, plan)
	seedModuleSDKVmlinuxForTest(t, plan)
	seedModuleSDKReleaseForTest(t, plan)
	if err := metadata.appendModuleSDKActionPlanNodes(plan); err != nil {
		t.Fatalf("appendModuleSDKActionPlanNodes() failed: %v", err)
	}
	projection := moduleSDKProjectionNodeForTest(t, plan, persistent)
	if len(projection.Inputs) != 1 || projection.Inputs[0].ProducerID != persistentSource.producer || projection.Inputs[0].Slot != persistentSource.slot {
		t.Fatalf("sdk/%s projection input = %#v, want %s slot %d", persistent, projection.Inputs, persistentSource.producer, persistentSource.slot)
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatalf("content-address projected plan: %v", err)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("projected action plan is invalid: %v", err)
	}
}

func seedModuleSDKPlanOutputForTest(t *testing.T, plan *ActionPlan, stage, tree, outputPath, product string) moduleSDKSelectedArtifact {
	t.Helper()
	return seedModuleSDKPlanArtifactForTest(t, plan, stage, ActionPlanOutput{Tree: tree, Path: outputPath}, product)
}

func seedModuleSDKReleaseForTest(t *testing.T, plan *ActionPlan) moduleSDKSelectedArtifact {
	t.Helper()
	return seedModuleSDKPlanOutputForTest(t, plan, "prep", "prep", "include/config/kernel.release", "sdk")
}

func moduleSDKProbeOverwriteGraphForTest(
	t *testing.T,
	destination string,
	host bool,
) (*compactKbuildSelectionGraph, compactKbuildSelectionKey, compactKbuildSelectionKey) {
	t.Helper()
	writer := func(name string) CompactKbuildProfile {
		return mustCompactKbuildProfileForTest(t, name, "scripts/"+name+".mk", "", `
cmd_emit = touch $@
`+destination+`: FORCE
	$(call if_changed,emit)
`, nil)
	}
	first := writer("module-sdk-probe-first")
	second := writer("module-sdk-probe-second")
	firstArtifact := CompactKbuildVisibleArtifact{
		Path: destination, Profile: first.Name, Target: destination,
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{firstArtifact})
	second.InvocationPredecessors = []string{first.Name}
	stage, scope := "prep", "target"
	if host {
		stage, scope = "host", "host"
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: destination, MakeTarget: destination, Lifecycle: "prep", Scope: scope, Stage: stage},
			{
				Profile: second.Name, Target: destination, MakeTarget: destination, Lifecycle: "prep", Scope: scope, Stage: stage, UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{firstArtifact}),
			},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatalf("build probe overwrite selection graph: %v", err)
	}
	firstKey, ok := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: first.Name, target: destination,
	}]
	if !ok {
		t.Fatalf("selection graph omits first probe writer %s:%s", first.Name, destination)
	}
	secondKey, ok := graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
		profile: second.Name, target: destination,
	}]
	if !ok {
		t.Fatalf("selection graph omits second probe writer %s:%s", second.Name, destination)
	}
	return graph, firstKey, secondKey
}

func seedModuleSDKPlanArtifactForTest(t *testing.T, plan *ActionPlan, stage string, output ActionPlanOutput, product string) moduleSDKSelectedArtifact {
	t.Helper()
	node := ActionPlanNode{
		Stage: stage, Kind: "generate", Tool: "actionfile", Product: product,
		Outputs: []ActionPlanOutput{output},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""},
		Outputs:   []string{"00000000"},
	}
	producer, err := appendActionPlanNode(plan, node, recipe)
	if err != nil {
		t.Fatalf("seed %s/%s: %v", output.Tree, output.Path, err)
	}
	return moduleSDKSelectedArtifact{
		producer: producer, tree: output.Tree, path: output.Path,
	}
}

func seedDependentModuleSDKPlanArtifactForTest(
	t *testing.T,
	plan *ActionPlan,
	stage string,
	output ActionPlanOutput,
	product string,
	dependency moduleSDKSelectedArtifact,
) moduleSDKSelectedArtifact {
	t.Helper()
	node := ActionPlanNode{
		Stage: stage, Kind: "copy", Tool: "actionfile", Product: product,
		Inputs:  []ActionPlanNodeEdge{{Role: "artifact", ProducerID: dependency.producer, Slot: dependency.slot}},
		Outputs: []ActionPlanOutput{output},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{
			"-input", "${input:artifact:00000000}", "-out", "${output:00000000}",
		},
		Inputs: []string{"artifact:00000000"}, Outputs: []string{"00000000"},
	}
	producer, err := appendActionPlanNode(plan, node, recipe)
	if err != nil {
		t.Fatalf("seed dependent %s/%s: %v", output.Tree, output.Path, err)
	}
	return moduleSDKSelectedArtifact{
		producer: producer, tree: output.Tree, path: output.Path,
	}
}

func seedModuleSDKSymversForTest(t *testing.T, plan *ActionPlan) moduleSDKSelectedArtifact {
	t.Helper()
	symvers := seedModuleSDKPlanOutputForTest(t, plan, "target", "metadata", "Module.symvers", "module_symvers")
	plan.Products = append(plan.Products, ActionPlanProduct{
		Name: "module_symvers", Tree: "metadata", Path: "Module.symvers",
	})
	return symvers
}

func seedModuleSDKVmlinuxForTest(t *testing.T, plan *ActionPlan) moduleSDKSelectedArtifact {
	t.Helper()
	vmlinux := seedModuleSDKPlanOutputForTest(t, plan, "target", "vmlinux", "vmlinux", "vmlinux")
	plan.Products = append(plan.Products, ActionPlanProduct{
		Name: "vmlinux", Tree: "vmlinux", Path: "vmlinux",
	})
	return vmlinux
}

func moduleSDKProjectionNodeForTest(t *testing.T, plan *ActionPlan, outputPath string) ActionPlanNode {
	t.Helper()
	producer, _, ok := planProducerByOutput(plan, "sdk", outputPath)
	if !ok {
		t.Fatalf("no SDK producer for %s", outputPath)
	}
	for _, node := range plan.Nodes {
		if node.ID == producer {
			return node
		}
	}
	t.Fatalf("missing SDK node %s", producer)
	return ActionPlanNode{}
}
