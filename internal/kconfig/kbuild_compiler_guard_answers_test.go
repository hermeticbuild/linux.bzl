package kconfig

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// These identities pin the pre-template constructor, including its ordered
// dependencies. Query-context extraction must not invalidate remote artifacts.
func TestCompilerProjectedRequestTemplateParity(t *testing.T) {
	for _, context := range []string{"literal", "symbolic", "source-shell"} {
		for _, query := range []string{"predefines", "definedness", "optional", "intrinsic"} {
			t.Run(context+"/"+query, func(t *testing.T) {
				evaluator, first, second := sourceShellWordsEvaluatorForTest(t, "target")
				evaluator.sourceRoot = "/fixture/linux"
				evaluator.sourceArchitecture = "x86"
				evaluator.tools = map[string]string{"cc": "compiler", "ld": "linker"}
				scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
				arguments := []string{"-DVALUE=7", "-DVALUE=8", "-UOTHER", "-include", "/fixture/linux/forced.h", "unit.c"}
				environment := map[string]string{"MODE": "/fixture/linux/include", "LD": "${tool:ld}"}
				switch context {
				case "symbolic":
					arguments = append(arguments, first)
					environment["MODE"] = second + first
				case "source-shell":
					wrapped, err := evaluator.renderSourceShellWords(second)
					if err != nil {
						t.Fatal(err)
					}
					arguments = append(arguments, wrapped, first)
					environment["MODE"] = second + first
				}
				projection := ProbeCandidateProjectionCompilerPredefines
				managed := []string{"-E", "-P", "-x", "c", "-"}
				stdin := "#if defined(__ANSWER)\n1\n#else\n0\n#endif\n"
				outcome := ProbeOutcome{Kind: "text", Step: query, Stream: "stdout", RequireSuccess: true}
				switch query {
				case "predefines":
					managed = []string{"-dM", "-E", "-x", "c", "-"}
					stdin = ""
				case "optional":
					outcome = ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: query}}
				case "intrinsic":
					projection = ProbeCandidateProjectionCompilerIntrinsic
					stdin = "#undef deprecated\n__has_attribute(deprecated)\n"
				}
				probe, err := scopes.compilerProjectedRequestWithProjection("target", "cc", "c", arguments,
					[]string{"unit.c", "unit.c"}, environment, query, managed, stdin, outcome, projection)
				if err != nil {
					t.Fatal(err)
				}
				request, err := probe.request.CanonicalJSON()
				if err != nil {
					t.Fatal(err)
				}
				dependencies, err := json.Marshal(probe.dependencies)
				if err != nil {
					t.Fatal(err)
				}
				identity := fmt.Sprintf("%x", sha256.Sum256(append(append(request, '\n'), dependencies...)))
				want := map[string]string{
					"literal/predefines":       "ae37d4da20d0de7b4b96c1247872b08f8503d65d02c82015b6b029b8ba3977f0",
					"literal/definedness":      "33b4b074fc93c0045674deb1c3f2fd9b5104d395f22b18bc35c8019ed647e345",
					"literal/optional":         "57109e51d40b03ba3408ad5b50daf835d0548a92a5747385403567ee544c9899",
					"literal/intrinsic":        "71da40ece211adbbf386d30f4fcec3eb166870693ed543d41cd9102b2592579b",
					"symbolic/predefines":      "8e3b32f4eeb38ff6fb341b2c0c49731d2e40759369b65054686b26fcd9100d31",
					"symbolic/definedness":     "a7b25aae7002c275968ec2eac9d36dfda63f9c3c05ae52ecaffbc1b4aef41bf7",
					"symbolic/optional":        "544d0f5dc8143f2e067aeda13d3c3bc33a4c93e018b096b2e894e2586086ba5d",
					"symbolic/intrinsic":       "d79348214c45d0dc3519483c7c756558377ea8044ca6aaa5afa3bd3e55cfcb28",
					"source-shell/predefines":  "440d59c89c885f5fe37ef42a2013448601f3582061aaa4392d93001ed7bac2f7",
					"source-shell/definedness": "69ef0627159b03d32746ab16f5de08f05e34f39ef7f7e1d8f74342254e023797",
					"source-shell/optional":    "5d92c7ae1c7b8df8fae8c811f3e4e20246438a1784a9fd055d221bb8bf9b4eaa",
					"source-shell/intrinsic":   "fd3e6a71d241b223c197bf5c0a9b441bec95e0dcaddc868768820ae8470f1b91",
				}
				if identity != want[context+"/"+query] {
					t.Fatalf("request/dependency identity = %s", identity)
				}
				template, err := scopes.compilerProjectedContext("target", "cc", "c", arguments,
					[]string{"unit.c"}, environment, projection)
				if err != nil {
					t.Fatal(err)
				}
				// Mutate caller inputs and every populated mutable output shape.
				// The same immutable context must still reproduce the pinned bytes.
				arguments[0], environment["MODE"] = "changed", "changed"
				for attempt := range 2 {
					reused, err := template.query(query, managed, stdin, outcome)
					if err != nil {
						t.Fatal(err)
					}
					got, err := reused.request.CanonicalJSON()
					if err != nil || string(got) != string(request) || !slices.Equal(reused.dependencies, probe.dependencies) {
						t.Fatalf("context reuse %d changed request or ordered dependencies: %v", attempt, err)
					}
					step := &reused.request.Steps[0]
					step.Arguments[0], step.AuxiliaryTools[0] = "changed", "changed"
					step.Candidate.Base[0], step.Candidate.TranslationUnits[0] = 999, "changed"
					step.Environment["LD"] = "changed"
					for index := range step.ConditionalArguments {
						step.ConditionalArguments[index].Arguments[0] = "changed"
						step.ConditionalArguments[index].When.Operator = "changed"
					}
					for index := range step.ArgumentFragments {
						step.ArgumentFragments[index].Fragments[0].Value = "changed"
					}
					for index := range step.EnvironmentFragments {
						step.EnvironmentFragments[index].Fragments[0].Value = "changed"
					}
					if len(reused.dependencies) != 0 {
						reused.dependencies[0].NodeID = "changed"
					}
				}
				transferred, err := template.consume(query, managed, stdin, outcome)
				if err != nil {
					t.Fatal(err)
				}
				got, err := transferred.request.CanonicalJSON()
				if err != nil || string(got) != string(request) || !slices.Equal(transferred.dependencies, probe.dependencies) {
					t.Fatalf("context transfer changed request or ordered dependencies: %v", err)
				}
				if _, err := template.query(query, managed, stdin, outcome); err == nil {
					t.Fatal("consumed context was reused")
				}
				if _, err := template.consume(query, managed, stdin, outcome); err == nil {
					t.Fatal("context was transferred twice")
				}
			})
		}
	}
}

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
