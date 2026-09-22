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
	readErr    error
}

func (v *testKbuildVirtualFileView) Match(pattern string) []string {
	v.matchCalls = append(v.matchCalls, pattern)
	return append([]string(nil), v.matches[pattern]...)
}

func (v *testKbuildVirtualFileView) Read(path string) (string, bool, bool, error) {
	v.readCalls = append(v.readCalls, path)
	if v.readErr != nil {
		return "", false, false, v.readErr
	}
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

func TestKbuildSourceCacheReevaluatesConfigAndEnvironment(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"Makefile": `include shared.mk
captured := $(CONFIG_MODE)|$(PROFILE)|$(from-shared)
`,
		"shared.mk": `from-shared = $(CONFIG_MODE)-$(PROFILE)
`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewKbuildSourceCache([]string{root}, nil)
	for _, test := range []struct {
		config  string
		profile string
		want    string
	}{
		{config: "y", profile: "first", want: "y|first|y-first"},
		{config: "n", profile: "second", want: "n|second|n-second"},
	} {
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
			RootDir:                 root,
			Variables:               map[string]string{"CONFIG_MODE": test.config},
			EnvironmentVariables:    map[string]string{"PROFILE": test.profile},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"captured"},
			SourceCache:             cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Variables["captured"]; got != test.want {
			t.Fatalf("config %q environment %q captured %q, want %q", test.config, test.profile, got, test.want)
		}
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{SourceReads: 2, CacheHits: 2, Entries: 2}); got != want {
		t.Fatalf("source cache stats = %#v, want %#v", got, want)
	}
}

func TestKbuildSourceCacheSharesLexingAcrossVariantScale(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"Makefile":  "include common.mk\ncaptured := $(COMMON)-$(VARIANT)\n",
		"common.mk": "COMMON := source\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewKbuildSourceCache([]string{root}, nil)
	const variants = 128
	for variant := 0; variant < variants; variant++ {
		value := fmt.Sprintf("variant-%03d", variant)
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
			RootDir:          root,
			Variables:        map[string]string{"VARIANT": value},
			CaptureVariables: []string{"captured"},
			SourceCache:      cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := parsed.Variables["captured"], "source-"+value; got != want {
			t.Fatalf("variant %d captured %q, want %q", variant, got, want)
		}
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{
		SourceReads: 2,
		CacheHits:   (variants - 1) * 2,
		Entries:     2,
	}); got != want {
		t.Fatalf("source cache stats = %#v, want %#v", got, want)
	}
}

func TestKbuildSourceCacheExcludesMutableObjectTree(t *testing.T) {
	root := t.TempDir()
	objectRoot := filepath.Join(root, "object")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`include $(objtree)/variant.mk
captured := $(OBJECT_VALUE)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	variantMakefile := filepath.Join(objectRoot, "variant.mk")
	cache := NewKbuildSourceCache([]string{root}, []string{objectRoot})
	for _, value := range []string{"first", "second"} {
		if err := os.WriteFile(variantMakefile, []byte("OBJECT_VALUE := "+value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
			RootDir:          root,
			Variables:        map[string]string{"objtree": "__OBJECT_TREE__"},
			SourceRoots:      map[string]string{"__OBJECT_TREE__": objectRoot},
			CaptureVariables: []string{"captured"},
			SourceCache:      cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Variables["captured"]; got != value {
			t.Fatalf("captured object value = %q, want %q", got, value)
		}
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{SourceReads: 1, CacheHits: 1, Entries: 1}); got != want {
		t.Fatalf("source cache stats = %#v, want source-only reuse %#v", got, want)
	}
}

func TestKbuildVirtualObjectIncludeReadsExactFrontierWithoutCachingIt(t *testing.T) {
	root := t.TempDir()
	objectRoot := filepath.Join(root, "object")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte(`include $(objtree)/feature-dump.mk
all: $(if $(filter 1,$(feature-bpf)),enabled.o,disabled.o)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// An existing physical file must not replace the version selected by the
	// declared Make-visible frontier, even on the first cache miss.
	if err := os.WriteFile(filepath.Join(objectRoot, "feature-dump.mk"), []byte("feature-bpf=stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const marker = "__LINUX_BZL_OBJECT_TREE__"
	const included = marker + "/feature-dump.mk"
	view := &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}}
	cache := NewKbuildSourceCache([]string{root}, []string{objectRoot})
	for _, tc := range []struct {
		contents, prerequisite string
	}{
		{contents: "feature-bpf=1\n", prerequisite: "enabled.o"},
		{contents: "feature-bpf=0\n", prerequisite: "disabled.o"},
	} {
		view.files[included] = testKbuildVirtualFile{content: tc.contents, exact: true}
		kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
			RootDir: root, SourceRoots: map[string]string{marker: objectRoot},
			Variables: map[string]string{"objtree": marker}, VirtualFileView: view,
			SourceCache: cache, CaptureVariables: []string{"feature-bpf", "MAKEFILE_LIST"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := kb.Variables["feature-bpf"]; got != strings.TrimSuffix(strings.TrimPrefix(tc.contents, "feature-bpf="), "\n") {
			t.Fatalf("included feature status = %q, want %q", got, tc.contents)
		}
		if len(kb.Rules) != 1 || !slices.Equal(kb.Rules[0].Prerequisites, []string{tc.prerequisite}) {
			t.Fatalf("selected rule = %+v, want all: %s", kb.Rules, tc.prerequisite)
		}
		if want := filepath.Join(objectRoot, "feature-dump.mk"); !strings.Contains(kb.Variables["MAKEFILE_LIST"], want) {
			t.Fatalf("included Makefile identity = %q, want %q", kb.Variables["MAKEFILE_LIST"], want)
		}
	}
	if !slices.Equal(view.readCalls, []string{included, included}) {
		t.Fatalf("virtual include reads = %q, want exact rooted object path twice", view.readCalls)
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{SourceReads: 1, CacheHits: 1, Entries: 1}); got != want {
		t.Fatalf("source cache stats = %#v, want source-only reuse %#v", got, want)
	}
}

func TestKbuildVirtualObjectIncludeRequiresDeclaredExactOwner(t *testing.T) {
	root := t.TempDir()
	objectRoot := filepath.Join(root, "object")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "__LINUX_BZL_OBJECT_TREE__"
	const included = marker + "/feature-dump.mk"
	makefile := filepath.Join(root, "Makefile")
	for _, tc := range []struct {
		name, include, want string
		roots               map[string]string
		view                *testKbuildVirtualFileView
		missing             bool
	}{
		{name: "opaque", include: "include $(objtree)/feature-dump.mk", roots: map[string]string{marker: objectRoot},
			view: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{included: {content: "feature-bpf=1\n"}}}, want: "requires exact contents"},
		{name: "conflicting producer", include: "include $(objtree)/feature-dump.mk", roots: map[string]string{marker: objectRoot},
			view: &testKbuildVirtualFileView{readErr: fmt.Errorf("distinct selected writers")}, want: "distinct selected writers"},
		{name: "wrong owner", include: "include $(objtree)/feature-dump.mk", roots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
			view: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{included: {content: "feature-bpf=1\n", exact: true}}}, want: "no declared object-tree root"},
		{name: "escaping alias", include: "include $(objtree)/../feature-dump.mk", roots: map[string]string{marker: objectRoot},
			view: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{included: {content: "feature-bpf=1\n", exact: true}}}, want: "not a canonical relative path"},
		{name: "noncanonical alias", include: "include $(objtree)/nested/./feature-dump.mk", roots: map[string]string{marker: objectRoot},
			view: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{included: {content: "feature-bpf=1\n", exact: true}}}, want: "not a canonical relative path"},
		{name: "absent, physical stale", include: "include $(objtree)/feature-dump.mk", roots: map[string]string{marker: objectRoot},
			view: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}}, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(makefile, []byte(tc.include+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(objectRoot, "feature-dump.mk"), []byte("feature-bpf=stale\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ParseKbuildFileTree(makefile, KbuildOptions{
				RootDir: root, SourceRoots: tc.roots, Variables: map[string]string{"objtree": marker}, VirtualFileView: tc.view,
			})
			if err == nil || tc.want != "" && !strings.Contains(err.Error(), tc.want) || tc.missing && !os.IsNotExist(err) {
				t.Fatalf("object include error = %v, want %q (missing=%t)", err, tc.want, tc.missing)
			}
			if tc.name == "escaping alias" || tc.name == "noncanonical alias" || tc.name == "wrong owner" {
				if len(tc.view.readCalls) != 0 {
					t.Fatalf("unowned or noncanonical include read virtual path %q", tc.view.readCalls)
				}
			}
		})
	}
	if err := os.WriteFile(makefile, []byte("-include $(objtree)/feature-dump.mk\nselected := yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		RootDir: root, SourceRoots: map[string]string{marker: objectRoot}, Variables: map[string]string{"objtree": marker},
		VirtualFileView:  &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
		CaptureVariables: []string{"selected"},
	})
	if err != nil || parsed.Variables["selected"] != "yes" {
		t.Fatalf("optional absent object include = %#v, %v; want selected yes", parsed, err)
	}
}

func TestKbuildSourceCacheExcludesSourceSymlinkIntoMutableObjectTree(t *testing.T) {
	root := t.TempDir()
	objectRoot := filepath.Join(root, "object")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("include shared.mk\ncaptured := $(OBJECT_VALUE)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	variantMakefile := filepath.Join(objectRoot, "variant.mk")
	if err := os.WriteFile(variantMakefile, []byte("OBJECT_VALUE := first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(variantMakefile, filepath.Join(root, "shared.mk")); err != nil {
		t.Fatal(err)
	}
	cache := NewKbuildSourceCache([]string{root}, []string{objectRoot})
	for _, value := range []string{"first", "second"} {
		if err := os.WriteFile(variantMakefile, []byte("OBJECT_VALUE := "+value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
			RootDir:          root,
			CaptureVariables: []string{"captured"},
			SourceCache:      cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Variables["captured"]; got != value {
			t.Fatalf("captured symlinked object value = %q, want %q", got, value)
		}
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{SourceReads: 1, CacheHits: 1, Entries: 1}); got != want {
		t.Fatalf("source cache stats = %#v, want only the root Makefile cached %#v", got, want)
	}
}

func TestKbuildSourceCacheAdmitsBazelStyleImmutableFileSymlink(t *testing.T) {
	root := t.TempDir()
	contentStore := t.TempDir()
	physical := filepath.Join(contentStore, "shared.mk")
	if err := os.WriteFile(physical, []byte("SHARED := immutable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, filepath.Join(root, "shared.mk")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("include shared.mk\ncaptured := $(SHARED)-$(VARIANT)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := NewKbuildSourceCache([]string{root}, nil)
	for _, variant := range []string{"first", "second"} {
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
			RootDir:          root,
			Variables:        map[string]string{"VARIANT": variant},
			CaptureVariables: []string{"captured"},
			SourceCache:      cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := parsed.Variables["captured"], "immutable-"+variant; got != want {
			t.Fatalf("captured = %q, want %q", got, want)
		}
	}
	if got, want := cache.Stats(), (KbuildSourceCacheStats{SourceReads: 2, CacheHits: 2, Entries: 2}); got != want {
		t.Fatalf("source cache stats = %#v, want Bazel-style symlink reuse %#v", got, want)
	}
}

func TestKbuildSourceCachePreservesMissingIncludeDiagnostic(t *testing.T) {
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte("include missing.mk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ordinaryErr := ParseKbuildFileTree(makefile, KbuildOptions{RootDir: root})
	_, cachedErr := ParseKbuildFileTree(makefile, KbuildOptions{
		RootDir: root, SourceCache: NewKbuildSourceCache([]string{root}, nil),
	})
	if ordinaryErr == nil || cachedErr == nil || ordinaryErr.Error() != cachedErr.Error() {
		t.Fatalf("missing include errors differ without/with cache:\nordinary: %v\n  cached: %v", ordinaryErr, cachedErr)
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

func TestKbuildRootedObjectFileReadDoesNotUsePhysicalFallback(t *testing.T) {
	const objectPath = "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	const sourcePath = "__LINUX_BZL_SOURCE_TREE__/payload"
	root := t.TempDir()
	physicalObject := filepath.Join(root, "include", "config", "kernel.release")
	if err := os.MkdirAll(filepath.Dir(physicalObject), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physicalObject, []byte("stale-physical-release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("immutable-source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{physicalObject, filepath.Join(root, "payload")} {
		info, err := os.Lstat(filename)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("declared test file %q is not a regular physical file: %#v, %v", filename, info, err)
		}
	}
	view := &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}}
	options := KbuildOptions{
		SourceRoots: map[string]string{
			"__LINUX_BZL_OBJECT_TREE__": root,
			"__LINUX_BZL_SOURCE_TREE__": root,
		},
		Variables: map[string]string{
			"objtree": "__LINUX_BZL_OBJECT_TREE__",
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		CaptureVariables: []string{"object", "source"},
	}
	const makefile = "object := $(file < $(objtree)/include/config/kernel.release)\nsource := $(file < $(srctree)/payload)\n"
	check := func(wantObject string, wantReads []string) {
		t.Helper()
		kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", options, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := kb.Variables["object"]; got != wantObject {
			t.Fatalf("object-root file read = %q, want %q", got, wantObject)
		}
		if got := kb.Variables["source"]; got != "immutable-source" {
			t.Fatalf("source-root physical file read = %q, want declared immutable source", got)
		}
		if options.VirtualFileView != nil {
			if !slices.Equal(view.readCalls, wantReads) {
				t.Fatalf("virtual file reads = %q, want %q", view.readCalls, wantReads)
			}
			view.readCalls = nil
		}
	}
	options.VirtualFileView = view
	check("", []string{objectPath, sourcePath})
	view.files[objectPath] = testKbuildVirtualFile{content: "source-generated-release\n", exact: true}
	check("source-generated-release", []string{objectPath, sourcePath})
	view.files[objectPath] = testKbuildVirtualFile{content: "opaque", exact: false}
	if _, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), `visible virtual file "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release" requires exact contents`) {
		t.Fatalf("opaque object-root file read error = %v, want exact-content rejection", err)
	}
	view.readCalls = nil
	view.files["payload"] = testKbuildVirtualFile{content: "forged-virtual-alias\n", exact: true}
	for _, test := range []struct{ variable, tree string }{
		{variable: "objtree", tree: "object-tree"},
		{variable: "srctree", tree: "source-tree"},
	} {
		invocation := "invalid := $(file < $(" + test.variable + ")/../payload)\n"
		if _, err := parseKbuildWithOptions(strings.NewReader(invocation), "Makefile", options, ""); err == nil ||
			!strings.Contains(err.Error(), "escapes declared "+test.tree+" root") {
			t.Fatalf("%s file read crossing declared root error = %v, want rejection", test.tree, err)
		}
		if len(view.readCalls) != 0 {
			t.Fatalf("%s escaping file read queried forged virtual alias %q", test.tree, view.readCalls)
		}
	}
	options.VirtualFileView = nil
	check("", nil)
}

func TestKbuildParseTimeObjectTreeFeatureDumpUsesExactPriorVersion(t *testing.T) {
	const root = "__LINUX_BZL_OBJECT_TREE__"
	const filename = root + "/tools/lib/bpf/FEATURE-DUMP.libbpf"
	const makefile = `OUTPUT := $(objtree)/tools/lib/bpf/
OUTPUT_FEATURES := $(OUTPUT)feature/
FEATURE_USER := .libbpf
FEATURE_DUMP_FILENAME = $(OUTPUT)FEATURE-DUMP$(FEATURE_USER)
$(shell mkdir -p $(OUTPUT_FEATURES))
FEATURE_DUMP := $(shell touch $(FEATURE_DUMP_FILENAME); cat $(FEATURE_DUMP_FILENAME))
ifeq ($(findstring feature-bpf=1,$(FEATURE_DUMP)),)
selected := missing
all: missing.o
else
selected := present
all: present.o
endif
$(shell rm -f $(FEATURE_DUMP_FILENAME))
$(foreach feat,libelf zlib bpf,$(shell echo "feature-$(feat)=1" >> $(FEATURE_DUMP_FILENAME)))
stored := $(file < $(FEATURE_DUMP_FILENAME))
relative := $(file < FEATURE-DUMP.libbpf)
present := $(wildcard FEATURE-DUMP.libbpf)
MSG = $(shell printf '...%30s: [ \033[32mon\033[m  ]' bpf)
$(info $(MSG))
`
	for _, tc := range []struct {
		name, previous, selected string
	}{
		{name: "absent", selected: "missing"},
		{name: "existing", previous: "feature-bpf=1\n", selected: "present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}}
			if tc.previous != "" {
				view.files[filename] = testKbuildVirtualFile{content: tc.previous, exact: true}
			}
			kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile.feature", KbuildOptions{
				Variables:        map[string]string{"objtree": root},
				SourceRoots:      map[string]string{root: "/declared/object"},
				WorkingDir:       "/declared/object/tools/lib/bpf",
				VirtualFileView:  view,
				CaptureVariables: []string{"selected", "stored", "relative", "present", "FEATURE_DUMP", "MSG"},
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := kb.Variables["selected"]; got != tc.selected {
				t.Fatalf("feature dump selected %q, want %q", got, tc.selected)
			}
			if len(kb.Rules) != 1 || len(kb.Rules[0].Prerequisites) != 1 || kb.Rules[0].Prerequisites[0] != tc.selected+".o" {
				t.Fatalf("feature dump selected graph %+v, want all: %s.o", kb.Rules, tc.selected)
			}
			if got := kb.Variables["FEATURE_DUMP"]; got != strings.TrimSuffix(tc.previous, "\n") {
				t.Fatalf("prior feature dump %q, want %q", got, tc.previous)
			}
			if got, want := kb.Variables["stored"], "feature-libelf=1\nfeature-zlib=1\nfeature-bpf=1"; got != want {
				t.Fatalf("appended feature dump = %q, want %q", got, want)
			}
			if got := kb.Variables["relative"]; got != kb.Variables["stored"] {
				t.Fatalf("cwd-relative read %q differs from rooted dump %q", got, kb.Variables["stored"])
			}
			if got := kb.Variables["present"]; got != "FEATURE-DUMP.libbpf" {
				t.Fatalf("cwd-relative wildcard = %q, want FEATURE-DUMP.libbpf", got)
			}
			if got, want := kb.Variables["MSG"], "...                           bpf: [ \x1b[32mon\x1b[m  ]"; got != want {
				t.Fatalf("source diagnostic printf = %q, want %q", got, want)
			}
			if _, exists := view.files[filename]; exists && tc.previous == "" {
				t.Fatal("parse-time effect modified the immutable prior view")
			}
		})
	}
	for _, tc := range []struct{ name, makefile, prior, want string }{
		{"opaque previous", makefile, "opaque", "requires exact prior object contents"},
		{"escaped path", `$(shell echo "feature-bpf=1" >> $(objtree)/../escaped)`, "", "canonical relative path"},
		{"active shell expansion", `$(shell echo "$UNDECLARED" >> $(objtree)/FEATURE-DUMP)`, "", "dynamic shell word"},
		{"foreign source write", `$(shell echo "feature-bpf=1" >> __LINUX_BZL_SOURCE_TREE__/FEATURE-DUMP)`, "", "requires a hermetic evaluator"},
		{"custom shell", "SHELL := /bin/false\n$(shell touch $(objtree)/cache; cat $(objtree)/cache)", "", "selected SHELL override"},
		{"custom tool path", "PATH := /custom/applets\n$(shell touch $(objtree)/cache; cat $(objtree)/cache)", "", "selected PATH override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]testKbuildVirtualFile{}
			if tc.prior != "" {
				files[filename] = testKbuildVirtualFile{content: tc.prior, exact: false}
			}
			view := &testKbuildVirtualFileView{files: files}
			_, err := parseKbuildWithOptions(strings.NewReader(tc.makefile+"\n"), "Makefile.feature", KbuildOptions{
				Variables:        map[string]string{"objtree": root},
				SourceRoots:      map[string]string{root: "/declared/object"},
				WorkingDir:       "/declared/object/tools/lib/bpf",
				VirtualFileView:  view,
				CaptureVariables: []string{"selected", "stored", "relative", "present", "FEATURE_DUMP"},
			}, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error = %v, want %q (virtual reads %q)", err, tc.want, view.readCalls)
			}
		})
	}
}

func TestKbuildParseTimeObjectTreeMultiDirectorySetup(t *testing.T) {
	const root = "__LINUX_BZL_OBJECT_TREE__"
	kb, err := parseKbuildWithOptions(strings.NewReader(`obj-dirs := . ./arch/x86/kernel ./include/generated ./kernel ./arch/x86/boot/compressed ./arch/x86/boot/compressed/..
$(shell mkdir -p $(obj-dirs))
found := $(wildcard $(objtree)/arch/x86/kernel/ $(objtree)/include/generated/ $(objtree)/kernel/ $(objtree)/arch/x86/boot/compressed/ $(objtree)/arch/x86/boot/)
`), "scripts/Makefile.build", KbuildOptions{
		Variables:        map[string]string{"objtree": root},
		WorkingDir:       "/declared/object",
		SourceRoots:      map[string]string{root: "/declared/object"},
		VirtualFileView:  &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
		CaptureVariables: []string{"found"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["found"], root+"/arch/x86/kernel/ "+root+"/include/generated/ "+root+"/kernel/ "+root+"/arch/x86/boot/compressed/ "+root+"/arch/x86/boot/"; got != want {
		t.Fatalf("Make-visible generated directories = %q, want %q", got, want)
	}
	for _, tc := range []struct{ name, operand, want string }{
		{"root escape", "../../../outside", "escapes the declared object tree"},
		{"temporary root escape", "a/../../a", "escapes the declared object tree"},
		{"opaque prior traversal", "arch/x86/boot/compressed arch/x86/boot/compressed/..", "untyped prior artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]testKbuildVirtualFile{}
			if tc.name == "opaque prior traversal" {
				files[root+"/arch/x86/boot/compressed"] = testKbuildVirtualFile{content: "opaque"}
			}
			_, err := parseKbuildWithOptions(strings.NewReader("$(shell mkdir -p "+tc.operand+")\n"), "scripts/Makefile.build", KbuildOptions{
				WorkingDir: "/declared/object", SourceRoots: map[string]string{root: "/declared/object"},
				VirtualFileView: &testKbuildVirtualFileView{files: files},
			}, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse-time mkdir error = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("symlink traversal", func(t *testing.T) {
		objectRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(objectRoot, "arch/x86/boot"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(objectRoot, "arch/x86/boot/compressed")); err != nil {
			t.Fatal(err)
		}
		_, err := parseKbuildWithOptions(strings.NewReader("$(shell mkdir -p arch/x86/boot/compressed arch/x86/boot/compressed/..)\n"), "scripts/Makefile.build", KbuildOptions{
			WorkingDir: objectRoot, SourceRoots: map[string]string{root: objectRoot},
			VirtualFileView: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
		}, "")
		if err == nil || !strings.Contains(err.Error(), "non-directory or symlink") {
			t.Fatalf("symlinked mkdir traversal error = %v, want rejection", err)
		}
	})
	if _, err := parseKbuildWithOptions(strings.NewReader("$(shell mkdir -p .)\n"), "Makefile", KbuildOptions{
		WorkingDir:      "/undeclared/directory",
		SourceRoots:     map[string]string{root: "/declared/object"},
		VirtualFileView: &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
	}, ""); err == nil || !strings.Contains(err.Error(), "requires a hermetic evaluator") {
		t.Fatalf("unowned cwd-relative mkdir error = %v, want source shell authority rejection", err)
	}
}

func TestKbuildParseTimeObjectDirectoryWaitsForMeasuredPathname(t *testing.T) {
	const root = "__LINUX_BZL_OBJECT_TREE__"
	probe := linuxProbeSymbolPrefix + strings.Repeat("d", 64)
	source := "$(shell mkdir -p $(OBJDIR))\nseen := $(wildcard $(objtree)/obj/)\n"
	options := KbuildOptions{
		Variables:  map[string]string{"objtree": root, "OBJDIR": probe},
		WorkingDir: "/declared/object", SourceRoots: map[string]string{root: "/declared/object"},
		VirtualFileView:  &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
		CaptureVariables: []string{"seen"},
		ResolveSymbolic:  func(value string) (string, error) { return value, nil },
	}
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), "wildcard") || !strings.Contains(err.Error(), "unmeasured parse-time pathname") {
		t.Fatalf("unmeasured pathname read error = %v, want conditional object wildcard rejection", err)
	}
	options.RejectUnmeasuredGraphGuards = true
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), "undeclared probe-dependent graph guard") {
		t.Fatalf("undeclared mkdir pathname error = %v, want exact graph guard rejection", err)
	}
	options.RejectUnmeasuredGraphGuards = false
	options.ResolveSymbolic = func(value string) (string, error) {
		return strings.ReplaceAll(value, probe, "obj"), nil
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["seen"], root+"/obj/"; got != want {
		t.Fatalf("measured mkdir wildcard = %q, want %q", got, want)
	}
	options.ResolveSymbolic = func(value string) (string, error) { return value, nil }
	if _, err := parseKbuildWithOptions(strings.NewReader("$(shell mkdir -p $(OBJDIR))\nread := $(file < $(objtree)/obj/file)\n"), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), "unmeasured parse-time pathname") {
		t.Fatalf("unmeasured pathname read error = %v, want exact-read rejection", err)
	}
}

func TestKbuildParseTimeObjectCatReadsExactInvocationFrontier(t *testing.T) {
	const marker = "__LINUX_BZL_OBJECT_TREE__"
	objectRoot := t.TempDir()
	objectDirectory := filepath.Join(objectRoot, "external/demo")
	if err := os.MkdirAll(objectDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objectDirectory, "modules.order"), []byte("stale.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	location := CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree, Directory: "external/demo"}
	view := &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{
		marker + "/external/demo/modules.order": {content: "demo.o\nsecond.o\n", exact: true},
	}}
	options := KbuildOptions{
		WorkingDir: objectDirectory, SourceRoots: map[string]string{marker: objectRoot},
		InvocationLocation: &location, VirtualFileView: view,
		CaptureVariables: []string{"modules"},
	}
	source := "MODORDER := modules.order\nmodules := $(sort $(shell cat $(MODORDER)))\n"
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.modfinal", options, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kb.Variables["modules"], "demo.o second.o"; got != want {
		t.Fatalf("source-selected modules = %q, want %q", got, want)
	}
	if len(view.readCalls) != 1 || view.readCalls[0] != marker+"/external/demo/modules.order" {
		t.Fatalf("modfinal cat read %q, want exact object-tree invocation path", view.readCalls)
	}
	for _, tc := range []struct {
		name, input, want string
		files             map[string]testKbuildVirtualFile
	}{
		{"stale physical file", source, "no completed producer", nil},
		{"opaque frontier", source, "producer with opaque contents", map[string]testKbuildVirtualFile{marker + "/external/demo/modules.order": {content: "demo.o\n"}}},
		{"escaping source path", "modules := $(shell cat ../modules.order)\n", "not a canonical relative path", view.files},
		{"dynamic shell operand", "modules := $(shell cat modules.*)\n", "escaping or dynamic path", view.files},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options.VirtualFileView = &testKbuildVirtualFileView{files: tc.files}
			if _, err := parseKbuildWithOptions(strings.NewReader(tc.input), "scripts/Makefile.modfinal", options, ""); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse error = %v, want %q", err, tc.want)
			}
		})
	}
	options.WorkingDir = objectRoot
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.modfinal", options, ""); err == nil ||
		!strings.Contains(err.Error(), "does not match its declared working directory") {
		t.Fatalf("wrong typed cwd error = %v, want rejection", err)
	}
}

func TestKbuildParseTimeConditionalObjectWritesDoNotChooseUnmeasuredGraph(t *testing.T) {
	const root = "__LINUX_BZL_OBJECT_TREE__"
	const filename = root + "/tools/lib/bpf/FEATURE-DUMP"
	probe := linuxProbeSymbolPrefix + strings.Repeat("d", 64)
	const source = `FEATURE_DUMP_FILENAME := $(objtree)/tools/lib/bpf/FEATURE-DUMP
ifneq ($(FLAG),)
$(shell rm -f $(FEATURE_DUMP_FILENAME))
$(shell echo "feature-bpf=1" >> $(FEATURE_DUMP_FILENAME))
endif
ifneq ($(wildcard $(FEATURE_DUMP_FILENAME)),)
all: present.o
else
all: absent.o
endif
`
	_, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile.feature", KbuildOptions{
		Variables:             map[string]string{"objtree": root, "FLAG": probe},
		SourceRoots:           map[string]string{root: "/declared/object"},
		VirtualFileView:       &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{filename: {content: "stale\n", exact: true}}},
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
		SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
			if linuxProbeSymbolPattern.MatchString(value) {
				return probe, true, nil
			}
			return "", false, nil
		},
		ResolveSymbolic: func(value string) (string, error) { return value, nil },
	}, "")
	if err == nil || !strings.Contains(err.Error(), "wildcard") || !strings.Contains(err.Error(), "before its graph guard is measured") {
		t.Fatalf("unmeasured conditional object mutation error = %v, want conditional wildcard rejection", err)
	}
}

func TestKbuildRootedSourceFileReadDistinguishesMissingFromUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "existing-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := KbuildOptions{
		SourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		Variables:   map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__"},
		VirtualFileView: &testKbuildVirtualFileView{
			files: map[string]testKbuildVirtualFile{},
		},
		CaptureVariables: []string{"missing"},
	}
	kb, err := parseKbuildWithOptions(strings.NewReader("missing := $(file < $(srctree)/missing)\n"), "Makefile", options, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := kb.Variables["missing"]; got != "" {
		t.Fatalf("missing immutable source read = %q, want empty", got)
	}
	_, err = parseKbuildWithOptions(strings.NewReader("directory := $(file < $(srctree)/existing-directory)\n"), "Makefile", options, "")
	if err == nil || !strings.Contains(err.Error(), `Kbuild file read "__LINUX_BZL_SOURCE_TREE__/existing-directory"`) {
		t.Fatalf("existing immutable source directory read error = %v, want source-located read failure", err)
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
ifeq ($(filter undefine,$(.FEATURES)),)
$(error GNU Make >= 3.82 is required. Your Make version is $(MAKE_VERSION))
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
	if got := strings.Fields(kb.Variables[".FEATURES"]); !slices.Contains(got, "output-sync") || !slices.Contains(got, "undefine") {
		t.Fatalf(".FEATURES=%q omits the selected Linux Make feature gates", got)
	}
	if _, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir: dir, MakeVariablesComplete: true,
		Variables: map[string]string{".FEATURES": "output-sync"},
	}); err == nil || !strings.Contains(err.Error(), "GNU Make >= 3.82") {
		t.Fatalf("caller-selected frontend without undefine passed the 6.6 source gate: %v", err)
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

func TestKbuildProbeConditionalExportPreservesPresence(t *testing.T) {
	const source = `
EMPTY :=
ifneq ($(shell compiler-version),)
export EMPTY
unexport INHERITED
export PINNED := source-value
endif
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	type selection struct {
		value, expected, trueText, falseText string
		equal                                bool
	}
	selections := map[string]selection{}
	opts := KbuildOptions{
		EnvironmentVariables:           map[string]string{"INHERITED": "parent"},
		CommandLineVariables:           map[string]string{"PINNED": "command-line"},
		AutoExportCommandLineVariables: map[string]bool{},
		MakeVariablesComplete:          true,
		ConfigVariablesComplete:        true,
		CaptureTargetEvaluator:         true,
		Shell: func(command string) (string, error) {
			if command != "compiler-version" {
				return "", fmt.Errorf("unexpected command %q", command)
			}
			return probe, nil
		},
		SelectSymbolic: func(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
			token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", len(selections)+1)
			selections[token] = selection{value, expected, trueText, falseText, equal}
			return token, true, nil
		},
	}
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", opts, ""); err == nil ||
		!strings.Contains(err.Error(), "unresolved probe-dependent presence") {
		t.Fatalf("premature export materialization error = %v, want unresolved membership", err)
	}
	opts.SkipExportedVariables = true
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", opts, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, version string
		present       map[string]string
		absent        []string
	}{
		{name: "selected", version: "clang", present: map[string]string{"EMPTY": "", "PINNED": "command-line"}, absent: []string{"INHERITED"}},
		{name: "unselected", version: "", present: map[string]string{"INHERITED": "parent"}, absent: []string{"EMPTY", "PINNED"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parser := cloneKbuildParserForEvaluation(parsed.evaluator.template)
			var resolve func(string) string
			resolve = func(value string) string {
				if value == probe {
					return test.version
				}
				if selected, ok := selections[value]; ok {
					match := resolve(selected.value) == resolve(selected.expected)
					if !selected.equal {
						match = !match
					}
					if match {
						return resolve(selected.trueText)
					}
					return resolve(selected.falseText)
				}
				return value
			}
			parser.resolveSymbolic = func(value string) (string, error) { return resolve(value), nil }
			if err := parser.finalizeExportedVariables(); err != nil {
				t.Fatal(err)
			}
			environment := parser.kb.ExportedEnvironment()
			for name, want := range test.present {
				if got, exists := environment[name]; !exists || got != want {
					t.Errorf("exported %s = %q, present %t; want %q, present", name, got, exists, want)
				}
			}
			for _, name := range test.absent {
				if got, exists := environment[name]; exists {
					t.Errorf("unexported %s = %q; want absent", name, got)
				}
			}
		})
	}
}

func TestKbuildSourceGraphGuardsRetainExportIncludeAndRecipeSelectors(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte(`
has_capability := $(shell capability-probe)
ifeq ($(has_capability),1)
export CONDITIONAL
-include optional.mk
endif
all:
ifeq ($(has_capability),1)
	@echo enabled
endif
	@echo ready
`), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	sequence := 0
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		CaptureTargetEvaluator: true, MakeVariablesComplete: true, SkipExportedVariables: true,
		Shell: func(command string) (string, error) {
			if command != "capability-probe" {
				return "", fmt.Errorf("unexpected graph probe %q", command)
			}
			return probe, nil
		},
		SelectSymbolic: func(_, _ string, _ bool, _, _ string) (string, bool, error) {
			sequence++
			return linuxProbeSymbolPrefix + fmt.Sprintf("%064x", sequence), true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), parsed)
	if err != nil {
		t.Fatal(err)
	}
	guards := CompactKbuildGraphGuards(profile)
	if len(guards) < 3 {
		t.Fatalf("source graph guards = %q, want guarded export, include and recipe", guards)
	}
	for _, guard := range guards {
		if !linuxProbeSymbolPattern.MatchString(guard) {
			t.Errorf("unmeasured source graph guard %q", guard)
		}
	}
}

func TestKbuildSourceGraphGuardDefersRuleAndItsRecipeOwner(t *testing.T) {
	const version = "rustc 1.100.0-nightly (923c95cdf 2026-09-16)"
	const source = `
previous: FORCE
	@echo previous
ifneq "$(shell version-probe)" "rustc 1.100.0-nightly (923c95cdf 2026-09-16)"
next: $(shell prerequisite-probe)
	$(shell recipe-only)
endif
	@echo following
last: FORCE
	@echo last
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	guard := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	parse := func(measured string, resolveGuard, final bool) (*KbuildFile, []string, error) {
		queries := []string{}
		opts := KbuildOptions{
			MakeVariablesComplete:       true,
			CaptureTargetEvaluator:      true,
			RejectUnmeasuredGraphGuards: final,
			ResolveMeasuredGraphGuards:  measured != "" || resolveGuard,
			Shell: func(command string) (string, error) {
				queries = append(queries, command)
				switch command {
				case "version-probe":
					return probe, nil
				case "prerequisite-probe":
					return "generated", nil
				default:
					return "", fmt.Errorf("recipe body executed while parsing: %q", command)
				}
			},
			SelectSymbolic: func(value, expected string, equal bool, whenTrue, whenFalse string) (string, bool, error) {
				if !linuxProbeSymbolPattern.MatchString(value) && !linuxProbeSymbolPattern.MatchString(expected) {
					return "", false, nil
				}
				if value != probe || expected != version || equal || whenTrue != "1" || whenFalse != "" {
					return "", true, fmt.Errorf("unexpected version comparison %q, %q, equal=%t", value, expected, equal)
				}
				return guard, true, nil
			},
			ResolveSymbolic: func(value string) (string, error) {
				if value == probe && measured != "" {
					return measured, nil
				}
				if value == guard && resolveGuard {
					return "", nil
				}
				return value, nil
			},
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", opts, "")
		return parsed, queries, err
	}
	deferred, queries, err := parse("", false, false)
	if err != nil || !slices.Equal(queries, []string{"version-probe"}) || len(deferred.Rules) != 2 ||
		!slices.Equal(deferred.Rules[0].Recipe, []string{"@echo previous"}) ||
		!slices.Equal(deferred.Rules[1].Recipe, []string{"@echo last"}) ||
		deferred.evaluator == nil || len(deferred.evaluator.template.deferredGraphGuards) != 1 {
		t.Fatalf("unmeasured rule = %#v, queries %q, err %v", deferred, queries, err)
	}
	if _, _, err := parse("", false, true); err == nil || !strings.Contains(err.Error(), "undeclared probe-dependent graph guard") {
		t.Fatalf("final rule with unmeasured guard error = %v", err)
	}
	for _, test := range []struct {
		name, measured string
		resolveGuard   bool
		wantRules      int
		wantPrior      []string
		wantNext       []string
		wantQueries    []string
	}{
		{"equal", version, false, 2, []string{"@echo previous", "@echo following"}, nil, []string{"version-probe"}},
		{"guard token false", "", true, 2, []string{"@echo previous", "@echo following"}, nil, []string{"version-probe"}},
		{"different", "rustc 1.100.0-nightly (different)", false, 3, []string{"@echo previous"}, []string{"$(shell recipe-only)", "@echo following"}, []string{"version-probe", "prerequisite-probe"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, queries, err := parse(test.measured, test.resolveGuard, false)
			if err != nil || len(parsed.Rules) != test.wantRules ||
				!slices.Equal(parsed.Rules[0].Recipe, test.wantPrior) ||
				!slices.Equal(queries, test.wantQueries) {
				t.Fatalf("measured rules = %#v, queries %q, err %v", parsed, queries, err)
			}
			if test.wantNext != nil && (!slices.Equal(parsed.Rules[1].Targets, []string{"next"}) ||
				!slices.Equal(parsed.Rules[1].Prerequisites, []string{"generated"}) ||
				!slices.Equal(parsed.Rules[1].Recipe, test.wantNext)) {
				t.Fatalf("selected guarded rule = %#v, want recipe %q", parsed.Rules[1], test.wantNext)
			}
			if !slices.Equal(parsed.Rules[len(parsed.Rules)-1].Recipe, []string{"@echo last"}) {
				t.Fatalf("later unconditional rule = %#v", parsed.Rules[len(parsed.Rules)-1])
			}
		})
	}
}

func TestKbuildSourceGraphGuardDefersTargetVariableExpansion(t *testing.T) {
	const source = `
ifneq ($(shell version-probe),stable)
target.o: CFLAGS := $(shell immediate-probe)
target.o: CXXFLAGS = $(shell recursive-probe)
endif
after: FORCE
	@echo after
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	guard := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	for _, test := range []struct {
		name, answer string
		measured     bool
		reject       bool
		wantError    string
		wantQueries  []string
		wantVars     int
	}{
		{"unmeasured discovery", "", false, false, "", []string{"version-probe"}, 0},
		{"unmeasured final", "", false, true, "undeclared probe-dependent graph guard", []string{"version-probe"}, 0},
		{"measured false", "", true, true, "", []string{"version-probe"}, 0},
		{"measured true", "1", true, true, "", []string{"version-probe", "immediate-probe"}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			queries := []string{}
			kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
				MakeVariablesComplete:       true,
				CaptureTargetEvaluator:      true,
				RejectUnmeasuredGraphGuards: test.reject,
				ResolveMeasuredGraphGuards:  test.measured,
				Shell: func(command string) (string, error) {
					queries = append(queries, command)
					switch command {
					case "version-probe":
						return probe, nil
					case "immediate-probe":
						return "-fselected", nil
					default:
						return "", fmt.Errorf("recursive target-variable RHS executed while parsing: %q", command)
					}
				},
				SelectSymbolic: func(value, expected string, equal bool, whenTrue, whenFalse string) (string, bool, error) {
					if value != probe || expected != "stable" || equal || whenTrue != "1" || whenFalse != "" {
						return "", false, nil
					}
					return guard, true, nil
				},
				ResolveSymbolic: func(value string) (string, error) {
					if value == guard && test.measured {
						return test.answer, nil
					}
					return value, nil
				},
			}, "")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || !slices.Equal(queries, test.wantQueries) {
					t.Fatalf("guarded target assignment error = %v, queries %q", err, queries)
				}
				return
			}
			if err != nil || !slices.Equal(queries, test.wantQueries) || len(kb.TargetVariables) != test.wantVars ||
				len(kb.Rules) != 1 || !slices.Equal(kb.Rules[0].Recipe, []string{"@echo after"}) {
				t.Fatalf("guarded target assignments = %+v, queries %q, err %v", kb, queries, err)
			}
			if test.wantVars != 0 {
				if got := kb.TargetVariables[0]; !slices.Equal(got.Targets, []string{"target.o"}) ||
					got.Variable != "CFLAGS" || got.Value != "-fselected" {
					t.Fatalf("selected immediate target variable = %+v", got)
				}
				if got := kb.TargetVariables[1]; got.Variable != "CXXFLAGS" ||
					got.Value != "$(shell recursive-probe)" {
					t.Fatalf("selected recursive target variable = %+v", got)
				}
			}
		})
	}
}

func TestKbuildSourceGraphGuardsRetainInheritedExportedScalar(t *testing.T) {
	const source = `
ifeq ($(shell host-link-probe),0)
SKIP_STACK_VALIDATION := 1
export SKIP_STACK_VALIDATION
endif
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	sequence := 0
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
		CaptureTargetEvaluator: true, SkipExportedVariables: true,
		Shell: func(command string) (string, error) {
			if command != "host-link-probe" {
				return "", fmt.Errorf("unexpected source probe %q", command)
			}
			return probe, nil
		},
		SelectSymbolic: func(_, _ string, _ bool, _, _ string) (string, bool, error) {
			sequence++
			return linuxProbeSymbolPrefix + fmt.Sprintf("%064x", sequence), true, nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", "Makefile", "", parsed)
	if err != nil {
		t.Fatal(err)
	}
	template := parsed.evaluator.template
	scalar, present := template.lookupVariable("SKIP_STACK_VALIDATION")
	if !present || !linuxProbeSymbolPattern.MatchString(scalar.value) {
		t.Fatalf("exported source scalar = %q, defined %t; want conditional probe", scalar.value, present)
	}
	guards := CompactKbuildGraphGuards(profile)
	if !slices.Contains(guards, scalar.value) || !slices.Contains(guards, template.exportedWhen["SKIP_STACK_VALIDATION"]) {
		t.Fatalf("source graph guards %q omit inherited value %q or membership %q", guards, scalar.value, template.exportedWhen["SKIP_STACK_VALIDATION"])
	}
}

func TestKbuildSourceGraphGuardsRetainUnconditionalExportWithConditionalValue(t *testing.T) {
	const source = `
CC_FLAGS_FTRACE :=
PRIVATE_FLAGS :=
ifeq ($(shell capability-probe),y)
CC_FLAGS_FTRACE += -mrecord-mcount
PRIVATE_FLAGS += -private
endif
export CC_FLAGS_FTRACE
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
		CaptureTargetEvaluator: true, SkipExportedVariables: true,
		Shell: func(command string) (string, error) {
			if command != "capability-probe" {
				return "", fmt.Errorf("unexpected source probe %q", command)
			}
			return probe, nil
		},
		SelectSymbolic: func(_, _ string, _ bool, _, _ string) (string, bool, error) {
			return probe, true, nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", "Makefile", "", parsed)
	if err != nil {
		t.Fatal(err)
	}
	template := parsed.evaluator.template
	value, defined := template.lookupVariable("CC_FLAGS_FTRACE")
	if !defined || !template.exported["CC_FLAGS_FTRACE"] ||
		!linuxProbeSymbolPattern.MatchString(value.value) {
		t.Fatalf("source export has no probe-selected flags: value=%q defined=%t export=%t", value.value, defined, template.exported["CC_FLAGS_FTRACE"])
	}
	if got := CompactKbuildGraphGuards(profile); !slices.Equal(got, []string{value.value}) {
		t.Fatalf("exported conditional value guards = %q, want only %q; unexported flags are local", got, value.value)
	}
}

func TestKbuildPregraphMeasuredInheritedScalarSelectsChildConditional(t *testing.T) {
	const source = `
ifneq ($(SKIP_STACK_VALIDATION),1)
objtool_args = $(if $(CONFIG_UNWINDER_ORC),orc generate,check)
endif
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, measured, want string
	}{
		{name: "skip", measured: "1", want: ""},
		{name: "validate", measured: "", want: "orc generate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selections := 0
			parsed := parseCapturedKbuild(t, source, KbuildOptions{
				EnvironmentVariables:  map[string]string{"SKIP_STACK_VALIDATION": probe},
				Variables:             map[string]string{"CONFIG_UNWINDER_ORC": "y"},
				MakeVariablesComplete: true, ConfigVariablesComplete: true,
				RejectUnmeasuredGraphGuards: true,
				ResolveSymbolic: func(value string) (string, error) {
					if value == probe {
						return tc.measured, nil
					}
					return value, nil
				},
				SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
					if linuxProbeSymbolPattern.MatchString(value) {
						selections++
						return probe, true, nil
					}
					return "", false, nil
				},
			}, "objtool_args")
			if got := parsed.Variables["objtool_args"]; got != tc.want {
				t.Fatalf("selected child objtool_args = %q, want %q", got, tc.want)
			}
			if selections != 0 {
				t.Fatalf("measured child scalar recreated %d source selection tokens", selections)
			}
		})
	}
}

func TestKbuildMeasuredCompilerGuardSelectsExportedRecipeFlags(t *testing.T) {
	// The same source-selected compiler flag is later inherited by a compiler
	// query in an exported Make variable. A family replay must consume the
	// measured guard before expanding that environment, just as ordinary
	// Kbuild discovery does.
	const source = `
KBUILD_CFLAGS := -Werror
cc-option-yn = $(shell source-option $(1))
ifdef CONFIG_FUNCTION_TRACER
ifdef CONFIG_FTRACE_MCOUNT_RECORD
ifeq ($(call cc-option-yn,-mrecord-mcount),y)
CC_FLAGS_FTRACE += -mrecord-mcount
endif
endif
endif
KBUILD_CFLAGS += $(CC_FLAGS_FTRACE)
export KBUILD_USERLDFLAGS = $(KBUILD_CFLAGS)
outputmakefile: scripts/mkmakefile
	$(CONFIG_SHELL) scripts/mkmakefile
`
	const probe = linuxProbeSymbolPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name, measured string
		want           []string
	}{
		{name: "compiler rejects flag", measured: "n", want: []string{"-Werror"}},
		{name: "compiler accepts flag", measured: "y", want: []string{"-Werror", "-mrecord-mcount"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed := parseCapturedKbuild(t, source, KbuildOptions{
				Variables: map[string]string{
					"CONFIG_FUNCTION_TRACER":      "y",
					"CONFIG_FTRACE_MCOUNT_RECORD": "y",
					"CONFIG_SHELL":                "sh",
				},
				ConfigVariablesComplete:     true,
				MakeVariablesComplete:       true,
				RejectUnmeasuredGraphGuards: true,
				Shell: func(command string) (string, error) {
					if command != "source-option -mrecord-mcount" {
						return "", fmt.Errorf("unexpected source compiler query %q", command)
					}
					return probe, nil
				},
				ResolveSymbolic: func(value string) (string, error) {
					if value == probe {
						return test.measured, nil
					}
					return value, nil
				},
				SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
					if linuxProbeSymbolPattern.MatchString(value) {
						return "", true, fmt.Errorf("measured source compiler guard was not resolved: %q", value)
					}
					return "", false, nil
				},
			}, "KBUILD_CFLAGS", "KBUILD_USERLDFLAGS")
			for _, name := range []string{"KBUILD_CFLAGS", "KBUILD_USERLDFLAGS"} {
				requireKbuildVariableWords(t, parsed, name, test.want)
			}
		})
	}
}

func TestKbuildPregraphMeasuredMakeIfSelectsOnlySealedCondition(t *testing.T) {
	probe := linuxProbeSymbolPrefix + strings.Repeat("c", 64)
	found := linuxProbeSymbolPrefix + strings.Repeat("d", 64)
	const source = `
CC_FLAGS_FTRACE := -pg
_c_flags := $(CAPABILITY)
cmd_record_mcount = $(if $(findstring $(CC_FLAGS_FTRACE),$(_c_flags)),recordmcount)
result := $(cmd_record_mcount)
`
	for _, tc := range []struct {
		name, measured, want string
	}{
		{name: "missing trace flag", measured: "-DOTHER=1", want: ""},
		{name: "embedded trace flag", measured: "-DTHING=-pgsuffix", want: "recordmcount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selections := 0
			parsed := parseCapturedKbuild(t, source, KbuildOptions{
				EnvironmentVariables:  map[string]string{"CAPABILITY": probe},
				MakeVariablesComplete: true, ConfigVariablesComplete: true,
				RejectUnmeasuredGraphGuards: true,
				ResolveSymbolic: func(value string) (string, error) {
					if value == found {
						if strings.Contains(tc.measured, "-pg") {
							return "-pg", nil
						}
						return "", nil
					}
					return value, nil
				},
				TransformSymbolic: func(function string, args []string) (string, bool, error) {
					if function != "findstring" || !slices.Equal(args, []string{"-pg", probe}) {
						return "", true, fmt.Errorf("unexpected source Make transform %q(%q)", function, args)
					}
					return found, true, nil
				},
				SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
					selections++
					return "", false, fmt.Errorf("unsealed Make if condition %q", value)
				},
			}, "result")
			if got := parsed.Variables["result"]; got != tc.want {
				t.Fatalf("record-mcount command = %q, want %q", got, tc.want)
			}
			if selections != 0 {
				t.Fatalf("sealed Make if condition selected %d symbolic branches", selections)
			}
		})
	}

	// A child-local result absent from the source guard remains symbolic. In
	// particular, a stateful branch cannot be expanded just to choose a
	// plausible value for an unsealed compiler result.
	_, err := parseKbuildWithOptions(strings.NewReader(`result := $(if $(CAPABILITY),$(eval UNSAFE := yes)recordmcount,safe)`), "Makefile", KbuildOptions{
		EnvironmentVariables:  map[string]string{"CAPABILITY": probe},
		MakeVariablesComplete: true, ConfigVariablesComplete: true,
		RejectUnmeasuredGraphGuards: true,
		ResolveSymbolic:             func(value string) (string, error) { return value, nil },
		SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
			if value != probe {
				return "", false, fmt.Errorf("unexpected probe %q", value)
			}
			return probe, true, nil
		},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "stateful branch") {
		t.Fatalf("unsealed Make if expanded a stateful branch: %v", err)
	}
}

func TestKbuildDynamicIncludeFilenameIsASeparateGraphGuard(t *testing.T) {
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte("candidate = $(shell capability-probe)\n-include $(candidate)\nall:\n\t@echo ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	for _, reject := range []bool{false, true} {
		parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
			CaptureTargetEvaluator: true, SkipExportedVariables: true,
			MakeVariablesComplete: true, RejectUnmeasuredGraphGuards: reject,
			Shell: func(command string) (string, error) {
				if command != "capability-probe" {
					return "", fmt.Errorf("unexpected include filename probe %q", command)
				}
				return probe, nil
			},
		})
		if reject {
			if err == nil || !strings.Contains(err.Error(), "Makefile:2") || !strings.Contains(err.Error(), "include filename") {
				t.Errorf("unmeasured dynamic include = %v; want source-positioned rejection", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		profile, err := NewCompactKbuildProfile("root", makefile, filepath.Dir(makefile), parsed)
		if err != nil {
			t.Fatal(err)
		}
		if got := CompactKbuildGraphGuards(profile); !slices.Equal(got, []string{probe}) {
			t.Errorf("dynamic include filename guards = %q, want measured text source", got)
		}
	}
}

func TestKbuildPostPregraphRejectsNewRecipeAndIncludeGuardsAtSource(t *testing.T) {
	probe := linuxProbeSymbolPrefix + strings.Repeat("b", 64)
	for _, test := range []struct {
		name, source, diagnostic string
	}{
		{"recipe", "has = $(shell capability-probe)\nall:\nifeq ($(has),1)\n\t@echo enabled\nendif\n", "selected recipe"},
		{"guarded include", "has = $(shell capability-probe)\nifeq ($(has),1)\n-include optional.mk\nendif\n", "selected include"},
	} {
		t.Run(test.name, func(t *testing.T) {
			makefile := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(makefile, []byte(test.source), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ParseKbuildFileTree(makefile, KbuildOptions{
				MakeVariablesComplete: true, RejectUnmeasuredGraphGuards: true,
				Shell: func(command string) (string, error) {
					if command != "capability-probe" {
						return "", fmt.Errorf("unexpected child guard probe %q", command)
					}
					return probe, nil
				},
				SelectSymbolic: func(value, _ string, _ bool, _, _ string) (string, bool, error) {
					if !linuxProbeSymbolPattern.MatchString(value) {
						return "", false, nil
					}
					return linuxProbeSymbolPrefix + strings.Repeat("c", 64), true, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "Makefile:") || !strings.Contains(err.Error(), test.diagnostic) {
				t.Fatalf("new child graph guard = %v; want source-positioned %q failure", err, test.diagnostic)
			}
		})
	}
	makefile := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(makefile, []byte("-include optional.mk\nall:\n\t@echo ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKbuildFileTree(makefile, KbuildOptions{
		RejectUnmeasuredGraphGuards: true,
	}); err != nil {
		t.Fatalf("guard-free child fan-out rejected: %v", err)
	}
}

func TestKbuildProbeConditionalRecipeSelectsSourceBranchOnReplay(t *testing.T) {
	const source = `
SKIP_STACK_VALIDATION := $(shell has_libelf)
prepare-objtool: objtool
ifeq ($(SKIP_STACK_VALIDATION),1)
ifdef CONFIG_UNWINDER_ORC
	@echo error: cannot generate ORC metadata without libelf
	@false
else
	@echo warning: cannot validate the stack without libelf
endif
endif
	@echo prepared
`
	probe := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	type selection struct {
		value, expected, trueText, falseText string
		equal                                bool
	}
	selections := map[string]selection{}
	options := KbuildOptions{
		Variables:               map[string]string{"CONFIG_UNWINDER_ORC": "y"},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		Shell: func(command string) (string, error) {
			if command != "has_libelf" {
				return "", fmt.Errorf("unexpected command %q", command)
			}
			return probe, nil
		},
		SelectSymbolic: func(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
			token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", len(selections)+1)
			selections[token] = selection{value, expected, trueText, falseText, equal}
			return token, true, nil
		},
	}
	parse := func() []string {
		t.Helper()
		kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(kb.Rules) != 1 {
			t.Fatalf("parsed %d rules, want the source declaration", len(kb.Rules))
		}
		return kb.Rules[0].Recipe
	}
	if got, want := parse(), []string{"@echo prepared"}; !slices.Equal(got, want) {
		t.Fatalf("discovery recipe = %#v, want %#v", got, want)
	}
	for _, tc := range []struct {
		name, result, unwinder string
		want                   []string
	}{
		{"missing-libelf-orc", "1", "y", []string{"@echo error: cannot generate ORC metadata without libelf", "@false", "@echo prepared"}},
		{"present-libelf-orc", "", "y", []string{"@echo prepared"}},
		{"missing-libelf-no-orc", "1", "", []string{"@echo warning: cannot validate the stack without libelf", "@echo prepared"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options.Variables["CONFIG_UNWINDER_ORC"] = tc.unwinder
			var resolve func(string) string
			resolve = func(value string) string {
				if value == probe {
					return tc.result
				}
				if selected, ok := selections[value]; ok {
					match := resolve(selected.value) == resolve(selected.expected)
					if !selected.equal {
						match = !match
					}
					if match {
						return resolve(selected.trueText)
					}
					return resolve(selected.falseText)
				}
				return value
			}
			options.ResolveSymbolic = func(value string) (string, error) {
				return resolve(value), nil
			}
			if got := parse(); !slices.Equal(got, tc.want) {
				t.Fatalf("replayed recipe = %#v, want %#v", got, tc.want)
			}
		})
	}
	options.ResolveSymbolic = func(value string) (string, error) {
		if _, ok := selections[value]; ok {
			return "unexpected", nil
		}
		return value, nil
	}
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), "non-boolean text") {
		t.Fatalf("non-boolean recipe guard error = %v, want rejection", err)
	}
}

func TestKbuildOptionalObjectTreeShellReadUsesExactVisibleContents(t *testing.T) {
	const query = "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	root := t.TempDir()
	physical := filepath.Join(root, "include", "config", "kernel.release")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("untracked-host-release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}}
	shellCalls := 0
	options := KbuildOptions{
		SourceRoots:     map[string]string{"__LINUX_BZL_OBJECT_TREE__": root},
		VirtualFileView: view,
		Shell: func(command string) (string, error) {
			shellCalls++
			return "", nil
		},
		SourceShell: func(command, directory string) (string, error) {
			return "", fmt.Errorf("unselected source-shell fallback for %q", command)
		},
	}
	const source = "KERNELRELEASE = $(shell cat include/config/kernel.release 2> /dev/null)\nexport INSTALL_DTBS_PATH := /dtbs/$(KERNELRELEASE)\n"
	check := func(want string) {
		t.Helper()
		kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := kb.ExportedEnvironment()["INSTALL_DTBS_PATH"]; got != want {
			t.Fatalf("INSTALL_DTBS_PATH = %q, want %q", got, want)
		}
		if !slices.Equal(view.readCalls, []string{query}) {
			t.Fatalf("optional cat observed %q, want one declared object read", view.readCalls)
		}
		if shellCalls != 0 {
			t.Fatalf("optional cat queried shell %d times, want parser-owned object read", shellCalls)
		}
		view.readCalls = nil
	}
	check("/dtbs/")
	view.files[query] = testKbuildVirtualFile{content: "5.10.270-test\n", exact: true}
	check("/dtbs/5.10.270-test")
	view.files[query] = testKbuildVirtualFile{content: "unknown", exact: false}
	if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, ""); err == nil ||
		!strings.Contains(err.Error(), "has no exact contents") {
		t.Fatalf("opaque object-tree read error = %v, want fail-closed", err)
	}
	if shellCalls != 0 {
		t.Fatalf("opaque object read queried shell %d times, want parser-owned rejection", shellCalls)
	}
	options.VirtualFileView = nil
	checkWithoutView, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
	if err != nil || checkWithoutView.ExportedEnvironment()["INSTALL_DTBS_PATH"] != "/dtbs/" {
		t.Fatalf("ambient object-tree file became input: %#v, %v", checkWithoutView, err)
	}
}

func TestKbuildParseTimeEchoRedirectIntoObjectTreeRejectsUnknownWrite(t *testing.T) {
	root := t.TempDir()
	_, err := parseKbuildWithOptions(strings.NewReader(`
GENERATED := $(shell echo literal > $(objtree)/generated.txt)
`), "Makefile", KbuildOptions{
		SourceRoots:      map[string]string{"__LINUX_BZL_OBJECT_TREE__": root},
		VirtualFileView:  &testKbuildVirtualFileView{files: map[string]testKbuildVirtualFile{}},
		CaptureVariables: []string{"GENERATED"},
		Variables:        map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		Shell: func(command string) (string, error) {
			t.Fatalf("object-tree writer unexpectedly delegated to probe shell: %q", command)
			return "", nil
		},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "unsupported append command") {
		t.Fatalf("unrecognized object-tree write error = %v", err)
	}
}

func TestKbuildOptionalObjectTreeShellReadRejectsDynamicPaths(t *testing.T) {
	options := KbuildOptions{
		SourceRoots: map[string]string{"__LINUX_BZL_OBJECT_TREE__": t.TempDir()},
		Shell: func(command string) (string, error) {
			return "", nil
		},
		MakeVariablesComplete: true,
	}
	for _, command := range []string{
		"cat include/config/$$PATH_FRAGMENT 2>/dev/null",
		"cat include/config/*.release 2> /dev/null",
		"cat include/config/../../Makefile 2>/dev/null",
		"cat include/config/release;other 2>/dev/null",
	} {
		source := "export RELEASE := $(shell " + command + ")\n"
		if _, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, ""); err == nil {
			t.Errorf("dynamic optional object read %q was accepted", command)
		}
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

func TestKbuildSourceRoleAliasReplacesOnlySyntheticToolPin(t *testing.T) {
	const source = "CC = $(HOSTCC)\nexport CC\nselected := $(CC)\norigin := $(origin CC)\n"
	target := KbuildActionRoleToken("target", "cc")
	host := KbuildActionRoleToken("host", "cc")
	for _, test := range []struct {
		name          string
		synthetic     map[string]bool
		wantCC        string
		wantOrigin    string
		wantDemotions int
	}{
		{name: "source alias supersedes configured pin", synthetic: map[string]bool{"CC": true}, wantCC: host, wantOrigin: "file", wantDemotions: 1},
		{name: "genuine Make CLI wins source alias", wantCC: target, wantOrigin: "command line"},
	} {
		t.Run(test.name, func(t *testing.T) {
			kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", KbuildOptions{
				CommandLineVariables:              map[string]string{"CC": target, "HOSTCC": host},
				SyntheticToolCommandLineVariables: test.synthetic,
				AutoExportCommandLineVariables:    map[string]bool{},
				CaptureVariables:                  []string{"selected", "origin"},
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := kb.Variables["selected"]; got != test.wantCC {
				t.Errorf("selected CC = %q, want %q", got, test.wantCC)
			}
			if got := kb.Variables["origin"]; got != test.wantOrigin {
				t.Errorf("CC origin = %q, want %q", got, test.wantOrigin)
			}
			if got := kb.ExportedEnvironment()["CC"]; got != test.wantCC {
				t.Errorf("exported CC = %q, want %q", got, test.wantCC)
			}
			if got := kb.SyntheticToolCommandLineDemotions(); len(got) != test.wantDemotions {
				t.Errorf("tool pin demotions = %v, want %d", got, test.wantDemotions)
			}
		})
	}

	initial, err := parseKbuildWithOptions(strings.NewReader("CC = clang\nselected := $(CC)\norigin := $(origin CC)\n"), "Makefile", KbuildOptions{
		CommandLineVariables:              map[string]string{"CC": target},
		SyntheticToolCommandLineVariables: map[string]bool{"CC": true},
		CaptureVariables:                  []string{"selected", "origin"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if initial.Variables["selected"] != target || initial.Variables["origin"] != "command line" || len(initial.SyntheticToolCommandLineDemotions()) != 0 {
		t.Fatalf("root selected compiler binding = %#v, origin %q, demotions %v", initial.Variables, initial.Variables["origin"], initial.SyntheticToolCommandLineDemotions())
	}
	_, err = parseKbuildWithOptions(strings.NewReader("ifeq ($(shell host-link-probe),1)\nCC = $(HOSTCC)\nendif\n"), "Makefile", KbuildOptions{
		CommandLineVariables:              map[string]string{"CC": target, "HOSTCC": host},
		SyntheticToolCommandLineVariables: map[string]bool{"CC": true},
		Shell: func(command string) (string, error) {
			if command != "host-link-probe" {
				return "", fmt.Errorf("unexpected probe %q", command)
			}
			return linuxProbeSymbolPrefix + strings.Repeat("f", 64), nil
		},
		SelectSymbolic: func(_, _ string, _ bool, _, _ string) (string, bool, error) {
			return linuxProbeSymbolPrefix + strings.Repeat("e", 64), true, nil
		},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "needs conditional command-line origin") {
		t.Fatalf("guarded tool alias changed all branches' compiler origin: %v", err)
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
