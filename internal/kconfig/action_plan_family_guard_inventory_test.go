package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func familyGuardMetadataForTest(roots map[string]string) *CompactMetadata {
	return &CompactMetadata{
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{{
			evaluator: &kbuildTargetEvaluator{template: &kbuildParser{sourceRoots: maps.Clone(roots)}},
		}}},
		compilerDefinedness: func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
			values := make(map[string]bool, len(names))
			for _, name := range names {
				values[name] = false
			}
			return values, true, nil
		},
	}
}

func TestFamilyGuardInventoryAttachmentOwnership(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(fmt.Sprintf("zero_%t", zero), func(t *testing.T) {
			cache := NewActionPlanFamilyPlanningCache()
			if zero {
				cache = &ActionPlanFamilyPlanningCache{}
			}
			var shared *configDependencyGuardInventory
			for range 4 {
				metadata := familyGuardMetadataForTest(nil)
				metadata.sourceGuardInventory = &configDependencyGuardInventory{}
				metadata.sourceGuardNames, metadata.sourceGuardNamesReady = []string{"_LOCAL_UNION"}, true
				plan := &ActionPlan{metadata: metadata}
				plan.attachFamilyPlanningCache(cache)
				if shared == nil {
					shared = metadata.sourceGuardInventory
				}
				if shared == nil || metadata.sourceGuardInventory != shared {
					t.Fatal("fresh family metadata did not retain the shared inventory")
				}
				cache.initialize()
				plan.attachFamilyPlanningCache(cache)
				if metadata.sourceGuardInventory != shared || !metadata.sourceGuardNamesReady ||
					!slices.Equal(metadata.sourceGuardNames, []string{"_LOCAL_UNION"}) || metadata.compilerDefinedness == nil {
					t.Fatal("reattachment replaced inventory, local union or callback")
				}
			}
			// Plans without metadata still use the existing persistent-store API.
			(&ActionPlan{}).attachFamilyPlanningCache(cache)
			var absent *ActionPlan
			absent.attachFamilyPlanningCache(cache)
		})
	}
	metadata := familyGuardMetadataForTest(nil)
	local := &configDependencyGuardInventory{}
	metadata.sourceGuardInventory = local
	(&ActionPlan{metadata: metadata}).attachFamilyPlanningCache(nil)
	if metadata.sourceGuardInventory != local {
		t.Fatal("non-family attachment changed the local inventory")
	}
}

func TestFamilyGuardInventoryFourFreshMetadataAndBindings(t *testing.T) {
	root, shadow, object := t.TempDir(), t.TempDir(), t.TempDir()
	mustWriteSource(t, root, "source.h", "#ifndef _SOURCE\n#endif\n")
	mustWriteSource(t, shadow, "shadow.h", "#ifndef _SHADOW\n#endif\n")
	mustWriteSource(t, object, "include/generated/autoconf.h", "#ifndef _GENERATED_CONFIG\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	var inventory *configDependencyGuardInventory
	for index, test := range []struct {
		roots map[string]string
		want  []string
	}{
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}, []string{"_SOURCE"}},
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": root, "__LINUX_BZL_OBJECT_TREE__": object}, []string{"_SOURCE"}},
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}, []string{"_SOURCE"}},
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}, []string{"_SOURCE"}},
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": shadow}, []string{"_SHADOW"}},
		{map[string]string{"__LINUX_BZL_SOURCE_TREE__": root, "__LINUX_BZL_SOURCE_TREE__/include": shadow}, []string{"_SHADOW", "_SOURCE"}},
		{map[string]string{"include": root, "__LINUX_BZL_SOURCE_TREE__/include": shadow}, nil},
	} {
		plan := &ActionPlan{metadata: familyGuardMetadataForTest(test.roots)}
		plan.attachFamilyPlanningCache(cache)
		if index == 0 {
			inventory = plan.metadata.sourceGuardInventory
		}
		if inventory == nil || plan.metadata.sourceGuardInventory != inventory {
			t.Fatalf("metadata %d has a separate inventory", index)
		}
		if got := configDependencyGuardNamesForPlan(plan); !slices.Equal(got, test.want) {
			t.Fatalf("metadata %d union = %q, want %q", index, got, test.want)
		}
		if index < 4 && len(inventory.entries) != 1 {
			t.Fatalf("four equal source bindings retained %d inventory entries", len(inventory.entries))
		}
	}
	if len(inventory.entries) != 4 {
		t.Fatalf("distinct root/shadow/ambiguous keys = %d, want 4", len(inventory.entries))
	}
	// A different immutable-input lifetime requires a fresh family cache, even
	// when a subsequent invocation reuses the same physical pathname.
	mustWriteSource(t, root, "source.h", "#ifndef _NEXT_INVOCATION\n#endif\n")
	fresh := &ActionPlan{metadata: familyGuardMetadataForTest(map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})}
	fresh.attachFamilyPlanningCache(NewActionPlanFamilyPlanningCache())
	if got := configDependencyGuardNamesForPlan(fresh); !slices.Equal(got, []string{"_NEXT_INVOCATION"}) {
		t.Fatalf("fresh input lifetime reused stale inventory: %q", got)
	}
}

func TestFamilyGuardInventoryDoesNotShareCompilerAnswers(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "guard.h", "#ifndef _QUERY\n#endif\n")
	cache := NewActionPlanFamilyPlanningCache()
	identities := map[string]bool{}
	for index := range 4 {
		metadata := familyGuardMetadataForTest(map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})
		scope, role, language := "target", "cc", "c"
		if index%2 != 0 {
			scope, role, language = "host", "cxx", "c++"
		}
		args, units := []string{fmt.Sprintf("-DVARIANT=%d", index)}, []string{"unit.c"}
		environment := map[string]string{"VARIANT": fmt.Sprint(index)}
		calls := 0
		metadata.compilerDefinedness = func(gotScope, gotRole, gotLanguage string, gotArgs, gotUnits, names []string, gotEnvironment map[string]string) (map[string]bool, bool, error) {
			calls++
			if gotScope != scope || gotRole != role || gotLanguage != language || !slices.Equal(gotArgs, args) ||
				!slices.Equal(gotUnits, units) || !maps.Equal(gotEnvironment, environment) || !slices.Equal(names, []string{"_QUERY"}) {
				t.Fatal("shared inventory changed the compiler-sensitive query")
			}
			names[0] = "_CALLBACK_MUTATION"
			return map[string]bool{"_QUERY": index%2 == 0}, index < 2, nil
		}
		plan := &ActionPlan{metadata: metadata}
		plan.attachFamilyPlanningCache(cache)
		values, identity, ready, err := actionPlanCompilerDefinedness(plan, scope, role, language, args, units, environment)
		if err != nil || calls != 1 || ready != (index < 2) {
			t.Fatalf("variant %d query = %v/%q/%t/%v, calls %d", index, values, identity, ready, err, calls)
		}
		if ready && (len(values) != 1 || values["_QUERY"] != (index%2 == 0)) || !ready && len(values) != 0 {
			t.Fatalf("variant %d borrowed another variant's facts: %v", index, values)
		}
		identities[identity] = true
		if got := configDependencyGuardNamesForPlan(plan); !slices.Equal(got, []string{"_QUERY"}) {
			t.Fatalf("callback mutated shared names: %q", got)
		}
	}
	if len(identities) != 3 {
		t.Fatalf("true/false/unavailable witnesses collapsed: %d", len(identities))
	}
	plan := &ActionPlan{metadata: familyGuardMetadataForTest(map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})}
	plan.metadata.compilerDefinedness = nil
	plan.attachFamilyPlanningCache(cache)
	if configDependencyGuardNamesForPlan(plan) != nil {
		t.Fatal("shared names enabled a missing compiler callback")
	}
}

func TestFamilyGuardInventorySerializedPlanParity(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/variant.c", "#include <variant.h>\nCONFIG_USED\n")
	mustWriteSource(t, root, "query.h", "#ifndef _QUERY\n#endif\n")
	shared := NewActionPlanFamilyPlanningCache()
	for variant := range 4 {
		var want []byte
		for _, useShared := range []bool{false, true} {
			metadata := familyVariantMetadataForTest(t, nil)
			for _, profile := range metadata.Config.KbuildProfiles {
				profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
			}
			metadata.configFragment["CONFIG_OTHER"] = fmt.Sprint(variant % 2)
			calls := 0
			metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
				calls++
				if !slices.Equal(names, []string{"_QUERY"}) {
					t.Fatalf("unexpected hint vector: %q", names)
				}
				return map[string]bool{"_QUERY": variant%2 == 0}, true, nil
			}
			cache := NewActionPlanFamilyPlanningCache()
			if useShared {
				cache = shared
			}
			plan, dependencies, err := metadata.ActionPlanWithFamilyPlanningCache(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, cache)
			if err != nil {
				t.Fatal(err)
			}
			if calls == 0 || metadata.sourceGuardInventory == nil || len(metadata.sourceGuardInventory.entries) != 1 {
				t.Fatal("lowering did not query the attached inventory")
			}
			canonical, err := canonicalFamilyReplayPlan(plan, familyTestConfig("1", fmt.Sprint(variant%2)))
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(struct {
				Plan         json.RawMessage
				Dependencies map[string]ConfigDependencySet
			}{canonical, dependencies})
			if err != nil {
				t.Fatal(err)
			}
			if !useShared {
				want = data
			} else if !bytes.Equal(data, want) {
				t.Fatalf("variant %d shared inventory changed canonical plan/dependencies", variant)
			}
		}
	}
}

func TestFamilyGuardInventoryProbeRequestParity(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "query.h", "#ifndef _QUERY\n#endif\n")
	fixtures := linuxCompilerBootstrapFixtures(t)
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixtures[1])}
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	options.Host = &host
	shared := NewActionPlanFamilyPlanningCache()
	for variant := range 4 {
		var want []byte
		for _, useShared := range []bool{false, true} {
			cache := NewActionPlanFamilyPlanningCache()
			if useShared {
				cache = shared
			}
			evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (int, error) {
				metadata := familyGuardMetadataForTest(map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})
				if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
					return 0, err
				}
				plan := &ActionPlan{metadata: metadata}
				plan.attachFamilyPlanningCache(cache)
				for _, scope := range []string{"target", "host"} {
					_, _, _, err := actionPlanCompilerDefinedness(plan, scope, "cc", "c",
						[]string{"-nostdinc", fmt.Sprintf("-DVARIANT=%d", variant)}, nil, map[string]string{"QUERY_ENV": "exact"})
					if err != nil {
						return 0, err
					}
				}
				return 0, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(evaluation.Plan.Nodes) != 2 {
				t.Fatalf("probe graph has %d nodes, want two distinct scopes", len(evaluation.Plan.Nodes))
			}
			data, err := json.Marshal(evaluation.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if !useShared {
				want = data
			} else if !bytes.Equal(data, want) {
				t.Fatalf("variant %d shared inventory changed serialized probe graph", variant)
			}
		}
	}
}

func BenchmarkFamilyGuardInventoryFourVariants(b *testing.B) {
	root := b.TempDir()
	const files = 128
	for index := range files {
		data := fmt.Sprintf("#ifndef _HEADER_%03d\n#define _HEADER_%03d\n#endif\n", index, index)
		data += strings.Repeat(" ", configDependencyGuardInventoryPrefixBytes-len(data))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("header-%03d.h", index)), []byte(data), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for _, shared := range []bool{false, true} {
		b.Run(fmt.Sprintf("shared_%t", shared), func(b *testing.B) {
			entries := 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				cache := NewActionPlanFamilyPlanningCache()
				inventories := map[*configDependencyGuardInventory]bool{}
				for range 4 {
					if !shared {
						cache = NewActionPlanFamilyPlanningCache()
					}
					plan := &ActionPlan{metadata: familyGuardMetadataForTest(map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})}
					plan.attachFamilyPlanningCache(cache)
					if got := configDependencyGuardNamesForPlan(plan); len(got) != files {
						b.Fatalf("inventory has %d names, want %d", len(got), files)
					}
					inventories[plan.metadata.sourceGuardInventory] = true
				}
				// Count retained entries once per distinct owner, not four pointer
				// references. This is not instrumentation of filesystem scans.
				for inventory := range inventories {
					entries += len(inventory.entries)
				}
			}
			b.StopTimer()
			want := 4
			if shared {
				want = 1
			}
			if entries != want*b.N {
				b.Fatalf("inventory entries %d, want %d", entries, want*b.N)
			}
			b.ReportMetric(float64(entries)/float64(b.N), "inventory_entries/op")
		})
	}
}
