package kconfig

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var probeCandidateTextResult = regexp.MustCompile(`\$\{result:([0-9]{8})\.text\}`)

// The configured package query may produce staged host-dependency flags only
// after its predecessor has run. Inspect the already lowered compiler
// candidate, not arbitrary probe dependencies or stdout: only source-owned
// candidate argv and the exact result fragments which can fill it request the
// host_deps root. The runner independently confines every rendered path to
// the declared tree.
func (e *LinuxProbeEvaluator) candidateHostDependencyRoot(
	arguments []string,
	conditional []ProbeConditionalArguments,
	argumentFragments []ProbeArgumentFragments,
	candidate *ProbeCandidateArguments,
	dependencies []ProbeReference,
) (bool, error) {
	if candidate == nil {
		return false, nil
	}
	visitText := func(value string) (bool, error) {
		if strings.Contains(value, linuxProbeHostDepsSentinel+"/") ||
			strings.Contains(value, "${source_root:"+linuxProbeHostDepsRootName+"}/") {
			return true, nil
		}
		for _, match := range probeCandidateTextResult.FindAllStringSubmatch(value, -1) {
			index, err := strconv.Atoi(match[1])
			if err != nil || index < 0 || index >= len(dependencies) {
				return false, fmt.Errorf("compiler candidate has an unbound result ordinal %q", match[1])
			}
			if e.candidateHostPackageFlagProducer(dependencies[index], map[string]bool{}, 0) {
				return true, nil
			}
		}
		return false, nil
	}
	var visitFragments func([]ProbeValueFragment, int) (bool, error)
	visitFragments = func(fragments []ProbeValueFragment, depth int) (bool, error) {
		if depth > MaxProbeValueFragmentDepth {
			return false, fmt.Errorf("compiler candidate root provenance exceeds fragment depth")
		}
		for _, fragment := range fragments {
			if present, err := visitText(fragment.Value); present || err != nil {
				return present, err
			}
			if len(fragment.Fragments) != 0 {
				if present, err := visitFragments(fragment.Fragments, depth+1); present || err != nil {
					return present, err
				}
			}
			for _, transform := range fragment.Transforms {
				for _, argument := range transform.Arguments {
					if present, err := visitText(argument); present || err != nil {
						return present, err
					}
				}
				for _, argument := range transform.ArgumentFragments {
					if present, err := visitFragments(argument.Fragments, depth+1); present || err != nil {
						return present, err
					}
				}
			}
		}
		return false, nil
	}
	for _, index := range candidate.Base {
		if index < 0 || index >= len(arguments) {
			return false, fmt.Errorf("compiler candidate has invalid base argument index %d", index)
		}
		if present, err := visitText(arguments[index]); present || err != nil {
			return present, err
		}
		for _, group := range argumentFragments {
			if group.Index != index {
				continue
			}
			if present, err := visitFragments(group.Fragments, 0); present || err != nil {
				return present, err
			}
		}
	}
	for _, index := range candidate.Conditional {
		if index < 0 || index >= len(conditional) {
			return false, fmt.Errorf("compiler candidate has invalid conditional argument index %d", index)
		}
		for _, argument := range conditional[index].Arguments {
			if present, err := visitText(argument); present || err != nil {
				return present, err
			}
		}
	}
	return false, nil
}

func (e *LinuxProbeEvaluator) candidateHostPackageFlagProducer(
	reference ProbeReference, visited map[string]bool, depth int,
) bool {
	if reference.NodeID == "" || depth > maxKbuildSymbolicComparisonDepth ||
		visited[reference.NodeID] || e.symbolRegistry == nil {
		return false
	}
	visited[reference.NodeID] = true
	definition, present := e.symbolRegistry.lookupDefinition(reference.NodeID)
	if !present || definition.reference != reference {
		return false
	}
	request := definition.request
	if reference.Scope == "host" && request.Outcome.Kind == "text" &&
		request.Outcome.GNUMakeShell && len(request.Steps) == 1 {
		step := request.Steps[0]
		if step.Name == "configured-pkg-config-query" && step.Tool == linuxProbeScriptRunner &&
			len(step.AuxiliaryTools) == 1 && step.AuxiliaryTools[0] == linuxProbePkgConfigRole &&
			configuredPkgConfigProducesHostDependencyFlags(step.Arguments) {
			return true
		}
	}
	for _, dependency := range definition.dependencies {
		if e.candidateHostPackageFlagProducer(dependency, visited, depth+1) {
			return true
		}
	}
	return false
}

// Only the primary configured query's mode is an authority claim. An --exists
// query can have an arbitrary safe echo continuation containing the literal
// word --cflags; that continuation is not a package flag producer.
func configuredPkgConfigProducesHostDependencyFlags(arguments []string) bool {
	for index, argument := range arguments {
		if argument != "-script_content" || index+1 >= len(arguments) {
			continue
		}
		primary, _, present := strings.Cut(arguments[index+1], " 2>/dev/null")
		if !present {
			return false
		}
		words, err := lexCompactKbuildRecipe(primary)
		if err != nil || len(words) < 3 || words[0].operator || words[0].value != linuxProbePkgConfigRole {
			return false
		}
		for _, word := range words[1:] {
			if word.operator {
				return false
			}
			if word.value == "--cflags" || word.value == "--libs" {
				return true
			}
		}
		return false
	}
	return false
}
