package kconfig

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// These fixtures exercise the real interpreter with only the ordinary-header
// cache switched off for the reference run. Source bytes remain immutable;
// invalidations change a scanner's bindings or preprocessing context instead.
type configDependencyHeaderCacheFixtureForTest struct {
	profile  CompactKbuildProfile
	root     *configDependencyMacroState
	physical *configDependencyPhysicalFileCache
	syntax   map[string]configDependencyConditionalSyntax
	parsed   map[string]configDependencyParsedFile
	autoconf configDependencyResolvedAutoconfDefinitions
}

func newConfigDependencyHeaderCacheFixtureForTest(t *testing.T, files map[string]string) *configDependencyHeaderCacheFixtureForTest {
	t.Helper()
	plan, _ := configDependencyCompilePlanForTest(t, files, []string{
		"-nostdinc", "-I${tree:kernel}/include", "-c", "drivers/example/driver.c",
	}, nil)
	root, reason := parseConfigDependencyCompilerPredefines("#define CACHE_PREDEFINED 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	return &configDependencyHeaderCacheFixtureForTest{
		profile: plan.selectionGraph.profiles["config-dependency"], root: &root,
		physical: newConfigDependencyPhysicalFileCache(),
		syntax:   map[string]configDependencyConditionalSyntax{}, parsed: map[string]configDependencyParsedFile{},
	}
}

func (f *configDependencyHeaderCacheFixtureForTest) scanner(cache *configDependencyHeaderCache) *configDependencyClosureScanner {
	return &configDependencyClosureScanner{
		profile: f.profile, language: "c", headerCache: cache,
		physicalFiles: f.physical, conditionalSyntax: f.syntax, parsed: f.parsed,
		generated: map[string]bool{}, generatedText: map[string]configDependencyGeneratedText{},
		preconfigured:      map[string]string{},
		includeDirectories: []configDependencyIncludeDirectory{{logical: "include", source: true}},
		symbols:            map[string]bool{}, sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
		queued: map[string]bool{}, queuedPhysical: map[string]bool{},
	}
}

type configDependencyHeaderCacheObservationForTest struct {
	set                       ConfigDependencySet
	cells                     map[string]configDependencyMacroSnapshotCell
	valid                     bool
	pending                   []configDependencyScanFile
	conditionalFiles          map[string]configDependencyScanFile
	conditionalParsed         map[string]configDependencyParsedFile
	conditionalPending        map[string]bool
	conditionalRepeatGuards   map[string]string
	repeatedConditionalGuards map[string]bool
	queued                    map[string]bool
	queuedPhysical            map[string]bool
}

func configDependencyHeaderCacheCellsForTest(state *configDependencyMacroState) (map[string]configDependencyMacroSnapshotCell, bool) {
	snapshot, valid := state.materializedSnapshot()
	if !valid {
		return nil, false
	}
	cells := map[string]configDependencyMacroSnapshotCell{}
	for index, namespace := range []configDependencyMacroSnapshotNamespace{snapshot.config, snapshot.ordinary, snapshot.reserved} {
		prefix := fmt.Sprintf("%d:", index)
		cells[prefix] = namespace.fallback
		configDependencyMacroSnapshotWalkTree(namespace.facts, func(name string, cell configDependencyMacroSnapshotCell) {
			if cell != namespace.fallback {
				cells[prefix+name] = cell
			}
		})
	}
	return cells, true
}

func (f *configDependencyHeaderCacheFixtureForTest) run(
	t *testing.T,
	cache *configDependencyHeaderCache,
	unit string,
	configure func(*configDependencyClosureScanner, *configDependencyMacroState),
) configDependencyHeaderCacheObservationForTest {
	t.Helper()
	scanner := f.scanner(cache)
	state := f.root.branch()
	if configure != nil {
		configure(scanner, state)
	}
	file, ok := scanner.sourceFile(unit)
	if !ok {
		t.Fatalf("translation unit %q is missing", unit)
	}
	scanner.interpretConditionalCompilerFiles([]configDependencyScanFile{file}, state, f.autoconf)
	observation := configDependencyHeaderCacheObservationForTest{
		pending:          slices.Clone(scanner.pending),
		conditionalFiles: maps.Clone(scanner.conditionalFiles), conditionalParsed: cloneConfigDependencyParsedFiles(scanner.conditionalParsed),
		conditionalPending:      maps.Clone(scanner.conditionalPending),
		conditionalRepeatGuards: maps.Clone(scanner.conditionalRepeatGuards), repeatedConditionalGuards: maps.Clone(scanner.repeatedConditionalGuards),
		queued: maps.Clone(scanner.queued), queuedPhysical: maps.Clone(scanner.queuedPhysical),
	}
	observation.cells, observation.valid = configDependencyHeaderCacheCellsForTest(state)
	observation.set = scanner.scan()
	return observation
}

func (f *configDependencyHeaderCacheFixtureForTest) compare(
	t *testing.T,
	cache *configDependencyHeaderCache,
	unit string,
	configure func(*configDependencyClosureScanner, *configDependencyMacroState),
) configDependencyHeaderCacheObservationForTest {
	t.Helper()
	cached := f.run(t, cache, unit, configure)
	fresh := f.run(t, nil, unit, configure)
	if !reflect.DeepEqual(cached, fresh) {
		t.Fatalf("cached observation differs from cache-disabled interpreter:\ncached: %#v\nfresh:  %#v", cached, fresh)
	}
	return cached
}

func TestConfigDependencyHeaderCacheReplaysNestedClosureAndLaterMacroEffects(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n#ifdef CACHE_SELECTED\n#include <after.h>\n#endif\nCONFIG_UNIT\n",
		"include/common.h":         "#ifndef CACHE_COMMON_H\n#define CACHE_COMMON_H\n#include \"nested.h\"\n#define CACHE_SELECTED 1\n#endif\n",
		"include/nested.h":         "CONFIG_NESTED\n", "include/after.h": "CONFIG_AFTER\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 3; iteration++ {
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_AFTER", "CONFIG_NESTED", "CONFIG_UNIT"}) {
			t.Fatalf("iteration %d result = %#v", iteration, result.set)
		}
		if iteration > 0 && cache.hits == before {
			t.Fatalf("iteration %d did not exercise a warm header-cache hit", iteration)
		}
	}
}

func TestConfigDependencyHeaderCacheRetainsSkippedConfigGuard(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <guarded.h>\nCONFIG_UNIT\n",
		"include/guarded.h":        "#ifndef CONFIG_GATE\n#define CONFIG_GATE 1\nCONFIG_BODY\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	configure := func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
		state.set("CONFIG_GATE", configDependencyMacroDefined)
	}
	for iteration := 0; iteration < 3; iteration++ {
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/driver.c", configure)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_GATE", "CONFIG_UNIT"}) {
			t.Fatalf("iteration %d omitted the skipped guard: %#v", iteration, result.set)
		}
		if iteration > 0 && cache.hits == before {
			t.Fatalf("iteration %d did not exercise cached guard replay", iteration)
		}
	}
	before := cache.hits
	entered := fixture.compare(t, cache, "drivers/example/driver.c", nil)
	if entered.set.Opaque || !slices.Equal(entered.set.Symbols, []string{"CONFIG_BODY", "CONFIG_GATE", "CONFIG_UNIT"}) || cache.hits != before {
		t.Fatalf("changed guard reused skipped body: %#v, hits=%d (before %d)", entered.set, cache.hits, before)
	}
}

func TestConfigDependencyHeaderCacheInvalidatesEarlierIncludeShadow(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <selected.h>\n", "include/selected.h": "CONFIG_ORIGINAL\n",
	})
	cache := &configDependencyHeaderCache{}
	configure := func(shadow bool) func(*configDependencyClosureScanner, *configDependencyMacroState) {
		return func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.includeDirectories = []configDependencyIncludeDirectory{
				{logical: "generated"}, {logical: "include", source: true},
			}
			if shadow {
				scanner.generated["generated/selected.h"] = true
				scanner.generatedText["generated/selected.h"] = configDependencyGeneratedText{
					identity: "new-shadow", contents: "CONFIG_NEW_SHADOW\n",
				}
			}
		}
	}
	first := fixture.compare(t, cache, "drivers/example/driver.c", configure(false))
	if first.set.Opaque || !slices.Equal(first.set.Symbols, []string{"CONFIG_ORIGINAL"}) || cache.stores == 0 {
		t.Fatalf("initial unshadowed result = %#v, stores=%d", first.set, cache.stores)
	}
	before := cache.hits
	second := fixture.compare(t, cache, "drivers/example/driver.c", configure(true))
	if second.set.Opaque || !slices.Equal(second.set.Symbols, []string{"CONFIG_NEW_SHADOW"}) {
		t.Fatalf("new earlier shadow result = %#v", second.set)
	}
	if cache.hits != before {
		t.Fatal("an old source winner was reused after an earlier generated include became visible")
	}
	fixture.compare(t, cache, "drivers/example/driver.c", configure(false))
	if cache.hits == before {
		t.Fatal("rejecting a shadowed candidate discarded the original reusable entry")
	}
}

func TestConfigDependencyHeaderCacheRejectsChangedMacroSnapshot(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifdef CACHE_ALTERNATIVE\n#include \"second.h\"\n#else\n#include \"first.h\"\n#endif\n",
		"include/first.h":          "CONFIG_FIRST\n", "include/second.h": "CONFIG_SECOND\n",
	})
	cache := &configDependencyHeaderCache{}
	first := fixture.compare(t, cache, "drivers/example/driver.c", nil)
	if first.set.Opaque || !slices.Equal(first.set.Symbols, []string{"CONFIG_FIRST"}) {
		t.Fatalf("first result = %#v", first.set)
	}
	before := cache.hits
	second := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
		state.set("CACHE_ALTERNATIVE", configDependencyMacroDefined)
	})
	if second.set.Opaque || !slices.Equal(second.set.Symbols, []string{"CONFIG_SECOND"}) || cache.hits != before {
		t.Fatalf("changed namespace result = %#v, hits=%d (before %d)", second.set, cache.hits, before)
	}
}

func TestConfigDependencyHeaderCacheRejectsScannerAliasWithoutLosingPrefix(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include \"nested.h\"\nCONFIG_COMMON\n", "include/nested.h": "CONFIG_NESTED\n",
	})
	cache := &configDependencyHeaderCache{}
	fixture.compare(t, cache, "drivers/example/driver.c", nil)
	for _, conditional := range []bool{false, true} {
		t.Run(fmt.Sprintf("conditional=%t", conditional), func(t *testing.T) {
			before := cache.hits
			result := fixture.compare(t, cache, "drivers/example/driver.c", func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
				file, ok := scanner.sourceFile("include/nested.h")
				if !ok {
					t.Fatal("missing nested header")
				}
				file.logical = "alias/nested.h"
				if conditional {
					scanner.queueConditional(file)
				} else {
					scanner.queue(file)
				}
			})
			if !result.set.Opaque || !strings.Contains(result.set.Reason, "alias") || cache.hits != before {
				t.Fatalf("aliased scanner result = %#v, hits=%d (before %d)", result.set, cache.hits, before)
			}
		})
	}
}

func TestConfigDependencyHeaderCacheAdmissionBounds(t *testing.T) {
	files := map[string]string{"drivers/example/driver.c": "#include <common.h>\n", "include/common.h": "CONFIG_COMMON\n"}
	for index := 0; index < 8; index++ {
		files[fmt.Sprintf("drivers/example/unit_%d.c", index)] = fmt.Sprintf("#include <header_%d.h>\n", index)
		files[fmt.Sprintf("include/header_%d.h", index)] = fmt.Sprintf("CONFIG_HEADER_%d\n", index)
	}
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, files)
	t.Run("entries", func(t *testing.T) {
		cache := &configDependencyHeaderCache{maximumEntries: 2}
		for index := 0; index < 8; index++ {
			fixture.compare(t, cache, fmt.Sprintf("drivers/example/unit_%d.c", index), nil)
			if cache.entryCount > 2 {
				t.Fatalf("retained %d entries with maximum 2", cache.entryCount)
			}
		}
		if cache.entryCount != 2 || cache.rejected == 0 {
			t.Fatalf("bounded entry accounting = entries %d rejected %d", cache.entryCount, cache.rejected)
		}
		before := cache.hits
		fixture.compare(t, cache, "drivers/example/unit_0.c", nil)
		if cache.hits == before {
			t.Fatal("full cache could not reuse an already admitted entry")
		}
	})
	for _, test := range []struct {
		name  string
		cache configDependencyHeaderCache
	}{
		{name: "records", cache: configDependencyHeaderCache{maximumRecords: 1}},
		{name: "bytes", cache: configDependencyHeaderCache{maximumBytes: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := &test.cache
			for range 2 {
				fixture.compare(t, cache, "drivers/example/driver.c", nil)
			}
			if cache.entryCount != 0 || cache.records != 0 || cache.bytes != 0 || cache.rejected == 0 {
				t.Fatalf("oversized entry retained state: entries=%d records=%d bytes=%d rejected=%d", cache.entryCount, cache.records, cache.bytes, cache.rejected)
			}
		})
	}
}

func TestConfigDependencyHeaderCachePreservesSkippedGuardReentry(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <prelude.h>\n#include <common.h>\n#undef CACHE_LEAF_H\n#include <leaf.h>\nCONFIG_UNIT\n",
		"include/prelude.h":        "CONFIG_PRELUDE\n",
		"include/common.h":         "#include \"leaf.h\"\n",
		"include/leaf.h":           "#ifndef CACHE_LEAF_H\n#define CACHE_LEAF_H\nCONFIG_REENTRY\n#endif\n",
	})
	fixture.root.set("CACHE_LEAF_H", configDependencyMacroDefined)
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_PRELUDE", "CONFIG_REENTRY", "CONFIG_UNIT"}) || len(result.set.SourcePaths) != 4 {
			t.Fatalf("iteration %d re-entered guard result = %#v", iteration, result.set)
		}
		if iteration > 0 && cache.hits == before {
			t.Fatal("guard re-entry fixture did not exercise a warm cache")
		}
	}
}

func TestConfigDependencyHeaderCachePreservesGuardedRecursion(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <prelude.h>\n#include <common.h>\n#ifdef CACHE_READY\n#include <right.h>\n#else\n#include <wrong.h>\n#endif\n",
		"include/prelude.h":        "CONFIG_PRELUDE\n",
		"include/common.h":         "#ifndef CACHE_COMMON_H\n#define CACHE_COMMON_H\nCONFIG_COMMON\n#include \"common.h\"\n#define CACHE_READY\n#endif\n",
		"include/right.h":          "CONFIG_RIGHT\n", "include/wrong.h": "CONFIG_WRONG\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_COMMON", "CONFIG_PRELUDE", "CONFIG_RIGHT"}) {
			t.Fatalf("iteration %d recursive result = %#v", iteration, result.set)
		}
		if iteration > 0 && cache.hits == before {
			t.Fatal("guarded recursive fixture did not exercise a warm cache")
		}
	}
}

func TestConfigDependencyHeaderCacheDoesNotAdmitFailedHeader(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifndef CACHE_COMMON_H\n#define CACHE_COMMON_H\n#define CACHE_SUCCESSFUL_PREFIX\n#include \"missing.h\"\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if !result.set.Opaque || !strings.Contains(result.set.Reason, "unavailable") {
			t.Fatalf("iteration %d failed header result = %#v", iteration, result.set)
		}
	}
	if cache.entryCount != 0 || cache.stores != 0 || cache.hits != 0 {
		t.Fatalf("failed header cache accounting: entries=%d stores=%d hits=%d", cache.entryCount, cache.stores, cache.hits)
	}
}

func TestConfigDependencyHeaderCacheDoesNotAdmitPossibleInclude(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifdef _CACHE_UNKNOWN_SELECTOR\n#include <common.h>\n#endif\n",
		"include/common.h":         "#define CACHE_POSSIBLE_EFFECT\nCONFIG_POSSIBLE\n",
	})
	cache := &configDependencyHeaderCache{}
	for range 2 {
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_POSSIBLE"}) {
			t.Fatalf("possible include result = %#v", result.set)
		}
	}
	if cache.entryCount != 0 || cache.hits != 0 || cache.misses != 0 {
		t.Fatalf("possibly executed include entered direct-header cache: entries=%d hits=%d misses=%d", cache.entryCount, cache.hits, cache.misses)
	}
}

func TestConfigDependencyHeaderCacheRevalidatesPreconfiguredProducerBinding(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <selected.h>\n",
		"immutable/selected.h":     "CONFIG_SELECTED\n",
	})
	physical, ok := ResolveCompactKbuildProfileSourcePath(fixture.profile, "immutable/selected.h")
	if !ok {
		t.Fatal("preconfigured fixture does not resolve")
	}
	configure := func(producer string) func(*configDependencyClosureScanner, *configDependencyMacroState) {
		return func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "generated"}, {logical: "include", source: true}}
			scanner.generated["generated/selected.h"] = true
			scanner.preconfigured["generated/selected.h"] = physical
			scanner.lookupWorkingInput = func(pathname string) (configDependencyInputSetProvenance, bool, error) {
				if pathname != "generated/selected.h" || producer == "" {
					return configDependencyInputSetProvenance{}, false, nil
				}
				return configDependencyInputSetProvenance{entry: ActionPlanInputSetEntry{
					Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
					ProducerID: producer, Slot: 0, CompilerUse: true,
				}}, true, nil
			}
		}
	}
	cache := &configDependencyHeaderCache{}
	fixture.compare(t, cache, "drivers/example/driver.c", configure("producer-a"))
	if cache.stores == 0 {
		t.Fatal("explicit preconfigured producer fixture was not admitted")
	}
	before := cache.hits
	result := fixture.compare(t, cache, "drivers/example/driver.c", configure("producer-b"))
	if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_SELECTED"}) || cache.hits != before {
		t.Fatalf("same-physical changed producer result=%#v hits=%d (before %d)", result.set, cache.hits, before)
	}
	fixture.compare(t, cache, "drivers/example/driver.c", configure("producer-b"))
	if cache.hits == before {
		t.Fatal("validated new producer binding was not reusable")
	}
	before = cache.hits
	fixture.compare(t, cache, "drivers/example/driver.c", configure(""))
	if cache.hits != before {
		t.Fatal("unbound preconfigured producer reused an explicitly bound entry")
	}
	unbound := &configDependencyHeaderCache{}
	fixture.compare(t, unbound, "drivers/example/driver.c", configure(""))
	if unbound.stores != 0 {
		t.Fatal("preconfigured generated file without exact producer provenance was admitted")
	}
}

func TestConfigDependencyHeaderCacheRevalidatesOrdinaryAutoconfShadow(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <generated/autoconf.h>\n#ifdef CONFIG_NEW_SELECTOR\n#include <new.h>\n#endif\n",
		"include/new.h":            "CONFIG_NEW_DEPENDENCY\n",
		"immutable/shadow.h":       "#define CONFIG_NEW_SELECTOR\nCONFIG_SHADOW\n",
	})
	configure := func(shadow bool) func(*configDependencyClosureScanner, *configDependencyMacroState) {
		return func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "include"}, {logical: "include", source: true}}
			if shadow {
				scanner.lookupWorkingInput = func(pathname string) (configDependencyInputSetProvenance, bool, error) {
					if pathname != configDependencyAutoconfPath {
						return configDependencyInputSetProvenance{}, false, nil
					}
					return configDependencyInputSetProvenance{
						entry: ActionPlanInputSetEntry{
							Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
							SourceID: "shadow-source", CompilerUse: true,
						},
						source: ActionPlanSource{ID: "shadow-source", Namespace: "kernel", Path: "immutable/shadow.h"},
					}, true, nil
				}
			}
		}
	}
	cache := &configDependencyHeaderCache{}
	first := fixture.compare(t, cache, "drivers/example/driver.c", configure(false))
	if first.set.Opaque || !slices.Equal(first.set.Symbols, []string{"CONFIG_NEW_SELECTOR"}) || cache.stores == 0 {
		t.Fatalf("initial virtual autoconf result=%#v stores=%d", first.set, cache.stores)
	}
	before := cache.hits
	second := fixture.compare(t, cache, "drivers/example/driver.c", configure(true))
	if second.set.Opaque || !slices.Equal(second.set.Symbols, []string{"CONFIG_NEW_DEPENDENCY", "CONFIG_NEW_SELECTOR", "CONFIG_SHADOW"}) || cache.hits != before {
		t.Fatalf("ordinary autoconf shadow result=%#v hits=%d (before %d)", second.set, cache.hits, before)
	}
}

func TestConfigDependencyHeaderCacheOwnsRetainedScannerSummaries(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifndef CACHE_COMMON_H\n#define CACHE_COMMON_H\n#define CACHE_ALIAS CACHE_SAFE\nCONFIG_ORIGINAL\n#endif\n",
	})
	cache := &configDependencyHeaderCache{}
	for iteration := 0; iteration < 2; iteration++ {
		scanner := fixture.scanner(cache)
		state := fixture.root.branch()
		unit, ok := scanner.sourceFile("drivers/example/driver.c")
		if !ok {
			t.Fatal("missing translation unit")
		}
		scanner.interpretConditionalCompilerFiles([]configDependencyScanFile{unit}, state, configDependencyResolvedAutoconfDefinitions{})
		for _, parsed := range scanner.conditionalParsed {
			for index := range parsed.symbols {
				parsed.symbols[index] = "CONFIG_CORRUPTED"
			}
			for index := range parsed.macroDefinitions {
				for inner := range parsed.macroDefinitions[index].identifiers {
					parsed.macroDefinitions[index].identifiers[inner] = "_Pragma"
				}
			}
		}
		for index := range scanner.pending {
			scanner.pending[index].logical = "corrupted/pending.h"
		}
		for key := range scanner.conditionalRepeatGuards {
			scanner.conditionalRepeatGuards[key] = "CORRUPTED_GUARD"
		}
		// The deliberately poisoned scanner may share syntax-only backing with
		// the fixture. A fresh reference parser must read the immutable bytes;
		// retained header events must own their nested summaries independently.
		fixture.syntax = map[string]configDependencyConditionalSyntax{}
		fixture.parsed = map[string]configDependencyParsedFile{}
		before := cache.hits
		result := fixture.compare(t, cache, "drivers/example/driver.c", nil)
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_ORIGINAL"}) || cache.hits == before {
			t.Fatalf("iteration %d retained summary result=%#v hits=%d (before %d)", iteration, result.set, cache.hits, before)
		}
	}
}

func TestConfigDependencyHeaderCacheSeparatesScannerContexts(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*configDependencyClosureScanner, *configDependencyMacroState)
	}{
		{name: "profile", configure: func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.profile.Name = "different-profile"
		}},
		{name: "language", configure: func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.language = "assembler-with-cpp"
		}},
		{name: "search class", configure: func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
			scanner.quoteDirectories = []configDependencyIncludeDirectory{{logical: "different-quote-root", source: true}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
				"drivers/example/driver.c": "#include <common.h>\n", "include/common.h": "CONFIG_COMMON\n",
			})
			cache := &configDependencyHeaderCache{}
			fixture.compare(t, cache, "drivers/example/driver.c", nil)
			before := cache.hits
			result := fixture.compare(t, cache, "drivers/example/driver.c", test.configure)
			if result.set.Opaque || cache.hits != before {
				t.Fatalf("changed context result=%#v hits=%d (before %d)", result.set, cache.hits, before)
			}
			fixture.compare(t, cache, "drivers/example/driver.c", test.configure)
			if cache.hits == before {
				t.Fatal("unchanged second context could not reuse its own entry")
			}
		})
	}
}

func TestConfigDependencyHeaderCacheRejectsForcedHeaderTrace(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n", "include/common.h": "#define CACHE_EFFECT\nCONFIG_COMMON\n",
	})
	cache := &configDependencyHeaderCache{}
	fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
		if !state.beginForcedHeaderTrace() {
			t.Fatal("cannot begin existing forced-header trace")
		}
	})
	if cache.hits != 0 || cache.misses != 0 || cache.stores != 0 {
		t.Fatalf("forced-header tracing entered ordinary cache: hits=%d misses=%d stores=%d", cache.hits, cache.misses, cache.stores)
	}
}

func TestConfigDependencyHeaderCacheRejectsActiveAncestorAndRollsBackReplay(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <selected.h>\n", "include/selected.h": "CONFIG_SELECTED\n",
	})
	cache := &configDependencyHeaderCache{}
	configure := func(scanner *configDependencyClosureScanner, _ *configDependencyMacroState) {
		scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "generated"}, {logical: "include", source: true}}
	}
	fixture.compare(t, cache, "drivers/example/driver.c", configure)
	var entry configDependencyHeaderCacheEntry
	found := false
	for key, entries := range cache.entries {
		if key.file.logical == "include/common.h" && len(entries) != 0 {
			entry, found = entries[0], true
		}
	}
	if !found {
		t.Fatal("common header was not admitted")
	}
	for _, ancestor := range []string{"include/common.h", "include/selected.h"} {
		t.Run(ancestor, func(t *testing.T) {
			scanner := fixture.scanner(nil)
			configure(scanner, nil)
			file, ok := scanner.sourceFile(ancestor)
			if !ok {
				t.Fatal("missing ancestor fixture")
			}
			if _, exact := entry.replay(scanner, map[string]*configDependencyConditionalActiveFile{file.identity(): {}}); exact {
				t.Fatal("replay accepted an active ancestor inside its recorded closure")
			}
			if len(scanner.pending) != 0 || len(scanner.conditionalFiles) != 0 || len(scanner.sourcePaths) != 0 {
				t.Fatal("rejected ancestor replay changed the original scanner")
			}
		})
	}
	scanner := fixture.scanner(nil)
	configure(scanner, nil)
	scanner.generated["generated/selected.h"] = true
	scanner.resolveGeneratedText = func(pathname string) (configDependencyGeneratedText, bool) {
		if pathname != "generated/selected.h" {
			return configDependencyGeneratedText{}, false
		}
		return configDependencyGeneratedText{identity: "shadow", contents: "CONFIG_SHADOW\n"}, true
	}
	if _, exact := entry.replay(scanner, nil); exact {
		t.Fatal("replay accepted a newly generated earlier include winner")
	}
	if len(scanner.generatedText) != 0 || len(scanner.resolvedGeneratedFiles) != 0 || scanner.opaqueReason != "" ||
		len(scanner.pending) != 0 || len(scanner.conditionalParsed) != 0 || len(scanner.queued) != 0 {
		t.Fatal("rejected generated-shadow replay leaked scratch bindings or partial queue edits")
	}
}

func configDependencyHeaderCacheCellForTest(result configDependencyHeaderCacheObservationForTest, name string) configDependencyMacroSnapshotCell {
	prefix := "1:"
	if strings.HasPrefix(name, "CONFIG_") {
		prefix = "0:"
	} else if strings.HasPrefix(name, "_") {
		prefix = "2:"
	}
	if cell, exists := result.cells[prefix+name]; exists {
		return cell
	}
	return result.cells[prefix]
}

func TestConfigDependencyHeaderCacheSpecializationPreservesUnreadEntryCells(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifdef CACHE_ENABLED\n#define CACHE_OUTPUT\nCONFIG_COMMON\n#endif\n",
	})
	fixture.root.set("CACHE_ENABLED", configDependencyMacroDefined)
	cache := &configDependencyHeaderCache{}
	for index, suffix := range []string{"FIRST", "SECOND", "THIRD"} {
		beforeHits, beforeMisses, beforeSpecialized := cache.hits, cache.misses, cache.specializedHits
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("TU_"+suffix, configDependencyMacroDefined)
			state.set("CONFIG_UNREAD_"+suffix, configDependencyMacroDefined)
		})
		if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_COMMON"}) ||
			configDependencyHeaderCacheCellForTest(result, "CACHE_OUTPUT").definition != configDependencyMacroDefined ||
			configDependencyHeaderCacheCellForTest(result, "TU_"+suffix).definition != configDependencyMacroDefined ||
			configDependencyHeaderCacheCellForTest(result, "CONFIG_UNREAD_"+suffix).definition != configDependencyMacroDefined {
			t.Fatalf("entry %s lost its own cells or the header effect: %#v", suffix, result)
		}
		if index > 0 {
			if cache.hits != beforeHits+1 || cache.misses != beforeMisses || cache.specializedHits != beforeSpecialized+1 {
				t.Fatalf("unread entry change missed: hits=%d/%d misses=%d/%d", cache.hits, beforeHits, cache.misses, beforeMisses)
			}
			if configDependencyHeaderCacheCellForTest(result, "TU_FIRST").definition != configDependencyMacroUndefined ||
				configDependencyHeaderCacheCellForTest(result, "CONFIG_UNREAD_FIRST").definition != configDependencyMacroUndefined {
				t.Fatal("specialization copied unread cells from its captured entry")
			}
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationReplaysEqualValueWrites(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#define CACHE_OVERWRITE\nCONFIG_COMMON\n",
	})
	cache := &configDependencyHeaderCache{}
	for index, initial := range []configDependencyMacroDefinition{configDependencyMacroDefined, configDependencyMacroUndefined} {
		beforeHits, beforeMisses, beforeSpecialized := cache.hits, cache.misses, cache.specializedHits
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("CACHE_OVERWRITE", initial)
		})
		if result.set.Opaque || configDependencyHeaderCacheCellForTest(result, "CACHE_OVERWRITE").definition != configDependencyMacroDefined {
			t.Fatalf("entry %d did not execute the explicit write: %#v", index, result)
		}
		if index > 0 && (cache.hits != beforeHits+1 || cache.misses != beforeMisses || cache.specializedHits != beforeSpecialized+1) {
			t.Fatal("write equal to the captured input was not reusable against a different input")
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationWitnessesOptionalUnchangedArm(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifdef _CACHE_UNKNOWN_SELECTOR\n#include \"setter.h\"\n#endif\nCONFIG_COMMON\n",
		"include/setter.h":         "#define CACHE_JOIN_TARGET\n",
	})
	cache := &configDependencyHeaderCache{}
	for index, initial := range []configDependencyMacroDefinition{configDependencyMacroDefined, configDependencyMacroUndefined, configDependencyMacroUndefined} {
		beforeHits, beforeSpecialized := cache.hits, cache.specializedHits
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("CACHE_JOIN_TARGET", initial)
			state.set(fmt.Sprintf("TU_UNREAD_%d", index), configDependencyMacroDefined)
		})
		want := configDependencyMacroDefined
		if index > 0 {
			want = configDependencyMacroUnknown
		}
		if result.set.Opaque || configDependencyHeaderCacheCellForTest(result, "CACHE_JOIN_TARGET").definition != want {
			t.Fatalf("entry %d optional join result = %#v", index, result)
		}
		if index == 1 && cache.hits != beforeHits {
			t.Fatal("changed unchanged-arm input reused an incompatible join specialization")
		}
		if index == 2 && (cache.hits != beforeHits+1 || cache.specializedHits != beforeSpecialized+1) {
			t.Fatal("matching unchanged-arm input did not reuse its specialization")
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationReplaysWildcardOnCurrentNamespace(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <numeric.h>\n#define CACHE_EXACT\nCONFIG_COMMON\n",
	})
	cache := &configDependencyHeaderCache{}
	for index, suffix := range []string{"FIRST", "SECOND"} {
		beforeHits, beforeMisses, beforeSpecialized := cache.hits, cache.misses, cache.specializedHits
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(scanner *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("TU_"+suffix, configDependencyMacroDefined)
			state.set("CONFIG_UNREAD_"+suffix, configDependencyMacroDefined)
			scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "generated"}, {logical: "include", source: true}}
			scanner.generated["generated/numeric.h"] = true
			scanner.generatedText["generated/numeric.h"] = configDependencyGeneratedText{identity: "validated-numeric-producer", macroTable: true}
		})
		if result.set.Opaque || configDependencyHeaderCacheCellForTest(result, "TU_"+suffix).definition != configDependencyMacroDefined ||
			configDependencyHeaderCacheCellForTest(result, "CONFIG_UNREAD_"+suffix).definition != configDependencyMacroDefined ||
			configDependencyHeaderCacheCellForTest(result, "CACHE_EXACT").definition != configDependencyMacroDefined ||
			configDependencyHeaderCacheCellForTest(result, "UNSPECIFIED_ORDINARY").definition != configDependencyMacroUnknown {
			t.Fatalf("entry %s wildcard result = %#v", suffix, result)
		}
		if index > 0 {
			if cache.hits != beforeHits+1 || cache.misses != beforeMisses || cache.specializedHits != beforeSpecialized+1 {
				t.Fatal("wildcard specialization did not reuse against unread current-entry changes")
			}
			if configDependencyHeaderCacheCellForTest(result, "TU_FIRST").definition != configDependencyMacroUnknown ||
				configDependencyHeaderCacheCellForTest(result, "CONFIG_UNREAD_FIRST").definition != configDependencyMacroUndefined {
				t.Fatal("wildcard replay imported captured-entry exact cells")
			}
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationWitnessesRecordedAutoconfGuard(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#include <generated/autoconf.h>\nCONFIG_COMMON\n",
	})
	fixture.autoconf = newConfigDependencyResolvedAutoconfDefinitions(map[string]string{"CONFIG_APPLIED": "y"})
	cache := &configDependencyHeaderCache{}
	for index, recorded := range []bool{false, true, true} {
		beforeHits, beforeSpecialized := cache.hits, cache.specializedHits
		result := fixture.compare(t, cache, "drivers/example/driver.c", func(scanner *configDependencyClosureScanner, state *configDependencyMacroState) {
			scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "include"}, {logical: "include", source: true}}
			if !state.putSnapshotCell(configDependencyResolvedAutoconfGuard, configDependencyMacroSnapshotCell{
				definition: configDependencyMacroUnknown, recorded: recorded,
			}) {
				t.Fatal("cannot set the autoconf guard cell")
			}
			state.set(fmt.Sprintf("TU_UNREAD_%d", index), configDependencyMacroDefined)
		})
		want := configDependencyMacroDefined
		if recorded {
			want = configDependencyMacroUnknown
		}
		if result.set.Opaque || configDependencyHeaderCacheCellForTest(result, "CONFIG_APPLIED").definition != want {
			t.Fatalf("recorded=%t autoconf result = %#v", recorded, result)
		}
		if index == 1 && cache.hits != beforeHits {
			t.Fatal("same definedness with different recorded bit reused the wrong autoconf effect")
		}
		if index == 2 && (cache.hits != beforeHits+1 || cache.specializedHits != beforeSpecialized+1) {
			t.Fatal("matching full guard cell did not reuse its specialization")
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationVariantBound(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifdef CACHE_SWITCH\n#define CACHE_OUTPUT\n#endif\nCONFIG_COMMON\n",
	})
	cache := &configDependencyHeaderCache{maximumVariants: 1}
	for index, initial := range []configDependencyMacroDefinition{configDependencyMacroUndefined, configDependencyMacroDefined, configDependencyMacroUndefined} {
		beforeHits := cache.hits
		fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
			state.set("CACHE_SWITCH", initial)
			state.set(fmt.Sprintf("TU_UNREAD_%d", index), configDependencyMacroDefined)
		})
		if cache.entryCount != 1 || len(cache.entries) != 1 {
			t.Fatalf("entry %d exceeded the per-key variant bound: entries=%d keys=%d", index, cache.entryCount, len(cache.entries))
		}
		if index == 1 && (cache.hits != beforeHits || cache.rejectedVariants != 1) {
			t.Fatal("changed consumed cell did not fall back after the variant budget was exhausted")
		}
		if index == 2 && cache.hits != beforeHits+1 {
			t.Fatal("exhausted variant budget prevented reuse of an existing specialization")
		}
	}
}

func TestConfigDependencyHeaderCacheSpecializationCountsMacroPayloadBudget(t *testing.T) {
	fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <common.h>\n",
		"include/common.h":         "#ifdef CACHE_ENABLED\n#define CACHE_WRITTEN\n#endif\nCONFIG_COMMON\n",
	})
	fixture.root.set("CACHE_ENABLED", configDependencyMacroDefined)
	probe := &configDependencyHeaderCache{}
	fixture.compare(t, probe, "drivers/example/driver.c", nil)
	if probe.entryCount != 1 {
		t.Fatal("macro-payload probe did not retain one entry")
	}
	eventRecords, eventAndKeyBytes := 0, 0
	for key, entries := range probe.entries {
		entry := entries[0]
		if len(entry.specialization.requirements) == 0 || entry.specialization.transform.exactSize() == 0 {
			t.Fatal("macro-payload fixture has no consumed cells or explicit writes")
		}
		for _, event := range entry.events {
			records, bytes := event.retainedSize()
			eventRecords += records
			eventAndKeyBytes += bytes
		}
		eventAndKeyBytes += len(key.profile) + len(key.language) + len(key.search) + len(key.file.logical) + len(key.file.physical)
	}
	if eventRecords <= 0 || probe.records <= eventRecords || probe.bytes <= eventAndKeyBytes {
		t.Fatal("macro-payload fixture does not separate event/key cost from macro support")
	}
	for _, test := range []struct {
		name  string
		cache configDependencyHeaderCache
	}{
		{name: "records", cache: configDependencyHeaderCache{maximumRecords: eventRecords}},
		{name: "bytes", cache: configDependencyHeaderCache{maximumBytes: eventAndKeyBytes}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := &test.cache
			fixture.compare(t, cache, "drivers/example/driver.c", nil)
			if cache.entryCount != 0 || cache.records != 0 || cache.bytes != 0 || cache.rejectedTrace != 0 {
				t.Fatalf("macro-payload overflow retained data or overflowed the event trace: %#v", cache)
			}
			if test.name == "records" && cache.rejectedRecords != 1 || test.name == "bytes" && cache.rejectedBytes != 1 {
				t.Fatalf("macro-payload overflow had the wrong rejection reason: %#v", cache)
			}
		})
	}
}
