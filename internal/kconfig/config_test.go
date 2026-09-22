package kconfig

import (
	"maps"
	"strings"
	"testing"
)

func TestCompactMetadataForResolvedConfigWithOptionsReusesResolvedValue(t *testing.T) {
	values := map[string]string{
		"CONFIG_ENABLED": "y", "CONFIG_DISABLED": "n", "CONFIG_EMPTY": `""`,
		"CONFIG_STRING_N": `"n"`, "CONFIG_PRESENT_EMPTY": "",
	}
	resolved := &ResolvedConfig{Effective: maps.Clone(values)}
	called := 0
	metadata, err := CompactMetadataForResolvedConfigWithOptions(
		resolved,
		CompactMetadataOptions{SelectedProductsOnly: true},
		func(got *ResolvedConfig) (CompactConfigGraph, error) {
			called++
			if got != resolved {
				t.Fatal("resolved-config phase boundary cloned or re-resolved its input")
			}
			return CompactConfigGraph{ImageTarget: "image"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || metadata.configFragment["CONFIG_ENABLED"] != "y" {
		t.Fatalf("resolved metadata callback calls=%d fragment=%#v", called, metadata.configFragment)
	}
	want := maps.Clone(values)
	delete(want, "CONFIG_DISABLED")
	if !maps.Equal(metadata.configFragment, want) || metadata.Config.imageTarget != "image" {
		t.Fatalf("imported metadata fragment = %#v, image = %q", metadata.configFragment, metadata.Config.imageTarget)
	}
	if !maps.Equal(resolved.Effective, values) {
		t.Fatal("imported config was mutated")
	}
	if len(metadata.configSymbolUniverse) != len(values) {
		t.Fatalf("symbol universe = %#v, want all imported values", metadata.configSymbolUniverse)
	}
	if _, err := CompactMetadataForResolvedConfigWithOptions(
		nil, CompactMetadataOptions{ConfigProjectionPaths: recognizedConfigDocuments()}, func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{}, nil
		},
	); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("nil resolved config error = %v", err)
	}
}

func TestCompactMetadataFirstLoweringConsumesValidatedSelectionGraph(t *testing.T) {
	metadata, err := CompactMetadataForResolvedConfigWithOptions(
		&ResolvedConfig{Effective: map[string]string{"CONFIG_ENABLED": "y"}},
		CompactMetadataOptions{PreconfiguredObjectTree: true, SelectedProductsOnly: true},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{ImageTarget: "image"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	validated := metadata.validatedSelectionGraph
	if validated == nil {
		t.Fatal("metadata construction did not retain its validated selection graph")
	}

	_, first, err := metadata.lowerSelectedActionPlan(
		actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != validated {
		t.Fatal("first lowering rebuilt the selection graph validated by metadata construction")
	}
	if metadata.validatedSelectionGraph != nil {
		t.Fatal("first lowering retained a mutable selection graph for reuse")
	}

	_, second, err := metadata.lowerSelectedActionPlan(
		actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("later lowering reused materialization state from the first traversal")
	}
}

func TestCompactMetadataRetainedSelectionGraphStillValidatesEagerly(t *testing.T) {
	_, err := CompactMetadataForResolvedConfigWithOptions(
		&ResolvedConfig{Effective: map[string]string{"CONFIG_ENABLED": "y"}},
		CompactMetadataOptions{SelectedProductsOnly: true},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				KbuildSelections: []CompactKbuildSelection{{
					Profile: "missing", Target: "target", MakeTarget: "target",
					Lifecycle: "target", Scope: "target", Stage: "target",
				}},
			}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), `resolve Kbuild selections: Kbuild selection references missing profile "missing"`) {
		t.Fatalf("metadata construction error = %v, want eager selection-graph validation", err)
	}
}

func TestParseConfigPreservesCanonicalUnsetComments(t *testing.T) {
	raw, err := ParseConfig(strings.NewReader(`# Generated configuration
# CONFIG_DEFAULT_ON is not set
# CONFIG_ is not set
# CONFIG_SPACED  is not set
# CONFIG_TRAILING is not set trailing text
# CONFIG_DIFFERENT_COMMENT has another meaning
`))
	if err != nil {
		t.Fatalf("ParseConfig() failed: %v", err)
	}
	if got, want := raw, map[string]string{"CONFIG_DEFAULT_ON": "n"}; !maps.Equal(got, want) {
		t.Fatalf("ParseConfig() = %#v, want %#v", got, want)
	}
}

func TestParseConfigRejectsDuplicateAssignmentAndUnset(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("CONFIG_DUPLICATE=y\n# CONFIG_DUPLICATE is not set\n"))
	if err == nil || !strings.Contains(err.Error(), `duplicate config key "CONFIG_DUPLICATE"`) {
		t.Fatalf("ParseConfig() error = %v, want duplicate key", err)
	}
}
