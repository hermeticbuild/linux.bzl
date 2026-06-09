// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

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

	"linux.bzl/internal/kconfig/buildgen"
)

type NamedConfig struct {
	Name        string
	Flags       map[string]string
	AllNoConfig bool
}

type CompactMetadata struct {
	Configs        []CompactConfig        `json:"configs"`
	ObjectPackages []CompactObjectPackage `json:"object_packages,omitempty"`
	ObjectVariants []CompactObjectVariant `json:"object_variants"`
}

type CompactConfig struct {
	Name                string   `json:"name"`
	ImageTarget         string   `json:"image_target"`
	ObjectTargets       []string `json:"object_targets"`
	ModuleObjectTargets []string `json:"module_object_targets,omitempty"`
}

type CompactObjectVariant struct {
	Target         string            `json:"target"`
	Package        string            `json:"package,omitempty"`
	Object         string            `json:"object"`
	Source         string            `json:"source,omitempty"`
	Mode           string            `json:"mode"`
	ModName        string            `json:"modname,omitempty"`
	Flags          []string          `json:"flags,omitempty"`
	RemoveFlags    []string          `json:"remove_flags,omitempty"`
	ConfigFragment map[string]string `json:"config_fragment,omitempty"`
	Deps           []string          `json:"deps,omitempty"`
	Members        []string          `json:"members,omitempty"`
}

type CompactObjectPackage struct {
	Package       string   `json:"package"`
	ObjectTargets []string `json:"object_targets"`
}

type CompactBuildFileOptions struct {
	Arch                string
	Visibility          []string
	RuleLoadLabel       string
	SourceLabelPackage  string
	SourceLabelPackages map[string]string
	SourceASN1Compiler  string
	SourceRelacheck     string
	SourceRootLabel     string
	SourceTreeLabels    []string
	GeneratedHeaders    string
	SourceConfig        string
	Srcarch             string
}

type CompactImageBuildFileOptions struct {
	Arch               string
	Visibility         []string
	ObjectLabelPackage string
	RequireReal        bool
	RuleLoadLabel      string
}

type CompactMetadataOptions struct {
	ObjectDir   string
	SourceRoot  string
	SourceRoots map[string]string
	LibraryDirs []string
}

func (t *Tree) CompactMetadata(kb *KbuildFile, configs []NamedConfig) (*CompactMetadata, error) {
	return t.CompactMetadataWithOptions(kb, configs, CompactMetadataOptions{})
}

func MergeCompactMetadata(parts ...*CompactMetadata) (*CompactMetadata, error) {
	out := &CompactMetadata{}
	seenConfigs := map[string]bool{}
	variants := map[string]CompactObjectVariant{}
	for _, part := range parts {
		if part == nil {
			continue
		}
		for _, config := range part.Configs {
			if seenConfigs[config.Name] {
				return nil, fmt.Errorf("duplicate compact config name %q", config.Name)
			}
			seenConfigs[config.Name] = true
			out.Configs = append(out.Configs, config)
		}
		for _, variant := range part.ObjectVariants {
			if existing, ok := variants[variant.Target]; ok && !existing.equal(variant) {
				return nil, fmt.Errorf("object variants %q and %q produce duplicate target %q", existing.Object, variant.Object, variant.Target)
			}
			variants[variant.Target] = variant
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
	out.ObjectPackages = compactObjectPackages(out.ObjectVariants)
	return out, nil
}

func (t *Tree) CompactMetadataWithOptions(kb *KbuildFile, configs []NamedConfig, opts CompactMetadataOptions) (*CompactMetadata, error) {
	variants := map[string]CompactObjectVariant{}
	out := &CompactMetadata{}
	seenConfigs := map[string]bool{}
	seenImageTargets := map[string]string{}
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
		imageTarget := sanitizeTargetName(named.Name) + "_image"
		if existing := seenImageTargets[imageTarget]; existing != "" {
			return nil, fmt.Errorf("compact config names %q and %q produce duplicate image target %q", existing, named.Name, imageTarget)
		}
		seenImageTargets[imageTarget] = named.Name

		objects := kb.resolvedObjects(resolved)
		resolvedVariants := compactVariantMemo{}
		for _, object := range objects.all() {
			variant, err := resolvedVariants.variantFor(object.object, resolved, opts, objects)
			if err != nil {
				return nil, err
			}
			resolvedVariants[object.object] = variant
			if existing, ok := variants[variant.Target]; ok && !existing.equal(variant) {
				return nil, fmt.Errorf("object variants %q and %q produce duplicate target %q", existing.Object, variant.Object, variant.Target)
			}
			variants[variant.Target] = variant
		}

		variantsByTarget := map[string]CompactObjectVariant{}
		for _, variant := range resolvedVariants {
			variantsByTarget[variant.Target] = variant
		}

		targets := make([]string, 0, len(objects.roots))
		moduleTargets := make([]string, 0, len(objects.roots))
		appendRootTargets := func(roots []resolvedKbuildObject) error {
			for _, object := range roots {
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
	out.ObjectPackages = compactObjectPackages(out.ObjectVariants)
	sort.Slice(out.Configs, func(i, j int) bool { return out.Configs[i].Name < out.Configs[j].Name })
	return out, nil
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
	if v.Target != other.Target || v.Package != other.Package || v.Object != other.Object || v.Source != other.Source || v.Mode != other.Mode || v.ModName != other.ModName || len(v.Flags) != len(other.Flags) || len(v.RemoveFlags) != len(other.RemoveFlags) || len(v.ConfigFragment) != len(other.ConfigFragment) || len(v.Deps) != len(other.Deps) || len(v.Members) != len(other.Members) {
		return false
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

func (m *CompactMetadata) JSON() ([]byte, error) {
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

func (memo compactVariantMemo) variantFor(name string, config *ResolvedConfig, opts CompactMetadataOptions, objects resolvedKbuildObjects) (CompactObjectVariant, error) {
	return memo.variantForStack(name, config, opts, objects, map[string]bool{})
}

func (memo compactVariantMemo) variantForStack(name string, config *ResolvedConfig, opts CompactMetadataOptions, objects resolvedKbuildObjects, stack map[string]bool) (CompactObjectVariant, error) {
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
	for _, member := range object.members {
		variant, err := memo.variantForStack(member, config, opts, objects, stack)
		if err != nil {
			return CompactObjectVariant{}, err
		}
		members = append(members, variant.Target)
	}
	delete(stack, name)

	source := sourceForObject(opts.SourceRoot, opts.ObjectDir, object.object, opts.SourceRoots)
	if len(members) != 0 {
		source = ""
	}
	deps := []string{}
	if source != "" {
		for _, dep := range asn1HeaderDepsForSource(opts.SourceRoot, source) {
			if dep == name {
				continue
			}
			if _, ok := objects.byName[dep]; !ok {
				continue
			}
			variant, err := memo.variantForStack(dep, config, opts, objects, stack)
			if err != nil {
				return CompactObjectVariant{}, err
			}
			deps = appendUnique(deps, variant.Target)
		}
		sort.Strings(deps)
	}

	variant := object.variant(config, source, members, deps, opts.SourceRoot)
	memo[name] = variant
	return variant, nil
}

func (o resolvedKbuildObject) variant(config *ResolvedConfig, source string, members, deps []string, sourceRoot string) CompactObjectVariant {
	fragment := map[string]string{}
	refs := make([]string, 0, len(o.footprint))
	for ref := range o.footprint {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		fragment[ref] = config.Value(ref)
	}
	flags := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.flags, source), sourceRoot)
	remove := normalizeSourceRootFlags(filterResolvedKbuildFlags(o.remove, source), sourceRoot)
	hash := objectVariantHash(o.object, o.mode, o.modname, flags, remove, fragment, deps, members)
	return CompactObjectVariant{
		Target:         sanitizeTargetName(strings.TrimSuffix(o.object, ".o")) + "__" + hash,
		Package:        objectPackage(o.object),
		Object:         o.object,
		Source:         source,
		Mode:           o.mode,
		ModName:        o.modname,
		Flags:          flags,
		RemoveFlags:    remove,
		ConfigFragment: fragment,
		Deps:           append([]string(nil), deps...),
		Members:        append([]string(nil), members...),
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

func asn1HeaderDepsForSource(sourceRoot, source string) []string {
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
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		include, ok := quotedInclude(line)
		if !ok || !strings.HasSuffix(include, ".asn1.h") {
			continue
		}
		dep := strings.TrimSuffix(include, ".h") + ".o"
		dep = filepath.ToSlash(filepath.Join(sourceDir, dep))
		if seen[dep] {
			continue
		}
		seen[dep] = true
		out = append(out, dep)
	}
	sort.Strings(out)
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

func objectVariantHash(object, mode, modname string, flags, removeFlags []string, fragment map[string]string, deps, members []string) string {
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
	visibility := opts.Visibility
	if len(visibility) == 0 {
		visibility = []string{"//visibility:public"}
	}
	file := buildgen.NewBuildFile("compact_objects.BUILD.bazel", "# Generated by kconfig_parse compact backend. Do not edit.")
	loads := []string{"linux_object"}
	sourceTreeTarget := ""
	if opts.SourceRootLabel != "" || len(opts.SourceTreeLabels) != 0 {
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
	if sourceTreeTarget != "" {
		r := file.AddRule("linux_source_tree", sourceTreeTarget)
		r.SetAttr("tags", []string{"manual"})
		if opts.SourceRootLabel != "" {
			r.SetAttr("root", opts.SourceRootLabel)
		}
		if len(opts.SourceTreeLabels) != 0 {
			r.SetAttr("srcs", opts.SourceTreeLabels)
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
				if opts.Arch != "" {
					r.SetAttr("arch", opts.Arch)
				}
				if opts.SourceConfig != "" {
					r.SetAttr("config", opts.SourceConfig)
				}
				if opts.GeneratedHeaders != "" {
					r.SetAttr("generated_headers", opts.GeneratedHeaders)
				}
				if opts.Srcarch != "" {
					r.SetAttr("srcarch", opts.Srcarch)
				}
				if sourceTreeTarget != "" {
					r.SetAttr("source_tree_info", ":"+sourceTreeTarget)
				}
				if len(variant.ConfigFragment) != 0 {
					r.SetAttr("config_fragment", variant.ConfigFragment)
				}
				continue
			}
			r := file.AddRule("linux_composite_object", variant.Target)
			r.SetAttr("object", variant.Object)
			r.SetAttr("mode", variant.Mode)
			r.SetAttr("tags", []string{"manual"})
			r.SetAttr("objects", localLabels(variant.Members))
			if opts.Arch != "" {
				r.SetAttr("arch", opts.Arch)
			}
			if len(variant.ConfigFragment) != 0 {
				r.SetAttr("config_fragment", variant.ConfigFragment)
			}
			continue
		}
		emitSource := opts.SourceLabelPackage != "" && variant.sourceBuildReady()
		if emitSource && opts.SourceConfig == "" {
			r := file.AddRule("linux_config", variant.Target+"_config")
			if len(variant.ConfigFragment) != 0 {
				r.SetAttr("config_flags", variant.ConfigFragment)
			}
		}
		r := file.AddRule("linux_object", variant.Target)
		r.SetAttr("object", variant.Object)
		r.SetAttr("mode", variant.Mode)
		r.SetAttr("tags", []string{"manual"})
		if opts.Arch != "" {
			r.SetAttr("arch", opts.Arch)
		}
		if emitSource {
			r.SetAttr("src", labelForSource(opts, variant.Source))
			if opts.SourceConfig != "" {
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
			if opts.GeneratedHeaders != "" {
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
		if len(variant.ConfigFragment) != 0 {
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
	if opts.SourceLabelPackage == "" || opts.SourceConfig != "" {
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
	if v.Source == "" {
		return false
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
				return false
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
				return false
			}
		}
	}
	return true
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
		r.SetAttr("tags", []string{"manual"})
		if opts.RequireReal {
			r.SetAttr("require_real", true)
		}
	}
	return file.Format(), nil
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
