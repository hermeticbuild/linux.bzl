package main

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func familyCompilerGuardCarryPipelineForTest(t *testing.T, mode string, inputs []familyCompilerGuardRoundInput, names []string) *familyCompilerGuardPipeline {
	t.Helper()
	flags, execution := familyCompilerGuardFlagsForTest(t, mode, 0)
	for index, input := range inputs {
		name := strconv.Itoa(index)
		flags.manifests = append(flags.manifests, namedPath{Name: name, Path: input.manifest})
		flags.plans = append(flags.plans, namedPath{Name: name, Path: input.plan})
		flags.hosts = append(flags.hosts, namedPath{Name: name, Path: input.host})
		flags.targets = append(flags.targets, namedPath{Name: name, Path: input.target})
	}
	validated, err := flags.validate(execution)
	if err != nil {
		t.Fatal(err)
	}
	var variants []familyPlanVariantRequest
	for _, name := range names {
		variants = append(variants, familyPlanVariantRequest{name: name})
	}
	p, err := newFamilyCompilerGuardPipeline(&flags, validated, variants, familyCompilerGuardPlanForTest(t).Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func familyCompilerGuardCarryScopesForTest(t *testing.T, toolsets map[string]string) *kconfig.KbuildProbeScopes {
	t.Helper()
	bootstrap, err := newLinuxCompilerBootstrapPlan(toolsets["target"], toolsets["host"])
	if err != nil {
		t.Fatal(err)
	}
	options := kconfig.KbuildProbeWorkloadOptions{}
	for _, scope := range []string{"target", "host"} {
		reference := bootstrap.target
		if scope == "host" {
			reference = bootstrap.host
		}
		facts, err := kconfig.ParseLinuxCompilerBootstrapResult(testLinuxCompilerBootstrapResult(t, reference, toolsets[scope], false), scope, toolsets[scope])
		if err != nil {
			t.Fatal(err)
		}
		option := kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: t.TempDir(),
			Facts: facts, Tools: map[string]string{"cc": "/configured/" + scope + "/cc"},
		}
		if scope == "target" {
			options.Target = option
		} else {
			options.Host = &option
		}
	}
	evaluation, err := kconfig.EvaluateKbuildProbeWorkload(options, nil, func(scopes *kconfig.KbuildProbeScopes) (*kconfig.KbuildProbeScopes, error) {
		return scopes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluation.Value
}

func familyCompilerGuardEvaluateEmptyForTest(t *testing.T, p *familyCompilerGuardPipeline) {
	t.Helper()
	scopes := familyCompilerGuardCarryScopesForTest(t, p.toolsets)
	for _, name := range p.names {
		if err := p.prepareVariant(name, scopes, &kconfig.CompactMetadata{}); err != nil {
			t.Fatal(err)
		}
		if err := p.finishVariant(name, scopes); err != nil {
			t.Fatal(err)
		}
	}
}

func familyCompilerGuardCarryTreeForTest(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		files[relative] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestFamilyCompilerGuardCarryMatchesEvaluatedEmptyRoundsAndFinalReplay(t *testing.T) {
	for _, names := range [][]string{{"base"}, {"relevant", "base", "irrelevant"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			first, manifest, _ := familyCompilerGuardRoundForTest(t, 0, nil)
			manifest.Variants = map[string][]familyCompilerGuardQuery{}
			for _, name := range names {
				manifest.Variants[name] = nil
			}
			previous := []string{familyCompilerGuardWriteManifestForTest(t, first.manifest, manifest)}
			inputs := []familyCompilerGuardRoundInput{first}
			for round := 1; round < maxFamilyCompilerGuardRounds; round++ {
				carried := familyCompilerGuardCarryPipelineForTest(t, "guards", inputs, names)
				if done, err := carried.carryForwardEmptyRound("guards"); err != nil || !done {
					t.Fatalf("round %d carry = %t/%v", round, done, err)
				}
				if len(carried.prepared)+len(carried.finished)+len(carried.plans)+len(carried.queries) != 0 || carried.activeVariant != "" {
					t.Fatal("carrying a transport fabricated variant lifecycle evidence")
				}
				for _, prior := range carried.prior {
					if len(prior.replayed) != 0 {
						t.Fatal("carrying a transport fabricated frozen union replay evidence")
					}
				}
				if err := carried.publish(); err == nil {
					t.Fatal("carried output allowed normal publication without evaluating variants")
				}
				evaluated := familyCompilerGuardCarryPipelineForTest(t, "guards", inputs, names)
				familyCompilerGuardEvaluateEmptyForTest(t, evaluated)
				if err := evaluated.publish(); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(carried.flags.manifestOut)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(evaluated.flags.manifestOut)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("carry changed canonical manifest bytes: %s != %s (%v)", got, want, err)
				}
				if !reflect.DeepEqual(familyCompilerGuardCarryTreeForTest(t, carried.flags.planOut), familyCompilerGuardCarryTreeForTest(t, evaluated.flags.planOut)) {
					t.Fatal("carry changed canonical frozen plan tree")
				}
				next, digest, err := readFamilyCompilerGuardManifest(carried.flags.manifestOut)
				if err != nil || next.Round != round || !slices.Equal(next.Previous, previous) || next.Truncated || next.PlanID != manifest.PlanID || !maps.Equal(next.Toolsets, manifest.Toolsets) || !reflect.DeepEqual(next.Variants, manifest.Variants) {
					t.Fatalf("carry lost the complete chain or family/toolset membership: %#v/%v", next, err)
				}
				previous = append(previous, digest)
				// The test's empty result trees represent the completed host and
				// target map_directory actions for the carried empty plan.
				input, _, _ := familyCompilerGuardRoundForTest(t, round, previous[:len(previous)-1])
				input.manifest, input.plan = carried.flags.manifestOut, carried.flags.planOut
				inputs = append(inputs, input)
			}
			replay := familyCompilerGuardCarryPipelineForTest(t, "replay", inputs, names)
			if done, err := replay.carryForwardEmptyRound("replay"); err != nil || done {
				t.Fatalf("final replay carried optional discovery: %t/%v", done, err)
			}
			if err := replay.publish(); err == nil {
				t.Fatal("final replay skipped variant evaluation after carried rounds")
			}
			familyCompilerGuardEvaluateEmptyForTest(t, replay)
			if err := replay.publish(); err != nil {
				t.Fatalf("complete real final replay rejected carried empty rounds: %v", err)
			}
			for index := range replay.prior {
				original := replay.prior[index].replayed
				replay.prior[index].replayed = []kconfig.ProbePlanVariant{{Name: "base", Plan: familyCompilerGuardPlanForTest(t, "unexpected")}}
				if err := replay.publish(); err == nil {
					t.Fatalf("carried chain bypassed exact frozen membership for round %d", index)
				}
				replay.prior[index].replayed = original
			}
		})
	}
}

func TestFamilyCompilerGuardCarryRequiresEligiblePristineDiscovery(t *testing.T) {
	for _, change := range []string{"nil", "initial", "replay", "no prior", "maximum rounds", "no flags", "no manifest output", "no plan output", "truncated", "nonempty query", "prepared", "finished", "active"} {
		t.Run(change, func(t *testing.T) {
			input, _, _ := familyCompilerGuardRoundForTest(t, 0, nil)
			p := familyCompilerGuardCarryPipelineForTest(t, "guards", []familyCompilerGuardRoundInput{input}, []string{"base"})
			outputs := *p.flags
			mode := "guards"
			switch change {
			case "nil":
				p = nil
			case "initial", "replay":
				mode = change
			case "no prior":
				p = familyCompilerGuardCarryPipelineForTest(t, "guards", nil, []string{"base"})
				outputs = *p.flags
			case "maximum rounds":
				p.prior = append(p.prior, p.prior[0], p.prior[0])
			case "no flags":
				p.flags = nil
			case "no manifest output":
				p.flags.manifestOut = ""
			case "no plan output":
				p.flags.planOut = ""
			case "truncated":
				p.prior[0].manifest.Truncated = true
			case "nonempty query":
				p.prior[0].manifest.Variants["base"] = []familyCompilerGuardQuery{{Scope: "target", Role: "cc", Language: "c", Names: []string{"__NEW"}}}
			case "prepared":
				if err := p.prepareVariant("base", familyCompilerGuardCarryScopesForTest(t, p.toolsets), &kconfig.CompactMetadata{}); err != nil {
					t.Fatal(err)
				}
			case "finished":
				familyCompilerGuardEvaluateEmptyForTest(t, p)
			case "active":
				p.activeVariant = "base"
			}
			if done, err := p.carryForwardEmptyRound(mode); err != nil || done {
				t.Fatalf("ineligible carry = %t/%v", done, err)
			}
			for _, path := range []string{outputs.manifestOut, outputs.planOut} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("ineligible carry touched %s: %v", path, err)
				}
			}
		})
	}
}

func familyCompilerGuardNonemptyCarryRoundForTest(t *testing.T) (familyCompilerGuardRoundInput, familyCompilerGuardManifest, string, kconfig.ProbeResult) {
	t.Helper()
	input, manifest, _ := familyCompilerGuardRoundForTest(t, 0, nil)
	plan := familyCompilerGuardPlanForTest(t, "frozen-nonempty")
	input.plan = filepath.Join(t.TempDir(), "plan")
	if err := plan.Write(input.plan); err != nil {
		t.Fatal(err)
	}
	id, err := familyCompilerGuardPlanID(plan)
	if err != nil {
		t.Fatal(err)
	}
	manifest.PlanID = id
	digest := familyCompilerGuardWriteManifestForTest(t, input.manifest, manifest)
	node := plan.Nodes[0]
	result := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "text",
		Steps: []kconfig.ProbeStepResult{{Name: "guard", Status: "success"}},
	}
	input.target = t.TempDir()
	writeTestProbeResult(t, input.target, result)
	return input, manifest, digest, result
}

func TestFamilyCompilerGuardCarryRejectsEmptyQueriesWithNonemptyValidatedPlan(t *testing.T) {
	input, _, _, _ := familyCompilerGuardNonemptyCarryRoundForTest(t)
	p := familyCompilerGuardCarryPipelineForTest(t, "guards", []familyCompilerGuardRoundInput{input}, []string{"base"})
	if done, err := p.carryForwardEmptyRound("guards"); err != nil || done {
		t.Fatalf("empty query list certified a nonempty frozen DAG: %t/%v", done, err)
	}
}

func TestFamilyCompilerGuardCarryLoadsEveryPriorResultBeforeShortcut(t *testing.T) {
	for _, change := range []string{"unchanged", "missing result", "missing tree", "wrong toolset", "wrong scope", "wrong request", "wrong kind", "malformed JSON", "invalid empty marker"} {
		t.Run(change, func(t *testing.T) {
			first, manifest, firstID, result := familyCompilerGuardNonemptyCarryRoundForTest(t)
			last, _, _ := familyCompilerGuardRoundForTest(t, 1, []string{firstID})
			filename := filepath.Join(first.target, "results", result.NodeID+".json")
			switch change {
			case "missing result":
				first.target = last.target
			case "missing tree":
				first.target = filepath.Join(t.TempDir(), "absent")
			case "wrong toolset":
				result.ToolsetIdentity = manifest.Toolsets["host"]
			case "wrong scope":
				result.Scope = "host"
			case "wrong request":
				result.RequestID = strings.Repeat("f", 64)
			case "wrong kind":
				value := true
				result.Kind, result.Boolean = "boolean", &value
			case "malformed JSON":
				if err := os.WriteFile(filename, []byte("{bad\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "invalid empty marker":
				if err := os.WriteFile(filepath.Join(last.target, ".empty"), []byte("forged"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(change, "wrong ") {
				writeTestProbeResult(t, first.target, result)
			}
			flags, _ := familyCompilerGuardFlagsForTest(t, "guards", 0)
			p, err := newFamilyCompilerGuardPipeline(&flags, []familyCompilerGuardRoundInput{first, last}, []familyPlanVariantRequest{{name: "base"}}, manifest.Toolsets)
			if change != "unchanged" {
				if err == nil || p != nil {
					t.Fatalf("empty last round bypassed invalid earlier results: %#v/%v", p, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if done, err := p.carryForwardEmptyRound("guards"); err != nil || !done {
				t.Fatalf("validated earlier results prevented empty last-round carry: %t/%v", done, err)
			}
			// Even this apparently empty manifest must be matched back to its
			// nonempty earlier plan at final replay; carrying did not certify it.
			replay := familyCompilerGuardCarryPipelineForTest(t, "replay", []familyCompilerGuardRoundInput{first, last}, []string{"base"})
			familyCompilerGuardEvaluateEmptyForTest(t, replay)
			if err := replay.publish(); err == nil {
				t.Fatal("carry erased the earlier nonempty frozen membership obligation")
			}
		})
	}
}

func TestFamilyCompilerGuardCarryDoesNotOverwriteOutputs(t *testing.T) {
	for _, output := range []string{"manifest", "plan"} {
		t.Run(output, func(t *testing.T) {
			input, _, _ := familyCompilerGuardRoundForTest(t, 0, nil)
			p := familyCompilerGuardCarryPipelineForTest(t, "guards", []familyCompilerGuardRoundInput{input}, []string{"base"})
			filename := p.flags.manifestOut
			if output == "plan" {
				if err := os.Mkdir(p.flags.planOut, 0o755); err != nil {
					t.Fatal(err)
				}
				filename = filepath.Join(p.flags.planOut, "existing")
			}
			want := []byte("existing output must remain unchanged\n")
			if err := os.WriteFile(filename, want, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := p.carryForwardEmptyRound("guards"); err == nil {
				t.Fatal("carry accepted an existing output")
			}
			got, err := os.ReadFile(filename)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("carry changed an existing output: %q/%v", got, err)
			}
			if output == "plan" && !reflect.DeepEqual(familyCompilerGuardCarryTreeForTest(t, p.flags.planOut), map[string]string{"existing": string(want)}) {
				t.Fatal("carry changed an existing plan tree")
			}
		})
	}
}
