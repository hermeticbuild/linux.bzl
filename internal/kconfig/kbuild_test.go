package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func parseCapturedKbuild(t *testing.T, source string, opts KbuildOptions, names ...string) *KbuildFile {
	t.Helper()
	opts.CaptureVariables = append([]string(nil), names...)
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "Kbuild", opts, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	return kb
}

func requireKbuildVariableWords(t *testing.T, kb *KbuildFile, name string, want []string) {
	t.Helper()
	if got := strings.Fields(kb.Variables[name]); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s words mismatch\nwant: %#v\n got: %#v", name, want, got)
	}
}

type testKbuildVirtualFile struct {
	content string
	exact   bool
}

type testKbuildVirtualFileView struct {
	matches    map[string][]string
	files      map[string]testKbuildVirtualFile
	matchCalls []string
	readCalls  []string
}

func (v *testKbuildVirtualFileView) Match(pattern string) []string {
	v.matchCalls = append(v.matchCalls, pattern)
	return append([]string(nil), v.matches[pattern]...)
}

func (v *testKbuildVirtualFileView) Read(path string) (string, bool, bool, error) {
	v.readCalls = append(v.readCalls, path)
	file, exists := v.files[path]
	return file.content, exists, file.exact, nil
}

func TestParseKbuildExpandsMakeVariablesAndFunctions(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`objects := core/main.o generated.h
subdirs := drivers/net firmware
obj-y += $(filter %.o,$(objects)) $(addsuffix /,$(filter drivers/%,$(subdirs)))
targets += $(patsubst %.c,%.o,foo.c bar.S)
CFLAGS_core/main.o += $(filter-out -Wbad,$(sort -Wok -Wbad -Wok))
targets += $(findstring needle,hay needle stack).o $(firstword alpha beta).o $(lastword alpha beta).o word$(word 2,one two three).o count$(words one two three).o
obj-y += $(notdir $(lastword $(MAKEFILE_LIST))).o
`), "Kbuild", KbuildOptions{CaptureVariables: []string{"obj-y", "CFLAGS_core/main.o"}}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	if got, want := kb.Variables["obj-y"], "core/main.o drivers/net/ Kbuild.o"; got != want {
		t.Fatalf("obj-y=%q, want evaluated assignment %q", got, want)
	}
	if got, want := kb.Variables["CFLAGS_core/main.o"], "-Wok"; got != want {
		t.Fatalf("CFLAGS_core/main.o=%q, want %q", got, want)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "targets", target: "foo.o", condKind: "const", state: "y", line: 4},
		{kind: "targets", target: "bar.S", condKind: "const", state: "y", line: 4},
		{kind: "targets", target: "needle.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "alpha.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "beta.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "wordtwo.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "count3.o", condKind: "const", state: "y", line: 6},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}

}

func TestParseKbuildCapturesSelectedVariables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`
setup-y := early.o
setup-y += late.o
selected-flag := -fselected
vmlinux-objs-y := $(setup-y) compressed/vmlinux
	`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:          dir,
		Variables:        map[string]string{"CONFIG_UNUSED": ""},
		CaptureVariables: []string{"setup-y", "selected-flag", "vmlinux-objs-y"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	for name, want := range map[string]string{
		"setup-y":        "early.o late.o",
		"selected-flag":  "-fselected",
		"vmlinux-objs-y": "early.o late.o compressed/vmlinux",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("Variables[%q]=%q, want %q", name, got, want)
		}
	}
}

func TestKbuildVariableBaseSnapshotsNormalizesAndShares(t *testing.T) {
	input := map[string]string{
		"SHARED":  "original",
		"srctree": `shared\kernel`,
	}
	base := NewKbuildVariableBase(input)
	input["SHARED"] = "mutated"
	input["srctree"] = `mutated\kernel`

	first := newKbuildParserWithVariableBase(base, nil, nil, "")
	second := newKbuildParserWithVariableBase(base, nil, nil, "")
	if first.initialVars != base.variables || second.initialVars != base.variables {
		t.Fatal("parsers did not retain the immutable variable base by identity")
	}
	if got, ok := first.lookupVariable("SHARED"); !ok || got.value != "original" {
		t.Fatalf("SHARED = (%q, %t), want (%q, true)", got.value, ok, "original")
	}
	if got, ok := first.lookupVariable("srctree"); !ok || got.value != "shared/kernel" {
		t.Fatalf("srctree = (%q, %t), want (%q, true)", got.value, ok, "shared/kernel")
	}
	if got, ok := first.lookupVariable("MAKE_VERSION"); !ok || got.value != "4.4" {
		t.Fatalf("MAKE_VERSION = (%q, %t), want semantic builtin", got.value, ok)
	}
}

func TestKbuildVariableBaseOverlaysAreNormalizedAndIsolated(t *testing.T) {
	base := NewKbuildVariableBase(map[string]string{
		"SHARED": "base",
		"obj":    `base\object`,
	})
	firstOverlay := map[string]string{
		"FIRST_ONLY": "present",
		"obj":        `drivers\first`,
	}
	first := newKbuildParserWithVariableBase(base, firstOverlay, nil, "")
	second := newKbuildParserWithVariableBase(base, map[string]string{
		"SECOND_ONLY": "present",
		"obj":         `drivers\second`,
	}, nil, "")
	firstOverlay["FIRST_ONLY"] = "mutated"
	firstOverlay["obj"] = `mutated\object`

	if first.initialVars == base.variables || first.initialVars.parent != base.variables {
		t.Fatal("first parser did not retain its sparse overlay over the shared base")
	}
	if second.initialVars == base.variables || second.initialVars.parent != base.variables {
		t.Fatal("second parser did not retain its sparse overlay over the shared base")
	}
	for _, test := range []struct {
		name   string
		parser *kbuildParser
		want   string
	}{
		{name: "first", parser: first, want: "drivers/first"},
		{name: "second", parser: second, want: "drivers/second"},
	} {
		if got, ok := test.parser.lookupVariable("obj"); !ok || got.value != test.want {
			t.Fatalf("%s obj = (%q, %t), want (%q, true)", test.name, got.value, ok, test.want)
		}
	}
	if _, ok := second.lookupVariable("FIRST_ONLY"); ok {
		t.Fatal("second parser inherited the first parser's overlay")
	}
	if got, ok := base.variables.lookup("obj"); !ok || got != "base/object" {
		t.Fatalf("base obj after overlays = (%q, %t), want (%q, true)", got, ok, "base/object")
	}

	first.applyEnvironmentVariables(map[string]string{"SHARED": "first-environment"})
	if got, ok := first.lookupVariable("SHARED"); !ok || got.value != "first-environment" {
		t.Fatalf("first SHARED = (%q, %t), want environment override", got.value, ok)
	}
	if got, ok := second.lookupVariable("SHARED"); !ok || got.value != "base" {
		t.Fatalf("second SHARED = (%q, %t), want isolated base value", got.value, ok)
	}
}

func TestKbuildVariableInputsDoNotPromotePrintableRecursiveMakeMarker(t *testing.T) {
	const marker = "__LINUX_BZL_MAKE__"
	tests := []struct {
		name   string
		parser func() *kbuildParser
	}{
		{
			name: "initial variables",
			parser: func() *kbuildParser {
				return newKbuildParser(map[string]string{"MAKE": marker}, "")
			},
		},
		{
			name: "shared variable base",
			parser: func() *kbuildParser {
				return newKbuildParserWithVariableBase(
					NewKbuildVariableBase(map[string]string{"MAKE": marker}), nil, nil, "",
				)
			},
		},
		{
			name: "variable-base overlay",
			parser: func() *kbuildParser {
				return newKbuildParserWithVariableBase(
					NewKbuildVariableBase(nil), map[string]string{"MAKE": marker}, nil, "",
				)
			},
		},
		{
			name: "parser override",
			parser: func() *kbuildParser {
				return newKbuildParserWithVariableBase(nil, nil, map[string]string{"MAKE": marker}, "")
			},
		},
		{
			name: "environment",
			parser: func() *kbuildParser {
				parser := newKbuildParser(nil, "")
				parser.applyEnvironmentVariables(map[string]string{"MAKE": marker})
				return parser
			},
		},
		{
			name: "command line",
			parser: func() *kbuildParser {
				parser := newKbuildParser(nil, "")
				parser.applyCommandLineVariables(map[string]string{"MAKE": marker}, nil)
				return parser
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			variable, defined := test.parser().lookupVariable("MAKE")
			if !defined || variable.value != marker {
				t.Fatalf("MAKE = (%q, %t), want ordinary printable value %q", variable.value, defined, marker)
			}
			if strings.Contains(variable.value, CompactKbuildRecursiveMakeProvenanceToken) {
				t.Fatalf("printable MAKE input acquired private provenance: %q", variable.value)
			}
		})
	}
}

func TestKbuildRecursiveMakeDefaultIsTrustedAndRetainsMakePrecedence(t *testing.T) {
	input := map[string]string{"PROFILE": "ordinary"}
	base, err := NewKbuildVariableBaseWithRecursiveMakeDefault(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, mutated := input["MAKE"]; mutated {
		t.Fatalf("recursive Make constructor mutated caller input: %#v", input)
	}
	if got, ok := base.variables.lookup("MAKE"); !ok || got != CompactKbuildRecursiveMakeProvenanceToken {
		t.Fatalf("trusted MAKE default = (%q, %t), want private capability", got, ok)
	}

	parsed, err := parseKbuildWithOptions(strings.NewReader("command := $(MAKE) child\n"), "Kbuild", KbuildOptions{
		VariableBase:     base,
		CaptureVariables: []string{"command"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Variables["command"], CompactKbuildRecursiveMakeProvenanceToken+" child"; got != want {
		t.Fatalf("trusted recursive command = %q, want %q", got, want)
	}

	parsed, err = parseKbuildWithOptions(strings.NewReader("command := $(MAKE) child\n"), "Kbuild", KbuildOptions{
		VariableBase:         base,
		EnvironmentVariables: map[string]string{"MAKE": "configured-make"},
		CaptureVariables:     []string{"command"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Variables["command"], "configured-make child"; got != want {
		t.Fatalf("environment MAKE override = %q, want %q", got, want)
	}

	printable, err := NewKbuildVariableBaseWithRecursiveMakeDefault(map[string]string{
		"MAKE": compactKbuildRecursiveMakeMarker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := printable.variables.lookup("MAKE"); !ok || got != compactKbuildRecursiveMakeMarker {
		t.Fatalf("printable MAKE override = (%q, %t), want ordinary marker", got, ok)
	}
}

func TestKbuildRecursiveMakeDefaultRejectsPrivateBytesInOrdinaryVariables(t *testing.T) {
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		for _, input := range []struct {
			name      string
			variables map[string]string
		}{
			{name: "variable name", variables: map[string]string{"PRIVATE" + boundary.value: "value"}},
			{name: "variable value", variables: map[string]string{"PRIVATE": "value" + boundary.value}},
		} {
			t.Run(boundary.name+"/"+input.name, func(t *testing.T) {
				_, err := NewKbuildVariableBaseWithRecursiveMakeDefault(input.variables)
				if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("NewKbuildVariableBaseWithRecursiveMakeDefault() error = %v, want reserved-provenance rejection", err)
				}
			})
		}
	}
}

func TestParseKbuildFileTreeReusesVariableBaseAcrossInvocationOverlays(t *testing.T) {
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte(`captured := $(SHARED)|$(obj)|$(srctree)|$(PROFILE)|$(COMMAND)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	input := map[string]string{
		"SHARED":  "stable",
		"srctree": `shared\source`,
	}
	base := NewKbuildVariableBase(input)
	input["SHARED"] = "mutated"
	delete(input, "srctree")

	for _, test := range []struct {
		name    string
		object  string
		profile string
		want    string
	}{
		{name: "first", object: `drivers\first`, profile: "one", want: "stable|drivers/first|shared/source|one|pinned"},
		{name: "second", object: `drivers\second`, profile: "two", want: "stable|drivers/second|shared/source|two|pinned"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
				RootDir:              root,
				VariableBase:         base,
				Variables:            map[string]string{"obj": test.object},
				EnvironmentVariables: map[string]string{"PROFILE": test.profile},
				CommandLineVariables: map[string]string{"COMMAND": "pinned"},
				CaptureVariables:     []string{"captured"},
			})
			if err != nil {
				t.Fatalf("ParseKbuildFileTree() failed: %v", err)
			}
			if got := parsed.Variables["captured"]; got != test.want {
				t.Fatalf("captured = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseKbuildFileTreeResolvesSentinelIncludeRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "Makefile.build"), []byte(`
include $(objtree)/include/config/auto.conf
include $(srctree)/scripts/Kbuild.include
selected := $(sentinel-definition)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "Kbuild.include"), []byte("sentinel-definition := source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(filepath.Join(root, "scripts", "Makefile.build"), KbuildOptions{
		RootDir:                 root,
		SourceRoots:             map[string]string{"__LINUX_BZL_SOURCE_TREE__": root, "__LINUX_BZL_OBJECT_TREE__": root},
		Variables:               map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__", "objtree": "__LINUX_BZL_OBJECT_TREE__"},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureVariables:        []string{"selected"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	if got, want := kb.Variables["selected"], "source"; got != want {
		t.Fatalf("selected=%q, want %q", got, want)
	}
}

func TestParseKbuildFileTreeKeepsSentinelsAcrossFilesystemFunctions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("source-derived\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("payload", filepath.Join(root, "payload-link")); err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(root, "scripts", "Makefile")
	if err := os.WriteFile(makefile, []byte(`
contents := $(file <$(srctree)/payload)
absolute := $(abspath $(srctree)/scripts/../payload)
real := $(realpath $(srctree)/payload-link)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
		RootDir:                 root,
		SourceRoots:             map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		Variables:               map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__"},
		CommandLineVariables:    map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__"},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureVariables:        []string{"contents", "absolute", "real"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	if got, want := kb.Variables["contents"], "source-derived"; got != want {
		t.Fatalf("contents=%q, want %q", got, want)
	}
	for _, name := range []string{"absolute", "real"} {
		if got, want := kb.Variables[name], "__LINUX_BZL_SOURCE_TREE__/payload"; got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
}

func TestParseKbuildReadsExactVirtualFileContents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modules.order"), []byte("stale-physical.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`empty :=
space := $(empty) $(empty)
define newline


endef
read-file = $(subst $(newline),$(space),$(file < $1))
modules := $(call read-file,./modules.order)
empty-read := $(file < empty.order)
visible := $(sort $(wildcard modules.order empty.order))
`), "Kbuild", KbuildOptions{
		WorkingDir: dir,
		VirtualFileView: &testKbuildVirtualFileView{
			matches: map[string][]string{
				"modules.order": {"modules.order"},
				"empty.order":   {"empty.order"},
			},
			files: map[string]testKbuildVirtualFile{
				"modules.order": {content: "first.o\nnested/second.o\n", exact: true},
				"empty.order":   {exact: true},
			},
		},
		CaptureVariables: []string{"modules", "empty-read", "visible"},
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	if got, want := strings.Fields(kb.Variables["modules"]), []string{"first.o", "nested/second.o"}; !slices.Equal(got, want) {
		t.Fatalf("modules words = %q, want %q", got, want)
	}
	if got := kb.Variables["empty-read"]; got != "" {
		t.Fatalf("empty-read = %q, want exact empty content", got)
	}
	if got, want := strings.Fields(kb.Variables["visible"]), []string{"empty.order", "modules.order"}; !slices.Equal(got, want) {
		t.Fatalf("visible files = %q, want %q", got, want)
	}
}

func TestParseKbuildRejectsRecursiveMakeProvenanceFromFilesystemFunctions(t *testing.T) {
	boundaries := []struct {
		name  string
		value string
	}{
		{name: "opening delimiter", value: "\x05"},
		{name: "closing delimiter", value: "\x06"},
	}
	ingresses := []string{
		"filesystem file contents",
		"virtual file contents",
		"filesystem wildcard filename",
		"virtual wildcard filename",
		"resolved symlink filename",
	}
	for _, ingress := range ingresses {
		for _, boundary := range boundaries {
			t.Run(ingress+"/"+boundary.name, func(t *testing.T) {
				root := t.TempDir()
				opts := KbuildOptions{
					WorkingDir:       root,
					CaptureVariables: []string{"value"},
				}
				source := ""
				value := "prefix" + boundary.value + "suffix"
				switch ingress {
				case "filesystem file contents":
					if err := os.WriteFile(filepath.Join(root, "payload"), []byte(value+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					source = "value := $(file < payload)\n"
				case "virtual file contents":
					opts.VirtualFileView = &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{
						"payload": {content: value + "\n", exact: true},
					}}
					source = "value := $(file < payload)\n"
				case "filesystem wildcard filename":
					if err := os.MkdirAll(filepath.Join(root, "matches"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "matches", value), nil, 0o644); err != nil {
						t.Fatal(err)
					}
					source = "value := $(wildcard matches/*)\n"
				case "virtual wildcard filename":
					opts.VirtualFileView = &testKbuildVirtualFileView{matches: map[string][]string{
						"matches/*": {"matches/" + value},
					}}
					source = "value := $(wildcard matches/*)\n"
				case "resolved symlink filename":
					if err := os.WriteFile(filepath.Join(root, value), nil, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(value, filepath.Join(root, "selected")); err != nil {
						t.Fatal(err)
					}
					source = "value := $(notdir $(realpath selected))\n"
				default:
					t.Fatalf("unknown test ingress %q", ingress)
				}

				_, err := parseKbuildWithOptions(strings.NewReader(source), "Kbuild", opts, root)
				if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("parseKbuildWithOptions() error = %v, want reserved-provenance rejection", err)
				}
			})
		}
	}
}

func TestParseKbuildRejectsSplitToolsetPathProvenanceAtOrdinaryIngress(t *testing.T) {
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	terminator := strings.LastIndex(token, toolaction.ExecutionRootProvenanceTerminator)
	if terminator <= 0 {
		t.Fatalf("toolset token %q has no terminator", token)
	}
	pieces := []string{token[:terminator], token[terminator:]}
	for index, piece := range pieces {
		t.Run(fmt.Sprintf("filesystem piece %d", index), func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "piece"), []byte(piece+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := parseKbuildWithOptions(strings.NewReader("value := $(file < piece)\n"), "Kbuild", KbuildOptions{
				WorkingDir: root, CaptureVariables: []string{"value"},
			}, root)
			if err == nil || !strings.Contains(err.Error(), "reserved toolset-path provenance byte") {
				t.Fatalf("filesystem token piece error = %v, want toolset-path provenance rejection", err)
			}
		})
		t.Run(fmt.Sprintf("virtual piece %d", index), func(t *testing.T) {
			_, err := parseKbuildWithOptions(strings.NewReader("value := $(file < piece)\n"), "Kbuild", KbuildOptions{
				CaptureVariables: []string{"value"},
				VirtualFileView: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{
					"piece": {content: piece + "\n", exact: true},
				}},
			}, "")
			if err == nil || !strings.Contains(err.Error(), "reserved toolset-path provenance byte") {
				t.Fatalf("virtual token piece error = %v, want toolset-path provenance rejection", err)
			}
		})
	}

	for _, boundary := range []string{"\x07", "\x08"} {
		_, err := parseKbuildWithOptions(strings.NewReader("part := "+boundary+"\nvalue := $(part)\n"), "Kbuild", KbuildOptions{
			CaptureVariables: []string{"value"},
		}, "")
		if err == nil || !strings.Contains(err.Error(), "reserved literal-marker byte") {
			t.Fatalf("source token boundary %q error = %v, want source-ingress rejection", boundary, err)
		}
		if _, err := NewKbuildVariableBaseWithRecursiveMakeDefault(map[string]string{"PART": boundary}); err == nil ||
			!strings.Contains(err.Error(), "reserved toolset-path provenance byte") {
			t.Fatalf("configured token boundary %q error = %v, want variable-ingress rejection", boundary, err)
		}
	}
}

func TestParseKbuildFilesystemFunctionProvenanceGuardPreservesOrdinaryValues(t *testing.T) {
	root := t.TempDir()
	ordinaryContents := "ordinary " + compactKbuildRecursiveMakeMarker + " bytes"
	if err := os.WriteFile(filepath.Join(root, "physical.payload"), []byte(ordinaryContents+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "matches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "matches", "physical.o"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("matches/physical.o", filepath.Join(root, "ordinary-link")); err != nil {
		t.Fatal(err)
	}
	view := &testKbuildVirtualFileView{
		matches: map[string][]string{"virtual/*": {"virtual/generated.o"}},
		files: map[string]testKbuildVirtualFile{
			"virtual.payload": {content: ordinaryContents + "\n", exact: true},
		},
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`physical := $(file < physical.payload)
virtual := $(file < virtual.payload)
matches := $(sort $(wildcard matches/* virtual/*))
resolved := $(notdir $(realpath ordinary-link))
`), "Kbuild", KbuildOptions{
		WorkingDir:       root,
		VirtualFileView:  view,
		CaptureVariables: []string{"physical", "virtual", "matches", "resolved"},
	}, root)
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	for _, name := range []string{"physical", "virtual"} {
		if got := kb.Variables[name]; got != ordinaryContents {
			t.Errorf("%s = %q, want %q", name, got, ordinaryContents)
		}
	}
	if got, want := kb.Variables["matches"], "matches/physical.o virtual/generated.o"; got != want {
		t.Errorf("matches = %q, want %q", got, want)
	}
	if got, want := kb.Variables["resolved"], "physical.o"; got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

func TestParseKbuildRejectsRecursiveMakeProvenanceFromMakefileListFilename(t *testing.T) {
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		t.Run(boundary.name, func(t *testing.T) {
			_, err := parseKbuildWithOptions(
				strings.NewReader("value := $(lastword $(MAKEFILE_LIST))\n"),
				"prefix"+boundary.value+"suffix/Makefile",
				KbuildOptions{CaptureVariables: []string{"value"}},
				"",
			)
			if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
				t.Fatalf("parseKbuildWithOptions() error = %v, want MAKEFILE_LIST provenance rejection", err)
			}
		})
	}

	filename := "ordinary/" + compactKbuildRecursiveMakeMarker + "/Makefile"
	parsed, err := parseKbuildWithOptions(
		strings.NewReader("value := $(lastword $(MAKEFILE_LIST))\n"),
		filename,
		KbuildOptions{CaptureVariables: []string{"value"}},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Variables["value"]; got != filename {
		t.Fatalf("printable MAKEFILE_LIST filename = %q, want %q", got, filename)
	}
}

func TestParseKbuildRejectsRecursiveMakeProvenanceFromParserPaths(t *testing.T) {
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		for _, ingress := range []string{"working directory abspath", "base directory abspath", "root srctree"} {
			t.Run(ingress+"/"+boundary.name, func(t *testing.T) {
				path := filepath.Join("configured", "prefix"+boundary.value+"suffix")
				var err error
				switch ingress {
				case "working directory abspath":
					_, err = parseKbuildWithOptions(
						strings.NewReader("value := $(abspath .)\n"), "Kbuild",
						KbuildOptions{WorkingDir: path, CaptureVariables: []string{"value"}}, "",
					)
				case "base directory abspath":
					_, err = parseKbuildWithOptions(
						strings.NewReader("value := $(abspath .)\n"), "Kbuild",
						KbuildOptions{CaptureVariables: []string{"value"}}, path,
					)
				case "root srctree":
					makefile := filepath.Join(t.TempDir(), "Makefile")
					if writeErr := os.WriteFile(makefile, []byte("value := $(srctree)\n"), 0o644); writeErr != nil {
						t.Fatal(writeErr)
					}
					_, err = ParseKbuildFileTree(makefile, KbuildOptions{
						RootDir: path, CaptureVariables: []string{"value"},
					})
				default:
					t.Fatalf("unknown parser path ingress %q", ingress)
				}
				if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("Kbuild parse error = %v, want parser-path provenance rejection", err)
				}
			})
		}
	}

	ordinary := filepath.Join("configured", compactKbuildRecursiveMakeMarker)
	parsed, err := parseKbuildWithOptions(
		strings.NewReader("value := $(abspath .)\n"), "Kbuild",
		KbuildOptions{WorkingDir: ordinary, CaptureVariables: []string{"value"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Variables["value"], filepath.ToSlash(filepath.Clean(ordinary)); got != want {
		t.Fatalf("printable abspath = %q, want %q", got, want)
	}
}

func TestParseKbuildMergesVirtualFileViewWithPhysicalFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "physical.o"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	view := &testKbuildVirtualFileView{
		matches: map[string][]string{
			"*.o":           {"lazy.o", "list.o"},
			"generated/*.o": {"generated/lazy.o"},
		},
		files: map[string]testKbuildVirtualFile{
			"generated/exact.order": {content: "lazy.o\nnested/next.o\n", exact: true},
			"list.order":            {content: "from-list.o\n", exact: true},
		},
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`visible := $(sort $(wildcard *.o generated/*.o))
exact := $(file < ./generated/exact.order)
fallback := $(file < list.order)
`), "Kbuild", KbuildOptions{
		WorkingDir:       dir,
		VirtualFileView:  view,
		CaptureVariables: []string{"visible", "exact", "fallback"},
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	if got, want := kb.Variables["visible"], "generated/lazy.o lazy.o list.o physical.o"; got != want {
		t.Fatalf("visible = %q, want merged wildcard result %q", got, want)
	}
	if got, want := kb.Variables["exact"], "lazy.o\nnested/next.o"; got != want {
		t.Fatalf("exact = %q, want %q", got, want)
	}
	if got, want := kb.Variables["fallback"], "from-list.o"; got != want {
		t.Fatalf("fallback = %q, want list-backed contents %q", got, want)
	}
	if got, want := view.matchCalls, []string{"*.o", "generated/*.o"}; !slices.Equal(got, want) {
		t.Fatalf("Match calls = %q, want %q", got, want)
	}
	if got, want := view.readCalls, []string{"generated/exact.order", "list.order"}; !slices.Equal(got, want) {
		t.Fatalf("Read calls = %q, want canonical paths %q", got, want)
	}
}

func TestParseKbuildLazyVirtualFileViewUnknownContentsTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modules.order"), []byte("stale-physical.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view := &testKbuildVirtualFileView{
		files: map[string]testKbuildVirtualFile{
			"modules.order": {},
		},
	}
	_, err := parseKbuildWithOptions(strings.NewReader(`modules := $(file < modules.order)
`), "Kbuild", KbuildOptions{
		WorkingDir:       dir,
		VirtualFileView:  view,
		CaptureVariables: []string{"modules"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), `visible virtual file "modules.order" requires exact contents`) {
		t.Fatalf("parseKbuildWithOptions() error = %v, want lazy unknown-content failure", err)
	}
}

func TestKbuildLazyVirtualFileViewIsRetainedByCapturedEvaluator(t *testing.T) {
	view := &testKbuildVirtualFileView{
		matches: map[string][]string{
			"late/*.o": {"late/generated.o"},
		},
		files: map[string]testKbuildVirtualFile{
			"late.order": {content: "late/generated.o\n", exact: true},
		},
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(""), "Kbuild", KbuildOptions{
		VirtualFileView:        view,
		CaptureTargetEvaluator: true,
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	if kb.evaluator == nil || kb.evaluator.template == nil {
		t.Fatal("captured Kbuild evaluator is missing")
	}
	if kb.evaluator.template.virtualFileView != view {
		t.Fatal("captured Kbuild evaluator did not retain the lazy virtual-file view")
	}

	// Parsing the empty file does not query the view. A later target-evaluation
	// clone must still query it instead of an eagerly materialized snapshot.
	parser := cloneKbuildParserForTargetEvaluation(kb.evaluator.template, 0, false)
	if parser.virtualFileView != view {
		t.Fatal("target-evaluation clone did not retain the lazy virtual-file view")
	}
	gotWildcard, err := parser.expandWildcard("late/*.o")
	if err != nil {
		t.Fatalf("late wildcard failed: %v", err)
	}
	if got, want := gotWildcard, "late/generated.o"; got != want {
		t.Fatalf("late wildcard = %q, want %q", got, want)
	}
	got, err := parser.makeFile("< ./late/../late.order", "$(file < ./late/../late.order)")
	if err != nil {
		t.Fatalf("late exact read failed: %v", err)
	}
	if want := "late/generated.o"; got != want {
		t.Fatalf("late exact read = %q, want %q", got, want)
	}
}

func TestParseKbuildRejectsVisibleVirtualFileWithoutContents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "modules.order"), []byte("stale-physical.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parseKbuildWithOptions(strings.NewReader(`modules := $(file < modules.order)
`), "Kbuild", KbuildOptions{
		WorkingDir: dir,
		VirtualFileView: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{
			"modules.order": {},
		}},
		CaptureVariables: []string{"modules"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), `visible virtual file "modules.order" requires exact contents`) {
		t.Fatalf("parseKbuildWithOptions() error = %v, want unknown virtual-content failure", err)
	}
}

func TestParseKbuildFileTreeResolvesRelativeIncludeFromInvocationWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	driver := filepath.Join(root, "tools", "build", "Makefile.build")
	workingDirectory := filepath.Join(root, "tools", "objtool")
	if err := os.MkdirAll(filepath.Dir(driver), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, []byte("include Build.linux-bzl\nselected := $(driver-input)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDirectory, "Build.linux-bzl"), []byte("driver-input := objtool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(driver, KbuildOptions{
		RootDir: root, WorkingDir: workingDirectory,
		MakeVariablesComplete: true, CaptureVariables: []string{"selected"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	if got, want := kb.Variables["selected"], "objtool"; got != want {
		t.Fatalf("selected=%q, want %q", got, want)
	}
}

func TestParseKbuildSelectedVariablesDoNotRetainResolvedConfigEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`selected := $(CONFIG_SELECTED)
local-empty :=
`), 0o644); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{"CONFIG_SELECTED": "y", "ARCH": "x86"}
	for index := 0; index < 20000; index++ {
		variables[fmt.Sprintf("CONFIG_UNUSED_%05d", index)] = ""
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:          dir,
		Variables:        variables,
		CaptureVariables: []string{"selected", "ARCH"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["selected"], "y"; got != want {
		t.Fatalf("selected=%q, want %q", got, want)
	}
	if _, retained := kb.Variables["CONFIG_SELECTED"]; retained {
		t.Fatal("final profile snapshot retained resolved CONFIG_SELECTED input")
	}
	if _, retained := kb.Variables["CONFIG_UNUSED_19999"]; retained {
		t.Fatal("final profile snapshot retained unused resolved config input")
	}
	if _, retained := kb.Variables["local-empty"]; retained {
		t.Fatal("final profile snapshot retained an empty value")
	}
	if got, want := kb.Variables["ARCH"], "x86"; got != want {
		t.Fatalf("inherited non-config ARCH=%q, want %q", got, want)
	}
	if len(kb.Variables) >= 100 {
		t.Fatalf("compact profile retained %d values from a 20,000-symbol invocation", len(kb.Variables))
	}
}

func TestParseKbuildShellRequiresHermeticEvaluator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte("selected = $(shell selected-tool --print identity)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKbuildFileTree(path, KbuildOptions{MakeVariablesComplete: true}); err != nil {
		t.Fatalf("unused recursive shell definition was evaluated eagerly: %v", err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{MakeVariablesComplete: true})
	if err != nil {
		t.Fatalf("unobserved recursive shell definition was evaluated by snapshot: %v", err)
	}
	if _, ok := kb.Variables["selected"]; ok {
		t.Fatal("snapshot retained an unobserved shell-bearing helper")
	}
	if err := os.WriteFile(path, []byte("selected = $(shell selected-tool --print identity)\nobj-y += $(selected)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKbuildFileTree(path, KbuildOptions{
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"obj-y"},
	}); err == nil || !strings.Contains(err.Error(), "requires a hermetic evaluator") {
		t.Fatalf("selected graph missing shell evaluator error = %v", err)
	}
	var command string
	kb, err = ParseKbuildFileTree(path, KbuildOptions{
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"obj-y"},
		Shell: func(value string) (string, error) {
			command = value
			return "selected.o\n", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := command, "selected-tool --print identity"; got != want {
		t.Fatalf("shell command=%q, want %q", got, want)
	}
	if got, want := kb.Variables["obj-y"], "selected.o"; got != want {
		t.Fatalf("obj-y=%q, want %q", got, want)
	}
}

func TestParseKbuildRecursiveDefinitionsAreLazyAndCompact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	var source strings.Builder
	const definitions = 10000
	for index := 0; index < definitions; index++ {
		fmt.Fprintf(&source, "cmd_%05d = prefix-$(shell probe %05d)-suffix\n", index, index)
	}
	if err := os.WriteFile(path, []byte(source.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	kb, err := ParseKbuildFileTree(path, KbuildOptions{Shell: func(string) (string, error) {
		calls++
		return "unexpected", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("parsing %d unused recursive command definitions invoked shell %d times", definitions, calls)
	}
	if kb.Variables != nil {
		t.Fatalf("profile snapshot materialized unrequested variables: %d entries", len(kb.Variables))
	}
}

func TestParseKbuildCompleteMakeEnvironmentExpandsMissingVariablesEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`
export KBUILD_CPPFLAGS := -D__KERNEL__
KBUILD_CPPFLAGS += $(KCPPFLAGS)
selected := before $(UNCONFIGURED_FLAGS) after
	`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:               dir,
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"KBUILD_CPPFLAGS", "selected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["KBUILD_CPPFLAGS"], "-D__KERNEL__"; got != want {
		t.Fatalf("KBUILD_CPPFLAGS=%q, want %q", got, want)
	}
	if got, want := kb.Variables["selected"], "before  after"; got != want {
		t.Fatalf("selected=%q, want exact empty expansion %q", got, want)
	}
}

func TestParseKbuildFinalSnapshotPreservesAutomaticVariableTemplates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`
obj := arch/x86/boot
cmd_image = $(obj)/tools/build $(obj)/setup.bin $(obj)/vmlinux.bin $(obj)/zoffset.h $@
cmd_inputs = $< $^ $+ $? $| $* $(@D) $(@F)
missing = before $(UNCONFIGURED) after
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:               dir,
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"cmd_image", "cmd_inputs", "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["cmd_image"], "arch/x86/boot/tools/build arch/x86/boot/setup.bin arch/x86/boot/vmlinux.bin arch/x86/boot/zoffset.h $@"; got != want {
		t.Fatalf("cmd_image=%q, want preserved template %q", got, want)
	}
	if got, want := kb.Variables["cmd_inputs"], "$< $^ $+ $? $| $* $(@D) $(@F)"; got != want {
		t.Fatalf("cmd_inputs=%q, want preserved template %q", got, want)
	}
	if got, want := kb.Variables["missing"], "before  after"; got != want {
		t.Fatalf("missing=%q, want complete-environment expansion %q", got, want)
	}
}

func TestParseKbuildProvidesSemanticGNUmakeBuiltins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`
ifeq ($(filter output-sync,$(.FEATURES)),)
$(error GNU Make >= 4.0 is required. Your Make version is $(MAKE_VERSION))
endif
selected-version := $(MAKE_VERSION)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:               dir,
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"selected-version", ".FEATURES"},
	})
	if err != nil {
		t.Fatalf("Linux GNU Make feature gate rejected semantic parser built-ins: %v", err)
	}
	if got, want := kb.Variables["selected-version"], "4.4"; got != want {
		t.Fatalf("selected-version=%q, want %q", got, want)
	}
	if got := strings.Fields(kb.Variables[".FEATURES"]); !slices.Contains(got, "output-sync") {
		t.Fatalf(".FEATURES=%q omits output-sync", got)
	}
}

func TestParseKbuildExpandsAdditionalPureMakeFunctions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.o"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile(existing.o) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects.list"), []byte("from-file.o\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(objects.list) failed: %v", err)
	}
	kbuildPath := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuildPath, []byte(`objects := one.o two.o three.o four.o
obj-y += $(wordlist 2,3,$(objects)) $(join joined-,one.o)
obj-y += $(notdir $(abspath rel.o)) $(notdir $(realpath existing.o)) $(notdir $(realpath missing.o))
ifeq ($(intcmp 1,0,,,y),y)
test-ge = $(intcmp $(strip $1)0,$(strip $2)0,,ge.o,ge.o)
test-gt = $(intcmp $(strip $1)0,$(strip $2)0,,,gt.o)
endif
obj-y += $(intcmp 1,1,lt.o,eq.o,gt.o) $(intcmp 0,1,lt.o,eq.o,gt.o) $(call test-ge,12,10) $(call test-gt,12,10)
obj-y += $(file < objects.list) $(file < missing.list)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}

	kb, err := ParseKbuildFileWithOptions(kbuildPath, KbuildOptions{CaptureVariables: []string{"obj-y"}})
	if err != nil {
		t.Fatalf("ParseKbuildFile() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"two.o", "three.o", "joined-one.o", "rel.o", "existing.o",
		"eq.o", "lt.o", "ge.o", "gt.o", "from-file.o",
	})
}

func TestParseKbuildEvaluatesLazyConditionalFunctions(t *testing.T) {
	kb := parseCapturedKbuild(t, `enabled := y
disabled :=
obj-y += $(if $(enabled),if-then.o,$(unknown_if_then))
obj-y += $(if $(disabled),$(unknown_if_else),if-else.o)
obj-y += $(or or-first.o,$(unknown_or))
obj-y += $(and $(disabled),$(unknown_and))
obj-y += $(and $(enabled),and-last.o)
obj-y += $(if $(disabled),$(error inactive error branch),diagnostic-else.o)
obj-y += $(info parser note)$(warning parser warning)diagnostic.o
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"if-then.o", "if-else.o", "or-first.o", "and-last.o", "diagnostic-else.o", "diagnostic.o",
	})

	_, err := ParseKbuild(strings.NewReader(`$(error active failure)
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with active $(error)")
	}
}

func TestParseKbuildDefersErrorsInUnknownConditionalBranches(t *testing.T) {
	_, err := ParseKbuild(strings.NewReader(`ifdef CONFIG_UNKNOWN
$(error config branch is only maybe active)
obj-y += maybe.o
else ifeq ($(unresolved),y)
$(error else-if branch is also maybe active)
obj-y += maybe-elseif.o
else
obj-y += fallback.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	_, err = ParseKbuild(strings.NewReader(`enabled := y
ifeq ($(enabled),y)
$(error known active failure)
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with definitely active $(error)")
	}
}

func TestParseKbuildConditionalExpansionErrorsRequireCompleteActiveEvaluation(t *testing.T) {
	const source = `ifeq ($(shell source-owned-failure),y)
visited += then
else
visited += else
endif
`
	shell := func(command string) (string, error) {
		return "", fmt.Errorf("cannot evaluate %q", command)
	}
	_, err := parseKbuildWithOptions(strings.NewReader(source), "Kbuild", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		Shell:                   shell,
	}, "")
	if err == nil || !strings.Contains(err.Error(), "expand left conditional operand") || !strings.Contains(err.Error(), "source-owned-failure") {
		t.Fatalf("complete conditional expansion error = %v", err)
	}

	for _, test := range []struct {
		name                         string
		configComplete, makeComplete bool
	}{
		{name: "config incomplete", makeComplete: true},
		{name: "make incomplete", configComplete: true},
		{name: "both incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			kb, err := parseKbuildWithOptions(strings.NewReader(source), "Kbuild", KbuildOptions{
				ConfigVariablesComplete: test.configComplete,
				MakeVariablesComplete:   test.makeComplete,
				Shell:                   shell,
				CaptureVariables:        []string{"visited"},
			}, "")
			if err != nil {
				t.Fatalf("incomplete conditional evaluation failed: %v", err)
			}
			requireKbuildVariableWords(t, kb, "visited", []string{"then", "else"})
		})
	}

	_, err = parseKbuildWithOptions(strings.NewReader(`ifeq (no,yes)
ifeq ($(shell source-owned-failure),y)
obj-y += unreachable.o
endif
endif
`), "Kbuild", KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		Shell:                   shell,
	}, "")
	if err != nil {
		t.Fatalf("inactive nested conditional propagated expansion error: %v", err)
	}
}

func TestParseKbuildStripsOnlyTopLevelComments(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`obj-y += before.o # trailing comment.o
obj-y += $(shell grep -Ev '^#|^$$' params) after.o # another comment.o
`), "Kbuild", KbuildOptions{CaptureVariables: []string{"obj-y"}, Shell: func(command string) (string, error) {
		if command != "grep -Ev '^#|^$' params" {
			return "", fmt.Errorf("unexpected shell command %q", command)
		}
		return "", nil
	}}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{"before.o", "after.o"})
}

func TestParseKbuildUnescapesTopLevelCommentHashesLikeGNUMake(t *testing.T) {
	kb := parseCapturedKbuild(t, `one := \#suffix
two := \\#suffix
three := \\\#suffix
nested := $(subst z,z,\#)
trailing := before \# literal # comment
`, KbuildOptions{}, "one", "two", "three", "nested", "trailing")
	for name, want := range map[string]string{
		"one":      "#suffix",
		"two":      `\`,
		"three":    `\#suffix`,
		"nested":   `\#`,
		"trailing": "before # literal",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestParseKbuildSplitsQuotedFlagWords(t *testing.T) {
	kb := parseCapturedKbuild(t, `ccflags-y += -D'pr_fmt(fmt)=KBUILD_MODNAME ": " fmt' -DDEFAULT_SYMBOL_NAMESPACE='"USB_STORAGE"'
obj-y += test.o
`, KbuildOptions{}, "ccflags-y")
	want := []string{
		`-Dpr_fmt(fmt)=KBUILD_MODNAME ": " fmt`,
		`-DDEFAULT_SYMBOL_NAMESPACE="USB_STORAGE"`,
	}
	if got := kbuildFields(kb.Variables["ccflags-y"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildExpandsAssignmentLHSVariables(t *testing.T) {
	kb := parseCapturedKbuild(t, `obj-$(CONFIG_DRIVER) += driver.o
driver-$(CONFIG_MMU) := mmu.o
driver-y += always.o
`, KbuildOptions{Variables: map[string]string{"CONFIG_DRIVER": "y", "CONFIG_MMU": "y"}}, "obj-y", "driver-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"driver.o"})
	requireKbuildVariableWords(t, kb, "driver-y", []string{"mmu.o", "always.o"})
}

func TestParseKbuildExpandsCallAndForeachMacros(t *testing.T) {
	kb := parseCapturedKbuild(t, `suffix-search = $(strip $(foreach s,$3,$($(1:%$(strip $2)=%$s))))
real-search = $(foreach m,$1,$(if $(call suffix-search,$m,$2,$3 -),$(call suffix-search,$m,$2,$3),$m))
objects := composite.o single.o
composite-y := core.o
composite-objs := base.o
obj-y += $(call real-search,$(objects),.o,-objs -y)
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"base.o", "core.o", "single.o"})
}

func TestParseKbuildExpandsSuffixSubstitutionReferences(t *testing.T) {
	kb := parseCapturedKbuild(t, `objects := intel-uncore.o plain
stripped := $(objects:.o=)
renamed := $(objects:.o=.ko)
pattern := $(objects:%.o=built/%.ko)
`, KbuildOptions{}, "stripped", "renamed", "pattern")

	for name, want := range map[string]string{
		"stripped": "intel-uncore plain",
		"renamed":  "intel-uncore.ko plain",
		"pattern":  "built/intel-uncore.ko plain",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestParseKbuildExpandsComputedNamesWithActiveAndEscapedDollars(t *testing.T) {
	kb := parseCapturedKbuild(t, `ordinary_active = $(value_$(selector))
ordinary_escaped = $(value_$$*)
ordinary_mixed = $(value_$(selector)_$$*)
substitution_active = $(objects_$(selector):.o=.ko)
substitution_escaped = $(objects_$$*:.o=.ko)
substitution_mixed = $(objects_$(selector)_$$*:%.o=built/%.ko)
`, KbuildOptions{Variables: map[string]string{
		"selector":      "32",
		"value_32":      "active",
		"value_$*":      "escaped",
		"value_32_$*":   "mixed",
		"objects_32":    "active.o plain",
		"objects_$*":    "escaped.o plain",
		"objects_32_$*": "mixed.o plain",
	}},
		"ordinary_active",
		"ordinary_escaped",
		"ordinary_mixed",
		"substitution_active",
		"substitution_escaped",
		"substitution_mixed",
	)

	for name, want := range map[string]string{
		"ordinary_active":      "active",
		"ordinary_escaped":     "escaped",
		"ordinary_mixed":       "mixed",
		"substitution_active":  "active.ko plain",
		"substitution_escaped": "escaped.ko plain",
		"substitution_mixed":   "built/mixed.ko plain",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestParseKbuildExpandsShortPositionalCapabilityMacro(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`cc-option = $(1)
cc-option-yn = $(if $(call cc-option,$1),y,n)
obj-$(call cc-option-yn,-fsupported) += selected.o
`), "scripts/Makefile.compiler", KbuildOptions{
		Variables:        map[string]string{"SRCARCH": "x86"},
		CaptureVariables: []string{"obj-y"},
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	if got, want := kb.Variables["obj-y"], "selected.o"; got != want {
		t.Fatalf("obj-y=%q, want %q", got, want)
	}
}

func TestParseKbuildTreeUsesCompleteConfigInsteadOfGeneratedMakeIncludes(t *testing.T) {
	dir := t.TempDir()
	makefile := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(makefile, []byte(`include $(objtree)/include/config/auto.conf
include include/config/auto.conf.cmd
ccflags-$(CONFIG_SELECTED) += -DSELECTED
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
		RootDir:                 dir,
		Variables:               map[string]string{"CONFIG_SELECTED": "y", "objtree": dir},
		ConfigVariablesComplete: true,
		CaptureVariables:        []string{"ccflags-y"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	if got, want := kb.Variables["ccflags-y"], "-DSELECTED"; got != want {
		t.Fatalf("ccflags-y=%q, want %q", got, want)
	}
}

func TestParseKbuildExpandsLetFunction(t *testing.T) {
	kb := parseCapturedKbuild(t, `OUTPUT := global
$(let OUTPUT,$(OUTPUT)/,$(eval obj-y += $(OUTPUT)scoped.o))
obj-y += $(OUTPUT).o
obj-y += $(let first rest,one two three,$(first).o $(lastword $(rest)).o)
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"global/scoped.o", "global.o", "one.o", "three.o"})
}

func TestParseKbuildPreservesRecursiveVariableReferences(t *testing.T) {
	kb := parseCapturedKbuild(t, `recursive = $(recursive) hidden.o
obj-y += $(recursive) visible.o
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"$(recursive)", "hidden.o", "visible.o"})
}

func TestParseKbuildHonorsMakeVariableFlavors(t *testing.T) {
	kb := parseCapturedKbuild(t, `stem = before
recursive = $(stem)-recursive.o
simple := $(stem)-simple.o
stem = after
late_recursive := late-recursive.o
late_simple := late-simple.o
recursive += $(late_recursive)
simple += $(late_simple)
created += $(created_late)
created_late := created.o
maybe ?= maybe.o
maybe ?= ignored.o
obj-y += $(recursive) $(simple) $(created) $(maybe)
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"after-recursive.o", "late-recursive.o", "before-simple.o", "late-simple.o", "created.o", "maybe.o",
	})
}

func TestParseKbuildExpandsDefineMacros(t *testing.T) {
	kb := parseCapturedKbuild(t, `define choose_objects
$(if $(1),defined.o,empty.o)
$(2)
endef
stem = early
define simple_object :=
$(stem)-simple.o
endef
stem = late
obj-y += $(call choose_objects,y,extra.o) $(call choose_objects,,fallback.o) $(simple_object)
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"defined.o", "extra.o", "empty.o", "fallback.o", "early-simple.o",
	})
}

func TestParseKbuildDefersRecursiveDefineCallsUntilUse(t *testing.T) {
	kb := parseCapturedKbuild(t, `define get-executable-or-default
$(if $($(1)),$(call _ge_attempt,$($(1)),$(1)),$(call _ge_attempt,$(2)))
endef
_ge_attempt = $(or $(1),fallback)
SELECTED_TOOL := configured
selected := $(call get-executable-or-default,SELECTED_TOOL,default)
obj-y += $(selected).o
`, KbuildOptions{
		MakeVariablesComplete: true,
	}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"configured.o"})
}

func TestParseKbuildUndefinedCallsFollowEnvironmentCompleteness(t *testing.T) {
	incomplete := parseCapturedKbuild(t, `flags := $(call source-helper,-fexample)
`, KbuildOptions{}, "flags")
	if got, want := incomplete.Variables["flags"], "$(call source-helper,-fexample)"; got != want {
		t.Fatalf("incomplete missing call = %q, want preserved %q", got, want)
	}

	_, err := parseKbuildWithOptions(strings.NewReader(`flags := $(call source-helper,-fexample)
`), "Makefile", KbuildOptions{
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"flags"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), `Kbuild call target "source-helper" is not defined`) {
		t.Fatalf("complete missing call error = %v", err)
	}
}

func TestParseKbuildEvaluatesEvalGeneratedAssignments(t *testing.T) {
	kb := parseCapturedKbuild(t, `dynamic_targets_y += foo.o baz.o
dynamic_targets_m += bar.o qux.o
define WRAP_OBJ
wrapper-$(1)-y := $(1).o
obj-$(2) += wrapper-$(1).o
endef
$(foreach target,$(basename $(dynamic_targets_y)),$(eval $(call WRAP_OBJ,$(target),y)))
$(eval $(foreach target,$(basename $(dynamic_targets_m)),$(call WRAP_OBJ,$(target),m)))
`, KbuildOptions{}, "obj-y", "obj-m", "wrapper-foo-y", "wrapper-baz-y", "wrapper-bar-y", "wrapper-qux-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{"wrapper-foo.o", "wrapper-baz.o"})
	requireKbuildVariableWords(t, kb, "obj-m", []string{"wrapper-bar.o", "wrapper-qux.o"})
	for name, want := range map[string]string{
		"wrapper-foo-y": "foo.o", "wrapper-baz-y": "baz.o",
		"wrapper-bar-y": "bar.o", "wrapper-qux-y": "qux.o",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
}

func TestParseKbuildHandlesAssignmentModifiers(t *testing.T) {
	kb := parseCapturedKbuild(t, `export objects := exported.o
override objects += override.o
private objects += private.o
obj-y += $(objects)
override define wrapped :=
wrapped.o
endef
obj-y += $(wrapped)
export obj-y += direct.o
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"exported.o", "override.o", "private.o", "wrapped.o", "direct.o",
	})
}

func TestKbuildFileExportedEnvironmentIsSourceOwnedAndCloned(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`base := source-selected
export DRIVER_FIELDS := $(base) -ffuture
PRIVATE_FIELD := not-exported
`), "Makefile")
	if err != nil {
		t.Fatal(err)
	}
	environment := kb.ExportedEnvironment()
	if got, want := environment["DRIVER_FIELDS"], "source-selected -ffuture"; got != want {
		t.Fatalf("exported DRIVER_FIELDS = %q, want %q", got, want)
	}
	if _, ok := environment["PRIVATE_FIELD"]; ok {
		t.Fatalf("unexported PRIVATE_FIELD leaked into environment: %#v", environment)
	}
	environment["DRIVER_FIELDS"] = "mutated"
	if got := kb.ExportedEnvironment()["DRIVER_FIELDS"]; got != "source-selected -ffuture" {
		t.Fatalf("caller mutated Kbuild exported environment through returned map: %q", got)
	}
}

func TestKbuildInheritedEnvironmentRemainsExportedUntilSourceUnexportsIt(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`
origin-before := $(origin INHERITED)
INHERITED += child
origin-after := $(origin INHERITED)
BARE := bare-value
    export BARE
export ASSIGNED = assigned-value
DROPPED := dropped-value
    export DROPPED
  unexport DROPPED
`), "Makefile", KbuildOptions{
		EnvironmentVariables: map[string]string{"INHERITED": "parent"},
		CaptureVariables:     []string{"origin-before", "origin-after"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["origin-before"], "environment"; got != want {
		t.Fatalf("origin before source assignment = %q, want %q", got, want)
	}
	if got, want := kb.Variables["origin-after"], "file"; got != want {
		t.Fatalf("origin after source assignment = %q, want %q", got, want)
	}
	environment := kb.ExportedEnvironment()
	for name, want := range map[string]string{
		"INHERITED": "parent child",
		"BARE":      "bare-value",
		"ASSIGNED":  "assigned-value",
	} {
		if got := environment[name]; got != want {
			t.Errorf("exported %s = %q, want %q", name, got, want)
		}
	}
	if _, ok := environment["DROPPED"]; ok {
		t.Fatalf("unexported variable leaked into environment: %#v", environment)
	}
}

func TestKbuildCommandLineVariablesAreAutomaticallyExported(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`
unexport DROPPED
`), "Makefile", KbuildOptions{
		CommandLineVariables: map[string]string{
			"KEPT":         "kept-value",
			"DROPPED":      "dropped-value",
			"MAKECMDGOALS": "all",
			"not.exported": "punctuation",
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	environment := kb.ExportedEnvironment()
	if got, want := environment["KEPT"], "kept-value"; got != want {
		t.Fatalf("automatically exported command-line variable = %q, want %q", got, want)
	}
	for _, name := range []string{"DROPPED", "MAKECMDGOALS", "not.exported"} {
		if _, ok := environment[name]; ok {
			t.Fatalf("%s unexpectedly entered command environment: %#v", name, environment)
		}
	}
}

func TestKbuildCommandLineVariablesHaveRecursiveFlavor(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`
srctree := /source
SDK := ignored-source-assignment
flags := -I$(SDK)/linux/include
sdk_flavor := $(flavor SDK)
sdk_raw := $(value SDK)
`), "Makefile", KbuildOptions{
		CommandLineVariables: map[string]string{
			"SDK": "$(srctree)/vendor",
		},
		CaptureVariables: []string{"flags", "sdk_flavor", "sdk_raw"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"flags":      "-I/source/vendor/linux/include",
		"sdk_flavor": "recursive",
		"sdk_raw":    "$(srctree)/vendor",
	} {
		if got := kb.Variables[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got, want := kb.ExportedEnvironment()["SDK"], "/source/vendor"; got != want {
		t.Fatalf("exported SDK = %q, want %q", got, want)
	}
}

func TestKbuildExportAssignmentExportsPinnedCommandLineValue(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`
export PINNED := source-value
PLAIN := source-value
`), "Makefile", KbuildOptions{
		CommandLineVariables: map[string]string{
			"PINNED": "command-line-value",
			"PLAIN":  "command-line-value",
		},
		AutoExportCommandLineVariables: map[string]bool{},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	environment := kb.ExportedEnvironment()
	if got, want := environment["PINNED"], "command-line-value"; got != want {
		t.Fatalf("exported pinned command-line variable = %q, want %q", got, want)
	}
	if _, ok := environment["PLAIN"]; ok {
		t.Fatalf("ordinary pinned command-line variable unexpectedly exported: %#v", environment)
	}
}

func TestParseKbuildHandlesVariableDirectives(t *testing.T) {
	kb := parseCapturedKbuild(t, `export exported_empty
ifeq ("$(origin exported_empty)","file")
obj-y += export-origin.o
endif
ifeq ("$(flavor exported_empty)","simple")
obj-y += export-flavor.o
endif
export later
later = later.o
unexport later
ifeq ("$(origin later)","file")
obj-y += unexport-keeps-origin.o
endif
obj-y += $(later)
temp := temp.o
undefine temp
ifeq ("$(origin temp)","undefined")
obj-y += undefine-origin.o
endif
again := again.o
override undefine again
ifeq ("$(origin again)","undefined")
obj-y += override-undefine.o
endif
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"export-origin.o", "export-flavor.o", "unexport-keeps-origin.o", "later.o",
		"undefine-origin.o", "override-undefine.o",
	})
}

func TestParseKbuildInitialVariablesUseMutableOverlay(t *testing.T) {
	initial := map[string]string{
		"objects":                    "initial.o",
		"UBSAN_SANITIZE_inherited.o": "n",
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`ifeq ("$(flavor objects)","simple")
obj-y += initial-is-simple.o
endif
obj-y += $(objects)
objects += appended.o
obj-y += $(objects)
objects ?= ignored.o
undefine objects
ifeq ("$(origin objects)","undefined")
obj-y += initial-was-undefined.o
endif
objects ?= reset.o
obj-y += $(objects)
`), "Kbuild", KbuildOptions{Variables: initial, CaptureVariables: []string{"obj-y"}}, "")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"initial-is-simple.o", "reset.o", "reset.o", "initial-was-undefined.o", "reset.o",
	})
	wantInitial := map[string]string{
		"objects":                    "initial.o",
		"UBSAN_SANITIZE_inherited.o": "n",
	}
	if !reflect.DeepEqual(initial, wantInitial) {
		t.Fatalf("initial variables mutated\nwant: %#v\n got: %#v", wantInitial, initial)
	}

	second, err := parseKbuildWithOptions(strings.NewReader("obj-y += $(objects)\n"), "second/Kbuild", KbuildOptions{
		Variables: initial, CaptureVariables: []string{"obj-y"},
	}, "")
	if err != nil {
		t.Fatalf("parseKbuild(second) failed: %v", err)
	}
	if got, want := second.Variables["obj-y"], "initial.o"; got != want {
		t.Fatalf("second obj-y=%q, want %q", got, want)
	}
}

func TestParseKbuildExpandsMakeVariableIntrospection(t *testing.T) {
	kb := parseCapturedKbuild(t, `recursive = raw$(suffix)
simple := simple.o
suffix = .o
ifeq ("$(origin missing)","undefined")
obj-y += missing-origin.o
endif
ifeq ("$(origin recursive)","file")
obj-y += file-origin.o
endif
ifeq ("$(flavor recursive)","recursive")
obj-y += recursive-flavor.o
endif
ifeq ("$(flavor simple)","simple")
obj-y += simple-flavor.o
endif
obj-y += $(value recursive) $(recursive)
`, KbuildOptions{}, "obj-y")
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"missing-origin.o", "file-origin.o", "recursive-flavor.o", "simple-flavor.o", "raw$(suffix)", "raw.o",
	})
}

func TestParseKbuildValuePreservesEscapedLiteralTreeMarkerPhase(t *testing.T) {
	kb := parseCapturedKbuild(t, `recursive = '$${tree:prep}'
raw := $(value recursive)
expanded := $(recursive)
single := '${tree:prep}'
`, KbuildOptions{MakeVariablesComplete: true}, "raw", "expanded", "single")
	if got, want := kb.Variables["raw"], `'$${tree:prep}'`; got != want {
		t.Fatalf("raw value=%q, want %q", got, want)
	}
	if got, want := kb.Variables["expanded"], `'${tree:prep}'`; got != want {
		t.Fatalf("expanded value=%q, want %q", got, want)
	}
	if got, want := kb.Variables["single"], `''`; got != want {
		t.Fatalf("single-dollar value=%q, want GNU Make variable expansion %q", got, want)
	}
}

func TestParseKbuildPreservesMakeRulesAndRecipes(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`obj := build
src := source
$(obj)/generated.h: $(src)/input.awk FORCE | $(obj) ; $(call filechk,generated)
	$(call if_changed,generated)
$(obj)/%.o: private objtool-enabled = y
$(obj)/generated.rs: private command-extra = ; sed -Ei 's/old/#[new]/g' $$@
$(eval $(obj)/module.o: $(obj)/part1.o $(obj)/part2.o)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotRules := kbuildRuleSummaries(kb.Rules)
	wantRules := []kbuildRuleSummary{
		{
			targets:       "build/generated.h",
			separator:     ":",
			prerequisites: "source/input.awk FORCE",
			orderOnly:     "build",
			recipe:        "$(call filechk,generated)\n$(call if_changed,generated)",
			line:          3,
		},
		{
			targets:       "build/module.o",
			separator:     ":",
			prerequisites: "build/part1.o build/part2.o",
			line:          7,
		},
	}
	if !reflect.DeepEqual(gotRules, wantRules) {
		t.Fatalf("rules mismatch\nwant: %#v\n got: %#v", wantRules, gotRules)
	}

	gotVars := kbuildTargetVariableSummaries(kb.TargetVariables)
	wantVars := []kbuildTargetVariableSummary{
		{
			targets:   "build/%.o",
			variable:  "objtool-enabled",
			operator:  "=",
			value:     "y",
			modifiers: "private",
			line:      5,
		},
		{
			targets:   "build/generated.rs",
			variable:  "command-extra",
			operator:  "=",
			value:     "; sed -Ei 's/old/#[new]/g' $$@",
			modifiers: "private",
			line:      6,
		},
	}
	if !reflect.DeepEqual(gotVars, wantVars) {
		t.Fatalf("target variables mismatch\nwant: %#v\n got: %#v", wantVars, gotVars)
	}
}

func TestParseKbuildPreservesRuleAcrossConditionalRecipes(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`image: vmlinux
ifeq ($(CONFIG_SELFTEST),y)
	$(MAKE) selftest
IGNORED := value
endif

	$(MAKE) image
other:
	$(MAKE) other
`), "Kbuild", KbuildOptions{
		Variables:               map[string]string{"CONFIG_SELFTEST": ""},
		ConfigVariablesComplete: true,
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}

	gotRules := kbuildRuleSummaries(kb.Rules)
	wantRules := []kbuildRuleSummary{
		{
			targets:       "image",
			separator:     ":",
			prerequisites: "vmlinux",
			recipe:        "$(MAKE) image",
			line:          1,
		},
		{
			targets:   "other",
			separator: ":",
			recipe:    "$(MAKE) other",
			line:      8,
		},
	}
	if !reflect.DeepEqual(gotRules, wantRules) {
		t.Fatalf("rules mismatch\nwant: %#v\n got: %#v", wantRules, gotRules)
	}
}

func TestParseKbuildRejectsUnterminatedDefine(t *testing.T) {
	_, err := ParseKbuild(strings.NewReader(`define missing_end
obj-y += hidden.o
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with unterminated define")
	}
}

func TestParseKbuildEvaluatesStaticConditionals(t *testing.T) {
	_, err := ParseKbuild(strings.NewReader(`enabled := y
disabled :=
ifeq ($(enabled),y)
obj-y += enabled.o
endif
ifneq ($(disabled),)
obj-y += disabled.o
else
obj-y += else.o
endif
ifdef enabled
include child
endif
ifndef disabled
always-y += generated.h
endif
else ifdef enabled
obj-y += invalid.o
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with unmatched else")
	}

	kb, err := parseKbuildWithOptions(strings.NewReader(`enabled := y
disabled :=
ifeq ($(enabled),y)
obj-y += enabled.o
endif
ifneq ($(disabled),)
obj-y += disabled.o
else
obj-y += else.o
endif
ifdef enabled
include child
endif
ifndef disabled
always-y += generated.h
endif
`), "Kbuild", KbuildOptions{CaptureVariables: []string{"obj-y"}}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{"enabled.o", "else.o"})

	gotIncludes := kbuildIncludeSummaries(kb.Includes)
	wantIncludes := []kbuildIncludeSummary{
		{path: "child", line: 12},
	}
	if !reflect.DeepEqual(gotIncludes, wantIncludes) {
		t.Fatalf("includes mismatch\nwant: %#v\n got: %#v", wantIncludes, gotIncludes)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "generated.h", condKind: "const", state: "y", line: 15},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildPreservesStaticPatternStem(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`
checks := .checked-first.h .checked-second.h
$(checks): .checked-%: include/linux/atomic/% FORCE
	touch $@
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	if got, want := len(kb.Rules), 1; got != want {
		t.Fatalf("rule count = %d, want %d: %#v", got, want, kb.Rules)
	}
	rule := kb.Rules[0]
	if got, want := rule.TargetPattern, ".checked-%"; got != want {
		t.Fatalf("static target pattern = %q, want %q", got, want)
	}
	if got, want := strings.Join(rule.Prerequisites, " "), "include/linux/atomic/% FORCE"; got != want {
		t.Fatalf("static prerequisites = %q, want %q", got, want)
	}
	profile := CompactKbuildProfile{Name: "static", Path: "Kbuild", Rules: kb.Rules}
	normal, _, stem, err := evaluatedKbuildTargetRuleContext(profile, ".checked-first.h")
	if err != nil {
		t.Fatal(err)
	}
	if stem != "first.h" || strings.Join(normal, " ") != "include/linux/atomic/first.h FORCE" {
		t.Fatalf("static target context stem=%q prerequisites=%q", stem, normal)
	}
}

func TestEvaluatedKbuildTargetRuleContextMergesExplicitPrerequisites(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`
modpost: FORCE
	$(call if_changed,host-cmulti)
modpost: modpost.o file2alias.o
`), "Kbuild")
	if err != nil {
		t.Fatal(err)
	}
	profile := CompactKbuildProfile{Name: "host", Path: "Kbuild", Rules: kb.Rules}
	normal, orderOnly, _, err := evaluatedKbuildTargetRuleContext(profile, "modpost")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normal, []string{"FORCE", "modpost.o", "file2alias.o"}; !slices.Equal(got, want) {
		t.Fatalf("merged explicit prerequisites = %q, want %q", got, want)
	}
	if len(orderOnly) != 0 {
		t.Fatalf("merged explicit order-only prerequisites = %q, want none", orderOnly)
	}
}

func TestParseKbuildEvaluatesConfiguredEmptyConfigInMakeFunctions(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`obj-y += dwc3.o
dwc3-y := core.o
ifneq ($(filter y,$(CONFIG_USB_DWC3_GADGET) $(CONFIG_USB_DWC3_DUAL_ROLE)),)
	dwc3-y += gadget.o ep0.o
endif
`), "Kbuild", KbuildOptions{
		Variables: map[string]string{
			"CONFIG_USB_DWC3_GADGET":    "",
			"CONFIG_USB_DWC3_DUAL_ROLE": "",
		},
		CaptureVariables: []string{"obj-y", "dwc3-y"},
	}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{"dwc3.o"})
	requireKbuildVariableWords(t, kb, "dwc3-y", []string{"core.o"})
}

func TestParseKbuildCompleteConfigDoesNotLeakConditionalAppend(t *testing.T) {
	for _, condition := range []struct {
		name string
		open string
	}{
		{name: "ifdef", open: "ifdef CONFIG_64BIT"},
		{name: "ifeq", open: "ifeq ($(CONFIG_64BIT),y)"},
		{name: "filtered ifneq", open: "ifneq ($(filter y,$(CONFIG_64BIT)),)"},
	} {
		t.Run(condition.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Makefile")
			content := "mmu-$(CONFIG_MMU) := memory.o\n" +
				condition.open + "\n" +
				"mmu-$(CONFIG_MMU) += mseal.o\n" +
				"endif\n" +
				"obj-y := $(mmu-y)\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, config := range []struct {
				name      string
				variables map[string]string
				wantMseal bool
			}{
				{name: "missing is unset", variables: map[string]string{"CONFIG_MMU": "y"}},
				{name: "defined empty is unset", variables: map[string]string{
					"CONFIG_64BIT": "",
					"CONFIG_MMU":   "y",
				}},
				{name: "enabled", variables: map[string]string{
					"CONFIG_64BIT": "y",
					"CONFIG_MMU":   "y",
				}, wantMseal: true},
			} {
				t.Run(config.name, func(t *testing.T) {
					kb, err := ParseKbuildFileWithOptions(path, KbuildOptions{
						Variables:               config.variables,
						ConfigVariablesComplete: true,
						CaptureVariables:        []string{"obj-y"},
					})
					if err != nil {
						t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
					}
					want := []string{"memory.o"}
					if config.wantMseal {
						want = append(want, "mseal.o")
					}
					requireKbuildVariableWords(t, kb, "obj-y", want)
				})
			}
		})
	}
}

func TestParseKbuildEvaluatesElseIfChains(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`selector := second
ifeq ($(selector),first)
obj-y += first.o
else ifeq ($(selector),second)
obj-y += second.o
else ifeq ($(selector),third)
obj-y += third.o
else
obj-y += fallback.o
endif
ifeq ($(CONFIG_UNKNOWN),y)
obj-y += unknown-y.o
else ifeq ($(CONFIG_UNKNOWN),m)
obj-y += unknown-m.o
else
obj-y += unknown-fallback.o
endif
`), "Kbuild", KbuildOptions{
		Variables:               map[string]string{"CONFIG_UNKNOWN": ""},
		ConfigVariablesComplete: true,
		CaptureVariables:        []string{"obj-y"},
	}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{"second.o", "unknown-fallback.o"})

	_, err = ParseKbuild(strings.NewReader(`ifeq (a,b)
else
else ifeq (a,a)
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with else conditional after else")
	}
}

func TestParseKbuildExpandsWildcardFunction(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"first.o", "second.o", "generated/one.h"} {
		fullPath := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", path, err)
		}
		if err := os.WriteFile(fullPath, nil, 0o644); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", path, err)
		}
	}
	kbuildPath := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuildPath, []byte(`objects := $(wildcard *.o)
obj-y += $(objects)
targets += $(wildcard generated/*.h missing/*)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}

	kb, err := ParseKbuildFileWithOptions(kbuildPath, KbuildOptions{CaptureVariables: []string{"obj-y"}})
	if err != nil {
		t.Fatalf("ParseKbuildFile() failed: %v", err)
	}

	requireKbuildVariableWords(t, kb, "obj-y", []string{"first.o", "second.o"})

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "targets", target: "generated/one.h", condKind: "const", state: "y", line: 3},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildWildcardIgnoresAmbientWorkingDirectory(t *testing.T) {
	ambient := t.TempDir()
	declared := t.TempDir()
	for path := range map[string]bool{
		filepath.Join(ambient, "match-ambient.o"):   true,
		filepath.Join(declared, "match-declared.o"): true,
	} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", path, err)
		}
	}
	t.Chdir(ambient)

	for _, test := range []struct {
		name    string
		opts    KbuildOptions
		baseDir string
	}{
		{name: "working directory", opts: KbuildOptions{WorkingDir: declared}},
		{name: "Makefile base directory", baseDir: declared},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.opts.CaptureVariables = []string{"objects"}
			kb, err := parseKbuildWithOptions(
				strings.NewReader("objects := $(wildcard match-*.o)\n"),
				"Kbuild",
				test.opts,
				test.baseDir,
			)
			if err != nil {
				t.Fatalf("parseKbuildWithOptions() failed: %v", err)
			}
			if got, want := kb.Variables["objects"], "match-declared.o"; got != want {
				t.Fatalf("objects = %q, want %q", got, want)
			}
		})
	}
}

func TestParseKbuildWildcardSeesVirtualPredecessorFiles(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`selected := $(if $(wildcard vmlinux.o),present,missing)
object := $(wildcard $(objtree)/vmlinux.o)
`), "Kbuild", KbuildOptions{
		Variables: map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		VirtualFileView: &testKbuildVirtualFileView{matches: map[string][]string{
			"vmlinux.o": {"vmlinux.o"},
			"__LINUX_BZL_OBJECT_TREE__/vmlinux.o": {
				"__LINUX_BZL_OBJECT_TREE__/vmlinux.o",
			},
		}},
		CaptureVariables: []string{"selected", "object"},
	}, "")
	if err != nil {
		t.Fatalf("parseKbuildWithOptions() failed: %v", err)
	}
	if got, want := kb.Variables["selected"], "present"; got != want {
		t.Fatalf("selected=%q, want %q", got, want)
	}
	if got, want := kb.Variables["object"], "__LINUX_BZL_OBJECT_TREE__/vmlinux.o"; got != want {
		t.Fatalf("object=%q, want %q", got, want)
	}
}

func TestParseKbuildLocalKbuildFlags(t *testing.T) {
	kb, err := parseKbuildWithOptions(strings.NewReader(`KBUILD_CFLAGS += -DLOCAL -fno-stack-protector $(DISABLE_STACKLEAK_PLUGIN)
KBUILD_CFLAGS := $(filter-out $(CC_FLAGS_LTO),$(KBUILD_CFLAGS))
obj-y += main.o
`), "Kbuild", KbuildOptions{
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"KBUILD_CFLAGS"},
	}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	if got, want := kb.Variables["KBUILD_CFLAGS"], "-DLOCAL -fno-stack-protector"; got != want {
		t.Fatalf("KBUILD_CFLAGS=%q, want %q", got, want)
	}
}

func TestParseKbuildLocalKbuildFlagsWithSelfReferenceAdditions(t *testing.T) {
	tmp := t.TempDir()
	kbuild := filepath.Join(tmp, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CFLAGS := $(subst $(CC_FLAGS_FTRACE),,$(KBUILD_CFLAGS)) -fpie \
	-I$(srctree)/scripts/dtc/libfdt -include $(srctree)/include/linux/hidden.h
KBUILD_CFLAGS := $(filter-out $(CC_FLAGS_SCS), $(KBUILD_CFLAGS))
obj-y += init.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		RootDir: tmp,
		Variables: map[string]string{
			"CC_FLAGS_FTRACE": "",
			"CC_FLAGS_SCS":    "",
			"KBUILD_CFLAGS":   "",
			"srctree":         tmp,
		},
		CaptureVariables: []string{"KBUILD_CFLAGS"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	want := "-fpie -I" + filepath.ToSlash(tmp) + "/scripts/dtc/libfdt -include " + filepath.ToSlash(tmp) + "/include/linux/hidden.h"
	if got := kb.Variables["KBUILD_CFLAGS"]; got != want {
		t.Fatalf("KBUILD_CFLAGS=%q, want %q", got, want)
	}
}

func TestParseKbuildNormalizesWindowsPathVariablesBeforeSplittingFlags(t *testing.T) {
	const sourceRoot = `D:\_bazel\external\+linux_source_repository+linux_6_18_39`
	for _, tc := range []struct {
		name string
		text string
		vars map[string]string
	}{
		{
			name: "injected",
			text: "KBUILD_CFLAGS += -include $(srctree)/include/linux/hidden.h\nobj-y += init.o\n",
			vars: map[string]string{"srctree": sourceRoot},
		},
		{
			name: "assigned",
			text: "srctree := " + sourceRoot + "\nKBUILD_CFLAGS += -include $(srctree)/include/linux/hidden.h\nobj-y += init.o\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kb, err := parseKbuildWithOptions(strings.NewReader(tc.text), "Kbuild", KbuildOptions{
				Variables:        tc.vars,
				CaptureVariables: []string{"KBUILD_CFLAGS"},
			}, ".")
			if err != nil {
				t.Fatalf("parseKbuild() failed: %v", err)
			}

			want := "-include D:/_bazel/external/+linux_source_repository+linux_6_18_39/include/linux/hidden.h"
			if got := kb.Variables["KBUILD_CFLAGS"]; got != want {
				t.Fatalf("KBUILD_CFLAGS=%q, want %q", got, want)
			}
		})
	}
}

func TestParseKbuildSubstPreservesResolvedWordsWithUnresolvedVariables(t *testing.T) {
	tmp := t.TempDir()
	kbuild := filepath.Join(tmp, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`cflags-y := $(KBUILD_CFLAGS)
cflags-y += -I$(srctree)/scripts/dtc/libfdt
KBUILD_CFLAGS := $(subst $(CC_FLAGS_FTRACE),,$(cflags-y)) -Os
obj-y += init.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		RootDir: tmp,
		Variables: map[string]string{
			"CC_FLAGS_FTRACE": "",
			"KBUILD_CFLAGS":   "",
			"srctree":         tmp,
		},
		CaptureVariables: []string{"KBUILD_CFLAGS"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	want := "-I" + tmp + "/scripts/dtc/libfdt -Os"
	if got := kb.Variables["KBUILD_CFLAGS"]; got != want {
		t.Fatalf("KBUILD_CFLAGS=%q, want %q", got, want)
	}
}

func TestParseKbuildGeneratedTargetsAndIncludes(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`include $(srctree)/scripts/Makefile.lib
-include include/config/auto.conf
always-y += bounds.h
always-$(CONFIG_FOO) += generated/foo.h
extra-y += vmlinux.lds
targets += asm-offsets.s $(dynamic-target)
hostprogs-y += fixdep
userprogs-$(CONFIG_USER) += user-helper
hostprogs := gen_init_cpio
userprogs += user-bare
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotIncludes := kbuildIncludeSummaries(kb.Includes)
	wantIncludes := []kbuildIncludeSummary{
		{path: "$(srctree)/scripts/Makefile.lib", line: 1},
		{path: "include/config/auto.conf", optional: true, line: 2},
	}
	if !reflect.DeepEqual(gotIncludes, wantIncludes) {
		t.Fatalf("includes mismatch\nwant: %#v\n got: %#v", wantIncludes, gotIncludes)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "bounds.h", condKind: "const", state: "y", line: 3},
		{kind: "always", target: "generated/foo.h", condKind: "config", symbol: "CONFIG_FOO", line: 4},
		{kind: "extra", target: "vmlinux.lds", condKind: "const", state: "y", line: 5},
		{kind: "targets", target: "asm-offsets.s", condKind: "const", state: "y", line: 6},
		{kind: "hostprogs", target: "fixdep", condKind: "const", state: "y", line: 7},
		{kind: "userprogs", target: "user-helper", condKind: "config", symbol: "CONFIG_USER", line: 8},
		{kind: "hostprogs", target: "gen_init_cpio", condKind: "const", state: "y", line: 9},
		{kind: "userprogs", target: "user-bare", condKind: "const", state: "y", line: 10},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildFileTreeFollowsExpandedStaticIncludes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root"), []byte(`children := $(wildcard child*)
include $(children)
include $(srctree)/child
-include missing
include vars
obj-y += root.o $(from_vars) $(from_makefile_list)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(root) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte(`obj-y += child.o
include grandchild
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child-extra"), []byte(`obj-y += child-extra.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child-extra) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grandchild"), []byte(`always-y += generated.h
`), 0o644); err != nil {
		t.Fatalf("WriteFile(grandchild) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vars"), []byte(`from_vars := from-vars.o
from_makefile_list := $(notdir $(lastword $(MAKEFILE_LIST))).o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(vars) failed: %v", err)
	}

	kb, err := ParseKbuildFileTree(filepath.Join(dir, "root"), KbuildOptions{
		RootDir:          dir,
		CaptureVariables: []string{"obj-y"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	requireKbuildVariableWords(t, kb, "obj-y", []string{
		"child.o", "child-extra.o", "root.o", "from-vars.o", "vars.o",
	})
	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "generated.h", condKind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildDefersShellUsedOnlyByDisabledGeneratedCollection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	content := "headers := $(shell unsupported-header-discovery)\n" +
		"always-$(CONFIG_HEADER_TEST) += $(headers)\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables: map[string]string{"CONFIG_HEADER_TEST": ""}, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}); err != nil {
		t.Fatalf("disabled generated collection evaluated discovery shell: %v", err)
	}
	_, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables: map[string]string{"CONFIG_HEADER_TEST": "y"}, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported-header-discovery") {
		t.Fatalf("enabled generated collection error = %v, want hermetic shell failure", err)
	}
}

func TestParseKbuildDoesNotEvaluateTargetsBookkeepingForSelectedGraph(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	content := "targets := $(shell find $(obj) -name \\*.gen.S 2>/dev/null)\n" +
		"obj-y += selected.o\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables:             map[string]string{"obj": "drivers/example"},
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"obj-y"},
		Shell: func(command string) (string, error) {
			called = true
			return "", fmt.Errorf("unexpected shell command %q", command)
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	if called {
		t.Fatal("unconsumed targets bookkeeping evaluated its discovery shell")
	}
	if len(kb.Generated) != 0 {
		t.Fatalf("targets bookkeeping became buildable generated targets: %#v", kb.Generated)
	}
	if got, want := kb.Variables["obj-y"], "selected.o"; got != want {
		t.Fatalf("obj-y=%q, want %q", got, want)
	}
}

func TestParseKbuildSkipsDisabledConditionalVariable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	content := "headers := $(shell unsupported-header-discovery)\n" +
		"always-$(CONFIG_HEADER_TEST) += $(headers)\n" +
		"obj-y += selected.o\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables:               map[string]string{"CONFIG_HEADER_TEST": ""},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	})
	if err != nil {
		t.Fatalf("disabled conditional snapshot evaluated discovery shell: %v", err)
	}
	if len(kb.Generated) != 0 {
		t.Fatalf("disabled always- collection was retained: %#v", kb.Generated)
	}
}

type kbuildGeneratedSummary struct {
	kind     string
	target   string
	condKind string
	symbol   string
	state    string
	line     int
}

func kbuildGeneratedSummaries(targets []KbuildTarget) []kbuildGeneratedSummary {
	out := make([]kbuildGeneratedSummary, 0, len(targets))
	for _, target := range targets {
		out = append(out, kbuildGeneratedSummary{
			kind:     target.Kind,
			target:   target.Target,
			condKind: target.Condition.Kind,
			symbol:   target.Condition.Symbol,
			state:    target.Condition.State,
			line:     target.Position.Line,
		})
	}
	return out
}

type kbuildIncludeSummary struct {
	path     string
	optional bool
	line     int
}

func kbuildIncludeSummaries(includes []KbuildInclude) []kbuildIncludeSummary {
	out := make([]kbuildIncludeSummary, 0, len(includes))
	for _, include := range includes {
		out = append(out, kbuildIncludeSummary{
			path:     include.Path,
			optional: include.Optional,
			line:     include.Position.Line,
		})
	}
	return out
}

type kbuildRuleSummary struct {
	targets       string
	separator     string
	prerequisites string
	orderOnly     string
	recipe        string
	line          int
}

func kbuildRuleSummaries(rules []KbuildRule) []kbuildRuleSummary {
	out := make([]kbuildRuleSummary, 0, len(rules))
	for _, rule := range rules {
		out = append(out, kbuildRuleSummary{
			targets:       strings.Join(rule.Targets, " "),
			separator:     rule.Separator,
			prerequisites: strings.Join(rule.Prerequisites, " "),
			orderOnly:     strings.Join(rule.OrderOnly, " "),
			recipe:        strings.Join(rule.Recipe, "\n"),
			line:          rule.Position.Line,
		})
	}
	return out
}

type kbuildTargetVariableSummary struct {
	targets   string
	variable  string
	operator  string
	value     string
	modifiers string
	line      int
}

func kbuildTargetVariableSummaries(variables []KbuildTargetVariable) []kbuildTargetVariableSummary {
	out := make([]kbuildTargetVariableSummary, 0, len(variables))
	for _, variable := range variables {
		out = append(out, kbuildTargetVariableSummary{
			targets:   strings.Join(variable.Targets, " "),
			variable:  variable.Variable,
			operator:  variable.Operator,
			value:     variable.Value,
			modifiers: strings.Join(variable.Modifiers, " "),
			line:      variable.Position.Line,
		})
	}
	return out
}
