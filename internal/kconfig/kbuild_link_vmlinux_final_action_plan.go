package kconfig

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

const compactKbuildFinalPhaseEnvironment = "LINUX_BZL_AUTHENTICATED_FINAL_SCRIPT"

// A selected Make command still owns its postscript effects (.vmlinux.cmd and
// fixdep's .vmlinux.d on older sources). Insert a shell -c only at its one
// authenticated script call. The rest of the selected wrapper, including the
// saved original command bytes, remains untouched.
func compactKbuildLinkVmlinuxFinalWrapper(
	profile CompactKbuildProfile,
	template string,
	phase CompactKbuildSelectedSourcePhase,
) (string, error) {
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		return "", fmt.Errorf("inspect selected link-vmlinux wrapper: %w", err)
	}
	insert := -1
	for _, command := range commands {
		if command.program != "sh" || len(command.arguments) == 0 {
			continue
		}
		pathname, source, valid := compactKbuildProfileCommandPath(profile, command.arguments[0])
		if !valid || !source || pathname != phase.SourcePath {
			continue
		}
		if insert >= 0 || !slices.Equal(command.arguments[1:], phase.SourceArguments) ||
			command.programEnd <= command.programStart || command.programEnd > len(template) ||
			command.sourceStart < 0 || command.sourceEnd > len(template) {
			return "", fmt.Errorf("selected link-vmlinux wrapper changes the exact source script invocation")
		}
		insert = command.programEnd
	}
	if insert < 0 {
		return "", fmt.Errorf("selected link-vmlinux wrapper has no exact source script invocation")
	}
	// The private environment value is a declared staged artifact path. sh -c
	// binds the next original source-script word to $0, preserving its source
	// semantics (including the .vmlinux.d text) and all original positional argv.
	return template[:insert] + ` -c '. "$` + compactKbuildFinalPhaseEnvironment + `"'` + template[insert:], nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildLinkVmlinuxFinalSourcePhase(
	owner compactKbuildSelectionKey,
) (CompactKbuildSelectedSourcePhase, CompactKbuildLinkVmlinuxPhases, bool, error) {
	var zero CompactKbuildSelectedSourcePhase
	var none CompactKbuildLinkVmlinuxPhases
	if b == nil || b.selectionGraph == nil || !b.selectionBound || b.selection != owner || b.profile == nil {
		return zero, none, false, nil
	}
	keys := b.selectionGraph.sourcePhasesByOwner[owner]
	if len(keys) == 0 {
		return zero, none, false, nil
	}
	if len(keys) != 2 {
		return zero, none, false, fmt.Errorf("selected link-vmlinux owner %s lacks its two source phases", compactKbuildSelectionKeyString(owner))
	}
	phase, _, found := b.selectionGraph.selectedSourcePhase(keys[1])
	if !found || phase.Ordinal != 1 || phase.OwnerTarget != owner.target {
		return zero, none, false, fmt.Errorf("selected link-vmlinux owner %s has no exact object writer", compactKbuildSelectionKeyString(owner))
	}
	content, err := readCompactKbuildProfileSource(*b.profile, phase.SourcePath)
	if err != nil {
		return zero, none, false, err
	}
	analyzed, recognized, err := AnalyzeCompactKbuildLinkVmlinuxPhases(string(content))
	if err != nil || !recognized || analyzed.SourceSHA256 != phase.SourceSHA256 ||
		!slices.Equal(analyzed.ObjectSpans, phase.Spans) {
		return zero, none, false, fmt.Errorf("selected link-vmlinux final source no longer matches its writer: recognized=%t: %w", recognized, err)
	}
	return phase, analyzed, true, nil
}

// A separate source-only action authenticates the final immutable span. The
// real Make action stages this private script and runs it within the original
// selected wrapper, so the two earlier children cannot become dependencies of
// the writer which they themselves require.
func (b *compactKbuildRulePlanBuilder) emitCompactKbuildLinkVmlinuxFinal(
	phase CompactKbuildSelectedSourcePhase,
	analyzed CompactKbuildLinkVmlinuxPhases,
) (compactKbuildRuleInput, error) {
	if b == nil || b.profile == nil || phase.SourceSHA256 != analyzed.SourceSHA256 ||
		len(analyzed.FinalSpans) != 2 || analyzed.FinalScript == "" {
		return compactKbuildRuleInput{}, fmt.Errorf("selected final source span has no authenticated content")
	}
	outputPath := path.Join(".linux-bzl-source-phases", analyzed.SourceSHA256, "final.sh")
	sourceID, err := b.metadata.ensureActionPlanSource(b.plan, phase.SourcePath)
	if err != nil {
		return compactKbuildRuleInput{}, err
	}
	node := b.actionNode("generate", compactKbuildScriptRunnerRole, outputPath)
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{}, WorkingDirectory: "kbuild-source-phase-final-" + escapeKbuildIdentifier(b.selection.target),
		WorkingOutputs: map[string]string{"00000000": outputPath}, Outputs: []string{"00000000"},
	}
	scriptKey, err := appendCompactKbuildRecipeInput(&node, &recipe,
		compactKbuildRuleInput{path: phase.SourcePath, sourceID: sourceID}, "script")
	if err != nil {
		return compactKbuildRuleInput{}, err
	}
	recipe.Arguments = append(recipe.Arguments,
		"-script", "${source:"+scriptKey+"}", "-script_source_sha256", analyzed.SourceSHA256)
	for _, span := range analyzed.FinalSpans {
		recipe.Arguments = append(recipe.Arguments, "-script_source_span", strconv.Itoa(span.Start)+":"+strconv.Itoa(span.End))
	}
	recipe.Arguments = append(recipe.Arguments, "-script_source_emit", "${work:root}/"+outputPath)
	if err := appendReferencedPlanTrees(b.plan, &node, &recipe); err != nil {
		return compactKbuildRuleInput{}, err
	}
	producer, err := appendActionPlanNode(b.plan, node, recipe)
	if err != nil {
		return compactKbuildRuleInput{}, err
	}
	return compactKbuildRuleInput{path: outputPath, producer: producer, slot: 0, workingOnly: true}, nil
}

func (b *compactKbuildRulePlanBuilder) compactKbuildFinalReplay(
	owner compactKbuildSelectionKey,
	replays []ActionRecipeCommandReplay,
) ([]ActionRecipeCommandReplay, error) {
	phases := b.selectionGraph.sourcePhasesByOwner[owner]
	if len(phases) != 2 || len(replays) != 1 || replays[0].Name != CompactKbuildRecursiveMakeReplayName ||
		len(replays[0].Invocations) != 2 {
		return nil, fmt.Errorf("selected link-vmlinux final wrapper has no ordered two-child command replay")
	}
	second := b.selectionGraph.sourcePhaseChildren[phases[1]]
	first := b.selectionGraph.sourcePhaseChildren[phases[0]]
	if first == "" || second == "" || first == second {
		return nil, fmt.Errorf("selected link-vmlinux final wrapper has ambiguous child provenance")
	}
	children := []string{first, second}
	for index, invocation := range replays[0].Invocations {
		matching := false
		for _, dependency := range b.profile.TargetInvocationDependencies {
			if dependency.Prerequisite || dependency.Target != owner.target || dependency.Profile != children[index] ||
				len(dependency.ReplayArguments) != len(invocation.Arguments) {
				continue
			}
			arguments := make([]string, len(dependency.ReplayArguments))
			for position, argument := range dependency.ReplayArguments {
				arguments[position] = compactKbuildSourceScriptReplayValue(*b.profile, argument)
			}
			if slices.Equal(arguments, invocation.Arguments) {
				matching = true
				break
			}
		}
		if !matching {
			return nil, fmt.Errorf("selected link-vmlinux final wrapper child %q changes source replay arguments", children[index])
		}
	}
	replays[0].Invocations = replays[0].Invocations[1:]
	return replays, nil
}

// Validate the retained wrapper and save the inserted action-private path in
// an environment binding. No original Make text (notably cmd_vmlinux := ...) is
// synthesized or changed by this transformation.
func (b *compactKbuildRulePlanBuilder) compactKbuildFinalWrapper(
	template string,
	phase CompactKbuildSelectedSourcePhase,
	input compactKbuildRuleInput,
	environment map[string]string,
) (string, error) {
	if _, exists := environment[compactKbuildFinalPhaseEnvironment]; exists || input.producer == "" {
		return "", fmt.Errorf("selected link-vmlinux wrapper conflicts with authenticated private source binding")
	}
	if !strings.HasPrefix(input.path, ".linux-bzl-source-phases/") {
		return "", fmt.Errorf("selected link-vmlinux wrapper has invalid source phase path %q", input.path)
	}
	environment[compactKbuildFinalPhaseEnvironment] = "${work:root}/" + input.path
	return compactKbuildLinkVmlinuxFinalWrapper(*b.profile, template, phase)
}

// The final Make wrapper executes only the authenticated final source spans.
// Module metadata and the symbol map are written after the second recursive
// Make boundary; publishing them from the earlier vmlinux.o action would make
// that action claim files which cannot yet exist in its private working tree.
func (b *compactKbuildRulePlanBuilder) appendCompactKbuildLinkVmlinuxFinalOutputs(
	analyzed CompactKbuildLinkVmlinuxPhases,
	declared []ActionPlanOutput,
	recipe ActionRecipe,
) ([]ActionPlanOutput, error) {
	if b == nil || b.profile == nil ||
		len(analyzed.FinalSpans) != 2 || analyzed.FinalScript == "" {
		return nil, fmt.Errorf("selected link-vmlinux final output lacks an authenticated source span")
	}
	outputs := make([]ActionPlanOutput, 0, 3)
	for _, output := range []ActionPlanOutput{
		{Tree: "metadata", Path: "modules.builtin.modinfo"},
		{Tree: "metadata", Path: "modules.builtin"},
		{Tree: "vmlinux", Path: "System.map"},
	} {
		var writes bool
		var err error
		if output.Path == "System.map" {
			writes, err = compactKbuildLinkVmlinuxFinalWritesSystemMap(*b.profile, analyzed, recipe)
		} else {
			writes, err = compactKbuildSourceScriptContentDeclaresOutput(
				*b.profile, "scripts/link-vmlinux.sh", analyzed.FinalScript, recipe, output.Path,
			)
			if err == nil && writes {
				writes, err = compactKbuildLinkVmlinuxFinalTopLevelWrite(*b.profile, analyzed, recipe, output.Path)
			}
		}
		if err != nil || !writes {
			return nil, fmt.Errorf("selected link-vmlinux final span has no declared write to %q: %w", output.Path, err)
		}
		for _, previous := range declared {
			if previous.Path == output.Path || previous.ObservedPath == output.Path {
				return nil, fmt.Errorf("selected link-vmlinux final output %q is already declared or observed", output.Path)
			}
		}
		outputs = append(outputs, output)
	}
	return outputs, nil
}

// System.map comes from a selected top-level call through the immutable
// mksysmap function and helper. Authenticate the function's shell invocation,
// its actual argument-to-output mapping, and the helper's stdout redirect.
func compactKbuildLinkVmlinuxFinalWritesSystemMap(
	profile CompactKbuildProfile,
	analyzed CompactKbuildLinkVmlinuxPhases,
	recipe ActionRecipe,
) (bool, error) {
	const function = "mksysmap()\n{\n\t${CONFIG_SHELL} \"${srctree}/scripts/mksysmap\" ${1} ${2}\n}\n"
	if strings.Count(analyzed.FinalScript, function) != 1 || recipe.Environment["CONFIG_SHELL"] != "sh" {
		return false, nil
	}
	writes, err := compactKbuildLinkVmlinuxFinalTopLevelWrite(profile, analyzed, recipe, "System.map")
	if err != nil || !writes {
		return writes, err
	}
	helper, err := readCompactKbuildProfileSource(profile, "scripts/mksysmap")
	if err != nil {
		return false, fmt.Errorf("read selected System.map helper: %w", err)
	}
	lines := strings.Split(string(helper), "\n")
	redirects := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "$NM -n $1 | grep -v ") && strings.HasSuffix(line, " > $2") {
			redirects++
			continue
		}
		return false, fmt.Errorf("selected System.map helper has unsupported write %q", line)
	}
	return redirects == 1, nil
}

// A source write can be claimed by an action only when its selected shell
// command is reachable outside a conditional or loop in that action's own
// final span. The prelude contains function definitions and is excluded here.
func compactKbuildLinkVmlinuxFinalTopLevelWrite(
	profile CompactKbuildProfile,
	analyzed CompactKbuildLinkVmlinuxPhases,
	recipe ActionRecipe,
	output string,
) (bool, error) {
	if len(analyzed.FinalSpans) != 2 || analyzed.FinalSpans[0].End-analyzed.FinalSpans[0].Start > len(analyzed.FinalScript) {
		return false, fmt.Errorf("selected final link source has no exact post-modpost span")
	}
	tail := analyzed.FinalScript[analyzed.FinalSpans[0].End-analyzed.FinalSpans[0].Start:]
	depth := 0
	writes := 0
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A successful early exit or an unmodeled shell list can skip any
		// subsequent writer. A cleanup command after a write can remove it
		// before mapdirectoryrecipe collects the declared working output.
		if line == "exit" || strings.HasPrefix(line, "exit ") && line != "exit 1" ||
			line == "return" || strings.HasPrefix(line, "return ") ||
			line == "exec" || strings.HasPrefix(line, "exec ") ||
			!strings.HasPrefix(line, "if ") && (strings.Contains(line, "&&") || strings.Contains(line, "||")) {
			return false, fmt.Errorf("selected final source can skip its %q writer: %q", output, line)
		}
		if writes != 0 && compactKbuildFinalSourceMayRemoveOutput(line, output) {
			return false, fmt.Errorf("selected final source removes %q after writing it: %q", output, line)
		}
		switch {
		case strings.HasPrefix(line, "for ") || strings.HasPrefix(line, "while ") ||
			strings.HasPrefix(line, "until ") || strings.HasPrefix(line, "case ") ||
			strings.HasPrefix(line, "select ") || strings.HasSuffix(line, "()") ||
			strings.HasPrefix(line, "alias ") || strings.HasPrefix(line, "function ") ||
			line == "eval" || strings.HasPrefix(line, "eval ") ||
			line == "cleanup" || strings.HasPrefix(line, "cleanup "):
			return false, fmt.Errorf("selected final source can replace or skip its %q writer: %q", output, line)
		case strings.HasPrefix(line, "if "):
			depth++
		case line == "fi" || line == "fi;":
			depth--
			if depth < 0 {
				return false, fmt.Errorf("selected System.map source has unmatched shell condition")
			}
		case line == "mksysmap vmlinux System.map" && output == "System.map":
			if depth == 0 {
				writes++
			}
		case output != "System.map" && strings.Contains(line, output):
			if depth != 0 {
				continue
			}
			// Prove the output grammar for this exact command line; a mention
			// in an info message or an earlier conditional is insufficient.
			writer, err := compactKbuildSourceScriptContentDeclaresOutput(profile,
				"scripts/link-vmlinux.sh", "#!/bin/sh\n"+line+"\n", recipe, output)
			if err != nil {
				return false, err
			}
			if writer {
				writes++
			}
		}
	}
	return writes == 1 && depth == 0, nil
}

func compactKbuildFinalSourceMayRemoveOutput(line, output string) bool {
	words := strings.Fields(line)
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "rm", "unlink", "mv":
		for _, word := range words[1:] {
			word = strings.Trim(word, `"'`)
			if word == output || word == "./"+output ||
				strings.Contains(word, "${") || strings.Contains(word, "$output") {
				return true
			}
		}
	}
	return false
}
