package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Weak inventories may retain complete query closures under staging pressure.
// Actual definedness demands retain their separate, stronger admission rules.
func (stage *familyCompilerGuardOptionalStage) supportsPlanSubset() bool {
	switch stage.kind {
	case familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind,
		familyCompilerCounterHintQueryKind, familyCompilerVariadicStage:
		return true
	default:
		return false
	}
}

// Select a canonical prefix of complete roots in linear graph work. Cost each
// newly retained node/request once, and stop on the first non-fitting closure:
// repeatedly retrying large rejected closures would make optional work quadratic.
// Validate the entire input before dropping anything, including discarded roots.
func boundedFamilyOptionalHintPlan(plan *kconfig.ProbePlan, terminals []string, byteLimit, nodeLimit int) (*kconfig.ProbePlan, int, error) {
	empty, err := kconfig.SelectProbePlanTerminals(plan, nil)
	if err != nil {
		return nil, 0, err
	}
	allowed := map[string]bool{}
	for _, root := range plan.Terminal {
		allowed[root] = true
	}
	roots := slices.Clone(terminals)
	slices.Sort(roots)
	roots = slices.Compact(roots)
	for _, root := range roots {
		if !allowed[root] {
			return nil, 0, fmt.Errorf("optional hint selects a nonterminal root")
		}
	}
	size, fits := familyCompilerGuardOptionalPlanBytes(empty, byteLimit)
	if !fits || nodeLimit < 0 {
		return nil, 0, nil
	}
	nodes := make(map[string]kconfig.ProbePlanNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.ID] = node
	}
	keptNodes, keptRequests := map[string]bool{}, map[string]bool{}
	selected := []string{}
	for _, root := range roots {
		newNodes, newRequests := map[string]bool{}, map[string]bool{}
		encoded, err := json.Marshal(root)
		if err != nil {
			return nil, 0, err
		}
		added := len(encoded) + 2
		stack := []string{root}
		fits = added <= byteLimit-size
		for fits && len(stack) != 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if keptNodes[id] || newNodes[id] {
				continue
			}
			node := nodes[id]
			newNodes[id] = true
			encoded, err = json.Marshal(node)
			if err != nil {
				return nil, 0, err
			}
			added += len(encoded) + 2
			if !keptRequests[node.RequestID] && !newRequests[node.RequestID] {
				newRequests[node.RequestID] = true
				encoded, err = json.Marshal(plan.Requests[node.RequestID])
				if err != nil {
					return nil, 0, err
				}
				added += len(encoded) + len(node.RequestID) + 8
			}
			fits = added <= byteLimit-size && len(newNodes) <= nodeLimit-len(keptNodes)
			stack = append(stack, node.Inputs...)
		}
		if !fits {
			break
		}
		size += added
		maps.Copy(keptNodes, newNodes)
		maps.Copy(keptRequests, newRequests)
		selected = append(selected, root)
	}
	result, err := kconfig.SelectProbePlanTerminals(plan, selected)
	if err != nil {
		return nil, 0, err
	}
	actual, fits := familyCompilerGuardOptionalPlanBytes(result, byteLimit)
	if !fits || actual != size || len(result.Nodes) > nodeLimit {
		return nil, 0, fmt.Errorf("optional hint closure accounting disagrees with selected plan")
	}
	return result, size, nil
}

func (stage *familyCompilerGuardOptionalStage) trimOptionalHintPlan(byteLimit, nodeLimit int) error {
	var roots []string
	for _, variant := range stage.variants {
		allowed := map[string]bool{}
		for _, root := range variant.terminals {
			allowed[root] = true
		}
		for _, candidate := range variant.queries {
			if !allowed[candidate.terminal] {
				return fmt.Errorf("optional hint root is outside its original variant")
			}
			roots = append(roots, candidate.terminal)
		}
	}
	selected, size, err := boundedFamilyOptionalHintPlan(stage.plan, roots, byteLimit, nodeLimit)
	if err != nil {
		return err
	}
	if selected == nil || len(selected.Terminal) == 0 {
		stage.disable("staging_hint_subset_limit")
		return nil
	}
	kept := map[string]bool{}
	for _, root := range selected.Terminal {
		kept[root] = true
	}
	dropped := false
	for index := range stage.variants {
		variant := &stage.variants[index]
		before := len(variant.queries)
		variant.queries = slices.DeleteFunc(variant.queries, func(q familyCompilerGuardOptionalQuery) bool { return !kept[q.terminal] })
		variant.terminals = slices.DeleteFunc(variant.terminals, func(root string) bool { return !kept[root] })
		dropped = dropped || len(variant.queries) != before
	}
	stage.variants = slices.DeleteFunc(stage.variants, func(v familyCompilerGuardOptionalVariant) bool { return len(v.queries) == 0 })
	stage.plan, stage.planBytes, stage.planNodes = selected, size, len(selected.Nodes)
	if dropped {
		stage.omit("staging_hint_subset_limit")
	}
	return nil
}

func (p *familyCompilerGuardPipeline) finishOptionalHintVariant(stage *familyCompilerGuardOptionalStage, variant familyCompilerGuardOptionalVariant, plan *kconfig.ProbePlan) error {
	// First bound the incoming graph, so union construction still holds at most
	// two individually bounded plans. Never retain an unbounded incoming sibling.
	incoming := &familyCompilerGuardOptionalStage{kind: stage.kind, plan: plan, variants: []familyCompilerGuardOptionalVariant{variant}}
	if err := incoming.trimOptionalHintPlan(maxFamilyCompilerGuardBytes/2, maxFamilyCompilerGuardMemberships); err != nil {
		return err
	}
	if incoming.omitted != 0 {
		stage.omit(incoming.reason)
	}
	if incoming.disabled {
		return nil // The retained earlier siblings remain intact.
	}
	if stage.plan != nil {
		var err error
		incoming.plan, err = kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{{Name: "retained", Plan: stage.plan}, {Name: "incoming", Plan: incoming.plan}})
		if err != nil {
			return err
		}
	}
	stage.plan = incoming.plan
	stage.variants = append(stage.variants, incoming.variants...)
	if err := stage.trimOptionalHintPlan(maxFamilyCompilerGuardBytes/2, maxFamilyCompilerGuardMemberships); err != nil {
		return err
	}
	return p.enforceOptionalStagingBudget()
}
