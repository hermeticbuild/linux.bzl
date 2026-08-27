package toolaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKbuildToolsetManifest() KbuildToolsetManifest {
	return KbuildToolsetManifest{
		Schema: KbuildToolsetManifestSchema,
		Scope:  "target",
		Actions: map[string][]string{
			"cc": {KbuildArgumentsSentinel},
		},
		Tools: map[string]string{"cc": "external/toolchain/bin/cc"},
		Closure: []string{
			"external/toolchain/bin/cc",
			"external/toolchain/include",
		},
		ArtifactKinds: map[string]string{
			"external/toolchain/bin/cc":  KbuildToolsetArtifactGeneratedFile,
			"external/toolchain/include": KbuildToolsetArtifactSource,
		},
		Environments:  map[string]map[string]string{"cc": {}},
		MakeVariables: map[string]string{"CC": "cc"},
		Requirements:  map[string]map[string]string{"cc": {}},
	}
}

func TestKbuildToolsetManifestIdentityAndStrictRead(t *testing.T) {
	manifest := testKbuildToolsetManifest()
	first, err := manifest.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "sha256-") || len(first) != len("sha256-")+64 {
		t.Fatalf("Identity() = %q", first)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(filename, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadKbuildToolsetManifest(filename)
	if err != nil {
		t.Fatal(err)
	}
	second, err := read.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("read identity = %q, want %q", second, first)
	}
	if err := os.WriteFile(filename, append(data, []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKbuildToolsetManifest(filename); err == nil || !strings.Contains(err.Error(), "trailer") {
		t.Fatalf("ReadKbuildToolsetManifest() trailer error = %v", err)
	}
}

func TestKbuildToolsetManifestRejectsPathsOutsideCanonicalClosure(t *testing.T) {
	for name, edit := range map[string]func(*KbuildToolsetManifest){
		"unsorted": func(m *KbuildToolsetManifest) {
			m.Closure[0], m.Closure[1] = m.Closure[1], m.Closure[0]
		},
		"parent": func(m *KbuildToolsetManifest) {
			m.Closure[1] = "external/toolchain/../escape"
		},
		"missing tool": func(m *KbuildToolsetManifest) {
			m.Tools["cc"] = "external/other/bin/cc"
		},
		"missing artifact kind": func(m *KbuildToolsetManifest) {
			delete(m.ArtifactKinds, "external/toolchain/include")
		},
		"invalid artifact kind": func(m *KbuildToolsetManifest) {
			m.ArtifactKinds["external/toolchain/include"] = "directory-ish"
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := testKbuildToolsetManifest()
			manifest.Closure = append([]string(nil), manifest.Closure...)
			manifest.ArtifactKinds = map[string]string{}
			for path, kind := range testKbuildToolsetManifest().ArtifactKinds {
				manifest.ArtifactKinds[path] = kind
			}
			edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("Validate() accepted invalid manifest")
			}
		})
	}
}

func TestCanonicalArtifactPath(t *testing.T) {
	for input, want := range map[string]string{
		"../toolchain/include/stddef.h":                        "external/toolchain/include/stddef.h",
		"bazel-out/k8-fastbuild/bin/external/toolchain/bin/cc": "external/toolchain/bin/cc",
		"bazel-out/cfg/genfiles/generated/header.h":            "generated/header.h",
		"external/toolchain/include":                           "external/toolchain/include",
	} {
		got, err := CanonicalArtifactPath(input)
		if err != nil {
			t.Fatalf("CanonicalArtifactPath(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("CanonicalArtifactPath(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "/absolute", "../..", "bazel-out/bad/path"} {
		if _, err := CanonicalArtifactPath(input); err == nil {
			t.Fatalf("CanonicalArtifactPath(%q) succeeded", input)
		}
	}
}
