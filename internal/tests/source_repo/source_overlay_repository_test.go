package source_repo_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type overlayPackageMarkerCase struct {
	name         string
	repository   string
	marker       string
	packageFile  string
	overlayPath  string
	relativePath string
}

func sourceRepositoryRunfile(t *testing.T, relative string) string {
	t.Helper()
	sourceDir, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE")
	if sourceDir != "" && workspace != "" {
		return filepath.Join(sourceDir, workspace, filepath.FromSlash(relative))
	}
	// `go test ./...` executes this package from internal/tests/source_repo.
	return filepath.Join("..", "..", "..", filepath.FromSlash(relative))
}

func copySourceRepositoryRunfile(t *testing.T, relative, destination string) {
	t.Helper()
	content, err := os.ReadFile(sourceRepositoryRunfile(t, relative))
	if err != nil {
		t.Fatalf("read runfile %s: %v", relative, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sourceRepositoryTempDir(t *testing.T) string {
	t.Helper()
	if testTempDir := os.Getenv("TEST_TMPDIR"); testTempDir != "" {
		directory, err := os.MkdirTemp(testTempDir, "source-overlay-repository-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(directory); err != nil {
				t.Errorf("remove source repository test directory: %v", err)
			}
		})
		return directory
	}
	return t.TempDir()
}

func writeSourceRepositoryTestSupport(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "MODULE.bazel"), `module(
    name = "linux_source_repository_test_support",
    version = "0.0.0",
)
`)
	writeFile(t, filepath.Join(root, "BUILD.bazel"), `exports_files(
    [
        "defs.bzl",
        "source_repo.BUILD.bazel",
    ],
    visibility = ["//visibility:public"],
)
`)
	writeFile(t, filepath.Join(root, "defs.bzl"), `"""Public test wrapper around the production repository rule."""

load("//internal:linux_source_repository.bzl", _linux_source_repository = "linux_source_repository")

visibility("public")

linux_source_repository = _linux_source_repository
`)
	writeFile(t, filepath.Join(root, "internal", "BUILD.bazel"), `exports_files(
    [
        "linux_source_repository.bzl",
        "linux_source_runfiles.bzl",
        "module_make_vars.bzl",
        "repository_utils.bzl",
    ],
    visibility = ["//visibility:public"],
)
`)
	for _, relative := range []string{
		"source_repo.BUILD.bazel",
		"internal/linux_source_repository.bzl",
		"internal/linux_source_runfiles.bzl",
		"internal/module_make_vars.bzl",
		"internal/repository_utils.bzl",
	} {
		copySourceRepositoryRunfile(t, relative, filepath.Join(root, filepath.FromSlash(relative)))
	}
}

func writeMinimalLinuxArchiveWithIntegrity(t *testing.T, filename string) string {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	gzipWriter := gzip.NewWriter(io.MultiWriter(file, hash))
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	files := []struct {
		name    string
		content string
	}{
		{name: "Kconfig", content: "mainmenu \"Test Linux\"\n"},
		{name: "Makefile", content: "VERSION = 1\nPATCHLEVEL = 2\nSUBLEVEL = 3\nEXTRAVERSION =\n"},
	}
	for _, entry := range files {
		content := []byte(entry.content)
		header := &tar.Header{
			Name:    "linux-1.2.3/" + entry.name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Unix(0, 0),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return "sha256-" + base64.StdEncoding.EncodeToString(hash.Sum(nil))
}

func sourceRepositoryBazelVersion(t *testing.T) string {
	t.Helper()
	if version := strings.TrimSpace(os.Getenv("USE_BAZEL_VERSION")); version != "" {
		return version
	}
	content, err := os.ReadFile(sourceRepositoryRunfile(t, ".bazelversion"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(content))
}

func writeOverlayRejectionWorkspace(
	t *testing.T,
	root, support, archiveURL, integrity string,
	cases []overlayPackageMarkerCase,
) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".bazelversion"), sourceRepositoryBazelVersion(t)+"\n")

	var module strings.Builder
	fmt.Fprintf(&module, `module(name = "source_overlay_rejection_test")

bazel_dep(name = "linux_source_repository_test_support", version = "0.0.0")
local_path_override(
    module_name = "linux_source_repository_test_support",
    path = %q,
)

linux_source_repository = use_repo_rule(
    "@linux_source_repository_test_support//:defs.bzl",
    "linux_source_repository",
)
`, support)
	for _, test := range cases {
		fmt.Fprintf(&module, `
linux_source_repository(
    name = %q,
    integrity = %q,
    source_overlays = {
        "drivers/vendor": %q,
    },
    strip_prefix = "linux-1.2.3",
    urls = [%q],
    version = "1.2.3",
)
`, test.repository, integrity, "//:"+test.marker, archiveURL)
	}
	writeFile(t, filepath.Join(root, "MODULE.bazel"), module.String())

	var build strings.Builder
	build.WriteString("exports_files(\n    [\n")
	for _, test := range cases {
		fmt.Fprintf(&build, "        %q,\n", test.marker)
		writeFile(t, filepath.Join(root, filepath.FromSlash(test.marker)), "overlay marker\n")
		writeFile(t, filepath.Join(root, filepath.FromSlash(test.packageFile)), "")
	}
	build.WriteString("    ],\n    visibility = [\"//visibility:public\"],\n)\n")
	writeFile(t, filepath.Join(root, "BUILD.bazel"), build.String())
}

func writeOverlayAcceptanceWorkspace(t *testing.T, root, support, archiveURL, integrity string) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".bazelversion"), sourceRepositoryBazelVersion(t)+"\n")
	writeFile(t, filepath.Join(root, "MODULE.bazel"), fmt.Sprintf(`module(name = "source_overlay_acceptance_test")

bazel_dep(name = "linux_source_repository_test_support", version = "0.0.0")
local_path_override(
    module_name = "linux_source_repository_test_support",
    path = %q,
)

linux_source_repository = use_repo_rule(
    "@linux_source_repository_test_support//:defs.bzl",
    "linux_source_repository",
)
linux_source_repository(
    name = "valid_overlay",
    integrity = %q,
    source_overlays = {
        "drivers/vendor": "//:overlays/accepted/marker",
    },
    strip_prefix = "linux-1.2.3",
    urls = [%q],
    version = "1.2.3",
)
`, support, integrity, archiveURL))
	writeFile(t, filepath.Join(root, "BUILD.bazel"), `exports_files(
    ["overlays/accepted/marker"],
    visibility = ["//visibility:public"],
)
`)
	for relative, content := range map[string]string{
		"marker":                                     "overlay marker\n",
		"metadata/BUILD":                             "exports_files([\"not_staged.c\"])\n",
		"metadata/kept.c":                            "int kept;\n",
		"metadata_bazel/BUILD.bazel":                 "exports_files([\"not_staged.h\"])\n",
		"metadata_bazel/kept.h":                      "#define KEPT 1\n",
		"named_dirs/BUILD/under_build.c":             "int under_build;\n",
		"named_dirs/BUILD.bazel/under_build_bazel.h": "#define UNDER_BUILD_BAZEL 1\n",
	} {
		writeFile(t, filepath.Join(root, "overlays", "accepted", filepath.FromSlash(relative)), content)
	}
}

func TestLinuxSourceRepositoryOmitsExactOverlayPackageMetadata(t *testing.T) {
	bazel, err := exec.LookPath("bazel")
	if err != nil {
		t.Fatalf("find Bazel for repository integration test: %v", err)
	}
	testRoot := sourceRepositoryTempDir(t)
	support := filepath.Join(testRoot, "support")
	workspace := filepath.Join(testRoot, "workspace")
	writeSourceRepositoryTestSupport(t, support)
	archive := filepath.Join(testRoot, "linux-1.2.3.tar.gz")
	integrity := writeMinimalLinuxArchiveWithIntegrity(t, archive)
	archiveURL := (&url.URL{Scheme: "file", Path: archive}).String()
	writeOverlayAcceptanceWorkspace(t, workspace, support, archiveURL, integrity)

	command := exec.Command(
		bazel,
		"--batch",
		"--output_user_root="+filepath.Join(testRoot, "output-user-root"),
		"--nosystem_rc",
		"--nohome_rc",
		"--noworkspace_rc",
		"build",
		"--enable_bzlmod",
		"--lockfile_mode=off",
		"--color=no",
		"--curses=no",
		"--noshow_progress",
		"@valid_overlay//:drivers/vendor/metadata/kept.c",
		"@valid_overlay//:drivers/vendor/metadata_bazel/kept.h",
		"@valid_overlay//:drivers/vendor/named_dirs/BUILD/under_build.c",
		"@valid_overlay//:drivers/vendor/named_dirs/BUILD.bazel/under_build_bazel.h",
	)
	command.Dir = workspace
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		t.Fatalf("Bazel did not stage ordinary files around ignored metadata and same-named directories: %v\n%s", commandErr, output)
	}
}

func TestLinuxSourceRepositoryRejectsCaseOnlyOverlayPackageMarkers(t *testing.T) {
	// Repository-rule failures happen before target analysis, so exercise the
	// production rule in an isolated Bazel invocation and assert its diagnostic.
	bazel, err := exec.LookPath("bazel")
	if err != nil {
		t.Fatalf("find Bazel for repository integration test: %v", err)
	}
	testRoot := sourceRepositoryTempDir(t)
	support := filepath.Join(testRoot, "support")
	workspace := filepath.Join(testRoot, "workspace")
	writeSourceRepositoryTestSupport(t, support)
	archive := filepath.Join(testRoot, "linux-1.2.3.tar.gz")
	integrity := writeMinimalLinuxArchiveWithIntegrity(t, archive)
	archiveURL := (&url.URL{Scheme: "file", Path: archive}).String()
	cases := []overlayPackageMarkerCase{
		{
			name:         "Build",
			repository:   "invalid_overlay_mixed_build",
			marker:       "overlays/mixed_build/marker",
			packageFile:  "overlays/mixed_build/nested/Build",
			overlayPath:  "overlays/mixed_build",
			relativePath: "nested/Build",
		},
		{
			name:         "build",
			repository:   "invalid_overlay_lower_build",
			marker:       "overlays/lower_build/marker",
			packageFile:  "overlays/lower_build/nested/build",
			overlayPath:  "overlays/lower_build",
			relativePath: "nested/build",
		},
		{
			name:         "BUILD.BAZEL",
			repository:   "invalid_overlay_upper_build_bazel",
			marker:       "overlays/upper_build_bazel/marker",
			packageFile:  "overlays/upper_build_bazel/nested/deeper/BUILD.BAZEL",
			overlayPath:  "overlays/upper_build_bazel",
			relativePath: "nested/deeper/BUILD.BAZEL",
		},
		{
			name:         "bUiLd.BaZeL",
			repository:   "invalid_overlay_mixed_build_bazel",
			marker:       "overlays/mixed_build_bazel/marker",
			packageFile:  "overlays/mixed_build_bazel/nested/deeper/bUiLd.BaZeL",
			overlayPath:  "overlays/mixed_build_bazel",
			relativePath: "nested/deeper/bUiLd.BaZeL",
		},
	}
	writeOverlayRejectionWorkspace(t, workspace, support, archiveURL, integrity, cases)

	outputUserRoot := filepath.Join(testRoot, "output-user-root")
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(
				bazel,
				"--batch",
				"--output_user_root="+outputUserRoot,
				"--nosystem_rc",
				"--nohome_rc",
				"--noworkspace_rc",
				"build",
				"--enable_bzlmod",
				"--lockfile_mode=off",
				"--color=no",
				"--curses=no",
				"--noshow_progress",
				"@"+test.repository+"//:all_files",
			)
			command.Dir = workspace
			output, commandErr := command.CombinedOutput()
			if commandErr == nil {
				t.Fatalf("Bazel accepted source overlay %s containing %s", test.overlayPath, test.relativePath)
			}
			diagnostic := string(output)
			for _, want := range []string{
				"source_overlays",
				test.relativePath,
				"case-folds to BUILD or BUILD.bazel",
				"deterministic behavior across case-sensitive and case-insensitive filesystems",
			} {
				if !strings.Contains(diagnostic, want) {
					t.Fatalf("Bazel failure for %s omitted %q:\n%s", test.relativePath, want, diagnostic)
				}
			}
		})
	}
}
