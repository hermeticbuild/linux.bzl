package kconfig

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
)

// ActionRecipeMakePhonyCompletion retains the source selection used to lower
// one PHONY status command. RecipeIndex == -1 describes a final private `:`
// after source-selected recursive commands; otherwise ExpandedLine is the
// complete expanded shell line at that source recipe index. The selected-line
// planner owns the source proof. A checkpoint retains this receipt, but cannot
// re-evaluate Make's frozen line state after restoration.
type ActionRecipeMakePhonyCompletion struct {
	Profile      string `json:"profile"`
	Target       string `json:"target"`
	SourcePath   string `json:"source_path"`
	RuleIndex    int    `json:"rule_index"`
	RecipeIndex  int    `json:"recipe_index"`
	ExpandedLine string `json:"expanded_line"`
	// A selected command template may wrap one immutable source-script call.
	// SelectedLine preserves the frozen Make result; ExpandedLine binds the
	// projected shell bytes which the private runner actually executes.
	SelectedLine string `json:"selected_line,omitempty"`
	ScriptPath   string `json:"script_path,omitempty"`
	// ActionScope determines whether a source-owned role names the selected
	// node's toolset or the explicitly opposite scoped toolset. It is part of
	// the wrapped source command's immutable execution receipt.
	ActionScope string `json:"action_scope,omitempty"`
	// ExpandedLines records a complete selected recipe with bounded private
	// effects. Each line ran in its own shell against a frozen Make state.
	ExpandedLines []string `json:"expanded_lines,omitempty"`
	// SequenceInputs counts exact recursive/native predecessors ordered before
	// this private status command. Their source identities are checked when the
	// selected line is lowered; the node retains their graph edges here.
	SequenceInputs int `json:"sequence_inputs"`
}

func validateActionRecipeMakePhonyCompletion(recipe ActionRecipe) error {
	completion := recipe.MakePhonyCompletion
	if completion == nil {
		return nil
	}
	if completion.Profile == "" || len(completion.Profile) > 512 ||
		strings.ContainsRune(completion.Profile, 0) {
		return fmt.Errorf("PHONY Make completion has an invalid profile identity")
	}
	for label, pathname := range map[string]string{
		"PHONY Make target":     completion.Target,
		"PHONY Makefile source": completion.SourcePath,
	} {
		if err := validatePlanRelativePath(label, pathname); err != nil {
			return err
		}
	}
	if completion.ScriptPath != "" {
		if err := validatePlanRelativePath("PHONY source script", completion.ScriptPath); err != nil {
			return err
		}
	}
	if completion.RuleIndex < 0 || completion.RecipeIndex < -1 ||
		completion.SequenceInputs < 0 || completion.SequenceInputs > maximumActionPlanInputBindingCount ||
		completion.RecipeIndex == -1 && completion.SequenceInputs == 0 {
		return fmt.Errorf("PHONY Make completion has invalid rule or recipe index")
	}
	wrapped := completion.ScriptPath != ""
	privateInline := len(completion.ExpandedLines) != 0
	private := privateInline || wrapped && len(recipe.PrivateWorkingEffects) != 0
	switch {
	case wrapped:
		if completion.RecipeIndex != 0 || completion.SelectedLine == "" ||
			(completion.ActionScope != "target" && completion.ActionScope != "host") ||
			len(completion.SelectedLine) > 1<<20 || strings.ContainsRune(completion.SelectedLine, 0) ||
			privateInline || completion.ExpandedLine == "" ||
			len(completion.ExpandedLine) > 1<<20 || strings.ContainsRune(completion.ExpandedLine, 0) {
			return fmt.Errorf("PHONY Make completion has invalid source-script wrapper")
		}
	case privateInline:
		if completion.SelectedLine != "" || completion.ActionScope != "" || completion.RecipeIndex != 0 ||
			completion.ExpandedLine != "" || len(completion.ExpandedLines) > 128 {
			return fmt.Errorf("PHONY Make completion has invalid private setup lines")
		}
		effects, proved := compactKbuildPhonyPrivateInlineEffects(completion.Target, completion.ExpandedLines)
		if !proved || !slices.Equal(effects, recipe.PrivateWorkingEffects) {
			return fmt.Errorf("PHONY Make completion does not authenticate its bounded private effects")
		}
		for _, line := range completion.ExpandedLines {
			if line == "" || len(line) > 1<<20 || strings.ContainsRune(line, 0) {
				return fmt.Errorf("PHONY Make completion has invalid expanded private line")
			}
		}
	default:
		if completion.SelectedLine != "" || completion.ActionScope != "" || completion.RecipeIndex == -1 && completion.ExpandedLine != ":" ||
			completion.RecipeIndex >= 0 && (completion.ExpandedLine == "" || len(completion.ExpandedLine) > 1<<20) ||
			strings.ContainsRune(completion.ExpandedLine, 0) {
			return fmt.Errorf("PHONY Make completion has invalid expanded shell line")
		}
	}
	if recipe.Kind != "generate" || recipe.Tool != compactKbuildScriptRunnerRole ||
		recipe.ArgumentsFile || len(recipe.ArgumentTransforms) != 0 ||
		recipe.RequireAbsentObservedOutput != planOrdinal(0) ||
		recipe.RequireUnchangedWorkingTree == private ||
		!private && len(recipe.PrivateWorkingEffects) != 0 || len(recipe.WorkingOutputs) != 0 ||
		recipe.Stdout != "" || recipe.Stdin != "" || recipe.WorkingDirectory == "" ||
		len(recipe.CommandReplays) != 0 || len(recipe.Outputs) != 1 ||
		recipe.Outputs[0] != planOrdinal(0) || len(recipe.ObservedOutputs) != 1 ||
		recipe.ObservedOutputs[planOrdinal(0)] != completion.Target {
		return fmt.Errorf("PHONY Make completion lacks a private status-only script recipe")
	}
	// Consume exactly the flags accepted for this one script. The runner also
	// recognizes `-flag=value` and positional arguments; rejecting both here
	// prevents another script flag from superseding the checked content.
	encoded := ""
	for index := 0; index < len(recipe.Arguments); index += 2 {
		if index+1 >= len(recipe.Arguments) || strings.Contains(recipe.Arguments[index], "=") {
			return fmt.Errorf("PHONY Make completion has malformed script runner flags")
		}
		switch recipe.Arguments[index] {
		case "-interpreter", "-interpreter_arg", "-multicall", "-tool", "-tree",
			"-applet", "-require_applet", "-literal_tree_offset", "-max_file_size_bytes":
			continue
		case "-script_content_base64":
			if encoded != "" {
				return fmt.Errorf("PHONY Make completion repeats script content")
			}
			encoded = recipe.Arguments[index+1]
		default:
			return fmt.Errorf("PHONY Make completion supplies an unsupported script runner flag %q", recipe.Arguments[index])
		}
	}
	if encoded == "" {
		return fmt.Errorf("PHONY Make completion has no encoded script content")
	}
	script, err := base64.StdEncoding.Strict().DecodeString(encoded)
	expandedScript := completion.ExpandedLine
	if private && !wrapped {
		expandedScript = compactKbuildRecipeLineShells(completion.ExpandedLines)
	}
	if err != nil || base64.StdEncoding.EncodeToString(script) != encoded ||
		string(script) != "#!/bin/sh\nset -e\n"+expandedScript+"\n" {
		return fmt.Errorf("PHONY Make completion script differs from its selected shell line")
	}
	if wrapped {
		effects, effectErr := compactKbuildWrappedSourceCheckEffects(recipe)
		if effectErr != nil || !slices.Equal(effects, recipe.PrivateWorkingEffects) {
			return fmt.Errorf("PHONY source-script completion has an unproven compiler invocation or private depfile: %v", effectErr)
		}
	}
	return nil
}

func compactKbuildMakePhonyCompletion(plan *ActionPlan, node ActionPlanNode, target string) bool {
	if plan == nil || node.Tool != compactKbuildScriptRunnerRole ||
		node.Kind != "generate" || len(node.Outputs) != 1 || target == "" {
		return false
	}
	recipe, exists := plan.Recipes[node.Recipe]
	if !exists || recipe.MakePhonyCompletion == nil ||
		validateActionRecipeMakePhonyCompletion(recipe) != nil ||
		recipe.Kind != node.Kind || recipe.Tool != node.Tool ||
		recipe.MakePhonyCompletion.Target != target {
		return false
	}
	if receiptScope := recipe.MakePhonyCompletion.ActionScope; receiptScope != "" {
		nodeScope := "target"
		if node.Stage == "prehost" || node.Stage == "host" {
			nodeScope = "host"
		}
		if receiptScope != nodeScope {
			return false
		}
	}
	sequenceInputs := 0
	predecessors := map[string]bool{}
	for ordinal, input := range node.Inputs {
		if input.Role != "sequence" {
			continue
		}
		predecessor := fmt.Sprintf("%s:%d", input.ProducerID, input.Slot)
		if predecessors[predecessor] ||
			!slices.Contains(recipe.Inputs, fmt.Sprintf("sequence:%08d", ordinal)) {
			return false
		}
		predecessors[predecessor] = true
		sequenceInputs++
	}
	if sequenceInputs != recipe.MakePhonyCompletion.SequenceInputs {
		return false
	}
	boundMakefile, boundScript := false, false
	for ordinal, source := range node.Sources {
		if source.Role == "script" {
			if boundScript || recipe.MakePhonyCompletion.ScriptPath == "" ||
				!slices.Contains(recipe.Sources, fmt.Sprintf("script:%08d", ordinal)) {
				return false
			}
			for _, declaration := range plan.Sources {
				if declaration.ID == source.SourceID && declaration.Namespace == "kernel" &&
					declaration.Path == recipe.MakePhonyCompletion.ScriptPath {
					boundScript = true
					break
				}
			}
			if !boundScript {
				return false
			}
			continue
		}
		if source.Role != "makefile" {
			continue
		}
		if boundMakefile || !slices.Contains(recipe.Sources, fmt.Sprintf("makefile:%08d", ordinal)) {
			return false
		}
		for _, declaration := range plan.Sources {
			if declaration.ID == source.SourceID && declaration.Namespace == "kernel" &&
				declaration.Path == recipe.MakePhonyCompletion.SourcePath {
				boundMakefile = true
				break
			}
		}
		if !boundMakefile {
			return false
		}
	}
	output := node.Outputs[0]
	return boundMakefile && boundScript == (recipe.MakePhonyCompletion.ScriptPath != "") &&
		actionPlanStageOwnsOutputTree(node.Stage, output.Tree) &&
		output.ObservedPath == target && actionPlanOutputIsCanonical(output) &&
		strings.HasPrefix(output.Path, ".linux-bzl-intermediate/")
}

// Execution checks may be selected source scripts or source-selected Make
// recipe status actions. Both own only private observed-state completions.
func compactKbuildAuthenticatedExecutionCheckCompletion(plan *ActionPlan, node ActionPlanNode, target string) bool {
	if plan != nil {
		if recipe, exists := plan.Recipes[node.Recipe]; exists && recipe.MakePhonyCompletion != nil {
			return compactKbuildMakePhonyCompletion(plan, node, target)
		}
	}
	return compactKbuildSourceCheckCompletion(plan, node, target) ||
		compactKbuildMakePhonyCompletion(plan, node, target)
}
