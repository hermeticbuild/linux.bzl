package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This measures repeated interpretation of an identical common-header prefix,
// not cross-config build cache hits. Both cases warm immutable syntax/status
// caches; only the completed ordinary-header effect cache differs.
func BenchmarkConfigDependencyHeaderCacheCommonClosure(b *testing.B) {
	rootDir := b.TempDir()
	write := func(name, contents string) {
		if err := os.WriteFile(filepath.Join(rootDir, name), []byte(contents), 0600); err != nil {
			b.Fatal(err)
		}
	}
	write("unit.c", "#include <common.h>\nCONFIG_UNIT\n")
	var common strings.Builder
	common.WriteString("#ifndef _BENCH_COMMON_H\n#define _BENCH_COMMON_H\n")
	for header := 0; header < 64; header++ {
		name := fmt.Sprintf("header_%d.h", header)
		fmt.Fprintf(&common, "#include <%s>\n", name)
		var contents strings.Builder
		fmt.Fprintf(&contents, "#ifndef _BENCH_HEADER_%d_H\n#define _BENCH_HEADER_%d_H\n", header, header)
		for macro := 0; macro < 100; macro++ {
			fmt.Fprintf(&contents, "#define BENCH_%d_%d 1\n", header, macro)
		}
		contents.WriteString("CONFIG_HEADER\n#endif\n")
		write(name, contents.String())
	}
	common.WriteString("#endif\n")
	write("common.h", common.String())
	profile := CompactKbuildProfile{
		Name: "header-cache-benchmark",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
			sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": rootDir},
		}},
	}
	var predefines strings.Builder
	for index := 0; index < 2048; index++ {
		fmt.Fprintf(&predefines, "#define PREDEFINED_%d 1\n", index)
	}
	initial, reason := parseConfigDependencyCompilerPredefines(predefines.String())
	if reason != "" {
		b.Fatal(reason)
	}
	for _, test := range []struct {
		name                 string
		enabled, specialized bool
	}{
		{"uncached", false, false}, {"exact_entry", true, false}, {"specialized_entry", true, true},
	} {
		b.Run(test.name, func(b *testing.B) {
			var cache *configDependencyHeaderCache
			if test.enabled {
				cache = &configDependencyHeaderCache{}
			}
			physical := newConfigDependencyPhysicalFileCache()
			syntax := map[string]configDependencyConditionalSyntax{}
			parsed := map[string]configDependencyParsedFile{}
			iteration := 0
			run := func() {
				scanner := configDependencyClosureScanner{
					sourceLookup: newConfigDependencySourceLookup(profile), language: "c", headerCache: cache,
					physicalFiles: physical, conditionalSyntax: syntax, parsed: parsed,
					includeDirectories: []configDependencyIncludeDirectory{{source: true}},
					symbols:            map[string]bool{}, sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
					queued: map[string]bool{}, queuedPhysical: map[string]bool{},
				}
				file, found := scanner.sourceFile("unit.c")
				if !found {
					b.Fatal("missing unit")
				}
				state := initial.branch()
				if test.specialized {
					state.set(fmt.Sprintf("UNREAD_BENCH_INPUT_%d", iteration), configDependencyMacroDefined)
				}
				iteration++
				scanner.interpretConditionalCompilerFiles([]configDependencyScanFile{file}, state, configDependencyResolvedAutoconfDefinitions{})
				if set := scanner.scan(); set.Opaque || len(set.SourcePaths) != 66 || len(set.Symbols) != 2 {
					b.Fatalf("unexpected scan result: %#v", set)
				}
			}
			run()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				run()
			}
			b.StopTimer()
			if test.enabled {
				b.ReportMetric(float64(cache.hits)/float64(b.N), "header-hits/op")
				if cache.hits != b.N {
					b.Fatalf("hits=%d want %d", cache.hits, b.N)
				}
				if test.specialized && cache.specializedHits != b.N {
					b.Fatalf("specialized hits=%d want %d", cache.specializedHits, b.N)
				}
			}
		})
	}
}

// This isolates the cost of initially unknown guards in a repeated diamond.
// Explicitly absent guards are a fixture assumption, NOT a permitted production
// inference from a missing -dM entry. Production requires a compiler witness.
func BenchmarkConfigDependencyGuardUncertaintyDiamond(b *testing.B) {
	for _, depth := range []int{4, 7, 10} {
		b.Run(fmt.Sprintf("depth-%d", depth), func(b *testing.B) {
			rootDir := b.TempDir()
			write := func(name, contents string) {
				if err := os.WriteFile(filepath.Join(rootDir, name), []byte(contents), 0600); err != nil {
					b.Fatal(err)
				}
			}
			write("unit.c", "#include <a0.h>\n#include <b0.h>\n")
			var guards []string
			for level := 0; level < depth; level++ {
				for _, side := range []string{"a", "b"} {
					guard := fmt.Sprintf("_DIAMOND_%s_%d_H", side, level)
					guards = append(guards, guard)
					var body strings.Builder
					fmt.Fprintf(&body, "#ifndef %s\n#define %s\n", guard, guard)
					if level+1 < depth {
						fmt.Fprintf(&body, "#include <a%d.h>\n#include <b%d.h>\n", level+1, level+1)
					} else {
						body.WriteString("CONFIG_LEAF\n")
					}
					body.WriteString("#endif\n")
					write(fmt.Sprintf("%s%d.h", side, level), body.String())
				}
			}
			profile := CompactKbuildProfile{
				Name: "guard-diamond-benchmark",
				evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
					sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": rootDir},
				}},
			}
			for _, known := range []bool{false, true} {
				name := "unknown"
				if known {
					name = "explicitly-absent"
				}
				b.Run(name, func(b *testing.B) {
					initial, reason := parseConfigDependencyCompilerPredefines("")
					if reason != "" {
						b.Fatal(reason)
					}
					if known {
						for _, guard := range guards {
							initial.set(guard, configDependencyMacroUndefined)
						}
					}
					physical := newConfigDependencyPhysicalFileCache()
					syntax := map[string]configDependencyConditionalSyntax{}
					parsed := map[string]configDependencyParsedFile{}
					run := func() {
						scanner := configDependencyClosureScanner{
							sourceLookup: newConfigDependencySourceLookup(profile), language: "c", physicalFiles: physical,
							conditionalSyntax: syntax, parsed: parsed,
							includeDirectories: []configDependencyIncludeDirectory{{source: true}},
							symbols:            map[string]bool{}, sourcePaths: map[string]bool{}, objectPaths: map[string]bool{},
							queued: map[string]bool{}, queuedPhysical: map[string]bool{},
						}
						file, found := scanner.sourceFile("unit.c")
						if !found {
							b.Fatal("missing translation unit")
						}
						state := initial.branch()
						scanner.interpretConditionalCompilerFiles([]configDependencyScanFile{file}, state, configDependencyResolvedAutoconfDefinitions{})
						if set := scanner.scan(); set.Opaque || len(set.SourcePaths) != 1+2*depth ||
							len(set.Symbols) != 1 || set.Symbols[0] != "CONFIG_LEAF" {
							b.Fatalf("unexpected diamond closure: %#v", set)
						}
					}
					run()
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						run()
					}
				})
			}
		})
	}
}

// Opt-in diagnostic over an existing immutable source checkout. Normal test
// runs do not read any external tree; this benchmark creates no source files.
func BenchmarkConfigDependencyGuardInventorySource(b *testing.B) {
	root := os.Getenv("LINUX_BZL_GUARD_INVENTORY_ROOT")
	if root == "" {
		b.Skip("set LINUX_BZL_GUARD_INVENTORY_ROOT to an existing source tree")
	}
	profile := CompactKbuildProfile{
		Name: "guard-inventory-benchmark",
		evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
			sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		}},
	}
	var names []string
	b.ReportAllocs()
	for b.Loop() {
		inventory := &configDependencyGuardInventory{}
		names = inventory.names(profile)
	}
	b.StopTimer()
	if len(names) == 0 {
		b.Fatal("source inventory is empty")
	}
	bytes := 0
	for _, name := range names {
		bytes += len(name) + 31
	}
	b.ReportMetric(float64(len(names)), "guard-names/op")
	b.ReportMetric(float64(bytes), "query-bytes/op")
}
