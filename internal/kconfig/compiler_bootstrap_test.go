package kconfig

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type bootstrapFixture struct {
	name, scope, machine, versionText, predefines string
	result                                        ProbeResult
}

func linuxCompilerBootstrapFixtures(t *testing.T) []bootstrapFixture {
	t.Helper()
	fixtures := []bootstrapFixture{
		{
			name: "gcc", scope: "host", machine: "x86_64-linux-gnu", versionText: "gcc (GCC) 15.2.0",
			predefines: "#define VENDOR_MAJOR 15\n#define POINTER_BYTES 8\n",
		},
		{
			name: "clang", scope: "target", machine: "aarch64-linux-gnu", versionText: "clang version 22.1.0",
			predefines: "#define VENDOR_MAJOR 22\r\n#define POINTER_BYTES 8\r\n",
		},
		{
			name: "unclassified", scope: "target", machine: "powerpc64le-linux-gnu", versionText: "Acme C compiler 4.2",
			predefines: "",
		},
	}
	for index := range fixtures {
		fixture := &fixtures[index]
		request := LinuxCompilerBootstrapRequest()
		requestID, err := request.ID()
		if err != nil {
			t.Fatal(err)
		}
		node := ProbePlanNode{Scope: fixture.scope, RequestID: requestID}
		node.ID = node.ContentID()
		value := true
		fixture.result = ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: requestID,
			Scope: fixture.scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &value,
			Steps: []ProbeStepResult{
				bootstrapSuccess(bootstrapCompilerMachine, fixture.machine+"\n", ""),
				bootstrapSuccess(bootstrapCompilerVersion, fixture.versionText+"\nadditional details\n", ""),
				bootstrapSuccess(bootstrapCompilerPredefines, fixture.predefines, ""),
			},
		}
	}
	return fixtures
}

const bootstrapTestIdentity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func bootstrapSuccess(name, stdout, stderr string) ProbeStepResult {
	return ProbeStepResult{Name: name, Status: "success", ExitCode: 0, Stdout: stdout, Stderr: stderr}
}

func TestLinuxCompilerBootstrapRequestIsMinimalCanonicalPlan(t *testing.T) {
	request := LinuxCompilerBootstrapRequest()
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	second, err := request.CanonicalJSON()
	if err != nil || !slices.Equal(first, second) {
		t.Fatalf("canonical JSON is unstable: %v", err)
	}
	if got, want := request.ToolRoles(), []string{"cc"}; !slices.Equal(got, want) {
		t.Fatalf("bootstrap roles = %q, want %q", got, want)
	}
	if got, want := len(request.Steps), 3; got != want {
		t.Fatalf("bootstrap has %d steps, want %d", got, want)
	}
	if got, want := request.Steps[1].Environment, map[string]string{"LC_ALL": "C"}; !maps.Equal(got, want) {
		t.Fatalf("bootstrap compiler-version environment = %q, want %q", got, want)
	}
	if got, want := request.Steps[2], (ProbeStep{
		Name: bootstrapCompilerPredefines, Tool: "cc", Arguments: []string{"-dM", "-E", "-x", "c", "/dev/null"},
	}); got.Name != want.Name || got.Tool != want.Tool || !slices.Equal(got.Arguments, want.Arguments) || len(got.Environment) != 0 {
		t.Fatalf("bootstrap compiler-predefines step = %#v, want %#v", got, want)
	}
	stepNames := make([]string, len(request.Steps))
	for index, step := range request.Steps {
		stepNames[index] = step.Name
	}
	if want := bootstrapStepNames(); !slices.Equal(stepNames, want) {
		t.Fatalf("bootstrap steps = %q, want %q", stepNames, want)
	}
	for _, forbidden := range []string{"-###", "-print-file-name", "pahole", `"tool":"ar"`, `"tool":"ld"`, `"tool":"nm"`, `"tool":"objcopy"`} {
		if strings.Contains(string(first), forbidden) {
			t.Errorf("minimal bootstrap still contains %q: %s", forbidden, first)
		}
	}
	requestID, err := request.ID()
	if err != nil {
		t.Fatal(err)
	}
	if len(requestID) != 64 {
		t.Fatalf("bootstrap request ID = %q", requestID)
	}

	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	target, err := AddLinuxCompilerBootstrap(builder, "target")
	if err != nil {
		t.Fatal(err)
	}
	host, err := AddLinuxCompilerBootstrap(builder, "host")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := builder.Plan(target, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || len(plan.Requests) != 1 || len(plan.Nodes[0].Inputs) != 0 || len(plan.Nodes[1].Inputs) != 0 {
		t.Fatalf("bootstrap plan = %#v", plan)
	}
	root := filepath.Join(t.TempDir(), "plan")
	if err := plan.Write(root); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"requests/" + requestID + ".json",
		"nodes/" + target.NodeID + "/scope/target",
		"nodes/" + host.NodeID + "/scope/host",
		"nodes/" + target.NodeID + "/tool/cc",
		"terminal/" + target.NodeID,
		"terminal/" + host.NodeID,
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err != nil {
			t.Errorf("bootstrap plan omits %s: %v", relative, err)
		}
	}
}

func TestParseLinuxCompilerBootstrapResultFixtures(t *testing.T) {
	for _, fixture := range linuxCompilerBootstrapFixtures(t) {
		t.Run(fixture.name, func(t *testing.T) {
			facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
			if err != nil {
				t.Fatal(err)
			}
			for name, pair := range map[string][2]string{
				"scope":        {facts.Scope(), fixture.scope},
				"identity":     {facts.ToolsetIdentity(), bootstrapTestIdentity},
				"machine":      {facts.Machine(), fixture.machine},
				"version text": {facts.VersionText(), fixture.versionText},
				"predefines":   {facts.Predefines(), fixture.predefines},
			} {
				if pair[0] != pair[1] {
					t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
				}
			}
		})
	}
}

func TestParseLinuxCompilerBootstrapResultPreservesRawPredefines(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	fixture.result.Steps[2].Stderr = "a diagnostic which does not alter stdout\n"
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if got := facts.Predefines(); got != fixture.predefines {
		t.Fatalf("predefines = %q, want exact raw stream %q", got, fixture.predefines)
	}
}

func TestParseLinuxCompilerBootstrapResultRejectsMalformedData(t *testing.T) {
	base := linuxCompilerBootstrapFixtures(t)[1]
	for _, test := range []struct {
		name, want string
		mutate     func(*ProbeResult)
	}{
		{name: "wrong node", want: "node ID", mutate: func(result *ProbeResult) { result.NodeID = strings.Repeat("f", 64) }},
		{name: "wrong request", want: "request ID", mutate: func(result *ProbeResult) { result.RequestID = strings.Repeat("e", 64) }},
		{name: "false outcome", want: "outcome is not successful", mutate: func(result *ProbeResult) { value := false; result.Boolean = &value }},
		{name: "reordered", want: "step 0", mutate: func(result *ProbeResult) { result.Steps[0], result.Steps[1] = result.Steps[1], result.Steps[0] }},
		{name: "failed step", want: "did not succeed", mutate: func(result *ProbeResult) {
			result.Steps[1] = ProbeStepResult{Name: bootstrapCompilerVersion, Status: "failure", ExitCode: 1}
		}},
		{name: "machine stderr", want: "wrote to stderr", mutate: func(result *ProbeResult) { result.Steps[0].Stderr = "warning" }},
		{name: "machine whitespace", want: "invalid machine", mutate: func(result *ProbeResult) { result.Steps[0].Stdout = "not a machine\n" }},
		{name: "empty version", want: "invalid version text", mutate: func(result *ProbeResult) { result.Steps[1].Stdout = "\n" }},
		{name: "oversized predefines", want: "maximum is", mutate: func(result *ProbeResult) {
			result.Steps[2].Stdout = strings.Repeat("x", maxLinuxCompilerPredefinesBytes+1)
		}},
		{name: "NUL predefines", want: "invalid predefines text", mutate: func(result *ProbeResult) {
			result.Steps[2].Stdout = "#define VALUE 1\x00\n"
		}},
		{name: "non-UTF-8 predefines", want: "invalid predefines text", mutate: func(result *ProbeResult) {
			result.Steps[2].Stdout = string([]byte{0xff})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := base.result
			result.Steps = append([]ProbeStepResult(nil), base.result.Steps...)
			test.mutate(&result)
			if _, err := ParseLinuxCompilerBootstrapResult(result, base.scope, bootstrapTestIdentity); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseLinuxCompilerBootstrapResult() error = %v, want %q", err, test.want)
			}
		})
	}
}
