package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestGraphGuardConvergenceNeedsIdenticalMeasuredResults(t *testing.T) {
	root := t.TempDir()
	plan := familyCompilerGuardPlanForTest(t, "source guard")
	makeEvidence := func(round string) kbuildSelectedProbeRoundEvidence {
		t.Helper()
		evidence := kbuildSelectedProbeRoundEvidence{
			plan: plan, hostResults: filepath.Join(root, round, "host"),
			targetResults: filepath.Join(root, round, "target"),
		}
		for _, directory := range []string{evidence.hostResults, evidence.targetResults} {
			if err := os.MkdirAll(filepath.Join(directory, "results"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, node := range plan.Nodes {
			directory := evidence.targetResults
			if node.Scope == "host" {
				directory = evidence.hostResults
			}
			if err := os.WriteFile(filepath.Join(directory, "results", node.ID+".json"), []byte(node.RequestID), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return evidence
	}
	earlier, current := makeEvidence("earlier"), makeEvidence("current")
	converged, err := sameKbuildSelectedProbeRoundEvidence("source graph guard", earlier, current)
	if err != nil || !converged {
		t.Fatalf("same guard plan and results = %t, %v; want convergence", converged, err)
	}
	changed := filepath.Join(earlier.targetResults, "results", plan.Nodes[0].ID+".json")
	if err := os.WriteFile(changed, []byte("different measured answer"), 0o644); err != nil {
		t.Fatal(err)
	}
	converged, err = sameKbuildSelectedProbeRoundEvidence("source graph guard", earlier, current)
	if err != nil || converged {
		t.Fatalf("same guard plan with changed result = %t, %v; want another discovery", converged, err)
	}
	if err := os.Remove(changed); err != nil {
		t.Fatal(err)
	}
	if _, err := sameKbuildSelectedProbeRoundEvidence("source graph guard", earlier, current); err == nil {
		t.Fatal("missing earlier guard result was accepted")
	}
}

func TestKbuildEmptyFeatureRoundTripPreservesMeasuredPlanEvidence(t *testing.T) {
	withProducer := familyCompilerGuardPlanForTest(t, "selected feature")
	empty := &kconfig.ProbePlan{
		Toolsets: withProducer.Toolsets,
		Requests: map[string]kconfig.ProbeRequest{},
	}
	firstPath := filepath.Join(t.TempDir(), "first")
	if err := empty.Write(firstPath); err != nil {
		t.Fatal(err)
	}
	first, err := kconfig.ReadProbePlan(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nextKbuildSelectedProbeRound(first, empty, "feature-dump", false)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(t.TempDir(), "second")
	if err := second.Write(secondPath); err != nil {
		t.Fatal(err)
	}
	measured, err := kconfig.ReadProbePlan(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEarlierKbuildSelectedProbeRound(first, measured, "feature-dump"); err != nil {
		t.Fatalf("empty canonical feature round lost evidence: %v", err)
	}
	if !kconfig.SameProbePlanContent(first, measured) {
		t.Fatal("empty feature plan changed after marker-tree round trip")
	}
	if err := validateEarlierKbuildSelectedProbeRound(withProducer, measured, "feature-dump"); err == nil {
		t.Fatal("a genuinely lost feature request and terminal passed monotonic validation")
	}
}

func TestPairedSourceFeatureConvergenceNeedsIdenticalSealedResults(t *testing.T) {
	root := t.TempDir()
	source := familyCompilerGuardPlanForTest(t, "source-selected writer")
	feature := familyCompilerGuardPlanForTest(t, "source-selected feature")
	makeEvidence := func(stage, round string, plan *kconfig.ProbePlan) kbuildSelectedProbeRoundEvidence {
		t.Helper()
		evidence := kbuildSelectedProbeRoundEvidence{
			plan:          plan,
			hostResults:   filepath.Join(root, stage, round, "host"),
			targetResults: filepath.Join(root, stage, round, "target"),
		}
		for _, directory := range []string{evidence.hostResults, evidence.targetResults} {
			if err := os.MkdirAll(filepath.Join(directory, "results"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, node := range plan.Nodes {
			folder := evidence.targetResults
			if node.Scope == "host" {
				folder = evidence.hostResults
			}
			if err := os.WriteFile(filepath.Join(folder, "results", node.ID+".json"), []byte(node.RequestID), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return evidence
	}
	sourceEarlier := makeEvidence("source", "earlier", source)
	sourcePrior := makeEvidence("source", "prior", source)
	featureEarlier := makeEvidence("feature", "earlier", feature)
	featurePrior := makeEvidence("feature", "prior", feature)
	converged, err := sameKbuildSelectedPairedRounds(sourceEarlier, sourcePrior, featureEarlier, featurePrior)
	if err != nil || !converged {
		t.Fatalf("unchanged paired plans and results = %t, %v; want convergence", converged, err)
	}
	featurePath := filepath.Join(featureEarlier.targetResults, "results", feature.Nodes[0].ID+".json")
	if err := os.WriteFile(featurePath, []byte("changed measured answer"), 0o644); err != nil {
		t.Fatal(err)
	}
	converged, err = sameKbuildSelectedPairedRounds(sourceEarlier, sourcePrior, featureEarlier, featurePrior)
	if err != nil || converged {
		t.Fatalf("same plans but changed feature result = %t, %v; want continued discovery", converged, err)
	}
	if err := os.WriteFile(featurePath, []byte(feature.Nodes[0].RequestID), 0o644); err != nil {
		t.Fatal(err)
	}
	changedFeature, err := nextKbuildSelectedProbeRound(feature, familyCompilerGuardPlanForTest(t, "new feature"), "feature-dump", false)
	if err != nil {
		t.Fatal(err)
	}
	featurePrior.plan = changedFeature
	converged, err = sameKbuildSelectedPairedRounds(sourceEarlier, sourcePrior, featureEarlier, featurePrior)
	if err != nil || converged {
		t.Fatalf("new feature request = %t, %v; want continued discovery", converged, err)
	}
	featurePrior.plan = feature
	if err := os.Remove(featurePath); err != nil {
		t.Fatal(err)
	}
	if _, err := sameKbuildSelectedPairedRounds(sourceEarlier, sourcePrior, featureEarlier, featurePrior); err == nil {
		t.Fatal("missing earlier measured feature result was accepted as a converged pair")
	}
}

func TestKbuildGraphGuardRoundsMeasureSelectedFrontierAndFailOnExhaustion(t *testing.T) {
	stack := familyCompilerGuardPlanForTest(t, "stack alignment")
	record := familyCompilerGuardPlanForTest(t, "record mcount after measured alignment")
	first, err := nextKbuildGraphGuardRound(nil, stack, false)
	if err != nil || !kconfig.SameProbePlanContent(first, stack) {
		t.Fatalf("initial source guard round = %v, %v", first, err)
	}
	if _, err := nextKbuildGraphGuardRound(first, record, true); err == nil || !strings.Contains(err.Error(), "rounds exhausted") {
		t.Fatalf("new terminal at bounded last round error = %v", err)
	}
	second, err := nextKbuildGraphGuardRound(first, record, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range append(slices.Clone(first.Terminal), record.Terminal...) {
		if !slices.Contains(second.Terminal, id) {
			t.Fatalf("union discarded measured source guard %s", id)
		}
	}
	if err := validateEarlierKbuildGraphGuardRound(first, second); err != nil {
		t.Fatalf("measured round lost predecessor: %v", err)
	}
	if err := validateEarlierKbuildGraphGuardRound(second, first); err == nil {
		t.Fatal("earlier source guard absent from prior measured round")
	}
	converged, err := nextKbuildGraphGuardRound(second, record, true)
	if err != nil || !kconfig.SameProbePlanContent(second, converged) {
		t.Fatalf("converged source guard = %v, %v", converged, err)
	}
}

func TestChildGraphGuardResumesAfterGeneratedMakefileSelection(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"Kconfig": "mainmenu \"guard fixture\"\n",
		"Makefile": `
SRCARCH := x86
UTS_MACHINE := x86_64
.PHONY: all
all: selected.mk
	$(MAKE) -f $(srctree)/$(file <selected.mk) all
selected.mk:
	echo child.mk > $@
`,
		"child.mk": `
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name first --crate-type proc-macro - </dev/null)
ifneq ($(filter %.dtb,$(name)),)
include $(srctree)/dtb.mk
endif
.PHONY: all
all: base.out $(extra)
%.out:
	touch $@
`,
		"dtb.mk": "extra := dtb.out\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const identity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootstrap, err := newLinuxCompilerBootstrapPlan(identity, identity)
	if err != nil {
		t.Fatal(err)
	}
	target, err := kconfig.ParseLinuxCompilerBootstrapResult(testLinuxCompilerBootstrapResult(t, bootstrap.target, identity, true), "target", identity)
	if err != nil {
		t.Fatal(err)
	}
	host, err := kconfig.ParseLinuxCompilerBootstrapResult(testLinuxCompilerBootstrapResult(t, bootstrap.host, identity, true), "host", identity)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := kconfig.Parse(t.Context(), strings.NewReader("mainmenu \"guard fixture\"\n"), "Kconfig", kconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	targetContract, hostContract := testKbuildContracts(map[string]configuredKbuildAction{
		"cc": {Path: "/configured/cc"}, "rustc": {Path: "/configured/rustc"},
	}, target)
	empty := &kconfig.ProbePlan{Toolsets: map[string]string{"target": identity, "host": identity}, Requests: map[string]kconfig.ProbeRequest{}}
	emptyOracle, err := kconfig.NewProbeResultOracleFromTrees(nil, empty.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	guards, err := kconfig.NewKbuildGraphGuardResults(empty, emptyOracle)
	if err != nil {
		t.Fatal(err)
	}
	options := linuxKbuildProbeOptions{
		tree: tree, nativeConfig: &nativeConfigProjection{files: nativeConfigFixtureForTest("", "", "")},
		rootPath: filepath.Join(root, "Kconfig"), kbuildPath: filepath.Join(root, "Makefile"),
		variables:    map[string]string{"ARCH": "x86", "SRCARCH": "x86"},
		entryTargets: []string{"all"}, selectedProductsOnly: true,
		target:      sourceDerivedLinuxTarget{Arch: "x86", Srcarch: "x86", UTSMachine: "x86_64"},
		targetFacts: target, hostFacts: host, targetContract: targetContract, hostContract: hostContract,
		graphGuardResults: guards, featureDumpDiscoveryOnly: true,
		normalizeConfigValue: func(value string) (string, error) { return value, nil },
	}
	options.featureDumpDiscoveryOnly = false
	options.sourceOutputDiscoveryOnly = true
	sourcePass, err := evaluateLinuxKbuildProbes(options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourcePass.Value.sourceOutputRequestIDs) != 0 || len(sourcePass.Value.featureDumpRequestIDs) != 0 {
		t.Fatal("source pass did not yield its child guard to the feature pass")
	}
	options.sourceOutputDiscoveryOnly = false
	options.featureDumpDiscoveryOnly = true
	discovery, err := evaluateLinuxKbuildProbes(options, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := kconfig.SelectProbePlanTerminals(discovery.Plan, discovery.Value.featureDumpRequestIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Terminal) == 0 {
		t.Fatal("recursive include guard was omitted from the measured frontier")
	}
	options.featureDumpDiscoveryOnly = false
	var missing *kconfig.UnmeasuredKbuildGraphGuardError
	if _, err := evaluateLinuxKbuildProbes(options, nil); !errors.As(err, &missing) {
		t.Fatalf("final planning without child guard results = %v; want unresolved guard", err)
	}
	for _, filename := range []string{"libfirst.so", "board.dtb"} {
		t.Run(filename, func(t *testing.T) {
			resultRoot := t.TempDir()
			nodes := map[string]kconfig.ProbePlanNode{}
			for _, node := range plan.Nodes {
				nodes[node.ID] = node
			}
			results := map[string]kconfig.ProbeResult{}
			var measure func(string) kconfig.ProbeResult
			measure = func(id string) kconfig.ProbeResult {
				if result, ok := results[id]; ok {
					return result
				}
				node := nodes[id]
				request := plan.Requests[node.RequestID]
				result := kconfig.ProbeResult{Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, ToolsetIdentity: identity, Kind: request.Outcome.Kind}
				inputs := map[string]kconfig.ProbeResult{}
				for i, input := range node.Inputs {
					inputs[fmt.Sprintf("%08d", i)] = measure(input)
				}
				switch {
				case len(request.Steps) == 1 && request.Outcome.Kind == "text":
					result.Text = filename
					result.Steps = []kconfig.ProbeStepResult{{Name: request.Steps[0].Name, Status: "success", Stdout: filename + "\n"}}
				case len(request.Steps) == 0 && request.Outcome.Kind == "text":
					result.Text, err = kconfig.RenderProbeDependencyFragments(request.Outcome.Fragments, inputs)
				case len(request.Steps) == 0 && request.Outcome.Kind == "boolean" && request.Outcome.Predicate != nil:
					result.Boolean = new(bool)
					*result.Boolean, err = kconfig.EvaluateProbeResultPredicate(*request.Outcome.Predicate, inputs)
				default:
					t.Fatalf("unexpected child guard request %#v", request)
				}
				if err != nil {
					t.Fatal(err)
				}
				results[node.ID] = result
				writeTestProbeResult(t, resultRoot, result)
				return result
			}
			for _, node := range plan.Nodes {
				measure(node.ID)
			}
			oracle, err := kconfig.NewProbeResultOracleFromTrees(map[string]string{"target": resultRoot}, plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			measured, err := kconfig.NewKbuildGraphGuardResults(plan, oracle)
			if err != nil {
				t.Fatal(err)
			}
			options.featureDumpResults = measured
			replay, err := evaluateLinuxKbuildProbes(options, oracle)
			if err != nil {
				t.Fatal(err)
			}
			included := false
			for _, node := range replay.Value.actionPlan.Nodes {
				for _, output := range node.Outputs {
					included = included || output.Path == "dtb.out"
				}
			}
			if included != (filename == "board.dtb") {
				t.Fatalf("dtb.out selected = %t for compiler filename %q", included, filename)
			}
		})
	}
}
