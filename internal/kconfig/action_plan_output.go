package kconfig

// ActionPlan bridges the semantic Kconfig/Kbuild graph into the
// execution-time v4 action plan.  It intentionally does not emit BUILD syntax
// or select a compiler family.  All flags in the recipes are the concrete
// values produced by the Kbuild parser with the selected tool probe.

import (
	"fmt"
	"sort"
	"strings"
)

// lowerSelectedActionPlan performs the source-derived action traversal shared
// by probe discovery and final action-plan generation. The provisional graph
// is sufficient to force every lazy Kbuild rule, command, source script, and
// deferred-content expression which can register a compiler probe. Products
// and serialized node identities are deliberately a later phase.
func (m *CompactMetadata) lowerSelectedActionPlan(
	targetToolsetIdentity, hostToolsetIdentity string,
	probeDiscoveryOnly bool,
) (*ActionPlan, *compactKbuildSelectionGraph, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("kernel action plan requires semantic Kbuild metadata")
	}
	if err := validateProbeIdentity(targetToolsetIdentity); err != nil {
		return nil, nil, fmt.Errorf("target toolset identity: %w", err)
	}
	if err := validateProbeIdentity(hostToolsetIdentity); err != nil {
		return nil, nil, fmt.Errorf("host toolset identity: %w", err)
	}
	if m.configFragment == nil {
		return nil, nil, fmt.Errorf("kernel action plan requires a resolved config fragment")
	}

	plan := &ActionPlan{
		Toolsets:           map[string]string{"target": targetToolsetIdentity, "host": hostToolsetIdentity},
		Recipes:            map[string]ActionRecipe{},
		metadata:           m,
		probeDiscoveryOnly: probeDiscoveryOnly,
	}
	selections, err := m.appendGeneratedActionPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	return plan, selections, nil
}

// DiscoverActionPlanProbes traverses every selected native Kbuild action so
// lazy compiler and source-script expressions register their probe requests.
// The provisional action graph is discarded: product facades, module SDK
// projections, final transitive content IDs, and serialization order belong
// only to replay, after probe results have made the graph concrete.
func (m *CompactMetadata) DiscoverActionPlanProbes(
	targetToolsetIdentity, hostToolsetIdentity string,
) error {
	_, _, err := m.lowerSelectedActionPlan(targetToolsetIdentity, hostToolsetIdentity, true)
	return err
}

func (m *CompactMetadata) ActionPlan(targetToolsetIdentity, hostToolsetIdentity string) (*ActionPlan, error) {
	plan, selections, err := m.lowerSelectedActionPlan(targetToolsetIdentity, hostToolsetIdentity, false)
	if err != nil {
		return nil, err
	}
	if err := m.appendSelectedModuleActionPlanProducts(plan, selections); err != nil {
		return nil, err
	}
	if !m.selectedProductsOnly {
		if err := m.appendTerminalActionPlanNodes(plan, selections); err != nil {
			return nil, err
		}
		if err := m.appendModuleSDKActionPlanNodes(plan); err != nil {
			return nil, err
		}
	}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		return nil, err
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].ID < plan.Nodes[j].ID })
	return plan, nil
}

func contentAddressActionPlanNodes(plan *ActionPlan) error {
	byOldID := make(map[string]ActionPlanNode, len(plan.Nodes))
	aliases := map[string]string{}
	for _, node := range plan.Nodes {
		if _, exists := byOldID[node.ID]; exists {
			return fmt.Errorf("semantic graph repeats provisional node ID %q", node.ID)
		}
		byOldID[node.ID] = node
	}
	for _, node := range plan.Nodes {
		// Provisional IDs are stable only within planning. Resolve every edge to
		// the producer's final transitive content identity before serialization.
		aliases[node.ID] = node.ID
	}
	resolved := map[string]ActionPlanNode{}
	stack := map[string]bool{}
	var resolve func(string) (ActionPlanNode, error)
	resolve = func(oldID string) (ActionPlanNode, error) {
		if node, ok := resolved[oldID]; ok {
			return node, nil
		}
		lookup := oldID
		if alias := aliases[oldID]; alias != "" {
			lookup = alias
		}
		node, ok := byOldID[lookup]
		if !ok {
			known := make([]string, 0, len(byOldID))
			for id := range byOldID {
				known = append(known, id)
			}
			sort.Strings(known)
			return ActionPlanNode{}, fmt.Errorf("node references unknown provisional producer %s (known %s)", oldID, strings.Join(known, ","))
		}
		if stack[oldID] {
			return ActionPlanNode{}, fmt.Errorf("semantic graph contains a cycle at %s", oldID)
		}
		node.Inputs = append([]ActionPlanNodeEdge(nil), node.Inputs...)
		node.Sources = append([]ActionPlanSourceEdge(nil), node.Sources...)
		node.Trees = append([]string(nil), node.Trees...)
		node.Outputs = append([]ActionPlanOutput(nil), node.Outputs...)
		stack[oldID] = true
		for i := range node.Inputs {
			producer, err := resolve(node.Inputs[i].ProducerID)
			if err != nil {
				return ActionPlanNode{}, err
			}
			node.Inputs[i].ProducerID = producer.ID
		}
		delete(stack, oldID)
		node.ID = node.ContentID()
		resolved[oldID] = node
		return node, nil
	}
	for i, node := range plan.Nodes {
		resolvedNode, err := resolve(node.ID)
		if err != nil {
			return err
		}
		plan.Nodes[i] = resolvedNode
	}
	plan.invalidateLookupIndexes()
	return nil
}

func containsUnresolvedPlanMakeReference(value string) bool {
	remaining := withoutActionPlanTreePlaceholders(value)
	return strings.Contains(remaining, "$(") || strings.Contains(remaining, "${")
}

func withoutActionPlanTreePlaceholders(value string) string {
	return actionRecipePlaceholder.ReplaceAllStringFunc(value, func(placeholder string) string {
		match := actionRecipePlaceholder.FindStringSubmatch(placeholder)
		if len(match) == 3 && match[1] == "tree" {
			return ""
		}
		return placeholder
	})
}

func escapeKbuildIdentifier(value string) string {
	var out strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' {
			out.WriteRune(char)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func expandPlanKbuildFlag(value string, config map[string]string, object string) (string, error) {
	remaining := value
	var out strings.Builder
	for {
		start := strings.Index(remaining, "$(")
		brace := strings.Index(remaining, "${")
		if start < 0 || (brace >= 0 && brace < start) {
			start = brace
		}
		if start < 0 {
			out.WriteString(remaining)
			return out.String(), nil
		}
		out.WriteString(remaining[:start])
		closer := byte(')')
		if remaining[start+1] == '{' {
			closer = '}'
		}
		end := strings.IndexByte(remaining[start+2:], closer)
		if end < 0 {
			return "", fmt.Errorf("unterminated make variable")
		}
		end += start + 2
		name := remaining[start+2 : end]
		replacement := ""
		switch name {
		case "obj":
			replacement = "${tree:prep}"
			if dir := pathDir(object); dir != "" && dir != "." {
				replacement += "/" + dir
			}
		case "src":
			replacement = "${tree:kernel}"
			if dir := pathDir(object); dir != "" && dir != "." {
				replacement += "/" + dir
			}
		case "srctree", "srcroot", "abs_srctree":
			replacement = "${tree:kernel}"
		case "objtree", "abs_output":
			replacement = "${tree:prep}"
		default:
			var ok bool
			replacement, ok = config[name]
			if !ok {
				return "", fmt.Errorf("unknown make variable %q", name)
			}
			if replacement == "n" {
				replacement = ""
			}
		}
		out.WriteString(replacement)
		remaining = remaining[end+1:]
	}
}

func pathDir(value string) string {
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[:index]
	}
	return "."
}
