package kconfig

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestKbuildProbeScopesImportAuthenticatedKconfigToolsetPath(t *testing.T) {
	const canonicalPath = "external/compiler/vendor-sdk"
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	oracle := &ProbeResultOracle{
		results:  map[string]ProbeResult{},
		toolsets: map[string]string{"target": bootstrapTestIdentity},
	}
	wantCore, err := toolaction.EncodeExecutionRootProvenancePath("target", canonicalPath)
	if err != nil {
		t.Fatal(err)
	}

	var stable string
	for replay := 0; replay < 2; replay++ {
		upstream, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
		if err != nil {
			t.Fatal(err)
		}
		capability, err := upstream.EncodePath("target", canonicalPath)
		if err != nil {
			t.Fatal(err)
		}
		evaluation, err := EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (string, error) {
			imported, err := scopes.ImportToolsetPathCapabilities(capability, upstream.NormalizeValue)
			if err != nil {
				return "", err
			}
			parserOptions, err := scopes.Options("target", KbuildOptions{})
			if err != nil {
				return "", err
			}
			transformed, recognized, err := parserOptions.TransformSymbolic("addprefix", []string{"-I", imported})
			if err != nil {
				return "", err
			}
			if !recognized {
				return "", fmt.Errorf("imported Kconfig path was not retained as symbolic Make text")
			}
			resolved, err := parserOptions.ResolveSymbolic(transformed)
			if err != nil {
				return "", err
			}
			return scopes.evaluators["target"].NormalizeToolsetPathCapabilities(resolved)
		})
		if err != nil {
			t.Fatalf("replay %d: %v", replay, err)
		}
		if got, want := evaluation.Value, "-I"+wantCore; got != want {
			t.Fatalf("replay %d imported path = %q, want %q", replay, got, want)
		}
		if replay == 0 {
			stable = evaluation.Value
		} else if evaluation.Value != stable {
			t.Fatalf("typed Kconfig handoff depends on either workload key: first=%q second=%q", stable, evaluation.Value)
		}
	}

	upstream, err := toolaction.NewExecutionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := upstream.EncodePath("target", canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := capability[:len(capability)-1] + map[bool]string{true: "0", false: "1"}[capability[len(capability)-1] != '0']
	if _, err := EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (struct{}, error) {
		_, err := scopes.ImportToolsetPathCapabilities(mutated, upstream.NormalizeValue)
		return struct{}{}, err
	}); err == nil || !strings.Contains(err.Error(), "authenticate Kconfig toolset path") {
		t.Fatalf("mutated upstream capability error = %v, want authenticated handoff rejection", err)
	}
}

type kbuildProbeWorkloadFixture struct {
	TargetFlags string
	HostFlags   string
}

const sourceDefinedCCOptionFixture = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) $(3) -c -x c /dev/null -o "$$TMP",$(3),$(4))
cc-option = $(call __cc-option,$(CC),$(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS),$(1),$(2))
flags := $(call cc-option,%s,%s)
`

func testKbuildProbeScopeOptions(t *testing.T, fixture bootstrapFixture) KbuildProbeScopeOptions {
	t.Helper()
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{
		"cc":      "/configured/" + fixture.scope + "/cc",
		"ld":      "/configured/" + fixture.scope + "/ld",
		"ar":      "/configured/" + fixture.scope + "/ar",
		"nm":      "/configured/" + fixture.scope + "/nm",
		"objcopy": "/configured/" + fixture.scope + "/objcopy",
	}
	return KbuildProbeScopeOptions{
		Architecture: "x86", Facts: facts, Tools: tools,
	}
}

func writeKbuildProbeResults(t *testing.T, plan *ProbePlan, success map[string]bool) map[string]string {
	t.Helper()
	roots := map[string]string{}
	for _, scope := range []string{"target", "host"} {
		root := filepath.Join(t.TempDir(), scope)
		if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
			t.Fatal(err)
		}
		roots[scope] = root
	}
	for _, node := range plan.Nodes {
		value := success[node.Scope]
		status, exitCode := "failure", 1
		if value {
			status, exitCode = "success", 0
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "boolean", Boolean: &value,
			Steps: []ProbeStepResult{{Name: "probe", Status: status, ExitCode: exitCode}},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roots[node.Scope], "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return roots
}

func TestCanonicalCompilerPredefineArgumentsOnlyRewritesObjectMacroBodies(t *testing.T) {
	arguments := []string{
		"-target", "-DTHIS_IS_THE_TARGET_OPERAND",
		"-DNO_VALUE",
		"-DVALUE=old=tail",
		"-D", "SEPARATED=drivers/example/module",
		"-DFUNCTION(argument)=argument",
		"-D${DYNAMIC_NAME}=value",
		"-DSYMBOLIC_VALUE=${DYNAMIC_VALUE}",
		"-D=malformed",
		"-UJOINED", "-U", "SEPARATED_UNDEF",
		"-Xclang", "-DXCLANG_PAYLOAD=value",
		"-mllvm", "-DMLLVM_PAYLOAD=value",
		"-Xpreprocessor", "-DPREPROCESSOR_PAYLOAD=value",
		"--", "-DAFTER_DELIMITER=value",
	}
	original := slices.Clone(arguments)
	want := []string{
		"-target", "-DTHIS_IS_THE_TARGET_OPERAND",
		"-DNO_VALUE=1",
		"-DVALUE=1",
		"-D", "SEPARATED=1",
		"-DFUNCTION(argument)=argument",
		"-D${DYNAMIC_NAME}=value",
		"-DSYMBOLIC_VALUE=1",
		"-D=malformed",
		"-UJOINED", "-U", "SEPARATED_UNDEF",
		"-Xclang", "-DXCLANG_PAYLOAD=value",
		"-mllvm", "-DMLLVM_PAYLOAD=value",
		"-Xpreprocessor", "-DPREPROCESSOR_PAYLOAD=value",
		"--", "-DAFTER_DELIMITER=value",
	}
	got := canonicalCompilerPredefineArguments("cc", arguments)
	if !slices.Equal(got, want) {
		t.Fatalf("canonical compiler-predefine arguments = %#v, want %#v", got, want)
	}
	if !slices.Equal(arguments, original) {
		t.Fatalf("canonical compiler-predefine arguments mutated caller argv: got %#v, want %#v", arguments, original)
	}
}

func TestKbuildProbeScopesCompilerPredefinesDiscoversAndReplaysExactText(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type value struct {
		contents string
		ready    bool
	}
	arguments := []string{
		"--target=x86_64-linux-gnu", "-std=gnu11",
		"-DKBUILD_MODFILE=drivers/example/module",
		"source.c",
	}
	originalArguments := slices.Clone(arguments)
	workload := func(scopes *KbuildProbeScopes) (value, error) {
		contents, ready, err := scopes.CompilerPredefines(
			"target", "cc", "c", arguments, []string{"source.c"},
			map[string]string{"COMPILER_MODE": "exact"},
		)
		return value{contents: contents, ready: ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.ready || discovery.Value.contents != "" {
		t.Fatalf("compiler-predefine discovery value = %#v, want unresolved", discovery.Value)
	}
	if len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("compiler-predefine discovery nodes = %d, want 1", len(discovery.Plan.Nodes))
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	if request.Outcome.Kind != "text" || request.Outcome.Stream != "stdout" ||
		!request.Outcome.RequireSuccess || request.Outcome.TrimSpace {
		t.Fatalf("compiler-predefine outcome = %#v, want exact successful stdout", request.Outcome)
	}
	if len(request.Steps) != 1 || request.Steps[0].Tool != "cc" ||
		!slices.Equal(request.Steps[0].Arguments, []string{
			"--target=x86_64-linux-gnu", "-std=gnu11", "-DKBUILD_MODFILE=1",
			"source.c",
			"-dM", "-E", "-x", "c", "/dev/null",
		}) {
		t.Fatalf("compiler-predefine request steps = %#v", request.Steps)
	}
	if request.Steps[0].Candidate == nil ||
		request.Steps[0].Candidate.Projection != ProbeCandidateProjectionCompilerPredefines ||
		!slices.Equal(request.Steps[0].Candidate.TranslationUnits, []string{"source.c"}) {
		t.Fatalf("compiler-predefine candidate = %#v, want runtime compiler-predefine projection", request.Steps[0].Candidate)
	}
	if !slices.Equal(arguments, originalArguments) {
		t.Fatalf("compiler-predefine request mutated executable argv: got %#v, want %#v", arguments, originalArguments)
	}
	candidateArguments := []string{}
	for _, index := range request.Steps[0].Candidate.Base {
		candidateArguments = append(candidateArguments, request.Steps[0].Arguments[index])
	}
	projected, _, err := ProjectProbeCandidateArguments(
		request.Steps[0].Candidate.Projection,
		candidateArguments,
		request.Steps[0].Candidate.TranslationUnits,
	)
	if err != nil {
		t.Fatalf("project canonical compiler-predefine candidate arguments: %v", err)
	}
	if _, err := ValidateProbeCandidateArguments(request.Steps[0].Candidate.Policy, projected); err != nil {
		t.Fatalf("canonical KBUILD_MODFILE candidate arguments are unsafe: %v", err)
	}
	if got := request.Steps[0].Environment; !maps.Equal(got, map[string]string{"COMPILER_MODE": "exact"}) {
		t.Fatalf("compiler-predefine request environment = %#v", got)
	}

	const predefines = "#define DYNAMIC_COMPILER_MARKER 1\n"
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: discovery.Plan.Toolsets[node.Scope], Kind: "text", Text: predefines,
		Steps: []ProbeStepResult{{
			Name: "compiler-predefines", Status: "success", ExitCode: 0, Stdout: predefines,
		}},
	}
	data, err := result.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{"target": root}
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Value.ready || replay.Value.contents != predefines {
		t.Fatalf("compiler-predefine replay value = %#v, want exact text %q", replay.Value, predefines)
	}
	if len(replay.Plan.Nodes) != 1 || replay.Plan.Nodes[0].ID != node.ID {
		t.Fatalf("compiler-predefine replay plan = %#v, want discovery node %s", replay.Plan.Nodes, node.ID)
	}
}

func TestKbuildProbeScopesCompilerPredefineIdentityCanonicalizesObjectMacroValues(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	discover := func(arguments []string, environment map[string]string) string {
		t.Helper()
		evaluation, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
			_, ready, err := scopes.CompilerPredefines("target", "cc", "c", arguments, nil, environment)
			if ready {
				return struct{}{}, fmt.Errorf("compiler predefines unexpectedly ready during discovery")
			}
			return struct{}{}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(evaluation.Plan.Nodes) != 1 {
			t.Fatalf("compiler-predefine identity nodes = %d, want 1", len(evaluation.Plan.Nodes))
		}
		return evaluation.Plan.Nodes[0].ID
	}
	environment := map[string]string{"OBJECT_MODE": "32"}
	defined := discover([]string{"--target=x86_64-linux-gnu", "-DSELECTED=drivers/one"}, environment)
	otherValue := discover([]string{"--target=x86_64-linux-gnu", "-DSELECTED=drivers/two"}, environment)
	separated := discover([]string{"--target=x86_64-linux-gnu", "-D", "SELECTED=drivers/one"}, environment)
	separatedOtherValue := discover([]string{"--target=x86_64-linux-gnu", "-D", "SELECTED=drivers/two"}, environment)
	undefined := discover([]string{"--target=x86_64-linux-gnu", "-USELECTED"}, environment)
	otherName := discover([]string{"--target=x86_64-linux-gnu", "-DOTHER=drivers/one"}, environment)
	otherEnvironment := discover(
		[]string{"--target=x86_64-linux-gnu", "-DSELECTED=drivers/one"},
		map[string]string{"OBJECT_MODE": "64"},
	)
	otherNonMacro := discover([]string{"--target=aarch64-linux-gnu", "-DSELECTED=drivers/one"}, environment)
	functionOne := discover([]string{"--target=x86_64-linux-gnu", "-DFUNCTION(x)=one"}, environment)
	functionTwo := discover([]string{"--target=x86_64-linux-gnu", "-DFUNCTION(x)=two"}, environment)
	defineThenUndef := discover(
		[]string{"--target=x86_64-linux-gnu", "-DSELECTED=drivers/one", "-UOTHER"}, environment,
	)
	undefThenDefine := discover(
		[]string{"--target=x86_64-linux-gnu", "-UOTHER", "-DSELECTED=drivers/one"}, environment,
	)

	if defined != otherValue {
		t.Fatalf("object-like -D replacement changed identity: one=%s two=%s", defined, otherValue)
	}
	if separated != separatedOtherValue {
		t.Fatalf("separated object-like -D replacement changed identity: one=%s two=%s", separated, separatedOtherValue)
	}
	for label, identity := range map[string]string{
		"joined/separated shape":            separated,
		"D/U operation":                     undefined,
		"macro name":                        otherName,
		"defined-name-relevant environment": otherEnvironment,
		"nonmacro option":                   otherNonMacro,
	} {
		if defined == identity {
			t.Fatalf("compiler-predefine identity omitted %s: both are %s", label, defined)
		}
	}
	if functionOne == functionTwo {
		t.Fatalf("function-like -D bodies were unexpectedly canonicalized: both are %s", functionOne)
	}
	if defineThenUndef == undefThenDefine {
		t.Fatalf("D/U operation order was omitted from identity: both are %s", defineThenUndef)
	}
}

func TestKbuildProbeScopesCompilerPredefinesLowersSymbolicEnvironmentForReplay(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type value struct {
		contents string
		ready    bool
	}
	workload := func(scopes *KbuildProbeScopes) (value, error) {
		evaluator := scopes.evaluators["target"]
		mode, err := evaluator.requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema,
			Steps:  []ProbeStep{{Name: "environment-source", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{
				Kind: "text", Step: "environment-source", Stream: "stdout", RequireSuccess: true,
			},
		})
		if err != nil {
			return value{}, err
		}
		contents, ready, err := scopes.CompilerPredefines(
			"target", "cc", "c", []string{"-DSELECTED=1"}, nil, map[string]string{"MODE": mode},
		)
		return value{contents: contents, ready: ready}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.ready || len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("symbolic-environment discovery = value %#v nodes %d", discovery.Value, len(discovery.Plan.Nodes))
	}
	compilerNode := ProbePlanNode{}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if len(request.Steps) != 0 && request.Steps[0].Name == "compiler-predefines" {
			compilerNode = node
			step := request.Steps[0]
			if request.InputCount != 1 || len(step.Environment) != 0 ||
				len(step.EnvironmentFragments) != 1 || step.EnvironmentFragments[0].Name != "MODE" {
				t.Fatalf("symbolic compiler environment request = %#v", request)
			}
		}
	}
	if compilerNode.ID == "" {
		t.Fatal("compiler-predefine request missing from symbolic-environment plan")
	}

	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		textValue := "dynamic-mode\n"
		if node.ID == compilerNode.ID {
			textValue = "#define SELECTED 1\n"
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: discovery.Plan.Toolsets[node.Scope], Kind: "text", Text: textValue,
			Steps: []ProbeStepResult{{
				Name: request.Steps[0].Name, Status: "success", ExitCode: 0, Stdout: textValue,
			}},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oracle, err := NewProbeResultOracleFromTrees(map[string]string{"target": root}, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(options, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Value.ready || replay.Value.contents != "#define SELECTED 1\n" ||
		len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
		t.Fatalf("symbolic-environment replay = %#v plan nodes %d", replay.Value, len(replay.Plan.Nodes))
	}
}

func TestKbuildProbeRequestsCanonicalizeLexicalAndPhysicalSourceRoots(t *testing.T) {
	physicalRoot := filepath.Join(t.TempDir(), "repository-cache", "linux")
	if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lexicalRoot := filepath.Join(t.TempDir(), "external-linux")
	if err := os.Symlink(physicalRoot, lexicalRoot); err != nil {
		t.Fatal(err)
	}
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	discover := func(sourceRoot string) *ProbePlan {
		opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
		opts.Target.SourceRoot = sourceRoot
		evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (string, error) {
			options, optionsErr := scopes.Options("target", KbuildOptions{
				Variables: map[string]string{
					"SRCARCH": "x86", "CC": opts.Target.Tools["cc"],
					"KBUILD_CPPFLAGS": "-fmacro-prefix-map=" + filepath.ToSlash(physicalRoot) + "/=",
				},
				MakeVariablesComplete: true, CaptureVariables: []string{"flags"},
			})
			if optionsErr != nil {
				return "", optionsErr
			}
			parsed, parseErr := parseKbuildWithOptions(
				strings.NewReader(fmt.Sprintf(sourceDefinedCCOptionFixture, "-fexample", "")),
				"Makefile", options, "",
			)
			if parseErr != nil {
				return "", parseErr
			}
			return parsed.Variables["flags"], nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(evaluation.Plan.Nodes); got != 1 {
			t.Fatalf("source-root discovery nodes = %d, want 1", got)
		}
		request := evaluation.Plan.Requests[evaluation.Plan.Nodes[0].RequestID]
		data, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), filepath.ToSlash(physicalRoot)) || strings.Contains(string(data), filepath.ToSlash(lexicalRoot)) {
			t.Fatalf("probe request retained executor-specific source root: %s", data)
		}
		if !strings.Contains(string(data), "__LINUX_BZL_SOURCE_TREE__/") {
			t.Fatalf("probe request has no stable source-tree marker: %s", data)
		}
		return evaluation.Plan
	}
	lexicalPlan := discover(lexicalRoot)
	physicalPlan := discover(physicalRoot)
	if got, want := physicalPlan.Nodes[0].ID, lexicalPlan.Nodes[0].ID; got != want {
		t.Fatalf("physical source-root node = %s, lexical source-root node = %s", got, want)
	}
}

func TestEvaluateKbuildProbeWorkloadSharesExactTargetAndHostPlan(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	opts := KbuildProbeWorkloadOptions{
		Target: testKbuildProbeScopeOptions(t, fixtures[1]),
		Host:   ptrKbuildProbeScopeOptions(testKbuildProbeScopeOptions(t, fixtures[0])),
	}
	workload := func(scopes *KbuildProbeScopes) (kbuildProbeWorkloadFixture, error) {
		targetOptions, err := scopes.Options("target", KbuildOptions{
			Variables:             map[string]string{"SRCARCH": "x86", "CC": opts.Target.Tools["cc"]},
			MakeVariablesComplete: true, CaptureVariables: []string{"flags"},
		})
		if err != nil {
			return kbuildProbeWorkloadFixture{}, err
		}
		hostOptions, err := scopes.Options("host", KbuildOptions{
			Variables:             map[string]string{"SRCARCH": "x86", "CC": opts.Host.Tools["cc"]},
			MakeVariablesComplete: true, CaptureVariables: []string{"flags"},
		})
		if err != nil {
			return kbuildProbeWorkloadFixture{}, err
		}
		targetSource := fmt.Sprintf(sourceDefinedCCOptionFixture, "-ftarget", "-ftarget-fallback")
		target, err := parseKbuildWithOptions(strings.NewReader(targetSource), "target/Makefile", targetOptions, "")
		if err != nil {
			return kbuildProbeWorkloadFixture{}, err
		}
		hostSource := fmt.Sprintf(sourceDefinedCCOptionFixture, "-fhost", "-fhost-fallback")
		host, err := parseKbuildWithOptions(strings.NewReader(hostSource), "host/Makefile", hostOptions, "")
		if err != nil {
			return kbuildProbeWorkloadFixture{}, err
		}
		return kbuildProbeWorkloadFixture{TargetFlags: target.Variables["flags"], HostFlags: host.Variables["flags"]}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 || len(discovery.Plan.Terminal) != 2 {
		t.Fatalf("discovery plan=%#v, want one target and one host terminal", discovery.Plan)
	}
	if discovery.Plan.Nodes[0].Scope != "target" || discovery.Plan.Nodes[1].Scope != "host" {
		t.Fatalf("discovery scopes=%#v", discovery.Plan.Nodes)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.TargetFlags) || !linuxProbeSymbolPattern.MatchString(discovery.Value.HostFlags) {
		t.Fatalf("discovery resolved compiler results early: %#v", discovery.Value)
	}

	roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": true, "host": false})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.TargetFlags != "-ftarget" || replay.Value.HostFlags != "-fhost-fallback" {
		t.Fatalf("replay workload=%#v", replay.Value)
	}
	if !slices.Equal(discovery.Plan.Terminal, replay.Plan.Terminal) || len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
		t.Fatalf("replay plan differs: %#v vs %#v", replay.Plan, discovery.Plan)
	}
	for index := range discovery.Plan.Nodes {
		if discovery.Plan.Nodes[index].ID != replay.Plan.Nodes[index].ID || discovery.Plan.Nodes[index].RequestID != replay.Plan.Nodes[index].RequestID {
			t.Fatalf("replay node %d differs: %#v vs %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
		}
	}
}

func TestKbuildCompilerMacroStatusUsesExactScopedBootstrapPredefines(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	opts := KbuildProbeWorkloadOptions{
		Target: testKbuildProbeScopeOptions(t, fixtures[1]),
		Host:   ptrKbuildProbeScopeOptions(testKbuildProbeScopeOptions(t, fixtures[0])),
	}
	type result struct {
		TargetStatus, TargetVisited string
		HostStatus, HostVisited     string
	}
	parse := func(scopes *KbuildProbeScopes, scope, compiler string) (string, string, error) {
		options, err := scopes.Options(scope, KbuildOptions{
			Variables:               map[string]string{"CC": compiler},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"CC_NO_COMPILER", "visited"},
		})
		if err != nil {
			return "", "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(`
CC_NO_COMPILER := $(shell $(CC) -dM -E -x c /dev/null | grep -Fq "VENDOR_MAJOR 22"; echo $$?)
ifeq ($(CC_NO_COMPILER), 1)
visited += without-marker
else
visited += with-marker
endif
`), scope+"/tools/scripts/Makefile.include", options, "")
		if err != nil {
			return "", "", err
		}
		return parsed.Variables["CC_NO_COMPILER"], parsed.Variables["visited"], nil
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		targetStatus, targetVisited, err := parse(scopes, "target", opts.Target.Tools["cc"])
		if err != nil {
			return result{}, err
		}
		hostStatus, hostVisited, err := parse(scopes, "host", opts.Host.Tools["cc"])
		return result{
			TargetStatus: targetStatus, TargetVisited: targetVisited,
			HostStatus: hostStatus, HostVisited: hostVisited,
		}, err
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	discovery.Value.TargetVisited = strings.TrimSpace(discovery.Value.TargetVisited)
	discovery.Value.HostVisited = strings.TrimSpace(discovery.Value.HostVisited)
	want := result{
		TargetStatus: "0", TargetVisited: "with-marker",
		HostStatus: "1", HostVisited: "without-marker",
	}
	if discovery.Value != want {
		t.Fatalf("scoped bootstrap macro result = %#v, want %#v", discovery.Value, want)
	}
	if len(discovery.Plan.Nodes) != 0 || len(discovery.Plan.Terminal) != 0 {
		t.Fatalf("bare compiler macro query emitted redundant nodes: %#v", discovery.Plan)
	}

	replay, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	replay.Value.TargetVisited = strings.TrimSpace(replay.Value.TargetVisited)
	replay.Value.HostVisited = strings.TrimSpace(replay.Value.HostVisited)
	if replay.Value != want {
		t.Fatalf("repeat compiler macro evaluation = %#v, want %#v", replay.Value, want)
	}
	if len(replay.Plan.Nodes) != 0 || !slices.Equal(replay.Plan.Terminal, discovery.Plan.Terminal) {
		t.Fatalf("compiler macro replay plan = %#v, want discovery %#v", replay.Plan, discovery.Plan)
	}
}

func TestKbuildSymbolicElseIfConcreteTrueMakesFollowingElseInactive(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	const makefile = `
CC_NO_SELECTED := $(shell $(CC) -D__LINUX_BZL_DYNAMIC_QUERY__=1 -dM -E -x c /dev/null | grep -Fq "__selected_compiler__"; echo $$?)
ifeq ($(CC_NO_SELECTED), 1)
flags += without-marker
else ifneq ($(CROSS_COMPILE),)
flags += cross-compiler
else
flags += native-compiler
endif
`
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC": opts.Target.Tools["cc"], "CROSS_COMPILE": "aarch64-linux-gnu-",
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"flags"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), "tools/scripts/Makefile.include", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["flags"], nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 || strings.Contains(discovery.Value, "native-compiler") {
		t.Fatalf("symbolic else-if discovery = %q, plan %#v; following else must be inactive", discovery.Value, discovery.Plan.Nodes)
	}

	for _, test := range []struct {
		name    string
		matched bool
		want    string
	}{
		{name: "selected marker", matched: true, want: "cross-compiler"},
		{name: "missing marker", matched: false, want: "without-marker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "target")
			if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
				t.Fatal(err)
			}
			node := discovery.Plan.Nodes[0]
			stdout := "#define __other_compiler__ 1\n"
			if test.matched {
				stdout += "#define __selected_compiler__ 1\n"
			}
			result := ProbeResult{
				Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
				Scope: "target", ToolsetIdentity: discovery.Plan.Toolsets["target"], Kind: "boolean", Boolean: &test.matched,
				Steps: []ProbeStepResult{{Name: "macro-preprocess", Status: "success", ExitCode: 0, Stdout: stdout}},
			}
			data, err := result.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			oracle, err := NewProbeResultOracleFromTrees(
				map[string]string{"target": root},
				map[string]string{"target": discovery.Plan.Toolsets["target"]},
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(replay.Value); got != test.want {
				t.Fatalf("symbolic else-if replay = %q, want %q", got, test.want)
			}
			if len(replay.Plan.Nodes) != 1 || replay.Plan.Nodes[0].ID != node.ID {
				t.Fatalf("symbolic else-if replay plan = %#v, want %#v", replay.Plan.Nodes, discovery.Plan.Nodes)
			}
		})
	}
}

func TestKbuildCompoundSymbolicComparisonsProveInvariantAndRetainOneDependency(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Empty, Nonempty, Selected string
	}
	const makefile = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
indirect := $(call cc-option,-mindirect-branch-cs-prefix)
RETPOLINE_CFLAGS := -mretpoline-external-thunk $(indirect) -mfunction-return=thunk-extern
ifeq ($(RETPOLINE_CFLAGS),)
empty := selected
else
nonempty := selected
endif
ifeq (prefix$(indirect),prefix-mindirect-branch-cs-prefix)
selected += supported
else
selected += unsupported
endif
`
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"empty", "nonempty", "selected"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), "arch/x86/Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Empty: parsed.Variables["empty"], Nonempty: parsed.Variables["nonempty"],
			Selected: parsed.Variables["selected"],
		}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Empty != "" || discovery.Value.Nonempty != "selected" {
		t.Fatalf("compound nonempty discovery = %#v, want statically selected nonempty branch", discovery.Value)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Selected) {
		t.Fatalf("one-result compound discovery = %q, want retained symbolic selection", discovery.Value.Selected)
	}
	if got := len(discovery.Plan.Nodes); got != 1 {
		t.Fatalf("compound comparison plan = %#v, want one source compiler capability", discovery.Plan.Nodes)
	}

	for _, test := range []struct {
		name, selected string
		supported      bool
	}{
		{name: "supported", supported: true, selected: "supported"},
		{name: "unsupported", supported: false, selected: "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": test.supported})
			oracle, err := NewProbeResultOracleFromTrees(
				map[string]string{"target": roots["target"]},
				discovery.Plan.Toolsets,
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			replay.Value.Selected = strings.TrimSpace(replay.Value.Selected)
			want := result{Nonempty: "selected", Selected: test.selected}
			if replay.Value != want {
				t.Fatalf("compound comparison replay = %#v, want %#v", replay.Value, want)
			}
			if len(replay.Plan.Nodes) != 1 || replay.Plan.Nodes[0].ID != discovery.Plan.Nodes[0].ID ||
				replay.Plan.Nodes[0].RequestID != discovery.Plan.Nodes[0].RequestID {
				t.Fatalf("compound comparison replay plan = %#v, want %#v", replay.Plan.Nodes, discovery.Plan.Nodes)
			}
		})
	}
}

func TestKbuildCompoundSymbolicComparisonRetainsMultipleDependencies(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (string, error) {
		options, optionsErr := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"selected"},
		})
		if optionsErr != nil {
			return "", optionsErr
		}
		parsed, parseErr := parseKbuildWithOptions(strings.NewReader(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
first := $(call cc-option,-ffirst)
second := $(call cc-option,-fsecond)
ifeq ($(first)$(second),-ffirst-fsecond)
selected += both
else
selected += not-both
endif
`), "arch/generic/Makefile", options, "")
		if parseErr != nil {
			return "", parseErr
		}
		return parsed.Variables["selected"], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 2 || !linuxProbeSymbolPattern.MatchString(evaluation.Value) {
		t.Fatalf("multi-result compound comparison = %q, plan %#v; want two inputs and one retained selection", evaluation.Value, evaluation.Plan.Nodes)
	}
}

func TestKbuildProbeDependentFlagsRemainOneExactReplayDAG(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	const makefile = `
pound := \#
CC_NO_SELECTED := $(shell $(CC) -D__LINUX_BZL_DYNAMIC_QUERY__=1 -dM -E -x c /dev/null | grep -Fq "__selected_compiler__"; echo $$?)
EXTRA_WARNINGS := -Wbase
ifeq ($(CC_NO_SELECTED), 1)
EXTRA_WARNINGS += -Wstrict-source-branch
else
IGNORED_EMPTY :=
endif
OBJTOOL_CFLAGS := -Werror $(EXTRA_WARNINGS)
elfshdr := $(shell echo '$(pound)include <libelf.h>' | $(CC) $(OBJTOOL_CFLAGS) -x c -E - 2>/dev/null | grep elf_getshdr)
OBJTOOL_CFLAGS += $(if $(elfshdr),,-DLIBELF_USE_DEPRECATED)
`
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"OBJTOOL_CFLAGS"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), "tools/objtool/Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["OBJTOOL_CFLAGS"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("probe-dependent flag plan = %#v, want macro query and dependent header query", discovery.Plan.Nodes)
	}
	macroNode, headerNode := discovery.Plan.Nodes[0], discovery.Plan.Nodes[1]
	if !slices.Equal(headerNode.Inputs, []string{macroNode.ID}) {
		t.Fatalf("header query inputs = %q, want macro query %s", headerNode.Inputs, macroNode.ID)
	}
	headerRequest := discovery.Plan.Requests[headerNode.RequestID]
	if headerRequest.InputCount != 1 || len(headerRequest.Steps) != 1 {
		t.Fatalf("header request = %#v, want one exact conditional input", headerRequest)
	}
	step := headerRequest.Steps[0]
	if slices.Contains(step.Arguments, "-Wstrict-source-branch") {
		t.Fatalf("header query flattened source branch into unconditional argv: %#v", step)
	}
	foundStrictBranch := false
	for _, conditional := range step.ConditionalArguments {
		if conditional.When.Operator == "result-false" && slices.Equal(conditional.Arguments, []string{"-Wstrict-source-branch"}) {
			foundStrictBranch = true
		}
	}
	if !foundStrictBranch {
		t.Fatalf("header query omitted source-selected compiler branch: %#v", step.ConditionalArguments)
	}

	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	macroMatched, headerMatched := true, false
	results := []ProbeResult{
		{
			Schema: LinuxProbeResultSchema, NodeID: macroNode.ID, RequestID: macroNode.RequestID,
			Scope: "target", ToolsetIdentity: discovery.Plan.Toolsets["target"], Kind: "boolean", Boolean: &macroMatched,
			Steps: []ProbeStepResult{{Name: "macro-preprocess", Status: "success", ExitCode: 0, Stdout: "#define __selected_compiler__ 1\n"}},
		},
		{
			Schema: LinuxProbeResultSchema, NodeID: headerNode.ID, RequestID: headerNode.RequestID,
			Scope: "target", ToolsetIdentity: discovery.Plan.Toolsets["target"], Kind: "boolean", Boolean: &headerMatched,
			Steps: []ProbeStepResult{{Name: "preprocess", Status: "success", ExitCode: 0, Stdout: "typedef int unrelated;\n"}},
		},
	}
	for _, result := range results {
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "results", result.NodeID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"target": root},
		map[string]string{"target": discovery.Plan.Toolsets["target"]},
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(replay.Value), []string{"-Werror", "-Wbase", "-DLIBELF_USE_DEPRECATED"}; !slices.Equal(got, want) {
		t.Fatalf("replayed source-selected flags = %q, want %q", got, want)
	}
	if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) || !slices.Equal(replay.Plan.Terminal, discovery.Plan.Terminal) {
		t.Fatalf("replay plan = %#v, want discovery %#v", replay.Plan, discovery.Plan)
	}
	for index := range discovery.Plan.Nodes {
		got, want := replay.Plan.Nodes[index], discovery.Plan.Nodes[index]
		if got.ID != want.ID || got.Scope != want.Scope || got.RequestID != want.RequestID || !slices.Equal(got.Inputs, want.Inputs) {
			t.Fatalf("replay node %d = %#v, want %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
		}
	}
}

func TestKbuildPreprocessorTailConditionalIsUnknownUntilExactReplay(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Width, Visited string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"WIDTH", "visited"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
CFLAGS := $(call cc-option,-m64)
WIDTH := $(shell echo __SOURCE_SELECTED_WIDTH__ | $(CC) $(CFLAGS) -E -x c - | tail -n 1)
ifeq ($(WIDTH), 1)
visited += expanded
else
visited += unexpanded
endif
`), "tools/scripts/Makefile.arch", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Width: parsed.Variables["WIDTH"], Visited: parsed.Variables["visited"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Width) ||
		len(linuxProbeSymbolPattern.FindAllString(discovery.Value.Visited, -1)) != 2 {
		t.Fatalf("preprocessor tail discovery = %#v, want unresolved width and two conditional branches", discovery.Value)
	}
	if len(discovery.Plan.Nodes) != 3 ||
		!slices.Equal(discovery.Plan.Nodes[1].Inputs, []string{discovery.Plan.Nodes[0].ID}) ||
		!slices.Equal(discovery.Plan.Nodes[2].Inputs, []string{discovery.Plan.Nodes[1].ID}) {
		t.Fatalf("preprocessor tail discovery plan = %#v, want flag-dependent text request and exact comparison", discovery.Plan.Nodes)
	}

	for _, test := range []struct {
		name, output, visited string
	}{
		{name: "expanded token", output: "1", visited: "expanded"},
		{name: "unexpanded token", output: "__SOURCE_SELECTED_WIDTH__", visited: "unexpanded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "target")
			if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
				t.Fatal(err)
			}
			flagSupported := true
			for index, node := range discovery.Plan.Nodes {
				probeResult := ProbeResult{
					Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
					Scope: "target", ToolsetIdentity: discovery.Plan.Toolsets["target"],
				}
				if index == 0 {
					probeResult.Kind = "boolean"
					probeResult.Boolean = &flagSupported
					probeResult.Steps = []ProbeStepResult{{Name: "probe", Status: "success", ExitCode: 0}}
				} else if index == 1 {
					probeResult.Kind = "text"
					probeResult.Text = test.output
					probeResult.Steps = []ProbeStepResult{{
						Name: "preprocess", Status: "success", ExitCode: 0,
						Stdout: "# 0 \"<stdin>\"\n# 0 \"<built-in>\"\n" + test.output + "\n",
					}}
				} else {
					matched := test.output == "1"
					probeResult.Kind = "boolean"
					probeResult.Boolean = &matched
				}
				data, err := probeResult.CanonicalJSON()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			oracle, err := NewProbeResultOracleFromTrees(
				map[string]string{"target": root},
				map[string]string{"target": discovery.Plan.Toolsets["target"]},
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			replay.Value.Visited = strings.TrimSpace(replay.Value.Visited)
			want := result{Width: test.output, Visited: test.visited}
			if replay.Value != want {
				t.Fatalf("preprocessor tail replay = %#v, want %#v", replay.Value, want)
			}
			if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) || !slices.Equal(replay.Plan.Terminal, discovery.Plan.Terminal) {
				t.Fatalf("preprocessor tail replay plan = %#v, want %#v", replay.Plan, discovery.Plan)
			}
			for index := range discovery.Plan.Nodes {
				if replay.Plan.Nodes[index].ID != discovery.Plan.Nodes[index].ID || replay.Plan.Nodes[index].RequestID != discovery.Plan.Nodes[index].RequestID {
					t.Fatalf("preprocessor tail replay node %d = %#v, want %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
				}
			}
		})
	}
}

func TestEvaluateKbuildProbeWorkloadPreservesExportedProbeDependencies(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Exported string
		Child    string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		base := KbuildOptions{
			Variables:             map[string]string{"CC": opts.Target.Tools["cc"]},
			MakeVariablesComplete: true,
		}
		rootOptions, err := scopes.Options("target", base)
		if err != nil {
			return result{}, err
		}
		root, err := parseKbuildWithOptions(strings.NewReader(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) $(3) -c -x c /dev/null -o "$$TMP",$(3),$(4))
cc-option = $(call __cc-option,$(CC),$(KBUILD_CFLAGS),$(1),$(2))
KBUILD_CFLAGS := $(call cc-option,-ffirst)
export KBUILD_CFLAGS
`), "root/Makefile", rootOptions, "")
		if err != nil {
			return result{}, err
		}
		exported := root.exportedVariables["KBUILD_CFLAGS"]
		childOptions, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC": opts.Target.Tools["cc"], "KBUILD_CFLAGS": exported,
			},
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"flags"},
		})
		if err != nil {
			return result{}, err
		}
		child, err := parseKbuildWithOptions(strings.NewReader(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) $(3) -c -x c /dev/null -o "$$TMP",$(3),$(4))
cc-option = $(call __cc-option,$(CC),$(KBUILD_CFLAGS),$(1),$(2))
flags := $(call cc-option,-fsecond)
`), "child/Makefile", childOptions, "")
		if err != nil {
			return result{}, err
		}
		return result{Exported: exported, Child: child.Variables["flags"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 || len(discovery.Plan.Nodes[1].Inputs) != 1 || discovery.Plan.Nodes[1].Inputs[0] != discovery.Plan.Nodes[0].ID {
		t.Fatalf("discovery value = %#v, dependency chain = %#v, requests = %#v, want child probe depending on exported root probe", discovery.Value, discovery.Plan.Nodes, discovery.Plan.Requests)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Exported) || !linuxProbeSymbolPattern.MatchString(discovery.Value.Child) {
		t.Fatalf("discovery resolved exported dependency early: %#v", discovery.Value)
	}

	roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": true})
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"target": roots["target"]},
		map[string]string{"target": discovery.Plan.Toolsets["target"]},
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(replay.Value.Exported) || replay.Value.Child != "-fsecond" {
		t.Fatalf("replay exported dependency = %#v, want symbolic export and concrete terminal", replay.Value)
	}
	if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
		t.Fatalf("replay plan = %#v, want discovery %#v", replay.Plan, discovery.Plan)
	}
	for index := range discovery.Plan.Nodes {
		if discovery.Plan.Nodes[index].ID != replay.Plan.Nodes[index].ID || discovery.Plan.Nodes[index].RequestID != replay.Plan.Nodes[index].RequestID {
			t.Fatalf("replay node %d differs: %#v vs %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
		}
	}
}

func TestKbuildProbeEnvironmentRefreshUsesFreshMemosAndRetainsReferences(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/rustc"
	target.ScriptEnvironment = map[string]string{
		"CC": "/configured/target/cc", "ROLE_FREE": "initial", "RUSTC": "/configured/rustc",
		"RUSTC_BOOTSTRAP": "initial",
	}
	opts := KbuildProbeWorkloadOptions{Target: target}
	command := `{ trap "rm -rf .tmp_2" EXIT; mkdir .tmp_2; /configured/rustc -Zbrand-new --crate-type=rlib /dev/null --out-dir=.tmp_2 -o .tmp_2/tmp.rlib; } >/dev/null 2>&1 && echo "y" || echo "n"`
	type result struct{ Before, After string }
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		beforeOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return result{}, err
		}
		before, err := beforeOptions.Shell(command)
		if err != nil {
			return result{}, err
		}
		if err := scopes.RefreshScriptEnvironments(map[string]map[string]string{
			"target": {"ROLE_FREE": "source-selected", "RUSTC_BOOTSTRAP": "source-selected"},
		}); err != nil {
			return result{}, err
		}
		afterOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return result{}, err
		}
		after, err := afterOptions.Shell(command)
		return result{Before: before, After: after}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(discovery.Plan.Nodes), 2; got != want {
		t.Fatalf("environment refresh nodes = %#v, want %d retained generations", discovery.Plan.Nodes, want)
	}
	environments := map[string]string{}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if len(request.Steps) != 1 || request.Steps[0].Tool != "rustc" {
			t.Fatalf("environment refresh request = %#v, want rustc probe", request)
		}
		step := request.Steps[0]
		environments[step.Environment["RUSTC_BOOTSTRAP"]] = step.Environment["ROLE_FREE"]
	}
	if got, want := environments, map[string]string{"initial": "initial", "source-selected": "source-selected"}; !maps.Equal(got, want) {
		t.Fatalf("environment generations = %#v, want %#v", got, want)
	}
	if discovery.Value.Before == discovery.Value.After {
		t.Fatal("environment refresh reused the pre-export shell memo")
	}
	roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": true})
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"target": roots["target"]},
		map[string]string{"target": discovery.Plan.Toolsets["target"]},
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(discovery.Plan.Terminal, replay.Plan.Terminal) || len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
		t.Fatalf("environment refresh replay plan = %#v, want %#v", replay.Plan, discovery.Plan)
	}
}

func TestKbuildProbeEnvironmentRefreshReplacesPriorExportSnapshot(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.ScriptEnvironment = map[string]string{"BASE": "configured"}
	_, err := EvaluateKbuildProbeWorkload(
		KbuildProbeWorkloadOptions{Target: target},
		nil,
		func(scopes *KbuildProbeScopes) (struct{}, error) {
			if err := scopes.RefreshScriptEnvironments(map[string]map[string]string{
				"target": {"FIRST_ONLY": "first"},
			}); err != nil {
				return struct{}{}, err
			}
			if err := scopes.RefreshScriptEnvironments(map[string]map[string]string{
				"target": {"SECOND_ONLY": "second"},
			}); err != nil {
				return struct{}{}, err
			}
			environment := scopes.evaluators["target"].scriptEnvironment
			if got := environment["BASE"]; got != "configured" {
				t.Fatalf("refreshed environment base = %q, want configured", got)
			}
			if got := environment["SECOND_ONLY"]; got != "second" {
				t.Fatalf("refreshed environment second snapshot = %q, want second", got)
			}
			if _, leaked := environment["FIRST_ONLY"]; leaked {
				t.Fatalf("prior export snapshot leaked into replacement: %#v", environment)
			}
			return struct{}{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestKbuildProbeExactEnvironmentBindingsReplaceInternAndRetainReferences(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/rustc"
	target.ScriptEnvironment = map[string]string{
		"DROP_ME": "configured", "RUSTC": "/configured/rustc", "RUSTC_BOOTSTRAP": "configured",
	}
	command := `{ trap "rm -rf .tmp_2" EXIT; mkdir .tmp_2; /configured/rustc -Zbrand-new --crate-type=rlib /dev/null --out-dir=.tmp_2 -o .tmp_2/tmp.rlib; } >/dev/null 2>&1 && echo "y" || echo "n"`
	evaluation, err := EvaluateKbuildProbeWorkload(
		KbuildProbeWorkloadOptions{Target: target},
		nil,
		func(scopes *KbuildProbeScopes) (struct{}, error) {
			firstEnvironment := map[string]map[string]string{"target": {
				"RUSTC_BOOTSTRAP": "first", "RUSTC": "/configured/rustc",
			}}
			activateFirst, err := scopes.BindExactScriptEnvironments(firstEnvironment)
			if err != nil {
				return struct{}{}, err
			}
			var firstBinding map[string]map[string]string
			for _, binding := range scopes.exactScriptEnvironmentBindings {
				firstBinding = binding
			}
			firstEnvironment["target"]["RUSTC_BOOTSTRAP"] = "caller-mutated"
			firstEnvironment["target"]["CALLER_ONLY"] = "caller-only"
			if got := firstBinding["target"]["RUSTC_BOOTSTRAP"]; got != "first" {
				t.Fatalf("interned RUSTC_BOOTSTRAP after caller mutation = %q, want first", got)
			}
			if _, leaked := firstBinding["target"]["CALLER_ONLY"]; leaked {
				t.Fatalf("caller mutation leaked into exact environment binding: %#v", firstBinding)
			}
			activateFirstAgain, err := scopes.BindExactScriptEnvironments(map[string]map[string]string{"target": {
				"RUSTC": "/configured/rustc", "RUSTC_BOOTSTRAP": "first",
			}})
			if err != nil {
				return struct{}{}, err
			}
			if got := len(scopes.exactScriptEnvironmentBindings); got != 1 {
				t.Fatalf("interned exact environments = %d, want 1", got)
			}
			if err := activateFirst(); err != nil {
				return struct{}{}, err
			}
			firstEvaluator := scopes.evaluators["target"]
			if err := activateFirstAgain(); err != nil {
				return struct{}{}, err
			}
			if scopes.evaluators["target"] != firstEvaluator {
				t.Fatal("repeated exact environment activation replaced an unchanged evaluator")
			}
			if _, leaked := firstEvaluator.scriptEnvironment["DROP_ME"]; leaked {
				t.Fatalf("configured-only variable survived exact replacement: %#v", firstEvaluator.scriptEnvironment)
			}
			if got, want := firstEvaluator.scriptEnvironment["ARCH"], target.Architecture; got != want {
				t.Fatalf("exact environment ARCH = %q, want %q", got, want)
			}
			options, err := scopes.Options("target", KbuildOptions{})
			if err != nil {
				return struct{}{}, err
			}
			if _, err := options.Shell(command); err != nil {
				return struct{}{}, err
			}

			activateSecond, err := scopes.BindExactScriptEnvironments(map[string]map[string]string{"target": {
				"RUSTC": "/configured/rustc", "RUSTC_BOOTSTRAP": "second",
			}})
			if err != nil {
				return struct{}{}, err
			}
			if got := len(scopes.exactScriptEnvironmentBindings); got != 2 {
				t.Fatalf("distinct exact environments = %d bindings, want 2", got)
			}
			if err := activateSecond(); err != nil {
				return struct{}{}, err
			}
			if _, err := options.Shell(command); err != nil {
				return struct{}{}, err
			}
			if err := activateFirst(); err != nil {
				return struct{}{}, err
			}
			if _, err := options.Shell(command); err != nil {
				return struct{}{}, err
			}
			if got := len(scopes.References()); got != 2 {
				t.Fatalf("references after first/second/first activation = %d, want 2", got)
			}
			return struct{}{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evaluation.Plan.Nodes); got != 2 {
		t.Fatalf("exact environment plan nodes = %#v, want 2", evaluation.Plan.Nodes)
	}
	boots := map[string]bool{}
	for _, node := range evaluation.Plan.Nodes {
		request := evaluation.Plan.Requests[node.RequestID]
		if len(request.Steps) != 1 || request.Steps[0].Tool != "rustc" {
			t.Fatalf("exact environment request = %#v, want rustc probe", request)
		}
		environment := request.Steps[0].Environment
		if _, leaked := environment["DROP_ME"]; leaked {
			t.Fatalf("configured-only variable leaked into exact request: %#v", environment)
		}
		boots[environment["RUSTC_BOOTSTRAP"]] = true
	}
	if want := map[string]bool{"first": true, "second": true}; !maps.Equal(boots, want) {
		t.Fatalf("exact request environments = %#v, want %#v", boots, want)
	}
}

func TestEvaluateKbuildProbeWorkloadRoutesScopedTokensWithSharedExecutables(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	opts := KbuildProbeWorkloadOptions{
		Target: testKbuildProbeScopeOptions(t, fixtures[1]),
		Host:   ptrKbuildProbeScopeOptions(testKbuildProbeScopeOptions(t, fixtures[0])),
	}
	// Native builds may use the exact same executable artifacts for both
	// contracts. Source-carried scope, never path equality, must route probes.
	for _, role := range []string{"cc", "ld"} {
		opts.Host.Tools[role] = opts.Target.Tools[role]
	}
	type result struct {
		Target string
		HostCC string
		HostLD string
		Stamp  string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		parseOptions, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC":     KbuildActionRoleToken("target", "cc"),
				"HOSTCC": KbuildActionRoleToken("host", "cc"),
				"HOSTLD": KbuildActionRoleToken("host", "ld"),
			},
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"target_flags", "hostcc_flags", "hostld_flags", "stamp"},
			Shell: func(command string) (string, error) {
				if command != "LC_ALL=C date" {
					return "", fmt.Errorf("unexpected noncompiler shell command %q", command)
				}
				return "normalized-date", nil
			},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__compiler-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __compiler-option,$(CC),$(1),$(2))
hostcc-option = $(call __compiler-option,$(HOSTCC),$(1),$(2))
hostld-option = $(call try-run,$(HOSTLD) $(1) -v,$(1),$(2))
target_flags := $(call cc-option,-ftarget,-ftarget-fallback)
hostcc_flags := $(call hostcc-option,-fhost,-fhost-fallback)
hostld_flags := $(call hostld-option,--host-linker,--host-linker-fallback)
stamp := $(shell LC_ALL=C date)
`), "mixed/Makefile", parseOptions, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Target: parsed.Variables["target_flags"],
			HostCC: parsed.Variables["hostcc_flags"],
			HostLD: parsed.Variables["hostld_flags"],
			Stamp:  parsed.Variables["stamp"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Stamp != "normalized-date" {
		t.Fatalf("noncompiler shell fallback = %q", discovery.Value.Stamp)
	}
	if len(discovery.Plan.Nodes) != 3 {
		t.Fatalf("mixed discovery nodes = %#v, want target cc, host cc, and host ld", discovery.Plan.Nodes)
	}
	wantScopes := []string{"target", "host", "host"}
	for index, want := range wantScopes {
		if got := discovery.Plan.Nodes[index].Scope; got != want {
			t.Fatalf("mixed node %d scope = %q, want %q", index, got, want)
		}
	}
	roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": true, "host": false})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := result{
		Target: "-ftarget", HostCC: "-fhost-fallback",
		HostLD: "--host-linker-fallback", Stamp: "normalized-date",
	}
	if replay.Value != want {
		t.Fatalf("mixed replay = %#v, want %#v", replay.Value, want)
	}
	if !slices.Equal(discovery.Plan.Terminal, replay.Plan.Terminal) {
		t.Fatalf("mixed replay terminals = %v, want %v", replay.Plan.Terminal, discovery.Plan.Terminal)
	}
}

func TestKbuildProbeScopeRoutingRejectsMixedTokensAndIgnoresToolPaths(t *testing.T) {
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{
		"target": {scope: "target", tools: map[string]string{"awk": "/target/awk", "cc": "/shared/compiler"}},
		"host":   {scope: "host", tools: map[string]string{"awk": "/host/awk", "cc": "/shared/compiler"}},
	}}
	selected, err := scopes.firstConfiguredToolScopes("/shared/compiler -v", "target")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 {
		t.Fatalf("executable path inferred scopes %q, want none", selected)
	}
	_, err = scopes.firstConfiguredToolScopes(
		"TOOLS="+KbuildActionRoleToken("target", "cc")+":"+KbuildActionRoleToken("host", "cc"),
		"target",
	)
	if err == nil || !strings.Contains(err.Error(), "mixes configured target and host") {
		t.Fatalf("mixed scoped-token error=%v", err)
	}
	for _, preferred := range []string{"target", "host"} {
		selected, err = scopes.firstConfiguredToolScopes(KbuildActionRoleToken(KbuildActionRoleAutoScope, "awk")+" -f generate.awk", preferred)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(selected, []string{preferred}) {
			t.Fatalf("scope-neutral AWK with preferred %s selected %q", preferred, selected)
		}
	}
	selected, err = scopes.firstConfiguredToolScopes(
		"TOOLS="+KbuildActionRoleToken(KbuildActionRoleAutoScope, "awk")+":"+KbuildActionRoleToken("host", "cc"),
		"target",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected, []string{"host"}) {
		t.Fatalf("scope-neutral utility did not follow explicit host provenance: %q", selected)
	}
}

func ptrKbuildProbeScopeOptions(value KbuildProbeScopeOptions) *KbuildProbeScopeOptions {
	return &value
}

func TestEvaluateKbuildProbeWorkloadRejectsExistingResolverOrMissingScope(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	_, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		_, err := scopes.Options("target", KbuildOptions{ResolveSymbolic: func(string) (string, error) { return "", nil }})
		return struct{}{}, err
	})
	if err == nil || !strings.Contains(err.Error(), "already contain a symbolic resolver") {
		t.Fatalf("existing resolver error=%v", err)
	}
	_, err = EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		_, err := scopes.Options("host", KbuildOptions{})
		return struct{}{}, err
	})
	if err == nil || !strings.Contains(err.Error(), "no host scope") {
		t.Fatalf("missing host scope error=%v", err)
	}
}

func TestKbuildSymbolicStripIsIdentityForCanonicalOpaqueMakeText(t *testing.T) {
	for _, test := range []struct {
		name, input, resolved string
		emptiness             kbuildSymbolicEmptiness
	}{
		{name: "nonempty", input: "rust/z.o rust/a.o", resolved: "rust/a.o rust/z.o", emptiness: kbuildSymbolicEmptinessNonempty},
		{name: "empty", input: "", resolved: "", emptiness: kbuildSymbolicEmptinessEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluator := &LinuxProbeEvaluator{
				scope: "target", symbols: map[string]linuxProbeSymbol{},
				symbolRegistry: newLinuxProbeSymbolRegistry(),
			}
			scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
			sorted, err := evaluator.renderMakeText(
				"sort", []string{test.input}, "", linuxProbeMakeTextProtocolUnusable,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := scopes.symbolicMakeTextEmptiness(sorted); err != nil || got != test.emptiness {
				t.Fatalf("opaque sort emptiness = %v, err=%v, want %v", got, err, test.emptiness)
			}
			filtered, recognized, err := scopes.transformSymbolic(
				"filter-out", []string{"", " " + sorted},
			)
			if err != nil || !recognized {
				t.Fatalf("empty-pattern filter-out = %q, recognized=%v, err=%v", filtered, recognized, err)
			}
			stripped, recognized, err := scopes.transformSymbolic("strip", []string{"  " + filtered + "  "})
			if err != nil || !recognized || stripped != filtered {
				t.Fatalf("strip of canonical opaque value = %q, recognized=%v, err=%v, want identity %q", stripped, recognized, err, filtered)
			}
			evaluator.oracle = &ProbeResultOracle{}
			resolved, err := evaluator.ResolveSymbolic(stripped)
			if err != nil || resolved != test.resolved {
				t.Fatalf("canonical opaque replay = %q, err=%v, want %q", resolved, err, test.resolved)
			}
		})
	}
}

func TestKbuildMakeTextProtocolChainsFromUsableRootWithoutTransforms(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	raw, err := evaluator.requestText(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "measure", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "measure", Stream: "stdout",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := evaluator.renderMakeText(
		"sort", []string{raw}, raw, linuxProbeMakeTextProtocolCanonicalWords,
	)
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	stripped, recognized, err := scopes.transformSymbolic("strip", []string{" " + root + " "})
	if err != nil || !recognized {
		t.Fatalf("strip zero-transform protocol root = %q, recognized=%v, err=%v", stripped, recognized, err)
	}
	_, symbol, ok := scopes.symbolOwner(stripped)
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil {
		t.Fatalf("stripped protocol root symbol = %#v", symbol)
	}
	if symbol.makeText.protocolMode != linuxProbeMakeTextProtocolExact ||
		len(symbol.makeText.protocolTransforms) != 1 ||
		symbol.makeText.protocolTransforms[0].function != "strip" ||
		symbol.makeText.protocolValue != raw {
		t.Fatalf("stripped zero-transform protocol = %#v", symbol.makeText)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(stripped)
	if err != nil || !dynamic || len(fragments) == 0 || len(lowerer.dependencies) != 1 {
		t.Fatalf("stripped zero-transform lowering = %#v, dependencies=%#v, dynamic=%v, err=%v", fragments, lowerer.dependencies, dynamic, err)
	}
}

func TestKbuildDynamicStripRequiresExactAggregateProtocol(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	raw, err := evaluator.requestText(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "measure", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "measure", Stream: "stdout",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}

	exactChild, err := evaluator.renderMakeTextWithProtocolTransforms(
		"filter-out",
		[]string{"drop", raw},
		raw,
		[]linuxProbeMakeTextProtocolTransform{{
			function: "filter-out", arguments: []string{"drop", ""}, inputArgument: 1,
		}},
		linuxProbeMakeTextProtocolExact,
	)
	if err != nil {
		t.Fatal(err)
	}
	stripped, recognized, err := scopes.transformSymbolic("strip", []string{"prefix  " + exactChild + "  suffix"})
	if err != nil || !recognized {
		t.Fatalf("aggregate exact strip = %q, recognized=%v, err=%v", stripped, recognized, err)
	}
	_, stripSymbol, ok := scopes.symbolOwner(stripped)
	if !ok || stripSymbol.kind != "make-text" || stripSymbol.makeText == nil ||
		stripSymbol.makeText.protocolMode != linuxProbeMakeTextProtocolExact ||
		len(stripSymbol.makeText.protocolTransforms) != 1 ||
		stripSymbol.makeText.protocolTransforms[0].function != "strip" {
		t.Fatalf("aggregate exact strip protocol = %#v", stripSymbol)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(stripped)
	if err != nil || !dynamic || len(fragments) == 0 || len(lowerer.dependencies) != 1 {
		t.Fatalf("aggregate exact strip lowering = %#v, dependencies=%#v, dynamic=%v, err=%v", fragments, lowerer.dependencies, dynamic, err)
	}
	got, err := RenderProbeDependencyFragments(fragments, map[string]ProbeResult{
		"00000000": dependencyTextResult("drop\tkeep  "),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prefix keep suffix" {
		t.Fatalf("aggregate exact strip = %q, want %q", got, "prefix keep suffix")
	}

	canonicalChild, err := evaluator.renderMakeText(
		"sort", []string{raw}, raw, linuxProbeMakeTextProtocolCanonicalWords,
	)
	if err != nil {
		t.Fatal(err)
	}
	canonicalAggregate, recognized, err := scopes.transformSymbolic("strip", []string{"prefix  " + canonicalChild + "  suffix"})
	if err != nil || !recognized {
		t.Fatalf("aggregate canonical strip = %q, recognized=%v, err=%v", canonicalAggregate, recognized, err)
	}
	canonicalLowerer := newProbeSymbolicValueLowerer(evaluator)
	canonicalFragments, dynamic, err := canonicalLowerer.value(canonicalAggregate)
	if err != nil || !dynamic || len(canonicalFragments) == 0 || len(canonicalLowerer.dependencies) != 1 {
		t.Fatalf("aggregate canonical strip lowering = %#v, dependencies=%#v, dynamic=%v, err=%v", canonicalFragments, canonicalLowerer.dependencies, dynamic, err)
	}
	got, err = RenderProbeDependencyFragments(canonicalFragments, map[string]ProbeResult{
		"00000000": dependencyTextResult("drop\tkeep  "),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prefix drop keep suffix" {
		t.Fatalf("aggregate canonical strip = %q, want %q", got, "prefix drop keep suffix")
	}

	unsafe, recognized, err := scopes.transformSymbolic("strip", []string{"prefix" + canonicalChild})
	if err != nil || !recognized {
		t.Fatalf("embedded canonical strip = %q, recognized=%v, err=%v", unsafe, recognized, err)
	}
	_, unsafeSymbol, ok := scopes.symbolOwner(unsafe)
	if !ok || unsafeSymbol.kind != "make-text" || unsafeSymbol.makeText == nil ||
		unsafeSymbol.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable ||
		len(unsafeSymbol.makeText.protocolTransforms) != 0 {
		t.Fatalf("embedded canonical strip incorrectly claimed a runtime protocol: %#v", unsafeSymbol)
	}
}

func TestKbuildRustObjDirsMaterializesCompleteWordTransformChain(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	dynamicName, err := evaluator.requestText(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "rustc", Tool: "rustc",
			Arguments: []string{"--print", "file-names", "--crate-name", "macros", "--crate-type", "proc-macro", "-"},
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: "rustc", Stream: "stdout", FirstLine: true,
			PathComponent: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	targets := "rust/core.o rust/" + dynamicName + " rust/ FORCE"
	value, err := evaluator.renderMakeTextWithProtocolTransforms(
		"filter-out",
		[]string{"rust/ FORCE", targets},
		targets,
		[]linuxProbeMakeTextProtocolTransform{{
			function: "filter-out", arguments: []string{"rust/ FORCE", ""}, inputArgument: 1,
		}},
		linuxProbeMakeTextProtocolExact,
	)
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	for _, transform := range []struct {
		function string
		args     func(string) []string
	}{
		{function: "dir", args: func(input string) []string { return []string{input} }},
		{function: "patsubst", args: func(input string) []string { return []string{"%/", "%", " " + input} }},
		{function: "sort", args: func(input string) []string { return []string{input} }},
		{function: "filter-out", args: func(input string) []string { return []string{"", " " + input} }},
		{function: "strip", args: func(input string) []string { return []string{"  " + input + "  "} }},
	} {
		var recognized bool
		var transformErr error
		value, recognized, transformErr = scopes.transformSymbolic(transform.function, transform.args(value))
		if transformErr != nil || !recognized {
			t.Fatalf("%s transform = %q, recognized=%v, err=%v", transform.function, value, recognized, transformErr)
		}
	}
	_, makeTextSymbol, ok := scopes.symbolOwner(value)
	if !ok || makeTextSymbol.kind != "make-text" || makeTextSymbol.makeText == nil {
		t.Fatalf("Rust obj-dirs value = %q, symbol = %#v", value, makeTextSymbol)
	}
	makeText := makeTextSymbol.makeText
	if makeText.protocolMode != linuxProbeMakeTextProtocolExact {
		t.Fatalf("Rust obj-dirs protocol mode = %q, want exact", makeText.protocolMode)
	}
	functions := make([]string, len(makeText.protocolTransforms))
	for index, transform := range makeText.protocolTransforms {
		functions[index] = transform.function
	}
	if want := []string{"filter-out", "dir", "patsubst", "sort", "filter-out", "strip"}; !slices.Equal(functions, want) {
		t.Fatalf("Rust obj-dirs protocol functions = %q, want %q", functions, want)
	}

	selected, recognized, err := scopes.selectSymbolic(value, "", false, "nonempty", "empty")
	if err != nil || !recognized || !linuxProbeSymbolPattern.MatchString(selected) {
		t.Fatalf("Rust obj-dirs ifneq = %q, recognized=%v, err=%v", selected, recognized, err)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 ||
		!slices.Equal(plan.Nodes[1].Inputs, []string{plan.Nodes[0].ID}) ||
		!slices.Equal(plan.Nodes[2].Inputs, []string{plan.Nodes[1].ID}) {
		t.Fatalf("Rust obj-dirs materialization plan = %#v, want three-node dependency chain", plan.Nodes)
	}
}

func TestKbuildProbeConsumerScopeRejectsTargetGraphAfterTargetResolution(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}

	workload := func(scopes *KbuildProbeScopes) (string, error) {
		targetOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return "", err
		}
		hostOptions, err := scopes.Options("host", KbuildOptions{})
		if err != nil {
			return "", err
		}
		command := fmt.Sprintf(`set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (%s -Werror -ftarget-only -c -x c /dev/null -o "$TMP") >/dev/null 2>&1; then echo "-ftarget-only"; else echo ""; fi`, opts.Target.Tools["cc"])
		flag, err := targetOptions.Shell(command)
		if err != nil {
			return "", err
		}
		if !linuxProbeSymbolPattern.MatchString(flag) {
			return "", fmt.Errorf("target probe returned non-symbolic value %q", flag)
		}
		targetOnly, recognized, err := targetOptions.TransformSymbolic("strip", []string{"cc " + flag})
		if err != nil || !recognized {
			return "", fmt.Errorf("build target-only whole Make text from flag %q: recognized=%v: %v", flag, recognized, err)
		}

		// Populate the target consumer's cache first. The host callback must use
		// a distinct consumer-policy key and revalidate the complete nested graph.
		resolved, err := targetOptions.ResolveSymbolic(targetOnly)
		if err != nil {
			return "", err
		}
		if _, err := hostOptions.ResolveSymbolic(targetOnly); err == nil || !strings.Contains(err.Error(), "cannot adopt target-scoped") {
			return "", fmt.Errorf("host resolver accepted target graph after target cache fill: %v", err)
		}
		if _, _, err := hostOptions.SelectSymbolic(targetOnly, "", false, "nonempty", "empty"); err == nil || !strings.Contains(err.Error(), "cannot adopt target-scoped") {
			return "", fmt.Errorf("host selector accepted target graph: %v", err)
		}
		if _, _, err := hostOptions.TransformSymbolic("strip", []string{targetOnly}); err == nil || !strings.Contains(err.Error(), "cannot adopt target-scoped") {
			return "", fmt.Errorf("host transformer accepted target graph: %v", err)
		}
		return resolved, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 || !linuxProbeSymbolPattern.MatchString(discovery.Value) {
		t.Fatalf("target-only discovery = %q, plan = %#v", discovery.Value, discovery.Plan.Nodes)
	}
	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(_ int, _ ProbePlanNode) bool { return true })
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value != "cc -ftarget-only" {
		t.Fatalf("target-only replay = %q, want %q", replay.Value, "cc -ftarget-only")
	}
}

func TestKbuildSymbolicWordLoweringBoundsProtocolAmplification(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	first, err := evaluator.renderMakeText(
		"strip", []string{"first"}, strings.Repeat("x", 600<<10), linuxProbeMakeTextProtocolArgvWords,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := evaluator.renderMakeText(
		"strip", []string{"second"}, strings.Repeat("y", 600<<10), linuxProbeMakeTextProtocolArgvWords,
	)
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	if _, err := scopes.resolveSymbolicWordsForScope("target", first+second); err == nil || !strings.Contains(err.Error(), "word value exceeds") {
		t.Fatalf("amplified symbolic word lowering error = %v", err)
	}
}

func TestKbuildMakeTextEmptinessMemoizesSharedSelectionDAG(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{scope: "target", symbols: map[string]linuxProbeSymbol{}}
	value := "nonempty"
	for depth := 0; depth < 10; depth++ {
		token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", depth+1)
		evaluator.symbols[token] = linuxProbeSymbol{
			kind:            "selection",
			selectionValues: slices.Repeat([]string{value}, 256),
		}
		value = token
	}
	wrapper, err := evaluator.renderMakeText("strip", []string{value}, "", linuxProbeMakeTextProtocolUnusable)
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	got, err := scopes.symbolicMakeTextEmptiness(wrapper)
	if err != nil || got != kbuildSymbolicEmptinessNonempty {
		t.Fatalf("shared selection DAG emptiness = %v, %v; want nonempty", got, err)
	}
}

func TestKbuildNestedSelectionBranchesSurviveSubstAndArgvLowering(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{
		scope: "target", symbols: map[string]linuxProbeSymbol{},
		symbolRegistry: newLinuxProbeSymbolRegistry(),
	}
	innerReference := ProbeReference{NodeID: "inner-node", RequestID: "inner-request", Scope: "target", Kind: "boolean"}
	inner, err := evaluator.renderTruth(linuxProbeTruth{
		reference: innerReference, trueWhenResult: true,
	}, "-finner", "")
	if err != nil {
		t.Fatal(err)
	}
	outerReference := ProbeReference{NodeID: "outer-node", RequestID: "outer-request", Scope: "target", Kind: "boolean"}
	outer, err := evaluator.renderSelection(
		[]linuxProbeSelectionInput{{reference: outerReference}},
		[]string{"", inner},
	)
	if err != nil {
		t.Fatal(err)
	}

	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	escaped, recognized, err := scopes.transformSymbolic("subst", []string{"$", "$$", outer})
	if err != nil || !recognized {
		t.Fatalf("nested selection subst = %q, recognized=%v, %v", escaped, recognized, err)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	base, conditional, fragments, err := lowerer.arguments([]string{escaped})
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 0 || len(fragments) != 0 ||
		!slices.Equal(lowerer.dependencies, []ProbeReference{outerReference, innerReference}) {
		t.Fatalf("nested selection argv lowering = base %q, fragments %#v, dependencies %#v", base, fragments, lowerer.dependencies)
	}
	if len(conditional) != 1 || !slices.Equal(conditional[0].Arguments, []string{"-finner"}) {
		t.Fatalf("nested selection conditional argv = %#v, want one -finner state", conditional)
	}
}

func TestKbuildSingleFiniteTokenSupportsPureMakeComposition(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{
		scope: "target", symbols: map[string]linuxProbeSymbol{},
		symbolRegistry: newLinuxProbeSymbolRegistry(),
	}
	reference := ProbeReference{NodeID: "flag-node", RequestID: "flag-request", Scope: "target", Kind: "boolean"}
	flag, err := evaluator.renderTruth(linuxProbeTruth{
		reference: reference, trueWhenResult: true,
	}, "-fb -fa", "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	sorted, recognized, err := scopes.transformSymbolic("sort", []string{flag})
	if err != nil || !recognized {
		t.Fatalf("single finite sort = %q, recognized=%v, %v", sorted, recognized, err)
	}
	_, symbol, ok := scopes.symbolOwner(sorted)
	if !ok || symbol.kind != "selection" ||
		!slices.Equal(symbol.selectionValues, []string{"", "-fa -fb"}) {
		t.Fatalf("single finite sort symbol = %#v", symbol)
	}
}

func TestKbuildFiniteOuterChoiceWithNestedTextUsesExactMakeAST(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{
		scope: "target", symbols: map[string]linuxProbeSymbol{},
		symbolRegistry: newLinuxProbeSymbolRegistry(),
	}
	textReference := ProbeReference{NodeID: "text-node", RequestID: "text-request", Scope: "target", Kind: "text"}
	textToken := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	if err := evaluator.publishSymbol(textToken, linuxProbeSymbol{kind: "text", reference: textReference}); err != nil {
		t.Fatal(err)
	}
	choiceReference := ProbeReference{NodeID: "choice-node", RequestID: "choice-request", Scope: "target", Kind: "boolean"}
	choice, err := evaluator.renderTruth(linuxProbeTruth{
		reference: choiceReference, trueWhenResult: true,
	}, textToken, "")
	if err != nil {
		t.Fatal(err)
	}

	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	sorted, recognized, err := scopes.transformSymbolic("sort", []string{choice})
	if err != nil || !recognized {
		t.Fatalf("mixed finite/text sort = %q, recognized=%v, %v", sorted, recognized, err)
	}
	_, symbol, ok := scopes.symbolOwner(sorted)
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil ||
		symbol.makeText.function != "sort" || symbol.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable ||
		!slices.Equal(symbol.makeText.arguments, []string{choice}) {
		t.Fatalf("mixed finite/text exact Make AST = %#v", symbol)
	}
}

func TestKbuildSharedSelectionDAGProtocolAndSubstAreBounded(t *testing.T) {
	sharedDAG := func(depth int) (*LinuxProbeEvaluator, string) {
		evaluator := &LinuxProbeEvaluator{scope: "target", symbols: map[string]linuxProbeSymbol{}}
		value := "x"
		for level := 0; level < depth; level++ {
			token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", level+1)
			inputs := make([]linuxProbeSelectionInput, maxKbuildSymbolicComparisonReferences)
			for index := range inputs {
				inputs[index].reference = ProbeReference{
					NodeID:    fmt.Sprintf("node-%02d-%02d", level, index),
					RequestID: fmt.Sprintf("request-%02d-%02d", level, index),
					Scope:     "target", Kind: "boolean",
				}
			}
			evaluator.symbols[token] = linuxProbeSymbol{
				kind: "selection", selectionInputs: inputs,
				selectionValues: slices.Repeat([]string{value}, 1<<len(inputs)),
			}
			value = token
		}
		return evaluator, value
	}

	t.Run("shared graph", func(t *testing.T) {
		evaluator, value := sharedDAG(10)
		scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
		mode, err := scopes.makeTextInputProtocolMode(value)
		if err != nil || mode != linuxProbeMakeTextProtocolExact {
			t.Fatalf("shared protocol proof = %q, %v; want exact", mode, err)
		}
		transformed, recognized, err := scopes.transformSymbolic("subst", []string{"x", "y", value})
		if err != nil || !recognized || transformed != "y" {
			t.Fatalf("shared subst = %q, recognized=%v, %v; want y", transformed, recognized, err)
		}
	})

	t.Run("global work bound", func(t *testing.T) {
		evaluator, value := sharedDAG(17)
		scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
		if _, err := scopes.makeTextInputProtocolMode(value); err == nil || !strings.Contains(err.Error(), "work items") {
			t.Fatalf("oversized protocol proof error = %v", err)
		}
		if _, _, err := scopes.transformSymbolic("subst", []string{"x", "y", value}); err == nil || !strings.Contains(err.Error(), "work items") {
			t.Fatalf("oversized subst error = %v", err)
		}
	})
}

func TestEvaluateKbuildProbeWorkloadRequiresSourceDefinedCallTargets(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	_, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (struct{}, error) {
		parseOptions, err := scopes.Options("target", KbuildOptions{
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"flags"},
		})
		if err != nil {
			return struct{}{}, err
		}
		_, err = parseKbuildWithOptions(
			strings.NewReader(`flags := $(call cc-option,-fmissing-definition)`),
			"missing/Makefile",
			parseOptions,
			"",
		)
		return struct{}{}, err
	})
	if err == nil || !strings.Contains(err.Error(), `Kbuild call target "cc-option" is not defined`) {
		t.Fatalf("missing source-defined call target error=%v", err)
	}
}

func TestKbuildCompositeSymbolicSubstReplaysMakeCmdAndLowersConsumer(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Escaped string
		Joined  string
	}
	const makefile = linearFilterCompilerFixture + `
first := $(call cc-option,-fa)
second := $(call cc-option,-fb)
# This is the inner operation in scripts/Kbuild.include's make-cmd. The
# command deliberately retains two independent compiler-capability atoms.
escaped := $(subst $$,$$$$,literal$$(value):$(first):$(second))
# A multi-byte match can span two symbolic values and therefore exercises the
# bounded whole-expression representation rather than fragment-wise rewriting.
joined := $(subst -fa-fb,-fjoined,$(first)$(second))
downstream := $(call cc-option,$(joined))
`
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"escaped", "joined", "downstream"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), "scripts/Kbuild.include", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Escaped: parsed.Variables["escaped"], Joined: parsed.Variables["joined"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.Plan.Nodes); got != 3 {
		t.Fatalf("composite subst plan has %d nodes, want two inputs and one consumer: %#v", got, discovery.Plan.Nodes)
	}
	if got := len(linuxProbeSymbolPattern.FindAllString(discovery.Value.Escaped, -1)); got != 2 {
		t.Fatalf("make-cmd subst retained %d symbolic fragments, want 2: %q", got, discovery.Value.Escaped)
	}
	if got := len(linuxProbeSymbolPattern.FindAllString(discovery.Value.Joined, -1)); got != 1 {
		t.Fatalf("cross-boundary subst retained %d selection atoms, want 1: %q", got, discovery.Value.Joined)
	}
	consumer := discovery.Plan.Nodes[2]
	if !slices.Equal(consumer.Inputs, []string{discovery.Plan.Nodes[0].ID, discovery.Plan.Nodes[1].ID}) {
		t.Fatalf("subst consumer inputs = %q, want first two probes", consumer.Inputs)
	}
	request := discovery.Plan.Requests[consumer.RequestID]
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("raw symbolic subst value crossed the probe protocol boundary: %s", data)
	}
	foundJoined := false
	for _, step := range request.Steps {
		for _, conditional := range step.ConditionalArguments {
			if slices.Contains(conditional.Arguments, "-fjoined") {
				foundJoined = true
			}
		}
	}
	if !foundJoined {
		t.Fatalf("consumer request omitted the both-supported subst result: %#v", request.Steps)
	}

	for _, test := range []struct {
		name            string
		first, second   bool
		escaped, joined string
	}{
		{name: "neither", escaped: "literal$$(value)::"},
		{name: "first", first: true, escaped: "literal$$(value):-fa:", joined: "-fa"},
		{name: "second", second: true, escaped: "literal$$(value)::-fb", joined: "-fb"},
		{name: "both and boundary match", first: true, second: true, escaped: "literal$$(value):-fa:-fb", joined: "-fjoined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				switch index {
				case 0:
					return test.first
				case 1:
					return test.second
				default:
					return true
				}
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != (result{Escaped: test.escaped, Joined: test.joined}) {
				t.Fatalf("composite subst replay = %#v, want escaped %q and joined %q", replay.Value, test.escaped, test.joined)
			}
			if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
				t.Fatalf("composite subst replay plan has %d nodes, want %d", len(replay.Plan.Nodes), len(discovery.Plan.Nodes))
			}
			for index := range discovery.Plan.Nodes {
				got, want := replay.Plan.Nodes[index], discovery.Plan.Nodes[index]
				if got.ID != want.ID || got.RequestID != want.RequestID || !slices.Equal(got.Inputs, want.Inputs) {
					t.Fatalf("composite subst replay node %d = %#v, want %#v", index, got, want)
				}
			}
		})
	}
}

func TestKbuildCompositeSymbolicSubstRetainsCoupledStateBeyondBoundAsOpaqueMakeText(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var makefile strings.Builder
	makefile.WriteString(linearFilterCompilerFixture)
	for index := 0; index <= maxKbuildSymbolicComparisonReferences; index++ {
		fmt.Fprintf(&makefile, "flag_%d := $(call cc-option,-f%d)\n", index, index)
	}
	makefile.WriteString("joined := $(subst -f0-f1,replaced,")
	for index := 0; index <= maxKbuildSymbolicComparisonReferences; index++ {
		fmt.Fprintf(&makefile, "$(flag_%d)", index)
	}
	makefile.WriteString(")\n")

	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"joined"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile.String()), "Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["joined"], nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != maxKbuildSymbolicComparisonReferences+1 {
		t.Fatalf("coupled subst plan nodes = %d, want %d", len(discovery.Plan.Nodes), maxKbuildSymbolicComparisonReferences+1)
	}
	evaluator, symbol, ok := evaluatedScopes.symbolOwner(discovery.Value)
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil {
		t.Fatalf("coupled subst discovery value = %q, symbol = %#v", discovery.Value, symbol)
	}
	if symbol.makeText.function != "subst" || symbol.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable ||
		len(symbol.makeText.arguments) != 3 || symbol.makeText.arguments[0] != "-f0-f1" || symbol.makeText.arguments[1] != "replaced" {
		t.Fatalf("coupled subst exact AST = %#v", symbol.makeText)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	if _, _, _, lowerErr := lowerer.arguments([]string{discovery.Value}); lowerErr == nil || !strings.Contains(lowerErr.Error(), "no proven protocol lowering") {
		t.Fatalf("coupled subst process lowering error = %v", lowerErr)
	}

	for _, test := range []struct {
		name      string
		supported func(int) bool
	}{
		{name: "all", supported: func(int) bool { return true }},
		{name: "alternating", supported: func(index int) bool { return index%2 == 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				return test.supported(index)
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			var raw strings.Builder
			for index := 0; index <= maxKbuildSymbolicComparisonReferences; index++ {
				if test.supported(index) {
					fmt.Fprintf(&raw, "-f%d", index)
				}
			}
			want := strings.ReplaceAll(raw.String(), "-f0-f1", "replaced")
			if replay.Value != want {
				t.Fatalf("coupled subst replay = %q, want %q", replay.Value, want)
			}
			if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
				t.Fatalf("coupled subst replay plan nodes = %d, want %d", len(replay.Plan.Nodes), len(discovery.Plan.Nodes))
			}
		})
	}
}

func TestKbuildMakeCmdEscapesSourceDerivedGetconfTextExactly(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for scope, options := range map[string]*KbuildProbeScopeOptions{"target": &target, "host": &host} {
		options.Tools["script-runtime"] = "/configured/" + scope + "/script-runtime"
		options.Tools["scriptrun"] = "/configured/" + scope + "/scriptrun"
	}
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	const makefile = `
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
HOST_LFS_CFLAGS := $(shell getconf LFS_CFLAGS 2>/dev/null)
HOST_LFS_LDFLAGS := $(shell getconf LFS_LDFLAGS 2>/dev/null)
cmd_fixdep = __LINUX_BZL_ACTION_ROLE_host_cc__ $(HOST_LFS_CFLAGS) -o $@ scripts/basic/fixdep.c $(HOST_LFS_LDFLAGS) $(pound)tail
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
SAVED := $(call make-cmd,fixdep)
`
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"SAVED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), "scripts/Kbuild.include", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["SAVED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("getconf make-cmd plan = %#v, want CFLAGS and LDFLAGS host text probes", discovery.Plan.Nodes)
	}
	finalTokens := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)
	if len(finalTokens) != 2 {
		t.Fatalf("getconf make-cmd discovery has %d atoms, want two independently transformed text values: %q", len(finalTokens), discovery.Value)
	}
	seenQueries := map[string]bool{}
	for _, token := range finalTokens {
		evaluator, symbol, ok := evaluatedScopes.symbolOwner(token)
		if !ok || symbol.kind != "transformed-text" {
			t.Fatalf("getconf make-cmd atom %q = %#v, owner %#v; want transformed text", token, symbol, evaluator)
		}
		needles := []string{}
		for depth := 0; symbol.kind == "transformed-text"; depth++ {
			if depth >= maxKbuildSymbolicComparisonDepth || symbol.textTransform == nil {
				t.Fatalf("invalid transformed-text chain for %q: %#v", token, symbol)
			}
			if symbol.textTransform.function != "subst" || len(symbol.textTransform.arguments) != 3 {
				t.Fatalf("getconf transform = %#v, want pure subst", symbol.textTransform)
			}
			needles = append(needles, symbol.textTransform.arguments[0])
			sourceToken := symbol.textTransform.sourceToken
			var exists bool
			symbol, exists = evaluator.symbols[sourceToken]
			if !exists {
				t.Fatalf("transformed getconf source %q is not owned by its evaluator", sourceToken)
			}
		}
		if symbol.kind != "text" || symbol.reference.Scope != "host" {
			t.Fatalf("transformed getconf provenance = %#v, want host text request", symbol)
		}
		requestData, err := symbol.request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"LFS_CFLAGS", "LFS_LDFLAGS"} {
			if strings.Contains(string(requestData), "_CS_"+query) {
				seenQueries[query] = true
			}
		}
		if !slices.Contains(needles, "#") || slices.Contains(needles, `\#`) {
			t.Fatalf("getconf make-cmd subst needles = %q, want GNU Make's unescaped one-byte pound value", needles)
		}
	}
	if !seenQueries["LFS_CFLAGS"] || !seenQueries["LFS_LDFLAGS"] {
		t.Fatalf("getconf make-cmd provenance queries = %v, want CFLAGS and LDFLAGS", seenQueries)
	}

	resultRoot := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(resultRoot, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		requestData, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		value := "-Wl,$origin"
		if strings.Contains(string(requestData), "_CS_LFS_CFLAGS") {
			value = "-DPRICE=$cash -DHASH=#"
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "text", Text: value,
			Steps: []ProbeStepResult{
				{Name: "compile", Status: "success", ExitCode: 0},
				{Name: "link", Status: "success", ExitCode: 0},
				{Name: "execute", Status: "success", ExitCode: 0, Stdout: value + "\n"},
			},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resultRoot, "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oracle, err := NewProbeResultOracleFromTrees(map[string]string{"host": resultRoot}, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := `__LINUX_BZL_ACTION_ROLE_host_cc__ -DPRICE=$$cash -DHASH=$(pound) -o $$@ scripts/basic/fixdep.c -Wl,$$origin $(pound)tail`
	if replay.Value != want {
		t.Fatalf("getconf make-cmd replay = %q, want %q", replay.Value, want)
	}
	if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) || !slices.Equal(replay.Plan.Terminal, discovery.Plan.Terminal) {
		t.Fatalf("getconf make-cmd replay plan = %#v, want %#v", replay.Plan, discovery.Plan)
	}
	for index := range discovery.Plan.Nodes {
		got, want := replay.Plan.Nodes[index], discovery.Plan.Nodes[index]
		if got.ID != want.ID || got.RequestID != want.RequestID || !slices.Equal(got.Inputs, want.Inputs) {
			t.Fatalf("getconf make-cmd replay node %d = %#v, want %#v", index, got, want)
		}
	}
}
