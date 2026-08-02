package source_repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKconfigFilegroupIncludesHyphenSuffixedSources(t *testing.T) {
	path := filepath.Join(
		os.Getenv("TEST_SRCDIR"),
		os.Getenv("TEST_WORKSPACE"),
		"source_repo.BUILD.bazel",
	)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"**/Kconfig*"`) {
		t.Fatal("source repository Kconfig filegroup does not include hyphen-suffixed sources such as arch/arm/Kconfig-nommu")
	}
}
