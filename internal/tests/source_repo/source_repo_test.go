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

func TestSourceRepositoryExportsDirectories(t *testing.T) {
	path := filepath.Join(
		os.Getenv("TEST_SRCDIR"),
		os.Getenv("TEST_WORKSPACE"),
		"source_repo.BUILD.bazel",
	)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `exclude_directories = 0`) {
		t.Fatal("source repository exports must include directories for consumer include-path labels")
	}
}

func TestSourceRepositoryExplicitlyExportsOverlayFiles(t *testing.T) {
	path := filepath.Join(
		os.Getenv("TEST_SRCDIR"),
		os.Getenv("TEST_WORKSPACE"),
		"source_repo.BUILD.bazel",
	)
	content, err := os.ReadFile(path)
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

func TestSourceRepositoryExposesGenksymsHeaders(t *testing.T) {
	path := filepath.Join(
		os.Getenv("TEST_SRCDIR"),
		os.Getenv("TEST_WORKSPACE"),
		"source_repo.BUILD.bazel",
	)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "genksyms_headers_cc"`,
		`"scripts/genksyms/genksyms.h"`,
		`"scripts/genksyms/keywords.c"`,
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("source repository is missing GENKSYMS header contract %s", want)
		}
	}
}
