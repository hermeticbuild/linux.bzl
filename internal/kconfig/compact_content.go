package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	compactConfigPayloadDomain      = "linux-compact-config-payload-v1"
	compactCompileEnvironmentDomain = "linux-compact-compile-environment-v1"
	compactHeaderGroupDomain        = "linux-compact-generated-headers-v1"
	compactObjectContentDomain      = "linux-compact-object-v1"
	compactShortIDLength            = 24
)

func compactContentID(domain string, values ...string) string {
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write([]byte{0})
	for _, value := range values {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
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
		Fragment: copied,
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

func newCompactHeaderGroup(configPayloadID, label, srcarch, footprint string, sourceInputs []CompactSourceInput) CompactHeaderGroup {
	values := []string{srcarch, configPayloadID, footprint}
	for _, input := range sourceInputs {
		values = append(values, input.Path+"\x00"+input.Digest)
	}
	return CompactHeaderGroup{
		ID:            compactContentID(compactHeaderGroupDomain, values...),
		ConfigPayload: configPayloadID,
		Labels:        compactOptionalString(label),
		Srcarch:       srcarch,
		Footprint:     footprint,
		SourceInputs:  append([]CompactSourceInput(nil), sourceInputs...),
	}
}

func newCompactCompileEnvironment(abi, configPayloadID string, headerGroupIDs []string) CompactCompileEnvironment {
	headerGroupIDs = append([]string(nil), headerGroupIDs...)
	sort.Strings(headerGroupIDs)
	values := append([]string{abi, configPayloadID}, headerGroupIDs...)
	return CompactCompileEnvironment{
		ID:            compactContentID(compactCompileEnvironmentDomain, values...),
		ABI:           abi,
		ConfigPayload: configPayloadID,
		HeaderGroups:  headerGroupIDs,
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
	values := []string{
		"object=" + object,
		"mode=" + mode,
		"modname=" + modname,
		"compile_environment=" + compileEnvironmentID,
		"abi=" + abi,
		"source=" + source,
	}
	for _, flag := range flags {
		values = append(values, "flag="+flag)
	}
	for _, flag := range removeFlags {
		values = append(values, "remove_flag="+flag)
	}
	for _, input := range sourceInputs {
		values = append(values, "source_input="+input.Path+"\x00"+input.Digest)
	}
	for _, contentID := range depContentIDs {
		values = append(values, "dep_content_id="+contentID)
	}
	for _, contentID := range memberContentIDs {
		values = append(values, "member_content_id="+contentID)
	}
	return compactContentID(compactObjectContentDomain, values...)
}

func compactCompileEnvironmentValue(environment CompactCompileEnvironment) string {
	var out strings.Builder
	out.WriteString(`{"abi":`)
	out.WriteString(fmt.Sprintf("%q", environment.ABI))
	out.WriteString(`,"config_payload":`)
	out.WriteString(fmt.Sprintf("%q", environment.ConfigPayload))
	out.WriteString(`,"header_groups":[`)
	for i, id := range environment.HeaderGroups {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(fmt.Sprintf("%q", id))
	}
	out.WriteString(`]}`)
	return out.String()
}

func compactOptionalString(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
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

func compactSourceInputsEqual(left, right []CompactSourceInput) bool {
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
	inline []CompactSourceInput,
	context string,
) ([]CompactSourceInput, error) {
	if group != 0 && len(inline) != 0 {
		return nil, fmt.Errorf("%s has both source_input_group and inline source_inputs", context)
	}
	if group == 0 {
		return append([]CompactSourceInput(nil), inline...), nil
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
	if metadata == nil || !metadata.Schema.isV013() {
		return nil
	}

	originalFiles := append([]CompactSourceInput(nil), metadata.SourceFiles...)
	filesByPath := map[string]CompactSourceInput{}
	for _, input := range originalFiles {
		if err := validateCompactSourceInput(input, "source_files"); err != nil {
			return err
		}
		if _, exists := filesByPath[input.Path]; exists {
			return fmt.Errorf("source_files repeats source path %q", input.Path)
		}
		filesByPath[input.Path] = input
	}
	addInlineInputs := func(inputs []CompactSourceInput, context string) error {
		canonical, err := canonicalCompactSourceInputs(inputs, context)
		if err != nil {
			return err
		}
		for _, input := range canonical {
			if existing, ok := filesByPath[input.Path]; ok && existing.Digest != input.Digest {
				return fmt.Errorf(
					"source path %q has conflicting digests %q and %q",
					input.Path,
					existing.Digest,
					input.Digest,
				)
			}
			filesByPath[input.Path] = input
		}
		return nil
	}

	referencedOldGroups := map[int]bool{}
	scanReference := func(group int, inline []CompactSourceInput, context string) error {
		if group != 0 && len(inline) != 0 {
			return fmt.Errorf("%s has both source_input_group and inline source_inputs", context)
		}
		if group != 0 {
			if group < 0 || group > len(metadata.SourceInputGroups) {
				return fmt.Errorf(
					"%s references source input group %d, out of range 1..%d",
					context,
					group,
					len(metadata.SourceInputGroups),
				)
			}
			referencedOldGroups[group] = true
			return nil
		}
		return addInlineInputs(inline, context)
	}
	for _, group := range metadata.HeaderGroups {
		if err := scanReference(
			group.SourceInputGroup,
			group.SourceInputs,
			fmt.Sprintf("header group %q", group.ID),
		); err != nil {
			return err
		}
	}
	for _, variant := range metadata.ObjectVariants {
		if err := scanReference(
			variant.SourceInputGroup,
			variant.SourceInputs,
			fmt.Sprintf("object target %q", variant.Target),
		); err != nil {
			return err
		}
	}
	for group := range metadata.SourceInputGroups {
		if !referencedOldGroups[group+1] {
			return fmt.Errorf("source input group %d is not referenced", group+1)
		}
	}

	paths := make([]string, 0, len(filesByPath))
	for path := range filesByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	metadata.SourceFiles = make([]CompactSourceInput, 0, len(paths))
	fileIndices := make(map[string]int, len(paths))
	for _, path := range paths {
		metadata.SourceFiles = append(metadata.SourceFiles, filesByPath[path])
		fileIndices[path] = len(metadata.SourceFiles)
	}

	encodeInputs := func(inputs []CompactSourceInput, context string) (string, error) {
		canonical, err := canonicalCompactSourceInputs(inputs, context)
		if err != nil {
			return "", err
		}
		indices := make([]int, 0, len(inputs))
		for _, input := range canonical {
			indices = append(indices, fileIndices[input.Path])
		}
		sort.Ints(indices)
		return encodeCompactSourceInputGroup(indices), nil
	}

	groupSet := map[string]bool{}
	oldGroupEncodings := make([]string, len(metadata.SourceInputGroups))
	seenOldGroups := map[string]bool{}
	for i, encoded := range metadata.SourceInputGroups {
		if seenOldGroups[encoded] {
			return fmt.Errorf("source_input_groups repeats group %q", encoded)
		}
		seenOldGroups[encoded] = true
		oldIndices, err := decodeCompactSourceInputGroup(encoded, len(originalFiles))
		if err != nil {
			return fmt.Errorf("source input group %d: %w", i+1, err)
		}
		indices := make([]int, 0, len(oldIndices))
		for _, oldIndex := range oldIndices {
			indices = append(indices, fileIndices[originalFiles[oldIndex-1].Path])
		}
		sort.Ints(indices)
		oldGroupEncodings[i] = encodeCompactSourceInputGroup(indices)
		groupSet[oldGroupEncodings[i]] = true
	}
	addInlineGroup := func(inputs []CompactSourceInput, context string) (string, error) {
		if len(inputs) == 0 {
			return "", nil
		}
		encoded, err := encodeInputs(inputs, context)
		if err != nil {
			return "", err
		}
		groupSet[encoded] = true
		return encoded, nil
	}
	headerInlineGroups := make([]string, len(metadata.HeaderGroups))
	for i, group := range metadata.HeaderGroups {
		if group.SourceInputGroup == 0 {
			encoded, err := addInlineGroup(group.SourceInputs, fmt.Sprintf("header group %q", group.ID))
			if err != nil {
				return err
			}
			headerInlineGroups[i] = encoded
		}
	}
	variantInlineGroups := make([]string, len(metadata.ObjectVariants))
	for i, variant := range metadata.ObjectVariants {
		if variant.SourceInputGroup == 0 {
			encoded, err := addInlineGroup(variant.SourceInputs, fmt.Sprintf("object target %q", variant.Target))
			if err != nil {
				return err
			}
			variantInlineGroups[i] = encoded
		}
	}

	metadata.SourceInputGroups = make([]string, 0, len(groupSet))
	for encoded := range groupSet {
		metadata.SourceInputGroups = append(metadata.SourceInputGroups, encoded)
	}
	sort.Strings(metadata.SourceInputGroups)
	groupIndices := make(map[string]int, len(metadata.SourceInputGroups))
	for i, encoded := range metadata.SourceInputGroups {
		groupIndices[encoded] = i + 1
	}

	for i := range metadata.HeaderGroups {
		encoded := headerInlineGroups[i]
		if metadata.HeaderGroups[i].SourceInputGroup != 0 {
			encoded = oldGroupEncodings[metadata.HeaderGroups[i].SourceInputGroup-1]
		}
		metadata.HeaderGroups[i].SourceInputs = nil
		metadata.HeaderGroups[i].SourceInputGroup = 0
		if encoded != "" {
			metadata.HeaderGroups[i].SourceInputGroup = groupIndices[encoded]
		}
	}
	for i := range metadata.ObjectVariants {
		encoded := variantInlineGroups[i]
		if metadata.ObjectVariants[i].SourceInputGroup != 0 {
			encoded = oldGroupEncodings[metadata.ObjectVariants[i].SourceInputGroup-1]
		}
		metadata.ObjectVariants[i].SourceInputs = nil
		metadata.ObjectVariants[i].SourceInputGroup = 0
		if encoded != "" {
			metadata.ObjectVariants[i].SourceInputGroup = groupIndices[encoded]
		}
	}
	return metadata.validateSourceInputIndex()
}

func (metadata *CompactMetadata) validateSourceInputIndex() error {
	if metadata == nil || !metadata.Schema.isV013() {
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

	for _, group := range metadata.HeaderGroups {
		if len(group.SourceInputs) != 0 {
			return fmt.Errorf("header group %q retains inline source_inputs in v0.0.13 metadata", group.ID)
		}
		if err := validateReference(group.SourceInputGroup, fmt.Sprintf("header group %q", group.ID), ""); err != nil {
			return err
		}
	}
	for _, variant := range metadata.ObjectVariants {
		if len(variant.SourceInputs) != 0 {
			return fmt.Errorf("object target %q retains inline source_inputs in v0.0.13 metadata", variant.Target)
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

func (payload CompactConfigPayload) equal(other CompactConfigPayload) bool {
	return payload.ID == other.ID &&
		payload.Content == other.Content &&
		reflect.DeepEqual(payload.Fragment, other.Fragment)
}

func (environment CompactCompileEnvironment) equal(other CompactCompileEnvironment) bool {
	return environment.ID == other.ID &&
		environment.ABI == other.ABI &&
		environment.ConfigPayload == other.ConfigPayload &&
		reflect.DeepEqual(environment.HeaderGroups, other.HeaderGroups)
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

func sortedCompactHeaderGroups(values map[string]CompactHeaderGroup) []CompactHeaderGroup {
	ids := sortedCompactIDs(values)
	out := make([]CompactHeaderGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, values[id])
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
	if metadata == nil || !metadata.Schema.isV013() {
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
		if payload.Fragment != nil {
			canonical := canonicalConfigContent(payload.Fragment)
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
	headerGroups := make(map[string]CompactHeaderGroup, len(metadata.HeaderGroups))
	for _, group := range metadata.HeaderGroups {
		if err := check("header group", group.ID); err != nil {
			return err
		}
		if _, ok := payloads[group.ConfigPayload]; !ok {
			return fmt.Errorf("header group %s references unknown config payload %s", group.ID, group.ConfigPayload)
		}
		inputs, err := metadata.expandedSourceInputGroup(
			group.SourceInputGroup,
			group.SourceInputs,
			fmt.Sprintf("header group %q", group.ID),
		)
		if err != nil {
			return err
		}
		expected := newCompactHeaderGroup(
			group.ConfigPayload,
			"",
			group.Srcarch,
			group.Footprint,
			inputs,
		).ID
		if group.ID != expected {
			return fmt.Errorf("header group %s canonical inputs hash to %s", group.ID, expected)
		}
		headerGroups[group.ID] = group
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
		for i, groupID := range environment.HeaderGroups {
			if _, ok := headerGroups[groupID]; !ok {
				return fmt.Errorf(
					"compile environment %s references unknown header group %s",
					environment.ID,
					groupID,
				)
			}
			if i != 0 && environment.HeaderGroups[i-1] >= groupID {
				return fmt.Errorf(
					"compile environment %s has duplicate or non-canonical header groups",
					environment.ID,
				)
			}
		}
		expected := newCompactCompileEnvironment(
			environment.ABI,
			environment.ConfigPayload,
			environment.HeaderGroups,
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
			variant.SourceInputs,
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
