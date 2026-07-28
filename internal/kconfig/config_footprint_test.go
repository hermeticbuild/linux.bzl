package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
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

func TestConfigSourceScannerV012PreservesLegacyCommentAndIncludeBehavior(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/legacy.c", "#if 0\n#include \"dead.inc\"\n#endif\n/* CONFIG_COMMENT_ONLY */\n")
	mustWriteSource(t, root, "drivers/dead.inc", "#ifdef CONFIG_DEAD_INCLUDE\n#endif\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV012,
		SourceRoot: root,
	})
	closure, err := scanner.closureForSource("drivers/legacy.c", nil)
	if err != nil {
		t.Fatalf("closureForSource() failed: %v", err)
	}
	if want := []string{"CONFIG_COMMENT_ONLY", "CONFIG_DEAD_INCLUDE"}; !reflect.DeepEqual(closure.refs, want) {
		t.Fatalf("closureForSource().refs = %v, want %v", closure.refs, want)
	}
	if want := []string{"drivers/dead.inc"}; !reflect.DeepEqual(closure.sourceIncludes, want) {
		t.Fatalf("closureForSource().sourceIncludes = %v, want %v", closure.sourceIncludes, want)
	}
}

func TestConfigSourceScannerV013StripsCommentsBeforeDirectives(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/comments.c", "/*\n#include COMMENT_ONLY\n*/\n#include /* separator */ \"literal.h\"\n")
	mustWriteSource(t, root, "drivers/literal.h", "#define LITERAL 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.exactClosureForSource("drivers/comments.c", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() failed: %v", err)
	}
	got := make([]string, 0, len(closure.sourceInputs))
	for _, input := range closure.sourceInputs {
		got = append(got, input.Path)
	}
	if want := []string{"drivers/comments.c", "drivers/literal.h"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exactClosureForSource().sourceInputs paths = %v, want %v", got, want)
	}
}

func TestConfigSourceScannerV013SplicesPreprocessorDirectives(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/spliced.c", "#inc\\\nlude HEADER_NAME\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	_, err := scanner.exactClosureForSource("drivers/spliced.c", nil)
	if err == nil {
		t.Fatal("exactClosureForSource() succeeded, want a nonliteral include error")
	}
	for _, want := range []string{
		"drivers/spliced.c:1",
		"unresolved potentially-active nonliteral include",
		"HEADER_NAME",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exactClosureForSource() error = %q, want substring %q", err, want)
		}
	}
}

func TestConfigSourceScannerV013FailsClosedOnContextCarryingIncludeMacro(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/block/drbd/drbd.c", "#include <linux/drbd_genl_api.h>\n")
	mustWriteSource(t, root, "include/linux/drbd_genl_api.h", `
#define GENL_MAGIC_INCLUDE_FILE <linux/drbd_genl.h>
#include <linux/genl_magic_struct.h>
`)
	mustWriteSource(t, root, "include/linux/genl_magic_struct.h", "#include GENL_MAGIC_INCLUDE_FILE\n")
	mustWriteSource(t, root, "include/linux/drbd_genl.h", "#define DRBD_GENL 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	_, err := scanner.exactClosureForSource("drivers/block/drbd/drbd.c", nil)
	if err == nil {
		t.Fatal("context-carrying include macro scan succeeded without macro-state modeling")
	}
	for _, want := range []string{
		"include/linux/genl_magic_struct.h:1",
		"unresolved potentially-active nonliteral include",
		"GENL_MAGIC_INCLUDE_FILE",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("context-carrying include error = %q, want substring %q", err, want)
		}
	}
}

func TestConfigSourceScannerV013ModelsLinuxLibfdtEnvironmentGuard(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "kernel/user.c", "#include <linux/libfdt.h>\n")
	mustWriteSource(t, root, "include/linux/libfdt.h", `
#include <linux/libfdt_env.h>
#include "../../scripts/dtc/libfdt/libfdt.h"
`)
	mustWriteSource(t, root, "include/linux/libfdt_env.h", `
#ifndef LIBFDT_ENV_H
#define LIBFDT_ENV_H
#define KERNEL_LIBFDT_ENV 1
#endif
`)
	mustWriteSource(t, root, "scripts/dtc/libfdt/libfdt.h", `
#include "libfdt_env.h"
#define LIBFDT_API 1
`)
	mustWriteSource(t, root, "scripts/dtc/libfdt/libfdt_env.h", `
#ifndef LIBFDT_ENV_H
#define LIBFDT_ENV_H
#include <stdlib.h>
#include <string.h>
#endif
`)
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.exactClosureForSource("kernel/user.c", nil)
	if err != nil {
		t.Fatalf("Linux libfdt wrapper scan failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"include/linux/libfdt.h",
		"include/linux/libfdt_env.h",
		"kernel/user.c",
		"scripts/dtc/libfdt/libfdt.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("Linux libfdt wrapper inputs = %v, want %v", paths, want)
	}

	mustWriteSource(t, root, "lib/fdt.c", `
#include <linux/libfdt_env.h>
#include "../scripts/dtc/libfdt/fdt.c"
`)
	mustWriteSource(t, root, "scripts/dtc/libfdt/fdt.c", `
#include "libfdt_env.h"
#include <libfdt.h>
#define LIBFDT_IMPLEMENTATION 1
`)
	closure, err = scanner.exactClosureForSource(
		"lib/fdt.c",
		[]string{"scripts/dtc/libfdt"},
	)
	if err != nil {
		t.Fatalf("Linux libfdt implementation wrapper scan failed: %v", err)
	}
	paths = paths[:0]
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want = []string{
		"include/linux/libfdt_env.h",
		"lib/fdt.c",
		"scripts/dtc/libfdt/fdt.c",
		"scripts/dtc/libfdt/libfdt.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("Linux libfdt implementation inputs = %v, want %v", paths, want)
	}

	mustWriteSource(t, root, "kernel/direct.c", `
#include "../scripts/dtc/libfdt/libfdt.h"
`)
	_, err = scanner.exactClosureForSource("kernel/direct.c", nil)
	if err == nil || !strings.Contains(err.Error(), "stdlib.h") {
		t.Fatalf("direct scripts libfdt environment error = %v, want unresolved stdlib.h", err)
	}
}

func TestConfigSourceScannerV013ConfigGatesNonliteralInclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/config-gated.c", "#ifdef CONFIG_CUSTOM\n#include CONFIG_CUSTOM_FILE\n#endif\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	off := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_CUSTOM": "n"},
		Written:   map[string]bool{"CONFIG_CUSTOM": true},
	}
	closure, err := scanner.closureForSourceConfig("drivers/config-gated.c", nil, off)
	if err != nil {
		t.Fatalf("closureForSourceConfig(off) failed: %v", err)
	}
	if got := len(closure.sourceInputs); got != 1 {
		t.Fatalf("closureForSourceConfig(off) source input count = %d, want 1", got)
	}

	on := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_CUSTOM": "y"},
		Written:   map[string]bool{"CONFIG_CUSTOM": true},
	}
	_, err = scanner.closureForSourceConfig("drivers/config-gated.c", nil, on)
	if err == nil {
		t.Fatal("closureForSourceConfig(on) succeeded, want a nonliteral include error")
	}
	for _, want := range []string{
		"drivers/config-gated.c:2",
		"unresolved potentially-active nonliteral include",
		"CONFIG_CUSTOM_FILE",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("closureForSourceConfig(on) error = %q, want substring %q", err, want)
		}
	}
}

func TestConfigSourceScannerV013CachesPreprocessingAcrossConfigs(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/config-gated.c", `#ifdef CONFIG_CUSTOM
#include "enabled.h"
#else
#include "disabled.h"
#endif
`)
	mustWriteSource(t, root, "drivers/enabled.h", "#define ENABLED 1\n")
	mustWriteSource(t, root, "drivers/disabled.h", "#define DISABLED 1\n")

	opts := CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	}
	sourceCache := newCompactSourceCache()
	contextScans := 0
	scans := 0
	for _, test := range []struct {
		name   string
		state  string
		header string
	}{
		{name: "off", state: "n", header: "drivers/disabled.h"},
		{name: "on", state: "y", header: "drivers/enabled.h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scanner := newConfigSourceScannerWithCache(opts, sourceCache)
			config := &ResolvedConfig{
				Effective: map[string]string{"CONFIG_CUSTOM": test.state},
				Written:   map[string]bool{"CONFIG_CUSTOM": true},
			}
			closure, err := scanner.closureForSourceConfig("drivers/config-gated.c", nil, config)
			if err != nil {
				t.Fatalf("closureForSourceConfig() failed: %v", err)
			}
			var paths []string
			for _, input := range closure.sourceInputs {
				paths = append(paths, input.Path)
			}
			want := []string{"drivers/config-gated.c", test.header}
			sort.Strings(want)
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("source inputs = %v, want %v", paths, want)
			}
			contextScans += len(scanner.files)
			scans++
			if scans == 1 {
				mustWriteSource(t, root, "drivers/config-gated.c", "#include \"replacement.h\"\n")
				mustWriteSource(t, root, "drivers/replacement.h", "#define REPLACEMENT 1\n")
			}
		})
	}

	if got, want := contextScans, 4; got != want {
		t.Fatalf("context-specific scan cache entries = %d, want %d", got, want)
	}
	if got, want := len(sourceCache.exactFiles), 3; got != want {
		t.Fatalf("preprocessed file cache size = %d, want %d", got, want)
	}
}

func TestConfigSourceScannerCachesMissingTreePaths(t *testing.T) {
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: t.TempDir(),
	})
	for range 2 {
		if path, ok := scanner.absForTreePath("include/missing.h"); ok || path != "" {
			t.Fatalf("absForTreePath(missing) = (%q, %v), want (\"\", false)", path, ok)
		}
	}
	if got, want := len(scanner.sourceCache.treePaths), 1; got != want {
		t.Fatalf("tree path cache size = %d, want %d", got, want)
	}
}

func TestConfigSourceScannerV013SeparatesVirtualTreePaths(t *testing.T) {
	root := t.TempDir()
	mapped := t.TempDir()
	mustWriteSource(t, mapped, "alias.h", `#if __has_include("relative.h")
#include "relative.h"
#endif
`)
	mustWriteSource(t, root, "one/relative.h", "#define RELATIVE 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		SourceRoots: map[string]string{
			"one": mapped,
			"two": mapped,
		},
	})
	for _, test := range []struct {
		source string
		want   []string
	}{
		{source: "one/alias.h", want: []string{"one/alias.h", "one/relative.h"}},
		{source: "two/alias.h", want: []string{"two/alias.h"}},
	} {
		closure, err := scanner.exactClosureForSource(test.source, nil)
		if err != nil {
			t.Fatalf("exactClosureForSource(%q) failed: %v", test.source, err)
		}
		var paths []string
		for _, input := range closure.sourceInputs {
			paths = append(paths, input.Path)
		}
		if !reflect.DeepEqual(paths, test.want) {
			t.Fatalf("exactClosureForSource(%q) inputs = %v, want %v", test.source, paths, test.want)
		}
	}

	if got, want := len(scanner.sourceCache.exactFiles), 2; got != want {
		t.Fatalf("shared physical file cache size = %d, want %d", got, want)
	}
}

func TestConfigSourceScannerV013ScansDirectoryFromSourceRootsOnly(t *testing.T) {
	mapped := t.TempDir()
	mustWriteSource(t, mapped, "payload/first.c", `
#include "local.h"
#ifdef CONFIG_MAPPED_DIRECTORY
#endif
`)
	mustWriteSource(t, mapped, "payload/local.h", "#define MAPPED_LOCAL 1\n")
	mustWriteSource(t, mapped, "payload/second.S", "#define MAPPED_ASSEMBLY 1\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema: CompactSchemaV013,
		SourceRoots: map[string]string{
			"virtual": mapped,
		},
	})
	closure, err := scanner.exactClosureForSourceDir("virtual/payload")
	if err != nil {
		t.Fatalf("exactClosureForSourceDir() failed: %v", err)
	}
	if want := []string{"CONFIG_MAPPED_DIRECTORY"}; !reflect.DeepEqual(closure.refs, want) {
		t.Fatalf("mapped directory refs = %v, want %v", closure.refs, want)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"virtual/payload/first.c",
		"virtual/payload/local.h",
		"virtual/payload/second.S",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("mapped directory inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013ResolvesConfigStringInclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/config-include.c", "#include CONFIG_CUSTOM_FILE\n")
	mustWriteSource(t, root, "drivers/custom.h", "#define CUSTOM 1\n")
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_CUSTOM_FILE": `"custom.h"`},
		Written:   map[string]bool{"CONFIG_CUSTOM_FILE": true},
	}
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.closureForSourceConfig("drivers/config-include.c", nil, config)
	if err != nil {
		t.Fatalf("closureForSourceConfig() failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	if want := []string{"drivers/config-include.c", "drivers/custom.h"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("config-string include inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013RejectsUnresolvedLiteralInclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/missing.c", "#include \"missing.h\"\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	_, err := scanner.exactClosureForSource("drivers/missing.c", nil)
	if err == nil {
		t.Fatal("exactClosureForSource() accepted an unresolved literal include")
	}
	for _, want := range []string{
		`drivers/missing.c:1`,
		`unresolved potentially-active literal include`,
		`"missing.h"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exactClosureForSource() error = %q, want substring %q", err, want)
		}
	}
}

func TestConfigSourceScannerV013ModelsTraceTemplateReinclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/trace.h", "#include <trace/define_trace.h>\n")
	mustWriteSource(t, root, "include/trace/define_trace.h", "#include TRACE_INCLUDE(TRACE_INCLUDE_FILE)\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.exactClosureForSource("drivers/trace.h", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() failed on modeled trace reinclude: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	if want := []string{"drivers/trace.h", "include/trace/define_trace.h"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("trace template inputs = %v, want %v", paths, want)
	}
}

func TestPreprocessorSymbolDefinedDistinguishesModules(t *testing.T) {
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_DRIVER": "m"},
		Written:   map[string]bool{"CONFIG_DRIVER": true},
	}
	if defined, known := preprocessorSymbolDefined([]string{"CONFIG_DRIVER"}, config); !known || defined {
		t.Fatalf("CONFIG_DRIVER defined/known = %v/%v, want false/true", defined, known)
	}
	if defined, known := preprocessorSymbolDefined([]string{"CONFIG_DRIVER_MODULE"}, config); !known || !defined {
		t.Fatalf("CONFIG_DRIVER_MODULE defined/known = %v/%v, want true/true", defined, known)
	}
}

func TestConfigSourceScannerV013KnowsKernelActionMacro(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/uapi/linux/if.h", "#ifndef __KERNEL__\n#include <sys/socket.h>\n#endif\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.exactClosureForSource("include/uapi/linux/if.h", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() followed userspace-only libc include: %v", err)
	}
	if got := len(closure.sourceInputs); got != 1 || closure.sourceInputs[0].Path != "include/uapi/linux/if.h" {
		t.Fatalf("kernel macro source inputs = %v, want only the UAPI header", closure.sourceInputs)
	}
}

func TestConfigSourceScannerV013CollectsOnlyPotentiallyActiveRefs(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/active.c", `
#if defined(CONFIG_GATE)
int active = CONFIG_ACTIVE_VALUE;
#else
int inactive_else = CONFIG_INACTIVE_ELSE;
#endif
#if 0
int dead = CONFIG_DEAD_BODY;
#if defined(CONFIG_DEAD_CONDITION)
int nested_dead;
#endif
#endif
`)
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_GATE": "y"},
		Written:   map[string]bool{"CONFIG_GATE": true},
	}
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.closureForSourceConfig("drivers/active.c", nil, config)
	if err != nil {
		t.Fatalf("closureForSourceConfig() failed: %v", err)
	}
	want := []string{"CONFIG_ACTIVE_VALUE", "CONFIG_GATE"}
	if !reflect.DeepEqual(closure.refs, want) {
		t.Fatalf("active CONFIG refs = %v, want %v", closure.refs, want)
	}
}

func TestConfigSourceScannerV013EvaluatesKernelConfigPredicates(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/predicates.c", `
#if IS_BUILTIN(CONFIG_DRIVER)
#include "builtin.h"
#elif IS_MODULE(CONFIG_DRIVER)
#include "module.h"
#endif
#if IS_ENABLED(CONFIG_FEATURE)
#include "enabled.h"
#endif
#if IS_REACHABLE(CONFIG_DRIVER)
#include "reachable.h"
#else
#include "unreachable.h"
#endif
`)
	for _, header := range []string{
		"builtin.h",
		"module.h",
		"enabled.h",
		"reachable.h",
		"unreachable.h",
	} {
		mustWriteSource(t, root, "drivers/"+header, "#define TEST 1\n")
	}
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	moduleConfig := &ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_DRIVER":  "m",
			"CONFIG_FEATURE": "m",
		},
		Written: map[string]bool{
			"CONFIG_DRIVER":  true,
			"CONFIG_FEATURE": true,
		},
	}
	closure, err := scanner.closureForSourceConfigProfile(
		"drivers/predicates.c",
		nil,
		moduleConfig,
		sourceScanKernelModule,
	)
	if err != nil {
		t.Fatalf("module predicate scan failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"drivers/enabled.h",
		"drivers/module.h",
		"drivers/predicates.c",
		"drivers/reachable.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("module predicate inputs = %v, want %v", paths, want)
	}

	closure, err = scanner.closureForSourceConfigProfile(
		"drivers/predicates.c",
		nil,
		moduleConfig,
		sourceScanKernel,
	)
	if err != nil {
		t.Fatalf("built-in action predicate scan failed: %v", err)
	}
	paths = paths[:0]
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want = []string{
		"drivers/enabled.h",
		"drivers/module.h",
		"drivers/predicates.c",
		"drivers/unreachable.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("built-in action predicate inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013UsesCompilerIncludeOrder(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/main.c", `
#include "duplicate.h"
#include <duplicate.h>
`)
	mustWriteSource(t, root, "drivers/duplicate.h", "#define LOCAL_DUPLICATE 1\n")
	mustWriteSource(t, root, "arch/x86/include/duplicate.h", "#define ARCH_DUPLICATE 1\n")
	mustWriteSource(t, root, "include/duplicate.h", "#define GENERIC_DUPLICATE 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	closure, err := scanner.closureForSourceConfigInputsSearchProfile(
		"drivers/main.c",
		scanner.actionIncludeSearch("drivers/main.c", nil),
		nil,
		false,
		nil,
		sourceScanKernel,
	)
	if err != nil {
		t.Fatalf("exactClosureForSource() failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"drivers/duplicate.h",
		"drivers/main.c",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ordered include inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013PreservesActionIncludeClassesAndOrder(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/foo/main.c", `
#include <wrapper.h>
#include "quoted.h"
#include "quote-only.h"
#include <ordered.h>
#include <system-only.h>
`)
	mustWriteSource(t, root, "include/wrapper.h", "#include <private.h>\n")
	mustWriteSource(t, root, "drivers/foo/private.h", "#define TU_PRIVATE 1\n")
	mustWriteSource(t, root, "drivers/foo/quoted.h", "#define LOCAL_QUOTED 1\n")
	mustWriteSource(t, root, "include/private.h", "#define GLOBAL_PRIVATE 1\n")
	mustWriteSource(t, root, "quote/quoted.h", "#define SHADOWED_QUOTED 1\n")
	mustWriteSource(t, root, "quote/quote-only.h", "#define QUOTE_ONLY 1\n")
	mustWriteSource(t, root, "first/ordered.h", "#define FIRST 1\n")
	mustWriteSource(t, root, "second/ordered.h", "#define SECOND 1\n")
	mustWriteSource(t, root, "system/system-only.h", "#define SYSTEM 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	flags := []string{
		"-iquote$(srctree)/quote",
		"-I$(srctree)/first",
		"-I", "$(srctree)/second",
		"-isystem", "$(srctree)/system",
	}
	closure, err := scanner.closureForSourceConfigInputsSearchProfile(
		"drivers/foo/main.c",
		scanner.actionIncludeSearch("drivers/foo/main.c", flags),
		nil,
		false,
		nil,
		sourceScanKernel,
	)
	if err != nil {
		t.Fatalf("action include scan failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"drivers/foo/main.c",
		"drivers/foo/private.h",
		"drivers/foo/quoted.h",
		"first/ordered.h",
		"include/wrapper.h",
		"quote/quote-only.h",
		"system/system-only.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("action include inputs = %v, want %v", paths, want)
	}
	for _, shadowed := range []string{"include/private.h", "quote/quoted.h", "second/ordered.h"} {
		if slices.Contains(paths, shadowed) {
			t.Errorf("action include scan selected shadowed path %q: %v", shadowed, paths)
		}
	}
}

func TestConfigSourceScannerV013ResolvesIncludeNextAfterCurrentRoot(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/main.c", "#include <wrapper.h>\n")
	mustWriteSource(t, root, "arch/x86/include/wrapper.h", "#include_next <wrapper.h>\n")
	mustWriteSource(t, root, "include/wrapper.h", "#define GENERIC_WRAPPER 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	closure, err := scanner.exactClosureForSource("drivers/main.c", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"arch/x86/include/wrapper.h",
		"drivers/main.c",
		"include/wrapper.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("include_next inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013EvaluatesLiteralHasInclude(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "security/landlock/errata.h", `
#if __has_include("errata/present.h")
#include "errata/present.h"
#else
#include "unexpected-present-fallback.h"
#endif
#if __has_include("errata/absent.h")
#include "errata/absent.h"
#else
#include "errata/fallback.h"
#endif
`)
	mustWriteSource(t, root, "security/landlock/errata/present.h", "#define PRESENT 1\n")
	mustWriteSource(t, root, "security/landlock/errata/fallback.h", "#define FALLBACK 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
	})
	closure, err := scanner.exactClosureForSource("security/landlock/errata.h", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() failed: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"security/landlock/errata.h",
		"security/landlock/errata/fallback.h",
		"security/landlock/errata/present.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("__has_include inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013ModelsClangLinuxPredefines(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/acpi/platform/acenv.h", `
#if defined(_MSC_VER)
#include "acmsvc.h"
#elif defined(__GNUC__) && !defined(__APPLE__)
#include "acgcc.h"
#endif
#if defined(__linux__) || defined(linux)
#include "aclinux.h"
#endif
#ifdef ACPI_USE_STANDARD_HEADERS
#include <stdlib.h>
#endif
#ifndef ACPI_USE_SYSTEM_CLIBRARY
#include "acclib.h"
#endif
#ifdef ACPI_ASL_COMPILER
#include <stdio.h>
#endif
#ifdef ACPI_DISASSEMBLER
#include "acdisasm.h"
#endif
#if defined(__ELF__) && defined(__has_include)
#include <cet.h>
#endif
`)
	mustWriteSource(t, root, "include/acpi/platform/acgcc.h", "#define ACPI_GCC 1\n")
	mustWriteSource(t, root, "include/acpi/platform/aclinux.h", "#define ACPI_LINUX 1\n")

	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	closure, err := scanner.exactClosureForSource("include/acpi/platform/acenv.h", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() followed an inactive compiler branch: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{
		"include/acpi/platform/acenv.h",
		"include/acpi/platform/acgcc.h",
		"include/acpi/platform/aclinux.h",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("compiler predefined inputs = %v, want %v", paths, want)
	}
}

func TestConfigSourceScannerV013TracksAssemblyPredefine(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "arch/x86/entry.S", `
#ifdef __ASSEMBLY__
#include "assembly.h"
#else
#include "c-only.h"
#endif
#ifndef __ASSEMBLER__
#include "assembler-c-only.h"
#endif
`)
	mustWriteSource(t, root, "arch/x86/assembly.h", "#define ASSEMBLY 1\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	closure, err := scanner.exactClosureForSource("arch/x86/entry.S", nil)
	if err != nil {
		t.Fatalf("exactClosureForSource() followed a C-only assembly branch: %v", err)
	}
	var paths []string
	for _, input := range closure.sourceInputs {
		paths = append(paths, input.Path)
	}
	want := []string{"arch/x86/assembly.h", "arch/x86/entry.S"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("assembly predefined inputs = %v, want %v", paths, want)
	}
}

func TestGeneratedHeaderFootprintArm64BindsDirectInputsAndConfig(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"arch/arm64/kernel/vdso",
		"arch/arm64/kernel/vdso32",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", dir, err)
		}
	}
	for path, content := range map[string]string{
		"arch/arm/vdso/vdsomunge.c":                         "int vdsomunge;\n",
		"arch/arm64/include/asm/cfi.h":                      "#define ARM64_CFI 1\n",
		"arch/arm64/include/asm/vdso/compat_gettimeofday.h": "#define COMPAT_GETTIMEOFDAY 1\n",
		"arch/arm64/include/asm/vdso/gettimeofday.h": `
#ifdef __aarch64__
#include "native_gettimeofday.h"
#else
#include "compat_gettimeofday.h"
#endif
`,
		"arch/arm64/include/asm/vdso/native_gettimeofday.h": "#define NATIVE_GETTIMEOFDAY 1\n",
		"arch/arm64/kernel/vdso32/vgettimeofday.c":          "#include <vdso/gettime.h>\n",
		"arch/arm64/tools/cpucaps":                          "CAP_ONE\n",
		"arch/arm64/tools/syscall_32.tbl":                   "0 common read\n",
		"arch/arm64/tools/syscall_64.tbl":                   "0 common read\n",
		"arch/arm64/tools/sysreg":                           "Sysreg TEST\nEndSysreg\n",
		"include/vdso/datapage.h":                           "#include <asm/vdso/gettimeofday.h>\n",
		"include/vdso/gettime.h":                            "#define VDSO_GETTIME 1\n",
		"lib/vdso/getrandom.c":                              "#ifdef CONFIG_VDSO_GETRANDOM\n#endif\n",
		"lib/vdso/gettimeofday.c": `
#include <vdso/datapage.h>
#ifdef CONFIG_VDSO_GETTIMEOFDAY
#endif
`,
	} {
		mustWriteSource(t, root, path, content)
	}
	config := &ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_STACKPROTECTOR_PER_TASK": "y",
			"CONFIG_VDSO_GETRANDOM":          "y",
			"CONFIG_VDSO_GETTIMEOFDAY":       "y",
		},
		Written: map[string]bool{
			"CONFIG_STACKPROTECTOR_PER_TASK": true,
			"CONFIG_VDSO_GETRANDOM":          true,
			"CONFIG_VDSO_GETTIMEOFDAY":       true,
		},
	}
	generate := func() (map[string]string, []CompactSourceInput, string) {
		t.Helper()
		opts := CompactMetadataOptions{
			Schema:     CompactSchemaV013,
			SourceRoot: root,
			Srcarch:    "arm64",
		}
		scanner := newConfigSourceScanner(opts)
		families, err := generatedHeaderFamilyFootprints(config, opts, scanner)
		if err != nil {
			t.Fatalf("generatedHeaderFamilyFootprints() failed: %v", err)
		}
		if len(families) != 1 || families[0].name != compactGeneratedHeaderFamilyAll {
			t.Fatalf("arm64 generated header families = %#v, want only all", families)
		}
		fragment := families[0].fragment
		inputs := families[0].sourceInputs
		payload := newCompactConfigPayload(fragment)
		family := newCompactGeneratedHeaderFamily(
			compactGeneratedHeaderFamilyAll,
			payload.ID,
			"//headers:arm64",
			"arm64",
			nil,
			inputs,
		)
		return fragment, inputs, family.ID
	}

	fragment, inputs, beforeID := generate()
	for _, symbol := range []string{
		"CONFIG_STACKPROTECTOR_PER_TASK",
		"CONFIG_VDSO_GETRANDOM",
		"CONFIG_VDSO_GETTIMEOFDAY",
	} {
		if got := fragment[symbol]; got != "y" {
			t.Errorf("header fragment %s = %q, want y", symbol, got)
		}
	}
	var paths []string
	for _, input := range inputs {
		paths = append(paths, input.Path)
	}
	for _, want := range []string{
		"arch/arm/vdso/vdsomunge.c",
		"arch/arm64/include/asm/cfi.h",
		"arch/arm64/include/asm/vdso/compat_gettimeofday.h",
		"arch/arm64/include/asm/vdso/gettimeofday.h",
		"arch/arm64/include/asm/vdso/native_gettimeofday.h",
		"arch/arm64/kernel/vdso32/vgettimeofday.c",
		"arch/arm64/tools/cpucaps",
		"arch/arm64/tools/syscall_32.tbl",
		"arch/arm64/tools/syscall_64.tbl",
		"arch/arm64/tools/sysreg",
		"include/vdso/datapage.h",
		"include/vdso/gettime.h",
		"lib/vdso/getrandom.c",
		"lib/vdso/gettimeofday.c",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("arm64 header inputs = %v, want %q", paths, want)
		}
	}

	mustWriteSource(t, root, "arch/arm64/include/asm/vdso/compat_gettimeofday.h", "#define COMPAT_GETTIMEOFDAY 2\n")
	_, _, compatChangedID := generate()
	if compatChangedID == beforeID {
		t.Fatalf("compat gettimeofday change did not change arm64 generated-header family ID %q", beforeID)
	}
	beforeID = compatChangedID
	mustWriteSource(t, root, "arch/arm64/tools/cpucaps", "CAP_TWO\n")
	_, _, changedID := generate()
	if changedID == beforeID {
		t.Fatalf("cpucaps change did not change arm64 generated-header family ID %q", beforeID)
	}
	if err := os.Remove(filepath.Join(root, "arch/arm64/include/asm/cfi.h")); err != nil {
		t.Fatalf("Remove(cfi.h) failed: %v", err)
	}
	_, absentInputs, absentID := generate()
	if absentID == changedID {
		t.Fatalf("arm64 cfi.h presence did not change generated-header family ID %q", absentID)
	}
	for _, input := range absentInputs {
		if input.Path == "arch/arm64/include/asm/cfi.h" {
			t.Fatalf("absent cfi.h remained in inputs: %v", absentInputs)
		}
	}
}

func TestGeneratedHeaderFootprintArm64HypConstantsUsesProducerIncludeRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"arch/arm64/kernel/vdso",
		"arch/arm64/kernel/vdso32",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", dir, err)
		}
	}
	mustWriteSource(t, root, "arch/arm64/kvm/hyp/hyp-constants.c", `
#include <nvhe/memory.h>
`)
	mustWriteSource(t, root, "arch/arm64/kvm/hyp/include/nvhe/memory.h", `
#ifdef CONFIG_KVM
#define HYP_MEMORY 1
#endif
`)
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_KVM": "y"},
		Written:   map[string]bool{"CONFIG_KVM": true},
	}
	opts := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
		SourceRoot:            root,
		Srcarch:               "arm64",
		CompileEnvironmentABI: "test-abi",
	}
	families, err := generatedHeaderFamilyFootprints(
		config,
		opts,
		newConfigSourceScanner(opts),
	)
	if err != nil {
		t.Fatalf("generatedHeaderFamilyFootprints() failed: %v", err)
	}
	if got := families[0].fragment["CONFIG_KVM"]; got != "y" {
		t.Fatalf("arm64 all-family CONFIG_KVM = %q, want y", got)
	}
	if !slices.ContainsFunc(families[0].sourceInputs, func(input CompactSourceInput) bool {
		return input.Path == "arch/arm64/kvm/hyp/include/nvhe/memory.h"
	}) {
		t.Fatalf("arm64 hyp include root input missing from %#v", families[0].sourceInputs)
	}
}

func TestGeneratedHeaderCompilerVersionFeatureIncludesAreConfigExact(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/linux/compiler-version.h", `
#ifdef GCC_PLUGINS
#include <generated/gcc-plugins.h>
#endif
#ifdef RANDSTRUCT
#include <generated/randstruct_hash.h>
#endif
`)
	opts := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
		SourceRoot:            root,
		Srcarch:               "x86",
		CompileEnvironmentABI: "test-abi",
	}
	config := func(gccPlugins, randstruct string) *ResolvedConfig {
		return &ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_GCC_PLUGINS": gccPlugins,
				"CONFIG_RANDSTRUCT":  randstruct,
			},
			Written: map[string]bool{
				"CONFIG_GCC_PLUGINS": true,
				"CONFIG_RANDSTRUCT":  true,
			},
		}
	}
	if _, err := generatedHeaderFamilyFootprints(
		config("n", "n"),
		opts,
		newConfigSourceScanner(opts),
	); err != nil {
		t.Fatalf("disabled compiler-version feature scan failed: %v", err)
	}
	for _, tc := range []struct {
		name       string
		gccPlugins string
		randstruct string
		want       string
	}{
		{
			name:       "gcc plugins",
			gccPlugins: "y",
			randstruct: "n",
			want:       "generated/gcc-plugins.h",
		},
		{
			name:       "randstruct",
			gccPlugins: "n",
			randstruct: "y",
			want:       "generated/randstruct_hash.h",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generatedHeaderFamilyFootprints(
				config(tc.gccPlugins, tc.randstruct),
				opts,
				newConfigSourceScanner(opts),
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("active compiler-version feature error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGeneratedHeaderFamilyClassifierUsesOwnedSpellings(t *testing.T) {
	for _, tc := range []struct {
		path    string
		name    string
		precise bool
	}{
		{path: "generated/autoconf.h", precise: true},
		{path: "generated/integer-wrap.h", precise: true},
		{path: "generated/rustc_cfg", precise: true},
		{
			path:    "generated/rustc_cfg.h",
			name:    compactGeneratedHeaderFamilyAll,
			precise: false,
		},
		{
			path:    "generated/gcc-plugins.h",
			name:    compactGeneratedHeaderFamilyAll,
			precise: false,
		},
		{
			path:    "generated/randstruct_hash.h",
			name:    compactGeneratedHeaderFamilyAll,
			precise: false,
		},
		{
			path:    "generated/timeconst.h",
			name:    compactGeneratedHeaderFamilyTimeconst,
			precise: true,
		},
		{
			path:    "linux/version.h",
			name:    compactGeneratedHeaderFamilyVersion,
			precise: true,
		},
		{
			path:    "generated/uapi/linux/version.h",
			name:    compactGeneratedHeaderFamilyVersion,
			precise: true,
		},
		{
			path:    "asm/unistd.h",
			name:    compactGeneratedHeaderFamilyStatic,
			precise: true,
		},
		{path: "linux/kernel.h"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			name, precise := generatedHeaderFamilyNameForInclude(tc.path)
			if name != tc.name || precise != tc.precise {
				t.Fatalf(
					"generatedHeaderFamilyNameForInclude(%q) = (%q, %t), want (%q, %t)",
					tc.path,
					name,
					precise,
					tc.name,
					tc.precise,
				)
			}
		})
	}
}

func TestGeneratedHeaderOffsetsBindForcedHeadersAndProducerABI(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/linux/compiler-version.h", "#define COMPILER_VERSION_INPUT 1\n")
	mustWriteSource(t, root, "include/linux/kconfig.h", "#include <generated/autoconf.h>\n")
	mustWriteSource(t, root, "include/linux/compiler_types.h", `
#include "forced-detail.h"
#include <generated/timeconst.h>
`)
	mustWriteSource(t, root, "include/linux/forced-detail.h", "#define FORCED_DETAIL 1\n")
	mustWriteSource(t, root, "kernel/bounds.c", "int bounds;\n")
	config := &ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_GCC_PLUGINS": "n",
			"CONFIG_RANDSTRUCT":  "n",
		},
		Written: map[string]bool{
			"CONFIG_GCC_PLUGINS": true,
			"CONFIG_RANDSTRUCT":  true,
		},
	}
	generate := func(abi string) map[string]CompactGeneratedHeaderFamily {
		t.Helper()
		opts := CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            root,
			Srcarch:               "x86",
			CompileEnvironmentABI: abi,
		}
		footprints, err := generatedHeaderFamilyFootprints(
			config,
			opts,
			newConfigSourceScanner(opts),
		)
		if err != nil {
			t.Fatalf("generatedHeaderFamilyFootprints() failed: %v", err)
		}
		out := make(map[string]CompactGeneratedHeaderFamily, len(footprints))
		for _, footprint := range footprints {
			if got := footprint.fragment[generatedHeaderProducerABIKey]; got != abi {
				t.Fatalf("%s producer ABI = %q, want %q", footprint.name, got, abi)
			}
			payload := newCompactConfigPayload(footprint.fragment)
			out[footprint.name] = newCompactGeneratedHeaderFamily(
				footprint.name,
				payload.ID,
				"//headers:test",
				"x86",
				footprint.dependencies,
				footprint.sourceInputs,
			)
		}
		return out
	}

	before := generate("abi-one")
	for _, name := range []string{
		compactGeneratedHeaderFamilyBounds,
		compactGeneratedHeaderFamilyASMOffsets,
		compactGeneratedHeaderFamilyRQOffsets,
		compactGeneratedHeaderFamilyKVMOffsets,
	} {
		family := before[name]
		if !slices.Contains(family.Dependencies, compactGeneratedHeaderFamilyTimeconst) {
			t.Errorf("%s dependencies = %v, want forced-header %q", name, family.Dependencies, compactGeneratedHeaderFamilyTimeconst)
		}
		for _, path := range []string{
			"include/linux/compiler-version.h",
			"include/linux/kconfig.h",
			"include/linux/compiler_types.h",
			"include/linux/forced-detail.h",
		} {
			if !slices.ContainsFunc(family.SourceInputs, func(input CompactSourceInput) bool {
				return input.Path == path
			}) {
				t.Errorf("%s source inputs missing %q: %#v", name, path, family.SourceInputs)
			}
		}
	}

	changedABI := generate("abi-two")
	for name, family := range before {
		if got := changedABI[name].ID; got == family.ID {
			t.Errorf("%s family ID did not change with producer ABI %q", name, got)
		}
	}

	mustWriteSource(t, root, "include/linux/forced-detail.h", "#define FORCED_DETAIL 2\n")
	changedHeader := generate("abi-one")
	for _, name := range []string{
		compactGeneratedHeaderFamilyBounds,
		compactGeneratedHeaderFamilyASMOffsets,
		compactGeneratedHeaderFamilyRQOffsets,
		compactGeneratedHeaderFamilyKVMOffsets,
	} {
		if got := changedHeader[name].ID; got == before[name].ID {
			t.Errorf("%s family ID did not change with forced-header closure %q", name, got)
		}
	}
}

func TestGeneratedHeaderVersionFamiliesUseDeclaredInputsOnly(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", "VERSION = 1\n")
	mustWriteSource(t, root, "scripts/setlocalversion", "#!/bin/sh\necho unowned\n")
	generate := func(kernelVersion, localVersion string) map[string]CompactGeneratedHeaderFamily {
		t.Helper()
		config := &ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_GCC_PLUGINS":  "n",
				"CONFIG_LOCALVERSION": localVersion,
				"CONFIG_RANDSTRUCT":   "n",
			},
			Written: map[string]bool{
				"CONFIG_GCC_PLUGINS":  true,
				"CONFIG_LOCALVERSION": true,
				"CONFIG_RANDSTRUCT":   true,
			},
		}
		opts := CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            root,
			Srcarch:               "x86",
			CompileEnvironmentABI: "test-abi",
			KernelVersion:         kernelVersion,
		}
		footprints, err := generatedHeaderFamilyFootprints(
			config,
			opts,
			newConfigSourceScanner(opts),
		)
		if err != nil {
			t.Fatalf("generatedHeaderFamilyFootprints() failed: %v", err)
		}
		out := make(map[string]CompactGeneratedHeaderFamily, len(footprints))
		for _, footprint := range footprints {
			for _, input := range footprint.sourceInputs {
				if input.Path == "Makefile" || input.Path == "scripts/setlocalversion" {
					t.Errorf("%s family retained unowned input %q", footprint.name, input.Path)
				}
			}
			payload := newCompactConfigPayload(footprint.fragment)
			out[footprint.name] = newCompactGeneratedHeaderFamily(
				footprint.name,
				payload.ID,
				"//headers:test",
				"x86",
				footprint.dependencies,
				footprint.sourceInputs,
			)
		}
		return out
	}

	before := generate("6.18.39", `"-base"`)
	mustWriteSource(t, root, "Makefile", "VERSION = 999\n")
	mustWriteSource(t, root, "scripts/setlocalversion", "#!/bin/sh\necho changed\n")
	changedUnowned := generate("6.18.39", `"-base"`)
	for _, name := range []string{
		compactGeneratedHeaderFamilyVersion,
		compactGeneratedHeaderFamilyUTSRelease,
		compactGeneratedHeaderFamilyAll,
	} {
		if got := changedUnowned[name].ID; got != before[name].ID {
			t.Errorf("%s family ID changed with unowned release scripts: %q != %q", name, got, before[name].ID)
		}
	}

	changedVersion := generate("6.18.40", `"-base"`)
	for _, name := range []string{
		compactGeneratedHeaderFamilyVersion,
		compactGeneratedHeaderFamilyUTSRelease,
		compactGeneratedHeaderFamilyAll,
	} {
		if got := changedVersion[name].ID; got == before[name].ID {
			t.Errorf("%s family ID did not change with declared kernel version %q", name, got)
		}
	}

	changedLocal := generate("6.18.39", `"-debug"`)
	if got := changedLocal[compactGeneratedHeaderFamilyVersion].ID; got != before[compactGeneratedHeaderFamilyVersion].ID {
		t.Errorf("version family ID changed with CONFIG_LOCALVERSION: %q != %q", got, before[compactGeneratedHeaderFamilyVersion].ID)
	}
	for _, name := range []string{
		compactGeneratedHeaderFamilyUTSRelease,
		compactGeneratedHeaderFamilyAll,
	} {
		if got := changedLocal[name].ID; got == before[name].ID {
			t.Errorf("%s family ID did not change with CONFIG_LOCALVERSION %q", name, got)
		}
	}
}

func TestExactClosureTracksOnlyActiveGeneratedIncludes(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "active.c", `
#ifdef CONFIG_STATIC_WRAPPER
#include <asm/unistd.h>
#else
#include <generated/asm-offsets.h>
#endif
`)
	mustWriteSource(t, root, "include/asm-generic/unistd.h", "#define GENERIC_UNISTD 1\n")
	scanner := newConfigSourceScanner(CompactMetadataOptions{
		Schema:     CompactSchemaV013,
		SourceRoot: root,
		Srcarch:    "x86",
	})
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_STATIC_WRAPPER": "y"},
		Written:   map[string]bool{"CONFIG_STATIC_WRAPPER": true},
	}
	closure, err := scanner.closureForSourceConfig("active.c", nil, config)
	if err != nil {
		t.Fatalf("closureForSourceConfig() failed: %v", err)
	}
	if want := []string{"asm/unistd.h"}; !reflect.DeepEqual(closure.generatedIncludes, want) {
		t.Fatalf("active generated includes = %v, want %v", closure.generatedIncludes, want)
	}

	disabled := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_STATIC_WRAPPER": "n"},
		Written:   map[string]bool{"CONFIG_STATIC_WRAPPER": true},
	}
	closure, err = scanner.closureForSourceConfig("active.c", nil, disabled)
	if err != nil {
		t.Fatalf("closureForSourceConfig(disabled) failed: %v", err)
	}
	if want := []string{"generated/asm-offsets.h"}; !reflect.DeepEqual(closure.generatedIncludes, want) {
		t.Fatalf("disabled generated includes = %v, want %v", closure.generatedIncludes, want)
	}
}

func TestGeneratedHeaderFamilyDependenciesFollowActiveIncludes(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "kernel/bounds.c", `
#include <generated/timeconst.h>
#include <generated/bounds.h>
#include <generated/rq-offsets.h>
#ifdef CONFIG_INCLUDE_VERSION
#include <linux/version.h>
#endif
`)
	mustWriteSource(t, root, "arch/x86/kernel/asm-offsets.c", `
#include <generated/bounds.h>
`)
	opts := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
		SourceRoot:            root,
		Srcarch:               "x86",
		CompileEnvironmentABI: "test-abi",
		KernelVersion:         "6.18.0",
	}
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_INCLUDE_VERSION": "n"},
		Written:   map[string]bool{"CONFIG_INCLUDE_VERSION": true},
	}
	families, err := generatedHeaderFamilyFootprints(
		config,
		opts,
		newConfigSourceScanner(opts),
	)
	if err != nil {
		t.Fatalf("generatedHeaderFamilyFootprints() failed: %v", err)
	}
	byName := map[string]compactGeneratedHeaderFamilyFootprint{}
	for _, family := range families {
		byName[family.name] = family
	}
	if want := []string{compactGeneratedHeaderFamilyTimeconst}; !reflect.DeepEqual(
		byName[compactGeneratedHeaderFamilyBounds].dependencies,
		want,
	) {
		t.Fatalf(
			"bounds dependencies = %v, want %v",
			byName[compactGeneratedHeaderFamilyBounds].dependencies,
			want,
		)
	}
	if want := []string{compactGeneratedHeaderFamilyBounds}; !reflect.DeepEqual(
		byName[compactGeneratedHeaderFamilyASMOffsets].dependencies,
		want,
	) {
		t.Fatalf(
			"asm_offsets dependencies = %v, want %v",
			byName[compactGeneratedHeaderFamilyASMOffsets].dependencies,
			want,
		)
	}

	config = &ResolvedConfig{
		Effective: map[string]string{"CONFIG_INCLUDE_VERSION": "y"},
		Written:   map[string]bool{"CONFIG_INCLUDE_VERSION": true},
	}
	families, err = generatedHeaderFamilyFootprints(
		config,
		opts,
		newConfigSourceScanner(opts),
	)
	if err != nil {
		t.Fatalf("generatedHeaderFamilyFootprints(enabled) failed: %v", err)
	}
	for _, family := range families {
		if family.name == compactGeneratedHeaderFamilyBounds {
			if want := []string{
				compactGeneratedHeaderFamilyTimeconst,
				compactGeneratedHeaderFamilyVersion,
			}; !reflect.DeepEqual(family.dependencies, want) {
				t.Fatalf("enabled bounds dependencies = %v, want %v", family.dependencies, want)
			}
		}
	}
}

func TestForcedSourceInputs(t *testing.T) {
	flags := []string{
		"-include", "$(srctree)/include/linux/forced.h",
		"-imacros$(srctree)/include/linux/macros.h",
		"-include$(srctree)/include/linux/joined.h",
		"-include", "$(obj)/generated.h",
	}

	got := forcedSourceInputs(flags, "drivers/foo.c")
	want := []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/forced.h",
		"include/linux/joined.h",
		"include/linux/kconfig.h",
		"include/linux/macros.h",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forcedSourceInputs(C) = %v, want %v", got, want)
	}

	got = forcedSourceInputs(flags, "arch/x86/entry.S")
	want = []string{
		"include/linux/compiler-version.h",
		"include/linux/forced.h",
		"include/linux/joined.h",
		"include/linux/kconfig.h",
		"include/linux/macros.h",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forcedSourceInputs(assembly) = %v, want %v", got, want)
	}
}

func TestConfigSourceScannerV013TracksTransitiveInputDigests(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/root.c", "#include \"nested.h\"\n")
	mustWriteSource(t, root, "drivers/nested.h", "#include \"deep.inc\"\n")
	mustWriteSource(t, root, "drivers/deep.inc", "#define VALUE 1\n")

	scan := func() sourceClosure {
		t.Helper()
		scanner := newConfigSourceScanner(CompactMetadataOptions{
			Schema:     CompactSchemaV013,
			SourceRoot: root,
		})
		closure, err := scanner.exactClosureForSource("drivers/root.c", nil)
		if err != nil {
			t.Fatalf("exactClosureForSource() failed: %v", err)
		}
		return closure
	}
	digests := func(closure sourceClosure) map[string]string {
		t.Helper()
		got := make(map[string]string, len(closure.sourceInputs))
		for _, input := range closure.sourceInputs {
			got[input.Path] = input.Digest
		}
		return got
	}

	beforeClosure := scan()
	var paths []string
	for _, input := range beforeClosure.sourceInputs {
		paths = append(paths, input.Path)
	}
	if want := []string{"drivers/deep.inc", "drivers/nested.h", "drivers/root.c"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("exactClosureForSource().sourceInputs paths = %v, want %v", paths, want)
	}
	before := digests(beforeClosure)
	for _, path := range []string{"drivers/deep.inc", "drivers/nested.h", "drivers/root.c"} {
		if before[path] == "" {
			t.Fatalf("missing digest for %q in %v", path, before)
		}
	}

	mustWriteSource(t, root, "drivers/deep.inc", "#define VALUE 2\n")
	after := digests(scan())
	if after["drivers/deep.inc"] == before["drivers/deep.inc"] {
		t.Fatalf("transitive input digest did not change: %q", after["drivers/deep.inc"])
	}
	for _, path := range []string{"drivers/nested.h", "drivers/root.c"} {
		if after[path] != before[path] {
			t.Errorf("%s digest changed from %q to %q", path, before[path], after[path])
		}
	}
}

func TestConfigSourceScannerCachesFullConfigID(t *testing.T) {
	scanner := newConfigSourceScanner(CompactMetadataOptions{Schema: CompactSchemaV013})
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_ONE": "y"},
		Written:   map[string]bool{"CONFIG_ONE": true},
	}
	first := scanner.configID(config)
	second := scanner.configID(config)
	if first == "" || second != first {
		t.Fatalf("cached config IDs = %q/%q", first, second)
	}
	if got := len(scanner.configIDs); got != 1 {
		t.Fatalf("config ID cache size = %d, want 1", got)
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
