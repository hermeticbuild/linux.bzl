package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestRunProbeProjectsDynamicCompilerIntrinsicArguments(t *testing.T) {
	type testCase struct {
		name      string
		arguments []string
		invalid   bool
	}
	tests := []testCase{
		{"exact macro values", []string{"-nostdinc", "-DVALUE=202311", "-D", "deprecated=other", "-UFLAG", "-DFLAG=7"}, false},
		{"function macro exact", []string{"-DOP(x)=202311", "-DVALUE=0x31647"}, false},
		{"frontend forwarding", []string{"-Xclang", "-load", "plugin.so"}, true},
		{"response file", []string{"@flags.rsp"}, true},
		{"response after macro", []string{`-DPATH="dir/file"`, "@flags.rsp"}, true},
		{"response as split operand", []string{"-D", "@flags.rsp"}, true},
		{"plugin", []string{"-fplugin=/undeclared/plugin.so"}, true},
		{"precompiled header", []string{"-include-pch", "undeclared/header.pch"}, true},
		{"preprocessor forwarding", []string{`-Wp,-DPATH="dir/file"`}, true},
		{"unowned source", []string{"unowned/source.c"}, true},
		{"pragma composition", []string{`-D__has_attribute(x)=_Pragma(P)`, `-DP="GCC dependency \"/undeclared/file\""`}, true},
	}
	for _, macro := range []struct {
		name, definition string
		invalid          bool
	}{
		{"kernel module path", `KBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/arch/x86/boot/regs"`, false},
		{"arbitrary relative path", `OTHER_VALUE="arbitrary/source/file"`, false},
		{"arbitrary absolute path", `OTHER_VALUE="/undeclared/does-not-exist"`, false},
		{"source marker", `OTHER_VALUE="__LINUX_BZL_SOURCE_TREE__/include/missing.h"`, false},
		{"parent path", `OTHER_VALUE="../outside/not-an-input"`, false},
		{"quoted spaces", `OTHER_VALUE="dir/hello world"`, false},
		{"literal dollar", `OTHER_VALUE="$unexpanded/$(not_executed)"`, false},
		{"literal response name", `OTHER_VALUE="@not-a-response/path.rsp"`, false},
		{"backslashes", `OTHER_VALUE="C:\\not\\an\\input"`, false},
		{"escaped quote", `OTHER_VALUE="dir/\"quoted\""`, false},
		{"C escapes", `OTHER_VALUE="dir/\141\x62\t\n"`, false},
		{"unquoted path", `OTHER_VALUE=dir/file`, true},
		{"quoted function replacement", `OTHER_VALUE(x)="dir/file"`, true},
		{"unterminated string", `OTHER_VALUE="dir/file`, true},
		{"concatenated strings", `OTHER_VALUE="dir/file""suffix"`, true},
		{"trailing expression", `OTHER_VALUE="dir/file"+1`, true},
		{"wide string", `OTHER_VALUE=L"dir/file"`, true},
		{"UTF8 string prefix", `OTHER_VALUE=u8"dir/file"`, true},
		{"unknown escape", `OTHER_VALUE="dir/\q"`, true},
		{"universal escape", `OTHER_VALUE="dir/\u002f"`, true},
		{"empty hex escape", `OTHER_VALUE="dir/\x"`, true},
		{"trigraph", `OTHER_VALUE="dir/??/"`, true},
		{"malformed macro name", `NOT-A-NAME="dir/file"`, true},
		{"operator definition", "__has_attribute=0", true},
		{"operator bare definition", "__has_attribute", true},
		{"operator function definition", "__has_attribute(x)=1", true},
	} {
		tests = append(tests,
			testCase{macro.name + "/attached", []string{"-D" + macro.definition}, macro.invalid},
			testCase{macro.name + "/split", []string{"-D", macro.definition}, macro.invalid},
		)
	}
	tests = append(tests,
		testCase{"operator undef attached", []string{"-U__has_attribute"}, true},
		testCase{"operator undef split", []string{"-U", "__has_attribute"}, true},
		testCase{"pragma composition split", []string{"-D", "__has_attribute(x)=_Pragma(P)", "-D", `P="GCC dependency \"/undeclared/file\""`}, true},
	)
	for _, option := range []string{"--define-macro", "--define-macr", "--undefine-macro", "--undefine-macr"} {
		operand := "__has_attribute"
		if strings.HasPrefix(option, "--define") {
			operand += "=0"
		}
		tests = append(tests,
			testCase{option + "/attached", []string{option + "=" + operand}, true},
			testCase{option + "/split", []string{option, operand}, true},
		)
	}
	for _, test := range tests {
		for _, dynamic := range []bool{false, true} {
			transport := "literal"
			invalid := test.invalid
			if dynamic {
				transport = "fragment"
				// Make-text fragments split whitespace, not shell words. A
				// quoted space must not silently acquire shell parsing.
				for _, argument := range test.arguments {
					if len(strings.Fields(argument)) != 1 {
						invalid = true
					}
				}
			}
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				runCompilerIntrinsicArgumentsForTest(t, test.arguments, dynamic, invalid)
			})
		}
	}
}

// Use a full canonical request/result envelope in both cases. Literal words are
// Candidate.Base entries; dynamic words are owned by one measured text fragment.
// Neither input form may rewrite macro text into filesystem operands.
func runCompilerIntrinsicArgumentsForTest(t *testing.T, arguments []string, dynamic, invalid bool) {
	t.Helper()
	mode := "literal"
	if dynamic {
		mode = "fragment"
	}
	runCompilerIntrinsicSourceArgumentsForTest(t, arguments, mode, "#undef deprecated\n__has_attribute(deprecated)\n", invalid)
}

func TestRunProbeProtectsExactlyInvokedCompilerIntrinsics(t *testing.T) {
	attribute := "#undef deprecated\n__has_attribute(deprecated)\n"
	builtin := "#undef __builtin_trap\n__has_builtin(__builtin_trap)\n"
	for _, source := range []struct {
		name, stdin        string
		attribute, builtin bool
	}{
		{"attribute", attribute, true, false},
		{"builtin", builtin, false, true},
		{"mixed", attribute + builtin, true, true},
	} {
		for _, operator := range []string{"__has_attribute", "__has_builtin"} {
			invoked := source.attribute
			if operator == "__has_builtin" {
				invoked = source.builtin
			}
			for _, test := range []struct {
				name      string
				arguments []string
				invalid   bool
			}{
				{"define attached", []string{"-D" + operator + "(x)=0"}, invoked},
				{"define split", []string{"-D", operator + "(x)=0"}, invoked},
				{"undef attached", []string{"-U" + operator}, invoked},
				{"undef split", []string{"-U", operator}, invoked},
				{"redefine after undef", []string{"-U" + operator, "-D" + operator + "(x)=7"}, invoked},
				{"pragma composition", []string{"-D" + operator + "(x)=_Pragma(P)", `-DP="GCC dependency \"/undeclared/path\""`}, invoked},
				{"alias attached", []string{"--define-macr=" + operator + "(x)=0"}, true},
				{"alias split", []string{"--undefine-macr", operator}, true},
				{"macro literal path", []string{`-DVALUE="__LINUX_BZL_OBJECT_TREE__/somewhere/file"`}, false},
			} {
				for _, mode := range []string{"literal", "fragment", "source-shell"} {
					// The legacy text mode does not grant shell quoting to spaces.
					if mode == "fragment" && test.name == "pragma composition" {
						continue
					}
					t.Run(source.name+"/"+operator+"/"+test.name+"/"+mode, func(t *testing.T) {
						runCompilerIntrinsicSourceArgumentsForTest(t, test.arguments, mode, source.stdin, test.invalid)
					})
				}
			}
		}
	}
}

func runCompilerIntrinsicSourceArgumentsForTest(t *testing.T, arguments []string, mode, stdin string, invalid bool) {
	runCompilerIntrinsicNamedSourceArgumentsForTest(t, arguments, mode, "compiler-intrinsic-integer", stdin, invalid)
}

func runCompilerIntrinsicNamedSourceArgumentsForTest(t *testing.T, arguments []string, mode, stepName, stdin string, invalid bool) {
	t.Helper()
	dir := t.TempDir()
	identity := "sha256-" + strings.Repeat("6", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidates := append(slices.Clone(arguments), "-DMANAGED=202311",
		"-include", "undeclared/forced.h", "-Iundeclared/include", "-c", "source.c",
		"-o", "source.o", "-MMD", "-MF", "source.d")
	tail := []string{"-E", "-P", "-x", "c", "-"}
	step := kconfig.ProbeStep{Name: stepName, Tool: "cc",
		Stdin: stdin,
		Candidate: &kconfig.ProbeCandidateArguments{Policy: kconfig.ProbeCandidatePolicyCC,
			Projection: kconfig.ProbeCandidateProjectionCompilerIntrinsic, TranslationUnits: []string{"source.c"}},
	}
	var inputNodeIDs []string
	inputs := map[string]string{}
	if mode != "literal" {
		text := strings.Join(candidates, " ")
		fragmentMode := ""
		if mode == "source-shell" {
			fragmentMode = kconfig.ProbeArgumentFragmentsModeSourceShellWords
			quoted := make([]string, len(candidates))
			for index, argument := range candidates {
				quoted[index] = "'" + strings.ReplaceAll(argument, "'", "'\\''") + "'"
			}
			text = strings.Join(quoted, " ")
		} else if mode != "fragment" {
			t.Fatalf("unknown intrinsic argument mode %q", mode)
		}
		dependency := kconfig.ProbeResult{
			Schema: kconfig.LinuxProbeResultSchema, NodeID: strings.Repeat("7", 64), RequestID: strings.Repeat("8", 64),
			Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: text,
		}
		dependencyData, err := dependency.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		dependencyPath := filepath.Join(dir, "input.json")
		if err := os.WriteFile(dependencyPath, dependencyData, 0o600); err != nil {
			t.Fatal(err)
		}
		inputs["00000000"] = dependencyPath
		inputNodeIDs = []string{dependency.NodeID}
		step.Arguments = append([]string{""}, tail...)
		step.ArgumentFragments = []kconfig.ProbeArgumentFragments{{Index: 0, Mode: fragmentMode, Fragments: []kconfig.ProbeValueFragment{{Value: "${result:00000000.text}"}}}}
		step.Candidate.Base = []int{0}
	} else {
		step.Arguments = append(slices.Clone(candidates), tail...)
		for index := range candidates {
			step.Candidate.Base = append(step.Candidate.Base, index)
		}
	}
	request := kconfig.ProbeRequest{Schema: kconfig.LinuxProbeRequestSchema, InputCount: len(inputNodeIDs),
		Steps:   []kconfig.ProbeStep{step},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: stepName, Stream: "stdout", RequireSuccess: true},
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
	wantArguments := append(slices.Clone(arguments), "-DMANAGED=202311")
	want, err := json.Marshal(append(wantArguments, tail...))
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker}, inputs: inputs,
		tools: map[string]actionContract{"cc": {path: executable,
			arguments:   []string{"-test.run=TestProbeHelperProcess", "--", "exact-json-arguments", kconfig.LinuxKbuildArgsSentinel},
			environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "EXPECTED_ARGUMENTS_JSON": string(want)},
		}},
	})
	if invalid {
		if err == nil || !strings.Contains(err.Error(), "candidate") {
			t.Fatalf("expected candidate rejection before helper execution, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "accepted" {
		t.Fatalf("projected result = %#v, %v", result, err)
	}
}
