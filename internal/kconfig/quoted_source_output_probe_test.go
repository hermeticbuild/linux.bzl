package kconfig

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

type selectedSourceResultReader struct {
	results ProbeResultLookup
	reads   []ProbeReference
}

func TestSelectedSourceFilechkMeasuresDirectScriptWithExportAndPrewriterOwner(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Kconfig", "mainmenu \"fixture\"\n")
	mustWriteSource(t, root, "scripts/generate-release", "#!/bin/sh\nset -e\nprintf '%s-fixture\\n' \"$KERNELVERSION\"\n")
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	scopes.evaluators["target"].scriptEnvironment["KERNELVERSION"] = "6.18.52"
	const target = "include/config/kernel.release"
	recipe := "{\n${tree:kernel}/scripts/generate-release ${tree:kernel}\n} > '" + target + "'"
	const configPath = "include/config/auto.conf"
	config := map[string]string{configPath: "CONFIG_LOCALVERSION=\"-selected\"\n"}
	owners := map[string]string{configPath: "selected-config-owner"}
	query := func(names []string, files map[string]string) (ProbeReference, ProbeRequest) {
		t.Helper()
		value, concrete, recognized, refs, err := scopes.SelectedSourceFilechkOutputText(
			target, recipe, config, names, files, owners, nil, true,
		)
		if err != nil || concrete || !recognized || value != "" || len(refs) != 1 || refs[0].Scope != "target" {
			t.Fatalf("selected source generator = %q/%t/%t refs=%v err=%v", value, concrete, recognized, refs, err)
		}
		plan, err := builder.Plan(refs...)
		if err != nil {
			t.Fatal(err)
		}
		return refs[0], plan.Requests[refs[0].RequestID]
	}
	selected, request := query([]string{configPath}, config)
	if !slices.Contains(request.Sources, "scripts/generate-release") ||
		!slices.Contains(request.SourceRoots, linuxProbeSourceRootName) ||
		request.Steps[1].Environment["KERNELVERSION"] != "6.18.52" {
		t.Fatalf("selected generator lost source/exports: %#v", request)
	}
	if !slices.ContainsFunc(request.Scratch, func(item ProbeScratch) bool {
		return item.Content == owners[configPath] && item.ContentIsOpaque
	}) {
		t.Fatalf("selected predecessor owner omitted from request: %#v", request.Scratch)
	}
	scopes.evaluators["target"].scriptEnvironment["KERNELVERSION"] = "6.18.53"
	changed, _ := query([]string{configPath}, config)
	if changed.RequestID == selected.RequestID {
		t.Fatal("different Make export did not change selected producer identity")
	}
	_, concrete, recognized, refs, err := scopes.SelectedSourceFilechkOutputText(
		target, recipe, config, []string{"include/generated/opaque.h"}, nil, nil, nil, true,
	)
	if err != nil || concrete || recognized || len(refs) != 0 {
		t.Fatalf("opaque prewriter artifact gained measured output authority: %t/%t refs=%v err=%v", concrete, recognized, refs, err)
	}
}

func TestSelectedSourceFilechkExportsRecursiveMakeCapabilityWithoutPrivateBytes(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Kconfig", "mainmenu \"fixture\"\n")
	mustWriteSource(t, root, "scripts/emit-release", "#!/bin/sh\nawk 'BEGIN { print ENVIRON[\"MAKE\"] }'\n")
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	scopes.evaluators["target"].scriptEnvironment["MAKE"] = CompactKbuildRecursiveMakeProvenanceToken
	scopes.evaluators["target"].scriptEnvironment["MAKE_ALIAS"] = CompactKbuildRecursiveMakeProvenanceToken + " --no-print-directory"
	scopes.evaluators["target"].scriptEnvironment["EXTRA"] = "exported-to-child"
	const target = "include/config/kernel.release"
	recipe := "{\n${tree:kernel}/scripts/emit-release ${tree:kernel}\n} > '" + target + "'"
	_, concrete, recognized, refs, err := scopes.SelectedSourceFilechkOutputText(
		target, recipe, nil, nil, nil, nil, nil, true,
	)
	if err != nil || concrete || !recognized || len(refs) != 1 {
		t.Fatalf("selected source capability = concrete=%t recognized=%t refs=%v err=%v", concrete, recognized, refs, err)
	}
	plan, err := builder.Plan(refs...)
	if err != nil {
		t.Fatal(err)
	}
	request := plan.Requests[refs[0].RequestID]
	process := request.Steps[1]
	if process.Environment["MAKE"] != CompactKbuildRecursiveMakeReplayName ||
		process.Environment["MAKE_ALIAS"] != CompactKbuildRecursiveMakeReplayName+" --no-print-directory" ||
		process.Environment["EXTRA"] != "exported-to-child" {
		t.Fatalf("selected exported capability and ordinary data = %#v", process.Environment)
	}
	for _, step := range request.Steps {
		for _, value := range step.Environment {
			if strings.Contains(value, CompactKbuildRecursiveMakeProvenanceToken) {
				t.Fatal("evaluator-private recursive Make marker escaped into a process environment")
			}
		}
	}
	denied := false
	for index, argument := range process.Arguments {
		if argument != "-replay_base64" || index+1 == len(process.Arguments) {
			continue
		}
		manifest, err := base64.StdEncoding.DecodeString(process.Arguments[index+1])
		if err != nil {
			t.Fatal(err)
		}
		denied = strings.Contains(string(manifest), `"name":"make"`) &&
			strings.Contains(string(manifest), `"deny_all":true`) &&
			strings.Contains(string(manifest), `"invocations":[]`)
	}
	if !denied {
		t.Fatal("selected source request omitted its explicit no-recursive-Make runtime capability")
	}
}

func TestSelectedSourceFeatureSourceRoundsAdvanceAcrossBothProducers(t *testing.T) {
	_, _, root, first, baseline := quotedSourceVersionFixtureScopes(t)
	const second = "include/config/another.release"
	mustWriteSource(t, root, "tools/build/feature/Makefile", featureChildMakefileFixture)
	mustWriteSource(t, root, "tools/build/feature/test-hello.c", "int main(void) { return 0; }\n")
	featureCommand := CompactKbuildRecursiveMakeProvenanceToken +
		` OUTPUT=__LINUX_BZL_OBJECT_TREE__/private/feature/` +
		` CC="` + KbuildActionRoleToken("host", "cc") + `"` +
		` CFLAGS=" -I." LDFLAGS=" "` +
		` -C __LINUX_BZL_SOURCE_TREE__/tools/build/feature` +
		` __LINUX_BZL_OBJECT_TREE__/private/feature/test-hello.bin` +
		` >/dev/null 2>/dev/null && echo 1 || echo 0`
	newRound := func() (*ProbePlanBuilder, *KbuildProbeScopes) {
		t.Helper()
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
		if err != nil {
			t.Fatal(err)
		}
		return builder, testSourceScriptOutputDualScopes(t, root, builder, nil)
	}
	selectedFilechk := func(scopes *KbuildProbeScopes, target string, measured ProbeResultLookup) (string, bool, []ProbeReference) {
		t.Helper()
		recipe := "{\necho \"5.10.270$(sh ${tree:kernel}/scripts/source-version ${tree:kernel})\"\n} > '" + target + "'"
		value, concrete, recognized, refs, err := scopes.SelectedSourceFilechkOutputText(
			target, recipe, baseline, nil, nil, nil, measured, true,
		)
		if err != nil || !recognized || len(refs) != 1 {
			t.Fatalf("selected source writer %q = (%q,%t,%t,%#v,%v)", target, value, concrete, recognized, refs, err)
		}
		return value, concrete, refs
	}
	firstBuilder, firstScopes := newRound()
	text, concrete, firstRefs := selectedFilechk(firstScopes, first, nil)
	if text != "" || concrete {
		t.Fatalf("first writer must await its measured result: (%q,%t)", text, concrete)
	}
	firstPlan, err := firstBuilder.Plan(firstRefs...)
	if err != nil {
		t.Fatal(err)
	}
	results := probeResultMap{}
	const exact = "5.10.270-fixture\n"
	addSourceResults := func(plan *ProbePlan, refs []ProbeReference) {
		for _, ref := range refs {
			request := plan.Requests[ref.RequestID]
			envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(exact)) + "\n"
			results[ref.NodeID] = ProbeResult{
				Schema: LinuxProbeResultSchema, NodeID: ref.NodeID, RequestID: ref.RequestID,
				Scope: ref.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
				Steps: evaluatedScriptOutputResultSteps(request, envelope),
			}
		}
	}
	addSourceResults(firstPlan, firstRefs)
	oracle := &ProbeResultOracle{
		results: results, toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
	}
	if err := oracle.ValidatePlan(firstPlan); err != nil {
		t.Fatal(err)
	}
	priorSource := NewSelectedSourceOutputProbeLookup(firstPlan, oracle)
	featureBuilder, featureScopes := newRound()
	if text, concrete, _ := selectedFilechk(featureScopes, first, priorSource); text != exact || !concrete {
		t.Fatalf("the feature stage did not pass the first writer: (%q,%t)", text, concrete)
	}
	status, featureIDs, err := featureScopes.MeasureSelectedFeatureDumpStatus(context.Background(), featureCommand, nil, true)
	if err != nil || status == "0" || status == "1" || len(featureIDs) != 1 {
		t.Fatalf("selected feature awaits measured compiler status: (%q,%v,%v)", status, featureIDs, err)
	}
	featureCandidates, err := featureBuilder.Plan(featureScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	featurePlan, err := SelectProbePlanTerminals(featureCandidates, featureIDs)
	if err != nil {
		t.Fatal(err)
	}
	featureNode := featurePlan.Nodes[0]
	passed := true
	results[featureNode.ID] = ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: featureNode.ID, RequestID: featureNode.RequestID,
		Scope: featureNode.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &passed,
		Steps: []ProbeStepResult{{Name: featurePlan.Requests[featureNode.RequestID].Steps[0].Name, Status: "success", ExitCode: 0}},
	}
	sealedFeature, err := NewKbuildGraphGuardResults(featurePlan, oracle)
	if err != nil {
		t.Fatal(err)
	}
	secondBuilder, secondScopes := newRound()
	if text, concrete, _ := selectedFilechk(secondScopes, first, priorSource); text != exact || !concrete {
		t.Fatalf("source round after feature lost first writer: (%q,%t)", text, concrete)
	}
	if status, _, err := secondScopes.MeasureSelectedFeatureDumpStatus(context.Background(), featureCommand, sealedFeature, true); err != nil || status != "1" {
		t.Fatalf("measured feature must let later writer become visible: (%q,%v)", status, err)
	}
	text, concrete, secondRefs := selectedFilechk(secondScopes, second, priorSource)
	if text != "" || concrete {
		t.Fatalf("second selected writer must remain pending: (%q,%t)", text, concrete)
	}
	secondPlan, err := secondBuilder.Plan(secondRefs...)
	if err != nil {
		t.Fatal(err)
	}
	union, err := MergeProbePlans([]ProbePlanVariant{{Name: "first", Plan: firstPlan}, {Name: "second", Plan: secondPlan}})
	if err != nil || len(union.Terminal) != 2 {
		t.Fatalf("both selected filechk writers = %#v, %v", union, err)
	}
	if err := oracle.ValidatePlan(union); err == nil {
		t.Fatal("unmeasured second writer must fail before ordinary replay")
	}
	addSourceResults(secondPlan, secondRefs)
	if err := oracle.ValidatePlan(union); err != nil {
		t.Fatal(err)
	}
	_, finalScopes := newRound()
	for _, target := range []string{first, second} {
		if text, concrete, _ := selectedFilechk(finalScopes, target, NewSelectedSourceOutputProbeLookup(union, oracle)); text != exact || !concrete {
			t.Fatalf("completed interleaved frontier lost writer %q: (%q,%t)", target, text, concrete)
		}
	}
}

func (reader *selectedSourceResultReader) Result(reference ProbeReference) (ProbeResult, error) {
	reader.reads = append(reader.reads, reference)
	return reader.results.Result(reference)
}

func TestQuotedSourceOutputPreflightsMeasuredVariantBeforeResultRead(t *testing.T) {
	_, _, root, target, baseline := quotedSourceVersionFixtureScopes(t)
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputDualScopes(t, root, builder, nil)
	recipe := "{\necho \"5.10.270$(sh ${tree:kernel}/scripts/source-version ${tree:kernel})\"\n} > '" + target + "'"
	discover := func(names []string, files, owners map[string]string) []ProbeReference {
		t.Helper()
		value, concrete, recognized, references, err := scopes.SelectedSourceFilechkOutputText(
			target, recipe, baseline, names, files, owners, nil, true,
		)
		if err != nil || value != "" || concrete || !recognized || len(references) != 1 {
			t.Fatalf("selected source discovery = (%q,%t,%t,%v,%v), want the target request", value, concrete, recognized, references, err)
		}
		return references
	}
	baseRefs := discover(nil, nil, nil)
	basePlan, err := builder.Plan(baseRefs...)
	if err != nil {
		t.Fatal(err)
	}
	const localversion = "localversion-extra"
	extraFiles := map[string]string{localversion: "-extra\n"}
	extraRefs := discover([]string{localversion}, extraFiles,
		map[string]string{localversion: "selected-writer-a"})
	extraPlan, err := builder.Plan(extraRefs...)
	if err != nil {
		t.Fatal(err)
	}
	if baseRefs[0].NodeID == extraRefs[0].NodeID {
		t.Fatal("different selected localversion presence/producer did not change the source request")
	}
	measured, err := MergeProbePlans([]ProbePlanVariant{
		{Name: "baseline", Plan: basePlan}, {Name: "extra-localversion", Plan: extraPlan},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(measured.Terminal) != 2 {
		t.Fatalf("merged source terminals = %q, want both selected source variants", measured.Terminal)
	}
	results := probeResultMap{}
	const exact = "5.10.270-fixture\n"
	for _, reference := range append(slices.Clone(baseRefs), extraRefs...) {
		request := measured.Requests[reference.RequestID]
		envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(exact)) + "\n"
		results[reference.NodeID] = ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
			Scope: reference.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
			Steps: evaluatedScriptOutputResultSteps(request, envelope),
		}
	}
	oracle := &ProbeResultOracle{
		results:  results,
		toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
	}
	if err := oracle.ValidatePlan(measured); err != nil {
		t.Fatalf("validate independently measured variant results: %v", err)
	}
	reader := &selectedSourceResultReader{results: oracle}
	replay, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	replayScopes := testSourceScriptOutputDualScopes(t, root, replay, nil)
	lookup := NewSelectedSourceOutputProbeLookup(measured, reader)
	value, concrete, recognized, references, err := replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, nil, nil, nil, lookup,
	)
	if err != nil || !concrete || !recognized || value != exact || !slices.Equal(references, baseRefs) ||
		!slices.Equal(reader.reads, baseRefs) {
		t.Fatalf("matched variant replay = (%q,%t,%t,%v,%v), reads=%v, want matching target/host results", value, concrete, recognized, references, err, reader.reads)
	}
	value, concrete, recognized, references, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, nil, nil, nil, lookup, true,
	)
	if err != nil || !concrete || !recognized || value != exact || !slices.Equal(references, baseRefs) ||
		!slices.Equal(reader.reads, append(slices.Clone(baseRefs), baseRefs...)) {
		t.Fatalf("next source discovery round = (%q,%t,%t,%v,%v), reads=%v, want exact prior writer replay", value, concrete, recognized, references, err, reader.reads)
	}
	reader.reads = slices.Clone(baseRefs)
	value, concrete, recognized, _, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, nil, nil, nil, oracle,
	)
	if err == nil || !strings.Contains(err.Error(), target) ||
		!strings.Contains(err.Error(), "without its source output plan") || value != "" || concrete || !recognized {
		t.Fatalf("bare measured oracle = (%q,%t,%t,%v), want a source-owned Plan error", value, concrete, recognized, err)
	}
	value, concrete, recognized, _, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, nil, nil, nil,
		NewSelectedSourceOutputProbeLookup(nil, oracle),
	)
	if err == nil || !strings.Contains(err.Error(), target) ||
		!strings.Contains(err.Error(), "no measured source output plan/results") ||
		value != "" || concrete || !recognized {
		t.Fatalf("ordinary source replay without its union plan = (%q,%t,%t,%v), want selected-writer rejection", value, concrete, recognized, err)
	}
	value, concrete, recognized, _, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, nil, nil, nil, oracle, true,
	)
	if err != nil || value != "" || concrete || !recognized {
		t.Fatalf("source-only discovery with prior oracle = (%q,%t,%t,%v), want registration without measured results", value, concrete, recognized, err)
	}
	value, concrete, recognized, _, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, []string{localversion}, extraFiles,
		map[string]string{localversion: "selected-writer-b"}, lookup,
	)
	if err == nil || !strings.Contains(err.Error(), target) ||
		!strings.Contains(err.Error(), "absent from measured source output terminals") ||
		value != "" || concrete || !recognized || !slices.Equal(reader.reads, baseRefs) {
		t.Fatalf("unmeasured variant replay = (%q,%t,%t,%v), reads=%v, want source-owned rejection before result read", value, concrete, recognized, err, reader.reads)
	}
	value, concrete, recognized, references, err = replayScopes.SelectedSourceFilechkOutputText(
		target, recipe, baseline, []string{localversion}, extraFiles,
		map[string]string{localversion: "selected-writer-b"}, lookup, true,
	)
	if err != nil || value != "" || concrete || !recognized || len(references) != 1 ||
		!slices.Equal(reader.reads, baseRefs) {
		t.Fatalf("source-only variant discovery = (%q,%t,%t,%v,%v), reads=%v, want registered requests without an oracle read", value, concrete, recognized, references, err, reader.reads)
	}
}

func TestQuotedSourceOutputRoundsMeasureTwoIndependentSelectedWriters(t *testing.T) {
	_, _, root, first, baseline := quotedSourceVersionFixtureScopes(t)
	const second = "include/config/another.release"
	workload := func(builder *ProbePlanBuilder) *KbuildProbeScopes {
		t.Helper()
		return testSourceScriptOutputDualScopes(t, root, builder, nil)
	}
	firstBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	firstScopes := workload(firstBuilder)
	recipe := func(target string) string {
		return "{\necho \"5.10.270$(sh ${tree:kernel}/scripts/source-version ${tree:kernel})\"\n} > '" + target + "'"
	}
	selectWriter := func(scopes *KbuildProbeScopes, target string, measured ProbeResultLookup) (string, bool, []ProbeReference) {
		t.Helper()
		value, concrete, recognized, refs, err := scopes.SelectedSourceFilechkOutputText(
			target, recipe(target), baseline, nil, nil, nil, measured, true,
		)
		if err != nil || !recognized || len(refs) != 1 {
			t.Fatalf("selected writer %q discovery = (%q,%t,%t,%#v,%v)", target, value, concrete, recognized, refs, err)
		}
		return value, concrete, refs
	}
	value, concrete, firstRefs := selectWriter(firstScopes, first, nil)
	if value != "" || concrete {
		t.Fatalf("first selected writer = (%q,%t,%#v), want pending target/host requests", value, concrete, firstRefs)
	}
	firstPlan, err := firstBuilder.Plan(firstRefs...)
	if err != nil {
		t.Fatal(err)
	}
	results := probeResultMap{}
	const exact = "5.10.270-fixture\n"
	registerResults := func(plan *ProbePlan, refs []ProbeReference) {
		for _, ref := range refs {
			request := plan.Requests[ref.RequestID]
			envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(exact)) + "\n"
			results[ref.NodeID] = ProbeResult{
				Schema: LinuxProbeResultSchema, NodeID: ref.NodeID, RequestID: ref.RequestID,
				Scope: ref.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
				Steps: evaluatedScriptOutputResultSteps(request, envelope),
			}
		}
	}
	registerResults(firstPlan, firstRefs)
	oracle := &ProbeResultOracle{
		results: results, toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
	}
	if err := oracle.ValidatePlan(firstPlan); err != nil {
		t.Fatal(err)
	}
	secondBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	secondScopes := workload(secondBuilder)
	prior := NewSelectedSourceOutputProbeLookup(firstPlan, oracle)
	if value, concrete, refs := selectWriter(secondScopes, first, prior); value != exact || !concrete || !slices.Equal(refs, firstRefs) {
		t.Fatalf("first source writer did not advance next round: (%q,%t,%#v)", value, concrete, refs)
	}
	value, concrete, secondRefs := selectWriter(secondScopes, second, prior)
	if value != "" || concrete || slices.Equal(secondRefs, firstRefs) {
		t.Fatalf("second independent writer did not become the next pending boundary: (%q,%t,%#v)", value, concrete, secondRefs)
	}
	secondPlan, err := secondBuilder.Plan(secondRefs...)
	if err != nil {
		t.Fatal(err)
	}
	union, err := MergeProbePlans([]ProbePlanVariant{
		{Name: "first-round", Plan: firstPlan}, {Name: "second-round", Plan: secondPlan},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(union.Terminal) != 2 {
		t.Fatalf("two selected source writers should retain two target terminals, got %q", union.Terminal)
	}
	registerResults(secondPlan, secondRefs)
	if err := oracle.ValidatePlan(union); err != nil {
		t.Fatal(err)
	}
	finalBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	finalScopes := workload(finalBuilder)
	for _, target := range []string{first, second} {
		value, concrete, recognized, _, err := finalScopes.SelectedSourceFilechkOutputText(
			target, recipe(target), baseline, nil, nil, nil, NewSelectedSourceOutputProbeLookup(union, oracle),
		)
		if err != nil || !recognized || value != exact || !concrete {
			t.Fatalf("complete source-output plan cannot replay selected writer %q: %q/%t", target, value, concrete)
		}
	}
	if _, _, _, _, err := finalScopes.SelectedSourceFilechkOutputText(
		second, recipe(second), baseline, nil, nil, nil, prior,
	); err == nil || !strings.Contains(err.Error(), "absent from measured source output terminals") {
		t.Fatalf("ordinary replay accepted second writer missing from first-round Plan: %v", err)
	}
}
