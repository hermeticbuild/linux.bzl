package main

import (
	"debug/elf"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var probeMarkerPattern = regexp.MustCompile(`^v1/probe/sha256-[0-9a-f]{64}$`)

func treeFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func validatePlan(root string) error {
	files, err := treeFiles(root)
	if err != nil {
		return fmt.Errorf("walk action plan: %w", err)
	}
	var probes, sources, compiles []string
	for _, path := range files {
		switch {
		case strings.HasPrefix(path, "v1/probe/"):
			probes = append(probes, path)
		case strings.HasPrefix(path, "v1/source/"):
			sources = append(sources, path)
		case strings.HasPrefix(path, "v1/compile/"):
			compiles = append(compiles, path)
		default:
			return fmt.Errorf("unexpected action-plan marker %q", path)
		}
	}
	if len(probes) != 1 || !probeMarkerPattern.MatchString(probes[0]) {
		return fmt.Errorf("measured compiler probe markers = %q, want one sha256 marker", probes)
	}
	wantSources := []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
		"supported.c",
	}
	if len(sources) != len(wantSources) {
		return fmt.Errorf("selected source markers = %q, want %q", sources, wantSources)
	}
	for _, want := range wantSources {
		found := false
		for _, source := range sources {
			if strings.HasSuffix(source, "/"+want) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("selected source markers = %q, missing %q", sources, want)
		}
	}
	if len(compiles) != 1 || !strings.HasSuffix(compiles[0], "/supported.o.json") {
		return fmt.Errorf("compile markers = %q, want only supported.o.json", compiles)
	}
	for _, path := range files {
		if strings.Contains(path, "unsupported") {
			return fmt.Errorf("unsupported compiler branch appeared in action plan as %q", path)
		}
	}
	return nil
}

func validateObject(root string) error {
	files, err := treeFiles(root)
	if err != nil {
		return fmt.Errorf("walk mapped objects: %w", err)
	}
	if len(files) != 1 || !strings.HasSuffix(files[0], "/supported.o") {
		return fmt.Errorf("mapped objects = %q, want exactly supported.o", files)
	}
	objectPath := filepath.Join(root, filepath.FromSlash(files[0]))
	object, err := elf.Open(objectPath)
	if err != nil {
		return fmt.Errorf("open mapped object %q: %w", files[0], err)
	}
	defer object.Close()
	if object.Type != elf.ET_REL {
		return fmt.Errorf("mapped object type = %s, want ET_REL", object.Type)
	}
	if object.Machine != elf.EM_X86_64 {
		return fmt.Errorf("mapped object machine = %s, want EM_X86_64", object.Machine)
	}
	symbols, err := object.Symbols()
	if err != nil {
		return fmt.Errorf("read mapped object symbols: %w", err)
	}
	for _, symbol := range symbols {
		if symbol.Name == "map_directory_selected" {
			return nil
		}
	}
	return fmt.Errorf("mapped object does not define map_directory_selected")
}

func run() error {
	plan := flag.String("plan", "", "action-plan tree")
	objects := flag.String("objects", "", "mapped object tree")
	out := flag.String("out", "", "validation stamp")
	flag.Parse()
	if *plan == "" || *objects == "" || *out == "" {
		return fmt.Errorf("-plan, -objects, and -out are required")
	}
	if err := validatePlan(*plan); err != nil {
		return err
	}
	if err := validateObject(*objects); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte("map_directory compiler capability spike passed\n"), 0o644); err != nil {
		return fmt.Errorf("write validation stamp: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
