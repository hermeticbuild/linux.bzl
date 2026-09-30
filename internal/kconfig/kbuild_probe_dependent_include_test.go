package kconfig

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type kbuildProbeIncludeTargetSnapshot struct {
	Kind      string
	Target    string
	Condition KbuildCondition
}

type kbuildProbeIncludeRuleSnapshot struct {
	Targets       []string
	Prerequisites []string
	Recipe        []string
	Condition     KbuildCondition
}

type kbuildProbeIncludeSnapshot struct {
	Scalar    string
	After     string
	Includes  []string
	Generated []kbuildProbeIncludeTargetSnapshot
	Rules     []kbuildProbeIncludeRuleSnapshot
}

func parseKbuildProbeIncludeSnapshot(
	scopes *KbuildProbeScopes,
	root string,
	probeOptions KbuildProbeWorkloadOptions,
) (kbuildProbeIncludeSnapshot, error) {
	options, err := scopes.Options("target", KbuildOptions{
		RootDir:    root,
		WorkingDir: root,
		Variables: map[string]string{
			"CC": probeOptions.Target.Tools["cc"], "RUSTC": probeOptions.Target.Tools["rustc"],
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureVariables:        []string{"SCALAR", "AFTER"},
	})
	if err != nil {
		return kbuildProbeIncludeSnapshot{}, err
	}
	parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
	if err != nil {
		return kbuildProbeIncludeSnapshot{}, err
	}
	snapshot := kbuildProbeIncludeSnapshot{
		Scalar: parsed.Variables["SCALAR"],
		After:  parsed.Variables["AFTER"],
	}
	for _, include := range parsed.Includes {
		snapshot.Includes = append(snapshot.Includes, include.Path)
	}
	for _, generated := range parsed.Generated {
		snapshot.Generated = append(snapshot.Generated, kbuildProbeIncludeTargetSnapshot{
			Kind: generated.Kind, Target: generated.Target, Condition: generated.Condition,
		})
	}
	for _, rule := range parsed.Rules {
		snapshot.Rules = append(snapshot.Rules, kbuildProbeIncludeRuleSnapshot{
			Targets:       slices.Clone(rule.Targets),
			Prerequisites: slices.Clone(rule.Prerequisites),
			Recipe:        slices.Clone(rule.Recipe),
			Condition:     rule.Condition,
		})
	}
	return snapshot, nil
}

func probeIncludeOracle(t *testing.T, plan *ProbePlan, guard bool) *ProbeResultOracle {
	t.Helper()
	roots := writeKbuildProbeResultsByNode(t, plan, func(_ int, _ ProbePlanNode) bool {
		return guard
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	return oracle
}

func TestKbuildProbeDependentIncludeDefersUntilConcreteReplay(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", linearFilterCompilerFixture+`
guard := $(call cc-option,-fprobe-include)
ifneq ($(guard),)
include child.mk
endif
AFTER := after-root
`)
	mustWriteSource(t, root, "child.mk", `
SCALAR := child-scalar
always-y += generated.h
generated.h: input.h
	generate $@ from $<
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: target}
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeIncludeSnapshot, error) {
		return parseKbuildProbeIncludeSnapshot(scopes, root, probeOptions)
	}

	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("guarded include discovery has %d probe nodes, want its condition probe: %#v", got, discovery.Plan.Nodes)
	}
	guardRequest := discovery.Plan.Requests[discovery.Plan.Nodes[0].RequestID]
	if guardRequest.Outcome.Kind != "boolean" || len(guardRequest.Steps) != 1 ||
		guardRequest.Steps[0].Tool != "cc" ||
		!strings.Contains(strings.Join(guardRequest.Steps[0].Arguments, " "), "-fprobe-include") {
		t.Fatalf("guarded include condition request = %#v", guardRequest)
	}
	wantSkipped := kbuildProbeIncludeSnapshot{After: "after-root"}
	if !reflect.DeepEqual(discovery.Value, wantSkipped) {
		t.Fatalf("guarded include discovery parsed deferred contents:\n got: %#v\nwant: %#v", discovery.Value, wantSkipped)
	}

	falseReplay, err := EvaluateKbuildProbeWorkload(probeOptions, probeIncludeOracle(t, discovery.Plan, false), workload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(falseReplay.Value, wantSkipped) {
		t.Fatalf("false guarded include replay = %#v, want skipped %#v", falseReplay.Value, wantSkipped)
	}
	if !reflect.DeepEqual(falseReplay.Plan, discovery.Plan) {
		t.Fatalf("false guarded include replay changed the probe plan:\n got: %#v\nwant: %#v", falseReplay.Plan, discovery.Plan)
	}

	trueReplay, err := EvaluateKbuildProbeWorkload(probeOptions, probeIncludeOracle(t, discovery.Plan, true), workload)
	if err != nil {
		t.Fatal(err)
	}
	wantIncluded := kbuildProbeIncludeSnapshot{
		Scalar:   "child-scalar",
		After:    "after-root",
		Includes: []string{"child.mk"},
		Generated: []kbuildProbeIncludeTargetSnapshot{{
			Kind: "always", Target: "generated.h", Condition: KbuildCondition{Kind: "const", State: "y"},
		}},
		Rules: []kbuildProbeIncludeRuleSnapshot{{
			Targets:       []string{"generated.h"},
			Prerequisites: []string{"input.h"},
			Recipe:        []string{"generate $@ from $<"},
			Condition:     KbuildCondition{Kind: "const", State: "y"},
		}},
	}
	if !reflect.DeepEqual(trueReplay.Value, wantIncluded) {
		t.Fatalf("true guarded include replay did not parse concrete contents:\n got: %#v\nwant: %#v", trueReplay.Value, wantIncluded)
	}
	if !reflect.DeepEqual(trueReplay.Plan, discovery.Plan) {
		t.Fatalf("true guarded include replay changed the probe plan:\n got: %#v\nwant: %#v", trueReplay.Plan, discovery.Plan)
	}
}

func TestKbuildProbeDependentIncludeRejectsReplayOnlyProbe(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", linearFilterCompilerFixture+`
guard := $(call cc-option,-fprobe-include)
ifneq ($(guard),)
include probe-child.mk
endif
AFTER := after-root
`)
	mustWriteSource(t, root, "probe-child.mk", `
export replay_only_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name replay_only --crate-type proc-macro - </dev/null)
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: target}
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeIncludeSnapshot, error) {
		return parseKbuildProbeIncludeSnapshot(scopes, root, probeOptions)
	}
	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("discovery plan has %d nodes, want only the include guard: %#v", got, discovery.Plan.Nodes)
	}

	_, err = EvaluateKbuildProbeWorkload(probeOptions, probeIncludeOracle(t, discovery.Plan, true), workload)
	if err == nil || !strings.Contains(err.Error(), "validate replayed Kbuild probe plan") ||
		!strings.Contains(err.Error(), "missing result for probe node") {
		t.Fatalf("replay-only include probe error = %v, want oracle plan validation failure", err)
	}
}
