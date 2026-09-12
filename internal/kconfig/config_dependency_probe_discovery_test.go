package kconfig

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func configDependencyProbeDiscoveryFixture(t *testing.T, mode string) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	const source = "drivers/example/driver.c"
	files := map[string]string{source: "#if defined(__NEXT)\nCONFIG_NEXT\n#endif\n", "include/forced.C": "#define FORCED 1\n"}
	plan, node := configDependencyCompilePlanForTest(t, files,
		[]string{"-nostdinc", "-DORDER=1", "-c", source, "-o", "drivers/example/driver.o"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	if strings.HasPrefix(mode, "driver link") {
		plan, node = configDependencySourceDriverLinkPlanForTest(t, source, files, []string{"-nostdinc"}, false)
		if mode == "driver link mismatch" {
			role, _ := toolaction.LinkContractRole("cc")
			plan.metadata.actionContracts[KbuildActionRoleRef{Scope: "host", Role: role}] = CompactKbuildActionContract{SuffixArguments: []string{"-DCHANGED=1"}}
		}
	}
	plan.Toolsets = map[string]string{"target": bootstrapTestIdentity, "host": bootstrapTestIdentity}
	recipe := plan.Recipes[node.Recipe]
	if mode == "forced source" {
		recipe.Arguments = append([]string{"-include", "include/forced.C"}, recipe.Arguments...)
		plan.Sources = append(plan.Sources, ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: "include/forced.C"})
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "source", SourceID: "src-00000002"})
	}
	if mode == "fallback stdin" {
		recipe.Stdin = "unmodeled-input"
	}
	if mode == "fallback modversions" {
		plan.metadata.configFragment = map[string]string{"CONFIG_MODVERSIONS": "y"}
	}
	if mode == "fallback unsupported option" {
		recipe.Arguments = append([]string{"-Xpreprocessor", "-unmodeled"}, recipe.Arguments...)
	}
	if mode == "retained" || mode == "duplicate" || mode == "forced source" {
		plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{
			node.ID: {Tool: "cc", Arguments: slices.Clone(recipe.Arguments), Environment: maps.Clone(recipe.Environment), RequireExplicitSources: true},
		}
	}
	if mode == "compound" {
		commands := make([]actionRecipeCompoundCompilerProbe, 0, 2)
		for _, marker := range []string{"FIRST", "SECOND"} {
			arguments := append([]string{"-D" + marker + "=1"}, recipe.Arguments...)
			commands = append(commands, actionRecipeCompoundCompilerProbe{
				Invocation: ActionRecipeCompilerInvocation{Tool: "cc", Arguments: arguments, WorkingInputUsesComplete: true},
				Projection: actionRecipeCompilerProbeInvocation{Tool: "cc", Arguments: arguments, Environment: map[string]string{"MODE": marker}, RequireExplicitSources: true},
			})
		}
		plan.compoundCompilerProbes = map[string][]actionRecipeCompoundCompilerProbe{node.ID: commands}
		node.Kind, node.Tool = "generate", compactKbuildScriptRunnerRole
		recipe.Kind, recipe.Tool = node.Kind, node.Tool
		recipe.Arguments = []string{"-script_content_base64", "not-executed-by-analysis"}
	}
	plan.Recipes[node.Recipe], plan.Nodes[0] = recipe, node
	if mode == "duplicate" {
		duplicate := node
		duplicate.ID += "-duplicate"
		plan.Nodes = append(plan.Nodes, duplicate)
		plan.compilerProbeInvocations[duplicate.ID] = plan.compilerProbeInvocations[node.ID]
	}
	// Include all three initial-state protocols without inventory filesystem
	// differences between independent workload runs affecting their requests.
	plan.metadata.sourceGuardNamesReady = true
	plan.metadata.sourceGuardNames = []string{"__QUERY"}
	return plan, node
}

func TestActionPlanCompilerProbeDiscoveryMatchesFullAnalysis(t *testing.T) {
	opts := compilerDefinednessTestOptions(t)
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	opts.Host = &host
	for _, mode := range []string{"retained", "typed fallback", "duplicate", "forced source", "compound", "driver link", "driver link mismatch", "fallback stdin", "fallback modversions", "fallback unsupported option"} {
		t.Run(mode, func(t *testing.T) {
			evaluate := func(oracle *ProbeResultOracle, registrationOnly bool) (*KbuildProbeEvaluation[[]configDependencyCompilerPredefineRequestKey], error) {
				return EvaluateKbuildProbeWorkload(opts, oracle, func(scopes *KbuildProbeScopes) ([]configDependencyCompilerPredefineRequestKey, error) {
					plan, _ := configDependencyProbeDiscoveryFixture(t, mode)
					if err := scopes.BindActionPlanToolsetPathCapabilities(plan.metadata); err != nil {
						return nil, err
					}
					callback := plan.metadata.compilerPredefines
					var calls []configDependencyCompilerPredefineRequestKey
					plan.metadata.compilerPredefines = func(scope, role, language string, arguments, units []string, environment map[string]string) (string, bool, error) {
						calls = append(calls, configDependencyCompilerPredefineKey(scope, role, language, arguments, units, environment))
						return callback(scope, role, language, arguments, units, environment)
					}
					var err error
					if registrationOnly {
						err = discoverActionPlanCompilerProbes(plan)
					} else {
						_, err = BuildActionPlanConfigDependencyAnalysis(plan)
					}
					return calls, err
				})
			}
			ordinary, err := evaluate(nil, false)
			if err != nil {
				t.Fatal(err)
			}
			discovery, err := evaluate(nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(discovery.Plan, ordinary.Plan) || !slices.Equal(discovery.Value, ordinary.Value) {
				t.Fatal("registration-only discovery changed complete probe DAG or callback order")
			}
			wantCalls := 1
			if mode == "compound" {
				wantCalls = 2
			} else if strings.HasPrefix(mode, "fallback ") || mode == "driver link mismatch" {
				wantCalls = 0
			}
			if len(discovery.Value) != wantCalls {
				t.Fatalf("compiler-state requests = %d, want %d", len(discovery.Value), wantCalls)
			}
			oracle := successfulProbeOracleForFixedPointTest(t, discovery.Plan)
			for _, node := range discovery.Plan.Nodes {
				request := discovery.Plan.Requests[node.RequestID]
				if len(request.Steps) == 1 && request.Steps[0].Name == "compiler-definedness" {
					result := oracle.results[node.ID]
					result.Text, result.Steps[0].Stdout = "0\n", "0\n"
					oracle.results[node.ID] = result
				}
			}
			replay, err := evaluate(oracle, false)
			if err != nil {
				t.Fatalf("strict full replay after registration-only discovery: %v", err)
			}
			if !reflect.DeepEqual(replay.Plan, discovery.Plan) || !slices.Equal(replay.Value, discovery.Value) {
				t.Fatal("strict replay changed the discovery fixed point")
			}
		})
	}
}

func TestActionPlanCompilerProbeDiscoveryPendingRequiresExactUnreadyKey(t *testing.T) {
	plan, node := configDependencyProbeDiscoveryFixture(t, "forced source")
	recipe := plan.Recipes[node.Recipe]
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, recipe)
	if reason != "" {
		t.Fatal(reason)
	}
	paths, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	probe, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, paths)
	if reason != "" || probe.language != "c" {
		t.Fatalf("forced C++ header changed the projected C context: %#v, %s", probe, reason)
	}
	exact := configDependencyCompilerPredefineKey("target", "cc", probe.language, probe.arguments, probe.translationUnits, probe.environment)
	for _, mode := range []string{"missing", "exact pending", "exact ready", "scope", "role", "language", "arguments", "argument order", "units", "environment"} {
		t.Run(mode, func(t *testing.T) {
			scope, role, language := "target", "cc", probe.language
			arguments, units, environment := slices.Clone(probe.arguments), slices.Clone(probe.translationUnits), maps.Clone(probe.environment)
			switch mode {
			case "scope":
				scope = "host"
			case "role":
				role = "cxx"
			case "language":
				language = "assembler-with-cpp"
			case "arguments":
				arguments = append(arguments, "-DCHANGED=1")
			case "argument order":
				slices.Reverse(arguments)
			case "units":
				units = append(units, "unowned.c")
			case "environment":
				if environment == nil {
					environment = map[string]string{}
				}
				environment["MODE"] = "changed"
			}
			key := configDependencyCompilerPredefineKey(scope, role, language, arguments, units, environment)
			requests := map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
			if mode != "missing" {
				requests[key] = configDependencyCompilerPredefineRequestResult{ready: mode == "exact ready"}
			}
			if mode != "missing" && !strings.HasPrefix(mode, "exact ") && key == exact {
				t.Fatal("fixture did not change the request identity")
			}
			if got := actionPlanCompilerDiscoveryRequestPending(plan, node, recipe, requests); got != (mode == "exact pending") {
				t.Fatalf("pending skip = %t for %s", got, mode)
			}
		})
	}
}

func TestActionPlanCompilerProbeDiscoveryReadyResultsStillAnalyze(t *testing.T) {
	for _, mode := range []string{"predefines pending", "definedness pending", "lexical pending", "all ready"} {
		plan, _ := configDependencyProbeDiscoveryFixture(t, "retained")
		calls, definednessCalls, lexicalCalls, observed := 0, 0, 0, 0
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
			calls++
			return "#define COMPILER_KNOWN 1\n", mode != "predefines pending", nil
		}
		plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
			definednessCalls++
			values := map[string]bool{}
			for _, name := range names {
				values[name] = false
			}
			return values, mode != "definedness pending", nil
		}
		plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
			lexicalCalls++
			return false, mode != "lexical pending", nil
		}
		plan.metadata.compilerGuardObserver = func(ConfigDependencyCompilerGuardObservation) error {
			observed++
			return nil
		}
		if err := discoverActionPlanCompilerProbes(plan); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || definednessCalls != 1 || lexicalCalls != 1 || (observed != 0) != (mode == "all ready") {
			t.Fatalf("%s calls=%d/%d/%d observed=%d; only aggregate-unready exact state may bypass the analyzer", mode, calls, definednessCalls, lexicalCalls, observed)
		}
	}
}

func TestActionPlanCompilerProbeDiscoveryRegistrationErrorsRemainFatal(t *testing.T) {
	failure := errors.New("exact configured compiler result is missing")
	for _, registrationOnly := range []bool{false, true} {
		plan, _ := configDependencyProbeDiscoveryFixture(t, "retained")
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
			return "", false, failure
		}
		var err error
		if registrationOnly {
			err = discoverActionPlanCompilerProbes(plan)
		} else {
			_, err = BuildActionPlanConfigDependencyAnalysis(plan)
		}
		if !errors.Is(err, failure) {
			t.Fatalf("registrationOnly=%t swallowed strict query failure: %v", registrationOnly, err)
		}
	}
	if err := discoverActionPlanCompilerProbes(nil); err == nil {
		t.Fatal("nil plan accepted")
	}
}
