package kconfig

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// This is the old final planning pipeline, intentionally independent of the
// new dispatcher. Shared immutable lowering/analysis primitives stay shared;
// orchestration and finalization order are the behavior under comparison.
func originalFamilyVariantPipelineForTest(m *CompactMetadata, analyze bool, cache *ActionPlanFamilyPlanningCache) (*ActionPlan, map[string]ConfigDependencySet, error) {
	plan, selections, err := m.lowerSelectedActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, cache)
	if err != nil {
		return nil, nil, err
	}
	if err := m.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		return nil, nil, err
	}
	if !m.selectedProductsOnly {
		if err := m.appendTerminalActionPlanNodes(plan, selections); err != nil {
			return nil, nil, err
		}
		if err := m.appendModuleSDKActionPlanNodes(plan); err != nil {
			return nil, nil, err
		}
	}
	var analysis *ActionPlanConfigDependencyAnalysis
	if analyze {
		if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
			return nil, nil, err
		}
		var dependencyCache *ActionPlanConfigDependencySharedCache
		if cache != nil {
			dependencyCache = cache.configDependencyCache()
		}
		analysis, err = BuildActionPlanConfigDependencyAnalysisWithCache(plan, dependencyCache)
		if err != nil {
			return nil, nil, err
		}
	} else {
		plan.projectedGeneratorCandidates = nil
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		return nil, nil, err
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	if analysis == nil {
		return plan, nil, nil
	}
	dependencies, err := analysis.ByNodeID(plan)
	return plan, dependencies, err
}

const familyVariantTestHeader = "include/generated/variant.h"

func familyVariantMetadataForTest(t *testing.T, predefineCalls *int) *CompactMetadata {
	t.Helper()
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/variant.c", "#include <variant.h>\nCONFIG_USED\n")
	// Exercise the typed Kbuild compiler boundary, while the arbitrary awk
	// generator stays opaque until its completed output is observed.
	profile := mustCompactKbuildProfileForTest(t, "build:variant-pipeline", "Makefile", "", `
all: drivers/variant.o
cmd_cc_o_c = $(CC) -nostdinc -I$(objtree)/include/generated -c -o $@ $<
if_changed_dep = $(cmd_$(1))
include/generated/variant.h: FORCE
	awk 'BEGIN { print "CONFIG_USED" }' > $@
drivers/variant.o: drivers/variant.c include/generated/variant.h FORCE
	$(call if_changed_dep,cc_o_c)
.PHONY: FORCE
FORCE:
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "objtree": ".", "srctree": "__LINUX_BZL_SOURCE_TREE__",
	})
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree}); err != nil {
		t.Fatal(err)
	}
	return &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: "drivers/variant.o", MakeTarget: "drivers/variant.o",
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
		configFragment:          map[string]string{"CONFIG_USED": "1", "CONFIG_OTHER": "0"},
		actionRoles:             testConfiguredScopedActionRoles,
		actionContracts:         map[KbuildActionRoleRef]CompactKbuildActionContract{{Scope: "target", Role: "cc"}: {}},
		preconfiguredObjectTree: true,
		selectedProductsOnly:    true,
		compilerPredefines: func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
			if predefineCalls != nil {
				*predefineCalls++
			}
			return "", true, nil
		},
	}
}

func assertFamilyVariantPlanEqualForTest(t *testing.T, got, want *ActionPlan, gotDependencies, wantDependencies map[string]ConfigDependencySet) {
	t.Helper()
	gotData, err := canonicalFamilyReplayPlan(got, familyTestConfig("1", "0"))
	if err != nil {
		t.Fatal(err)
	}
	wantData, err := canonicalFamilyReplayPlan(want, familyTestConfig("1", "0"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotData, wantData) || !reflect.DeepEqual(gotDependencies, wantDependencies) {
		t.Fatal("common pipeline changed legacy plan or annotations")
	}
	if !slices.IsSortedFunc(got.Nodes, func(a, b ActionPlanNode) int { return strings.Compare(a.ID, b.ID) }) {
		t.Fatal("final plan is not sorted")
	}
}

func TestActionPlanFamilyVariantPreservesLegacyPipeline(t *testing.T) {
	for _, mode := range []string{"plain", "dependencies", "family-cache"} {
		t.Run(mode, func(t *testing.T) {
			var gotCache, wantCache *ActionPlanFamilyPlanningCache
			if mode == "family-cache" {
				gotCache, wantCache = NewActionPlanFamilyPlanningCache(), NewActionPlanFamilyPlanningCache()
			}
			for range 2 {
				gotCalls, wantCalls := 0, 0
				want, wantDependencies, err := originalFamilyVariantPipelineForTest(familyVariantMetadataForTest(t, &wantCalls), mode != "plain", wantCache)
				if err != nil {
					t.Fatal(err)
				}
				metadata := familyVariantMetadataForTest(t, &gotCalls)
				var got *ActionPlan
				var gotDependencies map[string]ConfigDependencySet
				switch mode {
				case "plain":
					got, err = metadata.ActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity)
				case "dependencies":
					got, gotDependencies, err = metadata.ActionPlanWithConfigDependencies(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity)
				case "family-cache":
					got, gotDependencies, err = metadata.ActionPlanWithFamilyPlanningCache(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, gotCache)
				}
				if err != nil {
					t.Fatal(err)
				}
				assertFamilyVariantPlanEqualForTest(t, got, want, gotDependencies, wantDependencies)
				if gotCalls != wantCalls {
					t.Fatalf("compiler probe calls = %d, old pipeline %d", gotCalls, wantCalls)
				}
			}
		})
	}
}

func TestActionPlanFamilyVariantInitialReportsFinalDemandIDs(t *testing.T) {
	metadata := familyVariantMetadataForTest(t, nil)
	result, err := metadata.ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity,
		ActionPlanFamilyVariantPlanningOptions{Variant: "base", Cache: NewActionPlanFamilyPlanningCache()})
	if err != nil {
		t.Fatal(err)
	}
	want, dependencies, err := originalFamilyVariantPipelineForTest(familyVariantMetadataForTest(t, nil), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFamilyVariantPlanEqualForTest(t, result.Plan, want, result.Dependencies, dependencies)
	if !result.GeneratedHeaderDemands.Enabled || result.GeneratedHeaderDemands.Truncated || len(result.GeneratedHeaderDemands.Demands) != 1 || len(result.ObservedHeaderUses) != 0 {
		t.Fatalf("initial frontier = %#v, observed uses = %#v, dependencies = %#v", result.GeneratedHeaderDemands, result.ObservedHeaderUses, result.Dependencies)
	}
	if result.analysis == nil || result.observed != nil || result.cut != nil {
		t.Fatal("initial result retained incorrect private replay authority")
	}
	demand := result.GeneratedHeaderDemands.Demands[0]
	consumer, consumerFound := compactKbuildPlanNode(result.Plan, demand.ConsumerNodeID)
	producer, producerFound := compactKbuildPlanNode(result.Plan, demand.ProducerNodeID)
	if !consumerFound || !producerFound || !result.Dependencies[consumer.ID].Opaque || demand.Path != familyVariantTestHeader ||
		demand.Slot < 0 || demand.Slot >= len(producer.Outputs) || producer.Outputs[demand.Slot].Path != familyVariantTestHeader {
		t.Fatalf("demand was not rebound to the final plan: %#v", demand)
	}
}

func familyVariantReplayOptionsForTest(t *testing.T) ActionPlanFamilyVariantPlanningOptions {
	t.Helper()
	initial, err := familyVariantMetadataForTest(t, nil).ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity,
		ActionPlanFamilyVariantPlanningOptions{Variant: "base"})
	if err != nil {
		t.Fatal(err)
	}
	dependencies := maps.Clone(initial.Dependencies)
	for id := range dependencies {
		dependencies[id] = ConfigDependencySet{Opaque: true, Reason: "initial conservative execution"}
	}
	files := familyTestConfig("1", "0")
	snapshot, err := canonicalActionPlanSnapshot(initial.Plan, dependencies, files)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	roots := executionCutRootsForTest(family, familyVariantTestHeader)
	if len(roots) != 1 {
		t.Fatalf("expected one ordinary generator root, got %v", roots)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte("#define VARIANT_GENERATED 1\nCONFIG_USED\n")))
	if err != nil {
		t.Fatal(err)
	}
	return ActionPlanFamilyVariantPlanningOptions{
		Variant: "base", InitialSnapshot: &snapshot, Cut: cut, ObservedHeaders: observed, ResolvedConfigFiles: files,
	}
}

func TestActionPlanFamilyVariantReplayReturnsFinalObservedUses(t *testing.T) {
	options := familyVariantReplayOptionsForTest(t)
	options.Cache = NewActionPlanFamilyPlanningCache()
	result, err := familyVariantMetadataForTest(t, nil).ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GeneratedHeaderDemands.Enabled || result.GeneratedHeaderDemands.Truncated || len(result.GeneratedHeaderDemands.Demands) != 0 || len(result.ObservedHeaderUses) != 1 {
		t.Fatalf("replay frontier=%#v uses=%#v dependencies=%#v", result.GeneratedHeaderDemands, result.ObservedHeaderUses, result.Dependencies)
	}
	use := result.ObservedHeaderUses[0]
	if result.Plan.metadata != nil || result.Plan.selectionGraph != nil || result.Plan.familyPlanningCache != nil {
		t.Fatal("final replay result retained its source evaluator or graph caches")
	}
	if result.analysis == nil || result.observed != options.ObservedHeaders || result.cut != options.Cut ||
		result.variant != options.Variant || !maps.Equal(result.replayConfigFiles, options.ResolvedConfigFiles) {
		t.Fatal("replay result lost private analysis/observation authority")
	}
	replayBytes, err := canonicalFamilyReplayPlan(result.Plan, result.replayConfigFiles)
	if err != nil || sha256.Sum256(replayBytes) != result.replayContractDigest {
		t.Fatalf("final result lost original replay contract seal: %v", err)
	}
	consumer, consumerFound := compactKbuildPlanNode(result.Plan, use.ConsumerNodeID)
	producer, producerFound := compactKbuildPlanNode(result.Plan, use.ProducerNodeID)
	if !consumerFound || !producerFound || result.Dependencies[consumer.ID].Opaque || !result.Dependencies[producer.ID].Opaque ||
		use.Path != familyVariantTestHeader || use.ContentID != options.ObservedHeaders.Headers()[0].ContentID || use.OriginalProducerNodeID == "" {
		t.Fatalf("observed use did not retain final/original identities and cut opacity: %#v", use)
	}
	// The API returns separate receipts, not extra serialized snapshot fields.
	if _, err := canonicalActionPlanSnapshot(result.Plan, result.Dependencies, options.ResolvedConfigFiles); err != nil {
		t.Fatal(err)
	}
	result.ObservedHeaderUses[0].ContentID = "caller-forged-diagnostic"
	verified, err := result.analysis.ObservedHeaderUsesByNodeID(result.Plan)
	if err != nil || len(verified) != 1 || verified[0] != use {
		t.Fatalf("public diagnostic mutation changed retained evidence: %v %v", verified, err)
	}
	options.ResolvedConfigFiles["include/config/kernel.release"] = "caller-mutated\n"
	if result.replayConfigFiles["include/config/kernel.release"] == "caller-mutated\n" {
		t.Fatal("replay config witness aliases mutable caller input")
	}
}

func TestActionPlanFamilyVariantReplayInternsDiscoveredSourceHeaders(t *testing.T) {
	for _, generated := range []bool{false, true} {
		t.Run(fmt.Sprintf("generated-frontier-%v", generated), func(t *testing.T) {
			metadata := func() *CompactMetadata {
				m := familyVariantMetadataForTest(t, nil)
				root := m.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
				text := "#include \"discovered.h\"\nCONFIG_USED\n"
				if generated {
					text = "#include <variant.h>\n" + text
				}
				mustWriteSource(t, root, "drivers/variant.c", text)
				mustWriteSource(t, root, "drivers/discovered.h", "#define DISCOVERED 1\n")
				return m
			}
			initial, err := metadata().ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity,
				ActionPlanFamilyVariantPlanningOptions{Variant: "base"})
			if err != nil {
				t.Fatal(err)
			}
			files := familyTestConfig("1", "0")
			snapshot, err := canonicalActionPlanSnapshot(initial.Plan, initial.Dependencies, files)
			if err != nil {
				t.Fatal(err)
			}
			family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: snapshot}})
			if err != nil {
				t.Fatal(err)
			}
			cut, err := NewActionPlanFamilyExecutionCut(family, executionCutRootsForTest(family, familyVariantTestHeader))
			if err != nil {
				t.Fatal(err)
			}
			observed, err := cut.ObserveHeaders(observedHeadersTestStores(t, cut, []byte("#define VARIANT_GENERATED 1\n")))
			if err != nil {
				t.Fatal(err)
			}
			result, err := metadata().ActionPlanFamilyVariant(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity,
				ActionPlanFamilyVariantPlanningOptions{
					Variant: "base", InitialSnapshot: &snapshot, Cut: cut, ObservedHeaders: observed, ResolvedConfigFiles: files,
				})
			if err != nil {
				t.Fatal(err)
			}
			var discovered *ActionPlanSource
			for index := range result.Plan.Sources {
				if result.Plan.Sources[index].Path == "drivers/discovered.h" {
					discovered = &result.Plan.Sources[index]
				}
			}
			if discovered == nil || discovered.Namespace != "kernel" {
				t.Fatalf("replay lost discovered source namespace: %v", discovered)
			}
			precise := false
			for _, set := range result.Dependencies {
				precise = precise || !set.Opaque && slices.Contains(set.SourcePaths, discovered.Path)
			}
			if !precise {
				t.Fatal("replay did not analyze the discovered header")
			}
			if _, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result}); err != nil {
				t.Fatalf("final family rejected trusted source-catalog growth: %v", err)
			}
			// Namespace metadata is part of the full post-analysis seal, even
			// while its descriptor is not an edge in the unreduced plan.
			discovered.Namespace = "forged"
			if _, _, _, err := buildObservedActionPlanFamily([]*ActionPlanFamilyVariantPlanningResult{result}); err == nil {
				t.Fatal("final family accepted a changed discovered-source namespace")
			}
		})
	}
}

func TestActionPlanFamilyVariantRejectsPartialReplayBeforeLowering(t *testing.T) {
	var metadata *CompactMetadata
	for mask := 1; mask < 15; mask++ {
		t.Run(fmt.Sprintf("bundle-%02x", mask), func(t *testing.T) {
			options := ActionPlanFamilyVariantPlanningOptions{Variant: "base"}
			if mask&1 != 0 {
				options.InitialSnapshot = new(ActionPlanSnapshot)
			}
			if mask&2 != 0 {
				options.Cut = new(ActionPlanFamilyExecutionCut)
			}
			if mask&4 != 0 {
				options.ObservedHeaders = new(ActionPlanFamilyObservedHeaders)
			}
			if mask&8 != 0 {
				options.ResolvedConfigFiles = map[string]string{}
			}
			result, err := metadata.ActionPlanFamilyVariant("invalid", "invalid", options)
			if err == nil || result != nil || !strings.Contains(err.Error(), "together") {
				t.Fatalf("partial bundle reached lowering: %v %v", result, err)
			}
		})
	}
	for _, variant := range []string{"", "../invalid", "bad/name"} {
		result, err := metadata.ActionPlanFamilyVariant("invalid", "invalid", ActionPlanFamilyVariantPlanningOptions{Variant: variant})
		if err == nil || result != nil || !strings.Contains(err.Error(), "variant") {
			t.Fatalf("invalid initial variant reached lowering: %v %v", result, err)
		}
	}
}

func TestActionPlanFamilyVariantRejectsReplayBeforeCompilerAnalysis(t *testing.T) {
	for _, failure := range []string{"resolved-config", "variant", "toolset", "uninitialized-cut", "uninitialized-observations"} {
		t.Run(failure, func(t *testing.T) {
			options := familyVariantReplayOptionsForTest(t)
			target := actionPlanTestProbeIdentity
			switch failure {
			case "resolved-config":
				options.ResolvedConfigFiles = maps.Clone(options.ResolvedConfigFiles)
				options.ResolvedConfigFiles["include/config/kernel.release"] = "different\n"
			case "variant":
				options.Variant = "other"
			case "toolset":
				target = "sha256-" + strings.Repeat("a", 64)
			case "uninitialized-cut":
				options.Cut = new(ActionPlanFamilyExecutionCut)
			case "uninitialized-observations":
				options.ObservedHeaders = new(ActionPlanFamilyObservedHeaders)
			}
			calls := 0
			result, err := familyVariantMetadataForTest(t, &calls).ActionPlanFamilyVariant(target, actionPlanTestProbeIdentity, options)
			if err == nil || result != nil || calls != 0 {
				t.Fatalf("rejected replay reached compiler analysis: result=%v calls=%d error=%v", result, calls, err)
			}
		})
	}
}
