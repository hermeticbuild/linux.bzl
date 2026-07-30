package kconfig

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func (metadata *CompactMetadataV7) validate() error {
	if metadata == nil {
		return fmt.Errorf("compact-v7 metadata is nil")
	}
	if metadata.Protocol != CompactMetadataProtocolV7 {
		return fmt.Errorf(
			"compact-v7 protocol = %q, want %q",
			metadata.Protocol,
			CompactMetadataProtocolV7,
		)
	}
	if strings.TrimSpace(metadata.ToolchainProfile) == "" {
		return fmt.Errorf("compact-v7 metadata has an empty toolchain profile")
	}
	if strings.TrimSpace(metadata.CompileEnvironmentABI) == "" {
		return fmt.Errorf("compact-v7 metadata has an empty compile environment ABI")
	}

	payloads, err := metadata.validateCompactV7Payloads()
	if err != nil {
		return err
	}
	sourceSets, expandedSourceSets, err := metadata.validateCompactV7Sources()
	if err != nil {
		return err
	}
	families, err := metadata.validateCompactV7Families(payloads, expandedSourceSets)
	if err != nil {
		return err
	}
	environments, err := metadata.validateCompactV7Environments(payloads, families)
	if err != nil {
		return err
	}
	programs, err := metadata.validateCompactV7Programs()
	if err != nil {
		return err
	}
	reachability, err := metadata.validateCompactV7Reachability()
	if err != nil {
		return err
	}
	recipes, err := metadata.validateCompactV7Recipes(programs)
	if err != nil {
		return err
	}
	objects, err := metadata.validateCompactV7Objects(
		environments,
		recipes,
		reachability,
		expandedSourceSets,
	)
	if err != nil {
		return err
	}
	if err := metadata.validateCompactV7Configs(
		payloads,
		objects,
		reachability,
		expandedSourceSets,
	); err != nil {
		return err
	}
	if err := metadata.validateCompactV7RecipeGroups(objects, recipes, reachability); err != nil {
		return err
	}
	if err := metadata.validateCompactV7References(
		payloads,
		environments,
		families,
		sourceSets,
		programs,
		reachability,
		recipes,
		objects,
	); err != nil {
		return err
	}
	return nil
}

func (metadata *CompactMetadataV7) validateCompactV7Payloads() (
	map[string]CompactConfigPayload,
	error,
) {
	payloads := make(map[string]CompactConfigPayload, len(metadata.ConfigPayloads))
	previous := ""
	for _, payload := range metadata.ConfigPayloads {
		if previous != "" && previous >= payload.ID {
			return nil, fmt.Errorf(
				"compact-v7 config payloads are duplicate or not canonically ordered at %s",
				payload.ID,
			)
		}
		previous = payload.ID
		if err := compactV7CheckFullID("config payload", payload.ID); err != nil {
			return nil, err
		}
		if payload.fragment != nil &&
			payload.Content != canonicalConfigContent(payload.fragment) {
			return nil, fmt.Errorf(
				"compact-v7 config payload %s content does not match its fragment",
				payload.ID,
			)
		}
		expected := compactContentID(compactConfigPayloadDomain, payload.Content)
		if payload.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 config payload %s canonical content hashes to %s",
				payload.ID,
				expected,
			)
		}
		payloads[payload.ID] = payload
	}
	return payloads, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Sources() (
	map[string]CompactSourceSet,
	map[string]map[int]bool,
	error,
) {
	for i, input := range metadata.SourceFiles {
		if err := validateCompactSourceInput(
			input,
			fmt.Sprintf("compact-v7 source file %d", i+1),
		); err != nil {
			return nil, nil, err
		}
		if i != 0 && metadata.SourceFiles[i-1].Path >= input.Path {
			return nil, nil, fmt.Errorf(
				"compact-v7 source files are duplicate or not canonically ordered at %q",
				input.Path,
			)
		}
	}

	sourceSets := make(map[string]CompactSourceSet, len(metadata.SourceSets))
	previous := ""
	for _, sourceSet := range metadata.SourceSets {
		if previous != "" && previous >= sourceSet.ID {
			return nil, nil, fmt.Errorf(
				"compact-v7 source sets are duplicate or not canonically ordered at %s",
				sourceSet.ID,
			)
		}
		previous = sourceSet.ID
		if err := compactV7CheckFullID("source set", sourceSet.ID); err != nil {
			return nil, nil, err
		}
		if len(sourceSet.Files) == 0 && len(sourceSet.Children) == 0 {
			return nil, nil, fmt.Errorf("compact-v7 source set %s is empty", sourceSet.ID)
		}
		for i, index := range sourceSet.Files {
			if index <= 0 || index > len(metadata.SourceFiles) {
				return nil, nil, fmt.Errorf(
					"compact-v7 source set %s file index %d is out of range",
					sourceSet.ID,
					index,
				)
			}
			if i != 0 && sourceSet.Files[i-1] >= index {
				return nil, nil, fmt.Errorf(
					"compact-v7 source set %s has duplicate or non-canonical files",
					sourceSet.ID,
				)
			}
		}
		if !compactV7SortedUnique(sourceSet.Children) {
			return nil, nil, fmt.Errorf(
				"compact-v7 source set %s has duplicate or non-canonical children",
				sourceSet.ID,
			)
		}
		directInputs := make([]CompactSourceInput, 0, len(sourceSet.Files))
		for _, index := range sourceSet.Files {
			directInputs = append(directInputs, metadata.SourceFiles[index-1])
		}
		expected := compactV7SourceSetContentID(directInputs, sourceSet.Children)
		if sourceSet.ID != expected {
			return nil, nil, fmt.Errorf(
				"compact-v7 source set %s canonical fields hash to %s",
				sourceSet.ID,
				expected,
			)
		}
		sourceSets[sourceSet.ID] = sourceSet
	}

	expanded := map[string]map[int]bool{}
	states := map[string]uint8{}
	var expand func(string) (map[int]bool, error)
	expand = func(id string) (map[int]bool, error) {
		if files, ok := expanded[id]; ok {
			return files, nil
		}
		sourceSet, ok := sourceSets[id]
		if !ok {
			return nil, fmt.Errorf("compact-v7 references unknown source set %s", id)
		}
		if states[id] == 1 {
			return nil, fmt.Errorf("compact-v7 source set cycle at %s", id)
		}
		states[id] = 1
		files := map[int]bool{}
		for _, index := range sourceSet.Files {
			files[index] = true
		}
		for _, child := range sourceSet.Children {
			childFiles, err := expand(child)
			if err != nil {
				return nil, err
			}
			for index := range childFiles {
				if files[index] {
					return nil, fmt.Errorf(
						"compact-v7 source set %s overlaps file %d through child %s",
						id,
						index,
						child,
					)
				}
				files[index] = true
			}
		}
		states[id] = 2
		expanded[id] = files
		return files, nil
	}
	for id := range sourceSets {
		if _, err := expand(id); err != nil {
			return nil, nil, err
		}
	}

	groups := map[string]CompactActionSourceGroup{}
	previous = ""
	for _, group := range metadata.ActionSourceGroups {
		if previous != "" && previous >= group.ID {
			return nil, nil, fmt.Errorf(
				"compact-v7 action source groups are duplicate or not canonically ordered at %s",
				group.ID,
			)
		}
		previous = group.ID
		if err := compactV7CheckFullID("action source group", group.ID); err != nil {
			return nil, nil, err
		}
		files, ok := expanded[group.SourceSet]
		if !ok {
			return nil, nil, fmt.Errorf(
				"compact-v7 action source group %s references unknown source set %s",
				group.ID,
				group.SourceSet,
			)
		}
		if group.PrimarySource <= 0 || group.PrimarySource > len(metadata.SourceFiles) {
			return nil, nil, fmt.Errorf(
				"compact-v7 action source group %s primary source %d is out of range",
				group.ID,
				group.PrimarySource,
			)
		}
		if !files[group.PrimarySource] {
			return nil, nil, fmt.Errorf(
				"compact-v7 action source group %s source set omits primary source %q",
				group.ID,
				metadata.SourceFiles[group.PrimarySource-1].Path,
			)
		}
		expected := compactV7ActionSourceGroupContentID(
			group.SourceSet,
			metadata.SourceFiles[group.PrimarySource-1],
		)
		if group.ID != expected {
			return nil, nil, fmt.Errorf(
				"compact-v7 action source group %s canonical fields hash to %s",
				group.ID,
				expected,
			)
		}
		groups[group.ID] = group
	}
	_ = groups
	return sourceSets, expanded, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Families(
	payloads map[string]CompactConfigPayload,
	expandedSourceSets map[string]map[int]bool,
) (map[string]CompactGeneratedHeaderFamilyV7, error) {
	families := make(
		map[string]CompactGeneratedHeaderFamilyV7,
		len(metadata.GeneratedHeaderFamilies),
	)
	previous := ""
	for _, family := range metadata.GeneratedHeaderFamilies {
		if previous != "" && previous >= family.ID {
			return nil, fmt.Errorf(
				"compact-v7 generated header families are duplicate or not canonically ordered at %s",
				family.ID,
			)
		}
		previous = family.ID
		if err := compactV7CheckFullID("generated header family", family.ID); err != nil {
			return nil, err
		}
		if family.Name == "" || family.Srcarch == "" || len(family.Labels) == 0 {
			return nil, fmt.Errorf(
				"compact-v7 generated header family %s has incomplete metadata",
				family.ID,
			)
		}
		if !compactV7SortedUnique(family.Labels) {
			return nil, fmt.Errorf(
				"compact-v7 generated header family %s has non-canonical labels",
				family.ID,
			)
		}
		for _, label := range family.Labels {
			if strings.TrimSpace(label) == "" {
				return nil, fmt.Errorf(
					"compact-v7 generated header family %s has an empty label",
					family.ID,
				)
			}
		}
		if !compactV7SortedUnique(family.Dependencies) {
			return nil, fmt.Errorf(
				"compact-v7 generated header family %s has non-canonical dependencies",
				family.ID,
			)
		}
		if _, ok := payloads[family.ConfigPayload]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 generated header family %s references unknown config payload %s",
				family.ID,
				family.ConfigPayload,
			)
		}
		var inputs []CompactSourceInput
		if family.SourceSet != "" {
			files, ok := expandedSourceSets[family.SourceSet]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 generated header family %s references unknown source set %s",
					family.ID,
					family.SourceSet,
				)
			}
			indices := make([]int, 0, len(files))
			for index := range files {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			for _, index := range indices {
				inputs = append(inputs, metadata.SourceFiles[index-1])
			}
		}
		expected := compactGeneratedHeaderFamilyContentID(
			family.Name,
			family.ConfigPayload,
			family.Srcarch,
			family.Dependencies,
			inputs,
		)
		if family.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 generated header family %s canonical fields hash to %s",
				family.ID,
				expected,
			)
		}
		families[family.ID] = family
	}
	states := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if states[id] == 2 {
			return nil
		}
		if states[id] == 1 {
			return fmt.Errorf("compact-v7 generated header family cycle at %s", id)
		}
		family, ok := families[id]
		if !ok {
			return fmt.Errorf("compact-v7 references unknown generated header family %s", id)
		}
		states[id] = 1
		for _, dependency := range family.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[id] = 2
		return nil
	}
	for id := range families {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return families, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Environments(
	payloads map[string]CompactConfigPayload,
	families map[string]CompactGeneratedHeaderFamilyV7,
) (map[string]CompactCompileEnvironment, error) {
	environments := make(
		map[string]CompactCompileEnvironment,
		len(metadata.CompileEnvironments),
	)
	previous := ""
	for _, environment := range metadata.CompileEnvironments {
		if previous != "" && previous >= environment.ID {
			return nil, fmt.Errorf(
				"compact-v7 compile environments are duplicate or not canonically ordered at %s",
				environment.ID,
			)
		}
		previous = environment.ID
		if environment.ABI != metadata.CompileEnvironmentABI {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s uses ABI %q, want %q",
				environment.ID,
				environment.ABI,
				metadata.CompileEnvironmentABI,
			)
		}
		if _, ok := payloads[environment.ConfigPayload]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s references unknown config payload %s",
				environment.ID,
				environment.ConfigPayload,
			)
		}
		if !compactV7SortedUnique(environment.GeneratedHeaderFamilies) {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s has non-canonical generated header families",
				environment.ID,
			)
		}
		familyNames := map[string]bool{}
		for _, familyID := range environment.GeneratedHeaderFamilies {
			family, ok := families[familyID]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 compile environment %s references unknown generated header family %s",
					environment.ID,
					familyID,
				)
			}
			if familyNames[family.Name] {
				return nil, fmt.Errorf(
					"compact-v7 compile environment %s repeats generated header family %q",
					environment.ID,
					family.Name,
				)
			}
			familyNames[family.Name] = true
		}
		if familyNames[compactGeneratedHeaderFamilyAll] && len(familyNames) != 1 {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s mixes all with precise generated header families",
				environment.ID,
			)
		}
		expected := newCompactCompileEnvironment(
			environment.ABI,
			environment.ConfigPayload,
			environment.GeneratedHeaderFamilies,
		).ID
		if environment.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s canonical fields hash to %s",
				environment.ID,
				expected,
			)
		}
		environments[environment.ID] = environment
	}
	return environments, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Programs() (
	map[string]CompactKbuildFlagProgram,
	error,
) {
	terminals := map[string]CompactKbuildFlagTerminal{}
	previous := ""
	for _, terminal := range metadata.FlagTerminals {
		if previous != "" && previous >= terminal.ID {
			return nil, fmt.Errorf(
				"compact-v7 flag terminals are duplicate or not canonically ordered at %s",
				terminal.ID,
			)
		}
		previous = terminal.ID
		expected := compactV7FlagTerminalContentID(terminal.Argv)
		if terminal.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 flag terminal %s canonical argv hashes to %s",
				terminal.ID,
				expected,
			)
		}
		terminals[terminal.ID] = terminal
	}

	probes := map[string]CompactKbuildProbe{}
	previous = ""
	for _, probe := range metadata.KbuildProbes {
		if previous != "" && previous >= probe.ID {
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probes are duplicate or not canonically ordered at %s",
				probe.ID,
			)
		}
		previous = probe.ID
		switch probe.Kind {
		case "cc_option", "as_option", "ld_option":
		default:
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probe %s has unknown kind %q",
				probe.ID,
				probe.Kind,
			)
		}
		if len(probe.CandidateArgv) == 0 {
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probe %s has no candidate argv",
				probe.ID,
			)
		}
		expected := compactV7ProbeContentID(probe)
		if probe.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probe %s canonical fields hash to %s",
				probe.ID,
				expected,
			)
		}
		probes[probe.ID] = probe
	}

	nodes := map[string]CompactKbuildFlagNode{}
	previous = ""
	for _, node := range metadata.FlagNodes {
		if previous != "" && previous >= node.ID {
			return nil, fmt.Errorf(
				"compact-v7 flag nodes are duplicate or not canonically ordered at %s",
				node.ID,
			)
		}
		previous = node.ID
		expected := ""
		switch node.Kind {
		case "concat":
			if len(node.Children) < 2 ||
				node.Probe != "" || node.WhenTrue != "" || node.WhenFalse != "" {
				return nil, fmt.Errorf(
					"compact-v7 flag concat node %s is not reduced",
					node.ID,
				)
			}
			expected = compactV7FlagConcatContentID(node.Children)
		case "select":
			if len(node.Children) != 0 {
				return nil, fmt.Errorf(
					"compact-v7 flag select node %s has concat children",
					node.ID,
				)
			}
			if _, ok := probes[node.Probe]; !ok {
				return nil, fmt.Errorf(
					"compact-v7 flag node %s references unknown probe %s",
					node.ID,
					node.Probe,
				)
			}
			if node.WhenTrue == "" || node.WhenFalse == "" ||
				node.WhenTrue == node.WhenFalse {
				return nil, fmt.Errorf(
					"compact-v7 flag select node %s is not reduced",
					node.ID,
				)
			}
			expected = compactV7FlagSelectContentID(
				node.Probe,
				node.WhenTrue,
				node.WhenFalse,
			)
		default:
			return nil, fmt.Errorf(
				"compact-v7 flag node %s has unknown kind %q",
				node.ID,
				node.Kind,
			)
		}
		if node.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 flag node %s canonical fields hash to %s",
				node.ID,
				expected,
			)
		}
		nodes[node.ID] = node
	}

	programs := map[string]CompactKbuildFlagProgram{}
	previous = ""
	for _, program := range metadata.FlagPrograms {
		if previous != "" && previous >= program.ID {
			return nil, fmt.Errorf(
				"compact-v7 flag programs are duplicate or not canonically ordered at %s",
				program.ID,
			)
		}
		previous = program.ID
		canonical, err := canonicalCompactV7Effects(program.Effects)
		if err != nil || !compactV7EqualEffects(canonical, program.Effects) {
			return nil, fmt.Errorf(
				"compact-v7 flag program %s has non-canonical effects %v",
				program.ID,
				program.Effects,
			)
		}
		expected := compactV7FlagProgramContentID(program.Root, program.Effects)
		if program.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 flag program %s canonical fields hash to %s",
				program.ID,
				expected,
			)
		}
		programs[program.ID] = program
	}

	rootEffects := map[string][]string{}
	rootStates := map[string]uint8{}
	emptyTerminal := compactV7FlagTerminalContentID(nil)
	var evaluateRoot func(string) ([]string, error)
	evaluateRoot = func(root string) ([]string, error) {
		if terminal, ok := terminals[root]; ok {
			if effects, ok := rootEffects[root]; ok {
				return effects, nil
			}
			effects := classifyCompactV7FlagEffects(terminal.Argv)
			rootEffects[root] = effects
			return effects, nil
		}
		node, ok := nodes[root]
		if !ok {
			return nil, fmt.Errorf("compact-v7 references unknown flag root %s", root)
		}
		if effects, ok := rootEffects[root]; ok {
			return effects, nil
		}
		if rootStates[root] == 1 {
			return nil, fmt.Errorf("compact-v7 flag node cycle at %s", root)
		}
		rootStates[root] = 1
		var effectLists [][]string
		switch node.Kind {
		case "concat":
			for _, child := range node.Children {
				if child == emptyTerminal {
					return nil, fmt.Errorf(
						"compact-v7 flag concat node %s contains an empty child",
						node.ID,
					)
				}
				if childNode, ok := nodes[child]; ok && childNode.Kind == "concat" {
					return nil, fmt.Errorf(
						"compact-v7 flag concat node %s contains concat child %s",
						node.ID,
						child,
					)
				}
				childEffects, err := evaluateRoot(child)
				if err != nil {
					return nil, err
				}
				effectLists = append(effectLists, childEffects)
			}
		case "select":
			whenTrue, err := evaluateRoot(node.WhenTrue)
			if err != nil {
				return nil, err
			}
			whenFalse, err := evaluateRoot(node.WhenFalse)
			if err != nil {
				return nil, err
			}
			effectLists = append(effectLists, whenTrue, whenFalse)
		}
		effects, err := canonicalCompactV7Effects(effectLists...)
		if err != nil {
			return nil, err
		}
		rootStates[root] = 2
		rootEffects[root] = effects
		return effects, nil
	}
	for _, program := range metadata.FlagPrograms {
		effects, err := evaluateRoot(program.Root)
		if err != nil {
			return nil, fmt.Errorf("compact-v7 flag program %s: %w", program.ID, err)
		}
		if !reflect.DeepEqual(program.Effects, effects) {
			return nil, fmt.Errorf(
				"compact-v7 flag program %s effects = %v, want %v",
				program.ID,
				program.Effects,
				effects,
			)
		}
	}
	for _, probe := range metadata.KbuildProbes {
		if probe.ContextProgram == "" {
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probe %s has no context program",
				probe.ID,
			)
		}
		if _, ok := programs[probe.ContextProgram]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 Kbuild probe %s references unknown context program %s",
				probe.ID,
				probe.ContextProgram,
			)
		}
	}
	return programs, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Reachability() (
	map[string]CompactReachabilitySignature,
	error,
) {
	signatures := make(
		map[string]CompactReachabilitySignature,
		len(metadata.ReachabilitySignatures),
	)
	previous := ""
	for _, signature := range metadata.ReachabilitySignatures {
		if previous != "" && previous >= signature.ID {
			return nil, fmt.Errorf(
				"compact-v7 reachability signatures are duplicate or not canonically ordered at %s",
				signature.ID,
			)
		}
		previous = signature.ID
		if len(signature.Configs) == 0 || !compactV7SortedUnique(signature.Configs) {
			return nil, fmt.Errorf(
				"compact-v7 reachability signature %s has empty or non-canonical configs",
				signature.ID,
			)
		}
		expected := compactV7ReachabilityContentID(signature.Configs)
		if signature.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 reachability signature %s canonical fields hash to %s",
				signature.ID,
				expected,
			)
		}
		signatures[signature.ID] = signature
	}
	return signatures, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Recipes(
	programs map[string]CompactKbuildFlagProgram,
) (map[string]CompactActionRecipe, error) {
	recipes := make(map[string]CompactActionRecipe, len(metadata.ActionRecipes))
	previous := ""
	for _, recipe := range metadata.ActionRecipes {
		if previous != "" && previous >= recipe.ID {
			return nil, fmt.Errorf(
				"compact-v7 action recipes are duplicate or not canonically ordered at %s",
				recipe.ID,
			)
		}
		previous = recipe.ID
		switch recipe.Kind {
		case "compile":
			if recipe.Language != "c" && recipe.Language != "asm" {
				return nil, fmt.Errorf(
					"compact-v7 compile recipe %s has language %q",
					recipe.ID,
					recipe.Language,
				)
			}
		case "arm64_nvhe":
		case "composite":
			if recipe.Language != "" {
				return nil, fmt.Errorf(
					"compact-v7 composite recipe %s has compile-only metadata",
					recipe.ID,
				)
			}
		default:
			return nil, fmt.Errorf(
				"compact-v7 action recipe %s has unknown kind %q",
				recipe.ID,
				recipe.Kind,
			)
		}
		if recipe.Mode != "y" && recipe.Mode != "m" {
			return nil, fmt.Errorf(
				"compact-v7 action recipe %s has invalid mode %q",
				recipe.ID,
				recipe.Mode,
			)
		}
		if _, ok := programs[recipe.FlagProgram]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 action recipe %s references unknown flag program %s",
				recipe.ID,
				recipe.FlagProgram,
			)
		}
		if _, ok := programs[recipe.RemoveFlagProgram]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 action recipe %s references unknown remove-flag program %s",
				recipe.ID,
				recipe.RemoveFlagProgram,
			)
		}
		expected := compactV7ActionRecipeContentID(
			recipe,
			metadata.ToolchainProfile,
			metadata.CompileEnvironmentABI,
		)
		if recipe.ID != expected {
			return nil, fmt.Errorf(
				"compact-v7 action recipe %s canonical fields hash to %s",
				recipe.ID,
				expected,
			)
		}
		recipes[recipe.ID] = recipe
	}
	return recipes, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Objects(
	environments map[string]CompactCompileEnvironment,
	recipes map[string]CompactActionRecipe,
	reachability map[string]CompactReachabilitySignature,
	expandedSourceSets map[string]map[int]bool,
) (map[string]CompactObjectVariantV7, error) {
	actionGroups := make(map[string]CompactActionSourceGroup, len(metadata.ActionSourceGroups))
	for _, group := range metadata.ActionSourceGroups {
		actionGroups[group.ID] = group
	}
	objects := make(map[string]CompactObjectVariantV7, len(metadata.ObjectVariants))
	contentIDs := map[string]string{}
	previous := ""
	for _, object := range metadata.ObjectVariants {
		if previous != "" && previous >= object.Target {
			return nil, fmt.Errorf(
				"compact-v7 objects are duplicate or not canonically ordered at %q",
				object.Target,
			)
		}
		previous = object.Target
		if err := compactV7CheckFullID("object", object.ContentID); err != nil {
			return nil, err
		}
		if existing, ok := contentIDs[object.ContentID]; ok {
			return nil, fmt.Errorf(
				"compact-v7 objects %q and %q duplicate content ID %s",
				existing,
				object.Target,
				object.ContentID,
			)
		}
		contentIDs[object.ContentID] = object.Target
		recipe, ok := recipes[object.Recipe]
		if !ok {
			return nil, fmt.Errorf(
				"compact-v7 object %q references unknown recipe %s",
				object.Target,
				object.Recipe,
			)
		}
		if _, ok := reachability[object.Reachability]; !ok {
			return nil, fmt.Errorf(
				"compact-v7 object %q references unknown reachability %s",
				object.Target,
				object.Reachability,
			)
		}
		switch recipe.Kind {
		case "compile":
			if object.CompileEnvironment == "" || object.ActionSourceGroup == "" || len(object.Members) != 0 {
				return nil, fmt.Errorf(
					"compact-v7 compile object %q has invalid environment/source/member metadata",
					object.Target,
				)
			}
		case "arm64_nvhe":
			if object.CompileEnvironment == "" || object.ActionSourceGroup == "" || len(object.Members) == 0 {
				return nil, fmt.Errorf(
					"compact-v7 arm64 nVHE object %q has invalid environment/source/member metadata",
					object.Target,
				)
			}
		case "composite":
			if object.CompileEnvironment != "" || object.ActionSourceGroup != "" || len(object.Members) == 0 {
				return nil, fmt.Errorf(
					"compact-v7 composite object %q has compile-only metadata",
					object.Target,
				)
			}
		}
		if object.CompileEnvironment != "" {
			if _, ok := environments[object.CompileEnvironment]; !ok {
				return nil, fmt.Errorf(
					"compact-v7 object %q references unknown compile environment %s",
					object.Target,
					object.CompileEnvironment,
				)
			}
		}
		if object.ActionSourceGroup != "" {
			group, ok := actionGroups[object.ActionSourceGroup]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 object %q references unknown action source group %s",
					object.Target,
					object.ActionSourceGroup,
				)
			}
			if _, ok := expandedSourceSets[group.SourceSet]; !ok {
				return nil, fmt.Errorf(
					"compact-v7 object %q action source group references unknown source set",
					object.Target,
				)
			}
		}
		if !compactV7SortedUnique(object.Deps) {
			return nil, fmt.Errorf(
				"compact-v7 object %q has duplicate or non-canonical dependencies",
				object.Target,
			)
		}
		seenMembers := map[string]bool{}
		for _, member := range object.Members {
			if seenMembers[member] {
				return nil, fmt.Errorf(
					"compact-v7 object %q repeats member %q",
					object.Target,
					member,
				)
			}
			seenMembers[member] = true
		}
		objects[object.Target] = object
	}
	for _, object := range metadata.ObjectVariants {
		depContentIDs := make([]string, 0, len(object.Deps))
		for _, target := range object.Deps {
			dependency, ok := objects[target]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 object %q references unknown dependency %q",
					object.Target,
					target,
				)
			}
			depContentIDs = append(depContentIDs, dependency.ContentID)
		}
		sort.Strings(depContentIDs)
		memberContentIDs := make([]string, 0, len(object.Members))
		for _, target := range object.Members {
			member, ok := objects[target]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 object %q references unknown member %q",
					object.Target,
					target,
				)
			}
			memberContentIDs = append(memberContentIDs, member.ContentID)
		}
		expected := compactV7ObjectContentID(
			object.Object,
			object.Recipe,
			object.CompileEnvironment,
			object.ActionSourceGroup,
			depContentIDs,
			memberContentIDs,
			metadata.CompileEnvironmentABI,
		)
		if object.ContentID != expected {
			return nil, fmt.Errorf(
				"compact-v7 object %q canonical fields hash to %s, got %s",
				object.Target,
				expected,
				object.ContentID,
			)
		}
		if !strings.HasSuffix(object.Target, "__"+compactShortID(object.ContentID)) {
			return nil, fmt.Errorf(
				"compact-v7 object %q does not end in short content ID %q",
				object.Target,
				compactShortID(object.ContentID),
			)
		}
	}
	return objects, nil
}

func (metadata *CompactMetadataV7) validateCompactV7Configs(
	payloads map[string]CompactConfigPayload,
	objects map[string]CompactObjectVariantV7,
	reachability map[string]CompactReachabilitySignature,
	expandedSourceSets map[string]map[int]bool,
) error {
	configNames := map[string]bool{}
	previous := ""
	expectedReachability := map[string]map[string]bool{}
	for _, config := range metadata.Configs {
		if config.Name == "" || (previous != "" && previous >= config.Name) {
			return fmt.Errorf(
				"compact-v7 configs are empty, duplicate, or not canonically ordered at %q",
				config.Name,
			)
		}
		previous = config.Name
		configNames[config.Name] = true
		if _, ok := payloads[config.ConfigPayload]; !ok {
			return fmt.Errorf(
				"compact-v7 config %q references unknown config payload %s",
				config.Name,
				config.ConfigPayload,
			)
		}
		supportFiles, ok := expandedSourceSets[config.SupportSourceSet]
		if !ok {
			return fmt.Errorf(
				"compact-v7 config %q references unknown support source set %s",
				config.Name,
				config.SupportSourceSet,
			)
		}
		hasLinkerScript := false
		for index := range supportFiles {
			path := metadata.SourceFiles[index-1].Path
			if strings.HasPrefix(path, "arch/") &&
				strings.HasSuffix(path, "/kernel/vmlinux.lds.S") {
				hasLinkerScript = true
				break
			}
		}
		if !hasLinkerScript {
			return fmt.Errorf(
				"compact-v7 config %q support source set %s omits arch linker script",
				config.Name,
				config.SupportSourceSet,
			)
		}
		seen := map[string]bool{}
		var visit func(string) error
		visit = func(target string) error {
			if seen[target] {
				return nil
			}
			object, ok := objects[target]
			if !ok {
				return fmt.Errorf(
					"compact-v7 config %q references unknown object target %q",
					config.Name,
					target,
				)
			}
			seen[target] = true
			if expectedReachability[target] == nil {
				expectedReachability[target] = map[string]bool{}
			}
			expectedReachability[target][config.Name] = true
			for _, dependency := range object.Deps {
				if err := visit(dependency); err != nil {
					return err
				}
			}
			for _, member := range object.Members {
				if err := visit(member); err != nil {
					return err
				}
			}
			return nil
		}
		for _, target := range config.ObjectTargets {
			if err := visit(target); err != nil {
				return err
			}
		}
		for _, target := range config.ModuleObjectTargets {
			if err := visit(target); err != nil {
				return err
			}
		}
	}
	for _, signature := range metadata.ReachabilitySignatures {
		for _, config := range signature.Configs {
			if !configNames[config] {
				return fmt.Errorf(
					"compact-v7 reachability %s references unknown config %q",
					signature.ID,
					config,
				)
			}
		}
	}
	for target, object := range objects {
		names := sortedCompactIDs(expectedReachability[target])
		if len(names) == 0 {
			return fmt.Errorf("compact-v7 object %q is unreachable", target)
		}
		signature := reachability[object.Reachability]
		if !reflect.DeepEqual(names, signature.Configs) {
			return fmt.Errorf(
				"compact-v7 object %q reachability = %v, want %v",
				target,
				signature.Configs,
				names,
			)
		}
	}
	return nil
}

func (metadata *CompactMetadataV7) validateCompactV7RecipeGroups(
	objects map[string]CompactObjectVariantV7,
	recipes map[string]CompactActionRecipe,
	reachability map[string]CompactReachabilitySignature,
) error {
	groups := map[string]CompactActionRecipeGroup{}
	partitions := map[string]string{}
	objectGroups := map[string]string{}
	previous := ""
	for _, group := range metadata.ActionRecipeGroups {
		if previous != "" && previous >= group.ID {
			return fmt.Errorf(
				"compact-v7 action recipe groups are duplicate or not canonically ordered at %s",
				group.ID,
			)
		}
		previous = group.ID
		if _, ok := recipes[group.Recipe]; !ok {
			return fmt.Errorf(
				"compact-v7 action recipe group %s references unknown recipe %s",
				group.ID,
				group.Recipe,
			)
		}
		if _, ok := reachability[group.Reachability]; !ok {
			return fmt.Errorf(
				"compact-v7 action recipe group %s references unknown reachability %s",
				group.ID,
				group.Reachability,
			)
		}
		partition := group.Recipe + "\x00" + group.Reachability
		if existing := partitions[partition]; existing != "" {
			return fmt.Errorf(
				"compact-v7 action recipe groups %s and %s duplicate recipe/reachability partition",
				existing,
				group.ID,
			)
		}
		partitions[partition] = group.ID
		if len(group.Objects) == 0 || !compactV7SortedUnique(group.Objects) {
			return fmt.Errorf(
				"compact-v7 action recipe group %s has empty or non-canonical objects",
				group.ID,
			)
		}
		contentIDs := make([]string, 0, len(group.Objects))
		for _, target := range group.Objects {
			object, ok := objects[target]
			if !ok {
				return fmt.Errorf(
					"compact-v7 action recipe group %s references unknown object %q",
					group.ID,
					target,
				)
			}
			if object.Recipe != group.Recipe ||
				object.Reachability != group.Reachability {
				return fmt.Errorf(
					"compact-v7 action recipe group %s does not match object %q",
					group.ID,
					target,
				)
			}
			if existing := objectGroups[target]; existing != "" {
				return fmt.Errorf(
					"compact-v7 object %q occurs in recipe groups %s and %s",
					target,
					existing,
					group.ID,
				)
			}
			objectGroups[target] = group.ID
			contentIDs = append(contentIDs, object.ContentID)
		}
		expected := compactV7ActionRecipeGroupContentID(
			group.Recipe,
			group.Reachability,
			contentIDs,
		)
		if group.ID != expected {
			return fmt.Errorf(
				"compact-v7 action recipe group %s canonical fields hash to %s",
				group.ID,
				expected,
			)
		}
		groups[group.ID] = group
	}
	for target, object := range objects {
		if object.RecipeGroup == "" || objectGroups[target] != object.RecipeGroup {
			return fmt.Errorf(
				"compact-v7 object %q recipe group = %q, want %q",
				target,
				object.RecipeGroup,
				objectGroups[target],
			)
		}
		if _, ok := groups[object.RecipeGroup]; !ok {
			return fmt.Errorf(
				"compact-v7 object %q references unknown recipe group %s",
				target,
				object.RecipeGroup,
			)
		}
	}
	return nil
}

func (metadata *CompactMetadataV7) validateCompactV7References(
	payloads map[string]CompactConfigPayload,
	environments map[string]CompactCompileEnvironment,
	families map[string]CompactGeneratedHeaderFamilyV7,
	sourceSets map[string]CompactSourceSet,
	programs map[string]CompactKbuildFlagProgram,
	reachability map[string]CompactReachabilitySignature,
	recipes map[string]CompactActionRecipe,
	objects map[string]CompactObjectVariantV7,
) error {
	usedPayloads := map[string]bool{}
	usedEnvironments := map[string]bool{}
	usedFamilies := map[string]bool{}
	usedSourceSets := map[string]bool{}
	usedActionGroups := map[string]bool{}
	usedPrograms := map[string]bool{}
	usedRecipes := map[string]bool{}
	usedReachability := map[string]bool{}
	usedRecipeGroups := map[string]bool{}
	for _, config := range metadata.Configs {
		usedPayloads[config.ConfigPayload] = true
		usedSourceSets[config.SupportSourceSet] = true
	}
	var useFamily func(string)
	useFamily = func(id string) {
		if usedFamilies[id] {
			return
		}
		usedFamilies[id] = true
		family := families[id]
		usedPayloads[family.ConfigPayload] = true
		if family.SourceSet != "" {
			usedSourceSets[family.SourceSet] = true
		}
		for _, dependency := range family.Dependencies {
			useFamily(dependency)
		}
	}
	for id := range families {
		useFamily(id)
	}
	for _, object := range objects {
		usedRecipes[object.Recipe] = true
		usedReachability[object.Reachability] = true
		usedRecipeGroups[object.RecipeGroup] = true
		if object.CompileEnvironment != "" {
			usedEnvironments[object.CompileEnvironment] = true
		}
		if object.ActionSourceGroup != "" {
			usedActionGroups[object.ActionSourceGroup] = true
		}
	}
	for id := range usedRecipes {
		recipe := recipes[id]
		usedPrograms[recipe.FlagProgram] = true
		usedPrograms[recipe.RemoveFlagProgram] = true
	}
	for id := range usedEnvironments {
		environment := environments[id]
		usedPayloads[environment.ConfigPayload] = true
		for _, family := range environment.GeneratedHeaderFamilies {
			useFamily(family)
		}
	}
	actionGroups := map[string]CompactActionSourceGroup{}
	for _, group := range metadata.ActionSourceGroups {
		actionGroups[group.ID] = group
	}
	for id := range usedActionGroups {
		usedSourceSets[actionGroups[id].SourceSet] = true
	}
	visitedSourceSets := map[string]bool{}
	var useSourceSet func(string)
	useSourceSet = func(id string) {
		if id == "" || visitedSourceSets[id] {
			return
		}
		visitedSourceSets[id] = true
		usedSourceSets[id] = true
		for _, child := range sourceSets[id].Children {
			useSourceSet(child)
		}
	}
	initialSourceSets := mapsKeys(usedSourceSets)
	for _, id := range initialSourceSets {
		useSourceSet(id)
	}
	usedFiles := map[int]bool{}
	for id := range usedSourceSets {
		for _, index := range sourceSets[id].Files {
			usedFiles[index] = true
		}
	}

	if err := compactV7RequireAll("config payload", payloads, usedPayloads); err != nil {
		return err
	}
	if err := compactV7RequireAll("compile environment", environments, usedEnvironments); err != nil {
		return err
	}
	if err := compactV7RequireAll("generated header family", families, usedFamilies); err != nil {
		return err
	}
	if err := compactV7RequireAll("source set", sourceSets, usedSourceSets); err != nil {
		return err
	}
	if err := compactV7RequireAll("reachability", reachability, usedReachability); err != nil {
		return err
	}
	if err := compactV7RequireAll("action recipe", recipes, usedRecipes); err != nil {
		return err
	}
	for _, group := range metadata.ActionSourceGroups {
		if !usedActionGroups[group.ID] {
			return fmt.Errorf(
				"compact-v7 action source group %s is not referenced",
				group.ID,
			)
		}
	}
	for _, group := range metadata.ActionRecipeGroups {
		if !usedRecipeGroups[group.ID] {
			return fmt.Errorf(
				"compact-v7 action recipe group %s is not referenced",
				group.ID,
			)
		}
	}
	for i, input := range metadata.SourceFiles {
		if !usedFiles[i+1] {
			return fmt.Errorf(
				"compact-v7 source file %d %q is not referenced",
				i+1,
				input.Path,
			)
		}
	}
	return metadata.validateCompactV7ProgramReferences(usedPrograms)
}

func compactV7RequireAll[T any](
	kind string,
	values map[string]T,
	used map[string]bool,
) error {
	for id := range values {
		if !used[id] {
			return fmt.Errorf("compact-v7 %s %s is not referenced", kind, id)
		}
	}
	return nil
}

func (metadata *CompactMetadataV7) validateCompactV7ProgramReferences(
	usedPrograms map[string]bool,
) error {
	probes := map[string]CompactKbuildProbe{}
	for _, probe := range metadata.KbuildProbes {
		probes[probe.ID] = probe
	}
	nodes := map[string]CompactKbuildFlagNode{}
	for _, node := range metadata.FlagNodes {
		nodes[node.ID] = node
	}
	terminals := map[string]bool{}
	for _, terminal := range metadata.FlagTerminals {
		terminals[terminal.ID] = true
	}
	programs := map[string]CompactKbuildFlagProgram{}
	for _, program := range metadata.FlagPrograms {
		programs[program.ID] = program
	}
	usedRoots := map[string]bool{}
	usedProbes := map[string]bool{}
	programStates := map[string]uint8{}
	var useRoot func(string) error
	var useProgram func(string) error
	useRoot = func(root string) error {
		if usedRoots[root] {
			return nil
		}
		usedRoots[root] = true
		if terminals[root] {
			return nil
		}
		node, ok := nodes[root]
		if !ok {
			return fmt.Errorf("compact-v7 references unknown flag root %s", root)
		}
		switch node.Kind {
		case "concat":
			for _, child := range node.Children {
				if err := useRoot(child); err != nil {
					return err
				}
			}
			return nil
		case "select":
			usedProbes[node.Probe] = true
			probe := probes[node.Probe]
			if err := useProgram(probe.ContextProgram); err != nil {
				return err
			}
			if err := useRoot(node.WhenTrue); err != nil {
				return err
			}
			return useRoot(node.WhenFalse)
		default:
			return fmt.Errorf(
				"compact-v7 flag node %s has unknown kind %q",
				node.ID,
				node.Kind,
			)
		}
	}
	useProgram = func(id string) error {
		if programStates[id] == 2 {
			return nil
		}
		if programStates[id] == 1 {
			return fmt.Errorf("compact-v7 flag program/probe context cycle at %s", id)
		}
		program, ok := programs[id]
		if !ok {
			return fmt.Errorf("compact-v7 references unknown flag program %s", id)
		}
		programStates[id] = 1
		usedPrograms[id] = true
		if err := useRoot(program.Root); err != nil {
			return err
		}
		programStates[id] = 2
		return nil
	}
	initialPrograms := mapsKeys(usedPrograms)
	for _, id := range initialPrograms {
		if err := useProgram(id); err != nil {
			return err
		}
	}
	for _, program := range metadata.FlagPrograms {
		if !usedPrograms[program.ID] {
			return fmt.Errorf("compact-v7 flag program %s is not referenced", program.ID)
		}
	}
	for _, terminal := range metadata.FlagTerminals {
		if !usedRoots[terminal.ID] {
			return fmt.Errorf("compact-v7 flag terminal %s is not referenced", terminal.ID)
		}
	}
	for _, node := range metadata.FlagNodes {
		if !usedRoots[node.ID] {
			return fmt.Errorf("compact-v7 flag node %s is not referenced", node.ID)
		}
	}
	for _, probe := range metadata.KbuildProbes {
		if !usedProbes[probe.ID] {
			return fmt.Errorf("compact-v7 Kbuild probe %s is not referenced", probe.ID)
		}
	}
	return nil
}
