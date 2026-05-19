// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"context"
	"strings"
	"testing"
)

func TestParseBuildsMenuAndSymbols(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
mainmenu "Example"

config MODULES
	bool "Enable modules"
	modules

menu "Networking"
	depends on MODULES

config NET
	tristate "Networking support" if MODULES
	default y
	select CRYPTO if MODULES
	help
	  Enables networking.

comment "Drivers"
	depends on NET
endmenu
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	modules := tree.Symbols["MODULES"]
	if modules == nil || modules.Type != SymbolBool {
		t.Fatalf("MODULES = %#v, want bool symbol", modules)
	}
	net := tree.Symbols["NET"]
	if net == nil || net.Type != SymbolTristate {
		t.Fatalf("NET = %#v, want tristate symbol", net)
	}
	crypto := tree.Symbols["CRYPTO"]
	if crypto == nil || crypto.RevDep == nil {
		t.Fatalf("CRYPTO rev_dep = %v, want select dependency", exprString(crypto.RevDep))
	}
	if got, want := tree.Root.Prompt.Text, "Example"; got != want {
		t.Fatalf("root prompt = %q, want %q", got, want)
	}
	if len(tree.Root.Children) != 1 || tree.Root.Children[0].Symbol != modules {
		t.Fatalf("root children = %#v, want MODULES submenu root", tree.Root.Children)
	}
	if got := tree.Root.Children[0].Children; len(got) != 1 || got[0].Prompt.Text != "Networking" {
		t.Fatalf("MODULES children = %#v, want Networking menu", got)
	}
}

func TestParseChoice(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
choice
	prompt "Pick one"
	default FOO

config FOO
	bool "Foo"

config BAR
	bool "Bar"
endchoice
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if len(tree.Root.Children) != 1 {
		t.Fatalf("root children = %d, want 1", len(tree.Root.Children))
	}
	choice := tree.Root.Children[0]
	if choice.Type != MenuChoice {
		t.Fatalf("menu type = %q, want choice", choice.Type)
	}
	got := choice.Symbol.ChoiceMembers
	if len(got) != 2 || got[0].Name != "FOO" || got[1].Name != "BAR" {
		t.Fatalf("choice members = %#v, want FOO/BAR", got)
	}
}

func TestParseChoiceTypePrompt(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
choice
	bool "Pick one"
	default FOO

config FOO
	bool "Foo"

endchoice
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	choice := tree.Root.Children[0]
	if got, want := choice.Symbol.Type, SymbolBool; got != want {
		t.Fatalf("choice type = %q, want %q", got, want)
	}
	if choice.Prompt == nil || choice.Prompt.Text != "Pick one" {
		t.Fatalf("choice prompt = %#v, want Pick one", choice.Prompt)
	}
}

func TestParsePromptlessChoice(t *testing.T) {
	_, err := Parse(context.Background(), strings.NewReader(`
choice
	bool
	optional

config FOO
	bool "Foo"

endchoice
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
}

func TestParseTransitionalSymbols(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config OLD
	bool
	transitional
	help
	  Migration-only value.

config NEW
	bool "New"
	default OLD
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if old := tree.Symbols["OLD"]; old == nil || !old.Transitional {
		t.Fatalf("OLD transitional = %#v, want transitional symbol", old)
	}
}

func TestParseRejectsInvalidTransitionalSymbols(t *testing.T) {
	for name, input := range map[string]string{
		"default": `
config BAD
	bool
	transitional
	default y
`,
		"depends": `
config BAD
	bool
	transitional
	depends on OTHER
`,
		"prompt": `
config BAD
	bool "Bad"
	transitional
`,
		"no_type": `
config BAD
	transitional
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(context.Background(), strings.NewReader(input), "Kconfig", Options{})
			if err == nil {
				t.Fatalf("Parse() succeeded for invalid transitional symbol")
			}
		})
	}
}

func TestParseSourceIncludesFile(t *testing.T) {
	tree, err := ParseFile(context.Background(), "testdata/include/Kconfig", Options{})
	if err != nil {
		t.Fatalf("ParseFile() failed: %v", err)
	}
	if tree.Symbols["INCLUDED"] == nil {
		t.Fatalf("INCLUDED symbol missing after source include")
	}
	if len(tree.Sources) != 1 || tree.Sources[0].Path != "Kconfig.child" {
		t.Fatalf("sources = %#v, want Kconfig.child", tree.Sources)
	}
}

func TestParseRejectsRecursiveInclude(t *testing.T) {
	_, err := ParseFile(context.Background(), "testdata/include/Kconfig.recursive", Options{})
	if err == nil {
		t.Fatal("ParseFile() succeeded on recursive include")
	}
}

func TestParseRejectsShellByDefault(t *testing.T) {
	_, err := Parse(context.Background(), strings.NewReader(`
value := $(shell,echo y)
`), "Kconfig", Options{})
	if err == nil {
		t.Fatal("Parse() succeeded with $(shell,...) in hermetic mode")
	}
}
