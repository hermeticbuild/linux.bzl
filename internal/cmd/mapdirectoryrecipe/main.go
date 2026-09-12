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
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string         { return strings.Join(*f, " ") }
func (f *repeatedFlag) Set(value string) error { *f = append(*f, value); return nil }

type singleFlag struct {
	value string
	set   bool
}

func (f *singleFlag) String() string { return f.value }
func (f *singleFlag) Set(value string) error {
	if f.set {
		return fmt.Errorf("flag may be supplied only once")
	}
	if value == "" {
		return fmt.Errorf("flag value may not be empty")
	}
	f.value = value
	f.set = true
	return nil
}

const (
	maxParameterFileBytes     = 64 << 20
	maxParameterFileLineBytes = 1 << 20
	maxParameterFileArguments = 65536

	// An input-set node contains at most sixteen entries or sixteen child
	// references, but entry paths are deliberately not coupled to host PATH_MAX.
	// Bound both each witness and the complete imported closure before decoding
	// so a malformed callback invocation cannot turn a compact radix graph into
	// unbounded runner memory.
	maxActionPlanInputSetManifestBytes      = 64 << 20
	maxActionPlanInputSetManifestTotalBytes = 64 << 20
	maxActionPlanInputSetManifests          = maxParameterFileArguments
	maxActionPlanInputSetEntries            = 1 << 20
)

// expandParameterFileArguments implements the Bazel multiline parameter-file
// protocol used by this runner. A response file is recognized only when it is
// the sole argument, so direct invocations retain their literal argv (including
// values beginning with '@'). Each LF-terminated line is one exact argument;
// recursive response files are deliberately unsupported.
func expandParameterFileArguments(arguments []string) ([]string, error) {
	if len(arguments) != 1 || !strings.HasPrefix(arguments[0], "@") {
		return arguments, nil
	}
	filename := strings.TrimPrefix(arguments[0], "@")
	if filename == "" {
		return nil, fmt.Errorf("parameter file path is empty")
	}
	if strings.ContainsRune(filename, 0) {
		return nil, fmt.Errorf("parameter file path contains NUL")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open parameter file %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat parameter file %q: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("parameter file %q is not a regular file", filename)
	}
	if info.Size() < 0 || info.Size() > maxParameterFileBytes {
		return nil, fmt.Errorf("parameter file %q exceeds %d bytes", filename, maxParameterFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxParameterFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read parameter file %q: %w", filename, err)
	}
	if len(data) > maxParameterFileBytes {
		return nil, fmt.Errorf("parameter file %q exceeds %d bytes", filename, maxParameterFileBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("parameter file %q contains NUL", filename)
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		return nil, fmt.Errorf("parameter file %q contains carriage return; multiline parameter files require LF line endings", filename)
	}
	if len(data) != 0 && data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("parameter file %q does not end with LF", filename)
	}
	argumentCount := bytes.Count(data, []byte{'\n'})
	if argumentCount > maxParameterFileArguments {
		return nil, fmt.Errorf("parameter file %q contains %d arguments, maximum is %d", filename, argumentCount, maxParameterFileArguments)
	}

	expanded := make([]string, 0, argumentCount)
	for start, ordinal := 0, 0; start < len(data); ordinal++ {
		lineEnd := start + bytes.IndexByte(data[start:], '\n')
		line := data[start:lineEnd]
		if len(line) > maxParameterFileLineBytes {
			return nil, fmt.Errorf("parameter file %q argument %d exceeds %d bytes", filename, ordinal, maxParameterFileLineBytes)
		}
		if len(line) != 0 && line[0] == '@' {
			return nil, fmt.Errorf("parameter file %q argument %d nests the parameter-file protocol", filename, ordinal)
		}
		expanded = append(expanded, string(line))
		start = lineEnd + 1
	}
	return expanded, nil
}

type recipeOptions struct {
	recipe, kind, expectedNodeID, expectedRecipeID      string
	toolRole, workingDirectory, workingDirectoryMarker  string
	inputBindings, expectedInputBindingsID              string
	sourceProjections, expectedSourceProjectionsID      string
	inputSetRoot                                        string
	inputSetManifestRoot                                string
	inputSetStoreAnchors                                map[string]string
	inputSetStorePacks                                  []string
	sources, inputs, outputs, tools, trees              map[string]string
	artifactTrees                                       map[string]string
	privateInputTrees                                   map[string]bool
	inputSetManifests, inputSetSources, inputSetInputs  map[string]string
	runtimeTools                                        map[string]string
	actionArgs                                          []string
	actionEnvironment                                   map[string]string
	auxiliaryActionContracts                            map[string]toolaction.Contract
	toolsetIdentities, toolsetManifests, toolsetAnchors []string
}

func decodeSourceProjections(filename, expectedID string) (kconfig.ActionPlanSourceProjections, error) {
	if filename == "" {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("source projections manifest is required")
	}
	if !isDigest(expectedID) {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("expected source projections ID must be a canonical SHA-256 digest")
	}
	file, err := os.Open(filename)
	if err != nil {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("open source projections manifest %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("stat source projections manifest %q: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("source projections manifest %q is not a regular file", filename)
	}
	if info.Size() > kconfig.MaxActionPlanSourceProjectionsBytes {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf(
			"source projections manifest %q exceeds %d bytes", filename, kconfig.MaxActionPlanSourceProjectionsBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, kconfig.MaxActionPlanSourceProjectionsBytes+1))
	if err != nil {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("read source projections manifest %q: %w", filename, err)
	}
	if len(data) > kconfig.MaxActionPlanSourceProjectionsBytes {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf(
			"source projections manifest %q exceeds %d bytes", filename, kconfig.MaxActionPlanSourceProjectionsBytes,
		)
	}
	projections, err := kconfig.DecodeActionPlanSourceProjections(data)
	if err != nil {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("decode source projections manifest %q: %w", filename, err)
	}
	actualID, err := projections.ID()
	if err != nil {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf("identify source projections manifest %q: %w", filename, err)
	}
	if actualID != expectedID {
		return kconfig.ActionPlanSourceProjections{}, fmt.Errorf(
			"source projections manifest content ID = %q, want %q", actualID, expectedID,
		)
	}
	return projections, nil
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

type resolvedActionPlanInputSetEntry struct {
	entry      kconfig.ActionPlanInputSetEntry
	input      string
	provenance string
}

type resolvedActionPlanInputSet struct {
	root    string
	entries []resolvedActionPlanInputSetEntry
}

func decodeActionPlanInputSetManifest(filename, expectedID string) (kconfig.ActionPlanInputSetNode, int, error) {
	if !isDigest(expectedID) {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("input-set manifest ID %q is not a canonical SHA-256 digest", expectedID)
	}
	file, err := os.Open(filename)
	if err != nil {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("open input-set manifest %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("stat input-set manifest %q: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("input-set manifest %q is not a regular file", filename)
	}
	if info.Size() < 0 || info.Size() > maxActionPlanInputSetManifestBytes {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf(
			"input-set manifest %q exceeds %d bytes", filename, maxActionPlanInputSetManifestBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxActionPlanInputSetManifestBytes+1))
	if err != nil {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("read input-set manifest %q: %w", filename, err)
	}
	if len(data) == 0 {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("input-set manifest %q is empty", filename)
	}
	if len(data) > maxActionPlanInputSetManifestBytes {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf(
			"input-set manifest %q exceeds %d bytes", filename, maxActionPlanInputSetManifestBytes,
		)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var node kconfig.ActionPlanInputSetNode
	if err := decoder.Decode(&node); err != nil {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("decode input-set manifest %q: %w", filename, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("decode input-set manifest %q: trailing JSON value", filename)
		}
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("decode input-set manifest %q: %w", filename, err)
	}
	canonical, err := json.Marshal(node)
	if err != nil {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("encode input-set manifest %q: %w", filename, err)
	}
	if !bytes.Equal(data, canonical) {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf("input-set manifest %q is not canonically encoded", filename)
	}
	digest := sha256.Sum256(canonical)
	if actualID := hex.EncodeToString(digest[:]); actualID != expectedID {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf(
			"input-set manifest content ID = %q, want %q", actualID, expectedID,
		)
	}
	if node.Count < 0 || node.Count > maxActionPlanInputSetEntries {
		return kconfig.ActionPlanInputSetNode{}, 0, fmt.Errorf(
			"input-set manifest %q count %d is outside [0,%d]", expectedID, node.Count, maxActionPlanInputSetEntries,
		)
	}
	return node, len(data), nil
}

func actionPlanInputSetProducerBinding(producerID string, slot int) string {
	return fmt.Sprintf("%s:%08d", producerID, slot)
}

func exactActionPlanInputSetBindings(kind string, expected map[string]bool, bindings map[string]string) error {
	for _, name := range sortedKeys(bindings) {
		if !expected[name] {
			return fmt.Errorf("unexpected input-set %s binding %q", kind, name)
		}
		if bindings[name] == "" {
			return fmt.Errorf("input-set %s binding %q is empty", kind, name)
		}
	}
	expectedNames := make([]string, 0, len(expected))
	for name := range expected {
		expectedNames = append(expectedNames, name)
	}
	sort.Strings(expectedNames)
	for _, name := range expectedNames {
		if bindings[name] == "" {
			return fmt.Errorf("missing input-set %s binding %q", kind, name)
		}
	}
	return nil
}

func validateActionPlanInputSetArtifact(kind, name, filename string) error {
	info, err := os.Stat(filename)
	if err != nil {
		return fmt.Errorf("inspect input-set %s binding %q: %w", kind, name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input-set %s binding %q is not a regular file", kind, name)
	}
	return nil
}

func validateActionPlanInputSetTargetShapes(entries []resolvedActionPlanInputSetEntry) error {
	paths := map[string]map[string]bool{}
	for _, resolved := range entries {
		target := resolved.entry.Target
		namespace := string(target.Kind) + "\x00" + target.Tree
		if paths[namespace] == nil {
			paths[namespace] = map[string]bool{}
		}
		paths[namespace][target.Path] = true
	}
	namespaces := make([]string, 0, len(paths))
	for namespace := range paths {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	for _, namespace := range namespaces {
		namespacePaths := paths[namespace]
		orderedPaths := make([]string, 0, len(namespacePaths))
		for pathname := range namespacePaths {
			orderedPaths = append(orderedPaths, pathname)
		}
		sort.Strings(orderedPaths)
		for _, pathname := range orderedPaths {
			for parent := pathParent(pathname); parent != ""; parent = pathParent(parent) {
				if !namespacePaths[parent] {
					continue
				}
				kind, tree, _ := strings.Cut(namespace, "\x00")
				if tree != "" {
					kind += ":" + tree
				}
				return fmt.Errorf(
					"input-set %s target %q is below file target %q",
					kind, pathname, parent,
				)
			}
		}
	}
	return nil
}

func pathParent(value string) string {
	index := strings.LastIndexByte(value, '/')
	if index < 0 {
		return ""
	}
	return value[:index]
}

func validateActionPlanInputSetManifestGraph(root string, nodes map[string]kconfig.ActionPlanInputSetNode) error {
	if _, exists := nodes[root]; !exists {
		return fmt.Errorf("input-set root %q is missing", root)
	}
	state := make(map[string]uint8, len(nodes))
	var visit func(string, int) error
	visit = func(id string, depth int) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("input-set manifest graph contains a cycle at %q", id)
		case 2:
			return nil
		}
		if depth > sha256.Size*2 {
			return fmt.Errorf("input-set manifest graph exceeds %d radix levels at %q", sha256.Size*2, id)
		}
		node, exists := nodes[id]
		if !exists {
			return fmt.Errorf("input-set manifest graph references missing child %q", id)
		}
		state[id] = 1
		for _, child := range node.Children {
			if err := visit(child.ID, depth+1); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	if err := visit(root, 0); err != nil {
		return err
	}
	reachable := make(map[string]bool, len(state))
	for id := range state {
		reachable[id] = true
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == 0 {
			if err := visit(id, 0); err != nil {
				return err
			}
		}
	}
	if len(reachable) != len(nodes) {
		for _, id := range ids {
			if !reachable[id] {
				return fmt.Errorf("input-set manifest %q is not reachable from root %q", id, root)
			}
		}
	}
	return nil
}

// loadActionPlanInputSet imports exactly one persistent radix closure. The
// callback binds canonical manifest witnesses and the immutable artifacts
// named by leaf provenance; target paths and usage classifications remain
// authenticated inside those witnesses and never become caller-controlled
// flags of their own.
func loadActionPlanInputSet(opts recipeOptions) (resolvedActionPlanInputSet, error) {
	configured := opts.inputSetRoot != "" || len(opts.inputSetManifests) != 0 ||
		len(opts.inputSetSources) != 0 || len(opts.inputSetInputs) != 0 ||
		opts.inputSetManifestRoot != "" || len(opts.inputSetStoreAnchors) != 0 || len(opts.inputSetStorePacks) != 0
	if !configured {
		return resolvedActionPlanInputSet{}, nil
	}
	if !isDigest(opts.inputSetRoot) {
		return resolvedActionPlanInputSet{}, fmt.Errorf("input-set root must be a canonical SHA-256 digest")
	}
	compact := opts.inputSetManifestRoot != ""
	if compact && (len(opts.inputSetManifests) != 0 || len(opts.inputSetInputs) != 0) {
		return resolvedActionPlanInputSet{}, fmt.Errorf("compact input-set transport cannot include explicit manifests or producer bindings")
	}
	if !compact && (len(opts.inputSetStoreAnchors) != 0 || len(opts.inputSetStorePacks) != 0) {
		return resolvedActionPlanInputSet{}, fmt.Errorf("input-set store anchors and packs require compact manifest root")
	}
	if !compact && len(opts.inputSetManifests) == 0 {
		return resolvedActionPlanInputSet{}, fmt.Errorf("input-set root %q has no manifests", opts.inputSetRoot)
	}
	if len(opts.inputSetManifests) > maxActionPlanInputSetManifests {
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input set contains %d manifests, want at most %d", len(opts.inputSetManifests), maxActionPlanInputSetManifests,
		)
	}
	if len(opts.inputSetSources) > maxActionPlanInputSetEntries {
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input set contains %d source bindings, want at most %d", len(opts.inputSetSources), maxActionPlanInputSetEntries,
		)
	}
	if len(opts.inputSetInputs) > maxActionPlanInputSetEntries {
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input set contains %d producer bindings, want at most %d", len(opts.inputSetInputs), maxActionPlanInputSetEntries,
		)
	}
	if len(opts.inputSetSources)+len(opts.inputSetInputs) > maxActionPlanInputSetEntries {
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input set contains %d artifact bindings, want at most %d",
			len(opts.inputSetSources)+len(opts.inputSetInputs), maxActionPlanInputSetEntries,
		)
	}

	nodes := make(map[string]kconfig.ActionPlanInputSetNode, len(opts.inputSetManifests))
	if compact {
		var err error
		nodes, err = loadCompactActionPlanInputSetManifests(opts.inputSetRoot, opts.inputSetManifestRoot)
		if err != nil {
			return resolvedActionPlanInputSet{}, err
		}
	}
	totalBytes := 0
	for _, id := range sortedKeys(opts.inputSetManifests) {
		node, size, err := decodeActionPlanInputSetManifest(opts.inputSetManifests[id], id)
		if err != nil {
			return resolvedActionPlanInputSet{}, err
		}
		if size > maxActionPlanInputSetManifestTotalBytes-totalBytes {
			return resolvedActionPlanInputSet{}, fmt.Errorf(
				"input-set manifests exceed %d total bytes", maxActionPlanInputSetManifestTotalBytes,
			)
		}
		totalBytes += size
		nodes[id] = node
	}
	if err := validateActionPlanInputSetManifestGraph(opts.inputSetRoot, nodes); err != nil {
		return resolvedActionPlanInputSet{}, err
	}
	store, err := kconfig.NewActionPlanInputSetStoreFromNodes(nodes)
	if err != nil {
		return resolvedActionPlanInputSet{}, fmt.Errorf("validate input-set manifests: %w", err)
	}
	closure, err := store.ReachableNodes(opts.inputSetRoot)
	if err != nil {
		return resolvedActionPlanInputSet{}, fmt.Errorf("validate input-set root %q: %w", opts.inputSetRoot, err)
	}
	if len(closure) != len(nodes) {
		for _, id := range sortedKeys(opts.inputSetManifests) {
			if _, reachable := closure[id]; !reachable {
				return resolvedActionPlanInputSet{}, fmt.Errorf("input-set manifest %q is not reachable from root %q", id, opts.inputSetRoot)
			}
		}
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input-set root %q reaches %d of %d manifests", opts.inputSetRoot, len(closure), len(nodes),
		)
	}

	entries := make([]resolvedActionPlanInputSetEntry, 0, nodes[opts.inputSetRoot].Count)
	expectedSources := map[string]bool{}
	expectedInputs := map[string]bool{}
	err = store.Walk(opts.inputSetRoot, func(entry kconfig.ActionPlanInputSetEntry) error {
		if len(entries) == maxActionPlanInputSetEntries {
			return fmt.Errorf("input set contains more than %d entries", maxActionPlanInputSetEntries)
		}
		if entry.AuxiliaryUse && !entry.CompilerUse {
			return fmt.Errorf("input-set target %s/%s marks auxiliary use without compiler use", entry.Target.Kind, entry.Target.Path)
		}
		resolved := resolvedActionPlanInputSetEntry{entry: entry}
		if entry.SourceID != "" {
			expectedSources[entry.SourceID] = true
			resolved.input = opts.inputSetSources[entry.SourceID]
			resolved.provenance = entry.SourceID
		} else {
			binding := actionPlanInputSetProducerBinding(entry.ProducerID, entry.Slot)
			expectedInputs[binding] = true
			resolved.input = opts.inputSetInputs[binding]
			resolved.provenance = binding
		}
		entries = append(entries, resolved)
		return nil
	})
	if err != nil {
		return resolvedActionPlanInputSet{}, fmt.Errorf("walk input-set root %q: %w", opts.inputSetRoot, err)
	}
	if len(entries) != nodes[opts.inputSetRoot].Count {
		return resolvedActionPlanInputSet{}, fmt.Errorf(
			"input-set root %q yielded %d entries, want %d", opts.inputSetRoot, len(entries), nodes[opts.inputSetRoot].Count,
		)
	}
	if compact {
		opts.inputSetInputs, err = resolveCompactActionPlanInputSetProducers(expectedInputs, opts.inputSetStoreAnchors, opts.inputSetStorePacks)
		if err != nil {
			return resolvedActionPlanInputSet{}, err
		}
		if len(expectedSources)+len(opts.inputSetInputs) > maxActionPlanInputSetEntries {
			return resolvedActionPlanInputSet{}, fmt.Errorf("compact input set contains too many artifact bindings")
		}
		for index := range entries {
			if entries[index].entry.ProducerID != "" {
				entries[index].input = opts.inputSetInputs[entries[index].provenance]
			}
		}
	}
	if err := exactActionPlanInputSetBindings("source", expectedSources, opts.inputSetSources); err != nil {
		return resolvedActionPlanInputSet{}, err
	}
	if err := exactActionPlanInputSetBindings("input", expectedInputs, opts.inputSetInputs); err != nil {
		return resolvedActionPlanInputSet{}, err
	}
	for _, name := range sortedKeys(opts.inputSetSources) {
		if err := validateActionPlanInputSetArtifact("source", name, opts.inputSetSources[name]); err != nil {
			return resolvedActionPlanInputSet{}, err
		}
	}
	for _, name := range sortedKeys(opts.inputSetInputs) {
		if err := validateActionPlanInputSetArtifact("input", name, opts.inputSetInputs[name]); err != nil {
			return resolvedActionPlanInputSet{}, err
		}
	}
	if err := validateActionPlanInputSetTargetShapes(entries); err != nil {
		return resolvedActionPlanInputSet{}, err
	}
	return resolvedActionPlanInputSet{root: opts.inputSetRoot, entries: entries}, nil
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

func preparePrivateInputTreeProjections(recipeInputs, recipeSources []string, opts *recipeOptions) (func(), error) {
	return preparePrivateInputTreeProjectionsWithInputSet(recipeInputs, recipeSources, resolvedActionPlanInputSet{}, opts)
}

func sameRecipeInput(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func validateActionPlanInputSetTargets(recipe kconfig.ActionRecipe, inputSet resolvedActionPlanInputSet, opts recipeOptions) error {
	for _, resolved := range inputSet.entries {
		target := resolved.entry.Target
		switch target.Kind {
		case kconfig.ActionPlanInputSetWorkTarget:
			if recipe.WorkingDirectory == "" || opts.workingDirectory == "" {
				return fmt.Errorf("input-set work target %q requires a private recipe working directory", target.Path)
			}
		case kconfig.ActionPlanInputSetTreeTarget:
			if opts.trees[target.Tree] == "" {
				return fmt.Errorf("input-set tree target %s/%s has no declared tree binding", target.Tree, target.Path)
			}
			if !opts.privateInputTrees[target.Tree] {
				return fmt.Errorf("input-set tree target %s/%s requires private input tree %q", target.Tree, target.Path, target.Tree)
			}
		case kconfig.ActionPlanInputSetAmbientTarget:
			// Ambient entries are already materialized by Bazel at their bound
			// artifact paths. They grant dependency authority without asking the
			// runner to mutate an execroot-relative path.
		default:
			return fmt.Errorf("input-set target %q has unsupported kind %q", target.Path, target.Kind)
		}
	}
	return nil
}

func preparePrivateInputTreeProjectionsWithInputSet(
	recipeInputs, recipeSources []string,
	inputSet resolvedActionPlanInputSet,
	opts *recipeOptions,
) (func(), error) {
	cleanup := func() {}
	inputManifestMode := opts.inputBindings != "" || opts.expectedInputBindingsID != ""
	sourceManifestMode := opts.sourceProjections != "" || opts.expectedSourceProjectionsID != ""
	inputSetTreeMode := false
	for _, resolved := range inputSet.entries {
		if resolved.entry.Target.Kind == kconfig.ActionPlanInputSetTreeTarget {
			inputSetTreeMode = true
			break
		}
	}
	if len(opts.privateInputTrees) == 0 {
		if sourceManifestMode {
			return cleanup, fmt.Errorf("source projections require a private input tree")
		}
		if inputSetTreeMode {
			return cleanup, fmt.Errorf("input-set tree targets require a private input tree")
		}
		return cleanup, nil
	}
	if !inputManifestMode && !sourceManifestMode && !inputSetTreeMode {
		return cleanup, fmt.Errorf("private input trees require an input bindings, source projections, or input-set manifest")
	}
	inputManifest := kconfig.ActionPlanInputBindings{
		Schema: kconfig.LinuxKernelInputBindingsSchema, Bindings: map[string]kconfig.ActionPlanInputBinding{},
	}
	if inputManifestMode {
		if opts.inputBindings == "" || opts.expectedInputBindingsID == "" {
			return cleanup, fmt.Errorf("private input-tree input bindings require both manifest and expected ID")
		}
		var err error
		inputManifest, err = decodeInputBindings(opts.inputBindings, opts.expectedInputBindingsID)
		if err != nil {
			return cleanup, err
		}
	}
	sourceManifest := kconfig.ActionPlanSourceProjections{
		Schema: kconfig.LinuxKernelSourceProjectionsSchema, Bindings: map[string]kconfig.ActionPlanSourceProjectionBinding{},
	}
	if sourceManifestMode {
		if opts.sourceProjections == "" || opts.expectedSourceProjectionsID == "" {
			return cleanup, fmt.Errorf("private input-tree source projections require both manifest and expected ID")
		}
		var err error
		sourceManifest, err = decodeSourceProjections(opts.sourceProjections, opts.expectedSourceProjectionsID)
		if err != nil {
			return cleanup, err
		}
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
	for bindingName, binding := range sourceManifest.Bindings {
		if !opts.privateInputTrees[binding.Tree] {
			return cleanup, fmt.Errorf("source projection %q targets non-private tree %q", bindingName, binding.Tree)
		}
	}
	for _, resolved := range inputSet.entries {
		target := resolved.entry.Target
		if target.Kind == kconfig.ActionPlanInputSetTreeTarget && !opts.privateInputTrees[target.Tree] {
			return cleanup, fmt.Errorf("input-set tree target %s/%s targets non-private tree", target.Tree, target.Path)
		}
	}
	recipeSourceBindings := make(map[string]bool, len(recipeSources))
	for _, binding := range recipeSources {
		recipeSourceBindings[binding] = true
	}
	for bindingName := range sourceManifest.Bindings {
		if !recipeSourceBindings[bindingName] {
			return cleanup, fmt.Errorf("source projection %q is not a declared recipe source", bindingName)
		}
		if opts.sources[bindingName] == "" {
			return cleanup, fmt.Errorf("source projection %q has no resolved source", bindingName)
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
		project := func(projectionPath, source, description string) error {
			destination := filepath.Join(projection, filepath.FromSlash(projectionPath))
			contained, err := recipeTreeContains(projection, destination)
			if err != nil || !contained {
				return fmt.Errorf("private input tree %q path %q escapes its projection", name, projectionPath)
			}
			if prior := materialized[projectionPath]; prior != "" {
				if !sameRecipeInput(prior, source) {
					return fmt.Errorf("private input tree %q path %q has conflicting inputs", name, projectionPath)
				}
				return nil
			}
			for parent := pathParent(projectionPath); parent != ""; parent = pathParent(parent) {
				if materialized[parent] != "" {
					return fmt.Errorf("private input tree %q path %q is below projected file %q", name, projectionPath, parent)
				}
			}
			paths := sortedKeys(materialized)
			prefix := projectionPath + "/"
			if index := sort.SearchStrings(paths, prefix); index < len(paths) && strings.HasPrefix(paths[index], prefix) {
				return fmt.Errorf("private input tree %q projected file %q is below path %q", name, paths[index], projectionPath)
			}
			if err := copyRecipeFile(source, destination); err != nil {
				return fmt.Errorf("project private input tree %q %s at %q: %w", name, description, projectionPath, err)
			}
			materialized[projectionPath] = source
			return nil
		}
		for _, bindingName := range recipeInputs {
			binding, exists := inputManifest.Bindings[bindingName]
			projectionTree, projectionPath := binding.Tree, binding.Path
			if binding.ProjectionTree != "" {
				projectionTree, projectionPath = binding.ProjectionTree, binding.ProjectionPath
			}
			if !exists || projectionTree != name {
				continue
			}
			source := opts.inputs[bindingName]
			if source == "" {
				cleanup()
				return func() {}, fmt.Errorf("private input tree %q has no resolved input %q", name, bindingName)
			}
			if err := project(projectionPath, source, "generated input "+bindingName); err != nil {
				cleanup()
				return func() {}, err
			}
		}
		for _, bindingName := range sortedKeys(opts.sources) {
			binding, exists := sourceManifest.Bindings[bindingName]
			if !exists || binding.Tree != name {
				continue
			}
			if err := project(binding.Path, opts.sources[bindingName], "immutable source "+bindingName); err != nil {
				cleanup()
				return func() {}, err
			}
		}
		for _, resolved := range inputSet.entries {
			target := resolved.entry.Target
			if target.Kind != kconfig.ActionPlanInputSetTreeTarget || target.Tree != name {
				continue
			}
			if err := project(target.Path, resolved.input, "input-set provenance "+resolved.provenance); err != nil {
				cleanup()
				return func() {}, err
			}
		}
		opts.trees[name] = projection
	}
	return cleanup, nil
}

func inputSetWorkEntries(inputSet resolvedActionPlanInputSet) []resolvedActionPlanInputSetEntry {
	entries := []resolvedActionPlanInputSetEntry{}
	for _, resolved := range inputSet.entries {
		if resolved.entry.Target.Kind == kconfig.ActionPlanInputSetWorkTarget {
			entries = append(entries, resolved)
		}
	}
	return entries
}

func validateInputSetWorkCollisions(
	entries []resolvedActionPlanInputSetEntry,
	workingInputs map[string]string,
	bindings map[string]map[string]string,
) (map[string]bool, error) {
	setByPath := make(map[string]resolvedActionPlanInputSetEntry, len(entries))
	setPaths := make([]string, 0, len(entries))
	for _, resolved := range entries {
		pathname := resolved.entry.Target.Path
		setByPath[pathname] = resolved
		setPaths = append(setPaths, pathname)
	}
	sort.Strings(setPaths)
	duplicates := map[string]bool{}
	for _, reference := range sortedKeys(workingInputs) {
		kind, name, ok := strings.Cut(reference, ":")
		if !ok || bindings[kind] == nil {
			return nil, fmt.Errorf("working input %q has no typed binding", reference)
		}
		source := bindings[kind][name]
		pathname := workingInputs[reference]
		if existing, found := setByPath[pathname]; found {
			if !sameRecipeInput(existing.input, source) {
				return nil, fmt.Errorf(
					"input-set work target %q from %s conflicts with working input %s",
					pathname, existing.provenance, reference,
				)
			}
			duplicates[pathname] = true
		}
		for parent := pathParent(pathname); parent != ""; parent = pathParent(parent) {
			if existing, found := setByPath[parent]; found {
				return nil, fmt.Errorf(
					"working input %s at %q is below input-set file target %q from %s",
					reference, pathname, parent, existing.provenance,
				)
			}
		}
		prefix := pathname + "/"
		index := sort.SearchStrings(setPaths, prefix)
		if index < len(setPaths) && strings.HasPrefix(setPaths[index], prefix) {
			existing := setByPath[setPaths[index]]
			return nil, fmt.Errorf(
				"input-set work target %q from %s is below working input %s at %q",
				setPaths[index], existing.provenance, reference, pathname,
			)
		}
	}
	return duplicates, nil
}

func materializeActionPlanInputSetWork(
	inputSet resolvedActionPlanInputSet,
	workingRoot string,
	workingInputs map[string]string,
	bindings map[string]map[string]string,
) error {
	entries := inputSetWorkEntries(inputSet)
	if len(entries) == 0 {
		return nil
	}
	if workingRoot == "" {
		return fmt.Errorf("input-set work targets require an expanded private working root")
	}
	duplicates, err := validateInputSetWorkCollisions(entries, workingInputs, bindings)
	if err != nil {
		return err
	}
	for _, resolved := range entries {
		pathname := resolved.entry.Target.Path
		if duplicates[pathname] {
			// The later direct WorkingInputs pass owns the identical projection.
			continue
		}
		destination := filepath.Join(workingRoot, filepath.FromSlash(pathname))
		contained, err := recipeTreeContains(workingRoot, destination)
		if err != nil {
			return fmt.Errorf("resolve input-set work target %q: %w", pathname, err)
		}
		if !contained {
			return fmt.Errorf("input-set work target %q escapes its private working root", pathname)
		}
		if err := validateRecipeOutputAncestors(workingRoot, destination); err != nil {
			return fmt.Errorf("materialize input-set work target %q: %w", pathname, err)
		}
		if info, err := os.Lstat(destination); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("input-set work target %q collides with a non-regular working path", pathname)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect input-set work target %q: %w", pathname, err)
		}
		if sameRecipeInput(resolved.input, destination) {
			continue
		}
		if err := copyRecipeFile(resolved.input, destination); err != nil {
			return fmt.Errorf("materialize input-set work target %q from %s: %w", pathname, resolved.provenance, err)
		}
	}
	return nil
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
	var toolsetPaths *toolsetpath.Resolver
	if len(opts.toolsetIdentities) != 0 || len(opts.toolsetManifests) != 0 || len(opts.toolsetAnchors) != 0 {
		projectionRoot, err := os.MkdirTemp("", "linux-bzl-recipe-toolsets-")
		if err != nil {
			return fmt.Errorf("create recipe toolset projection root: %w", err)
		}
		defer os.RemoveAll(projectionRoot)
		toolsetPaths, err = toolsetpath.LoadFlags(
			executionRoot,
			projectionRoot,
			opts.toolsetIdentities,
			opts.toolsetManifests,
			opts.toolsetAnchors,
		)
		if err != nil {
			return fmt.Errorf("load recipe toolset bindings: %w", err)
		}
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
	inputSet, err := loadActionPlanInputSet(opts)
	if err != nil {
		return err
	}
	if err := validateActionPlanInputSetTargets(recipe, inputSet, opts); err != nil {
		return err
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
	privateTreeCleanup, err := preparePrivateInputTreeProjectionsWithInputSet(recipe.Inputs, recipe.Sources, inputSet, &opts)
	if err != nil {
		return err
	}
	cleanups = append(cleanups, privateTreeCleanup)
	runtimeToolDirectory := ""
	if len(opts.runtimeTools) != 0 {
		var cleanup func()
		runtimeToolDirectory, cleanup, err = prepareRuntimeToolDirectory(opts.workingDirectory, opts.runtimeTools)
		if err != nil {
			return err
		}
		cleanups = append(cleanups, cleanup)
	}
	toolBindings := opts.tools
	// scriptrun consumes the forwarded contracts and installs its own private
	// proxies for source-script commands. Every other recipe receives proxies
	// here so a direct primary tool (notably rustc) cannot bypass the configured
	// action envelope when it invokes an explicit ${tool:...} binding.
	if recipe.Tool != "scriptrun" && len(recipe.AuxiliaryTools) != 0 {
		var cleanup func()
		toolBindings, cleanup, err = prepareDirectAuxiliaryToolBindings(
			opts.workingDirectory,
			opts.runtimeTools["script-runtime"],
			recipe.AuxiliaryTools,
			opts.tools,
			opts.auxiliaryActionContracts,
		)
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
		"tool": toolBindings, "tree": opts.trees, "work": {},
	}
	contentBindings, err := materializeRecipeContentSubstitutions(recipe.ContentSubstitutions, bindings)
	if err != nil {
		return err
	}
	bindings["content"] = contentBindings
	expand := func(value string) (string, error) {
		value, err := toolsetpath.Rewrite(value, toolsetPaths)
		if err != nil {
			return "", err
		}
		return expandValue(value, bindings)
	}
	expandLiteral := func(value string) (string, error) {
		return expandValueWithLiteralActionMarkersAndToolsets(value, bindings, toolsetPaths)
	}
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
		// Compare set/direct collisions against the declared artifact paths, not
		// inputBindings: executable inputs may already have been copied to a
		// private chmod-capable location by this point.
		declaredInputBindings := map[string]map[string]string{
			"source": opts.sources,
			"input":  opts.inputs,
		}
		if err := materializeActionPlanInputSetWork(inputSet, workingRoot, recipe.WorkingInputs, declaredInputBindings); err != nil {
			return err
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
	replayArgs, err := encodeRecipeCommandReplays(recipe.CommandReplays, expandLiteral)
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
		args, err = toolaction.SpliceArguments(opts.actionArgs, linuxArgs)
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
	recipeEnvironment, err := restoreRecipeEnvironmentNames(recipe.Environment)
	if err != nil {
		return err
	}
	for _, name := range []string{
		toolaction.EnvironmentName,
		toolaction.RuntimeToolPathEnvironmentName,
		toolsetpath.HandoffEnvironmentName,
	} {
		if _, exists := opts.actionEnvironment[name]; exists {
			return fmt.Errorf("configured action environment uses reserved variable %s", name)
		}
		if _, exists := recipeEnvironment[name]; exists {
			return fmt.Errorf("recipe environment uses reserved variable %s", name)
		}
	}
	if len(recipeEnvironment) != 0 || len(opts.auxiliaryActionContracts) != 0 || runtimeToolDirectory != "" || recipe.Tool == "scriptrun" {
		environment := environmentMap(command.Env)
		for _, key := range sortedKeys(recipeEnvironment) {
			value, err := expandLiteral(recipeEnvironment[key])
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
		if recipe.Tool == "scriptrun" && toolsetPaths != nil {
			handoff, err := toolsetPaths.CreateHandoff("")
			if err != nil {
				return fmt.Errorf("create scriptrun toolset handoff: %w", err)
			}
			cleanups = append(cleanups, func() { _ = os.Remove(handoff) })
			environment[toolsetpath.HandoffEnvironmentName] = handoff
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
	if opts.inputSetManifestRoot != "" {
		paths["input-set manifest root"] = &opts.inputSetManifestRoot
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
		"input-set manifest": opts.inputSetManifests, "input-set source": opts.inputSetSources,
		"input-set input":        opts.inputSetInputs,
		"input-set store anchor": opts.inputSetStoreAnchors,
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

func prepareDirectAuxiliaryToolBindings(
	privateRoot, multicall string,
	roles []string,
	tools map[string]string,
	contracts map[string]toolaction.Contract,
) (map[string]string, func(), error) {
	bindings := make(map[string]string, len(tools))
	for role, executable := range tools {
		bindings[role] = executable
	}
	noop := func() {}
	if len(roles) == 0 {
		return bindings, noop, nil
	}
	if privateRoot == "" || !filepath.IsAbs(privateRoot) {
		return nil, noop, fmt.Errorf("direct auxiliary tools require an absolute private working-directory root")
	}
	if multicall == "" {
		return nil, noop, fmt.Errorf("direct auxiliary tools require the configured script-runtime multicall")
	}
	proxyDirectory := filepath.Join(privateRoot, ".linux-bzl-action-tools")
	if err := os.Mkdir(proxyDirectory, 0o700); err != nil {
		return nil, noop, fmt.Errorf("create private action-tool directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(proxyDirectory) }
	orderedRoles := append([]string(nil), roles...)
	sort.Strings(orderedRoles)
	for _, role := range orderedRoles {
		executable := tools[role]
		if executable == "" {
			cleanup()
			return nil, noop, fmt.Errorf("direct auxiliary tool %q has no executable binding", role)
		}
		contract, exists := contracts[role]
		if !exists {
			cleanup()
			return nil, noop, fmt.Errorf("direct auxiliary tool %q has no configured action contract", role)
		}
		var linkContract *toolaction.Contract
		if linkRole, compilerDriver := toolaction.LinkContractRole(role); compilerDriver {
			if companion, exists := contracts[linkRole]; exists {
				linkContract = &companion
			}
		}
		proxy, err := toolaction.InstallToolActionProxy(
			proxyDirectory,
			multicall,
			role,
			executable,
			contract,
			linkContract,
		)
		if err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("install direct auxiliary tool %s: %w", role, err)
		}
		bindings[role] = proxy
	}
	return bindings, cleanup, nil
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

func expandValue(value string, bindings map[string]map[string]string) (string, error) {
	return expandValueKinds(value, bindings, nil)
}

// expandValueWithLiteralActionMarkers expands typed placeholders and decodes
// protected source literals in one pass. Template literals are decoded as they
// are copied, after the scanner has passed them, so a restored ${tree:...}
// spelling cannot become a capability. Placeholder replacements are copied as
// opaque data and are never inspected for private escape bytes.
func expandValueWithLiteralActionMarkers(value string, bindings map[string]map[string]string) (string, error) {
	return expandValueWithLiteralActionMarkersAndToolsets(value, bindings, nil)
}

func expandValueWithLiteralActionMarkersAndToolsets(
	value string,
	bindings map[string]map[string]string,
	toolsetPaths *toolsetpath.Resolver,
) (string, error) {
	var out strings.Builder
	writeLiteral := func(literal string) error {
		var err error
		literal, err = toolsetpath.Rewrite(literal, toolsetPaths)
		if err != nil {
			return err
		}
		restored, err := kconfig.RestoreCompactKbuildLiteralActionMarkers(literal)
		if err != nil {
			return err
		}
		out.WriteString(restored)
		return nil
	}
	for cursor := 0; ; {
		relativeStart := strings.Index(value[cursor:], "${")
		if relativeStart < 0 {
			if err := writeLiteral(value[cursor:]); err != nil {
				return "", err
			}
			return out.String(), nil
		}
		start := cursor + relativeStart
		if err := writeLiteral(value[cursor:start]); err != nil {
			return "", err
		}
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
		out.WriteString(replacement)
		cursor = end + 1
	}
}

func restoreRecipeEnvironmentNames(environment map[string]string) (map[string]string, error) {
	restored := make(map[string]string, len(environment))
	for _, key := range sortedKeys(environment) {
		name, err := kconfig.RestoreCompactKbuildLiteralActionMarkers(key)
		if err != nil {
			return nil, fmt.Errorf("environment name %q literal marker: %w", key, err)
		}
		if _, exists := restored[name]; exists {
			return nil, fmt.Errorf("environment name %q restores to duplicate %q", key, name)
		}
		restored[name] = environment[key]
	}
	return restored, nil
}

// expandContentTemplate substitutes generated content while retaining typed
// placeholders for the downstream consumer. Only tree markers which were
// complete placeholders in the serialized template may reach scriptrun: bytes
// read from one or several generated files must not be able to manufacture a
// new marker by themselves or together with adjacent template bytes.
func expandContentTemplate(value string, bindings map[string]map[string]string) (string, error) {
	var out strings.Builder
	allowedTrees := map[int]string{}
	allowedToolsetPaths := map[int]string{}
	writeLiteral := func(literal string) error {
		tokens, err := indexedToolsetPathTokens(literal)
		if err != nil {
			return fmt.Errorf("template literal contains split or invalid toolset-path provenance: %w", err)
		}
		base := out.Len()
		for offset, token := range tokens {
			allowedToolsetPaths[base+offset] = token
		}
		out.WriteString(literal)
		return nil
	}
	for cursor := 0; ; {
		relativeStart := strings.Index(value[cursor:], "${")
		if relativeStart < 0 {
			if err := writeLiteral(value[cursor:]); err != nil {
				return "", err
			}
			break
		}
		start := cursor + relativeStart
		if err := writeLiteral(value[cursor:start]); err != nil {
			return "", err
		}
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
	expandedToolsetPaths, err := indexedToolsetPathTokens(expanded)
	if err != nil {
		return "", fmt.Errorf("generated content altered reserved toolset-path provenance: %w", err)
	}
	for offset, token := range expandedToolsetPaths {
		if allowedToolsetPaths[offset] != token {
			return "", fmt.Errorf("generated content created or modified reserved toolset-path provenance at byte %d", offset)
		}
	}
	for offset, token := range allowedToolsetPaths {
		if expandedToolsetPaths[offset] != token {
			return "", fmt.Errorf("generated content removed or modified reserved toolset-path provenance at byte %d", offset)
		}
	}
	return expanded, nil
}

// indexedToolsetPathTokens validates every private provenance delimiter and
// returns each complete canonical token by its byte offset. Content templates
// treat those tokens as indivisible literals: generated substitutions may move
// later tokens only by changing the length of preceding content, never split or
// rewrite a token which the planner supplied.
func indexedToolsetPathTokens(value string) (map[int]string, error) {
	if err := toolaction.ValidateExecutionRootProvenanceValue(value); err != nil {
		return nil, err
	}
	tokens := map[int]string{}
	for cursor := 0; ; {
		relativeStart := strings.Index(value[cursor:], toolaction.ExecutionRootProvenanceMarker)
		if relativeStart < 0 {
			return tokens, nil
		}
		start := cursor + relativeStart
		payloadStart := start + len(toolaction.ExecutionRootProvenanceMarker)
		relativeEnd := strings.Index(value[payloadStart:], toolaction.ExecutionRootProvenanceTerminator)
		if relativeEnd < 0 {
			return nil, fmt.Errorf("toolset-path token at byte %d is unterminated", start)
		}
		end := payloadStart + relativeEnd + len(toolaction.ExecutionRootProvenanceTerminator)
		tokens[start] = value[start:end]
		cursor = end
	}
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
	arguments, err := expandArgumentChunks(os.Args[1:])
	if err == nil && len(arguments) > 0 && arguments[0] == "-write_argument_chunk" {
		if len(arguments) < 3 || arguments[2] != "--" {
			err = fmt.Errorf("-write_argument_chunk requires OUTPUT -- ARGUMENTS...")
		} else {
			err = writeArgumentChunk(arguments[1], arguments[3:])
		}
		if err == nil {
			return
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
		os.Exit(2)
	}
	var sourceFlags, inputFlags, outputFlags, toolFlags, runtimeToolFlags, treeFlags, artifactTreeFlags, privateInputTreeFlags, actionArgs, actionEnvironment repeatedFlag
	var inputSetManifestFlags, inputSetSourceFlags, inputSetInputFlags repeatedFlag
	var inputSetStoreAnchorFlags, inputSetStorePackFlags compactInputSetFlags
	var auxiliaryActionRoles, auxiliaryActionArguments, auxiliaryActionEnvironment repeatedFlag
	var toolsetIdentities, toolsetManifests, toolsetAnchors repeatedFlag
	recipe := flag.String("recipe", "", "v4 action recipe JSON")
	kind := flag.String("kind", "", "node kind encoded by the plan")
	expectedNodeID := flag.String("expected_node_id", "", "content-addressed node ID")
	expectedRecipeID := flag.String("expected_recipe_id", "", "content-addressed recipe ID")
	inputBindings := flag.String("input_bindings", "", "canonical per-node input bindings manifest")
	expectedInputBindingsID := flag.String("expected_input_bindings_id", "", "content ID of the input bindings manifest")
	sourceProjections := flag.String("source_projections", "", "canonical per-node immutable source projections manifest")
	expectedSourceProjectionsID := flag.String("expected_source_projections_id", "", "content ID of the source projections manifest")
	var inputSetRoot singleFlag
	flag.Var(&inputSetRoot, "input_set_root", "content ID of the node's persistent input-set root")
	var inputSetManifestRoot singleFlag
	flag.Var(&inputSetManifestRoot, "input_set_manifest_root", "typed root manifest for compact family input-set transport")
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
	flag.Var(&inputSetManifestFlags, "input_set_manifest", "persistent input-set node binding ID=PATH (repeatable)")
	flag.Var(&inputSetSourceFlags, "input_set_source", "persistent input-set source binding SOURCE_ID=PATH (repeatable)")
	flag.Var(&inputSetInputFlags, "input_set_input", "persistent input-set producer binding PRODUCER_ID:SLOT=PATH (repeatable)")
	flag.Var(&inputSetStoreAnchorFlags, "input_set_store_anchor", "compact producer store anchor INDEX:PRODUCER_ID:SLOT=PATH (repeatable)")
	flag.Var(&inputSetStorePackFlags, "input_set_store_pack", "compact producer store indices START:INDEX.INDEX... (repeatable)")
	flag.Var(&actionArgs, "action_arg", "configured tool action argument (repeatable)")
	flag.Var(&actionEnvironment, "action_env", "configured tool action environment NAME=VALUE (repeatable)")
	flag.Var(&auxiliaryActionRoles, "auxiliary_action_role", "auxiliary configured action role (repeatable)")
	flag.Var(&auxiliaryActionArguments, "auxiliary_action_arg", "auxiliary configured action argument ROLE=VALUE (repeatable)")
	flag.Var(&auxiliaryActionEnvironment, "auxiliary_action_env", "auxiliary configured action environment ROLE=NAME=VALUE (repeatable)")
	flag.Var(&toolsetIdentities, "toolset_identity", "identity-bound toolset scope SCOPE=SHA256 (repeatable)")
	flag.Var(&toolsetManifests, "toolset_manifest", "identity-bound toolset manifest SCOPE=PATH (repeatable)")
	flag.Var(&toolsetAnchors, "toolset_anchor", "typed toolset root anchor SCOPE=ROOT=PATH (repeatable)")
	_ = flag.CommandLine.Parse(arguments)
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
	inputSetManifests, err := namedBindings(inputSetManifestFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: input-set manifest bindings: %v\n", err)
		os.Exit(2)
	}
	inputSetSources, err := namedBindings(inputSetSourceFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: input-set source bindings: %v\n", err)
		os.Exit(2)
	}
	inputSetInputs, err := namedBindings(inputSetInputFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: input-set input bindings: %v\n", err)
		os.Exit(2)
	}
	inputSetStoreAnchors, err := namedBindings(inputSetStoreAnchorFlags.values)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: input-set store anchors: %v\n", err)
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
	opts := recipeOptions{recipe: *recipe, kind: *kind, expectedNodeID: *expectedNodeID, expectedRecipeID: *expectedRecipeID, inputBindings: *inputBindings, expectedInputBindingsID: *expectedInputBindingsID, sourceProjections: *sourceProjections, expectedSourceProjectionsID: *expectedSourceProjectionsID, inputSetRoot: inputSetRoot.value, toolRole: *toolRole, workingDirectory: workingDirectory, workingDirectoryMarker: *workingDirectoryMarker, actionArgs: actionArgs,
		actionEnvironment: actionEnv, auxiliaryActionContracts: auxiliaryContracts, sources: *bindings[0].out, inputs: *bindings[1].out, outputs: *bindings[2].out, tools: *bindings[3].out, runtimeTools: runtimeTools, trees: *bindings[4].out, artifactTrees: artifactTrees, privateInputTrees: privateInputTrees,
		inputSetManifests: inputSetManifests, inputSetSources: inputSetSources, inputSetInputs: inputSetInputs,
		inputSetManifestRoot: inputSetManifestRoot.value, inputSetStoreAnchors: inputSetStoreAnchors, inputSetStorePacks: inputSetStorePackFlags.values,
		toolsetIdentities: toolsetIdentities, toolsetManifests: toolsetManifests, toolsetAnchors: toolsetAnchors}
	if err := runRecipe(opts); err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
		os.Exit(1)
	}
}
