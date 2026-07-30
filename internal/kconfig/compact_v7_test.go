package kconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestCompactV7FlagProgramsPreserveOrderAndDeduplicate(t *testing.T) {
	interner := newCompactV7ProgramInterner()
	first := interner.internLiteral([]string{"-Wall", "-O2"})
	copy := interner.internLiteral([]string{"-Wall", "-O2"})
	reversed := interner.internLiteral([]string{"-O2", "-Wall"})
	emptyA := interner.internLiteral(nil)
	emptyB := interner.internLiteral([]string{})

	if first != copy {
		t.Fatalf("identical flag programs = %q and %q", first, copy)
	}
	if first == reversed {
		t.Fatalf("flag order did not affect program ID %q", first)
	}
	if emptyA != emptyB {
		t.Fatalf("empty flag programs = %q and %q", emptyA, emptyB)
	}
	metadata := &CompactMetadataV7{}
	interner.apply(metadata)
	if got, want := len(metadata.FlagTerminals), 3; got != want {
		t.Fatalf("flag terminal count = %d, want %d", got, want)
	}
	if got, want := len(metadata.FlagPrograms), 3; got != want {
		t.Fatalf("flag program count = %d, want %d", got, want)
	}
	for _, program := range metadata.FlagPrograms {
		if got, want := program.Effects, []string{compactV7EffectArgv}; !reflect.DeepEqual(got, want) {
			t.Fatalf("program %s effects = %v, want %v", program.ID, got, want)
		}
	}
}

func TestCompactV7FlagEffectClassificationIsConservative(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "argv only",
			argv: []string{"-Wall", "-O2", "-g"},
			want: []string{compactV7EffectArgv},
		},
		{
			name: "preprocessor input",
			argv: []string{"-DDEBUG=1", "-include", "forced.h"},
			want: []string{compactV7EffectArgv, compactV7EffectInput},
		},
		{
			name: "target input",
			argv: []string{"-mno-outline-atomics"},
			want: []string{compactV7EffectArgv, compactV7EffectInput},
		},
		{
			name: "side output",
			argv: []string{"-MF", "deps.d"},
			want: []string{compactV7EffectArgv, compactV7EffectOutput},
		},
		{
			name: "response file",
			argv: []string{"@flags.rsp"},
			want: []string{
				compactV7EffectArgv,
				compactV7EffectInput,
				compactV7EffectOutput,
			},
		},
		{
			name: "action graph",
			argv: []string{"-flto=thin"},
			want: []string{compactV7EffectArgv, compactV7EffectGraph},
		},
		{
			name: "unknown defaults to output sensitive",
			argv: []string{"-fexperimental-unmodeled"},
			want: []string{compactV7EffectArgv, compactV7EffectOutput},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyCompactV7FlagEffects(test.argv); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("classifyCompactV7FlagEffects(%v) = %v, want %v", test.argv, got, test.want)
			}
		})
	}
}

func TestCompactV7DecisionProgramValidation(t *testing.T) {
	interner := newCompactV7ProgramInterner()
	contextID := interner.internLiteral(nil)
	context := interner.programs[contextID]
	whenTrue := CompactKbuildFlagTerminal{
		Argv: []string{"-Wall"},
	}
	whenTrue.ID = compactV7FlagTerminalContentID(whenTrue.Argv)
	whenFalse := CompactKbuildFlagTerminal{
		Argv: []string{"-DDEBUG=1"},
	}
	whenFalse.ID = compactV7FlagTerminalContentID(whenFalse.Argv)
	probe := CompactKbuildProbe{
		Kind:           "cc_option",
		CandidateArgv:  []string{"-mrecord-mcount"},
		ContextProgram: contextID,
		Language:       "c",
		Srcarch:        "x86",
	}
	probe.ID = compactV7ProbeContentID(probe)
	node := CompactKbuildFlagNode{
		Kind:      "select",
		Probe:     probe.ID,
		WhenTrue:  whenTrue.ID,
		WhenFalse: whenFalse.ID,
	}
	node.ID = compactV7FlagSelectContentID(node.Probe, node.WhenTrue, node.WhenFalse)
	effects := []string{compactV7EffectArgv, compactV7EffectInput}
	program := CompactKbuildFlagProgram{
		Root:    node.ID,
		Effects: effects,
	}
	program.ID = compactV7FlagProgramContentID(program.Root, program.Effects)
	metadata := &CompactMetadataV7{
		KbuildProbes: []CompactKbuildProbe{probe},
		FlagTerminals: []CompactKbuildFlagTerminal{
			interner.terminals[context.Root],
			whenTrue,
			whenFalse,
		},
		FlagNodes:    []CompactKbuildFlagNode{node},
		FlagPrograms: []CompactKbuildFlagProgram{context, program},
	}
	sort.Slice(metadata.FlagTerminals, func(i, j int) bool {
		return metadata.FlagTerminals[i].ID < metadata.FlagTerminals[j].ID
	})
	sort.Slice(metadata.FlagPrograms, func(i, j int) bool {
		return metadata.FlagPrograms[i].ID < metadata.FlagPrograms[j].ID
	})
	if _, err := metadata.validateCompactV7Programs(); err != nil {
		t.Fatalf("validateCompactV7Programs() failed: %v", err)
	}

	metadata.FlagNodes[0].WhenFalse = metadata.FlagNodes[0].WhenTrue
	metadata.FlagNodes[0].ID = compactV7FlagSelectContentID(
		metadata.FlagNodes[0].Probe,
		metadata.FlagNodes[0].WhenTrue,
		metadata.FlagNodes[0].WhenFalse,
	)
	if _, err := metadata.validateCompactV7Programs(); err == nil ||
		!strings.Contains(err.Error(), "not reduced") {
		t.Fatalf("validateCompactV7Programs() error = %v, want reduction error", err)
	}
}

func TestCompactV7FlagProgramsRejectResponseFileArguments(t *testing.T) {
	for _, arg := range []string{
		"@flags.rsp",
		"$(CLANG_FLAGS)@flags.rsp",
	} {
		terminal := CompactKbuildFlagTerminal{
			Argv: []string{arg},
		}
		terminal.ID = compactV7FlagTerminalContentID(terminal.Argv)
		metadata := &CompactMetadataV7{
			FlagTerminals: []CompactKbuildFlagTerminal{terminal},
		}
		if _, err := metadata.validateCompactV7Programs(); err == nil ||
			!strings.Contains(err.Error(), "unsupported response-file argument") {
			t.Fatalf(
				"validateCompactV7Programs(%q) error = %v, want response-file error",
				arg,
				err,
			)
		}
	}

	interner := newCompactV7ProgramInterner()
	contextID := interner.internLiteral(nil)
	metadata := &CompactMetadataV7{}
	interner.apply(metadata)
	probe := CompactKbuildProbe{
		Kind:           "cc_option",
		CandidateArgv:  []string{"$(CLANG_FLAGS)@candidate.rsp"},
		ContextProgram: contextID,
		Language:       "c",
		Srcarch:        "x86",
	}
	probe.ID = compactV7ProbeContentID(probe)
	metadata.KbuildProbes = []CompactKbuildProbe{probe}
	if _, err := metadata.validateCompactV7Programs(); err == nil ||
		!strings.Contains(err.Error(), "unsupported response-file candidate") {
		t.Fatalf("validateCompactV7Programs() error = %v, want response-file candidate error", err)
	}
}

func TestCompactV7StaticLinkRecipesRequireEmptyFlagPrograms(t *testing.T) {
	interner := newCompactV7ProgramInterner()
	emptyID := interner.internLiteral(nil)
	flagsID := interner.internLiteral([]string{"-Wl,--build-id=none"})
	metadata := &CompactMetadataV7{
		ToolchainProfile:      "llvm-test/x86",
		CompileEnvironmentABI: "linux.bzl/compact-v7/test",
	}
	interner.apply(metadata)
	programs := make(map[string]CompactKbuildFlagProgram, len(metadata.FlagPrograms))
	for _, program := range metadata.FlagPrograms {
		programs[program.ID] = program
	}
	recipe := CompactActionRecipe{
		Kind:              "composite",
		Mode:              "y",
		FlagProgram:       flagsID,
		RemoveFlagProgram: emptyID,
	}
	recipe.ID = compactV7ActionRecipeContentID(
		recipe,
		metadata.ToolchainProfile,
		metadata.CompileEnvironmentABI,
	)
	metadata.ActionRecipes = []CompactActionRecipe{recipe}
	if _, err := metadata.validateCompactV7Recipes(programs); err == nil ||
		!strings.Contains(err.Error(), "must use the canonical empty flag program") {
		t.Fatalf("validateCompactV7Recipes() error = %v, want empty-program error", err)
	}
}

func TestCompactV7SymbolicKbuildProbesPreserveContextAndInputUnion(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"symbolic Kbuild flags\"\n")
	sourceRoot := t.TempDir()
	for _, path := range []string{
		"main.c",
		"candidate.h",
		"fallback-one.h",
		"fallback-two.h",
	} {
		mustWriteSource(t, sourceRoot, path, "int value;\n")
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	kb, err := parseKbuildWithOptions(
		strings.NewReader(`
obj-y += main.o
KBUILD_CFLAGS := -DPROBE_CONTEXT=one
ccflags-y += $(call cc-option,-include $(srctree)/candidate.h,-include $(srctree)/fallback-one.h)
KBUILD_CFLAGS := -DPROBE_CONTEXT=two
ccflags-y += $(call cc-option,-include $(srctree)/candidate.h,-include $(srctree)/fallback-two.h)
`),
		"Kbuild",
		KbuildOptions{Variables: map[string]string{
			"SRCARCH": "x86",
			"srctree": sourceRoot,
		}},
		sourceRoot,
	)
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	metadata, err := tree.CompactMetadataBatchV7WithOptions(
		[]NamedConfig{{Name: "base"}},
		CompactMetadataV7Options{
			CompactMetadataOptions: CompactMetadataOptions{
				SourceRoot:            sourceRoot,
				Srcarch:               "x86",
				CompileEnvironmentABI: "linux.bzl/compact-v7/test",
			},
			ToolchainProfileID: "llvm-test/x86",
		},
		func(config *ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: "//headers:" + config.Name,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchV7WithOptions() failed: %v", err)
	}
	if got, want := len(metadata.KbuildProbes), 2; got != want {
		t.Fatalf("Kbuild probe count = %d, want %d: %#v", got, want, metadata.KbuildProbes)
	}
	first, second := metadata.KbuildProbes[0], metadata.KbuildProbes[1]
	if !reflect.DeepEqual(first.CandidateArgv, second.CandidateArgv) {
		t.Fatalf(
			"same candidate changed across contexts: %v/%v",
			first.CandidateArgv,
			second.CandidateArgv,
		)
	}
	if first.ContextProgram == second.ContextProgram || first.ID == second.ID {
		t.Fatalf(
			"context-sensitive probes collapsed: contexts=%s/%s ids=%s/%s",
			first.ContextProgram,
			second.ContextProgram,
			first.ID,
			second.ID,
		)
	}
	selectIDs := map[string]bool{}
	for _, node := range metadata.FlagNodes {
		if node.Kind == "select" {
			selectIDs[node.ID] = true
		}
	}
	if got, want := len(selectIDs), 2; got != want {
		t.Fatalf("select node count = %d, want %d: %#v", got, want, metadata.FlagNodes)
	}

	object := compactV7SingleObject(
		t,
		compactV7ObjectsByName(metadata.ObjectVariants),
		"main.o",
	)
	recipe := compactV7RecipeByID(metadata, object.Recipe)
	program := compactV7ProgramByID(metadata, recipe.FlagProgram)
	if !slices.Contains(program.Effects, compactV7EffectInput) {
		t.Fatalf("symbolic flag program effects = %v, want input", program.Effects)
	}
	sourceSet := ""
	for _, group := range metadata.ActionSourceGroups {
		if group.ID == object.ActionSourceGroup {
			sourceSet = group.SourceSet
			break
		}
	}
	if sourceSet == "" {
		t.Fatalf("object action source group %s is missing", object.ActionSourceGroup)
	}
	files := compactV7ExpandedSetForTest(t, metadata, sourceSet)
	paths := map[string]bool{}
	for index := range files {
		paths[metadata.SourceFiles[index-1].Path] = true
	}
	for _, path := range []string{
		"candidate.h",
		"fallback-one.h",
		"fallback-two.h",
	} {
		if !paths[path] {
			t.Errorf("symbolic source input union omits %q: %v", path, paths)
		}
	}
}

func TestCompactV7SourceSetDAGValidation(t *testing.T) {
	files := []CompactSourceInput{
		{Path: "a.h", Digest: strings.Repeat("a", 64)},
		{Path: "b.h", Digest: strings.Repeat("b", 64)},
	}
	child := CompactSourceSet{Files: []int{1}}
	child.ID = compactV7SourceSetContentID(files[:1], nil)
	parent := CompactSourceSet{
		Files:    []int{2},
		Children: []string{child.ID},
	}
	parent.ID = compactV7SourceSetContentID(files[1:], parent.Children)
	group := CompactActionSourceGroup{
		SourceSet:     parent.ID,
		PrimarySource: 2,
	}
	group.ID = compactV7ActionSourceGroupContentID(parent.ID, files[1])
	metadata := &CompactMetadataV7{
		SourceFiles:        files,
		SourceSets:         []CompactSourceSet{child, parent},
		ActionSourceGroups: []CompactActionSourceGroup{group},
	}
	sort.Slice(metadata.SourceSets, func(i, j int) bool {
		return metadata.SourceSets[i].ID < metadata.SourceSets[j].ID
	})
	_, expanded, err := metadata.validateCompactV7Sources()
	if err != nil {
		t.Fatalf("validateCompactV7Sources() failed: %v", err)
	}
	if got, want := expanded[parent.ID], map[int]bool{1: true, 2: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded parent = %v, want %v", got, want)
	}

	for i := range metadata.SourceSets {
		if metadata.SourceSets[i].ID != parent.ID {
			continue
		}
		metadata.SourceSets[i].Files = []int{1, 2}
		metadata.SourceSets[i].ID = compactV7SourceSetContentID(
			files,
			metadata.SourceSets[i].Children,
		)
		break
	}
	sort.Slice(metadata.SourceSets, func(i, j int) bool {
		return metadata.SourceSets[i].ID < metadata.SourceSets[j].ID
	})
	if _, _, err := metadata.validateCompactV7Sources(); err == nil ||
		!strings.Contains(err.Error(), "overlaps file") {
		t.Fatalf("validateCompactV7Sources() error = %v, want overlap error", err)
	}
}

func TestCompactV7ConversionPrunesCompositesAndGroupsRecipes(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "compact-v7"

config MODULES
	bool "Modules"
	modules

config STACK
	tristate "Stack"
`)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y += bundle.o common.o peer.o
bundle-y += first.o second.o
obj-$(CONFIG_STACK) += module.o
module-y += module_member.o
ccflags-y += -Wall -O2
CFLAGS_first.o += -DFIRST
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for _, path := range []string{
		"common.c",
		"first.c",
		"module_member.c",
		"peer.c",
		"second.c",
	} {
		mustWriteSource(t, sourceRoot, path, "int value;\n")
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	configs := []NamedConfig{
		{
			Name: "module",
			Flags: map[string]string{
				"CONFIG_MODULES": "y",
				"CONFIG_STACK":   "m",
			},
		},
		{Name: "base"},
	}
	opts := CompactMetadataV7Options{
		CompactMetadataOptions: CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "linux.bzl/compact-v7/test",
		},
		ToolchainProfileID: "llvm-test/x86",
	}
	graphForConfig := func(config *ResolvedConfig) (CompactConfigGraph, error) {
		return CompactConfigGraph{
			Kbuild:                kb,
			GeneratedHeadersLabel: "//headers:" + config.Name,
		}, nil
	}

	eagerBefore, err := tree.CompactMetadataBatchWithOptions(
		configs,
		opts.CompactMetadataOptions,
		graphForConfig,
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	eagerBeforeJSON, err := eagerBefore.JSON()
	if err != nil {
		t.Fatalf("eager metadata JSON() failed: %v", err)
	}
	metadata, err := tree.CompactMetadataBatchV7WithOptions(configs, opts, graphForConfig)
	if err != nil {
		t.Fatalf("CompactMetadataBatchV7WithOptions() failed: %v", err)
	}
	eagerAfter, err := tree.CompactMetadataBatchWithOptions(
		configs,
		opts.CompactMetadataOptions,
		graphForConfig,
	)
	if err != nil {
		t.Fatalf("second CompactMetadataBatchWithOptions() failed: %v", err)
	}
	eagerAfterJSON, err := eagerAfter.JSON()
	if err != nil {
		t.Fatalf("second eager metadata JSON() failed: %v", err)
	}
	if !reflect.DeepEqual(eagerBeforeJSON, eagerAfterJSON) {
		t.Fatal("compact-v7 conversion mutated eager metadata")
	}
	v7Families := make(map[string]bool, len(metadata.GeneratedHeaderFamilies))
	for _, family := range metadata.GeneratedHeaderFamilies {
		v7Families[family.ID] = true
	}
	for _, family := range eagerBefore.GeneratedHeaderFamilies {
		if !v7Families[family.ID] {
			t.Fatalf("compact-v7 pruned generated-header family %s (%s)", family.Name, family.ID)
		}
	}

	if metadata.Protocol != CompactMetadataProtocolV7 {
		t.Fatalf("protocol = %q", metadata.Protocol)
	}
	if got, want := compactV7ConfigNames(metadata.Configs), []string{"base", "module"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("config names = %v, want %v", got, want)
	}
	for _, config := range metadata.Configs {
		files := compactV7ExpandedSetForTest(t, metadata, config.SupportSourceSet)
		hasLinkerScript := false
		for index := range files {
			if metadata.SourceFiles[index-1].Path == "arch/x86/kernel/vmlinux.lds.S" {
				hasLinkerScript = true
				break
			}
		}
		if !hasLinkerScript {
			t.Errorf(
				"config %q support source set %s omits x86 linker script",
				config.Name,
				config.SupportSourceSet,
			)
		}
	}
	objectsByName := compactV7ObjectsByName(metadata.ObjectVariants)
	for _, pruned := range []string{"bundle.o"} {
		if len(objectsByName[pruned]) != 0 {
			t.Fatalf("flattened built-in composite %q was retained: %#v", pruned, objectsByName[pruned])
		}
	}
	for _, retained := range []string{
		"common.o",
		"first.o",
		"module.o",
		"module_member.o",
		"peer.o",
		"second.o",
	} {
		if len(objectsByName[retained]) == 0 {
			t.Errorf("reachable object %q was pruned", retained)
		}
	}
	module := objectsByName["module.o"]
	if len(module) != 1 {
		t.Fatalf("module variants = %#v, want one", module)
	}
	moduleRecipe := compactV7RecipeByID(metadata, module[0].Recipe)
	if moduleRecipe.Kind != "composite" || !moduleRecipe.ModuleRoot {
		t.Fatalf("module recipe = %#v, want module-root composite", moduleRecipe)
	}

	common := compactV7SingleObject(t, objectsByName, "common.o")
	peer := compactV7SingleObject(t, objectsByName, "peer.o")
	if common.CompileEnvironment == "" || peer.CompileEnvironment == "" {
		t.Fatalf("common/peer compile environments = %q/%q, want per-object environments", common.CompileEnvironment, peer.CompileEnvironment)
	}
	if common.Recipe != peer.Recipe {
		t.Fatalf("common/peer recipes = %s/%s, want one shared recipe", common.Recipe, peer.Recipe)
	}
	if common.ActionSourceGroup == peer.ActionSourceGroup {
		t.Fatalf("common/peer unexpectedly share object-specific source group %s", common.ActionSourceGroup)
	}
	if common.RecipeGroup != peer.RecipeGroup {
		t.Fatalf("common/peer recipe groups = %s/%s, want one group", common.RecipeGroup, peer.RecipeGroup)
	}
	group := compactV7RecipeGroupByID(metadata, common.RecipeGroup)
	if got, want := group.Objects, sortedStrings([]string{common.Target, peer.Target}); !reflect.DeepEqual(got, want) {
		t.Fatalf("shared recipe group objects = %v, want %v", got, want)
	}

	recipe := compactV7RecipeByID(metadata, common.Recipe)
	program := compactV7ProgramByID(metadata, recipe.FlagProgram)
	terminal := compactV7TerminalByID(metadata, program.Root)
	if got, want := terminal.Argv, []string{"-Wall", "-O2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("common flags = %v, want %v", got, want)
	}
	if len(metadata.KbuildProbes) != 0 {
		t.Fatalf(
			"probe-free Kbuild emitted symbolic probes: probes=%v nodes=%v",
			metadata.KbuildProbes,
			metadata.FlagNodes,
		)
	}

	data, err := metadata.JSON()
	if err != nil {
		t.Fatalf("v7 JSON() failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal v7 JSON: %v", err)
	}
	for _, raw := range decoded["object_variants"].([]any) {
		object := raw.(map[string]any)
		if _, compileObject := object["action_source_group"]; compileObject {
			if _, ok := object["compile_environment"]; !ok {
				t.Fatalf("v7 compile object omits per-object compile_environment: %#v", object)
			}
		} else if _, ok := object["compile_environment"]; ok {
			t.Fatalf("v7 composite object contains compile_environment: %#v", object)
		}
		if _, ok := object["flags"]; ok {
			t.Fatalf("v7 object contains eager flags: %#v", object)
		}
		if _, ok := object["remove_flags"]; ok {
			t.Fatalf("v7 object contains eager remove_flags: %#v", object)
		}
	}
	for _, raw := range decoded["action_recipes"].([]any) {
		recipe := raw.(map[string]any)
		if _, ok := recipe["compile_environment"]; ok {
			t.Fatalf("v7 shared recipe contains object-specific compile_environment: %#v", recipe)
		}
	}
	for _, raw := range decoded["flag_terminals"].([]any) {
		terminal := raw.(map[string]any)
		if _, ok := terminal["argv"].([]any); !ok {
			t.Fatalf("v7 flag terminal argv is not a JSON array: %#v", terminal)
		}
	}

	var split CompactMetadataV7
	if err := json.Unmarshal(data, &split); err != nil {
		t.Fatalf("clone compact-v7 metadata: %v", err)
	}
	split.ActionRecipeGroups = nil
	for _, candidate := range metadata.ActionRecipeGroups {
		if candidate.ID != common.RecipeGroup {
			split.ActionRecipeGroups = append(split.ActionRecipeGroups, candidate)
			continue
		}
		for _, target := range candidate.Objects {
			contentID := ""
			for _, object := range metadata.ObjectVariants {
				if object.Target == target {
					contentID = object.ContentID
					break
				}
			}
			if contentID == "" {
				t.Fatalf("recipe group target %q has no object", target)
			}
			replacement := CompactActionRecipeGroup{
				Recipe:       candidate.Recipe,
				Reachability: candidate.Reachability,
				Objects:      []string{target},
			}
			replacement.ID = compactV7ActionRecipeGroupContentID(
				replacement.Recipe,
				replacement.Reachability,
				[]string{contentID},
			)
			split.ActionRecipeGroups = append(split.ActionRecipeGroups, replacement)
			for index := range split.ObjectVariants {
				if split.ObjectVariants[index].Target == target {
					split.ObjectVariants[index].RecipeGroup = replacement.ID
				}
			}
		}
	}
	sort.Slice(split.ActionRecipeGroups, func(left, right int) bool {
		return split.ActionRecipeGroups[left].ID < split.ActionRecipeGroups[right].ID
	})
	if err := split.validate(); err == nil || !strings.Contains(err.Error(), "duplicate recipe/reachability partition") {
		t.Fatalf("split recipe/reachability validation error = %v", err)
	}
}

func TestCompactV7ConversionIsCanonicalAcrossConfigOrder(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"compact-v7 order\"\n")
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "int init_value;\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	opts := CompactMetadataV7Options{
		CompactMetadataOptions: CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "linux.bzl/compact-v7/test",
		},
		ToolchainProfileID: "llvm-test/x86",
	}
	graphForConfig := func(config *ResolvedConfig) (CompactConfigGraph, error) {
		return CompactConfigGraph{
			Kbuild:                kb,
			GeneratedHeadersLabel: "//headers:" + config.Name,
		}, nil
	}
	generate := func(configs []NamedConfig) (*CompactMetadataV7, []byte) {
		t.Helper()
		metadata, err := tree.CompactMetadataBatchV7WithOptions(configs, opts, graphForConfig)
		if err != nil {
			t.Fatalf("CompactMetadataBatchV7WithOptions() failed: %v", err)
		}
		data, err := metadata.JSON()
		if err != nil {
			t.Fatalf("JSON() failed: %v", err)
		}
		return metadata, data
	}

	forward, forwardJSON := generate([]NamedConfig{{Name: "alpha"}, {Name: "beta"}})
	_, reverseJSON := generate([]NamedConfig{{Name: "beta"}, {Name: "alpha"}})
	if !reflect.DeepEqual(forwardJSON, reverseJSON) {
		t.Fatalf("config discovery order changed compact-v7 JSON\nforward:\n%s\nreverse:\n%s", forwardJSON, reverseJSON)
	}
	if len(forward.Configs) != 2 ||
		!reflect.DeepEqual(forward.Configs[0].ObjectTargets, forward.Configs[1].ObjectTargets) {
		t.Fatalf("identical configs did not share exact graph roots: %#v", forward.Configs)
	}
	forwardObject := compactV7SingleObject(t, compactV7ObjectsByName(forward.ObjectVariants), "init.o")
	var reachability CompactReachabilitySignature
	for _, candidate := range forward.ReachabilitySignatures {
		if candidate.ID == forwardObject.Reachability {
			reachability = candidate
			break
		}
	}
	if got, want := reachability.Configs, []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared object reachability = %v, want %v", got, want)
	}
	group := compactV7RecipeGroupByID(forward, forwardObject.RecipeGroup)
	if group.Reachability != reachability.ID ||
		!reflect.DeepEqual(group.Objects, []string{forwardObject.Target}) {
		t.Fatalf("shared object recipe group = %#v", group)
	}

	renamed, _ := generate([]NamedConfig{{Name: "first"}, {Name: "second"}})
	renamedObject := compactV7SingleObject(t, compactV7ObjectsByName(renamed.ObjectVariants), "init.o")
	if forwardObject.ContentID != renamedObject.ContentID ||
		forwardObject.Target != renamedObject.Target ||
		forwardObject.Recipe != renamedObject.Recipe {
		t.Fatalf(
			"config rename changed object identity:\nforward=%#v\nrenamed=%#v",
			forwardObject,
			renamedObject,
		)
	}
	if forwardObject.Reachability == renamedObject.Reachability ||
		forwardObject.RecipeGroup == renamedObject.RecipeGroup {
		t.Fatalf(
			"config rename did not change partition identity:\nforward=%#v\nrenamed=%#v",
			forwardObject,
			renamedObject,
		)
	}
}

func TestCompactV7ValidationRejectsPrimarySourceOutsideSet(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"compact-v7 validation\"\n")
	kb, err := ParseKbuild(strings.NewReader("obj-y += first.o second.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "first.c", "int first;\n")
	mustWriteSource(t, sourceRoot, "second.c", "int second;\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	metadata, err := tree.CompactMetadataBatchV7WithOptions(
		[]NamedConfig{{Name: "base"}},
		CompactMetadataV7Options{
			CompactMetadataOptions: CompactMetadataOptions{
				SourceRoot:            sourceRoot,
				Srcarch:               "x86",
				CompileEnvironmentABI: "linux.bzl/compact-v7/test",
			},
			ToolchainProfileID: "llvm-test/x86",
		},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: "//headers:base",
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchV7WithOptions() failed: %v", err)
	}
	if len(metadata.ActionSourceGroups) < 2 {
		t.Fatalf("action source groups = %#v, want at least two", metadata.ActionSourceGroups)
	}
	first := &metadata.ActionSourceGroups[0]
	firstFiles := compactV7ExpandedSetForTest(t, metadata, first.SourceSet)
	replacement := 0
	for index := range metadata.SourceFiles {
		if !firstFiles[index+1] {
			replacement = index + 1
			break
		}
	}
	if replacement == 0 {
		t.Fatal("fixture has no source outside the first action source set")
	}
	first.PrimarySource = replacement
	if err := metadata.validate(); err == nil || !strings.Contains(err.Error(), "omits primary source") {
		t.Fatalf("validate() error = %v, want omitted primary source", err)
	}
}

func TestCompactV7RequiresToolchainProfile(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"compact-v7 profile\"\n")
	_, err := tree.CompactMetadataBatchV7WithOptions(
		nil,
		CompactMetadataV7Options{},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "toolchain profile") {
		t.Fatalf("CompactMetadataBatchV7WithOptions() error = %v, want toolchain profile error", err)
	}
}

func compactV7ConfigNames(configs []CompactConfigV7) []string {
	out := make([]string, 0, len(configs))
	for _, config := range configs {
		out = append(out, config.Name)
	}
	return out
}

func compactV7ObjectsByName(
	objects []CompactObjectVariantV7,
) map[string][]CompactObjectVariantV7 {
	out := map[string][]CompactObjectVariantV7{}
	for _, object := range objects {
		out[object.Object] = append(out[object.Object], object)
	}
	return out
}

func compactV7SingleObject(
	t *testing.T,
	objects map[string][]CompactObjectVariantV7,
	name string,
) CompactObjectVariantV7 {
	t.Helper()
	if len(objects[name]) != 1 {
		t.Fatalf("%s variants = %#v, want one", name, objects[name])
	}
	return objects[name][0]
}

func compactV7RecipeByID(
	metadata *CompactMetadataV7,
	id string,
) CompactActionRecipe {
	for _, recipe := range metadata.ActionRecipes {
		if recipe.ID == id {
			return recipe
		}
	}
	return CompactActionRecipe{}
}

func compactV7RecipeGroupByID(
	metadata *CompactMetadataV7,
	id string,
) CompactActionRecipeGroup {
	for _, group := range metadata.ActionRecipeGroups {
		if group.ID == id {
			return group
		}
	}
	return CompactActionRecipeGroup{}
}

func compactV7ProgramByID(
	metadata *CompactMetadataV7,
	id string,
) CompactKbuildFlagProgram {
	for _, program := range metadata.FlagPrograms {
		if program.ID == id {
			return program
		}
	}
	return CompactKbuildFlagProgram{}
}

func compactV7TerminalByID(
	metadata *CompactMetadataV7,
	id string,
) CompactKbuildFlagTerminal {
	for _, terminal := range metadata.FlagTerminals {
		if terminal.ID == id {
			return terminal
		}
	}
	return CompactKbuildFlagTerminal{}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func compactV7ExpandedSetForTest(
	t *testing.T,
	metadata *CompactMetadataV7,
	id string,
) map[int]bool {
	t.Helper()
	sets := map[string]CompactSourceSet{}
	for _, sourceSet := range metadata.SourceSets {
		sets[sourceSet.ID] = sourceSet
	}
	out := map[int]bool{}
	var visit func(string)
	visit = func(id string) {
		sourceSet := sets[id]
		for _, file := range sourceSet.Files {
			out[file] = true
		}
		for _, child := range sourceSet.Children {
			visit(child)
		}
	}
	visit(id)
	return out
}

func TestCompactV7JSONInitializesCollections(t *testing.T) {
	metadata := &CompactMetadataV7{
		Protocol:              CompactMetadataProtocolV7,
		ToolchainProfile:      "llvm-test/x86",
		CompileEnvironmentABI: "linux.bzl/compact-v7/test",
	}
	data, err := metadata.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal JSON(): %v", err)
	}
	for _, field := range []string{
		"compile_environments",
		"generated_header_families",
		"source_files",
		"source_sets",
		"action_source_groups",
		"kbuild_probes",
		"flag_terminals",
		"flag_nodes",
		"flag_programs",
		"reachability_signatures",
		"action_recipes",
		"action_recipe_groups",
		"object_variants",
	} {
		value, ok := decoded[field]
		if !ok {
			t.Errorf("JSON() omitted top-level collection %q", field)
			continue
		}
		items, ok := value.([]any)
		if !ok || items == nil || len(items) != 0 {
			t.Errorf("JSON() field %q = %#v, want empty array", field, value)
		}
	}
}
