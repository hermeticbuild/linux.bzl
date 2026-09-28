package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

const maxCompactInputSetPackEntries = 256

// Compact flags are bounded even when invoked directly, without a parameter
// file. Decoding below applies the same limits to programmatic callers.
type compactInputSetFlags struct {
	values []string
	bytes  int
}

func (f *compactInputSetFlags) String() string { return strings.Join(f.values, " ") }
func (f *compactInputSetFlags) Set(value string) error {
	if len(f.values) >= maxParameterFileArguments || len(value) > maxParameterFileLineBytes || len(value) > maxParameterFileBytes-f.bytes || strings.ContainsRune(value, 0) {
		return fmt.Errorf("compact input-set flag exceeds transport limits")
	}
	f.values = append(f.values, value)
	f.bytes += len(value)
	return nil
}

// compactInputSetPhysicalRoot removes only an exact authenticated layout
// suffix from a typed artifact path. It knows nothing about Bazel configuration
// names or path mapping. In particular, staging targets never locate artifacts.
func compactInputSetPhysicalRoot(filename, suffix string) (string, error) {
	if filename == "" || strings.ContainsAny(filename, "\x00\\") || filepath.ToSlash(filepath.Clean(filename)) != filename {
		return "", fmt.Errorf("noncanonical compact input-set artifact path %q", filename)
	}
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("resolve compact input-set artifact: %w", err)
	}
	absolute = filepath.ToSlash(absolute)
	if !strings.HasSuffix(absolute, "/"+suffix) {
		return "", fmt.Errorf("compact input-set artifact %q does not end with %q", filename, suffix)
	}
	root := strings.TrimSuffix(absolute, suffix)
	return filepath.Clean(filepath.FromSlash(root)), nil
}

func compactInputSetManifestSuffix(id string) string {
	return "input-sets/" + id + "/manifest/" + id + ".json"
}

// Discover only authenticated child IDs, retaining the ordinary decoder,
// graph validation and immutable-store import as independent checks. No scan
// of a directory can add an unreferenced witness to this closure.
func loadCompactActionPlanInputSetManifests(rootID, filename string) (map[string]kconfig.ActionPlanInputSetNode, error) {
	if !isDigest(rootID) || len(filename) > maxParameterFileLineBytes {
		return nil, fmt.Errorf("invalid compact input-set root")
	}
	physicalRoot, err := compactInputSetPhysicalRoot(filename, compactInputSetManifestSuffix(rootID))
	if err != nil {
		return nil, err
	}
	nodes := map[string]kconfig.ActionPlanInputSetNode{}
	totalBytes := 0
	var visit func(string, int) error
	visit = func(id string, depth int) error {
		if !isDigest(id) || depth > sha256.Size*2 {
			return fmt.Errorf("invalid compact input-set child or radix depth")
		}
		if _, exists := nodes[id]; exists {
			return nil // The ordinary graph validator separately rejects cycles.
		}
		if len(nodes) == maxActionPlanInputSetManifests {
			return fmt.Errorf("compact input-set manifest count exceeds %d", maxActionPlanInputSetManifests)
		}
		path := filepath.Join(physicalRoot, filepath.FromSlash(compactInputSetManifestSuffix(id)))
		node, size, err := decodeActionPlanInputSetManifest(path, id)
		if err != nil {
			return err
		}
		if size > maxActionPlanInputSetManifestTotalBytes-totalBytes {
			return fmt.Errorf("compact input-set manifests exceed %d total bytes", maxActionPlanInputSetManifestTotalBytes)
		}
		if len(node.Children) > 16 || len(node.Entries) > 16 {
			return fmt.Errorf("compact input-set witness exceeds radix fanout")
		}
		totalBytes += size
		nodes[id] = node
		for _, child := range node.Children {
			if err := visit(child.ID, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(rootID, 0); err != nil {
		return nil, err
	}
	return nodes, nil
}

func compactInputSetDecimal(value string, width int) (int, error) {
	if value == "" || (width != 0 && len(value) != width) ||
		(width == 0 && len(value) > 1 && value[0] == '0') || len(value) > 8 {
		return 0, fmt.Errorf("noncanonical compact input-set decimal %q", value)
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("noncanonical compact input-set decimal %q", value)
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n > maxActionPlanInputSetEntries {
		return 0, fmt.Errorf("compact input-set decimal exceeds limit")
	}
	return n, nil
}

// The callback supplies store assignment, as it previously supplied each
// producer's complete physical path. The hashed closure owns the sorted key
// vector; neither caller-controlled order nor staging aliases may redefine it.
// The callback must retain the identical complete artifact depset in both modes.
func resolveCompactActionPlanInputSetProducers(expected map[string]bool, anchors map[string]string, packs []string) (map[string]string, error) {
	if len(expected) > maxActionPlanInputSetEntries || len(anchors) > maxParameterFileArguments ||
		len(packs) > maxParameterFileArguments || len(anchors) > len(expected) {
		return nil, fmt.Errorf("compact input-set producer transport exceeds count limit")
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type anchor struct{ root, key string }
	roots := make([]anchor, len(anchors))
	seenKeys := map[string]bool{}
	totalBytes := 0
	for name, filename := range anchors {
		if len(name)+len(filename) > maxParameterFileLineBytes || strings.ContainsRune(filename, 0) {
			return nil, fmt.Errorf("compact input-set store anchor exceeds transport limit")
		}
		totalBytes += len(name) + len(filename)
		if totalBytes > maxParameterFileBytes {
			return nil, fmt.Errorf("compact input-set transport exceeds total byte limit")
		}
		indexText, key, ok := strings.Cut(name, ":")
		index, err := compactInputSetDecimal(indexText, 0)
		if !ok || err != nil || index >= len(roots) || roots[index].root != "" {
			return nil, fmt.Errorf("invalid or repeated compact input-set store index %q", indexText)
		}
		producer, slot, ok := strings.Cut(key, ":")
		if !ok || !isDigest(producer) || len(slot) != 8 || !expected[key] || seenKeys[key] {
			return nil, fmt.Errorf("compact input-set anchor is not a unique producer in the authenticated closure: %q", key)
		}
		for _, digit := range slot {
			if digit < '0' || digit > '9' {
				return nil, fmt.Errorf("invalid compact input-set anchor slot")
			}
		}
		root, err := compactInputSetPhysicalRoot(filename, "nodes/"+producer+"/"+slot)
		if err != nil {
			return nil, err
		}
		// Distinct original configured roots can legitimately converge after
		// Bazel path mapping when their selected leaf paths do not collide.
		seenKeys[key] = true
		roots[index] = anchor{root: root, key: key}
	}
	bindings := make(map[string]string, len(keys))
	assignedIndices := make(map[string]int, len(anchors))
	usedRoots := make([]bool, len(roots))
	for _, pack := range packs {
		if len(pack) > 9+maxCompactInputSetPackEntries*9 {
			return nil, fmt.Errorf("compact input-set store pack exceeds transport limit")
		}
		totalBytes += len(pack)
		if totalBytes > maxParameterFileBytes {
			return nil, fmt.Errorf("compact input-set transport exceeds total byte limit")
		}
		startText, body, ok := strings.Cut(pack, ":")
		start, err := compactInputSetDecimal(startText, 8)
		if !ok || err != nil || start != len(bindings) || body == "" {
			return nil, fmt.Errorf("compact input-set store packs must have exact contiguous offsets")
		}
		indices := strings.Split(body, ".")
		if len(indices) > maxCompactInputSetPackEntries || len(indices) > len(keys)-start {
			return nil, fmt.Errorf("compact input-set store pack has excess entries")
		}
		for offset, text := range indices {
			index, err := compactInputSetDecimal(text, 0)
			if err != nil || index >= len(roots) || roots[index].root == "" {
				return nil, fmt.Errorf("compact input-set store pack references unbound index %q", text)
			}
			key := keys[start+offset]
			producer, slot, _ := strings.Cut(key, ":")
			if !isDigest(producer) || len(slot) != 8 {
				return nil, fmt.Errorf("invalid authenticated compact input-set producer key %q", key)
			}
			bindings[key] = filepath.Join(roots[index].root, "nodes", producer, slot)
			if seenKeys[key] {
				assignedIndices[key] = index
			}
			usedRoots[index] = true
		}
	}
	if len(bindings) != len(keys) {
		return nil, fmt.Errorf("compact input-set store packs cover %d producers, want %d", len(bindings), len(keys))
	}
	for index, anchor := range roots {
		assigned, exists := assignedIndices[anchor.key]
		if !usedRoots[index] || !exists || assigned != index {
			return nil, fmt.Errorf("compact input-set store anchor does not belong to its assigned index %d", index)
		}
	}
	return bindings, nil
}
