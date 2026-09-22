package mapped_kernel_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestNativeConfigRecordsCompilerGuardAndPreservesSDKArtifacts(t *testing.T) {
	paths := strings.Fields(os.Getenv("FAMILY_SMOKE_NATIVE_CONFIGS"))
	if len(paths) != len(numericVariants) {
		t.Fatalf("native config trees = %q; want one per family variant", paths)
	}
	sdks := map[string]string{}
	for _, logical := range strings.Fields(os.Getenv("FAMILY_SMOKE_SDKS")) {
		root, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		variant := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(root), "family_smoke."), ".tree-sdk")
		sdks[variant] = root
	}
	seen := map[string]bool{}
	for _, logical := range paths {
		root, err := runfiles.Rlocation(logical)
		if err != nil {
			t.Fatal(err)
		}
		variant := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(root), "family_smoke."), ".native-config")
		if !slices.Contains(numericVariants, variant) || seen[variant] ||
			filepath.Base(root) != "family_smoke."+variant+".native-config" {
			t.Fatalf("unexpected native config artifact %q", root)
		}
		seen[variant] = true
		sdk := sdks[variant]
		if sdk == "" {
			t.Fatalf("missing public SDK for %s", variant)
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(root, filename)
			if err != nil {
				return err
			}
			want := numericReadFile(t, filename, 8<<20)
			got := numericReadFile(t, filepath.Join(sdk, relative), 8<<20)
			if !bytes.Equal(got, want) {
				t.Errorf("SDK %s changed native configuration artifact %s", variant, relative)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		contents := string(numericReadFile(t, filepath.Join(root, "include/config/auto.conf.cmd"), 8<<20))
		const prefix = `ifneq "$(CC_VERSION_TEXT)" "`
		found := false
		for _, line := range strings.Split(contents, "\n") {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			if !strings.HasSuffix(line, `"`) || len(line) <= len(prefix)+1 || found {
				t.Fatalf("native config %s has malformed or duplicate compiler environment guard", variant)
			}
			found = true
		}
		if !found {
			t.Fatalf("native config %s did not record its source-exported compiler text", variant)
		}
	}
}
