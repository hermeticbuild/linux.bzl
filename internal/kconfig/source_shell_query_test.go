package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type sourceShellQueryWorkloadValue struct {
	version string
	target  string
	rules   []KbuildRule
}

type sourceShellQueryBindgenParametersValue struct {
	parameters string
}

func TestKbuildSourceShellRoutesScopedCompilerAfterLiteralEnvironmentPrefix(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	target, targetFixture := newFixtureProbeEvaluator(t, 1, "arm64", builder, nil, false)
	host, _ := newFixtureProbeEvaluator(t, 0, "x86", builder, nil, false)
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{
		"target": target,
		"host":   host,
	}}

	command := "LC_ALL=C " + KbuildActionRoleToken("target", "cc") + " --version 2>/dev/null | head -n 1"
	got, err := scopes.kbuildSourceShell(context.Background(), command, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != targetFixture.versionText {
		t.Fatalf("source-phase compiler version = %q, want target bootstrap fact %q", got, targetFixture.versionText)
	}
	if references := scopes.References(); len(references) != 0 {
		t.Fatalf("bootstrap-backed compiler version emitted probe references: %#v", references)
	}
}

func TestKbuildSourceShellEnvironmentPrefixesStayLiteralAndBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Kconfig"), []byte("mainmenu \"fixture\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "versions.map"), []byte("VERSION_1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 0, "x86", builder, nil, false)
	evaluator.sourceRoot = root
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/runtime"

	value, err := evaluator.sourceShellQuery(
		context.Background(),
		"LC_ALL='C' grep -oE '^VERSION_([0-9.]+)' versions.map",
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("literal environment source query = %q, want symbolic text", value)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("literal environment source query nodes = %#v, want one", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	arguments := strings.Join(request.Steps[0].Arguments, " ")
	if !strings.Contains(arguments, "LC_ALL='C' grep '-oE' '^VERSION_([0-9.]+)' 'versions.map'") {
		t.Fatalf("literal environment source query argv = %q", arguments)
	}

	for name, test := range map[string]struct {
		command string
		error   string
	}{
		"dynamic parameter": {
			command: `LC_ALL=$LANG grep -oE '^VERSION_([0-9.]+)' versions.map`,
			error:   "dynamic shell text",
		},
		"command substitution": {
			command: "LC_ALL=`locale` grep -oE '^VERSION_([0-9.]+)' versions.map",
			error:   "dynamic shell text",
		},
		"program lookup": {
			command: `PATH=/tmp grep -oE '^VERSION_([0-9.]+)' versions.map`,
			error:   "unsupported environment assignment",
		},
		"ambient locale": {
			command: `LC_ALL=en_US.UTF-8 grep -oE '^VERSION_([0-9.]+)' versions.map`,
			error:   "unsupported environment assignment",
		},
		"program allowlist": {
			command: `LC_ALL=C date -u versions.map`,
			error:   "outside the pure source-filter grammar",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := evaluator.sourceShellQuery(context.Background(), test.command, root)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("source-shell environment error = %v, want %q", err, test.error)
			}
		})
	}
}

func TestKbuildSourceShellQueryRejectsAmbientAuthority(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "tools", "lib", "bpf")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for filename, content := range map[string]string{
		filepath.Join(root, "Kconfig"):         "mainmenu \"fixture\"\n",
		filepath.Join(directory, "libbpf.map"): "LIBBPF_1.5.0 { };\n",
	} {
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evaluator := &LinuxProbeEvaluator{
		scope: "host", architecture: "x86", sourceRoot: root,
		tools: map[string]string{
			linuxProbeScriptRunner:  "/configured/scriptrun",
			linuxProbeScriptRuntime: "/configured/runtime",
		},
	}
	validPrefix := "grep -oE '^LIBBPF_([0-9.]+)' libbpf.map"
	for name, test := range map[string]struct {
		command string
		error   string
	}{
		"quoted ambient file": {
			command: validPrefix + ` | grep -oE '^x' "/etc/hostname"`,
			error:   "undeclared or escaping source operand",
		},
		"attached file option": {
			command: `grep -f/etc/hostname '^x' libbpf.map`,
			error:   "unsupported option",
		},
		"one-component file option": {
			command: `grep -flibbpf.map '^x' libbpf.map`,
			error:   "unsupported option",
		},
		"inverse pattern file option": {
			command: `grep -Ev -f/etc/hostname libbpf.map`,
			error:   "unsupported option",
		},
		"inverse ambient file": {
			command: `grep -Ev '^x' /etc/hostname`,
			error:   "undeclared or escaping source operand",
		},
		"mixed extraction inversion": {
			command: `grep -oEv '^x' libbpf.map`,
			error:   "requires either -oE extraction or -Ev whole-line inversion",
		},
		"newline command": {
			command: validPrefix + "\ncut -d'_' -f2",
			error:   "empty, invalid",
		},
		"ambient program": {
			command: validPrefix + " | date -u",
			error:   "outside the pure source-filter grammar",
		},
		"sort output": {
			command: validPrefix + " | sort -oresult",
			error:   "unsupported option or operand",
		},
		"head file operand": {
			command: validPrefix + " | head -n1 libbpf.map",
			error:   "unsupported option or operand",
		},
		"cut file operand": {
			command: validPrefix + " | cut -d'_' -f2 libbpf.map",
			error:   "unsupported option or operand",
		},
		"concatenated quoted pattern": {
			command: `grep -oE ''/etc/*'' libbpf.map`,
			error:   "must be one quoted literal",
		},
		"protocol placeholder pattern": {
			command: `grep -oE '${source_root:linux}' libbpf.map`,
			error:   "reserved protocol placeholder syntax",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := evaluator.sourceShellQuery(context.Background(), test.command, directory)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("unsafe source-shell query error = %v, want %q for %q", err, test.error, test.command)
			}
		})
	}

	outside := t.TempDir()
	traversal, err := filepath.Rel(outside, filepath.Join(directory, "libbpf.map"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = evaluator.sourceShellQuery(
		context.Background(),
		"grep -oE '^LIBBPF_([0-9.]+)' "+sourceShellQueryScriptWord(filepath.ToSlash(traversal)),
		outside,
	)
	if err == nil || !strings.Contains(err.Error(), "undeclared or escaping source operand") {
		t.Fatalf("outside-cwd relative traversal error = %v, want containment rejection", err)
	}
}

func TestKbuildSourceShellQueryResolvesParseTimeTargetTopology(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root'with-quote")
	directory := filepath.Join(root, "tools", "lib", "bpf")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for filename, content := range map[string]string{
		filepath.Join(root, "Kconfig"):         "mainmenu \"fixture\"\n",
		filepath.Join(directory, "libbpf.map"): "LIBBPF_0.0.1 { };\nLIBBPF_1.5.0 { };\n",
	} {
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for _, options := range []*KbuildProbeScopeOptions{&target, &host} {
		options.SourceRoot = root
		options.Tools[linuxProbeScriptRunner] = "/configured/scriptrun"
		options.Tools[linuxProbeScriptRuntime] = "/configured/runtime"
	}
	// An override remains toolchain data. The query lowerer carries it without
	// embedding grep/sort/cut compatibility or executable paths in Go.
	host.Tools[compactKbuildScriptAppletRolePrefix+"sort"] = "/configured/sort"
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}

	const makefile = `VERSION_SCRIPT := libbpf.map
LIBBPF_VERSION := $(shell grep -oE '^LIBBPF_([0-9.]+)' $(VERSION_SCRIPT) | sort -rV | head -n1 | cut -d'_' -f2)
LIB_TARGET = libbpf.a libbpf.so.$(LIBBPF_VERSION)
OUTPUT := out/
LIB_TARGET := $(addprefix $(OUTPUT),$(LIB_TARGET))
all: $(LIB_TARGET)
$(LIB_TARGET):
	@echo $@
`
	workload := func(scopes *KbuildProbeScopes) (sourceShellQueryWorkloadValue, error) {
		options, err := scopes.Options("target", KbuildOptions{
			WorkingDir: directory, MakeVariablesComplete: true,
			CaptureVariables: []string{"LIBBPF_VERSION", "LIB_TARGET"},
		})
		if err != nil {
			return sourceShellQueryWorkloadValue{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), filepath.Join(directory, "Makefile"), options, directory)
		if err != nil {
			return sourceShellQueryWorkloadValue{}, err
		}
		return sourceShellQueryWorkloadValue{
			version: parsed.Variables["LIBBPF_VERSION"],
			target:  parsed.Variables["LIB_TARGET"],
			rules:   slices.Clone(parsed.Rules),
		}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 || discovery.Plan.Nodes[0].Scope != "host" {
		t.Fatalf("source query plan nodes = %#v, want one host node", discovery.Plan.Nodes)
	}
	request := discovery.Plan.Requests[discovery.Plan.Nodes[0].RequestID]
	if got, want := request.Sources, []string{"Kconfig", "tools/lib/bpf/libbpf.map"}; !slices.Equal(got, want) {
		t.Fatalf("source query inputs = %q, want %q", got, want)
	}
	step := request.Steps[0]
	if step.Tool != linuxProbeScriptRunner || step.WorkingDirectory != "${source_root:linux}/tools/lib/bpf" ||
		!request.Outcome.GNUMakeShell || !request.Outcome.RequireSuccess || !request.Outcome.SingleMakeWord {
		t.Fatalf("source query request = %#v", request)
	}
	// The runtime and applet overrides are explicit path arguments, not tools
	// invoked through configured compiler-action envelopes. Forwarding their
	// contracts would make scriptrun treat script-runtime as an unbound external
	// proxy instead of its interpreter/multicall runtime.
	if len(step.AuxiliaryTools) != 0 {
		t.Fatalf("source query auxiliary action contracts = %q, want none", step.AuxiliaryTools)
	}
	if got, want := request.ToolRoles(), []string{"script-applet-sort", "script-runtime", "scriptrun"}; !slices.Equal(got, want) {
		t.Fatalf("source query tool roles = %q, want %q", got, want)
	}
	joined := strings.Join(step.Arguments, " ")
	if strings.Contains(joined, "${source_root:") {
		t.Fatalf("source query script embeds a late-expanded source-root path: %q", joined)
	}
	for _, required := range []string{
		"-script_content",
		"grep '-oE' '^LIBBPF_([0-9.]+)' 'libbpf.map' | sort '-rV' | head '-n1' | cut '-d_' '-f2'",
		"-applet sort=${tool:script-applet-sort}",
		"-require_applet cut",
		"-require_applet grep",
		"-require_applet head",
		"-require_applet sh",
		"-require_applet sort",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("source query argv %q omits %q", joined, required)
		}
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.version) || !linuxProbeSymbolPattern.MatchString(discovery.Value.target) {
		t.Fatalf("discovery topology = %#v, want source-query symbolic values", discovery.Value)
	}
	discoveryTargets := strings.Fields(discovery.Value.target)
	if len(discoveryTargets) != 2 || discoveryTargets[0] != "out/libbpf.a" ||
		!strings.HasPrefix(discoveryTargets[1], "out/libbpf.so.") ||
		!linuxProbeSymbolPattern.MatchString(discoveryTargets[1]) {
		t.Fatalf("discovery library targets = %q, want static and symbolic shared targets", discoveryTargets)
	}
	assertSourceShellQueryRuleTopology(t, discovery.Value.rules, discoveryTargets)

	roots := writeSourceShellQueryTextResults(t, discovery.Plan, "1.5.0\n")
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.version != "1.5.0" || replay.Value.target != "out/libbpf.a out/libbpf.so.1.5.0" {
		t.Fatalf("replayed parse-time topology = %#v", replay.Value)
	}
	assertSourceShellQueryRuleTopology(t, replay.Value.rules, []string{"out/libbpf.a", "out/libbpf.so.1.5.0"})
	if replay.Plan.Nodes[0].ID != discovery.Plan.Nodes[0].ID {
		t.Fatalf("replayed source query node = %s, discovered %s", replay.Plan.Nodes[0].ID, discovery.Plan.Nodes[0].ID)
	}

	invalidRoots := writeSourceShellQueryTextResults(t, discovery.Plan, "1.5.0 2\n")
	invalidOracle, err := NewProbeResultOracleFromTrees(invalidRoots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateKbuildProbeWorkload(opts, invalidOracle, workload); err == nil ||
		!strings.Contains(err.Error(), "not one nonempty Make word") {
		t.Fatalf("multi-word source query replay error = %v", err)
	}
}

func TestKbuildSourceShellQueryReplaysUpstreamBindgenParametersWordList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "linux-source")
	objectRoot := filepath.Join(t.TempDir(), "linux-object")
	if err := os.MkdirAll(filepath.Join(root, "rust"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for filename, content := range map[string]string{
		filepath.Join(root, "Kconfig"): "mainmenu \"fixture\"\n",
		filepath.Join(root, "rust", "bindgen_parameters"): `# SPDX-License-Identifier: GPL-2.0

--allowlist-type block_device
--allowlist-var init_task
`,
	} {
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	for _, options := range []*KbuildProbeScopeOptions{&target, &host} {
		options.SourceRoot = root
		options.Tools[linuxProbeScriptRunner] = "/configured/scriptrun"
		options.Tools[linuxProbeScriptRuntime] = "/configured/runtime"
	}
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}

	const makefile = `BINDGEN_PARAMETERS := $(shell grep -Ev '^#|^$$' $(srctree)/rust/bindgen_parameters)
all:
	@echo $(BINDGEN_PARAMETERS)
`
	workload := func(scopes *KbuildProbeScopes) (sourceShellQueryBindgenParametersValue, error) {
		options, err := scopes.Options("target", KbuildOptions{
			// External modules evaluate source Makefiles from the configured
			// kernel object tree, outside the immutable Linux source root.
			WorkingDir:            objectRoot,
			CommandLineVariables:  map[string]string{"srctree": "__LINUX_BZL_SOURCE_TREE__"},
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"BINDGEN_PARAMETERS"},
		})
		if err != nil {
			return sourceShellQueryBindgenParametersValue{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(makefile), filepath.Join(root, "rust", "Makefile"), options, root)
		if err != nil {
			return sourceShellQueryBindgenParametersValue{}, err
		}
		return sourceShellQueryBindgenParametersValue{parameters: parsed.Variables["BINDGEN_PARAMETERS"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.parameters) {
		t.Fatalf("discovered bindgen parameters = %q, want symbolic text", discovery.Value.parameters)
	}
	if len(discovery.Plan.Nodes) != 1 || discovery.Plan.Nodes[0].Scope != "host" {
		t.Fatalf("bindgen-parameter source query nodes = %#v, want one host node", discovery.Plan.Nodes)
	}
	request := discovery.Plan.Requests[discovery.Plan.Nodes[0].RequestID]
	if got, want := request.Sources, []string{"Kconfig", "rust/bindgen_parameters"}; !slices.Equal(got, want) {
		t.Fatalf("bindgen-parameter sources = %q, want %q", got, want)
	}
	if !request.Outcome.GNUMakeShell || !request.Outcome.RequireSuccess || request.Outcome.SingleMakeWord {
		t.Fatalf("bindgen-parameter outcome = %#v, want unconstrained GNU Make word list", request.Outcome)
	}
	step := request.Steps[0]
	if got, want := step.WorkingDirectory, "${source_root:linux}"; got != want {
		t.Fatalf("bindgen-parameter working directory = %q, want %q", got, want)
	}
	joined := strings.Join(step.Arguments, " ")
	for _, want := range []string{
		"grep '-Ev' '^#|^$' 'rust/bindgen_parameters'",
		"-require_applet grep",
		"-require_applet sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bindgen-parameter source query argv %q omits %q", joined, want)
		}
	}

	const stdout = "--allowlist-type block_device\n--allowlist-var init_task\n"
	roots := writeSourceShellQueryTextResults(t, discovery.Plan, stdout)
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replay.Value.parameters, NormalizeGNUMakeShellOutput(stdout); got != want {
		t.Fatalf("replayed bindgen parameters = %q, want %q", got, want)
	}
	if replay.Plan.Nodes[0].ID != discovery.Plan.Nodes[0].ID {
		t.Fatalf("replayed bindgen-parameter node = %s, discovered %s", replay.Plan.Nodes[0].ID, discovery.Plan.Nodes[0].ID)
	}
}

func TestSourceShellQueryScriptWordSuppressesShellExpansion(t *testing.T) {
	for value, want := range map[string]string{
		"*":           "'*'",
		"$HOME":       "'$HOME'",
		"a b":         "'a b'",
		"a'b":         `'a'"'"'b'`,
		"$(hostname)": "'$(hostname)'",
	} {
		if got := sourceShellQueryScriptWord(value); got != want {
			t.Errorf("sourceShellQueryScriptWord(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestSourceShellQueryRelativeOperand(t *testing.T) {
	for _, test := range []struct {
		source    string
		directory string
		want      string
	}{
		{source: "tools/lib/bpf/libbpf.map", directory: "tools/lib/bpf", want: "libbpf.map"},
		{source: "tools/lib/-version.map", directory: "tools/lib", want: "./-version.map"},
		{source: "include/uapi/linux/bpf.h", directory: "tools/lib/bpf", want: "../../../include/uapi/linux/bpf.h"},
	} {
		got, err := sourceShellQueryRelativeOperand(test.source, test.directory)
		if err != nil || got != test.want {
			t.Errorf("sourceShellQueryRelativeOperand(%q, %q) = %q, %v; want %q", test.source, test.directory, got, err, test.want)
		}
	}
}

func assertSourceShellQueryRuleTopology(t *testing.T, rules []KbuildRule, targets []string) {
	t.Helper()
	allFound := false
	concreteRuleFound := false
	for _, rule := range rules {
		if slices.Contains(rule.Targets, "all") {
			allFound = slices.Equal(rule.Prerequisites, targets)
		}
		if slices.Equal(rule.Targets, targets) {
			concreteRuleFound = true
		}
	}
	if !allFound || !concreteRuleFound {
		t.Fatalf("rules do not preserve all prerequisites and concrete target rule for %q: %#v", targets, rules)
	}
}

func writeSourceShellQueryTextResults(t *testing.T, plan *ProbePlan, stdout string) map[string]string {
	t.Helper()
	roots := map[string]string{}
	for _, scope := range []string{"host", "target"} {
		root := filepath.Join(t.TempDir(), scope)
		if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
			t.Fatal(err)
		}
		roots[scope] = root
	}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		text := NormalizeGNUMakeShellOutput(stdout)
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "text", Text: text,
			Steps: []ProbeStepResult{{Name: request.Outcome.Step, Status: "success", ExitCode: 0, Stdout: stdout}},
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

func TestKbuildSourceShellDoesNotCatchOwnedToolFailure(t *testing.T) {
	called := false
	_, err := parseKbuildWithOptions(strings.NewReader(`value := $(shell configured-cc malformed source.map)
`), "Makefile", KbuildOptions{
		MakeVariablesComplete: true,
		Shell: func(command string) (string, error) {
			return "", &LinuxProbeOwnedUnsupportedCommandError{Architecture: "x86", Command: command}
		},
		SourceShell: func(command, workingDirectory string) (string, error) {
			called = true
			return "unexpected", nil
		},
		CaptureVariables: []string{"value"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "unsupported symbolic Linux Kconfig probe command") {
		t.Fatalf("owned tool parse error = %v", err)
	}
	if called {
		t.Fatal("owned tool failure fell through to source-shell query")
	}
}

func TestKbuildSingleMakeWordSourceTextPreservesAddAffixBoundaries(t *testing.T) {
	provenToken := linuxProbeSymbolPrefix + strings.Repeat("1", 64)
	unprovenToken := linuxProbeSymbolPrefix + strings.Repeat("2", 64)
	evaluator := &LinuxProbeEvaluator{
		scope: "host",
		symbols: map[string]linuxProbeSymbol{
			provenToken: {
				kind: "text", reference: ProbeReference{Scope: "host", Kind: "text"},
				request: ProbeRequest{Outcome: ProbeOutcome{Kind: "text", SingleMakeWord: true}},
			},
			unprovenToken: {
				kind: "text", reference: ProbeReference{Scope: "host", Kind: "text"},
				request: ProbeRequest{Outcome: ProbeOutcome{Kind: "text"}},
			},
		},
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"host": evaluator}}

	got, recognized, err := scopes.transformSymbolic(
		"addprefix", []string{"out/", "libbpf.a libbpf.so." + provenToken},
	)
	want := "out/libbpf.a out/libbpf.so." + provenToken
	if err != nil || !recognized || got != want {
		t.Fatalf("single-word addprefix = %q, recognized=%v, %v; want %q", got, recognized, err, want)
	}
	got, recognized, err = scopes.transformSymbolic("addsuffix", []string{".o", "src/" + provenToken})
	want = "src/" + provenToken + ".o"
	if err != nil || !recognized || got != want {
		t.Fatalf("single-word addsuffix = %q, recognized=%v, %v; want %q", got, recognized, err, want)
	}

	unsafe, recognized, err := scopes.transformSymbolic("addprefix", []string{"out/", "lib." + unprovenToken})
	if err != nil || !recognized || !linuxProbeSymbolPattern.MatchString(unsafe) {
		t.Fatalf("unproven addprefix = %q, recognized=%v, %v; want fail-closed whole Make text", unsafe, recognized, err)
	}
	if _, err := scopes.resolveSymbolicWordsForScope("host", unsafe); err == nil ||
		!strings.Contains(err.Error(), "no proven Make-word lowering") {
		t.Fatalf("unproven addprefix word-lowering error = %v", err)
	}
}
