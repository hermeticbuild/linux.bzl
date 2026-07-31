package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

func TestApplyRustToolchainProbe(t *testing.T) {
	vars := stringMapFlag{"RUSTC_VERSION_TEXT": "repository baseline"}
	rustcVersion, rustcLLVMVersion := applyRustToolchainProbe(vars, rusttoolchain.Probe{
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	})
	if got, want := vars["RUSTC_VERSION_TEXT"], "rustc 1.98.0-nightly (012345678 2026-06-24)"; got != want {
		t.Fatalf("RUSTC_VERSION_TEXT = %q, want %q", got, want)
	}
	if got, want := rustcVersion, 109800; got != want {
		t.Fatalf("rustc version = %d, want %d", got, want)
	}
	if got, want := rustcLLVMVersion, 220107; got != want {
		t.Fatalf("Rust LLVM version = %d, want %d", got, want)
	}
}

func TestFixedLinuxProbeShellOwnsToolEnvironment(t *testing.T) {
	env := stringMapFlag{"ARCH": "arm64"}
	shell, err := fixedLinuxProbeShell(
		"arm64",
		kconfig.LinuxProbeDefaultRustcVersion,
		kconfig.LinuxProbeDefaultRustcLLVMVersion,
		env,
	)
	if err != nil {
		t.Fatalf("fixedLinuxProbeShell() failed: %v", err)
	}
	if shell == nil {
		t.Fatal("fixedLinuxProbeShell() returned nil shell")
	}
	for name, want := range fixedLinuxProbeEnvironment {
		if got := env[name]; got != want {
			t.Fatalf("env[%q] = %q, want %q", name, got, want)
		}
	}
}

func TestFixedLinuxProbeShellRejectsToolEnvironmentOverride(t *testing.T) {
	_, err := fixedLinuxProbeShell(
		"x86",
		kconfig.LinuxProbeDefaultRustcVersion,
		kconfig.LinuxProbeDefaultRustcLLVMVersion,
		stringMapFlag{"CC": "gcc"},
	)
	if err == nil || !strings.Contains(err.Error(), `CC="clang"`) {
		t.Fatalf("fixedLinuxProbeShell() error = %v, want fixed CC error", err)
	}
}

func TestStartRuntimeProfilesWritesMemoryProfiles(t *testing.T) {
	dir := t.TempDir()
	heapPath := filepath.Join(dir, "heap.pprof")
	allocsPath := filepath.Join(dir, "allocs.pprof")
	stop, err := startRuntimeProfiles("", heapPath, allocsPath)
	if err != nil {
		t.Fatalf("startRuntimeProfiles() failed: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop profiles failed: %v", err)
	}
	for _, path := range []string{heapPath, allocsPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q) failed: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("profile %q is empty", path)
		}
	}
}

func TestStartRuntimeProfilesRejectsInvalidCPUPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "cpu.pprof")
	if _, err := startRuntimeProfiles(path, "", ""); err == nil {
		t.Fatalf("startRuntimeProfiles(%q) succeeded, want an error", path)
	}
}

func TestKbuildVariablesForConfigUsesWrittenConfigView(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config DISABLED
	bool
config HIDDEN
	bool
config MODULE
	tristate
config WRITTEN
	bool
config STRING
	string
config EMPTY
	string
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	vars := kbuildVariablesForConfig(
		map[string]string{
			"ARCH":        "arm64",
			"CONFIG_BASE": "base",
		},
		tree,
		&kconfig.ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_DISABLED": "n",
				"CONFIG_EMPTY":    `""`,
				"CONFIG_HIDDEN":   "y",
				"CONFIG_MODULE":   "m",
				"CONFIG_STRING":   `"one two"`,
				"CONFIG_WRITTEN":  "y",
			},
			Written: map[string]bool{
				"CONFIG_EMPTY":   true,
				"CONFIG_MODULE":  true,
				"CONFIG_STRING":  true,
				"CONFIG_WRITTEN": true,
			},
		},
	)

	for key, want := range map[string]string{
		"ARCH":            "arm64",
		"CONFIG_BASE":     "base",
		"CONFIG_DISABLED": "",
		"CONFIG_EMPTY":    "",
		"CONFIG_HIDDEN":   "",
		"CONFIG_MODULE":   "m",
		"CONFIG_STRING":   "one two",
		"CONFIG_WRITTEN":  "y",
		"comma":           ",",
	} {
		if got := vars[key]; got != want {
			t.Fatalf("vars[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestKbuildVariablesForConfigDoesNotInventEmptyFirmwareObject(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config EXTRA_FIRMWARE
	string "External firmware"
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{"CONFIG_EXTRA_FIRMWARE": `""`},
		Written:   map[string]bool{"CONFIG_EXTRA_FIRMWARE": true},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`firmware := $(addsuffix .gen.o, $(CONFIG_EXTRA_FIRMWARE))
obj-y += $(firmware)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := kconfig.ParseKbuildFileWithOptions(path, kconfig.KbuildOptions{
		Variables: kbuildVariablesForConfig(nil, tree, resolved),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kb.Objects) != 0 {
		t.Fatalf("empty CONFIG_EXTRA_FIRMWARE produced objects: %#v", kb.Objects)
	}
}

func TestWriteResolvedConfigOutputsUsesAutoConfStringEncoding(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config ENABLED
	bool
config EXTRA_FIRMWARE
	string
config FIRMWARE_LIST
	string
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outputs := resolvedConfigOutputs{
		config:        filepath.Join(dir, ".config"),
		autoConf:      filepath.Join(dir, "auto.conf"),
		autoConfCmd:   filepath.Join(dir, "auto.conf.cmd"),
		autoconf:      filepath.Join(dir, "autoconf.h"),
		rustcCfg:      filepath.Join(dir, "rustc_cfg"),
		kernelRelease: filepath.Join(dir, "kernel.release"),
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_ENABLED":        "y",
			"CONFIG_EXTRA_FIRMWARE": `""`,
			"CONFIG_FIRMWARE_LIST":  `"one.bin two.bin"`,
		},
		Written: map[string]bool{
			"CONFIG_ENABLED":        true,
			"CONFIG_EXTRA_FIRMWARE": true,
			"CONFIG_FIRMWARE_LIST":  true,
		},
	}
	if err := writeResolvedConfigOutputs(tree, resolved, outputs, "6.18.39"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		outputs.config:   "CONFIG_ENABLED=y\nCONFIG_EXTRA_FIRMWARE=\"\"\nCONFIG_FIRMWARE_LIST=\"one.bin two.bin\"\n",
		outputs.autoConf: "CONFIG_ENABLED=y\nCONFIG_EXTRA_FIRMWARE=\nCONFIG_FIRMWARE_LIST=one.bin two.bin\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s =\n%s\nwant:\n%s", filepath.Base(path), got, want)
		}
	}
}

func TestRustcCfgLinesMatchKernelEncoding(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config BOOL
	bool
config TRI
	tristate
config STR
	string
config EMPTY
	string
config INT
	int
config HEX
	hex
config HEX_PREFIXED
	hex
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_BOOL":         "y",
			"CONFIG_EMPTY":        "",
			"CONFIG_TRI":          "m",
			"CONFIG_STR":          `"quoted \"value\""`,
			"CONFIG_INT":          "42",
			"CONFIG_HEX":          "2a",
			"CONFIG_HEX_PREFIXED": "0X2A",
		},
		Written: map[string]bool{
			"CONFIG_BOOL":         true,
			"CONFIG_EMPTY":        true,
			"CONFIG_TRI":          true,
			"CONFIG_STR":          true,
			"CONFIG_INT":          true,
			"CONFIG_HEX":          true,
			"CONFIG_HEX_PREFIXED": true,
		},
	}
	got := strings.Join(rustcCfgLines(tree, resolved), "\n")
	want := strings.Join([]string{
		`--cfg=CONFIG_BOOL`,
		`--cfg=CONFIG_BOOL="y"`,
		`--cfg=CONFIG_EMPTY=""`,
		`--cfg=CONFIG_HEX="0x2a"`,
		`--cfg=CONFIG_HEX_PREFIXED="0X2A"`,
		`--cfg=CONFIG_INT="42"`,
		`--cfg=CONFIG_STR="quoted \"value\""`,
		`--cfg=CONFIG_TRI`,
		`--cfg=CONFIG_TRI="m"`,
	}, "\n")
	if got != want {
		t.Fatalf("rustcCfgLines() =\n%s\nwant:\n%s", got, want)
	}
}
