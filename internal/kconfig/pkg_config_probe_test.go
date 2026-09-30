package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/pkgconfigmanifest"
)

type pkgConfigProbeWorkloadValue struct {
	cflags string
	libs   string
}

func pkgConfigProbeOptions(t *testing.T) KbuildProbeWorkloadOptions {
	t.Helper()
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	host.Tools[linuxProbePkgConfigRole] = "/configured/pkgconfigshim"
	host.Tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	host.Tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	return KbuildProbeWorkloadOptions{Target: target, Host: &host}
}

func writePkgConfigProbeResults(t *testing.T, plan *ProbePlan) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if node.Scope != "host" || len(request.Steps) != 1 {
			t.Fatalf("pkg-config probe node = %#v request = %#v", node, request)
		}
		stdout := "-I__LINUX_BZL_HOST_DEPS__/openssl/include -DOPENSSL_API_COMPAT=30000\n"
		if strings.Contains(strings.Join(request.Steps[0].Arguments, " "), "--libs") {
			stdout = "-L__LINUX_BZL_HOST_DEPS__/openssl/lib -lcrypto\n"
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: "host", ToolsetIdentity: plan.Toolsets["host"], Kind: "text",
			Text: NormalizeGNUMakeShellOutput(stdout),
			Steps: []ProbeStepResult{{
				Name: request.Steps[0].Name, Status: "success", ExitCode: 0, Stdout: stdout,
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
	return map[string]string{"host": root}
}

func TestConfiguredPkgConfigQueriesDiscoverAndReplayHostFlags(t *testing.T) {
	opts := pkgConfigProbeOptions(t)
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte(`
HOSTPKG_CONFIG = ambient-pkg-config
HOSTCFLAGS_extract-cert.o = $(shell $(HOSTPKG_CONFIG) --cflags libcrypto 2> /dev/null) -I$(srctree)/scripts
HOSTLDLIBS_extract-cert = $(shell $(HOSTPKG_CONFIG) libcrypto --libs 2>/dev/null || echo -lcrypto)
certs/extract-cert.o: certs/extract-cert.c
	$(HOSTCC) $(HOSTCFLAGS_extract-cert.o) -c -o $@ $<
`), 0o600); err != nil {
		t.Fatal(err)
	}
	workload := func(scopes *KbuildProbeScopes) (pkgConfigProbeWorkloadValue, error) {
		options, err := scopes.Options("host", KbuildOptions{
			RootDir: root,
			CommandLineVariables: map[string]string{
				"HOSTCC":         KbuildActionRoleToken("host", "cc"),
				"HOSTPKG_CONFIG": KbuildActionRoleToken("host", linuxProbePkgConfigRole),
				"srctree":        "__LINUX_BZL_SOURCE_TREE__",
			},
			MakeVariablesComplete:  true,
			CaptureTargetEvaluator: true,
		})
		if err != nil {
			return pkgConfigProbeWorkloadValue{}, err
		}
		parsed, err := ParseKbuildFileTree(makefile, options)
		if err != nil {
			return pkgConfigProbeWorkloadValue{}, err
		}
		profile, err := NewCompactKbuildProfile("certs:pkg-config", makefile, root, parsed)
		if err != nil {
			return pkgConfigProbeWorkloadValue{}, err
		}
		values, err := EvaluateCompactKbuildTarget(
			profile, "certs/extract-cert.o", "", []string{"certs/extract-cert.c"}, nil, nil,
			"HOSTCFLAGS_extract-cert.o", "HOSTLDLIBS_extract-cert",
		)
		return pkgConfigProbeWorkloadValue{
			cflags: values["HOSTCFLAGS_extract-cert.o"],
			libs:   values["HOSTLDLIBS_extract-cert"],
		}, err
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.cflags) || !linuxProbeSymbolPattern.MatchString(discovery.Value.libs) {
		t.Fatalf("pkg-config discovery values = %#v, want symbolic text", discovery.Value)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("pkg-config probe nodes = %#v, want two", discovery.Plan.Nodes)
	}
	for _, node := range discovery.Plan.Nodes {
		if node.Scope != "host" {
			t.Errorf("pkg-config node scope = %q, want host", node.Scope)
		}
		request := discovery.Plan.Requests[node.RequestID]
		if request.InputCount != 0 || len(request.Steps) != 1 {
			t.Fatalf("pkg-config request = %#v", request)
		}
		step := request.Steps[0]
		if step.Tool != linuxProbeScriptRunner || !slices.Equal(step.AuxiliaryTools, []string{linuxProbePkgConfigRole}) {
			t.Fatalf("pkg-config step = %#v", step)
		}
		if got, want := request.ToolRoles(), []string{linuxProbePkgConfigRole, linuxProbeScriptRuntime, linuxProbeScriptRunner}; !slices.Equal(got, want) {
			t.Fatalf("pkg-config roles = %q, want %q", got, want)
		}
		if !request.Outcome.GNUMakeShell || !request.Outcome.RequireSuccess || request.Outcome.SingleMakeWord {
			t.Fatalf("pkg-config outcome = %#v", request.Outcome)
		}
		arguments := strings.Join(step.Arguments, " ")
		for _, want := range []string{
			"-script_content", "pkg-config=${tool:pkg-config}", "-require_applet sh",
		} {
			if !strings.Contains(arguments, want) {
				t.Errorf("pkg-config probe argv %q omits %q", arguments, want)
			}
		}
		if strings.Contains(arguments, "/configured/") || strings.Contains(arguments, kbuildActionRoleTokenPrefix) {
			t.Errorf("pkg-config probe argv leaks configured spelling or source token: %q", arguments)
		}
		if strings.Contains(arguments, "--cflags") {
			if !strings.Contains(arguments, "pkg-config '--cflags' 'libcrypto' 2>/dev/null || :") {
				t.Errorf("cflags script = %q", arguments)
			}
		} else if !strings.Contains(arguments, "pkg-config 'libcrypto' '--libs' 2>/dev/null || echo '-lcrypto'") {
			t.Errorf("libs script = %q", arguments)
		}
	}

	oracle, err := NewProbeResultOracleFromTrees(writePkgConfigProbeResults(t, discovery.Plan), discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.cflags != "-I__LINUX_BZL_HOST_DEPS__/openssl/include -DOPENSSL_API_COMPAT=30000 -I__LINUX_BZL_SOURCE_TREE__/scripts" ||
		replay.Value.libs != "-L__LINUX_BZL_HOST_DEPS__/openssl/lib -lcrypto" {
		t.Fatalf("pkg-config replay values = %#v", replay.Value)
	}
	for index := range replay.Plan.Nodes {
		if replay.Plan.Nodes[index].ID != discovery.Plan.Nodes[index].ID {
			t.Fatalf("replayed pkg-config node %d = %s, discovered %s", index, replay.Plan.Nodes[index].ID, discovery.Plan.Nodes[index].ID)
		}
	}
}

func TestConfiguredPkgConfigExistsProjectsSourceSelectedStatusToText(t *testing.T) {
	const command = "pkg-config --exists liboptional 2>/dev/null && echo yes"
	options := pkgConfigProbeOptions(t)
	query := func(scopes *KbuildProbeScopes) (string, error) {
		value, err := scopes.kbuildSourceShell(context.Background(), command, options.Host.SourceRoot)
		if err != nil {
			return "", err
		}
		return scopes.resolveSymbolicForScope("host", value)
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, query)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value) || len(discovery.Plan.Terminal) != 1 ||
		len(discovery.Plan.Nodes) != 1 || discovery.Plan.Nodes[0].Scope != "host" {
		t.Fatalf("source-selected package check = %q, plan = %#v; want one host text producer", discovery.Value, discovery.Plan)
	}
	request := discovery.Plan.Requests[discovery.Plan.Nodes[0].RequestID]
	if request.Outcome.Kind != "text" || !request.Outcome.GNUMakeShell || !request.Outcome.RequireSuccess ||
		!strings.Contains(strings.Join(request.Steps[0].Arguments, " "), "pkg-config '--exists' 'liboptional' 2>/dev/null && echo 'yes' || :") {
		t.Fatalf("source pkg-config status projection request = %#v", request)
	}
	for _, tc := range []struct {
		name, output string
	}{
		{name: "declared package available", output: "yes"},
		{name: "declared package absent", output: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "host")
			if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
				t.Fatal(err)
			}
			node := discovery.Plan.Nodes[0]
			stdout := ""
			if tc.output != "" {
				stdout = tc.output + "\n"
			}
			result := ProbeResult{
				Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
				Scope: node.Scope, ToolsetIdentity: discovery.Plan.Toolsets[node.Scope], Kind: "text",
				Text:  tc.output,
				Steps: []ProbeStepResult{{Name: request.Steps[0].Name, Status: "success", ExitCode: 0, Stdout: stdout}},
			}
			data, err := result.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			oracle, err := NewProbeResultOracleFromTrees(map[string]string{"host": root}, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(options, oracle, query)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != tc.output || replay.Plan.Nodes[0].ID != node.ID {
				t.Fatalf("source package check = %q node %q; want %q node %q", replay.Value, replay.Plan.Nodes[0].ID, tc.output, node.ID)
			}
		})
	}
}

func TestConfiguredPkgConfigExistsBindsManifestContentsAtSamePath(t *testing.T) {
	const command = "pkg-config --exists liboptional 2>/dev/null && echo yes"
	filename := filepath.Join(t.TempDir(), "packages.json")
	options := pkgConfigProbeOptions(t)
	var profileKeys []string
	for index, tc := range []struct {
		packages, want string
	}{
		{packages: `{}`, want: ""},
		{packages: `{"liboptional":{"cflags":[],"libs":[]}}`, want: "yes"},
	} {
		contents := `{"schema":"linux.bzl/pkg-config-manifest/v1","packages":` + tc.packages + `}`
		if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest, err := pkgconfigmanifest.Read(filename)
		if err != nil {
			t.Fatal(err)
		}
		options.Host.PkgConfigManifest = manifest
		value, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (string, error) {
			profileKeys = append(profileKeys, scopes.activeScriptEnvironmentIdentity)
			return scopes.kbuildSourceShell(context.Background(), command, options.Host.SourceRoot)
		})
		if err != nil {
			t.Fatal(err)
		}
		if value.Value != tc.want || len(value.Plan.Nodes) != 0 || len(value.Plan.Terminal) != 0 {
			t.Fatalf("manifest state %d: shell output %q plan %#v; want %q without a measured query", index, value.Value, value.Plan, tc.want)
		}
	}
	if len(profileKeys) != 2 || profileKeys[0] == profileKeys[1] {
		t.Fatalf("same-path manifest changes reused a cached source profile: %q", profileKeys)
	}
}

func TestConfiguredPkgConfigQueryRejectsAmbientOrBroaderShellAuthority(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := newFixtureProbeEvaluator(t, 0, "x86", builder, nil, false)
	host.tools[linuxProbePkgConfigRole] = "/configured/pkgconfigshim"
	host.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	host.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	hostToken := KbuildActionRoleToken("host", linuxProbePkgConfigRole)
	targetToken := KbuildActionRoleToken("target", linuxProbePkgConfigRole)
	tests := map[string]struct {
		command string
		error   string
	}{
		"ambient program":                 {command: "pkg-config --cflags libcrypto 2>/dev/null", error: "unsupported symbolic"},
		"target scope":                    {command: targetToken + " --cflags libcrypto 2>/dev/null", error: "host action scope"},
		"missing discard":                 {command: hostToken + " --cflags libcrypto", error: "stderr discard"},
		"wrong descriptor":                {command: hostToken + " --cflags libcrypto > /dev/null", error: "stderr discard"},
		"spaced descriptor":               {command: hostToken + " --cflags libcrypto 2 > /dev/null", error: "exact stderr discard"},
		"ambient output":                  {command: hostToken + " --cflags libcrypto 2> result", error: "exact stderr discard"},
		"pipeline":                        {command: hostToken + " --cflags libcrypto 2>/dev/null | cat", error: "unsupported operator"},
		"shell statement":                 {command: hostToken + " --cflags libcrypto 2>/dev/null; touch owned", error: "unsupported operator"},
		"dynamic package":                 {command: hostToken + " --cflags '$PACKAGE' 2>/dev/null", error: "dynamic"},
		"path package":                    {command: hostToken + " --cflags ../libcrypto 2>/dev/null", error: "unsupported package"},
		"missing mode":                    {command: hostToken + " libcrypto 2>/dev/null", error: "one output mode"},
		"combined modes":                  {command: hostToken + " --cflags --libs libcrypto 2>/dev/null", error: "combines output modes"},
		"success query requires exists":   {command: hostToken + " --libs libcrypto 2>/dev/null && echo yes", error: "requires --exists"},
		"success query rejects execution": {command: hostToken + " --exists liboptional 2>/dev/null && sh -c true", error: "echo LITERAL"},
		"success query rejects injection": {command: hostToken + " --exists liboptional 2>/dev/null && echo '$(touch /tmp/owned)'", error: "dynamic"},
		"success query rejects chain":     {command: hostToken + " --exists liboptional 2>/dev/null && echo yes || echo fallback", error: "repeats"},
		"command fallback":                {command: hostToken + " --libs libcrypto 2>/dev/null || sh -c true", error: "echo LITERAL"},
		"multiword fallback":              {command: hostToken + " --libs libcrypto 2>/dev/null || echo -Lone -ltwo", error: "echo LITERAL"},
		"active fallback":                 {command: hostToken + " --libs libcrypto 2>/dev/null || echo '$LIBS'", error: "dynamic"},
		"placeholder fallback":            {command: hostToken + " --libs libcrypto 2>/dev/null || echo '${tool:cc}'", error: "dynamic"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := host.KbuildShell(context.Background(), test.command)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("KbuildShell(%q) error = %v, want %q", test.command, err, test.error)
			}
			if got := len(host.References()); got != 0 {
				t.Fatalf("invalid pkg-config query emitted %d probe references", got)
			}
		})
	}
}
