package main

import (
	"bytes"
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

func TestRunValidatesConfigIndependentMacroHeader(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	out := filepath.Join(dir, "validated.h")
	if err := os.WriteFile(first, []byte(`#ifndef __GENERATED_OFFSETS_H__
#define __GENERATED_OFFSETS_H__
/* CONFIG_NR_CPUS is producer-owned commentary, not an active dependency. */
#define POSITIVE 8 /* offsetof(CONFIG_NR_CPUS) */
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(`#define NEGATIVE -0x10
#endif
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"-out", out, "-input", first, "-input", second,
		"-" + validateConfigIndependentMacroHeaderFlag,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("CONFIG_NR_CPUS")) {
		t.Fatalf("validated output lost producer-owned comment: %q", data)
	}
}

func TestValidateConfigIndependentMacroHeaderRejectsPreprocessorEffects(t *testing.T) {
	wrap := func(body string) []byte {
		return []byte("#ifndef __OFFSETS_H__\n#define __OFFSETS_H__\n" + body + "\n#endif\n")
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "active config", data: wrap("#define OFFSET CONFIG_VALUE")},
		{name: "include", data: wrap("#include <evil.h>")},
		{name: "import", data: wrap("#import <evil.h>")},
		{name: "include next", data: wrap("#include_next <evil.h>")},
		{name: "embed", data: wrap("#embed \"evil.bin\"")},
		{name: "digraph include", data: wrap("%:include <evil.h>")},
		{name: "token paste", data: wrap("#define OFFSET A##B")},
		{name: "pragma", data: wrap("#define OFFSET _Pragma(\"once\")")},
		{name: "function macro", data: wrap("#define OFFSET(x) 1")},
		{name: "config guard", data: []byte("#ifndef CONFIG_OFFSET\n#define CONFIG_OFFSET\n#endif\n")},
		{name: "line splice", data: wrap("#define OFFSET 1\\\n#include <evil.h>")},
		{name: "trigraph", data: wrap("/* ??/ */\n#define OFFSET 1")},
		{name: "normal token", data: wrap("int value;")},
		{name: "unterminated comment", data: wrap("/* comment")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfigIndependentMacroHeader(test.data); err == nil {
				t.Fatalf("validateConfigIndependentMacroHeader(%q) succeeded", test.data)
			}
		})
	}
}

func TestRunValidatesClosedIntegerMacroHeader(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "cpufeaturemasks.h")
	out := filepath.Join(dir, "validated.h")
	data := []byte(`#ifndef _ASM_X86_CPUFEATUREMASKS_H
#define _ASM_X86_CPUFEATUREMASKS_H
/* REQUIRED features are rendered by the immutable generator. */
#define REQUIRED_MASK0 0x00000001U
#define REQUIRED_MASK1 0x80000000UL
#define REQUIRED_MASK_BIT_SET(x) \
	((((x) >> 5) == 0 ? REQUIRED_MASK0 : \
	  ((x) >> 5) == 1 ? REQUIRED_MASK1 : 0U) & \
	 (1ULL << ((x) & 31)))
#endif /* _ASM_X86_CPUFEATUREMASKS_H */
`)
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"-out", out, "-input", input, "-" + validateClosedIntegerMacroHeaderFlag,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("validated header changed bytes:\n%s", got)
	}
}

func TestValidateClosedIntegerMacroHeaderRejectsOpenOrMalformedLanguage(t *testing.T) {
	wrap := func(body string) []byte {
		return []byte("#ifndef _GENERATED_MASKS_H\n#define _GENERATED_MASKS_H\n" + body + "\n#endif\n")
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "include", body: "#include <linux/config.h>"},
		{name: "conditional directive", body: "#if 1\n#define VALUE 1\n#endif"},
		{name: "config identifier", body: "#define VALUE CONFIG_DYNAMIC"},
		{name: "unbound identifier", body: "#define VALUE runtime_value"},
		{name: "string literal", body: `#define VALUE "text"`},
		{name: "joined decimal identifier", body: "#define VALUE 123abc"},
		{name: "empty hexadecimal", body: "#define VALUE 0x"},
		{name: "invalid octal", body: "#define VALUE 08"},
		{name: "repeated suffix", body: "#define VALUE 1UU"},
		{name: "token paste", body: "#define VALUE(x) x##x"},
		{name: "hidden splice directive", body: "#define VALUE 1\\\n#include <evil.h>"},
		{name: "config guard", body: "#ifdef CONFIG_DYNAMIC\n#define VALUE 1\n#endif"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateClosedIntegerMacroHeader(wrap(test.body)); err == nil {
				t.Fatalf("validateClosedIntegerMacroHeader accepted %q", test.body)
			}
		})
	}
}

func TestRunComparesInputsByteForByte(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left")
	right := filepath.Join(dir, "right")
	out := filepath.Join(dir, "stamp")
	for _, pathname := range []string{left, right} {
		if err := os.WriteFile(pathname, []byte("same\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"-out", out, "-compare_input", left, "-compare_input", right}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "validated\n" {
		t.Fatalf("comparison stamp = %q, %v", got, err)
	}
	if err := os.WriteFile(right, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-out", filepath.Join(dir, "different"), "-compare_input", left, "-compare_input", right}); err == nil || !strings.Contains(err.Error(), "files differ") {
		t.Fatalf("different comparison error = %v", err)
	}
}

func TestRunRejectsMixedClosedHeaderAndComparisonForms(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-out", filepath.Join(dir, "both-validators"), "-input", input, "-" + validateConfigIndependentMacroHeaderFlag, "-" + validateClosedIntegerMacroHeaderFlag},
		{"-out", filepath.Join(dir, "mixed-forms"), "-input", input, "-compare_input", input, "-compare_input", input},
		{"-out", filepath.Join(dir, "one-compare"), "-compare_input", input},
		{"-out", filepath.Join(dir, "compare-validator"), "-compare_input", input, "-compare_input", input, "-" + validateClosedIntegerMacroHeaderFlag},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) unexpectedly succeeded", args)
		}
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

func TestRunStagesCanonicalTreePreservesExecutableBits(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("#!/bin/sh\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "tree")
	if err := run([]string{
		"-tree_out", out,
		"-copy", "bin/tool=" + input,
		"-preserve_mode",
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(out, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("staged output mode = %o, want %o", got, want)
	}
}

func TestRunCreatesEmptyTree(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "tree")
	if err := run([]string{"-tree_out", out}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty tree contains %d entries: %v", len(entries), entries)
	}
}

func TestRunAcceptsBazelPrecreatedTreeFileParents(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(out, "nested", "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-tree_out", out, "-copy", "nested/parent/result=" + input}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "nested", "parent", "result"))
	if err != nil || string(got) != "selected" {
		t.Fatalf("staged output = %q, %v", got, err)
	}
}

func TestRunRejectsPreexistingTreeOutputLeaves(t *testing.T) {
	for _, test := range []struct {
		name    string
		symlink bool
	}{
		{name: "file"},
		{name: "symlink", symlink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "tree")
			if err := os.MkdirAll(filepath.Join(out, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(out, "nested", "leaf")
			if test.symlink {
				if err := os.Symlink(dir, leaf); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(leaf, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"-tree_out", out}); err == nil || !strings.Contains(err.Error(), "pre-existing leaf") {
				t.Fatalf("run error = %v, want pre-existing leaf", err)
			}
		})
	}
}

func TestRunRejectsSymlinkTreeOutputRoot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "tree")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-tree_out", root}); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("run error = %v, want non-directory root rejection", err)
	}
}

func TestValidateFamilyViewDestinationsFindsNonadjacentAncestor(t *testing.T) {
	err := validateFamilyViewDestinations([]familyViewProjection{
		{relative: "a"},
		{relative: "a-0"},
		{relative: "a/x"},
	})
	if err == nil || !strings.Contains(err.Error(), "file/subtree collision") {
		t.Fatalf("validateFamilyViewDestinations error = %v, want collision", err)
	}
}

func TestRunProjectsFamilyViewFromExactMarkers(t *testing.T) {
	dir := t.TempDir()
	planRoot := filepath.Join(dir, "plan")
	storeRoot := filepath.Join(dir, "store")
	outputRoot := filepath.Join(dir, "view")
	firstNode := strings.Repeat("a", 64)
	secondNode := strings.Repeat("b", 64)
	for _, entry := range []struct {
		node        string
		slot        string
		destination string
		content     string
		mode        os.FileMode
	}{
		{node: firstNode, slot: "00000000", destination: "bin/tool", content: "#!/bin/sh\n", mode: 0o751},
		{node: secondNode, slot: "00000003", destination: "metadata", content: "selected\n", mode: 0o600},
	} {
		marker := filepath.Join(planRoot, "variants", "base", "view", "image", "from", entry.node, entry.slot, "at", filepath.FromSlash(entry.destination))
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(storeRoot, "nodes", entry.node, entry.slot)
		if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(entry.content), entry.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, entry.mode); err != nil {
			t.Fatal(err)
		}
	}
	// Bazel may precreate parents of declared TreeFile outputs.
	if err := os.MkdirAll(filepath.Join(outputRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"-family_view_plan_root", planRoot,
		"-family_view_variant", "base",
		"-family_view_tree", "image",
		"-family_view_store_root", storeRoot,
		"-family_view_output_root", outputRoot,
		"-family_view_expected_count", "2",
		"-preserve_mode",
	}); err != nil {
		t.Fatal(err)
	}
	tool, err := os.ReadFile(filepath.Join(outputRoot, "bin", "tool"))
	if err != nil || string(tool) != "#!/bin/sh\n" {
		t.Fatalf("projected tool = %q, %v", tool, err)
	}
	info, err := os.Stat(filepath.Join(outputRoot, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Fatalf("projected tool mode = %o, want %o", got, want)
	}
	metadata, err := os.ReadFile(filepath.Join(outputRoot, "metadata"))
	if err != nil || string(metadata) != "selected\n" {
		t.Fatalf("projected metadata = %q, %v", metadata, err)
	}
}

func TestProjectFamilyViewFailsClosed(t *testing.T) {
	makeFixture := func(t *testing.T, markerRelative string, markerContent []byte) (string, string, string) {
		t.Helper()
		dir := t.TempDir()
		planRoot := filepath.Join(dir, "plan")
		storeRoot := filepath.Join(dir, "store")
		outputRoot := filepath.Join(dir, "view")
		node := strings.Repeat("a", 64)
		slot := "00000000"
		marker := filepath.Join(planRoot, "variants", "base", "view", "image", filepath.FromSlash(markerRelative))
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, markerContent, 0o644); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(storeRoot, "nodes", node, slot)
		if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		return planRoot, storeRoot, outputRoot
	}
	node := strings.Repeat("a", 64)
	validMarker := "from/" + node + "/00000000/at/result"
	for _, test := range []struct {
		name          string
		marker        string
		markerContent []byte
		expected      int
		preserve      bool
		want          string
	}{
		{name: "wrong count", marker: validMarker, expected: 2, preserve: true, want: "contains 1 markers"},
		{name: "nonempty marker", marker: validMarker, markerContent: []byte("unexpected"), expected: 1, preserve: true, want: "not an empty regular file"},
		{name: "bad digest", marker: "from/not-a-digest/00000000/at/result", expected: 1, preserve: true, want: "unexpected marker directory"},
		{name: "missing mode", marker: validMarker, expected: 1, want: "requires -preserve_mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			planRoot, storeRoot, outputRoot := makeFixture(t, test.marker, test.markerContent)
			err := projectFamilyView(planRoot, "base", "image", storeRoot, outputRoot, test.expected, test.preserve)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("projectFamilyView error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-out", "x"},
		{"-tree_out", "x", "-copy", "../escape=input"},
		{"-tree_out", "x", "-copy", "same=input", "-copy", "same=other"},
		{"-out", "x", "-content_base64", "***"},
		{"-out", "x", "-input", ""},
		{"-out", "x", "-input", "missing", "-content_base64", ""},
		{"-out", "x", "-state", "missing", "-line", "line"},
		{"-out", "x", "-content_base64", "", "-preserve_mode"},
		{"-out", "x", "-input", "first", "-input", "second", "-preserve_mode"},
		{"-family_view_plan_root", "plan"},
		{"-tree_out", "x", "-copy", "same=input", "-state", "state"},
		{"-out", "x", "-content_base64", "", "extra"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}
