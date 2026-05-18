// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

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

func TestPreprocessorShellDoesNotInheritHostEnvironmentByDefault(t *testing.T) {
	t.Setenv("KCONFIG_HOST_ONLY_VALUE", "leaked")
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_SHELL
	string
	default "$(shell,printf %s \"${KCONFIG_HOST_ONLY_VALUE-unset}\")"
`), "Kconfig", Options{
		AllowShell: true,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"unset"` {
		t.Fatalf("default expr = %#v, want quoted unset", defaults)
	}
}

func TestPreprocessorShellUsesHermeticEnvironment(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_SHELL
	string
	default "$(shell,printf %s \"$KCONFIG_VALUE\")"
`), "Kconfig", Options{
		AllowShell: true,
		Env: map[string]string{
			"KCONFIG_VALUE": "hermetic",
		},
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"hermetic"` {
		t.Fatalf("default expr = %#v, want quoted hermetic", defaults)
	}
}

func TestPreprocessorShellCanOptIntoHostEnvironment(t *testing.T) {
	t.Setenv("KCONFIG_HOST_VALUE", "host")
	tree, err := Parse(context.Background(), strings.NewReader(`
config FROM_SHELL
	string
	default "$(shell,printf %s \"$KCONFIG_HOST_VALUE\")"
`), "Kconfig", Options{
		AllowShell: true,
		UseHostEnv: true,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	defaults := propertiesOfType(tree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"host"` {
		t.Fatalf("default expr = %#v, want quoted host", defaults)
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
