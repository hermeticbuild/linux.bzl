package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadAndExpandLineArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(path, []byte(`
# comment
--blocklist-type value

--no-doc-comments
`), 0o600); err != nil {
		t.Fatal(err)
	}
	fileArgs, err := readLineArgs(path)
	if err != nil {
		t.Fatal(err)
	}
	wantFileArgs := []string{"--blocklist-type", "value", "--no-doc-comments"}
	if !reflect.DeepEqual(fileArgs, wantFileArgs) {
		t.Fatalf("readLineArgs() = %#v, want %#v", fileArgs, wantFileArgs)
	}
	got, err := expandedArgs(
		[]string{"input.h", "{args_file}", "-o", "{output}"},
		fileArgs,
		"output.rs",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"input.h", "--blocklist-type", "value", "--no-doc-comments", "-o", "output.rs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandedArgs() = %#v, want %#v", got, want)
	}
}

func TestParseArgs(t *testing.T) {
	cfg, err := parseArgs([]string{
		"-args_file", "params",
		"-output", "bindings.rs",
		"--", "bindgen", "input.h", "{args_file}", "-o", "{output}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tool != "bindgen" || cfg.argsFile != "params" || cfg.output != "bindings.rs" {
		t.Fatalf("parseArgs() = %#v", cfg)
	}
}

func TestExpandedArgsRequiresPlaceholder(t *testing.T) {
	_, err := expandedArgs([]string{"input.h"}, nil, "output.rs")
	if err == nil || !strings.Contains(err.Error(), "{args_file}") {
		t.Fatalf("expandedArgs() error = %v, want placeholder error", err)
	}
}
