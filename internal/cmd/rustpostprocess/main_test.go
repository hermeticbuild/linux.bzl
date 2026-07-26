package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRustAssembly(t *testing.T) {
	got, err := rustAssembly([]byte("expanded C\n// Cut here.\n\n::kernel::concat_literals!(VALUE)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "::kernel::concat_literals!(VALUE)\n\n"; string(got) != want {
		t.Fatalf("rustAssembly() = %q, want %q", got, want)
	}
}

func TestRustAssemblyRequiresMarker(t *testing.T) {
	_, err := rustAssembly([]byte("missing marker"))
	if err == nil || !strings.Contains(err.Error(), "Cut here") {
		t.Fatalf("rustAssembly() error = %v, want marker error", err)
	}
}

func TestExportHeader(t *testing.T) {
	input := []byte(`0000000000000000 T _Rkernel
0000000000000000 R rust_data
0000000000000000 D rust_mutable
0000000000000000 B rust_bss
                 U unresolved
0000000000000000 t local
0000000000000000 T __pfx_rust
0000000000000000 T __cfi_rust
0000000000000000 T __odr_asan_rust
`)
	got := string(exportHeader(input))
	want := "" +
		"EXPORT_SYMBOL_RUST_GPL(_Rkernel);\n" +
		"EXPORT_SYMBOL_RUST_GPL(rust_data);\n" +
		"EXPORT_SYMBOL_RUST_GPL(rust_mutable);\n" +
		"EXPORT_SYMBOL_RUST_GPL(rust_bss);\n"
	if got != want {
		t.Fatalf("exportHeader() = %q, want %q", got, want)
	}
}

func TestRunBindingsAndHelpers(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input.rs")
	out := filepath.Join(dir, "output.rs")
	if err := os.WriteFile(in, []byte(
		"pub const RUST_CONST_HELPER_VALUE: u32 = 1;\n"+
			"    pub fn rust_helper_call(value: u32);\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("bindings", in, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "pub const VALUE") {
		t.Fatalf("bindings output = %q", got)
	}

	if err := run("helpers", in, out); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "#[link_name=\"rust_helper_call\"]\n    pub fn call") {
		t.Fatalf("helpers output = %q", got)
	}
}
