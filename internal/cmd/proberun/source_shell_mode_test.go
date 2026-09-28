package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func renderSourceShellForTest(fragments []kconfig.ProbeValueFragment, inputs map[string]kconfig.ProbeResult, guard bool) (string, error) {
	var resultGuard func(string) error
	if guard {
		resultGuard = validateProbeSourceShellResult
	}
	return renderProbeValueFragmentsWithResultGuard("source-shell test", fragments, 0,
		nil, nil, nil, nil, nil, inputs, "", nil, resultGuard)
}

func TestSourceShellResultGuardRejectsBeforeTransforms(t *testing.T) {
	const reference = "${result:00000000.text}"
	for _, test := range []struct {
		name      string
		fragments []kconfig.ProbeValueFragment
	}{
		{"direct", []kconfig.ProbeValueFragment{{Value: reference}}},
		{"embedded", []kconfig.ProbeValueFragment{{Value: "prefix" + reference + "suffix"}}},
		{"nested aggregate", []kconfig.ProbeValueFragment{{Fragments: []kconfig.ProbeValueFragment{{Value: reference}}}}},
		{"discarding outer transform", []kconfig.ProbeValueFragment{{
			Fragments:  []kconfig.ProbeValueFragment{{Value: reference}},
			Transforms: []kconfig.ProbeValueTransform{{Function: "filter-out", Arguments: []string{"%", ""}, InputArgument: 1}},
		}}},
		{"transform literal argument", []kconfig.ProbeValueFragment{{
			Value: "ordinary", Transforms: []kconfig.ProbeValueTransform{{
				Function: "filter-out", Arguments: []string{reference, ""}, InputArgument: 1,
			}},
		}}},
		{"transform nested argument", []kconfig.ProbeValueFragment{{
			Value: "ordinary", Transforms: []kconfig.ProbeValueTransform{{
				Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
				ArgumentFragments: []kconfig.ProbeValueTransformArgumentFragments{{Index: 0,
					Fragments: []kconfig.ProbeValueFragment{{Fragments: []kconfig.ProbeValueFragment{{Value: reference}}}},
				}},
			}},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range []string{
				"\x01linux-bzl-action-object-tree\x02", "\x01", "\x02", "\x03", "\x04",
				"\x05", "\x06", "\x07", "\x08", "\x00", "\r", "\x0b", "\x0c", "\x1f", "\x7f",
			} {
				inputs := map[string]kconfig.ProbeResult{"00000000": {Kind: "text", Text: value}}
				got, err := renderSourceShellForTest(test.fragments, inputs, true)
				if err == nil || got != "" || !strings.Contains(err.Error(), "measured result contains a prohibited control byte") {
					t.Fatalf("result %q was not rejected before transformation: %q, %v", value, got, err)
				}
				if test.name == "direct" {
					if got, err := renderSourceShellForTest(test.fragments, inputs, false); err != nil || got != value {
						t.Fatalf("default fragment semantics changed for %q: %q, %v", value, got, err)
					}
				}
			}
		})
	}
}

func TestSourceShellResultGuardPreservesSelectedBranchesAndWhitespace(t *testing.T) {
	selected := false
	inputs := map[string]kconfig.ProbeResult{
		"00000000": {Kind: "text", Text: "\x01forged\x02"},
		"00000001": {Kind: "boolean", Boolean: &selected},
		"00000002": {Kind: "text", Text: "  -O2\t -g "},
	}
	fragments := []kconfig.ProbeValueFragment{
		{Value: "${result:00000000.text}", When: &kconfig.ProbePredicate{Operator: "result-true", Result: "00000001"}},
		{Value: "${result:00000002.text}", Transforms: []kconfig.ProbeValueTransform{{Function: "strip", Arguments: []string{""}, InputArgument: 0}}},
		{Value: " ${result:00000001.boolean}"},
	}
	got, err := renderSourceShellForTest(fragments, inputs, true)
	if err != nil || got != "-O2 -g false" {
		t.Fatalf("inactive result or ordinary Make whitespace changed: %q, %v", got, err)
	}
	selected = true
	if _, err := renderSourceShellForTest(fragments, inputs, true); err == nil {
		t.Fatal("selected forged result was accepted")
	}
	if _, err := renderSourceShellForTest([]kconfig.ProbeValueFragment{{Value: "${result:99999999.text}"}}, inputs, true); err == nil {
		t.Fatal("missing result acquired a fallback")
	}
}

func TestSourceShellLiteralRootsFinalizeAfterMakeTransforms(t *testing.T) {
	const root = "\x01linux-bzl-action-object-tree\x02"
	fragments := []kconfig.ProbeValueFragment{{
		Fragments: []kconfig.ProbeValueFragment{
			{Value: `-DKBUILD_MODFILE='"` + root + `/asm offsets"' `},
			{Value: "${result:00000000.text}"},
		},
		Transforms: []kconfig.ProbeValueTransform{{Function: "filter-out", Arguments: []string{"-g -flto", ""}, InputArgument: 1}},
	}}
	inputs := map[string]kconfig.ProbeResult{"00000000": {Kind: "text", Text: "-g -flto -O2"}}
	rendered, err := renderSourceShellForTest(fragments, inputs, true)
	if err != nil || !strings.Contains(rendered, root) {
		t.Fatalf("planner root was rejected or finalized before Make: %q, %v", rendered, err)
	}
	words, err := kconfig.ParseProbeSourceShellWords(rendered)
	want := []string{`-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/asm offsets"`, "-O2"}
	if err != nil || !slices.Equal(words, want) {
		t.Fatalf("source shell words = %q, %v, want %q", words, err, want)
	}
	// This suffix matches the public spelling but not the private marker.
	// Finalizing a literal AST leaf before filter-out incorrectly removes it.
	fragments = []kconfig.ProbeValueFragment{{Value: root + "/asm-offsets", Transforms: []kconfig.ProbeValueTransform{{
		Function: "filter-out", Arguments: []string{"%OBJECT_TREE__/asm-offsets", ""}, InputArgument: 1,
	}}}}
	rendered, err = renderSourceShellForTest(fragments, nil, true)
	if err != nil || rendered != root+"/asm-offsets" {
		t.Fatalf("transform observed finalized root: %q, %v", rendered, err)
	}
	words, err = kconfig.ParseProbeSourceShellWords(rendered)
	if err != nil || !slices.Equal(words, []string{"__LINUX_BZL_OBJECT_TREE__/asm-offsets"}) {
		t.Fatalf("post-transform finalization = %q, %v", words, err)
	}
}

func TestSourceShellResultGuardControlByteDomain(t *testing.T) {
	for character := byte(0); character < 0x20; character++ {
		err := validateProbeSourceShellResult(string([]byte{character}))
		if (err == nil) != (character == '\t' || character == '\n') {
			t.Fatalf("control byte %02x acceptance: %v", character, err)
		}
	}
}

func TestRunProbeSourceShellModeExactArgumentsAndEarlyResultGuard(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(fmt.Sprintf("forged=%t", forged), func(t *testing.T) {
			dir := t.TempDir()
			identity := "sha256-" + strings.Repeat("6", 64)
			marker := filepath.Join(dir, identity)
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			dependency := kconfig.ProbeResult{
				Schema: kconfig.LinuxProbeResultSchema, NodeID: strings.Repeat("7", 64), RequestID: strings.Repeat("8", 64),
				Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: "-g -flto -O2",
			}
			if forged {
				dependency.Text = "\x01forged\x02"
			}
			dependencyData, err := dependency.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			dependencyPath := filepath.Join(dir, "input.json")
			if err := os.WriteFile(dependencyPath, dependencyData, 0o600); err != nil {
				t.Fatal(err)
			}
			request := kconfig.ProbeRequest{
				Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
				Steps: []kconfig.ProbeStep{{
					Name: "compiler-intrinsic-integer", Tool: "cc",
					Arguments: []string{"", "-E", "-P", "-x", "c", "-"},
					Stdin:     "#undef deprecated\n__has_attribute(deprecated)\n",
					Candidate: &kconfig.ProbeCandidateArguments{Policy: kconfig.ProbeCandidatePolicyCC,
						Projection: kconfig.ProbeCandidateProjectionCompilerIntrinsic, Base: []int{0}},
					ArgumentFragments: []kconfig.ProbeArgumentFragments{{Index: 0, Mode: kconfig.ProbeArgumentFragmentsModeSourceShellWords,
						Fragments: []kconfig.ProbeValueFragment{{
							Fragments: []kconfig.ProbeValueFragment{
								{Value: "-DKBUILD_MODFILE='\"\x01linux-bzl-action-object-tree\x02/asm offsets\"' "},
								{Value: "${result:00000000.text}"},
							},
							Transforms: []kconfig.ProbeValueTransform{{Function: "filter-out", Arguments: []string{"-g -flto", ""}, InputArgument: 1}},
						}},
					}},
				}},
				Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "compiler-intrinsic-integer", Stream: "stdout", RequireSuccess: true},
			}
			requestID, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			requestData, err := request.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(dir, "request.json")
			if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
				t.Fatal(err)
			}
			node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{dependency.NodeID}}
			node.ID = node.ContentID()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal([]string{`-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/asm offsets"`, "-O2", "-E", "-P", "-x", "c", "-"})
			resultPath := filepath.Join(dir, "result.json")
			err = runTestProbe(t, probeOptions{
				request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
				toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": dependencyPath},
				tools: map[string]actionContract{"cc": {path: executable,
					arguments:   []string{"-test.run=TestProbeHelperProcess", "--", "exact-json-arguments", kconfig.LinuxKbuildArgsSentinel},
					environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "EXPECTED_ARGUMENTS_JSON": string(want)},
				}},
			})
			if forged {
				if err == nil || !strings.Contains(err.Error(), "measured result contains a prohibited control byte") {
					t.Fatalf("forged result was not rejected before compiler execution: %v", err)
				}
				if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
					t.Fatal("failed source-shell rendering published a result")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := kconfig.ReadProbeResult(resultPath)
			if err != nil || result.Text != "accepted" {
				t.Fatalf("source-shell argv was not executed exactly: %#v, %v", result, err)
			}
		})
	}
}
