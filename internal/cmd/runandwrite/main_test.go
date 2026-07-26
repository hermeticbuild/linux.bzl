package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAndWriteWithStdin(t *testing.T) {
	tempDir := t.TempDir()
	input := filepath.Join(tempDir, "input")
	output := filepath.Join(tempDir, "output")
	if err := os.WriteFile(input, []byte("target input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := runAndWrite(
		input,
		output,
		executable,
		[]string{"-test.run=TestRunAndWriteHelperProcess", "--", "runandwrite-helper"},
	); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := "target input\n"; string(got) != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunAndWriteHelperProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "runandwrite-helper" {
		return
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
