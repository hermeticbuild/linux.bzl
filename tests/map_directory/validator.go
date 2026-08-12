package main

import (
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
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

type compileRecipe struct {
	ContentID string   `json:"content_id"`
	Object    string   `json:"object"`
	Source    string   `json:"source"`
	Flags     []string `json:"flags"`
}

type planValidation struct {
	identity string
	family   string
}

func validatePlan(root string) (planValidation, error) {
	files, err := treeFiles(root)
	if err != nil {
		return planValidation{}, fmt.Errorf("walk action plan: %w", err)
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
			return planValidation{}, fmt.Errorf("unexpected action-plan marker %q", path)
		}
	}
	if len(probes) != 1 || !probeMarkerPattern.MatchString(probes[0]) {
		return planValidation{}, fmt.Errorf("measured compiler probe markers = %q, want one sha256 marker", probes)
	}
	family := ""
	for _, path := range compiles {
		switch {
		case strings.HasSuffix(path, "/clang_selected.o.json"):
			if family != "" {
				return planValidation{}, fmt.Errorf("compile markers select more than one compiler family: %q", compiles)
			}
			family = "clang"
		case strings.HasSuffix(path, "/gcc_selected.o.json"):
			if family != "" {
				return planValidation{}, fmt.Errorf("compile markers select more than one compiler family: %q", compiles)
			}
			family = "gcc"
		}
	}
	if family == "" {
		return planValidation{}, fmt.Errorf("compile markers select no compiler family: %q", compiles)
	}
	wantSources := []string{
		family + "_selected.c",
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
		"required_local_header.h",
		"supported.c",
	}
	if len(sources) != len(wantSources) {
		return planValidation{}, fmt.Errorf("selected source markers = %q, want %q", sources, wantSources)
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
			return planValidation{}, fmt.Errorf("selected source markers = %q, missing %q", sources, want)
		}
	}
	wantObjects := map[string][]string{
		"supported.o": {
			"-fno-omit-frame-pointer",
			"-DMAP_DIRECTORY_RECIPE_REPLAYED=1",
		},
		family + "_selected.o": {
			"-DMAP_DIRECTORY_" + strings.ToUpper(family) + "_RECIPE_REPLAYED=1",
		},
	}
	if len(compiles) != len(wantObjects) {
		return planValidation{}, fmt.Errorf("compile markers = %q, want objects %v", compiles, sortedKeys(wantObjects))
	}
	seenObjects := map[string]bool{}
	for _, compile := range compiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(compile)))
		if err != nil {
			return planValidation{}, fmt.Errorf("read compile recipe: %w", err)
		}
		var recipe compileRecipe
		if err := json.Unmarshal(data, &recipe); err != nil {
			return planValidation{}, fmt.Errorf("decode compile recipe: %w", err)
		}
		parts := strings.Split(compile, "/")
		wantFlags, wanted := wantObjects[recipe.Object]
		if len(parts) < 5 || recipe.ContentID != parts[2] || !strings.HasSuffix(compile, "/"+recipe.Object+".json") || recipe.Source != strings.TrimSuffix(recipe.Object, ".o")+".c" || !wanted {
			return planValidation{}, fmt.Errorf("compile recipe does not match its marker path or selected family: path=%q recipe=%#v", compile, recipe)
		}
		if seenObjects[recipe.Object] {
			return planValidation{}, fmt.Errorf("compile plan repeats recipe for %q", recipe.Object)
		}
		seenObjects[recipe.Object] = true
		for _, want := range wantFlags {
			found := false
			for _, flag := range recipe.Flags {
				if flag == want {
					found = true
					break
				}
			}
			if !found {
				return planValidation{}, fmt.Errorf("compile recipe %q flags = %q, missing replayed Kbuild flag %q", recipe.Object, recipe.Flags, want)
			}
		}
	}
	for _, path := range files {
		if strings.Contains(path, "unsupported") || strings.Contains(path, map[string]string{"clang": "gcc_selected", "gcc": "clang_selected"}[family]) {
			return planValidation{}, fmt.Errorf("unselected compiler branch appeared in action plan as %q", path)
		}
	}
	return planValidation{
		identity: strings.TrimPrefix(probes[0], "v1/probe/"),
		family:   family,
	}, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, " ") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func validateObjects(root, family string) error {
	files, err := treeFiles(root)
	if err != nil {
		return fmt.Errorf("walk mapped objects: %w", err)
	}
	wantSymbols := map[string]string{
		"supported.o":          "map_directory_selected",
		family + "_selected.o": "map_directory_" + family + "_selected",
	}
	if len(files) != len(wantSymbols) {
		return fmt.Errorf("mapped objects = %q, want %v", files, sortedKeys(wantSymbols))
	}
	seen := map[string]bool{}
	for _, path := range files {
		base := filepath.Base(path)
		wantSymbol, wanted := wantSymbols[base]
		if !wanted || seen[base] {
			return fmt.Errorf("unexpected or duplicate mapped object %q", path)
		}
		seen[base] = true
		object, err := elf.Open(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return fmt.Errorf("open mapped object %q: %w", path, err)
		}
		if object.Type != elf.ET_REL || object.Machine != elf.EM_X86_64 {
			object.Close()
			return fmt.Errorf("mapped object %q has type %s machine %s, want ET_REL x86_64", path, object.Type, object.Machine)
		}
		symbols, err := object.Symbols()
		object.Close()
		if err != nil {
			return fmt.Errorf("read mapped object %q symbols: %w", path, err)
		}
		found := false
		for _, symbol := range symbols {
			if symbol.Name == wantSymbol {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("mapped object %q does not define %s", path, wantSymbol)
		}
	}
	return nil
}

func run() error {
	var compilerArgs repeatedFlag
	var linkerDriverArgs repeatedFlag
	plan := flag.String("plan", "", "action-plan tree")
	objects := flag.String("objects", "", "mapped object tree")
	otherPlan := flag.String("other_plan", "", "second action-plan tree whose probe identity must differ")
	out := flag.String("out", "", "validation stamp")
	compiler := flag.String("compiler", "", "selected compiler artifact")
	linker := flag.String("linker", "", "selected linker artifact")
	archiver := flag.String("archiver", "", "selected archiver artifact")
	nm := flag.String("nm", "", "selected nm artifact")
	objcopy := flag.String("objcopy", "", "selected objcopy artifact")
	targetProfile := flag.String("target_profile", "", "selected Linux target profile")
	linuxArch := flag.String("linux_arch", "", "selected Linux ARCH")
	targetTriple := flag.String("target_triple", "", "selected compiler target triple")
	flag.Var(&compilerArgs, "compiler_arg", "configured compiler prefix argument")
	flag.Var(&linkerDriverArgs, "linker_driver_arg", "configured linker-driver prefix argument")
	flag.Parse()
	if *otherPlan != "" {
		if *plan == "" || *out == "" {
			return fmt.Errorf("-plan, -other_plan, and -out are required for identity comparison")
		}
		first, err := validatePlan(*plan)
		if err != nil {
			return fmt.Errorf("first plan: %w", err)
		}
		second, err := validatePlan(*otherPlan)
		if err != nil {
			return fmt.Errorf("second plan: %w", err)
		}
		if first.identity == second.identity {
			return fmt.Errorf("compiler-selected plans share probe identity %q", first.identity)
		}
		if first.family == second.family {
			return fmt.Errorf("compiler-selected plans both selected %s recipes", first.family)
		}
		contents := fmt.Sprintf("map_directory compiler plans differ: %s/%s != %s/%s\n", first.family, first.identity, second.family, second.identity)
		if err := os.WriteFile(*out, []byte(contents), 0o644); err != nil {
			return fmt.Errorf("write identity comparison stamp: %w", err)
		}
		return nil
	}
	if *plan == "" || *objects == "" || *out == "" || *compiler == "" || *linker == "" || *archiver == "" || *nm == "" || *objcopy == "" || *targetProfile == "" || *linuxArch == "" || *targetTriple == "" {
		return fmt.Errorf("-plan, -objects, -out, -compiler, -linker, -archiver, -nm, -objcopy, -target_profile, -linux_arch, and -target_triple are required")
	}
	planResult, err := validatePlan(*plan)
	if err != nil {
		return err
	}
	probe, err := kconfig.NewLinuxToolProbe(kconfig.LinuxToolProbeOptions{
		Profile:          *targetProfile,
		Architecture:     *linuxArch,
		TargetTriple:     *targetTriple,
		CompilerPath:     *compiler,
		LinkerPath:       *linker,
		ArchiverPath:     *archiver,
		NMPath:           *nm,
		ObjcopyPath:      *objcopy,
		CompilerArgs:     compilerArgs,
		LinkerDriverArgs: linkerDriverArgs,
	})
	if err != nil {
		return err
	}
	selectedIdentity := probe.Identity()
	if planResult.identity != selectedIdentity {
		return fmt.Errorf("plan probe identity = %q, selected tool identity = %q", planResult.identity, selectedIdentity)
	}
	selectedFamily := probe.CompilerFamily()
	if planResult.family != selectedFamily {
		return fmt.Errorf("plan selected %s recipes for the measured %s compiler", planResult.family, selectedFamily)
	}
	if err := validateObjects(*objects, selectedFamily); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(fmt.Sprintf("map_directory %s compiler capability spike passed: %s\n", selectedFamily, planResult.identity)), 0o644); err != nil {
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
