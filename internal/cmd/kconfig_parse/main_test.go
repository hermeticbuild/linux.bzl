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
	probeValues := stringMapFlag{}
	applyRustToolchainProbe(vars, probeValues, rusttoolchain.Probe{
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	})
	if got, want := vars["RUSTC_VERSION_TEXT"], "rustc 1.98.0-nightly (012345678 2026-06-24)"; got != want {
		t.Fatalf("RUSTC_VERSION_TEXT = %q, want %q", got, want)
	}
	if got, want := probeValues["rustc_version"], "109800"; got != want {
		t.Fatalf("rustc_version = %q, want %q", got, want)
	}
	if got, want := probeValues["rustc_llvm_version"], "220107"; got != want {
		t.Fatalf("rustc_llvm_version = %q, want %q", got, want)
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
	vars := kbuildVariablesForConfig(
		map[string]string{
			"ARCH":        "arm64",
			"CONFIG_BASE": "base",
		},
		&kconfig.ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_DISABLED": "n",
				"CONFIG_HIDDEN":   "y",
				"CONFIG_MODULE":   "m",
				"CONFIG_WRITTEN":  "y",
			},
			Written: map[string]bool{
				"CONFIG_MODULE":  true,
				"CONFIG_WRITTEN": true,
			},
		},
	)

	for key, want := range map[string]string{
		"ARCH":            "arm64",
		"CONFIG_BASE":     "base",
		"CONFIG_DISABLED": "",
		"CONFIG_HIDDEN":   "",
		"CONFIG_MODULE":   "m",
		"CONFIG_WRITTEN":  "y",
	} {
		if got := vars[key]; got != want {
			t.Fatalf("vars[%q] = %q, want %q", key, got, want)
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
