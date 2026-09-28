package kconfig

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func configDependencyHeaderCacheSaturationFixtureForTest(t *testing.T) *configDependencyHeaderCacheFixtureForTest {
	t.Helper()
	var large strings.Builder
	for index := 0; index < 96; index++ {
		fmt.Fprintf(&large, "#define CACHE_LARGE_MACRO_%03d 1\n", index)
	}
	return newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <small.h>\n",
		"drivers/example/large.c":  "#include <large.h>\n",
		"drivers/example/fresh.c":  "#include <fresh.h>\n",
		"include/small.h": "#ifndef CACHE_SMALL_H\n#define CACHE_SMALL_H\n" +
			"#ifdef CACHE_SWITCH\n#define CACHE_SELECTED\n#endif\n#include <probe.h>\nCONFIG_SMALL\n#endif\n",
		"include/large.h": large.String(),
		"include/fresh.h": "#ifndef CACHE_FRESH_H\n#define CACHE_FRESH_H\n#define CACHE_FRESH_WRITE\n#endif\n",
	})
}

func configureConfigDependencyHeaderCacheSaturationForTest(
	scanner *configDependencyClosureScanner,
	_ *configDependencyMacroState,
) {
	scanner.includeDirectories = []configDependencyIncludeDirectory{{logical: "generated"}, {logical: "include", source: true}}
	scanner.generated["generated/probe.h"] = true
	scanner.resolveGeneratedText = func(pathname string) (configDependencyGeneratedText, bool) {
		if pathname != "generated/probe.h" {
			return configDependencyGeneratedText{}, false
		}
		return configDependencyGeneratedText{identity: "saturation-probe", contents: "CONFIG_PROBE\n"}, true
	}
}

func configDependencyHeaderCacheNearFullLimitForTest(
	t *testing.T,
	fixture *configDependencyHeaderCacheFixtureForTest,
	reason configDependencyHeaderRejection,
) *configDependencyHeaderCache {
	t.Helper()
	probe := &configDependencyHeaderCache{}
	fixture.compare(t, probe, "drivers/example/driver.c", configureConfigDependencyHeaderCacheSaturationForTest)
	if probe.stores != 1 {
		t.Fatal("small header did not establish exactly one entry")
	}
	cache := &configDependencyHeaderCache{}
	if reason == configDependencyHeaderRejectRecords {
		cache.maximumRecords = probe.records + 4
	} else {
		cache.maximumBytes = probe.bytes + 128
	}
	return cache
}

func TestConfigDependencyHeaderCacheSaturationEmptyOversizeAllowsSmallerEntry(t *testing.T) {
	for _, reason := range []configDependencyHeaderRejection{configDependencyHeaderRejectRecords, configDependencyHeaderRejectBytes} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			fixture := configDependencyHeaderCacheSaturationFixtureForTest(t)
			cache := configDependencyHeaderCacheNearFullLimitForTest(t, fixture, reason)
			fixture.compare(t, cache, "drivers/example/large.c", configureConfigDependencyHeaderCacheSaturationForTest)
			if cache.entryCount != 0 || cache.captureAttempts != 1 || cache.rejected != 1 || cache.admissionStopped {
				t.Fatalf("empty oversized attempt prevented future admission: %#v", cache)
			}
			fixture.compare(t, cache, "drivers/example/driver.c", configureConfigDependencyHeaderCacheSaturationForTest)
			if cache.entryCount != 1 || cache.captureAttempts != 2 || cache.admissionStopped {
				t.Fatal("smaller later header was not admitted after an empty-cache overflow")
			}
		})
	}
}

func TestConfigDependencyHeaderCacheSaturationPreservesHitsAndLiveFallback(t *testing.T) {
	for _, reason := range []configDependencyHeaderRejection{configDependencyHeaderRejectRecords, configDependencyHeaderRejectBytes} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			fixture := configDependencyHeaderCacheSaturationFixtureForTest(t)
			cache := configDependencyHeaderCacheNearFullLimitForTest(t, fixture, reason)
			fixture.compare(t, cache, "drivers/example/driver.c", configureConfigDependencyHeaderCacheSaturationForTest)
			fixture.compare(t, cache, "drivers/example/large.c", configureConfigDependencyHeaderCacheSaturationForTest)
			if !cache.admissionStopped || cache.admissionStopReason != reason || cache.entryCount != 1 || cache.captureAttempts != 2 {
				t.Fatalf("populated overflow did not latch admission: %#v", cache)
			}
			if cache.records >= cache.maximumRecords || cache.bytes >= cache.maximumBytes {
				t.Fatal("fixture must leave positive headroom to exercise near-full rather than exact-full admission")
			}
			attempts, records, bytes := cache.captureAttempts, cache.records, cache.bytes
			beforeExact := cache.exactHits
			fixture.compare(t, cache, "drivers/example/driver.c", configureConfigDependencyHeaderCacheSaturationForTest)
			if cache.exactHits != beforeExact+1 {
				t.Fatal("latched admission disabled an existing exact-input hit")
			}
			beforeSpecialized := cache.specializedHits
			fixture.compare(t, cache, "drivers/example/driver.c", func(scanner *configDependencyClosureScanner, state *configDependencyMacroState) {
				configureConfigDependencyHeaderCacheSaturationForTest(scanner, state)
				state.set("TU_UNREAD", configDependencyMacroDefined)
			})
			if cache.specializedHits != beforeSpecialized+1 {
				t.Fatal("latched admission disabled an existing specialized hit")
			}
			beforeMismatches := cache.requirementMismatches
			probeCalls := 0
			fixture.compare(t, cache, "drivers/example/driver.c", func(scanner *configDependencyClosureScanner, state *configDependencyMacroState) {
				configureConfigDependencyHeaderCacheSaturationForTest(scanner, state)
				state.set("CACHE_SWITCH", configDependencyMacroDefined)
				resolve := scanner.resolveGeneratedText
				scanner.resolveGeneratedText = func(pathname string) (configDependencyGeneratedText, bool) {
					if pathname == "generated/probe.h" {
						probeCalls++
						if scanner.headerTrace != nil {
							t.Fatal("mismatching populated bucket restarted cold tracing after saturation")
						}
					}
					return resolve(pathname)
				}
			})
			if cache.requirementMismatches != beforeMismatches+1 || probeCalls != 2 {
				t.Fatal("nonempty bucket did not miss and execute both live/reference include closures")
			}
			for iteration := 0; iteration < 3; iteration++ {
				fixture.compare(t, cache, "drivers/example/large.c", configureConfigDependencyHeaderCacheSaturationForTest)
				fixture.compare(t, cache, "drivers/example/fresh.c", configureConfigDependencyHeaderCacheSaturationForTest)
			}
			if cache.captureAttempts != attempts || cache.records != records || cache.bytes != bytes || cache.stores != 1 {
				t.Fatal("saturated live fallbacks retraced or retained additional cache payload")
			}
		})
	}
}

func TestConfigDependencyHeaderCacheSaturationEmptyBucketKeepsMacroStateSparse(t *testing.T) {
	fixture := configDependencyHeaderCacheSaturationFixtureForTest(t)
	cache := configDependencyHeaderCacheNearFullLimitForTest(t, fixture, configDependencyHeaderRejectRecords)
	fixture.compare(t, cache, "drivers/example/driver.c", configureConfigDependencyHeaderCacheSaturationForTest)
	fixture.compare(t, cache, "drivers/example/large.c", configureConfigDependencyHeaderCacheSaturationForTest)
	if !cache.admissionStopped {
		t.Fatal("fixture did not saturate admission")
	}
	scanner, reference := fixture.scanner(cache), fixture.scanner(nil)
	state, want := fixture.root.branch(), fixture.root.branch()
	state.set("PRIOR_SPARSE_WRITE", configDependencyMacroDefined)
	want.set("PRIOR_SPARSE_WRITE", configDependencyMacroDefined)
	file, found := scanner.sourceFile("include/fresh.h")
	unit, unitFound := scanner.sourceFile("drivers/example/driver.c")
	if !found || !unitFound || state.materializedSnapshotCache != nil {
		t.Fatal("invalid sparse empty-bucket fixture")
	}
	active := map[string]*configDependencyConditionalActiveFile{unit.identity(): {}}
	attempts := cache.captureAttempts
	if !scanner.interpretDirectCompilerHeader(file, state, fixture.autoconf, active) ||
		!reference.interpretConditionalCompilerFile(file, want, fixture.autoconf, active) {
		t.Fatal("live guarded header interpretation failed")
	}
	if state.materializedSnapshotCache != nil || cache.captureAttempts != attempts || state.forcedHeaderTrace != nil {
		t.Fatal("blocked empty bucket materialized or traced the caller's sparse state")
	}
	actual, _ := state.materializedSnapshot()
	expected, _ := want.materializedSnapshot()
	requireConfigDependencyMacroEffectSnapshotsEqual(t, actual, expected)
	if actualSet, expectedSet := scanner.scan(), reference.scan(); !reflect.DeepEqual(actualSet, expectedSet) {
		t.Fatalf("sparse fallback changed scanner result: got %#v want %#v", actualSet, expectedSet)
	}
}
