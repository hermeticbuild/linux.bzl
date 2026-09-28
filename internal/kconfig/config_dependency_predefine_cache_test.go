package kconfig

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func predefineCacheForTest(entries int) *configDependencyPredefineCache {
	return &configDependencyPredefineCache{
		maximumEntries: entries, maximumRecords: 1 << 20, maximumBytes: 1 << 28,
	}
}

func predefineCacheParseForTest(t *testing.T, contents string, calls bool) configDependencyCompilerPredefineParse {
	t.Helper()
	state, reason := parseConfigDependencyCompilerPredefinesWithReplacements(contents, calls)
	if reason != "" {
		t.Fatal(reason)
	}
	return configDependencyCompilerPredefineParse{state: state}
}

func TestConfigDependencyPredefineCacheExactAccounting(t *testing.T) {
	state := configDependencyMacroState{
		snapshot: newConfigDependencyMacroSnapshot(),
		symbols:  map[string]bool{"CONFIG_SYMBOL": true},
		macroExpansions: []configDependencyMacroExpansionDefinition{{
			name: "MACRO", identifiers: []string{"REF"}, safeTokenPastePrefixes: []string{"CONFIG_"},
		}},
		macroReplacements: map[string]configDependencyMacroReplacement{
			"MACRO": {text: "MACRO 1", origin: "dump:0"},
		},
	}
	state.set("__CELL", configDependencyMacroDefined)
	state.compilerPredefinedSnapshot = state.snapshot
	parsed := configDependencyCompilerPredefineParse{state: state}
	want := configDependencyPredefineCacheCost{records: 8}
	for _, text := range []string{"key", "dump", "__CELL", "CONFIG_SYMBOL", "MACRO", "REF", "CONFIG_", "MACRO", "MACRO 1", "dump:0"} {
		want.bytes += len(text)
	}
	got, fits := configDependencyPredefineCacheMeasure("key", "dump", parsed, want.records, want.bytes)
	if !fits || got != want {
		t.Fatalf("exact admission cost = %#v/%t, want %#v", got, fits, want)
	}
	for _, limits := range []configDependencyPredefineCacheCost{
		{records: want.records - 1, bytes: want.bytes},
		{records: want.records, bytes: want.bytes - 1},
		{},
	} {
		if _, fits := configDependencyPredefineCacheMeasure("key", "dump", parsed, limits.records, limits.bytes); fits {
			t.Fatalf("over-budget parse admitted at %#v", limits)
		}
	}
	// Counting does not ask the semantic namespace to build acceleration state.
	entries := 0
	state.snapshot.lookupMemo.Range(func(_, _ any) bool { entries++; return true })
	if entries != 0 || state.snapshot.projectionValid {
		t.Fatal("accounting populated a snapshot memo/projection")
	}
}

func TestConfigDependencyPredefineCacheFIFOAndBounds(t *testing.T) {
	parsed := predefineCacheParseForTest(t, "#define A 1\n", true)
	unit, fits := configDependencyPredefineCacheMeasure("a", "#define A 1\n", parsed, 1<<20, 1<<28)
	if !fits {
		t.Fatal("small fixture exceeds accounting budget")
	}
	for _, bound := range []string{"entries", "records", "bytes"} {
		t.Run(bound, func(t *testing.T) {
			cache := predefineCacheForTest(10)
			switch bound {
			case "entries":
				cache.maximumEntries = 2
			case "records":
				cache.maximumRecords = 2 * unit.records
			case "bytes":
				cache.maximumBytes = 2 * unit.bytes
			}
			cache.put("a", "#define A 1\n", parsed)
			cache.put("b", "#define A 1\n", parsed)
			if len(cache.entries) != 2 || cache.cost != (configDependencyPredefineCacheCost{records: 2 * unit.records, bytes: 2 * unit.bytes}) {
				t.Fatalf("exact two-entry boundary rejected: %#v", cache)
			}
			before := cache.cost
			for range 3 {
				if _, found := cache.get("a"); !found {
					t.Fatal("exact hit missed")
				}
				cache.put("a", "ignored duplicate", configDependencyCompilerPredefineParse{reason: "must not replace"})
			}
			if cache.cost != before || !slices.Equal(cache.order, []string{"a", "b"}) {
				t.Fatal("hits recharged or renewed FIFO order")
			}
			borrowed, _ := cache.get("a")
			branch := borrowed.state.branch()
			// Pointer aliases must see the same FIFO mutation.
			alias := cache
			alias.put("c", "#define A 1\n", parsed)
			if _, found := cache.get("a"); found || !slices.Equal(cache.order, []string{"b", "c"}) {
				t.Fatal("oldest key survived eviction after a hit")
			}
			branch.set("LOCAL", configDependencyMacroDefined)
			if branch.definition("A") != configDependencyMacroDefined ||
				borrowed.state.definition("LOCAL") != configDependencyMacroUndefined {
				t.Fatal("eviction invalidated or mutated an active borrower")
			}
			cache.evictOldest()
			cache.evictOldest()
			if len(cache.entries) != 0 || len(cache.order) != 0 || cache.cost != (configDependencyPredefineCacheCost{}) {
				t.Fatal("eviction retained ledger state")
			}
			for _, key := range cache.order[:cap(cache.order)] {
				if key != "" {
					t.Fatal("queue backing array retained an evicted key")
				}
			}
		})
	}
}

func TestConfigDependencyPredefineCacheOversizeAndDisabled(t *testing.T) {
	parsed := predefineCacheParseForTest(t, "#define A 1\n", false)
	cache := predefineCacheForTest(2)
	cache.maximumBytes = 1024
	cache.put("small", "#define A 1\n", parsed)
	cache.put(strings.Repeat("x", 1025), "", parsed)
	if len(cache.entries) != 1 || !slices.Equal(cache.order, []string{"small"}) {
		t.Fatal("oversized input evicted a useful entry")
	}
	for _, disabled := range []*configDependencyPredefineCache{
		nil, {}, {maximumEntries: 1, maximumRecords: 1 << 20},
		{maximumEntries: 1, maximumBytes: 1 << 20},
		{maximumRecords: 1 << 20, maximumBytes: 1 << 20},
	} {
		disabled.put("key", "#define A 1\n", parsed)
		if _, found := disabled.get("key"); found {
			t.Fatal("disabled cache retained a result")
		}
	}
	child := parsed.state.branch()
	cache.put("speculative", "", configDependencyCompilerPredefineParse{state: *child})
	intrinsic := predefineCacheParseForTest(t, "#define A 1\n", true)
	intrinsic.state.macroReplacements["A"] = configDependencyMacroReplacement{
		intrinsic: &configDependencyCompilerIntrinsicBinding{},
	}
	cache.put("context-bound", "", intrinsic)
	if len(cache.entries) != 1 {
		t.Fatal("non-initial state retained through a hidden parent/intrinsic reference")
	}
}

func TestConfigDependencyPredefineCacheKeysKeepDumpGuardAndMode(t *testing.T) {
	cache := predefineCacheForTest(16)
	dumps := []string{"#define ALIAS CONFIG_LEFT\n", "#define ALIAS CONFIG_RIGHT\n"}
	var keys []string
	for _, dump := range dumps {
		for _, answer := range []configDependencyMacroDefinition{
			configDependencyMacroUnknown, configDependencyMacroUndefined, configDependencyMacroDefined,
		} {
			definitions := map[string]bool{}
			if answer != configDependencyMacroUnknown {
				definitions["__QUERY"] = answer == configDependencyMacroDefined
			}
			identity := configDependencyCompilerDefinednessIdentity([]string{"__QUERY"}, definitions, true)
			for _, calls := range []bool{false, true} {
				key := configDependencyCompilerPredefineParseKey(dump, identity)
				if calls {
					key += "\x00complete-call-coverage"
				}
				parsed := predefineCacheParseForTest(t, dump, calls)
				var valid bool
				parsed.state, valid = configDependencyApplyCompilerDefinedness(parsed.state, definitions, identity)
				if !valid {
					t.Fatal("valid measured namespace rejected")
				}
				cache.put(key, dump, parsed)
				got, found := cache.get(key)
				if !found || got.state.definition("__QUERY") != answer ||
					(got.state.macroReplacements != nil) != calls ||
					!reflect.DeepEqual(got.state.symbols, parsed.state.symbols) {
					t.Fatal("dump, guard facts, or complete-call mode aliased")
				}
				keys = append(keys, key)
			}
		}
	}
	if len(cache.entries) != len(keys) {
		t.Fatal("distinct exact identities collided")
	}
}

func TestConfigDependencyPredefineCacheScannerParity(t *testing.T) {
	const path = "drivers/example/driver.c"
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		path: "#if defined(__QUERY)\nCONFIG_ON\n#else\nCONFIG_OFF\n#endif\n" +
			"#if 0\n#if defined(__UNANSWERED)\nCONFIG_MAYBE\n#endif\n#endif\n" +
			"#undef LOCAL\n#define LOCAL CONFIG_SOURCE\nLOCAL\n" +
			"#define PICK(x) CONFIG_ ## x\nint value = PICK(DRIVER);\n",
		"include/forced.h": "#ifndef __FORCED_H\n#define __FORCED_H\nCONFIG_FORCED\n#endif\n",
	}, []string{"-nostdinc", "-U__FORCED_H", "-DALIAS=CONFIG_ARG", "-D", "ORDER=CONFIG_BEFORE", "-UORDER",
		"-U", "ALIAS", "-DORDER=CONFIG_AFTER", "-include", "${tree:kernel}/include/forced.h",
		"-include", "${tree:prep}/include/generated/autoconf.h", "-c", path},
		map[string]string{"CONFIG_DRIVER": "7", "CONFIG_SOURCE": "11", "CONFIG_ARG": "3", "CONFIG_AFTER": "5"})
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.sourceGuardNamesReady = true
	plan.metadata.sourceGuardNames = []string{"__QUERY"}
	type scenario struct {
		dump       string
		defined    bool
		ready      bool
		queryError bool
	}
	scenarios := []scenario{
		{"#define SELECTED CONFIG_LEFT\n#define __QUERY 1\n", true, true, false},
		{"#define SELECTED CONFIG_RIGHT\n", false, true, false},
		{"#define SELECTED CONFIG_LEFT\n#define __QUERY 1\n", true, true, false},
		{"#define SELECTED CONFIG_LEFT\n#define __QUERY 1\n", false, true, false}, // contradiction
		{"#define SELECTED CONFIG_LEFT\n#define __QUERY 1\n", true, false, false},
		{"#define SELECTED CONFIG_LEFT\n#define __QUERY 1\n", true, true, true},
		{"not a compiler dump\n", false, true, false},
	}
	type result struct {
		sets         []ConfigDependencySet
		observations [][]ConfigDependencyCompilerGuardObservation
		requests     []configDependencyCompilerPredefineRequestKey
	}
	run := func(cache *configDependencyPredefineCache) result {
		shared := NewActionPlanConfigDependencySharedCache()
		if cache != nil {
			shared.compilerPredefines = cache
		}
		context := newConfigDependencyAnalysisContextWithCache(plan, shared)
		if cache == nil {
			context.compilerPredefines = nil // preserve former nil-map parameter behavior
		}
		alias := newConfigDependencyAnalysisContextWithCache(plan, shared)
		if cache != nil && alias.compilerPredefines != context.compilerPredefines {
			t.Fatal("shared contexts received copied cache ownership")
		}
		var result result
		for _, scenario := range scenarios {
			var observed []ConfigDependencyCompilerGuardObservation
			plan.metadata.SetCompilerGuardObserver(func(observation ConfigDependencyCompilerGuardObservation) error {
				observed = append(observed, observation)
				return nil
			})
			plan.metadata.compilerPredefines = func(scope, role, language string, args, units []string, env map[string]string) (string, bool, error) {
				result.requests = append(result.requests, configDependencyCompilerPredefineKey(scope, role, language, args, units, env))
				if scenario.queryError {
					return "", false, errors.New("current compiler receipt rejected")
				}
				return scenario.dump, scenario.ready, nil
			}
			plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
				if !slices.Equal(names, []string{"__QUERY"}) {
					t.Fatalf("unexpected measured names: %q", names)
				}
				return map[string]bool{"__QUERY": scenario.defined}, true, nil
			}
			context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
			set, err := analyzeActionPlanNodeConfigDependencies(plan, node, context)
			if err != nil {
				t.Fatal(err)
			}
			result.sets = append(result.sets, set)
			result.observations = append(result.observations, observed)
		}
		return result
	}
	want := run(predefineCacheForTest(1024))
	for name, cache := range map[string]*configDependencyPredefineCache{
		"nil": nil, "disabled": {}, "tiny": predefineCacheForTest(1), "default": newConfigDependencyPredefineCache(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := run(cache); !reflect.DeepEqual(got, want) {
				t.Fatalf("retention changed dependency sets, observer demands, or exact requests:\ngot %#v\nwant %#v", got, want)
			}
		})
	}
	for _, index := range []int{0, 1, 2} {
		if want.sets[index].Opaque || len(want.sets[index].Symbols) == 0 {
			t.Fatalf("valid fixture did not reach precise call coverage: %#v", want.sets[index])
		}
	}
	for _, index := range []int{3, 4, 5, 6} {
		if !want.sets[index].Opaque || len(want.sets[index].Symbols) != 0 ||
			len(want.sets[index].SourcePaths) != 0 || len(want.sets[index].ObjectPaths) != 0 {
			t.Fatalf("invalid/unavailable current result leaked a cached prefix: %#v", want.sets[index])
		}
	}
	if len(want.requests) != len(scenarios) {
		t.Fatalf("parsed memo suppressed current-context validation: %d requests", len(want.requests))
	}
}

func TestConfigDependencyPredefineCacheKeepsStrictProbeReceipts(t *testing.T) {
	opts := compilerDefinednessTestOptions(t)
	evaluate := func(cache *configDependencyPredefineCache, oracle *ProbeResultOracle) (*KbuildProbeEvaluation[[]ConfigDependencySet], error) {
		return EvaluateKbuildProbeWorkload(opts, oracle, func(scopes *KbuildProbeScopes) ([]ConfigDependencySet, error) {
			plan, node := configDependencyProbeDiscoveryFixture(t, "retained")
			if err := scopes.BindActionPlanToolsetPathCapabilities(plan.metadata); err != nil {
				return nil, err
			}
			shared := NewActionPlanConfigDependencySharedCache()
			shared.compilerPredefines = cache
			var result []ConfigDependencySet
			for range 3 {
				analysis, err := BuildActionPlanConfigDependencyAnalysisWithCache(plan, shared)
				if err != nil {
					return nil, err
				}
				sets, err := analysis.ByNodeID(plan)
				if err != nil {
					return nil, err
				}
				result = append(result, sets[node.ID])
			}
			return result, nil
		})
	}
	discovery, err := evaluate(predefineCacheForTest(1024), nil)
	if err != nil {
		t.Fatal(err)
	}
	oracle := successfulProbeOracleForFixedPointTest(t, discovery.Plan)
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if len(request.Steps) == 1 && request.Steps[0].Name == "compiler-definedness" {
			result := oracle.results[node.ID]
			result.Text, result.Steps[0].Stdout = "0\n", "0\n"
			oracle.results[node.ID] = result
		}
	}
	want, err := evaluate(predefineCacheForTest(1024), oracle)
	if err != nil {
		t.Fatal(err)
	}
	for _, cache := range []*configDependencyPredefineCache{{}, predefineCacheForTest(1), newConfigDependencyPredefineCache()} {
		got, err := evaluate(cache, oracle)
		if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got.Plan, discovery.Plan) {
			t.Fatalf("cache policy changed strict discovery/replay requests or results: %v", err)
		}
	}
	// A warmed parsed memo is not permission to accept a wrong current receipt.
	warm := newConfigDependencyPredefineCache()
	if _, err := evaluate(warm, oracle); err != nil || len(warm.entries) == 0 {
		t.Fatalf("failed to warm parsed memo before adversarial replay: %v", err)
	}
	bad := &ProbeResultOracle{results: maps.Clone(oracle.results), toolsets: maps.Clone(oracle.toolsets)}
	for id, result := range bad.results {
		result.RequestID = strings.Repeat("f", 64)
		bad.results[id] = result
		break
	}
	if _, err := evaluate(warm, bad); err == nil {
		t.Fatal("parsed-state memo bypassed a mismatched current probe request")
	}
}

func TestConfigDependencyPredefineCacheNumericDemandsAndObservedBytesParity(t *testing.T) {
	const source = "#include <selected.h>\n#define PICK(x) CONFIG_ ## x\n#if defined(__RESERVED)\nPICK(ON)\n#else\nPICK(OFF)\n#endif\n"
	plan, consumerID, producerID := prospectiveNumericDemandPlanForTest(t, source)
	type observation struct {
		replay  *ActionPlanFamilyVerifiedReplay
		headers *ActionPlanFamilyObservedHeaders
	}
	var observations []observation
	for _, contents := range []string{"#define NUMBER 7\n", "#define __RESERVED 1\n"} {
		replay, headers := observedDependencyGateForTest(t, plan, producerID, contents)
		observations = append(observations, observation{replay, headers})
	}
	type result struct {
		sets    []ConfigDependencySet
		demands []ConfigDependencyGeneratedHeaderDemandCollection
		uses    [][]ConfigDependencyObservedHeaderUse
	}
	run := func(cache *configDependencyPredefineCache) result {
		shared := NewActionPlanConfigDependencySharedCache()
		shared.compilerPredefines = cache
		var out result
		for index := range 4 {
			var analysis *ActionPlanConfigDependencyAnalysis
			var err error
			if index == 0 || index == 3 {
				analysis, err = BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, shared)
			} else {
				observed := observations[index-1]
				analysis, err = BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, shared, observed.replay, observed.headers)
			}
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil {
				t.Fatal(err)
			}
			demands, err := analysis.GeneratedHeaderDemandsByNodeID(plan)
			if err != nil {
				t.Fatal(err)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil {
				t.Fatal(err)
			}
			out.sets = append(out.sets, sets[consumerID])
			out.demands = append(out.demands, demands)
			out.uses = append(out.uses, uses)
		}
		return out
	}
	want := run(predefineCacheForTest(1024))
	for _, cache := range []*configDependencyPredefineCache{{}, predefineCacheForTest(1), newConfigDependencyPredefineCache()} {
		if got := run(cache); !reflect.DeepEqual(got, want) {
			t.Fatal("retention changed numeric demand tiers, rebound owners, precise uses or observed bytes")
		}
	}
	for _, index := range []int{0, 3} {
		if !want.sets[index].Opaque || len(want.demands[index].Demands) != 0 ||
			len(want.demands[index].prospective) != 1 || want.demands[index].prospectiveTruncated {
			t.Fatalf("ordinary replay borrowed observed bytes or lost numeric tier: %#v", want)
		}
	}
	for index, symbol := range []string{"CONFIG_OFF", "CONFIG_ON"} {
		if want.sets[index+1].Opaque || !slices.Equal(want.sets[index+1].Symbols, []string{symbol}) ||
			len(want.uses[index+1]) != 1 || want.uses[index+1][0].ProducerNodeID != producerID ||
			want.uses[index+1][0].ContentID != observations[index].headers.Headers()[0].ContentID {
			t.Fatalf("observed bytes lost current writer/content authority: %#v", want)
		}
	}
}
