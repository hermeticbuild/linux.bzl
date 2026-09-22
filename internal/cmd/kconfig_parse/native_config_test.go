package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestNativeConfigBindsDeclaredRelativeSourceRoots(t *testing.T) {
	execroot, work := t.TempDir(), t.TempDir()
	const alias = "external/declared-source/library"
	const physical = "mapped-inputs/source"
	if err := os.MkdirAll(filepath.Join(execroot, physical, "core/src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execroot, physical, "core/src/lib.rs"), []byte("core source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bindNativeConfigSourceRoots(work, execroot, map[string]string{alias: toolaction.ExecutionRootMarker + "/" + physical}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(work, alias, "core/src/lib.rs"))
	if err != nil || string(contents) != "core source" {
		t.Fatalf("native private-tree source read = %q, %v", contents, err)
	}
	// A nested alias must not traverse the first immutable source and create
	// files there, even when both aliases name declared inputs.
	if err := bindNativeConfigSourceRoots(work, execroot, map[string]string{alias + "/nested": physical}); err == nil {
		t.Fatal("native source binding accepted a nested immutable source alias")
	}
}

func TestReadNativeConfigProjectionPreservesSourceArtifacts(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".config": "CONFIG_PRESENT=y\n", ".config.old": "seed\n",
		"include/config/auto.conf":     "CONFIG_PRESENT=y\n",
		"include/config/auto.conf.cmd": "deps_config := Kconfig\n",
		"include/generated/autoconf.h": "#define CONFIG_PRESENT 1\n",
		"include/config/PRESENT":       "", "include/config/cc/version/text.h": "",
		".tools/runtime": "private\n", "unrelated": "private\n",
	}
	for path, content := range files {
		filename := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("missing-source", filepath.Join(root, kbuildEvalSourceTree)); err != nil {
		t.Fatal(err)
	}
	projection, err := readNativeConfigProjection(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".config.old", ".tools/runtime", "unrelated", "include/generated/rustc_cfg"} {
		if _, exists := projection.files[path]; exists {
			t.Errorf("projection includes non-native artifact %q", path)
		}
	}
	for _, path := range []string{"include/config/PRESENT", "include/config/cc/version/text.h"} {
		if value, exists := projection.files[path]; !exists || value != "" {
			t.Errorf("native dependency marker %q = (%q, %t), want present and empty", path, value, exists)
		}
	}
}

func TestNativeConfigImportsValuesAndDeclaredSymbolInventory(t *testing.T) {
	tree, err := kconfig.Parse(t.Context(), strings.NewReader(`
config DEFAULT_ON
	bool
	default y
config HIDDEN
	bool
config EMPTY
	string
	default "fallback"
config NUMBER
	hex
`), "Kconfig", kconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const config = "# CONFIG_DEFAULT_ON is not set\nCONFIG_EMPTY=\"\"\nCONFIG_NUMBER=0x00a\nCONFIG_NATIVE_ONLY=y\n"
	projection := &nativeConfigProjection{files: map[string]string{".config": config}}
	resolved, err := projection.resolved(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CONFIG_DEFAULT_ON": "n", "CONFIG_HIDDEN": "n", "CONFIG_EMPTY": `""`,
		"CONFIG_NUMBER": "0x00a", "CONFIG_NATIVE_ONLY": "y",
	}
	if !maps.Equal(resolved.Effective, want) {
		t.Fatalf("native values = %#v, want %#v", resolved.Effective, want)
	}
	for key := range want {
		written := key != "CONFIG_DEFAULT_ON" && key != "CONFIG_HIDDEN"
		if resolved.ShouldWrite(key) != written {
			t.Errorf("ShouldWrite(%s) = %t, want %t", key, resolved.ShouldWrite(key), written)
		}
	}
	if _, present := resolved.Raw["CONFIG_HIDDEN"]; present {
		t.Fatal("symbol inventory changed native assignments")
	}
	if projection.files[".config"] != config {
		t.Fatal("import changed native configuration bytes")
	}
}

// These documents are source-shaped inputs to planner unit tests. The native
// serializer itself is exercised by real-source Bazel actions.
func nativeConfigFixtureForTest(config, autoConf, autoconf string) map[string]string {
	return map[string]string{
		".config":                      config,
		"include/config/auto.conf":     autoConf,
		"include/config/auto.conf.cmd": "include/config/auto.conf: Kconfig\n",
		"include/generated/autoconf.h": autoconf,
	}
}

func TestDeclaredNativeConfigTreeRootSymlink(t *testing.T) {
	root := t.TempDir()
	for name, contents := range nativeConfigFixtureForTest("CONFIG_TEST=y\n", "CONFIG_TEST=y\n", "#define CONFIG_TEST 1\n") {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "declared-tree")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readDeclaredNativeConfigProjection(alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeConfigProjection(alias); err == nil {
		t.Fatal("serializer output audit accepted a symlink root")
	}
}
