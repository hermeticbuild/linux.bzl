package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

func TestLoadGraphProfileProjectionDecodesConsumedInput(t *testing.T) {
	root := t.TempDir()
	script := []byte("#!/bin/sh\necho Clang 220108\n")
	writeKconfigParseTestFile(t, root, "scripts/cc-version.sh", string(script))
	stdout := "Clang 220108"
	command := ccprofile.KconfigCommand{
		Kind:    ccprofile.KconfigCommandKindStdout,
		Command: ccprofile.GraphProfileSourceRoot + "/scripts/cc-version.sh clang",
		Environment: map[string]string{
			"CC":      "clang",
			"OBJCOPY": "llvm-objcopy",
			"PYTHON3": "python3",
		},
		Inputs: map[string]string{
			"scripts/cc-version.sh": fmt.Sprintf("%x", sha256.Sum256(script)),
		},
		Stdout: &stdout,
	}
	command.ID = ccprofile.KconfigCommandID(command)
	projection := ccprofile.GraphProjection{
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands:   []ccprofile.KconfigCommand{command},
		KbuildGraphProbes: []ccprofile.KbuildGraphProbe{},
	}
	data, err := ccprofile.CanonicalGraphProjectionJSON(projection)
	if err != nil {
		t.Fatalf("CanonicalGraphProjectionJSON() failed: %v", err)
	}
	path := filepath.Join(root, "graph_projection.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	loaded, err := loadGraphProfileProjection(path)
	if err != nil {
		t.Fatalf("loadGraphProfileProjection() failed: %v", err)
	}
	if got, want := loaded.Schema, ccprofile.GraphProfileSchema; got != want {
		t.Fatalf("loaded schema = %q, want %q", got, want)
	}
	if got, want := loaded.Identity(), projection.Identity(); got != want {
		t.Fatalf("loaded identity = %#v, want %#v", got, want)
	}
	shell, err := kconfig.NewGraphProfileShell(
		*loaded,
		root,
		command.Environment,
	)
	if err != nil {
		t.Fatalf("NewGraphProfileShell() failed: %v", err)
	}
	got, err := shell.Run(
		t.Context(),
		filepath.Join(root, "scripts", "cc-version.sh")+" clang",
	)
	if err != nil {
		t.Fatalf("projection shell lookup failed: %v", err)
	}
	if got != stdout {
		t.Fatalf("projection shell lookup = %q, want %q", got, stdout)
	}
}

func TestCombineGraphProfileAndRustProbeShells(t *testing.T) {
	var graphCommands []string
	var rustCommands []kconfig.RustToolchainVersionCommand
	graphShell := func(_ context.Context, command string) (string, error) {
		graphCommands = append(graphCommands, command)
		return "graph", nil
	}
	rustResolver := func(
		_ context.Context,
		command kconfig.RustToolchainVersionCommand,
	) (string, error) {
		rustCommands = append(rustCommands, command)
		return "rust", nil
	}
	shell, err := combineGraphProfileAndRustProbeShells(graphShell, rustResolver)
	if err != nil {
		t.Fatalf("combineGraphProfileAndRustProbeShells() failed: %v", err)
	}
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "/src/scripts/cc-can-link.sh clang", want: "graph"},
		{command: "bindgen --version workaround-for-0.69.0 2>/dev/null", want: "graph"},
		{command: `{ /src/scripts/rust_is_available.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "graph"},
		{command: `{ rustc -Copt-level=2 --crate-type=rlib /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "graph"},
		{command: "echo /src/scripts/rustc-version.sh", want: "graph"},
		{command: "/src/scripts/rustc-version.sh-old rustc", want: "graph"},
		{command: `{ /src/scripts/rustc-version.sh rustc; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "graph"},
		{command: "/src/scripts/rustc-version.sh rustc", want: "rust"},
		{command: "/src/scripts/rustc-llvm-version.sh rustc", want: "rust"},
	} {
		got, err := shell(t.Context(), test.command)
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", test.command, err)
		}
		if got != test.want {
			t.Errorf("shell(%q) = %q, want %q", test.command, got, test.want)
		}
	}
	if got, want := len(graphCommands), 7; got != want {
		t.Fatalf("graph shell calls = %d, want %d: %q", got, want, graphCommands)
	}
	if got, want := len(rustCommands), 2; got != want {
		t.Fatalf("Rust shell calls = %d, want %d: %q", got, want, rustCommands)
	}
}

func TestHybridGraphProfileAndRustToolchainResolution(t *testing.T) {
	root := t.TempDir()
	script := []byte("#!/bin/sh\necho y\n")
	writeKconfigParseTestFile(t, root, "scripts/c-capability.sh", string(script))
	stdout := "y"
	command := ccprofile.KconfigCommand{
		Kind:    ccprofile.KconfigCommandKindStdout,
		Command: ccprofile.GraphProfileSourceRoot + "/scripts/c-capability.sh",
		Inputs: map[string]string{
			"scripts/c-capability.sh": fmt.Sprintf("%x", sha256.Sum256(script)),
		},
		Stdout: &stdout,
	}
	command.ID = ccprofile.KconfigCommandID(command)
	profile := ccprofile.GraphProfile{
		Schema:         ccprofile.GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: []ccprofile.KconfigCommand{command},
	}
	newHybridShell := func(profile ccprofile.GraphProfile) func(context.Context, string) (string, error) {
		t.Helper()
		graphShell, err := kconfig.NewGraphProfileShell(profile, root, nil)
		if err != nil {
			t.Fatal(err)
		}
		shell, err := combineGraphProfileAndRustProbeShells(
			graphShell.Run,
			newRustToolchainVersionResolver(rusttoolchain.Probe{
				VersionCode:     109800,
				LLVMVersionCode: 220107,
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return shell
	}
	fixture := `
srctree := ` + filepath.ToSlash(root) + `

config C_CAPABILITY
	bool
	default $(shell,$(srctree)/scripts/c-capability.sh)

config C_PROFILE_FEATURE
	bool
	default y if C_CAPABILITY

config RUSTC_VERSION
	int
	default $(shell,$(srctree)/scripts/rustc-version.sh rustc)

config RUSTC_LLVM_VERSION
	int
	default $(shell,$(srctree)/scripts/rustc-llvm-version.sh rustc)

config RUSTC_HAS_MODERN
	bool
	default y if RUSTC_VERSION >= 109800
`
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(fixture),
		"Kconfig",
		kconfig.Options{
			AllowShell: true,
			RootDir:    root,
			Shell:      newHybridShell(profile),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := tree.ResolveConfig("hybrid", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"CONFIG_C_CAPABILITY":       "y",
		"CONFIG_C_PROFILE_FEATURE":  "y",
		"CONFIG_RUSTC_VERSION":      "109800",
		"CONFIG_RUSTC_LLVM_VERSION": "220107",
		"CONFIG_RUSTC_HAS_MODERN":   "y",
	} {
		if got := resolved.Value(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if err := kconfig.ValidateRustToolchainEquivalence(
		map[string]string{
			"CONFIG_C_CAPABILITY":       "y",
			"CONFIG_C_PROFILE_FEATURE":  "y",
			"CONFIG_RUSTC_VERSION":      "107800",
			"CONFIG_RUSTC_LLVM_VERSION": "190100",
			"CONFIG_RUSTC_HAS_MODERN":   "n",
		},
		resolved,
	); err != nil {
		t.Fatalf("dynamic Rust versions changed structural config: %v", err)
	}

	profile.KconfigCommands = nil
	_, err = kconfig.Parse(
		t.Context(),
		strings.NewReader(fixture),
		"Kconfig",
		kconfig.Options{
			AllowShell: true,
			RootDir:    root,
			Shell:      newHybridShell(profile),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "missing from graph profile") {
		t.Fatalf("parse with missing C projection error = %v", err)
	}
}

func TestValidateGraphProfileMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    graphProfileMode
		wantErr string
	}{
		{
			name: "profile",
			mode: graphProfileMode{profile: "profile.json"},
		},
		{
			name: "projection config replay",
			mode: graphProfileMode{
				projection:    "projection.json",
				resolveConfig: "base=config",
			},
		},
		{
			name: "mutually exclusive inputs",
			mode: graphProfileMode{
				profile:    "profile.json",
				projection: "projection.json",
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "projection output",
			mode: graphProfileMode{
				projection:    "projection.json",
				projectionOut: "reprojected.json",
				resolveConfig: "base=config",
			},
			wantErr: "projection_out",
		},
		{
			name: "compact metadata",
			mode: graphProfileMode{
				projection:           "projection.json",
				resolveConfig:        "base=config",
				otherGraphOutputMode: true,
			},
			wantErr: "only valid for resolved config replay",
		},
		{
			name:    "missing config",
			mode:    graphProfileMode{projection: "projection.json"},
			wantErr: "only valid for resolved config replay",
		},
		{
			name: "projection with probe model",
			mode: graphProfileMode{
				projection:      "projection.json",
				resolveConfig:   "base=config",
				linuxProbeModel: "linux_llvm",
			},
			wantErr: "cannot be used with -linux_probe_model or -linux_probe_value",
		},
		{
			name: "projection with probe values",
			mode: graphProfileMode{
				projection:          "projection.json",
				resolveConfig:       "base=config",
				hasLinuxProbeValues: true,
			},
			wantErr: "cannot be used with -linux_probe_model or -linux_probe_value",
		},
		{
			name: "projection with generic shell",
			mode: graphProfileMode{
				projection:    "projection.json",
				resolveConfig: "base=config",
				allowShell:    true,
			},
			wantErr: "cannot be used with -allow_shell",
		},
		{
			name: "Rust probe without graph replay",
			mode: graphProfileMode{
				resolveConfig:      "base=config",
				rustToolchainProbe: "rust.json",
			},
			wantErr: "requires -graph_profile or -graph_profile_projection",
		},
		{
			name: "Rust probe without config replay",
			mode: graphProfileMode{
				profile:            "profile.json",
				rustToolchainProbe: "rust.json",
			},
			wantErr: "only valid for resolved config replay",
		},
		{
			name: "Rust probe while recording",
			mode: graphProfileMode{
				profile:            "profile.json",
				recordOut:          "recorded.json",
				resolveConfig:      "base=config",
				rustToolchainProbe: "rust.json",
			},
			wantErr: "cannot be used with -graph_profile_record_out",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateGraphProfileMode(test.mode)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGraphProfileMode() failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateGraphProfileMode() error = %v, want %q", err, test.wantErr)
			}
		})
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
	applyRustToolchainProbe(vars, rusttoolchain.Probe{
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	})
	if got, want := vars["RUSTC_VERSION_TEXT"], "rustc 1.98.0-nightly (012345678 2026-06-24)"; got != want {
		t.Fatalf("RUSTC_VERSION_TEXT = %q, want %q", got, want)
	}
}

func TestRustToolchainVersionResolver(t *testing.T) {
	resolve := newRustToolchainVersionResolver(rusttoolchain.Probe{
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	})
	for command, want := range map[kconfig.RustToolchainVersionCommand]string{
		kconfig.RustToolchainVersionCommandRustc: "109800",
		kconfig.RustToolchainVersionCommandLLVM:  "220107",
	} {
		got, err := resolve(t.Context(), command)
		if err != nil {
			t.Fatalf("resolve(%d) failed: %v", command, err)
		}
		if got != want {
			t.Errorf("resolve(%d) = %q, want %q", command, got, want)
		}
	}
	if _, err := resolve(
		t.Context(),
		kconfig.RustToolchainVersionCommandUnknown,
	); err == nil {
		t.Fatal("selected Rust version resolver accepted an unknown command")
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
