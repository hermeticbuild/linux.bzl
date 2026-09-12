package kconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestActionPlanCheckpointRejectsMalformedOrUnboundState(t *testing.T) {
	m := familyVariantMetadataForTest(t, nil)
	plan, _, err := m.lowerSelectedActionPlan(actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := m.Config.KbuildProfiles[0].evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	b := ActionPlanCheckpointBindings{Variant: "base", SourceArtifacts: map[string]string{"linux": root}, Toolsets: plan.Toolsets, ConfigValues: m.configFragment, ConfigFiles: familyTestConfig("1", "0"), ConfigSymbolUniverse: m.configSymbolUniverse, ActionContracts: m.actionContracts, ActionRoles: m.actionRoles}
	data, err := CaptureActionPlanCheckpoint(plan, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreActionPlanCheckpoint(data, b); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"unknown-field", "duplicate-field", "trailing", "wrong-schema", "variant", "toolset", "config", "config-files", "config-universe", "roles", "contracts", "source-artifacts", "physical-root", "traversal", "absolute", "backslash", "unknown-artifact", "missing-profile-roots", "duplicate-profile", "duplicate-selection", "missing-producer", "unknown-child", "invalid-projection-slot", "missing-compiler-node"} {
		t.Run(mutation, func(t *testing.T) {
			var r actionPlanCheckpoint
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			profile := r.Profiles[0]
			switch mutation {
			case "wrong-schema":
				r.Schema = "other"
			case "variant":
				r.Variant = "other"
			case "toolset":
				r.Plan.Toolsets["target"] = bootstrapTestIdentity
			case "config":
				r.ConfigValues["CONFIG_USED"] = "changed"
			case "config-files":
				r.ConfigFiles[".config"] = "different current config"
			case "config-universe":
				r.ConfigSymbolUniverse = []string{"CONFIG_OTHER_UNIVERSE"}
			case "roles":
				r.Roles = nil
			case "contracts":
				r.Contracts = nil
			case "source-artifacts":
				r.SourceArtifacts = []string{"other"}
			case "physical-root":
				p := r.Graph.Profiles[profile]
				p.Roots = map[string]string{"injected": root}
				r.Graph.Profiles[profile] = p
			case "traversal", "absolute", "backslash", "unknown-artifact":
				value := planCheckpointRoot{Artifact: "linux", Path: "../outside"}
				if mutation == "absolute" {
					value.Path = "/outside"
				}
				if mutation == "backslash" {
					value.Path = "a\\outside"
				}
				if mutation == "unknown-artifact" {
					value = planCheckpointRoot{Artifact: "outside", Path: "."}
				}
				r.Roots[profile]["__LINUX_BZL_SOURCE_TREE__"] = value
			case "missing-profile-roots":
				delete(r.Roots, profile)
			case "duplicate-profile":
				r.Profiles = append(r.Profiles, profile)
			case "duplicate-selection":
				r.Graph.Selections = append(r.Graph.Selections, r.Graph.Selections[0])
			case "missing-producer":
				r.Graph.Producers[0].Value = "not-in-plan"
			case "unknown-child":
				r.Graph.TargetInvocations[profile] = map[string][]string{"target": {"missing"}}
			case "invalid-projection-slot":
				r.ProjectedInternalOutputs = []planCheckpointOutput{{r.Plan.Nodes[0].ID, -1, true}}
			case "missing-compiler-node":
				r.CompilerInvocations["absent"] = actionRecipeCompilerProbeInvocation{Tool: "cc"}
			}
			candidate, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "unknown-field":
				candidate = append([]byte("{\"Unknown\":0,"), candidate[1:]...)
			case "duplicate-field":
				candidate = append([]byte("{\"Schema\":\"duplicate\","), candidate[1:]...)
			case "trailing":
				candidate = append(candidate, []byte("{}")...)
			}
			if _, err := RestoreActionPlanCheckpoint(candidate, b); err == nil {
				t.Fatal("accepted malformed or unbound plan checkpoint")
			}
		})
	}
}

func TestActionPlanCheckpointRetainsProjectedGeneratorCommitments(t *testing.T) {
	plan, _, _ := projectedGeneratorPlanForTest(t, "y", "n")
	plan.Toolsets["host"] = plan.Toolsets["target"]
	plan.selectionGraph = &compactKbuildSelectionGraph{profiles: map[string]CompactKbuildProfile{}}
	plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{}
	b := ActionPlanCheckpointBindings{Variant: "base", SourceArtifacts: map[string]string{"linux": t.TempDir()}, Toolsets: plan.Toolsets, ConfigValues: plan.metadata.configFragment, ConfigFiles: familyTestConfig("1", "0"), ConfigSymbolUniverse: plan.metadata.configSymbolUniverse, ActionContracts: plan.metadata.actionContracts}
	if _, err := CaptureActionPlanCheckpoint(plan, b); err == nil {
		t.Fatal("captured unlowered projected-generator candidates")
	}
	if err := lowerProjectedGeneratorsForFamilySnapshot(plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.projectedGeneratorValidations) == 0 || len(plan.projectedGeneratorOriginalOutputs) == 0 {
		t.Fatal("fixture did not lower validation witnesses")
	}
	data, err := CaptureActionPlanCheckpoint(plan, b)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreActionPlanCheckpoint(data, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.projectedGeneratorValidations, restored.projectedGeneratorValidations) || !reflect.DeepEqual(plan.projectedGeneratorInternalNodes, restored.projectedGeneratorInternalNodes) || !reflect.DeepEqual(plan.projectedGeneratorInternalOutputs, restored.projectedGeneratorInternalOutputs) || !reflect.DeepEqual(plan.projectedGeneratorOriginalOutputs, restored.projectedGeneratorOriginalOutputs) {
		t.Fatal("checkpoint lost projected-generator validation or original output commitments")
	}
}
