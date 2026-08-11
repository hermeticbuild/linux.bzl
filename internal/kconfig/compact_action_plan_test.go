package kconfig

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const compactActionPlanTestProbeIdentity = "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestWriteCompactActionPlanEmitsDeterministicSelectedRecipes(t *testing.T) {
	metadata := compactActionPlanMetadataForTest(t)
	first := filepath.Join(t.TempDir(), "plan")
	second := filepath.Join(t.TempDir(), "plan")
	if err := os.Mkdir(second, 0o755); err != nil {
		t.Fatalf("precreate TreeArtifact directory: %v", err)
	}
	if err := metadata.WriteCompactActionPlan(first); err != nil {
		t.Fatalf("WriteCompactActionPlan(first) failed: %v", err)
	}
	if err := metadata.WriteCompactActionPlan(second); err != nil {
		t.Fatalf("WriteCompactActionPlan(second) failed: %v", err)
	}

	firstFiles := compactActionPlanFilesForTest(t, first)
	secondFiles := compactActionPlanFilesForTest(t, second)
	if !reflect.DeepEqual(firstFiles, secondFiles) {
		t.Fatalf("compact action plan is not deterministic:\nfirst: %#v\nsecond: %#v", firstFiles, secondFiles)
	}
	probePath := "v1/probe/" + compactActionPlanTestProbeIdentity
	if data, ok := firstFiles[probePath]; !ok || len(data) != 0 {
		t.Fatalf("probe marker %q = %q, present=%v; want empty marker", probePath, data, ok)
	}

	selected := compactActionPlanSelectedVariantsForTest(t, metadata)
	compileFiles := map[string][]byte{}
	sourceMarkers := 0
	for name, data := range firstFiles {
		switch {
		case strings.HasPrefix(name, "v1/compile/"):
			compileFiles[name] = data
		case strings.HasPrefix(name, "v1/source/"):
			sourceMarkers++
			if len(data) != 0 {
				t.Fatalf("source marker %q is not empty: %q", name, data)
			}
		}
	}
	if len(compileFiles) != len(selected) {
		t.Fatalf("compile marker count = %d, want %d", len(compileFiles), len(selected))
	}
	if sourceMarkers == 0 {
		t.Fatal("compact action plan emitted no source markers")
	}
	for _, variant := range selected {
		primary, err := metadata.sourceFileIndex(variant.Source)
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf(
			"v1/compile/%s/%08d/%s.json",
			variant.ContentID,
			primary,
			variant.Object,
		)
		got, ok := compileFiles[name]
		if !ok {
			t.Fatalf("missing compile marker %q; got %v", name, sortedCompactActionPlanFileNames(compileFiles))
		}
		want, err := json.MarshalIndent(variant, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, '\n')
		if string(got) != string(want) {
			t.Fatalf("compile marker %q =\n%s\nwant:\n%s", name, got, want)
		}
	}
}

func TestWriteCompactActionPlanRejectsNonemptyOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "plan")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(output, "user-data")
	if err := os.WriteFile(sentinel, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := compactActionPlanMetadataForTest(t).WriteCompactActionPlan(output)
	if err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("WriteCompactActionPlan() error = %v, want nonempty output error", err)
	}
	data, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(data) != "preserve me" {
		t.Fatalf("nonempty output was modified: data=%q, error=%v", data, readErr)
	}
}

func TestWriteCompactActionPlanRejectsUnsupportedMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompactMetadata, *CompactObjectVariant)
		want   string
	}{
		{
			name: "non-adaptive metadata",
			mutate: func(metadata *CompactMetadata, _ *CompactObjectVariant) {
				metadata.Schema = ""
			},
			want: "measured adaptive compact metadata",
		},
		{
			name: "unmeasured probe identity",
			mutate: func(metadata *CompactMetadata, _ *CompactObjectVariant) {
				metadata.Target.ProbeIdentity = "fixed-clang"
			},
			want: "measured sha256 probe identity",
		},
		{
			name: "multiple configs",
			mutate: func(metadata *CompactMetadata, _ *CompactObjectVariant) {
				metadata.Configs = append(metadata.Configs, metadata.Configs[0])
			},
			want: "exactly one config",
		},
		{
			name: "composite recipe",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Members = []string{"member"}
			},
			want: "unsupported composite recipe",
		},
		{
			name: "generated dependency",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Deps = []string{"generated"}
			},
			want: "unsupported generated-object dependencies",
		},
		{
			name: "assembly source",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Source = strings.TrimSuffix(variant.Source, ".c") + ".S"
			},
			want: "unsupported non-C source",
		},
		{
			name: "module recipe",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Mode = "m"
				variant.ModuleRoot = true
			},
			want: "unsupported non-built-in mode",
		},
		{
			name: "object path traversal",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Object = "../escape.o"
			},
			want: "not a canonical relative path",
		},
		{
			name: "missing primary source",
			mutate: func(_ *CompactMetadata, variant *CompactObjectVariant) {
				variant.Source = "missing.c"
			},
			want: "omits primary source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := compactActionPlanMetadataForTest(t)
			variant := compactActionPlanFirstSelectedVariantForTest(t, metadata)
			test.mutate(metadata, variant)
			output := filepath.Join(t.TempDir(), "plan")
			err := metadata.WriteCompactActionPlan(output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("WriteCompactActionPlan() error = %v, want substring %q", err, test.want)
			}
			if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
				t.Fatalf("invalid plan published output %q: %v", output, statErr)
			}
		})
	}
}

func compactActionPlanMetadataForTest(t *testing.T) *CompactMetadata {
	t.Helper()
	metadata, err := compactMetadataBatchForTest(
		t,
		mustParseCompactFixture(t),
		mustParseKbuildFixture(t),
		[]NamedConfig{{Name: "selected", Flags: map[string]string{"CONFIG_NET": "y"}}},
	)
	if err != nil {
		t.Fatalf("compactMetadataBatchForTest() failed: %v", err)
	}
	metadata.Schema = compactActionPlanMetadataSchema
	metadata.Target = &CompactTarget{
		Profile:       "x86_64",
		LinuxArch:     "x86",
		Srcarch:       "x86",
		UTSMachine:    "x86_64",
		TargetTriple:  "x86_64-linux-gnu",
		ProbeIdentity: compactActionPlanTestProbeIdentity,
	}
	return metadata
}

func compactActionPlanFirstSelectedVariantForTest(t *testing.T, metadata *CompactMetadata) *CompactObjectVariant {
	t.Helper()
	if len(metadata.Configs) != 1 || len(metadata.Configs[0].ObjectTargets) == 0 {
		t.Fatalf("test metadata has no selected object: %#v", metadata.Configs)
	}
	target := metadata.Configs[0].ObjectTargets[0]
	for i := range metadata.ObjectVariants {
		if metadata.ObjectVariants[i].Target == target {
			return &metadata.ObjectVariants[i]
		}
	}
	t.Fatalf("selected target %q has no variant", target)
	return nil
}

func compactActionPlanSelectedVariantsForTest(t *testing.T, metadata *CompactMetadata) []CompactObjectVariant {
	t.Helper()
	byTarget := map[string]CompactObjectVariant{}
	for _, variant := range metadata.ObjectVariants {
		byTarget[variant.Target] = variant
	}
	targets := append([]string(nil), metadata.Configs[0].ObjectTargets...)
	targets = append(targets, metadata.Configs[0].ModuleObjectTargets...)
	selected := make([]CompactObjectVariant, 0, len(targets))
	seen := map[string]bool{}
	for _, target := range targets {
		if seen[target] {
			continue
		}
		seen[target] = true
		variant, ok := byTarget[target]
		if !ok {
			t.Fatalf("selected target %q has no variant", target)
		}
		selected = append(selected, variant)
	}
	return selected
}

func compactActionPlanFilesForTest(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(relative)] = data
		return nil
	}); err != nil {
		t.Fatalf("walk compact action plan: %v", err)
	}
	return out
}

func sortedCompactActionPlanFileNames(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for name := range files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
