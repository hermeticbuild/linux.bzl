package kconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type kbuildProbeComputedNameSnapshot struct {
	Target       string
	Existing     string
	Saved        string
	Wrapper      string
	WrapperRoles []KbuildActionRoleRef
	ActionRoles  []KbuildActionRoleRef
	Includes     []string
}

func evaluateKbuildProbeComputedNameSnapshot(
	t *testing.T,
	root string,
	probeOptions KbuildProbeWorkloadOptions,
	oracle *ProbeResultOracle,
) (*KbuildProbeEvaluation[kbuildProbeComputedNameSnapshot], error) {
	t.Helper()
	return EvaluateKbuildProbeWorkload(probeOptions, oracle, func(scopes *KbuildProbeScopes) (kbuildProbeComputedNameSnapshot, error) {
		options, err := scopes.Options("target", KbuildOptions{
			RootDir:         root,
			WorkingDir:      root,
			VirtualFileView: compilerSelectedVirtualFileView(),
			Variables: map[string]string{
				"RUSTC": probeOptions.Target.Tools["rustc"],
			},
			CommandLineVariables: map[string]string{
				"CC": KbuildActionRoleToken("target", "cc"),
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"target", "existing-targets"},
			CaptureTargetEvaluator:  true,
		})
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		profile, err := NewCompactKbuildProfile("root:computed-name", filepath.Join(root, "Makefile"), root, parsed)
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		target := parsed.Variables["target"]
		effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, target)
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		if !selected {
			return kbuildProbeComputedNameSnapshot{}, &kbuildProbeComputedNameTargetNotSelectedError{target: target}
		}
		wrapper, wrapperRoles, err := EvaluateCompactKbuildTextActionRolesForAutomaticTarget(
			profile, target, target, "", []string{"FORCE"}, nil, nil, profile.Rules[0].Recipe[0],
		)
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		saved, err := EvaluateCompactKbuildText(profile, target, "", []string{"FORCE"}, nil, nil, "$(savedcmd_$@)")
		if err != nil {
			return kbuildProbeComputedNameSnapshot{}, err
		}
		return kbuildProbeComputedNameSnapshot{
			Target: target, Existing: parsed.Variables["existing-targets"], Saved: saved,
			Wrapper: wrapper, WrapperRoles: wrapperRoles, ActionRoles: effects.ActionRoles,
			Includes: func() []string {
				includes := make([]string, 0, len(parsed.Includes))
				for _, include := range parsed.Includes {
					includes = append(includes, include.Path)
				}
				return includes
			}(),
		}, nil
	})
}

type kbuildProbeComputedNameTargetNotSelectedError struct {
	target string
}

func (e *kbuildProbeComputedNameTargetNotSelectedError) Error() string {
	return "probe-dependent Kbuild target was not selected: " + e.target
}

func TestKbuildProbeDependentSavedCommandPreservesSelectedCompilerRole(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", `
empty :=
space := $(empty) $(empty)
space_escape := _-_SPACE_-_
PHONY := FORCE
newer-prereqs = $(filter-out $(PHONY),$?)
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name compiler_selected --crate-type proc-macro - </dev/null)
target := rust/$(name)
existing-targets := $(wildcard $(target))
-include $(foreach f,$(existing-targets),$(dir $(f)).$(notdir $(f)).cmd)
cmd_emit = $(CC) -o $@ -x c /dev/null
cmd-check = $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd_$@))),$(subst $(space),$(space_escape),$(strip $(cmd_$(1)))))
if_changed = $(if $(newer-prereqs)$(cmd-check),$(cmd_$(1)),@:)
$(target): FORCE
	$(call if_changed,emit)
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	targetOptions := testKbuildProbeScopeOptions(t, fixture)
	targetOptions.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: targetOptions}
	ccRole := KbuildActionRoleToken("target", "cc")
	mustWriteSource(t, root, "rust/.compiler-selected.o.cmd",
		"savedcmd_rust/compiler-selected.o := "+ccRole+" -o rust/compiler-selected.o -x c /dev/null\n")
	discovery, err := evaluateKbuildProbeComputedNameSnapshot(t, root, probeOptions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("computed-name discovery has %d probe nodes, want one Rust filename query: %#v", got, discovery.Plan.Nodes)
	}
	request := discovery.Plan.Requests[discovery.Plan.Nodes[0].RequestID]
	if request.Outcome.Kind != "text" || !request.Outcome.PathComponent ||
		len(request.Steps) != 1 || request.Steps[0].Tool != "rustc" {
		t.Fatalf("computed-name filename request = %#v, want a Rust path-component query", request)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Target) ||
		!linuxProbeSymbolPattern.MatchString(discovery.Value.Existing) {
		t.Fatalf("computed-name discovery target/existing = %q/%q, want path-component atoms",
			discovery.Value.Target, discovery.Value.Existing)
	}
	if discovery.Value.Saved != "" || len(discovery.Value.Includes) != 0 {
		t.Fatalf("computed-name discovery selected unavailable cache state: %#v", discovery.Value)
	}
	wantRoles := []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}
	if !strings.Contains(discovery.Value.Wrapper, ccRole) ||
		!reflect.DeepEqual(discovery.Value.WrapperRoles, wantRoles) ||
		!reflect.DeepEqual(discovery.Value.ActionRoles, wantRoles) {
		t.Fatalf("computed-name discovery hid configured compiler provenance: %#v, want roles %#v", discovery.Value, wantRoles)
	}

	roots := writeKbuildTextProbeResultsByNode(t, discovery.Plan, func(_ int, _ ProbePlanNode) string {
		return "compiler-selected.o"
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := evaluateKbuildProbeComputedNameSnapshot(t, root, probeOptions, oracle)
	if err != nil {
		t.Fatal(err)
	}
	wantSaved := ccRole + " -o rust/compiler-selected.o -x c /dev/null"
	if replay.Value.Target != "rust/compiler-selected.o" ||
		replay.Value.Existing != "rust/compiler-selected.o" ||
		replay.Value.Saved != wantSaved || replay.Value.Wrapper != "@:" ||
		len(replay.Value.WrapperRoles) != 0 ||
		!reflect.DeepEqual(replay.Value.ActionRoles, wantRoles) {
		t.Fatalf("computed-name replay = %#v, want exact unchanged cache with selected roles %#v", replay.Value, wantRoles)
	}
	if !reflect.DeepEqual(replay.Value.Includes, []string{"rust/.compiler-selected.o.cmd"}) {
		t.Fatalf("computed-name replay includes = %q, want selected command cache", replay.Value.Includes)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatal("computed-name replay changed the probe plan")
	}
	if linuxProbeSymbolPattern.MatchString(replay.Value.Target) ||
		linuxProbeSymbolPattern.MatchString(replay.Value.Existing) ||
		linuxProbeSymbolPattern.MatchString(replay.Value.Saved) ||
		linuxProbeSymbolPattern.MatchString(replay.Value.Wrapper) ||
		linuxProbeSymbolPattern.MatchString(strings.Join(replay.Value.Includes, " ")) {
		t.Fatalf("computed-name replay leaked a probe atom: %#v", replay.Value)
	}
}

func TestKbuildProbeDependentSavedCommandRejectsReplayOnlyProbe(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", `
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name compiler_selected --crate-type proc-macro - </dev/null)
target := rust/$(name)
existing-targets := $(wildcard $(target))
-include $(foreach f,$(existing-targets),$(dir $(f)).$(notdir $(f)).cmd)
cmd_rustc_o_rs = $(RUSTC) --emit=obj -o $@ $<
if_changed = $(cmd_$(1)) $(savedcmd_$@)
$(target): input.rs FORCE
	$(call if_changed,rustc_o_rs)
`)
	mustWriteSource(t, root, "rust/.compiler-selected.o.cmd", `
export replay_only := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name replay_only --crate-type proc-macro - </dev/null)
savedcmd_rust/compiler-selected.o := cached compiler command
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	targetOptions := testKbuildProbeScopeOptions(t, fixture)
	targetOptions.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: targetOptions}
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			RootDir: root, WorkingDir: root,
			VirtualFileView: compilerSelectedVirtualFileView(),
			CommandLineVariables: map[string]string{
				"RUSTC": KbuildActionRoleToken("target", "rustc"),
			},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"target"}, CaptureTargetEvaluator: true,
		})
		if err != nil {
			return "", err
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
		if err != nil {
			return "", err
		}
		return parsed.Variables["target"], nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("computed-name discovery has %d nodes, want only the target filename query: %#v", got, discovery.Plan.Nodes)
	}

	roots := writeKbuildTextProbeResultsByNode(t, discovery.Plan, func(_ int, _ ProbePlanNode) string {
		return "compiler-selected.o"
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
	if err == nil || !strings.Contains(err.Error(), "validate replayed Kbuild probe plan") ||
		!strings.Contains(err.Error(), "missing result for probe node") {
		t.Fatalf("replay-only computed-name probe error = %v, want oracle plan validation failure", err)
	}
}

func compilerSelectedVirtualFileView() KbuildVirtualFileView {
	return &testKbuildVirtualFileView{matches: map[string][]string{
		"rust/*.o":                 {"rust/compiler-selected.o"},
		"rust/compiler-selected.o": {"rust/compiler-selected.o"},
	}}
}

func TestKbuildProbeDependentComputedNameIsProvisionalOnlyDuringTargetEvaluation(t *testing.T) {
	for _, test := range []struct {
		name      string
		reference string
	}{
		{name: "ordinary reference", reference: "$(savedcmd_$(name))"},
		{name: "substitution reference", reference: "$(savedcmd_$(name):.o=.ko)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := linuxCompilerBootstrapFixtures(t)[1]
			targetOptions := testKbuildProbeScopeOptions(t, fixture)
			targetOptions.Tools["rustc"] = "/configured/target/rustc"
			probeOptions := KbuildProbeWorkloadOptions{Target: targetOptions}
			_, err := EvaluateKbuildProbeWorkload(probeOptions, nil, func(scopes *KbuildProbeScopes) (string, error) {
				options, err := scopes.Options("target", KbuildOptions{
					Variables:               map[string]string{"RUSTC": targetOptions.Tools["rustc"]},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureVariables:        []string{"selected"},
				})
				if err != nil {
					return "", err
				}
				source := `
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name compiler_selected --crate-type proc-macro - </dev/null)
savedcmd_compiler-selected.o := cached compiler command
selected := ` + test.reference + "\n"
				parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
				if err != nil {
					return "", err
				}
				return parsed.Variables["selected"], nil
			})
			if err == nil || !strings.Contains(err.Error(), "probe-dependent computed Make variable name") ||
				!strings.Contains(err.Error(), "unsupported outside target replay") {
				t.Fatalf("top-level computed-name error = %v, want target-evaluation boundary", err)
			}
		})
	}
}
