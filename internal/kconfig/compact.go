package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
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

type CompactSchema string

const (
	CompactSchemaV011 CompactSchema = "v0.0.11"
	CompactSchemaV012 CompactSchema = "v0.0.12"
	CompactSchemaV013 CompactSchema = "v0.0.13"
)

func ParseCompactSchema(value string) (CompactSchema, error) {
	schema := CompactSchema(value)
	switch schema {
	case CompactSchemaV011, CompactSchemaV012, CompactSchemaV013:
		return schema, nil
	default:
		return "", fmt.Errorf(
			"unsupported compact schema %q (want %q, %q, or %q)",
			value,
			CompactSchemaV011,
			CompactSchemaV012,
			CompactSchemaV013,
		)
	}
}

func (s CompactSchema) isV012() bool {
	return s == CompactSchemaV012 || s == CompactSchemaV013
}

func (s CompactSchema) isV013() bool {
	return s == CompactSchemaV013
}

type CompactMetadata struct {
	Schema              CompactSchema               `json:"schema,omitempty"`
	Configs             []CompactConfig             `json:"configs"`
	ConfigPayloads      []CompactConfigPayload      `json:"config_payloads,omitempty"`
	CompileEnvironments []CompactCompileEnvironment `json:"compile_environments,omitempty"`
	HeaderGroups        []CompactHeaderGroup        `json:"header_groups,omitempty"`
	SourceFiles         []CompactSourceInput        `json:"source_files,omitempty"`
	SourceInputGroups   []string                    `json:"source_input_groups,omitempty"`
	ObjectPackages      []CompactObjectPackage      `json:"object_packages,omitempty"`
	ObjectVariants      []CompactObjectVariant      `json:"object_variants"`
}

type CompactConfig struct {
	Name                string   `json:"name"`
	ImageTarget         string   `json:"image_target"`
	ConfigPayload       string   `json:"config_payload,omitempty"`
	ObjectTargets       []string `json:"object_targets"`
	ModuleObjectTargets []string `json:"module_object_targets,omitempty"`
}

type CompactSourceInput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type CompactConfigPayload struct {
	ID       string            `json:"id"`
	Content  string            `json:"content"`
	Fragment map[string]string `json:"fragment,omitempty"`
}

type CompactCompileEnvironment struct {
	ID            string   `json:"id"`
	ABI           string   `json:"abi"`
	ConfigPayload string   `json:"config_payload"`
	HeaderGroups  []string `json:"header_groups,omitempty"`
}

type CompactHeaderGroup struct {
	ID               string               `json:"id"`
	ConfigPayload    string               `json:"config_payload"`
	Labels           []string             `json:"labels,omitempty"`
	Srcarch          string               `json:"srcarch"`
	Footprint        string               `json:"footprint"`
	SourceInputGroup int                  `json:"source_input_group,omitempty"`
	SourceInputs     []CompactSourceInput `json:"source_inputs,omitempty"`
}

type CompactObjectVariant struct {
	Target             string               `json:"target"`
	ContentID          string               `json:"content_id,omitempty"`
	CompileEnvironment string               `json:"compile_environment,omitempty"`
	Package            string               `json:"package,omitempty"`
	Object             string               `json:"object"`
	Source             string               `json:"source,omitempty"`
	SourceIncludes     []string             `json:"source_includes,omitempty"`
	SourceInputGroup   int                  `json:"source_input_group,omitempty"`
	SourceInputs       []CompactSourceInput `json:"source_inputs,omitempty"`
	Mode               string               `json:"mode"`
	ModName            string               `json:"modname,omitempty"`
	Flags              []string             `json:"flags,omitempty"`
	RemoveFlags        []string             `json:"remove_flags,omitempty"`
	ConfigFragment     map[string]string    `json:"config_fragment,omitempty"`
	Deps               []string             `json:"deps,omitempty"`
	Members            []string             `json:"members,omitempty"`
}

type CompactObjectPackage struct {
	Package       string   `json:"package"`
	ObjectTargets []string `json:"object_targets"`
}

type CompactBuildFileOptions struct {
	Schema                   CompactSchema
	Arch                     string
	Version                  string
	Visibility               []string
	RuleLoadLabel            string
	SourceLabelPackage       string
	SourceLabelPackages      map[string]string
	SourceASN1Compiler       string
	SourceRelacheck          string
	SourceRootLabel          string
	SourceTreeAllFiles       []string
	SourceTreeArchHeaders    []string
	SourceTreeDtbSources     []string
	SourceTreeGlobalHeaders  []string
	SourceTreeHeaders        []string
	SourceTreeKbuildFiles    []string
	SourceTreeLocalIncludes  []string
	SourceTreeLookupFiles    []string
	SourceTreeScriptsHeaders []string
	SourceTreeUapiHeaders    []string
	GeneratedHeaders         string
	SourceConfig             string
	Srcarch                  string
}

type CompactImageBuildFileOptions struct {
	Schema             CompactSchema
	Arch               string
	BaseConfig         string
	Visibility         []string
	ObjectLabelPackage string
	RequireReal        bool
	RuleLoadLabel      string
}

func (opts CompactBuildFileOptions) hasSourceTreeLabels() bool {
	return len(opts.SourceTreeAllFiles) != 0 ||
		len(opts.SourceTreeArchHeaders) != 0 ||
		len(opts.SourceTreeDtbSources) != 0 ||
		len(opts.SourceTreeGlobalHeaders) != 0 ||
		len(opts.SourceTreeHeaders) != 0 ||
		len(opts.SourceTreeKbuildFiles) != 0 ||
		len(opts.SourceTreeLocalIncludes) != 0 ||
		len(opts.SourceTreeLookupFiles) != 0 ||
		len(opts.SourceTreeScriptsHeaders) != 0 ||
		len(opts.SourceTreeUapiHeaders) != 0
}

type CompactMetadataOptions struct {
	Schema                CompactSchema
	ObjectDir             string
	SourceRoot            string
	SourceRoots           map[string]string
	LibraryDirs           []string
	GeneratedHeadersLabel string
	CompileEnvironmentABI string
	// Srcarch selects architecture include roots while scanning source files for
	// CONFIG_* dependencies.
	Srcarch string
}

func (t *Tree) CompactMetadata(kb *KbuildFile, configs []NamedConfig) (*CompactMetadata, error) {
	return t.CompactMetadataWithOptions(kb, configs, CompactMetadataOptions{})
}

func MergeCompactMetadata(parts ...*CompactMetadata) (*CompactMetadata, error) {
	out := &CompactMetadata{}
	seenConfigs := map[string]bool{}
	variants := map[string]CompactObjectVariant{}
	configPayloads := map[string]CompactConfigPayload{}
	compileEnvironments := map[string]CompactCompileEnvironment{}
	headerGroups := map[string]CompactHeaderGroup{}
	sourceInputInterner := newCompactSourceInputInterner()
	for _, part := range parts {
		if part == nil {
			continue
		}
		if part.Schema != "" {
			if out.Schema != "" && out.Schema != part.Schema {
				return nil, fmt.Errorf("cannot merge compact metadata schemas %q and %q", out.Schema, part.Schema)
			}
			out.Schema = part.Schema
		}
		for _, config := range part.Configs {
			if seenConfigs[config.Name] {
				return nil, fmt.Errorf("duplicate compact config name %q", config.Name)
			}
			seenConfigs[config.Name] = true
			out.Configs = append(out.Configs, config)
		}
		for _, original := range part.ObjectVariants {
			variant := original
			if part.Schema.isV013() {
				sourceInputs, err := part.expandedSourceInputGroup(
					variant.SourceInputGroup,
					variant.SourceInputs,
					fmt.Sprintf("object target %q", variant.Target),
				)
				if err != nil {
					return nil, err
				}
				variant.SourceInputGroup, err = sourceInputInterner.intern(
					sourceInputs,
					fmt.Sprintf("object target %q", variant.Target),
				)
				if err != nil {
					return nil, err
				}
				variant.SourceInputs = nil
			}
			if existing, ok := variants[variant.Target]; ok && !existing.equal(variant) {
				return nil, fmt.Errorf("object variants %q and %q produce duplicate target %q", existing.Object, variant.Object, variant.Target)
			}
			variants[variant.Target] = variant
		}
		for _, payload := range part.ConfigPayloads {
			if existing, ok := configPayloads[payload.ID]; ok && !existing.equal(payload) {
				return nil, fmt.Errorf("config payloads with content ID %q differ", payload.ID)
			}
			configPayloads[payload.ID] = payload
		}
		for _, environment := range part.CompileEnvironments {
			if existing, ok := compileEnvironments[environment.ID]; ok && !existing.equal(environment) {
				return nil, fmt.Errorf("compile environments with content ID %q differ", environment.ID)
			}
			compileEnvironments[environment.ID] = environment
		}
		for _, original := range part.HeaderGroups {
			group := original
			if part.Schema.isV013() {
				sourceInputs, err := part.expandedSourceInputGroup(
					group.SourceInputGroup,
					group.SourceInputs,
					fmt.Sprintf("header group %q", group.ID),
				)
				if err != nil {
					return nil, err
				}
				group.SourceInputGroup, err = sourceInputInterner.intern(
					sourceInputs,
					fmt.Sprintf("header group %q", group.ID),
				)
				if err != nil {
					return nil, err
				}
				group.SourceInputs = nil
			}
			if existing, ok := headerGroups[group.ID]; ok {
				if existing.ConfigPayload != group.ConfigPayload || existing.Srcarch != group.Srcarch || existing.Footprint != group.Footprint || existing.SourceInputGroup != group.SourceInputGroup || !compactSourceInputsEqual(existing.SourceInputs, group.SourceInputs) {
					return nil, fmt.Errorf("header groups with content ID %q differ", group.ID)
				}
				existing.Labels = appendUniqueStrings(existing.Labels, group.Labels...)
				headerGroups[group.ID] = existing
				continue
			}
			headerGroups[group.ID] = group
		}
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
	out.HeaderGroups = sortedCompactHeaderGroups(headerGroups)
	out.ObjectPackages = compactObjectPackages(out.ObjectVariants)
	if out.Schema.isV013() {
		sourceInputInterner.apply(out)
		if err := out.canonicalizeSourceInputIndex(); err != nil {
			return nil, err
		}
	}
	if err := out.validateContentIDs(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tree) CompactMetadataWithOptions(kb *KbuildFile, configs []NamedConfig, opts CompactMetadataOptions) (*CompactMetadata, error) {
	return t.compactMetadataBatchWithOptions(configs, opts, func(*ResolvedConfig) (*KbuildFile, string, error) {
		return kb, opts.GeneratedHeadersLabel, nil
	})
}

// CompactMetadataBatchWithOptions resolves and emits multiple config-specific
// Kbuild graphs while sharing immutable source scanning work for the invocation.
func (t *Tree) CompactMetadataBatchWithOptions(
	configs []NamedConfig,
	opts CompactMetadataOptions,
	kbuildForConfig func(*ResolvedConfig) (*KbuildFile, string, error),
) (*CompactMetadata, error) {
	if kbuildForConfig == nil {
		return nil, fmt.Errorf("compact metadata Kbuild resolver must not be nil")
	}
	return t.compactMetadataBatchWithOptions(configs, opts, kbuildForConfig)
}

func (t *Tree) compactMetadataBatchWithOptions(
	configs []NamedConfig,
	opts CompactMetadataOptions,
	kbuildForConfig func(*ResolvedConfig) (*KbuildFile, string, error),
) (*CompactMetadata, error) {
	if opts.Schema.isV013() && strings.TrimSpace(opts.CompileEnvironmentABI) == "" {
		return nil, fmt.Errorf("compact schema %s requires a non-empty compile environment ABI", opts.Schema)
	}
	if opts.Schema.isV013() && opts.SourceRoot == "" && len(opts.SourceRoots) == 0 {
		return nil, fmt.Errorf("compact schema %s requires a source root for exact input scanning", opts.Schema)
	}
	variants := map[string]CompactObjectVariant{}
	out := &CompactMetadata{}
	if opts.Schema.isV013() {
		out.Schema = opts.Schema
	}
	configPayloads := map[string]CompactConfigPayload{}
	compileEnvironments := map[string]CompactCompileEnvironment{}
	headerGroups := map[string]CompactHeaderGroup{}
	var sourceInputInterner *compactSourceInputInterner
	if opts.Schema.isV013() {
		sourceInputInterner = newCompactSourceInputInterner()
	}
	seenConfigs := map[string]bool{}
	seenImageTargets := map[string]string{}
	var sourceCache *compactSourceCache
	if opts.Schema.isV012() {
		sourceCache = newCompactSourceCache()
	}
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
		kb, generatedHeadersLabel, err := kbuildForConfig(resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve Kbuild for config %q: %w", named.Name, err)
		}
		if kb == nil {
			return nil, fmt.Errorf("resolve Kbuild for config %q: nil Kbuild", named.Name)
		}
		var scanner *configSourceScanner
		if sourceCache != nil {
			scanner = newConfigSourceScannerWithCache(opts, sourceCache)
		}
		fullConfigPayload := CompactConfigPayload{}
		headerGroupID := ""
		if opts.Schema.isV013() {
			fullConfigPayload = newCompactConfigPayload(compactFullConfigFragment(resolved))
			configPayloads[fullConfigPayload.ID] = fullConfigPayload
			if generatedHeadersLabel != "" {
				headerFragment, headerInputs, footprint, err := generatedHeaderFootprint(resolved, opts, scanner)
				if err != nil {
					return nil, fmt.Errorf("derive generated headers for config %q: %w", named.Name, err)
				}
				headerConfigPayload := newCompactConfigPayload(headerFragment)
				configPayloads[headerConfigPayload.ID] = headerConfigPayload
				group := newCompactHeaderGroup(
					headerConfigPayload.ID,
					generatedHeadersLabel,
					opts.Srcarch,
					footprint,
					headerInputs,
				)
				group.SourceInputGroup, err = sourceInputInterner.intern(
					group.SourceInputs,
					fmt.Sprintf("header group %q", group.ID),
				)
				if err != nil {
					return nil, err
				}
				group.SourceInputs = nil
				if existing, ok := headerGroups[group.ID]; ok {
					existing.Labels = appendUniqueStrings(existing.Labels, group.Labels...)
					headerGroups[group.ID] = existing
				} else {
					headerGroups[group.ID] = group
				}
				headerGroupID = group.ID
			}
		}
		imageTarget := sanitizeTargetName(named.Name) + "_image"
		if existing := seenImageTargets[imageTarget]; existing != "" {
			return nil, fmt.Errorf("compact config names %q and %q produce duplicate image target %q", existing, named.Name, imageTarget)
		}
		seenImageTargets[imageTarget] = named.Name

		objects := kb.resolvedObjects(resolved)
		resolvedVariants := compactVariantMemo{}
		for _, object := range objects.all() {
			if opts.Schema.isV012() && rustSDKOwnsObject(object.object) {
				continue
			}
			variant, err := resolvedVariants.variantFor(object.object, resolved, opts, objects, scanner, headerGroupID)
			if err != nil {
				return nil, err
			}
			if sourceInputInterner != nil {
				variant.SourceInputGroup, err = sourceInputInterner.intern(
					variant.SourceInputs,
					fmt.Sprintf("object target %q", variant.Target),
				)
				if err != nil {
					return nil, err
				}
				variant.SourceInputs = nil
			}
			resolvedVariants[object.object] = variant
			if existing, ok := variants[variant.Target]; ok && !existing.equal(variant) {
				return nil, fmt.Errorf("object variants %q and %q produce duplicate target %q", existing.Object, variant.Object, variant.Target)
			}
			variants[variant.Target] = variant
			if opts.Schema.isV013() && variant.CompileEnvironment != "" {
				payload := newCompactConfigPayload(variant.ConfigFragment)
				configPayloads[payload.ID] = payload
				environment := newCompactCompileEnvironment(opts.CompileEnvironmentABI, payload.ID, compactOptionalString(headerGroupID))
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
				if opts.Schema.isV012() && rustSDKOwnsObject(object.object) {
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
			ImageTarget:         imageTarget,
			ConfigPayload:       fullConfigPayload.ID,
			ObjectTargets:       targets,
			ModuleObjectTargets: moduleTargets,
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
	out.HeaderGroups = sortedCompactHeaderGroups(headerGroups)
	out.ObjectPackages = compactObjectPackages(out.ObjectVariants)
	if sourceInputInterner != nil {
		sourceInputInterner.apply(out)
		if err := out.canonicalizeSourceInputIndex(); err != nil {
			return nil, err
		}
	}
	if err := out.validateContentIDs(); err != nil {
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
	if v.Target != other.Target || v.ContentID != other.ContentID || v.CompileEnvironment != other.CompileEnvironment || v.Package != other.Package || v.Object != other.Object || v.Source != other.Source || v.SourceInputGroup != other.SourceInputGroup || v.Mode != other.Mode || v.ModName != other.ModName || len(v.SourceIncludes) != len(other.SourceIncludes) || len(v.SourceInputs) != len(other.SourceInputs) || len(v.Flags) != len(other.Flags) || len(v.RemoveFlags) != len(other.RemoveFlags) || len(v.ConfigFragment) != len(other.ConfigFragment) || len(v.Deps) != len(other.Deps) || len(v.Members) != len(other.Members) {
		return false
	}
	for i := range v.SourceIncludes {
		if v.SourceIncludes[i] != other.SourceIncludes[i] {
			return false
		}
	}
	for i := range v.SourceInputs {
		if v.SourceInputs[i] != other.SourceInputs[i] {
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
	for key, value := range v.ConfigFragment {
		if other.ConfigFragment[key] != value {
			return false
		}
	}
	return true
}

type compactMetadataJSON CompactMetadata

func (m CompactMetadata) MarshalJSON() ([]byte, error) {
	normalized := compactMetadataJSON(m)
	if !m.Schema.isV013() {
		return json.Marshal(normalized)
	}
	normalized.ConfigPayloads = append([]CompactConfigPayload(nil), m.ConfigPayloads...)
	for i := range normalized.ConfigPayloads {
		normalized.ConfigPayloads[i].Fragment = nil
	}
	normalized.ObjectVariants = append([]CompactObjectVariant(nil), m.ObjectVariants...)
	for i := range normalized.ObjectVariants {
		normalized.ObjectVariants[i].ConfigFragment = nil
	}
	return json.Marshal(normalized)
}

func (m *CompactMetadata) JSON() ([]byte, error) {
	if err := m.canonicalizeSourceInputIndex(); err != nil {
		return nil, err
	}
	if err := m.validateContentIDs(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(m, "", "  ")
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
	object    string
	directory string
	mode      string
	modname   string
	flags     []resolvedKbuildFlag
	remove    []resolvedKbuildFlag
	footprint map[string]bool
	members   []string
	root      bool
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

func (memo compactVariantMemo) variantFor(name string, config *ResolvedConfig, opts CompactMetadataOptions, objects resolvedKbuildObjects, scanner *configSourceScanner, headerGroupID string) (CompactObjectVariant, error) {
	return memo.variantForStack(name, config, opts, objects, scanner, headerGroupID, map[string]bool{})
}

func (memo compactVariantMemo) variantForStack(name string, config *ResolvedConfig, opts CompactMetadataOptions, objects resolvedKbuildObjects, scanner *configSourceScanner, headerGroupID string, stack map[string]bool) (CompactObjectVariant, error) {
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
		variant, err := memo.variantForStack(member, config, opts, objects, scanner, headerGroupID, stack)
		if err != nil {
			return CompactObjectVariant{}, err
		}
		members = append(members, variant.Target)
		memberContentIDs = append(memberContentIDs, variant.ContentID)
	}
	delete(stack, name)

	source := sourceForObject(opts.SourceRoot, opts.ObjectDir, object.object, opts.SourceRoots)
	specialSources := compactSpecialSourceInputs{}
	if opts.Schema.isV013() {
		specialSources = compactSpecialSourcesForObject(name)
		if specialSources.primary != "" {
			source = specialSources.primary
		}
	}
	if len(members) != 0 {
		source = ""
	}
	if opts.Schema.isV013() && len(members) == 0 && source == "" {
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
			variant, err := memo.variantForStack(dep.object, config, opts, objects, scanner, headerGroupID, stack)
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

	var sourceRefs, sourceIncludes []string
	var sourceInputs []CompactSourceInput
	if (opts.Schema == CompactSchemaV012 || opts.Schema == CompactSchemaV013) && source != "" {
		flags := normalizeSourceRootFlags(filterResolvedKbuildFlags(object.flags, source), opts.SourceRoot)
		includeDirs := includeDirsFromFlags(flags)
		actionFootprint := compactObjectActionFootprintForObject(name, flags)
		actionFootprint.providedIncludes = appendUniqueStrings(
			actionFootprint.providedIncludes,
			asn1ProvidedIncludes...,
		)
		profile := sourceScanKernel
		if object.mode == "m" {
			profile = sourceScanKernelModule
		}
		var closure sourceClosure
		var err error
		if opts.Schema.isV013() {
			closure, err = scanner.closureForSourceConfigInputsSearchProfile(
				source,
				scanner.actionIncludeSearch(source, flags),
				config,
				isAssemblySourcePath(source),
				actionFootprint.providedIncludes,
				profile,
			)
		} else {
			closure, err = scanner.closureForSourceConfigInputs(
				source,
				includeDirs,
				config,
				isAssemblySourcePath(source),
				actionFootprint.providedIncludes,
			)
		}
		if err != nil {
			return CompactObjectVariant{}, fmt.Errorf("scan source inputs for %s: %w", name, err)
		}
		sourceRefs = append(sourceRefs, closure.refs...)
		sourceIncludes = append(sourceIncludes, closure.sourceIncludes...)
		if opts.Schema.isV013() {
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
			for _, forcedSource := range forcedSourceInputs(flags, source) {
				forcedClosure, err := scanner.closureForSourceConfigInputsSearchProfile(
					forcedSource,
					scanner.actionIncludeSearch(source, flags),
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
				sourceIncludes = appendUniqueStrings(sourceIncludes, forcedClosure.sourceIncludes...)
				sourceInputs = appendUniqueSourceInputs(sourceInputs, forcedClosure.sourceInputs...)
			}
			for _, generatedSource := range actionFootprint.closureInputs {
				generatedClosure, err := scanner.closureForSourceConfigInputsSearchProfile(
					generatedSource,
					scanner.actionIncludeSearch(source, flags),
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
				sourceIncludes = appendUniqueStrings(sourceIncludes, generatedClosure.sourceIncludes...)
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
				sourceIncludes = appendUniqueStrings(sourceIncludes, specialClosure.sourceIncludes...)
				sourceInputs = appendUniqueSourceInputs(sourceInputs, specialClosure.sourceInputs...)
				if !input.compiled {
					continue
				}
				for _, forcedSource := range forcedSourceInputs(nil, input.path) {
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
					sourceIncludes = appendUniqueStrings(sourceIncludes, forcedClosure.sourceIncludes...)
					sourceInputs = appendUniqueSourceInputs(sourceInputs, forcedClosure.sourceInputs...)
				}
			}
		}
		if opts.Schema == CompactSchemaV012 && isMultiSourceImageObject(name) {
			dirClosure, err := scanner.closureForSourceDirConfig(objectPackage(name), config)
			if err != nil {
				return CompactObjectVariant{}, fmt.Errorf("scan multi-source inputs for %s: %w", name, err)
			}
			sourceRefs = appendUniqueStrings(sourceRefs, dirClosure.refs...)
			sourceIncludes = appendUniqueStrings(sourceIncludes, dirClosure.sourceIncludes...)
		}
	}
	if opts.Schema.isV013() && isArm64NvheObject(name) {
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
			sourceIncludes = appendUniqueStrings(sourceIncludes, closure.sourceIncludes...)
			sourceInputs = appendUniqueSourceInputs(sourceInputs, closure.sourceInputs...)
		}
	}
	variant := object.variant(
		config,
		source,
		sourceIncludes,
		sourceInputs,
		members,
		deps,
		memberContentIDs,
		depContentIDs,
		opts.SourceRoot,
		sourceRefs,
		opts.Schema,
		opts.CompileEnvironmentABI,
		headerGroupID,
	)
	memo[name] = variant
	return variant, nil
}

func isMultiSourceImageObject(object string) bool {
	return strings.HasPrefix(object, "arch/x86/entry/vdso/vdso-image-") ||
		object == "arch/x86/realmode/rmpiggy.o" ||
		object == "arch/x86/purgatory/kexec-purgatory.o"
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
	sourceInputs     []string
	closureInputs    []string
	providedIncludes []string
	configSymbols    []string
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
	case "arch/x86/kernel/cpu/capflags.o":
		footprint.sourceInputs = []string{
			"arch/x86/include/asm/cpufeatures.h",
			"arch/x86/include/asm/vmxfeatures.h",
		}
	case "arch/x86/realmode/rmpiggy.o":
		footprint.providedIncludes = []string{"pasyms.h"}
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
	return footprint
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

func includeDirsFromFlags(flags []string) []string {
	var dirs []string
	for i := 0; i < len(flags); i++ {
		path := ""
		if flags[i] == "-I" && i+1 < len(flags) {
			path = flags[i+1]
			i++
		} else if rest, ok := strings.CutPrefix(flags[i], "-I"); ok {
			path = rest
		} else {
			continue
		}
		path = strings.TrimSpace(path)
		if path == "$(srctree)" {
			dirs = append(dirs, "")
		} else if rel, ok := strings.CutPrefix(path, "$(srctree)/"); ok {
			dirs = append(dirs, rel)
		}
	}
	return dirs
}

func forcedSourceInputs(flags []string, source string) []string {
	out := []string{
		"include/linux/compiler-version.h",
		"include/linux/kconfig.h",
	}
	if !strings.HasSuffix(source, ".S") && !strings.HasSuffix(source, ".s") {
		out = append(out, "include/linux/compiler_types.h")
	}
	for i := 0; i < len(flags); i++ {
		value := ""
		switch {
		case (flags[i] == "-include" || flags[i] == "-imacros") && i+1 < len(flags):
			value = flags[i+1]
			i++
		case strings.HasPrefix(flags[i], "-include") && len(flags[i]) > len("-include"):
			value = strings.TrimPrefix(flags[i], "-include")
		case strings.HasPrefix(flags[i], "-imacros") && len(flags[i]) > len("-imacros"):
			value = strings.TrimPrefix(flags[i], "-imacros")
		}
		value = strings.TrimSpace(value)
		if rel, ok := strings.CutPrefix(value, "$(srctree)/"); ok {
			out = appendUnique(out, filepath.ToSlash(rel))
		}
	}
	sort.Strings(out)
	return out
}

func (o resolvedKbuildObject) variant(
	config *ResolvedConfig,
	source string,
	sourceIncludes []string,
	sourceInputs []CompactSourceInput,
	members []string,
	deps []string,
	memberContentIDs []string,
	depContentIDs []string,
	sourceRoot string,
	sourceRefs []string,
	schema CompactSchema,
	compileEnvironmentABI string,
	headerGroupID string,
) CompactObjectVariant {
	fragment := map[string]string{}
	refset := make(map[string]bool, len(o.footprint)+len(sourceRefs))
	if !schema.isV013() || len(members) == 0 {
		for ref := range o.footprint {
			refset[ref] = true
		}
	}
	if schema == CompactSchemaV012 || schema == CompactSchemaV013 {
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
	}
	refs := make([]string, 0, len(refset))
	for ref := range refset {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if !schema.isV012() || config.ShouldWrite(ref) {
			fragment[ref] = config.Value(ref)
		} else {
			fragment[ref] = "n"
		}
	}
	flags := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.flags, source), sourceRoot)
	remove := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.remove, source), sourceRoot)
	modname := o.modname
	compileComposite := schema.isV013() && isArm64NvheObject(o.object)
	if schema.isV013() && len(members) != 0 {
		modname = ""
		flags = nil
		remove = nil
		sourceIncludes = nil
		deps = nil
		depContentIDs = nil
		if !compileComposite {
			sourceInputs = nil
		}
	}
	targetHash := ""
	contentID := ""
	compileEnvironmentID := ""
	if schema.isV013() {
		usesCompileEnvironment := len(members) == 0 || compileComposite
		if usesCompileEnvironment {
			payload := newCompactConfigPayload(fragment)
			environment := newCompactCompileEnvironment(compileEnvironmentABI, payload.ID, compactOptionalString(headerGroupID))
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
		)
		targetHash = compactShortID(contentID)
	} else {
		targetHash = objectVariantHash(o.object, o.mode, modname, flags, remove, fragment, sourceIncludes, deps, members)
	}
	return CompactObjectVariant{
		Target:             sanitizeTargetName(strings.TrimSuffix(o.object, ".o")) + "__" + targetHash,
		ContentID:          contentID,
		CompileEnvironment: compileEnvironmentID,
		Package:            objectPackage(o.object),
		Object:             o.object,
		Source:             source,
		SourceIncludes:     append([]string(nil), sourceIncludes...),
		SourceInputs:       append([]CompactSourceInput(nil), sourceInputs...),
		Mode:               o.mode,
		ModName:            modname,
		Flags:              flags,
		RemoveFlags:        remove,
		ConfigFragment:     fragment,
		Deps:               append([]string(nil), deps...),
		Members:            append([]string(nil), members...),
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
	flag = normalizeCompactPath(flag)
	if flag == sourceRoot {
		return "$(srctree)"
	}
	if strings.HasPrefix(flag, sourceRoot+"/") {
		return "$(srctree)/" + strings.TrimPrefix(flag, sourceRoot+"/")
	}
	for _, prefix := range []string{"-I", "-iquote", "-isystem", "-include"} {
		path := strings.TrimPrefix(flag, prefix)
		if path == flag {
			continue
		}
		if path == sourceRoot {
			return prefix + "$(srctree)"
		}
		if strings.HasPrefix(path, sourceRoot+"/") {
			return prefix + "$(srctree)/" + strings.TrimPrefix(path, sourceRoot+"/")
		}
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

func compactObjectPackages(variants []CompactObjectVariant) []CompactObjectPackage {
	byPackage := map[string][]string{}
	for _, variant := range variants {
		byPackage[variant.Package] = append(byPackage[variant.Package], variant.Target)
	}
	packages := make([]string, 0, len(byPackage))
	for pkg := range byPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	out := make([]CompactObjectPackage, 0, len(packages))
	for _, pkg := range packages {
		targets := byPackage[pkg]
		sort.Strings(targets)
		out = append(out, CompactObjectPackage{
			Package:       pkg,
			ObjectTargets: targets,
		})
	}
	return out
}

func objectPackage(object string) string {
	dir := filepath.ToSlash(filepath.Dir(object))
	if dir == "." {
		return ""
	}
	return dir
}

func objectVariantHash(object, mode, modname string, flags, removeFlags []string, fragment map[string]string, sourceIncludes, deps, members []string) string {
	var b strings.Builder
	b.WriteString(object)
	b.WriteByte('\n')
	b.WriteString(mode)
	b.WriteByte('\n')
	b.WriteString(modname)
	b.WriteByte('\n')
	for _, flag := range flags {
		b.WriteString(flag)
		b.WriteByte('\n')
	}
	for _, flag := range removeFlags {
		b.WriteString("remove=")
		b.WriteString(flag)
		b.WriteByte('\n')
	}
	for _, key := range sortedConfigKeys(fragment) {
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(fragment[key])
		b.WriteByte('\n')
	}
	for _, include := range sourceIncludes {
		b.WriteString("source_include=")
		b.WriteString(include)
		b.WriteByte('\n')
	}
	for _, dep := range deps {
		b.WriteString("dep=")
		b.WriteString(dep)
		b.WriteByte('\n')
	}
	for _, member := range members {
		b.WriteString("member=")
		b.WriteString(member)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

func (m *CompactMetadata) ObjectBuildFile(opts CompactBuildFileOptions) ([]byte, error) {
	if opts.Schema.isV013() {
		if err := m.validateContentIDs(); err != nil {
			return nil, err
		}
	}
	visibility := opts.Visibility
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}
	file := buildgen.NewBuildFile("compact_objects.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	loads := []string{"linux_object"}
	compileEnvironmentIndexTarget := ""
	sourceInputIndexTarget := ""
	if opts.Schema.isV013() {
		loads = append(loads, "linux_compile_environment_index", "linux_source_input_index")
		compileEnvironmentIndexTarget = "_compile_environment_index"
		sourceInputIndexTarget = "_source_input_index"
	}
	sourceTreeTarget := ""
	if opts.SourceRootLabel != "" || opts.hasSourceTreeLabels() {
		loads = append(loads, "linux_source_tree")
		sourceTreeTarget = "_source_tree"
	}
	if m.objectBuildFileNeedsConfig(opts) {
		loads = append(loads, "linux_config")
	}
	if m.objectBuildFileNeedsComposite() {
		loads = append(loads, "linux_composite_object")
	}
	if m.objectBuildFileNeedsArm64Nvhe() {
		loads = append(loads, "linux_arm64_nvhe_object")
	}
	sort.Strings(loads)
	file.AddLoad(compactRuleLoadLabel(opts.RuleLoadLabel), loads...)
	file.AddPackage(visibility)
	if compileEnvironmentIndexTarget != "" {
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
		if opts.Schema.isV013() && expectedABI == "" {
			return nil, fmt.Errorf("v0.0.13 compile environment index has no ABI")
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
		generatedHeaders := make(map[string]string, len(m.HeaderGroups))
		for _, group := range m.HeaderGroups {
			labels := append([]string(nil), group.Labels...)
			sort.Strings(labels)
			if len(labels) == 0 {
				continue
			}
			label := labels[0]
			if existing, ok := generatedHeaders[label]; ok && existing != group.ID {
				return nil, fmt.Errorf(
					"generated headers label %q maps to header groups %q and %q",
					label,
					existing,
					group.ID,
				)
			}
			generatedHeaders[label] = group.ID
		}
		r := file.AddRule("linux_compile_environment_index", compileEnvironmentIndexTarget)
		r.SetAttr("config_payloads", configPayloads)
		r.SetAttr("compile_environments", compileEnvironments)
		if expectedABI != "" {
			r.SetAttr("expected_abi", expectedABI)
		}
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
	}
	if sourceInputIndexTarget != "" {
		sourcePaths := make([]string, 0, len(m.SourceFiles))
		sourceLabels := make([]string, 0, len(m.SourceFiles))
		for _, input := range m.SourceFiles {
			sourcePaths = append(sourcePaths, input.Path)
			sourceLabels = append(sourceLabels, labelForSource(opts, input.Path))
		}
		r := file.AddRule("linux_source_input_index", sourceInputIndexTarget)
		r.SetAttr("groups", m.SourceInputGroups)
		r.SetAttr("source_paths", sourcePaths)
		r.SetAttr("srcs", sourceLabels)
		r.SetAttr("tags", []string{"manual"})
	}
	if sourceTreeTarget != "" {
		r := file.AddRule("linux_source_tree", sourceTreeTarget)
		r.SetAttr("tags", []string{"manual"})
		if opts.SourceRootLabel != "" {
			r.SetAttr("root", opts.SourceRootLabel)
		}
		if !opts.Schema.isV013() {
			if len(opts.SourceTreeAllFiles) != 0 {
				r.SetAttr("all_files", opts.SourceTreeAllFiles)
			}
			if len(opts.SourceTreeArchHeaders) != 0 {
				r.SetAttr("arch_headers", opts.SourceTreeArchHeaders)
			}
			if len(opts.SourceTreeDtbSources) != 0 {
				r.SetAttr("dtb_sources", opts.SourceTreeDtbSources)
			}
			if len(opts.SourceTreeGlobalHeaders) != 0 {
				r.SetAttr("global_headers", opts.SourceTreeGlobalHeaders)
			}
			if len(opts.SourceTreeHeaders) != 0 {
				r.SetAttr("headers", opts.SourceTreeHeaders)
			}
			if len(opts.SourceTreeKbuildFiles) != 0 {
				r.SetAttr("kbuild_files", opts.SourceTreeKbuildFiles)
			}
			if opts.Schema.isV012() && len(opts.SourceTreeLocalIncludes) != 0 {
				r.SetAttr("local_include_files", opts.SourceTreeLocalIncludes)
			}
			if len(opts.SourceTreeScriptsHeaders) != 0 {
				r.SetAttr("scripts_headers", opts.SourceTreeScriptsHeaders)
			}
			if len(opts.SourceTreeUapiHeaders) != 0 {
				r.SetAttr("uapi_headers", opts.SourceTreeUapiHeaders)
			}
		}
		if opts.Schema == CompactSchemaV012 && len(opts.SourceTreeLookupFiles) != 0 {
			r.SetAttr("lookup_files", opts.SourceTreeLookupFiles)
		}
	}
	for _, variant := range m.ObjectVariants {
		if len(variant.Members) != 0 {
			if variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o" {
				r := file.AddRule("linux_arm64_nvhe_object", variant.Target)
				r.SetAttr("object", variant.Object)
				r.SetAttr("mode", variant.Mode)
				r.SetAttr("tags", []string{"manual"})
				r.SetAttr("objects", localLabels(variant.Members))
				if opts.Schema.isV013() {
					r.SetAttr("content_id", variant.ContentID)
					if variant.CompileEnvironment == "" {
						return nil, fmt.Errorf("arm64 nVHE object %q has no compile environment", variant.Object)
					}
					r.SetAttr("compile_environment_index", ":"+compileEnvironmentIndexTarget)
					r.SetAttr("compile_environment_id", variant.CompileEnvironment)
					r.SetAttr("source_input_index", ":"+sourceInputIndexTarget)
					r.SetAttr("source_input_group", variant.SourceInputGroup)
				}
				if opts.Arch != "" {
					r.SetAttr("arch", opts.Arch)
				}
				if !opts.Schema.isV013() && opts.SourceConfig != "" {
					r.SetAttr("config", opts.SourceConfig)
				}
				if !opts.Schema.isV013() && opts.GeneratedHeaders != "" {
					r.SetAttr("generated_headers", opts.GeneratedHeaders)
				}
				if opts.Srcarch != "" {
					r.SetAttr("srcarch", opts.Srcarch)
				}
				if sourceTreeTarget != "" {
					r.SetAttr("source_tree_info", ":"+sourceTreeTarget)
				}
				if len(variant.ConfigFragment) != 0 && !opts.Schema.isV013() {
					r.SetAttr("config_fragment", variant.ConfigFragment)
				}
				continue
			}
			r := file.AddRule("linux_composite_object", variant.Target)
			r.SetAttr("object", variant.Object)
			r.SetAttr("mode", variant.Mode)
			r.SetAttr("tags", []string{"manual"})
			r.SetAttr("objects", localLabels(variant.Members))
			if opts.Schema.isV013() {
				r.SetAttr("content_id", variant.ContentID)
			}
			if opts.Arch != "" {
				r.SetAttr("arch", opts.Arch)
			}
			if len(variant.ConfigFragment) != 0 && !opts.Schema.isV013() {
				r.SetAttr("config_fragment", variant.ConfigFragment)
			}
			continue
		}
		emitSource := opts.SourceLabelPackage != "" && variant.sourceBuildReady()
		if opts.Schema.isV012() {
			if opts.SourceRootLabel == "" {
				return nil, fmt.Errorf(
					"cannot emit source-backed linux_object %q for %q: source root label is required",
					variant.Target,
					variant.Object,
				)
			}
			if opts.SourceLabelPackage == "" {
				return nil, fmt.Errorf(
					"cannot emit source-backed linux_object %q for %q: source label package is required",
					variant.Target,
					variant.Object,
				)
			}
			if reason := variant.sourceBuildError(); reason != "" {
				return nil, fmt.Errorf(
					"cannot emit source-backed linux_object %q for %q: %s",
					variant.Target,
					variant.Object,
					reason,
				)
			}
			emitSource = true
		}
		if emitSource && opts.SourceConfig == "" && !opts.Schema.isV013() {
			r := file.AddRule("linux_config", variant.Target+"_config")
			if opts.Schema.isV012() && opts.Arch != "" {
				r.SetAttr("arch", opts.Arch)
			}
			if len(variant.ConfigFragment) != 0 {
				r.SetAttr("config_flags", variant.ConfigFragment)
			}
		}
		r := file.AddRule("linux_object", variant.Target)
		r.SetAttr("object", variant.Object)
		r.SetAttr("mode", variant.Mode)
		r.SetAttr("tags", []string{"manual"})
		if opts.Schema.isV013() {
			r.SetAttr("content_id", variant.ContentID)
		}
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		if emitSource {
			if opts.Schema.isV013() {
				sourceFile, err := m.sourceFileIndex(variant.Source)
				if err != nil {
					return nil, fmt.Errorf("source-backed object %q: %w", variant.Object, err)
				}
				r.SetAttr("source_input_file", sourceFile)
				r.SetAttr("source_input_group", variant.SourceInputGroup)
				r.SetAttr("source_input_index", ":"+sourceInputIndexTarget)
			} else {
				r.SetAttr("src", labelForSource(opts, variant.Source))
			}
			var sourceInputPaths []string
			switch opts.Schema {
			case CompactSchemaV012:
				sourceInputPaths = variant.SourceIncludes
			}
			if opts.Schema == CompactSchemaV012 {
				r.SetAttr("source_includes_complete", true)
				if len(sourceInputPaths) != 0 {
					sourceIncludes := make([]string, 0, len(sourceInputPaths))
					for _, include := range sourceInputPaths {
						sourceIncludes = append(sourceIncludes, labelForSource(opts, include))
					}
					r.SetAttr("source_includes", sourceIncludes)
				}
			}
			if opts.Schema.isV013() {
				if variant.CompileEnvironment == "" {
					return nil, fmt.Errorf("source-backed object %q has no compile environment", variant.Object)
				}
				r.SetAttr("compile_environment_index", ":"+compileEnvironmentIndexTarget)
				r.SetAttr("compile_environment_id", variant.CompileEnvironment)
			} else if opts.SourceConfig != "" {
				r.SetAttr("config", opts.SourceConfig)
			} else {
				r.SetAttr("config", ":"+variant.Target+"_config")
			}
			if opts.Srcarch != "" {
				r.SetAttr("srcarch", opts.Srcarch)
			}
			if sourceTreeTarget != "" {
				r.SetAttr("source_tree_info", ":"+sourceTreeTarget)
			}
			if !opts.Schema.isV013() && opts.GeneratedHeaders != "" {
				r.SetAttr("generated_headers", opts.GeneratedHeaders)
			}
			if opts.SourceASN1Compiler != "" && strings.HasSuffix(variant.Object, ".asn1.o") {
				r.SetAttr("asn1_compiler", opts.SourceASN1Compiler)
			}
			if opts.SourceRelacheck != "" && strings.HasSuffix(variant.Object, ".pi.o") {
				r.SetAttr("relacheck", opts.SourceRelacheck)
			}
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
		if len(variant.ConfigFragment) != 0 && !opts.Schema.isV013() {
			r.SetAttr("config_fragment", variant.ConfigFragment)
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

func labelForSource(opts CompactBuildFileOptions, source string) string {
	if len(opts.SourceLabelPackages) != 0 {
		prefixes := make([]string, 0, len(opts.SourceLabelPackages))
		for prefix := range opts.SourceLabelPackages {
			prefix = strings.Trim(filepath.ToSlash(prefix), "/")
			if prefix != "" && prefix != "." {
				prefixes = append(prefixes, prefix)
			}
		}
		sort.Slice(prefixes, func(i, j int) bool {
			if len(prefixes[i]) == len(prefixes[j]) {
				return prefixes[i] < prefixes[j]
			}
			return len(prefixes[i]) > len(prefixes[j])
		})
		for _, prefix := range prefixes {
			if source != prefix && !strings.HasPrefix(source, prefix+"/") {
				continue
			}
			target := strings.TrimPrefix(source, prefix)
			target = strings.TrimPrefix(target, "/")
			return labelFor(opts.SourceLabelPackages[prefix], target)
		}
	}
	return labelFor(opts.SourceLabelPackage, source)
}

func (m *CompactMetadata) objectBuildFileNeedsConfig(opts CompactBuildFileOptions) bool {
	if opts.Schema.isV013() || opts.SourceLabelPackage == "" || opts.SourceConfig != "" {
		return false
	}
	for _, variant := range m.ObjectVariants {
		if variant.sourceBuildReady() {
			return true
		}
	}
	return false
}

func (m *CompactMetadata) objectBuildFileNeedsComposite() bool {
	for _, variant := range m.ObjectVariants {
		if len(variant.Members) != 0 {
			return true
		}
	}
	return false
}

func (m *CompactMetadata) objectBuildFileNeedsArm64Nvhe() bool {
	for _, variant := range m.ObjectVariants {
		if variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o" && len(variant.Members) != 0 {
			return true
		}
	}
	return false
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
			if _, ok := v.ConfigFragment[ref]; !ok {
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
			if _, ok := v.ConfigFragment[ref]; !ok {
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

func (m *CompactMetadata) ImageBuildFile(opts CompactImageBuildFileOptions) ([]byte, error) {
	if opts.Schema.isV013() {
		return m.compactDeltaImageBuildFile(opts)
	}
	visibility := opts.Visibility
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}
	file := buildgen.NewBuildFile("compact_images.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	file.AddLoad(compactRuleLoadLabel(opts.RuleLoadLabel), "linux_compact_image")
	file.AddPackage(visibility)
	canonicalImages := map[string]string{}
	for _, config := range m.Configs {
		key := objectTargetsKey(config.ObjectTargets)
		if opts.Schema.isV012() {
			key += "\x01" + objectTargetsKey(config.ModuleObjectTargets)
		}
		if canonical, ok := canonicalImages[key]; ok {
			r := file.AddRule("alias", config.ImageTarget)
			r.SetAttr("actual", ":"+canonical)
			r.SetAttr("tags", []string{"manual"})
			continue
		}
		canonicalImages[key] = config.ImageTarget

		labels := make([]string, len(config.ObjectTargets))
		for i, target := range config.ObjectTargets {
			labels[i] = labelFor(opts.ObjectLabelPackage, target)
		}
		r := file.AddRule("linux_compact_image", config.ImageTarget)
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		r.SetAttr("objects", labels)
		if opts.Schema.isV012() {
			moduleLabels := make([]string, len(config.ModuleObjectTargets))
			for i, target := range config.ModuleObjectTargets {
				moduleLabels[i] = labelFor(opts.ObjectLabelPackage, target)
			}
			if len(moduleLabels) != 0 {
				r.SetAttr("module_objects", moduleLabels)
			}
		} else if opts.RequireReal {
			r.SetAttr("require_real", true)
		}
		r.SetAttr("tags", []string{"manual"})
	}
	return file.Format(), nil
}

func (m *CompactMetadata) compactDeltaImageBuildFile(opts CompactImageBuildFileOptions) ([]byte, error) {
	if opts.BaseConfig == "" {
		return nil, fmt.Errorf("compact schema %s image BUILD emission requires a base config", opts.Schema)
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
	variants := make(map[string]CompactObjectVariant, len(m.ObjectVariants))
	for _, variant := range m.ObjectVariants {
		if variant.ContentID == "" {
			return nil, fmt.Errorf("object target %q has no v0.0.13 content ID", variant.Target)
		}
		variants[variant.Target] = variant
	}

	file := buildgen.NewBuildFile("compact_images.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	file.AddLoad(
		compactRuleLoadLabel(opts.RuleLoadLabel),
		"linux_compact_delta_image",
		"linux_compact_image",
	)
	file.AddPackage(visibility)
	emitBase := func(config CompactConfig) error {
		objectLabels, err := compactTargetLabels(config.ObjectTargets, variants, opts.ObjectLabelPackage)
		if err != nil {
			return err
		}
		moduleLabels, err := compactTargetLabels(config.ModuleObjectTargets, variants, opts.ObjectLabelPackage)
		if err != nil {
			return err
		}
		r := file.AddRule("linux_compact_image", config.ImageTarget)
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		r.SetAttr("objects", objectLabels)
		if len(moduleLabels) != 0 {
			r.SetAttr("module_objects", moduleLabels)
		}
		r.SetAttr("tags", []string{"manual"})
		return nil
	}
	if err := emitBase(base); err != nil {
		return nil, err
	}

	baseTargets := append(append([]string(nil), base.ObjectTargets...), base.ModuleObjectTargets...)
	baseSet := compactTargetSet(baseTargets)
	baseObjectIDs, err := compactContentIDs(base.ObjectTargets, variants)
	if err != nil {
		return nil, err
	}
	baseModuleIDs, err := compactContentIDs(base.ModuleObjectTargets, variants)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.Configs)-1)
	for _, config := range m.Configs {
		if config.Name != base.Name {
			names = append(names, config.Name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		config := configs[name]
		finalTargets := append(append([]string(nil), config.ObjectTargets...), config.ModuleObjectTargets...)
		finalSet := compactTargetSet(finalTargets)
		finalObjectIDs, err := compactContentIDs(config.ObjectTargets, variants)
		if err != nil {
			return nil, err
		}
		finalModuleIDs, err := compactContentIDs(config.ModuleObjectTargets, variants)
		if err != nil {
			return nil, err
		}
		addTargets := make([]string, 0)
		for _, target := range finalTargets {
			if !baseSet[target] {
				addTargets = appendUnique(addTargets, target)
			}
		}
		removeContentIDs := make([]string, 0)
		for _, target := range baseTargets {
			if finalSet[target] {
				continue
			}
			variant, ok := variants[target]
			if !ok {
				return nil, fmt.Errorf("compact image target %q is absent from object metadata", target)
			}
			removeContentIDs = appendUnique(removeContentIDs, variant.ContentID)
		}
		sort.Strings(removeContentIDs)
		if len(addTargets) == 0 &&
			len(removeContentIDs) == 0 &&
			stringSlicesEqual(baseObjectIDs, finalObjectIDs) &&
			stringSlicesEqual(baseModuleIDs, finalModuleIDs) {
			r := file.AddRule("alias", config.ImageTarget)
			r.SetAttr("actual", ":"+base.ImageTarget)
			r.SetAttr("tags", []string{"manual"})
			continue
		}
		addLabels, err := compactTargetLabels(addTargets, variants, opts.ObjectLabelPackage)
		if err != nil {
			return nil, err
		}
		r := file.AddRule("linux_compact_delta_image", config.ImageTarget)
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		r.SetAttr("base_image", ":"+base.ImageTarget)
		if len(addLabels) != 0 {
			r.SetAttr("add_objects", addLabels)
		}
		if len(removeContentIDs) != 0 {
			r.SetAttr("remove_content_ids", removeContentIDs)
		}
		r.SetAttr("ordered_content_ids", finalObjectIDs)
		r.SetAttr("ordered_module_content_ids", finalModuleIDs)
		r.SetAttr("tags", []string{"manual"})
	}
	return file.Format(), nil
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

func objectTargetsKey(targets []string) string {
	return strings.Join(targets, "\x00")
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
