package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func testManifest() toolsetManifest {
	return toolsetManifest{
		Schema: toolsetIdentitySchema,
		Scope:  "target",
		Actions: map[string][]string{
			"cc": {"wrapper", "--before", "__LINUX_BZL_KBUILD_ARGS_V1__", "--after"},
			"ld": {"wrapper", "__LINUX_BZL_KBUILD_ARGS_V1__"},
		},
		Tools:   map[string]string{"cc": "external/toolchain/bin/cc", "ld": "external/toolchain/bin/ld"},
		Closure: []string{"external/toolchain/bin/cc", "external/toolchain/bin/ld", "external/toolchain/lib/runtime.a"},
		ArtifactKinds: map[string]string{
			"external/toolchain/bin/cc":        "generated-file",
			"external/toolchain/bin/ld":        "generated-file",
			"external/toolchain/lib/runtime.a": "source-artifact",
		},
		ArtifactRoots: map[string]toolaction.KbuildToolsetArtifactRoot{
			"external/toolchain/bin/cc":        {Root: "root-00000000", Path: "external/toolchain/bin/cc"},
			"external/toolchain/bin/ld":        {Root: "root-00000000", Path: "external/toolchain/bin/ld"},
			"external/toolchain/lib/runtime.a": {Root: "root-00000000", Path: "external/toolchain/lib/runtime.a"},
		},
		Roots: map[string]string{"root-00000000": "external/toolchain/bin/cc"},
		Environments: map[string]map[string]string{
			"cc": {"PATH": "/toolchain/cc", "ZERO_AR_DATE": "1"},
			"ld": {"PATH": "/toolchain/ld"},
		},
		MakeVariables: map[string]string{"CC": "cc", "LD": "ld"},
		Requirements: map[string]map[string]string{
			"cc": {"supports-path-mapping": "1"},
			"ld": {"requires-network": "0"},
		},
	}
}

func cloneManifest(t *testing.T, manifest toolsetManifest) toolsetManifest {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var clone toolsetManifest
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestManifestIdentityIsStableAndSensitiveToEveryField(t *testing.T) {
	base := testManifest()
	want, err := manifestIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, "sha256-") || len(want) != len("sha256-")+64 {
		t.Fatalf("identity=%q", want)
	}
	reordered := testManifest()
	reordered.Actions = map[string][]string{"ld": {"wrapper", "__LINUX_BZL_KBUILD_ARGS_V1__"}, "cc": {"wrapper", "--before", "__LINUX_BZL_KBUILD_ARGS_V1__", "--after"}}
	reordered.Tools = map[string]string{"ld": "external/toolchain/bin/ld", "cc": "external/toolchain/bin/cc"}
	got, err := manifestIdentity(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("map insertion order changed identity: got %q want %q", got, want)
	}

	tests := map[string]func(*toolsetManifest){
		"scope":       func(m *toolsetManifest) { m.Scope = "host" },
		"action argv": func(m *toolsetManifest) { m.Actions["cc"][1] = "--different" },
		"tool path": func(m *toolsetManifest) {
			old := m.Closure[0]
			delete(m.ArtifactKinds, old)
			delete(m.ArtifactRoots, old)
			m.Closure[0] = "external/toolchain/bin/alternate-cc"
			m.Tools["cc"] = m.Closure[0]
			m.ArtifactKinds[m.Closure[0]] = "generated-file"
			m.ArtifactRoots[m.Closure[0]] = toolaction.KbuildToolsetArtifactRoot{Root: "root-00000000", Path: m.Closure[0]}
			m.Roots["root-00000000"] = m.Closure[0]
		},
		"closure": func(m *toolsetManifest) {
			old := m.Closure[2]
			delete(m.ArtifactKinds, old)
			delete(m.ArtifactRoots, old)
			m.Closure[2] = "external/toolchain/lib/runtime.so"
			m.ArtifactKinds[m.Closure[2]] = "source-artifact"
			m.ArtifactRoots[m.Closure[2]] = toolaction.KbuildToolsetArtifactRoot{Root: "root-00000000", Path: m.Closure[2]}
		},
		"artifact kind": func(m *toolsetManifest) { m.ArtifactKinds["external/toolchain/bin/cc"] = "source-artifact" },
		"artifact root path": func(m *toolsetManifest) {
			location := m.ArtifactRoots["external/toolchain/bin/ld"]
			location.Path = "alternate/root/bin/ld"
			m.ArtifactRoots["external/toolchain/bin/ld"] = location
		},
		"root anchor":     func(m *toolsetManifest) { m.Roots["root-00000000"] = "external/toolchain/bin/ld" },
		"cc environment":  func(m *toolsetManifest) { m.Environments["cc"]["ZERO_AR_DATE"] = "0" },
		"ld environment":  func(m *toolsetManifest) { m.Environments["ld"]["PATH"] = "/other/ld" },
		"cc requirements": func(m *toolsetManifest) { m.Requirements["cc"]["supports-path-mapping"] = "0" },
		"ld requirements": func(m *toolsetManifest) { m.Requirements["ld"]["requires-network"] = "1" },
		"make variable":   func(m *toolsetManifest) { m.MakeVariables["CC"] = "ld" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := cloneManifest(t, base)
			mutate(&changed)
			got, err := manifestIdentity(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("field change did not change identity %q", got)
			}
		})
	}
}

func TestRunWritesExactlyOneEmptyMarker(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	data, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(manifestPath, output); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].IsDir() || !strings.HasPrefix(entries[0].Name(), "sha256-") {
		t.Fatalf("entries=%v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("marker size=%d, want 0", info.Size())
	}
	if err := run(manifestPath, output); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second run err=%v", err)
	}
}

func TestManifestValidationRejectsNonCanonicalClosureAndMismatchedRoles(t *testing.T) {
	unsorted := testManifest()
	unsorted.Closure[0], unsorted.Closure[1] = unsorted.Closure[1], unsorted.Closure[0]
	if _, err := manifestIdentity(unsorted); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("unsorted err=%v", err)
	}
	mismatched := testManifest()
	delete(mismatched.Tools, "ld")
	if _, err := manifestIdentity(mismatched); err == nil || !strings.Contains(err.Error(), "identical roles") {
		t.Fatalf("mismatched err=%v", err)
	}
	unknownBinding := testManifest()
	unknownBinding.MakeVariables["OBJCOPY"] = "objcopy"
	if _, err := manifestIdentity(unknownBinding); err == nil || !strings.Contains(err.Error(), "unknown action role") {
		t.Fatalf("unknown Make binding err=%v", err)
	}
	invalidRole := testManifest()
	invalidRole.Actions["CC"] = invalidRole.Actions["cc"]
	invalidRole.Tools["CC"] = invalidRole.Tools["cc"]
	invalidRole.Environments["CC"] = invalidRole.Environments["cc"]
	invalidRole.Requirements["CC"] = invalidRole.Requirements["cc"]
	delete(invalidRole.Actions, "cc")
	delete(invalidRole.Tools, "cc")
	delete(invalidRole.Environments, "cc")
	delete(invalidRole.Requirements, "cc")
	invalidRole.MakeVariables["CC"] = "CC"
	if _, err := manifestIdentity(invalidRole); err == nil || !strings.Contains(err.Error(), "invalid action role") {
		t.Fatalf("invalid action role err=%v", err)
	}
}
