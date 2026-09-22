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
		t.Fatalf("CRYPTO rev_dep = %v, want select dependency", parserTestExprString(crypto))
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

func TestParseLegacyConfigOptions(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config DEFCONFIG_LIST
	string
	option defconfig_list
	default "configs/fallback"

config MODULES
	bool "Modules"
	option modules

config ENABLED_BY_ALLNOCONFIG
	bool "Enable in allnoconfig"
	option allnoconfig_y
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tree.defconfigSym != tree.Symbols["DEFCONFIG_LIST"] {
		t.Errorf("defconfig_list = %#v, want default-config symbol", tree.defconfigSym)
	}
	if tree.modulesSym != tree.Symbols["MODULES"] {
		t.Errorf("modules = %#v, want MODULES", tree.modulesSym)
	}
	for _, input := range []string{
		"config BAD\n\tbool\n\toption unrecognized\n",
		"config BAD\n\tbool\n\toption modules trailing\n",
		"config BAD\n\tbool\n\toption \"modules\"\n",
		"choice\n\toption modules\nendchoice\n",
	} {
		if _, err := Parse(context.Background(), strings.NewReader(input), "Kconfig", Options{}); err == nil {
			t.Errorf("Parse(%q) accepted unsupported option", input)
		}
	}
}

func parserTestExprString(symbol *Symbol) string {
	if symbol == nil || symbol.RevDep == nil {
		return ""
	}
	return symbol.RevDep.String()
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

func TestParseChoiceInfersTristateAndUntypedMembers(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
choice
	prompt "Enumeration method"

config BASIC
	tristate "Basic"

config OTHER
	prompt "Other"
endchoice
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	choice := tree.Root.Children[0].Symbol
	if choice.Type != SymbolTristate || tree.Symbols["OTHER"].Type != SymbolTristate {
		t.Fatalf("inferred choice type = %q, untyped member = %q; want tristate", choice.Type, tree.Symbols["OTHER"].Type)
	}
	_, err = Parse(context.Background(), strings.NewReader(`
choice
	prompt "Invalid"
config WRONG
	int "Wrong"
endchoice
`), "Kconfig", Options{})
	if err == nil || !strings.Contains(err.Error(), `choice member "WRONG" must be bool or tristate`) {
		t.Fatalf("scalar choice member error = %v, want type rejection", err)
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

func TestParseHelpKeywordAtSameIndentIsText(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config FIRMWARE
	bool "Build firmware"
	help
	This option modifies firmware
	source to the device driver.

config NEXT
	bool "Next"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Symbols["FIRMWARE"].Menus[0].Help; !strings.Contains(got, "source to the device driver.") {
		t.Fatalf("help lost same-indentation keyword: %q", got)
	}
	if tree.Symbols["NEXT"] == nil {
		t.Fatal("next source block was swallowed as help")
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
