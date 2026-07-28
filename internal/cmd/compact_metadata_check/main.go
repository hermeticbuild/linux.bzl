package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type repeated []string

func (r *repeated) String() string {
	return strings.Join(*r, ",")
}

func (r *repeated) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty assertion")
	}
	*r = append(*r, value)
	return nil
}

type metadata struct {
	Configs                 []config                `json:"configs"`
	ConfigPayloads          []configPayload         `json:"config_payloads"`
	CompileEnvironments     []compileEnvironment    `json:"compile_environments"`
	GeneratedHeaderFamilies []generatedHeaderFamily `json:"generated_header_families"`
	SourceFiles             []sourceInput           `json:"source_files"`
	SourceInputGroups       []string                `json:"source_input_groups"`
	ObjectVariants          []objectVariant         `json:"object_variants"`
}

type config struct {
	Name                string   `json:"name"`
	ConfigPayload       string   `json:"config_payload,omitempty"`
	ObjectTargets       []string `json:"object_targets"`
	ModuleObjectTargets []string `json:"module_object_targets,omitempty"`
}

type objectVariant struct {
	Target             string   `json:"target"`
	ContentID          string   `json:"content_id,omitempty"`
	CompileEnvironment string   `json:"compile_environment,omitempty"`
	Object             string   `json:"object"`
	Source             string   `json:"source,omitempty"`
	SourceInputGroup   int      `json:"source_input_group,omitempty"`
	Mode               string   `json:"mode"`
	ModuleRoot         bool     `json:"module_root,omitempty"`
	ModName            string   `json:"modname,omitempty"`
	Flags              []string `json:"flags,omitempty"`
	RemoveFlags        []string `json:"remove_flags,omitempty"`
	ObjtoolArgs        []string `json:"objtool_args,omitempty"`
	ObjtoolDisabled    bool     `json:"objtool_disabled,omitempty"`
	ObjtoolForce       bool     `json:"objtool_force,omitempty"`
	Deps               []string `json:"deps,omitempty"`
	Members            []string `json:"members,omitempty"`
}

type sourceInput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type configPayload struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type compileEnvironment struct {
	ID                      string   `json:"id"`
	ABI                     string   `json:"abi"`
	ConfigPayload           string   `json:"config_payload"`
	GeneratedHeaderFamilies []string `json:"generated_header_families,omitempty"`
}

type generatedHeaderFamily struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	ConfigPayload    string   `json:"config_payload"`
	Labels           []string `json:"labels,omitempty"`
	Srcarch          string   `json:"srcarch"`
	Dependencies     []string `json:"dependencies,omitempty"`
	SourceInputGroup int      `json:"source_input_group,omitempty"`
}

func main() {
	var (
		metadataPath = flag.String("metadata", "", "Compact metadata JSON to validate")
		maxVariants  = flag.Int("max_object_variants", -1, "Fail when unique object variants exceed this value")
		printSummary = flag.Bool("summary", false, "Print graph membership and deduplication metrics")
		share        repeated
		differ       repeated
		present      repeated
		absent       repeated
	)
	flag.Var(&share, "share", "Assert CONFIG_A:CONFIG_B:OBJECT use the same object target. May be repeated")
	flag.Var(&differ, "differ", "Assert CONFIG_A:CONFIG_B:OBJECT use different object targets. May be repeated")
	flag.Var(&present, "present", "Assert CONFIG:OBJECT is present. May be repeated")
	flag.Var(&absent, "absent", "Assert CONFIG:OBJECT is absent. May be repeated")
	flag.Parse()

	if *metadataPath == "" {
		fmt.Fprintln(os.Stderr, "-metadata is required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*metadataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read metadata: %v\n", err)
		os.Exit(1)
	}
	meta, err := decodeMetadata(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse metadata: %v\n", err)
		os.Exit(1)
	}
	stats, err := validateMetadata(meta)
	if err != nil {
		fail(err)
	}
	if *maxVariants >= 0 && stats.objectVariants > *maxVariants {
		fail(fmt.Errorf(
			"object variants %d exceed limit %d",
			stats.objectVariants,
			*maxVariants,
		))
	}
	index := newIndex(meta)

	if err := checkPresence(index, present, true); err != nil {
		fail(err)
	}
	if err := checkPresence(index, absent, false); err != nil {
		fail(err)
	}
	if err := checkPair(index, share, true); err != nil {
		fail(err)
	}
	if err := checkPair(index, differ, false); err != nil {
		fail(err)
	}
	if *printSummary {
		fmt.Printf(
			"configs=%d object_memberships=%d selected_object_variants=%d object_definitions=%d duplicate_memberships=%d\n",
			len(meta.Configs),
			stats.objectMemberships,
			stats.selectedObjectVariants,
			stats.objectVariants,
			stats.duplicateMemberships,
		)
	}
	fmt.Println("compact metadata checks passed")
}

func decodeMetadata(data []byte) (*metadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var meta metadata
	if err := decoder.Decode(&meta); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	if err := validateJSONShape(data, reflect.TypeOf(metadata{}), "metadata"); err != nil {
		return nil, err
	}
	return &meta, nil
}

func validateJSONShape(data []byte, model reflect.Type, context string) error {
	raw := bytes.TrimSpace(data)
	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("%s must not be null", context)
	}
	switch model.Kind() {
	case reflect.Struct:
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a JSON object: %w", context, err)
		}
		fields := make(map[string]reflect.StructField, model.NumField())
		required := make(map[string]bool, model.NumField())
		for index := 0; index < model.NumField(); index++ {
			field := model.Field(index)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field
			required[name] = !strings.Contains(","+options+",", ",omitempty,")
		}
		keys := make([]string, 0, len(value))
		for name := range value {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("%s has unknown field %q", context, name)
			}
			if err := validateJSONShape(value[name], field.Type, context+"."+name); err != nil {
				return err
			}
		}
		for name, isRequired := range required {
			if _, ok := value[name]; isRequired && !ok {
				return fmt.Errorf("%s is missing required field %q", context, name)
			}
		}
		return nil
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return fmt.Errorf("%s must be a JSON array: %w", context, err)
		}
		for index, value := range values {
			if err := validateJSONShape(
				value,
				model.Elem(),
				fmt.Sprintf("%s[%d]", context, index),
			); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a JSON string: %w", context, err)
		}
		return nil
	case reflect.Int:
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a JSON integer: %w", context, err)
		}
		return nil
	case reflect.Bool:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a JSON boolean: %w", context, err)
		}
		return nil
	default:
		return fmt.Errorf("%s uses unsupported validation type %s", context, model)
	}
}

type metadataIndex struct {
	targetObject map[string]string
	objectsByCfg map[string]map[string]string
}

type metadataStats struct {
	objectMemberships      int
	objectVariants         int
	selectedObjectVariants int
	duplicateMemberships   int
}

func validateMetadata(meta *metadata) (metadataStats, error) {
	stats := metadataStats{objectVariants: len(meta.ObjectVariants)}
	targets := map[string]objectVariant{}
	contentIDs := map[string]string{}
	for _, variant := range meta.ObjectVariants {
		if variant.Target == "" || variant.Object == "" {
			return stats, fmt.Errorf("object variant has empty target or object path")
		}
		if variant.Mode != "" && variant.Mode != "y" && variant.Mode != "m" {
			return stats, fmt.Errorf("object target %q has invalid mode %q", variant.Target, variant.Mode)
		}
		if existing, ok := targets[variant.Target]; ok {
			return stats, fmt.Errorf(
				"duplicate object target %q for %q and %q",
				variant.Target,
				existing.Object,
				variant.Object,
			)
		}
		targets[variant.Target] = variant
		if variant.ContentID != "" {
			if !isContentID(variant.ContentID) {
				return stats, fmt.Errorf(
					"object target %q has invalid content ID %q",
					variant.Target,
					variant.ContentID,
				)
			}
			if existing := contentIDs[variant.ContentID]; existing != "" {
				return stats, fmt.Errorf(
					"object targets %q and %q duplicate content ID %s",
					existing,
					variant.Target,
					variant.ContentID,
				)
			}
			contentIDs[variant.ContentID] = variant.Target
		} else {
			return stats, fmt.Errorf("object target %q has no content ID", variant.Target)
		}
	}
	for _, variant := range meta.ObjectVariants {
		for _, dependency := range append(append([]string(nil), variant.Deps...), variant.Members...) {
			if _, ok := targets[dependency]; !ok {
				return stats, fmt.Errorf(
					"object target %q references unknown dependency %q",
					variant.Target,
					dependency,
				)
			}
		}
	}

	configPayloads, err := validateContentAddressedMetadata(meta, targets)
	if err != nil {
		return stats, err
	}

	configNames := map[string]bool{}
	selectedTargets := map[string]bool{}
	for _, cfg := range meta.Configs {
		if cfg.Name == "" {
			return stats, fmt.Errorf("compact config has no name")
		}
		if configNames[cfg.Name] {
			return stats, fmt.Errorf("duplicate compact config name %q", cfg.Name)
		}
		configNames[cfg.Name] = true
		if cfg.ConfigPayload != "" && !configPayloads[cfg.ConfigPayload] {
			return stats, fmt.Errorf(
				"config %q references unknown config payload %s",
				cfg.Name,
				cfg.ConfigPayload,
			)
		}
		if cfg.ConfigPayload == "" {
			return stats, fmt.Errorf("config %q has no full config payload", cfg.Name)
		}
		roots := append(
			append([]string(nil), cfg.ObjectTargets...),
			cfg.ModuleObjectTargets...,
		)
		for _, target := range roots {
			if _, ok := targets[target]; !ok {
				return stats, fmt.Errorf(
					"config %q references unknown object target %q",
					cfg.Name,
					target,
				)
			}
		}
		reachable := map[string]bool{}
		stack := append([]string(nil), roots...)
		for len(stack) != 0 {
			target := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if reachable[target] {
				continue
			}
			reachable[target] = true
			selectedTargets[target] = true
			variant := targets[target]
			stack = append(stack, variant.Deps...)
			stack = append(stack, variant.Members...)
		}
		stats.objectMemberships += len(reachable)
	}
	stats.selectedObjectVariants = len(selectedTargets)
	stats.duplicateMemberships = stats.objectMemberships - stats.selectedObjectVariants
	if stats.duplicateMemberships < 0 {
		stats.duplicateMemberships = 0
	}

	return stats, nil
}

const (
	configPayloadDomain         = "linux-compact-config-payload-v1"
	compileEnvironmentDomain    = "linux-compact-compile-environment-v2"
	generatedHeaderFamilyDomain = "linux-compact-generated-header-family-v1"
	objectContentDomain         = "linux-compact-object-v1"
)

func canonicalContentID(domain string, values ...string) string {
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write([]byte{0})
	for _, value := range values {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func decodeSourceInputGroup(encoded string, fileCount int) ([]int, error) {
	if encoded == "" {
		return nil, fmt.Errorf("source input group is empty")
	}
	indices := make([]int, 0, strings.Count(encoded, ",")+1)
	previous := 0
	for _, value := range strings.Split(encoded, ",") {
		index, err := strconv.Atoi(value)
		if err != nil || index <= 0 || strconv.Itoa(index) != value {
			return nil, fmt.Errorf("invalid source file index %q", value)
		}
		if index > fileCount {
			return nil, fmt.Errorf("source file index %d is out of range 1..%d", index, fileCount)
		}
		if index <= previous {
			return nil, fmt.Errorf("duplicate or non-canonical source file index %d", index)
		}
		indices = append(indices, index)
		previous = index
	}
	return indices, nil
}

func validateContentAddressedMetadata(
	meta *metadata,
	targets map[string]objectVariant,
) (map[string]bool, error) {
	if len(meta.ConfigPayloads) == 0 || len(meta.CompileEnvironments) == 0 {
		return nil, fmt.Errorf("content graph has no compile environments")
	}

	for i, input := range meta.SourceFiles {
		if input.Path == "" || !isContentID(input.Digest) {
			return nil, fmt.Errorf("source file %d is invalid: %+v", i+1, input)
		}
		if i != 0 && meta.SourceFiles[i-1].Path >= input.Path {
			return nil, fmt.Errorf("source files are duplicate or not canonical at %q", input.Path)
		}
	}
	groupFiles := make([][]int, len(meta.SourceInputGroups))
	for i, encoded := range meta.SourceInputGroups {
		if i != 0 && meta.SourceInputGroups[i-1] >= encoded {
			return nil, fmt.Errorf("source input groups are duplicate or not canonical at %q", encoded)
		}
		indices, err := decodeSourceInputGroup(encoded, len(meta.SourceFiles))
		if err != nil {
			return nil, fmt.Errorf("source input group %d: %w", i+1, err)
		}
		groupFiles[i] = indices
	}
	referencedGroups := map[int]bool{}
	referencedFiles := map[int]bool{}
	groupInputs := func(group int, context string) ([]sourceInput, error) {
		if group <= 0 || group > len(groupFiles) {
			return nil, fmt.Errorf(
				"%s source input group %d is out of range 1..%d",
				context,
				group,
				len(groupFiles),
			)
		}
		referencedGroups[group] = true
		inputs := make([]sourceInput, 0, len(groupFiles[group-1]))
		for _, index := range groupFiles[group-1] {
			referencedFiles[index] = true
			inputs = append(inputs, meta.SourceFiles[index-1])
		}
		return inputs, nil
	}

	payloadIDs := map[string]bool{}
	for _, payload := range meta.ConfigPayloads {
		if !isContentID(payload.ID) || payloadIDs[payload.ID] {
			return nil, fmt.Errorf("invalid or duplicate config payload ID %q", payload.ID)
		}
		expected := canonicalContentID(configPayloadDomain, payload.Content)
		if payload.ID != expected {
			return nil, fmt.Errorf("config payload %s canonical content hashes to %s", payload.ID, expected)
		}
		payloadIDs[payload.ID] = true
	}

	familyByID := map[string]generatedHeaderFamily{}
	for _, family := range meta.GeneratedHeaderFamilies {
		if !isContentID(family.ID) {
			return nil, fmt.Errorf("invalid generated header family ID %q", family.ID)
		}
		if _, exists := familyByID[family.ID]; exists {
			return nil, fmt.Errorf("duplicate generated header family ID %q", family.ID)
		}
		if !payloadIDs[family.ConfigPayload] {
			return nil, fmt.Errorf(
				"generated header family %s references unknown config payload %s",
				family.ID,
				family.ConfigPayload,
			)
		}
		if family.Name == "" || family.Srcarch == "" || len(family.Labels) == 0 {
			return nil, fmt.Errorf(
				"generated header family %s has incomplete metadata",
				family.ID,
			)
		}
		for i, label := range family.Labels {
			if label == "" {
				return nil, fmt.Errorf(
					"generated header family %s has an empty label",
					family.ID,
				)
			}
			if i != 0 && family.Labels[i-1] >= label {
				return nil, fmt.Errorf(
					"generated header family %s has duplicate or non-canonical labels",
					family.ID,
				)
			}
		}
		for i, dependency := range family.Dependencies {
			if !isContentID(dependency) || dependency == family.ID {
				return nil, fmt.Errorf(
					"generated header family %s has invalid dependency %q",
					family.ID,
					dependency,
				)
			}
			if i != 0 && family.Dependencies[i-1] >= dependency {
				return nil, fmt.Errorf(
					"generated header family %s has duplicate or non-canonical dependencies",
					family.ID,
				)
			}
		}
		var inputs []sourceInput
		if family.SourceInputGroup != 0 {
			var err error
			inputs, err = groupInputs(
				family.SourceInputGroup,
				"generated header family "+family.ID,
			)
			if err != nil {
				return nil, err
			}
		}
		values := []string{
			"name=" + family.Name,
			"srcarch=" + family.Srcarch,
			"config_payload=" + family.ConfigPayload,
		}
		for _, dependency := range family.Dependencies {
			values = append(values, "dependency="+dependency)
		}
		for _, input := range inputs {
			values = append(values, "source_input="+input.Path+"\x00"+input.Digest)
		}
		expected := canonicalContentID(generatedHeaderFamilyDomain, values...)
		if family.ID != expected {
			return nil, fmt.Errorf(
				"generated header family %s canonical fields hash to %s",
				family.ID,
				expected,
			)
		}
		familyByID[family.ID] = family
	}
	familyState := map[string]uint8{}
	var validateFamilyDependencies func(string) error
	validateFamilyDependencies = func(familyID string) error {
		switch familyState[familyID] {
		case 1:
			return fmt.Errorf("generated header family dependency cycle at %s", familyID)
		case 2:
			return nil
		}
		familyState[familyID] = 1
		family := familyByID[familyID]
		for _, dependency := range family.Dependencies {
			if _, ok := familyByID[dependency]; !ok {
				return fmt.Errorf(
					"generated header family %s references unknown dependency %s",
					familyID,
					dependency,
				)
			}
			if err := validateFamilyDependencies(dependency); err != nil {
				return err
			}
		}
		familyState[familyID] = 2
		return nil
	}
	for familyID := range familyByID {
		if err := validateFamilyDependencies(familyID); err != nil {
			return nil, err
		}
	}

	environments := map[string]compileEnvironment{}
	abi := ""
	for _, environment := range meta.CompileEnvironments {
		if !isContentID(environment.ID) {
			return nil, fmt.Errorf("invalid compile environment ID %q", environment.ID)
		}
		if _, exists := environments[environment.ID]; exists {
			return nil, fmt.Errorf("duplicate compile environment ID %s", environment.ID)
		}
		if environment.ABI == "" || !payloadIDs[environment.ConfigPayload] {
			return nil, fmt.Errorf("compile environment %s has invalid ABI or config payload", environment.ID)
		}
		familyNames := map[string]bool{}
		for i, familyID := range environment.GeneratedHeaderFamilies {
			family, ok := familyByID[familyID]
			if !ok {
				return nil, fmt.Errorf(
					"compile environment %s references unknown generated header family %s",
					environment.ID,
					familyID,
				)
			}
			if i != 0 && environment.GeneratedHeaderFamilies[i-1] >= familyID {
				return nil, fmt.Errorf(
					"compile environment %s has duplicate or non-canonical generated header families",
					environment.ID,
				)
			}
			if familyNames[family.Name] {
				return nil, fmt.Errorf(
					"compile environment %s repeats generated header family %q",
					environment.ID,
					family.Name,
				)
			}
			familyNames[family.Name] = true
		}
		if familyNames["all"] && len(familyNames) != 1 {
			return nil, fmt.Errorf(
				"compile environment %s mixes all with precise generated header families",
				environment.ID,
			)
		}
		values := []string{
			"abi=" + environment.ABI,
			"config_payload=" + environment.ConfigPayload,
		}
		for _, familyID := range environment.GeneratedHeaderFamilies {
			values = append(values, "generated_header_family="+familyID)
		}
		expected := canonicalContentID(compileEnvironmentDomain, values...)
		if environment.ID != expected {
			return nil, fmt.Errorf("compile environment %s canonical fields hash to %s", environment.ID, expected)
		}
		if abi == "" {
			abi = environment.ABI
		} else if abi != environment.ABI {
			return nil, fmt.Errorf("compile environments use ABIs %q and %q", abi, environment.ABI)
		}
		environments[environment.ID] = environment
	}

	for _, variant := range meta.ObjectVariants {
		if variant.Mode != "y" && variant.Mode != "m" {
			return nil, fmt.Errorf("object target %q has invalid mode %q", variant.Target, variant.Mode)
		}
		if !strings.HasSuffix(variant.Target, "__"+variant.ContentID[:24]) {
			return nil, fmt.Errorf("object target %q does not use its collision-checked content ID", variant.Target)
		}
		exactAction := len(variant.Members) == 0 || variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"
		var inputs []sourceInput
		if exactAction {
			var err error
			inputs, err = groupInputs(variant.SourceInputGroup, "object target "+variant.Target)
			if err != nil {
				return nil, err
			}
			requiredSource := variant.Source
			if variant.Object == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o" {
				requiredSource = "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"
			}
			foundSource := false
			for _, input := range inputs {
				if input.Path == requiredSource {
					foundSource = true
				}
			}
			if requiredSource == "" || !foundSource {
				return nil, fmt.Errorf("object target %q exact inputs omit primary source %q", variant.Target, requiredSource)
			}
			if _, ok := environments[variant.CompileEnvironment]; !ok {
				return nil, fmt.Errorf(
					"object target %q references unknown compile environment %s",
					variant.Target,
					variant.CompileEnvironment,
				)
			}
		} else if variant.SourceInputGroup != 0 {
			return nil, fmt.Errorf("composite object target %q unexpectedly references source input group %d", variant.Target, variant.SourceInputGroup)
		}

		depIDs := make([]string, 0, len(variant.Deps))
		for i, target := range variant.Deps {
			if i != 0 && variant.Deps[i-1] >= target {
				return nil, fmt.Errorf(
					"object target %q has duplicate or non-canonical dependencies",
					variant.Target,
				)
			}
			depIDs = append(depIDs, targets[target].ContentID)
		}
		sort.Strings(depIDs)
		memberIDs := make([]string, 0, len(variant.Members))
		for _, target := range variant.Members {
			memberIDs = append(memberIDs, targets[target].ContentID)
		}
		values := []string{
			"object=" + variant.Object,
			"mode=" + variant.Mode,
			"modname=" + variant.ModName,
			"compile_environment=" + variant.CompileEnvironment,
			"abi=" + abi,
			"source=" + variant.Source,
		}
		for _, flag := range variant.Flags {
			values = append(values, "flag="+flag)
		}
		for _, flag := range variant.RemoveFlags {
			values = append(values, "remove_flag="+flag)
		}
		for _, input := range inputs {
			values = append(values, "source_input="+input.Path+"\x00"+input.Digest)
		}
		for _, id := range depIDs {
			values = append(values, "dep_content_id="+id)
		}
		for _, id := range memberIDs {
			values = append(values, "member_content_id="+id)
		}
		if variant.ModuleRoot {
			values = append(values, "module_root=true")
		}
		if variant.ObjtoolDisabled {
			values = append(values, "objtool_disabled=true")
		}
		if variant.ObjtoolForce {
			values = append(values, "objtool_force=true")
		}
		for _, arg := range variant.ObjtoolArgs {
			values = append(values, "objtool_arg="+arg)
		}
		expected := canonicalContentID(objectContentDomain, values...)
		if variant.ContentID != expected {
			return nil, fmt.Errorf("object target %q canonical fields hash to %s, got %s", variant.Target, expected, variant.ContentID)
		}
	}
	for group := range meta.SourceInputGroups {
		if !referencedGroups[group+1] {
			return nil, fmt.Errorf("source input group %d is not referenced", group+1)
		}
	}
	for file := range meta.SourceFiles {
		if !referencedFiles[file+1] {
			return nil, fmt.Errorf("source file %d %q is not referenced", file+1, meta.SourceFiles[file].Path)
		}
	}
	return payloadIDs, nil
}

func isContentID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func newIndex(meta *metadata) *metadataIndex {
	idx := &metadataIndex{
		targetObject: map[string]string{},
		objectsByCfg: map[string]map[string]string{},
	}
	for _, variant := range meta.ObjectVariants {
		idx.targetObject[variant.Target] = variant.Object
	}
	for _, cfg := range meta.Configs {
		objects := map[string]string{}
		for _, target := range cfg.ObjectTargets {
			object := idx.targetObject[target]
			if object == "" {
				continue
			}
			objects[object] = target
		}
		idx.objectsByCfg[cfg.Name] = objects
	}
	return idx
}

func checkPresence(idx *metadataIndex, assertions []string, wantPresent bool) error {
	for _, assertion := range assertions {
		cfg, object, err := parsePresence(assertion)
		if err != nil {
			return err
		}
		target := idx.objectTarget(cfg, object)
		if wantPresent && target == "" {
			return fmt.Errorf("%s: object %q is absent", cfg, object)
		}
		if !wantPresent && target != "" {
			return fmt.Errorf("%s: object %q unexpectedly present as %s", cfg, object, target)
		}
	}
	return nil
}

func checkPair(idx *metadataIndex, assertions []string, wantSame bool) error {
	for _, assertion := range assertions {
		left, right, object, err := parsePair(assertion)
		if err != nil {
			return err
		}
		leftTarget := idx.objectTarget(left, object)
		rightTarget := idx.objectTarget(right, object)
		if leftTarget == "" || rightTarget == "" {
			return fmt.Errorf("%s: missing object %q in %q or %q", assertion, object, left, right)
		}
		if wantSame && leftTarget != rightTarget {
			return fmt.Errorf("%s: targets differ: %s != %s", assertion, leftTarget, rightTarget)
		}
		if !wantSame && leftTarget == rightTarget {
			return fmt.Errorf("%s: targets unexpectedly match: %s", assertion, leftTarget)
		}
	}
	return nil
}

func (idx *metadataIndex) objectTarget(config, object string) string {
	objects := idx.objectsByCfg[config]
	if objects == nil {
		return ""
	}
	return objects[object]
}

func parsePresence(value string) (string, string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected CONFIG:OBJECT assertion, got %q", value)
	}
	return parts[0], parts[1], nil
}

func parsePair(value string) (string, string, string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("expected CONFIG_A:CONFIG_B:OBJECT assertion, got %q", value)
	}
	return parts[0], parts[1], parts[2], nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
