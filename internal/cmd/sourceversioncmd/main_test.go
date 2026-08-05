package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDepfileDependenciesEscapesAndOrder(t *testing.T) {
	got, err := depfileDependencies([]byte("out\\ file.o: src.c first.h \\\n dir/second\\ header.h third\\#header.h\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src.c", "first.h", "dir/second header.h", "third#header.h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies = %#v, want %#v", got, want)
	}
}

func TestRunWritesKbuildRecordsAndAppendsSymversions(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "kernel", "example.c")
	header := filepath.Join(dir, "kernel", "local.h")
	depfile := filepath.Join(dir, "example.d")
	if err := os.WriteFile(depfile, []byte("example.o: "+source+" "+header+" "+header+" /toolchain/stddef.h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symversions := filepath.Join(dir, "symversions")
	if err := os.WriteFile(symversions, []byte("#SYMVER exported 0x12345678\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, ".example.o.cmd")
	cfg := config{
		depfile:     depfile,
		object:      "kernel/example.o",
		out:         out,
		primary:     "kernel/example.c",
		symversions: symversions,
		physical:    []string{source, header},
		canonical:   []string{"kernel/example.c", "kernel/local.h"},
	}
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"source_kernel/example.o := kernel/example.c",
		"deps_kernel/example.o := \\",
		"  kernel/local.h \\",
		"kernel/example.o: $(deps_kernel/example.o)",
		"#SYMVER exported 0x12345678",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output %q does not contain %q", text, want)
		}
	}
	if strings.Count(text, "kernel/local.h") != 1 {
		t.Errorf("dependency order was not deduplicated in %q", text)
	}
}
