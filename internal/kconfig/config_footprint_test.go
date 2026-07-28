package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		fragment, inputs, footprint, err := generatedHeaderFootprint(config, opts, scanner)
		if err != nil {
			t.Fatalf("generatedHeaderFootprint() failed: %v", err)
		}
		payload := newCompactConfigPayload(fragment)
		group := newCompactHeaderGroup(payload.ID, "//headers:arm64", "arm64", footprint, inputs)
		return fragment, inputs, group.ID
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
		t.Fatalf("compat gettimeofday change did not change arm64 header group ID %q", beforeID)
	}
	beforeID = compatChangedID
	mustWriteSource(t, root, "arch/arm64/tools/cpucaps", "CAP_TWO\n")
	_, _, changedID := generate()
	if changedID == beforeID {
		t.Fatalf("cpucaps change did not change arm64 header group ID %q", beforeID)
	}
	if err := os.Remove(filepath.Join(root, "arch/arm64/include/asm/cfi.h")); err != nil {
		t.Fatalf("Remove(cfi.h) failed: %v", err)
	}
	_, absentInputs, absentID := generate()
	if absentID == changedID {
		t.Fatalf("arm64 cfi.h presence did not change header group ID %q", absentID)
	}
	for _, input := range absentInputs {
		if input.Path == "arch/arm64/include/asm/cfi.h" {
			t.Fatalf("absent cfi.h remained in inputs: %v", absentInputs)
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
