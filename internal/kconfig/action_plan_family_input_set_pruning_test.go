package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Move only working-only input edges: these are exactly the operands which
// compactKbuildInputMayEnterSet moves out of recipe-addressable bindings.
func familyPersistentCompilerInputsSnapshotForTest(t *testing.T, snapshot ActionPlanSnapshot) ActionPlanSnapshot {
	t.Helper()
	plan := snapshotActionPlan(snapshot)
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	dependencies := make([]ConfigDependencySet, len(plan.Nodes))
	for nodeIndex := range plan.Nodes {
		node := &plan.Nodes[nodeIndex]
		dependencies[nodeIndex] = snapshot.ConfigDependencies[node.ID]
		if node.Kind != "compile" {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		semantic := familyRecipeSemanticBindings(recipe)
		uses, auxiliaryUses := map[string]bool{}, map[string]bool{}
		if recipe.CompilerInvocation != nil {
			for _, reference := range recipe.CompilerInvocation.WorkingInputUses {
				uses[reference] = true
			}
			for _, reference := range recipe.CompilerInvocation.AuxiliaryWorkingInputUses {
				auxiliaryUses[reference] = true
			}
		}
		for index, input := range node.Inputs {
			reference := "input:" + input.Role + ":" + planOrdinal(index)
			pathname, staged := recipe.WorkingInputs[reference]
			if !staged || semantic[reference] {
				t.Fatalf("fixture input %q is not a working-only operand", reference)
			}
			node.InputSet, err = store.Insert(node.InputSet, ActionPlanInputSetEntry{
				Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
				ProducerID: input.ProducerID, Slot: input.Slot,
				CompilerUse: uses[reference], AuxiliaryUse: auxiliaryUses[reference],
			})
			if err != nil {
				t.Fatal(err)
			}
			delete(recipe.WorkingInputs, reference)
		}
		node.Inputs = nil
		recipe.Inputs = nil
		if recipe.CompilerInvocation != nil {
			recipe.CompilerInvocation.WorkingInputUses = slices.DeleteFunc(recipe.CompilerInvocation.WorkingInputUses, func(reference string) bool {
				return strings.HasPrefix(reference, "input:")
			})
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses = slices.DeleteFunc(recipe.CompilerInvocation.AuxiliaryWorkingInputUses, func(reference string) bool {
				return strings.HasPrefix(reference, "input:")
			})
		}
		recipeID, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for index, node := range plan.Nodes {
		byID[node.ID] = dependencies[index]
	}
	result, err := canonicalActionPlanSnapshot(plan, byID, snapshot.ConfigFiles)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestActionPlanFamilyPersistentInputsPruneUnrelatedConfigProducer(t *testing.T) {
	variants := make([]ActionPlanFamilyVariant, 0, 2)
	for index, other := range []string{"n", "m"} {
		snapshot := familyTestGeneratedCompileInputSnapshot(t, familyTestConfig("y", other), false)
		variants = append(variants, ActionPlanFamilyVariant{
			Name:     []string{"base", "overlay"}[index],
			Snapshot: familyPersistentCompilerInputsSnapshotForTest(t, snapshot),
		})
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 1 || !slices.Equal(family.Memberships[compiles[0].ID], []string{"base", "overlay"}) {
		t.Fatalf("persistent unrelated producer left %d compiles, want one shared compile", len(compiles))
	}
	if compiles[0].InputSet != "" || len(family.InputSets) != 0 || len(family.Nodes) != 1 {
		t.Fatalf("unused persistent producer survived: root=%q sets=%d nodes=%d", compiles[0].InputSet, len(family.InputSets), len(family.Nodes))
	}
}

func TestActionPlanFamilyPersistentInputsHonorCompoundUses(t *testing.T) {
	for _, mode := range []string{familyCompoundUsesComplete, familyCompoundUsesIncompleteGlob, familyCompoundUsesIncompleteSearch, familyCompoundUsesIncompleteRoot} {
		t.Run(mode, func(t *testing.T) {
			snapshot := familyTestCompoundCompilerExtraInputSnapshot(t, familyTestConfig("y", "n"), mode)
			snapshot = familyPersistentCompilerInputsSnapshotForTest(t, snapshot)
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
			if err != nil {
				t.Fatal(err)
			}
			compiles := slices.DeleteFunc(slices.Clone(family.Nodes), func(node ActionPlanNode) bool { return node.Kind != "compile" })
			if len(compiles) != 1 {
				t.Fatalf("compiles = %d, want one", len(compiles))
			}
			entries := actionPlanNodeInputSetEntriesForTest(t, &ActionPlan{InputSets: family.InputSets}, compiles[0])
			if len(entries) != 1 || entries[0].Target.Path != "drivers/example/extra.o" {
				t.Fatalf("compound persistent inputs = %#v, want only retained middle-command extra.o", entries)
			}
		})
	}
}

func TestActionPlanFamilyPersistentInputsReattachSymmetricPriorTreeClosure(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyPersistentCompilerInputsSnapshotForTest(t,
			familyTestSymmetricGeneratedObjectClosureSnapshot(t, familyTestConfig("y", "n"), "include/generated/base-only.h"))},
		{Name: "overlay", Snapshot: familyPersistentCompilerInputsSnapshotForTest(t,
			familyTestSymmetricGeneratedObjectClosureSnapshot(t, familyTestConfig("y", "m"), "include/generated/overlay-only.h"))},
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 1 || len(compiles[0].Inputs) != 2 || compiles[0].InputSet != "" {
		t.Fatalf("persistent prior-tree closure = %#v, want one shared compile with two symmetric tree inputs", compiles)
	}
}

func TestActionPlanFamilyPersistentInputsLocalizeFallbackProjection(t *testing.T) {
	for _, producerMode := range []string{"fallback", "selected", "noncanonical"} {
		t.Run(producerMode, func(t *testing.T) {
			variants := make([]ActionPlanFamilyVariant, 0, 2)
			for index, other := range []string{"n", "m"} {
				variants = append(variants, ActionPlanFamilyVariant{
					Name: []string{"base", "overlay"}[index],
					Snapshot: familyPersistentCompilerInputsSnapshotForTest(t,
						familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", other), producerMode)),
				})
			}
			family, err := BuildActionPlanFamily(variants)
			if err != nil {
				t.Fatal(err)
			}
			compiles := familyCompileNodes(family)
			want := 2
			if producerMode == "fallback" {
				want = 1
			}
			if len(compiles) != want {
				t.Fatalf("%s persistent config projection left %d compiles, want %d", producerMode, len(compiles), want)
			}
			if producerMode != "fallback" {
				return
			}
			entries := actionPlanNodeInputSetEntriesForTest(t, &ActionPlan{InputSets: family.InputSets}, compiles[0])
			if len(entries) != 1 || entries[0].ProducerID == "" {
				t.Fatalf("localized projection provenance = %#v", entries)
			}
			producer, ok := compactKbuildPlanNode(&ActionPlan{Nodes: family.Nodes}, entries[0].ProducerID)
			if !ok || len(producer.Sources) != 1 {
				t.Fatalf("localized fallback producer = %#v, found=%t", producer, ok)
			}
			for _, source := range family.Sources {
				if source.ID != producer.Sources[0].SourceID {
					continue
				}
				capsuleID, _, _ := strings.Cut(source.Path, "/")
				header := family.Capsules[capsuleID]["include/generated/autoconf.h"]
				if !strings.Contains(header, "CONFIG_USED") || strings.Contains(header, "CONFIG_OTHER") {
					t.Fatalf("persistent fallback header = %q, want exact CONFIG_USED capsule", header)
				}
				return
			}
			t.Fatal("localized fallback capsule source is absent")
		})
	}
}

func TestActionPlanFamilyPersistentInputsRelevantFallbackConfigStillSplits(t *testing.T) {
	variants := []ActionPlanFamilyVariant{
		{Name: "base", Snapshot: familyPersistentCompilerInputsSnapshotForTest(t,
			familyTestPrepCopyCompileSnapshot(t, familyTestConfig("y", "n"), "fallback"))},
		{Name: "overlay", Snapshot: familyPersistentCompilerInputsSnapshotForTest(t,
			familyTestPrepCopyCompileSnapshot(t, familyTestConfig("n", "n"), "fallback"))},
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	compiles := familyCompileNodes(family)
	if len(compiles) != 2 {
		t.Fatalf("relevant persistent config change produced %d compiles, want two", len(compiles))
	}
	for _, compile := range compiles {
		if len(family.Memberships[compile.ID]) != 1 {
			t.Fatalf("relevant-config compile is shared: %#v", family.Memberships[compile.ID])
		}
	}
}

func TestActionPlanFamilyPersistentInputPrunerRetainsAliasesAndUnknownTargets(t *testing.T) {
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "original/seed.h"}},
		Nodes:   []ActionPlanNode{{ID: "producer", Stage: "prep", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "logical/used.h"}}}},
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		t.Fatal(err)
	}
	plan.ensureNodeLookupIndexes()
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	entries := []ActionPlanInputSetEntry{
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "staged/used.h"}, ProducerID: "producer"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "staged/source.h"}, SourceID: "src-00000001"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "owned/result.o"}, ProducerID: "producer"},
		{Target: ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: "prep", Path: "tree/opaque"}, ProducerID: "producer"},
	}
	root := ""
	for _, entry := range entries {
		root, err = store.Insert(root, entry)
		if err != nil {
			t.Fatal(err)
		}
	}
	pruner := &familyPreciseInputSetPruner{plan: plan}
	node := ActionPlanNode{Stage: "target", InputSet: root}
	kept, err := pruner.prune(node, ActionRecipe{}, map[string]bool{"logical/used.h": true, "original/seed.h": true}, map[string]bool{"owned/result.o": true})
	if err != nil || kept != root {
		t.Fatalf("logical/source aliases or semantic/unknown targets were dropped: root=%q, want=%q, error=%v", kept, root, err)
	}
	// The baseline is shared, but consumer-specific closure decisions are not.
	kept, err = pruner.prune(node, ActionRecipe{}, map[string]bool{}, map[string]bool{"owned/result.o": true})
	if err != nil {
		t.Fatal(err)
	}
	keptNode, ok := store.Node(kept)
	if !ok || keptNode.Count != 2 {
		t.Fatalf("second consumer reused another closure: %#v, found=%t", keptNode, ok)
	}
	for _, entry := range entries[2:] {
		actual, found, err := store.Lookup(kept, entry.Target)
		if err != nil || !found || actual != entry {
			t.Fatalf("semantic/unknown target %s was not retained: %#v, found=%t, error=%v", entry.Target.Path, actual, found, err)
		}
	}
}

func TestActionPlanFamilyPersistentInputPrunerSharesCumulativeRoots(t *testing.T) {
	const count = 512
	plan := &ActionPlan{}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	roots := make([]string, 0, count)
	root := ""
	for index := 0; index < count; index++ {
		producer := fmt.Sprintf("producer-%04d", index)
		pathname := fmt.Sprintf("generated/%04d.h", index)
		plan.Nodes = append(plan.Nodes, ActionPlanNode{ID: producer, Stage: "prep", Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}}})
		root, err = store.Insert(root, ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
			ProducerID: producer, CompilerUse: index%7 == 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		t.Fatal(err)
	}
	plan.ensureNodeLookupIndexes()
	uniqueBefore := store.NodeCount()
	pruner := &familyPreciseInputSetPruner{plan: plan}
	recipe := ActionRecipe{CompilerInvocation: &ActionRecipeCompilerInvocation{WorkingInputUsesComplete: true}}
	for index, root := range roots {
		pathname := fmt.Sprintf("generated/%04d.h", index)
		kept, err := pruner.prune(ActionPlanNode{Stage: "target", InputSet: root}, recipe, map[string]bool{pathname: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		keptNode, ok := store.Node(kept)
		want := index/7 + 1
		if index%7 != 0 {
			want++
		}
		if !ok || keptNode.Count != want {
			t.Fatalf("root %d retained %d entries, want %d", index, keptNode.Count, want)
		}
	}
	if got := len(pruner.filtered[1]); got != uniqueBefore || len(pruner.indexed) != uniqueBefore {
		t.Fatalf("memoized/indexed subtries=%d/%d, want %d unique persistent nodes", got, len(pruner.indexed), uniqueBefore)
	}
	if got := store.NodeCount() - uniqueBefore; got > 4*uniqueBefore {
		t.Fatalf("pruning %d cumulative roots created %d nodes from %d unique input nodes", count, got, uniqueBefore)
	}
}

// Exercise the actual cmd_and_fixdep recognizer and dependency analysis: the
// retained/ambient bits below are produced by lowering, not hand-authored.
func TestActionPlanFamilyPersistentInputsPruneNormalCmdAndFixdepAmbientConfig(t *testing.T) {
	const target, source, fixdep = "drivers/example/foo.o", "drivers/example/foo.c", "scripts/basic/fixdep"
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.build", "drivers/example", "dummy := y\n", nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	sourceRoot := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	if err := os.WriteFile(filepath.Join(sourceRoot, source), []byte("#if defined(CONFIG_USED)\n#endif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example"}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:     testConfiguredScopedActionRoles,
		Config:          CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{{Scope: "target", Role: "cc"}: {}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	fixdepProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	configID, err := ensureActionPlanSource(plan, "config", "autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	ambientID, err := ensureActionPlanSource(plan, "config", "kernel.release")
	if err != nil {
		t.Fatal(err)
	}
	const root = "__LINUX_BZL_OBJECT_TREE__/"
	const depfile = "drivers/example/.foo.o.d"
	command := KbuildActionRoleToken("target", "cc") + " -nostdinc -Wp,-MMD," + root + depfile +
		" -include " + root + "include/generated/autoconf.h -c -o " + root + target + " __LINUX_BZL_SOURCE_TREE__/" + source +
		"; " + root + fixdep + " " + root + depfile + " " + root + target + " 'saved command' > " + root + "drivers/example/.foo.o.cmd; rm -f " + root + depfile
	match := compactKbuildRuleMatch{profile: profile, stem: "foo", commandTemplates: []CompactKbuildCommandTemplate{{Name: "cc_o_c", Text: command}}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forOutput("target", "objects", "vmlinux").forProfile(profile)
	producerID, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
		{path: source, sourceID: sourceID}, {path: fixdep, producer: fixdepProducer},
		{path: "include/generated/autoconf.h", sourceID: configID, objectTree: true, workingOnly: true},
		{path: "include/config/kernel.release", sourceID: ambientID, objectTree: true, workingOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producerID)
	if !ok {
		t.Fatal("normal compiler node is absent")
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.CompilerInvocation == nil || !recipe.CompilerInvocation.WorkingInputUsesComplete {
		t.Fatalf("normal compiler did not produce complete use metadata: %#v", recipe.CompilerInvocation)
	}
	selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	plan.selectionGraph = &compactKbuildSelectionGraph{
		profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
		selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
		materializedProducers:       map[compactKbuildSelectionKey]string{selection: node.ID},
		selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
		selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
	}
	dependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if dependencies.Opaque || !slices.Equal(dependencies.Symbols, []string{"CONFIG_USED"}) {
		t.Fatalf("normal compiler dependencies = %#v, want precise CONFIG_USED", dependencies)
	}
	before := actionPlanNodeInputSetEntriesForTest(t, plan, node)
	if len(before) != 2 {
		t.Fatalf("normal compiler persistent frontier = %#v, want used and ambient config", before)
	}
	if _, _, err := prunePreciseFamilyCompilerInputs(plan, &node, dependencies); err != nil {
		t.Fatal(err)
	}
	after := actionPlanNodeInputSetEntriesForTest(t, plan, node)
	if len(after) != 1 || after[0].Target.Path != "include/generated/autoconf.h" || !after[0].CompilerUse {
		t.Fatalf("normal compiler retained ambient persistent config: %#v", after)
	}
}

// Keep the pre-optimization whole-root mapper as an independent semantic
// oracle and benchmark baseline. Production has only the memoized implementation.
func naiveCompactKbuildMapInputFrontierUsesForTest(
	plan *ActionPlan,
	frontier compactKbuildInputFrontier,
	compilerPaths, auxiliaryPaths map[string]bool,
) (compactKbuildInputFrontier, error) {
	if frontier.inputSet == "" {
		return frontier, nil
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	mapper, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		entry.CompilerUse = compilerPaths[entry.Target.Path]
		entry.AuxiliaryUse = auxiliaryPaths[entry.Target.Path]
		if entry.AuxiliaryUse && !entry.CompilerUse {
			return ActionPlanInputSetEntry{}, fmt.Errorf(
				"Kbuild auxiliary input-set target %q is absent from complete compiler uses",
				entry.Target.Path,
			)
		}
		return entry, nil
	})
	if err != nil {
		return compactKbuildInputFrontier{}, err
	}
	frontier.inputSet, err = mapper.Map(frontier.inputSet)
	return frontier, err
}

func cumulativeInputUseFrontiersForTest(t testing.TB, count int, inheritedUses bool) (*ActionPlan, []compactKbuildInputFrontier) {
	t.Helper()
	store := newPlanningActionPlanInputSetStore()
	plan := &ActionPlan{inputSetStore: store}
	frontiers := make([]compactKbuildInputFrontier, count)
	root := ""
	for index := range frontiers {
		var err error
		root, err = store.Insert(root, ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{
				Kind: ActionPlanInputSetWorkTarget,
				Path: fmt.Sprintf("generated/frontier/%08d.h", index),
			},
			ProducerID:  fmt.Sprintf("%064x", index+1),
			CompilerUse: inheritedUses, AuxiliaryUse: inheritedUses,
		})
		if err != nil {
			t.Fatal(err)
		}
		frontiers[index] = compactKbuildInputFrontier{inputSet: root}
	}
	return plan, frontiers
}

func TestPersistentInputUseClearMemoMatchesConsumerSemantics(t *testing.T) {
	plan, frontiers := cumulativeInputUseFrontiersForTest(t, 96, true)
	for _, index := range []int{0, 31, 63, 95, 31, 0, 95} {
		for _, test := range []struct {
			name                string
			compiler, auxiliary map[string]bool
		}{
			{name: "clear all inherited flags"},
			{name: "positive and false entries", compiler: map[string]bool{
				"generated/frontier/00000000.h": true,
				"generated/frontier/00000001.h": false,
			}, auxiliary: map[string]bool{"generated/frontier/00000000.h": true}},
			{name: "unmatched auxiliary remains absent", auxiliary: map[string]bool{"generated/not-present.h": true}},
			{name: "invalid auxiliary key remains absent", auxiliary: map[string]bool{"../invalid": true, "": true}},
			{name: "compiler-only use", compiler: map[string]bool{"generated/frontier/00000000.h": true}},
			{name: "matching auxiliary requires compiler", auxiliary: map[string]bool{"generated/frontier/00000000.h": true}},
			{name: "multiple auxiliary errors keep the error class", auxiliary: map[string]bool{
				"generated/frontier/00000000.h": true,
				"generated/frontier/00000001.h": true,
				"generated/frontier/00000002.h": true,
			}},
		} {
			frontier := frontiers[index]
			frontier.direct = []compactKbuildRuleInput{{path: "direct/input.o", producer: "direct-owner"}}
			want, wantErr := naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, test.compiler, test.auxiliary)
			got, gotErr := compactKbuildMapInputFrontierUses(plan, frontier, test.compiler, test.auxiliary)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("root %d %s: optimized error = %v, current error = %v", index, test.name, gotErr, wantErr)
			}
			if got.inputSet != want.inputSet || !slices.Equal(got.direct, want.direct) {
				t.Fatalf("root %d %s: optimized = %#v, current = %#v", index, test.name, got, want)
			}
			if wantErr != nil {
				const errorClass = "is absent from complete compiler uses"
				if !strings.Contains(gotErr.Error(), errorClass) || !strings.Contains(wantErr.Error(), errorClass) {
					t.Fatalf("root %d %s: optimized error = %v, current error = %v", index, test.name, gotErr, wantErr)
				}
			}
		}
	}
}

func TestPersistentInputUseClearMemoPreservesTargetNamespaces(t *testing.T) {
	plan, frontiers := cumulativeInputUseFrontiersForTest(t, 1, true)
	const pathname = "same/path.h"
	root := frontiers[0].inputSet
	for _, target := range []ActionPlanInputSetTarget{
		{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
		{Kind: ActionPlanInputSetAmbientTarget, Path: pathname},
		{Kind: ActionPlanInputSetTreeTarget, Tree: "kernel", Path: pathname},
		{Kind: ActionPlanInputSetTreeTarget, Tree: "prep", Path: pathname},
	} {
		var err error
		root, err = plan.inputSetStore.Insert(root, ActionPlanInputSetEntry{
			Target: target, SourceID: "src-" + strings.Repeat("1", 64), CompilerUse: true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		root                string
		compiler, auxiliary map[string]bool
	}{
		{root: root, compiler: map[string]bool{pathname: true}, auxiliary: map[string]bool{pathname: true}},
		{root: root},
		{root: root, compiler: map[string]bool{pathname: true}},
		// The alias index now remembers two tree targets. Neither belongs to
		// this older root, so an auxiliary-only stale key must remain harmless.
		{root: frontiers[0].inputSet, auxiliary: map[string]bool{pathname: true}},
		{root: root, auxiliary: map[string]bool{pathname: true}},
	} {
		frontier := compactKbuildInputFrontier{inputSet: test.root}
		want, wantErr := naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, test.compiler, test.auxiliary)
		got, gotErr := compactKbuildMapInputFrontierUses(plan, frontier, test.compiler, test.auxiliary)
		if (gotErr == nil) != (wantErr == nil) || wantErr == nil && got.inputSet != want.inputSet {
			t.Fatalf("namespace mapping optimized = (%q, %v), current = (%q, %v)", got.inputSet, gotErr, want.inputSet, wantErr)
		}
	}
}

func TestPersistentInputUseClearMemoPreservesDirectInputsOnRootError(t *testing.T) {
	plan, _ := cumulativeInputUseFrontiersForTest(t, 1, true)
	for _, root := range []string{"not-a-digest", strings.Repeat("f", 64)} {
		frontier := compactKbuildInputFrontier{
			inputSet: root,
			direct:   []compactKbuildRuleInput{{path: "direct/input.o", producer: "direct-owner"}},
		}
		want, wantErr := naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, nil, nil)
		got, gotErr := compactKbuildMapInputFrontierUses(plan, frontier, nil, nil)
		if wantErr == nil || gotErr == nil || got.inputSet != "" || want.inputSet != "" ||
			!slices.Equal(got.direct, frontier.direct) || !slices.Equal(got.direct, want.direct) {
			t.Fatalf("invalid root %q: optimized = (%#v, %v), current = (%#v, %v)", root, got, gotErr, want, wantErr)
		}
	}
}

func TestPersistentInputUseClearMemoResetsOnStoreReplacement(t *testing.T) {
	plan, frontiers := cumulativeInputUseFrontiersForTest(t, 32, true)
	const pathname = "history/path.h"
	root, err := plan.inputSetStore.Insert(frontiers[len(frontiers)-1].inputSet, ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: "old-tree", Path: pathname},
		ProducerID: strings.Repeat("e", 64), CompilerUse: true, AuxiliaryUse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Import only original roots, not any transformed roots subsequently
	// created by the memo. A stale memo hit would otherwise name an absent node.
	serialized, err := plan.inputSetStore.ReachableNodesForRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compactKbuildMapInputFrontierUses(plan,
		compactKbuildInputFrontier{inputSet: root}, map[string]bool{pathname: true}, nil); err != nil {
		t.Fatal(err)
	}
	memo := plan.inputUseProjection
	if len(memo.treeTargets[pathname]) != 1 {
		t.Fatal("initial store did not populate the historical alias fixture")
	}

	originalStore := plan.inputSetStore
	replacement, replacementFrontiers := cumulativeInputUseFrontiersForTest(t, 7, true)
	for _, test := range []struct {
		name  string
		store *ActionPlanInputSetStore
		nodes map[string]ActionPlanInputSetNode
		root  string
	}{
		{name: "same roots reimported into new store", nodes: serialized, root: root},
		{name: "replacement store with smaller frontier", store: replacement.inputSetStore, root: replacementFrontiers[6].inputSet},
		{name: "return to original store", store: originalStore, root: root},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan.InputSets, plan.inputSetStore = test.nodes, test.store
			store, err := plan.planningActionPlanInputSetStore()
			if err != nil {
				t.Fatal(err)
			}
			if test.store == nil && store == originalStore {
				t.Fatal("reimport fixture unexpectedly reused its original store")
			}
			oldClear := memo.clear
			frontier := compactKbuildInputFrontier{inputSet: test.root}
			got, err := compactKbuildMapInputFrontierUses(plan, frontier, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if plan.inputUseProjection != memo || memo.store != store || memo.clear == oldClear || memo.clear.store != store {
				t.Fatal("store replacement retained the old clearing mapper")
			}
			if len(memo.indexed) != 0 || len(memo.treeTargets) != 0 {
				t.Fatal("nil-map replacement retained historical alias indexes")
			}
			if _, err := store.nodeAt(got.inputSet); err != nil {
				t.Fatalf("cleared root was not interned in replacement store: %v", err)
			}
			want, err := naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, nil, nil)
			if err != nil || got.inputSet != want.inputSet {
				t.Fatalf("replacement root = %q, naive = (%q, %v)", got.inputSet, want.inputSet, err)
			}
		})
	}
}

func TestPersistentInputUseClearMemoHistoricalAliasesAreUniqueTargets(t *testing.T) {
	const (
		count    = 96
		pathname = "same/history.h"
	)
	plan, base := cumulativeInputUseFrontiersForTest(t, 1, false)
	roots := make([]string, count)
	root := base[0].inputSet
	for index := range roots {
		var err error
		root, err = plan.inputSetStore.Insert(root, ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{
				Kind: ActionPlanInputSetTreeTarget, Tree: fmt.Sprintf("history-%03d", index), Path: pathname,
			},
			ProducerID: fmt.Sprintf("%064x", index+100), CompilerUse: true, AuxiliaryUse: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		roots[index] = root
	}
	positive := map[string]bool{pathname: true}
	for _, root := range append(slices.Clone(roots), roots...) {
		frontier := compactKbuildInputFrontier{inputSet: root}
		got, err := compactKbuildMapInputFrontierUses(plan, frontier, positive, positive)
		if err != nil {
			t.Fatal(err)
		}
		want, err := naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, positive, positive)
		if err != nil || got.inputSet != want.inputSet {
			t.Fatalf("historical aliases root = %q, current = (%q, %v)", got.inputSet, want.inputSet, err)
		}
	}
	uniqueNodes, err := plan.inputSetStore.ReachableNodesForRoots(roots)
	if err != nil {
		t.Fatal(err)
	}
	memo := plan.inputUseProjection
	if len(memo.treeTargets) != 1 || len(memo.treeTargets[pathname]) != count || len(memo.indexed) != len(uniqueNodes) {
		t.Fatalf("alias index paths/targets/nodes = %d/%d/%d, want 1/%d/%d unique keys",
			len(memo.treeTargets), len(memo.treeTargets[pathname]), len(memo.indexed), count, len(uniqueNodes))
	}
	// All 96 historical same-path aliases are now known, but none belongs to
	// the original work-only root. An auxiliary-only key still must not error.
	got, err := compactKbuildMapInputFrontierUses(plan, base[0], nil, positive)
	if err != nil || got.inputSet != base[0].inputSet {
		t.Fatalf("historically known but absent aliases changed old root: (%#v, %v)", got, err)
	}
}

func TestPersistentInputUseClearMemoVisitsUniqueSubtries(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		plan, frontiers := cumulativeInputUseFrontiersForTest(t, 256, inherited)
		roots := make([]string, len(frontiers))
		for index, frontier := range frontiers {
			roots[index] = frontier.inputSet
		}
		uniqueNodes, err := plan.inputSetStore.ReachableNodesForRoots(roots)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			for _, frontier := range frontiers {
				cleared, err := compactKbuildMapInputFrontierUses(plan, frontier, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !inherited && cleared.inputSet != frontier.inputSet {
					t.Fatal("clearing a flag-free frontier changed its canonical root")
				}
			}
		}
		memo := plan.inputUseProjection
		if len(memo.clear.mapped) != len(uniqueNodes) {
			t.Fatalf("inherited=%t: clearing memo records %d subtries, want %d unique original nodes",
				inherited, len(memo.clear.mapped), len(uniqueNodes))
		}
		if len(memo.indexed) != 0 || len(memo.treeTargets) != 0 {
			t.Fatal("nil-map clearing unnecessarily populated the tree alias index")
		}
	}
}

func BenchmarkPersistentInputUseCumulativeRoots(b *testing.B) {
	for _, count := range []int{500, 1000, 2000} {
		for _, mode := range []string{"nil_clean", "nil_inherited", "sparse_positive"} {
			for _, implementation := range []string{"naive", "memoized"} {
				b.Run(fmt.Sprintf("%s/%s/%d", mode, implementation, count), func(b *testing.B) {
					plan, frontiers := cumulativeInputUseFrontiersForTest(b, count, mode != "nil_clean")
					var compiler, auxiliary map[string]bool
					if mode == "sparse_positive" {
						compiler = map[string]bool{"generated/frontier/00000000.h": true}
						auxiliary = map[string]bool{"generated/frontier/00000000.h": true}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						// Each timed batch represents one new plan sharing the
						// persistent store but not another plan's projection memo.
						plan.inputUseProjection = nil
						for _, frontier := range frontiers {
							var err error
							if implementation == "naive" {
								_, err = naiveCompactKbuildMapInputFrontierUsesForTest(plan, frontier, compiler, auxiliary)
							} else {
								_, err = compactKbuildMapInputFrontierUses(plan, frontier, compiler, auxiliary)
							}
							if err != nil {
								b.Fatal(err)
							}
						}
					}
					b.ReportMetric(float64(count), "roots/op")
					b.ReportMetric(float64(count*(count+1)/2), "logical_entries/op")
				})
			}
		}
	}
}
