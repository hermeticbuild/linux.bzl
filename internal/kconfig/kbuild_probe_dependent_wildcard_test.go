package kconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type kbuildProbeWildcardSnapshot struct {
	Existing string
	Marker   string
	After    string
	Saved    string
	Includes []string
}

type kbuildProbeWildcardReductionSnapshot struct {
	ObjectDirectories string
	Selected          string
}

func TestKbuildProbeDependentWildcardClosesLatePureReduction(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", `
first_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name first --crate-type proc-macro - </dev/null)
second_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name second --crate-type proc-macro - </dev/null)
targets := rust/core.o rust/$(first_name) rust/$(second_name)
intermediate_targets := $(filter %.asn1.o,$(targets))
targets += $(patsubst %.asn1.o,%.asn1.c,$(intermediate_targets))
targets += $(patsubst %.asn1.o,%.asn1.h,$(intermediate_targets))
existing-targets := $(wildcard $(sort $(targets)))
obj-dirs := $(sort $(patsubst %/,%, $(dir $(filter-out rust/ FORCE,$(targets)))))
existing-dirs := $(sort $(patsubst %/,%, $(dir $(existing-targets))))
obj-dirs := $(strip $(filter-out $(existing-dirs), $(obj-dirs)))
SELECTED := false
ifneq ($(obj-dirs),)
SELECTED := true
endif
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: target}
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeWildcardReductionSnapshot, error) {
		options, err := scopes.Options("target", KbuildOptions{
			RootDir:                 root,
			WorkingDir:              root,
			Variables:               map[string]string{"RUSTC": target.Tools["rustc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"obj-dirs", "SELECTED"},
		})
		if err != nil {
			return kbuildProbeWildcardReductionSnapshot{}, err
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
		if err != nil {
			return kbuildProbeWildcardReductionSnapshot{}, err
		}
		return kbuildProbeWildcardReductionSnapshot{
			ObjectDirectories: parsed.Variables["obj-dirs"],
			Selected:          parsed.Variables["SELECTED"],
		}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 2 {
		t.Fatalf("wildcard reduction discovery has %d probe nodes, want two Rust filename leaves: %#v", got, discovery.Plan.Nodes)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.ObjectDirectories) ||
		!linuxProbeSymbolPattern.MatchString(discovery.Value.Selected) {
		t.Fatalf("wildcard reduction discovery did not remain contextual: %#v", discovery.Value)
	}

	roots := writeKbuildTextProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) string {
		return []string{"libfirst.so", "libsecond.rlib"}[index]
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if want := (kbuildProbeWildcardReductionSnapshot{ObjectDirectories: "rust", Selected: "true"}); replay.Value != want {
		t.Fatalf("wildcard reduction replay = %#v, want %#v", replay.Value, want)
	}
	if got := len(replay.Plan.Nodes); got != 4 {
		t.Fatalf("wildcard reduction replay has %d nodes, want two leaves and two pure reductions: %#v", got, replay.Plan.Nodes)
	}
	derivedNode, predicateNode := replay.Plan.Nodes[2], replay.Plan.Nodes[3]
	if want := []string{replay.Plan.Nodes[0].ID, replay.Plan.Nodes[1].ID}; !reflect.DeepEqual(derivedNode.Inputs, want) {
		t.Fatalf("late derived-text inputs = %q, want Rust leaves %q", derivedNode.Inputs, want)
	}
	if want := []string{derivedNode.ID}; !reflect.DeepEqual(predicateNode.Inputs, want) {
		t.Fatalf("late predicate inputs = %q, want derived text %q", predicateNode.Inputs, want)
	}
	for _, node := range replay.Plan.Nodes[2:] {
		request := replay.Plan.Requests[node.RequestID]
		if len(request.Steps) != 0 || len(request.Scratch) != 0 || len(request.Sources) != 0 || len(request.SourceRoots) != 0 {
			t.Fatalf("late wildcard reduction is not dependency-only: %#v", request)
		}
		reference := ProbeReference{
			NodeID: node.ID, RequestID: node.RequestID, Scope: node.Scope, Kind: request.Outcome.Kind,
		}
		if _, err := oracle.Result(reference); err != nil {
			t.Fatalf("late wildcard reduction %s was not closed in the replay oracle: %v", node.ID, err)
		}
	}
	if request := replay.Plan.Requests[derivedNode.RequestID]; request.Outcome.Kind != "text" || len(request.Outcome.Fragments) == 0 {
		t.Fatalf("late derived-text request = %#v", request)
	}
	if request := replay.Plan.Requests[predicateNode.RequestID]; request.Outcome.Kind != "boolean" ||
		request.Outcome.Predicate == nil || request.Outcome.Predicate.Operator != "result-text-empty" {
		t.Fatalf("late empty-text predicate request = %#v", request)
	}
	if strings.Contains(replay.Value.ObjectDirectories+replay.Value.Selected, linuxProbeSymbolPrefix) {
		t.Fatalf("wildcard reduction replay leaked a planner token: %#v", replay.Value)
	}

	mustWriteSource(t, root, "rust/libfirst.so", "")
	mustWriteSource(t, root, "rust/libsecond.rlib", "")
	existingReplay, err := EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if want := (kbuildProbeWildcardReductionSnapshot{Selected: "false"}); existingReplay.Value != want {
		t.Fatalf("existing wildcard reduction replay = %#v, want %#v", existingReplay.Value, want)
	}
	if got := len(existingReplay.Plan.Nodes); got != 4 {
		t.Fatalf("concrete existing-directory replay has %d nodes, want leaves plus pure reductions", got)
	}
	if existingReplay.Plan.Nodes[2].ID == derivedNode.ID {
		t.Fatal("context-dependent existing-directory filter reused the empty-filesystem reduction")
	}
	if strings.Contains(existingReplay.Value.ObjectDirectories+existingReplay.Value.Selected, linuxProbeSymbolPrefix) {
		t.Fatalf("existing wildcard reduction replay leaked a planner token: %#v", existingReplay.Value)
	}
}

func TestKbuildProbeDependentWildcardUsesConcreteReplayFilesystem(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "Makefile", `
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name wildcard --crate-type proc-macro - </dev/null)
target := rust/$(name)
savedcmd_rust/compiler-selected.mk := cached compiler command
targets := missing.mk static.mk obj/$(name)
existing-targets := $(wildcard $(sort $(targets)))
-include $(foreach f,$(existing-targets),$(dir $(f))$(notdir $(f)))
obj-dirs := $(sort $(patsubst %/,%, $(dir $(targets))))
existing-dirs := $(sort $(patsubst %/,%, $(dir $(existing-targets))))
obj-dirs := $(strip $(filter-out $(existing-dirs), $(obj-dirs)))
ifneq ($(obj-dirs),)
$(shell mkdir -p $(obj-dirs))
endif
AFTER := root
$(target): FORCE
	@true
`)
	mustWriteSource(t, root, "static.mk", "MARKER += static\n")
	mustWriteSource(t, root, "obj/compiler-selected.mk", "MARKER += dynamic\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	probeOptions := KbuildProbeWorkloadOptions{Target: target}
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeWildcardSnapshot, error) {
		options, err := scopes.Options("target", KbuildOptions{
			RootDir:                 root,
			WorkingDir:              root,
			Variables:               map[string]string{"RUSTC": target.Tools["rustc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"target", "existing-targets", "MARKER", "AFTER"},
			CaptureTargetEvaluator:  true,
		})
		if err != nil {
			return kbuildProbeWildcardSnapshot{}, err
		}
		parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), options)
		if err != nil {
			return kbuildProbeWildcardSnapshot{}, err
		}
		snapshot := kbuildProbeWildcardSnapshot{
			Existing: parsed.Variables["existing-targets"],
			Marker:   parsed.Variables["MARKER"],
			After:    parsed.Variables["AFTER"],
		}
		profile, err := NewCompactKbuildProfile("root:probe-wildcard", filepath.Join(root, "Makefile"), root, parsed)
		if err != nil {
			return kbuildProbeWildcardSnapshot{}, err
		}
		snapshot.Saved, err = EvaluateCompactKbuildText(
			profile, parsed.Variables["target"], "", []string{"FORCE"}, nil, nil, "$(savedcmd_$@)",
		)
		if err != nil {
			return kbuildProbeWildcardSnapshot{}, err
		}
		for _, include := range parsed.Includes {
			snapshot.Includes = append(snapshot.Includes, include.Path)
		}
		return snapshot, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("wildcard discovery has %d probe nodes, want one filename query: %#v", got, discovery.Plan.Nodes)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Existing) ||
		discovery.Value.Marker != "" || discovery.Value.After != "root" || discovery.Value.Saved != "" ||
		len(discovery.Value.Includes) != 0 {
		t.Fatalf("wildcard discovery did not defer its include: %#v", discovery.Value)
	}

	for _, test := range []struct {
		name     string
		filename string
		want     kbuildProbeWildcardSnapshot
	}{
		{
			name:     "dynamic and static matches",
			filename: "compiler-selected.mk",
			want: kbuildProbeWildcardSnapshot{
				Existing: "obj/compiler-selected.mk static.mk",
				Marker:   "dynamic static",
				After:    "root",
				Saved:    "cached compiler command",
				Includes: []string{"obj/compiler-selected.mk", "./static.mk"},
			},
		},
		{
			name:     "missing dynamic match",
			filename: "compiler-missing.mk",
			want: kbuildProbeWildcardSnapshot{
				Existing: "static.mk",
				Marker:   "static",
				After:    "root",
				Includes: []string{"./static.mk"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildTextProbeResultsByNode(t, discovery.Plan, func(_ int, _ ProbePlanNode) string {
				return test.filename
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replay.Value, test.want) {
				t.Fatalf("wildcard replay = %#v, want %#v", replay.Value, test.want)
			}
			if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
				t.Fatalf("wildcard replay changed the probe plan")
			}
			if strings.Contains(replay.Value.Existing, linuxProbeSymbolPrefix) {
				t.Fatalf("wildcard replay leaked a planner token: %q", replay.Value.Existing)
			}
		})
	}
}
