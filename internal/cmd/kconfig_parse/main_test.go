package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

func TestCompactMetadataV7EmitsSelectedProtocol(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"Kconfig":                          "mainmenu \"compact-v7 CLI test\"\n",
		"Kbuild":                           "obj-y += init.o\nccflags-y += $(call cc-option,-fprofile-supported)\n",
		"base.config":                      "",
		"arch/x86/kernel/vmlinux.lds.S":    "\n",
		"init.c":                           "int init_value;\n",
		"include/linux/compiler-version.h": "\n",
		"include/linux/compiler_types.h":   "\n",
		"include/linux/kconfig.h":          "\n",
	} {
		writeKconfigParseTestFile(t, root, path, content)
	}
	const profileID = "test-graph-profile"

	kconfigPath := filepath.Join(root, "Kconfig")
	tree, err := kconfig.ParseFile(t.Context(), kconfigPath, kconfig.Options{
		RootDir: root,
	})
	if err != nil {
		t.Fatalf("ParseFile() failed: %v", err)
	}
	metadata, err := compactMetadataV7(
		tree,
		kconfigPath,
		filepath.Join(root, "Kbuild"),
		[]namedPath{{
			Name: "base",
			Path: filepath.Join(root, "base.config"),
		}},
		"default",
		false,
		map[string]string{
			"ARCH":    "x86",
			"SRCARCH": "x86",
		},
		nil,
		map[string]string{
			"base": "//headers:base",
		},
		"6.18.2",
		"linux.bzl/test/x86",
		profileID,
		nil,
	)
	if err != nil {
		t.Fatalf("compactMetadataV7() failed: %v", err)
	}
	if got, want := metadata.Protocol, kconfig.CompactMetadataProtocolV7; got != want {
		t.Fatalf("protocol = %q, want %q", got, want)
	}
	if got, want := metadata.ToolchainProfile, profileID; got != want {
		t.Fatalf("toolchain profile = %q, want %q", got, want)
	}

	data, err := metadata.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() failed: %v", err)
	}
	if got, want := decoded["protocol"], kconfig.CompactMetadataProtocolV7; got != want {
		t.Fatalf("JSON protocol = %#v, want %q", got, want)
	}
	if _, ok := decoded["buildfile"]; ok {
		t.Fatalf("compact-v7 JSON contains a legacy buildfile field: %s", data)
	}
}

func TestLinuxProbeShellUsesExplicitModelOverrides(t *testing.T) {
	shell, err := linuxProbeShell(
		kconfig.LinuxProbeModelLLVM,
		map[string]string{
			"bindgen_version": "bindgen 0.73.0",
			"cc_version":      "210002",
			"cc_version_text": "clang version 21.0.2",
			"ld_version":      "210002",
			"pahole_version":  "140",
			"rustc_version":   "109900",
		},
	)
	if err != nil {
		t.Fatalf("linuxProbeShell() failed: %v", err)
	}
	for _, test := range []struct {
		command string
		want    string
	}{
		{
			command: "/src/scripts/cc-version.sh clang",
			want:    "Clang 210002",
		},
		{
			command: "/src/scripts/ld-version.sh ld.lld",
			want:    "LLD 210002",
		},
		{
			command: "/src/scripts/pahole-version.sh pahole",
			want:    "140",
		},
		{
			command: "bindgen --version workaround-for-0.69.0 2>/dev/null",
			want:    "bindgen 0.73.0",
		},
		{
			command: "/src/scripts/rustc-version.sh rustc",
			want:    "109900",
		},
	} {
		got, err := shell(t.Context(), test.command)
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", test.command, err)
		}
		if got != test.want {
			t.Errorf("shell(%q) = %q, want %q", test.command, got, test.want)
		}
	}

}

func TestLoadGraphProfileDecodesCheckedInInput(t *testing.T) {
	profile := ccprofile.GraphProfile{
		Schema:         ccprofile.GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands:   []ccprofile.KconfigCommand{},
		KbuildGraphProbes: []ccprofile.KbuildGraphProbe{},
	}
	data, err := ccprofile.CanonicalGraphProfileJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalGraphProfileJSON() failed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "graph_profile.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	loaded, err := loadGraphProfile(path)
	if err != nil {
		t.Fatalf("loadGraphProfile() failed: %v", err)
	}
	if err := ccprofile.CompareGraphProfiles(profile, *loaded); err != nil {
		t.Fatalf("loaded profile mismatch: %v", err)
	}
}

func writeKconfigParseTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) failed: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) failed: %v", path, err)
	}
}

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
