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

func TestCompactToolchainProfileID(t *testing.T) {
	profile := kconfigParseTestCCProfile()
	profileDigest, err := ccprofile.Digest(*profile)
	if err != nil {
		t.Fatalf("Digest() failed: %v", err)
	}
	tests := []struct {
		name               string
		protocol           string
		toolchainProfileID string
		buildfileOut       string
		ccProfile          *ccprofile.Profile
		wantID             string
		wantError          string
	}{
		{
			name:         "v6 metadata and buildfile",
			protocol:     compactMetadataProtocolV6,
			buildfileOut: "BUILD.bazel",
		},
		{
			name:      "v6 permits CC profile without v7 identity",
			protocol:  compactMetadataProtocolV6,
			ccProfile: profile,
		},
		{
			name:               "v6 rejects toolchain profile",
			protocol:           compactMetadataProtocolV6,
			toolchainProfileID: "llvm/x86",
			wantError:          "only supported with",
		},
		{
			name:               "v7 metadata",
			protocol:           kconfig.CompactMetadataProtocolV7,
			toolchainProfileID: "llvm/x86",
			wantID:             "llvm/x86",
		},
		{
			name:      "v7 derives CC profile digest",
			protocol:  kconfig.CompactMetadataProtocolV7,
			ccProfile: profile,
			wantID:    profileDigest,
		},
		{
			name:               "v7 accepts matching CC profile digest",
			protocol:           kconfig.CompactMetadataProtocolV7,
			toolchainProfileID: profileDigest,
			ccProfile:          profile,
			wantID:             profileDigest,
		},
		{
			name:               "v7 rejects mismatching CC profile digest",
			protocol:           kconfig.CompactMetadataProtocolV7,
			toolchainProfileID: strings.Repeat("0", 64),
			ccProfile:          profile,
			wantError:          "does not match canonical -cc_profile digest",
		},
		{
			name:      "v7 without CC profile requires explicit identity",
			protocol:  kconfig.CompactMetadataProtocolV7,
			wantError: "-toolchain_profile_id is required",
		},
		{
			name:               "v7 rejects buildfile",
			protocol:           kconfig.CompactMetadataProtocolV7,
			toolchainProfileID: "llvm/x86",
			buildfileOut:       "BUILD.bazel",
			wantError:          "compact-v7 emits metadata only",
		},
		{
			name:      "unknown protocol",
			protocol:  "compact-v99",
			wantError: "unsupported -compact_protocol",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := compactToolchainProfileID(
				test.protocol,
				test.toolchainProfileID,
				test.buildfileOut,
				test.ccProfile,
			)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("compactToolchainProfileID() failed: %v", err)
				}
				if got != test.wantID {
					t.Fatalf("compactToolchainProfileID() = %q, want %q", got, test.wantID)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf(
					"compactToolchainProfileID() error = %v, want substring %q",
					err,
					test.wantError,
				)
			}
		})
	}
}

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
	profile := kconfigParseTestCCProfile(ccprofile.StructuralProbe{
		Kind:       "cc-option",
		Language:   "c",
		PrefixArgv: []string{"-Werror"},
		Argv:       []string{"-fprofile-supported"},
		Supported:  true,
	})
	profileID, err := compactToolchainProfileID(
		kconfig.CompactMetadataProtocolV7,
		"",
		"",
		profile,
	)
	if err != nil {
		t.Fatalf("compactToolchainProfileID() failed: %v", err)
	}

	kconfigPath := filepath.Join(root, "Kconfig")
	tree, err := kconfig.ParseFile(t.Context(), kconfigPath, kconfig.Options{
		RootDir: root,
	})
	if err != nil {
		t.Fatalf("ParseFile() failed: %v", err)
	}
	v6, err := compactMetadata(
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
		profile,
		nil,
	)
	if err != nil {
		t.Fatalf("compactMetadata() with CC profile failed: %v", err)
	}
	if len(v6.ObjectVariants) == 0 {
		t.Fatal("compactMetadata() with CC profile emitted no object variants")
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
		profile,
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

func TestParseKbuildRecordsCCProbeRequests(t *testing.T) {
	root := t.TempDir()
	kbuildPath := filepath.Join(root, "Kbuild")
	writeKconfigParseTestFile(t, root, "Kbuild", `KBUILD_CPPFLAGS := -DSTANDALONE
obj-y += init.o
ccflags-y += $(call cc-option,-fstandalone)
`)

	recorder := kconfig.NewStructuralProbeRecorder()
	if _, err := parseKbuild(
		kbuildPath,
		false,
		"",
		"",
		nil,
		nil,
		recorder,
	); err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	requests := recorder.Requests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("request count = %d, want %d: %#v", got, want, requests)
	}
	if got, want := strings.Join(requests[0].PrefixArgv, " "), "-Werror -DSTANDALONE"; got != want {
		t.Fatalf("prefix argv = %q, want %q", got, want)
	}
	if got, want := strings.Join(requests[0].Argv, " "), "-fstandalone"; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestValidateKbuildTreeSharesCCProbeRecorder(t *testing.T) {
	root := t.TempDir()
	writeKconfigParseTestFile(t, root, "Kbuild", `ccflags-y += $(call cc-option,-fshared)
`)
	writeKconfigParseTestFile(t, root, "drivers/example/Makefile", `ccflags-y += $(call cc-option,-fshared)
`)

	recorder := kconfig.NewStructuralProbeRecorder()
	summary, err := validateKbuildTree(root, nil, nil, nil, recorder)
	if err != nil {
		t.Fatalf("validateKbuildTree() failed: %v", err)
	}
	if got, want := summary.Count, 2; got != want {
		t.Fatalf("parsed file count = %d, want %d", got, want)
	}
	requests := recorder.Requests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("deduplicated request count = %d, want %d: %#v", got, want, requests)
	}
}

func TestCompactMetadataV7RecordsPerConfigCCProbeRequests(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"Kconfig": `config ALT
	bool "alternate"
`,
		"Kbuild": `obj-y += init.o
KBUILD_CFLAGS += $(if $(CONFIG_ALT),-DALT,-DBASE)
ccflags-y += $(call cc-option,-fcompact)
`,
		"base.config":                      "# CONFIG_ALT is not set\n",
		"alt.config":                       "CONFIG_ALT=y\n",
		"arch/x86/kernel/vmlinux.lds.S":    "\n",
		"init.c":                           "int init_value;\n",
		"include/linux/compiler-version.h": "\n",
		"include/linux/compiler_types.h":   "\n",
		"include/linux/kconfig.h":          "\n",
	} {
		writeKconfigParseTestFile(t, root, path, content)
	}

	kconfigPath := filepath.Join(root, "Kconfig")
	tree, err := kconfig.ParseFile(t.Context(), kconfigPath, kconfig.Options{
		RootDir: root,
	})
	if err != nil {
		t.Fatalf("ParseFile() failed: %v", err)
	}
	recorder := kconfig.NewStructuralProbeRecorder()
	_, err = compactMetadataV7(
		tree,
		kconfigPath,
		filepath.Join(root, "Kbuild"),
		[]namedPath{
			{Name: "base", Path: filepath.Join(root, "base.config")},
			{Name: "alt", Path: filepath.Join(root, "alt.config")},
		},
		"default",
		false,
		map[string]string{
			"ARCH":    "x86",
			"SRCARCH": "x86",
		},
		nil,
		map[string]string{
			"base": "//headers:base",
			"alt":  "//headers:alt",
		},
		"6.18.2",
		"linux.bzl/test/x86",
		"bootstrap-profile",
		nil,
		recorder,
	)
	if err != nil {
		t.Fatalf("compactMetadataV7() failed: %v", err)
	}

	requests := recorder.Requests()
	if got, want := len(requests), 2; got != want {
		t.Fatalf("per-config request count = %d, want %d: %#v", got, want, requests)
	}
	prefixes := map[string]bool{}
	for _, request := range requests {
		prefixes[strings.Join(request.PrefixArgv, " ")] = true
	}
	for _, want := range []string{"-Werror -DBASE", "-Werror -DALT"} {
		if !prefixes[want] {
			t.Errorf("missing expanded prefix %q in %#v", want, requests)
		}
	}

	out := filepath.Join(root, "cc_probe_requests.json")
	if err := writeCCProbeRequests(out, recorder); err != nil {
		t.Fatalf("writeCCProbeRequests() failed: %v", err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}
	wantJSON, err := recorder.JSON()
	if err != nil {
		t.Fatalf("recorder.JSON() failed: %v", err)
	}
	if string(written) != string(wantJSON) {
		t.Fatalf("written requests differ from canonical JSON:\n%s\nwant:\n%s", written, wantJSON)
	}
	if strings.Contains(string(written), `"supported"`) {
		t.Fatalf("request manifest contains an unmeasured supported result:\n%s", written)
	}
}

func TestLinuxProbeShellUsesCCProfileAndNonCCOverrides(t *testing.T) {
	profile := kconfigParseTestCCProfile()
	profile.KconfigIdentity.CCVersion = 210002
	profile.KconfigIdentity.CCVersionText = "clang version 21.0.2"
	profile.KconfigIdentity.LDVersion = 210002
	shell, err := linuxProbeShell(
		"",
		map[string]string{
			"bindgen_version": "bindgen 0.73.0",
			"pahole_version":  "140",
			"rustc_version":   "109900",
		},
		profile,
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

	_, err = linuxProbeShell(
		kconfig.LinuxProbeModelLLVM,
		map[string]string{"cc_version": "999999"},
		profile,
	)
	if err == nil || !strings.Contains(err.Error(), "cannot override checked-in -cc_profile identity") {
		t.Fatalf("CC identity override error = %v, want checked-in profile rejection", err)
	}
}

func TestLoadCCProfileDecodesCheckedInInput(t *testing.T) {
	profile := kconfigParseTestCCProfile()
	data, err := ccprofile.CanonicalJSON(*profile)
	if err != nil {
		t.Fatalf("CanonicalJSON() failed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cc_profile.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	loaded, err := loadCCProfile(path)
	if err != nil {
		t.Fatalf("loadCCProfile() failed: %v", err)
	}
	if err := ccprofile.Compare(*profile, *loaded); err != nil {
		t.Fatalf("loaded profile mismatch: %v", err)
	}
}

func kconfigParseTestCCProfile(probes ...ccprofile.StructuralProbe) *ccprofile.Profile {
	if probes == nil {
		probes = []ccprofile.StructuralProbe{}
	}
	for index := range probes {
		probes[index].ID = ccprofile.StructuralProbeID(probes[index])
	}
	return &ccprofile.Profile{
		Schema:         ccprofile.Schema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigIdentity: ccprofile.KconfigIdentity{
			CCName:        "Clang",
			CCVersion:     220108,
			CCVersionText: "clang version 22.1.8",
			ASName:        "LLVM",
			LDName:        "LLD",
			LDVersion:     220108,
			CanLink:       true,
			BuiltinMacros: map[string]string{"__SIZEOF_INT128__": "16"},
		},
		StructuralProbes: probes,
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
