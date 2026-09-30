package kconfig

// Terminal planning only validates and projects the native artifacts produced
// by the selected Kbuild graph. It must never re-enter rule lowering: target
// selection and recursive prerequisite discovery are owned by
// appendGeneratedActionPlan.

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
)

func (m *CompactMetadata) appendTerminalActionPlanNodes(
	plan *ActionPlan,
	selections *compactKbuildSelectionGraph,
) error {
	if m == nil {
		return fmt.Errorf("terminal action plan requires one resolved config")
	}
	if plan == nil {
		return fmt.Errorf("terminal action plan requires a non-nil action plan")
	}
	if selections == nil {
		return fmt.Errorf("terminal action plan requires the selected Kbuild graph")
	}
	config := m.Config
	vmlinuxProducer := ""

	for _, product := range []struct {
		name   string
		target string
	}{
		{name: "vmlinux", target: "vmlinux"},
		{name: "modules_builtin_modinfo", target: "modules.builtin.modinfo"},
		{name: "modules_builtin", target: "modules.builtin"},
	} {
		producer, tree := "", ""
		if product.name != "vmlinux" {
			if _, selected := selections.owner(product.target); !selected {
				owner, selected := selections.owner("vmlinux")
				if !selected {
					return fmt.Errorf("terminal product %s has no selected vmlinux invocation", product.name)
				}
				if phases := selections.sourcePhasesByOwner[owner]; len(phases) != 0 {
					if len(phases) != 2 {
						return fmt.Errorf("terminal product %s has incomplete selected link-vmlinux source phases", product.name)
					}
					writer, _, exists := planProducerByOutput(plan, "metadata", product.target)
					if !exists || writer != vmlinuxProducer {
						return fmt.Errorf("terminal product %s requires its authenticated final link-vmlinux writer %q, found %q", product.name, vmlinuxProducer, writer)
					}
					tree = "metadata"
				} else {
					profile, exists := selections.profile(owner.profile)
					if !exists {
						return fmt.Errorf("terminal product %s references absent vmlinux profile %q", product.name, owner.profile)
					}
					output := ActionPlanOutput{Tree: "metadata", Path: product.target}
					if err := appendNearestSourceScriptSideOutputWithValidation(
						plan, vmlinuxProducer, output,
						func(recipe ActionRecipe) (bool, error) {
							return compactKbuildSelectedSourceScriptDeclaresOutput(profile, "scripts/link-vmlinux.sh", recipe, product.target)
						},
					); err != nil {
						return fmt.Errorf("terminal product %s has neither a selected Make writer nor an authenticated source-script writer: %w", product.name, err)
					}
					if _, _, exists := planProducerByOutput(plan, "metadata", product.target); !exists {
						return fmt.Errorf("terminal product %s source script did not materialize metadata/%s", product.name, product.target)
					}
					tree = "metadata"
				}
			} else {
				var err error
				producer, _, tree, err = selectedKbuildPlanOutput(plan, config, selections, product.target)
				if err != nil {
					return fmt.Errorf("terminal product %s: %w", product.name, err)
				}
			}
		} else {
			var err error
			producer, _, tree, err = selectedKbuildPlanOutput(plan, config, selections, product.target)
			if err != nil {
				return fmt.Errorf("terminal product %s: %w", product.name, err)
			}
		}
		if err := appendActionPlanProduct(plan, ActionPlanProduct{
			Name: product.name, Tree: tree, Path: product.target,
		}); err != nil {
			return err
		}
		if product.name == "vmlinux" {
			vmlinuxProducer = producer
		}
	}

	// System.map is a declared side output of Linux's source-owned
	// link-vmlinux command. Generic source-script lowering materializes it in the
	// same private working directory as the upstream image; terminal handling only
	// exposes that already-owned path as a stable product.
	owner, selected := selections.owner("vmlinux")
	if !selected {
		return fmt.Errorf("selected link-vmlinux graph has no terminal owner")
	}
	mapWriter, _, hasMapWriter := planProducerByOutput(plan, "vmlinux", "System.map")
	if phases := selections.sourcePhasesByOwner[owner]; len(phases) != 0 {
		if len(phases) != 2 || !hasMapWriter || mapWriter != vmlinuxProducer {
			return fmt.Errorf("selected link-vmlinux graph has no authenticated final System.map writer %q (found %q)", vmlinuxProducer, mapWriter)
		}
	} else if !hasMapWriter {
		if err := appendNearestSourceScriptSideOutput(plan, vmlinuxProducer, ActionPlanOutput{
			Tree: "vmlinux", Path: "System.map",
		}); err != nil {
			return fmt.Errorf("selected link-vmlinux graph omits vmlinux/System.map: %w", err)
		}
	}
	if err := appendActionPlanProduct(plan, ActionPlanProduct{
		Name: "system_map", Tree: "vmlinux", Path: "System.map",
	}); err != nil {
		return err
	}

	imageTarget, err := terminalImageEntryTarget(config)
	if err != nil {
		return err
	}
	image, imageSlot, imageTree, err := selectedKbuildPlanOutput(plan, config, selections, imageTarget)
	if err != nil {
		return fmt.Errorf("terminal product image: %w", err)
	}
	if imageTree != "image" {
		return fmt.Errorf("selected KBUILD_IMAGE target %q is in output tree %q, want image", imageTarget, imageTree)
	}
	if imageTarget != "kernel" {
		if _, err := appendActionPlanProjection(plan, image, imageSlot, "image", "image", "kernel"); err != nil {
			return fmt.Errorf("project selected KBUILD_IMAGE target %q: %w", imageTarget, err)
		}
	}
	return appendActionPlanProduct(plan, ActionPlanProduct{Name: "image", Tree: "image", Path: "kernel"})
}

// appendNearestSourceScriptSideOutput turns a stable facade demand into a
// declared output of the nearest source-owned script in that terminal's native
// dependency chain. The selected terminal itself is the first layer: some
// Linux versions link vmlinux there, while others add a native post-link action
// and retain the source script as an upstream producer.
func appendNearestSourceScriptSideOutput(plan *ActionPlan, terminalProducer string, output ActionPlanOutput) error {
	return appendNearestSourceScriptSideOutputWithValidation(plan, terminalProducer, output, nil)
}

func appendNearestSourceScriptSideOutputWithValidation(
	plan *ActionPlan,
	terminalProducer string,
	output ActionPlanOutput,
	validatedWrite func(ActionRecipe) (bool, error),
) error {
	if plan == nil || terminalProducer == "" {
		return fmt.Errorf("terminal producer is missing")
	}
	previousProducer, _, previouslyOwned := planProducerByOutput(plan, output.Tree, output.Path)
	indexByID := make(map[string]int, len(plan.Nodes))
	for index, node := range plan.Nodes {
		indexByID[node.ID] = index
	}
	if _, ok := indexByID[terminalProducer]; !ok {
		return fmt.Errorf("terminal producer %q is absent from the plan", terminalProducer)
	}
	queue := []string{terminalProducer}
	visited := map[string]bool{}
	for len(queue) != 0 {
		pending := make([]string, 0, len(queue))
		for _, producer := range queue {
			if !visited[producer] {
				pending = append(pending, producer)
			}
		}
		var err error
		pending, err = nativeProducerFrontier(plan.Nodes, indexByID, pending)
		if err != nil {
			return err
		}
		next := []string{}
		candidates := []int{}
		for _, producer := range pending {
			visited[producer] = true
			index, exists := indexByID[producer]
			if !exists {
				return fmt.Errorf("terminal dependency %q is absent from the plan", producer)
			}
			node := plan.Nodes[index]
			recipe, exists := plan.Recipes[node.Recipe]
			if exists && actionPlanNodeExecutesImmutableSourceScriptPath(
				plan, indexByID, node, recipe, "kernel", "scripts/link-vmlinux.sh",
			) {
				candidates = append(candidates, index)
			}
			next = append(next, terminalNativeInputProducers(node)...)
		}
		if len(candidates) != 0 {
			if len(candidates) != 1 {
				details := make([]string, 0, len(candidates))
				for _, index := range candidates {
					node := plan.Nodes[index]
					recipe := plan.Recipes[node.Recipe]
					details = append(details, fmt.Sprintf("%s(tool=%s outputs=%v args=%v)", node.ID, recipe.Tool, node.Outputs, recipe.Arguments))
				}
				return fmt.Errorf("nearest dependency layer has %d source-script producers: %s", len(candidates), strings.Join(details, "; "))
			}
			index := candidates[0]
			node := plan.Nodes[index]
			if validatedWrite != nil {
				writes, err := validatedWrite(plan.Recipes[node.Recipe])
				if err != nil {
					return fmt.Errorf("selected immutable source script output %q: %w", output.Path, err)
				}
				if !writes {
					return fmt.Errorf("selected immutable source script has no declared write to %q", output.Path)
				}
			}
			if previouslyOwned {
				if previousProducer != node.ID {
					return fmt.Errorf("terminal side output %q has unrelated producer %q; selected immutable script is %q", output.Path, previousProducer, node.ID)
				}
				return nil
			}
			recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
			// Recipes are content-addressed and may be shared by more than one
			// node. Derive a fully independent recipe so extending this node does
			// not corrupt the canonical recipe still stored under its old ID.
			if recipe.WorkingOutputs == nil {
				recipe.WorkingOutputs = map[string]string{}
			}
			binding := planOrdinal(len(node.Outputs))
			for _, workingPath := range recipe.WorkingOutputs {
				if workingPath == output.Path {
					return fmt.Errorf("source-script working output %q is already bound", output.Path)
				}
			}
			node.Outputs = append(append([]ActionPlanOutput(nil), node.Outputs...), output)
			recipe.Outputs = append(recipe.Outputs, binding)
			recipe.WorkingOutputs[binding] = output.Path
			recipeID, err := recipe.ID()
			if err != nil {
				return fmt.Errorf("declare source-script side output %s/%s: %w", output.Tree, output.Path, err)
			}
			plan.Recipes[recipeID] = recipe
			node.Recipe = recipeID
			plan.Nodes[index] = node
			plan.invalidateLookupIndexes()
			return nil
		}
		queue = next
	}
	return fmt.Errorf("terminal dependency chain has no source-owned script action")
}

// compactKbuildSelectedSourceScriptDeclaresOutput scans the selected immutable
// program for a finite source-shell write. A literal POSIX stdout redirect
// names a writable file; configured objcopy's explicit infile/outfile grammar
// names the binary copied out of the selected image. Branches remain in the
// original script action and its working output is collected only if the
// actual selected execution writes it.
func compactKbuildSelectedSourceScriptDeclaresOutput(
	profile CompactKbuildProfile,
	scriptPath string,
	recipe ActionRecipe,
	output string,
) (bool, error) {
	content, err := readCompactKbuildProfileSource(profile, scriptPath)
	if err != nil {
		return false, fmt.Errorf("read declared source %s: %w", scriptPath, err)
	}
	return compactKbuildSourceScriptContentDeclaresOutput(profile, scriptPath, string(content), recipe, output)
}

// A split source script must prove the write in the bytes its selected action
// actually executes. The immutable whole-script source contains earlier
// phases whose outputs cannot be collected from the final action (and vice
// versa).
func compactKbuildSourceScriptContentDeclaresOutput(
	profile CompactKbuildProfile,
	scriptPath, content string,
	recipe ActionRecipe,
	output string,
) (bool, error) {
	_, commands, err := prepareCompactKbuildSourceScript(content)
	if err != nil {
		return false, fmt.Errorf("inspect declared source %s: %w", scriptPath, err)
	}
	tokens, err := lexCompactKbuildRecipe(commands)
	if err != nil {
		return false, fmt.Errorf("lex declared source %s: %w", scriptPath, err)
	}
	// A shell program can change the role named by its exported environment.
	// Static writer recognition only applies while that configured binding is
	// stable. The generated auto.conf source is admitted separately by the
	// declared, validated CONFIG_* shell-assignment contract.
	for index, token := range tokens {
		if token.operator {
			continue
		}
		if strings.HasPrefix(token.value, "OBJCOPY=") || token.value == "eval" ||
			(token.value == "OBJCOPY" && index+1 < len(tokens) && tokens[index+1].value == "(") ||
			(token.value == "unset" && index+1 < len(tokens) &&
				(tokens[index+1].value == "OBJCOPY" ||
					index+2 < len(tokens) && tokens[index+1].value == "-v" && tokens[index+2].value == "OBJCOPY")) {
			return false, fmt.Errorf("source script %s changes its configured OBJCOPY binding", scriptPath)
		}
		if token.value == "." && index+1 < len(tokens) && !tokens[index+1].operator {
			declared, err := compactKbuildDeclaredGeneratedShellSource(profile, tokens[index+1].value)
			if err != nil {
				return false, fmt.Errorf("source script %s sources %q: %w", scriptPath, tokens[index+1].value, err)
			}
			if !declared {
				return false, fmt.Errorf("source script %s sources unverified shell assignments from %q", scriptPath, tokens[index+1].value)
			}
		}
	}
	if err := validatePlanRelativePath("source-script terminal output", output); err != nil {
		return false, err
	}
	write := false
	command := []compactKbuildRecipeToken{}
	redirect := ""
	redirectsStderr := false
	flush := func() {
		if len(command) == 7 && command[0].value == "${OBJCOPY}" &&
			recipe.Environment["OBJCOPY"] == "objcopy" && slices.Contains(recipe.AuxiliaryTools, "objcopy") &&
			actionPlanArgumentPair(recipe.Arguments, "-tool", "objcopy=${tool:objcopy}") &&
			command[1].value == "-j" && command[2].value != "" &&
			command[3].value == "-O" && command[4].value != "" &&
			!command[5].shellExpansion && !command[5].pathnameExpansion &&
			!command[6].shellExpansion && !command[6].pathnameExpansion {
			infile, validInfile := compactKbuildCommandPath(command[5].value)
			outfile, validOutfile := compactKbuildCommandPath(command[6].value)
			if validInfile && infile != "" && validOutfile &&
				!compactKbuildRecipeImmutableSourceOutput(command[6].value) && outfile == output {
				write = true
			}
		}
		command = command[:0]
	}
	for _, token := range tokens {
		if token.operator {
			switch token.value {
			case ">", ">>":
				redirect = token.value
				redirectsStderr = len(command) != 0 &&
					compactKbuildDecimalWord(command[len(command)-1].value) &&
					command[len(command)-1].value != "1" && command[len(command)-1].end == token.start
			case "|", ";", "&&", "||", "&":
				flush()
				redirect = ""
			default:
				redirect = ""
			}
			continue
		}
		if redirect != "" {
			if redirect == ">" && len(command) != 0 && !redirectsStderr &&
				!token.shellExpansion && !token.pathnameExpansion {
				if destination, valid := compactKbuildCommandPath(token.value); valid &&
					!compactKbuildRecipeImmutableSourceOutput(token.value) && destination == output {
					write = true
				}
			}
			redirect = ""
			continue
		}
		command = append(command, token)
	}
	flush()
	return write, nil
}

// actionPlanNodeExecutesImmutableSourceScript distinguishes source programs
// from generic compound shell actions, which also use scriptrun. A linear
// source-script action carries its immutable script directly. An atomic source
// command carries an executable input whose producer is the standard private
// executable copy of an immutable source program.
func actionPlanNodeExecutesImmutableSourceScript(
	plan *ActionPlan,
	indexByID map[string]int,
	node ActionPlanNode,
	recipe ActionRecipe,
) bool {
	return actionPlanNodeExecutesImmutableSourceScriptPath(plan, indexByID, node, recipe, "", "")
}

func actionPlanNodeExecutesImmutableSourceScriptPath(
	plan *ActionPlan,
	indexByID map[string]int,
	node ActionPlanNode,
	recipe ActionRecipe,
	wantNamespace, wantPath string,
) bool {
	if plan == nil || recipe.Tool != compactKbuildScriptRunnerRole || recipe.WorkingDirectory == "" {
		return false
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return false
	}
	matches := func(source ActionPlanSource, exists bool) bool {
		if !exists || source.ID == "" || validatePlanName("source namespace", source.Namespace) != nil ||
			validatePlanRelativePath("source", source.Path) != nil {
			return false
		}
		return (wantNamespace == "" || source.Namespace == wantNamespace) &&
			(wantPath == "" || source.Path == wantPath)
	}
	sourceBindings := make(map[string]bool, len(recipe.Sources))
	for _, binding := range recipe.Sources {
		sourceBindings[binding] = true
	}
	for ordinal, source := range node.Sources {
		candidate, exists := plan.sourcesByID[source.SourceID]
		if source.Role == "script" && source.SourceID != "" &&
			sourceBindings[source.Role+":"+planOrdinal(ordinal)] &&
			matches(candidate, exists) {
			return true
		}
	}
	executableBindings := make(map[string]bool, len(recipe.ExecutableInputs))
	for _, binding := range recipe.ExecutableInputs {
		executableBindings[binding] = true
	}
	for ordinal, input := range node.Inputs {
		if !executableBindings[input.Role+":"+planOrdinal(ordinal)] {
			continue
		}
		source, exists := actionPlanInputImmutableSourceExecutable(plan, indexByID, input)
		if matches(source, exists) {
			return true
		}
	}
	// The selected Kbuild command can invoke an immutable script through its
	// configured shell and then perform another source-selected command. Such
	// an atomic compound has a source prerequisite rather than a dedicated
	// script/executable input. Prove both the exact source edge and an actual
	// shell command which executes that path; a passive prerequisite alone is
	// not evidence that its script writes an output.
	return actionPlanCompoundShellExecutesSourceScript(
		plan, node, recipe, wantNamespace, wantPath,
	)
}

func actionPlanCompoundShellExecutesSourceScript(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	wantNamespace, wantPath string,
) bool {
	if wantNamespace == "" || wantPath == "" ||
		!slices.Contains(node.Trees, wantNamespace) ||
		!slices.Contains(recipe.Trees, wantNamespace) ||
		!slices.Contains(node.AuxiliaryTools, compactKbuildScriptRuntimeRole) ||
		!slices.Contains(recipe.AuxiliaryTools, compactKbuildScriptRuntimeRole) {
		return false
	}
	if !actionPlanArgumentPair(recipe.Arguments, "-interpreter", "${tool:"+compactKbuildScriptRuntimeRole+"}") ||
		!actionPlanArgumentPair(recipe.Arguments, "-interpreter_arg", "sh") ||
		!actionPlanArgumentPair(recipe.Arguments, "-multicall", "${tool:"+compactKbuildScriptRuntimeRole+"}") ||
		!actionPlanArgumentPair(recipe.Arguments, "-tool", compactKbuildScriptRuntimeRole+"=${tool:"+compactKbuildScriptRuntimeRole+"}") {
		return false
	}
	encoded := -1
	for index := 0; index+1 < len(recipe.Arguments); index++ {
		if recipe.Arguments[index] == "-script_content_base64" {
			if encoded >= 0 {
				return false
			}
			encoded = index + 1
		}
	}
	if encoded < 0 {
		return false
	}
	for _, transform := range recipe.ArgumentTransforms {
		if transform.Index == encoded {
			// A deferred text template has no concrete script command yet.
			return false
		}
	}
	content, err := base64.StdEncoding.DecodeString(recipe.Arguments[encoded])
	if err != nil {
		return false
	}
	commands, err := compactKbuildCompoundProgramCommands(string(content))
	if err != nil {
		return false
	}
	boundPaths := map[string]bool{}
	for ordinal, edge := range node.Sources {
		if edge.SourceID == "" || edge.Role == "script" {
			continue
		}
		binding := edge.Role + ":" + planOrdinal(ordinal)
		if !slices.Contains(recipe.Sources, binding) ||
			recipe.WorkingInputs["source:"+binding] != wantPath {
			continue
		}
		source, exists := plan.sourcesByID[edge.SourceID]
		if exists && source.Namespace == wantNamespace && source.Path == wantPath {
			boundPaths["${tree:"+wantNamespace+"}/"+wantPath] = true
			boundPaths[wantPath] = true // generic shell stages the declared file below its private root
		}
	}
	for _, command := range commands {
		if command.program != "sh" || len(command.arguments) == 0 ||
			len(command.argumentTokens) == 0 || command.argumentTokens[0].pathnameExpansion ||
			len(command.environment) != 0 {
			continue
		}
		if boundPaths[command.arguments[0]] {
			return true
		}
	}
	return false
}

func actionPlanArgumentPair(arguments []string, flag, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == value {
			return true
		}
	}
	return false
}

// actionPlanInputImmutableSourceExecutable recognizes only the exact private
// source-program projection emitted by materializeSourceExecutable. A source
// edge is provenance, not a capability by itself: the producer must copy that
// one immutable path with the canonical actionfile recipe and expose its sole
// canonical output to the executable input binding.
func actionPlanInputImmutableSourceExecutable(
	plan *ActionPlan,
	indexByID map[string]int,
	input ActionPlanNodeEdge,
) (ActionPlanSource, bool) {
	if plan == nil || input.Slot != 0 {
		return ActionPlanSource{}, false
	}
	producerIndex, exists := indexByID[input.ProducerID]
	if !exists {
		return ActionPlanSource{}, false
	}
	producer := plan.Nodes[producerIndex]
	if (producer.Stage != "prehost" && producer.Stage != "bootstrap" && producer.Stage != "host") ||
		producer.Kind != "copy" || producer.Tool != "actionfile" || producer.Product != "sdk" ||
		len(producer.Sources) != 1 || producer.Sources[0].Role != "program" || producer.Sources[0].SourceID == "" ||
		len(producer.Inputs) != 0 || len(producer.Trees) != 0 || len(producer.AuxiliaryTools) != 0 ||
		len(producer.Outputs) != 1 {
		return ActionPlanSource{}, false
	}
	output := producer.Outputs[0]
	if output.Tree != producer.Stage || output.Path == "" || output.ArtifactPath != "" ||
		output.ObservedPath != "" || output.persistent {
		return ActionPlanSource{}, false
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return ActionPlanSource{}, false
	}
	source, exists := plan.sourcesByID[producer.Sources[0].SourceID]
	if !exists || source.Path != output.Path || validatePlanName("source namespace", source.Namespace) != nil ||
		validatePlanRelativePath("source", source.Path) != nil {
		return ActionPlanSource{}, false
	}
	producerRecipe, exists := plan.Recipes[producer.Recipe]
	if !exists || !isImmutableSourceExecutableProjectionRecipe(producerRecipe) {
		return ActionPlanSource{}, false
	}
	recipeID, err := producerRecipe.ID()
	if err != nil || recipeID != producer.Recipe {
		return ActionPlanSource{}, false
	}
	return source, true
}

// nativeProducerFrontier removes producers which are transitively upstream of
// another candidate. Rule lowering deliberately gives an action every exact
// native prerequisite it can observe, so a terminal can have direct edges to
// both a source script and a tool which that script itself consumes. Those
// nodes are not competing nearest owners: the downstream script is the unique
// nearest node in the source-derived DAG. Independent producers remain in the
// frontier and therefore retain the caller's ambiguity check.
//
// Walk the union of all upstream closures once. This remains linear when a
// Kbuild product (notably modules.order) has one selected aggregate per source
// directory.
func nativeProducerFrontier(
	nodes []ActionPlanNode,
	indexByID map[string]int,
	producers []string,
) ([]string, error) {
	unique := []string{}
	seen := map[string]bool{}
	for _, producer := range producers {
		if producer == "" || seen[producer] {
			continue
		}
		if _, ok := indexByID[producer]; !ok {
			return nil, fmt.Errorf("terminal dependency %q is absent from the plan", producer)
		}
		seen[producer] = true
		unique = append(unique, producer)
	}
	candidates := make(map[string]bool, len(unique))
	pending := []string{}
	for _, producer := range unique {
		candidates[producer] = true
		pending = append(pending, terminalNativeInputProducers(nodes[indexByID[producer]])...)
	}
	upstream := map[string]bool{}
	visited := map[string]bool{}
	for len(pending) != 0 {
		last := len(pending) - 1
		current := pending[last]
		pending = pending[:last]
		if visited[current] {
			continue
		}
		visited[current] = true
		index, ok := indexByID[current]
		if !ok {
			return nil, fmt.Errorf("terminal dependency %q is absent from the plan", current)
		}
		if candidates[current] {
			upstream[current] = true
		}
		pending = append(pending, terminalNativeInputProducers(nodes[index])...)
	}
	frontier := make([]string, 0, len(unique))
	for _, producer := range unique {
		if !upstream[producer] {
			frontier = append(frontier, producer)
		}
	}
	return frontier, nil
}

// terminalNativeInputProducers follows only the producer edges selected as
// data or command-sequence inputs by the evaluated Make rule. Source scripts
// also receive a materialized transitive working closure, and Make may declare
// order-only prerequisites. Those edges constrain execution but are not
// producer lineage; traversing them makes unrelated source-owned scripts appear
// to own a side output merely because they run first or share the writable tree.
func terminalNativeInputProducers(node ActionPlanNode) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, input := range node.Inputs {
		if input.Role == compactKbuildWorkingClosureInputRole || input.Role == "order-only" ||
			input.ProducerID == "" || seen[input.ProducerID] {
			continue
		}
		seen[input.ProducerID] = true
		result = append(result, input.ProducerID)
	}
	return result
}

func terminalImageEntryTarget(config CompactConfig) (string, error) {
	if config.imageTarget == "" {
		return "", fmt.Errorf("evaluated root Make graph has no KBUILD_IMAGE target")
	}
	return config.imageTarget, nil
}

// selectedKbuildPlanOutput joins the exact selection owner to its native plan
// output. This prevents the stable facade from guessing a profile name or
// silently accepting an artifact built by a different Make invocation.
func selectedKbuildPlanOutput(
	plan *ActionPlan,
	config CompactConfig,
	selections *compactKbuildSelectionGraph,
	target string,
) (string, int, string, error) {
	if selections == nil {
		return "", 0, "", fmt.Errorf("selected Kbuild graph is nil")
	}
	owner, ok := selections.owner(target)
	if !ok {
		return "", 0, "", fmt.Errorf("Kbuild target %q has no selected owner", target)
	}
	selection, ok := selections.selection(owner)
	if !ok {
		return "", 0, "", fmt.Errorf("Kbuild target %q owner %s has no selection", target, compactKbuildSelectionKeyString(owner))
	}
	context := compactKbuildSelectionPlanContext(config, selection)
	if selection.Stage == "bootstrap" {
		var err error
		context, err = compactKbuildSelectionCanonicalPlanContext(config, selection)
		if err != nil {
			return "", 0, "", fmt.Errorf("resolve canonical bootstrap output for %s: %w", compactKbuildSelectionKeyString(owner), err)
		}
	}
	producer, slot, ok := planProducerByOutput(plan, context.OutputTree, owner.target)
	if !ok {
		return "", 0, "", fmt.Errorf(
			"selected owner %s has no native output %s/%s",
			compactKbuildSelectionKeyString(owner), context.OutputTree, owner.target,
		)
	}
	return producer, slot, context.OutputTree, nil
}

func appendActionPlanProjection(
	plan *ActionPlan,
	producer string,
	slot int,
	product, outputTree, outputPath string,
) (string, error) {
	if existing, existingSlot, ok := planProducerByOutput(plan, outputTree, outputPath); ok {
		if existing == producer && existingSlot == slot {
			return existing, nil
		}
		return "", fmt.Errorf("projection output %s/%s already has producer %s slot %d", outputTree, outputPath, existing, existingSlot)
	}
	node := ActionPlanNode{
		Stage: "target", Kind: "copy", Tool: "actionfile", Product: product,
		Inputs:  []ActionPlanNodeEdge{{Role: "artifact", ProducerID: producer, Slot: slot}},
		Outputs: []ActionPlanOutput{{Tree: outputTree, Path: outputPath}},
	}
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{
			"-input", "${input:artifact:00000000}", "-out", "${output:00000000}",
		},
		Inputs: []string{"artifact:00000000"}, Outputs: []string{"00000000"},
	}
	return appendActionPlanNode(plan, node, recipe)
}

func appendActionPlanProduct(plan *ActionPlan, product ActionPlanProduct) error {
	for _, existing := range plan.Products {
		if existing.Name != product.Name {
			continue
		}
		if existing == product {
			return nil
		}
		return fmt.Errorf(
			"product %q already refers to %s/%s, cannot project %s/%s",
			product.Name, existing.Tree, existing.Path, product.Tree, product.Path,
		)
	}
	plan.Products = append(plan.Products, product)
	return nil
}
