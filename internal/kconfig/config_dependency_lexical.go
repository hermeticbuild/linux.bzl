package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Evidence belongs to one immutable, invocation-keyed probe result, never a
// compiler family or a source filename. No callback means no extra capability.
// The negative value deliberately does not teach the scanner identifier-dollar
// rules; it simply retains the existing refusal of dollar-containing spans.
type configDependencyCompilerLexicalEvidence struct {
	dollarPunctuation bool
	ready             bool
	identity          string
}

func actionPlanCompilerLexicalEvidence(
	plan *ActionPlan,
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (configDependencyCompilerLexicalEvidence, error) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerDollarPunctuation == nil {
		return configDependencyCompilerLexicalEvidence{ready: true}, nil
	}
	value, ready, err := plan.metadata.compilerDollarPunctuation(scope, role, language, arguments, translationUnits, environment)
	if err != nil {
		return configDependencyCompilerLexicalEvidence{}, err
	}
	if !ready {
		return configDependencyCompilerLexicalEvidence{}, nil
	}
	var key strings.Builder
	appendConfigDependencyCacheString(&key, "compiler-dollar-punctuation-v1")
	appendConfigDependencyCacheString(&key, string(configDependencyCompilerPredefineKey(
		scope, role, language, arguments, translationUnits, environment,
	)))
	appendConfigDependencyCacheString(&key, fmt.Sprint(value))
	return configDependencyCompilerLexicalEvidence{
		dollarPunctuation: value, ready: true,
		identity: fmt.Sprintf("%x", sha256.Sum256([]byte(key.String()))),
	}, nil
}

// A completed-result cache hit can bypass the source scanner, so it must carry
// the current lexical observation as well as ordinary compiler/CONFIG inputs.
// Only already-registered evidence for the exact projected invocation qualifies.
func configDependencyCompilerLexicalWitness(
	plan *ActionPlan,
	node ActionPlanNode,
	invocation configDependencyCompilerInvocation,
	context *configDependencyAnalysisContext,
) (string, bool) {
	if plan == nil || plan.metadata == nil || plan.metadata.compilerDollarPunctuation == nil {
		return "", true
	}
	if context == nil {
		return "", false
	}
	sources, err := actionPlanConfigDependencySourcePaths(plan, node)
	if err != nil {
		return "", false
	}
	probe, reason := configDependencyCompilerPredefineProbeForActionInvocation(invocation, sources)
	if reason != "" {
		return "", false
	}
	key := configDependencyCompilerPredefineKey(actionPlanConfigDependencyScope(node), invocation.tool,
		probe.language, probe.arguments, probe.translationUnits, probe.environment)
	result, found := context.compilerPredefineRequests[key]
	return result.lexical.identity, found && result.ready && result.lexical.ready && result.lexical.identity != ""
}
