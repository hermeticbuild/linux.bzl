package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// An empty base slot is the ordering anchor for one dynamically expanded
// argument.  Conditional groups retain their ordinary Before indexes around
// that slot, including when the measured text contributes no words.
func TestRunProbePreservesArgumentFragmentAndConditionalOrder(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		arguments   []string
		conditional []kconfig.ProbeConditionalArguments
		fragments   []kconfig.ProbeArgumentFragments
	}{
		{
			name:      "finite before text",
			text:      "linux",
			arguments: []string{"", "suffix"},
			conditional: []kconfig.ProbeConditionalArguments{{
				Before:    0,
				When:      kconfig.ProbePredicate{Operator: "result-true", Result: "00000000"},
				Arguments: []string{"prefix"},
			}},
			fragments: []kconfig.ProbeArgumentFragments{{
				Index: 0, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000001.text}"}},
			}},
		},
		{
			name:      "text before finite",
			text:      "prefix",
			arguments: []string{"", "suffix"},
			conditional: []kconfig.ProbeConditionalArguments{{
				Before:    1,
				When:      kconfig.ProbePredicate{Operator: "result-true", Result: "00000000"},
				Arguments: []string{"linux"},
			}},
			fragments: []kconfig.ProbeArgumentFragments{{
				Index: 0, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000001.text}"}},
			}},
		},
		{
			name:      "empty text still anchors following finite argument",
			text:      "",
			arguments: []string{"prefix", "", "suffix"},
			conditional: []kconfig.ProbeConditionalArguments{{
				Before:    2,
				When:      kconfig.ProbePredicate{Operator: "result-true", Result: "00000000"},
				Arguments: []string{"linux"},
			}},
			fragments: []kconfig.ProbeArgumentFragments{{
				Index: 1, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000001.text}"}},
			}},
		},
		{
			name:      "adjacent text slots",
			text:      "prefix",
			arguments: []string{"", "", "suffix"},
			fragments: []kconfig.ProbeArgumentFragments{
				{Index: 0, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000001.text}"}}},
				{Index: 1, Fragments: []kconfig.ProbeValueFragment{{Value: "linux"}}},
			},
		},
		{
			name:      "one text slot splits whitespace words",
			text:      "prefix\t linux",
			arguments: []string{"", "suffix"},
			fragments: []kconfig.ProbeArgumentFragments{{
				Index: 0, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000001.text}"}},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			identity := "sha256-" + strings.Repeat("a", 64)
			marker := filepath.Join(dir, identity)
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			truth := true
			inputResults := []kconfig.ProbeResult{
				{
					Schema: kconfig.LinuxProbeResultSchema,
					NodeID: strings.Repeat("b", 64), RequestID: strings.Repeat("c", 64),
					Scope: "target", ToolsetIdentity: identity, Kind: "boolean", Boolean: &truth,
				},
				{
					Schema: kconfig.LinuxProbeResultSchema,
					NodeID: strings.Repeat("d", 64), RequestID: strings.Repeat("e", 64),
					Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: test.text,
				},
			}
			inputs := map[string]string{}
			inputNodeIDs := make([]string, len(inputResults))
			for index, result := range inputResults {
				data, err := result.CanonicalJSON()
				if err != nil {
					t.Fatal(err)
				}
				name := strings.Repeat("0", 7) + string(rune('0'+index))
				filename := filepath.Join(dir, "input-"+name+".json")
				if err := os.WriteFile(filename, data, 0o600); err != nil {
					t.Fatal(err)
				}
				inputs[name] = filename
				inputNodeIDs[index] = result.NodeID
			}

			request := kconfig.ProbeRequest{
				Schema: kconfig.LinuxProbeRequestSchema, InputCount: len(inputResults),
				Steps: []kconfig.ProbeStep{{
					Name: "consume", Tool: "cc", Arguments: test.arguments,
					ConditionalArguments: test.conditional,
					ArgumentFragments:    test.fragments,
				}},
				Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "consume", Stream: "stdout"},
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
			node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: inputNodeIDs}
			node.ID = node.ContentID()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			resultPath := filepath.Join(dir, "result.json")
			err = runTestProbe(t, probeOptions{
				request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
				toolsetMarkers: map[string]string{"target": marker}, inputs: inputs,
				tools: map[string]actionContract{"cc": {
					path:        executable,
					arguments:   []string{"-test.run=TestProbeHelperProcess", "--", "envelope", kconfig.LinuxKbuildArgsSentinel},
					environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "CONTRACT_VALUE": "exact"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := kconfig.ReadProbeResult(resultPath)
			if err != nil || result.Text != "exact" {
				t.Fatalf("result = %#v, %v", result, err)
			}
		})
	}
}

func TestRunProbeAppliesPureMakeTransformChainBeforeArgumentSplitting(t *testing.T) {
	dir := t.TempDir()
	identity := "sha256-" + strings.Repeat("f", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dependency := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema,
		NodeID: strings.Repeat("1", 64), RequestID: strings.Repeat("2", 64),
		Scope: "host", ToolsetIdentity: identity, Kind: "text",
		Text: "  -Wall -DREMOVE\t-g -DOTHER  ",
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
			Name: "consume", Tool: "cc", Arguments: []string{"prefix", "", "suffix"},
			ArgumentFragments: []kconfig.ProbeArgumentFragments{{
				Index: 1,
				Fragments: []kconfig.ProbeValueFragment{{
					Value: "${result:00000000.text}",
					Transforms: []kconfig.ProbeValueTransform{
						{Function: "filter-out", Arguments: []string{"-DREMOVE", ""}, InputArgument: 1},
						{Function: "filter-out", Arguments: []string{"-DOTHER", ""}, InputArgument: 1},
						{Function: "strip", Arguments: []string{""}, InputArgument: 0},
					},
				}},
			}},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "consume", Stream: "stdout"},
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
	node := kconfig.ProbePlanNode{Scope: "host", RequestID: requestID, Inputs: []string{dependency.NodeID}}
	node.ID = node.ContentID()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "host",
		toolsetMarkers: map[string]string{"host": marker}, inputs: map[string]string{"00000000": dependencyPath},
		tools: map[string]actionContract{"cc": {
			path:      executable,
			arguments: []string{"-test.run=TestProbeHelperProcess", "--", "transformed-arguments", kconfig.LinuxKbuildArgsSentinel},
			environment: map[string]string{
				"LINUX_BZL_PROBE_HELPER": "1", "EXPECTED_ARGUMENTS": "prefix -Wall -g suffix",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "accepted" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunProbeValidatesSignedDecimalArgumentFragments(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "zero", value: "0", valid: true},
		{name: "nineteen digits", value: "1234567890123456789", valid: true},
		{name: "negative nineteen digits", value: "-9223372036854775808", valid: true},
		{name: "multiword", value: "1 2"},
		{name: "empty"},
		{name: "metacharacter", value: "1;id"},
		{name: "glob", value: "*"},
		{name: "unicode whitespace", value: "1\u00a02"},
		{name: "twenty digits", value: "12345678901234567890"},
		{name: "plus sign", value: "+1"},
		{name: "bare minus", value: "-"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			identity := "sha256-" + strings.Repeat("7", 64)
			marker := filepath.Join(dir, identity)
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			dependency := kconfig.ProbeResult{
				Schema: kconfig.LinuxProbeResultSchema,
				NodeID: strings.Repeat("8", 64), RequestID: strings.Repeat("9", 64),
				Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: test.value,
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
					Name: "consume", Tool: "cc", Arguments: []string{"prefix", "", "suffix"},
					ArgumentFragments: []kconfig.ProbeArgumentFragments{{
						Index: 1, Mode: kconfig.ProbeArgumentFragmentsModeSignedDecimal,
						Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000000.text}"}},
					}},
				}},
				Outcome: kconfig.ProbeOutcome{
					Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "exit-zero", Step: "consume"},
				},
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
			resultPath := filepath.Join(dir, "result.json")
			err = runTestProbe(t, probeOptions{
				request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
				toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": dependencyPath},
				tools: map[string]actionContract{"cc": {
					path: executable,
					arguments: []string{
						"-test.run=TestProbeHelperProcess", "--", "transformed-arguments", kconfig.LinuxKbuildArgsSentinel,
					},
					environment: map[string]string{
						"LINUX_BZL_PROBE_HELPER": "1", "EXPECTED_ARGUMENTS": "prefix " + test.value + " suffix",
					},
				}},
			})
			if !test.valid {
				if err == nil || !strings.Contains(err.Error(), "signed-decimal argument") {
					t.Fatalf("runProbe() error = %v, want signed-decimal validation failure", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := kconfig.ReadProbeResult(resultPath)
			if err != nil || result.Boolean == nil || !*result.Boolean {
				t.Fatalf("result = %#v, error = %v; want true", result, err)
			}
		})
	}
}

func TestRunProbeProjectsDynamicCompilerPredefineArgumentsBeforePathResolution(t *testing.T) {
	dir := t.TempDir()
	identity := "sha256-" + strings.Repeat("6", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// This is the production shape which motivated the execution-time
	// projection: one Make-text fragment owns an entire compiler flag vector.
	// Its forced header is a logical Kbuild spelling, not a proberun-declared
	// path, and -Wa belongs only to the real assembler phase.
	const dynamic = "-C -CC -nostdinc -I__LINUX_BZL_SOURCE_TREE__/include " +
		"-include __LINUX_BZL_SOURCE_TREE__/include/linux/hidden.h " +
		"-DSELECTED=drivers/example --wrapper-mode mode.c " +
		"-fmacro-prefix-map=/mapped/drivers/example.c=drivers/example.c " +
		"-Wa,-gdwarf-5 -c -o drivers/example.o " +
		"drivers/example.c -MMD -MF drivers/example.d"
	dependency := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema,
		NodeID: strings.Repeat("7", 64), RequestID: strings.Repeat("8", 64),
		Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: dynamic,
	}
	dependencyData, err := dependency.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	dependencyPath := filepath.Join(dir, "input.json")
	if err := os.WriteFile(dependencyPath, dependencyData, 0o600); err != nil {
		t.Fatal(err)
	}
	mappedSource := filepath.Join(dir, "mapped.c")
	if err := os.WriteFile(mappedSource, []byte("int mapped;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1, Sources: []string{"mapped.c"},
		Steps: []kconfig.ProbeStep{{
			Name: "compiler-predefines", Tool: "cc",
			Arguments: []string{"", "${source:mapped.c}", "-dM", "-E", "-x", "c", "/dev/null"},
			ArgumentFragments: []kconfig.ProbeArgumentFragments{{
				Index:     0,
				Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000000.text}"}},
			}},
			Candidate: &kconfig.ProbeCandidateArguments{
				Policy:           kconfig.ProbeCandidatePolicyCC,
				Projection:       kconfig.ProbeCandidateProjectionCompilerPredefines,
				Base:             []int{0, 1},
				TranslationUnits: []string{"${source:mapped.c}", "drivers/example.c"},
			},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "compiler-predefines", Stream: "stdout"},
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
	node := kconfig.ProbePlanNode{
		Scope: "target", RequestID: requestID, Inputs: []string{dependency.NodeID},
	}
	node.ID = node.ContentID()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": dependencyPath},
		sources: map[string]string{"mapped.c": mappedSource},
		tools: map[string]actionContract{"cc": {
			path: executable,
			arguments: []string{
				"-test.run=TestProbeHelperProcess", "--", "transformed-arguments", kconfig.LinuxKbuildArgsSentinel,
			},
			environment: map[string]string{
				"LINUX_BZL_PROBE_HELPER": "1",
				"EXPECTED_ARGUMENTS": "-nostdinc -DSELECTED=1 --wrapper-mode mode.c " +
					"-fmacro-prefix-map=/mapped/drivers/example.c=drivers/example.c " +
					"-dM -E -x c /dev/null",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "accepted" {
		t.Fatalf("result = %#v, error = %v; want projected compiler invocation", result, err)
	}
}
