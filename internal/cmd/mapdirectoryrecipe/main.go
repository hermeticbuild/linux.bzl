// mapdirectoryrecipe is a private execution-time adapter for the Bazel 9
// map_directory Kconfig feasibility test. It replays the concrete flags from
// one emitted compact action-plan recipe instead of reconstructing Kbuild
// flags in Starlark.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

type recipeOptions struct {
	recipe         string
	kind           string
	tool           string
	source         string
	inputs         []string
	output         string
	expectedSource string
	expectedObject string
	expectedID     string
	actionArgs     []string
}

const (
	kbuildArgsSentinel = "__LINUX_BZL_KBUILD_ARGS_V1__"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, " ") }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseRecipe(path string) (kconfig.CompactObjectVariant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return kconfig.CompactObjectVariant{}, fmt.Errorf("read recipe: %w", err)
	}
	var recipe kconfig.CompactObjectVariant
	if err := json.Unmarshal(data, &recipe); err != nil {
		return kconfig.CompactObjectVariant{}, fmt.Errorf("decode recipe: %w", err)
	}
	return recipe, nil
}

func runRecipe(opts recipeOptions) error {
	for name, value := range map[string]string{
		"recipe": opts.recipe, "kind": opts.kind, "tool": opts.tool,
		"output":          opts.output,
		"expected object": opts.expectedObject, "expected content ID": opts.expectedID,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	recipe, err := parseRecipe(opts.recipe)
	if err != nil {
		return err
	}
	if recipe.Object != opts.expectedObject {
		return fmt.Errorf("recipe object = %q, want %q", recipe.Object, opts.expectedObject)
	}
	if recipe.ContentID != opts.expectedID {
		return fmt.Errorf("recipe content ID = %q, want %q", recipe.ContentID, opts.expectedID)
	}
	if (recipe.Mode != "y" && recipe.Mode != "m") ||
		(recipe.ModuleRoot && recipe.Mode != "m") ||
		len(recipe.Deps) != 0 || len(recipe.RemoveFlags) != 0 || filepath.Ext(recipe.Object) != ".o" {
		return errors.New("recipe is not a supported object action")
	}
	if opts.kind == "compile" {
		if recipe.Source != opts.expectedSource {
			return fmt.Errorf("recipe source = %q, want %q", recipe.Source, opts.expectedSource)
		}
		sourceExtension := filepath.Ext(recipe.Source)
		if len(recipe.Members) != 0 || len(opts.inputs) != 0 || opts.source == "" ||
			(sourceExtension != ".c" && sourceExtension != ".S" && sourceExtension != ".s") {
			return fmt.Errorf("recipe is not a supported source-to-object compile: %q -> %q", recipe.Source, recipe.Object)
		}
	} else if opts.kind == "composite" {
		if len(recipe.Members) == 0 || len(opts.inputs) != len(recipe.Members) || opts.source != "" || opts.expectedSource != "" {
			return fmt.Errorf("recipe is not a composite with %d declared members", len(opts.inputs))
		}
	} else {
		return fmt.Errorf("unsupported action kind %q", opts.kind)
	}

	var args []string
	foundSentinel := false
	for _, argument := range opts.actionArgs {
		if argument == kbuildArgsSentinel {
			if foundSentinel {
				return errors.New("compile action repeats its Kbuild-arguments marker")
			}
			foundSentinel = true
			if opts.kind == "compile" {
				args = append(args, recipe.Flags...)
				args = append(args, "-c", opts.source, "-o", opts.output)
			} else {
				args = append(args, "-r", "-o", opts.output)
				args = append(args, opts.inputs...)
			}
			continue
		}
		args = append(args, argument)
	}
	if !foundSentinel {
		return errors.New("compile action is missing its Kbuild-arguments marker")
	}
	command := exec.Command(opts.tool, args...)
	command.Env = os.Environ()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("compile recipe: %w", err)
	}
	return nil
}

func main() {
	var actionArgs repeatedFlag
	var inputs repeatedFlag
	opts := recipeOptions{}
	flag.StringVar(&opts.kind, "kind", "", "action kind")
	flag.StringVar(&opts.recipe, "recipe", "", "compact action-plan recipe JSON")
	flag.StringVar(&opts.tool, "tool", "", "selected Kbuild tool")
	flag.StringVar(&opts.source, "source", "", "declared primary source input")
	flag.StringVar(&opts.output, "output", "", "declared object output")
	flag.StringVar(&opts.expectedSource, "expected_source", "", "canonical source path encoded by the plan")
	flag.StringVar(&opts.expectedObject, "expected_object", "", "object path encoded by the plan")
	flag.StringVar(&opts.expectedID, "expected_content_id", "", "content ID encoded by the plan")
	flag.Var(&actionArgs, "action_arg", "selected CcToolchain compile action argument (repeatable)")
	flag.Var(&inputs, "input", "declared node input (repeatable)")
	flag.Parse()
	opts.actionArgs = actionArgs
	opts.inputs = inputs
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mapdirectoryrecipe: positional arguments are not supported")
		os.Exit(2)
	}
	if err := runRecipe(opts); err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
		os.Exit(1)
	}
}
