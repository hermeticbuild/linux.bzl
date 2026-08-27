package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func writeArgumentsFile(t *testing.T, arguments []string) string {
	t.Helper()
	data, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	filename := filepath.Join(t.TempDir(), "arguments.json")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func writeObservedState(t *testing.T, directory, name string, state toolaction.ObservedOutputState) string {
	t.Helper()
	data, err := toolaction.EncodeObservedOutputState(state)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, name)
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestRunWritesExactBytes(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "manifest")
	want := []byte("drivers/test.ko\n\x00binary\n")
	if err := run([]string{"-out", out, "-content_base64", base64.StdEncoding.EncodeToString(want)}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunReadsCanonicalArgumentsFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "large-argv-output")
	arguments := []string{"-out", out}
	const lineCount = 4000
	for ordinal := range lineCount {
		arguments = append(arguments, "-line", fmt.Sprintf("line-%04d", ordinal))
	}
	filename := writeArgumentsFile(t, arguments)
	if err := run([]string{"-arguments_file", filename}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != lineCount || lines[0] != "line-0000" || lines[len(lines)-1] != "line-3999" {
		t.Fatalf("response-file output has %d lines, first=%q last=%q", len(lines), lines[0], lines[len(lines)-1])
	}
}

func TestDecodeArgumentsFileRejectsMalformedOrNestedInput(t *testing.T) {
	canonical := func(arguments []string) []byte {
		data, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "malformed", data: []byte("not json\n"), want: "decode arguments file"},
		{name: "not array", data: []byte("null\n"), want: "JSON string array"},
		{name: "missing newline", data: []byte(`[]`), want: "not canonically encoded"},
		{name: "leading whitespace", data: []byte(" []\n"), want: "not canonically encoded"},
		{name: "trailing value", data: []byte("[]\n{}\n"), want: "trailing JSON value"},
		{name: "nul", data: canonical([]string{"-line", "bad\x00value"}), want: "contains NUL"},
		{name: "nested pair", data: canonical([]string{"-arguments_file", "nested.json"}), want: "nests"},
		{name: "nested equals", data: canonical([]string{"-arguments_file=nested.json"}), want: "nests"},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "arguments.json")
			if err := os.WriteFile(filename, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := decodeArgumentsFile(filename); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeArgumentsFile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunArgumentsFileRequiresExclusivePath(t *testing.T) {
	for _, arguments := range [][]string{
		{"-arguments_file"},
		{"-arguments_file", "one", "two"},
	} {
		if err := run(arguments); err == nil || !strings.Contains(err.Error(), "exactly one path") {
			t.Fatalf("run(%q) error = %v", arguments, err)
		}
	}
}

func TestRunWritesRuntimeLinesInOrder(t *testing.T) {
	out := filepath.Join(t.TempDir(), "members")
	if err := run([]string{"-out", out, "-line", "first.o", "-line", "path/second.o"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first.o\npath/second.o\n"; string(got) != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestRunCopiesExactBytes(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "source")
	out := filepath.Join(dir, "nested", "copy")
	want := []byte("generated config\n\x00bytes\n")
	if err := os.WriteFile(in, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-out", out, "-input", in}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("output = %q, want %q", got, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("output mode = %o, want %o", got, want)
	}
}

func TestRunPreservesExecutableBitsFromSingleInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "source")
	out := filepath.Join(dir, "copy")
	if err := os.WriteFile(in, []byte("#!/bin/sh\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-out", out, "-input", in, "-preserve_mode"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("output mode = %o, want %o", got, want)
	}
}

func TestRunConcatenatesInputsInOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	out := filepath.Join(dir, "joined")
	if err := os.WriteFile(first, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-out", out, "-input", first, "-input", second}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first\nsecond\x00"; string(got) != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunMergesAbsoluteObservedOutputStates(t *testing.T) {
	dir := t.TempDir()
	writer := strings.Repeat("a", 64)
	absent := writeObservedState(t, dir, "absent", toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputAbsent})
	present := toolaction.ObservedOutputState{
		Disposition: toolaction.ObservedOutputPresent,
		Writer:      writer,
		Content:     []byte("selected\x00bytes\n"), ExecutableMode: 0o101,
	}
	first := writeObservedState(t, dir, "present-first", present)
	duplicate := writeObservedState(t, dir, "present-duplicate", present)
	out := filepath.Join(dir, "nested", "selected")
	if err := run([]string{"-out", out, "-state", absent, "-state", first, "-state", duplicate}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "selected\x00bytes\n"; got != want {
		t.Fatalf("selected output = %q, want %q", got, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o745); got != want {
		t.Fatalf("selected output mode = %o, want %o", got, want)
	}
}

func TestRunRejectsInvalidAbsoluteObservedOutputStateMerges(t *testing.T) {
	dir := t.TempDir()
	writerA := strings.Repeat("a", 64)
	writerB := strings.Repeat("b", 64)
	absent := writeObservedState(t, dir, "absent", toolaction.ObservedOutputState{Disposition: toolaction.ObservedOutputAbsent})
	presentA := writeObservedState(t, dir, "present-a", toolaction.ObservedOutputState{
		Disposition: toolaction.ObservedOutputPresent, Writer: writerA, Content: []byte("first"),
	})
	presentB := writeObservedState(t, dir, "present-b", toolaction.ObservedOutputState{
		Disposition: toolaction.ObservedOutputPresent, Writer: writerB, Content: []byte("first"),
	})
	presentAConflict := writeObservedState(t, dir, "present-a-conflict", toolaction.ObservedOutputState{
		Disposition: toolaction.ObservedOutputPresent, Writer: writerA, Content: []byte("second"),
	})
	deleted := writeObservedState(t, dir, "deleted", toolaction.ObservedOutputState{
		Disposition: toolaction.ObservedOutputDeleted, Writer: writerA,
	})
	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte("not a state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		states []string
		want   string
	}{
		{name: "no writer", states: []string{absent}, want: "no -state input has a writer"},
		{name: "unordered writers", states: []string{presentA, presentB}, want: "unordered writers"},
		{name: "inconsistent same writer", states: []string{presentA, presentAConflict}, want: "inconsistent states"},
		{name: "final deletion", states: []string{absent, deleted}, want: "final deletion"},
		{name: "malformed", states: []string{presentA, malformed}, want: "decode -state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := filepath.Join(dir, "out-"+strings.ReplaceAll(test.name, " ", "-"))
			args := []string{"-out", out}
			for _, state := range test.states {
				args = append(args, "-state", state)
			}
			if err := run(args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run error = %v, want %q", err, test.want)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("failed state merge created output: %v", err)
			}
		})
	}
}

func TestRunStagesCanonicalTree(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "tree")
	if err := run([]string{
		"-tree_out", out,
		"-copy", "external/pkg/include/header.h=" + first,
		"-copy", "external/pkg/lib/libpkg.a=" + second,
	}); err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string]string{
		"external/pkg/include/header.h": "header",
		"external/pkg/lib/libpkg.a":     "archive",
	} {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(relative)))
		if err != nil || string(got) != want {
			t.Fatalf("staged %s = %q, %v; want %q", relative, got, err, want)
		}
	}
}

func TestRunRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-out", "x"},
		{"-tree_out", "x"},
		{"-tree_out", "x", "-copy", "../escape=input"},
		{"-tree_out", "x", "-copy", "same=input", "-copy", "same=other"},
		{"-out", "x", "-content_base64", "***"},
		{"-out", "x", "-input", ""},
		{"-out", "x", "-input", "missing", "-content_base64", ""},
		{"-out", "x", "-state", "missing", "-line", "line"},
		{"-out", "x", "-content_base64", "", "-preserve_mode"},
		{"-out", "x", "-input", "first", "-input", "second", "-preserve_mode"},
		{"-tree_out", "x", "-copy", "same=input", "-preserve_mode"},
		{"-tree_out", "x", "-copy", "same=input", "-state", "state"},
		{"-out", "x", "-content_base64", "", "extra"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}
