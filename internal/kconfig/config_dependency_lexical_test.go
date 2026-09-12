package kconfig

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestConfigDependencyCompilerLexicalEvidenceIdentity(t *testing.T) {
	plan, _ := configDependencyCompilePlanForTest(t, nil, nil, nil)
	positive := true
	plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
		return positive, true, nil
	}
	seen := map[string]string{}
	for _, test := range []struct {
		name, scope, role, language string
		arguments, units            []string
		environment                 map[string]string
		positive                    bool
	}{
		{"base", "target", "cc", "c", []string{"-DONE", "-UTWO"}, nil, nil, true},
		{"negative", "target", "cc", "c", []string{"-DONE", "-UTWO"}, nil, nil, false},
		{"scope", "host", "cc", "c", []string{"-DONE", "-UTWO"}, nil, nil, true},
		{"role", "target", "cxx", "c", []string{"-DONE", "-UTWO"}, nil, nil, true},
		{"language", "target", "cc", "assembler-with-cpp", []string{"-DONE", "-UTWO"}, nil, nil, true},
		{"order", "target", "cc", "c", []string{"-UTWO", "-DONE"}, nil, nil, true},
		{"units", "target", "cc", "c", []string{"-DONE", "-UTWO"}, []string{"driver.c"}, nil, true},
		{"environment", "target", "cc", "c", []string{"-DONE", "-UTWO"}, nil, map[string]string{"MODE": "one"}, true},
	} {
		positive = test.positive
		evidence, err := actionPlanCompilerLexicalEvidence(plan, test.scope, test.role, test.language, test.arguments, test.units, test.environment)
		if err != nil || !evidence.ready || evidence.dollarPunctuation != positive || evidence.identity == "" {
			t.Fatalf("%s: invalid evidence %#v, %v", test.name, evidence, err)
		}
		if previous, exists := seen[evidence.identity]; exists {
			t.Fatalf("%s borrowed identity from %s", test.name, previous)
		}
		seen[evidence.identity] = test.name
	}
	for _, test := range []struct {
		name  string
		ready bool
		err   error
	}{
		{"pending", false, nil},
		{"failure", true, errors.New("invalid result")},
	} {
		plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
			return true, test.ready, test.err
		}
		evidence, err := actionPlanCompilerLexicalEvidence(plan, "target", "cc", "c", nil, nil, nil)
		if !errors.Is(err, test.err) || evidence.ready || evidence.dollarPunctuation || evidence.identity != "" {
			t.Fatalf("%s supplied partial lexical evidence: %#v, %v", test.name, evidence, err)
		}
	}
}

func TestConfigDependencyCompilerLexicalRegistrationBeforePredefinesReady(t *testing.T) {
	plan, _ := configDependencyCompilePlanForTest(t, nil, nil, nil)
	plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
		return "", false, nil
	}
	arguments := []string{"-fdollars-in-identifiers", "-fno-dollars-in-identifiers"}
	units := []string{"dynamic.S"}
	environment := map[string]string{"MODE": "exact"}
	calls := 0
	plan.metadata.compilerDollarPunctuation = func(scope, role, language string, args, sources []string, env map[string]string) (bool, bool, error) {
		calls++
		if scope != "host" || role != "cc" || language != "assembler-with-cpp" ||
			!slices.Equal(args, arguments) || !slices.Equal(sources, units) || !maps.Equal(env, environment) {
			t.Fatalf("changed lexical context: %s/%s/%s/%q/%q/%v", scope, role, language, args, sources, env)
		}
		return true, false, nil
	}
	cache := map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{}
	for range 2 {
		result, err := actionPlanCompilerPredefines(plan, cache, "host", "cc", "assembler-with-cpp", arguments, units, environment)
		if err != nil || result.ready || result.lexical.dollarPunctuation || result.lexical.identity != "" {
			t.Fatalf("pending registration supplied scanner authority: %#v, %v", result, err)
		}
	}
	if calls != 1 {
		t.Fatalf("discovery lexical registrations = %d, want one cached request", calls)
	}
}

func TestConfigDependencyCompilerLexicalWitnessRequiresExactReadyRequest(t *testing.T) {
	plan, node := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "CONFIG_DRIVER\n",
	}, []string{"-nostdinc", "-c", "drivers/example/driver.c"}, nil)
	configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
	plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
		t.Fatal("witness must not register a new query")
		return false, false, nil
	}
	invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, plan.Recipes[node.Recipe])
	if reason != "" {
		t.Fatal(reason)
	}
	sources, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	probe, reason := configDependencyCompilerPredefineProbeForInvocation(invocation, sources)
	if reason != "" {
		t.Fatal(reason)
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	context := newConfigDependencyAnalysisContext(plan)
	for _, test := range []struct {
		name   string
		result configDependencyCompilerPredefineRequestResult
		valid  bool
	}{
		{"missing lexical", configDependencyCompilerPredefineRequestResult{ready: true}, false},
		{"pending request", configDependencyCompilerPredefineRequestResult{lexical: configDependencyCompilerLexicalEvidence{identity: "exact", ready: true}}, false},
		{"pending lexical", configDependencyCompilerPredefineRequestResult{ready: true, lexical: configDependencyCompilerLexicalEvidence{identity: "exact"}}, false},
		{"empty identity", configDependencyCompilerPredefineRequestResult{ready: true, lexical: configDependencyCompilerLexicalEvidence{ready: true}}, false},
		{"measured negative", configDependencyCompilerPredefineRequestResult{ready: true, lexical: configDependencyCompilerLexicalEvidence{identity: "exact", ready: true}}, true},
	} {
		context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{key: test.result}
		identity, valid := configDependencyCompilerLexicalWitness(plan, node, invocation, context)
		if valid != test.valid || valid && identity != "exact" {
			t.Fatalf("%s: witness = %q/%t", test.name, identity, valid)
		}
	}
	for _, test := range []struct {
		name, scope, language string
		arguments             []string
		environment           map[string]string
	}{
		{"scope", "host", probe.language, probe.arguments, probe.environment},
		{"language", actionPlanConfigDependencyScope(node), "assembler-with-cpp", probe.arguments, probe.environment},
		{"arguments", actionPlanConfigDependencyScope(node), probe.language, append(slices.Clone(probe.arguments), "-fno-dollars-in-identifiers"), probe.environment},
		{"environment", actionPlanConfigDependencyScope(node), probe.language, probe.arguments, map[string]string{"MODE": "other"}},
	} {
		otherKey := configDependencyCompilerPredefineKey(test.scope, invocation.tool, test.language, test.arguments, probe.translationUnits, test.environment)
		if otherKey == key {
			t.Fatal("fixture did not change context")
		}
		context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{
			otherKey: {ready: true, lexical: configDependencyCompilerLexicalEvidence{identity: "sibling", ready: true, dollarPunctuation: true}},
		}
		if _, valid := configDependencyCompilerLexicalWitness(plan, node, invocation, context); valid {
			t.Fatalf("%s borrowed a sibling's lexical witness", test.name)
		}
	}
}

func TestConfigDependencyCompilerLexicalCompletedCacheTracksAnswers(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "drivers/example/driver.c", "#define PICK(x) CONFIG_ ## x\nPICK(DRIVER)\n")
	cache := NewActionPlanFamilyPlanningCache()
	for index, answer := range []bool{false, true, false} {
		plan, node := configDependencyCompletedCachePlanForTest(t, root, "lexical-profile", fmt.Sprintf("lexical-%d", index), "src-00000001", "image", "target-toolset", map[string]string{})
		configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
		plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) { return "", true, nil }
		plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) { return answer, true, nil }
		plan.compilerProbeInvocations = map[string]actionRecipeCompilerProbeInvocation{node.ID: {Tool: "cc", Arguments: slices.Clone(plan.Recipes[node.Recipe].Arguments)}}
		set := configDependencyBuildWithFamilyCacheForTest(t, plan, cache)[node.ID]
		if set.Opaque || !slices.Equal(set.Symbols, []string{"CONFIG_DRIVER"}) {
			t.Fatalf("completed lexical variant %d = %#v", index, set)
		}
		if index < 2 && cache.configDependencies.completed.hits != 0 {
			t.Fatal("changed lexical answer reused an old completed closure")
		}
	}
	if cache.configDependencies.completed.hits != 1 || cache.configDependencies.completed.stores != 2 {
		t.Fatalf("completed cache hits/stores = %d/%d, want 1/2", cache.configDependencies.completed.hits, cache.configDependencies.completed.stores)
	}
}
