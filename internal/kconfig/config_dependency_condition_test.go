package kconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func configDependencyConditionalStateForTest(t *testing.T) *configDependencyMacroState {
	t.Helper()
	state, reason := parseConfigDependencyCompilerPredefines("#define A 0\n#define B 1\n#define FOO 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	return &state
}

func TestConfigDependencyConditionalDefinitionTruthAndPrecedence(t *testing.T) {
	const (
		f = configDependencyMacroUndefined
		u = configDependencyMacroUnknown
		v = configDependencyMacroDefined
	)
	definitions := []configDependencyMacroDefinition{f, u, v}
	and := [][]configDependencyMacroDefinition{{f, f, f}, {f, u, u}, {f, u, v}}
	or := [][]configDependencyMacroDefinition{{f, u, v}, {u, u, v}, {v, v, v}}
	not := []configDependencyMacroDefinition{v, u, f}
	for leftIndex, left := range definitions {
		for rightIndex, right := range definitions {
			state := configDependencyConditionalStateForTest(t)
			state.set("LEFT", left)
			state.set("RIGHT", right)
			for _, test := range []struct {
				expression string
				want       configDependencyMacroDefinition
			}{
				{"defined(LEFT) && defined(RIGHT)", and[leftIndex][rightIndex]},
				{"defined(LEFT) || defined(RIGHT)", or[leftIndex][rightIndex]},
				{"!defined(LEFT)", not[leftIndex]},
			} {
				if got := configDependencyConditionalDefinition(test.expression, state); got != test.want {
					t.Errorf("%s with (%d,%d) = %d, want %d", test.expression, left, right, got, test.want)
				}
			}
		}
	}
	state := configDependencyConditionalStateForTest(t)
	for _, test := range []struct {
		expression string
		want       configDependencyMacroDefinition
	}{
		{"0", f},
		{"1", v},
		{"!0", v},
		{"!!1", v},
		{"defined A", v},
		{"defined ( A )", v},
		{"defined(MISSING)", f},
		{"defined(_UNDUMPED_RESERVED)", u},
		{"1 || 0 && 0", v},
		{"(1 || 0) && 0", f},
		{"!defined(A) || defined(B) && 0", f},
		{"!(!defined A || defined MISSING)", v},
		{"defined(_UNDUMPED_RESERVED) && 0", f},
		{"defined(_UNDUMPED_RESERVED) || 1", v},
		{" \t(defined(A)\v&&\f!defined(MISSING))\r\n", v},
	} {
		if got := configDependencyConditionalDefinition(test.expression, state); got != test.want {
			t.Errorf("%q = %d, want %d", test.expression, got, test.want)
		}
	}
}

func assertConfigDependencyConditionalRejectsWithoutReads(t *testing.T, expression string) {
	t.Helper()
	root := configDependencyConditionalStateForTest(t)
	state := root.branch()
	if state == nil || !state.beginForcedHeaderTrace() {
		t.Fatal("cannot start conditional read trace")
	}
	defer state.endForcedHeaderTrace()
	state.recordForcedHeaderRead("EXISTING_READ")
	beforeSnapshot := state.snapshot
	beforeMaterialized := state.materializedSnapshotCache
	beforeRevision := state.snapshotRevision
	beforeFork := state.snapshotForkRevision
	if got := configDependencyConditionalDefinition(expression, state); got != configDependencyMacroUnknown {
		t.Fatalf("unsupported expression %q = %d, want unknown", expression, got)
	}
	trace := state.forcedHeaderTrace
	if trace.reads.size() != 1 || !trace.reads.contains("EXISTING_READ") ||
		trace.invalid || trace.wholeNamespace || trace.nonConfigWildcard ||
		state.forcedHeaderTouches.size() != 0 || !state.snapshotChanges.empty() ||
		state.snapshot != beforeSnapshot || state.materializedSnapshotCache != beforeMaterialized ||
		state.snapshotRevision != beforeRevision || state.snapshotForkRevision != beforeFork ||
		state.tainted || state.consumed {
		t.Fatalf("rejecting %q read or mutated macro state", expression)
	}
}

func TestConfigDependencyConditionalDefinitionRejectsWholeUnsupportedStream(t *testing.T) {
	for index, expression := range []string{
		"", " ", "A", "CONFIG_X", "0 && M", "1 || M",
		"defined(A) && M", "defined(A) || __has_builtin(x)", "IS_ENABLED(CONFIG_X)",
		"definedFOO", "defined_foo", "defined0",
		"defined", "defined()", "defined(0)", "defined((A))",
		"defined(A B)", "defined(A)defined(B)", "defined(A) &&",
		"(defined(A)", "defined(A))", "defined(A) & defined(B)",
		"defined(A) | defined(B)", "defined(A) &&& defined(B)",
		"defined(A) ||| defined(B)", "defined(A) != 0",
		"defined(A) + 1", "defined(A) ? 1 : 0",
		"0x0", "01", "10", "1U", "1.0", "1e0", "'1'", "\"1\"",
		"defined/**/(A)", "defined(A)\x00", "defined(A)\\", "defined($A)",
		"defined(\u00e9)", "\u00a01",
	} {
		t.Run(fmt.Sprintf("%02d", index), func(t *testing.T) {
			assertConfigDependencyConditionalRejectsWithoutReads(t, expression)
		})
	}
}

func TestConfigDependencyConditionalDefinitionBounds(t *testing.T) {
	state := configDependencyConditionalStateForTest(t)
	for _, expression := range []string{
		strings.Repeat(" ", configDependencyConditionalMaximumBytes-1) + "1",
		strings.Repeat("(", configDependencyConditionalMaximumDepth) + "1" +
			strings.Repeat(")", configDependencyConditionalMaximumDepth),
		strings.Repeat("(", configDependencyConditionalMaximumDepth-1) + "defined(A)" +
			strings.Repeat(")", configDependencyConditionalMaximumDepth-1),
		strings.Repeat("1&&", 1000) + "1",
		strings.Repeat("0||", 1000) + "1",
	} {
		if got := configDependencyConditionalDefinition(expression, state); got != configDependencyMacroDefined {
			t.Errorf("valid bounded expression of %d bytes = %d, want defined", len(expression), got)
		}
	}
	// Negation is iterative, so it does not spend the parenthesis depth budget.
	negations := strings.Repeat("!", configDependencyConditionalMaximumBytes-1) + "1"
	if got := configDependencyConditionalDefinition(negations, state); got != configDependencyMacroUndefined {
		t.Fatalf("long odd negation chain = %d, want undefined", got)
	}
	for _, expression := range []string{
		strings.Repeat(" ", configDependencyConditionalMaximumBytes) + "1",
		strings.Repeat("!", configDependencyConditionalMaximumBytes) + "1",
		strings.Repeat("(", configDependencyConditionalMaximumDepth+1) + "defined(A)" +
			strings.Repeat(")", configDependencyConditionalMaximumDepth+1),
		strings.Repeat("(", configDependencyConditionalMaximumDepth) + "defined(A)" +
			strings.Repeat(")", configDependencyConditionalMaximumDepth),
	} {
		assertConfigDependencyConditionalRejectsWithoutReads(t, expression)
	}
}

func TestConfigDependencyConditionalDefinitionRecordsAllValidatedOperands(t *testing.T) {
	root := configDependencyConditionalStateForTest(t)
	state := root.branch()
	if !state.beginForcedHeaderTrace() {
		t.Fatal("cannot start conditional read trace")
	}
	defer state.endForcedHeaderTrace()
	beforeSnapshot, beforeRevision := state.snapshot, state.snapshotRevision
	// The first operand determines the result, but all defined operands still
	// pass through the existing read recorder; repeated names remain deduplicated.
	if got := configDependencyConditionalDefinition("1 || (defined(A) && defined(B)) || defined(A)", state); got != configDependencyMacroDefined {
		t.Fatalf("validated expression = %d, want defined", got)
	}
	if state.forcedHeaderTrace.reads.size() != 2 ||
		!state.forcedHeaderTrace.reads.contains("A") || !state.forcedHeaderTrace.reads.contains("B") ||
		state.forcedHeaderTouches.size() != 0 || state.snapshot != beforeSnapshot ||
		state.snapshotRevision != beforeRevision || state.tainted {
		t.Fatal("validated expression lost reads or mutated semantic state")
	}
}

func TestConfigDependencyConditionalDefinitionPreprocessingAndElif(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{
			name:   "comments and operator splice",
			source: "#if defined/**/(A) &\\\n& !defined(MISSING)\n#include \"chosen.h\"\n#else\n#include \"other.h\"\n#endif\n",
			want:   []string{"chosen.h"},
		},
		{
			name:   "elif precedence",
			source: "#if 0\n#include \"never.h\"\n#elif defined(A) && (!defined(MISSING) || defined(B))\n#include \"chosen.h\"\n#else\n#include \"other.h\"\n#endif\n",
			want:   []string{"chosen.h"},
		},
		{
			name:   "split keyword stays unsupported",
			source: "#if def/**/ined(A)\n#include \"chosen.h\"\n#else\n#include \"other.h\"\n#endif\n",
			want:   []string{"chosen.h", "other.h"},
		},
		{
			name:   "spliced raw identifier stays unsupported",
			source: "#if defined\\\nFOO\n#include \"chosen.h\"\n#else\n#include \"other.h\"\n#endif\n",
			want:   []string{"chosen.h", "other.h"},
		},
		{
			name:   "elif invalid tail stays unknown",
			source: "#if 0\n#include \"never.h\"\n#elif defined(A) && M\n#include \"chosen.h\"\n#else\n#include \"other.h\"\n#endif\n",
			want:   []string{"chosen.h", "other.h"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := configDependencyConditionalStateForTest(t)
			includes, reason := configDependencyLiteralIncludesWithMacroEffects(
				[]byte(test.source), state,
				func(configDependencyLiteralInclude, configDependencyMacroDefinition) (bool, string) {
					return true, ""
				},
			)
			names := make([]string, len(includes))
			for index, include := range includes {
				names[index] = include.name
			}
			if reason != "" || !slices.Equal(names, test.want) {
				t.Fatalf("includes = %q, reason=%q, want %q", names, reason, test.want)
			}
		})
	}
}

func TestConfigDependencyConditionalDefinitionKeepsExpansionInjectionPossible(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{
			name:   "raw replacement injects disjunction",
			source: "#define M 1 || CONFIG_X\n#if 0 && M\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
		{
			name:   "defined-prefixed identifier is not operator",
			source: "#define definedFOO 1\n#if definedFOO\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
				"drivers/example/driver.c": test.source,
				"include/chosen.h":         "CONFIG_CHOSEN\n",
				"include/other.h":          "CONFIG_OTHER\n",
			})
			result := fixture.compare(t, &configDependencyHeaderCache{}, "drivers/example/driver.c",
				func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
					state.set("CONFIG_X", configDependencyMacroDefined)
				},
			)
			// M expands to 1 || CONFIG_X: the real #if may be true despite its
			// literal-zero prefix. Neither possible closure may be discarded.
			if result.set.Opaque || !slices.Contains(result.set.SourcePaths, "include/chosen.h") ||
				!slices.Contains(result.set.SourcePaths, "include/other.h") {
				t.Fatalf("unsupported macro expansion lost a possible closure: %#v", result.set)
			}
		})
	}
}

func TestConfigDependencyConditionalDefinitionCompoundReadsInvalidateHeaderCache(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#if defined(LEFT) && defined(RIGHT)\n#include \"selected.h\"\n#else\n#include \"fallback.h\"\n#endif\n#define HEADER_COMPLETE\n",
		"include/selected.h":       "CONFIG_SELECTED\n",
		"include/fallback.h":       "CONFIG_FALLBACK\n",
	})
	cache := &configDependencyHeaderCache{}
	for index, test := range []struct {
		left, right configDependencyMacroDefinition
		want        string
		hit         bool
	}{
		{configDependencyMacroDefined, configDependencyMacroDefined, "CONFIG_SELECTED", false},
		{configDependencyMacroDefined, configDependencyMacroDefined, "CONFIG_SELECTED", true},
		{configDependencyMacroUndefined, configDependencyMacroDefined, "CONFIG_FALLBACK", false},
		{configDependencyMacroDefined, configDependencyMacroUndefined, "CONFIG_FALLBACK", false},
		{configDependencyMacroDefined, configDependencyMacroDefined, "CONFIG_SELECTED", true},
	} {
		beforeHits, beforeMisses := cache.hits, cache.misses
		result := fixture.compare(t, cache, "drivers/example/driver.c",
			func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
				state.set("LEFT", test.left)
				state.set("RIGHT", test.right)
				state.set(fmt.Sprintf("UNREAD_%d", index), configDependencyMacroDefined)
			},
		)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{test.want}) {
			t.Fatalf("entry %d result = %#v, want %s", index, result.set, test.want)
		}
		if test.hit {
			if cache.hits != beforeHits+1 || cache.misses != beforeMisses {
				t.Fatalf("entry %d did not reuse matching read cells", index)
			}
		} else if cache.hits != beforeHits || cache.misses != beforeMisses+1 {
			t.Fatalf("entry %d reused changed read cells", index)
		}
	}
}

func TestConfigDependencyConditionalDefinitionRetainsRecordedBitRequirements(t *testing.T) {
	root := configDependencyConditionalStateForTest(t)
	entry := captureForcedHeaderMacroEntryForTest(t, root, func(state *configDependencyMacroState) {
		expression := "defined(" + configDependencyResolvedAutoconfGuard + ") || 1"
		if got := configDependencyConditionalDefinition(expression, state); got != configDependencyMacroDefined {
			t.Fatalf("guard expression = %d, want defined", got)
		}
		state.set("PREFIX_DONE", configDependencyMacroDefined)
	})
	matching := configDependencyConditionalStateForTest(t)
	if _, valid := entry.restore(&configDependencyClosureScanner{}, matching); !valid {
		t.Fatal("matching compound-read cells did not restore")
	}
	cell, valid := matching.snapshotCell(configDependencyResolvedAutoconfGuard)
	if !valid {
		t.Fatal("cannot read synthetic guard")
	}
	cell.recorded = !cell.recorded
	if !matching.putSnapshotCell(configDependencyResolvedAutoconfGuard, cell) {
		t.Fatal("cannot change the guard recorded bit")
	}
	if _, valid := entry.restore(&configDependencyClosureScanner{}, matching); valid {
		t.Fatal("recorded-bit-only requirement change reused a forced-prefix result")
	}
}

func TestConfigDependencyPreprocessorTextMultilineBlockComments(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{
			name:   "block comment joins physical lines with one space",
			source: "#if 0 /* first\nsecond */ || 1\n",
			want:   "#if 0   || 1\n",
		},
		{
			name:   "comment still separates preprocessing tokens",
			source: "def/* first\nsecond */ined(A)\n",
			want:   "def ined(A)\n",
		},
		{
			name:   "comment cannot manufacture a directive line",
			source: "ordinary /* first\nsecond */ #include <not-a-directive.h>\n",
			want:   "ordinary   #include <not-a-directive.h>\n",
		},
		{
			name:   "macro replacement remains on its directive",
			source: "#define VALUE /* first\nsecond */ 7\n",
			want:   "#define VALUE   7\n",
		},
		{
			name:   "CRLF inside comment is removed and outside preserved",
			source: "left/* first\r\nsecond */right\r\n",
			want:   "left right\r\n",
		},
		{
			name:   "line comment retains terminating newline",
			source: "// first\n#if 1\n",
			want:   " \n#if 1\n",
		},
		{
			name:   "phase two splice precedes line comment removal",
			source: "// first\\\ncontinued\n#if 1\n",
			want:   " \n#if 1\n",
		},
		{
			name:   "phase two splice can complete block comment delimiter",
			source: "left/* first *\\\n/right\n",
			want:   "left right\n",
		},
		{
			name:   "string literal comment spelling remains untouched",
			source: "\"/*\\n*/\"\n",
			want:   "\"/*\\n*/\"\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := configDependencyPreprocessorText([]byte(test.source))
			if reason != "" || got != test.want {
				t.Fatalf("preprocessor text = %q, reason=%q, want %q", got, reason, test.want)
			}
		})
	}
	if _, reason := configDependencyPreprocessorText([]byte("/* unterminated\ncomment")); !strings.Contains(reason, "unterminated block comment") {
		t.Fatalf("unterminated block comment reason = %q", reason)
	}
}

func TestConfigDependencyConditionalDefinitionMultilineCommentScannerCache(t *testing.T) {
	// GCC 15.2.0 and Clang 21.1.8 native preprocessor oracles both select the
	// true branch when a block comment crosses physical lines in a directive.
	// In particular, keeping the newline in the first two cases would fold a
	// false prefix and incorrectly omit the selected immutable header.
	for _, test := range []struct {
		name, source string
	}{
		{
			name:   "compound condition continues after comment",
			source: "#define A 1\n#define C 1\n#if defined(A) && defined(B) /* first\nsecond */ || defined(C)\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
		{
			name:   "literal condition continues after comment",
			source: "#if 0 /* first\nsecond */ || 1\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
		{
			name:   "elif condition continues after comment",
			source: "#if 0\n#include <other.h>\n#elif 0 /* first\nsecond */ || 1\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
		{
			name:   "defined operand continues after comment",
			source: "#define A 1\n#if defined /* first\nsecond */ (A)\n#include <chosen.h>\n#else\n#include <other.h>\n#endif\n",
		},
		{
			name:   "include operand continues after comment",
			source: "#include /* first\nsecond */ <chosen.h>\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
				"drivers/example/driver.c": test.source,
				"include/chosen.h":         "CONFIG_CHOSEN\n",
				"include/other.h":          "CONFIG_OTHER\n",
			})
			cache := &configDependencyHeaderCache{}
			for iteration := 0; iteration < 2; iteration++ {
				beforeHits := cache.hits
				result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
				if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_CHOSEN"}) ||
					!slices.Equal(result.set.SourcePaths, []string{"drivers/example/driver.c", "include/chosen.h"}) {
					t.Fatalf("iteration %d selected the wrong conditional closure: %#v", iteration, result.set)
				}
				if iteration > 0 && cache.hits != beforeHits+1 {
					t.Fatal("repeat interpretation did not exercise the warm selected-header cache")
				}
			}
		})
	}
}
