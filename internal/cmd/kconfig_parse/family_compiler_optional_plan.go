package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Bounded, private scheduling evidence. One consumer can contribute at most
// once per variant and projected compiler-context class, regardless of how
// many headers or names it observes. No source/argument strings are retained.
// Exhaustion or missing identities discard ALL scores, restoring canonical
// ordering without suppressing any query or affecting compiler facts.
type familyCompilerHintConsumers struct {
	seen     map[[32]byte]struct{}
	counts   map[[32]byte]int
	disabled bool
}

func (c *familyCompilerHintConsumers) observe(class [32]byte, variant, consumer string) {
	if c.disabled {
		return
	}
	if len(variant) == 0 || len(variant) > 256 || len(consumer) == 0 || len(consumer) > 256 {
		c.discard()
		return
	}
	var framed [32 + 2 + 256 + 256]byte
	copy(framed[:32], class[:])
	binary.BigEndian.PutUint16(framed[32:34], uint16(len(variant)))
	copy(framed[34:], variant)
	copy(framed[34+len(variant):], consumer)
	id := sha256.Sum256(framed[:34+len(variant)+len(consumer)])
	if _, found := c.seen[id]; found {
		return
	}
	if len(c.seen) >= maxFamilyCompilerGuardMemberships ||
		c.counts[class] == 0 && len(c.counts) >= maxFamilyCompilerGuardQueries {
		c.discard()
		return
	}
	if c.seen == nil {
		c.seen = map[[32]byte]struct{}{}
		c.counts = map[[32]byte]int{}
	}
	c.seen[id] = struct{}{}
	c.counts[class]++
}

func (c *familyCompilerHintConsumers) discard() {
	c.disabled = true
	c.seen, c.counts = nil, nil
}

func (stage *familyCompilerGuardOptionalStage) rootConsumerScores() map[string]int {
	if stage.consumers == nil || stage.consumers.disabled {
		return nil
	}
	scores := map[string]int{}
	for _, variant := range stage.variants {
		for _, candidate := range variant.queries {
			// Shared terminals must not multiply the class's count by the
			// number of variant memberships. Scheduling equivalence is the
			// same projection used for pending-name deduplication.
			score := stage.consumers.counts[familyCompilerGuardSchedulingKey(candidate.query)]
			scores[candidate.terminal] = max(scores[candidate.terminal], score)
		}
	}
	return scores
}

// Weak inventories may retain complete query closures under staging pressure.
// Actual definedness demands retain their separate, stronger admission rules.
func (stage *familyCompilerGuardOptionalStage) supportsPlanSubset() bool {
	switch stage.kind {
	case familyCompilerTokenHintQueryKind, familyCompilerLiteralHintQueryKind,
		familyCompilerCounterHintQueryKind, familyCompilerVariadicStage, familyCompilerVariadicLookaheadStage:
		return true
	default:
		return false
	}
}

// Select a deterministic prefix of complete roots in linear graph work after
// ordering them. The unranked entry point keeps canonical ID order. Cost each
// newly retained node/request once, and stop on the first non-fitting closure:
// repeatedly retrying large rejected closures would make optional work quadratic.
// Validate the entire input before dropping anything, including discarded roots.
func boundedFamilyOptionalHintPlan(plan *kconfig.ProbePlan, terminals []string, byteLimit, nodeLimit int) (*kconfig.ProbePlan, int, error) {
	return boundedFamilyRankedOptionalHintPlan(plan, terminals, byteLimit, nodeLimit, nil)
}

func boundedFamilyRankedOptionalHintPlan(plan *kconfig.ProbePlan, terminals []string, byteLimit, nodeLimit int, scores map[string]int) (*kconfig.ProbePlan, int, error) {
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
	if len(scores) != 0 {
		slices.SortFunc(roots, func(a, b string) int {
			if order := cmp.Compare(scores[b], scores[a]); order != 0 {
				return order
			}
			return cmp.Compare(a, b)
		})
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
	selected, size, err := boundedFamilyRankedOptionalHintPlan(stage.plan, roots, byteLimit, nodeLimit, stage.rootConsumerScores())
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
	incoming := &familyCompilerGuardOptionalStage{kind: stage.kind, plan: plan, variants: []familyCompilerGuardOptionalVariant{variant}, consumers: stage.consumers}
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
