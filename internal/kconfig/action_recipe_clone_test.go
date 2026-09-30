package kconfig

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func populatedActionRecipeForCloneTest() ActionRecipe {
	return ActionRecipe{
		Schema:    LinuxKernelPlanSchema,
		Kind:      "generate",
		Tool:      "cc",
		Arguments: []string{"argument"},
		ArgumentTransforms: []ActionRecipeArgumentTransform{{
			Index: 0, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
		}},
		ArgumentsFile:    false,
		Environment:      map[string]string{"ENV": "value"},
		WorkingDirectory: "working-directory",
		WorkingDirectories: []string{
			"first/directory",
			"second/directory",
		},
		WorkingTrees:    []string{"external"},
		WorkingInputs:   map[string]string{"source:input": "input/path"},
		WorkingOutputs:  map[string]string{"output": "output/path"},
		ObservedOutputs: map[string]string{"observed": "observed/path"},
		ObservedOutputBases: map[string][]string{
			"observed": {"base-first", "base-second"},
		},
		ContentSubstitutions: map[string]ActionRecipeContentSubstitution{
			"content": {Input: "source:input", Transform: ActionRecipeContentTransformMakeShellWord},
		},
		CommandReplays: []ActionRecipeCommandReplay{{
			Name: "make",
			Invocations: []ActionRecipeCommandReplayInvocation{{
				Arguments: []string{"replay-argument"},
				Outputs:   []string{"replay-output"},
			}},
		}},
		CompilerInvocation: &ActionRecipeCompilerInvocation{
			Tool: "cc", Arguments: []string{"-c", "input.c", "-o", "input.o"},
			WorkingInputUses: []string{"source:input"}, WorkingInputUsesComplete: true,
			AuxiliaryWorkingInputUses: []string{"source:input"},
		},
		ExecutableInputs: []string{"executable"},
		Stdin:            "source:input",
		Stdout:           "output",
		Sources:          []string{"input"},
		Inputs:           []string{"node-input", "base-first", "base-second"},
		Outputs:          []string{"output", "observed"},
		Trees:            []string{"kernel", "external"},
		AuxiliaryTools:   []string{"objcopy"},
	}
}

func TestCloneActionRecipeOwnsEveryMutableField(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ActionRecipe)
	}{
		{name: "arguments", mutate: func(recipe *ActionRecipe) { recipe.Arguments[0] = "changed" }},
		{name: "argument transforms", mutate: func(recipe *ActionRecipe) { recipe.ArgumentTransforms[0].Index = 1 }},
		{name: "arguments file", mutate: func(recipe *ActionRecipe) { recipe.ArgumentsFile = !recipe.ArgumentsFile }},
		{name: "environment", mutate: func(recipe *ActionRecipe) { recipe.Environment["ENV"] = "changed" }},
		{name: "working directories", mutate: func(recipe *ActionRecipe) { recipe.WorkingDirectories[0] = "changed" }},
		{name: "working trees", mutate: func(recipe *ActionRecipe) { recipe.WorkingTrees[0] = "changed" }},
		{name: "working inputs", mutate: func(recipe *ActionRecipe) { recipe.WorkingInputs["source:input"] = "changed" }},
		{name: "working outputs", mutate: func(recipe *ActionRecipe) { recipe.WorkingOutputs["output"] = "changed" }},
		{name: "observed outputs", mutate: func(recipe *ActionRecipe) { recipe.ObservedOutputs["observed"] = "changed" }},
		{name: "observed output bases", mutate: func(recipe *ActionRecipe) {
			recipe.ObservedOutputBases["observed"] = []string{"changed"}
		}},
		{name: "observed output base states", mutate: func(recipe *ActionRecipe) {
			recipe.ObservedOutputBases["observed"][0] = "changed"
		}},
		{name: "content substitutions", mutate: func(recipe *ActionRecipe) {
			recipe.ContentSubstitutions["content"] = ActionRecipeContentSubstitution{Input: "input:changed", Transform: "changed"}
		}},
		{name: "command replays", mutate: func(recipe *ActionRecipe) { recipe.CommandReplays[0].Name = "changed" }},
		{name: "command replay invocations", mutate: func(recipe *ActionRecipe) {
			recipe.CommandReplays[0].Invocations[0] = ActionRecipeCommandReplayInvocation{
				Arguments: []string{"changed"}, Outputs: []string{"changed"},
			}
		}},
		{name: "command replay arguments", mutate: func(recipe *ActionRecipe) {
			recipe.CommandReplays[0].Invocations[0].Arguments[0] = "changed"
		}},
		{name: "command replay outputs", mutate: func(recipe *ActionRecipe) {
			recipe.CommandReplays[0].Invocations[0].Outputs[0] = "changed"
		}},
		{name: "compiler invocation arguments", mutate: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.Arguments[0] = "changed"
		}},
		{name: "compiler invocation working input uses", mutate: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.WorkingInputUses[0] = "changed"
		}},
		{name: "compiler invocation auxiliary working input uses", mutate: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.AuxiliaryWorkingInputUses[0] = "changed"
		}},
		{name: "compiler invocation completeness", mutate: func(recipe *ActionRecipe) {
			recipe.CompilerInvocation.WorkingInputUsesComplete = false
		}},
		{name: "executable inputs", mutate: func(recipe *ActionRecipe) { recipe.ExecutableInputs[0] = "changed" }},
		{name: "sources", mutate: func(recipe *ActionRecipe) { recipe.Sources[0] = "changed" }},
		{name: "inputs", mutate: func(recipe *ActionRecipe) { recipe.Inputs[0] = "changed" }},
		{name: "outputs", mutate: func(recipe *ActionRecipe) { recipe.Outputs[0] = "changed" }},
		{name: "trees", mutate: func(recipe *ActionRecipe) { recipe.Trees[0] = "changed" }},
		{name: "auxiliary tools", mutate: func(recipe *ActionRecipe) { recipe.AuxiliaryTools[0] = "changed" }},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name+" source mutation", func(t *testing.T) {
			source := populatedActionRecipeForCloneTest()
			cloned := cloneActionRecipe(source)
			if !reflect.DeepEqual(cloned, source) {
				t.Fatalf("clone differs before mutation\nsource: %#v\n clone: %#v", source, cloned)
			}

			mutation.mutate(&source)
			want := populatedActionRecipeForCloneTest()
			if !reflect.DeepEqual(cloned, want) {
				t.Fatalf("source mutation changed clone\n want: %#v\n  got: %#v", want, cloned)
			}
		})

		t.Run(mutation.name+" clone mutation", func(t *testing.T) {
			source := populatedActionRecipeForCloneTest()
			cloned := cloneActionRecipe(source)
			mutation.mutate(&cloned)

			want := populatedActionRecipeForCloneTest()
			if !reflect.DeepEqual(source, want) {
				t.Fatalf("clone mutation changed source\n want: %#v\n  got: %#v", want, source)
			}
		})
	}
}

func TestCloneActionRecipePreservesNilMutableFields(t *testing.T) {
	source := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc"}
	if cloned := cloneActionRecipe(source); !reflect.DeepEqual(cloned, source) {
		t.Fatalf("zero-value mutable fields changed\nsource: %#v\n clone: %#v", source, cloned)
	}
}

func TestAppendActionPlanNodeOwnsInternedRecipe(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments:   []string{"-o", "${output:00000000}"},
		Environment: map[string]string{"MODE": "original"},
		Outputs:     []string{"00000000"},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	_, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "result.o"}},
	}, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Recipes) != 1 {
		t.Fatalf("interned recipes=%d, want one", len(plan.Recipes))
	}
	var recipeID string
	for recipeID = range plan.Recipes {
	}
	recipe.Arguments[0] = "changed"
	recipe.Environment["MODE"] = "changed"
	recipe.Outputs[0] = "changed"
	stored := plan.Recipes[recipeID]
	if got, err := stored.ID(); err != nil || got != recipeID {
		t.Fatalf("stored recipe identity=(%q,%v), want immutable key %q", got, err, recipeID)
	}
	if stored.Arguments[0] != "-o" || stored.Environment["MODE"] != "original" || stored.Outputs[0] != "00000000" {
		t.Fatalf("caller mutation changed interned recipe: %#v", stored)
	}
}

func actionRecipeInterningTestRecipe() ActionRecipe {
	return ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments:   []string{"-o", "${output:00000000}"},
		Environment: map[string]string{"MODE": "original"},
		Outputs:     []string{"00000000"},
	}
}

func actionRecipeInterningTestNode(index int) ActionPlanNode {
	return ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{
			Tree: "objects", Path: fmt.Sprintf("intern/%08d.o", index),
		}},
	}
}

func TestActionPlanRecipeInterningCanonicalizesAndClonesEachCandidateOnce(t *testing.T) {
	const count = 1024
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	recipe := actionRecipeInterningTestRecipe()
	for index := 0; index < count; index++ {
		if _, err := appendActionPlanNode(plan, actionRecipeInterningTestNode(index), recipe); err != nil {
			t.Fatalf("append candidate %d: %v", index, err)
		}
	}

	if got := len(plan.Recipes); got != 1 {
		t.Fatalf("interned recipes = %d, want 1", got)
	}
	if got := plan.recipeInterning.clones; got != count {
		t.Fatalf("full recipe clones = %d, want one per candidate (%d)", got, count)
	}
	if got := plan.recipeInterning.candidateCanonicalizations; got != count {
		t.Fatalf("candidate canonicalizations = %d, want one per candidate (%d)", got, count)
	}
	if got := plan.recipeInterning.existingCanonicalizations; got != count-1 {
		t.Fatalf("existing recipe canonicalizations = %d, want one live check per duplicate (%d)", got, count-1)
	}
}

func TestAppendPreparedActionPlanNodeClonesItsCallerOnce(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	recipe := actionRecipeInterningTestRecipe()
	if _, err := appendPreparedActionPlanNode(
		plan, actionRecipeInterningTestNode(0), recipe, nil,
	); err != nil {
		t.Fatal(err)
	}
	if got := plan.recipeInterning.clones; got != 1 {
		t.Fatalf("full recipe clones = %d, want 1", got)
	}
	if got := plan.recipeInterning.candidateCanonicalizations; got != 1 {
		t.Fatalf("candidate canonicalizations = %d, want 1", got)
	}

	recipe.Arguments[0] = "changed"
	recipe.Environment["MODE"] = "changed"
	stored := plan.Recipes[plan.Nodes[0].Recipe]
	if stored.Arguments[0] != "-o" || stored.Environment["MODE"] != "original" {
		t.Fatalf("prepared recipe aliases its caller: %#v", stored)
	}
}

func TestAppendPreparedActionPlanNodeOwnsAndPreservesCompilerProbeProjection(t *testing.T) {
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	probe := &actionRecipeCompilerProbeInvocation{
		Tool: "cc", Arguments: []string{"-DORIGINAL=1"},
		Environment: map[string]string{"MODE": "original"},
	}
	node := actionRecipeInterningTestNode(0)
	producer, err := appendPreparedActionPlanNode(plan, node, actionRecipeInterningTestRecipe(), probe)
	if err != nil {
		t.Fatal(err)
	}
	probe.Arguments[0] = "-DCHANGED=1"
	probe.Environment["MODE"] = "changed"
	stored := plan.compilerProbeInvocations[producer]
	if got := stored.Arguments[0]; got != "-DORIGINAL=1" {
		t.Fatalf("stored compiler-probe argument = %q, want owned original", got)
	}
	if got := stored.Environment["MODE"]; got != "original" {
		t.Fatalf("stored compiler-probe environment = %q, want owned original", got)
	}

	duplicate := actionRecipeInterningTestNode(1)
	duplicate.ID = producer
	if _, err := appendPreparedActionPlanNode(
		plan, duplicate, actionRecipeInterningTestRecipe(),
		&actionRecipeCompilerProbeInvocation{Tool: "cc", Arguments: []string{"-DPOISON=1"}},
	); err == nil || !strings.Contains(err.Error(), "repeats provisional node") {
		t.Fatalf("duplicate append error = %v, want node collision", err)
	}
	stored = plan.compilerProbeInvocations[producer]
	if got := stored.Arguments[0]; got != "-DORIGINAL=1" {
		t.Fatalf("failed duplicate poisoned compiler-probe argument to %q", got)
	}
}

func TestActionPlanRecipeInterningBackfillsExternalRecipeOnce(t *testing.T) {
	recipe := actionRecipeInterningTestRecipe()
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{recipeID: cloneActionRecipe(recipe)},
	}
	for index := 0; index < 2; index++ {
		if _, err := appendActionPlanNode(plan, actionRecipeInterningTestNode(index), recipe); err != nil {
			t.Fatalf("append candidate %d: %v", index, err)
		}
	}
	if got := plan.recipeInterning.existingCanonicalizations; got != 2 {
		t.Fatalf("external recipe canonicalizations = %d, want one per duplicate append", got)
	}
	if got := plan.recipeInterning.candidateCanonicalizations; got != 2 {
		t.Fatalf("candidate canonicalizations = %d, want 2", got)
	}
}

func TestActionPlanRecipeInterningRejectsMutatedStoredRecipeImmediately(t *testing.T) {
	recipe := actionRecipeInterningTestRecipe()
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	if _, err := appendActionPlanNode(plan, actionRecipeInterningTestNode(0), recipe); err != nil {
		t.Fatal(err)
	}
	recipeID := plan.Nodes[0].Recipe

	// Mutate a nested map through the public Recipes contract. A duplicate must
	// compare against this live value instead of trusting stale append metadata.
	stored := plan.Recipes[recipeID]
	stored.Environment["MODE"] = "externally-mutated"
	if _, err := appendActionPlanNode(plan, actionRecipeInterningTestNode(1), recipe); err == nil ||
		!strings.Contains(err.Error(), "recipe content ID collision") {
		t.Fatalf("duplicate append error = %v, want immediate live-recipe collision", err)
	}
}
