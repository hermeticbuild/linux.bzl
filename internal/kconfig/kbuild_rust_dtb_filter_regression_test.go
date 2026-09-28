package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeKbuildRustDTBFilterResults(
	t *testing.T,
	plan *ProbePlan,
	firstName string,
	secondName string,
) (map[string]string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}

	leafText := []string{firstName, secondName}
	results := make(map[string]ProbeResult, len(plan.Nodes))
	materialized := ""
	for index, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope],
		}
		switch index {
		case 0, 1:
			result.Kind = "text"
			result.Text = leafText[index]
			result.Steps = []ProbeStepResult{{
				Name: request.Steps[0].Name, Status: "success", ExitCode: 0,
				Stdout: leafText[index] + "\n",
			}}
		case 2:
			inputs := make(map[string]ProbeResult, len(node.Inputs))
			for ordinal, input := range node.Inputs {
				inputs[fmt.Sprintf("%08d", ordinal)] = results[input]
			}
			var err error
			materialized, err = RenderProbeDependencyFragments(request.Outcome.Fragments, inputs)
			if err != nil {
				t.Fatal(err)
			}
			result.Kind = "text"
			result.Text = materialized
		case 3:
			value := results[node.Inputs[0]].Text == ""
			result.Kind = "boolean"
			result.Boolean = &value
		default:
			t.Fatalf("unexpected DTB filter plan node %d: %#v", index, node)
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		results[node.ID] = result
	}
	return map[string]string{"target": root}, materialized
}

func TestKbuildRustTargetsSelectDTBRulesThroughDerivedTextProbe(t *testing.T) {
	const source = `
first_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name first --crate-type proc-macro - </dev/null)
second_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name second --crate-type proc-macro - </dev/null)
targets := $(addprefix rust/,$(first_name) $(second_name))
SELECTED := false
ifneq ($(need-dtbslist)$(dtb-y)$(dtb-)$(filter %.dtb %.dtb.o %.dtbo.o,$(targets)),)
SELECTED := true
endif
`

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	target.ScriptEnvironment = map[string]string{
		"CC":              target.Tools["cc"],
		"RUSTC":           target.Tools["rustc"],
		"RUSTC_BOOTSTRAP": "1",
	}
	opts := KbuildProbeWorkloadOptions{Target: target}
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"RUSTC": opts.Target.Tools["rustc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"SELECTED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["SELECTED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value) {
		t.Fatalf("DTB condition discovery value = %q, want symbolic boolean", discovery.Value)
	}
	if got := len(discovery.Plan.Nodes); got != 4 {
		t.Fatalf("DTB condition plan has %d nodes, want two text leaves, derived text, and predicate: %#v", got, discovery.Plan.Nodes)
	}
	leafNodes := discovery.Plan.Nodes[:2]
	derivedNode := discovery.Plan.Nodes[2]
	predicateNode := discovery.Plan.Nodes[3]
	if len(leafNodes[0].Inputs) != 0 || len(leafNodes[1].Inputs) != 0 {
		t.Fatalf("rustc text leaves have inputs: %#v", leafNodes)
	}
	if want := []string{leafNodes[0].ID, leafNodes[1].ID}; !slices.Equal(derivedNode.Inputs, want) {
		t.Fatalf("derived text inputs = %q, want rustc leaves %q", derivedNode.Inputs, want)
	}
	if want := []string{derivedNode.ID}; !slices.Equal(predicateNode.Inputs, want) {
		t.Fatalf("empty predicate inputs = %q, want derived text %q", predicateNode.Inputs, want)
	}

	for index, crateName := range []string{"first", "second"} {
		request := discovery.Plan.Requests[leafNodes[index].RequestID]
		wantArguments := []string{
			"--print", "file-names", "--crate-name", crateName,
			"--crate-type", "proc-macro", "-",
		}
		if request.InputCount != 0 || len(request.Steps) != 1 ||
			request.Steps[0].Name != "print-file-names" || request.Steps[0].Tool != "rustc" ||
			!slices.Equal(request.Steps[0].Arguments, wantArguments) ||
			!slices.Equal(request.Steps[0].AuxiliaryTools, []string{"cc"}) ||
			!reflect.DeepEqual(request.Steps[0].Environment, map[string]string{
				"ARCH": "x86", "CC": "${tool:cc}", "MAKEFLAGS": "", "RUSTC": "${tool:rustc}",
				"RUSTC_BOOTSTRAP": "1", "SRCARCH": "x86",
			}) || len(request.SourceRoots) != 0 ||
			request.Outcome.Kind != "text" || request.Outcome.Step != "print-file-names" ||
			request.Outcome.Stream != "stdout" || !request.Outcome.TrimSpace || !request.Outcome.PathComponent ||
			!request.Outcome.RequireSuccess {
			t.Fatalf("rustc text leaf %d = %#v", index, request)
		}
	}

	derivedRequest := discovery.Plan.Requests[derivedNode.RequestID]
	wantFragments := []ProbeValueFragment{{
		Fragments: []ProbeValueFragment{
			{Value: "rust/"},
			{Value: "${result:00000000.text}"},
			{Value: " rust/"},
			{Value: "${result:00000001.text}"},
		},
		Transforms: []ProbeValueTransform{{
			Function: "filter", Arguments: []string{"%.dtb %.dtb.o %.dtbo.o", ""}, InputArgument: 1,
		}},
	}}
	if derivedRequest.InputCount != 2 || len(derivedRequest.Steps) != 0 ||
		derivedRequest.Outcome.Kind != "text" || !reflect.DeepEqual(derivedRequest.Outcome.Fragments, wantFragments) {
		t.Fatalf("derived DTB filter request = %#v, want exact zero-step fragment reduction %#v", derivedRequest, wantFragments)
	}
	predicateRequest := discovery.Plan.Requests[predicateNode.RequestID]
	wantPredicate := &ProbePredicate{Operator: "result-text-empty", Result: "00000000"}
	if predicateRequest.InputCount != 1 || len(predicateRequest.Steps) != 0 ||
		predicateRequest.Outcome.Kind != "boolean" || !reflect.DeepEqual(predicateRequest.Outcome.Predicate, wantPredicate) {
		t.Fatalf("DTB empty predicate request = %#v, want exact zero-step predicate %#v", predicateRequest, wantPredicate)
	}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		data, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), linuxProbeSymbolPrefix) {
			t.Fatalf("request %s leaked a planner token: %s", node.ID, data)
		}
	}
	for _, request := range []ProbeRequest{derivedRequest, predicateRequest} {
		if len(request.Steps) != 0 {
			t.Fatalf("pure reduction unexpectedly has process steps: %#v", request)
		}
		data, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"tool"`) {
			t.Fatalf("pure reduction unexpectedly names a tool: %s", data)
		}
	}

	for _, test := range []struct {
		name             string
		firstName        string
		secondName       string
		wantMaterialized string
		wantSelected     string
	}{
		{
			name: "non-DTB rust filenames", firstName: "libfirst.so", secondName: "libsecond.rlib",
			wantMaterialized: "", wantSelected: "false",
		},
		{
			name: ".dtb", firstName: "board.dtb", secondName: "libsecond.rlib",
			wantMaterialized: "rust/board.dtb", wantSelected: "true",
		},
		{
			name: ".dtb.o", firstName: "libfirst.so", secondName: "board.dtb.o",
			wantMaterialized: "rust/board.dtb.o", wantSelected: "true",
		},
		{
			name: ".dtbo.o", firstName: "overlay.dtbo.o", secondName: "libsecond.rlib",
			wantMaterialized: "rust/overlay.dtbo.o", wantSelected: "true",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots, materialized := writeKbuildRustDTBFilterResults(
				t, discovery.Plan, test.firstName, test.secondName,
			)
			if materialized != test.wantMaterialized {
				t.Fatalf("materialized DTB filter = %q, want %q", materialized, test.wantMaterialized)
			}
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.wantSelected {
				t.Fatalf("DTB selection replay = %q, want %q", replay.Value, test.wantSelected)
			}
			if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
				t.Fatalf("DTB replay plan changed:\n got: %#v\nwant: %#v", replay.Plan, discovery.Plan)
			}
		})
	}
}
