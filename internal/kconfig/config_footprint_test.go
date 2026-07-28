package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIncludeDirective(t *testing.T) {
	cases := []struct {
		text            string
		wantPath        string
		directiveTarget string
		literal         bool
		directive       bool
	}{
		{`#include "local.h"`, "local.h", "local.h", true, true},
		{`#include <linux/sched.h>`, "linux/sched.h", "linux/sched.h", true, true},
		{`#  include   "spaced.h"`, "spaced.h", "spaced.h", true, true},
		{`#include<linux/nospace.h>`, "linux/nospace.h", "linux/nospace.h", true, true},
		{`# include <asm/page.h> /* trailing */`, "asm/page.h", "asm/page.h", true, true},
		{`   #include "indented.h"`, "indented.h", "indented.h", true, true},
		{`/* lead */ #include "leading-comment.h"`, "leading-comment.h", "leading-comment.h", true, true},
		{"/* lead\n */ #include \"multiline-comment.h\"", "multiline-comment.h", "multiline-comment.h", true, true},
		{`#/**/include "commented-space.h"`, "commented-space.h", "commented-space.h", true, true},
		{`#include/**/"commented-target.h"`, "commented-target.h", "commented-target.h", true, true},
		{"#\\\ninclude \"spliced-directive.h\"", "spliced-directive.h", "spliced-directive.h", true, true},
		{"#inc\\\nlude \"spliced-keyword.h\"", "spliced-keyword.h", "spliced-keyword.h", true, true},
		{`#include MACRO_HEADER`, "", "MACRO_HEADER", false, true},
		{`#/**/include MACRO_HEADER`, "", "MACRO_HEADER", false, true},
		{`#include/* comment */ MACRO_HEADER`, "", "MACRO_HEADER", false, true},
		{`#include_next <linux/next.h>`, "", "<linux/next.h>", false, true},
		{`#include\`, "", `\`, false, true},
		{`#include`, "", "", false, true},
		{`#included "not-a-directive.h"`, "", "", false, false},
		{`int included = 1;`, "", "", false, false},
		{"#\ninclude \"not-spliced.h\"", "", "", false, false},
		{`// #include "commented.h"`, "", "", false, false},
		{`#define X 1`, "", "", false, false},
	}
	for _, tc := range cases {
		var got string
		var literal, directive bool
		for _, line := range preprocessorLines(tc.text) {
			got, literal, directive = includeDirective(line)
			if directive {
				break
			}
		}
		if got != tc.directiveTarget || literal != tc.literal || directive != tc.directive {
			t.Errorf(
				"preprocessed includeDirective(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.text,
				got,
				literal,
				directive,
				tc.directiveTarget,
				tc.literal,
				tc.directive,
			)
		}
		path := ""
		if literal {
			path = got
		}
		if path != tc.wantPath {
			t.Errorf("preprocessed include path for %q = %q, want %q", tc.text, path, tc.wantPath)
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
#include "private/wrapper.h"
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
	mustWriteSource(t, root, "drivers/bpf/private/wrapper.h", `#include "nested.h"`+"\n")
	mustWriteSource(t, root, "drivers/bpf/private/nested.h", "int nested;\n")
	mustWriteSource(t, root, "shared/unrelated.inc", "int unrelated;\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got, complete := scanner.sourceIncludesForSource("drivers/bpf/prog.c", nil)
	want := []string{
		"drivers/bpf/private/nested.h",
		"drivers/bpf/private/wrapper.h",
		"include/linux/angled-source.inc",
		"include/linux/wrapper.h",
		"shared/first.inc",
		"shared/header-source.inc",
		"shared/nested/second.inc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceIncludesForSource() = %v, want %v", got, want)
	}
	if !complete {
		t.Fatal("sourceIncludesForSource() unexpectedly marked a literal closure incomplete")
	}
}

func TestConfigSourceScannerMarksTraceReincludeIncomplete(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/trace.c", "#include \"trace.h\"\n")
	mustWriteSource(t, root, "drivers/trace.h", `
#define TRACE_SYSTEM fixture
#include <trace/define_trace.h>
`)
	mustWriteSource(t, root, "include/trace/define_trace.h", "#include TRACE_INCLUDE(TRACE_INCLUDE_FILE)\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got, complete := scanner.sourceIncludesForSource("drivers/trace.c", nil)
	want := []string{"drivers/trace.h", "include/trace/define_trace.h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceIncludesForSource() = %v, want %v", got, want)
	}
	if complete {
		t.Fatal("sourceIncludesForSource() assumed the trace macro expansion was already resolved")
	}
}

func TestConfigSourceScannerMarksGlobalComputedIncludeIncomplete(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "#include <linux/wrapper.h>\n")
	mustWriteSource(t, root, "include/linux/wrapper.h", "#include GLOBAL_DYNAMIC_HEADER\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	_, complete := scanner.sourceIncludesForSource("drivers/foo.c", nil)
	if complete {
		t.Fatal("sourceIncludesForSource() ignored an unknown computed include in a global header")
	}
}

func TestConfigSourceScannerMarksComputedIncludeIncomplete(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", `
#include "private.h"
#include CONFIG_PRIVATE_HEADER
`)
	mustWriteSource(t, root, "drivers/private.h", "int private;\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got, complete := scanner.sourceIncludesForSource("drivers/foo.c", nil)
	if want := []string{"drivers/private.h"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceIncludesForSource() = %v, want %v", got, want)
	}
	if complete {
		t.Fatal("sourceIncludesForSource() marked a computed include complete")
	}
}

func TestConfigSourceScannerMarksAssemblerIncbinIncomplete(t *testing.T) {
	for _, directive := range []string{
		`.incbin "lib/default.bconf"`,
		`.incbin CONFIG_EFI_SBAT_FILE`,
	} {
		t.Run(directive, func(t *testing.T) {
			root := t.TempDir()
			mustWriteSource(t, root, "lib/data.S", directive+"\n")
			mustWriteSource(t, root, "lib/default.bconf", "kernel.printk = 1\n")

			scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
			_, complete := scanner.sourceIncludesForSource("lib/data.S", nil)
			if complete {
				t.Fatalf("sourceIncludesForSource() marked %q complete", directive)
			}
		})
	}
}

func TestConfigSourceScannerMarksUnresolvedLiteralIncludeIncomplete(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "#include \"missing-private.h\"\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	got, complete := scanner.sourceIncludesForSource("drivers/foo.c", nil)
	if len(got) != 0 {
		t.Fatalf("sourceIncludesForSource() = %v, want no resolved includes", got)
	}
	if complete {
		t.Fatal("sourceIncludesForSource() marked an unresolved literal include complete")
	}
}

func TestConfigSourceScannerAcceptsUnresolvedNonSourceLiteral(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "#include <generated/value.h>\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	closure := scanner.closureForSource("drivers/foo.c", nil, nil, true)
	if !closure.sourceIncludesComplete {
		t.Fatal("closureForSource() rejected an unresolved non-source include with a complete source search model")
	}
}

func TestCompactMetadataRequiresCompleteGeneratedIncludeManifest(t *testing.T) {
	tree := mustParseCompactFixture(t)
	root := t.TempDir()
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	mustWriteSource(t, root, "init.c", "#include <generated/value.h>\n")

	for _, test := range []struct {
		name             string
		manifestEntry    bool
		manifestComplete bool
		wantComplete     bool
	}{
		{name: "unspecified manifest"},
		{name: "exact entry without completeness assertion", manifestEntry: true},
		{name: "complete manifest", manifestComplete: true, wantComplete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			generatedIncludes := map[string][]string{}
			if test.manifestEntry {
				generatedIncludes["generated/value.h"] = nil
			}
			metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
				{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
			}, CompactMetadataOptions{
				Schema:                          CompactSchemaV012,
				SourceRoot:                      root,
				SourceGeneratedIncludes:         generatedIncludes,
				SourceGeneratedIncludesComplete: test.manifestComplete,
			})
			if err != nil {
				t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
			}
			variant := variantByTarget(metadata, objectTarget(metadata, configByName(metadata, "base"), "init.o"))
			if variant.SourceIncludesComplete != test.wantComplete {
				t.Fatalf("SourceIncludesComplete = %t, want %t", variant.SourceIncludesComplete, test.wantComplete)
			}
		})
	}
}

func TestConfigSourceScannerMarksUnresolvedForcedIncludeIncomplete(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "int foo;\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{SourceRoot: root})
	closure := scanner.closureForSource(
		"drivers/foo.c",
		nil,
		[]sourceForcedInclude{{path: "missing-forced.h"}},
		false,
	)
	if closure.sourceIncludesComplete {
		t.Fatal("closureForSource() marked an unresolved forced include complete")
	}
}

func TestConfigSourceScannerUsesGeneratedManifestForForcedInclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "int foo;\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		SourceRoot: root,
		SourceGeneratedIncludes: map[string][]string{
			"include/generated/value.h": nil,
		},
	})
	closure := scanner.closureForSource(
		"drivers/foo.c",
		nil,
		[]sourceForcedInclude{{path: "include/generated/value.h"}},
		false,
	)
	if !closure.sourceIncludesComplete {
		t.Fatal("generated forced include produced an incomplete closure")
	}
}

func TestConfigSourceScannerUsesGeneratedIncludeManifest(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", `
#include <asm/wrapper.h>
#include <generated/value.h>
`)
	mustWriteSource(t, root, "include/asm-generic/wrapper.h", "#ifdef CONFIG_WRAPPER\n#endif\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		SourceRoot: root,
		SourceGeneratedIncludes: map[string][]string{
			"asm/wrapper.h":     {"include/asm-generic/wrapper.h"},
			"generated/value.h": nil,
		},
	})
	closure := scanner.closureForSource("drivers/foo.c", nil, nil, false)
	if want := []string{"include/asm-generic/wrapper.h"}; !reflect.DeepEqual(closure.sourceIncludes, want) {
		t.Fatalf("SourceIncludes = %v, want %v", closure.sourceIncludes, want)
	}
	if want := []string{"CONFIG_WRAPPER"}; !reflect.DeepEqual(closure.refs, want) {
		t.Fatalf("refs = %v, want %v", closure.refs, want)
	}
	if !closure.sourceIncludesComplete {
		t.Fatal("generated include manifest produced an incomplete closure")
	}
}

func TestConfigSourceScannerDoesNotHideGeneratedBackingBehindSourceMatch(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo.c", "#include <generated/value.h>\n")
	mustWriteSource(t, root, "drivers/generated/value.h", "#ifdef CONFIG_SOURCE_MATCH\n#endif\n")
	mustWriteSource(t, root, "include/generated-backing.h", "#ifdef CONFIG_GENERATED_BACKING\n#endif\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		SourceRoot: root,
		SourceGeneratedIncludes: map[string][]string{
			"generated/value.h": {"include/generated-backing.h"},
		},
	})
	closure := scanner.closureForSource("drivers/foo.c", nil, nil, true)
	wantIncludes := []string{"drivers/generated/value.h", "include/generated-backing.h"}
	if !reflect.DeepEqual(closure.sourceIncludes, wantIncludes) {
		t.Fatalf("SourceIncludes = %v, want %v", closure.sourceIncludes, wantIncludes)
	}
	wantRefs := []string{"CONFIG_GENERATED_BACKING", "CONFIG_SOURCE_MATCH"}
	if !reflect.DeepEqual(closure.refs, wantRefs) {
		t.Fatalf("refs = %v, want %v", closure.refs, wantRefs)
	}
	if !closure.sourceIncludesComplete {
		t.Fatal("combined source/generated closure was marked incomplete")
	}
}

func TestIncludeDirsFromFlags(t *testing.T) {
	got := includeDirsFromFlags([]string{
		"-Wall",
		"-I$(srctree)/drivers/foo",
		"-I",
		"$(srctree)/include/generated",
		"-iquote$(src)/private",
		"-isystem",
		"vendor/include",
		"-iquote$(obj)/generated",
		"-idirafter${srctree}/brace",
		"-I/absolute/path",
	}, "drivers/net/foo.c")
	want := []string{
		"drivers/foo",
		"include/generated",
		"drivers/net/private",
		"vendor/include",
		"brace",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includeDirsFromFlags() = %v, want %v", got, want)
	}
}

func TestForcedIncludesFromFlags(t *testing.T) {
	got := forcedIncludesFromFlags([]string{
		"-include",
		"$(srctree)/include/linux/hidden.h",
		"-imacros$(src)/private.h",
		"-include${srctree}/include/linux/brace.h",
		"-include=$(obj)/utsversion-tmp.h",
	}, "init/version.c")
	want := []sourceForcedInclude{
		{path: "include/linux/hidden.h", direct: true},
		{path: "init/private.h", direct: true},
		{path: "include/linux/brace.h", direct: true},
		{path: "init/utsversion-tmp.h", direct: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forcedIncludesFromFlags() = %#v, want %#v", got, want)
	}
}

func TestSourcePreincludePathsTreatsShippedSourceAsC(t *testing.T) {
	want := []string{
		"include/linux/compiler-version.h",
		"include/linux/kconfig.h",
		"include/linux/compiler_types.h",
	}
	if got := sourcePreincludePaths("drivers/foo.c_shipped"); !reflect.DeepEqual(got, want) {
		t.Fatalf("sourcePreincludePaths() = %v, want %v", got, want)
	}
}

func TestSourceIncludeFlagsComplete(t *testing.T) {
	for _, flags := range [][]string{
		nil,
		{"-I$(srctree)/private", "-iquote", "$(src)", "-include", "$(srctree)/generated.h"},
		{"-isystem/opt/toolchain/include"},
	} {
		if !sourceIncludeFlagsComplete(flags) {
			t.Errorf("sourceIncludeFlagsComplete(%v) = false, want true", flags)
		}
	}
	for _, flags := range [][]string{
		{"-I"},
		{"-I$(UNKNOWN)/private"},
		{"-I$(srctree)/$(UNKNOWN)"},
		{"-I${src}/${UNKNOWN}"},
		{"-include", "$(obj)/generated.h"},
		{"-iprefix", "$(srctree)/private"},
		{"-Wp,-I,$(srctree)/private"},
		{"-Wp,-imacros,$(srctree)/private.h"},
		{"--include", "$(srctree)/private.h"},
		{"--imacros=$(srctree)/private.h"},
		{"-Xclang", "-include"},
	} {
		if sourceIncludeFlagsComplete(flags) {
			t.Errorf("sourceIncludeFlagsComplete(%v) = true, want false", flags)
		}
	}
}

func TestCompactFootprintTreatsObjectDirectoryIncludeAsIncomplete(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y += init.o
ccflags-y += -include $(obj)/utsversion-tmp.h
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "int init;\n")
	mustWriteSource(t, root, "utsversion-tmp.h", "int source_collision;\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "off", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_DEBUG": "y"}},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	off := objectTarget(metadata, configByName(metadata, "off"), "init.o")
	on := objectTarget(metadata, configByName(metadata, "on"), "init.o")
	if off == on {
		t.Fatalf("generated object-directory include did not retain the full config: both = %q", off)
	}
	if variantByTarget(metadata, on).SourceIncludesComplete {
		t.Fatal("generated object-directory include was marked complete")
	}
}

func TestCompactFootprintFollowsIncludeFlags(t *testing.T) {
	tree := mustParseCompactFixture(t)
	root := t.TempDir()
	kb, err := ParseKbuild(strings.NewReader(fmt.Sprintf(`
obj-y += init.o
ccflags-y += -iquote%s/private -include %s/forced.h
`, root, root)), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	mustWriteSource(t, root, "init.c", "#include \"quoted.h\"\n")
	mustWriteSource(t, root, "private/quoted.h", "int quoted;\n")
	mustWriteSource(t, root, "forced.h", "#ifdef CONFIG_DEBUG\nint forced;\n#endif\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "off", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_DEBUG": "y"}},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	off := objectTarget(metadata, configByName(metadata, "off"), "init.o")
	on := objectTarget(metadata, configByName(metadata, "on"), "init.o")
	if off == on {
		t.Fatalf("forced-header CONFIG_DEBUG did not split init.o: both = %q", off)
	}
	variant := variantByTarget(metadata, on)
	if want := []string{"forced.h", "private/quoted.h"}; !reflect.DeepEqual(variant.SourceIncludes, want) {
		t.Fatalf("SourceIncludes = %v, want %v (flags: %v)", variant.SourceIncludes, want, variant.Flags)
	}
	if !variant.SourceIncludesComplete {
		t.Fatal("resolved include flags produced an incomplete source closure")
	}
	if got := variant.ConfigFragment["CONFIG_DEBUG"]; got != "y" {
		t.Fatalf("ConfigFragment[CONFIG_DEBUG] = %q, want y", got)
	}
}

func TestCompactFootprintScansDefaultPreincludes(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "int init;\n")
	mustWriteSource(t, root, "include/linux/compiler-version.h", "int compiler_version;\n")
	mustWriteSource(t, root, "include/linux/kconfig.h", "int kconfig;\n")
	mustWriteSource(t, root, "include/linux/compiler_types.h", "#ifdef CONFIG_DEBUG\nint compiler_type;\n#endif\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "off", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_DEBUG": "y"}},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	off := objectTarget(metadata, configByName(metadata, "off"), "init.o")
	on := objectTarget(metadata, configByName(metadata, "on"), "init.o")
	if off == on {
		t.Fatalf("default-preinclude CONFIG_DEBUG did not split init.o: both = %q", off)
	}
	variant := variantByTarget(metadata, on)
	if len(variant.SourceIncludes) != 0 {
		t.Fatalf("SourceIncludes = %v, want no redundant classified preinclude headers", variant.SourceIncludes)
	}
	if !variant.SourceIncludesComplete {
		t.Fatal("resolved default preincludes produced an incomplete source closure")
	}
}

func TestCompactFootprintIncludesDefaultPreincludeClosure(t *testing.T) {
	tree := mustParseCompactFixture(t)
	root := t.TempDir()
	kb, err := ParseKbuild(strings.NewReader(fmt.Sprintf(`
obj-y += init.o
ccflags-y += -I%s/private
`, root)), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	mustWriteSource(t, root, "init.c", "int init;\n")
	mustWriteSource(t, root, "include/linux/compiler_types.h", `
#include "private/generated.inc"
#include <kbuild.inc>
`)
	mustWriteSource(t, root, "include/linux/private/generated.inc", "#ifdef CONFIG_DEBUG\n#endif\n")
	mustWriteSource(t, root, "private/kbuild.inc", "#ifdef CONFIG_EFI_STUB\n#endif\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_DEBUG": "y", "CONFIG_EFI_STUB": "y"}},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	variant := variantByTarget(metadata, objectTarget(metadata, configByName(metadata, "base"), "init.o"))
	wantIncludes := []string{"include/linux/private/generated.inc", "private/kbuild.inc"}
	if !reflect.DeepEqual(variant.SourceIncludes, wantIncludes) {
		t.Fatalf("SourceIncludes = %v, want %v", variant.SourceIncludes, wantIncludes)
	}
	if !variant.SourceIncludesComplete {
		t.Fatal("resolved default-preinclude closure was marked incomplete")
	}
	if variant.ConfigFragment["CONFIG_DEBUG"] != "y" || variant.ConfigFragment["CONFIG_EFI_STUB"] != "y" {
		t.Fatalf("ConfigFragment = %v, want default-preinclude CONFIG refs", variant.ConfigFragment)
	}
}

func TestCompactFootprintPropagatesIncompleteDefaultPreinclude(t *testing.T) {
	tree := mustParseCompactFixture(t)
	root := t.TempDir()
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	mustWriteSource(t, root, "init.c", "int init;\n")
	mustWriteSource(t, root, "include/linux/compiler-version.h", "#include GENERATED_VERSION_HEADER\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	variant := variantByTarget(metadata, objectTarget(metadata, configByName(metadata, "base"), "init.o"))
	if variant.SourceIncludesComplete {
		t.Fatal("computed include in a default preinclude was marked complete")
	}
}

func TestObjectsWithGeneratedCompileSourcesRequireFallback(t *testing.T) {
	for _, object := range []string{
		"crypto/example.asn1.o",
		"arch/x86/kernel/cpu/capflags.o",
		"drivers/tty/vt/consolemap_deftbl.o",
	} {
		if !objectUsesGeneratedCSource(object) {
			t.Errorf("objectUsesGeneratedCSource(%q) = false, want true", object)
		}
	}
	if objectUsesGeneratedCSource("kernel/fork.o") {
		t.Fatal("ordinary C object requires generated-source fallback")
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
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
		Srcarch:                         "x86",
	})
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
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      root,
		SourceGeneratedIncludesComplete: true,
		Srcarch:                         "x86",
	})
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

func TestCompactComputedIncludeUsesFullConfig(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	root := t.TempDir()
	mustWriteSource(t, root, "init.c", "#include CONFIG_PRIVATE_HEADER\n")

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
		t.Fatalf("computed-include init.o shared across distinct configs: both = %q", off)
	}
	if got := variantByTarget(metadata, on).ConfigFragment["CONFIG_DEBUG"]; got != "y" {
		t.Fatalf("init.o (on) fragment CONFIG_DEBUG = %q, want y", got)
	}
}

func TestCompactComputedIncludeDoesNotChangeLegacyVariant(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	var targets []string
	for _, source := range []string{"int init;\n", "#include CONFIG_PRIVATE_HEADER\n"} {
		root := t.TempDir()
		mustWriteSource(t, root, "init.c", source)
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
			{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
		}, CompactMetadataOptions{Schema: CompactSchemaV011, SourceRoot: root, Srcarch: "x86"})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
		}
		target := objectTarget(metadata, configByName(metadata, "base"), "init.o")
		variant := variantByTarget(metadata, target)
		if variant.SourceIncludesComplete {
			t.Fatal("legacy object variant carries a source-include completeness marker")
		}
		targets = append(targets, target)
	}
	if targets[0] != targets[1] {
		t.Fatalf("legacy object target changed for computed include: %q != %q", targets[0], targets[1])
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
