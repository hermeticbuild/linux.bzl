package kconfig

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestKbuildGraphGuardDiscoveryMeasuresChangedCompilerRequestAfterAlignment(t *testing.T) {
	// The first compiler result changes KBUILD_CFLAGS before a later source
	// ifeq calls cc-option-yn. The latter request has a different input DAG
	// once the first result is sealed, even though its direct flag is the same.
	const source = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(KBUILD_CFLAGS) $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
cc-option-yn = $(if $(call cc-option,$(1)),y,n)
KBUILD_CFLAGS :=
stack_alignment := $(call cc-option,-mpreferred-stack-boundary=3)
ifeq ($(stack_alignment),-mpreferred-stack-boundary=3)
KBUILD_CFLAGS += -mpreferred-stack-boundary=3
endif
CC_FLAGS_FTRACE :=
ifeq ($(call cc-option-yn,-mrecord-mcount),y)
CC_FLAGS_FTRACE += -mrecord-mcount
export CC_USING_RECORD_MCOUNT := 1
endif
export CC_FLAGS_FTRACE
all:
	@echo ready
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	discover := func(prior *KbuildGraphGuardResults) *ProbePlan {
		t.Helper()
		evaluated, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) ([]ProbeReference, error) {
			if err := scopes.InstallGraphGuardResults(prior, true); err != nil {
				return nil, err
			}
			parserOptions, err := scopes.Options("target", KbuildOptions{
				Variables:               map[string]string{"CC": KbuildActionRoleToken("target", "cc")},
				ConfigVariablesComplete: true, MakeVariablesComplete: true,
				CaptureTargetEvaluator: true, SkipExportedVariables: true,
			})
			if err != nil {
				return nil, err
			}
			parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", parserOptions, "")
			if err != nil {
				return nil, err
			}
			profile, err := NewCompactKbuildProfile("root", "Makefile", "", parsed)
			if err != nil {
				return nil, err
			}
			guards := CompactKbuildGraphGuards(profile)
			before, err := scopes.GraphGuardReferences(guards)
			if err != nil {
				return nil, err
			}
			// A later source export changes the active environment before
			// guards captured by the earlier invocation are collected.
			environment := maps.Clone(scopes.evaluators["target"].scriptEnvironment)
			environment["KBUILD_TEST_PHASE"] = "collect-guards"
			activate, err := scopes.BindExactScriptEnvironments(map[string]map[string]string{"target": environment})
			if err != nil {
				return nil, err
			}
			if err := activate(); err != nil {
				return nil, err
			}
			if len(scopes.evaluators["target"].symbols) != 0 {
				t.Fatal("environment activation retained the previous evaluator's symbol cache")
			}
			after, err := scopes.GraphGuardReferences(guards)
			if err != nil {
				return nil, err
			}
			if !slices.Equal(before, after) {
				t.Fatalf("source environment switch changed graph guard references: before=%v after=%v", before, after)
			}
			return after, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		terminals := []string{}
		for _, ref := range evaluated.Value {
			terminals = append(terminals, ref.NodeID)
		}
		selected, err := SelectProbePlanTerminals(evaluated.Plan, terminals)
		if err != nil {
			t.Fatal(err)
		}
		return selected
	}
	record := func(plan *ProbePlan) ProbePlanNode {
		t.Helper()
		for _, node := range plan.Nodes {
			for _, step := range plan.Requests[node.RequestID].Steps {
				if slices.Contains(step.Arguments, "-mrecord-mcount") {
					return node
				}
			}
		}
		t.Fatalf("source graph guard did not retain selected -mrecord-mcount request: terminals=%v", plan.Terminal)
		return ProbePlanNode{}
	}
	first := discover(nil)
	initial := record(first)
	if len(first.Terminal) != 1 || first.Terminal[0] != initial.ID {
		t.Fatalf("source-selected export guard roots = %v; want mrecord node %s", first.Terminal, initial.ID)
	}
	if len(initial.Inputs) == 0 {
		t.Fatal("mrecord guard has no earlier stack-alignment probe inputs")
	}
	roots := writeKbuildProbeResultsByNode(t, first, func(_ int, _ ProbePlanNode) bool { return false })
	oracle, err := NewProbeResultOracleFromTrees(roots, first.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := NewKbuildGraphGuardResults(first, oracle)
	if err != nil {
		t.Fatal(err)
	}
	stackNode := ProbePlanNode{}
	for _, node := range first.Nodes {
		if !slices.Contains(initial.Inputs, node.ID) {
			continue
		}
		for _, step := range first.Requests[node.RequestID].Steps {
			if slices.Contains(step.Arguments, "-mpreferred-stack-boundary=3") {
				stackNode = node
				break
			}
		}
		if stackNode.ID != "" {
			break
		}
	}
	if stackNode.ID == "" || slices.Contains(first.Terminal, stackNode.ID) {
		t.Fatalf("mrecord guard did not depend on a nonterminal stack-alignment probe: %s", stackNode.ID)
	}
	if !sealed.binds([]ProbeReference{{
		NodeID: stackNode.ID, RequestID: stackNode.RequestID, Scope: stackNode.Scope,
		Kind: first.Requests[stackNode.RequestID].Outcome.Kind,
	}}) {
		t.Fatalf("measured dependency %s cannot select a following source guard", stackNode.ID)
	}
	second := discover(sealed)
	changed := record(second)
	if changed.ID == initial.ID {
		t.Fatalf("source-selected mrecord request retained obsolete alignment dependencies %s", changed.ID)
	}
	before, after := first.Requests[initial.RequestID], second.Requests[changed.RequestID]
	if before.InputCount <= after.InputCount || !slices.Contains(first.Terminal, initial.ID) || !slices.Contains(second.Terminal, changed.ID) {
		t.Fatalf("new source guard did not prune measured alignment: first=%d/%s second=%d/%s", before.InputCount, initial.ID, after.InputCount, changed.ID)
	}
	if slices.Contains(first.Terminal, changed.ID) {
		t.Fatal("newly selected request was already an initial measured terminal")
	}
	if _, err := oracle.Result(ProbeReference{NodeID: changed.ID, RequestID: changed.RequestID, Scope: changed.Scope, Kind: after.Outcome.Kind}); err == nil {
		t.Fatal("new request silently reused a different measured compiler answer")
	} else if !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("unexpected unmeasured request error: %v", err)
	}
	// An oracle can contain unused results from another discovery branch. A
	// result outside the selected guard plan has no authority even when its
	// node ID, request, kind, scope, and toolset are individually valid.
	extraRoots := writeKbuildProbeResultsByNode(t, second, func(_ int, _ ProbePlanNode) bool { return false })
	extraOracle, err := NewProbeResultOracleFromTrees(extraRoots, second.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	extraReference := ProbeReference{NodeID: changed.ID, RequestID: changed.RequestID, Scope: changed.Scope, Kind: after.Outcome.Kind}
	extraResult, err := extraOracle.Result(extraReference)
	if err != nil {
		t.Fatal(err)
	}
	oracle.results[changed.ID] = extraResult
	if sealed.binds([]ProbeReference{extraReference}) {
		t.Fatal("a measured compiler result outside the selected guard closure gained source-selection authority")
	}
}
