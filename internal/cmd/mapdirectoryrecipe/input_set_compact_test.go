package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func compactManifestFixture(t *testing.T, root string, manifests map[string]string, directory string) string {
	t.Helper()
	for id, source := range manifests {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(directory, filepath.FromSlash(compactInputSetManifestSuffix(id)))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(directory, filepath.FromSlash(compactInputSetManifestSuffix(root)))
}

func compactProducerFixture(t *testing.T, store, producer string, slot int, contents string) string {
	t.Helper()
	filename := filepath.Join(store, "nodes", producer, fmt.Sprintf("%08d", slot))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return filename
}

func compactTransportFixture(t *testing.T, mapped bool) (recipeOptions, recipeOptions) {
	t.Helper()
	directory := t.TempDir()
	p, q := strings.Repeat("a", 64), strings.Repeat("b", 64)
	firstConfig, secondConfig := "k8-fastbuild-ST-first", "k8-fastbuild-ST-second"
	if mapped {
		firstConfig, secondConfig = "cfg", "cfg"
	}
	firstStore := filepath.Join(directory, "bazel-out", firstConfig, "bin/family.objects")
	secondStore := filepath.Join(directory, "bazel-out", secondConfig, "bin/family.objects")
	first := compactProducerFixture(t, firstStore, p, 0, "first\n")
	second := compactProducerFixture(t, secondStore, q, 1, "second\n")
	third := compactProducerFixture(t, secondStore, q, 2, "third\n")
	source := filepath.Join(directory, "source")
	if err := os.WriteFile(source, []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []kconfig.ActionPlanInputSetEntry{
		{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetWorkTarget, Path: "renamed/header"}, ProducerID: p, Slot: 0},
		{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetAmbientTarget, Path: "other/second"}, ProducerID: q, Slot: 1},
		{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetTreeTarget, Tree: "kernel", Path: "other/third"}, ProducerID: q, Slot: 2, CompilerUse: true, AuxiliaryUse: true},
	}
	for index := 0; index < 37; index++ {
		entries = append(entries, kconfig.ActionPlanInputSetEntry{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("source/%02d", index)}, SourceID: "src-00000001"})
	}
	root, manifests := writeActionPlanInputSet(t, entries)
	rootFile := compactManifestFixture(t, root, manifests, filepath.Join(directory, "bazel-out", firstConfig, "bin/family.plan"))
	explicit := recipeOptions{inputSetRoot: root, inputSetManifests: manifests,
		inputSetSources: map[string]string{"src-00000001": source},
		inputSetInputs:  map[string]string{actionPlanInputSetProducerBinding(p, 0): first, actionPlanInputSetProducerBinding(q, 1): second, actionPlanInputSetProducerBinding(q, 2): third}}
	compact := explicit
	compact.inputSetManifests, compact.inputSetInputs = nil, nil
	compact.inputSetManifestRoot = rootFile
	compact.inputSetStoreAnchors = map[string]string{"0:" + actionPlanInputSetProducerBinding(p, 0): first, "1:" + actionPlanInputSetProducerBinding(q, 1): second}
	compact.inputSetStorePacks = []string{"00000000:0.1", "00000002:1"}
	return explicit, compact
}

func cloneCompactOptions(opts recipeOptions) recipeOptions {
	opts.inputSetSources = cloneStringMap(opts.inputSetSources)
	opts.inputSetStoreAnchors = cloneStringMap(opts.inputSetStoreAnchors)
	opts.inputSetStorePacks = append([]string(nil), opts.inputSetStorePacks...)
	return opts
}

func TestCompactInputSetEquivalentExplicitAndMappedStores(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped-%t", mapped), func(t *testing.T) {
			explicit, compact := compactTransportFixture(t, mapped)
			before, err := loadActionPlanInputSet(explicit)
			if err != nil {
				t.Fatal(err)
			}
			after, err := loadActionPlanInputSet(compact)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("compact transport changed resolved entries/provenance/targets")
			}
			// Input maps are caller-owned; import must not fill or rewrite them.
			if compact.inputSetInputs != nil || compact.inputSetManifests != nil {
				t.Fatal("compact options mutated")
			}
		})
	}
}

func TestCompactInputSetRejectsInvalidTransport(t *testing.T) {
	explicit, original := compactTransportFixture(t, true)
	p, q := strings.Repeat("a", 64), strings.Repeat("b", 64)
	firstKey, secondKey := "0:"+actionPlanInputSetProducerBinding(p, 0), "1:"+actionPlanInputSetProducerBinding(q, 1)
	tests := map[string]func(*recipeOptions){
		"explicit manifests":         func(o *recipeOptions) { o.inputSetManifests = explicit.inputSetManifests },
		"explicit producers":         func(o *recipeOptions) { o.inputSetInputs = explicit.inputSetInputs },
		"anchors without compact":    func(o *recipeOptions) { o.inputSetManifestRoot = ""; o.inputSetManifests = explicit.inputSetManifests },
		"wrong root manifest suffix": func(o *recipeOptions) { o.inputSetManifestRoot += ".extra" },
		"wrong root identity":        func(o *recipeOptions) { o.inputSetRoot = strings.Repeat("e", 64) },
		"anchor suffix":              func(o *recipeOptions) { o.inputSetStoreAnchors[firstKey] += ".extra" },
		"anchor root mixing":         func(o *recipeOptions) { o.inputSetStoreAnchors[firstKey] = o.inputSetStoreAnchors[secondKey] },
		"padded anchor index": func(o *recipeOptions) {
			value := o.inputSetStoreAnchors[firstKey]
			delete(o.inputSetStoreAnchors, firstKey)
			o.inputSetStoreAnchors["00:"+firstKey[2:]] = value
		},
		"gap anchor index": func(o *recipeOptions) {
			value := o.inputSetStoreAnchors[secondKey]
			delete(o.inputSetStoreAnchors, secondKey)
			o.inputSetStoreAnchors["2:"+secondKey[2:]] = value
		},
		"duplicate anchor index": func(o *recipeOptions) {
			value := o.inputSetStoreAnchors[secondKey]
			delete(o.inputSetStoreAnchors, secondKey)
			o.inputSetStoreAnchors["0:"+secondKey[2:]] = value
		},
		"duplicate anchor key": func(o *recipeOptions) {
			delete(o.inputSetStoreAnchors, secondKey)
			o.inputSetStoreAnchors["1:"+firstKey[2:]] = o.inputSetStoreAnchors[firstKey]
		},
		"unknown anchor key": func(o *recipeOptions) {
			delete(o.inputSetStoreAnchors, secondKey)
			o.inputSetStoreAnchors["1:"+strings.Repeat("e", 64)+":00000001"] = o.inputSetStoreAnchors[firstKey]
		},
		"short slot": func(o *recipeOptions) {
			value := o.inputSetStoreAnchors[firstKey]
			delete(o.inputSetStoreAnchors, firstKey)
			o.inputSetStoreAnchors["0:"+p+":0"] = value
		},
		"missing pack":                        func(o *recipeOptions) { o.inputSetStorePacks = o.inputSetStorePacks[:1] },
		"extra pack":                          func(o *recipeOptions) { o.inputSetStorePacks = append(o.inputSetStorePacks, "00000003:0") },
		"pack gap":                            func(o *recipeOptions) { o.inputSetStorePacks[1] = "00000003:1" },
		"pack overlap":                        func(o *recipeOptions) { o.inputSetStorePacks[1] = "00000001:1" },
		"pack offset short":                   func(o *recipeOptions) { o.inputSetStorePacks[0] = "0:0.1" },
		"pack empty":                          func(o *recipeOptions) { o.inputSetStorePacks[0] = "00000000:" },
		"pack empty component":                func(o *recipeOptions) { o.inputSetStorePacks[0] = "00000000:0..1" },
		"pack index padded":                   func(o *recipeOptions) { o.inputSetStorePacks[0] = "00000000:00.1" },
		"pack index negative":                 func(o *recipeOptions) { o.inputSetStorePacks[0] = "00000000:-1.1" },
		"pack index unbound":                  func(o *recipeOptions) { o.inputSetStorePacks[0] = "00000000:2.1" },
		"unused store":                        func(o *recipeOptions) { o.inputSetStorePacks = []string{"00000000:0.0.0"} },
		"converged root anchor misassignment": func(o *recipeOptions) { o.inputSetStorePacks = []string{"00000000:1.0.1"} },
		"missing source":                      func(o *recipeOptions) { o.inputSetSources = nil },
		"extra source":                        func(o *recipeOptions) { o.inputSetSources["src-00000002"] = o.inputSetSources["src-00000001"] },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			opts := cloneCompactOptions(original)
			change(&opts)
			if _, err := loadActionPlanInputSet(opts); err == nil {
				t.Fatal("invalid compact transport accepted")
			}
		})
	}
}

func TestCompactInputSetRejectsMissingAndTamperedWitnesses(t *testing.T) {
	for _, kind := range []string{"missing child", "child hash", "canonical bytes", "producer absent", "producer directory"} {
		t.Run(kind, func(t *testing.T) {
			_, opts := compactTransportFixture(t, false)
			root, err := compactInputSetPhysicalRoot(opts.inputSetManifestRoot, compactInputSetManifestSuffix(opts.inputSetRoot))
			if err != nil {
				t.Fatal(err)
			}
			node, _, err := decodeActionPlanInputSetManifest(opts.inputSetManifestRoot, opts.inputSetRoot)
			if err != nil || len(node.Children) == 0 {
				t.Fatalf("branch fixture: %v", err)
			}
			child := filepath.Join(root, compactInputSetManifestSuffix(node.Children[0].ID))
			switch kind {
			case "missing child":
				err = os.Remove(child)
			case "child hash":
				data, readErr := os.ReadFile(child)
				if readErr != nil {
					t.Fatal(readErr)
				}
				data = []byte(strings.Replace(string(data), "source/", "forged/", 1))
				err = os.WriteFile(child, data, 0o644)
			case "canonical bytes":
				data, readErr := os.ReadFile(child)
				if readErr != nil {
					t.Fatal(readErr)
				}
				err = os.WriteFile(child, append(data, '\n'), 0o644)
			default:
				filename := opts.inputSetStoreAnchors["0:"+strings.Repeat("a", 64)+":00000000"]
				err = os.Remove(filename)
				if err == nil && kind == "producer directory" {
					err = os.Mkdir(filename, 0o755)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadActionPlanInputSet(opts); err == nil {
				t.Fatal("missing/tampered input accepted")
			}
		})
	}
}

func TestCompactInputSetSourceOnlyAndPackBoundaries(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, manifests := writeActionPlanInputSet(t, []kconfig.ActionPlanInputSetEntry{{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetWorkTarget, Path: "source"}, SourceID: "src-00000001"}})
	opts := recipeOptions{inputSetRoot: root, inputSetManifestRoot: compactManifestFixture(t, root, manifests, t.TempDir()), inputSetSources: map[string]string{"src-00000001": source}}
	if _, err := loadActionPlanInputSet(opts); err != nil {
		t.Fatal(err)
	}
	opts.inputSetStorePacks = []string{"00000000:0"}
	if _, err := loadActionPlanInputSet(opts); err == nil {
		t.Fatal("producer pack accepted for source-only closure")
	}

	expected := map[string]bool{}
	producer := strings.Repeat("a", 64)
	for slot := 0; slot < 257; slot++ {
		expected[actionPlanInputSetProducerBinding(producer, slot)] = true
	}
	anchors := map[string]string{"0:" + actionPlanInputSetProducerBinding(producer, 0): "/store/nodes/" + producer + "/00000000"}
	pack := "00000000:" + strings.TrimSuffix(strings.Repeat("0.", 256), ".")
	if bindings, err := resolveCompactActionPlanInputSetProducers(expected, anchors, []string{pack, "00000256:0"}); err != nil || len(bindings) != 257 {
		t.Fatalf("boundary: %d %v", len(bindings), err)
	}
	if _, err := resolveCompactActionPlanInputSetProducers(expected, anchors, []string{pack + ".0"}); err == nil {
		t.Fatal("257-entry pack accepted")
	}
	var flag compactInputSetFlags
	if err := flag.Set(strings.Repeat("x", maxParameterFileLineBytes+1)); err == nil {
		t.Fatal("oversized compact flag accepted")
	}
	flag.bytes = maxParameterFileBytes
	if err := flag.Set("x"); err == nil {
		t.Fatal("total flag budget not enforced")
	}
	flag.bytes = 0
	flag.values = make([]string, maxParameterFileArguments)
	if err := flag.Set("x"); err == nil {
		t.Fatal("flag count budget not enforced")
	}
}

func TestRunRecipeCompactInputSetRelativePathsAndRenamedTargets(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	producer := strings.Repeat("c", 64)
	input := compactProducerFixture(t, "bazel-out/cfg/bin/family.store", producer, 3, "observed bytes\n")
	root, manifests := writeActionPlanInputSet(t, []kconfig.ActionPlanInputSetEntry{{
		Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetWorkTarget, Path: "renamed/staging.txt"}, ProducerID: producer, Slot: 3,
	}})
	manifestRoot := compactManifestFixture(t, root, manifests, "bazel-out/cfg/bin/family.plan")
	recipe := kconfig.ActionRecipe{Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper",
		WorkingDirectory: "object", Arguments: []string{"renamed/staging.txt", "${output:00000000}"}, Outputs: []string{"00000000"}}
	recipePath, recipeID := writeRecipe(t, recipe)
	if err := os.WriteFile("helper", []byte("#!/bin/sh\nset -eu\nIFS= read -r line < \"$1\"\nprintf '%s\\n' \"$line\" > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runRecipe(recipeOptions{recipe: recipePath, kind: "generate", expectedNodeID: strings.Repeat("d", 64), expectedRecipeID: recipeID,
		toolRole: "helper", workingDirectory: "work", workingDirectoryMarker: "work/.linux-bzl-work-root",
		tools: map[string]string{"helper": "helper"}, outputs: map[string]string{"00000000": "out/result"},
		inputSetRoot: root, inputSetManifestRoot: manifestRoot, inputSetStoreAnchors: map[string]string{"0:" + actionPlanInputSetProducerBinding(producer, 3): input}, inputSetStorePacks: []string{"00000000:0"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("out/result")
	if err != nil || string(data) != "observed bytes\n" {
		t.Fatalf("actual recipe output %q: %v", data, err)
	}
}

// Exercise actual flag dispatch in a subprocess without building a second
// binary or letting main replace the test process's global flag set.
func TestCompactInputSetCLIHelper(t *testing.T) {
	if os.Getenv("LINUX_BZL_COMPACT_INPUT_SET_TEST_HELPER") != "1" {
		return
	}
	for index, argument := range os.Args {
		if argument == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[index+1:]...)
			flag.CommandLine = flag.NewFlagSet("mapdirectoryrecipe", flag.ExitOnError)
			main()
			return
		}
	}
	t.Fatal("helper missing argument separator")
}

func TestCompactInputSetCLIDispatch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, negative := range []string{"", "repeated root", "duplicate anchor", "explicit override"} {
		t.Run(negative, func(t *testing.T) {
			directory := t.TempDir()
			producer := strings.Repeat("c", 64)
			input := compactProducerFixture(t, filepath.Join(directory, "bazel-out/cfg/bin/store"), producer, 3, "cli\n")
			root, manifests := writeActionPlanInputSet(t, []kconfig.ActionPlanInputSetEntry{{Target: kconfig.ActionPlanInputSetTarget{Kind: kconfig.ActionPlanInputSetWorkTarget, Path: "renamed/input"}, ProducerID: producer, Slot: 3}})
			rootFile := compactManifestFixture(t, root, manifests, filepath.Join(directory, "bazel-out/cfg/bin/plan"))
			recipe := kconfig.ActionRecipe{Schema: kconfig.LinuxKernelPlanSchema, Kind: "generate", Tool: "helper", WorkingDirectory: "object", Arguments: []string{"renamed/input", "${output:00000000}"}, Outputs: []string{"00000000"}}
			recipePath, recipeID := writeRecipe(t, recipe)
			helper := filepath.Join(directory, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nset -eu\nIFS= read -r line < \"$1\"\nprintf '%s\\n' \"$line\" > \"$2\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			args := []string{"-recipe", recipePath, "-kind", "generate", "-tool_role", "helper", "-tool", "helper=" + helper,
				"-expected_node_id", strings.Repeat("d", 64), "-expected_recipe_id", recipeID,
				"-working_directory_marker", "work/.linux-bzl-work-root", "-recipe_output", "00000000=out/result",
				"-input_set_root", root, "-input_set_manifest_root", rootFile,
				"-input_set_store_anchor", "0:" + actionPlanInputSetProducerBinding(producer, 3) + "=" + input, "-input_set_store_pack", "00000000:0"}
			switch negative {
			case "repeated root":
				args = append(args, "-input_set_manifest_root", rootFile)
			case "duplicate anchor":
				args = append(args, "-input_set_store_anchor", "0:"+actionPlanInputSetProducerBinding(producer, 3)+"="+input)
			case "explicit override":
				args = append(args, "-input_set_input", actionPlanInputSetProducerBinding(producer, 3)+"="+input)
			}
			command := exec.Command(executable, append([]string{"-test.run=^TestCompactInputSetCLIHelper$", "--"}, args...)...)
			command.Dir = directory
			command.Env = append(os.Environ(), "LINUX_BZL_COMPACT_INPUT_SET_TEST_HELPER=1")
			output, err := command.CombinedOutput()
			if negative != "" {
				if err == nil {
					t.Fatalf("invalid CLI succeeded: %s", output)
				}
				if _, err := os.Stat(filepath.Join(directory, "out/result")); !os.IsNotExist(err) {
					t.Fatalf("invalid CLI published output: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CLI failed: %v\n%s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(directory, "out/result"))
			if err != nil || string(data) != "cli\n" {
				t.Fatalf("CLI output %q: %v", data, err)
			}
		})
	}
}
