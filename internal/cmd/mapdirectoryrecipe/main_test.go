package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestRunRecipeReplaysFlags(t *testing.T) {
	temp := t.TempDir()
	recipePath := filepath.Join(temp, "recipe.json")
	recipe := kconfig.CompactObjectVariant{
		ContentID: strings.Repeat("a", 64),
		Object:    "selected.o",
		Source:    "selected.c",
		Mode:      "y",
		Flags:     []string{"-DRECIPE_REPLAYED=1"},
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	compiler := filepath.Join(temp, "compiler")
	log := filepath.Join(temp, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECIPE_TEST_LOG\"\n"
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECIPE_TEST_LOG", log)
	err = runRecipe(recipeOptions{
		recipe: recipePath, kind: "compile", tool: compiler,
		source: filepath.Join(temp, "selected.c"), output: filepath.Join(temp, "selected.o"),
		expectedSource: "selected.c", expectedObject: "selected.o", expectedID: recipe.ContentID,
		actionArgs: []string{
			"--target=x86_64-linux-gnu",
			kbuildArgsSentinel,
		},
	})
	if err != nil {
		t.Fatalf("runRecipe() failed: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "--target=x86_64-linux-gnu\n-DRECIPE_REPLAYED=1\n-c\n" + filepath.Join(temp, "selected.c") + "\n-o\n" + filepath.Join(temp, "selected.o") + "\n"
	if string(got) != want {
		t.Fatalf("compiler argv = %q, want %q", got, want)
	}
}

func TestRunRecipeRejectsPlanMismatch(t *testing.T) {
	temp := t.TempDir()
	recipePath := filepath.Join(temp, "recipe.json")
	recipe := kconfig.CompactObjectVariant{
		ContentID: strings.Repeat("a", 64), Object: "selected.o", Source: "selected.c", Mode: "y",
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err = runRecipe(recipeOptions{
		recipe: recipePath, kind: "compile", tool: "/unused", source: "/unused", output: "/unused",
		expectedSource: "other.c", expectedObject: recipe.Object, expectedID: recipe.ContentID,
	})
	if err == nil || !strings.Contains(err.Error(), "recipe source") {
		t.Fatalf("runRecipe() error = %v, want source mismatch", err)
	}
}

func TestRunRecipeLinksCompositeMembers(t *testing.T) {
	temp := t.TempDir()
	recipePath := filepath.Join(temp, "recipe.json")
	recipe := kconfig.CompactObjectVariant{
		ContentID: strings.Repeat("b", 64),
		Object:    "composite.o",
		Mode:      "y",
		Members:   []string{"first", "second"},
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	linker := filepath.Join(temp, "linker")
	log := filepath.Join(temp, "argv")
	if err := os.WriteFile(linker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RECIPE_TEST_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECIPE_TEST_LOG", log)
	output := filepath.Join(temp, "composite.o")
	inputs := []string{filepath.Join(temp, "first.o"), filepath.Join(temp, "second.o")}
	err = runRecipe(recipeOptions{
		recipe: recipePath, kind: "composite", tool: linker,
		inputs: inputs, output: output,
		expectedObject: recipe.Object, expectedID: recipe.ContentID,
		actionArgs: []string{"--prefix", kbuildArgsSentinel, "--suffix"},
	})
	if err != nil {
		t.Fatalf("runRecipe() failed: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "--prefix\n-r\n-o\n" + output + "\n" + strings.Join(inputs, "\n") + "\n--suffix\n"
	if string(got) != want {
		t.Fatalf("linker argv = %q, want %q", got, want)
	}
}
