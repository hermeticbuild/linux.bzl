package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestEncodeRecipeCommandReplaysExpandsTypedPaths(t *testing.T) {
	replays := []kconfig.ActionRecipeCommandReplay{{
		Name: "make",
		Invocations: []kconfig.ActionRecipeCommandReplayInvocation{{
			Arguments: []string{"-f", "${tree:kernel}/scripts/Makefile.build", "obj=init"},
			Outputs:   []string{"${work:root}/init/version-timestamp.o"},
		}},
	}}
	arguments, err := encodeRecipeCommandReplays(replays, func(value string) (string, error) {
		return strings.NewReplacer("${tree:kernel}", "/source", "${work:root}", "/work").Replace(value), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(arguments) != 2 || arguments[0] != "-replay_base64" {
		t.Fatalf("encoded replay arguments=%q", arguments)
	}
	data, err := base64.StdEncoding.DecodeString(arguments[1])
	if err != nil {
		t.Fatal(err)
	}
	var got kconfig.ActionRecipeCommandReplay
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	wantArguments := []string{"-f", "/source/scripts/Makefile.build", "obj=init"}
	if !slices.Equal(got.Invocations[0].Arguments, wantArguments) ||
		!slices.Equal(got.Invocations[0].Outputs, []string{"/work/init/version-timestamp.o"}) {
		t.Fatalf("expanded replay=%#v", got)
	}
}

func writeRecipe(t *testing.T, recipe kconfig.ActionRecipe) (string, string) {
	t.Helper()
	data, err := recipe.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	id, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, id
}

func writeInputBindings(t *testing.T, bindings kconfig.ActionPlanInputBindings) (string, string) {
	t.Helper()
	data, err := bindings.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	id, err := bindings.ID()
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), id+".json")
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return filename, id
}

func TestResolveRecipeInputBindingsExpandsLargeContentAddressedManifest(t *testing.T) {
	const inputCount = 12000
	root := t.TempDir()
	recipeInputs := make([]string, inputCount)
	bindings := make(map[string]kconfig.ActionPlanInputBinding, inputCount)
	for ordinal := range inputCount {
		name := fmt.Sprintf("input:%08d", ordinal)
		recipeInputs[ordinal] = name
		bindings[name] = kconfig.ActionPlanInputBinding{
			Tree: "objects",
			Path: fmt.Sprintf(".linux-bzl-versions/node-%08d/output.o", ordinal),
		}
	}
	manifest, id := writeInputBindings(t, kconfig.ActionPlanInputBindings{
		Schema:   kconfig.LinuxKernelInputBindingsSchema,
		Bindings: bindings,
	})
	got, err := resolveRecipeInputBindings(recipeInputs, recipeOptions{
		inputBindings: manifest, expectedInputBindingsID: id,
		artifactTrees: map[string]string{"objects": root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != inputCount {
		t.Fatalf("resolved input count = %d, want %d", len(got), inputCount)
	}
	for _, ordinal := range []int{0, inputCount / 2, inputCount - 1} {
		name := recipeInputs[ordinal]
		want := filepath.Join(root, ".linux-bzl-versions", fmt.Sprintf("node-%08d", ordinal), "output.o")
		if got[name] != want {
			t.Fatalf("resolved input %q = %q, want %q", name, got[name], want)
		}
	}
}

func TestRunRecipeConsumesContentAddressedInputBindingsManifest(t *testing.T) {
	artifactRoot := t.TempDir()
	inputRelative := "generated/value.txt"
	input := filepath.Join(artifactRoot, filepath.FromSlash(inputRelative))
	if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("manifest-selected input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output.txt")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "copy", Tool: "helper",
		Arguments: []string{"${input:payload:00000000}", "${output:00000000}"},
		Inputs:    []string{"payload:00000000"}, Outputs: []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	manifest, manifestID := writeInputBindings(t, kconfig.ActionPlanInputBindings{
		Schema: kconfig.LinuxKernelInputBindingsSchema,
		Bindings: map[string]kconfig.ActionPlanInputBinding{
			"payload:00000000": {Tree: "objects", Path: inputRelative},
		},
	})
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -eu\ncp \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "copy", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		inputBindings: manifest, expectedInputBindingsID: manifestID,
		artifactTrees: map[string]string{"objects": artifactRoot},
		toolRole:      "helper", tools: map[string]string{"helper": helper},
		sources: map[string]string{}, outputs: map[string]string{"00000000": output}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "manifest-selected input\n" {
		t.Fatalf("manifest-backed recipe output = %q", got)
	}
}

func TestResolveRecipeInputBindingsRequiresExclusiveExactRootsAndInputs(t *testing.T) {
	root := t.TempDir()
	bindingName := "state:00000000"
	manifest, id := writeInputBindings(t, kconfig.ActionPlanInputBindings{
		Schema: kconfig.LinuxKernelInputBindingsSchema,
		Bindings: map[string]kconfig.ActionPlanInputBinding{
			bindingName: {Tree: "objects", Path: "generated/state.json"},
		},
	})
	base := recipeOptions{
		inputBindings: manifest, expectedInputBindingsID: id,
		artifactTrees: map[string]string{"objects": root},
	}
	for _, test := range []struct {
		name string
		opts recipeOptions
		want string
	}{
		{name: "direct input conflict", opts: func() recipeOptions {
			opts := base
			opts.inputs = map[string]string{bindingName: filepath.Join(root, "direct")}
			return opts
		}(), want: "cannot be combined with direct input bindings"},
		{name: "missing manifest", opts: recipeOptions{
			expectedInputBindingsID: id,
			artifactTrees:           map[string]string{"objects": root},
		}, want: "require an input bindings manifest"},
		{name: "missing expected ID", opts: recipeOptions{
			inputBindings: manifest,
			artifactTrees: map[string]string{"objects": root},
		}, want: "requires an expected input bindings ID"},
		{name: "missing root", opts: recipeOptions{
			inputBindings: manifest, expectedInputBindingsID: id,
			artifactTrees: map[string]string{},
		}, want: `missing artifact tree root "objects"`},
		{name: "unexpected root", opts: recipeOptions{
			inputBindings: manifest, expectedInputBindingsID: id,
			artifactTrees: map[string]string{"objects": root, "unused": root},
		}, want: `unexpected artifact tree root "unused"`},
		{name: "missing physical root", opts: recipeOptions{
			inputBindings: manifest, expectedInputBindingsID: id,
			artifactTrees: map[string]string{"objects": filepath.Join(root, "missing")},
		}, want: `inspect artifact tree root "objects"`},
		{name: "manifest input differs from recipe", opts: base, want: `unexpected input manifest binding "state:00000000"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipeInputs := []string{bindingName}
			if test.name == "manifest input differs from recipe" {
				recipeInputs = []string{"different:00000000"}
			}
			if _, err := resolveRecipeInputBindings(recipeInputs, test.opts); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveRecipeInputBindings error = %v, want %q", err, test.want)
			}
		})
	}

	regularRoot := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regularRoot, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRecipeInputBindings([]string{bindingName}, recipeOptions{
		inputBindings: manifest, expectedInputBindingsID: id,
		artifactTrees: map[string]string{"objects": regularRoot},
	}); err == nil || !strings.Contains(err.Error(), `artifact tree root "objects" is not a directory`) {
		t.Fatalf("regular artifact root error = %v", err)
	}
}

func TestPreparePrivateInputTreeProjectionsExposesOnlyExactNodeInputs(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	selected := filepath.Join(shared, "include", "selected.h")
	unrelated := filepath.Join(shared, "include", "unrelated.h")
	for filename, content := range map[string]string{selected: "selected\n", unrelated: "unrelated\n"} {
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, id := writeInputBindings(t, kconfig.ActionPlanInputBindings{
		Schema: kconfig.LinuxKernelInputBindingsSchema,
		Bindings: map[string]kconfig.ActionPlanInputBinding{
			"input:00000000": {Tree: "prep", Path: "include/selected.h"},
		},
	})
	opts := recipeOptions{
		inputBindings:           manifest,
		expectedInputBindingsID: id,
		artifactTrees:           map[string]string{"prep": shared},
		inputs:                  map[string]string{},
		trees:                   map[string]string{"prep": shared},
		privateInputTrees:       map[string]bool{"prep": true},
	}
	resolved, err := resolveRecipeInputBindings([]string{"input:00000000"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.inputs = resolved
	cleanup, err := preparePrivateInputTreeProjections([]string{"input:00000000"}, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if opts.trees["prep"] == shared {
		t.Fatal("private tree retained the shared current-stage root")
	}
	data, err := os.ReadFile(filepath.Join(opts.trees["prep"], "include", "selected.h"))
	if err != nil || string(data) != "selected\n" {
		t.Fatalf("projected selected input = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(opts.trees["prep"], "include", "unrelated.h")); !os.IsNotExist(err) {
		t.Fatalf("private tree exposed unrelated sibling: %v", err)
	}
}

func TestDecodeInputBindingsRejectsInvalidFilesAndTampering(t *testing.T) {
	valid := kconfig.ActionPlanInputBindings{
		Schema: kconfig.LinuxKernelInputBindingsSchema,
		Bindings: map[string]kconfig.ActionPlanInputBinding{
			"input:00000000": {Tree: "objects", Path: "first.o"},
		},
	}
	manifest, id := writeInputBindings(t, valid)
	tampered := valid
	tampered.Bindings = map[string]kconfig.ActionPlanInputBinding{
		"input:00000000": {Tree: "objects", Path: "second.o"},
	}
	tamperedData, err := tampered.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, tamperedData, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeInputBindings(manifest, id); err == nil || !strings.Contains(err.Error(), "content ID") {
		t.Fatalf("tampered manifest error = %v, want content ID mismatch", err)
	}

	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, kconfig.MaxActionPlanInputBindingsBytes+1); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		filename string
		want     string
	}{
		{name: "directory", filename: t.TempDir(), want: "not a regular file"},
		{name: "oversized", filename: oversized, want: "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeInputBindings(test.filename, strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeInputBindings error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveRecipeInputBindingsRejectsTraversal(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "traversal.json")
	data := []byte(`{"schema":"linux-kernel-input-bindings-v1","bindings":{"input:00000000":{"tree":"objects","path":"../escape"}}}` + "\n")
	if err := os.WriteFile(manifest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveRecipeInputBindings([]string{"input:00000000"}, recipeOptions{
		inputBindings: manifest, expectedInputBindingsID: strings.Repeat("a", 64),
		artifactTrees: map[string]string{"objects": t.TempDir()},
	})
	if err == nil || (!strings.Contains(err.Error(), "canonical relative path") && !strings.Contains(err.Error(), "escapes")) {
		t.Fatalf("traversal manifest error = %v", err)
	}
}

func TestRunRecipeExpandsBindingsAndSplicesToolchainAction(t *testing.T) {
	tree := t.TempDir()
	source := filepath.Join(tree, "selected.c")
	output := filepath.Join(t.TempDir(), "out", "selected.o")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments:   []string{"-I${tree:kernel}/include", "-DVALUE=${source:src:00000000}", "-o", "${output:00000000}"},
		Environment: map[string]string{"PLAN_SOURCE": "${source:src:00000000}"},
		Sources:     []string{"src:00000000"}, Outputs: []string{"00000000"}, Trees: []string{"kernel"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	compiler := filepath.Join(t.TempDir(), "compiler")
	log := filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$PLAN_SOURCE\" \"$@\" > \"$RECIPE_TEST_LOG\"\ntouch \"$5\"\n"
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECIPE_TEST_LOG", log)
	err := runRecipe(recipeOptions{recipe: recipePath, kind: "compile", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID, toolRole: "cc",
		sources: map[string]string{"src:00000000": source}, outputs: map[string]string{"00000000": output}, tools: map[string]string{"cc": compiler}, trees: map[string]string{"kernel": tree}, inputs: map[string]string{},
		actionArgs: []string{"--prefix", kconfig.LinuxKbuildArgsSentinel, "--suffix"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := source + "\n--prefix\n-I" + tree + "/include\n-DVALUE=" + source + "\n-o\n" + output + "\n--suffix\n"
	if string(got) != want {
		t.Fatalf("argv=%q want=%q", got, want)
	}
}

func TestRunRecipeUsesOnlySelectedRoleEnvironment(t *testing.T) {
	t.Setenv("LINUX_BZL_AMBIENT_SECRET", "must-not-leak")
	output := filepath.Join(t.TempDir(), "environment")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"${output:00000000}"},
		Environment: map[string]string{
			"RECIPE_VALUE": "${output:00000000}",
		},
		Outputs: []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$SELECTED_ROLE_VALUE\" \"${LINUX_BZL_AMBIENT_SECRET-unset}\" \"$RECIPE_VALUE\" > \"$1\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper}, trees: map[string]string{},
		actionEnvironment: map[string]string{"SELECTED_ROLE_VALUE": "exact-role-environment"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := "exact-role-environment\nunset\n" + output + "\n"
	if string(got) != want {
		t.Fatalf("environment output = %q, want %q", got, want)
	}
}

func TestRunRecipeUsesConfiguredRuntimeToolForCompilerSubprocess(t *testing.T) {
	directory := t.TempDir()
	ambientDirectory := filepath.Join(directory, "ambient-bin")
	if err := os.Mkdir(ambientDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDirectory, "ld"), []byte("#!/bin/sh\nexit 71\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambientDirectory)

	output := filepath.Join(directory, "linked")
	workRoot := filepath.Join(directory, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"${output:00000000}"}, Outputs: []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	compiler := filepath.Join(directory, "cc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexec ld \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linker := filepath.Join(directory, "selected-linker")
	if err := os.WriteFile(linker, []byte("#!/bin/sh\nprintf '%s' selected-runtime-linker > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "compile", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "cc", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"cc": compiler}, runtimeTools: map[string]string{"ld": linker}, trees: map[string]string{},
		actionEnvironment: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "selected-runtime-linker" {
		t.Fatalf("linked output = %q, want selected runtime linker", got)
	}
}

func TestRunRecipePrependsRuntimeToolsToConfiguredAndRecipePath(t *testing.T) {
	for _, test := range []struct {
		name              string
		configuredPath    string
		recipeSelectsPath bool
	}{
		{name: "configured action PATH"},
		{name: "recipe PATH", configuredPath: "/configured/path-must-not-win", recipeSelectsPath: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			suffixDirectory := filepath.Join(directory, "configured-bin")
			if err := os.Mkdir(suffixDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(suffixDirectory, "ld"), []byte("#!/bin/sh\nexit 72\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(suffixDirectory, "fallback"), []byte("#!/bin/sh\nprintf '%s\\n' configured-fallback >> \"$1\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			output := filepath.Join(directory, "path-result")
			workRoot := filepath.Join(directory, "work")
			recipeEnvironment := map[string]string{}
			configuredPath := test.configuredPath
			if test.recipeSelectsPath {
				recipeEnvironment["PATH"] = suffixDirectory
			} else {
				configuredPath = suffixDirectory
			}
			recipe := kconfig.ActionRecipe{
				Schema: kconfig.LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
				Arguments: []string{"${output:00000000}"}, Outputs: []string{"00000000"}, Environment: recipeEnvironment,
			}
			recipePath, recipeID := writeRecipe(t, recipe)
			compiler := filepath.Join(directory, "cc")
			compilerScript := "#!/bin/sh\nprintf '%s\\n' \"$PATH\" > \"$1\"\nld \"$1\"\nfallback \"$1\"\n"
			if err := os.WriteFile(compiler, []byte(compilerScript), 0o755); err != nil {
				t.Fatal(err)
			}
			linker := filepath.Join(directory, "selected-linker")
			if err := os.WriteFile(linker, []byte("#!/bin/sh\nprintf '%s\\n' runtime-linker >> \"$1\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := runRecipe(recipeOptions{
				recipe: recipePath, kind: "compile", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
				toolRole: "cc", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
				sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
				tools: map[string]string{"cc": compiler}, runtimeTools: map[string]string{"ld": linker}, trees: map[string]string{},
				actionEnvironment: map[string]string{"PATH": configuredPath},
			}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDirectory := filepath.Join(workRoot, ".linux-bzl-tool-runtime", "bin")
			want := runtimeDirectory + string(os.PathListSeparator) + suffixDirectory + "\nruntime-linker\nconfigured-fallback\n"
			if string(got) != want {
				t.Fatalf("PATH/runtime output = %q, want %q", got, want)
			}
		})
	}
}

func TestPrepareRuntimeToolDirectoryRejectsInvalidBindings(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "tool")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"", ".", "..", "nested/ld", "ld:plugin", "ld tool"} {
		t.Run("role_"+strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(role), func(t *testing.T) {
			if _, _, err := prepareRuntimeToolDirectory(directory, map[string]string{role: executable}); err == nil {
				t.Fatalf("runtime role %q was accepted", role)
			}
		})
	}
	if _, _, err := prepareRuntimeToolDirectory("", map[string]string{"ld": executable}); err == nil || !strings.Contains(err.Error(), "private working-directory root") {
		t.Fatalf("missing private root error = %v", err)
	}
	nonExecutable := filepath.Join(directory, "not-executable")
	if err := os.WriteFile(nonExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareRuntimeToolDirectory(directory, map[string]string{"ld": nonExecutable}); err == nil || !strings.Contains(err.Error(), "executable regular file") {
		t.Fatalf("non-executable runtime tool error = %v", err)
	}
	if _, err := namedBindings([]string{"ld=" + executable, "ld=" + executable}); err == nil || !strings.Contains(err.Error(), "repeated binding") {
		t.Fatalf("duplicate runtime binding error = %v", err)
	}
}

func TestPrepareRuntimeToolDirectoryRejectsPrivateRuntimeCollision(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "tool")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, ".linux-bzl-tool-runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareRuntimeToolDirectory(directory, map[string]string{"ld": executable}); err == nil || !strings.Contains(err.Error(), "create private runtime-tool root") {
		t.Fatalf("private runtime collision error = %v", err)
	}
}

func TestRunRecipeDoesNotExposeUnprojectedHostCompilerToScriptRunner(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "environment")
	ambientHostCompiler := filepath.Join(directory, "ambient-hostcc")
	if err := os.WriteFile(ambientHostCompiler, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOSTCC", ambientHostCompiler)
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments:      []string{"${output:00000000}", "${tool:script-runtime}"},
		Outputs:        []string{"00000000"},
		AuxiliaryTools: []string{"script-runtime"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	scriptRunner := filepath.Join(directory, "scriptrun")
	if err := os.WriteFile(scriptRunner, []byte(`#!/bin/sh
if [ "${HOSTCC+x}" = x ]; then
	"$HOSTCC"
	exit 65
fi
printf '%s' hostcc-omitted > "$1"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(directory, "runtime")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "scriptrun", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output},
		tools:   map[string]string{"scriptrun": scriptRunner, "script-runtime": runtime}, trees: map[string]string{},
		actionEnvironment: map[string]string{},
		auxiliaryActionContracts: map[string]toolaction.Contract{
			"script-runtime": {Arguments: []string{}, Environment: map[string]string{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hostcc-omitted" {
		t.Fatalf("script runner environment output = %q, want omitted HOSTCC", got)
	}
}

func TestRunRecipeForwardsAuxiliaryActionContracts(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "contract")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"${tool:frobnicator}", "${output:00000000}"}, Outputs: []string{"00000000"},
		AuxiliaryTools: []string{"frobnicator"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(directory, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s' \"$"+toolaction.EnvironmentName+"\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	frobnicator := filepath.Join(directory, "frobnicator")
	if err := os.WriteFile(frobnicator, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string]toolaction.Contract{
		"frobnicator": {
			Arguments:   []string{"prefix", toolaction.KbuildArgumentsSentinel, "suffix"},
			Environment: map[string]string{"SELECTED_ENV": "exact=value"},
		},
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper, "frobnicator": frobnicator}, trees: map[string]string{},
		actionEnvironment: map[string]string{}, auxiliaryActionContracts: want,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got, err := toolaction.Decode(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if got["frobnicator"].Environment["SELECTED_ENV"] != "exact=value" || !slices.Equal(got["frobnicator"].Arguments, want["frobnicator"].Arguments) {
		t.Fatalf("forwarded contract=%#v, want %#v", got, want)
	}
}

func TestParseAuxiliaryActionContractsPreservesOrderAndEquals(t *testing.T) {
	got, err := parseAuxiliaryActionContracts(
		[]string{"cc"},
		[]string{"cc=prefix=one", "cc=" + toolaction.KbuildArgumentsSentinel, "cc=suffix"},
		[]string{"cc=VALUE=with=equals", "cc=EMPTY="},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"prefix=one", toolaction.KbuildArgumentsSentinel, "suffix"}; !slices.Equal(got["cc"].Arguments, want) {
		t.Fatalf("arguments=%q, want %q", got["cc"].Arguments, want)
	}
	if got["cc"].Environment["VALUE"] != "with=equals" || got["cc"].Environment["EMPTY"] != "" {
		t.Fatalf("environment=%q", got["cc"].Environment)
	}
}

func TestRunRecipeExpandsGeneratedContentWithGNUMakeShellSemantics(t *testing.T) {
	dir := t.TempDir()
	queryResult := filepath.Join(dir, "query-result")
	if err := os.WriteFile(queryResult, []byte("160\r\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"-DSTACK_OFFSET=${content:offset}", "${output:00000000}"},
		Inputs:    []string{"query"}, Outputs: []string{"00000000"},
		ContentSubstitutions: map[string]kconfig.ActionRecipeContentSubstitution{
			"offset": {Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellWord},
		},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{"query": queryResult},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-DSTACK_OFFSET=160"; string(got) != want {
		t.Fatalf("expanded generated content = %q, want %q", got, want)
	}
}

func TestGNUMakeShellValueNormalizesCRLFAndRejectsBareCarriageReturns(t *testing.T) {
	got, err := kconfig.NormalizeActionRecipeMakeShellValue("one\r\ntwo\r\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "one two"; got != want {
		t.Fatalf("normalized value = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "middle", input: "one\rtwo\n"},
		{name: "trailing", input: "one\r"},
		{name: "before-terminal-crlf", input: "one\r\r\n"},
	} {
		t.Run("reject-bare-carriage-return-"+test.name, func(t *testing.T) {
			if _, err := kconfig.NormalizeActionRecipeMakeShellValue(test.input); err == nil {
				t.Fatal("normalization succeeded")
			}
		})
	}
}

func TestRunRecipeExecutesGNUMakeShellSingleWordSizeAppendFormat(t *testing.T) {
	dir := t.TempDir()
	queryResult := filepath.Join(dir, "size-append-query")
	// Linux's size_append query prints two backslashes before each octal byte.
	// The recipe shell consumes one level of quoting, leaving printf with one
	// backslash per escape in its single format-word argument.
	if err := os.WriteFile(queryResult, []byte("\\\\004\\\\003\\\\002\\\\001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "size-bytes")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "sh",
		Arguments: []string{"-c", `printf ${content:size} > "$1"`, "linux-bzl-size-append", "${output:00000000}"},
		Inputs:    []string{"query"}, Outputs: []string{"00000000"},
		ContentSubstitutions: map[string]kconfig.ActionRecipeContentSubstitution{
			"size": {Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellSingleWord},
		},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "sh", sources: map[string]string{}, inputs: map[string]string{"query": queryResult},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"sh": "/bin/sh"}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{4, 3, 2, 1}; !slices.Equal(got, want) {
		t.Fatalf("size bytes = %v, want %v", got, want)
	}
}

func TestRunRecipeMaterializesGNUMakeShellSingleQuotedSegment(t *testing.T) {
	dir := t.TempDir()
	queryResult := filepath.Join(dir, "saved-command-query")
	if err := os.WriteFile(queryResult, []byte("\\\\004\\\\003\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "saved-command")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "sh",
		Arguments: []string{"-c", `printf '%s' 'savedcmd_result := emit ${content:value}' > "$1"`, "linux-bzl-saved-command", "${output:00000000}"},
		Inputs:    []string{"query"}, Outputs: []string{"00000000"},
		ContentSubstitutions: map[string]kconfig.ActionRecipeContentSubstitution{
			"value": {
				Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellSingleQuotedSegment,
			},
		},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "sh", sources: map[string]string{}, inputs: map[string]string{"query": queryResult},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"sh": "/bin/sh"}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := `savedcmd_result := emit \\004\\003`; string(got) != want {
		t.Fatalf("saved-command bytes = %q, want %q", got, want)
	}
}

func TestGNUMakeShellSingleQuotedSegmentTransformIsInvariantAndBounded(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "number", input: "4096\n", want: "4096"},
		{name: "backslash-quoted-octal", input: "\\\\004\\\\003\r\n", want: `\\004\\003`},
		{name: "double-quoted-static-word", input: `"two words"` + "\n", want: `"two words"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := kconfig.FormatActionRecipeMakeShellSingleQuotedSegment(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("transformed segment = %q, want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "dollar", input: `'$value'`},
		{name: "pound", input: `word\#value`},
		{name: "single-quote", input: `'word'`},
		{name: "multiword", input: "one two\n"},
		{name: "operator", input: "one;"},
		{name: "middle-bare-carriage-return", input: "one\rtwo"},
		{name: "trailing-bare-carriage-return", input: "one\r"},
		{name: "bare-carriage-return-before-terminal-crlf", input: "one\r\r\n"},
		{name: "nul", input: "bad\x00word"},
	} {
		t.Run("reject-"+test.name, func(t *testing.T) {
			if _, err := kconfig.FormatActionRecipeMakeShellSingleQuotedSegment(test.input); err == nil {
				t.Fatal("transform succeeded")
			}
		})
	}
}

func TestGNUMakeShellSingleWordTransformIsStaticAndBounded(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "backslash-quoted-octal", input: "\\\\004\\\\003\r\n", want: `'\004\003'`},
		{name: "quoted-whitespace", input: `"two words"` + "\n", want: `'two words'`},
		{name: "escaped-whitespace", input: "two\\ words\n", want: `'two words'`},
		{name: "quoted-literal-dollar", input: `'$value'`, want: `'$value'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := kconfig.QuoteActionRecipeMakeShellSingleWord(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("transformed word = %q, want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "empty", input: "\n"},
		{name: "multiple-words", input: "one two\n"},
		{name: "normalized-multiple-lines", input: "one\ntwo\n"},
		{name: "operator", input: "one;"},
		{name: "parameter-expansion", input: "$value"},
		{name: "command-substitution", input: "$(command)"},
		{name: "backtick-substitution", input: "`command`"},
		{name: "pathname-expansion", input: "*.o"},
		{name: "tilde-expansion", input: "~/source"},
		{name: "comment", input: "#ignored"},
		{name: "middle-bare-carriage-return", input: "one\rtwo"},
		{name: "trailing-bare-carriage-return", input: "one\r"},
		{name: "bare-carriage-return-before-terminal-crlf", input: "one\r\r\n"},
		{name: "reserved-marker", input: "\x01linux-bzl-literal-dollar\x02"},
		{name: "nul", input: "bad\x00word"},
	} {
		t.Run("reject-"+test.name, func(t *testing.T) {
			if _, err := kconfig.QuoteActionRecipeMakeShellSingleWord(test.input); err == nil {
				t.Fatal("transform succeeded")
			}
		})
	}
}

func TestRecipeContentSubstitutionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	bindings := map[string]map[string]string{"source": {}, "input": {}}
	for name, contents := range map[string][]byte{
		"empty":                            {},
		"newline-only":                     []byte("\r\n\n"),
		"nul":                              []byte("unsafe\x00value"),
		"multiword":                        []byte("160\nunsafe"),
		"shell":                            []byte("$(unsafe)"),
		"middle-bare-carriage-return":      []byte("one\rtwo"),
		"trailing-bare-carriage-return":    []byte("one\r"),
		"bare-carriage-return-before-crlf": []byte("one\r\r\n"),
		"oversized":                        make([]byte, maxRecipeContentSubstitutionBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(dir, name)
			if err := os.WriteFile(filename, contents, 0o644); err != nil {
				t.Fatal(err)
			}
			bindings["input"][name] = filename
			_, err := materializeRecipeContentSubstitutions(map[string]kconfig.ActionRecipeContentSubstitution{
				"value": {Input: "input:" + name, Transform: kconfig.ActionRecipeContentTransformMakeShellWord},
			}, bindings)
			if err == nil {
				t.Fatal("materializeRecipeContentSubstitutions succeeded")
			}
		})
	}
}

func TestExpandContentTemplateRejectsGeneratedTreeMarkers(t *testing.T) {
	for _, test := range []struct {
		name     string
		template string
		contents map[string]string
	}{
		{name: "complete marker", template: "${content:value}", contents: map[string]string{"value": "${tree:kernel}"}},
		{name: "template-boundary split", template: "${content:value}ee:kernel}", contents: map[string]string{"value": "${tr"}},
		{name: "two-binding split", template: "${content:left}${content:right}", contents: map[string]string{"left": "${tr", "right": "ee:kernel}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := expandContentTemplate(test.template, map[string]map[string]string{
				"content": test.contents,
				"tree":    {"kernel": "/declared/kernel"},
			})
			if err == nil || !strings.Contains(err.Error(), "generated content created reserved ${tree: prefix") {
				t.Fatalf("expandContentTemplate error = %v, want generated tree-prefix rejection", err)
			}
		})
	}

	got, err := expandContentTemplate(
		"printf ${content:value} ${tree:kernel}/source.c",
		map[string]map[string]string{
			"content": {"value": "generated-value"},
			"tree":    {"kernel": "/declared/kernel"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "printf generated-value ${tree:kernel}/source.c"; got != want {
		t.Fatalf("expanded literal tree template = %q, want %q", got, want)
	}
}

func TestRunRecipeRejectsEnvironmentOnlyContentTransformBeforeExecution(t *testing.T) {
	directory := t.TempDir()
	queryResult := filepath.Join(directory, "query-result")
	if err := os.WriteFile(queryResult, []byte(`touch "$OUT"`), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "helper-ran")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"printf ${content:value}", "${output:00000000}"},
		ArgumentTransforms: []kconfig.ActionRecipeArgumentTransform{{
			Index: 0, Transform: kconfig.ActionRecipeArgumentTransformContentTemplateBase64,
		}},
		Inputs: []string{"query"}, Outputs: []string{"00000000"},
		ContentSubstitutions: map[string]kconfig.ActionRecipeContentSubstitution{
			"value": {Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellValue},
		},
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipePath := filepath.Join(directory, "recipe.json")
	if err := os.WriteFile(recipePath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(directory, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: strings.Repeat("b", 64),
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{"query": queryResult},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper}, trees: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "environment-only") {
		t.Fatalf("runRecipe error = %v, want environment-only template rejection", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("helper ran before unsafe template was rejected: %v", statErr)
	}
}

func TestGeneratedTreeSpellingIsSafeOutsideContentTemplates(t *testing.T) {
	directory := t.TempDir()
	queryResult := filepath.Join(directory, "query-result")
	if err := os.WriteFile(queryResult, []byte("${tree:literal}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	values, err := materializeRecipeContentSubstitutions(
		map[string]kconfig.ActionRecipeContentSubstitution{
			"value": {Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellValue},
		},
		map[string]map[string]string{"source": {}, "input": {"query": queryResult}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["value"], "${tree:literal}"; got != want {
		t.Fatalf("direct generated value = %q, want %q", got, want)
	}
}

func TestExpandValueTreatsReplacementAsData(t *testing.T) {
	got, err := expandValue("prefix-${content:value}", map[string]map[string]string{
		"content": {"value": "${output:must-stay-literal}"},
		"output":  {"must-stay-literal": "injected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "prefix-${output:must-stay-literal}"; got != want {
		t.Fatalf("expandValue() = %q, want %q", got, want)
	}
}

func TestRunRecipeBase64ContentTemplatePreservesDownstreamTreePlaceholder(t *testing.T) {
	directory := t.TempDir()
	queryResult := filepath.Join(directory, "query-result")
	if err := os.WriteFile(queryResult, []byte("generated-value\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(directory, "kernel")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "captured-arguments")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "scriptrun",
		Arguments: []string{
			"-script_content_base64", `printf '%s' ${content:value} ${tree:kernel}/source.c`,
			"-tree", "kernel=${tree:kernel}",
		},
		ArgumentTransforms: []kconfig.ActionRecipeArgumentTransform{{
			Index: 1, Transform: kconfig.ActionRecipeArgumentTransformContentTemplateBase64,
		}},
		Environment: map[string]string{"CAPTURE": "${output:00000000}"},
		Inputs:      []string{"query"}, Outputs: []string{"00000000"}, Trees: []string{"kernel"},
		ContentSubstitutions: map[string]kconfig.ActionRecipeContentSubstitution{
			"value": {Input: "input:query", Transform: kconfig.ActionRecipeContentTransformMakeShellWord},
		},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	scriptRunner := filepath.Join(directory, "scriptrun")
	if err := os.WriteFile(scriptRunner, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "scriptrun", sources: map[string]string{}, inputs: map[string]string{"query": queryResult},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"scriptrun": scriptRunner},
		trees: map[string]string{"kernel": tree},
	}); err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(captured), "\x00"), "\x00")
	encodedIndex := slices.Index(arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 == len(arguments) {
		t.Fatalf("captured arguments = %q", arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(arguments[encodedIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `printf '%s' generated-value ${tree:kernel}/source.c`; string(decoded) != want {
		t.Fatalf("decoded downstream content = %q, want %q", decoded, want)
	}
	treeIndex := slices.Index(arguments, "-tree")
	if treeIndex < 0 || treeIndex+1 == len(arguments) || arguments[treeIndex+1] != "kernel="+tree {
		t.Fatalf("captured downstream tree binding = %q, want kernel=%s", arguments, tree)
	}
}

func TestRunRecipeTreatsRegularTreeMarkerAsItsParent(t *testing.T) {
	tree := t.TempDir()
	marker := filepath.Join(tree, "Kconfig")
	if err := os.WriteFile(marker, []byte("# root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "tree-path")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"${tree:kernel}/include", "${output:00000000}"},
		Outputs:   []string{"00000000"}, Trees: []string{"kernel"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper},
		trees: map[string]string{"kernel": marker},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(tree, "include"); string(got) != want {
		t.Fatalf("expanded tree = %q, want %q", got, want)
	}
}

func TestRunRecipeStaticAndGeneratedToolsUseExactRecipeArguments(t *testing.T) {
	for _, test := range []struct {
		name     string
		tool     string
		toolRole string
		toolKey  string
	}{
		{name: "static helper", tool: "initramfsdata", toolRole: "initramfsdata", toolKey: "initramfsdata"},
		{name: "generated executable", tool: "input:helper", toolRole: "generated", toolKey: "helper"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "out", "generated")
			recipe := kconfig.ActionRecipe{
				Schema:    kconfig.LinuxKernelPlanSchema,
				Kind:      "generate",
				Tool:      test.tool,
				Arguments: []string{"--exact", "${output:00000000}"},
				Outputs:   []string{"00000000"},
			}
			inputs := map[string]string{}
			tools := map[string]string{}
			helper := filepath.Join(t.TempDir(), "helper")
			log := filepath.Join(t.TempDir(), "argv")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECIPE_TEST_LOG\"\ntouch \"$2\"\n"
			mode := os.FileMode(0o755)
			if test.toolRole == "generated" {
				mode = 0o644
			}
			if err := os.WriteFile(helper, []byte(script), mode); err != nil {
				t.Fatal(err)
			}
			if test.toolRole == "generated" {
				recipe.Inputs = []string{test.toolKey}
				inputs[test.toolKey] = helper
			} else {
				tools[test.toolKey] = helper
			}
			recipePath, recipeID := writeRecipe(t, recipe)
			t.Setenv("RECIPE_TEST_LOG", log)
			workRoot := filepath.Join(t.TempDir(), "work")
			err := runRecipe(recipeOptions{
				recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
				toolRole: test.toolRole, sources: map[string]string{}, inputs: inputs,
				outputs: map[string]string{"00000000": output}, tools: tools, trees: map[string]string{},
				workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			want := "--exact\n" + output + "\n"
			if string(got) != want {
				t.Fatalf("argv=%q want=%q", got, want)
			}
		})
	}
}

func TestRunRecipeMaterializesExecutableInputPrivately(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "result")
	subtool := filepath.Join(dir, "immutable-subtool")
	if err := os.WriteFile(subtool, []byte("#!/bin/sh\nprintf 'executed\\n' > \"$1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "driver")
	if err := os.WriteFile(driver, []byte("#!/bin/sh\n\"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "driver",
		Arguments:        []string{"${input:subtool}", "${output:00000000}"},
		Inputs:           []string{"subtool"},
		ExecutableInputs: []string{"subtool"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	workRoot := filepath.Join(dir, "work")
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "driver", sources: map[string]string{}, inputs: map[string]string{"subtool": subtool},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"driver": driver}, trees: map[string]string{},
		workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "executed\n" {
		t.Fatalf("output = %q", got)
	}
	info, err := os.Stat(subtool)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("immutable input mode = %o, want 644", info.Mode().Perm())
	}
}

func TestRunRecipeUsesAndCleansMarkerDerivedWorkingDirectory(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out", "pwd")
	workRoot := filepath.Join(t.TempDir(), "work", "node")
	workMarker := filepath.Join(workRoot, ".linux-bzl-work-root")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${output:00000000}"},
		WorkingDirectory: "nested/path",
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s' \"$PWD\" > \"$1\"\ntouch scratch\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: workMarker,
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workRoot, "nested", "path")
	if string(got) != want {
		t.Fatalf("working directory=%q want %q", got, want)
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".linux-bzl-work-root" || entries[0].IsDir() {
		t.Fatalf("private work root entries=%v", entries)
	}
}

func TestRunRecipeCreatesDeclaredWorkingDirectoriesBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "out", "result")
	workRoot := filepath.Join(dir, "work", "node")
	workMarker := filepath.Join(workRoot, ".linux-bzl-work-root")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:          []string{"${output:00000000}"},
		WorkingDirectory:   "object-tree",
		WorkingDirectories: []string{"scripts", "include/generated/nested"},
		Outputs:            []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -e\ntest -d scripts\ntest -d include/generated/nested\nprintf 'directories existed\\n' > \"$1\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: workMarker,
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "directories existed\n" {
		t.Fatalf("output = %q", got)
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".linux-bzl-work-root" || entries[0].IsDir() {
		t.Fatalf("private work root entries=%v", entries)
	}
}

func TestRunRecipeRejectsSymlinkWorkingOutput(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("undeclared bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out", "result")
	workRoot := filepath.Join(dir, "work", "node")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{outside},
		WorkingDirectory: "object-tree",
		WorkingOutputs:   map[string]string{"00000000": "generated/result"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -e\nmkdir -p generated\nln -s \"$1\" generated/result\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file created in the private working tree") {
		t.Fatalf("symlink working output error = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("symlink working output was collected: %v", err)
	}
}

func TestRunRecipeRejectsSymlinkAncestorOfWorkingOutput(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out", "result")
	workRoot := filepath.Join(dir, "work", "node")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{outside},
		WorkingDirectory: "object-tree",
		WorkingOutputs:   map[string]string{"00000000": "generated/result"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -e\nrm -rf generated\nln -s \"$1\" generated\nprintf 'escaped bytes\\n' > generated/result\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "traverses symlink ancestor") {
		t.Fatalf("symlink-ancestor working output error = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("symlink-ancestor working output was collected: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "result")); err != nil || string(got) != "escaped bytes\n" {
		t.Fatalf("outside sentinel = %q, %v", got, err)
	}
}

func TestRunRecipeRejectsSymlinkAncestorOfObservedOutput(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "result"), []byte("outside bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out", "state")
	workRoot := filepath.Join(dir, "work", "node")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{outside},
		WorkingDirectory: "object-tree",
		ObservedOutputs:  map[string]string{"capture": "generated/result"},
		Outputs:          []string{"capture"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -e\nln -s \"$1\" generated\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"capture": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "traverses symlink ancestor") {
		t.Fatalf("symlink-ancestor observed output error = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("symlink-ancestor observed state was collected: %v", err)
	}
}

func TestRunRecipeProjectsDeclaredWorkingOutputsAndDiscardsScratch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "immutable-input")
	if err := os.WriteFile(source, []byte("exact staged bytes\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	subtool := filepath.Join(dir, "immutable-subtool")
	if err := os.WriteFile(subtool, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "declared", "result")
	workRoot := filepath.Join(dir, "work", "node")
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workRoot, "stale"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"staged/source", "generated/result", "${work:root}"},
		WorkingDirectory: "source-owned",
		WorkingInputs:    map[string]string{"input:data": "staged/source"},
		WorkingOutputs:   map[string]string{"00000000": "generated/result"},
		Inputs:           []string{"data", "subtool"},
		ExecutableInputs: []string{"subtool"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -e\ntest \"$3\" = \"$PWD\"\ntest -d generated\nmkdir -p dynamic\ncp \"$1\" \"$2\"\nprintf '%s' \"$PWD\" > observed.cwd\nprintf 'side effect\\n' > dynamic/side\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{"data": source, "subtool": subtool}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{output} {
		got, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read preserved file %s: %v", filename, err)
		}
		if string(got) != "exact staged bytes\n" {
			t.Fatalf("preserved file %s = %q", filename, got)
		}
	}
	workingDirectory := filepath.Join(workRoot, "source-owned")
	if _, err := os.Stat(workingDirectory); !os.IsNotExist(err) {
		t.Fatalf("private working directory survived declared-output projection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workRoot, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale pre-action subtree entry still exists: %v", err)
	}
	if info, err := os.Stat(filepath.Join(workRoot, ".linux-bzl-work-root")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private work marker = %v, %v", info, err)
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".linux-bzl-generated-tool-") {
			t.Fatalf("preserved subtree retained private generated-tool copy %q", entry.Name())
		}
	}
}

func writeObservedOutputStateFile(t *testing.T, filename string, state toolaction.ObservedOutputState) {
	t.Helper()
	data, err := toolaction.EncodeObservedOutputState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecipeEmitsDeterministicAbsoluteObservedOutputStates(t *testing.T) {
	dir := t.TempDir()
	baseWriter := strings.Repeat("b", 64)
	nodeWriter := strings.Repeat("a", 64)
	inputs := map[string]string{}
	for name, content := range map[string]string{
		"unchanged": "same bytes\n",
		"changed":   "before bytes\n",
		"deleted":   "delete me\n",
		"mode":      "mode-only change\n",
	} {
		binding := "base-" + name
		filename := filepath.Join(dir, binding)
		writeObservedOutputStateFile(t, filename, toolaction.ObservedOutputState{
			Disposition: toolaction.ObservedOutputPresent,
			Writer:      baseWriter,
			Content:     []byte(content),
		})
		inputs[binding] = filename
	}

	observed := map[string]string{
		"absent":    "generated/absent.mod.c",
		"unchanged": "candidates/unchanged.mod.c",
		"changed":   "candidates/changed.mod.c",
		"deleted":   "candidates/deleted.mod.c",
		"created":   "generated/created.mod.c",
		"mode":      "candidates/mode.mod.c",
	}
	bases := map[string][]string{
		"absent": nil, "created": nil,
	}
	inputBindings := []string{}
	for _, name := range []string{"unchanged", "changed", "deleted", "mode"} {
		binding := "base-" + name
		bases[name] = []string{binding}
		inputBindings = append(inputBindings, binding)
	}
	outputs := map[string]string{}
	outputBindings := []string{"absent", "unchanged", "changed", "deleted", "created", "mode"}
	for _, binding := range outputBindings {
		outputs[binding] = filepath.Join(dir, "states", binding)
	}
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		WorkingDirectory:    "module-lds",
		ObservedOutputs:     observed,
		ObservedOutputBases: bases,
		Inputs:              inputBindings,
		Outputs:             outputBindings,
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -eu\n" +
		"test ! -e generated\n" +
		"mkdir -p generated\n" +
		"printf 'same bytes\\n' > candidates/unchanged.mod.c\n" +
		"printf 'after bytes\\n' > candidates/changed.mod.c\n" +
		"rm candidates/deleted.mod.c\n" +
		"printf 'created bytes\\n' > generated/created.mod.c\n" +
		"chmod 0101 generated/created.mod.c\n" +
		"chmod 0111 candidates/mode.mod.c\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(dir, "work")
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: nodeWriter, expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: inputs, outputs: outputs,
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	want := map[string]toolaction.ObservedOutputState{
		"absent":    {Disposition: toolaction.ObservedOutputAbsent},
		"unchanged": {Disposition: toolaction.ObservedOutputPresent, Writer: baseWriter, Content: []byte("same bytes\n")},
		"changed":   {Disposition: toolaction.ObservedOutputPresent, Writer: nodeWriter, Content: []byte("after bytes\n")},
		"deleted":   {Disposition: toolaction.ObservedOutputDeleted, Writer: nodeWriter},
		"created":   {Disposition: toolaction.ObservedOutputPresent, Writer: nodeWriter, Content: []byte("created bytes\n"), ExecutableMode: 0o101},
		"mode":      {Disposition: toolaction.ObservedOutputPresent, Writer: nodeWriter, Content: []byte("mode-only change\n"), ExecutableMode: 0o111},
	}
	for _, binding := range outputBindings {
		data, err := os.ReadFile(outputs[binding])
		if err != nil {
			t.Fatalf("read state %s: %v", binding, err)
		}
		got, err := toolaction.DecodeObservedOutputState(data)
		if err != nil {
			t.Fatalf("decode state %s: %v", binding, err)
		}
		if expected := want[binding]; got.Disposition != expected.Disposition || got.Writer != expected.Writer || got.ExecutableMode != expected.ExecutableMode || string(got.Content) != string(expected.Content) {
			t.Fatalf("state %s = %#v, want %#v", binding, got, expected)
		}
		info, err := os.Stat(outputs[binding])
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("state %s mode = %v, %v; want 0644", binding, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workRoot, "module-lds")); !os.IsNotExist(err) {
		t.Fatalf("observed work tree survived cleanup: %v", err)
	}
}

func TestRunRecipeUsesPrivateCanonicalArgumentsFile(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("input bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments:     []string{"literal", "${input:data}", "${output:result}"},
		ArgumentsFile: true,
		Stdout:        "result",
		Inputs:        []string{"data"},
		Outputs:       []string{"result"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "actionfile")
	script := "#!/bin/sh\nset -eu\n" +
		"test \"$#\" -eq 2\n" +
		"test \"$1\" = -arguments_file\n" +
		"test -f \"$2\"\n" +
		"test ! -x \"$2\"\n" +
		"cat \"$2\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(dir, "work")
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "actionfile", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{"data": input}, outputs: map[string]string{"result": output},
		tools: map[string]string{"actionfile": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var arguments []string
	if err := json.Unmarshal(data, &arguments); err != nil {
		t.Fatalf("decode captured arguments file: %v", err)
	}
	want := []string{"literal", input, output}
	if !slices.Equal(arguments, want) {
		t.Fatalf("expanded arguments = %q, want %q", arguments, want)
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".linux-bzl-work-root" {
		t.Fatalf("private response file survived cleanup: %v", entries)
	}
}

func runObservedStateRecipe(
	t *testing.T,
	dir string,
	name string,
	writer string,
	script string,
	baseStates []string,
) string {
	t.Helper()
	inputs := make(map[string]string, len(baseStates))
	inputBindings := make([]string, len(baseStates))
	for ordinal, filename := range baseStates {
		binding := fmt.Sprintf("base-%d", ordinal)
		inputBindings[ordinal] = binding
		inputs[binding] = filename
	}
	output := filepath.Join(dir, "state-"+name)
	recipe := kconfig.ActionRecipe{
		Schema:           kconfig.LinuxKernelPlanSchema,
		Kind:             "generate",
		Tool:             "helper",
		WorkingDirectory: "observed-state",
		ObservedOutputs:  map[string]string{"capture": "state/value"},
		ObservedOutputBases: map[string][]string{
			"capture": inputBindings,
		},
		Inputs:  inputBindings,
		Outputs: []string{"capture"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper-"+name)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(dir, "work-"+name)
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: writer, expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: inputs, outputs: map[string]string{"capture": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	return output
}

func decodeObservedOutputState(t *testing.T, filename string) toolaction.ObservedOutputState {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	state, err := toolaction.DecodeObservedOutputState(data)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRunRecipeThreadsObservedCreateThenDeleteState(t *testing.T) {
	dir := t.TempDir()
	createWriter := strings.Repeat("a", 64)
	deleteWriter := strings.Repeat("b", 64)
	inheritedWriter := strings.Repeat("c", 64)
	createdPath := runObservedStateRecipe(t, dir, "create", createWriter, ""+
		"test ! -e state\n"+
		"mkdir -p state\n"+
		"printf created > state/value\n", nil)
	if created := decodeObservedOutputState(t, createdPath); created.Disposition != toolaction.ObservedOutputPresent || created.Writer != createWriter || string(created.Content) != "created" {
		t.Fatalf("create state = %#v", created)
	}

	deletedPath := runObservedStateRecipe(t, dir, "delete", deleteWriter, ""+
		"test \"$(cat state/value)\" = created\n"+
		"rm state/value\n", []string{createdPath})
	if deleted := decodeObservedOutputState(t, deletedPath); deleted.Disposition != toolaction.ObservedOutputDeleted || deleted.Writer != deleteWriter {
		t.Fatalf("delete state = %#v", deleted)
	}

	inheritedPath := runObservedStateRecipe(t, dir, "inherit-delete", inheritedWriter, ""+
		"test ! -e state\n", []string{deletedPath})
	if inherited := decodeObservedOutputState(t, inheritedPath); inherited.Disposition != toolaction.ObservedOutputDeleted || inherited.Writer != deleteWriter {
		t.Fatalf("inherited deletion state = %#v, want writer %s preserved", inherited, deleteWriter)
	}
}

func TestRunRecipeThreadsObservedAppendState(t *testing.T) {
	dir := t.TempDir()
	firstWriter := strings.Repeat("a", 64)
	secondWriter := strings.Repeat("b", 64)
	thirdWriter := strings.Repeat("c", 64)
	firstPath := runObservedStateRecipe(t, dir, "append-first", firstWriter, ""+
		"test ! -e state\n"+
		"mkdir -p state\n"+
		"printf alpha > state/value\n"+
		"chmod 0744 state/value\n", nil)
	secondPath := runObservedStateRecipe(t, dir, "append-second", secondWriter, ""+
		"test \"$(cat state/value)\" = alpha\n"+
		"test -x state/value\n"+
		"printf +beta >> state/value\n", []string{firstPath})
	if second := decodeObservedOutputState(t, secondPath); second.Disposition != toolaction.ObservedOutputPresent || second.Writer != secondWriter || string(second.Content) != "alpha+beta" || second.ExecutableMode != 0o100 {
		t.Fatalf("second append state = %#v", second)
	}
	thirdPath := runObservedStateRecipe(t, dir, "append-third", thirdWriter, ""+
		"test \"$(cat state/value)\" = alpha+beta\n"+
		"test -x state/value\n"+
		"printf +gamma >> state/value\n", []string{secondPath})
	if third := decodeObservedOutputState(t, thirdPath); third.Disposition != toolaction.ObservedOutputPresent || third.Writer != thirdWriter || string(third.Content) != "alpha+beta+gamma" || third.ExecutableMode != 0o100 {
		t.Fatalf("third append state = %#v", third)
	}
}

func TestMergeObservedOutputBaseRejectsInvalidStates(t *testing.T) {
	dir := t.TempDir()
	writerA := strings.Repeat("a", 64)
	writerB := strings.Repeat("b", 64)
	presentA := filepath.Join(dir, "present-a")
	presentAConflict := filepath.Join(dir, "present-a-conflict")
	presentB := filepath.Join(dir, "present-b")
	writeObservedOutputStateFile(t, presentA, toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputPresent, Writer: writerA, Content: []byte("first")})
	writeObservedOutputStateFile(t, presentAConflict, toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputPresent, Writer: writerA, Content: []byte("second")})
	writeObservedOutputStateFile(t, presentB, toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputPresent, Writer: writerB, Content: []byte("first")})
	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte("not a state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		base   []string
		inputs map[string]string
		want   string
	}{
		{
			name: "malformed state", base: []string{"bad"},
			inputs: map[string]string{"bad": malformed}, want: "decode state ordinal 0",
		},
		{
			name: "unordered writers", base: []string{"first", "second"},
			inputs: map[string]string{"first": presentA, "second": presentB}, want: "unordered writers",
		},
		{
			name: "inconsistent same writer", base: []string{"first", "second"},
			inputs: map[string]string{"first": presentA, "second": presentAConflict}, want: "inconsistent states",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mergeObservedOutputBase(test.base, test.inputs); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("base merge error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeObservedOutputAbsenceDoesNotCreateParent(t *testing.T) {
	for _, state := range []toolaction.ObservedOutputState{
		{Disposition: toolaction.ObservedOutputAbsent},
		{Disposition: toolaction.ObservedOutputDeleted, Writer: strings.Repeat("a", 64)},
	} {
		parent := filepath.Join(t.TempDir(), "absent-parent")
		if err := materializeObservedOutputState(state, filepath.Join(parent, "value")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(parent); !os.IsNotExist(err) {
			t.Fatalf("absent observed output parent was created: %v", err)
		}
	}
}

func TestRunRecipeObservedAbsentPreservesStagedInputAndDeletedRemovesIt(t *testing.T) {
	for _, test := range []struct {
		name       string
		base       toolaction.ObservedOutputState
		helperBody string
	}{
		{
			name:       "absent preserves staged input",
			base:       toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputAbsent},
			helperBody: `test "$(cat state/value)" = staged`,
		},
		{
			name: "deleted removes staged input",
			base: toolaction.ObservedOutputState{
				Disposition: toolaction.ObservedOutputDeleted,
				Writer:      strings.Repeat("b", 64),
			},
			helperBody: `test ! -e state/value`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			staged := filepath.Join(dir, "staged")
			if err := os.WriteFile(staged, []byte("staged\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, "base.state")
			writeObservedOutputStateFile(t, base, test.base)
			output := filepath.Join(dir, "observed.state")
			recipe := kconfig.ActionRecipe{
				Schema:           kconfig.LinuxKernelPlanSchema,
				Kind:             "generate",
				Tool:             "helper",
				WorkingDirectory: "observed-overlap",
				WorkingInputs: map[string]string{
					"input:staged": "state/value",
				},
				ObservedOutputs: map[string]string{
					"capture": "state/value",
				},
				ObservedOutputBases: map[string][]string{
					"capture": {"base"},
				},
				Inputs:  []string{"staged", "base"},
				Outputs: []string{"capture"},
			}
			recipePath, recipeID := writeRecipe(t, recipe)
			helper := filepath.Join(dir, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -eu\n"+test.helperBody+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			writer := strings.Repeat("a", 64)
			workRoot := filepath.Join(dir, "work")
			if err := runRecipe(recipeOptions{
				recipe: recipePath, kind: "generate", expectedNodeID: writer, expectedRecipeID: recipeID,
				toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
				sources: map[string]string{}, inputs: map[string]string{"staged": staged, "base": base},
				outputs: map[string]string{"capture": output}, tools: map[string]string{"helper": helper}, trees: map[string]string{},
			}); err != nil {
				t.Fatal(err)
			}
			if got := decodeObservedOutputState(t, output); got.Disposition != test.base.Disposition || got.Writer != test.base.Writer {
				t.Fatalf("unchanged observed state = %#v, want base %#v", got, test.base)
			}
		})
	}
}

func TestSnapshotObservedRegularFileRejectsNonRegularPaths(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := snapshotObservedRegularFile(regular); err != nil || !got.present || string(got.content) != "bytes" || got.executableMode != 0o111 {
		t.Fatalf("regular snapshot = %#v, %v", got, err)
	}
	if got, err := snapshotObservedRegularFile(filepath.Join(dir, "missing")); err != nil || got.present {
		t.Fatalf("missing snapshot = %#v, %v", got, err)
	}
	if _, err := snapshotObservedRegularFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory snapshot error = %v", err)
	}
	if err := os.Symlink(regular, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotObservedRegularFile(filepath.Join(dir, "symlink")); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink snapshot error = %v", err)
	}
}

func TestRunRecipeResolvesVersionedOutputPlaceholderAtLogicalWorkingPath(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "declared", ".linux-bzl-versions", "first", "generated", "shared.o")
	workRoot := filepath.Join(dir, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${output:00000000}"},
		WorkingDirectory: "writer",
		WorkingOutputs:   map[string]string{"00000000": "generated/shared.o"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -e\ntest \"$1\" = \"$PWD/generated/shared.o\"\nprintf 'logical path bytes\\n' > \"$1\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "logical path bytes\n"; got != want {
		t.Fatalf("versioned physical output = %q, want %q", got, want)
	}
}

func TestRunRecipeCollectsVersionedStdoutFromLogicalWorkingPath(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "declared", ".linux-bzl-versions", "first", "generated", "stdout")
	workRoot := filepath.Join(dir, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{output},
		WorkingDirectory: "writer",
		WorkingOutputs:   map[string]string{"00000000": "generated/stdout"},
		Stdout:           "00000000",
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	// The declared physical destination must not exist until the private
	// logical stdout has been collected after the command exits.
	script := "#!/bin/sh\nset -e\ntest ! -e \"$1\"\nprintf 'logical stdout bytes\\n'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("b", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "logical stdout bytes\n"; got != want {
		t.Fatalf("versioned physical stdout = %q, want %q", got, want)
	}
}

func TestCopyRecipeFilePreservesExecutablePermission(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "generated-tool")
	destination := filepath.Join(directory, "staged", "generated-tool")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyRecipeFile(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("staged executable mode = %o", info.Mode().Perm())
	}
}

func TestRunRecipeKeepsRelativeExecrootBindingsAnchoredAfterChdir(t *testing.T) {
	execroot := t.TempDir()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeRoot, err := filepath.Rel(current, execroot)
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(relativeRoot, "tree")
	tool := filepath.Join(relativeRoot, "tools", "helper")
	output := filepath.Join(relativeRoot, "out", "result")
	workRoot := filepath.Join(relativeRoot, "work")
	for _, directory := range []string{tree, filepath.Dir(tool)} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "input"), []byte("tree input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ncat \"$1/input\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${tree:sdk}", "${output:00000000}"},
		WorkingDirectory: "nested", Outputs: []string{"00000000"}, Trees: []string{"sdk"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": tool}, trees: map[string]string{"sdk": tree},
		workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "tree input\n" {
		t.Fatalf("output = %q", data)
	}
}

func TestRunRecipeExpandsToolchainContractPathsBeforeWorkingDirectory(t *testing.T) {
	executionRoot := t.TempDir()
	t.Chdir(executionRoot)
	sysroot := filepath.Join(executionRoot, "toolchain", "sysroot")
	if err := os.MkdirAll(filepath.Join(sysroot, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysroot, "sys", "select.h"), []byte("declared header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	header := filepath.Join(sysroot, "sys", "select.h")
	output := filepath.Join(executionRoot, "out", "contract")
	workRoot := filepath.Join(executionRoot, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments: []string{"${output:00000000}", "${tool:auxiliary}"}, Outputs: []string{"00000000"},
		AuxiliaryTools: []string{"auxiliary"}, WorkingDirectory: "nested",
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(executionRoot, "helper")
	script := "#!/bin/sh\n" +
		"test -f \"$3/sys/select.h\" || exit 41\n" +
		"printf '%s\\n%s\\n%s' \"$PRIMARY_SYSROOT\" \"$3\" \"$" + toolaction.EnvironmentName + "\" > \"$1\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := toolaction.ExecutionRootMarker + "/toolchain/sysroot"
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", sources: map[string]string{}, inputs: map[string]string{},
		outputs: map[string]string{"00000000": output},
		tools:   map[string]string{"helper": helper, "auxiliary": helper}, trees: map[string]string{},
		workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		actionArgs:        []string{toolaction.KbuildArgumentsSentinel, markerPath},
		actionEnvironment: map[string]string{"PRIMARY_SYSROOT": markerPath},
		auxiliaryActionContracts: map[string]toolaction.Contract{
			"auxiliary": {
				Arguments: []string{toolaction.ExecutionRootMarker + "/toolchain/bin", toolaction.KbuildArgumentsSentinel},
				Environment: map[string]string{
					"SELECT_HEADER": toolaction.ExecutionRootMarker + "/toolchain/sysroot/sys/select.h",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) != 3 || lines[0] != sysroot || lines[1] != sysroot {
		t.Fatalf("expanded primary contract = %q, want sysroot %q", lines, sysroot)
	}
	contracts, err := toolaction.Decode(lines[2])
	if err != nil {
		t.Fatal(err)
	}
	contract := contracts["auxiliary"]
	if contract.Arguments[0] != filepath.Join(executionRoot, "toolchain", "bin") || contract.Environment["SELECT_HEADER"] != header {
		t.Fatalf("expanded auxiliary contract = %#v", contract)
	}
	if strings.Contains(string(data), toolaction.ExecutionRootMarker) {
		t.Fatalf("reserved execution-root marker survived expansion: %q", data)
	}
}

func TestExpandExecutionRootActionValueRejectsMalformedMarker(t *testing.T) {
	for _, value := range []string{
		toolaction.ExecutionRootMarker,
		"-I" + toolaction.ExecutionRootMarker + "relative",
	} {
		if _, err := expandExecutionRootActionValue(value, "/execroot"); err == nil {
			t.Errorf("expandExecutionRootActionValue(%q) succeeded", value)
		}
	}
}

func TestRunRecipeStagesAndCollectsWorkingFiles(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "immutable-input")
	if err := os.WriteFile(source, []byte("exact staged bytes\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "declared", "result")
	workRoot := filepath.Join(dir, "work")
	workMarker := filepath.Join(workRoot, ".linux-bzl-work-root")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"staged/source", "generated/result"},
		WorkingDirectory: "modpost",
		WorkingInputs:    map[string]string{"input:data": "staged/source"},
		WorkingOutputs:   map[string]string{"00000000": "generated/result"},
		Inputs:           []string{"data"},
		Outputs:          []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nmkdir -p \"$(dirname \"$2\")\"\ncp \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: workMarker,
		sources: map[string]string{}, inputs: map[string]string{"data": source}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "exact staged bytes\n" {
		t.Fatalf("collected bytes = %q", got)
	}
}

func TestRunRecipeStagesWorkingTreesBeforeExactInputs(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "immutable-tree")
	const module = ".linux-bzl/external/demo"
	for filename, contents := range map[string]string{
		filepath.Join(tree, module, "Kbuild"):                "obj-m += demo.o\n",
		filepath.Join(tree, module, "include", "module.h"):   "complete tree header\n",
		filepath.Join(tree, module, "generated", "config.h"): "tree baseline\n",
		filepath.Join(tree, module, "scripts", "generate"):   "#!/bin/sh\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if filepath.Base(filename) == "generate" {
			mode = 0o755
		}
		if err := os.WriteFile(filename, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tree, module, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("module.h", filepath.Join(tree, module, "include", "module-link.h")); err != nil {
		t.Skipf("create file symlink: %v", err)
	}
	exact := filepath.Join(dir, "exact-config.h")
	if err := os.WriteFile(exact, []byte("exact config\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "declared", "result")
	workRoot := filepath.Join(dir, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:          []string{"${output:00000000}"},
		WorkingDirectory:   "overlay",
		ExecutionDirectory: module,
		WorkingTrees:       []string{"external"},
		WorkingInputs: map[string]string{
			"input:config": module + "/generated/config.h",
		},
		WorkingOutputs: map[string]string{"00000000": module + "/result"},
		Inputs:         []string{"config"},
		Outputs:        []string{"00000000"},
		Trees:          []string{"external"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := `#!/bin/sh
set -eu
test "$(cat Kbuild)" = 'obj-m += demo.o'
test "$(cat include/module.h)" = 'complete tree header'
test "$(cat include/module-link.h)" = 'complete tree header'
test ! -L include/module-link.h
test "$(cat generated/config.h)" = 'exact config'
test -x scripts/generate
test -d empty
printf 'complete staged tree\n' > "$1"
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot,
		workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources:                map[string]string{},
		inputs:                 map[string]string{"config": exact},
		outputs:                map[string]string{"00000000": output},
		tools:                  map[string]string{"helper": helper},
		trees:                  map[string]string{"external": tree},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "complete staged tree\n"; got != want {
		t.Fatalf("working-tree result = %q, want %q", got, want)
	}
	baseline, err := os.ReadFile(filepath.Join(tree, module, "generated", "config.h"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(baseline), "tree baseline\n"; got != want {
		t.Fatalf("immutable tree baseline changed to %q, want %q", got, want)
	}
}

func TestCopyRecipeTreeRejectsEscapingRegularFileSymlink(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func(string) string
	}{
		{name: "absolute", target: func(outside string) string { return outside }},
		{name: "parent traversal", target: func(string) string { return "../outside" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			tree := filepath.Join(dir, "tree")
			if err := os.MkdirAll(tree, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(dir, "outside")
			if err := os.WriteFile(outside, []byte("undeclared bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target(outside), filepath.Join(tree, "escape")); err != nil {
				t.Skipf("create file symlink: %v", err)
			}
			destination := filepath.Join(dir, "destination")
			err := copyRecipeTree(tree, destination)
			if err == nil || !strings.Contains(err.Error(), "resolves outside the declared tree") {
				t.Fatalf("escaping regular-file symlink error = %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(destination, "escape")); !os.IsNotExist(statErr) {
				t.Fatalf("escaping regular-file symlink was staged: %v", statErr)
			}
		})
	}
}

func TestCopyRecipeTreeRejectsOverlappingBaselineTrees(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  map[string]string
		second map[string]string
	}{
		{
			name:   "file and file",
			first:  map[string]string{"shared": "first\n"},
			second: map[string]string{"shared": "second\n"},
		},
		{
			name:   "file and directory",
			first:  map[string]string{"shared": "first\n"},
			second: map[string]string{"shared/child": "second\n"},
		},
		{
			name:   "directory and file",
			first:  map[string]string{"shared/child": "first\n"},
			second: map[string]string{"shared": "second\n"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree := func(root string, files map[string]string) {
				t.Helper()
				for relative, contents := range files {
					filename := filepath.Join(root, filepath.FromSlash(relative))
					if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			first := filepath.Join(dir, "first")
			second := filepath.Join(dir, "second")
			writeTree(first, test.first)
			writeTree(second, test.second)
			destination := filepath.Join(dir, "destination")
			if err := copyRecipeTree(first, destination); err != nil {
				t.Fatalf("stage first baseline: %v", err)
			}
			if err := copyRecipeTree(second, destination); err == nil {
				t.Fatal("overlapping second baseline was accepted")
			}
		})
	}
}

func TestRunRecipeRejectsWorkingTreeDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "real", "input"), []byte("input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(tree, "directory-link")); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}
	output := filepath.Join(dir, "output")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${output:00000000}"},
		WorkingDirectory: "overlay",
		WorkingTrees:     []string{"external"},
		Outputs:          []string{"00000000"},
		Trees:            []string{"external"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(dir, "work")
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot,
		workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources:                map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{"external": tree},
	})
	if err == nil || !strings.Contains(err.Error(), "symlink to a directory") {
		t.Fatalf("directory-symlink working tree error = %v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("helper ran after rejected working tree: %v", statErr)
	}
}

func TestRunRecipeRejectsRegularMarkerAsWorkingTree(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "Kconfig")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${output:00000000}"},
		WorkingDirectory: "overlay",
		WorkingTrees:     []string{"kernel"},
		Outputs:          []string{"00000000"},
		Trees:            []string{"kernel"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(dir, "work")
	err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot,
		workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources:                map[string]string{}, inputs: map[string]string{}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{"kernel": marker},
	})
	if err == nil || !strings.Contains(err.Error(), "regular root marker") {
		t.Fatalf("regular-marker working tree error = %v", err)
	}
}

func TestRunRecipePreservesSourceAndRebindsGeneratedWorkingInputs(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "immutable", "gen_crc32table.c")
	checkedIn := filepath.Join(dir, "immutable", "vdso2c.h")
	generated := filepath.Join(dir, "declared", "autoconf.h")
	for filename, contents := range map[string]string{
		source:    "source bytes\n",
		checkedIn: "checked-in header\n",
		generated: "generated config\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "output", "result")
	workRoot := filepath.Join(dir, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:        []string{"${source:unit}", "${input:config}", "${work:root}", "${output:00000000}"},
		WorkingDirectory: "kernel",
		WorkingInputs: map[string]string{
			"source:unit":  "lib/crc/gen_crc32table.c",
			"input:config": "include/generated/autoconf.h",
		},
		WorkingOutputs: map[string]string{"00000000": "lib/crc/gen_crc32table"},
		Sources:        []string{"unit"},
		Inputs:         []string{"config"},
		Outputs:        []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -eu\n" +
		"test \"$1\" = '" + source + "'\n" +
		"test \"$(cat \"$(dirname \"$1\")/vdso2c.h\")\" = 'checked-in header'\n" +
		"test \"$2\" = \"$PWD/include/generated/autoconf.h\"\n" +
		"test \"$3\" = \"$PWD\"\n" +
		"test \"$(cat \"$3/lib/crc/../../include/generated/autoconf.h\")\" = 'generated config'\n" +
		"cp \"$2\" \"$4\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot, workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources: map[string]string{"unit": source}, inputs: map[string]string{"config": generated},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "generated config\n" {
		t.Fatalf("collected bytes = %q", got)
	}
}

func TestRunRecipeExecutesFromTypedDirectoryWithoutRewritingArguments(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "declared", "main.o")
	if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("object bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output", "result")
	workRoot := filepath.Join(dir, "work")
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		Arguments:          []string{"main.o", "main.o", "${output:00000000}"},
		WorkingDirectory:   "object-tree",
		ExecutionDirectory: "drivers/example",
		WorkingInputs:      map[string]string{"input:object": "drivers/example/main.o"},
		Inputs:             []string{"object"},
		Outputs:            []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n%s\\n%s\\n' \"$(cat \"$1\")\" \"$2\" \"$PWD\" > \"$3\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: workRoot,
		workingDirectoryMarker: filepath.Join(workRoot, ".linux-bzl-work-root"),
		sources:                map[string]string{}, inputs: map[string]string{"object": input}, outputs: map[string]string{"00000000": output},
		tools: map[string]string{"helper": helper}, trees: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := "object bytes\nmain.o\n" + filepath.Join(workRoot, "object-tree", "drivers", "example") + "\n"
	if string(data) != want {
		t.Fatalf("typed-cwd output = %q, want %q", data, want)
	}
}

func TestRunRecipeBindsExactStdinAndStdout(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(input, []byte("stdin bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := kconfig.ActionRecipe{
		Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "input:filter",
		Stdin: "input:data", Stdout: "00000000", Inputs: []string{"filter", "data"}, Outputs: []string{"00000000"},
	}
	recipePath, recipeID := writeRecipe(t, recipe)
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRecipe(recipeOptions{
		recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: recipeID,
		toolRole: "generated", sources: map[string]string{}, inputs: map[string]string{"filter": helper, "data": input},
		outputs: map[string]string{"00000000": output}, tools: map[string]string{}, trees: map[string]string{},
		workingDirectory: filepath.Join(dir, "work"), workingDirectoryMarker: filepath.Join(dir, "work", ".linux-bzl-work-root"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "stdin bytes\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunRecipeRejectsUnpairedOrBroadWorkingDirectory(t *testing.T) {
	for name, values := range map[string][2]string{
		"missing marker": {filepath.Join(t.TempDir(), "work"), ""},
		"broad root":     {".", ".linux-bzl-work-root"},
		"wrong marker":   {filepath.Join(t.TempDir(), "work"), filepath.Join(t.TempDir(), "marker")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := prepareWorkingDirectory(values[0], values[1]); err == nil {
				t.Fatal("prepareWorkingDirectory unexpectedly succeeded")
			}
		})
	}
}

func TestRunRecipeRejectsTamperingAndBindings(t *testing.T) {
	recipe := kconfig.ActionRecipe{Schema: kconfig.LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy", Arguments: []string{"${input:src:00000000}", "${output:00000000}"}, Inputs: []string{"src:00000000"}, Outputs: []string{"00000000"}}
	path, id := writeRecipe(t, recipe)
	base := recipeOptions{recipe: path, kind: "copy", expectedNodeID: strings.Repeat("a", 64), expectedRecipeID: id, toolRole: "objcopy", inputs: map[string]string{"src:00000000": "in"}, outputs: map[string]string{"00000000": "out"}, tools: map[string]string{"objcopy": "tool"}, sources: map[string]string{}, trees: map[string]string{}, actionArgs: []string{kconfig.LinuxKbuildArgsSentinel}}
	t.Run("hash", func(t *testing.T) {
		opts := base
		opts.expectedRecipeID = strings.Repeat("b", 64)
		if err := runRecipe(opts); err == nil || !strings.Contains(err.Error(), "content ID") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("extra binding", func(t *testing.T) {
		opts := base
		opts.inputs = map[string]string{"src:00000000": "in", "extra": "bad"}
		if err := runRecipe(opts); err == nil || !strings.Contains(err.Error(), "unexpected input") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestCopyTreeFileValidatesManifestAndSymlinks(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "drivers"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(tree, "drivers", "module.ko")
	if err := os.WriteFile(source, []byte("module"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "manifest")
	if err := os.WriteFile(manifest, []byte("drivers/module.ko\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "projected.ko")
	if err := copyTreeFile(tree, "drivers/module.ko", manifest, out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "module" {
		t.Fatalf("got %q", data)
	}
	if err := copyTreeFile(tree, "../escape", manifest, out); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := os.Symlink(source, filepath.Join(tree, "drivers", "link.ko")); err != nil {
		t.Fatal(err)
	}
	if err := copyTreeFile(tree, "drivers/link.ko", "", out); err != nil {
		t.Fatalf("final Bazel-style symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tree, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "real", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tree, "real"), filepath.Join(tree, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := copyTreeFile(tree, "dirlink/file", "", out); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}
