package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type familyViewProjection struct {
	relative string
	source   string
}

func validateFamilyViewDestinations(projections []familyViewProjection) error {
	destinations := make(map[string]bool, len(projections))
	for _, projection := range projections {
		destinations[projection.relative] = true
	}
	for _, projection := range projections {
		for ancestor := path.Dir(projection.relative); ancestor != "."; ancestor = path.Dir(ancestor) {
			if destinations[ancestor] {
				return fmt.Errorf("family view destinations %q and %q have a file/subtree collision", ancestor, projection.relative)
			}
		}
	}
	return nil
}

func validPlanComponent(value string) bool {
	if value == "" || len(value) > 240 || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= '0' && value[0] <= '9')) {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && !strings.ContainsRune("_.+-", character) {
			return false
		}
	}
	return true
}

func canonicalRelativePath(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\\\x00\r\n") && !path.IsAbs(value) && path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../")
}

func canonicalDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func canonicalSlot(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func projectFamilyView(planRoot, variant, tree, storeRoot, outputRoot string, expectedCount int, preserveMode bool, markers []string) error {
	if planRoot == "" || storeRoot == "" || outputRoot == "" {
		return fmt.Errorf("family view projection roots must not be empty")
	}
	if !validPlanComponent(variant) || !validPlanComponent(tree) {
		return fmt.Errorf("family view variant/tree %q/%q is invalid", variant, tree)
	}
	if expectedCount <= 0 {
		return fmt.Errorf("family view expected count must be positive")
	}
	if !preserveMode {
		return fmt.Errorf("family view projection requires -preserve_mode")
	}
	markerRoot := filepath.Join(planRoot, "variants", variant, "view", tree)
	projections := make([]familyViewProjection, 0, expectedCount)
	seenDestinations := map[string]bool{}
	for _, filename := range markers {
		relative, err := filepath.Rel(markerRoot, filename)
		if err != nil {
			return err
		}
		if !canonicalRelativePath(filepath.ToSlash(relative)) {
			return fmt.Errorf("family view marker %q is outside its declared view", filename)
		}
		info, err := os.Lstat(filename)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("family view marker %q is a symlink", filepath.ToSlash(relative))
		}
		if !info.Mode().IsRegular() || info.Size() != 0 {
			return fmt.Errorf("family view marker %q is not an empty regular file", filepath.ToSlash(relative))
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) < 5 || parts[0] != "from" || !canonicalDigest(parts[1]) || !canonicalSlot(parts[2]) || parts[3] != "at" {
			return fmt.Errorf("family view marker %q has invalid grammar", filepath.ToSlash(relative))
		}
		destination := strings.Join(parts[4:], "/")
		if !canonicalRelativePath(destination) {
			return fmt.Errorf("family view marker %q has invalid destination", filepath.ToSlash(relative))
		}
		if seenDestinations[destination] {
			return fmt.Errorf("family view repeats destination %q", destination)
		}
		seenDestinations[destination] = true
		source := filepath.Join(storeRoot, "nodes", parts[1], parts[2])
		sourceInfo, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("inspect family view source for %q: %w", destination, err)
		}
		if !sourceInfo.Mode().IsRegular() {
			return fmt.Errorf("family view source for %q is not a regular file", destination)
		}
		projections = append(projections, familyViewProjection{relative: destination, source: source})
	}
	if len(projections) != expectedCount {
		return fmt.Errorf("family view contains %d markers, want %d", len(projections), expectedCount)
	}
	sort.Slice(projections, func(i, j int) bool { return projections[i].relative < projections[j].relative })
	if err := validateFamilyViewDestinations(projections); err != nil {
		return err
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return fmt.Errorf("create family view output root: %w", err)
	}
	if err := validateFamilyViewOutputPaths(outputRoot, projections); err != nil {
		return err
	}
	for _, projection := range projections {
		if err := copyFile(projection.source, filepath.Join(outputRoot, filepath.FromSlash(projection.relative)), true); err != nil {
			return fmt.Errorf("project family view %q: %w", projection.relative, err)
		}
	}
	return nil
}

// Disjoint projection batches share their declared output parent in a local
// execroot. Validate only the paths owned by this batch: other batches may
// already have produced siblings. Selected leaves must still be absent, and
// none of their ancestors may redirect a copy through a symlink.
func validateFamilyViewOutputPaths(root string, projections []familyViewProjection) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("family view output root %q is not a real directory", root)
	}
	for _, projection := range projections {
		parent := root
		components := strings.Split(projection.relative, "/")
		for _, component := range components[:len(components)-1] {
			parent = filepath.Join(parent, component)
			if err := os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			info, err := os.Lstat(parent)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("family view output ancestor %q is not a real directory", parent)
			}
		}
		leaf := filepath.Join(root, filepath.FromSlash(projection.relative))
		if _, err := os.Lstat(leaf); err == nil {
			return fmt.Errorf("family view output %q is a pre-existing leaf", leaf)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
