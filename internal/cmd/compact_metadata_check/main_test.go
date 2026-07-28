package main

import (
	"sort"
	"strings"
	"testing"
)

func TestValidateMetadataV013(t *testing.T) {
	meta := validV013Metadata()
	stats, err := validateMetadata(meta, true)
	if err != nil {
		t.Fatalf("validateMetadata() error: %v", err)
	}
	if stats.objectVariants != 1 || stats.selectedObjectVariants != 1 || stats.objectMemberships != 1 {
		t.Fatalf("validateMetadata() stats = %+v", stats)
	}
}

func validV013Metadata() *metadata {
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
		Schema: "v0.0.13",
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

func TestValidateMetadataV013RejectsStaleContentIDs(t *testing.T) {
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
			meta := validV013Metadata()
			test.mutate(meta)
			_, err := validateMetadata(meta, true)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateMetadata() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateMetadataV013RejectsAllMixedWithPreciseFamilies(t *testing.T) {
	meta := validV013Metadata()
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

	_, err := validateMetadata(meta, true)
	if err == nil || !strings.Contains(err.Error(), "mixes all with precise") {
		t.Fatalf("validateMetadata() error = %v, want all/precise rejection", err)
	}
}

func TestValidateMetadataV013RejectsUnknownFamilyDependency(t *testing.T) {
	meta := validV013Metadata()
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

	_, err := validateMetadata(meta, true)
	if err == nil || !strings.Contains(err.Error(), "unknown dependency") {
		t.Fatalf("validateMetadata() error = %v, want unknown dependency rejection", err)
	}
}

func TestValidateMetadataV013RejectsNonCanonicalDependencyOrder(t *testing.T) {
	meta := validV013Metadata()
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
	if _, err := validateMetadata(meta, true); err != nil {
		t.Fatalf("validateMetadata() rejected canonical dependencies: %v", err)
	}

	meta.ObjectVariants[0].Deps[0], meta.ObjectVariants[0].Deps[1] =
		meta.ObjectVariants[0].Deps[1], meta.ObjectVariants[0].Deps[0]
	_, err := validateMetadata(meta, true)
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
	}, false)
	if err == nil || !strings.Contains(err.Error(), "duplicate content ID") {
		t.Fatalf("validateMetadata() error = %v, want duplicate content ID", err)
	}
}

func TestValidateMetadataRejectsDanglingObjectTarget(t *testing.T) {
	_, err := validateMetadata(&metadata{
		Configs: []config{{
			Name:          "base",
			ObjectTargets: []string{"missing"},
		}},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "unknown object target") {
		t.Fatalf("validateMetadata() error = %v, want unknown object target", err)
	}
}

func TestValidateMetadataLegacyStats(t *testing.T) {
	stats, err := validateMetadata(&metadata{
		Configs: []config{
			{Name: "base", ObjectTargets: []string{"shared"}},
			{Name: "overlay", ObjectTargets: []string{"shared", "added"}},
		},
		ObjectVariants: []objectVariant{
			{Target: "shared", Object: "shared.o"},
			{Target: "added", Object: "added.o"},
		},
	}, false)
	if err != nil {
		t.Fatalf("validateMetadata() error: %v", err)
	}
	if stats.objectMemberships != 3 || stats.objectVariants != 2 || stats.duplicateMemberships != 1 {
		t.Fatalf("validateMetadata() stats = %+v", stats)
	}
}
