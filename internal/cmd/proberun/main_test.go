package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

func TestProbeHelperProcess(t *testing.T) {
	if os.Getenv("LINUX_BZL_PROBE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) != 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	switch args[0] {
	case "emit":
		if err := os.WriteFile(args[1], []byte("object"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Print("supported\nmore\n")
	case "fail":
		fmt.Fprint(os.Stderr, "unsupported")
		os.Exit(7)
	case "envelope":
		if got := strings.Join(args[1:], " "); got != "prefix linux suffix" || os.Getenv("CONTRACT_VALUE") != "exact" || os.Getenv("HOME") != "" {
			fmt.Fprintf(os.Stderr, "argv=%q CONTRACT_VALUE=%q HOME=%q", got, os.Getenv("CONTRACT_VALUE"), os.Getenv("HOME"))
			os.Exit(9)
		}
		fmt.Print("exact")
	case "transformed-arguments":
		if got, want := strings.Join(args[1:], " "), os.Getenv("EXPECTED_ARGUMENTS"); got != want {
			fmt.Fprintf(os.Stderr, "arguments=%q want=%q", got, want)
			os.Exit(19)
		}
		fmt.Print("accepted")
	case "flood":
		fmt.Print(strings.Repeat("x", 1024))
	case "multiline":
		fmt.Print("supported\nmore\n")
	case "hang":
		time.Sleep(2 * time.Second)
	case "dependency":
		if got := strings.Join(args[1:], " "); got != "selected=true base" {
			fmt.Fprintf(os.Stderr, "arguments=%q", got)
			os.Exit(11)
		}
		fmt.Print("accepted")
	case "fragments":
		stdin, err := io.ReadAll(os.Stdin)
		dynamicMatches := len(args) < 4 || os.Getenv("FILTERED") == args[3]
		if err != nil || os.Getenv("SELECTED") != args[1] || string(stdin) != args[2] || !dynamicMatches {
			fmt.Fprintf(os.Stderr, "SELECTED=%q FILTERED=%q stdin=%q error=%v", os.Getenv("SELECTED"), os.Getenv("FILTERED"), stdin, err)
			os.Exit(18)
		}
		fmt.Print("accepted")
	case "emit-path":
		fmt.Print(args[1])
	case "inspect-header":
		content, err := os.ReadFile(args[1])
		if err != nil || string(content) != "#define PROBE 1\n" {
			fmt.Fprintf(os.Stderr, "header=%q error=%v", content, err)
			os.Exit(12)
		}
		fmt.Print("tool available\n")
	case "inspect-source":
		content, err := os.ReadFile(args[1])
		workingDirectory, cwdErr := os.Getwd()
		if err != nil || cwdErr != nil || filepath.Clean(workingDirectory) != filepath.Dir(filepath.Clean(args[2])) {
			fmt.Fprintf(os.Stderr, "source=%q read=%v cwd=%q cwd_error=%v anchor=%q", content, err, workingDirectory, cwdErr, args[2])
			os.Exit(13)
		}
		fmt.Print(string(content))
	case "inspect-execution-root":
		content, err := os.ReadFile(args[1])
		if err != nil || string(content) != "selected resource\n" || !filepath.IsAbs(args[1]) || os.Getenv("RESOURCE_HEADER") != args[1] {
			fmt.Fprintf(os.Stderr, "resource=%q content=%q error=%v environment=%q", args[1], content, err, os.Getenv("RESOURCE_HEADER"))
			os.Exit(17)
		}
		fmt.Print(args[1])
	case "inspect-auxiliary-contracts":
		contracts, err := toolaction.Decode(os.Getenv(toolaction.EnvironmentName))
		contract, exists := contracts["cc"]
		if err != nil || !exists || len(contracts) != 1 || strings.Join(contract.Arguments, "|") != "prefix|"+toolaction.KbuildArgumentsSentinel+"|suffix" || contract.Environment["SELECTED_ENV"] != "exact=value" || os.Getenv("PRIMARY_ENV") != "primary" || os.Getenv("STEP_ENV") != "step" || !filepath.IsAbs(args[1]) {
			fmt.Fprintf(os.Stderr, "contracts=%#v error=%v argv=%q primary=%q step=%q", contracts, err, args[1:], os.Getenv("PRIMARY_ENV"), os.Getenv("STEP_ENV"))
			os.Exit(14)
		}
		fmt.Print("forwarded")
	case "runtime-link":
		if os.Getenv("CONTRACT_VALUE") != "link" {
			fmt.Fprintf(os.Stderr, "CONTRACT_VALUE=%q", os.Getenv("CONTRACT_VALUE"))
			os.Exit(15)
		}
		output := ""
		for index := 1; index+1 < len(args); index++ {
			if args[index] == "-o" {
				output = args[index+1]
				break
			}
		}
		command := exec.Command("ld", output)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if output == "" || command.Run() != nil {
			os.Exit(16)
		}
		fmt.Print("link-runtime")
	case "compound-cc":
		compoundProbeCompiler(args[1:])
	case "compound-nm":
		compoundProbeNM(args[1:])
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func compoundProbeCompiler(arguments []string) {
	if os.Getenv("TMP") != "" {
		fmt.Fprintf(os.Stderr, "TMP was exported to compiler: %q", os.Getenv("TMP"))
		os.Exit(20)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(21)
	}
	output := ""
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == "-o" {
			output = arguments[index+1]
			break
		}
	}
	if output == "" {
		fmt.Fprintf(os.Stderr, "compiler arguments omit -o: %q", arguments)
		os.Exit(22)
	}
	fmt.Fprintf(os.Stdout, "compiler pid=%d output=%s\n", os.Getpid(), output)
	fmt.Fprintf(os.Stderr, "compiler diagnostic pid=%d output=%s\n", os.Getpid(), output)
	isBase := strings.HasSuffix(output, ".base")
	if !isBase && os.Getenv("COMPOUND_PROBE_SCENARIO") == "second-compile-fails" {
		os.Exit(23)
	}
	marker := "test"
	if isBase {
		marker = "base"
	}
	if err := os.WriteFile(output, []byte(marker), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(24)
	}
}

func compoundProbeNM(arguments []string) {
	if os.Getenv("TMP") != "" {
		fmt.Fprintf(os.Stderr, "TMP was exported to nm: %q", os.Getenv("TMP"))
		os.Exit(25)
	}
	if len(arguments) != 1 {
		fmt.Fprintf(os.Stderr, "nm arguments = %q, want one object", arguments)
		os.Exit(26)
	}
	marker, err := os.ReadFile(arguments[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(27)
	}
	fmt.Fprintf(os.Stderr, "nm diagnostic pid=%d input=%s\n", os.Getpid(), arguments[0])
	if string(marker) == "test" && os.Getenv("COMPOUND_PROBE_SCENARIO") == "changed-undefined-symbols" {
		fmt.Print("         U changed_symbol\n")
		return
	}
	fmt.Print("         U shared_symbol\n")
}

func TestSelectProbeStepContractRoleUsesCandidatePolicy(t *testing.T) {
	tests := []struct {
		name      string
		step      kconfig.ProbeStep
		arguments []string
		want      string
	}{
		{
			name: "candidate scalar cannot consume managed compile mode into link",
			step: kconfig.ProbeStep{
				Name: "compile", Tool: "cc",
				Candidate: &kconfig.ProbeCandidateArguments{Policy: kconfig.ProbeCandidatePolicyCC, Base: []int{0}},
			},
			arguments: []string{"-D", "-c", "-o", "probe.o", "-"},
			want:      "cc",
		},
		{
			name: "link policy ignores rendered compile token",
			step: kconfig.ProbeStep{
				Name: "link", Tool: "cc",
				Candidate: &kconfig.ProbeCandidateArguments{Policy: kconfig.ProbeCandidatePolicyCCLink, Base: []int{0}},
			},
			arguments: []string{"-c", "-o", "probe"},
			want:      "cc-link",
		},
		{
			name: "non-driver remains primary",
			step: kconfig.ProbeStep{
				Name: "link", Tool: "ld",
				Candidate: &kconfig.ProbeCandidateArguments{Policy: kconfig.ProbeCandidatePolicyCCLink, Base: []int{0}},
			},
			arguments: []string{"-o", "probe"},
			want:      "ld",
		},
		{
			name:      "managed invocation retains semantic dispatch",
			step:      kconfig.ProbeStep{Name: "link", Tool: "cc"},
			arguments: []string{"input.o", "-o", "probe"},
			want:      "cc-link",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectProbeStepContractRole(test.step, test.arguments)
			if err != nil || got != test.want {
				t.Fatalf("contract role = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestValidateProbeRequestEnvironmentNameProtectsExecutionEnvelope(t *testing.T) {
	contract := map[string]string{"CONTRACT_VALUE": "exact"}
	for _, name := range []string{
		"CONTRACT_VALUE",
		"LD_PRELOAD",
		"DYLD_INSERT_LIBRARIES",
		"PATH",
		"BASH_ENV",
		"GCC_EXEC_PREFIX",
		"LANG",
		toolaction.EnvironmentName,
		toolaction.RuntimeToolPathEnvironmentName,
		toolsetpath.HandoffEnvironmentName,
	} {
		t.Run("reject "+name, func(t *testing.T) {
			if err := validateProbeRequestEnvironmentName(name, contract); err == nil {
				t.Fatalf("environment authority %s was accepted", name)
			}
		})
	}
	for _, name := range []string{"ARCH", "CC", "KBUILD_CFLAGS", "ROLE_FREE", "srctree"} {
		t.Run("preserve "+name, func(t *testing.T) {
			if err := validateProbeRequestEnvironmentName(name, contract); err != nil {
				t.Fatalf("ordinary Kbuild environment %s was rejected: %v", name, err)
			}
		})
	}
}

func TestApplyProbeRequestEnvironmentValueAcceptsOnlyRedundantCLocale(t *testing.T) {
	for _, name := range []string{"LANG", "LC_ALL"} {
		t.Run(name, func(t *testing.T) {
			environment := map[string]string{"LANG": "C", "LC_ALL": "C"}
			if err := applyProbeRequestEnvironmentValue(environment, map[string]string{}, name, "C"); err != nil {
				t.Fatalf("redundant deterministic locale was rejected: %v", err)
			}
			if err := applyProbeRequestEnvironmentValue(environment, map[string]string{}, name, "en_US.UTF-8"); err == nil {
				t.Fatal("request-controlled locale was accepted")
			}
			if err := applyProbeRequestEnvironmentValue(environment, map[string]string{name: "contract"}, name, "C"); err == nil {
				t.Fatal("identity-bound locale collision was accepted")
			}
		})
	}
}

func TestResolveProbeWorkingDirectoryCanonicalizesAndRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	sourceRoot := filepath.Join(parent, "linux")
	inside := filepath.Join(sourceRoot, "scripts")
	outside := filepath.Join(parent, "outside")
	for _, directory := range []string{inside, outside} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("scripts", filepath.Join(sourceRoot, "inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sourceRoot, "escape-link")); err != nil {
		t.Fatal(err)
	}
	expand := func(value string) (string, error) {
		return strings.Replace(value, "${source_root:linux}", sourceRoot, 1), nil
	}
	got, err := resolveProbeWorkingDirectory(
		"${source_root:linux}/inside-link", t.TempDir(), map[string]string{"linux": sourceRoot}, expand,
	)
	if err != nil || got != inside {
		t.Fatalf("resolved in-tree cwd = %q, %v; want %q", got, err, inside)
	}
	if _, err := resolveProbeWorkingDirectory(
		"${source_root:linux}/escape-link", t.TempDir(), map[string]string{"linux": sourceRoot}, expand,
	); err == nil || !strings.Contains(err.Error(), "outside declared source root") {
		t.Fatalf("cwd symlink escape error = %v", err)
	}
}

func TestRunProbeDispatchesDriverLinkContractWithRuntimeTools(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Scratch: []kconfig.ProbeScratch{{Name: "output", Kind: "file"}},
		Steps:   []kconfig.ProbeStep{{Name: "link", Tool: "cc", Arguments: []string{"first.o", "-o", "${scratch:output}"}}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "link", Stream: "stdout"},
	}
	requestID, _ := request.ID()
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	_ = os.WriteFile(requestPath, data, 0o600)
	identity := "sha256-" + strings.Repeat("f", 64)
	marker := filepath.Join(dir, identity)
	_ = os.WriteFile(marker, nil, 0o600)
	linker := filepath.Join(dir, "selected-ld")
	_ = os.WriteFile(linker, []byte("#!/bin/sh\nprintf linked > \"$1\"\n"), 0o700)
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker}, runtimeTools: map[string]string{"ld": linker},
		tools: map[string]actionContract{
			"cc":      {path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", "fail", kconfig.LinuxKbuildArgsSentinel}, environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "CONTRACT_VALUE": "compile"}},
			"cc-link": {path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", "runtime-link", kconfig.LinuxKbuildArgsSentinel}, environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "CONTRACT_VALUE": "link"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "link-runtime" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunProbeMaterializesInlineScratchAndMatchesStream(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Scratch: []kconfig.ProbeScratch{{Name: "probe.h", Kind: "file", Content: "#define PROBE 1\n"}},
		Steps: []kconfig.ProbeStep{{
			Name: "inspect", Tool: "tool", Arguments: []string{"inspect-header", "${scratch:probe.h}"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{
			Operator: "stream-matches", Step: "inspect", Stream: "stdout", Value: `(?m)^tool available$`,
		}},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	data, _ := request.CanonicalJSON()
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("b", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	executable, _ := os.Executable()
	resultPath := filepath.Join(dir, "result.json")
	if err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"tool": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Boolean == nil || !*result.Boolean {
		t.Fatalf("stream match result = %#v, %v", result, err)
	}
}

func TestRunProbeBindsDeclaredSourcesAndAnchorBeforeWorkingDirectory(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot")
	for _, relative := range []string{"plan", "identities", "src/scripts", "tools", "scratch"} {
		if err := os.MkdirAll(filepath.Join(execroot, filepath.FromSlash(relative)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(execroot, "src", "Kconfig"), []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execroot, "src", "scripts", "probe.sh"), []byte("dynamic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema:      kconfig.LinuxProbeRequestSchema,
		Sources:     []string{"Kconfig", "scripts/probe.sh"},
		SourceRoots: []string{"linux"},
		Steps: []kconfig.ProbeStep{{
			Name: "source", Tool: "runner", WorkingDirectory: "${source_root:linux}",
			Arguments:   []string{"inspect-source", "${source:scripts/probe.sh}", "${source:Kconfig}"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "source", Stream: "stdout"},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, _ := request.CanonicalJSON()
	if err := os.WriteFile(filepath.Join(execroot, "plan", "request.json"), requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("4", 64)
	if err := os.WriteFile(filepath.Join(execroot, "identities", identity), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(execroot, "tools", "proberun-test")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	if err := runTestProbe(t, probeOptions{
		request: "plan/request.json", result: "result.json", nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": "identities/" + identity},
		tools: map[string]actionContract{"runner": {
			path: "tools/proberun-test", arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
		sources: map[string]string{
			"Kconfig":          "src/Kconfig",
			"scripts/probe.sh": "src/scripts/probe.sh",
		},
		sourceRootAnchors: map[string]string{"linux": "src/Kconfig"},
		tempDir:           "scratch",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(filepath.Join(execroot, "result.json"))
	if err != nil || result.Text != "dynamic\n" {
		t.Fatalf("source-backed result = %#v, %v", result, err)
	}
}

func TestRunProbeExpandsToolchainPathsBeforeScratchWorkingDirectory(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot")
	resource := filepath.Join(execroot, "external", "toolchain", "lib", "clang", "22", "include", "stddef.h")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("selected resource\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Steps:   []kconfig.ProbeStep{{Name: "resource", Tool: "cc"}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "resource", Stream: "stdout", TrimSpace: true},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, _ := request.CanonicalJSON()
	requestPath := filepath.Join(execroot, "request.json")
	if err := os.MkdirAll(execroot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("6", 64)
	identityPath := filepath.Join(execroot, identity)
	if err := os.WriteFile(identityPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	markerPath := toolaction.ExecutionRootMarker + "/external/toolchain/lib/clang/22/include/stddef.h"
	resultPath := filepath.Join(execroot, "result.json")
	if err := runTestProbeWithToolset(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": identityPath},
		tools: map[string]actionContract{"cc": {
			path: executable,
			arguments: []string{
				"-test.run=TestProbeHelperProcess",
				"--",
				"inspect-execution-root",
				markerPath,
				kconfig.LinuxKbuildArgsSentinel,
			},
			environment: map[string]string{
				"LINUX_BZL_PROBE_HELPER": "1",
				"RESOURCE_HEADER":        markerPath,
			},
		}},
	}, resource); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != resource {
		t.Fatalf("resource-path result = %#v, %v; want %q", result, err, resource)
	}
}

func TestRunProbeRejectsMissingExtraAndNonRegularSources(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "source")
	if err := os.WriteFile(regular, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Sources: []string{"script"},
		Steps: []kconfig.ProbeStep{{
			Name: "source", Tool: "runner", Arguments: []string{"inspect-source", "${source:script}", "${source:script}"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "source", Stream: "stdout"},
	}
	requestID, _ := request.ID()
	requestData, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("3", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	executable, _ := os.Executable()
	base := probeOptions{
		request: requestPath, result: filepath.Join(dir, "result.json"), nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"runner": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	}
	for name, sources := range map[string]map[string]string{
		"missing":     {},
		"extra":       {"script": regular, "extra": regular},
		"non-regular": {"script": directory},
	} {
		t.Run(name, func(t *testing.T) {
			opts := base
			opts.sources = sources
			err := runProbe(opts)
			if err == nil || name == "non-regular" && !strings.Contains(err.Error(), "regular file") || name != "non-regular" && !strings.Contains(err.Error(), "source bindings") {
				t.Fatalf("source binding error = %v", err)
			}
		})
	}
}

func TestResolveProbeSourceRootsRequiresExactDeclaredAnchors(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "source-root")
	otherRoot := filepath.Join(dir, "other-root")
	for _, directory := range []string{root, otherRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	anchor := filepath.Join(root, "Kconfig")
	otherAnchor := filepath.Join(otherRoot, "Kconfig")
	for _, filename := range []string{anchor, otherAnchor} {
		if err := os.WriteFile(filename, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources := map[string]string{"Kconfig": anchor}
	resolved, err := resolveProbeSourceRoots([]string{"linux"}, sources, nil, nil, map[string]string{"linux": anchor}, nil)
	if err != nil || resolved["linux"] != root {
		t.Fatalf("resolved source root = %q, %v; want %q", resolved, err, root)
	}
	for name, test := range map[string]struct {
		directories map[string]string
		anchors     map[string]string
		want        string
	}{
		"missing": {want: "source root bindings"},
		"extra": {
			anchors: map[string]string{"linux": anchor, "rust": anchor},
			want:    "source root bindings",
		},
		"both binding modes": {
			directories: map[string]string{"linux": root},
			anchors:     map[string]string{"linux": anchor},
			want:        "both directory and anchor",
		},
		"undeclared anchor": {
			anchors: map[string]string{"linux": otherAnchor},
			want:    "not a declared source artifact",
		},
		"root without source": {
			directories: map[string]string{"linux": otherRoot},
			want:        "contains no declared source artifact",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveProbeSourceRoots([]string{"linux"}, sources, test.directories, nil, test.anchors, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("source root error = %v, want %q", err, test.want)
			}
		})
	}
	resolved, err = resolveProbeSourceRoots([]string{"host_deps"}, sources, nil, map[string]string{"host_deps": otherRoot}, nil, nil)
	if err != nil || resolved["host_deps"] != otherRoot {
		t.Fatalf("declared tree source root = %q, %v; want %q", resolved, err, otherRoot)
	}
}

func TestResolveProbeSourceRootsAcceptsDeclaredDirectoryWitness(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "rust", "library")
	witness := filepath.Join(root, "core", "src", "lib.rs")
	outside := filepath.Join(dir, "outside.rs")
	if err := os.MkdirAll(filepath.Dir(witness), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{witness, outside} {
		if err := os.WriteFile(filename, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := resolveProbeSourceRoots(
		[]string{"rust"},
		nil,
		map[string]string{"rust": root},
		nil,
		nil,
		map[string]string{"rust": witness},
	)
	if err != nil || resolved["rust"] != root {
		t.Fatalf("witness-backed source root = %q, %v; want %q", resolved["rust"], err, root)
	}
	for name, witnesses := range map[string]map[string]string{
		"outside root":        {"rust": outside},
		"unknown root":        {"other": witness},
		"non-regular witness": {"rust": filepath.Dir(witness)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveProbeSourceRoots(
				[]string{"rust"},
				nil,
				map[string]string{"rust": root},
				nil,
				nil,
				witnesses,
			)
			if err == nil {
				t.Fatal("invalid source-root witness was accepted")
			}
		})
	}
}

func TestResolveProbeArtifactPathsRejectsNoncanonicalSourceRoot(t *testing.T) {
	for _, value := range []string{"../escape", "/absolute", "source\\root", "source/../root"} {
		if _, err := resolveProbeArtifactPaths(probeOptions{sourceRoots: map[string]string{"linux": value}}); err == nil {
			t.Errorf("source root %q was accepted", value)
		}
	}
}

func TestRunProbeForwardsExactlyDeclaredAuxiliaryActionContracts(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "inspect", Tool: "runner", AuxiliaryTools: []string{"cc"},
			Arguments:   []string{"inspect-auxiliary-contracts", "${tool:cc}", "${tool:ld}"},
			Environment: map[string]string{"STEP_ENV": "step"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "inspect", Stream: "stdout"},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("2", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	if err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{
			"runner": {
				path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
				environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1", "PRIMARY_ENV": "primary"},
			},
			"cc": {
				path: executable, arguments: []string{"prefix", toolaction.KbuildArgumentsSentinel, "suffix"},
				environment: map[string]string{"SELECTED_ENV": "exact=value"},
			},
			"ld": {path: executable, arguments: []string{}, environment: map[string]string{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "forwarded" {
		t.Fatalf("auxiliary contract result = %#v, %v", result, err)
	}
}

func TestRunProbeNormalizesReportedExecrootPath(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot")
	if err := os.MkdirAll(filepath.Join(execroot, "external", "toolchain", "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "query", Tool: "cc", StdoutExecrootRelative: true, StdoutFallbackPath: "plugin",
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "query", Stream: "stdout", TrimSpace: true},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := request.CanonicalJSON()
	if err := os.WriteFile("request.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("9", 64)
	if err := os.WriteFile(identity, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	executable, _ := os.Executable()
	run := func(reported, result string, closureArtifacts ...string) error {
		if len(closureArtifacts) == 0 {
			closureArtifacts = []string{filepath.Join(execroot, "external", "toolchain", "include")}
		}
		return runTestProbeWithToolset(t, probeOptions{
			request: "request.json", result: result, nodeID: node.ID, requestID: requestID, scope: "target",
			toolsetMarkers: map[string]string{"target": identity},
			tools: map[string]actionContract{"cc": {
				path:      executable,
				arguments: []string{"-test.run=TestProbeHelperProcess", "--", "emit-path", reported, kconfig.LinuxKbuildArgsSentinel},
			}},
		}, closureArtifacts...)
	}
	if err := run(filepath.Join(execroot, "external", "toolchain", "include")+"\n", "result.json"); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult("result.json")
	if err != nil || result.Text != "external/toolchain/include" || result.Steps[0].Stdout != result.Text || result.Steps[0].StdoutPathKind != kconfig.ProbeStdoutPathToolset {
		t.Fatalf("normalized path result = %#v, %v", result, err)
	}
	if err := run("plugin\n", "fallback.json"); err != nil {
		t.Fatalf("literal fallback path: %v", err)
	}
	fallback, err := kconfig.ReadProbeResult("fallback.json")
	if err != nil || fallback.Text != "plugin" || fallback.Steps[0].Stdout != fallback.Text || fallback.Steps[0].StdoutPathKind != kconfig.ProbeStdoutPathFallback {
		t.Fatalf("fallback path result = %#v, %v", fallback, err)
	}
	if err := run(filepath.Join(execroot, "plugin"), "absolute-fallback.json"); err != nil {
		t.Fatalf("unbound absolute fallback path: %v", err)
	}
	absoluteFallback, err := kconfig.ReadProbeResult("absolute-fallback.json")
	if err != nil || absoluteFallback.Text != "plugin" || absoluteFallback.Steps[0].Stdout != absoluteFallback.Text || absoluteFallback.Steps[0].StdoutPathKind != kconfig.ProbeStdoutPathFallback {
		t.Fatalf("absolute fallback path result = %#v, %v", absoluteFallback, err)
	}
	if err := os.MkdirAll(filepath.Join(execroot, "plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run("plugin\n", "exact-sentinel-artifact.json", filepath.Join(execroot, "plugin")); err != nil {
		t.Fatalf("real artifact with fallback spelling: %v", err)
	}
	exact, err := kconfig.ReadProbeResult("exact-sentinel-artifact.json")
	if err != nil || exact.Text != "plugin" || exact.Steps[0].StdoutPathKind != kconfig.ProbeStdoutPathToolset {
		t.Fatalf("exact-sentinel artifact result = %#v, %v", exact, err)
	}
	outside := filepath.Join(filepath.Dir(execroot), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(outside, "outside.json"); err == nil || !strings.Contains(err.Error(), "outside the execroot") {
		t.Fatalf("outside path error = %v", err)
	}
}

func TestRunProbeRebasesRepositoryCachePathThroughConfiguredTool(t *testing.T) {
	root := t.TempDir()
	execroot := filepath.Join(root, "execroot")
	repository := filepath.Join(root, "repository-cache", "cc-toolchain")
	for _, directory := range []string{
		filepath.Join(execroot, "external"),
		filepath.Join(repository, "bin"),
		filepath.Join(repository, "lib", "gcc", "include"),
		filepath.Join(repository, "lib", "gcc", "plugin", "include"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(repository, "bin", "proberun-test")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repository, filepath.Join(execroot, "external", "cc-toolchain")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "query", Tool: "cc", StdoutExecrootRelative: true, StdoutFallbackPath: "plugin",
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "query", Stream: "stdout", TrimSpace: true},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, _ := request.CanonicalJSON()
	if err := os.WriteFile("request.json", requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("5", 64)
	if err := os.WriteFile(identity, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	run := func(reported, output string) error {
		return runTestProbeWithToolset(t, probeOptions{
			request: "request.json", result: output, nodeID: node.ID, requestID: requestID, scope: "target",
			toolsetMarkers: map[string]string{"target": identity},
			tools: map[string]actionContract{"cc": {
				path:      "external/cc-toolchain/bin/proberun-test",
				arguments: []string{"-test.run=TestProbeHelperProcess", "--", "emit-path", reported, kconfig.LinuxKbuildArgsSentinel},
			}},
		}, "external/cc-toolchain/lib/gcc/include")
	}
	reported := filepath.Join(repository, "lib", "gcc", "include")
	if err := run(reported, "result.json"); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult("result.json")
	if err != nil || result.Text != "external/cc-toolchain/lib/gcc/include" || result.Steps[0].Stdout != result.Text || result.Steps[0].StdoutPathKind != kconfig.ProbeStdoutPathToolset {
		t.Fatalf("rebased repository result = %#v, %v", result, err)
	}
	reportedPlugin := filepath.Join(repository, "lib", "gcc", "plugin")
	if err := run(reportedPlugin, "unbound-plugin.json"); err == nil || !strings.Contains(err.Error(), "outside the identity-bound") {
		t.Fatalf("unbound repository plugin path error = %v", err)
	}
	unrelated := filepath.Join(root, "unrelated", "include")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(unrelated, "unrelated.json"); err == nil || !strings.Contains(err.Error(), "unrelated to configured tool") {
		t.Fatalf("unrelated outside path error = %v", err)
	}
}

func TestRunProbeEvaluatesExecrootFileDependencyWithoutTool(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot")
	if err := os.MkdirAll(filepath.Join(execroot, "external", "plugin", "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execroot, "external", "plugin", "include", "plugin-version.h"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	identity := "sha256-" + strings.Repeat("8", 64)
	if err := os.WriteFile(identity, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	predecessorNodeID := strings.Repeat("7", 64)
	predecessor := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: predecessorNodeID, RequestID: strings.Repeat("6", 64),
		Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: "external/plugin",
	}
	predecessorData, err := predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("predecessor.json", predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{
			Operator: "execroot-regular-file", Value: "${result:00000000.text}/include/plugin-version.h",
		}},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, _ := request.CanonicalJSON()
	if err := os.WriteFile("request.json", requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{predecessorNodeID}}
	node.ID = node.ContentID()
	if err := runTestProbeWithToolset(t, probeOptions{
		request: "request.json", result: "result.json", nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": identity}, inputs: map[string]string{"00000000": "predecessor.json"},
	}, "external/plugin/include/plugin-version.h"); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult("result.json")
	if err != nil || result.Boolean == nil || !*result.Boolean || len(result.Steps) != 0 {
		t.Fatalf("dependent file result = %#v, %v", result, err)
	}

	predecessor.Text = "plugin"
	predecessorData, err = predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("predecessor.json", predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	fallbackRequest := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{
			Operator: "all", Operands: []kconfig.ProbePredicate{
				{Operator: "not", Operands: []kconfig.ProbePredicate{{
					Operator: "result-text-equals", Result: "00000000", Value: "plugin",
				}}},
				{Operator: "execroot-exists", Value: "${result:00000000.text}/include/plugin-version.h"},
			},
		}},
	}
	fallbackRequestID, err := fallbackRequest.ID()
	if err != nil {
		t.Fatal(err)
	}
	fallbackRequestData, _ := fallbackRequest.CanonicalJSON()
	if err := os.WriteFile("fallback-request.json", fallbackRequestData, 0o600); err != nil {
		t.Fatal(err)
	}
	fallbackNode := kconfig.ProbePlanNode{Scope: "target", RequestID: fallbackRequestID, Inputs: []string{predecessorNodeID}}
	fallbackNode.ID = fallbackNode.ContentID()
	runFallback := func(output string) error {
		return runTestProbeWithToolset(t, probeOptions{
			request: "fallback-request.json", result: output, nodeID: fallbackNode.ID, requestID: fallbackRequestID, scope: "target",
			toolsetMarkers: map[string]string{"target": identity}, inputs: map[string]string{"00000000": "predecessor.json"},
		}, "external/plugin/include/plugin-version.h")
	}
	if err := runFallback("fallback-result.json"); err != nil {
		t.Fatal(err)
	}
	fallbackResult, err := kconfig.ReadProbeResult("fallback-result.json")
	if err != nil || fallbackResult.Boolean == nil || *fallbackResult.Boolean {
		t.Fatalf("fallback file result = %#v, %v", fallbackResult, err)
	}
	predecessor.Text = "unbound"
	predecessorData, err = predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("predecessor.json", predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runFallback("unbound-result.json"); err == nil || !strings.Contains(err.Error(), "outside the identity-bound target toolset closure") {
		t.Fatalf("unbound nonfallback path error = %v", err)
	}
}

func TestRunProbeExecutesGenericRecipeAndWritesCanonicalResult(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Scratch: []kconfig.ProbeScratch{{Name: "object", Kind: "file"}},
		Steps: []kconfig.ProbeStep{{
			Name: "compile", Tool: "cc", Arguments: []string{"emit", "${scratch:object}"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "all", Operands: []kconfig.ProbePredicate{
			{Operator: "exit-zero", Step: "compile"}, {Operator: "regular-file", Scratch: "object"},
		}}},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	data, _ := request.CanonicalJSON()
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("a", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "results", node.ID+".json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"cc": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
			environment: map[string]string{},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Boolean == nil || !*result.Boolean || result.Steps[0].Stdout != "supported\nmore\n" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunProbeAnchorsRelativeArtifactsBeforeScratchWorkingDirectory(t *testing.T) {
	execroot := filepath.Join(t.TempDir(), "execroot", "nested")
	for _, relative := range []string{"plan", "identities", "inputs", "tools", "scratch/parent"} {
		if err := os.MkdirAll(filepath.Join(execroot, filepath.FromSlash(relative)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	identity := "sha256-" + strings.Repeat("f", 64)
	if err := os.WriteFile(filepath.Join(execroot, "identities", identity), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	predecessorNodeID := strings.Repeat("a", 64)
	value := true
	predecessor := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: predecessorNodeID, RequestID: strings.Repeat("b", 64),
		Scope: "target", ToolsetIdentity: identity, Kind: "boolean", Boolean: &value,
	}
	predecessorData, err := predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execroot, "inputs", "predecessor.json"), predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
		Scratch: []kconfig.ProbeScratch{{Name: "object", Kind: "file"}},
		Steps: []kconfig.ProbeStep{{
			Name: "compile", Tool: "cc", Arguments: []string{"emit", "${scratch:object}"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "all", Operands: []kconfig.ProbePredicate{
			{Operator: "exit-zero", Step: "compile"}, {Operator: "regular-file", Scratch: "object"},
		}}},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execroot, "plan", "request.json"), requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(execroot, "tools", "proberun-test")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(execroot)
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{predecessorNodeID}}
	node.ID = node.ContentID()
	resultPath := filepath.Join(execroot, "out", "result.json")
	err = runTestProbe(t, probeOptions{
		request: "plan/request.json", result: "out/result.json", nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": "identities/" + identity},
		inputs:         map[string]string{"00000000": "inputs/predecessor.json"},
		tempDir:        "scratch/parent",
		tools: map[string]actionContract{"cc": {
			path: "tools/proberun-test", arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Boolean == nil || !*result.Boolean || result.Steps[0].Stdout != "supported\nmore\n" {
		t.Fatalf("relative-artifact result = %#v", result)
	}
}

func TestRunProbeTreatsToolExitAsData(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "compile", Tool: "cc", Arguments: []string{"fail"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"}, DiscardStdout: true, DiscardStderr: true,
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "exit-zero", Step: "compile"}},
	}
	requestID, _ := request.ID()
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	_ = os.WriteFile(requestPath, data, 0o600)
	identity := "sha256-" + strings.Repeat("c", 64)
	marker := filepath.Join(dir, identity)
	_ = os.WriteFile(marker, nil, 0o600)
	executable, _ := os.Executable()
	resultPath := filepath.Join(dir, "result.json")
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools:          map[string]actionContract{"cc": {path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Boolean == nil || *result.Boolean || result.Steps[0].ExitCode != 7 || result.Steps[0].Status != "failure" ||
		result.Steps[0].Stdout != "" || result.Steps[0].Stderr != "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunProbeExecutesCompoundTryRunThroughScriptRunner(t *testing.T) {
	scriptRunner := resolveProbeTestRunfile(t, "LINUX_BZL_TEST_SCRIPTRUN")
	scriptRuntime := resolveProbeTestRunfile(t, "LINUX_BZL_TEST_SCRIPT_RUNTIME")
	tests := []struct {
		name     string
		scenario string
		value    bool
		status   string
	}{
		{name: "matching undefined symbols", scenario: "matching-undefined-symbols", value: true, status: "success"},
		{name: "second compile fails", scenario: "second-compile-fails", value: false, status: "failure"},
		{name: "undefined symbols change", scenario: "changed-undefined-symbols", value: false, status: "failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := executeCompoundTryRunProbe(t, scriptRunner, scriptRuntime, test.scenario, test.value, test.status)
			second := executeCompoundTryRunProbe(t, scriptRunner, scriptRuntime, test.scenario, test.value, test.status)
			if !bytes.Equal(first, second) {
				t.Fatalf("probe results differ across private scratch roots:\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

func executeCompoundTryRunProbe(t *testing.T, scriptRunner, scriptRuntime, scenario string, wantValue bool, wantStatus string) []byte {
	t.Helper()
	directory := t.TempDir()
	sourceRoot := filepath.Join(directory, "linux")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	kconfigPath := filepath.Join(sourceRoot, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte("# compound try-run source root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema:      kconfig.LinuxProbeRequestSchema,
		Sources:     []string{"Kconfig"},
		SourceRoots: []string{"linux"},
		Scratch:     []kconfig.ProbeScratch{{Name: "tmp", Kind: "file"}},
		Steps: []kconfig.ProbeStep{{
			Name:             "compound-try-run",
			Tool:             "scriptrun",
			AuxiliaryTools:   []string{"cc", "nm"},
			WorkingDirectory: "${source_root:linux}",
			DiscardStdout:    true,
			DiscardStderr:    true,
			Arguments: []string{
				"-interpreter", "${tool:script-runtime}",
				"-interpreter_arg", "sh",
				"-multicall", "${tool:script-runtime}",
				"-script_stdin",
				"-tool", "cc=${tool:cc}",
				"-tool", "nm=${tool:nm}",
				"-require_applet", "cmp",
				"-require_applet", "echo",
				"-require_applet", "grep",
				"-require_applet", "sh",
				"-require_applet", "true",
				"--", "${scratch:tmp}",
			},
			Stdin: `TMP="$1"; shift; ` +
				`echo 'long long x; void f(void){x++;}' | cc -w -fprofile-arcs -ftest-coverage -x c - -c -o "$TMP.base" && ` +
				`echo 'long long x; void f(void){x++;}' | cc -w -fprofile-arcs -ftest-coverage -fprofile-update=prefer-atomic -x c - -c -o "$TMP" && ` +
				`nm "$TMP.base" | grep ' U ' > "$TMP.ubase" || true ; ` +
				`nm "$TMP" | grep ' U ' > "$TMP.utest" || true ; ` +
				`cmp -s "$TMP.ubase" "$TMP.utest"`,
		}},
		Outcome: kconfig.ProbeOutcome{
			Kind: "boolean",
			Predicate: &kconfig.ProbePredicate{
				Operator: "exit-zero",
				Step:     "compound-try-run",
			},
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
	requestPath := filepath.Join(directory, "request.json")
	if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("e", 64)
	marker := filepath.Join(directory, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tempDirectory := filepath.Join(directory, "probe-temporary-parent")
	if err := os.Mkdir(tempDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helperContract := func(helper string) actionContract {
		return actionContract{
			path: executable,
			arguments: []string{
				"-test.run=^TestProbeHelperProcess$", "--", helper, kconfig.LinuxKbuildArgsSentinel,
			},
			environment: map[string]string{
				"COMPOUND_PROBE_SCENARIO": scenario,
				"LINUX_BZL_PROBE_HELPER":  "1",
			},
		}
	}
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(directory, "result.json")
	if err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers:    map[string]string{"target": marker},
		sources:           map[string]string{"Kconfig": kconfigPath},
		sourceRootAnchors: map[string]string{"linux": kconfigPath},
		tempDir:           tempDirectory,
		tools: map[string]actionContract{
			"cc":             helperContract("compound-cc"),
			"nm":             helperContract("compound-nm"),
			"script-runtime": {path: scriptRuntime, environment: map[string]string{}},
			"scriptrun": {
				path:        scriptRunner,
				arguments:   []string{kconfig.LinuxKbuildArgsSentinel},
				environment: map[string]string{},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Boolean == nil || *result.Boolean != wantValue || len(result.Steps) != 1 ||
		result.Steps[0].Status != wantStatus || result.Steps[0].Stdout != "" || result.Steps[0].Stderr != "" {
		t.Fatalf("compound probe result = %#v, want boolean %t, status %q, and discarded streams", result, wantValue, wantStatus)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func resolveProbeTestRunfile(t *testing.T, environment string) string {
	t.Helper()
	logicalPath := os.Getenv(environment)
	if logicalPath == "" {
		t.Fatalf("%s is empty", environment)
	}
	path, err := runfiles.Rlocation(logicalPath)
	if err != nil {
		t.Fatalf("resolve %s=%q: %v", environment, logicalPath, err)
	}
	return path
}

func TestRunProbeReducesTextToLastLine(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "preprocess", Tool: "cc", Arguments: []string{"multiline"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{
			Kind: "text", Step: "preprocess", Stream: "stdout", TrimSpace: true, LastLine: true,
		},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("d", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	if err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"cc": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "more" || result.Steps[0].Stdout != "supported\nmore\n" {
		t.Fatalf("last-line result = %#v, %v", result, err)
	}
}

func TestRunProbeRejectsUnsafePathComponentText(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "query", Tool: "cc", Arguments: []string{"emit-path", "../escape"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{
			Kind: "text", Step: "query", Stream: "stdout", TrimSpace: true, PathComponent: true,
		},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("e", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"cc": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "safe path component") {
		t.Fatalf("unsafe path component error = %v", err)
	}
	if _, statErr := os.Stat(resultPath); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe path component wrote result: %v", statErr)
	}
}

func TestRunProbeRejectsMultiWordGNUMakeShellText(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema,
		Steps: []kconfig.ProbeStep{{
			Name: "query", Tool: "cc", Arguments: []string{"multiline"},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{
			Kind: "text", Step: "query", Stream: "stdout", GNUMakeShell: true, SingleMakeWord: true,
		},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("f", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"cc": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", kconfig.LinuxKbuildArgsSentinel},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "one nonempty Make word") {
		t.Fatalf("multi-word Make text error = %v", err)
	}
	if _, statErr := os.Stat(resultPath); !os.IsNotExist(statErr) {
		t.Fatalf("multi-word Make text wrote result: %v", statErr)
	}
}

func TestSpliceActionArguments(t *testing.T) {
	got, err := spliceActionArguments([]string{"prefix", kconfig.LinuxKbuildArgsSentinel, "suffix"}, []string{"linux"})
	if err != nil || strings.Join(got, ",") != "prefix,linux,suffix" {
		t.Fatalf("splice = %q, %v", got, err)
	}
	if _, err := spliceActionArguments([]string{"missing"}, nil); err == nil {
		t.Fatal("missing sentinel accepted")
	}
}

func TestRunProbePreservesExactActionEnvelope(t *testing.T) {
	dir := t.TempDir()
	request := kconfig.ProbeRequest{
		Schema:  kconfig.LinuxProbeRequestSchema,
		Steps:   []kconfig.ProbeStep{{Name: "inspect", Tool: "cc", Arguments: []string{"linux"}}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "inspect", Stream: "stdout"},
	}
	requestID, _ := request.ID()
	data, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	_ = os.WriteFile(requestPath, data, 0o600)
	identity := "sha256-" + strings.Repeat("a", 64)
	marker := filepath.Join(dir, identity)
	_ = os.WriteFile(marker, nil, 0o600)
	executable, _ := os.Executable()
	resultPath := filepath.Join(dir, "result.json")
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
	node.ID = node.ContentID()
	err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker},
		tools: map[string]actionContract{"cc": {
			path:        executable,
			arguments:   []string{"-test.run=TestProbeHelperProcess", "--", "envelope", "prefix", kconfig.LinuxKbuildArgsSentinel, "suffix"},
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
}

func TestRunProbeFailsOnOutputCapAndTimeout(t *testing.T) {
	executable, _ := os.Executable()
	for _, test := range []struct {
		name, mode, want string
		timeout          time.Duration
		limit            int
	}{
		{name: "output cap", mode: "flood", want: "output exceeded", limit: 8},
		{name: "timeout", mode: "hang", want: "timed out", timeout: 10 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			request := kconfig.ProbeRequest{
				Schema:  kconfig.LinuxProbeRequestSchema,
				Steps:   []kconfig.ProbeStep{{Name: "step", Tool: "cc", Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"}}},
				Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "exit-zero", Step: "step"}},
			}
			requestID, _ := request.ID()
			data, _ := request.CanonicalJSON()
			requestPath := filepath.Join(dir, "request.json")
			_ = os.WriteFile(requestPath, data, 0o600)
			identity := "sha256-" + strings.Repeat("c", 64)
			marker := filepath.Join(dir, identity)
			_ = os.WriteFile(marker, nil, 0o600)
			node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID}
			node.ID = node.ContentID()
			err := runTestProbe(t, probeOptions{
				request: requestPath, result: filepath.Join(dir, "result.json"), nodeID: node.ID, requestID: requestID, scope: "target",
				toolsetMarkers: map[string]string{"target": marker}, timeout: test.timeout, outputLimit: test.limit,
				tools: map[string]actionContract{"cc": {
					path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", test.mode, kconfig.LinuxKbuildArgsSentinel},
				}},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runProbe() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunProbeConsumesDependencyResultDynamically(t *testing.T) {
	dir := t.TempDir()
	identity := "sha256-" + strings.Repeat("e", 64)
	marker := filepath.Join(dir, identity)
	_ = os.WriteFile(marker, nil, 0o600)
	predecessorRequestID := strings.Repeat("a", 64)
	predecessorNodeID := strings.Repeat("b", 64)
	value := true
	predecessor := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: predecessorNodeID, RequestID: predecessorRequestID,
		Scope: "target", ToolsetIdentity: identity, Kind: "boolean", Boolean: &value,
	}
	predecessorData, err := predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	predecessorPath := filepath.Join(dir, "predecessor.json")
	if err := os.WriteFile(predecessorPath, predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
		Steps: []kconfig.ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{"base"},
			ConditionalArguments: []kconfig.ProbeConditionalArguments{{
				Before: 0, When: kconfig.ProbePredicate{Operator: "result-true", Result: "00000000"},
				Arguments: []string{"selected=${result:00000000.boolean}"},
			}},
			Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
		}},
		Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "consume", Stream: "stdout"},
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	requestData, _ := request.CanonicalJSON()
	requestPath := filepath.Join(dir, "request.json")
	_ = os.WriteFile(requestPath, requestData, 0o600)
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{predecessorNodeID}}
	node.ID = node.ContentID()
	executable, _ := os.Executable()
	resultPath := filepath.Join(dir, "result.json")
	err = runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": predecessorPath},
		tools: map[string]actionContract{"cc": {
			path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", "dependency", kconfig.LinuxKbuildArgsSentinel},
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

func TestRunProbeRendersConditionalEnvironmentAndStdinFragments(t *testing.T) {
	for _, test := range []struct {
		name, environment, stdin, filtered string
		value                              bool
	}{
		{name: "true", value: true, environment: "prefix-yes-suffix", stdin: "stdin=yes\n", filtered: "-fkeep"},
		{name: "false", value: false, environment: "prefix-no-suffix", stdin: "stdin=no\n", filtered: "-fdrop-literal -fkeep"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			identity := "sha256-" + strings.Repeat("4", 64)
			marker := filepath.Join(dir, identity)
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			predecessorNodeID := strings.Repeat("5", 64)
			predecessor := kconfig.ProbeResult{
				Schema: kconfig.LinuxProbeResultSchema, NodeID: predecessorNodeID, RequestID: strings.Repeat("6", 64),
				Scope: "target", ToolsetIdentity: identity, Kind: "boolean", Boolean: &test.value,
			}
			predecessorData, err := predecessor.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			predecessorPath := filepath.Join(dir, "predecessor.json")
			if err := os.WriteFile(predecessorPath, predecessorData, 0o600); err != nil {
				t.Fatal(err)
			}
			truth := &kconfig.ProbePredicate{Operator: "result-true", Result: "00000000"}
			falsehood := &kconfig.ProbePredicate{Operator: "result-false", Result: "00000000"}
			request := kconfig.ProbeRequest{
				Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
				Steps: []kconfig.ProbeStep{{
					Name: "consume", Tool: "cc", Arguments: []string{test.environment, test.stdin, test.filtered},
					Environment: map[string]string{"LINUX_BZL_PROBE_HELPER": "1"},
					EnvironmentFragments: []kconfig.ProbeEnvironmentFragments{{
						Name: "FILTERED", Fragments: []kconfig.ProbeValueFragment{{
							Fragments: []kconfig.ProbeValueFragment{{Value: "-fdrop-literal -fkeep"}},
							Transforms: []kconfig.ProbeValueTransform{{
								Function: "filter-out", Arguments: []string{"", ""}, InputArgument: 1,
								ArgumentFragments: []kconfig.ProbeValueTransformArgumentFragments{{
									Index: 0, Fragments: []kconfig.ProbeValueFragment{
										{Value: "-fdrop%", When: truth}, {Value: "-fnone", When: falsehood},
									},
								}},
							}},
						}},
					}, {
						Name: "SELECTED", Fragments: []kconfig.ProbeValueFragment{
							{Value: "prefix-"},
							{
								Fragments: []kconfig.ProbeValueFragment{
									{Value: "  "}, {Value: "yes", When: truth}, {Value: "no", When: falsehood}, {Value: "  "},
								},
								Transforms: []kconfig.ProbeValueTransform{{Function: "strip", Arguments: []string{""}, InputArgument: 0}},
							},
							{Value: "-suffix"},
						},
					}},
					StdinFragments: []kconfig.ProbeValueFragment{
						{Value: "stdin="},
						{
							Fragments: []kconfig.ProbeValueFragment{
								{Value: "  "}, {Value: "yes", When: truth}, {Value: "no", When: falsehood}, {Value: "  "},
							},
							Transforms: []kconfig.ProbeValueTransform{{Function: "strip", Arguments: []string{""}, InputArgument: 0}},
						},
						{Value: "\n"},
					},
				}},
				Outcome: kconfig.ProbeOutcome{Kind: "text", Step: "consume", Stream: "stdout"},
			}
			requestID, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			requestData, _ := request.CanonicalJSON()
			requestPath := filepath.Join(dir, "request.json")
			if err := os.WriteFile(requestPath, requestData, 0o600); err != nil {
				t.Fatal(err)
			}
			node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{predecessorNodeID}}
			node.ID = node.ContentID()
			executable, _ := os.Executable()
			resultPath := filepath.Join(dir, "result.json")
			err = runTestProbe(t, probeOptions{
				request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
				toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": predecessorPath},
				tools: map[string]actionContract{"cc": {
					path: executable, arguments: []string{"-test.run=TestProbeHelperProcess", "--", "fragments", kconfig.LinuxKbuildArgsSentinel},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := kconfig.ReadProbeResult(resultPath)
			if err != nil || result.Text != "accepted" {
				t.Fatalf("result = %#v, %v", result, err)
			}
		})
	}
}

func TestRunProbeSelectsWhitespaceWordFromTextDependency(t *testing.T) {
	dir := t.TempDir()
	identity := "sha256-" + strings.Repeat("d", 64)
	marker := filepath.Join(dir, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	predecessorNodeID := strings.Repeat("1", 64)
	predecessor := kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: predecessorNodeID, RequestID: strings.Repeat("2", 64),
		Scope: "target", ToolsetIdentity: identity, Kind: "text", Text: "  Clang\t220108\ncustom  ",
	}
	predecessorData, err := predecessor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	predecessorPath := filepath.Join(dir, "predecessor.json")
	if err := os.WriteFile(predecessorPath, predecessorData, 0o600); err != nil {
		t.Fatal(err)
	}
	request := kconfig.ProbeRequest{
		Schema: kconfig.LinuxProbeRequestSchema, InputCount: 1,
		Outcome: kconfig.ProbeOutcome{Kind: "text", Result: "00000000", Word: 2},
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
	node := kconfig.ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: []string{predecessorNodeID}}
	node.ID = node.ContentID()
	resultPath := filepath.Join(dir, "result.json")
	if err := runTestProbe(t, probeOptions{
		request: requestPath, result: resultPath, nodeID: node.ID, requestID: requestID, scope: "target",
		toolsetMarkers: map[string]string{"target": marker}, inputs: map[string]string{"00000000": predecessorPath},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := kconfig.ReadProbeResult(resultPath)
	if err != nil || result.Text != "220108" || len(result.Steps) != 0 {
		t.Fatalf("word projection result = %#v, %v", result, err)
	}
}
