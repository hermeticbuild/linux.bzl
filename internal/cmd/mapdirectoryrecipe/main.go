// mapdirectoryrecipe executes a content-addressed v4 Kconfig/Kbuild recipe.
// It knows nothing about compiler families or kernel configuration symbols;
// the execution-time planner supplies the complete argv and dependency set.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string         { return strings.Join(*f, " ") }
func (f *repeatedFlag) Set(value string) error { *f = append(*f, value); return nil }

type recipeOptions struct {
	recipe, kind, expectedNodeID, expectedRecipeID     string
	toolRole, workingDirectory, workingDirectoryMarker string
	inputBindings, expectedInputBindingsID             string
	sources, inputs, outputs, tools, trees             map[string]string
	artifactTrees                                      map[string]string
	privateInputTrees                                  map[string]bool
	runtimeTools                                       map[string]string
	actionArgs                                         []string
	actionEnvironment                                  map[string]string
	auxiliaryActionContracts                           map[string]toolaction.Contract
}

func decodeRecipe(filename string) (kconfig.ActionRecipe, []byte, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return kconfig.ActionRecipe{}, nil, fmt.Errorf("read recipe: %w", err)
	}
	var recipe kconfig.ActionRecipe
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recipe); err != nil {
		return recipe, nil, fmt.Errorf("decode recipe: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return recipe, nil, fmt.Errorf("decode recipe: trailing JSON value")
		}
		return recipe, nil, fmt.Errorf("decode recipe: %w", err)
	}
	canonical, err := recipe.CanonicalJSON()
	if err != nil {
		return recipe, nil, fmt.Errorf("validate recipe: %w", err)
	}
	if string(data) != string(canonical) {
		return recipe, nil, fmt.Errorf("recipe is not canonically encoded")
	}
	return recipe, canonical, nil
}

func decodeInputBindings(filename, expectedID string) (kconfig.ActionPlanInputBindings, error) {
	if filename == "" {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("input bindings manifest is required")
	}
	if !isDigest(expectedID) {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("expected input bindings ID must be a canonical SHA-256 digest")
	}
	file, err := os.Open(filename)
	if err != nil {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("open input bindings manifest %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("stat input bindings manifest %q: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("input bindings manifest %q is not a regular file", filename)
	}
	if info.Size() > kconfig.MaxActionPlanInputBindingsBytes {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf(
			"input bindings manifest %q exceeds %d bytes", filename, kconfig.MaxActionPlanInputBindingsBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, kconfig.MaxActionPlanInputBindingsBytes+1))
	if err != nil {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("read input bindings manifest %q: %w", filename, err)
	}
	if len(data) > kconfig.MaxActionPlanInputBindingsBytes {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf(
			"input bindings manifest %q exceeds %d bytes", filename, kconfig.MaxActionPlanInputBindingsBytes,
		)
	}
	bindings, err := kconfig.DecodeActionPlanInputBindings(data)
	if err != nil {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("decode input bindings manifest %q: %w", filename, err)
	}
	actualID, err := bindings.ID()
	if err != nil {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf("identify input bindings manifest %q: %w", filename, err)
	}
	if actualID != expectedID {
		return kconfig.ActionPlanInputBindings{}, fmt.Errorf(
			"input bindings manifest content ID = %q, want %q", actualID, expectedID,
		)
	}
	return bindings, nil
}

// resolveRecipeInputBindings expands the compact, content-addressed per-node
// input table against its explicitly declared artifact-tree roots. Direct
// -input bindings remain supported for small callers, but the two protocols
// are deliberately exclusive so an argv binding cannot override the plan.
func resolveRecipeInputBindings(recipeInputs []string, opts recipeOptions) (map[string]string, error) {
	manifestMode := opts.inputBindings != "" || opts.expectedInputBindingsID != "" || len(opts.artifactTrees) != 0
	if !manifestMode {
		return opts.inputs, nil
	}
	if len(opts.inputs) != 0 {
		return nil, fmt.Errorf("input bindings manifest cannot be combined with direct input bindings")
	}
	if opts.inputBindings == "" {
		return nil, fmt.Errorf("artifact-tree input bindings require an input bindings manifest")
	}
	if opts.expectedInputBindingsID == "" {
		return nil, fmt.Errorf("input bindings manifest requires an expected input bindings ID")
	}

	manifest, err := decodeInputBindings(opts.inputBindings, opts.expectedInputBindingsID)
	if err != nil {
		return nil, err
	}
	logical := make(map[string]string, len(manifest.Bindings))
	for name, binding := range manifest.Bindings {
		logical[name] = binding.Path
	}
	if err := exactBindings("input manifest", recipeInputs, logical); err != nil {
		return nil, err
	}

	usedTrees := make(map[string]bool, len(opts.artifactTrees))
	for _, binding := range manifest.Bindings {
		usedTrees[binding.Tree] = true
	}
	usedTreeNames := make([]string, 0, len(usedTrees))
	for tree := range usedTrees {
		usedTreeNames = append(usedTreeNames, tree)
	}
	sort.Strings(usedTreeNames)
	for _, tree := range usedTreeNames {
		if opts.artifactTrees[tree] == "" {
			return nil, fmt.Errorf("missing artifact tree root %q", tree)
		}
	}
	for _, tree := range sortedKeys(opts.artifactTrees) {
		if !usedTrees[tree] {
			return nil, fmt.Errorf("unexpected artifact tree root %q", tree)
		}
	}
	for _, tree := range sortedKeys(opts.artifactTrees) {
		root := opts.artifactTrees[tree]
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("inspect artifact tree root %q: %w", tree, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("artifact tree root %q is not a directory", tree)
		}
	}

	resolved := make(map[string]string, len(manifest.Bindings))
	for _, name := range recipeInputs {
		binding := manifest.Bindings[name]
		if err := validateRelativePath(binding.Path); err != nil {
			return nil, fmt.Errorf("input binding %q artifact path: %w", name, err)
		}
		root := filepath.Clean(opts.artifactTrees[binding.Tree])
		filename := filepath.Join(root, filepath.FromSlash(binding.Path))
		contained, err := recipeTreeContains(root, filename)
		if err != nil {
			return nil, fmt.Errorf("resolve input binding %q beneath artifact tree %q: %w", name, binding.Tree, err)
		}
		if !contained {
			return nil, fmt.Errorf("input binding %q artifact path %q escapes tree %q", name, binding.Path, binding.Tree)
		}
		resolved[name] = filename
	}
	return resolved, nil
}

func preparePrivateInputTreeProjections(recipeInputs []string, opts *recipeOptions) (func(), error) {
	cleanup := func() {}
	if len(opts.privateInputTrees) == 0 {
		return cleanup, nil
	}
	if opts.inputBindings == "" || opts.expectedInputBindingsID == "" {
		return cleanup, fmt.Errorf("private input trees require an input bindings manifest")
	}
	manifest, err := decodeInputBindings(opts.inputBindings, opts.expectedInputBindingsID)
	if err != nil {
		return cleanup, err
	}
	declaredTrees := make(map[string]bool, len(opts.trees))
	for name := range opts.trees {
		declaredTrees[name] = true
	}
	for name := range opts.privateInputTrees {
		if !declaredTrees[name] {
			return cleanup, fmt.Errorf("private input tree %q has no declared tree binding", name)
		}
	}

	root, err := os.MkdirTemp("", "linux-bzl-input-trees-")
	if err != nil {
		return cleanup, fmt.Errorf("create private input-tree root: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	treeNames := make([]string, 0, len(opts.privateInputTrees))
	for name := range opts.privateInputTrees {
		treeNames = append(treeNames, name)
	}
	sort.Strings(treeNames)
	for ordinal, name := range treeNames {
		projection := filepath.Join(root, fmt.Sprintf("%08d", ordinal))
		if err := os.MkdirAll(projection, 0o700); err != nil {
			cleanup()
			return func() {}, fmt.Errorf("create private input tree %q: %w", name, err)
		}
		materialized := map[string]string{}
		for _, bindingName := range recipeInputs {
			binding, exists := manifest.Bindings[bindingName]
			if !exists || binding.Tree != name {
				continue
			}
			source := opts.inputs[bindingName]
			if source == "" {
				cleanup()
				return func() {}, fmt.Errorf("private input tree %q has no resolved input %q", name, bindingName)
			}
			destination := filepath.Join(projection, filepath.FromSlash(binding.Path))
			contained, err := recipeTreeContains(projection, destination)
			if err != nil || !contained {
				cleanup()
				return func() {}, fmt.Errorf("private input tree %q path %q escapes its projection", name, binding.Path)
			}
			if prior := materialized[binding.Path]; prior != "" {
				if filepath.Clean(prior) != filepath.Clean(source) {
					cleanup()
					return func() {}, fmt.Errorf("private input tree %q path %q has conflicting inputs", name, binding.Path)
				}
				continue
			}
			if err := copyRecipeFile(source, destination); err != nil {
				cleanup()
				return func() {}, fmt.Errorf("project private input tree %q path %q: %w", name, binding.Path, err)
			}
			materialized[binding.Path] = source
		}
		opts.trees[name] = projection
	}
	return cleanup, nil
}

func runRecipe(opts recipeOptions) error {
	executionRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve recipe execution root: %w", err)
	}
	for name, value := range map[string]string{
		"recipe": opts.recipe, "kind": opts.kind, "expected node ID": opts.expectedNodeID,
		"expected recipe ID": opts.expectedRecipeID, "tool role": opts.toolRole,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !isDigest(opts.expectedNodeID) || !isDigest(opts.expectedRecipeID) {
		return fmt.Errorf("expected node and recipe IDs must be canonical SHA-256 digests")
	}
	recipe, canonical, err := decodeRecipe(opts.recipe)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if got := hex.EncodeToString(digest[:]); got != opts.expectedRecipeID {
		return fmt.Errorf("recipe content ID = %q, want %q", got, opts.expectedRecipeID)
	}
	if recipe.Kind != opts.kind {
		return fmt.Errorf("recipe kind = %q, want %q", recipe.Kind, opts.kind)
	}
	if err := expandExecutionRootActionContracts(&opts, executionRoot); err != nil {
		return err
	}
	opts.inputs, err = resolveRecipeInputBindings(recipe.Inputs, opts)
	if err != nil {
		return err
	}
	if err := exactBindings("source", recipe.Sources, opts.sources); err != nil {
		return err
	}
	if err := exactBindings("input", recipe.Inputs, opts.inputs); err != nil {
		return err
	}
	if err := exactBindings("output", recipe.Outputs, opts.outputs); err != nil {
		return err
	}
	workingTreeBindings := make(map[string]bool, len(recipe.WorkingTrees))
	for _, name := range recipe.WorkingTrees {
		workingTreeBindings[name] = true
	}
	for name, root := range opts.trees {
		if opts.privateInputTrees[name] {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("inspect tree binding %s: %w", name, err)
		}
		if info.Mode().IsRegular() {
			if workingTreeBindings[name] {
				return fmt.Errorf("working tree binding %s is a regular root marker, not a directory", name)
			}
			// A source tree is represented by its root Kconfig File so Bazel
			// can keep the argument path-mappable. Recipes consume its parent
			// directory as ${tree:kernel}.
			opts.trees[name] = filepath.Dir(root)
		} else if !info.IsDir() {
			return fmt.Errorf("tree binding %s is neither a directory nor a regular root marker", name)
		}
	}
	if err := exactBindings("tree", recipe.Trees, opts.trees); err != nil {
		return err
	}
	if recipe.WorkingDirectory != "" || len(opts.runtimeTools) != 0 || recipe.ArgumentsFile || len(opts.privateInputTrees) != 0 {
		if err := absolutizeWorkingRecipeOptions(&opts); err != nil {
			return err
		}
	}
	if err := prepareWorkingDirectory(opts.workingDirectory, opts.workingDirectoryMarker); err != nil {
		return err
	}

	inputBindings := make(map[string]string, len(opts.inputs))
	for name, filename := range opts.inputs {
		inputBindings[name] = filename
	}
	preparedExecutables := map[string]bool{}
	cleanups := []func(){}
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	if len(opts.privateInputTrees) != 0 {
		cleanup, err := preparePrivateInputTreeProjections(recipe.Inputs, &opts)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, cleanup)
	}
	runtimeToolDirectory := ""
	if len(opts.runtimeTools) != 0 {
		var cleanup func()
		runtimeToolDirectory, cleanup, err = prepareRuntimeToolDirectory(opts.workingDirectory, opts.runtimeTools)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, cleanup)
	}
	for _, name := range recipe.ExecutableInputs {
		var cleanup func()
		inputBindings[name], cleanup, err = actionLocalExecutable(inputBindings[name], opts.workingDirectory)
		if err != nil {
			return fmt.Errorf("prepare executable input %s: %w", name, err)
		}
		preparedExecutables[name] = true
		cleanups = append(cleanups, cleanup)
	}

	executable := ""
	if generated := strings.TrimPrefix(recipe.Tool, "input:"); generated != recipe.Tool {
		if opts.toolRole != "generated" {
			return fmt.Errorf("generated recipe tool requires node tool role generated, got %q", opts.toolRole)
		}
		if preparedExecutables[generated] {
			executable = inputBindings[generated]
		} else {
			var cleanup func()
			executable, cleanup, err = actionLocalExecutable(inputBindings[generated], opts.workingDirectory)
			if err != nil {
				return fmt.Errorf("prepare generated recipe tool: %w", err)
			}
			cleanups = append(cleanups, cleanup)
		}
	} else {
		if opts.toolRole != recipe.Tool {
			return fmt.Errorf("recipe tool = %q, node tool role = %q", recipe.Tool, opts.toolRole)
		}
		executable = opts.tools[recipe.Tool]
	}
	expectedTools := append([]string(nil), recipe.AuxiliaryTools...)
	if !strings.HasPrefix(recipe.Tool, "input:") {
		expectedTools = append(expectedTools, recipe.Tool)
	}
	if err := exactBindings("tool", expectedTools, opts.tools); err != nil {
		return err
	}
	if err := validateAuxiliaryActionContracts(recipe.AuxiliaryTools, opts.auxiliaryActionContracts); err != nil {
		return err
	}
	if executable == "" {
		return fmt.Errorf("recipe executable is empty")
	}

	outputBindings := make(map[string]string, len(opts.outputs))
	for name, filename := range opts.outputs {
		outputBindings[name] = filename
	}
	bindings := map[string]map[string]string{
		"source": opts.sources, "input": inputBindings, "output": outputBindings,
		"tool": opts.tools, "tree": opts.trees, "work": {},
	}
	contentBindings, err := materializeRecipeContentSubstitutions(recipe.ContentSubstitutions, bindings)
	if err != nil {
		return err
	}
	bindings["content"] = contentBindings
	expand := func(value string) (string, error) { return expandValue(value, bindings) }
	workingRoot := ""
	executionDirectory := ""
	workingOutputPaths := map[string]string{}
	observedOutputPaths := map[string]string{}
	observedBefore := map[string]observedRegularFileSnapshot{}
	observedBases := map[string]toolaction.ObservedOutputState{}
	if recipe.WorkingDirectory != "" {
		if opts.workingDirectory == "" {
			return fmt.Errorf("recipe requires a working-directory root")
		}
		relative, err := expand(recipe.WorkingDirectory)
		if err != nil {
			return fmt.Errorf("working directory: %w", err)
		}
		if err := validateRelativePath(relative); err != nil {
			return fmt.Errorf("working directory: %w", err)
		}
		workingRoot = filepath.Join(opts.workingDirectory, filepath.FromSlash(relative))
		bindings["work"]["root"] = workingRoot
		if err := os.MkdirAll(workingRoot, 0o755); err != nil {
			return fmt.Errorf("create working directory: %w", err)
		}
		for ordinal, relative := range recipe.WorkingDirectories {
			if err := validateRelativePath(relative); err != nil {
				return fmt.Errorf("working directory ordinal %d: %w", ordinal, err)
			}
			directory := filepath.Join(workingRoot, filepath.FromSlash(relative))
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return fmt.Errorf("create declared working directory %q: %w", relative, err)
			}
		}
		executionDirectory = workingRoot
		if recipe.ExecutionDirectory != "" {
			executionDirectory = filepath.Join(workingRoot, filepath.FromSlash(recipe.ExecutionDirectory))
			if err := os.MkdirAll(executionDirectory, 0o755); err != nil {
				return fmt.Errorf("create execution directory: %w", err)
			}
		}
		workingTrees := append([]string(nil), recipe.WorkingTrees...)
		sort.Strings(workingTrees)
		for _, binding := range workingTrees {
			if err := copyRecipeTree(opts.trees[binding], workingRoot); err != nil {
				return fmt.Errorf("stage working tree %s: %w", binding, err)
			}
		}
		for _, binding := range sortedKeys(recipe.WorkingInputs) {
			kind, name, _ := strings.Cut(binding, ":")
			source := bindings[kind][name]
			destination := filepath.Join(workingRoot, filepath.FromSlash(recipe.WorkingInputs[binding]))
			if err := copyRecipeFile(source, destination); err != nil {
				return fmt.Errorf("stage working input %s: %w", binding, err)
			}
			// Generated inputs belong to the writable object-tree view. Immutable
			// source placeholders must continue to name the source tree: compilers
			// search that location first for quoted checked-in headers, while
			// Kbuild's evaluated out-of-tree include flags search this staged view
			// for generated headers.
			if kind == "input" {
				bindings[kind][name] = destination
			}
		}
		for _, binding := range sortedKeys(recipe.WorkingOutputs) {
			destination := filepath.Join(workingRoot, filepath.FromSlash(recipe.WorkingOutputs[binding]))
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return fmt.Errorf("create working output directory %s: %w", binding, err)
			}
			// A working output is the logical pathname observed by the tool. Its
			// declared Bazel output may instead be an internal versioned path. Keep
			// the physical destination in opts.outputs for collection, while every
			// output placeholder (and stdout below) resolves inside the private cwd.
			workingOutputPaths[binding] = destination
			bindings["output"][binding] = destination
		}
		for _, binding := range sortedKeys(recipe.ObservedOutputs) {
			destination := filepath.Join(workingRoot, filepath.FromSlash(recipe.ObservedOutputs[binding]))
			observedOutputPaths[binding] = destination
			base, err := mergeObservedOutputBase(recipe.ObservedOutputBases[binding], inputBindings)
			if err != nil {
				return fmt.Errorf("merge observed output base %s: %w", binding, err)
			}
			if err := materializeObservedOutputState(base, destination); err != nil {
				return fmt.Errorf("materialize observed output base %s: %w", binding, err)
			}
			observedBases[binding] = base
		}
		// Snapshot only after every working input and merged predecessor state has
		// been materialized. The comparison therefore records only this action's
		// effect on the inherited absolute state.
		for _, binding := range sortedKeys(recipe.ObservedOutputs) {
			snapshot, err := snapshotObservedWorkingOutput(workingRoot, observedOutputPaths[binding])
			if err != nil {
				return fmt.Errorf("snapshot observed output %s before execution: %w", binding, err)
			}
			observedBefore[binding] = snapshot
		}
	}
	linuxArgs := make([]string, len(recipe.Arguments))
	argumentTransforms := make(map[int]string, len(recipe.ArgumentTransforms))
	for _, transform := range recipe.ArgumentTransforms {
		argumentTransforms[transform.Index] = transform.Transform
	}
	for i, value := range recipe.Arguments {
		if transform := argumentTransforms[i]; transform != "" {
			switch transform {
			case kconfig.ActionRecipeArgumentTransformContentTemplateBase64:
				value, err = expandContentTemplate(value, bindings)
				if err == nil {
					linuxArgs[i] = base64.StdEncoding.EncodeToString([]byte(value))
				}
			default:
				err = fmt.Errorf("unsupported transform %q", transform)
			}
		} else {
			linuxArgs[i], err = expand(value)
		}
		if err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
	}
	replayArgs, err := encodeRecipeCommandReplays(recipe.CommandReplays, expand)
	if err != nil {
		return err
	}
	if len(replayArgs) != 0 {
		linuxArgs = append(replayArgs, linuxArgs...)
	}
	if recipe.ArgumentsFile {
		argumentsFile, cleanup, err := writeRecipeArgumentsFile(opts.workingDirectory, linuxArgs)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, cleanup)
		linuxArgs = []string{"-arguments_file", argumentsFile}
	}
	args := linuxArgs
	if len(opts.actionArgs) != 0 {
		args, err = spliceActionArgs(opts.actionArgs, linuxArgs)
		if err != nil {
			return err
		}
	}
	for _, output := range opts.outputs {
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}

	command := exec.Command(executable, args...)
	if opts.actionEnvironment == nil {
		command.Env = os.Environ()
	} else {
		command.Env = environmentList(opts.actionEnvironment)
	}
	var stdin *os.File
	if recipe.Stdin != "" {
		kind, name, _ := strings.Cut(recipe.Stdin, ":")
		stdin, err = os.Open(bindings[kind][name])
		if err != nil {
			return fmt.Errorf("open stdin %s: %w", recipe.Stdin, err)
		}
		defer stdin.Close()
		command.Stdin = stdin
	}
	for _, name := range []string{toolaction.EnvironmentName, toolaction.RuntimeToolPathEnvironmentName} {
		if _, exists := opts.actionEnvironment[name]; exists {
			return fmt.Errorf("configured action environment uses reserved variable %s", name)
		}
		if _, exists := recipe.Environment[name]; exists {
			return fmt.Errorf("recipe environment uses reserved variable %s", name)
		}
	}
	if len(recipe.Environment) != 0 || len(opts.auxiliaryActionContracts) != 0 || runtimeToolDirectory != "" {
		environment := environmentMap(command.Env)
		for _, key := range sortedKeys(recipe.Environment) {
			value, err := expand(recipe.Environment[key])
			if err != nil {
				return fmt.Errorf("environment %s: %w", key, err)
			}
			environment[key] = value
		}
		if len(opts.auxiliaryActionContracts) != 0 {
			encoded, err := toolaction.Encode(opts.auxiliaryActionContracts)
			if err != nil {
				return fmt.Errorf("auxiliary action contracts: %w", err)
			}
			environment[toolaction.EnvironmentName] = encoded
		}
		if runtimeToolDirectory != "" {
			if configuredPath := environment["PATH"]; configuredPath != "" {
				environment["PATH"] = runtimeToolDirectory + string(os.PathListSeparator) + configuredPath
			} else {
				environment["PATH"] = runtimeToolDirectory
			}
			environment[toolaction.RuntimeToolPathEnvironmentName] = runtimeToolDirectory
		}
		command.Env = environmentList(environment)
	}
	if executionDirectory != "" {
		command.Dir = executionDirectory
	}
	var stdout *os.File
	if recipe.Stdout != "" {
		stdout, err = os.OpenFile(bindings["output"][recipe.Stdout], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create stdout output: %w", err)
		}
		command.Stdout = stdout
	} else {
		command.Stdout = os.Stdout
	}
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if stdout != nil {
			_ = stdout.Close()
		}
		return fmt.Errorf("execute %s recipe for node %s: %w", recipe.Kind, opts.expectedNodeID, err)
	}
	if stdout != nil {
		if err := stdout.Close(); err != nil {
			return fmt.Errorf("close stdout output: %w", err)
		}
	}
	for _, binding := range sortedKeys(recipe.ObservedOutputs) {
		after, err := snapshotObservedWorkingOutput(workingRoot, observedOutputPaths[binding])
		if err != nil {
			return fmt.Errorf("snapshot observed output %s after execution: %w", binding, err)
		}
		state := observedOutputPostState(observedBases[binding], observedBefore[binding], after, opts.expectedNodeID)
		data, err := toolaction.EncodeObservedOutputState(state)
		if err != nil {
			return fmt.Errorf("encode observed output state %s: %w", binding, err)
		}
		if err := os.WriteFile(opts.outputs[binding], data, 0o644); err != nil {
			return fmt.Errorf("write observed output state %s: %w", binding, err)
		}
		if err := os.Chmod(opts.outputs[binding], 0o644); err != nil {
			return fmt.Errorf("set observed output state mode %s: %w", binding, err)
		}
	}
	for _, binding := range sortedKeys(recipe.WorkingOutputs) {
		source := workingOutputPaths[binding]
		if err := copyRecipeWorkingOutput(workingRoot, source, opts.outputs[binding]); err != nil {
			return fmt.Errorf("collect working output %s: %w", binding, err)
		}
	}
	for slot, output := range opts.outputs {
		info, err := os.Stat(output)
		if err != nil {
			return fmt.Errorf("recipe did not create output %s (%s): %w", slot, output, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("recipe output %s (%s) is not a regular file", slot, output)
		}
	}
	if err := finalizeWorkingDirectory(opts.workingDirectory, opts.workingDirectoryMarker); err != nil {
		return err
	}
	return nil
}

type observedRegularFileSnapshot struct {
	present        bool
	content        []byte
	executableMode uint32
}

func mergeObservedOutputBase(
	baseBindings []string,
	inputBindings map[string]string,
) (toolaction.ObservedOutputState, error) {
	states := make([]toolaction.ObservedOutputState, len(baseBindings))
	for ordinal, binding := range baseBindings {
		filename := inputBindings[binding]
		data, err := os.ReadFile(filename)
		if err != nil {
			return toolaction.ObservedOutputState{}, fmt.Errorf("read state ordinal %d input %q (%s): %w", ordinal, binding, filename, err)
		}
		state, err := toolaction.DecodeObservedOutputState(data)
		if err != nil {
			return toolaction.ObservedOutputState{}, fmt.Errorf("decode state ordinal %d input %q (%s): %w", ordinal, binding, filename, err)
		}
		states[ordinal] = state
	}
	merged, err := toolaction.MergeObservedOutputStates(states)
	if err != nil {
		return toolaction.ObservedOutputState{}, err
	}
	return merged, nil
}

func materializeObservedOutputState(state toolaction.ObservedOutputState, destination string) error {
	switch state.Disposition {
	case toolaction.ObservedOutputAbsent:
		// Absent means that no predecessor candidate has written this path. It
		// does not mean that the selected recipe's ordinary staged input is
		// absent: an observed path may intentionally overlap a WorkingInput.
		// Leave that baseline intact so the action can observe or mutate it.
		return nil
	case toolaction.ObservedOutputDeleted:
		return ensureObservedOutputAbsent(destination)
	case toolaction.ObservedOutputPresent:
		return stageObservedOutputPresent(destination, state)
	default:
		return fmt.Errorf("unsupported observed output disposition %q", state.Disposition)
	}
}

func ensureObservedOutputAbsent(filename string) error {
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filename)
	}
	return os.Remove(filename)
}

func stageObservedOutputPresent(filename string, state toolaction.ObservedOutputState) error {
	if state.Disposition != toolaction.ObservedOutputPresent {
		return fmt.Errorf("cannot stage observed output disposition %q", state.Disposition)
	}
	if state.ExecutableMode&^uint32(0o111) != 0 {
		return fmt.Errorf("observed output executable mode %#o contains non-executable bits", state.ExecutableMode)
	}
	if err := ensureObservedOutputAbsent(filename); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644) | os.FileMode(state.ExecutableMode)
	if err := os.WriteFile(filename, state.Content, mode); err != nil {
		return err
	}
	return os.Chmod(filename, mode)
}

func snapshotObservedRegularFile(filename string) (observedRegularFileSnapshot, error) {
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return observedRegularFileSnapshot{}, nil
	}
	if err != nil {
		return observedRegularFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return observedRegularFileSnapshot{}, fmt.Errorf("%s is not a regular file", filename)
	}
	// The state owns a private post-action work tree. A generator may leave a
	// regular output executable but unreadable (for example mode 0101); retain
	// that exact mode in the envelope while temporarily granting the runner
	// owner-read access so its bytes can still be recorded deterministically.
	originalMode := info.Mode().Perm()
	if originalMode&0o400 == 0 {
		if err := os.Chmod(filename, originalMode|0o400); err != nil {
			return observedRegularFileSnapshot{}, fmt.Errorf("temporarily make %s readable: %w", filename, err)
		}
		defer func() { _ = os.Chmod(filename, originalMode) }()
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		return observedRegularFileSnapshot{}, err
	}
	return observedRegularFileSnapshot{
		present:        true,
		content:        content,
		executableMode: uint32(originalMode & 0o111),
	}, nil
}

func snapshotObservedWorkingOutput(root, filename string) (observedRegularFileSnapshot, error) {
	if err := validateRecipeOutputAncestors(root, filename); err != nil {
		return observedRegularFileSnapshot{}, err
	}
	return snapshotObservedRegularFile(filename)
}

func observedOutputPostState(
	base toolaction.ObservedOutputState,
	before, after observedRegularFileSnapshot,
	writer string,
) toolaction.ObservedOutputState {
	switch {
	case !before.present && !after.present:
		return base
	case before.present && !after.present:
		return toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputDeleted, Writer: writer}
	case before.present && bytes.Equal(before.content, after.content) && before.executableMode == after.executableMode:
		return base
	default:
		return toolaction.ObservedOutputState{
			Disposition:    toolaction.ObservedOutputPresent,
			Writer:         writer,
			Content:        after.content,
			ExecutableMode: after.executableMode,
		}
	}
}

func encodeRecipeCommandReplays(
	replays []kconfig.ActionRecipeCommandReplay,
	expand func(string) (string, error),
) ([]string, error) {
	arguments := make([]string, 0, len(replays)*2)
	for replayIndex, replay := range replays {
		expanded := kconfig.ActionRecipeCommandReplay{
			Name:        replay.Name,
			Invocations: make([]kconfig.ActionRecipeCommandReplayInvocation, len(replay.Invocations)),
		}
		for invocationIndex, invocation := range replay.Invocations {
			expandedInvocation := &expanded.Invocations[invocationIndex]
			for argumentIndex, argument := range invocation.Arguments {
				value, err := expand(argument)
				if err != nil {
					return nil, fmt.Errorf("command replay %d invocation %d argument %d: %w", replayIndex, invocationIndex, argumentIndex, err)
				}
				expandedInvocation.Arguments = append(expandedInvocation.Arguments, value)
			}
			for outputIndex, output := range invocation.Outputs {
				value, err := expand(output)
				if err != nil {
					return nil, fmt.Errorf("command replay %d invocation %d output %d: %w", replayIndex, invocationIndex, outputIndex, err)
				}
				expandedInvocation.Outputs = append(expandedInvocation.Outputs, value)
			}
		}
		data, err := json.Marshal(expanded)
		if err != nil {
			return nil, fmt.Errorf("encode command replay %d: %w", replayIndex, err)
		}
		arguments = append(arguments, "-replay_base64", base64.StdEncoding.EncodeToString(data))
	}
	return arguments, nil
}

const maxRecipeContentSubstitutionBytes = 1 << 20

// materializeRecipeContentSubstitutions performs bounded, deterministic
// byte-to-text transforms over declared graph inputs. It deliberately cannot
// execute a command or discover another path: generated-file queries are
// represented by their own producer action, and only that immutable output is
// visible here.
func materializeRecipeContentSubstitutions(
	substitutions map[string]kconfig.ActionRecipeContentSubstitution,
	bindings map[string]map[string]string,
) (map[string]string, error) {
	values := make(map[string]string, len(substitutions))
	names := make([]string, 0, len(substitutions))
	for name := range substitutions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		substitution := substitutions[name]
		kind, binding, ok := strings.Cut(substitution.Input, ":")
		if !ok || (kind != "source" && kind != "input") {
			return nil, fmt.Errorf("content substitution %s has invalid input %q", name, substitution.Input)
		}
		filename := bindings[kind][binding]
		if filename == "" {
			return nil, fmt.Errorf("content substitution %s has unbound input %q", name, substitution.Input)
		}
		value, err := readBoundedRecipeContent(filename)
		if err != nil {
			return nil, fmt.Errorf("content substitution %s: %w", name, err)
		}
		switch substitution.Transform {
		case kconfig.ActionRecipeContentTransformMakeShellWord:
			value, err = kconfig.NormalizeActionRecipeMakeShellValue(value)
			if err != nil {
				return nil, fmt.Errorf("content substitution %s: %w", name, err)
			}
			// This value occupies one already-planned argv field. Accept exactly
			// the shell-safe single-word subset, for which GNU Make plus the
			// recipe shell and direct argv execution are equivalent. Multi-word
			// or shell-active output would require changing argv cardinality or
			// executing generated syntax, so fail closed.
			if !isRecipeShellSafeWord(value) {
				return nil, fmt.Errorf("content substitution %s is not one shell-safe Make word", name)
			}
		case kconfig.ActionRecipeContentTransformMakeShellSingleWord:
			value, err = kconfig.QuoteActionRecipeMakeShellSingleWord(value)
			if err != nil {
				return nil, fmt.Errorf("content substitution %s: %w", name, err)
			}
		case kconfig.ActionRecipeContentTransformMakeShellSingleQuotedSegment:
			value, err = kconfig.FormatActionRecipeMakeShellSingleQuotedSegment(value)
			if err != nil {
				return nil, fmt.Errorf("content substitution %s: %w", name, err)
			}
		case kconfig.ActionRecipeContentTransformMakeShellValue:
			value, err = kconfig.NormalizeActionRecipeMakeShellValue(value)
			if err != nil {
				return nil, fmt.Errorf("content substitution %s: %w", name, err)
			}
		default:
			return nil, fmt.Errorf("content substitution %s has unsupported transform %q", name, substitution.Transform)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("content substitution %s contains NUL", name)
		}
		values[name] = value
	}
	return values, nil
}

func isRecipeShellSafeWord(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("_@%+=:,./-", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func readBoundedRecipeContent(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open input: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect input: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("input %s is not a regular file", filename)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRecipeContentSubstitutionBytes+1))
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	if len(data) > maxRecipeContentSubstitutionBytes {
		return "", fmt.Errorf("input exceeds %d bytes", maxRecipeContentSubstitutionBytes)
	}
	return string(data), nil
}

// copyRecipeTree materializes a directory binding as regular directories and
// files below a private working root. os.ReadDir returns entries in lexical
// order, and recursion preserves that order at every level. A symlink to a
// regular immutable input is copied by value only when its resolved target
// remains below the declared tree; directory symlinks and escaping file
// symlinks are rejected. Existing file leaves are rejected here, while the
// later WorkingInputs pass deliberately replaces an exact leaf from this
// collision-free merged baseline.
func copyRecipeTree(sourceRoot, destinationRoot string) error {
	resolvedRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolve tree root %s: %w", sourceRoot, err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", sourceRoot)
	}
	return copyRecipeTreeDirectory(resolvedRoot, resolvedRoot, destinationRoot, "")
}

func copyRecipeTreeDirectory(sourceRoot, sourceDirectory, destinationRoot, relativeDirectory string) error {
	entries, err := os.ReadDir(sourceDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		relative := entry.Name()
		if relativeDirectory != "" {
			relative = relativeDirectory + "/" + relative
		}
		if err := validateRelativePath(relative); err != nil {
			return fmt.Errorf("tree entry %q: %w", relative, err)
		}
		source := filepath.Join(sourceDirectory, entry.Name())
		destination := filepath.Join(destinationRoot, filepath.FromSlash(relative))
		entryInfo, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("inspect tree entry %q: %w", relative, err)
		}
		symlink := entryInfo.Mode()&os.ModeSymlink != 0
		copySource := source
		if symlink {
			copySource, err = filepath.EvalSymlinks(source)
			if err != nil {
				return fmt.Errorf("resolve tree symlink %q: %w", relative, err)
			}
			contained, err := recipeTreeContains(sourceRoot, copySource)
			if err != nil {
				return fmt.Errorf("resolve tree symlink %q containment: %w", relative, err)
			}
			if !contained {
				return fmt.Errorf("tree entry %q is a symlink which resolves outside the declared tree", relative)
			}
			entryInfo, err = os.Stat(copySource)
			if err != nil {
				return fmt.Errorf("resolve tree symlink %q: %w", relative, err)
			}
		}
		switch {
		case entryInfo.IsDir():
			if symlink {
				return fmt.Errorf("tree entry %q is a symlink to a directory", relative)
			}
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("create tree directory %q: %w", relative, err)
			}
			if err := copyRecipeTreeDirectory(sourceRoot, source, destinationRoot, relative); err != nil {
				return err
			}
		case entryInfo.Mode().IsRegular():
			if _, err := os.Lstat(destination); err == nil {
				return fmt.Errorf("tree file %q collides with an existing working path", relative)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect tree destination %q: %w", relative, err)
			}
			if err := copyRecipeFile(copySource, destination); err != nil {
				return fmt.Errorf("copy tree file %q: %w", relative, err)
			}
		default:
			return fmt.Errorf("tree entry %q is neither a directory nor a regular file", relative)
		}
	}
	return nil
}

func recipeTreeContains(root, filename string) (bool, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(filename))
	if err != nil {
		return false, err
	}
	return relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative), nil
}

func copyRecipeFile(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info.Mode().Perm()&0o111 != 0 {
		mode |= 0o111
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := output.Chmod(mode); err != nil {
		_ = output.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

// copyRecipeWorkingOutput rejects symlinks before collecting a tool-created
// path. Immutable Bazel inputs may themselves be symlinks and are staged by
// copyRecipeFile, but a declared output and every ancestor beneath root must
// remain inside the private work tree rather than aliasing an undeclared path.
func copyRecipeWorkingOutput(root, source, destination string) error {
	if err := validateRecipeOutputAncestors(root, source); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file created in the private working tree", source)
	}
	return copyRecipeFile(source, destination)
}

func validateRecipeOutputAncestors(root, filename string) error {
	root = filepath.Clean(root)
	filename = filepath.Clean(filename)
	relative, err := filepath.Rel(root, filename)
	if err != nil {
		return fmt.Errorf("resolve private working-tree output %q beneath %q: %w", filename, root, err)
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("output path %q is not beneath private working tree %q", filename, root)
	}
	ancestors := []string{root}
	parent := filepath.Dir(relative)
	if parent != "." {
		current := root
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			ancestors = append(ancestors, current)
		}
	}
	for _, ancestor := range ancestors {
		info, err := os.Lstat(ancestor)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect output ancestor %q: %w", ancestor, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output path %q traverses symlink ancestor %q", filename, ancestor)
		}
		if !info.IsDir() {
			return fmt.Errorf("output path %q traverses non-directory ancestor %q", filename, ancestor)
		}
	}
	return nil
}

func writeRecipeArgumentsFile(privateRoot string, arguments []string) (string, func(), error) {
	cleanup := func() {}
	if privateRoot == "" {
		return "", cleanup, fmt.Errorf("arguments_file requires a private working-directory root")
	}
	if arguments == nil {
		arguments = []string{}
	}
	data, err := json.Marshal(arguments)
	if err != nil {
		return "", cleanup, fmt.Errorf("encode recipe arguments file: %w", err)
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(privateRoot, ".linux-bzl-arguments-*.json")
	if err != nil {
		return "", cleanup, fmt.Errorf("create recipe arguments file: %w", err)
	}
	filename := file.Name()
	cleanup = func() { _ = os.Remove(filename) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("set recipe arguments file mode: %w", err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write recipe arguments file: %w", err)
	}
	return filename, cleanup, nil
}

func expandExecutionRootActionValue(value, executionRoot string) (string, error) {
	return toolaction.ExpandExecutionRootValue(value, executionRoot)
}

func expandExecutionRootActionContracts(opts *recipeOptions, executionRoot string) error {
	arguments := make([]string, len(opts.actionArgs))
	for index, value := range opts.actionArgs {
		expanded, err := expandExecutionRootActionValue(value, executionRoot)
		if err != nil {
			return fmt.Errorf("configured action argument %d: %w", index, err)
		}
		arguments[index] = expanded
	}
	opts.actionArgs = arguments

	if opts.actionEnvironment != nil {
		environment := make(map[string]string, len(opts.actionEnvironment))
		for name, value := range opts.actionEnvironment {
			expanded, err := expandExecutionRootActionValue(value, executionRoot)
			if err != nil {
				return fmt.Errorf("configured action environment %s: %w", name, err)
			}
			environment[name] = expanded
		}
		opts.actionEnvironment = environment
	}

	contracts := make(map[string]toolaction.Contract, len(opts.auxiliaryActionContracts))
	for role, contract := range opts.auxiliaryActionContracts {
		arguments := make([]string, len(contract.Arguments))
		for index, value := range contract.Arguments {
			expanded, err := expandExecutionRootActionValue(value, executionRoot)
			if err != nil {
				return fmt.Errorf("auxiliary %s action argument %d: %w", role, index, err)
			}
			arguments[index] = expanded
		}
		environment := make(map[string]string, len(contract.Environment))
		for name, value := range contract.Environment {
			expanded, err := expandExecutionRootActionValue(value, executionRoot)
			if err != nil {
				return fmt.Errorf("auxiliary %s action environment %s: %w", role, name, err)
			}
			environment[name] = expanded
		}
		contracts[role] = toolaction.Contract{Arguments: arguments, Environment: environment}
	}
	opts.auxiliaryActionContracts = contracts
	return nil
}

// Bazel artifact paths are execroot-relative. Once a recipe changes cwd, all
// artifact and tool bindings must remain anchored to that execroot rather than
// being reinterpreted below the private working directory.
func absolutizeWorkingRecipeOptions(opts *recipeOptions) error {
	if opts.workingDirectory == "" {
		return fmt.Errorf("working-directory root is required")
	}
	paths := map[string]*string{
		"working-directory root": &opts.workingDirectory,
	}
	if opts.workingDirectoryMarker != "" {
		paths["working-directory marker"] = &opts.workingDirectoryMarker
	}
	for name, value := range paths {
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		*value = absolute
	}
	for kind, values := range map[string]map[string]string{
		"source": opts.sources, "input": opts.inputs, "output": opts.outputs,
		"tool": opts.tools, "runtime tool": opts.runtimeTools, "tree": opts.trees,
	} {
		for name, value := range values {
			absolute, err := filepath.Abs(value)
			if err != nil {
				return fmt.Errorf("resolve %s binding %s: %w", kind, name, err)
			}
			values[name] = absolute
		}
	}
	return nil
}

// prepareRuntimeToolDirectory exposes the selected toolchain's transitive
// executable roles to tools that spawn subprocesses by name. These bindings
// are deliberately separate from recipe auxiliary tools: a compiler driver
// chooses how to invoke its linker or assembler and must not receive the
// auxiliary tool's Kbuild argv contract.
func prepareRuntimeToolDirectory(privateRoot string, tools map[string]string) (string, func(), error) {
	return toolaction.PrepareRuntimeToolDirectory(privateRoot, tools)
}

func validateWorkingDirectory(root, marker string) error {
	if root == "" && marker == "" {
		return nil
	}
	if root == "" || marker == "" {
		return fmt.Errorf("working-directory root and marker must be supplied together")
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
		return fmt.Errorf("working-directory root %q is not private", root)
	}
	wantMarker := filepath.Join(cleanRoot, ".linux-bzl-work-root")
	if filepath.Clean(marker) != wantMarker {
		return fmt.Errorf("working-directory marker %q must be %q", marker, wantMarker)
	}
	return nil
}

func prepareWorkingDirectory(root, marker string) error {
	if err := validateWorkingDirectory(root, marker); err != nil {
		return err
	}
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clear private working-directory root: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create private working-directory root: %w", err)
	}
	return nil
}

func finalizeWorkingDirectory(root, marker string) error {
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clean private working-directory root: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("recreate private working-directory root: %w", err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("write private working-directory marker: %w", err)
	}
	return nil
}

// Bazel outputs are not intrinsically executable. Generated host-tool nodes
// therefore remain ordinary immutable File inputs, and their consumer creates
// a private executable copy inside its action sandbox. This never mutates a
// shared input and works identically on local and remote executors.
func actionLocalExecutable(source, privateRoot string) (string, func(), error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", func() {}, err
	}
	if !info.Mode().IsRegular() {
		return "", func() {}, fmt.Errorf("%s is not a regular file", source)
	}
	if privateRoot == "" {
		return "", func() {}, fmt.Errorf("generated executables require a private working-directory root")
	}
	privateRoot, err = filepath.Abs(privateRoot)
	if err != nil {
		return "", func() {}, fmt.Errorf("resolve private working-directory root: %w", err)
	}
	directory, err := os.MkdirTemp(privateRoot, ".linux-bzl-generated-tool-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	destination := filepath.Join(directory, filepath.Base(source))
	input, err := os.Open(source)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		_ = input.Close()
		cleanup()
		return "", func() {}, err
	}
	_, copyErr := io.Copy(output, input)
	closeInputErr := input.Close()
	closeOutputErr := output.Close()
	if err := errors.Join(copyErr, closeInputErr, closeOutputErr); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return destination, cleanup, nil
}

func spliceActionArgs(actionArgs, linuxArgs []string) ([]string, error) {
	found := false
	out := []string{}
	for _, argument := range actionArgs {
		if argument == kconfig.LinuxKbuildArgsSentinel {
			if found {
				return nil, fmt.Errorf("configured tool action repeats its Kbuild-arguments marker")
			}
			found = true
			out = append(out, linuxArgs...)
		} else {
			out = append(out, argument)
		}
	}
	if !found {
		return nil, fmt.Errorf("configured tool action is missing its Kbuild-arguments marker")
	}
	return out, nil
}

func expandValue(value string, bindings map[string]map[string]string) (string, error) {
	return expandValueKinds(value, bindings, nil)
}

// expandContentTemplate substitutes generated content while retaining typed
// placeholders for the downstream consumer. Only tree markers which were
// complete placeholders in the serialized template may reach scriptrun: bytes
// read from one or several generated files must not be able to manufacture a
// new marker by themselves or together with adjacent template bytes.
func expandContentTemplate(value string, bindings map[string]map[string]string) (string, error) {
	var out strings.Builder
	allowedTrees := map[int]string{}
	for cursor := 0; ; {
		relativeStart := strings.Index(value[cursor:], "${")
		if relativeStart < 0 {
			out.WriteString(value[cursor:])
			break
		}
		start := cursor + relativeStart
		out.WriteString(value[cursor:start])
		relativeEnd := strings.IndexByte(value[start+2:], '}')
		if relativeEnd < 0 {
			return "", fmt.Errorf("unterminated placeholder in %q", value)
		}
		end := start + 2 + relativeEnd
		body := value[start+2 : end]
		kind, name, ok := strings.Cut(body, ":")
		if !ok || bindings[kind] == nil {
			return "", fmt.Errorf("unsupported placeholder %q", body)
		}
		replacement, ok := bindings[kind][name]
		if !ok {
			return "", fmt.Errorf("unbound placeholder %q", body)
		}
		if kind == "content" {
			out.WriteString(replacement)
		} else {
			placeholder := value[start : end+1]
			if kind == "tree" {
				allowedTrees[out.Len()] = placeholder
			}
			out.WriteString(placeholder)
		}
		cursor = end + 1
	}

	expanded := out.String()
	for cursor := 0; ; {
		relative := strings.Index(expanded[cursor:], "${tree:")
		if relative < 0 {
			break
		}
		start := cursor + relative
		placeholder, allowed := allowedTrees[start]
		if !allowed || !strings.HasPrefix(expanded[start:], placeholder) {
			return "", fmt.Errorf("generated content created reserved ${tree: prefix at byte %d", start)
		}
		cursor = start + len("${tree:")
	}
	return expanded, nil
}

// expandValueKinds expands only the selected typed placeholder kinds. Every
// other declared placeholder is preserved byte-for-byte for a downstream
// typed consumer. A nil kind set selects every kind.
func expandValueKinds(value string, bindings map[string]map[string]string, kinds map[string]bool) (string, error) {
	var out strings.Builder
	for cursor := 0; ; {
		relativeStart := strings.Index(value[cursor:], "${")
		if relativeStart < 0 {
			out.WriteString(value[cursor:])
			return out.String(), nil
		}
		start := cursor + relativeStart
		out.WriteString(value[cursor:start])
		relativeEnd := strings.IndexByte(value[start+2:], '}')
		if relativeEnd < 0 {
			return "", fmt.Errorf("unterminated placeholder in %q", value)
		}
		end := start + 2 + relativeEnd
		body := value[start+2 : end]
		kind, name, ok := strings.Cut(body, ":")
		if !ok || bindings[kind] == nil {
			return "", fmt.Errorf("unsupported placeholder %q", body)
		}
		replacement, ok := bindings[kind][name]
		if !ok {
			return "", fmt.Errorf("unbound placeholder %q", body)
		}
		if kinds != nil && !kinds[kind] {
			out.WriteString(value[start : end+1])
			cursor = end + 1
			continue
		}
		// Replacements are data, never recipe syntax. In particular, bytes read
		// from a generated content input cannot inject another path placeholder.
		out.WriteString(replacement)
		cursor = end + 1
	}
}

func exactBindings(kind string, want []string, got map[string]string) error {
	wanted := make(map[string]bool, len(want))
	for _, key := range want {
		wanted[key] = true
	}
	for key, value := range got {
		if !wanted[key] {
			return fmt.Errorf("unexpected %s binding %q", kind, key)
		}
		if value == "" {
			return fmt.Errorf("%s binding %q is empty", kind, key)
		}
	}
	for _, key := range want {
		if got[key] == "" {
			return fmt.Errorf("missing %s binding %q", kind, key)
		}
	}
	return nil
}

func namedBindings(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		name, filename, ok := strings.Cut(value, "=")
		if !ok || name == "" || filename == "" {
			return nil, fmt.Errorf("expected NAME=PATH, got %q", value)
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("repeated binding %q", name)
		}
		out[name] = filename
	}
	return out, nil
}

func environmentBindings(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		name, data, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsRune(name, 0) || strings.ContainsRune(data, 0) {
			return nil, fmt.Errorf("expected NAME=VALUE, got %q", value)
		}
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("repeated environment variable %q", name)
		}
		out[name] = data
	}
	return out, nil
}

func parseAuxiliaryActionContracts(
	roles, arguments, environment []string,
) (map[string]toolaction.Contract, error) {
	contracts := make(map[string]toolaction.Contract, len(roles))
	for _, role := range roles {
		if _, exists := contracts[role]; exists {
			return nil, fmt.Errorf("repeated auxiliary action role %q", role)
		}
		contracts[role] = toolaction.Contract{Arguments: []string{}, Environment: map[string]string{}}
	}
	for _, value := range arguments {
		role, argument, ok := strings.Cut(value, "=")
		contract, exists := contracts[role]
		if !ok || !exists {
			return nil, fmt.Errorf("auxiliary action argument %q has no declared role", value)
		}
		contract.Arguments = append(contract.Arguments, argument)
		contracts[role] = contract
	}
	for _, value := range environment {
		role, assignment, ok := strings.Cut(value, "=")
		contract, exists := contracts[role]
		if !ok || !exists {
			return nil, fmt.Errorf("auxiliary action environment %q has no declared role", value)
		}
		name, data, ok := strings.Cut(assignment, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("auxiliary action environment %q is not ROLE=NAME=VALUE", value)
		}
		if _, exists := contract.Environment[name]; exists {
			return nil, fmt.Errorf("auxiliary action role %q repeats environment variable %q", role, name)
		}
		contract.Environment[name] = data
		contracts[role] = contract
	}
	if err := toolaction.Validate(contracts); err != nil {
		return nil, err
	}
	return contracts, nil
}

func validateAuxiliaryActionContracts(auxiliaryRoles []string, contracts map[string]toolaction.Contract) error {
	allowed := make(map[string]bool, len(auxiliaryRoles))
	for _, role := range auxiliaryRoles {
		allowed[role] = true
	}
	for _, role := range toolaction.Roles(contracts) {
		if allowed[role] {
			continue
		}
		base, companion := toolaction.BaseContractRole(role)
		if !companion || !allowed[base] {
			return fmt.Errorf("action contract for non-auxiliary tool role %q", role)
		}
		if _, exists := contracts[base]; !exists {
			return fmt.Errorf("companion action contract %q has no base %q contract", role, base)
		}
	}
	if err := toolaction.Validate(contracts); err != nil {
		return fmt.Errorf("auxiliary action contracts: %w", err)
	}
	return nil
}

func copyTreeFile(tree, relative, manifest, output string) error {
	if tree == "" || relative == "" || output == "" {
		return fmt.Errorf("-tree, -path, and -output are required for -copy_tree_file")
	}
	if err := validateRelativePath(relative); err != nil {
		return err
	}
	if manifest != "" {
		allowed, err := readManifest(manifest)
		if err != nil {
			return err
		}
		if !allowed[relative] {
			return fmt.Errorf("path %q is not present in manifest", relative)
		}
	}
	root, err := filepath.Abs(tree)
	if err != nil {
		return err
	}
	current := root
	components := strings.Split(relative, "/")
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect projected path %q: %w", relative, err)
		}
		// Bazel may materialize the final declared TreeFile as a symlink inside
		// an action sandbox. Intermediate symlinks could redirect path traversal
		// and remain forbidden; the final target is still required to be regular.
		if info.Mode()&os.ModeSymlink != 0 && index != len(components)-1 {
			return fmt.Errorf("projected path %q traverses symlink %q", relative, current)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("projected path %q is not a regular file", relative)
	}
	return atomicCopy(current, output, info.Mode().Perm())
}

func readManifest(filename string) (map[string]bool, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	out := map[string]bool{}
	previous := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		entry := scanner.Text()
		if err := validateRelativePath(entry); err != nil {
			return nil, fmt.Errorf("manifest entry: %w", err)
		}
		if previous != "" && entry <= previous {
			return nil, fmt.Errorf("manifest is not strictly sorted at %q", entry)
		}
		out[entry], previous = true, entry
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func atomicCopy(source, output string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), "."+filepath.Base(output)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	in, err := os.Open(source)
	if err != nil {
		tmp.Close()
		return err
	}
	_, copyErr := io.Copy(tmp, in)
	closeInErr := in.Close()
	chmodErr := tmp.Chmod(mode)
	closeErr := tmp.Close()
	if err := errors.Join(copyErr, closeInErr, chmodErr, closeErr); err != nil {
		return err
	}
	return os.Rename(name, output)
}

func validateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, `\`) || filepath.ToSlash(filepath.Clean(value)) != value {
		return fmt.Errorf("path %q is not a canonical relative path", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("path %q is not a canonical relative path", value)
		}
	}
	return nil
}

func environmentMap(values []string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		if key, val, ok := strings.Cut(value, "="); ok {
			out[key] = val
		}
	}
	return out
}
func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = key + "=" + values[key]
	}
	return out
}
func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func isDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func main() {
	var sourceFlags, inputFlags, outputFlags, toolFlags, runtimeToolFlags, treeFlags, artifactTreeFlags, privateInputTreeFlags, actionArgs, actionEnvironment repeatedFlag
	var auxiliaryActionRoles, auxiliaryActionArguments, auxiliaryActionEnvironment repeatedFlag
	recipe := flag.String("recipe", "", "v4 action recipe JSON")
	kind := flag.String("kind", "", "node kind encoded by the plan")
	expectedNodeID := flag.String("expected_node_id", "", "content-addressed node ID")
	expectedRecipeID := flag.String("expected_recipe_id", "", "content-addressed recipe ID")
	inputBindings := flag.String("input_bindings", "", "canonical per-node input bindings manifest")
	expectedInputBindingsID := flag.String("expected_input_bindings_id", "", "content ID of the input bindings manifest")
	toolRole := flag.String("tool_role", "", "tool role encoded by the node")
	workingDirectoryMarker := flag.String("working_directory_marker", "", "declared output proving the private working root was cleaned")
	copyMode := flag.Bool("copy_tree_file", false, "project one TreeArtifact child to a fixed output")
	copyTree := flag.String("tree", "", "TreeArtifact root for projection mode")
	copyPath := flag.String("path", "", "canonical TreeArtifact-relative path for projection mode")
	manifest := flag.String("manifest", "", "optional sorted projection manifest")
	copyOutput := flag.String("output", "", "fixed output for projection mode")
	flag.Var(&sourceFlags, "source", "recipe source binding NAME=PATH (repeatable)")
	flag.Var(&inputFlags, "input", "recipe node-input binding NAME=PATH (repeatable)")
	flag.Var(&outputFlags, "recipe_output", "recipe output binding SLOT=PATH (repeatable)")
	flag.Var(&toolFlags, "tool", "recipe tool binding NAME=PATH (repeatable)")
	flag.Var(&runtimeToolFlags, "runtime_tool", "configured runtime tool binding ROLE=PATH (repeatable)")
	flag.Var(&treeFlags, "input_tree", "recipe input-tree binding NAME=ROOT (repeatable)")
	flag.Var(&artifactTreeFlags, "artifact_tree", "input-binding artifact tree TREE=ROOT (repeatable)")
	flag.Var(&privateInputTreeFlags, "private_input_tree", "current-stage tree projected from exact inputs (repeatable)")
	flag.Var(&actionArgs, "action_arg", "configured tool action argument (repeatable)")
	flag.Var(&actionEnvironment, "action_env", "configured tool action environment NAME=VALUE (repeatable)")
	flag.Var(&auxiliaryActionRoles, "auxiliary_action_role", "auxiliary configured action role (repeatable)")
	flag.Var(&auxiliaryActionArguments, "auxiliary_action_arg", "auxiliary configured action argument ROLE=VALUE (repeatable)")
	flag.Var(&auxiliaryActionEnvironment, "auxiliary_action_env", "auxiliary configured action environment ROLE=NAME=VALUE (repeatable)")
	flag.Parse()
	workingDirectory := ""
	if *workingDirectoryMarker != "" {
		// The callback supplies the declared marker as a typed Artifact so Bazel
		// can path-map it; deriving its parent here avoids freezing File.dirname
		// into an unmapped command-line string.
		workingDirectory = filepath.Dir(*workingDirectoryMarker)
	}
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mapdirectoryrecipe: positional arguments are not supported")
		os.Exit(2)
	}
	if *copyMode {
		if err := copyTreeFile(*copyTree, *copyPath, *manifest, *copyOutput); err != nil {
			fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
			os.Exit(1)
		}
		return
	}
	bindings := []struct {
		name string
		raw  []string
		out  *map[string]string
	}{
		{"source", sourceFlags, new(map[string]string)}, {"input", inputFlags, new(map[string]string)},
		{"output", outputFlags, new(map[string]string)}, {"tool", toolFlags, new(map[string]string)},
		{"tree", treeFlags, new(map[string]string)},
	}
	for i := range bindings {
		parsed, err := namedBindings(bindings[i].raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %s bindings: %v\n", bindings[i].name, err)
			os.Exit(2)
		}
		*bindings[i].out = parsed
	}
	actionEnv, err := environmentBindings(actionEnvironment)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: action environment: %v\n", err)
		os.Exit(2)
	}
	runtimeTools, err := namedBindings(runtimeToolFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: runtime tool bindings: %v\n", err)
		os.Exit(2)
	}
	artifactTrees, err := namedBindings(artifactTreeFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: artifact tree bindings: %v\n", err)
		os.Exit(2)
	}
	privateInputTrees := map[string]bool{}
	for _, name := range privateInputTreeFlags {
		if name == "" || strings.ContainsAny(name, "=/\\\x00\r\n\t ") {
			fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: invalid private input tree %q\n", name)
			os.Exit(2)
		}
		if privateInputTrees[name] {
			fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: repeated private input tree %q\n", name)
			os.Exit(2)
		}
		privateInputTrees[name] = true
	}
	auxiliaryContracts, err := parseAuxiliaryActionContracts(auxiliaryActionRoles, auxiliaryActionArguments, auxiliaryActionEnvironment)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: auxiliary action contracts: %v\n", err)
		os.Exit(2)
	}
	opts := recipeOptions{recipe: *recipe, kind: *kind, expectedNodeID: *expectedNodeID, expectedRecipeID: *expectedRecipeID, inputBindings: *inputBindings, expectedInputBindingsID: *expectedInputBindingsID, toolRole: *toolRole, workingDirectory: workingDirectory, workingDirectoryMarker: *workingDirectoryMarker, actionArgs: actionArgs,
		actionEnvironment: actionEnv, auxiliaryActionContracts: auxiliaryContracts, sources: *bindings[0].out, inputs: *bindings[1].out, outputs: *bindings[2].out, tools: *bindings[3].out, runtimeTools: runtimeTools, trees: *bindings[4].out, artifactTrees: artifactTrees, privateInputTrees: privateInputTrees}
	if err := runRecipe(opts); err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
		os.Exit(1)
	}
}
