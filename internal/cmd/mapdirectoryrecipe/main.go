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
	compiler       string
	source         string
	output         string
	expectedSource string
	expectedObject string
	expectedID     string
	actionArgs     []string
}

const (
	compileOutputSentinel = "__linux_bzl_map_output__.o"
	compileRecipeSentinel = "-D__LINUX_BZL_MAP_RECIPE_FLAGS__"
	compileSourceSentinel = "__linux_bzl_map_source__.c"
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
		"recipe": opts.recipe, "compiler": opts.compiler, "source": opts.source,
		"output": opts.output, "expected source": opts.expectedSource,
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
	if recipe.Source != opts.expectedSource {
		return fmt.Errorf("recipe source = %q, want %q", recipe.Source, opts.expectedSource)
	}
	if recipe.Object != opts.expectedObject {
		return fmt.Errorf("recipe object = %q, want %q", recipe.Object, opts.expectedObject)
	}
	if recipe.ContentID != opts.expectedID {
		return fmt.Errorf("recipe content ID = %q, want %q", recipe.ContentID, opts.expectedID)
	}
	if recipe.Mode != "y" || recipe.ModuleRoot || len(recipe.Members) != 0 || len(recipe.Deps) != 0 || len(recipe.RemoveFlags) != 0 {
		return errors.New("recipe is not a supported built-in leaf compile")
	}
	if filepath.Ext(recipe.Source) != ".c" || filepath.Ext(recipe.Object) != ".o" {
		return fmt.Errorf("recipe is not a C-to-object compile: %q -> %q", recipe.Source, recipe.Object)
	}

	var args []string
	foundSource := false
	foundOutput := false
	foundRecipe := false
	for _, argument := range opts.actionArgs {
		if argument == compileRecipeSentinel {
			if foundRecipe {
				return errors.New("compile action repeats its recipe-flags marker")
			}
			foundRecipe = true
			args = append(args, recipe.Flags...)
			continue
		}
		if strings.Contains(argument, compileSourceSentinel) {
			foundSource = true
			argument = strings.ReplaceAll(argument, compileSourceSentinel, opts.source)
		}
		if strings.Contains(argument, compileOutputSentinel) {
			foundOutput = true
			argument = strings.ReplaceAll(argument, compileOutputSentinel, opts.output)
		}
		args = append(args, argument)
	}
	if !foundSource || !foundOutput || !foundRecipe {
		return fmt.Errorf("compile action markers: source=%t output=%t recipe=%t; want all true", foundSource, foundOutput, foundRecipe)
	}
	command := exec.Command(opts.compiler, args...)
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
	opts := recipeOptions{}
	flag.StringVar(&opts.recipe, "recipe", "", "compact action-plan recipe JSON")
	flag.StringVar(&opts.compiler, "compiler", "", "selected C compiler")
	flag.StringVar(&opts.source, "source", "", "declared primary source input")
	flag.StringVar(&opts.output, "output", "", "declared object output")
	flag.StringVar(&opts.expectedSource, "expected_source", "", "canonical source path encoded by the plan")
	flag.StringVar(&opts.expectedObject, "expected_object", "", "object path encoded by the plan")
	flag.StringVar(&opts.expectedID, "expected_content_id", "", "content ID encoded by the plan")
	flag.Var(&actionArgs, "action_arg", "selected CcToolchain compile action argument (repeatable)")
	flag.Parse()
	opts.actionArgs = actionArgs
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "mapdirectoryrecipe: positional arguments are not supported")
		os.Exit(2)
	}
	if err := runRecipe(opts); err != nil {
		fmt.Fprintf(os.Stderr, "mapdirectoryrecipe: %v\n", err)
		os.Exit(1)
	}
}
