package kconfig

import "testing"

func TestConfigDependencyForcedHeaderResolutionMissDoesNotPublishScratchState(t *testing.T) {
	for _, failure := range []string{"different-winner", "unavailable-later-header"} {
		t.Run(failure, func(t *testing.T) {
			root := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
			before := root.snapshot
			file := configDependencyScanFile{logical: "include/selected.h", exactIdentity: "producer:0", exactContents: "CONFIG_SELECTED\n"}
			event := configDependencyHeaderEvent{
				kind: configDependencyHeaderResolve, file: file,
				including: configDependencyScanFile{logical: "forced.h", source: true},
				include:   configDependencyLiteralInclude{name: "selected.h"},
			}
			entry := configDependencyForcedHeaderCacheEntry{
				inputSnapshot: before, snapshot: before, includeResolutionsReady: true,
				includeResolutions: []configDependencyHeaderEvent{event},
			}
			if failure == "unavailable-later-header" {
				missing := event
				missing.include.name = "missing.h"
				missing.file.logical = "include/missing.h"
				entry.includeResolutions = append(entry.includeResolutions, missing)
			}
			calls := 0
			scanner := &configDependencyClosureScanner{
				includeDirectories:     []configDependencyIncludeDirectory{{logical: "include"}},
				generated:              map[string]bool{"include/selected.h": true, "include/missing.h": true},
				generatedText:          map[string]configDependencyGeneratedText{},
				resolvedGeneratedFiles: map[string]configDependencyScanFile{},
				resolveGeneratedText: func(logical string) (configDependencyGeneratedText, bool) {
					calls++
					if logical != file.logical {
						return configDependencyGeneratedText{}, false
					}
					identity := file.exactIdentity
					if failure == "different-winner" {
						identity = "replacement:0"
					}
					return configDependencyGeneratedText{identity: identity, contents: file.exactContents}, true
				},
			}
			if _, restored := entry.restore(scanner, root); restored {
				t.Fatal("invalid closure reused")
			}
			if calls == 0 {
				t.Fatal("resolution validation was not exercised")
			}
			if scanner.opaqueReason != "" || len(scanner.generatedText) != 0 || len(scanner.resolvedGeneratedFiles) != 0 ||
				len(scanner.pending) != 0 || root.snapshot != before || root.tainted || root.consumed {
				t.Fatalf("failed cache validation published scratch state: scanner=%#v root=%#v", scanner, root)
			}
		})
	}
}

func TestConfigDependencyForcedHeaderResolutionStoreHonorsCumulativeLimits(t *testing.T) {
	event := configDependencyHeaderEvent{kind: configDependencyHeaderResolve, include: configDependencyLiteralInclude{name: "a.h"}}
	entry := configDependencyForcedHeaderCacheEntry{includeResolutionsReady: true, includeResolutions: []configDependencyHeaderEvent{event}}
	records, bytes := event.retainedSize()
	for _, limit := range []string{"records", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			cache := configDependencyForcedHeaderCache{}
			if limit == "records" {
				cache.resolutionRecords = (1 << 18) - records
			} else {
				cache.resolutionBytes = (32 << 20) - bytes
			}
			key := configDependencyForcedHeaderCacheKey{profile: "at-limit"}
			cache.store(key, entry)
			if len(cache.entries[key]) != 1 {
				t.Fatal("entry exactly at cumulative limit rejected")
			}
			recordsBefore, bytesBefore := cache.resolutionRecords, cache.resolutionBytes
			cache.store(key, entry)
			if len(cache.entries[key]) != 1 || cache.resolutionRecords != recordsBefore || cache.resolutionBytes != bytesBefore {
				t.Fatal("overflowing entry changed retained cache state")
			}
			entry.includeResolutionsReady = false
			cache.store(configDependencyForcedHeaderCacheKey{profile: "incomplete"}, entry)
			entry.includeResolutionsReady = true
			if len(cache.entries) != 1 {
				t.Fatal("incomplete entry admitted")
			}
		})
	}
}
