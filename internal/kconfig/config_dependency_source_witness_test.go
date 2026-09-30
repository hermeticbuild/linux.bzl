package kconfig

import (
	"slices"
	"strings"
	"testing"
)

// Registration and optional completed-result reuse must identify the same
// compiler query after source-owned argument lowering removes a prerequisite
// that cannot be an operand. Neither witness may create a query during lookup.
func TestCompilerWitnessesUseRegisteredSourceCandidateProjection(t *testing.T) {
	for _, extra := range []string{"forced.c", "forced.S", "forced.s"} {
		t.Run(extra, func(t *testing.T) {
			const source = "drivers/example/driver.c"
			arguments := []string{"-nostdinc", "LINUX_BZL_PROBE_" + strings.Repeat("a", 64), "-c", source}
			plan, node := configDependencyCompilePlanForTest(t, map[string]string{
				source: "CONFIG_DRIVER\n", extra: "/* source prerequisite, not an operand */\n",
			}, arguments, nil)
			plan.Sources = append(plan.Sources, ActionPlanSource{ID: "src-00000002", Namespace: "kernel", Path: extra})
			node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "source", SourceID: "src-00000002"})
			plan.Nodes[0] = node
			configDependencySetCompilerContractForTest(plan, node, CompactKbuildActionContract{})
			plan.metadata.compilerSourceCandidates = func(_, _ string, _ []string, candidates []string) []string {
				return slices.DeleteFunc(slices.Clone(candidates), func(candidate string) bool { return candidate == extra })
			}
			plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, _ []string, _ map[string]string) (map[string]bool, bool, error) {
				t.Fatal("definedness witness must not register a query")
				return nil, false, nil
			}
			plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
				t.Fatal("lexical witness must not register a query")
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
			registered, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, sources)
			if reason != "" {
				t.Fatal(reason)
			}
			if len(registered.translationUnits) != 0 || registered.language != "c" {
				t.Fatalf("fixture did not remove false operand/language: %#v", registered)
			}
			key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
				registered.language, registered.arguments, registered.translationUnits, registered.environment)
			context := newConfigDependencyAnalysisContext(plan)
			for _, ready := range []bool{false, true} {
				context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{
					key: {ready: ready, guardIdentity: "registered-guard", lexical: configDependencyCompilerLexicalEvidence{ready: ready, identity: "registered-lexical"}},
				}
				guard, guardReady := configDependencyCompilerDefinednessWitness(plan, node, invocation, context)
				lexical, lexicalReady := configDependencyCompilerLexicalWitness(plan, node, invocation, context)
				if guardReady != ready || lexicalReady != ready || ready && (guard != "registered-guard" || lexical != "registered-lexical") {
					t.Fatalf("ready=%t: guard=%q/%t lexical=%q/%t", ready, guard, guardReady, lexical, lexicalReady)
				}
			}
			unnormalized, _ := configDependencyCompilerPredefineProbeForInvocation(invocation, sources)
			otherKey := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
				unnormalized.language, unnormalized.arguments, unnormalized.translationUnits, unnormalized.environment)
			if otherKey == key {
				t.Fatal("fixture did not distinguish raw and registered query identities")
			}
			context.compilerPredefineRequests = map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{
				otherKey: {ready: true, guardIdentity: "different-query", lexical: configDependencyCompilerLexicalEvidence{ready: true, identity: "different-query"}},
			}
			if _, ready := configDependencyCompilerDefinednessWitness(plan, node, invocation, context); ready {
				t.Fatal("borrowed a differently projected definedness witness")
			}
			if _, ready := configDependencyCompilerLexicalWitness(plan, node, invocation, context); ready {
				t.Fatal("borrowed a differently projected lexical witness")
			}
		})
	}
}
