package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompilerPredefineForcedSourceOwnership(t *testing.T) {
	const first = "driver.c"
	const forced = "headers/helper.C"
	for _, test := range []struct {
		name      string
		arguments []string
		want      []string
	}{
		{"separated include", []string{"-include", forced, "-c", first}, []string{first}},
		{"joined include", []string{"-include" + forced, "-c", first}, []string{first}},
		{"imacros", []string{"-imacros", forced, "-c", first}, []string{first}},
		{"rooted include", []string{"-include", "__LINUX_BZL_SOURCE_TREE__/" + forced, "-c", first}, []string{first}},
		{"source binding", []string{"-include", "${source:forced}", "-c", "${source:first}"}, []string{first}},
		{"forced and positional", []string{"-include", forced, "-c", first, forced}, []string{first, forced}},
		{"mixed static and later dynamic TU", []string{"-include", forced, "-c", first, "second.c"}, []string{first, "second.c"}},
		{"unresolved positional word", []string{"-include", forced, "-c", first, "${result:00000000.text}"}, []string{first, forced}},
		{"unresolved Make word", []string{"-include", forced, "-c", first, "$(UNRESOLVED)"}, []string{first, forced}},
		{"unresolved private word", []string{"-include", forced, "-c", first, linuxProbeSymbolPrefix + strings.Repeat("a", 64)}, []string{first, forced}},
		{"include-looking scalar operand", []string{"-D", "-include", forced, "-c", first}, []string{first, forced}},
		{"unproven source binding", []string{"-include", "${source:unknown}", "-c", first}, []string{first, forced}},
		{"not a forced include", []string{"-I", forced, "-c", first}, []string{first, forced}},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := []string{first, forced}
			if test.name == "mixed static and later dynamic TU" {
				paths = append(paths, "second.c")
			}
			originalPaths, originalArguments := slices.Clone(paths), slices.Clone(test.arguments)
			invocation := configDependencyCompilerInvocation{
				tool: "cc", arguments: test.arguments, kbuildEnd: len(test.arguments), configuredContract: true,
				sourceBindings: map[string]ActionPlanSource{
					"${source:forced}": {Namespace: "kernel", Path: forced},
					"${source:first}":  {Namespace: "kernel", Path: first},
				},
			}
			got := configDependencyCompilerPredefineSourcePaths(invocation, paths)
			if !slices.Equal(got, test.want) {
				t.Fatalf("source ownership = %#v, want %#v", got, test.want)
			}
			if !slices.Equal(paths, originalPaths) || !slices.Equal(test.arguments, originalArguments) {
				t.Fatal("source ownership mutated input slices")
			}
		})
	}
}

func TestCompilerPredefineForcedSourceOwnershipPreservesDynamicTUChecks(t *testing.T) {
	for _, sameFile := range []bool{false, true} {
		t.Run(fmt.Sprint(sameFile), func(t *testing.T) {
			first, second, forced := "first.c", "other/second.c", "headers/helper.C"
			if sameFile {
				second = forced
			}
			concrete := []string{"-include", forced, "-c", first, second}
			invocation := configDependencyCompilerInvocation{
				tool: "cc", arguments: concrete, kbuildEnd: len(concrete), configuredContract: true,
			}
			paths := []string{first, forced}
			if !sameFile {
				paths = append(paths, second)
			}
			candidates := configDependencyCompilerPredefineSourcePaths(invocation, paths)
			invocation.arguments = []string{"-c", first, "${result:00000000.text}"}
			invocation.kbuildEnd = len(invocation.arguments)
			probe, reason := configDependencyCompilerPredefineProbeForInvocation(invocation, candidates)
			if reason != "" || !slices.Equal(probe.translationUnits, []string{second}) {
				t.Fatalf("dynamic projection = %#v, %q", probe, reason)
			}
			// The dynamic fragment still has exact input authority. Removing it
			// or repeating its TU must fail, even with another visible source.
			for _, argv := range [][]string{
				{"-include", forced, second},
				{"-include", forced},
				{"-include", forced, second, second},
			} {
				_, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerPredefines, argv, probe.translationUnits)
				// Reusing one spelling as both forced and positional remains
				// unsupported by the existing strict occurrence-count proof.
				wantError := sameFile || len(argv) != 3
				if (err != nil) != wantError {
					t.Fatalf("argv %#v error=%v, wantError=%v", argv, err, wantError)
				}
			}
		})
	}
}

func TestCompilerPredefineForcedSourceOwnershipPrecedesLanguageInference(t *testing.T) {
	for _, forced := range []string{"helper.C", "helper.S", "helper.s"} {
		args := []string{"-include", forced, "-c", "driver.c"}
		invocation := configDependencyCompilerInvocation{tool: "cc", arguments: args, kbuildEnd: len(args), configuredContract: true}
		paths := configDependencyCompilerPredefineSourcePaths(invocation, []string{"driver.c", forced})
		probe, reason := configDependencyCompilerPredefineProbeForInvocation(invocation, paths)
		if reason != "" || probe.language != "c" || len(probe.translationUnits) != 0 {
			t.Fatalf("forced %s changed C initial-state projection: %#v, %q", forced, probe, reason)
		}
	}
}

func TestActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t *testing.T) {
	for name, expression := range map[string]string{
		"direct": "$(call cc-option,-ffixedpoint-selected) " +
			"$(same-value-one) $(same-value-two)",
		"word transformed": "$(strip $(call cc-option,-ffixedpoint-selected)) " +
			"$(same-value-one) $(same-value-two)",
		"composite word list": "$(strip -DLEFT=left $(call cc-option,-ffixedpoint-selected) " +
			"-DMIDDLE=middle $(same-value-one) $(same-value-two) -DRIGHT=right)",
		"probe-dependent forced C input": "$(strip $(call cc-option,-ffixedpoint-selected) " +
			"-include $(srctree)/forced.c)",
		"probe-dependent forced C++ input": "$(strip $(call cc-option,-ffixedpoint-selected) " +
			"-include $(srctree)/forced.C)",
		"probe-dependent imacros assembly input": "$(strip $(call cc-option,-ffixedpoint-selected) " +
			"-imacros $(srctree)/forced.S)",
	} {
		t.Run(name, func(t *testing.T) {
			testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t, expression, "")
		})
	}
}

func TestActionPlanConfigProbeForcedCPrerequisiteDoesNotGainPositionalAuthority(t *testing.T) {
	testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t,
		"$(strip $(call cc-option,-ffixedpoint-selected) -include $(srctree)/forced.c)", "forced.c")
}

func TestActionPlanConfigProbeExtraSourcePrerequisiteDoesNotBecomeTranslationUnit(t *testing.T) {
	for _, prerequisite := range []string{"forced.c", "forced.C", "forced.S", "forced.s"} {
		t.Run(prerequisite, func(t *testing.T) {
			testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t,
				"$(strip $(call cc-option,-ffixedpoint-selected))", prerequisite)
		})
	}
}

func testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t *testing.T, flagExpression string, extraPrerequisite string) {
	const target = "driver.o"
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	mustWriteSource(t, root, "driver.c", "#if CONFIG_FIXEDPOINT\nint selected;\n#endif\n")
	mustWriteSource(t, root, "forced.c", "#define FORCED_SOURCE_INPUT 1\n")
	mustWriteSource(t, root, "forced.C", "#define FORCED_SOURCE_INPUT 1\n")
	mustWriteSource(t, root, "forced.S", "#define FORCED_SOURCE_INPUT 1\n")
	mustWriteSource(t, root, "forced.s", "#define FORCED_SOURCE_INPUT 1\n")
	prerequisites := "driver.c"
	if extraPrerequisite != "" {
		prerequisites += " " + extraPrerequisite
	}
	if err := os.WriteFile(makefile, []byte(fmt.Sprintf(`
TMPOUT = .tmp_$$$$
try-run = $(shell set -e; TMP=$(TMPOUT)/tmp; trap "rm -rf $(TMPOUT)" EXIT; mkdir -p $(TMPOUT); if ($(1)) >/dev/null 2>&1; then echo "$(2)"; else echo "$(3)"; fi)
__cc-option = $(call try-run,$(1) -Werror $(2) -c -x c /dev/null -o "$$TMP",$(2),$(3))
cc-option = $(call __cc-option,$(CC),$(1),$(2))
if_changed = $(cmd_$(1))

export FIXEDPOINT_WRAPPER_FLAG := $(strip $(call cc-option,-ffixedpoint-environment))
same-value-one := $(call try-run,$(CC) -Werror -ffixedpoint-identity-one -c -x c /dev/null -o "$$TMP",-ffixedpoint-same,)
same-value-two := $(call try-run,$(CC) -Werror -ffixedpoint-identity-two -c -x c /dev/null -o "$$TMP",-ffixedpoint-same,)
ccflags-y := %s
cmd_cc_o_c = $(CC) -nostdinc $(ccflags-y) -c -o $@ $<

driver.o: %s FORCE
	$(call if_changed,cc_o_c)
`, flagExpression, prerequisites)), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type workloadValue struct {
		plan *ActionPlan
	}
	evaluate := func(oracle *ProbeResultOracle, fullDiscovery bool) (*KbuildProbeEvaluation[workloadValue], error) {
		return EvaluateKbuildProbeWorkload(probeOptions, oracle, func(scopes *KbuildProbeScopes) (workloadValue, error) {
			options, err := scopes.Options("target", KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"CC":      KbuildActionRoleToken("target", "cc"),
					"SRCARCH": "x86",
					"srctree": "__LINUX_BZL_SOURCE_TREE__",
				},
				SourceRoots: map[string]string{
					"__LINUX_BZL_SOURCE_TREE__": root,
					"__LINUX_BZL_OBJECT_TREE__": root,
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
				SkipExportedVariables:   true,
			})
			if err != nil {
				return workloadValue{}, err
			}
			parsed, err := ParseKbuildFileTree(makefile, options)
			if err != nil {
				return workloadValue{}, err
			}
			profile, err := NewCompactKbuildProfile("build:root", makefile, root, parsed)
			if err != nil {
				return workloadValue{}, err
			}
			profile.EntryTargets = []string{target}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				return workloadValue{}, err
			}
			metadata := &CompactMetadata{
				Config: CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{profile},
					KbuildSelections: []CompactKbuildSelection{{
						Profile: profile.Name, Target: target, MakeTarget: target,
						Lifecycle: "target", Scope: "target", Stage: "target",
					}},
				},
				configFragment: map[string]string{
					"CONFIG_FIXEDPOINT":  "y",
					"CONFIG_MODULES":     "n",
					"CONFIG_MODVERSIONS": "n",
				},
				actionRoles: testConfiguredScopedActionRoles,
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "target", Role: "cc"}: {},
				},
				preconfiguredObjectTree: true,
				selectedProductsOnly:    true,
			}
			if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
				return workloadValue{}, err
			}
			if oracle == nil {
				if fullDiscovery {
					plan, _, err := metadata.lowerSelectedActionPlan(bootstrapTestIdentity, bootstrapTestIdentity, true, nil)
					if err == nil {
						_, err = BuildActionPlanConfigDependencyAnalysis(plan)
					}
					return workloadValue{}, err
				}
				return workloadValue{}, metadata.DiscoverActionPlanProbes(bootstrapTestIdentity, bootstrapTestIdentity)
			}
			plan, _, err := metadata.ActionPlanWithConfigDependencies(bootstrapTestIdentity, bootstrapTestIdentity)
			return workloadValue{plan: plan}, err
		})
	}

	discovery, err := evaluate(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	fullDiscovery, err := evaluate(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discovery.Plan, fullDiscovery.Plan) {
		t.Fatal("registration-only discovery changed exact requests, ordered dependencies, terminals or scope/toolsets")
	}
	foundDependentPredefines := false
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if len(request.Steps) == 1 && request.Steps[0].Name == "compiler-predefines" {
			foundDependentPredefines = request.InputCount != 0 && len(node.Inputs) != 0
		}
	}
	if !foundDependentPredefines {
		t.Fatalf("discovery plan has no compiler-predefines request dependent on cc-option: %#v", discovery.Plan.Nodes)
	}
	if extraPrerequisite != "" {
		for _, request := range discovery.Plan.Requests {
			if len(request.Steps) != 1 || request.Steps[0].Name != "compiler-predefines" {
				continue
			}
			candidate := request.Steps[0].Candidate
			if candidate == nil || len(candidate.TranslationUnits) != 0 {
				t.Fatalf("extra prerequisite acquired positional authority: %#v", candidate)
			}
			if !slices.Contains(request.Steps[0].Arguments, "c") {
				t.Fatal("extra prerequisite changed the C compiler language")
			}
		}
	}

	oracle := successfulProbeOracleForFixedPointTest(t, discovery.Plan)
	replay, err := evaluate(oracle, false)
	if err != nil {
		t.Fatalf("replay introduced a compiler-predefines request absent from discovery: %v", err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatalf("replay probe plan differs from discovery\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
	if replay.Value.plan == nil {
		t.Fatal("replay returned no action plan")
	}
}

func successfulProbeOracleForFixedPointTest(t *testing.T, plan *ProbePlan) *ProbeResultOracle {
	t.Helper()
	results := make(map[string]ProbeResult, len(plan.Nodes))
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		steps := make([]ProbeStepResult, len(request.Steps))
		for index, step := range request.Steps {
			steps[index] = ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
		}
		result := ProbeResult{
			Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
			Scope: node.Scope, ToolsetIdentity: plan.Toolsets[node.Scope], Steps: steps,
		}
		switch request.Outcome.Kind {
		case "boolean":
			value := true
			// Empty successful stdout is not the lexical proof. Keep this
			// fixture's negative measurement consistent with its recorded step.
			if len(request.Steps) == 1 && request.Steps[0].Name == "compiler-dollar-punctuation" {
				value = false
			}
			result.Kind = "boolean"
			result.Boolean = &value
		case "text":
			const value = "#define FIXEDPOINT_COMPILER 1\n"
			result.Kind = "text"
			result.Text = value
			for index := range result.Steps {
				if result.Steps[index].Name == request.Outcome.Step {
					result.Steps[index].Stdout = value
				}
			}
		default:
			t.Fatalf("unexpected probe outcome kind %q", request.Outcome.Kind)
		}
		if err := result.Validate(); err != nil {
			t.Fatalf("result for node %s is invalid: %v", node.ID, err)
		}
		results[node.ID] = result
	}
	return &ProbeResultOracle{results: results, toolsets: maps.Clone(plan.Toolsets)}
}

func TestCompilerPredefineProjectionOmitsTargetDerivedKbuildUserFlagsForHost(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	options := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	type value struct {
		projected map[string]string
		ready     bool
	}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[value], error) {
		return EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (value, error) {
			targetValue, err := scopes.evaluators["target"].requestText(ProbeRequest{
				Schema: LinuxProbeRequestSchema,
				Steps: []ProbeStep{{
					Name: "target-user-flags", Tool: "cc", Arguments: []string{"--version"},
				}},
				Outcome: ProbeOutcome{
					Kind: "text", Step: "target-user-flags", Stream: "stdout", RequireSuccess: true,
				},
			})
			if err != nil {
				return value{}, err
			}
			probe, reason := configDependencyCompilerPredefineProbeForInvocation(
				configDependencyCompilerInvocation{
					tool: "cc", arguments: []string{"-nostdinc", "-c", "scripts/basic/fixdep.c"}, kbuildEnd: 3,
					configuredContract: true,
					probeEnvironment: map[string]string{
						"KBUILD_USERCFLAGS":  targetValue,
						"KBUILD_USERLDFLAGS": targetValue,
						"WRAPPER_MODE":       "exact",
					},
				},
				[]string{"scripts/basic/fixdep.c"},
			)
			if reason != "" {
				return value{}, fmt.Errorf("project host compiler predefines: %s", reason)
			}
			_, ready, err := scopes.CompilerPredefines(
				"host", "cc", probe.language, probe.arguments,
				probe.translationUnits, probe.environment,
			)
			return value{projected: maps.Clone(probe.environment), ready: ready}, err
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantEnvironment := map[string]string{"WRAPPER_MODE": "exact"}
	if discovery.Value.ready || !maps.Equal(discovery.Value.projected, wantEnvironment) {
		t.Fatalf("host predefine discovery = %#v, want unresolved with environment %#v", discovery.Value, wantEnvironment)
	}
	hostPredefine := ProbePlanNode{}
	for _, node := range discovery.Plan.Nodes {
		request := discovery.Plan.Requests[node.RequestID]
		if node.Scope != "host" || len(request.Steps) != 1 || request.Steps[0].Name != "compiler-predefines" {
			continue
		}
		hostPredefine = node
		step := request.Steps[0]
		if request.InputCount != 0 || len(node.Inputs) != 0 ||
			!maps.Equal(step.Environment, wantEnvironment) || len(step.EnvironmentFragments) != 0 {
			t.Fatalf("host compiler-predefine request retains target dependency: node=%#v request=%#v", node, request)
		}
	}
	if hostPredefine.ID == "" {
		t.Fatal("discovery plan has no host compiler-predefines request")
	}
	replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Value.ready || !maps.Equal(replay.Value.projected, wantEnvironment) {
		t.Fatalf("host predefine replay = %#v, want ready with environment %#v", replay.Value, wantEnvironment)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatalf("host predefine replay plan differs from discovery\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
}

func TestCompilerPredefineProjectionCrossScopeFallbackKeepsProbePlanStable(t *testing.T) {
	fixtures := linuxCompilerBootstrapFixtures(t)
	target := testKbuildProbeScopeOptions(t, fixtures[1])
	host := testKbuildProbeScopeOptions(t, fixtures[0])
	options := KbuildProbeWorkloadOptions{Target: target, Host: &host}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[struct{}], error) {
		return EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (struct{}, error) {
			targetValue, err := scopes.evaluators["target"].requestText(ProbeRequest{
				Schema: LinuxProbeRequestSchema,
				Steps: []ProbeStep{{
					Name: "target-realmode-flags", Tool: "cc", Arguments: []string{"--version"},
				}},
				Outcome: ProbeOutcome{
					Kind: "text", Step: "target-realmode-flags", Stream: "stdout", RequireSuccess: true,
				},
			})
			if err != nil {
				return struct{}{}, err
			}
			const (
				nodeID   = "host-compiler-projection"
				recipeID = "host-script-recipe"
				sourceID = "host-source"
			)
			metadata := &CompactMetadata{
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "host", Role: "cc"}: {},
				},
				compilerPredefines: scopes.CompilerPredefines,
			}
			node := ActionPlanNode{
				ID: nodeID, Stage: "host", Kind: "generate", Recipe: recipeID,
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: sourceID}},
			}
			plan := &ActionPlan{
				Sources: []ActionPlanSource{{ID: sourceID, Namespace: "kernel", Path: "arch/x86/realmode/rm/header.c"}},
				Recipes: map[string]ActionRecipe{recipeID: {
					Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole,
				}},
				Nodes:    []ActionPlanNode{node},
				metadata: metadata,
				compilerProbeInvocations: map[string]actionRecipeCompilerProbeInvocation{
					nodeID: {
						Tool: "cc", Arguments: []string{"-nostdinc", "-c", "arch/x86/realmode/rm/header.c"},
						Environment: map[string]string{"REALMODE_CFLAGS": targetValue},
					},
				},
			}
			return struct{}{}, registerActionPlanCompilerProbeProjection(plan, node)
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 || discovery.Plan.Nodes[0].Scope != "target" {
		t.Fatalf("cross-scope fallback registered an incomplete host probe: %#v", discovery.Plan.Nodes)
	}
	replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatalf("cross-scope fallback changed the replay probe plan\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
}

func TestCompilerPredefineUnlowerableMakeTextFailsBeforeRequestRegistration(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[struct{}], error) {
		return EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (struct{}, error) {
			evaluator := scopes.evaluators["target"]
			measured, err := evaluator.requestText(ProbeRequest{
				Schema: LinuxProbeRequestSchema,
				Steps:  []ProbeStep{{Name: "opaque-make-text", Tool: "cc", Arguments: []string{"--version"}}},
				Outcome: ProbeOutcome{
					Kind: "text", Step: "opaque-make-text", Stream: "stdout", RequireSuccess: true,
				},
			})
			if err != nil {
				return struct{}{}, err
			}
			whole, err := evaluator.renderMakeText(
				"strip", []string{measured}, measured, linuxProbeMakeTextProtocolUnusable,
			)
			if err != nil {
				return struct{}{}, err
			}
			_, _, err = scopes.CompilerPredefines(
				"target", "cc", "c", []string{whole}, []string{"source.c"}, nil,
			)
			var unsupported *compilerPredefineProjectionUnsupportedError
			if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "no proven protocol lowering") {
				return struct{}{}, fmt.Errorf("unlowerable Make text error = %v, want typed pre-request fallback", err)
			}
			return struct{}{}, nil
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("unlowerable compiler projection registered a partial request: %#v", discovery.Plan.Nodes)
	}
	replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatalf("unlowerable compiler projection changed the replay plan\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
}

func TestCompilerPredefineUnsupportedEnvironmentTemplatesFailBeforeRegistration(t *testing.T) {
	for _, testcase := range []struct {
		name, value string
		symbolic    bool
	}{
		{name: "private working root", value: "${work:root}/arch/arm64/lib/lib.a"},
		{name: "action tree", value: "${tree:prep}/arch/arm64/lib/lib.a"},
		{name: "action input", value: "${input:library:00000000}"},
		{name: "malformed", value: "${tree:prep"},
		{name: "symbolic private root", value: "${work:root}/arch/arm64/lib/", symbolic: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			fixture := linuxCompilerBootstrapFixtures(t)[1]
			options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
			evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[struct{}], error) {
				return EvaluateKbuildProbeWorkload(options, oracle, func(scopes *KbuildProbeScopes) (struct{}, error) {
					measured, err := scopes.evaluators["target"].requestText(ProbeRequest{
						Schema: LinuxProbeRequestSchema,
						Steps:  []ProbeStep{{Name: "environment-source", Tool: "cc", Arguments: []string{"--version"}}},
						Outcome: ProbeOutcome{
							Kind: "text", Step: "environment-source", Stream: "stdout", RequireSuccess: true,
						},
					})
					if err != nil {
						return struct{}{}, err
					}
					value := testcase.value
					if testcase.symbolic {
						value += measured
					}
					environment := map[string]string{"KBUILD_VMLINUX_LIBS": value, "WRAPPER_MODE": "exact"}
					projected, reason := configDependencyCompilerPredefineEnvironmentProjection(environment)
					if reason != "" || !maps.Equal(projected, environment) {
						return struct{}{}, fmt.Errorf("environment contract was dropped or rewritten: projected=%#v reason=%s", projected, reason)
					}
					_, _, err = scopes.CompilerPredefines("target", "cc", "c", nil, nil, projected)
					var unsupported *compilerPredefineProjectionUnsupportedError
					if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "KBUILD_VMLINUX_LIBS") {
						return struct{}{}, fmt.Errorf("unsupported environment error = %v, want typed pre-registration fallback", err)
					}
					// The same typed result must be handled by the optional compiler
					// projection registrar without mutating the executable recipe.
					const nodeID, recipeID, sourceID = "compiler-projection", "compiler-recipe", "src-00000001"
					node := ActionPlanNode{ID: nodeID, Stage: "target", Kind: "generate", Recipe: recipeID,
						Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: sourceID}}}
					plan := &ActionPlan{
						Sources: []ActionPlanSource{{ID: sourceID, Namespace: "kernel", Path: "arch/arm64/kernel/asm-offsets.c"}},
						Recipes: map[string]ActionRecipe{recipeID: {Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole, Environment: maps.Clone(environment)}},
						Nodes:   []ActionPlanNode{node},
						metadata: &CompactMetadata{
							actionContracts:    map[KbuildActionRoleRef]CompactKbuildActionContract{{Scope: "target", Role: "cc"}: {}},
							compilerPredefines: scopes.CompilerPredefines,
						},
						compilerProbeInvocations: map[string]actionRecipeCompilerProbeInvocation{nodeID: {
							Tool: "cc", Arguments: []string{"-nostdinc", "-c", "arch/arm64/kernel/asm-offsets.c"}, Environment: maps.Clone(environment),
						}},
					}
					if err := registerActionPlanCompilerProbeProjection(plan, node); err != nil {
						return struct{}{}, err
					}
					if !maps.Equal(plan.Recipes[recipeID].Environment, environment) || !maps.Equal(plan.compilerProbeInvocations[nodeID].Environment, environment) {
						return struct{}{}, errors.New("unsupported projection mutated the compiler environment contract")
					}
					return struct{}{}, nil
				})
			}
			discovery, err := evaluate(nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(discovery.Plan.Nodes) != 1 {
				t.Fatalf("unsupported environment registered a partial compiler request: %#v", discovery.Plan.Nodes)
			}
			replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
				t.Fatal("unsupported environment fallback changed the discovery/replay probe graph")
			}
		})
	}
}

func TestCompilerPredefineEnvironmentReplayErrorsRemainFatal(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	options := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	workload := func(scopes *KbuildProbeScopes) (struct{}, error) {
		_, _, err := scopes.CompilerPredefines("target", "cc", "c", nil, nil,
			map[string]string{"KBUILD_VMLINUX_LIBS": "arch/arm64/lib/lib.a", "WRAPPER_TOOL": "${tool:ld}"})
		return struct{}{}, err
	}
	discovery, err := EvaluateKbuildProbeWorkload(options, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("representable compiler environment registered %d requests, want one", len(discovery.Plan.Nodes))
	}
	for _, request := range discovery.Plan.Requests {
		if request.Steps[0].Environment["KBUILD_VMLINUX_LIBS"] != "arch/arm64/lib/lib.a" ||
			!slices.Equal(request.Steps[0].AuxiliaryTools, []string{"ld"}) {
			t.Fatalf("representable compiler environment changed: %#v", request.Steps[0])
		}
	}
	oracle := successfulProbeOracleForFixedPointTest(t, discovery.Plan)
	for id, result := range oracle.results {
		result.RequestID = strings.Repeat("0", 64)
		oracle.results[id] = result
	}
	_, err = EvaluateKbuildProbeWorkload(options, oracle, workload)
	var unsupported *compilerPredefineProjectionUnsupportedError
	if err == nil || errors.As(err, &unsupported) || !strings.Contains(err.Error(), "result request") {
		t.Fatalf("stale environment probe replay error = %v, want genuine replay failure", err)
	}
}

func TestActionPlanConfigProbeReplayPreservesCmdAndFixdepCompilerProjection(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprintf("compiler_options_supported_%t", supported), func(t *testing.T) {
			testActionPlanCmdAndFixdepCompilerProjection(t, supported)
		})
	}
}

func testActionPlanCmdAndFixdepCompilerProjection(t *testing.T, supported bool) {
	const (
		target  = "scripts/mod/devicetable-offsets.s"
		source  = "scripts/mod/devicetable-offsets.c"
		fixdep  = "scripts/basic/fixdep"
		depfile = "scripts/mod/.devicetable-offsets.s.d"
	)
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	mustWriteSource(t, root, source, "#if CONFIG_FIXEDPOINT\nint selected;\n#endif\n")
	if err := os.WriteFile(makefile, []byte(linearFilterCompilerFixture+`
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = set -e; $(cmd_$(1))
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
depfile = $(dot-target).d
cmd_and_fixdep = $(cmd); $(objtree)/scripts/basic/fixdep $(depfile) $@ '$(make-cmd)' > $(dot-target).cmd; rm -f $(depfile)
if_changed_dep = $(cmd_and_fixdep)

CC_FLAGS_LTO := $(call cc-option,-flto)
c_flags = -Wp,-MMD,$(depfile) -nostdinc -I$(srctree)/include -I$(objtree)/include $(call cc-option,-ffixedpoint-selected) -DKBUILD_MODFILE='"$(objtree)/asm-offsets"' -DKBUILD_BASENAME=devicetable_offsets -DKBUILD_MODNAME=devicetable_offsets -D__KBUILD_MODNAME=kmod_devicetable_offsets $(CC_FLAGS_LTO)
cmd_cc_s_c = $(CC) $(filter-out $(DEBUG_CFLAGS) $(CC_FLAGS_LTO), $(c_flags)) -fverbose-asm -S -o $@ $<

scripts/mod/devicetable-offsets.s: scripts/mod/devicetable-offsets.c FORCE
	$(call if_changed_dep,cc_s_c)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[*ActionPlan], error) {
		return EvaluateKbuildProbeWorkload(probeOptions, oracle, func(scopes *KbuildProbeScopes) (*ActionPlan, error) {
			options, err := scopes.Options("target", KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"CC":      KbuildActionRoleToken("target", "cc"),
					"SRCARCH": "x86",
					"objtree": "__LINUX_BZL_OBJECT_TREE__",
					"srctree": "__LINUX_BZL_SOURCE_TREE__",
				},
				SourceRoots: map[string]string{
					"__LINUX_BZL_SOURCE_TREE__": root,
					"__LINUX_BZL_OBJECT_TREE__": root,
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
				SkipExportedVariables:   true,
			})
			if err != nil {
				return nil, err
			}
			parsed, err := ParseKbuildFileTree(makefile, options)
			if err != nil {
				return nil, err
			}
			profile, err := NewCompactKbuildProfile("build:scripts/mod", makefile, root, parsed)
			if err != nil {
				return nil, err
			}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				return nil, err
			}
			metadata := &CompactMetadata{
				Config:         CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				configFragment: map[string]string{"CONFIG_FIXEDPOINT": "y", "CONFIG_MODVERSIONS": "n"},
				actionRoles:    testConfiguredScopedActionRoles,
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "target", Role: "cc"}: {},
				},
				preconfiguredObjectTree: true,
				selectedProductsOnly:    true,
			}
			if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
				return nil, err
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
				Recipes:  map[string]ActionRecipe{}, metadata: metadata,
			}
			fixdepProducer, err := appendActionPlanNode(plan, ActionPlanNode{
				Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
			}, ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
			})
			if err != nil {
				return nil, err
			}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				return nil, err
			}
			match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
			if err != nil || !found {
				if err == nil {
					err = fmt.Errorf("no selected rule for %s", target)
				}
				return nil, err
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forOutput("target", "objects", "vmlinux").
				forProfile(profile)
			producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
				{path: source, sourceID: sourceID},
				{path: fixdep, producer: fixdepProducer},
			})
			if err != nil {
				return nil, err
			}
			selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
			plan.selectionGraph = &compactKbuildSelectionGraph{
				profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
				selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
				materializedProducers:       map[compactKbuildSelectionKey]string{selection: producer},
				selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
				selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
			}
			if _, err := BuildActionPlanConfigDependencyAnalysis(plan); err != nil {
				return nil, err
			}
			return plan, nil
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	discoveryTarget := ActionPlanNode{}
	for _, node := range discovery.Value.Nodes {
		for _, output := range node.Outputs {
			if output.Path == target {
				discoveryTarget = node
			}
		}
	}
	if discoveryTarget.ID == "" {
		t.Fatalf("discovery action plan has no producer for %s", target)
	}
	discoveryRecipe := discovery.Value.Recipes[discoveryTarget.Recipe]
	if discoveryTarget.Kind != "generate" || discoveryRecipe.CompilerInvocation != nil {
		t.Fatalf("probe-dependent discovery action was classified reusable: node=%#v recipe=%#v", discoveryTarget, discoveryRecipe)
	}
	if _, ok := discovery.Value.compilerProbeInvocations[discoveryTarget.ID]; !ok {
		t.Fatal("probe-dependent discovery action lost its planner-only compiler projection")
	}
	foundDevicetablePredefines := false
	for _, request := range discovery.Plan.Requests {
		if len(request.Steps) != 1 || request.Steps[0].Name != "compiler-predefines" {
			continue
		}
		if request.InputCount != 0 && slices.Contains(request.Steps[0].Arguments, "-fverbose-asm") {
			foundDevicetablePredefines = true
		}
	}
	if !foundDevicetablePredefines {
		t.Fatalf(
			"discovery plan has no dependency-backed devicetable compiler-predefines request (%d requests, %d action nodes)",
			len(discovery.Plan.Requests), len(discovery.Value.Nodes),
		)
	}
	oracle := fixedPointCompilerOptionOracleForTest(t, discovery.Plan, supported)
	replay, err := evaluate(oracle)
	if err != nil {
		t.Fatalf("replay introduced a cmd_and_fixdep compiler-predefines request absent from discovery: %v", err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatalf("cmd_and_fixdep replay probe plan differs from discovery\ndiscovery: %#v\nreplay: %#v", discovery.Plan, replay.Plan)
	}
	replayTarget := ActionPlanNode{}
	for _, node := range replay.Value.Nodes {
		for _, output := range node.Outputs {
			if output.Path == target {
				replayTarget = node
			}
		}
	}
	replayRecipe := replay.Value.Recipes[replayTarget.Recipe]
	if replayTarget.ID == "" || replayTarget.Kind != "compile" || replayRecipe.CompilerInvocation == nil {
		t.Fatalf("resolved cmd_and_fixdep action did not become a typed compile: node=%#v recipe=%#v", replayTarget, replayRecipe)
	}
	assertFixedPointCompilerSourceShellWords(t, discovery.Plan, oracle, replayRecipe.CompilerInvocation.Arguments, supported)
}

func fixedPointCompilerOptionOracleForTest(t *testing.T, plan *ProbePlan, supported bool) *ProbeResultOracle {
	t.Helper()
	oracle := successfulProbeOracleForFixedPointTest(t, plan)
	changed := 0
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if request.Outcome.Kind != "boolean" || len(request.Steps) != 1 ||
			(!slices.Contains(request.Steps[0].Arguments, "-flto") && !slices.Contains(request.Steps[0].Arguments, "-ffixedpoint-selected")) {
			continue
		}
		result := oracle.results[node.ID]
		result.Boolean = &supported
		if !supported {
			result.Steps[0].Status = "failure"
			result.Steps[0].ExitCode = 1
		}
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
		oracle.results[node.ID] = result
		changed++
	}
	if changed != 2 {
		t.Fatalf("selected %d compiler-option results, want exactly LTO and the retained flag", changed)
	}
	return oracle
}

func assertFixedPointCompilerSourceShellWords(t *testing.T, plan *ProbePlan, oracle *ProbeResultOracle, concrete []string, supported bool) {
	t.Helper()
	const macro = `-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/asm-offsets"`
	end := slices.Index(concrete, "-fverbose-asm")
	if end < 0 || !slices.Contains(concrete[:end], macro) || slices.Contains(concrete[:end], "-flto") ||
		slices.Contains(concrete[:end], "-ffixedpoint-selected") != supported {
		t.Fatal("concrete generated compiler arguments lost exact macro quoting or compiler-option filtering")
	}
	// Ignore only the compiler's include/depfile paths when comparing this
	// initial-state context; unlike predefine canonicalization, the intrinsic
	// projection retains every macro replacement byte in both argv vectors.
	want, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerIntrinsic, concrete[:end], nil)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, node := range plan.Nodes {
		request := plan.Requests[node.RequestID]
		if len(request.Steps) != 1 || request.Steps[0].Name != "compiler-predefines" ||
			!slices.Contains(request.Steps[0].Arguments, "-fverbose-asm") {
			continue
		}
		step := request.Steps[0]
		inputs := make(map[string]ProbeResult, len(node.Inputs))
		for index, id := range node.Inputs {
			result, found := oracle.results[id]
			if !found {
				t.Fatal("compiler projection references an unmeasured fixture dependency")
			}
			inputs[fmt.Sprintf("%08d", index)] = result
		}
		for _, group := range step.ArgumentFragments {
			if group.Mode != ProbeArgumentFragmentsModeSourceShellWords {
				continue
			}
			if step.Candidate == nil || !slices.Contains(step.Candidate.Base, group.Index) {
				t.Fatal("source-shell fragment group lost candidate ownership")
			}
			value, err := RenderProbeDependencyFragments(group.Fragments, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(value, "-DKBUILD_MODFILE='\""+compactKbuildActionObjectTreeMarker+"/asm-offsets\"'") {
				t.Fatal("Make evaluation lost the original quoted, privately rooted macro before shell-word formation")
			}
			words, err := ParseProbeSourceShellWords(value)
			if err != nil {
				t.Fatal(err)
			}
			for _, word := range words {
				if strings.ContainsAny(word, "'\x01\x02") {
					t.Fatal("completed compiler argv retains source-shell quotes or private tree markers")
				}
			}
			if !slices.Contains(words, macro) || slices.Contains(words, "-flto") ||
				slices.Contains(words, "-ffixedpoint-selected") != supported {
				t.Fatal("rendered generated compiler flags lost their measured selection or exact string macro")
			}
			got, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerIntrinsic, words, nil)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("rendered source-shell compiler context differs from concrete argv: %v", err)
			}
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("got %d exact generated-compiler source-shell groups, want one", matched)
	}
}

func TestActionPlanConfigProbeReplayPreservesSplitSuffixCompilerProjection(t *testing.T) {
	const (
		target  = "scripts/mod/devicetable-offsets.s"
		source  = "scripts/mod/devicetable-offsets.c"
		fixdep  = "scripts/basic/fixdep"
		depfile = "scripts/mod/.devicetable-offsets.s.d"
	)
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	mustWriteSource(t, root, source, "#if CONFIG_FIXEDPOINT\nint selected;\n#endif\n")
	if err := os.WriteFile(makefile, []byte(linearFilterCompilerFixture+`
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = set -e; $(cmd_$(1))
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
depfile = $(dot-target).d
cmd_and_fixdep = $(cmd); $(objtree)/scripts/basic/fixdep $(depfile) $@ '$(make-cmd)' > $(dot-target).cmd; rm -f $(depfile)
if_changed_dep = $(cmd_and_fixdep)

CC_FLAGS_LTO := $(call cc-option,-flto)
c_flags = -Wp,-MMD,$(depfile) -nostdinc -I$(srctree)/include -I$(objtree)/include $(call cc-option,-ffixedpoint-selected) -DKBUILD_MODFILE='"$(objtree)/asm-offsets"' -DKBUILD_BASENAME=devicetable_offsets -DKBUILD_MODNAME=devicetable_offsets -D__KBUILD_MODNAME=kmod_devicetable_offsets $(CC_FLAGS_LTO)
cmd_gensymtypes = if true; then $(CC) -D__GENKSYMS__ $(c_flags) -E $< >> $(dot-target).cmd; fi
export PROBE_FLAGS = $(call cc-option,-fenvironment-selected)
cmd_cc_s_c = $(CC) -Wp,-MMD,$(depfile) -nostdinc -fverbose-asm -S -o $@ $<

scripts/mod/devicetable-offsets.s: scripts/mod/devicetable-offsets.c FORCE
	$(call if_changed_dep,cc_s_c)
	$(call cmd,gensymtypes)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[*ActionPlan], error) {
		return EvaluateKbuildProbeWorkload(probeOptions, oracle, func(scopes *KbuildProbeScopes) (*ActionPlan, error) {
			options, err := scopes.Options("target", KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"CC":      KbuildActionRoleToken("target", "cc"),
					"SRCARCH": "x86",
					"objtree": "__LINUX_BZL_OBJECT_TREE__",
					"srctree": "__LINUX_BZL_SOURCE_TREE__",
				},
				SourceRoots: map[string]string{
					"__LINUX_BZL_SOURCE_TREE__": root,
					"__LINUX_BZL_OBJECT_TREE__": root,
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
				SkipExportedVariables:   false,
			})
			if err != nil {
				return nil, err
			}
			parsed, err := ParseKbuildFileTree(makefile, options)
			if err != nil {
				return nil, err
			}
			profile, err := NewCompactKbuildProfile("build:scripts/mod", makefile, root, parsed)
			if err != nil {
				return nil, err
			}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				return nil, err
			}
			metadata := &CompactMetadata{
				Config:         CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				configFragment: map[string]string{"CONFIG_FIXEDPOINT": "y", "CONFIG_MODVERSIONS": "n"},
				actionRoles:    testConfiguredScopedActionRoles,
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "target", Role: "cc"}: {},
				},
				preconfiguredObjectTree: true,
				selectedProductsOnly:    true,
			}
			if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
				return nil, err
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
				Recipes:  map[string]ActionRecipe{}, metadata: metadata,
			}
			fixdepProducer, err := appendActionPlanNode(plan, ActionPlanNode{
				Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
			}, ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
			})
			if err != nil {
				return nil, err
			}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				return nil, err
			}
			match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
			if err != nil || !found {
				if err == nil {
					err = fmt.Errorf("no selected rule for %s", target)
				}
				return nil, err
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forOutput("target", "objects", "vmlinux").
				forProfile(profile)
			producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
				{path: source, sourceID: sourceID},
				{path: fixdep, producer: fixdepProducer},
			})
			if err != nil {
				return nil, err
			}
			selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
			plan.selectionGraph = &compactKbuildSelectionGraph{
				profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
				selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
				materializedProducers:       map[compactKbuildSelectionKey]string{selection: producer},
				selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
				selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
			}
			if _, err := BuildActionPlanConfigDependencyAnalysis(plan); err != nil {
				return nil, err
			}
			return plan, nil
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, request := range discovery.Plan.Requests {
		if len(request.Steps) == 1 && request.Steps[0].Name == "compiler-predefines" &&
			strings.Contains(strings.Join(request.Steps[0].Arguments, " "), "GENKSYMS") {
			if request.InputCount == 0 || len(request.Steps[0].ArgumentFragments)+len(request.Steps[0].ConditionalArguments) == 0 || len(request.Steps[0].EnvironmentFragments) == 0 {
				t.Fatalf("suffix discovery lost dynamic dependencies: args=%q", request.Steps[0].Arguments)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("discovery did not register the suffix compiler request")
	}
	replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
	if err != nil {
		t.Fatalf("suffix compiler replay introduced an undiscovered request: %v", err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatal("suffix replay changed registered request bytes or ordered dependencies")
	}
	var typedPrefix, opaqueSuffix bool
	for _, node := range replay.Value.Nodes {
		recipe := replay.Value.Recipes[node.Recipe]
		if node.Kind == "compile" && recipe.CompilerInvocation != nil {
			typedPrefix = true
		}
		if projection, ok := replay.Value.compilerProbeInvocations[node.ID]; ok &&
			slices.Contains(projection.Arguments, "-D__GENKSYMS__") {
			opaqueSuffix = node.Kind == "generate" && recipe.CompilerInvocation == nil
			if !linuxProbeSymbolPattern.MatchString(strings.Join(projection.Arguments, " ")) {
				t.Fatal("suffix lost its symbolic argv after replay")
			}
		}
	}
	if !typedPrefix || !opaqueSuffix {
		t.Fatalf("changed split classification: prefix=%t suffix=%t", typedPrefix, opaqueSuffix)
	}
}

func TestActionPlanConfigProbeReplayPreservesDynamicFirstCompilerProjection(t *testing.T) {
	for _, distinctSources := range []bool{false, true} {
		name := "same explicit source"
		if distinctSources {
			name = "different sources keep independent projection proofs"
		}
		t.Run(name, func(t *testing.T) {
			testActionPlanDynamicFirstCompilerProjection(t, distinctSources)
		})
	}
}

func testActionPlanDynamicFirstCompilerProjection(t *testing.T, distinctSources bool) {
	const (
		target       = "scripts/mod/devicetable-offsets.s"
		source       = "scripts/mod/devicetable-offsets.c"
		fixdep       = "scripts/basic/fixdep"
		depfile      = "scripts/mod/.devicetable-offsets.s.d"
		secondSource = "scripts/mod/second.c"
	)
	root := t.TempDir()
	makefile := filepath.Join(root, "Makefile")
	mustWriteSource(t, root, source, "#if CONFIG_FIXEDPOINT\nint selected;\n#endif\n")
	makeText := linearFilterCompilerFixture + `
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = set -e; $(cmd_$(1))
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
depfile = $(dot-target).d
cmd_and_fixdep = $(cmd); $(objtree)/scripts/basic/fixdep $(depfile) $@ '$(make-cmd)' > $(dot-target).cmd; rm -f $(depfile)
if_changed_dep = $(cmd_and_fixdep)

CC_FLAGS_LTO := $(call cc-option,-flto)
c_flags = -Wp,-MMD,$(depfile) -nostdinc -I$(srctree)/include -I$(objtree)/include $(call cc-option,-ffixedpoint-selected) -DKBUILD_MODFILE=scripts/mod/devicetable-offsets -DKBUILD_BASENAME=devicetable_offsets -DKBUILD_MODNAME=devicetable_offsets -D__KBUILD_MODNAME=kmod_devicetable_offsets $(CC_FLAGS_LTO)
cmd_gensymtypes = if true; then $(CC) -D__GENKSYMS__ $(c_flags) -E $< >> $(dot-target).cmd; fi
export PROBE_FLAGS = $(call cc-option,-fenvironment-selected)
cmd_cc_s_c = $(CC) $(filter-out $(DEBUG_CFLAGS) $(CC_FLAGS_LTO), $(c_flags)) -fverbose-asm -S -o $@ $<

scripts/mod/devicetable-offsets.s: scripts/mod/devicetable-offsets.c FORCE
	$(call if_changed_dep,cc_s_c)
	$(call cmd,gensymtypes)
`
	if distinctSources {
		mustWriteSource(t, root, secondSource, "int second;\n")
		makeText = strings.ReplaceAll(makeText, "-E $< >>", "-E "+secondSource+" >>")
		makeText = strings.ReplaceAll(makeText, source+" FORCE", source+" "+secondSource+" FORCE")
	}
	if err := os.WriteFile(makefile, []byte(makeText), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := linuxCompilerBootstrapFixtures(t)[1]
	probeOptions := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	evaluate := func(oracle *ProbeResultOracle) (*KbuildProbeEvaluation[*ActionPlan], error) {
		return EvaluateKbuildProbeWorkload(probeOptions, oracle, func(scopes *KbuildProbeScopes) (*ActionPlan, error) {
			options, err := scopes.Options("target", KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"CC":      KbuildActionRoleToken("target", "cc"),
					"SRCARCH": "x86",
					"objtree": "__LINUX_BZL_OBJECT_TREE__",
					"srctree": "__LINUX_BZL_SOURCE_TREE__",
				},
				SourceRoots: map[string]string{
					"__LINUX_BZL_SOURCE_TREE__": root,
					"__LINUX_BZL_OBJECT_TREE__": root,
				},
				ConfigVariablesComplete: true,
				MakeVariablesComplete:   true,
				CaptureTargetEvaluator:  true,
				SkipExportedVariables:   false,
			})
			if err != nil {
				return nil, err
			}
			parsed, err := ParseKbuildFileTree(makefile, options)
			if err != nil {
				return nil, err
			}
			profile, err := NewCompactKbuildProfile("build:scripts/mod", makefile, root, parsed)
			if err != nil {
				return nil, err
			}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				return nil, err
			}
			metadata := &CompactMetadata{
				Config:         CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				configFragment: map[string]string{"CONFIG_FIXEDPOINT": "y", "CONFIG_MODVERSIONS": "n"},
				actionRoles:    testConfiguredScopedActionRoles,
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "target", Role: "cc"}: {},
				},
				preconfiguredObjectTree: true,
				selectedProductsOnly:    true,
			}
			if err := scopes.BindActionPlanToolsetPathCapabilities(metadata); err != nil {
				return nil, err
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity},
				Recipes:  map[string]ActionRecipe{}, metadata: metadata,
			}
			fixdepProducer, err := appendActionPlanNode(plan, ActionPlanNode{
				Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
			}, ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
			})
			if err != nil {
				return nil, err
			}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				return nil, err
			}
			match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
			if err != nil || !found {
				if err == nil {
					err = fmt.Errorf("no selected rule for %s", target)
				}
				return nil, err
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forOutput("target", "objects", "vmlinux").
				forProfile(profile)
			inputs := []compactKbuildRuleInput{
				{path: source, sourceID: sourceID},
				{path: fixdep, producer: fixdepProducer},
			}
			if distinctSources {
				secondID, err := metadata.ensureActionPlanSource(plan, secondSource)
				if err != nil {
					return nil, err
				}
				inputs = append(inputs, compactKbuildRuleInput{path: secondSource, sourceID: secondID})
			}
			producer, err := builder.buildCommandTemplate(target, match, inputs)
			if err != nil {
				return nil, err
			}
			selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
			plan.selectionGraph = &compactKbuildSelectionGraph{
				profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
				selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
				materializedProducers:       map[compactKbuildSelectionKey]string{selection: producer},
				selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
				selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
			}
			if _, err := BuildActionPlanConfigDependencyAnalysis(plan); err != nil {
				return nil, err
			}
			return plan, nil
		})
	}

	discovery, err := evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	firstSourceShell, suffixSourceShell := false, false
	for _, request := range discovery.Plan.Requests {
		if len(request.Steps) != 1 || request.Steps[0].Name != "compiler-predefines" {
			continue
		}
		step := request.Steps[0]
		if step.Candidate == nil || len(step.Candidate.TranslationUnits) != 0 {
			t.Fatalf("compound compiler claimed a hidden source from another command: %#v", step.Candidate)
		}
		suffix := strings.Contains(strings.Join(step.Arguments, " "), "GENKSYMS")
		if suffix {
			count++
		}
		for _, group := range step.ArgumentFragments {
			if group.Mode != ProbeArgumentFragmentsModeSourceShellWords {
				continue
			}
			if step.Candidate == nil || !slices.Contains(step.Candidate.Base, group.Index) {
				t.Fatal("unsplit discovery lost compiler source-shell candidate ownership")
			}
			if suffix {
				suffixSourceShell = true
			} else if slices.Contains(step.Arguments, "-fverbose-asm") {
				firstSourceShell = true
			}
		}
	}
	if count == 0 {
		t.Fatal("dynamic first compiler suppressed discovery of the suffix compiler query")
	}
	if !suffixSourceShell || (!distinctSources && !firstSourceShell) {
		t.Fatalf("unsplit discovery lost a supported source-shell twin: first=%t suffix=%t", firstSourceShell, suffixSourceShell)
	}
	for _, node := range discovery.Value.Nodes {
		if node.Kind == "compile" || discovery.Value.Recipes[node.Recipe].CompilerInvocation != nil {
			t.Fatal("registration-only compound discovery became a typed compiler action")
		}
	}
	replay, err := evaluate(successfulProbeOracleForFixedPointTest(t, discovery.Plan))
	if err != nil {
		t.Fatalf("dynamic-first replay introduced an undiscovered request: %v", err)
	}
	if !reflect.DeepEqual(replay.Plan, discovery.Plan) {
		t.Fatal("dynamic-first compiler replay changed the probe DAG or ordered dependencies")
	}
	typed := false
	for _, node := range replay.Value.Nodes {
		recipe := replay.Value.Recipes[node.Recipe]
		if recipe.CompilerInvocation != nil {
			typed = true
			if distinctSources {
				set, err := AnalyzeActionPlanNodeConfigDependencies(replay.Value, node)
				// Unlike the suffix, the first command has dynamic filter-out
				// operands outside the bounded finite proof. Its old fail-closed
				// precision boundary must survive the new suffix query.
				if err != nil || !set.Opaque || !strings.Contains(set.Reason, "unresolved translation-unit ownership") {
					t.Fatalf("split replay bypassed unresolved first-command ownership: set=%#v err=%v", set, err)
				}
			}
		}
	}
	if !typed {
		t.Fatal("resolved first compiler did not retain ordinary typed lowering")
	}
}

func TestCompoundCompilerProbeRegistrationKeepsCommandOwnership(t *testing.T) {
	const nodeID, recipeID = "compound", "recipe"
	makeCommand := func(marker string) actionRecipeCompoundCompilerProbe {
		return actionRecipeCompoundCompilerProbe{
			Invocation: ActionRecipeCompilerInvocation{Tool: "cc", Arguments: []string{"-nostdinc", "-include", "helper.C", "-D" + marker, "-c", "source.c"}},
			Projection: actionRecipeCompilerProbeInvocation{
				Tool: "cc", Arguments: []string{"-nostdinc", "-D" + marker, "-c", "source.c"},
				Environment: map[string]string{"MODE": marker}, RequireExplicitSources: true,
			},
		}
	}
	for _, mode := range []string{"both", "opaque first", "wrong role", "stdin first", "callback error"} {
		t.Run(mode, func(t *testing.T) {
			commands := []actionRecipeCompoundCompilerProbe{makeCommand("FIRST"), makeCommand("SECOND")}
			switch mode {
			case "opaque first":
				commands[0].Projection.OpaqueReason = "unsupported command"
			case "wrong role":
				commands[0].Projection.Tool = "cxx"
			case "stdin first":
				commands[0].Projection.Arguments = []string{"-nostdinc", "-xc", "-"}
			}
			node := ActionPlanNode{ID: nodeID, Stage: "target", Kind: "generate", Recipe: recipeID,
				Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}, {Role: "source", SourceID: "src-00000002"}}}
			calls := []string{}
			failure := errors.New("exact callback error")
			plan := &ActionPlan{
				Sources: []ActionPlanSource{{ID: "src-00000001", Namespace: "kernel", Path: "source.c"}, {ID: "src-00000002", Namespace: "kernel", Path: "helper.C"}},
				Nodes:   []ActionPlanNode{node}, Toolsets: map[string]string{"target": bootstrapTestIdentity},
				Recipes: map[string]ActionRecipe{recipeID: {Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: compactKbuildScriptRunnerRole,
					Environment: map[string]string{"MODE": "execution"}}},
				compoundCompilerProbes: map[string][]actionRecipeCompoundCompilerProbe{nodeID: commands},
				metadata: &CompactMetadata{actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{{Scope: "target", Role: "cc"}: {}},
					compilerPredefines: func(scope, tool, language string, arguments, translationUnits []string, environment map[string]string) (string, bool, error) {
						if scope != "target" || tool != "cc" || language != "c" || len(translationUnits) != 0 {
							t.Fatalf("changed command ownership: scope=%s tool=%s language=%s claims=%v", scope, tool, language, translationUnits)
						}
						marker := environment["MODE"]
						if !slices.Contains(arguments, "-D"+marker) {
							t.Fatalf("command arguments/environment crossed: args=%q env=%v", arguments, environment)
						}
						calls = append(calls, marker)
						if mode == "callback error" {
							return "", false, failure
						}
						return "", false, nil
					}},
			}
			before := cloneActionRecipeCompoundCompilerProbes(commands)
			err := registerActionPlanCompilerProbeProjection(plan, node)
			if mode == "callback error" {
				if !errors.Is(err, failure) {
					t.Fatalf("callback error was hidden: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := []string{"SECOND"}
			if mode == "both" {
				want = []string{"FIRST", "SECOND"}
			} else if mode == "callback error" {
				want = []string{"FIRST"}
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("registration order=%v want=%v", calls, want)
			}
			if !reflect.DeepEqual(commands, before) || plan.Recipes[recipeID].CompilerInvocation != nil ||
				!maps.Equal(plan.Recipes[recipeID].Environment, map[string]string{"MODE": "execution"}) || len(plan.compilerProbeInvocations) != 0 {
				t.Fatal("registration mutated execution or single-compiler precision authority")
			}
		})
	}
}

func TestCompoundCompilerProbeAppendOwnsPrivatePayload(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		name := "ordinary"
		if prepared {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			recipe := actionRecipeInterningTestRecipe()
			baseline, err := recipe.ID()
			if err != nil {
				t.Fatal(err)
			}
			recipe.compoundCompilerProbes = []actionRecipeCompoundCompilerProbe{{
				Invocation: ActionRecipeCompilerInvocation{Tool: "cc", Arguments: []string{"-c", "source.c"}, WorkingInputUses: []string{"source:00000000"}},
				Projection: actionRecipeCompilerProbeInvocation{Tool: "cc", Arguments: []string{"-DORIGINAL"}, Environment: map[string]string{"MODE": "original"}, RequireExplicitSources: true},
			}}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			node := actionRecipeInterningTestNode(0)
			var producer string
			if prepared {
				producer, err = appendPreparedActionPlanNode(plan, node, recipe, nil)
			} else {
				producer, err = appendActionPlanNode(plan, node, recipe)
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.Nodes[0].Recipe != baseline || len(plan.Recipes[baseline].compoundCompilerProbes) != 0 || len(plan.compilerProbeInvocations) != 0 {
				t.Fatal("private compound discovery payload changed executable identity or leaked into precision authority")
			}
			recipe.compoundCompilerProbes[0].Invocation.Arguments[0] = "changed"
			recipe.compoundCompilerProbes[0].Invocation.WorkingInputUses[0] = "changed"
			recipe.compoundCompilerProbes[0].Projection.Arguments[0] = "changed"
			recipe.compoundCompilerProbes[0].Projection.Environment["MODE"] = "changed"
			stored := plan.compoundCompilerProbes[producer][0]
			if stored.Invocation.Arguments[0] != "-c" || stored.Invocation.WorkingInputUses[0] != "source:00000000" ||
				stored.Projection.Arguments[0] != "-DORIGINAL" || stored.Projection.Environment["MODE"] != "original" || !stored.Projection.RequireExplicitSources {
				t.Fatal("plan compound payload aliases caller-owned values")
			}
			duplicate := actionRecipeInterningTestNode(1)
			duplicate.ID = producer
			if _, err := appendPreparedActionPlanNode(plan, duplicate, recipe, nil); err == nil || !strings.Contains(err.Error(), "repeats provisional node") {
				t.Fatalf("duplicate append should preserve the original payload: %v", err)
			}
			if plan.compoundCompilerProbes[producer][0].Projection.Arguments[0] != "-DORIGINAL" {
				t.Fatal("failed append replaced original compound provenance")
			}
		})
	}
}
