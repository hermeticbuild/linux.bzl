package kconfig

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func sourceShellWordsEvaluatorForTest(t *testing.T, scope string) (*LinuxProbeEvaluator, string, string) {
	t.Helper()
	evaluator := &LinuxProbeEvaluator{
		scope: scope, symbols: map[string]linuxProbeSymbol{}, symbolRegistry: newLinuxProbeSymbolRegistry(),
	}
	for index, text := range []string{"-flto", "'-DOTHER=\"two words\"'"} {
		token := linuxProbeSymbolPrefix + strings.Repeat(fmt.Sprint(index+1), 64)
		reference := ProbeReference{
			NodeID: strings.Repeat(fmt.Sprint(index+3), 64), RequestID: strings.Repeat(fmt.Sprint(index+5), 64),
			Scope: scope, Kind: "boolean",
		}
		if err := evaluator.publishSymbol(token, linuxProbeSymbol{kind: "boolean", reference: reference, trueText: text}); err != nil {
			t.Fatal(err)
		}
	}
	return evaluator, linuxProbeSymbolPrefix + strings.Repeat("1", 64), linuxProbeSymbolPrefix + strings.Repeat("2", 64)
}

func TestProbeSourceShellWordsDefersQuotingAndRootsUntilAfterCompleteMake(t *testing.T) {
	evaluator, lto, other := sourceShellWordsEvaluatorForTest(t, "target")
	value := "-nostdinc -DKBUILD_MODFILE='\"" + compactKbuildActionObjectTreeMarker + "/asm-offsets\"' " + lto + " " + other
	expression, err := evaluator.renderMakeTextWithProtocolTransforms(
		"filter-out", []string{lto, value}, value,
		[]linuxProbeMakeTextProtocolTransform{{function: "filter-out", arguments: []string{lto, ""}, inputArgument: 1}},
		linuxProbeMakeTextProtocolExact,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := evaluator.renderSourceShellWords(expression)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{compilerProbeSourceShellWords: evaluator.renderSourceShellWords}
	template := KbuildActionRoleToken("target", "cc") + " " + expression + " -fverbose-asm -S -o asm-offsets.s asm-offsets.c"
	captured, opaque, err := compactKbuildWrapCompilerProbeSourceShellWords(metadata, template)
	if err != nil || opaque != "" || !strings.Contains(captured, wrapped) || strings.Contains(captured, expression) {
		t.Fatalf("capture = %q, %q, %v", captured, opaque, err)
	}
	var originalRequest *ProbeRequest
	var originalRequestID string
	var originalNodeID string
	var originalCanonical []byte
	var originalDependencies []ProbeReference
	stdin, err := compilerIntrinsicIntegerSource("__has_attribute", "__retain__")
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range []bool{false, true} {
		lowerer := newProbeSymbolicValueLowerer(evaluator)
		base, conditional, groups, err := lowerer.arguments([]string{wrapped, "-fverbose-asm"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(base, []string{"", "-fverbose-asm"}) || len(conditional) != 0 || len(groups) != 1 ||
			groups[0].Index != 0 || groups[0].Mode != ProbeArgumentFragmentsModeSourceShellWords {
			t.Fatalf("lowered argv lost source mode/order: %#v %#v %#v", base, conditional, groups)
		}
		if len(lowerer.dependencies) != 2 || lowerer.dependencies[0] != evaluator.symbols[lto].reference ||
			lowerer.dependencies[1] != evaluator.symbols[other].reference {
			t.Fatalf("dependencies = %#v", lowerer.dependencies)
		}
		request := ProbeRequest{
			Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
			Steps: []ProbeStep{{Name: "compiler-intrinsic-integer", Tool: "cc",
				Arguments: append(slices.Clone(base), "-E", "-P", "-x", "c", "-"), ArgumentFragments: groups, Stdin: stdin,
				Candidate: &ProbeCandidateArguments{
					Policy: ProbeCandidatePolicyCC, Projection: ProbeCandidateProjectionCompilerIntrinsic, Base: []int{0, 1},
				},
			}},
			Outcome: ProbeOutcome{Kind: "text", Step: "compiler-intrinsic-integer", Stream: "stdout"},
		}
		canonical, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		requestID, err := request.ID()
		if err != nil {
			t.Fatal(err)
		}
		inputs := make([]string, len(lowerer.dependencies))
		for index, dependency := range lowerer.dependencies {
			inputs[index] = dependency.NodeID
		}
		node := ProbePlanNode{Scope: "target", RequestID: requestID, Inputs: inputs}
		nodeID := node.ContentID()
		if originalRequest == nil {
			originalRequest = &request
			originalRequestID = requestID
			originalNodeID = nodeID
			originalCanonical = slices.Clone(canonical)
			originalDependencies = slices.Clone(lowerer.dependencies)
		} else if originalRequestID != requestID || originalNodeID != nodeID || !bytes.Equal(originalCanonical, canonical) ||
			!reflect.DeepEqual(*originalRequest, request) || !slices.Equal(originalDependencies, lowerer.dependencies) {
			t.Fatal("selected result changed retained request or dependency identity")
		}
		rendered, err := RenderProbeDependencyFragments(groups[0].Fragments, map[string]ProbeResult{
			"00000000": dependencyBooleanResult(selected), "00000001": dependencyBooleanResult(selected),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rendered, "-DKBUILD_MODFILE='\""+compactKbuildActionObjectTreeMarker+"/asm-offsets\"'") ||
			strings.Contains(rendered, "-flto") {
			t.Fatalf("Make transform prematurely cooked source bytes: %q", rendered)
		}
		argv, err := ParseProbeSourceShellWords(rendered)
		want := []string{"-nostdinc", `-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/asm-offsets"`}
		if selected {
			want = append(want, `-DOTHER="two words"`)
		}
		if err != nil || !slices.Equal(argv, want) {
			t.Fatalf("source argv = %q, %v; want %q", argv, err, want)
		}
	}
}

func TestProbeSourceShellWordsDoesNotRewriteRootsBeforeMakeTransforms(t *testing.T) {
	evaluator, token, _ := sourceShellWordsEvaluatorForTest(t, "target")
	value := "-DROOT='\"" + compactKbuildActionObjectTreeMarker + "/file\"' " + token
	expression, err := evaluator.renderMakeTextWithProtocolTransforms(
		"subst", []string{compactKbuildActionObjectTreeMarker, "/literal", value}, value,
		[]linuxProbeMakeTextProtocolTransform{{function: "subst", arguments: []string{compactKbuildActionObjectTreeMarker, "/literal", ""}, inputArgument: 2}},
		linuxProbeMakeTextProtocolExact,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := evaluator.renderSourceShellWords(expression)
	if err != nil {
		t.Fatal(err)
	}
	_, _, groups, err := newProbeSymbolicValueLowerer(evaluator).arguments([]string{wrapped})
	if err != nil || len(groups) != 1 {
		t.Fatalf("lower = %#v, %v", groups, err)
	}
	rendered, err := RenderProbeDependencyFragments(groups[0].Fragments, map[string]ProbeResult{
		"00000000": dependencyBooleanResult(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := ParseProbeSourceShellWords(rendered)
	if err != nil || !slices.Equal(argv, []string{`-DROOT="/literal/file"`}) {
		t.Fatalf("source transform observed rewritten root: %q, %v", argv, err)
	}
}

func TestProbeSourceShellWordsCaptureRequiresExactSourceOccurrence(t *testing.T) {
	evaluator, token, _ := sourceShellWordsEvaluatorForTest(t, "target")
	for _, argument := range []string{"'" + token + "'", `"` + token + `"`, "prefix" + token, token + "suffix", token + token} {
		calls := 0
		metadata := &CompactMetadata{compilerProbeSourceShellWords: func(value string) (string, error) {
			calls++
			return evaluator.renderSourceShellWords(value)
		}}
		template := KbuildActionRoleToken("target", "cc") + " " + token + " " + argument + " -c source.c"
		got, opaque, err := compactKbuildWrapCompilerProbeSourceShellWords(metadata, template)
		if err != nil || opaque == "" || got != template || calls != 0 {
			t.Fatalf("ambiguous capture published a prefix: %q, %q, %v, %d calls", got, opaque, err, calls)
		}
	}
	metadata := &CompactMetadata{compilerProbeSourceShellWords: evaluator.renderSourceShellWords}
	template := KbuildActionRoleToken("target", "cc") + ` -DSTATIC='"literal"' ` + token + " -c source.c"
	got, opaque, err := compactKbuildWrapCompilerProbeSourceShellWords(metadata, template)
	if err != nil || opaque != "" || !strings.Contains(got, `-DSTATIC='"literal"'`) {
		t.Fatalf("static source argv was re-quoted: %q, %q, %v", got, opaque, err)
	}
	commands, err := parseCompactKbuildRecipe(got, compactKbuildAutomaticContext{})
	if err != nil || len(commands) != 1 || commands[0].arguments[0] != `-DSTATIC="literal"` {
		t.Fatalf("static argument was not parsed exactly once: %#v, %v", commands, err)
	}
	if got, opaque, err := compactKbuildWrapCompilerProbeSourceShellWords(nil, template); err != nil || opaque == "" || got != template {
		t.Fatalf("missing workload authority: %q, %q, %v", got, opaque, err)
	}
}

func TestProbeSourceShellWordsRejectsNonExactOrNonArgvUse(t *testing.T) {
	evaluator, token, _ := sourceShellWordsEvaluatorForTest(t, "target")
	wrapped, err := evaluator.renderSourceShellWords(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "literal", "prefix" + token, wrapped, linuxProbeSymbolPrefix + strings.Repeat("f", 64)} {
		if _, err := evaluator.renderSourceShellWords(invalid); err == nil {
			t.Fatalf("accepted non-original expression %q", invalid)
		}
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	if _, _, err := lowerer.value(wrapped); err == nil {
		t.Fatal("source-shell annotation was accepted as stdin/environment text")
	}
	if _, _, _, err := lowerer.arguments([]string{"prefix" + wrapped}); err == nil {
		t.Fatal("source-shell annotation was accepted inside cooked argv")
	}
	for _, mode := range []linuxProbeMakeTextProtocolMode{linuxProbeMakeTextProtocolArgvWords, linuxProbeMakeTextProtocolUnusable} {
		expression, err := evaluator.renderMakeText("strip", []string{token}, token, mode)
		if err != nil {
			t.Fatal(err)
		}
		annotated, err := evaluator.renderSourceShellWords(expression)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := newProbeSymbolicValueLowerer(evaluator).arguments([]string{annotated}); err == nil {
			t.Fatalf("accepted nonexact recipe expression %s", mode)
		}
	}
	base, _, groups, err := newProbeSymbolicValueLowerer(evaluator).arguments([]string{`-DSTATIC="literal"`})
	if err != nil || len(groups) != 0 || !slices.Equal(base, []string{`-DSTATIC="literal"`}) {
		t.Fatalf("ordinary cooked argv changed: %q %#v %v", base, groups, err)
	}
}

func TestProbeSourceShellWordsPreservesRegistryScopeAuthority(t *testing.T) {
	target, token, _ := sourceShellWordsEvaluatorForTest(t, "target")
	wrapped, err := target.renderSourceShellWords(token)
	if err != nil {
		t.Fatal(err)
	}
	host := &LinuxProbeEvaluator{scope: "host", symbols: map[string]linuxProbeSymbol{}, symbolRegistry: target.symbolRegistry}
	if _, _, err := host.adoptSymbol(wrapped); err == nil {
		t.Fatal("source-shell annotation hid a target dependency from host scope")
	}
	replay := &LinuxProbeEvaluator{scope: "target", symbols: map[string]linuxProbeSymbol{}, symbolRegistry: target.symbolRegistry}
	if _, exists, err := replay.adoptSymbol(wrapped); err != nil || !exists {
		t.Fatalf("target adoption: %t, %v", exists, err)
	}
	first := newProbeSymbolicValueLowerer(target)
	second := newProbeSymbolicValueLowerer(replay)
	_, _, left, leftErr := first.arguments([]string{wrapped})
	_, _, right, rightErr := second.arguments([]string{wrapped})
	if leftErr != nil || rightErr != nil || !reflect.DeepEqual(left, right) || !slices.Equal(first.dependencies, second.dependencies) {
		t.Fatalf("scope adoption changed exact fragment/dependency identity: %v, %v", leftErr, rightErr)
	}
}
