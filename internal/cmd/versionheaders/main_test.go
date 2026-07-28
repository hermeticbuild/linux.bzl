package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxVersionHeaderUsesDeclaredKernelVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		wants   []string
	}{
		{
			name:    "stable",
			version: "6.18.300",
			wants: []string{
				"#define LINUX_VERSION_MAJOR 6",
				"#define LINUX_VERSION_PATCHLEVEL 18",
				"#define LINUX_VERSION_SUBLEVEL 300",
				"#define LINUX_VERSION_CODE 398079",
			},
		},
		{
			name:    "release candidate",
			version: "6.19.0-rc7",
			wants: []string{
				"#define LINUX_VERSION_MAJOR 6",
				"#define LINUX_VERSION_PATCHLEVEL 19",
				"#define LINUX_VERSION_SUBLEVEL 0",
				"#define LINUX_VERSION_CODE 398080",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, err := linuxVersionHeader(tc.version)
			if err != nil {
				t.Fatalf("linuxVersionHeader(%q) failed: %v", tc.version, err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(header, want) {
					t.Errorf("linux version header = %q, want %q", header, want)
				}
			}
		})
	}

	for _, version := range []string{
		"",
		"6",
		"6.18",
		"6.x.39",
		"6.18.-rc1",
		".18.39",
	} {
		t.Run(version, func(t *testing.T) {
			if _, err := linuxVersionHeader(version); err == nil {
				t.Fatalf("linuxVersionHeader(%q) succeeded, want strict version error", version)
			}
		})
	}
}

func TestRunReadsOnlyInputsRequiredByRequestedOutput(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")

	compileOut := filepath.Join(root, "compile.h")
	if err := run([]string{
		"-compile_out", compileOut,
		"-config", missing,
		"-kernel_release", missing,
	}); err != nil {
		t.Fatalf("compile-only run read an unused input: %v", err)
	}
	assertFileContains(t, compileOut, "#define UTS_MACHINE")

	linuxVersionOut := filepath.Join(root, "version.h")
	if err := run([]string{
		"-linux_version_out", linuxVersionOut,
		"-kernel_version", "6.18.39",
		"-config", missing,
		"-kernel_release", missing,
	}); err != nil {
		t.Fatalf("linux-version-only run read an unused input: %v", err)
	}
	assertFileContains(t, linuxVersionOut, "#define LINUX_VERSION_SUBLEVEL 39")

	kernelRelease := filepath.Join(root, "kernel.release")
	if err := os.WriteFile(kernelRelease, []byte("6.18.39-local\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(kernel.release) failed: %v", err)
	}
	utsreleaseOut := filepath.Join(root, "utsrelease.h")
	if err := run([]string{
		"-utsrelease_out", utsreleaseOut,
		"-kernel_release", kernelRelease,
		"-config", missing,
	}); err != nil {
		t.Fatalf("utsrelease-only run read an unused config: %v", err)
	}
	assertFileContains(t, utsreleaseOut, `#define UTS_RELEASE "6.18.39-local"`)

	config := filepath.Join(root, "config")
	if err := os.WriteFile(config, []byte("CONFIG_SMP=y\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config) failed: %v", err)
	}
	utsversionOut := filepath.Join(root, "utsversion.h")
	if err := run([]string{
		"-utsversion_out", utsversionOut,
		"-config", config,
		"-kernel_release", missing,
	}); err != nil {
		t.Fatalf("utsversion-only run read an unused release: %v", err)
	}
	assertFileContains(t, utsversionOut, `#define UTS_VERSION "#1 SMP 1970-01-01T00:00:00Z"`)
}

func TestRunRequiresEachOutputOwnedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "linux version",
			args: []string{"-linux_version_out", "version.h"},
			want: "-kernel_version is required",
		},
		{
			name: "utsrelease",
			args: []string{"-utsrelease_out", "utsrelease.h"},
			want: "-kernel_release is required",
		},
		{
			name: "utsversion",
			args: []string{"-utsversion_out", "utsversion.h"},
			want: "-config is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
