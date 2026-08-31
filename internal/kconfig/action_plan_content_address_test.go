package kconfig

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestNormalizeActionRecipeToolsetPathCapabilitiesRebasesLiteralTreeOffset(t *testing.T) {
	codec, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("target", "external/toolchain/bin/cc")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/toolchain/bin/cc")
	if err != nil {
		t.Fatal(err)
	}
	const firstMarker = "${tree:prep}"
	const secondMarker = "${tree:kernel}"
	removedBytes := len(capability) - len(canonical)
	if removedBytes < len(firstMarker) {
		t.Fatalf("capability suffix length %d is too short for collision regression", removedBytes)
	}
	// Arrange for the stale pre-normalization offset of the literal first marker
	// to identify the genuine second marker after the capability tag is removed.
	// Rebasing by marker ordinal must keep the literal authority on the first.
	before := "invoke " + capability + " " + firstMarker + strings.Repeat("x", removedBytes-len(firstMarker)) + secondMarker
	oldLiteralOffset := strings.Index(before, firstMarker)
	wantScript, err := codec.NormalizeValue(before)
	if err != nil {
		t.Fatal(err)
	}
	wantLiteralOffset := strings.Index(wantScript, firstMarker)
	if got := strings.Index(wantScript, secondMarker); got != oldLiteralOffset {
		t.Fatalf("collision setup second marker offset=%d, want stale literal offset %d", got, oldLiteralOffset)
	}

	recipe := ActionRecipe{
		Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{
			"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(before)),
			"-literal_tree_offset", strconv.Itoa(oldLiteralOffset),
		},
	}
	if err := normalizeActionRecipeToolsetPathCapabilities(&recipe, codec.NormalizeValue); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(recipe.Arguments[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(decoded); got != wantScript {
		t.Fatalf("normalized script=%q, want %q", got, wantScript)
	}
	if got, want := recipe.Arguments[3], strconv.Itoa(wantLiteralOffset); got != want {
		t.Fatalf("rebased literal offset=%q, want %q (stale offset %d selects the second marker)", got, want, oldLiteralOffset)
	}
}

func TestNormalizeActionRecipeToolsetPathCapabilitiesRejectsUnsafeLiteralOffsetsAtomically(t *testing.T) {
	codec, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("target", "external/toolchain/bin/cc")
	if err != nil {
		t.Fatal(err)
	}
	script := "invoke " + capability + " ${tree:prep} ${tree:kernel}"
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	markerOffset := strings.Index(script, "${tree:prep}")
	for _, test := range []struct {
		name      string
		arguments []string
		wantError string
	}{
		{
			name: "malformed offset",
			arguments: []string{
				capability, "-script_content_base64", encoded, "-literal_tree_offset", "not-a-number",
			},
			wantError: "invalid literal evaluated-script tree offset",
		},
		{
			name: "offset is not marker start",
			arguments: []string{
				capability, "-script_content_base64", encoded, "-literal_tree_offset", strconv.Itoa(markerOffset + 1),
			},
			wantError: "does not identify a complete tree marker",
		},
		{
			name: "ambiguous scripts",
			arguments: []string{
				capability,
				"-script_content_base64", encoded,
				"--script_content_base64=" + encoded,
				"--literal_tree_offset=" + strconv.Itoa(markerOffset),
			},
			wantError: "require exactly one untransformed base64 script argument",
		},
		{
			name: "ambiguous raw script",
			arguments: []string{
				capability,
				"-script_content_base64", encoded,
				"-script_content", "${tree:prep}",
				"-literal_tree_offset", strconv.Itoa(markerOffset),
			},
			wantError: "require exactly one untransformed base64 script argument",
		},
		{
			name: "repeated offset",
			arguments: []string{
				capability, "-script_content_base64", encoded,
				"-literal_tree_offset", strconv.Itoa(markerOffset),
				"--literal_tree_offset=" + strconv.Itoa(markerOffset),
			},
			wantError: "repeats literal evaluated-script tree offset",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipe := ActionRecipe{Tool: compactKbuildScriptRunnerRole, Arguments: slices.Clone(test.arguments)}
			before := cloneActionRecipe(recipe)
			err := normalizeActionRecipeToolsetPathCapabilities(&recipe, codec.NormalizeValue)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("normalization error=%v, want %q", err, test.wantError)
			}
			if !reflect.DeepEqual(recipe, before) {
				t.Fatalf("failed normalization mutated recipe\n got: %#v\nwant: %#v", recipe, before)
			}
		})
	}
}

func TestNormalizeActionRecipeToolsetPathCapabilitiesRejectsTreeMarkerMutation(t *testing.T) {
	const script = "${tree:prep} then ${tree:kernel}"
	recipe := ActionRecipe{
		Tool: compactKbuildScriptRunnerRole,
		Arguments: []string{
			"-script_content_base64", base64.StdEncoding.EncodeToString([]byte(script)),
			"-literal_tree_offset", "0",
		},
	}
	before := cloneActionRecipe(recipe)
	err := normalizeActionRecipeToolsetPathCapabilities(&recipe, func(value string) (string, error) {
		return strings.Replace(value, "${tree:prep}", "${tree:host}", 1), nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed evaluated-script tree marker 0") {
		t.Fatalf("normalization error=%v, want marker-mutation rejection", err)
	}
	if !reflect.DeepEqual(recipe, before) {
		t.Fatalf("failed normalization mutated recipe\n got: %#v\nwant: %#v", recipe, before)
	}
}

func TestActionRecipeToolsetScopesCoverRuntimeRewriteSurfaces(t *testing.T) {
	target, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	host, err := toolaction.EncodeExecutionRootProvenancePath("host", "external/clang/include")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		recipe ActionRecipe
		want   []string
	}{
		{name: "ordinary", recipe: ActionRecipe{Arguments: []string{"-c", "source.c"}}},
		{name: "argument", recipe: ActionRecipe{Arguments: []string{target}}, want: []string{"target"}},
		{name: "environment", recipe: ActionRecipe{Environment: map[string]string{"FLAGS": host}}, want: []string{"host"}},
		{name: "working directory", recipe: ActionRecipe{WorkingDirectory: target}, want: []string{"target"}},
		{name: "replay argument", recipe: ActionRecipe{CommandReplays: []ActionRecipeCommandReplay{{Invocations: []ActionRecipeCommandReplayInvocation{{Arguments: []string{host}}}}}}, want: []string{"host"}},
		{name: "replay output", recipe: ActionRecipe{CommandReplays: []ActionRecipeCommandReplay{{Invocations: []ActionRecipeCommandReplayInvocation{{Outputs: []string{target}}}}}}, want: []string{"target"}},
		{
			name: "evaluated script",
			recipe: ActionRecipe{Tool: compactKbuildScriptRunnerRole, Arguments: []string{
				"-script_content_base64",
				base64.StdEncoding.EncodeToString([]byte("printf '%s\\n' " + host)),
			}},
			want: []string{"host"},
		},
		{
			name: "evaluated script equals flag",
			recipe: ActionRecipe{Tool: compactKbuildScriptRunnerRole, Arguments: []string{
				"--script_content_base64=" + base64.StdEncoding.EncodeToString([]byte("printf '%s\\n' "+target)),
			}},
			want: []string{"target"},
		},
		{
			name: "non-scriptrun base64-looking argument is opaque",
			recipe: ActionRecipe{Tool: "actionfile", Arguments: []string{
				"-script_content_base64",
				base64.StdEncoding.EncodeToString([]byte("printf '%s\\n' " + host)),
			}},
		},
		{
			name: "content template script",
			recipe: ActionRecipe{
				Arguments: []string{"-script_content_base64", "printf '%s\\n' " + target},
				ArgumentTransforms: []ActionRecipeArgumentTransform{{
					Index: 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
				}},
			},
			want: []string{"target"},
		},
		{
			name: "both scopes sorted",
			recipe: ActionRecipe{
				Arguments:   []string{target},
				Environment: map[string]string{"FLAGS": host},
			},
			want: []string{"host", "target"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := actionRecipeToolsetScopes(test.recipe)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("toolset scopes = %q, want %q", got, test.want)
			}
		})
	}
}

func TestActionRecipeRequiresCompleteCanonicalToolsetPathTokens(t *testing.T) {
	valid, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", valid}, Outputs: []string{"00000000"},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid toolset-path token: %v", err)
	}
	for _, value := range []string{
		"stray\x07delimiter",
		toolaction.ExecutionRootProvenanceMarker + "target:external/gcc/../../etc" + toolaction.ExecutionRootProvenanceTerminator,
		valid + "/../../etc",
	} {
		recipe := cloneActionRecipe(base)
		recipe.Arguments[2] = value
		if err := recipe.Validate(); err == nil || !strings.Contains(err.Error(), "invalid toolset-path provenance") {
			t.Fatalf("Validate(%q) error = %v, want invalid toolset-path rejection", value, err)
		}
	}
}

func TestActionRecipeRejectsPrivateRecursiveMakeProvenanceBytes(t *testing.T) {
	base := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	for _, test := range []struct {
		name   string
		mutate func(*ActionRecipe, string)
	}{
		{name: "tool", mutate: func(recipe *ActionRecipe, boundary string) { recipe.Tool = "actionfile" + boundary }},
		{name: "argument", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Arguments = append(recipe.Arguments, "value"+boundary)
		}},
		{name: "environment-name", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Environment = map[string]string{"NAME" + boundary: "value"}
		}},
		{name: "environment-value", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.Environment = map[string]string{"MAKE": "value" + boundary}
		}},
		{name: "working-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work" + boundary
		}},
		{name: "execution-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.ExecutionDirectory = "directory" + boundary
		}},
		{name: "declared-working-directory", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.WorkingDirectories = []string{"directory" + boundary}
		}},
		{name: "working-input-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.Inputs = []string{"data"}
			recipe.WorkingInputs = map[string]string{"input:data": "data" + boundary}
		}},
		{name: "working-output-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.WorkingOutputs = map[string]string{"00000000": "output" + boundary}
		}},
		{name: "observed-output-path", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.WorkingDirectory = "work"
			recipe.ObservedOutputs = map[string]string{"00000000": "output" + boundary}
		}},
		{name: "command-replay-argument", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.CommandReplays = []ActionRecipeCommandReplay{{
				Name: "make", Invocations: []ActionRecipeCommandReplayInvocation{{Arguments: []string{"value" + boundary}}},
			}}
		}},
		{name: "command-replay-output", mutate: func(recipe *ActionRecipe, boundary string) {
			recipe.CommandReplays = []ActionRecipeCommandReplay{{
				Name: "make", Invocations: []ActionRecipeCommandReplayInvocation{{Outputs: []string{"output" + boundary}}},
			}}
		}},
	} {
		for _, boundary := range []struct {
			name  string
			value string
		}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
			t.Run(test.name+"/"+boundary.name, func(t *testing.T) {
				recipe := cloneActionRecipe(base)
				test.mutate(&recipe, boundary.value)
				if err := recipe.Validate(); err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("Validate() error = %v, want private provenance-byte rejection", err)
				}
			})
		}
	}
}

func TestPrivateRecursiveMakeProvenanceBytesRejectedAtMakeIngress(t *testing.T) {
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		t.Run(boundary.name+"/source", func(t *testing.T) {
			if _, err := protectCompactKbuildSourceLiteralActionMarkers("value=" + boundary.value); err == nil ||
				!strings.Contains(err.Error(), "reserved literal-marker byte") {
				t.Fatalf("source protection error = %v, want reserved-byte rejection", err)
			}
		})
		t.Run(boundary.name+"/shell-output", func(t *testing.T) {
			if _, err := normalizeKbuildShellOutput("value=" + boundary.value); err == nil ||
				!strings.Contains(err.Error(), "reserved provenance byte") {
				t.Fatalf("shell normalization error = %v, want reserved-byte rejection", err)
			}
		})
	}
}

func TestActionRecipeMakeShellTransformsRejectPrivateProvenanceBytes(t *testing.T) {
	transforms := []struct {
		name string
		run  func(string) (string, error)
	}{
		{name: "value", run: NormalizeActionRecipeMakeShellValue},
		{name: "single-word", run: QuoteActionRecipeMakeShellSingleWord},
		{name: "single-quoted-segment", run: FormatActionRecipeMakeShellSingleQuotedSegment},
	}
	for _, transform := range transforms {
		for _, boundary := range []struct {
			name  string
			value string
		}{
			{name: "recursive opening", value: "\x05"}, {name: "recursive closing", value: "\x06"},
			{name: "toolset opening", value: "\x07"}, {name: "toolset closing", value: "\x08"},
		} {
			t.Run(transform.name+"/"+boundary.name, func(t *testing.T) {
				if _, err := transform.run("value" + boundary.value); err == nil ||
					!strings.Contains(err.Error(), "reserved provenance byte") {
					t.Fatalf("transform error = %v, want reserved-byte rejection", err)
				}
			})
		}
	}
}

func TestActionRecipeAcceptsPrintableAndProtectedRecursiveMakeLiterals(t *testing.T) {
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", compactKbuildRecursiveMakeMarker},
		Environment: map[string]string{
			"\x04_LINUX_BZL_MAKE__": "named literal",
		},
		CommandReplays: []ActionRecipeCommandReplay{{
			Name: "make",
			Invocations: []ActionRecipeCommandReplayInvocation{{
				Arguments: []string{"FLAG=\x04_LINUX_BZL_MAKE__"},
			}},
		}},
		Outputs: []string{"00000000"},
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("literal recursive Make spellings rejected: %v", err)
	}
}

func TestContentAddressActionPlanNodesRewritesSelectedDependencyEdges(t *testing.T) {
	profile := CompactKbuildProfile{Name: "root", Path: "Makefile"}
	selections := []CompactKbuildSelection{
		{Profile: profile.Name, Target: "generated/input.o", MakeTarget: "generated/input.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		{Profile: profile.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile}, KbuildSelections: selections,
	}
	if _, err := newCompactKbuildSelectionGraph(config); err != nil {
		t.Fatalf("selection-based fixture is invalid: %v", err)
	}
	selectionID := func(selection CompactKbuildSelection) string {
		return compactKbuildSelectionKeyString(compactKbuildSelectionKey{
			profile: selection.Profile,
			target:  selection.Target,
			stage:   selection.Stage,
		})
	}

	producerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	producerRecipeID, err := producerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	consumerRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "objcopy",
		Arguments: []string{"${input:src:00000000}", "${output:00000000}"},
		Inputs:    []string{"src:00000000"}, Outputs: []string{"00000000"},
	}
	consumerRecipeID, err := consumerRecipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	producer := ActionPlanNode{
		ID: selectionID(selections[0]), Stage: "target", Kind: "generate", Recipe: producerRecipeID,
		Tool: "cc", Product: "vmlinux", Outputs: []ActionPlanOutput{{Tree: "objects", Path: selections[0].Target}},
	}
	consumer := ActionPlanNode{
		ID: selectionID(selections[1]), Stage: "target", Kind: "copy", Recipe: consumerRecipeID,
		Tool: "objcopy", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "src", ProducerID: producer.ID}},
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: selections[1].Target}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes: map[string]ActionRecipe{
			producerRecipeID: producerRecipe,
			consumerRecipeID: consumerRecipe,
		},
		Nodes: []ActionPlanNode{producer, consumer},
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Nodes[0].ID, plan.Nodes[0].ContentID(); got != want || got == producer.ID {
		t.Fatalf("producer content ID = %q, want %q distinct from provisional %q", got, want, producer.ID)
	}
	if got, want := plan.Nodes[1].Inputs[0].ProducerID, plan.Nodes[0].ID; got != want {
		t.Fatalf("consumer producer edge = %q, want readdressed producer %q", got, want)
	}
	if got, want := plan.Nodes[1].ID, plan.Nodes[1].ContentID(); got != want || got == consumer.ID {
		t.Fatalf("consumer content ID = %q, want %q distinct from provisional %q", got, want, consumer.ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("content-addressed selected graph is invalid: %v", err)
	}
}
