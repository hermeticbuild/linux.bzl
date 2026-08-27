package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const linearFilterCompilerFixture = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
`

func writeKbuildProbeResultsByNode(
	t *testing.T,
	plan *ProbePlan,
	resultFor func(int, ProbePlanNode) bool,
) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for index, node := range plan.Nodes {
		value := resultFor(index, node)
		status, exitCode := "failure", 1
		if value {
			status, exitCode = "success", 0
		}
		request := plan.Requests[node.RequestID]
		stepName := "probe"
		if len(request.Steps) != 0 {
			stepName = request.Steps[0].Name
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "boolean", Boolean: &value,
			Steps: []ProbeStepResult{{Name: stepName, Status: status, ExitCode: exitCode}},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"target": root}
}

func writeKbuildTextProbeResultsByNode(
	t *testing.T,
	plan *ProbePlan,
	resultFor func(int, ProbePlanNode) string,
) map[string]string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for index, node := range plan.Nodes {
		value := resultFor(index, node)
		request := plan.Requests[node.RequestID]
		stepName := "probe"
		if len(request.Steps) != 0 {
			stepName = request.Steps[0].Name
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Kind: "text", Text: value,
			Steps: []ProbeStepResult{{Name: stepName, Status: "success", ExitCode: 0, Stdout: value + "\n"}},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"target": root}
}

func TestKbuildWordwiseFilterKeepsIndependentSelectionsLinear(t *testing.T) {
	const selectionCount = 24
	var source strings.Builder
	source.WriteString(linearFilterCompilerFixture)
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, "choice_%02d := $(call cc-option,-fflag%02d)\n", index, index)
	}
	source.WriteString("FLAGS :=")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, " $(choice_%02d)", index)
	}
	source.WriteString("\nFILTERED := $(filter -fflag%,$(FLAGS))\nall: $(FILTERED)\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source.String()), "Makefile", options, "")
		if err != nil {
			return "", err
		}
		if len(parsed.Rules) != 1 {
			return "", fmt.Errorf("parsed %d rules, want one", len(parsed.Rules))
		}
		return strings.Join(parsed.Rules[0].Prerequisites, " "), nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	discoveryScopes := evaluatedScopes
	if len(discovery.Plan.Nodes) != selectionCount {
		t.Fatalf("filter plan has %d nodes, want %d", len(discovery.Plan.Nodes), selectionCount)
	}
	words := strings.Fields(discovery.Value)
	if len(words) != selectionCount {
		t.Fatalf("discovery filter has %d words, want %d: %q", len(words), selectionCount, discovery.Value)
	}
	tableEntries := 0
	for index, word := range words {
		evaluator, symbol, ok := discoveryScopes.symbolOwner(word)
		if !ok || evaluator.scope != "target" || symbol.kind != "boolean" {
			t.Fatalf("filter word %d = %q, symbol = %#v, owner = %#v", index, word, symbol, evaluator)
		}
		if symbol.reference.NodeID != discovery.Plan.Nodes[index].ID {
			t.Fatalf("filter word %d input = %q, want node %q", index, symbol.reference.NodeID, discovery.Plan.Nodes[index].ID)
		}
		if symbol.falseText != "" || symbol.trueText != fmt.Sprintf("-fflag%02d", index) {
			t.Fatalf("filter word %d values = %q/%q", index, symbol.falseText, symbol.trueText)
		}
		tableEntries += 2
	}
	if tableEntries != 2*selectionCount {
		t.Fatalf("filter selection representation has %d entries, want linear %d", tableEntries, 2*selectionCount)
	}

	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
		return index%2 == 0
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	wantWords := make([]string, 0, selectionCount/2)
	for index := 0; index < selectionCount; index += 2 {
		wantWords = append(wantWords, fmt.Sprintf("-fflag%02d", index))
	}
	if replay.Value != strings.Join(wantWords, " ") {
		t.Fatalf("filter replay = %q, want %q", replay.Value, strings.Join(wantWords, " "))
	}
	if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
		t.Fatalf("replay plan has %d nodes, want %d", len(replay.Plan.Nodes), len(discovery.Plan.Nodes))
	}
	for index := range discovery.Plan.Nodes {
		if replay.Plan.Nodes[index].ID != discovery.Plan.Nodes[index].ID ||
			replay.Plan.Nodes[index].RequestID != discovery.Plan.Nodes[index].RequestID ||
			!slices.Equal(replay.Plan.Nodes[index].Inputs, discovery.Plan.Nodes[index].Inputs) {
			t.Fatalf("replay node %d = %#v, want %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
		}
	}
}

func TestKbuildWordwiseFilterKeepsNestedSelectionsLocal(t *testing.T) {
	const selectionCount = 14
	var source strings.Builder
	source.WriteString(linearFilterCompilerFixture)
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, "inner_%02d := $(call cc-option,-finner%02d)\n", index, index)
		fmt.Fprintf(&source, "choice_%02d := $(call cc-option,-fouter%02d,$(inner_%02d))\n", index, index, index)
	}
	source.WriteString("FLAGS :=")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, " $(choice_%02d)", index)
	}
	source.WriteString("\nFILTERED := $(filter -f%,$(FLAGS))\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"FILTERED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source.String()), "Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["FILTERED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2*selectionCount {
		t.Fatalf("nested filter plan has %d nodes, want %d", len(discovery.Plan.Nodes), 2*selectionCount)
	}
	if matches := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1); len(matches) != 1 || matches[0] != discovery.Value {
		t.Fatalf("nested filter discovery value = %q, want one exact protocol token", discovery.Value)
	}

	// Every inner probe succeeds and every dependent outer probe fails, so the
	// replay must select and filter all nested fallback flags.
	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(_ int, node ProbePlanNode) bool {
		request := discovery.Plan.Requests[node.RequestID]
		data, err := request.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(data), "-finner")
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, selectionCount)
	for index := range selectionCount {
		want[index] = fmt.Sprintf("-finner%02d", index)
	}
	if replay.Value != strings.Join(want, " ") {
		t.Fatalf("nested filter replay = %q, want %q", replay.Value, strings.Join(want, " "))
	}
}

func TestKbuildDynamicFilterPatternsKeepVDSOFlagsLinearAndExact(t *testing.T) {
	const selectionCount = 26
	var source strings.Builder
	source.WriteString(linearFilterCompilerFixture)
	for index := 0; index < selectionCount; index++ {
		flag := fmt.Sprintf("-fflag%02d", index)
		if index == 0 {
			flag = "-fdrop%"
		}
		fmt.Fprintf(&source, "choice_%02d := $(call cc-option,%s)\n", index, flag)
	}
	for index, variable := range []string{
		"PADDING_CFLAGS", "CC_FLAGS_LTO", "CC_FLAGS_CFI", "RANDSTRUCT_CFLAGS",
		"KSTACK_ERASE_CFLAGS", "GCC_PLUGINS_CFLAGS", "RETPOLINE_CFLAGS",
	} {
		fmt.Fprintf(&source, "%s := $(choice_%02d)\n", variable, index)
	}
	source.WriteString("KBUILD_CFLAGS := -fdrop-literal")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, " $(choice_%02d)", index)
	}
	source.WriteString(`
FILTERED := $(filter-out $(PADDING_CFLAGS) $(CC_FLAGS_LTO) $(CC_FLAGS_CFI) $(RANDSTRUCT_CFLAGS) $(KSTACK_ERASE_CFLAGS) $(GCC_PLUGINS_CFLAGS) $(RETPOLINE_CFLAGS),$(KBUILD_CFLAGS))
`)

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"FILTERED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source.String()), "arch/x86/entry/vdso/Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["FILTERED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != selectionCount {
		t.Fatalf("VDSO filter plan has %d nodes, want %d", len(discovery.Plan.Nodes), selectionCount)
	}
	evaluator, symbol, ok := evaluatedScopes.symbolOwner(discovery.Value)
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil {
		t.Fatalf("VDSO filter discovery value = %q, symbol = %#v", discovery.Value, symbol)
	}
	makeText := symbol.makeText
	if makeText.function != "filter-out" || makeText.protocolMode != linuxProbeMakeTextProtocolExact ||
		len(makeText.protocolTransforms) != 1 || makeText.protocolTransforms[0].function != "filter-out" ||
		makeText.protocolTransforms[0].inputArgument != 1 {
		t.Fatalf("VDSO filter protocol = %#v", makeText)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(discovery.Value)
	if err != nil || !dynamic || len(fragments) != 1 {
		t.Fatalf("VDSO filter lowering = %#v, dynamic=%v, error=%v", fragments, dynamic, err)
	}
	aggregate := fragments[0]
	if len(aggregate.Fragments) == 0 || len(aggregate.Transforms) != 1 ||
		aggregate.Transforms[0].Function != "filter-out" ||
		len(aggregate.Transforms[0].ArgumentFragments) != 1 ||
		aggregate.Transforms[0].ArgumentFragments[0].Index != 0 {
		t.Fatalf("VDSO filter aggregate = %#v", aggregate)
	}
	if len(lowerer.dependencies) != selectionCount {
		t.Fatalf("VDSO filter lowering has %d dependencies, want %d", len(lowerer.dependencies), selectionCount)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Steps:   []ProbeStep{{Name: "consume", Tool: "cc", Arguments: []string{""}, ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: fragments}}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("VDSO filter request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name     string
		selected func(int) bool
		want     []string
	}{
		{
			name:     "even including wildcard pattern",
			selected: func(index int) bool { return index%2 == 0 },
			want:     []string{"-fflag08", "-fflag10", "-fflag12", "-fflag14", "-fflag16", "-fflag18", "-fflag20", "-fflag22", "-fflag24"},
		},
		{
			name:     "odd",
			selected: func(index int) bool { return index%2 != 0 },
			want:     []string{"-fdrop-literal", "-fflag07", "-fflag09", "-fflag11", "-fflag13", "-fflag15", "-fflag17", "-fflag19", "-fflag21", "-fflag23", "-fflag25"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				return test.selected(index)
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(replay.Value); !slices.Equal(got, test.want) {
				t.Fatalf("VDSO filter replay = %q, want %q", got, test.want)
			}
		})
	}
}

func TestKbuildOuterFilterPreservesEmbeddedDynamicFilterProtocol(t *testing.T) {
	const source = linearFilterCompilerFixture + `
pattern := $(call cc-option,-fdrop%)
value := $(call cc-option,-fdrop-other)
INNER := $(filter-out $(pattern),-fdrop-literal $(value) -fkeep)
OUTER := $(filter-out -fkeep,prefix $(INNER) suffix)
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"OUTER"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "arch/x86/entry/vdso/Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["OUTER"], nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("nested dynamic filter plan = %#v, want two probes", discovery.Plan.Nodes)
	}
	evaluator, outer, ok := evaluatedScopes.symbolOwner(discovery.Value)
	if !ok || outer.kind != "make-text" || outer.makeText == nil || len(outer.makeText.protocolTransforms) != 1 {
		t.Fatalf("outer dynamic filter symbol = %#v", outer)
	}
	nestedTokens := linuxProbeSymbolPattern.FindAllString(outer.makeText.protocolValue, -1)
	if len(nestedTokens) != 1 {
		t.Fatalf("outer protocol source = %q, want one nested protocol", outer.makeText.protocolValue)
	}
	_, inner, ok := evaluatedScopes.symbolOwner(nestedTokens[0])
	if !ok || inner.makeText == nil || len(inner.makeText.protocolTransforms) != 1 {
		t.Fatalf("inner dynamic filter protocol was discarded: %#v", inner)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, _, err := lowerer.value(discovery.Value)
	if err != nil {
		t.Fatal(err)
	}
	transformCount := 0
	var inspect func([]ProbeValueFragment)
	inspect = func(values []ProbeValueFragment) {
		for _, fragment := range values {
			transformCount += len(fragment.Transforms)
			inspect(fragment.Fragments)
			for _, transform := range fragment.Transforms {
				for _, group := range transform.ArgumentFragments {
					inspect(group.Fragments)
				}
			}
		}
	}
	inspect(fragments)
	if transformCount != 2 {
		t.Fatalf("outer lowering retained %d transforms, want inner and outer: %#v", transformCount, fragments)
	}

	for _, test := range []struct {
		name           string
		pattern, value bool
		want           string
	}{
		{name: "cross-match", pattern: true, value: true, want: "prefix suffix"},
		{name: "pattern absent", pattern: false, value: true, want: "prefix -fdrop-literal -fdrop-other suffix"},
		{name: "value absent", pattern: true, value: false, want: "prefix suffix"},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				if index == 0 {
					return test.pattern
				}
				return test.value
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("outer dynamic filter replay = %q, want %q", replay.Value, test.want)
			}
		})
	}
}

func TestKbuildIntermediateTargetsChainsDynamicFilterIntoPatsubstProtocol(t *testing.T) {
	// scripts/Makefile.build derives generated intermediates with this exact
	// filter -> patsubst shape. Make the target list itself compiler-dependent
	// so discovery has to retain and compose the complete runtime protocol.
	const source = linearFilterCompilerFixture + `
dynamic_pattern := $(subst -fprobe-pattern,%.asn1.o,$(call cc-option,-fprobe-pattern))
dynamic_target := $(subst -fprobe-target,generated.asn1.o,$(call cc-option,-fprobe-target))
targets := $(filter $(dynamic_pattern),literal.asn1.o $(dynamic_target) unrelated.o)
intermediate_targets = $(foreach sfx,$(2), \
	$(patsubst %$(strip $(1)),%$(sfx), \
		$(filter %$(strip $(1)),$(targets))))
OUT := $(call intermediate_targets,.asn1.o,.asn1.c .asn1.h)
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"OUT"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["OUT"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("intermediate-target plan = %#v, want pattern and target probes", discovery.Plan.Nodes)
	}
	tokens := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)
	if len(tokens) != 2 {
		t.Fatalf("intermediate-target discovery value = %q, want one protocol token per suffix", discovery.Value)
	}
	for index, token := range tokens {
		evaluator, symbol, ok := evaluatedScopes.symbolOwner(token)
		if !ok || evaluator.scope != "target" || symbol.kind != "make-text" || symbol.makeText == nil {
			t.Fatalf("intermediate-target token %d = %q, symbol = %#v, owner = %#v", index, token, symbol, evaluator)
		}
		makeText := symbol.makeText
		if makeText.protocolMode != linuxProbeMakeTextProtocolExact || makeText.function != "patsubst" || len(makeText.protocolTransforms) != 3 {
			t.Fatalf("intermediate-target protocol %d = %#v", index, makeText)
		}
		gotFunctions := make([]string, len(makeText.protocolTransforms))
		gotInputs := make([]int, len(makeText.protocolTransforms))
		for transformIndex, transform := range makeText.protocolTransforms {
			gotFunctions[transformIndex] = transform.function
			gotInputs[transformIndex] = transform.inputArgument
		}
		if want := []string{"filter", "filter", "patsubst"}; !slices.Equal(gotFunctions, want) {
			t.Fatalf("intermediate-target protocol %d functions = %q, want %q", index, gotFunctions, want)
		}
		if want := []int{1, 1, 2}; !slices.Equal(gotInputs, want) {
			t.Fatalf("intermediate-target protocol %d inputs = %v, want %v", index, gotInputs, want)
		}
	}

	evaluator, _, ok := evaluatedScopes.symbolOwner(tokens[0])
	if !ok {
		t.Fatalf("no symbolic evaluator owns %q", tokens[0])
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(discovery.Value)
	if err != nil || !dynamic || len(fragments) == 0 {
		t.Fatalf("intermediate-target lowering = %#v, dynamic=%v, error=%v", fragments, dynamic, err)
	}
	if len(lowerer.dependencies) != 2 {
		t.Fatalf("intermediate-target lowering dependencies = %#v, want two", lowerer.dependencies)
	}
	var functions []string
	var inspect func([]ProbeValueFragment)
	inspect = func(values []ProbeValueFragment) {
		for _, fragment := range values {
			inspect(fragment.Fragments)
			for _, transform := range fragment.Transforms {
				for _, group := range transform.ArgumentFragments {
					inspect(group.Fragments)
				}
				functions = append(functions, transform.Function)
			}
		}
	}
	inspect(fragments)
	if want := []string{"filter", "filter", "patsubst", "filter", "filter", "patsubst"}; !slices.Equal(functions, want) {
		t.Fatalf("intermediate-target runtime transform order = %q, want %q", functions, want)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: fragments}},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("intermediate-target request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name                   string
		pattern, dynamicTarget bool
		want                   string
	}{
		{
			name: "pattern and dynamic target", pattern: true, dynamicTarget: true,
			want: "literal.asn1.c generated.asn1.c literal.asn1.h generated.asn1.h",
		},
		{
			name: "literal target only", pattern: true, dynamicTarget: false,
			want: "literal.asn1.c literal.asn1.h",
		},
		{name: "pattern absent", pattern: false, dynamicTarget: true, want: " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				if index == 0 {
					return test.pattern
				}
				return test.dynamicTarget
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("intermediate-target replay = %q, want %q", replay.Value, test.want)
			}
		})
	}
}

func TestKbuildRustPathComponentTargetsChainFilterIntoPatsubstProtocol(t *testing.T) {
	// rust/Makefile discovers two procedural-macro filenames as safe path
	// components. scripts/Makefile.build prefixes all always-y entries with the
	// object directory before its static filter -> patsubst intermediate-target
	// chain observes the mixed target list.
	const source = `
libmacros_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name macros --crate-type proc-macro - </dev/null)
libpin_init_internal_name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name pin_init_internal --crate-type proc-macro - </dev/null)
always-y := exports_core_generated.h $(libmacros_name) $(libpin_init_internal_name)
always-y := $(addprefix rust/,$(always-y))
targets := rust/core.o rust/parser.asn1.o $(always-y) rust/unrelated.o
intermediate_targets = $(foreach sfx, $(2), \
				$(patsubst %$(strip $(1)),%$(sfx), \
					$(filter %$(strip $(1)), $(targets))))
OUT := $(call intermediate_targets, .asn1.o, .asn1.c .asn1.h)
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	opts := KbuildProbeWorkloadOptions{Target: target}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"RUSTC": opts.Target.Tools["rustc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"OUT"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["OUT"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("Rust intermediate-target plan = %#v, want two rustc text probes", discovery.Plan.Nodes)
	}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if request.Outcome.Kind != "text" || !request.Outcome.PathComponent {
			t.Fatalf("Rust filename request outcome = %#v, want path-component text", request.Outcome)
		}
	}
	tokens := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)
	if len(tokens) != 2 {
		t.Fatalf("Rust intermediate-target discovery value = %q, want one protocol token per suffix", discovery.Value)
	}
	for index, token := range tokens {
		evaluator, symbol, ok := evaluatedScopes.symbolOwner(token)
		if !ok || evaluator.scope != "target" || symbol.kind != "make-text" || symbol.makeText == nil {
			t.Fatalf("Rust intermediate-target token %d = %q, symbol = %#v, owner = %#v", index, token, symbol, evaluator)
		}
		makeText := symbol.makeText
		if makeText.function != "patsubst" || makeText.protocolMode != linuxProbeMakeTextProtocolExact || len(makeText.protocolTransforms) != 2 {
			t.Fatalf("Rust intermediate-target protocol %d = %#v", index, makeText)
		}
		gotFunctions := []string{makeText.protocolTransforms[0].function, makeText.protocolTransforms[1].function}
		gotInputs := []int{makeText.protocolTransforms[0].inputArgument, makeText.protocolTransforms[1].inputArgument}
		if want := []string{"filter", "patsubst"}; !slices.Equal(gotFunctions, want) {
			t.Fatalf("Rust intermediate-target protocol %d functions = %q, want %q", index, gotFunctions, want)
		}
		if want := []int{1, 2}; !slices.Equal(gotInputs, want) {
			t.Fatalf("Rust intermediate-target protocol %d inputs = %v, want %v", index, gotInputs, want)
		}
		if !strings.Contains(makeText.protocolValue, "rust/LINUX_BZL_PROBE_") {
			t.Fatalf("Rust intermediate-target protocol %d lost embedded path-component word: %q", index, makeText.protocolValue)
		}
	}

	evaluator, _, ok := evaluatedScopes.symbolOwner(tokens[0])
	if !ok {
		t.Fatalf("no symbolic evaluator owns %q", tokens[0])
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(discovery.Value)
	if err != nil || !dynamic || len(fragments) == 0 {
		t.Fatalf("Rust intermediate-target lowering = %#v, dynamic=%v, error=%v", fragments, dynamic, err)
	}
	if len(lowerer.dependencies) != 2 {
		t.Fatalf("Rust intermediate-target dependencies = %#v, want two", lowerer.dependencies)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: fragments}},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("Rust intermediate-target request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name            string
		macros, pinInit string
		want            string
	}{
		{
			name: "ordinary proc-macro names", macros: "libmacros.so", pinInit: "libpin_init_internal.so",
			want: "rust/parser.asn1.c rust/parser.asn1.h",
		},
		{
			name: "one dynamic intermediate", macros: "libmacros.asn1.o", pinInit: "libpin_init_internal.so",
			want: "rust/parser.asn1.c rust/libmacros.asn1.c rust/parser.asn1.h rust/libmacros.asn1.h",
		},
		{
			name: "both dynamic intermediates", macros: "libmacros.asn1.o", pinInit: "libpin_init_internal.asn1.o",
			want: "rust/parser.asn1.c rust/libmacros.asn1.c rust/libpin_init_internal.asn1.c rust/parser.asn1.h rust/libmacros.asn1.h rust/libpin_init_internal.asn1.h",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildTextProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) string {
				if index == 0 {
					return test.macros
				}
				return test.pinInit
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("Rust intermediate-target replay = %q, want %q", replay.Value, test.want)
			}
		})
	}
}

func TestKbuildPatsubstDoesNotChainAcrossNonWhitespacePrefix(t *testing.T) {
	const source = linearFilterCompilerFixture + `
dynamic_pattern := $(subst -fprobe-pattern,%.asn1.o,$(call cc-option,-fprobe-pattern))
dynamic_target := $(subst -fprobe-target,generated.asn1.o,$(call cc-option,-fprobe-target))
FILTERED := $(filter $(dynamic_pattern),literal.asn1.o $(dynamic_target) unrelated.o)
OUT := $(patsubst %.asn1.o,%.asn1.c,prefix$(FILTERED))
`
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"OUT"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Makefile.build", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["OUT"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("prefixed patsubst plan = %#v, want pattern and target probes", discovery.Plan.Nodes)
	}
	evaluator, outer, ok := evaluatedScopes.symbolOwner(discovery.Value)
	if !ok || evaluator.scope != "target" || outer.kind != "make-text" || outer.makeText == nil {
		t.Fatalf("prefixed patsubst discovery value = %q, symbol = %#v, owner = %#v", discovery.Value, outer, evaluator)
	}
	if outer.makeText.function != "patsubst" ||
		outer.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable ||
		len(outer.makeText.protocolTransforms) != 0 {
		t.Fatalf("prefixed patsubst incorrectly chained protocol = %#v", outer.makeText)
	}
	if nested := linuxProbeSymbolPattern.FindAllString(outer.makeText.arguments[2], -1); len(nested) != 1 {
		t.Fatalf("prefixed patsubst argument = %q, want retained nested protocol", outer.makeText.arguments[2])
	}

	for _, test := range []struct {
		name                   string
		pattern, dynamicTarget bool
		want                   string
	}{
		{
			name: "pattern and dynamic target", pattern: true, dynamicTarget: true,
			want: "prefixliteral.asn1.c generated.asn1.c",
		},
		{
			name: "literal target only", pattern: true, dynamicTarget: false,
			want: "prefixliteral.asn1.c",
		},
		{name: "pattern absent", pattern: false, dynamicTarget: true, want: "prefix"},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				if index == 0 {
					return test.pattern
				}
				return test.dynamicTarget
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("prefixed patsubst replay = %q, want %q", replay.Value, test.want)
			}
		})
	}
}

func TestKbuildMakeCmdSubstRunsAfterVDSODynamicFilter(t *testing.T) {
	roleToken := KbuildActionRoleToken("target", "cc")
	source := linearFilterCompilerFixture + fmt.Sprintf(`
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
PADDING_CFLAGS := $(call cc-option,-fdrop%%)
CC_FLAGS_LTO := $(call cc-option,-fdrop-other)
KBUILD_CFLAGS := -fdrop-literal $(CC_FLAGS_LTO) -fkeep
VDSO_CFLAGS := $(filter-out $(PADDING_CFLAGS),$(KBUILD_CFLAGS))
cmd_vclock_gettime.o := %s $(VDSO_CFLAGS) -DVALUE=$$(value) $(pound)tail -DQUOTE='quoted' -c vclock_gettime.c -o vclock_gettime.o
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
make_cmd := $(call make-cmd,vclock_gettime.o)
`, roleToken)
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"make_cmd"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Kbuild.include", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["make_cmd"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("VDSO make-cmd plan = %#v, want two probes", discovery.Plan.Nodes)
	}
	if !strings.Contains(discovery.Value, roleToken) {
		t.Fatalf("VDSO make-cmd hid action-role provenance: %q", discovery.Value)
	}
	symbols := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)
	if len(symbols) != 1 {
		t.Fatalf("VDSO make-cmd discovery value = %q, want one nested protocol", discovery.Value)
	}
	evaluator, makeCmd, ok := evaluatedScopes.symbolOwner(symbols[0])
	if !ok || makeCmd.kind != "make-text" || makeCmd.makeText == nil ||
		makeCmd.makeText.function != "subst" || makeCmd.makeText.protocolMode != linuxProbeMakeTextProtocolExact ||
		len(makeCmd.makeText.protocolTransforms) != 4 ||
		makeCmd.makeText.protocolTransforms[0].function != "filter-out" ||
		makeCmd.makeText.protocolTransforms[1].function != "subst" ||
		makeCmd.makeText.protocolTransforms[2].function != "subst" ||
		makeCmd.makeText.protocolTransforms[3].function != "subst" {
		t.Fatalf("VDSO make-cmd protocol = %#v", makeCmd)
	}

	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(discovery.Value)
	if err != nil || !dynamic || len(fragments) == 0 {
		t.Fatalf("VDSO make-cmd lowering = %#v, dynamic=%v, error=%v", fragments, dynamic, err)
	}
	var functions []string
	var inspect func([]ProbeValueFragment)
	inspect = func(values []ProbeValueFragment) {
		for _, fragment := range values {
			inspect(fragment.Fragments)
			for _, transform := range fragment.Transforms {
				for _, group := range transform.ArgumentFragments {
					inspect(group.Fragments)
				}
				functions = append(functions, transform.Function)
			}
		}
	}
	inspect(fragments)
	if want := []string{"filter-out", "subst", "subst", "subst"}; !slices.Equal(functions, want) {
		t.Fatalf("VDSO make-cmd runtime transform order = %q, want %q", functions, want)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Steps:   []ProbeStep{{Name: "consume", Tool: "cc", Arguments: []string{""}, ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: fragments}}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("VDSO make-cmd request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name         string
		padding, lto bool
		want         string
	}{
		{
			name: "wildcard removes literal and dynamic flags", padding: true, lto: true,
			want: roleToken + ` -fkeep -DVALUE=$$(value) $(pound)tail -DQUOTE='\''quoted'\'' -c vclock_gettime.c -o vclock_gettime.o`,
		},
		{
			name: "pattern absent", padding: false, lto: true,
			want: roleToken + ` -fdrop-literal -fdrop-other -fkeep -DVALUE=$$(value) $(pound)tail -DQUOTE='\''quoted'\'' -c vclock_gettime.c -o vclock_gettime.o`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				if index == 0 {
					return test.padding
				}
				return test.lto
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("VDSO make-cmd replay = %q, want %q", replay.Value, test.want)
			}
		})
	}
}

func TestKbuildWholeTextTokenDoesNotAliasItsRawProtocolSpelling(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	const source = linearFilterCompilerFixture + `
first := $(call cc-option,-f0)
second := $(call cc-option,-f1)
RAW := $(first) $(second)
FILTERED := $(filter -f%,$(RAW))
`
	type result struct {
		Opaque string
		Raw    string
	}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"FILTERED", "RAW"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Opaque: parsed.Variables["FILTERED"], Raw: parsed.Variables["RAW"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Opaque == discovery.Value.Raw {
		t.Fatalf("whole-text token aliased raw protocol spelling %q", discovery.Value.Raw)
	}
	_, symbol, ok := evaluatedScopes.symbolOwner(discovery.Value.Opaque)
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil || symbol.makeText.protocolValue != discovery.Value.Raw {
		t.Fatalf("whole-text provenance = %#v, raw spelling %q", symbol, discovery.Value.Raw)
	}

	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
		return index == 1
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.Opaque != "-f1" || replay.Value.Raw != " -f1" {
		t.Fatalf("whole-text replay = %#v, want normalized opaque and independently preserved raw separator", replay.Value)
	}
}

func TestKbuildCmdCheckProvesNonemptyFromExactMakeASTWithoutEnumeratingFlags(t *testing.T) {
	const selectionCount = 26
	var base strings.Builder
	base.WriteString(linearFilterCompilerFixture)
	base.WriteString("empty :=\nspace := $(empty) $(empty)\nspace_escape := _-_SPACE_-_\n")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&base, "flag_%02d := $(call cc-option,-fcmd%02d)\n", index, index)
	}
	base.WriteString("FLAGS :=")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&base, " $(flag_%02d)", index)
	}
	base.WriteString("\ncmd := cc $(FLAGS) -c input.c\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Selected string
		Check    string
	}
	evaluate := func(source string, oracle *ProbeResultOracle) (*KbuildProbeEvaluation[result], error) {
		return EvaluateKbuildProbeWorkload(opts, oracle, func(scopes *KbuildProbeScopes) (result, error) {
			options, err := scopes.Options("target", KbuildOptions{
				Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureVariables:        []string{"selected", "cmd_check"},
			})
			if err != nil {
				return result{}, err
			}
			parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Kbuild.include", options, "")
			if err != nil {
				return result{}, err
			}
			return result{Selected: parsed.Variables["selected"], Check: parsed.Variables["cmd_check"]}, nil
		})
	}

	positive := base.String() + `
savedcmd :=
cmd_check := $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd))),$(subst $(space),$(space_escape),$(strip $(cmd))))
selected := $(if $(cmd_check),changed,same)
`
	discovery, err := evaluate(positive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Selected != "changed" || len(discovery.Plan.Nodes) != selectionCount || !linuxProbeSymbolPattern.MatchString(discovery.Value.Check) {
		t.Fatalf("cmd-check discovery = %#v, plan nodes = %d", discovery.Value, len(discovery.Plan.Nodes))
	}
	for _, test := range []struct {
		name      string
		supported func(int) bool
	}{
		{name: "none", supported: func(int) bool { return false }},
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
			replay, err := evaluate(positive, oracle)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value.Selected != "changed" || len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
				t.Fatalf("cmd-check replay = %#v, plan nodes = %d", replay.Value, len(replay.Plan.Nodes))
			}
		})
	}

	// Invocation discovery evaluates cmd-check before a concrete rule binds
	// $@. The saved command therefore remains exact Make text. The dynamic
	// current command must still be retained without requiring an argv protocol;
	// concrete target evaluation will bind the automatic variable later.
	unboundAutomatic := base.String() + `
cmd_check := $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd_$@))),$(subst $(space),$(space_escape),$(strip $(cmd))))
selected := deferred
`
	unboundDiscovery, err := evaluate(unboundAutomatic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matches := linuxProbeSymbolPattern.FindAllString(unboundDiscovery.Value.Check, -1); len(matches) != 1 || matches[0] != unboundDiscovery.Value.Check {
		t.Fatalf("unbound-automatic cmd-check = %q, want one exact-AST token", unboundDiscovery.Value.Check)
	}
	unboundRoots := writeKbuildProbeResultsByNode(t, unboundDiscovery.Plan, func(index int, _ ProbePlanNode) bool {
		return index%2 == 0
	})
	unboundOracle, err := NewProbeResultOracleFromTrees(unboundRoots, unboundDiscovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	unboundReplay, err := evaluate(unboundAutomatic, unboundOracle)
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.MatchString(unboundReplay.Value.Check) || !strings.Contains(unboundReplay.Value.Check, "cc_-_SPACE_-") {
		t.Fatalf("unbound-automatic cmd-check replay = %q", unboundReplay.Value.Check)
	}

	negativeSources := map[string]string{
		"dynamic equal saved command": base.String() + `
savedcmd = $(cmd)
cmd_check := $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd))),$(subst $(space),$(space_escape),$(strip $(cmd))))
selected := $(if $(cmd_check),changed,same)
`,
		"nonempty saved command": base.String() + `
savedcmd := cc
cmd_check := $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd))),$(subst $(space),$(space_escape),$(strip $(cmd))))
selected := $(if $(cmd_check),changed,same)
`,
	}
	for name, source := range negativeSources {
		t.Run(name, func(t *testing.T) {
			if _, err := evaluate(source, nil); err == nil {
				t.Fatalf("unsafe cmd-check proof error = %v", err)
			}
		})
	}

	// Once strip owns one complete runtime fragment graph, a following bytewise
	// subst can safely delete the only fixed non-space byte. The result must be
	// measured, not guessed non-empty from source literals or rejected merely
	// because the branch carries more than eight independent compiler probes.
	dynamicSubst := base.String() + `
cmd_check := $(subst x,,$(strip x$(FLAGS)))
selected := $(if $(cmd_check),changed,same)
`
	dynamicDiscovery, err := evaluate(dynamicSubst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(dynamicDiscovery.Value.Check) ||
		!linuxProbeSymbolPattern.MatchString(dynamicDiscovery.Value.Selected) {
		t.Fatalf("dynamic subst result = %#v, want retained measured text and selection", dynamicDiscovery.Value)
	}
	if got, want := len(dynamicDiscovery.Plan.Nodes), selectionCount+2; got != want {
		t.Fatalf("dynamic subst plan nodes = %d, want %d: %#v", got, want, dynamicDiscovery.Plan.Nodes)
	}
	derived := dynamicDiscovery.Plan.Nodes[selectionCount]
	request := dynamicDiscovery.Plan.Requests[derived.RequestID]
	if len(derived.Inputs) != selectionCount || request.Outcome.Kind != "text" || len(request.Outcome.Fragments) == 0 {
		t.Fatalf("dynamic subst derived node = %#v, request = %#v", derived, request)
	}
	render := func(supported int) string {
		t.Helper()
		inputs := make(map[string]ProbeResult, selectionCount)
		for index := 0; index < selectionCount; index++ {
			value := index == supported
			inputs[fmt.Sprintf("%08d", index)] = ProbeResult{Kind: "boolean", Boolean: &value}
		}
		materialized, renderErr := RenderProbeDependencyFragments(request.Outcome.Fragments, inputs)
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return materialized
	}
	if got := render(-1); got != "" {
		t.Fatalf("dynamic subst with no supported flags = %q, want empty", got)
	}
	if got := render(0); got != "-fcmd00" {
		t.Fatalf("dynamic subst with one supported flag = %q, want %q", got, "-fcmd00")
	}
}

func TestKbuildWholeMakeTextIsClosedUnderPureFunctionComposition(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	const source = linearFilterCompilerFixture + `
a := $(call cc-option,-fa)
b := $(call cc-option,-fb)
c := $(call cc-option,-fc)
d := $(call cc-option,-fd)
LEFT := $(filter -f%,$(a) $(b))
RIGHT := $(filter -f%,$(c) $(d))
FIRST := $(firstword $(LEFT))
SORTED := $(sort fixed $(LEFT))
JOINED := $(join $(LEFT),$(RIGHT))
`
	type result struct {
		First  string
		Sorted string
		Joined string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"FIRST", "SORTED", "JOINED"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{First: parsed.Variables["FIRST"], Sorted: parsed.Variables["SORTED"], Joined: parsed.Variables["JOINED"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"firstword": discovery.Value.First, "embedded sort": discovery.Value.Sorted, "two-input join": discovery.Value.Joined} {
		if matches := linuxProbeSymbolPattern.FindAllString(value, -1); len(matches) != 1 || matches[0] != value {
			t.Fatalf("%s discovery value = %q, want one opaque exact-AST token", name, value)
		}
	}

	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
		return index != 0
	})
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := result{First: "-fb", Sorted: "-fb fixed", Joined: "-fb-fc -fd"}
	if replay.Value != want {
		t.Fatalf("pure Make composition replay = %#v, want %#v", replay.Value, want)
	}
}

func TestKbuildWordwiseFilterPreservesExactMakeWordSemantics(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	source := linearFilterCompilerFixture + `
choice_00 := $(call cc-option,-fkeep-one -fdup,-fdrop-one -fdup)
choice_01 := $(call cc-option,-fkeep-01,-fdrop-01)
choice_02 := $(call cc-option,-fkeep-02,-fdrop-02)
choice_03 := $(call cc-option,-fkeep-03,-fdrop-03)
choice_04 := $(call cc-option,-fkeep-04,-fdrop-04)
choice_05 := $(call cc-option,-fkeep-05,-fdrop-05)
choice_06 := $(call cc-option,-fkeep-06,-fdrop-06)
choice_07 := $(call cc-option,-fkeep-07,-fdrop-07)
choice_08 := $(call cc-option,-fkeep-08,-fdrop-08)
TEXT := -fliteral -fdup $(choice_00) -fdup $(choice_01) $(choice_02) $(choice_03) $(choice_04) $(choice_05) $(choice_06) $(choice_07) $(choice_08) -fother
KEPT := $(filter -fkeep% -fdup,$(TEXT))
REMOVED := $(filter-out -fkeep% -fdup,$(TEXT))
`
	type result struct {
		Kept    string
		Removed string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"KEPT", "REMOVED"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Kept: parsed.Variables["KEPT"], Removed: parsed.Variables["REMOVED"]}, nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 9 {
		t.Fatalf("filter plan nodes = %#v, want nine source choices", discovery.Plan.Nodes)
	}
	for _, test := range []struct {
		name      string
		supported bool
		want      result
	}{
		{
			name: "true branch", supported: true,
			want: result{
				Kept:    "-fdup -fkeep-one -fdup -fdup -fkeep-01 -fkeep-02 -fkeep-03 -fkeep-04 -fkeep-05 -fkeep-06 -fkeep-07 -fkeep-08",
				Removed: "-fliteral -fother",
			},
		},
		{
			name: "false branch", supported: false,
			want: result{
				Kept:    "-fdup -fdup -fdup",
				Removed: "-fliteral -fdrop-one -fdrop-01 -fdrop-02 -fdrop-03 -fdrop-04 -fdrop-05 -fdrop-06 -fdrop-07 -fdrop-08 -fother",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": test.supported})
			delete(roots, "host")
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != test.want {
				t.Fatalf("filter replay = %#v, want %#v", replay.Value, test.want)
			}
			if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
				t.Fatalf("filter replay plan = %#v, want %#v", replay.Plan.Nodes, discovery.Plan.Nodes)
			}
			for index := range discovery.Plan.Nodes {
				if replay.Plan.Nodes[index].ID != discovery.Plan.Nodes[index].ID || replay.Plan.Nodes[index].RequestID != discovery.Plan.Nodes[index].RequestID {
					t.Fatalf("filter replay node %d = %#v, want %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
				}
			}
		})
	}
}

func TestKbuildOversizedEmbeddedFilterUsesLinearAggregateProtocol(t *testing.T) {
	const selectionCount = 21
	var source strings.Builder
	source.WriteString(linearFilterCompilerFixture)
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, "choice_%02d := $(call cc-option,-fflag%02d)\n", index, index)
	}
	source.WriteString("COUPLED := $(filter prefix-fflag%,prefix")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, "$(choice_%02d)", index)
	}
	source.WriteString(")\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, optionsErr := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"COUPLED"},
		})
		if optionsErr != nil {
			return "", optionsErr
		}
		parsed, parseErr := parseKbuildWithOptions(strings.NewReader(source.String()), "Makefile", options, "")
		if parseErr != nil {
			return "", parseErr
		}
		return parsed.Variables["COUPLED"], nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != selectionCount {
		t.Fatalf("embedded aggregate plan has %d nodes, want %d", len(discovery.Plan.Nodes), selectionCount)
	}
	evaluator, symbol, ok := evaluatedScopes.symbolOwner(discovery.Value)
	if !ok || evaluator.scope != "target" || symbol.kind != "make-text" || symbol.makeText == nil {
		t.Fatalf("embedded aggregate discovery value = %q, symbol = %#v, owner = %#v", discovery.Value, symbol, evaluator)
	}
	makeText := symbol.makeText
	if makeText.function != "filter" || makeText.protocolMode != linuxProbeMakeTextProtocolExact ||
		len(makeText.protocolTransforms) != 1 || makeText.protocolTransforms[0].function != "filter" ||
		makeText.protocolTransforms[0].inputArgument != 1 {
		t.Fatalf("embedded aggregate protocol = %#v", makeText)
	}
	if matches := linuxProbeSymbolPattern.FindAllString(makeText.protocolValue, -1); len(matches) != selectionCount {
		t.Fatalf("embedded aggregate protocol has %d source atoms, want linear %d: %q", len(matches), selectionCount, makeText.protocolValue)
	}

	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, dynamic, err := lowerer.value(discovery.Value)
	if err != nil || !dynamic || len(fragments) != 1 {
		t.Fatalf("embedded aggregate lowering = %#v, dynamic=%v, error=%v", fragments, dynamic, err)
	}
	if len(lowerer.dependencies) != selectionCount {
		t.Fatalf("embedded aggregate lowering has %d dependencies, want %d", len(lowerer.dependencies), selectionCount)
	}
	aggregate := fragments[0]
	if len(aggregate.Fragments) == 0 || len(aggregate.Transforms) != 1 || aggregate.Transforms[0].Function != "filter" {
		t.Fatalf("embedded aggregate fragments = %#v", fragments)
	}
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Steps: []ProbeStep{{
			Name: "consume", Tool: "cc", Arguments: []string{""},
			ArgumentFragments: []ProbeArgumentFragments{{Index: 0, Fragments: fragments}},
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "consume"}},
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("embedded aggregate request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name     string
		selected func(int) bool
	}{
		{name: "none", selected: func(int) bool { return false }},
		{name: "even", selected: func(index int) bool { return index%2 == 0 }},
		{name: "all", selected: func(int) bool { return true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool {
				return test.selected(index)
			})
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			var want strings.Builder
			want.WriteString("prefix")
			selected := false
			for index := 0; index < selectionCount; index++ {
				if test.selected(index) {
					selected = true
					fmt.Fprintf(&want, "-fflag%02d", index)
				}
			}
			wantValue := ""
			if selected {
				wantValue = want.String()
			}
			if replay.Value != wantValue {
				t.Fatalf("embedded aggregate replay = %q, want %q", replay.Value, wantValue)
			}
		})
	}
}

func TestKbuildMakeCmdSubstRetainsMixedSymbolicCommandExactly(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	const source = linearFilterCompilerFixture + `
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
first := $(call cc-option,-ffirst)
second := $(call cc-option,-fsecond)
cmd_fixdep = __LINUX_BZL_ACTION_ROLE_host_cc__ $(first) -o $@ scripts/basic/fixdep.c $(second) -DVALUE=$$value $(pound)tail
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
SAVED := $(call make-cmd,fixdep)
`
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"SAVED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Kbuild.include", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["SAVED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 2 || len(linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)) != 2 {
		t.Fatalf("make-cmd discovery = %q, plan %#v; want two independently retained source probes", discovery.Value, discovery.Plan.Nodes)
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
	want := `__LINUX_BZL_ACTION_ROLE_host_cc__ -ffirst -o $$@ scripts/basic/fixdep.c -fsecond -DVALUE=$$value $(pound)tail`
	if replay.Value != want {
		t.Fatalf("make-cmd replay = %q, want %q", replay.Value, want)
	}
}

func TestKbuildMakeCmdEscapesGetconfTextAndFiniteCompilerFlag(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	host.Tools["script-runtime"] = "/configured/host/script-runtime"
	host.Tools["scriptrun"] = "/configured/host/scriptrun"
	opts := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	const source = linearFilterCompilerFixture + `
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
lfs_flags := $(shell getconf LFS_CFLAGS)
choice := $(call cc-option,-ffirst)
cmd_fixdep = __LINUX_BZL_ACTION_ROLE_host_cc__ $(lfs_flags) $(choice) -o $@ scripts/basic/fixdep.c $(pound)tail
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
SAVED := $(call make-cmd,fixdep)
`
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("host", KbuildOptions{
			Variables:               map[string]string{"CC": host.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"SAVED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source), "scripts/Kbuild.include", options, "")
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
		t.Fatalf("getconf make-cmd plan = %#v, want text query and compiler capability", discovery.Plan.Nodes)
	}
	kinds := map[string]int{}
	for _, token := range linuxProbeSymbolPattern.FindAllString(discovery.Value, -1) {
		_, symbol, ok := evaluatedScopes.symbolOwner(token)
		if !ok {
			t.Fatalf("getconf make-cmd has unknown atom %q", token)
		}
		kinds[symbol.kind]++
	}
	if kinds["transformed-text"] != 1 || kinds["selection"] != 1 || len(kinds) != 2 {
		t.Fatalf("getconf make-cmd atom kinds = %#v, want one genuine text transform and one finite selection", kinds)
	}

	resultRoot := filepath.Join(t.TempDir(), "host")
	if err := os.MkdirAll(filepath.Join(resultRoot, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: discovery.Plan.Toolsets[node.Scope],
		}
		if request.Outcome.Kind == "text" {
			result.Kind = "text"
			result.Text = "-D_FILE_OFFSET_BITS=64"
			result.Steps = []ProbeStepResult{
				{Name: "compile", Status: "success", ExitCode: 0},
				{Name: "link", Status: "success", ExitCode: 0},
				{Name: "execute", Status: "success", ExitCode: 0, Stdout: "-D_FILE_OFFSET_BITS=64\n"},
			}
		} else {
			supported := true
			result.Kind = "boolean"
			result.Boolean = &supported
			result.Steps = []ProbeStepResult{{Name: "probe", Status: "success", ExitCode: 0}}
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
	want := `__LINUX_BZL_ACTION_ROLE_host_cc__ -D_FILE_OFFSET_BITS=64 -ffirst -o $$@ scripts/basic/fixdep.c $(pound)tail`
	if replay.Value != want {
		t.Fatalf("getconf make-cmd replay = %q, want %q", replay.Value, want)
	}
}

func TestKbuildCompositionalSubstKeepsIndependentSelectionsLinear(t *testing.T) {
	const selectionCount = 24
	var source strings.Builder
	source.WriteString(linearFilterCompilerFixture)
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, "choice_%02d := $(if $(call cc-option,-fprobe%02d),-fenabled%02d$$x,-fdisabled%02d$$x)\n", index, index, index, index)
	}
	source.WriteString("TEXT := literal$$value")
	for index := 0; index < selectionCount; index++ {
		fmt.Fprintf(&source, " $(choice_%02d)", index)
	}
	source.WriteString(" $$tail\nESCAPED := $(subst $$,$$$$,$(TEXT))\n")

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	var evaluatedScopes *KbuildProbeScopes
	workload := func(scopes *KbuildProbeScopes) (string, error) {
		evaluatedScopes = scopes
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"ESCAPED"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(source.String()), "Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["ESCAPED"], nil
	}

	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != selectionCount {
		t.Fatalf("subst plan has %d nodes, want %d", len(discovery.Plan.Nodes), selectionCount)
	}
	tokens := linuxProbeSymbolPattern.FindAllString(discovery.Value, -1)
	if len(tokens) != selectionCount {
		t.Fatalf("subst result has %d atoms, want %d: %q", len(tokens), selectionCount, discovery.Value)
	}
	for index, token := range tokens {
		evaluator, symbol, ok := evaluatedScopes.symbolOwner(token)
		if !ok || evaluator.scope != "target" || symbol.kind != "selection" {
			t.Fatalf("subst atom %d = %q, symbol %#v, owner %#v", index, token, symbol, evaluator)
		}
		if len(symbol.selectionInputs) != 1 || len(symbol.selectionValues) != 2 {
			t.Fatalf("subst atom %d selection = %#v, want one input and two values", index, symbol)
		}
		wantFalse := fmt.Sprintf("-fdisabled%02d$$x", index)
		wantTrue := fmt.Sprintf("-fenabled%02d$$x", index)
		if symbol.selectionValues[0] != wantFalse || symbol.selectionValues[1] != wantTrue {
			t.Fatalf("subst atom %d values = %q, want %q and %q", index, symbol.selectionValues, wantFalse, wantTrue)
		}
	}

	roots := writeKbuildProbeResultsByNode(t, discovery.Plan, func(index int, _ ProbePlanNode) bool { return index%2 == 0 })
	oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`literal$$value`}
	for index := 0; index < selectionCount; index++ {
		branch := "disabled"
		if index%2 == 0 {
			branch = "enabled"
		}
		want = append(want, fmt.Sprintf("-f%s%02d$$x", branch, index))
	}
	want = append(want, `$$tail`)
	if replay.Value != strings.Join(want, " ") {
		t.Fatalf("subst replay = %q, want %q", replay.Value, strings.Join(want, " "))
	}
}
