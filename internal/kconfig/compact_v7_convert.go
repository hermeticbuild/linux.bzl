package kconfig

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// CompactMetadataBatchV7WithOptions emits the isolated compact-v7 contract.
// Collection uses a private eager representation before content-addressing the
// lazy v7 graph.
func (t *Tree) CompactMetadataBatchV7WithOptions(
	configs []NamedConfig,
	opts CompactMetadataV7Options,
	graphForConfig func(*ResolvedConfig) (CompactConfigGraph, error),
) (*CompactMetadataV7, error) {
	if strings.TrimSpace(opts.ToolchainProfileID) == "" {
		return nil, fmt.Errorf("compact-v7 metadata requires a non-empty toolchain profile ID")
	}
	eagerOptions := opts.CompactMetadataOptions
	eagerOptions.collectSupportSourceInputs = true
	eager, err := t.CompactMetadataBatchWithOptions(
		configs,
		eagerOptions,
		graphForConfig,
	)
	if err != nil {
		return nil, err
	}
	return newCompactMetadataV7(
		eager,
		opts.CompileEnvironmentABI,
		opts.ToolchainProfileID,
	)
}

func newCompactMetadataV7(
	eager *CompactMetadata,
	compileEnvironmentABI string,
	toolchainProfile string,
) (*CompactMetadataV7, error) {
	if eager == nil {
		return nil, fmt.Errorf("compact-v7 conversion requires eager metadata")
	}
	if strings.TrimSpace(compileEnvironmentABI) == "" {
		return nil, fmt.Errorf("compact-v7 conversion requires a non-empty compile environment ABI")
	}
	if strings.TrimSpace(toolchainProfile) == "" {
		return nil, fmt.Errorf("compact-v7 conversion requires a non-empty toolchain profile ID")
	}
	if err := eager.validateContentIDs(); err != nil {
		return nil, fmt.Errorf("validate eager metadata input: %w", err)
	}

	variants := make(map[string]CompactObjectVariant, len(eager.ObjectVariants))
	for _, variant := range eager.ObjectVariants {
		variants[variant.Target] = variant
	}
	reachability, err := compactV7Reachability(eager.Configs, variants)
	if err != nil {
		return nil, err
	}

	environmentsByID := make(map[string]CompactCompileEnvironment, len(eager.CompileEnvironments))
	for _, environment := range eager.CompileEnvironments {
		if environment.ABI != compileEnvironmentABI {
			return nil, fmt.Errorf(
				"compact-v7 compile environment %s uses ABI %q, want %q",
				environment.ID,
				environment.ABI,
				compileEnvironmentABI,
			)
		}
		environmentsByID[environment.ID] = environment
	}
	familiesByID := make(map[string]CompactGeneratedHeaderFamily, len(eager.GeneratedHeaderFamilies))
	for _, family := range eager.GeneratedHeaderFamilies {
		familiesByID[family.ID] = family
	}

	usedEnvironments := map[string]bool{}
	usedFamilies := map[string]bool{}
	for target := range reachability {
		variant := variants[target]
		if variant.CompileEnvironment == "" {
			continue
		}
		environment, ok := environmentsByID[variant.CompileEnvironment]
		if !ok {
			return nil, fmt.Errorf(
				"compact-v7 reachable object %q references unknown compile environment %s",
				variant.Object,
				variant.CompileEnvironment,
			)
		}
		usedEnvironments[environment.ID] = true
		for _, familyID := range environment.GeneratedHeaderFamilies {
			if err := compactV7MarkFamily(familyID, familiesByID, usedFamilies, map[string]bool{}); err != nil {
				return nil, err
			}
		}
	}
	// Generated-header rules have a fixed family contract even when no selected
	// compile environment consumes a particular family directly.
	for familyID := range familiesByID {
		if err := compactV7MarkFamily(familyID, familiesByID, usedFamilies, map[string]bool{}); err != nil {
			return nil, err
		}
	}

	sourceFiles, err := compactV7ReachableSourceFiles(
		eager,
		variants,
		reachability,
		familiesByID,
		usedFamilies,
	)
	if err != nil {
		return nil, err
	}
	sourceInterner := newCompactV7SourceInterner(sourceFiles)
	programInterner := newCompactV7ProgramInterner()

	out := &CompactMetadataV7{
		Protocol:              CompactMetadataProtocolV7,
		ToolchainProfile:      toolchainProfile,
		CompileEnvironmentABI: compileEnvironmentABI,
	}

	referencedPayloads := map[string]bool{}
	configs := append([]CompactConfig(nil), eager.Configs...)
	sort.Slice(configs, func(i, j int) bool {
		return configs[i].Name < configs[j].Name
	})
	for _, config := range configs {
		referencedPayloads[config.ConfigPayload] = true
	}

	for _, id := range sortedCompactIDs(usedEnvironments) {
		environment := environmentsByID[id]
		out.CompileEnvironments = append(out.CompileEnvironments, environment)
		referencedPayloads[environment.ConfigPayload] = true
	}

	for _, id := range sortedCompactIDs(usedFamilies) {
		family := familiesByID[id]
		inputs, err := eager.expandedSourceInputGroup(
			family.SourceInputGroup,
			fmt.Sprintf("compact-v7 generated header family %q", family.ID),
		)
		if err != nil {
			return nil, err
		}
		sourceSet, err := sourceInterner.internFlat(
			inputs,
			fmt.Sprintf("compact-v7 generated header family %q", family.ID),
		)
		if err != nil {
			return nil, err
		}
		labels := append([]string(nil), family.Labels...)
		sort.Strings(labels)
		dependencies := append([]string(nil), family.Dependencies...)
		sort.Strings(dependencies)
		out.GeneratedHeaderFamilies = append(out.GeneratedHeaderFamilies, CompactGeneratedHeaderFamilyV7{
			ID:            family.ID,
			Name:          family.Name,
			ConfigPayload: family.ConfigPayload,
			Labels:        labels,
			Srcarch:       family.Srcarch,
			Dependencies:  dependencies,
			SourceSet:     sourceSet,
		})
		referencedPayloads[family.ConfigPayload] = true
	}

	payloadsByID := make(map[string]CompactConfigPayload, len(eager.ConfigPayloads))
	for _, payload := range eager.ConfigPayloads {
		payloadsByID[payload.ID] = payload
	}
	for _, id := range sortedCompactIDs(referencedPayloads) {
		payload, ok := payloadsByID[id]
		if !ok {
			return nil, fmt.Errorf("compact-v7 references unknown config payload %s", id)
		}
		out.ConfigPayloads = append(out.ConfigPayloads, payload)
	}

	reachabilityByTarget := map[string]string{}
	reachabilityByID := map[string]CompactReachabilitySignature{}
	for target, namesSet := range reachability {
		names := sortedCompactIDs(namesSet)
		signature := CompactReachabilitySignature{
			ID:      compactV7ReachabilityContentID(names),
			Configs: names,
		}
		if existing, ok := reachabilityByID[signature.ID]; ok &&
			!reflect.DeepEqual(existing, signature) {
			return nil, fmt.Errorf("compact-v7 reachability signatures collide at %s", signature.ID)
		}
		reachabilityByID[signature.ID] = signature
		reachabilityByTarget[target] = signature.ID
	}
	for _, id := range sortedCompactIDs(reachabilityByID) {
		out.ReachabilitySignatures = append(
			out.ReachabilitySignatures,
			reachabilityByID[id],
		)
	}

	recipesByID := map[string]CompactActionRecipe{}
	convertedByOldTarget := map[string]CompactObjectVariantV7{}
	converting := map[string]bool{}
	var convertObject func(string) (CompactObjectVariantV7, error)
	convertObject = func(oldTarget string) (CompactObjectVariantV7, error) {
		if converted, ok := convertedByOldTarget[oldTarget]; ok {
			return converted, nil
		}
		if converting[oldTarget] {
			return CompactObjectVariantV7{}, fmt.Errorf(
				"compact-v7 object dependency cycle at %q",
				oldTarget,
			)
		}
		old, ok := variants[oldTarget]
		if !ok {
			return CompactObjectVariantV7{}, fmt.Errorf(
				"compact-v7 references unknown eager object target %q",
				oldTarget,
			)
		}
		if _, ok := reachability[oldTarget]; !ok {
			return CompactObjectVariantV7{}, fmt.Errorf(
				"compact-v7 reachable object %q references pruned target %q",
				old.Object,
				oldTarget,
			)
		}
		converting[oldTarget] = true
		defer delete(converting, oldTarget)

		deps := make([]string, 0, len(old.Deps))
		depContentIDs := make([]string, 0, len(old.Deps))
		for _, oldDependency := range old.Deps {
			dependency, err := convertObject(oldDependency)
			if err != nil {
				return CompactObjectVariantV7{}, err
			}
			deps = append(deps, dependency.Target)
			depContentIDs = append(depContentIDs, dependency.ContentID)
		}
		sort.Strings(deps)
		sort.Strings(depContentIDs)

		members := make([]string, 0, len(old.Members))
		memberContentIDs := make([]string, 0, len(old.Members))
		for _, oldMember := range old.Members {
			member, err := convertObject(oldMember)
			if err != nil {
				return CompactObjectVariantV7{}, err
			}
			members = append(members, member.Target)
			memberContentIDs = append(memberContentIDs, member.ContentID)
		}

		actionSourceGroup := ""
		if old.SourceInputGroup != 0 {
			inputs, err := eager.expandedSourceInputGroup(
				old.SourceInputGroup,
				fmt.Sprintf("compact-v7 object %q", old.Object),
			)
			if err != nil {
				return CompactObjectVariantV7{}, err
			}
			sourceSet, err := sourceInterner.internFlat(
				inputs,
				fmt.Sprintf("compact-v7 object %q", old.Object),
			)
			if err != nil {
				return CompactObjectVariantV7{}, err
			}
			primarySource := old.Source
			if primarySource == "" && isArm64NvheObject(old.Object) {
				primarySource = "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"
			}
			if primarySource == "" {
				return CompactObjectVariantV7{}, fmt.Errorf(
					"compact-v7 object %q has source inputs but no primary source",
					old.Object,
				)
			}
			actionSourceGroup, err = sourceInterner.internActionGroup(
				sourceSet,
				primarySource,
			)
			if err != nil {
				return CompactObjectVariantV7{}, fmt.Errorf(
					"compact-v7 object %q: %w",
					old.Object,
					err,
				)
			}
		}

		flagProgram := programInterner.internExpression(old.flagProgram)
		if old.flagProgram == nil {
			flagProgram = programInterner.internLiteral(old.Flags)
		}
		removeFlagProgram := programInterner.internExpression(old.removeFlagProgram)
		if old.removeFlagProgram == nil {
			removeFlagProgram = programInterner.internLiteral(old.RemoveFlags)
		}
		recipe := CompactActionRecipe{
			Kind:              compactV7ActionKind(old),
			Language:          compactV7SourceLanguage(old.Source),
			Mode:              old.Mode,
			FlagProgram:       flagProgram,
			RemoveFlagProgram: removeFlagProgram,
			ModuleRoot:        old.ModuleRoot,
			ModName:           old.ModName,
			ObjtoolArgs:       append([]string(nil), old.ObjtoolArgs...),
			ObjtoolDisabled:   old.ObjtoolDisabled,
			ObjtoolForce:      old.ObjtoolForce,
		}
		recipe.ID = compactV7ActionRecipeContentID(
			recipe,
			toolchainProfile,
			compileEnvironmentABI,
		)
		if existing, ok := recipesByID[recipe.ID]; ok && !reflect.DeepEqual(existing, recipe) {
			return CompactObjectVariantV7{}, fmt.Errorf(
				"compact-v7 action recipes collide at %s",
				recipe.ID,
			)
		}
		recipesByID[recipe.ID] = recipe

		contentID := compactV7ObjectContentID(
			old.Object,
			recipe.ID,
			old.CompileEnvironment,
			actionSourceGroup,
			depContentIDs,
			memberContentIDs,
			compileEnvironmentABI,
		)
		converted := CompactObjectVariantV7{
			Target:             sanitizeTargetName(strings.TrimSuffix(old.Object, ".o")) + "__" + compactShortID(contentID),
			ContentID:          contentID,
			Object:             old.Object,
			Recipe:             recipe.ID,
			Reachability:       reachabilityByTarget[oldTarget],
			CompileEnvironment: old.CompileEnvironment,
			ActionSourceGroup:  actionSourceGroup,
			Deps:               deps,
			Members:            members,
		}
		convertedByOldTarget[oldTarget] = converted
		return converted, nil
	}

	oldTargets := sortedCompactIDs(reachability)
	for _, target := range oldTargets {
		if _, err := convertObject(target); err != nil {
			return nil, err
		}
	}

	for _, config := range configs {
		supportSourceSet, err := sourceInterner.internFlat(
			config.supportSourceInputs,
			fmt.Sprintf("compact-v7 config %q support sources", config.Name),
		)
		if err != nil {
			return nil, err
		}
		converted := CompactConfigV7{
			Name:             config.Name,
			ConfigPayload:    config.ConfigPayload,
			SupportSourceSet: supportSourceSet,
			imageTarget:      config.imageTarget,
		}
		for _, oldTarget := range config.ObjectTargets {
			object, ok := convertedByOldTarget[oldTarget]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 config %q references pruned object target %q",
					config.Name,
					oldTarget,
				)
			}
			converted.ObjectTargets = append(converted.ObjectTargets, object.Target)
		}
		for _, oldTarget := range config.ModuleObjectTargets {
			object, ok := convertedByOldTarget[oldTarget]
			if !ok {
				return nil, fmt.Errorf(
					"compact-v7 config %q references pruned module target %q",
					config.Name,
					oldTarget,
				)
			}
			converted.ModuleObjectTargets = append(
				converted.ModuleObjectTargets,
				object.Target,
			)
		}
		out.Configs = append(out.Configs, converted)
	}

	for _, id := range sortedCompactIDs(recipesByID) {
		out.ActionRecipes = append(out.ActionRecipes, recipesByID[id])
	}

	objectsByTarget := map[string]CompactObjectVariantV7{}
	for _, oldTarget := range oldTargets {
		object := convertedByOldTarget[oldTarget]
		if existing, ok := objectsByTarget[object.Target]; ok &&
			!reflect.DeepEqual(existing, object) {
			return nil, fmt.Errorf(
				"compact-v7 objects %q and %q produce duplicate target %q",
				existing.Object,
				object.Object,
				object.Target,
			)
		}
		objectsByTarget[object.Target] = object
	}

	type recipeGroupKey struct {
		recipe       string
		reachability string
	}
	groupTargets := map[recipeGroupKey][]string{}
	for target, object := range objectsByTarget {
		key := recipeGroupKey{
			recipe:       object.Recipe,
			reachability: object.Reachability,
		}
		groupTargets[key] = append(groupTargets[key], target)
	}
	groupsByID := map[string]CompactActionRecipeGroup{}
	for key, targets := range groupTargets {
		sort.Strings(targets)
		contentIDs := make([]string, 0, len(targets))
		for _, target := range targets {
			contentIDs = append(contentIDs, objectsByTarget[target].ContentID)
		}
		group := CompactActionRecipeGroup{
			Recipe:       key.recipe,
			Reachability: key.reachability,
			Objects:      targets,
		}
		group.ID = compactV7ActionRecipeGroupContentID(
			group.Recipe,
			group.Reachability,
			contentIDs,
		)
		if existing, ok := groupsByID[group.ID]; ok && !reflect.DeepEqual(existing, group) {
			return nil, fmt.Errorf(
				"compact-v7 action recipe groups collide at %s",
				group.ID,
			)
		}
		groupsByID[group.ID] = group
		for _, target := range targets {
			object := objectsByTarget[target]
			object.RecipeGroup = group.ID
			objectsByTarget[target] = object
		}
	}
	for _, id := range sortedCompactIDs(groupsByID) {
		out.ActionRecipeGroups = append(out.ActionRecipeGroups, groupsByID[id])
	}
	for _, target := range sortedCompactIDs(objectsByTarget) {
		out.ObjectVariants = append(out.ObjectVariants, objectsByTarget[target])
	}

	programInterner.apply(out)
	sourceInterner.apply(out)
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func compactV7Reachability(
	configs []CompactConfig,
	variants map[string]CompactObjectVariant,
) (map[string]map[string]bool, error) {
	reachability := map[string]map[string]bool{}
	for _, config := range configs {
		seen := map[string]bool{}
		var visit func(string) error
		visit = func(target string) error {
			if seen[target] {
				return nil
			}
			variant, ok := variants[target]
			if !ok {
				return fmt.Errorf(
					"compact-v7 config %q reaches unknown object target %q",
					config.Name,
					target,
				)
			}
			seen[target] = true
			if reachability[target] == nil {
				reachability[target] = map[string]bool{}
			}
			reachability[target][config.Name] = true
			for _, dependency := range variant.Deps {
				if err := visit(dependency); err != nil {
					return err
				}
			}
			for _, member := range variant.Members {
				if err := visit(member); err != nil {
					return err
				}
			}
			return nil
		}
		for _, target := range config.ObjectTargets {
			if err := visit(target); err != nil {
				return nil, err
			}
		}
		for _, target := range config.ModuleObjectTargets {
			if err := visit(target); err != nil {
				return nil, err
			}
		}
	}
	return reachability, nil
}

func compactV7MarkFamily(
	id string,
	families map[string]CompactGeneratedHeaderFamily,
	used map[string]bool,
	stack map[string]bool,
) error {
	if used[id] {
		return nil
	}
	family, ok := families[id]
	if !ok {
		return fmt.Errorf("compact-v7 references unknown generated header family %s", id)
	}
	if stack[id] {
		return fmt.Errorf("compact-v7 generated header family dependency cycle at %s", id)
	}
	stack[id] = true
	for _, dependency := range family.Dependencies {
		if err := compactV7MarkFamily(dependency, families, used, stack); err != nil {
			return err
		}
	}
	delete(stack, id)
	used[id] = true
	return nil
}

func compactV7ReachableSourceFiles(
	eager *CompactMetadata,
	variants map[string]CompactObjectVariant,
	reachability map[string]map[string]bool,
	families map[string]CompactGeneratedHeaderFamily,
	usedFamilies map[string]bool,
) ([]CompactSourceInput, error) {
	byPath := map[string]CompactSourceInput{}
	addInputs := func(inputs []CompactSourceInput) error {
		for _, input := range inputs {
			if existing, ok := byPath[input.Path]; ok && existing.Digest != input.Digest {
				return fmt.Errorf(
					"compact-v7 source path %q has conflicting digests %q and %q",
					input.Path,
					existing.Digest,
					input.Digest,
				)
			}
			byPath[input.Path] = input
		}
		return nil
	}
	addGroup := func(group int, context string) error {
		if group == 0 {
			return nil
		}
		inputs, err := eager.expandedSourceInputGroup(group, context)
		if err != nil {
			return err
		}
		return addInputs(inputs)
	}
	for target := range reachability {
		variant := variants[target]
		if err := addGroup(
			variant.SourceInputGroup,
			fmt.Sprintf("compact-v7 object %q", variant.Object),
		); err != nil {
			return nil, err
		}
	}
	for _, config := range eager.Configs {
		if err := addInputs(config.supportSourceInputs); err != nil {
			return nil, err
		}
	}
	for id := range usedFamilies {
		family := families[id]
		if err := addGroup(
			family.SourceInputGroup,
			fmt.Sprintf("compact-v7 generated header family %q", family.ID),
		); err != nil {
			return nil, err
		}
	}
	out := make([]CompactSourceInput, 0, len(byPath))
	for _, input := range byPath {
		out = append(out, input)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out, nil
}
