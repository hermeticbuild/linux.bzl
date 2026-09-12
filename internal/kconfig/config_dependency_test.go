package kconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestCompilerProbeProjectionFallsBackOnlyForCrossScopeDependencies(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#if CONFIG_DRIVER\nint selected;\n#endif\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	node.Stage = "host"
	plan.Nodes[0] = node
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
		node.ID: {
			Tool:        "cc",
			Arguments:   []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			Environment: map[string]string{"REALMODE_CFLAGS": "LINUX_BZL_TARGET_SYMBOL"},
		},
	}
	calls := 0
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		calls++
		return "", false, unsupportedCompilerPredefineProjection(&linuxProbeScopeAdoptionError{
			token: "LINUX_BZL_TARGET_SYMBOL", kind: "symbolic value",
		})
	}
	if err := registerActionPlanCompilerProbeProjection(plan, node); err != nil {
		t.Fatalf("cross-scope compiler-probe projection did not fail closed: %v", err)
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !set.Opaque || !strings.Contains(set.Reason, "cannot adopt target-scoped") {
		t.Fatalf("cross-scope compiler dependency set = %#v, callback calls=%d", set, calls)
	}

	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", false, errors.New("malformed probe request")
	}
	if err := registerActionPlanCompilerProbeProjection(plan, node); err == nil || !strings.Contains(err.Error(), "malformed probe request") {
		t.Fatalf("non-scope compiler-probe error was suppressed: %v", err)
	}
}

func TestExtractConfigDependencySymbolsMatchesFixdepTokens(t *testing.T) {
	contents := []byte(strings.Join([]string{
		"CONFIG_ALPHA CONFIG_ALPHA",
		"xCONFIG_PREFIXED _CONFIG_PREFIXED CONFIG_",
		"CONFIG_DRIVER_MODULE CONFIG_MODULE",
		"CONFIG_WITH_123 CONFIG_TRAILING-punctuation",
		"/* CONFIG_COMMENT */",
	}, "\n"))
	want := []string{
		"CONFIG_ALPHA",
		"CONFIG_COMMENT",
		"CONFIG_DRIVER",
		"CONFIG_MODULE",
		"CONFIG_TRAILING",
		"CONFIG_WITH_123",
	}
	if got := ExtractConfigDependencySymbols(contents); !slices.Equal(got, want) {
		t.Fatalf("ExtractConfigDependencySymbols() = %q, want %q", got, want)
	}
}

func TestExtractConfigDependencySymbolsScansPhaseTwoLineSplices(t *testing.T) {
	contents := []byte("#if CONFI\\\nG_SPLIT\n#endif\n#if CONFI\\\r\nG_DRIVER_MODULE\n#endif\n#if xCONFI\\\nG_PREFIXED\n#endif\n")
	want := []string{"CONFIG_DRIVER", "CONFIG_SPLIT"}
	if got := ExtractConfigDependencySymbols(contents); !slices.Equal(got, want) {
		t.Fatalf("line-spliced CONFIG symbols = %q, want %q", got, want)
	}
}

func TestConfigDependencySetCanonicalizesAndUnionsFileClosure(t *testing.T) {
	first, err := CanonicalConfigDependencySet(ConfigDependencySet{
		Symbols:     []string{"CONFIG_B", "CONFIG_A", "CONFIG_A"},
		SourcePaths: []string{"drivers/example/z.h", "drivers/example/a.c", "drivers/example/a.c"},
		ObjectPaths: []string{"include/generated/z.h", "include/generated/a.h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Symbols, []string{"CONFIG_A", "CONFIG_B"}) ||
		!slices.Equal(first.SourcePaths, []string{"drivers/example/a.c", "drivers/example/z.h"}) ||
		!slices.Equal(first.ObjectPaths, []string{"include/generated/a.h", "include/generated/z.h"}) {
		t.Fatalf("canonical dependency set = %#v", first)
	}
	union, err := UnionConfigDependencySets(first, ConfigDependencySet{
		Symbols:     []string{"CONFIG_C"},
		SourcePaths: []string{"include/linux/shared.h"},
		ObjectPaths: []string{"include/generated/a.h", "scripts/generated/tool.h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(union.Symbols, []string{"CONFIG_A", "CONFIG_B", "CONFIG_C"}) ||
		!slices.Equal(union.SourcePaths, []string{"drivers/example/a.c", "drivers/example/z.h", "include/linux/shared.h"}) ||
		!slices.Equal(union.ObjectPaths, []string{"include/generated/a.h", "include/generated/z.h", "scripts/generated/tool.h"}) {
		t.Fatalf("union dependency set = %#v", union)
	}
	if _, err := CanonicalConfigDependencySet(ConfigDependencySet{SourcePaths: []string{"../escape.h"}}); err == nil {
		t.Fatal("non-canonical closure path was accepted")
	}
}

func TestConfigDependencyLiteralIncludesUnionsBranchesAndFailsClosed(t *testing.T) {
	includes, reason := configDependencyLiteralIncludes([]byte(`#if CONFIG_ONE
# include "one.h"
#else
#include <two.h>
#endif
#include \
  "continued.h"
/* leading comment */ #include "after-comment.h"
#/**/include "interstitial-comment.h"
%:include "digraph.h"
#import "imported.h"
`))
	if reason != "" {
		t.Fatal(reason)
	}
	want := []configDependencyLiteralInclude{
		{name: "one.h", quoted: true},
		{name: "two.h"},
		{name: "continued.h", quoted: true},
		{name: "after-comment.h", quoted: true},
		{name: "interstitial-comment.h", quoted: true},
		{name: "digraph.h", quoted: true},
		{name: "imported.h", quoted: true},
	}
	if !slices.Equal(includes, want) {
		t.Fatalf("literal includes = %#v, want %#v", includes, want)
	}
	for _, source := range []string{
		"#include SELECTED_HEADER\n",
		"#include_next <linux/types.h>\n",
		"#include \"unterminated\n",
		"#if __has_include(<generated.h>)\n#endif\n",
		"#if __has_include_next(<generated.h>)\n#endif\n",
	} {
		if _, reason := configDependencyLiteralIncludes([]byte(source)); reason == "" {
			t.Errorf("source %q was not classified opaque", source)
		}
	}
	for _, source := range []string{
		`static const char *names[] = {"TIMER", "DRIVER"};`,
		`/* a prose token ending in DRIVER" is not source syntax */`,
		`static const char *name = "literal R\" text";`,
	} {
		if _, reason := configDependencyLiteralIncludes([]byte(source)); reason != "" {
			t.Errorf("ordinary C source %q was classified opaque: %s", source, reason)
		}
	}
	if _, reason := configDependencyLiteralIncludes([]byte(`auto value = R"tag(raw)tag";`)); !strings.Contains(reason, "raw string") {
		t.Fatalf("C++ raw string reason = %q, want fail-closed classification", reason)
	}
}

func TestConfigDependencyCompilerPredefineAbsenceKeepsReservedIdentifiersUnknown(t *testing.T) {
	state, reason := parseConfigDependencyCompilerPredefines("#define __GNUC__ 15\n")
	if reason != "" {
		t.Fatal(reason)
	}
	if got := state.definition("__GNUC__"); got != configDependencyMacroDefined {
		t.Fatalf("serialized __GNUC__ definition = %d, want defined", got)
	}
	// GCC 15 and Clang 21 both report these as defined to #ifdef while omitting
	// them from `-dM -E -x c /dev/null`. The list is regression evidence, not a
	// completeness allowlist: every absent reserved identifier stays unknown.
	for _, name := range []string{
		"_Pragma", "__FILE__", "__BASE_FILE__", "__LINE__", "__INCLUDE_LEVEL__",
		"__COUNTER__", "__DATE__", "__TIME__", "__TIMESTAMP__", "__has_include",
		"__has_builtin", "__is_target_arch",
	} {
		if got := state.definition(name); got != configDependencyMacroUnknown {
			t.Errorf("undumped dynamic builtin %s definition = %d, want unknown", name, got)
		}
	}
	if got := state.definition("GCC_PLUGINS"); got != configDependencyMacroUndefined {
		t.Fatalf("absent ordinary Kbuild macro definition = %d, want undefined", got)
	}

	includes, reason := configDependencyLiteralIncludesWithMacros([]byte(`#ifdef __FILE__
#include "file-defined.h"
#else
#include "file-undefined.h"
#endif
#ifndef __COUNTER__
#include "counter-undefined.h"
#else
#include "counter-defined.h"
#endif
`), &state)
	if reason != "" {
		t.Fatal(reason)
	}
	want := []configDependencyLiteralInclude{
		{name: "file-defined.h", quoted: true},
		{name: "file-undefined.h", quoted: true},
		{name: "counter-undefined.h", quoted: true},
		{name: "counter-defined.h", quoted: true},
	}
	if !slices.Equal(includes, want) {
		t.Fatalf("undumped dynamic-builtin branches = %#v, want union %#v", includes, want)
	}
}

func TestConfigDependencyAutoconfInSpeculativeBranchUsesInheritedGuard(t *testing.T) {
	const guard = "__GENERATED_AUTOCONF_H__"
	fragment := map[string]string{"CONFIG_BRANCH_VALUE": "y"}
	definitions := newConfigDependencyResolvedAutoconfDefinitions(fragment)
	for _, test := range []struct {
		name       string
		guard      configDependencyMacroDefinition
		wantConfig configDependencyMacroDefinition
	}{
		{
			name:       "defined guard skips inherited header",
			guard:      configDependencyMacroDefined,
			wantConfig: configDependencyMacroUndefined,
		},
		{
			name:       "unknown guard joins skipped and applied paths",
			guard:      configDependencyMacroUnknown,
			wantConfig: configDependencyMacroUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			base.set(guard, test.guard)
			branch := base.branch()
			branch.applyResolvedConfigAutoconf(definitions)
			if got := branch.definition(guard); got != configDependencyMacroDefined {
				t.Errorf("branch autoconf guard = %d, want defined", got)
			}
			if got := branch.definition("CONFIG_BRANCH_VALUE"); got != test.wantConfig {
				t.Errorf("branch autoconf config = %d, want %d", got, test.wantConfig)
			}
			if got := base.definition(guard); got != test.guard {
				t.Errorf("speculative autoconf mutated parent guard = %d, want %d", got, test.guard)
			}
			if got := base.definition("CONFIG_BRANCH_VALUE"); got != configDependencyMacroUndefined {
				t.Errorf("speculative autoconf mutated parent config = %d, want undefined", got)
			}
		})
	}
}

func BenchmarkConfigDependencyAcyclicUnguardedIncludeChain(b *testing.B) {
	const (
		chainDepth = 256
		baseMacros = 8192
	)
	root := b.TempDir()
	write := func(pathname, contents string) {
		filename := filepath.Join(root, filepath.FromSlash(pathname))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	write("drivers/example/driver.c", "#include <linux/chain-000.h>\n")
	for index := 0; index < chainDepth; index++ {
		contents := "CONFIG_CHAIN_LEAF\n"
		if index+1 < chainDepth {
			contents = fmt.Sprintf("#include \"chain-%03d.h\"\n", index+1)
		}
		write(fmt.Sprintf("include/linux/chain-%03d.h", index), contents)
	}

	kb, err := parseKbuildWithOptions(
		strings.NewReader("obj-y += driver.o\n"),
		"scripts/Makefile.build",
		KbuildOptions{
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureTargetEvaluator:  true,
		},
		"",
	)
	if err != nil {
		b.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile(
		"config-dependency-chain-benchmark", "scripts/Makefile.build", "", kb,
	)
	if err != nil {
		b.Fatal(err)
	}
	profile.Directory = "drivers/example"
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		b.Fatal(err)
	}
	source := ActionPlanSource{
		ID: "src-00000001", Namespace: "kernel", Path: "drivers/example/driver.c",
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "compile",
		Tool:   "cc",
		Arguments: []string{
			"-nostdinc", "-I${tree:kernel}/include", "-c", source.Path,
		},
	}
	node := ActionPlanNode{
		ID: "compile-provisional", Stage: "target", Kind: "compile",
		Recipe: "compile-recipe", Tool: "cc", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: source.ID}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/driver.o"}},
	}
	selection := compactKbuildSelectionKey{
		profile: profile.Name, target: "drivers/example/driver.o", stage: "target",
	}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{source},
		Recipes: map[string]ActionRecipe{node.Recipe: recipe},
		Nodes:   []ActionPlanNode{node},
		selectionGraph: &compactKbuildSelectionGraph{
			profiles: map[string]CompactKbuildProfile{profile.Name: profile},
			selections: map[compactKbuildSelectionKey]CompactKbuildSelection{
				selection: {Profile: profile.Name},
			},
			materializedProducers: map[compactKbuildSelectionKey]string{selection: node.ID},
		},
		metadata: &CompactMetadata{},
	}
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	var predefines strings.Builder
	for index := 0; index < baseMacros; index++ {
		fmt.Fprintf(&predefines, "#define BENCHMARK_MACRO_%05d 1\n", index)
	}
	predefineText := predefines.String()
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return predefineText, true, nil
	}

	b.ReportAllocs()
	b.ReportMetric(chainDepth, "headers")
	b.ReportMetric(baseMacros, "base_macros")
	b.ResetTimer()
	var set ConfigDependencySet
	for range b.N {
		set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_CHAIN_LEAF"}) ||
		len(set.SourcePaths) != chainDepth+1 {
		b.Fatalf("acyclic unguarded include-chain dependency set = %#v", set)
	}
}

func TestConfigDependencyCompilerMacroReplacementSymbols(t *testing.T) {
	state, reason := parseConfigDependencyCompilerPredefines(`#define ALIAS CONFIG_FROM_BUILTIN
#define FUNCTION(x) ((x) + CONFIG_FUNCTION_MODULE)
#define CONFIG_FIXED 1
`)
	if reason != "" {
		t.Fatal(reason)
	}
	if got, want := slices.Sorted(maps.Keys(state.symbols)), []string{"CONFIG_FROM_BUILTIN", "CONFIG_FUNCTION"}; !slices.Equal(got, want) {
		t.Fatalf("predefine replacement symbols = %#v, want %#v", got, want)
	}
	got, reason := configDependencyCompilerDefineReplacementSymbols([]string{
		"-DALIAS=CONFIG_JOINED",
		"-D", "FUNCTION(x)=CONFIG_SEPARATED_MODULE",
		"-DCONFIG_FIXED=1",
	})
	if reason != "" {
		t.Fatal(reason)
	}
	if want := []string{"CONFIG_JOINED", "CONFIG_SEPARATED"}; !slices.Equal(got, want) {
		t.Fatalf("compiler -D replacement symbols = %#v, want %#v", got, want)
	}
	for _, replacement := range []string{
		"CONFIG_ ## DYNAMIC", "CONFIG ## _DYNAMIC",
		"CONFIG_ %:%: DYNAMIC", "CONFIG %:%: _DYNAMIC",
	} {
		if symbols, reason := configDependencyMacroReplacementSymbols(replacement); reason != "" || len(symbols) != 0 {
			t.Errorf("token-paste replacement %q = symbols %#v reason %q, want deferred reachability", replacement, symbols, reason)
		}
	}
	if _, reason := configDependencyMacroReplacementSymbols("CONFIG_"); reason == "" {
		t.Error("incomplete non-pasting CONFIG_ replacement was accepted")
	}
}

func TestConfigDependencyMacroTokenPasteRisk(t *testing.T) {
	for _, test := range []struct {
		name         string
		replacement  string
		parameters   map[string]bool
		wantUnsafe   bool
		wantPrefixes []string
	}{
		{
			name: "kentry fixed prefix", replacement: "__kentry_ ## sym",
			parameters: map[string]bool{"sym": true}, wantPrefixes: []string{"__kentry_"},
		},
		{
			name: "gen fixed prefix", replacement: "__EXPORT_THUNK(__x86_indirect_thunk_ ## reg)",
			parameters: map[string]bool{"reg": true}, wantPrefixes: []string{"__x86_indirect_thunk_"},
		},
		{
			name: "multi token chain", replacement: "safe_ ## first ## second",
			parameters: map[string]bool{"first": true, "second": true}, wantPrefixes: []string{"safe_"},
		},
		{
			name: "two safe chains", replacement: "first_ ## x + second_ %:%: y",
			parameters: map[string]bool{"x": true, "y": true}, wantPrefixes: []string{"first_", "second_"},
		},
		{
			name: "parameter prefix", replacement: "left ## right",
			parameters: map[string]bool{"left": true, "right": true}, wantUnsafe: true,
		},
		{
			name: "complete config prefix", replacement: "CONFIG_ ## value",
			parameters: map[string]bool{"value": true}, wantUnsafe: true,
		},
		{
			name: "partial config prefix", replacement: "CON ## value",
			parameters: map[string]bool{"value": true}, wantUnsafe: true,
		},
		{
			name: "config identifier prefix", replacement: "CONFIG_FEATURE ## value",
			parameters: map[string]bool{"value": true}, wantUnsafe: true,
		},
		{
			name: "fixed suffix only", replacement: "value ## _feature",
			parameters: map[string]bool{"value": true}, wantUnsafe: true,
		},
		{
			name: "one unsafe chain", replacement: "safe_ ## value + value ## _feature",
			parameters: map[string]bool{"value": true}, wantUnsafe: true,
		},
		{name: "missing operand", replacement: "safe_ ##", wantUnsafe: true},
		{name: "unterminated literal", replacement: `safe_ ## "value`, wantUnsafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			unsafe, prefixes := configDependencyMacroTokenPasteRisk(test.replacement, test.parameters)
			if unsafe != test.wantUnsafe || !slices.Equal(prefixes, test.wantPrefixes) {
				t.Fatalf("paste risk for %q = unsafe=%t prefixes=%q, want unsafe=%t prefixes=%q",
					test.replacement, unsafe, prefixes, test.wantUnsafe, test.wantPrefixes)
			}
		})
	}
}

func TestConfigDependencySortedPrefixRangeMatchesLinearFilter(t *testing.T) {
	values := []string{
		"A", "GEN", "GENERIC", "GEN_ALPHA", "GEN_BETA", "KENTRY",
		"KENTRY_ALIAS", "KENTRY_ZED", "SAFE", "_PRIVATE", "__kentry_alpha",
		"__kentry_beta", "__other",
	}
	slices.Sort(values)
	for _, prefix := range []string{
		"", "A", "B", "GEN", "GEN_", "GEN_ALPHA", "GEN_ALPHA_MORE",
		"KENTRY_", "SAFE", "Z", "_", "__kentry_", "__other", "___",
	} {
		start, end := configDependencySortedPrefixRange(values, prefix)
		got := slices.Clone(values[start:end])
		want := []string{}
		for _, value := range values {
			if strings.HasPrefix(value, prefix) {
				want = append(want, value)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("prefix %q range [%d:%d] = %q, want linear filter %q", prefix, start, end, got, want)
		}
	}
}

func BenchmarkConfigDependencyMacroDefinitionPrefixLookup(b *testing.B) {
	const (
		definitionCount = 16 * 1024
		prefixCount     = 64
	)
	definitions := make([]string, definitionCount)
	for index := range definitions {
		definitions[index] = fmt.Sprintf("SAFE_%03d_%05d", index%128, index)
	}
	slices.Sort(definitions)
	prefixes := make([]string, prefixCount)
	for index := range prefixes {
		prefixes[index] = fmt.Sprintf("SAFE_%03d_", index*2)
	}
	linearCount := func(prefix string) int {
		count := 0
		for _, definition := range definitions {
			if strings.HasPrefix(definition, prefix) {
				count++
			}
		}
		return count
	}
	for _, benchmark := range []struct {
		name  string
		count func(string) int
	}{
		{
			name: "sorted-prefix-range",
			count: func(prefix string) int {
				start, end := configDependencySortedPrefixRange(definitions, prefix)
				return end - start
			},
		},
		{name: "linear-reference", count: linearCount},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(definitionCount, "definitions/op")
			b.ReportMetric(prefixCount, "prefixes/op")
			total := 0
			for range b.N {
				for _, prefix := range prefixes {
					total += benchmark.count(prefix)
				}
			}
			if total == 0 {
				b.Fatal("macro prefix benchmark matched no definitions")
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsFixedNonConfigPastePrefixes(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{
			name: "linux kentry",
			source: `#ifndef KENTRY
#define KENTRY(sym) extern typeof(sym) sym; static const unsigned long __kentry_ ## sym = (unsigned long)&sym
#endif
KENTRY(example_entry);
#if CONFIG_FEATURE
int selected;
#endif
`,
		},
		{
			name: "x86 gen export thunk",
			source: `#define __EXPORT_THUNK(sym) __export_symbol_ ## sym
#define GEN(reg) __EXPORT_THUNK(__x86_indirect_thunk_ ## reg)
GEN(rax)
GEN(rcx)
#if CONFIG_FEATURE
int selected;
#endif
`,
		},
		{
			name: "nonoverlapping config spelling",
			source: `#define BUILD(name) CONFIGURED_ ## name
int selected = BUILD(FEATURE);
#if CONFIG_FEATURE
int enabled;
#endif
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": test.source,
			}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_FEATURE"}) ||
				!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) {
				t.Fatalf("fixed non-CONFIG paste dependency set = %#v", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsDynamicSourceMacroConstruction(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string]string
	}{
		{
			name: "translation unit",
			files: map[string]string{
				"drivers/example/driver.c": "#define CFG(x) CONFIG_ ## x\nint selected = CFG(FEATURE);\n",
			},
		},
		{
			name: "reachable header",
			files: map[string]string{
				"drivers/example/driver.c":  "#include \"dynamic.h\"\n",
				"drivers/example/dynamic.h": "#define CFG(x) CONFIG_ ## x\nint selected = CFG(FEATURE);\n",
			},
		},
		{
			name: "generic paste in translation unit",
			files: map[string]string{
				"drivers/example/driver.c": "#define CAT(a, b) a ## b\nint selected = CAT(CONFIG_, FEATURE);\n",
			},
		},
		{
			name: "generic paste from reachable header",
			files: map[string]string{
				"drivers/example/driver.c": "#include \"paste.h\"\nint selected = CAT(CONFIG_, FEATURE);\n",
				"drivers/example/paste.h":  "#define CAT(a, b) a ## b\n",
			},
		},
		{
			name: "split config prefix",
			files: map[string]string{
				"drivers/example/driver.c": "#define CAT(a, b) a ## b\nint selected = CAT(CON, FIG_FEATURE);\n",
			},
		},
		{
			name: "split config suffix",
			files: map[string]string{
				"drivers/example/driver.c": "#define CAT(a, b) a ## b\nint selected = CAT(CONFIG_F, EATURE);\n",
			},
		},
		{
			name: "literal partial config prefix",
			files: map[string]string{
				"drivers/example/driver.c": "#define BUILD(x) CON ## x\nint selected = BUILD(FIG_FEATURE);\n",
			},
		},
		{
			name: "parameter with fixed config suffix",
			files: map[string]string{
				"drivers/example/driver.c": "#define BUILD(x) x ## _FEATURE\nint selected = BUILD(CONFIG);\n",
			},
		},
		{
			name: "pasted macro name rescans to generic paste",
			files: map[string]string{
				"drivers/example/driver.c": `#define KENTRY(sym) __kentry_ ## sym
#define __kentry_dispatch(a, b) a ## b
int selected = KENTRY(dispatch)(CONFIG_, FEATURE);
`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(
				t, test.files, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil,
			)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "dynamically constructs a CONFIG_*") {
				t.Fatalf("dynamic source macro dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsOrdinaryTokenPaste(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#define CAT(a, b) a ## b
#define MODULE_NAME(x) CAT(x, _module)
#if CONFIG_FEATURE
int selected = MODULE_NAME(driver);
#endif
`,
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "token pasting") {
		t.Fatalf("ordinary token-paste dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesIgnoresUnreachableTokenPasteDefinitions(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#define LOCAL_UNUSED(a, b) a ## b
#define LOCAL_ALIAS(x) LOCAL_UNUSED(x, suffix)
#define PARAMETER_ONLY(LOCAL_UNUSED) LOCAL_UNUSED
#define HASH_LITERAL "##"
/* LOCAL_UNUSED(CONFIG_, COMMENT) */
static const char *literal = "LOCAL_UNUSED(CONFIG_, STRING)";
static const char *hash = HASH_LITERAL;
int passthrough = PARAMETER_ONLY(1);
#if CONFIG_FEATURE
int selected;
#endif
`,
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return `#define __INTMAX_C(c) c ## L
#define __UINTMAX_C(c) c ## UL
#define __INT64_C(c) c ## L
`, true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_FEATURE"}) ||
		!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) {
		t.Fatalf("unused token-paste definitions dependency set = %#v, want precise CONFIG_FEATURE closure", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsReachableTokenPasteAliases(t *testing.T) {
	for _, test := range []struct {
		name       string
		source     string
		arguments  []string
		predefines string
	}{
		{
			name: "source alias",
			source: `#define CAT(a, b) a ## b
#define SOURCE_ALIAS CAT
int selected = SOURCE_ALIAS(CONFIG_, FEATURE);
`,
			arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"},
		},
		{
			name:      "command-line alias",
			source:    "int selected = COMMAND_ALIAS(CONFIG_, FEATURE);\n",
			arguments: []string{"-nostdinc", "-DCAT(a,b)=a##b", "-DCOMMAND_ALIAS=CAT", "-c", "drivers/example/driver.c"},
		},
		{
			name:       "compiler-predefine alias",
			source:     "int selected = PREDEFINED_ALIAS(CONFIG_, FEATURE);\n",
			arguments:  []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			predefines: "#define CAT(a,b) a##b\n#define PREDEFINED_ALIAS CAT\n",
		},
		{
			name:       "cross-origin alias chain",
			source:     "#define SOURCE_ALIAS COMMAND_ALIAS\nint selected = SOURCE_ALIAS(CONFIG_, FEATURE);\n",
			arguments:  []string{"-nostdinc", "-DCOMMAND_ALIAS=PREDEFINED_CAT", "-c", "drivers/example/driver.c"},
			predefines: "#define PREDEFINED_CAT(a,b) a##b\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": test.source,
			}, test.arguments, nil)
			if test.predefines != "" {
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return test.predefines, true, nil
				}
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "reachable token pasting macro") {
				t.Fatalf("reachable token-paste alias dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesCompilerDefinedGenericConfigPasteRequiresCompleteState(t *testing.T) {
	for _, test := range []struct {
		name       string
		arguments  []string
		predefines string
	}{
		{
			name:      "command line",
			arguments: []string{"-nostdinc", "-DCAT(a,b)=a##b", "-c", "drivers/example/driver.c"},
		},
		{
			name:       "compiler predefine",
			arguments:  []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			predefines: "#define CAT(a,b) a##b\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "int selected = CAT(CONFIG_, FEATURE);\n",
			}, test.arguments, nil)
			if test.predefines != "" {
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return test.predefines, true, nil
				}
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if test.predefines != "" {
				if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_FEATURE"}) {
					t.Fatalf("compiler-probed paste lost complete call proof: %#v", set)
				}
			} else if !set.Opaque || !strings.Contains(set.Reason, "token pasting") {
				t.Fatalf("compiler-defined config-paste dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRetainsLiteralSourceMacroAliases(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":  "#define SOURCE_ALIAS CONFIG_FROM_SOURCE\n#include \"literal.h\"\n",
		"drivers/example/literal.h": "#define HEADER_ALIAS CONFIG_FROM_HEADER_MODULE\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CONFIG_FROM_HEADER", "CONFIG_FROM_SOURCE"}
	if set.Opaque || !slices.Equal(set.Symbols, want) {
		t.Fatalf("literal source macro dependency set = %#v, want symbols %q", set, want)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRetainsCompilerMacroAliases(t *testing.T) {
	for _, test := range []struct {
		name       string
		arguments  []string
		predefines string
		want       string
	}{
		{
			name:       "command line replacement before probe canonicalization",
			arguments:  []string{"-nostdinc", "-DALIAS=CONFIG_FROM_ARGUMENT", "-c", "drivers/example/driver.c"},
			predefines: "#define ALIAS 1\n",
			want:       "CONFIG_FROM_ARGUMENT",
		},
		{
			name:       "compiler builtin replacement without forced header",
			arguments:  []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			predefines: "#define ALIAS CONFIG_FROM_BUILTIN\n",
			want:       "CONFIG_FROM_BUILTIN",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#if ALIAS\nint selected;\n#endif\n",
			}, test.arguments, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			called := false
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				called = true
				return test.predefines, true, nil
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !called || set.Opaque || !slices.Equal(set.Symbols, []string{test.want}) {
				t.Fatalf("macro-alias dependency set = %#v, callback=%t", set, called)
			}
		})
	}
}

func TestConfigDependencyFirstForcedHeaderHandlesModernConditionalsAndFailsClosedOnMacroStacks(t *testing.T) {
	state, reason := parseConfigDependencyCompilerPredefines("#define COMPILER_SELECTED 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	inclusions, reason := configDependencyLiteralIncludesWithMacros([]byte(`#if 0
#include "inactive.h"
#elifdef COMPILER_SELECTED
#include "selected.h"
#else
#include "fallback.h"
#endif
#if 0
#elifndef ABSENT_COMPILER_MACRO
#include "absent.h"
#endif
`), &state)
	if reason != "" {
		t.Fatal(reason)
	}
	want := []configDependencyLiteralInclude{
		{name: "selected.h", quoted: true},
		{name: "absent.h", quoted: true},
	}
	if !slices.Equal(inclusions, want) {
		t.Fatalf("#elifdef/#elifndef inclusions = %#v, want %#v", inclusions, want)
	}

	for _, source := range []string{
		"#pragma push_macro(\"COMPILER_SELECTED\")\n#undef COMPILER_SELECTED\n#pragma pop_macro(\"COMPILER_SELECTED\")\n",
		"_Pragma(\"push_macro(\\\"COMPILER_SELECTED\\\")\")\n",
	} {
		state, _ := parseConfigDependencyCompilerPredefines("#define COMPILER_SELECTED 1\n")
		if _, reason := configDependencyLiteralIncludesWithMacros([]byte(source), &state); !strings.Contains(reason, "macro-stack") {
			t.Fatalf("macro-stack source reason = %q, want fail-closed classification", reason)
		}
	}
}

func TestMergeConfigDependencyConditionalParsedUsesStableIncrementalUnions(t *testing.T) {
	first := configDependencyLiteralInclude{name: "first.h", quoted: true}
	second := configDependencyLiteralInclude{name: "second.h"}
	left := configDependencyParsedFile{
		macroUndefinitions: []string{"ALPHA", "GAMMA"},
		includes:           []configDependencyLiteralInclude{first},
	}
	right := configDependencyParsedFile{
		macroUndefinitions: []string{"BETA", "GAMMA"},
		includes:           []configDependencyLiteralInclude{first, second},
	}
	merged := mergeConfigDependencyConditionalParsed(left, right)
	if !slices.Equal(merged.macroUndefinitions, []string{"ALPHA", "BETA", "GAMMA"}) {
		t.Fatalf("macro undefinition union = %#v", merged.macroUndefinitions)
	}
	if !slices.Equal(merged.includes, []configDependencyLiteralInclude{first, second}) {
		t.Fatalf("literal include union = %#v", merged.includes)
	}

	// Reinterpreting the same physical file is the hot path. Once every static
	// fact has been seen, merging a subset must preserve the existing backing
	// storage instead of allocating and sorting it again.
	definitions := merged.macroUndefinitions
	again := mergeConfigDependencyConditionalParsed(merged, right)
	if &again.macroUndefinitions[0] != &definitions[0] {
		t.Fatal("repeated subset merge replaced the canonical sorted summary")
	}
	if !slices.Equal(again.includes, merged.includes) {
		t.Fatalf("repeated literal includes = %#v", again.includes)
	}
}

func BenchmarkMergeConfigDependencyConditionalParsedRepeatedPhysicalFile(b *testing.B) {
	names := make([]string, 512)
	for index := range names {
		names[index] = fmt.Sprintf("GUARD_%04d", index)
	}
	includes := []configDependencyLiteralInclude{
		{name: "linux/compiler_types.h"},
		{name: "linux/compiler_attributes.h"},
	}
	parsed := configDependencyParsedFile{
		macroUndefinitions: names,
		includes:           includes,
	}
	merged := mergeConfigDependencyConditionalParsed(configDependencyParsedFile{}, parsed)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		merged = mergeConfigDependencyConditionalParsed(merged, parsed)
	}
}

func TestConfigDependencyEmbedFailsClosed(t *testing.T) {
	for _, source := range []string{
		"#embed \"payload.bin\"\n",
		"#if __has_embed(\"payload.bin\")\n#endif\n",
	} {
		if _, reason := configDependencyLiteralIncludes([]byte(source)); reason == "" {
			t.Fatalf("embed source %q was not classified opaque", source)
		}
	}
	state := configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	if _, reason := configDependencyLiteralIncludesWithMacros([]byte("#embed \"payload.bin\"\n"), &state); reason == "" {
		t.Fatal("active first-header #embed was not classified opaque")
	}
}

func TestConfigDependencyQueueDistinguishesSourceAndObjectAliases(t *testing.T) {
	physical := filepath.Join(t.TempDir(), "shared.h")
	mustWriteSource(t, filepath.Dir(physical), filepath.Base(physical), "CONFIG_SHARED\n")
	scanner := configDependencyClosureScanner{
		sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
		queued: map[string]bool{}, queuedPhysical: map[string]bool{},
	}
	scanner.queue(configDependencyScanFile{logical: "vendor/shared.h", physical: physical, source: true})
	scanner.queue(configDependencyScanFile{logical: "vendor/shared.h", physical: physical})
	if scanner.opaqueReason != "" || len(scanner.pending) != 2 ||
		!scanner.sourcePaths["vendor/shared.h"] || !scanner.objectPaths["vendor/shared.h"] {
		t.Fatalf("source/object physical alias queue = %#v", scanner)
	}
}

func configDependencyCompilePlanForTest(
	t *testing.T,
	files map[string]string,
	arguments []string,
	config map[string]string,
) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	root := t.TempDir()
	for pathname, contents := range files {
		mustWriteSource(t, root, pathname, contents)
	}
	profile := mustCompactKbuildProfileForTest(
		t, "config-dependency", "scripts/Makefile.build", "drivers/example", "obj-y += driver.o\n", nil,
	)
	if profile.evaluator == nil || profile.evaluator.template == nil {
		t.Fatal("test profile has no evaluator")
	}
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	source := ActionPlanSource{ID: "src-00000001", Namespace: "kernel", Path: "drivers/example/driver.c"}
	recipe := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc", Arguments: arguments}
	node := ActionPlanNode{
		ID: "compile-provisional", Stage: "target", Kind: "compile", Recipe: "compile-recipe", Tool: "cc", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: source.ID}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/driver.o"}},
	}
	selection := compactKbuildSelectionKey{profile: profile.Name, target: "drivers/example/driver.o", stage: "target"}
	graph := &compactKbuildSelectionGraph{
		profiles:              map[string]CompactKbuildProfile{profile.Name: profile},
		selections:            map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
		materializedProducers: map[compactKbuildSelectionKey]string{selection: node.ID},
	}
	metadata := &CompactMetadata{configFragment: maps.Clone(config)}
	plan := &ActionPlan{
		Sources: []ActionPlanSource{source}, Recipes: map[string]ActionRecipe{node.Recipe: recipe}, Nodes: []ActionPlanNode{node},
		selectionGraph: graph, metadata: metadata,
	}
	return plan, node
}

func configDependencySetCompilerContractForTest(
	plan *ActionPlan,
	node ActionPlanNode,
	contract CompactKbuildActionContract,
) {
	ref := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: node.Tool}
	plan.metadata.actionRoles = []KbuildActionRoleRef{ref}
	plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{ref: contract}
}

func TestAnalyzeActionPlanNodeConfigDependenciesPrunesFirstForcedHeaderWithCompilerPredefines(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/compiler-version.h": `#ifdef COMPILER_SELECTED
#define COMPILER_BRANCH
#else
#include INACTIVE_COMPILER_HEADER
#endif
#ifdef DISABLED_BY_ARGUMENT
#include INACTIVE_ARGUMENT_HEADER
#else
#define ARGUMENT_BRANCH
#endif
#if defined(COMPILER_BRANCH)
#ifdef ARGUMENT_BRANCH
#include "compiler-selected.h"
#endif
#endif
`,
		"include/linux/compiler-selected.h": "CONFIG_COMPILER_SELECTED\n",
	}, []string{
		"-nostdinc", "--target=x86_64-linux-gnu", "-std=gnu11",
		"-DCOMPILER_SELECTED=0", "-U", "DISABLED_BY_ARGUMENT",
		"-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	recipe := plan.Recipes[node.Recipe]
	recipe.Environment = map[string]string{
		"SOURCE_MODE":       "source",
		"OBJECT_MODE":       "source-shadowed",
		"KBUILD_CFLAGS":     "${tree:malformed",
		"LANG":              "de_DE.UTF-8",
		"SOURCE_DATE_EPOCH": "${input:missing}",
	}
	plan.Recipes[node.Recipe] = recipe
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{
		Environment: map[string]string{"OBJECT_MODE": "64"},
	})
	called := false
	plan.metadata.compilerPredefines = func(scope, role, language string, arguments, translationUnits []string, environment map[string]string) (string, bool, error) {
		called = true
		if scope != "target" || role != "cc" || language != "c" {
			t.Fatalf("compiler-predefine request scope/role/language = %q/%q/%q", scope, role, language)
		}
		want := []string{
			"-nostdinc", "--target=x86_64-linux-gnu", "-std=gnu11",
			"-DCOMPILER_SELECTED=0", "-U", "DISABLED_BY_ARGUMENT",
		}
		if !slices.Equal(arguments, want) {
			t.Fatalf("normalized compiler-predefine arguments = %#v, want %#v", arguments, want)
		}
		if len(translationUnits) != 0 {
			t.Fatalf("normalized compiler-predefine translation units = %#v, want none after exact static removal", translationUnits)
		}
		if !maps.Equal(environment, map[string]string{"SOURCE_MODE": "source"}) {
			t.Fatalf("compiler-predefine environment = %#v, want exact unknown wrapper state without the configured override or defined-name-irrelevant values", environment)
		}
		// The compiler probe executes the retained -D/-U names in their original
		// order. Its name-only consumer may canonicalize object-like -D bodies, so
		// the dump still contains their exact final definedness state.
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("compiler-predefine callback was not invoked")
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_COMPILER_SELECTED", "CONFIG_DRIVER"}) {
		t.Fatalf("compiler-pruned dependency set = %#v", set)
	}
	wantSources := []string{
		"drivers/example/driver.c",
		"include/linux/compiler-selected.h",
		"include/linux/compiler-version.h",
	}
	if !slices.Equal(set.SourcePaths, wantSources) ||
		!slices.Equal(set.ObjectPaths, []string{"include/generated/autoconf.h"}) {
		t.Fatalf("compiler-pruned file closure = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesDoesNotCacheCompilerConditionalParseByPath(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/compiler-version.h": `#ifdef SELECT_FIRST
#include "first.h"
#else
#include "second.h"
#endif
`,
		"include/linux/first.h":  "CONFIG_FIRST\n",
		"include/linux/second.h": "CONFIG_SECOND\n",
	}, []string{
		"-nostdinc", "-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	context := newConfigDependencyAnalysisContext(plan)
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define SELECT_FIRST 1\n", true, nil
	}
	first, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	second, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	if first.Opaque || !slices.Equal(first.Symbols, []string{"CONFIG_DRIVER", "CONFIG_FIRST"}) {
		t.Fatalf("first macro-state closure = %#v", first)
	}
	if second.Opaque || !slices.Equal(second.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SECOND"}) {
		t.Fatalf("second macro-state closure after shared parsed cache = %#v", second)
	}
	if got := len(context.conditionalSyntax); got != 4 {
		t.Fatalf("cached conditional syntax entries = %d, want translation unit, compiler header, and two selected headers", got)
	}
	if got := len(context.compilerPredefines.entries); got != 2 {
		t.Fatalf("cached compiler-predefine parses = %d, want one per exact dump", got)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesBranchesFromImmutableCachedCompilerPredefineState(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/forced.h": `#ifndef __FORCED_STATE_GUARD_H
#define __FORCED_STATE_GUARD_H
CONFIG_FORCED
#endif
`,
	}, []string{
		"-nostdinc", "-include", "${tree:kernel}/include/linux/forced.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	context := newConfigDependencyAnalysisContext(plan)
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	want := []string{"CONFIG_DRIVER", "CONFIG_FORCED"}
	for iteration := 0; iteration < 2; iteration++ {
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil {
			t.Fatal(err)
		}
		if set.Opaque || !slices.Equal(set.Symbols, want) {
			t.Fatalf("iteration %d dependency set = %#v, want isolated cached predefine state", iteration, set)
		}
	}
	if got := len(context.conditionalSyntax); got != 2 {
		t.Fatalf("cached conditional syntax entries = %d, want one shared forced header and translation unit", got)
	}
	if got := len(context.compilerPredefines.entries); got != 1 {
		t.Fatalf("cached compiler-predefine parses = %d, want one exact dump", got)
	}
	cached, _ := context.compilerPredefines.get(configDependencyCompilerPredefineParseKey("#define COMPILER_SELECTED 1\n", ""))
	if cached.state.parent != nil || cached.state.definition("COMPILER_SELECTED") != configDependencyMacroDefined ||
		cached.state.definition("__FORCED_STATE_GUARD_H") != configDependencyMacroUnknown {
		t.Fatalf("cached compiler-predefine root was mutated by a translation unit: %#v", cached.state)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesReusesExactForcedHeaderPrefix(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#ifdef CONFIG_DRIVER
#include <linux/driver-enabled.h>
#endif
#define AFTER_REENTRY 1
#undef __FORCED_PREFIX_H
#include <linux/forced.h>
CONFIG_TRANSLATION_UNIT
`,
		"include/linux/forced.h": `#ifndef __FORCED_PREFIX_H
#define __FORCED_PREFIX_H
#ifdef COMPILER_SELECTED
#include <linux/compiler-selected.h>
#endif
#ifdef AFTER_REENTRY
#include <linux/reentered.h>
#endif
#endif
`,
		"include/linux/compiler-selected.h": "CONFIG_COMPILER_SELECTED\n",
		"include/linux/driver-enabled.h":    "CONFIG_DRIVER_ENABLED\n",
		"include/linux/reentered.h":         "CONFIG_REENTERED\n",
	}, []string{
		"-nostdinc", "-I", "${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/forced.h",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-c", "drivers/example/driver.c",
	}, map[string]string{"CONFIG_DRIVER": "y"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	context := newConfigDependencyAnalysisContext(plan)
	first, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	second, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	freshContext := newConfigDependencyAnalysisContext(plan)
	freshContext.forcedHeaders = nil
	fresh, err := analyzeActionPlanNodeConfigDependencies(plan, node, freshContext)
	if err != nil {
		t.Fatal(err)
	}
	setsEqual := func(left, right ConfigDependencySet) bool {
		return left.Opaque == right.Opaque && left.Reason == right.Reason &&
			slices.Equal(left.Symbols, right.Symbols) &&
			slices.Equal(left.SourcePaths, right.SourcePaths) &&
			slices.Equal(left.ObjectPaths, right.ObjectPaths)
	}
	if !setsEqual(first, second) || !setsEqual(first, fresh) {
		t.Fatalf("forced-header cache changed dependency closure: first=%#v second=%#v fresh=%#v", first, second, fresh)
	}
	if first.Opaque || !slices.Equal(first.Symbols, []string{
		"CONFIG_COMPILER_SELECTED", "CONFIG_DRIVER", "CONFIG_DRIVER_ENABLED", "CONFIG_REENTERED", "CONFIG_TRANSLATION_UNIT",
	}) {
		t.Fatalf("forced-header dependency set = %#v", first)
	}
	if cache := context.forcedHeaders; cache == nil || cache.hits != 1 || cache.misses != 1 || len(cache.entries) != 1 {
		t.Fatalf("forced-header cache accounting = %#v, want one miss followed by one hit", cache)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesReusesForcedPrefixAcrossCompilerPredefines(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#ifdef TU_ONLY
#include <linux/tu-only.h>
#else
#include <linux/tu-fallback.h>
#endif
`,
		"include/linux/forced.h": `#ifndef FORCED_CACHE_H
#define FORCED_CACHE_H
#ifdef PREFIX_SWITCH
#include <linux/prefix-on.h>
#else
#include <linux/prefix-off.h>
#endif
#endif
`,
		"include/linux/prefix-on.h":   "CONFIG_PREFIX_ON\n",
		"include/linux/prefix-off.h":  "CONFIG_PREFIX_OFF\n",
		"include/linux/tu-only.h":     "CONFIG_TU_ONLY\n",
		"include/linux/tu-fallback.h": "CONFIG_TU_FALLBACK\n",
	}, []string{
		"-nostdinc", "-I", "${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/forced.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	predefines := ""
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return predefines, true, nil
	}
	context := newConfigDependencyAnalysisContext(plan)
	run := func(contents, present, absent string) ConfigDependencySet {
		t.Helper()
		predefines = contents
		// The real actions have distinct probe requests because their argv/TU
		// bindings differ. Reinitialize only that request memo so this compact
		// fixture can exercise the same distinct predefine results on one node.
		context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil {
			t.Fatal(err)
		}
		if set.Opaque || !slices.Contains(set.SourcePaths, present) || slices.Contains(set.SourcePaths, absent) {
			t.Fatalf("predefines %q dependency set = %#v; want %q and not %q", contents, set, present, absent)
		}
		return set
	}

	// The second root has the same modeled defined-name set but different
	// KBUILD replacement values, so it takes the O(1) post-snapshot path.
	firstSet := run("#define PREFIX_SWITCH 1\n#define TU_ONLY 1\n#define KBUILD_BASENAME first\n#define KBUILD_MODNAME CONFIG_PREDEFINE_FIRST\n",
		"include/linux/tu-only.h", "include/linux/tu-fallback.h")
	secondSet := run("#define PREFIX_SWITCH 2\n#define TU_ONLY 2\n#define KBUILD_BASENAME second\n#define KBUILD_MODNAME CONFIG_PREDEFINE_SECOND\n",
		"include/linux/tu-only.h", "include/linux/tu-fallback.h")
	if !slices.Contains(firstSet.Symbols, "CONFIG_PREDEFINE_FIRST") ||
		!slices.Contains(secondSet.Symbols, "CONFIG_PREDEFINE_SECOND") ||
		slices.Contains(secondSet.Symbols, "CONFIG_PREDEFINE_FIRST") {
		t.Fatalf("current predefine replacement symbols were frozen by cache reuse: first=%#v second=%#v", firstSet, secondSet)
	}
	// TU_ONLY is not consumed by the forced prefix. Its absence must survive a
	// sparse transform replay rather than being frozen from the cached root.
	run("#define PREFIX_SWITCH 3\n#define KBUILD_BASENAME third\n#define KBUILD_MODNAME third\n",
		"include/linux/tu-fallback.h", "include/linux/tu-only.h")
	// PREFIX_SWITCH is consumed by the prefix, so changing its cell creates a
	// second specialization. A replacement-only change then reuses that one.
	run("#define KBUILD_BASENAME fourth\n#define KBUILD_MODNAME fourth\n",
		"include/linux/prefix-off.h", "include/linux/prefix-on.h")
	run("#define KBUILD_BASENAME fifth\n#define KBUILD_MODNAME fifth\n",
		"include/linux/prefix-off.h", "include/linux/prefix-on.h")

	cache := context.forcedHeaders
	if cache == nil || cache.hits != 3 || cache.misses != 2 || len(cache.entries) != 1 {
		t.Fatalf("cross-predefine forced-header cache = %#v, want three hits/two misses/one structural key", cache)
	}
	for _, entries := range cache.entries {
		if len(entries) != 2 {
			t.Fatalf("forced-header specializations = %d, want 2", len(entries))
		}
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesForcedHeaderCacheUsesCurrentCompilerReplacementGraph(t *testing.T) {
	tests := []struct {
		name            string
		forcedBody      string
		firstInert      string
		dangerous       string
		secondInert     string
		dangerousReason string
	}{
		{
			name:       "reachable _Pragma",
			forcedBody: "CACHE_DISPATCH\nCONFIG_FORCED\n",
			firstInert: `#define CACHE_DISPATCH CACHE_SAFE_ONE
#define CACHE_SAFE_ONE 1
#define CACHE_SAFE_TWO 2
#define CACHE_DANGER _Pragma("once")
`,
			dangerous: `#define CACHE_DISPATCH CACHE_DANGER
#define CACHE_SAFE_ONE 1
#define CACHE_SAFE_TWO 2
#define CACHE_DANGER _Pragma("once")
`,
			secondInert: `#define CACHE_DISPATCH CACHE_SAFE_TWO
#define CACHE_SAFE_ONE 1
#define CACHE_SAFE_TWO 2
#define CACHE_DANGER _Pragma("once")
`,
			dangerousReason: "unmodeled _Pragma effect",
		},
		{
			name:       "reachable token paste",
			forcedBody: "CACHE_DISPATCH(left, right)\nCONFIG_FORCED\n",
			firstInert: `#define CACHE_DISPATCH CACHE_SAFE_ONE
#define CACHE_SAFE_ONE(left, right) left right
#define CACHE_SAFE_TWO(left, right) right left
#define CACHE_DANGER(left, right) left ## right
`,
			dangerous: `#define CACHE_DISPATCH CACHE_DANGER
#define CACHE_SAFE_ONE(left, right) left right
#define CACHE_SAFE_TWO(left, right) right left
#define CACHE_DANGER(left, right) left ## right
`,
			secondInert: `#define CACHE_DISPATCH CACHE_SAFE_TWO
#define CACHE_SAFE_ONE(left, right) left right
#define CACHE_SAFE_TWO(left, right) right left
#define CACHE_DANGER(left, right) left ## right
`,
			dangerousReason: "reachable token pasting macro CACHE_DANGER",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
				"include/linux/forced.h":   test.forcedBody,
			}, []string{
				"-nostdinc", "-I${tree:kernel}/include",
				"-include", "${tree:kernel}/include/linux/forced.h",
				"-c", "drivers/example/driver.c",
			}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			predefines := test.firstInert
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return predefines, true, nil
			}

			setsEqual := func(left, right ConfigDependencySet) bool {
				return left.Opaque == right.Opaque && left.Reason == right.Reason &&
					slices.Equal(left.Symbols, right.Symbols) &&
					slices.Equal(left.SourcePaths, right.SourcePaths) &&
					slices.Equal(left.ObjectPaths, right.ObjectPaths)
			}
			context := newConfigDependencyAnalysisContext(plan)
			run := func(name, contents string, wantOpaque bool) ConfigDependencySet {
				t.Helper()
				predefines = contents
				// Real compiler nodes have distinct predefine requests. Reset only
				// that request-result memo so this compact fixture changes the
				// probed replacement graph without discarding the shared prefix cache.
				context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
				cached, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
				if err != nil {
					t.Fatal(err)
				}
				freshContext := newConfigDependencyAnalysisContext(plan)
				freshContext.forcedHeaders = nil
				fresh, err := analyzeActionPlanNodeConfigDependencies(plan, node, freshContext)
				if err != nil {
					t.Fatal(err)
				}
				if !setsEqual(cached, fresh) {
					t.Fatalf("%s cached dependency set differs from cache-disabled analysis: cached=%#v fresh=%#v", name, cached, fresh)
				}
				if cached.Opaque != wantOpaque {
					t.Fatalf("%s dependency set = %#v, want opaque=%t", name, cached, wantOpaque)
				}
				if wantOpaque {
					if !strings.Contains(cached.Reason, test.dangerousReason) {
						t.Fatalf("%s opaque reason = %q, want %q", name, cached.Reason, test.dangerousReason)
					}
				} else if !slices.Equal(cached.Symbols, []string{"CONFIG_DRIVER", "CONFIG_FORCED"}) {
					t.Fatalf("%s precise symbols = %q", name, cached.Symbols)
				}
				return cached
			}

			run("first inert graph", test.firstInert, false)
			run("dangerous graph", test.dangerous, true)
			run("second inert graph", test.secondInert, false)
			cache := context.forcedHeaders
			if cache == nil || cache.misses != 1 || cache.hits != 2 || len(cache.entries) != 1 {
				t.Fatalf("replacement-graph forced-header cache = %#v, want one miss and two hits", cache)
			}
			for _, entries := range cache.entries {
				if len(entries) != 1 {
					t.Fatalf("replacement-graph forced-header specializations = %d, want 1", len(entries))
				}
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesForcedHeaderCacheSpecializesGeneratedInputsByCompilerNode(t *testing.T) {
	generatedPath := "include/generated/selected.h"
	plan, firstNode := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":      "CONFIG_DRIVER_A\n",
		"drivers/example/driver_b.c":    "CONFIG_DRIVER_B\n",
		"include/linux/forced-select.h": "#include <selected.h>\nCONFIG_FORCED\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/forced-select.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	secondSource := ActionPlanSource{
		ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/driver_b.c",
	}
	secondRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{
			"-nostdinc", "-I${tree:prep}/include/generated", "-I${tree:kernel}/include",
			"-include", "${tree:kernel}/include/linux/forced-select.h",
			"-c", secondSource.Path,
		},
	}
	secondNode := ActionPlanNode{
		ID: "compile-second", Stage: "target", Kind: "compile", Recipe: "compile-second-recipe", Tool: "cc", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: secondSource.ID}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/driver_b.o"}},
	}
	plan.Sources = append(plan.Sources, secondSource)
	plan.Recipes[secondNode.Recipe] = secondRecipe
	plan.Nodes = append(plan.Nodes, secondNode)
	secondSelection := compactKbuildSelectionKey{
		profile: "config-dependency", target: "drivers/example/driver_b.o", stage: "target",
	}
	plan.selectionGraph.selections[secondSelection] = CompactKbuildSelection{Profile: secondSelection.profile}
	plan.selectionGraph.materializedProducers[secondSelection] = secondNode.ID

	configDependencyStageGeneratedHeaderForTest(t, plan, &firstNode, "selected-header-a", generatedPath, ActionRecipe{
		Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_SELECTED_A",
		},
	})
	configDependencyStageGeneratedHeaderForTest(t, plan, &secondNode, "selected-header-b", generatedPath, ActionRecipe{
		Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_SELECTED_B",
		},
	})
	configDependencySetCompilerContractForTest(plan, firstNode, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SHARED 1\n", true, nil
	}

	setsEqual := func(left, right ConfigDependencySet) bool {
		return left.Opaque == right.Opaque && left.Reason == right.Reason &&
			slices.Equal(left.Symbols, right.Symbols) &&
			slices.Equal(left.SourcePaths, right.SourcePaths) &&
			slices.Equal(left.ObjectPaths, right.ObjectPaths)
	}
	context := newConfigDependencyAnalysisContext(plan)
	run := func(name string, node ActionPlanNode, wantSymbols []string) ConfigDependencySet {
		t.Helper()
		cached, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil {
			t.Fatal(err)
		}
		freshContext := newConfigDependencyAnalysisContext(plan)
		freshContext.forcedHeaders = nil
		fresh, err := analyzeActionPlanNodeConfigDependencies(plan, node, freshContext)
		if err != nil {
			t.Fatal(err)
		}
		if !setsEqual(cached, fresh) {
			t.Fatalf("%s cached dependency set differs from cache-disabled analysis: cached=%#v fresh=%#v", name, cached, fresh)
		}
		if cached.Opaque || !slices.Equal(cached.Symbols, wantSymbols) ||
			!slices.Equal(cached.ObjectPaths, []string{generatedPath}) {
			t.Fatalf("%s generated forced-header dependency set = %#v", name, cached)
		}
		return cached
	}

	first := run("first compiler miss", firstNode, []string{"CONFIG_DRIVER_A", "CONFIG_FORCED", "CONFIG_SELECTED_A"})
	second := run("second compiler miss", secondNode, []string{"CONFIG_DRIVER_B", "CONFIG_FORCED", "CONFIG_SELECTED_B"})
	firstHit := run("first compiler hit", firstNode, []string{"CONFIG_DRIVER_A", "CONFIG_FORCED", "CONFIG_SELECTED_A"})
	secondHit := run("second compiler hit", secondNode, []string{"CONFIG_DRIVER_B", "CONFIG_FORCED", "CONFIG_SELECTED_B"})
	if !setsEqual(first, firstHit) || !setsEqual(second, secondHit) {
		t.Fatalf("generated-input specialization was not stable: first=%#v first-hit=%#v second=%#v second-hit=%#v", first, firstHit, second, secondHit)
	}
	cache := context.forcedHeaders
	if cache == nil || cache.misses != 2 || cache.hits != 2 || len(cache.entries) != 1 {
		t.Fatalf("generated-input forced-header cache = %#v, want two misses, two hits, and one structural key", cache)
	}
	for _, entries := range cache.entries {
		if len(entries) != 2 {
			t.Fatalf("generated-input forced-header specializations = %d, want 2", len(entries))
		}
		identities := map[string]bool{}
		for _, entry := range entries {
			input, ok := entry.generatedInputs[generatedPath]
			if !ok || input.exactIdentity == "" {
				t.Fatalf("generated-input specialization lacks exact selected.h provenance: %#v", entry.generatedInputs)
			}
			identities[input.exactIdentity] = true
		}
		if len(identities) != 2 {
			t.Fatalf("generated-input specialization identities = %q, want two distinct producers", slices.Sorted(maps.Keys(identities)))
		}
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesForcedHeaderCacheRequeuesReenteredGuard(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#undef __DEFERRED_FORCED_BODY_H
#include <linux/deferred-forced-body.h>
CONFIG_TRANSLATION_UNIT
`,
		"include/linux/forced.h": "#include <linux/deferred-forced-body.h>\n",
		"include/linux/deferred-forced-body.h": `#ifndef __DEFERRED_FORCED_BODY_H
#define __DEFERRED_FORCED_BODY_H
CONFIG_ONLY_ON_REENTRY
#endif
`,
	}, []string{
		"-nostdinc", "-I", "${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/forced.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		// The forced prefix opens deferred-forced-body.h but skips its complete
		// conventional-guard body. The translation unit then invalidates that
		// initial state and reaches the body for the first time.
		return "#define __DEFERRED_FORCED_BODY_H 1\n", true, nil
	}

	context := newConfigDependencyAnalysisContext(plan)
	first, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	second, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	freshContext := newConfigDependencyAnalysisContext(plan)
	freshContext.forcedHeaders = nil
	fresh, err := analyzeActionPlanNodeConfigDependencies(plan, node, freshContext)
	if err != nil {
		t.Fatal(err)
	}
	wantSymbols := []string{"CONFIG_ONLY_ON_REENTRY", "CONFIG_TRANSLATION_UNIT"}
	for name, set := range map[string]ConfigDependencySet{
		"cache miss": first,
		"cache hit":  second,
		"fresh":      fresh,
	} {
		if set.Opaque || !slices.Equal(set.Symbols, wantSymbols) {
			t.Errorf("%s dependency set = %#v, want symbols %q", name, set, wantSymbols)
		}
	}
	if first.Reason != second.Reason || first.Reason != fresh.Reason ||
		!slices.Equal(first.SourcePaths, second.SourcePaths) ||
		!slices.Equal(first.SourcePaths, fresh.SourcePaths) ||
		!slices.Equal(first.ObjectPaths, second.ObjectPaths) ||
		!slices.Equal(first.ObjectPaths, fresh.ObjectPaths) {
		t.Fatalf("forced-header cache hit changed closure: first=%#v second=%#v fresh=%#v", first, second, fresh)
	}
	if cache := context.forcedHeaders; cache == nil || cache.hits != 1 || cache.misses != 1 {
		t.Fatalf("forced-header cache accounting = %#v, want one miss and one hit", cache)
	}
}

func TestConfigDependencyForcedHeaderCacheStructuralKeyIsExact(t *testing.T) {
	baseScanner := configDependencyClosureScanner{
		sourceLookup:       newConfigDependencySourceLookup(CompactKbuildProfile{Name: "profile"}),
		language:           "c",
		quoteDirectories:   []configDependencyIncludeDirectory{{logical: "quote", source: true}},
		includeDirectories: []configDependencyIncludeDirectory{{logical: "include"}},
		systemDirectories:  []configDependencyIncludeDirectory{{external: true}},
		afterDirectories:   []configDependencyIncludeDirectory{{logical: "after", source: true}},
	}
	baseFiles := []configDependencyScanFile{{
		logical: "include/linux/forced.h", physical: "/source/include/linux/forced.h", source: true,
	}}
	baseAutoconf := newConfigDependencyResolvedAutoconfDefinitions(
		map[string]string{"CONFIG_BASE": "y"},
	)
	base := newConfigDependencyForcedHeaderCacheKey(&baseScanner, baseFiles, baseAutoconf)

	changedScanners := map[string]configDependencyClosureScanner{}
	profile := baseScanner
	profile.sourceLookup.profileName = "other-profile"
	changedScanners["profile"] = profile
	language := baseScanner
	language.language = "assembler-with-cpp"
	changedScanners["language"] = language
	quote := baseScanner
	quote.quoteDirectories = []configDependencyIncludeDirectory{{logical: "other-quote", source: true}}
	changedScanners["quote search"] = quote
	include := baseScanner
	include.includeDirectories = []configDependencyIncludeDirectory{{logical: "include", source: true}}
	changedScanners["include provenance"] = include
	system := baseScanner
	system.systemDirectories = []configDependencyIncludeDirectory{{logical: "system"}}
	changedScanners["system search"] = system
	after := baseScanner
	after.afterDirectories = []configDependencyIncludeDirectory{{logical: "after", source: true}, {external: true}}
	changedScanners["after search"] = after
	for name, scanner := range changedScanners {
		if got := newConfigDependencyForcedHeaderCacheKey(&scanner, baseFiles, baseAutoconf); got == base {
			t.Errorf("forced-header cache key did not distinguish %s", name)
		}
	}
	changedFiles := slices.Clone(baseFiles)
	changedFiles[0].physical = "/other/include/linux/forced.h"
	if got := newConfigDependencyForcedHeaderCacheKey(&baseScanner, changedFiles, baseAutoconf); got == base {
		t.Error("forced-header cache key did not distinguish forced-file provenance")
	}
	if got := newConfigDependencyForcedHeaderCacheKey(
		&baseScanner, baseFiles,
		newConfigDependencyResolvedAutoconfDefinitions(map[string]string{"CONFIG_OTHER": "y"}),
	); got == base {
		t.Error("forced-header cache key did not distinguish resolved autoconf state")
	}
}

func TestConfigDependencyForcedHeaderCacheValidatesGeneratedInputProvenance(t *testing.T) {
	logical := "include/generated/shared.h"
	generated := func(identity, contents string) configDependencyScanFile {
		return configDependencyScanFile{
			logical: logical, exactIdentity: identity, exactContents: contents,
		}
	}
	entry := configDependencyForcedHeaderCacheEntry{
		generatedInputs: map[string]configDependencyScanFile{logical: generated("producer-a", "#define A 1\n")},
	}
	scanner := configDependencyClosureScanner{
		generated: map[string]bool{logical: true},
		generatedText: map[string]configDependencyGeneratedText{
			logical: {identity: "producer-a", contents: "#define A 1\n"},
		},
	}
	if resolved, ok := scanner.objectFile(logical); !ok || resolved != entry.generatedInputs[logical] ||
		scanner.resolvedGeneratedFiles[logical] != resolved {
		t.Fatalf("generated input was not retained for cache validation: resolved=%#v files=%#v", resolved, scanner.resolvedGeneratedFiles)
	}
	if !entry.matchesGeneratedInputs(&scanner) {
		t.Fatal("exact generated forced-header provenance was rejected")
	}
	scanner.generatedText[logical] = configDependencyGeneratedText{identity: "producer-b", contents: "#define B 1\n"}
	if entry.matchesGeneratedInputs(&scanner) {
		t.Fatal("changed generated forced-header producer reused a cached prefix")
	}
}

func TestConfigDependencyForcedHeaderCacheSharesMaterializedSnapshot(t *testing.T) {
	definitions := newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
		"CONFIG_CACHED_ALPHA": "y",
		"CONFIG_CACHED_BETA":  "y",
	})
	root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	state := root.branch()
	state.applyValidatedNumericMacroHeader()
	state.set("__GENERATED_AUTOCONF_H__", configDependencyMacroUndefined)
	state.applyResolvedConfigAutoconf(definitions)
	state.set("CONFIG_CACHED_ALPHA", configDependencyMacroUndefined)
	state.set("AFTER_CACHED_AUTOCONF", configDependencyMacroDefined)

	entry, ok := captureConfigDependencyForcedHeaderCacheEntry(
		&configDependencyClosureScanner{headerTrace: newConfigDependencyForcedHeaderResolutionTrace()}, state, root, nil,
	)
	if !ok || entry.snapshot == nil {
		t.Fatal("exact forced-header state was not materialized for caching")
	}
	cached := entry.snapshot
	restored, ok := entry.restore(&configDependencyClosureScanner{}, root)
	if !ok || restored == nil || restored.parent == nil || restored.snapshot != cached ||
		restored.parent.snapshot != cached {
		t.Fatalf("cached snapshot was not shared by restore: %#v", restored)
	}
	if !restored.snapshotChanges.empty() || restored.materializedSnapshotCache != nil {
		t.Fatalf(
			"restored cache hit did not start as a sparse overlay: changes=%d materialized=%p",
			restored.snapshotChanges.exactSize(), restored.materializedSnapshotCache,
		)
	}
	for name, want := range map[string]configDependencyMacroDefinition{
		"__GENERATED_AUTOCONF_H__": configDependencyMacroDefined,
		"CONFIG_CACHED_ALPHA":      configDependencyMacroUndefined,
		"CONFIG_CACHED_BETA":       configDependencyMacroDefined,
		"AFTER_CACHED_AUTOCONF":    configDependencyMacroDefined,
	} {
		if got := restored.definition(name); got != want {
			t.Errorf("restored %s = %d, want %d", name, got, want)
		}
	}

	restored.set("CONFIG_CACHED_BETA", configDependencyMacroUndefined)
	if restored.snapshot != cached || restored.snapshotChanges.exactSize() != 1 ||
		restored.materializedSnapshotCache != nil {
		t.Fatalf(
			"restored cache-hit mutation was not retained sparsely: snapshot=%p cached=%p changes=%d materialized=%p",
			restored.snapshot, cached, restored.snapshotChanges.exactSize(),
			restored.materializedSnapshotCache,
		)
	}
	again, ok := entry.restore(&configDependencyClosureScanner{}, root)
	if !ok || again.parent == nil || again.snapshot != cached ||
		again.materializedSnapshotCache != nil || !again.snapshotChanges.empty() {
		t.Fatal("second restore did not reuse the cached immutable snapshot")
	}
	if got := again.definition("CONFIG_CACHED_BETA"); got != configDependencyMacroDefined {
		t.Fatalf("restored-state mutation leaked into cache: %d", got)
	}
}

func captureForcedHeaderMacroEntryForTest(
	t testing.TB,
	root *configDependencyMacroState,
	mutate func(*configDependencyMacroState),
) configDependencyForcedHeaderCacheEntry {
	t.Helper()
	state := root.branch()
	if state == nil || !state.beginForcedHeaderTrace() {
		t.Fatal("could not start forced-header macro trace")
	}
	mutate(state)
	entry, ok := captureConfigDependencyForcedHeaderCacheEntry(
		&configDependencyClosureScanner{headerTrace: newConfigDependencyForcedHeaderResolutionTrace()}, state, root, nil,
	)
	if !ok {
		t.Fatal("could not capture forced-header macro transform")
	}
	return entry
}

func TestConfigDependencyForcedHeaderCacheReusesEquivalentCompilerDefinedNames(t *testing.T) {
	first, reason := parseConfigDependencyCompilerPredefines(
		"#define KBUILD_BASENAME first_unit\n#define KBUILD_MODNAME first_module\n",
	)
	if reason != "" {
		t.Fatal(reason)
	}
	second, reason := parseConfigDependencyCompilerPredefines(
		"#define KBUILD_BASENAME second_unit\n#define KBUILD_MODNAME second_module\n",
	)
	if reason != "" {
		t.Fatal(reason)
	}
	if first.snapshot == second.snapshot {
		t.Fatal("independent predefine parses unexpectedly shared their input snapshot")
	}
	entry := captureForcedHeaderMacroEntryForTest(t, &first, func(state *configDependencyMacroState) {
		state.set("FORCED_PREFIX_COMPLETE", configDependencyMacroDefined)
	})
	restored, ok := entry.restore(&configDependencyClosureScanner{}, &second)
	if !ok {
		t.Fatal("equivalent compiler defined-name set missed the forced-prefix cache")
	}
	if restored.snapshot != entry.snapshot || restored.parent == nil || restored.parent.snapshot != entry.snapshot {
		t.Fatal("equivalent compiler defined-name set did not take the O(1) snapshot path")
	}
	if restored.definition("FORCED_PREFIX_COMPLETE") != configDependencyMacroDefined {
		t.Fatal("O(1) restored prefix lost its exact output")
	}
}

func TestConfigDependencyForcedHeaderCacheRestoreInitializesZeroValueScannerSymbols(t *testing.T) {
	root, reason := parseConfigDependencyCompilerPredefines("#define COMPILER_ROOT 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	entry := captureForcedHeaderMacroEntryForTest(t, &root, func(state *configDependencyMacroState) {
		state.set("FORCED_PREFIX_COMPLETE", configDependencyMacroDefined)
	})
	entry.symbols = []string{"CONFIG_CACHED_PREFIX_SYMBOL"}

	scanner := &configDependencyClosureScanner{}
	if _, ok := entry.restore(scanner, &root); !ok {
		t.Fatal("forced-prefix cache entry did not restore into a zero-value pristine scanner")
	}
	if !scanner.symbols["CONFIG_CACHED_PREFIX_SYMBOL"] {
		t.Fatal("restore did not initialize a pristine scanner's symbol set")
	}
}

func TestConfigDependencyForcedHeaderCacheSparseReplayPreservesUnreadInput(t *testing.T) {
	first, reason := parseConfigDependencyCompilerPredefines(
		"#define FORCED_CONSTANT 1\n#define FIRST_TU_ONLY 1\n",
	)
	if reason != "" {
		t.Fatal(reason)
	}
	entry := captureForcedHeaderMacroEntryForTest(t, &first, func(state *configDependencyMacroState) {
		// This is deliberately a no-op in the captured root. A normalized
		// snapshot delta alone would forget that the prefix overwrites the name.
		state.set("FORCED_CONSTANT", configDependencyMacroDefined)
	})
	second, reason := parseConfigDependencyCompilerPredefines("#define SECOND_TU_ONLY 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	restored, ok := entry.restore(&configDependencyClosureScanner{}, &second)
	if !ok {
		t.Fatal("sparse forced-prefix transform did not replay on a different root")
	}
	if restored.snapshot == entry.snapshot {
		t.Fatal("different defined-name set incorrectly reused the captured post snapshot")
	}
	for name, want := range map[string]configDependencyMacroDefinition{
		"FORCED_CONSTANT": configDependencyMacroDefined,
		"FIRST_TU_ONLY":   configDependencyMacroUndefined,
		"SECOND_TU_ONLY":  configDependencyMacroDefined,
	} {
		if got := restored.definition(name); got != want {
			t.Errorf("sparse replay %s = %d, want %d", name, got, want)
		}
	}
	// Restores materialize independent persistent snapshots. A later TU write
	// must not mutate either the entry transform or another restored state.
	restored.set("FORCED_CONSTANT", configDependencyMacroUndefined)
	again, ok := entry.restore(&configDependencyClosureScanner{}, &second)
	if !ok || again.definition("FORCED_CONSTANT") != configDependencyMacroDefined {
		t.Fatal("restored-state mutation escaped into the cached transform")
	}
}

func TestConfigDependencyForcedHeaderCacheConcurrentSparseRestoresAreIsolated(t *testing.T) {
	captured, reason := parseConfigDependencyCompilerPredefines("#define CAPTURED_ONLY 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	entry := captureForcedHeaderMacroEntryForTest(t, &captured, func(state *configDependencyMacroState) {
		state.applyValidatedNumericMacroHeader()
		state.set("PREFIX_OUTPUT", configDependencyMacroDefined)
	})
	current, reason := parseConfigDependencyCompilerPredefines("#define CURRENT_ONLY 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	const workers = 32
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			restored, ok := entry.restore(&configDependencyClosureScanner{}, &current)
			if !ok {
				errors <- fmt.Errorf("worker %d: sparse restore missed", worker)
				return
			}
			if restored.definition("CURRENT_ONLY") != configDependencyMacroDefined ||
				restored.definition("CAPTURED_ONLY") != configDependencyMacroUnknown ||
				restored.definition("PREFIX_OUTPUT") != configDependencyMacroDefined {
				errors <- fmt.Errorf("worker %d: restored state has incorrect cells", worker)
				return
			}
			restored.set("PREFIX_OUTPUT", configDependencyMacroUndefined)
		}(worker)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	again, ok := entry.restore(&configDependencyClosureScanner{}, &current)
	if !ok || again.definition("PREFIX_OUTPUT") != configDependencyMacroDefined {
		t.Fatal("concurrent restored-state writes mutated the cache entry")
	}
}

func TestConfigDependencyForcedHeaderCacheValidatesExplicitAndImplicitMacroReads(t *testing.T) {
	t.Run("explicit conditional read", func(t *testing.T) {
		first, reason := parseConfigDependencyCompilerPredefines("#define PREFIX_SWITCH 1\n")
		if reason != "" {
			t.Fatal(reason)
		}
		entry := captureForcedHeaderMacroEntryForTest(t, &first, func(state *configDependencyMacroState) {
			if state.definition("PREFIX_SWITCH") == configDependencyMacroDefined {
				state.set("PREFIX_SELECTED", configDependencyMacroDefined)
			}
		})
		other, reason := parseConfigDependencyCompilerPredefines("")
		if reason != "" {
			t.Fatal(reason)
		}
		if _, ok := entry.restore(&configDependencyClosureScanner{}, &other); ok {
			t.Fatal("different explicitly-read compiler macro reused a specialization")
		}
		matching, reason := parseConfigDependencyCompilerPredefines("#define PREFIX_SWITCH 0\n")
		if reason != "" {
			t.Fatal(reason)
		}
		if _, ok := entry.restore(&configDependencyClosureScanner{}, &matching); !ok {
			t.Fatal("equal explicitly-read compiler cell did not reuse a specialization")
		}
	})

	t.Run("possible-branch unchanged arm", func(t *testing.T) {
		first, reason := parseConfigDependencyCompilerPredefines("#define JOIN_TARGET 1\n")
		if reason != "" {
			t.Fatal(reason)
		}
		entry := captureForcedHeaderMacroEntryForTest(t, &first, func(state *configDependencyMacroState) {
			possible := state.branch()
			possible.set("JOIN_TARGET", configDependencyMacroDefined)
			state.mergePossibleBranch(possible)
		})
		other, reason := parseConfigDependencyCompilerPredefines("")
		if reason != "" {
			t.Fatal(reason)
		}
		if _, ok := entry.restore(&configDependencyClosureScanner{}, &other); ok {
			t.Fatal("join output reused without matching the unchanged arm's input cell")
		}
	})
}

func TestConfigDependencyForcedHeaderCacheSparseReplayRetainsNumericWildcard(t *testing.T) {
	first, reason := parseConfigDependencyCompilerPredefines("#define OLD_DEFINED 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	entry := captureForcedHeaderMacroEntryForTest(t, &first, func(state *configDependencyMacroState) {
		state.applyValidatedNumericMacroHeader()
		state.set("EXACT_AFTER_WILDCARD", configDependencyMacroUndefined)
	})
	second, reason := parseConfigDependencyCompilerPredefines(
		"#define NEW_DEFINED 1\n#define CONFIG_STAYS_DEFINED 1\n",
	)
	if reason != "" {
		t.Fatal(reason)
	}
	restored, ok := entry.restore(&configDependencyClosureScanner{}, &second)
	if !ok {
		t.Fatal("numeric wildcard transform did not replay")
	}
	for name, want := range map[string]configDependencyMacroDefinition{
		"OLD_DEFINED":          configDependencyMacroUnknown,
		"NEW_DEFINED":          configDependencyMacroDefined,
		"CONFIG_STAYS_DEFINED": configDependencyMacroDefined,
		"EXACT_AFTER_WILDCARD": configDependencyMacroUndefined,
	} {
		if got := restored.definition(name); got != want {
			t.Errorf("numeric wildcard replay %s = %d, want %d", name, got, want)
		}
	}
	if definition, recorded := restored.explicitDefinition(configDependencyResolvedAutoconfGuard); definition != configDependencyMacroUnknown || !recorded {
		t.Fatalf("numeric wildcard autoconf guard = (%d,%t), want (unknown,true)", definition, recorded)
	}
}

func TestConfigDependencyForcedHeaderCacheRejectsWholeNamespaceRead(t *testing.T) {
	root, reason := parseConfigDependencyCompilerPredefines("#define COMPILER 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	state := root.branch()
	if !state.beginForcedHeaderTrace() {
		t.Fatal("could not start forced-header trace")
	}
	if _, valid := configDependencyConditionalMacroStateKey(state); !valid {
		t.Fatal("ordinary recursive-state projection unexpectedly failed")
	}
	if _, ok := captureConfigDependencyForcedHeaderCacheEntry(
		&configDependencyClosureScanner{headerTrace: newConfigDependencyForcedHeaderResolutionTrace()}, state, &root, nil,
	); ok {
		t.Fatal("whole-namespace read was captured as a sparse specialization")
	}
	if state.tainted {
		t.Fatal("cache capture instrumentation tainted normal preprocessing")
	}
}

func TestConfigDependencyForcedHeaderCacheScalesAcrossTranslationUnits(t *testing.T) {
	files := map[string]string{"drivers/example/driver.c": "CONFIG_DRIVER\n"}
	const headerCount = 96
	for index := 0; index < headerCount; index++ {
		next := ""
		if index+1 < headerCount {
			next = fmt.Sprintf("#include <linux/cache-%03d.h>\n", index+1)
		}
		files[fmt.Sprintf("include/linux/cache-%03d.h", index)] = fmt.Sprintf(
			"#ifndef __CACHE_%03d_H\n#define __CACHE_%03d_H\n#ifdef CACHE_ENABLED\nCONFIG_CACHE_%03d\n%s#endif\n#endif\n",
			index, index, index, next,
		)
	}
	plan, node := configDependencyCompilePlanForTest(t, files, []string{
		"-nostdinc", "-I", "${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/cache-000.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define CACHE_ENABLED 1\n", true, nil
	}
	context := newConfigDependencyAnalysisContext(plan)
	const analyses = 256
	for iteration := 0; iteration < analyses; iteration++ {
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		if err != nil {
			t.Fatal(err)
		}
		if set.Opaque || len(set.SourcePaths) != headerCount+1 || len(set.Symbols) != headerCount+1 {
			t.Fatalf("iteration %d dependency closure = %#v", iteration, set)
		}
	}
	if cache := context.forcedHeaders; cache == nil || cache.misses != 1 || cache.hits != analyses-1 || len(cache.entries) != 1 {
		t.Fatalf("scaled forced-header cache accounting = %#v", cache)
	}
}

func BenchmarkConfigDependencyForcedHeaderPrefixAcrossCompilerNodes(b *testing.B) {
	root := b.TempDir()
	var forcedContents strings.Builder
	forcedContents.WriteString("#ifndef __BENCHMARK_FORCED_H\n#define __BENCHMARK_FORCED_H\n")
	const conditionalCount = 512
	for index := 0; index < conditionalCount; index++ {
		fmt.Fprintf(
			&forcedContents,
			"#ifdef CACHE_ENABLED\n#define CACHE_NAME_%d 1\nCONFIG_CACHE_%d\n#endif\n",
			index, index,
		)
	}
	forcedContents.WriteString("#endif\n")
	forcedPhysical := filepath.Join(root, "forced.h")
	unitPhysical := filepath.Join(root, "unit.c")
	if err := os.WriteFile(forcedPhysical, []byte(forcedContents.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(unitPhysical, []byte("CONFIG_UNIT\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	forcedFile := configDependencyScanFile{
		logical: "include/linux/forced.h", physical: forcedPhysical, source: true,
	}
	unitFile := configDependencyScanFile{
		logical: "drivers/example/unit.c", physical: unitPhysical, source: true,
	}
	rootState, reason := parseConfigDependencyCompilerPredefines("#define CACHE_ENABLED 1\n")
	if reason != "" {
		b.Fatal(reason)
	}
	newScanner := func(
		conditionalSyntax map[string]configDependencyConditionalSyntax,
		parsed map[string]configDependencyParsedFile,
	) *configDependencyClosureScanner {
		return &configDependencyClosureScanner{
			sourceLookup: newConfigDependencySourceLookup(CompactKbuildProfile{Name: "benchmark"}), language: "c",
			generated: map[string]bool{}, generatedText: map[string]configDependencyGeneratedText{},
			preconfigured: map[string]string{}, physicalFiles: newConfigDependencyPhysicalFileCache(),
			symbols: map[string]bool{}, sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
			queued: map[string]bool{}, queuedPhysical: map[string]bool{},
			conditionalSyntax: conditionalSyntax, parsed: parsed,
		}
	}
	run := func(
		cache *configDependencyForcedHeaderCache,
		conditionalSyntax map[string]configDependencyConditionalSyntax,
		parsed map[string]configDependencyParsedFile,
	) ConfigDependencySet {
		scanner := newScanner(conditionalSyntax, parsed)
		state := rootState.branch()
		if cache == nil {
			scanner.interpretConditionalCompilerFiles(
				[]configDependencyScanFile{forcedFile}, state, configDependencyResolvedAutoconfDefinitions{},
			)
		} else {
			key := newConfigDependencyForcedHeaderCacheKey(
				scanner, []configDependencyScanFile{forcedFile},
				configDependencyResolvedAutoconfDefinitions{},
			)
			if cached, hit := cache.lookup(key, scanner, &rootState); hit {
				state = cached
			} else {
				if !state.beginForcedHeaderTrace() {
					b.Fatal("could not start forced-header benchmark trace")
				}
				scanner.headerTrace = newConfigDependencyForcedHeaderResolutionTrace()
				scanner.interpretConditionalCompilerFiles(
					[]configDependencyScanFile{forcedFile}, state, configDependencyResolvedAutoconfDefinitions{},
				)
				if entry, exact := captureConfigDependencyForcedHeaderCacheEntry(
					scanner, state, &rootState, []configDependencyScanFile{forcedFile},
				); exact {
					cache.store(key, entry)
				}
				scanner.headerTrace = nil
				state.endForcedHeaderTrace()
			}
		}
		scanner.interpretConditionalCompilerFiles(
			[]configDependencyScanFile{unitFile}, state, configDependencyResolvedAutoconfDefinitions{},
		)
		return scanner.scan()
	}
	for _, benchmark := range []struct {
		name  string
		cache bool
	}{
		{name: "uncached"},
		{name: "cached", cache: true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			conditionalSyntax := map[string]configDependencyConditionalSyntax{}
			parsed := map[string]configDependencyParsedFile{}
			var cache *configDependencyForcedHeaderCache
			if benchmark.cache {
				cache = &configDependencyForcedHeaderCache{}
				set := run(cache, conditionalSyntax, parsed)
				if set.Opaque || len(set.Symbols) != conditionalCount+1 {
					b.Fatalf("warm dependency set = %#v", set)
				}
			}
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				set := run(cache, conditionalSyntax, parsed)
				if set.Opaque || len(set.Symbols) != conditionalCount+1 {
					b.Fatalf("dependency set = %#v", set)
				}
			}
		})
	}
}

func TestBuildActionPlanConfigDependencyAnalysisMemoizesExactCompilerProjection(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready-%t", ready), func(t *testing.T) {
			arguments := []string{"-nostdinc", "-c", "drivers/example/driver.c"}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
			}, arguments, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
				node.ID: {
					Tool: "cc", Arguments: slices.Clone(arguments), Environment: map[string]string{},
				},
			}
			calls := 0
			plan.metadata.compilerPredefines = func(
				scope, role, language string,
				arguments, translationUnits []string,
				environment map[string]string,
			) (string, bool, error) {
				calls++
				if scope != "target" || role != "cc" || language != "c" ||
					!slices.Equal(arguments, []string{"-nostdinc"}) || len(translationUnits) != 0 || len(environment) != 0 {
					t.Fatalf(
						"compiler projection %d = %q/%q/%q args=%q units=%q env=%#v",
						calls, scope, role, language, arguments, translationUnits, environment,
					)
				}
				return "#define __STDC__ 1\n", ready, nil
			}

			analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("compiler-predefine callback calls = %d, want one shared registration/analysis request", calls)
			}
			if len(analysis.sets) != 1 {
				t.Fatalf("analysis sets = %#v", analysis.sets)
			}
			set := analysis.sets[0]
			if !ready {
				if !set.Opaque || !strings.Contains(set.Reason, "unavailable during discovery") {
					t.Fatalf("discovery dependency set = %#v, want unavailable opaque result", set)
				}
				return
			}
			if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) ||
				!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) {
				t.Fatalf("replay dependency set = %#v, want precise compiler closure", set)
			}
		})
	}
}

func TestConfigDependencyCompilerPredefineKeyIsExact(t *testing.T) {
	first := configDependencyCompilerPredefineKey(
		"target", "cc", "c", []string{"a", "bc"}, []string{"unit.c"},
		map[string]string{"SECOND": "2", "FIRST": "1"},
	)
	reorderedEnvironment := configDependencyCompilerPredefineKey(
		"target", "cc", "c", []string{"a", "bc"}, []string{"unit.c"},
		map[string]string{"FIRST": "1", "SECOND": "2"},
	)
	if first != reorderedEnvironment {
		t.Fatalf("compiler-predefine key depends on environment map iteration: %q != %q", first, reorderedEnvironment)
	}
	for name, changed := range map[string]configDependencyCompilerPredefineRequestKey{
		"scope": configDependencyCompilerPredefineKey(
			"host", "cc", "c", []string{"a", "bc"}, []string{"unit.c"}, map[string]string{"FIRST": "1", "SECOND": "2"},
		),
		"role": configDependencyCompilerPredefineKey(
			"target", "cxx", "c", []string{"a", "bc"}, []string{"unit.c"}, map[string]string{"FIRST": "1", "SECOND": "2"},
		),
		"language": configDependencyCompilerPredefineKey(
			"target", "cc", "c++", []string{"a", "bc"}, []string{"unit.c"}, map[string]string{"FIRST": "1", "SECOND": "2"},
		),
		"argument boundary": configDependencyCompilerPredefineKey(
			"target", "cc", "c", []string{"ab", "c"}, []string{"unit.c"}, map[string]string{"FIRST": "1", "SECOND": "2"},
		),
		"translation unit": configDependencyCompilerPredefineKey(
			"target", "cc", "c", []string{"a", "bc"}, []string{"other.c"}, map[string]string{"FIRST": "1", "SECOND": "2"},
		),
		"environment": configDependencyCompilerPredefineKey(
			"target", "cc", "c", []string{"a", "bc"}, []string{"unit.c"}, map[string]string{"FIRST": "changed", "SECOND": "2"},
		),
	} {
		if changed == first {
			t.Errorf("compiler-predefine key did not distinguish %s", name)
		}
	}
}

func TestConfigDependencyAnalysisContextCachesPreconfiguredPathsByObjectRoot(t *testing.T) {
	plan, _ := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	objectRoot := t.TempDir()
	profile := plan.selectionGraph.profiles["config-dependency"]
	profile.evaluator.template.sourceRoots["__LINUX_BZL_OBJECT_TREE__"] = objectRoot
	plan.selectionGraph.profiles[profile.Name] = profile
	plan.metadata.preconfiguredObjectTree = true
	plan.metadata.exactSourcePaths = map[string]string{
		"include/config/exact.h": "prep",
	}

	context := newConfigDependencyAnalysisContext(plan)
	first := context.preconfiguredConfigDependencyPaths(profile, plan.metadata)
	second := context.preconfiguredConfigDependencyPaths(profile, plan.metadata)
	want := map[string]string{
		"drivers/example/driver.o": filepath.Join(objectRoot, "drivers/example/driver.o"),
		"include/config/exact.h":   filepath.Join(objectRoot, "include/config/exact.h"),
	}
	if !maps.Equal(first, want) || !maps.Equal(second, want) {
		t.Fatalf("cached preconfigured paths = first %#v second %#v, want %#v", first, second, want)
	}
	if got := len(context.preconfiguredByRoot); got != 1 {
		t.Fatalf("preconfigured object-root cache entries = %d, want one", got)
	}
}

func configDependencyPhysicalFileCacheScannerForTest(
	profile CompactKbuildProfile,
	cache *configDependencyPhysicalFileCache,
) *configDependencyClosureScanner {
	return &configDependencyClosureScanner{
		sourceLookup: newConfigDependencySourceLookup(profile), physicalFiles: cache,
		generated: map[string]bool{}, preconfigured: map[string]string{},
		symbols: map[string]bool{}, sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
		queued: map[string]bool{}, queuedPhysical: map[string]bool{},
	}
}

func TestConfigDependencyAnalysisContextCachesPhysicalFileStatus(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "include", "linux", "shared.h")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("CONFIG_SHARED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := CompactKbuildProfile{
		Name: "physical-file-cache",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	context := newConfigDependencyAnalysisContext(nil)
	statCalls := 0
	context.physicalFiles.stat = func(filename string) (os.FileInfo, error) {
		statCalls++
		return os.Stat(filename)
	}
	for range 2 {
		scanner := configDependencyPhysicalFileCacheScannerForTest(profile, context.physicalFiles)
		file, ok := scanner.sourceFile("include/linux/shared.h")
		if !ok {
			t.Fatal("cached regular source file was not resolved")
		}
		scanner.queue(file)
		if scanner.opaqueReason != "" || len(scanner.pending) != 1 {
			t.Fatalf("cached regular source queue = pending %d opaque %q", len(scanner.pending), scanner.opaqueReason)
		}
	}
	if statCalls != 1 {
		t.Fatalf("shared analysis-context stat calls = %d, want one across source resolution, queue admission, and compiler nodes", statCalls)
	}
	if got := len(context.physicalFiles.entries); got != 1 {
		t.Fatalf("shared analysis-context physical-file entries = %d, want one", got)
	}

	other := newConfigDependencyAnalysisContext(nil)
	if other.physicalFiles == context.physicalFiles || len(other.physicalFiles.entries) != 0 {
		t.Fatal("physical-file status cache escaped its analysis context")
	}
}

func TestConfigDependencySharedCacheReusesImmutableInputsAcrossPlans(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "include", "linux", "shared.h")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("CONFIG_SHARED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := CompactKbuildProfile{
		Name: "family-shared-cache",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	shared := NewActionPlanConfigDependencySharedCache()
	statCalls := 0
	shared.physicalFiles.stat = func(filename string) (os.FileInfo, error) {
		statCalls++
		return os.Stat(filename)
	}
	contexts := []*configDependencyAnalysisContext{
		newConfigDependencyAnalysisContextWithCache(nil, shared),
		newConfigDependencyAnalysisContextWithCache(nil, shared),
	}
	for index, context := range contexts {
		scanner := configDependencyPhysicalFileCacheScannerForTest(profile, context.physicalFiles)
		scanner.parsed = context.parsed
		scanner.conditionalSyntax = context.conditionalSyntax
		file, ok := scanner.sourceFile("include/linux/shared.h")
		if !ok {
			t.Fatalf("variant %d did not resolve shared source", index)
		}
		scanner.queue(file)
		set := scanner.scan()
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_SHARED"}) {
			t.Fatalf("variant %d dependency set = %#v", index, set)
		}
	}
	if statCalls != 1 {
		t.Fatalf("family-shared stat calls = %d, want one across two plans", statCalls)
	}
	if len(shared.parsed) != 1 {
		t.Fatalf("family-shared parsed-file entries = %d, want one", len(shared.parsed))
	}
	if contexts[0].physicalFiles != contexts[1].physicalFiles || contexts[0].parsed != nil && len(contexts[0].parsed) != len(contexts[1].parsed) {
		t.Fatal("immutable source caches were not shared across family contexts")
	}
	if contexts[0].forcedHeaders == contexts[1].forcedHeaders ||
		contexts[0].sourcePathInterns == contexts[1].sourcePathInterns ||
		contexts[0].generatedText == nil || contexts[1].generatedText == nil {
		t.Fatal("variant-local dependency state was not isolated")
	}
}

func configDependencyCompletedCachePlanForTest(
	t *testing.T,
	root string,
	profileName string,
	nodeID string,
	sourceID string,
	product string,
	toolset string,
	config map[string]string,
) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	profile := mustCompactKbuildProfileForTest(
		t, profileName, "scripts/Makefile.build", "drivers/example", "obj-y += driver.o\n", nil,
	)
	if profile.evaluator == nil || profile.evaluator.template == nil {
		t.Fatal("completed-cache test profile has no evaluator")
	}
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	source := ActionPlanSource{ID: sourceID, Namespace: "kernel", Path: "drivers/example/driver.c"}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{
			"-nostdinc", "-c", "${source:source:00000000}", "-o", "${output:00000000}",
		},
		Sources: []string{"source:00000000"}, Outputs: []string{"00000000"},
	}
	node := ActionPlanNode{
		ID: nodeID, Stage: "target", Kind: "compile", Recipe: "compile-recipe", Tool: "cc", Product: product,
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: source.ID}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/driver.o"}},
	}
	selection := compactKbuildSelectionKey{profile: profile.Name, target: "drivers/example/driver.o", stage: "target"}
	graph := &compactKbuildSelectionGraph{
		profiles:              map[string]CompactKbuildProfile{profile.Name: profile},
		selections:            map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
		materializedProducers: map[compactKbuildSelectionKey]string{selection: node.ID},
	}
	plan := &ActionPlan{
		Toolsets:       map[string]string{"target": toolset, "host": "host-toolset"},
		Sources:        []ActionPlanSource{source},
		Recipes:        map[string]ActionRecipe{node.Recipe: recipe},
		Nodes:          []ActionPlanNode{node},
		metadata:       &CompactMetadata{configFragment: maps.Clone(config)},
		selectionGraph: graph,
	}
	return plan, node
}

func configDependencyBuildWithFamilyCacheForTest(
	t *testing.T,
	plan *ActionPlan,
	cache *ActionPlanFamilyPlanningCache,
) map[string]ConfigDependencySet {
	t.Helper()
	plan.attachFamilyPlanningCache(cache)
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithCache(
		plan, cache.configDependencyCache(),
	)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(plan)
	if err != nil {
		t.Fatal(err)
	}
	return sets
}

func configDependencySourceTableSnapshotForTest(plan *ActionPlan) []string {
	if plan == nil {
		return nil
	}
	result := make([]string, 0, len(plan.Sources))
	for _, source := range plan.Sources {
		result = append(result, source.Namespace+"\x00"+source.Path)
	}
	sort.Strings(result)
	return result
}

func TestConfigDependencyCompletedCompilerCacheHitsAcrossUnrelatedConfigChange(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	first, firstNode := configDependencyCompletedCachePlanForTest(
		t, root, "variant-a", "compile-a", "src-00000001", "image-a", "target-toolset",
		map[string]string{"CONFIG_RELEVANT": "y", "CONFIG_UNRELATED": "n"},
	)
	second, secondNode := configDependencyCompletedCachePlanForTest(
		t, root, "variant-b", "compile-b", "src-00000001", "image-a", "target-toolset",
		map[string]string{"CONFIG_RELEVANT": "y", "CONFIG_UNRELATED": "y"},
	)
	if firstNode.ContentID() != secondNode.ContentID() {
		t.Fatalf("semantically identical completed-cache nodes have content IDs %q and %q",
			firstNode.ContentID(), secondNode.ContentID())
	}
	firstSet := configDependencyBuildWithFamilyCacheForTest(t, first, cache)[firstNode.ID]
	secondSet := configDependencyBuildWithFamilyCacheForTest(t, second, cache)[secondNode.ID]
	if firstSet.Opaque || !configDependencySetsEqual(firstSet, secondSet) ||
		!slices.Equal(secondSet.Symbols, []string{"CONFIG_RELEVANT"}) {
		t.Fatalf("completed-cache dependency sets = first %#v second %#v", firstSet, secondSet)
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("completed-cache stats = hits %d stores %d, want 1/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencyCompletedCompilerCacheHitsWithDiscoveredHeader(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#include \"feature.h\"\n")
	mustWriteSource(t, root, "drivers/example/feature.h", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	snapshots := [][]string{}
	for index, unrelated := range []string{"n", "y"} {
		plan, node := configDependencyCompletedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			"src-00000001", "image", "target-toolset",
			map[string]string{"CONFIG_RELEVANT": "y", "CONFIG_UNRELATED": unrelated},
		)
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_RELEVANT"}) ||
			!slices.Equal(set.SourcePaths, []string{
				"drivers/example/driver.c",
				"drivers/example/feature.h",
			}) {
			t.Fatalf("discovered-header variant %d dependency set = %#v", index, set)
		}
		snapshots = append(snapshots, configDependencySourceTableSnapshotForTest(plan))
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("discovered-header cache stats = hits %d stores %d, want 1/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
	wantSources := []string{
		"kernel\x00drivers/example/driver.c",
		"kernel\x00drivers/example/feature.h",
	}
	if !slices.Equal(snapshots[0], wantSources) || !slices.Equal(snapshots[1], wantSources) {
		t.Fatalf("live/hit source snapshots = %#v, want %#v", snapshots, wantSources)
	}
}

func TestConfigDependencyCompletedCompilerCacheHitsForIdenticalPersistentInputSetRoot(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	sets := make([]ConfigDependencySet, 0, 2)
	inputSetRoots := make([]string, 0, 2)
	for index, unrelated := range []string{"n", "y"} {
		plan, node := configDependencyCompletedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			"src-00000001", "image", "target-toolset",
			map[string]string{"CONFIG_RELEVANT": "y", "CONFIG_UNRELATED": unrelated},
		)
		plan.attachFamilyPlanningCache(cache)
		node.Sources = nil
		recipe := plan.Recipes[node.Recipe]
		recipe.Arguments[2] = "drivers/example/driver.c"
		plan.Recipes[node.Recipe] = recipe
		node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
			Target: ActionPlanInputSetTarget{
				Kind: ActionPlanInputSetWorkTarget, Path: "drivers/example/driver.c",
			},
			SourceID:    "src-00000001",
			CompilerUse: true,
		})
		plan.Nodes[0] = node
		sets = append(sets, configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID])
		inputSetRoots = append(inputSetRoots, node.InputSet)
	}
	if inputSetRoots[0] == "" || inputSetRoots[0] != inputSetRoots[1] {
		t.Fatalf("persistent input-set roots = %q, want one shared content root", inputSetRoots)
	}
	if sets[0].Opaque || !configDependencySetsEqual(sets[0], sets[1]) ||
		!slices.Equal(sets[1].Symbols, []string{"CONFIG_RELEVANT"}) {
		t.Fatalf("persistent completed-cache dependency sets = %#v", sets)
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("persistent completed-cache stats = hits %d stores %d, want 1/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func configDependencyCompletedDriverLinkPlanForTest(
	t *testing.T,
	root string,
	profileName string,
	nodeID string,
	sourceID string,
	product string,
) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	arguments := []string{
		"-I", "${tree:kernel}/include",
		"-o", "tools/helper",
		"${tree:kernel}/drivers/example/driver.c",
	}
	plan, node := configDependencyCompletedCachePlanForTest(
		t, root, profileName, nodeID, sourceID, product, "target-toolset", map[string]string{},
	)
	node.Outputs = []ActionPlanOutput{{Tree: "host", Path: "tools/helper"}}
	node.Trees = []string{"kernel"}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: compactKbuildScriptRunnerRole,
		Arguments:        []string{"${output:00000000}"},
		WorkingDirectory: "driver-link",
		WorkingInputs: map[string]string{
			"source:source:00000000": "drivers/example/driver.c",
		},
		WorkingOutputs: map[string]string{"00000000": "tools/helper"},
		CompilerInvocation: &ActionRecipeCompilerInvocation{
			Tool:                     "cc",
			Arguments:                arguments,
			WorkingInputUses:         []string{"source:source:00000000"},
			WorkingInputUsesComplete: true,
		},
		Sources: []string{"source:00000000"}, Outputs: []string{"00000000"}, Trees: []string{"kernel"},
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("completed-cache driver-link recipe: %v", err)
	}
	plan.Recipes[node.Recipe] = recipe
	plan.Nodes = []ActionPlanNode{node}
	profile := plan.selectionGraph.profiles[profileName]
	selection := compactKbuildSelectionKey{profile: profileName, target: "tools/helper", stage: node.Stage}
	plan.selectionGraph = &compactKbuildSelectionGraph{
		profiles:              map[string]CompactKbuildProfile{profileName: profile},
		selections:            map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profileName}},
		materializedProducers: map[compactKbuildSelectionKey]string{selection: node.ID},
	}
	baseRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: "cc"}
	linkRole, ok := toolaction.LinkContractRole("cc")
	if !ok {
		t.Fatal("cc link contract is unavailable")
	}
	linkRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: linkRole}
	plan.metadata.actionRoles = []KbuildActionRoleRef{baseRef, linkRef}
	plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
		baseRef: {}, linkRef: {},
	}
	return plan, node
}

func TestConfigDependencyCompletedCompilerCacheHitsForDriverLinkIncludeSnapshot(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#include <shared.h>\nint main(void) { return SHARED; }\n")
	mustWriteSource(t, root, "include/shared.h", "#define SHARED 1\n")
	cache := NewActionPlanFamilyPlanningCache()
	snapshots := [][]string{}
	for index := range 2 {
		plan, node := configDependencyCompletedDriverLinkPlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			"src-00000001", "image",
		)
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.SourcePaths, []string{
			"drivers/example/driver.c", "include/shared.h",
		}) {
			t.Fatalf("driver-link variant %d dependency set = %#v", index, set)
		}
		snapshots = append(snapshots, configDependencySourceTableSnapshotForTest(plan))
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("driver-link completed-cache stats = hits %d stores %d, want 1/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
	wantSources := []string{
		"kernel\x00drivers/example/driver.c",
		"kernel\x00include/shared.h",
	}
	if !slices.Equal(snapshots[0], wantSources) || !slices.Equal(snapshots[1], wantSources) {
		t.Fatalf("driver-link live/hit source snapshots = %#v, want %#v", snapshots, wantSources)
	}
}

func TestConfigDependencyCompletedCompilerCacheMissesOnRelevantConfigChange(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index, value := range []string{"y", "n"} {
		plan, _ := configDependencyCompletedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), fmt.Sprintf("image-%d", index), "target-toolset",
			map[string]string{"CONFIG_RELEVANT": value},
		)
		configDependencyBuildWithFamilyCacheForTest(t, plan, cache)
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.misses != 2 ||
		cache.configDependencies.completed.stores != 2 {
		t.Fatalf("relevant-change stats = hits %d misses %d stores %d, want 0/2/2",
			cache.configDependencies.completed.hits,
			cache.configDependencies.completed.misses,
			cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencyCompletedCompilerCacheMissesOnToolsetOrSourceChange(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	for _, root := range []string{firstRoot, secondRoot} {
		mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	}
	cache := NewActionPlanFamilyPlanningCache()
	for index, fixture := range []struct {
		root    string
		toolset string
	}{
		{root: firstRoot, toolset: "target-toolset-a"},
		{root: firstRoot, toolset: "target-toolset-b"},
		{root: secondRoot, toolset: "target-toolset-a"},
	} {
		plan, _ := configDependencyCompletedCachePlanForTest(
			t, fixture.root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), "image", fixture.toolset,
			map[string]string{"CONFIG_RELEVANT": "y"},
		)
		configDependencyBuildWithFamilyCacheForTest(t, plan, cache)
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.misses != 3 ||
		cache.configDependencies.completed.stores != 3 {
		t.Fatalf("toolset/source-change stats = hits %d misses %d stores %d, want 0/3/3",
			cache.configDependencies.completed.hits,
			cache.configDependencies.completed.misses,
			cache.configDependencies.completed.stores)
	}
}

func configDependencyCompletedGeneratedCachePlanForTest(
	t *testing.T,
	root string,
	profileName string,
	compileID string,
	sourceID string,
	producerID string,
	product string,
	producerRecipe ActionRecipe,
) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	plan, node := configDependencyCompletedCachePlanForTest(
		t, root, profileName, compileID, sourceID, product, "target-toolset",
		map[string]string{"CONFIG_GENERATED": "y"},
	)
	const generatedPath = "include/generated/completed-cache.h"
	producerRecipe.Schema = LinuxKernelPlanSchema
	producerRecipe.Kind = "generate"
	producerRecipe.Outputs = []string{"00000000"}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		ID: producerID, Stage: "prep", Kind: "generate", Recipe: producerRecipeID, Tool: producerRecipe.Tool,
		Product: product,
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: generatedPath}},
	}
	node.Inputs = []ActionPlanNodeEdge{{Role: "generated", ProducerID: producer.ID, Slot: 0}}
	node.Trees = []string{"prep"}
	compileRecipe := plan.Recipes[node.Recipe]
	compileRecipe.Arguments = []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "${source:source:00000000}",
		"-o", "${output:00000000}",
	}
	compileRecipe.WorkingDirectory = "compile"
	compileRecipe.WorkingInputs = map[string]string{"input:generated:00000000": generatedPath}
	compileRecipe.Inputs = []string{"generated:00000000"}
	compileRecipe.Trees = []string{"prep"}
	plan.Recipes[node.Recipe] = compileRecipe
	plan.Recipes[producerRecipeID] = producerRecipe
	// The family structural index is predecessor-ordered. Production lowering
	// naturally has this order; spell it explicitly in this compact fixture.
	plan.Nodes = []ActionPlanNode{producer, node}
	return plan, node
}

func TestConfigDependencyCompletedCompilerCacheHitsForIdenticalGeneratedProducerIdentity(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#include <completed-cache.h>\n")
	cache := NewActionPlanFamilyPlanningCache()
	producerRecipe := ActionRecipe{Tool: "actionfile", Arguments: []string{
		"-out", "${output:00000000}", "-line", "CONFIG_GENERATED",
	}}
	for index := range 2 {
		plan, node := configDependencyCompletedGeneratedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			"src-00000001", "producer", "image", producerRecipe,
		)
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_GENERATED"}) {
			t.Fatalf("generated producer variant %d dependency set = %#v", index, set)
		}
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("generated-producer rebind stats = hits %d stores %d, want 1/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencyCompletedCompilerCacheMissesOnGeneratedProducerIdentityChange(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#include <completed-cache.h>\n")
	cache := NewActionPlanFamilyPlanningCache()
	producerRecipes := []ActionRecipe{
		{Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_GENERATED",
		}},
		{Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-content_base64",
			base64.StdEncoding.EncodeToString([]byte("CONFIG_GENERATED\n")),
		}},
	}
	for index, producerRecipe := range producerRecipes {
		plan, node := configDependencyCompletedGeneratedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), fmt.Sprintf("producer-%d", index),
			fmt.Sprintf("image-%d", index), producerRecipe,
		)
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_GENERATED"}) ||
			!slices.Equal(set.ObjectPaths, []string{"include/generated/completed-cache.h"}) {
			t.Fatalf("generated producer variant %d dependency set = %#v", index, set)
		}
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.misses != 2 ||
		cache.configDependencies.completed.stores != 2 {
		t.Fatalf("generated-producer-change stats = hits %d misses %d stores %d, want 0/2/2",
			cache.configDependencies.completed.hits,
			cache.configDependencies.completed.misses,
			cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencyCompletedCompilerCacheDoesNotReuseOpaqueResults(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index := range 2 {
		plan, node := configDependencyCompletedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), fmt.Sprintf("image-%d", index), "target-toolset",
			map[string]string{"CONFIG_RELEVANT": "y"},
		)
		recipe := plan.Recipes[node.Recipe]
		recipe.Stdin = "source:source:00000000"
		plan.Recipes[node.Recipe] = recipe
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if !set.Opaque || !strings.Contains(set.Reason, "stdin") {
			t.Fatalf("opaque variant %d dependency set = %#v", index, set)
		}
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.stores != 0 ||
		cache.configDependencies.completed.entryCount != 0 {
		t.Fatalf("opaque-result cache stats = hits %d stores %d entries %d, want 0/0/0",
			cache.configDependencies.completed.hits,
			cache.configDependencies.completed.stores,
			cache.configDependencies.completed.entryCount)
	}
}

func TestConfigDependencyCompletedCompilerCacheMissesWhenLiveRecipeBecomesOpaque(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	first, _ := configDependencyCompletedCachePlanForTest(
		t, root, "variant-0", "compile-0", "src-00000001", "image-0", "target-toolset",
		map[string]string{"CONFIG_RELEVANT": "y"},
	)
	configDependencyBuildWithFamilyCacheForTest(t, first, cache)

	second, node := configDependencyCompletedCachePlanForTest(
		t, root, "variant-1", "compile-1", "src-00000002", "image-1", "target-toolset",
		map[string]string{"CONFIG_RELEVANT": "y"},
	)
	recipe := second.Recipes[node.Recipe]
	recipe.Stdin = "source:source:00000000"
	second.Recipes[node.Recipe] = recipe
	set := configDependencyBuildWithFamilyCacheForTest(t, second, cache)[node.ID]
	if !set.Opaque || !strings.Contains(set.Reason, "stdin") {
		t.Fatalf("mutated sibling dependency set = %#v, want live opaque result", set)
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.stores != 1 {
		t.Fatalf("live-recipe mutation cache stats = hits %d stores %d, want 0/1",
			cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}

func TestConfigDependencyCompletedCompilerCacheRejectsPathSensitiveAncestry(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#include <completed-cache.h>\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index := range 2 {
		plan, node := configDependencyCompletedGeneratedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), fmt.Sprintf("producer-%d", index),
			fmt.Sprintf("image-%d", index), ActionRecipe{
				Tool: "actionfile", Arguments: []string{
					"-out", "${output:00000000}", "-line", "CONFIG_GENERATED",
				},
			},
		)
		if err := plan.markPathSensitiveArchiveOutput(plan.Nodes[0].ID, 0); err != nil {
			t.Fatal(err)
		}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque {
			t.Fatalf("path-sensitive cache bypass changed live dependency result: %#v", set)
		}
	}
	if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.stores != 0 ||
		cache.configDependencies.completed.entryCount != 0 {
		t.Fatalf("path-sensitive cache stats = hits %d stores %d entries %d, want 0/0/0",
			cache.configDependencies.completed.hits,
			cache.configDependencies.completed.stores,
			cache.configDependencies.completed.entryCount)
	}
}

func TestConfigDependencyCompletedCompilerCacheIsBounded(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "CONFIG_RELEVANT\n")
	cache := NewActionPlanFamilyPlanningCache()
	cache.configDependencies.completed.maximumEntries = 1
	for index, toolset := range []string{"toolset-a", "toolset-b", "toolset-b"} {
		plan, _ := configDependencyCompletedCachePlanForTest(
			t, root, fmt.Sprintf("variant-%d", index), fmt.Sprintf("compile-%d", index),
			fmt.Sprintf("src-%08d", index+1), "image", toolset,
			map[string]string{"CONFIG_RELEVANT": "y"},
		)
		configDependencyBuildWithFamilyCacheForTest(t, plan, cache)
	}
	completed := &cache.configDependencies.completed
	if completed.entryCount != 1 || completed.stores != 1 || completed.hits != 0 || completed.misses != 3 {
		t.Fatalf("bounded completed-cache stats = entries %d stores %d hits %d misses %d, want 1/1/0/3",
			completed.entryCount, completed.stores, completed.hits, completed.misses)
	}
}

func TestConfigDependencySharedCacheReusesImmutableIncludeDirectoryAcrossSiblingPlans(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/alpha.h", "#define ALPHA 1\n")
	mustWriteSource(t, root, "include/nested/beta.h", "#define BETA 1\n")
	profile := CompactKbuildProfile{
		Name: "family-shared-include-directory",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	arguments := []string{"-I", "${tree:kernel}/include"}
	shared := NewActionPlanConfigDependencySharedCache()
	walkCalls := 0
	shared.sourceIncludePaths.walkDir = func(root string, visit fs.WalkDirFunc) error {
		walkCalls++
		return filepath.WalkDir(root, visit)
	}
	want := []string{"include/alpha.h", "include/nested/beta.h"}
	for sibling := range 2 {
		context := newConfigDependencyAnalysisContextWithCache(nil, shared)
		if context.sourceIncludePaths != shared.sourceIncludePaths {
			t.Fatalf("sibling plan %d did not retain the family include-directory cache", sibling)
		}
		siblingProfile := profile
		siblingProfile.Name = fmt.Sprintf("family-variant-%d", sibling)
		siblingProfile.evaluator = &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}}
		for linkNode := range 3 {
			got, err := configDependencyExplicitSourceIncludePathsWithCache(
				siblingProfile, arguments, context.sourceIncludePaths,
			)
			if err != nil {
				t.Fatalf("sibling plan %d link node %d: %v", sibling, linkNode, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("sibling plan %d link node %d include paths = %q, want %q", sibling, linkNode, got, want)
			}
			// Callers receive their own slice; mutating one result must not poison
			// later link nodes or sibling configurations.
			got[0] = "mutated-by-caller"
		}
	}
	if walkCalls != 1 {
		t.Fatalf("family-shared include directory walks = %d, want one across six link-node analyses", walkCalls)
	}
	if got := len(shared.sourceIncludePaths.entries); got != 1 {
		t.Fatalf("family-shared include-directory cache entries = %d, want one", got)
	}
}

func TestConfigDependencyAnalysisScansDriverLinkClosureWithoutDirectorySnapshot(t *testing.T) {
	arguments := []string{
		"-I", "${tree:kernel}/include",
		"-o", "tools/helper",
		"${tree:kernel}/drivers/example/driver.c",
	}
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <shared.h>\nint main(void) { return SHARED; }\n",
		"include/shared.h":         "#define SHARED 1\n",
		"include/unrelated.h":      "#define UNRELATED 1\n",
	}, arguments, nil)
	node.Outputs = []ActionPlanOutput{{Tree: "host", Path: "tools/helper"}}
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Tool = compactKbuildScriptRunnerRole
	recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
		Tool:                     "cc",
		Arguments:                arguments,
		WorkingInputUses:         []string{"source:source:00000000"},
		WorkingInputUsesComplete: true,
	}
	recipe.Sources = []string{"source:00000000"}
	recipe.WorkingInputs = map[string]string{
		"source:source:00000000": "drivers/example/driver.c",
	}
	plan.Recipes[node.Recipe] = recipe
	linkRole, ok := toolaction.LinkContractRole("cc")
	if !ok {
		t.Fatal("cc link contract is unavailable")
	}
	baseRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: "cc"}
	linkRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: linkRole}
	plan.metadata.actionRoles = []KbuildActionRoleRef{baseRef, linkRef}
	plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
		baseRef: {},
		linkRef: {},
	}
	secondNode := node
	secondNode.ID = "compile-provisional-second-link"
	secondNode.Outputs = []ActionPlanOutput{{Tree: "host", Path: "tools/helper-second"}}
	plan.Nodes = append(plan.Nodes, secondNode)
	profileName := ""
	for name := range plan.selectionGraph.profiles {
		profileName = name
	}
	secondSelection := compactKbuildSelectionKey{
		profile: profileName, target: "tools/helper-second", stage: secondNode.Stage,
	}
	plan.selectionGraph.selections[secondSelection] = CompactKbuildSelection{Profile: profileName}
	plan.selectionGraph.materializedProducers[secondSelection] = secondNode.ID

	// Independently lowered family variants can share immutable parsed source
	// syntax, but neither may substitute an entire include directory for the
	// actual preprocessing closure.
	sibling := *plan
	sibling.Sources = slices.Clone(plan.Sources)
	sibling.Nodes = slices.Clone(plan.Nodes)
	sibling.Recipes = maps.Clone(plan.Recipes)
	sibling.invalidateLookupIndexes()

	shared := NewActionPlanConfigDependencySharedCache()
	walkCalls := 0
	shared.sourceIncludePaths.walkDir = func(root string, visit fs.WalkDirFunc) error {
		walkCalls++
		return filepath.WalkDir(root, visit)
	}
	for index, candidate := range []*ActionPlan{plan, &sibling} {
		context := newConfigDependencyAnalysisContextWithCache(candidate, shared)
		for linkIndex, linkNode := range candidate.Nodes {
			set, err := analyzeActionPlanNodeConfigDependencies(candidate, linkNode, context)
			if err != nil {
				t.Fatalf("family plan %d link node %d dependency analysis: %v", index, linkIndex, err)
			}
			if set.Opaque {
				t.Fatalf("family plan %d link node %d dependency set = %#v, want precise driver link", index, linkIndex, set)
			}
			want := []string{"drivers/example/driver.c", "include/shared.h"}
			if !slices.Equal(set.SourcePaths, want) {
				t.Fatalf("family plan %d link node %d source closure = %q, want %q", index, linkIndex, set.SourcePaths, want)
			}
		}
		if got := context.sourcePathInterns.passes; got != 0 {
			t.Fatalf("family plan %d broad include-snapshot intern passes = %d, want none", index, got)
		}
	}
	if walkCalls != 0 {
		t.Fatalf("driver-link include directory walks = %d, want exact file lookup only", walkCalls)
	}
	if len(shared.parsed) != 2 {
		t.Fatalf("driver-link shared syntax entries = %d, want one TU and its one included header", len(shared.parsed))
	}
}

func TestConfigDependencySharedCacheRetainsImmutableIncludeDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "include/header.h", "#define HEADER 1\n")
	profile := CompactKbuildProfile{
		Name: "family-shared-include-error",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	shared := NewActionPlanConfigDependencySharedCache()
	walkFailure := errors.New("synthetic immutable include walk failure")
	walkCalls := 0
	shared.sourceIncludePaths.walkDir = func(string, fs.WalkDirFunc) error {
		walkCalls++
		return walkFailure
	}
	for sibling := range 2 {
		context := newConfigDependencyAnalysisContextWithCache(nil, shared)
		_, err := configDependencyExplicitSourceIncludePathsWithCache(
			profile, []string{"-I${tree:kernel}/include"}, context.sourceIncludePaths,
		)
		if !errors.Is(err, walkFailure) {
			t.Fatalf("sibling plan %d include error = %v, want cached walk failure", sibling, err)
		}
	}
	if walkCalls != 1 {
		t.Fatalf("failed immutable include directory walks = %d, want one cached failure", walkCalls)
	}
	if got := len(shared.sourceIncludePaths.entries); got != 1 {
		t.Fatalf("failed include-directory cache entries = %d, want one", got)
	}
}

func TestConfigDependencySharedCacheSeparatesImmutableIncludeMountTables(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	mustWriteSource(t, firstRoot, "include/first.h", "#define FIRST 1\n")
	mustWriteSource(t, secondRoot, "include/second.h", "#define SECOND 1\n")
	profile := func(name, root string) CompactKbuildProfile {
		return CompactKbuildProfile{
			Name: name,
			evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
				"__LINUX_BZL_SOURCE_TREE__": root,
			}}},
		}
	}
	shared := NewActionPlanConfigDependencySharedCache()
	walkCalls := 0
	shared.sourceIncludePaths.walkDir = func(root string, visit fs.WalkDirFunc) error {
		walkCalls++
		return filepath.WalkDir(root, visit)
	}
	for _, test := range []struct {
		profile CompactKbuildProfile
		want    []string
	}{
		{profile: profile("first", firstRoot), want: []string{"include/first.h"}},
		{profile: profile("second", secondRoot), want: []string{"include/second.h"}},
	} {
		got, err := configDependencyExplicitSourceIncludePathsWithCache(
			test.profile, []string{"-I${tree:kernel}/include"}, shared.sourceIncludePaths,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, test.want) {
			t.Fatalf("profile %q include paths = %q, want %q", test.profile.Name, got, test.want)
		}
	}
	if walkCalls != 2 || len(shared.sourceIncludePaths.entries) != 2 {
		t.Fatalf("distinct mount tables produced walks=%d entries=%d, want 2/2", walkCalls, len(shared.sourceIncludePaths.entries))
	}
}

func TestActionPlanConfigDependencyInternSourcePathsPreservesNamespaceConflicts(t *testing.T) {
	const pathname = "include/linux/shared.h"
	for _, test := range []struct {
		name     string
		sources  []ActionPlanSource
		want     bool
		wantSize int
	}{
		{
			name: "matching immutable namespace",
			sources: []ActionPlanSource{{
				ID: "src-00000001", Namespace: "kernel", Path: pathname,
			}},
			want: true, wantSize: 1,
		},
		{
			name: "config namespace does not conflict",
			sources: []ActionPlanSource{{
				ID: "src-00000001", Namespace: "config", Path: pathname,
			}},
			want: true, wantSize: 2,
		},
		{
			name: "different immutable namespace conflicts",
			sources: []ActionPlanSource{{
				ID: "src-00000001", Namespace: "vendor", Path: pathname,
			}},
			want: false, wantSize: 1,
		},
		{
			name: "multiple immutable namespaces conflict",
			sources: []ActionPlanSource{
				{ID: "src-00000001", Namespace: "kernel", Path: pathname},
				{ID: "src-00000002", Namespace: "vendor", Path: pathname},
			},
			want: false, wantSize: 2,
		},
		{
			name: "canonicalized different namespace conflicts",
			sources: []ActionPlanSource{{
				ID: "src-00000001", Namespace: "vendor", Path: "include/linux/../linux/shared.h",
			}},
			want: false, wantSize: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &ActionPlan{Sources: slices.Clone(test.sources)}
			if got := actionPlanConfigDependencyInternSourcePaths(plan, []string{pathname}); got != test.want {
				t.Fatalf("intern source paths = %t, want %t", got, test.want)
			}
			if got := len(plan.Sources); got != test.wantSize {
				t.Fatalf("interned source count = %d, want %d", got, test.wantSize)
			}
		})
	}

	t.Run("completed snapshot invalidates after external source append", func(t *testing.T) {
		plan := &ActionPlan{}
		cache := newConfigDependencyPlanSourcePathInternCache()
		snapshot := configDependencySourceIncludeSnapshot{
			key: configDependencyExplicitSourceIncludeCacheKey{
				bindings: "test", directories: "test-include-snapshot",
			},
			paths: []string{pathname},
		}
		if !cache.intern(plan, snapshot) || !cache.intern(plan, snapshot) {
			t.Fatal("unchanged include snapshot was not admitted")
		}
		if cache.passes != 1 {
			t.Fatalf("unchanged include snapshot intern passes = %d, want one", cache.passes)
		}
		plan.Sources = append(plan.Sources, ActionPlanSource{
			ID: "src-00000002", Namespace: "vendor", Path: pathname,
		})
		if cache.intern(plan, snapshot) {
			t.Fatal("completed include snapshot survived a conflicting namespace append")
		}
		if cache.passes != 2 {
			t.Fatalf("namespace-conflict revalidation passes = %d, want two", cache.passes)
		}
		if cache.intern(plan, snapshot) {
			t.Fatal("cached namespace conflict was not retained")
		}
		if cache.passes != 2 {
			t.Fatalf("cached namespace conflict repeated the intern pass: %d", cache.passes)
		}
	})
}

func TestConfigDependencyCompletedInternSourcePathsIsAtomic(t *testing.T) {
	const conflict = "include/linux/z-conflict.h"
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: "src-00000001", Namespace: "vendor", Path: conflict,
		}},
		metadata: &CompactMetadata{},
	}
	cache := newConfigDependencyPlanSourcePathInternCache()
	before := slices.Clone(plan.Sources)
	if configDependencyCompletedInternSourcePaths(plan, cache, []string{
		"include/linux/a-new.h",
		conflict,
	}) {
		t.Fatal("conflicting completed compiler closure was admitted")
	}
	if !slices.Equal(plan.Sources, before) {
		t.Fatalf("failed admission changed source table: got %#v, want %#v", plan.Sources, before)
	}
	if _, added := plan.sourceIDs[actionPlanLookupKey("kernel", "include/linux/a-new.h")]; added {
		t.Fatal("failed admission installed the pre-conflict source lookup")
	}

	exhausted := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: "src-99999999", Namespace: "kernel", Path: "include/linux/existing.h",
		}},
		metadata: &CompactMetadata{},
	}
	if configDependencyCompletedInternSourcePaths(
		exhausted, newConfigDependencyPlanSourcePathInternCache(),
		[]string{"include/linux/new.h"},
	) {
		t.Fatal("source-ID-exhausted compiler closure was admitted")
	}
	if len(exhausted.Sources) != 1 {
		t.Fatalf("source-ID exhaustion changed source table size to %d", len(exhausted.Sources))
	}
}

func TestConfigDependencyCompletedInternSourcePathsScalesWithAppends(t *testing.T) {
	const (
		initialSources = 2048
		admissions     = 256
	)
	plan := &ActionPlan{
		Sources:  make([]ActionPlanSource, 0, initialSources+admissions),
		metadata: &CompactMetadata{},
	}
	for index := range initialSources {
		plan.Sources = append(plan.Sources, ActionPlanSource{
			ID:        fmt.Sprintf("src-%08d", index+1),
			Namespace: "kernel",
			Path:      fmt.Sprintf("include/existing/%08d.h", index),
		})
	}
	cache := newConfigDependencyPlanSourcePathInternCache()
	closure := make([]string, 0, admissions)
	for index := range admissions {
		closure = append(closure, fmt.Sprintf("include/discovered/%08d.h", index))
		if !configDependencyCompletedInternSourcePaths(plan, cache, closure) {
			t.Fatalf("growing closure %d was not admitted", index)
		}
	}
	if got, want := len(plan.Sources), initialSources+admissions; got != want {
		t.Fatalf("interned source count = %d, want %d", got, want)
	}
	// The initial source table is indexed once. Sources appended by this cache
	// update both indexes directly, so later growing closures never scan the
	// already-growing ActionPlan.Sources table again.
	if got := cache.sourceVisits; got != initialSources {
		t.Fatalf("incremental source-index visits = %d, want %d", got, initialSources)
	}
	if got := plan.sourceLookupCount; got != len(plan.Sources) {
		t.Fatalf("action-plan source lookup count = %d, want %d", got, len(plan.Sources))
	}
}

func TestConfigDependencyCompilerBindingProjectionScalesWithReferences(t *testing.T) {
	const inputCount = 4096
	plan := &ActionPlan{
		Sources: []ActionPlanSource{{
			ID: "src-00000001", Namespace: "kernel", Path: "drivers/example.c",
		}},
		Recipes: map[string]ActionRecipe{},
		Nodes:   make([]ActionPlanNode, inputCount),
	}
	inputs := make([]ActionPlanNodeEdge, inputCount)
	for ordinal := range inputCount {
		producerID := "producer-" + planOrdinal(ordinal)
		plan.Nodes[ordinal] = ActionPlanNode{
			ID: producerID,
			Outputs: []ActionPlanOutput{{
				Tree: "objects", Path: "generated/" + planOrdinal(ordinal) + ".h",
			}},
		}
		inputs[ordinal] = ActionPlanNodeEdge{
			Role: "working", ProducerID: producerID,
		}
	}
	lastBinding := "working:" + planOrdinal(inputCount-1)
	node := ActionPlanNode{
		ID: "consumer",
		Sources: []ActionPlanSourceEdge{{
			Role: "translation", SourceID: "src-00000001",
		}},
		Inputs: inputs,
	}
	recipe := ActionRecipe{CompilerInvocation: &ActionRecipeCompilerInvocation{
		Arguments: []string{
			"${source:translation:00000000}",
			"${input:" + lastBinding + "}",
		},
		WorkingInputUses: []string{"input:" + lastBinding},
	}}

	sources, generated := configDependencyCompilerReferencedBindings(
		plan, node, recipe, recipe.CompilerInvocation.Arguments,
	)
	if got, want := len(sources), 1; got != want {
		t.Fatalf("projected source bindings = %d, want %d", got, want)
	}
	if got, want := len(generated), 1; got != want {
		t.Fatalf("projected generated bindings = %d, want %d", got, want)
	}
	if got := sources["${source:translation:00000000}"].Path; got != "drivers/example.c" {
		t.Fatalf("projected source path = %q", got)
	}
	if got := generated["${input:"+lastBinding+"}"].producer.ID; got != "producer-"+planOrdinal(inputCount-1) {
		t.Fatalf("projected generated producer = %q", got)
	}
}

func TestConfigDependencySharedParsedCacheSeparatesCompilerLanguages(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "drivers", "example", "shared.input")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte(".include \"fragment.S\"\nCONFIG_SHARED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := CompactKbuildProfile{
		Name: "family-shared-language-cache",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	for _, order := range [][]string{
		{"c", "assembler-with-cpp"},
		{"assembler-with-cpp", "c"},
	} {
		name := strings.Join(order, "-then-")
		t.Run(name, func(t *testing.T) {
			shared := NewActionPlanConfigDependencySharedCache()
			analyze := func(language string) ConfigDependencySet {
				context := newConfigDependencyAnalysisContextWithCache(nil, shared)
				scanner := configDependencyPhysicalFileCacheScannerForTest(profile, context.physicalFiles)
				scanner.language = language
				scanner.parsed = context.parsed
				scanner.conditionalSyntax = context.conditionalSyntax
				file, ok := scanner.sourceFile("drivers/example/shared.input")
				if !ok {
					t.Fatalf("%s scanner did not resolve shared source", language)
				}
				scanner.queue(file)
				return scanner.scan()
			}
			sets := map[string]ConfigDependencySet{}
			for _, language := range order {
				sets[language] = analyze(language)
			}
			cSet := sets["c"]
			if cSet.Opaque || !slices.Equal(cSet.Symbols, []string{"CONFIG_SHARED"}) {
				t.Fatalf("C dependency set = %#v", cSet)
			}
			asmSet := sets["assembler-with-cpp"]
			if !asmSet.Opaque || !strings.Contains(asmSet.Reason, "assembler file-input directive .include") {
				t.Fatalf("assembler dependency set = %#v, want language-specific opaque fallback", asmSet)
			}
			if len(shared.parsed) != 2 {
				t.Fatalf("language-keyed shared parsed-file entries = %d, want two", len(shared.parsed))
			}
		})
	}
}

func BenchmarkConfigDependencyPhysicalFileStatusAcrossCompilerNodes(b *testing.B) {
	const compilerNodeCount = 128
	root := b.TempDir()
	physical := filepath.Join(root, "include", "linux", "shared.h")
	if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("CONFIG_SHARED\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	profile := CompactKbuildProfile{
		Name: "physical-file-cache-benchmark",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		}}},
	}
	for _, benchmark := range []struct {
		name   string
		shared bool
	}{
		{name: "cached-analysis-context", shared: true},
		{name: "uncached-reference", shared: false},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(compilerNodeCount, "compiler-nodes/op")
			total := 0
			for range b.N {
				shared := newConfigDependencyPhysicalFileCache()
				for range compilerNodeCount {
					cache := shared
					if !benchmark.shared {
						cache = newConfigDependencyPhysicalFileCache()
					}
					scanner := configDependencyPhysicalFileCacheScannerForTest(profile, cache)
					file, ok := scanner.sourceFile("include/linux/shared.h")
					if !ok {
						b.Fatal("benchmark source file was not resolved")
					}
					scanner.queue(file)
					total += len(scanner.pending)
				}
			}
			if total == 0 {
				b.Fatal("physical-file cache benchmark queued no inputs")
			}
		})
	}
}

func BenchmarkConfigDependencyPreconfiguredPathsAcrossCompilerNodes(b *testing.B) {
	const (
		pathCount         = 2048
		compilerNodeCount = 128
	)
	profile := CompactKbuildProfile{
		Name: "config-dependency-benchmark",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: map[string]string{
			"__LINUX_BZL_OBJECT_TREE__": b.TempDir(),
		}}},
	}
	metadata := &CompactMetadata{
		preconfiguredObjectTree: true,
		exactSourcePaths:        make(map[string]string, pathCount),
	}
	for index := range pathCount {
		metadata.exactSourcePaths[fmt.Sprintf("include/benchmark/path-%05d.h", index)] = "prep"
	}
	generated := map[string]bool{"drivers/benchmark/driver.o": true}
	newContext := func() *configDependencyAnalysisContext {
		return &configDependencyAnalysisContext{
			generated:           generated,
			preconfiguredByRoot: map[string]map[string]string{},
		}
	}

	for _, benchmark := range []struct {
		name  string
		paths func(*configDependencyAnalysisContext) map[string]string
	}{
		{
			name: "cached-analysis-context",
			paths: func(context *configDependencyAnalysisContext) map[string]string {
				return context.preconfiguredConfigDependencyPaths(profile, metadata)
			},
		},
		{
			name: "uncached-reference",
			paths: func(_ *configDependencyAnalysisContext) map[string]string {
				return actionPlanPreconfiguredConfigDependencyPaths(profile, metadata, generated)
			},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(pathCount, "sdk-paths/op")
			b.ReportMetric(compilerNodeCount, "compiler-nodes/op")
			total := 0
			for range b.N {
				context := newContext()
				for range compilerNodeCount {
					total += len(benchmark.paths(context))
				}
			}
			if total == 0 {
				b.Fatal("preconfigured path benchmark produced no paths")
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesDoesNotPruneUndumpedDynamicBuiltin(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/compiler-version.h": `#ifdef __FILE__
#include ACTUAL_FILE_DEFINED_BRANCH
#else
#include "incorrect-undefined-branch.h"
#endif
`,
		"include/linux/incorrect-undefined-branch.h": "CONFIG_INCORRECT\n",
	}, []string{
		"-nostdinc", "-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		// GCC and Clang omit __FILE__ from this otherwise complete dump even
		// though the actual #ifdef branch is true.
		return "", true, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-literal") {
		t.Fatalf("undumped __FILE__ dependency set = %#v, want conservative union", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRegistersCompilerPredefinesDuringDiscovery(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":         "CONFIG_DRIVER\n",
		"include/linux/compiler-version.h": "#ifdef COMPILER_SELECTED\n#endif\n",
	}, []string{
		"-nostdinc", "-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	called := false
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		called = true
		return "", false, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !set.Opaque || !strings.Contains(set.Reason, "unavailable during discovery") {
		t.Fatalf("compiler-predefine discovery dependency set = %#v, called=%t", set, called)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAdvancesCompilerStateAcrossForcedHeaders(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":         "CONFIG_DRIVER\n",
		"include/linux/compiler-version.h": "#ifdef COMPILER_SELECTED\n#define FIRST_SELECTED\n#endif\n",
		"include/linux/later-forced.h": `#ifdef COMPILER_SELECTED
#include "selected.h"
#else
#include UNMODELED_LATER_HEADER
#endif
`,
		"include/linux/selected.h": "CONFIG_SELECTED\n",
	}, []string{
		"-nostdinc",
		"-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-include", "${tree:kernel}/include/linux/later-forced.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SELECTED"}) {
		t.Fatalf("later forced-header dependency set = %#v, want ordered compiler-state pruning", set)
	}
}

func TestConfigDependencyResolvedAutoconfDefinitionsMatchGeneratedHeader(t *testing.T) {
	state, reason := parseConfigDependencyCompilerPredefines("#define CONFIG_OFF 1\n#define CONFIG_EMPTY 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	state.set("__GENERATED_AUTOCONF_H__", configDependencyMacroUndefined)
	state.applyResolvedConfigAutoconf(newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
		"CONFIG_ENABLED": "y",
		"CONFIG_MODULE":  "m",
		"CONFIG_SCALAR":  "17",
		"CONFIG_STRING":  `"value"`,
		"CONFIG_OFF":     "n",
		"CONFIG_EMPTY":   "",
	}))
	for _, name := range []string{
		"CONFIG_ENABLED", "CONFIG_MODULE_MODULE", "CONFIG_SCALAR", "CONFIG_STRING",
		"CONFIG_OFF", "CONFIG_EMPTY",
	} {
		if got := state.definition(name); got != configDependencyMacroDefined {
			t.Errorf("autoconf macro %s = %d, want defined", name, got)
		}
	}
	if got := state.definition("CONFIG_MODULE"); got != configDependencyMacroUndefined {
		t.Fatalf("m-valued base macro = %d, want undefined", got)
	}
}

func TestConfigDependencyAnalysisContextSnapshotsResolvedAutoconfDefinitions(t *testing.T) {
	fragment := map[string]string{
		"CONFIG_DISABLED": "n",
		"CONFIG_ENABLED":  "y",
		"CONFIG_EMPTY":    "",
		"CONFIG_MODULE":   "m",
		"NOT_CONFIG":      "y",
	}
	plan := &ActionPlan{metadata: &CompactMetadata{configFragment: fragment}}
	context := newConfigDependencyAnalysisContext(plan)
	if got, want := context.autoconfDefinitions.names, []string{
		"CONFIG_ENABLED", "CONFIG_MODULE_MODULE",
	}; !slices.Equal(got, want) {
		t.Fatalf("precomputed autoconf definitions = %#v, want %#v", got, want)
	}
	if !context.autoconfDefinitions.cacheDigestReady {
		t.Fatal("analysis context did not precompute the autoconf cache digest")
	}

	// The analysis context owns an immutable snapshot. Later mutation of the
	// source map cannot change definitions replayed by another translation unit.
	fragment["CONFIG_ENABLED"] = "n"
	fragment["CONFIG_LATE"] = "y"
	state, reason := parseConfigDependencyCompilerPredefines("")
	if reason != "" {
		t.Fatal(reason)
	}
	state.applyResolvedConfigAutoconf(context.autoconfDefinitions)
	for _, name := range []string{"CONFIG_ENABLED", "CONFIG_MODULE_MODULE"} {
		if got := state.definition(name); got != configDependencyMacroDefined {
			t.Errorf("snapshotted autoconf macro %s = %d, want defined", name, got)
		}
	}
	if got := state.definition("CONFIG_LATE"); got != configDependencyMacroUndefined {
		t.Fatalf("post-snapshot autoconf macro = %d, want undefined", got)
	}
}

// applyResolvedConfigAutoconfEagerForTest inserts emitted names one by one. It
// is intentionally kept in tests as a small semantic oracle for the snapshot
// batch operation.
func applyResolvedConfigAutoconfEagerForTest(
	state *configDependencyMacroState,
	definitions configDependencyResolvedAutoconfDefinitions,
) {
	const guard = "__GENERATED_AUTOCONF_H__"
	guardState, recorded := state.explicitDefinition(guard)
	if !recorded {
		guardState = configDependencyMacroUndefined
	}
	if guardState == configDependencyMacroDefined {
		return
	}
	apply := func(branch *configDependencyMacroState) {
		branch.set(guard, configDependencyMacroDefined)
		for _, name := range definitions.names {
			branch.set(name, configDependencyMacroDefined)
		}
	}
	if guardState == configDependencyMacroUnknown {
		skipped := state.branch()
		skipped.set(guard, configDependencyMacroDefined)
		applied := state.branch()
		apply(applied)
		state.mergePossibleBranches(skipped, applied)
		return
	}
	apply(state)
}

func assertConfigDependencyMacroStatesEquivalent(
	t *testing.T,
	stage string,
	bulk, eager *configDependencyMacroState,
	names []string,
) {
	t.Helper()
	for _, name := range names {
		if got, want := bulk.definition(name), eager.definition(name); got != want {
			t.Errorf("%s: bulk %s = %d, eager = %d", stage, name, got, want)
		}
	}
	bulkProjection, bulkExact := configDependencyConditionalMacroStateKey(bulk)
	eagerProjection, eagerExact := configDependencyConditionalMacroStateKey(eager)
	if bulkExact != eagerExact || bulkProjection.key != eagerProjection.key {
		t.Errorf(
			"%s: bulk projection (%t, %q) != eager projection (%t, %q)",
			stage, bulkExact, bulkProjection.key, eagerExact, eagerProjection.key,
		)
	}
}

func TestConfigDependencyResolvedAutoconfBatchMatchesEagerOrderedSemantics(t *testing.T) {
	const guard = "__GENERATED_AUTOCONF_H__"
	definitions := newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
		"CONFIG_ALPHA":   "y",
		"CONFIG_BETA":    "m",
		"CONFIG_GAMMA":   "17",
		"CONFIG_OMITTED": "n",
	})
	names := []string{
		guard,
		"CONFIG_ALPHA",
		"CONFIG_BETA",
		"CONFIG_BETA_MODULE",
		"CONFIG_GAMMA",
		"CONFIG_OMITTED",
		"NON_CONFIG_GUARD",
	}

	for _, test := range []struct {
		name  string
		setup func(*configDependencyMacroState)
	}{
		{
			name: "before wildcard",
			setup: func(state *configDependencyMacroState) {
				state.set(guard, configDependencyMacroUndefined)
				state.set("CONFIG_ALPHA", configDependencyMacroUndefined)
			},
		},
		{
			name: "after wildcard with unknown guard",
			setup: func(state *configDependencyMacroState) {
				state.set("CONFIG_ALPHA", configDependencyMacroDefined)
				state.set("CONFIG_GAMMA", configDependencyMacroUndefined)
				state.applyValidatedNumericMacroHeader()
			},
		},
		{
			name: "after wildcard with explicit undefined guard",
			setup: func(state *configDependencyMacroState) {
				state.applyValidatedNumericMacroHeader()
				state.set(guard, configDependencyMacroUndefined)
			},
		},
		{
			name: "predefined guard skips",
			setup: func(state *configDependencyMacroState) {
				state.set(guard, configDependencyMacroDefined)
				state.set("CONFIG_ALPHA", configDependencyMacroUndefined)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bulk := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			eager := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			test.setup(bulk)
			test.setup(eager)

			bulk.applyResolvedConfigAutoconf(definitions)
			applyResolvedConfigAutoconfEagerForTest(eager, definitions)
			assertConfigDependencyMacroStatesEquivalent(t, "applied", bulk, eager, names)

			// Exact writes after the batch must win.
			bulk.set("CONFIG_ALPHA", configDependencyMacroUndefined)
			eager.set("CONFIG_ALPHA", configDependencyMacroUndefined)
			bulk.set("NON_CONFIG_GUARD", configDependencyMacroUndefined)
			eager.set("NON_CONFIG_GUARD", configDependencyMacroUndefined)
			assertConfigDependencyMacroStatesEquivalent(t, "later writes", bulk, eager, names)

			// A later wildcard does not affect CONFIG names and preserves a
			// definitely-defined synthetic guard, but may affect other names.
			bulk.applyValidatedNumericMacroHeader()
			eager.applyValidatedNumericMacroHeader()
			assertConfigDependencyMacroStatesEquivalent(t, "later wildcard", bulk, eager, names)

			// Persistent snapshots can be shared without aliasing later mutations.
			bulkClone := &configDependencyMacroState{snapshot: bulk.snapshot}
			eagerClone := &configDependencyMacroState{snapshot: eager.snapshot}
			bulkClone.set("CONFIG_GAMMA", configDependencyMacroUndefined)
			eagerClone.set("CONFIG_GAMMA", configDependencyMacroUndefined)
			assertConfigDependencyMacroStatesEquivalent(t, "clone", bulkClone, eagerClone, names)
			if bulk.definition("CONFIG_GAMMA") != eager.definition("CONFIG_GAMMA") {
				t.Fatal("clone mutation leaked into bulk parent")
			}
		})
	}
}

func TestConfigDependencyResolvedAutoconfBoundaryBranchCommitAndMerge(t *testing.T) {
	definitions := newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
		"CONFIG_ALPHA": "y",
		"CONFIG_BETA":  "y",
	})
	names := []string{
		"__GENERATED_AUTOCONF_H__", "CONFIG_ALPHA", "CONFIG_BETA", "AFTER_HEADER",
	}
	newPair := func() (*configDependencyMacroState, *configDependencyMacroState) {
		return &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()},
			&configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	}

	t.Run("commit", func(t *testing.T) {
		bulk, eager := newPair()
		bulkChild, eagerChild := bulk.branch(), eager.branch()
		bulkChild.applyResolvedConfigAutoconf(definitions)
		applyResolvedConfigAutoconfEagerForTest(eagerChild, definitions)
		bulkChild.set("AFTER_HEADER", configDependencyMacroDefined)
		eagerChild.set("AFTER_HEADER", configDependencyMacroDefined)
		if !bulk.commitBranch(bulkChild) || !eager.commitBranch(eagerChild) {
			t.Fatal("exact autoconf child did not commit")
		}
		assertConfigDependencyMacroStatesEquivalent(t, "commit", bulk, eager, names)
	})

	t.Run("possible branch", func(t *testing.T) {
		bulk, eager := newPair()
		bulkChild, eagerChild := bulk.branch(), eager.branch()
		bulkChild.applyResolvedConfigAutoconf(definitions)
		applyResolvedConfigAutoconfEagerForTest(eagerChild, definitions)
		bulk.mergePossibleBranch(bulkChild)
		eager.mergePossibleBranch(eagerChild)
		assertConfigDependencyMacroStatesEquivalent(t, "possible branch", bulk, eager, names)
	})

	t.Run("two branches", func(t *testing.T) {
		bulk, eager := newPair()
		bulkLeft, bulkRight := bulk.branch(), bulk.branch()
		eagerLeft, eagerRight := eager.branch(), eager.branch()
		bulkLeft.applyResolvedConfigAutoconf(definitions)
		bulkRight.applyResolvedConfigAutoconf(definitions)
		applyResolvedConfigAutoconfEagerForTest(eagerLeft, definitions)
		applyResolvedConfigAutoconfEagerForTest(eagerRight, definitions)
		bulkLeft.set("AFTER_HEADER", configDependencyMacroDefined)
		eagerLeft.set("AFTER_HEADER", configDependencyMacroDefined)
		bulk.mergePossibleBranches(bulkLeft, bulkRight)
		eager.mergePossibleBranches(eagerLeft, eagerRight)
		assertConfigDependencyMacroStatesEquivalent(t, "two branches", bulk, eager, names)
	})
}

func TestConfigDependencyResolvedAutoconfCacheKeyIsPrecomputed(t *testing.T) {
	fragment := make(map[string]string, 65536)
	for index := 0; index < 65536; index++ {
		fragment[fmt.Sprintf("CONFIG_ENABLED_%06d", index)] = "y"
	}
	definitions := newConfigDependencyResolvedAutoconfDefinitions(fragment)
	want := configDependencyAutoconfNamesDigest(definitions.names)
	if definitions.cacheDigest != want || configDependencyForcedHeaderAutoconfKey(definitions) != want {
		t.Fatal("precomputed autoconf cache digest differs from canonical name serialization")
	}
	var got [sha256.Size]byte
	if allocations := testing.AllocsPerRun(1000, func() {
		got = configDependencyForcedHeaderAutoconfKey(definitions)
	}); allocations != 0 {
		t.Fatalf("precomputed per-action autoconf key allocated %.1f objects, want 0", allocations)
	}
	if got != want {
		t.Fatal("precomputed autoconf key changed during allocation measurement")
	}
}

func BenchmarkConfigDependencyResolvedAutoconfApplication(b *testing.B) {
	for _, test := range []struct {
		enabled  int
		disabled int
	}{
		{enabled: 1},
		{enabled: 1, disabled: 65536},
		{enabled: 64, disabled: 65536},
		{enabled: 1024, disabled: 65536},
		{enabled: 65536},
	} {
		name := fmt.Sprintf("enabled-%d/disabled-%d", test.enabled, test.disabled)
		b.Run(name, func(b *testing.B) {
			fragment := make(map[string]string, test.enabled+test.disabled)
			for index := 0; index < test.enabled; index++ {
				fragment[fmt.Sprintf("CONFIG_ENABLED_%06d", index)] = "y"
			}
			for index := 0; index < test.disabled; index++ {
				fragment[fmt.Sprintf("CONFIG_DISABLED_%06d", index)] = "n"
			}
			// Filtering deliberately precedes ResetTimer: the timed path is the
			// per-translation-unit replay and must scale with enabled definitions,
			// independently of total fragment cardinality.
			definitions := newConfigDependencyResolvedAutoconfDefinitions(fragment)
			if len(definitions.names) != test.enabled {
				b.Fatalf("precomputed definitions = %d, want %d", len(definitions.names), test.enabled)
			}
			sentinel := fmt.Sprintf("CONFIG_ENABLED_%06d", test.enabled-1)
			base := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			var state *configDependencyMacroState
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				state = base.branch()
				state.applyResolvedConfigAutoconf(definitions)
			}
			b.StopTimer()
			b.ReportMetric(float64(test.enabled), "enabled_macros")
			b.ReportMetric(float64(len(fragment)), "fragment_entries")
			if state == nil || state.definition(sentinel) != configDependencyMacroDefined {
				got := configDependencyMacroUnknown
				if state != nil {
					got = state.definition(sentinel)
				}
				b.Fatalf("last enabled macro = %d, want defined", got)
			}
		})
	}
}

func BenchmarkConfigDependencyForcedHeaderCacheKeyAutoconfCardinality(b *testing.B) {
	for _, enabled := range []int{1, 1024, 65536} {
		b.Run(fmt.Sprintf("enabled-%d", enabled), func(b *testing.B) {
			fragment := make(map[string]string, enabled)
			for index := 0; index < enabled; index++ {
				fragment[fmt.Sprintf("CONFIG_ENABLED_%06d", index)] = "y"
			}
			definitions := newConfigDependencyResolvedAutoconfDefinitions(fragment)
			scanner := configDependencyClosureScanner{
				sourceLookup: newConfigDependencySourceLookup(CompactKbuildProfile{Name: "benchmark"}),
				language:     "c",
			}
			var key configDependencyForcedHeaderCacheKey
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				key = newConfigDependencyForcedHeaderCacheKey(
					&scanner, nil, definitions,
				)
			}
			b.StopTimer()
			if key.autoconfDefinitions != definitions.cacheDigest {
				b.Fatal("per-action key did not reuse precomputed autoconf serialization")
			}
			b.ReportMetric(float64(enabled), "enabled_macros")
		})
	}
}

func BenchmarkConfigDependencyForcedHeaderCacheLookupHitAutoconfCardinality(b *testing.B) {
	for _, enabled := range []int{1, 1024, 65536} {
		b.Run(fmt.Sprintf("enabled-%d", enabled), func(b *testing.B) {
			fragment := make(map[string]string, enabled)
			for index := 0; index < enabled; index++ {
				fragment[fmt.Sprintf("CONFIG_ENABLED_%06d", index)] = "y"
			}
			definitions := newConfigDependencyResolvedAutoconfDefinitions(fragment)
			baseScanner := configDependencyClosureScanner{
				sourceLookup: newConfigDependencySourceLookup(CompactKbuildProfile{Name: "benchmark"}),
				language:     "c",
			}
			key := newConfigDependencyForcedHeaderCacheKey(
				&baseScanner, nil, definitions,
			)
			root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			prefix := root.branch()
			prefix.applyResolvedConfigAutoconf(definitions)
			cachedSnapshot, valid := prefix.materializedSnapshot()
			if !valid {
				b.Fatal("could not materialize forced-header benchmark snapshot")
			}
			cache := configDependencyForcedHeaderCache{
				entries: map[configDependencyForcedHeaderCacheKey][]configDependencyForcedHeaderCacheEntry{
					key: {{inputSnapshot: root.snapshot, snapshot: cachedSnapshot, includeResolutionsReady: true}},
				},
			}
			var restored *configDependencyMacroState
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				scanner := baseScanner
				lookupKey := newConfigDependencyForcedHeaderCacheKey(
					&scanner, nil, definitions,
				)
				var ok bool
				restored, ok = cache.lookup(lookupKey, &scanner, root)
				if !ok {
					b.Fatal("exact forced-header cache lookup missed")
				}
			}
			b.StopTimer()
			if restored == nil || cache.hits != b.N {
				b.Fatalf("forced-header cache hits = %d, want %d", cache.hits, b.N)
			}
			b.ReportMetric(float64(enabled), "enabled_macros")
		})
	}
}

func configDependencyForcedHeaderBenchmarkRoot(count int, variant string) configDependencyMacroState {
	state := configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
	names := make([]string, 0, count+2)
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("BENCHMARK_PREDEFINED_%06d", index)
		state.set(name, configDependencyMacroDefined)
		names = append(names, name)
	}
	state.set("BENCHMARK_RELEVANT", configDependencyMacroDefined)
	names = append(names, "BENCHMARK_RELEVANT")
	if variant != "" {
		state.set(variant, configDependencyMacroDefined)
		names = append(names, variant)
	}
	slices.Sort(names)
	state.compilerPredefinedDigest = configDependencyAutoconfNamesDigest(names)
	state.compilerPredefinedSnapshot = state.snapshot
	return state
}

func BenchmarkConfigDependencyForcedHeaderCacheEquivalentPredefineFastPath(b *testing.B) {
	const (
		rootNames = 1024
		outputs   = 4096
	)
	root := configDependencyForcedHeaderBenchmarkRoot(rootNames, "")
	entry := captureForcedHeaderMacroEntryForTest(b, &root, func(state *configDependencyMacroState) {
		_ = state.definition("BENCHMARK_RELEVANT")
		for index := 0; index < outputs; index++ {
			state.set(fmt.Sprintf("CONFIG_PREFIX_OUTPUT_%06d", index), configDependencyMacroDefined)
		}
	})
	// Each independently built root models a distinct -dM text whose
	// replacement values differ but whose defined-name projection is equal.
	roots := make([]configDependencyMacroState, 32)
	for index := range roots {
		roots[index] = configDependencyForcedHeaderBenchmarkRoot(rootNames, "")
	}
	var restored *configDependencyMacroState
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		var ok bool
		restored, ok = entry.restore(
			&configDependencyClosureScanner{},
			&roots[iteration%len(roots)],
		)
		if !ok {
			b.Fatal("equivalent compiler-predefine root missed")
		}
	}
	b.StopTimer()
	if restored == nil || restored.snapshot != entry.snapshot {
		b.Fatal("equivalent compiler-predefine root did not share post snapshot")
	}
	b.ReportMetric(float64(rootNames), "predefined_names")
	b.ReportMetric(float64(outputs), "prefix_outputs")
}

func BenchmarkConfigDependencyForcedHeaderCacheSparseReplayRootCardinality(b *testing.B) {
	for _, rootNames := range []int{1, 1024, 65536} {
		b.Run(fmt.Sprintf("predefined-%d", rootNames), func(b *testing.B) {
			captured := configDependencyForcedHeaderBenchmarkRoot(rootNames, "AAA_CAPTURED_ROOT")
			entry := captureForcedHeaderMacroEntryForTest(b, &captured, func(state *configDependencyMacroState) {
				_ = state.definition("BENCHMARK_RELEVANT")
				for index := 0; index < 4; index++ {
					state.set(fmt.Sprintf("PREFIX_OUTPUT_%d", index), configDependencyMacroDefined)
				}
			})
			current := configDependencyForcedHeaderBenchmarkRoot(rootNames, "AAA_CURRENT_ROOT")
			var restored *configDependencyMacroState
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var ok bool
				restored, ok = entry.restore(&configDependencyClosureScanner{}, &current)
				if !ok {
					b.Fatal("sparse replay missed")
				}
			}
			b.StopTimer()
			if restored == nil || restored.definition("PREFIX_OUTPUT_3") != configDependencyMacroDefined {
				b.Fatal("sparse replay lost an output")
			}
			b.ReportMetric(float64(rootNames), "predefined_names")
		})
	}
}

func BenchmarkConfigDependencyForcedHeaderCacheSparseReplayOutputCardinality(b *testing.B) {
	for _, outputs := range []int{1, 64, 1024} {
		b.Run(fmt.Sprintf("outputs-%d", outputs), func(b *testing.B) {
			captured := configDependencyForcedHeaderBenchmarkRoot(1024, "AAA_CAPTURED_ROOT")
			entry := captureForcedHeaderMacroEntryForTest(b, &captured, func(state *configDependencyMacroState) {
				_ = state.definition("BENCHMARK_RELEVANT")
				for index := 0; index < outputs; index++ {
					state.set(fmt.Sprintf("CONFIG_PREFIX_OUTPUT_%06d", index), configDependencyMacroDefined)
				}
			})
			current := configDependencyForcedHeaderBenchmarkRoot(1024, "AAA_CURRENT_ROOT")
			var restored *configDependencyMacroState
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				var ok bool
				restored, ok = entry.restore(&configDependencyClosureScanner{}, &current)
				if !ok {
					b.Fatal("sparse replay missed")
				}
			}
			b.StopTimer()
			if restored == nil {
				b.Fatal("sparse replay returned nil")
			}
			b.ReportMetric(float64(outputs), "prefix_outputs")
		})
	}
}

func BenchmarkConfigDependencyForcedHeaderCacheSpecializationLookup(b *testing.B) {
	for _, specializations := range []int{1, 16, 128} {
		b.Run(fmt.Sprintf("entries-%d", specializations), func(b *testing.B) {
			root := configDependencyForcedHeaderBenchmarkRoot(1, "MATCHING_SPECIALIZATION")
			matching, _ := root.snapshot.lookup("MATCHING_SPECIALIZATION")
			missing, _ := root.snapshot.lookup("MISSING_SPECIALIZATION")
			entries := make([]configDependencyForcedHeaderCacheEntry, specializations)
			for index := range entries {
				requirement := configDependencyForcedHeaderMacroRequirement{
					name: "MATCHING_SPECIALIZATION",
					cell: missing,
				}
				if index == len(entries)-1 {
					requirement.cell = matching
				}
				entries[index] = configDependencyForcedHeaderCacheEntry{
					inputSnapshot:           root.snapshot,
					snapshot:                root.snapshot,
					macroRequirements:       []configDependencyForcedHeaderMacroRequirement{requirement},
					includeResolutionsReady: true,
				}
			}
			key := configDependencyForcedHeaderCacheKey{}
			cache := configDependencyForcedHeaderCache{
				entries: map[configDependencyForcedHeaderCacheKey][]configDependencyForcedHeaderCacheEntry{
					key: entries,
				},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, ok := cache.lookup(key, &configDependencyClosureScanner{}, &root); !ok {
					b.Fatal("matching specialization missed")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(specializations), "specializations")
		})
	}
}

func TestConfigDependencyResolvedAutoconfHonorsGeneratedHeaderGuard(t *testing.T) {
	predefined, reason := parseConfigDependencyCompilerPredefines("#define __GENERATED_AUTOCONF_H__ 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	definitions := newConfigDependencyResolvedAutoconfDefinitions(
		map[string]string{"CONFIG_ENABLED": "y"},
	)
	predefined.applyResolvedConfigAutoconf(definitions)
	if got := predefined.definition("CONFIG_ENABLED"); got != configDependencyMacroUndefined {
		t.Fatalf("pre-guarded autoconf CONFIG_ENABLED = %d, want unchanged undefined", got)
	}

	repeated, reason := parseConfigDependencyCompilerPredefines("")
	if reason != "" {
		t.Fatal(reason)
	}
	repeated.set("__GENERATED_AUTOCONF_H__", configDependencyMacroUndefined)
	repeated.applyResolvedConfigAutoconf(definitions)
	repeated.set("CONFIG_ENABLED", configDependencyMacroUndefined)
	repeated.applyResolvedConfigAutoconf(definitions)
	if got := repeated.definition("CONFIG_ENABLED"); got != configDependencyMacroUndefined {
		t.Fatalf("repeated guarded autoconf CONFIG_ENABLED = %d, want intervening undef preserved", got)
	}
}

func TestConfigDependencyOrderedInterpreterRejectsOnceOnlyIncludeSemantics(t *testing.T) {
	for _, source := range []string{
		"#import \"once.h\"\n",
		"#pragma once\n",
		`_Pragma("once")` + "\n",
		"#define APPLY_ONCE _Pragma(\"once\")\nAPPLY_ONCE\n",
	} {
		state, reason := parseConfigDependencyCompilerPredefines("")
		if reason != "" {
			t.Fatal(reason)
		}
		_, reason = configDependencyLiteralIncludesWithMacroEffects(
			[]byte(source), &state,
			func(configDependencyLiteralInclude, configDependencyMacroDefinition) (bool, string) {
				return true, ""
			},
		)
		if reason == "" {
			t.Errorf("ordered interpreter accepted once-only source %q", source)
		}
	}
	state, reason := parseConfigDependencyCompilerPredefines("")
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := configDependencyLiteralIncludesWithMacroEffects(
		[]byte("#define INERT_DIAGNOSTIC(x) _Pragma(#x)\n"), &state,
		func(configDependencyLiteralInclude, configDependencyMacroDefinition) (bool, string) {
			return true, ""
		},
	); reason != "" {
		t.Fatalf("inert _Pragma macro definition was rejected: %s", reason)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsCrossHeaderPragmaExpansion(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":          "CONFIG_DRIVER\n",
		"include/linux/pragma-definition.h": "#define APPLY_PRAGMA(x) _Pragma(#x)\n",
		"include/linux/pragma-use.h":        "APPLY_PRAGMA(once)\n",
	}, []string{
		"-nostdinc",
		"-include", "${tree:kernel}/include/linux/pragma-definition.h",
		"-include", "${tree:kernel}/include/linux/pragma-use.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "_Pragma") {
		t.Fatalf("cross-header _Pragma dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAppliesAutoconfAtOrderedIncludePoint(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/before-config.h": `#undef __GENERATED_AUTOCONF_H__
#ifdef CONFIG_ENABLED
#include UNMODELED_PREMATURE_CONFIG_HEADER
#endif
`,
		"include/linux/after-config.h": `#ifdef CONFIG_ENABLED
#include "enabled.h"
#endif
#ifdef CONFIG_MODULE_MODULE
#include "module.h"
#endif
#ifdef CONFIG_MODULE
#include UNMODELED_MODULE_BASE_HEADER
#endif
#ifdef CONFIG_SCALAR
#include "scalar.h"
#endif
#ifdef CONFIG_OFF
#include "command-line-off.h"
#endif
`,
		"include/linux/enabled.h":          "CONFIG_ENABLED_HEADER\n",
		"include/linux/module.h":           "CONFIG_MODULE_HEADER\n",
		"include/linux/scalar.h":           "CONFIG_SCALAR_HEADER\n",
		"include/linux/command-line-off.h": "CONFIG_OFF_HEADER\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-I${tree:prep}/include",
		"-include", "${tree:kernel}/include/linux/before-config.h",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-include", "${tree:kernel}/include/linux/after-config.h",
		"-c", "drivers/example/driver.c",
	}, map[string]string{
		"CONFIG_ENABLED": "y",
		"CONFIG_MODULE":  "m",
		"CONFIG_SCALAR":  "17",
		"CONFIG_OFF":     "n",
	})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		// The generated autoconf header does not undef an n-valued symbol, so a
		// command-line definition remains visible after the transition.
		return "#define CONFIG_OFF 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CONFIG_DRIVER", "CONFIG_ENABLED", "CONFIG_ENABLED_HEADER", "CONFIG_MODULE",
		"CONFIG_MODULE_HEADER", "CONFIG_OFF", "CONFIG_OFF_HEADER", "CONFIG_SCALAR",
		"CONFIG_SCALAR_HEADER",
	}
	if set.Opaque || !slices.Equal(set.Symbols, want) {
		t.Fatalf("ordered autoconf dependency set = %#v, want symbols %#v", set, want)
	}
	if !slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("ordered autoconf object closure = %#v", set.ObjectPaths)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesPrunesConfigDisabledNestedAutoconfPrelude(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/kconfig.h": `#ifndef __LINUX_KCONFIG_H
#define __LINUX_KCONFIG_H
#include <generated/autoconf.h>
#endif
`,
		"include/linux/compiler-types.h": `#ifndef __LINUX_COMPILER_TYPES_H
#define __LINUX_COMPILER_TYPES_H
#include <linux/compiler-attributes.h>
#ifdef CONFIG_HAVE_ARCH_COMPILER_H
#include <asm/compiler.h>
#endif
#endif
`,
		"include/linux/compiler-attributes.h": "#define __always_inline inline\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-I${tree:prep}/include",
		"-include", "${tree:kernel}/include/linux/kconfig.h",
		"-include", "${tree:kernel}/include/linux/compiler-types.h",
		"-c", "drivers/example/driver.c",
	}, map[string]string{"CONFIG_HAVE_ARCH_COMPILER_H": "n"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_HAVE_ARCH_COMPILER_H"}) {
		t.Fatalf("nested autoconf prelude dependency set = %#v, want disabled architecture include pruned", set)
	}
	wantSources := []string{
		"drivers/example/driver.c",
		"include/linux/compiler-attributes.h",
		"include/linux/compiler-types.h",
		"include/linux/kconfig.h",
	}
	if !slices.Equal(set.SourcePaths, wantSources) ||
		!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("nested autoconf prelude closure = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesJoinsPossibleNestedMacroMutation(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/forced.h": `#ifdef __FILE__
#include "maybe-undef.h"
#endif
#ifdef CONFIG_ENABLED
#include "enabled.h"
#else
#include UNMODELED_DISABLED_HEADER
#endif
`,
		"include/linux/maybe-undef.h": "#undef CONFIG_ENABLED\n",
		"include/linux/enabled.h":     "CONFIG_ENABLED_HEADER\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-include", "${tree:kernel}/include/linux/forced.h",
		"-c", "drivers/example/driver.c",
	}, map[string]string{"CONFIG_ENABLED": "y"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-literal") {
		t.Fatalf("possible nested macro mutation dependency set = %#v, want conservative branch union", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesInterpretsRepeatedConditionalFirstForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":         "#include <linux/compiler-version.h>\nCONFIG_DRIVER\n",
		"include/linux/compiler-version.h": "#ifdef COMPILER_SELECTED\n#define SELECTED 1\n#endif\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/compiler-version.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/compiler-version.h",
	}) {
		t.Fatalf("repeated conditional first-header dependency set = %#v, want exact ordered interpretation", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsRepeatedProvenGuardedForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#include <linux/compiler-types.h>
CONFIG_DRIVER
`,
		"include/linux/compiler-types.h": `#ifndef __LINUX_COMPILER_TYPES_H
#define __LINUX_COMPILER_TYPES_H
#ifdef COMPILER_SELECTED
#include "selected.h"
#endif
#endif
`,
		"include/linux/selected.h": "CONFIG_SELECTED\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/compiler-types.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SELECTED"}) {
		t.Fatalf("repeated guarded forced-header dependency set = %#v, want proven no-op repeat", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesInterpretsRepeatedForcedHeaderWhoseGuardIsUndone(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/unguarded-after-include.h>\n",
		"include/linux/unguarded-after-include.h": `#ifndef __UNDONE_GUARD_H
#define __UNDONE_GUARD_H
#undef __UNDONE_GUARD_H
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/unguarded-after-include.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || len(set.Symbols) != 0 || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/unguarded-after-include.h",
	}) {
		t.Fatalf("repeated header with undone guard dependency set = %#v, want exact repeated interpretation", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesInterpretsRepeatedForcedHeaderAfterLaterUndef(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/guarded.h>\n",
		"include/linux/guarded.h": `#ifndef __LATER_UNDEF_GUARD_H
#define __LATER_UNDEF_GUARD_H
#ifdef COMPILER_SELECTED
CONFIG_SELECTED
#endif
#endif
`,
		"include/linux/undo.h": "#undef __LATER_UNDEF_GUARD_H\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-include", "${tree:kernel}/include/linux/undo.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define COMPILER_SELECTED 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_SELECTED"}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/guarded.h", "include/linux/undo.h",
	}) {
		t.Fatalf("later guard undef dependency set = %#v, want exact repeated interpretation", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesInterpretsRepeatedForcedHeaderAfterTranslationUnitUndefWithTrailingTokens(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#undef __TU_UNDEF_GUARD_H trailing
#include <linux/guarded.h>
`,
		"include/linux/guarded.h": `#ifndef __TU_UNDEF_GUARD_H
#define __TU_UNDEF_GUARD_H
CONFIG_GUARDED
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_GUARDED"}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/guarded.h",
	}) {
		t.Fatalf("translation-unit guard undef dependency set = %#v, want exact repeated interpretation", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsUnrelatedUndefBeforeRepeatedForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#undef UNRELATED_GUARD
#include <linux/guarded.h>
CONFIG_DRIVER
`,
		"include/linux/guarded.h": `#ifndef __STABLE_GUARD_H
#define __STABLE_GUARD_H
CONFIG_GUARDED
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_GUARDED"}) {
		t.Fatalf("unrelated undef dependency set = %#v, want precise guarded repeat", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsMacroStackAroundRepeatedForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#pragma push_macro("__STACK_GUARD_H")
#include <linux/guarded.h>
#pragma pop_macro("__STACK_GUARD_H")
`,
		"include/linux/guarded.h": `#ifndef __STACK_GUARD_H
#define __STACK_GUARD_H
CONFIG_GUARDED
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "macro-stack") {
		t.Fatalf("macro-stack guard dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsMSPragmaAroundRepeatedForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `__pragma(push_macro("__MS_STACK_GUARD_H"))
#include <linux/guarded.h>
__pragma(pop_macro("__MS_STACK_GUARD_H"))
`,
		"include/linux/guarded.h": `#ifndef __MS_STACK_GUARD_H
#define __MS_STACK_GUARD_H
CONFIG_GUARDED
#endif
`,
	}, []string{
		"-nostdinc", "-fms-extensions", "-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "macro-stack") {
		t.Fatalf("MS macro-stack guard dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsCompilerMacroMSPragmaAroundRepeatedForcedHeader(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `RESTORE_GUARD
#include <linux/guarded.h>
`,
		"include/linux/save.h": "SAVE_GUARD\n",
		"include/linux/guarded.h": `#ifndef __ALIASED_STACK_GUARD_H
#define __ALIASED_STACK_GUARD_H
CONFIG_GUARDED
#endif
`,
	}, []string{
		"-nostdinc", "-fms-extensions",
		`-DSAVE_GUARD=__pragma(push_macro("__ALIASED_STACK_GUARD_H"))`,
		`-DRESTORE_GUARD=__pragma(pop_macro("__ALIASED_STACK_GUARD_H"))`,
		"-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/save.h",
		"-include", "${tree:kernel}/include/linux/guarded.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return `#define SAVE_GUARD __pragma(push_macro("__ALIASED_STACK_GUARD_H"))
#define RESTORE_GUARD __pragma(pop_macro("__ALIASED_STACK_GUARD_H"))
`, true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unmodeled __pragma effect") {
		t.Fatalf("compiler-macro MS pragma dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsIncludeEscapingModeledTree(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include \"../../../outside.h\"\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "escapes its modeled source/object tree") {
		t.Fatalf("escaping-include dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsAssemblerFileInputs(t *testing.T) {
	for _, test := range []struct {
		name      string
		pathname  string
		files     map[string]string
		directive string
	}{
		{
			name: "assembler with cpp include", pathname: "drivers/example/driver.S", directive: ".include",
			files: map[string]string{"drivers/example/driver.S": ".include \"fragment.S\"\n"},
		},
		{
			name: "C inline asm incbin", pathname: "drivers/example/driver.c", directive: ".incbin",
			files: map[string]string{"drivers/example/driver.c": "asm(\".incbin \\\"payload.bin\\\"\");\n"},
		},
		{
			name: "C++ inline asm include", pathname: "drivers/example/driver.cc", directive: ".include",
			files: map[string]string{"drivers/example/driver.cc": "asm(\".include \\\"fragment.S\\\"\");\n"},
		},
		{
			name: "inline asm in header", pathname: "drivers/example/driver.c", directive: ".incbin",
			files: map[string]string{
				"drivers/example/driver.c": "#include \"inline.h\"\n",
				"drivers/example/inline.h": "asm(\".incbin \\\"payload.bin\\\"\");\n",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(
				t, test.files, []string{"-nostdinc", "-c", test.pathname}, nil,
			)
			plan.Sources[0].Path = test.pathname

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "assembler file-input directive "+test.directive) {
				t.Fatalf("assembler file-input dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesInterpretsAssemblerFileInputReachability(t *testing.T) {
	for _, test := range []struct {
		name       string
		pathname   string
		contents   string
		predefines string
		language   string
		directive  string
		opaque     bool
	}{
		{
			name: "comment text is not an assembler input", pathname: "drivers/example/driver.c",
			contents: `/* It is possible to use asm(".include <asm/example.h>"), but do not. */
CONFIG_VISIBLE
`,
			language: "c",
		},
		{
			name: "inactive C inline asm", pathname: "drivers/example/driver.c",
			contents: `#ifdef ENABLE_FILE_ASM
asm(".incbin \"payload.bin\"");
#endif
CONFIG_VISIBLE
`,
			language: "c",
		},
		{
			name: "active C inline asm", pathname: "drivers/example/driver.c",
			contents: `#ifdef ENABLE_FILE_ASM
asm(".incbin \"payload.bin\"");
#endif
`,
			predefines: "#define ENABLE_FILE_ASM 1\n", language: "c", directive: ".incbin", opaque: true,
		},
		{
			name: "unknown C inline asm", pathname: "drivers/example/driver.c",
			contents: `#ifdef __CONTEXTUAL_FILE_ASM
asm(".include \"fragment.S\"");
#endif
`,
			language: "c", directive: ".include", opaque: true,
		},
		{
			name: "inactive assembler directive", pathname: "drivers/example/driver.S",
			contents: `#if 0
.include "fragment.S"
#endif
CONFIG_VISIBLE
`,
			language: "assembler-with-cpp",
		},
		{
			name: "active assembler directive", pathname: "drivers/example/driver.S",
			contents: ".include \"fragment.S\"\n",
			language: "assembler-with-cpp", directive: ".include", opaque: true,
		},
		{
			name: "unknown assembler directive", pathname: "drivers/example/driver.S",
			contents: `#ifdef __CONTEXTUAL_FILE_ASM
.incbin "payload.bin"
#endif
`,
			language: "assembler-with-cpp", directive: ".incbin", opaque: true,
		},
		{
			name: "active C macro can produce inline asm", pathname: "drivers/example/driver.c",
			contents: "#define FILE_ASM asm(\".incbin \\\"payload.bin\\\"\")\nFILE_ASM\n",
			language: "c", directive: ".incbin", opaque: true,
		},
		{
			name: "inactive C macro cannot produce inline asm", pathname: "drivers/example/driver.c",
			contents: `#if 0
#define FILE_ASM asm(".incbin \"payload.bin\"")
#endif
CONFIG_VISIBLE
`,
			language: "c",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(
				t,
				map[string]string{test.pathname: test.contents},
				[]string{"-nostdinc", "-c", test.pathname},
				nil,
			)
			plan.Sources[0].Path = test.pathname
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			requestedLanguage := ""
			plan.metadata.compilerPredefines = func(_, _, language string, _, _ []string, _ map[string]string) (string, bool, error) {
				requestedLanguage = language
				return test.predefines, true, nil
			}

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if requestedLanguage != test.language {
				t.Fatalf("compiler language = %q, want %q", requestedLanguage, test.language)
			}
			if test.opaque {
				if !set.Opaque || !strings.Contains(set.Reason, "assembler file-input directive "+test.directive) {
					t.Fatalf("assembler file-input dependency set = %#v, want opaque fallback", set)
				}
				return
			}
			if set.Opaque {
				t.Fatalf("inactive/comment assembler text dependency set = %#v, want precise", set)
			}
		})
	}
}

func TestConfigDependencyCompilerPredefineNormalizationResolvesSourceDotSegments(t *testing.T) {
	const source = "drivers/shared/unit.S"
	for _, operand := range []string{
		"drivers/shared/./unit.S",
		"drivers/consumer/private/../../shared/unit.S",
		"__LINUX_BZL_SOURCE_TREE__/drivers/consumer/private/../../shared/unit.S",
		"/immutable/source/drivers/consumer/private/../../shared/unit.S",
	} {
		t.Run(operand, func(t *testing.T) {
			// Option payloads may contain the same path but are not positional
			// source operands. Only the last word should be removed for probing.
			want := []string{"-nostdinc", "-DKEEP=" + operand, "-D", "VALUE=" + operand, "-target", operand}
			arguments := append(slices.Clone(want), "-c", operand)
			probe, reason := configDependencyCompilerPredefineProbeForInvocation(configDependencyCompilerInvocation{
				tool: "cc", arguments: arguments, kbuildEnd: len(arguments), configuredContract: true,
			}, []string{source})
			if reason != "" || probe.language != "assembler-with-cpp" || len(probe.translationUnits) != 0 ||
				!slices.Equal(probe.arguments, want) {
				t.Fatalf("dot-segment source projection = %#v, reason=%q", probe, reason)
			}
		})
	}
}

func TestConfigDependencyCompilerSourceDotSegmentsDoNotEraseSymbolicOrOptionOwnership(t *testing.T) {
	const source = "drivers/shared/unit.S"
	for _, argument := range []string{
		"-Iunused/../" + source,
		"-DVALUE=unused/../" + source,
		"${result:00000000.text}/../" + source,
		linuxProbeSymbolPrefix + strings.Repeat("1", 64) + "/../" + source,
		"drivers/different/unit.S",
	} {
		if got, ok := configDependencyCompilerSourceArgument(argument, []string{source}, nil); ok {
			t.Fatalf("non-source operand %q was accepted as %q", argument, got)
		}
	}
}

func TestConfigDependencyCompilerPredefineNormalizationRejectsStdinTranslationUnit(t *testing.T) {
	for _, arguments := range [][]string{
		{"-E", "-D__GENKSYMS__", "-nostdinc", "${result:00000000.text}", "-xc", "-"},
		{"-E", "-D__GENKSYMS__", "-nostdinc", "-x", "c", "-"},
		{"-c", "drivers/example/unit.c", "-"},
		{"-c", "-", "drivers/example/unit.c"},
	} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			probe, reason := configDependencyCompilerPredefineProbeForInvocation(
				configDependencyCompilerInvocation{
					tool: "cc", arguments: arguments, kbuildEnd: len(arguments), configuredContract: true,
				},
				[]string{"drivers/example/unit.c"},
			)
			if !strings.Contains(reason, "stdin translation unit") || probe.arguments != nil || probe.translationUnits != nil {
				t.Fatalf("stdin projection = %#v, reason=%q; want no partial optional query", probe, reason)
			}
		})
	}
}

func TestConfigDependencyCompilerPredefineNormalizationPreservesDashScalarOperands(t *testing.T) {
	const source = "drivers/example/unit.c"
	for _, option := range []string{"-target", "-G", "-D", "-U", "-meabi", "--sysroot"} {
		for _, operand := range []string{source, "/immutable/source/" + source, "${source:unit}"} {
			t.Run(option+"/"+operand, func(t *testing.T) {
				arguments := []string{option, "-", "-c", operand}
				probe, reason := configDependencyCompilerPredefineProbeForInvocation(
					configDependencyCompilerInvocation{
						tool: "cc", arguments: arguments, kbuildEnd: len(arguments), configuredContract: true,
						sourceBindings: map[string]ActionPlanSource{
							"${source:unit}": {Namespace: "kernel", Path: source},
						},
					},
					[]string{source},
				)
				// This tests option arity, not whether a compiler accepts '-' as
				// the value of that option. The configured probe remains responsible
				// for reporting unsupported option values without reclassifying them.
				if reason != "" || probe.language != "c" || len(probe.translationUnits) != 0 ||
					!slices.Equal(probe.arguments, []string{option, "-"}) {
					t.Fatalf("scalar/path projection = %#v, reason=%q", probe, reason)
				}
			})
		}
	}
}

func TestCompilerProbeProjectionSkipsAssemblyModversionsStdin(t *testing.T) {
	// scripts/Makefile.build's cmd_gensymtypes_S preprocesses a generated C
	// stream with $(CPP) -D__GENKSYMS__ $(c_flags) -xc -. The .S file belongs
	// to the surrounding source action, not to that compiler's argv. In symbolic
	// discovery c_flags is one unresolved argument word, as in the real request.
	for _, source := range []string{
		"arch/x86/realmode/rm/bioscall.S",
		"arch/x86/realmode/rm/copy.S",
		"arch/x86/realmode/rm/header.S",
	} {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/persistent-%t", source, persistent), func(t *testing.T) {
				arguments := []string{
					"-E", "-D__GENKSYMS__", "-nostdinc", "${result:00000000.text}",
					"-DKBUILD_MODFILE=1", "-DKBUILD_BASENAME=1", "-DKBUILD_MODNAME=1", "-D__KBUILD_MODNAME=1",
					"-xc", "-",
				}
				plan, node := configDependencyCompilePlanForTest(t, map[string]string{
					source: "#ifndef QUERY_GUARD\n#define QUERY_GUARD\n#endif\nCONFIG_ASM_INPUT\n",
				}, arguments, map[string]string{"CONFIG_MODVERSIONS": "y"})
				plan.Sources[0].Path = source
				if persistent {
					node.Sources = nil
					node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
						Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: source},
						SourceID: plan.Sources[0].ID,
					})
				}
				plan.Nodes[0] = node
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
					node.ID: {Tool: "cc", Arguments: slices.Clone(arguments)},
				}
				plan.metadata.sourceGuardNamesReady = true
				plan.metadata.sourceGuardNames = []string{"QUERY_GUARD"}
				predefineCalls, definednessCalls := 0, 0
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					predefineCalls++
					return "", false, nil
				}
				plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
					definednessCalls++
					return nil, false, nil
				}
				paths, err := actionPlanConfigDependencySourcePaths(plan, node)
				if err != nil || !slices.Equal(paths, []string{source}) {
					t.Fatalf("source closure = %q, err=%v; fixture must reach the real source projection", paths, err)
				}
				analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
				if err != nil {
					t.Fatal(err)
				}
				if predefineCalls != 0 || definednessCalls != 0 {
					t.Fatalf("stdin optional queries registered: predefines=%d, definedness=%d", predefineCalls, definednessCalls)
				}
				if len(analysis.sets) != 1 || !analysis.sets[0].Opaque ||
					!strings.Contains(analysis.sets[0].Reason, "MODVERSIONS") {
					t.Fatalf("stdin secondary action lost existing full-config fallback: %#v", analysis.sets)
				}
			})
		}
	}
}

func TestConfigDependencyCompilerPredefineNormalizationPreservesMacroOptionsAndScalarArity(t *testing.T) {
	arguments := []string{
		"-target", "-DTHIS_IS_THE_TARGET_OPERAND",
		"-DREAL_DRIVER_MACRO=1", "-DPATH_MACRO=x/drivers/example/driver.c", "-U", "REAL_UNDEF",
		"-c", "drivers/example/driver.c",
	}
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: arguments, kbuildEnd: len(arguments), configuredContract: true,
			probeEnvironment: map[string]string{"MODE": "exact"},
		},
		[]string{"drivers/example/driver.c"},
	)
	if reason != "" {
		t.Fatal(reason)
	}
	want := []string{
		"-target", "-DTHIS_IS_THE_TARGET_OPERAND",
		"-DREAL_DRIVER_MACRO=1", "-DPATH_MACRO=x/drivers/example/driver.c", "-U", "REAL_UNDEF",
	}
	if !slices.Equal(probe.arguments, want) {
		t.Fatalf("normalized macro/scalar argv = %#v, want exact %#v", probe.arguments, want)
	}
	if !maps.Equal(probe.environment, map[string]string{"MODE": "exact"}) {
		t.Fatalf("normalized compiler environment = %#v", probe.environment)
	}
	uppercaseCArguments := []string{"-c", "drivers/example/driver.C"}
	uppercaseCProbe, reason := configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: uppercaseCArguments, kbuildEnd: len(uppercaseCArguments), configuredContract: true,
		},
		[]string{"drivers/example/driver.C"},
	)
	if reason != "" {
		t.Fatal(reason)
	}
	if uppercaseCProbe.language != "c++" {
		t.Fatalf("uppercase .C compiler-predefine language = %q, want c++", uppercaseCProbe.language)
	}

	for _, forwarding := range [][]string{
		{"-Xassembler", "-DNOT_A_DRIVER_MACRO"},
		{"-Xlinker", "-xc++"},
		{"-mllvm", "-UNOT_A_DRIVER_MACRO"},
		{"-Wa,-DNOT_A_DRIVER_MACRO"},
		{"-Wl,-xc++"},
	} {
		candidate := append(slices.Clone(forwarding), "-c", "drivers/example/driver.c")
		_, reason := configDependencyCompilerPredefineProbeForInvocation(
			configDependencyCompilerInvocation{
				tool: "cc", arguments: candidate, kbuildEnd: len(candidate), configuredContract: true,
			},
			[]string{"drivers/example/driver.c"},
		)
		if !strings.Contains(reason, "forwarding") {
			t.Errorf("forwarding argv %#v reason = %q, want fail-closed normalization", forwarding, reason)
		}
	}
	_, reason = configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: []string{"-c", "drivers/example/driver.c"}, kbuildEnd: 2,
			configuredContract: true, probeEnvironment: map[string]string{"CPATH": "/source-controlled"},
		},
		[]string{"drivers/example/driver.c"},
	)
	if !strings.Contains(reason, "safe probe") {
		t.Fatalf("unsafe compiler probe environment reason = %q", reason)
	}
}

func TestConfigDependencyCompilerPredefineNormalizationCarriesUnresolvedTranslationUnitProvenance(t *testing.T) {
	const source = "drivers/example/driver.c"
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: []string{"${result:00000000.text}"}, kbuildEnd: 1,
			configuredContract: true,
		},
		[]string{source},
	)
	if reason != "" {
		t.Fatal(reason)
	}
	if !slices.Equal(probe.arguments, []string{"${result:00000000.text}"}) {
		t.Fatalf("compiler-predefine arguments = %#v, want unresolved Make-text argument", probe.arguments)
	}
	if !slices.Equal(probe.translationUnits, []string{source}) {
		t.Fatalf("compiler-predefine translation units = %#v, want exact source %q", probe.translationUnits, source)
	}
}

func TestConfigDependencyCompilerPredefineEnvironmentProjection(t *testing.T) {
	omitted := []string{
		"CLIPPY_CONF_DIR",
		"INSTALL_HDR_PATH",
		"KBUILD_AFLAGS",
		"KBUILD_CFLAGS",
		"KBUILD_CPPFLAGS",
		"KBUILD_HOSTCFLAGS",
		"KBUILD_HOSTCXXFLAGS",
		"KBUILD_RUSTFLAGS",
		"KBUILD_USERCFLAGS",
		"KBUILD_USERLDFLAGS",
		"KERNELDOC",
		"LIBELF_FLAGS",
		"LIBELF_LIBS",
		"LINUXINCLUDE",
		"M",
		"RESOLVE_BTFIDS",
		"VPATH",
		"obj",
		"objtree",
		"srcroot",
		"srctree",
		"LANG",
		"LANGUAGE",
		"LC_ALL",
		"LC_CTYPE",
		"LC_MESSAGES",
		"SOURCE_DATE_EPOCH",
	}
	environment := map[string]string{
		"WRAPPER_MODE":             "exact",
		"OBJECT_MODE":              "64",
		"MACOSX_DEPLOYMENT_TARGET": "14.4",
	}
	for _, name := range omitted {
		// Deliberately malformed action syntax proves that an irrelevant Make or
		// replacement-body value is removed before symbolic environment lowering.
		environment[name] = "${tree:malformed"
	}
	original := maps.Clone(environment)
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: []string{"-c", "drivers/example/driver.c"}, kbuildEnd: 2,
			configuredContract: true, probeEnvironment: environment,
		},
		[]string{"drivers/example/driver.c"},
	)
	if reason != "" {
		t.Fatal(reason)
	}
	want := map[string]string{
		"WRAPPER_MODE":             "exact",
		"OBJECT_MODE":              "64",
		"MACOSX_DEPLOYMENT_TARGET": "14.4",
	}
	if !maps.Equal(probe.environment, want) {
		t.Fatalf("compiler-predefine environment projection = %#v, want %#v", probe.environment, want)
	}
	if !maps.Equal(environment, original) {
		t.Fatalf("compiler-predefine environment projection mutated caller input: got %#v, want %#v", environment, original)
	}
}

func TestConfigDependencyCompilerPredefineEnvironmentRejectsUnsafeControls(t *testing.T) {
	for _, name := range []string{
		"BASH_ENV",
		"CCC_OVERRIDE_OPTIONS",
		"CL",
		"CLANG_CONFIG_PATH",
		"CLANG_NO_DEFAULT_CONFIG",
		"COMPILER_PATH",
		"CPATH",
		"GCC_COMPARE_DEBUG",
		"GCC_SPECS",
		"LD_PRELOAD",
		"PATH",
		"_CL_",
	} {
		t.Run(name, func(t *testing.T) {
			_, reason := configDependencyCompilerPredefineProbeForInvocation(
				configDependencyCompilerInvocation{
					tool: "cc", arguments: []string{"-c", "drivers/example/driver.c"}, kbuildEnd: 2,
					configuredContract: true, probeEnvironment: map[string]string{name: "unsafe"},
				},
				[]string{"drivers/example/driver.c"},
			)
			if !strings.Contains(reason, "safe probe") {
				t.Fatalf("unsafe compiler-predefine environment %s reason = %q", name, reason)
			}
		})
	}
}

func TestConfigDependencyCompilerPredefineNormalizationRejectsConfiguredPrefixLanguage(t *testing.T) {
	arguments := []string{"-x", "c++", "-c", "drivers/example/driver.c"}
	_, reason := configDependencyCompilerPredefineProbeForInvocation(
		configDependencyCompilerInvocation{
			tool: "cc", arguments: arguments, kbuildStart: 2, kbuildEnd: len(arguments), configuredContract: true,
		},
		[]string{"drivers/example/driver.c"},
	)
	if !strings.Contains(reason, "prefix contains a language override") {
		t.Fatalf("configured prefix language reason = %q, want fail-closed normalization", reason)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansLiteralClosure(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `/* CONFIG_COMMENT */
#if CONFIG_GATE
#include "enabled.h"
#endif
#include <linux/shared.h>
`,
		"drivers/example/enabled.h": `CONFIG_ONLY_MODULE
#include "nested.h"
`,
		"drivers/example/nested.h": "CONFIG_NESTED\n",
		"include/linux/shared.h":   "CONFIG_SHARED\n",
	}, []string{
		"-I${tree:kernel}/include",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CONFIG_COMMENT", "CONFIG_GATE", "CONFIG_NESTED", "CONFIG_ONLY", "CONFIG_SHARED"}
	if set.Opaque || !slices.Equal(set.Symbols, want) {
		t.Fatalf("config dependency set = %#v, want symbols %q", set, want)
	}
	wantSources := []string{
		"drivers/example/driver.c",
		"drivers/example/enabled.h",
		"drivers/example/nested.h",
		"include/linux/shared.h",
	}
	if !slices.Equal(set.SourcePaths, wantSources) ||
		!slices.Equal(set.ObjectPaths, []string{"include/generated/autoconf.h"}) {
		t.Fatalf("config dependency file closure = %#v, want source %q and autoconf object path", set, wantSources)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAppliesLocalMacroStateToOrdinaryIncludes(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#define COMPILE_OFFSETS
#include <linux/sched.h>
CONFIG_TRANSLATION_UNIT
`,
		"include/linux/sched.h": `#ifndef COMPILE_OFFSETS
#include <rq-offsets.h>
#endif
CONFIG_SCHED_HEADER
`,
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I${tree:prep}/include/generated",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencyStageGeneratedHeaderForTest(
		t, plan, &node, "rq-offsets", "include/generated/rq-offsets.h",
		ActionRecipe{Tool: "actionfile"},
	)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_SCHED_HEADER", "CONFIG_TRANSLATION_UNIT",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/sched.h",
	}) || len(set.ObjectPaths) != 0 {
		t.Fatalf("locally guarded generated include dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesDoesNotPruneLocalGuardAfterUndefOrUnknownBranch(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
	}{
		{
			name: "exact undef",
			prefix: `#define COMPILE_OFFSETS
#undef COMPILE_OFFSETS
`,
		},
		{
			name: "define on unknown branch",
			prefix: `#if defined(__CONTEXT_SENSITIVE_STATE__)
#define COMPILE_OFFSETS
#endif
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": test.prefix + `#include <linux/sched.h>
CONFIG_TRANSLATION_UNIT
`,
				"include/linux/sched.h": `#ifndef COMPILE_OFFSETS
#include <rq-offsets.h>
#endif
CONFIG_SCHED_HEADER
`,
			}, []string{
				"-nostdinc",
				"-I${tree:kernel}/include",
				"-I${tree:prep}/include/generated",
				"-c", "drivers/example/driver.c",
			}, nil)
			configDependencyStageGeneratedHeaderForTest(
				t, plan, &node, "rq-offsets", "include/generated/rq-offsets.h",
				ActionRecipe{Tool: "actionfile"},
			)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "unavailable generated object-tree input include/generated/rq-offsets.h") {
				t.Fatalf("%s generated include dependency set = %#v, want fail-closed result", test.name, set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesIsolatesLocalMacrosBetweenTranslationUnits(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#define LOCAL_TO_FIRST_UNIT
#include <linux/shared.h>
CONFIG_FIRST_UNIT
`,
		"drivers/example/second.c": `#include <linux/shared.h>
CONFIG_SECOND_UNIT
`,
		"include/linux/shared.h": `#ifndef LOCAL_TO_FIRST_UNIT
#include "selected.h"
#endif
`,
		"include/linux/selected.h": "CONFIG_SECOND_UNIT_HEADER\n",
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-c",
		"drivers/example/driver.c", "drivers/example/second.c",
	}, nil)
	second := ActionPlanSource{
		ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/second.c",
	}
	plan.Sources = append(plan.Sources, second)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "source", SourceID: second.ID})
	plan.Nodes[0] = node
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_FIRST_UNIT", "CONFIG_SECOND_UNIT", "CONFIG_SECOND_UNIT_HEADER",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "drivers/example/second.c",
		"include/linux/selected.h", "include/linux/shared.h",
	}) {
		t.Fatalf("multi-translation-unit local macro dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsProvenGuardedIncludeRecursion(t *testing.T) {
	for _, test := range []struct {
		name        string
		entry       string
		headers     map[string]string
		wantSymbols []string
		wantSources []string
	}{
		{
			name:  "direct",
			entry: "recursive.h",
			headers: map[string]string{
				"include/linux/recursive.h": `#ifndef __RECURSIVE_H
#define __RECURSIVE_H
CONFIG_RECURSIVE
#include "recursive.h"
#endif
`,
			},
			wantSymbols: []string{"CONFIG_DRIVER", "CONFIG_RECURSIVE"},
			wantSources: []string{"drivers/example/driver.c", "include/linux/recursive.h"},
		},
		{
			name:  "replacement-valued guard",
			entry: "replacement.h",
			headers: map[string]string{
				"include/linux/replacement.h": `#ifndef REPLACEMENT_GUARD
#define REPLACEMENT_GUARD 1
CONFIG_REPLACEMENT_GUARD
#include "replacement.h"
#endif
`,
			},
			wantSymbols: []string{"CONFIG_DRIVER", "CONFIG_REPLACEMENT_GUARD"},
			wantSources: []string{"drivers/example/driver.c", "include/linux/replacement.h"},
		},
		{
			name:  "indirect",
			entry: "first.h",
			headers: map[string]string{
				"include/linux/first.h": `#ifndef __FIRST_H
#define __FIRST_H
CONFIG_FIRST
#include "second.h"
#endif
`,
				"include/linux/second.h": `#ifndef __SECOND_H
#define __SECOND_H
CONFIG_SECOND
#include "first.h"
#endif
`,
			},
			wantSymbols: []string{"CONFIG_DRIVER", "CONFIG_FIRST", "CONFIG_SECOND"},
			wantSources: []string{
				"drivers/example/driver.c", "include/linux/first.h", "include/linux/second.h",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{
				"drivers/example/driver.c": "#include <linux/" + test.entry + ">\nCONFIG_DRIVER\n",
			}
			for pathname, contents := range test.headers {
				files[pathname] = contents
			}
			plan, node := configDependencyCompilePlanForTest(t, files, []string{
				"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
			}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque || !slices.Equal(set.Symbols, test.wantSymbols) ||
				!slices.Equal(set.SourcePaths, test.wantSources) {
				t.Fatalf("%s guarded recursion dependency set = %#v", test.name, set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsUnguardedDispatcherToActiveGuardedLeaf(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/dispatcher.h>\nCONFIG_DRIVER\n",
		"include/linux/dispatcher.h": `#ifdef SELECT_ALTERNATE_LEAF
#include "alternate.h"
#else
#include "selected.h"
#endif
`,
		"include/linux/alternate.h": `#ifndef ALTERNATE_GUARD
#define ALTERNATE_GUARD
CONFIG_ALTERNATE
#endif
`,
		"include/linux/selected.h": `#ifndef SELECTED_GUARD
#define SELECTED_GUARD
#if defined(__CONTEXT_SENSITIVE_STATE__)
#define POSSIBLY_SELECTED_HELPER
#endif
CONFIG_SELECTED
#include "middle.h"
#endif
`,
		"include/linux/middle.h": `#ifndef MIDDLE_GUARD
#define MIDDLE_GUARD
CONFIG_MIDDLE
#include "dispatcher.h"
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_DRIVER", "CONFIG_MIDDLE", "CONFIG_SELECTED",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/dispatcher.h",
		"include/linux/middle.h", "include/linux/selected.h",
	}) {
		t.Fatalf("unguarded dispatcher recursion dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsWildcardBeforeUnguardedDispatcher(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/dispatcher.h>\nCONFIG_DRIVER\n",
		"include/linux/dispatcher.h": `#ifdef CONFIG_SELECT_ALTERNATE
#include "alternate.h"
#else
#include "selected.h"
#endif
`,
		"include/linux/alternate.h": `#ifndef ALTERNATE_GUARD
#define ALTERNATE_GUARD
CONFIG_ALTERNATE
#endif
`,
		"include/linux/selected.h": `#ifndef SELECTED_GUARD
#define SELECTED_GUARD
CONFIG_SELECTED
#include "middle.h"
#endif
`,
		"include/linux/middle.h": `#ifndef MIDDLE_GUARD
#define MIDDLE_GUARD
CONFIG_MIDDLE
#include "dispatcher.h"
#endif
`,
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-include", "${tree:prep}/include/generated/offsets.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_DRIVER", "CONFIG_MIDDLE", "CONFIG_SELECTED", "CONFIG_SELECT_ALTERNATE",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/dispatcher.h",
		"include/linux/middle.h", "include/linux/selected.h",
	}) || !slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("wildcard-before-dispatcher dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsWildcardDuringUnguardedDispatcher(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/dispatcher.h>\nCONFIG_DRIVER\n",
		"include/linux/dispatcher.h": `#ifdef CONFIG_SELECT_ALTERNATE
#include "alternate.h"
#else
#include "selected.h"
#endif
`,
		"include/linux/alternate.h": `#ifndef ALTERNATE_GUARD
#define ALTERNATE_GUARD
CONFIG_ALTERNATE
#endif
`,
		"include/linux/selected.h": `#ifndef SELECTED_GUARD
#define SELECTED_GUARD
CONFIG_SELECTED
#include <generated/offsets.h>
#include "middle.h"
#endif
`,
		"include/linux/middle.h": `#ifndef MIDDLE_GUARD
#define MIDDLE_GUARD
CONFIG_MIDDLE
#include "dispatcher.h"
#endif
`,
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I${tree:prep}/include",
		"-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_DRIVER", "CONFIG_MIDDLE", "CONFIG_SELECTED", "CONFIG_SELECT_ALTERNATE",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/dispatcher.h",
		"include/linux/middle.h", "include/linux/selected.h",
	}) || !slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("wildcard-during-dispatcher dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsGuardAfterGuardedPrelude(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/prelude.h>\nCONFIG_DRIVER\n",
		"include/linux/prelude.h": `#include "core.h"
#ifndef PRELUDE_GUARD
#define PRELUDE_GUARD
CONFIG_PRELUDE
#endif
`,
		"include/linux/core.h": `#ifndef CORE_GUARD
#define CORE_GUARD
CONFIG_CORE
#include "prelude.h"
#endif
`,
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_CORE", "CONFIG_DRIVER", "CONFIG_PRELUDE",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "include/linux/core.h", "include/linux/prelude.h",
	}) {
		t.Fatalf("guarded-prelude recursion dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsSentinelControlledSelfInclusion(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#ifndef SECOND_PASS
#define SECOND_PASS
#include "driver.c"
#else
#include "second-pass.h"
#endif
CONFIG_DRIVER
`,
		"drivers/example/second-pass.h": "CONFIG_SECOND_PASS\n",
	}, []string{
		"-nostdinc", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{
		"CONFIG_DRIVER", "CONFIG_SECOND_PASS",
	}) || !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c", "drivers/example/second-pass.h",
	}) {
		t.Fatalf("sentinel-controlled self-inclusion dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRecursiveEntryMatchesAcyclicExpansion(t *testing.T) {
	for _, test := range []struct {
		name     string
		wildcard bool
		wantLeaf []string
	}{
		{
			name: "exact macro writes",
			wantLeaf: []string{
				"include/linux/result-done.h",
				"include/linux/result-maybe-undefined.h",
				"include/linux/result-post-undefined.h",
				"include/linux/result-stable.h",
			},
		},
		{
			name:     "validated wildcard ordering",
			wildcard: true,
			wantLeaf: []string{
				"include/linux/result-done.h",
				"include/linux/result-maybe-defined.h",
				"include/linux/result-maybe-undefined.h",
				"include/linux/result-post-undefined.h",
				"include/linux/result-stable.h",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutations := `#define STABLE_ACROSS_ENTRY
#undef BECOMES_MAYBE
`
			if test.wildcard {
				mutations += "#include <generated/offsets.h>\n"
			}
			mutations += "#undef POST_WILDCARD_UNDEFINED\n"
			driver := `#include <linux/state.h>
#ifdef RECURSION_DONE
#include <linux/result-done.h>
#else
#include <linux/result-not-done.h>
#endif
#ifdef STABLE_ACROSS_ENTRY
#include <linux/result-stable.h>
#else
#include <linux/result-not-stable.h>
#endif
#ifndef POST_WILDCARD_UNDEFINED
#include <linux/result-post-undefined.h>
#else
#include <linux/result-post-defined.h>
#endif
#ifdef BECOMES_MAYBE
#include <linux/result-maybe-defined.h>
#else
#include <linux/result-maybe-undefined.h>
#endif
`
			baseFiles := map[string]string{
				"drivers/example/driver.c": driver,
			}
			for _, pathname := range []string{
				"result-done.h", "result-not-done.h",
				"result-stable.h", "result-not-stable.h",
				"result-post-undefined.h", "result-post-defined.h",
				"result-maybe-defined.h", "result-maybe-undefined.h",
			} {
				baseFiles["include/linux/"+pathname] = ""
			}

			analyze := func(recursive bool) ConfigDependencySet {
				files := maps.Clone(baseFiles)
				if recursive {
					files["include/linux/state.h"] = `#ifndef RECURSION_PASS
#define RECURSION_PASS
` + mutations + `#include "state.h"
#else
#define RECURSION_DONE
#endif
`
				} else {
					files["include/linux/state.h"] = "#define RECURSION_PASS\n" + mutations + "#include \"state-next.h\"\n"
					files["include/linux/state-next.h"] = "#define RECURSION_DONE\n"
				}
				arguments := []string{
					"-nostdinc", "-I${tree:kernel}/include",
				}
				if test.wildcard {
					arguments = append(arguments, "-I${tree:prep}/include")
				}
				arguments = append(arguments, "-c", "drivers/example/driver.c")
				plan, node := configDependencyCompilePlanForTest(t, files, arguments, nil)
				if test.wildcard {
					configDependencyStageValidatedMacroHeaderForTest(
						t, plan, &node, "include/generated/offsets.h",
					)
				}
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					return "", true, nil
				}
				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				if err != nil {
					t.Fatal(err)
				}
				return set
			}

			recursive := analyze(true)
			expanded := analyze(false)
			leafPaths := func(set ConfigDependencySet) []string {
				var leaves []string
				for _, pathname := range set.SourcePaths {
					if strings.HasPrefix(pathname, "include/linux/result-") {
						leaves = append(leaves, pathname)
					}
				}
				return leaves
			}
			if recursive.Opaque || expanded.Opaque ||
				!slices.Equal(leafPaths(recursive), test.wantLeaf) ||
				!slices.Equal(leafPaths(recursive), leafPaths(expanded)) ||
				!slices.Equal(recursive.ObjectPaths, expanded.ObjectPaths) {
				t.Fatalf("recursive set %#v does not match acyclic expansion %#v; want leaves %q", recursive, expanded, test.wantLeaf)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsUnprovenIncludeRecursion(t *testing.T) {
	for _, test := range []struct {
		name       string
		header     string
		wantReason string
	}{
		{
			name:       "unguarded",
			header:     "#include \"recursive.h\"\n",
			wantReason: "recursive include with repeated macro state",
		},
		{
			name:       "guard becomes unknown",
			wantReason: "tainted or unknown macro-state progress",
			header: `#ifndef __RECURSIVE_H
#define __RECURSIVE_H
#if defined(__CONTEXT_SENSITIVE_STATE__)
#undef __RECURSIVE_H
#endif
#include "recursive.h"
#endif
`,
		},
		{
			name:       "toggle repeats initial state",
			wantReason: "recursive include with repeated macro state",
			header: `#ifdef TOGGLE_STATE
#undef TOGGLE_STATE
#else
#define TOGGLE_STATE
#endif
#include "recursive.h"
`,
		},
		{
			name:       "unknown-only change",
			wantReason: "tainted or unknown macro-state progress",
			header: `#if defined(__CONTEXT_SENSITIVE_STATE__)
#define POSSIBLY_DEFINED
#endif
#include "recursive.h"
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c":  "#include <linux/recursive.h>\n",
				"include/linux/recursive.h": test.header,
			}, []string{
				"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
			}, nil)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, test.wantReason) {
				t.Fatalf("%s recursion dependency set = %#v, want opaque fallback", test.name, set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesBoundsRecursiveMacroStateProgress(t *testing.T) {
	var header strings.Builder
	for index := 0; index < configDependencyMaxConditionalActiveStatesPerFile; index++ {
		if index == 0 {
			header.WriteString("#ifndef PROGRESS_0\n")
		} else {
			fmt.Fprintf(&header, "#elifndef PROGRESS_%d\n", index)
		}
		fmt.Fprintf(&header, "#define PROGRESS_%d\n#include \"bounded.h\"\n", index)
	}
	header.WriteString("#endif\n")
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <linux/bounded.h>\n",
		"include/linux/bounded.h":  header.String(),
	}, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "exceeds the macro-state progress bound") {
		t.Fatalf("bounded recursive progress dependency set = %#v, want bounded opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesResolvesNestedAutoconfFromObjectIncludeRoot(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#include <linux/kconfig.h>
#if CONFIG_NESTED_AUTOCONF
int enabled;
#endif
`,
		// Real Linux reaches autoconf through this nested include instead of a
		// direct compiler -include operand.
		"include/linux/kconfig.h": "#include <generated/autoconf.h>\n",
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I${tree:prep}/include",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{
		// Model rules_cc's Clang resource-directory suffix. The explicit object
		// include root precedes this uninspectable system class and must resolve
		// the staged autoconf projection before reaching it.
		SuffixArguments: []string{"-Xclang", "-internal-isystem", "-Xclang", "/toolchain/resource/include"},
	})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_NESTED_AUTOCONF"}) {
		t.Fatalf("nested autoconf dependency set = %#v, want precise symbol projection", set)
	}
	if !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c",
		"include/linux/kconfig.h",
	}) || !slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("nested autoconf file closure = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesKeepsPureSourceAutoconfLookupOpaque(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/kconfig.h":  "#include <generated/autoconf.h>\n",
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-include", "${tree:kernel}/include/linux/kconfig.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{
		SuffixArguments: []string{"-Xclang", "-internal-isystem", "-Xclang", "/toolchain/resource/include"},
	})

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "shadowed by an uninspectable compiler search root: generated/autoconf.h") {
		t.Fatalf("pure-source nested autoconf dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesPrefersSourceAutoconfShadow(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#include <linux/kconfig.h>
#if CONFIG_DRIVER
int enabled;
#endif
`,
		"include/linux/kconfig.h":      "#include <generated/autoconf.h>\n",
		"include/generated/autoconf.h": "#define CONFIG_SOURCE_AUTOCONF 1\n",
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I${tree:prep}/include",
		"-c", "drivers/example/driver.c",
	}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SOURCE_AUTOCONF"}) {
		t.Fatalf("source-shadowed autoconf dependency set = %#v, want precise source closure", set)
	}
	if !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c",
		"include/generated/autoconf.h",
		"include/linux/kconfig.h",
	}) || len(set.ObjectPaths) != 0 {
		t.Fatalf("source-shadowed autoconf file closure = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansForcedSourceAutoconfShadow(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":   "CONFIG_DRIVER\n",
		configDependencyAutoconfPath: "CONFIG_FORCED_SOURCE_AUTOCONF\n",
	}, []string{
		"-nostdinc",
		"-include", "${tree:kernel}/" + configDependencyAutoconfPath,
		"-c", "drivers/example/driver.c",
	}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_FORCED_SOURCE_AUTOCONF"}) {
		t.Fatalf("forced source-shadowed autoconf dependency set = %#v, want precise source contents", set)
	}
	if !slices.Equal(set.SourcePaths, []string{
		"drivers/example/driver.c",
		configDependencyAutoconfPath,
	}) || len(set.ObjectPaths) != 0 {
		t.Fatalf("forced source-shadowed autoconf file closure = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesKeepsExternalShadowBeforeObjectAutoconfOpaque(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/linux/kconfig.h":  "#include <generated/autoconf.h>\n",
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I/uninspectable/include",
		"-I${tree:prep}/include",
		"-include", "${tree:kernel}/include/linux/kconfig.h",
		"-c", "drivers/example/driver.c",
	}, nil)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "shadowed by an uninspectable compiler search root: generated/autoconf.h") {
		t.Fatalf("externally shadowed nested autoconf dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesIgnoresAmbientKconfigConfigForTypedCompiler(t *testing.T) {
	arguments := []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-include", "${tree:prep}/include/generated/autoconf.h",
		"-c", "drivers/example/driver.c",
	}
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#if CONFIG_DRIVER\n#endif\n",
	}, arguments, nil)
	recipe := plan.Recipes[node.Recipe]
	recipe.Tool = compactKbuildScriptRunnerRole
	recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
	recipe.Environment = map[string]string{"KCONFIG_CONFIG": ".config"}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
		Tool:                     "cc",
		Arguments:                arguments,
		WorkingInputUsesComplete: true,
	}
	recipe.Sources = []string{"config", "translation-unit"}
	recipe.WorkingInputs = map[string]string{
		"source:config":           ".config",
		"source:translation-unit": "drivers/example/driver.c",
	}
	plan.Recipes[node.Recipe] = recipe
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: ".config"}
	plan.Sources = append(plan.Sources, configSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
		t.Fatalf("ambient KCONFIG_CONFIG dependency set = %#v, want precise CONFIG_DRIVER", set)
	}

	recipe.CompilerInvocation.Arguments = append(slices.Clone(arguments), "-include", "${tree:prep}/.config")
	plan.Recipes[node.Recipe] = recipe
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-autoconf config projection .config") {
		t.Fatalf("explicit compiler .config dependency set = %#v, want opaque", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesDriverLinkContractForTypedCompiler(t *testing.T) {
	for _, tool := range []string{"cc", "cxx"} {
		for _, helper := range []struct {
			name   string
			source string
			output string
		}{
			{name: "production fixdep", source: "scripts/basic/fixdep.c", output: "scripts/basic/fixdep"},
			{name: "renamed arbitrary helper", source: "tools/host/random_helper.c", output: "tools/host/random_helper"},
		} {
			t.Run(tool+"/"+helper.name, func(t *testing.T) {
				sourceDirectory := path.Dir(helper.source)
				const (
					xallocHeader          = "scripts/include/xalloc.h"
					baseNestedHeader      = "scripts/include/nested/shadowed.h"
					baseRecursiveHeader   = "scripts/include/utility/deep/helper.h"
					unrelatedKernelSource = "scripts/not-included.c"
				)
				arguments := []string{
					"-Wp,-MMD," + sourceDirectory + "/.helper.d",
					"-I", "${tree:kernel}/scripts/include",
					"-I", sourceDirectory,
					"-o", helper.output,
					"${tree:kernel}/" + helper.source,
				}
				plan, node := configDependencyCompilePlanForTest(t, map[string]string{
					helper.source: `#include <stdio.h>
#include <stdlib.h>
/* fixdep deliberately scans CONFIG_COMMENT-like dependency text. */
static const char config_prefix[] = "CONFIG_";
`,
					xallocHeader:          "#define xmalloc(size) malloc(size)\n",
					baseNestedHeader:      "#error shadowed source root must not be projected\n",
					baseRecursiveHeader:   "#define HELPER_HEADER 1\n",
					unrelatedKernelSource: "int unrelated;\n",
				}, arguments, nil)
				for name, profile := range plan.selectionGraph.profiles {
					overlayRoot := t.TempDir()
					mustWriteSource(t, overlayRoot, "detail.h", "#define NESTED_DETAIL 1\n")
					profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__/scripts/include/nested"] = overlayRoot
					if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
						Tree: CompactKbuildInvocationObjectTree,
					}); err != nil {
						t.Fatal(err)
					}
					plan.selectionGraph.profiles[name] = profile
				}

				// Model the bounded compiler-driver portion of the production
				// cmd_and_fixdep scriptrun. The source build links directly and thus
				// deliberately has no -c argument.
				plan.Sources[0].Path = helper.source
				node.Tool = tool
				node.Outputs = []ActionPlanOutput{{Tree: "objects", Path: helper.output}}
				recipe := plan.Recipes[node.Recipe]
				recipe.Tool = compactKbuildScriptRunnerRole
				recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
				recipe.Environment = map[string]string{"KCONFIG_CONFIG": ".config"}
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool: tool, Arguments: arguments,
					WorkingInputUses:         []string{"source:source:00000000"},
					WorkingInputUsesComplete: true,
				}
				recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
				recipe.WorkingInputs = map[string]string{
					"source:source:00000000":          helper.source,
					"source:working-closure:00000001": ".config",
				}
				plan.Recipes[node.Recipe] = recipe
				configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: ".config"}
				plan.Sources = append(plan.Sources, configSource)
				node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
				plan.Nodes[0] = node

				linkRole, ok := toolaction.LinkContractRole(tool)
				if !ok {
					t.Fatalf("LinkContractRole(%q) is unavailable", tool)
				}
				baseRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: tool}
				linkRef := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: linkRole}
				plan.metadata.actionRoles = []KbuildActionRoleRef{baseRef, linkRef}
				plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
					baseRef: {PrefixArguments: []string{"-DBASE_COMPILE_CONTRACT=1"}},
					linkRef: {
						PrefixArguments: []string{"-DDRIVER_LINK_CONTRACT=1"},
						// The real fixdep source reaches compiler-owned libc headers.
						// This configured root is intentionally unavailable to the
						// planner and would make ordinary source-closure analysis opaque.
						SuffixArguments: []string{
							"-isystem", "/configured/libc/include",
							"/configured/lib/libcompiler_rt.a",
						},
					},
				}

				invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
				if reason != "" {
					t.Fatalf("typed driver-link invocation is opaque: %s", reason)
				}
				if invocation.tool != tool {
					t.Fatalf("typed driver-link executable role = %q, want %q", invocation.tool, tool)
				}
				if !slices.Contains(invocation.arguments, "-DDRIVER_LINK_CONTRACT=1") ||
					slices.Contains(invocation.arguments, "-DBASE_COMPILE_CONTRACT=1") {
					t.Fatalf("typed driver-link normalized argv = %q, want only the %s contract", invocation.arguments, linkRole)
				}

				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				if err != nil {
					t.Fatal(err)
				}
				// The link contract selects the right compiler, but cannot prove
				// the source's preprocessing closure from argv alone. Unknown libc
				// headers must preserve full source/config fallback, not a partial
				// translation-unit plus explicit -I projection.
				if !set.Opaque || set.Reason == "" || len(set.SourcePaths) != 0 {
					t.Fatalf("typed driver-link dependency set = %#v, want opaque full-tree fallback", set)
				}

				for _, negative := range []struct {
					name   string
					mutate func(*ActionRecipe)
				}{
					{name: "CONFIG definition", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.Arguments = append(slices.Clone(arguments), "-DCONFIG_HELPER=1")
					}},
					{name: "compiler plugin", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.Arguments = append(slices.Clone(arguments), "-fplugin=/untrusted/plugin.so")
					}},
					{name: "compiler specs", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.Arguments = append(slices.Clone(arguments), "-specs=/untrusted/specs")
					}},
					{name: "source-selected sysroot", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.Arguments = append(slices.Clone(arguments), "--sysroot=/untrusted/sysroot")
					}},
					{name: "include environment", mutate: func(candidate *ActionRecipe) {
						candidate.Environment["CPATH"] = "/untrusted/include"
					}},
					{name: "compiler specs environment", mutate: func(candidate *ActionRecipe) {
						candidate.Environment["GCC_SPECS"] = "/untrusted/specs"
					}},
					{name: "complete working tree", mutate: func(candidate *ActionRecipe) {
						candidate.WorkingTrees = []string{"prep"}
					}},
					{name: "incomplete working input uses", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.WorkingInputUsesComplete = false
						candidate.CompilerInvocation.WorkingInputUses = nil
					}},
					{name: "used config binding", mutate: func(candidate *ActionRecipe) {
						candidate.CompilerInvocation.WorkingInputUses = append(
							candidate.CompilerInvocation.WorkingInputUses,
							"source:working-closure:00000001",
						)
					}},
					{name: "config staged below object include root", mutate: func(candidate *ActionRecipe) {
						candidate.WorkingInputs["source:working-closure:00000001"] = sourceDirectory + "/generated-config.h"
					}},
				} {
					t.Run(negative.name, func(t *testing.T) {
						candidate := cloneActionRecipe(recipe)
						negative.mutate(&candidate)
						plan.Recipes[node.Recipe] = candidate
						set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
						if err != nil {
							t.Fatal(err)
						}
						if !set.Opaque || set.Reason == "" {
							t.Fatalf("unsafe driver-link dependency set = %#v, want opaque fallback", set)
						}
					})
				}

			})
		}
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesHonorsIncludeSearchPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		include   string
		arguments []string
		want      []string
		opaque    bool
	}{
		{
			name:    "external I shadows local isystem",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-I", "/uninspectable/include",
				"-isystem", "${tree:kernel}/include/system",
				"-c", "drivers/example/driver.c",
			},
			opaque: true,
		},
		{
			name:    "I class precedes isystem despite argv order",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-isystem", "${tree:kernel}/include/system",
				"-I", "/uninspectable/include",
				"-c", "drivers/example/driver.c",
			},
			opaque: true,
		},
		{
			name:    "local I precedes external isystem despite argv order",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-isystem", "/uninspectable/include",
				"-I", "${tree:kernel}/include/local",
				"-c", "drivers/example/driver.c",
			},
			want: []string{"CONFIG_LOCAL_INCLUDE"},
		},
		{
			name:    "same class retains local then external order",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-I", "${tree:kernel}/include/local",
				"-I", "/uninspectable/include",
				"-c", "drivers/example/driver.c",
			},
			want: []string{"CONFIG_LOCAL_INCLUDE"},
		},
		{
			name:    "same class retains external then local order",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-I", "/uninspectable/include",
				"-I", "${tree:kernel}/include/local",
				"-c", "drivers/example/driver.c",
			},
			opaque: true,
		},
		{
			name:    "local isystem precedes external idirafter despite argv order",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-idirafter", "/uninspectable/include",
				"-isystem", "${tree:kernel}/include/system",
				"-c", "drivers/example/driver.c",
			},
			want: []string{"CONFIG_SYSTEM_INCLUDE"},
		},
		{
			name:    "external iquote shadows local I for quoted include",
			include: "#include \"selected.h\"\n",
			arguments: []string{
				"-I", "${tree:kernel}/include/local",
				"-iquote", "/uninspectable/include",
				"-c", "drivers/example/driver.c",
			},
			opaque: true,
		},
		{
			name:    "iquote does not apply to angle include",
			include: "#include <selected.h>\n",
			arguments: []string{
				"-iquote", "/uninspectable/include",
				"-I", "${tree:kernel}/include/local",
				"-c", "drivers/example/driver.c",
			},
			want: []string{"CONFIG_LOCAL_INCLUDE"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c":  test.include,
				"include/local/selected.h":  "CONFIG_LOCAL_INCLUDE\n",
				"include/system/selected.h": "CONFIG_SYSTEM_INCLUDE\n",
			}, test.arguments, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if test.opaque {
				if !set.Opaque || !strings.Contains(set.Reason, "shadowed") {
					t.Fatalf("config dependency set = %#v, want uninspectable-shadow fallback", set)
				}
				return
			}
			if set.Opaque || !slices.Equal(set.Symbols, test.want) {
				t.Fatalf("config dependency set = %#v, want symbols %q", set, test.want)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRecognizesConfigProvenanceForStagedAutoconfSource(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#if CONFIG_STAGED_AUTOCONF\nint enabled;\n#endif\n",
	}, []string{
		"-include", "${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"}
	plan.Sources = append(plan.Sources, configSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
	recipe.WorkingInputs = map[string]string{
		"source:working-closure:00000001": "include/generated/autoconf.h",
	}
	plan.Recipes[node.Recipe] = recipe
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_STAGED_AUTOCONF"}) {
		t.Fatalf("staged autoconf dependency set = %#v", set)
	}
	if !slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("staged autoconf file closure = %#v, want config projection", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansStagedKernelSourceInsteadOfDestination(t *testing.T) {
	const sourcePath = "include/custom/staged-autoconf.h"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		sourcePath:                 "CONFIG_STAGED_SOURCE\n",
	}, []string{
		"-include", "${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	stagedSource := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: sourcePath}
	plan.Sources = append(plan.Sources, stagedSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: stagedSource.ID})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
	recipe.WorkingInputs = map[string]string{
		"source:working-closure:00000001": configDependencyAutoconfPath,
	}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_STAGED_SOURCE"}) {
		t.Fatalf("staged kernel-source dependency set = %#v, want original source contents", set)
	}
	if !slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c", sourcePath}) || len(set.ObjectPaths) != 0 {
		t.Fatalf("staged kernel-source file closure = %#v, want source provenance only", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsUnprovenStagedAutoconfSource(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-include", "${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	recipe := plan.Recipes[node.Recipe]
	recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
	recipe.WorkingInputs = map[string]string{
		"source:working-closure:00000001": configDependencyAutoconfPath,
	}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unproven source binding") {
		t.Fatalf("unproven staged autoconf dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsPositionalConfigSourceBinding(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"}
	plan.Sources = append(plan.Sources, configSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "config source binding outside a modeled forced include") {
		t.Fatalf("positional config-source dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsNonTranslationUnitSourceBinding(t *testing.T) {
	const sourcePath = "include/custom/compiler-data.h"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		sourcePath:                 "CONFIG_COMPILER_DATA\n",
	}, []string{
		"${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	dataSource := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: sourcePath}
	plan.Sources = append(plan.Sources, dataSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: dataSource.ID})
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-translation-unit source binding") {
		t.Fatalf("non-TU source dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsForcedNonAutoconfConfigSource(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-include", "${source:working-closure:00000001}",
		"-c", "drivers/example/driver.c",
	}, nil)
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "auto.conf"}
	plan.Sources = append(plan.Sources, configSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-autoconf config projection include/config/auto.conf") {
		t.Fatalf("forced non-autoconf config-source dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsConfigSourceOutsideCompilerProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe)
	}{
		{
			name: "outer argument",
			mutate: func(recipe *ActionRecipe) {
				recipe.Arguments = append(recipe.Arguments, "${source:working-closure:00000001}")
			},
		},
		{
			name: "environment",
			mutate: func(recipe *ActionRecipe) {
				recipe.Environment = map[string]string{"CONFIG_BYTES": "${source:working-closure:00000001}"}
			},
		},
		{
			name: "stdin",
			mutate: func(recipe *ActionRecipe) {
				recipe.Stdin = "source:working-closure:00000001"
			},
		},
		{
			name: "content substitution",
			mutate: func(recipe *ActionRecipe) {
				recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
					"config": {Input: "source:working-closure:00000001", Transform: ActionRecipeContentTransformMakeShellWord},
				}
			},
		},
		{
			name: "command replay",
			mutate: func(recipe *ActionRecipe) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Arguments: []string{"${source:working-closure:00000001}"},
					}},
				}}
			},
		},
		{
			name: "auxiliary compound command",
			mutate: func(recipe *ActionRecipe) {
				reference := "source:working-closure:00000001"
				recipe.CompilerInvocation.WorkingInputUsesComplete = true
				recipe.CompilerInvocation.WorkingInputUses = []string{reference}
				recipe.CompilerInvocation.AuxiliaryWorkingInputUses = []string{reference}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
			}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"}
			plan.Sources = append(plan.Sources, configSource)
			node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
			plan.Nodes[0] = node
			recipe := plan.Recipes[node.Recipe]
			recipe.Tool = compactKbuildScriptRunnerRole
			recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
				Tool: "cc", Arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			}
			recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
			recipe.WorkingInputs = map[string]string{
				"source:working-closure:00000001": configDependencyAutoconfPath,
			}
			test.mutate(&recipe)
			plan.Recipes[node.Recipe] = recipe

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "outside its modeled compiler invocation") {
				t.Fatalf("config source semantic use dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsIncompleteCompoundWithStagedConfig(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"}
	plan.Sources = append(plan.Sources, configSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: configSource.ID})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Tool = compactKbuildScriptRunnerRole
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
		Tool: "cc", Arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"},
	}
	recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
	recipe.WorkingInputs = map[string]string{
		"source:working-closure:00000001": configDependencyAutoconfPath,
	}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "incomplete working-input projection") {
		t.Fatalf("incomplete compound dependency set = %#v, want opaque staged-config fallback", set)
	}
}

func configDependencyNonCompilerConfigSourcePlanForTest() (*ActionPlan, ActionPlanNode, string) {
	const (
		recipeID = "non-compiler-config-source-recipe"
		binding  = "config:00000000"
	)
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "archive",
		Tool:   "ar",
		Arguments: []string{
			"rcs", "${output:00000000}",
		},
		WorkingInputs: map[string]string{"source:" + binding: ".config"},
		Sources:       []string{binding},
		Outputs:       []string{"00000000"},
	}
	node := ActionPlanNode{
		ID: "non-compiler-config-source", Stage: "target", Kind: "archive",
		Recipe: recipeID, Tool: "ar", Product: "image",
		Sources: []ActionPlanSourceEdge{{Role: "config", SourceID: "src-00000001"}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/built-in.a"}},
	}
	return &ActionPlan{
		Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "config", Path: ".config"}},
		Recipes: map[string]ActionRecipe{recipeID: recipe},
		Nodes:   []ActionPlanNode{node},
	}, node, binding
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsNonCompilerConfigSourceSemanticUse(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe, string)
	}{
		{
			name: "argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Arguments = append(recipe.Arguments, "${source:"+binding+"}")
			},
		},
		{
			name: "environment",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Environment = map[string]string{"CONFIG_BYTES": "${source:" + binding + "}"}
			},
		},
		{
			name: "stdin",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdin = "source:" + binding
			},
		},
		{
			name: "stdout",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdout = "source:" + binding
			},
		},
		{
			name: "compiler invocation argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool: "cc", Arguments: []string{"${source:" + binding + "}"},
				}
			},
		},
		{
			name: "content substitution",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
					"config": {Input: "source:" + binding, Transform: ActionRecipeContentTransformMakeShellWord},
				}
			},
		},
		{
			name: "command replay argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Arguments: []string{"${source:" + binding + "}"},
					}},
				}}
			},
		},
		{
			name: "command replay output",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Outputs: []string{"${source:" + binding + "}"},
					}},
				}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node, binding := configDependencyNonCompilerConfigSourcePlanForTest()
			recipe := plan.Recipes[node.Recipe]
			test.mutate(&recipe, binding)
			plan.Recipes[node.Recipe] = recipe

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "non-compiler recipe uses config source binding") {
				t.Fatalf("non-compiler config-source dependency set = %#v, want opaque semantic-use fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsStagedUnusedNonCompilerConfigSource(t *testing.T) {
	plan, node, _ := configDependencyNonCompilerConfigSourcePlanForTest()
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
		t.Fatalf("staged unused non-compiler config-source dependency set = %#v, want config-free", set)
	}
}

func configDependencyAttachFallbackProjectionInputForTest(
	t *testing.T,
	plan *ActionPlan,
	node *ActionPlanNode,
	projectionInput string,
	projectionOutput string,
) string {
	t.Helper()
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: projectionInput}
	plan.Sources = append(plan.Sources, configSource)
	copyRecipeID := "fallback-config-copy"
	copyRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "copy",
		Tool:   "actionfile",
		Arguments: []string{
			"-input", "${source:input:00000000}",
			"-out", "${output:00000000}",
		},
		Sources: []string{"input:00000000"},
		Outputs: []string{"00000000"},
	}
	copyNode := ActionPlanNode{
		ID: "fallback-config-copy", Stage: "prep", Kind: "copy", Recipe: copyRecipeID,
		Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSource.ID}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: projectionOutput}},
	}
	plan.Recipes[copyRecipeID] = copyRecipe
	plan.Nodes = append(plan.Nodes, copyNode)
	binding := "working-closure:" + planOrdinal(len(node.Inputs))
	node.Inputs = append(node.Inputs, ActionPlanNodeEdge{
		Role: "working-closure", ProducerID: copyNode.ID, Slot: 0,
	})
	consumerRecipe := plan.Recipes[node.Recipe]
	consumerRecipe.Inputs = append(consumerRecipe.Inputs, binding)
	plan.Recipes[node.Recipe] = consumerRecipe
	plan.Nodes[0] = *node
	return binding
}

func configDependencyNonCompilerConfigInputPlanForTest(t *testing.T) (*ActionPlan, ActionPlanNode, string) {
	t.Helper()
	const recipeID = "non-compiler-config-input-recipe"
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema,
		Kind:   "archive",
		Tool:   "ar",
		Arguments: []string{
			"rcs", "${output:00000000}",
		},
		Outputs: []string{"00000000"},
	}
	node := ActionPlanNode{
		ID: "non-compiler-config-input", Stage: "target", Kind: "archive",
		Recipe: recipeID, Tool: "ar", Product: "image",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/built-in.a"}},
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{recipeID: recipe},
		Nodes:   []ActionPlanNode{node},
	}
	binding := configDependencyAttachFallbackProjectionInputForTest(t, plan, &node, ".config", ".config")
	recipe = plan.Recipes[node.Recipe]
	if recipe.WorkingInputs == nil {
		recipe.WorkingInputs = map[string]string{}
	}
	recipe.WorkingInputs["input:"+binding] = ".config"
	plan.Recipes[node.Recipe] = recipe
	return plan, node, binding
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsNonCompilerConfigInputSemanticUse(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe, string)
	}{
		{
			name: "argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Arguments = append(recipe.Arguments, "${input:"+binding+"}")
			},
		},
		{
			name: "environment",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Environment = map[string]string{"CONFIG_BYTES": "${input:" + binding + "}"}
			},
		},
		{
			name: "stdin",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdin = "input:" + binding
			},
		},
		{
			name: "stdout",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdout = "input:" + binding
			},
		},
		{
			name: "compiler invocation argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool: "cc", Arguments: []string{"${input:" + binding + "}"},
				}
			},
		},
		{
			name: "content substitution",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
					"config": {Input: "input:" + binding, Transform: ActionRecipeContentTransformMakeShellWord},
				}
			},
		},
		{
			name: "command replay argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Arguments: []string{"${input:" + binding + "}"},
					}},
				}}
			},
		},
		{
			name: "command replay output",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Outputs: []string{"${input:" + binding + "}"},
					}},
				}}
			},
		},
		{
			name: "tool",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Tool = "input:" + binding
			},
		},
		{
			name: "executable input",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ExecutableInputs = []string{binding}
			},
		},
		{
			name: "observed output base",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ObservedOutputBases = map[string][]string{"00000000": {binding}}
			},
		},
		{
			name: "auxiliary compound command",
			mutate: func(recipe *ActionRecipe, binding string) {
				reference := "input:" + binding
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{}
				recipe.CompilerInvocation.WorkingInputUsesComplete = true
				recipe.CompilerInvocation.WorkingInputUses = []string{reference}
				recipe.CompilerInvocation.AuxiliaryWorkingInputUses = []string{reference}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node, binding := configDependencyNonCompilerConfigInputPlanForTest(t)
			recipe := plan.Recipes[node.Recipe]
			test.mutate(&recipe, binding)
			plan.Recipes[node.Recipe] = recipe

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "non-compiler recipe uses config input binding") {
				t.Fatalf("non-compiler config-input dependency set = %#v, want opaque semantic-use fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsStagedUnusedNonCompilerConfigInput(t *testing.T) {
	plan, node, _ := configDependencyNonCompilerConfigInputPlanForTest(t)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
		t.Fatalf("staged unused non-compiler config-input dependency set = %#v, want config-free", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesModelsForcedAutoconfFallbackInput(t *testing.T) {
	const binding = "working-closure:00000000"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_INPUT_AUTOCONF\n",
	}, []string{
		"-include", "${input:" + binding + "}",
		"-c", "drivers/example/driver.c",
	}, nil)
	if got := configDependencyAttachFallbackProjectionInputForTest(
		t, plan, &node, "autoconf.h", configDependencyAutoconfPath,
	); got != binding {
		t.Fatalf("fallback input binding = %q, want %q", got, binding)
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_INPUT_AUTOCONF"}) ||
		!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("forced fallback autoconf input dependency set = %#v, want precise projection", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsForcedNonAutoconfFallbackInput(t *testing.T) {
	const binding = "working-closure:00000000"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-include", "${input:" + binding + "}",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencyAttachFallbackProjectionInputForTest(
		t, plan, &node, "auto.conf", "include/config/auto.conf",
	)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "non-autoconf config projection include/config/auto.conf") {
		t.Fatalf("forced non-autoconf fallback input dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsPositionalConfigFallbackInput(t *testing.T) {
	const binding = "working-closure:00000000"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"${input:" + binding + "}",
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencyAttachFallbackProjectionInputForTest(
		t, plan, &node, "autoconf.h", configDependencyAutoconfPath,
	)

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "config input binding outside a modeled forced autoconf include") {
		t.Fatalf("positional fallback input dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesDoesNotInferAutoconfFromStagedInputDestination(t *testing.T) {
	const binding = "working-closure:00000000"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-include", "${input:" + binding + "}",
		"-c", "drivers/example/driver.c",
	}, nil)
	producer := ActionPlanNode{
		ID: "ordinary-generated-input", Stage: "prep", Kind: "generate", Recipe: "ordinary-generated-recipe",
		Tool: "awk", Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/ordinary.h"}},
	}
	plan.Recipes[producer.Recipe] = ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "awk", Outputs: []string{"00000000"},
	}
	plan.Nodes = append(plan.Nodes, producer)
	node.Inputs = append(node.Inputs, ActionPlanNodeEdge{
		Role: "working-closure", ProducerID: producer.ID, Slot: 0,
	})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Inputs = []string{binding}
	recipe.WorkingInputs = map[string]string{
		"input:" + binding: configDependencyAutoconfPath,
	}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unmodeled generated input binding") {
		t.Fatalf("destination-only autoconf input dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsConfigFallbackInputOutsideCompilerProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe, string)
	}{
		{
			name: "outer argument",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Arguments = append(recipe.Arguments, "${input:"+binding+"}")
			},
		},
		{
			name: "environment",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Environment = map[string]string{"CONFIG_BYTES": "${input:" + binding + "}"}
			},
		},
		{
			name: "stdin",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdin = "input:" + binding
			},
		},
		{
			name: "stdout",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Stdout = "input:" + binding
			},
		},
		{
			name: "content substitution",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
					"config": {Input: "input:" + binding, Transform: ActionRecipeContentTransformMakeShellWord},
				}
			},
		},
		{
			name: "command replay",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.CommandReplays = []ActionRecipeCommandReplay{{
					Name: "replay",
					Invocations: []ActionRecipeCommandReplayInvocation{{
						Arguments: []string{"${input:" + binding + "}"},
					}},
				}}
			},
		},
		{
			name: "tool",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.Tool = "input:" + binding
			},
		},
		{
			name: "executable input",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ExecutableInputs = []string{binding}
			},
		},
		{
			name: "observed output base",
			mutate: func(recipe *ActionRecipe, binding string) {
				recipe.ObservedOutputBases = map[string][]string{"00000000": {binding}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
			}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			binding := configDependencyAttachFallbackProjectionInputForTest(
				t, plan, &node, "autoconf.h", configDependencyAutoconfPath,
			)
			recipe := plan.Recipes[node.Recipe]
			recipe.Tool = compactKbuildScriptRunnerRole
			recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
				Tool: "cc", Arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"},
			}
			test.mutate(&recipe, binding)
			plan.Recipes[node.Recipe] = recipe

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "config input binding outside its modeled compiler invocation") {
				t.Fatalf("config input semantic use dependency set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsCompilerContentSubstitution(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c":  "CONFIG_DRIVER\n",
		"drivers/example/flags.txt": "CONFIG_FROM_CONTENT\n",
	}, []string{
		"-DVALUE=${content:config_define}",
		"-c", "drivers/example/driver.c",
	}, nil)
	contentSource := ActionPlanSource{
		ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/flags.txt",
	}
	plan.Sources = append(plan.Sources, contentSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "content", SourceID: contentSource.ID})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Sources = []string{"source:00000000", "content:00000001"}
	recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{
		"config_define": {
			Input: "source:content:00000001", Transform: ActionRecipeContentTransformMakeShellWord,
		},
	}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unmodeled content substitution") {
		t.Fatalf("compiler content-substitution dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsCompilerArgumentTransform(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-DCONFIG_TRANSFORMED=1",
		"-c", "drivers/example/driver.c",
	}, nil)
	recipe := plan.Recipes[node.Recipe]
	recipe.ArgumentTransforms = []ActionRecipeArgumentTransform{{
		Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
	}}
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unmodeled argument transform") {
		t.Fatalf("compiler argument-transform dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsOrdinarySourceStdin(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"drivers/example/stdin.c":  "CONFIG_STDIN\n",
	}, []string{
		"-nostdinc", "-x", "c", "-c", "drivers/example/driver.c", "-",
	}, nil)
	stdinSource := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/stdin.c"}
	plan.Sources = append(plan.Sources, stdinSource)
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "stdin", SourceID: stdinSource.ID})
	plan.Nodes[0] = node
	recipe := plan.Recipes[node.Recipe]
	recipe.Sources = append(recipe.Sources, "stdin:00000001")
	recipe.Stdin = "source:stdin:00000001"
	plan.Recipes[node.Recipe] = recipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unmodeled stdin stream") {
		t.Fatalf("compiler stdin dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		source    string
		arguments []string
		extra     map[string]string
	}{
		{
			name: "macro include", source: "#include SELECTED_HEADER\n",
			arguments: []string{"-c", "drivers/example/driver.c"},
		},
		{
			name: "uninspectable system include", source: "#include <stddef.h>\n",
			arguments: []string{"-isystem=toolchain/include", "-c", "drivers/example/driver.c"},
		},
		{
			name: "non-autoconf projection", source: "CONFIG_VALUE\n",
			arguments: []string{"-include", "${tree:prep}/include/generated/rustc_cfg", "-c", "drivers/example/driver.c"},
		},
		{
			name: "legacy I separator", source: "#include \"local.h\"\n",
			arguments: []string{"-I-", "-c", "drivers/example/driver.c"},
			extra:     map[string]string{"drivers/example/local.h": "CONFIG_LOCAL\n"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{"drivers/example/driver.c": test.source}
			for pathname, contents := range test.extra {
				files[pathname] = contents
			}
			plan, node := configDependencyCompilePlanForTest(t, files, test.arguments, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || set.Reason == "" {
				t.Fatalf("config dependency set = %#v, want opaque reason", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesFailsClosedOnGeneratedInclude(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <generated.h>\n",
	}, []string{
		"-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "generated-header", Stage: "prep", Kind: "generate", Recipe: "header-recipe", Tool: "actionfile",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/generated.h"}},
	})
	plan.Recipes["header-recipe"] = ActionRecipe{Kind: "generate", Tool: "actionfile"}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "generated object-tree input") {
		t.Fatalf("generated include config dependency set = %#v, want opaque fallback", set)
	}
}

func configDependencyStageGeneratedHeaderForTest(
	t *testing.T,
	plan *ActionPlan,
	node *ActionPlanNode,
	producerID string,
	pathname string,
	producerRecipe ActionRecipe,
) {
	t.Helper()
	producerRecipeID := producerID + "-recipe"
	producerRecipe.Schema = LinuxKernelPlanSchema
	producerRecipe.Kind = "generate"
	producerRecipe.Outputs = []string{"00000000"}
	producer := ActionPlanNode{
		ID: producerID, Stage: "prep", Kind: "generate", Recipe: producerRecipeID, Tool: producerRecipe.Tool,
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	plan.Recipes[producerRecipeID] = producerRecipe
	plan.Nodes = append(plan.Nodes, producer)

	binding := "generated:" + planOrdinal(len(node.Inputs))
	node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: "generated", ProducerID: producerID, Slot: 0})
	compileRecipe := plan.Recipes[node.Recipe]
	compileRecipe.Inputs = append(compileRecipe.Inputs, binding)
	if compileRecipe.WorkingDirectory == "" {
		compileRecipe.WorkingDirectory = "compile"
	}
	if compileRecipe.WorkingInputs == nil {
		compileRecipe.WorkingInputs = map[string]string{}
	}
	compileRecipe.WorkingInputs["input:"+binding] = pathname
	plan.Recipes[node.Recipe] = compileRecipe
	for index := range plan.Nodes {
		if plan.Nodes[index].ID == node.ID {
			plan.Nodes[index] = *node
			break
		}
	}
}

func configDependencyInsertInputSetEntryForTest(
	t *testing.T,
	plan *ActionPlan,
	root string,
	entry ActionPlanInputSetEntry,
) string {
	t.Helper()
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	root, err = store.Insert(root, entry)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansPersistentInputSetClosure(t *testing.T) {
	const generatedPath = "include/generated/persistent.h"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <persistent.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencyStageGeneratedHeaderForTest(t, plan, &node, "persistent-header", generatedPath, ActionRecipe{
		Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_PERSISTENT_HEADER",
		},
	})
	generated := node.Inputs[0]
	sourceID := node.Sources[0].SourceID
	node.Sources = nil
	node.Inputs = nil
	node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
		Target:      ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "drivers/example/driver.c"},
		SourceID:    sourceID,
		CompilerUse: true,
	})
	node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, node.InputSet, ActionPlanInputSetEntry{
		Target:      ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: generatedPath},
		ProducerID:  generated.ProducerID,
		Slot:        generated.Slot,
		CompilerUse: true,
	})
	recipe := plan.Recipes[node.Recipe]
	recipe.Inputs = nil
	recipe.WorkingInputs = nil
	recipe.WorkingDirectory = ""
	plan.Recipes[node.Recipe] = recipe
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_PERSISTENT_HEADER"}) ||
		!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) ||
		!slices.Equal(set.ObjectPaths, []string{generatedPath}) {
		t.Fatalf("persistent source/generated closure dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesHonorsPersistentAutoconfShadow(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-include", "${tree:prep}/" + configDependencyAutoconfPath,
		"-c", "drivers/example/driver.c",
	}, nil)
	configDependencyStageGeneratedHeaderForTest(t, plan, &node, "autoconf-shadow", configDependencyAutoconfPath, ActionRecipe{
		Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_PERSISTENT_SHADOW",
		},
	})
	shadow := node.Inputs[0]
	node.Inputs = nil
	node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
		Target:      ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: configDependencyAutoconfPath},
		ProducerID:  shadow.ProducerID,
		Slot:        shadow.Slot,
		CompilerUse: true,
	})
	recipe := plan.Recipes[node.Recipe]
	recipe.Inputs = nil
	recipe.WorkingInputs = nil
	recipe.WorkingDirectory = ""
	plan.Recipes[node.Recipe] = recipe
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_PERSISTENT_SHADOW"}) ||
		!slices.Equal(set.ObjectPaths, []string{configDependencyAutoconfPath}) {
		t.Fatalf("persistent ordinary autoconf-path shadow dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesClassifiesPersistentConfigProjectionUse(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	recipe := plan.Recipes[node.Recipe]
	recipe.Tool = compactKbuildScriptRunnerRole
	recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
		Tool: "cc", Arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"},
	}
	plan.Recipes[node.Recipe] = recipe
	configSource := ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"}
	plan.Sources = append(plan.Sources, configSource)
	node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: configDependencyAutoconfPath},
		SourceID: configSource.ID,
	})
	plan.Nodes[0] = node

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "incomplete working-input projection") {
		t.Fatalf("incomplete persistent config closure dependency set = %#v, want opaque", set)
	}

	recipe.CompilerInvocation.WorkingInputUsesComplete = true
	plan.Recipes[node.Recipe] = recipe
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
		t.Fatalf("complete persistent ambient config closure dependency set = %#v", set)
	}

	recipe.CompilerInvocation.WorkingInputUsesComplete = false
	plan.Recipes[node.Recipe] = recipe
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	entry, found, err := store.Lookup(node.InputSet, ActionPlanInputSetTarget{
		Kind: ActionPlanInputSetWorkTarget, Path: configDependencyAutoconfPath,
	})
	if err != nil || !found {
		t.Fatalf("lookup persistent config entry: found=%t err=%v", found, err)
	}
	entry.CompilerUse = true
	entry.AuxiliaryUse = true
	node.InputSet, err = store.Replace(node.InputSet, entry)
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes[0] = node
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "outside its modeled compiler invocation") {
		t.Fatalf("persistent auxiliary config use dependency set = %#v, want opaque", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansExactGeneratedText(t *testing.T) {
	tests := []struct {
		name          string
		producer      ActionRecipe
		generatedPath string
		include       string
		files         map[string]string
		wantSymbols   []string
		wantSources   []string
	}{
		{
			name: "literal lines follow source closure",
			producer: ActionRecipe{Tool: "actionfile", Arguments: []string{
				"-out", "${output:00000000}",
				"-line", "#include <linux/projected-nested.h>",
				"-line", "CONFIG_GENERATED_LINE",
			}},
			generatedPath: "include/generated/projected.h",
			include:       "#include <projected.h>\nCONFIG_DRIVER\n",
			files: map[string]string{
				"include/linux/projected-nested.h": "CONFIG_GENERATED_NESTED\n",
			},
			wantSymbols: []string{"CONFIG_DRIVER", "CONFIG_GENERATED_LINE", "CONFIG_GENERATED_NESTED"},
			wantSources: []string{"drivers/example/driver.c", "include/linux/projected-nested.h"},
		},
		{
			name: "literal base64 permits empty bytes",
			producer: ActionRecipe{Tool: "actionfile", Arguments: []string{
				"-out", "${output:00000000}", "-content_base64", base64.StdEncoding.EncodeToString(nil),
			}},
			generatedPath: "include/generated/empty.h",
			include:       "#include <empty.h>\nCONFIG_DRIVER\n",
			wantSymbols:   []string{"CONFIG_DRIVER"},
			wantSources:   []string{"drivers/example/driver.c"},
		},
		{
			name: "literal hermetic runtime stdout",
			producer: ActionRecipe{
				Tool: compactKbuildScriptRuntimeRole,
				Arguments: []string{
					"echo", "#include <asm-generic/projected.h>",
				},
				Stdout: "00000000",
			},
			generatedPath: "arch/x86/include/generated/asm/projected.h",
			include:       "#include <asm/projected.h>\nCONFIG_DRIVER\n",
			files: map[string]string{
				"include/asm-generic/projected.h": "CONFIG_ASM_GENERIC_PROJECTED\n",
			},
			wantSymbols: []string{"CONFIG_ASM_GENERIC_PROJECTED", "CONFIG_DRIVER"},
			wantSources: []string{"drivers/example/driver.c", "include/asm-generic/projected.h"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := map[string]string{"drivers/example/driver.c": test.include}
			for pathname, contents := range test.files {
				files[pathname] = contents
			}
			includeRoot := "${tree:prep}/include/generated"
			if strings.HasPrefix(test.generatedPath, "arch/x86/") {
				includeRoot = "${tree:prep}/arch/x86/include/generated"
			}
			plan, node := configDependencyCompilePlanForTest(t, files, []string{
				"-nostdinc", "-I" + includeRoot, "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
			}, nil)
			configDependencyStageGeneratedHeaderForTest(t, plan, &node, "generated-header", test.generatedPath, test.producer)
			if test.producer.Tool == compactKbuildScriptRuntimeRole {
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				producerRef := KbuildActionRoleRef{Scope: "target", Role: compactKbuildScriptRuntimeRole}
				plan.metadata.actionRoles = append(plan.metadata.actionRoles, producerRef)
				plan.metadata.actionContracts[producerRef] = CompactKbuildActionContract{}
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque || !slices.Equal(set.Symbols, test.wantSymbols) ||
				!slices.Equal(set.SourcePaths, test.wantSources) ||
				!slices.Equal(set.ObjectPaths, []string{test.generatedPath}) {
				t.Fatalf("exact generated text dependency set = %#v", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScopesGeneratedTextToBoundProducer(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <selected.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	path := "include/generated/selected.h"
	configDependencyStageGeneratedHeaderForTest(t, plan, &node, "header-a", path, ActionRecipe{
		Tool: "actionfile", Arguments: []string{
			"-out", "${output:00000000}", "-line", "CONFIG_SELECTED_A",
		},
	})
	producerBRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_SELECTED_B"},
		Outputs:   []string{"00000000"},
	}
	plan.Recipes["header-b-recipe"] = producerBRecipe
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "header-b", Stage: "prep", Kind: "generate", Recipe: "header-b-recipe", Tool: "actionfile",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: path}},
	})
	context := newConfigDependencyAnalysisContext(plan)
	first, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil {
		t.Fatal(err)
	}
	if first.Opaque || !slices.Equal(first.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SELECTED_A"}) {
		t.Fatalf("first selected generated producer = %#v", first)
	}
	selectedB := node
	selectedB.Inputs[0].ProducerID = "header-b"
	second, err := analyzeActionPlanNodeConfigDependencies(plan, selectedB, context)
	if err != nil {
		t.Fatal(err)
	}
	if second.Opaque || !slices.Equal(second.Symbols, []string{"CONFIG_DRIVER", "CONFIG_SELECTED_B"}) {
		t.Fatalf("second selected generated producer = %#v", second)
	}
	unbound := node
	unbound.Inputs = nil
	missing, err := analyzeActionPlanNodeConfigDependencies(plan, unbound, context)
	if err != nil {
		t.Fatal(err)
	}
	if !missing.Opaque || !strings.Contains(missing.Reason, "unavailable generated object-tree input") {
		t.Fatalf("unbound generated producer = %#v, want opaque fallback", missing)
	}
}

func configDependencyUnrecordedTreeGeneratedHeaderPlanForTest(
	t *testing.T,
) (*ActionPlan, ActionPlanNode, compactKbuildSelectionKey, compactKbuildSelectionKey, string) {
	t.Helper()
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <selected.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/selected.h"
	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRuntimeRole,
		Arguments: []string{"echo", "CONFIG_TREE_SELECTED"}, Stdout: "00000000",
		Outputs: []string{"00000000"},
	}
	producer := ActionPlanNode{
		ID: "tree-header", Stage: "prep", Kind: "generate", Recipe: "tree-header-recipe", Tool: compactKbuildScriptRuntimeRole,
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	plan.Recipes[producer.Recipe] = producerRecipe
	plan.Nodes = append(plan.Nodes, producer)
	compileRecipe := plan.Recipes[node.Recipe]
	compileRecipe.Trees = []string{"prep"}
	plan.Recipes[node.Recipe] = compileRecipe
	node.Trees = []string{"prep"}
	plan.Nodes[0] = node

	consumer := compactKbuildSelectionKey{
		profile: "config-dependency", target: "drivers/example/driver.o", stage: "target",
	}
	owner := compactKbuildSelectionKey{
		profile: "config-dependency", target: pathname, stage: "prep",
	}
	graph := plan.selectionGraph
	graph.selections[owner] = CompactKbuildSelection{Profile: owner.profile, Target: owner.target, Stage: owner.stage}
	graph.materializedProducers[owner] = producer.ID
	graph.outputOwnersByPath = map[string][]compactKbuildSelectionKey{pathname: {owner}}
	graph.selectionsByProfileTarget = map[compactKbuildProfileTargetKey]compactKbuildSelectionKey{
		{profile: owner.profile, target: owner.target}: owner,
	}
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	runtimeRef := KbuildActionRoleRef{Scope: "target", Role: compactKbuildScriptRuntimeRole}
	plan.metadata.actionRoles = append(plan.metadata.actionRoles, runtimeRef)
	plan.metadata.actionContracts[runtimeRef] = CompactKbuildActionContract{}
	return plan, node, consumer, owner, pathname
}

func TestAnalyzeActionPlanNodeConfigDependenciesResolvesGeneratedTextFromTreeVisibility(t *testing.T) {
	plan, node, consumer, owner, pathname := configDependencyUnrecordedTreeGeneratedHeaderPlanForTest(t)
	graph := plan.selectionGraph
	graph.selectionInitialArtifacts = map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{
		consumer: {{Path: pathname, Profile: owner.profile, Target: owner.target}},
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_TREE_SELECTED"}) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("tree-frontier generated text dependency set = %#v", set)
	}

	delete(graph.selectionInitialArtifacts, consumer)
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_TREE_SELECTED"}) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("compiler-discovered prior-tree generated text dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesOnlyUniqueCanonicalUnrecordedTreeWriter(t *testing.T) {
	for _, test := range []struct {
		name            string
		shadowCanonical bool
		wantOpaque      bool
	}{
		{name: "noncanonical shadow and canonical winner", shadowCanonical: false},
		{name: "ambiguous canonical winners", shadowCanonical: true, wantOpaque: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node, _, owner, pathname := configDependencyUnrecordedTreeGeneratedHeaderPlanForTest(t)
			shadowRecipe := ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_TREE_SHADOW"},
				Outputs:   []string{"00000000"},
			}
			shadowOutput := ActionPlanOutput{
				Tree: "prep", Path: pathname,
			}
			if !test.shadowCanonical {
				shadowOutput.ArtifactPath = ".linux-bzl-versions/shadow/" + pathname
			}
			plan.Recipes["tree-header-shadow-recipe"] = shadowRecipe
			plan.Nodes = append(plan.Nodes, ActionPlanNode{
				ID: "tree-header-shadow", Stage: "prep", Kind: "generate",
				Recipe: "tree-header-shadow-recipe", Tool: "actionfile",
				Outputs: []ActionPlanOutput{shadowOutput},
			})

			graph := plan.selectionGraph
			shadowProfile := graph.profiles[owner.profile]
			shadowProfile.Name = "config-dependency-shadow"
			graph.profiles[shadowProfile.Name] = shadowProfile
			shadowOwner := compactKbuildSelectionKey{
				profile: shadowProfile.Name, target: pathname, stage: "prep",
			}
			graph.selections[shadowOwner] = CompactKbuildSelection{
				Profile: shadowOwner.profile, Target: shadowOwner.target, Stage: shadowOwner.stage,
			}
			graph.materializedProducers[shadowOwner] = "tree-header-shadow"
			graph.outputOwnersByPath[pathname] = append(graph.outputOwnersByPath[pathname], shadowOwner)
			graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
				profile: shadowOwner.profile, target: shadowOwner.target,
			}] = shadowOwner

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantOpaque {
				if !set.Opaque || !strings.Contains(set.Reason, "unavailable generated object-tree input") {
					t.Fatalf("ambiguous canonical tree writers set = %#v, want opaque fallback", set)
				}
				return
			}
			if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_TREE_SELECTED"}) ||
				!slices.Equal(set.ObjectPaths, []string{pathname}) {
				t.Fatalf("canonical winner with noncanonical shadow set = %#v", set)
			}
		})
	}
}

func configDependencyReferenceUniqueCanonicalOutput(
	context *configDependencyAnalysisContext,
	logical string,
	node ActionPlanNode,
	recipe ActionRecipe,
) (configDependencyCanonicalOutput, bool) {
	candidates := []configDependencyCanonicalOutput{}
	for producerID, producer := range context.nodes {
		for slot, output := range producer.Outputs {
			if canonicalKbuildRulePath(output.Path) != logical || output.Tree == "" ||
				!actionPlanOutputIsCanonical(output) ||
				!slices.Contains(recipe.Trees, output.Tree) || !slices.Contains(node.Trees, output.Tree) {
				continue
			}
			candidates = append(candidates, configDependencyCanonicalOutput{
				producerID: producerID,
				stage:      producer.Stage,
				tree:       output.Tree,
				slot:       slot,
			})
		}
	}
	if len(candidates) != 1 {
		return configDependencyCanonicalOutput{}, false
	}
	return candidates[0], true
}

func TestConfigDependencyCanonicalOutputIndexMatchesFullNodeScan(t *testing.T) {
	const (
		uniquePath    = "include/generated/unique.h"
		ambiguousPath = "include/generated/ambiguous.h"
		oldPath       = "include/generated/replaced.h"
		finalPath     = "include/generated/final.h"
	)
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{
			ID: "unique", Stage: "prep",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: uniquePath}},
		},
		{
			ID: "noncanonical-shadow", Stage: "prep",
			Outputs: []ActionPlanOutput{{
				Tree: "prep", Path: uniquePath,
				ArtifactPath: ".linux-bzl-versions/shadow/" + uniquePath,
			}},
		},
		{
			ID: "other-tree", Stage: "prep",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: uniquePath}},
		},
		{
			ID: "ambiguous-a", Stage: "prep",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: ambiguousPath}},
		},
		{
			ID: "ambiguous-b", Stage: "prep",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: ambiguousPath}},
		},
		{
			ID: "empty-tree", Stage: "prep",
			Outputs: []ActionPlanOutput{{Path: "include/generated/empty-tree.h"}},
		},
		{
			ID: "repeated-id", Stage: "prep",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: oldPath}},
		},
		{
			ID: "repeated-id", Stage: "host",
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: finalPath}},
		},
	}}
	context := newConfigDependencyAnalysisContext(plan)
	for _, test := range []struct {
		name         string
		logical      string
		trees        []string
		wantFound    bool
		wantProducer string
	}{
		{name: "unique declared tree", logical: uniquePath, trees: []string{"prep"}, wantFound: true, wantProducer: "unique"},
		{name: "two declared trees", logical: uniquePath, trees: []string{"prep", "objects"}},
		{name: "ambiguous canonical writers", logical: ambiguousPath, trees: []string{"prep"}},
		{name: "replaced duplicate ID absent", logical: oldPath, trees: []string{"prep"}},
		{name: "final duplicate ID retained", logical: finalPath, trees: []string{"prep"}, wantFound: true, wantProducer: "repeated-id"},
		{name: "empty tree excluded", logical: "include/generated/empty-tree.h", trees: []string{"prep"}},
		{name: "missing path", logical: "include/generated/missing.h", trees: []string{"prep"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := ActionPlanNode{Trees: slices.Clone(test.trees)}
			recipe := ActionRecipe{Trees: slices.Clone(test.trees)}
			got, gotFound := context.uniqueCanonicalOutput(test.logical, node, recipe)
			want, wantFound := configDependencyReferenceUniqueCanonicalOutput(context, test.logical, node, recipe)
			if gotFound != wantFound || got != want {
				t.Fatalf("indexed canonical output = (%#v, %t), full scan = (%#v, %t)", got, gotFound, want, wantFound)
			}
			if gotFound != test.wantFound || gotFound && got.producerID != test.wantProducer {
				t.Fatalf("canonical output = (%#v, %t), want producer %q found=%t", got, gotFound, test.wantProducer, test.wantFound)
			}
		})
	}
}

func TestConfigDependencyInvocationPredecessorClosureCacheMatchesGraphAndIsIsolated(t *testing.T) {
	const pathname = "include/generated/predecessor.h"
	consumer := compactKbuildSelectionKey{profile: "consumer", target: "drivers/example/driver.o", stage: "target"}
	producer := compactKbuildSelectionKey{profile: "producer", target: pathname, stage: "prep"}
	graph := &compactKbuildSelectionGraph{
		profiles: map[string]CompactKbuildProfile{
			"consumer": {Name: "consumer", InvocationPredecessors: []string{"middle"}},
			"middle":   {Name: "middle", InvocationPredecessors: []string{"producer"}},
			"producer": {Name: "producer"},
		},
		selections: map[compactKbuildSelectionKey]CompactKbuildSelection{
			consumer: {Profile: consumer.profile, Target: consumer.target, Stage: consumer.stage},
			producer: {Profile: producer.profile, Target: producer.target, Stage: producer.stage},
		},
		outputOwnersByPath: map[string][]compactKbuildSelectionKey{pathname: {producer}},
	}
	baselineOwner, baselineSelected, baselineErr := graph.compactKbuildSelectionRecordedPathOwner(consumer, pathname)
	context := newConfigDependencyAnalysisContext(&ActionPlan{selectionGraph: graph})
	resolve := func() (compactKbuildSelectionKey, bool, error) {
		return graph.compactKbuildSelectionRecordedPathOwnerWithPredecessorClosure(
			consumer,
			pathname,
			func(profileName string) []string {
				return context.invocationPredecessorClosure(graph, profileName)
			},
		)
	}
	for range 2 {
		owner, selected, err := resolve()
		if owner != baselineOwner || selected != baselineSelected || fmt.Sprint(err) != fmt.Sprint(baselineErr) {
			t.Fatalf("cached owner = (%#v, %t, %v), baseline = (%#v, %t, %v)",
				owner, selected, err, baselineOwner, baselineSelected, baselineErr)
		}
	}
	if !baselineSelected || baselineOwner != producer || len(context.invocationPredecessorClosures) != 1 {
		t.Fatalf("cached predecessor result = owner %#v selected=%t cache=%#v", baselineOwner, baselineSelected, context.invocationPredecessorClosures)
	}

	other := newConfigDependencyAnalysisContext(&ActionPlan{selectionGraph: graph})
	if len(other.invocationPredecessorClosures) != 0 {
		t.Fatalf("new analysis context inherited predecessor closures: %#v", other.invocationPredecessorClosures)
	}
	first := context.invocationPredecessorClosure(graph, consumer.profile)
	second := other.invocationPredecessorClosure(graph, consumer.profile)
	if !slices.Equal(first, second) || len(other.invocationPredecessorClosures) != 1 {
		t.Fatalf("isolated predecessor closures = %q and %q; other cache %#v", first, second, other.invocationPredecessorClosures)
	}
	first[0] = "mutated-first-context"
	if second[0] == first[0] {
		t.Fatal("invocation predecessor closure storage escaped its analysis context")
	}
}

func BenchmarkConfigDependencyCanonicalGeneratedOutputLookup(b *testing.B) {
	const (
		nodeCount   = 4096
		lookupCount = 256
	)
	plan := &ActionPlan{Nodes: make([]ActionPlanNode, nodeCount)}
	for index := range plan.Nodes {
		plan.Nodes[index] = ActionPlanNode{
			ID: fmt.Sprintf("generated-%05d", index), Stage: "prep",
			Outputs: []ActionPlanOutput{{
				Tree: "prep", Path: fmt.Sprintf("include/generated/benchmark-%05d.h", index),
			}},
		}
	}
	context := newConfigDependencyAnalysisContext(plan)
	node := ActionPlanNode{Trees: []string{"prep"}}
	recipe := ActionRecipe{Trees: []string{"prep"}}
	queries := make([]string, lookupCount)
	for index := range queries {
		queries[index] = fmt.Sprintf("include/generated/benchmark-%05d.h", index*11)
	}
	for _, benchmark := range []struct {
		name   string
		lookup func(string) (configDependencyCanonicalOutput, bool)
	}{
		{
			name: "indexed",
			lookup: func(logical string) (configDependencyCanonicalOutput, bool) {
				return context.uniqueCanonicalOutput(logical, node, recipe)
			},
		},
		{
			name: "full-node-scan-reference",
			lookup: func(logical string) (configDependencyCanonicalOutput, bool) {
				return configDependencyReferenceUniqueCanonicalOutput(context, logical, node, recipe)
			},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(lookupCount, "lookups/op")
			total := 0
			for range b.N {
				for _, query := range queries {
					candidate, found := benchmark.lookup(query)
					if found {
						total += candidate.slot + 1
					}
				}
			}
			if total == 0 {
				b.Fatal("generated-output benchmark found no candidates")
			}
		})
	}
}

func BenchmarkConfigDependencyInvocationPredecessorClosure(b *testing.B) {
	const (
		profileCount = 512
		lookupCount  = 128
	)
	graph := &compactKbuildSelectionGraph{profiles: make(map[string]CompactKbuildProfile, profileCount)}
	for index := range profileCount {
		name := fmt.Sprintf("profile-%04d", index)
		profile := CompactKbuildProfile{Name: name}
		if index != 0 {
			profile.InvocationPredecessors = []string{fmt.Sprintf("profile-%04d", index-1)}
		}
		graph.profiles[name] = profile
	}
	terminal := fmt.Sprintf("profile-%04d", profileCount-1)
	for _, benchmark := range []struct {
		name   string
		cached bool
	}{
		{name: "cached-analysis-context", cached: true},
		{name: "uncached-reference", cached: false},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(lookupCount, "lookups/op")
			total := 0
			for range b.N {
				context := &configDependencyAnalysisContext{invocationPredecessorClosures: map[string][]string{}}
				for range lookupCount {
					var closure []string
					if benchmark.cached {
						closure = context.invocationPredecessorClosure(graph, terminal)
					} else {
						closure = graph.compactKbuildInvocationPredecessorClosure(terminal)
					}
					total += len(closure)
				}
			}
			if total == 0 {
				b.Fatal("invocation-predecessor benchmark found no profiles")
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsUnsafeUnrecordedTreeWriter(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ActionPlan, *ActionPlanNode, compactKbuildSelectionKey, string)
	}{
		{
			name: "ambiguous writers",
			mutate: func(plan *ActionPlan, _ *ActionPlanNode, owner compactKbuildSelectionKey, pathname string) {
				graph := plan.selectionGraph
				producerID := graph.materializedProducers[owner]
				delete(graph.selections, owner)
				delete(graph.materializedProducers, owner)
				delete(graph.selectionsByProfileTarget, compactKbuildProfileTargetKey{
					profile: owner.profile, target: owner.target,
				})
				firstProfile := graph.profiles[owner.profile]
				firstProfile.Name = "producer-a"
				otherProfile := firstProfile
				otherProfile.Name = "producer-b"
				graph.profiles[firstProfile.Name] = firstProfile
				graph.profiles[otherProfile.Name] = otherProfile
				firstOwner := compactKbuildSelectionKey{profile: firstProfile.Name, target: pathname, stage: "prep"}
				otherOwner := compactKbuildSelectionKey{profile: otherProfile.Name, target: pathname, stage: "prep"}
				graph.selections[firstOwner] = CompactKbuildSelection{
					Profile: firstOwner.profile, Target: firstOwner.target, Stage: firstOwner.stage,
				}
				graph.materializedProducers[firstOwner] = producerID
				graph.selections[otherOwner] = CompactKbuildSelection{
					Profile: otherOwner.profile, Target: otherOwner.target, Stage: otherOwner.stage,
				}
				graph.materializedProducers[otherOwner] = "other-tree-header"
				graph.outputOwnersByPath[pathname] = []compactKbuildSelectionKey{firstOwner, otherOwner}
				graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
					profile: firstOwner.profile, target: firstOwner.target,
				}] = firstOwner
				graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
					profile: otherOwner.profile, target: otherOwner.target,
				}] = otherOwner
				plan.Recipes["other-tree-header-recipe"] = ActionRecipe{
					Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
					Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_OTHER"},
					Outputs:   []string{"00000000"},
				}
				plan.Nodes = append(plan.Nodes, ActionPlanNode{
					ID: "other-tree-header", Stage: "prep", Kind: "generate",
					Recipe: "other-tree-header-recipe", Tool: "actionfile",
					Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
				})
			},
		},
		{
			name: "same-stage writer",
			mutate: func(plan *ActionPlan, _ *ActionPlanNode, owner compactKbuildSelectionKey, pathname string) {
				graph := plan.selectionGraph
				producerID := graph.materializedProducers[owner]
				selection := graph.selections[owner]
				delete(graph.selections, owner)
				delete(graph.materializedProducers, owner)
				owner.stage = "target"
				selection.Stage = owner.stage
				graph.selections[owner] = selection
				graph.materializedProducers[owner] = producerID
				graph.outputOwnersByPath[pathname] = []compactKbuildSelectionKey{owner}
				graph.selectionsByProfileTarget[compactKbuildProfileTargetKey{
					profile: owner.profile, target: owner.target,
				}] = owner
				for index := range plan.Nodes {
					if plan.Nodes[index].ID == producerID {
						plan.Nodes[index].Stage = owner.stage
					}
				}
			},
		},
		{
			name: "undeclared tree",
			mutate: func(plan *ActionPlan, node *ActionPlanNode, _ compactKbuildSelectionKey, _ string) {
				recipe := plan.Recipes[node.Recipe]
				recipe.Trees = nil
				plan.Recipes[node.Recipe] = recipe
			},
		},
		{
			name: "configured runtime envelope",
			mutate: func(plan *ActionPlan, _ *ActionPlanNode, _ compactKbuildSelectionKey, _ string) {
				ref := KbuildActionRoleRef{Scope: "target", Role: compactKbuildScriptRuntimeRole}
				plan.metadata.actionContracts[ref] = CompactKbuildActionContract{
					PrefixArguments: []string{"--configured-prefix"},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node, _, owner, pathname := configDependencyUnrecordedTreeGeneratedHeaderPlanForTest(t)
			test.mutate(plan, &node, owner, pathname)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "unavailable generated object-tree input") {
				t.Fatalf("unsafe unrecorded tree writer set = %#v, want opaque fallback", set)
			}
		})
	}
}

func configDependencyStageValidatedMacroHeaderForTest(
	t *testing.T,
	plan *ActionPlan,
	node *ActionPlanNode,
	pathname string,
) {
	t.Helper()
	partRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-line", "opaque execution-time bytes"},
		Outputs:   []string{"00000000"},
	}
	part := ActionPlanNode{
		ID: "macro-header-part", Stage: "prep", Kind: "generate", Recipe: "macro-header-part-recipe", Tool: "actionfile",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: ".macro-header-parts/00000000"}},
	}
	headerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{
			"-input", "${input:part:00000000}",
			"-validate_config_independent_macro_header_v1",
			"-out", "${output:00000000}",
		},
		Inputs: []string{"part:00000000"}, Outputs: []string{"00000000"},
	}
	header := ActionPlanNode{
		ID: "validated-macro-header", Stage: "prep", Kind: "generate", Recipe: "validated-macro-header-recipe", Tool: "actionfile",
		Inputs:  []ActionPlanNodeEdge{{Role: "part", ProducerID: part.ID, Slot: 0}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: pathname}},
	}
	plan.Recipes[part.Recipe] = partRecipe
	plan.Recipes[header.Recipe] = headerRecipe
	plan.Nodes = append(plan.Nodes, part, header)

	binding := "generated:" + planOrdinal(len(node.Inputs))
	node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: "generated", ProducerID: header.ID, Slot: 0})
	compileRecipe := plan.Recipes[node.Recipe]
	compileRecipe.Inputs = append(compileRecipe.Inputs, binding)
	compileRecipe.WorkingDirectory = "compile"
	if compileRecipe.WorkingInputs == nil {
		compileRecipe.WorkingInputs = map[string]string{}
	}
	compileRecipe.WorkingInputs["input:"+binding] = pathname
	plan.Recipes[node.Recipe] = compileRecipe
	plan.Nodes[0] = *node
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesValidatedMacroHeaderSummary(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <offsets.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("validated macro-header dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesStagedValidatedMacroHeaderSummary(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <offsets.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)

	header := plan.Nodes[len(plan.Nodes)-1]
	headerRecipe := plan.Recipes[header.Recipe]
	headerRecipe.Arguments[len(headerRecipe.Arguments)-1] = pathname
	headerRecipe.WorkingDirectory = "validated-header"
	headerRecipe.WorkingOutputs = map[string]string{"00000000": pathname}
	plan.Recipes[header.Recipe] = headerRecipe

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("staged validated macro-header dependency set = %#v", set)
	}

	headerRecipe.WorkingOutputs["00000000"] = "include/generated/different.h"
	plan.Recipes[header.Recipe] = headerRecipe
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "unavailable generated object-tree input") {
		t.Fatalf("mismatched staged macro-header dependency set = %#v, want opaque fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesResolvesValidatedMacroHeaderFromTree(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <offsets.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	// The real kernel exposes preparation headers through ${tree:prep}; they are
	// not direct WorkingInputs of every compiler action.
	node.Inputs = nil
	compileRecipe := plan.Recipes[node.Recipe]
	compileRecipe.Inputs = nil
	compileRecipe.WorkingInputs = nil
	compileRecipe.WorkingDirectory = ""
	compileRecipe.Trees = []string{"prep"}
	node.Trees = []string{"prep"}
	plan.Recipes[node.Recipe] = compileRecipe
	plan.Nodes[0] = node

	consumer := compactKbuildSelectionKey{
		profile: "config-dependency", target: "drivers/example/driver.o", stage: "target",
	}
	owner := compactKbuildSelectionKey{
		profile: "config-dependency", target: pathname, stage: "prep",
	}
	graph := plan.selectionGraph
	graph.selections[owner] = CompactKbuildSelection{Profile: owner.profile, Target: owner.target, Stage: owner.stage}
	graph.materializedProducers[owner] = "validated-macro-header"
	graph.outputOwnersByPath = map[string][]compactKbuildSelectionKey{pathname: {owner}}
	graph.selectionsByProfileTarget = map[compactKbuildProfileTargetKey]compactKbuildSelectionKey{
		{profile: owner.profile, target: owner.target}: owner,
	}
	graph.selectionInitialArtifacts = map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{
		consumer: {{Path: pathname, Profile: owner.profile, Target: owner.target}},
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("tree-visible validated macro-header dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesModelsForcedMacroHeaderState(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
		"include/after.h": `#ifdef GENERATED_OFFSET
#include "defined.h"
#else
#include "undefined.h"
#endif
`,
		"include/defined.h":   "CONFIG_DEFINED_BRANCH\n",
		"include/undefined.h": "CONFIG_UNDEFINED_BRANCH\n",
	}, []string{
		"-nostdinc",
		"-include", "${tree:prep}/include/generated/offsets.h",
		"-include", "${tree:kernel}/include/after.h",
		"-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CONFIG_DEFINED_BRANCH", "CONFIG_DRIVER", "CONFIG_UNDEFINED_BRANCH"}
	if set.Opaque || !slices.Equal(set.Symbols, want) || !slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("forced validated macro-header dependency set = %#v, want symbols %q", set, want)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesKeepsGuardAcrossValidatedAsmOffsets(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#include <asm/nospec-branch.h>
#include <asm/nospec-branch.h>
CONFIG_DRIVER
`,
		"include/asm/nospec-branch.h": `#ifndef _ASM_EXAMPLE_NOSPEC_BRANCH_H_
#define _ASM_EXAMPLE_NOSPEC_BRANCH_H_
#if defined(CONFIG_MITIGATION_CALL_DEPTH_TRACKING) && !defined(COMPILE_OFFSETS)
#include <asm/asm-offsets.h>
#endif
#ifdef GENERATED_OFFSET
#include <linux/generated-defined.h>
#else
#include <linux/generated-undefined.h>
#endif
#endif
`,
		"include/asm/asm-offsets.h":           "#include <generated/asm-offsets.h>\n",
		"include/linux/generated-defined.h":   "CONFIG_GENERATED_DEFINED_BRANCH\n",
		"include/linux/generated-undefined.h": "CONFIG_GENERATED_UNDEFINED_BRANCH\n",
	}, []string{
		"-nostdinc",
		"-I${tree:kernel}/include",
		"-I${tree:prep}/include",
		"-c", "drivers/example/driver.c",
	}, nil)
	pathname := "include/generated/asm-offsets.h"
	configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, pathname)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "#define CONFIG_MITIGATION_CALL_DEPTH_TRACKING 1\n", true, nil
	}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	wantSymbols := []string{
		"CONFIG_DRIVER",
		"CONFIG_GENERATED_DEFINED_BRANCH",
		"CONFIG_GENERATED_UNDEFINED_BRANCH",
		"CONFIG_MITIGATION_CALL_DEPTH_TRACKING",
	}
	wantSources := []string{
		"drivers/example/driver.c",
		"include/asm/asm-offsets.h",
		"include/asm/nospec-branch.h",
		"include/linux/generated-defined.h",
		"include/linux/generated-undefined.h",
	}
	if set.Opaque || !slices.Equal(set.Symbols, wantSymbols) ||
		!slices.Equal(set.SourcePaths, wantSources) ||
		!slices.Equal(set.ObjectPaths, []string{pathname}) {
		t.Fatalf("validated asm-offsets guarded-repeat dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsWildcardRepeatedRecursiveState(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "direct", header: "#include <linux/recursive.h>\n"},
		{
			name: "unknown toggle",
			header: `#ifdef TOGGLE_STATE
#undef TOGGLE_STATE
#else
#define TOGGLE_STATE
#endif
#include <linux/recursive.h>
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c":  "#include <linux/recursive.h>\n",
				"include/linux/recursive.h": test.header,
			}, []string{
				"-nostdinc",
				"-I${tree:kernel}/include",
				"-include", "${tree:prep}/include/generated/offsets.h",
				"-c", "drivers/example/driver.c",
			}, nil)
			configDependencyStageValidatedMacroHeaderForTest(t, plan, &node, "include/generated/offsets.h")
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "", true, nil
			}

			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "recursive include with repeated macro state") {
				t.Fatalf("wildcard recursive dependency set = %#v, want repeated-state fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsDynamicGeneratedText(t *testing.T) {
	toolsetPath, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/toolchain/include/generated.h")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name             string
		recipe           ActionRecipe
		producerContract *CompactKbuildActionContract
	}{
		{
			name: "unknown tool",
			recipe: ActionRecipe{
				Tool: "generator", Arguments: []string{"-out", "${output:00000000}"},
			},
		},
		{
			name: "transformed literal",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_DYNAMIC"},
				ArgumentTransforms: []ActionRecipeArgumentTransform{{Index: 3, Transform: ActionRecipeArgumentTransformContentTemplateBase64}},
			},
		},
		{
			name: "observed state",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-state", "${input:state:00000000}"},
			},
		},
		{
			name: "input copy requires producer-profile provenance",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-input", "${input:source:00000000}"},
			},
		},
		{
			name: "arguments file changes invocation",
			recipe: ActionRecipe{
				Tool: "actionfile", ArgumentsFile: true,
				Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_DYNAMIC"},
			},
		},
		{
			name: "protected literal marker",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{
					"-out", "${output:00000000}", "-line", compactKbuildLiteralTreeEscapeByte + "{tree:prep}",
				},
			},
		},
		{
			name: "configured tool envelope",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_DYNAMIC"},
			},
			producerContract: &CompactKbuildActionContract{PrefixArguments: []string{"--configured-prefix"}},
		},
		{
			name: "runtime without exact configured contract",
			recipe: ActionRecipe{
				Tool: compactKbuildScriptRuntimeRole, Arguments: []string{"echo", "CONFIG_DYNAMIC"}, Stdout: "00000000",
			},
		},
		{
			name: "recipe environment",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-line", "CONFIG_DYNAMIC"},
				Environment: map[string]string{"MODE": "dynamic"},
			},
		},
		{
			name: "runtime-rewritten toolset path",
			recipe: ActionRecipe{
				Tool: "actionfile", Arguments: []string{"-out", "${output:00000000}", "-line", toolsetPath},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <dynamic.h>\n",
			}, []string{
				"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
			}, nil)
			configDependencyStageGeneratedHeaderForTest(
				t, plan, &node, "generated-header", "include/generated/dynamic.h", test.recipe,
			)
			if test.producerContract != nil {
				configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
				producerRef := KbuildActionRoleRef{
					Scope: actionPlanConfigDependencyScope(plan.Nodes[len(plan.Nodes)-1]), Role: test.recipe.Tool,
				}
				plan.metadata.actionRoles = append(plan.metadata.actionRoles, producerRef)
				plan.metadata.actionContracts[producerRef] = *test.producerContract
			}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "unavailable generated object-tree input") {
				t.Fatalf("dynamic generated text set = %#v, want opaque fallback", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesScansAvailableGeneratedObjectInput(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <generated.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-nostdinc", "-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, "include/generated/generated.h", "CONFIG_GENERATED\n")
	profile := plan.selectionGraph.profiles["config-dependency"]
	profile.evaluator.template.sourceRoots["__LINUX_BZL_OBJECT_TREE__"] = objectRoot
	plan.selectionGraph.profiles[profile.Name] = profile
	plan.metadata.preconfiguredObjectTree = true
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "generated-header", Stage: "prep", Kind: "generate", Recipe: "header-recipe", Tool: "actionfile",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/generated.h"}},
	})
	plan.Recipes["header-recipe"] = ActionRecipe{Kind: "generate", Tool: "actionfile"}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER", "CONFIG_GENERATED"}) ||
		!slices.Equal(set.SourcePaths, []string{"drivers/example/driver.c"}) ||
		!slices.Equal(set.ObjectPaths, []string{"include/generated/generated.h"}) {
		t.Fatalf("available generated include dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesFollowsObjectTreeSourceOverlay(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_WRONG_ROOT\n",
	}, []string{
		"-I${tree:prep}/drivers/example", "-c", "drivers/example/driver.c",
	}, nil)
	overlayRoot := t.TempDir()
	mustWriteSource(t, overlayRoot, "driver.c", "#include <selected.h>\n")
	mustWriteSource(t, overlayRoot, "selected.h", "CONFIG_OVERLAY_SELECTED\n")
	profile := plan.selectionGraph.profiles["config-dependency"]
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__/drivers/example"] = overlayRoot
	plan.selectionGraph.profiles[profile.Name] = profile

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_OVERLAY_SELECTED"}) {
		t.Fatalf("source-overlay config dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesDetectsGeneratedQuotedOverlayShadow(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_WRONG_ROOT\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	overlayRoot := t.TempDir()
	mustWriteSource(t, overlayRoot, "driver.c", "#include \"selected.h\"\n")
	mustWriteSource(t, overlayRoot, "selected.h", "CONFIG_OVERLAY_SELECTED\n")
	profile := plan.selectionGraph.profiles["config-dependency"]
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__/drivers/example"] = overlayRoot
	plan.selectionGraph.profiles[profile.Name] = profile
	plan.Nodes = append(plan.Nodes, ActionPlanNode{
		ID: "generated-overlay-shadow", Stage: "target", Kind: "generate", Recipe: "generated-overlay-shadow",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/selected.h"}},
	})
	plan.Recipes["generated-overlay-shadow"] = ActionRecipe{Kind: "generate", Tool: "actionfile"}

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "generated object-tree input drivers/example/selected.h") {
		t.Fatalf("quoted overlay-shadow config dependency set = %#v, want generated-shadow fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesIgnoresMissingLiteralBranch(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": `#if CONFIG_OPTIONAL
#include "not-present-for-this-config.h"
#endif
`,
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_OPTIONAL"}) {
		t.Fatalf("missing conditional include config dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesModelsImplicitCompilerSearch(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		opaque    bool
	}{
		{name: "GCC or Clang implicit standard roots are unmodeled", arguments: []string{"-c", "drivers/example/driver.c"}, opaque: true},
		{name: "nostdinc disables implicit roots", arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <compiler-owned.h>\nCONFIG_DRIVER\n",
			}, test.arguments, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque != test.opaque {
				t.Fatalf("implicit-search config dependency set = %#v, opaque=%t", set, test.opaque)
			}
			if test.opaque && !strings.Contains(set.Reason, "uninspectable compiler search root") {
				t.Fatalf("implicit-search config dependency reason = %q, want unmodeled-root fallback", set.Reason)
			}
			if !test.opaque && !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
				t.Fatalf("nostdinc config dependency set = %#v", set)
			}
		})
	}
}

func TestConfigDependencyMacroDebugArgumentAdmission(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		opaque    bool
	}{
		{name: "empty"},
		{name: "ordinary levels", arguments: []string{"-g", "-g0", "-g1", "-g2", "-ggdb", "-ggdb0", "-ggdb1", "-ggdb2"}},
		{name: "other canonical levels", arguments: []string{"-gvms", "-gvms0", "-gvms1", "-gvms2", "-gcodeview", "-gcodeview0", "-gcodeview1", "-gcodeview2"}},
		{name: "dwarf format is not level", arguments: []string{"-gdwarf-3", "-gdwarf-5"}},
		{name: "disabled", arguments: []string{"-fno-debug-macro"}},
		{name: "macro replacement is not option", arguments: []string{"-DDEBUG_OPTION=-g3"}},
		// A presence-only gate intentionally rejects these even after a real delimiter.
		{name: "after delimiter", arguments: []string{"--", "-g3", "-fdebug-macro"}, opaque: true},
		{name: "MT delimiter-shaped operand", arguments: []string{"-MT", "--", "-g3"}, opaque: true},
		{name: "MQ delimiter-shaped operand", arguments: []string{"-MQ", "--", "-fdebug-macro"}, opaque: true},
		{name: "level three", arguments: []string{"-g3"}, opaque: true},
		{name: "gdb level three", arguments: []string{"-ggdb3"}, opaque: true},
		{name: "leading zero", arguments: []string{"-g03"}, opaque: true},
		{name: "hexadecimal", arguments: []string{"-g0x3"}, opaque: true},
		{name: "gdb hexadecimal", arguments: []string{"-ggdb0x3"}, opaque: true},
		{name: "vms level", arguments: []string{"-gvms3"}, opaque: true},
		{name: "vms leading zero", arguments: []string{"-gvms03"}, opaque: true},
		{name: "codeview level", arguments: []string{"-gcodeview3"}, opaque: true},
		{name: "codeview hexadecimal", arguments: []string{"-gcodeview0x3"}, opaque: true},
		{name: "oversized numeric", arguments: []string{"-g4294967299"}, opaque: true},
		{name: "unmodeled numeric spelling", arguments: []string{"-g00"}, opaque: true},
		{name: "explicit macro debug", arguments: []string{"-g2", "-fdebug-macro"}, opaque: true},
		{name: "later level disable", arguments: []string{"-g3", "-g0"}, opaque: true},
		{name: "later explicit disable", arguments: []string{"-fdebug-macro", "-fno-debug-macro"}, opaque: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := slices.Clone(test.arguments)
			reason := configDependencyMacroDebugArgumentsReason(test.arguments)
			if (reason != "") != test.opaque {
				t.Fatalf("macro-debug admission = %q, want opaque=%t", reason, test.opaque)
			}
			_, coldReason := configDependencyUnsupportedCompilerArgument(test.arguments, len(test.arguments))
			if (coldReason != "") != test.opaque {
				t.Fatalf("cold compiler admission = %q, want opaque=%t", coldReason, test.opaque)
			}
			if !slices.Equal(test.arguments, before) {
				t.Fatal("proof admission changed original compiler arguments")
			}
		})
	}
}

func TestConfigDependencyMacroDebugPreservesOriginalEnvelopeAndFullConfig(t *testing.T) {
	for _, placement := range []string{"recipe", "configured-prefix", "configured-suffix", "compound", "retained-symbolic-twin"} {
		t.Run(placement, func(t *testing.T) {
			const binding = "working-closure:00000000"
			arguments := []string{
				"-nostdinc", "-include", "${input:" + binding + "}",
				"-c", "drivers/example/driver.c",
			}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#if CONFIG_USED\nint selected;\n#endif\n",
			}, arguments, map[string]string{"CONFIG_USED": "y", "CONFIG_OTHER": "y"})
			if got := configDependencyAttachFallbackProjectionInputForTest(
				t, plan, &node, "autoconf.h", configDependencyAutoconfPath,
			); got != binding {
				t.Fatalf("config input binding = %q, want %q", got, binding)
			}
			recipe := plan.Recipes[node.Recipe]
			contract := CompactKbuildActionContract{}
			switch placement {
			case "configured-prefix":
				contract.PrefixArguments = []string{"-g03"}
			case "configured-suffix":
				contract.SuffixArguments = []string{"-ggdb0x3"}
			default:
				recipe.Arguments = append(slices.Clone(arguments), "-fdebug-macro")
			}
			if placement == "compound" || placement == "retained-symbolic-twin" {
				recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
					Tool:                     "cc",
					Arguments:                slices.Clone(recipe.Arguments),
					WorkingInputUses:         []string{"input:" + binding},
					WorkingInputUsesComplete: true,
				}
				recipe.Tool = compactKbuildScriptRunnerRole
				recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
			}
			plan.Recipes[node.Recipe] = recipe
			configDependencySetCompilerContractForTest(plan, node, contract)
			if placement == "retained-symbolic-twin" {
				// A deferred probe twin is not the final compiler argv. In
				// particular, it must not hide an original namespace-observing
				// flag merely because its current symbolic value is unavailable.
				plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
					node.ID: {Tool: "cc", Arguments: []string{
						"-nostdinc", "${result:00000000.text}", "-c", "drivers/example/driver.c",
					}},
				}
			}
			invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
			if reason != "" || configDependencyMacroDebugArgumentsReason(invocation.arguments) == "" {
				t.Fatalf("fixture lost original unsafe compiler argv: reason=%q", reason)
			}
			if placement == "retained-symbolic-twin" &&
				configDependencyMacroDebugArgumentsReason(invocation.predefineArguments) != "" {
				t.Fatal("fixture did not distinguish original argv from retained probe twin")
			}
			originalArguments := slices.Clone(invocation.arguments)
			originalNodeID := node.ContentID()
			originalSources := configDependencySourceTableSnapshotForTest(plan)
			originalInputs := slices.Clone(node.Inputs)
			originalProducerID := plan.Nodes[1].ContentID()
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "macro debug") ||
				len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
				t.Fatalf("macro-debug dependency set published partial precision: %#v", set)
			}
			retained, changed, err := prunePreciseFamilyCompilerInputs(plan, &node, set)
			if err != nil || changed {
				t.Fatalf("opaque input pruning = changed %t, err %v", changed, err)
			}
			if node.ContentID() != originalNodeID || !slices.Equal(node.Inputs, originalInputs) ||
				!slices.Equal(configDependencySourceTableSnapshotForTest(plan), originalSources) ||
				len(plan.Nodes) != 2 || plan.Nodes[1].ContentID() != originalProducerID {
				t.Fatal("macro-debug fallback changed original source or producer closure")
			}
			retainedInvocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, retained)
			if reason != "" || !slices.Equal(retainedInvocation.arguments, originalArguments) {
				t.Fatal("macro-debug fallback changed original compiler envelope")
			}
			full := familyTestConfig("1", "1")
			capsule, err := RenderConfigCapsule(full, set)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(capsule.Files, full) {
				t.Fatal("macro-debug fallback projected the full config capsule")
			}
		})
	}
}

func TestConfigDependencyMacroDebugNeverPublishesCompletedPrecision(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#if CONFIG_USED\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	safe, safeNode := configDependencyCompletedCachePlanForTest(
		t, root, "safe", "compile-safe", "src-00000001", "image", "target-toolset",
		map[string]string{"CONFIG_USED": "y", "CONFIG_OTHER": "n"},
	)
	safeSet := configDependencyBuildWithFamilyCacheForTest(t, safe, cache)[safeNode.ID]
	if safeSet.Opaque || !slices.Equal(safeSet.Symbols, []string{"CONFIG_USED"}) ||
		cache.configDependencies.completed.stores != 1 {
		t.Fatalf("safe baseline did not establish a completed-cache entry: %#v", safeSet)
	}
	for _, debugArguments := range [][]string{{"-g3"}, {"-MT", "--", "-g3"}, {"-MQ", "--", "-fdebug-macro"}} {
		for index, other := range []string{"n", "y", "n"} {
			plan, node := configDependencyCompletedCachePlanForTest(
				t, root, fmt.Sprintf("unsafe-%d", index), fmt.Sprintf("compile-unsafe-%d", index),
				"src-00000001", "image", "target-toolset",
				map[string]string{"CONFIG_USED": "y", "CONFIG_OTHER": other},
			)
			recipe := plan.Recipes[node.Recipe]
			probeArguments := slices.Clone(recipe.Arguments)
			recipe.Arguments = append(slices.Clone(recipe.Arguments), debugArguments...)
			plan.Recipes[node.Recipe] = recipe
			plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
				node.ID: {Tool: "cc", Arguments: probeArguments},
			}
			plan.attachFamilyPlanningCache(cache)
			context := newConfigDependencyAnalysisContextWithCache(plan, cache.configDependencyCache())
			if _, ok := configDependencyCompletedCompilerCandidateForNode(
				plan, node, recipe, context, cache.configDependencyCache(),
			); ok {
				t.Fatal("unsafe original argv remained eligible for completed-cache lookup")
			}
			set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
			if !set.Opaque || !strings.Contains(set.Reason, "macro debug") ||
				len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0 {
				t.Fatalf("shared-cache analysis published macro-debug precision: %#v", set)
			}
			full := familyTestConfig("1", other)
			capsule, err := RenderConfigCapsule(full, set)
			if err != nil || !maps.Equal(capsule.Files, full) {
				t.Fatalf("macro-debug completed-cache fallback projected full config: %v", err)
			}
			if cache.configDependencies.completed.hits != 0 || cache.configDependencies.completed.stores != 1 ||
				cache.configDependencies.completed.entryCount != 1 {
				t.Fatalf("unsafe entry reached completed cache: hits=%d stores=%d entries=%d",
					cache.configDependencies.completed.hits, cache.configDependencies.completed.stores,
					cache.configDependencies.completed.entryCount)
			}
		}
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsOpaqueCompilerForwarding(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
	}{
		{name: "GCC response", arguments: []string{"@compiler.rsp", "-c", "drivers/example/driver.c"}},
		{name: "response after delimiter", arguments: []string{"--", "@compiler.rsp"}},
		{name: "GCC Wp include", arguments: []string{"-Wp,-include,forced.h", "-c", "drivers/example/driver.c"}},
		{name: "GCC Xpreprocessor", arguments: []string{"-Xpreprocessor", "-include", "-c", "drivers/example/driver.c"}},
		{name: "Clang Xclang", arguments: []string{"-Xclang", "-include", "-c", "drivers/example/driver.c"}},
		{name: "Clang Xassembler macro-shaped operand", arguments: []string{"-Xassembler", "-DNOT_A_DRIVER_MACRO", "-c", "drivers/example/driver.c"}},
		{name: "Clang Xlinker language-shaped operand", arguments: []string{"-Xlinker", "-xc++", "-c", "drivers/example/driver.c"}},
		{name: "Clang mllvm undef-shaped operand", arguments: []string{"-mllvm", "-UNOT_A_DRIVER_MACRO", "-c", "drivers/example/driver.c"}},
		{name: "GCC assembler comma forwarding", arguments: []string{"-Wa,-DNOT_A_DRIVER_MACRO", "-c", "drivers/example/driver.c"}},
		{name: "GCC linker comma forwarding", arguments: []string{"-Wl,-xc++", "-c", "drivers/example/driver.c"}},
		{name: "GCC split B prefix", arguments: []string{"-B", "/configured/prefix", "-c", "drivers/example/driver.c"}},
		{name: "GCC joined B prefix", arguments: []string{"-B/configured/prefix", "-c", "drivers/example/driver.c"}},
		{name: "GCC specs", arguments: []string{"-specs=/configured/specs", "-c", "drivers/example/driver.c"}},
		{name: "GCC wrapper", arguments: []string{"-wrapper", "/configured/wrapper", "-c", "drivers/example/driver.c"}},
		{name: "GCC plugin", arguments: []string{"-fplugin=/configured/plugin.so", "-c", "drivers/example/driver.c"}},
		{name: "Clang pass plugin", arguments: []string{"-fpass-plugin=/configured/plugin.so", "-c", "drivers/example/driver.c"}},
		{name: "framework F", arguments: []string{"-F/configured/frameworks", "-c", "drivers/example/driver.c"}},
		{name: "framework include", arguments: []string{"-iframework", "/configured/frameworks", "-c", "drivers/example/driver.c"}},
		{name: "Clang system after", arguments: []string{"-isystem-after", "/configured/include", "-c", "drivers/example/driver.c"}},
		{name: "GCC include with sysroot", arguments: []string{"-iwithsysroot", "/configured/include", "-c", "drivers/example/driver.c"}},
		{name: "GCC multilib include", arguments: []string{"-imultilib", "multilib", "-c", "drivers/example/driver.c"}},
		{name: "trigraph preprocessing", arguments: []string{"-trigraphs", "-c", "drivers/example/driver.c"}},
		{name: "traditional preprocessing", arguments: []string{"-traditional", "-c", "drivers/example/driver.c"}},
		{name: "traditional cpp preprocessing", arguments: []string{"-traditional-cpp", "-c", "drivers/example/driver.c"}},
		{name: "already preprocessed", arguments: []string{"-fpreprocessed", "-c", "drivers/example/driver.c"}},
		{name: "directives-only preprocessing", arguments: []string{"-fdirectives-only", "-c", "drivers/example/driver.c"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
			}, test.arguments, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "compiler argv") {
				t.Fatalf("forwarded compiler config dependency set = %#v, want opaque", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsProbeGrammarRegularFileInputs(t *testing.T) {
	regularOptions := 0
	for _, option := range probeCandidatePathOptions {
		if option.kind != ProbeCandidatePathRegularFile {
			continue
		}
		regularOptions++
		operand := "${tree:kernel}/drivers/example/compiler-input.txt"
		operandKind := "source-tree"
		if regularOptions%2 == 0 {
			operand = "${tree:prep}/include/generated/compiler-input.txt"
			operandKind = "generated-object-tree"
		}
		t.Run(strings.TrimPrefix(option.name, "-")+" "+operandKind, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "CONFIG_DRIVER\n",
			}, []string{option.name + "=" + operand, "-nostdinc", "-c", "drivers/example/driver.c"}, nil)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || !strings.Contains(set.Reason, "regular-file input option "+option.name) {
				t.Fatalf("regular-file compiler input dependency set = %#v, want opaque fallback", set)
			}
		})
	}
	if regularOptions < 2 {
		t.Fatalf("probe grammar has %d regular-file options; test requires source and generated operand coverage", regularOptions)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesAllowsLinuxWpDependencyOutput(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{
		"-Wp,-MMD,drivers/example/.driver.o.d,-MT,drivers/example/driver.o",
		"-Wp,-MT,drivers/example/driver.o", "-nostdinc", "-c", "drivers/example/driver.c",
	}, nil)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
		t.Fatalf("Linux dependency-output config dependency set = %#v", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesConfiguredCompilerContract(t *testing.T) {
	for _, test := range []struct {
		name        string
		prefix      []string
		suffix      []string
		environment map[string]string
		arguments   []string
		want        []string
		opaque      bool
	}{
		{
			name: "GCC or Clang prefix I shadows Kbuild I", prefix: []string{"-I", "/configured/include"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name: "Kbuild I precedes GCC or Clang suffix I", suffix: []string{"-I", "/configured/include"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"},
			want:      []string{"CONFIG_LOCAL_INCLUDE"},
		},
		{
			name: "configured forced include", prefix: []string{"-include", "/configured/forced.h"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name:      "configured Clang resource directory suffix",
			suffix:    []string{"-Xclang", "-internal-isystem", "-Xclang", "/configured/clang-resource/include"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"},
			want:      []string{"CONFIG_LOCAL_INCLUDE"},
		},
		{
			name:      "configured Clang resource directory reached",
			suffix:    []string{"-Xclang", "-internal-isystem", "-Xclang", "/configured/clang-resource/include"},
			arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name:      "configured Clang resource directory prefix remains opaque",
			prefix:    []string{"-Xclang", "-internal-isystem", "-Xclang", "/configured/clang-resource/include"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name:      "configured Clang resource directory response remains opaque",
			suffix:    []string{"-Xclang", "-internal-isystem", "-Xclang", "@resource.rsp"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name: "CPATH after Kbuild I", environment: map[string]string{"CPATH": "/configured/include"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"},
			want:      []string{"CONFIG_LOCAL_INCLUDE"},
		},
		{
			name: "CPATH reached", environment: map[string]string{"CPATH": "/configured/include"},
			arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name: "GCC_EXEC_PREFIX reached despite nostdinc", environment: map[string]string{"GCC_EXEC_PREFIX": "/configured/gcc"},
			arguments: []string{"-nostdinc", "-c", "drivers/example/driver.c"}, opaque: true,
		},
		{
			name: "opaque compiler environment", environment: map[string]string{"COMPILER_PATH": "/configured/compiler"},
			arguments: []string{"-nostdinc", "-I${tree:kernel}/include/local", "-c", "drivers/example/driver.c"}, opaque: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <selected.h>\n",
				"include/local/selected.h": "CONFIG_LOCAL_INCLUDE\n",
			}, test.arguments, nil)
			originalArguments := slices.Clone(plan.Recipes[node.Recipe].Arguments)
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{
				PrefixArguments: test.prefix,
				SuffixArguments: test.suffix,
				Environment:     test.environment,
			})
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque != test.opaque || !test.opaque && !slices.Equal(set.Symbols, test.want) {
				t.Fatalf("configured compiler config dependency set = %#v, want symbols %q opaque=%t", set, test.want, test.opaque)
			}
			if test.name == "configured Clang resource directory reached" &&
				!strings.Contains(set.Reason, "uninspectable compiler search root") {
				t.Fatalf("configured resource-root dependency reason = %q, want uninspectable-root fallback", set.Reason)
			}
			if !slices.Equal(plan.Recipes[node.Recipe].Arguments, originalArguments) {
				t.Fatalf("planner contract analysis mutated serialized recipe argv: %#v", plan.Recipes[node.Recipe].Arguments)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesUsesLanguageIncludeEnvironment(t *testing.T) {
	for _, test := range []struct {
		name        string
		tool        string
		arguments   []string
		environment map[string]string
		opaque      bool
	}{
		{name: "GCC or Clang C include path", tool: "cc", environment: map[string]string{"C_INCLUDE_PATH": "/configured/c"}, opaque: true},
		{name: "C ignores C++ include path", tool: "cc", environment: map[string]string{"CPLUS_INCLUDE_PATH": "/configured/cxx"}},
		{name: "GCC or Clang C++ include path", tool: "cxx", environment: map[string]string{"CPLUS_INCLUDE_PATH": "/configured/cxx"}, opaque: true},
		{name: "C++ ignores C include path", tool: "cxx", environment: map[string]string{"C_INCLUDE_PATH": "/configured/c"}},
		{name: "explicit language makes include environment opaque", tool: "cc", arguments: []string{"-nostdinc", "-x", "c++", "-c", "drivers/example/driver.c"}, environment: map[string]string{"CPLUS_INCLUDE_PATH": "/configured/cxx"}, opaque: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := test.arguments
			if arguments == nil {
				arguments = []string{"-nostdinc", "-c", "drivers/example/driver.c"}
			}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <language-owned.h>\nCONFIG_DRIVER\n",
			}, arguments, nil)
			recipe := plan.Recipes[node.Recipe]
			recipe.Tool = test.tool
			plan.Recipes[node.Recipe] = recipe
			node.Tool = test.tool
			plan.Nodes[0] = node
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{Environment: test.environment})
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque != test.opaque {
				t.Fatalf("language include environment config dependency set = %#v, opaque=%t", set, test.opaque)
			}
			if !test.opaque && !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
				t.Fatalf("ignored language environment config dependency set = %#v", set)
			}
		})
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRejectsUppercaseCIncludeEnvironment(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.C": "#include <language-owned.h>\nCONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.C"}, nil)
	plan.Sources[0].Path = "drivers/example/driver.C"
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{
		Environment: map[string]string{"CPLUS_INCLUDE_PATH": "/configured/cxx"},
	})

	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "language selection") {
		t.Fatalf("uppercase .C include-environment dependency set = %#v, want language fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesRequiresConfiguredCompilerContract(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	plan.metadata.actionRoles = []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "action-contract metadata") {
		t.Fatalf("missing configured contract dependency set = %#v, want opaque", set)
	}
}

func TestNormalizeCompactKbuildActionContractsClonesExactRegistry(t *testing.T) {
	cc := KbuildActionRoleRef{Scope: "target", Role: "cc"}
	cxx := KbuildActionRoleRef{Scope: "host", Role: "cxx"}
	input := map[KbuildActionRoleRef]CompactKbuildActionContract{
		cc: {
			PrefixArguments: []string{"-nostdinc"},
			SuffixArguments: []string{"-I", "/configured/include"},
			Environment:     map[string]string{"CPATH": "/configured/cpath"},
		},
		cxx: {},
	}
	got, err := normalizeCompactKbuildActionContracts([]KbuildActionRoleRef{cc, cxx}, input)
	if err != nil {
		t.Fatal(err)
	}
	input[cc].PrefixArguments[0] = "@changed.rsp"
	input[cc].Environment["CPATH"] = "/changed"
	if !slices.Equal(got[cc].PrefixArguments, []string{"-nostdinc"}) ||
		!maps.Equal(got[cc].Environment, map[string]string{"CPATH": "/configured/cpath"}) || got[cxx].Environment == nil {
		t.Fatalf("normalized configured contracts alias caller data: %#v", got)
	}
	for _, invalid := range []map[KbuildActionRoleRef]CompactKbuildActionContract{
		{cc: {}},
		{cc: {}, {Scope: "target", Role: "unknown"}: {}},
		{cc: {}, cxx: {Environment: map[string]string{"BAD=NAME": "value"}}},
	} {
		if _, err := normalizeCompactKbuildActionContracts([]KbuildActionRoleRef{cc, cxx}, invalid); err == nil {
			t.Fatalf("invalid configured action-contract registry accepted: %#v", invalid)
		}
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesPreservesModversions(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_ONLY\n",
	}, []string{"-c", "drivers/example/driver.c"}, map[string]string{"CONFIG_MODVERSIONS": "y"})
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || !strings.Contains(set.Reason, "MODVERSIONS") {
		t.Fatalf("MODVERSIONS config dependency set = %#v, want conservative fallback", set)
	}
}

func TestAnalyzeActionPlanNodeConfigDependenciesClassifiesKnownAndOpaqueActions(t *testing.T) {
	for _, test := range []struct {
		name   string
		node   ActionPlanNode
		recipe ActionRecipe
		opaque bool
	}{
		{
			name: "archive ignores staged baseline",
			node: ActionPlanNode{ID: "archive", Kind: "archive", Recipe: "recipe", Tool: "ar"},
			recipe: ActionRecipe{Kind: "archive", Tool: "ar", WorkingInputs: map[string]string{
				"source:config": "include/generated/autoconf.h",
			}},
		},
		{
			name:   "script is opaque",
			node:   ActionPlanNode{ID: "script", Kind: "generate", Recipe: "recipe", Tool: compactKbuildScriptRunnerRole},
			recipe: ActionRecipe{Kind: "generate", Tool: compactKbuildScriptRunnerRole},
			opaque: true,
		},
		{
			name:   "explicit auto conf is opaque",
			node:   ActionPlanNode{ID: "generate", Kind: "generate", Recipe: "recipe", Tool: "awk"},
			recipe: ActionRecipe{Kind: "generate", Tool: "awk", Arguments: []string{"include/config/auto.conf"}},
			opaque: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{"recipe": test.recipe}}
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, test.node)
			if err != nil {
				t.Fatal(err)
			}
			if set.Opaque != test.opaque {
				t.Fatalf("config dependency set = %#v, opaque=%t", set, test.opaque)
			}
		})
	}
}

func resolvedConfigProjectionFixture() map[string]string {
	return map[string]string{
		".config": strings.Join([]string{
			"# CONFIG_DISABLED is not set",
			"CONFIG_ENABLED=y",
			"CONFIG_MODULE=m",
			"CONFIG_TEXT=\"CONFIG_UNRELATED text\"",
		}, "\n") + "\n",
		"include/config/auto.conf": strings.Join([]string{
			"CONFIG_ENABLED=y",
			"CONFIG_MODULE=m",
			"CONFIG_TEXT=CONFIG_UNRELATED text",
		}, "\n") + "\n",
		"include/config/auto.conf.cmd": "cmd_include/config/auto.conf := bazel kconfig_parse -resolve_config\n",
		"include/generated/autoconf.h": strings.Join([]string{
			"/* Generated by Bazel kconfig_parse. */",
			"#ifndef __GENERATED_AUTOCONF_H__",
			"#define __GENERATED_AUTOCONF_H__",
			"#define CONFIG_ENABLED 1",
			"#define CONFIG_MODULE_MODULE 1",
			"#define CONFIG_TEXT \"CONFIG_UNRELATED text\"",
			"#endif",
		}, "\n") + "\n",
		"include/generated/rustc_cfg": strings.Join([]string{
			"--cfg=CONFIG_ENABLED",
			"--cfg=CONFIG_ENABLED=\"y\"",
			"--cfg=CONFIG_MODULE",
			"--cfg=CONFIG_MODULE=\"m\"",
			"--cfg=CONFIG_TEXT=\"CONFIG_UNRELATED text\"",
		}, "\n") + "\n",
		"include/config/kernel.release": "6.18-selected\n",
	}
}

func TestRenderConfigCapsuleFiltersAllSixProjections(t *testing.T) {
	full := resolvedConfigProjectionFixture()
	capsule, err := RenderConfigCapsule(full, ConfigDependencySet{
		Symbols: []string{"CONFIG_MODULE_MODULE", "CONFIG_DISABLED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capsule.ID) != 64 || len(capsule.Files) != len(ResolvedConfigProjectionOutputs()) {
		t.Fatalf("capsule identity/files = %q/%#v", capsule.ID, capsule.Files)
	}
	for pathname, wantFragments := range map[string][]string{
		".config":                       {"# CONFIG_DISABLED is not set", "CONFIG_MODULE=m"},
		"include/config/auto.conf":      {"CONFIG_MODULE=m"},
		"include/generated/autoconf.h":  {"__GENERATED_AUTOCONF_H__", "CONFIG_MODULE_MODULE"},
		"include/generated/rustc_cfg":   {"CONFIG_MODULE", `CONFIG_MODULE="m"`},
		"include/config/auto.conf.cmd":  {"kconfig_parse -resolve_config"},
		"include/config/kernel.release": {},
	} {
		contents := capsule.Files[pathname]
		for _, fragment := range wantFragments {
			if !strings.Contains(contents, fragment) {
				t.Errorf("capsule %s = %q, want fragment %q", pathname, contents, fragment)
			}
		}
		for _, rejected := range []string{"CONFIG_ENABLED", "CONFIG_TEXT", "CONFIG_UNRELATED", "6.18-selected"} {
			if strings.Contains(contents, rejected) {
				t.Errorf("capsule %s retained unrelated %q in %q", pathname, rejected, contents)
			}
		}
	}
	if got := capsule.Files["include/config/kernel.release"]; got != "\n" {
		t.Fatalf("filtered kernel.release = %q, want empty projection", got)
	}
}

func TestRenderConfigCapsuleIdentityIgnoresUnselectedValues(t *testing.T) {
	first := resolvedConfigProjectionFixture()
	second := maps.Clone(first)
	second[".config"] = strings.ReplaceAll(second[".config"], "CONFIG_ENABLED=y", "# CONFIG_ENABLED is not set")
	second["include/config/auto.conf"] = strings.ReplaceAll(second["include/config/auto.conf"], "CONFIG_ENABLED=y\n", "")
	second["include/generated/autoconf.h"] = strings.ReplaceAll(second["include/generated/autoconf.h"], "#define CONFIG_ENABLED 1\n", "")
	second["include/generated/rustc_cfg"] = strings.ReplaceAll(second["include/generated/rustc_cfg"], "--cfg=CONFIG_ENABLED\n--cfg=CONFIG_ENABLED=\"y\"\n", "")
	dependencies := ConfigDependencySet{Symbols: []string{"CONFIG_MODULE"}}
	a, err := RenderConfigCapsule(first, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderConfigCapsule(second, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || !maps.Equal(a.Files, b.Files) {
		t.Fatalf("unselected config changed capsule: %s/%s\n%#v\n%#v", a.ID, b.ID, a.Files, b.Files)
	}

	opaque, err := RenderConfigCapsule(first, ConfigDependencySet{Opaque: true, Reason: "test fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(opaque.Files, first) {
		t.Fatalf("opaque capsule = %#v, want exact full projections", opaque.Files)
	}
}

func TestActionPlanConfigDependencyAnalysisRekeysNodeIDs(t *testing.T) {
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{"recipe": {Kind: "archive", Tool: "ar"}},
		Nodes: []ActionPlanNode{{
			ID: "provisional", Stage: "target", Kind: "archive", Recipe: "recipe", Tool: "ar",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: "built-in.a"}},
		}},
	}
	analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if err != nil {
		t.Fatal(err)
	}
	if byID, err := analysis.ByNodeID(plan); err != nil || byID["provisional"].Opaque {
		t.Fatalf("provisional annotations = %#v, %v", byID, err)
	}
	plan.Nodes[0].ID = "final"
	if byID, err := analysis.ByNodeID(plan); err != nil || byID["final"].Opaque {
		t.Fatalf("final annotations = %#v, %v", byID, err)
	}
	plan.Nodes[0].Outputs[0].Path = filepath.ToSlash("changed/output")
	if _, err := analysis.ByNodeID(plan); err == nil {
		t.Fatal("annotation accepted a structurally changed node")
	}
}

func TestActionPlanConfigDependencyAnalysisRekeysPersistentProducerIDs(t *testing.T) {
	producer := ActionPlanNode{
		ID: "producer-provisional", Stage: "prep", Kind: "generate",
		Recipe: "producer-recipe", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/persistent.h"}},
	}
	consumer := ActionPlanNode{
		ID: "consumer-provisional", Stage: "target", Kind: "archive",
		Recipe: "consumer-recipe", Tool: "ar", Product: "image",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "built-in.a"}},
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{
			producer.Recipe: {Kind: "generate", Tool: "actionfile"},
			consumer.Recipe: {Kind: "archive", Tool: "ar"},
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	consumer.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
		Target: ActionPlanInputSetTarget{
			Kind: ActionPlanInputSetWorkTarget, Path: "include/generated/persistent.h",
		},
		ProducerID: producer.ID,
	})
	plan.Nodes[1] = consumer
	analysis, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if err != nil {
		t.Fatal(err)
	}

	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	producer.ID = "producer-final"
	consumer.ID = "consumer-final"
	consumer.InputSet, err = store.Map(consumer.InputSet, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		entry.ProducerID = producer.ID
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes = []ActionPlanNode{producer, consumer}
	plan.invalidateLookupIndexes()
	byID, err := analysis.ByNodeID(plan)
	if err != nil || len(byID) != 2 || byID[consumer.ID].Opaque {
		t.Fatalf("rekeyed persistent annotations = %#v, %v", byID, err)
	}

	consumer.InputSet, err = store.Map(consumer.InputSet, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		entry.Target.Path = "include/generated/changed.h"
		return entry, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan.Nodes[1] = consumer
	if _, err := analysis.ByNodeID(plan); err == nil {
		t.Fatal("annotation accepted a structurally changed persistent input set")
	}
}

func TestConfigDependencyWitnessSharesPersistentSubtries(t *testing.T) {
	plan := contentAddressCumulativeRootsTestPlan(t, 256)
	builder, err := newConfigDependencyWitnessBuilder(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		witness, err := builder.witness(node)
		if err != nil {
			t.Fatal(err)
		}
		if node.InputSet != "" && len(witness.inputSet) != 64 {
			t.Fatalf("input-set witness retains %d bytes, want one digest", len(witness.inputSet))
		}
	}
	if len(builder.roots) != len(plan.InputSets) {
		t.Fatalf("witness cache has %d records for %d unique trie nodes", len(builder.roots), len(plan.InputSets))
	}
	if len(builder.active) != 0 {
		t.Fatal("completed witness traversal retains active nodes")
	}
}

func TestConfigDependencyWitnessRetainsPersistentEntrySemantics(t *testing.T) {
	for _, field := range []string{"producer name", "source identity", "provenance kind", "slot", "target path", "target kind", "tree", "compiler use", "auxiliary use"} {
		t.Run(field, func(t *testing.T) {
			plan := &ActionPlan{}
			entry := ActionPlanInputSetEntry{
				Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetTreeTarget, Tree: "prep", Path: "generated.h"},
				ProducerID: "producer-before", CompilerUse: true,
			}
			if field == "source identity" {
				entry.ProducerID, entry.SourceID = "", "src-00000001"
			}
			node := ActionPlanNode{InputSet: configDependencyInsertInputSetEntryForTest(t, plan, "", entry)}
			builder, err := newConfigDependencyWitnessBuilder(plan)
			if err != nil {
				t.Fatal(err)
			}
			before, err := builder.witness(node)
			if err != nil {
				t.Fatal(err)
			}
			node.InputSet, err = builder.store.Map(node.InputSet, func(value ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
				switch field {
				case "producer name":
					value.ProducerID = "producer-after"
				case "source identity":
					value.SourceID = "src-00000002"
				case "provenance kind":
					value.ProducerID, value.SourceID = "", "src-00000001"
				case "slot":
					value.Slot++
				case "target path":
					value.Target.Path = "another.h"
				case "target kind":
					value.Target.Kind, value.Target.Tree = ActionPlanInputSetWorkTarget, ""
				case "tree":
					value.Target.Tree = "objects"
				case "compiler use":
					value.CompilerUse = false
				case "auxiliary use":
					value.AuxiliaryUse = true
				}
				return value, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after, err := builder.witness(node)
			if err != nil {
				t.Fatal(err)
			}
			if equal, wantEqual := before == after, field == "producer name"; equal != wantEqual {
				t.Fatalf("witness equality after changing %s = %t, want %t", field, equal, wantEqual)
			}
		})
	}
}
