package kconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
)

const (
	LinuxKernelFamilyExecutionCutSchema      = "linux-kernel-family-execution-cut-v1"
	LinuxKernelFamilyExecutionSchema         = "linux-kernel-family-execution-v1"
	MaxActionPlanFamilyExecutionCutNodes     = 4096
	MaxActionPlanFamilyExecutionCutOutputs   = 16384
	MaxActionPlanFamilyExecutionCutInputSets = 131072
	MaxActionPlanFamilyExecutionCutRecords   = 262144
	MaxActionPlanFamilyExecutionCutBytes     = 64 << 20
)

// ActionPlanFamilyExecutionCutRoot selects one exact ordinary output. Logical
// path discovery belongs to the caller: historical publishers and sidecar
// slots must never be guessed from a pathname at this authority boundary.
type ActionPlanFamilyExecutionCutRoot struct {
	NodeID string `json:"node_id"`
	Slot   int    `json:"slot"`
}

// ActionPlanFamilyExecutionCutOrigin is the original snapshot identity which
// must remain opaque when the final family is rebuilt. Several original nodes
// in one or several variants can reduce to the same execution node.
type ActionPlanFamilyExecutionCutOrigin struct {
	Variant        string `json:"variant"`
	OriginalNodeID string `json:"original_node_id"`
	NodeID         string `json:"node_id"`
}

// ActionPlanFamilyExecutionCutOutput includes every slot of every selected
// node, including observed-state envelopes. Their writer identities are part
// of the actual output bytes; this API never changes or synthesizes writers.
type ActionPlanFamilyExecutionCutOutput struct {
	NodeID string           `json:"node_id"`
	Slot   int              `json:"slot"`
	Output ActionPlanOutput `json:"output"`
}

type actionPlanFamilyExecutionCutNode struct {
	Node              ActionPlanNode                     `json:"node"`
	SourceProjections []ActionPlanFamilySourceProjection `json:"source_projections,omitempty"`
	Memberships       []string                           `json:"memberships"`
}

type actionPlanFamilyExecutionCutContract struct {
	Schema      string                               `json:"schema"`
	Variants    []string                             `json:"variants"`
	Toolsets    map[string]string                    `json:"toolsets"`
	Roots       []ActionPlanFamilyExecutionCutRoot   `json:"roots"`
	Nodes       []actionPlanFamilyExecutionCutNode   `json:"nodes"`
	Origins     []ActionPlanFamilyExecutionCutOrigin `json:"origins"`
	Recipes     map[string]ActionRecipe              `json:"recipes"`
	Sources     []ActionPlanSource                   `json:"sources"`
	InputSets   map[string]ActionPlanInputSetNode    `json:"input_sets"`
	Capsules    map[string]map[string]string         `json:"capsules"`
	Validations []ActionPlanFamilyValidation         `json:"validations"`
}

// ActionPlanFamilyExecutionCut is a detached immutable contract captured from
// a complete, reduced family. The canonical JSON is a bounded seal for later
// transport; it is not a public mutable authority. Transport decoding rebuilds
// the contract from the supplied complete family, never from asserted nodes.
// Its construction does not reduce an extracted graph.
//
// Callers must retain the original source/config/tool/probe invocation inputs
// independently: a family contract identifies declared execution, not mutable
// external filesystem contents. As with family emission, the supplied family
// must not be mutated concurrently with construction or verification.
type ActionPlanFamilyExecutionCut struct {
	contract  actionPlanFamilyExecutionCutContract
	canonical []byte
	id        string
}

type actionPlanFamilyExecutionCutLimits struct{ nodes, outputs, inputSets, records, bytes int }

// Resource limits may decline an optimization, but malformed execution
// contracts must still fail. Keep those cases distinguishable without parsing
// diagnostic strings.
type actionPlanFamilyExecutionCutBudgetError struct{ message string }

func (e *actionPlanFamilyExecutionCutBudgetError) Error() string { return e.message }

func executionCutBudgetError(format string, args ...any) error {
	return &actionPlanFamilyExecutionCutBudgetError{message: fmt.Sprintf(format, args...)}
}

func defaultActionPlanFamilyExecutionCutLimits() actionPlanFamilyExecutionCutLimits {
	return actionPlanFamilyExecutionCutLimits{MaxActionPlanFamilyExecutionCutNodes, MaxActionPlanFamilyExecutionCutOutputs,
		MaxActionPlanFamilyExecutionCutInputSets, MaxActionPlanFamilyExecutionCutRecords, MaxActionPlanFamilyExecutionCutBytes}
}

// NewActionPlanFamilyExecutionCut seals the dependency closure of exact roots
// AFTER complete family reduction has installed implicit prior-tree edges.
// It validates the family once, traverses each selected node and immutable
// InputSet subtree once, and includes applicable comparison obligations.
func NewActionPlanFamilyExecutionCut(family *ActionPlanFamily, roots []ActionPlanFamilyExecutionCutRoot) (*ActionPlanFamilyExecutionCut, error) {
	return newActionPlanFamilyExecutionCut(family, roots, defaultActionPlanFamilyExecutionCutLimits())
}

func newActionPlanFamilyExecutionCut(family *ActionPlanFamily, roots []ActionPlanFamilyExecutionCutRoot, limits actionPlanFamilyExecutionCutLimits) (*ActionPlanFamilyExecutionCut, error) {
	if limits.nodes <= 0 || limits.outputs <= 0 || limits.inputSets <= 0 || limits.records <= 0 || limits.bytes <= 0 {
		return nil, fmt.Errorf("execution cut requires positive limits")
	}
	if len(roots) > limits.outputs {
		return nil, executionCutBudgetError("execution cut accepts at most %d exact output roots", limits.outputs)
	}
	if err := family.validate(); err != nil {
		return nil, fmt.Errorf("execution cut family: %w", err)
	}
	if len(roots) != 0 && len(family.originalNodeIDs) == 0 {
		return nil, fmt.Errorf("execution cut family has no original snapshot node provenance")
	}
	return captureActionPlanFamilyExecutionCut(family, roots, limits)
}

func executionCutRootLess(left, right ActionPlanFamilyExecutionCutRoot) bool {
	return left.NodeID < right.NodeID || left.NodeID == right.NodeID && left.Slot < right.Slot
}

func captureActionPlanFamilyExecutionCut(family *ActionPlanFamily, requested []ActionPlanFamilyExecutionCutRoot, limits actionPlanFamilyExecutionCutLimits) (*ActionPlanFamilyExecutionCut, error) {
	nodes := make(map[string]ActionPlanNode, len(family.Nodes))
	for _, node := range family.Nodes {
		nodes[node.ID] = node
	}
	sources := make(map[string]ActionPlanSource, len(family.Sources))
	for _, source := range family.Sources {
		sources[source.ID] = source
	}
	roots := append([]ActionPlanFamilyExecutionCutRoot{}, requested...)
	sort.Slice(roots, func(i, j int) bool { return executionCutRootLess(roots[i], roots[j]) })
	for index, root := range roots {
		node, ok := nodes[root.NodeID]
		if !ok || root.Slot < 0 || root.Slot >= len(node.Outputs) {
			return nil, fmt.Errorf("execution cut root references unavailable output %s[%d]", root.NodeID, root.Slot)
		}
		if node.Outputs[root.Slot].ObservedPath != "" {
			return nil, fmt.Errorf("execution cut root %s[%d] is an observed state, not an ordinary output", root.NodeID, root.Slot)
		}
		if index != 0 && root == roots[index-1] {
			return nil, fmt.Errorf("execution cut repeats root %s[%d]", root.NodeID, root.Slot)
		}
	}

	// Snapshot validation currently permits this one exact comparison protocol.
	// Activate its obligation from either compared producer, never from a shared
	// distant ancestor which also feeds an unrelated comparison.
	validationsByProducer := map[string][]string{}
	validationIDs := map[string]bool{}
	for _, validation := range family.Validations {
		if validationIDs[validation.NodeID] {
			continue
		}
		root := nodes[validation.NodeID]
		want := ActionRecipe{Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile",
			Arguments: []string{"-compare_input", "${input:projected:00000000}", "-compare_input", "${input:full:00000001}", "-out", "${output:00000000}"},
			Inputs:    []string{"projected:00000000", "full:00000001"}, Outputs: []string{"00000000"}}
		actualData, err := family.Recipes[root.Recipe].CanonicalJSON()
		if err != nil {
			return nil, err
		}
		wantData, err := want.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		if root.Kind != "metadata" || root.Tool != "actionfile" || len(root.Inputs) != 2 ||
			root.Inputs[0].Role != "projected" || root.Inputs[1].Role != "full" || root.Inputs[0].Slot != root.Inputs[1].Slot ||
			len(root.Outputs) != 1 || root.Outputs[0].ObservedPath != "" || validation.Slot != 0 || !bytes.Equal(actualData, wantData) {
			return nil, fmt.Errorf("execution cut has unsupported validation contract %s", root.ID)
		}
		validationIDs[root.ID] = true
		for _, input := range root.Inputs {
			validationsByProducer[input.ProducerID] = append(validationsByProducer[input.ProducerID], root.ID)
		}
	}

	contract := actionPlanFamilyExecutionCutContract{Schema: LinuxKernelFamilyExecutionCutSchema,
		Variants: slices.Clone(family.Variants), Toolsets: maps.Clone(family.Toolsets), Roots: roots,
		Recipes: map[string]ActionRecipe{}, InputSets: map[string]ActionPlanInputSetNode{}, Capsules: map[string]map[string]string{}}
	selected := map[string]bool{}
	selectedSources := map[string]bool{}
	type work struct{ node, inputSet string }
	pending := []work{}
	outputCount, records := 0, len(roots)+len(family.Variants)+len(family.Toolsets)
	account := func(count int) error {
		if count > limits.records-records {
			return executionCutBudgetError("execution cut exceeds %d contract records", limits.records)
		}
		records += count
		return nil
	}
	addNode := func(id string) error {
		if selected[id] {
			return nil
		}
		node, ok := nodes[id]
		if !ok {
			return fmt.Errorf("execution cut references unavailable node %s", id)
		}
		if len(selected) >= limits.nodes {
			return executionCutBudgetError("execution cut exceeds %d nodes", limits.nodes)
		}
		if len(node.Outputs) > limits.outputs-outputCount {
			return executionCutBudgetError("execution cut exceeds %d output slots", limits.outputs)
		}
		if err := account(1 + len(node.Sources) + len(node.Inputs) + len(node.Outputs) + len(node.familySourceProjections)); err != nil {
			return err
		}
		outputCount += len(node.Outputs)
		selected[id] = true
		pending = append(pending, work{node: id})
		return nil
	}
	addRef := func(id string, slot int) error {
		node, ok := nodes[id]
		if !ok || slot < 0 || slot >= len(node.Outputs) {
			return fmt.Errorf("execution cut references unavailable output %s[%d]", id, slot)
		}
		return addNode(id)
	}
	addSet := func(id string) error {
		if id == "" {
			return nil
		}
		if _, found := contract.InputSets[id]; found {
			return nil
		}
		set, found := family.InputSets[id]
		if !found {
			return fmt.Errorf("execution cut references unavailable input set %s", id)
		}
		if len(contract.InputSets) >= limits.inputSets {
			return executionCutBudgetError("execution cut exceeds %d input-set nodes", limits.inputSets)
		}
		if err := account(1 + len(set.Entries) + len(set.Children)); err != nil {
			return err
		}
		contract.InputSets[id] = cloneActionPlanInputSetNode(set)
		pending = append(pending, work{inputSet: id})
		return nil
	}
	for _, root := range roots {
		if err := addNode(root.NodeID); err != nil {
			return nil, err
		}
	}
	for len(pending) != 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if item.node != "" {
			node := nodes[item.node]
			for _, source := range node.Sources {
				selectedSources[source.SourceID] = true
			}
			for _, input := range node.Inputs {
				if err := addRef(input.ProducerID, input.Slot); err != nil {
					return nil, err
				}
			}
			if err := addSet(node.InputSet); err != nil {
				return nil, err
			}
			for _, validation := range validationsByProducer[item.node] {
				if err := addNode(validation); err != nil {
					return nil, err
				}
			}
		} else {
			set := contract.InputSets[item.inputSet]
			for _, entry := range set.Entries {
				if entry.SourceID != "" {
					selectedSources[entry.SourceID] = true
				} else if err := addRef(entry.ProducerID, entry.Slot); err != nil {
					return nil, err
				}
			}
			for _, child := range set.Children {
				if err := addSet(child.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(selected)) {
		node := nodes[id]
		node.Sources = slices.Clone(node.Sources)
		node.Inputs = slices.Clone(node.Inputs)
		node.Outputs = slices.Clone(node.Outputs)
		node.Trees = slices.Clone(node.Trees)
		node.AuxiliaryTools = slices.Clone(node.AuxiliaryTools)
		node.familySourceProjections = slices.Clone(node.familySourceProjections)
		contract.Nodes = append(contract.Nodes, actionPlanFamilyExecutionCutNode{Node: node,
			SourceProjections: slices.Clone(node.familySourceProjections), Memberships: slices.Clone(family.Memberships[id])})
		if _, found := contract.Recipes[node.Recipe]; !found {
			if err := account(1); err != nil {
				return nil, err
			}
			contract.Recipes[node.Recipe] = cloneActionRecipe(family.Recipes[node.Recipe])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(selectedSources)) {
		if err := account(1); err != nil {
			return nil, err
		}
		source, found := sources[id]
		if !found {
			return nil, fmt.Errorf("execution cut references unavailable source %s", id)
		}
		contract.Sources = append(contract.Sources, source)
		if source.Namespace == "capsule" {
			digest, _, _ := strings.Cut(source.Path, "/")
			if _, found := contract.Capsules[digest]; !found {
				if err := account(1 + len(family.Capsules[digest])); err != nil {
					return nil, err
				}
				contract.Capsules[digest] = maps.Clone(family.Capsules[digest])
			}
		}
	}
	originMemberships := map[string]map[string]bool{}
	for _, variant := range family.Variants {
		if len(selected) == 0 {
			break
		}
		originals, found := family.originalNodeIDs[variant]
		if !found {
			return nil, fmt.Errorf("execution cut has no original node mapping for variant %s", variant)
		}
		for _, originalID := range slices.Sorted(maps.Keys(originals)) {
			id := originals[originalID]
			if !selected[id] {
				continue
			}
			if err := validatePlanDigest("execution cut original node", originalID); err != nil {
				return nil, err
			}
			if !slices.Contains(family.Memberships[id], variant) {
				return nil, fmt.Errorf("execution cut original node %s/%s maps outside node %s membership", variant, originalID, id)
			}
			if err := account(1); err != nil {
				return nil, err
			}
			contract.Origins = append(contract.Origins, ActionPlanFamilyExecutionCutOrigin{Variant: variant, OriginalNodeID: originalID, NodeID: id})
			if originMemberships[id] == nil {
				originMemberships[id] = map[string]bool{}
			}
			originMemberships[id][variant] = true
		}
	}
	for _, record := range contract.Nodes {
		for _, variant := range record.Memberships {
			if !originMemberships[record.Node.ID][variant] {
				return nil, fmt.Errorf("execution cut node %s has no original snapshot node in variant %s", record.Node.ID, variant)
			}
		}
	}
	for _, validation := range family.Validations {
		if !selected[validation.NodeID] {
			continue
		}
		if err := account(1); err != nil {
			return nil, err
		}
		contract.Validations = append(contract.Validations, validation)
	}
	sort.Slice(contract.Validations, func(i, j int) bool {
		left, right := contract.Validations[i], contract.Validations[j]
		return left.Variant < right.Variant || left.Variant == right.Variant && (left.NodeID < right.NodeID || left.NodeID == right.NodeID && left.Slot < right.Slot)
	})
	if records > limits.records {
		return nil, executionCutBudgetError("execution cut exceeds %d contract records", limits.records)
	}
	writer := &actionPlanSnapshotBoundedBuffer{limit: int64(limits.bytes)}
	if err := json.NewEncoder(writer).Encode(contract); err != nil {
		if errors.Is(err, errActionPlanSnapshotCompressedTooLarge) {
			return nil, executionCutBudgetError("execution cut canonical contract exceeds %d bytes", limits.bytes)
		}
		return nil, fmt.Errorf("execution cut canonical contract exceeds %d bytes or cannot encode: %w", limits.bytes, err)
	}
	canonical := slices.Clone(writer.Bytes())
	digest := sha256.Sum256(canonical)
	return &ActionPlanFamilyExecutionCut{contract: contract, canonical: canonical, id: hex.EncodeToString(digest[:])}, nil
}

func (cut *ActionPlanFamilyExecutionCut) ID() string {
	if cut == nil {
		return ""
	}
	return cut.id
}

func (cut *ActionPlanFamilyExecutionCut) CanonicalJSON() ([]byte, error) {
	if cut == nil || cut.id == "" {
		return nil, fmt.Errorf("execution cut is uninitialized")
	}
	return slices.Clone(cut.canonical), nil
}

func (cut *ActionPlanFamilyExecutionCut) Roots() []ActionPlanFamilyExecutionCutRoot {
	if cut == nil {
		return nil
	}
	return slices.Clone(cut.contract.Roots)
}

func (cut *ActionPlanFamilyExecutionCut) NodeIDs() []string {
	if cut == nil {
		return nil
	}
	ids := make([]string, len(cut.contract.Nodes))
	for index, node := range cut.contract.Nodes {
		ids[index] = node.Node.ID
	}
	return ids
}

func (cut *ActionPlanFamilyExecutionCut) Origins() []ActionPlanFamilyExecutionCutOrigin {
	if cut == nil {
		return nil
	}
	return slices.Clone(cut.contract.Origins)
}

func (cut *ActionPlanFamilyExecutionCut) Outputs() []ActionPlanFamilyExecutionCutOutput {
	if cut == nil {
		return nil
	}
	outputs := []ActionPlanFamilyExecutionCutOutput{}
	for _, node := range cut.contract.Nodes {
		for slot, output := range node.Node.Outputs {
			outputs = append(outputs, ActionPlanFamilyExecutionCutOutput{NodeID: node.Node.ID, Slot: slot, Output: output})
		}
	}
	return outputs
}

// Verify validates the complete final family once and returns pinned IDs only
// when the cut's exact execution contracts AND all original variant/node
// bindings remain unchanged. Call this before final shard emission; on error,
// never rerun a changed node while keeping observations from the old execution.
func (cut *ActionPlanFamilyExecutionCut) Verify(family *ActionPlanFamily) ([]string, error) {
	if cut == nil || cut.id == "" {
		return nil, fmt.Errorf("execution cut is uninitialized")
	}
	current, err := NewActionPlanFamilyExecutionCut(family, cut.contract.Roots)
	if err != nil {
		return nil, fmt.Errorf("verify execution cut %s: %w", cut.id, err)
	}
	if current.id != cut.id || !bytes.Equal(current.canonical, cut.canonical) {
		return nil, fmt.Errorf("execution cut %s changed in final family (current contract %s)", cut.id, current.id)
	}
	return cut.NodeIDs(), nil
}

// ReadActionPlanFamilyExecutionCut accepts only the exact canonical contract
// reconstructed from the supplied complete initial family. The caller must
// authenticate that family's original source/config/tool/probe inputs before
// loading observations; decoded JSON is never itself execution authority.
func ReadActionPlanFamilyExecutionCut(family *ActionPlanFamily, filename string) (*ActionPlanFamilyExecutionCut, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxActionPlanFamilyExecutionCutBytes {
		return nil, fmt.Errorf("execution cut transport must be a regular file of at most %d bytes", MaxActionPlanFamilyExecutionCutBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxActionPlanFamilyExecutionCutBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxActionPlanFamilyExecutionCutBytes {
		return nil, fmt.Errorf("execution cut transport exceeds %d bytes", MaxActionPlanFamilyExecutionCutBytes)
	}
	roots, err := readActionPlanFamilyExecutionCutRoots(data)
	if err != nil {
		return nil, fmt.Errorf("decode execution cut: %w", err)
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		return nil, err
	}
	// This also rejects duplicate/unknown nested fields, alternate encodings,
	// missing fields and trailing whitespace: no attacker-supplied map or node
	// is used to generate the canonical bytes being compared.
	if !bytes.Equal(data, cut.canonical) {
		return nil, fmt.Errorf("execution cut transport is not the canonical contract of the supplied complete family")
	}
	return cut, nil
}

// Decode only the bounded roots, skipping the asserted contract token by token.
// A byte limit alone is insufficient for decoding []largeStruct: millions of
// tiny empty objects could otherwise amplify a small file into a huge heap.
func readActionPlanFamilyExecutionCutRoots(data []byte) ([]ActionPlanFamilyExecutionCutRoot, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, fmt.Errorf("expected execution cut object")
	}
	limits := map[string]int{
		"schema": 1, "variants": MaxActionPlanFamilyExecutionCutRecords, "toolsets": MaxActionPlanFamilyExecutionCutRecords,
		"roots": MaxActionPlanFamilyExecutionCutOutputs, "nodes": MaxActionPlanFamilyExecutionCutNodes,
		"origins": MaxActionPlanFamilyExecutionCutRecords, "recipes": MaxActionPlanFamilyExecutionCutNodes,
		"sources": MaxActionPlanFamilyExecutionCutRecords, "input_sets": MaxActionPlanFamilyExecutionCutInputSets,
		"capsules": MaxActionPlanFamilyExecutionCutRecords, "validations": MaxActionPlanFamilyExecutionCutRecords,
	}
	seen := map[string]bool{}
	roots := []ActionPlanFamilyExecutionCutRoot{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		limit, known := limits[key]
		if !ok || !known || seen[key] {
			return nil, fmt.Errorf("unknown or duplicate execution cut field %q", key)
		}
		seen[key] = true
		if key != "roots" {
			if err := skipActionPlanFamilyExecutionCutValue(decoder, 0, limit); err != nil {
				return nil, err
			}
			continue
		}
		start, err := decoder.Token()
		if err != nil || start != json.Delim('[') {
			return nil, fmt.Errorf("expected execution cut roots array")
		}
		for decoder.More() {
			if len(roots) >= limit {
				return nil, fmt.Errorf("execution cut exceeds %d roots", limit)
			}
			var root ActionPlanFamilyExecutionCutRoot
			if err := decoder.Decode(&root); err != nil {
				return nil, err
			}
			roots = append(roots, root)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("execution cut transport contains trailing data")
	}
	return roots, nil
}

func skipActionPlanFamilyExecutionCutValue(decoder *json.Decoder, depth, childLimit int) error {
	// The fixed contract schema is shallow (including command replays). Bound
	// malformed nested containers without allocating an attacker-shaped tree.
	if depth >= 64 {
		return fmt.Errorf("execution cut transport nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	opening, container := token.(json.Delim)
	if !container {
		return nil
	}
	if opening != '[' && opening != '{' {
		return fmt.Errorf("unexpected execution cut container delimiter")
	}
	count := 0
	for decoder.More() {
		if childLimit != 0 && count >= childLimit {
			return fmt.Errorf("execution cut transport collection exceeds %d entries", childLimit)
		}
		count++
		if opening == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipActionPlanFamilyExecutionCutValue(decoder, depth+1, 0); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// WriteSelectionMarkers publishes a bounded execution selection only after
// its complete immutable contract has been captured or reconstructed.
func (cut *ActionPlanFamilyExecutionCut) WriteSelectionMarkers(directory string) error {
	if cut == nil || cut.id == "" {
		return fmt.Errorf("execution cut is uninitialized")
	}
	return cut.writeMarkers(directory, "cut", cut.NodeIDs())
}

// WritePinnedMarkers verifies the complete final family before creating any
// directories or files. The executor can then copy every exact node/slot from
// the prior cut stores without running those recipes again.
func (cut *ActionPlanFamilyExecutionCut) WritePinnedMarkers(family *ActionPlanFamily, directory string) error {
	ids, err := cut.Verify(family)
	if err != nil {
		return err
	}
	return cut.writeMarkers(directory, "pinned", ids)
}

func (cut *ActionPlanFamilyExecutionCut) writeMarkers(directory, mode string, ids []string) error {
	entries := []actionPlanEntry{
		{path: "schema/" + LinuxKernelFamilyExecutionSchema},
		{path: "mode/" + mode},
		{path: "seal/" + cut.id},
	}
	for _, id := range ids {
		entries = append(entries, actionPlanEntry{path: mode + "/" + id})
	}
	return writeActionPlanTree(directory, entries)
}

// WriteSegments emits only the dependency-closed initial execution cut. The
// complete family has already been reduced and validated when this immutable
// contract was captured; no selected subgraph is reduced or rekeyed here.
// Final variant views/products are not initial-execution outputs. Replay still
// rebuilds the complete family and verifies this original cut before publishing.
func (cut *ActionPlanFamilyExecutionCut) WriteSegments(outputDirs map[string]string) error {
	if err := validateFamilyPlanSegmentOutputs(outputDirs); err != nil {
		return err
	}
	entries, err := cut.segmentEntries()
	if err != nil {
		return err
	}
	return writeActionPlanFamilySegments(outputDirs, entries)
}

func (cut *ActionPlanFamilyExecutionCut) segmentEntries() (map[string][]actionPlanEntry, error) {
	if _, err := cut.CanonicalJSON(); err != nil {
		return nil, err
	}
	contract := cut.contract
	plan := cloneActionPlan(&ActionPlan{
		Toolsets: contract.Toolsets, Sources: contract.Sources,
		Recipes: contract.Recipes, InputSets: contract.InputSets,
	})
	for _, record := range contract.Nodes {
		node := record.Node
		node.familySourceProjections = slices.Clone(record.SourceProjections)
		plan.Nodes = append(plan.Nodes, node)
	}
	// This private emission view contains exact sealed execution contracts,
	// not a public complete family on which to rerun liveness or reduction.
	emission := &ActionPlanFamily{
		Toolsets: plan.Toolsets, Sources: plan.Sources, Recipes: plan.Recipes,
		Nodes: plan.Nodes, InputSets: plan.InputSets,
		Variants: slices.Clone(contract.Variants), Capsules: contract.Capsules,
		Validations: slices.Clone(contract.Validations),
	}
	return emission.segmentEntriesValidated()
}
