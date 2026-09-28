package source_repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceRepositoryTemplate(t *testing.T) string {
	t.Helper()
	if sourceDir, workspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sourceDir != "" && workspace != "" {
		return filepath.Join(sourceDir, workspace, "source_repo.BUILD.bazel")
	}
	// `go test ./...` executes this package from internal/tests/source_repo;
	// Bazel supplies the runfiles variables above.
	return filepath.Join("..", "..", "..", "source_repo.BUILD.bazel")
}

func TestSourceRepositoryExportsDirectories(t *testing.T) {
	content, err := os.ReadFile(sourceRepositoryTemplate(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `exclude_directories = 0`) {
		t.Fatal("source repository exports must include directories for consumer include-path labels")
	}
}

func TestSourceRepositoryExplicitlyExportsOverlayFiles(t *testing.T) {
	content, err := os.ReadFile(sourceRepositoryTemplate(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`_SOURCE_OVERLAY_FILES = [`,
		`exclude = _SOURCE_OVERLAY_FILES`,
		`) + _SOURCE_OVERLAY_FILES`,
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("source repository overlay export contract is missing %s", want)
		}
	}
}
