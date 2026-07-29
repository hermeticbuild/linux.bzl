package profiles_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

const structuralProbeRequestsSchema = "linux.bzl/cc-structural-probe-requests-v1"

type requestManifest struct {
	Schema           string                      `json:"schema"`
	StructuralProbes []ccprofile.StructuralProbe `json:"structural_probes"`
}

func TestCheckedInProfilesCoverExampleRequests(t *testing.T) {
	type profileCase struct {
		architecture string
		profile      string
		requests     string
		total        int
		supported    int
	}
	cases := []profileCase{
		{
			architecture: "x86_64",
			profile:      "llvm_22_1_8_x86_64.json",
			requests:     "llvm_22_1_8_x86_64.requests.json",
			total:        63,
			supported:    36,
		},
		{
			architecture: "aarch64",
			profile:      "llvm_22_1_8_aarch64.json",
			requests:     "llvm_22_1_8_aarch64.requests.json",
			total:        42,
			supported:    23,
		},
	}
	if paths := flag.Args(); len(paths) != 0 {
		if len(paths) != 4 {
			t.Fatalf("got %d profile test paths, want 4", len(paths))
		}
		cases[0].profile = paths[0]
		cases[0].requests = paths[1]
		cases[1].profile = paths[2]
		cases[1].requests = paths[3]
	}

	for _, test := range cases {
		t.Run(test.architecture, func(t *testing.T) {
			profileData := readProfileTestData(t, test.profile)
			profile, err := ccprofile.Decode(profileData)
			if err != nil {
				t.Fatalf("Decode(profile) failed: %v", err)
			}
			canonical, err := ccprofile.CanonicalJSON(profile)
			if err != nil {
				t.Fatalf("CanonicalJSON(profile) failed: %v", err)
			}
			if !bytes.Equal(profileData, canonical) {
				t.Fatal("checked-in profile is not canonical JSON")
			}
			if got, want := profile.Architecture, test.architecture; got != want {
				t.Fatalf("profile architecture = %q, want %q", got, want)
			}

			requestData := readProfileTestData(t, test.requests)
			if bytes.Contains(requestData, []byte("null")) {
				t.Fatal("request manifest contains null")
			}
			if bytes.Contains(requestData, []byte("$(")) ||
				bytes.Contains(requestData, []byte("${")) {
				t.Fatal("request manifest contains unresolved Make syntax")
			}
			var manifest requestManifest
			if err := json.Unmarshal(requestData, &manifest); err != nil {
				t.Fatalf("Unmarshal(requests) failed: %v", err)
			}
			if got, want := manifest.Schema, structuralProbeRequestsSchema; got != want {
				t.Fatalf("request schema = %q, want %q", got, want)
			}
			if got, want := len(manifest.StructuralProbes), test.total; got != want {
				t.Fatalf("request count = %d, want %d", got, want)
			}
			for _, probe := range manifest.StructuralProbes {
				for _, arg := range append(
					append([]string{}, probe.PrefixArgv...),
					probe.Argv...,
				) {
					if strings.Contains(arg, "/") &&
						!strings.Contains(arg, ccprofile.StructuralProbeSourceRoot) &&
						!strings.Contains(arg, ccprofile.StructuralProbeObjectRoot) {
						t.Fatalf("request %s contains a non-canonical path in %q", probe.ID, arg)
					}
				}
			}
			if err := ccprofile.ValidateStructuralProbeCoverage(
				profile,
				manifest.StructuralProbes,
			); err != nil {
				t.Fatalf("profile request coverage failed: %v", err)
			}
			supported := 0
			for _, probe := range profile.StructuralProbes {
				if probe.Supported {
					supported++
				}
			}
			if got, want := supported, test.supported; got != want {
				t.Fatalf("supported request count = %d, want %d", got, want)
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
