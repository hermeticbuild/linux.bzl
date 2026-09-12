package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func configDependencyDefinednessProfileForTest(t *testing.T, roots map[string]string) CompactKbuildProfile {
	t.Helper()
	profile := mustCompactKbuildProfileForTest(t, "definedness", "scripts/Makefile.build", "", "obj-y += driver.o\n", nil)
	profile.evaluator.template.sourceRoots = maps.Clone(roots)
	return profile
}

func configDependencyDefinednessPopulateProfilesForTest(plan *ActionPlan) {
	plan.metadata.Config.KbuildProfiles = nil
	for _, profile := range plan.selectionGraph.profiles {
		plan.metadata.Config.KbuildProfiles = append(plan.metadata.Config.KbuildProfiles, profile)
	}
}

func configDependencyDefinednessHintPlanForTest(t *testing.T, names []string) *ActionPlan {
	t.Helper()
	profile := configDependencyDefinednessProfileForTest(t, nil)
	key := configDependencySourceIncludeCacheKeyForProfile(profile, "").bindings
	return &ActionPlan{metadata: &CompactMetadata{
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		sourceGuardInventory: &configDependencyGuardInventory{entries: map[string][]string{
			key: slices.Clone(names),
		}},
	}}
}

func TestConfigDependencyGuardInventoryDiscoversSortedHints(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "empty.h", "")
	mustWriteSource(t, root, "a.h", "#ifndef __Z_GUARD\n#define __Z_GUARD\n#ifdef _A_GUARD\n#endif\n#endif\n")
	mustWriteSource(t, root, "b.c", "# ifdef _B_GUARD /* comment */\n#endif\n%:ifndef _DIGRAPH_GUARD\n#endif\n#ifdef ORDINARY_GUARD\n#endif\n#if !defined(_NOT_AN_INVENTORY_HINT)\n#endif\n#ifdef _INVALID-NAME\n#endif\n")
	mustWriteSource(t, root, "splice.h", "#if\\\nndef _SPLICED_GUARD\n#endif\n#ifdef _A_GUARD\n#endif\n")
	mustWriteSource(t, root, "late.h", strings.Repeat(" ", configDependencyGuardInventoryPrefixBytes)+"\n#ifndef _TOO_LATE\n#endif\n")
	mustWriteSource(t, root, "incomplete-prefix.h", "#ifndef _INCOMPLETE_PREFIX\n/*"+strings.Repeat("x", configDependencyGuardInventoryPrefixBytes)+"*/\n#endif\n")
	outside := t.TempDir()
	mustWriteSource(t, outside, "outside.h", "#ifndef _OUTSIDE_SYMLINK\n#endif\n")
	for name, target := range map[string]string{"linked.h": filepath.Join(outside, "outside.h"), "linked-directory": outside} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	profile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": root})
	inventory := &configDependencyGuardInventory{}
	want := []string{"_A_GUARD", "_B_GUARD", "_DIGRAPH_GUARD", "_SPLICED_GUARD", "__Z_GUARD"}
	if got := inventory.names(profile); !slices.Equal(got, want) {
		t.Fatalf("inventory = %q, want %q", got, want)
	}
	alias := filepath.Join(t.TempDir(), "repository-root")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	aliasProfile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": alias})
	if got := inventory.names(aliasProfile); !slices.Equal(got, want) {
		t.Fatalf("repository-root symlink inventory = %q, want %q", got, want)
	}
	if got := inventory.names(CompactKbuildProfile{}); got != nil {
		t.Fatalf("missing template produced hints: %q", got)
	}
}

func BenchmarkConfigDependencyGuardInventory(b *testing.B) {
	root := b.TempDir()
	const count = 128
	for index := 0; index < count; index++ {
		contents := fmt.Sprintf("#ifndef _HEADER_%03d\n#define _HEADER_%03d\n#endif\n", index, index)
		contents += strings.Repeat(" ", configDependencyGuardInventoryPrefixBytes-len(contents))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("header-%03d.h", index)), []byte(contents), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	profile := CompactKbuildProfile{evaluator: &kbuildTargetEvaluator{template: &kbuildParser{
		sourceRoots: map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
	}}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		inventory := &configDependencyGuardInventory{}
		if got := inventory.names(profile); len(got) != count {
			b.Fatalf("inventory has %d names, want %d", len(got), count)
		}
	}
}

func TestConfigDependencyGuardInventoryIsolatesRootsAndShadows(t *testing.T) {
	first, second, object := t.TempDir(), t.TempDir(), t.TempDir()
	mustWriteSource(t, first, "include/shadow.h", "#ifndef _FIRST_ROOT\n#endif\n")
	mustWriteSource(t, second, "shadow.h", "#ifndef _SECOND_ROOT\n#endif\n")
	mustWriteSource(t, object, "object.h", "#ifndef _OBJECT_ONLY\n#endif\n")
	inventory := &configDependencyGuardInventory{}
	firstProfile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": first})
	secondProfile := configDependencyDefinednessProfileForTest(t, map[string]string{"__LINUX_BZL_SOURCE_TREE__": second})
	if got := inventory.names(firstProfile); !slices.Equal(got, []string{"_FIRST_ROOT"}) {
		t.Fatalf("first root = %q", got)
	}
	if got := inventory.names(secondProfile); !slices.Equal(got, []string{"_SECOND_ROOT"}) {
		t.Fatalf("same logical binding borrowed another physical root: %q", got)
	}
	shadowProfile := configDependencyDefinednessProfileForTest(t, map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":         first,
		"__LINUX_BZL_SOURCE_TREE__/include": second,
		"__LINUX_BZL_OBJECT_TREE__":         object,
	})
	// Shadowed source bytes may contribute extra query hints, but an object
	// binding cannot supply source evidence. Neither hint proves macro absence.
	if got := inventory.names(shadowProfile); !slices.Equal(got, []string{"_FIRST_ROOT", "_SECOND_ROOT"}) {
		t.Fatalf("shadow hint inventory = %q", got)
	}
	ambiguous := configDependencyDefinednessProfileForTest(t, map[string]string{
		"include": first, "__LINUX_BZL_SOURCE_TREE__/include": second,
	})
	if got := inventory.names(ambiguous); len(got) != 0 {
		t.Fatalf("ambiguous source binding supplied hints: %q", got)
	}
	plan := &ActionPlan{metadata: &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{secondProfile, firstProfile, firstProfile},
	}, sourceGuardInventory: inventory}}
	if got := configDependencyGuardNamesForPlan(plan); len(got) != 0 {
		t.Fatalf("legacy nil query callback enabled hints: %q", got)
	}
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
		return nil, false, nil
	}
	if got := configDependencyGuardNamesForPlan(plan); !slices.Equal(got, []string{"_FIRST_ROOT", "_SECOND_ROOT"}) {
		t.Fatalf("plan-wide stable union = %q", got)
	}
}

func TestActionPlanCompilerDefinednessRegistersEveryUnavailableChunk(t *testing.T) {
	names := make([]string, 300)
	for index := range names {
		names[index] = fmt.Sprintf("__CHUNK_%04d_", index) + strings.Repeat("X", 4096)
	}
	plan := configDependencyDefinednessHintPlanForTest(t, names)
	arguments, units := []string{"-nostdinc", "-DQUERY_MODE=1"}, []string{"unit.c"}
	environment := map[string]string{"QUERY_ENV": "exact"}
	var chunks [][]string
	plan.metadata.compilerDefinedness = func(scope, role, language string, gotArguments, gotUnits, chunk []string, gotEnvironment map[string]string) (map[string]bool, bool, error) {
		if scope != "host" || role != "cxx" || language != "c++" ||
			!slices.Equal(gotArguments, arguments) || !slices.Equal(gotUnits, units) || !maps.Equal(gotEnvironment, environment) {
			t.Fatal("definedness query changed the normalized compiler context")
		}
		bytes := 0
		for _, name := range chunk {
			bytes += len(name) + 31
		}
		if len(chunk) == 0 || bytes > MaxProbeInterpolatedBytes || !slices.IsSorted(chunk) {
			t.Fatal("invalid query chunk")
		}
		chunks = append(chunks, slices.Clone(chunk))
		// An unavailable result must not leak even a callback-supplied value.
		return map[string]bool{chunk[0]: true}, false, nil
	}
	definitions, identity, ready, err := actionPlanCompilerDefinedness(plan, "host", "cxx", "c++", arguments, units, environment)
	if err != nil || ready || len(definitions) != 0 || identity == "" || len(chunks) < 2 {
		t.Fatalf("unavailable query = %d definitions/%q/%t/%v, chunks=%d", len(definitions), identity, ready, err, len(chunks))
	}
	if got := slices.Concat(chunks...); !slices.Equal(got, names) {
		t.Fatal("discovery stopped before registering the full source inventory")
	}
}

func TestActionPlanCompilerDefinednessValidatesExactCallbackResults(t *testing.T) {
	names := []string{"__NEGATIVE", "__POSITIVE"}
	for _, test := range []struct {
		name      string
		values    map[string]bool
		available bool
		err       error
		wantError bool
		wantReady bool
	}{
		{"exact", map[string]bool{"__NEGATIVE": false, "__POSITIVE": true}, true, nil, false, true},
		{"empty", nil, true, nil, true, false},
		{"partial", map[string]bool{"__NEGATIVE": false}, true, nil, true, false},
		{"wrong_same_size", map[string]bool{"__NEGATIVE": false, "__UNREQUESTED": true}, true, nil, true, false},
		{"extra", map[string]bool{"__NEGATIVE": false, "__POSITIVE": true, "__EXTRA": false}, true, nil, true, false},
		{"unavailable", map[string]bool{"__NEGATIVE": false, "__POSITIVE": true}, false, nil, false, false},
		{"unsupported", nil, false, unsupportedCompilerPredefineProjection(errors.New("unsupported query")), false, true},
		{"runtime_error", nil, false, errors.New("query replay failed"), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := configDependencyDefinednessHintPlanForTest(t, names)
			plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, chunk []string, _ map[string]string) (map[string]bool, bool, error) {
				if !slices.Equal(chunk, names) {
					t.Fatalf("requested names = %q", chunk)
				}
				chunk[0] = "__CALLBACK_OWNED_SLICE"
				return test.values, test.available, test.err
			}
			got, identity, ready, err := actionPlanCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
			if (err != nil) != test.wantError || ready != test.wantReady {
				t.Fatalf("query = %#v/%q/%t/%v", got, identity, ready, err)
			}
			if test.name == "exact" {
				if !maps.Equal(got, test.values) {
					t.Fatalf("exact answer = %#v", got)
				}
				test.values["__NEGATIVE"] = true
				if got["__NEGATIVE"] {
					t.Fatal("result retained the callback-owned map")
				}
			} else if !test.wantError && len(got) != 0 {
				t.Fatal("unavailable/unsupported callback supplied usable facts")
			}
			if gotNames := configDependencyGuardNamesForPlan(plan); !slices.Equal(gotNames, names) {
				t.Fatal("callback mutated the cached inventory")
			}
		})
	}
}

func TestActionPlanCompilerPredefinesDefinednessReadinessAndLegacy(t *testing.T) {
	for _, dumpReady := range []bool{false, true} {
		for _, guardsReady := range []bool{false, true} {
			t.Run(fmt.Sprintf("dump_%t_guards_%t", dumpReady, guardsReady), func(t *testing.T) {
				plan := configDependencyDefinednessHintPlanForTest(t, []string{"__QUERY"})
				dumpCalls, guardCalls := 0, 0
				plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
					dumpCalls++
					return "#define DUMPED 1\n", dumpReady, nil
				}
				plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
					guardCalls++
					return map[string]bool{names[0]: false}, guardsReady, nil
				}
				cache := map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
				result, err := actionPlanCompilerPredefines(plan, cache, "target", "cc", "c", nil, nil, nil)
				if err != nil || result.ready != (dumpReady && guardsReady) || dumpCalls != 1 || guardCalls != 1 {
					t.Fatalf("typed result = %#v/%v, calls=%d/%d", result, err, dumpCalls, guardCalls)
				}
				if _, err := actionPlanCompilerPredefines(plan, cache, "target", "cc", "c", nil, nil, nil); err != nil || dumpCalls != 1 || guardCalls != 1 {
					t.Fatal("exact request memo did not retain the complete typed result")
				}
			})
		}
	}
	plan := configDependencyDefinednessHintPlanForTest(t, []string{"__UNQUERIED"})
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	result, err := actionPlanCompilerPredefines(plan, nil, "target", "cc", "c", nil, nil, nil)
	if err != nil || !result.ready || result.guardIdentity != "" || result.guardDefinitions != nil {
		t.Fatalf("legacy nil callback result = %#v/%v", result, err)
	}
}

func TestConfigDependencyCompilerDefinednessDerivedStateAndIdentity(t *testing.T) {
	original, reason := parseConfigDependencyCompilerPredefines("#define __DUMPED 1\n#define ORDINARY 1\n")
	if reason != "" {
		t.Fatal(reason)
	}
	before, digest := original.snapshot, original.compilerPredefinedDigest
	names := []string{"__NEGATIVE", "__POSITIVE", configDependencyResolvedAutoconfGuard}
	slices.Sort(names)
	definitions := map[string]bool{"__NEGATIVE": false, "__POSITIVE": true, configDependencyResolvedAutoconfGuard: false}
	identity := configDependencyCompilerDefinednessIdentity(names, definitions, true)
	derived, valid := configDependencyApplyCompilerDefinedness(original, definitions, identity)
	if !valid || derived.snapshot == before || derived.compilerPredefinedSnapshot != derived.snapshot || derived.compilerPredefinedDigest == digest {
		t.Fatal("definedness facts did not create a separately witnessed initial state")
	}
	requireConfigDependencyMacroSnapshotCell(t, derived.snapshot, "__NEGATIVE", configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined})
	requireConfigDependencyMacroSnapshotCell(t, derived.snapshot, "__POSITIVE", configDependencyMacroSnapshotCell{definition: configDependencyMacroDefined})
	requireConfigDependencyMacroSnapshotCell(t, derived.snapshot, "__UNQUERIED", configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown})
	requireConfigDependencyMacroSnapshotCell(t, derived.snapshot, configDependencyResolvedAutoconfGuard, configDependencyMacroSnapshotCell{definition: configDependencyMacroUndefined})
	if original.snapshot != before || original.compilerPredefinedDigest != digest || original.compilerPredefinedSnapshot != before {
		t.Fatal("derived state mutated the cached -dM root")
	}
	requireConfigDependencyMacroSnapshotCell(t, before, "__NEGATIVE", configDependencyMacroSnapshotCell{definition: configDependencyMacroUnknown})
	explicit := derived.branch()
	explicit.set(configDependencyResolvedAutoconfGuard, configDependencyMacroUndefined)
	if value, recorded := explicit.explicitDefinition(configDependencyResolvedAutoconfGuard); value != configDependencyMacroUndefined || !recorded {
		t.Fatal("explicit #undef lost its provenance distinction from queried absence")
	}
	contradictory := map[string]bool{"__DUMPED": false}
	if _, valid := configDependencyApplyCompilerDefinedness(original, contradictory,
		configDependencyCompilerDefinednessIdentity([]string{"__DUMPED"}, contradictory, true)); valid {
		t.Fatal("negative query contradicted a positive compiler dump")
	}
	for _, invalid := range []string{"", "__BAD-NAME", "__BAD trailing"} {
		if _, valid := configDependencyApplyCompilerDefinedness(original, map[string]bool{invalid: false}, "measured"); valid {
			t.Fatalf("invalid query name %q entered the initial state", invalid)
		}
	}
	identities := map[string]bool{}
	for _, values := range []map[string]bool{nil, {"__QUERY": false}, {"__QUERY": true}} {
		for _, ready := range []bool{false, true} {
			identity := configDependencyCompilerDefinednessIdentity([]string{"__QUERY"}, values, ready)
			if identities[identity] {
				t.Fatal("missing/negative/positive/readiness states share an identity")
			}
			identities[identity] = true
		}
	}
	legacy, valid := configDependencyApplyCompilerDefinedness(original, nil, "")
	if !valid || legacy.snapshot != before || legacy.compilerPredefinedDigest != digest {
		t.Fatal("legacy nil query changed the initial macro state")
	}
}

func TestConfigDependencyCompilerDefinednessForcedPrefixAnswersRemainCurrent(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifdef __QUERY_UNREAD\n#include <linux/tu-on.h>\n#else\n#include <linux/tu-off.h>\n#endif\n",
		"include/linux/forced.h":   "#ifdef __QUERY_SWITCH\n#include <linux/prefix-on.h>\n#else\n#include <linux/prefix-off.h>\n#endif\n",
		"include/linux/tu-on.h":    "CONFIG_TU_ON\n", "include/linux/tu-off.h": "CONFIG_TU_OFF\n",
		"include/linux/prefix-on.h": "CONFIG_PREFIX_ON\n", "include/linux/prefix-off.h": "CONFIG_PREFIX_OFF\n",
	}, []string{"-nostdinc", "-I", "${tree:kernel}/include", "-include", "${tree:kernel}/include/linux/forced.h", "-c", "drivers/example/driver.c"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	configDependencyDefinednessPopulateProfilesForTest(plan)
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) { return "", true, nil }
	var answers map[string]bool
	available := true
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
		if !slices.Equal(names, []string{"__QUERY_SWITCH", "__QUERY_UNREAD"}) {
			t.Fatalf("source-discovered guards = %q", names)
		}
		return maps.Clone(answers), available, nil
	}
	context := newConfigDependencyAnalysisContext(plan)
	for index, values := range []map[string]bool{
		{"__QUERY_SWITCH": false, "__QUERY_UNREAD": false},
		{"__QUERY_SWITCH": true, "__QUERY_UNREAD": false},
		{"__QUERY_SWITCH": false, "__QUERY_UNREAD": true},
	} {
		answers = values
		context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
		set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
		wantPrefix, wantTU := "CONFIG_PREFIX_OFF", "CONFIG_TU_OFF"
		if values["__QUERY_SWITCH"] {
			wantPrefix = "CONFIG_PREFIX_ON"
		}
		if values["__QUERY_UNREAD"] {
			wantTU = "CONFIG_TU_ON"
		}
		want := []string{wantPrefix, wantTU}
		slices.Sort(want)
		if err != nil || set.Opaque || !slices.Equal(set.Symbols, want) {
			t.Fatalf("query variant %d = %#v/%v, want %q", index, set, err, want)
		}
	}
	if context.forcedHeaders.hits != 1 || context.forcedHeaders.misses != 2 || len(context.compilerPredefines.entries) != 3 {
		t.Fatalf("forced cache hits/misses/parses = %d/%d/%d, want 1/2/3", context.forcedHeaders.hits, context.forcedHeaders.misses, len(context.compilerPredefines.entries))
	}
	available = false
	context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
	set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil || !set.Opaque || !strings.Contains(set.Reason, "unavailable during discovery") {
		t.Fatalf("unavailable answers supplied usable initial state: %#v/%v", set, err)
	}
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
		return map[string]bool{"__QUERY_SWITCH": false, "__QUERY_UNREAD": false}, true,
			unsupportedCompilerPredefineProjection(errors.New("unrepresentable query"))
	}
	context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
	set, err = analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_PREFIX_OFF", "CONFIG_PREFIX_ON", "CONFIG_TU_OFF", "CONFIG_TU_ON"}) {
		t.Fatalf("unsupported query inferred negative facts: %#v/%v", set, err)
	}
	plan.metadata.compilerDefinedness = nil
	context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
	set, err = analyzeActionPlanNodeConfigDependencies(plan, node, context)
	if err != nil || set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_PREFIX_OFF", "CONFIG_PREFIX_ON", "CONFIG_TU_OFF", "CONFIG_TU_ON"}) {
		t.Fatalf("legacy unqueried guards did not retain branch union: %#v/%v", set, err)
	}
}

func TestConfigDependencyCompilerDefinednessWitnessRequiresExactReadyRequest(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#ifdef __QUERY_WITNESS\nCONFIG_QUERY\n#endif\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	configDependencyDefinednessPopulateProfilesForTest(plan)
	plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
		t.Fatal("witness lookup must not register a new compiler query")
		return nil, false, nil
	}
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, plan.Recipes[node.Recipe])
	if reason != "" {
		t.Fatal(reason)
	}
	sources, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	probeInvocation := invocation
	if invocation.hasPredefineProjection {
		probeInvocation.arguments = invocation.predefineArguments
		probeInvocation.kbuildStart = invocation.predefineKbuildStart
		probeInvocation.kbuildEnd = invocation.predefineKbuildEnd
		probeInvocation.probeEnvironment = invocation.predefineProbeEnvironment
	}
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(probeInvocation, sources)
	if reason != "" {
		t.Fatal(reason)
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	context := newConfigDependencyAnalysisContext(plan)
	for _, test := range []struct {
		name   string
		result *configDependencyCompilerPredefineRequestResult
		valid  bool
	}{
		{"missing", nil, false},
		{"unavailable", &configDependencyCompilerPredefineRequestResult{guardIdentity: "exact", ready: false}, false},
		{"legacy_without_identity", &configDependencyCompilerPredefineRequestResult{ready: true}, false},
		{"ready", &configDependencyCompilerPredefineRequestResult{guardIdentity: "exact", ready: true}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
			if test.result != nil {
				context.compilerPredefineRequests[key] = *test.result
			}
			identity, valid := configDependencyCompilerDefinednessWitness(plan, node, invocation, context)
			if valid != test.valid || valid && identity != "exact" {
				t.Fatalf("request witness = %q/%t, want valid %t", identity, valid, test.valid)
			}
		})
	}
	// A registered sibling request cannot witness changed arguments, language,
	// environment, or scope, even when it queried exactly the same guard names.
	for _, test := range []struct {
		name, scope, language string
		arguments             []string
		environment           map[string]string
	}{
		{"arguments", actionPlanConfigDependencyScope(node), probe.language, append(slices.Clone(probe.arguments), "-DOTHER=1"), probe.environment},
		{"language", actionPlanConfigDependencyScope(node), "c++", probe.arguments, probe.environment},
		{"scope", "host", probe.language, probe.arguments, probe.environment},
		{"environment", actionPlanConfigDependencyScope(node), probe.language, probe.arguments, map[string]string{"OTHER": "1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			otherKey := configDependencyCompilerPredefineKey(test.scope, invocation.tool,
				test.language, test.arguments, probe.translationUnits, test.environment)
			if otherKey == key {
				t.Fatal("fixture did not change the request context")
			}
			context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{
				otherKey: {guardIdentity: "sibling", ready: true},
			}
			if _, valid := configDependencyCompilerDefinednessWitness(plan, node, invocation, context); valid {
				t.Fatal("sibling compiler request supplied the guard witness")
			}
		})
	}
	plan.metadata.compilerDefinedness = nil
	context.compilerPredefineRequests = nil
	if identity, valid := configDependencyCompilerDefinednessWitness(plan, node, invocation, context); !valid || identity != "" {
		t.Fatalf("legacy nil callback requires a new guard witness: %q/%t", identity, valid)
	}
}

func TestConfigDependencyCompilerDefinednessCompletedCacheTracksAnswers(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#ifdef __QUERY_CACHE\n#include \"enabled.h\"\n#else\n#include \"disabled.h\"\n#endif\n")
	mustWriteSource(t, root, "drivers/example/enabled.h", "CONFIG_ENABLED\n")
	mustWriteSource(t, root, "drivers/example/disabled.h", "CONFIG_DISABLED\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index, answer := range []bool{false, true, false} {
		plan, node := configDependencyCompletedCachePlanForTest(t, root, "definedness-profile", fmt.Sprintf("definedness-%d", index), "src-00000001", "image", "target-toolset", map[string]string{})
		configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
		configDependencyDefinednessPopulateProfilesForTest(plan)
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) { return "", true, nil }
		plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
			if !slices.Equal(names, []string{"__QUERY_CACHE"}) {
				t.Fatalf("query names = %q", names)
			}
			return map[string]bool{"__QUERY_CACHE": answer}, true, nil
		}
		plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{node.ID: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[node.Recipe].Arguments)}}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		want := "CONFIG_DISABLED"
		if answer {
			want = "CONFIG_ENABLED"
		}
		if set.Opaque || !slices.Equal(set.Symbols, []string{want}) {
			t.Fatalf("completed query variant %d = %#v, want %s", index, set, want)
		}
		if index < 2 && cache.configDependencies.completed.hits != 0 {
			t.Fatal("changed compiler query answer reused an old completed closure")
		}
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 2 {
		t.Fatalf("completed cache hits/stores = %d/%d, want 1/2", cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}
