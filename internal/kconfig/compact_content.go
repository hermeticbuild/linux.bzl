package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
)

const (
	compactConfigPayloadDomain         = "linux-compact-config-payload-v1"
	compactCompileEnvironmentDomain    = "linux-compact-compile-environment-v2"
	compactGeneratedHeaderFamilyDomain = "linux-compact-generated-header-family-v1"
	compactObjectContentDomain         = "linux-compact-object-v1"
	compactShortIDLength               = 24
)

var compactContentSeparator = []byte{0}

type compactContentHasher struct {
	hash hash.Hash
}

func newCompactContentHasher(domain string) *compactContentHasher {
	hasher := &compactContentHasher{hash: sha256.New()}
	hasher.writeValue(domain)
	return hasher
}

func (h *compactContentHasher) writeValue(parts ...string) {
	for _, part := range parts {
		_, _ = h.hash.Write([]byte(part))
	}
	_, _ = h.hash.Write(compactContentSeparator)
}

func (h *compactContentHasher) id() string {
	return hex.EncodeToString(h.hash.Sum(nil))
}

func compactContentID(domain string, values ...string) string {
	hasher := newCompactContentHasher(domain)
	for _, value := range values {
		hasher.writeValue(value)
	}
	return hasher.id()
}

func compactShortID(id string) string {
	if len(id) <= compactShortIDLength {
		return id
	}
	return id[:compactShortIDLength]
}

func canonicalConfigContent(fragment map[string]string) string {
	keys := sortedConfigKeys(fragment)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key)
		out.WriteByte('=')
		out.WriteString(fragment[key])
		out.WriteByte('\n')
	}
	return out.String()
}

func newCompactConfigPayload(fragment map[string]string) CompactConfigPayload {
	copied := make(map[string]string, len(fragment))
	for key, value := range fragment {
		copied[key] = value
	}
	content := canonicalConfigContent(copied)
	return CompactConfigPayload{
		ID:       compactContentID(compactConfigPayloadDomain, content),
		Content:  content,
		fragment: copied,
	}
}

func compactFullConfigFragment(config *ResolvedConfig) map[string]string {
	fragment := map[string]string{}
	if config == nil {
		return fragment
	}
	for key := range config.Effective {
		if config.ShouldWrite(key) {
			fragment[key] = config.Value(key)
		}
	}
	return fragment
}

func newCompactGeneratedHeaderFamily(
	name string,
	configPayloadID string,
	label string,
	srcarch string,
	dependencies []string,
	sourceInputs []CompactSourceInput,
) CompactGeneratedHeaderFamily {
	dependencies = append([]string(nil), dependencies...)
	sort.Strings(dependencies)
	sourceInputs = append([]CompactSourceInput(nil), sourceInputs...)
	sort.Slice(sourceInputs, func(i, j int) bool {
		if sourceInputs[i].Path != sourceInputs[j].Path {
			return sourceInputs[i].Path < sourceInputs[j].Path
		}
		return sourceInputs[i].Digest < sourceInputs[j].Digest
	})
	id := compactGeneratedHeaderFamilyContentID(
		name,
		configPayloadID,
		srcarch,
		dependencies,
		sourceInputs,
	)
	return CompactGeneratedHeaderFamily{
		ID:            id,
		Name:          name,
		ConfigPayload: configPayloadID,
		Labels:        []string{label},
		Srcarch:       srcarch,
		Dependencies:  dependencies,
		sourceInputs:  sourceInputs,
	}
}

func compactGeneratedHeaderFamilyContentID(
	name string,
	configPayloadID string,
	srcarch string,
	dependencies []string,
	sourceInputs []CompactSourceInput,
) string {
	dependencies = append([]string(nil), dependencies...)
	sort.Strings(dependencies)
	sourceInputs = append([]CompactSourceInput(nil), sourceInputs...)
	sort.Slice(sourceInputs, func(i, j int) bool {
		if sourceInputs[i].Path != sourceInputs[j].Path {
			return sourceInputs[i].Path < sourceInputs[j].Path
		}
		return sourceInputs[i].Digest < sourceInputs[j].Digest
	})
	hasher := newCompactContentHasher(compactGeneratedHeaderFamilyDomain)
	hasher.writeValue("name=", name)
	hasher.writeValue("srcarch=", srcarch)
	hasher.writeValue("config_payload=", configPayloadID)
	for _, dependency := range dependencies {
		hasher.writeValue("dependency=", dependency)
	}
	for _, input := range sourceInputs {
		hasher.writeValue("source_input=", input.Path, "\x00", input.Digest)
	}
	return hasher.id()
}

func newCompactCompileEnvironment(abi, configPayloadID string, generatedHeaderFamilyIDs []string) CompactCompileEnvironment {
	generatedHeaderFamilyIDs = append([]string(nil), generatedHeaderFamilyIDs...)
	sort.Strings(generatedHeaderFamilyIDs)
	hasher := newCompactContentHasher(compactCompileEnvironmentDomain)
	hasher.writeValue("abi=", abi)
	hasher.writeValue("config_payload=", configPayloadID)
	for _, familyID := range generatedHeaderFamilyIDs {
		hasher.writeValue("generated_header_family=", familyID)
	}
	return CompactCompileEnvironment{
		ID:                      hasher.id(),
		ABI:                     abi,
		ConfigPayload:           configPayloadID,
		GeneratedHeaderFamilies: generatedHeaderFamilyIDs,
	}
}

func objectVariantContentID(
	object string,
	mode string,
	modname string,
	flags []string,
	removeFlags []string,
	compileEnvironmentID string,
	source string,
	sourceInputs []CompactSourceInput,
	depContentIDs []string,
	memberContentIDs []string,
	abi string,
) string {
	hasher := newCompactContentHasher(compactObjectContentDomain)
	hasher.writeValue("object=", object)
	hasher.writeValue("mode=", mode)
	hasher.writeValue("modname=", modname)
	hasher.writeValue("compile_environment=", compileEnvironmentID)
	hasher.writeValue("abi=", abi)
	hasher.writeValue("source=", source)
	for _, flag := range flags {
		hasher.writeValue("flag=", flag)
	}
	for _, flag := range removeFlags {
		hasher.writeValue("remove_flag=", flag)
	}
	for _, input := range sourceInputs {
		hasher.writeValue("source_input=", input.Path, "\x00", input.Digest)
	}
	for _, contentID := range depContentIDs {
		hasher.writeValue("dep_content_id=", contentID)
	}
	for _, contentID := range memberContentIDs {
		hasher.writeValue("member_content_id=", contentID)
	}
	return hasher.id()
}

func compactCompileEnvironmentValue(environment CompactCompileEnvironment) string {
	var out strings.Builder
	out.WriteString(`{"abi":`)
	out.WriteString(fmt.Sprintf("%q", environment.ABI))
	out.WriteString(`,"config_payload":`)
	out.WriteString(fmt.Sprintf("%q", environment.ConfigPayload))
	out.WriteString(`,"generated_header_families":[`)
	for i, id := range environment.GeneratedHeaderFamilies {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(fmt.Sprintf("%q", id))
	}
	out.WriteString(`]}`)
	return out.String()
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]bool, len(values)+len(additions))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func appendUniqueSourceInputs(values []CompactSourceInput, additions ...CompactSourceInput) []CompactSourceInput {
	byPath := make(map[string]CompactSourceInput, len(values)+len(additions))
	for _, value := range values {
		byPath[value.Path] = value
	}
	for _, value := range additions {
		if existing, ok := byPath[value.Path]; ok && existing.Digest != value.Digest {
			// A logical source-tree path resolves deterministically within one scan.
			// Preserve the existing value; validateContentIDs still makes the
			// resulting object identity stable.
			continue
		}
		byPath[value.Path] = value
	}
	out := make([]CompactSourceInput, 0, len(byPath))
	for _, value := range byPath {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

func encodeCompactSourceInputGroup(indices []int) string {
	parts := make([]string, len(indices))
	for i, index := range indices {
		parts[i] = strconv.Itoa(index)
	}
	return strings.Join(parts, ",")
}

func decodeCompactSourceInputGroup(encoded string, fileCount int) ([]int, error) {
	if encoded == "" {
		return nil, fmt.Errorf("source input group is empty")
	}
	parts := strings.Split(encoded, ",")
	indices := make([]int, 0, len(parts))
	previous := 0
	for _, part := range parts {
		index, err := strconv.Atoi(part)
		if err != nil || index <= 0 || strconv.Itoa(index) != part {
			return nil, fmt.Errorf("source input group %q has invalid file index %q", encoded, part)
		}
		if index > fileCount {
			return nil, fmt.Errorf(
				"source input group %q file index %d is out of range for %d files",
				encoded,
				index,
				fileCount,
			)
		}
		if index <= previous {
			return nil, fmt.Errorf(
				"source input group %q has duplicate or non-canonical file index %d",
				encoded,
				index,
			)
		}
		previous = index
		indices = append(indices, index)
	}
	return indices, nil
}

func validateCompactSourceInput(input CompactSourceInput, context string) error {
	if input.Path == "" {
		return fmt.Errorf("%s has an empty source path", context)
	}
	if len(input.Digest) != sha256.Size*2 {
		return fmt.Errorf("%s source %q has invalid SHA-256 digest %q", context, input.Path, input.Digest)
	}
	if _, err := hex.DecodeString(input.Digest); err != nil {
		return fmt.Errorf("%s source %q has invalid SHA-256 digest %q: %w", context, input.Path, input.Digest, err)
	}
	return nil
}

type compactSourceInputInterner struct {
	files        []CompactSourceInput
	fileIndices  map[string]int
	groups       []string
	groupIndices map[string]int
}

func newCompactSourceInputInterner() *compactSourceInputInterner {
	return &compactSourceInputInterner{
		fileIndices:  map[string]int{},
		groupIndices: map[string]int{},
	}
}

func (interner *compactSourceInputInterner) intern(
	inputs []CompactSourceInput,
	context string,
) (int, error) {
	if len(inputs) == 0 {
		return 0, nil
	}
	canonical, err := canonicalCompactSourceInputs(inputs, context)
	if err != nil {
		return 0, err
	}
	indices := make([]int, 0, len(canonical))
	for _, input := range canonical {
		index, ok := interner.fileIndices[input.Path]
		if ok {
			existing := interner.files[index-1]
			if existing.Digest != input.Digest {
				return 0, fmt.Errorf(
					"source path %q has conflicting digests %q and %q",
					input.Path,
					existing.Digest,
					input.Digest,
				)
			}
		} else {
			interner.files = append(interner.files, input)
			index = len(interner.files)
			interner.fileIndices[input.Path] = index
		}
		indices = append(indices, index)
	}
	sort.Ints(indices)
	encoded := encodeCompactSourceInputGroup(indices)
	if group, ok := interner.groupIndices[encoded]; ok {
		return group, nil
	}
	interner.groups = append(interner.groups, encoded)
	group := len(interner.groups)
	interner.groupIndices[encoded] = group
	return group, nil
}

func (interner *compactSourceInputInterner) apply(metadata *CompactMetadata) {
	metadata.SourceFiles = append([]CompactSourceInput(nil), interner.files...)
	metadata.SourceInputGroups = append([]string(nil), interner.groups...)
}

func (metadata *CompactMetadata) expandedSourceInputGroup(
	group int,
	context string,
) ([]CompactSourceInput, error) {
	if group == 0 {
		return nil, nil
	}
	if group < 0 || group > len(metadata.SourceInputGroups) {
		return nil, fmt.Errorf(
			"%s references source input group %d, out of range 1..%d",
			context,
			group,
			len(metadata.SourceInputGroups),
		)
	}
	indices, err := decodeCompactSourceInputGroup(
		metadata.SourceInputGroups[group-1],
		len(metadata.SourceFiles),
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	out := make([]CompactSourceInput, 0, len(indices))
	for _, index := range indices {
		out = append(out, metadata.SourceFiles[index-1])
	}
	return out, nil
}

func (metadata *CompactMetadata) sourceFileIndex(path string) (int, error) {
	index := sort.Search(len(metadata.SourceFiles), func(i int) bool {
		return metadata.SourceFiles[i].Path >= path
	})
	if index == len(metadata.SourceFiles) || metadata.SourceFiles[index].Path != path {
		return 0, fmt.Errorf("source file %q is missing from source_files", path)
	}
	return index + 1, nil
}

func canonicalCompactSourceInputs(inputs []CompactSourceInput, context string) ([]CompactSourceInput, error) {
	out := append([]CompactSourceInput(nil), inputs...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	for i, input := range out {
		if err := validateCompactSourceInput(input, context); err != nil {
			return nil, err
		}
		if i != 0 && out[i-1].Path == input.Path {
			return nil, fmt.Errorf("%s repeats source path %q", context, input.Path)
		}
	}
	return out, nil
}

func (metadata *CompactMetadata) canonicalizeSourceInputIndex() error {
	if metadata == nil {
		return nil
	}
	for _, family := range metadata.GeneratedHeaderFamilies {
		if len(family.sourceInputs) != 0 {
			return fmt.Errorf("generated header family %q retains internal inline source inputs", family.ID)
		}
	}
	for _, variant := range metadata.ObjectVariants {
		if len(variant.sourceInputs) != 0 {
			return fmt.Errorf("object target %q retains internal inline source inputs", variant.Target)
		}
	}

	originalFiles := append([]CompactSourceInput(nil), metadata.SourceFiles...)
	metadata.SourceFiles = append([]CompactSourceInput(nil), originalFiles...)
	sort.Slice(metadata.SourceFiles, func(i, j int) bool {
		return metadata.SourceFiles[i].Path < metadata.SourceFiles[j].Path
	})
	newFileIndices := make(map[string]int, len(metadata.SourceFiles))
	for i, input := range metadata.SourceFiles {
		if err := validateCompactSourceInput(input, "source_files"); err != nil {
			return err
		}
		if _, exists := newFileIndices[input.Path]; exists {
			return fmt.Errorf("source_files repeats source path %q", input.Path)
		}
		newFileIndices[input.Path] = i + 1
	}

	canonicalGroups := make([]string, len(metadata.SourceInputGroups))
	groupSet := make(map[string]bool, len(metadata.SourceInputGroups))
	for i, encoded := range metadata.SourceInputGroups {
		oldIndices, err := decodeCompactSourceInputGroup(encoded, len(originalFiles))
		if err != nil {
			return fmt.Errorf("source input group %d: %w", i+1, err)
		}
		newIndices := make([]int, 0, len(oldIndices))
		for _, oldIndex := range oldIndices {
			newIndices = append(newIndices, newFileIndices[originalFiles[oldIndex-1].Path])
		}
		sort.Ints(newIndices)
		canonicalGroups[i] = encodeCompactSourceInputGroup(newIndices)
		groupSet[canonicalGroups[i]] = true
	}
	metadata.SourceInputGroups = make([]string, 0, len(groupSet))
	for encoded := range groupSet {
		metadata.SourceInputGroups = append(metadata.SourceInputGroups, encoded)
	}
	sort.Strings(metadata.SourceInputGroups)
	newGroupIndices := make(map[string]int, len(metadata.SourceInputGroups))
	for i, encoded := range metadata.SourceInputGroups {
		newGroupIndices[encoded] = i + 1
	}
	remapGroup := func(group int, context string) (int, error) {
		if group == 0 {
			return 0, nil
		}
		if group < 0 || group > len(canonicalGroups) {
			return 0, fmt.Errorf(
				"%s references source input group %d, out of range 1..%d",
				context,
				group,
				len(canonicalGroups),
			)
		}
		return newGroupIndices[canonicalGroups[group-1]], nil
	}
	for i := range metadata.GeneratedHeaderFamilies {
		group, err := remapGroup(
			metadata.GeneratedHeaderFamilies[i].SourceInputGroup,
			fmt.Sprintf("generated header family %q", metadata.GeneratedHeaderFamilies[i].ID),
		)
		if err != nil {
			return err
		}
		metadata.GeneratedHeaderFamilies[i].SourceInputGroup = group
	}
	for i := range metadata.ObjectVariants {
		group, err := remapGroup(
			metadata.ObjectVariants[i].SourceInputGroup,
			fmt.Sprintf("object target %q", metadata.ObjectVariants[i].Target),
		)
		if err != nil {
			return err
		}
		metadata.ObjectVariants[i].SourceInputGroup = group
	}
	return metadata.validateSourceInputIndex()
}

func (metadata *CompactMetadata) validateSourceInputIndex() error {
	if metadata == nil {
		return nil
	}
	for i, input := range metadata.SourceFiles {
		if err := validateCompactSourceInput(input, fmt.Sprintf("source file %d", i+1)); err != nil {
			return err
		}
		if i != 0 && metadata.SourceFiles[i-1].Path >= input.Path {
			return fmt.Errorf(
				"source files are duplicate or not canonically ordered at %q",
				input.Path,
			)
		}
	}

	groupFiles := make([]map[int]bool, len(metadata.SourceInputGroups))
	for i, encoded := range metadata.SourceInputGroups {
		if i != 0 && metadata.SourceInputGroups[i-1] >= encoded {
			return fmt.Errorf("source input groups are duplicate or not canonically ordered at %q", encoded)
		}
		indices, err := decodeCompactSourceInputGroup(encoded, len(metadata.SourceFiles))
		if err != nil {
			return fmt.Errorf("source input group %d: %w", i+1, err)
		}
		groupFiles[i] = make(map[int]bool, len(indices))
		for _, index := range indices {
			groupFiles[i][index] = true
		}
	}

	referencedGroups := map[int]bool{}
	referencedFiles := map[int]bool{}
	validateReference := func(group int, context string, requiredPath string) error {
		if group <= 0 || group > len(groupFiles) {
			return fmt.Errorf(
				"%s references source input group %d, out of range 1..%d",
				context,
				group,
				len(groupFiles),
			)
		}
		referencedGroups[group] = true
		for index := range groupFiles[group-1] {
			referencedFiles[index] = true
		}
		if requiredPath == "" {
			return nil
		}
		fileIndex := sort.Search(len(metadata.SourceFiles), func(i int) bool {
			return metadata.SourceFiles[i].Path >= requiredPath
		})
		if fileIndex == len(metadata.SourceFiles) || metadata.SourceFiles[fileIndex].Path != requiredPath {
			return fmt.Errorf("%s source file %q is missing from source_files", context, requiredPath)
		}
		if !groupFiles[group-1][fileIndex+1] {
			return fmt.Errorf("%s source input group %d omits primary source %q", context, group, requiredPath)
		}
		return nil
	}

	for _, family := range metadata.GeneratedHeaderFamilies {
		if len(family.sourceInputs) != 0 {
			return fmt.Errorf(
				"generated header family %q retains internal inline source inputs",
				family.ID,
			)
		}
		if family.SourceInputGroup == 0 {
			continue
		}
		if err := validateReference(
			family.SourceInputGroup,
			fmt.Sprintf("generated header family %q", family.ID),
			"",
		); err != nil {
			return err
		}
	}
	for _, variant := range metadata.ObjectVariants {
		if len(variant.sourceInputs) != 0 {
			return fmt.Errorf("object target %q retains internal inline source inputs", variant.Target)
		}
		exactAction := len(variant.Members) == 0 || isArm64NvheObject(variant.Object)
		if !exactAction {
			if variant.SourceInputGroup != 0 {
				return fmt.Errorf(
					"composite object target %q unexpectedly references source input group %d",
					variant.Target,
					variant.SourceInputGroup,
				)
			}
			continue
		}
		requiredPath := variant.Source
		if isArm64NvheObject(variant.Object) {
			requiredPath = "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"
		}
		if err := validateReference(
			variant.SourceInputGroup,
			fmt.Sprintf("object target %q", variant.Target),
			requiredPath,
		); err != nil {
			return err
		}
	}
	for group := range metadata.SourceInputGroups {
		if !referencedGroups[group+1] {
			return fmt.Errorf("source input group %d is not referenced", group+1)
		}
	}
	for file := range metadata.SourceFiles {
		if !referencedFiles[file+1] {
			return fmt.Errorf("source file %d %q is not referenced by any group", file+1, metadata.SourceFiles[file].Path)
		}
	}
	return nil
}

func sortedCompactConfigPayloads(values map[string]CompactConfigPayload) []CompactConfigPayload {
	ids := sortedCompactIDs(values)
	out := make([]CompactConfigPayload, 0, len(ids))
	for _, id := range ids {
		out = append(out, values[id])
	}
	return out
}

func sortedCompactCompileEnvironments(values map[string]CompactCompileEnvironment) []CompactCompileEnvironment {
	ids := sortedCompactIDs(values)
	out := make([]CompactCompileEnvironment, 0, len(ids))
	for _, id := range ids {
		out = append(out, values[id])
	}
	return out
}

func sortedCompactGeneratedHeaderFamilies(values map[string]CompactGeneratedHeaderFamily) []CompactGeneratedHeaderFamily {
	ids := sortedCompactIDs(values)
	out := make([]CompactGeneratedHeaderFamily, 0, len(ids))
	for _, id := range ids {
		family := values[id]
		sort.Strings(family.Labels)
		out = append(out, family)
	}
	return out
}

func sortedCompactIDs[T any](values map[string]T) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (metadata *CompactMetadata) validateContentIDs() error {
	if metadata == nil {
		return nil
	}
	if err := metadata.validateSourceInputIndex(); err != nil {
		return err
	}
	fullIDs := map[string]string{}
	shortIDs := map[string]string{}
	check := func(kind, id string) error {
		if len(id) != sha256.Size*2 {
			return fmt.Errorf("%s content ID %q is not a full SHA-256 digest", kind, id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			return fmt.Errorf("%s content ID %q is not hexadecimal: %w", kind, id, err)
		}
		if existingKind, ok := fullIDs[id]; ok {
			return fmt.Errorf("%s and %s duplicate content ID %q", existingKind, kind, id)
		}
		fullIDs[id] = kind
		short := compactShortID(id)
		if existing, ok := shortIDs[short]; ok && existing != id {
			return fmt.Errorf("content IDs %q and %q collide at short ID %q", existing, id, short)
		}
		shortIDs[short] = id
		return nil
	}
	payloads := make(map[string]CompactConfigPayload, len(metadata.ConfigPayloads))
	for _, payload := range metadata.ConfigPayloads {
		if err := check("config payload", payload.ID); err != nil {
			return err
		}
		if payload.fragment != nil {
			canonical := canonicalConfigContent(payload.fragment)
			if payload.Content != canonical {
				return fmt.Errorf(
					"config payload %s content does not match its canonical fragment",
					payload.ID,
				)
			}
		}
		expected := compactContentID(compactConfigPayloadDomain, payload.Content)
		if payload.ID != expected {
			return fmt.Errorf("config payload %s canonical content hashes to %s", payload.ID, expected)
		}
		payloads[payload.ID] = payload
	}
	generatedHeaderFamilies := make(map[string]CompactGeneratedHeaderFamily, len(metadata.GeneratedHeaderFamilies))
	for _, family := range metadata.GeneratedHeaderFamilies {
		if err := check("generated header family", family.ID); err != nil {
			return err
		}
		if family.Name == "" || family.Srcarch == "" || len(family.Labels) == 0 {
			return fmt.Errorf("generated header family %s has incomplete metadata", family.ID)
		}
		for i, label := range family.Labels {
			if strings.TrimSpace(label) == "" {
				return fmt.Errorf("generated header family %s has an empty label", family.ID)
			}
			if i != 0 && family.Labels[i-1] >= label {
				return fmt.Errorf(
					"generated header family %s has duplicate or non-canonical labels",
					family.ID,
				)
			}
		}
		for i, dependency := range family.Dependencies {
			if dependency == family.ID {
				return fmt.Errorf("generated header family %s depends on itself", family.ID)
			}
			if i != 0 && family.Dependencies[i-1] >= dependency {
				return fmt.Errorf(
					"generated header family %s has duplicate or non-canonical dependencies",
					family.ID,
				)
			}
		}
		if _, ok := payloads[family.ConfigPayload]; !ok {
			return fmt.Errorf(
				"generated header family %s references unknown config payload %s",
				family.ID,
				family.ConfigPayload,
			)
		}
		inputs, err := metadata.expandedSourceInputGroup(
			family.SourceInputGroup,
			fmt.Sprintf("generated header family %q", family.ID),
		)
		if err != nil {
			return err
		}
		expected := compactGeneratedHeaderFamilyContentID(
			family.Name,
			family.ConfigPayload,
			family.Srcarch,
			family.Dependencies,
			inputs,
		)
		if family.ID != expected {
			return fmt.Errorf(
				"generated header family %s canonical inputs hash to %s",
				family.ID,
				expected,
			)
		}
		generatedHeaderFamilies[family.ID] = family
	}
	for _, family := range metadata.GeneratedHeaderFamilies {
		for _, dependency := range family.Dependencies {
			if _, ok := generatedHeaderFamilies[dependency]; !ok {
				return fmt.Errorf(
					"generated header family %s references unknown dependency %s",
					family.ID,
					dependency,
				)
			}
		}
	}
	familyStates := map[string]uint8{}
	var validateFamilyDependencies func(string) error
	validateFamilyDependencies = func(familyID string) error {
		switch familyStates[familyID] {
		case 1:
			return fmt.Errorf("generated header family dependency cycle at %s", familyID)
		case 2:
			return nil
		}
		familyStates[familyID] = 1
		for _, dependency := range generatedHeaderFamilies[familyID].Dependencies {
			if err := validateFamilyDependencies(dependency); err != nil {
				return err
			}
		}
		familyStates[familyID] = 2
		return nil
	}
	for familyID := range generatedHeaderFamilies {
		if err := validateFamilyDependencies(familyID); err != nil {
			return err
		}
	}
	environments := make(map[string]CompactCompileEnvironment, len(metadata.CompileEnvironments))
	abi := ""
	for _, environment := range metadata.CompileEnvironments {
		if err := check("compile environment", environment.ID); err != nil {
			return err
		}
		if _, ok := payloads[environment.ConfigPayload]; !ok {
			return fmt.Errorf(
				"compile environment %s references unknown config payload %s",
				environment.ID,
				environment.ConfigPayload,
			)
		}
		familyNames := map[string]bool{}
		for i, familyID := range environment.GeneratedHeaderFamilies {
			family, ok := generatedHeaderFamilies[familyID]
			if !ok {
				return fmt.Errorf(
					"compile environment %s references unknown generated header family %s",
					environment.ID,
					familyID,
				)
			}
			if i != 0 && environment.GeneratedHeaderFamilies[i-1] >= familyID {
				return fmt.Errorf(
					"compile environment %s has duplicate or non-canonical generated header families",
					environment.ID,
				)
			}
			if familyNames[family.Name] {
				return fmt.Errorf(
					"compile environment %s repeats generated header family %q",
					environment.ID,
					family.Name,
				)
			}
			familyNames[family.Name] = true
		}
		if familyNames[compactGeneratedHeaderFamilyAll] && len(familyNames) != 1 {
			return fmt.Errorf(
				"compile environment %s mixes all with precise generated header families",
				environment.ID,
			)
		}
		expected := newCompactCompileEnvironment(
			environment.ABI,
			environment.ConfigPayload,
			environment.GeneratedHeaderFamilies,
		).ID
		if environment.ID != expected {
			return fmt.Errorf("compile environment %s canonical fields hash to %s", environment.ID, expected)
		}
		if abi == "" {
			abi = environment.ABI
		} else if abi != environment.ABI {
			return fmt.Errorf("compile environments use ABIs %q and %q", abi, environment.ABI)
		}
		environments[environment.ID] = environment
	}

	variants := make(map[string]CompactObjectVariant, len(metadata.ObjectVariants))
	for _, variant := range metadata.ObjectVariants {
		if err := check("object", variant.ContentID); err != nil {
			return err
		}
		if _, ok := variants[variant.Target]; ok {
			return fmt.Errorf("duplicate object target %q", variant.Target)
		}
		variants[variant.Target] = variant
	}
	for _, variant := range metadata.ObjectVariants {
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			fmt.Sprintf("object target %q", variant.Target),
		)
		if err != nil {
			return err
		}
		depContentIDs := make([]string, 0, len(variant.Deps))
		for i, target := range variant.Deps {
			if i != 0 && variant.Deps[i-1] >= target {
				return fmt.Errorf(
					"object target %q has duplicate or non-canonical dependencies",
					variant.Target,
				)
			}
			dependency, ok := variants[target]
			if !ok {
				return fmt.Errorf("object target %q references unknown dependency %q", variant.Target, target)
			}
			depContentIDs = append(depContentIDs, dependency.ContentID)
		}
		sort.Strings(depContentIDs)
		memberContentIDs := make([]string, 0, len(variant.Members))
		for _, target := range variant.Members {
			member, ok := variants[target]
			if !ok {
				return fmt.Errorf("object target %q references unknown member %q", variant.Target, target)
			}
			memberContentIDs = append(memberContentIDs, member.ContentID)
		}
		if variant.CompileEnvironment != "" {
			if _, ok := environments[variant.CompileEnvironment]; !ok {
				return fmt.Errorf(
					"object target %q references unknown compile environment %s",
					variant.Target,
					variant.CompileEnvironment,
				)
			}
		}
		expected := objectVariantContentID(
			variant.Object,
			variant.Mode,
			variant.ModName,
			variant.Flags,
			variant.RemoveFlags,
			variant.CompileEnvironment,
			variant.Source,
			inputs,
			depContentIDs,
			memberContentIDs,
			abi,
		)
		if variant.ContentID != expected {
			return fmt.Errorf("object target %q canonical fields hash to %s, got %s", variant.Target, expected, variant.ContentID)
		}
		if !strings.HasSuffix(variant.Target, "__"+compactShortID(variant.ContentID)) {
			return fmt.Errorf(
				"object target %q does not end in collision-checked short content ID %q",
				variant.Target,
				compactShortID(variant.ContentID),
			)
		}
	}
	return nil
}
