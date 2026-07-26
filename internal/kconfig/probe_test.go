// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxLLVMProbeProfile(t *testing.T) {
	config, err := LinuxProbeConfigForModel(LinuxProbeModelLLVM)
	if err != nil {
		t.Fatalf("LinuxProbeConfigForModel() failed: %v", err)
	}
	if config.CCVersion != 220108 || config.LDVersion != 220108 {
		t.Fatalf("LLVM probe versions = clang %d, lld %d; want 220108", config.CCVersion, config.LDVersion)
	}
	if config.RustcVersion != 109700 || config.RustcLLVMVersion != 220106 {
		t.Fatalf(
			"Rust probe versions = rustc %d, LLVM %d; want 109700 and 220106",
			config.RustcVersion,
			config.RustcLLVMVersion,
		)
	}
	if config.PaholeVersion != 131 || config.BindgenVersion != "bindgen 0.72.1" {
		t.Fatalf(
			"host probes = pahole %d, %q; want 131 and bindgen 0.72.1",
			config.PaholeVersion,
			config.BindgenVersion,
		)
	}
	if !config.RustAvailable || !config.RustOptions {
		t.Fatalf(
			"Rust probe support = available %t, options %t; want both true",
			config.RustAvailable,
			config.RustOptions,
		)
	}
}

func TestLinuxLLVMProbeShellSupportsKconfigIncludeProbes(t *testing.T) {
	rootDir := t.TempDir()
	scriptsDir := filepath.Join(rootDir, "scripts")
	if err := os.Mkdir(scriptsDir, 0o755); err != nil {
		t.Fatalf("Mkdir() failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "Kconfig.include"), []byte(`
comma := ,
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
failure = $(if-success,$(1),n,y)

$(error-if,$(failure,command -v $(CC)),C compiler '$(CC)' not found)
$(error-if,$(failure,command -v $(LD)),linker '$(LD)' not found)

cc-info := $(shell,$(srctree)/scripts/cc-version.sh $(CC))
$(error-if,$(success,test -z "$(cc-info)"),Sorry$(comma) this C compiler is not supported.)
cc-name := $(shell,set -- $(cc-info) && echo $1)
cc-version := $(shell,set -- $(cc-info) && echo $2)

as-info := $(shell,$(srctree)/scripts/as-version.sh $(CC) $(CLANG_FLAGS))
$(error-if,$(success,test -z "$(as-info)"),Sorry$(comma) this assembler is not supported.)
as-name := $(shell,set -- $(as-info) && echo $1)
as-version := $(shell,set -- $(as-info) && echo $2)

ld-info := $(shell,$(srctree)/scripts/ld-version.sh $(LD))
$(error-if,$(success,test -z "$(ld-info)"),Sorry$(comma) this linker is not supported.)
ld-name := $(shell,set -- $(ld-info) && echo $1)
ld-version := $(shell,set -- $(ld-info) && echo $2)

config CC_IS_GCC
	def_bool $(success,test "$(cc-name)" = GCC)

config CC_IS_CLANG
	def_bool $(success,test "$(cc-name)" = Clang)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	shell, err := LinuxProbeShell(LinuxProbeModelLLVM)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	tree, err := Parse(context.Background(), strings.NewReader(`
srctree := `+rootDir+`
CC := clang
LD := ld.lld
CLANG_FLAGS := -fintegrated-as

source "scripts/Kconfig.include"

config CC_IS_CLANG_TEST
	bool
	default y if CC_IS_CLANG

config CC_IS_GCC_TEST
	bool
	default y if CC_IS_GCC

config LLD_MAJOR
	int
	default $(shell,expr $(ld-version) / 10000)

config PAHOLE_VERSION
	int
	default $(shell,$(srctree)/scripts/pahole-version.sh $(PAHOLE))

config BINDGEN_VERSION_TEXT
	string
	default "$(shell,$(BINDGEN) --version workaround-for-0.69.0 2>/dev/null)"
`), "Kconfig", Options{
		AllowShell: true,
		RootDir:    rootDir,
		Env: map[string]string{
			"BINDGEN": "bindgen",
			"PAHOLE":  "pahole",
		},
		Shell: shell,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfig("test", nil)
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	if got := resolved.Value("CONFIG_CC_IS_CLANG"); got != "y" {
		t.Fatalf("CONFIG_CC_IS_CLANG = %q, want y", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_GCC"); got != "n" {
		t.Fatalf("CONFIG_CC_IS_GCC = %q, want n", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_CLANG_TEST"); got != "y" {
		t.Fatalf("CONFIG_CC_IS_CLANG_TEST = %q, want y", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_GCC_TEST"); got != "n" {
		t.Fatalf("CONFIG_CC_IS_GCC_TEST = %q, want n", got)
	}
	if got := resolved.Value("CONFIG_LLD_MAJOR"); got != "22" {
		t.Fatalf("CONFIG_LLD_MAJOR = %q, want 22", got)
	}
	if got := resolved.Value("CONFIG_PAHOLE_VERSION"); got != "131" {
		t.Fatalf("CONFIG_PAHOLE_VERSION = %q, want 131", got)
	}
	if got := resolved.Value("CONFIG_BINDGEN_VERSION_TEXT"); got != "\"bindgen 0.72.1\"" {
		t.Fatalf("CONFIG_BINDGEN_VERSION_TEXT = %q, want bindgen 0.72.1", got)
	}
}

func TestLinuxLLVMProbeShellSupportsLLVMNmAndArProbes(t *testing.T) {
	shell, err := LinuxProbeShell(LinuxProbeModelLLVM)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	for _, command := range []string{
		"llvm-nm --help | head -n 1 | grep -qi llvm",
		"llvm-ar --help | head -n 1 | grep -qi llvm",
	} {
		out, err := shell(context.Background(), "{ "+command+"; } >/dev/null 2>&1 && echo \"y\" || echo \"n\"")
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", command, err)
		}
		if got := strings.TrimSpace(out); got != "y" {
			t.Fatalf("shell(%q) = %q, want y", command, got)
		}
	}
}

func TestLinuxLLVMProbeShellSupportsCCOptionInt128Probe(t *testing.T) {
	rootDir := t.TempDir()
	scriptsDir := filepath.Join(rootDir, "scripts")
	if err := os.Mkdir(scriptsDir, 0o755); err != nil {
		t.Fatalf("Mkdir() failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "Kconfig.include"), []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
cc-option = $(success,trap "rm -rf .tmp_$$" EXIT; mkdir .tmp_$$; $(CC) -Werror $(CLANG_FLAGS) $(1) -c -x c /dev/null -o .tmp_$$/tmp.o)
cc-option-bit = $(if-success,$(CC) -Werror $(1) -E -x c /dev/null -o /dev/null,$(1))
m64-flag := $(cc-option-bit,-m64)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	var commands []string
	baseShell, err := LinuxProbeShell(LinuxProbeModelLLVM)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	shell := func(ctx context.Context, command string) (string, error) {
		commands = append(commands, command)
		return baseShell(ctx, command)
	}
	tree, err := Parse(context.Background(), strings.NewReader(`
srctree := `+rootDir+`
CC := clang
CLANG_FLAGS := -fintegrated-as

source "scripts/Kconfig.include"

config 64BIT
	def_bool y

config CC_HAS_INT128
	def_bool !$(cc-option,$(m64-flag) -D__SIZEOF_INT128__=0) && 64BIT
`), "Kconfig", Options{
		AllowShell: true,
		RootDir:    rootDir,
		Shell:      shell,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfig("test", nil)
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	if got := resolved.Value("CONFIG_CC_HAS_INT128"); got != "y" {
		t.Fatalf("CONFIG_CC_HAS_INT128 = %q, want y; commands=%q", got, commands)
	}
}

func TestLinuxProbeShellSupportsValueOverrides(t *testing.T) {
	config, err := LinuxProbeConfigForModel(LinuxProbeModelLLVM)
	if err != nil {
		t.Fatalf("LinuxProbeConfigForModel() failed: %v", err)
	}
	for key, value := range map[string]string{
		"bindgen_version": "bindgen 0.71.1",
		"ld_version":      "210000",
		"pahole_version":  "124",
		"rust_available":  "yes",
	} {
		if err := ApplyLinuxProbeValue(&config, key, value); err != nil {
			t.Fatalf("ApplyLinuxProbeValue(%q, %q) failed: %v", key, value, err)
		}
	}

	shell := LinuxProbeShellFromConfig(config)
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "linker version",
			command: "/src/scripts/ld-version.sh ld.lld",
			want:    "LLD 210000",
		},
		{
			name:    "pahole version",
			command: "/src/scripts/pahole-version.sh pahole",
			want:    "124",
		},
		{
			name:    "bindgen version",
			command: "bindgen --version workaround-for-0.69.0 2>/dev/null",
			want:    "bindgen 0.71.1",
		},
		{
			name:    "rust availability",
			command: `{ /src/scripts/rust_is_available.sh rustc; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "generic compiler option",
			command: `{ clang -Werror -fno-stack-protector -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "compiler smoke test",
			command: `{ echo 'void foo(void) { asm inline (""); }' | clang -x c - -c -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "compiler option with trailing Werror",
			command: `{ echo '__attribute__((no_profile_instrument_function)) int x();' | clang -x c - -c -o /dev/null -Werror; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "builtin macro redefined",
			command: `{ clang -Werror -D__SIZEOF_INT128__=0 -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "n",
		},
		{
			name:    "builtin macro redefined to same value",
			command: `{ clang -Werror -D__SIZEOF_INT128__=16 -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := shell(context.Background(), test.command)
			if err != nil {
				t.Fatalf("shell(%q) failed: %v", test.command, err)
			}
			if got != test.want {
				t.Fatalf("shell(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}
