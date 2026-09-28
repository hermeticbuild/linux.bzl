package kconfig

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type probeResultMap map[string]ProbeResult

func (m probeResultMap) Result(reference ProbeReference) (ProbeResult, error) {
	result, ok := m[reference.NodeID]
	if !ok {
		return ProbeResult{}, fmt.Errorf("missing fixture result %s", reference.NodeID)
	}
	if result.RequestID != reference.RequestID || result.Scope != reference.Scope || result.Kind != reference.Kind {
		return ProbeResult{}, fmt.Errorf("fixture result does not match reference")
	}
	return result, nil
}

type countingProbeDiscovery struct {
	delegate ProbeDiscovery
	requests int
}

func (d *countingProbeDiscovery) Request(scope string, request ProbeRequest, dependencies ...ProbeReference) (ProbeReference, error) {
	d.requests++
	return d.delegate.Request(scope, request, dependencies...)
}

type countingProbeResultLookup struct {
	delegate ProbeResultLookup
	results  int
}

func TestLinuxProbeEvaluatorRejectsPrivateRecursiveMakeBytesFromTextResults(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	request := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "text", Tool: "cc"}},
		Outcome: ProbeOutcome{Kind: "text", Step: "text", Stream: "stdout"},
	}
	reference, err := builder.Request("target", request)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		value     string
		wantError bool
	}{
		{name: "opening", value: "prefix\x05suffix", wantError: true},
		{name: "closing", value: "prefix\x06suffix", wantError: true},
		{name: "printable marker", value: compactKbuildRecursiveMakeMarker},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := &ProbeResultOracle{
				toolsets: map[string]string{"target": bootstrapTestIdentity},
				results: map[string]ProbeResult{
					reference.NodeID: {
						Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
						Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: test.value,
						Steps: []ProbeStepResult{{Name: "text", Status: "success", ExitCode: 0, Stdout: test.value}},
					},
				},
			}
			evaluator := &LinuxProbeEvaluator{oracle: oracle, symbolRegistry: newLinuxProbeSymbolRegistry()}
			got, err := evaluator.readText(reference, request)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
					t.Fatalf("readText() error = %v, want reserved-provenance rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.value {
				t.Fatalf("readText() = %q, want printable value %q", got, test.value)
			}
		})
	}
}

func TestLinuxProbeEvaluatorReplaysCompilerPathsWithExecutionRootProvenance(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: "print-file", Tool: "cc", Arguments: []string{"-print-file-name=include"},
			StdoutExecrootRelative: true, StdoutFallbackPath: "include",
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "print-file", Stream: "stdout", TrimSpace: true},
	}
	reference, err := builder.Request("target", request)
	if err != nil {
		t.Fatal(err)
	}
	toolchainPath, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/gcc/lib/gcc/aarch64-linux/15.2.0/include")
	if err != nil {
		t.Fatal(err)
	}
	exactSentinelArtifact, err := toolaction.EncodeExecutionRootProvenancePath("target", "include")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, result, pathKind, want string
	}{
		{
			name: "identity-bound toolchain directory", result: "external/gcc/lib/gcc/aarch64-linux/15.2.0/include",
			pathKind: ProbeStdoutPathToolset, want: toolchainPath,
		},
		{name: "real artifact with fallback spelling", result: "include", pathKind: ProbeStdoutPathToolset, want: exactSentinelArtifact},
		{name: "unresolved compiler fallback", result: "include", pathKind: ProbeStdoutPathFallback, want: "include"},
	} {
		t.Run(test.name, func(t *testing.T) {
			oracle := &ProbeResultOracle{
				toolsets: map[string]string{"target": bootstrapTestIdentity},
				results: map[string]ProbeResult{
					reference.NodeID: {
						Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
						Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: test.result,
						Steps: []ProbeStepResult{{
							Name: "print-file", Status: "success", ExitCode: 0, Stdout: test.result, StdoutPathKind: test.pathKind,
						}},
					},
				},
			}
			registry := newLinuxProbeSymbolRegistry()
			evaluator := &LinuxProbeEvaluator{oracle: oracle, symbolRegistry: registry}
			got, err := evaluator.readText(reference, request)
			if err != nil {
				t.Fatal(err)
			}
			codec, err := registry.executionRootProvenanceCapabilityCodec()
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := codec.NormalizeValue(got)
			if err != nil {
				t.Fatal(err)
			}
			if normalized != test.want {
				t.Fatalf("normalized readText() = %q, want %q (transient %q)", normalized, test.want, got)
			}
			if test.pathKind == ProbeStdoutPathToolset && got == normalized {
				t.Fatalf("toolset path replay exposed unauthenticated deterministic token %q", got)
			}
			if stored, err := oracle.Result(reference); err != nil || stored.Text != test.result {
				t.Fatalf("durable result = %#v, %v; want canonical relative text %q", stored, err, test.result)
			}
		})
	}
}

func TestLinuxProbeEvaluatorReauthorizesStableConfigPathOnlyAfterExactReplay(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	stable, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/compiler/vendor-sdk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.NormalizeOrAuthorizeToolsetPathCapabilities(stable); err == nil || !strings.Contains(err.Error(), "was not measured") {
		t.Fatalf("unmeasured stable config path error = %v, want authorization rejection", err)
	}
	evaluator.symbolRegistry.authorizeToolsetPath("target", "external/compiler/vendor-sdk")
	if got, err := evaluator.NormalizeOrAuthorizeToolsetPathCapabilities(stable); err != nil || got != stable {
		t.Fatalf("measured stable config path = %q, error %v, want %q", got, err, stable)
	}
	other, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/compiler/other-sdk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evaluator.NormalizeOrAuthorizeToolsetPathCapabilities(other); err == nil || !strings.Contains(err.Error(), "was not measured") {
		t.Fatalf("different stable config path error = %v, want exact authorization rejection", err)
	}

	codec, err := evaluator.symbolRegistry.executionRootProvenanceCapabilityCodec()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("target", "external/compiler/vendor-sdk")
	if err != nil {
		t.Fatal(err)
	}
	mutated := capability[:len(capability)-1] + map[bool]string{true: "0", false: "1"}[capability[len(capability)-1] != '0']
	if _, err := evaluator.NormalizeOrAuthorizeToolsetPathCapabilities(mutated); err == nil || !strings.Contains(err.Error(), "authenticated planning capability") {
		t.Fatalf("mutated capability error = %v, want keyed rejection without raw fallback", err)
	}
}

func TestLinuxProbeEvaluatorRejectsRecursiveMakeTokenReconstructedByDerivedText(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	leafRequest := func(name string) ProbeRequest {
		return ProbeRequest{
			Schema:  LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: name, Tool: "cc"}},
			Outcome: ProbeOutcome{Kind: "text", Step: name, Stream: "stdout"},
		}
	}
	openingRequest := leafRequest("opening")
	opening, err := builder.Request("target", openingRequest)
	if err != nil {
		t.Fatal(err)
	}
	closingRequest := leafRequest("closing")
	closing, err := builder.Request("target", closingRequest)
	if err != nil {
		t.Fatal(err)
	}
	derivedRequest := ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: 2,
		Outcome: ProbeOutcome{Kind: "text", Fragments: []ProbeValueFragment{
			{Value: "${result:00000000.text}"},
			{Value: "linux-bzl-recursive-make"},
			{Value: "${result:00000001.text}"},
		}},
	}
	derived, err := builder.Request("target", derivedRequest, opening, closing)
	if err != nil {
		t.Fatal(err)
	}
	result := func(reference ProbeReference, request ProbeRequest, value string) ProbeResult {
		return ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: reference.NodeID, RequestID: reference.RequestID,
			Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: value,
			Steps: []ProbeStepResult{{Name: request.Steps[0].Name, Status: "success", ExitCode: 0, Stdout: value}},
		}
	}
	oracle := &ProbeResultOracle{
		toolsets: map[string]string{"target": bootstrapTestIdentity},
		results: map[string]ProbeResult{
			opening.NodeID: result(opening, openingRequest, "\x05"),
			closing.NodeID: result(closing, closingRequest, "\x06"),
		},
	}
	evaluator := &LinuxProbeEvaluator{oracle: oracle, symbolRegistry: newLinuxProbeSymbolRegistry()}
	_, err = evaluator.readText(derived, derivedRequest, opening, closing)
	if err == nil || !strings.Contains(err.Error(), "reserved recursive Make provenance byte") {
		t.Fatalf("derived readText() error = %v, want reconstructed-token rejection", err)
	}
}

func TestReplayPureDependencyWordUsesDeclaredResultOrdinal(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	leafRequest := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "leaf", Tool: "cc"}},
		Outcome: ProbeOutcome{Kind: "text", Step: "leaf", Stream: "stdout"},
	}
	first, err := builder.Request("target", leafRequest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder.Request("target", ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "other-leaf", Tool: "cc"}},
		Outcome: ProbeOutcome{Kind: "text", Step: "other-leaf", Stream: "stdout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	derivedRequest := ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: 2,
		Outcome:    ProbeOutcome{Kind: "text", Result: "00000001", Word: 2},
	}
	derived, err := builder.Request("target", derivedRequest, first, second)
	if err != nil {
		t.Fatal(err)
	}
	oracle := &ProbeResultOracle{
		toolsets: map[string]string{"target": bootstrapTestIdentity},
		results: map[string]ProbeResult{
			first.NodeID: {
				Schema: LinuxProbeResultSchema, NodeID: first.NodeID, RequestID: first.RequestID,
				Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "first value",
				Steps: []ProbeStepResult{{Name: "leaf", Status: "success", ExitCode: 0, Stdout: "first value"}},
			},
			second.NodeID: {
				Schema: LinuxProbeResultSchema, NodeID: second.NodeID, RequestID: second.RequestID,
				Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "second selected-value tail",
				Steps: []ProbeStepResult{{Name: "other-leaf", Status: "success", ExitCode: 0, Stdout: "second selected-value tail"}},
			},
		},
	}
	evaluator := &LinuxProbeEvaluator{oracle: oracle, symbolRegistry: newLinuxProbeSymbolRegistry()}
	got, err := evaluator.readText(derived, derivedRequest, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if got != "selected-value" {
		t.Fatalf("derived dependency word = %q, want selected-value", got)
	}
	if recorded, err := oracle.Result(derived); err != nil || recorded.Text != got {
		t.Fatalf("recorded derived result = %#v, %v; want text %q", recorded, err, got)
	}
}

func TestReplayPureBooleanRejectsExistingDisagreement(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	leafRequest := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "leaf", Tool: "cc"}},
		Outcome: ProbeOutcome{Kind: "text", Step: "leaf", Stream: "stdout"},
	}
	leaf, err := builder.Request("target", leafRequest)
	if err != nil {
		t.Fatal(err)
	}
	predicate := ProbePredicate{Operator: "result-text-equals", Result: "00000000", Value: "selected"}
	derivedRequest := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: 1,
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &predicate},
	}
	derived, err := builder.Request("target", derivedRequest, leaf)
	if err != nil {
		t.Fatal(err)
	}
	wrong := false
	oracle := &ProbeResultOracle{
		toolsets: map[string]string{"target": bootstrapTestIdentity},
		results: map[string]ProbeResult{
			leaf.NodeID: {
				Schema: LinuxProbeResultSchema, NodeID: leaf.NodeID, RequestID: leaf.RequestID,
				Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "selected",
				Steps: []ProbeStepResult{{Name: "leaf", Status: "success", ExitCode: 0, Stdout: "selected"}},
			},
			derived.NodeID: {
				Schema: LinuxProbeResultSchema, NodeID: derived.NodeID, RequestID: derived.RequestID,
				Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &wrong,
			},
		},
	}
	evaluator := &LinuxProbeEvaluator{oracle: oracle, symbolRegistry: newLinuxProbeSymbolRegistry()}
	if _, err := evaluator.readBoolean(derived, derivedRequest, leaf); err == nil || !strings.Contains(err.Error(), "disagrees with its exact dependency reduction") {
		t.Fatalf("derived boolean disagreement error = %v", err)
	}
}

func (l *countingProbeResultLookup) Result(reference ProbeReference) (ProbeResult, error) {
	l.results++
	return l.delegate.Result(reference)
}

func testRustSourceRoot() string {
	return "external/rust-src/library"
}

func testSymbolicProbeEvaluator(t *testing.T, discovery ProbeDiscovery, oracle ProbeResultLookup) (*LinuxProbeEvaluator, *LinuxCompilerFacts) {
	t.Helper()
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	rustSourceRoot := testRustSourceRoot()
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: fixture.scope, Architecture: "arm64", SourceArchitecture: "arm64", SourceRoot: "/src", Facts: facts,
		Tools: map[string]string{
			"cc": "/configured/clang", "ld": "/configured/ld.lld", "ar": "/configured/llvm-ar",
			"nm": "/configured/llvm-nm", "objcopy": "/configured/llvm-objcopy", "pahole": "/configured/pahole",
			"rustc": "/configured/rustc", "bindgen": "/configured/bindgen",
			"python3": "/configured/python3",
		},
		ScriptEnvironment: map[string]string{
			"BINDGEN": "/configured/bindgen", "CC": "/configured/clang", "KRUSTFLAGS": "",
			"RUSTC": "/configured/rustc", "RUST_LIB_SRC": rustSourceRoot,
		},
		Discovery: discovery, Oracle: oracle,
		RustSourceRoot: rustSourceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluator, facts
}

type evaluatorExercise struct {
	ccOption, bitOption, assembler, linker, cSource string
}

func exerciseSymbolicProbeEvaluator(t *testing.T, evaluator *LinuxProbeEvaluator) evaluatorExercise {
	t.Helper()
	run := func(command string) string {
		t.Helper()
		value, err := evaluator.Shell(context.Background(), command)
		if err != nil {
			t.Fatalf("Shell(%q): %v", command, err)
		}
		return value
	}
	ccOption := run(`{ trap "rm -rf .tmp_1" EXIT; mkdir .tmp_1; /configured/clang -Werror -fintegrated-as -fbrand-new -c -x c /dev/null -o .tmp_1/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	bitOption := run(`{ /configured/clang -Werror -m64 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "-m64" || echo ""`)
	assembler := run(`{ printf "%b\n" ".arch_extension lse" | /configured/clang -fintegrated-as ` + bitOption + ` -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	linker := run(`{ /configured/ld.lld --brand-new-linker-option -v; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	cSource := run(`{ echo 'int feature(void) { return 1; }' | /configured/clang -x c - -S -o /dev/null -Werror; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	return evaluatorExercise{ccOption: ccOption, bitOption: bitOption, assembler: assembler, linker: linker, cSource: cSource}
}

func TestLinuxProbeEvaluatorOwnsLinux618SanitizerIgnorelistCCOption(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	command := `{ /configured/clang -Werror -fsanitize-ignorelist=/dev/null -c -x c /dev/null -o .tmp_probe/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	value, err := evaluator.Shell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("Linux 6.18 sanitizer ignorelist cc-option = %q, want symbolic probe", value)
	}

	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("Linux 6.18 sanitizer ignorelist plan nodes = %#v, want one", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	step := request.Steps[0]
	wantArguments := []string{
		"-Werror", "-fsanitize-ignorelist=/dev/null",
		"-x", "c", "-c", "-o", "${scratch:output}", "-",
	}
	if !slices.Equal(step.Arguments, wantArguments) {
		t.Fatalf("Linux 6.18 sanitizer ignorelist argv = %q, want %q", step.Arguments, wantArguments)
	}
	if candidate := step.Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
		!slices.Equal(candidate.Base, []int{0, 1}) || len(candidate.Conditional) != 0 {
		t.Fatalf("Linux 6.18 sanitizer ignorelist candidate ownership = %#v, want cc base [0 1]", candidate)
	}
}

func TestLinuxProbeEvaluatorMemoizesSuccessfulShellCommandsWithinExactScope(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	discovery := &countingProbeDiscovery{delegate: builder}
	fixtures := linuxCompilerBootstrapFixtures(t)
	newEvaluator := func(fixture bootstrapFixture) *LinuxProbeEvaluator {
		facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]string{
			"ar": "/configured/ar", "cc": "/configured/cc", "ld": "/configured/ld",
			"nm": "/configured/nm", "objcopy": "/configured/objcopy",
		}
		evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
			Scope: fixture.scope, Architecture: "x86", SourceArchitecture: "x86",
			Facts: facts, Tools: tools, ScriptEnvironment: map[string]string{"CC": tools["cc"]},
			Discovery: discovery,
		})
		if err != nil {
			t.Fatal(err)
		}
		return evaluator
	}
	target := newEvaluator(fixtures[1])
	host := newEvaluator(fixtures[0])
	command := `{ /configured/cc -Werror -fshared-cache -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	first, err := target.Shell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := target.Shell(context.Background(), "  "+command+"  ")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || discovery.requests != 1 {
		t.Fatalf("identical target Shell calls = %q/%q with %d requests, want one request", first, second, discovery.requests)
	}
	distinct := strings.Replace(command, "-fshared-cache", "-fdistinct-cache", 1)
	if _, err := target.Shell(context.Background(), distinct); err != nil {
		t.Fatal(err)
	}
	if discovery.requests != 2 {
		t.Fatalf("distinct target Shell command reused cache: requests=%d, want 2", discovery.requests)
	}
	hostValue, err := host.Shell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.requests != 3 || hostValue == first {
		t.Fatalf("host-scoped Shell call = %q with %d requests, want a distinct scoped request", hostValue, discovery.requests)
	}

	kbuildCommand := `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (/configured/cc -Werror -fkbuild-cache -c -x c /dev/null -o "$TMP") >/dev/null 2>&1; then echo "-fkbuild-cache"; else echo ""; fi`
	kbuildFirst, err := target.KbuildShell(context.Background(), kbuildCommand)
	if err != nil {
		t.Fatal(err)
	}
	kbuildSecond, err := target.KbuildShell(context.Background(), kbuildCommand)
	if err != nil {
		t.Fatal(err)
	}
	if kbuildFirst != kbuildSecond || discovery.requests != 4 {
		t.Fatalf("identical KbuildShell calls = %q/%q with %d requests, want one additional request", kbuildFirst, kbuildSecond, discovery.requests)
	}
}

func TestKbuildPreprocessorGrepIsAnIdentityBoundCompilerProbe(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	command := `echo '\#include <libelf.h>' | /configured/clang -Werror -I __LINUX_BZL_SOURCE_TREE__/tools/include -I__LINUX_BZL_HOST_DEPS__/external/elfutils+/libelf -I__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/include -x c -E - 2>/dev/null | grep elf_getshdr`
	value, err := evaluator.KbuildShell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(value, linuxProbeSymbolPrefix) {
		t.Fatalf("preprocessor grep = %q, want symbolic result", value)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("preprocessor grep nodes = %d, want one: %#v", len(plan.Nodes), plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if candidate := request.Steps[0].Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
		!slices.Equal(candidate.Base, []int{0, 1, 2, 3}) || len(candidate.Conditional) != 0 {
		t.Fatalf("preprocessor candidate ownership = %#v", candidate)
	}
	if got, want := request.Steps[0].Stdin, "#include <libelf.h>\n"; got != want {
		t.Fatalf("preprocessor stdin = %q, want %q", got, want)
	}
	for _, argument := range []string{
		"-I${source_root:linux}/tools/include",
		"-I${source_root:host_deps}/external/elfutils+/libelf",
		"-I__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/include",
		"-x", "c", "-E", "-",
	} {
		if !slices.Contains(request.Steps[0].Arguments, argument) {
			t.Fatalf("preprocessor argv = %q, missing %q", request.Steps[0].Arguments, argument)
		}
	}
	if got, want := strings.Join(request.SourceRoots, " "), "host_deps linux"; got != want {
		t.Fatalf("preprocessor roots = %q, want %q", got, want)
	}
	if request.Outcome.Predicate == nil || request.Outcome.Predicate.Operator != "all" || len(request.Outcome.Predicate.Operands) != 2 ||
		request.Outcome.Predicate.Operands[1].Operator != "stream-contains" || request.Outcome.Predicate.Operands[1].Value != "elf_getshdr" {
		t.Fatalf("preprocessor reduction = %#v, want exit-zero and source-selected grep", request.Outcome.Predicate)
	}
}

func TestCompilerMacroGrepStatusIsSourceOwnedAndReplaysShellStatus(t *testing.T) {
	command := `/configured/clang -D__LINUX_BZL_DYNAMIC_QUERY__=1 -dM -E -x c /dev/null | grep -Fq "__clang__"; echo $?`
	discoveryBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	value, err := discovery.Shell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("compiler macro status = %q, want symbolic shell status", value)
	}
	plan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("compiler macro status nodes = %d, want one: %#v", len(plan.Nodes), plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if request.InputCount != 0 || len(request.Steps) != 1 {
		t.Fatalf("compiler macro status request = %#v", request)
	}
	step := request.Steps[0]
	if step.Name != "macro-preprocess" || step.Tool != "cc" ||
		!slices.Equal(step.Arguments, []string{"-D__LINUX_BZL_DYNAMIC_QUERY__=1", "-dM", "-E", "-x", "c", "/dev/null"}) ||
		len(step.ConditionalArguments) != 0 {
		t.Fatalf("compiler macro status step = %#v", step)
	}
	if candidate := step.Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
		!slices.Equal(candidate.Base, []int{0}) || len(candidate.Conditional) != 0 {
		t.Fatalf("compiler macro candidate ownership = %#v", candidate)
	}
	wantPredicate := ProbePredicate{
		Operator: "stream-contains", Step: "macro-preprocess", Stream: "stdout", Value: "__clang__",
	}
	predicate := request.Outcome.Predicate
	if request.Outcome.Kind != "boolean" || predicate == nil ||
		predicate.Operator != wantPredicate.Operator || predicate.Step != wantPredicate.Step ||
		predicate.Stream != wantPredicate.Stream || predicate.Value != wantPredicate.Value || len(predicate.Operands) != 0 {
		t.Fatalf("compiler macro status outcome = %#v, want grep's stdout reduction %#v", request.Outcome, wantPredicate)
	}

	for _, test := range []struct {
		name       string
		grepMatch  bool
		wantStatus string
	}{
		{name: "grep success", grepMatch: true, wantStatus: "0"},
		{name: "grep failure", grepMatch: false, wantStatus: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout := "#define __GNUC__ 15\n"
			if test.grepMatch {
				stdout += "#define __clang__ 1\n"
			}
			result := ProbeResult{
				Schema: LinuxProbeResultSchema, NodeID: plan.Nodes[0].ID, RequestID: plan.Nodes[0].RequestID,
				Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &test.grepMatch,
				Steps: []ProbeStepResult{{Name: "macro-preprocess", Status: "success", ExitCode: 0, Stdout: stdout}},
			}
			replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
			if err != nil {
				t.Fatal(err)
			}
			replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, probeResultMap{plan.Nodes[0].ID: result})
			replayed, err := replay.Shell(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			got, err := replay.ResolveSymbolic(replayed)
			if err != nil || got != test.wantStatus {
				t.Fatalf("replayed compiler macro status = %q, %v; want %q", got, err, test.wantStatus)
			}
			replayPlan, err := replayBuilder.Plan(replay.References()...)
			if err != nil {
				t.Fatal(err)
			}
			if len(replayPlan.Nodes) != 1 || replayPlan.Nodes[0].ID != plan.Nodes[0].ID || replayPlan.Nodes[0].RequestID != plan.Nodes[0].RequestID {
				t.Fatalf("compiler macro replay plan = %#v, want %#v", replayPlan.Nodes, plan.Nodes)
			}
		})
	}
}

func TestCompilerMacroGrepStatusRejectsMalformedOwnedShapes(t *testing.T) {
	for name, command := range map[string]string{
		"missing macro dump": `/configured/clang -E -x c /dev/null | grep -Fq "__clang__"; echo $?`,
		"wrong language":     `/configured/clang -dM -E -x c++ /dev/null | grep -Fq "__clang__"; echo $?`,
		"unsafe pattern":     `/configured/clang -dM -E -x c /dev/null | grep -Fq "__clang__.*"; echo $?`,
		"regular grep":       `/configured/clang -dM -E -x c /dev/null | grep -q "__clang__"; echo $?`,
		"extra command":      `/configured/clang -dM -E -x c /dev/null | grep -Fq "__clang__"; echo $?; id`,
		"wrong status":       `/configured/clang -dM -E -x c /dev/null | grep -Fq "__clang__"; echo 0`,
	} {
		t.Run(name, func(t *testing.T) {
			builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
			if err != nil {
				t.Fatal(err)
			}
			evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
			_, err = evaluator.Shell(context.Background(), command)
			if err == nil || IsLinuxProbeUnsupportedCommand(err) {
				t.Fatalf("malformed compiler macro status error = %v, want owned rejection", err)
			}
			if len(evaluator.References()) != 0 {
				t.Fatalf("malformed compiler macro status emitted references: %#v", evaluator.References())
			}
		})
	}
}

func TestCompilerPreprocessorTailOutputPreservesSourceArgvAndDependencies(t *testing.T) {
	discoveryBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	flagCommand := `{ /configured/clang -Werror -m64 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "-m64" || echo ""`
	flag := shellProbeValue(t, discovery, flagCommand)
	command := `echo __SOURCE_SELECTED_WIDTH__ | /configured/clang ` + flag + ` -E -x c - | tail -n 1`
	value := shellProbeValue(t, discovery, command)
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("preprocessor tail output = %q, want symbolic text", value)
	}
	plan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || !slices.Equal(plan.Nodes[1].Inputs, []string{plan.Nodes[0].ID}) {
		t.Fatalf("preprocessor tail dependency plan = %#v", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[1].RequestID]
	if len(request.Steps) != 1 || request.Steps[0].Tool != "cc" || request.Steps[0].Stdin != "__SOURCE_SELECTED_WIDTH__\n" ||
		!slices.Equal(request.Steps[0].Arguments, []string{"-E", "-x", "c", "-"}) {
		t.Fatalf("preprocessor tail request step = %#v", request.Steps)
	}
	conditional := request.Steps[0].ConditionalArguments
	if len(conditional) != 1 || conditional[0].Before != 0 || conditional[0].When.Operator != "result-true" ||
		conditional[0].When.Result != "00000000" || !slices.Equal(conditional[0].Arguments, []string{"-m64"}) {
		t.Fatalf("preprocessor tail conditional argv = %#v", conditional)
	}
	if candidate := request.Steps[0].Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
		len(candidate.Base) != 0 || !slices.Equal(candidate.Conditional, []int{0}) {
		t.Fatalf("preprocessor tail candidate ownership = %#v", candidate)
	}
	if request.Outcome.Kind != "text" || request.Outcome.Step != "preprocess" || request.Outcome.Stream != "stdout" ||
		!request.Outcome.TrimSpace || !request.Outcome.LastLine || request.Outcome.FirstLine {
		t.Fatalf("preprocessor tail outcome = %#v", request.Outcome)
	}

	flagSupported := true
	results := probeResultMap{
		plan.Nodes[0].ID: {
			Schema: LinuxProbeResultSchema, NodeID: plan.Nodes[0].ID, RequestID: plan.Nodes[0].RequestID,
			Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &flagSupported,
			Steps: []ProbeStepResult{{Name: "probe", Status: "success", ExitCode: 0}},
		},
		plan.Nodes[1].ID: {
			Schema: LinuxProbeResultSchema, NodeID: plan.Nodes[1].ID, RequestID: plan.Nodes[1].RequestID,
			Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "1",
			Steps: []ProbeStepResult{{
				Name: "preprocess", Status: "success", ExitCode: 0,
				Stdout: "# 0 \"<stdin>\"\n# 0 \"<built-in>\"\n1\n",
			}},
		},
	}
	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	replayFlag := shellProbeValue(t, replay, flagCommand)
	replayValue := shellProbeValue(t, replay, `echo __SOURCE_SELECTED_WIDTH__ | /configured/clang `+replayFlag+` -E -x c - | tail -n 1`)
	if got, err := replay.ResolveSymbolic(replayValue); err != nil || got != "1" {
		t.Fatalf("preprocessor tail replay = %q, %v; want 1", got, err)
	}
	replayPlan, err := replayBuilder.Plan(replay.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayPlan.Nodes) != len(plan.Nodes) {
		t.Fatalf("preprocessor tail replay nodes = %#v, want %#v", replayPlan.Nodes, plan.Nodes)
	}
	for index := range plan.Nodes {
		if replayPlan.Nodes[index].ID != plan.Nodes[index].ID || replayPlan.Nodes[index].RequestID != plan.Nodes[index].RequestID {
			t.Fatalf("preprocessor tail replay node %d = %#v, want %#v", index, replayPlan.Nodes[index], plan.Nodes[index])
		}
	}
}

func TestCompilerPreprocessorTailOutputAcceptsSourceOrderingVariants(t *testing.T) {
	for name, test := range map[string]struct {
		command string
		argv    []string
		base    []int
	}{
		"linux ordering": {
			command: `echo __SOURCE_TOKEN__ | /configured/clang -m64 -E -x c - | tail -n 1`,
			argv:    []string{"-m64", "-E", "-x", "c", "-"},
			base:    []int{0},
		},
		"equivalent source ordering": {
			command: `echo "__SOURCE_TOKEN__" | /configured/clang -x c -E - -m64 | tail -n1`,
			argv:    []string{"-x", "c", "-E", "-", "-m64"},
			base:    []int{4},
		},
	} {
		t.Run(name, func(t *testing.T) {
			builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
			evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
			value := shellProbeValue(t, evaluator, test.command)
			if !linuxProbeSymbolPattern.MatchString(value) {
				t.Fatalf("preprocessor tail output = %q, want symbolic text", value)
			}
			plan, err := builder.Plan(evaluator.References()...)
			if err != nil {
				t.Fatal(err)
			}
			request := plan.Requests[plan.Nodes[0].RequestID]
			if !slices.Equal(request.Steps[0].Arguments, test.argv) {
				t.Fatalf("preprocessor tail argv = %q, want exact source argv %q", request.Steps[0].Arguments, test.argv)
			}
			if candidate := request.Steps[0].Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
				!slices.Equal(candidate.Base, test.base) || len(candidate.Conditional) != 0 {
				t.Fatalf("preprocessor tail candidate ownership = %#v, want base %v", candidate, test.base)
			}
		})
	}
}

func TestCompilerPreprocessorTailOutputRejectsMalformedOwnedShapes(t *testing.T) {
	for name, command := range map[string]string{
		"unsafe producer":    `echo __SOURCE_TOKEN__.* | /configured/clang -E -x c - | tail -n 1`,
		"missing preprocess": `echo __SOURCE_TOKEN__ | /configured/clang -x c - | tail -n 1`,
		"wrong language":     `echo __SOURCE_TOKEN__ | /configured/clang -E -x c++ - | tail -n 1`,
		"missing stdin":      `echo __SOURCE_TOKEN__ | /configured/clang -E -x c /dev/null | tail -n 1`,
		"wrong reducer":      `echo __SOURCE_TOKEN__ | /configured/clang -E -x c - | head -n 1`,
		"wrong line count":   `echo __SOURCE_TOKEN__ | /configured/clang -E -x c - | tail -n 2`,
		"extra command":      `echo __SOURCE_TOKEN__ | /configured/clang -E -x c - | tail -n 1; id`,
	} {
		t.Run(name, func(t *testing.T) {
			builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
			evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
			_, err := evaluator.Shell(context.Background(), command)
			if err == nil || IsLinuxProbeUnsupportedCommand(err) {
				t.Fatalf("malformed preprocessor tail error = %v, want owned rejection", err)
			}
			if len(evaluator.References()) != 0 {
				t.Fatalf("malformed preprocessor tail emitted references: %#v", evaluator.References())
			}
		})
	}
}

func TestLinuxProbeEvaluatorMemoizesValidatedSymbolResultsAcrossExpressions(t *testing.T) {
	discoveryBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	commands := []string{
		`{ /configured/clang -Werror -fcache-first -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`{ /configured/clang -Werror -fcache-second -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	}
	for _, command := range commands {
		if _, err := discovery.Shell(context.Background(), command); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	results := resultsForCapabilityPlan(plan, map[int]bool{0: true, 1: false}, bootstrapTestIdentity)
	lookup := &countingProbeResultLookup{delegate: results}
	replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, lookup)
	values := make([]string, len(commands))
	for index, command := range commands {
		values[index], err = replay.Shell(context.Background(), command)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, err := replay.ResolveSymbolic(values[0]); err != nil || got != "y" {
		t.Fatalf("first symbolic result = %q, %v; want y", got, err)
	}
	if got, err := replay.ResolveSymbolic("prefix=" + values[0]); err != nil || got != "prefix=y" {
		t.Fatalf("embedded repeated symbolic result = %q, %v; want prefix=y", got, err)
	}
	if lookup.results != 1 {
		t.Fatalf("same symbolic atom used in distinct expressions read %d oracle results, want 1", lookup.results)
	}
	if got, err := replay.ResolveSymbolic(values[1]); err != nil || got != "n" {
		t.Fatalf("distinct symbolic result = %q, %v; want n", got, err)
	}
	if lookup.results != 2 {
		t.Fatalf("distinct symbolic atoms read %d oracle results, want 2", lookup.results)
	}
}

func TestLowerSymbolicCandidateArgumentsTracksFiniteGroups(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{symbols: map[string]linuxProbeSymbol{}}
	arguments := []string{"-Wall"}
	const symbolCount = 160
	for index := 0; index < symbolCount; index++ {
		token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", index+1)
		nodeID := fmt.Sprintf("%064x", index/4+1)
		evaluator.symbols[token] = linuxProbeSymbol{
			kind: "boolean",
			reference: ProbeReference{
				NodeID: nodeID, RequestID: nodeID, Scope: "target", Kind: "boolean",
			},
			trueText: fmt.Sprintf("-Wsymbolic-%d", index),
		}
		arguments = append(arguments, token)
	}
	base, conditional, argumentFragments, candidate, dependencies, err := evaluator.lowerSymbolicCandidateArguments(
		arguments, probeCandidateArgumentMask(len(arguments)), ProbeCandidatePolicyCC,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(argumentFragments) != 0 {
		t.Fatalf("finite symbolic arguments unexpectedly used text fragments: %#v", argumentFragments)
	}
	if !slices.Equal(base, []string{"-Wall"}) || len(conditional) != symbolCount || len(dependencies) != symbolCount/4 {
		t.Fatalf("lowered symbolic arguments: base=%q conditional=%d dependencies=%d", base, len(conditional), len(dependencies))
	}
	if candidate == nil || candidate.Policy != ProbeCandidatePolicyCC || !slices.Equal(candidate.Base, []int{0}) || len(candidate.Conditional) != symbolCount {
		t.Fatalf("candidate ownership = %#v, want base 0 and %d conditional groups", candidate, symbolCount)
	}
	for index, group := range conditional {
		wantResult := fmt.Sprintf("%08d", index/4)
		wantArguments := []string{fmt.Sprintf("-Wsymbolic-%d", index)}
		if group.Before != 1 || group.When.Operator != "result-true" || group.When.Result != wantResult || !slices.Equal(group.Arguments, wantArguments) {
			t.Fatalf("conditional argument %d = %#v, want before=1 result-true(%s) arguments=%q", index, group, wantResult, wantArguments)
		}
		if candidate.Conditional[index] != index {
			t.Fatalf("candidate conditional ownership %d = %d, want %d", index, candidate.Conditional[index], index)
		}
	}
	for index, dependency := range dependencies {
		if want := fmt.Sprintf("%064x", index+1); dependency.NodeID != want {
			t.Fatalf("dependency %d = %q, want %q", index, dependency.NodeID, want)
		}
	}
}

func TestLowerSymbolicProcessInputsResolvesNestedBranchAtoms(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	probe := func(argument string) linuxProbeTruth {
		t.Helper()
		truth, err := evaluator.requestTruth(ProbeRequest{
			Schema: LinuxProbeRequestSchema,
			Steps:  []ProbeStep{{Name: "probe", Tool: "cc", Arguments: []string{argument}}},
			Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
				Operator: "exit-zero", Step: "probe",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return truth
	}
	inner, err := evaluator.renderTruth(probe("-finner"), "old", "older")
	if err != nil {
		t.Fatal(err)
	}
	outer, err := evaluator.renderTruth(probe("-fouter"), "-DVALUE="+inner, "-DVALUE=new")
	if err != nil {
		t.Fatal(err)
	}
	arguments, conditional, argumentFragments, stdin, stdinFragments, dependencies, err := evaluator.lowerSymbolicProcessInputs(
		[]string{outer}, "source="+outer+"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(argumentFragments) != 0 {
		t.Fatalf("finite process arguments unexpectedly used text fragments: %#v", argumentFragments)
	}
	if len(arguments) != 0 || stdin != "" || len(conditional) != 4 || len(stdinFragments) == 0 || len(dependencies) != 2 {
		t.Fatalf("nested process lowering: argv=%q conditional=%#v stdin=%q fragments=%#v dependencies=%#v", arguments, conditional, stdin, stdinFragments, dependencies)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "probe", Tool: "cc", Arguments: arguments,
			ConditionalArguments: conditional, StdinFragments: stdinFragments,
		}},
		Outcome: ProbeOutcome{Kind: "text", Step: "probe", Stream: "stdout"},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{inner, outer, linuxProbeSymbolPrefix} {
		if strings.Contains(string(data), token) {
			t.Fatalf("nested symbolic atom %q leaked into executable request: %s", token, data)
		}
	}
}

func TestLowerSymbolicArgumentsResolvesNestedCompilerOptionFallback(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	probe := func(argument string) linuxProbeTruth {
		t.Helper()
		truth, err := evaluator.requestTruth(ProbeRequest{
			Schema: LinuxProbeRequestSchema,
			Steps:  []ProbeStep{{Name: "probe", Tool: "cc", Arguments: []string{argument}}},
			Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
				Operator: "exit-zero", Step: "probe",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return truth
	}
	inner, err := evaluator.renderTruth(probe("-malignment-traps"), "-malignment-traps", "")
	if err != nil {
		t.Fatal(err)
	}
	outer, err := evaluator.renderTruth(probe("-mshort-load-bytes"), "-mshort-load-bytes", inner)
	if err != nil {
		t.Fatal(err)
	}
	candidate := []string{outer, "-fzero-init-padding-bits=all"}
	arguments, conditional, fragments, dependencies, err := evaluator.lowerSymbolicArguments(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(arguments, []string{"-fzero-init-padding-bits=all"}) || len(conditional) != 3 || len(fragments) != 0 || len(dependencies) != 2 {
		t.Fatalf("nested fallback lowering: arguments=%q conditional=%#v fragments=%#v dependencies=%#v", arguments, conditional, fragments, dependencies)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Steps: []ProbeStep{{
			Name: "probe", Tool: "cc", Arguments: arguments, ConditionalArguments: conditional,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "probe"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("nested compiler-option fallback leaked a symbolic token into executable request: %s", data)
	}
}

func TestLinuxProbeEvaluatorLowersCompilerMetadataQueriesToActions(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, facts := testSymbolicProbeEvaluator(t, builder, nil)
	commands := []string{
		"/src/scripts/cc-version.sh /configured/clang",
		"/src/scripts/as-version.sh /configured/clang -fintegrated-as",
		"/src/scripts/ld-version.sh /configured/ld.lld",
		"/configured/clang --version",
		"/configured/clang -print-file-name=vendor-sdk",
		"/src/scripts/pahole-version.sh /configured/pahole",
		"/src/scripts/rustc-version.sh rustc",
		"/src/scripts/rustc-llvm-version.sh rustc",
		"/configured/rustc --version 2>/dev/null",
	}
	for _, command := range commands {
		got, runErr := evaluator.Shell(context.Background(), command)
		if runErr != nil || !linuxProbeSymbolPattern.MatchString(got) {
			t.Errorf("Shell(%q) = %q, %v; want symbolic action", command, got, runErr)
		}
	}
	headed := "LC_ALL=C " + KbuildActionRoleToken("target", "cc") + " --version 2>/dev/null | head -n 1"
	if got, runErr := evaluator.Shell(context.Background(), headed); runErr != nil || got != facts.VersionText() {
		t.Fatalf("Shell(%q) = %q, %v; want bootstrap version fact %q", headed, got, runErr, facts.VersionText())
	}
	metadataReferences := len(evaluator.References())
	if metadataReferences != len(commands) {
		t.Fatalf("metadata queries emitted %d references, want %d", metadataReferences, len(commands))
	}
	unqualifiedHeaded := KbuildActionRoleToken("target", "cc") + " --version 2>/dev/null | head -n 1"
	if got, runErr := evaluator.Shell(context.Background(), unqualifiedHeaded); runErr != nil || !linuxProbeSymbolPattern.MatchString(got) {
		t.Fatalf("Shell(%q) = %q, %v; want a distinct source-requested version action", unqualifiedHeaded, got, runErr)
	}
	if got := len(evaluator.References()); got != metadataReferences+1 {
		t.Fatalf("unqualified compiler version emitted %d total references, want %d", got, metadataReferences+1)
	}
	metadataReferences = len(evaluator.References())
	for _, command := range []string{
		`{ command -v /configured/clang; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`{ command -v /configured/ld.lld; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`{ command -v ` + KbuildActionRoleToken(KbuildActionRoleAutoScope, "cc") + `; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	} {
		if got, runErr := evaluator.Shell(context.Background(), command); runErr != nil || got != "y" {
			t.Errorf("Shell(%q) = %q, %v; want y", command, got, runErr)
		}
	}
	if len(evaluator.References()) != metadataReferences {
		t.Fatal("configured command lookup unexpectedly emitted a capability action")
	}
	if _, err := evaluator.Shell(context.Background(), "/other/clang --version"); err == nil {
		t.Fatal("evaluator accepted an unselected compiler with the same basename")
	}
}

func TestLinuxProbeEvaluatorDoesNotSelectRustCompilerByBareName(t *testing.T) {
	for _, withRust := range []bool{false, true} {
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
		if err != nil {
			t.Fatal(err)
		}
		evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, withRust)
		for _, command := range []string{"rustc --version", "rustc --version 2>/dev/null"} {
			got, runErr := evaluator.Shell(context.Background(), command)
			if runErr != nil || got != "" {
				t.Errorf("withRust=%t Shell(%q) = %q, %v; want bare command unavailable", withRust, command, got, runErr)
			}
		}
		if len(evaluator.References()) != 0 {
			t.Fatal("bare rustc version queries emitted probe requests")
		}
		if withRust {
			command := KbuildActionRoleToken(KbuildActionRoleAutoScope, "rustc") + " --version 2>/dev/null"
			got, runErr := evaluator.Shell(context.Background(), command)
			if runErr != nil || !linuxProbeSymbolPattern.MatchString(got) {
				t.Errorf("Shell(%q) = %q, %v; want configured Rust capability", command, got, runErr)
			}
		}
	}
}

func TestLinuxProbeEvaluatorDoesNotInferConfiguredToolFromExecutableName(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 0, "x86", builder, nil, false)
	evaluator.tools["cc"] = "/configured/aarch64-linux-gnu-gcc"
	for command, want := range map[string]string{
		"command -v -- aarch64-linux-gnu-gcc 2>/dev/null":                                          "",
		"command -v -- m68k-linux-gnu-gcc 2>/dev/null":                                             "",
		"command -v -- /configured/aarch64-linux-gnu-gcc 2>/dev/null":                              "/configured/aarch64-linux-gnu-gcc",
		"command -v -- " + KbuildActionRoleToken(KbuildActionRoleAutoScope, "cc") + " 2>/dev/null": KbuildActionRoleToken(KbuildActionRoleAutoScope, "cc"),
	} {
		got, runErr := evaluator.Shell(context.Background(), command)
		if runErr != nil || got != want {
			t.Errorf("Shell(%q) = %q, %v; want %q", command, got, runErr, want)
		}
	}
	if len(evaluator.References()) != 0 {
		t.Fatal("configured command lookup unexpectedly emitted a capability action")
	}
	if _, err := evaluator.Shell(context.Background(), "command -v -- aarch64-linux-gnu-gcc unexpected"); err == nil {
		t.Fatal("malformed command -v -- query was accepted")
	}
}

func TestLinuxProbeEvaluatorSelectsCompilerThroughExactShellEnvironmentToken(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	evaluator.scriptEnvironment["SOURCE_SELECTED_DRIVER"] = KbuildActionRoleToken(evaluator.scope, "cc")
	evaluator.scriptEnvironment["OTHER_DRIVER"] = "/configured/other-clang"

	for _, tool := range []string{"$CC", `${CC}`, `"$CC"`, `${SOURCE_SELECTED_DRIVER}`, `'/configured/clang'`, `"/configured/clang"`} {
		command := `{ echo 'int feature(void) { return 1; }' | ` + tool + ` -x c - -c -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
		value, shellErr := evaluator.Shell(context.Background(), command)
		if shellErr != nil || !linuxProbeSymbolPattern.MatchString(value) {
			t.Errorf("Shell(%q) = %q, %v; want selected compiler capability", command, value, shellErr)
		}
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("equivalent environment-selected compiler probes produced %d nodes, want 1", len(plan.Nodes))
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if got := request.ToolRoles(); !slices.Equal(got, []string{"cc"}) {
		t.Fatalf("environment-selected compiler roles = %q, want cc", got)
	}

	for _, tool := range []string{
		"'$CC'", `$MISSING_DRIVER`, `${CC:-fallback}`, `$CC-suffix`, `$OTHER_DRIVER`,
		`'/configured/clang"`, `"/configured/clang'`, `"/configured/clang`, `/configured/clang'`,
	} {
		command := `{ echo 'int feature(void) { return 1; }' | ` + tool + ` -x c - -c -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
		if _, shellErr := evaluator.Shell(context.Background(), command); shellErr == nil {
			t.Errorf("Shell(%q) selected a compiler through a non-exact environment token", command)
		}
	}
}

func TestLinuxProbeEvaluatorDirectToolVersionQueriesAreManifestDriven(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	evaluator.scriptEnvironment["OPTIONAL_VERSION_TOOL"] = "future-optional-tool"
	selected := KbuildActionRoleToken(evaluator.scope, "objcopy") + ` --version source-compatibility-next 2>/dev/null`
	value, err := evaluator.Shell(context.Background(), selected)
	if err != nil || !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("selected direct version query = %q, %v; want symbolic action", value, err)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("selected direct version query nodes = %d, want 1", len(plan.Nodes))
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if got, want := request.ToolRoles(), []string{"objcopy"}; !slices.Equal(got, want) {
		t.Fatalf("selected direct version query roles = %q, want %q", got, want)
	}
	if got, want := request.Steps[0].Arguments, []string{"--version", "source-compatibility-next"}; !slices.Equal(got, want) {
		t.Fatalf("selected direct version query argv = %q, want %q", got, want)
	}

	// Linux currently appends this compatibility argument to its optional
	// bindings-generator query. The name and argv are source data: any safe bare
	// command absent from the selected manifest is unavailable in the hermetic
	// probe PATH and therefore produces no stdout.
	for _, command := range []string{
		`bindgen --version workaround-for-0.69.0 2>/dev/null`,
		`future-optional-tool --version`,
		`'future-optional-tool' --version`,
		`"future-optional-tool" --version`,
		`$OPTIONAL_VERSION_TOOL --version compatibility-name`,
	} {
		if got, shellErr := evaluator.Shell(context.Background(), command); shellErr != nil || got != "" {
			t.Errorf("unconfigured direct version Shell(%q) = %q, %v; want empty output", command, got, shellErr)
		}
	}
	if got := len(evaluator.References()); got != 1 {
		t.Fatalf("unconfigured direct version queries emitted capability actions: references=%d", got)
	}

	for _, command := range []string{
		`../bindgen --version workaround-for-0.69.0 2>/dev/null`,
		`bindgen --version @response 2>/dev/null`,
		`bindgen --version --output=escape 2>/dev/null`,
		`bindgen --version $(command) 2>/dev/null`,
		`bindgen --version safe 2>/dev/null | command`,
		KbuildActionRoleToken("target", "missing") + ` --version 2>/dev/null`,
	} {
		if _, shellErr := evaluator.Shell(context.Background(), command); shellErr == nil || IsLinuxProbeUnsupportedCommand(shellErr) {
			t.Errorf("unsafe direct version Shell(%q) error = %v; want owned rejection", command, shellErr)
		}
	}
}

func TestLinuxProbeEvaluatorDiscoversCanonicalCapabilityDAG(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	values := exerciseSymbolicProbeEvaluator(t, evaluator)
	for name, value := range map[string]string{
		"cc option": values.ccOption, "bit option": values.bitOption, "assembler": values.assembler,
		"linker": values.linker, "C source": values.cSource,
	} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Errorf("%s discovery value = %q, want symbolic atom", name, value)
		}
		if got, resolveErr := evaluator.ResolveSymbolic(value); resolveErr != nil || got != value {
			t.Errorf("discovery ResolveSymbolic(%s) = %q, %v", name, got, resolveErr)
		}
	}
	references := evaluator.References()
	if len(references) != 5 {
		t.Fatalf("capability references = %d, want 5", len(references))
	}
	plan, err := builder.Plan(references...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 5 || len(plan.Requests) != 5 {
		t.Fatalf("capability plan = %#v", plan)
	}
	assemblerNode := plan.Nodes[2]
	if got, want := assemblerNode.Inputs, []string{plan.Nodes[1].ID}; !slices.Equal(got, want) {
		t.Fatalf("assembler dependencies = %v, want %v", got, want)
	}
	assemblerRequest := plan.Requests[assemblerNode.RequestID]
	if assemblerRequest.InputCount != 1 || len(assemblerRequest.Steps) != 1 || len(assemblerRequest.Steps[0].ConditionalArguments) != 1 {
		t.Fatalf("assembler request did not retain conditional -m64: %#v", assemblerRequest)
	}
	conditional := assemblerRequest.Steps[0].ConditionalArguments[0]
	if conditional.When.Operator != "result-true" || conditional.When.Result != "00000000" || !slices.Equal(conditional.Arguments, []string{"-m64"}) {
		t.Fatalf("assembler conditional arguments = %#v", conditional)
	}
	linkerRequest := plan.Requests[plan.Nodes[3].RequestID]
	if got, want := linkerRequest.Steps[0].Arguments, []string{"--brand-new-linker-option", "-v"}; !slices.Equal(got, want) {
		t.Fatalf("linker arguments = %v, want exact trailing-version argv %v", got, want)
	}
	wantCandidates := []ProbeCandidateArguments{
		{Policy: ProbeCandidatePolicyCC, Base: []int{0, 1, 2}},
		{Policy: ProbeCandidatePolicyCC, Base: []int{0, 1}},
		{Policy: ProbeCandidatePolicyCC, Base: []int{0, 1}, Conditional: []int{0}},
		{Policy: ProbeCandidatePolicyLD, Base: []int{0}},
		{Policy: ProbeCandidatePolicyCC, Base: []int{0}},
	}
	for index, want := range wantCandidates {
		candidate := plan.Requests[plan.Nodes[index].RequestID].Steps[0].Candidate
		if candidate == nil || candidate.Policy != want.Policy || !slices.Equal(candidate.Base, want.Base) ||
			!slices.Equal(candidate.Conditional, want.Conditional) {
			t.Errorf("capability candidate %d = %#v, want %#v", index, candidate, want)
		}
	}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if err := request.Validate(); err != nil {
			t.Fatalf("request %s: %v", node.ID, err)
		}
		roles := request.ToolRoles()
		if len(roles) != 1 || (roles[0] != "cc" && roles[0] != "ld") {
			t.Errorf("request roles = %v", roles)
		}
	}
	if got, want := plan.Nodes[0].RequestID, "2a3c2724f14a5f1b12f74a06ca682bb635dcd4e95f69c5401fbbff28ef09a692"; got != want {
		t.Fatalf("cc-option canonical request ID = %s, want %s", got, want)
	}
}

func resultsForCapabilityPlan(plan *ProbePlan, success map[int]bool, identity string) probeResultMap {
	results := probeResultMap{}
	for index, node := range plan.Nodes {
		value := success[index]
		status, exitCode := "failure", 1
		if value {
			status, exitCode = "success", 0
		}
		results[node.ID] = ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: identity, Kind: "boolean", Boolean: &value,
			Steps: []ProbeStepResult{{Name: "probe", Status: status, ExitCode: exitCode}},
		}
	}
	return results
}

func TestLinuxProbeEvaluatorReplaysExactResultsAndSameDAG(t *testing.T) {
	discoveryBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	discoveredValues := exerciseSymbolicProbeEvaluator(t, discovery)
	discoveredPlan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	results := resultsForCapabilityPlan(discoveredPlan, map[int]bool{0: true, 1: true, 2: false, 3: true, 4: false}, bootstrapTestIdentity)

	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	replayedValues := exerciseSymbolicProbeEvaluator(t, replay)
	if replayedValues != discoveredValues {
		t.Fatalf("replay symbolic values = %#v, want discovery %#v", replayedValues, discoveredValues)
	}
	for name, test := range map[string]struct{ value, want string }{
		"cc option": {replayedValues.ccOption, "y"}, "bit option": {replayedValues.bitOption, "-m64"},
		"assembler": {replayedValues.assembler, "n"}, "linker": {replayedValues.linker, "y"},
		"C source": {replayedValues.cSource, "n"},
	} {
		got, resolveErr := replay.ResolveSymbolic(test.value)
		if resolveErr != nil || got != test.want {
			t.Errorf("replay %s = %q, %v; want %q", name, got, resolveErr, test.want)
		}
	}
	replayPlan, err := replayBuilder.Plan(replay.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replayPlan.Nodes, discoveredPlan.Nodes; !slices.EqualFunc(got, want, func(a, b ProbePlanNode) bool {
		return a.ID == b.ID && a.RequestID == b.RequestID && a.Scope == b.Scope && slices.Equal(a.Inputs, b.Inputs)
	}) {
		t.Fatalf("replay nodes differ from discovery:\n%#v\n%#v", got, want)
	}

	bad := resultsForCapabilityPlan(discoveredPlan, map[int]bool{0: true}, bootstrapTestIdentity)
	result := bad[discoveredPlan.Nodes[0].ID]
	result.Steps[0].Name = "different-step"
	bad[discoveredPlan.Nodes[0].ID] = result
	badBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	badReplay, _ := testSymbolicProbeEvaluator(t, badBuilder, bad)
	badValues := exerciseSymbolicProbeEvaluator(t, badReplay)
	if _, resolveErr := badReplay.ResolveSymbolic(badValues.ccOption); resolveErr == nil || !strings.Contains(resolveErr.Error(), "exact step plan") {
		t.Fatalf("malformed result error = %v, want exact step plan", resolveErr)
	}
}

func testLargeLinuxProbeSelectionInput(t testing.TB, index int, variant string) linuxProbeSelectionInput {
	t.Helper()
	dependencies := []ProbeReference{
		{
			NodeID: fmt.Sprintf("%064x", 4*index+1), RequestID: fmt.Sprintf("%064x", 4*index+2),
			Scope: "target", Kind: "boolean",
		},
		{
			NodeID: fmt.Sprintf("%064x", 4*index+3), RequestID: fmt.Sprintf("%064x", 4*index+4),
			Scope: "target", Kind: "text",
		},
	}
	arguments := []string{
		"-Werror", "-c", "-x", "c", "${source:scripts/probe.c}", "-o", "${scratch:out}",
	}
	candidateBase := make([]int, 0, 128)
	for flag := 0; flag < 128; flag++ {
		candidateBase = append(candidateBase, len(arguments))
		arguments = append(arguments, fmt.Sprintf("-DSELECTION_%02d_%03d=%d", index, flag, flag))
	}
	environment := make(map[string]string, 64)
	for variable := 0; variable < 64; variable++ {
		environment[fmt.Sprintf("PROBE_ENV_%03d", variable)] = fmt.Sprintf("selection-%02d-value-%03d", index, variable)
	}
	warnings := make([]ProbePredicate, 32)
	for warning := range warnings {
		warnings[warning] = ProbePredicate{
			Operator: "stream-contains", Step: "compile", Stream: "stderr",
			Value: fmt.Sprintf("warning-%03d-%s", warning, variant),
		}
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(dependencies),
		Sources: []string{"scripts/probe.c"},
		Scratch: []ProbeScratch{{Name: "out", Kind: "file"}},
		Steps: []ProbeStep{{
			Name: "compile", Tool: "cc", Arguments: arguments, Environment: environment,
			ConditionalArguments: []ProbeConditionalArguments{{
				Before:    0,
				When:      ProbePredicate{Operator: "result-true", Result: "00000000"},
				Arguments: []string{"-DDEPENDENCY_ENABLED=1"},
			}},
			Candidate: &ProbeCandidateArguments{Policy: ProbeCandidatePolicyCC, Base: candidateBase},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "all",
			Operands: []ProbePredicate{
				{Operator: "exit-zero", Step: "compile"},
				{Operator: "not", Operands: []ProbePredicate{{Operator: "any", Operands: warnings}}},
			},
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("large selection request is invalid: %v", err)
	}
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := builder.Request("target", request, dependencies...)
	if err != nil {
		t.Fatal(err)
	}
	return linuxProbeSelectionInput{reference: reference, request: request, dependencies: dependencies}
}

func testLinuxProbeSelectionValues(inputCount int) []string {
	values := make([]string, 1<<inputCount)
	for state := range values {
		values[state] = fmt.Sprintf("selection-state-%03d", state)
	}
	return values
}

func testLinuxProbeSelectionEvaluator() *LinuxProbeEvaluator {
	return &LinuxProbeEvaluator{
		scope: "target", symbols: map[string]linuxProbeSymbol{}, symbolRegistry: newLinuxProbeSymbolRegistry(),
	}
}

func linuxProbeSelectionRegistrySize(evaluator *LinuxProbeEvaluator) int {
	evaluator.symbolRegistry.mu.RLock()
	defer evaluator.symbolRegistry.mu.RUnlock()
	return len(evaluator.symbolRegistry.symbols)
}

func TestLinuxProbeEvaluatorRenderSelectionExistingTokenFastPath(t *testing.T) {
	t.Run("exact repeat keeps maps stable", func(t *testing.T) {
		evaluator := testLinuxProbeSelectionEvaluator()
		inputs := make([]linuxProbeSelectionInput, 4)
		for index := range inputs {
			inputs[index] = testLargeLinuxProbeSelectionInput(t, index, "same")
		}
		values := testLinuxProbeSelectionValues(len(inputs))
		first, err := evaluator.renderSelection(inputs, values)
		if err != nil {
			t.Fatal(err)
		}
		localSize, registrySize := len(evaluator.symbols), linuxProbeSelectionRegistrySize(evaluator)
		second, err := evaluator.renderSelection(inputs, values)
		if err != nil {
			t.Fatal(err)
		}
		if second != first {
			t.Fatalf("repeated selection token = %q, want %q", second, first)
		}
		if got := len(evaluator.symbols); got != localSize {
			t.Fatalf("repeated selection grew evaluator symbols to %d, want %d", got, localSize)
		}
		if got := linuxProbeSelectionRegistrySize(evaluator); got != registrySize {
			t.Fatalf("repeated selection grew registry symbols to %d, want %d", got, registrySize)
		}
	})

	t.Run("canonical empty collections reuse across evaluators", func(t *testing.T) {
		firstEvaluator := testLinuxProbeSelectionEvaluator()
		original := testLargeLinuxProbeSelectionInput(t, 0, "same")
		values := testLinuxProbeSelectionValues(1)
		token, err := firstEvaluator.renderSelection([]linuxProbeSelectionInput{original}, values)
		if err != nil {
			t.Fatal(err)
		}

		equivalent := original
		equivalent.request = cloneLinuxProbeRequest(original.request)
		equivalent.dependencies = slices.Clone(original.dependencies)
		equivalent.request.SourceRoots = []string{}
		equivalent.request.Steps[0].AuxiliaryTools = []string{}
		equivalent.request.Steps[0].ArgumentFragments = []ProbeArgumentFragments{}
		equivalent.request.Steps[0].EnvironmentFragments = []ProbeEnvironmentFragments{}
		equivalent.request.Steps[0].StdinFragments = []ProbeValueFragment{}
		equivalent.request.Steps[0].Candidate.Conditional = []int{}
		equivalent.request.Steps[0].Candidate.TranslationUnits = []string{}
		equivalent.request.Outcome.Fragments = []ProbeValueFragment{}
		originalID, err := original.request.ID()
		if err != nil {
			t.Fatal(err)
		}
		equivalentID, err := equivalent.request.ID()
		if err != nil {
			t.Fatal(err)
		}
		if equivalentID != originalID {
			t.Fatalf("canonically equivalent request ID = %s, want %s", equivalentID, originalID)
		}

		secondEvaluator := testLinuxProbeSelectionEvaluator()
		secondEvaluator.symbolRegistry = firstEvaluator.symbolRegistry
		reused, err := secondEvaluator.renderSelection([]linuxProbeSelectionInput{equivalent}, slices.Clone(values))
		if err != nil {
			t.Fatal(err)
		}
		if reused != token {
			t.Fatalf("canonically equivalent selection token = %q, want %q", reused, token)
		}
	})

	t.Run("different nested request collides", func(t *testing.T) {
		evaluator := testLinuxProbeSelectionEvaluator()
		original := testLargeLinuxProbeSelectionInput(t, 0, "original")
		if _, err := evaluator.renderSelection([]linuxProbeSelectionInput{original}, testLinuxProbeSelectionValues(1)); err != nil {
			t.Fatal(err)
		}
		changed := testLargeLinuxProbeSelectionInput(t, 0, "changed")
		changed.reference = original.reference
		if _, err := evaluator.renderSelection([]linuxProbeSelectionInput{changed}, testLinuxProbeSelectionValues(1)); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("structurally different request error = %v, want collision", err)
		}
	})

	t.Run("different nested request collides through shared registry", func(t *testing.T) {
		firstEvaluator := testLinuxProbeSelectionEvaluator()
		original := testLargeLinuxProbeSelectionInput(t, 0, "original")
		values := testLinuxProbeSelectionValues(1)
		if _, err := firstEvaluator.renderSelection([]linuxProbeSelectionInput{original}, values); err != nil {
			t.Fatal(err)
		}
		changed := testLargeLinuxProbeSelectionInput(t, 0, "changed")
		changed.reference = original.reference
		secondEvaluator := testLinuxProbeSelectionEvaluator()
		secondEvaluator.symbolRegistry = firstEvaluator.symbolRegistry
		if _, err := secondEvaluator.renderSelection([]linuxProbeSelectionInput{changed}, values); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("shared-registry request error = %v, want collision", err)
		}
	})

	t.Run("different dependencies collide", func(t *testing.T) {
		evaluator := testLinuxProbeSelectionEvaluator()
		original := testLargeLinuxProbeSelectionInput(t, 0, "same")
		if _, err := evaluator.renderSelection([]linuxProbeSelectionInput{original}, testLinuxProbeSelectionValues(1)); err != nil {
			t.Fatal(err)
		}
		changed := testLargeLinuxProbeSelectionInput(t, 0, "same")
		changed.dependencies[1].NodeID = strings.Repeat("f", 64)
		if _, err := evaluator.renderSelection([]linuxProbeSelectionInput{changed}, testLinuxProbeSelectionValues(1)); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("different dependencies error = %v, want collision", err)
		}
	})

	t.Run("stored selection owns caller data", func(t *testing.T) {
		evaluator := testLinuxProbeSelectionEvaluator()
		inputs := []linuxProbeSelectionInput{testLargeLinuxProbeSelectionInput(t, 0, "same")}
		values := testLinuxProbeSelectionValues(len(inputs))
		token, err := evaluator.renderSelection(inputs, values)
		if err != nil {
			t.Fatal(err)
		}

		exactInputs := []linuxProbeSelectionInput{testLargeLinuxProbeSelectionInput(t, 0, "same")}
		exactValues := testLinuxProbeSelectionValues(len(exactInputs))
		inputs[0].reference.NodeID = strings.Repeat("e", 64)
		inputs[0].dependencies[0].NodeID = strings.Repeat("d", 64)
		inputs[0].request.Steps[0].Arguments[0] = "-Wmutated"
		inputs[0].request.Steps[0].Environment["PROBE_ENV_000"] = "mutated"
		inputs[0].request.Outcome.Predicate.Operands[1].Operands[0].Operands[0].Value = "mutated"
		values[0] = "mutated"

		stored := evaluator.symbols[token]
		if !reflect.DeepEqual(stored.selectionInputs, exactInputs) || !slices.Equal(stored.selectionValues, exactValues) {
			t.Fatalf("stored selection changed through caller-owned slices: %#v", stored)
		}
		registered, ok := evaluator.symbolRegistry.lookup(token)
		if !ok || !reflect.DeepEqual(registered.selectionInputs, exactInputs) || !slices.Equal(registered.selectionValues, exactValues) {
			t.Fatalf("registered selection changed through caller-owned slices: %#v, found %v", registered, ok)
		}
		repeated, err := evaluator.renderSelection(exactInputs, exactValues)
		if err != nil {
			t.Fatal(err)
		}
		if repeated != token {
			t.Fatalf("reconstructed selection token = %q, want %q", repeated, token)
		}
	})
}

func BenchmarkLinuxProbeEvaluatorRenderSelectionExistingToken(b *testing.B) {
	for _, inputCount := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("inputs-%d", inputCount), func(b *testing.B) {
			evaluator := testLinuxProbeSelectionEvaluator()
			inputs := make([]linuxProbeSelectionInput, inputCount)
			for index := range inputs {
				inputs[index] = testLargeLinuxProbeSelectionInput(b, index, "benchmark")
			}
			values := testLinuxProbeSelectionValues(inputCount)
			want, err := evaluator.renderSelection(inputs, values)
			if err != nil {
				b.Fatal(err)
			}
			localSize, registrySize := len(evaluator.symbols), linuxProbeSelectionRegistrySize(evaluator)
			var got string
			b.ReportAllocs()
			b.ReportMetric(float64(inputCount), "inputs/op")
			b.ResetTimer()
			for range b.N {
				got, err = evaluator.renderSelection(inputs, values)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got != want {
				b.Fatalf("last selection token = %q, want %q", got, want)
			}
			if len(evaluator.symbols) != localSize || linuxProbeSelectionRegistrySize(evaluator) != registrySize {
				b.Fatal("existing-token fast path grew symbol maps")
			}
		})
	}
}

func TestLinuxProbeEvaluatorStructuralReplayPreservesNestedSelectionAtoms(t *testing.T) {
	outerSelection := func(evaluator *LinuxProbeEvaluator, values evaluatorExercise) string {
		t.Helper()
		gate, ok, err := evaluator.adoptSymbol(values.ccOption)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || gate.kind != "boolean" {
			t.Fatalf("gate symbol = %#v, want boolean", gate)
		}
		selected, err := evaluator.renderSelection([]linuxProbeSelectionInput{{
			reference: gate.reference, request: gate.request,
			dependencies: slices.Clone(gate.dependencies),
		}}, []string{"", "__LINUX_BZL_MAKE__ FLAGS=" + values.bitOption + " child"})
		if err != nil {
			t.Fatal(err)
		}
		return selected
	}

	discoveryBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	discoveryValues := exerciseSymbolicProbeEvaluator(t, discovery)
	discoveryOuter := outerSelection(discovery, discoveryValues)
	structure, err := discovery.ResolveSymbolicStructure(discoveryOuter)
	if err != nil {
		t.Fatal(err)
	}
	if structure != discoveryOuter {
		t.Fatalf("discovery structure = %q, want unchanged outer selection %q", structure, discoveryOuter)
	}
	discoveryPlan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	results := resultsForCapabilityPlan(discoveryPlan, map[int]bool{
		0: true, 1: true, 2: true, 3: true, 4: true,
	}, bootstrapTestIdentity)

	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	replayValues := exerciseSymbolicProbeEvaluator(t, replay)
	replayOuter := outerSelection(replay, replayValues)
	structure, err = replay.ResolveSymbolicStructure(replayOuter)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(structure, "__LINUX_BZL_MAKE__ FLAGS=") ||
		!strings.Contains(structure, replayValues.bitOption) ||
		!linuxProbeSymbolPattern.MatchString(structure) {
		t.Fatalf("structural replay = %q, want selected Make branch retaining nested atom %q", structure, replayValues.bitOption)
	}
	resolved, err := replay.ResolveSymbolic(replayOuter)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "__LINUX_BZL_MAKE__ FLAGS=-m64 child" {
		t.Fatalf("full replay = %q, want concrete recursive Make argv", resolved)
	}
}

func TestKconfigParserPreservesSymbolicProbeUntilReplay(t *testing.T) {
	const source = `
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
cc-option = $(success,$(CC) -Werror $(1) -c -x c /dev/null -o .tmp_probe/tmp.o)
capability := $(cc-option,-fbrand-new)
$(warning-if,$(capability),measured capability enabled)

config MEASURED_CAPABILITY
	bool
	default $(capability)
`
	discoveryBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	discoveryTree, err := Parse(context.Background(), strings.NewReader(source), "Kconfig", Options{
		Env: map[string]string{"CC": "/configured/clang"}, Shell: discovery.Shell, ResolveSymbolic: discovery.ResolveSymbolic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discoveryTree.Diagnostics) != 0 {
		t.Fatalf("discovery guessed symbolic warning condition: %#v", discoveryTree.Diagnostics)
	}
	defaults := propertiesOfType(discoveryTree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || !strings.Contains(defaults[0].Expr.String(), linuxProbeSymbolPrefix) {
		t.Fatalf("discovery default did not preserve symbolic atom: %#v", defaults)
	}
	plan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	results := resultsForCapabilityPlan(plan, map[int]bool{0: true}, bootstrapTestIdentity)
	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	replayTree, err := Parse(context.Background(), strings.NewReader(source), "Kconfig", Options{
		Env: map[string]string{"CC": "/configured/clang"}, Shell: replay.Shell, ResolveSymbolic: replay.ResolveSymbolic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayTree.Diagnostics) != 1 || replayTree.Diagnostics[0].Message != "measured capability enabled" {
		t.Fatalf("replay diagnostics = %#v", replayTree.Diagnostics)
	}
	defaults = propertiesOfType(replayTree.Root.Children[0], PropertyDefault)
	if len(defaults) != 1 || defaults[0].Expr.String() != `"y"` {
		t.Fatalf("replay default = %#v, want y", defaults)
	}
	replayPlan, err := replayBuilder.Plan(replay.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayPlan.Nodes) != 1 || replayPlan.Nodes[0].ID != plan.Nodes[0].ID {
		t.Fatalf("replay Kconfig plan differs from discovery: %#v vs %#v", replayPlan.Nodes, plan.Nodes)
	}
}

func TestKbuildShellAcceptsExternalModuleTryRunDirectory(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	tempDir := "__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/module/.tmp_$$"
	command := `set -e; TMP=` + tempDir + `/tmp; trap "rm -rf ` + tempDir + `" EXIT; mkdir -p ` + tempDir + `; if (` +
		KbuildActionRoleToken("target", "ld") +
		` --no-apply-dynamic-relocs -v) >/dev/null 2>&1; then echo " --no-apply-dynamic-relocs"; else echo ""; fi`
	value, err := evaluator.KbuildShell(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(strings.TrimSpace(value)) {
		t.Fatalf("external-module Kbuild try-run = %q, want symbolic linker result", value)
	}

	for _, invalid := range []string{
		"__LINUX_BZL_SOURCE_TREE__/.tmp_$$",
		"__LINUX_BZL_SOURCE_TREE__/../outside/.tmp_$$",
		"__LINUX_BZL_SOURCE_TREE__/external/$module/.tmp_$$",
		"__LINUX_BZL_OBJECT_TREE__/external/module/.tmp_$$",
	} {
		if validKbuildTryRunTempDir(invalid) {
			t.Errorf("validKbuildTryRunTempDir(%q) = true", invalid)
		}
	}
}

func symbolicKbuildOptions(evaluator *LinuxProbeEvaluator) KbuildOptions {
	return KbuildOptions{
		Variables: map[string]string{
			"SRCARCH": "arm64",
			"CC":      "/configured/clang",
			"LD":      "/configured/ld.lld",
		},
		MakeVariablesComplete: true,
		Shell: func(command string) (string, error) {
			return evaluator.KbuildShell(context.Background(), command)
		},
		ResolveSymbolic: evaluator.ResolveSymbolic,
		CaptureVariables: []string{
			"KBUILD_CFLAGS", "KBUILD_AFLAGS", "KBUILD_LDFLAGS", "try_run_result",
		},
	}
}

func parseSymbolicKbuildFixture(t *testing.T, evaluator *LinuxProbeEvaluator) *KbuildFile {
	t.Helper()
	const source = `
comma := ,
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) $(3) -c -x c /dev/null -o "$$TMP",$(3),$(4))
cc-option = $(call __cc-option,$(CC),$(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS),$(1),$(2))
as-option = $(call try-run,$(CC) -Werror $(KBUILD_CPPFLAGS) $(KBUILD_AFLAGS) $(1) -c -x assembler-with-cpp /dev/null -o "$$TMP",$(1),$(2))
as-instr = $(call try-run,printf "%b\n" "$(1)" | $(CC) -Werror $(KBUILD_CPPFLAGS) $(KBUILD_AFLAGS) -c -x assembler-with-cpp -o "$$TMP" -,$(2),$(3))
ld-option = $(call try-run,$(LD) $(KBUILD_LDFLAGS) $(1) -v,$(1),$(2))
first_cc := $(call cc-option,-fbrand-new,-ffallback)
KBUILD_CFLAGS := $(first_cc)
second_cc := $(call cc-option,-fsecond)
KBUILD_CFLAGS += $(second_cc)
first_as := $(call as-option,-Wa$(comma)--brand-new)
KBUILD_AFLAGS := $(first_as)
second_as := $(call as-instr,.arch_extension lse,-DHAVE_LSE=1,-DNO_LSE=1)
KBUILD_AFLAGS += $(second_as)
selected_ld := $(call ld-option,--brand-new-linker,--fallback-linker)
KBUILD_LDFLAGS := $(selected_ld)
try_run_result := $(call try-run,echo 'int feature(void) { return 1; }' | /configured/clang -x c - -S -o "$$TMP" -Werror,-DTRY_RUN=1,-DTRY_RUN=0)
`
	kb, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", symbolicKbuildOptions(evaluator), "")
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

func TestKbuildParserDiscoversSymbolicCompilerCapabilities(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	kb := parseSymbolicKbuildFixture(t, evaluator)
	for name, value := range kb.Variables {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Errorf("discovery variable %s = %q, want symbolic probe", name, value)
		}
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 6; got != want {
		t.Fatalf("Kbuild capability nodes = %d, want %d: %#v", got, want, plan.Nodes)
	}
	if got, want := plan.Nodes[1].Inputs, []string{plan.Nodes[0].ID}; !slices.Equal(got, want) {
		t.Fatalf("second cc-option inputs = %v, want prior flag probe %v", got, want)
	}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if err := request.Validate(); err != nil {
			t.Fatalf("request %s: %v", node.ID, err)
		}
		if roles := request.ToolRoles(); len(roles) != 1 || (roles[0] != "cc" && roles[0] != "ld") {
			t.Errorf("request %s roles = %v", node.ID, roles)
		}
	}
}

func TestKbuildProbeReplayAcceptsConfiguredActionRoleToken(t *testing.T) {
	for _, scope := range []string{"target", KbuildActionRoleAutoScope} {
		t.Run(scope, func(t *testing.T) {
			builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
			if err != nil {
				t.Fatal(err)
			}
			evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
			command := `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (` +
				KbuildActionRoleToken(scope, "ld") +
				` -m elf_x86_64 -z noexecstack --eh-frame-hdr -v) >/dev/null 2>&1; then echo " --eh-frame-hdr"; else echo ""; fi`
			value, err := evaluator.KbuildShell(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if !linuxProbeSymbolPattern.MatchString(strings.TrimSpace(value)) {
				t.Fatalf("configured-role probe replay = %q, want symbolic linker result", value)
			}
		})
	}
}

func TestKbuildParserReplaysExactCompilerCapabilitiesAndSamePlan(t *testing.T) {
	discoveryBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	parseSymbolicKbuildFixture(t, discovery)
	discoveredPlan, err := discoveryBuilder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	results := resultsForCapabilityPlan(discoveredPlan, map[int]bool{
		0: true, 1: false, 2: true, 3: false, 4: false, 5: true,
	}, bootstrapTestIdentity)

	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	kb := parseSymbolicKbuildFixture(t, replay)
	for name, want := range map[string]string{
		"KBUILD_CFLAGS":  "-fbrand-new",
		"KBUILD_AFLAGS":  "-Wa,--brand-new -DNO_LSE=1",
		"KBUILD_LDFLAGS": "--fallback-linker",
		"try_run_result": "-DTRY_RUN=1",
	} {
		if got := strings.TrimSpace(kb.Variables[name]); got != want {
			t.Errorf("replay variable %s = %q, want %q", name, got, want)
		}
	}
	replayPlan, err := replayBuilder.Plan(replay.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replayPlan.Nodes, discoveredPlan.Nodes; !slices.EqualFunc(got, want, func(a, b ProbePlanNode) bool {
		return a.ID == b.ID && a.RequestID == b.RequestID && a.Scope == b.Scope && slices.Equal(a.Inputs, b.Inputs)
	}) {
		t.Fatalf("replay Kbuild nodes differ from discovery:\n%#v\n%#v", got, want)
	}
}

func TestLinuxProbeEvaluatorDefersCandidatePolicyValidationToExecution(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		policy   string
		wantBase func(ProbeStep) []int
	}{
		{
			name:    "macro output selection",
			command: `/configured/clang -dM -E -x c -o /tmp/escape /dev/null | grep -Fq "__clang__"; echo $?`,
			policy:  ProbeCandidatePolicyCC,
			wantBase: func(ProbeStep) []int {
				return []int{4, 5}
			},
		},
		{
			name:    "preprocessor tail output selection",
			command: `echo __SOURCE_TOKEN__ | /configured/clang -o /tmp/escape -E -x c - | tail -n 1`,
			policy:  ProbeCandidatePolicyCC,
			wantBase: func(ProbeStep) []int {
				return []int{0, 1}
			},
		},
		{
			name:    "compiler plugin selection",
			command: `{ /configured/clang -Werror -fplugin=/tmp/escape.so -c -x c /dev/null -o .tmp_probe/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			policy:  ProbeCandidatePolicyCC,
			wantBase: func(ProbeStep) []int {
				return []int{0, 1}
			},
		},
		{
			name:    "compiler positional input",
			command: `{ echo 'int x;' | /configured/clang -x c /tmp/escape.c -c -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			policy:  ProbeCandidatePolicyCC,
			wantBase: func(ProbeStep) []int {
				return []int{0}
			},
		},
		{
			name:    "linker script selection",
			command: `{ /configured/ld.lld -v --script=/tmp/escape.ld; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			policy:  ProbeCandidatePolicyLD,
			wantBase: func(ProbeStep) []int {
				return []int{1}
			},
		},
		{
			name:    "source script link-driver candidates",
			command: `/src/scripts/cc-version.sh /other/compiler extra`,
			policy:  ProbeCandidatePolicyCCLink,
			wantBase: func(step ProbeStep) []int {
				return []int{len(step.Arguments) - 2, len(step.Arguments) - 1}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
			evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
			if value, err := evaluator.Shell(context.Background(), test.command); err != nil || !linuxProbeSymbolPattern.MatchString(value) {
				t.Fatalf("Shell(%q) = %q, %v; want runtime-owned candidate probe", test.command, value, err)
			}
			plan, err := builder.Plan(evaluator.References()...)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Nodes) != 1 {
				t.Fatalf("candidate plan nodes = %#v, want one", plan.Nodes)
			}
			request := plan.Requests[plan.Nodes[0].RequestID]
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			step := request.Steps[0]
			if step.Candidate == nil || step.Candidate.Policy != test.policy ||
				!slices.Equal(step.Candidate.Base, test.wantBase(step)) || len(step.Candidate.Conditional) != 0 {
				t.Fatalf("candidate ownership = %#v, argv=%q", step.Candidate, step.Arguments)
			}
		})
	}
}

func TestLinuxProbeEvaluatorRejectsUnsafeOrUnknownCommands(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	for _, command := range []string{
		`{ printf "%b\n" ".incbin /tmp/escape" | /configured/clang -c -x assembler-with-cpp -o /dev/null -; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`/configured/clang -print-file-name=../escape`,
		`{ ` + KbuildActionRoleToken("target", "objcopy") + ` --version | head -n1 | grep -qi ` + strings.Repeat("x", 129) + `; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`{ ` + KbuildActionRoleToken("target", "objcopy") + ` --version | head -n1 | grep -qi 'llvm.*'; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`{ ` + KbuildActionRoleToken("target", "objcopy") + ` --version | head -n1 | grep -qi "$(command)"; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		"{ " + KbuildActionRoleToken("target", "objcopy") + " --version | head -n1 | grep -qi '`command`'; } >/dev/null 2>&1 && echo \"y\" || echo \"n\"",
		`{ ` + KbuildActionRoleToken("target", "objcopy") + ` --version | head -n1 | grep -qi -llvm; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		`$(CC) --version`,
	} {
		if _, err := evaluator.Shell(context.Background(), command); err == nil {
			t.Errorf("unsafe Shell(%q) succeeded", command)
		} else if IsLinuxProbeUnsupportedCommand(err) {
			t.Errorf("unsafe compiler Shell(%q) was marked safe to delegate: %v", command, err)
		}
	}
	if _, err := evaluator.ResolveSymbolic(linuxProbeSymbolPrefix + strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown symbolic result error = %v", err)
	}
	if len(evaluator.References()) != 0 {
		t.Fatal("unsafe commands emitted probe requests")
	}
}

func TestLinuxProbeEvaluatorRejectsNoncanonicalCompilerPathSuffix(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	root := shellProbeValue(t, evaluator, `/configured/clang -print-file-name=vendor-sdk`)
	command := `{ test -e ` + root + `/../escape.h; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if _, err := evaluator.Shell(context.Background(), command); err == nil {
		t.Fatalf("Shell(%q) accepted a noncanonical compiler path suffix", command)
	} else if IsLinuxProbeUnsupportedCommand(err) {
		t.Fatalf("unsafe compiler path suffix was marked safe to delegate: %v", err)
	}
	if got := len(evaluator.References()); got != 1 {
		t.Fatalf("unsafe compiler path suffix emitted %d additional requests", got-1)
	}
}

func TestLinuxProbeEvaluatorIdentifiesOnlyDelegableUnsupportedCommands(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	for _, command := range []string{
		`printf '%s' source-defined`,
		`{ test -d generated; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	} {
		_, err := evaluator.Shell(context.Background(), command)
		if err == nil || !IsLinuxProbeUnsupportedCommand(err) {
			t.Errorf("Shell(%q) error = %v, want delegable unsupported command", command, err)
			continue
		}
		if !IsLinuxProbeDeferredRecipeCommand(err) {
			t.Errorf("Shell(%q) error was not recognized as deferrable recipe work: %v", command, err)
		}
		if !IsLinuxProbeUnsupportedCommand(fmt.Errorf("caller context: %w", err)) {
			t.Errorf("wrapped Shell(%q) error was not recognized: %v", command, err)
		}
	}
	if got, err := evaluator.Shell(context.Background(), `{ command -v custom-generator; } >/dev/null 2>&1 && echo "y" || echo "n"`); err != nil || got != "n" {
		t.Fatalf("unselected manifest tool lookup = %q, %v; want n", got, err)
	}
	for index, command := range []string{
		`/configured/clang --unsupported-probe`,
		`/other/clang --version`,
		`{ command -v /configured/clang extra; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	} {
		_, err := evaluator.Shell(context.Background(), command)
		if err == nil {
			t.Errorf("malformed compiler Shell(%q) succeeded", command)
		} else if IsLinuxProbeUnsupportedCommand(err) {
			t.Errorf("malformed compiler Shell(%q) was marked safe to delegate: %v", command, err)
		} else if index == 0 && !IsLinuxProbeDeferredRecipeCommand(fmt.Errorf("selected recipe: %w", err)) {
			t.Errorf("owned compiler Shell(%q) was not recognized as deferrable selected-recipe work: %v", command, err)
		}
	}
	if len(evaluator.References()) != 0 {
		t.Fatal("unsupported commands emitted probe requests")
	}
}

func newFixtureProbeEvaluator(
	t *testing.T,
	fixtureIndex int,
	architecture string,
	discovery ProbeDiscovery,
	oracle ProbeResultLookup,
	withRust bool,
) (*LinuxProbeEvaluator, bootstrapFixture) {
	t.Helper()
	fixture := linuxCompilerBootstrapFixtures(t)[fixtureIndex]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{
		"cc": "/configured/gcc", "ld": "/configured/ld.bfd", "ar": "/configured/ar",
		"nm": "/configured/nm", "objcopy": "/configured/objcopy",
	}
	var rustSourceRoot string
	// Tool paths are explicit fixture inputs; they are never inferred from the
	// bootstrap version text by the production evaluator.
	if fixture.name == "clang" {
		tools = map[string]string{
			"cc": "/configured/clang", "ld": "/configured/ld.lld", "ar": "/configured/llvm-ar",
			"nm": "/configured/llvm-nm", "objcopy": "/configured/llvm-objcopy",
		}
	}
	if withRust {
		tools["rustc"], tools["bindgen"] = "/configured/rustc", "/configured/bindgen"
		rustSourceRoot = testRustSourceRoot()
	}
	scriptEnvironment := map[string]string{"CC": tools["cc"]}
	if rustSourceRoot != "" {
		scriptEnvironment["BINDGEN"] = tools["bindgen"]
		scriptEnvironment["KRUSTFLAGS"] = ""
		scriptEnvironment["RUSTC"] = tools["rustc"]
		scriptEnvironment["RUST_LIB_SRC"] = rustSourceRoot
	}
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: fixture.scope, Architecture: architecture, SourceArchitecture: architecture, SourceRoot: "/src", Facts: facts, Tools: tools,
		ScriptEnvironment: scriptEnvironment, Discovery: discovery, Oracle: oracle, RustSourceRoot: rustSourceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluator, fixture
}

func TestLinuxProbeEvaluatorModelsSourceScriptAndPythonLXMLAsActions(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	rust := shellProbeValue(t, evaluator, `{ /src/scripts/rust_is_available.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	python := shellProbeValue(t, evaluator, `{ /configured/python3 -c "import lxml"; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	for name, value := range map[string]string{"Rust": rust, "Python": python} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Fatalf("%s capability = %q, want symbolic action", name, value)
		}
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	foundSourceScript, foundPython := false, false
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		switch strings.Join(request.ToolRoles(), ",") {
		case "bindgen,cc,rustc,script-runtime,scriptrun":
			foundSourceScript = true
			if got, want := request.Sources, []string{"Kconfig", "scripts/rust_is_available.sh"}; !slices.Equal(got, want) {
				t.Fatalf("source-script sources = %q, want %q", got, want)
			}
			if got, want := request.SourceRoots, []string{"linux", "rust"}; !slices.Equal(got, want) {
				t.Fatalf("source-script roots = %q, want %q", got, want)
			}
			if len(request.Scratch) != 0 || len(request.Steps) != 1 {
				t.Fatalf("source-script recipe = %#v", request)
			}
			step := request.Steps[0]
			if step.Tool != "scriptrun" || step.WorkingDirectory != "${source_root:linux}" {
				t.Fatalf("source-script step = %#v", step)
			}
			if got, want := step.AuxiliaryTools, []string{"bindgen", "cc", "rustc"}; !slices.Equal(got, want) {
				t.Fatalf("source-script auxiliary tools = %q, want %q", got, want)
			}
			if got, want := step.Arguments, []string{
				"-interpreter", "${tool:script-runtime}",
				"-interpreter_arg", "sh",
				"-multicall", "${tool:script-runtime}",
				"-script", "${source:scripts/rust_is_available.sh}",
				"-tool", "bindgen=${tool:bindgen}",
				"-tool", "cc=${tool:cc}",
				"-tool", "rustc=${tool:rustc}",
				"--",
			}; !slices.Equal(got, want) {
				t.Fatalf("source-script argv = %q, want %q", got, want)
			}
			for name, want := range map[string]string{
				"ARCH": "arm64", "BINDGEN": "bindgen", "CC": "cc", "KRUSTFLAGS": "",
				"RUSTC": "rustc", "RUST_LIB_SRC": "${source_root:rust}", "SRCARCH": "arm64",
			} {
				if got := step.Environment[name]; got != want {
					t.Errorf("source-script environment %s = %q, want %q", name, got, want)
				}
			}
			if len(step.Environment) != 7 {
				t.Fatalf("source-script environment = %q", step.Environment)
			}
		case "python3":
			foundPython = true
			if len(request.Steps) != 1 || !slices.Equal(request.Steps[0].Arguments, []string{"-c", "import lxml"}) {
				t.Fatalf("Python lxml recipe = %#v", request)
			}
		}
	}
	if !foundSourceScript || !foundPython {
		t.Fatalf("capability plan found source script=%v Python=%v", foundSourceScript, foundPython)
	}
}

func TestLinuxProbeEvaluatorDefersIncompleteRustToolAvailabilityToSource(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: fixture.scope, Architecture: "arm64", SourceArchitecture: "arm64", SourceRoot: "/src",
		Facts: facts, Tools: map[string]string{"cc": "/configured/clang"},
		ScriptEnvironment: map[string]string{
			"CC": "/configured/clang", "RUST_LIB_SRC": testRustSourceRoot(),
		},
		Discovery: builder, RustSourceRoot: testRustSourceRoot(),
	})
	if err != nil {
		t.Fatalf("partial Rust toolset was rejected before source availability probing: %v", err)
	}
	shellProbeValue(t, evaluator, `{ /src/scripts/rust_is_available.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("partial Rust availability nodes = %d, want %d source-script probe", got, want)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if got, want := request.ToolRoles(), []string{"cc", "script-runtime", "scriptrun"}; !slices.Equal(got, want) {
		t.Fatalf("partial Rust availability roles = %q, want only configured roles %q", got, want)
	}
	if got, want := request.SourceRoots, []string{"linux", "rust"}; !slices.Equal(got, want) {
		t.Fatalf("partial Rust availability source roots = %q, want %q", got, want)
	}
}

func shellProbeValue(t *testing.T, evaluator *LinuxProbeEvaluator, command string) string {
	t.Helper()
	value, err := evaluator.Shell(context.Background(), command)
	if err != nil {
		t.Fatalf("Shell(%q): %v", command, err)
	}
	return value
}

func TestLinuxProbeEvaluatorWithScriptEnvironmentAdoptsSymbolsLazily(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "arm64", builder, nil, true)
	token := shellProbeValue(t, evaluator, `{ /configured/clang -Werror -fshared-symbol -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "-fshared-symbol" || echo ""`)
	if _, ok := evaluator.symbols[token]; !ok {
		t.Fatalf("original evaluator omits published symbol %q", token)
	}

	environment := maps.Clone(evaluator.scriptEnvironment)
	environment["SOURCE_SELECTED"] = "next"
	refreshed, err := evaluator.WithScriptEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(refreshed.symbols); got != 0 {
		t.Fatalf("refreshed evaluator eagerly copied %d symbols, want an empty adoption cache", got)
	}
	resolved, err := refreshed.ResolveSymbolic(token)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != token {
		t.Fatalf("discovery resolution = %q, want preserved token %q", resolved, token)
	}
	if _, ok := refreshed.symbols[token]; !ok {
		t.Fatalf("refreshed evaluator did not adopt shared symbol %q", token)
	}
	if got, want := refreshed.References(), evaluator.References(); !slices.Equal(got, want) {
		t.Fatalf("refreshed references = %#v, want %#v", got, want)
	}
}

func TestRustCompilerOptionProbeMergesInheritedAndInlineEnvironment(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "arm64", builder, nil, true)
	sourceEnvironment := maps.Clone(evaluator.scriptEnvironment)
	sourceEnvironment["ROLE_FREE"] = "source-selected"
	sourceEnvironment["RUSTC_BOOTSTRAP"] = "source-selected"
	evaluator, err = evaluator.WithScriptEnvironment(sourceEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	command := func(inline string) string {
		return `{ trap "rm -rf .tmp_2" EXIT; mkdir .tmp_2; ` + inline + `/configured/rustc -Zbrand-new --crate-type=rlib /dev/null --out-dir=.tmp_2 -o .tmp_2/tmp.rlib; } >/dev/null 2>&1 && echo "y" || echo "n"`
	}
	shellProbeValue(t, evaluator, command(""))
	shellProbeValue(t, evaluator, command("RUSTC_BOOTSTRAP=inline "))
	if got := sourceEnvironment["RUSTC_BOOTSTRAP"]; got != "source-selected" {
		t.Fatalf("caller environment mutated to RUSTC_BOOTSTRAP=%q", got)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if len(request.Steps) != 1 || request.Steps[0].Tool != "rustc" {
			continue
		}
		step := request.Steps[0]
		bootstrap := step.Environment["RUSTC_BOOTSTRAP"]
		found[bootstrap] = node.RequestID
		if got := step.Environment["ROLE_FREE"]; got != "source-selected" {
			t.Errorf("%s rustc ROLE_FREE=%q, want inherited source-selected", bootstrap, got)
		}
		if got := step.Environment["RUSTC"]; got != "${tool:rustc}" {
			t.Errorf("%s rustc RUSTC=%q, want configured tool placeholder", bootstrap, got)
		}
		if got := step.Environment["RUST_LIB_SRC"]; got != "${source_root:rust}" {
			t.Errorf("%s rustc RUST_LIB_SRC=%q, want declared source root", bootstrap, got)
		}
		if got, want := step.AuxiliaryTools, []string{"bindgen", "cc"}; !slices.Equal(got, want) {
			t.Errorf("%s rustc auxiliary tools=%q, want %q", bootstrap, got, want)
		}
		if got, want := request.SourceRoots, []string{"rust"}; !slices.Equal(got, want) {
			t.Errorf("%s rustc source roots=%q, want %q", bootstrap, got, want)
		}
	}
	if found["source-selected"] == "" || found["inline"] == "" {
		t.Fatalf("rustc environments = %#v, want inherited and inline override requests", found)
	}
	if found["source-selected"] == found["inline"] {
		t.Fatal("rustc request identity did not include the effective environment")
	}
	if _, err := evaluator.Shell(context.Background(), command("RUSTC_BOOTSTRAP=$(unsafe) ")); err == nil || !strings.Contains(err.Error(), "unsafe rustc-option environment") {
		t.Fatalf("unsafe inline assignment error = %v", err)
	}
}

func TestLinuxProbeEvaluatorDiscoversExactSpecialProbeRecipes(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "arm64", builder, nil, true)
	bit := shellProbeValue(t, evaluator, `{ /configured/clang -Werror -m64 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "-m64" || echo ""`)
	values := []string{
		shellProbeValue(t, evaluator, `{ /src/scripts/cc-can-link.sh /configured/clang -fintegrated-as `+bit+` -static; } >/dev/null 2>&1 && echo "y" || echo "n"`),
		shellProbeValue(t, evaluator, `{ trap "rm -rf .tmp_2" EXIT; mkdir .tmp_2; /configured/rustc -Cllvm-args=--loongarch-annotate-tablejump --crate-type=rlib /dev/null --out-dir=.tmp_2 -o .tmp_2/tmp.rlib; } >/dev/null 2>&1 && echo "y" || echo "n"`),
		shellProbeValue(t, evaluator, `{ `+KbuildActionRoleToken("target", "nm")+` --help | head -n 1 | grep -qi 'Acme Tools'; } >/dev/null 2>&1 && echo "y" || echo "n"`),
		shellProbeValue(t, evaluator, `{ env "CC=/configured/clang" "LD=/configured/ld.lld" "NM=/configured/llvm-nm" "OBJCOPY=/configured/llvm-objcopy" /src/scripts/tools-support-relr.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`),
	}
	vendorRoot := shellProbeValue(t, evaluator, `/configured/clang -print-file-name=vendor-sdk`)
	values = append(values, vendorRoot)
	values = append(values, shellProbeValue(t, evaluator, `{ test -e `+vendorRoot+`/headers/vendor-capability.h; } >/dev/null 2>&1 && echo "y" || echo "n"`))
	values = append(values, shellProbeValue(t, evaluator, `/configured/bindgen --version workaround-for-0.69.0 2>/dev/null`))
	for index, value := range values {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Errorf("special probe %d = %q, want symbolic result", index, value)
		}
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		roles := strings.Join(request.ToolRoles(), ",")
		found[roles]++
		if len(request.Steps) == 1 && request.Steps[0].Tool == "rustc" {
			found["primary:rustc"]++
		}
		if roles == "bindgen,cc,rustc,script-runtime,scriptrun" && request.InputCount == 1 {
			if len(request.Steps[0].ConditionalArguments) == 0 || len(node.Inputs) != 1 {
				t.Fatalf("cc-can-link lost symbolic bit dependency: %#v %#v", node, request)
			}
			if !slices.Contains(request.Sources, "scripts/cc-can-link.sh") {
				t.Fatalf("dependent source-script request is not cc-can-link: %#v", request)
			}
		}
		if len(request.Steps) == 1 && request.Steps[0].Tool == "rustc" && !slices.Equal(request.Steps[0].Arguments, []string{
			"-Cllvm-args=--loongarch-annotate-tablejump", "--crate-type=rlib", "/dev/null",
			"--out-dir=${scratch:out}", "-o", "${scratch:out}/tmp.rlib",
		}) {
			t.Fatalf("rustc request argv = %q", request.Steps[0].Arguments)
		}
		if roles == "nm" {
			if got, want := request.Steps[0].Arguments, []string{"--help"}; !slices.Equal(got, want) {
				t.Fatalf("source-selected auxiliary request argv = %q, want %q", got, want)
			}
			if got := request.Outcome.Predicate.Operator; got != "stream-matches" {
				t.Fatalf("source-selected auxiliary grep predicate = %q, want stream-matches", got)
			}
			if got := request.Outcome.Predicate.Value; got != `(?i)^[^\n]*Acme Tools` {
				t.Fatalf("source-selected auxiliary grep expression = %q", got)
			}
		}
		if roles == "cc" && len(request.Steps) == 1 && slices.Equal(request.Steps[0].Arguments, []string{"-print-file-name=vendor-sdk"}) {
			if !request.Steps[0].StdoutExecrootRelative || request.Steps[0].StdoutFallbackPath != "vendor-sdk" {
				t.Fatalf("source-selected compiler path contract = %#v", request.Steps[0])
			}
		}
		if roles == "" && request.Outcome.Predicate != nil && request.Outcome.Predicate.Operator == "all" && len(request.Outcome.Predicate.Operands) == 2 && request.Outcome.Predicate.Operands[1].Operator == "execroot-exists" {
			operands := request.Outcome.Predicate.Operands
			if got, want := operands[1].Value, "${result:00000000.text}/headers/vendor-capability.h"; operands[1].Operator != "execroot-exists" || got != want {
				t.Fatalf("source-selected compiler file predicate = %#v, want execroot-exists %q", operands[1], want)
			}
			if got := operands[0]; got.Operator != "not" || len(got.Operands) != 1 || got.Operands[0].Operator != "result-path-fallback" || got.Operands[0].Result != "00000000" || got.Operands[0].Value != "" {
				t.Fatalf("source-selected compiler fallback predicate = %#v", got)
			}
			found["compiler-path-predicate"]++
		}
		if roles == "" && request.InputCount != 1 {
			t.Fatalf("pure dependent result request = %#v", request)
		}
	}
	for role, minimum := range map[string]int{
		"cc": 2, "primary:rustc": 1, "nm": 1,
		"bindgen,cc,rustc,script-runtime,scriptrun":               1,
		"bindgen,cc,ld,nm,objcopy,rustc,script-runtime,scriptrun": 1,
		"bindgen": 1, "": 1, "compiler-path-predicate": 1,
	} {
		if found[role] < minimum {
			t.Errorf("special plan has %d %q requests, want at least %d", found[role], role, minimum)
		}
	}
}

func TestLinuxProbeEvaluatorReplaysSourceSelectedPreprocessorTextDependencyWithoutArchitectureTable(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	discovery, fixture := newFixtureProbeEvaluator(t, 0, "unlisted-architecture", builder, nil, false)
	inner := shellProbeValue(t, discovery, `echo __SOURCE_SELECTED_TARGET_PROPERTY__ | /configured/gcc -mabi=call0 -E -P -`)
	outer := shellProbeValue(t, discovery, `{ test "`+inner+`" = 1; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	plan, err := builder.Plan(discovery.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || !slices.Equal(plan.Nodes[1].Inputs, []string{plan.Nodes[0].ID}) {
		t.Fatalf("preprocessor dependency plan = %#v", plan.Nodes)
	}
	preprocessorStep := plan.Requests[plan.Nodes[0].RequestID].Steps[0]
	if candidate := preprocessorStep.Candidate; candidate == nil || candidate.Policy != ProbeCandidatePolicyCC ||
		!slices.Equal(candidate.Base, []int{0}) || len(candidate.Conditional) != 0 {
		t.Fatalf("preprocessor text candidate ownership = %#v, argv=%q", candidate, preprocessorStep.Arguments)
	}
	truth := true
	results := probeResultMap{
		plan.Nodes[0].ID: {
			Schema: LinuxProbeResultSchema, NodeID: plan.Nodes[0].ID, RequestID: plan.Nodes[0].RequestID,
			Scope: fixture.scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "1",
			Steps: []ProbeStepResult{{Name: "preprocess", Status: "success", ExitCode: 0, Stdout: "1\n"}},
		},
		plan.Nodes[1].ID: {
			Schema: LinuxProbeResultSchema, NodeID: plan.Nodes[1].ID, RequestID: plan.Nodes[1].RequestID,
			Scope: fixture.scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "boolean", Boolean: &truth,
		},
	}
	replayBuilder, _ := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	replay, _ := newFixtureProbeEvaluator(t, 0, "unlisted-architecture", replayBuilder, results, false)
	replayInner := shellProbeValue(t, replay, `echo __SOURCE_SELECTED_TARGET_PROPERTY__ | /configured/gcc -mabi=call0 -E -P -`)
	replayOuter := shellProbeValue(t, replay, `{ test "`+replayInner+`" = 1; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	if replayInner != inner || replayOuter != outer {
		t.Fatalf("preprocessor replay atoms differ: %q/%q, want %q/%q", replayInner, replayOuter, inner, outer)
	}
	if got, err := replay.ResolveSymbolic(replayOuter); err != nil || got != "y" {
		t.Fatalf("preprocessor replay = %q, %v; want y", got, err)
	}
}

func TestLinuxProbeEvaluatorRecognizesLinux612And618SpecialInventory(t *testing.T) {
	tests := []struct {
		name, architecture, command string
		fixture                     int
	}{
		{"6.12 x86 stack 32", "x86", `{ /src/scripts/gcc-x86_32-has-stack-protector.sh /configured/gcc; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12 x86 stack 64", "x86", `{ /src/scripts/gcc-x86_64-has-stack-protector.sh /configured/gcc; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12 static can-link", "x86", `{ /src/scripts/cc-can-link.sh /configured/gcc -m64 -static; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12/6.18 PowerPC mprofile", "powerpc", `{ /src/arch/powerpc/tools/gcc-check-mprofile-kernel.sh /configured/gcc -mlittle-endian; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12/6.18 PowerPC fpatchable", "powerpc", `{ /src/arch/powerpc/tools/gcc-check-fpatchable-function-entry.sh /configured/gcc -mbig-endian; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12/6.18 s390 thunk extern", "s390", `{ /src/arch/s390/tools/gcc-thunk-extern.sh /configured/gcc; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12/6.18 objcopy identity", "x86", `{ ` + KbuildActionRoleToken(KbuildActionRoleAutoScope, "objcopy") + ` --version | head -n1 | grep -qv AcmeTools; } >/dev/null 2>&1 && echo "y" || echo "n"`, 0},
		{"6.12/6.18 Xtensa call0", "xtensa", `echo __XTENSA_CALL0_ABI__ | /configured/gcc -mabi=call0 -E -P - 2>/dev/null`, 0},
		{"6.18 Clang source", "arm64", `{ echo 'char tag[][4] __attribute__((__nonstring__)) = { };' | /configured/clang -fintegrated-as -x c - -c -o /dev/null -Werror; } >/dev/null 2>&1 && echo "y" || echo "n"`, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
			evaluator, _ := newFixtureProbeEvaluator(t, test.fixture, test.architecture, builder, nil, false)
			value := shellProbeValue(t, evaluator, test.command)
			if !linuxProbeSymbolPattern.MatchString(value) {
				t.Fatalf("inventory command returned %q, want symbolic result", value)
			}
			plan, err := builder.Plan(evaluator.References()...)
			if err != nil || len(plan.Nodes) == 0 {
				t.Fatalf("inventory plan = %#v, %v", plan, err)
			}
			if test.name == "6.12/6.18 objcopy identity" {
				request := plan.Requests[plan.Nodes[0].RequestID]
				if got, want := request.Steps[0].Tool, "objcopy"; got != want {
					t.Fatalf("source-selected auxiliary role = %q, want %q", got, want)
				}
				inverse := *request.Outcome.Predicate
				if inverse.Operator != "all" || len(inverse.Operands) != 2 || inverse.Operands[1].Operator != "not" || inverse.Operands[1].Operands[0].Value != `^[^\n]*AcmeTools` {
					t.Fatalf("source-selected inverted grep predicate = %#v", inverse)
				}
			}
		})
	}
}

func TestLinuxProbeSymbolicReplayBoundsDepthAndExpansion(t *testing.T) {
	newReplay := func(t *testing.T) *LinuxProbeEvaluator {
		t.Helper()
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
		if err != nil {
			t.Fatal(err)
		}
		evaluator, _ := testSymbolicProbeEvaluator(t, builder, probeResultMap{})
		return evaluator
	}

	t.Run("nested depth", func(t *testing.T) {
		evaluator := newReplay(t)
		token, err := evaluator.renderMakeText("strip", []string{"leaf"}, "leaf", linuxProbeMakeTextProtocolExact)
		if err != nil {
			t.Fatal(err)
		}
		for range maxLinuxProbeResolveDepth {
			token, err = evaluator.renderMakeText("strip", []string{token}, token, linuxProbeMakeTextProtocolExact)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := evaluator.ResolveSymbolic(token); err == nil || !strings.Contains(err.Error(), "exceeds depth") {
			t.Fatalf("deep symbolic replay error = %v", err)
		}
	})

	t.Run("flat graph", func(t *testing.T) {
		evaluator := newReplay(t)
		var symbolic, want strings.Builder
		for index := 0; index < 64; index++ {
			literal := fmt.Sprintf("flat-%02d", index)
			token, err := evaluator.renderMakeText("strip", []string{literal}, literal, linuxProbeMakeTextProtocolExact)
			if err != nil {
				t.Fatal(err)
			}
			symbolic.WriteString(token)
			want.WriteString(literal)
		}
		resolved, err := evaluator.ResolveSymbolic(symbolic.String())
		if err != nil || resolved != want.String() {
			t.Fatalf("flat symbolic replay = %q, %v; want %q", resolved, err, want.String())
		}
	})

	t.Run("concatenated expansion", func(t *testing.T) {
		evaluator := newReplay(t)
		largeX := strings.Repeat("x", 600<<10)
		largeY := strings.Repeat("y", 600<<10)
		first, err := evaluator.renderMakeText("strip", []string{largeX}, "", linuxProbeMakeTextProtocolUnusable)
		if err != nil {
			t.Fatal(err)
		}
		second, err := evaluator.renderMakeText("strip", []string{largeY}, "", linuxProbeMakeTextProtocolUnusable)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := evaluator.ResolveSymbolic(first + second); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
			t.Fatalf("amplified symbolic replay error = %v", err)
		}
	})

	t.Run("oversized entry", func(t *testing.T) {
		evaluator := newReplay(t)
		if _, err := evaluator.ResolveSymbolic(strings.Repeat("x", MaxProbeInterpolatedBytes+1)); err == nil || !strings.Contains(err.Error(), "value exceeds") {
			t.Fatalf("oversized symbolic replay error = %v", err)
		}
	})
}

func TestLinuxProbeScopeAdoptionBoundsRegistryGraphs(t *testing.T) {
	newPair := func() (*LinuxProbeEvaluator, *LinuxProbeEvaluator) {
		registry := newLinuxProbeSymbolRegistry()
		return &LinuxProbeEvaluator{
				scope: "host", symbols: map[string]linuxProbeSymbol{}, symbolRegistry: registry,
			}, &LinuxProbeEvaluator{
				scope: "host", symbols: map[string]linuxProbeSymbol{}, symbolRegistry: registry,
			}
	}

	t.Run("depth", func(t *testing.T) {
		publisher, consumer := newPair()
		token, err := publisher.renderMakeText("strip", []string{"leaf"}, "", linuxProbeMakeTextProtocolUnusable)
		if err != nil {
			t.Fatal(err)
		}
		for range maxLinuxProbeResolveDepth {
			token, err = publisher.renderMakeText("strip", []string{token}, "", linuxProbeMakeTextProtocolUnusable)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := consumer.adoptSymbol(token); err == nil || !strings.Contains(err.Error(), "scope adoption exceeds depth") {
			t.Fatalf("deep scope adoption error = %v", err)
		}
	})

	t.Run("unique nodes", func(t *testing.T) {
		publisher, consumer := newPair()
		var children strings.Builder
		for index := 0; index < maxLinuxProbeResolveNodes; index++ {
			token, err := publisher.renderMakeText(
				"strip", []string{fmt.Sprintf("leaf-%04d", index)}, "", linuxProbeMakeTextProtocolUnusable,
			)
			if err != nil {
				t.Fatal(err)
			}
			children.WriteString(token)
		}
		root, err := publisher.renderMakeText("strip", []string{children.String()}, "", linuxProbeMakeTextProtocolUnusable)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := consumer.adoptSymbol(root); err == nil || !strings.Contains(err.Error(), "scope adoption exceeds 4096 nodes") {
			t.Fatalf("wide scope adoption error = %v", err)
		}
	})
}

func TestProbeSymbolicValueLoweringRejectsNestedSelectionBeforeExpansion(t *testing.T) {
	evaluator := &LinuxProbeEvaluator{scope: "target", symbols: map[string]linuxProbeSymbol{}}
	value := "leaf"
	for depth := 0; depth < 13; depth++ {
		token := linuxProbeSymbolPrefix + fmt.Sprintf("%064x", depth+1)
		reference := ProbeReference{
			NodeID: fmt.Sprintf("node-%02d", depth), RequestID: fmt.Sprintf("request-%02d", depth),
			Scope: "target", Kind: "boolean",
		}
		evaluator.symbols[token] = linuxProbeSymbol{
			kind: "boolean", reference: reference,
			trueText: value, falseText: value,
		}
		value = token
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	if _, _, err := lowerer.value(value); err == nil || !strings.Contains(err.Error(), "work items") {
		t.Fatalf("nested symbolic lowering error = %v", err)
	}
}
