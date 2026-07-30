package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestDecodeMetadataAcceptsCurrentShape(t *testing.T) {
	data, err := json.Marshal(validContentGraphMetadata())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeMetadata(data); err != nil {
		t.Fatalf("decodeMetadata() rejected current metadata shape: %v", err)
	}
}

func TestDecodeMetadataRejectsUnknownFieldsAtEveryLevel(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "top-level schema",
			data: `{"schema":"v0.0.13"}`,
			want: `"schema"`,
		},
		{
			name: "top-level object packages",
			data: `{"object_packages":[]}`,
			want: `"object_packages"`,
		},
		{
			name: "config package",
			data: `{"configs":[{"package":"//graph"}]}`,
			want: `"package"`,
		},
		{
			name: "config image target",
			data: `{"configs":[{"image_target":"base_image"}]}`,
			want: `"image_target"`,
		},
		{
			name: "config payload fragment",
			data: `{"config_payloads":[{"fragment":{}}]}`,
			want: `"fragment"`,
		},
		{
			name: "compile environment schema",
			data: `{"compile_environments":[{"schema":""}]}`,
			want: `"schema"`,
		},
		{
			name: "generated header inline inputs",
			data: `{"generated_header_families":[{"source_inputs":[]}]}`,
			want: `"source_inputs"`,
		},
		{
			name: "source file package",
			data: `{"source_files":[{"package":""}]}`,
			want: `"package"`,
		},
		{
			name: "object source includes",
			data: `{"object_variants":[{"source_includes":[]}]}`,
			want: `"source_includes"`,
		},
		{
			name: "object empty inline inputs",
			data: `{"object_variants":[{"source_inputs":[]}]}`,
			want: `"source_inputs"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeMetadata([]byte(test.data))
			if err == nil || !strings.Contains(err.Error(), "unknown field "+test.want) {
				t.Fatalf("decodeMetadata() error = %v, want unknown field %s", err, test.want)
			}
		})
	}
}

func sparseMetadataDocument() map[string]any {
	return map[string]any{
		"configs": []any{
			map[string]any{
				"name":           "base",
				"object_targets": []any{},
			},
		},
		"config_payloads": []any{
			map[string]any{
				"id":      strings.Repeat("1", 64),
				"content": "",
			},
		},
		"compile_environments": []any{
			map[string]any{
				"id":             strings.Repeat("2", 64),
				"abi":            "llvm-test/x86",
				"config_payload": strings.Repeat("1", 64),
			},
		},
		"generated_header_families": []any{
			map[string]any{
				"id":             strings.Repeat("3", 64),
				"name":           "static",
				"config_payload": strings.Repeat("1", 64),
				"srcarch":        "x86",
			},
		},
		"source_files": []any{
			map[string]any{
				"path":   "init/main.c",
				"digest": strings.Repeat("4", 64),
			},
		},
		"source_input_groups": []any{"1"},
		"action_groups":       []any{},
		"object_variants": []any{
			map[string]any{
				"target": "init",
				"object": "init/main.o",
				"mode":   "y",
			},
		},
	}
}

func firstMetadataObject(document map[string]any, collection string) map[string]any {
	return document[collection].([]any)[0].(map[string]any)
}

func decodeMetadataDocument(document map[string]any) error {
	data, err := json.Marshal(document)
	if err != nil {
		return err
	}
	_, err = decodeMetadata(data)
	return err
}

func TestDecodeMetadataAcceptsSparseOptionalFields(t *testing.T) {
	if err := decodeMetadataDocument(sparseMetadataDocument()); err != nil {
		t.Fatalf("decodeMetadata() rejected omitted optional fields: %v", err)
	}
}

func TestDecodeMetadataRejectsMissingNullAndWrongTypes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "missing top-level configs",
			mutate: func(document map[string]any) {
				delete(document, "configs")
			},
			want: `missing required field "configs"`,
		},
		{
			name: "null top-level configs",
			mutate: func(document map[string]any) {
				document["configs"] = nil
			},
			want: "configs",
		},
		{
			name: "wrong top-level configs type",
			mutate: func(document map[string]any) {
				document["configs"] = map[string]any{}
			},
			want: "configs",
		},
		{
			name: "missing object targets",
			mutate: func(document map[string]any) {
				delete(firstMetadataObject(document, "configs"), "object_targets")
			},
			want: `missing required field "object_targets"`,
		},
		{
			name: "null object targets",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "configs")["object_targets"] = nil
			},
			want: "object_targets",
		},
		{
			name: "wrong object targets type",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "configs")["object_targets"] = ""
			},
			want: "object_targets",
		},
		{
			name: "wrong object target item type",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "configs")["object_targets"] = []any{1}
			},
			want: "object_targets",
		},
		{
			name: "null optional config payload",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "configs")["config_payload"] = nil
			},
			want: "config_payload",
		},
		{
			name: "null optional module targets",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "configs")["module_object_targets"] = nil
			},
			want: "module_object_targets",
		},
		{
			name: "null optional environment families",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "compile_environments")["generated_header_families"] = nil
			},
			want: "generated_header_families",
		},
		{
			name: "null optional family labels",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "generated_header_families")["labels"] = nil
			},
			want: "labels",
		},
		{
			name: "null optional family source input group",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "generated_header_families")["source_input_group"] = nil
			},
			want: "source_input_group",
		},
		{
			name: "null optional object source",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "object_variants")["source"] = nil
			},
			want: "source",
		},
		{
			name: "null optional object flags",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "object_variants")["flags"] = nil
			},
			want: "flags",
		},
		{
			name: "wrong optional object group type",
			mutate: func(document map[string]any) {
				firstMetadataObject(document, "object_variants")["source_input_group"] = "1"
			},
			want: "source_input_group",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := sparseMetadataDocument()
			test.mutate(document)
			err := decodeMetadataDocument(document)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodeMetadata() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateMetadataContentGraph(t *testing.T) {
	meta := validContentGraphMetadata()
	stats, err := validateMetadata(meta)
	if err != nil {
		t.Fatalf("validateMetadata() error: %v", err)
	}
	if stats.objectVariants != 1 || stats.selectedObjectVariants != 1 || stats.objectMemberships != 1 {
		t.Fatalf("validateMetadata() stats = %+v", stats)
	}
}

func validContentGraphMetadata() *metadata {
	payloadContent := "CONFIG_TEST=y\n"
	payloadID := canonicalContentID(configPayloadDomain, payloadContent)
	headerDigest := strings.Repeat("6", 64)
	headerID := canonicalContentID(
		generatedHeaderFamilyDomain,
		"name=static",
		"srcarch=x86",
		"config_payload="+payloadID,
		"source_input=include/generated.h\x00"+headerDigest,
	)
	environmentID := canonicalContentID(
		compileEnvironmentDomain,
		"abi=llvm-test/x86",
		"config_payload="+payloadID,
		"generated_header_family="+headerID,
	)
	sourceDigest := strings.Repeat("5", 64)
	objectID := canonicalContentID(
		objectContentDomain,
		"object=init/main.o",
		"mode=y",
		"modname=",
		"compile_environment="+environmentID,
		"abi=llvm-test/x86",
		"source=init/main.c",
		"source_input=init/main.c\x00"+sourceDigest,
	)
	target := "init__" + objectID[:24]
	return &metadata{
		Configs: []config{{
			Name:          "base",
			ConfigPayload: payloadID,
			ObjectTargets: []string{target},
		}},
		ConfigPayloads: []configPayload{{
			ID:      payloadID,
			Content: payloadContent,
		}},
		SourceFiles: []sourceInput{
			{Path: "include/generated.h", Digest: headerDigest},
			{Path: "init/main.c", Digest: sourceDigest},
		},
		SourceInputGroups: []string{"1", "2"},
		ActionGroups:      []actionGroup{},
		GeneratedHeaderFamilies: []generatedHeaderFamily{{
			ID:               headerID,
			Name:             "static",
			ConfigPayload:    payloadID,
			Labels:           []string{"//:generated_headers"},
			Srcarch:          "x86",
			SourceInputGroup: 1,
		}},
		CompileEnvironments: []compileEnvironment{{
			ID:                      environmentID,
			ABI:                     "llvm-test/x86",
			ConfigPayload:           payloadID,
			GeneratedHeaderFamilies: []string{headerID},
		}},
		ObjectVariants: []objectVariant{{
			Target:             target,
			ContentID:          objectID,
			CompileEnvironment: environmentID,
			Object:             "init/main.o",
			Source:             "init/main.c",
			SourceInputGroup:   2,
			Mode:               "y",
		}},
	}
}

func TestValidateMetadataContentGraphRejectsStaleContentIDs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*metadata)
		want   string
	}{
		{
			name: "payload content",
			mutate: func(meta *metadata) {
				meta.ConfigPayloads[0].Content += "CONFIG_MUTATED=y\n"
			},
			want: "config payload",
		},
		{
			name: "header digest",
			mutate: func(meta *metadata) {
				meta.SourceFiles[0].Digest = strings.Repeat("a", 64)
			},
			want: "generated header family",
		},
		{
			name: "ABI",
			mutate: func(meta *metadata) {
				meta.CompileEnvironments[0].ABI += "-mutated"
			},
			want: "compile environment",
		},
		{
			name: "leaf flags",
			mutate: func(meta *metadata) {
				meta.ObjectVariants[0].Flags = []string{"-DMUTATED"}
			},
			want: "object target",
		},
		{
			name: "leaf source digest",
			mutate: func(meta *metadata) {
				meta.SourceFiles[1].Digest = strings.Repeat("b", 64)
			},
			want: "object target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			meta := validContentGraphMetadata()
			test.mutate(meta)
			_, err := validateMetadata(meta)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateMetadata() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateMetadataContentGraphRejectsAllMixedWithPreciseFamilies(t *testing.T) {
	meta := validContentGraphMetadata()
	payloadID := meta.ConfigPayloads[0].ID
	allID := canonicalContentID(
		generatedHeaderFamilyDomain,
		"name=all",
		"srcarch=x86",
		"config_payload="+payloadID,
	)
	meta.GeneratedHeaderFamilies = append(meta.GeneratedHeaderFamilies, generatedHeaderFamily{
		ID:            allID,
		Name:          "all",
		ConfigPayload: payloadID,
		Labels:        []string{"//:generated_headers"},
		Srcarch:       "x86",
	})
	meta.CompileEnvironments[0].GeneratedHeaderFamilies = append(
		meta.CompileEnvironments[0].GeneratedHeaderFamilies,
		allID,
	)
	sort.Strings(meta.CompileEnvironments[0].GeneratedHeaderFamilies)

	_, err := validateMetadata(meta)
	if err == nil || !strings.Contains(err.Error(), "mixes all with precise") {
		t.Fatalf("validateMetadata() error = %v, want all/precise rejection", err)
	}
}

func TestValidateMetadataContentGraphRejectsUnknownFamilyDependency(t *testing.T) {
	meta := validContentGraphMetadata()
	payloadID := meta.ConfigPayloads[0].ID
	unknownID := strings.Repeat("a", 64)
	familyID := canonicalContentID(
		generatedHeaderFamilyDomain,
		"name=bounds",
		"srcarch=x86",
		"config_payload="+payloadID,
		"dependency="+unknownID,
	)
	meta.GeneratedHeaderFamilies = append(meta.GeneratedHeaderFamilies, generatedHeaderFamily{
		ID:            familyID,
		Name:          "bounds",
		ConfigPayload: payloadID,
		Labels:        []string{"//:generated_headers"},
		Srcarch:       "x86",
		Dependencies:  []string{unknownID},
	})

	_, err := validateMetadata(meta)
	if err == nil || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("validateMetadata() error = %v, want unknown dependency rejection", err)
	}
}

func TestValidateMetadataContentGraphRejectsNonCanonicalDependencyOrder(t *testing.T) {
	meta := validContentGraphMetadata()
	root := meta.ObjectVariants[0]
	abi := meta.CompileEnvironments[0].ABI
	source := meta.SourceFiles[1]
	dependencies := make([]objectVariant, 0, 2)
	for _, object := range []string{"deps/a.o", "deps/b.o"} {
		contentID := canonicalContentID(
			objectContentDomain,
			"object="+object,
			"mode=y",
			"modname=",
			"compile_environment="+root.CompileEnvironment,
			"abi="+abi,
			"source="+root.Source,
			"source_input="+source.Path+"\x00"+source.Digest,
		)
		dependencies = append(dependencies, objectVariant{
			Target:             strings.TrimSuffix(object, ".o") + "__" + contentID[:24],
			ContentID:          contentID,
			CompileEnvironment: root.CompileEnvironment,
			Object:             object,
			Source:             root.Source,
			SourceInputGroup:   root.SourceInputGroup,
			Mode:               "y",
		})
	}
	meta.ObjectVariants = append(meta.ObjectVariants, dependencies...)
	dependencyTargets := []string{dependencies[0].Target, dependencies[1].Target}
	dependencyIDs := []string{dependencies[0].ContentID, dependencies[1].ContentID}
	sort.Strings(dependencyTargets)
	sort.Strings(dependencyIDs)
	root.Deps = dependencyTargets
	root.ContentID = canonicalContentID(
		objectContentDomain,
		"object="+root.Object,
		"mode="+root.Mode,
		"modname="+root.ModName,
		"compile_environment="+root.CompileEnvironment,
		"abi="+abi,
		"source="+root.Source,
		"source_input="+source.Path+"\x00"+source.Digest,
		"dep_content_id="+dependencyIDs[0],
		"dep_content_id="+dependencyIDs[1],
	)
	root.Target = "init__" + root.ContentID[:24]
	meta.ObjectVariants[0] = root
	meta.Configs[0].ObjectTargets[0] = root.Target
	if _, err := validateMetadata(meta); err != nil {
		t.Fatalf("validateMetadata() rejected canonical dependencies: %v", err)
	}

	meta.ObjectVariants[0].Deps[0], meta.ObjectVariants[0].Deps[1] =
		meta.ObjectVariants[0].Deps[1], meta.ObjectVariants[0].Deps[0]
	_, err := validateMetadata(meta)
	if err == nil || !strings.Contains(err.Error(), "non-canonical dependencies") {
		t.Fatalf("validateMetadata() error = %v, want non-canonical dependencies", err)
	}
}

func TestValidateMetadataRejectsDuplicateContentID(t *testing.T) {
	contentID := strings.Repeat("a", 64)
	_, err := validateMetadata(&metadata{
		ObjectVariants: []objectVariant{
			{Target: "left", Object: "left.o", ContentID: contentID},
			{Target: "right", Object: "right.o", ContentID: contentID},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate content ID") {
		t.Fatalf("validateMetadata() error = %v, want duplicate content ID", err)
	}
}

func TestValidateMetadataRejectsDanglingObjectTarget(t *testing.T) {
	meta := validContentGraphMetadata()
	meta.Configs[0].ObjectTargets[0] = "missing"
	_, err := validateMetadata(meta)
	if err == nil || !strings.Contains(err.Error(), "unknown object target") {
		t.Fatalf("validateMetadata() error = %v, want unknown object target", err)
	}
}
