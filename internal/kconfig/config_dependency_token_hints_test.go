package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestConfigDependencyOpenedHeaderHintRetainsCompleteFilesWithinLimits(t *testing.T) {
	for _, limit := range []string{"work", "names", "bytes", "producer", "invalid-work"} {
		t.Run(limit, func(t *testing.T) {
			scanner := configDependencyClosureScanner{
				collectCompilerGuards: true, callCoverage: &configDependencyCallCoverage{},
				compilerIntrinsicInitialSnapshot: newConfigDependencyMacroSnapshot(),
				compilerGuardFiles:               map[string]configDependencyScanFile{},
				conditionalSyntax:                map[string]configDependencyConditionalSyntax{},
			}
			first := configDependencyOpenedHeaderHintInventory{
				hints: configDependencyCompilerGuardHints{names: []string{"__PREFIX"}, optionalTokenHints: true}, work: 1,
			}
			second := configDependencyOpenedHeaderHintInventory{
				hints: configDependencyCompilerGuardHints{names: []string{"__SECOND"}, optionalTokenHints: true}, work: 1,
			}
			switch limit {
			case "work":
				second.work = 65536
			case "names":
				second.hints.names = nil
				for index := range configDependencyCompilerGuardHintsMaximumNames {
					second.hints.names = append(second.hints.names, "__NAME_"+strconv.Itoa(index))
				}
				second.work = len(second.hints.names)
			case "bytes":
				second.hints.names = nil
				for index := range 2048 {
					second.hints.names = append(second.hints.names, "__"+strings.Repeat("x", 140)+strconv.Itoa(index))
				}
				second.work = len(second.hints.names)
			case "producer":
				second.hints.truncated = true
			case "invalid-work":
				second.work = -1
			}
			root := t.TempDir()
			for index, inventory := range []configDependencyOpenedHeaderHintInventory{first, second} {
				file := configDependencyScanFile{logical: "include/" + strconv.Itoa(index) + ".h", physical: root + "/absent-" + strconv.Itoa(index), source: true}
				scanner.compilerGuardFiles[file.identity()] = file
				scanner.conditionalSyntax[scanner.conditionalSyntaxCacheKey(file)] = configDependencyConditionalSyntax{
					compilerTokenInventoryReady: true, compilerGuardContentID: "cached-content-id",
					compilerTokenInventory: inventory,
				}
			}
			// Both files are cached; no path exists to accidentally reread them.
			for range 2 {
				calls := 0
				scanner.emitCompilerOpenedHeaderTokenHints(configDependencyCompilerPredefineProbe{language: "c"},
					func(_ configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
						calls++
						if hints.truncated || !hints.optionalTokenHints || hints.optionalDefinedness ||
							!slices.Equal(hints.names, []string{"__PREFIX"}) || file.logical != "include/0.h" || contentID != "cached-content-id" {
							t.Fatalf("%s admitted an incomplete file or lost origin: %#v %#v %q", limit, file, hints, contentID)
						}
					})
				if calls != 1 {
					t.Fatalf("%s lost the complete bounded file: observations=%d, want 1", limit, calls)
				}
			}
		})
	}
}

func TestConfigDependencyOpenedHeaderHintSelectionIsDeterministic(t *testing.T) {
	for _, equalWork := range []bool{false, true} {
		scanner := configDependencyClosureScanner{
			collectCompilerGuards: true, callCoverage: &configDependencyCallCoverage{},
			compilerIntrinsicInitialSnapshot: newConfigDependencyMacroSnapshot(),
			compilerGuardFiles:               map[string]configDependencyScanFile{},
			conditionalSyntax:                map[string]configDependencyConditionalSyntax{},
		}
		var identities []string
		files := map[string]configDependencyScanFile{}
		root := t.TempDir()
		for index := range 3 {
			file := configDependencyScanFile{logical: "include/" + strconv.Itoa(index) + ".h", physical: root + "/absent-" + strconv.Itoa(index), source: true}
			identities = append(identities, file.identity())
			files[file.identity()] = file
		}
		slices.Sort(identities)
		// The most expensive file sorts first by identity, but must be selected
		// last by cost. Equal-cost files instead use identity to break ties.
		for index, identity := range identities {
			work := 32768
			if !equalWork {
				work = 1
				if index == 0 {
					work = 65536
				}
			}
			scanner.conditionalSyntax[scanner.conditionalSyntaxCacheKey(files[identity])] = configDependencyConditionalSyntax{
				compilerTokenInventoryReady: true, compilerGuardContentID: identity,
				compilerTokenInventory: configDependencyOpenedHeaderHintInventory{
					hints: configDependencyCompilerGuardHints{names: []string{"__NAME_" + strconv.Itoa(index)}, optionalTokenHints: true}, work: work,
				},
			}
		}
		want := []string{"__NAME_1", "__NAME_2"}
		if equalWork {
			want = []string{"__NAME_0", "__NAME_1"}
		}
		for range 4 {
			clear(scanner.compilerGuardFiles)
			for _, identity := range identities {
				scanner.compilerGuardFiles[identity] = files[identity]
			}
			var got []string
			scanner.emitCompilerOpenedHeaderTokenHints(configDependencyCompilerPredefineProbe{language: "c"},
				func(_ configDependencyCompilerPredefineProbe, file configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
					if hints.truncated || !hints.optionalTokenHints || contentID != file.identity() {
						t.Fatal("selection lost authenticated complete-file origin")
					}
					got = append(got, hints.names...)
					hints.names[0] = "__MUTATED"
				})
			if !slices.Equal(got, want) {
				t.Fatalf("equal work=%t: selection = %v, want %v", equalWork, got, want)
			}
			slices.Reverse(identities)
		}
	}
}

func TestConfigDependencyOpenedHeaderHintOverflowIsConsumerLocal(t *testing.T) {
	retained := map[string]bool{}
	actualDemands := map[string]bool{}
	baselineSeen := map[string]bool{}
	truncations := 0
	observe := func(value ConfigDependencyCompilerGuardObservation) error {
		if value.OptionalTokenHints {
			if value.Truncated {
				// Mirrors the unchanged coordinator's global weak-stage rollback.
				clear(retained)
				truncations++
			} else {
				for _, name := range value.Names {
					retained[name] = true
				}
			}
		} else if value.OptionalDefinedness {
			for _, name := range value.Names {
				actualDemands[name] = true
			}
		} else if slices.Contains(value.Names, "__BASELINE") {
			baselineSeen[value.Origin.LogicalPath] = true
		}
		return nil
	}
	for _, consumer := range []struct {
		name, token string
		overflow    bool
	}{
		{"before", "__GOOD_BEFORE", false},
		{"overflow", "__OVERFLOW_FIRST", true},
		{"after", "__GOOD_AFTER", false},
	} {
		// The fixture helper binds its node to this path. Each call owns a
		// separate source root and analysis context, with one shared observer.
		const pathname = "drivers/example/driver.c"
		clear(baselineSeen)
		source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n" + consumer.token + "\n"
		if consumer.overflow {
			// A later span exceeds the inventory lexer's token budget, but the
			// actual ordered proof still stops at the preceding unknown token.
			source += "#if 1\n" + strings.Repeat("ordinary; ", 5000) + "\n#endif\n"
		}
		source += "#if defined(__BASELINE)\n#endif\n"
		plan, node := compilerGuardObservationPlanForTest(t, map[string]string{pathname: source}, []string{"-nostdinc", "-c", pathname})
		baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil {
			t.Fatal(err)
		}
		plan.metadata.SetCompilerGuardObserver(observe)
		got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
		if err != nil || !reflect.DeepEqual(got, baseline) || !got.Opaque || !strings.Contains(got.Reason, consumer.token) {
			t.Fatalf("%s changed the original first-failure proof: %#v != %#v, %v", consumer.name, got, baseline, err)
		}
		if !actualDemands[consumer.token] || !baselineSeen[pathname] {
			t.Fatalf("%s lost an actual demand or baseline query", consumer.name)
		}
		if consumer.overflow && (!retained["__GOOD_BEFORE"] || retained[consumer.token] || truncations != 0) {
			t.Fatal("local overflow escaped a prefix or discarded the earlier consumer")
		}
	}
	if truncations != 0 || !slices.Equal(slices.Sorted(maps.Keys(retained)), []string{"__GOOD_AFTER", "__GOOD_BEFORE"}) {
		t.Fatalf("consumer transaction retained wrong weak hints: %v; truncations=%d", retained, truncations)
	}
}

func configDependencyOpenedHeaderTokenHints(contents []byte) configDependencyCompilerGuardHints {
	return configDependencyOpenedHeaderTokenHintInventoryForContents(contents).hints
}

func TestConfigDependencyOpenedHeaderHintsIsolateOversizedFile(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	const large = "include/linux/large.h"
	const small = "include/linux/small.h"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname: "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#include <linux/large.h>\n#include <linux/small.h>\n",
		large:    "/*" + strings.Repeat("x", configDependencyCompilerGuardHintsMaximumBytes) + "*/\n",
		small:    "__NEEDED __LATER\n",
	}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", pathname})
	baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !baseline.Opaque || !strings.Contains(baseline.Reason, "__NEEDED") {
		t.Fatalf("fixture did not reach the intended unknown: %#v %v", baseline, err)
	}
	seen := map[string]bool{}
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		if value.OptionalTokenHints {
			if value.Truncated || value.OptionalDefinedness || len(value.Calls) != 0 ||
				!value.Origin.Source || value.Origin.ContentID == "" || value.Origin.LogicalPath != small {
				t.Fatalf("hint accepted the oversized file or lost origin: %#v", value)
			}
			for _, name := range value.Names {
				seen[name] = true
			}
		}
		return nil
	})
	got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !reflect.DeepEqual(got, baseline) {
		t.Fatalf("hint selection changed the unmeasured proof: %#v != %#v, %v", got, baseline, err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(seen)), []string{"__LATER", "__NEEDED"}) {
		t.Fatalf("oversized unrelated file suppressed complete small-file hints: %v", seen)
	}
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__NEEDED"}, false))
	got, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !got.Opaque || !strings.Contains(got.Reason, "__LATER") {
		t.Fatalf("partial compiler answer supplied an unmeasured fact: %#v %v", got, err)
	}
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__NEEDED", "__LATER"}, false))
	got, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || got.Opaque || !slices.Equal(got.Symbols, []string{"CONFIG_PREFIX"}) ||
		!slices.Contains(got.SourcePaths, large) || !slices.Contains(got.SourcePaths, small) {
		t.Fatalf("measured replay lost actual source dependencies: %#v %v", got, err)
	}
}

func configDependencyOpenedHeaderTokenHintInventoryForContents(contents []byte) configDependencyOpenedHeaderHintInventory {
	if len(contents) > configDependencyCompilerGuardHintsMaximumBytes {
		return configDependencyOpenedHeaderHintInventory{hints: configDependencyCompilerGuardHints{truncated: true, optionalTokenHints: true}}
	}
	program := compileConfigDependencyConditionalProgramWithCalls(contents, "test-opened-file", true)
	return configDependencyOpenedHeaderTokenHintInventoryForProgram(len(contents), program)
}

func TestConfigDependencyOpenedHeaderHintsKeepFirstFailure(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	for _, test := range []struct {
		name, text string
		want       []string
	}{
		{"later in span", "__FIRST __LATER\n", []string{"__FIRST", "__LATER"}},
		{"unknown raw arguments", "__FIRST(__RAW) __LATER\n", []string{"__FIRST", "__LATER", "__RAW"}},
		{"used argument", "#define ID(x) x\nID(__FIRST __LATER)\n", []string{"__FIRST", "__LATER"}},
		{"used variadic", "#define V(x, ...) __VA_ARGS__\nV(0, __FIRST __LATER)\n", []string{"__FIRST", "__LATER"}},
		{"unused variadic", "#define V(x, ...) 0\nV(0, __VA_ARGS__)\n", []string{"__VA_ARGS__"}},
		{"pasted replacement", "#define P(a,b) a ## b __LATER\nP(__FI,RST)\n", []string{"__FI", "__LATER"}},
		{"unused replacement", "#define UNUSED __HIDDEN\n0\n", []string{"__HIDDEN"}},
		{"formal", "#define F(__formal) __formal\nF(7)\n", nil},
		{"inactive", "#if 0\n__HIDDEN\n#endif\n__FIRST __LATER\n", []string{"__FIRST", "__HIDDEN", "__LATER"}},
		{"unopened later file", "__FIRST\n#include <not-opened.h>\n", []string{"__FIRST"}},
		{"later directive boundary", "__FIRST\n#if 1\n__LATER\n#endif\n", []string{"__FIRST", "__LATER"}},
		{"literal and comment", "/* __HIDDEN */ \"__STRING\"\n", nil},
		{"source writes", "#define __DEFINED 7\n#undef __UNDEFINED\n__DEFINED __UNDEFINED __FIRST\n", []string{"__DEFINED", "__FIRST", "__UNDEFINED"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n" + test.text
			plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
				pathname: source, "include/not-opened.h": "__UNOPENED\n",
			}, []string{"-nostdinc", "-c", pathname})
			baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				if value.OptionalTokenHints {
					if value.OptionalDefinedness || value.Truncated || len(value.Calls) != 0 || value.Origin.LogicalPath != pathname || !value.Origin.Source || value.Origin.ContentID == "" {
						t.Fatalf("hint lost its separate tier or immutable origin: %#v", value)
					}
					for _, name := range value.Names {
						seen[name] = true
					}
				}
				return nil
			})
			got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil || !reflect.DeepEqual(got, baseline) {
				t.Fatalf("observer changed current proof: %#v != %#v, %v", got, baseline, err)
			}
			if names := slices.Sorted(maps.Keys(seen)); !slices.Equal(names, test.want) {
				t.Fatalf("opened-header hints = %v, want %v", names, test.want)
			}
		})
	}
}

func TestConfigDependencyLiteralIncludeHintLookaheadNeedsMeasuredReplay(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname:              "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n#include <first.h>\n#include <later.h>\n",
		"include/first.h":     "__FIRST_HEADER\n",
		"include/later.h":     "__NEXT_HEADER\n",
		"include/unrelated.h": "__UNRELATED_HEADER\n",
	}, []string{"-nostdinc", "-I${tree:kernel}/include", "-c", pathname})
	baseline, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !baseline.Opaque || !strings.Contains(baseline.Reason, "__FIRST_HEADER") {
		t.Fatalf("fixture did not stop in the first header: %#v, %v", baseline, err)
	}
	seenLater := false
	plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
		if slices.Contains(value.Names, "__UNRELATED_HEADER") {
			t.Error("literal lookahead scanned an unrelated source header")
		}
		if slices.Contains(value.Names, "__NEXT_HEADER") {
			if !value.OptionalTokenHints || !value.LiteralIncludeHints || value.OptionalDefinedness || value.Truncated || len(value.Calls) != 0 ||
				!value.Origin.Source || value.Origin.LogicalPath != "include/later.h" || value.Origin.ContentID == "" {
				t.Errorf("later candidate acquired demand authority or lost its immutable origin: %#v", value)
			}
			seenLater = true
		}
		return nil
	})
	got, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !reflect.DeepEqual(got, baseline) {
		t.Fatalf("candidate lookahead changed the current source proof: %#v != %#v, %v", got, baseline, err)
	}
	if !seenLater {
		t.Error("later literal include supplied no optional name hint after the earlier header stopped replay")
	}
	plan.metadata.SetCompilerGuardObserver(nil)
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__FIRST_HEADER"}, false))
	got, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !got.Opaque || !strings.Contains(got.Reason, "__NEXT_HEADER") || len(got.Symbols) != 0 {
		t.Fatalf("lookahead supplied an unmeasured later binding: %#v, %v", got, err)
	}
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__FIRST_HEADER", "__NEXT_HEADER"}, false))
	got, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || got.Opaque || !slices.Equal(got.Symbols, []string{"CONFIG_PREFIX"}) ||
		!slices.Contains(got.SourcePaths, "include/later.h") {
		t.Fatalf("fresh measured source replay did not prove the later include: %#v, %v", got, err)
	}
}

func TestConfigDependencyOpenedHeaderHintsNeedMeasuredReplay(t *testing.T) {
	const pathname = "drivers/example/driver.c"
	plan, node := compilerGuardObservationPlanForTest(t, map[string]string{
		pathname: "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n__FIRST __LATER\n",
	}, []string{"-nostdinc", "-c", pathname})
	plan.metadata.SetCompilerGuardObserver(func(ConfigDependencyCompilerGuardObservation) error { return nil })
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || !strings.Contains(set.Reason, "__FIRST") || len(set.Symbols) != 0 {
		t.Fatalf("hint became evidence: %#v %v", set, err)
	}
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__FIRST"}, false))
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || !set.Opaque || !strings.Contains(set.Reason, "__LATER") || len(set.Symbols) != 0 {
		t.Fatalf("partial answer supplied missing fact: %#v %v", set, err)
	}
	plan.metadata.SetCompilerGuardAnswers(configDependencyGuardAnswersForTest(t, plan, node, []string{"__FIRST", "__LATER"}, false))
	set, err = AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_PREFIX"}) {
		t.Fatalf("fresh measured replay failed: %#v %v", set, err)
	}
}

func TestConfigDependencyOpenedHeaderTokenHint(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       []string
	}{
		{"later directive", "__FIRST\n#if 1\n__LATER\n#endif\n", []string{"__FIRST", "__LATER"}},
		{"inactive ordinary", "#if 0\n__INACTIVE\n#endif\n__FIRST\n", []string{"__FIRST", "__INACTIVE"}},
		{"nested tokens", "__ATTRIBUTE__((__INNER__))\n", []string{"__ATTRIBUTE__", "__INNER__"}},
		{"replacement not a demand", "#define UNUSED __REPLACEMENT\n0\n", []string{"__REPLACEMENT"}},
		{"formal binding", "#define F(__formal) __formal __BODY\nF(7)\n", []string{"__BODY"}},
		{"variadic binding", "#define V(x, ...) __VA_ARGS__ __BODY\nV(0, 7)\n", []string{"__BODY"}},
		{"unsupported definition", "#define V(x, ...) __VA_OPT__(__NOT_INVENTORIED)\n__REAL\n", []string{"__REAL"}},
		{"strings comments", "/* __COMMENT */ \"__STRING\"\n", nil},
		{"include is not traversal", "#include <__OTHER.h>\n__HERE\n", []string{"__HERE"}},
		{"conditional tier remains separate", "#if defined(__COND)\n#endif\n", nil},
		{"source writes are not initial facts", "#define __LOCAL 1\n#undef __LOCAL\n__LOCAL\n", []string{"__LOCAL"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := configDependencyOpenedHeaderTokenHints([]byte(test.text))
			if got.truncated || !reflect.DeepEqual(got.names, test.want) || len(got.calls) != 0 || got.optionalDefinedness {
				t.Fatalf("weak candidate inventory = %#v, want names %v", got, test.want)
			}
			if len(got.names) != 0 && !got.optionalTokenHints {
				t.Fatalf("inventory escaped weak tier: %#v", got)
			}
		})
	}
}

func TestConfigDependencyOpenedHeaderTokenHintOversize(t *testing.T) {
	contents := []byte("__PREFIX\n" + strings.Repeat(" ", configDependencyCompilerGuardHintsMaximumBytes))
	got := configDependencyOpenedHeaderTokenHints(contents)
	if !got.truncated || !got.optionalTokenHints || got.optionalDefinedness || len(got.names) != 0 || len(got.calls) != 0 {
		t.Fatalf("oversized weak inventory retained a prefix: %#v", got)
	}
}

func TestConfigDependencyOpenedHeaderHintAggregateWork(t *testing.T) {
	budget := configDependencyOpenedHeaderHintBudget{}
	inventory := configDependencyOpenedHeaderHintInventory{
		hints: configDependencyCompilerGuardHints{names: []string{"__ONE", "__ONE"}, optionalTokenHints: true},
		work:  32768,
	}
	if names, truncated := budget.add(inventory); truncated || !reflect.DeepEqual(names, []string{"__ONE"}) {
		t.Fatalf("first complete inventory = %v, truncated %v", names, truncated)
	}
	if names, truncated := budget.add(inventory); truncated || len(names) != 0 || budget.work != 65536 {
		t.Fatalf("cache/repeated inventory evaded accounting: %v, truncated %v, work %d", names, truncated, budget.work)
	}
	inventory.work = 1
	if names, truncated := budget.add(inventory); !truncated || len(names) != 0 || !budget.disabled || budget.names != nil {
		t.Fatalf("aggregate overflow retained state: %v, truncated %v, budget %#v", names, truncated, budget)
	}
	if names, truncated := budget.add(inventory); truncated || len(names) != 0 {
		t.Fatalf("disabled ledger emitted another observation: %v, truncated %v", names, truncated)
	}
}

func TestConfigDependencyOpenedHeaderHintAtomicNames(t *testing.T) {
	for _, limit := range []string{"count", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			budget := configDependencyOpenedHeaderHintBudget{}
			if _, truncated := budget.add(configDependencyOpenedHeaderHintInventory{
				hints: configDependencyCompilerGuardHints{names: []string{"__OLD"}}, work: 1,
			}); truncated {
				t.Fatal("initial inventory truncated")
			}
			names := []string{"__PREFIX"}
			if limit == "bytes" {
				names = append(names, "__"+strings.Repeat("x", configDependencyCompilerGuardHintsMaximumNameBytes))
			} else {
				for index := 0; index < configDependencyCompilerGuardHintsMaximumNames; index++ {
					names = append(names, "__NAME_"+strconv.Itoa(index))
				}
			}
			got, truncated := budget.add(configDependencyOpenedHeaderHintInventory{
				hints: configDependencyCompilerGuardHints{names: names}, work: len(names),
			})
			if !truncated || len(got) != 0 || !budget.disabled || budget.names != nil {
				t.Fatalf("overflow retained a partial inventory: %v, truncated %v, budget %#v", got, truncated, budget)
			}
		})
	}
}

func TestConfigDependencyOpenedHeaderHintWorkProducer(t *testing.T) {
	inventory := configDependencyOpenedHeaderTokenHintInventoryForContents([]byte("#define F(__arg) __arg __BODY\n__ONE (__TWO)\n"))
	if inventory.hints.truncated || inventory.work < len(inventory.hints.names) || inventory.work == 0 {
		t.Fatalf("producer omitted inventory work: %#v", inventory)
	}
	budget := configDependencyOpenedHeaderHintBudget{}
	if names, truncated := budget.add(inventory); truncated || !reflect.DeepEqual(names, []string{"__BODY", "__ONE", "__TWO"}) {
		t.Fatalf("producer/ledger mismatch: %v, truncated %v", names, truncated)
	}
	invalid := inventory
	invalid.work = 0
	if names, truncated := new(configDependencyOpenedHeaderHintBudget).add(invalid); !truncated || len(names) != 0 {
		t.Fatalf("invalid cached work granted names: %v, truncated %v", names, truncated)
	}
}

func TestConfigDependencyOpenedHeaderHintCachedEmission(t *testing.T) {
	snapshot := newConfigDependencyMacroSnapshot()
	for _, definition := range []struct {
		name  string
		value configDependencyMacroDefinition
	}{{"__DEFINED", configDependencyMacroDefined}, {"__UNDEFINED", configDependencyMacroUndefined}} {
		var valid bool
		snapshot, valid = snapshot.withDefinition(definition.name, definition.value)
		if !valid {
			t.Fatal("invalid test snapshot")
		}
	}
	// A nonexistent physical path makes accidental file I/O observable. The
	// observer consumes only syntax cached when the scanner opened the file.
	file := configDependencyScanFile{logical: "include/opened.h", physical: t.TempDir() + "/absent.h", source: true}
	scanner := configDependencyClosureScanner{
		collectCompilerGuards: true, callCoverage: &configDependencyCallCoverage{},
		compilerIntrinsicInitialSnapshot: snapshot,
		compilerGuardInitialDefinitions:  map[string]bool{"__MEASURED": false},
		compilerGuardFiles:               map[string]configDependencyScanFile{file.identity(): file},
		conditionalSyntax:                map[string]configDependencyConditionalSyntax{},
	}
	names := []string{"__DEFINED", "__MEASURED", "__UNDEFINED", "__UNKNOWN"}
	key := scanner.conditionalSyntaxCacheKey(file)
	scanner.conditionalSyntax[key] = configDependencyConditionalSyntax{
		compilerTokenInventoryReady: true, compilerGuardContentID: "cached-content-id",
		compilerTokenInventory: configDependencyOpenedHeaderHintInventory{
			hints: configDependencyCompilerGuardHints{names: slices.Clone(names), optionalTokenHints: true}, work: 4,
		},
	}
	for range 2 {
		observations := 0
		scanner.emitCompilerOpenedHeaderTokenHints(configDependencyCompilerPredefineProbe{language: "c"},
			func(_ configDependencyCompilerPredefineProbe, origin configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
				observations++
				if origin != file || contentID != "cached-content-id" || hints.truncated || !hints.optionalTokenHints || !slices.Equal(hints.names, []string{"__UNKNOWN"}) {
					t.Fatalf("cached emission lost initial facts or origin: %#v %#v %q", origin, hints, contentID)
				}
				hints.names[0] = "__CALLBACK_MUTATION"
			})
		if observations != 1 || !slices.Equal(scanner.conditionalSyntax[key].compilerTokenInventory.hints.names, names) || scanner.compilerIntrinsicInitialSnapshot != snapshot {
			t.Fatal("cached emission was lost or mutated borrowed state")
		}
		requireConfigDependencyMacroSnapshotCell(t, snapshot, "__UNKNOWN", configDependencyMacroSnapshotTestCell(configDependencyMacroUnknown, false))
	}
}

func TestConfigDependencyOpenedHeaderHintCachedOverflow(t *testing.T) {
	scanner := configDependencyClosureScanner{
		collectCompilerGuards: true, callCoverage: &configDependencyCallCoverage{},
		compilerIntrinsicInitialSnapshot: newConfigDependencyMacroSnapshot(),
		compilerGuardFiles:               map[string]configDependencyScanFile{},
		conditionalSyntax:                map[string]configDependencyConditionalSyntax{},
	}
	for index := range 4 {
		file := configDependencyScanFile{logical: "include/" + strconv.Itoa(index) + ".h", exactIdentity: "authenticated-owner", exactContents: "__SAME"}
		scanner.compilerGuardFiles[file.identity()] = file
		scanner.conditionalSyntax[scanner.conditionalSyntaxCacheKey(file)] = configDependencyConditionalSyntax{
			compilerTokenInventoryReady: true, compilerGuardContentID: "cached-content-id",
			compilerTokenInventory: configDependencyOpenedHeaderHintInventory{
				hints: configDependencyCompilerGuardHints{names: []string{"__SAME"}, optionalTokenHints: true}, work: 32768,
			},
		}
	}
	normal, truncated := 0, 0
	scanner.emitCompilerOpenedHeaderTokenHints(configDependencyCompilerPredefineProbe{language: "c"},
		func(_ configDependencyCompilerPredefineProbe, origin configDependencyScanFile, hints configDependencyCompilerGuardHints, contentID string) {
			if !hints.optionalTokenHints || hints.optionalDefinedness || len(hints.calls) != 0 {
				t.Fatal("overflow escaped weak tier")
			}
			if hints.truncated {
				truncated++
				if origin != (configDependencyScanFile{}) || contentID != "" || len(hints.names) != 0 {
					t.Fatal("overflow published a partial inventory or false origin")
				}
			} else {
				normal++
			}
		})
	if normal != 1 || truncated != 0 {
		t.Fatalf("overflow lost complete files or invalidated other consumers: normal=%d truncated=%d", normal, truncated)
	}
}

func TestConfigDependencyOpenedHeaderHintsBindGeneratedOwner(t *testing.T) {
	for _, mode := range []string{"direct", "work", "tree"} {
		t.Run(mode, func(t *testing.T) {
			plan, consumer, producer := observedDependencyPlanForTest(t, mode, "#include <selected.h>\n")
			contents := "#define PICK(x) CONFIG_ ## x\nPICK(PREFIX)\n__FIRST\n#if 1\n__LATER\n#endif\n"
			replay, observed := observedDependencyGateForTest(t, plan, producer, contents)
			var got []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(value ConfigDependencyCompilerGuardObservation) error {
				if value.OptionalTokenHints {
					got = append(got, cloneCompilerGuardObservationForTest(value))
				}
				return nil
			})
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || !sets[consumer].Opaque || len(got) != 1 {
				t.Fatalf("generated inventory lost weak observation: %#v %#v %v", sets, got, err)
			}
			want := ConfigDependencyCompilerGuardOrigin{
				LogicalPath: "include/generated/selected.h", ContentID: observed.Headers()[0].ContentID,
				OriginalProducerNodeID: replay.originalIDs[producer], Slot: 0,
				Tree: "prep", OutputPath: "include/generated/selected.h",
			}
			if got[0].Origin != want || got[0].ConsumerNodeID != consumer || got[0].Truncated || !slices.Equal(got[0].Names, []string{"__FIRST", "__LATER"}) {
				t.Fatalf("weak hint lost authenticated owner: %#v, want %#v", got, want)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(uses) != 0 {
				t.Fatalf("hint granted actual-read authority: %#v %v", uses, err)
			}
		})
	}
}
