package kconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestKbuildAndOrReturnFinalSymbolicArgumentWithoutBranching(t *testing.T) {
	token := linuxProbeSymbolPrefix + strings.Repeat("a", 64)
	parsed := parseCapturedKbuild(t, `
or_result := $(or ,$(probe))
and_result := $(and concrete,$(probe))
`, KbuildOptions{
		Variables:             map[string]string{"probe": token},
		MakeVariablesComplete: true,
	}, "or_result", "and_result")
	for _, name := range []string{"or_result", "and_result"} {
		if got := parsed.Variables[name]; got != token {
			t.Fatalf("%s = %q, want final symbolic argument %q", name, got, token)
		}
	}
	if _, err := parseKbuildWithOptions(strings.NewReader(`value := $(or $(probe),fallback)`), "Kbuild", KbuildOptions{
		Variables: map[string]string{"probe": token}, MakeVariablesComplete: true,
	}, ""); err == nil || !strings.Contains(err.Error(), "cannot decide") {
		t.Fatalf("non-final symbolic or error = %v, want fail-closed decision", err)
	}
}

const symbolicHardeningCompilerFixture = `
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
first := $(call cc-option,-ffirst)
`

func evaluateSymbolicHardeningFixture(
	t *testing.T,
	source string,
	capture ...string,
) (*KbuildProbeEvaluation[map[string]string], error) {
	t.Helper()
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	return EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (map[string]string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"], "obj": "out", "CROSS_COMPILE": ""},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        capture,
		})
		if err != nil {
			return nil, err
		}
		parsed, err := parseKbuildWithOptions(
			strings.NewReader(symbolicHardeningCompilerFixture+source),
			"symbolic/Makefile", options, "",
		)
		if err != nil {
			return nil, err
		}
		return parsed.Variables, nil
	})
}

func TestKbuildSymbolicIfFunctionLowersIntoDownstreamCompilerArguments(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
selected := $(if $(first),-fselected,-ffallback)
downstream := $(call cc-option,$(selected))
`, "selected", "downstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 2 {
		t.Fatalf("nodes = %#v, want source and downstream probes", evaluation.Plan.Nodes)
	}
	request := evaluation.Plan.Requests[evaluation.Plan.Nodes[1].RequestID]
	groups := request.Steps[0].ConditionalArguments
	if len(groups) != 2 || !slices.Equal(groups[0].Arguments, []string{"-ffallback"}) ||
		!slices.Equal(groups[1].Arguments, []string{"-fselected"}) {
		t.Fatalf("downstream $(if) conditional arguments = %#v", groups)
	}
}

func TestKbuildSymbolicFindstringMaterializesCompilerDerivedText(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
needle := $(strip $(first))
filtered := $(filter-out impossible,base $(first))
flags := prefix $(filtered) suffix
found := $(findstring $(needle),$(flags))
selected := $(if $(found),yes,no)
`, "needle", "flags", "found", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evaluation.Plan.Nodes); got != 3 {
		t.Fatalf("findstring plan nodes = %d, want compiler result, derived text, and emptiness predicate: %#v", got, evaluation.Plan.Nodes)
	}
	if !linuxProbeSymbolPattern.MatchString(evaluation.Value["found"]) ||
		!linuxProbeSymbolPattern.MatchString(evaluation.Value["selected"]) {
		t.Fatalf("symbolic findstring values = %#v, want derived text and selection atoms", evaluation.Value)
	}
	request := evaluation.Plan.Requests[evaluation.Plan.Nodes[1].RequestID]
	if request.Outcome.Kind != "text" || len(request.Outcome.Fragments) != 1 ||
		len(request.Outcome.Fragments[0].Transforms) != 1 ||
		request.Outcome.Fragments[0].Transforms[0].Function != "findstring" {
		t.Fatalf("findstring derived-text request = %#v", request)
	}
	if data, jsonErr := request.CanonicalJSON(); jsonErr != nil {
		t.Fatal(jsonErr)
	} else if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("findstring request leaked planner-only token: %s", data)
	}
}

func renderSingleBooleanDerivedText(t *testing.T, evaluation *KbuildProbeEvaluation[map[string]string], supported bool) string {
	t.Helper()
	if len(evaluation.Plan.Nodes) < 2 {
		t.Fatalf("probe plan has no derived text node: %#v", evaluation.Plan.Nodes)
	}
	node := evaluation.Plan.Nodes[1]
	request := evaluation.Plan.Requests[node.RequestID]
	if len(node.Inputs) != 1 || request.Outcome.Kind != "text" || len(request.Outcome.Fragments) == 0 {
		t.Fatalf("derived findstring node = %#v, request = %#v", node, request)
	}
	result := ProbeResult{Kind: "boolean", Boolean: &supported}
	materialized, err := RenderProbeDependencyFragments(
		request.Outcome.Fragments,
		map[string]ProbeResult{"00000000": result},
	)
	if err != nil {
		t.Fatal(err)
	}
	return materialized
}

func TestKbuildSymbolicFindstringNormalizesCanonicalHaystackBeforeBytewiseSearch(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
empty :=
space := $(empty) $(empty)
needle := base$(space)
flags := $(strip base $(first))
found := $(findstring $(needle),$(flags))
selected := $(if $(found),yes,no)
`, "needle", "flags", "found", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if got := renderSingleBooleanDerivedText(t, evaluation, false); got != "" {
		t.Fatalf("findstring over stripped unsupported flag = %q, want empty", got)
	}
	if got := renderSingleBooleanDerivedText(t, evaluation, true); got != "base " {
		t.Fatalf("findstring over stripped supported flag = %q, want %q", got, "base ")
	}
}

func TestKbuildSymbolicFindstringPreservesHaystackArgumentWhitespace(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
empty :=
space := $(empty) $(empty)
needle := $(space)-ffirst
flags := $(strip $(first))
found := $(findstring $(needle), $(flags))
selected := $(if $(found),yes,no)
`, "needle", "flags", "found", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if got := renderSingleBooleanDerivedText(t, evaluation, false); got != "" {
		t.Fatalf("findstring over empty unsupported flag = %q, want empty", got)
	}
	if got := renderSingleBooleanDerivedText(t, evaluation, true); got != " -ffirst" {
		t.Fatalf("findstring lost the source haystack space: got %q, want %q", got, " -ffirst")
	}
}

func TestKbuildSymbolicStripComposesMultipleRuntimeWholeTextValues(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
second := $(call cc-option,-fsecond)
first_found := $(findstring -ffirst,$(strip $(first)))
second_found := $(findstring -fsecond,$(strip $(second)))
combined := $(strip $(first_found) $(second_found))
selected := $(if $(combined),yes,no)
`, "combined", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evaluation.Plan.Nodes), 4; got != want {
		t.Fatalf("nested strip plan nodes = %d, want %d: %#v", got, want, evaluation.Plan.Nodes)
	}
	node := evaluation.Plan.Nodes[2]
	request := evaluation.Plan.Requests[node.RequestID]
	if len(node.Inputs) != 2 || request.Outcome.Kind != "text" || len(request.Outcome.Fragments) == 0 {
		t.Fatalf("nested strip derived node = %#v, request = %#v", node, request)
	}
	render := func(first, second bool) string {
		t.Helper()
		value, renderErr := RenderProbeDependencyFragments(request.Outcome.Fragments, map[string]ProbeResult{
			"00000000": {Kind: "boolean", Boolean: &first},
			"00000001": {Kind: "boolean", Boolean: &second},
		})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return value
	}
	for _, test := range []struct {
		first, second bool
		want          string
	}{
		{want: ""},
		{first: true, want: "-ffirst"},
		{second: true, want: "-fsecond"},
		{first: true, second: true, want: "-ffirst -fsecond"},
	} {
		if got := render(test.first, test.second); got != test.want {
			t.Fatalf("nested strip(%t, %t) = %q, want %q", test.first, test.second, got, test.want)
		}
	}
}

func TestKbuildNestedCcOptionFallbackFeedsLaterCcOption(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
fallback := $(call cc-option,-mshort-load-bytes,$(call cc-option,-malignment-traps,))
downstream := $(call cc-option,$(fallback) -fzero-init-padding-bits=all)
`, "fallback", "downstream")
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(evaluation.Value["fallback"]) || !linuxProbeSymbolPattern.MatchString(evaluation.Value["downstream"]) {
		t.Fatalf("nested cc-option values = %#v, want symbolic fallback and downstream", evaluation.Value)
	}
	if len(evaluation.Plan.Nodes) != 4 {
		t.Fatalf("nested cc-option plan nodes = %#v, want fixture, inner, outer, and downstream probes", evaluation.Plan.Nodes)
	}
	downstream := evaluation.Plan.Nodes[len(evaluation.Plan.Nodes)-1]
	request := evaluation.Plan.Requests[downstream.RequestID]
	if request.InputCount != 2 {
		t.Fatalf("downstream nested cc-option input count = %d, want outer and inner probes", request.InputCount)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("nested cc-option leaked a symbolic token into executable request: %s", data)
	}
}

func TestKbuildFiniteSelectionFilterPreservesLinuxUserFlagsAndNestedProbeArguments(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		UserFlags    string
		NonUserFlags string
		Nested       string
		Downstream   string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"USERFLAGS", "NON_USERFLAGS", "NESTED", "downstream"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
second := $(call cc-option,-fsecond)
KBUILD_CPPFLAGS := -DKEEP
ifneq ($(first),)
KBUILD_CPPFLAGS += --target=x86_64 -ffirst-selected
else
KBUILD_CPPFLAGS += -m32 -fno-first
endif
KBUILD_CFLAGS :=
ifneq ($(second),)
KBUILD_CFLAGS += -m64 -fsecond-selected
else
KBUILD_CFLAGS += -m32 -fno-second
endif
USERFLAGS_FROM_KERNEL := -m32 -m64 --target=%
USERFLAGS := $(filter $(USERFLAGS_FROM_KERNEL), $(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS))
NON_USERFLAGS := $(filter-out $(USERFLAGS_FROM_KERNEL), $(KBUILD_CPPFLAGS) $(KBUILD_CFLAGS))
NESTED := $(filter -f%, $(NON_USERFLAGS))
downstream := $(call cc-option,$(NESTED))
`), "Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			UserFlags: parsed.Variables["USERFLAGS"], NonUserFlags: parsed.Variables["NON_USERFLAGS"],
			Nested: parsed.Variables["NESTED"], Downstream: parsed.Variables["downstream"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 3 {
		t.Fatalf("filter plan nodes = %#v, want two source selections and one downstream probe", discovery.Plan.Nodes)
	}
	for name, value := range map[string]string{
		"USERFLAGS": discovery.Value.UserFlags, "NON_USERFLAGS": discovery.Value.NonUserFlags,
		"NESTED": discovery.Value.Nested, "downstream": discovery.Value.Downstream,
	} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Fatalf("discovery %s = %q, want retained symbolic value", name, value)
		}
	}
	downstream := discovery.Plan.Nodes[2]
	if !slices.Equal(downstream.Inputs, []string{discovery.Plan.Nodes[0].ID, discovery.Plan.Nodes[1].ID}) {
		t.Fatalf("nested filtered probe inputs = %q", downstream.Inputs)
	}
	groups := discovery.Plan.Requests[downstream.RequestID].Steps[0].ConditionalArguments
	fragments := discovery.Plan.Requests[downstream.RequestID].Steps[0].ArgumentFragments
	if len(groups) != 0 || len(fragments) != 1 || fragments[0].Index != 1 {
		t.Fatalf("nested filter lowering groups = %#v, fragments = %#v", groups, fragments)
	}
	wantFragments := map[string]bool{
		"-fno-first": false, "-ffirst-selected": false,
		"-fno-second": false, "-fsecond-selected": false,
	}
	spaces := 0
	for _, fragment := range fragments[0].Fragments {
		if fragment.When == nil {
			if fragment.Value != " " {
				t.Fatalf("nested filter unconditional fragment = %#v", fragment)
			}
			spaces++
			continue
		}
		if _, ok := wantFragments[fragment.Value]; ok {
			wantFragments[fragment.Value] = true
		}
	}
	if spaces != 3 {
		t.Fatalf("nested filter has %d unconditional separators, want 3: %#v", spaces, fragments)
	}
	for value, found := range wantFragments {
		if !found {
			t.Errorf("downstream filter fragments omitted %q: %#v", value, fragments)
		}
	}

	for _, replayCase := range []struct {
		name      string
		supported bool
		want      result
	}{
		{
			name: "selected", supported: true,
			want: result{
				UserFlags: "--target=x86_64 -m64", NonUserFlags: "-DKEEP -ffirst-selected -fsecond-selected",
				Nested: "-ffirst-selected -fsecond-selected", Downstream: "-ffirst-selected -fsecond-selected",
			},
		},
		{
			name: "fallback", supported: false,
			want: result{
				// GNU Make filter preserves both matching -m32 words and their order.
				UserFlags: "-m32 -m32", NonUserFlags: "-DKEEP -fno-first -fno-second",
				Nested: "-fno-first -fno-second", Downstream: "",
			},
		},
	} {
		t.Run("replay "+replayCase.name, func(t *testing.T) {
			roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": replayCase.supported})
			delete(roots, "host")
			oracle, err := NewProbeResultOracleFromTrees(roots, discovery.Plan.Toolsets)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
			if err != nil {
				t.Fatal(err)
			}
			if replay.Value != replayCase.want {
				t.Fatalf("filter replay = %#v, want %#v", replay.Value, replayCase.want)
			}
			if len(replay.Plan.Nodes) != len(discovery.Plan.Nodes) {
				t.Fatalf("filter replay plan nodes = %#v, want %#v", replay.Plan.Nodes, discovery.Plan.Nodes)
			}
			for index := range discovery.Plan.Nodes {
				if replay.Plan.Nodes[index].ID != discovery.Plan.Nodes[index].ID || replay.Plan.Nodes[index].RequestID != discovery.Plan.Nodes[index].RequestID {
					t.Fatalf("filter replay node %d = %#v, want %#v", index, replay.Plan.Nodes[index], discovery.Plan.Nodes[index])
				}
			}
		})
	}
}

func TestKbuildGuardedScalarAndOrderedElseIfLowerExactly(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
second := $(call cc-option,-fsecond)
ifneq ($(first),)
selected := -ffirst-selected
else ifneq ($(second),)
selected := -fsecond-selected
else
selected := -ffallback-selected
endif
downstream := $(call cc-option,$(selected))
`, "selected", "downstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 3 {
		t.Fatalf("nodes = %#v, want two independent inputs and downstream", evaluation.Plan.Nodes)
	}
	downstream := evaluation.Plan.Nodes[2]
	if !slices.Equal(downstream.Inputs, []string{evaluation.Plan.Nodes[0].ID, evaluation.Plan.Nodes[1].ID}) {
		t.Fatalf("downstream inputs = %q", downstream.Inputs)
	}
	groups := evaluation.Plan.Requests[downstream.RequestID].Steps[0].ConditionalArguments
	want := map[string]bool{
		"-ffallback-selected": false,
		"-ffirst-selected":    false,
		"-fsecond-selected":   false,
	}
	for _, group := range groups {
		if len(group.Arguments) == 1 {
			if _, ok := want[group.Arguments[0]]; ok {
				want[group.Arguments[0]] = true
			}
		}
		if group.When.Operator != "all" || len(group.When.Operands) != 2 {
			t.Fatalf("ordered selection predicate = %#v, want two-input conjunction", group.When)
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("downstream selection omitted %s: %#v", value, groups)
		}
	}
}

func TestKbuildNestedSymbolicAppendIsRecursivelyLowered(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
second := $(call cc-option,-fsecond)
flags := -fbase
ifneq ($(first),)
flags += $(if $(second),-fboth,-ffirst-only)
endif
downstream := $(call cc-option,$(flags))
`, "flags", "downstream")
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 3 || len(evaluation.Plan.Requests[evaluation.Plan.Nodes[2].RequestID].Steps[0].ConditionalArguments) == 0 {
		t.Fatalf("nested append plan = %#v", evaluation.Plan)
	}
}

func TestKbuildProbeDependentIfdefRetainsTheBranch(t *testing.T) {
	for _, directive := range []string{"ifdef first", "ifndef first"} {
		t.Run(strings.Fields(directive)[0], func(t *testing.T) {
			evaluation, err := evaluateSymbolicHardeningFixture(t, `
flags := -fbase
`+directive+`
flags += -fthen
else
flags += -felse
endif
downstream := $(call cc-option,$(flags))
`, "flags", "downstream")
			if err != nil {
				t.Fatal(err)
			}
			if len(evaluation.Plan.Nodes) != 2 || len(evaluation.Plan.Requests[evaluation.Plan.Nodes[1].RequestID].Steps[0].ConditionalArguments) != 2 {
				t.Fatalf("%s plan = %#v", directive, evaluation.Plan)
			}
		})
	}
}

func TestKbuildProbeDependentEffectsFailClosed(t *testing.T) {
	tests := map[string]string{
		"assignment lhs": `ifneq ($(first),)
$(first) := value
endif`,
		"generated topology": `ifneq ($(first),)
always-y += generated.h
endif`,
		"target variable": `ifneq ($(first),)
target.o: CFLAGS += -fbranch
endif`,
		"define": `ifneq ($(first),)
define branch_helper
value
endef
endif`,
		"export": `ifneq ($(first),)
export BRANCH_VALUE
endif`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := evaluateSymbolicHardeningFixture(t, source); err == nil || !strings.Contains(err.Error(), "probe-dependent") {
				t.Fatalf("error = %v, want probe-dependent rejection", err)
			}
		})
	}
}

func TestKbuildProbeAtomsFailClosedInUnmodeledMakeDecisions(t *testing.T) {
	tests := map[string]string{
		"and": `result := $(and $(first),selected)`,
		"or":  `result := $(or $(first),fallback)`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := evaluateSymbolicHardeningFixture(t, source, "result"); err == nil {
				t.Fatalf("unmodeled %s accepted an unresolved probe", name)
			}
		})
	}
}

func TestKbuildProbeDependentIfRejectsStatefulBranches(t *testing.T) {
	for name, source := range map[string]string{
		"eval":                `result := $(if $(first),$(eval MUTATED := yes),safe)`,
		"shell":               `result := $(if $(first),$(shell printf unsafe),safe)`,
		"error":               `result := $(if $(first),$(error unsafe),safe)`,
		"warning nested eval": `result := $(if $(first),$(warning $(eval MUTATED := yes)),safe)`,
		"info nested shell":   `result := $(if $(first),$(info $(shell printf unsafe)),safe)`,
		"short variable eval": `x = $(eval MUTATED := yes)
result := $(if $(first),$x,safe)`,
		"substitution eval": `danger = $(eval MUTATED := yes)x
result := $(if $(first),$(danger:x=y),safe)`,
		"computed eval": `effect_name = unsafe_effect
unsafe_effect = $(eval MUTATED := yes)
result := $(if $(first),$($(effect_name)),safe)`,
		"argument-sensitive recursive call": `value_stop = $(eval MUTATED := yes)
dispatch = $(if $(filter stop,$1),$(value_$1),$(call dispatch,stop))
result := $(if $(first),$(call dispatch,start),safe)`,
		"hidden call": `stateful = $(eval MUTATED := yes)
result := $(if $(first),$(call stateful),safe)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := evaluateSymbolicHardeningFixture(t, source, "result"); err == nil || !strings.Contains(err.Error(), "stateful branch") {
				t.Fatalf("stateful branch error = %v", err)
			}
		})
	}
}

func TestKbuildProbeDependentIfAllowsConcreteComputedCommandReferences(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
quiet := quiet_
quiet_log_print = echo compile;
PHONY := FORCE
delete-on-interrupt = \
	$(if $(filter-out $(PHONY), $@),\
		$(foreach sig,HUP INT QUIT TERM PIPE,\
			trap 'rm -f $@; trap - $(sig); kill -s $(sig) $$$$' $(sig);))
late_flag = $(call cc-option,-flate)
cmd_record_mcount = $(late_flag)$(first)
cmd = @$(if $(cmd_$(1)),set -e; $($(quiet)log_print) $(delete-on-interrupt) $(cmd_$(1)),:)
result := $(call cmd,record_mcount)
`, "result")
	if err != nil {
		t.Fatal(err)
	}
	if result := evaluation.Value["result"]; !linuxProbeSymbolPattern.MatchString(result) {
		t.Fatalf("computed command wrapper result = %q, want retained probe-dependent selection", result)
	}
	if got := len(evaluation.Plan.Nodes); got != 2 {
		t.Fatalf("computed command wrapper plan nodes = %#v, want initial and late compiler capability probes", evaluation.Plan.Nodes)
	}
}

func TestKbuildActionRoleReplayAllowsPreviouslyObservedRecursiveCompilerProbe(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type snapshot struct {
		Text string
		Refs []KbuildActionRoleRef
	}
	evaluation, err := EvaluateKbuildProbeWorkload(probeOptions, nil, func(scopes *KbuildProbeScopes) (snapshot, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{"obj": "lib"},
			CommandLineVariables: map[string]string{
				"CC": KbuildActionRoleToken("target", "cc"),
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureTargetEvaluator:  true,
		})
		if err != nil {
			return snapshot{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
CFLAGS_debug_info.o += $(call cc-option, -femit-struct-debug-detailed=any)
_c_flags = $(first) $(CFLAGS_$(target-stem).o)
CC_FLAGS_FTRACE := -ffirst
sub_cmd_record_mcount = $(CC) -o $@
cmd_record_mcount = $(if $(findstring $(strip $(CC_FLAGS_FTRACE)),$(_c_flags)), \
	$(sub_cmd_record_mcount))
quiet := quiet_
quiet_log_print = echo compile;
PHONY := FORCE
delete-on-interrupt = \
	$(if $(filter-out $(PHONY), $@), \
		$(foreach sig, HUP INT QUIT TERM PIPE, \
			trap 'rm -f $@; trap - $(sig); kill -s $(sig) $$$$' $(sig);))
cmd = @$(if $(cmd_$(1)),set -e; $($(quiet)log_print) $(delete-on-interrupt) $(cmd_$(1)),:)
lib/debug_info.o: FORCE
	$(call cmd,record_mcount)
`), "scripts/Makefile.build", options, "")
		if err != nil {
			return snapshot{}, err
		}
		profile, err := NewCompactKbuildProfile("build:lib", "scripts/Makefile.build", "lib", parsed)
		if err != nil {
			return snapshot{}, err
		}
		text, refs, err := EvaluateCompactKbuildTextActionRolesForAutomaticTarget(
			profile, "lib/debug_info.o", "lib/debug_info.o", "", []string{"FORCE"}, nil, nil,
			profile.Rules[0].Recipe[0],
		)
		return snapshot{Text: text, Refs: refs}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(evaluation.Value.Text) {
		t.Fatalf("record_mcount wrapper = %q, want retained probe-dependent selection", evaluation.Value.Text)
	}
	wantRefs := []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}
	if !slices.Equal(evaluation.Value.Refs, wantRefs) {
		t.Fatalf("record_mcount action roles = %#v, want %#v", evaluation.Value.Refs, wantRefs)
	}
}

func TestKbuildProbeDependentIfScansCallsWithTheirOwnLocals(t *testing.T) {
	for _, test := range []struct {
		name                   string
		outerValue, innerValue string
		wantError              bool
	}{
		{
			name:       "pure inner ignores stateful outer",
			outerValue: "$(eval OUTER_MUTATION := yes)", innerValue: "pure",
		},
		{
			name:       "stateful inner does not inherit pure outer",
			outerValue: "pure", innerValue: "$(eval INNER_MUTATION := yes)", wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := `
value_outer = ` + test.outerValue + `
value_inner = ` + test.innerValue + `
dispatch = $(value_$1)
wrapper = $(if $(first),$(call dispatch,inner),safe)
result := $(call wrapper,outer)
`
			evaluation, err := evaluateSymbolicHardeningFixture(t, source, "result")
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "stateful branch") {
					t.Fatalf("call-local stateful branch error = %v, want fail-closed rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result := evaluation.Value["result"]; !linuxProbeSymbolPattern.MatchString(result) {
				t.Fatalf("call-local pure result = %q, want retained probe-dependent selection", result)
			}
		})
	}
}

func TestKbuildProbeDependentIfScansBindingFunctionsWithTheirOwnLocals(t *testing.T) {
	for _, function := range []string{"foreach", "let"} {
		for _, test := range []struct {
			name                   string
			outerValue, innerValue string
			wantError              bool
		}{
			{
				name:       "pure inner ignores stateful outer",
				outerValue: "$(eval OUTER_MUTATION := yes)", innerValue: "pure",
			},
			{
				name:       "stateful inner does not inherit pure outer",
				outerValue: "pure", innerValue: "$(eval INNER_MUTATION := yes)", wantError: true,
			},
		} {
			t.Run(function+"/"+test.name, func(t *testing.T) {
				binding := "$(foreach x,inner,$(value_$x))"
				if function == "let" {
					binding = "$(let x,inner,$(value_$x))"
				}
				source := `
x := outer
value_outer = ` + test.outerValue + `
value_inner = ` + test.innerValue + `
result := $(if $(first),` + binding + `,safe)
`
				evaluation, err := evaluateSymbolicHardeningFixture(t, source, "result")
				if test.wantError {
					if err == nil || !strings.Contains(err.Error(), "stateful branch") {
						t.Fatalf("binding-local stateful branch error = %v, want fail-closed rejection", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if result := evaluation.Value["result"]; !linuxProbeSymbolPattern.MatchString(result) {
					t.Fatalf("binding-local pure result = %q, want retained probe-dependent selection", result)
				}
			})
		}
	}
}

func TestKbuildProbeDependentIfDoesNotReexpandSimpleVariableBytes(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
literal := $$(eval MUTATED := yes)
result := $(if $(first),$(literal),safe)
observed := $(MUTATED)
`, "result", "observed")
	if err != nil {
		t.Fatal(err)
	}
	if observed := evaluation.Value["observed"]; observed != "" {
		t.Fatalf("escaped simple-variable bytes executed as Make: MUTATED=%q", observed)
	}
	if result := evaluation.Value["result"]; !linuxProbeSymbolPattern.MatchString(result) {
		t.Fatalf("simple-variable branch result = %q, want retained probe-dependent selection", result)
	}
}

func TestKbuildProbeDependentIfAllowsValueNeutralDiagnostics(t *testing.T) {
	for name, diagnostic := range map[string]string{
		"warning": "warning",
		"info":    "info",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := linuxCompilerBootstrapFixtures(t)[1]
			opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
			evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (string, error) {
				bootstrapOptions, err := scopes.Options("target", KbuildOptions{
					Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureVariables:        []string{"first", "prerequisites"},
				})
				if err != nil {
					return "", err
				}
				bootstrap, err := parseKbuildWithOptions(
					strings.NewReader(symbolicHardeningCompilerFixture+`prerequisites := $(if $(first),FORCE,)
`),
					"scripts/Kbuild.include.bootstrap", bootstrapOptions, "",
				)
				if err != nil {
					return "", err
				}

				// Keep the upstream scripts/Kbuild.include shape, including the
				// automatic prerequisite variable. A compiler probe makes the
				// presence of FORCE symbolic, but warning/info still expand to the
				// same empty command text on either branch.
				options, err := scopes.Options("target", KbuildOptions{
					Variables: map[string]string{
						"CC":    opts.Target.Tools["cc"],
						"first": bootstrap.Variables["first"],
						"^":     bootstrap.Variables["prerequisites"],
					},
					ConfigVariablesComplete: true,
					MakeVariablesComplete:   true,
					CaptureVariables:        []string{"result"},
				})
				if err != nil {
					return "", err
				}
				source := `check-FORCE = $(if $(filter FORCE, $^),,$(` + diagnostic + ` FORCE prerequisite is missing))
if-changed-cond = $(first)$(check-FORCE)
rule_rustc_library = $(CC) -c -o rust/pin_init.o rust/pin-init/lib.rs
if_changed_rule = $(if $(if-changed-cond),$(rule_rustc_library),@:)
result := $(if_changed_rule)
`
				parsed, err := parseKbuildWithOptions(
					strings.NewReader(source), "scripts/Kbuild.include", options, "",
				)
				if err != nil {
					return "", err
				}
				return parsed.Variables["result"], nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !linuxProbeSymbolPattern.MatchString(evaluation.Value) {
				t.Fatalf("if_changed_rule result = %q, want retained probe-dependent command selection", evaluation.Value)
			}
		})
	}
}

func TestKbuildSymbolicIfLazilyExpandsProvablyNonemptyCommand(t *testing.T) {
	evaluation, err := evaluateSymbolicHardeningFixture(t, `
quiet := quiet_
quiet_log_print = echo compile;
delete-on-interrupt = $(eval SELECTED_BRANCH := yes)trap cleanup;
cmd_compile = cc $(first) -o $@
cmd = @$(if $(cmd_$(1)),set -e; $($(quiet)log_print) $(delete-on-interrupt) $(cmd_$(1)),:)
result := $(call cmd,compile)
observed := $(SELECTED_BRANCH)
`, "result", "observed")
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluation.Value["observed"]; got != "yes" {
		t.Fatalf("selected branch effect = %q, want yes", got)
	}
	result := evaluation.Value["result"]
	for _, fragment := range []string{"@set -e; echo compile;", "trap cleanup;", "cc ", " -o $@"} {
		if !strings.Contains(result, fragment) {
			t.Fatalf("cmd result = %q, want fragment %q", result, fragment)
		}
	}
	if !linuxProbeSymbolPattern.MatchString(result) {
		t.Fatalf("cmd result = %q, want retained compiler probe", result)
	}
}

func TestKbuildSymbolicIfKeepsLargeFiniteCommandBranchNested(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type snapshot struct {
		Selected        string
		Wrapped         string
		Symbolic        bool
		FragmentCount   int
		DependencyCount int
	}
	evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (snapshot, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"], "obj": "out", "CROSS_COMPILE": ""},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"selected", "wrapped"},
		})
		if err != nil {
			return snapshot{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
branch0 := $(call cc-option,-fbranch-0)
branch1 := $(call cc-option,-fbranch-1)
branch2 := $(call cc-option,-fbranch-2)
branch3 := $(call cc-option,-fbranch-3)
branch4 := $(call cc-option,-fbranch-4)
branch5 := $(call cc-option,-fbranch-5)
branch6 := $(call cc-option,-fbranch-6)
branch7 := $(call cc-option,-fbranch-7)
branch8 := $(call cc-option,-fbranch-8)
command := -fcommand $(first) $(branch0) $(branch1) $(branch2) $(branch3) $(branch4) $(branch5) $(branch6) $(branch7) $(branch8)
selected := $(if $(first),$(command),)
wrapped := $(if $(selected),$(selected),-ffallback)
		`), "symbolic/Makefile", options, "")
		if err != nil {
			return snapshot{}, err
		}
		evaluator, err := scopes.compatibleSymbolicEvaluator(parsed.Variables["wrapped"])
		if err != nil {
			return snapshot{}, err
		}
		lowerer := newProbeSymbolicValueLowerer(evaluator)
		fragments, symbolic, err := lowerer.value(parsed.Variables["wrapped"])
		if err != nil {
			return snapshot{}, err
		}
		return snapshot{
			Selected: parsed.Variables["selected"], Wrapped: parsed.Variables["wrapped"],
			Symbolic: symbolic, FragmentCount: len(fragments), DependencyCount: len(lowerer.dependencies),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"selected": evaluation.Value.Selected, "wrapped": evaluation.Value.Wrapped} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Fatalf("%s = %q, want retained symbolic command graph", name, value)
		}
	}
	if got, want := len(evaluation.Plan.Nodes), 10; got != want {
		t.Fatalf("large-branch plan nodes = %d, want %d: %#v", got, want, evaluation.Plan.Nodes)
	}
	if !evaluation.Value.Symbolic || evaluation.Value.FragmentCount == 0 || evaluation.Value.DependencyCount != 10 {
		t.Fatalf("large-branch lowering = %#v, want symbolic fragments with 10 dependencies", evaluation.Value)
	}
}

func TestKbuildConditionalEmptyScalarTaintsDefinednessOnly(t *testing.T) {
	valueOnly, err := evaluateSymbolicHardeningFixture(t, `
ifeq ($(first),)
CLANG_CROSS_FLAGS :=
endif
consumer := $(CLANG_CROSS_FLAGS)
`, "consumer")
	if err != nil || valueOnly.Value["consumer"] != "" {
		t.Fatalf("value-only conditional empty assignment = %#v, %v", valueOnly.Value, err)
	}
	for name, use := range map[string]string{
		"ifdef": `ifdef CLANG_CROSS_FLAGS
consumer := defined
endif`,
		"origin":  `consumer := $(origin CLANG_CROSS_FLAGS)`,
		"flavor":  `consumer := $(flavor CLANG_CROSS_FLAGS)`,
		"value":   `consumer := $(value CLANG_CROSS_FLAGS)`,
		"default": `CLANG_CROSS_FLAGS ?= fallback`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := evaluateSymbolicHardeningFixture(t, `
ifeq ($(first),)
CLANG_CROSS_FLAGS :=
endif
`+use, "consumer")
			if err == nil || !strings.Contains(err.Error(), "probe-dependent") {
				t.Fatalf("identity-sensitive use error = %v", err)
			}
		})
	}
}

func TestKbuildSymbolicTextTransformsAreDistinctAndReplayExactly(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	opts := KbuildProbeWorkloadOptions{Target: target}
	type topology struct {
		Extension string
		Prefixed  string
		Generated []string
		Targets   []string
	}
	workload := func(scopes *KbuildProbeScopes) (topology, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC": opts.Target.Tools["cc"], "RUSTC": opts.Target.Tools["rustc"], "obj": "out",
			},
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
			CaptureVariables: []string{"extension", "prefixed"},
		})
		if err != nil {
			return topology{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(`
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name macros --crate-type proc-macro - </dev/null)
extension := $(patsubst libmacros.%,%,$(name))
prefixed := $(addprefix out/,$(name))
always-y += $(name)
$(obj)/$(name): FORCE
`), "rust/Makefile", options, "")
		if err != nil {
			return topology{}, err
		}
		result := topology{Extension: parsed.Variables["extension"], Prefixed: parsed.Variables["prefixed"]}
		for _, generated := range parsed.Generated {
			result.Generated = append(result.Generated, generated.Target)
		}
		for _, rule := range parsed.Rules {
			result.Targets = append(result.Targets, rule.Targets...)
		}
		return result, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	extension, prefixed := discovery.Value.Extension, discovery.Value.Prefixed
	if extension == prefixed || !linuxProbeSymbolPattern.MatchString(extension) || !linuxProbeSymbolPattern.MatchString(prefixed) {
		t.Fatalf("derived text atoms = extension %q, prefixed %q", extension, prefixed)
	}
	node := discovery.Plan.Nodes[0]
	request := discovery.Plan.Requests[node.RequestID]
	if !request.Outcome.PathComponent {
		t.Fatalf("rust filename request has no single-component contract: %#v", request.Outcome)
	}
	for _, argument := range request.Steps[0].Arguments {
		if linuxProbeSymbolPattern.MatchString(argument) {
			t.Fatalf("raw symbolic token reached runner argv: %#v", request.Steps[0])
		}
	}
	replayText := func(text string) (*KbuildProbeEvaluation[topology], error) {
		root := filepath.Join(t.TempDir(), "target")
		if err := os.MkdirAll(filepath.Join(root, "results"), 0o755); err != nil {
			t.Fatal(err)
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: "target", ToolsetIdentity: discovery.Plan.Toolsets["target"], Kind: "text", Text: text,
			Steps: []ProbeStepResult{{Name: request.Steps[0].Name, Status: "success", ExitCode: 0, Stdout: text + "\n"}},
		}
		data, err := result.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(root, "results", node.ID+".json"), data, 0o600); err != nil {
			return nil, err
		}
		oracle, err := NewProbeResultOracleFromTrees(map[string]string{"target": root}, discovery.Plan.Toolsets)
		if err != nil {
			return nil, err
		}
		return EvaluateKbuildProbeWorkload(opts, oracle, workload)
	}
	if _, err := replayText("../escape"); err == nil || !strings.Contains(err.Error(), "safe path component") {
		t.Fatalf("unsafe rust filename replay error = %v", err)
	}
	replay, err := replayText("libmacros.so")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Value.Extension != "so" || replay.Value.Prefixed != "out/libmacros.so" ||
		!slices.Equal(replay.Value.Generated, []string{"libmacros.so"}) ||
		!slices.Equal(replay.Value.Targets, []string{"out/libmacros.so"}) {
		t.Fatalf("replayed transforms = %#v", replay.Value)
	}
}

func TestKbuildTransformedTextChainLowersIntoCompilerProbeArgv(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	target := testKbuildProbeScopeOptions(t, fixture)
	target.Tools["rustc"] = "/configured/target/rustc"
	opts := KbuildProbeWorkloadOptions{Target: target}
	evaluation, err := EvaluateKbuildProbeWorkload(opts, nil, func(scopes *KbuildProbeScopes) (string, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{
				"CC": opts.Target.Tools["cc"], "RUSTC": opts.Target.Tools["rustc"],
			},
			MakeVariablesComplete: true, CaptureVariables: []string{"downstream"},
		})
		if err != nil {
			return "", err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
name := $(shell MAKEFLAGS= $(RUSTC) --print file-names --crate-name macros --crate-type proc-macro - </dev/null)
extension := $(patsubst libmacros.%,%,$(name))
filtered := $(filter s%,$(extension))
downstream := $(call cc-option,$(filtered))
`), "rust/Makefile", options, "")
		if err != nil {
			return "", err
		}
		return parsed.Variables["downstream"], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Plan.Nodes) != 3 {
		t.Fatalf("transformed compiler plan nodes = %#v, want baseline cc, rustc text, and dependent cc", evaluation.Plan.Nodes)
	}
	request := evaluation.Plan.Requests[evaluation.Plan.Nodes[2].RequestID]
	if len(request.Steps) != 1 || len(request.Steps[0].ArgumentFragments) != 1 {
		t.Fatalf("transformed compiler request = %#v", request)
	}
	fragments := request.Steps[0].ArgumentFragments[0].Fragments
	if len(fragments) != 1 || fragments[0].Value != "${result:00000000.text}" || len(fragments[0].Transforms) != 2 {
		t.Fatalf("transformed compiler fragments = %#v, want one text result and patsubst/filter chain", fragments)
	}
	if got, want := []string{fragments[0].Transforms[0].Function, fragments[0].Transforms[1].Function}, []string{"patsubst", "filter"}; !slices.Equal(got, want) {
		t.Fatalf("transformed compiler functions = %q, want %q", got, want)
	}
	for _, transform := range fragments[0].Transforms {
		if transform.InputArgument < 0 || transform.InputArgument >= len(transform.Arguments) || transform.Arguments[transform.InputArgument] != "" {
			t.Fatalf("transformed compiler input slot = %#v", transform)
		}
	}
}
