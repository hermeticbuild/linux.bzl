package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func observedDependencyPlanForTest(t *testing.T, mode, source string) (*ActionPlan, string, string) {
	t.Helper()
	plan, node, _ := generatedHeaderDemandPlanForTest(t, mode)
	profile := plan.selectionGraph.profiles["config-dependency"]
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, "drivers/example/driver.c", source)
	// Give the real snapshot/replay gate complete canonical recipe contracts.
	recipes := map[string]ActionRecipe{}
	for index := range plan.Nodes {
		current := &plan.Nodes[index]
		current.Product = "vmlinux"
		recipe := cloneActionRecipe(plan.Recipes[current.Recipe])
		recipe.Sources = nil
		for ordinal, edge := range current.Sources {
			recipe.Sources = append(recipe.Sources, edge.Role+":"+planOrdinal(ordinal))
		}
		recipe.Outputs = nil
		for slot := range current.Outputs {
			recipe.Outputs = append(recipe.Outputs, planOrdinal(slot))
		}
		if current.ID == node.ID {
			current.Trees, recipe.Trees = []string{"prep"}, []string{"prep"}
			recipe.Arguments = []string{"-nostdinc", "-I${tree:prep}/include/generated",
				"-c", "${source:source:00000000}", "-o", "${output:00000000}"}
		} else {
			// generatedHeaderDemandPlanForTest deliberately uses an unmodeled
			// generator. Give its ordinary slot a valid stdout contract without
			// teaching dependency analysis what that arbitrary command writes.
			recipe.Stdout = "00000000"
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		current.Recipe = id
		recipes[id] = recipe
	}
	plan.Recipes = recipes
	plan.Toolsets = map[string]string{"target": "sha256-" + strings.Repeat("1", 64), "host": "sha256-" + strings.Repeat("2", 64)}
	// Exercise the execution-probed conditional scanner, including its ordered
	// include failures and real header caches, rather than the literal fallback.
	ref := KbuildActionRoleRef{Scope: actionPlanConfigDependencyScope(node), Role: node.Tool}
	if !slices.Contains(plan.metadata.actionRoles, ref) {
		plan.metadata.actionRoles = append(plan.metadata.actionRoles, ref)
	}
	if plan.metadata.actionContracts == nil {
		plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{}
	}
	plan.metadata.actionContracts[ref] = CompactKbuildActionContract{}
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", true, nil
	}
	producer := "selected-header"
	if mode == "tree" {
		producer = "tree-header"
	}
	// Real provisional producers already have digest-shaped IDs. The older
	// scanner fixture uses readable names, which cannot cross the strict
	// serialized input-set import inside the replay gate.
	const producerDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		t.Fatal(err)
	}
	for index := range plan.Nodes {
		current := &plan.Nodes[index]
		if current.ID == producer {
			current.ID = producerDigest
		}
		for index := range current.Inputs {
			if current.Inputs[index].ProducerID == producer {
				current.Inputs[index].ProducerID = producerDigest
			}
		}
		current.InputSet, err = store.Map(current.InputSet, func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
			if entry.ProducerID == producer {
				entry.ProducerID = producerDigest
			}
			return entry, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for selection, id := range plan.selectionGraph.materializedProducers {
		if id == producer {
			plan.selectionGraph.materializedProducers[selection] = producerDigest
		}
	}
	plan.invalidateLookupIndexes()
	producer = producerDigest
	return plan, node.ID, producer
}

func observedDependencyGateForTest(t *testing.T, plan *ActionPlan, rootProducer string, contents string) (*ActionPlanFamilyVerifiedReplay, *ActionPlanFamilyObservedHeaders) {
	t.Helper()
	// Fixture insertion uses the private persistent store. Snapshot cloning
	// intentionally copies only exported nodes, just like the public transport.
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	addressed := cloneActionPlan(plan)
	if err := contentAddressActionPlanNodes(addressed); err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]ConfigDependencySet{}
	originalRoot := ""
	for index, node := range addressed.Nodes {
		dependencies[node.ID] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
		if plan.Nodes[index].ID == rootProducer {
			originalRoot = node.ID
		}
	}
	if originalRoot == "" {
		t.Fatal("test root is absent")
	}
	files := familyTestConfig("1", "0")
	snapshot, err := canonicalActionPlanSnapshot(addressed, dependencies, files)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	executed := family.originalNodeIDs["base"][originalRoot]
	cut, err := NewActionPlanFamilyExecutionCut(family, []ActionPlanFamilyExecutionCutRoot{{NodeID: executed, Slot: 0}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := cut.VerifyVariantReplay("base", snapshot, plan, files)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte(contents)))
	if err != nil {
		t.Fatal(err)
	}
	return replay, observed
}

func TestConfigDependencyObservedHeadersExactOwnerAndRebinding(t *testing.T) {
	for _, mode := range []string{"direct", "work", "tree"} {
		t.Run(mode, func(t *testing.T) {
			plan, consumer, producer := observedDependencyPlanForTest(t, mode, "#include <selected.h>\n")
			ordinary, err := BuildActionPlanConfigDependencyAnalysis(plan)
			if err != nil {
				t.Fatal(err)
			}
			before, err := ordinary.ByNodeID(plan)
			if err != nil || !before[consumer].Opaque {
				t.Fatalf("before = %#v, %v", before, err)
			}
			replay, observed := observedDependencyGateForTest(t, plan, producer, "CONFIG_OBSERVED\n")
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || sets[consumer].Opaque || !slices.Contains(sets[consumer].Symbols, "CONFIG_OBSERVED") || !sets[producer].Opaque {
				t.Fatalf("observed annotations = %#v, %v", sets, err)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(uses) != 1 {
				t.Fatalf("uses = %#v, %v", uses, err)
			}
			use := uses[0]
			if use.ConsumerNodeID != consumer || use.ProducerNodeID != producer || use.Slot != 0 ||
				use.OriginalProducerNodeID != replay.originalIDs[producer] || use.ContentID != observed.Headers()[0].ContentID ||
				use.LogicalPath != "include/generated/selected.h" || use.Tree != "prep" {
				t.Fatalf("receipt lost exact owner/destination: %#v", use)
			}
			if err := contentAddressActionPlanNodes(plan); err != nil {
				t.Fatal(err)
			}
			slices.Reverse(plan.Nodes)
			rebound, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(rebound) != 1 || rebound[0].ProducerNodeID == producer ||
				rebound[0].ConsumerNodeID == consumer || rebound[0].OriginalProducerNodeID != use.OriginalProducerNodeID ||
				rebound[0].ContentID != use.ContentID {
				t.Fatalf("rebound = %#v, %v", rebound, err)
			}
			rebound[0].LogicalPath = "mutated"
			again, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || again[0].LogicalPath != use.LogicalPath {
				t.Fatalf("mutable receipt: %#v, %v", again, err)
			}
		})
	}
}

func TestConfigDependencyObservedHeadersOnlyPublishSuccessfulActualReads(t *testing.T) {
	for _, test := range []struct {
		name, source string
		precise      bool
	}{
		{"unused", "CONFIG_SOURCE\n", true},
		{"later_opaque", "#include <selected.h>\n#include <unavailable.h>\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, consumer, producer := observedDependencyPlanForTest(t, "direct", test.source)
			replay, observed := observedDependencyGateForTest(t, plan, producer, "CONFIG_OBSERVED\n")
			analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			sets, err := analysis.ByNodeID(plan)
			if err != nil || sets[consumer].Opaque == test.precise {
				t.Fatalf("sets = %#v, %v", sets, err)
			}
			uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
			if err != nil || len(uses) != 0 {
				t.Fatalf("eager/unfinished read published: %#v, %v", uses, err)
			}
		})
	}
}

func TestConfigDependencyObservedHeadersChangedBytesInvalidateSharedSyntax(t *testing.T) {
	plan, consumer, producer := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	cache := NewActionPlanConfigDependencySharedCache()
	var previousID string
	for _, symbol := range []string{"CONFIG_FIRST", "CONFIG_SECOND"} {
		replay, observed := observedDependencyGateForTest(t, plan, producer, symbol+"\n")
		analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, cache, replay, observed)
		if err != nil {
			t.Fatal(err)
		}
		sets, err := analysis.ByNodeID(plan)
		if err != nil || sets[consumer].Opaque || !slices.Equal(sets[consumer].Symbols, []string{symbol}) {
			t.Fatalf("same-path changed bytes reused stale scan: %#v, %v", sets, err)
		}
		uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
		if err != nil || len(uses) != 1 || uses[0].ContentID == previousID {
			t.Fatalf("same-path content identity reused: %#v, %v", uses, err)
		}
		previousID = uses[0].ContentID
	}
}

func TestConfigDependencyObservedHeadersPinnedNodesBypassCompletedCache(t *testing.T) {
	plan, consumer, _ := observedDependencyPlanForTest(t, "direct", "CONFIG_USED\n")
	familyCache := NewActionPlanFamilyPlanningCache()
	plan.attachFamilyPlanningCache(familyCache)
	cache := familyCache.configDependencyCache()
	plan.metadata.configFragment = map[string]string{"CONFIG_USED": "y"}
	probes := 0
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		probes++
		return "", true, nil
	}
	// Pinning skips the compiler scan, so only the lowering's retained exact
	// discovery projection can register this invocation before classification.
	plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
		consumer: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[plan.Nodes[0].Recipe].Arguments)},
	}
	ordinary, err := BuildActionPlanConfigDependencyAnalysisWithCache(plan, cache)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := ordinary.ByNodeID(plan)
	if err != nil || sets[consumer].Opaque {
		t.Fatalf("cache seed must be precise: %#v, %v", sets, err)
	}
	if cache.completed.stores == 0 {
		t.Fatal("test did not seed a real completed-compiler cache entry")
	}
	// The whole-result cache is intentionally bypassed in this mode, including
	// entries for unpinned consumers. Pinning must occur after probe registration.
	before := [3]int{cache.completed.hits, cache.completed.misses, cache.completed.stores}
	replay, observed := observedDependencyGateForTest(t, plan, consumer, "not parsed as header\n")
	probes = 0
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, cache, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	sets, err = analysis.ByNodeID(plan)
	if err != nil || !sets[consumer].Opaque || probes == 0 {
		t.Fatalf("pinned node became precise or skipped registration: %#v, probes %d, %v", sets, probes, err)
	}
	after := [3]int{cache.completed.hits, cache.completed.misses, cache.completed.stores}
	if before != after {
		t.Fatalf("completed cache was accessed: %v -> %v", before, after)
	}
}

func TestConfigDependencyObservedHeadersRejectInvalidAuthority(t *testing.T) {
	for _, failure := range []string{"nil-replay", "foreign-plan", "cut", "recipe", "config-slot", "sidecar", "contents"} {
		t.Run(failure, func(t *testing.T) {
			plan, _, producer := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
			replay, observed := observedDependencyGateForTest(t, plan, producer, "CONFIG_OBSERVED\n")
			switch failure {
			case "nil-replay":
				replay = nil
			case "foreign-plan":
				plan = cloneActionPlan(plan)
			case "cut":
				copy := *observed
				copy.cutID = strings.Repeat("9", 64)
				observed = &copy
			case "recipe":
				recipe := plan.Recipes[plan.Nodes[0].Recipe]
				recipe.Arguments = append(slices.Clone(recipe.Arguments), "-DCHANGED")
				plan.Recipes[plan.Nodes[0].Recipe] = recipe
			case "config-slot", "sidecar":
				copy := *observed
				copy.headers = observed.Headers()
				if failure == "config-slot" {
					copy.headers[0].Output.Path = configDependencyAutoconfPath
				} else {
					copy.headers[0].Output.ObservedPath = "state.h"
				}
				observed = &copy
			case "contents":
				copy := *observed
				copy.contents = maps.Clone(observed.contents)
				copy.contents[copy.headers[0].ContentID] = []byte("changed")
				observed = &copy
			}
			if analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed); err == nil || analysis != nil {
				t.Fatalf("accepted %s: %#v / %v", failure, analysis, err)
			}
		})
	}
}

func TestConfigDependencyObservedHeadersRejectSpeculativeCacheReadLeak(t *testing.T) {
	plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	replay, observed := observedDependencyGateForTest(t, plan, producerID, "CONFIG_OBSERVED\n")
	observation, err := newConfigDependencyObservedHeaders(plan, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	projection := observation.projections[configDependencyObservedOutputKey{producerID, 0}]
	profile := plan.selectionGraph.profiles["config-dependency"]
	scanner := configDependencyClosureScanner{
		profile: profile, physicalFiles: newConfigDependencyPhysicalFileCache(),
		generated:          map[string]bool{"include/generated/selected.h": true},
		generatedText:      map[string]configDependencyGeneratedText{"include/generated/selected.h": projection.text},
		includeDirectories: []configDependencyIncludeDirectory{{logical: "include/generated"}},
	}
	including := configDependencyScanFile{logical: "drivers/example/driver.c", source: true}
	include := configDependencyLiteralInclude{name: "selected.h"}
	// Build the first event with the real resolver/binding witness.
	cold := scanner.headerReplayScanner()
	file, found := cold.resolveIncludeFile(including, include)
	if !found {
		t.Fatal("test observed file did not resolve")
	}
	binding, valid := cold.headerBinding(file)
	if !valid {
		t.Fatal("test observed binding not exact")
	}
	entry := configDependencyHeaderCacheEntry{events: []configDependencyHeaderEvent{
		{kind: configDependencyHeaderResolve, including: including, include: include, file: file, binding: binding},
		{kind: configDependencyHeaderResolve, including: including, include: configDependencyLiteralInclude{name: "missing.h"}, file: file},
	}}
	if _, hit := entry.replay(&scanner, nil); hit {
		t.Fatal("invalid cache suffix hit")
	}
	var consumer ActionPlanNode
	for _, node := range plan.Nodes {
		if node.ID == consumerID {
			consumer = node
		}
	}
	observation.readResolved(consumer, scanner.resolvedGeneratedFiles)
	if len(observation.pending) != 0 {
		t.Fatalf("rejected scratch replay published: %#v", observation.pending)
	}
	entry.events = entry.events[:1]
	committed, hit := entry.replay(&scanner, nil)
	if !hit {
		t.Fatal("valid event replay missed")
	}
	observation.readResolved(consumer, committed.resolvedGeneratedFiles)
	if len(observation.pending) != 1 {
		t.Fatalf("valid committed replay lost receipt: %#v", observation.pending)
	}
}

func TestConfigDependencyObservedHeadersUseBudgetFailsClosed(t *testing.T) {
	plan, _, producer := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	replay, observed := observedDependencyGateForTest(t, plan, producer, "CONFIG_OBSERVED\n")
	state, err := newConfigDependencyObservedHeaders(plan, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	state.uses.witnesses.maximumBytes = 1
	if analysis, err := buildActionPlanConfigDependencyAnalysisWithObservations(plan, nil, nil, state); err == nil || analysis != nil {
		t.Fatalf("precise annotations escaped without bounded receipts: %#v / %v", analysis, err)
	}
}

func TestConfigDependencyObservedHeadersOrdinaryAnalysisUnchanged(t *testing.T) {
	plan, _, _ := observedDependencyPlanForTest(t, "direct", "CONFIG_SOURCE\n")
	first, err := BuildActionPlanConfigDependencyAnalysis(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildActionPlanConfigDependencyAnalysisWithGeneratedHeaderDemands(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := first.ByNodeID(plan)
	right, _ := second.ByNodeID(plan)
	if !reflect.DeepEqual(left, right) {
		t.Fatal("disabled annotations changed")
	}
	for _, analysis := range []*ActionPlanConfigDependencyAnalysis{first, second} {
		uses, err := analysis.ObservedHeaderUsesByNodeID(plan)
		if err != nil || len(uses) != 0 {
			t.Fatalf("disabled uses = %#v, %v", uses, err)
		}
	}
}

func TestConfigDependencyObservedHeadersCacheHitsRetainActualReads(t *testing.T) {
	for _, mode := range []string{"ordinary", "forced"} {
		t.Run(mode, func(t *testing.T) {
			source := "#include \"bridge.h\"\n"
			if mode == "forced" {
				source = "CONFIG_UNIT\n"
			}
			plan, consumerID, producerID := observedDependencyPlanForTest(t, "direct", source)
			profile := plan.selectionGraph.profiles["config-dependency"]
			root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
			mustWriteSource(t, root, "drivers/example/bridge.h", "#include <selected.h>\n")
			node := plan.Nodes[0]
			if node.ID != consumerID {
				t.Fatal("unexpected fixture node order")
			}
			if mode == "forced" {
				source := ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: "drivers/example/bridge.h"}
				plan.Sources = append(plan.Sources, source)
				node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "prefix", SourceID: source.ID})
				recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
				recipe.Sources = append(recipe.Sources, "prefix:00000001")
				recipe.Arguments = append([]string{"-include", "${source:prefix:00000001}"}, recipe.Arguments...)
				id, err := recipe.ID()
				if err != nil {
					t.Fatal(err)
				}
				delete(plan.Recipes, node.Recipe)
				node.Recipe = id
				plan.Recipes[id] = recipe
				plan.Nodes[0] = node
			}
			second := node
			second.ID = "second-consumer"
			second.Outputs = []ActionPlanOutput{{Tree: "objects", Path: "drivers/example/second.o"}}
			plan.Nodes = append(plan.Nodes, second)
			selection := compactKbuildSelectionKey{profile: "config-dependency", target: second.Outputs[0].Path, stage: second.Stage}
			plan.selectionGraph.selections[selection] = CompactKbuildSelection{Profile: selection.profile, Target: selection.target, Stage: selection.stage}
			plan.selectionGraph.materializedProducers[selection] = second.ID
			plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
				return "#define __CC__ 1\n", true, nil
			}
			replay, observed := observedDependencyGateForTest(t, plan, producerID, "CONFIG_OBSERVED\n")
			state, err := newConfigDependencyObservedHeaders(plan, replay, observed)
			if err != nil {
				t.Fatal(err)
			}
			context := newConfigDependencyAnalysisContext(plan)
			context.observedHeaders = state
			for _, consumer := range []ActionPlanNode{node, second} {
				set, err := analyzeActionPlanNodeConfigDependencies(plan, consumer, context)
				if err != nil || set.Opaque {
					t.Fatalf("cache consumer = %#v, %v", set, err)
				}
				if err := state.commitUses(consumer, set); err != nil {
					t.Fatal(err)
				}
			}
			hits := context.forcedHeaders.ordinary.hits
			if mode == "forced" {
				hits = context.forcedHeaders.hits
			}
			if hits == 0 || len(state.uses.records) != 2 {
				t.Fatalf("%s cache did not preserve both receipts: hits %d, uses %#v", mode, hits, state.uses.records)
			}
		})
	}
}

func TestConfigDependencyObservedHeadersDoNotWhitelistGeneratorTools(t *testing.T) {
	plan, consumer, producer := observedDependencyPlanForTest(t, "direct", "#include <selected.h>\n")
	for index := range plan.Nodes {
		node := &plan.Nodes[index]
		if node.ID != producer {
			continue
		}
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		delete(plan.Recipes, node.Recipe)
		node.Tool, recipe.Tool = "fixture-generator-tool", "fixture-generator-tool"
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		node.Recipe = id
		plan.Recipes[id] = recipe
	}
	replay, observed := observedDependencyGateForTest(t, plan, producer, "CONFIG_AUTHENTICATED\n")
	analysis, err := BuildActionPlanConfigDependencyAnalysisWithObservedHeaders(plan, nil, replay, observed)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := analysis.ByNodeID(plan)
	if err != nil || sets[consumer].Opaque || !slices.Contains(sets[consumer].Symbols, "CONFIG_AUTHENTICATED") {
		t.Fatalf("authenticated arbitrary producer was not inspectable: %#v, %v", sets, err)
	}
}
