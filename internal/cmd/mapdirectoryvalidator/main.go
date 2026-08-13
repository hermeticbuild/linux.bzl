package main

import (
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

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
	ContentID  string   `json:"content_id"`
	Object     string   `json:"object"`
	Source     string   `json:"source"`
	Mode       string   `json:"mode"`
	ModuleRoot bool     `json:"module_root"`
	Flags      []string `json:"flags"`
	Members    []string `json:"members"`
}

type planValidation struct {
	identity string
	objects  []string
}

func validatePlan(root string) (planValidation, error) {
	files, err := treeFiles(root)
	if err != nil {
		return planValidation{}, fmt.Errorf("walk action plan: %w", err)
	}
	identity := ""
	schema := false
	var sources []string
	recipes := map[string]string{}
	nodes := map[string]map[string]string{}
	for _, marker := range files {
		parts := strings.Split(marker, "/")
		switch {
		case marker == "schema/linux-kernel-plan-v2":
			schema = true
		case len(parts) == 3 && parts[0] == "toolsets" && parts[1] == "target":
			if identity != "" || !strings.HasPrefix(parts[2], "sha256-") || len(parts[2]) != len("sha256-")+64 {
				return planValidation{}, fmt.Errorf("invalid or repeated target toolset marker %q", marker)
			}
			identity = parts[2]
		case len(parts) >= 4 && parts[0] == "sources" && parts[2] == "kernel":
			sources = append(sources, marker)
		case len(parts) == 2 && parts[0] == "recipes" && strings.HasSuffix(parts[1], ".json"):
			id := strings.TrimSuffix(parts[1], ".json")
			if _, exists := recipes[id]; exists {
				return planValidation{}, fmt.Errorf("repeated recipe %q", id)
			}
			recipes[id] = marker
		case len(parts) >= 5 && parts[0] == "nodes" && parts[1] == "target":
			id := parts[2]
			if nodes[id] == nil {
				nodes[id] = map[string]string{}
			}
			node := nodes[id]
			var key, value string
			switch {
			case len(parts) == 5 && (parts[3] == "kind" || parts[3] == "recipe" || parts[3] == "tool"):
				key, value = parts[3], parts[4]
			case len(parts) == 8 && parts[3] == "in" && parts[4] == "source" && parts[5] == "src" && parts[6] == "00000000":
				key, value = "source", parts[7]
			case len(parts) == 9 && parts[3] == "in" && parts[4] == "node" && parts[5] == "member" && parts[8] == "00000000":
				key, value = "member:"+parts[6], parts[7]
			case len(parts) >= 7 && parts[3] == "out" && parts[4] == "objects" && parts[5] == "00000000":
				key, value = "output", strings.Join(parts[6:], "/")
			default:
				return planValidation{}, fmt.Errorf("invalid target node marker %q", marker)
			}
			if _, exists := node[key]; exists {
				return planValidation{}, fmt.Errorf("node %q repeats %s", id, key)
			}
			node[key] = value
		default:
			return planValidation{}, fmt.Errorf("unexpected action-plan marker %q", marker)
		}
	}
	if !schema || identity == "" {
		return planValidation{}, fmt.Errorf("action plan is missing its v2 schema or target toolset marker")
	}
	wantSources := []string{
		"assembly_selected.S",
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
		"required_local_header.h",
		"supported.c",
	}
	if len(sources) != len(wantSources)+3 {
		return planValidation{}, fmt.Errorf("selected source markers = %q, want common closure, one Kconfig-selected source, and two composite members", sources)
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
	}
	if len(nodes) != 6 {
		return planValidation{}, fmt.Errorf("target nodes = %v, want five compiles and one module composite", sortedKeys(nodes))
	}
	seenObjects := map[string]bool{}
	selectedObject := ""
	for id, node := range nodes {
		if node["tool"] != "target" || node["recipe"] == "" || node["output"] == "" {
			return planValidation{}, fmt.Errorf("target node %q is incomplete: %v", id, node)
		}
		recipePath, exists := recipes[node["recipe"]]
		if !exists {
			return planValidation{}, fmt.Errorf("target node %q references unknown recipe %q", id, node["recipe"])
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(recipePath)))
		if err != nil {
			return planValidation{}, fmt.Errorf("read compile recipe: %w", err)
		}
		var recipe compileRecipe
		if err := json.Unmarshal(data, &recipe); err != nil {
			return planValidation{}, fmt.Errorf("decode compile recipe: %w", err)
		}
		wantFlags := wantObjects[recipe.Object]
		if recipe.ContentID != id || node["recipe"] != id || node["output"] != recipe.Object {
			return planValidation{}, fmt.Errorf("compile recipe does not match node %q: node=%v recipe=%#v", id, node, recipe)
		}
		if node["kind"] == "composite" {
			if recipe.Object != "composite.o" || recipe.Mode != "m" || !recipe.ModuleRoot || len(recipe.Members) != 2 || node["member:00000000"] == "" || node["member:00000001"] == "" || node["source"] != "" {
				return planValidation{}, fmt.Errorf("module composite recipe does not match node %q: node=%v recipe=%#v", id, node, recipe)
			}
			seenObjects[recipe.Object] = true
			continue
		}
		sourceExtension := filepath.Ext(recipe.Source)
		if node["kind"] != "compile" || node["source"] == "" || len(recipe.Members) != 0 ||
			(sourceExtension != ".c" && sourceExtension != ".S" && sourceExtension != ".s") ||
			recipe.Source != strings.TrimSuffix(recipe.Object, ".o")+sourceExtension {
			return planValidation{}, fmt.Errorf("compile recipe does not match node %q: node=%v recipe=%#v", id, node, recipe)
		}
		sourceFound := false
		for _, source := range sources {
			parts := strings.Split(source, "/")
			if parts[1] == node["source"] && strings.HasSuffix(source, "/"+recipe.Source) {
				sourceFound = true
				break
			}
		}
		if !sourceFound {
			return planValidation{}, fmt.Errorf("node %q does not reference the recipe source %q", id, recipe.Source)
		}
		if seenObjects[recipe.Object] {
			return planValidation{}, fmt.Errorf("compile plan repeats recipe for %q", recipe.Object)
		}
		seenObjects[recipe.Object] = true
		if recipe.Object != "supported.o" && recipe.Object != "assembly_selected.o" && !strings.HasPrefix(recipe.Object, "composite_") {
			if !strings.HasSuffix(recipe.Object, "_selected.o") {
				return planValidation{}, fmt.Errorf("Kconfig-selected object %q does not use the fixture's selected-object contract", recipe.Object)
			}
			selectedObject = recipe.Object
			wantFlags = []string{"-DMAP_DIRECTORY_" + strings.ToUpper(strings.TrimSuffix(recipe.Object, "_selected.o")) + "_RECIPE_REPLAYED=1"}
		}
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
	if !seenObjects["supported.o"] || !seenObjects["assembly_selected.o"] || !seenObjects["composite.o"] || !seenObjects["composite_first.o"] || !seenObjects["composite_second.o"] || selectedObject == "" {
		return planValidation{}, fmt.Errorf("compile plan did not contain its common and selected objects: %v", sortedKeys(seenObjects))
	}
	for _, path := range files {
		if strings.Contains(path, "unsupported") {
			return planValidation{}, fmt.Errorf("Kconfig-disabled branch appeared in action plan as %q", path)
		}
	}
	return planValidation{
		identity: identity,
		objects:  []string{"supported.o", "assembly_selected.o", selectedObject, "composite_first.o", "composite_second.o", "composite.o"},
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

func validateObjects(root string, objects []string) error {
	files, err := treeFiles(root)
	if err != nil {
		return fmt.Errorf("walk mapped objects: %w", err)
	}
	wantSymbols := map[string]string{}
	for _, object := range objects {
		stem := strings.TrimSuffix(object, ".o")
		if object == "supported.o" {
			wantSymbols[object] = "map_directory_selected"
		} else if object == "composite.o" {
			wantSymbols[object] = ""
		} else if strings.HasPrefix(stem, "composite_") {
			wantSymbols[object] = "map_directory_" + stem
		} else {
			wantSymbols[object] = "map_directory_" + strings.TrimSuffix(stem, "_selected") + "_selected"
		}
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
		found := wantSymbol == ""
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
	var compilerSuffixArgs repeatedFlag
	var linkerArgs repeatedFlag
	var linkerSuffixArgs repeatedFlag
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
	flag.Var(&compilerSuffixArgs, "compiler_suffix_arg", "configured compiler suffix argument")
	flag.Var(&linkerArgs, "linker_arg", "configured linker prefix argument")
	flag.Var(&linkerSuffixArgs, "linker_suffix_arg", "configured linker suffix argument")
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
		if strings.Join(first.objects, "\x00") == strings.Join(second.objects, "\x00") {
			return fmt.Errorf("different compiler probes selected the same object recipes %q", first.objects)
		}
		contents := fmt.Sprintf("map_directory compiler plans differ: %s/%q != %s/%q\n", first.identity, first.objects, second.identity, second.objects)
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
		Profile:            *targetProfile,
		Architecture:       *linuxArch,
		TargetTriple:       *targetTriple,
		CompilerPath:       *compiler,
		LinkerPath:         *linker,
		ArchiverPath:       *archiver,
		NMPath:             *nm,
		ObjcopyPath:        *objcopy,
		CompilerArgs:       compilerArgs,
		CompilerSuffixArgs: compilerSuffixArgs,
		LinkerArgs:         linkerArgs,
		LinkerSuffixArgs:   linkerSuffixArgs,
	})
	if err != nil {
		return err
	}
	selectedIdentity := probe.Identity()
	if planResult.identity != selectedIdentity {
		return fmt.Errorf("plan probe identity = %q, selected tool identity = %q", planResult.identity, selectedIdentity)
	}
	if err := validateObjects(*objects, planResult.objects); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(fmt.Sprintf("map_directory compiler capability spike passed: %s, objects=%q\n", planResult.identity, planResult.objects)), 0o644); err != nil {
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
