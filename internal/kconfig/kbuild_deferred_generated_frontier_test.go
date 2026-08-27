package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestDeferredKbuildQueryObservesInvocationRelativeObjectOperand(t *testing.T) {
	profile := CompactKbuildProfile{Name: "external-demo"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: ".linux-bzl/external/demo",
	}); err != nil {
		t.Fatal(err)
	}
	query, err := normalizedKbuildDeferredContentQuery(KbuildDeferredContentQuery{
		Command: "awk '{print $1}' ../../../include/generated/asm-offsets.h " +
			"--input=../../../include/generated/asm-offsets.h section=noload",
		Target:  "stack_protector_prepare",
		Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := query.ObjectTree, (CompactKbuildObjectTreeObservation{
		ObservesObjectTree: true,
		References:         []string{"include/generated/asm-offsets.h"},
	}); !slices.Equal(got.References, want.References) ||
		got.ObservesObjectTree != want.ObservesObjectTree || got.ObservesAll != want.ObservesAll {
		t.Fatalf("invocation-relative object-tree observation = %#v, want %#v", got, want)
	}
}

func TestDeferredKbuildQueryIgnoresSymbolicProbeOperands(t *testing.T) {
	const directory = ".linux-bzl/external/demo"
	probeToken := "LINUX_BZL_PROBE_" + strings.Repeat("a", 64)
	profile := CompactKbuildProfile{Name: "external-demo"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	query, err := normalizedKbuildDeferredContentQuery(KbuildDeferredContentQuery{
		Command: "cat rust/" + probeToken + " --flag=rust/" + probeToken,
		Target:  "probe_dependent_query",
		Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := query.ObjectTree; got.ObservesObjectTree || got.ObservesAll || len(got.References) != 0 {
		t.Fatalf("symbolic deferred-query operands produced object-tree observation %#v", got)
	}
}

func TestDeferredKbuildQueryBindsExactPreconfiguredObjectOperand(t *testing.T) {
	const (
		invocationDir = ".linux-bzl/external/demo"
		operand       = "include/generated/asm-offsets.h"
		relativeInput = "../../../" + operand
	)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, operand, "#define TSK_STACK_CANARY 40\n")
	profile := mustCompactKbuildProfileForTest(t, "external-demo", "scripts/Makefile.build", invocationDir, "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": t.TempDir(),
		"__LINUX_BZL_OBJECT_TREE__": objectRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: invocationDir,
	}); err != nil {
		t.Fatal(err)
	}
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("a", 64)
	query, err := normalizedKbuildDeferredContentQuery(KbuildDeferredContentQuery{
		Token:   token,
		Command: "awk '{if ($2 == \"TSK_STACK_CANARY\") print $3;}' " + relativeInput,
		Target:  "stack_protector_prepare",
		Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:             testConfiguredScopedActionRoles,
		preconfiguredObjectTree: true,
		exactSourceNamespaces:   map[string]string{operand: "prep"},
		Config:                  CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, metadata: metadata}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	producer, err := builder.buildDeferredKbuildContentQuery(
		query,
		KbuildDeferredContentSelection{
			Token: token, Profile: profile.Name, Target: query.Target,
			Lifecycle: "target", Scope: "target", Stage: "target", UsesInitialObjectTree: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("deferred query producer %q is absent", producer)
	}
	preparedSourceID := ""
	for _, source := range plan.Sources {
		if source.Namespace == "prep" && source.Path == operand {
			preparedSourceID = source.ID
			break
		}
	}
	if preparedSourceID == "" || !slices.ContainsFunc(node.Sources, func(edge ActionPlanSourceEdge) bool {
		return edge.SourceID == preparedSourceID
	}) {
		t.Fatalf("deferred query sources = %#v from %#v, want exact prep/%s source", node.Sources, plan.Sources, operand)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), operand) {
		t.Fatalf("deferred query working inputs = %#v, want canonical path %q", recipe.WorkingInputs, operand)
	}
	if got := recipe.ExecutionDirectory; got != invocationDir {
		t.Fatalf("deferred query execution directory = %q, want %q", got, invocationDir)
	}
}

// A deferred Make-shell query may enter a configured action through an
// exported variable rather than through the action's argv.  Its object-tree
// operands still belong to that exact selected invocation: in particular, a
// target-generated operand consumed by a host action must use the native
// bootstrap producer selected for that host closure.
func TestDeferredKbuildExportBindsExactGeneratedFrontierOperand(t *testing.T) {
	const (
		invocationDir  = ".linux-bzl/external/demo"
		operand        = "include/generated/asm-offsets.h"
		relativeInput  = "../../../" + operand
		operandSource  = "arch/arm64/kernel/asm-offsets.c"
		firstConsumer  = "usr/gen_init_cpio"
		secondConsumer = "usr/gen_crc32table"
		firstSource    = firstConsumer + ".c"
		secondSource   = secondConsumer + ".c"
	)
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("e", 64)

	producerProfile := mustCompactKbuildProfileForTest(t, "asm-offset-owner", "Kbuild", "", `
cmd_cc_s_c = $(CC) -c -o $@ $<
include/generated/asm-offsets.h: arch/arm64/kernel/asm-offsets.c FORCE
	$(call if_changed,cc_s_c)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
	})
	producerProfile = compactKbuildProfileWithSourcesForTest(t, producerProfile, operandSource)

	consumerProfile := mustCompactKbuildProfileForTest(t, "host-consumer", "scripts/Makefile.host", "usr", `
export KBUILD_CFLAGS
cmd_host-csingle = $(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ $<
usr/gen_init_cpio: usr/gen_init_cpio.c FORCE
	$(call if_changed,host-csingle)
usr/gen_crc32table: usr/gen_crc32table.c FORCE
	$(call if_changed,host-csingle)
`, map[string]string{
		"HOSTCC":            KbuildActionRoleToken("host", "cc"),
		"KBUILD_CFLAGS":     "-mstack-protector-guard-offset=" + token,
		"KBUILD_HOSTCFLAGS": "-DHOST_BUILD",
	})
	consumerProfile = compactKbuildProfileWithSourcesForTest(t, consumerProfile, firstSource, secondSource)
	queryProfile := consumerProfile
	if err := SetCompactKbuildProfileInvocationLocation(&queryProfile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: invocationDir,
	}); err != nil {
		t.Fatal(err)
	}
	queryProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
	consumerProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{
		token: {
			Token: token,
			Command: "awk '{if ($2 == \"TSK_STACK_CANARY\") print $3;}' " +
				relativeInput,
			Target:    "stack_protector_prepare",
			Profile:   queryProfile,
			Transform: ActionRecipeContentTransformMakeShellSingleWord,
		},
	}

	artifact := CompactKbuildVisibleArtifact{
		Path: operand, Profile: producerProfile.Name, Target: operand,
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{producerProfile, consumerProfile},
			KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
				Token: token, Profile: consumerProfile.Name, Target: "stack_protector_prepare",
				Lifecycle: "prep", Scope: "target", Stage: "bootstrap",
				GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
					[]CompactKbuildVisibleArtifact{artifact},
				),
			}},
			KbuildSelections: []CompactKbuildSelection{
				{
					Profile: producerProfile.Name, Target: operand, MakeTarget: operand, Lifecycle: "prep", Scope: "target", Stage: "bootstrap",
				},
				{
					Profile: consumerProfile.Name, Target: firstConsumer, MakeTarget: firstConsumer, DeferredContentQueries: EncodeCompactKbuildDeferredContentQueries([]string{token}),
					Lifecycle: "target", Scope: "host", Stage: "host",
				},
				{
					Profile: consumerProfile.Name, Target: secondConsumer, MakeTarget: secondConsumer, DeferredContentQueries: EncodeCompactKbuildDeferredContentQueries([]string{token}),
					Lifecycle: "target", Scope: "host", Stage: "host",
				},
			},
		},
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": actionPlanTestProbeIdentity,
			"host":   actionPlanTestProbeIdentity,
		},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	if _, err := metadata.appendGeneratedActionPlan(plan); err != nil {
		t.Fatal(err)
	}

	bootstrapID, _, ok := planProducerByOutput(plan, "bootstrap", operand)
	if !ok {
		t.Fatalf("plan omits native bootstrap producer for %q: %#v", operand, plan.Nodes)
	}
	queryID := ""
	for _, consumer := range []string{firstConsumer, secondConsumer} {
		consumerID, _, found := planProducerByOutput(plan, "host", consumer)
		if !found {
			t.Fatalf("plan omits host consumer %q: %#v", consumer, plan.Nodes)
		}
		consumerNode, found := compactKbuildPlanNode(plan, consumerID)
		if !found {
			t.Fatalf("host consumer producer %q is absent", consumerID)
		}
		consumerQueryID := ""
		for _, edge := range consumerNode.Inputs {
			candidate, candidateFound := compactKbuildPlanNode(plan, edge.ProducerID)
			if candidateFound && len(candidate.Outputs) == 1 &&
				strings.HasPrefix(candidate.Outputs[0].Path, ".linux-bzl-content/") {
				consumerQueryID = candidate.ID
				break
			}
		}
		if consumerQueryID == "" {
			t.Fatalf("host consumer %q inputs = %#v, want deferred-content query", consumer, consumerNode.Inputs)
		}
		if queryID == "" {
			queryID = consumerQueryID
		} else if queryID != consumerQueryID {
			t.Fatalf("shared token produced host queries %q and %q", queryID, consumerQueryID)
		}
	}
	queryNode, ok := compactKbuildPlanNode(plan, queryID)
	if !ok || queryNode.Stage != "bootstrap" || queryNode.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("deferred query node = %#v, want bootstrap scriptrun fallback", queryNode)
	}
	if !slices.ContainsFunc(queryNode.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == bootstrapID
	}) {
		t.Fatalf("deferred query inputs = %#v, want exact bootstrap operand producer %s", queryNode.Inputs, bootstrapID)
	}
	queryRecipe := plan.Recipes[queryNode.Recipe]
	workingInput := false
	for _, workingPath := range queryRecipe.WorkingInputs {
		if workingPath == operand {
			workingInput = true
			break
		}
	}
	if !workingInput {
		t.Fatalf("deferred query working inputs = %#v, want canonical path %q", queryRecipe.WorkingInputs, operand)
	}
	if got := queryRecipe.ExecutionDirectory; got != invocationDir {
		t.Fatalf("deferred query execution directory = %q, want %q", got, invocationDir)
	}
}
