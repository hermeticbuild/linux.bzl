package profiles_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func TestCheckedInGraphProfiles(t *testing.T) {
	type profileCase struct {
		architecture string
		profile      string
		commands     int
		graphProbes  int
		inputFiles   int
	}
	cases := []profileCase{
		{
			architecture: "x86_64",
			profile:      "llvm_22_1_8_x86_64.graph.json",
			commands:     122,
			graphProbes:  75,
			inputFiles:   15,
		},
		{
			architecture: "aarch64",
			profile:      "llvm_22_1_8_aarch64.graph.json",
			commands:     122,
			graphProbes:  18,
			inputFiles:   13,
		},
	}
	if paths := flag.Args(); len(paths) != 0 {
		if len(paths) != 2 {
			t.Fatalf("got %d profile test paths, want 2", len(paths))
		}
		cases[0].profile = paths[0]
		cases[1].profile = paths[1]
	}

	for _, test := range cases {
		t.Run(test.architecture, func(t *testing.T) {
			profileData := readProfileTestData(t, test.profile)
			profile, err := ccprofile.DecodeGraphProfile(profileData)
			if err != nil {
				t.Fatalf("DecodeGraphProfile(profile) failed: %v", err)
			}
			canonical, err := ccprofile.CanonicalGraphProfileJSON(profile)
			if err != nil {
				t.Fatalf("CanonicalGraphProfileJSON(profile) failed: %v", err)
			}
			if !bytes.Equal(profileData, canonical) {
				t.Fatal("checked-in profile is not canonical JSON")
			}
			if got, want := profile.Architecture, test.architecture; got != want {
				t.Fatalf("profile architecture = %q, want %q", got, want)
			}
			if got, want := len(profile.KconfigCommands), test.commands; got != want {
				t.Fatalf("profile command count = %d, want %d", got, want)
			}
			if got, want := len(profile.KbuildGraphProbes), test.graphProbes; got != want {
				t.Fatalf("profile Kbuild graph probe count = %d, want %d", got, want)
			}
			if bytes.Contains(profileData, []byte("/home/")) ||
				bytes.Contains(profileData, []byte("/workspace/")) {
				t.Fatal("profile contains a machine-local absolute path")
			}

			inputs := map[string]bool{}
			minToolVersions := map[string]bool{}
			for _, command := range profile.KconfigCommands {
				for _, name := range []string{"CC", "LD", "OBJCOPY", "PYTHON3"} {
					if _, ok := command.Environment[name]; !ok {
						t.Fatalf("command %s has no %s environment entry", command.ID, name)
					}
				}
				for path, digest := range command.Inputs {
					if filepath.IsAbs(path) ||
						path == ".." ||
						strings.HasPrefix(path, "../") ||
						strings.Contains(path, "\\") {
						t.Fatalf("command %s has unsafe input path %q", command.ID, path)
					}
					inputs[path] = true
					if path == "scripts/min-tool-version.sh" {
						minToolVersions[digest] = true
					}
				}
			}
			for _, probe := range profile.KbuildGraphProbes {
				for path := range probe.Inputs {
					if filepath.IsAbs(path) ||
						path == ".." ||
						strings.HasPrefix(path, "../") ||
						strings.Contains(path, "\\") {
						t.Fatalf("Kbuild graph probe %s has unsafe input path %q", probe.ID, path)
					}
				}
			}
			if got, want := len(inputs), test.inputFiles; got != want {
				t.Fatalf("profile source input count = %d, want %d", got, want)
			}
			for _, required := range []string{
				"scripts/cc-version.sh",
				"scripts/min-tool-version.sh",
				"scripts/rust_is_available.sh",
				"scripts/rust_is_available_bindgen_libclang.h",
			} {
				if !inputs[required] {
					t.Fatalf("profile does not contain transitive source input %q", required)
				}
			}
			if got, want := len(minToolVersions), 2; got != want {
				t.Fatalf(
					"profile covers %d min-tool-version.sh contents, want %d kernel-version variants",
					got,
					want,
				)
			}
		})
	}
}

func readProfileTestData(t *testing.T, path string) []byte {
	t.Helper()
	candidates := []string{path}
	if !filepath.IsAbs(path) && !strings.Contains(path, "/") {
		candidates = append(candidates, filepath.Join("profiles", path))
	}
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err == nil {
			return data
		}
	}
	resolved, err := runfiles.Rlocation(path)
	if err != nil {
		t.Fatalf("resolve runfile %q: %v", path, err)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read %q: %v", resolved, err)
	}
	return data
}
