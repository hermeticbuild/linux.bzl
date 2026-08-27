package sonic

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestArm64ExcludesLinuxNgbde(t *testing.T) {
	configPath, err := runfiles.Rlocation(os.Getenv("SONIC_ARM64_CONFIG"))
	if err != nil {
		t.Fatalf("resolve arm64 config: %v", err)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read arm64 config: %v", err)
	}
	if strings.Contains(string(config), "CONFIG_LINUX_NGBDE=") {
		t.Fatalf("x86-only vendor Kconfig symbol unexpectedly resolved on arm64:\n%s", config)
	}

	root, err := runfiles.Rlocation(os.Getenv("SONIC_ARM64_MODULES"))
	if err != nil {
		t.Fatalf("resolve arm64 module tree: %v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve arm64 module tree symlink: %v", err)
	}
	sawModulesOrder := false
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "linux_ngbde.ko" {
			t.Fatalf("x86-only linux_ngbde unexpectedly exists in arm64 module tree at %s", path)
		}
		if path == filepath.Join(root, "modules.order") {
			sawModulesOrder = true
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect arm64 module tree: %v", err)
	}
	if !sawModulesOrder {
		t.Fatal("arm64 module tree did not contain mapped Kbuild's modules.order")
	}
}
