package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIncludePath(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{`#include "local.h"`, "local.h", true},
		{`#include <linux/sched.h>`, "linux/sched.h", true},
		{`#  include   "spaced.h"`, "spaced.h", true},
		{`#include<linux/nospace.h>`, "linux/nospace.h", true},
		{`# include <asm/page.h> /* trailing */`, "asm/page.h", true},
		{`   #include "indented.h"`, "indented.h", true},
		{`#include MACRO_HEADER`, "", false},
		{`#included "not-a-directive.h"`, "", false},
		{`int included = 1;`, "", false},
		{`// #include "commented.h"`, "", false},
		{`#define X 1`, "", false},
	}
	for _, tc := range cases {
		got, ok := includePath(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("includePath(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestConfigSourceScannerFollowsIncludeClosure(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", `
#include "foo.h"
#include <linux/bar.h>
#ifdef CONFIG_FOO_DIRECT
int direct;
#endif
`)
	mustWriteSource(t, root, "drivers/foo.h", `
#if IS_ENABLED(CONFIG_FOO_HEADER)
struct foo;
#endif
#include <linux/deep.h>
`)
	mustWriteSource(t, root, "include/linux/bar.h", "int bar(void); /* CONFIG_BAR_ANGLED in a comment is still counted */\n")
	mustWriteSource(t, root, "include/linux/deep.h", "#define X CONFIG_DEEP\n")

	s := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root, Srcarch: "x86"})
	got := s.refsForSource("drivers/foo.c", nil)
	want := []string{"CONFIG_BAR_ANGLED", "CONFIG_DEEP", "CONFIG_FOO_DIRECT", "CONFIG_FOO_HEADER"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refsForSource() = %v, want %v", got, want)
	}

	if got := s.refsForSource("does/not/exist.c", nil); got != nil {
		t.Fatalf("refsForSource(missing) = %v, want nil", got)
	}
}

func TestConfigSourceScannerFollowsKbuildIncludeDir(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "#include <private.h>\n#include <root-private.h>\n")
	mustWriteSource(t, root, "drivers/include/private.h", "#ifdef CONFIG_PRIVATE\n#endif\n")
	mustWriteSource(t, root, "root-private.h", "#ifdef CONFIG_ROOT_PRIVATE\n#endif\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got := scanner.refsForSource("drivers/foo.c", []string{"", "drivers/include"})
	if want := []string{"CONFIG_PRIVATE", "CONFIG_ROOT_PRIVATE"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refsForSource() = %v, want %v", got, want)
	}
}

func TestConfigSourceScannerReturnsRecursiveSourceIncludeClosure(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/bpf/prog.c", `
#include "../../shared/first.inc"
#include <linux/wrapper.h>
`)
	mustWriteSource(t, root, "shared/first.inc", `
#include "nested/second.inc"
#include "../drivers/bpf/prog.c"
`)
	mustWriteSource(t, root, "shared/nested/second.inc", "int second;\n")
	mustWriteSource(t, root, "include/linux/wrapper.h", `#include "../../shared/header-source.inc"`+"\n")
	mustWriteSource(t, root, "shared/header-source.inc", "int header_source;\n")
	mustWriteSource(t, root, "include/linux/angled-source.inc", "int angled_source;\n")
	mustWriteSource(t, root, "include/linux/wrapper.h", `
#include "../../shared/header-source.inc"
#include <linux/angled-source.inc>
`)
	mustWriteSource(t, root, "shared/unrelated.inc", "int unrelated;\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got := scanner.sourceIncludesForSource("drivers/bpf/prog.c", nil)
	want := []string{
		"include/linux/angled-source.inc",
		"shared/first.inc",
		"shared/header-source.inc",
		"shared/nested/second.inc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceIncludesForSource() = %v, want %v", got, want)
	}
}

func TestIncludeDirsFromFlags(t *testing.T) {
	got := includeDirsFromFlags([]string{
		"-Wall",
		"-I$(srctree)/drivers/foo",
		"-I",
		"$(srctree)/include/generated",
		"-I/absolute/path",
	})
	want := []string{"drivers/foo", "include/generated"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includeDirsFromFlags() = %v, want %v", got, want)
	}
}

func TestCompactFootprintSplitsOnSourceLevelConfig(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "#ifdef CONFIG_DEBUG\nint debug;\n#endif\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "off", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_DEBUG": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: root, Srcarch: "x86"})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	off := objectTarget(metadata, configByName(metadata, "off"), "init.o")
	on := objectTarget(metadata, configByName(metadata, "on"), "init.o")
	if off == "" || on == "" {
		t.Fatalf("missing init.o target: off=%q on=%q", off, on)
	}
	if off == on {
		t.Fatalf("source-level CONFIG_DEBUG did not split init.o: both = %q", off)
	}
	if got := variantByTarget(metadata, on).ConfigFragment["CONFIG_DEBUG"]; got != "y" {
		t.Fatalf("init.o (on) fragment CONFIG_DEBUG = %q, want y", got)
	}
	if got := variantByTarget(metadata, off).ConfigFragment["CONFIG_DEBUG"]; got != "n" {
		t.Fatalf("init.o (off) fragment CONFIG_DEBUG = %q, want n", got)
	}
}

func TestCompactFootprintSharesWhenSourceConfigAgrees(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "#ifdef CONFIG_DEBUG\nint debug;\n#endif\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "a", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "b", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_EFI_STUB": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: root, Srcarch: "x86"})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	a := objectTarget(metadata, configByName(metadata, "a"), "init.o")
	b := objectTarget(metadata, configByName(metadata, "b"), "init.o")
	if a == "" || b == "" {
		t.Fatalf("missing init.o target: a=%q b=%q", a, b)
	}
	if a != b {
		t.Fatalf("init.o split on an unreferenced config: a=%q b=%q", a, b)
	}
}

func TestCompactFootprintSplitsOnGlobalCompilerFlagConfig(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Global flags"

config FRAME_POINTER
	bool "Frame pointers"
`)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "int init(void) { return 0; }\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "off"},
		{Name: "on", Flags: map[string]string{"CONFIG_FRAME_POINTER": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: root})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	off := objectTarget(metadata, configByName(metadata, "off"), "init.o")
	on := objectTarget(metadata, configByName(metadata, "on"), "init.o")
	if off == on {
		t.Fatalf("global CONFIG_FRAME_POINTER did not split init.o: both = %q", off)
	}
}

func mustWriteSource(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) failed: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) failed: %v", path, err)
	}
}
