package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePkgConfigManifest(t *testing.T, content string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func testPkgConfigManifest(t *testing.T) string {
	t.Helper()
	return writePkgConfigManifest(t, `{
  "schema": "linux.bzl/pkg-config-manifest/v1",
  "packages": {
    "libcrypto": {
      "cflags": ["-I__LINUX_BZL_HOST_DEPS__/openssl/include", "-DNAME=value with space"],
      "libs": ["-L__LINUX_BZL_HOST_DEPS__/openssl/lib", "-lcrypto"]
    },
    "libelf": {
      "cflags": [],
      "libs": ["/__LINUX_BZL_HOST_DEPS__/libelf.a"]
    }
  }
}`)
}

func TestPkgConfigShimSupportsLinuxArgumentOrders(t *testing.T) {
	manifest := testPkgConfigManifest(t)
	for name, arguments := range map[string][]string{
		"option first":   {"-manifest", manifest, "--", "--cflags", "libcrypto"},
		"package first":  {"-manifest", manifest, "--", "libcrypto", "--cflags"},
		"multiple libs":  {"-manifest", manifest, "--", "--libs", "libelf", "libcrypto"},
		"package exists": {"-manifest", manifest, "--", "--exists", "libcrypto"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := run(arguments, &stdout, &stderr); status != 0 {
				t.Fatalf("run status = %d, stderr = %q", status, stderr.String())
			}
			switch name {
			case "option first", "package first":
				want := "-I__LINUX_BZL_HOST_DEPS__/openssl/include '-DNAME=value with space'\n"
				if got := stdout.String(); got != want {
					t.Fatalf("stdout = %q, want %q", got, want)
				}
			case "multiple libs":
				want := "/__LINUX_BZL_HOST_DEPS__/libelf.a -L__LINUX_BZL_HOST_DEPS__/openssl/lib -lcrypto\n"
				if got := stdout.String(); got != want {
					t.Fatalf("stdout = %q, want %q", got, want)
				}
			case "package exists":
				if stdout.Len() != 0 {
					t.Fatalf("--exists stdout = %q, want empty", stdout.String())
				}
			}
		})
	}
}

func TestPkgConfigShimReportsUnavailablePackageLikePkgConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := run([]string{"-manifest", testPkgConfigManifest(t), "--", "--libs", "missing"}, &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `package "missing" is unavailable`) {
		t.Fatalf("unavailable package status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestPkgConfigShimRejectsMalformedManifestAndQueries(t *testing.T) {
	valid := testPkgConfigManifest(t)
	tests := map[string]struct {
		manifest  string
		arguments []string
		error     string
	}{
		"wrong schema": {
			manifest:  writePkgConfigManifest(t, `{"schema":"other","packages":{}}`),
			arguments: []string{"--cflags", "libcrypto"},
			error:     "manifest schema",
		},
		"missing flag family": {
			manifest:  writePkgConfigManifest(t, `{"schema":"linux.bzl/pkg-config-manifest/v1","packages":{"libcrypto":{"cflags":[]}}}`),
			arguments: []string{"--cflags", "libcrypto"},
			error:     "must define cflags and libs",
		},
		"unsafe package": {
			manifest:  valid,
			arguments: []string{"--cflags", "../libcrypto"},
			error:     "unsupported argument",
		},
		"combined modes": {
			manifest:  valid,
			arguments: []string{"--cflags", "--libs", "libcrypto"},
			error:     "combines output modes",
		},
		"missing package": {
			manifest:  valid,
			arguments: []string{"--cflags"},
			error:     "at least one package",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			arguments := append([]string{"-manifest", test.manifest, "--"}, test.arguments...)
			if status := run(arguments, &stdout, &stderr); status != 2 || !strings.Contains(stderr.String(), test.error) {
				t.Fatalf("status=%d stdout=%q stderr=%q, want error %q", status, stdout.String(), stderr.String(), test.error)
			}
		})
	}
}
