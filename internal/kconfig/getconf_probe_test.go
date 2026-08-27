package kconfig

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func testLinuxGetconfEvaluator(t *testing.T, scope string, builder ProbeDiscovery, oracle ProbeResultLookup) *LinuxProbeEvaluator {
	t.Helper()
	fixtureIndex := 1
	if scope == "host" {
		fixtureIndex = 0
	}
	fixture := linuxCompilerBootstrapFixtures(t)[fixtureIndex]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{
		"cc":             "/configured/" + scope + "/cc",
		"script-runtime": "/configured/" + scope + "/script-runtime",
		"scriptrun":      "/configured/" + scope + "/scriptrun",
	}
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: scope, Architecture: "x86", SourceArchitecture: "x86",
		Facts: facts, Tools: tools, Discovery: builder, Oracle: oracle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

func TestLinuxGetconfProbeUsesGenericHostRolesAndRequest(t *testing.T) {
	request, err := linuxGetconfProbeRequest("LFS_VENDOR_EXTENSION")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := request.ToolRoles(), []string{"cc", "script-runtime", "scriptrun"}; !slices.Equal(got, want) {
		t.Fatalf("getconf tool roles = %q, want %q", got, want)
	}
	if request.InputCount != 0 || len(request.Steps) != 3 || len(request.Scratch) != 3 {
		t.Fatalf("getconf request = %#v, want a dependency-free compile and execute request", request)
	}
	compile := request.Steps[0]
	if compile.Name != "compile" || compile.Tool != "cc" ||
		!slices.Equal(compile.Arguments, []string{"-x", "c", "-c", "${scratch:getconf.c}", "-o", "${scratch:getconf.o}"}) {
		t.Fatalf("getconf compile step = %#v", compile)
	}
	if got := toolaction.InvocationContractRole(compile.Tool, compile.Arguments); got != "cc" {
		t.Fatalf("getconf compile argv selects contract %q, want cc", got)
	}
	link := request.Steps[1]
	if link.Name != "link" || link.Tool != "cc" || link.When == nil || link.When.Operator != "all" || len(link.When.Operands) != 2 ||
		toolaction.InvocationContractRole(link.Tool, link.Arguments) != "cc-link" {
		t.Fatalf("getconf link step = %#v", link)
	}
	if compileGuard, objectGuard := link.When.Operands[0], link.When.Operands[1]; compileGuard.Operator != "exit-zero" || compileGuard.Step != "compile" ||
		objectGuard.Operator != "regular-file" || objectGuard.Scratch != "getconf.o" {
		t.Fatalf("getconf link guards = %#v, want successful compile and regular object", link.When)
	}
	execute := request.Steps[2]
	if execute.Name != "execute" || execute.Tool != "scriptrun" || execute.When == nil ||
		execute.When.Operator != "all" || len(execute.When.Operands) != 2 || len(execute.Arguments) < 8 {
		t.Fatalf("getconf execute step = %#v", execute)
	}
	if linkGuard, outputGuard := execute.When.Operands[0], execute.When.Operands[1]; linkGuard.Operator != "exit-zero" || linkGuard.Step != "link" ||
		outputGuard.Operator != "regular-file" || outputGuard.Scratch != "getconf" {
		t.Fatalf("getconf execute guards = %#v, want successful link and regular executable", execute.When)
	}
	if got, want := execute.Arguments[:6], []string{
		"-interpreter", "${tool:script-runtime}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:script-runtime}",
	}; !slices.Equal(got, want) {
		t.Fatalf("getconf execute runtime argv = %q, want %q", got, want)
	}
	encodedScript := execute.Arguments[7]
	script, err := base64.StdEncoding.DecodeString(encodedScript)
	if err != nil || string(script) != linuxGetconfRunScript {
		t.Fatalf("getconf execution script = %q, %v", script, err)
	}
	if request.Outcome.Kind != "text" || request.Outcome.Step != "execute" ||
		request.Outcome.Stream != "stdout" || !request.Outcome.TrimSpace {
		t.Fatalf("getconf outcome = %#v, want trimmed execution stdout", request.Outcome)
	}

	source := request.Scratch[1].Content
	if !strings.Contains(source, "#ifdef _CS_LFS_VENDOR_EXTENSION") ||
		strings.Count(source, "_CS_LFS_VENDOR_EXTENSION") != 3 {
		t.Fatalf("getconf helper does not mechanically use its selected constant:\n%s", source)
	}
	for _, behavior := range []string{
		"#else\n\treturn 0;",
		"if (size == 0)",
		"if (confstr(_CS_LFS_VENDOR_EXTENSION, value, size) == 0)",
	} {
		if !strings.Contains(source, behavior) {
			t.Fatalf("getconf helper omits empty-result behavior %q:\n%s", behavior, source)
		}
	}
}

func TestLinuxGetconfProbeAcceptsOnlyTheOptionalUpstreamRedirect(t *testing.T) {
	for _, command := range []string{
		"getconf LFS_CFLAGS",
		"getconf LFS_CFLAGS 2>/dev/null",
	} {
		name, recognized, err := parseLinuxGetconfQuery(command)
		if err != nil || !recognized || name != "LFS_CFLAGS" {
			t.Fatalf("parse getconf query %q = (%q, %t, %v)", command, name, recognized, err)
		}
	}
}

func TestLinuxGetconfProbeRejectsMalformedOwnedQueries(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator := testLinuxGetconfEvaluator(t, "host", builder, nil)
	for _, command := range []string{
		"getconf LFS_",
		"getconf LFS_CFLAGS; id",
		"getconf LFS_CFLAGS 2>/tmp/result",
		"getconf LFS_CFLAGS 2> /dev/null",
		"getconf LFS_CFLAGS 2>/dev/null trailing",
		"getconf LFS_CFLAGS-DASH",
		"getconf LFS_" + strings.Repeat("A", linuxGetconfMaximumNameLen),
	} {
		t.Run(command, func(t *testing.T) {
			_, err := evaluator.Shell(context.Background(), command)
			if err == nil || IsLinuxProbeUnsupportedCommand(err) {
				t.Fatalf("malformed getconf query error = %v, want owned rejection", err)
			}
		})
	}
	if len(evaluator.References()) != 0 {
		t.Fatalf("malformed getconf queries emitted references: %#v", evaluator.References())
	}
	if _, err := evaluator.Shell(context.Background(), "getconf PATH"); err == nil || !IsLinuxProbeUnsupportedCommand(err) {
		t.Fatalf("unrelated getconf query error = %v, want delegable unsupported command", err)
	}
}

func TestLinuxGetconfTargetEvaluatorRecordsHostOwnedRequest(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator := testLinuxGetconfEvaluator(t, "target", builder, nil)
	value, err := evaluator.Shell(context.Background(), "getconf LFS_CFLAGS 2>/dev/null")
	if err != nil || !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("target-first getconf = %q, %v; want symbolic host result", value, err)
	}
	references := evaluator.References()
	if len(references) != 1 || references[0].Scope != "host" {
		t.Fatalf("target-first getconf references = %#v, want one host reference", references)
	}
	plan, err := builder.Plan(references...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].Scope != "host" {
		t.Fatalf("target-first getconf plan = %#v, want one host node", plan.Nodes)
	}
}

func TestKbuildHostTextComparisonStaysHostScopedAfterTargetFirstOwnership(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for scope, options := range map[string]*KbuildProbeScopeOptions{"target": &target, "host": &host} {
		options.Tools["script-runtime"] = "/configured/" + scope + "/script-runtime"
		options.Tools["scriptrun"] = "/configured/" + scope + "/scriptrun"
	}
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (string, error) {
		targetOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return "", err
		}
		hostOptions, err := scopes.Options("host", KbuildOptions{})
		if err != nil {
			return "", err
		}
		value, err := targetOptions.Shell("getconf LFS_CFLAGS 2>/dev/null")
		if err != nil {
			return "", err
		}
		selected, recognized, err := hostOptions.SelectSymbolic(value, "", false, "nonempty", "empty")
		if err != nil || !recognized {
			return "", fmt.Errorf("host text comparison recognized=%v: %v", recognized, err)
		}
		return selected, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 2 || evaluation.Plan.Nodes[0].Scope != "host" || evaluation.Plan.Nodes[1].Scope != "host" ||
		!slices.Equal(evaluation.Plan.Nodes[1].Inputs, []string{evaluation.Plan.Nodes[0].ID}) {
		t.Fatalf("target-first host text comparison plan = %#v", evaluation.Plan.Nodes)
	}
}

func TestKbuildCompleteTextSubstKeepsExactProcessLowering(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator := testLinuxGetconfEvaluator(t, "host", builder, nil)
	raw, err := evaluator.Shell(context.Background(), "getconf LFS_CFLAGS 2>/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"host": evaluator}}
	transformed, recognized, err := scopes.transformSymbolic("subst", []string{"foo", "bar", raw})
	if err != nil || !recognized {
		t.Fatalf("complete text subst = %q, recognized=%v, %v", transformed, recognized, err)
	}
	_, symbol, ok := scopes.symbolOwner(transformed)
	if !ok || symbol.kind != "transformed-text" || symbol.textTransform == nil || symbol.textTransform.function != "subst" {
		t.Fatalf("complete text subst symbol = %#v", symbol)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	base, conditional, fragments, err := lowerer.arguments([]string{transformed})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(base, []string{""}) || len(conditional) != 0 || len(fragments) != 1 || len(fragments[0].Fragments) != 1 ||
		len(fragments[0].Fragments[0].Transforms) != 1 || fragments[0].Fragments[0].Transforms[0].Function != "subst" {
		t.Fatalf("complete text subst lowering = base %q, conditional %#v, fragments %#v", base, conditional, fragments)
	}
}

func TestKbuildEmbeddedGetconfTextUsesExactMakeASTAndReplays(t *testing.T) {
	build := func(t *testing.T, oracle ProbeResultLookup) (*ProbePlanBuilder, *LinuxProbeEvaluator, string) {
		t.Helper()
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
		if err != nil {
			t.Fatal(err)
		}
		evaluator := testLinuxGetconfEvaluator(t, "host", builder, oracle)
		raw, err := evaluator.Shell(context.Background(), "getconf LFS_CFLAGS 2>/dev/null")
		if err != nil {
			t.Fatal(err)
		}
		scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"host": evaluator}}
		sorted, recognized, err := scopes.transformSymbolic("sort", []string{"fixed " + raw})
		if err != nil || !recognized {
			t.Fatalf("embedded getconf sort = %q, recognized=%v, %v", sorted, recognized, err)
		}
		_, symbol, ok := scopes.symbolOwner(sorted)
		if !ok || symbol.kind != "make-text" || symbol.makeText == nil ||
			symbol.makeText.function != "sort" || symbol.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable ||
			len(symbol.makeText.arguments) != 1 || symbol.makeText.arguments[0] != "fixed "+raw {
			t.Fatalf("embedded getconf exact Make AST = %#v", symbol)
		}
		return builder, evaluator, sorted
	}

	discoveryBuilder, discoveryEvaluator, discoveryValue := build(t, nil)
	plan, err := discoveryBuilder.Plan(discoveryEvaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("embedded getconf plan = %#v, want one text probe", plan.Nodes)
	}
	node := plan.Nodes[0]
	results := probeResultMap{node.ID: {
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: "host", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "-Dz -Da",
		Steps: []ProbeStepResult{
			{Name: "compile", Status: "success", ExitCode: 0},
			{Name: "link", Status: "success", ExitCode: 0},
			{Name: "execute", Status: "success", ExitCode: 0, Stdout: "-Dz -Da\n"},
		},
	}}
	_, replayEvaluator, replayValue := build(t, results)
	if replayValue != discoveryValue {
		t.Fatalf("embedded getconf replay token = %q, want stable %q", replayValue, discoveryValue)
	}
	resolved, err := replayEvaluator.ResolveSymbolic(replayValue)
	if err != nil || resolved != "-Da -Dz fixed" {
		t.Fatalf("embedded getconf exact replay = %q, %v; want %q", resolved, err, "-Da -Dz fixed")
	}
}

func TestLinuxGetconfRoutesToDependencyFreeHostProbeAndReplaysStably(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for scope, options := range map[string]*KbuildProbeScopeOptions{"target": &target, "host": &host} {
		options.Tools["script-runtime"] = "/configured/" + scope + "/script-runtime"
		options.Tools["scriptrun"] = "/configured/" + scope + "/scriptrun"
	}
	workloadOptions := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	const command = "getconf LFS_VENDOR_FLAGS 2>/dev/null"
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return "", err
		}
		value, err := options.Shell(command)
		if err != nil {
			return "", err
		}
		return options.ResolveSymbolic(value)
	}

	discovery, err := EvaluateKbuildProbeWorkload(workloadOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value) || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("getconf discovery = %q, nodes %#v", discovery.Value, discovery.Plan.Nodes)
	}
	node := discovery.Plan.Nodes[0]
	if node.Scope != "host" || len(node.Inputs) != 0 || discovery.Plan.Toolsets["host"] == "" {
		t.Fatalf("getconf node = %#v, toolsets %#v; want dependency-free host identity", node, discovery.Plan.Toolsets)
	}
	request := discovery.Plan.Requests[node.RequestID]
	if request.InputCount != 0 || !slices.Equal(request.ToolRoles(), []string{"cc", "script-runtime", "scriptrun"}) {
		t.Fatalf("getconf routed request = %#v", request)
	}

	resultRoot := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(resultRoot, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "text",
		Text: "-D_FILE_OFFSET_BITS=64",
		Steps: []ProbeStepResult{
			{Name: "compile", Status: "success", ExitCode: 0},
			{Name: "link", Status: "success", ExitCode: 0},
			{Name: "execute", Status: "success", ExitCode: 0, Stdout: " \t-D_FILE_OFFSET_BITS=64\n"},
		},
	}
	data, err := result.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultRoot, "results", node.ID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"host": resultRoot},
		discovery.Plan.Toolsets,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(workloadOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value != "-D_FILE_OFFSET_BITS=64" {
		t.Fatalf("getconf replay = %q, want measured host text", replay.Value)
	}
	if len(replay.Plan.Nodes) != 1 || replay.Plan.Nodes[0].ID != node.ID ||
		replay.Plan.Nodes[0].RequestID != node.RequestID ||
		!slices.Equal(replay.Plan.Terminal, discovery.Plan.Terminal) {
		t.Fatalf("getconf replay plan = %#v terminals %v, want %#v terminals %v", replay.Plan.Nodes, replay.Plan.Terminal, discovery.Plan.Nodes, discovery.Plan.Terminal)
	}
}

func TestTargetFirstGetconfFlagsFeedHostPreprocessorProbe(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for scope, options := range map[string]*KbuildProbeScopeOptions{"target": &target, "host": &host} {
		options.Tools["script-runtime"] = "/configured/" + scope + "/script-runtime"
		options.Tools["scriptrun"] = "/configured/" + scope + "/scriptrun"
	}
	workloadOptions := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		targetOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return "", err
		}
		flags, err := targetOptions.Shell("getconf LFS_CFLAGS 2>/dev/null")
		if err != nil {
			return "", err
		}
		hostOptions, err := scopes.Options("host", KbuildOptions{})
		if err != nil {
			return "", err
		}
		command := "echo '#include <libelf.h>' | " + KbuildActionRoleToken("host", "cc") +
			" -Werror -std=gnu11 " + flags + " -I scripts/include -x c -E - 2>/dev/null | grep elf_getshdr"
		value, err := hostOptions.Shell(command)
		if err != nil {
			return "", err
		}
		return hostOptions.ResolveSymbolic(value)
	}

	discovery, err := EvaluateKbuildProbeWorkload(workloadOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("target-first getconf host preprocessor nodes = %#v, want getconf and dependent preprocess", discovery.Plan.Nodes)
	}
	getconfNode, preprocessNode := discovery.Plan.Nodes[0], discovery.Plan.Nodes[1]
	if getconfNode.Scope != "host" || preprocessNode.Scope != "host" || !slices.Equal(preprocessNode.Inputs, []string{getconfNode.ID}) {
		t.Fatalf("target-first getconf host graph = %#v", discovery.Plan.Nodes)
	}
	preprocessRequest := discovery.Plan.Requests[preprocessNode.RequestID]
	if preprocessRequest.InputCount != 1 || len(preprocessRequest.Steps) != 1 {
		t.Fatalf("host preprocessor request = %#v", preprocessRequest)
	}
	step := preprocessRequest.Steps[0]
	if len(step.ArgumentFragments) != 1 {
		t.Fatalf("host preprocessor dynamic argv = %#v, want one getconf-backed slot", step.ArgumentFragments)
	}
	group := step.ArgumentFragments[0]
	if group.Index < 0 || group.Index >= len(step.Arguments) || step.Arguments[group.Index] != "" ||
		len(group.Fragments) != 1 || group.Fragments[0].Value != "${result:00000000.text}" {
		t.Fatalf("host preprocessor dynamic argv group = %#v in %q", group, step.Arguments)
	}
	requestData, err := preprocessRequest.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(requestData) {
		t.Fatalf("host preprocessor request leaked planner symbol: %s", requestData)
	}

	resultRoot := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(resultRoot, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	matched := true
	results := []ProbeResult{
		{
			Schema: LinuxProbeResultSchema, NodeID: getconfNode.ID, RequestID: getconfNode.RequestID,
			Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "text",
			Text: "-D_FILE_OFFSET_BITS=64 -D_LARGEFILE_SOURCE",
			Steps: []ProbeStepResult{
				{Name: "compile", Status: "success", ExitCode: 0},
				{Name: "link", Status: "success", ExitCode: 0},
				{Name: "execute", Status: "success", ExitCode: 0, Stdout: "-D_FILE_OFFSET_BITS=64 -D_LARGEFILE_SOURCE\n"},
			},
		},
		{
			Schema: LinuxProbeResultSchema, NodeID: preprocessNode.ID, RequestID: preprocessNode.RequestID,
			Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "boolean", Boolean: &matched,
			Steps: []ProbeStepResult{{Name: "preprocess", Status: "success", ExitCode: 0, Stdout: "elf_getshdr\n"}},
		},
	}
	for _, result := range results {
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resultRoot, "results", result.NodeID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oracle, err := NewProbeResultOracleFromTrees(map[string]string{"host": resultRoot}, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(workloadOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value != "y" || len(replay.Plan.Nodes) != 2 || replay.Plan.Nodes[1].ID != preprocessNode.ID {
		t.Fatalf("target-first getconf host replay = %#v, plan %#v", replay.Value, replay.Plan.Nodes)
	}
}

func TestObjtoolGetconfFlagsSurviveWordwiseFiltersArgCheckAndStrip(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	workloadOptions := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	type result struct {
		Flags              string
		ArgCheck           string
		RecordedCommand    string
		UnsafeCommand      string
		ArgCheckTransforms []ProbeValueTransform
		Probe              string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		targetOptions, err := scopes.Options("target", KbuildOptions{})
		if err != nil {
			return result{}, err
		}
		getconfFlags, err := targetOptions.Shell("getconf LFS_CFLAGS 2>/dev/null")
		if err != nil {
			return result{}, err
		}
		hostOptions, err := scopes.Options("host", KbuildOptions{
			Variables: map[string]string{
				"CC": KbuildActionRoleToken("host", "cc"), "CFLAGS": "-Werror " + getconfFlags + " -O2",
			},
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"c_flags", "arg_check", "recorded_command", "unsafe_command", "probe"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(`
basetarget := weak
obj := objtool
CFLAGS_REMOVE_weak.o :=
CFLAGS_REMOVE_objtool :=
c_flags_1 = -Wp,-MMD $(CFLAGS) -DKEEP
c_flags_2 = $(filter-out $(CFLAGS_REMOVE_$(basetarget).o), $(c_flags_1))
c_flags = $(filter-out $(CFLAGS_REMOVE_$(obj)), $(c_flags_2))
saved_command :=
arg_check = $(strip $(filter-out $(c_flags),$(saved_command)) $(filter-out $(saved_command),$(c_flags)))
pound := \#
cmd = cc $(c_flags) -c
make_cmd = $(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd)))
recorded_command = prefix $(make_cmd) suffix
unsafe_command = prefix $(subst x,,$(strip x $(arg_check))) suffix
probe := $(shell echo '\#include <libelf.h>' | $(CC) $(c_flags) -std=gnu11 -x c -E - 2>/dev/null | grep elf_getshdr)
`), "tools/build/Build.include", hostOptions, "")
		if err != nil {
			return result{}, err
		}
		flags := parsed.Variables["c_flags"]
		argCheck := parsed.Variables["arg_check"]
		argLowerer := newProbeSymbolicValueLowerer(scopes.evaluators["host"])
		_, _, groups, err := argLowerer.arguments(strings.Fields(argCheck))
		if err != nil {
			return result{}, err
		}
		var transforms []ProbeValueTransform
		for _, group := range groups {
			for _, fragment := range group.Fragments {
				transforms = append(transforms, fragment.Transforms...)
			}
		}
		return result{
			Flags: flags, ArgCheck: argCheck, RecordedCommand: parsed.Variables["recorded_command"], UnsafeCommand: parsed.Variables["unsafe_command"],
			ArgCheckTransforms: transforms, Probe: parsed.Variables["probe"],
		}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(workloadOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("objtool filter plan nodes = %#v, want getconf and dependent preprocess", discovery.Plan.Nodes)
	}
	preprocess := discovery.Plan.Requests[discovery.Plan.Nodes[1].RequestID]
	if len(preprocess.Steps) != 1 || len(preprocess.Steps[0].ArgumentFragments) != 1 {
		t.Fatalf("objtool preprocess request = %#v", preprocess)
	}
	fragments := preprocess.Steps[0].ArgumentFragments[0].Fragments
	var dynamic []ProbeValueFragment
	for _, fragment := range fragments {
		if fragment.Value == "${result:00000000.text}" {
			dynamic = append(dynamic, fragment)
		}
	}
	if len(dynamic) != 1 || len(dynamic[0].Transforms) != 0 {
		t.Fatalf("objtool dynamic fragments = %#v, want one exact getconf result after identity filters", fragments)
	}
	gotArgCheckChain := make([]string, len(discovery.Value.ArgCheckTransforms))
	for index, transform := range discovery.Value.ArgCheckTransforms {
		gotArgCheckChain[index] = transform.Function
	}
	// All three filter-out operations are source-proven identities for this
	// empty removal/saved-command fixture. Only the final strip remains as a
	// runtime text transform.
	wantArgCheckChain := []string{"strip"}
	if !slices.Equal(gotArgCheckChain, wantArgCheckChain) {
		t.Fatalf("objtool arg-check transform chain = %q, want %q (value %q)", gotArgCheckChain, wantArgCheckChain, discovery.Value.ArgCheck)
	}
	requestData, err := preprocess.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(requestData) {
		t.Fatalf("objtool filtered request leaked planner token: %s", requestData)
	}

	resultRoot := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(resultRoot, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	matched := true
	results := []ProbeResult{
		{
			Schema: LinuxProbeResultSchema, NodeID: discovery.Plan.Nodes[0].ID, RequestID: discovery.Plan.Nodes[0].RequestID,
			Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "text", Text: "",
			Steps: []ProbeStepResult{
				{Name: "compile", Status: "success", ExitCode: 0},
				{Name: "link", Status: "success", ExitCode: 0},
				{Name: "execute", Status: "success", ExitCode: 0},
			},
		},
		{
			Schema: LinuxProbeResultSchema, NodeID: discovery.Plan.Nodes[1].ID, RequestID: discovery.Plan.Nodes[1].RequestID,
			Scope: "host", ToolsetIdentity: discovery.Plan.Toolsets["host"], Kind: "boolean", Boolean: &matched,
			Steps: []ProbeStepResult{{Name: "preprocess", Status: "success", ExitCode: 0, Stdout: "elf_getshdr\n"}},
		},
	}
	for _, probeResult := range results {
		data, err := probeResult.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resultRoot, "results", probeResult.NodeID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oracle, err := NewProbeResultOracleFromTrees(map[string]string{"host": resultRoot}, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(workloadOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	wantFlags := "-Wp,-MMD -Werror -O2 -DKEEP"
	if replay.Value.Flags != wantFlags || replay.Value.ArgCheck != wantFlags {
		t.Fatalf("empty getconf normalized flags=%q arg_check=%q, want %q", replay.Value.Flags, replay.Value.ArgCheck, wantFlags)
	}
	if want := "prefix cc " + wantFlags + " -c suffix"; replay.Value.RecordedCommand != want {
		t.Fatalf("empty getconf recorded command = %q, want %q", replay.Value.RecordedCommand, want)
	}
	if want := "prefix  -Wp,-MMD -Werror -O2 -DKEEP suffix"; replay.Value.UnsafeCommand != want {
		t.Fatalf("word-erasing subst command = %q, want unnormalized %q", replay.Value.UnsafeCommand, want)
	}
}
