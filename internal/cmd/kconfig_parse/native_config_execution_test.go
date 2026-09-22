package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

func TestNativeConfigRejectsWorkerPathsFromCompiler(t *testing.T) {
	conf, err := runfiles.Rlocation(os.Getenv("LINUX_BZL_TEST_NATIVE_CONF"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runfiles.Rlocation(os.Getenv("LINUX_BZL_TEST_SCRIPT_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	execroot, toolroot := t.TempDir(), t.TempDir()
	toolAlias := filepath.Join(execroot, "external", "compiler")
	if err := os.MkdirAll(filepath.Dir(toolAlias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(toolroot, toolAlias); err != nil {
		t.Fatal(err)
	}
	compiler := filepath.Join(toolroot, "cc")
	if err := os.WriteFile(compiler, []byte("#!"+runtime+" sh\n[ \"$1\" = -print-file-name=vendor-sdk ] || exit 1\nprintf '%s\\n' \"$FIXTURE_PATH\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		wantError  bool
	}{
		{"runtime path", "/sbin/init", false},
		{"execroot", filepath.Join(execroot, "vendor-sdk"), true},
		{"resolved tool root", filepath.Join(toolroot, "vendor-sdk"), true},
		{"shell alias", toolsetpath.ShellAliasRootPath + "/vendor-sdk", true},
		{"private work tree", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := t.TempDir()
			path := test.path
			if path == "" {
				path = filepath.Join(work, "vendor-sdk")
			}
			if err := os.WriteFile(filepath.Join(work, "Kconfig"), []byte(`
config VENDOR_SDK
	string
	default "$(shell,$(CC) -print-file-name=vendor-sdk)"
`), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"--alldefconfig", "--syncconfig"} {
				command := exec.Command(conf, mode, "Kconfig")
				command.Dir = work
				command.Env = []string{"CC=" + filepath.Join(toolAlias, "cc"), "FIXTURE_PATH=" + path, "KCONFIG_CONFIG=.config"}
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("native conf %s: %v\n%s", mode, err, output)
				}
			}
			projection, err := readNativeConfigProjection(work)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(projection.files[".config"], `CONFIG_VENDOR_SDK="`+path+`"`) {
				t.Fatalf("native compiler query was not written: %s", projection.files[".config"])
			}
			err = projection.rejectWorkerPaths([]string{work, execroot, toolAlias})
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "CONFIG_VENDOR_SDK contains build-worker path") {
					t.Fatalf("worker path publication: %v", err)
				}
			} else if err != nil {
				t.Fatalf("runtime path rejected: %v", err)
			}
		})
	}
}
