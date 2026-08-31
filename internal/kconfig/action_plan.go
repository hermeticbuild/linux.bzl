package kconfig

// This file is the execution-time contract between the Linux planner, Bazel's
// map_directory callback, and mapdirectoryrecipe.  Keep the contract generic:
// Kconfig and Kbuild decide which nodes and arguments exist; the callback only
// validates marker paths and turns their dependency edges into Bazel actions.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const (
	LinuxKernelPlanSchema          = "linux-kernel-plan-v4"
	LinuxKernelInputBindingsSchema = "linux-kernel-input-bindings-v1"
	LinuxKbuildArgsSentinel        = "__LINUX_BZL_KBUILD_ARGS_V1__"
	LinuxKernelTreeRootMarker      = ".linux-bzl-tree-root"

	// MaxActionPlanInputBindingsBytes bounds the canonical per-node binding
	// manifest before it is decoded or consumed by an action runner.
	MaxActionPlanInputBindingsBytes = 64 << 20

	maximumActionPlanOrdinal              = 99999999
	maximumActionPlanNameComponent        = 240
	maximumActionPlanInputTuplesPerMarker = 64
	maximumActionPlanInputMarkerFilename  = 240
	maximumActionPlanInputBindingCount    = 1 << 20

	// Keep the process-local working-tree topology cache bounded independently
	// of the size of the serialized plan. Each entry is a uint32 node index, so
	// this permits at most roughly 32 MiB of cached flattened topology.
	maximumWorkingTreeTopologyCacheIndexes = 8 << 20
)

var LinuxKernelPlanStages = map[string]bool{
	"prehost": true, "bootstrap": true, "host": true, "prep": true, "target": true,
}

var linuxKernelPlanStageOrder = []string{"prehost", "bootstrap", "host", "prep", "target"}

var LinuxKernelPlanTrees = map[string]bool{
	"prehost": true, "bootstrap": true, "prep": true, "host": true, "objects": true, "sdk": true,
	"vmlinux": true, "image": true, "modules": true, "metadata": true,
}

func actionPlanStageOwnsOutputTree(stage, tree string) bool {
	switch stage {
	case "prehost":
		return tree == "prehost"
	case "bootstrap":
		return tree == "bootstrap"
	case "host":
		return tree == "host"
	case "prep":
		return tree == "prep"
	case "target":
		return tree == "objects" || tree == "sdk" || tree == "vmlinux" || tree == "image" || tree == "modules" || tree == "metadata"
	default:
		return false
	}
}

// actionPlanStageScratchTree returns the canonical stage-owned tree for hidden
// planner artifacts which are not themselves Kbuild products. The target
// stage uses metadata so those artifacts neither pollute product trees nor get
// duplicated when one query is consumed by several target products.
func actionPlanStageScratchTree(stage string) (string, bool) {
	var tree string
	switch stage {
	case "prehost":
		tree = "prehost"
	case "bootstrap":
		tree = "bootstrap"
	case "host":
		tree = "host"
	case "prep":
		tree = "prep"
	case "target":
		tree = "metadata"
	default:
		return "", false
	}
	if !actionPlanStageOwnsOutputTree(stage, tree) {
		return "", false
	}
	return tree, true
}

var LinuxKernelPlanNodeKinds = map[string]bool{
	"compile": true, "assemble": true, "preprocess": true,
	"link-driver": true, "link-relocatable": true, "archive": true, "copy": true,
	"generate": true, "metadata": true,
}

// "shell" is intentionally not a tool role. Recipes are argv-only and cannot
// smuggle a shell script into the execution graph.

var LinuxKernelPlanProducts = map[string]bool{
	"config": true, "image": true, "vmlinux": true,
	"kernel_release": true, "system_map": true, "module_symvers": true,
	"modules": true, "modules_builtin": true,
	"modules_builtin_modinfo": true, "modules_order": true, "sdk": true,
}

var actionRecipePlaceholder = regexp.MustCompile(`\$\{(source|input|output|tool|tree|work|content):([^}]+)\}`)

// ActionRecipeTemplateRetainsDynamicShellSyntax reports whether an already
// lexicalized argv value still depends on shell expansion. Typed action-plan
// placeholders are execution bindings rather than shell syntax, so they are
// removed before checking for a remaining dollar or command substitution.
func ActionRecipeTemplateRetainsDynamicShellSyntax(value string) bool {
	return strings.ContainsAny(actionRecipePlaceholder.ReplaceAllString(value, ""), "$`")
}

const (
	ActionRecipeContentTransformMakeShellWord                = "gnu-make-shell-safe-word"
	ActionRecipeContentTransformMakeShellSingleWord          = "gnu-make-shell-single-word-literal"
	ActionRecipeContentTransformMakeShellSingleQuotedSegment = "gnu-make-shell-single-quoted-segment"
	ActionRecipeContentTransformMakeShellValue               = "gnu-make-shell-value"
	ActionRecipeArgumentTransformContentTemplateBase64       = "content-template-base64"
)

// NormalizeActionRecipeMakeShellValue applies GNU Make's $(shell ...) text
// normalization without interpreting the result as shell syntax. This is the
// field-typed transform for process-environment values.
func NormalizeActionRecipeMakeShellValue(raw string) (string, error) {
	value, err := normalizeActionRecipeMakeShellNewlines(raw)
	if err != nil {
		return "", err
	}
	if compactKbuildContainsPrivateProvenanceByte(value) || compactKbuildContainsPrivateToolsetPathByte(value) {
		return "", fmt.Errorf("GNU Make shell output contains a reserved provenance byte")
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("GNU Make shell output contains NUL")
	}
	return value, nil
}

// QuoteActionRecipeMakeShellSingleWord implements the bounded content
// transform named by ActionRecipeContentTransformMakeShellSingleWord. GNU Make
// removes trailing newlines from $(shell ...) output and replaces every other
// newline (or CRLF pair) with a space before the recipe shell parses it. The
// result must describe exactly one static, non-operator shell word. Returning a
// POSIX single-quoted literal preserves that word when it is substituted into
// an already-planned script without permitting the generated bytes to add
// syntax or expansions.
func QuoteActionRecipeMakeShellSingleWord(raw string) (string, error) {
	_, word, err := normalizeActionRecipeMakeShellSingleWord(raw)
	if err != nil {
		return "", err
	}
	return "'" + strings.ReplaceAll(word, "'", "'\\''") + "'", nil
}

// FormatActionRecipeMakeShellSingleQuotedSegment implements the bounded
// transform used when the same $(shell ...) result also occurs inside an
// existing source single-quoted recipe word (notably Kbuild's savedcmd_ line).
// It preserves the normalized source spelling, rather than the cooked argv
// word, because backslashes and quotes are bytes in that surrounding quote.
// Values which GNU Make's make-cmd helper would rewrite are rejected: without
// executing source-defined Make at action time, accepting $, #, or ' could
// make the saved command differ from the command which actually ran.
func FormatActionRecipeMakeShellSingleQuotedSegment(raw string) (string, error) {
	value, _, err := normalizeActionRecipeMakeShellSingleWord(raw)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(value, "$#'") {
		return "", fmt.Errorf("GNU Make shell output is not invariant in a single-quoted command segment")
	}
	return value, nil
}

func normalizeActionRecipeMakeShellSingleWord(raw string) (string, string, error) {
	value, err := normalizeActionRecipeMakeShellNewlines(raw)
	if err != nil {
		return "", "", err
	}
	if compactKbuildContainsPrivateProvenanceByte(value) || compactKbuildContainsPrivateToolsetPathByte(value) {
		return "", "", fmt.Errorf("GNU Make shell output contains a reserved provenance byte")
	}
	if strings.ContainsRune(value, 0) {
		return "", "", fmt.Errorf("GNU Make shell output contains NUL")
	}
	if strings.Contains(value, compactKbuildLiteralDollarToken) {
		return "", "", fmt.Errorf("GNU Make shell output contains a reserved lexical marker")
	}
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return "", "", fmt.Errorf("parse GNU Make shell output: %w", err)
	}
	if len(tokens) != 1 || tokens[0].operator {
		return "", "", fmt.Errorf("GNU Make shell output is not exactly one non-operator word")
	}
	if err := validateActionRecipeStaticShellWord(value); err != nil {
		return "", "", err
	}
	word := strings.ReplaceAll(tokens[0].value, compactKbuildLiteralDollarToken, "$")
	return value, word, nil
}

// normalizeActionRecipeMakeShellNewlines applies the newline portion of GNU
// Make's $(shell ...) normalization. A CR is accepted only as part of a CRLF
// pair: terminal CRLF pairs are removed and non-terminal pairs become spaces.
// Rejecting every residual CR avoids silently accepting output whose meaning
// differs between host text conventions.
func normalizeActionRecipeMakeShellNewlines(raw string) (string, error) {
	value := raw
	for strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
		value = strings.TrimSuffix(value, "\r")
	}
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	if strings.ContainsRune(value, '\r') {
		return "", fmt.Errorf("GNU Make shell output contains an unsupported bare carriage return")
	}
	return value, nil
}

// validateActionRecipeStaticShellWord rejects source forms whose value would
// depend on shell expansion. Quotes and backslashes may still provide ordinary
// POSIX lexical quoting; lexCompactKbuildRecipe above computes the resulting
// word. This keeps the accepted language independent of filesystem contents,
// environment variables, and the selected shell implementation.
func validateActionRecipeStaticShellWord(value string) error {
	quote := byte(0)
	wordStarted := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote == '\'' {
			wordStarted = true
			if character == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			wordStarted = true
			switch character {
			case '"':
				quote = 0
			case '$', '`':
				return fmt.Errorf("GNU Make shell output contains dynamic shell expansion")
			case '\\':
				if index+1 == len(value) {
					return fmt.Errorf("GNU Make shell output has a trailing escape")
				}
				next := value[index+1]
				if !strings.ContainsRune("$`\"\\", rune(next)) {
					return fmt.Errorf("GNU Make shell output uses unsupported double-quoted escape")
				}
				index++
			}
			continue
		}

		switch character {
		case '\'', '"':
			quote = character
			wordStarted = true
		case '\\':
			if index+1 == len(value) {
				return fmt.Errorf("GNU Make shell output has a trailing escape")
			}
			index++
			wordStarted = true
		case '$', '`':
			return fmt.Errorf("GNU Make shell output contains dynamic shell expansion")
		case '*', '?', '[':
			return fmt.Errorf("GNU Make shell output contains dynamic pathname expansion")
		case '{', '}':
			return fmt.Errorf("GNU Make shell output contains shell-dependent brace syntax")
		case '~':
			if !wordStarted {
				return fmt.Errorf("GNU Make shell output contains dynamic tilde expansion")
			}
			wordStarted = true
		case '#':
			if !wordStarted {
				return fmt.Errorf("GNU Make shell output begins a shell comment")
			}
			wordStarted = true
		case ' ', '\t', '\r', '\n', ';', '|', '&', '<', '>', '(', ')':
			// Word cardinality and operators are rejected by the lexer above.
		default:
			wordStarted = true
		}
	}
	return nil
}

// ActionRecipeContentSubstitution derives one argv/environment fragment from
// the bytes of an already declared source or node input. This is the typed
// action-graph equivalent of GNU Make expansion-time queries whose input is a
// generated file: the planner records the producer edge, while the consuming
// action reads the immutable File only after Bazel has materialized it.
//
// Input is a source:BINDING or input:BINDING reference. Transform names the
// exact, bounded byte-to-text operation; no command or shell is executed.
type ActionRecipeContentSubstitution struct {
	Input     string `json:"input"`
	Transform string `json:"transform"`
}

// ActionRecipeArgumentTransform applies one typed, deterministic transform to
// an argument after generated content has been materialized. Index addresses
// the argument in ActionRecipe.Arguments; transforms are ordered by Index so
// their canonical encoding is independent of planner traversal order.
type ActionRecipeArgumentTransform struct {
	Index     int    `json:"index"`
	Transform string `json:"transform"`
}

// ActionRecipeCommandReplay describes one command which a declared source
// script may invoke after the corresponding source-owned work has already been
// materialized as ordinary action-plan dependencies. The private proxy accepts
// only an exact argv vector and verifies the declared results before returning
// success; it cannot execute another build graph dynamically.
type ActionRecipeCommandReplay struct {
	Name        string                                `json:"name"`
	Invocations []ActionRecipeCommandReplayInvocation `json:"invocations"`
}

type ActionRecipeCommandReplayInvocation struct {
	Arguments []string `json:"arguments"`
	Outputs   []string `json:"outputs"`
}

// ActionRecipe is deliberately an argv description, not a compiler model.
// Every string may contain the exact tokens ${source:KEY}, ${input:KEY},
// ${output:KEY}, ${tool:KEY}, ${tree:KEY}, or ${content:KEY}. The runner rejects
// undeclared, unused, or malformed bindings. Tool names beginning with
// "input:" select a generated executable from the node's input bindings.
type ActionRecipe struct {
	Schema             string                          `json:"schema"`
	Kind               string                          `json:"kind"`
	Tool               string                          `json:"tool"`
	Arguments          []string                        `json:"arguments"`
	ArgumentTransforms []ActionRecipeArgumentTransform `json:"argument_transforms,omitempty"`
	// ArgumentsFile replaces the expanded recipe argv with the internal
	// actionfile response-file protocol. The runner writes the expanded argv as
	// a canonical JSON string array in its private work root and invokes
	// actionfile with only "-arguments_file PATH".
	ArgumentsFile    bool              `json:"arguments_file,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	// ExecutionDirectory selects a canonical relative directory below the
	// private WorkingDirectory as the tool's cwd. Keeping cwd separate from the
	// staging root lets source-selected commands use ordinary relative path
	// operands without rewriting path-shaped data in their argv or byte streams.
	ExecutionDirectory string `json:"execution_directory,omitempty"`
	// WorkingDirectories names canonical relative directories which must exist
	// below WorkingDirectory before the tool runs. This models tools which write
	// implicit outputs into a declared directory without requiring a fake file
	// binding solely to create that directory.
	WorkingDirectories []string `json:"working_directories,omitempty"`
	// WorkingTrees names declared tree bindings whose complete directory contents
	// are staged at their tree-relative paths below WorkingDirectory. The trees
	// must form a collision-free immutable baseline of regular directories and
	// files (contained regular-file symlinks are copied by value; directory
	// symlinks are invalid). They are materialized before WorkingInputs, so an
	// exact file binding owns a leaf which overlaps that merged baseline.
	WorkingTrees []string `json:"working_trees,omitempty"`
	// WorkingInputs stages declared source/input bindings at canonical paths
	// below WorkingDirectory before the tool runs. Keys have the form
	// "source:BINDING" or "input:BINDING". Source placeholders keep naming
	// their immutable source-tree files after staging so quoted checked-in
	// includes retain normal compiler lookup semantics; generated input
	// placeholders name their staged writable-object-tree copies. WorkingOutputs
	// collects files created at canonical paths below WorkingDirectory into
	// declared output bindings after the tool exits. These generic mappings are
	// required by upstream host tools such as modpost, whose file names are part
	// of their protocol rather than command-line arguments.
	WorkingInputs  map[string]string `json:"working_inputs,omitempty"`
	WorkingOutputs map[string]string `json:"working_outputs,omitempty"`
	// ObservedOutputs snapshots canonical paths below WorkingDirectory after
	// staging, then writes an always-present absolute state to each declared
	// output binding after the tool exits. The state distinguishes an absent,
	// present, or deleted path and retains the node which last changed it.
	ObservedOutputs map[string]string `json:"observed_outputs,omitempty"`
	// ObservedOutputBases is keyed by an ObservedOutputs binding. Each value
	// names declared input bindings containing projected maximal predecessor
	// states. The runner merges them into the absolute regular-file state which
	// it materializes and snapshots immediately before this action.
	ObservedOutputBases map[string][]string `json:"observed_output_bases,omitempty"`
	// ContentSubstitutions declares ${content:NAME} values used by arguments or
	// environment entries. They remain
	// separate from ordinary path placeholders so recipe validation can prove
	// that every dynamic value is backed by an exact graph input.
	ContentSubstitutions map[string]ActionRecipeContentSubstitution `json:"content_substitutions,omitempty"`
	CommandReplays       []ActionRecipeCommandReplay                `json:"command_replays,omitempty"`
	// ExecutableInputs names declared input bindings that are programs invoked
	// by the primary recipe tool. Bazel output Files are not intrinsically
	// executable, so the runner materializes private executable copies without
	// changing the immutable producer outputs.
	ExecutableInputs []string `json:"executable_inputs,omitempty"`
	Stdin            string   `json:"stdin,omitempty"`
	Stdout           string   `json:"stdout,omitempty"`
	Sources          []string `json:"sources,omitempty"`
	Inputs           []string `json:"inputs,omitempty"`
	Outputs          []string `json:"outputs"`
	Trees            []string `json:"trees,omitempty"`
	// AuxiliaryTools are additional identity-bound configured executables used
	// by this argv (for example the selected objcopy passed to pahole through
	// LLVM_OBJCOPY).
	// They remain explicit node markers so map_directory never discovers tools
	// by inspecting recipe contents.
	AuxiliaryTools []string `json:"auxiliary_tools,omitempty"`
}

// cloneActionRecipe returns a recipe which owns every mutable field. Recipes
// are content-addressed once interned in an ActionPlan, so neither the caller's
// value nor another interned recipe may share mutable backing storage with the
// clone.
func cloneActionRecipe(recipe ActionRecipe) ActionRecipe {
	recipe.Arguments = slices.Clone(recipe.Arguments)
	recipe.ArgumentTransforms = slices.Clone(recipe.ArgumentTransforms)
	recipe.Environment = maps.Clone(recipe.Environment)
	recipe.WorkingDirectories = slices.Clone(recipe.WorkingDirectories)
	recipe.WorkingTrees = slices.Clone(recipe.WorkingTrees)
	recipe.WorkingInputs = maps.Clone(recipe.WorkingInputs)
	recipe.WorkingOutputs = maps.Clone(recipe.WorkingOutputs)
	recipe.ObservedOutputs = maps.Clone(recipe.ObservedOutputs)
	if recipe.ObservedOutputBases != nil {
		bases := make(map[string][]string, len(recipe.ObservedOutputBases))
		for binding, base := range recipe.ObservedOutputBases {
			bases[binding] = slices.Clone(base)
		}
		recipe.ObservedOutputBases = bases
	}
	recipe.ContentSubstitutions = maps.Clone(recipe.ContentSubstitutions)
	recipe.CommandReplays = slices.Clone(recipe.CommandReplays)
	for replayIndex := range recipe.CommandReplays {
		replay := &recipe.CommandReplays[replayIndex]
		replay.Invocations = slices.Clone(replay.Invocations)
		for invocationIndex := range replay.Invocations {
			invocation := &replay.Invocations[invocationIndex]
			invocation.Arguments = slices.Clone(invocation.Arguments)
			invocation.Outputs = slices.Clone(invocation.Outputs)
		}
	}
	recipe.ExecutableInputs = slices.Clone(recipe.ExecutableInputs)
	recipe.Sources = slices.Clone(recipe.Sources)
	recipe.Inputs = slices.Clone(recipe.Inputs)
	recipe.Outputs = slices.Clone(recipe.Outputs)
	recipe.Trees = slices.Clone(recipe.Trees)
	recipe.AuxiliaryTools = slices.Clone(recipe.AuxiliaryTools)
	return recipe
}

// actionRecipeToolsetScopes derives the exact compiler-toolset authorities
// carried by a recipe. The marker tree records this deterministic projection
// of the content-addressed recipe so Bazel can bind only the required typed
// closures without reading recipe contents in the map_directory callback.
func actionRecipeToolsetScopes(recipe ActionRecipe) ([]string, error) {
	scopes := map[string]bool{}
	visit := func(value string) error {
		_, err := toolaction.RewriteExecutionRootProvenanceValue(value, func(scope, canonical string) (string, error) {
			scopes[scope] = true
			return canonical, nil
		})
		return err
	}

	for _, value := range recipe.Arguments {
		if err := visit(value); err != nil {
			return nil, fmt.Errorf("recipe toolset scope argument: %w", err)
		}
	}
	for name, value := range recipe.Environment {
		if err := visit(value); err != nil {
			return nil, fmt.Errorf("recipe toolset scope environment %q: %w", name, err)
		}
	}
	if err := visit(recipe.WorkingDirectory); err != nil {
		return nil, fmt.Errorf("recipe toolset scope working directory: %w", err)
	}
	for replayIndex, replay := range recipe.CommandReplays {
		for invocationIndex, invocation := range replay.Invocations {
			for argumentIndex, value := range invocation.Arguments {
				if err := visit(value); err != nil {
					return nil, fmt.Errorf(
						"recipe toolset scope replay %d invocation %d argument %d: %w",
						replayIndex, invocationIndex, argumentIndex, err,
					)
				}
			}
			for outputIndex, value := range invocation.Outputs {
				if err := visit(value); err != nil {
					return nil, fmt.Errorf(
						"recipe toolset scope replay %d invocation %d output %d: %w",
						replayIndex, invocationIndex, outputIndex, err,
					)
				}
			}
		}
	}

	// Ordinary evaluated scripts are stored as base64 so arbitrary shell bytes
	// remain one canonical JSON string. Decode only the semantic scriptrun flag;
	// content-template transforms remain plaintext until the action executes and
	// were already visited above.
	if recipe.Tool == compactKbuildScriptRunnerRole {
		transformed := map[int]bool{}
		for _, transform := range recipe.ArgumentTransforms {
			transformed[transform.Index] = true
		}
		visitEncodedScript := func(index int, encoded string) error {
			if transformed[index] {
				return nil
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				// The selected runner owns malformed base64 diagnostics. An
				// undecodable value cannot hide raw provenance delimiter bytes from
				// the scans above.
				return nil
			}
			if err := visit(string(decoded)); err != nil {
				return fmt.Errorf("recipe toolset scope evaluated script: %w", err)
			}
			return nil
		}
		for index, argument := range recipe.Arguments {
			if index > 0 && (recipe.Arguments[index-1] == "-script_content_base64" || recipe.Arguments[index-1] == "--script_content_base64") {
				if err := visitEncodedScript(index, argument); err != nil {
					return nil, err
				}
				continue
			}
			for _, prefix := range []string{"-script_content_base64=", "--script_content_base64="} {
				if encoded, ok := strings.CutPrefix(argument, prefix); ok {
					if err := visitEncodedScript(index, encoded); err != nil {
						return nil, err
					}
					break
				}
			}
		}
	}

	return slices.Sorted(maps.Keys(scopes)), nil
}

// normalizeActionRecipeToolsetPathCapabilities verifies every Make-visible
// compiler path with the workload-local authority that issued it, then removes
// the ephemeral authentication tag. The resulting deterministic tokens are
// the only form permitted in content-addressed recipes and runtime runners.
func normalizeActionRecipeToolsetPathCapabilities(
	recipe *ActionRecipe,
	normalize func(string) (string, error),
) error {
	if recipe == nil || normalize == nil {
		return nil
	}
	normalized := cloneActionRecipe(*recipe)
	if err := normalizeActionRecipeToolsetPathCapabilitiesInPlace(&normalized, normalize); err != nil {
		return err
	}
	*recipe = normalized
	return nil
}

func normalizeActionRecipeToolsetPathCapabilitiesInPlace(
	recipe *ActionRecipe,
	normalize func(string) (string, error),
) error {
	apply := func(label string, value *string) error {
		normalized, err := normalize(*value)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		*value = normalized
		return nil
	}
	for index := range recipe.Arguments {
		if err := apply(fmt.Sprintf("recipe argument %d toolset-path capability", index), &recipe.Arguments[index]); err != nil {
			return err
		}
	}
	for name, value := range recipe.Environment {
		if err := apply("recipe environment "+strconv.Quote(name)+" toolset-path capability", &value); err != nil {
			return err
		}
		recipe.Environment[name] = value
	}
	if err := apply("recipe working directory toolset-path capability", &recipe.WorkingDirectory); err != nil {
		return err
	}
	for replayIndex := range recipe.CommandReplays {
		replay := &recipe.CommandReplays[replayIndex]
		for invocationIndex := range replay.Invocations {
			invocation := &replay.Invocations[invocationIndex]
			for argumentIndex := range invocation.Arguments {
				if err := apply(fmt.Sprintf(
					"recipe replay %d invocation %d argument %d toolset-path capability",
					replayIndex, invocationIndex, argumentIndex,
				), &invocation.Arguments[argumentIndex]); err != nil {
					return err
				}
			}
			for outputIndex := range invocation.Outputs {
				if err := apply(fmt.Sprintf(
					"recipe replay %d invocation %d output %d toolset-path capability",
					replayIndex, invocationIndex, outputIndex,
				), &invocation.Outputs[outputIndex]); err != nil {
					return err
				}
			}
		}
	}

	// scriptrun transports ordinary evaluated shell text as base64. Authenticate
	// and normalize tokens inside that decoded runtime surface as well. A
	// content-template transform keeps plaintext in the argument until execution
	// and was therefore handled by the direct argument pass above.
	if recipe.Tool != compactKbuildScriptRunnerRole {
		return nil
	}
	transformed := map[int]bool{}
	for _, transform := range recipe.ArgumentTransforms {
		transformed[transform.Index] = true
	}
	type encodedScriptArgument struct {
		index          int
		prefix         string
		decoded        string
		normalized     string
		transformed    bool
		decodeError    error
		normalizeError error
	}
	encodedScripts := []encodedScriptArgument{}
	appendEncodedScript := func(index int, prefix, encoded string) {
		script := encodedScriptArgument{
			index: index, prefix: prefix, transformed: transformed[index],
		}
		if !script.transformed {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			script.decodeError = err
			if err == nil {
				script.decoded = string(decoded)
				script.normalized, err = normalize(script.decoded)
				if err != nil {
					script.normalizeError = fmt.Errorf("recipe evaluated script toolset-path capability: %w", err)
				}
			}
		}
		encodedScripts = append(encodedScripts, script)
	}
	for index := range recipe.Arguments {
		if index > 0 && (recipe.Arguments[index-1] == "-script_content_base64" || recipe.Arguments[index-1] == "--script_content_base64") {
			appendEncodedScript(index, "", recipe.Arguments[index])
			continue
		}
		for _, prefix := range []string{"-script_content_base64=", "--script_content_base64="} {
			encoded, ok := strings.CutPrefix(recipe.Arguments[index], prefix)
			if !ok {
				continue
			}
			appendEncodedScript(index, prefix, encoded)
			break
		}
	}
	scriptContentArgumentCount := len(encodedScripts)
	for _, argument := range recipe.Arguments {
		switch argument {
		case "-script", "--script", "-script_content", "--script_content", "-script_stdin", "--script_stdin":
			scriptContentArgumentCount++
			continue
		}
		for _, prefix := range []string{
			"-script=", "--script=", "-script_content=", "--script_content=", "-script_stdin=", "--script_stdin=",
		} {
			if strings.HasPrefix(argument, prefix) {
				scriptContentArgumentCount++
				break
			}
		}
	}
	for _, script := range encodedScripts {
		if script.normalizeError != nil {
			return script.normalizeError
		}
	}
	literalOffsets, err := actionRecipeLiteralTreeOffsetArguments(recipe.Arguments)
	if err != nil {
		return err
	}
	if len(literalOffsets) != 0 {
		if scriptContentArgumentCount != 1 || len(encodedScripts) != 1 {
			return fmt.Errorf(
				"recipe literal evaluated-script tree offsets require exactly one untransformed base64 script argument, got %d script arguments (%d base64)",
				scriptContentArgumentCount, len(encodedScripts),
			)
		}
		script := &encodedScripts[0]
		if script.transformed {
			return fmt.Errorf("recipe literal evaluated-script tree offsets cannot address a transformed script argument")
		}
		if script.decodeError != nil {
			return fmt.Errorf("recipe literal evaluated-script tree offsets cannot address an invalid base64 script: %w", script.decodeError)
		}
		rebased, err := rebaseActionRecipeLiteralTreeOffsets(
			recipe.Arguments, literalOffsets, script.decoded, script.normalized,
		)
		if err != nil {
			return err
		}
		recipe.Arguments = rebased
	}
	for _, script := range encodedScripts {
		if script.transformed || script.decodeError != nil {
			// Without byte-addressed literal markers, the selected runner owns
			// malformed base64 diagnostics. Base64 cannot contain the private
			// provenance delimiters, so it cannot conceal an unnormalized token.
			continue
		}
		recipe.Arguments[script.index] = script.prefix + base64.StdEncoding.EncodeToString([]byte(script.normalized))
	}
	return nil
}

type actionRecipeLiteralTreeOffsetArgument struct {
	valueIndex int
	prefix     string
	offset     int
}

func actionRecipeLiteralTreeOffsetArguments(arguments []string) ([]actionRecipeLiteralTreeOffsetArgument, error) {
	var offsets []actionRecipeLiteralTreeOffsetArgument
	seen := map[int]bool{}
	appendOffset := func(valueIndex int, prefix, value string) error {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return fmt.Errorf("recipe has invalid literal evaluated-script tree offset %q", value)
		}
		if seen[offset] {
			return fmt.Errorf("recipe repeats literal evaluated-script tree offset %d", offset)
		}
		seen[offset] = true
		offsets = append(offsets, actionRecipeLiteralTreeOffsetArgument{
			valueIndex: valueIndex, prefix: prefix, offset: offset,
		})
		return nil
	}
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "-literal_tree_offset", "--literal_tree_offset":
			if index+1 == len(arguments) {
				return nil, fmt.Errorf("recipe literal evaluated-script tree offset flag is missing its value")
			}
			index++
			if err := appendOffset(index, "", arguments[index]); err != nil {
				return nil, err
			}
			continue
		}
		for _, prefix := range []string{"-literal_tree_offset=", "--literal_tree_offset="} {
			if value, ok := strings.CutPrefix(arguments[index], prefix); ok {
				if err := appendOffset(index, prefix, value); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	return offsets, nil
}

type actionRecipeScriptTreeMarker struct {
	offset int
	text   string
}

func actionRecipeScriptTreeMarkers(script string) ([]actionRecipeScriptTreeMarker, error) {
	const prefix = "${tree:"
	var markers []actionRecipeScriptTreeMarker
	for cursor := 0; ; {
		relative := strings.Index(script[cursor:], prefix)
		if relative < 0 {
			return markers, nil
		}
		start := cursor + relative
		relativeEnd := strings.IndexByte(script[start+len(prefix):], '}')
		if relativeEnd < 0 {
			return nil, fmt.Errorf("recipe evaluated script contains an unterminated tree marker at byte offset %d", start)
		}
		end := start + len(prefix) + relativeEnd
		marker := script[start : end+1]
		if strings.Contains(marker[len(prefix):len(marker)-1], prefix) {
			return nil, fmt.Errorf("recipe evaluated script contains a nested tree marker at byte offset %d", start)
		}
		markers = append(markers, actionRecipeScriptTreeMarker{offset: start, text: marker})
		cursor = end + 1
	}
}

func rebaseActionRecipeLiteralTreeOffsets(
	arguments []string,
	offsets []actionRecipeLiteralTreeOffsetArgument,
	before, after string,
) ([]string, error) {
	beforeMarkers, err := actionRecipeScriptTreeMarkers(before)
	if err != nil {
		return nil, err
	}
	afterMarkers, err := actionRecipeScriptTreeMarkers(after)
	if err != nil {
		return nil, err
	}
	if len(beforeMarkers) != len(afterMarkers) {
		return nil, fmt.Errorf("recipe toolset-path normalization changed evaluated-script tree marker count from %d to %d", len(beforeMarkers), len(afterMarkers))
	}
	for ordinal := range beforeMarkers {
		if beforeMarkers[ordinal].text != afterMarkers[ordinal].text {
			return nil, fmt.Errorf(
				"recipe toolset-path normalization changed evaluated-script tree marker %d from %q to %q",
				ordinal, beforeMarkers[ordinal].text, afterMarkers[ordinal].text,
			)
		}
	}

	rebased := slices.Clone(arguments)
	for _, literal := range offsets {
		ordinal := -1
		for candidate, marker := range beforeMarkers {
			if marker.offset == literal.offset {
				ordinal = candidate
				break
			}
		}
		if ordinal < 0 {
			return nil, fmt.Errorf(
				"recipe literal evaluated-script tree offset %d does not identify a complete tree marker",
				literal.offset,
			)
		}
		rebased[literal.valueIndex] = literal.prefix + strconv.Itoa(afterMarkers[ordinal].offset)
	}
	return rebased, nil
}

// ActionPlan is the typed form used by planners and unit tests. WriteStages
// writes the sharded marker layout consumed by map_directory:
//
//	schema/linux-kernel-plan-v4
//	toolsets/{target,host}/sha256-<digest>
//	products/<product>/root/<tree>/<canonical path>
//	sources/src-########/<namespace>/<canonical path>
//	recipes/<sha256>.json
//	index/<ordinal>/<node sha256>
//	nodes/<stage>/<sha256>/{kind,recipe,tool,product}/<value>
//	nodes/<stage>/<sha256>/in/source/<role>/<ordinal>/src-########
//	nodes/<stage>/<sha256>/in/node-pack/<role>/<role chunk>.<payload>
//	nodes/<stage>/<sha256>/in/tree/<name>
//	nodes/<stage>/<sha256>/in/tool/<scope>/<role>/<scoped|unscoped>
//	nodes/<stage>/<sha256>/in/toolset/<scope>
//	nodes/<stage>/<sha256>/out/<tree>/<slot>/<physical artifact path>
//
// A packed node-input payload contains comma-separated
// inputOrdinal.producerOrdinal.slot tuples in lowercase base36. Node ordinals
// come from the global lexical node-ID index above; each role chunk contains at
// most 64 tuples, and the complete marker filename is bounded to 240 bytes.
//
// Marker contents are empty except recipes. All callback decisions therefore
// depend only on paths, as required by Bazel 9.
type ActionPlan struct {
	Toolsets map[string]string
	Sources  []ActionPlanSource
	Recipes  map[string]ActionRecipe
	Nodes    []ActionPlanNode
	Products []ActionPlanProduct
	metadata *CompactMetadata
	// selectionGraph is process-local provenance used while first-class deferred
	// query nodes resolve their exact artifact owners. Serialized node edges own
	// the final execution contract.
	selectionGraph *compactKbuildSelectionGraph
	// deferredContentBuilding detects source-level query cycles before a query
	// action can recursively try to bind its own opaque result through an
	// exported environment value.
	deferredContentBuilding map[string]bool
	// pathSensitiveArchiveOutputs records outputs whose archive payload may retain
	// member pathnames. Consumers must stage the producing node's input closure
	// beside the archive before invoking the selected tool. This is planning-only
	// provenance: the resulting node edges and WorkingInputs own the serialized
	// execution contract.
	pathSensitiveArchiveOutputs map[actionPlanOutputRef]bool
	// observedOutputBases is planning-only state lineage keyed by an observed
	// capture's physical tree/path identity. appendActionPlanNode converts these
	// refs into ordinary node input edges plus the recipe runtime contract before
	// either content ID is computed.
	observedOutputBases map[string][]ActionPlanNodeEdge

	// Planning routinely asks whether an already-materialized action owns a
	// prerequisite. Keep that operation linear over the whole plan instead of
	// rescanning every node for every edge. The counts make the indexes safe for
	// callers that append directly to the public slices before returning to the
	// generic planner.
	nodeLookupCount   int
	nodesByID         map[string]ActionPlanNode
	nodeIndexesByID   map[string]uint32
	outputProducers   map[string]actionPlanOutputRef
	sourceLookupCount int
	sourceIDs         map[string]string
	sourcesByID       map[string]ActionPlanSource
	maximumSourceID   int

	// workingTreeTopologyCache contains successful DFS-postorder ancestor
	// closures. Node indices remain stable while appendActionPlanNode grows the
	// plan, while every full lookup rebuild or explicit invalidation clears the
	// cache before a possibly-mutated public Nodes slice is used again.
	workingTreeTopologyCache        map[string][]uint32
	workingTreeTopologyCacheIndexes int

	// Probe discovery executes the complete selected lowering so every lazy
	// expansion and validation runs, but never serializes its provisional plan.
	// Keep exact canonical recipe witnesses for collision checking while
	// releasing executor-only recipe and node payloads as soon as the next node
	// proves that the previous append can no longer acquire archive provenance.
	probeDiscoveryOnly            bool
	probeDiscoveryCompactedNodes  int
	probeDiscoveryRecipeCanonical map[string][]byte
	probeDiscoveryRetainedRecipes map[string]bool
}

type actionPlanOutputRef struct {
	producerID string
	slot       int
}

func (p *ActionPlan) markPathSensitiveArchiveOutput(producerID string, slot int) error {
	if p == nil {
		return fmt.Errorf("cannot mark a path-sensitive archive output in a nil action plan")
	}
	p.ensureNodeLookupIndexes()
	nodeIndex, ok := p.nodeIndexesByID[producerID]
	if !ok {
		return fmt.Errorf("path-sensitive archive producer %q is absent from the action plan", producerID)
	}
	node := p.Nodes[nodeIndex]
	if slot < 0 || slot >= len(node.Outputs) {
		return fmt.Errorf("path-sensitive archive producer %q has no output slot %d", producerID, slot)
	}
	if p.pathSensitiveArchiveOutputs == nil {
		p.pathSensitiveArchiveOutputs = map[actionPlanOutputRef]bool{}
	}
	p.pathSensitiveArchiveOutputs[actionPlanOutputRef{producerID: producerID, slot: slot}] = true
	return nil
}

func (p *ActionPlan) isPathSensitiveArchiveOutput(producerID string, slot int) bool {
	return p != nil && p.pathSensitiveArchiveOutputs[actionPlanOutputRef{producerID: producerID, slot: slot}]
}

func (p *ActionPlan) hasPathSensitiveArchiveOutput(producerID string) bool {
	if p == nil {
		return false
	}
	p.ensureNodeLookupIndexes()
	nodeIndex, ok := p.nodeIndexesByID[producerID]
	if !ok {
		return false
	}
	node := p.Nodes[nodeIndex]
	for slot := range node.Outputs {
		if p.pathSensitiveArchiveOutputs[actionPlanOutputRef{producerID: producerID, slot: slot}] {
			return true
		}
	}
	return false
}

type ActionPlanSource struct {
	ID        string
	Namespace string
	Path      string
}

type ActionPlanNode struct {
	ID             string
	Stage          string
	Kind           string
	Recipe         string
	Tool           string
	Product        string
	Sources        []ActionPlanSourceEdge
	Inputs         []ActionPlanNodeEdge
	Trees          []string
	AuxiliaryTools []string
	Outputs        []ActionPlanOutput
}

type ActionPlanSourceEdge struct {
	Role     string
	SourceID string
}

type ActionPlanNodeEdge struct {
	Role       string
	ProducerID string
	Slot       int
}

// ActionPlanInputBinding identifies the exact physical producer output bound
// to one recipe input. Path is relative to the TreeArtifact named by Tree.
type ActionPlanInputBinding struct {
	Tree string `json:"tree"`
	Path string `json:"path"`
}

// ActionPlanInputBindings is the compact execution contract for one node's
// generated inputs. Bindings are keyed by the recipe's exact role:%08d names.
// Packed node markers remain the source of Bazel graph edges; this manifest
// gives the action runner a bounded file-based mapping once those edges have
// resolved to physical TreeArtifact children.
type ActionPlanInputBindings struct {
	Schema   string                            `json:"schema"`
	Bindings map[string]ActionPlanInputBinding `json:"bindings"`
}

// Validate verifies the complete standalone input-binding contract.
func (b ActionPlanInputBindings) Validate() error {
	if b.Schema != LinuxKernelInputBindingsSchema {
		return fmt.Errorf("input bindings schema %q, want %q", b.Schema, LinuxKernelInputBindingsSchema)
	}
	if b.Bindings == nil {
		return fmt.Errorf("input bindings must contain a bindings object")
	}
	if len(b.Bindings) > maximumActionPlanInputBindingCount {
		return fmt.Errorf("input bindings contain %d entries, want at most %d", len(b.Bindings), maximumActionPlanInputBindingCount)
	}
	seenOrdinals := make([]bool, len(b.Bindings))
	for _, key := range slices.Sorted(maps.Keys(b.Bindings)) {
		role, ordinalText, ok := strings.Cut(key, ":")
		if !ok || strings.Contains(ordinalText, ":") {
			return fmt.Errorf("input binding key %q is not role:%%08d", key)
		}
		if err := validatePlanName("input binding role", role); err != nil {
			return err
		}
		if len(ordinalText) != 8 || strings.Trim(ordinalText, "0123456789") != "" {
			return fmt.Errorf("input binding key %q does not have an eight-digit ordinal", key)
		}
		ordinal, err := strconv.Atoi(ordinalText)
		if err != nil || ordinal < 0 || ordinal >= len(seenOrdinals) {
			return fmt.Errorf("input binding key %q has non-contiguous ordinal %q for %d bindings", key, ordinalText, len(b.Bindings))
		}
		if seenOrdinals[ordinal] {
			return fmt.Errorf("input bindings repeat ordinal %s", ordinalText)
		}
		seenOrdinals[ordinal] = true

		binding := b.Bindings[key]
		if !LinuxKernelPlanTrees[binding.Tree] {
			return fmt.Errorf("input binding %q has unknown tree %q", key, binding.Tree)
		}
		if err := validatePlanRelativePath("input binding physical artifact", binding.Path); err != nil {
			return fmt.Errorf("input binding %q: %w", key, err)
		}
	}
	return nil
}

// CanonicalJSON returns the bounded canonical bytes used both on disk and for
// the manifest's content identity.
func (b ActionPlanInputBindings) CanonicalJSON() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode input bindings: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxActionPlanInputBindingsBytes {
		return nil, fmt.Errorf("canonical input bindings contain %d bytes, want at most %d", len(data), MaxActionPlanInputBindingsBytes)
	}
	return data, nil
}

// ID returns the SHA-256 identity of CanonicalJSON.
func (b ActionPlanInputBindings) ID() (string, error) {
	data, err := b.CanonicalJSON()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// DecodeActionPlanInputBindings decodes and independently validates one
// bounded canonical manifest. Unknown fields, trailing values, and alternate
// JSON spellings are rejected so the filename digest has exactly one encoding.
func DecodeActionPlanInputBindings(data []byte) (ActionPlanInputBindings, error) {
	if len(data) == 0 {
		return ActionPlanInputBindings{}, fmt.Errorf("input bindings are empty")
	}
	if len(data) > MaxActionPlanInputBindingsBytes {
		return ActionPlanInputBindings{}, fmt.Errorf("input bindings contain %d bytes, want at most %d", len(data), MaxActionPlanInputBindingsBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bindings ActionPlanInputBindings
	if err := decoder.Decode(&bindings); err != nil {
		return ActionPlanInputBindings{}, fmt.Errorf("decode input bindings: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return ActionPlanInputBindings{}, fmt.Errorf("decode input bindings: trailing JSON value")
		}
		return ActionPlanInputBindings{}, fmt.Errorf("decode input bindings: %w", err)
	}
	canonical, err := bindings.CanonicalJSON()
	if err != nil {
		return ActionPlanInputBindings{}, err
	}
	if !bytes.Equal(data, canonical) {
		return ActionPlanInputBindings{}, fmt.Errorf("input bindings are not canonically encoded")
	}
	return bindings, nil
}

type ActionPlanOutput struct {
	Tree string
	// Path is the canonical logical object-tree path observed by Kbuild.
	Path string
	// ArtifactPath is the unique physical path declared below the output
	// TreeArtifact. An empty value means Path. Keeping the two identities
	// separate lets ordered Make invocations publish successive immutable
	// versions of one logical path without giving two Bazel actions the same
	// output artifact.
	ArtifactPath string
	// ObservedPath marks an internal absolute-state output. The selected recipe does
	// not own this binding directly; mapdirectoryrecipe compares this logical
	// working-tree path before and after the command and writes an always-present
	// observation envelope to the declared ArtifactPath. This field is planner
	// metadata and is carried by the recipe JSON, not the map_directory marker.
	ObservedPath string
	// persistent is planner-only semantic provenance for a compiler side output
	// which later Kbuild invocations may consume. It is intentionally absent from
	// the serialized node marker: SDK projections consume it before the plan is
	// content-addressed and emitted.
	persistent bool
}

func actionPlanOutputArtifactPath(output ActionPlanOutput) string {
	if output.ArtifactPath != "" {
		return output.ArtifactPath
	}
	return output.Path
}

func actionPlanOutputIsCanonical(output ActionPlanOutput) bool {
	return actionPlanOutputArtifactPath(output) == output.Path
}

type ActionPlanProduct struct {
	Name string
	Tree string
	Path string
}

func actionPlanLookupKey(namespace, relativePath string) string {
	return namespace + "\x00" + relativePath
}

func (p *ActionPlan) ensureNodeLookupIndexes() {
	if p == nil {
		return
	}
	if p.nodesByID != nil && p.nodeIndexesByID != nil && p.outputProducers != nil && p.nodeLookupCount == len(p.Nodes) {
		return
	}
	p.clearWorkingTreeTopologyCache()
	p.nodesByID = make(map[string]ActionPlanNode, len(p.Nodes))
	p.nodeIndexesByID = make(map[string]uint32, len(p.Nodes))
	p.outputProducers = map[string]actionPlanOutputRef{}
	for index, node := range p.Nodes {
		p.nodesByID[node.ID] = node
		p.nodeIndexesByID[node.ID] = uint32(index)
		for slot, output := range node.Outputs {
			if !actionPlanOutputIsCanonical(output) {
				continue
			}
			key := actionPlanLookupKey(output.Tree, output.Path)
			if _, exists := p.outputProducers[key]; !exists {
				p.outputProducers[key] = actionPlanOutputRef{producerID: node.ID, slot: slot}
			}
		}
	}
	p.nodeLookupCount = len(p.Nodes)
}

func (p *ActionPlan) recordNodeLookup(node ActionPlanNode) {
	// appendActionPlanNode refreshes the index before appending, so the
	// indexed count must describe exactly the prefix preceding node. Fall back
	// to one rebuild for callers that do not follow that sequence.
	if p.nodesByID == nil || p.nodeIndexesByID == nil || p.outputProducers == nil || p.nodeLookupCount != len(p.Nodes)-1 {
		p.ensureNodeLookupIndexes()
		return
	}
	p.nodesByID[node.ID] = node
	p.nodeIndexesByID[node.ID] = uint32(len(p.Nodes) - 1)
	for slot, output := range node.Outputs {
		if !actionPlanOutputIsCanonical(output) {
			continue
		}
		key := actionPlanLookupKey(output.Tree, output.Path)
		if _, exists := p.outputProducers[key]; !exists {
			p.outputProducers[key] = actionPlanOutputRef{producerID: node.ID, slot: slot}
		}
	}
	p.nodeLookupCount = len(p.Nodes)
}

func (p *ActionPlan) clearWorkingTreeTopologyCache() {
	if p == nil {
		return
	}
	p.workingTreeTopologyCache = nil
	p.workingTreeTopologyCacheIndexes = 0
}

// walkCompactKbuildWorkingTreeTopology visits root's ActionPlan ancestry in
// the same DFS postorder as the uncached working-tree closure planner. The
// cache owns topology only: processNode is invoked on every newly visited node
// for every call, so recipes, source bindings, outputs, archive provenance,
// and native-root policy are always read from the current planning state.
//
// visited is shared by every root in one closure computation and implements
// the original visited-on-entry behavior. A separate per-root bitset records a
// complete reusable topology. If that root encounters an already-visited
// uncached subtree, traversal still preserves the original behavior but the
// necessarily-partial topology is not cached.
func (p *ActionPlan) walkCompactKbuildWorkingTreeTopology(
	root string,
	visited []bool,
	processNode func(uint32) error,
) error {
	if p == nil {
		return fmt.Errorf("working object-tree closure requires an action plan")
	}
	p.ensureNodeLookupIndexes()
	if len(visited) != len(p.Nodes) {
		return fmt.Errorf("working object-tree traversal has %d visited entries for %d action plan nodes", len(visited), len(p.Nodes))
	}
	if cached, ok := p.workingTreeTopologyCache[root]; ok {
		for _, index := range cached {
			if visited[index] {
				continue
			}
			visited[index] = true
			if err := processNode(index); err != nil {
				return err
			}
		}
		return nil
	}
	rootIndex, ok := p.nodeIndexesByID[root]
	if !ok {
		return fmt.Errorf("working object-tree ancestor %q is absent from the action plan", root)
	}
	if visited[rootIndex] {
		return nil
	}

	localSeen := make([]bool, len(p.Nodes))
	topology := []uint32{}
	cacheable := p.workingTreeTopologyCacheIndexes < maximumWorkingTreeTopologyCacheIndexes
	record := func(index uint32) {
		if !cacheable {
			return
		}
		if len(topology) >= maximumWorkingTreeTopologyCacheIndexes-p.workingTreeTopologyCacheIndexes {
			cacheable = false
			topology = nil
			return
		}
		topology = append(topology, index)
	}

	var visit func(string) error
	visit = func(producer string) error {
		index, ok := p.nodeIndexesByID[producer]
		if !ok {
			return fmt.Errorf("working object-tree ancestor %q is absent from the action plan", producer)
		}
		if localSeen[index] {
			return nil
		}
		if cached, ok := p.workingTreeTopologyCache[producer]; ok {
			// The cached list includes producer itself in postorder. Do not mark
			// producer before splicing it: a cycle may already have marked an
			// ancestor locally, and that ancestor must still be emitted by the
			// stack frame which first entered it.
			for _, cachedIndex := range cached {
				if localSeen[cachedIndex] {
					continue
				}
				localSeen[cachedIndex] = true
				if !visited[cachedIndex] {
					visited[cachedIndex] = true
					if err := processNode(cachedIndex); err != nil {
						return err
					}
				}
				record(cachedIndex)
			}
			return nil
		}
		if visited[index] {
			// The original traversal stops here. Without a cached closure for
			// this producer, doing the same leaves this root's local topology
			// incomplete, so do not publish it as reusable.
			cacheable = false
			topology = nil
			return nil
		}

		visited[index] = true
		localSeen[index] = true
		node := p.Nodes[index]
		for _, edge := range node.Inputs {
			if err := visit(edge.ProducerID); err != nil {
				return err
			}
		}
		if err := processNode(index); err != nil {
			return err
		}
		record(index)
		return nil
	}
	if err := visit(root); err != nil {
		return err
	}
	if cacheable {
		if p.workingTreeTopologyCache == nil {
			p.workingTreeTopologyCache = map[string][]uint32{}
		}
		stored := append([]uint32(nil), topology...)
		p.workingTreeTopologyCache[root] = stored
		p.workingTreeTopologyCacheIndexes += len(stored)
	}
	return nil
}

func (p *ActionPlan) ensureSourceLookupIndex() error {
	if p == nil {
		return fmt.Errorf("cannot index sources in a nil action plan")
	}
	if p.sourceIDs != nil && p.sourcesByID != nil && p.sourceLookupCount == len(p.Sources) {
		return nil
	}
	p.sourceIDs = make(map[string]string, len(p.Sources))
	p.sourcesByID = make(map[string]ActionPlanSource, len(p.Sources))
	p.maximumSourceID = 0
	for _, source := range p.Sources {
		if !validSourceID(source.ID) {
			return fmt.Errorf("action plan contains invalid source ID %q", source.ID)
		}
		key := actionPlanLookupKey(source.Namespace, source.Path)
		if _, exists := p.sourceIDs[key]; !exists {
			p.sourceIDs[key] = source.ID
		}
		if previous, exists := p.sourcesByID[source.ID]; exists && previous != source {
			return fmt.Errorf("action plan repeats source ID %q", source.ID)
		}
		p.sourcesByID[source.ID] = source
		value, _ := strconv.Atoi(strings.TrimPrefix(source.ID, "src-"))
		if value > p.maximumSourceID {
			p.maximumSourceID = value
		}
	}
	p.sourceLookupCount = len(p.Sources)
	return nil
}

func (p *ActionPlan) invalidateLookupIndexes() {
	if p == nil {
		return
	}
	p.nodesByID = nil
	p.nodeIndexesByID = nil
	p.outputProducers = nil
	p.nodeLookupCount = 0
	p.clearWorkingTreeTopologyCache()
	p.sourceIDs = nil
	p.sourcesByID = nil
	p.sourceLookupCount = 0
	p.maximumSourceID = 0
}

type actionPlanEntry struct {
	path string
	data []byte
}

func (r ActionRecipe) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (r ActionRecipe) ID() (string, error) {
	data, err := r.CanonicalJSON()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (r ActionRecipe) Validate() error {
	if r.Schema != LinuxKernelPlanSchema {
		return fmt.Errorf("recipe schema %q, want %q", r.Schema, LinuxKernelPlanSchema)
	}
	if compactKbuildContainsPrivateProvenanceByte(r.Tool) {
		return fmt.Errorf("recipe retains a reserved recursive Make provenance byte")
	}
	if !LinuxKernelPlanNodeKinds[r.Kind] {
		return fmt.Errorf("recipe has unsupported kind %q", r.Kind)
	}
	if err := validatePlanName("recipe tool", r.Tool); err != nil {
		if !strings.HasPrefix(r.Tool, "input:") || validateBindingName("generated tool input", strings.TrimPrefix(r.Tool, "input:")) != nil {
			return err
		}
	}
	if r.ArgumentsFile && r.Tool != "actionfile" {
		return fmt.Errorf("recipe arguments_file requires actionfile tool, got %q", r.Tool)
	}
	for ordinal, transform := range r.ArgumentTransforms {
		if transform.Index < 0 || transform.Index >= len(r.Arguments) {
			return fmt.Errorf("recipe argument transform ordinal %d index %d is out of range for %d arguments", ordinal, transform.Index, len(r.Arguments))
		}
		if ordinal != 0 {
			previous := r.ArgumentTransforms[ordinal-1].Index
			if transform.Index == previous {
				return fmt.Errorf("recipe repeats argument transform index %d", transform.Index)
			}
			if transform.Index < previous {
				return fmt.Errorf("recipe argument transform ordinal %d index %d is not in canonical numeric order", ordinal, transform.Index)
			}
		}
		if transform.Transform != ActionRecipeArgumentTransformContentTemplateBase64 {
			return fmt.Errorf("recipe argument transform ordinal %d has unsupported transform %q", ordinal, transform.Transform)
		}
	}
	if len(r.Outputs) == 0 {
		return fmt.Errorf("recipe has no outputs")
	}
	for kind, names := range map[string][]string{
		"source": r.Sources, "input": r.Inputs, "output": r.Outputs, "tree": r.Trees,
	} {
		seen := map[string]bool{}
		for _, name := range names {
			if err := validateBindingName(kind, name); err != nil {
				return err
			}
			if seen[name] {
				return fmt.Errorf("recipe repeats %s binding %q", kind, name)
			}
			seen[name] = true
		}
	}
	restoredEnvironmentNames := map[string]string{}
	for _, key := range sortedStringMapKeys(r.Environment) {
		restored, err := RestoreCompactKbuildLiteralActionMarkers(key)
		if err != nil {
			return fmt.Errorf("recipe environment name %q protected source literal: %w", key, err)
		}
		if restored == "" || strings.ContainsAny(restored, "=\x00") {
			return fmt.Errorf("recipe has invalid environment name %q", restored)
		}
		if compactKbuildContainsPrivateProvenanceByte(restored) {
			return fmt.Errorf("recipe retains a reserved recursive Make provenance byte")
		}
		if compactKbuildContainsPrivateToolsetPathByte(restored) {
			return fmt.Errorf("recipe environment name contains a reserved toolset-path provenance byte")
		}
		if previous, exists := restoredEnvironmentNames[restored]; exists {
			return fmt.Errorf("recipe environment names %q and %q restore to the same name %q", previous, key, restored)
		}
		restoredEnvironmentNames[restored] = key
	}
	if r.WorkingDirectory != "" && strings.ContainsRune(r.WorkingDirectory, 0) {
		return fmt.Errorf("recipe working directory contains NUL")
	}
	if strings.Contains(r.WorkingDirectory, "${content:") {
		return fmt.Errorf("recipe working directory cannot depend on generated content")
	}
	if r.ExecutionDirectory != "" {
		if r.WorkingDirectory == "" {
			return fmt.Errorf("recipe execution_directory requires working_directory")
		}
		if err := validatePlanRelativePath("recipe execution directory", r.ExecutionDirectory); err != nil {
			return err
		}
	}
	if len(r.WorkingDirectories) != 0 && r.WorkingDirectory == "" {
		return fmt.Errorf("recipe working_directories require working_directory")
	}
	if len(r.WorkingTrees) != 0 && r.WorkingDirectory == "" {
		return fmt.Errorf("recipe working_trees require working_directory")
	}
	declared := map[string]map[string]bool{
		"source": sliceSet(r.Sources), "input": sliceSet(r.Inputs),
		"output": sliceSet(r.Outputs), "tree": sliceSet(r.Trees),
		"tool": {}, "work": {}, "content": {},
	}
	if r.WorkingDirectory != "" {
		declared["work"]["root"] = true
	}
	for _, tool := range r.AuxiliaryTools {
		if !toolaction.ValidBinding(tool) {
			return fmt.Errorf("recipe has invalid auxiliary tool binding %q", tool)
		}
		if declared["tool"][tool] {
			return fmt.Errorf("recipe repeats auxiliary tool %q", tool)
		}
		declared["tool"][tool] = true
	}
	used := map[string]map[string]bool{"source": {}, "input": {}, "output": {}, "tool": {}, "tree": {}, "work": {}, "content": {}}
	executableInputs := map[string]bool{}
	for _, name := range r.ExecutableInputs {
		if !declared["input"][name] {
			return fmt.Errorf("recipe executable input %q is not a declared input binding", name)
		}
		if executableInputs[name] {
			return fmt.Errorf("recipe repeats executable input %q", name)
		}
		executableInputs[name] = true
		used["input"][name] = true
	}
	if (len(r.WorkingInputs) != 0 || len(r.WorkingOutputs) != 0 || len(r.ObservedOutputs) != 0 || len(r.ObservedOutputBases) != 0) && r.WorkingDirectory == "" {
		return fmt.Errorf("recipe working input/output/observation mappings require working_directory")
	}
	workingDirectories := map[string]string{}
	if r.ExecutionDirectory != "" {
		workingDirectories[r.ExecutionDirectory] = "execution directory"
	}
	workingPaths := map[string]string{}
	seenWorkingDirectories := map[string]bool{}
	for ordinal, relative := range r.WorkingDirectories {
		if err := validatePlanRelativePath("recipe working directory", relative); err != nil {
			return err
		}
		if seenWorkingDirectories[relative] {
			return fmt.Errorf("recipe working directory ordinal %d repeats path %q", ordinal, relative)
		}
		seenWorkingDirectories[relative] = true
		workingDirectories[relative] = "working directory"
	}
	seenWorkingTrees := map[string]bool{}
	for ordinal, binding := range r.WorkingTrees {
		if !declared["tree"][binding] {
			return fmt.Errorf("recipe working tree ordinal %d references undeclared tree binding %q", ordinal, binding)
		}
		if seenWorkingTrees[binding] {
			return fmt.Errorf("recipe working tree ordinal %d repeats tree binding %q", ordinal, binding)
		}
		if ordinal != 0 && binding < r.WorkingTrees[ordinal-1] {
			return fmt.Errorf("recipe working tree ordinal %d binding %q is not in canonical lexical order", ordinal, binding)
		}
		seenWorkingTrees[binding] = true
		used["tree"][binding] = true
	}
	for name, substitution := range r.ContentSubstitutions {
		if err := validateBindingName("recipe content substitution", name); err != nil {
			return err
		}
		kind, binding, ok := strings.Cut(substitution.Input, ":")
		if !ok || (kind != "source" && kind != "input") || !declared[kind][binding] {
			return fmt.Errorf("recipe content substitution %q references undeclared source/input %q", name, substitution.Input)
		}
		if substitution.Transform != ActionRecipeContentTransformMakeShellWord &&
			substitution.Transform != ActionRecipeContentTransformMakeShellSingleWord &&
			substitution.Transform != ActionRecipeContentTransformMakeShellSingleQuotedSegment &&
			substitution.Transform != ActionRecipeContentTransformMakeShellValue {
			return fmt.Errorf("recipe content substitution %q has unsupported transform %q", name, substitution.Transform)
		}
		declared["content"][name] = true
		used[kind][binding] = true
	}
	for _, transform := range r.ArgumentTransforms {
		if transform.Transform != ActionRecipeArgumentTransformContentTemplateBase64 {
			continue
		}
		if err := validateActionRecipeContentTemplate(r.Arguments[transform.Index], r.ContentSubstitutions); err != nil {
			return fmt.Errorf("recipe argument %d: %w", transform.Index, err)
		}
	}
	seenReplayNames := map[string]bool{}
	for _, replay := range r.CommandReplays {
		if err := validatePlanName("recipe command replay", replay.Name); err != nil {
			return err
		}
		if seenReplayNames[replay.Name] {
			return fmt.Errorf("recipe repeats command replay %q", replay.Name)
		}
		seenReplayNames[replay.Name] = true
		if len(replay.Invocations) == 0 {
			return fmt.Errorf("recipe command replay %q has no invocations", replay.Name)
		}
		for invocationIndex, invocation := range replay.Invocations {
			for argumentIndex, argument := range invocation.Arguments {
				if strings.ContainsRune(argument, 0) {
					return fmt.Errorf("recipe command replay %q invocation %d argument %d contains NUL", replay.Name, invocationIndex, argumentIndex)
				}
			}
			for outputIndex, output := range invocation.Outputs {
				if strings.ContainsRune(output, 0) {
					return fmt.Errorf("recipe command replay %q invocation %d output %d contains NUL", replay.Name, invocationIndex, outputIndex)
				}
			}
		}
	}
	for binding, relative := range r.WorkingInputs {
		kind, name, ok := strings.Cut(binding, ":")
		if !ok || (kind != "source" && kind != "input") || !declared[kind][name] {
			return fmt.Errorf("recipe working input %q is not a declared source/input binding", binding)
		}
		if err := validatePlanRelativePath("recipe working input path", relative); err != nil {
			return err
		}
		if directory := workingDirectories[relative]; directory != "" {
			return fmt.Errorf("recipe working path %q is used by %s and %s", relative, directory, binding)
		}
		if previous := workingPaths[relative]; previous != "" {
			return fmt.Errorf("recipe working path %q is used by %s and %s", relative, previous, binding)
		}
		workingPaths[relative] = binding
	}
	for binding, relative := range r.WorkingOutputs {
		if !declared["output"][binding] {
			return fmt.Errorf("recipe working output %q is not a declared output binding", binding)
		}
		if err := validatePlanRelativePath("recipe working output path", relative); err != nil {
			return err
		}
		if directory := workingDirectories[relative]; directory != "" {
			return fmt.Errorf("recipe working path %q is used by %s and output:%s", relative, directory, binding)
		}
		if previous := workingPaths[relative]; previous != "" && !strings.HasPrefix(previous, "source:") && !strings.HasPrefix(previous, "input:") {
			return fmt.Errorf("recipe working path %q is used by %s and output:%s", relative, previous, binding)
		}
		// Reusing one staged input path as an output models upstream tools that
		// intentionally mutate an ELF in place. The immutable graph input is
		// copied into the private working directory before execution and the
		// mutated regular file is collected into a distinct declared output.
		workingPaths[relative] = "output:" + binding
		used["output"][binding] = true
	}
	observedOutputBindings := map[string]bool{}
	for binding, relative := range r.ObservedOutputs {
		if !declared["output"][binding] {
			return fmt.Errorf("recipe observed output %q is not a declared output binding", binding)
		}
		if _, direct := r.WorkingOutputs[binding]; direct {
			return fmt.Errorf("recipe output binding %q is both a working output and an observed output", binding)
		}
		if err := validatePlanRelativePath("recipe observed output path", relative); err != nil {
			return err
		}
		if directory := workingDirectories[relative]; directory != "" {
			return fmt.Errorf("recipe working path %q is used by %s and observed output:%s", relative, directory, binding)
		}
		if previous := workingPaths[relative]; previous != "" && !strings.HasPrefix(previous, "source:") && !strings.HasPrefix(previous, "input:") {
			return fmt.Errorf("recipe working path %q is used by %s and observed output:%s", relative, previous, binding)
		}
		// Observing a staged input path is intentional: it models upstream tools
		// which may mutate one of several candidate files in place. Two outputs
		// may not observe the same path because their captures would be aliases.
		workingPaths[relative] = "observed output:" + binding
		observedOutputBindings[binding] = true
		used["output"][binding] = true
	}
	if err := validateActionRecipeWorkingPathStructure(workingPaths, workingDirectories); err != nil {
		return err
	}
	for binding, base := range r.ObservedOutputBases {
		if !observedOutputBindings[binding] {
			return fmt.Errorf("recipe observed output base %q is not an observed output binding", binding)
		}
		seenStates := map[string]bool{}
		for ordinal, state := range base {
			if !declared["input"][state] {
				return fmt.Errorf("recipe observed output base %q state ordinal %d references undeclared input binding %q", binding, ordinal, state)
			}
			if seenStates[state] {
				return fmt.Errorf("recipe observed output base %q repeats state input binding %q", binding, state)
			}
			seenStates[state] = true
			used["input"][state] = true
		}
	}
	if strings.HasPrefix(r.Tool, "input:") {
		input := strings.TrimPrefix(r.Tool, "input:")
		if !declared["input"][input] {
			return fmt.Errorf("recipe generated tool references undeclared input %q", input)
		}
	} else {
		if declared["tool"][r.Tool] {
			return fmt.Errorf("recipe primary tool %q is also auxiliary", r.Tool)
		}
		declared["tool"][r.Tool] = true
		used["tool"][r.Tool] = true
	}
	nonLiteralValues := append([]string{r.Tool}, r.Arguments...)
	nonLiteralValues = append(nonLiteralValues, r.WorkingDirectory)
	for _, replay := range r.CommandReplays {
		for _, invocation := range replay.Invocations {
			nonLiteralValues = append(nonLiteralValues, invocation.Outputs...)
		}
	}
	for _, value := range nonLiteralValues {
		if compactKbuildContainsProtectedLiteralActionMarker(value) {
			return fmt.Errorf("recipe retains a protected source literal outside its environment")
		}
	}
	values := append([]string(nil), nonLiteralValues...)
	for replayIndex, replay := range r.CommandReplays {
		for invocationIndex, invocation := range replay.Invocations {
			for argumentIndex, value := range invocation.Arguments {
				if compactKbuildContainsProtectedLiteralActionMarker(value) {
					if _, err := RestoreCompactKbuildLiteralActionMarkers(value); err != nil {
						return fmt.Errorf(
							"recipe command replay %d invocation %d argument %d protected source literal: %w",
							replayIndex, invocationIndex, argumentIndex, err,
						)
					}
				}
				values = append(values, value)
			}
		}
	}
	if r.Stdin != "" {
		kind, name, ok := strings.Cut(r.Stdin, ":")
		if !ok || (kind != "source" && kind != "input") || !declared[kind][name] {
			return fmt.Errorf("recipe stdin %q is not a declared source/input binding", r.Stdin)
		}
		used[kind][name] = true
	}
	if r.Stdout != "" {
		if !declared["output"][r.Stdout] {
			return fmt.Errorf("recipe stdout references undeclared output %q", r.Stdout)
		}
		if observedOutputBindings[r.Stdout] {
			return fmt.Errorf("recipe observed output binding %q cannot also capture stdout", r.Stdout)
		}
		used["output"][r.Stdout] = true
	}
	for _, key := range sortedStringMapKeys(r.Environment) {
		value := r.Environment[key]
		if compactKbuildContainsProtectedLiteralActionMarker(value) {
			if _, err := RestoreCompactKbuildLiteralActionMarkers(value); err != nil {
				return fmt.Errorf("recipe environment %q protected source literal: %w", key, err)
			}
		}
		values = append(values, value)
	}
	for _, value := range values {
		if compactKbuildContainsPrivateProvenanceByte(value) {
			return fmt.Errorf("recipe retains a reserved recursive Make provenance byte")
		}
		if err := toolaction.ValidateExecutionRootProvenanceValue(value); err != nil {
			return fmt.Errorf("recipe contains invalid toolset-path provenance: %w", err)
		}
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if match[1] == "output" && observedOutputBindings[match[2]] {
				return fmt.Errorf("recipe observed output binding %q cannot be referenced as a path placeholder", match[2])
			}
		}
		if err := validateRecipePlaceholders(value, declared, used); err != nil {
			return err
		}
	}
	for kind, names := range declared {
		for name := range names {
			if kind == "input" && strings.TrimPrefix(r.Tool, "input:") == name && strings.HasPrefix(r.Tool, "input:") {
				used[kind][name] = true
			}
			// Sources, node inputs, and trees may be declared only to close the
			// exact sandbox/RBE input set. They need not appear in argv.
			if kind != "source" && kind != "input" && kind != "tree" && kind != "work" && !used[kind][name] {
				return fmt.Errorf("recipe declares unused %s binding %q", kind, name)
			}
		}
	}
	return nil
}

func validateRecipePlaceholders(value string, declared, used map[string]map[string]bool) error {
	for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
		kind, name := match[1], match[2]
		if !declared[kind][name] {
			return fmt.Errorf("recipe references undeclared %s binding %q", kind, name)
		}
		used[kind][name] = true
	}
	// Any leftover opener is a typo or an unsupported substitution and must not
	// reach an execution shell/tool as literal text.
	cleaned := actionRecipePlaceholder.ReplaceAllString(value, "")
	if strings.Contains(cleaned, "${") {
		return fmt.Errorf("recipe contains malformed or unsupported placeholder in %q", value)
	}
	return nil
}

func sliceSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func sortedStringMapKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

type actionPlanPackedInputTuple struct {
	inputOrdinal    int
	producerOrdinal int
	slot            int
}

func actionPlanNodeOrdinalIndex(nodes []ActionPlanNode) (map[string]int, []string, error) {
	if len(nodes) > maximumActionPlanOrdinal+1 {
		return nil, nil, fmt.Errorf("kernel action plan node index space is exhausted")
	}
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.ID
	}
	sort.Strings(ids)
	ordinals := make(map[string]int, len(ids))
	for ordinal, id := range ids {
		if ordinal != 0 && id == ids[ordinal-1] {
			return nil, nil, fmt.Errorf("repeated node ID %q", id)
		}
		ordinals[id] = ordinal
	}
	return ordinals, ids, nil
}

func validateActionPlanInputOrdinal(nodeID string, inputOrdinal int) error {
	if inputOrdinal < 0 || inputOrdinal > maximumActionPlanOrdinal {
		return fmt.Errorf("node %s input ordinal %d is out of range", nodeID, inputOrdinal)
	}
	return nil
}

func encodeActionPlanPackedInputTuple(tuple actionPlanPackedInputTuple) string {
	return strconv.FormatInt(int64(tuple.inputOrdinal), 36) + "." +
		strconv.FormatInt(int64(tuple.producerOrdinal), 36) + "." +
		strconv.FormatInt(int64(tuple.slot), 36)
}

func actionPlanPackedInputMarkerFilenames(tuples []actionPlanPackedInputTuple) ([]string, error) {
	filenames := []string{}
	for cursor := 0; cursor < len(tuples); {
		roleChunk := len(filenames)
		if roleChunk > maximumActionPlanOrdinal {
			return nil, fmt.Errorf("packed action-plan input chunk index %d is out of range", roleChunk)
		}
		filename := strings.Builder{}
		filename.WriteString(planOrdinal(roleChunk))
		filename.WriteByte('.')
		tupleCount := 0
		for cursor < len(tuples) && tupleCount < maximumActionPlanInputTuplesPerMarker {
			encoded := encodeActionPlanPackedInputTuple(tuples[cursor])
			separator := 0
			if tupleCount != 0 {
				separator = 1
			}
			if filename.Len()+separator+len(encoded) > maximumActionPlanInputMarkerFilename {
				if tupleCount == 0 {
					return nil, fmt.Errorf("packed action-plan input tuple %q exceeds %d-byte marker filename", encoded, maximumActionPlanInputMarkerFilename)
				}
				break
			}
			if separator != 0 {
				filename.WriteByte(',')
			}
			filename.WriteString(encoded)
			cursor++
			tupleCount++
		}
		filenames = append(filenames, filename.String())
	}
	return filenames, nil
}

func actionPlanPackedInputEntries(
	node ActionPlanNode,
	root string,
	nodeOrdinals map[string]int,
) ([]actionPlanEntry, error) {
	byRole := map[string][]actionPlanPackedInputTuple{}
	for inputOrdinal, input := range node.Inputs {
		if err := validateActionPlanInputOrdinal(node.ID, inputOrdinal); err != nil {
			return nil, err
		}
		if err := validatePlanName("input role", input.Role); err != nil {
			return nil, err
		}
		if err := validatePlanDigest("producer node ID", input.ProducerID); err != nil {
			return nil, err
		}
		if input.Slot < 0 || input.Slot > 99999999 {
			return nil, fmt.Errorf("node %s input slot %d is out of range", node.ID, input.Slot)
		}
		producerOrdinal, ok := nodeOrdinals[input.ProducerID]
		if !ok {
			// The complete-plan validation below reports the unknown producer after
			// higher-priority node/toolset errors. The provisional ordinal is never
			// serialized when that validation fails.
			producerOrdinal = 0
		}
		byRole[input.Role] = append(byRole[input.Role], actionPlanPackedInputTuple{
			inputOrdinal:    inputOrdinal,
			producerOrdinal: producerOrdinal,
			slot:            input.Slot,
		})
	}

	roles := make([]string, 0, len(byRole))
	for role := range byRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	entries := []actionPlanEntry{}
	for _, role := range roles {
		tuples := byRole[role]
		filenames, err := actionPlanPackedInputMarkerFilenames(tuples)
		if err != nil {
			return nil, fmt.Errorf("node %s input role %q: %w", node.ID, role, err)
		}
		for _, filename := range filenames {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "node-pack", role, filename)})
		}
	}
	return entries, nil
}

func actionPlanNodeInputBindings(node ActionPlanNode, nodes map[string]ActionPlanNode) (ActionPlanInputBindings, error) {
	bindings := ActionPlanInputBindings{
		Schema:   LinuxKernelInputBindingsSchema,
		Bindings: make(map[string]ActionPlanInputBinding, len(node.Inputs)),
	}
	for ordinal, input := range node.Inputs {
		producer, ok := nodes[input.ProducerID]
		if !ok {
			return ActionPlanInputBindings{}, fmt.Errorf("node %s references unknown producer %s", node.ID, input.ProducerID)
		}
		if input.Slot < 0 || input.Slot >= len(producer.Outputs) {
			return ActionPlanInputBindings{}, fmt.Errorf("node %s references output slot %d of producer %s with %d outputs", node.ID, input.Slot, producer.ID, len(producer.Outputs))
		}
		output := producer.Outputs[input.Slot]
		key := input.Role + ":" + planOrdinal(ordinal)
		bindings.Bindings[key] = ActionPlanInputBinding{
			Tree: output.Tree,
			Path: actionPlanOutputArtifactPath(output),
		}
	}
	if err := bindings.Validate(); err != nil {
		return ActionPlanInputBindings{}, fmt.Errorf("node %s input bindings: %w", node.ID, err)
	}
	return bindings, nil
}

// ContentID binds a node to its recipe, exact graph edges, and owned outputs.
// Producer IDs make the identity transitively content-addressed.
func (n ActionPlanNode) ContentID() string {
	h := sha256.New()
	write := func(values ...string) {
		for _, value := range values {
			_, _ = h.Write([]byte(value))
		}
		_, _ = h.Write([]byte{0})
	}
	write("linux-kernel-action-node-v3")
	write(n.Stage, n.Kind, n.Recipe, n.Tool, n.Product)
	for i, source := range n.Sources {
		write("source", planOrdinal(i), source.Role, source.SourceID)
	}
	for i, input := range n.Inputs {
		write("input", planOrdinal(i), input.Role, input.ProducerID, planOrdinal(input.Slot))
	}
	for _, tree := range n.Trees {
		write("tree", tree)
	}
	for _, tool := range n.AuxiliaryTools {
		write("auxiliary-tool", tool)
	}
	for i, output := range n.Outputs {
		write("output", planOrdinal(i), output.Tree, output.Path, actionPlanOutputArtifactPath(output), output.ObservedPath)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WriteStages writes one self-contained marker tree per map_directory stage.
// Each shard contains only that stage's nodes and their recipe/source metadata,
// plus the exact output descriptors needed to bind dependencies on an earlier
// stage. This keeps Bazel from expanding and retaining the complete marker
// graph once for every stage callback.
func (p *ActionPlan) WriteStages(outputDirs map[string]string) error {
	if len(outputDirs) != len(linuxKernelPlanStageOrder) {
		return fmt.Errorf("kernel action plan requires exactly %d stage outputs, got %d", len(linuxKernelPlanStageOrder), len(outputDirs))
	}
	seenOutputs := map[string]string{}
	for _, stage := range linuxKernelPlanStageOrder {
		output, ok := outputDirs[stage]
		if !ok || strings.TrimSpace(output) == "" {
			return fmt.Errorf("kernel action plan has no %s stage output", stage)
		}
		canonical := filepath.Clean(output)
		if owner := seenOutputs[canonical]; owner != "" {
			return fmt.Errorf("kernel action plan stages %s and %s share output directory %q", owner, stage, canonical)
		}
		seenOutputs[canonical] = stage
	}
	for stage := range outputDirs {
		if !LinuxKernelPlanStages[stage] {
			return fmt.Errorf("kernel action plan has unknown stage output %q", stage)
		}
	}

	entries, err := p.entries()
	if err != nil {
		return err
	}
	nodes := make(map[string]ActionPlanNode, len(p.Nodes))
	recipeStages := map[string]map[string]bool{}
	sourceStages := map[string]map[string]bool{}
	crossStageOutputs := map[string]map[string]bool{}
	for _, stage := range linuxKernelPlanStageOrder {
		crossStageOutputs[stage] = map[string]bool{}
	}
	for _, node := range p.Nodes {
		nodes[node.ID] = node
		stages := recipeStages[node.Recipe]
		if stages == nil {
			stages = map[string]bool{}
			recipeStages[node.Recipe] = stages
		}
		stages[node.Stage] = true
		for _, source := range node.Sources {
			stages := sourceStages[source.SourceID]
			if stages == nil {
				stages = map[string]bool{}
				sourceStages[source.SourceID] = stages
			}
			stages[node.Stage] = true
		}
	}
	for _, node := range p.Nodes {
		for _, input := range node.Inputs {
			producer := nodes[input.ProducerID]
			if producer.Stage != node.Stage {
				crossStageOutputs[node.Stage][input.ProducerID+":"+planOrdinal(input.Slot)] = true
			}
		}
	}

	stageEntries := map[string][]actionPlanEntry{}
	appendEntry := func(stage string, entry actionPlanEntry) {
		stageEntries[stage] = append(stageEntries[stage], entry)
	}
	appendAllStages := func(entry actionPlanEntry) {
		for _, stage := range linuxKernelPlanStageOrder {
			appendEntry(stage, entry)
		}
	}
	for _, entry := range entries {
		parts := strings.Split(entry.path, "/")
		switch parts[0] {
		case "schema", "toolsets", "index":
			appendAllStages(entry)
		case "recipes":
			id := strings.TrimSuffix(parts[1], ".json")
			for stage := range recipeStages[id] {
				appendEntry(stage, entry)
			}
		case "sources":
			for stage := range sourceStages[parts[1]] {
				appendEntry(stage, entry)
			}
		case "products":
			// Product roots are diagnostic plan metadata and do not create
			// actions. Keep them with the terminal target-stage shard.
			appendEntry("target", entry)
		case "nodes":
			nodeStage, nodeID := parts[1], parts[2]
			appendEntry(nodeStage, entry)
			if len(parts) >= 6 && parts[3] == "out" {
				key := nodeID + ":" + parts[5]
				for _, consumerStage := range linuxKernelPlanStageOrder {
					if consumerStage != nodeStage && crossStageOutputs[consumerStage][key] {
						appendEntry(consumerStage, entry)
					}
				}
			}
		default:
			return fmt.Errorf("kernel action plan cannot shard marker %q", entry.path)
		}
	}
	for _, stage := range linuxKernelPlanStageOrder {
		if err := writeActionPlanTree(outputDirs[stage], stageEntries[stage]); err != nil {
			return fmt.Errorf("write %s action-plan stage: %w", stage, err)
		}
	}
	return nil
}

func (p *ActionPlan) entries() ([]actionPlanEntry, error) {
	if p == nil {
		return nil, fmt.Errorf("kernel action plan is nil")
	}
	nodeOrdinals, indexedNodeIDs, err := actionPlanNodeOrdinalIndex(p.Nodes)
	if err != nil {
		return nil, err
	}
	entries := []actionPlanEntry{{path: path.Join("schema", LinuxKernelPlanSchema)}}
	for ordinal, nodeID := range indexedNodeIDs {
		entries = append(entries, actionPlanEntry{path: path.Join("index", planOrdinal(ordinal), nodeID)})
	}
	for scope, identity := range p.Toolsets {
		if scope != "target" && scope != "host" {
			return nil, fmt.Errorf("unknown toolset scope %q", scope)
		}
		if err := validateProbeIdentity(identity); err != nil {
			return nil, fmt.Errorf("%s toolset: %w", scope, err)
		}
		entries = append(entries, actionPlanEntry{path: path.Join("toolsets", scope, identity)})
	}
	if p.Toolsets["target"] == "" {
		return nil, fmt.Errorf("kernel action plan has no target toolset")
	}

	sources := map[string]ActionPlanSource{}
	for _, source := range p.Sources {
		if !validSourceID(source.ID) {
			return nil, fmt.Errorf("invalid source ID %q", source.ID)
		}
		if err := validatePlanName("source namespace", source.Namespace); err != nil {
			return nil, err
		}
		if err := validatePlanRelativePath("source", source.Path); err != nil {
			return nil, err
		}
		if _, exists := sources[source.ID]; exists {
			return nil, fmt.Errorf("repeated source ID %q", source.ID)
		}
		sources[source.ID] = source
		entries = append(entries, actionPlanEntry{path: path.Join("sources", source.ID, source.Namespace, source.Path)})
	}

	recipeData := map[string][]byte{}
	for id, recipe := range p.Recipes {
		if err := validatePlanDigest("recipe ID", id); err != nil {
			return nil, err
		}
		data, err := recipe.CanonicalJSON()
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", id, err)
		}
		actual := sha256.Sum256(data)
		if got := hex.EncodeToString(actual[:]); got != id {
			return nil, fmt.Errorf("recipe ID %s does not match canonical content %s", id, got)
		}
		recipeData[id] = data
		entries = append(entries, actionPlanEntry{path: path.Join("recipes", id+".json"), data: data})
	}

	nodes := map[string]ActionPlanNode{}
	artifactOutputs := map[string]string{}
	canonicalOutputs := map[string]string{}
	hasHostScopeNode := false
	for _, node := range p.Nodes {
		if err := validatePlanDigest("node ID", node.ID); err != nil {
			return nil, err
		}
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("repeated node ID %q", node.ID)
		}
		if !LinuxKernelPlanStages[node.Stage] || !LinuxKernelPlanNodeKinds[node.Kind] {
			return nil, fmt.Errorf("node %s has unsupported stage/kind %q/%q", node.ID, node.Stage, node.Kind)
		}
		if node.Stage == "prehost" || node.Stage == "host" {
			hasHostScopeNode = true
		}
		if got := node.ContentID(); got != node.ID {
			return nil, fmt.Errorf("node ID %s does not match canonical content %s", node.ID, got)
		}
		if _, ok := recipeData[node.Recipe]; !ok {
			return nil, fmt.Errorf("node %s references unknown recipe %q", node.ID, node.Recipe)
		}
		if p.Recipes[node.Recipe].Kind != node.Kind {
			return nil, fmt.Errorf("node %s kind %q differs from recipe kind %q", node.ID, node.Kind, p.Recipes[node.Recipe].Kind)
		}
		if err := validatePlanName("node tool", node.Tool); err != nil {
			return nil, err
		}
		recipe := p.Recipes[node.Recipe]
		if strings.HasPrefix(recipe.Tool, "input:") {
			if node.Tool != "generated" {
				return nil, fmt.Errorf("node %s uses generated recipe tool %q but marker tool is %q", node.ID, recipe.Tool, node.Tool)
			}
		} else if recipe.Tool != node.Tool {
			return nil, fmt.Errorf("node %s tool %q differs from recipe tool %q", node.ID, node.Tool, recipe.Tool)
		}
		if !LinuxKernelPlanProducts[node.Product] {
			return nil, fmt.Errorf("node %s has unknown product %q", node.ID, node.Product)
		}
		if len(node.Outputs) == 0 {
			return nil, fmt.Errorf("node %s has no outputs", node.ID)
		}
		nodes[node.ID] = node
		root := path.Join("nodes", node.Stage, node.ID)
		entries = append(entries,
			actionPlanEntry{path: path.Join(root, "kind", node.Kind)},
			actionPlanEntry{path: path.Join(root, "recipe", node.Recipe)},
			actionPlanEntry{path: path.Join(root, "tool", node.Tool)},
			actionPlanEntry{path: path.Join(root, "product", node.Product)},
		)
		for i, source := range node.Sources {
			if _, ok := sources[source.SourceID]; !ok {
				return nil, fmt.Errorf("node %s references unknown source %q", node.ID, source.SourceID)
			}
			if err := validatePlanName("source role", source.Role); err != nil {
				return nil, err
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "source", source.Role, planOrdinal(i), source.SourceID)})
		}
		inputEntries, err := actionPlanPackedInputEntries(node, root, nodeOrdinals)
		if err != nil {
			return nil, err
		}
		entries = append(entries, inputEntries...)
		seenTrees := map[string]bool{}
		for _, tree := range node.Trees {
			if err := validatePlanName("input tree", tree); err != nil {
				return nil, err
			}
			if seenTrees[tree] {
				return nil, fmt.Errorf("node %s repeats input tree %q", node.ID, tree)
			}
			seenTrees[tree] = true
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "tree", tree)})
		}
		seenTools := map[string]bool{}
		primaryScope := "target"
		if node.Stage == "prehost" || node.Stage == "host" {
			primaryScope = "host"
		}
		for _, tool := range node.AuxiliaryTools {
			scope, role, scoped, valid := toolaction.SplitBinding(tool)
			if !valid {
				return nil, fmt.Errorf("node %s has invalid auxiliary tool binding %q", node.ID, tool)
			}
			if !scoped {
				scope = "target"
				if node.Stage == "prehost" || node.Stage == "host" {
					scope = "host"
				}
			}
			if seenTools[tool] || scope == primaryScope && role == node.Tool {
				return nil, fmt.Errorf("node %s repeats primary/auxiliary tool %q", node.ID, tool)
			}
			seenTools[tool] = true
			if scope == "host" {
				hasHostScopeNode = true
			}
			bindingForm := "scoped"
			if !scoped {
				bindingForm = "unscoped"
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "tool", scope, role, bindingForm)})
		}
		recipeToolsetScopes, err := actionRecipeToolsetScopes(recipe)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", node.ID, err)
		}
		for _, scope := range recipeToolsetScopes {
			if p.Toolsets[scope] == "" {
				return nil, fmt.Errorf("node %s recipe requires unavailable %s toolset", node.ID, scope)
			}
			if scope == "host" {
				hasHostScopeNode = true
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "toolset", scope)})
		}
		for i, output := range node.Outputs {
			if !LinuxKernelPlanTrees[output.Tree] {
				return nil, fmt.Errorf("node %s has unknown output tree %q", node.ID, output.Tree)
			}
			if !actionPlanStageOwnsOutputTree(node.Stage, output.Tree) {
				return nil, fmt.Errorf("node %s in %s stage cannot write %s tree", node.ID, node.Stage, output.Tree)
			}
			if err := validatePlanRelativePath("node output", output.Path); err != nil {
				return nil, err
			}
			artifactPath := actionPlanOutputArtifactPath(output)
			if err := validatePlanRelativePath("node physical output", artifactPath); err != nil {
				return nil, err
			}
			key := output.Tree + "/" + artifactPath
			if owner := artifactOutputs[key]; owner != "" {
				return nil, fmt.Errorf("physical output %q is owned by both %s and %s", key, owner, node.ID)
			}
			artifactOutputs[key] = node.ID
			if actionPlanOutputIsCanonical(output) {
				logicalKey := output.Tree + "/" + output.Path
				if owner := canonicalOutputs[logicalKey]; owner != "" {
					return nil, fmt.Errorf("canonical output %q is owned by both %s and %s", logicalKey, owner, node.ID)
				}
				canonicalOutputs[logicalKey] = node.ID
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "out", output.Tree, planOrdinal(i), artifactPath)})
		}
		wantSources := make([]string, len(node.Sources))
		for i, edge := range node.Sources {
			wantSources[i] = fmt.Sprintf("%s:%08d", edge.Role, i)
		}
		wantInputs := make([]string, len(node.Inputs))
		for i, edge := range node.Inputs {
			wantInputs[i] = fmt.Sprintf("%s:%08d", edge.Role, i)
		}
		wantOutputs := make([]string, len(node.Outputs))
		for i := range node.Outputs {
			wantOutputs[i] = planOrdinal(i)
		}
		if !equalStringSets(recipe.Sources, wantSources) || !equalStringSets(recipe.Inputs, wantInputs) || !equalStringSets(recipe.Trees, node.Trees) || !equalStringSets(recipe.AuxiliaryTools, node.AuxiliaryTools) || !equalStringSets(recipe.Outputs, wantOutputs) {
			return nil, fmt.Errorf("node %s bindings do not match recipe: sources %q/%q inputs %q/%q trees %q/%q outputs %q/%q", node.ID, wantSources, recipe.Sources, wantInputs, recipe.Inputs, node.Trees, recipe.Trees, wantOutputs, recipe.Outputs)
		}
	}
	if hasHostScopeNode && p.Toolsets["host"] == "" {
		return nil, fmt.Errorf("kernel action plan has host-scope stage nodes but no host toolset")
	}
	for _, node := range p.Nodes {
		bindings, err := actionPlanNodeInputBindings(node, nodes)
		if err != nil {
			return nil, err
		}
		for _, input := range node.Inputs {
			producer, ok := nodes[input.ProducerID]
			if !ok {
				return nil, fmt.Errorf("node %s references unknown producer %s", node.ID, input.ProducerID)
			}
			if input.Slot < 0 || input.Slot >= len(producer.Outputs) {
				return nil, fmt.Errorf("node %s references output slot %d of producer %s with %d outputs", node.ID, input.Slot, producer.ID, len(producer.Outputs))
			}
			stageOrder := map[string]int{"prehost": 0, "bootstrap": 1, "host": 2, "prep": 3, "target": 4}
			if stageOrder[producer.Stage] > stageOrder[node.Stage] {
				return nil, fmt.Errorf(
					"node %s in %s stage (%s/%s, outputs=%v) has backward dependency input %s[%d] from %s node %s (%s/%s, outputs=%v)",
					node.ID, node.Stage, node.Tool, node.Product, node.Outputs,
					input.Role, input.Slot,
					producer.Stage, producer.ID, producer.Tool, producer.Product, producer.Outputs,
				)
			}
		}
		bindingData, err := bindings.CanonicalJSON()
		if err != nil {
			return nil, fmt.Errorf("node %s input bindings: %w", node.ID, err)
		}
		bindingDigest := sha256.Sum256(bindingData)
		bindingID := hex.EncodeToString(bindingDigest[:])
		entries = append(entries, actionPlanEntry{
			path: path.Join("nodes", node.Stage, node.ID, "in", "bindings", bindingID+".json"),
			data: bindingData,
		})
	}
	if err := validateActionPlanAcyclic(nodes); err != nil {
		return nil, err
	}

	seenProducts := map[string]bool{}
	for _, product := range p.Products {
		if !LinuxKernelPlanProducts[product.Name] || !LinuxKernelPlanTrees[product.Tree] {
			return nil, fmt.Errorf("invalid product root %q/%q", product.Name, product.Tree)
		}
		if seenProducts[product.Name] {
			return nil, fmt.Errorf("repeated product root %q", product.Name)
		}
		seenProducts[product.Name] = true
		if err := validatePlanRelativePath("product root", product.Path); err != nil {
			return nil, err
		}
		if product.Path != LinuxKernelTreeRootMarker && canonicalOutputs[product.Tree+"/"+product.Path] == "" {
			return nil, fmt.Errorf("product %q refers to unowned output %s/%s", product.Name, product.Tree, product.Path)
		}
		entries = append(entries, actionPlanEntry{path: path.Join("products", product.Name, "root", product.Tree, product.Path)})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for i := 1; i < len(entries); i++ {
		if entries[i-1].path == entries[i].path {
			return nil, fmt.Errorf("plan repeats marker %q", entries[i].path)
		}
	}
	return entries, nil
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a, b := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateActionPlanAcyclic(nodes map[string]ActionPlanNode) error {
	state := map[string]byte{}
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("kernel action plan contains a cycle at node %s", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, input := range nodes[id].Inputs {
			if err := visit(input.ProducerID); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func writeActionPlanTree(outputDir string, entries []actionPlanEntry) error {
	if strings.TrimSpace(outputDir) == "" {
		return fmt.Errorf("kernel action plan output directory must not be empty")
	}
	outputDir = filepath.Clean(outputDir)
	precreated := false
	if info, err := os.Lstat(outputDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("kernel action plan output %q exists and is not a directory", outputDir)
		}
		children, err := os.ReadDir(outputDir)
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return fmt.Errorf("kernel action plan output directory %q is not empty", outputDir)
		}
		precreated = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outputDir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(outputDir), "."+filepath.Base(outputDir)+".tmp-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(tmp)
		}
	}()
	// The marker tree has many files below a comparatively small directory
	// prefix graph. Calling MkdirAll for every marker repeatedly stats the same
	// ancestors and dominates large kernel plans on remote filesystems. Since
	// tmp is private and initially empty, create each directory exactly once.
	createdDirectories := map[string]bool{tmp: true}
	var ensureDirectory func(string) error
	ensureDirectory = func(directory string) error {
		if createdDirectories[directory] {
			return nil
		}
		parent := filepath.Dir(directory)
		if parent != directory {
			if err := ensureDirectory(parent); err != nil {
				return err
			}
		}
		if err := os.Mkdir(directory, 0o755); err != nil {
			return err
		}
		createdDirectories[directory] = true
		return nil
	}
	for _, entry := range entries {
		filename := filepath.Join(tmp, filepath.FromSlash(entry.path))
		if err := ensureDirectory(filepath.Dir(filename)); err != nil {
			return err
		}
		if err := os.WriteFile(filename, entry.data, 0o644); err != nil {
			return err
		}
	}
	if precreated {
		children, err := os.ReadDir(tmp)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := os.Rename(filepath.Join(tmp, child.Name()), filepath.Join(outputDir, child.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
	} else if err := os.Rename(tmp, outputDir); err != nil {
		return err
	}
	published = true
	return nil
}

func planOrdinal(value int) string { return fmt.Sprintf("%08d", value) }

func validSourceID(value string) bool {
	if len(value) != 12 || !strings.HasPrefix(value, "src-") {
		return false
	}
	for _, c := range value[4:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return value != "src-00000000"
}

func validateProbeIdentity(value string) error {
	digest, ok := strings.CutPrefix(value, "sha256-")
	if !ok {
		return fmt.Errorf("probe identity %q does not start with sha256-", value)
	}
	return validatePlanDigest("probe identity", digest)
}

func validatePlanDigest(kind, value string) error {
	if len(value) != 64 || strings.ToLower(value) != value {
		return fmt.Errorf("%s %q is not a canonical SHA-256 digest", kind, value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s %q is not a canonical SHA-256 digest: %w", kind, value, err)
	}
	return nil
}

func validatePlanName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if len(value) > maximumActionPlanNameComponent {
		return fmt.Errorf("%s %q exceeds %d-byte plan component limit", kind, value, maximumActionPlanNameComponent)
	}
	for i, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (i > 0 && strings.ContainsRune("_.+-", c)) {
			continue
		}
		return fmt.Errorf("%s %q is not a canonical plan name", kind, value)
	}
	return nil
}

func validateBindingName(kind, value string) error {
	parts := strings.Split(value, ":")
	if len(parts) > 2 {
		return fmt.Errorf("invalid %s binding %q", kind, value)
	}
	for _, part := range parts {
		if err := validatePlanName(kind+" binding", part); err != nil {
			return err
		}
	}
	if len(parts) == 2 && (len(parts[1]) != 8 || strings.Trim(parts[1], "0123456789") != "") {
		return fmt.Errorf("invalid %s binding ordinal %q", kind, value)
	}
	return nil
}

func validatePlanRelativePath(kind, value string) error {
	if compactKbuildContainsPrivateProvenanceByte(value) || compactKbuildContainsPrivateToolsetPathByte(value) {
		return fmt.Errorf("kernel action plan %s path contains a reserved recursive Make provenance byte", kind)
	}
	if value == "" || strings.Contains(value, `\`) || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("kernel action plan %s path %q is not a canonical relative path", kind, value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("kernel action plan %s path %q is not a canonical relative path", kind, value)
		}
	}
	return nil
}

// validateActionRecipeWorkingPathStructure rejects file/directory shapes that
// cannot be materialized without treating a declared file as a directory.
// Exact staged-input/output aliases have already collapsed to one file entry
// in files and remain valid for intentional in-place mutation. Directories may
// freely contain files and other directories.
func validateActionRecipeWorkingPathStructure(files, directories map[string]string) error {
	filePaths := sortedStringMapKeys(files)
	directoryPaths := sortedStringMapKeys(directories)
	for _, filePath := range filePaths {
		prefix := filePath + "/"
		if index := sort.SearchStrings(filePaths, prefix); index < len(filePaths) &&
			strings.HasPrefix(filePaths[index], prefix) {
			otherPath := filePaths[index]
			return fmt.Errorf(
				"recipe working file path %q used by %s is an ancestor of file path %q used by %s",
				filePath, files[filePath], otherPath, files[otherPath],
			)
		}
		if index := sort.SearchStrings(directoryPaths, prefix); index < len(directoryPaths) &&
			strings.HasPrefix(directoryPaths[index], prefix) {
			directoryPath := directoryPaths[index]
			return fmt.Errorf(
				"recipe working file path %q used by %s is an ancestor of directory path %q used by %s",
				filePath, files[filePath], directoryPath, directories[directoryPath],
			)
		}
	}
	return nil
}
