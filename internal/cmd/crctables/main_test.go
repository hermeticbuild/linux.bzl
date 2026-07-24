package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyCRC32Tables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crc32table.h")
	if err := run("crc32-legacy", path, 8); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"crc32table_le[8][256]",
		"crc32table_be[8][256]",
		"crc32ctable_le[8][256]",
		"tole(",
		"tobe(",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("legacy output does not contain %q", want)
		}
	}
}

func TestLegacyCRC32BitMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crc32table.h")
	if err := run("crc32-legacy", path, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "crc32table_le") {
		t.Fatal("bit-at-a-time mode emitted a lookup table")
	}
}
