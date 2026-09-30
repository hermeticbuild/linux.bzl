package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const featureChildMakefileFixture = `
__BUILD = $(CC) $(CFLAGS) -MD -Wall -Werror -o $@ $(patsubst %.bin,%.c,$(@F)) $(LDFLAGS)
  BUILD = $(__BUILD) > $(@:.bin=.make.output) 2>&1

$(OUTPUT)test-other.bin:
	$(BUILD) -lother

$(OUTPUT)test-hello.bin:
	$(BUILD) -O2 -lselected

-include $(OUTPUT)*.d
`

func featureChildFixture(t *testing.T, makefile, source string) (KbuildProbeWorkloadOptions, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		"Kconfig":                          "mainmenu \"feature probe\"\n",
		"tools/build/feature/Makefile":     makefile,
		"tools/build/feature/test-hello.c": source,
		"tools/build/feature/checked.h":    "#define checked() 0\n",
	} {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	host.SourceRoot = root
	target := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[1])
	command := CompactKbuildRecursiveMakeProvenanceToken +
		` OUTPUT=__LINUX_BZL_OBJECT_TREE__/private/feature/` +
		` CC="` + KbuildActionRoleToken("host", "cc") + `"` +
		` CFLAGS=" -I." LDFLAGS=" "` +
		` -C __LINUX_BZL_SOURCE_TREE__/tools/build/feature` +
		` __LINUX_BZL_OBJECT_TREE__/private/feature/test-hello.bin` +
		` >/dev/null 2>/dev/null && echo 1 || echo 0`
	return KbuildProbeWorkloadOptions{Target: target, Host: &host}, command
}

func TestRecursiveMakeFeatureProbeSelectsChildSourceRecipeAndPrivateOutput(t *testing.T) {
	options, command := featureChildFixture(t, featureChildMakefileFixture,
		"#include \"checked.h\"\nint main(void) { return checked(); }\n")
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (string, error) {
		return scopes.kbuildShell(context.Background(), "target", command, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value) || len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("child feature discovery = %q, nodes = %#v; want one symbolic compiler link", discovery.Value, discovery.Plan.Nodes)
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	wantSources := []string{"Kconfig", "tools/build/feature/checked.h", "tools/build/feature/test-hello.c"}
	if node.Scope != "host" || !slices.Equal(request.Sources, wantSources) ||
		len(request.Steps) != 1 || request.Steps[0].Tool != "cc" ||
		request.Steps[0].WorkingDirectory != "${source_root:linux}/tools/build/feature" ||
		request.Outcome.Kind != "boolean" || request.Outcome.Predicate.Operator != "exit-zero" {
		t.Fatalf("selected child compiler request = %#v, node = %#v", request, node)
	}
	step := request.Steps[0]
	wantArguments := []string{
		"-I.", "-MD", "-Wall", "-Werror", "-O2", "-lselected",
		"-o", "${scratch:output}", "${source:tools/build/feature/test-hello.c}",
	}
	if !slices.Equal(step.Arguments, wantArguments) || !step.DiscardStdout || !step.DiscardStderr {
		t.Fatalf("selected feature compiler argv = %#v; want %#v", step.Arguments, wantArguments)
	}
	if step.Candidate == nil || !slices.Equal(step.Candidate.Base, []int{0, 2, 3, 4, 5}) {
		t.Fatalf("selected compiler ownership = %#v; want source -MD managed and flags candidate-owned", step.Candidate)
	}
	for _, success := range []bool{true, false} {
		status, exitCode := "failure", 1
		if success {
			status, exitCode = "success", 0
		}
		oracle := &ProbeResultOracle{
			toolsets: discovery.Plan.Toolsets,
			results: map[string]ProbeResult{node.ID: {
				Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
				Scope: node.Scope, ToolsetIdentity: discovery.Plan.Toolsets[node.Scope],
				Kind: "boolean", Boolean: &success,
				Steps: []ProbeStepResult{{Name: step.Name, Status: status, ExitCode: exitCode}},
			}},
		}
		replay, err := EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (string, error) {
			value, err := scopes.kbuildShell(context.Background(), "target", command, nil)
			if err != nil {
				return "", err
			}
			return scopes.resolveSymbolicForScope("host", value)
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "0"
		if success {
			want = "1"
		}
		if replay.Value != want {
			t.Fatalf("child compiler success=%t produced %q, want %q", success, replay.Value, want)
		}
	}
}

func TestRecursiveMakeFeatureProbeSplitsSealedSourceShellFlags(t *testing.T) {
	makefile := strings.Replace(featureChildMakefileFixture, "-O2 -lselected",
		"$(shell perl -MExtUtils::Embed -e ccopts 2>/dev/null) -O2 -lselected", 1)
	options, command := featureChildFixture(t, makefile, "int main(void) { return 0; }\n")
	options.Host.Tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	options.Host.Tools[linuxProbeScriptRuntime] = "/configured/multicall"
	options.Host.Tools[compactKbuildScriptAppletRolePrefix+"perl"] = "/configured/perl"
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (string, error) {
		return scopes.kbuildShell(context.Background(), "target", command, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("selected Perl text and compiler link need two ordered probes: %#v", discovery.Plan.Nodes)
	}
	var textNode, compilerNode *ProbePlanNode
	for index := range discovery.Plan.Nodes {
		node := &discovery.Plan.Nodes[index]
		request := discovery.Plan.Requests[node.RequestID]
		switch request.Steps[0].Tool {
		case linuxProbeScriptRunner:
			textNode = node
		case "cc":
			compilerNode = node
		}
	}
	if textNode == nil || compilerNode == nil || textNode.Scope != "host" || compilerNode.Scope != "host" ||
		!slices.Equal(compilerNode.Inputs, []string{textNode.ID}) {
		t.Fatalf("selected feature Perl query is not the compiler's exact predecessor: %#v", discovery.Plan.Nodes)
	}
	request := discovery.Plan.Requests[compilerNode.RequestID]
	step := request.Steps[0]
	if request.InputCount != 1 || len(step.ArgumentFragments) != 1 ||
		step.ArgumentFragments[0].Mode != ProbeArgumentFragmentsModeSourceShellWords ||
		!slices.Contains(step.Arguments, "-O2") || !slices.Contains(step.Arguments, "-lselected") {
		t.Fatalf("selected feature compiler lost measured Perl shell-word ownership: %#v", request)
	}
}

func TestRecursiveMakeFeatureProbeRejectsUnboundSourcesAndShellOperations(t *testing.T) {
	for _, test := range []struct {
		name     string
		makefile string
		source   string
		command  func(string) string
		want     string
	}{
		{
			name: "missing quoted include", makefile: featureChildMakefileFixture,
			source: "#include \"not-declared.h\"\nint main(void) { return 0; }\n",
			want:   "feature source include",
		},
		{
			name: "unbound absolute include", makefile: strings.Replace(featureChildMakefileFixture,
				"-O2 -lselected", "-I/usr/include/slang -O2 -lselected", 1),
			source: "int main(void) { return 0; }\n",
			want:   "outside declared source",
		},
		{
			name: "missing child goal", makefile: featureChildMakefileFixture,
			source: "int main(void) { return 0; }\n",
			command: func(command string) string {
				return strings.Replace(command, "test-hello.bin", "test-unknown.bin", 1)
			},
			want: "no selected source recipe",
		},
		{
			name: "extra shell command", makefile: featureChildMakefileFixture,
			source: "int main(void) { return 0; }\n",
			command: func(command string) string {
				return strings.Replace(command, "&& echo 1", "; echo injected && echo 1", 1)
			},
			want: "child Make",
		},
		{
			name: "Make interpreter assignment", makefile: featureChildMakefileFixture,
			source: "int main(void) { return 0; }\n",
			command: func(command string) string {
				return strings.Replace(command, " CC=", " MAKEFLAGS=--no-builtin-rules CC=", 1)
			},
			want: "interpreter-control assignment",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, command := featureChildFixture(t, test.makefile, test.source)
			if test.command != nil {
				command = test.command(command)
			}
			_, err := EvaluateKbuildProbeWorkload(options, nil, func(scopes *KbuildProbeScopes) (string, error) {
				return scopes.kbuildShell(context.Background(), "target", command, nil)
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("selected child error = %v, want %q without a guessed false feature", err, test.want)
			}
		})
	}
}
