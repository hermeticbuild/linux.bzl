package kconfig

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestKbuildCompilerGuardAnswersRetainValueSensitivePayloads(t *testing.T) {
	for _, test := range []struct {
		name          string
		first, second []string
	}{
		{"macro name", []string{"-DX=1"}, []string{"-DY=1"}},
		{"function replacement", []string{"-DF(x)=1"}, []string{"-DF(x)=2"}},
		{"preprocessor payload", []string{"-Xpreprocessor", "-DX=first"}, []string{"-Xpreprocessor", "-DX=second"}},
		{"ordered undefinition", []string{"-DX=1", "-UX"}, []string{"-UX", "-DX=2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if CompilerGuardDefinednessSchedulingKey("target", "cc", "c", test.first, nil, nil) ==
				CompilerGuardDefinednessSchedulingKey("target", "cc", "c", test.second, nil, nil) {
				t.Fatal("pending scheduling merged a value-sensitive or differently owned payload")
			}
			scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
			_, stdin, err := compilerDefinednessSource([]string{"__ANSWER"})
			if err != nil {
				t.Fatal(err)
			}
			var requestIDs []string
			for _, arguments := range [][]string{test.first, test.second} {
				probe, err := scopes.compilerProjectedRequest("target", "cc", "c", arguments, nil, nil,
					"compiler-definedness", []string{"-E", "-P", "-x", "c", "-"}, stdin,
					ProbeOutcome{Kind: "text", Step: "compiler-definedness", Stream: "stdout", RequireSuccess: true})
				if err != nil {
					t.Fatal(err)
				}
				id, err := probe.request.ID()
				if err != nil {
					t.Fatal(err)
				}
				requestIDs = append(requestIDs, id)
			}
			if requestIDs[0] == requestIDs[1] {
				t.Fatal("negative fixture unexpectedly has an equivalent projected request")
			}
			batch, frozen := compilerGuardAnswerBatchForTest(t, scopes, test.first, nil, nil,
				[][]string{{"__ANSWER"}}, []string{"0\n"})
			answers, err := batch.Answers()
			if err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{Toolsets: maps.Clone(frozen.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
			values, identity, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", test.second, nil, nil)
			if err != nil || values != nil || identity != "" {
				t.Fatalf("distinct query borrowed an answer: %v, %q, %v", values, identity, err)
			}
		})
	}
}

// Conflicting measurements for the same projected initial context
// must not be hidden by different raw object-macro replacement values.
func TestKbuildCompilerGuardAnswersRejectProjectedContextConflict(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	queries := []struct{ arguments, names []string }{
		{[]string{"-DUNIT=first"}, []string{"__ANSWER"}},
		{[]string{"-DUNIT=second"}, []string{"__ANSWER", "__OTHER"}},
	}
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range queries {
		if _, _, err := discovery.CompilerDefinedness("target", "cc", "c", query.arguments, nil, query.names, nil); err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := discovery.Plan()
	if err != nil || len(frozen.Nodes) != 2 {
		t.Fatalf("conflict fixture plan: %v", err)
	}
	oracle := compilerDefinednessTestOracle(t, frozen, map[string]string{"compiler-definedness": "0\n"})
	for _, node := range frozen.Nodes {
		if strings.Contains(frozen.Requests[node.RequestID].Steps[0].Stdin, "__OTHER") {
			result := oracle.results[node.ID]
			result.Text, result.Steps[0].Stdout = "1 0\n", "1 0\n"
			oracle.results[node.ID] = result
		}
	}
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range queries {
		if _, ready, err := replay.CompilerDefinedness("target", "cc", "c", query.arguments, nil, query.names, nil); err != nil || !ready {
			t.Fatalf("conflict fixture replay: ready=%t, %v", ready, err)
		}
	}
	if answers, err := replay.Answers(); err == nil || answers != nil {
		t.Fatal("different raw values hid contradictory measured initial facts")
	}
}

func TestKbuildCompilerGuardAnswersReuseExistingDefinednessProjection(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	first := []string{"-nostdinc", "-DUNIT=first", "-D", "VALUE=CONFIG_FIRST", "-UREMOVED"}
	second := []string{"-nostdinc", "-DUNIT=second", "-D", "VALUE=CONFIG_SECOND", "-UREMOVED"}
	environment := map[string]string{"MODE": "exact"}
	_, stdin, err := compilerDefinednessSource([]string{"__ANSWER"})
	if err != nil {
		t.Fatal(err)
	}
	construct := func(arguments []string) *compilerProjectedProbe {
		t.Helper()
		probe, err := scopes.compilerProjectedRequest("target", "cc", "c", arguments, nil, environment,
			"compiler-definedness", []string{"-E", "-P", "-x", "c", "-"}, stdin,
			ProbeOutcome{Kind: "text", Step: "compiler-definedness", Stream: "stdout", RequireSuccess: true})
		if err != nil {
			t.Fatal(err)
		}
		return probe
	}
	left, right := construct(first), construct(second)
	if CompilerGuardDefinednessSchedulingKey("target", "cc", "c", first, nil, environment) !=
		CompilerGuardDefinednessSchedulingKey("target", "cc", "c", second, nil, environment) {
		t.Fatal("equivalent complete requests did not share the scheduling projection")
	}
	leftBytes, err := left.request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := right.request.CanonicalJSON()
	if err != nil || string(leftBytes) != string(rightBytes) || !slices.Equal(left.dependencies, right.dependencies) {
		t.Fatalf("fixture queries differ in complete request bytes or ordered dependencies: %v", err)
	}
	// Execution-shaped replay is performed only for the first raw context.
	// The second may borrow its measured name only because the existing query
	// constructor above proves these requests and dependencies identical.
	batch, probePlan := compilerGuardAnswerBatchForTest(t, scopes, first, nil, environment,
		[][]string{{"__ANSWER"}}, []string{"0\n"})
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
	var previousIdentity string
	for _, arguments := range [][]string{first, second} {
		values, identity, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", arguments, nil, environment)
		if err != nil || identity == "" || !maps.Equal(values, map[string]bool{"__ANSWER": false}) {
			t.Fatalf("equivalent query did not retrieve measured initial fact: %v, %q, %v", values, identity, err)
		}
		if previousIdentity != "" && identity != previousIdentity {
			t.Fatal("equivalent query changed the measurement identity")
		}
		previousIdentity = identity
	}
}

func TestCompilerGuardDefinednessSchedulingKeepsContextFields(t *testing.T) {
	base := CompilerGuardDefinednessSchedulingKey("target", "cc", "c", []string{"-DUNIT=first"}, nil, map[string]string{"MODE": "one"})
	for name, key := range map[string][32]byte{
		"scope":        CompilerGuardDefinednessSchedulingKey("host", "cc", "c", []string{"-DUNIT=first"}, nil, map[string]string{"MODE": "one"}),
		"role":         CompilerGuardDefinednessSchedulingKey("target", "ld", "c", []string{"-DUNIT=first"}, nil, map[string]string{"MODE": "one"}),
		"language":     CompilerGuardDefinednessSchedulingKey("target", "cc", "assembler-with-cpp", []string{"-DUNIT=first"}, nil, map[string]string{"MODE": "one"}),
		"units":        CompilerGuardDefinednessSchedulingKey("target", "cc", "c", []string{"-DUNIT=first"}, []string{"other.c"}, map[string]string{"MODE": "one"}),
		"environment":  CompilerGuardDefinednessSchedulingKey("target", "cc", "c", []string{"-DUNIT=first"}, nil, map[string]string{"MODE": "two"}),
		"codegen flag": CompilerGuardDefinednessSchedulingKey("target", "cc", "c", []string{"-DUNIT=first", "-m32"}, nil, map[string]string{"MODE": "one"}),
	} {
		if key == base {
			t.Errorf("scheduling dropped %s", name)
		}
	}
}

func compilerGuardAnswerBatchForTest(t *testing.T, scopes *KbuildProbeScopes,
	arguments, units []string, environment map[string]string, batches [][]string, texts []string,
) (*KbuildCompilerGuardBatch, *ProbePlan) {
	t.Helper()
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range batches {
		if _, _, err := discovery.CompilerDefinedness("target", "cc", "c", arguments, units, names, environment); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != len(texts) {
		t.Fatalf("answer fixture plan = %#v %v", plan, err)
	}
	oracle := compilerDefinednessTestOracle(t, plan, map[string]string{"compiler-definedness": "0\n"})
	for index, node := range plan.Nodes {
		result := oracle.results[node.ID]
		result.Text, result.Steps[0].Stdout = texts[index], texts[index]
		oracle.results[node.ID] = result
	}
	replay, err := NewKbuildCompilerGuardBatch(scopes, oracle)
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range batches {
		values, ready, err := replay.CompilerDefinedness("target", "cc", "c", arguments, units, names, environment)
		if err != nil || !ready {
			t.Fatalf("answer fixture replay = %#v %t %v", values, ready, err)
		}
		// The snapshot must not retain a caller-owned result map.
		for name, value := range values {
			values[name] = !value
		}
	}
	return replay, plan
}

func TestKbuildCompilerGuardAnswersMergeAndDefensiveOwnership(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	arguments, units := []string{"-DX=1", "-UX", "source.c"}, []string{"source.c"}
	environment := map[string]string{"MODE": "exact"}
	batch, probePlan := compilerGuardAnswerBatchForTest(t, scopes, arguments, units, environment,
		[][]string{{"__A"}, {"__A", "__B"}}, []string{"0\n", "0 1\n"})
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
	lookup := func() (map[string]bool, string, error) {
		return configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", arguments, units, environment)
	}
	values, identity, err := lookup()
	if err != nil || identity == "" || !maps.Equal(values, map[string]bool{"__A": false, "__B": true}) {
		t.Fatalf("merged initial facts = %#v %q %v", values, identity, err)
	}
	values["__A"], values["__UNQUERIED"] = true, false
	values, secondIdentity, err := lookup()
	if err != nil || secondIdentity != identity || !maps.Equal(values, map[string]bool{"__A": false, "__B": true}) {
		t.Fatal("lookup result mutated the immutable snapshot")
	}
	// Measured compiler facts deliberately do not authenticate source bytes.
	// The separate round manifest and observer own source/cut provenance.
	plan.Sources = []ActionPlanSource{{ID: "different-source", Namespace: "kernel", Path: "different-bytes.c"}}
	_, thirdIdentity, err := lookup()
	if err != nil || thirdIdentity != identity {
		t.Fatalf("source-independent exact compiler facts changed: %q %v", thirdIdentity, err)
	}
	if _, _, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__LATE"}, nil); err == nil {
		t.Fatal("Answers did not freeze its batch")
	}
}

func TestKbuildCompilerGuardAnswersUnknownContextsAndToolsetMismatch(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	scopes := compilerGuardBatchScopesForTest(t, options)
	arguments, units := []string{"-DX=1", "-UX", "source.c"}, []string{"source.c"}
	environment := map[string]string{"MODE": "exact"}
	batch, probePlan := compilerGuardAnswerBatchForTest(t, scopes, arguments, units, environment, [][]string{{"__A"}}, []string{"0\n"})
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
	for _, change := range []string{"scope", "role", "language", "argument order", "source projection", "environment"} {
		t.Run(change, func(t *testing.T) {
			scope, role, language := "target", "cc", "c"
			args, sources, env := slices.Clone(arguments), slices.Clone(units), maps.Clone(environment)
			switch change {
			case "scope":
				scope = "host"
			case "role":
				role = "cxx"
			case "language":
				language = "assembler-with-cpp"
			case "argument order":
				args[0], args[1] = args[1], args[0]
			case "source projection":
				sources[0] = "other.c"
			case "environment":
				env["MODE"] = "different"
			}
			values, identity, err := configDependencySupplementalCompilerDefinedness(plan, scope, role, language, args, sources, env)
			if err != nil || values != nil || identity != "" {
				t.Fatalf("unknown context borrowed facts: %#v %q %v", values, identity, err)
			}
		})
	}
	for _, scope := range []string{"target", "host"} {
		plan.Toolsets = maps.Clone(probePlan.Toolsets)
		plan.Toolsets[scope] = "sha256-" + strings.Repeat("f", 64)
		if _, _, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", arguments, units, environment); err == nil {
			t.Fatalf("changed %s toolset borrowed an answer", scope)
		}
	}
	for _, missing := range []*ActionPlan{nil, {}, {metadata: &CompactMetadata{}}} {
		if values, identity, err := configDependencySupplementalCompilerDefinedness(missing, "target", "cc", "c", nil, nil, nil); err != nil || values != nil || identity != "" {
			t.Fatalf("absent supplemental snapshot = %#v %q %v", values, identity, err)
		}
	}
}

func TestKbuildCompilerGuardAnswersIdentityCommitsMeasurements(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	var identities []string
	for _, test := range []struct {
		batches [][]string
		texts   []string
	}{
		{[][]string{{"__A"}, {"__A", "__B"}}, []string{"0\n", "0 1\n"}},
		{[][]string{{"__A", "__B"}, {"__A"}}, []string{"0 1\n", "0\n"}},
		{[][]string{{"__A", "__B"}}, []string{"0 1\n"}},
		{[][]string{{"__A", "__B"}}, []string{"1 1\n"}},
		{[][]string{{"__A", "__C"}}, []string{"0 1\n"}},
	} {
		batch, probePlan := compilerGuardAnswerBatchForTest(t, scopes, nil, nil, nil, test.batches, test.texts)
		answers, err := batch.Answers()
		if err != nil {
			t.Fatal(err)
		}
		plan := &ActionPlan{Toolsets: maps.Clone(probePlan.Toolsets), metadata: &CompactMetadata{compilerGuardAnswers: answers}}
		_, identity, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c", nil, nil, nil)
		if err != nil || identity == "" {
			t.Fatalf("measurement identity = %q %v", identity, err)
		}
		identities = append(identities, identity)
	}
	if identities[0] != identities[1] {
		t.Fatal("equivalent measurements depend on query registration order")
	}
	for index := 2; index < len(identities); index++ {
		if slices.Contains(identities[:index], identities[index]) {
			t.Fatal("changed batches, names, or measured values reused an identity")
		}
	}
}

func TestKbuildCompilerGuardAnswersRejectDiscoveryFailureAndConflict(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answers, err := discovery.Answers(); err == nil || answers != nil {
		t.Fatal("discovery published measured facts")
	}
	batch, _ := compilerGuardAnswerBatchForTest(t, scopes, nil, nil, nil,
		[][]string{{"__A"}, {"__A", "__B"}}, []string{"0\n", "1 0\n"})
	if answers, err := batch.Answers(); err == nil || answers != nil {
		t.Fatal("conflicting name batches published an answer snapshot")
	}
	batch, probePlan := compilerGuardAnswerBatchForTest(t, scopes, nil, nil, nil, [][]string{{"__A"}}, []string{"0\n"})
	if _, ready, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__MISSING"}, nil); err == nil || ready {
		t.Fatal("fixture unexpectedly found an unexecuted query")
	}
	if answers, err := batch.Answers(); err == nil || answers != nil {
		t.Fatal("failed replay published an earlier partial answer")
	}
	batch, _ = compilerGuardAnswerBatchForTest(t, scopes, nil, nil, nil, [][]string{{"__A"}}, []string{"0\n"})
	result := batch.oracle.results[probePlan.Nodes[0].ID]
	result.Text, result.Steps[0].Stdout = "1\n", "1\n"
	batch.oracle.results[result.NodeID] = result
	if _, ready, err := batch.CompilerDefinedness("target", "cc", "c", nil, nil, []string{"__A"}, nil); err == nil || ready {
		t.Fatal("repeated request accepted contradictory measured answers")
	}
	if answers, err := batch.Answers(); err == nil || answers != nil {
		t.Fatal("contradictory repeated request published facts")
	}
}
