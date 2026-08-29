package kconfig

import (
	"strings"
	"testing"
)

func TestActionRecipeRejectsPrivateRecursiveMakeProvenanceBytes(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe, string)
	}{
		{name: "tool", mutate: func(recipe *ActionRecipe, boundary string) { recipe.Tool = "actionfile" + boundary }},
		{name: "argument", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Arguments = append(recipe.Arguments, "value"+boundary)
		}},
		{name: "environment-name", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Environment = map[string]string{"NAME" + boundary: "value"}
		}},
		{name: "environment-value", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Environment = map[string]string{"MAKE": "value" + boundary}
		}},
		{name: "working-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work" + boundary
		}},
		{name: "execution-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.ExecutionDirectory = "directory" + boundary
		}},
		{name: "declared-working-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.WorkingDirectories = []string{"directory" + boundary}
		}},
		{name: "working-input-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.Inputs = []string{"data"}
			recipe.WorkingInputs = map[string]string{"input:data": "data" + boundary}
		}},
		{name: "working-output-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.WorkingOutputs = map[string]string{"00000000": "output" + boundary}
		}},
		{name: "observed-output-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.ObservedOutputs = map[string]string{"00000000": "output" + boundary}
		}},
		{name: "command-replay-argument", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.CommandReplays = []ActionRecipeCommandReplay{{
				Name: "make", Invocations: []ActionRecipeCommandReplayInvocation{{Arguments: []string{"value" + boundary}}},
			}}
		}},
		{name: "command-replay-output", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.CommandReplays = []ActionRecipeCommandReplay{{
				Name: "make", Invocations: []ActionRecipeCommandReplayInvocation{{Outputs: []string{"output" + boundary}}},
			}}
		}},
	} {
		for _, boundary := range []struct {
			name  string
			value string
		}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
			t.Run(test.name+"/"+boundary.name, func(t *testing.T) {
				recipe := cloneActionRecipe(base)
				test.mutate(&recipe, boundary.value)
				if err := recipe.Validate(); err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("Validate() error = %v, want private provenance-byte rejection", err)
				}
			})
		}
	}
}

func TestPrivateRecursiveMakeProvenanceBytesRejectedAtMakeIngress(t *testing.T) {
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		t.Run(boundary.name+"/source", func(t *testing.T) {
			if _, err := protectCompactKbuildSourceLiteralActionMarkers("value=" + boundary.value); err == nil ||
				!strings.Contains(err.Error(), "reserved literal-marker byte") {
				t.Fatalf("source protection error = %v, want reserved-byte rejection", err)
			}
		})
		t.Run(boundary.name+"/shell-output", func(t *testing.T) {
			if _, err := normalizeKbuildShellOutput("value=" + boundary.value); err == nil ||
				!strings.Contains(err.Error(), "reserved provenance byte") {
				t.Fatalf("shell normalization error = %v, want reserved-byte rejection", err)
			}
		})
	}
}

func TestActionRecipeMakeShellTransformsRejectPrivateProvenanceBytes(t *testing.T) {
	transforms := []struct {
		name string
		run  func(string) (string, error)
	}{
		{name: "value", run: NormalizeActionRecipeMakeShellValue},
		{name: "single-word", run: QuoteActionRecipeMakeShellSingleWord},
		{name: "single-quoted-segment", run: FormatActionRecipeMakeShellSingleQuotedSegment},
	}
	for _, transform := range transforms {
		for _, boundary := range []struct {
			name  string
			value string
		}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
			t.Run(transform.name+"/"+boundary.name, func(t *testing.T) {
				if _, err := transform.run("value" + boundary.value); err == nil ||
					!strings.Contains(err.Error(), "reserved provenance byte") {
					t.Fatalf("transform error = %v, want reserved-byte rejection", err)
				}
			})
		}
	}
}

func TestActionRecipeAcceptsPrintableAndProtectedRecursiveMakeLiterals(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", compactKbuildRecursiveMakeMarker},
		Environment: map[string]string{
			"\x04_LINUX_BZL_MAKE__": "named literal",
		},
		CommandReplays: []ActionRecipeCommandReplay{{
			Name: "make",
			Invocations: []ActionRecipeCommandReplayInvocation{{
				Arguments: []string{"FLAG=\x04_LINUX_BZL_MAKE__"},
			}},
		}},
		Outputs: []string{"00000000"},
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("literal recursive Make spellings rejected: %v", err)
	}
}

func TestContentAddressActionPlanNodesRewritesSelectedDependencyEdges(t *testing.T) {
	profile := CompactKbuildProfile{Name: "root", Path: "Makefile"}
	selections := []CompactKbuildSelection{
		{Profile: profile.Name, Target: "generated/input.o", MakeTarget: "generated/input.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		{Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile}, KbuildSelections: selections,
	}
	if _, err := newCompactKbuildSelectionGraph(config); err != nil {
		t.Fatalf("selection-based fixture is invalid: %v", err)
	}
	selectionID := func(selection CompactKbuildSelection) string {
		return compactKbuildSelectionKeyString(compactKbuildSelectionKey{
			profile: selection.Profile,
			target:  selection.Target,
			stage:   selection.Stage,
		})
	}

	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy",
		Arguments: []string{"${input:src:00000000}", "${output:00000000}"},
		Inputs:    []string{"src:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		ID: selectionID(selections[0]), Stage: "target", Kind: "generate", Recipe: producerRecipeID,
		Tool: "cc", Product: "vmlinux", Outputs: []ActionPlanOutput{{Tree: "objects", Path: selections[0].Target}},
	}
	consumer := ActionPlanNode{
		ID: selectionID(selections[1]), Stage: "target", Kind: "copy", Recipe: consumerRecipeID,
		Tool: "objcopy", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "src", ProducerID: producer.ID}},
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: selections[1].Target}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Nodes[0].ID, plan.Nodes[0].ContentID(); got != want || got == producer.ID {
		t.Fatalf("producer content ID = %q, want %q distinct from provisional %q", got, want, producer.ID)
	}
	if got, want := plan.Nodes[1].Inputs[0].ProducerID, plan.Nodes[0].ID; got != want {
		t.Fatalf("consumer producer edge = %q, want readdressed producer %q", got, want)
	}
	if got, want := plan.Nodes[1].ID, plan.Nodes[1].ContentID(); got != want || got == consumer.ID {
		t.Fatalf("consumer content ID = %q, want %q distinct from provisional %q", got, want, consumer.ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("content-addressed selected graph is invalid: %v", err)
	}
}
