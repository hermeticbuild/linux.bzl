package kconfig

import (
	"reflect"
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
