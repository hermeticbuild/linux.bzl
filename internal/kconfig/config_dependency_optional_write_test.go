package kconfig

import (
	"fmt"
	"slices"
	"testing"
)

func configDependencyOptionalWriteForTest(t *testing.T, state *configDependencyMacroState, condition, directive, name string) {
	t.Helper()
	source := "#if " + condition + "\n#" + directive + " " + name + "\n#endif\n"
	if _, reason := configDependencyLiteralIncludesWithMacroEffects([]byte(source), state, nil); reason != "" {
		t.Fatal(reason)
	}
}

func TestConfigDependencyOptionalWriteMatchesExecuteSkipJoin(t *testing.T) {
	for _, name := range []string{"OPTIONAL_TARGET", "_OPTIONAL_TARGET", "CONFIG_OPTIONAL_TARGET", configDependencyResolvedAutoconfGuard} {
		for index := 0; index < configDependencyMacroSnapshotCellCount; index++ {
			cell, _ := configDependencyMacroSnapshotCellAt(index)
			for _, directive := range []string{"define", "undef"} {
				t.Run(fmt.Sprintf("%s/cell%d/%s", name, index, directive), func(t *testing.T) {
					input, valid := newConfigDependencyMacroSnapshot().withCell(name, cell)
					if !valid {
						t.Fatal("invalid input cell")
					}
					actual := &configDependencyMacroState{snapshot: input}
					oracle := &configDependencyMacroState{snapshot: input}
					if !actual.beginForcedHeaderTrace() || !oracle.beginForcedHeaderTrace() {
						t.Fatal("cannot start traces")
					}
					configDependencyOptionalWriteForTest(t, actual, "defined(_OPTIONAL_SELECTOR)", directive, name)

					// Independently execute the definite write in one child, then
					// join that child with the unchanged path. Do not use the
					// conditional interpreter for either oracle branch.
					if oracle.definition("_OPTIONAL_SELECTOR") != configDependencyMacroUnknown {
						t.Fatal("selector must remain unknown")
					}
					written := configDependencyMacroDefined
					if directive == "undef" {
						written = configDependencyMacroUndefined
					}
					executed := oracle.branch()
					executed.set(name, written)
					oracle.mergePossibleBranch(executed)
					gotSnapshot, gotValid := actual.materializedSnapshot()
					wantSnapshot, wantValid := oracle.materializedSnapshot()
					if !gotValid || !wantValid {
						t.Fatal("optional write or execute/skip join became invalid")
					}
					requireConfigDependencyMacroEffectSnapshotsEqual(t, gotSnapshot, wantSnapshot)

					// Check the scalar truth table independently of the join
					// implementation, including the one observable recorded bit.
					want := configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown}
					if cell.definition == written {
						want.definition = written
					}
					want.recorded = name == configDependencyResolvedAutoconfGuard
					if got, valid := actual.snapshotCell(name); !valid || got != want {
						t.Fatalf("optional %s of %#v = %#v, valid=%t, want %#v", directive, cell, got, valid, want)
					}
					for label, state := range map[string]*configDependencyMacroState{"actual": actual, "oracle": oracle} {
						if state.forcedHeaderTrace.invalid || state.forcedHeaderTrace.reads.size() != 2 ||
							!state.forcedHeaderTrace.reads.contains("_OPTIONAL_SELECTOR") ||
							!state.forcedHeaderTrace.reads.contains(name) ||
							state.forcedHeaderTouches.size() != 1 || !state.forcedHeaderTouches.contains(name) {
							t.Fatalf("%s lost a read or explicit touch for an optional write", label)
						}
					}
				})
			}
		}
	}
}

func TestConfigDependencyOptionalWritePreservesNamespaceFallbacks(t *testing.T) {
	for _, name := range []string{"OPTIONAL_ABSENT", "CONFIG_OPTIONAL_ABSENT", "_OPTIONAL_ABSENT", configDependencyResolvedAutoconfGuard} {
		for _, directive := range []string{"define", "undef"} {
			t.Run(name+"/"+directive, func(t *testing.T) {
				input := newConfigDependencyMacroSnapshot()
				state := &configDependencyMacroState{snapshot: input}
				if !state.beginForcedHeaderTrace() {
					t.Fatal("cannot start trace")
				}
				configDependencyOptionalWriteForTest(t, state, "defined(_OPTIONAL_SELECTOR)", directive, name)
				want := configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown}
				if directive == "undef" && name[0] != '_' {
					want.definition = configDependencyMacroUndefined
				}
				want.recorded = name == configDependencyResolvedAutoconfGuard
				if got, valid := state.snapshotCell(name); !valid || got != want {
					t.Fatalf("absent %s after optional %s = %#v, valid=%t, want %#v", name, directive, got, valid, want)
				}
				if !state.forcedHeaderTrace.reads.contains(name) || !state.forcedHeaderTouches.contains(name) {
					t.Fatal("fallback input did not retain its read and touch")
				}
				// Updating one absent reserved name must not change the
				// unknown fallback for the rest of its namespace.
				if got, _ := state.snapshotCell("_UNRELATED_ABSENT"); got.definition != configDependencyMacroUnknown || got.recorded {
					t.Fatalf("reserved fallback changed: %#v", got)
				}
			})
		}
	}
}

func TestConfigDependencyOptionalWriteDoesNotReadDefiniteWriteTargets(t *testing.T) {
	for _, directive := range []string{"define", "undef"} {
		for _, condition := range []string{"0", "1"} {
			t.Run(directive+"/"+condition, func(t *testing.T) {
				state := &configDependencyMacroState{snapshot: newConfigDependencyMacroSnapshot()}
				if !state.beginForcedHeaderTrace() {
					t.Fatal("cannot start trace")
				}
				configDependencyOptionalWriteForTest(t, state, condition, directive, "TARGET")
				if state.forcedHeaderTrace.reads.size() != 0 {
					t.Fatal("definite write gained an input requirement")
				}
				if state.forcedHeaderTouches.contains("TARGET") != (condition == "1") {
					t.Fatal("inactive write touched its target, or definite write lost its touch")
				}
				want := configDependencyMacroUndefined
				if condition == "1" && directive == "define" {
					want = configDependencyMacroDefined
				}
				if got, _ := state.snapshotCell("TARGET"); got.definition != want {
					t.Fatalf("definition = %d, want %d", got.definition, want)
				}
			})
		}
	}
}

func TestConfigDependencyOptionalWriteNoOpInvalidatesHeaderCache(t *testing.T) {
	for _, name := range []string{"TARGET", "_RESERVED_TARGET", configDependencyResolvedAutoconfGuard} {
		for _, directive := range []string{"define", "undef"} {
			t.Run(name+"/"+directive, func(t *testing.T) {
				fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
					"drivers/example/driver.c": "#include <common.h>\n",
					// No later condition reads the target: the optional write
					// itself must contribute the complete-cell requirement.
					"include/common.h": "#if defined(_OPTIONAL_SELECTOR)\n#" + directive + " " + name + "\n#endif\nCONFIG_COMMON\n",
				})
				written, other := configDependencyMacroDefined, configDependencyMacroUndefined
				if directive == "undef" {
					written, other = other, written
				}
				base := configDependencyMacroSnapshotCell{definition: written}
				type cacheInput struct {
					cell configDependencyMacroSnapshotCell
					hit  bool
				}
				cases := []cacheInput{{base, false}, {base, true}}
				if name == configDependencyResolvedAutoconfGuard {
					// Definedness is unchanged; recorded-bit-only changes must
					// still invalidate the complete input-cell requirement.
					cases = append(cases, cacheInput{configDependencyMacroSnapshotCell{definition: written, recorded: true}, false})
				}
				cases = append(cases, cacheInput{configDependencyMacroSnapshotCell{definition: other}, false}, cacheInput{base, true})
				cache := &configDependencyHeaderCache{}
				for index, test := range cases {
					beforeHits, beforeMisses := cache.hits, cache.misses
					result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
						if !state.putSnapshotCell(name, test.cell) {
							t.Fatal("cannot set input cell")
						}
						state.set(fmt.Sprintf("UNREAD_UNIT_%d", index), configDependencyMacroDefined)
					})
					if result.set.Opaque || !slices.Equal(result.set.Symbols, []string{"CONFIG_COMMON"}) {
						t.Fatalf("iteration %d closure = %#v", index, result.set)
					}
					want := configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown, recorded: name == configDependencyResolvedAutoconfGuard}
					if test.cell.definition == written {
						want.definition = written
					}
					if got := configDependencyHeaderCacheCellForTest(result, name); got != want {
						t.Fatalf("iteration %d result cell = %#v, want %#v", index, got, want)
					}
					if test.hit && (cache.hits != beforeHits+1 || cache.misses != beforeMisses) ||
						!test.hit && (cache.hits != beforeHits || cache.misses != beforeMisses+1) {
						t.Fatalf("iteration %d hit=%t, hits %d->%d, misses %d->%d", index, test.hit, beforeHits, cache.hits, beforeMisses, cache.misses)
					}
				}
			})
		}
	}
}

func TestConfigDependencyOptionalWriteNoOpInvalidatesForcedPrefix(t *testing.T) {
	for _, name := range []string{"TARGET", "_RESERVED_TARGET", configDependencyResolvedAutoconfGuard} {
		for _, directive := range []string{"define", "undef"} {
			t.Run(name+"/"+directive, func(t *testing.T) {
				written, other := configDependencyMacroDefined, configDependencyMacroUndefined
				if directive == "undef" {
					written, other = other, written
				}
				input, _ := newConfigDependencyMacroSnapshot().withCell(name, configDependencyMacroSnapshotCell{definition: written})
				root := &configDependencyMacroState{snapshot: input}
				entry := captureForcedHeaderMacroEntryForTest(t, root, func(state *configDependencyMacroState) {
					configDependencyOptionalWriteForTest(t, state, "defined(_OPTIONAL_SELECTOR)", directive, name)
				})
				unread, _ := input.withCell("UNREAD_UNIT", configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
				matching := &configDependencyMacroState{snapshot: unread}
				restored, valid := entry.restore(&configDependencyClosureScanner{}, matching)
				if !valid {
					t.Fatal("unchanged optional-write input did not reuse across an unread macro")
				}
				fresh := matching.branch()
				configDependencyOptionalWriteForTest(t, fresh, "defined(_OPTIONAL_SELECTOR)", directive, name)
				got, gotValid := restored.materializedSnapshot()
				want, wantValid := fresh.materializedSnapshot()
				if !gotValid || !wantValid {
					t.Fatal("invalid replay or fresh output")
				}
				requireConfigDependencyMacroEffectSnapshotsEqual(t, got, want)
				changed, _ := unread.withCell(name, configDependencyMacroSnapshotCell{definition: other})
				if _, valid := entry.restore(&configDependencyClosureScanner{}, &configDependencyMacroState{snapshot: changed}); valid {
					t.Fatal("changed optional-write input reused an idempotent cached write")
				}
				if name == configDependencyResolvedAutoconfGuard {
					changed, _ := unread.withCell(name, configDependencyMacroSnapshotCell{definition: written, recorded: true})
					if _, valid := entry.restore(&configDependencyClosureScanner{}, &configDependencyMacroState{snapshot: changed}); valid {
						t.Fatal("recorded-bit-only change reused an optional-write prefix")
					}
				}
			})
		}
	}
}

func TestConfigDependencyOptionalWriteRetainsOnlyProvenIncludePruning(t *testing.T) {
	for _, directive := range []string{"define", "undef"} {
		for _, initial := range []configDependencyMacroDefinition{configDependencyMacroUnknown, configDependencyMacroUndefined, configDependencyMacroDefined} {
			t.Run(fmt.Sprintf("%s/%d", directive, initial), func(t *testing.T) {
				fixture := newConfigDependencyHeaderCacheFixtureForTest(t, map[string]string{
					"drivers/example/driver.c": "#include <common.h>\n",
					"include/common.h":         "#if defined(_OPTIONAL_SELECTOR)\n#" + directive + " TARGET\n#endif\n#ifdef TARGET\n#include \"defined.h\"\n#else\n#include \"undefined.h\"\n#endif\n",
					"include/defined.h":        "CONFIG_DEFINED\n", "include/undefined.h": "CONFIG_UNDEFINED\n",
				})
				written := configDependencyMacroDefined
				if directive == "undef" {
					written = configDependencyMacroUndefined
				}
				want := []string{"CONFIG_DEFINED", "CONFIG_UNDEFINED"}
				if initial == written {
					if written == configDependencyMacroDefined {
						want = []string{"CONFIG_DEFINED"}
					} else {
						want = []string{"CONFIG_UNDEFINED"}
					}
				}
				cache := &configDependencyHeaderCache{}
				for iteration := 0; iteration < 2; iteration++ {
					result := fixture.compare(t, cache, "drivers/example/driver.c", func(_ *configDependencyClosureScanner, state *configDependencyMacroState) {
						state.set("TARGET", initial)
					})
					if result.set.Opaque || !slices.Equal(result.set.Symbols, want) {
						t.Fatalf("iteration %d closure = %#v, want symbols %q", iteration, result.set, want)
					}
				}
			})
		}
	}
}
