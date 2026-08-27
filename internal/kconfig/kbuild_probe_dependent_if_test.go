package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type kbuildProbeDependentIfSnapshot struct {
	Result      string
	Structure   string
	ActionRoles []KbuildActionRoleRef
}

func writeKbuildProbeDependentIfResults(t *testing.T, plan *ProbePlan, filename string) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	results := map[string]ProbeResult{}
	for index, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope],
		}
		switch index {
		case 0:
			result.Kind = "text"
			result.Text = filename
			result.Steps = []ProbeStepResult{{
				Name: request.Steps[0].Name, Status: "success", ExitCode: 0,
				Stdout: filename + "\n",
			}}
		case 1:
			inputs := map[string]ProbeResult{"00000000": results[node.Inputs[0]]}
			materialized, err := RenderProbeDependencyFragments(request.Outcome.Fragments, inputs)
			if err != nil {
				t.Fatal(err)
			}
			result.Kind = "text"
			result.Text = materialized
		case 2:
			value := results[node.Inputs[0]].Text == ""
			result.Kind = "boolean"
			result.Boolean = &value
		default:
			t.Fatalf("unexpected probe-dependent Make if node %d: %#v", index, node)
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
	return map[string]string{"target": root}
}

func TestKbuildProbeDependentIfRetainsDynamicCommandBranch(t *testing.T) {
	// This is the nested selection shape used by scripts/Kbuild.include's
	// if_changed_dep helper. The command check is arbitrary measured text; once
	// classified it is a finite boolean, while cmd_rustc_procmacro still embeds
	// the compiler-reported proc-macro filename in the selected command branch.
	const source = `
cmd_rustc_procmacro = $(RUSTC) --emit=link=rust/$(name)
cmd-check = $(filter-out cached,$(strip $(name)))
if_changed_dep = $(if $(cmd-check),$(cmd_rustc_procmacro),@:)
RESULT := $(if_changed_dep)
result: FORCE
	printf '%s' '$(if_changed_dep)' > $@
`

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	opts := KbuildProbeWorkloadOptions{Target: target}
	var discoveryScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeDependentIfSnapshot, error) {
		if discoveryScopes == nil {
			discoveryScopes = scopes
		}
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"RUSTC": KbuildActionRoleToken("target", "rustc"),
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"RESULT"},
			CaptureTargetEvaluator:  true,
		})
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		// The real rust/Makefile has already evaluated its deferred simple
		// libmacros_name definition before target-context recipe expansion.
		// Inject that measured value here so the branch is expansion-pure just
		// as it is at the failing upstream boundary.
		name, err := options.Shell("MAKEFLAGS= " + KbuildActionRoleToken("target", "rustc") + " --print file-names --crate-name macros --crate-type proc-macro - </dev/null")
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		options.Variables["name"] = name
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		result, err := options.ResolveSymbolic(parsed.Variables["RESULT"])
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		structure, err := options.ResolveSymbolicStructure(parsed.Variables["RESULT"])
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		profile, err := NewCompactKbuildProfile("build:probe-if", "Makefile", "", parsed)
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "result")
		if err != nil {
			return kbuildProbeDependentIfSnapshot{}, err
		}
		if !selected {
			return kbuildProbeDependentIfSnapshot{}, fmt.Errorf("probe-dependent Make if target was not selected")
		}
		return kbuildProbeDependentIfSnapshot{
			Result: result, Structure: structure, ActionRoles: effects.ActionRoles,
		}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 3 {
		t.Fatalf("probe-dependent Make if plan has %d nodes, want text, derived text, and predicate: %#v", got, discovery.Plan.Nodes)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Result) {
		t.Fatalf("probe-dependent Make if discovery = %q, want one finite selection", discovery.Value.Result)
	}
	if discovery.Value.Structure != discovery.Value.Result {
		t.Fatalf(
			"probe-dependent Make if discovery structure = %q, want unchanged selection %q",
			discovery.Value.Structure, discovery.Value.Result,
		)
	}
	_, selection, ok := discoveryScopes.symbolOwner(discovery.Value.Result)
	if !ok || selection.kind != "selection" || len(selection.selectionInputs) != 1 || len(selection.selectionValues) != 2 {
		t.Fatalf("probe-dependent Make if symbol = %#v, want one-input finite selection", selection)
	}
	branchText := strings.Join(selection.selectionValues, "\n")
	if !strings.Contains(branchText, KbuildActionRoleToken("target", "rustc")) ||
		!linuxProbeSymbolPattern.MatchString(branchText) || !strings.Contains(branchText, "@:") {
		t.Fatalf("probe-dependent Make if branches = %q, want dynamic rustc command and no-op", selection.selectionValues)
	}
	for index, wantKind := range []string{"text", "text", "boolean"} {
		request := discovery.Plan.Requests[discovery.Plan.Nodes[index].RequestID]
		if request.Outcome.Kind != wantKind {
			t.Fatalf("probe-dependent Make if node %d outcome = %q, want %q", index, request.Outcome.Kind, wantKind)
		}
	}
	wantRoles := []KbuildActionRoleRef{{Scope: "target", Role: "rustc"}}
	if !reflect.DeepEqual(discovery.Value.ActionRoles, wantRoles) {
		t.Fatalf("probe-dependent Make if discovery action roles = %#v, want %#v", discovery.Value.ActionRoles, wantRoles)
	}

	for _, test := range []struct {
		filename string
		want     string
	}{
		{filename: "cached", want: "@:"},
		{
			filename: "libmacros.so",
			want:     fmt.Sprintf("%s --emit=link=rust/libmacros.so", KbuildActionRoleToken("target", "rustc")),
		},
	} {
		t.Run(test.filename, func(t *testing.T) {
			roots := writeKbuildProbeDependentIfResults(t, discovery.Plan, test.filename)
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value.Result != test.want {
				t.Fatalf("probe-dependent Make if replay = %q, want %q", replay.Value.Result, test.want)
			}
			if replay.Value.Structure != test.want {
				t.Fatalf(
					"probe-dependent Make if structural replay = %q, want %q",
					replay.Value.Structure, test.want,
				)
			}
			if !reflect.DeepEqual(replay.Value.ActionRoles, wantRoles) {
				t.Fatalf("probe-dependent Make if replay action roles = %#v, want %#v", replay.Value.ActionRoles, wantRoles)
			}
			if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
				t.Fatal("probe-dependent Make if replay changed the probe plan")
			}
		})
	}
}
