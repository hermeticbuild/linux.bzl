package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

var testConfiguredKbuildActionRoles = []string{
	"ar", "as", "cc", "cxx", "ld", "nm", "objcopy", "objdump", "ranlib", "readelf", "strip",
}

func TestReadConfiguredKbuildToolsetManifestRejectsInvalidActionRole(t *testing.T) {
	manifest := configuredKbuildToolsetManifest{
		Schema: configuredKbuildToolsetSchema,
		Scope:  "target",
		Actions: map[string][]string{
			"CC": {"tool", configuredKbuildArgsSentinel},
		},
		Tools:         map[string]string{"CC": "tool"},
		Closure:       []string{"tool"},
		ArtifactKinds: map[string]string{"tool": toolaction.KbuildToolsetArtifactSource},
		Environments:  map[string]map[string]string{"CC": {}},
		MakeVariables: map[string]string{"CC": "CC"},
		Requirements:  map[string]map[string]string{"CC": {}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readConfiguredKbuildToolsetManifest(filename, "target", "sha256-ignored")
	if err == nil || !strings.Contains(err.Error(), "invalid action role") {
		t.Fatalf("invalid manifest role error=%v", err)
	}
}

func testConfiguredRustContracts() (*hostKbuildContract, *hostKbuildContract) {
	return &hostKbuildContract{
			Actions: map[string]configuredKbuildAction{
				"bindgen": {Path: "configured-bindgen"},
				"cc":      {Path: "configured-target-cc"},
				"rustc":   {Path: "configured-target-rustc"},
			},
			MakeVariables: map[string]string{
				"BINDGEN":         "bindgen",
				"CC":              "cc",
				"RUSTC":           "rustc",
				"RUSTC_OR_CLIPPY": "rustc",
			},
		}, &hostKbuildContract{
			Actions: map[string]configuredKbuildAction{
				"cc":    {Path: "configured-host-cc"},
				"rustc": {Path: "configured-host-rustc"},
			},
			MakeVariables: map[string]string{
				"HOSTCC":    "cc",
				"HOSTRUSTC": "rustc",
			},
		}
}

func TestConfiguredRustSourceRootComesOnlyFromGenericToolsets(t *testing.T) {
	target, host := testConfiguredRustContracts()
	root := "external/rust-src/library"
	got, err := configuredRustSourceRoot(
		target,
		host,
		map[string]string{"RUST_LIB_SRC": root},
		map[string]string{root: workspacePath(root)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("Rust source root = %q, want %q", got, root)
	}
	if _, exists := target.MakeVariables["RUSTC_VERSION_TEXT"]; exists {
		t.Fatal("generic target manifest unexpectedly carries a precomputed rustc version")
	}
}

func TestConfiguredRustSourceRootLeavesPartialToolAvailabilityToKconfig(t *testing.T) {
	root := "external/rust-src/library"
	for name, mutate := range map[string]func(*hostKbuildContract, *hostKbuildContract){
		"missing bindgen role":            func(target, _ *hostKbuildContract) { delete(target.Actions, "bindgen") },
		"missing host rustc role":         func(_, host *hostKbuildContract) { delete(host.Actions, "rustc") },
		"missing source variable binding": func(target, _ *hostKbuildContract) { delete(target.MakeVariables, "RUSTC_OR_CLIPPY") },
	} {
		t.Run(name, func(t *testing.T) {
			target, host := testConfiguredRustContracts()
			mutate(target, host)
			got, err := configuredRustSourceRoot(
				target, host,
				map[string]string{"RUST_LIB_SRC": root},
				map[string]string{root: workspacePath(root)},
			)
			if err != nil {
				t.Fatal(err)
			}
			if got != root {
				t.Fatalf("Rust source root = %q, want Kconfig-visible %q", got, root)
			}
		})
	}
}

func TestConfiguredRustSourceRootRejectsInvalidOrUnmappedRoots(t *testing.T) {
	for name, test := range map[string]struct {
		root        string
		sourceRoots map[string]string
	}{
		"unmapped": {root: "external/rust-src/library", sourceRoots: map[string]string{}},
		"absolute": {root: "/external/rust-src/library", sourceRoots: map[string]string{}},
		"parent":   {root: "../rust-src/library", sourceRoots: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configuredRustSourceRoot(nil, nil, map[string]string{"RUST_LIB_SRC": test.root}, test.sourceRoots); err == nil {
				t.Fatal("invalid Rust source root succeeded")
			}
		})
	}
}

func TestConfiguredRustSourceRootAllowsToolsetsWithoutRust(t *testing.T) {
	target := &hostKbuildContract{Actions: map[string]configuredKbuildAction{"cc": {Path: "target-cc"}}, MakeVariables: map[string]string{"CC": "cc"}}
	host := &hostKbuildContract{Actions: map[string]configuredKbuildAction{"cc": {Path: "host-cc"}}, MakeVariables: map[string]string{"HOSTCC": "cc"}}
	got, err := configuredRustSourceRoot(target, host, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("Rust source root = %q, want unavailable", got)
	}
}

func TestHermeticKbuildDirectoryQueriesUseDeclaredObjectTree(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "tools", "objtool")
	for command, want := range map[string]string{
		"cd ; test -d " + root + " || echo " + root:                 "",
		"cd " + root + "; test -d " + output + " || echo " + output: "",
		"cd " + root + "; cd " + output + " ; pwd":                  kbuildEvalObjectTree + "/tools/objtool",
		"cd " + output + " && pwd":                                  kbuildEvalObjectTree + "/tools/objtool",
		"cd " + kbuildEvalSourceTree + "/tools; test -d " + kbuildEvalObjectTree + " || echo " + kbuildEvalObjectTree: "",
		"cd " + kbuildEvalSourceTree + "/tools && pwd":                                                                kbuildEvalObjectTree + "/tools",
	} {
		got, handled, err := evaluateHermeticKbuildDirectoryQuery(command, root)
		if err != nil {
			t.Fatalf("query %q: %v", command, err)
		}
		if !handled || got != want {
			t.Fatalf("query %q = (%q, %t), want (%q, true)", command, got, handled, want)
		}
	}
}

func TestHermeticKbuildHostConfigFallbackDoesNotReadExecutionHost(t *testing.T) {
	got, err := hermeticLinuxKbuildShell("uname -r", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != hermeticKbuildHostKernelRelease {
		t.Fatalf("uname -r = %q, want stable unavailable release %q", got, hermeticKbuildHostKernelRelease)
	}
}

func TestHermeticKbuildShellDoesNotHardcodeHostGetconfResults(t *testing.T) {
	for _, command := range []string{
		"getconf LFS_CFLAGS",
		"getconf LFS_LDFLAGS 2>/dev/null",
		"getconf LFS_VENDOR_EXTENSION 2>/dev/null",
	} {
		if value, err := hermeticLinuxKbuildShell(command, t.TempDir(), nil, nil); err == nil {
			t.Fatalf("hermetic fallback %q = %q, want source-derived host probe", command, value)
		} else if !strings.Contains(err.Error(), "unsupported hermetic Kbuild shell command") {
			t.Fatalf("hermetic fallback %q error = %v", command, err)
		}
	}
}

func TestHermeticKbuildBuildVersionAcceptsSourceTreeSentinel(t *testing.T) {
	got, err := hermeticLinuxKbuildShell(kbuildEvalSourceTree+"/scripts/build-version", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != hermeticKbuildBuildVersion {
		t.Fatalf("source-tree build version = %q, want %q", got, hermeticKbuildBuildVersion)
	}
}

func TestReadToolsetIdentity(t *testing.T) {
	root := t.TempDir()
	want := "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(root, want), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readToolsetIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
}

func TestReadToolsetIdentityRejectsMalformedTrees(t *testing.T) {
	for name, populate := range map[string]func(string){
		"empty": func(string) {},
		"bad name": func(root string) {
			if err := os.WriteFile(filepath.Join(root, "identity"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"nonempty": func(root string) {
			if err := os.WriteFile(filepath.Join(root, "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			populate(root)
			if _, err := readToolsetIdentity(root); err == nil {
				t.Fatal("readToolsetIdentity succeeded")
			}
		})
	}
}

func writeTestToolsetIdentity(t *testing.T, identity string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, identity), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func testLinuxCompilerBootstrapResult(t *testing.T, reference kconfig.ProbeReference, identity string, clang bool) kconfig.ProbeResult {
	t.Helper()
	if clang {
		return testLinuxCompilerBootstrapResultWithVersion(t, reference, identity, "aarch64-linux-gnu", "clang version 22.1.0")
	}
	return testLinuxCompilerBootstrapResultWithVersion(t, reference, identity, "x86_64-linux-gnu", "gcc (GCC) 15.2.0")
}

func testLinuxCompilerBootstrapResultWithVersion(t *testing.T, reference kconfig.ProbeReference, identity, machine, versionText string) kconfig.ProbeResult {
	t.Helper()
	request := kconfig.LinuxCompilerBootstrapRequest()
	steps := make([]kconfig.ProbeStepResult, 0, len(request.Steps))
	for _, step := range request.Steps {
		result := kconfig.ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
		switch step.Name {
		case "compiler-machine":
			result.Stdout = machine + "\n"
		case "compiler-version":
			result.Stdout = versionText + "\nadditional fixture details\n"
		case "compiler-predefines":
			result.Stdout = "#define __linux__ 1\n#define __SIZEOF_POINTER__ 8\n"
		default:
			t.Fatalf("unexpected compiler bootstrap step %q", step.Name)
		}
		steps = append(steps, result)
	}
	value := true
	return kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
		Scope: reference.Scope, ToolsetIdentity: identity, Kind: reference.Kind, Boolean: &value,
		Steps: steps,
	}
}

func writeTestLinuxCompilerMakefiles(t *testing.T, root string) {
	t.Helper()
	directory := filepath.Join(root, "scripts")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Makefile.clang"), []byte(`
CLANG_TARGET_FLAGS_arm64 := aarch64-linux-gnu
CLANG_FLAGS := --target=$(CLANG_TARGET_FLAGS_$(SRCARCH))
ifeq ($(LLVM_IAS),0)
CLANG_FLAGS += -fno-integrated-as
else
CLANG_FLAGS += -fintegrated-as
endif
export CLANG_FLAGS
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
config-build :=
ifneq ($(filter %config,$(MAKECMDGOALS)),)
config-build := 1
endif
COMPILER_MACHINE := $(shell $(CC) -dumpmachine)
SUBARCH := $(word 1,$(subst -, ,$(COMPILER_MACHINE)))
ifeq ($(SUBARCH),aarch64)
SUBARCH := arm64
endif
ifeq ($(SUBARCH),x86_64)
SUBARCH := x86
endif
ARCH ?= $(SUBARCH)
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
CC_VERSION_TEXT = $(shell LC_ALL=C $(CC) --version 2>/dev/null | head -n 1)
CLANG_FLAGS :=
ifneq ($(findstring clang,$(CC_VERSION_TEXT)),)
include $(srctree)/scripts/Makefile.clang
endif
export AR BINDGEN CC LD NM OBJCOPY PAHOLE PYTHON3 RUSTC
ifdef config-build
export CC_VERSION_TEXT
endif
`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testKbuildContracts(actions map[string]configuredKbuildAction, facts *kconfig.LinuxCompilerFacts) (*hostKbuildContract, *hostKbuildContract) {
	targetVariables := map[string]string{}
	hostVariables := map[string]string{}
	for role := range actions {
		targetVariables[strings.ToUpper(role)] = role
		hostVariables["HOST"+strings.ToUpper(role)] = role
	}
	return &hostKbuildContract{
			Actions: actions, MakeVariables: targetVariables, CompilerMachine: facts.Machine(),
		}, &hostKbuildContract{
			Actions: actions, MakeVariables: hostVariables, CompilerMachine: facts.Machine(),
		}
}

func testSourceDerivedLinuxKconfigIdentity(t *testing.T, root, machine string) sourceDerivedLinuxTarget {
	t.Helper()
	const targetIdentity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const hostIdentity = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResultWithVersion(t, bootstrap.target, targetIdentity, machine, "Acme C compiler 1.0"),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{"cc": {Path: filepath.Join(root, "configured-cc")}}
	target, host := testKbuildContracts(actions, facts)
	builder, err := kconfig.NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapArchitecture, err := kconfig.LinuxCompilerMachineArchitecture(machine)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := kconfig.NewLinuxProbeEvaluator(kconfig.LinuxProbeEvaluatorOptions{
		Scope:              "target",
		Architecture:       bootstrapArchitecture,
		SourceArchitecture: bootstrapArchitecture,
		SourceRoot:         root,
		Facts:              facts,
		Tools:              map[string]string{"cc": actions["cc"].Path},
		Discovery:          builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := sourceDerivedLinuxKconfigIdentity(t.Context(), root, nil, target, host, evaluator)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func writeTestProbeResult(t *testing.T, root string, result kconfig.ProbeResult) {
	t.Helper()
	data, err := result.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "results")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, result.NodeID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWriteLinuxCompilerProbePlanUsesOnlyToolsetIdentities(t *testing.T) {
	targetIdentity := "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hostIdentity := "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	targetRoot := writeTestToolsetIdentity(t, targetIdentity)
	hostRoot := writeTestToolsetIdentity(t, hostIdentity)
	output := filepath.Join(t.TempDir(), "plan")
	if err := writeLinuxCompilerProbePlan(output, targetRoot, hostRoot); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"toolsets/target/" + targetIdentity,
		"toolsets/host/" + hostIdentity,
		"nodes/" + bootstrap.target.NodeID + "/tool/cc",
		"nodes/" + bootstrap.host.NodeID + "/tool/cc",
		"terminal/" + bootstrap.target.NodeID,
		"terminal/" + bootstrap.host.NodeID,
	} {
		if _, err := os.Stat(filepath.Join(output, filepath.FromSlash(relative))); err != nil {
			t.Errorf("probe plan omits %s: %v", relative, err)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "nodes", bootstrap.target.NodeID, "tool", "pahole")); !os.IsNotExist(err) {
		t.Fatalf("minimal bootstrap unexpectedly contains pahole marker: %v", err)
	}
}

func TestRunProbePlanDoesNotInspectConfiguredToolsOrSourceTree(t *testing.T) {
	targetIdentity := "sha256-1111111111111111111111111111111111111111111111111111111111111111"
	hostIdentity := "sha256-2222222222222222222222222222222222222222222222222222222222222222"
	output := filepath.Join(t.TempDir(), "plan")
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	}()
	flag.CommandLine = flag.NewFlagSet("kconfig_parse", flag.PanicOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{
		"kconfig_parse",
		"-probe_plan_out=" + output,
		"-target_toolset_identity=" + writeTestToolsetIdentity(t, targetIdentity),
		"-host_toolset_identity=" + writeTestToolsetIdentity(t, hostIdentity),
		"-srctree=" + filepath.Join(t.TempDir(), "does-not-exist"),
	}
	if code := run(); code != 0 {
		t.Fatalf("run() = %d, want plan-only success without inspecting tools or source tree", code)
	}
	if _, err := os.Stat(filepath.Join(output, "schema", kconfig.LinuxProbePlanSchema)); err != nil {
		t.Fatalf("plan-only invocation did not write probe plan: %v", err)
	}
}

func TestRunRejectsMutuallyExclusivePlannerOutputs(t *testing.T) {
	outputs := []string{
		"-probe_plan_out",
		"-kconfig_probe_plan_out",
		"-kbuild_probe_plan_out",
		"-action_plan_stage_out=target",
	}
	for first := 0; first < len(outputs); first++ {
		for second := first + 1; second < len(outputs); second++ {
			name := strings.TrimPrefix(outputs[first], "-") + " with " + strings.TrimPrefix(outputs[second], "-")
			t.Run(name, func(t *testing.T) {
				stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
				if err != nil {
					t.Fatal(err)
				}
				oldCommandLine, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
				code := func() int {
					defer func() {
						flag.CommandLine = oldCommandLine
						os.Args = oldArgs
						os.Stderr = oldStderr
					}()
					flag.CommandLine = flag.NewFlagSet("kconfig_parse", flag.PanicOnError)
					flag.CommandLine.SetOutput(io.Discard)
					os.Args = []string{
						"kconfig_parse",
						outputs[first] + "=" + filepath.Join(t.TempDir(), "first"),
						outputs[second] + "=" + filepath.Join(t.TempDir(), "second"),
						"-kbuild_var=M=external/module",
					}
					os.Stderr = stderrFile
					return run()
				}()
				if err := stderrFile.Close(); err != nil {
					t.Fatal(err)
				}
				stderr, err := os.ReadFile(stderrFile.Name())
				if err != nil {
					t.Fatal(err)
				}
				if code != 2 {
					t.Errorf("run() = %d, want usage error 2", code)
				}
				if got := string(stderr); !strings.Contains(got, "are mutually exclusive planner outputs") {
					t.Errorf("stderr = %q, want mutually-exclusive planner-output diagnostic", got)
				}
			})
		}
	}
}

func TestRunValidatesActionPlanStageOutputsBeforePlanning(t *testing.T) {
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldCommandLine, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
	code := func() int {
		defer func() {
			flag.CommandLine = oldCommandLine
			os.Args = oldArgs
			os.Stderr = oldStderr
		}()
		flag.CommandLine = flag.NewFlagSet("kconfig_parse", flag.PanicOnError)
		flag.CommandLine.SetOutput(io.Discard)
		os.Args = []string{
			"kconfig_parse",
			"-action_plan_stage_out=target=" + filepath.Join(t.TempDir(), "target"),
		}
		os.Stderr = stderrFile
		return run()
	}()
	if err := stderrFile.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(stderrFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("run() = %d, want usage error 2", code)
	}
	if got := string(stderr); !strings.Contains(got, `missing action-plan stage "prehost"`) {
		t.Errorf("stderr = %q, want early staged-output validation diagnostic", got)
	}
}

func TestActionPlanStageOutputMapRequiresEveryStageExactlyOnce(t *testing.T) {
	root := t.TempDir()
	values := []namedPath{}
	for _, stage := range []string{"prehost", "bootstrap", "host", "prep", "target"} {
		values = append(values, namedPath{Name: stage, Path: filepath.Join(root, stage)})
	}
	outputs, err := actionPlanStageOutputMap(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if got := outputs[value.Name]; got != value.Path {
			t.Errorf("output %s = %q, want %q", value.Name, got, value.Path)
		}
	}

	tests := []struct {
		name   string
		values []namedPath
		want   string
	}{
		{name: "missing", values: values[:len(values)-1], want: `missing action-plan stage "target"`},
		{name: "duplicate", values: append(append([]namedPath{}, values...), values[0]), want: `duplicate action-plan stage "prehost"`},
		{name: "unknown", values: append(append([]namedPath{}, values...), namedPath{Name: "later", Path: filepath.Join(root, "later")}), want: `unknown action-plan stage "later"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := actionPlanStageOutputMap(test.values)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("actionPlanStageOutputMap() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunRejectsKconfigEvaluationWithoutStagedProbes(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "root only",
			args: []string{
				"-root=" + filepath.Join(t.TempDir(), "Kconfig"),
			},
		},
		{
			name: "resolved config",
			args: []string{
				"-root=" + filepath.Join(t.TempDir(), "Kconfig"),
				"-resolve_config=" + filepath.Join(t.TempDir(), ".config"),
				"-resolved_config_out=" + filepath.Join(t.TempDir(), "resolved.config"),
				"-kernel_version=6.18.39",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			oldCommandLine, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
			code := func() int {
				defer func() {
					flag.CommandLine = oldCommandLine
					os.Args = oldArgs
					os.Stderr = oldStderr
				}()
				flag.CommandLine = flag.NewFlagSet("kconfig_parse", flag.PanicOnError)
				flag.CommandLine.SetOutput(io.Discard)
				os.Args = append([]string{"kconfig_parse"}, test.args...)
				os.Stderr = stderrFile
				return run()
			}()
			if err := stderrFile.Close(); err != nil {
				t.Fatal(err)
			}
			stderr, err := os.ReadFile(stderrFile.Name())
			if err != nil {
				t.Fatal(err)
			}
			if code != 2 {
				t.Errorf("run() = %d, want usage error 2", code)
			}
			const diagnostic = "Kconfig evaluation requires staged probe discovery or replay via -kconfig_probe_plan_out or target/host Kconfig probe results"
			if got := string(stderr); !strings.Contains(got, diagnostic) {
				t.Errorf("stderr = %q, want staged-probe diagnostic %q", got, diagnostic)
			}
		})
	}
}

func TestLoadLinuxCompilerBootstrapResultsRequiresEveryDemandedResult(t *testing.T) {
	targetIdentity := "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hostIdentity := "sha256-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot, hostRoot := t.TempDir(), t.TempDir()
	targetResult := testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, true)
	hostResult := testLinuxCompilerBootstrapResult(t, bootstrap.host, hostIdentity, false)
	writeTestProbeResult(t, targetRoot, targetResult)
	if _, err := loadLinuxCompilerBootstrapResults(targetRoot, hostRoot, targetIdentity, hostIdentity); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("missing host result error = %v, want missing-demand failure", err)
	}
	writeTestProbeResult(t, hostRoot, hostResult)
	facts, err := loadLinuxCompilerBootstrapResults(targetRoot, hostRoot, targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"target identity": {facts.target.ToolsetIdentity(), targetIdentity},
		"target machine":  {facts.target.Machine(), "aarch64-linux-gnu"},
		"target version":  {facts.target.VersionText(), "clang version 22.1.0"},
		"host identity":   {facts.host.ToolsetIdentity(), hostIdentity},
		"host machine":    {facts.host.Machine(), "x86_64-linux-gnu"},
		"host version":    {facts.host.VersionText(), "gcc (GCC) 15.2.0"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	tool := filepath.Join(t.TempDir(), "configured-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1\" = -print-file-name=include ]; then echo /configured/include; exit 0; fi\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]configuredKbuildAction, len(testConfiguredKbuildActionRoles))
	for _, role := range testConfiguredKbuildActionRoles {
		actions[role] = configuredKbuildAction{Path: tool}
	}
	if _, err := configuredKbuildContract("target", actions, nil); err == nil || !strings.Contains(err.Error(), "require replayed compiler facts") {
		t.Fatalf("configured contract without bootstrap facts error = %v, want staged-facts requirement", err)
	}
	contract, err := configuredKbuildContract("target", actions, facts.target)
	if err != nil {
		t.Fatalf("facts-backed configured contract reran compiler identity probes: %v", err)
	}
	if got, want := contract.CompilerMachine, facts.target.Machine(); got != want {
		t.Errorf("contract machine = %q, want bootstrap fact %q", got, want)
	}
	hostContract, err := configuredKbuildContract("host", actions, facts.host)
	if err != nil {
		t.Fatal(err)
	}
	contract.MakeVariables = map[string]string{}
	hostContract.MakeVariables = map[string]string{}
	for _, role := range testConfiguredKbuildActionRoles {
		variable := strings.ToUpper(role)
		contract.MakeVariables[variable] = role
		hostContract.MakeVariables["HOST"+variable] = role
	}
	commandLine, err := kbuildCommandLineVariables(contract, hostContract, map[string]string{
		"CROSS_COMPILE": "aarch64-linux-gnu-",
		"LIBELF_FLAGS":  "-Iconfigured/libelf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := commandLine["LIBELF_FLAGS"], "-Iconfigured/libelf"; got != want {
		t.Errorf("configured Kbuild variable = %q, want command-line override %q", got, want)
	}
	for _, variable := range []string{
		"AR", "AS", "CC", "CXX", "LD", "NM", "OBJCOPY", "OBJDUMP", "RANLIB", "READELF", "STRIP",
	} {
		role := strings.ToLower(variable)
		if got, want := commandLine[variable], kbuildActionRoleMakeCommand("target", role); got != want {
			t.Errorf("Kbuild %s = %q, want source-time action role %q", variable, got, want)
		}
		if got, want := commandLine["HOST"+variable], kbuildActionRoleMakeCommand("host", role); got != want {
			t.Errorf("Kbuild HOST%s = %q, want source-time host action role %q", variable, got, want)
		}
	}
	if _, configured := commandLine["CPP"]; configured {
		t.Errorf("Kbuild adapter overrides source-owned CPP=%q", commandLine["CPP"])
	}
	if _, configured := commandLine["HOSTCPP"]; configured {
		t.Errorf("Kbuild adapter overrides source-owned HOSTCPP=%q", commandLine["HOSTCPP"])
	}
	for _, name := range []string{"CC_VERSION_TEXT", "LLVM", "LLVM_IAS"} {
		if value, ok := commandLine[name]; ok {
			t.Errorf("Kbuild adapter unexpectedly injected %s=%q", name, value)
		}
	}
	extra := hostResult
	extra.NodeID = strings.Repeat("e", 64)
	writeTestProbeResult(t, hostRoot, extra)
	if _, err := loadLinuxCompilerBootstrapResults(targetRoot, hostRoot, targetIdentity, hostIdentity); err != nil {
		t.Fatalf("discovery-only extra host result should be inert: %v", err)
	}
}

func TestSourceDerivedLinuxKconfigIdentityReadsArchitectureMappingOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "subarch.include"), []byte("SUBARCH := $(shell uname -m | sed -e s/x86_64/x86/)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
include $(srctree)/scripts/subarch.include
ARCH ?= $(SUBARCH)
UTS_MACHINE := $(ARCH)
SRCARCH := $(ARCH)
ifeq ($(ARCH),x86_64)
SRCARCH := x86
endif
KCONFIG_CONFIG ?= .config
HOST_LFS_CFLAGS := $(shell false)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := testSourceDerivedLinuxKconfigIdentity(t, root, "x86_64-linux-gnu")
	if want := (sourceDerivedLinuxTarget{Machine: "x86_64-linux-gnu", Arch: "x86", Srcarch: "x86", UTSMachine: "x86"}); identity != want {
		t.Fatalf("identity = %#v, want %#v", identity, want)
	}
}

func TestKbuildCommandLineVariablesRejectsCrossScopeMakeVariableCollision(t *testing.T) {
	_, err := kbuildCommandLineVariables(
		&hostKbuildContract{MakeVariables: map[string]string{"CC": "cc"}},
		&hostKbuildContract{MakeVariables: map[string]string{"CC": "cxx"}},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "bind Make variable CC to different roles") {
		t.Fatalf("cross-scope Make variable collision error = %v", err)
	}
}

func TestKbuildCommandLineVariablesShareScopeNeutralUtilityRole(t *testing.T) {
	commandLine, err := kbuildCommandLineVariables(
		&hostKbuildContract{MakeVariables: map[string]string{"AWK": "awk", "CC": "cc"}},
		&hostKbuildContract{MakeVariables: map[string]string{"AWK": "awk", "HOSTCC": "cc"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := commandLine["AWK"], kconfig.KbuildActionRoleToken(kconfig.KbuildActionRoleAutoScope, "awk"); got != want {
		t.Fatalf("shared AWK binding = %q, want scope-neutral token %q", got, want)
	}
	if got, want := commandLine["CC"], kconfig.KbuildActionRoleToken("target", "cc"); got != want {
		t.Fatalf("target-only CC binding = %q, want %q", got, want)
	}
	if got, want := commandLine["HOSTCC"], kconfig.KbuildActionRoleToken("host", "cc"); got != want {
		t.Fatalf("host-only HOSTCC binding = %q, want %q", got, want)
	}
}

func TestSourceDerivedLinuxKconfigEnvironmentOwnsCppMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
ARCH := x86
SRCARCH := x86
CPP = $(CC) -E
export CC CPP
`), 0o644); err != nil {
		t.Fatal(err)
	}

	const targetIdentity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const hostIdentity = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResultWithVersion(t, bootstrap.target, targetIdentity, "x86_64-linux-gnu", "Acme C compiler 1.0"),
		"target",
		targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := kconfig.NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetCC := filepath.Join(root, "configured-target-cc")
	evaluator, err := kconfig.NewLinuxProbeEvaluator(kconfig.LinuxProbeEvaluatorOptions{
		Scope:              "target",
		Architecture:       "x86",
		SourceArchitecture: "x86",
		SourceRoot:         root,
		Facts:              facts,
		Tools:              map[string]string{"cc": targetCC},
		Discovery:          builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := &hostKbuildContract{
		Actions:       map[string]configuredKbuildAction{"cc": {Path: targetCC}},
		MakeVariables: map[string]string{"CC": "cc"},
	}
	host := &hostKbuildContract{
		Actions:       map[string]configuredKbuildAction{"cc": {Path: filepath.Join(root, "configured-host-cc")}},
		MakeVariables: map[string]string{"HOSTCC": "cc"},
	}
	environment, err := sourceDerivedLinuxKconfigEnvironment(
		t.Context(), root, "x86", nil, nil, target, host, evaluator,
	)
	if err != nil {
		t.Fatal(err)
	}
	cc := kconfig.KbuildActionRoleToken("target", "cc")
	if got := environment["CC"]; got != cc {
		t.Fatalf("source-exported CC = %q, want selected role %q", got, cc)
	}
	if got, want := environment["CPP"], cc+" -E"; got != want {
		t.Fatalf("source-exported CPP = %q, want source-defined command %q", got, want)
	}
}

func TestSourceDerivedLinuxKconfigIdentityDoesNotPinArchSpecificUTSMachine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "subarch.include"), []byte("SUBARCH := $(shell uname -m | sed -e s/aarch64.*/arm64/)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
include $(srctree)/scripts/subarch.include
ARCH ?= $(SUBARCH)
UTS_MACHINE := $(ARCH)
SRCARCH := $(ARCH)
KCONFIG_CONFIG ?= .config
`), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := testSourceDerivedLinuxKconfigIdentity(t, root, "aarch64-linux-gnu")
	if identity.Arch != "arm64" || identity.Srcarch != "arm64" {
		t.Fatalf("identity = %#v, want arm64 ARCH/SRCARCH", identity)
	}
	vars := map[string]string{}
	for name, value := range map[string]string{"ARCH": identity.Arch, "SRCARCH": identity.Srcarch} {
		vars[name] = value
	}
	if _, ok := vars["UTS_MACHINE"]; ok {
		t.Fatal("preliminary Kconfig identity pinned UTS_MACHINE before arch/arm64/Makefile can override it")
	}
}

func TestSourceDerivedLinuxKconfigIdentitySupportsVendorLayoutAndAssignments(t *testing.T) {
	root := t.TempDir()
	vendor := filepath.Join(root, "vendor", "acme", "make")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "target.mk"), []byte(`
raw_compiler_target := $(shell $(CC) -dumpmachine)
vendor_cpu := $(word 1,$(subst -, ,$(raw_compiler_target)))
ifeq ($(vendor_cpu),AARCH64_BE)
vendor_kernel_arch := arm64
else
vendor_kernel_arch := $(vendor_cpu)
endif
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
include $(srctree)/vendor/acme/make/target.mk
SELECTED_ARCH ?= $(vendor_kernel_arch)
ARCH := $(SELECTED_ARCH)
SRCARCH := $(ARCH)
UTS_MACHINE := acme_$(ARCH)

# There is deliberately no upstream KCONFIG_CONFIG-shaped boundary here.
UNUSED_VENDOR_QUERY := $(shell undeclared-vendor-query)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	identity := testSourceDerivedLinuxKconfigIdentity(t, root, "AARCH64_BE-acme-elf")
	if want := (sourceDerivedLinuxTarget{Machine: "AARCH64_BE-acme-elf", Arch: "arm64", Srcarch: "arm64", UTSMachine: "acme_arm64"}); identity != want {
		t.Fatalf("vendor identity = %#v, want %#v", identity, want)
	}
}

func TestSourceDerivedLinuxKconfigIdentityRejectsUnboundDynamicTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
ARCH := $(shell vendor-architecture-query)
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	const targetIdentity = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const hostIdentity = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResultWithVersion(t, bootstrap.target, targetIdentity, "x86_64-acme-linux", "Acme compiler"),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{"cc": {Path: filepath.Join(root, "configured-cc")}}
	target, host := testKbuildContracts(actions, facts)
	builder, err := kconfig.NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := kconfig.NewLinuxProbeEvaluator(kconfig.LinuxProbeEvaluatorOptions{
		Scope: "target", Architecture: "x86_64", SourceArchitecture: "x86_64",
		SourceRoot: root, Facts: facts, Tools: map[string]string{"cc": actions["cc"].Path}, Discovery: builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sourceDerivedLinuxKconfigIdentity(t.Context(), root, nil, target, host, evaluator)
	if err == nil || !strings.Contains(err.Error(), "unsupported hermetic Kbuild shell command") {
		t.Fatalf("unbound vendor architecture query error = %v", err)
	}
}

func TestEvaluateLinuxKconfigProbesDiscoversAndExactlyReplaysWithoutTools(t *testing.T) {
	const targetIdentity = "sha256-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const hostIdentity = "sha256-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, true),
		"target",
		targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestLinuxCompilerMakefiles(t, root)
	kconfigPath := filepath.Join(root, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
cc-option = $(success,$(CC) -Werror $(CLANG_FLAGS) $(1) -c -x c /dev/null -o .tmp_probe/tmp.o)
capability := $(cc-option,-fbrand-new)

config CC_VERSION_TEXT
	string
	default "$(CC_VERSION_TEXT)"

config MEASURED_CAPABILITY
	bool
	default $(capability)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{}
	for _, role := range testConfiguredKbuildActionRoles {
		actions[role] = configuredKbuildAction{Path: filepath.Join(root, "missing-"+role)}
	}
	targetContract, hostContract := testKbuildContracts(actions, facts)
	discovery, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil,
		map[string]string{"ARCH": "arm64", "MAKECMDGOALS": "all", "SRCARCH": "arm64", "UTS_MACHINE": "arm64"},
		targetIdentity, hostIdentity, facts,
		targetContract, hostContract, "", nil,
	)
	if err != nil {
		t.Fatalf("discovery touched a configured tool or failed: %v", err)
	}
	if len(discovery.plan.Nodes) != 1 {
		t.Fatalf("discovery nodes = %d, want 1", len(discovery.plan.Nodes))
	}
	results := t.TempDir()
	node := discovery.plan.Nodes[0]
	request := discovery.plan.Requests[node.RequestID]
	if len(request.Steps) != 1 ||
		!slices.Contains(request.Steps[0].Arguments, "--target=aarch64-linux-gnu") ||
		!slices.Contains(request.Steps[0].Arguments, "-fintegrated-as") {
		t.Fatalf("Kconfig compiler probe did not use source-exported flags: %#v", request.Steps)
	}
	steps := make([]kconfig.ProbeStepResult, len(request.Steps))
	for index, step := range request.Steps {
		steps[index] = kconfig.ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
	}
	value := true
	writeTestProbeResult(t, results, kconfig.ProbeResult{
		Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: node.Scope, ToolsetIdentity: targetIdentity, Kind: "boolean", Boolean: &value, Steps: steps,
	})
	oracle, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"target": results},
		map[string]string{"target": targetIdentity, "host": hostIdentity},
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil,
		map[string]string{"ARCH": "arm64", "MAKECMDGOALS": "all", "SRCARCH": "arm64", "UTS_MACHINE": "arm64"},
		targetIdentity, hostIdentity, facts,
		targetContract, hostContract, "", oracle,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.plan.Nodes) != 1 || replay.plan.Nodes[0].ID != node.ID {
		t.Fatalf("replay plan %#v differs from discovery %#v", replay.plan.Nodes, discovery.plan.Nodes)
	}
	resolved, err := replay.tree.ResolveConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Value("CONFIG_MEASURED_CAPABILITY"); got != "y" {
		t.Fatalf("resolved capability = %q, want y", got)
	}
	if got, want := resolved.Value("CONFIG_CC_VERSION_TEXT"), strconv.Quote(facts.VersionText()); got != want {
		t.Fatalf("resolved compiler version text = %q, want source-exported %q", got, want)
	}
}

func TestKbuildOnlyVariablesStayOutOfReusableKconfigAndReachKbuild(t *testing.T) {
	const (
		targetIdentity = "sha256-6767676767676767676767676767676767676767676767676767676767676767"
		hostIdentity   = "sha256-6868686868686868686868686868686868686868686868686868686868686868"
	)
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, false),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.host, hostIdentity, false),
		"host", hostIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	externalRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
ARCH ?= x86
SRCARCH := $(ARCH)
UTS_MACHINE := kernel
ifeq ("$(origin M)", "command line")
KBUILD_EXTMOD := $(M)
endif
export KBUILD_EXTMOD
M_PROBE :=
ifneq ($(KBUILD_EXTMOD),)
srcroot := $(realpath $(KBUILD_EXTMOD))
$(if $(srcroot),,$(error specified external module directory "$(KBUILD_EXTMOD)" does not exist))
ifeq ("$(origin KBUILD_EXTMOD)", "file")
M_PROBE := $(shell { $(CC) -Werror -fexternal-module-only -c -x c /dev/null -o .tmp_probe/m.o; } >/dev/null 2>&1 && echo "-fexternal-enabled" || echo "")
UTS_MACHINE := external
endif
endif
export CC M_PROBE
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kconfigPath := filepath.Join(root, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte(`
config BASE
	bool
	default y
`), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{
		"cc": {Path: filepath.Join(root, "configured-cc")},
	}
	targetContract, hostContract := testKbuildContracts(actions, targetFacts)
	sharedVariables := map[string]string{"ARCH": "x86", "SRCARCH": "x86"}

	discovery, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil, sharedVariables,
		targetIdentity, hostIdentity, targetFacts,
		targetContract, hostContract, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(discovery.plan.Nodes); got != 0 {
		t.Fatalf("kernel Kconfig discovery contains %d external-module probes, want none: %#v", got, discovery.plan.Nodes)
	}
	oracle, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"target": t.TempDir(), "host": t.TempDir()},
		map[string]string{"target": targetIdentity, "host": hostIdentity},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil, sharedVariables,
		targetIdentity, hostIdentity, targetFacts,
		targetContract, hostContract, "", oracle,
	); err != nil {
		t.Fatalf("reusable kernel Kconfig replay failed without consumer-only M: %v", err)
	}

	leakedKconfigVariables := maps.Clone(sharedVariables)
	addKbuildOnlyVariables(leakedKconfigVariables, map[string]string{"M": externalRoot})
	if _, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil, leakedKconfigVariables,
		targetIdentity, hostIdentity, targetFacts,
		targetContract, hostContract, "", oracle,
	); err == nil || !strings.Contains(err.Error(), "missing result") {
		t.Fatalf("Kconfig replay with consumer-only M error = %v, want a newly demanded external probe", err)
	}

	externalMarker := kbuildEvalSourceTree + "/.linux-bzl/external/module"
	kbuildVariables := maps.Clone(sharedVariables)
	identityVariables := addKbuildOnlyVariables(kbuildVariables, map[string]string{"M": externalMarker})
	if _, leaked := identityVariables["M"]; leaked {
		t.Fatalf("kernel identity variables contain consumer-only M: %#v", identityVariables)
	}
	if got := kbuildVariables["M"]; got != externalMarker {
		t.Fatalf("actual Kbuild variables M = %q, want %q", got, externalMarker)
	}
	workload := kconfig.KbuildProbeWorkloadOptions{
		Target: kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: targetFacts, Tools: map[string]string{"cc": actions["cc"].Path},
		},
		Host: &kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: hostFacts, Tools: map[string]string{"cc": actions["cc"].Path},
		},
	}
	identityEvaluation, err := kconfig.EvaluateKbuildProbeWorkload(
		workload,
		nil,
		func(scopes *kconfig.KbuildProbeScopes) (sourceDerivedLinuxTarget, error) {
			return sourceDerivedLinuxMakeIdentity(
				root, "x86", identityVariables, nil, targetContract, hostContract, scopes,
			)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := identityEvaluation.Value.UTSMachine, "kernel"; got != want {
		t.Fatalf("kernel-context Kbuild UTS_MACHINE = %q, want %q", got, want)
	}

	// The identity evaluator itself still resolves any explicitly supplied
	// mapped source roots hermetically. The phase boundary above, rather than a
	// hard-coded M exception in that evaluator, decides which variables belong
	// to the reusable kernel identity.
	externalEvaluation, err := kconfig.EvaluateKbuildProbeWorkload(
		workload,
		nil,
		func(scopes *kconfig.KbuildProbeScopes) (sourceDerivedLinuxTarget, error) {
			return sourceDerivedLinuxMakeIdentity(
				root,
				"x86",
				kbuildVariables,
				map[string]string{externalMarker: externalRoot},
				targetContract,
				hostContract,
				scopes,
			)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := externalEvaluation.Value.UTSMachine, "external"; got != want {
		t.Fatalf("Kbuild UTS_MACHINE = %q, want M-selected %q", got, want)
	}
}

func TestEvaluateLinuxKconfigProbesLeavesCompilerIdentificationToSourceMake(t *testing.T) {
	const targetIdentity = "sha256-5656565656565656565656565656565656565656565656565656565656565656"
	const hostIdentity = "sha256-7878787878787878787878787878787878787878787878787878787878787878"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResultWithVersion(
			t, bootstrap.target, targetIdentity,
			"riscv64-acme-elf", "Acme C compiler 4.2",
		),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
COMPILER_MACHINE := $(shell $(CC) -dumpmachine)
SUBARCH := $(word 1,$(subst -, ,$(COMPILER_MACHINE)))
ifeq ($(SUBARCH),riscv64)
SUBARCH := riscv
endif
ARCH ?= $(SUBARCH)
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
CC_VERSION_TEXT = $(shell LC_ALL=C $(CC) --version 2>/dev/null | head -n 1)
ACME_DRIVER_FLAGS :=
ifneq ($(findstring Acme C compiler,$(CC_VERSION_TEXT)),)
ACME_DRIVER_FLAGS += -facme-source-owned -mllvm -future-pass=2
endif
ROOT_POLICY := $(shell { $(CC) -Werror -froot-policy -c -x c /dev/null -o .tmp_probe/root.o; } >/dev/null 2>&1 && echo "-froot-enabled" || echo "")
export ACME_DRIVER_FLAGS CC CC_VERSION_TEXT ROOT_POLICY
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kconfigPath := filepath.Join(root, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
capability := $(success,$(CC) $(ACME_DRIVER_FLAGS) $(ROOT_POLICY) -fprobe -c -x c /dev/null -o .tmp_probe/tmp.o)
config ACME_CAPABILITY
	bool
	default $(capability)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{
		"cc": {Path: filepath.Join(root, "configured-acme-cc")},
	}
	targetContract, hostContract := testKbuildContracts(actions, facts)
	evaluation, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil,
		map[string]string{"ARCH": "riscv", "SRCARCH": "riscv"},
		targetIdentity, hostIdentity, facts,
		targetContract, hostContract, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evaluation.plan.Nodes), 2; got != want {
		t.Fatalf("source-selected Acme probe nodes = %d, want %d", got, want)
	}
	rootPolicy, finalKconfig := false, false
	for _, node := range evaluation.plan.Nodes {
		request := evaluation.plan.Requests[node.RequestID]
		if len(request.Steps) != 1 {
			continue
		}
		arguments := request.Steps[0].Arguments
		if slices.Contains(arguments, "-froot-policy") {
			rootPolicy = true
		}
		if slices.Contains(arguments, "-facme-source-owned") && slices.Contains(arguments, "-future-pass=2") {
			finalKconfig = true
			if len(node.Inputs) != 1 || len(request.Steps[0].ConditionalArguments) != 1 {
				t.Fatalf("final Kconfig probe lost source-policy dependency: node=%#v request=%#v", node, request)
			}
		}
	}
	if !rootPolicy || !finalKconfig {
		t.Fatalf("source-derived plan retained root policy=%v final Kconfig=%v: %#v", rootPolicy, finalKconfig, evaluation.plan.Nodes)
	}
	if got := facts.VersionText(); got != "Acme C compiler 4.2" {
		t.Fatalf("bootstrap version text = %q, want raw unclassified value", got)
	}
}

func TestEvaluateLinuxKconfigProbesUsesGCCExportedDriverFields(t *testing.T) {
	const targetIdentity = "sha256-9090909090909090909090909090909090909090909090909090909090909090"
	const hostIdentity = "sha256-9191919191919191919191919191919191919191919191919191919191919191"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, false),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
COMPILER_MACHINE := $(shell $(CC) -dumpmachine)
SUBARCH := $(word 1,$(subst -, ,$(COMPILER_MACHINE)))
ifeq ($(SUBARCH),x86_64)
SUBARCH := x86
endif
ARCH ?= $(SUBARCH)
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
CC_VERSION_TEXT = $(shell LC_ALL=C $(CC) --version 2>/dev/null | head -n 1)
GCC_DRIVER_FIELDS :=
PRIVATE_DRIVER_FIELDS := -fprivate-must-not-leak
ifneq ($(findstring gcc,$(CC_VERSION_TEXT)),)
GCC_DRIVER_FIELDS += -fgcc-source-owned
endif
export CC CC_VERSION_TEXT GCC_DRIVER_FIELDS
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kconfigPath := filepath.Join(root, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
capability := $(success,$(CC) $(GCC_DRIVER_FIELDS) $(PRIVATE_DRIVER_FIELDS) -fprobe -c -x c /dev/null -o .tmp_probe/tmp.o)
config GCC_CAPABILITY
	bool
	default $(capability)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{
		"cc": {Path: filepath.Join(root, "configured-gcc")},
	}
	targetContract, hostContract := testKbuildContracts(actions, facts)
	evaluation, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil,
		map[string]string{"ARCH": "x86", "SRCARCH": "x86"},
		targetIdentity, hostIdentity, facts,
		targetContract, hostContract, "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evaluation.plan.Nodes), 1; got != want {
		t.Fatalf("source-selected GCC probe nodes = %d, want %d", got, want)
	}
	request := evaluation.plan.Requests[evaluation.plan.Nodes[0].RequestID]
	if len(request.Steps) != 1 || !slices.Contains(request.Steps[0].Arguments, "-fgcc-source-owned") {
		t.Fatalf("GCC source export did not reach Kconfig probe: %#v", request.Steps)
	}
	if slices.Contains(request.Steps[0].Arguments, "-fprivate-must-not-leak") {
		t.Fatalf("unexported compiler field leaked into Kconfig probe: %#v", request.Steps)
	}
}

func TestEvaluateLinuxKconfigProbesDerivesRustAndBindgenFromGenericManifest(t *testing.T) {
	const targetIdentity = "sha256-1212121212121212121212121212121212121212121212121212121212121212"
	const hostIdentity = "sha256-3434343434343434343434343434343434343434343434343434343434343434"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, true),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestLinuxCompilerMakefiles(t, root)
	makefilePath := filepath.Join(root, "Makefile")
	makefile, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatal(err)
	}
	makefile = append(makefile, []byte("\nRUSTC_BOOTSTRAP := source-selected\nROLE_FREE_ENV := source-selected\nexport RUSTC_BOOTSTRAP ROLE_FREE_ENV\n")...)
	if err := os.WriteFile(makefilePath, makefile, 0o644); err != nil {
		t.Fatal(err)
	}
	kconfigPath := filepath.Join(root, "Kconfig")
	if err := os.WriteFile(kconfigPath, []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
rustc-option = $(success,trap "rm -rf .tmp_$$" EXIT; mkdir .tmp_$$; $(RUSTC) $(1) --crate-type=rlib /dev/null --out-dir=.tmp_$$ -o .tmp_$$/tmp.rlib)
rust-capability := $(rustc-option,-Zbrand-new)
bindgen-version := $(shell,$(BINDGEN) --version workaround-for-0.69.0 2>/dev/null)
python-capability := $(success,$(PYTHON3) -c "import lxml")

config RUST_CAPABILITY
	bool
	default $(rust-capability)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := map[string]configuredKbuildAction{}
	for _, role := range testConfiguredKbuildActionRoles {
		actions[role] = configuredKbuildAction{Path: filepath.Join(root, "missing-"+role)}
	}
	for _, role := range []string{"bindgen", "pahole", "python3", "rustc"} {
		actions[role] = configuredKbuildAction{Path: filepath.Join(root, "missing-"+role)}
	}
	targetContract, hostContract := testKbuildContracts(actions, facts)
	targetContract.MakeVariables["RUSTC_OR_CLIPPY"] = "rustc"
	probeVariables := map[string]string{
		"ARCH": "arm64", "SRCARCH": "arm64", "RUST_LIB_SRC": "external/rust-src/library",
	}
	probeVariablesBefore := maps.Clone(probeVariables)
	discovery, err := evaluateLinuxKconfigProbes(
		t.Context(), kconfigPath, root, nil,
		probeVariables,
		targetIdentity, hostIdentity, facts,
		targetContract, hostContract,
		"external/rust-src/library", nil,
	)
	if err != nil {
		t.Fatalf("Rust/Python discovery executed an absent tool or failed: %v", err)
	}
	if !maps.Equal(probeVariables, probeVariablesBefore) {
		t.Errorf("Kconfig evaluation mutated caller variables: got %#v, want %#v", probeVariables, probeVariablesBefore)
	}
	roles := map[string]bool{}
	for _, request := range discovery.plan.Requests {
		for _, role := range request.ToolRoles() {
			roles[role] = true
		}
	}
	for _, role := range []string{"rustc", "bindgen", "python3"} {
		if !roles[role] {
			t.Errorf("Kconfig discovery plan does not bind %s: %#v", role, roles)
		}
	}
	foundRustEnvironment := false
	for _, request := range discovery.plan.Requests {
		if len(request.Steps) != 1 || request.Steps[0].Tool != "rustc" {
			continue
		}
		foundRustEnvironment = true
		for name, want := range map[string]string{
			"ROLE_FREE_ENV": "source-selected", "RUSTC_BOOTSTRAP": "source-selected",
		} {
			if got := request.Steps[0].Environment[name]; got != want {
				t.Errorf("source-exported rustc environment %s=%q, want %q", name, got, want)
			}
		}
	}
	if !foundRustEnvironment {
		t.Fatal("Kconfig discovery plan has no rustc request")
	}
}

func TestLinuxRootMakeInvocationVariablesSelectFinalBuild(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	vars := linuxRootMakeInvocationVariables(root)
	for name, want := range map[string]string{
		"CURDIR": root, "MAKECMDGOALS": "all", "MAKEFLAGS": "--no-print-directory",
		"abs_srctree": root, "objtree": root, "srctree": root,
	} {
		if got := vars[name]; got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"KBUILD_EXTMOD", "KBUILD_OUTPUT", "need-sub-make"} {
		if got, ok := vars[name]; !ok || got != "" {
			t.Errorf("%s=%q,%v, want defined empty", name, got, ok)
		}
	}
	if _, ok := vars["sub_make_done"]; ok {
		t.Fatalf("sub_make_done must remain source-derived: %#v", vars)
	}
}

func TestLinuxRootKconfigInvocationVariablesSelectConfigBuild(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	vars := linuxRootKconfigInvocationVariables(root)
	if got, want := vars["MAKECMDGOALS"], "olddefconfig"; got != want {
		t.Fatalf("MAKECMDGOALS=%q, want canonical Kconfig goal %q", got, want)
	}
	if got := vars["srctree"]; got != root {
		t.Fatalf("srctree=%q, want %q", got, root)
	}
}

func TestKbuildVariablesForConfigUsesWrittenConfigView(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config DISABLED
	bool
config HIDDEN
	bool
config MODULE
	tristate
config WRITTEN
	bool
config STRING
	string
config EMPTY
	string
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	vars := kbuildVariablesForConfig(
		map[string]string{
			"ARCH":        "arm64",
			"CONFIG_BASE": "base",
		},
		tree,
		&kconfig.ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_DISABLED": "n",
				"CONFIG_EMPTY":    `""`,
				"CONFIG_HIDDEN":   "y",
				"CONFIG_MODULE":   "m",
				"CONFIG_STRING":   `"one two"`,
				"CONFIG_WRITTEN":  "y",
			},
			Written: map[string]bool{
				"CONFIG_EMPTY":   true,
				"CONFIG_MODULE":  true,
				"CONFIG_STRING":  true,
				"CONFIG_WRITTEN": true,
			},
		},
	)

	for key, want := range map[string]string{
		"ARCH":            "arm64",
		"CONFIG_BASE":     "base",
		"CONFIG_DISABLED": "",
		"CONFIG_EMPTY":    "",
		"CONFIG_HIDDEN":   "",
		"CONFIG_MODULE":   "m",
		"CONFIG_STRING":   "one two",
		"CONFIG_WRITTEN":  "y",
		"comma":           ",",
	} {
		if got := vars[key]; got != want {
			t.Fatalf("vars[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestKbuildVariablesForConfigDoesNotInventEmptyFirmwareObject(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config EXTRA_FIRMWARE
	string "External firmware"
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{"CONFIG_EXTRA_FIRMWARE": `""`},
		Written:   map[string]bool{"CONFIG_EXTRA_FIRMWARE": true},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(path, []byte(`firmware := $(addsuffix .gen.o, $(CONFIG_EXTRA_FIRMWARE))
obj-y += $(firmware)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := kconfig.ParseKbuildFileWithOptions(path, kconfig.KbuildOptions{
		Variables:        kbuildVariablesForConfig(nil, tree, resolved),
		CaptureVariables: []string{"firmware"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kb.Variables["firmware"]; got != "" {
		t.Fatalf("empty CONFIG_EXTRA_FIRMWARE produced firmware value %q", got)
	}
}

func TestWriteResolvedConfigOutputsUsesAutoConfStringEncoding(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config ENABLED
	bool
config DISABLED
	bool
config EXTRA_FIRMWARE
	string
config FIRMWARE_LIST
	string
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outputs := resolvedConfigOutputs{
		config:        filepath.Join(dir, ".config"),
		autoConf:      filepath.Join(dir, "auto.conf"),
		autoConfCmd:   filepath.Join(dir, "auto.conf.cmd"),
		autoconf:      filepath.Join(dir, "autoconf.h"),
		rustcCfg:      filepath.Join(dir, "rustc_cfg"),
		kernelRelease: filepath.Join(dir, "kernel.release"),
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_DISABLED":       "n",
			"CONFIG_ENABLED":        "y",
			"CONFIG_EXTRA_FIRMWARE": `""`,
			"CONFIG_FIRMWARE_LIST":  `"one.bin two.bin"`,
		},
		Written: map[string]bool{
			"CONFIG_ENABLED":        true,
			"CONFIG_EXTRA_FIRMWARE": true,
			"CONFIG_FIRMWARE_LIST":  true,
		},
	}
	if err := writeResolvedConfigOutputs(tree, resolved, outputs, "6.18.39"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		outputs.config:   "# CONFIG_DISABLED is not set\nCONFIG_ENABLED=y\nCONFIG_EXTRA_FIRMWARE=\"\"\nCONFIG_FIRMWARE_LIST=\"one.bin two.bin\"\n",
		outputs.autoConf: "CONFIG_ENABLED=y\nCONFIG_EXTRA_FIRMWARE=\nCONFIG_FIRMWARE_LIST=one.bin two.bin\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s =\n%s\nwant:\n%s", filepath.Base(path), got, want)
		}
	}
}

func TestWriteResolvedArchitecturePreservesSourceDerivedValue(t *testing.T) {
	output := filepath.Join(t.TempDir(), "linux.arch")
	if err := writeResolvedArchitecture(output, "vendor-riscv"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "vendor-riscv\n"; got != want {
		t.Fatalf("resolved architecture = %q, want %q", got, want)
	}
}

func TestResolvedConfigUnsetStateRoundTripsIntoDefaultMode(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config DEFAULT_ON
	bool "Default on"
	default y

config DEFAULT_OFF
	bool "Default off"
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := tree.ResolveConfigWithOptions(nil, kconfig.ResolveConfigOptions{AllNoConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outputs := resolvedConfigOutputs{
		config:        filepath.Join(dir, ".config"),
		autoConf:      filepath.Join(dir, "auto.conf"),
		autoConfCmd:   filepath.Join(dir, "auto.conf.cmd"),
		autoconf:      filepath.Join(dir, "autoconf.h"),
		rustcCfg:      filepath.Join(dir, "rustc_cfg"),
		kernelRelease: filepath.Join(dir, "kernel.release"),
	}
	if err := writeResolvedConfigOutputs(tree, resolved, outputs, "6.18.39"); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(outputs.config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(config), "# CONFIG_DEFAULT_OFF is not set\n# CONFIG_DEFAULT_ON is not set\n"; got != want {
		t.Fatalf("resolved .config = %q, want %q", got, want)
	}
	raw, err := kconfig.ParseConfig(strings.NewReader(string(config)))
	if err != nil {
		t.Fatal(err)
	}
	replanned, err := tree.ResolveConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CONFIG_DEFAULT_OFF", "CONFIG_DEFAULT_ON"} {
		if got := replanned.Value(key); got != "n" {
			t.Fatalf("replanned %s = %q, want n; raw=%#v", key, got, raw)
		}
	}
}

func TestWriteResolvedConfigAcceptsPlainPath(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader("config ENABLED\n\tbool \"Enabled\"\n"),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "input=config")
	if err := os.WriteFile(input, []byte("CONFIG_ENABLED=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputs := resolvedConfigOutputs{
		config:        filepath.Join(dir, ".config"),
		autoConf:      filepath.Join(dir, "auto.conf"),
		autoConfCmd:   filepath.Join(dir, "auto.conf.cmd"),
		autoconf:      filepath.Join(dir, "autoconf.h"),
		rustcCfg:      filepath.Join(dir, "rustc_cfg"),
		kernelRelease: filepath.Join(dir, "kernel.release"),
	}
	if err := writeResolvedConfig(tree, input, nil, "default", outputs, "6.18.39"); err != nil {
		t.Fatalf("writeResolvedConfig(%q) failed: %v", input, err)
	}
	content, err := os.ReadFile(outputs.config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "CONFIG_ENABLED=y\n"; got != want {
		t.Fatalf("resolved config = %q, want %q", got, want)
	}
}

func TestValidateKernelVersionRequiresExplicitValueForPlanning(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		required bool
		wantErr  bool
	}{
		{name: "unused", required: false},
		{name: "explicit", value: "6.18.39", required: true},
		{name: "missing", required: true, wantErr: true},
		{name: "whitespace", value: " \t\n", required: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateKernelVersion(test.value, test.required)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateKernelVersion(%q, %t) error = %v, want error %t", test.value, test.required, err, test.wantErr)
			}
			if test.wantErr && !strings.Contains(err.Error(), "-kernel_version is required") {
				t.Fatalf("validateKernelVersion error = %q, want required flag diagnostic", err)
			}
		})
	}
}

func TestRustcCfgLinesMatchKernelEncoding(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config BOOL
	bool
config TRI
	tristate
config STR
	string
config EMPTY
	string
config INT
	int
config HEX
	hex
config HEX_PREFIXED
	hex
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_BOOL":         "y",
			"CONFIG_EMPTY":        "",
			"CONFIG_TRI":          "m",
			"CONFIG_STR":          `"quoted \"value\""`,
			"CONFIG_INT":          "42",
			"CONFIG_HEX":          "2a",
			"CONFIG_HEX_PREFIXED": "0X2A",
		},
		Written: map[string]bool{
			"CONFIG_BOOL":         true,
			"CONFIG_EMPTY":        true,
			"CONFIG_TRI":          true,
			"CONFIG_STR":          true,
			"CONFIG_INT":          true,
			"CONFIG_HEX":          true,
			"CONFIG_HEX_PREFIXED": true,
		},
	}
	got := strings.Join(rustcCfgLines(tree, resolved), "\n")
	want := strings.Join([]string{
		`--cfg=CONFIG_BOOL`,
		`--cfg=CONFIG_BOOL="y"`,
		`--cfg=CONFIG_EMPTY=""`,
		`--cfg=CONFIG_HEX="0x2a"`,
		`--cfg=CONFIG_HEX_PREFIXED="0X2A"`,
		`--cfg=CONFIG_INT="42"`,
		`--cfg=CONFIG_STR="quoted \"value\""`,
		`--cfg=CONFIG_TRI`,
		`--cfg=CONFIG_TRI="m"`,
	}, "\n")
	if got != want {
		t.Fatalf("rustcCfgLines() =\n%s\nwant:\n%s", got, want)
	}
}

func TestWorkspaceDirectoryAcceptsDirectoryOrRegularRootMarker(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "Kconfig")
	if err := os.WriteFile(marker, []byte("# root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{dir, marker} {
		got, err := workspaceDirectory(input)
		if err != nil {
			t.Fatalf("workspaceDirectory(%q): %v", input, err)
		}
		if got != dir {
			t.Fatalf("workspaceDirectory(%q) = %q, want %q", input, got, dir)
		}
	}
	other := filepath.Join(dir, "pipe")
	if err := os.Symlink(marker, other); err != nil {
		t.Fatal(err)
	}
	// os.Stat follows Bazel's input symlink and still recognizes the declared
	// regular marker.
	if got, err := workspaceDirectory(other); err != nil || got != dir {
		t.Fatalf("workspaceDirectory(symlink) = %q, %v, want %q", got, err, dir)
	}
}

func TestSourceDerivedLinuxMakeIdentityCanonicalizesKconfigRootMarker(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "external", "+linux_source_repository+linux_6_18_39")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repository, "Kconfig")
	if err := os.WriteFile(marker, []byte("# root marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "Makefile"), []byte(`
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", workspace)
	relativeMarker, err := filepath.Rel(workspace, marker)
	if err != nil {
		t.Fatal(err)
	}

	const targetIdentity = "sha256-3434343434343434343434343434343434343434343434343434343434343434"
	const hostIdentity = "sha256-5656565656565656565656565656565656565656565656565656565656565656"
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, true),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.host, hostIdentity, false),
		"host", hostIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := func(scope string, facts *kconfig.LinuxCompilerFacts) *hostKbuildContract {
		actions := make(map[string]configuredKbuildAction, len(testConfiguredKbuildActionRoles))
		for _, role := range testConfiguredKbuildActionRoles {
			actions[role] = configuredKbuildAction{Path: filepath.Join(repository, scope+"-"+role)}
		}
		return &hostKbuildContract{Actions: actions, CompilerMachine: facts.Machine()}
	}
	targetContract := contract("target", targetFacts)
	hostContract := contract("host", hostFacts)
	probeTools := func(contract *hostKbuildContract, includePahole bool) map[string]string {
		tools := map[string]string{}
		for _, role := range []string{"ar", "cc", "ld", "nm", "objcopy"} {
			tools[role] = contract.Actions[role].Path
		}
		if includePahole {
			tools["pahole"] = filepath.Join(repository, "target-pahole")
		}
		return tools
	}
	evaluation, err := kconfig.EvaluateKbuildProbeWorkload(
		kconfig.KbuildProbeWorkloadOptions{
			Target: kconfig.KbuildProbeScopeOptions{Architecture: "arm64", Facts: targetFacts, Tools: probeTools(targetContract, true)},
			Host:   &kconfig.KbuildProbeScopeOptions{Architecture: "arm64", Facts: hostFacts, Tools: probeTools(hostContract, false)},
		},
		nil,
		func(scopes *kconfig.KbuildProbeScopes) (sourceDerivedLinuxTarget, error) {
			return sourceDerivedLinuxMakeIdentity(
				relativeMarker,
				"arm64",
				map[string]string{},
				nil,
				targetContract,
				hostContract,
				scopes,
			)
		},
	)
	if err != nil {
		t.Fatalf("sourceDerivedLinuxMakeIdentity(root marker) failed: %v", err)
	}
	if got, want := evaluation.Value, (sourceDerivedLinuxTarget{Arch: "arm64", Srcarch: "arm64", UTSMachine: "arm64"}); got != want {
		t.Fatalf("source-derived identity = %#v, want %#v", got, want)
	}
}

func TestKbuildInvocationSentinelShellEvaluatesFindAgainstTreeIndexes(t *testing.T) {
	called := false
	calledWith := ""
	shell := kbuildInvocationSentinelShell("/physical/kernel", "/physical/kernel", "x86", kbuildSourceInputIndex{
		files: []string{
			"drivers/example/generated.h",
			"drivers/example/nested/other.h",
			"drivers/example/source.c",
		},
		directories: []string{"drivers", "drivers/example", "drivers/example/nested"},
	}, nil, func(command string) (string, error) {
		called = true
		calledWith = command
		return command, nil
	})
	for _, command := range []string{
		`find __LINUX_BZL_OBJECT_TREE__/drivers/example -name \*.gen.S 2>/dev/null`,
		`find drivers/example -name \*.gen.S 2>/dev/null`,
	} {
		got, err := shell(command)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Fatalf("object-tree find %q = %q, want empty", command, got)
		}
		if called {
			t.Fatalf("object-tree find %q reached physical shell evaluator", command)
		}
	}
	got, err := shell(`find __LINUX_BZL_SOURCE_TREE__/drivers/example -type f -name '*.h'`)
	if err != nil {
		t.Fatal(err)
	}
	want := "__LINUX_BZL_SOURCE_TREE__/drivers/example/generated.h\n" +
		"__LINUX_BZL_SOURCE_TREE__/drivers/example/nested/other.h\n"
	if got != want {
		t.Fatalf("source-tree find = %q, want %q", got, want)
	}
	if called {
		t.Fatal("source-tree find reached physical shell evaluator")
	}

	got, err = shell(`probe __LINUX_BZL_SOURCE_TREE__/drivers/example/source.c`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `probe __LINUX_BZL_SOURCE_TREE__/drivers/example/source.c`; got != want {
		t.Fatalf("non-find shell result = %q, want stable %q", got, want)
	}
	if !called {
		t.Fatal("non-find command did not reach declared shell evaluator")
	}
	if want := `probe __LINUX_BZL_SOURCE_TREE__/drivers/example/source.c`; calledWith != want {
		t.Fatalf("declared shell command = %q, want stable %q", calledWith, want)
	}

	called = false
	got, err = shell(`probe /physical/kernel/drivers/example/source.c`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `probe __LINUX_BZL_SOURCE_TREE__/drivers/example/source.c`; got != want || calledWith != want {
		t.Fatalf("physical command normalized to result %q via %q, want %q", got, calledWith, want)
	}
	if !called {
		t.Fatal("normalized physical command did not reach declared shell evaluator")
	}
}

func TestKbuildInvocationSentinelShellResolvesOutputDirectoryBeforeProbeFallback(t *testing.T) {
	called := false
	shell := kbuildInvocationSentinelShell(
		"/physical/kernel", "/physical/object", "x86", kbuildSourceInputIndex{}, nil,
		func(command string) (string, error) {
			called = true
			return command, nil
		},
	)
	command := "cd " + kbuildEvalSourceTree + "/tools/lib/subcmd; cd " +
		kbuildEvalObjectTree + "/tools/objtool/libsubcmd ; pwd"
	got, err := shell(command)
	if err != nil {
		t.Fatal(err)
	}
	if want := kbuildEvalObjectTree + "/tools/objtool/libsubcmd"; got != want {
		t.Fatalf("split-root output directory query = %q, want %q", got, want)
	}
	if called {
		t.Fatal("split-root output directory query reached generic probe fallback")
	}
}

func TestKbuildInvocationSentinelShellPhysicalizesOnlyFallbackQueries(t *testing.T) {
	commands := []string{}
	shell := kbuildInvocationSentinelShell("/physical/kernel", "/physical/kernel", "x86", kbuildSourceInputIndex{}, nil, func(command string) (string, error) {
		commands = append(commands, command)
		if strings.Contains(command, kbuildEvalSourceTree) {
			return "", kbuildInvocationPhysicalPathError{err: fmt.Errorf("stable path requires filesystem fallback")}
		}
		return command, nil
	})
	got, err := shell(`read __LINUX_BZL_SOURCE_TREE__/scripts/value`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `read __LINUX_BZL_SOURCE_TREE__/scripts/value`; got != want {
		t.Fatalf("fallback result = %q, want stable %q", got, want)
	}
	wantCommands := []string{
		`read __LINUX_BZL_SOURCE_TREE__/scripts/value`,
		`read /physical/kernel/scripts/value`,
	}
	if !slices.Equal(commands, wantCommands) {
		t.Fatalf("fallback commands = %q, want %q", commands, wantCommands)
	}
}

func TestKbuildInvocationSentinelVariablesNormalizeLexicalAndPhysicalRoots(t *testing.T) {
	physicalRoot := filepath.Join(t.TempDir(), "repository-cache", "linux")
	if err := os.MkdirAll(filepath.Join(physicalRoot, "arch", "x86", "include", "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	lexicalParent := t.TempDir()
	lexicalRoot := filepath.Join(lexicalParent, "external-linux")
	if err := os.Symlink(physicalRoot, lexicalRoot); err != nil {
		t.Fatal(err)
	}

	values := kbuildInvocationSentinelVariables(lexicalRoot, map[string]string{
		"SRCARCH":         "x86",
		"PHYSICAL_SOURCE": "-fmacro-prefix-map=" + filepath.ToSlash(physicalRoot) + "/=",
		"LEXICAL_SOURCE":  "-I" + filepath.ToSlash(lexicalRoot) + "/include",
		"GENERATED":       "-I" + filepath.ToSlash(physicalRoot) + "/arch/x86/include/generated",
	}, "")
	if got, want := values["PHYSICAL_SOURCE"], "-fmacro-prefix-map="+kbuildEvalSourceTree+"/="; got != want {
		t.Fatalf("physical source value = %q, want %q", got, want)
	}
	if got, want := values["LEXICAL_SOURCE"], "-I"+kbuildEvalSourceTree+"/include"; got != want {
		t.Fatalf("lexical source value = %q, want %q", got, want)
	}
	if got, want := values["GENERATED"], "-I"+kbuildEvalObjectTree+"/arch/x86/include/generated"; got != want {
		t.Fatalf("generated value = %q, want %q", got, want)
	}
}

func TestKbuildInvocationSentinelNormalizationRetainsLexicalAndPhysicalRoots(t *testing.T) {
	physicalRoot := filepath.Join(t.TempDir(), "repository-cache", "linux")
	if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lexicalRoot := filepath.Join(t.TempDir(), "external-linux")
	if err := os.Symlink(physicalRoot, lexicalRoot); err != nil {
		t.Fatal(err)
	}

	normalization := newKbuildInvocationSentinelNormalization(lexicalRoot, "x86")
	// Removing the symlink after construction proves that value normalization
	// reuses the captured physical root instead of resolving it for every value.
	if err := os.Remove(lexicalRoot); err != nil {
		t.Fatal(err)
	}
	value := "-I" + filepath.ToSlash(lexicalRoot) + "/include " +
		"-fmacro-prefix-map=" + filepath.ToSlash(physicalRoot) + "/="
	if got, want := normalization.value(value),
		"-I"+kbuildEvalSourceTree+"/include -fmacro-prefix-map="+kbuildEvalSourceTree+"/="; got != want {
		t.Fatalf("cached sentinel normalization = %q, want %q", got, want)
	}
}

func TestKbuildInvocationSentinelVariableOverridesAreSparseAndIsolated(t *testing.T) {
	root := filepath.ToSlash(t.TempDir())
	normalization := newKbuildInvocationSentinelNormalization(root, "arm")
	base := normalization.normalizedVariableBase(map[string]string{
		"SRCARCH": "arm",
		"CFLAGS":  "-I" + root + "/include",
	})
	drivers := kbuildInvocationSentinelVariableOverrides("drivers/example")
	arch := kbuildInvocationSentinelVariableOverrides("arch/arm")

	if got, want := base["CFLAGS"], "-I"+kbuildEvalSourceTree+"/include"; got != want {
		t.Fatalf("normalized base CFLAGS = %q, want %q", got, want)
	}
	if got, want := base["obj"], "."; got != want {
		t.Fatalf("normalized base obj = %q, want %q", got, want)
	}
	if got, want := base["src"], kbuildEvalSourceTree; got != want {
		t.Fatalf("normalized base src = %q, want %q", got, want)
	}
	if got, want := drivers["obj"], "drivers/example"; got != want {
		t.Fatalf("drivers obj = %q, want %q", got, want)
	}
	if got, want := drivers["src"], kbuildEvalSourceTree+"/drivers/example"; got != want {
		t.Fatalf("drivers src = %q, want %q", got, want)
	}
	if got, want := arch["obj"], "arch/arm"; got != want {
		t.Fatalf("arch obj = %q, want %q", got, want)
	}
	if got, want := arch["src"], kbuildEvalSourceTree+"/arch/arm"; got != want {
		t.Fatalf("arch src = %q, want %q", got, want)
	}
	if got, want := len(drivers), 2; got != want {
		t.Fatalf("directory override count = %d, want sparse %d", got, want)
	}
	if _, ok := drivers["CFLAGS"]; ok {
		t.Fatal("directory overrides copied the invariant CFLAGS base")
	}

	drivers["CFLAGS"] = "mutated"
	drivers["obj"] = "mutated"
	if got, want := base["CFLAGS"], "-I"+kbuildEvalSourceTree+"/include"; got != want {
		t.Fatalf("directory clone mutated normalized base CFLAGS: got %q, want %q", got, want)
	}
	if got, want := arch["obj"], "arch/arm"; got != want {
		t.Fatalf("directory clone mutated sibling obj: got %q, want %q", got, want)
	}
}

func TestKbuildProfileExplicitPhonyTargetDoesNotSelectCatchAllImplicitRule(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "root-explicit-phony-fixture",
		EntryTargets: []string{"__default"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"__default"}, Prerequisites: []string{"vmlinux"}},
			{Targets: []string{"vmlinux"}, Recipe: []string{"touch $@"}},
			{Targets: []string{"%"}, Prerequisites: []string{"%_shipped"}, Recipe: []string{"cp $< $@"}},
			{Targets: []string{".PHONY"}, Prerequisites: []string{"__default"}},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	rules := kbuildProfileRulesForTargetIndexed(profile, newKbuildProfileTargetIndex(profile, nil), "__default", map[string]bool{})
	if got, want := len(rules), 1; got != want || !slices.Contains(rules[0].Targets, "__default") {
		t.Fatalf("selected __default rules = %#v, want only the explicit rule", rules)
	}
	requests, err := selectedKbuildRecursiveMakeRequests(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("recursive Make requests = %#v, want none", requests)
	}
	for _, selection := range mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{}) {
		if strings.Contains(selection.Target, "_shipped") {
			t.Fatalf("catch-all implicit rule invented selection %#v", selection)
		}
	}
}

func TestKbuildRecipeOnlyCreatesDirectoriesAcceptsEvaluatedTargetDirectory(t *testing.T) {
	for _, recipe := range []string{
		"mkdir -p tools/objtool/libsubcmd",
		"@mkdir --parents arch/x86/include/generated",
		"$(Q)mkdir -p $@",
	} {
		if !kbuildRecipeOnlyCreatesDirectories(recipe) {
			t.Errorf("recipe %q was not classified as directory-only", recipe)
		}
	}
	for _, recipe := range []string{
		"mkdir tools/objtool/libsubcmd",
		"mkdir -p generated && touch generated/result",
	} {
		if kbuildRecipeOnlyCreatesDirectories(recipe) {
			t.Errorf("recipe %q was incorrectly classified as directory-only", recipe)
		}
	}
}

func TestKbuildSelectedRecipeObjectTreeReferencesArePathScoped(t *testing.T) {
	value := strings.Join([]string{
		kconfig.KbuildActionRoleToken("target", "cc"),
		"-iquote", kbuildEvalSourceTree + "/arch/x86/include",
		"-I" + kbuildEvalObjectTree + "/arch/x86/include/generated/uapi",
		"-isystem${tree:kernel}/include",
		"-include", kbuildEvalObjectTree + "/include/generated/autoconf.h",
		"-fmacro-prefix-map=" + kbuildEvalObjectTree + "/=.",
		"${tree:prep}/tools/objtool/objtool",
	}, " ")
	want := []string{
		"include/generated/autoconf.h",
		"tools/objtool/objtool",
	}
	if got := kbuildSelectedRecipeObjectTreeReferences(value); !slices.Equal(got, want) {
		t.Fatalf("object-tree references = %q, want %q", got, want)
	}
	if kbuildVisibleArtifactMatchesObjectTreeReferences("scripts/mod/file2alias.o", want) {
		t.Fatal("unrelated host object matched generated include/tool references")
	}
	plans, err := kbuildSelectedRecipeIncludeSearchReferences(kconfig.CompactKbuildProfile{}, value, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("include search plans = %#v, want one compiler command", plans)
	}
	plan := plans[0]
	wantDirectories := []kbuildIncludeSearchDirectory{
		{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include", source: true}, quoteOnly: true},
		{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include/generated/uapi"}},
		{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "include", source: true}},
	}
	if !slices.Equal(plan.directories, wantDirectories) {
		t.Fatalf("include search directories = %#v, want %#v", plan.directories, wantDirectories)
	}
	if wantFiles := []kbuildIncludeTreePath{{path: "include/generated/autoconf.h"}}; !slices.Equal(plan.forced, wantFiles) {
		t.Fatalf("forced compiler inputs = %#v, want %#v", plan.forced, wantFiles)
	}
}

func TestKbuildCompilerIncludeSearchPlanUsesTypedInvocationLocation(t *testing.T) {
	for _, test := range []struct {
		name   string
		tree   kconfig.CompactKbuildInvocationTree
		source bool
	}{
		{name: "source", tree: kconfig.CompactKbuildInvocationSourceTree, source: true},
		{name: "object", tree: kconfig.CompactKbuildInvocationObjectTree},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := kconfig.CompactKbuildProfile{}
			if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
				Tree: test.tree, Directory: "arch/x86/kernel",
			}); err != nil {
				t.Fatal(err)
			}
			value := strings.Join([]string{
				kconfig.KbuildActionRoleToken("target", "cc"),
				"-I.", "-iquote", "../include", "-I=toolchain/include",
				"-include", "../../include/generated/autoconf.h",
			}, " ")
			plan, err := kbuildCompilerIncludeSearchPlan(profile, "cc", value, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantDirectories := []kbuildIncludeSearchDirectory{
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include", source: test.source}, quoteOnly: true},
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/kernel", source: test.source}},
			}
			if !slices.Equal(plan.directories, wantDirectories) {
				t.Fatalf("include directories = %#v, want %#v", plan.directories, wantDirectories)
			}
			wantForced := []kbuildIncludeTreePath{{
				path: "arch/include/generated/autoconf.h", source: test.source,
				searchAfterDirect: true, searchName: "../../include/generated/autoconf.h",
			}}
			if !slices.Equal(plan.forced, wantForced) {
				t.Fatalf("forced includes = %#v, want %#v", plan.forced, wantForced)
			}
		})
	}
}

func TestKbuildCompilerIncludeSearchPlanAllowsTreeRootAndRejectsEscape(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := kbuildCompilerIncludeSearchPlan(
		profile,
		"cc",
		kconfig.KbuildActionRoleToken("target", "cc")+" -I. -I=toolchain/include -include generated/autoconf.h",
		nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.directories, []kbuildIncludeSearchDirectory{{
		kbuildIncludeTreePath: kbuildIncludeTreePath{},
	}}; !slices.Equal(got, want) {
		t.Fatalf("root include directories = %#v, want %#v", got, want)
	}
	if len(plan.forced) != 1 || plan.forced[0].path != "generated/autoconf.h" {
		t.Fatalf("root forced includes = %#v", plan.forced)
	}

	if _, err := kbuildCompilerIncludeSearchPlan(
		profile,
		"cc",
		kconfig.KbuildActionRoleToken("target", "cc")+" -I../escape -include generated/autoconf.h",
		nil, nil,
	); err == nil {
		t.Fatal("include directory escaping its declared object tree was accepted")
	}
}

func TestKbuildCompilerIncludeSearchPlanSelectsOnlyGeneratedTranslationUnits(t *testing.T) {
	compiler := kconfig.KbuildActionRoleToken("target", "cc")
	compile, err := kbuildCompilerIncludeSearchPlan(
		kconfig.CompactKbuildProfile{},
		"cc",
		compiler+" -I__LINUX_BZL_OBJECT_TREE__/include -c -o generated.o generated.c",
		nil, []string{"generated.c", "order-only-tool"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(compile.generatedSources, []string{"generated.c"}) {
		t.Fatalf("compile generated sources = %q, want only exact translation-unit operand", compile.generatedSources)
	}
	link, err := kbuildCompilerIncludeSearchPlan(
		kconfig.CompactKbuildProfile{},
		"cc",
		compiler+" -I__LINUX_BZL_OBJECT_TREE__/include -o host-tool generated.o",
		nil, []string{"generated.o"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(link.generatedSources) != 0 {
		t.Fatalf("compiler link inputs were classified as translation units: %q", link.generatedSources)
	}
}

func TestKbuildBindgenIncludeSearchPlanUsesPostDelimiterClangArguments(t *testing.T) {
	const helper = "rust/bindings/bindings_helper.h"
	profile := kconfig.CompactKbuildProfile{}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	value := strings.Join([]string{
		kconfig.KbuildActionRoleToken("target", "bindgen"),
		helper, "-o", "rust/bindings/bindings_generated.rs",
		"-Iignored-bindgen-option",
		"--",
		"-I" + kbuildEvalSourceTree + "/arch/x86/include",
		"-I" + kbuildEvalObjectTree + "/arch/x86/include/generated/uapi",
		"-include", kbuildEvalObjectTree + "/include/generated/autoconf.h",
	}, " ")
	plan, err := kbuildCompilerIncludeSearchPlan(profile, "bindgen", value, []string{helper}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDirectories := []kbuildIncludeSearchDirectory{
		{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include", source: true}},
		{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include/generated/uapi"}},
	}
	if !slices.Equal(plan.directories, wantDirectories) {
		t.Fatalf("bindgen include directories = %#v, want %#v", plan.directories, wantDirectories)
	}
	if want := []kbuildIncludeTreePath{{path: "include/generated/autoconf.h"}}; !slices.Equal(plan.forced, want) {
		t.Fatalf("bindgen forced includes = %#v, want %#v", plan.forced, want)
	}
	if want := []string{helper}; !slices.Equal(plan.sources, want) {
		t.Fatalf("bindgen translation units = %q, want pre-delimiter source %q", plan.sources, want)
	}
	withoutDelimiter, err := kbuildCompilerIncludeSearchPlan(
		profile, "bindgen", kconfig.KbuildActionRoleToken("target", "bindgen")+" "+helper+" -Iignored", []string{helper}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutDelimiter.directories) != 0 || len(withoutDelimiter.sources) != 0 {
		t.Fatalf("bindgen argv without delimiter was modeled: %#v", withoutDelimiter)
	}
}

func TestKbuildGeneratedSourceScriptInvocationUsesTypedImmutableArguments(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	target := "generated/capflags.c"
	recipe := strings.Join([]string{
		"sh",
		"${tree:kernel}/scripts/generate.sh",
		target,
		"include/features.h",
		"scripts/generate.sh",
		"literal-mode",
	}, " ")
	invocation, recognized, err := kbuildGeneratedSourceScriptInvocationForRecipe(
		profile, target, recipe,
		[]string{"include/features.h", "scripts/generate.sh"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !recognized || invocation.scope != "target" || invocation.script != "scripts/generate.sh" {
		t.Fatalf("generated source-script invocation = (%#v,%t), want target-scoped immutable script", invocation, recognized)
	}
	want := []kconfig.KbuildSourceScriptArgument{
		{Kind: kconfig.KbuildSourceScriptOutputArgument},
		{Kind: kconfig.KbuildSourceScriptSourceArgument, Value: "include/features.h"},
		{Kind: kconfig.KbuildSourceScriptSourceArgument, Value: "scripts/generate.sh"},
		{Kind: kconfig.KbuildSourceScriptLiteralArgument, Value: "literal-mode"},
	}
	if !slices.Equal(invocation.arguments, want) {
		t.Fatalf("generated source-script arguments = %#v, want %#v", invocation.arguments, want)
	}

	generatedRecipe := strings.ReplaceAll(
		recipe,
		"include/features.h",
		"__LINUX_BZL_OBJECT_TREE__/include/features.h",
	)
	if _, recognized, err := kbuildGeneratedSourceScriptInvocationForRecipe(
		profile, target, generatedRecipe,
		[]string{"scripts/generate.sh"}, []string{"include/features.h"},
	); err != nil || recognized {
		t.Fatalf("generated-input script probe = recognized %t, error %v; want conservative rejection", recognized, err)
	}

	for name, optionBearingRecipe := range map[string]string{
		"valued option": strings.Join([]string{
			"sh", "-o", "errexit", "${tree:kernel}/scripts/generate.sh", target,
		}, " "),
		"option terminator": strings.Join([]string{
			"sh", "--", "${tree:kernel}/scripts/generate.sh", target,
		}, " "),
	} {
		t.Run(name, func(t *testing.T) {
			if _, recognized, err := kbuildGeneratedSourceScriptInvocationForRecipe(
				profile, target, optionBearingRecipe,
				[]string{"scripts/generate.sh"}, nil,
			); err != nil || recognized {
				t.Fatalf("option-bearing script probe = recognized %t, error %v; want conservative rejection", recognized, err)
			}
		})
	}
}

func TestKbuildGeneratedSourceScriptInvocationCapturesFinalStdoutRedirection(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	target := "include/generated/syscalls.h"
	recipe := strings.Join([]string{
		"sh",
		"${tree:kernel}/scripts/syscallhdr.sh",
		"--abis", "common,64", "arch/arm64/tools/syscall.tbl",
		">", target,
	}, " ")
	invocation, recognized, err := kbuildGeneratedSourceScriptInvocationForRecipe(
		profile, target, recipe,
		[]string{"scripts/syscallhdr.sh", "arch/arm64/tools/syscall.tbl"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !recognized || invocation.script != "scripts/syscallhdr.sh" {
		t.Fatalf("stdout source-script invocation = (%#v,%t)", invocation, recognized)
	}
	want := []kconfig.KbuildSourceScriptArgument{
		{Kind: kconfig.KbuildSourceScriptStdoutArgument},
		{Kind: kconfig.KbuildSourceScriptLiteralArgument, Value: "--abis"},
		{Kind: kconfig.KbuildSourceScriptLiteralArgument, Value: "common,64"},
		{Kind: kconfig.KbuildSourceScriptSourceArgument, Value: "arch/arm64/tools/syscall.tbl"},
	}
	if !slices.Equal(invocation.arguments, want) {
		t.Fatalf("stdout source-script arguments = %#v, want %#v", invocation.arguments, want)
	}

	for _, unsafe := range []string{
		strings.Replace(recipe, "> "+target, "> other.h", 1),
		strings.Replace(recipe, "> "+target, ">> "+target, 1),
		strings.Replace(recipe, "> "+target, "< arch/arm64/tools/syscall.tbl", 1),
	} {
		if _, recognized, err := kbuildGeneratedSourceScriptInvocationForRecipe(
			profile, target, unsafe,
			[]string{"scripts/syscallhdr.sh", "arch/arm64/tools/syscall.tbl"}, nil,
		); err != nil || recognized {
			t.Errorf("unsafe source-script I/O %q = recognized %t, error %v", unsafe, recognized, err)
		}
	}
}

func TestSelectedKbuildGeneratedContentUsesSelectedPatternStem(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
CONFIG_SHELL := sh
obj := arch/arm64/include/generated/uapi/asm
srctree := __LINUX_BZL_SOURCE_TREE__
syscall_abis_32 += common,32
cmd_systbl = $(CONFIG_SHELL) $(srctree)/scripts/syscalltbl.sh \
	--abis $(subst $(space),$(comma),$(strip $(syscall_abis_$*))) $< $@
all: $(obj)/syscall_table_32.h
$(obj)/syscall_table_%.h: arch/arm64/tools/syscall_32.tbl scripts/syscalltbl.sh FORCE
	$(cmd_systbl)
.PHONY: FORCE
FORCE:
`, map[string]string{
		"arch/arm64/tools/syscall_32.tbl": "0 common read sys_read\n",
		"scripts/syscalltbl.sh":           "#!/bin/sh\n",
	}, "all")
	profile.Name = "root:source-script-pattern-stem"
	_, _, selectedStem, contextErr := kconfig.EvaluateCompactKbuildTargetRuleContext(
		profile, "arch/arm64/include/generated/uapi/asm/syscall_table_32.h",
	)
	if contextErr != nil {
		t.Fatal(contextErr)
	}
	if selectedStem != "32" {
		t.Fatalf("selected syscall pattern stem = %q, want 32; rules=%#v", selectedStem, profile.Rules)
	}

	got := ""
	_, err := selectedKbuildSelectionsWithStatsSourceRootAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"arch/arm64/tools/syscall_32.tbl": true,
			"scripts/syscalltbl.sh":           true,
		},
		nil, filepath.Dir(profile.Path),
		func(_ kconfig.CompactKbuildProfile, target, recipe string, _, _ []string) (string, bool, bool, error) {
			if target == "arch/arm64/include/generated/uapi/asm/syscall_table_32.h" {
				got = recipe
			}
			return "", false, false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--abis common,32",
		"arch/arm64/tools/syscall_32.tbl",
		"arch/arm64/include/generated/uapi/asm/syscall_table_32.h",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("evaluated source-script recipe %q omits %q", got, want)
		}
	}
}

func TestKbuildSelectedSourceRelativeObjectTreeReferencesFollowCheckedInIncludes(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("arch/x86/boot/mkcpustr.c", `
#include "../include/asm/features.h"
#include "../kernel/cpu/capflags.c"
#include <generated/angle.h>
#include GENERATED_MACRO
`)
	write("arch/x86/include/asm/features.h", `
# include \
  "nested.h"
#include "../../../../../outside-source-root.h"
`)
	write("arch/x86/include/asm/nested.h", "#define NESTED 1\n")

	profile := kconfig.CompactKbuildProfile{}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "arch/x86/kernel/cpu/capflags.c", Profile: "producer", Target: "arch/x86/kernel/cpu/capflags.c"},
		{Path: "generated/angle.h", Profile: "producer", Target: "generated/angle.h"},
		{Path: "unrelated/generated.h", Profile: "producer", Target: "unrelated/generated.h"},
	})
	got, err := kbuildSelectedSourceRelativeObjectTreeReferences(
		root,
		profile,
		[]string{"arch/x86/boot/mkcpustr.c"},
		map[string]bool{
			"arch/x86/boot/mkcpustr.c":        true,
			"arch/x86/include/asm/features.h": true,
			"arch/x86/include/asm/nested.h":   true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"arch/x86/kernel/cpu/capflags.c"}
	if !slices.Equal(got, want) {
		t.Fatalf("source-relative generated references = %q, want %q", got, want)
	}
}

func TestKbuildQuotedIncludeIndexReusesSharedSourceReads(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("first.c", `#include "shared/wrapper.h"`)
	write("second.c", `#include "shared/wrapper.h"`)
	write("shared/wrapper.h", `#include "../generated/out.h"`)

	index := newKbuildQuotedIncludeIndex(root)
	satisfied := map[string]bool{
		"first.c":          true,
		"second.c":         true,
		"shared/wrapper.h": true,
	}
	want := []string{"generated/out.h"}
	for _, prerequisite := range []string{"first.c", "second.c"} {
		resolution, err := index.references(
			kconfig.CompactKbuildProfile{},
			[]kbuildIncludeSearchPlan{{
				sources: []string{prerequisite},
				directories: []kbuildIncludeSearchDirectory{{
					kbuildIncludeTreePath: kbuildIncludeTreePath{path: "shared"},
				}},
			}},
			satisfied,
			func(string) bool { return true },
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		got := resolution.references
		if !slices.Equal(got, want) {
			t.Fatalf("references for %q = %q, want %q", prerequisite, got, want)
		}
	}
	if got, wantLoads := index.loads, 4; got != wantLoads {
		t.Fatalf("quoted-include source loads = %d, want %d with shared wrapper and missing output cached", got, wantLoads)
	}
}

func TestKbuildQuotedIncludeIndexBoundsOpaqueGeneratedSourcesToObjectIncludeRoots(t *testing.T) {
	index := newKbuildQuotedIncludeIndex(t.TempDir())
	resolution, err := index.references(
		kconfig.CompactKbuildProfile{},
		[]kbuildIncludeSearchPlan{{
			generatedSources: []string{"generated/translation-unit.c"},
			directories: []kbuildIncludeSearchDirectory{
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "include", source: true}},
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "arch/x86/include/generated"}},
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "include/generated"}},
			},
		}},
		nil,
		func(string) bool { return true },
		func(string) (kbuildGeneratedIncludeProjection, bool, error) {
			return kbuildGeneratedIncludeProjection{}, false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.references) != 0 || resolution.objectAllVisible || !slices.Equal(
		resolution.objectDirectories,
		[]string{"arch/x86/include/generated", "generated", "include/generated"},
	) {
		t.Fatalf("opaque generated-source include resolution = %#v, want only sorted object include roots", resolution)
	}
}

func TestKbuildQuotedIncludeIndexBoundsOpaqueGeneratedIncludesToObjectIncludeRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "consumer.c"), []byte("#include <opaque.h>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := newKbuildQuotedIncludeIndex(root)
	resolution, err := index.references(
		kconfig.CompactKbuildProfile{},
		[]kbuildIncludeSearchPlan{{
			sources: []string{"consumer.c"},
			directories: []kbuildIncludeSearchDirectory{
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "source/include", source: true}},
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "generated"}},
				{kbuildIncludeTreePath: kbuildIncludeTreePath{path: "include/generated"}},
			},
		}},
		map[string]bool{"consumer.c": true},
		func(candidate string) bool { return candidate == "generated/opaque.h" },
		func(candidate string) (kbuildGeneratedIncludeProjection, bool, error) {
			if candidate != "generated/opaque.h" {
				t.Fatalf("generated include source = %q, want generated/opaque.h", candidate)
			}
			return kbuildGeneratedIncludeProjection{}, false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolution.references, []string{"generated/opaque.h"}) ||
		resolution.objectAllVisible || !slices.Equal(
		resolution.objectDirectories,
		[]string{"generated", "include/generated"},
	) {
		t.Fatalf("opaque discovered-include resolution = %#v, want exact include and bounded object roots", resolution)
	}
}

func TestSelectedKbuildSelectionsTreatResolvedConfigAsCompilerIncludeBaseline(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/crc/gen_crc32table.c", `#include "../../include/generated/autoconf.h"`)
	write("unrelated.in", "#define UNRELATED 1\n")
	makefile := filepath.Join(root, "Makefile")
	write("Makefile", `
all: lib/crc/gen_crc32table
include/generated/unrelated.h: unrelated.in
	sed 's/^//' $< > $@
lib/crc/gen_crc32table: lib/crc/gen_crc32table.c include/generated/unrelated.h
	$(HOSTCC) -I$(objtree)/lib/crc -o $@ $<
`)
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		RootDir: root,
		Variables: map[string]string{
			"objtree": kbuildEvalObjectTree,
			"srctree": kbuildEvalSourceTree,
		},
		CommandLineVariables: map[string]string{
			"HOSTCC": kconfig.KbuildActionRoleToken("host", "cc"),
		},
		SourceRoots: map[string]string{
			kbuildEvalSourceTree: root,
			kbuildEvalObjectTree: root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("resolved-config-include", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	setTestKbuildInvocationLocation(t, &profile)
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"lib/crc/gen_crc32table.c": true, "unrelated.in": true},
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "lib/crc/gen_crc32table")
	if consumer.Scope != "host" || consumer.Stage != "host" || !consumer.UsesInitialObjectTree {
		t.Fatalf("resolved-config include consumer = %#v, want host action using config baseline", consumer)
	}
	if consumer.InitialObjectTreeArtifacts != "" || consumer.GeneratedObjectTreeArtifacts != "" {
		t.Fatalf("resolved-config baseline was encoded as an ordinary Kbuild artifact: %#v", consumer)
	}
}

func TestSelectedKbuildSelectionsMoveSourceRelativeGeneratedIncludeBeforeHost(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("arch/x86/boot/mkcpustr.c", `#include "../kernel/cpu/capflags.c"`)
	write("arch/x86/kernel/cpu/features.h", "#define FEATURE 1\n")
	write("arch/x86/boot/compressed/vmlinux.lds.S", "SECTIONS {}\n")
	write("arch/x86/boot/compressed/mkpiggy.c", "int main(void) { return 0; }\n")
	write("unrelated.in", "unrelated\n")
	makefile := filepath.Join(root, "Makefile")
	write("Makefile", `
all: arch/x86/kernel/cpu/capflags.c unrelated.out arch/x86/boot/compressed/vmlinux.lds arch/x86/boot/compressed/piggy.S arch/x86/boot/mkcpustr
arch/x86/kernel/cpu/capflags.c: arch/x86/kernel/cpu/features.h
	sed 's/^//' $< > $@
unrelated.out: unrelated.in
	cp $< $@
arch/x86/boot/compressed/vmlinux.lds: arch/x86/boot/compressed/vmlinux.lds.S
	sed 's/^//' $< > $@
arch/x86/boot/compressed/mkpiggy: arch/x86/boot/compressed/mkpiggy.c
	$(HOSTCC) -o $@ $<
arch/x86/boot/compressed/piggy.S: arch/x86/boot/compressed/mkpiggy
	$(objtree)/arch/x86/boot/compressed/mkpiggy > $@
arch/x86/boot/mkcpustr: arch/x86/boot/mkcpustr.c
arch/x86/boot/%:
	$(HOSTCC) -I$(objtree)/arch/x86/boot -o $@ $<
`)
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		RootDir: root,
		Variables: map[string]string{
			"objtree": kbuildEvalObjectTree,
			"srctree": kbuildEvalSourceTree,
		},
		CommandLineVariables: map[string]string{
			"HOSTCC": kconfig.KbuildActionRoleToken("host", "cc"),
		},
		SourceRoots: map[string]string{
			kbuildEvalSourceTree: root,
			kbuildEvalObjectTree: root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("generated-include", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "unrelated.out", Profile: profile.Name, Target: "unrelated.out"},
	})
	setTestKbuildInvocationLocation(t, &profile)
	satisfied := map[string]bool{
		"arch/x86/boot/mkcpustr.c":               true,
		"arch/x86/boot/compressed/mkpiggy.c":     true,
		"arch/x86/boot/compressed/vmlinux.lds.S": true,
		"arch/x86/kernel/cpu/features.h":         true,
		"unrelated.in":                           true,
	}
	rules := kbuildProfileRulesForTargetIndexed(profile, newKbuildProfileTargetIndex(profile, nil), "arch/x86/boot/mkcpustr", satisfied)
	if len(rules) != 2 || len(rules[1].Prerequisites) != 0 || len(rules[1].Recipe) == 0 {
		t.Fatalf("raw selected mkcpustr rules = %#v, want prerequisite-free implicit recipe", rules)
	}
	normal, orderOnly, stem, err := kconfig.EvaluateCompactKbuildTargetRuleContext(profile, "arch/x86/boot/mkcpustr")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"arch/x86/boot/mkcpustr.c"}; !slices.Equal(normal, want) || len(orderOnly) != 0 || stem != "mkcpustr" {
		t.Fatalf("evaluated mkcpustr context = normal %q order-only %q stem %q, want %q, none, mkcpustr", normal, orderOnly, stem, want)
	}
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		satisfied,
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	producer := selectionByTarget(t, selections, "arch/x86/kernel/cpu/capflags.c")
	if producer.Scope != "host" || producer.Stage != "host" {
		t.Fatalf("generated include producer = %#v, want host-visible neutral producer", producer)
	}
	consumer := selectionByTarget(t, selections, "arch/x86/boot/mkcpustr")
	wantArtifacts := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "arch/x86/kernel/cpu/capflags.c", Profile: profile.Name, Target: "arch/x86/kernel/cpu/capflags.c",
	}})
	if !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" || consumer.GeneratedObjectTreeArtifacts != wantArtifacts {
		t.Fatalf("host generated-include consumer = %#v, want only generated source artifact %q", consumer, wantArtifacts)
	}
	for _, independent := range []string{"arch/x86/boot/compressed/vmlinux.lds", "arch/x86/boot/compressed/piggy.S", "arch/x86/boot/compressed/mkpiggy"} {
		if strings.Contains(consumer.GeneratedObjectTreeArtifacts, independent) {
			t.Fatalf("host generated-include consumer bound unordered compressed sibling %q: %#v", independent, consumer)
		}
	}
	unrelated := selectionByTarget(t, selections, "unrelated.out")
	if unrelated.Scope != "target" || unrelated.Stage != "target" {
		t.Fatalf("unrelated producer = %#v, want target default", unrelated)
	}
}

func TestSelectedKbuildSelectionsBindGeneratedObjectTreeDirectoryOutputs(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
prepare: arch/x86/include/generated/uapi/asm/unistd_64.h arch/x86/include/generated/uapi/mktool arch/x86/kernel/asm-offsets.s unrelated.out
arch/x86/include/generated/uapi/asm/unistd_64.h: syscall.tbl
	cp $< $@
arch/x86/include/generated/uapi/mktool: tool.c
	$(HOSTCC) -o $@ $<
arch/x86/kernel/asm-offsets.s: arch/x86/kernel/asm-offsets.c
	$(CC) -I__LINUX_BZL_SOURCE_TREE__/include -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi -S -o $@ $<
unrelated.out: unrelated.in
	cp $< $@
.PHONY: prepare
`, map[string]string{
		"arch/x86/kernel/asm-offsets.c": "#include <linux/wrapper.h>\n",
		"include/linux/wrapper.h":       "#include <asm/unistd_64.h>\n",
		"syscall.tbl":                   "syscall\n",
		"tool.c":                        "int main(void) { return 0; }\n",
		"unrelated.in":                  "unrelated\n",
	}, "prepare")
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{{
		Path: "unrelated.out", Profile: profile.Name, Target: "unrelated.out",
	}})
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"arch/x86/kernel/asm-offsets.c": true,
			"syscall.tbl":                   true,
			"tool.c":                        true,
			"unrelated.in":                  true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "arch/x86/kernel/asm-offsets.s")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "arch/x86/include/generated/uapi/asm/unistd_64.h", Profile: profile.Name,
		Target: "arch/x86/include/generated/uapi/asm/unistd_64.h",
	}})
	if !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" || consumer.GeneratedObjectTreeArtifacts != want {
		t.Fatalf("generated include-directory consumer = %#v, want exact generated artifact %q", consumer, want)
	}
	unrelated := selectionByTarget(t, selections, "unrelated.out")
	if unrelated.GeneratedObjectTreeArtifacts != "" {
		t.Fatalf("unrelated output gained generated object-tree inputs: %#v", unrelated)
	}
}

func TestSelectedKbuildSelectionsBindBindgenNestedGeneratedHeader(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
prepare: arch/x86/include/generated/uapi/asm/unistd_64.h rust/bindings/bindings_generated.rs unrelated.out
arch/x86/include/generated/uapi/asm/unistd_64.h: syscall.tbl
	cp $< $@
rust/bindings/bindings_generated.rs: rust/bindings/bindings_helper.h
	$(BINDGEN) $< -o $@ -- -I__LINUX_BZL_SOURCE_TREE__/arch/x86/include -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi
unrelated.out: unrelated.in
	cp $< $@
.PHONY: prepare
`, map[string]string{
		"arch/x86/include/asm/unistd.h":   "#include <asm/unistd_64.h>\n",
		"rust/bindings/bindings_helper.h": "#include <asm/unistd.h>\n",
		"syscall.tbl":                     "syscall\n",
		"unrelated.in":                    "unrelated\n",
	}, "prepare")
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{{
		Path: "unrelated.out", Profile: profile.Name, Target: "unrelated.out",
	}})
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"arch/x86/include/asm/unistd.h":   true,
			"rust/bindings/bindings_helper.h": true,
			"syscall.tbl":                     true,
			"unrelated.in":                    true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "rust/bindings/bindings_generated.rs")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "arch/x86/include/generated/uapi/asm/unistd_64.h", Profile: profile.Name,
		Target: "arch/x86/include/generated/uapi/asm/unistd_64.h",
	}})
	if !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" || consumer.GeneratedObjectTreeArtifacts != want {
		t.Fatalf("bindgen generated-header consumer = %#v, want exact generated artifact %q", consumer, want)
	}
	if strings.Contains(consumer.GeneratedObjectTreeArtifacts, "unrelated.out") {
		t.Fatalf("bindgen consumer gained unrelated prep output: %#v", consumer)
	}
}

func TestSelectedKbuildSelectionsDoNotBindFutureInvocationGeneratedInclude(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:future-generated-include"
	early := selectionRoleProfileWithSources(t, `
early.s: early.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/include/generated -S -o $@ $<
`, map[string]string{
		"early.c": "#if 0\n#include <future.h>\n#endif\nint early;\n",
	}, "early.s")
	early.Name = "child:early-compiler"
	later := selectionRoleProfile(t, `
include/generated/future.h: future.in
	cp $< $@
`, "include/generated/future.h")
	later.Name = "child:later-header"
	later.InvocationPredecessors = []string{early.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: early.Name, Goals: early.EntryTargets},
		{Target: "all", Profile: later.Name, Goals: later.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, early, later},
		map[string]bool{"early.c": true, "future.in": true},
		filepath.Dir(early.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionByTarget(t, selections, "early.s")
	if selection.GeneratedObjectTreeArtifacts != "" {
		t.Fatalf("earlier compiler action bound a generated include from a later invocation: %#v", selection)
	}
}

func TestSelectedKbuildSelectionsBindEarlierInvocationGeneratedInclude(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:earlier-generated-include"
	early := selectionRoleProfile(t, `
include/generated/earlier.h: earlier.in
	cp $< $@
`, "include/generated/earlier.h")
	early.Name = "child:earlier-header"
	later := selectionRoleProfileWithSources(t, `
later.s: later.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/include/generated -S -o $@ $<
`, map[string]string{
		"later.c": "#include <earlier.h>\nint later;\n",
	}, "later.s")
	later.Name = "child:later-compiler"
	later.InvocationPredecessors = []string{early.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: early.Name, Goals: early.EntryTargets},
		{Target: "all", Profile: later.Name, Goals: later.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, early, later},
		map[string]bool{"earlier.in": true, "later.c": true},
		filepath.Dir(later.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionByTarget(t, selections, "later.s")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "include/generated/earlier.h", Profile: early.Name, Target: "include/generated/earlier.h",
	}})
	if selection.GeneratedObjectTreeArtifacts != want {
		t.Fatalf("later compiler generated include = %q, want earlier invocation artifact %q", selection.GeneratedObjectTreeArtifacts, want)
	}
}

func TestKbuildSelectedRecipeSourceProjectionFailsClosedAroundHeaderInstall(t *testing.T) {
	const target = "generated/sdk/include/source.h"
	for _, test := range []struct {
		name       string
		recipe     string
		wantSource string
		wantExact  bool
		wantWrites bool
		wantOpaque bool
	}{
		{
			name: "upstream conditional install",
			recipe: "printf '  INSTALL %s\\n' 'generated/sdk/include/source.h'; " +
				"if [ ! -d 'generated/sdk/include' ]; then install -d -m 755 'generated/sdk/include'; fi; " +
				"install source.h -m 644 'generated/sdk/include'",
			wantSource: "source.h", wantExact: true, wantWrites: true,
		},
		{
			name:       "copy then opaque transform",
			recipe:     "cp source.h 'generated/sdk/include/source.h'; strip 'generated/sdk/include/source.h'",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "copy then source executable named strip",
			recipe:     "cp source.h 'generated/sdk/include/source.h'; tools/strip 'generated/sdk/include/source.h'",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "copy in conditional compound",
			recipe:     "cp source.h 'generated/sdk/include/source.h' && printf done",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "copy in pipeline",
			recipe:     "cp source.h 'generated/sdk/include/source.h' | printf done",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "copy with malformed directory setup",
			recipe:     "mkdir --unknown generated/sdk/include; cp source.h 'generated/sdk/include/source.h'",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "redirected diagnostic can mutate source",
			recipe:     "printf changed > source.h; cp source.h 'generated/sdk/include/source.h'",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "copy with directory setup at target",
			recipe:     "mkdir -p 'generated/sdk/include/source.h'; cp source.h 'generated/sdk/include/source.h'",
			wantWrites: true, wantOpaque: true,
		},
		{
			name:       "unrelated opaque command without projection",
			recipe:     "opaque-tool source.h",
			wantOpaque: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, exact, writes, opaque := kbuildSelectedRecipeSourceProjection(test.recipe, target, []string{"source.h"})
			if source != test.wantSource || exact != test.wantExact || writes != test.wantWrites || opaque != test.wantOpaque {
				t.Fatalf(
					"kbuildSelectedRecipeSourceProjection(%q) = (%q, %t, %t, %t), want (%q, %t, %t, %t)",
					test.recipe, source, exact, writes, opaque,
					test.wantSource, test.wantExact, test.wantWrites, test.wantOpaque,
				)
			}
		})
	}
}

func TestKbuildSelectedRecipeSourceProjectionMatchesUpstreamLibbpfInstall(t *testing.T) {
	const (
		target = "tools/bpf/resolve_btfids/libbpf/include/bpf/libbpf_common.h"
		source = "tools/lib/bpf/libbpf_common.h"
		recipe = "printf '  INSTALL %s\\n' ${tree:prep}/tools/bpf/resolve_btfids/libbpf//include/bpf/libbpf_common.h; " +
			"if [ ! -d ''${tree:prep}/tools/bpf/resolve_btfids/libbpf/'/include/bpf' ]; then " +
			"install -d -m 755 ''${tree:prep}/tools/bpf/resolve_btfids/libbpf/'/include/bpf'; fi; " +
			"install -m 644 tools/lib/bpf/libbpf_common.h ''${tree:prep}/tools/bpf/resolve_btfids/libbpf/'/include/bpf'"
	)
	projection, exact, writes, opaque := kbuildSelectedRecipeSourceProjection(recipe, target, []string{source})
	if projection != source || !exact || !writes || opaque {
		t.Fatalf(
			"upstream libbpf install projection = (%q, %t, %t, %t), want (%q, true, true, false)",
			projection, exact, writes, opaque, source,
		)
	}
}

func TestSelectedKbuildSelectionsFollowEarlierGeneratedCompilerHeaderCopyAliases(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:generated-header-copy-alias"
	earlier := selectionRoleProfileWithSources(t, `
generated/sdk/include/public.h: upstream/include/public.h
	cp $< $@
generated/sdk/include/peer.h: upstream/include/peer.h
	cp $< $@
generated/sdk/include/unrelated.h: upstream/include/unrelated.h
	cp $< $@
generated/sdk/include/rendered.h: upstream/include/rendered.in
	sed '/DROP/d' $< > $@
generated/other/include/peer.h: upstream/include/peer.h
	cp $< $@
`, map[string]string{
		"upstream/include/public.h":    "#include \"peer.h\"\n#define PUBLIC 1\n",
		"upstream/include/peer.h":      "#define PEER 1\n",
		"upstream/include/unrelated.h": "#define UNRELATED 1\n",
		"upstream/include/rendered.in": "#define DROP 1\n",
	},
		"generated/sdk/include/public.h",
		"generated/sdk/include/peer.h",
		"generated/sdk/include/unrelated.h",
		"generated/sdk/include/rendered.h",
		"generated/other/include/peer.h",
	)
	earlier.Name = "child:earlier-header-installs"
	consumer := selectionRoleProfileWithSources(t, `
consumer.o: consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/sdk/include -c -o $@ $<
`, map[string]string{
		"consumer.c": "#include <public.h>\nint consumer;\n",
	}, "consumer.o")
	consumer.Name = "child:later-compiler"
	consumer.InvocationPredecessors = []string{earlier.Name}
	future := selectionRoleProfile(t, `
generated/sdk/include/future.h: future.in
	cp $< $@
`, "generated/sdk/include/future.h")
	future.Name = "child:future-header"
	future.InvocationPredecessors = []string{consumer.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlier.Name, Goals: earlier.EntryTargets},
		{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
		{Target: "all", Profile: future.Name, Goals: future.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, earlier, consumer, future},
		map[string]bool{
			"upstream/include/public.h":    true,
			"upstream/include/peer.h":      true,
			"upstream/include/unrelated.h": true,
			"upstream/include/rendered.in": true,
			"consumer.c":                   true,
			"future.in":                    true,
		},
		filepath.Dir(consumer.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{
			Path: "generated/sdk/include/public.h", Profile: earlier.Name,
			Target: "generated/sdk/include/public.h",
		},
		{
			Path: "generated/sdk/include/peer.h", Profile: earlier.Name,
			Target: "generated/sdk/include/peer.h",
		},
	})
	if selection.GeneratedObjectTreeArtifacts != want {
		t.Fatalf(
			"later compiler generated-header copy aliases = %q, want exact earlier peer artifacts %q",
			selection.GeneratedObjectTreeArtifacts,
			want,
		)
	}
	if !selection.UsesInitialObjectTree || selection.InitialObjectTreeArtifacts != "" {
		t.Fatalf("later compiler copy-alias selection = %#v, want generated-only object-tree inputs", selection)
	}
}

func TestSelectedKbuildSelectionsConcretizePatternCopyAliasTemplates(t *testing.T) {
	for _, test := range []struct {
		name      string
		rule      string
		copyInput string
		orderOnly bool
	}{
		{
			name:      "implicit pattern normal",
			rule:      "generated/sdk/include/%.h: upstream/include/%.h",
			copyInput: "$<",
		},
		{
			name:      "implicit pattern order-only",
			rule:      "generated/sdk/include/%.h: | upstream/include/%.h",
			copyInput: "$|",
			orderOnly: true,
		},
		{
			name:      "static pattern normal",
			rule:      "generated/sdk/include/public.h generated/sdk/include/peer.h: generated/sdk/include/%.h: upstream/include/%.h",
			copyInput: "$<",
		},
		{
			name:      "static pattern order-only",
			rule:      "generated/sdk/include/public.h generated/sdk/include/peer.h: generated/sdk/include/%.h: | upstream/include/%.h",
			copyInput: "$|",
			orderOnly: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := selectionRoleProfileWithSources(t, `
if_changed = $(cmd_$(1))
cmd_install = cp `+test.copyInput+` $@
all: generated/sdk/include/public.h generated/sdk/include/peer.h consumer.o
`+test.rule+`
	$(call if_changed,install)
consumer.o: consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/sdk/include -c -o $@ $<
`, map[string]string{
				"upstream/include/public.h": "#include \"peer.h\"\n#define PUBLIC 1\n",
				"upstream/include/peer.h":   "#define PEER 1\n",
				"consumer.c":                "#include <public.h>\nint consumer;\n",
			}, "all")
			profile.Name = "root:pattern-copy-alias"
			if profile.Directory != "" {
				t.Fatalf("pattern fixture logical invocation directory = %q, want source root", profile.Directory)
			}
			satisfied := map[string]bool{
				"upstream/include/public.h": true,
				"upstream/include/peer.h":   true,
				"consumer.c":                true,
			}
			for _, target := range []string{
				"generated/sdk/include/public.h",
				"generated/sdk/include/peer.h",
			} {
				rules := kbuildProfileRulesForTargetIndexed(
					profile, newKbuildProfileTargetIndex(profile, nil), target, satisfied,
				)
				if !slices.ContainsFunc(rules, func(rule kconfig.KbuildRule) bool {
					return len(rule.Recipe) != 0 &&
						(slices.Contains(rule.Prerequisites, "upstream/include/%.h") ||
							slices.Contains(rule.OrderOnly, "upstream/include/%.h"))
				}) {
					t.Fatalf("raw selected rules for %q = %#v, want recipe with pattern prerequisite", target, rules)
				}
				normal, orderOnly, stem, err := kconfig.EvaluateCompactKbuildTargetRuleContext(profile, target)
				if err != nil {
					t.Fatal(err)
				}
				wantSource := strings.Replace(target, "generated/sdk", "upstream", 1)
				wantNormal := []string{wantSource}
				wantOrderOnly := []string{}
				if test.orderOnly {
					wantNormal, wantOrderOnly = nil, wantNormal
				}
				if !slices.Equal(normal, wantNormal) || !slices.Equal(orderOnly, wantOrderOnly) || stem == "" {
					t.Fatalf(
						"evaluated context for %q = normal %q order-only %q stem %q, want %q, %q, nonempty",
						target, normal, orderOnly, stem, wantNormal, wantOrderOnly,
					)
				}
			}

			selections, err := selectedKbuildSelectionsFromSourceRoot(
				[]kconfig.CompactKbuildProfile{profile}, satisfied, filepath.Dir(profile.Path),
			)
			if err != nil {
				t.Fatal(err)
			}
			consumer := selectionByTarget(t, selections, "consumer.o")
			want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
				{
					Path: "generated/sdk/include/public.h", Profile: profile.Name,
					Target: "generated/sdk/include/public.h",
				},
				{
					Path: "generated/sdk/include/peer.h", Profile: profile.Name,
					Target: "generated/sdk/include/peer.h",
				},
			})
			if consumer.GeneratedObjectTreeArtifacts != want || !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" {
				t.Fatalf(
					"pattern copy-alias compiler selection = %#v, want generated public and quoted peer artifacts %q",
					consumer, want,
				)
			}
		})
	}
}

func TestSelectedKbuildSelectionsFollowLiteralGeneratedHeaderIncludes(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
if_changed = $(cmd_$(1))
cmd_wrap = echo "\#include <asm-generic/$*.h>" > $@
all: arch/x86/include/generated/uapi/asm/public.h arch/x86/include/generated/uapi/asm/peer.h arch/x86/include/generated/uapi/asm/unrelated.h consumer.o
arch/x86/include/generated/uapi/asm/public.h arch/x86/include/generated/uapi/asm/peer.h arch/x86/include/generated/uapi/asm/unrelated.h: arch/x86/include/generated/uapi/asm/%.h: include/uapi/asm-generic/%.h
	$(call if_changed,wrap)
consumer.o: consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi -I__LINUX_BZL_SOURCE_TREE__/include/uapi -c -o $@ $<
`, map[string]string{
		"consumer.c":                           "#include <asm/public.h>\nint consumer;\n",
		"include/uapi/asm-generic/public.h":    "#include <asm/peer.h>\n#define PUBLIC 1\n",
		"include/uapi/asm-generic/peer.h":      "#define PEER 1\n",
		"include/uapi/asm-generic/unrelated.h": "#define UNRELATED 1\n",
	}, "all")
	profile.Name = "root:literal-generated-header"
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"consumer.c":                           true,
			"include/uapi/asm-generic/public.h":    true,
			"include/uapi/asm-generic/peer.h":      true,
			"include/uapi/asm-generic/unrelated.h": true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{
			Path: "arch/x86/include/generated/uapi/asm/public.h", Profile: profile.Name,
			Target: "arch/x86/include/generated/uapi/asm/public.h",
		},
		{
			Path: "arch/x86/include/generated/uapi/asm/peer.h", Profile: profile.Name,
			Target: "arch/x86/include/generated/uapi/asm/peer.h",
		},
	})
	if consumer.GeneratedObjectTreeArtifacts != want || !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" {
		t.Fatalf(
			"literal generated-header compiler selection = %#v, want exact public and transitive peer artifacts %q",
			consumer, want,
		)
	}
}

func TestSelectedKbuildSelectionsRejectLiteralGeneratedHeaderRewrittenLater(t *testing.T) {
	for _, test := range []struct {
		name           string
		templateSuffix string
		rewrite        string
	}{
		{
			name:    "later opaque rewrite",
			rewrite: "\topaque-filter $@\n",
		},
		{
			name:    "conflicting literal rewrite",
			rewrite: "\techo \"\\#include <asm-generic/unrelated.h>\" > $@\n",
		},
		{
			name:           "same-line opaque rewrite after command template",
			templateSuffix: "; opaque-filter $@",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := selectionRoleProfileWithSources(t, `
if_changed = $(cmd_$(1))
cmd_wrap = echo "\#include <asm-generic/$*.h>" > $@
all: arch/x86/include/generated/uapi/asm/public.h arch/x86/include/generated/uapi/asm/peer.h arch/x86/include/generated/uapi/asm/unrelated.h consumer.o
arch/x86/include/generated/uapi/asm/public.h arch/x86/include/generated/uapi/asm/peer.h arch/x86/include/generated/uapi/asm/unrelated.h: arch/x86/include/generated/uapi/asm/%.h: include/uapi/asm-generic/%.h
	$(call if_changed,wrap)`+test.templateSuffix+`
`+test.rewrite+`consumer.o: consumer.c arch/x86/include/generated/uapi/asm/peer.h arch/x86/include/generated/uapi/asm/unrelated.h
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi -I__LINUX_BZL_SOURCE_TREE__/include/uapi -c -o $@ $<
`, map[string]string{
				"consumer.c":                           "#include <asm/public.h>\nint consumer;\n",
				"include/uapi/asm-generic/public.h":    "#include <asm/peer.h>\n#define PUBLIC 1\n",
				"include/uapi/asm-generic/peer.h":      "#define PEER 1\n",
				"include/uapi/asm-generic/unrelated.h": "#define UNRELATED 1\n",
			}, "all")
			profile.Name = "root:rewritten-literal-generated-header"
			selections, err := selectedKbuildSelectionsFromSourceRoot(
				[]kconfig.CompactKbuildProfile{profile},
				map[string]bool{
					"consumer.c":                           true,
					"include/uapi/asm-generic/public.h":    true,
					"include/uapi/asm-generic/peer.h":      true,
					"include/uapi/asm-generic/unrelated.h": true,
				},
				filepath.Dir(profile.Path),
			)
			if err != nil {
				t.Fatal(err)
			}
			consumer := selectionByTarget(t, selections, "consumer.o")
			want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
				{
					Path: "arch/x86/include/generated/uapi/asm/public.h", Profile: profile.Name,
					Target: "arch/x86/include/generated/uapi/asm/public.h",
				},
				{
					Path: "arch/x86/include/generated/uapi/asm/peer.h", Profile: profile.Name,
					Target: "arch/x86/include/generated/uapi/asm/peer.h",
				},
				{
					Path: "arch/x86/include/generated/uapi/asm/unrelated.h", Profile: profile.Name,
					Target: "arch/x86/include/generated/uapi/asm/unrelated.h",
				},
			})
			if consumer.GeneratedObjectTreeArtifacts != want || !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" {
				t.Fatalf(
					"later-rewritten literal generated-header compiler selection = %#v, want bounded selected include-root closure %q",
					consumer, want,
				)
			}
		})
	}
}

func TestSelectedKbuildSelectionsFollowIncludesFromProjectedGeneratedCompilerSource(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
if_changed = $(cmd_$(1))
cmd_copy = cat $< > $@
cmd_wrap = echo "\#include <asm-generic/$*.h>" > $@
all: arch/x86/include/generated/uapi/asm/types.h drivers/tty/vt/defkeymap.c consumer.o
arch/x86/include/generated/uapi/asm/types.h: arch/x86/include/generated/uapi/asm/%.h: include/uapi/asm-generic/%.h
	$(call if_changed,wrap)
drivers/tty/vt/defkeymap.c: drivers/tty/vt/defkeymap.c_shipped
	$(call if_changed,copy)
consumer.o: drivers/tty/vt/defkeymap.c
	$(CC) -I__LINUX_BZL_SOURCE_TREE__/arch/x86/include/uapi -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi -I__LINUX_BZL_SOURCE_TREE__/include/uapi -I__LINUX_BZL_OBJECT_TREE__/include/generated/uapi -c -o $@ $<
`, map[string]string{
		"drivers/tty/vt/defkeymap.c_shipped": "#include <linux/types.h>\nint generated_source;\n",
		"include/uapi/linux/types.h":         "#include <asm/types.h>\n",
		"include/uapi/asm-generic/types.h":   "#define PROJECTED_TYPES 1\n",
	}, "all")
	profile.Name = "root:projected-generated-compiler-source"
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"drivers/tty/vt/defkeymap.c_shipped": true,
			"include/uapi/linux/types.h":         true,
			"include/uapi/asm-generic/types.h":   true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "arch/x86/include/generated/uapi/asm/types.h", Profile: profile.Name,
		Target: "arch/x86/include/generated/uapi/asm/types.h",
	}})
	if consumer.GeneratedObjectTreeArtifacts != want || !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" {
		t.Fatalf(
			"projected generated-source compiler selection = %#v, want exact generated types wrapper %q",
			consumer, want,
		)
	}
}

func TestSelectedKbuildSelectionsRejectCopyAliasRewrittenByLaterRecipeLine(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: generated/sdk/include/public.h generated/sdk/include/peer.h consumer.o
generated/sdk/include/public.h: upstream/include/public.h
	cp $< $@
	opaque-filter $@
generated/sdk/include/peer.h: upstream/include/peer.h
	cp $< $@
consumer.o: consumer.c generated/sdk/include/peer.h
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/sdk/include -c -o $@ $<
`, map[string]string{
		"upstream/include/public.h": "#include \"peer.h\"\n#define PUBLIC 1\n",
		"upstream/include/peer.h":   "#define PEER 1\n",
		"consumer.c":                "#include <public.h>\nint consumer;\n",
	}, "all")
	profile.Name = "root:multiline-generated-header-rewrite"
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"upstream/include/public.h": true,
			"upstream/include/peer.h":   true,
			"consumer.c":                true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{
			Path: "generated/sdk/include/public.h", Profile: profile.Name,
			Target: "generated/sdk/include/public.h",
		},
		{
			Path: "generated/sdk/include/peer.h", Profile: profile.Name,
			Target: "generated/sdk/include/peer.h",
		},
	})
	if selection.GeneratedObjectTreeArtifacts != want {
		t.Fatalf(
			"multiline-rewritten generated-header frontier = %q, want bounded selected include-root closure %q",
			selection.GeneratedObjectTreeArtifacts, want,
		)
	}
}

func TestSelectedKbuildSelectionsBoundNonCopyGeneratedHeaderToSelectedRoot(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: generated/sdk/include/opaque.h generated/sdk/include/alias-only.h generated/sdk/include/cycle.h consumer.o
generated/sdk/include/opaque.h: upstream/opaque.in
	sed '/#include/d' $< > $@
generated/sdk/include/alias-only.h: upstream/alias-only.h
	cp $< $@
generated/sdk/include/cycle.h: consumer.o
	printf '#define CYCLE 1\n' > $@
consumer.o: consumer.c generated/sdk/include/alias-only.h
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/sdk/include -c -o $@ $<
`, map[string]string{
		"upstream/opaque.in":    "#include \"alias-only.h\"\n#define OPAQUE 1\n",
		"upstream/alias-only.h": "#define ALIAS_ONLY 1\n",
		"consumer.c":            "#include <opaque.h>\nint consumer;\n",
	}, "all")
	profile.Name = "root:opaque-generated-header"
	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"upstream/opaque.in":    true,
			"upstream/alias-only.h": true,
			"consumer.c":            true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{
			Path: "generated/sdk/include/opaque.h", Profile: profile.Name,
			Target: "generated/sdk/include/opaque.h",
		},
		{
			Path: "generated/sdk/include/alias-only.h", Profile: profile.Name,
			Target: "generated/sdk/include/alias-only.h",
		},
	})
	if selection.GeneratedObjectTreeArtifacts != want {
		t.Fatalf("opaque generated-header frontier = %q, want prior selected root without cyclic future %q", selection.GeneratedObjectTreeArtifacts, want)
	}
}

func TestSelectedKbuildSelectionsBoundOpaqueGeneratedIncludeToPriorSelectedHeaders(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:opaque-generated-include"
	earlier := selectionRoleProfileWithSources(t, `
prepare: include/generated/needed.h outside/generated/unrelated.h
include/generated/needed.h: needed.in
	cp $< $@
outside/generated/unrelated.h: unrelated.in
	cp $< $@
.PHONY: prepare
`, map[string]string{
		"needed.in":    "#define NEEDED 1\n",
		"unrelated.in": "#define UNRELATED 1\n",
	}, "prepare")
	earlier.Name = "child:opaque-include-frontier"
	later := selectionRoleProfileWithSources(t, `
all: generated/opaque.h consumer.o
generated/opaque.h: opaque.in
	sed 's/^//' $< > $@
consumer.o: consumer.c generated/opaque.h
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated -I__LINUX_BZL_OBJECT_TREE__/include/generated -c -o $@ $<
`, map[string]string{
		"opaque.in":  "#include <needed.h>\n",
		"consumer.c": "#include <opaque.h>\nint consumer;\n",
	}, "all")
	later.Name = "child:opaque-include-consumer"
	later.InvocationPredecessors = []string{earlier.Name}
	future := selectionRoleProfileWithSources(t, `
include/generated/future.h: future.in
	cp $< $@
`, map[string]string{
		"future.in": "#define FUTURE 1\n",
	}, "include/generated/future.h")
	future.Name = "child:future-opaque-include"
	future.InvocationPredecessors = []string{later.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlier.Name, Goals: earlier.EntryTargets},
		{Target: "all", Profile: later.Name, Goals: later.EntryTargets},
		{Target: "all", Profile: future.Name, Goals: future.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, earlier, later, future},
		map[string]bool{
			"needed.in":    true,
			"unrelated.in": true,
			"opaque.in":    true,
			"consumer.c":   true,
			"future.in":    true,
		},
		filepath.Dir(later.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "consumer.o")
	wantGenerated := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{Path: "generated/opaque.h", Profile: later.Name, Target: "generated/opaque.h"},
		{Path: "include/generated/needed.h", Profile: earlier.Name, Target: "include/generated/needed.h"},
	})
	if !consumer.UsesInitialObjectTree || consumer.InitialObjectTreeArtifacts != "" ||
		consumer.GeneratedObjectTreeArtifacts != wantGenerated {
		t.Fatalf(
			"opaque generated-include consumer = %#v, want only prior selected generated artifacts %q",
			consumer, wantGenerated,
		)
	}
}

func TestSelectedKbuildSelectionsDeduplicateOnlyPlanEquivalentOpaqueIncludeProducers(t *testing.T) {
	for _, test := range []struct {
		name                          string
		firstPreamble, secondPreamble string
		secondRecipe                  string
		splitLifecycle                bool
		splitInitialFrontier          bool
		differentRelevantInitial      bool
		wantError                     bool
	}{
		{name: "equivalent duplicate", secondRecipe: "\tcp shared.in $@\n"},
		{
			name: "equivalent duplicate with irrelevant initial frontier versions", secondRecipe: "\tcp shared.in $@\n",
			splitInitialFrontier: true,
		},
		{
			name: "same command different relevant initial artifact", secondRecipe: "\tcp shared.in $@\n",
			splitInitialFrontier: true, differentRelevantInitial: true, wantError: true,
		},
		{
			name: "equivalent duplicate from prepare and target invocations", secondRecipe: "\tcp shared.in $@\n",
			splitLifecycle: true,
		},
		{name: "different recipe", secondRecipe: "\tsed 's/^//' shared.in > $@\n", wantError: true},
		{
			name:          "same command different exported environment",
			firstPreamble: "export PRODUCER_CONTRACT := first\n", secondPreamble: "export PRODUCER_CONTRACT := second\n",
			secondRecipe: "\tcp shared.in $@\n", wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot := t.TempDir()
			for name, contents := range map[string]string{
				"shared.in":   "#define SHARED 1\n",
				"consumer.in": "int consumer;\n",
			} {
				if err := os.WriteFile(filepath.Join(sourceRoot, name), []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			makeProfile := func(name, makefile, source string, entryTargets ...string) kconfig.CompactKbuildProfile {
				t.Helper()
				filename := filepath.Join(sourceRoot, makefile)
				if err := os.WriteFile(filename, []byte(selectionRoleFixtureMakefile(source)), 0o644); err != nil {
					t.Fatal(err)
				}
				parsed, err := kconfig.ParseKbuildFileTree(filename, kconfig.KbuildOptions{
					RootDir: sourceRoot,
					Variables: map[string]string{
						"objtree": kbuildEvalObjectTree,
						"srctree": kbuildEvalSourceTree,
					},
					CommandLineVariables: map[string]string{
						"CC": kconfig.KbuildActionRoleToken("target", "cc"),
					},
					SourceRoots: map[string]string{
						kbuildEvalSourceTree: sourceRoot,
						kbuildEvalObjectTree: sourceRoot,
					},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureTargetEvaluator:  true,
				})
				if err != nil {
					t.Fatal(err)
				}
				profile, err := kconfig.NewCompactKbuildProfile(name, filename, sourceRoot, parsed)
				if err != nil {
					t.Fatal(err)
				}
				profile.EntryTargets = append([]string(nil), entryTargets...)
				setTestKbuildInvocationLocation(t, &profile)
				return profile
			}

			rootMakefile := "all:\n"
			firstName := "child:a-equivalent-producer"
			secondName := "child:z-equivalent-producer"
			if test.splitLifecycle {
				// Linux selects scripts/Makefile.build obj=rust once through
				// prepare and again through the ordinary target directory walk.
				// The later invocation overwrites the common generated Rust target
				// after the preparation invocation has completed.
				rootMakefile = "all: prepare\nprepare:\n"
				// Give the target-lifecycle profile the lexically earlier name so
				// version selection is proven by ordering rather than name sorting.
				firstName = "build:rust#7ac253255016"
				secondName = "build:rust#61b2e3543bcb"
			}
			root := makeProfile("root:duplicate-opaque-producers", "root.mk", rootMakefile, "all")
			firstRecipe := "\tcp shared.in $@\n"
			secondRecipe := test.secondRecipe
			if test.splitInitialFrontier {
				// The proc-macro recipe observes one exact generated response file.
				// Repeated recursive invocations can inherit different unrelated
				// frontier versions without changing that physical action input.
				firstRecipe = "\tCFG=__LINUX_BZL_OBJECT_TREE__/include/generated/rustc_cfg cp shared.in $@\n"
				secondRecipe = firstRecipe
			}
			first := makeProfile(
				firstName, "producer.mk",
				test.firstPreamble+"generated/include/shared.h: shared.in\n"+firstRecipe,
				"generated/include/shared.h",
			)
			second := makeProfile(
				secondName, "producer.mk",
				test.secondPreamble+"generated/include/shared.h: shared.in\n"+secondRecipe,
				"generated/include/shared.h",
			)
			consumer := makeProfile("child:opaque-consumer", "consumer.mk", `generated/consumer.c: consumer.in
	sed 's/^//' $< > $@
consumer.o: generated/consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/include -c -o $@ $<
`, "consumer.o")
			consumer.InvocationPredecessors = []string{second.Name, first.Name}
			profiles := []kconfig.CompactKbuildProfile{root, second, first, consumer}
			if test.splitInitialFrontier {
				provider := makeProfile("child:rustc-cfg-provider", "provider.mk", `include/generated/rustc_cfg: shared.in
	cp shared.in $@
`, "include/generated/rustc_cfg")
				visible := kconfig.CompactKbuildVisibleArtifact{
					Path: "include/generated/rustc_cfg", Profile: provider.Name, Target: "include/generated/rustc_cfg",
				}
				secondVisible := visible
				if test.differentRelevantInitial {
					secondVisible.Target = "include/generated/other_cfg"
				}
				setTestCompactKbuildInitialVisibleArtifacts(t, &first, []kconfig.CompactKbuildVisibleArtifact{
					visible,
					{Path: "unrelated/prep-only.h", Profile: provider.Name, Target: provider.EntryTargets[0]},
				})
				setTestCompactKbuildInitialVisibleArtifacts(t, &second, []kconfig.CompactKbuildVisibleArtifact{
					secondVisible,
					{Path: "unrelated/target-only.h", Profile: provider.Name, Target: provider.EntryTargets[0]},
					{Path: "unrelated/target-later.h", Profile: provider.Name, Target: provider.EntryTargets[0]},
				})
				profiles = []kconfig.CompactKbuildProfile{root, second, first, provider, consumer}
				root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
					{Target: "all", Profile: provider.Name, Goals: provider.EntryTargets},
					{Target: "all", Profile: second.Name, Goals: second.EntryTargets},
					{Target: "all", Profile: first.Name, Goals: first.EntryTargets},
					{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
				}
				profiles[0] = root
				profiles[1] = second
				profiles[2] = first
			} else if test.splitLifecycle {
				root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
					{Target: "prepare", Profile: first.Name, Goals: first.EntryTargets},
					{Target: "all", Profile: second.Name, Goals: second.EntryTargets},
					{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
				}
			} else {
				root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
					{Target: "all", Profile: second.Name, Goals: second.EntryTargets},
					{Target: "all", Profile: first.Name, Goals: first.EntryTargets},
					{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
				}
			}
			profiles[0] = root
			profiles[1] = second

			// Reverse the equivalent producers' input order. Ownership must still
			// choose the stable profile-name representative rather than discovery
			// or map iteration order.
			selections, err := selectedKbuildSelectionsFromSourceRoot(
				profiles,
				map[string]bool{"shared.in": true, "consumer.in": true},
				sourceRoot,
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), `opaque compiler include root "generated/include/shared.h" resolves to 2 selected producers`) {
					t.Fatalf("non-equivalent duplicate producer error = %v, want selected-producer ambiguity", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selection := selectionByTarget(t, selections, "consumer.o")
			selectedProducer := first
			if test.splitLifecycle {
				selectedProducer = second
			}
			want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
				{Path: "generated/consumer.c", Profile: consumer.Name, Target: "generated/consumer.c"},
				{Path: "generated/include/shared.h", Profile: selectedProducer.Name, Target: "generated/include/shared.h"},
			})
			if selection.GeneratedObjectTreeArtifacts != want {
				t.Fatalf("equivalent duplicate producer selection = %#v, want deterministic artifact %q", selection, want)
			}
		})
	}
}

func TestSelectedKbuildSelectionsResolveExactIncludesBeforeOpaquePredecessorClosure(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: m/nested.h a/consumer.o
a/consumer.o: z/generated.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/m -c -o $@ $<
z/generated.c: z/source.S
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/m -E -o $@ $<
m/nested.h: nested.in
	sed 's/^//' $< > $@
`, map[string]string{
		"z/source.S": "#include <nested.h>\nint generated;\n",
		"nested.in":  "#define NESTED 1\n",
	}, "all")
	profile.Name = "root:include-fixed-point"

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"z/source.S": true, "nested.in": true},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "a/consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{Path: "z/generated.c", Profile: profile.Name, Target: "z/generated.c"},
		{Path: "m/nested.h", Profile: profile.Name, Target: "m/nested.h"},
	})
	if consumer.GeneratedObjectTreeArtifacts != want {
		t.Fatalf(
			"reverse-lexical opaque predecessor closure = %q, want generated source and its later-discovered include %q",
			consumer.GeneratedObjectTreeArtifacts, want,
		)
	}
}

func TestSelectedKbuildSelectionsExcludeGeneratedProgramsFromOpaqueCompilerRoots(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: scripts/mod/elfconfig.h
scripts/mod/mk_elfconfig: scripts/mod/mk_elfconfig.c
	$(HOSTCC) -o $@ $<
scripts/mod/generated-empty.c: scripts/mod/empty.in
	sed 's/^//' $< > $@
scripts/mod/empty.o: scripts/mod/generated-empty.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__ -c -o $@ $<
scripts/mod/elfconfig.h: scripts/mod/empty.o scripts/mod/mk_elfconfig
	__LINUX_BZL_OBJECT_TREE__/scripts/mod/mk_elfconfig < $< > $@
`, map[string]string{
		"scripts/mod/mk_elfconfig.c": "int main(void) { return 0; }\n",
		"scripts/mod/empty.in":       "int empty;\n",
	}, "all")
	profile.Name = "root:generated-program-opaque-root"

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"scripts/mod/mk_elfconfig.c": true,
			"scripts/mod/empty.in":       true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	empty := selectionByTarget(t, selections, "scripts/mod/empty.o")
	if !empty.UsesInitialObjectTree {
		t.Fatalf("opaque compiler selection = %#v, want object-tree snapshot", empty)
	}
	if strings.Contains(empty.GeneratedObjectTreeArtifacts, "mk_elfconfig") {
		t.Fatalf(
			"opaque compiler selection bound generated executable as include data: %#v",
			empty,
		)
	}
}

func TestSelectedKbuildSelectionsExcludeExactProgramProducerAcrossSamePathVersions(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:versioned-generated-program"
	early := selectionRoleProfileWithSources(t, `
all: generated/tool generated/result.h
generated/tool: generated/tool.c
	$(HOSTCC) -o $@ $<
generated/result.h: generated/tool
	__LINUX_BZL_OBJECT_TREE__/generated/tool > $@
`, map[string]string{
		"generated/tool.c": "int main(void) { return 0; }\n",
	}, "all")
	early.Name = "child:early-generated-program"
	consumerProfile := selectionRoleProfileWithSources(t, `
all: generated/consumer.o
generated/consumer.c: generated/consumer.in
	sed 's/^//' $< > $@
generated/consumer.o: generated/consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated -c -o $@ $<
`, map[string]string{
		"generated/consumer.in": "int consumer;\n",
	}, "all")
	consumerProfile.Name = "child:opaque-program-consumer"
	consumerProfile.InvocationPredecessors = []string{early.Name}
	future := selectionRoleProfileWithSources(t, `
generated/tool: generated/future-tool.c
	$(HOSTCC) -o $@ $<
`, map[string]string{
		"generated/future-tool.c": "int main(void) { return 0; }\n",
	}, "generated/tool")
	future.Name = "child:future-generated-program"
	future.InvocationPredecessors = []string{consumerProfile.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: early.Name, Goals: early.EntryTargets},
		{Target: "all", Profile: consumerProfile.Name, Goals: consumerProfile.EntryTargets},
		{Target: "all", Profile: future.Name, Goals: future.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, early, consumerProfile, future},
		map[string]bool{
			"generated/tool.c":        true,
			"generated/consumer.in":   true,
			"generated/future-tool.c": true,
		},
		filepath.Dir(consumerProfile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "generated/consumer.o")
	if strings.Contains(consumer.GeneratedObjectTreeArtifacts, "generated/tool") {
		t.Fatalf("opaque compiler bound exact earlier executable despite same-path future writer: %#v", consumer)
	}
}

func TestSelectedKbuildSelectionsExcludeUninvokedNonIncludeOutputFromOpaqueRoots(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: generated/host-tool generated/consumer.o
generated/host-tool: generated/host-tool.c
	$(HOSTCC) -o $@ $<
generated/consumer.c: generated/consumer.in
	sed 's/^//' $< > $@
generated/consumer.o: generated/consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated -c -o $@ $<
`, map[string]string{
		"generated/host-tool.c": "int main(void) { return 0; }\n",
		"generated/consumer.in": "int consumer;\n",
	}, "all")
	profile.Name = "root:uninvoked-linked-output"

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"generated/host-tool.c": true,
			"generated/consumer.in": true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "generated/consumer.o")
	if strings.Contains(consumer.GeneratedObjectTreeArtifacts, "generated/host-tool") {
		t.Fatalf("opaque compiler bound source-proven linked output as include data: %#v", consumer)
	}
}

func TestSelectedKbuildSelectionsExcludeNonIncludeOutputsFromOpaqueHostCompilerRoots(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:binary-output-frontier"
	earlier := selectionRoleProfileWithSources(t, `
all: generated/target.o generated/built-in.a generated/data.h
generated/target.o: generated/target.c
	$(CC) -c -o $@ $<
generated/built-in.a: generated/target.o
	$(AR) cDPrST $@ $<
generated/data.h: generated/data.in
	cp $< $@
`, map[string]string{
		"generated/target.c": "int target;\n",
		"generated/data.in":  "#define DATA 1\n",
	}, "all")
	earlier.Name = "child:binary-output-producer"
	later := selectionRoleProfileWithSources(t, `
all: tools/host.o
tools/host.o: tools/host.c
	$(HOSTCC) -I__LINUX_BZL_OBJECT_TREE__/generated -c -o $@ $<
`, map[string]string{
		"tools/host.c": "#include <data.h>\nint host;\n",
	}, "all")
	later.Name = "child:binary-output-consumer"
	later.InvocationPredecessors = []string{earlier.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &later, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "generated/built-in.a", Profile: earlier.Name, Target: "generated/built-in.a"},
		{Path: "generated/data.h", Profile: earlier.Name, Target: "generated/data.h"},
		{Path: "generated/target.o", Profile: earlier.Name, Target: "generated/target.o"},
	})
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlier.Name, Goals: earlier.EntryTargets},
		{Target: "all", Profile: later.Name, Goals: later.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, earlier, later},
		map[string]bool{
			"generated/target.c": true,
			"generated/data.in":  true,
			"tools/host.c":       true,
		},
		filepath.Dir(later.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	host := selectionByTarget(t, selections, "tools/host.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/data.h", Profile: earlier.Name, Target: "generated/data.h",
	}})
	if host.InitialObjectTreeArtifacts != want || strings.Contains(host.InitialObjectTreeArtifacts, "target.o") ||
		strings.Contains(host.InitialObjectTreeArtifacts, "built-in.a") {
		t.Fatalf("host compiler initial frontier = %#v, want only generated source data %q", host, want)
	}
}

func TestSelectedKbuildSelectionsExcludeUninvokedInitialProgramsFromOpaqueRoots(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:initial-program-frontier"
	earlier := selectionRoleProfileWithSources(t, `
all: generated/host-tool generated/host-data.h
generated/host-tool: generated/host-tool.c
	$(HOSTCC) -o $@ $<
generated/host-data.h: generated/host-data.in
	cp $< $@
`, map[string]string{
		"generated/host-tool.c":  "int main(void) { return 0; }\n",
		"generated/host-data.in": "#define HOST_DATA 1\n",
	}, "all")
	earlier.Name = "child:initial-program-producer"
	later := selectionRoleProfileWithSources(t, `
all: generated/consumer.o generated/invoked.h generated/tool-copy
generated/consumer.o: generated/consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated -c -o $@ $<
generated/invoked.h:
	__LINUX_BZL_OBJECT_TREE__/generated/host-tool > $@
generated/tool-copy:
	cp __LINUX_BZL_OBJECT_TREE__/generated/host-tool $@
`, map[string]string{
		"generated/consumer.c": "#include <host-data.h>\nint consumer;\n",
	}, "all")
	later.Name = "child:initial-program-consumer"
	later.InvocationPredecessors = []string{earlier.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &later, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "generated/host-data.h", Profile: earlier.Name, Target: "generated/host-data.h"},
		{Path: "generated/host-tool", Profile: earlier.Name, Target: "generated/host-tool"},
	})
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlier.Name, Goals: earlier.EntryTargets},
		{Target: "all", Profile: later.Name, Goals: later.EntryTargets},
	}

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{root, earlier, later},
		map[string]bool{
			"generated/host-tool.c":  true,
			"generated/host-data.in": true,
			"generated/consumer.c":   true,
		},
		filepath.Dir(later.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "generated/consumer.o")
	wantData := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/host-data.h", Profile: earlier.Name, Target: "generated/host-data.h",
	}})
	if consumer.InitialObjectTreeArtifacts != wantData || strings.Contains(consumer.InitialObjectTreeArtifacts, "host-tool") {
		t.Fatalf("opaque compiler initial frontier = %#v, want only generated data %q", consumer, wantData)
	}
	invoked := selectionByTarget(t, selections, "generated/invoked.h")
	wantTool := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/host-tool", Profile: earlier.Name, Target: "generated/host-tool",
	}})
	if invoked.InitialObjectTreeArtifacts != wantTool {
		t.Fatalf("direct program consumer initial frontier = %#v, want exact invoked tool %q", invoked, wantTool)
	}
	copied := selectionByTarget(t, selections, "generated/tool-copy")
	if copied.InitialObjectTreeArtifacts != wantTool {
		t.Fatalf("direct data consumer initial frontier = %#v, want exact read artifact %q", copied, wantTool)
	}
}

func TestSelectedKbuildSelectionsKeepHostGeneratedDataInOpaqueCompilerRoots(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
all: generated/data/host-data.h generated/data/consumer.o
generated/data/host-data.h: generated/data/host-data.c
	$(HOSTCC) -E -o $@ $<
generated/data/consumer.c: generated/data/consumer.in generated/data/host-data.h
	sed 's/^//' $< > $@
generated/data/consumer.o: generated/data/consumer.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__/generated/data -c -o $@ $<
`, map[string]string{
		"generated/data/host-data.c": "#define HOST_DATA 1\n",
		"generated/data/consumer.in": "int consumer;\n",
	}, "all")
	profile.Name = "root:host-generated-data-opaque-root"

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"generated/data/host-data.c": true,
			"generated/data/consumer.in": true,
		},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "generated/data/consumer.o")
	if !strings.Contains(consumer.GeneratedObjectTreeArtifacts, "host-data.h") {
		t.Fatalf(
			"opaque compiler selection = %#v, want source-derived host data retained",
			consumer,
		)
	}
}

func TestSelectedKbuildSelectionsDoNotBindTargetLifecycleOutputsIntoOpaquePrepRoots(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
prepare: generated/prep.o
generated/prep.c: generated/prep.in
	sed 's/^//' $< > $@
generated/prep.o: generated/prep.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__ -c -o $@ $<
all: prepare drivers/example/built-in.a
drivers/example/built-in.a: drivers/example/target.c
	$(CC) -c -o $@ $<
`, map[string]string{
		"generated/prep.in":        "int prep;\n",
		"drivers/example/target.c": "int target;\n",
	}, "prepare", "all")
	profile.Name = "root:prep-opaque-root"

	selections, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{
			"generated/prep.in":        true,
			"drivers/example/target.c": true,
		},
		nil, filepath.Dir(profile.Path), []string{"prepare"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	prep := selectionByTarget(t, selections, "generated/prep.o")
	if prep.Lifecycle != "prep" || prep.Stage != "prep" {
		t.Fatalf("preparation compiler selection = %#v, want prep lifecycle and stage", prep)
	}
	if strings.Contains(prep.GeneratedObjectTreeArtifacts, "drivers/example/built-in.a") {
		t.Fatalf("preparation compiler bound target-only opaque output: %#v", prep)
	}
}

func TestSelectedKbuildSelectionsDoNotPublishPhonyActionsIntoOpaqueRoots(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
PHONY += prepare outputmakefile
.PHONY: $(PHONY)
prepare: outputmakefile generated/prep.o
outputmakefile:
	ln -fsn source Makefile
generated/prep.c: generated/prep.in
	sed 's/^//' $< > $@
generated/prep.o: generated/prep.c
	$(CC) -I__LINUX_BZL_OBJECT_TREE__ -c -o $@ $<
`, map[string]string{
		"generated/prep.in": "int prep;\n",
	}, "prepare")
	profile.Name = "root:phony-opaque-root"

	selections, err := selectedKbuildSelectionsFromSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"generated/prep.in": true},
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	prep := selectionByTarget(t, selections, "generated/prep.o")
	if strings.Contains(prep.GeneratedObjectTreeArtifacts, "outputmakefile") {
		t.Fatalf("opaque compiler published phony action as generated file: %#v", prep)
	}
}

func mustSelectedKbuildSelections(
	t *testing.T,
	profiles []kconfig.CompactKbuildProfile,
	satisfied map[string]bool,
) []kconfig.CompactKbuildSelection {
	t.Helper()
	for index := range profiles {
		setTestKbuildInvocationLocation(t, &profiles[index])
	}
	selections, err := selectedKbuildSelections(profiles, satisfied)
	if err != nil {
		t.Fatal(err)
	}
	return selections
}

func setTestKbuildInvocationLocation(t *testing.T, profile *kconfig.CompactKbuildProfile) {
	t.Helper()
	if _, ok := kconfig.CompactKbuildProfileInvocationLocation(*profile); ok {
		return
	}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: profile.Directory,
	}); err != nil {
		t.Fatalf("set test Kbuild profile %q invocation location: %v", profile.Name, err)
	}
}

// syntheticSelectionProfileWithEvaluator keeps focused selection fixtures
// source-backed without forcing each test to spell a complete Makefile. The
// exported graph remains the fixture's source of truth; only the process-local
// evaluator comes from the empty parsed file.
func syntheticSelectionProfileWithEvaluator(t *testing.T, profile kconfig.CompactKbuildProfile) kconfig.CompactKbuildProfile {
	t.Helper()
	root := t.TempDir()
	filename := filepath.Join(root, "Makefile")
	if err := os.WriteFile(filename, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(filename, kconfig.KbuildOptions{
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	backed, err := kconfig.NewCompactKbuildProfile(profile.Name, filename, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	backed.Name = profile.Name
	backed.Path = profile.Path
	backed.Directory = profile.Directory
	setTestCompactKbuildInitialVisibleArtifacts(t, &backed, testCompactKbuildInitialVisibleArtifacts(profile))
	backed.InvocationPredecessors = append([]string(nil), profile.InvocationPredecessors...)
	backed.TargetInvocationDependencies = append([]kconfig.CompactKbuildInvocationDependency(nil), profile.TargetInvocationDependencies...)
	backed.EntryTargets = append([]string(nil), profile.EntryTargets...)
	backed.Generated = append([]kconfig.KbuildTarget(nil), profile.Generated...)
	backed.Rules = append([]kconfig.KbuildRule(nil), profile.Rules...)
	backed.TargetVariables = append([]kconfig.KbuildTargetVariable(nil), profile.TargetVariables...)
	setTestKbuildInvocationLocation(t, &backed)
	return backed
}

// selectionRoleFixtureMakefile models the way real Kbuild recipes acquire the
// source and object roots. The public tree markers are parser output and must
// not be authored directly in Makefile text: production parsing deliberately
// protects such literals so they cannot forge tree capabilities.
func selectionRoleFixtureMakefile(makefile string) string {
	return strings.NewReplacer(
		kbuildEvalSourceTree, "$(srctree)",
		kbuildEvalObjectTree, "$(objtree)",
	).Replace(makefile)
}

func selectionRoleProfile(t *testing.T, makefile string, entryTargets ...string) kconfig.CompactKbuildProfile {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(filename, []byte(selectionRoleFixtureMakefile(makefile)), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(filename, kconfig.KbuildOptions{
		Variables: map[string]string{
			"objtree": kbuildEvalObjectTree,
			"srctree": kbuildEvalSourceTree,
		},
		CommandLineVariables: map[string]string{
			"AR":      kconfig.KbuildActionRoleToken("target", "ar"),
			"BINDGEN": kconfig.KbuildActionRoleToken("target", "bindgen"),
			"CC":      kconfig.KbuildActionRoleToken("target", "cc"),
			"LD":      kconfig.KbuildActionRoleToken("target", "ld"),
			"RUSTC":   kconfig.KbuildActionRoleToken("target", "rustc"),
			"HOSTAR":  kconfig.KbuildActionRoleToken("host", "ar"),
			"HOSTCC":  kconfig.KbuildActionRoleToken("host", "cc"),
			"HOSTLD":  kconfig.KbuildActionRoleToken("host", "ld"),
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("root:selection-scope", filename, filepath.Dir(filename), parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = append([]string(nil), entryTargets...)
	setTestKbuildInvocationLocation(t, &profile)
	return profile
}

func selectionRoleProfileWithSources(
	t *testing.T,
	makefile string,
	sources map[string]string,
	entryTargets ...string,
) kconfig.CompactKbuildProfile {
	t.Helper()
	root := t.TempDir()
	filename := filepath.Join(root, "Makefile")
	if err := os.WriteFile(filename, []byte(selectionRoleFixtureMakefile(makefile)), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range sources {
		pathname := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(pathname), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pathname, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := kconfig.ParseKbuildFileTree(filename, kconfig.KbuildOptions{
		Variables: map[string]string{
			"objtree": kbuildEvalObjectTree,
			"srctree": kbuildEvalSourceTree,
		},
		CommandLineVariables: map[string]string{
			"AR":      kconfig.KbuildActionRoleToken("target", "ar"),
			"BINDGEN": kconfig.KbuildActionRoleToken("target", "bindgen"),
			"CC":      kconfig.KbuildActionRoleToken("target", "cc"),
			"LD":      kconfig.KbuildActionRoleToken("target", "ld"),
			"RUSTC":   kconfig.KbuildActionRoleToken("target", "rustc"),
			"HOSTAR":  kconfig.KbuildActionRoleToken("host", "ar"),
			"HOSTCC":  kconfig.KbuildActionRoleToken("host", "cc"),
			"HOSTLD":  kconfig.KbuildActionRoleToken("host", "ld"),
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("root:selection-scope", filename, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = append([]string(nil), entryTargets...)
	setTestKbuildInvocationLocation(t, &profile)
	return profile
}

func selectionByTarget(t *testing.T, selections []kconfig.CompactKbuildSelection, target string) kconfig.CompactKbuildSelection {
	t.Helper()
	for _, selection := range selections {
		if selection.Target == target {
			return selection
		}
	}
	t.Fatalf("selections omit %q: %#v", target, selections)
	return kconfig.CompactKbuildSelection{}
}

func TestSelectedKbuildSelectionsDeriveHostScopeForSameExecutableThroughOverridesAndFixedPoint(t *testing.T) {
	const sharedCompiler = "/same/toolchain/bin/cc"
	targetContract := &hostKbuildContract{
		Actions:       map[string]configuredKbuildAction{"cc": {Path: sharedCompiler}},
		MakeVariables: map[string]string{"CC": "cc"},
	}
	hostContract := &hostKbuildContract{
		Actions:       map[string]configuredKbuildAction{"cc": {Path: sharedCompiler}},
		MakeVariables: map[string]string{"HOSTCC": "cc"},
	}
	commandLine, err := kbuildCommandLineVariables(targetContract, hostContract, nil)
	if err != nil {
		t.Fatal(err)
	}
	if targetContract.Actions["cc"].Path != hostContract.Actions["cc"].Path {
		t.Fatal("test fixture does not use the same host and target compiler executable")
	}
	if commandLine["CC"] == commandLine["HOSTCC"] ||
		commandLine["CC"] != kconfig.KbuildActionRoleToken("target", "cc") ||
		commandLine["HOSTCC"] != kconfig.KbuildActionRoleToken("host", "cc") {
		t.Fatalf("same executable lost scoped source provenance: %#v", commandLine)
	}
	profile := selectionRoleProfile(t, `
HOST_OVERRIDES := CC="$(HOSTCC)" LD="$(HOSTLD)"
all: target-output host-output
target-output: target.c
	$(CC) -c -o $@ $<
host-output: neutral-middle
	$(HOST_OVERRIDES) $(HOSTCC) -o $@ host.c
neutral-middle: neutral-leaf
	cp $< $@
neutral-leaf: neutral.in
	cp $< $@
`, "all")
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"target.c": true, "host.c": true, "neutral.in": true,
	})
	for _, target := range []string{"host-output", "neutral-middle", "neutral-leaf"} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != "host" || selection.Stage != "host" || selection.Lifecycle != "target" {
			t.Errorf("%s selection = %#v, want target-lifecycle host scope/stage", target, selection)
		}
	}
	target := selectionByTarget(t, selections, "target-output")
	if target.Scope != "target" || target.Stage != "target" || target.Lifecycle != "target" {
		t.Fatalf("target compiler selection = %#v, want target lifecycle/scope/stage", target)
	}
}

func TestEvaluatedKbuildProfilesInheritRecursiveCommandLineActionRoles(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all:
	$(MAKE) -f $(srctree)/parent.mk LD="$(HOSTLD)" parent
`)
	write("parent.mk", `
parent:
	$(MAKE) -f $(srctree)/child.mk inherited
	$(MAKE) -f $(srctree)/child.mk LD="$(TARGETLD)" explicit
`)
	write("child.mk", `
ifeq ($(MAKECMDGOALS),inherited)
inherited: inherited.in
	$(LD) -r -o $@ $<
endif
ifeq ($(MAKECMDGOALS),explicit)
explicit: explicit.in
	$(LD) -r -o $@ $<
endif
`)
	write("inherited.in", "host input\n")
	write("explicit.in", "target input\n")

	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, map[string]string{"SRCARCH": "x86"}, kconfig.KbuildOptions{
		RootDir: root,
		CommandLineVariables: map[string]string{
			"LD":       kconfig.KbuildActionRoleToken("target", "ld"),
			"HOSTLD":   kconfig.KbuildActionRoleToken("host", "ld"),
			"TARGETLD": kconfig.KbuildActionRoleToken("target", "ld"),
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profilesByName := make(map[string]kconfig.CompactKbuildProfile, len(profiles))
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}
	wantScopes := map[string]string{"inherited": "host", "explicit": "target"}
	gotScopes := map[string]string{}
	for _, selection := range selections {
		profile := profilesByName[selection.Profile]
		if profile.Path != "child.mk" {
			continue
		}
		gotScopes[selection.Target] = selection.Scope
		if selection.Target == "inherited" && selection.Stage != "host" {
			t.Errorf("inherited recursive action stage = %q, want host", selection.Stage)
		}
	}
	if !maps.Equal(gotScopes, wantScopes) {
		t.Fatalf("recursive child scopes = %#v, want %#v; selections: %#v", gotScopes, wantScopes, selections)
	}
}

func TestEvaluatedKbuildProfilesPreserveExternalModuleModeAcrossRootSelfSubmake(t *testing.T) {
	root := t.TempDir()
	objectRoot := t.TempDir()
	externalRoot := t.TempDir()
	write := func(base, relative, content string) {
		t.Helper()
		filename := filepath.Join(base, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root, "Makefile", `
this-makefile := $(lastword $(MAKEFILE_LIST))
abs_srctree := $(realpath $(dir $(this-makefile)))
abs_output := $(CURDIR)

ifeq ("$(origin M)", "command line")
KBUILD_EXTMOD := $(M)
endif
export KBUILD_EXTMOD

ifneq ($(sub_make_done),1)
output := $(if $(KBUILD_EXTMOD),$(KBUILD_EXTMOD),$(abs_output))
srcroot := $(realpath $(KBUILD_EXTMOD))
export objtree srcroot
$(shell mkdir -p "$(output)")
abs_output := $(realpath $(output))
export sub_make_done := 1
endif

ifeq ($(abs_output),$(CURDIR))
need-sub-make :=
else
need-sub-make := 1
endif

ifeq ($(need-sub-make),1)
modules:
	$(MAKE) -C $(abs_output) -f $(abs_srctree)/Makefile modules
else
export srctree := $(abs_srctree)
ifeq ($(KBUILD_EXTMOD),)
modules: prepare
prepare:
	$(MAKE) -f $(srctree)/scripts/Makefile.build obj=rust all
else
build-dir := .
PHONY += $(build-dir)
modules: prepare $(build-dir) modpost
prepare:
	@:
$(build-dir):
	$(MAKE) -f $(srctree)/scripts/Makefile.build obj=$@ all
modpost:
	$(MAKE) -f $(srctree)/scripts/Makefile.modpost Module.symvers
.PHONY: $(PHONY)
endif
endif
`)
	write(root, "scripts/Makefile.build", `
src := $(srcroot)/$(obj)
ifeq ($(obj),rust)
all: rust/uapi/uapi_generated.rs
rust/uapi/uapi_generated.rs: $(srctree)/rust/uapi/uapi_helper.h
	cp $< $@
else
include $(src)/Kbuild
all: $(obj)/module.o
$(obj)/module.o: $(src)/$(MODULE_SOURCE)
	cp $< $@
endif
`)
	write(root, "scripts/Makefile.modpost", `
MODPOST = $(objtree)/scripts/mod/modpost
Module.symvers: FORCE
	$(MODPOST) -o $@
FORCE:
`)
	write(root, "rust/uapi/uapi_helper.h", "in-tree Rust input\n")
	write(externalRoot, "Kbuild", "MODULE_SOURCE := module.c\n")
	write(externalRoot, "module.c", "external module input\n")
	write(objectRoot, "scripts/mod/modpost", "prepared modpost\n")

	const externalDirectory = "external/module"
	externalSourceRoot := kbuildEvalSourceTree + "/" + externalDirectory
	configured := map[string]string{
		"M":       externalSourceRoot,
		"SRCARCH": "x86",
	}
	targetContract := &hostKbuildContract{MakeVariables: map[string]string{}}
	hostContract := &hostKbuildContract{MakeVariables: map[string]string{}}
	commandLine, err := kbuildCommandLineVariables(targetContract, hostContract, configured)
	if err != nil {
		t.Fatal(err)
	}
	variables := maps.Clone(configured)
	for name, value := range linuxRootMakeInvocationVariables(root) {
		if _, exists := variables[name]; !exists {
			variables[name] = value
		}
	}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		objectRoot,
		[]string{"modules"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                        root,
			Variables:                      variables,
			CommandLineVariables:           commandLine,
			AutoExportCommandLineVariables: kbuildConfiguredCommandLineAutoExports(configured, targetContract, hostContract),
			SourceRoots: map[string]string{
				externalSourceRoot: externalRoot,
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	profilesByName := make(map[string]kconfig.CompactKbuildProfile, len(profiles))
	var externalRootProfile *kconfig.CompactKbuildProfile
	for index := range profiles {
		profile := &profiles[index]
		profilesByName[profile.Name] = *profile
		location, located := kconfig.CompactKbuildProfileInvocationLocation(*profile)
		if profile.Path == "Makefile" && profile.Directory == externalDirectory {
			externalRootProfile = profile
			if !located || location.Tree != kconfig.CompactKbuildInvocationObjectTree || location.Directory != externalDirectory {
				t.Fatalf("external root invocation location = %#v,%t, want object overlay %q", location, located, externalDirectory)
			}
		}
	}
	if externalRootProfile == nil {
		rootStates := map[string]map[string]string{}
		for _, profile := range profiles {
			if profile.Path != "Makefile" {
				continue
			}
			state := map[string]string{}
			for _, expression := range []string{"$(CURDIR)", "$(output)", "$(abs_output)", "$(M)", "$(KBUILD_EXTMOD)", "$(sub_make_done)"} {
				state[expression], _ = kconfig.EvaluateCompactKbuildTextSymbolic(profile, "modules", "", nil, nil, nil, expression)
			}
			rootStates[profile.Name] = state
		}
		t.Fatalf("profiles omit root self-submake in external object overlay: root states=%#v profiles=%#v", rootStates, profiles)
	}
	var externalModpostProfile *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/Makefile.modpost" && profiles[index].Directory == externalDirectory {
			externalModpostProfile = &profiles[index]
			break
		}
	}
	if externalModpostProfile == nil {
		t.Fatalf("profiles omit external modpost driver: %#v", profiles)
	}
	modpostTarget := externalDirectory + "/Module.symvers"
	normal, orderOnly, stem, evalErr := kconfig.EvaluateCompactKbuildTargetRuleContext(*externalModpostProfile, modpostTarget)
	if evalErr != nil {
		t.Fatalf("evaluate external modpost rule context: %v", evalErr)
	}
	injected, evalErr := kconfig.CompactKbuildTargetEvaluationInjections(
		*externalModpostProfile, modpostTarget, stem, normal, orderOnly,
	)
	if evalErr != nil {
		t.Fatalf("evaluate external modpost invocation aliases: %v", evalErr)
	}
	modpostProgram, evalErr := kconfig.EvaluateCompactKbuildTextSymbolic(
		*externalModpostProfile, modpostTarget, stem, normal, orderOnly, injected, "$(MODPOST)",
	)
	if evalErr != nil {
		t.Fatalf("evaluate external modpost program: %v", evalErr)
	}
	if got, want := modpostProgram, kbuildEvalObjectTree+"/scripts/mod/modpost"; got != want {
		t.Fatalf("external modpost program = %q, want prepared-tree root %q", got, want)
	}
	for expression, want := range map[string]string{
		"$(origin M)":             "command line",
		"$(origin KBUILD_EXTMOD)": "file",
		"$(KBUILD_EXTMOD)":        externalSourceRoot,
		"$(srcroot)":              externalSourceRoot,
	} {
		got, evalErr := kconfig.EvaluateCompactKbuildTextSymbolic(
			*externalRootProfile, externalDirectory+"/modules", "", nil, nil, nil, expression,
		)
		if evalErr != nil {
			t.Fatalf("evaluate external root %s: %v", expression, evalErr)
		}
		if got != want {
			t.Errorf("external root %s = %q, want %q", expression, got, want)
		}
	}

	selectedExternal := false
	for _, selection := range selections {
		if selection.Target == externalDirectory+"/module.o" {
			selectedExternal = true
		}
		if selection.Target == "rust/uapi/uapi_generated.rs" {
			t.Fatalf("external module selected in-tree Rust bindgen output: %#v", selection)
		}
		if profile := profilesByName[selection.Profile]; profile.Directory == "rust" {
			t.Fatalf("external module selected in-tree Rust profile: %#v", selection)
		}
	}
	if !selectedExternal {
		t.Fatalf("selections omit external module object: selections=%#v profiles=%#v", selections, profiles)
	}
}

func TestEvaluatedKbuildProfilesRefreshProbeEnvironmentBeforeChildren(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
ROLE_FREE_ENV := source-selected
export ROLE_FREE_ENV
all:
	$(MAKE) -f $(srctree)/child.mk child
`)
	write("child.mk", `
ifeq ($(shell child-policy),selected)
child: child.in
	cp $< $@
endif
`)
	write("child.in", "input\n")
	refreshed := false
	baseOptions := kconfig.KbuildOptions{
		RootDir: root, ConfigVariablesComplete: true, MakeVariablesComplete: true,
		Shell: func(command string) (string, error) {
			if command != "child-policy" {
				return "", fmt.Errorf("unexpected shell command %q", command)
			}
			if !refreshed {
				return "", fmt.Errorf("child evaluated before root environment refresh")
			}
			return "selected", nil
		},
	}
	profiles, _, _, err := evaluatedKbuildProfilesWithOptions(
		root, root, []string{"all"}, map[string]string{"SRCARCH": "x86"}, baseOptions,
		func(exported map[string]string) (func() error, error) {
			if got := exported["ROLE_FREE_ENV"]; got != "source-selected" {
				return nil, fmt.Errorf("root ROLE_FREE_ENV=%q, want source-selected", got)
			}
			return func() error {
				refreshed = true
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("root environment refresh callback was not called")
	}
	if !slices.ContainsFunc(profiles, func(profile kconfig.CompactKbuildProfile) bool { return profile.Path == "child.mk" }) {
		t.Fatalf("profiles omit post-refresh child invocation: %#v", profiles)
	}
}

func TestEvaluatedKbuildProfilesInheritCanonicalRootAliasesIntoDefaultGoalChild(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all:
	$(MAKE) -f $(srctree)/parent.mk parent
`)
	write("parent.mk", `
build := -f $(srctree)/scripts/Makefile.build obj
parent:
	$(MAKE) $(build)=generated
`)
	write("scripts/Makefile.build", `
$(obj)/: $(obj)/result
	@:
$(obj)/result: $(obj)/input
	cp $< $@
`)
	write("generated/input", "input\n")

	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, map[string]string{"SRCARCH": "x86"}, kconfig.KbuildOptions{
		RootDir: root,
		// This is the logical Bazel execroot spelling received by the planner.
		// Profile evaluation canonicalizes it to the declared source-tree sentinel
		// before GNU Make command-line inheritance reaches nested invocations.
		CommandLineVariables: map[string]string{
			"srctree": "external/+linux_source_repository+fixture",
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profilesByName := make(map[string]kconfig.CompactKbuildProfile, len(profiles))
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}
	for _, selection := range selections {
		profile := profilesByName[selection.Profile]
		if profile.Path == "scripts/Makefile.build" && selection.Target == "generated/result" {
			return
		}
	}
	t.Fatalf("default-goal recursive driver selection not discovered: profiles=%#v selections=%#v", profiles, selections)
}

func TestInheritKbuildInvocationCommandLineVariables(t *testing.T) {
	parent := map[string]string{
		"CC":           kconfig.KbuildActionRoleToken("host", "cc"),
		"LD":           kconfig.KbuildActionRoleToken("host", "ld"),
		"MAKECMDGOALS": "parent",
	}
	request := kbuildInvocationRequest{variables: map[string]string{
		"LD":           kconfig.KbuildActionRoleToken("target", "ld"),
		"MAKECMDGOALS": "child",
	}}
	parentAutoExport := map[string]bool{"CC": true, "LD": true}
	got := inheritKbuildInvocationCommandLineVariables(request, parent, parentAutoExport)
	want := map[string]string{
		"CC":           kconfig.KbuildActionRoleToken("host", "cc"),
		"LD":           kconfig.KbuildActionRoleToken("target", "ld"),
		"MAKECMDGOALS": "child",
	}
	if !maps.Equal(got.variables, want) {
		t.Fatalf("inherited command-line variables = %#v, want %#v", got.variables, want)
	}
	if parent["MAKECMDGOALS"] != "parent" || request.variables["MAKECMDGOALS"] != "child" {
		t.Fatalf("inheritance mutated its inputs: parent=%#v request=%#v", parent, request.variables)
	}
	suppressed := request
	suppressed.suppressParentCommandLine = true
	suppressed = inheritKbuildInvocationCommandLineVariables(suppressed, parent, parentAutoExport)
	wantSuppressed := map[string]string{
		"LD":           kconfig.KbuildActionRoleToken("target", "ld"),
		"MAKECMDGOALS": "child",
	}
	if !maps.Equal(suppressed.variables, wantSuppressed) {
		t.Fatalf("MAKEOVERRIDES-suppressed variables = %#v, want %#v", suppressed.variables, wantSuppressed)
	}
}

func TestEvaluatedKbuildProfilesInheritSourceExportedEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, childAssignment, childDefinition, wantCFLAGS string
	}{
		{name: "parent export", wantCFLAGS: "-I" + kbuildEvalSourceTree + "/tools/include"},
		{name: "child file assignment replaces environment", childDefinition: "CFLAGS := -DLOCAL", wantCFLAGS: "-DLOCAL"},
		{name: "child command line wins", childAssignment: "CFLAGS=-DCHILD", childDefinition: "CFLAGS := -DLOCAL", wantCFLAGS: "-DCHILD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(relative, content string) {
				t.Helper()
				filename := filepath.Join(root, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("Makefile", `
all:
	$(MAKE) -C $(srctree)/tools/lib all
`)
			write("tools/lib/Makefile", `
CFLAGS := -I$(srctree)/tools/include
export CFLAGS
BARE := -DBARE
export BARE
export ASSIGNED = -DASSIGNED
all:
	$(MAKE) -f $(srctree)/tools/build/Makefile.build obj=tools/lib/subcmd `+test.childAssignment+` tools/lib/subcmd/exec-cmd.o
`)
			write("tools/build/Makefile.build", test.childDefinition+`
c_flags_1 = -Wp,-MD,$(obj)/.exec-cmd.o.d $(CFLAGS)
cmd_cc_o_c = $(CC) $(c_flags_1) -c -o $@ $<
all: $(obj)/exec-cmd.o
$(obj)/exec-cmd.o: $(srctree)/tools/lib/subcmd/exec-cmd.c
	$(cmd_cc_o_c) $(BARE) $(ASSIGNED)
`)
			write("tools/lib/subcmd/exec-cmd.c", "int main(void) { return 0; }\n")

			variables := map[string]string{"SRCARCH": "x86"}
			profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
				RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var child *kconfig.CompactKbuildProfile
			for index := range profiles {
				if profiles[index].Path == "tools/build/Makefile.build" {
					child = &profiles[index]
					break
				}
			}
			if child == nil {
				t.Fatalf("profiles omit exported-environment child: %#v", profiles)
			}
			target := "tools/lib/subcmd/exec-cmd.o"
			values, err := kconfig.EvaluateCompactKbuildTargetSymbolic(
				*child, target, "", nil, nil, nil, "CFLAGS", "BARE", "ASSIGNED",
			)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{
				"CFLAGS":   test.wantCFLAGS,
				"BARE":     "-DBARE",
				"ASSIGNED": "-DASSIGNED",
			} {
				if got := values[name]; got != want {
					t.Errorf("child %s = %q, want %q", name, got, want)
				}
			}
			var compileRule *kconfig.KbuildRule
			for index := range child.Rules {
				if slices.Contains(child.Rules[index].Targets, target) {
					compileRule = &child.Rules[index]
					break
				}
			}
			if compileRule == nil || len(compileRule.Recipe) != 1 {
				t.Fatalf("child compile rule = %#v, want one source-derived recipe", compileRule)
			}
			command, err := kconfig.EvaluateCompactKbuildTextSymbolic(
				*child, target, "", compileRule.Prerequisites, compileRule.OrderOnly,
				nil, compileRule.Recipe[0],
			)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(command, test.wantCFLAGS) {
				t.Fatalf("evaluated child compile command %q omits inherited flag %q", command, test.wantCFLAGS)
			}
			for _, want := range []string{"-Wp,-MD,tools/lib/subcmd/.exec-cmd.o.d", "-DBARE", "-DASSIGNED"} {
				if !strings.Contains(command, want) {
					t.Errorf("evaluated child compile command %q omits %q", command, want)
				}
			}
			if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
				return selection.Profile == child.Name && selection.Target == target
			}) {
				t.Fatalf("selections omit exported-environment child target from %q: %#v", child.Name, selections)
			}
		})
	}
}

func TestEvaluatedKbuildProfilesExportConfigSelectedRecordMcountMode(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
ifdef CONFIG_FUNCTION_TRACER
ifdef CONFIG_FTRACE_MCOUNT_USE_RECORDMCOUNT
ifdef CONFIG_HAVE_C_RECORDMCOUNT
    BUILD_C_RECORDMCOUNT := y
    export BUILD_C_RECORDMCOUNT
endif
endif
endif
all:
	$(MAKE) -f $(srctree)/scripts/Makefile.build obj=demo demo/example.o
`)
	write("scripts/Makefile.build", `
ifdef BUILD_C_RECORDMCOUNT
record_mcount = $(objtree)/scripts/recordmcount
else
record_mcount = perl $(srctree)/scripts/recordmcount.pl
endif
$(obj)/example.o: $(srctree)/demo/example.c
	$(CC) -c -o $@ $<; $(record_mcount) $@
`)
	write("demo/example.c", "int example;\n")

	variables := map[string]string{
		"CC":                                    "cc",
		"CONFIG_FUNCTION_TRACER":                "y",
		"CONFIG_FTRACE_MCOUNT_USE_RECORDMCOUNT": "y",
		"CONFIG_HAVE_C_RECORDMCOUNT":            "y",
		"SRCARCH":                               "arm",
	}
	profiles, _, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"all"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                 root,
			Variables:               variables,
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var child *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/Makefile.build" {
			child = &profiles[index]
			break
		}
	}
	if child == nil {
		t.Fatalf("profiles omit recordmcount build child: %#v", profiles)
	}
	values, err := kconfig.EvaluateCompactKbuildTargetSymbolic(
		*child, "demo/example.o", "demo/example", nil, nil, nil,
		"BUILD_C_RECORDMCOUNT", "record_mcount",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["BUILD_C_RECORDMCOUNT"], "y"; got != want {
		t.Fatalf("child BUILD_C_RECORDMCOUNT = %q, want %q", got, want)
	}
	if got, want := values["record_mcount"], kbuildEvalObjectTree+"/scripts/recordmcount"; got != want {
		t.Fatalf("child record_mcount = %q, want %q", got, want)
	}
}

func TestEvaluatedKbuildProfilesPreserveConfiguredLibelfFlagsAcrossRecursiveBuild(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all:
	$(MAKE) -f $(srctree)/tools/bpf/resolve_btfids/Makefile resolve_btfids
`)
	write("tools/bpf/resolve_btfids/Makefile", `
LIBELF_FLAGS := $(shell host-pkg-config libelf --cflags)
HOSTCFLAGS_resolve_btfids += $(LIBELF_FLAGS)
export HOSTCFLAGS_resolve_btfids
resolve_btfids:
	$(MAKE) -f $(srctree)/tools/build/Makefile.build obj=tools/bpf/resolve_btfids tools/bpf/resolve_btfids/main.o
`)
	write("tools/build/Makefile.build", `
cmd_host-csingle = $(HOSTCC) $(HOSTCFLAGS_resolve_btfids) -c -o $@ $<
tools/bpf/resolve_btfids/main.o: $(srctree)/tools/bpf/resolve_btfids/main.c
	$(cmd_host-csingle)
`)
	write("tools/bpf/resolve_btfids/main.c", "#include <libelf.h>\n")

	const configured = "-I__LINUX_BZL_HOST_DEPS__/external/elfutils/libelf"
	variables := map[string]string{"SRCARCH": "x86"}
	shellCalls := 0
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"all"},
		variables,
		kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			CommandLineVariables: map[string]string{
				"HOSTCC":       kconfig.KbuildActionRoleToken("host", "cc"),
				"LIBELF_FLAGS": configured,
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			Shell: func(command string) (string, error) {
				shellCalls++
				if command != "host-pkg-config libelf --cflags" {
					return "", fmt.Errorf("unexpected shell command %q", command)
				}
				return "-Iambient/libelf", nil
			},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if shellCalls != 0 {
		t.Fatalf("command-line LIBELF_FLAGS evaluated source fallback %d times, want zero", shellCalls)
	}

	const target = "tools/bpf/resolve_btfids/main.o"
	var buildProfile *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "tools/build/Makefile.build" {
			buildProfile = &profiles[index]
			break
		}
	}
	if buildProfile == nil {
		t.Fatalf("profiles omit recursive tools/build invocation: %#v", profiles)
	}
	values, err := kconfig.EvaluateCompactKbuildTargetSymbolic(
		*buildProfile, target, "", nil, nil, nil, "HOSTCFLAGS_resolve_btfids", "LIBELF_FLAGS",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOSTCFLAGS_resolve_btfids", "LIBELF_FLAGS"} {
		if got := values[name]; got != configured {
			t.Errorf("recursive %s = %q, want configured command-line value %q", name, got, configured)
		}
	}

	var compileRule *kconfig.KbuildRule
	for index := range buildProfile.Rules {
		if slices.Contains(buildProfile.Rules[index].Targets, target) {
			compileRule = &buildProfile.Rules[index]
			break
		}
	}
	if compileRule == nil || len(compileRule.Recipe) != 1 {
		t.Fatalf("recursive compile rule = %#v, want one source-derived recipe", compileRule)
	}
	command, err := kconfig.EvaluateCompactKbuildTextSymbolic(
		*buildProfile, target, "", compileRule.Prerequisites, compileRule.OrderOnly,
		nil, compileRule.Recipe[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, configured) {
		t.Errorf("recursive host compile command %q omits configured include %q", command, configured)
	}
	if strings.Contains(command, "ambient/libelf") {
		t.Errorf("recursive host compile command %q contains source fallback", command)
	}
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Profile == buildProfile.Name && selection.Target == target && selection.Stage == "host"
	}) {
		t.Fatalf("selections omit recursive host compile target from %q: %#v", buildProfile.Name, selections)
	}
}

func TestEvaluatedKbuildProfilesHonorSourceMakeOverrides(t *testing.T) {
	for _, test := range []struct {
		name, makeOverrides, childAssignment, want string
	}{
		{name: "default inherits command line", want: "-DPARENT"},
		{name: "empty make overrides demotes to environment", makeOverrides: "MAKEOVERRIDES :=", want: "-DLOCAL"},
		{name: "explicit child assignment still wins", makeOverrides: "MAKEOVERRIDES :=", childAssignment: "FLAGS=-DEXPLICIT", want: "-DEXPLICIT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(relative, content string) {
				t.Helper()
				filename := filepath.Join(root, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("Makefile", test.makeOverrides+`
all:
	$(MAKE) -f $(srctree)/scripts/child.mk `+test.childAssignment+` output
`)
			write("scripts/child.mk", `
FLAGS := -DLOCAL
output:
	printf '%s\n' '$(FLAGS)' > $@
`)
			variables := map[string]string{"SRCARCH": "x86"}
			profiles, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
				RootDir: root, Variables: variables,
				CommandLineVariables:    map[string]string{"FLAGS": "-DPARENT"},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var child *kconfig.CompactKbuildProfile
			for index := range profiles {
				if profiles[index].Path == "scripts/child.mk" {
					child = &profiles[index]
					break
				}
			}
			if child == nil {
				t.Fatalf("profiles omit MAKEOVERRIDES child: %#v", profiles)
			}
			values, err := kconfig.EvaluateCompactKbuildTargetSymbolic(*child, "output", "", nil, nil, nil, "FLAGS")
			if err != nil {
				t.Fatal(err)
			}
			if got := values["FLAGS"]; got != test.want {
				var rootProfile *kconfig.CompactKbuildProfile
				for index := range profiles {
					if profiles[index].Path == "Makefile" {
						rootProfile = &profiles[index]
						break
					}
				}
				origin, value := "", ""
				if rootProfile != nil {
					origin, _ = kconfig.EvaluateCompactKbuildTextSymbolic(*rootProfile, "all", "", nil, nil, nil, "$(origin MAKEOVERRIDES)")
					value, _ = kconfig.EvaluateCompactKbuildTextSymbolic(*rootProfile, "all", "", nil, nil, nil, "$(MAKEOVERRIDES)")
				}
				t.Fatalf("child FLAGS = %q, want %q (root MAKEOVERRIDES origin=%q value=%q)", got, test.want, origin, value)
			}
		})
	}
}

func TestKbuildInvocationRequestKeySeparatesEnvironmentFromCommandLine(t *testing.T) {
	request := kbuildInvocationRequest{
		name: "child", makefile: "scripts/child.mk",
		environment: map[string]string{"FLAGS": "parent"},
		variables:   map[string]string{"FLAGS": "child"},
	}
	base := kbuildInvocationRequestKey(request)
	environmentChanged := request
	environmentChanged.environment = map[string]string{"FLAGS": "different"}
	if got := kbuildInvocationRequestKey(environmentChanged); got == base {
		t.Fatal("request identity ignored inherited environment")
	}
	commandLineChanged := request
	commandLineChanged.variables = map[string]string{"FLAGS": "different"}
	if got := kbuildInvocationRequestKey(commandLineChanged); got == base {
		t.Fatal("request identity ignored command-line assignment")
	}
}

func TestKbuildInvocationRequestKeyIncludesProcessTreeProvenance(t *testing.T) {
	request := kbuildInvocationRequest{
		name: "child", makefile: "scripts/child.mk",
		processLocation: kconfig.CompactKbuildInvocationLocation{
			Tree: kconfig.CompactKbuildInvocationSourceTree, Directory: "shared/path",
		},
	}
	object := request
	object.processLocation.Tree = kconfig.CompactKbuildInvocationObjectTree
	if kbuildInvocationRequestKey(request) == kbuildInvocationRequestKey(object) {
		t.Fatal("request identity collapsed source- and object-rooted process directories")
	}
}

func TestKbuildInvocationRequestKeyIncludesInvocationPredecessors(t *testing.T) {
	request := kbuildInvocationRequest{
		name: "child", makefile: "scripts/child.mk",
		invocationPredecessors: []string{"producer-a"},
	}
	changed := request
	changed.invocationPredecessors = []string{"producer-b"}
	if kbuildInvocationRequestKey(request) == kbuildInvocationRequestKey(changed) {
		t.Fatal("request identity ignored source-ordered invocation predecessors")
	}
}

func TestKbuildRecursiveMakeRequestCapturesInlineEnvironment(t *testing.T) {
	invocations, err := kbuildRecursiveMakeInvocations(
		`V=1 confdir=/configured __LINUX_BZL_MAKE__ -f `+kbuildEvalSourceTree+`/scripts/child.mk all`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	want := map[string]string{"V": "1", "confdir": "/configured"}
	if got := invocations[0].request.environment; !maps.Equal(got, want) {
		t.Fatalf("inline recursive Make environment = %#v, want %#v", got, want)
	}
}

func TestKbuildInvocationRequestKeyIncludesVisibleArtifactOwner(t *testing.T) {
	artifact := kconfig.CompactKbuildVisibleArtifact{
		Path: "generated.o", Profile: "producer-a", Target: "target-a",
	}
	request := kbuildInvocationRequest{
		name: "child", makefile: "scripts/child.mk",
		visibleState: kbuildFrontierSet(kbuildFrontierState{}, artifact.Path, kbuildFrontierValue{artifact: artifact}),
	}
	baseKey := kbuildInvocationRequestKey(request)

	profileChanged := request
	profileChanged.visibleState = kbuildFrontierSet(profileChanged.visibleState, artifact.Path, kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{
			Path: artifact.Path, Profile: "producer-b", Target: artifact.Target,
		},
	})
	if got := kbuildInvocationRequestKey(profileChanged); got == baseKey {
		t.Fatal("request key ignores visible artifact producer profile")
	}

	targetChanged := request
	targetChanged.visibleState = kbuildFrontierSet(targetChanged.visibleState, artifact.Path, kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{
			Path: artifact.Path, Profile: artifact.Profile, Target: "target-b",
		},
	})
	if got := kbuildInvocationRequestKey(targetChanged); got == baseKey {
		t.Fatal("request key ignores visible artifact producer target")
	}

	unrelated := kconfig.CompactKbuildVisibleArtifact{
		Path: "unrelated.o", Profile: "unrelated", Target: "unrelated.o",
	}
	forward := request
	forward.visibleState = kbuildFrontierSet(forward.visibleState, unrelated.Path, kbuildFrontierValue{artifact: unrelated})
	reverse := request
	reverse.visibleState = kbuildFrontierSet(kbuildFrontierState{}, unrelated.Path, kbuildFrontierValue{artifact: unrelated})
	reverse.visibleState = kbuildFrontierSet(reverse.visibleState, artifact.Path, kbuildFrontierValue{artifact: artifact})
	if got, want := kbuildInvocationRequestKey(reverse), kbuildInvocationRequestKey(forward); got != want {
		t.Fatalf("insertion-order request key = %q, want canonical state key %q", got, want)
	}
}

func TestCanonicalKbuildInvocationRequestDigestMatchesStableKey(t *testing.T) {
	request := kbuildInvocationRequest{
		name: "child", makefile: "scripts/child.mk", directory: "drivers",
		processLocation: kconfig.CompactKbuildInvocationLocation{
			Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: "drivers",
		},
		entryTargets:           []string{"all", "modules"},
		environment:            map[string]string{"LC_ALL": "C"},
		variables:              map[string]string{"ARCH": "arm", "CC": "gcc"},
		commandLineAutoExport:  map[string]bool{"ARCH": true},
		invocationPredecessors: []string{"producer"},
	}
	request.visibleState = newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "first.order", value: kbuildFrontierValue{
			artifact: kconfig.CompactKbuildVisibleArtifact{Path: "first.order", Profile: "first", Target: "first.order"},
			content:  "first.o\n", exact: true,
		}},
		{path: "second.order", value: kbuildFrontierValue{
			artifact: kconfig.CompactKbuildVisibleArtifact{Path: "second.order", Profile: "second", Target: "second.order"},
		}},
	})
	key := canonicalKbuildInvocationRequestKey(request)
	want := sha256.Sum256([]byte(key))
	if got := canonicalKbuildInvocationRequestDigest(request); got != want {
		t.Fatalf("streamed request digest = %x, want SHA-256(stable key) %x", got, want)
	}
	if got, wantName := kbuildInvocationProfileNameFromDigest(request.name, want), kbuildInvocationProfileNameFromKey(request.name, key); got != wantName {
		t.Fatalf("digest-derived profile name = %q, want key-derived name %q", got, wantName)
	}
	clone := request
	clone.commandLineAutoExport = maps.Clone(request.commandLineAutoExport)
	clone.commandLineAutoExport["unused"] = false
	if !canonicalKbuildInvocationRequestsEqual(request, clone) {
		t.Fatal("canonical request equality rejected a snapshot with the same stable key")
	}
	first, _ := kbuildFrontierGet(clone.visibleState, "first.order")
	first.content = "changed\n"
	clone.visibleState = kbuildFrontierSet(clone.visibleState, "first.order", first)
	if canonicalKbuildInvocationRequestsEqual(request, clone) {
		t.Fatal("canonical request equality ignored exact-content change")
	}
}

func TestSelectedKbuildSelectionsUsePrimaryRoleForMixedExplicitScopes(t *testing.T) {
	profile := selectionRoleProfile(t, `
if_changed = $(if y,,$(cmd_$(1)))
all: proc-macro
proc-macro: private chosen = rustc_procmacro
proc-macro: private cmd_rustc_procmacro = $(RUSTC) --crate-type proc-macro -Clinker-flavor=gcc -Clinker=$(HOSTCC) -o $@ $<
proc-macro: macros.rs
	$(call if_changed,$(chosen))
`, "all")
	selection := selectionByTarget(t, mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"macros.rs": true,
	}), "proc-macro")
	if selection.Scope != "target" || selection.Stage != "target" {
		t.Fatalf("target rustc with host linker selection = %#v, want target scope/stage", selection)
	}
}

func TestSelectedKbuildSelectionsUseHostPrimaryWithTargetAuxiliary(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: host-wrapper
host-wrapper: wrapper.c
	$(HOSTCC) --target-driver=$(CC) -o $@ $<
`, "all")
	selection := selectionByTarget(t, mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"wrapper.c": true,
	}), "host-wrapper")
	if selection.Scope != "host" || selection.Stage != "host" {
		t.Fatalf("host cc with target auxiliary selection = %#v, want host scope/stage", selection)
	}
}

func TestSelectedKbuildSelectionsRejectMixedPrimaryScopes(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: mixed-output
mixed-output: mixed.c
	$(HOSTCC) -c -o $@.host $<
	$(CC) -c -o $@ $<
`, "all")
	_, err := selectedKbuildSelections([]kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"mixed.c": true,
	})
	if err == nil || !strings.Contains(err.Error(), "mixes explicit host primary roles") {
		t.Fatalf("mixed host/target primary action error = %v", err)
	}
}

func TestSelectedKbuildSelectionsClassifyIndirectHostCommandTemplate(t *testing.T) {
	profile := selectionRoleProfile(t, `
if_changed = $(if y,,$(cmd_$(1)))
all: host-tool
host-tool: private chosen = host-cmulti
host-tool: private nested-host-command = $(HOSTCC) -o $@ $<
host-tool: private cmd_host-cmulti = $(nested-host-command)
host-tool: host-tool.c
	$(call if_changed,$(chosen))
`, "all")
	selection := selectionByTarget(t, mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"host-tool.c": true,
	}), "host-tool")
	if selection.Scope != "host" || selection.Stage != "host" {
		t.Fatalf("indirect host command selection = %#v, want host scope/stage", selection)
	}
}

func TestSelectedKbuildSelectionsStopHostPropagationAtExplicitTargetBoundary(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: host-generator
host-generator: generated/header.h host-generator.c
	$(HOSTCC) -o $@ $^
generated/header.h: target-input.o
	cp $< $@
target-input.o: target-input.c
	$(CC) -c -o $@ $<
`, "all")
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"host-generator.c": true, "target-input.c": true,
	})
	for _, selected := range []string{"host-generator", "generated/header.h"} {
		selection := selectionByTarget(t, selections, selected)
		if selection.Scope != "host" || selection.Stage != "host" {
			t.Errorf("%s selection = %#v, want propagated host scope", selected, selection)
		}
	}
	target := selectionByTarget(t, selections, "target-input.o")
	if target.Scope != "target" || target.Stage != "bootstrap" || target.Lifecycle != "target" {
		t.Fatalf("explicit target boundary selection = %#v, want target-scope bootstrap stage", target)
	}
}

func TestSelectedKbuildSelectionsKeepExplicitTargetConsumerAfterHostStage(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: target-consumer
target-consumer: neutral-relocs target-consumer.c
	$(CC) -c -o $@ target-consumer.c
neutral-relocs: host-object
	cp $< $@
host-object: host-object.c
	$(HOSTCC) -o $@ $<
`, "all")
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"target-consumer.c": true, "host-object.c": true,
	})
	host := selectionByTarget(t, selections, "host-object")
	if host.Scope != "host" || host.Stage != "host" {
		t.Fatalf("host producer selection = %#v, want host scope/stage", host)
	}
	neutral := selectionByTarget(t, selections, "neutral-relocs")
	if neutral.Scope != "target" || neutral.Stage != "target" {
		t.Fatalf("neutral host-artifact consumer selection = %#v, want post-host target scope/stage", neutral)
	}
	target := selectionByTarget(t, selections, "target-consumer")
	if target.Scope != "target" || target.Stage != "target" {
		t.Fatalf("explicit target consumer selection = %#v, want post-host target scope/stage", target)
	}
}

func TestSelectedKbuildSelectionsUsePrehostForHostTargetHostTopology(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: final-host
final-host: target-middle host-final.c
	$(HOSTCC) -o $@ $^
target-middle: neutral-first-host target-middle.c
	$(CC) -c -o $@ target-middle.c
neutral-first-host: first-host
	cp $< $@
first-host: first-host.c
	$(HOSTCC) -o $@ $<
`, "all")
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"host-final.c": true, "target-middle.c": true, "first-host.c": true,
	})
	for target, want := range map[string]struct{ scope, stage string }{
		"first-host":         {scope: "host", stage: "prehost"},
		"neutral-first-host": {scope: "target", stage: "bootstrap"},
		"target-middle":      {scope: "target", stage: "bootstrap"},
		"final-host":         {scope: "host", stage: "host"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
}

func TestSelectedKbuildSelectionsRejectUnsupportedHostTargetHostTargetHostTopology(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: final-host
final-host: target-after-host final-host.c
	$(HOSTCC) -o $@ $^
target-after-host: middle-host target-after-host.c
	$(CC) -c -o $@ target-after-host.c
middle-host: target-middle middle-host.c
	$(HOSTCC) -o $@ $^
target-middle: first-host target-middle.c
	$(CC) -c -o $@ target-middle.c
first-host: first-host.c
	$(HOSTCC) -o $@ $<
`, "all")
	_, err := selectedKbuildSelections([]kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"final-host.c": true, "target-after-host.c": true, "middle-host.c": true,
		"target-middle.c": true, "first-host.c": true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires another toolchain-scope alternation") {
		t.Fatalf("unsupported additional toolchain-scope alternation error = %v", err)
	}
}

func TestSelectedKbuildSelectionsKeepCompilerSearchRootsOutOfHostFrontier(t *testing.T) {
	profile := selectionRoleProfileWithSources(t, `
objtree := __LINUX_BZL_OBJECT_TREE__
cmd = $(cmd_$(1))
make-cmd = $(cmd_$(1))
cmd_and_fixdep = $(cmd); fixdep $(depfile) $@ '$(make-cmd)'; rm -f $(depfile)
if_changed_dep = $(cmd_and_fixdep)
cmd_cc_s_c = $(CC) -fmacro-prefix-map=$(objtree)/=. -I$(objtree) -include $(objtree)/include/generated/autoconf.h -S -o $@ $<
cmd_host-cobjs = $(HOSTCC) -I$(objtree)/scripts/mod -c -o $@ $<
all: include/generated/autoconf.h scripts/dtc/dtc-parser.tab.h scripts/mod/symsearch.o final-host
final-host: arch/x86/kernel/asm-offsets.s final-host.c
	$(HOSTCC) -o $@ $^
arch/x86/kernel/asm-offsets.s: arch/x86/kernel/asm-offsets.c
	$(call if_changed_dep,cc_s_c)
include/generated/autoconf.h: config.in
	cp $< $@
scripts/dtc/dtc-parser.tab.h: scripts/dtc/parser.c
	$(HOSTCC) -E -o $@ $<
scripts/mod/symsearch.o: scripts/mod/symsearch.c
	$(call if_changed_dep,host-cobjs)
`, map[string]string{
		"arch/x86/kernel/asm-offsets.c": "int offsets;\n",
		"scripts/dtc/parser.c":          "#define DTC_TOKEN 1\n",
		"scripts/mod/symsearch.c":       "int symbols;\n",
		"final-host.c":                  "int main(void) { return 0; }\n",
		"config.in":                     "#define CONFIG_TEST 1\n",
	}, "all")
	headerArtifact := kconfig.CompactKbuildVisibleArtifact{
		Path: "include/generated/autoconf.h", Profile: profile.Name, Target: "include/generated/autoconf.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{
		headerArtifact,
		{Path: "scripts/dtc/dtc-parser.tab.h", Profile: profile.Name, Target: "scripts/dtc/dtc-parser.tab.h"},
		{Path: "scripts/mod/symsearch.o", Profile: profile.Name, Target: "scripts/mod/symsearch.o"},
	})
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"arch/x86/kernel/asm-offsets.c": true,
		"scripts/dtc/parser.c":          true,
		"scripts/mod/symsearch.c":       true,
		"final-host.c":                  true,
		"config.in":                     true,
	})
	asmOffsets := selectionByTarget(t, selections, "arch/x86/kernel/asm-offsets.s")
	if asmOffsets.Scope != "target" || asmOffsets.Stage != "bootstrap" {
		t.Fatalf("asm-offset selection = %#v, want target/bootstrap", asmOffsets)
	}
	wantHeader := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{headerArtifact})
	if !asmOffsets.UsesInitialObjectTree || asmOffsets.InitialObjectTreeArtifacts != wantHeader {
		t.Fatalf("compiler search/explicit-file frontier = %#v, want only %s", asmOffsets, wantHeader)
	}
	autoconf := selectionByTarget(t, selections, "include/generated/autoconf.h")
	if autoconf.Scope != "target" || autoconf.Stage != "bootstrap" {
		t.Fatalf("exact autoconf producer selection = %#v, want target/bootstrap", autoconf)
	}
	dtcHeader := selectionByTarget(t, selections, "scripts/dtc/dtc-parser.tab.h")
	if dtcHeader.Scope != "host" || dtcHeader.Stage != "host" {
		t.Fatalf("weak opaque-root header selection = %#v, want host/host", dtcHeader)
	}
	symsearch := selectionByTarget(t, selections, "scripts/mod/symsearch.o")
	if symsearch.Scope != "host" || symsearch.Stage != "host" {
		t.Fatalf("symsearch selection = %#v, want host/host", symsearch)
	}
}

func TestSelectedKbuildSelectionsKeepForwardedCompilerSearchRootOutOfHostFrontier(t *testing.T) {
	fixture := func(script string) ([]kconfig.CompactKbuildProfile, map[string]bool) {
		t.Helper()
		root := selectionRoleProfile(t, "all:\n", "all")
		root.Name = "root:forwarded-compiler-frontier"
		scriptsMod := selectionRoleProfileWithSources(t, `
scripts/mod/sumversion.o: scripts/mod/sumversion.c scripts/mod/elfconfig.h
	$(HOSTCC) -c -o $@ $<
scripts/mod/elfconfig.h: scripts/mod/empty.o scripts/mod/mk_elfconfig
	__LINUX_BZL_OBJECT_TREE__/scripts/mod/mk_elfconfig < $< > $@
scripts/mod/empty.o: scripts/mod/empty.c
	$(CC) -c -o $@ $<
scripts/mod/mk_elfconfig: scripts/mod/mk_elfconfig.c
	$(HOSTCC) -o $@ $<
`, map[string]string{
			"scripts/mod/sumversion.c":   "int sumversion;\n",
			"scripts/mod/empty.c":        "int empty;\n",
			"scripts/mod/mk_elfconfig.c": "int main(void) { return 0; }\n",
		}, "scripts/mod/sumversion.o")
		scriptsMod.Name = "child:forwarded-scripts-mod"
		rootBuild := selectionRoleProfileWithSources(t, `
CONFIG_SHELL = sh
c_flags = -I __LINUX_BZL_OBJECT_TREE__ -include __LINUX_BZL_OBJECT_TREE__/include/generated/autoconf.h
final-host: missing-syscalls final-host.c
	$(HOSTCC) -o $@ final-host.c
missing-syscalls: scripts/checksyscalls.sh
	$(CONFIG_SHELL) __LINUX_BZL_SOURCE_TREE__/scripts/checksyscalls.sh $(CC) $(c_flags); printf checked > $@
`, map[string]string{
			"scripts/checksyscalls.sh": script,
			"final-host.c":             "int main(void) { return 0; }\n",
		}, "final-host")
		rootBuild.Name = "child:forwarded-root-build"
		setTestCompactKbuildInitialVisibleArtifacts(t, &rootBuild, []kconfig.CompactKbuildVisibleArtifact{{
			Path: "scripts/mod/sumversion.o", Profile: scriptsMod.Name, Target: "scripts/mod/sumversion.o",
		}})
		root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
			{Target: "all", Profile: scriptsMod.Name, Goals: scriptsMod.EntryTargets},
			{Target: "all", Profile: rootBuild.Name, Goals: rootBuild.EntryTargets},
		}
		return []kconfig.CompactKbuildProfile{root, scriptsMod, rootBuild}, map[string]bool{
			"scripts/mod/sumversion.c":   true,
			"scripts/mod/empty.c":        true,
			"scripts/mod/mk_elfconfig.c": true,
			"final-host.c":               true,
		}
	}

	profiles, satisfied := fixture("#!/bin/sh\nsyscall_list() { grep \"$1\"; }\ndirname \"$0\" >/dev/null\n$* -Wno-error -E -x c - >/dev/null\n")
	selections := mustSelectedKbuildSelections(t, profiles, satisfied)
	for target, want := range map[string]struct{ scope, stage string }{
		"scripts/mod/empty.o":      {scope: "target", stage: "bootstrap"},
		"scripts/mod/mk_elfconfig": {scope: "host", stage: "host"},
		"scripts/mod/elfconfig.h":  {scope: "host", stage: "host"},
		"scripts/mod/sumversion.o": {scope: "host", stage: "host"},
		"missing-syscalls":         {scope: "target", stage: "bootstrap"},
		"final-host":               {scope: "host", stage: "host"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
	missing := selectionByTarget(t, selections, "missing-syscalls")
	if strings.Contains(missing.InitialObjectTreeArtifacts, "sumversion") ||
		strings.Contains(missing.GeneratedObjectTreeArtifacts, "sumversion") {
		t.Fatalf("forwarded compiler search root retained unrelated host object: %#v", missing)
	}

	profiles, satisfied = fixture("#!/bin/sh\nsyscall_list() { grep \"$1\"; }\ndirname \"$0\" >/dev/null\n$* -Wno-error -E -x c - >/dev/null\nprintf '%s\\n' \"$3\" >/dev/null\n")
	controlSelections, err := selectedKbuildSelections(profiles, satisfied)
	if err == nil || !strings.Contains(err.Error(), "requires another toolchain-scope alternation") {
		t.Fatalf("direct positional object-root observation error = %v, selections = %#v; want conservative extra-alternation rejection", err, controlSelections)
	}
}

func TestSelectedKbuildSelectionsProjectWeakEarlyHostOrderAroundTargetBootstrap(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:weak-order"
	earlyHost := selectionRoleProfile(t, `
early-host: early-host-object
	cp $< $@
early-host-object: early-host.c
	$(HOSTCC) -o $@ $<
`, "early-host")
	earlyHost.Name = "child:early-host"
	laterMixed := selectionRoleProfile(t, `
late-host: target-bootstrap.o late-host.c
	$(HOSTCC) -o $@ $^
target-bootstrap.o: target-bootstrap.c
	$(CC) -c -o $@ $<
`, "late-host")
	laterMixed.Name = "child:later-mixed"
	laterMixed.InvocationPredecessors = []string{earlyHost.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlyHost.Name, Goals: earlyHost.EntryTargets},
		{Target: "all", Profile: laterMixed.Name, Goals: laterMixed.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, earlyHost, laterMixed}, map[string]bool{
		"early-host.c": true, "late-host.c": true, "target-bootstrap.c": true,
	})
	for target, want := range map[string]struct{ scope, stage string }{
		"early-host":         {scope: "target", stage: "target"},
		"early-host-object":  {scope: "host", stage: "host"},
		"target-bootstrap.o": {scope: "target", stage: "bootstrap"},
		"late-host":          {scope: "host", stage: "host"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
}

func TestSelectedKbuildSelectionsProjectWeakLaterHostBeforeNativePostHostTarget(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:weak-native-host-order"
	earlyMixed := selectionRoleProfile(t, `
target-after-host: generated.c
	$(CC) -c -o $@ $<
generated.c: target-bootstrap.o host-tool
	cp $< $@
target-bootstrap.o: target-bootstrap.c
	$(CC) -c -o $@ $<
host-tool: host-tool.c
	$(HOSTCC) -o $@ $<
`, "target-after-host")
	earlyMixed.Name = "child:early-mixed"
	laterHost := selectionRoleProfile(t, `
later-host: later-host.c
	$(HOSTCC) -o $@ $<
`, "later-host")
	laterHost.Name = "child:later-host"
	laterHost.InvocationPredecessors = []string{earlyMixed.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: earlyMixed.Name, Goals: earlyMixed.EntryTargets},
		{Target: "all", Profile: laterHost.Name, Goals: laterHost.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, earlyMixed, laterHost}, map[string]bool{
		"target-bootstrap.c": true, "host-tool.c": true, "later-host.c": true,
	})
	for target, want := range map[string]struct{ scope, stage string }{
		"target-bootstrap.o": {scope: "target", stage: "bootstrap"},
		"host-tool":          {scope: "host", stage: "host"},
		"generated.c":        {scope: "target", stage: "target"},
		"later-host":         {scope: "host", stage: "host"},
		"target-after-host":  {scope: "target", stage: "target"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
}

func TestSelectedKbuildSelectionsKeepDemandedSideOutputConsumerAfterHostStage(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:side-output-stage"
	modpost := selectionRoleProfile(t, `
modules.symvers: modpost-input
	$(HOSTCC) -o $@ $<
`, "modules.symvers")
	modpost.Name = "child:modpost"
	vmlinux := selectionRoleProfile(t, `
.vmlinux.export.o: .vmlinux.export.c
	$(CC) -c -o $@ $<
`, ".vmlinux.export.o")
	vmlinux.Name = "child:vmlinux"
	vmlinux.InvocationPredecessors = []string{modpost.Name}
	bootHost := selectionRoleProfile(t, `
mkcpustr: mkcpustr.c
	$(HOSTCC) -o $@ $<
`, "mkcpustr")
	bootHost.Name = "child:boot-host"
	bootHost.InvocationPredecessors = []string{vmlinux.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: modpost.Name, Goals: modpost.EntryTargets},
		{Target: "all", Profile: vmlinux.Name, Goals: vmlinux.EntryTargets},
		{Target: "all", Profile: bootHost.Name, Goals: bootHost.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, modpost, vmlinux, bootHost}, map[string]bool{
		"modpost-input": true,
		"mkcpustr.c":    true,
	})
	for target, want := range map[string]struct{ scope, stage string }{
		"modules.symvers":   {scope: "host", stage: "host"},
		".vmlinux.export.o": {scope: "target", stage: "target"},
		"mkcpustr":          {scope: "host", stage: "host"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
}

func TestSelectedKbuildSelectionsUseRecursiveInvocationExecutionOrderForBootstrap(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	targetChild := selectionRoleProfile(t, `
target-child: target.c
	$(CC) -c -o $@ $<
`, "target-child")
	targetChild.Name = "child:target"
	hostChild := selectionRoleProfile(t, `
host-child: host.c
	$(HOSTCC) -o $@ $<
`, "host-child")
	hostChild.Name = "child:host"
	hostChild.InvocationPredecessors = []string{targetChild.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: targetChild.Name, Goals: targetChild.EntryTargets},
		{Target: "all", Profile: hostChild.Name, Goals: hostChild.EntryTargets},
	}
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, targetChild, hostChild}, map[string]bool{
		"target.c": true,
		"host.c":   true,
	})
	target := selectionByTarget(t, selections, "target-child")
	if target.Scope != "target" || target.Stage != "bootstrap" {
		t.Fatalf("execution predecessor selection = %#v, want target bootstrap", target)
	}
	host := selectionByTarget(t, selections, "host-child")
	if host.Scope != "host" || host.Stage != "host" {
		t.Fatalf("later recursive host selection = %#v, want host", host)
	}
}

func TestSelectedKbuildSelectionsBootstrapExactInitialObjectTreeOwner(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:visible-frontier"
	header := selectionRoleProfile(t, `
generated/header.h: header.in
	cp $< $@
`, "generated/header.h")
	header.Name = "child:generated-header"
	fixdep := selectionRoleProfile(t, `
scripts/basic/fixdep: scripts/basic/fixdep.c
	$(HOSTCC) -o $@ $<
`, "scripts/basic/fixdep")
	fixdep.Name = "child:fixdep"
	mixed := selectionRoleProfile(t, `
if_changed_dep = $(cmd_$(1)); __LINUX_BZL_OBJECT_TREE__/scripts/basic/fixdep dep $@ > .$(@F).cmd
final-host: target-bootstrap.o final-host.c
	$(HOSTCC) -o $@ $^
target-bootstrap.o: private cmd_compile = $(CC) -I__LINUX_BZL_OBJECT_TREE__/generated -include __LINUX_BZL_OBJECT_TREE__/generated/header.h -c -o $@ $<
target-bootstrap.o: target-bootstrap.c
	$(call if_changed_dep,compile)
`, "final-host")
	mixed.Name = "child:mixed"
	mixed.InvocationPredecessors = []string{header.Name, fixdep.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &mixed, []kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/header.h", Profile: header.Name, Target: "generated/header.h",
	}, {
		Path: "scripts/basic/fixdep", Profile: fixdep.Name, Target: "scripts/basic/fixdep",
	}})
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: header.Name, Goals: header.EntryTargets},
		{Target: "all", Profile: fixdep.Name, Goals: fixdep.EntryTargets},
		{Target: "all", Profile: mixed.Name, Goals: mixed.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, header, fixdep, mixed}, map[string]bool{
		"header.in": true, "scripts/basic/fixdep.c": true, "final-host.c": true, "target-bootstrap.c": true,
	})
	generated := selectionByTarget(t, selections, "generated/header.h")
	if generated.Scope != "target" || generated.Stage != "bootstrap" {
		t.Fatalf("visible object-tree owner selection = %#v, want target/bootstrap", generated)
	}
	fixdepSelection := selectionByTarget(t, selections, "scripts/basic/fixdep")
	if fixdepSelection.Scope != "host" || fixdepSelection.Stage != "prehost" {
		t.Fatalf("bootstrap executable owner selection = %#v, want host/prehost", fixdepSelection)
	}
	bootstrap := selectionByTarget(t, selections, "target-bootstrap.o")
	wantInitialArtifacts := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/header.h", Profile: header.Name, Target: "generated/header.h",
	}, {
		Path: "scripts/basic/fixdep", Profile: fixdep.Name, Target: "scripts/basic/fixdep",
	}})
	if bootstrap.Scope != "target" || bootstrap.Stage != "bootstrap" || !bootstrap.UsesInitialObjectTree ||
		bootstrap.InitialObjectTreeArtifacts != wantInitialArtifacts {
		t.Fatalf("object-tree consumer selection = %#v, want target/bootstrap with frontier usage", bootstrap)
	}
	host := selectionByTarget(t, selections, "final-host")
	if host.Scope != "host" || host.Stage != "host" {
		t.Fatalf("host consumer selection = %#v, want host/host", host)
	}
}

func TestSelectedKbuildSelectionsUseInitialObjectTreeToolForImplicitRuleViability(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:dtc-visible-frontier"
	dtc := selectionRoleProfile(t, `
scripts/dtc/dtc: scripts/dtc/dtc.c
	$(HOSTCC) -o $@ $<
unrelated/frontier.stamp: unrelated/frontier.in
	cp $< $@
`, "scripts/dtc/dtc", "unrelated/frontier.stamp")
	dtc.Name = "child:scripts-dtc"
	consumer := selectionRoleProfileWithSources(t, `
DTC = __LINUX_BZL_OBJECT_TREE__/scripts/dtc/dtc
MISSING_DTC = __LINUX_BZL_OBJECT_TREE__/scripts/dtc/missing-dtc
if_changed = $(cmd_$(1))
cmd_missing_dtc = $(HOSTCC) -E -o $@.tmp $<; $(MISSING_DTC) -o $@ $@.tmp
cmd_dtc = $(HOSTCC) -E -o $@.tmp $<; $(DTC) -o $@ $@.tmp
drivers/of/%.dtb: drivers/of/%.dts $(MISSING_DTC) FORCE
	$(call if_changed,missing_dtc)
drivers/of/%.dtb: drivers/of/%.dts $(DTC) FORCE
	$(call if_changed,dtc)
drivers/of/%.dtb.S: drivers/of/%.dtb
	cp $< $@
drivers/of/%.dtb.o: drivers/of/%.dtb.S
	$(CC) -c -o $@ $<
drivers/of/built-in.a: drivers/of/empty_root.dtb.o
	$(LD) -r -o $@ $<
`, map[string]string{
		"drivers/of/empty_root.dts": "/dts-v1/;\n/ {};\n",
	}, "drivers/of/built-in.a")
	consumer.Name = "child:drivers-of"
	consumer.InvocationPredecessors = []string{dtc.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "scripts/dtc/dtc", Profile: dtc.Name, Target: "scripts/dtc/dtc"},
		{Path: "unrelated/frontier.stamp", Profile: dtc.Name, Target: "unrelated/frontier.stamp"},
	})
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: dtc.Name, Goals: dtc.EntryTargets},
		{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, dtc, consumer}, map[string]bool{
		"scripts/dtc/dtc.c":         true,
		"unrelated/frontier.in":     true,
		"drivers/of/empty_root.dts": true,
	})
	for target, want := range map[string]struct{ scope, stage string }{
		"scripts/dtc/dtc":             {scope: "host", stage: "host"},
		"drivers/of/empty_root.dtb":   {scope: "host", stage: "host"},
		"drivers/of/empty_root.dtb.S": {scope: "target", stage: "target"},
		"drivers/of/empty_root.dtb.o": {scope: "target", stage: "target"},
		"drivers/of/built-in.a":       {scope: "target", stage: "target"},
	} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != want.scope || selection.Stage != want.stage {
			t.Errorf("%s selection = %#v, want %s/%s", target, selection, want.scope, want.stage)
		}
	}
	dtb := selectionByTarget(t, selections, "drivers/of/empty_root.dtb")
	wantArtifact := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "scripts/dtc/dtc", Profile: dtc.Name, Target: "scripts/dtc/dtc",
	}})
	if !dtb.UsesInitialObjectTree || dtb.InitialObjectTreeArtifacts != wantArtifact {
		t.Fatalf("DTB selection = %#v, want exact generated DTC owner %q", dtb, wantArtifact)
	}
}

func TestSelectedKbuildSelectionsUseTargetContextObjForObjectTreeSnapshot(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture string
	}{
		{name: "command template", fixture: `
if_changed = $(cmd_$(1))
cmd_host-csingle = $(HOSTCC) -I $(obj) -include $(obj)/generated.h -o $@ $<
lib/crc/gen_crc32table: lib/crc/gen_crc32table.c
	$(call if_changed,host-csingle)
		`},
		{name: "direct recipe", fixture: `
lib/crc/gen_crc32table: lib/crc/gen_crc32table.c
	$(HOSTCC) -I$(obj) -include $(obj)/generated.h -o $@ $<
		`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := selectionRoleProfile(t, "all:\n", "all")
			root.Name = "root:target-context-obj"
			scoped := selectionRoleProfile(t, `
lib/crc/generated.h: header.in
	cp $< $@
`, "lib/crc/generated.h")
			scoped.Name = "child:target-context-scoped"
			unrelated := selectionRoleProfile(t, `
unrelated/large.o: unrelated.in
	cp $< $@
`, "unrelated/large.o")
			unrelated.Name = "child:target-context-unrelated"
			unrelated.InvocationPredecessors = []string{scoped.Name}
			profile := selectionRoleProfile(t, test.fixture, "lib/crc/gen_crc32table")
			profile.Name = "child:target-context-consumer"
			profile.Directory = "lib/crc"
			profile.InvocationPredecessors = []string{unrelated.Name}
			setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []kconfig.CompactKbuildVisibleArtifact{
				{Path: "lib/crc/generated.h", Profile: scoped.Name, Target: "lib/crc/generated.h"},
				{Path: "unrelated/large.o", Profile: unrelated.Name, Target: "unrelated/large.o"},
			})
			root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
				{Target: "all", Profile: scoped.Name, Goals: scoped.EntryTargets},
				{Target: "all", Profile: unrelated.Name, Goals: unrelated.EntryTargets},
				{Target: "all", Profile: profile.Name, Goals: profile.EntryTargets},
			}
			selection := selectionByTarget(t, mustSelectedKbuildSelections(
				t, []kconfig.CompactKbuildProfile{root, scoped, unrelated, profile}, map[string]bool{
					"header.in":                true,
					"unrelated.in":             true,
					"lib/crc/gen_crc32table.c": true,
				},
			), "lib/crc/gen_crc32table")
			wantInitialArtifacts := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
				Path: "lib/crc/generated.h", Profile: scoped.Name, Target: "lib/crc/generated.h",
			}})
			if !selection.UsesInitialObjectTree || selection.InitialObjectTreeArtifacts != wantInitialArtifacts {
				t.Fatalf("target-context $(obj) selection = %#v, want exact object-tree frontier %q", selection, wantInitialArtifacts)
			}
		})
	}
}

func TestSelectedKbuildSelectionsResolveDynamicFilterObjectTreeFrontierAtReplay(t *testing.T) {
	const (
		target        = "scripts/mod/devicetable-offsets.s"
		visibleHeader = "arch/x86/include/generated/uapi/asm/types.h"
		unrelated     = "scripts/mod/file2alias.o"
	)
	targetIdentity := "sha256-" + strings.Repeat("6a", 32)
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, targetIdentity)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, false),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	probeOptions := kconfig.KbuildProbeWorkloadOptions{Target: kconfig.KbuildProbeScopeOptions{
		Architecture: "x86", SourceArchitecture: "x86", Facts: facts,
		Tools: map[string]string{"cc": "/configured/target/cc"},
	}}

	rootProfile := selectionRoleProfile(t, "all:\n", "all")
	rootProfile.Name = "root:dynamic-filter-frontier"
	headerProfile := selectionRoleProfile(t, `
arch/x86/include/generated/uapi/asm/types.h: include/uapi/asm-generic/types.h
	cp $< $@
scripts/mod/file2alias.o: scripts/mod/file2alias.c
	$(CC) -c -o $@ $<
`, visibleHeader, unrelated)
	headerProfile.Name = "child:dynamic-filter-producers"
	visibleArtifacts := []kconfig.CompactKbuildVisibleArtifact{
		{Path: visibleHeader, Profile: headerProfile.Name, Target: visibleHeader},
		{Path: unrelated, Profile: headerProfile.Name, Target: unrelated},
	}

	const compilerFixture = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
if_changed_dep = $(cmd_$(1))
CC_FLAGS_DYNAMIC := %s
c_flags = -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi $(CC_FLAGS_DYNAMIC)
cmd_cc_s_c = $(CC) $(filter-out $(DEBUG_CFLAGS) $(CC_FLAGS_DYNAMIC), $(c_flags)) -fverbose-asm -S -o $@ $<
scripts/mod/devicetable-offsets.s: scripts/mod/devicetable-offsets.c FORCE
	$(call if_changed_dep,cc_s_c)
`
	for _, test := range []struct {
		name, dynamicValue string
		usesFrontier       bool
	}{
		{name: "keep exact include frontier", dynamicValue: "$(call cc-option,-flto)", usesFrontier: true},
		{
			name: "remove include frontier",
			dynamicValue: "$(subst -fdrop-generated,-I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi," +
				"$(call cc-option,-fdrop-generated))",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profileRoot := t.TempDir()
			makefile := filepath.Join(profileRoot, "Makefile")
			fixture := selectionRoleFixtureMakefile(fmt.Sprintf(compilerFixture, test.dynamicValue))
			if err := os.WriteFile(makefile, []byte(fixture), 0o644); err != nil {
				t.Fatal(err)
			}
			workload := func(scopes *kconfig.KbuildProbeScopes) ([]kconfig.CompactKbuildSelection, error) {
				options, optionsErr := scopes.Options("target", kconfig.KbuildOptions{
					RootDir: profileRoot,
					Variables: map[string]string{
						"CC":      probeOptions.Target.Tools["cc"],
						"SRCARCH": "x86",
						"objtree": kbuildEvalObjectTree,
						"srctree": kbuildEvalSourceTree,
					},
					SourceRoots: map[string]string{
						kbuildEvalSourceTree: profileRoot,
						kbuildEvalObjectTree: profileRoot,
					},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureTargetEvaluator:  true,
				})
				if optionsErr != nil {
					return nil, optionsErr
				}
				parsed, parseErr := kconfig.ParseKbuildFileTree(makefile, options)
				if parseErr != nil {
					return nil, parseErr
				}
				consumer, profileErr := kconfig.NewCompactKbuildProfile(
					"child:dynamic-filter-consumer", makefile, profileRoot, parsed,
				)
				if profileErr != nil {
					return nil, profileErr
				}
				consumer.EntryTargets = []string{target}
				consumer.InvocationPredecessors = []string{headerProfile.Name}
				setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, visibleArtifacts)
				if err := kconfig.SetCompactKbuildProfileInvocationLocation(&consumer, kconfig.CompactKbuildInvocationLocation{
					Tree: kconfig.CompactKbuildInvocationObjectTree,
				}); err != nil {
					return nil, err
				}
				root := rootProfile
				root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
					{Target: "all", Profile: headerProfile.Name, Goals: headerProfile.EntryTargets},
					{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
				}
				return selectedKbuildSelections([]kconfig.CompactKbuildProfile{root, headerProfile, consumer}, map[string]bool{
					"include/uapi/asm-generic/types.h":  true,
					"scripts/mod/file2alias.c":          true,
					"scripts/mod/devicetable-offsets.c": true,
				})
			}

			discovery, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(discovery.Plan.Nodes); got != 1 {
				t.Fatalf("discovery plan has %d nodes, want one", got)
			}
			discoverySelection := selectionByTarget(t, discovery.Value, target)
			if discoverySelection.UsesInitialObjectTree || discoverySelection.InitialObjectTreeArtifacts != "" {
				t.Fatalf("nil-oracle selection conservatively captured hidden frontier: %#v", discoverySelection)
			}

			resultRoot := t.TempDir()
			node := discovery.Plan.Nodes[0]
			request := discovery.Plan.Requests[node.RequestID]
			steps := make([]kconfig.ProbeStepResult, len(request.Steps))
			for index, step := range request.Steps {
				steps[index] = kconfig.ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
			}
			value := true
			writeTestProbeResult(t, resultRoot, kconfig.ProbeResult{
				Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
				Scope: node.Scope, ToolsetIdentity: targetIdentity, Kind: "boolean", Boolean: &value, Steps: steps,
			})
			oracle, err := kconfig.NewProbeResultOracleFromTrees(
				map[string]string{"target": resultRoot}, discovery.Plan.Toolsets,
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			selection := selectionByTarget(t, replay.Value, target)
			if test.usesFrontier {
				wantArtifacts := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts(visibleArtifacts[:1])
				if !selection.UsesInitialObjectTree || selection.InitialObjectTreeArtifacts != wantArtifacts {
					t.Fatalf("replay selection = %#v, want only exact generated UAPI frontier %q", selection, wantArtifacts)
				}
			} else if selection.UsesInitialObjectTree || selection.InitialObjectTreeArtifacts != "" {
				t.Fatalf("filter-removed replay selection retained object-tree frontier: %#v", selection)
			}
		})
	}
}

func TestSelectedKbuildSelectionsScopeSourceScriptInitialObjectTreeArtifacts(t *testing.T) {
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:script-visible-frontier"
	header := selectionRoleProfile(t, `
generated/header.h: header.in
	cp $< $@
`, "generated/header.h")
	header.Name = "child:script-header"
	unrelated := selectionRoleProfile(t, `
unrelated/large.o: unrelated.in
	cp $< $@
`, "unrelated/large.o")
	unrelated.Name = "child:script-unrelated"
	unrelated.InvocationPredecessors = []string{header.Name}
	consumer := selectionRoleProfileWithSources(t, `
CONFIG_SHELL = sh
export OBSERVED_ROOT = __LINUX_BZL_OBJECT_TREE__
script-consumer.o: script-consumer.c
	$(CONFIG_SHELL) __LINUX_BZL_SOURCE_TREE__/scripts/scoped.sh; $(CC) -c -o $@ $<
`, map[string]string{
		"scripts/scoped.sh": "#!/bin/sh\nprintf '%s\\n' \"${OBSERVED_ROOT}/generated/header.h\" >/dev/null\n",
	}, "script-consumer.o")
	consumer.Name = "child:script-consumer"
	consumer.InvocationPredecessors = []string{unrelated.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []kconfig.CompactKbuildVisibleArtifact{
		{Path: "generated/header.h", Profile: header.Name, Target: "generated/header.h"},
		{Path: "unrelated/large.o", Profile: unrelated.Name, Target: "unrelated/large.o"},
	})
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: header.Name, Goals: header.EntryTargets},
		{Target: "all", Profile: unrelated.Name, Goals: unrelated.EntryTargets},
		{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
	}

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{root, header, unrelated, consumer}, map[string]bool{
		"header.in": true, "unrelated.in": true, "script-consumer.c": true,
	})
	selection := selectionByTarget(t, selections, "script-consumer.o")
	wantInitialArtifacts := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated/header.h", Profile: header.Name, Target: "generated/header.h",
	}})
	if !selection.UsesInitialObjectTree || selection.InitialObjectTreeArtifacts != wantInitialArtifacts {
		t.Fatalf("source-script consumer selection = %#v, want only generated/header.h from visible frontier", selection)
	}
}

func TestSelectedKbuildSelectionsBootstrapWinsForSharedNeutralDiamond(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: first-host second-host
first-host: neutral shared-host.c
	$(HOSTCC) -o $@ $^
second-host: target-boundary second-host.c
	$(HOSTCC) -o $@ $^
target-boundary: neutral target.c
	$(CC) -c -o $@ target.c
neutral: neutral.in
	cp $< $@
`, "all")
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"shared-host.c": true,
		"second-host.c": true,
		"target.c":      true,
		"neutral.in":    true,
	})
	for _, target := range []string{"target-boundary", "neutral"} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != "target" || selection.Stage != "bootstrap" {
			t.Errorf("shared diamond %s selection = %#v, want bootstrap precedence", target, selection)
		}
	}
	for _, target := range []string{"first-host", "second-host"} {
		selection := selectionByTarget(t, selections, target)
		if selection.Scope != "host" || selection.Stage != "host" {
			t.Errorf("shared diamond %s selection = %#v, want host", target, selection)
		}
	}
}

func TestSelectedKbuildSelectionsResolveSharedUtilityFromClosureScope(t *testing.T) {
	targetContract := &hostKbuildContract{MakeVariables: map[string]string{"AWK": "awk"}}
	hostContract := &hostKbuildContract{MakeVariables: map[string]string{"AWK": "awk", "HOSTCC": "cc"}}
	commandLine, err := kbuildCommandLineVariables(targetContract, hostContract, nil)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(filename, []byte(`
all: tools/objtool/objtool target-generated
tools/objtool/objtool: tools/objtool/arch/x86/lib/inat-tables.c host.c
	$(HOSTCC) -o $@ $^
tools/objtool/arch/x86/lib/inat-tables.c: tools/objtool/arch/x86/lib/inat.awk inat.h
	$(AWK) -f $< inat.h > $@
target-generated: target.in
	$(AWK) '{ print }' $< > $@
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(filename, kconfig.KbuildOptions{
		CommandLineVariables:    commandLine,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("root:shared-utility", filename, filepath.Dir(filename), parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		"host.c": true, "tools/objtool/arch/x86/lib/inat.awk": true, "inat.h": true, "target.in": true,
	})
	for _, selected := range []string{"tools/objtool/objtool", "tools/objtool/arch/x86/lib/inat-tables.c"} {
		selection := selectionByTarget(t, selections, selected)
		if selection.Scope != "host" || selection.Stage != "host" {
			t.Errorf("%s selection = %#v, want host closure scope", selected, selection)
		}
	}
	target := selectionByTarget(t, selections, "target-generated")
	if target.Scope != "target" || target.Stage != "target" {
		t.Fatalf("target utility selection = %#v, want target scope", target)
	}
}

func TestSelectedKbuildSelectionsKeepPrepareLifecycleSeparateFromHostScope(t *testing.T) {
	profile := selectionRoleProfile(t, `
all: prepare kernel.o
prepare: generated-host-tool
generated-host-tool: host-tool.c
	$(HOSTCC) -o $@ $<
kernel.o: kernel.c
	$(CC) -c -o $@ $<
.PHONY: all prepare
`, "all", "prepare")
	selections, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"host-tool.c": true, "kernel.c": true},
		nil, "", []string{"prepare"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostTool := selectionByTarget(t, selections, "generated-host-tool")
	if hostTool.Lifecycle != "prep" || hostTool.Scope != "host" || hostTool.Stage != "host" {
		t.Fatalf("prepare host tool selection = %#v, want prep lifecycle with host scope/stage", hostTool)
	}
	kernel := selectionByTarget(t, selections, "kernel.o")
	if kernel.Lifecycle != "target" || kernel.Scope != "target" || kernel.Stage != "target" {
		t.Fatalf("kernel selection = %#v, want target lifecycle/scope/stage", kernel)
	}
}

func TestSelectedKbuildSelectionsIgnoreHostprogsClassificationWithoutHostRole(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "root:no-hostprogs-heuristic",
		EntryTargets: []string{"helper"},
		Generated:    []kconfig.KbuildTarget{{Kind: "hostprogs", Target: "helper"}},
		Rules:        []kconfig.KbuildRule{{Targets: []string{"helper"}, Recipe: []string{"cp source helper"}}},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)
	selection := selectionByTarget(t, mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, nil), "helper")
	if selection.Scope != "target" || selection.Stage != "target" {
		t.Fatalf("neutral hostprogs declaration selected %#v, want target default", selection)
	}
}

func TestKbuildProfileCatchAllImplicitRuleRequiresExistingShippedSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "firmware_shipped"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	index, err := newKbuildSourceInputIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	satisfied := kbuildSatisfiedTargets(index)
	if !satisfied["firmware_shipped"] {
		t.Fatalf("declared source index was not added to satisfied targets: %#v", satisfied)
	}
	profile := kconfig.CompactKbuildProfile{
		Name:         "source-selected-root-without-magic-name",
		EntryTargets: []string{"firmware"},
		Rules: []kconfig.KbuildRule{{
			Targets: []string{"%"}, Prerequisites: []string{"%_shipped"}, Recipe: []string{"cp $< $@"},
		}},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	rules := kbuildProfileRulesForTargetIndexed(profile, newKbuildProfileTargetIndex(profile, nil), "firmware", satisfied)
	if got, want := len(rules), 1; got != want || !slices.Contains(rules[0].Targets, "%") {
		t.Fatalf("selected firmware rules = %#v, want the viable shipped-source rule", rules)
	}
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, satisfied)
	if got, want := len(selections), 1; got != want || selections[0].Target != "firmware" {
		t.Fatalf("selected firmware actions = %#v, want firmware", selections)
	}
}

func TestKbuildInvocationInputIndexFollowsSymlinkedSourceRoots(t *testing.T) {
	kernel := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kernel, "arch", "x86", "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kernel, "arch", "x86", "boot", "mkcpustr.c"), []byte("int main(void) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "vendor.c"), []byte("int vendor;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir()
	kernelLink := filepath.Join(links, "kernel")
	moduleLink := filepath.Join(links, "module")
	if err := os.Symlink(kernel, kernelLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(module, moduleLink); err != nil {
		t.Fatal(err)
	}

	index, err := newKbuildInvocationInputIndex(kernelLink, kernelLink, map[string]string{
		kbuildEvalSourceTree + "/drivers/vendor": moduleLink,
	})
	if err != nil {
		t.Fatal(err)
	}
	satisfied := kbuildSatisfiedTargets(index)
	for _, source := range []string{"arch/x86/boot/mkcpustr.c", "drivers/vendor/vendor.c"} {
		if !satisfied[source] {
			t.Fatalf("symlinked source %q absent from input index: %#v", source, index.files)
		}
	}
}

func TestKbuildProfileImplicitRuleIdentityStopsRecursiveTargetGrowth(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name: "driver:scripts/Makefile.vmlinux@.#fixture",
		Rules: []kconfig.KbuildRule{{
			Targets: []string{"%"}, Prerequisites: []string{"%_shipped"}, Recipe: []string{"cp $< $@"},
		}},
	}

	index := newKbuildProfileTargetIndex(profile, nil)
	if kbuildProfileTargetCanBeMadeIndexed(profile, index, "__default", map[string]bool{}, map[string]bool{}, map[int]bool{}) {
		t.Fatal("self-recursive catch-all implicit rule made an unanchored target viable")
	}
	if rules := kbuildProfileRulesForTargetIndexed(profile, index, "__default", map[string]bool{}); len(rules) != 0 {
		t.Fatalf("unanchored implicit rule selected for __default: %#v", rules)
	}
}

func TestSelectedKbuildSelectionsExpandGroupedPeerPrerequisitesFromOneEntry(t *testing.T) {
	profile := selectionRoleProfile(t, `
z-trigger a-peer &: FORCE
	$(HOSTCC) -o $@
a-peer: generated.dep
generated.dep: FORCE
	$(CC) -o $@
	`, "z-trigger")
	control, err := kconfig.EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile = control.Profile

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{})
	if got, want := len(selections), 3; got != want {
		t.Fatalf("grouped selected closure = %#v, want exactly %d actions", selections, want)
	}
	for _, target := range []string{"z-trigger", "a-peer"} {
		selection := selectionByTarget(t, selections, target)
		// z-trigger is deliberately lexically later than a-peer. GroupedTrigger
		// is the lowering contract for the shared recipe's $@/$^ context and must
		// therefore retain discovery order across the final selection sort.
		if got, want := selection.GroupedTrigger, "z-trigger"; got != want {
			t.Errorf("grouped peer %q trigger = %q, want first-reached %q: %#v", target, got, want, selection)
		}
		if selection.Scope != "host" || selection.Stage != "host" {
			t.Errorf("grouped peer %q physical trigger scope/stage = %s/%s, want host/host: %#v", target, selection.Scope, selection.Stage, selection)
		}
	}
	dependency := selectionByTarget(t, selections, "generated.dep")
	if dependency.GroupedTrigger != "" {
		t.Fatalf("ordinary grouped-peer prerequisite has trigger %q: %#v", dependency.GroupedTrigger, dependency)
	}
	if dependency.Scope != "target" || dependency.Stage != "bootstrap" {
		t.Fatalf("grouped peer dependency scope/stage = %s/%s, want target/bootstrap: %#v", dependency.Scope, dependency.Stage, dependency)
	}
}

func TestSelectedKbuildSelectionsExpandHistoricalGroupedPatternPeers(t *testing.T) {
	profile := selectionRoleProfile(t, `
%.z %.a: FORCE
	$(HOSTCC) -o $@
fixture.a: generated.dep
generated.dep: FORCE
	$(CC) -o $@
	`, "fixture.z")
	control, err := kconfig.EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile = control.Profile

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{})
	if got, want := len(selections), 3; got != want {
		t.Fatalf("historical grouped selected closure = %#v, want exactly %d actions", selections, want)
	}
	for _, target := range []string{"fixture.z", "fixture.a"} {
		selection := selectionByTarget(t, selections, target)
		if got, want := selection.GroupedTrigger, "fixture.z"; got != want {
			t.Errorf("historical grouped peer %q trigger = %q, want %q: %#v", target, got, want, selection)
		}
	}
	selectionByTarget(t, selections, "generated.dep")
}

func TestEvaluatedKbuildProfilesPreserveCommandLineGoalOrderForGroupedTrigger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
z-trigger a-peer &: common.in
	touch $@
a-peer: peer-only.source
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"common.in", "peer-only.source"} {
		if err := os.WriteFile(filepath.Join(root, source), []byte(source+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root, root, []string{"z-trigger", "a-peer"}, variables,
		kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 0 || !slices.Equal(profiles[0].EntryTargets, []string{"z-trigger", "a-peer"}) {
		t.Fatalf("root entry targets = %#v, want command-line order preserved", profiles)
	}
	for _, target := range []string{"z-trigger", "a-peer"} {
		selection := selectionByTarget(t, selections, target)
		if selection.GroupedTrigger != "z-trigger" {
			t.Fatalf("selection %q trigger = %q, want first command-line goal z-trigger", target, selection.GroupedTrigger)
		}
	}
}

func TestEvaluatedKbuildProfilesSkipSatisfiedFirstGroupedGoalWhenSelectingTrigger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
a-peer z-trigger &: common.in
	touch $@
a-peer: peer-only.source
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"a-peer":           "already satisfied\n",
		"common.in":        "common\n",
		"peer-only.source": "peer\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root, root, []string{"a-peer", "z-trigger"}, variables,
		kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 0 || !slices.Equal(profiles[0].EntryTargets, []string{"a-peer", "z-trigger"}) {
		t.Fatalf("root entry targets = %#v, want original multi-goal order", profiles)
	}
	for _, target := range []string{"a-peer", "z-trigger"} {
		selection := selectionByTarget(t, selections, target)
		if selection.GroupedTrigger != "z-trigger" {
			t.Fatalf("selection %q trigger = %q, want missing second goal z-trigger", target, selection.GroupedTrigger)
		}
	}
}

func TestEvaluatedKbuildProfilesFollowIndirectForceBeforeBindingGroupedTrigger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
a-peer z-trigger &: stamp
	touch $@
stamp: FORCE
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-peer", "stamp"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("already present\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	variables := map[string]string{"SRCARCH": "x86"}
	_, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root, root, []string{"a-peer", "z-trigger"}, variables,
		kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"a-peer", "z-trigger"} {
		selection := selectionByTarget(t, selections, target)
		if selection.GroupedTrigger != "a-peer" {
			t.Fatalf("selection %q trigger = %q, want first peer made stale through stamp: FORCE", target, selection.GroupedTrigger)
		}
	}
}

func TestKbuildSourceSatisfactionHonorsAlwaysRunSemantics(t *testing.T) {
	profile := selectionRoleProfile(t, `
.PHONY: phony-target phony-prerequisite
ordinary:
	touch $@
phony-target:
	touch $@
forced: FORCE
	touch $@
forced-by-phony: phony-prerequisite
	touch $@
order-only-force: | FORCE
	touch $@
double-colon::
	touch $@
indirect: stamp
	touch $@
stamp: FORCE
cycle-a: cycle-b
cycle-b: cycle-a
`, "ordinary")
	index := newKbuildProfileTargetIndex(profile, nil)
	satisfied := map[string]bool{
		"ordinary": true, "phony-target": true, "forced": true,
		"forced-by-phony": true, "order-only-force": true, "double-colon": true,
		"indirect": true, "stamp": true, "cycle-a": true, "cycle-b": true,
	}
	currentness := newKbuildProfileTargetSatisfaction(profile, index, satisfied)
	for target, want := range map[string]bool{
		"ordinary":         true,
		"phony-target":     false,
		"forced":           false,
		"forced-by-phony":  false,
		"order-only-force": true,
		"double-colon":     false,
		"indirect":         false,
		"cycle-a":          true,
		"cycle-b":          true,
	} {
		if got := currentness.targetIsSatisfied(target); got != want {
			t.Errorf("target %q satisfaction = %t, want %t", target, got, want)
		}
	}
}

func TestEffectiveRecipeSelectionFeedsSelectionRecursiveAndVisibleWalkers(t *testing.T) {
	selectionProfile := selectionRoleProfile(t, `
all:
	$(HOSTCC) -o overridden
all:
	$(CC) -o $@
`, "all")
	selection := selectionByTarget(
		t,
		mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{selectionProfile}, map[string]bool{}),
		"all",
	)
	if selection.Scope != "target" {
		t.Fatalf("ordinary overridden recipe polluted selected scope: %#v", selection)
	}

	recursiveProfile := selectionRoleProfile(t, `
all:
	$(MAKE) -f overridden.mk old
all:
	$(MAKE) -f effective.mk new
`, "all")
	plan, err := selectedKbuildRecursiveMakePlan(recursiveProfile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].request.makefile, "effective.mk") || strings.Contains(plan[0].request.makefile, "overridden.mk") {
		t.Fatalf("ordinary recursive plan = %#v, want only effective.mk", plan)
	}

	doubleColonProfile := selectionRoleProfile(t, `
all::
	$(MAKE) -f first.mk first
all::
	$(MAKE) -f second.mk second
`, "all")
	plan, err = selectedKbuildRecursiveMakePlan(doubleColonProfile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || !strings.Contains(plan[0].request.makefile, "first.mk") || !strings.Contains(plan[1].request.makefile, "second.mk") {
		t.Fatalf("double-colon recursive plan = %#v, want both independent recipes in order", plan)
	}

	visibleProfile := selectionRoleProfile(t, `
all:
	touch $@
all:
	echo no-output
`, "all")
	var completion *kbuildRecursiveMakeFrontier
	_, err = selectedKbuildRecursiveMakePlanWithControlAndCompletion(
		visibleProfile, map[string]bool{}, nil, &completion,
	)
	if err != nil {
		t.Fatal(err)
	}
	visible := kbuildRecursiveMakeFrontierArtifactEvents(completion)
	if slices.ContainsFunc(visible, func(artifact kconfig.CompactKbuildVisibleArtifact) bool { return artifact.Path == "all" }) {
		t.Fatalf("overridden materializing recipe leaked into completion frontier: %#v", visible)
	}
}

func TestGroupedAuthorityControlsRecursivePlanAndCompletionFrontier(t *testing.T) {
	profile := selectionRoleProfile(t, `
root: left1 right
left1: left2
left2: z-trigger
right: a-peer
a-peer z-trigger &: common
	touch $@; $(MAKE) -f $@.mk z-child a-child
a-peer: peer-effect
peer-effect:
	$(MAKE) -f peer-effect.mk
common:
`, "root")
	control, err := kconfig.EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile = control.Profile

	var completion *kbuildRecursiveMakeFrontier
	plan, err := selectedKbuildRecursiveMakePlanWithControlAndCompletion(
		profile, map[string]bool{}, &control, &completion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || !strings.Contains(plan[0].request.makefile, "peer-effect.mk") ||
		!strings.Contains(plan[1].request.makefile, "z-trigger.mk") {
		t.Fatalf("grouped recursive plan = %#v, want peer prerequisite then one z-trigger invocation", plan)
	}
	if got, want := plan[1].request.entryTargets, []string{"z-child", "a-child"}; !slices.Equal(got, want) {
		t.Fatalf("grouped recursive child goals = %q, want source argv order %q", got, want)
	}
	if strings.Contains(plan[1].request.makefile, "a-peer.mk") {
		t.Fatalf("grouped recursive plan replayed non-trigger recipe: %#v", plan)
	}
	visible := kbuildRecursiveMakeFrontierArtifactEvents(completion)
	for _, output := range []string{"a-peer", "z-trigger"} {
		if !slices.ContainsFunc(visible, func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
			return artifact.Path == output && artifact.Target == output
		}) {
			t.Errorf("completion frontier %q omits grouped output %q", visible, output)
		}
	}
}

func TestSelectedKbuildSelectionsWalkGrowsWithSelectedClosure(t *testing.T) {
	const targetCount = 2048
	profile := kconfig.CompactKbuildProfile{
		Name:         "root-selection-scale-fixture",
		EntryTargets: []string{"all"},
		Rules:        make([]kconfig.KbuildRule, 0, targetCount+2),
	}
	targets := make([]string, 0, targetCount)
	for targetIndex := 0; targetIndex < targetCount; targetIndex++ {
		target := fmt.Sprintf("generated/target-%04d", targetIndex)
		targets = append(targets, target)
	}
	profile.Rules = append(profile.Rules, kconfig.KbuildRule{
		Targets:       []string{"all"},
		Prerequisites: targets,
	})
	for _, target := range targets {
		profile.Rules = append(profile.Rules, kconfig.KbuildRule{
			Targets:       []string{target},
			Prerequisites: []string{"generated/shared"},
			Recipe:        []string{"touch $@"},
		})
	}
	profile.Rules = append(profile.Rules, kconfig.KbuildRule{
		Targets: []string{"generated/shared"},
		Recipe:  []string{"touch $@"},
	})
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	stats := &kbuildSelectionWalkStats{}
	selections, err := selectedKbuildSelectionsWithStats(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{},
		stats,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(selections), targetCount+1; got != want {
		t.Fatalf("selected materialized closure size = %d, want %d", got, want)
	}
	// Every concrete target is scheduled and evaluated once. In particular,
	// the shared prerequisite is not appended targetCount times while the leaf
	// queue is drained.
	if got, want := stats.Scheduled, targetCount+2; got != want {
		t.Fatalf("scheduled targets = %d, want %d", got, want)
	}
	if got, want := stats.EvaluatedTargets, targetCount+2; got != want {
		t.Fatalf("evaluated targets = %d, want %d", got, want)
	}
	// The rule index examines the one direct rule for each selected target,
	// rather than every profile rule for every target.
	if got, want := stats.RuleCandidateChecks, targetCount+2; got != want {
		t.Fatalf("rule candidate checks = %d, want linear %d", got, want)
	}
	if stats.RecipeEvaluations != 0 {
		t.Fatalf("ordinary action recipes were evaluated %d times", stats.RecipeEvaluations)
	}
	if got, want := stats.MaxPending, targetCount; got != want {
		t.Fatalf("maximum pending targets = %d, want bounded %d", got, want)
	}
}

func TestSelectedKbuildSelectionsOpaqueRootWalksOnlyPredecessorTargets(t *testing.T) {
	const unrelatedCount = 1024
	var makefile strings.Builder
	makefile.WriteString("all: consumer.o")
	for index := 0; index < unrelatedCount; index++ {
		fmt.Fprintf(&makefile, " unrelated/generated-%04d.h", index)
	}
	makefile.WriteString(`
consumer.o: generated/source.c generated/header.h
	$(CC) -I__LINUX_BZL_OBJECT_TREE__ -c -o $@ $<
generated/source.c: source.in
	sed 's/^//' $< > $@
generated/header.h: header.in
	cp $< $@
`)
	for index := 0; index < unrelatedCount; index++ {
		fmt.Fprintf(
			&makefile,
			"unrelated/generated-%04d.h: unrelated.in\n\tcp $< $@\n",
			index,
		)
	}
	profile := selectionRoleProfileWithSources(t, makefile.String(), map[string]string{
		"source.in":    "int consumer;\n",
		"header.in":    "#define HEADER 1\n",
		"unrelated.in": "#define UNRELATED 1\n",
	}, "all")
	profile.Name = "root:opaque-predecessor-scale"
	stats := &kbuildSelectionWalkStats{}
	selections, err := selectedKbuildSelectionsWithStatsAndSourceRoot(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"source.in": true, "header.in": true, "unrelated.in": true},
		stats,
		filepath.Dir(profile.Path),
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer := selectionByTarget(t, selections, "consumer.o")
	want := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{
		{Path: "generated/header.h", Profile: profile.Name, Target: "generated/header.h"},
		{Path: "generated/source.c", Profile: profile.Name, Target: "generated/source.c"},
	})
	if consumer.GeneratedObjectTreeArtifacts != want || strings.Contains(consumer.GeneratedObjectTreeArtifacts, "unrelated/") {
		t.Fatalf("opaque-root artifacts = %q, want only native predecessor artifacts %q", consumer.GeneratedObjectTreeArtifacts, want)
	}
	if got, want := stats.OpaqueRootPredecessorChecks, 2; got != want {
		t.Fatalf(
			"opaque-root predecessor checks = %d, want %d independent of %d unrelated selected targets",
			got, want, unrelatedCount,
		)
	}
}

func TestSelectedKbuildSelectionsCachesForwardedVisibleArtifactOwner(t *testing.T) {
	const consumerCount = 256
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:cached-visible-owner"
	forwarder := selectionRoleProfile(t, "generated/shared.h:\n", "generated/shared.h")
	forwarder.Name = "forwarder:cached-visible-owner"
	producer := selectionRoleProfileWithSources(t, `
generated/shared.h: header.in
	cp $< $@
`, map[string]string{"header.in": "#define SHARED 1\n"}, "generated/shared.h")
	producer.Name = "producer:cached-visible-owner"
	forwarder.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{{
		Target: "generated/shared.h", Profile: producer.Name, Goals: producer.EntryTargets,
	}}

	var consumerMakefile strings.Builder
	consumerMakefile.WriteString("all:")
	for index := 0; index < consumerCount; index++ {
		fmt.Fprintf(&consumerMakefile, " consumer-%04d.o", index)
	}
	consumerMakefile.WriteByte('\n')
	for index := 0; index < consumerCount; index++ {
		fmt.Fprintf(
			&consumerMakefile,
			"consumer-%04d.o: FORCE\n\t$(CC) -include __LINUX_BZL_OBJECT_TREE__/generated/shared.h -c -x c /dev/null -o $@\n",
			index,
		)
	}
	consumer := selectionRoleProfile(t, consumerMakefile.String(), "all")
	consumer.Name = "consumer:cached-visible-owner"
	artifact := kconfig.CompactKbuildVisibleArtifact{
		Path: "generated/shared.h", Profile: forwarder.Name, Target: "generated/shared.h",
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []kconfig.CompactKbuildVisibleArtifact{artifact})
	consumer.InvocationPredecessors = []string{forwarder.Name}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: forwarder.Name, Goals: forwarder.EntryTargets},
		{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
	}

	stats := &kbuildSelectionWalkStats{}
	selections, err := selectedKbuildSelectionsWithStats(
		[]kconfig.CompactKbuildProfile{root, forwarder, producer, consumer},
		map[string]bool{"header.in": true},
		stats,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantArtifact := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{artifact})
	for _, target := range []string{"consumer-0000.o", fmt.Sprintf("consumer-%04d.o", consumerCount-1)} {
		selection := selectionByTarget(t, selections, target)
		if selection.InitialObjectTreeArtifacts != wantArtifact {
			t.Fatalf("%s initial visible artifacts = %q, want forwarded owner %q", target, selection.InitialObjectTreeArtifacts, wantArtifact)
		}
	}
	if got, want := stats.VisibleOwnerCandidateChecks, 1; got != want {
		t.Fatalf("visible owner candidate checks = %d, want %d cached across %d consumers", got, want, consumerCount)
	}
	if got, want := stats.InvocationDescentWalks, 1; got != want {
		t.Fatalf("invocation descent walks = %d, want %d cached across %d consumers", got, want, consumerCount)
	}
}

func TestSelectedKbuildSelectionsDoNotTurnGeneratedDeclarationsIntoProducers(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "generated-declaration-fixture",
		EntryTargets: []string{"generated/table.c"},
		Generated: []kconfig.KbuildTarget{{
			Kind: "targets", Target: "generated/table.c",
		}},
		Rules: []kconfig.KbuildRule{{
			Targets: []string{"%.c"}, Prerequisites: []string{"%_shipped"}, Recipe: []string{"cp $< $@"},
		}},
	}
	if selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{}); len(selections) != 0 {
		t.Fatalf("generated declaration became an action producer: %#v", selections)
	}
}

func TestSelectedKbuildSelectionsAssignExplicitPreparationRootClosure(t *testing.T) {
	root := kconfig.CompactKbuildProfile{
		Name:         "root-module-sdk-closure-fixture",
		EntryTargets: []string{"all", "modules_prepare"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"all"}, Prerequisites: []string{"generated/shared", "generated/target-only"}},
			{Targets: []string{"modules_prepare"}, Prerequisites: []string{"prepare", "generated/shared", "generated/sdk-only"}},
			{Targets: []string{"prepare"}},
			{Targets: []string{"generated/shared"}, Recipe: []string{"touch $@"}},
			{Targets: []string{"generated/target-only"}, Recipe: []string{"touch $@"}},
			{Targets: []string{"generated/sdk-only"}, Recipe: []string{"touch $@"}},
		},
	}
	child := kconfig.CompactKbuildProfile{
		Name:         "build:scripts-module-sdk-closure-fixture",
		EntryTargets: []string{"scripts/module.lds"},
		Rules: []kconfig.KbuildRule{{
			Targets: []string{"scripts/module.lds"}, Recipe: []string{"touch $@"},
		}},
	}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{{
		Target: "modules_prepare", Profile: child.Name, Goals: child.EntryTargets,
	}}
	root = syntheticSelectionProfileWithEvaluator(t, root)
	child = syntheticSelectionProfileWithEvaluator(t, child)
	profiles := []kconfig.CompactKbuildProfile{root, child}

	selections, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		profiles, map[string]bool{}, nil, "", []string{"modules_prepare"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]kconfig.CompactKbuildSelection{}
	for _, selection := range selections {
		byTarget[selection.Target] = selection
	}
	for _, target := range []string{"generated/shared", "generated/sdk-only", "scripts/module.lds"} {
		selection, ok := byTarget[target]
		if !ok || selection.Lifecycle != "prep" || selection.Scope != "target" || selection.Stage != "prep" {
			t.Errorf("preparation-root selection %q = %#v, want prep/target/prep", target, selection)
		}
	}
	if selection := byTarget["generated/target-only"]; selection.Lifecycle != "target" || selection.Scope != "target" || selection.Stage != "target" {
		t.Errorf("target-only selection = %#v, want target/target/target", selection)
	}
}

func TestCanonicalKbuildPreparationTargets(t *testing.T) {
	got, err := canonicalKbuildPreparationTargets([]string{"./modules_prepare", "modules_prepare", "."})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"modules_prepare", "."}; !slices.Equal(got, want) {
		t.Fatalf("canonical preparation targets = %q, want %q", got, want)
	}

	for _, target := range []string{
		"", "FORCE", "/modules_prepare", "../modules_prepare", "drivers/../../modules_prepare",
		`drivers\\modules_prepare`, "modules_%", "$(PREPARE)", "modules_prepare\nall",
	} {
		t.Run(fmt.Sprintf("invalid_%q", target), func(t *testing.T) {
			if _, err := canonicalKbuildPreparationTargets([]string{target}); err == nil {
				t.Fatalf("canonicalKbuildPreparationTargets(%q) succeeded", target)
			}
		})
	}
}

func TestSelectedKbuildSelectionsRejectUnknownPreparationRoot(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name: "unknown-module-sdk-root-fixture", EntryTargets: []string{"missing_prepare"},
	}
	_, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile}, map[string]bool{}, nil, "", []string{"missing_prepare"}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "has no source rule") {
		t.Fatalf("unknown preparation root error = %v", err)
	}
}

func TestSelectedKbuildSelectionsRequirePreparationRootEntryTarget(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "omitted-module-sdk-root-fixture",
		EntryTargets: []string{"all"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"all"}},
			{Targets: []string{"modules_prepare"}},
		},
	}
	_, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile}, map[string]bool{}, nil, "", []string{"modules_prepare"}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "is absent from root profile") {
		t.Fatalf("omitted preparation root error = %v", err)
	}
}

func TestSelectedKbuildSelectionsAcceptPhonyPreparationRoot(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "phony-module-sdk-root-fixture",
		EntryTargets: []string{"no_op"},
		Rules: []kconfig.KbuildRule{{
			Targets: []string{".PHONY"}, Prerequisites: []string{"no_op"},
		}},
	}
	if _, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile}, map[string]bool{}, nil, "", []string{"no_op"}, nil,
	); err != nil {
		t.Fatalf("phony preparation root rejected: %v", err)
	}
}

func TestSelectedKbuildSelectionsAcceptSatisfiedPreparationRoot(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name: "source-module-sdk-root-fixture", EntryTargets: []string{"prepared/source"},
	}
	if _, err := selectedKbuildSelectionsWithStatsSourceRootPreparationTargetsAndGeneratedContent(
		[]kconfig.CompactKbuildProfile{profile}, map[string]bool{"prepared/source": true}, nil, "", []string{"prepared/source"}, nil,
	); err != nil {
		t.Fatalf("satisfied preparation root rejected: %v", err)
	}
}

func TestSelectedKbuildSelectionsIgnoreUnrelatedPatternFamilies(t *testing.T) {
	const patternCount = 16384
	profile := kconfig.CompactKbuildProfile{
		Name:         "pattern-prefix-scale-fixture",
		EntryTargets: []string{"wanted/result.o"},
		Rules:        make([]kconfig.KbuildRule, 0, patternCount+1),
	}
	for index := 0; index < patternCount; index++ {
		profile.Rules = append(profile.Rules, kconfig.KbuildRule{
			Targets:       []string{fmt.Sprintf("unrelated/%05d/%%.o", index)},
			Prerequisites: []string{fmt.Sprintf("unrelated/%05d/%%.c", index)},
			Recipe:        []string{"compile $< -o $@"},
		})
	}
	profile.Rules = append(profile.Rules, kconfig.KbuildRule{
		Targets:       []string{"wanted/%.o"},
		Prerequisites: []string{"wanted/%.c"},
		Recipe:        []string{"compile $< -o $@"},
	})
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	stats := &kbuildSelectionWalkStats{}
	selections, err := selectedKbuildSelectionsWithStats(
		[]kconfig.CompactKbuildProfile{profile},
		map[string]bool{"wanted/result.c": true},
		stats,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(selections), 1; got != want || selections[0].Target != "wanted/result.o" {
		t.Fatalf("selected pattern closure = %#v, want wanted/result.o", selections)
	}
	// One lookup selects the producer and one verifies that the satisfied source
	// prerequisite is not phony/always-run. Both remain prefix-index bounded.
	if got, want := stats.RuleCandidateChecks, 2; got != want {
		t.Fatalf("pattern candidate checks = %d, want %d independent of %d unrelated patterns", got, want, patternCount)
	}
}

func TestKbuildProfileRootDirectoryGoalRetainsPrepareClosureWithoutImplicitLifecycle(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "root-prepare-closure-fixture",
		EntryTargets: []string{"built-in.a"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"built-in.a"}, Prerequisites: []string{"."}},
			{Targets: []string{"."}, Prerequisites: []string{"prepare"}},
			{Targets: []string{"prepare"}, Recipe: []string{"touch $@"}},
			{Targets: []string{".PHONY"}, Prerequisites: []string{".", "prepare"}},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	if got := kbuildProfileTarget(profile, "."); got != "." {
		t.Fatalf("root build-directory goal = %q, want invocation-local .", got)
	}
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{})
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Target == "prepare" && selection.Lifecycle == "target" && selection.Stage == "target"
	}) {
		t.Fatalf("root directory closure selections = %#v, want target-stage prepare without an explicit preparation root", selections)
	}
}

func TestSelectedKbuildSelectionsCrossExplicitImplicitSearchBoundary(t *testing.T) {
	const directory = "arch/x86/entry/vdso"
	profile := kconfig.CompactKbuildProfile{
		Name:         "vdso-ought-to-exist-fixture",
		EntryTargets: []string{directory + "/built-in.a"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{directory + "/built-in.a"}, Prerequisites: []string{directory + "/vdso-image-64.o"}, Recipe: []string{"touch $@"}},
			{Targets: []string{directory + "/%.o"}, Prerequisites: []string{directory + "/%.c"}, Recipe: []string{"touch $@"}},
			{Targets: []string{directory + "/vdso-image-%.c"}, Prerequisites: []string{directory + "/vdso%.so.dbg"}, Recipe: []string{"touch $@"}},
			{Targets: []string{directory + "/vdso64.so.dbg"}, Prerequisites: []string{directory + "/vclock_gettime.o"}, Recipe: []string{"touch $@"}},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{
		directory + "/vclock_gettime.c": true,
	})
	selected := map[string]bool{}
	for _, selection := range selections {
		selected[selection.Target] = true
	}
	for _, target := range []string{
		directory + "/built-in.a",
		directory + "/vdso-image-64.o",
		directory + "/vdso-image-64.c",
		directory + "/vdso64.so.dbg",
		directory + "/vclock_gettime.o",
	} {
		if !selected[target] {
			t.Fatalf("selected implicit closure omits %q: %#v", target, selections)
		}
	}
}

func TestSelectedKbuildSelectionsPreserveParentTraversalTargetSpelling(t *testing.T) {
	const directory = "arch/x86/kvm"
	const archive = directory + "/built-in.a"
	const makeObject = directory + "/../../../virt/kvm/kvm_main.o"
	const object = "virt/kvm/kvm_main.o"
	const source = "virt/kvm/kvm_main.c"
	const header = "include/linux/kvm_host.h"
	profile := kconfig.CompactKbuildProfile{
		Name:         "kvm-parent-traversal-fixture",
		Directory:    directory,
		EntryTargets: []string{archive},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{archive}, Prerequisites: []string{makeObject}, Recipe: []string{"touch $@"}},
			{Targets: []string{makeObject}, Prerequisites: []string{header}},
			{Targets: []string{directory + "/%.o"}, Prerequisites: []string{directory + "/%.c"}, Recipe: []string{"touch $@"}},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	satisfied := map[string]bool{
		source: true,
		header: true,
	}
	index := newKbuildProfileTargetIndex(profile, nil)
	currentness := newKbuildProfileTargetSatisfaction(profile, index, satisfied)
	control, err := kconfig.EvaluateSelectedKbuildControlEffectsWithOptions(
		profile,
		kconfig.KbuildControlEvaluationOptions{
			SelectedRuleIndexes: func(target, makeTarget string) []int {
				return kbuildProfileRuleIndexesForMakeTargetIndexed(
					profile, index, target, makeTarget, satisfied,
				)
			},
			TargetIsSatisfied: func(target string) bool {
				return currentness.targetIsSatisfied(target)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	const compileRuleIndex = 2
	if _, ok := kconfig.KbuildControlEvaluationBeforeRecipeIndex(control, object, compileRuleIndex, 0); !ok {
		t.Fatalf("control traversal omitted lexical implicit recipe snapshot for canonical target %q", object)
	}
	if _, err := selectedKbuildRecursiveMakePlanWithControl(control.Profile, satisfied, &control); err != nil {
		t.Fatalf("parent-traversal recursive-Make planning with control snapshots: %v", err)
	}
	var completion *kbuildRecursiveMakeFrontier
	if _, err := selectedKbuildRecursiveMakePlanWithControlAndCompletion(
		profile, satisfied, nil, &completion,
	); err != nil {
		t.Fatalf("parent-traversal recursive-Make ordering: %v", err)
	}
	visible := kbuildRecursiveMakeFrontierArtifactEvents(completion)
	for _, target := range []string{archive, object} {
		if !slices.ContainsFunc(visible, func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
			return artifact.Path == target
		}) {
			t.Fatalf("parent-traversal visible artifacts omit %q: %#v", target, visible)
		}
	}
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, satisfied)

	selected := map[string]int{}
	var objectSelection kconfig.CompactKbuildSelection
	for _, selection := range selections {
		selected[selection.Target]++
		if selection.Target == object {
			objectSelection = selection
		}
		if strings.Contains(selection.Target, "..") {
			t.Fatalf("selection leaked lexical Make target %q: %#v", selection.Target, selections)
		}
	}
	for _, target := range []string{archive, object} {
		if selected[target] != 1 {
			t.Fatalf("selected parent-traversal closure has %d instances of %q: %#v", selected[target], target, selections)
		}
	}
	if got, want := objectSelection.MakeTarget, makeObject; got != want {
		t.Fatalf("selected object lexical Make target = %q, want %q: %#v", got, want, selections)
	}
}

func TestSelectedKbuildSelectionsRetainGeneratedImplicitTargetWithSideEffectInput(t *testing.T) {
	predecessor := kconfig.CompactKbuildProfile{
		Name:         "modpost-fixture",
		EntryTargets: []string{"vmlinux.o"},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"vmlinux.o"}, Recipe: []string{"touch $@"}},
		},
	}
	predecessor = syntheticSelectionProfileWithEvaluator(t, predecessor)
	profile := kconfig.CompactKbuildProfile{
		Name:                   "generated-side-effect-fixture",
		EntryTargets:           []string{"vmlinux.unstripped"},
		InvocationPredecessors: []string{predecessor.Name},
		Generated: []kconfig.KbuildTarget{{
			Kind: "targets", Target: ".vmlinux.export.o",
		}},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"vmlinux.unstripped"}, Prerequisites: []string{".vmlinux.export.o"}, Recipe: []string{"touch $@"}},
			{Targets: []string{"%.o"}, Prerequisites: []string{"%.c", "FORCE"}, Recipe: []string{"touch $@"}},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)
	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile, predecessor}, map[string]bool{})
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Profile == profile.Name && selection.Target == ".vmlinux.export.o"
	}) {
		t.Fatalf("generated implicit target with side-effect input was not selected: %#v", selections)
	}
}

func TestGeneratedImplicitFallbackUsesGNUShortestStemForControlAndPlanning(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:                   "generated-side-effect-rule-order-fixture",
		EntryTargets:           []string{"vmlinux.unstripped"},
		InvocationPredecessors: []string{"modpost-fixture"},
		Generated: []kconfig.KbuildTarget{{
			Kind: "targets", Target: ".vmlinux.export.o",
		}},
		Rules: []kconfig.KbuildRule{
			{Targets: []string{"vmlinux.unstripped"}, Prerequisites: []string{".vmlinux.export.o"}},
			{
				Targets: []string{"%"}, Prerequisites: []string{"%_shipped"},
				Recipe: []string{"__LINUX_BZL_MAKE__ -f __LINUX_BZL_SOURCE_TREE__/scripts/wrong.mk all"},
			},
			{
				Targets: []string{"%.o"}, Prerequisites: []string{"%.c", "FORCE"},
				Recipe: []string{"__LINUX_BZL_MAKE__ -f __LINUX_BZL_SOURCE_TREE__/scripts/right.mk all"},
			},
		},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)
	index := newKbuildProfileTargetIndex(profile, nil)
	selected := kbuildProfileRuleIndexesForTargetIndexed(profile, index, ".vmlinux.export.o", map[string]bool{})
	if got, want := selected, []int{2}; !slices.Equal(got, want) {
		t.Fatalf("generated implicit fallback indexes = %v, want GNU shortest-stem rule %v", got, want)
	}

	control, err := kconfig.EvaluateSelectedKbuildControlEffectsWithOptions(
		profile,
		kconfig.KbuildControlEvaluationOptions{
			SelectedRuleIndexes: func(target, makeTarget string) []int {
				return kbuildProfileRuleIndexesForMakeTargetIndexed(
					profile, index, target, makeTarget, map[string]bool{},
				)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectedKbuildRecursiveMakePlanWithControl(control.Profile, map[string]bool{}, &control)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 1; got != want || plan[0].request.makefile != "scripts/right.mk" {
		t.Fatalf("generated implicit recursive plan = %#v, want only shortest-stem compile rule", plan)
	}
}

func TestEvaluatedKbuildProfilesFollowRootDotDirectoryIntoBuiltinArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootMakefile := `
srctree := ` + filepath.ToSlash(root) + `
build := -f $(srctree)/scripts/Makefile.build obj
all: vmlinux.a
vmlinux.a: ./built-in.a
	touch $@
built-in.a: . ;
.: prepare
	$(MAKE) $(build)=. need-builtin=1
prepare:
	touch $@
`
	childMakefile := `
./: built-in.a
	@:
built-in.a:
	touch $@
`
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(rootMakefile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "Makefile.build"), []byte(childMakefile), 0o644); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var child *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/Makefile.build" {
			child = &profiles[index]
		}
	}
	if child == nil {
		t.Fatalf("profiles omit root build-directory child: %#v", profiles)
	}
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Profile == child.Name && selection.Target == "built-in.a"
	}) {
		t.Fatalf("selections omit child built-in.a from %q: %#v", child.Name, selections)
	}
}

func TestEvaluatedKbuildProfilesUseSubdirectoryBuildDefaultGoal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootMakefile := `
srctree := ` + filepath.ToSlash(root) + `
build := -f $(srctree)/scripts/Makefile.build obj
all: drivers/demo
drivers/demo:
	$(MAKE) $(build)=$@ need-builtin=1
`
	childMakefile := `
$(obj)/: $(obj)/built-in.a
	@:
$(obj)/built-in.a:
	touch $@
`
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(rootMakefile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "Makefile.build"), []byte(childMakefile), 0o644); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var child *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/Makefile.build" {
			child = &profiles[index]
		}
	}
	if child == nil {
		t.Fatalf("profiles omit subdirectory build child: %#v", profiles)
	}
	if got, want := child.EntryTargets, []string{"drivers/demo/"}; !slices.Equal(got, want) {
		t.Fatalf("child entry targets = %q, want evaluated default goal %q", got, want)
	}
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Profile == child.Name && selection.Target == "drivers/demo/built-in.a"
	}) {
		t.Fatalf("selections omit child built-in.a from %q: %#v", child.Name, selections)
	}
}

func TestEvaluatedKbuildProfilesKeepBuildDriverPhonyGoalLiteral(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
srctree := `+filepath.ToSlash(root)+`
build := -f $(srctree)/scripts/Makefile.build obj
all: prepare
prepare: archprepare
archprepare: archheaders
archheaders:
	$(MAKE) $(build)=arch/x86/entry/syscalls all
`)
	write("scripts/Makefile.build", `
include $(srctree)/$(obj)/Makefile
`)
	write("arch/x86/entry/syscalls/Makefile", `
.PHONY: all
all: arch/x86/include/generated/uapi/asm/unistd_64.h \
     arch/x86/include/generated/asm/unistd_64_x32.h \
     arch/x86/include/generated/asm/unistd_32_ia32.h
	@:
arch/x86/include/generated/uapi/asm/unistd_64.h: arch/x86/entry/syscalls/syscall_64.tbl
	touch $@
arch/x86/include/generated/asm/unistd_64_x32.h: arch/x86/entry/syscalls/syscall_x32.tbl
	touch $@
arch/x86/include/generated/asm/unistd_32_ia32.h: arch/x86/entry/syscalls/syscall_32.tbl
	touch $@
`)
	for _, table := range []string{"syscall_64.tbl", "syscall_x32.tbl", "syscall_32.tbl"} {
		write("arch/x86/entry/syscalls/"+table, "0 common read sys_read\n")
	}

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var syscalls *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Directory == "arch/x86/entry/syscalls" {
			syscalls = &profiles[index]
			break
		}
	}
	if syscalls == nil {
		t.Fatalf("profiles omit selected syscall-header invocation: %#v", profiles)
	}
	if got, want := syscalls.EntryTargets, []string{"all"}; !slices.Equal(got, want) {
		t.Fatalf("syscall-header entry targets = %q, want literal child goal %q", got, want)
	}
	for _, target := range []string{
		"arch/x86/include/generated/uapi/asm/unistd_64.h",
		"arch/x86/include/generated/asm/unistd_64_x32.h",
		"arch/x86/include/generated/asm/unistd_32_ia32.h",
	} {
		if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
			return selection.Profile == syscalls.Name && selection.Target == target
		}) {
			t.Errorf("syscall-header selections omit %q from %q: %#v", target, syscalls.Name, selections)
		}
	}
}

func mixedRecipeEffectProfile(t *testing.T, command string, commandTemplate bool) kconfig.CompactKbuildProfile {
	t.Helper()
	recipe := command
	definitions := ""
	if commandTemplate {
		// The wrapper deliberately looks like a local write. Command-call
		// lowering executes cmd_mixed; recursive discovery must use that same
		// authoritative text instead of treating the wrapper as another action.
		definitions = "cmd = touch $@; $(cmd_$(1))\ncmd_mixed = " + command + "\n"
		recipe = "$(call cmd,mixed)"
	}
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	content := definitions + "all: generated.stamp\ngenerated.stamp:\n\t" + recipe + "\n"
	if err := os.WriteFile(makefile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		Variables:               map[string]string{"SRCARCH": "x86"},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("mixed-recipe-effects", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"generated.stamp"}
	setTestKbuildInvocationLocation(t, &profile)
	return profile
}

func selectedCommandStructureProfile(
	t *testing.T,
	source, structure, replay string,
) kconfig.CompactKbuildProfile {
	t.Helper()
	const marker = "__SELECTED_COMMAND_STRUCTURE__"
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	content := strings.Replace(`
cmd = $(cmd_$(1))
cmd_record_mcount = __SOURCE_COMMAND__
result.o:
	$(call cmd,record_mcount)
`, "__SOURCE_COMMAND__", source, 1)
	if err := os.WriteFile(makefile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func(replacement string) func(string) (string, error) {
		return func(value string) (string, error) {
			return strings.ReplaceAll(value, marker, replacement), nil
		}
	}
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		ConfigVariablesComplete:  true,
		MakeVariablesComplete:    true,
		CaptureTargetEvaluator:   true,
		ResolveSymbolic:          resolve(replay),
		ResolveSymbolicStructure: resolve(structure),
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("selected-command-structure", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"result.o"}
	setTestKbuildInvocationLocation(t, &profile)
	return profile
}

func TestKbuildSelectedRecipeEffectsUseProbeSelectedCommandStructure(t *testing.T) {
	for _, test := range []struct {
		name                      string
		source, structure, replay string
		wantEffects               int
		wantRecursiveReplayFlag   string
		wantRecursiveSymbolicFlag string
		wantRecursiveGoal         string
		wantLocalCommands         []string
		wantError                 string
	}{
		{name: "selected command disappears", source: "__SELECTED_COMMAND_STRUCTURE__"},
		{
			name:        "selected command expands into multiple segments",
			source:      "__SELECTED_COMMAND_STRUCTURE__",
			structure:   "cc -c input.c -o result.o; touch result.o",
			replay:      "cc -c input.c -o result.o; touch result.o",
			wantEffects: 2,
			wantLocalCommands: []string{
				"'cc' '-c' 'input.c' '-o' 'result.o'",
				"'touch' 'result.o'",
			},
		},
		{
			name:      "recursive command binds structural argv to replay",
			source:    "__LINUX_BZL_MAKE__ FLAGS=__SELECTED_COMMAND_STRUCTURE__ child",
			structure: "-O2", replay: "-O2",
			wantEffects: 1, wantRecursiveReplayFlag: "FLAGS=-O2", wantRecursiveSymbolicFlag: "-O2",
		},
		{
			name:      "connector topology must match",
			source:    "__SELECTED_COMMAND_STRUCTURE__",
			structure: "touch result.o; :", replay: "touch result.o && :",
			wantError: "different shell connector sequences",
		},
		{
			name:        "selected branch reveals recursive Make while retaining nested atom",
			source:      "__SELECTED_COMMAND_STRUCTURE__",
			structure:   "__LINUX_BZL_MAKE__ FLAGS=__NESTED_PROBE__ child",
			replay:      "__LINUX_BZL_MAKE__ FLAGS=-O2 child",
			wantEffects: 1, wantRecursiveReplayFlag: "FLAGS=-O2", wantRecursiveSymbolicFlag: "__NESTED_PROBE__",
		},
		{
			name:      "recursive Make cannot move between connector segments",
			source:    "__SELECTED_COMMAND_STRUCTURE__",
			structure: "__LINUX_BZL_MAKE__ child; touch result.o",
			replay:    "touch result.o; __LINUX_BZL_MAKE__ child",
			wantError: "shell segment 0: recursive Make symbolic and replay forms contain different invocation counts",
		},
		{
			name:              "newline separates local and recursive effects",
			source:            "__SELECTED_COMMAND_STRUCTURE__",
			structure:         "touch result.o\n__LINUX_BZL_MAKE__ child",
			replay:            "touch result.o\n__LINUX_BZL_MAKE__ child",
			wantEffects:       2,
			wantRecursiveGoal: "child",
			wantLocalCommands: []string{
				"'touch' 'result.o'",
			},
		},
		{
			name:              "reserved word retains recursive executable",
			source:            "__SELECTED_COMMAND_STRUCTURE__",
			structure:         "if test -f marker; then __LINUX_BZL_MAKE__ child; fi",
			replay:            "if test -f marker; then __LINUX_BZL_MAKE__ child; fi",
			wantEffects:       1,
			wantRecursiveGoal: "child",
		},
		{
			name:              "brace group retains recursive executable",
			source:            "__SELECTED_COMMAND_STRUCTURE__",
			structure:         "{ __LINUX_BZL_MAKE__ child; }",
			replay:            "{ __LINUX_BZL_MAKE__ child; }",
			wantEffects:       1,
			wantRecursiveGoal: "child",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := selectedCommandStructureProfile(t, test.source, test.structure, test.replay)
			var rule kconfig.KbuildRule
			for _, candidate := range profile.Rules {
				if slices.Contains(candidate.Targets, "result.o") {
					rule = candidate
					break
				}
			}
			if len(rule.Recipe) != 1 {
				t.Fatalf("result.o rule = %#v, want one recipe", rule)
			}
			effects, err := kbuildSelectedRecipeExecutionEffects(
				profile, rule, "result.o", "result.o", "result.o", "", nil, nil, rule.Recipe[0],
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("selected recipe error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(effects) != test.wantEffects {
				t.Fatalf("selected recipe effects = %#v, want %d", effects, test.wantEffects)
			}
			if len(test.wantLocalCommands) != 0 {
				for index, want := range test.wantLocalCommands {
					if !effects[index].materializesTarget || effects[index].command != want {
						t.Fatalf("local selected effect %d = %#v, want materializing command %q", index, effects[index], want)
					}
				}
			}
			if test.wantRecursiveGoal != "" {
				index := len(test.wantLocalCommands)
				if index >= len(effects) || !effects[index].recursive {
					t.Fatalf("selected effect %d = %#v, want recursive Make", index, effects)
				}
				if got := effects[index].invocation.request.variables["MAKECMDGOALS"]; got != test.wantRecursiveGoal {
					t.Fatalf("selected recursive Make goals = %q, want %q", got, test.wantRecursiveGoal)
				}
			}
			if test.wantRecursiveReplayFlag == "" {
				return
			}
			if !effects[0].recursive || !slices.Contains(effects[0].invocation.replayArguments, test.wantRecursiveReplayFlag) {
				t.Fatalf("recursive selected effect = %#v, want replay flag %q", effects[0], test.wantRecursiveReplayFlag)
			}
			if got := effects[0].invocation.request.variables["FLAGS"]; got != test.wantRecursiveSymbolicFlag {
				t.Fatalf("recursive structural FLAGS = %q, want %q", got, test.wantRecursiveSymbolicFlag)
			}
		})
	}
}

func TestKbuildEvaluatedShellCommandShapePreservesQuotedOperators(t *testing.T) {
	segments, connectors, err := kbuildEvaluatedShellCommandShape(
		`printf '%s' ';' '&&' '>' ; touch result.o`,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantSegments := []string{
		`'printf' '%s' ';' '&&' '>'`,
		`'touch' 'result.o'`,
	}
	if !slices.Equal(segments, wantSegments) || !slices.Equal(connectors, []string{";"}) {
		t.Fatalf("shell shape = segments %q connectors %q, want %q and one semicolon", segments, connectors, wantSegments)
	}
}

func TestKbuildRecursiveMakeRedirectionsRemainShellSyntax(t *testing.T) {
	for _, test := range []struct {
		name, command string
		wantReplay    []string
		wantGoals     string
	}{
		{
			name:       "descriptor duplication is not a connector or argument",
			command:    "__LINUX_BZL_MAKE__ child 2>&1; touch result.o",
			wantReplay: []string{"child"},
			wantGoals:  "child",
		},
		{
			name:       "redirections do not truncate later arguments",
			command:    "__LINUX_BZL_MAKE__ >log child 2>/dev/null",
			wantReplay: []string{"child"},
			wantGoals:  "child",
		},
		{
			name:       "adjacent redirections retain the first operand",
			command:    "__LINUX_BZL_MAKE__ child 2>&1>/dev/null",
			wantReplay: []string{"child"},
			wantGoals:  "child",
		},
		{
			name:       "quoted operator remains an argument",
			command:    "__LINUX_BZL_MAKE__ '>'",
			wantReplay: []string{">"},
			wantGoals:  ">",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocations, err := kbuildRecursiveMakeInvocationsAt(
				test.command,
				kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(invocations) != 1 {
				t.Fatalf("recursive Make invocations = %#v, want one", invocations)
			}
			if !slices.Equal(invocations[0].replayArguments, test.wantReplay) {
				t.Fatalf("recursive Make replay argv = %q, want %q", invocations[0].replayArguments, test.wantReplay)
			}
			if got := invocations[0].request.variables["MAKECMDGOALS"]; got != test.wantGoals {
				t.Fatalf("recursive Make goals = %q, want %q", got, test.wantGoals)
			}
		})
	}
}

func TestKbuildRecursiveMakeRequiresSimpleCommandExecutable(t *testing.T) {
	for _, command := range []string{
		"echo __LINUX_BZL_MAKE__ child",
		"echo -__LINUX_BZL_MAKE__ child",
	} {
		invocations, err := kbuildRecursiveMakeInvocations(command, "")
		if err != nil {
			t.Fatalf("recursive Make discovery for %q: %v", command, err)
		}
		if len(invocations) != 0 {
			t.Fatalf("recursive Make discovery for %q = %#v, want none", command, invocations)
		}
	}

	invocations, err := kbuildRecursiveMakeInvocations("KEEP=ok __LINUX_BZL_MAKE__ child EXTRA=value", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	if got, want := invocations[0].request.environment, map[string]string{"KEEP": "ok"}; !maps.Equal(got, want) {
		t.Fatalf("recursive Make environment = %#v, want %#v", got, want)
	}
	if got := invocations[0].request.variables["EXTRA"]; got != "value" {
		t.Fatalf("recursive Make command-line EXTRA = %q, want value", got)
	}
}

func TestKbuildRecursiveMakeShellAssignmentProvenance(t *testing.T) {
	location := kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree}
	for _, command := range []string{
		`"FOO=bar" __LINUX_BZL_MAKE__ child`,
		`FOO-BAR=x __LINUX_BZL_MAKE__ child`,
		`FOO.BAR=x __LINUX_BZL_MAKE__ child`,
	} {
		invocations, err := kbuildRecursiveMakeInvocationsAt(command, location)
		if err != nil {
			t.Fatalf("recursive Make discovery for %q: %v", command, err)
		}
		if len(invocations) != 0 {
			t.Fatalf("recursive Make discovery for %q = %#v, want none", command, invocations)
		}
	}

	invocations, err := kbuildRecursiveMakeInvocationsAt(
		`FOO="bar baz" __LINUX_BZL_MAKE__ child FOO-BAR=value`, location,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	if got, want := invocations[0].request.environment, map[string]string{"FOO": "bar baz"}; !maps.Equal(got, want) {
		t.Fatalf("recursive Make environment = %#v, want %#v", got, want)
	}
	if got := invocations[0].request.variables["FOO-BAR"]; got != "value" {
		t.Fatalf("recursive Make command-line FOO-BAR = %q, want value", got)
	}
}

func TestKbuildShellShapeRetainsAssignmentProvenance(t *testing.T) {
	segments, connectors, err := kbuildEvaluatedShellCommandShape(
		`FOO="bar baz" __LINUX_BZL_MAKE__ child`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || len(connectors) != 0 {
		t.Fatalf("shell shape = segments %q connectors %q, want one command", segments, connectors)
	}
	invocations, err := kbuildRecursiveMakeInvocations(segments[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	if got, want := invocations[0].request.environment, map[string]string{"FOO": "bar baz"}; !maps.Equal(got, want) {
		t.Fatalf("recursive Make environment = %#v, want %#v", got, want)
	}
}

func TestKbuildRecursiveMakeBehindShellReservedWord(t *testing.T) {
	for _, command := range []string{
		`if test -f marker; then __LINUX_BZL_MAKE__ child; fi`,
		`{ __LINUX_BZL_MAKE__ child; }`,
	} {
		invocations, err := kbuildRecursiveMakeInvocationsAt(
			command,
			kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree},
		)
		if err != nil {
			t.Fatalf("recursive Make discovery for %q: %v", command, err)
		}
		if len(invocations) != 1 {
			t.Fatalf("recursive Make invocations for %q = %#v, want one", command, invocations)
		}
		if got := invocations[0].request.variables["MAKECMDGOALS"]; got != "child" {
			t.Fatalf("recursive Make goals for %q = %q, want child", command, got)
		}
	}
}

func TestKbuildRecursiveMakeContinuesAfterMultilineComment(t *testing.T) {
	invocations, err := kbuildRecursiveMakeInvocations(
		"echo setup # __LINUX_BZL_MAKE__ is data\n__LINUX_BZL_MAKE__ child", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	if got := invocations[0].request.variables["MAKECMDGOALS"]; got != "child" {
		t.Fatalf("recursive Make goals = %q, want child", got)
	}
}

func TestKbuildShellLexemesPreservePOSIXBackslashesAndEmptyQuotedWords(t *testing.T) {
	for _, test := range []struct {
		name, command string
		want          []string
	}{
		{name: "ordinary double-quoted escape", command: `"a\qb"`, want: []string{`a\qb`}},
		{name: "line continuation", command: "foo\\\nbar", want: []string{"foobar"}},
		{name: "empty quote keeps comment marker in word", command: `''#value`, want: []string{"#value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lexemes, err := kbuildShellLexemes(test.command)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(lexemes))
			for index, lexeme := range lexemes {
				if lexeme.kind != kbuildShellWordLexeme {
					t.Fatalf("shell lexeme %d kind = %d, want word", index, lexeme.kind)
				}
				got[index] = lexeme.value
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("shell words = %q, want %q", got, test.want)
			}
		})
	}
}

func TestKbuildRecursiveMakeInlineEnvironmentDoesNotCrossCommandBoundary(t *testing.T) {
	invocations, err := kbuildRecursiveMakeInvocationsAt(
		"LEAK=bad echo setup; KEEP=ok __LINUX_BZL_MAKE__ child",
		kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("recursive Make invocations = %#v, want one", invocations)
	}
	want := map[string]string{"KEEP": "ok"}
	if !maps.Equal(invocations[0].request.environment, want) {
		t.Fatalf("recursive Make inline environment = %#v, want %#v", invocations[0].request.environment, want)
	}
}

func linearKbuildRecursiveMakeFrontierEvents(
	t *testing.T,
	frontier *kbuildRecursiveMakeFrontier,
) []kbuildRecursiveMakeFrontierEvent {
	t.Helper()
	if frontier == nil {
		return nil
	}
	if !frontier.hasEvent || len(frontier.parents) != 1 {
		t.Fatalf("frontier %q is a prerequisite join, want one causal sequence", frontier.id)
	}
	events := linearKbuildRecursiveMakeFrontierEvents(t, frontier.parents[0])
	return append(events, frontier.event)
}

func kbuildRecursiveMakeFrontierArtifactEvents(
	frontier *kbuildRecursiveMakeFrontier,
) []kconfig.CompactKbuildVisibleArtifact {
	visited := map[string]bool{}
	artifacts := []kconfig.CompactKbuildVisibleArtifact{}
	var visit func(*kbuildRecursiveMakeFrontier)
	visit = func(current *kbuildRecursiveMakeFrontier) {
		if current == nil || visited[current.id] {
			return
		}
		visited[current.id] = true
		for _, parent := range current.parents {
			visit(parent)
		}
		if current.hasEvent && current.event.artifact.Path != "" {
			artifacts = append(artifacts, current.event.artifact)
		}
	}
	visit(frontier)
	return canonicalKbuildVisibleArtifacts(artifacts)
}

func TestSelectedKbuildRecursiveMakePlanPreservesDirectMixedLineOrder(t *testing.T) {
	profile := mixedRecipeEffectProfile(t, strings.Join([]string{
		"$(MAKE) -f $(srctree)/scripts/first.mk",
		"touch $@",
		"$(MAKE) -f $(srctree)/scripts/second.mk",
	}, "; "), false)
	plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 2; got != want {
		t.Fatalf("mixed-line recursive plan = %#v, want %d invocations", plan, want)
	}
	if got := kbuildFrontierLen(plan[0].request.visibleState); got != 0 {
		t.Fatalf("unevaluated invocation frontier has %d entries, want 0", got)
	}
	wantArtifact := kconfig.CompactKbuildVisibleArtifact{
		Path: "generated.stamp", Profile: profile.Name, Target: "generated.stamp",
	}
	if got, want := plan[1].predecessors, []string{plan[0].key}; !slices.Equal(got, want) {
		t.Fatalf("second invocation predecessors = %q, want first invocation %q", got, want)
	}
	if got, want := linearKbuildRecursiveMakeFrontierEvents(t, plan[1].frontier), []kbuildRecursiveMakeFrontierEvent{
		{invocation: plan[0].key},
		{artifact: wantArtifact, command: `'touch' 'generated.stamp'`},
	}; !slices.Equal(got, want) {
		t.Fatalf("second invocation frontier = %#v, want exact source order %#v", got, want)
	}
}

func TestSelectedKbuildRecursiveMakePlanExposesEarlierSameLineWrite(t *testing.T) {
	for _, commandTemplate := range []bool{false, true} {
		name := "direct"
		if commandTemplate {
			name = "command template"
		}
		t.Run(name, func(t *testing.T) {
			profile := mixedRecipeEffectProfile(
				t,
				"touch $@; $(MAKE) -f $(srctree)/scripts/child.mk",
				commandTemplate,
			)
			plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(plan), 1; got != want {
				t.Fatalf("mixed-line recursive plan = %#v, want %d invocation", plan, want)
			}
			want := kconfig.CompactKbuildVisibleArtifact{
				Path: "generated.stamp", Profile: profile.Name, Target: "generated.stamp",
			}
			events := linearKbuildRecursiveMakeFrontierEvents(t, plan[0].frontier)
			if len(events) != 1 || events[0].artifact != want {
				t.Fatalf("recursive invocation frontier = %#v, want earlier same-line write %#v", events, want)
			}
		})
	}
}

func TestSelectedKbuildRecursiveMakePlanUsesCommandTemplateEffectOrder(t *testing.T) {
	profile := mixedRecipeEffectProfile(t, strings.Join([]string{
		"$(MAKE) -f $(srctree)/scripts/first.mk",
		"touch $@",
		"$(MAKE) -f $(srctree)/scripts/second.mk",
	}, "; "), true)
	plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 2; got != want {
		t.Fatalf("template recursive plan = %#v, want %d invocations", plan, want)
	}
	if got := kbuildFrontierLen(plan[0].request.visibleState); got != 0 {
		t.Fatalf("unevaluated template invocation frontier has %d entries, want 0", got)
	}
	events := linearKbuildRecursiveMakeFrontierEvents(t, plan[1].frontier)
	if len(events) != 2 || events[1].artifact.Path != "generated.stamp" || events[1].artifact.Profile != profile.Name {
		t.Fatalf("second template invocation frontier = %#v, want cmd_mixed local write", events)
	}
}

func TestSelectedKbuildRecursiveMakePlanUsesMergedExplicitPrerequisiteForImplicitRecipe(t *testing.T) {
	profile := selectionRoleProfile(t, `
result.out: generated.input
%.out:
	$(MAKE) -f $(srctree)/scripts/consumer.mk $<
generated.input:
	$(MAKE) -f $(srctree)/scripts/producer.mk
`, "result.out")

	plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 2; got != want {
		t.Fatalf("merged-rule recursive plan = %#v, want %d invocations", plan, want)
	}
	if got, want := plan[0].request.makefile, "scripts/producer.mk"; got != want {
		t.Fatalf("prerequisite invocation makefile = %q, want %q", got, want)
	}
	if got, want := plan[1].request.makefile, "scripts/consumer.mk"; got != want {
		t.Fatalf("implicit-recipe invocation makefile = %q, want %q", got, want)
	}
	if got, want := plan[1].request.entryTargets, []string{"generated.input"}; !slices.Equal(got, want) {
		t.Fatalf("implicit-recipe entry targets = %q, want merged explicit $< %q", got, want)
	}
	if got, want := plan[1].request.variables["MAKECMDGOALS"], "generated.input"; got != want {
		t.Fatalf("implicit-recipe MAKECMDGOALS = %q, want merged explicit $< %q", got, want)
	}
	if got, want := plan[1].predecessors, []string{plan[0].key}; !slices.Equal(got, want) {
		t.Fatalf("implicit-recipe predecessors = %q, want explicit prerequisite invocation %q", got, want)
	}
}

func TestSelectedKbuildCompletionFrontierIncludesMixedLineLocalWrite(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{name: "before recursive Make", command: "touch $@; $(MAKE) -f $(srctree)/scripts/child.mk"},
		{name: "after recursive Make", command: "$(MAKE) -f $(srctree)/scripts/child.mk; touch $@"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mixedRecipeEffectProfile(t, test.command, false)
			var completion *kbuildRecursiveMakeFrontier
			_, err := selectedKbuildRecursiveMakePlanWithControlAndCompletion(
				profile, map[string]bool{}, nil, &completion,
			)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := kbuildRecursiveMakeFrontierArtifactEvents(completion)
			want := []kconfig.CompactKbuildVisibleArtifact{{
				Path: "generated.stamp", Profile: profile.Name, Target: "generated.stamp",
			}}
			if !slices.Equal(artifacts, want) {
				t.Fatalf("completed visible artifacts = %#v, want mixed-line local write %#v", artifacts, want)
			}
		})
	}
}

func TestSelectedKbuildMixedLineNonWritingSegmentsAreNotMaterialization(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{name: "directory setup", command: "mkdir -p $@; $(MAKE) -f $(srctree)/scripts/child.mk"},
		{name: "status output", command: "echo setup; $(MAKE) -f $(srctree)/scripts/child.mk"},
		{name: "status pipeline", command: "echo setup | cat; $(MAKE) -f $(srctree)/scripts/child.mk"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mixedRecipeEffectProfile(t, test.command, false)
			plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(plan), 1; got != want {
				t.Fatalf("non-writing recursive plan = %#v, want %d invocation", plan, want)
			}
			if got := kbuildFrontierLen(plan[0].request.visibleState); got != 0 {
				t.Fatalf("unevaluated recursive invocation frontier has %d entries, want 0", got)
			}
			var completion *kbuildRecursiveMakeFrontier
			_, err = selectedKbuildRecursiveMakePlanWithControlAndCompletion(
				profile, map[string]bool{}, nil, &completion,
			)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := kbuildRecursiveMakeFrontierArtifactEvents(completion)
			if len(artifacts) != 0 {
				t.Fatalf("non-writing segment materialized target file: %#v", artifacts)
			}
		})
	}
}

func TestEvaluatedKbuildProfilesPreserveRecursiveInvocationPredecessors(t *testing.T) {
	for _, test := range []struct {
		name     string
		makefile string
	}{
		{
			name: "prerequisite before recipe",
			makefile: `
all: modules
modules: postprocess
	$(MAKE) -f $(srctree)/scripts/consumer.mk
postprocess:
	$(MAKE) -f $(srctree)/scripts/producer.mk
`,
		},
		{
			name: "successive recipe lines",
			makefile: `
all: single
single:
	$(MAKE) -f $(srctree)/scripts/producer.mk
	$(MAKE) -f $(srctree)/scripts/consumer.mk
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(relative, content string) {
				t.Helper()
				filename := filepath.Join(root, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("Makefile", test.makefile)
			write("scripts/producer.mk", `
__producer: generated.sym
generated.sym:
	touch $@
`)
			write("scripts/consumer.mk", `
__consumer: drivers/demo.ko
drivers/demo.ko: drivers/demo.mod.o
	touch $@
%.mod.o: %.mod.c
	touch $@
`)

			variables := map[string]string{"SRCARCH": "x86"}
			profiles, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
				RootDir:                 root,
				Variables:               variables,
				ConfigVariablesComplete: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var producer, consumer *kconfig.CompactKbuildProfile
			for index := range profiles {
				switch profiles[index].Path {
				case "scripts/producer.mk":
					producer = &profiles[index]
				case "scripts/consumer.mk":
					consumer = &profiles[index]
				}
			}
			if producer == nil || consumer == nil {
				t.Fatalf("profiles omit recursive producer or consumer: %#v", profiles)
			}
			if got, want := consumer.InvocationPredecessors, []string{producer.Name}; !slices.Equal(got, want) {
				t.Fatalf("consumer predecessors = %q, want source-ordered %q", got, want)
			}
			if len(producer.InvocationPredecessors) != 0 {
				t.Fatalf("producer unexpectedly depends on consumer: %q", producer.InvocationPredecessors)
			}
		})
	}
}

func TestEvaluatedKbuildProfilesBindPredecessorsBeforeGeneratedFallbackControl(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all: final
final: modpost
	$(MAKE) -f $(srctree)/scripts/consumer.mk vmlinux.unstripped
modpost:
	$(MAKE) -f $(srctree)/scripts/producer.mk vmlinux.symvers
`)
	write("scripts/producer.mk", `
vmlinux.symvers: FORCE
	touch $@
FORCE:
`)
	write("scripts/consumer.mk", `
targets += .vmlinux.export.o
vmlinux.unstripped: .vmlinux.export.o FORCE
	touch $@
%: %_shipped
	$(MAKE) -f $(srctree)/scripts/wrong.mk all
%.o: %.c FORCE
	$(MAKE) -f $(srctree)/scripts/right.mk all
FORCE:
`)
	write("scripts/right.mk", "all:\n\ttouch $@\n")
	write("scripts/wrong.mk", "all:\n\ttouch $@\n")

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	var consumer *kconfig.CompactKbuildProfile
	for index := range profiles {
		paths[profiles[index].Path] = true
		if profiles[index].Path == "scripts/consumer.mk" {
			consumer = &profiles[index]
		}
	}
	if consumer == nil || len(consumer.InvocationPredecessors) != 1 {
		t.Fatalf("consumer profile/predecessor provenance = %#v", consumer)
	}
	if !paths["scripts/right.mk"] || paths["scripts/wrong.mk"] {
		t.Fatalf("discovered recursive drivers = %#v, want only GNU shortest-stem compile child", paths)
	}
}

func TestEvaluatedKbuildProfilesExposeCompletedPredecessorFilesToWildcard(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("input.c", "int input;\n")
	write("Makefile", `
all: final
final: produced.o
	$(MAKE) -f $(srctree)/scripts/consumer.mk
produced.o: first
	@:
first:
	$(MAKE) -f $(srctree)/scripts/producer.mk
`)
	write("scripts/producer.mk", `
PHONY := __default
__default: z-produced.o ./a-produced.o produced.o ./produced.o
produced.o: input.c
	$(CC) -c -o $@ $<
z-produced.o: input.c
	$(CC) -c -o $@ $<
./a-produced.o: input.c
	$(CC) -c -o $@ $<
.PHONY: $(PHONY)
`)
	write("scripts/consumer.mk", `
ifneq ($(wildcard produced.o),)
selected := present.out
else
selected := missing.out
endif
PHONY := __default
__default: $(selected)
present.out: input.c
	$(CC) -c -o $@ $<
missing.out: input.c
	$(CC) -c -o $@ $<
.PHONY: $(PHONY)
`)
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:   root,
		Variables: variables,
		CommandLineVariables: map[string]string{
			"CC": kconfig.KbuildActionRoleToken("target", "cc"),
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var producer, consumer *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "scripts/producer.mk":
			producer = &profiles[index]
		case "scripts/consumer.mk":
			consumer = &profiles[index]
		}
	}
	if producer == nil || consumer == nil {
		t.Fatalf("profiles omit wildcard producer or consumer: %#v", profiles)
	}
	wantArtifacts := []kconfig.CompactKbuildVisibleArtifact{
		{Path: "a-produced.o", Profile: producer.Name, Target: "a-produced.o"},
		{Path: "produced.o", Profile: producer.Name, Target: "produced.o"},
		{Path: "z-produced.o", Profile: producer.Name, Target: "z-produced.o"},
	}
	if got := testCompactKbuildInitialVisibleArtifacts(*consumer); !slices.Equal(got, wantArtifacts) {
		t.Fatalf("wildcard consumer visible artifacts = %#v, want canonical invocation frontier %#v", got, wantArtifacts)
	}
	evaluation, err := kconfig.EvaluateSelectedKbuildControlEffects(*consumer)
	if err != nil {
		t.Fatalf("recompute wildcard consumer control profile: %v", err)
	}
	if got, want := testCompactKbuildInitialVisibleArtifacts(evaluation.Profile), testCompactKbuildInitialVisibleArtifacts(*consumer); !slices.Equal(got, want) {
		t.Fatalf("recomputed wildcard consumer visible artifacts = %#v, want persisted frontier %#v", got, want)
	}
	selected := map[string]bool{}
	for _, selection := range selections {
		if selection.Profile == consumer.Name {
			selected[selection.Target] = true
		}
	}
	if !selected["present.out"] || selected["missing.out"] {
		t.Fatalf("wildcard consumer selections = %#v, want completed predecessor branch", selected)
	}
}

func TestEvaluatedKbuildProfilesExposeControlledCmdMaterializationToLaterSibling(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all: consume
consume: produce
	$(MAKE) -f $(srctree)/scripts/consumer.mk
produce:
	$(MAKE) -f $(srctree)/scripts/producer.mk
`)
	write("scripts/producer.mk", `
if_changed = $(cmd_$(1))
cmd_emit =
all: generated.stamp
generated.stamp:
	$(eval cmd_emit = touch $@)
	$(call if_changed,emit)
`)
	write("scripts/consumer.mk", `
ifneq ($(wildcard generated.stamp),)
selected := present.out
else
selected := missing.out
endif
all: $(selected)
present.out:
	touch $@
missing.out:
	touch $@
	`)

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var producer, consumer *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "scripts/producer.mk":
			producer = &profiles[index]
		case "scripts/consumer.mk":
			consumer = &profiles[index]
		}
	}
	if producer == nil || consumer == nil {
		t.Fatalf("profiles omit controlled producer or later consumer: %#v", profiles)
	}
	wantArtifacts := []kconfig.CompactKbuildVisibleArtifact{{
		Path: "generated.stamp", Profile: producer.Name, Target: "generated.stamp",
	}}
	if got := testCompactKbuildInitialVisibleArtifacts(*consumer); !slices.Equal(got, wantArtifacts) {
		t.Fatalf("later sibling visible artifacts = %#v, want controlled producer frontier %#v", got, wantArtifacts)
	}
	selected := map[string]bool{}
	for _, selection := range selections {
		if selection.Profile == consumer.Name {
			selected[selection.Target] = true
		}
	}
	if !selected["present.out"] || selected["missing.out"] {
		t.Fatalf("later sibling selections = %#v, want branch selected from controlled completion frontier", selected)
	}
}

func TestEvaluatedKbuildProfilesPreserveLocalLastWriterAfterRecursiveProducer(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all: generated.stamp
generated.stamp:
	$(MAKE) -f $(srctree)/scripts/producer.mk
	touch $@
	$(MAKE) -f $(srctree)/scripts/consumer.mk
`)
	write("scripts/producer.mk", `
all: generated.stamp
generated.stamp:
	touch $@
`)
	write("scripts/consumer.mk", `
ifneq ($(wildcard generated.stamp),)
selected := present.out
else
selected := missing.out
endif
all: $(selected)
present.out:
	touch $@
missing.out:
	touch $@
`)
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var parent, consumer *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "Makefile":
			parent = &profiles[index]
		case "scripts/consumer.mk":
			consumer = &profiles[index]
		}
	}
	if parent == nil || consumer == nil {
		t.Fatalf("profiles omit parent or consumer: %#v", profiles)
	}
	generated, generatedOK := kconfig.CompactKbuildProfileInitialVisibleArtifact(*consumer, "generated.stamp")
	if !generatedOK {
		t.Fatalf("consumer initial frontier omits generated.stamp: %#v", testCompactKbuildInitialVisibleArtifacts(*consumer))
	}
	if generated.Profile != parent.Name || generated.Target != "generated.stamp" {
		t.Fatalf("generated.stamp owner = %#v, want later local writer profile %q", generated, parent.Name)
	}
	selected := map[string]bool{}
	for _, selection := range selections {
		if selection.Profile == consumer.Name {
			selected[selection.Target] = true
		}
	}
	if !selected["present.out"] || selected["missing.out"] {
		t.Fatalf("consumer selections = %#v, want wildcard-visible local rewrite", selected)
	}
}

func TestEvaluatedKbuildProfilesKeepIndependentPrerequisiteFrontiersIsolated(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
.PHONY: all left right
all: left right
	$(MAKE) -f $(srctree)/scripts/final.mk
left:
	$(MAKE) -f $(srctree)/scripts/left.mk
right:
	$(MAKE) -f $(srctree)/scripts/right.mk
`)
	write("scripts/left.mk", `
.PHONY: all
all: left.out
left.out:
	touch $@
`)
	write("scripts/right.mk", `
ifneq ($(wildcard left.out),)
selected := leaked.out
else
selected := isolated.out
endif
.PHONY: all
all: right.out $(selected)
right.out leaked.out isolated.out:
	touch $@
`)
	write("scripts/final.mk", `
left_seen := $(if $(wildcard left.out),left.present,left.missing)
right_seen := $(if $(wildcard right.out),right.present,right.missing)
.PHONY: all
all: $(left_seen) $(right_seen)
left.present left.missing right.present right.missing:
	touch $@
`)

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var left, right, final *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "scripts/left.mk":
			left = &profiles[index]
		case "scripts/right.mk":
			right = &profiles[index]
		case "scripts/final.mk":
			final = &profiles[index]
		}
	}
	if left == nil || right == nil || final == nil {
		t.Fatalf("profiles omit independent prerequisites or their join: %#v", profiles)
	}
	if len(right.InvocationPredecessors) != 0 || kconfig.CompactKbuildProfileInitialVisibleArtifactCount(*right) != 0 {
		t.Fatalf(
			"independent right prerequisite inherited left execution state: predecessors=%q artifacts=%#v",
			right.InvocationPredecessors, testCompactKbuildInitialVisibleArtifacts(*right),
		)
	}
	finalPaths := map[string]bool{}
	for _, artifact := range testCompactKbuildInitialVisibleArtifacts(*final) {
		finalPaths[artifact.Path] = true
	}
	if !finalPaths["left.out"] || !finalPaths["right.out"] {
		t.Fatalf("joined final frontier = %#v, want both independent outputs", testCompactKbuildInitialVisibleArtifacts(*final))
	}
	if got, want := len(final.InvocationPredecessors), 2; got != want {
		t.Fatalf("joined final predecessors = %q, want %d independent terminals", final.InvocationPredecessors, want)
	}
	selected := map[string]bool{}
	for _, selection := range selections {
		if selection.Profile == right.Name || selection.Profile == final.Name {
			selected[selection.Target] = true
		}
	}
	for _, target := range []string{"isolated.out", "left.present", "right.present"} {
		if !selected[target] {
			t.Errorf("independent/join selections omit %q: %#v", target, selected)
		}
	}
	for _, target := range []string{"leaked.out", "left.missing", "right.missing"} {
		if selected[target] {
			t.Errorf("independent/join selections unexpectedly include %q: %#v", target, selected)
		}
	}
}

func TestEvaluatedKbuildProfilesRejectIncomparablePrerequisiteWriters(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
.PHONY: all left right
all: left right
	$(MAKE) -f $(srctree)/scripts/final.mk
left:
	$(MAKE) -f $(srctree)/scripts/left.mk
right:
	$(MAKE) -f $(srctree)/scripts/right.mk
`)
	for _, child := range []string{"left", "right"} {
		write("scripts/"+child+".mk", `
.PHONY: all
all: shared.out
shared.out:
	touch $@
`)
	}
	write("scripts/final.mk", ".PHONY: all\nall:\n\t@:\n")

	variables := map[string]string{"SRCARCH": "x86"}
	_, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "incomparable versions") || !strings.Contains(err.Error(), "shared.out") {
		t.Fatalf("incomparable prerequisite writer error = %v", err)
	}
}

func TestSelectedKbuildRecursiveCallPreservesDeclarationLocalAutomaticTarget(t *testing.T) {
	root := t.TempDir()
	makefile := filepath.Join(root, "scripts", "parent.mk")
	if err := os.MkdirAll(filepath.Dir(makefile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makefile, []byte(`
descend = $(MAKE) -f $(srctree)/scripts/leaf.mk $(1)
local:
	$(call descend,$@)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		RootDir:                 root,
		WorkingDir:              filepath.Join(root, "parent"),
		Variables:               map[string]string{"srctree": kbuildEvalSourceTree},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
		SourceRoots:             map[string]string{kbuildEvalSourceTree: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := kconfig.NewCompactKbuildProfile("parent", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	kconfig.SetCompactKbuildProfileDirectory(&profile, "parent")
	profile.EntryTargets = []string{"parent/local"}
	setTestKbuildInvocationLocation(t, &profile)

	plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 1; got != want {
		t.Fatalf("recursive Make plan = %#v, want %d invocation", plan, want)
	}
	entry := plan[0]
	if got, want := entry.request.makefile, "scripts/leaf.mk"; got != want {
		t.Fatalf("child makefile = %q, want %q", got, want)
	}
	if got, want := entry.request.directory, "parent"; got != want {
		t.Fatalf("child directory = %q, want inherited %q", got, want)
	}
	if got, want := entry.request.entryTargets, []string{"parent/local"}; !slices.Equal(got, want) {
		t.Fatalf("child entry targets = %q, want canonical %q", got, want)
	}
	if got, want := entry.request.variables["MAKECMDGOALS"], "local"; got != want {
		t.Fatalf("child MAKECMDGOALS = %q, want declaration-local $@ value %q", got, want)
	}
	if got, want := entry.replayArguments, []string{"-f", kbuildEvalSourceTree + "/scripts/leaf.mk", "local"}; !slices.Equal(got, want) {
		t.Fatalf("child replay argv = %q, want declaration-local %q", got, want)
	}
}

func TestSelectedKbuildRecursiveMakePlanUsesInvocationContextObjAndSrc(t *testing.T) {
	const target = "lib/crc/nested/generated"
	profile := selectionRoleProfile(t, `
if_changed = $(cmd_$(1))
cmd_descend = $(MAKE) -C $(src) -f $(src)/child.mk obj=$(obj)/nested $@
lib/crc/nested/generated:
	$(call if_changed,descend)
`, target)
	profile.Directory = "lib/crc"

	plan, err := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan), 1; got != want {
		t.Fatalf("invocation-context recursive plan = %#v, want %d invocation", plan, want)
	}
	entry := plan[0]
	if got, want := entry.request.makefile, "lib/crc/child.mk"; got != want {
		t.Fatalf("child makefile = %q, want invocation-context $(src) path %q", got, want)
	}
	if got, want := entry.request.processLocation.Directory, "lib/crc"; got != want {
		t.Fatalf("child process directory = %q, want invocation-context $(src) directory %q", got, want)
	}
	if got, want := entry.request.directory, "lib/crc/nested"; got != want {
		t.Fatalf("child object directory = %q, want invocation-context $(obj) directory %q", got, want)
	}
	if got, want := entry.request.entryTargets, []string{target}; !slices.Equal(got, want) {
		t.Fatalf("child entry targets = %q, want object-directory-relative %q", got, want)
	}
	if got, want := entry.request.variables["obj"], "lib/crc/nested"; got != want {
		t.Fatalf("child planner obj = %q, want logical object directory %q", got, want)
	}
	if got, want := entry.replayArguments, []string{
		"-C", kbuildEvalSourceTree + "/lib/crc",
		"-f", kbuildEvalSourceTree + "/lib/crc/child.mk",
		"obj=" + kbuildEvalObjectTree + "/lib/crc/nested",
		target,
	}; !slices.Equal(got, want) {
		t.Fatalf("child replay argv = %q, want exact invocation-context provenance %q", got, want)
	}
}

func TestEvaluatedKbuildProfilesReadSourceDerivedRecursiveGeneratedText(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
build := -f $(srctree)/scripts/Makefile.build obj
.PHONY: all modules
all: modules
modules: modules.order
	$(MAKE) -f $(srctree)/scripts/Makefile.modfinal modules
modules.order:
	$(MAKE) $(build)=drivers/demo __build
	{ cat drivers/demo/modules.order; :; } > $@
`)
	write("scripts/Makefile.build", `
.PHONY: __build
__build: $(obj)/modules.order
$(obj)/demo.o:
	touch $@
$(obj)/modules.order: $(obj)/demo.o
	{ echo $(obj)/demo.o; :; } > $@
`)
	write("scripts/Makefile.modfinal", `
empty :=
space := $(empty) $(empty)
define newline


endef
read-file = $(subst $(newline),$(space),$(file < $1))
modules := $(call read-file,modules.order)
.PHONY: modules
modules: $(modules:%.o=%.ko)
%.ko: %.o
	cp $< $@
`)

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"all"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                 root,
			Variables:               variables,
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() failed: %v", err)
	}

	selectedModules := []string{}
	for _, selection := range selections {
		if strings.HasSuffix(selection.Target, ".ko") {
			selectedModules = append(selectedModules, selection.Target)
		}
	}
	if got, want := sortedUniquePaths(selectedModules), []string{"drivers/demo/demo.ko"}; !slices.Equal(got, want) {
		t.Fatalf("selected module targets = %q, want source-derived %q; selections=%#v", got, want, selections)
	}
	foundModfinal := false
	for _, profile := range profiles {
		if profile.Path != "scripts/Makefile.modfinal" {
			continue
		}
		foundModfinal = true
		if got, want := profile.EntryTargets, []string{"modules"}; !slices.Equal(got, want) {
			t.Fatalf("modfinal entry targets = %q, want %q", got, want)
		}
	}
	if !foundModfinal {
		t.Fatalf("profiles omit modfinal invocation: %#v", profiles)
	}
}

func TestEvaluatedKbuildProfilesReadNestedInvocationGeneratedText(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
.PHONY: all
all:
	$(MAKE) -C $(objtree)/external/demo -f $(srctree)/scripts/External modules
`)
	write("scripts/External", `
.PHONY: modules
modules: modules.order
	$(MAKE) -f $(srctree)/scripts/Makefile.modfinal
demo.o:
	touch $@
leaf.order: demo.o
	{ echo demo.o; :; } > $@
modules.order: leaf.order
	{ cat leaf.order; :; } > $@
`)
	write("scripts/Makefile.modfinal", `
empty :=
space := $(empty) $(empty)
define newline


endef
read-file = $(subst $(newline),$(space),$(file < $1))
modules := $(call read-file,modules.order)
.PHONY: __modfinal
__modfinal: $(modules:%.o=%.ko)
%.ko: %.o
	cp $< $@
`)
	if err := os.MkdirAll(filepath.Join(root, "external/demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"all"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                 root,
			Variables:               variables,
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() failed: %v", err)
	}

	selectedModules := []string{}
	for _, selection := range selections {
		if strings.HasSuffix(selection.Target, ".ko") {
			selectedModules = append(selectedModules, selection.Target)
		}
	}
	if got, want := sortedUniquePaths(selectedModules), []string{"external/demo/demo.ko"}; !slices.Equal(got, want) {
		t.Fatalf("selected nested module targets = %q, want source-derived %q; profiles=%#v selections=%#v", got, want, profiles, selections)
	}
}

func TestEvaluatedKbuildProfilesReadGeneratedModuleOrderAfterModpost(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
.PHONY: modules modpost
modules: modpost
ifneq ($(KBUILD_MODPOST_NOFINAL),1)
	$(MAKE) -f $(srctree)/scripts/Makefile.modfinal
endif
modpost: modules.order
	$(MAKE) -f $(srctree)/scripts/Makefile.modpost
modules.order: demo.o
	set -e; trap 'rm -f modules.order; trap - HUP; kill -s HUP $$$$' HUP; { echo demo.o; :; } > modules.order; printf '%s\n' 'savedcmd_modules.order := generated' > .modules.order.cmd
demo.o:
	touch $@
`)
	write("scripts/Makefile.modpost", `
.PHONY: __modpost
__modpost: Module.symvers
Module.symvers: modules.order
	cp $< $@
`)
	write("scripts/Makefile.modfinal", `
empty :=
space := $(empty) $(empty)
define newline


endef
read-file = $(subst $(newline),$(space),$(file < $1))
modules := $(call read-file, modules.order)
.PHONY: __modfinal
__modfinal: $(modules:%.o=%.ko)
%.ko: %.o %.mod.o
	cp $< $@
%.mod.o: %.mod.c
	cp $< $@
targets += $(modules:%.o=%.ko) $(modules:%.o=%.mod.o)
`)

	variables := map[string]string{"SRCARCH": "x86"}
	_, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"modules"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                 root,
			Variables:               variables,
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() failed: %v", err)
	}

	selected := map[string]bool{}
	for _, selection := range selections {
		selected[selection.Target] = true
	}
	for _, target := range []string{"demo.mod.o", "demo.ko"} {
		if !selected[target] {
			t.Fatalf("selections omit %q after modpost: %#v", target, selections)
		}
	}
	if selected["demo.mod.c"] {
		t.Fatalf("modpost side output demo.mod.c became a materialized selection: %#v", selections)
	}
}

func TestEvaluatedKbuildProfilesReadChildGeneratedExternalModuleOrderAfterModpost(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
.PHONY: all
all:
	$(MAKE) -C $(objtree)/external/demo -f $(srctree)/scripts/External modules
`)
	write("scripts/External", `
.PHONY: modules modpost modules_check .
modules.order: .
	@:
modules: modpost
	$(MAKE) -f $(srctree)/scripts/Makefile.modfinal
modpost: modules_check
	$(MAKE) -f $(srctree)/scripts/Makefile.modpost
modules_check: modules.order
	@:
.:
	$(MAKE) -f $(srctree)/scripts/Makefile.build obj=. __build
`)
	write("scripts/Kbuild.include", `
empty :=
space := $(empty) $(empty)
squote := '
pound := \#
define newline


endef
read-file = $(subst $(newline),$(space),$(file < $1))
dot-target = $(dir $@).$(notdir $@)
escsq = $(subst $(squote),'\$(squote)',$1)
delete-on-interrupt = $(if $(filter-out $(PHONY), $@), $(foreach sig, HUP INT QUIT TERM PIPE, trap 'rm -f $@; trap - $(sig); kill -s $(sig) $$$$' $(sig);))
cmd = @$(if $(cmd_$(1)),set -e; $(delete-on-interrupt) $(cmd_$(1)),:)
cmd-check = 1
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
newer-prereqs = $(filter-out $(PHONY),$?)
if-changed-cond = $(newer-prereqs)$(cmd-check)
if_changed = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
`)
	write("scripts/Makefile.build", `
include $(srctree)/scripts/Kbuild.include
.PHONY: __build FORCE
__build: $(obj)/modules.order
cmd_gen_order = { echo demo.o; :; } > $@
$(obj)/modules.order: $(obj)/demo.o FORCE
	$(call if_changed,gen_order)
$(obj)/demo.o:
	touch $@
FORCE:
`)
	write("scripts/Makefile.modpost", `
.PHONY: __modpost
__modpost: Module.symvers
Module.symvers: modules.order
	cp $< $@
`)
	write("scripts/Makefile.modfinal", `
include $(srctree)/scripts/Kbuild.include
modules := $(call read-file, modules.order)
.PHONY: __modfinal
__modfinal: $(modules:%.o=%.ko)
%.ko: %.o %.mod.o .module-common.o
	cp $< $@
%.mod.o: %.mod.c
	cp $< $@
.module-common.o:
	touch $@
targets += $(modules:%.o=%.ko) $(modules:%.o=%.mod.o) .module-common.o
`)
	if err := os.MkdirAll(filepath.Join(root, "external/demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(
		root,
		root,
		[]string{"all"},
		variables,
		kconfig.KbuildOptions{
			RootDir:                 root,
			Variables:               variables,
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		},
		nil,
	)
	if err != nil {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() failed: %v", err)
	}

	selected := map[string]bool{}
	for _, selection := range selections {
		selected[selection.Target] = true
	}
	for _, target := range []string{
		"external/demo/demo.mod.o",
		"external/demo/.module-common.o",
		"external/demo/demo.ko",
	} {
		if !selected[target] {
			t.Fatalf("selections omit %q after child modules.order and modpost: profiles=%#v selections=%#v", target, profiles, selections)
		}
	}
	if selected["external/demo/demo.mod.c"] {
		t.Fatalf("modpost side output became a materialized selection: %#v", selections)
	}
}

func TestEvaluatedKbuildProfilesDiscoverImageSubtreeWithoutSnapshottingVariables(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Kbuild", "obj-y += init.o drivers/demo/\n")
	write("Makefile", "srctree := "+filepath.ToSlash(root)+"\n"+`
	export boot := arch/$(SRCARCH)/boot
	export KBUILD_IMAGE := $(boot)/bzImage
export KBUILD_LDS := arch/x86/kernel/vmlinux.lds
include $(srctree)/scripts/Kbuild.include
root-terminal-y := vmlinux
PHONY += bzImage
.PHONY: $(PHONY)
all: prepare vmlinux bzImage
vmlinux: prepare
	$(Q)$(MAKE) -f $(srctree)/scripts/Makefile.vmlinux vmlinux
bzImage: vmlinux
ifeq ($(CONFIG_X86_DECODER_SELFTEST),y)
	$(Q)$(MAKE) $(build)=arch/x86/tools posttest
endif
	$(Q)$(MAKE) $(build)=arch/x86/boot $(KBUILD_IMAGE)
	$(Q)mkdir -p $(objtree)/arch/$(UTS_MACHINE)/boot
	$(Q)ln -fsn ../../x86/boot/bzImage $(objtree)/arch/$(UTS_MACHINE)/boot/$@
prepare: archscripts include/generated/demo.h include/generated/side-effect.h tools/helper tools/objtool
archscripts: scripts_basic
	$(Q)$(MAKE) $(build)=arch/x86/tools relocs
include/generated/demo.h: FORCE
	$(HOSTCC) -E -o $@ scripts/demo.h
include/generated/side-effect.h: FORCE
	$(shell mkdir -p $(dir $@))
	$(HOSTCC) -E -o $@ scripts/side-effect.h
kselftest:
	$(MAKE) -C $(srctree)/tools/testing/selftests run_tests

tools/%: FORCE
	$(Q)mkdir -p $(objtree)/tools
	$(MAKE) -C $(srctree)/tools $*
`)
	write("drivers/demo/Makefile", `
include $(src)/common/mmu/Makefile
obj-y += $(DEMO_MMU_OBJECTS)
hostprogs-y += unused-helper
`)
	write("drivers/demo/common/mmu/Makefile", "DEMO_MMU_OBJECTS := common/mmu/page.o\n")
	write("scripts/Makefile.build", `
-include $(src)/Kbuild
-include $(src)/Makefile
`)
	write("scripts/Kbuild.include", `build := -f $(srctree)/scripts/Makefile.build obj
if_changed = $(cmd_$(1))
if_changed_dep = $(cmd_$(1))
`)
	write("scripts/Makefile.vmlinux", `
targets += vmlinux.unstripped .vmlinux.export.o vmlinux
vmlinux.unstripped: scripts/link-vmlinux.sh vmlinux.o .vmlinux.export.o $(KBUILD_LDS) FORCE
	$(LD) -o $@ vmlinux.o
vmlinux: vmlinux.unstripped FORCE
`)
	write("scripts/Makefile.modpost", "Module.symvers: modules.order FORCE\n")
	write("scripts/Makefile.modfinal", "%.ko: %.o %.mod.o FORCE\n")
	write("tools/helper/Makefile", `
hostprogs-y := helper
helper: helper-in.o
helper-in.o: FORCE
	$(MAKE) $(build)=helper
`)
	write("tools/helper/Build.linux-bzl", `
hostprogs := helper
helper-y := main.o dynamic.o
$(OUTPUT)%.o: ../../lib/%.c FORCE
	$(call if_changed_dep,host_cc_o_c)
`)
	write("tools/Makefile", `
all: tracing
objtool:
	$(MAKE) -C objtool
tracing:
	$(MAKE) -C tracing
`)
	write("tools/objtool/Makefile", `
hostprogs-y := objtool
all: objtool
objtool: main.o
	$(HOSTCC) -o $@ $^
`)
	write("tools/tracing/Makefile", "all: latency\n")
	write("tools/tracing/latency/Makefile", "$(error pkg-config must not run for an unselected tool)\n")
	write("arch/x86/tools/Makefile", `
hostprogs += relocs
relocs-objs := relocs_32.o relocs_64.o relocs_common.o
PHONY += relocs
relocs: $(obj)/relocs
$(obj)/relocs: FORCE
	$(HOSTCC) -o $@ relocs.c
`)
	write("tools/testing/selftests/Makefile", "TARGETS := pstore\n")
	write("tools/testing/selftests/pstore/Makefile", "$(error set INSTALL_PATH to use install)\n")
	write("drivers/inactive/Makefile", `
UNSET_PREFIX :=
include $(UNSET_PREFIX)/impossible/Makefile
`)
	write("arch/x86/boot/Makefile", `
setup-y := a.o
setup-y += b.o
hostprogs-y := mkimage
targets += bzImage
quiet_cmd_image = BUILD $@
cmd_image = (cat $< $(filter-out $<,$(real-prereqs))) >$@
$(obj)/bzImage: $(obj)/setup.bin $(obj)/compressed/vmlinux $(obj)/tools/build FORCE
	$(call if_changed,image)
	@echo 'Kernel: $@ is ready'
`)
	write("arch/x86/boot/compressed/Makefile", `
vmlinux-objs-y := head.o
vmlinux-objs-y += misc.o
`)
	write("arch/x86/boot/tools/Makefile", "hostprogs-y := build\n")
	variables := map[string]string{"SRCARCH": "x86", "CONFIG_UNUSED": ""}
	options := kconfig.KbuildOptions{
		RootDir:   root,
		Variables: variables,
		CommandLineVariables: map[string]string{
			"CC":     kconfig.KbuildActionRoleToken("target", "cc"),
			"LD":     kconfig.KbuildActionRoleToken("target", "ld"),
			"HOSTCC": kconfig.KbuildActionRoleToken("host", "cc"),
		},
		ConfigVariablesComplete: true,
	}
	profiles, selections, imageTarget, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, options, nil)
	if err != nil {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() failed: %v", err)
	}
	if got, want := imageTarget, "arch/x86/boot/bzImage"; got != want {
		t.Fatalf("evaluated root image target = %q, want %q", got, want)
	}
	byName := map[string]kconfig.CompactKbuildProfile{}
	for _, profile := range profiles {
		byName[profile.Name] = profile
		if strings.HasPrefix(profile.Name, "terminal:") || strings.HasPrefix(profile.Name, "prep:") || strings.HasPrefix(profile.Name, "tool:") {
			t.Errorf("manufactured demand profile survived source-derived cutover: %q", profile.Name)
		}
	}
	findProfile := func(path, directory, target string) *kconfig.CompactKbuildProfile {
		t.Helper()
		for index := range profiles {
			profile := &profiles[index]
			if profile.Path == path && profile.Directory == directory && slices.Contains(profile.EntryTargets, target) {
				return profile
			}
		}
		return nil
	}
	vmlinux := findProfile("scripts/Makefile.vmlinux", "", "vmlinux")
	if vmlinux == nil {
		t.Fatalf("profiles omit source-selected vmlinux invocation: %#v", byName)
	}
	if got, want := vmlinux.Path, "scripts/Makefile.vmlinux"; got != want {
		t.Fatalf("vmlinux invocation path=%q, want %q", got, want)
	}
	if got, want := strings.Join(vmlinux.EntryTargets, " "), "vmlinux"; got != want {
		t.Fatalf("vmlinux invocation entry targets=%q, want %q", got, want)
	}
	var unstripped *kconfig.KbuildRule
	for index := range vmlinux.Rules {
		for _, target := range vmlinux.Rules[index].Targets {
			if target == "vmlinux.unstripped" {
				unstripped = &vmlinux.Rules[index]
			}
		}
	}
	if unstripped == nil {
		t.Fatal("terminal:vmlinux omits vmlinux.unstripped rule")
	}
	for _, prerequisite := range []string{"vmlinux.o", ".vmlinux.export.o", "arch/x86/kernel/vmlinux.lds"} {
		if !slices.Contains(unstripped.Prerequisites, prerequisite) {
			t.Errorf("vmlinux.unstripped prerequisites %q omit %q", unstripped.Prerequisites, prerequisite)
		}
	}
	image := findProfile("scripts/Makefile.build", "arch/x86/boot", "arch/x86/boot/bzImage")
	if image == nil {
		t.Fatalf("profiles omit source-selected image invocation: %#v", byName)
	}
	if got, want := image.Path, "scripts/Makefile.build"; got != want {
		t.Fatalf("image invocation path=%q, want %q", got, want)
	}
	if got, want := strings.Join(image.EntryTargets, " "), "arch/x86/boot/bzImage"; got != want {
		t.Fatalf("image invocation entry targets=%q, want %q", got, want)
	}
	if _, invented := byName["build:external"]; invented {
		t.Fatal("kernel planning invented an external-consumer Kbuild invocation")
	}
	if _, invented := byName["build:drivers/demo/common/mmu"]; invented {
		t.Fatal("nested object pathname invented a Kbuild invocation without recursive provenance")
	}
	selectionSet := map[string]bool{}
	for _, selection := range selections {
		selectionSet[selection.Stage+"\x00"+selection.Target] = true
		if _, ok := byName[selection.Profile]; !ok {
			t.Errorf("selection %#v references an unknown source profile", selection)
		}
	}
	for _, selected := range []string{
		"host\x00include/generated/demo.h",
		"host\x00include/generated/side-effect.h",
		"host\x00tools/objtool/objtool",
		"host\x00arch/x86/tools/relocs",
		"target\x00arch/x86/boot/bzImage",
		"target\x00vmlinux.unstripped",
	} {
		if !selectionSet[selected] {
			t.Errorf("source-derived selections omit %q: %#v", selected, selections)
		}
	}
	if selectionSet["prep\x00tools/objtool"] {
		t.Fatalf("recursive Make setup directory became a materialized artifact: %#v", selections)
	}
}

func TestEvaluatedKbuildProfilesExportSelectedStackProtectorControlEffectBeforeChildFlags(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Kbuild", "obj-y += demo.o\n")
	write("early/Kbuild", "obj-y += early.o\n")
	write("Makefile", `
export KBUILD_CFLAGS := -DBASE
all: prepare demo.o
demo.o: prepare
	$(Q)$(MAKE) -f $(srctree)/scripts/Makefile.build obj=. demo.o
prepare: stack_protector_prepare
stack_protector_prepare: prepare0
	$(eval KBUILD_CFLAGS += -mstack-protector-guard-offset=$(shell $(AWK) '{if ($$2 == "TSK_STACK_CANARY") print $$3;}' $(objtree)/include/generated/asm-offsets.h))
prepare0: include/generated/asm-offsets.h
	$(Q)$(MAKE) -f $(srctree)/scripts/Makefile.build obj=early early/early.o
include/generated/asm-offsets.h: FORCE
	$(CC) -o $@ scripts/asm-offsets.c
`)
	write("scripts/Makefile.build", `
KBUILD_CFLAGS += -DCHILD
-include $(src)/Kbuild
demo.o: demo.c
	$(CC) $(KBUILD_CFLAGS) -c -o $@ $<
early/early.o: early/early.c
	$(CC) $(KBUILD_CFLAGS) -c -o $@ $<
`)
	write("scripts/asm-offsets.c", "int generated_offset;\n")
	write("demo.c", "int demo;\n")
	write("early/early.c", "int early;\n")

	variables := map[string]string{"AWK": "awk", "CC": "cc", "SRCARCH": "arm64"}
	options := kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	var build, early *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/Makefile.build" && slices.Contains(profiles[index].EntryTargets, "demo.o") {
			build = &profiles[index]
		}
		if profiles[index].Path == "scripts/Makefile.build" && slices.Contains(profiles[index].EntryTargets, "early/early.o") {
			early = &profiles[index]
		}
	}
	if build == nil {
		t.Fatalf("profiles omit root build invocation: %#v", profiles)
	}
	if early == nil {
		t.Fatalf("profiles omit pre-control build invocation: %#v", profiles)
	}
	earlyValues, err := kconfig.EvaluateCompactKbuildTarget(*early, "early/early.o", "early", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := earlyValues["KBUILD_CFLAGS"], "-DBASE -DCHILD"; got != want || strings.Contains(got, "LINUX_BZL_KBUILD_CONTENT_") {
		t.Fatalf("pre-control child KBUILD_CFLAGS = %q, want %q without a deferred query", got, want)
	}
	values, err := kconfig.EvaluateCompactKbuildTarget(*build, "demo.o", "demo", nil, nil, nil, "KBUILD_CFLAGS")
	if err != nil {
		t.Fatal(err)
	}
	flags := strings.Fields(values["KBUILD_CFLAGS"])
	if len(flags) != 3 || flags[0] != "-DBASE" || flags[2] != "-DCHILD" ||
		!strings.HasPrefix(flags[1], "-mstack-protector-guard-offset=LINUX_BZL_KBUILD_CONTENT_") {
		t.Fatalf("child KBUILD_CFLAGS = %q, want root base, selected generated offset, then child-local flags", values["KBUILD_CFLAGS"])
	}
	selected := false
	for _, selection := range selections {
		if selection.Profile == build.Name && selection.Target == "demo.o" && selection.Stage == "target" {
			selected = true
			break
		}
	}
	if !selected {
		t.Fatalf("source-derived selections omit child demo.o from profile %q: %#v", build.Name, selections)
	}
}

func TestKbuildRecursiveMakeRequestDerivesDriverWorkingDirectoryAndGoals(t *testing.T) {
	request, ok, err := kbuildRecursiveMakeRequest(
		`__LINUX_BZL_MAKE__ -C __LINUX_BZL_SOURCE_TREE__/tools/objtool -f __LINUX_BZL_SOURCE_TREE__/tools/build/Makefile.build OUTPUT=out CFLAGS="-O2 -DSELECTED" all install ; echo ignored`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("recursive Make invocation was not recognized")
	}
	if got, want := request.makefile, "tools/build/Makefile.build"; got != want {
		t.Fatalf("makefile=%q, want %q", got, want)
	}
	if got, want := request.directory, "tools/objtool"; got != want {
		t.Fatalf("directory=%q, want %q", got, want)
	}
	if got, want := request.processLocation.Tree, kconfig.CompactKbuildInvocationSourceTree; got != want {
		t.Fatalf("process tree=%q, want %q", got, want)
	}
	if got, want := request.name, "driver:tools/build/Makefile.build@tools/objtool"; got != want {
		t.Fatalf("name=%q, want %q", got, want)
	}
	if got, want := strings.Join(request.entryTargets, " "), "tools/objtool/all tools/objtool/install"; got != want {
		t.Fatalf("entry targets=%q, want %q", got, want)
	}
	if got, want := request.variables["CFLAGS"], "-O2 -DSELECTED"; got != want {
		t.Fatalf("CFLAGS=%q, want %q", got, want)
	}
	if got, want := request.variables["MAKECMDGOALS"], "all install"; got != want {
		t.Fatalf("MAKECMDGOALS=%q, want %q", got, want)
	}
}

func TestKbuildRecursiveMakeRequestSeparatesCustomDriverObjectAndProcessDirectories(t *testing.T) {
	request, ok, err := kbuildRecursiveMakeRequest(
		`__LINUX_BZL_MAKE__ -C __LINUX_BZL_SOURCE_TREE__/tools/runner -f __LINUX_BZL_SOURCE_TREE__/scripts/custom-driver.mk obj=drivers/demo drivers/demo/leaf.o`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("recursive Make invocation was not recognized")
	}
	if got, want := request.makefile, "scripts/custom-driver.mk"; got != want {
		t.Fatalf("makefile=%q, want %q", got, want)
	}
	if got, want := request.directory, "drivers/demo"; got != want {
		t.Fatalf("logical object directory=%q, want obj= directory %q", got, want)
	}
	if got, want := request.processLocation.Directory, "tools/runner"; got != want {
		t.Fatalf("Make process directory=%q, want -C directory %q", got, want)
	}
	if got, want := request.processLocation.Tree, kconfig.CompactKbuildInvocationSourceTree; got != want {
		t.Fatalf("Make process tree=%q, want -C tree %q", got, want)
	}
	if got, want := request.name, "build:drivers/demo"; got != want {
		t.Fatalf("name=%q, want %q", got, want)
	}
	if got, want := request.entryTargets, []string{"drivers/demo/leaf.o"}; !slices.Equal(got, want) {
		t.Fatalf("entry targets=%q, want literal Make goals %q", got, want)
	}
}

func TestKbuildRecursiveMakeRequestDoesNotScopePhonyGoalToObjectDirectory(t *testing.T) {
	request, ok, err := kbuildRecursiveMakeRequest(
		`__LINUX_BZL_MAKE__ -f __LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build obj=arch/x86/entry/syscalls all`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("recursive Make invocation was not recognized")
	}
	if got, want := request.directory, "arch/x86/entry/syscalls"; got != want {
		t.Fatalf("logical object directory=%q, want %q", got, want)
	}
	if got, want := request.entryTargets, []string{"all"}; !slices.Equal(got, want) {
		t.Fatalf("entry targets=%q, want literal process-cwd goal %q", got, want)
	}
}

func TestKbuildRecursiveMakeRequestPreservesAndSwitchesInvocationTreeAcrossC(t *testing.T) {
	parent := kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationSourceTree, Directory: "tools/build",
	}
	preserved, ok, err := kbuildRecursiveMakeRequestAt(
		`__LINUX_BZL_MAKE__ -C ../objtool -f ../build/Makefile all`, parent,
	)
	if err != nil || !ok {
		t.Fatalf("relative source-tree request = (%#v, %t, %v)", preserved, ok, err)
	}
	if got, want := preserved.processLocation, (kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationSourceTree, Directory: "tools/objtool",
	}); got != want {
		t.Fatalf("relative -C location = %#v, want %#v", got, want)
	}

	switched, ok, err := kbuildRecursiveMakeRequestAt(
		`__LINUX_BZL_MAKE__ -C __LINUX_BZL_OBJECT_TREE__/arch/x86 -f __LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build all`, parent,
	)
	if err != nil || !ok {
		t.Fatalf("object-tree request = (%#v, %t, %v)", switched, ok, err)
	}
	if got, want := switched.processLocation, (kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationObjectTree, Directory: "arch/x86",
	}); got != want {
		t.Fatalf("explicit object -C location = %#v, want %#v", got, want)
	}

	if _, _, err := kbuildRecursiveMakeRequestAt(`__LINUX_BZL_MAKE__ -C ../../../escape all`, parent); err == nil {
		t.Fatal("recursive -C escaping its declared source tree was accepted")
	}
}

func TestEvaluatedKbuildProfilesSeparateCustomDriverObjectAndProcessDirectories(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all: custom-driver
custom-driver:
	$(MAKE) -C $(srctree)/tools/runner -f $(srctree)/scripts/custom-driver.mk obj=drivers/demo drivers/demo/leaf.o
`)
	write("tools/runner/.keep", "")
	write("drivers/demo/leaf.c", "int leaf;\n")
	write("scripts/custom-driver.mk", `
$(obj)/leaf.o: $(srctree)/drivers/demo/leaf.c FORCE
	$(CC) -c -o $@ $<
`)

	variables := map[string]string{
		"CC":      kconfig.KbuildActionRoleToken("target", "cc"),
		"SRCARCH": "x86",
	}
	profiles, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		CommandLineVariables:    variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var custom *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Path == "scripts/custom-driver.mk" {
			custom = &profiles[index]
			break
		}
	}
	if custom == nil {
		t.Fatalf("profiles omit selected custom driver: %#v", profiles)
	}
	if got, want := custom.Directory, "drivers/demo"; got != want {
		t.Fatalf("custom logical object directory=%q, want %q", got, want)
	}
	if got, ok := kconfig.CompactKbuildProfileInvocationLocation(*custom); !ok || got != (kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationSourceTree, Directory: "tools/runner",
	}) {
		t.Fatalf("custom Make process location=(%#v, %t), want source tree tools/runner", got, ok)
	}
	if got, want := custom.EntryTargets, []string{"drivers/demo/leaf.o"}; !slices.Equal(got, want) {
		t.Fatalf("custom entry targets=%q, want %q", got, want)
	}
	if !slices.ContainsFunc(custom.Rules, func(rule kconfig.KbuildRule) bool {
		return slices.Contains(rule.Targets, "drivers/demo/leaf.o")
	}) {
		t.Fatalf("custom driver rules omit obj=-scoped leaf: %#v", custom.Rules)
	}
}

func TestKbuildRecursiveMakeRequestPreservesObjectRootedGoalProvenance(t *testing.T) {
	request, ok, err := kbuildRecursiveMakeRequest(
		`__LINUX_BZL_MAKE__ -C __LINUX_BZL_SOURCE_TREE__/tools/build __LINUX_BZL_OBJECT_TREE__/tools/objtool/fixdep`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("recursive Make invocation was not recognized")
	}
	if got, want := request.entryTargets, []string{kbuildEvalObjectTree + "/tools/objtool/fixdep"}; !slices.Equal(got, want) {
		t.Fatalf("entry targets = %q, want object-root provenance %q", got, want)
	}
}

func TestKbuildProfileLookupTargetUsesSharedRootAndInvocationIdentity(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{Name: "split-root", Directory: "libsubcmd"}
	if err := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
		Tree: kconfig.CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, target, makeTarget, want string
	}{
		{
			name:   "rooted OUTPUT target",
			target: "tools/objtool/libsubcmd/exec-cmd.o", makeTarget: "tools/objtool/libsubcmd/exec-cmd.o",
			want: "tools/objtool/libsubcmd/exec-cmd.o",
		},
		{
			name:   "invocation-relative source target",
			target: "tools/lib/subcmd/exec-cmd.o", makeTarget: "exec-cmd.o",
			want: "tools/lib/subcmd/exec-cmd.o",
		},
		{
			name:   "parent traversal",
			target: "virt/kvm/kvm_main.o", makeTarget: "arch/x86/kvm/../../../virt/kvm/kvm_main.o",
			want: "arch/x86/kvm/../../../virt/kvm/kvm_main.o",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := kbuildProfileLookupTarget(profile, test.target, test.makeTarget); got != test.want {
				t.Fatalf("parser lookup target = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCanonicalChildGoalOutsideInvocationCwdIsNotRescoped(t *testing.T) {
	profile := kconfig.CompactKbuildProfile{
		Name:         "objtool-fixdep-child",
		Directory:    "tools/build",
		EntryTargets: []string{"tools/objtool/fixdep"},
		Rules: []kconfig.KbuildRule{{
			Targets: []string{"$(objtree)/tools/objtool/fixdep"},
			Recipe:  []string{"touch $@"},
		}},
	}
	profile = syntheticSelectionProfileWithEvaluator(t, profile)

	selections := mustSelectedKbuildSelections(t, []kconfig.CompactKbuildProfile{profile}, map[string]bool{})
	if got, want := selections, []kconfig.CompactKbuildSelection{{
		Profile:    profile.Name,
		Target:     "tools/objtool/fixdep",
		MakeTarget: "tools/objtool/fixdep",
		Lifecycle:  "target",
		Scope:      "target",
		Stage:      "target",
	}}; !slices.Equal(got, want) {
		t.Fatalf("canonical child selections = %#v, want %#v", got, want)
	}

	root := t.TempDir()
	makefile := filepath.Join(root, "tools", "build", "Makefile")
	if err := os.MkdirAll(filepath.Dir(makefile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makefile, []byte(`
$(objtree)/tools/objtool/fixdep:
	$(MAKE) -f $(srctree)/scripts/child.mk child
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := kconfig.ParseKbuildFileTree(makefile, kconfig.KbuildOptions{
		RootDir:    root,
		WorkingDir: filepath.Join(root, "tools", "build"),
		Variables: map[string]string{
			"objtree": kbuildEvalObjectTree,
			"srctree": kbuildEvalSourceTree,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
		SourceRoots: map[string]string{
			kbuildEvalObjectTree: root,
			kbuildEvalSourceTree: root,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recursiveProfile, err := kconfig.NewCompactKbuildProfile("objtool-recursive", makefile, root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	kconfig.SetCompactKbuildProfileDirectory(&recursiveProfile, "tools/build")
	recursiveProfile.EntryTargets = []string{"tools/objtool/fixdep"}
	setTestKbuildInvocationLocation(t, &recursiveProfile)
	requests, err := selectedKbuildRecursiveMakeRequests(recursiveProfile, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(requests), 1; got != want {
		t.Fatalf("recursive requests through canonical child goal = %#v, want %d", requests, want)
	}
}

func TestKbuildRecursiveMakeReplayArgumentsPreserveExactValues(t *testing.T) {
	invocations, err := kbuildRecursiveMakeInvocations(
		`__LINUX_BZL_MAKE__ -f ${tree:kernel}/scripts/child.mk FLAGS="-O2 -DSELECTED" '' child`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(invocations), 1; got != want {
		t.Fatalf("recursive Make invocations = %#v, want %d", invocations, want)
	}
	if got, want := invocations[0].replayArguments, []string{
		"-f", kbuildEvalSourceTree + "/scripts/child.mk", "FLAGS=-O2 -DSELECTED", "", "child",
	}; !slices.Equal(got, want) {
		t.Fatalf("recursive Make replay argv = %q, want %q", got, want)
	}
}

func TestDirectRecursiveMakeKeepsSymbolicAnalysisAndResolvesQuotedReplay(t *testing.T) {
	const (
		targetIdentity = "sha256-7171717171717171717171717171717171717171717171717171717171717171"
		hostIdentity   = "sha256-7272727272727272727272727272727272727272727272727272727272727272"
		target         = "tools/objtool/libsubcmd/libsubcmd.a"
	)
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, false),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.host, hostIdentity, false),
		"host", hostIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	hostDeps := filepath.Join(t.TempDir(), "configured-host-dependencies")
	makefile := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefile, []byte(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__hostcc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
hostcc-option = $(call __hostcc-option,$(HOSTCC),$(1),$(2))

EXTRA_WARNINGS := $(call hostcc-option,-Wformat-security)
KBUILD_HOSTCFLAGS := $(call hostcc-option,-Wstrict-aliasing=3,-Wno-strict-aliasing)
LIBSUBCMD_DIR = $(srctree)/tools/lib/subcmd
LIBSUBCMD_OUTPUT = $(objtree)/tools/objtool/libsubcmd
LIBSUBCMD = $(LIBSUBCMD_OUTPUT)/libsubcmd.a
OBJTOOL_CFLAGS := -Werror $(EXTRA_WARNINGS) $(KBUILD_HOSTCFLAGS) -iquote$(HOST_DEPS)/external/elfutils+
export EXPORTED_OBJTOOL_CFLAGS := $(OBJTOOL_CFLAGS)
HOST_OVERRIDES := CC="$(HOSTCC)"

$(LIBSUBCMD): FORCE
	$(Q)$(MAKE) -C $(LIBSUBCMD_DIR) O=$(LIBSUBCMD_OUTPUT) \
		DESTDIR=$(LIBSUBCMD_OUTPUT) prefix= subdir= \
		$(HOST_OVERRIDES) EXTRA_CFLAGS="$(OBJTOOL_CFLAGS)" \
		$@ install_headers

FORCE:
.PHONY: FORCE
`), 0o644); err != nil {
		t.Fatal(err)
	}
	probeOptions := kconfig.KbuildProbeWorkloadOptions{
		Target: kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: targetFacts, Tools: map[string]string{"cc": "/configured/target/cc"},
		},
		Host: &kconfig.KbuildProbeScopeOptions{
			Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: hostFacts, Tools: map[string]string{"cc": "/configured/host/cc"},
		},
	}
	type workloadValue struct {
		replayArguments []string
		environment     map[string]string
	}
	workload := func(scopes *kconfig.KbuildProbeScopes) (workloadValue, error) {
		options, optionsErr := scopes.Options("host", kconfig.KbuildOptions{
			RootDir: root,
			Variables: map[string]string{
				"HOSTCC":    probeOptions.Host.Tools["cc"],
				"HOST_DEPS": hostDeps,
				"SRCARCH":   "x86",
				"objtree":   kbuildEvalObjectTree,
				"srctree":   kbuildEvalSourceTree,
			},
			SourceRoots: map[string]string{
				kbuildEvalSourceTree:      root,
				kbuildEvalObjectTree:      root,
				"__LINUX_BZL_HOST_DEPS__": hostDeps,
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureTargetEvaluator:  true,
		})
		if optionsErr != nil {
			return workloadValue{}, optionsErr
		}
		parsed, parseErr := kconfig.ParseKbuildFileTree(makefile, options)
		if parseErr != nil {
			return workloadValue{}, parseErr
		}
		profile, profileErr := kconfig.NewCompactKbuildProfile("driver:tools/objtool/Makefile", makefile, root, parsed)
		if profileErr != nil {
			return workloadValue{}, profileErr
		}
		profile.EntryTargets = []string{target}
		if locationErr := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
			Tree: kconfig.CompactKbuildInvocationObjectTree,
		}); locationErr != nil {
			return workloadValue{}, locationErr
		}
		// Recursive-plan evaluation is the production boundary which originally
		// exposed the unresolved quoted EXTRA_CFLAGS assignment.
		plan, planErr := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
		if planErr != nil {
			return workloadValue{}, planErr
		}
		if len(plan) != 1 {
			return workloadValue{}, fmt.Errorf("recursive Make plan has %d entries, want one; profile rules: %#v", len(plan), profile.Rules)
		}
		return workloadValue{
			replayArguments: append([]string(nil), plan[0].replayArguments...),
			environment:     maps.Clone(plan[0].request.environment),
		}, nil
	}

	discovery, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(discovery.Plan.Nodes), 2; got != want {
		t.Fatalf("discovery plan has %d nodes, want %d compiler probes", got, want)
	}
	if got, want := strings.Count(strings.Join(discovery.Value.replayArguments, "\n"), "LINUX_BZL_PROBE_"), 2; got != want {
		t.Fatalf("discovery replay argv has %d probe atoms, want %d: %q", got, want, discovery.Value.replayArguments)
	}
	if got, want := strings.Count(discovery.Value.environment["EXPORTED_OBJTOOL_CFLAGS"], "LINUX_BZL_PROBE_"), 2; got != want {
		t.Fatalf("discovery exported flags have %d probe atoms, want %d: %#v", got, want, discovery.Value.environment)
	}

	resultRoot := t.TempDir()
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if request.Outcome.Kind != "boolean" {
			t.Fatalf("probe request %s outcome = %q, want boolean", node.RequestID, request.Outcome.Kind)
		}
		steps := make([]kconfig.ProbeStepResult, len(request.Steps))
		for index, step := range request.Steps {
			steps[index] = kconfig.ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
		}
		value := true
		writeTestProbeResult(t, resultRoot, kconfig.ProbeResult{
			Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: hostIdentity, Kind: "boolean", Boolean: &value, Steps: steps,
		})
	}
	oracle, err := kconfig.NewProbeResultOracleFromTrees(
		map[string]string{"host": resultRoot}, discovery.Plan.Toolsets,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	discoveryPlan, err := json.Marshal(discovery.Plan)
	if err != nil {
		t.Fatal(err)
	}
	replayPlan, err := json.Marshal(replay.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(discoveryPlan, replayPlan) {
		t.Fatalf("replay probe plan differs from discovery\ndiscovery: %s\nreplay: %s", discoveryPlan, replayPlan)
	}
	wantExtraFlags := "EXTRA_CFLAGS=-Werror -Wformat-security -Wstrict-aliasing=3 -iquote${tree:host_deps}/external/elfutils+"
	if !slices.Contains(replay.Value.replayArguments, wantExtraFlags) {
		t.Fatalf("replay argv = %q, want exact quoted assignment %q", replay.Value.replayArguments, wantExtraFlags)
	}
	if strings.Contains(strings.Join(replay.Value.replayArguments, "\n"), "LINUX_BZL_PROBE_") {
		t.Fatalf("replay argv retains compiler-probe atoms: %q", replay.Value.replayArguments)
	}
	if got, want := replay.Value.environment["EXPORTED_OBJTOOL_CFLAGS"], discovery.Value.environment["EXPORTED_OBJTOOL_CFLAGS"]; got != want {
		t.Fatalf("replay analysis flags = %q, want symbolic discovery value %q", got, want)
	}
	if got, want := strings.Count(replay.Value.environment["EXPORTED_OBJTOOL_CFLAGS"], "LINUX_BZL_PROBE_"), 2; got != want {
		t.Fatalf("replay analysis flags have %d probe atoms, want %d: %#v", got, want, replay.Value.environment)
	}
}

func TestDirectRecursiveMakeProbeBackedBindingsRetainChildCompilerProbeDependency(t *testing.T) {
	const (
		targetIdentity = "sha256-7373737373737373737373737373737373737373737373737373737373737373"
		hostIdentity   = "sha256-7474747474747474747474747474747474747474747474747474747474747474"
	)
	bootstrap, err := newLinuxCompilerBootstrapPlan(targetIdentity, hostIdentity)
	if err != nil {
		t.Fatal(err)
	}
	targetFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.target, targetIdentity, false),
		"target", targetIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostFacts, err := kconfig.ParseLinuxCompilerBootstrapResult(
		testLinuxCompilerBootstrapResult(t, bootstrap.host, hostIdentity, false),
		"host", hostIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name                string
		recipe              string
		commandLineBinding  bool
		wantReplayArguments []string
	}{
		{
			name:               "after Make argv assignment",
			recipe:             `$(MAKE) -f $(srctree)/child.mk EXTRA_CFLAGS="$(PROBED_FLAGS)" child`,
			commandLineBinding: true,
			wantReplayArguments: []string{
				"-f", kbuildEvalSourceTree + "/child.mk", "EXTRA_CFLAGS=-Wkeep", "child",
			},
		},
		{
			name:   "before Make environment assignment",
			recipe: `EXTRA_CFLAGS="$(PROBED_FLAGS)" $(MAKE) -f $(srctree)/child.mk child`,
			wantReplayArguments: []string{
				"-f", kbuildEvalSourceTree + "/child.mk", "child",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			parentPath := filepath.Join(root, "Makefile")
			parentMakefile := `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__hostcc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
hostcc-option = $(call __hostcc-option,$(HOSTCC),$(1),$(2))

PROBED_PATTERN := $(call hostcc-option,-Wdrop%)
DYNAMIC_FLAGS := $(filter-out $(PROBED_PATTERN),-Wdrop-value -Wkeep)
PROBED_FLAGS := $(filter-out ,$(DYNAMIC_FLAGS))

all: FORCE
` + "\t" + test.recipe + `

FORCE:
.PHONY: all FORCE
`
			if err := os.WriteFile(parentPath, []byte(parentMakefile), 0o644); err != nil {
				t.Fatal(err)
			}
			childPath := filepath.Join(root, "child.mk")
			if err := os.WriteFile(childPath, []byte(`
pound := \#
CHILD_FLAGS := $(strip $(EXTRA_CFLAGS))
header_available := $(shell echo '$(pound)include <libelf.h>' | $(HOSTCC) $(CHILD_FLAGS) -x c -E - 2>/dev/null | grep elf_getshdr)
CHILD_FLAGS += $(if $(header_available),,-DLIBELF_USE_DEPRECATED)

child: input.c
	$(HOSTCC) $(CHILD_FLAGS) -c -o $@ $<
`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "input.c"), []byte("int value;\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			probeOptions := kconfig.KbuildProbeWorkloadOptions{
				Target: kconfig.KbuildProbeScopeOptions{
					Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
					Facts: targetFacts, Tools: map[string]string{"cc": "/configured/target/cc"},
				},
				Host: &kconfig.KbuildProbeScopeOptions{
					Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
					Facts: hostFacts, Tools: map[string]string{"cc": "/configured/host/cc"},
				},
			}
			type workloadValue struct {
				analysisBinding string
				childFlags      string
				replayArguments []string
			}
			workload := func(scopes *kconfig.KbuildProbeScopes) (workloadValue, error) {
				parentOptions, optionsErr := scopes.Options("host", kconfig.KbuildOptions{
					RootDir: root,
					Variables: map[string]string{
						"HOSTCC":  probeOptions.Host.Tools["cc"],
						"MAKE":    "__LINUX_BZL_MAKE__",
						"SRCARCH": "x86",
						"objtree": kbuildEvalObjectTree,
						"srctree": kbuildEvalSourceTree,
					},
					SourceRoots: map[string]string{
						kbuildEvalSourceTree: root,
						kbuildEvalObjectTree: root,
					},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureTargetEvaluator:  true,
				})
				if optionsErr != nil {
					return workloadValue{}, optionsErr
				}
				parsed, parseErr := kconfig.ParseKbuildFileTree(parentPath, parentOptions)
				if parseErr != nil {
					return workloadValue{}, parseErr
				}
				profile, profileErr := kconfig.NewCompactKbuildProfile("driver:Makefile", parentPath, root, parsed)
				if profileErr != nil {
					return workloadValue{}, profileErr
				}
				profile.EntryTargets = []string{"all"}
				if locationErr := kconfig.SetCompactKbuildProfileInvocationLocation(&profile, kconfig.CompactKbuildInvocationLocation{
					Tree: kconfig.CompactKbuildInvocationObjectTree,
				}); locationErr != nil {
					return workloadValue{}, locationErr
				}
				plan, planErr := selectedKbuildRecursiveMakePlan(profile, map[string]bool{})
				if planErr != nil {
					return workloadValue{}, planErr
				}
				if len(plan) != 1 {
					return workloadValue{}, fmt.Errorf("recursive Make plan has %d entries, want one", len(plan))
				}
				entry := plan[0]
				analysisBinding := entry.request.environment["EXTRA_CFLAGS"]
				if test.commandLineBinding {
					analysisBinding = entry.request.variables["EXTRA_CFLAGS"]
				}
				if analysisBinding == "" {
					return workloadValue{}, fmt.Errorf(
						"recursive Make request omits EXTRA_CFLAGS binding: environment=%#v variables=%#v",
						entry.request.environment, entry.request.variables,
					)
				}
				childVariables := maps.Clone(entry.request.environment)
				if childVariables == nil {
					childVariables = map[string]string{}
				}
				for name, value := range entry.request.variables {
					childVariables[name] = value
				}
				childVariables["HOSTCC"] = probeOptions.Host.Tools["cc"]
				childVariables["SRCARCH"] = "x86"
				childOptions, optionsErr := scopes.Options("host", kconfig.KbuildOptions{
					RootDir:                 root,
					Variables:               childVariables,
					EnvironmentVariables:    maps.Clone(entry.request.environment),
					CommandLineVariables:    maps.Clone(entry.request.variables),
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureVariables:        []string{"CHILD_FLAGS"},
				})
				if optionsErr != nil {
					return workloadValue{}, optionsErr
				}
				child, childErr := kconfig.ParseKbuildFileTree(childPath, childOptions)
				if childErr != nil {
					return workloadValue{}, childErr
				}
				childFlags, resolveErr := childOptions.ResolveSymbolic(child.Variables["CHILD_FLAGS"])
				if resolveErr != nil {
					return workloadValue{}, resolveErr
				}
				return workloadValue{
					analysisBinding: analysisBinding,
					childFlags:      childFlags,
					replayArguments: append([]string(nil), entry.replayArguments...),
				}, nil
			}

			discovery, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, nil, workload)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(discovery.Plan.Nodes), 2; got != want {
				t.Fatalf("discovery plan has %d nodes, want parent and child probes: %#v", got, discovery.Plan.Nodes)
			}
			childIndex := -1
			for index, node := range discovery.Plan.Nodes {
				if len(node.Inputs) != 0 {
					if childIndex >= 0 {
						t.Fatalf("discovery plan has multiple dependent probes: %#v", discovery.Plan.Nodes)
					}
					childIndex = index
				}
			}
			if childIndex < 0 {
				t.Fatalf("discovery plan has no child probe dependency: %#v", discovery.Plan.Nodes)
			}
			childNode := discovery.Plan.Nodes[childIndex]
			if len(childNode.Inputs) != 1 {
				t.Fatalf("child probe inputs = %q, want one parent", childNode.Inputs)
			}
			parentFound := false
			for _, node := range discovery.Plan.Nodes {
				if node.ID == childNode.Inputs[0] && node.Scope == "host" {
					parentFound = true
				}
			}
			if childNode.Scope != "host" || !parentFound {
				t.Fatalf("recursive probe dependency = %#v, want host child depending on host parent", discovery.Plan.Nodes)
			}
			if !strings.Contains(discovery.Value.analysisBinding, "LINUX_BZL_PROBE_") ||
				!strings.Contains(discovery.Value.childFlags, "LINUX_BZL_PROBE_") {
				t.Fatalf("discovery resolved recursive probe values early: %#v", discovery.Value)
			}

			resultRoot := t.TempDir()
			for _, node := range discovery.Plan.Nodes {
				request := discovery.Plan.Requests[node.RequestID]
				steps := make([]kconfig.ProbeStepResult, len(request.Steps))
				for index, step := range request.Steps {
					steps[index] = kconfig.ProbeStepResult{
						Name: step.Name, Status: "success", ExitCode: 0, Stdout: "elf_getshdr\n",
					}
				}
				value := true
				writeTestProbeResult(t, resultRoot, kconfig.ProbeResult{
					Schema: kconfig.LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
					Scope: node.Scope, ToolsetIdentity: hostIdentity, Kind: "boolean", Boolean: &value, Steps: steps,
				})
			}
			oracle, err := kconfig.NewProbeResultOracleFromTrees(
				map[string]string{"host": resultRoot}, discovery.Plan.Toolsets,
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := kconfig.EvaluateKbuildProbeWorkload(probeOptions, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			discoveryPlan, err := json.Marshal(discovery.Plan)
			if err != nil {
				t.Fatal(err)
			}
			replayPlan, err := json.Marshal(replay.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(discoveryPlan, replayPlan) {
				t.Fatalf("replay probe plan differs from discovery\ndiscovery: %s\nreplay: %s", discoveryPlan, replayPlan)
			}
			if !strings.Contains(replay.Value.analysisBinding, "LINUX_BZL_PROBE_") {
				t.Fatalf("replay child-analysis binding lost probe provenance: %q", replay.Value.analysisBinding)
			}
			if got, want := strings.TrimSpace(replay.Value.childFlags), "-Wkeep"; got != want {
				t.Fatalf("replay child flags = %q, want concrete %q", got, want)
			}
			if got := replay.Value.replayArguments; !slices.Equal(got, test.wantReplayArguments) {
				t.Fatalf("replay argv = %q, want exact concrete argv %q", got, test.wantReplayArguments)
			}
			if strings.Contains(strings.Join(replay.Value.replayArguments, "\n"), "LINUX_BZL_PROBE_") {
				t.Fatalf("replay argv retains compiler-probe atoms: %q", replay.Value.replayArguments)
			}
		})
	}
}

func TestEvaluatedKbuildProfilesExportSourceOwnedRootAliasesToSelectedSourceScriptReplay(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
this-makefile := $(lastword $(MAKEFILE_LIST))
abs_srctree := $(realpath $(dir $(this-makefile)))
abs_output := $(CURDIR)
ifneq ($(sub_make_done),1)
export objtree srcroot
export sub_make_done := 1
endif
export srctree := $(srcroot)
all:
	$(MAKE) -f $(srctree)/scripts/Makefile.vmlinux vmlinux.unstripped
`)
	write("scripts/Makefile.vmlinux", `
CONFIG_SHELL := sh
cmd_selected = $< "$(LD)" "$@"
if_changed_dep = $(cmd_$(1))
vmlinux.unstripped: scripts/link-vmlinux.sh FORCE
	+$(call if_changed_dep,selected)
`)
	write("scripts/link-vmlinux.sh", `#!/bin/sh
# ${MAKE} -f "${srctree}/scripts/not-selected.mk" ignored
printf '%s\n' '${MAKE} -f "${srctree}/scripts/not-selected.mk" ignored'
${MAKE} -f "${srctree}/scripts/Makefile.build" \
	ROOT_SRCTREE="${srctree}" \
	ROOT_OBJTREE="${objtree}" \
	ROOT_SRCROOT="${srcroot}" \
	ROOT_SUB_MAKE_DONE="${sub_make_done}" \
	obj=init \
	init/version-timestamp.o
`)
	write("scripts/Makefile.build", `
init/version-timestamp.o: FORCE
	touch $@
`)

	variables := linuxRootMakeInvocationVariables(root)
	variables["SRCARCH"] = "x86"
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir:                 root,
		Variables:               variables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var link, timestamp *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "scripts/Makefile.vmlinux":
			link = &profiles[index]
		case "scripts/Makefile.build":
			if slices.Contains(profiles[index].EntryTargets, "init/version-timestamp.o") {
				timestamp = &profiles[index]
			}
		}
	}
	if link == nil || timestamp == nil {
		t.Fatalf("profiles omit source-script recursive invocation: %#v", profiles)
	}
	environment, err := kconfig.EvaluateCompactKbuildTargetEnvironmentSymbolic(
		*link, "vmlinux.unstripped", "", nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"objtree":       kbuildEvalObjectTree,
		"srcroot":       kbuildEvalSourceTree,
		"srctree":       kbuildEvalSourceTree,
		"sub_make_done": "1",
	} {
		if got := environment[name]; got != want {
			t.Errorf("source-script environment %s = %q, want %q; environment=%#v", name, got, want, environment)
		}
	}
	for _, name := range []string{"abs_output", "abs_srctree"} {
		if _, ok := environment[name]; ok {
			t.Errorf("source-script environment unexpectedly exports planner-only alias %s: %#v", name, environment)
		}
	}
	var dependency *kconfig.CompactKbuildInvocationDependency
	for index := range link.TargetInvocationDependencies {
		candidate := &link.TargetInvocationDependencies[index]
		if candidate.Target == "vmlinux.unstripped" && candidate.Profile == timestamp.Name {
			dependency = candidate
			break
		}
	}
	if dependency == nil {
		t.Fatalf("link target dependencies = %#v, want timestamp invocation %q", link.TargetInvocationDependencies, timestamp.Name)
	}
	if got, want := dependency.Goals, []string{"init/version-timestamp.o"}; !slices.Equal(got, want) {
		t.Fatalf("source-script child goals = %q, want %q", got, want)
	}
	if got, want := dependency.ReplayArguments, []string{
		"-f", kbuildEvalSourceTree + "/scripts/Makefile.build",
		"ROOT_SRCTREE=" + kbuildEvalSourceTree,
		"ROOT_OBJTREE=" + kbuildEvalObjectTree,
		"ROOT_SRCROOT=" + kbuildEvalSourceTree,
		"ROOT_SUB_MAKE_DONE=1",
		"obj=init", "init/version-timestamp.o",
	}; !slices.Equal(got, want) {
		t.Fatalf("source-script replay argv = %q, want %q", got, want)
	}
	if !slices.ContainsFunc(selections, func(selection kconfig.CompactKbuildSelection) bool {
		return selection.Profile == timestamp.Name && selection.Target == "init/version-timestamp.o"
	}) {
		t.Fatalf("selections omit source-script child target from %q: %#v", timestamp.Name, selections)
	}
}

func TestSourceScriptRecursiveMakeResolvesExportedTargetEnvironment(t *testing.T) {
	invocations, err := kbuildSourceScriptRecursiveMakeInvocationsAt(`
$MAKE -f "$objtree/$DRIVER" obj=$OBJECT_DIR $GOAL
`, map[string]string{
		"DRIVER":     "scripts/Makefile.build",
		"GOAL":       "drivers/example/module.o",
		"OBJECT_DIR": "drivers/example",
		"objtree":    kbuildEvalObjectTree,
	}, kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(invocations), 1; got != want {
		t.Fatalf("source-script invocations = %#v, want %d", invocations, want)
	}
	if got, want := invocations[0].replayArguments, []string{
		"-f", kbuildEvalObjectTree + "/scripts/Makefile.build",
		"obj=drivers/example", "drivers/example/module.o",
	}; !slices.Equal(got, want) {
		t.Fatalf("export-resolved replay argv = %q, want %q", got, want)
	}
}

func TestSourceScriptRecursiveMakeIgnoresDataUseAndFindsConditionalExecutable(t *testing.T) {
	invocations, err := kbuildSourceScriptRecursiveMakeInvocationsAt(`
echo "$MAKE"
if test -f marker; then "$MAKE" child; fi
`, map[string]string{
		"MAKE": "__LINUX_BZL_MAKE__",
	}, kconfig.CompactKbuildInvocationLocation{Tree: kconfig.CompactKbuildInvocationObjectTree})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("source-script invocations = %#v, want one", invocations)
	}
	if got := invocations[0].request.variables["MAKECMDGOALS"]; got != "child" {
		t.Fatalf("source-script recursive Make goals = %q, want child", got)
	}
}

func TestEvaluatedKbuildProfilesRejectInconsistentReplayArgumentsForOneInvocation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	makefile := `
all: first second
first:
	$(MAKE) -f $(srctree)/scripts/child.mk FLAG=selected child
second:
	$(MAKE) -f $(srctree)/scripts/child.mk child FLAG=selected
`
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "child.mk"), []byte("child:\n\ttouch $@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{"SRCARCH": "x86"}
	_, _, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		RootDir: root, Variables: variables, ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "inconsistent replay argv") {
		t.Fatalf("evaluatedKbuildProfilesWithOptions() error = %v, want inconsistent replay argv", err)
	}
}

func TestKbuildRecursiveMakeRequestLeavesGoalToEvaluatedBuildDefault(t *testing.T) {
	request, ok, err := kbuildRecursiveMakeRequest(
		`__LINUX_BZL_MAKE__ -f __LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build obj=drivers/demo need-builtin=1`,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("recursive Make invocation was not recognized")
	}
	if got, want := request.name, "build:drivers/demo"; got != want {
		t.Fatalf("name=%q, want %q", got, want)
	}
	if len(request.entryTargets) != 0 {
		t.Fatalf("entry targets=%q, want source-evaluated default goal", request.entryTargets)
	}
	if got, want := request.variables["need-builtin"], "1"; got != want {
		t.Fatalf("need-builtin=%q, want %q", got, want)
	}
}

func testCompactKbuildInitialVisibleArtifacts(
	profile kconfig.CompactKbuildProfile,
) []kconfig.CompactKbuildVisibleArtifact {
	artifacts := make([]kconfig.CompactKbuildVisibleArtifact, 0,
		kconfig.CompactKbuildProfileInitialVisibleArtifactCount(profile))
	kconfig.RangeCompactKbuildProfileInitialVisibleArtifacts(
		profile, "", func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
			artifacts = append(artifacts, artifact)
			return true
		},
	)
	return artifacts
}

func setTestCompactKbuildInitialVisibleArtifacts(
	t *testing.T,
	profile *kconfig.CompactKbuildProfile,
	artifacts []kconfig.CompactKbuildVisibleArtifact,
) {
	t.Helper()
	state := kbuildFrontierState{}
	for _, artifact := range artifacts {
		if artifact.Path == "" || artifact.Path != kconfig.CanonicalKbuildGraphTarget(artifact.Path) {
			t.Fatalf("test initial visible artifact has noncanonical path %#v", artifact)
		}
		if _, duplicate := kbuildFrontierGet(state, artifact.Path); duplicate {
			t.Fatalf("test initial visible artifact path %q is duplicated", artifact.Path)
		}
		state = kbuildFrontierSet(state, artifact.Path, kbuildFrontierValue{artifact: artifact})
	}
	kconfig.SetCompactKbuildProfileInitialVisibleArtifactView(
		profile, kbuildFrontierArtifactView{state: state},
	)
}
