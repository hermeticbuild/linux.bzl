package kconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig/buildgen"
)

type NamedConfig struct {
	Name        string
	Flags       map[string]string
	AllNoConfig bool
}

type CompactMetadata struct {
	Configs                 []CompactConfig                `json:"configs"`
	ConfigPayloads          []CompactConfigPayload         `json:"config_payloads"`
	CompileEnvironments     []CompactCompileEnvironment    `json:"compile_environments"`
	GeneratedHeaderFamilies []CompactGeneratedHeaderFamily `json:"generated_header_families"`
	SourceFiles             []CompactSourceInput           `json:"source_files"`
	SourceInputGroups       []string                       `json:"source_input_groups"`
	ActionGroups            []CompactActionGroup           `json:"action_groups"`
	ObjectVariants          []CompactObjectVariant         `json:"object_variants"`
}

type CompactConfig struct {
	Name                string   `json:"name"`
	ConfigPayload       string   `json:"config_payload,omitempty"`
	ObjectTargets       []string `json:"object_targets"`
	ModuleObjectTargets []string `json:"module_object_targets,omitempty"`
	imageTarget         string
}

type CompactSourceInput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type CompactConfigPayload struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	fragment map[string]string
}

type CompactCompileEnvironment struct {
	ID                      string   `json:"id"`
	ABI                     string   `json:"abi"`
	ConfigPayload           string   `json:"config_payload"`
	GeneratedHeaderFamilies []string `json:"generated_header_families,omitempty"`
}

type CompactGeneratedHeaderFamily struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	ConfigPayload    string   `json:"config_payload"`
	Labels           []string `json:"labels,omitempty"`
	Srcarch          string   `json:"srcarch"`
	Dependencies     []string `json:"dependencies,omitempty"`
	SourceInputGroup int      `json:"source_input_group,omitempty"`
	sourceInputs     []CompactSourceInput
}

type CompactObjectVariant struct {
	Target                   string `json:"target"`
	ContentID                string `json:"content_id,omitempty"`
	CompileEnvironment       string `json:"compile_environment,omitempty"`
	Object                   string `json:"object"`
	Source                   string `json:"source,omitempty"`
	SourceInputGroup         int    `json:"source_input_group,omitempty"`
	sourceInputs             []CompactSourceInput
	Mode                     string   `json:"mode"`
	ModuleRoot               bool     `json:"module_root,omitempty"`
	ModName                  string   `json:"modname,omitempty"`
	Flags                    []string `json:"flags,omitempty"`
	RemoveFlags              []string `json:"remove_flags,omitempty"`
	ObjtoolArgs              []string `json:"objtool_args,omitempty"`
	ObjtoolDisabled          bool     `json:"objtool_disabled,omitempty"`
	ObjtoolForce             bool     `json:"objtool_force,omitempty"`
	configFragment           map[string]string
	Deps                     []string `json:"deps,omitempty"`
	Members                  []string `json:"members,omitempty"`
	generatedHeaderFamilyIDs []string
}

// CompactActionGroup is the only lazy graph metadata layered on compact-v6.
// Objects share one configured rule when their concrete action recipe and the
// set of configs that can reach them are identical.
type CompactActionGroup struct {
	ID               string   `json:"id"`
	RecipeID         string   `json:"recipe_id"`
	ReachableConfigs []string `json:"reachable_configs"`
	ObjectTargets    []string `json:"object_targets"`
}

type CompactBuildFileOptions struct {
	Arch               string
	Version            string
	Visibility         []string
	RuleLoadLabel      string
	BaseConfig         string
	ObjectLabelPackage string
	Exports            []string
	SourceLabelPackage string
	SourceASN1Compiler string
	SourceObjtool      string
	SourceRelacheck    string
	SourceRootLabel    string
	Srcarch            string
}

type CompactMetadataOptions struct {
	ObjectDir             string
	SourceRoot            string
	SourceRoots           map[string]string
	LibraryDirs           []string
	CompileEnvironmentABI string
	KernelVersion         string
	// Srcarch selects architecture include roots while scanning source files for
	// CONFIG_* dependencies.
	Srcarch string
}

// CompactConfigGraph binds one resolved configuration to its Kbuild graph and
// the Bazel target that produces that configuration's generated headers.
type CompactConfigGraph struct {
	Kbuild                *KbuildFile
	GeneratedHeadersLabel string
}

// CompactMetadataBatchWithOptions resolves and emits multiple config-specific
// Kbuild graphs while sharing immutable source scanning work for the invocation.
func (t *Tree) CompactMetadataBatchWithOptions(
	configs []NamedConfig,
	opts CompactMetadataOptions,
	graphForConfig func(*ResolvedConfig) (CompactConfigGraph, error),
) (*CompactMetadata, error) {
	if graphForConfig == nil {
		return nil, fmt.Errorf("compact metadata config graph resolver must not be nil")
	}
	if strings.TrimSpace(opts.CompileEnvironmentABI) == "" {
		return nil, fmt.Errorf("compact metadata requires a non-empty compile environment ABI")
	}
	if opts.SourceRoot == "" && len(opts.SourceRoots) == 0 {
		return nil, fmt.Errorf("compact metadata requires a source root for exact input scanning")
	}
	variants := map[string]CompactObjectVariant{}
	out := &CompactMetadata{}
	configPayloads := map[string]CompactConfigPayload{}
	compileEnvironments := map[string]CompactCompileEnvironment{}
	generatedHeaderFamilies := map[string]CompactGeneratedHeaderFamily{}
	sourceInputInterner := newCompactSourceInputInterner()
	seenConfigs := map[string]bool{}
	seenImageTargets := map[string]string{}
	sourceCache := newCompactSourceCache()
	for _, named := range configs {
		if named.Name == "" {
			return nil, fmt.Errorf("compact config name must not be empty")
		}
		if seenConfigs[named.Name] {
			return nil, fmt.Errorf("duplicate compact config name %q", named.Name)
		}
		seenConfigs[named.Name] = true

		resolved, err := t.ResolveConfigWithOptions(named.Name, named.Flags, ResolveConfigOptions{
			AllNoConfig: named.AllNoConfig,
		})
		if err != nil {
			return nil, err
		}
		graph, err := graphForConfig(resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve Kbuild for config %q: %w", named.Name, err)
		}
		if graph.Kbuild == nil {
			return nil, fmt.Errorf("resolve Kbuild for config %q: nil Kbuild", named.Name)
		}
		if strings.TrimSpace(graph.GeneratedHeadersLabel) == "" {
			return nil, fmt.Errorf("resolve Kbuild for config %q: generated headers label must not be empty", named.Name)
		}
		kb := graph.Kbuild
		generatedHeadersLabel := graph.GeneratedHeadersLabel
		scanner := newConfigSourceScannerWithCache(opts, sourceCache)
		fullConfigPayload := newCompactConfigPayload(compactFullConfigFragment(resolved))
		configPayloads[fullConfigPayload.ID] = fullConfigPayload
		configHeaderFamilies := compactGeneratedHeaderFamilySet{}
		footprints, err := generatedHeaderFamilyFootprints(resolved, opts, scanner)
		if err != nil {
			return nil, fmt.Errorf("derive generated headers for config %q: %w", named.Name, err)
		}
		for _, footprint := range footprints {
			if _, ok := configHeaderFamilies[footprint.name]; ok {
				return nil, fmt.Errorf(
					"derive generated headers for config %q: duplicate family %q",
					named.Name,
					footprint.name,
				)
			}
			dependencyIDs := make([]string, 0, len(footprint.dependencies))
			for _, dependencyName := range footprint.dependencies {
				dependency, ok := configHeaderFamilies[dependencyName]
				if !ok {
					return nil, fmt.Errorf(
						"derive generated headers for config %q: family %q depends on unknown or later family %q",
						named.Name,
						footprint.name,
						dependencyName,
					)
				}
				dependencyIDs = append(dependencyIDs, dependency.ID)
			}
			payload := newCompactConfigPayload(footprint.fragment)
			configPayloads[payload.ID] = payload
			family := newCompactGeneratedHeaderFamily(
				footprint.name,
				payload.ID,
				generatedHeadersLabel,
				opts.Srcarch,
				dependencyIDs,
				footprint.sourceInputs,
			)
			family.SourceInputGroup, err = sourceInputInterner.intern(
				family.sourceInputs,
				fmt.Sprintf("generated header family %q", family.ID),
			)
			if err != nil {
				return nil, err
			}
			family.sourceInputs = nil
			if existing, ok := generatedHeaderFamilies[family.ID]; ok {
				existing.Labels = appendUniqueStrings(existing.Labels, family.Labels...)
				generatedHeaderFamilies[family.ID] = existing
			} else {
				generatedHeaderFamilies[family.ID] = family
			}
			configHeaderFamilies[family.Name] = family
		}
		imageTarget := sanitizeTargetName(named.Name) + "_image"
		if existing := seenImageTargets[imageTarget]; existing != "" {
			return nil, fmt.Errorf("compact config names %q and %q produce duplicate image target %q", existing, named.Name, imageTarget)
		}
		seenImageTargets[imageTarget] = named.Name

		objects := kb.resolvedObjects(resolved)
		resolvedVariants := compactVariantMemo{}
		for _, object := range objects.all() {
			if rustSDKOwnsObject(object.object) {
				continue
			}
			variant, err := resolvedVariants.variantFor(
				object.object,
				resolved,
				opts,
				objects,
				scanner,
				configHeaderFamilies,
			)
			if err != nil {
				return nil, err
			}
			variant.SourceInputGroup, err = sourceInputInterner.intern(
				variant.sourceInputs,
				fmt.Sprintf("object target %q", variant.Target),
			)
			if err != nil {
				return nil, err
			}
			variant.sourceInputs = nil
			resolvedVariants[object.object] = variant
			if existing, ok := variants[variant.Target]; ok && !existing.equal(variant) {
				return nil, fmt.Errorf("object variants %q and %q produce duplicate target %q", existing.Object, variant.Object, variant.Target)
			}
			variants[variant.Target] = variant
			if variant.CompileEnvironment != "" {
				payload := newCompactConfigPayload(variant.configFragment)
				configPayloads[payload.ID] = payload
				environment := newCompactCompileEnvironment(
					opts.CompileEnvironmentABI,
					payload.ID,
					variant.generatedHeaderFamilyIDs,
				)
				if environment.ID != variant.CompileEnvironment {
					return nil, fmt.Errorf(
						"internal error: object %q compile environment is %q, want %q",
						variant.Object,
						variant.CompileEnvironment,
						environment.ID,
					)
				}
				compileEnvironments[environment.ID] = environment
			}
		}

		variantsByTarget := map[string]CompactObjectVariant{}
		for _, variant := range resolvedVariants {
			variantsByTarget[variant.Target] = variant
		}

		targets := make([]string, 0, len(objects.roots))
		moduleTargets := make([]string, 0, len(objects.roots))
		appendRootTargets := func(roots []resolvedKbuildObject) error {
			for _, object := range roots {
				if rustSDKOwnsObject(object.object) {
					continue
				}
				variant, ok := resolvedVariants[object.object]
				if !ok {
					return fmt.Errorf("internal error: missing resolved variant for %q", object.object)
				}
				switch variant.Mode {
				case "y":
					expanded, err := imageObjectTargetsForVariant(variant, variantsByTarget)
					if err != nil {
						return err
					}
					targets = append(targets, expanded...)
				case "m":
					moduleTargets = append(moduleTargets, variant.Target)
				}
			}
			return nil
		}
		if err := appendRootTargets(objects.roots); err != nil {
			return nil, err
		}
		if err := appendRootTargets(objects.orderedLibRoots(opts.LibraryDirs)); err != nil {
			return nil, err
		}
		out.Configs = append(out.Configs, CompactConfig{
			Name:                named.Name,
			ConfigPayload:       fullConfigPayload.ID,
			ObjectTargets:       targets,
			ModuleObjectTargets: moduleTargets,
			imageTarget:         imageTarget,
		})
	}

	targets := make([]string, 0, len(variants))
	for target := range variants {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		out.ObjectVariants = append(out.ObjectVariants, variants[target])
	}
	out.ConfigPayloads = sortedCompactConfigPayloads(configPayloads)
	out.CompileEnvironments = sortedCompactCompileEnvironments(compileEnvironments)
	out.GeneratedHeaderFamilies = sortedCompactGeneratedHeaderFamilies(generatedHeaderFamilies)
	sourceInputInterner.apply(out)
	if err := out.canonicalizeSourceInputIndex(); err != nil {
		return nil, err
	}
	if err := out.validateContentIDs(); err != nil {
		return nil, err
	}
	if err := out.ensureActionGroups(); err != nil {
		return nil, err
	}
	return out, nil
}

// The Rust-for-Linux support crates have a configuration-specific build graph
// that does not follow the ordinary one-source-to-one-object Kbuild model.
// Bazel builds them as a dedicated SDK and injects their objects into vmlinux.
func rustSDKOwnsObject(object string) bool {
	object = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(object)), "./")
	return strings.HasPrefix(object, "rust/")
}

func imageObjectTargetsForVariant(variant CompactObjectVariant, variantsByTarget map[string]CompactObjectVariant) ([]string, error) {
	return imageObjectTargetsForVariantStack(variant, variantsByTarget, map[string]bool{})
}

func imageObjectTargetsForVariantStack(variant CompactObjectVariant, variantsByTarget map[string]CompactObjectVariant, stack map[string]bool) ([]string, error) {
	if len(variant.Members) == 0 || keepLinkedBuiltinComposite(variant.Object) {
		return []string{variant.Target}, nil
	}
	if stack[variant.Target] {
		return nil, fmt.Errorf("cycle in compact image object graph at %q", variant.Object)
	}
	stack[variant.Target] = true
	out := []string{}
	for _, memberTarget := range variant.Members {
		member, ok := variantsByTarget[memberTarget]
		if !ok {
			return nil, fmt.Errorf("compact image member target %q for %q is missing", memberTarget, variant.Object)
		}
		expanded, err := imageObjectTargetsForVariantStack(member, variantsByTarget, stack)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	delete(stack, variant.Target)
	return out, nil
}

func keepLinkedBuiltinComposite(object string) bool {
	return object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"
}

const (
	compactActionRecipeDomain = "linux-compact-concrete-action-recipe-v1"
	compactReachabilityDomain = "linux-compact-reachability-v1"
	compactActionGroupDomain  = "linux-compact-action-group-v1"
)

func compactActionKind(variant CompactObjectVariant) string {
	if len(variant.Members) == 0 {
		return "compile"
	}
	if variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o" {
		return "arm64_nvhe"
	}
	return "composite"
}

func compactSourceLanguage(source string) string {
	switch filepath.Ext(source) {
	case ".S", ".s":
		return "asm"
	case ".c":
		return "c"
	default:
		return ""
	}
}

func compactConcreteRecipeID(variant CompactObjectVariant) string {
	hasher := newCompactContentHasher(compactActionRecipeDomain)
	hasher.writeValue("kind=", compactActionKind(variant))
	hasher.writeValue("language=", compactSourceLanguage(variant.Source))
	hasher.writeValue("mode=", variant.Mode)
	hasher.writeValue("modname=", variant.ModName)
	if variant.ModuleRoot {
		hasher.writeValue("module_root=true")
	}
	if variant.ObjtoolDisabled {
		hasher.writeValue("objtool_disabled=true")
	}
	if variant.ObjtoolForce {
		hasher.writeValue("objtool_force=true")
	}
	for _, flag := range variant.Flags {
		hasher.writeValue("flag=", flag)
	}
	for _, flag := range variant.RemoveFlags {
		hasher.writeValue("remove_flag=", flag)
	}
	for _, arg := range variant.ObjtoolArgs {
		hasher.writeValue("objtool_arg=", arg)
	}
	return hasher.id()
}

func compactReachabilityID(configs []string) string {
	hasher := newCompactContentHasher(compactReachabilityDomain)
	for _, config := range configs {
		hasher.writeValue("config=", config)
	}
	return hasher.id()
}

func compactActionGroupID(recipeID, reachabilityID string, variants map[string]CompactObjectVariant, targets []string) string {
	hasher := newCompactContentHasher(compactActionGroupDomain)
	hasher.writeValue("recipe=", recipeID)
	hasher.writeValue("reachability=", reachabilityID)
	for _, target := range targets {
		hasher.writeValue("object=", variants[target].ContentID)
	}
	return hasher.id()
}

func compactActionGroupRuleName(group CompactActionGroup) string {
	return "_action_group_" +
		compactShortID(group.RecipeID) + "_" +
		compactShortID(compactReachabilityID(group.ReachableConfigs))
}

func (m *CompactMetadata) deriveActionGroups() ([]CompactActionGroup, error) {
	variants := make(map[string]CompactObjectVariant, len(m.ObjectVariants))
	for _, variant := range m.ObjectVariants {
		if _, ok := variants[variant.Target]; ok {
			return nil, fmt.Errorf("derive compact action groups: duplicate object target %q", variant.Target)
		}
		variants[variant.Target] = variant
	}

	reachable := make(map[string]map[string]bool, len(variants))
	for _, config := range m.Configs {
		seen := map[string]bool{}
		var visit func(string) error
		visit = func(target string) error {
			if seen[target] {
				return nil
			}
			variant, ok := variants[target]
			if !ok {
				return fmt.Errorf("compact config %q reaches unknown object target %q", config.Name, target)
			}
			seen[target] = true
			if reachable[target] == nil {
				reachable[target] = map[string]bool{}
			}
			reachable[target][config.Name] = true
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

	type actionGroupKey struct {
		recipe       string
		reachability string
	}
	targetsByKey := map[actionGroupKey][]string{}
	configsByReachability := map[string][]string{}
	for _, variant := range m.ObjectVariants {
		namesSet := reachable[variant.Target]
		if len(namesSet) == 0 {
			continue
		}
		names := make([]string, 0, len(namesSet))
		for name := range namesSet {
			names = append(names, name)
		}
		sort.Strings(names)
		reachabilityID := compactReachabilityID(names)
		configsByReachability[reachabilityID] = names
		key := actionGroupKey{
			recipe:       compactConcreteRecipeID(variant),
			reachability: reachabilityID,
		}
		targetsByKey[key] = append(targetsByKey[key], variant.Target)
	}

	groups := make([]CompactActionGroup, 0, len(targetsByKey))
	for key, targets := range targetsByKey {
		sort.Strings(targets)
		groups = append(groups, CompactActionGroup{
			ID:               compactActionGroupID(key.recipe, key.reachability, variants, targets),
			RecipeID:         key.recipe,
			ReachableConfigs: append([]string(nil), configsByReachability[key.reachability]...),
			ObjectTargets:    targets,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].ID < groups[j].ID
	})
	return groups, nil
}

func compactActionGroupsEqual(left, right []CompactActionGroup) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].ID != right[i].ID ||
			left[i].RecipeID != right[i].RecipeID ||
			!stringSlicesEqual(left[i].ReachableConfigs, right[i].ReachableConfigs) ||
			!stringSlicesEqual(left[i].ObjectTargets, right[i].ObjectTargets) {
			return false
		}
	}
	return true
}

func (m *CompactMetadata) ensureActionGroups() error {
	groups, err := m.deriveActionGroups()
	if err != nil {
		return err
	}
	if len(m.ActionGroups) == 0 {
		m.ActionGroups = groups
		return nil
	}
	if !compactActionGroupsEqual(m.ActionGroups, groups) {
		return fmt.Errorf("compact action groups do not match the concrete object/config graph")
	}
	return nil
}

func (objects resolvedKbuildObjects) orderedLibRoots(libraryDirs []string) []resolvedKbuildObject {
	if len(objects.libRoots) == 0 {
		return nil
	}
	if len(libraryDirs) == 0 {
		out := append([]resolvedKbuildObject(nil), objects.libRoots...)
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].object < out[j].object
		})
		return out
	}

	byDir := map[string][]resolvedKbuildObject{}
	var remainingDirs []string
	seenRemainingDir := map[string]bool{}
	for _, root := range objects.libRoots {
		dir := cleanKbuildDir(root.directory)
		byDir[dir] = append(byDir[dir], root)
		if !seenRemainingDir[dir] {
			seenRemainingDir[dir] = true
			remainingDirs = append(remainingDirs, dir)
		}
	}
	appendDir := func(out []resolvedKbuildObject, dir string) []resolvedKbuildObject {
		roots := byDir[dir]
		if len(roots) == 0 {
			return out
		}
		sort.SliceStable(roots, func(i, j int) bool {
			return roots[i].object < roots[j].object
		})
		out = append(out, roots...)
		delete(byDir, dir)
		return out
	}

	out := []resolvedKbuildObject{}
	for _, dir := range libraryDirs {
		out = appendDir(out, cleanKbuildDir(dir))
	}
	for _, dir := range remainingDirs {
		out = appendDir(out, dir)
	}
	return out
}

func cleanKbuildDir(dir string) string {
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if dir == "." {
		return ""
	}
	return dir
}

func (v CompactObjectVariant) equal(other CompactObjectVariant) bool {
	if v.Target != other.Target || v.ContentID != other.ContentID || v.CompileEnvironment != other.CompileEnvironment || v.Object != other.Object || v.Source != other.Source || v.SourceInputGroup != other.SourceInputGroup || v.Mode != other.Mode || v.ModuleRoot != other.ModuleRoot || v.ModName != other.ModName || v.ObjtoolDisabled != other.ObjtoolDisabled || v.ObjtoolForce != other.ObjtoolForce || len(v.sourceInputs) != len(other.sourceInputs) || len(v.Flags) != len(other.Flags) || len(v.RemoveFlags) != len(other.RemoveFlags) || len(v.ObjtoolArgs) != len(other.ObjtoolArgs) || len(v.configFragment) != len(other.configFragment) || len(v.Deps) != len(other.Deps) || len(v.Members) != len(other.Members) {
		return false
	}
	for i := range v.sourceInputs {
		if v.sourceInputs[i] != other.sourceInputs[i] {
			return false
		}
	}
	for i := range v.Flags {
		if v.Flags[i] != other.Flags[i] {
			return false
		}
	}
	for i := range v.RemoveFlags {
		if v.RemoveFlags[i] != other.RemoveFlags[i] {
			return false
		}
	}
	for i := range v.ObjtoolArgs {
		if v.ObjtoolArgs[i] != other.ObjtoolArgs[i] {
			return false
		}
	}
	for i := range v.Deps {
		if v.Deps[i] != other.Deps[i] {
			return false
		}
	}
	for i := range v.Members {
		if v.Members[i] != other.Members[i] {
			return false
		}
	}
	for key, value := range v.configFragment {
		if other.configFragment[key] != value {
			return false
		}
	}
	return true
}

func (m *CompactMetadata) JSON() ([]byte, error) {
	if err := m.canonicalizeSourceInputIndex(); err != nil {
		return nil, err
	}
	if err := m.validateContentIDs(); err != nil {
		return nil, err
	}
	if err := m.ensureActionGroups(); err != nil {
		return nil, err
	}
	serializable := *m
	if serializable.Configs == nil {
		serializable.Configs = []CompactConfig{}
	}
	if serializable.ConfigPayloads == nil {
		serializable.ConfigPayloads = []CompactConfigPayload{}
	}
	if serializable.CompileEnvironments == nil {
		serializable.CompileEnvironments = []CompactCompileEnvironment{}
	}
	if serializable.GeneratedHeaderFamilies == nil {
		serializable.GeneratedHeaderFamilies = []CompactGeneratedHeaderFamily{}
	}
	if serializable.SourceFiles == nil {
		serializable.SourceFiles = []CompactSourceInput{}
	}
	if serializable.SourceInputGroups == nil {
		serializable.SourceInputGroups = []string{}
	}
	if serializable.ActionGroups == nil {
		serializable.ActionGroups = []CompactActionGroup{}
	}
	if serializable.ObjectVariants == nil {
		serializable.ObjectVariants = []CompactObjectVariant{}
	}
	data, err := json.MarshalIndent(&serializable, "", "  ")
	if err != nil {
		return nil, err
	}
	return compactShortStringArrays(append(data, '\n'), 80), nil
}

func compactShortStringArrays(data []byte, printWidth int) []byte {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasSuffix(strings.TrimSpace(line), "[") {
			out = append(out, line)
			continue
		}

		prefixEnd := strings.LastIndex(line, "[")
		if prefixEnd < 0 {
			out = append(out, line)
			continue
		}

		var values []string
		j := i + 1
		ok := true
		for ; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "]" || trimmed == "]," {
				break
			}
			value := strings.TrimSuffix(trimmed, ",")
			var decoded string
			if err := json.Unmarshal([]byte(value), &decoded); err != nil {
				ok = false
				break
			}
			values = append(values, value)
		}
		if !ok || j == len(lines) {
			out = append(out, line)
			continue
		}

		suffix := "]"
		if strings.TrimSpace(lines[j]) == "]," {
			suffix = "],"
		}
		candidate := line[:prefixEnd+1] + strings.Join(values, ", ") + suffix
		if len(candidate) > printWidth {
			out = append(out, line)
			continue
		}
		out = append(out, candidate)
		i = j
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

type resolvedKbuildObject struct {
	object          string
	directory       string
	mode            string
	modname         string
	flags           []resolvedKbuildFlag
	remove          []resolvedKbuildFlag
	objtoolArgs     []string
	objtoolDisabled bool
	objtoolForce    bool
	footprint       map[string]bool
	members         []string
	root            bool
}

type resolvedKbuildFlag struct {
	language string
	values   []string
}

type resolvedKbuildObjects struct {
	byName   map[string]*resolvedKbuildObject
	roots    []resolvedKbuildObject
	libRoots []resolvedKbuildObject
}

func (kb *KbuildFile) resolvedObjects(config *ResolvedConfig) resolvedKbuildObjects {
	byObject := map[string]*resolvedKbuildObject{}
	var rootOrder []string
	var libRootOrder []string
	seenRoot := map[string]bool{}
	seenLibRoot := map[string]bool{}
	for _, entry := range kb.resolvedObjectEntries(config) {
		mode := entry.Condition.Mode(config)
		if mode == "n" {
			continue
		}
		object := byObject[entry.Object]
		if object == nil {
			object = &resolvedKbuildObject{
				object:    entry.Object,
				directory: entry.Directory,
				mode:      mode,
				footprint: map[string]bool{},
			}
			byObject[entry.Object] = object
		}
		if object.directory == "" {
			object.directory = entry.Directory
		}
		if entry.Root {
			object.root = true
			if entry.Kind == "lib" {
				if !seenLibRoot[entry.Object] {
					seenLibRoot[entry.Object] = true
					libRootOrder = append(libRootOrder, entry.Object)
				}
			} else if !seenRoot[entry.Object] {
				seenRoot[entry.Object] = true
				rootOrder = append(rootOrder, entry.Object)
			}
		}
		if modePrecedence(mode) > modePrecedence(object.mode) {
			object.mode = mode
		}
		for _, ref := range entry.Condition.Refs() {
			object.footprint[ref] = true
		}
	}

	for _, member := range kb.resolvedCompositeMembers(config, byObject) {
		parent := byObject[member.Composite]
		if parent == nil {
			continue
		}
		mode := compositeMemberMode(parent.mode, member.Mode)
		if mode == "n" {
			continue
		}
		parent.members = appendUnique(parent.members, member.Object)
		object := byObject[member.Object]
		if object == nil {
			object = &resolvedKbuildObject{
				object:    member.Object,
				directory: member.Directory,
				mode:      mode,
				footprint: map[string]bool{},
			}
			byObject[member.Object] = object
		}
		object.modname = appendModName(object.modname, strings.TrimSuffix(member.Composite, ".o"))
		if object.directory == "" {
			object.directory = member.Directory
		}
		if modePrecedence(mode) > modePrecedence(object.mode) {
			object.mode = mode
		}
		for ref := range parent.footprint {
			object.footprint[ref] = true
		}
		for _, ref := range member.Condition.Refs() {
			object.footprint[ref] = true
		}
	}

	for _, object := range byObject {
		for _, flag := range kb.Flags {
			if flag.Scope == "object" && flag.Object != object.object {
				continue
			}
			if flag.Scope == "global" && !globalFlagAppliesToObject(flag, object) {
				continue
			}
			for _, ref := range flag.Condition.Refs() {
				object.footprint[ref] = true
			}
			if !flag.Condition.Enabled(config) {
				continue
			}
			object.flags = append(object.flags, resolvedKbuildFlag{
				language: flag.Language,
				values:   append([]string(nil), flag.Flags...),
			})
			for _, value := range flag.Flags {
				for _, ref := range configRefs(value) {
					object.footprint[ref] = true
				}
			}
		}
		for _, flag := range kb.RemoveFlags {
			if flag.Scope == "object" && flag.Object != object.object {
				continue
			}
			if flag.Scope == "global" && !globalFlagAppliesToObject(flag, object) {
				continue
			}
			for _, ref := range flag.Condition.Refs() {
				object.footprint[ref] = true
			}
			if !flag.Condition.Enabled(config) {
				continue
			}
			object.remove = append(object.remove, resolvedKbuildFlag{
				language: flag.Language,
				values:   append([]string(nil), flag.Flags...),
			})
			for _, value := range flag.Flags {
				for _, ref := range configRefs(value) {
					object.footprint[ref] = true
				}
			}
		}
		object.flags = append(object.flags, sanitizerKbuildFlags(config, kb.objectSettings, object)...)
		object.objtoolDisabled, object.objtoolForce, object.objtoolArgs = kbuildObjtoolSettings(kb, object)
	}

	out := resolvedKbuildObjects{
		byName: byObject,
	}
	for _, name := range rootOrder {
		if object := byObject[name]; object != nil && object.root {
			out.roots = append(out.roots, *object)
		}
	}
	for _, name := range libRootOrder {
		if object := byObject[name]; object != nil && object.root {
			out.libRoots = append(out.libRoots, *object)
		}
	}
	return out
}

func kbuildObjtoolSettings(kb *KbuildFile, object *resolvedKbuildObject) (disabled, force bool, args []string) {
	compileObject := kbuildCompileObjectName(object.object)
	settingsObject := object
	if compileObject != object.object {
		settingsObject = &resolvedKbuildObject{
			object:    compileObject,
			directory: object.directory,
		}
	}
	objectValue, directoryValue := kbuildObjectSettingValues(
		kb.objectSettings,
		settingsObject,
		"OBJECT_FILES_NON_STANDARD",
	)
	nonStandardValue, hasNonStandardValue := kbuildTargetVariableValue(
		kb.TargetVariables,
		compileObject,
		"OBJECT_FILES_NON_STANDARD",
	)
	nonStandard := firstKbuildSettingEnabled(
		false,
		conditionalSettingValue(nonStandardValue, hasNonStandardValue),
		objectValue,
		directoryValue,
	)
	enabledValue, hasEnabledValue := kbuildTargetVariableValue(
		kb.TargetVariables,
		compileObject,
		"objtool-enabled",
	)
	enabled := !nonStandard && compileObject == object.object
	if hasEnabledValue {
		enabled = strings.TrimSpace(enabledValue) != ""
		force = enabled
	}
	argsValue, hasArgsValue := kbuildTargetVariableValue(
		kb.TargetVariables,
		compileObject,
		"objtool-args",
	)
	if hasArgsValue {
		args = concreteKbuildFlags(kbuildFields(argsValue))
	}
	return !enabled, force, args
}

func conditionalSettingValue(value string, present bool) string {
	if !present {
		return ""
	}
	if strings.TrimSpace(value) == "" {
		return "n"
	}
	return value
}

func kbuildCompileObjectName(object string) string {
	for _, suffix := range []string{".pi.o", ".stub.o"} {
		if strings.HasSuffix(object, suffix) {
			return strings.TrimSuffix(object, suffix) + ".o"
		}
	}
	return object
}

func kbuildTargetVariableValue(variables []KbuildTargetVariable, object, name string) (string, bool) {
	value := ""
	assigned := false
	for _, variable := range variables {
		if variable.Variable != name || !kbuildTargetVariableMatches(variable.Targets, object) {
			continue
		}
		switch variable.Operator {
		case "+=":
			value = appendMakeValue(value, variable.Value)
			assigned = true
		case "?=":
			if !assigned {
				value = variable.Value
				assigned = true
			}
		default:
			value = variable.Value
			assigned = true
		}
	}
	return strings.TrimSpace(value), assigned
}

func kbuildTargetVariableMatches(targets []string, object string) bool {
	object = filepath.ToSlash(strings.TrimPrefix(object, "./"))
	for _, target := range targets {
		target = filepath.ToSlash(strings.TrimPrefix(target, "./"))
		prefix, suffix, pattern := strings.Cut(target, "%")
		if !pattern && target == object {
			return true
		}
		if pattern &&
			len(object) >= len(prefix)+len(suffix) &&
			strings.HasPrefix(object, prefix) &&
			strings.HasSuffix(object, suffix) {
			return true
		}
	}
	return false
}

func (kb *KbuildFile) resolvedObjectEntries(config *ResolvedConfig) []KbuildObject {
	if len(kb.objectAssigns) == 0 {
		return kb.Objects
	}

	type bucketKey struct {
		directory string
		kind      string
		mode      string
	}
	type assignmentRecord struct {
		key    bucketKey
		values []KbuildObject
		active bool
	}

	var records []*assignmentRecord
	recordsByKey := map[bucketKey][]*assignmentRecord{}
	assigned := map[bucketKey]bool{}
	for _, assignment := range kb.objectAssigns {
		mode := assignment.Condition.Mode(config)
		if mode == "n" {
			continue
		}
		key := bucketKey{directory: assignment.Directory, kind: assignment.Kind, mode: mode}
		values := make([]KbuildObject, 0, len(assignment.Objects))
		for _, object := range assignment.Objects {
			values = append(values, KbuildObject{
				Object:    object,
				Kind:      assignment.Kind,
				Directory: assignment.Directory,
				Condition: assignment.Condition,
				Root:      assignment.Root,
				Position:  assignment.Position,
			})
		}
		addRecord := func() {
			record := &assignmentRecord{
				key:    key,
				values: values,
				active: true,
			}
			records = append(records, record)
			recordsByKey[key] = append(recordsByKey[key], record)
		}
		switch assignment.Operator {
		case "+=":
			addRecord()
			assigned[key] = true
		case "?=":
			if !assigned[key] {
				addRecord()
				assigned[key] = true
			}
		default:
			for _, record := range recordsByKey[key] {
				record.active = false
			}
			addRecord()
			assigned[key] = true
		}
	}

	var out []KbuildObject
	for _, record := range records {
		if record.active {
			out = append(out, record.values...)
		}
	}
	return out
}

type resolvedCompositeMember struct {
	Composite string
	Object    string
	Directory string
	Mode      string
	Condition KbuildCondition
}

type compositeMemberValue struct {
	object    string
	directory string
	condition KbuildCondition
}

func (kb *KbuildFile) resolvedCompositeMembers(config *ResolvedConfig, parents map[string]*resolvedKbuildObject) []resolvedCompositeMember {
	if len(kb.compositeAssigns) == 0 {
		out := make([]resolvedCompositeMember, 0, len(kb.compositeMembers))
		for _, member := range kb.compositeMembers {
			out = append(out, resolvedCompositeMember{
				Composite: member.Composite,
				Object:    member.Object,
				Directory: member.Directory,
				Mode:      member.Condition.Mode(config),
				Condition: member.Condition,
			})
		}
		return out
	}

	type bucketKey struct {
		composite string
		mode      string
	}
	buckets := map[bucketKey][]compositeMemberValue{}
	var bucketOrder []bucketKey
	assigned := map[bucketKey]bool{}
	for _, assignment := range kb.compositeAssigns {
		if parents[assignment.Composite] == nil {
			continue
		}
		mode := assignment.Condition.Mode(config)
		if mode == "n" {
			continue
		}
		key := bucketKey{composite: assignment.Composite, mode: mode}
		if _, ok := buckets[key]; !ok {
			bucketOrder = append(bucketOrder, key)
		}
		values := make([]compositeMemberValue, 0, len(assignment.Objects))
		for _, object := range assignment.Objects {
			values = append(values, compositeMemberValue{
				object:    object,
				directory: assignment.Directory,
				condition: assignment.Condition,
			})
		}
		switch assignment.Operator {
		case "+=":
			buckets[key] = append(buckets[key], values...)
		case "?=":
			if !assigned[key] {
				buckets[key] = values
				assigned[key] = true
			}
		default:
			buckets[key] = values
			assigned[key] = true
		}
	}

	out := []resolvedCompositeMember{}
	for _, key := range bucketOrder {
		for _, value := range buckets[key] {
			out = append(out, resolvedCompositeMember{
				Composite: key.composite,
				Object:    value.object,
				Directory: value.directory,
				Mode:      key.mode,
				Condition: value.condition,
			})
		}
	}
	return out
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendModName(existing, value string) string {
	if existing == "" {
		return value
	}
	for _, part := range strings.Split(existing, ":") {
		if part == value {
			return existing
		}
	}
	parts := append(strings.Split(existing, ":"), value)
	sort.Strings(parts)
	return strings.Join(parts, ":")
}

func globalFlagAppliesToObject(flag KbuildFlag, object *resolvedKbuildObject) bool {
	flagDir := strings.TrimSuffix(flag.Directory, "/")
	objectDir := strings.TrimSuffix(object.directory, "/")
	if objectDir == flagDir {
		return true
	}
	if !flag.Recursive {
		return false
	}
	if flagDir == "" {
		return true
	}
	return strings.HasPrefix(objectDir+"/", flagDir+"/")
}

func (objects resolvedKbuildObjects) all() []resolvedKbuildObject {
	names := make([]string, 0, len(objects.byName))
	for name := range objects.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]resolvedKbuildObject, 0, len(names))
	for _, name := range names {
		out = append(out, *objects.byName[name])
	}
	return out
}

type compactVariantMemo map[string]CompactObjectVariant

type compactGeneratedHeaderFamilySet map[string]CompactGeneratedHeaderFamily

func (families compactGeneratedHeaderFamilySet) selectForAction(
	generatedIncludes []string,
	forceAll bool,
) ([]string, error) {
	if len(families) == 0 {
		return nil, nil
	}
	all, hasAll := families[compactGeneratedHeaderFamilyAll]
	monolithic := hasAll && len(families) == 1
	if forceAll {
		if !hasAll {
			return nil, fmt.Errorf("generated-header action requires the all family, but it is unavailable")
		}
		return []string{all.ID}, nil
	}

	names := map[string]bool{}
	for _, include := range generatedIncludes {
		name, precise := generatedHeaderFamilyNameForInclude(include)
		if name == "" {
			continue
		}
		if monolithic {
			return []string{all.ID}, nil
		}
		if !precise {
			return nil, fmt.Errorf("generated include %q is unclassified", include)
		}
		if _, ok := families[name]; !ok {
			return nil, fmt.Errorf(
				"generated include %q requires unavailable family %q",
				include,
				name,
			)
		}
		names[name] = true
	}
	ids := make([]string, 0, len(names))
	for name := range names {
		ids = append(ids, families[name].ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (memo compactVariantMemo) variantFor(
	name string,
	config *ResolvedConfig,
	opts CompactMetadataOptions,
	objects resolvedKbuildObjects,
	scanner *configSourceScanner,
	generatedHeaderFamilies compactGeneratedHeaderFamilySet,
) (CompactObjectVariant, error) {
	return memo.variantForStack(
		name,
		config,
		opts,
		objects,
		scanner,
		generatedHeaderFamilies,
		map[string]bool{},
	)
}

func (memo compactVariantMemo) variantForStack(
	name string,
	config *ResolvedConfig,
	opts CompactMetadataOptions,
	objects resolvedKbuildObjects,
	scanner *configSourceScanner,
	generatedHeaderFamilies compactGeneratedHeaderFamilySet,
	stack map[string]bool,
) (CompactObjectVariant, error) {
	if variant, ok := memo[name]; ok {
		return variant, nil
	}
	object, ok := objects.byName[name]
	if !ok {
		return CompactObjectVariant{}, fmt.Errorf("composite member %q is not present in resolved object graph", name)
	}
	if stack[name] {
		return CompactObjectVariant{}, fmt.Errorf("cycle in composite Kbuild object graph at %q", name)
	}
	stack[name] = true
	members := make([]string, 0, len(object.members))
	memberContentIDs := make([]string, 0, len(object.members))
	for _, member := range object.members {
		variant, err := memo.variantForStack(
			member,
			config,
			opts,
			objects,
			scanner,
			generatedHeaderFamilies,
			stack,
		)
		if err != nil {
			return CompactObjectVariant{}, err
		}
		members = append(members, variant.Target)
		memberContentIDs = append(memberContentIDs, variant.ContentID)
	}
	delete(stack, name)

	source := sourceForObject(opts.SourceRoot, opts.ObjectDir, object.object, opts.SourceRoots)
	specialSources := compactSpecialSourcesForObject(name)
	if specialSources.primary != "" {
		source = specialSources.primary
	}
	if len(members) != 0 {
		source = ""
	}
	if len(members) == 0 && source == "" {
		return CompactObjectVariant{}, fmt.Errorf("exact input scan cannot resolve a source for leaf object %q", name)
	}
	deps := []string{}
	depContentIDs := []string{}
	asn1ProvidedIncludes := []string{}
	if source != "" {
		for _, dep := range asn1HeaderDepsForSource(opts.SourceRoot, source) {
			if dep.object == name {
				continue
			}
			if _, ok := objects.byName[dep.object]; !ok {
				continue
			}
			variant, err := memo.variantForStack(
				dep.object,
				config,
				opts,
				objects,
				scanner,
				generatedHeaderFamilies,
				stack,
			)
			if err != nil {
				return CompactObjectVariant{}, err
			}
			depCount := len(deps)
			deps = appendUnique(deps, variant.Target)
			if len(deps) != depCount {
				depContentIDs = append(depContentIDs, variant.ContentID)
			}
			asn1ProvidedIncludes = appendUniqueStrings(asn1ProvidedIncludes, dep.include)
		}
		sort.Strings(deps)
		sort.Strings(depContentIDs)
	}

	var sourceRefs, generatedIncludes []string
	var sourceInputs []CompactSourceInput
	forceAllGeneratedHeaders := false
	if source != "" {
		flags := normalizeSourceRootFlags(filterResolvedKbuildFlags(object.flags, source), opts.SourceRoot)
		includeDirs, err := includeDirsFromFlags(flags, source)
		if err != nil {
			return CompactObjectVariant{}, fmt.Errorf(
				"model source include flags for %s: %w",
				name,
				err,
			)
		}
		actionIncludeSearch, err := scanner.actionIncludeSearch(source, flags)
		if err != nil {
			return CompactObjectVariant{}, fmt.Errorf(
				"model source include search for %s: %w",
				name,
				err,
			)
		}
		forcedSources, err := forcedSourceInputs(flags, source, name)
		if err != nil {
			return CompactObjectVariant{}, fmt.Errorf(
				"model forced source inputs for %s: %w",
				name,
				err,
			)
		}
		actionFootprint := compactObjectActionFootprintForObject(name, flags)
		if opts.Srcarch == "x86" && !object.objtoolDisabled {
			actionFootprint.configSymbols = appendUniqueStrings(
				actionFootprint.configSymbols,
				ObjtoolConfigSymbols()...,
			)
		}
		if object.root && object.mode == "m" {
			actionFootprint.configSymbols = appendUniqueStrings(
				actionFootprint.configSymbols,
				"CONFIG_LTO_CLANG",
			)
		}
		actionFootprint.providedIncludes = appendUniqueStrings(
			actionFootprint.providedIncludes,
			asn1ProvidedIncludes...,
		)
		profile := sourceScanKernel
		if object.mode == "m" {
			profile = sourceScanKernelModule
		}
		closure, err := scanner.closureForSourceConfigInputsSearchProfile(
			source,
			actionIncludeSearch,
			config,
			isAssemblySourcePath(source),
			actionFootprint.providedIncludes,
			profile,
		)
		if err != nil {
			return CompactObjectVariant{}, fmt.Errorf("scan source inputs for %s: %w", name, err)
		}
		sourceRefs = append(sourceRefs, closure.refs...)
		generatedIncludes = append(generatedIncludes, closure.generatedIncludes...)
		forceAllGeneratedHeaders = actionFootprint.fullGeneratedHeaders ||
			len(specialSources.inputs) != 0
		sourceRefs = appendUniqueStrings(sourceRefs, actionFootprint.configSymbols...)
		sourceInputs = append(sourceInputs, closure.sourceInputs...)
		for _, path := range actionFootprint.sourceInputs {
			input, err := scanner.inputForTreePath(path)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf(
					"record generated-input producer %s for %s: %w",
					path,
					name,
					err,
				)
			}
			sourceInputs = appendUniqueSourceInputs(sourceInputs, input)
		}
		for _, forcedSource := range forcedSources {
			forcedClosure, err := scanner.closureForSourceConfigInputsSearchProfile(
				forcedSource,
				actionIncludeSearch,
				config,
				isAssemblySourcePath(source),
				actionFootprint.providedIncludes,
				profile,
			)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf(
					"scan forced source input %s for %s: %w",
					forcedSource,
					name,
					err,
				)
			}
			sourceRefs = appendUniqueStrings(sourceRefs, forcedClosure.refs...)
			generatedIncludes = appendUniqueStrings(
				generatedIncludes,
				forcedClosure.generatedIncludes...,
			)
			sourceInputs = appendUniqueSourceInputs(sourceInputs, forcedClosure.sourceInputs...)
		}
		for _, generatedSource := range actionFootprint.closureInputs {
			generatedClosure, err := scanner.closureForSourceConfigInputsSearchProfile(
				generatedSource,
				actionIncludeSearch,
				config,
				isAssemblySourcePath(source),
				actionFootprint.providedIncludes,
				profile,
			)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf(
					"scan generated source input %s for %s: %w",
					generatedSource,
					name,
					err,
				)
			}
			sourceRefs = appendUniqueStrings(sourceRefs, generatedClosure.refs...)
			generatedIncludes = appendUniqueStrings(
				generatedIncludes,
				generatedClosure.generatedIncludes...,
			)
			sourceInputs = appendUniqueSourceInputs(sourceInputs, generatedClosure.sourceInputs...)
		}
		specialIncludeDirs := appendUniqueStrings(
			append([]string(nil), includeDirs...),
			specialSources.includeRoots...,
		)
		for _, input := range specialSources.inputs {
			if input.path == source {
				continue
			}
			specialProfile := profile
			if input.profile != sourceScanKernel {
				specialProfile = input.profile
			}
			specialClosure, err := scanner.closureForSourceConfigInputsProfile(
				input.path,
				specialIncludeDirs,
				config,
				isAssemblySourcePath(input.path),
				actionFootprint.providedIncludes,
				specialProfile,
			)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf(
					"scan special source input %s for %s: %w",
					input.path,
					name,
					err,
				)
			}
			sourceRefs = appendUniqueStrings(sourceRefs, specialClosure.refs...)
			generatedIncludes = appendUniqueStrings(
				generatedIncludes,
				specialClosure.generatedIncludes...,
			)
			sourceInputs = appendUniqueSourceInputs(sourceInputs, specialClosure.sourceInputs...)
			if !input.compiled {
				continue
			}
			for _, forcedSource := range defaultForcedSourceInputs(input.path) {
				forcedClosure, err := scanner.closureForSourceConfigInputsProfile(
					forcedSource,
					specialIncludeDirs,
					config,
					isAssemblySourcePath(input.path),
					actionFootprint.providedIncludes,
					specialProfile,
				)
				if err != nil {
					return CompactObjectVariant{}, fmt.Errorf(
						"scan forced source input %s for special input %s of %s: %w",
						forcedSource,
						input.path,
						name,
						err,
					)
				}
				sourceRefs = appendUniqueStrings(sourceRefs, forcedClosure.refs...)
				generatedIncludes = appendUniqueStrings(
					generatedIncludes,
					forcedClosure.generatedIncludes...,
				)
				sourceInputs = appendUniqueSourceInputs(sourceInputs, forcedClosure.sourceInputs...)
			}
		}
	}
	if isArm64NvheObject(name) {
		forceAllGeneratedHeaders = true
		for _, actionSource := range []string{
			"arch/arm64/kvm/hyp/nvhe/hyp.lds.S",
			"include/linux/compiler-version.h",
			"include/linux/kconfig.h",
		} {
			closure, err := scanner.closureForSourceConfigMode(actionSource, nil, config, true)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf(
					"scan arm64 nVHE action input %s for %s: %w",
					actionSource,
					name,
					err,
				)
			}
			sourceRefs = appendUniqueStrings(sourceRefs, closure.refs...)
			generatedIncludes = appendUniqueStrings(
				generatedIncludes,
				closure.generatedIncludes...,
			)
			sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
		}
	}
	generatedHeaderFamilyIDs, err := generatedHeaderFamilies.selectForAction(
		generatedIncludes,
		forceAllGeneratedHeaders,
	)
	if err != nil {
		return CompactObjectVariant{}, fmt.Errorf(
			"select generated-header families for %s: %w",
			name,
			err,
		)
	}
	variant := object.variant(
		config,
		source,
		sourceInputs,
		members,
		deps,
		memberContentIDs,
		depContentIDs,
		opts.SourceRoot,
		sourceRefs,
		opts.CompileEnvironmentABI,
		generatedHeaderFamilyIDs,
	)
	memo[name] = variant
	return variant, nil
}

type compactSpecialSourceInput struct {
	path     string
	compiled bool
	profile  sourceScanProfile
}

type compactSpecialSourceInputs struct {
	primary      string
	includeRoots []string
	inputs       []compactSpecialSourceInput
}

type compactObjectActionFootprint struct {
	sourceInputs         []string
	closureInputs        []string
	providedIncludes     []string
	configSymbols        []string
	fullGeneratedHeaders bool
}

func compactObjectActionFootprintForObject(object string, flags []string) compactObjectActionFootprint {
	footprint := compactObjectActionFootprint{}
	switch object {
	case "drivers/tty/vt/ucs.o":
		footprint.sourceInputs = []string{
			"drivers/tty/vt/ucs_width_table.h_shipped",
			"drivers/tty/vt/ucs_recompose_table.h_shipped",
			"drivers/tty/vt/ucs_fallback_table.h_shipped",
		}
		footprint.providedIncludes = []string{
			"ucs_width_table.h",
			"ucs_recompose_table.h",
			"ucs_fallback_table.h",
		}
	case "drivers/scsi/scsi_sysfs.o":
		footprint.sourceInputs = []string{"include/scsi/scsi_devinfo.h"}
		footprint.providedIncludes = []string{"scsi_devinfo_tbl.c"}
	case "drivers/tty/vt/consolemap_deftbl.o":
		footprint.closureInputs = []string{"include/linux/types.h"}
	case "drivers/of/empty_root.dtb.o":
		footprint.closureInputs = []string{"include/asm-generic/vmlinux.lds.h"}
	case "lib/crc/crc32-main.o":
		footprint.providedIncludes = []string{"crc32table.h"}
	case "lib/crc32.o":
		footprint.providedIncludes = []string{"crc32table.h"}
		footprint.configSymbols = []string{
			"CONFIG_CRC32_BIT",
			"CONFIG_CRC32_SARWATE",
			"CONFIG_CRC32_SLICEBY4",
		}
	case "lib/crc/crc64-main.o", "lib/crc64.o":
		footprint.providedIncludes = []string{"crc64table.h"}
	case "lib/oid_registry.o":
		footprint.sourceInputs = []string{"include/linux/oid_registry.h"}
		footprint.providedIncludes = []string{"oid_registry_data.c"}
	case "arch/x86/lib/inat.o":
		footprint.sourceInputs = []string{
			"arch/x86/lib/x86-opcode-map.txt",
			"arch/x86/include/asm/inat.h",
		}
		footprint.providedIncludes = []string{"inat-tables.c"}
	case "usr/initramfs_data.o":
		footprint.sourceInputs = []string{"usr/default_cpio_list"}
		footprint.providedIncludes = []string{"usr/initramfs_inc_data"}
	case "arch/x86/kernel/cpu/capflags.o":
		footprint.closureInputs = []string{
			"arch/x86/include/asm/cpufeatures.h",
			"arch/x86/include/asm/vmxfeatures.h",
		}
	case "arch/x86/purgatory/kexec-purgatory.o":
		footprint.providedIncludes = []string{
			"arch/x86/purgatory/purgatory.ro",
		}
	case "arch/x86/realmode/rmpiggy.o":
		footprint.providedIncludes = []string{
			"arch/x86/realmode/rm/realmode.bin",
			"arch/x86/realmode/rm/realmode.relocs",
			"pasyms.h",
		}
	case "arch/arm64/kernel/vdso-wrap.o":
		footprint.providedIncludes = []string{
			"arch/arm64/kernel/vdso/vdso.so",
		}
	case "arch/arm64/kernel/vdso32-wrap.o":
		footprint.providedIncludes = []string{
			"arch/arm64/kernel/vdso32/vdso.so",
		}
	case "init/version.o":
		footprint.sourceInputs = []string{"init/version-timestamp.c"}
	}
	if strings.HasPrefix(object, "lib/fdt") && strings.HasSuffix(object, ".o") {
		source := strings.TrimSuffix(filepath.Base(object), ".o") + ".c"
		footprint.sourceInputs = appendUniqueStrings(
			footprint.sourceInputs,
			"scripts/dtc/libfdt/"+source,
		)
	}
	if strings.HasSuffix(object, ".asn1.o") {
		footprint.sourceInputs = appendUniqueStrings(
			footprint.sourceInputs,
			"scripts/asn1_compiler.c",
		)
		footprint.closureInputs = appendUniqueStrings(
			footprint.closureInputs,
			"include/linux/asn1_ber_bytecode.h",
			"include/linux/asn1_decoder.h",
		)
	}
	if flagsNeedUTSVersionTmp(flags) {
		footprint.configSymbols = appendUniqueStrings(footprint.configSymbols,
			"CONFIG_LOCALVERSION",
			"CONFIG_PREEMPT_BUILD",
			"CONFIG_PREEMPT_DYNAMIC",
			"CONFIG_PREEMPT_RT",
			"CONFIG_SMP",
		)
	}
	if isMultiSourceImageObject(object) ||
		object == "arch/arm64/kernel/vdso-wrap.o" ||
		object == "arch/arm64/kernel/vdso32-wrap.o" ||
		isArm64NvheObject(object) {
		footprint.fullGeneratedHeaders = true
	}
	return footprint
}

func isMultiSourceImageObject(object string) bool {
	return strings.HasPrefix(object, "arch/x86/entry/vdso/vdso-image-") ||
		object == "arch/x86/realmode/rmpiggy.o" ||
		object == "arch/x86/purgatory/kexec-purgatory.o"
}

func flagsNeedUTSVersionTmp(flags []string) bool {
	for _, flag := range flags {
		if strings.Contains(flag, "utsversion-tmp.h") {
			return true
		}
	}
	return false
}

func compactSpecialSourcesForObject(object string) compactSpecialSourceInputs {
	compiled := func(paths ...string) []compactSpecialSourceInput {
		out := make([]compactSpecialSourceInput, 0, len(paths))
		for _, path := range paths {
			out = append(out, compactSpecialSourceInput{path: path, compiled: true})
		}
		return out
	}
	switch object {
	case "arch/x86/entry/vdso/vdso-image-64.o":
		inputs := compiled(
			"arch/x86/entry/vdso/vdso-note.S",
			"arch/x86/entry/vdso/vclock_gettime.c",
			"arch/x86/entry/vdso/vgetcpu.c",
			"arch/x86/entry/vdso/vgetrandom.c",
			"arch/x86/entry/vdso/vgetrandom-chacha.S",
			"arch/x86/entry/vdso/vdso.lds.S",
		)
		inputs = append(inputs, compactSpecialSourceInput{
			path: "arch/x86/include/asm/vdso.h",
		})
		return compactSpecialSourceInputs{
			primary:      "arch/x86/entry/vdso/vdso-note.S",
			includeRoots: []string{"arch/x86/entry/vdso"},
			inputs:       inputs,
		}
	case "arch/x86/realmode/rmpiggy.o":
		return compactSpecialSourceInputs{
			includeRoots: []string{
				"arch/x86/boot",
				"arch/x86/realmode/rm",
			},
			inputs: compiled(
				"arch/x86/realmode/rm/header.S",
				"arch/x86/realmode/rm/trampoline_64.S",
				"arch/x86/realmode/rm/stack.S",
				"arch/x86/realmode/rm/reboot.S",
				"arch/x86/realmode/rm/wakeup_asm.S",
				"arch/x86/realmode/rm/wakemain.c",
				"arch/x86/realmode/rm/video-mode.c",
				"arch/x86/realmode/rm/copy.S",
				"arch/x86/realmode/rm/bioscall.S",
				"arch/x86/realmode/rm/regs.c",
				"arch/x86/realmode/rm/video-vga.c",
				"arch/x86/realmode/rm/video-vesa.c",
				"arch/x86/realmode/rm/video-bios.c",
				"arch/x86/realmode/rm/realmode.lds.S",
			),
		}
	case "arch/x86/purgatory/kexec-purgatory.o":
		return compactSpecialSourceInputs{
			inputs: compiled(
				"arch/x86/purgatory/purgatory.c",
				"arch/x86/purgatory/stack.S",
				"arch/x86/purgatory/setup-x86_64.S",
				"arch/x86/purgatory/entry64.S",
				"arch/x86/boot/compressed/string.c",
				"lib/crypto/sha256.c",
			),
		}
	case "arch/arm64/kernel/vdso32-wrap.o":
		profile := func(path string, compiled bool) compactSpecialSourceInput {
			return compactSpecialSourceInput{
				path:     path,
				compiled: compiled,
				profile:  sourceScanArm32CompatVDSO,
			}
		}
		return compactSpecialSourceInputs{
			includeRoots: []string{
				"arch/arm64/kernel/vdso32",
				"lib/vdso",
			},
			inputs: []compactSpecialSourceInput{
				profile("arch/arm64/kernel/vdso32/note.c", true),
				profile("arch/arm64/kernel/vdso32/vgettimeofday.c", true),
				profile("arch/arm64/kernel/vdso32/vdso.lds.S", true),
				profile("lib/vdso/gettimeofday.c", false),
			},
		}
	default:
		return compactSpecialSourceInputs{}
	}
}

func isArm64NvheObject(object string) bool {
	return object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"
}

func objectNeedsFullConfig(object string) bool {
	return object == "arch/x86/purgatory/kexec-purgatory.o"
}

func includeDirsFromFlags(flags []string, source string) ([]string, error) {
	var dirs []string
	pathFlags, err := sourcePathFlags(flags)
	if err != nil {
		return nil, err
	}
	for _, flag := range pathFlags {
		if flag.option == "-include" || flag.option == "-imacros" {
			continue
		}
		class, path, err := resolveSourceFlagPath(flag.path, source)
		if err != nil {
			return nil, fmt.Errorf("%s path %q: %w", flag.option, flag.path, err)
		}
		if class == sourceFlagPathTree {
			dirs = appendUnique(dirs, path)
		}
	}
	return dirs, nil
}

func defaultForcedSourceInputs(source string) []string {
	out := []string{
		"include/linux/compiler-version.h",
		"include/linux/kconfig.h",
	}
	if !strings.HasSuffix(source, ".S") && !strings.HasSuffix(source, ".s") {
		out = append(out, "include/linux/compiler_types.h")
	}
	sort.Strings(out)
	return out
}

func forcedSourceInputs(flags []string, source, object string) ([]string, error) {
	out := defaultForcedSourceInputs(source)
	pathFlags, err := sourcePathFlags(flags)
	if err != nil {
		return nil, err
	}
	for _, flag := range pathFlags {
		if flag.option != "-include" && flag.option != "-imacros" {
			continue
		}
		class, path, err := resolveSourceFlagPath(flag.path, source)
		if err != nil {
			return nil, fmt.Errorf("%s path %q: %w", flag.option, flag.path, err)
		}
		if isGeneratedUTSVersionForcedInput(path, object) {
			continue
		}
		switch class {
		case sourceFlagPathTree:
			if path == "" {
				return nil, fmt.Errorf("%s path %q does not name a source-tree file", flag.option, flag.path)
			}
			out = appendUnique(out, path)
		case sourceFlagPathObject:
			if filepath.Base(path) != "utsversion-tmp.h" {
				return nil, fmt.Errorf(
					"%s path %q names an unmodeled generated forced include",
					flag.option,
					flag.path,
				)
			}
		case sourceFlagPathExternal:
			return nil, fmt.Errorf(
				"%s path %q is outside the source tree",
				flag.option,
				flag.path,
			)
		}
	}
	sort.Strings(out)
	return out, nil
}

func isGeneratedUTSVersionForcedInput(path, object string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "utsversion-tmp.h" {
		return true
	}
	objectDir := filepath.ToSlash(filepath.Dir(object))
	return objectDir != "." && path == objectDir+"/utsversion-tmp.h"
}

func (o resolvedKbuildObject) variant(
	config *ResolvedConfig,
	source string,
	sourceInputs []CompactSourceInput,
	members []string,
	deps []string,
	memberContentIDs []string,
	depContentIDs []string,
	sourceRoot string,
	sourceRefs []string,
	compileEnvironmentABI string,
	generatedHeaderFamilyIDs []string,
) CompactObjectVariant {
	fragment := map[string]string{}
	refset := make(map[string]bool, len(o.footprint)+len(sourceRefs))
	if len(members) == 0 {
		for ref := range o.footprint {
			refset[ref] = true
		}
	}
	for _, ref := range sourceRefs {
		refset[ref] = true
	}
	if source != "" || isArm64NvheObject(o.object) {
		for _, ref := range KernelFlagsConfigSymbols() {
			refset[ref] = true
		}
	}
	if objectNeedsFullConfig(o.object) {
		for key, written := range config.Written {
			if written {
				refset[key] = true
			}
		}
	}
	refs := make([]string, 0, len(refset))
	for ref := range refset {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if config.ShouldWrite(ref) {
			fragment[ref] = config.Value(ref)
		} else {
			fragment[ref] = "n"
		}
	}
	flags := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.flags, source), sourceRoot)
	remove := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.remove, source), sourceRoot)
	modname := o.modname
	moduleRoot := o.root && o.mode == "m"
	compileComposite := isArm64NvheObject(o.object)
	if len(members) != 0 {
		modname = ""
		flags = nil
		remove = nil
		deps = nil
		depContentIDs = nil
		if !compileComposite {
			sourceInputs = nil
		}
	}
	contentID := ""
	compileEnvironmentID := ""
	usesCompileEnvironment := len(members) == 0 || compileComposite
	if usesCompileEnvironment {
		payload := newCompactConfigPayload(fragment)
		environment := newCompactCompileEnvironment(
			compileEnvironmentABI,
			payload.ID,
			generatedHeaderFamilyIDs,
		)
		compileEnvironmentID = environment.ID
	} else {
		fragment = map[string]string{}
	}
	contentID = objectVariantContentID(
		o.object,
		o.mode,
		modname,
		flags,
		remove,
		compileEnvironmentID,
		source,
		sourceInputs,
		depContentIDs,
		memberContentIDs,
		compileEnvironmentABI,
		moduleRoot,
		o.objtoolDisabled,
		o.objtoolForce,
		o.objtoolArgs,
	)
	return CompactObjectVariant{
		Target:             sanitizeTargetName(strings.TrimSuffix(o.object, ".o")) + "__" + compactShortID(contentID),
		ContentID:          contentID,
		CompileEnvironment: compileEnvironmentID,
		Object:             o.object,
		Source:             source,
		sourceInputs:       append([]CompactSourceInput(nil), sourceInputs...),
		Mode:               o.mode,
		ModuleRoot:         moduleRoot,
		ModName:            modname,
		Flags:              flags,
		RemoveFlags:        remove,
		ObjtoolArgs:        append([]string(nil), o.objtoolArgs...),
		ObjtoolDisabled:    o.objtoolDisabled,
		ObjtoolForce:       o.objtoolForce,
		configFragment:     fragment,
		Deps:               append([]string(nil), deps...),
		Members:            append([]string(nil), members...),
		generatedHeaderFamilyIDs: append(
			[]string(nil),
			generatedHeaderFamilyIDs...,
		),
	}
}

func normalizeSourceRootFlags(flags []string, sourceRoot string) []string {
	if sourceRoot == "" {
		return flags
	}
	sourceRoot = normalizeCompactPath(sourceRoot)
	if sourceRoot == "." || sourceRoot == "/" {
		return flags
	}
	out := make([]string, len(flags))
	changed := false
	for i, flag := range flags {
		normalized := normalizeSourceRootFlag(flag, sourceRoot)
		if normalized != flag {
			changed = true
		}
		out[i] = normalized
	}
	if !changed {
		return flags
	}
	return out
}

func normalizeSourceRootFlag(flag, sourceRoot string) string {
	for _, prefix := range []string{"-I", "-iquote", "-isystem", "-include"} {
		path := strings.TrimPrefix(flag, prefix)
		if path == flag {
			continue
		}
		if path == "" {
			return flag
		}
		path = normalizeCompactPath(path)
		if path == sourceRoot {
			return prefix + "$(srctree)"
		}
		if strings.HasPrefix(path, sourceRoot+"/") {
			return prefix + "$(srctree)/" + strings.TrimPrefix(path, sourceRoot+"/")
		}
		return prefix + path
	}
	flag = normalizeCompactPath(flag)
	if flag == sourceRoot {
		return "$(srctree)"
	}
	if strings.HasPrefix(flag, sourceRoot+"/") {
		return "$(srctree)/" + strings.TrimPrefix(flag, sourceRoot+"/")
	}
	return flag
}

func normalizeCompactPath(path string) string {
	return filepath.ToSlash(filepath.Clean(strings.ReplaceAll(path, `\`, "/")))
}

func filterResolvedKbuildFlags(groups []resolvedKbuildFlag, source string) []string {
	out := []string{}
	for _, group := range groups {
		if !kbuildFlagLanguageMatchesSource(group.language, source) {
			continue
		}
		out = append(out, group.values...)
	}
	return out
}

func kbuildFlagLanguageMatchesSource(language string, source string) bool {
	if language == "" || language == "any" || source == "" {
		return true
	}
	switch filepath.Ext(source) {
	case ".c":
		return language == "c"
	case ".S", ".s":
		return language == "asm"
	default:
		return true
	}
}

func sourceForObject(sourceRoot, objectDir, object string, sourceRoots map[string]string) string {
	if !strings.HasSuffix(object, ".o") {
		return ""
	}
	for _, mapped := range sourceCandidatesForObject(object) {
		candidate := filepath.ToSlash(filepath.Join(objectDir, mapped))
		if sourceRoot != "" && fileExists(filepath.Join(sourceRoot, filepath.FromSlash(candidate))) {
			return candidate
		}
		if mappedPath, ok := mappedSourceRootPath(candidate, sourceRoots); ok && fileExists(mappedPath) {
			return candidate
		}
	}
	stem := strings.TrimSuffix(object, ".o")
	for _, ext := range []string{".c", ".S", ".s"} {
		candidate := filepath.ToSlash(filepath.Join(objectDir, stem+ext))
		if sourceRoot != "" && fileExists(filepath.Join(sourceRoot, filepath.FromSlash(candidate))) {
			return candidate
		}
		if mappedPath, ok := mappedSourceRootPath(candidate, sourceRoots); ok && fileExists(mappedPath) {
			return candidate
		}
	}
	return ""
}

func sourceCandidatesForObject(object string) []string {
	stem := strings.TrimSuffix(object, ".o")
	var out []string
	if base, ok := strings.CutSuffix(stem, ".pi"); ok {
		for _, ext := range []string{".c", ".S", ".s"} {
			out = append(out, base+ext)
		}
		if dir, file := filepath.Split(base); strings.HasPrefix(file, "lib-") {
			for _, ext := range []string{".c", ".S", ".s"} {
				out = append(out, filepath.ToSlash(filepath.Join("lib", strings.TrimPrefix(file, "lib-")+ext)))
				out = append(out, filepath.ToSlash(filepath.Join(dir, strings.TrimPrefix(file, "lib-")+ext)))
			}
		}
	}
	if base, ok := strings.CutSuffix(stem, ".nvhe"); ok {
		for _, ext := range []string{".c", ".S", ".s"} {
			out = append(out, base+ext)
		}
	}
	if base, ok := strings.CutSuffix(stem, ".stub"); ok {
		for _, ext := range []string{".c", ".S", ".s"} {
			out = append(out, base+ext)
		}
		if dir, file := filepath.Split(base); strings.HasPrefix(file, "lib-") {
			for _, ext := range []string{".c", ".S", ".s"} {
				out = append(out, filepath.ToSlash(filepath.Join("lib", strings.TrimPrefix(file, "lib-")+ext)))
				out = append(out, filepath.ToSlash(filepath.Join(dir, strings.TrimPrefix(file, "lib-")+ext)))
			}
		}
	}
	if base, ok := strings.CutSuffix(stem, ".asn1"); ok {
		out = append(out, base+".asn1")
	}
	if base, ok := strings.CutSuffix(stem, ".dtb"); ok {
		out = append(out, base+".dts")
	}
	if base, ok := strings.CutSuffix(stem, ".dtbo"); ok {
		out = append(out, base+".dtso")
	}
	switch object {
	case "arch/x86/entry/vdso/vdso-image-64.o":
		out = append(out, "arch/x86/entry/vdso/vdso2c.c")
	case "arch/x86/kernel/cpu/capflags.o":
		// linux_object still carries this nominal src as a direct action input;
		// the exact producer headers are added by compactObjectActionFootprint.
		out = append(out, "arch/x86/kernel/cpu/mkcapflags.sh")
	case "drivers/tty/vt/consolemap_deftbl.o":
		out = append(out, "drivers/tty/vt/cp437.uni")
	case "drivers/tty/vt/defkeymap.o":
		out = append(out, "drivers/tty/vt/defkeymap.c_shipped")
	case "fs/unicode/utf8data.o":
		out = append(out, "fs/unicode/utf8data.c_shipped")
	case "lib/crypto/arm64/poly1305-core.o":
		out = append(out, "lib/crypto/arm64/poly1305-armv8.pl")
	case "lib/crypto/arm64/sha256-core.o", "lib/crypto/arm64/sha512-core.o":
		out = append(out, "lib/crypto/arm64/sha2-armv8.pl")
	case "lib/crypto/x86/poly1305-x86_64-cryptogams.o":
		out = append(out, "lib/crypto/x86/poly1305-x86_64-cryptogams.pl")
	}
	if dir, file := filepath.Split(stem); strings.HasPrefix(file, "lib-") {
		for _, ext := range []string{".c", ".S", ".s"} {
			out = append(out, filepath.ToSlash(filepath.Join("lib", strings.TrimPrefix(file, "lib-")+ext)))
			out = append(out, filepath.ToSlash(filepath.Join(dir, strings.TrimPrefix(file, "lib-")+ext)))
		}
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

type asn1HeaderDep struct {
	include string
	object  string
}

func asn1HeaderDepsForSource(sourceRoot, source string) []asn1HeaderDep {
	if sourceRoot == "" || source == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(source)))
	if err != nil {
		return nil
	}
	sourceDir := filepath.ToSlash(filepath.Dir(source))
	if sourceDir == "." {
		sourceDir = ""
	}
	seen := map[string]bool{}
	var out []asn1HeaderDep
	for _, line := range strings.Split(string(data), "\n") {
		include, ok := quotedInclude(line)
		if !ok || !strings.HasSuffix(include, ".asn1.h") {
			continue
		}
		object := strings.TrimSuffix(include, ".h") + ".o"
		object = filepath.ToSlash(filepath.Join(sourceDir, object))
		if seen[object] {
			continue
		}
		seen[object] = true
		out = append(out, asn1HeaderDep{
			include: filepath.ToSlash(include),
			object:  object,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].object == out[j].object {
			return out[i].include < out[j].include
		}
		return out[i].object < out[j].object
	})
	return out
}

func quotedInclude(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	if !strings.HasPrefix(line, "include") {
		return "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "include"))
	if !strings.HasPrefix(line, "\"") {
		return "", false
	}
	line = strings.TrimPrefix(line, "\"")
	end := strings.IndexByte(line, '"')
	if end < 0 {
		return "", false
	}
	return line[:end], true
}

var compactGroupedSpecialObjects = map[string]bool{
	"arch/arm64/kernel/vdso-wrap.o":               true,
	"arch/arm64/kernel/vdso32-wrap.o":             true,
	"arch/x86/entry/vdso/vdso-image-64.o":         true,
	"arch/x86/kernel/cpu/capflags.o":              true,
	"arch/x86/lib/inat.o":                         true,
	"arch/x86/purgatory/kexec-purgatory.o":        true,
	"arch/x86/realmode/rmpiggy.o":                 true,
	"certs/blacklist_hashes.o":                    true,
	"certs/revocation_certificates.o":             true,
	"certs/system_certificates.o":                 true,
	"drivers/of/empty_root.dtb.o":                 true,
	"drivers/scsi/scsi_sysfs.o":                   true,
	"drivers/tty/vt/consolemap_deftbl.o":          true,
	"drivers/tty/vt/ucs.o":                        true,
	"lib/crc/crc32-main.o":                        true,
	"lib/crc/crc64-main.o":                        true,
	"lib/crc32.o":                                 true,
	"lib/crc64.o":                                 true,
	"lib/crypto/arm64/poly1305-core.o":            true,
	"lib/crypto/arm64/sha256-core.o":              true,
	"lib/crypto/arm64/sha512-core.o":              true,
	"lib/crypto/x86/poly1305-x86_64-cryptogams.o": true,
	"lib/oid_registry.o":                          true,
	"usr/initramfs_data.o":                        true,
}

type compactActionGroupEmission struct {
	Group          CompactActionGroup
	GroupedName    string
	GroupedTargets []string
	LegacyName     string
	LegacyTargets  []string
}

type compactActionGraphPlan struct {
	Emissions         []compactActionGroupEmission
	GroupNameByTarget map[string]string
	LegacyTargets     map[string]bool
	Variants          map[string]CompactObjectVariant
}

func (m *CompactMetadata) groupedCompileFallbackReason(variant CompactObjectVariant) string {
	if len(variant.Deps) != 0 {
		return "has generated-header object dependencies"
	}
	if reason := variant.sourceBuildError(); reason != "" {
		return reason
	}
	if len(variant.RemoveFlags) != 0 {
		return "has Kbuild remove flags"
	}
	if compactGroupedSpecialObjects[variant.Object] {
		return "requires generated-object actions"
	}
	if strings.HasSuffix(variant.Object, ".asn1.o") ||
		strings.HasSuffix(variant.Object, ".pi.o") ||
		strings.HasSuffix(variant.Object, ".stub.o") {
		return "requires generated-source or post-compile processing"
	}
	if strings.HasSuffix(variant.Source, ".c_shipped") {
		return "requires source materialization"
	}
	language := compactSourceLanguage(variant.Source)
	if language == "" {
		return "has unsupported primary source"
	}
	for _, flag := range variant.Flags {
		if strings.Contains(flag, "$(obj)") ||
			strings.Contains(flag, "${obj}") ||
			strings.Contains(flag, "utsversion-tmp.h") {
			return "requires an object-local generated directory"
		}
	}
	if variant.Mode == "m" && language == "c" &&
		(variant.ModuleRoot || variant.ObjtoolForce) &&
		(variant.configFragment["CONFIG_LTO_CLANG"] == "y" ||
			variant.configFragment["CONFIG_LTO_CLANG_FULL"] == "y" ||
			variant.configFragment["CONFIG_LTO_CLANG_THIN"] == "y") {
		return "requires the module LTO relocatable-link stage"
	}

	inputs, err := m.expandedSourceInputGroup(
		variant.SourceInputGroup,
		fmt.Sprintf("object target %q", variant.Target),
	)
	if err != nil {
		return err.Error()
	}
	paths := map[string]bool{}
	for _, input := range inputs {
		paths[input.Path] = true
	}
	required := []string{
		"include/linux/compiler-version.h",
		"include/linux/kconfig.h",
	}
	if language == "c" {
		required = append(required, "include/linux/compiler_types.h")
	}
	for _, path := range required {
		if !paths[path] {
			return "exact source set omits required preinclude " + path
		}
	}
	return ""
}

func (m *CompactMetadata) actionGraphPlan() (*compactActionGraphPlan, error) {
	if err := m.ensureActionGroups(); err != nil {
		return nil, err
	}
	variants := make(map[string]CompactObjectVariant, len(m.ObjectVariants))
	for _, variant := range m.ObjectVariants {
		variants[variant.Target] = variant
	}

	legacy := map[string]bool{}
	active := map[string]bool{}
	for _, group := range m.ActionGroups {
		for _, target := range group.ObjectTargets {
			active[target] = true
		}
	}
	for _, variant := range m.ObjectVariants {
		if !active[variant.Target] {
			continue
		}
		switch compactActionKind(variant) {
		case "compile":
			if m.groupedCompileFallbackReason(variant) != "" {
				legacy[variant.Target] = true
			}
		case "arm64_nvhe":
			legacy[variant.Target] = true
		}
	}

	// Legacy rules consume ordinary labels. Keep each legacy island closed over
	// dependencies and members so no action is duplicated to bridge providers.
	for changed := true; changed; {
		changed = false
		for target := range legacy {
			variant := variants[target]
			children := append(append([]string(nil), variant.Deps...), variant.Members...)
			for _, child := range children {
				if !legacy[child] {
					legacy[child] = true
					changed = true
				}
			}
		}
	}

	plan := &compactActionGraphPlan{
		GroupNameByTarget: map[string]string{},
		LegacyTargets:     legacy,
		Variants:          variants,
	}
	groupIdentitiesByName := map[string]string{}
	for _, group := range m.ActionGroups {
		emission := compactActionGroupEmission{Group: group}
		for _, target := range group.ObjectTargets {
			if legacy[target] {
				emission.LegacyTargets = append(emission.LegacyTargets, target)
			} else {
				emission.GroupedTargets = append(emission.GroupedTargets, target)
			}
		}
		reachabilityID := compactReachabilityID(group.ReachableConfigs)
		baseName := compactActionGroupRuleName(group)
		identity := group.RecipeID + "\x00" + reachabilityID
		if existing := groupIdentitiesByName[baseName]; existing != "" && existing != identity {
			return nil, fmt.Errorf("compact action-group identities produce duplicate rule name %q", baseName)
		}
		groupIdentitiesByName[baseName] = identity
		if len(emission.GroupedTargets) != 0 {
			emission.GroupedName = baseName
			for _, target := range emission.GroupedTargets {
				plan.GroupNameByTarget[target] = emission.GroupedName
			}
		}
		if len(emission.LegacyTargets) != 0 {
			emission.LegacyName = baseName + "_legacy"
			for _, target := range emission.LegacyTargets {
				plan.GroupNameByTarget[target] = emission.LegacyName
			}
		}
		plan.Emissions = append(plan.Emissions, emission)
	}
	return plan, nil
}

func (m *CompactMetadata) BuildFile(opts CompactBuildFileOptions) ([]byte, error) {
	objectBuild, err := m.objectBuildFile(opts)
	if err != nil {
		return nil, err
	}
	imageBuild, err := m.imageBuildFile(opts)
	if err != nil {
		return nil, err
	}
	return mergeBuildFiles("compact.BUILD.bazel", opts.Exports, objectBuild, imageBuild)
}

func (m *CompactMetadata) objectBuildFile(opts CompactBuildFileOptions) ([]byte, error) {
	if err := m.validateContentIDs(); err != nil {
		return nil, err
	}
	plan, err := m.actionGraphPlan()
	if err != nil {
		return nil, err
	}
	if opts.SourceLabelPackage == "" {
		return nil, fmt.Errorf("compact object BUILD emission requires a source label package")
	}
	if opts.SourceRootLabel == "" {
		return nil, fmt.Errorf("compact object BUILD emission requires a source root label")
	}
	visibility := opts.Visibility
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}
	file := buildgen.NewBuildFile("compact_objects.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	loadsByName := map[string]bool{
		"linux_compile_environment_index": true,
		"linux_source_input_index":        true,
		"linux_source_tree":               true,
	}
	groupLoadsByName := map[string]bool{}
	for _, variant := range m.ObjectVariants {
		if !plan.LegacyTargets[variant.Target] {
			continue
		}
		switch compactActionKind(variant) {
		case "compile":
			loadsByName["linux_object"] = true
		case "arm64_nvhe":
			loadsByName["linux_arm64_nvhe_object"] = true
		case "composite":
			loadsByName["linux_composite_object"] = true
		}
	}
	for _, emission := range plan.Emissions {
		if emission.GroupedName != "" {
			first := plan.Variants[emission.GroupedTargets[0]]
			switch compactActionKind(first) {
			case "compile":
				groupLoadsByName["linux_object_action_group"] = true
			case "composite":
				groupLoadsByName["linux_composite_object_action_group"] = true
			}
		}
		if emission.LegacyName != "" {
			groupLoadsByName["linux_object_action_group_import"] = true
		}
	}
	loads := make([]string, 0, len(loadsByName))
	for name := range loadsByName {
		loads = append(loads, name)
	}
	sort.Strings(loads)
	groupLoads := make([]string, 0, len(groupLoadsByName))
	for name := range groupLoadsByName {
		groupLoads = append(groupLoads, name)
	}
	sort.Strings(groupLoads)
	compileEnvironmentIndexTarget := "_compile_environment_index"
	sourceInputIndexTarget := "_source_input_index"
	sourceTreeTarget := "_source_tree"
	file.AddLoad(compactRuleLoadLabel(opts.RuleLoadLabel), loads...)
	if len(groupLoads) != 0 {
		file.AddLoad(compactObjectGroupRuleLoadLabel(opts.RuleLoadLabel), groupLoads...)
	}
	file.AddPackage(visibility)
	referencedPayloads := make(map[string]bool, len(m.CompileEnvironments))
	expectedABI := ""
	for _, environment := range m.CompileEnvironments {
		referencedPayloads[environment.ConfigPayload] = true
		if expectedABI == "" {
			expectedABI = environment.ABI
		} else if expectedABI != environment.ABI {
			return nil, fmt.Errorf(
				"compile environments use ABIs %q and %q",
				expectedABI,
				environment.ABI,
			)
		}
	}
	if expectedABI == "" {
		return nil, fmt.Errorf("compact compile environment index has no ABI")
	}
	configPayloads := make(map[string]string, len(referencedPayloads))
	for _, payload := range m.ConfigPayloads {
		if referencedPayloads[payload.ID] {
			configPayloads[payload.ID] = payload.Content
		}
	}
	compileEnvironments := make(map[string]string, len(m.CompileEnvironments))
	for _, environment := range m.CompileEnvironments {
		compileEnvironments[environment.ID] = compactCompileEnvironmentValue(environment)
	}
	generatedHeaders := []string{}
	seenGeneratedHeaders := map[string]bool{}
	for _, family := range m.GeneratedHeaderFamilies {
		labels := append([]string(nil), family.Labels...)
		sort.Strings(labels)
		if len(labels) == 0 {
			continue
		}
		label := labels[0]
		if seenGeneratedHeaders[label] {
			continue
		}
		seenGeneratedHeaders[label] = true
		generatedHeaders = append(generatedHeaders, label)
	}
	sort.Strings(generatedHeaders)
	r := file.AddRule("linux_compile_environment_index", compileEnvironmentIndexTarget)
	r.SetAttr("config_payloads", configPayloads)
	r.SetAttr("compile_environments", compileEnvironments)
	r.SetAttr("expected_abi", expectedABI)
	if len(generatedHeaders) != 0 {
		r.SetAttr("generated_headers", generatedHeaders)
	}
	if opts.Arch != "" {
		r.SetAttr("arch", opts.Arch)
	}
	if opts.Version != "" {
		r.SetAttr("version", opts.Version)
	}
	r.SetAttr("tags", []string{"manual"})

	sourceLabels := make([]string, 0, len(m.SourceFiles))
	for _, input := range m.SourceFiles {
		sourceLabels = append(sourceLabels, labelFor(opts.SourceLabelPackage, input.Path))
	}
	r = file.AddRule("linux_source_input_index", sourceInputIndexTarget)
	r.SetAttr("groups", m.SourceInputGroups)
	r.SetAttr("srcs", sourceLabels)
	r.SetAttr("source_tree_info", ":"+sourceTreeTarget)
	r.SetAttr("tags", []string{"manual"})

	r = file.AddRule("linux_source_tree", sourceTreeTarget)
	r.SetAttr("root", opts.SourceRootLabel)
	r.SetAttr("tags", []string{"manual"})

	for _, variant := range m.ObjectVariants {
		if !plan.LegacyTargets[variant.Target] {
			continue
		}
		if len(variant.Members) != 0 {
			if variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o" {
				r := file.AddRule("linux_arm64_nvhe_object", variant.Target)
				r.SetAttr("object", variant.Object)
				r.SetAttr("mode", variant.Mode)
				r.SetAttr("tags", []string{"manual"})
				r.SetAttr("objects", localLabels(variant.Members))
				r.SetAttr("content_id", variant.ContentID)
				if variant.CompileEnvironment == "" {
					return nil, fmt.Errorf("arm64 nVHE object %q has no compile environment", variant.Object)
				}
				r.SetAttr("compile_environment_index", ":"+compileEnvironmentIndexTarget)
				r.SetAttr("compile_environment_id", variant.CompileEnvironment)
				r.SetAttr("source_input_index", ":"+sourceInputIndexTarget)
				r.SetAttr("source_input_group", variant.SourceInputGroup)
				if opts.Arch != "" {
					r.SetAttr("arch", opts.Arch)
				}
				if opts.Srcarch != "" {
					r.SetAttr("srcarch", opts.Srcarch)
				}
				continue
			}
			r := file.AddRule("linux_composite_object", variant.Target)
			r.SetAttr("object", variant.Object)
			r.SetAttr("mode", variant.Mode)
			r.SetAttr("tags", []string{"manual"})
			r.SetAttr("objects", localLabels(variant.Members))
			r.SetAttr("content_id", variant.ContentID)
			if variant.ModuleRoot {
				r.SetAttr("module_root", true)
			}
			if variant.ObjtoolForce {
				r.SetAttr("objtool_force", true)
			}
			if len(variant.ObjtoolArgs) != 0 {
				r.SetAttr("objtool_args", variant.ObjtoolArgs)
			}
			if opts.Arch != "" {
				r.SetAttr("arch", opts.Arch)
			}
			continue
		}
		if reason := variant.sourceBuildError(); reason != "" {
			return nil, fmt.Errorf(
				"cannot emit source-backed linux_object %q for %q: %s",
				variant.Target,
				variant.Object,
				reason,
			)
		}
		r := file.AddRule("linux_object", variant.Target)
		r.SetAttr("object", variant.Object)
		r.SetAttr("mode", variant.Mode)
		r.SetAttr("tags", []string{"manual"})
		r.SetAttr("content_id", variant.ContentID)
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		sourceFile, err := m.sourceFileIndex(variant.Source)
		if err != nil {
			return nil, fmt.Errorf("source-backed object %q: %w", variant.Object, err)
		}
		r.SetAttr("source_input_file", sourceFile)
		r.SetAttr("source_input_group", variant.SourceInputGroup)
		r.SetAttr("source_input_index", ":"+sourceInputIndexTarget)
		if variant.CompileEnvironment == "" {
			return nil, fmt.Errorf("source-backed object %q has no compile environment", variant.Object)
		}
		r.SetAttr("compile_environment_index", ":"+compileEnvironmentIndexTarget)
		r.SetAttr("compile_environment_id", variant.CompileEnvironment)
		if opts.Srcarch != "" {
			r.SetAttr("srcarch", opts.Srcarch)
		}
		if opts.SourceASN1Compiler != "" && strings.HasSuffix(variant.Object, ".asn1.o") {
			r.SetAttr("asn1_compiler", opts.SourceASN1Compiler)
		}
		if opts.SourceRelacheck != "" && strings.HasSuffix(variant.Object, ".pi.o") {
			r.SetAttr("relacheck", opts.SourceRelacheck)
		}
		if opts.Arch == "x86" && opts.SourceObjtool != "" && !variant.ObjtoolDisabled {
			r.SetAttr("objtool", opts.SourceObjtool)
			if variant.ObjtoolForce {
				r.SetAttr("objtool_force", true)
			}
			if len(variant.ObjtoolArgs) != 0 {
				r.SetAttr("objtool_args", variant.ObjtoolArgs)
			}
		}
		if variant.ModuleRoot {
			r.SetAttr("module_root", true)
		}
		if len(variant.Flags) != 0 {
			r.SetAttr("flags", variant.Flags)
		}
		if len(variant.RemoveFlags) != 0 {
			r.SetAttr("remove_flags", variant.RemoveFlags)
		}
		if variant.ModName != "" {
			r.SetAttr("modname", variant.ModName)
		}
		if len(variant.Deps) != 0 {
			r.SetAttr("deps", localLabels(variant.Deps))
		}
	}

	for _, emission := range plan.Emissions {
		if emission.GroupedName != "" {
			first := plan.Variants[emission.GroupedTargets[0]]
			reachabilityID := compactReachabilityID(emission.Group.ReachableConfigs)
			switch compactActionKind(first) {
			case "compile":
				objects := map[string]string{}
				for _, target := range emission.GroupedTargets {
					variant := plan.Variants[target]
					sourceFile, err := m.sourceFileIndex(variant.Source)
					if err != nil {
						return nil, fmt.Errorf("grouped source-backed object %q: %w", variant.Object, err)
					}
					encoded, err := json.Marshal(map[string]any{
						"compile_environment": variant.CompileEnvironment,
						"content_id":          variant.ContentID,
						"object":              variant.Object,
						"source_input_file":   sourceFile,
						"source_input_group":  variant.SourceInputGroup,
					})
					if err != nil {
						return nil, fmt.Errorf("encode grouped object %q: %w", variant.Object, err)
					}
					objects[target] = string(encoded)
				}
				r := file.AddRule("linux_object_action_group", emission.GroupedName)
				r.SetAttr("objects", objects)
				r.SetAttr("mode", first.Mode)
				r.SetAttr("language", compactSourceLanguage(first.Source))
				r.SetAttr("flags", first.Flags)
				r.SetAttr("recipe_id", emission.Group.RecipeID)
				r.SetAttr("reachability_id", reachabilityID)
				r.SetAttr("reachable_configs", emission.Group.ReachableConfigs)
				r.SetAttr("compile_environment_index", ":"+compileEnvironmentIndexTarget)
				r.SetAttr("source_input_index", ":"+sourceInputIndexTarget)
				r.SetAttr("tags", []string{"manual"})
				if opts.Arch != "" {
					r.SetAttr("arch", opts.Arch)
				}
				if opts.Srcarch != "" {
					r.SetAttr("srcarch", opts.Srcarch)
				}
				if first.ModuleRoot {
					r.SetAttr("module_root", true)
				}
				if first.ModName != "" {
					r.SetAttr("modname", first.ModName)
				}
				if first.ObjtoolDisabled {
					r.SetAttr("objtool_disabled", true)
				}
				if first.ObjtoolForce {
					r.SetAttr("objtool_force", true)
				}
				if len(first.ObjtoolArgs) != 0 {
					r.SetAttr("objtool_args", first.ObjtoolArgs)
				}
				if opts.Arch == "x86" && opts.SourceObjtool != "" && !first.ObjtoolDisabled {
					r.SetAttr("objtool", opts.SourceObjtool)
				}
			case "composite":
				objects := map[string]string{}
				memberGroups := map[string]bool{}
				for _, target := range emission.GroupedTargets {
					variant := plan.Variants[target]
					encoded, err := json.Marshal(map[string]any{
						"content_id": variant.ContentID,
						"members":    variant.Members,
						"object":     variant.Object,
					})
					if err != nil {
						return nil, fmt.Errorf("encode grouped composite %q: %w", variant.Object, err)
					}
					objects[target] = string(encoded)
					for _, member := range variant.Members {
						groupName := plan.GroupNameByTarget[member]
						if groupName == "" {
							return nil, fmt.Errorf("grouped composite %q references unowned member %q", variant.Object, member)
						}
						if groupName != emission.GroupedName {
							memberGroups[groupName] = true
						}
					}
				}
				memberLabels := make([]string, 0, len(memberGroups))
				for name := range memberGroups {
					memberLabels = append(memberLabels, ":"+name)
				}
				sort.Strings(memberLabels)
				r := file.AddRule("linux_composite_object_action_group", emission.GroupedName)
				r.SetAttr("objects", objects)
				r.SetAttr("member_groups", memberLabels)
				r.SetAttr("mode", first.Mode)
				r.SetAttr("recipe_id", emission.Group.RecipeID)
				r.SetAttr("reachability_id", reachabilityID)
				r.SetAttr("reachable_configs", emission.Group.ReachableConfigs)
				r.SetAttr("tags", []string{"manual"})
				if opts.Arch != "" {
					r.SetAttr("arch", opts.Arch)
				}
				if first.ModuleRoot {
					r.SetAttr("module_root", true)
				}
				if first.ObjtoolForce {
					r.SetAttr("objtool_force", true)
				}
				if len(first.ObjtoolArgs) != 0 {
					r.SetAttr("objtool_args", first.ObjtoolArgs)
				}
			default:
				return nil, fmt.Errorf("unsupported grouped action kind %q", compactActionKind(first))
			}
		}
		if emission.LegacyName != "" {
			first := plan.Variants[emission.LegacyTargets[0]]
			r := file.AddRule("linux_object_action_group_import", emission.LegacyName)
			r.SetAttr("object_targets", emission.LegacyTargets)
			r.SetAttr("objects", localLabels(emission.LegacyTargets))
			r.SetAttr("mode", first.Mode)
			r.SetAttr("recipe_id", emission.Group.RecipeID)
			r.SetAttr("reachability_id", compactReachabilityID(emission.Group.ReachableConfigs))
			r.SetAttr("reachable_configs", emission.Group.ReachableConfigs)
			r.SetAttr("tags", []string{"manual"})
		}
	}
	return file.Format(), nil
}

func localLabels(targets []string) []string {
	labels := make([]string, 0, len(targets))
	for _, target := range targets {
		labels = append(labels, ":"+target)
	}
	return labels
}

func (v CompactObjectVariant) sourceBuildReady() bool {
	return v.sourceBuildError() == ""
}

func (v CompactObjectVariant) sourceBuildError() string {
	if v.Source == "" {
		return "no concrete source was resolved"
	}
	for _, flag := range v.Flags {
		for _, ref := range makeVariableRefs(flag) {
			if ref == "obj" || ref == "src" || ref == "srctree" {
				continue
			}
			if knownEmptyKbuildMakeRef(ref) {
				continue
			}
			if _, ok := v.configFragment[ref]; !ok {
				return fmt.Sprintf("flag %q contains unsupported Kbuild make variable %q", flag, ref)
			}
		}
	}
	for _, flag := range v.RemoveFlags {
		for _, ref := range makeVariableRefs(flag) {
			if ref == "obj" || ref == "src" || ref == "srctree" {
				continue
			}
			if knownEmptyKbuildMakeRef(ref) {
				continue
			}
			if _, ok := v.configFragment[ref]; !ok {
				return fmt.Sprintf("remove flag %q contains unsupported Kbuild make variable %q", flag, ref)
			}
		}
	}
	return ""
}

func knownEmptyKbuildMakeRef(ref string) bool {
	if strings.HasPrefix(ref, "cflags-nogcse-") {
		return true
	}
	switch ref {
	case "CC_FLAGS_CFI", "CC_FLAGS_FTRACE", "CC_FLAGS_LTO", "CC_FLAGS_SCS", "CLANG_FLAGS", "DISABLE_KSTACK_ERASE", "DISABLE_LATENT_ENTROPY_PLUGIN", "DISABLE_STACKLEAK_PLUGIN", "RANDSTRUCT_CFLAGS":
		return true
	default:
		return false
	}
}

func makeVariableRefs(value string) []string {
	var out []string
	for i := 0; i < len(value); i++ {
		if value[i] != '$' || i+1 >= len(value) {
			continue
		}
		closer := byte(0)
		switch value[i+1] {
		case '(':
			closer = ')'
		case '{':
			closer = '}'
		default:
			continue
		}
		start := i + 2
		end := strings.IndexByte(value[start:], closer)
		if end < 0 {
			out = append(out, "")
			continue
		}
		end += start
		out = append(out, value[start:end])
		i = end
	}
	return out
}

func (m *CompactMetadata) imageBuildFile(opts CompactBuildFileOptions) ([]byte, error) {
	if opts.BaseConfig == "" {
		return nil, fmt.Errorf("compact image BUILD emission requires a base config")
	}
	plan, err := m.actionGraphPlan()
	if err != nil {
		return nil, err
	}
	visibility := opts.Visibility
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}
	configs := make(map[string]CompactConfig, len(m.Configs))
	for _, config := range m.Configs {
		configs[config.Name] = config
	}
	base, ok := configs[opts.BaseConfig]
	if !ok {
		return nil, fmt.Errorf("compact base config %q is absent from metadata", opts.BaseConfig)
	}

	file := buildgen.NewBuildFile("compact_images.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	file.AddLoad(
		compactObjectGroupRuleLoadLabel(opts.RuleLoadLabel),
		"linux_grouped_compact_image",
	)
	file.AddPackage(visibility)

	names := make([]string, 0, len(m.Configs))
	names = append(names, base.Name)
	for _, config := range m.Configs {
		if config.Name != base.Name {
			names = append(names, config.Name)
		}
	}
	sort.Strings(names[1:])
	shapeTargets := map[string]string{}
	for _, name := range names {
		config := configs[name]
		shape := strings.Join(config.ObjectTargets, "\x00") + "\x01" +
			strings.Join(config.ModuleObjectTargets, "\x00")
		if actual := shapeTargets[shape]; actual != "" {
			r := file.AddRule("alias", config.imageTarget)
			r.SetAttr("actual", ":"+actual)
			r.SetAttr("tags", []string{"manual"})
			continue
		}
		shapeTargets[shape] = config.imageTarget

		groupNames := map[string]bool{}
		for _, emission := range plan.Emissions {
			if !compactStringContains(emission.Group.ReachableConfigs, config.Name) {
				continue
			}
			if emission.GroupedName != "" {
				groupNames[emission.GroupedName] = true
			}
			if emission.LegacyName != "" {
				groupNames[emission.LegacyName] = true
			}
		}
		groupLabels := make([]string, 0, len(groupNames))
		for groupName := range groupNames {
			groupLabels = append(groupLabels, labelFor(opts.ObjectLabelPackage, groupName))
		}
		sort.Strings(groupLabels)
		r := file.AddRule("linux_grouped_compact_image", config.imageTarget)
		r.SetAttr("config", config.Name)
		r.SetAttr("groups", groupLabels)
		r.SetAttr("object_targets", config.ObjectTargets)
		if len(config.ModuleObjectTargets) != 0 {
			r.SetAttr("module_object_targets", config.ModuleObjectTargets)
		}
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		r.SetAttr("tags", []string{"manual"})
	}
	return file.Format(), nil
}

func compactStringContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func compactTargetSet(targets []string) map[string]bool {
	out := make(map[string]bool, len(targets))
	for _, target := range targets {
		out[target] = true
	}
	return out
}

func compactTargetLabels(targets []string, variants map[string]CompactObjectVariant, objectLabelPackage string) ([]string, error) {
	labels := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, ok := variants[target]; !ok {
			return nil, fmt.Errorf("compact image target %q is absent from object metadata", target)
		}
		labels = append(labels, labelFor(objectLabelPackage, target))
	}
	return labels, nil
}

func compactContentIDs(targets []string, variants map[string]CompactObjectVariant) ([]string, error) {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		variant, ok := variants[target]
		if !ok {
			return nil, fmt.Errorf("compact image target %q is absent from object metadata", target)
		}
		if variant.ContentID == "" {
			return nil, fmt.Errorf("compact image target %q has no content ID", target)
		}
		ids = append(ids, variant.ContentID)
	}
	return ids, nil
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func configRefs(value string) []string {
	refs := map[string]bool{}
	for start := 0; start < len(value); {
		idx := strings.Index(value[start:], "CONFIG_")
		if idx < 0 {
			break
		}
		idx += start
		end := idx + len("CONFIG_")
		for end < len(value) {
			c := value[end]
			if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				end++
				continue
			}
			break
		}
		if end > idx+len("CONFIG_") {
			refs[value[idx:end]] = true
		}
		start = end
	}
	out := make([]string, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func compactRuleLoadLabel(label string) string {
	if label == "" {
		return "@linux.bzl//internal:linux_objects.bzl"
	}
	return label
}

func compactObjectGroupRuleLoadLabel(linuxObjectsLabel string) string {
	label := compactRuleLoadLabel(linuxObjectsLabel)
	if strings.HasSuffix(label, "linux_objects.bzl") {
		return strings.TrimSuffix(label, "linux_objects.bzl") + "linux_object_groups.bzl"
	}
	return "@linux.bzl//internal:linux_object_groups.bzl"
}

func sanitizeTargetName(value string) string {
	value = filepath.ToSlash(value)
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r)
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "unnamed"
	}
	return out.String()
}

func modePrecedence(mode string) int {
	switch mode {
	case "y":
		return 2
	case "m":
		return 1
	default:
		return 0
	}
}

func compositeMemberMode(parentMode, memberMode string) string {
	if memberMode == "n" {
		return "n"
	}
	if memberMode == "m" && parentMode != "m" {
		return "n"
	}
	return parentMode
}
