package kconfig

import (
	"context"
	"strings"
	"testing"
)

func TestPreprocessorVariablesAndFunctions(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
name = WORLD
prompt := Hello $(name)
wrap = $(1)-$(2)

config EXAMPLE
	bool "$(prompt)"
	default "$(wrap,a,b)"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	menu := tree.Root.Children[0]
	if got, want := menu.Prompt.Text, "Hello WORLD"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	defaults := propertiesOfType(menu, PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"a-b"` {
		t.Fatalf("default expr = %#v, want quoted a-b", defaults)
	}
}

func TestPreprocessorUsesHermeticEnvironment(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_ENV
	string
	default "$(VALUE)"
`), "Kconfig", Options{
		Env: map[string]string{"VALUE": "hermetic"},
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"hermetic"` {
		t.Fatalf("default expr = %#v, want quoted hermetic", defaults)
	}
}

func TestPreprocessorShellRequiresExplicitHermeticEvaluator(t *testing.T) {
	_, err := Parse(context.Background(), strings.NewReader(`
config FROM_SHELL
	string
	default "$(shell,probe)"
`), "Kconfig", Options{})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit hermetic evaluator") {
		t.Fatalf("Parse() error = %v, want missing hermetic evaluator error", err)
	}
}

func TestPreprocessorShellUsesExplicitHermeticEvaluator(t *testing.T) {
	var command string
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_SHELL
	string
	default "$(shell,probe $(KCONFIG_VALUE))"
`), "Kconfig", Options{
		Env: map[string]string{
			"KCONFIG_VALUE": "hermetic",
		},
		Shell: func(_ context.Context, got string) (string, error) {
			command = got
			return "measured\nvalue\n", nil
		},
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if got, want := command, "probe hermetic"; got != want {
		t.Fatalf("shell command = %q, want %q", got, want)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"measured value"` {
		t.Fatalf("default expr = %#v, want normalized evaluator output", defaults)
	}
}

func TestPreprocessorDoesNotReadHostEnvironment(t *testing.T) {
	t.Setenv("KCONFIG_HOST_ONLY_VALUE", "leaked")
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_ENV
	string
	default "$(KCONFIG_HOST_ONLY_VALUE)"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `""` {
		t.Fatalf("default expr = %#v, want empty host-independent value", defaults)
	}
}

func propertiesOfType(menu *Menu, typ PropertyType) []*Property {
	var out []*Property
	for _, prop := range menu.Properties {
		if prop.Type == typ {
			out = append(out, prop)
		}
	}
	return out
}
