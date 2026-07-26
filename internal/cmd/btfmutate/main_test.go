// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyAndRun(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(input, []byte("original"), 0o400); err != nil {
		t.Fatal(err)
	}

	tool, arguments := helperCommand(t, "append", outputPlaceholder, "-mutated")
	err := run(options{
		inputPath:     input,
		outputPath:    output,
		toolPath:      tool,
		toolArguments: arguments,
	})
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "original-mutated"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("output mode = %#o, want %#o", got, want)
	}
	inputData, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(inputData), "original"; got != want {
		t.Fatalf("input = %q after run, want %q", got, want)
	}
}

func TestToolFailureIsReturnedWithoutELFPatch(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	header := elfHeader("little", 2)
	if err := os.WriteFile(input, header, 0o400); err != nil {
		t.Fatal(err)
	}

	tool, arguments := helperCommand(t, "fail", outputPlaceholder)
	err := run(options{
		inputPath:      input,
		outputPath:     output,
		elfETRelEndian: "little",
		toolPath:       tool,
		toolArguments:  arguments,
	})
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run() error = %v, want exec.ExitError", err)
	}
	if got, want := exitError.ExitCode(), 23; got != want {
		t.Fatalf("tool exit code = %d, want %d", got, want)
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(data[16:18], []byte{2, 0}) {
		t.Fatalf("e_type after failed tool = %v, want unpatched [2 0]", data[16:18])
	}
}

func TestPatchELFTypeToRelocatable(t *testing.T) {
	tests := []struct {
		name      string
		endian    string
		wantBytes []byte
	}{
		{name: "little", endian: "little", wantBytes: []byte{1, 0}},
		{name: "big", endian: "big", wantBytes: []byte{0, 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input")
			output := filepath.Join(dir, "output")
			if err := os.WriteFile(input, elfHeader(test.endian, 2), 0o400); err != nil {
				t.Fatal(err)
			}

			tool, arguments := helperCommand(t, "noop", outputPlaceholder)
			err := run(options{
				inputPath:      input,
				outputPath:     output,
				elfETRelEndian: test.endian,
				toolPath:       tool,
				toolArguments:  arguments,
			})
			if err != nil {
				t.Fatalf("run() failed: %v", err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data[16:18], test.wantBytes) {
				t.Fatalf("e_type = %v, want %v", data[16:18], test.wantBytes)
			}
		})
	}
}

func TestToolOutputIsMadeWritableBeforeELFPatch(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	if err := os.WriteFile(input, elfHeader("little", 2), 0o400); err != nil {
		t.Fatal(err)
	}

	tool, arguments := helperCommand(t, "readonly", outputPlaceholder)
	err := run(options{
		inputPath:      input,
		outputPath:     output,
		elfETRelEndian: "little",
		toolPath:       tool,
		toolArguments:  arguments,
	})
	if err != nil {
		t.Fatalf("run() failed: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[16:18], []byte{1, 0}) {
		t.Fatalf("e_type = %v, want [1 0]", data[16:18])
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("output mode = %#o, want %#o", got, want)
	}
}

func TestPatchELFTypeRejectsWrongByteOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(path, elfHeader("little", 2), 0o600); err != nil {
		t.Fatal(err)
	}
	err := patchELFTypeToRelocatable(path, "big")
	if err == nil || !strings.Contains(err.Error(), "does not match requested big endian") {
		t.Fatalf("patchELFTypeToRelocatable() error = %v, want byte-order mismatch", err)
	}
}

func TestParseOptions(t *testing.T) {
	opts, err := parseOptions([]string{
		"-input", "input",
		"-output", "output",
		"-env", "LLVM_OBJCOPY=/tools/llvm-objcopy",
		"-elf-et-rel-endian", "little",
		"--",
		"/tools/pahole",
		"-J",
		outputPlaceholder,
	})
	if err != nil {
		t.Fatalf("parseOptions() failed: %v", err)
	}
	if got, want := opts.environment, []string{"LLVM_OBJCOPY=/tools/llvm-objcopy"}; !equalStrings(got, want) {
		t.Fatalf("environment = %q, want %q", got, want)
	}
	if got, want := opts.toolArguments, []string{"-J", outputPlaceholder}; !equalStrings(got, want) {
		t.Fatalf("tool arguments = %q, want %q", got, want)
	}
}

func TestParseOptionsRequiresOneOutputPlaceholder(t *testing.T) {
	for _, arguments := range [][]string{
		{"-input", "input", "-output", "output", "--", "/tools/pahole"},
		{"-input", "input", "-output", "output", "--", "/tools/pahole", outputPlaceholder, outputPlaceholder},
	} {
		if _, err := parseOptions(arguments); err == nil || !strings.Contains(err.Error(), "exactly once") {
			t.Fatalf("parseOptions(%q) error = %v, want output placeholder error", arguments, err)
		}
	}
}

func helperCommand(t *testing.T, operation string, arguments ...string) (string, []string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable, append([]string{
		"-test.run=^TestBTFMutateHelperProcess$",
		"--",
		"btfmutate-test-helper",
		operation,
	}, arguments...)
}

func TestBTFMutateHelperProcess(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == "btfmutate-test-helper" {
			marker = index
			break
		}
	}
	if marker == -1 {
		return
	}
	arguments := os.Args[marker+1:]
	if len(arguments) < 2 {
		os.Exit(97)
	}
	operation := arguments[0]
	path := arguments[1]
	switch operation {
	case "append":
		if len(arguments) != 3 {
			os.Exit(96)
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			os.Exit(95)
		}
		if _, err := file.WriteString(arguments[2]); err != nil {
			file.Close()
			os.Exit(94)
		}
		if err := file.Close(); err != nil {
			os.Exit(93)
		}
	case "noop":
	case "readonly":
		if err := os.Chmod(path, 0o400); err != nil {
			os.Exit(91)
		}
	case "fail":
		os.Exit(23)
	default:
		os.Exit(92)
	}
	os.Exit(0)
}

func elfHeader(endian string, fileType byte) []byte {
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4] = 2
	switch endian {
	case "little":
		header[5] = 1
		header[16] = fileType
	case "big":
		header[5] = 2
		header[17] = fileType
	}
	header[6] = 1
	return header
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
